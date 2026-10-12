package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// --------------------------------------------------------------------------- 自定义脚本执行
//
// 「自定义脚本」模式下，构建流程**整个由用户那段 shell 定义**：拉代码、编译、打包、
// 推送都写在里面。平台不解析脚本、不替它做决定，只负责三件事：
//
//	1. 把仓库凭据配好（GIT_ASKPASS），脚本里写普通 clone 地址即可，不出现密码；
//	2. 注入一组 YUNNIAN_* 环境变量，脚本靠它们拿到"构建哪个仓库、产出哪个镜像"；
//	3. 把输出逐行收进构建日志，失败时取尾部作为真因。
//
// 为什么不做"平台解析脚本、提取镜像名"这类事：那等于给脚本加了一套只有平台懂的
// 隐式约定，用户改一行脚本就可能让它失效，而失效方式是静默的（平台提取不到，
// 于是镜像列表空着）。宁可要求脚本把镜像推到 `$YUNNIAN_IMAGE` 这个明确的位置。

// 脚本阶段的错误码。与 dockerfile 路径的 DOCKER_BUILD_FAILED 分开：
// 一个是"docker 打不动这个包"，一个是"你写的脚本返回了非 0"，排查方向不同。
const (
	// ErrCodeScriptRun 脚本本身返回非 0。
	ErrCodeScriptRun = "SCRIPT_FAILED"
	// ErrCodeScriptTimeout 脚本超时。单独一个码：超时的修法是"调大超时或优化构建"，
	// 和"脚本里有语法错误"完全不是一件事。
	ErrCodeScriptTimeout = "SCRIPT_TIMEOUT"
	// ErrCodeScriptNoImage 脚本跑完了，但构建机上没有它本该产出的那个镜像。
	//
	// ⚠️ 这一条是本模式最容易踩的坑：一段只做 git clone 的脚本会**退出码 0**，
	// 于是构建记录显示成功、日志一片祥和，而镜像列表里什么都没有、
	// 部署时选不到版本。不把它判成失败，用户会以为构建成功了。
	ErrCodeScriptNoImage = "SCRIPT_NO_IMAGE"
)

// scriptTailLines 脚本失败时保留多少行输出作为真因。
//
// 比 dockerfile 路径的 40 行多：脚本是用户自己写的，失败往往发生在中间某一步，
// 而他需要看到那一步**是做什么的**（命令回显）以及**报了什么**。
const scriptTailLines = 60

// buildByScript 执行一次自定义脚本构建。
func (b *DockerBuilder) buildByScript(
	ctx context.Context, req BuildRequest, sink func(level, line string) error, start time.Time,
) (*BuildResult, error) {
	script := strings.TrimSpace(req.BuildScript)
	if script == "" {
		return nil, &BuildFailure{
			Code:   ErrCodeScriptRun,
			Stage:  "读取脚本",
			Detail: "项目没有保存构建脚本，请到项目编辑里填写（可用「恢复默认脚本」）",
		}
	}
	if err := sink("info", fmt.Sprintf("执行自定义构建脚本（%d 行）", strings.Count(script, "\n")+1)); err != nil {
		return nil, err
	}

	ws, err := b.prepareWorkspace(req)
	if err != nil {
		return nil, err
	}

	cloneEnv, cleanup, err := prepareAskpass(b.workRoot, req.GitUsername, req.GitPassword)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, &BuildFailure{Code: ErrCodeWorkspace, Stage: "准备凭据", Detail: err.Error(), Err: err}
	}

	// 脚本落成文件再执行，而不是 `sh -c "<原文>"`：
	// 原文是多行、带引号的，塞进 -c 之后 shell 要再解释一遍，报错里的行号全是错的；
	// 落成文件后 `sh -n` 之外的所有报错都能指到正确的那一行。
	scriptPath := filepath.Join(b.workRoot, fmt.Sprintf("build-%d.sh", req.BuildID))
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		return nil, &BuildFailure{Code: ErrCodeWorkspace, Stage: "写入脚本", Detail: err.Error(), Err: err}
	}
	defer func() { _ = os.Remove(scriptPath) }()

	// 把脚本原文回显进日志。构建详情里只看得到"脚本跑过"，看不到"跑了什么"，
	// 事后排查就只能靠猜；而这行成本极低。
	// 前面加 `#` 前缀是为了让它和真实命令输出区分开，不至于被误认为执行结果。
	for _, line := range strings.Split(script, "\n") {
		if err := sink("cmd", "# "+line); err != nil {
			return nil, err
		}
	}

	env := append(append([]string{}, cloneEnv...), scriptEnv(req, ws)...)
	tail := newTailBuffer(scriptTailLines)
	digests := map[string]string{}

	runErr := b.run(ctx, Cmd{
		Dir:     ws,
		Env:     env,
		Name:    "sh",
		Args:    []string{scriptPath},
		Secrets: secretList(req.GitUsername, req.GitPassword),
	}, func(_, line string) error {
		tail.push(line)
		if tag, dg, ok := parsePushDigestLine(line); ok {
			digests[tag] = dg
		} else if dg := lastDigestOf(line); dg != "" {
			// 认不出 tag 的 digest 行也记下（不同 docker 版本输出形态有差异），
			// 用空 key 存，作为最后的兜底取值。
			digests[""] = dg
		}
		return sink("info", line)
	})

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, &BuildFailure{
				Code:  ErrCodeScriptTimeout,
				Stage: "执行脚本",
				Detail: fmt.Sprintf("超过 %s 仍未结束，构建被中断。%s",
					req.Timeout.Round(time.Second), tail.String()),
				Err: runErr,
			}
		}
		return nil, &BuildFailure{Code: ErrCodeScriptRun, Stage: "执行脚本", Detail: tail.String(), Err: runErr}
	}

	// 脚本退出码 0，但**不代表它真的构建了什么**。这是本模式最容易出现的一种
	// "看起来成功"：脚本里条件写错、变量名拼错、或者干脆是空脚本，都会安静地跑完。
	// 平台能查证的事实只有一条 —— 构建机上到底有没有这个镜像。
	arch, ok := b.inspectImageArch(ctx, req.Image)
	if !ok {
		return nil, &BuildFailure{
			Code:  ErrCodeScriptNoImage,
			Stage: "校验产物",
			Detail: fmt.Sprintf(
				"脚本执行成功（退出码 0），但构建机上找不到本次要产出的镜像 %s。\n"+
					"请确认脚本里执行了 `docker build -t \"$YUNNIAN_IMAGE\" .`（或推送到同一个地址）。\n"+
					"最后几行输出：\n%s", req.Image, tail.String()),
		}
	}

	digest := digests[req.Tag]
	if digest == "" {
		digest = digests[""]
	}
	if digest == "" {
		// 读不到就如实留空，由平台侧从仓库回读补齐（EnrichMissingImageMeta）。
		// **不编** —— 一句"我们没读到"永远好过一条长得像真的假数据。
		if err := sink("info", "未能从脚本输出中读到 digest，将由平台从仓库回读补齐"); err != nil {
			return nil, err
		}
	} else {
		if err := sink("info", fmt.Sprintf("镜像已推送 %s digest=%s", req.Image, digest)); err != nil {
			return nil, err
		}
	}

	if err := sink("info", "构建成功"); err != nil {
		return nil, err
	}
	return &BuildResult{
		Digest:   digest,
		Duration: time.Since(start),
		Source:   BuildSourceDocker,
		Arch:     arch,
	}, nil
}

// inspectImageArch 读本地镜像的 os/arch，同时用它回答"这个镜像到底存不存在"。
//
// 两件事合成一次调用：分开做会有两次机会拿到不一致的答案（比如第一次在、
// 第二次被别的构建 rmi 掉了），而这里只需要一个当下的结论。
func (b *DockerBuilder) inspectImageArch(ctx context.Context, image string) (arch string, ok bool) {
	out, err := b.capture(ctx, Cmd{Name: "docker", Args: []string{
		"image", "inspect", image, "--format", "{{.Os}}/{{.Architecture}}",
	}})
	if err != nil {
		return "", false
	}
	if out == "" || out == "<no value>" {
		// 镜像在，但读不出平台信息（极老的镜像格式）。存在性成立，架构如实留空。
		return "", true
	}
	return out, true
}

// scriptEnv 平台注入给脚本的环境变量。
//
// 命名统一加 YUNNIAN_ 前缀，是为了**不和构建参数撞名** —— 用户完全可以定义
// 一个叫 REGISTRY 或 IMAGE 的构建参数，而那两个值可能和他以为的不一样。
// 前缀把"平台给的"和"你自己定义的"在脚本里一眼分开。
func scriptEnv(req BuildRequest, workDir string) []string {
	env := []string{
		"YUNNIAN_REPO=" + req.GitURL,
		"YUNNIAN_BRANCH=" + req.Branch,
		"YUNNIAN_COMMIT=" + req.CommitID,
		"YUNNIAN_PROJECT=" + req.ProjectKey,
		"YUNNIAN_BUILD_ID=" + strconv.FormatUint(req.BuildID, 10),
		"YUNNIAN_BUILD_NO=" + strconv.FormatInt(req.BuildNo, 10),
		"YUNNIAN_REGISTRY=" + req.Registry,
		"YUNNIAN_IMAGE_NAME=" + req.ImageName,
		"YUNNIAN_TAG=" + req.Tag,
		"YUNNIAN_IMAGE=" + req.Image,
		"YUNNIAN_PLATFORM=" + req.Platform,
		"YUNNIAN_WORKDIR=" + workDir,
	}
	// 构建参数放最后：万一用户定义了一个 YUNNIAN_ 开头的参数，他会覆盖掉平台的同名项，
	// 而这是他显式写的，比平台默认值更该生效。
	for k, v := range req.Params {
		key := strings.TrimSpace(k)
		if key == "" || strings.ContainsAny(key, " \t=$\"'`\\") {
			// 非法变量名直接跳过，不要让整个构建因为一个参数名而失败 ——
			// 参数名在保存时已经校验过，这里是最后一道防线。
			continue
		}
		env = append(env, key+"="+v)
	}
	return env
}

// rePushDigestLine 从 `docker push` 的输出行里同时抓 tag 与 digest。
//
// 输出形态：`v8: digest: sha256:<64hex> size: 1234`
// 前面那截是**被推送的 tag** —— 一次脚本可能推多个 tag（v8 和 latest），
// 而平台要记的是本次构建的那个；只抓 digest 不抓 tag，就只能拿到最后一个。
var rePushDigestLine = regexp.MustCompile(`(?i)^\s*([^:\s]+):\s*digest:\s*(sha256:[0-9a-f]{64})\s+size:`)

// parsePushDigestLine 解析带 tag 的 digest 行。
func parsePushDigestLine(line string) (tag, digest string, ok bool) {
	m := rePushDigestLine.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// lastDigestOf 兜底：只认 `sha256:` + 64 位十六进制。
//
// 宁可不匹配，也不能匹配到半截字符串 —— 一条长度不对的 digest 看起来仍然像真的。
func lastDigestOf(line string) string {
	m := rePushDigest.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}
