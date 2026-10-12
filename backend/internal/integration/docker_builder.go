package integration

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// --------------------------------------------------------------------------- 真执行构建
//
// 本文件是「构建」这件事**真的发生**的地方。
//
// 在它之前，`LocalBuilder` 是唯一的实现：不执行任何 docker 命令，只按写死的脚本 sleep
// 打日志，成败由 `failProjects` 这个写死 map 决定。那条路会产出一条完整的、看起来
// 毫无异常的成功记录 —— 日志一行行刷、进度条走完、状态「成功」、镜像列表多一条 ——
// 而仓库里根本没有这个镜像。**这不是降级，是凭空造了一条成功记录。**
//
// 所以本实现的每条结论都只能来自外部命令的真实输出：
//
//	成功     = docker build 与 docker push 都返回 0；
//	digest   = 从 `docker push` 的输出里读到的（Registry 在 manifest PUT 的响应里给的），
//	           **读不到就留空**，由平台侧从仓库回读补齐 —— 绝不自己算一个；
//	失败原因 = 失败那条命令输出的**尾部原文**（真因几乎总在最后几行）。

// 构建各阶段的错误码。
//
// 沿用项目既有的大写下划线口径（与 COMPILE_ERROR / NPM_INSTALL / TIMEOUT 同族），
// 前端与 `diagnoseBuild` 都按它查表。
const (
	// ErrCodeWorkspace 工作目录准备失败。几乎总是权限或磁盘问题。
	ErrCodeWorkspace = "WORKSPACE_ERROR"
	// ErrCodeGitClone 检出代码失败。
	//
	// 单独一个码，因为它最容易被误判：主机名解析不了、凭据过期、分支被删，
	// 三种原因的表象都是"构建失败"，但都会让人先去怀疑是不是代码有问题。
	ErrCodeGitClone = "GIT_CLONE_FAILED"
	// ErrCodeBuildStage 独立编译步骤（两步模式的第一步）失败。
	ErrCodeBuildStage = "BUILD_STAGE_FAILED"
	// ErrCodeDockerBuild 打包镜像失败。
	ErrCodeDockerBuild = "DOCKER_BUILD_FAILED"
	// ErrCodeDockerPush 推送镜像失败。
	ErrCodeDockerPush = "DOCKER_PUSH_FAILED"
)

// ---------------------------------------------------------------- 命令执行

// Cmd 一条要执行的外部命令。
type Cmd struct {
	// Dir 工作目录。
	Dir string
	// Env **追加**到当前环境之上（不是替换），形如 KEY=VALUE。
	Env []string
	// Name 可执行文件名，Args 其余参数。
	Name string
	Args []string
	// Secrets 本命令的**敏感子串**，出现在日志里要替换成 ***。
	//
	// 为什么必须显式声明、而不是靠调用方小心：
	// git 与 docker 都会把失败的完整命令行原样打进 stderr
	// （`fatal: unable to access 'http://user:token@host/x.git/'`），
	// 而这行会被落进构建日志、再被送给 AI 归因 —— 凭据就是这样流出去的，
	// 而且是在"出错"的时候流出去，那时没人会去检查日志里有什么。
	Secrets []string
}

// CmdRunner 执行一条命令，把输出**按行**回调给 onLine。
//
// 抽成函数类型而不是直接调 exec.Command，是为了让"哪一步失败映射成哪个错误码"
// 这件事可测：真起 docker / git 的测试跑不快也不可靠，而错误码映射恰恰是
// 这段代码最该被盯住的部分。
type CmdRunner func(ctx context.Context, c Cmd, onLine func(level, line string) error) error

// ExecRunner 真正调用系统命令的实现。
func ExecRunner(ctx context.Context, c Cmd, onLine func(level, line string) error) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)

	// stdout 与 stderr 合并到同一个管道。
	// 不分开的原因：构建日志要的是**时序**（"报错发生在哪一步之后"），
	// 分成两路反而要看时间戳拼回去；docker 自己也是这么输出的。
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("创建输出管道失败: %w", err)
	}
	cmd.Stdout, cmd.Stderr = pw, pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return err
	}
	// 父进程必须立刻关掉写端：子进程已经继承了它。不关的话 scanner 永远读不到 EOF，
	// 表现是构建跑完了、日志也刷完了，但这一步**永远不返回**。
	_ = pw.Close()

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := redact(sc.Text(), c.Secrets)
		if err := onLine("info", line); err != nil {
			// 回调失败（前端日志流已关闭）不应该让命令继续跑下去。
			_ = cmd.Process.Kill()
			break
		}
	}
	_ = pr.Close()
	return cmd.Wait()
}

// ---------------------------------------------------------------- 构建器

// DockerBuilder 真正执行构建的 Builder。
//
// 与 LocalBuilder 的关系：LocalBuilder 只在平台进程内打日志、不执行 docker，
// 保留它是为了在**没有构建机**的环境里仍能把编排链路跑通；它不再产出 digest。
// 生产路径一律走本实现。
//
// ⚠️ 取消的边界（如实说明，不假装做到了）：ctx 被取消时本实现只杀掉**本机的
// docker / git 进程**。docker CLI 死了不等于 dockerd 那边的构建停了 ——
// daemon 会把已经开始的构建跑完，只是没人再接它的输出。
// 要做到真取消得走 BuildKit 的控制接口，本版没做。
type DockerBuilder struct {
	name     string
	workRoot string
	platform string
	run      CmdRunner
}

// NewDockerBuilder 构造。name 用于在接口响应里标注执行者（如 docker / docker-agent）；
// workRoot 是构建工作根目录，每次构建在它下面开一个 build-<id> 子目录。
func NewDockerBuilder(name, workRoot, platform string) *DockerBuilder {
	if strings.TrimSpace(name) == "" {
		name = "docker"
	}
	if strings.TrimSpace(workRoot) == "" {
		workRoot = defaultBuildWorkRoot()
	}
	if strings.TrimSpace(platform) == "" {
		platform = "linux/amd64"
	}
	return &DockerBuilder{name: name, workRoot: workRoot, platform: platform, run: ExecRunner}
}

// defaultBuildWorkRoot 构建工作根目录的默认值。
func defaultBuildWorkRoot() string { return filepath.Join(os.TempDir(), "yunnian-build") }

// BuildWorkspace 本次构建的工作目录。
//
// 导出它是因为**另一个进程**（构建机上的 Agent）需要知道这个路径：构建结束后
// 它要清理这个目录，否则历次构建的残留会把构建机的磁盘一点点吃满
// （每次构建都是一个新的 build-<id>，清不到的话就是几十 GB 的量级）。
//
// 让"路径怎么算"只有一份：两处各写一遍，改一处忘另一处的结果是
// "Agent 说清理成功了、磁盘还在涨"，而那种错没人会往"路径算得不一样"上想。
func BuildWorkspace(workRoot string, buildID uint64) string {
	if strings.TrimSpace(workRoot) == "" {
		workRoot = defaultBuildWorkRoot()
	}
	return filepath.Join(workRoot, fmt.Sprintf("build-%d", buildID))
}

// Name 实现名。
func (b *DockerBuilder) Name() string { return b.name }

// Build 执行一次构建。
//
// 两条路，按 Method 分：
//
//	dockerfile：检出 → docker build -f <项目自带的那份> → push → 回读 digest。
//	            编译怎么写全在 Dockerfile 里（多阶段就 `FROM maven AS builder`）。
//	script    ：把项目的脚本原样交给构建机执行，平台不解析、不干预。
//
// 为什么不把 script 做成"平台拼 Dockerfile"：那会让用户既看不到也改不了平台替他
// 决定的东西（编译镜像、产物路径、启动命令），构建坏了他没有任何抓手。
// 一份能读、能改、能进版本库的脚本是更好的模板。
func (b *DockerBuilder) Build(ctx context.Context, req BuildRequest, sink func(level, line string) error) (*BuildResult, error) {
	start := time.Now()

	if strings.TrimSpace(req.Method) == "script" {
		return b.buildByScript(ctx, req, sink, start)
	}

	platform := strings.TrimSpace(b.platform)
	if req.Platform != "" {
		platform = req.Platform
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

	tail := newTailBuffer(errorTailLines)

	// feed 是各阶段输出的统一出口：边刷给前端，边攒一份尾巴备查失败真因。
	feed := func(_ string, line string) error {
		tail.push(line)
		return sink("info", line)
	}

	// stage 跑一个阶段：把命令行打出来（用户要知道这一步到底执行了什么），
	// 失败时把尾部输出作为真因带出去。
	stage := func(name, code string, c Cmd) error {
		tail.reset()
		if err := sink("cmd", "$ "+displayCmd(c)); err != nil {
			return err
		}
		if runErr := b.run(ctx, c, feed); runErr != nil {
			return &BuildFailure{Code: code, Stage: name, Detail: tail.String(), Err: runErr}
		}
		return nil
	}

	// ---- 1) 检出代码
	//
	// CommitID 为空时只按分支取（触发构建时分支已解析过）。
	if url := strings.TrimSpace(req.GitURL); url != "" {
		base := []string{"clone"}
		if br := strings.TrimSpace(req.Branch); br != "" {
			base = append(base, "--branch", br)
		}

		// 先试浅克隆，失败了再退完整克隆。
		//
		// 为什么要留这条退路：`--depth 1` 依赖服务端的 smart HTTP 能力。
		// 实测一个只支持哑 HTTP 的仓库会直接报
		// `fatal: dumb http transport does not support shallow capabilities` ——
		// 用户看到的是"拉取代码失败"，而这句话对他没有任何指向性。
		// 浅克隆省的是几十秒，换来一类查不明白的失败，不划算；
		// 但直接放弃它又会让大仓库每次全量拉。所以：能浅就浅，不能就退。
		shallow := append(append([]string{}, base...), "--depth", "1", url, ws)
		tail.reset()
		if err := sink("cmd", "$ git "+strings.Join(shallow, " ")); err != nil {
			return nil, err
		}
		if runErr := b.run(ctx, Cmd{Dir: b.workRoot, Env: cloneEnv, Name: "git", Args: shallow}, feed); runErr != nil {
			// 只在**能判定是"服务端不支持浅克隆"**时才退。
			// 凭据错、分支不存在这类失败退一百次也一样，而且重试的输出会把真因刷掉 ——
			// 那正好是我们最不想要的结果。
			if !strings.Contains(tail.String(), "shallow") {
				return nil, &BuildFailure{Code: ErrCodeGitClone, Stage: "拉取代码", Detail: tail.String(), Err: runErr}
			}
			if err := sink("info", "服务端不支持浅克隆，改为完整克隆（会慢一些）"); err != nil {
				return nil, err
			}
			// 半途失败会留下不完整的目录，git clone 到非空目录会直接拒绝，必须先清掉。
			if err := os.RemoveAll(ws); err != nil {
				return nil, &BuildFailure{Code: ErrCodeWorkspace, Stage: "重试前清理工作目录", Detail: err.Error(), Err: err}
			}
			if err := os.MkdirAll(ws, 0o755); err != nil {
				return nil, &BuildFailure{Code: ErrCodeWorkspace, Stage: "重试前重建工作目录", Detail: err.Error(), Err: err}
			}
			full := append(append([]string{}, base...), url, ws)
			if err := stage("拉取代码（完整）", ErrCodeGitClone, Cmd{Dir: b.workRoot, Env: cloneEnv, Name: "git", Args: full}); err != nil {
				return nil, err
			}
		}

		if id := strings.TrimSpace(req.CommitID); id != "" {
			// 分支头就是它时直接检出，省一次往返；否则按 SHA 取回来。
			head, _ := b.capture(ctx, Cmd{Dir: ws, Env: cloneEnv, Name: "git", Args: []string{"rev-parse", "HEAD"}})
			if !commitEqual(head, id) {
				if err := stage("拉取指定提交", ErrCodeGitClone,
					Cmd{Dir: ws, Env: cloneEnv, Name: "git", Args: []string{"fetch", "--depth", "1", "origin", id}}); err != nil {
					return nil, err
				}
				if err := stage("检出指定提交", ErrCodeGitClone,
					Cmd{Dir: ws, Env: cloneEnv, Name: "git", Args: []string{"checkout", "--detach", "FETCH_HEAD"}}); err != nil {
					return nil, err
				}
			}
		}
	}

	// ---- 2) 打包镜像
	//
	// 编译不在这里做：它要么写在项目自带的 Dockerfile 里（多阶段构建），
	// 要么写在 script 模式的脚本里。「平台替用户跑一次 mvn」曾经存在过，
	// 但它和 Dockerfile 里可能也有的编译步骤是两件事，同时生效时会重复编译、
	// 而只生效一件时又没人说得清是哪件 —— 现在只有一条路，没有歧义。
	dockerfile, ctxDir := b.resolveDockerfile(req, ws)
	buildArgs := []string{"build", "--platform", platform, "-f", dockerfile, "-t", req.Image}
	// `--provenance=false`：BuildKit 默认会给镜像挂上 attestation，于是推上去的是一个
	// **manifest list**（含一个 unknown/unknown 平台条目）。单架构镜像变成列表本身无害，
	// 但 Harbor 界面会多出莫名条目、部分老 kubelet 会挑不到平台。这里关掉，保持朴素单架构。
	buildArgs = append(buildArgs, "--provenance=false")
	buildArgs = append(buildArgs, ctxDir)
	if err := stage("打包镜像", ErrCodeDockerBuild, Cmd{Dir: ws, Name: "docker", Args: buildArgs}); err != nil {
		return nil, err
	}

	// ---- 3) 推送
	var digest string
	{
		tail.reset()
		push := Cmd{Dir: ws, Name: "docker", Args: []string{"push", req.Image}}
		if err := sink("cmd", "$ "+displayCmd(push)); err != nil {
			return nil, err
		}
		if runErr := b.run(ctx, push, func(_ string, line string) error {
			tail.push(line)
			// digest 由 registry 在 manifest PUT 的响应里给出，CLI 只是把它打出来。
			// 这是**唯一**可信的 digest 来源：本地算出的 sha256 和仓库里的 manifest digest
			// 完全是两码事（前者是内容寻址，后者是 manifest 的哈希），对不上就是编的。
			if m := rePushDigest.FindStringSubmatch(line); m != nil {
				digest = m[1]
			}
			return sink("info", line)
		}); runErr != nil {
			return nil, &BuildFailure{Code: ErrCodeDockerPush, Stage: "推送镜像", Detail: tail.String(), Err: runErr}
		}
	}

	if digest == "" {
		// 读不到就如实留空，由平台侧从仓库回读补齐（EnrichMissingImageMeta）。
		// **不编**：一句"我们没读到"永远好过一条长得像真的假数据。
		_ = sink("info", "未能从推送输出中读到 digest，将由平台从仓库回读补齐")
	} else {
		_ = sink("info", fmt.Sprintf("镜像已推送 %s digest=%s", req.Image, digest))
	}
	// 读回产出镜像的架构。
	//
	// 这一步读的是**结果**，不是**意图**：请求里的 Platform 是"我要 linux/amd64"，
	// 这里是"我实际得到了什么"。正常情况两者一致；一旦不一致（例如服务端忽略了
	// 平台参数、或构建器写错了参数），只有这里看得出来 —— 而写"我要的"会把问题
	// 盖得毫无痕迹。
	//
	// 读不到就留空，由仓库回读补齐。**不拿请求里的 Platform 冒充结果。**
	arch, _ := b.capture(ctx, Cmd{Dir: ws, Name: "docker", Args: []string{
		"image", "inspect", req.Image, "--format", "{{.Os}}/{{.Architecture}}",
	}})
	if arch == "<no value>" {
		arch = ""
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

// prepareWorkspace 准备本次构建的工作目录。
//
// 每次构建都**先删再建**：残留的上一轮产物会让"编译其实失败了但产物还在"
// 这类问题伪装成成功 —— 这与测试脚本"先清产物再断言"是同一个道理。
func (b *DockerBuilder) prepareWorkspace(req BuildRequest) (string, error) {
	ws := BuildWorkspace(b.workRoot, req.BuildID)
	if err := os.MkdirAll(b.workRoot, 0o755); err != nil {
		return "", &BuildFailure{Code: ErrCodeWorkspace, Stage: "准备工作目录", Detail: err.Error(), Err: err}
	}
	if err := os.RemoveAll(ws); err != nil {
		return "", &BuildFailure{Code: ErrCodeWorkspace, Stage: "清理工作目录", Detail: err.Error(), Err: err}
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return "", &BuildFailure{Code: ErrCodeWorkspace, Stage: "创建工作目录", Detail: err.Error(), Err: err}
	}
	return ws, nil
}

// resolveDockerfile 决定"用哪个 Dockerfile、构建上下文在哪"。
//
// 两者都由项目配置给出，且都相对**检出后的工作目录**：
// Dockerfile 用 DockerfilePath（默认 `Dockerfile`），上下文用 BuildContext（默认 `.`）。
//
// 路径不做存在性检查：不存在时 `docker build` 会自己报
// `failed to read dockerfile: open Dockerfile: no such file or directory`，
// 那是一句能直接指向根因的话；在这里提前判反而要把同一件事再说一遍，
// 而且平台的判断（相对谁、软链算不算）未必和 docker 一致。
func (b *DockerBuilder) resolveDockerfile(req BuildRequest, ws string) (dockerfile, ctxDir string) {
	ctxDir = strings.TrimSpace(req.Context)
	if ctxDir == "" {
		ctxDir = "."
	}
	df := strings.TrimSpace(req.Dockerfile)
	if df == "" {
		df = "Dockerfile"
	}
	return filepath.Join(ws, df), filepath.Join(ws, ctxDir)
}

// capture 跑一条"只想知道结果、不关心输出"的命令。
func (b *DockerBuilder) capture(ctx context.Context, c Cmd) (string, error) {
	var sb strings.Builder
	err := b.run(ctx, c, func(_, line string) error {
		sb.WriteString(line)
		sb.WriteString("\n")
		return nil
	})
	return strings.TrimSpace(sb.String()), err
}

// ---------------------------------------------------------------- 辅助

// errorTailLines 失败时保留多少行输出作为真因。
//
// 取 40 而不是"整个输出"：error_message 列是 512 字节，塞不下整份日志；
// 而构建失败的真因（哪一行报了错）几乎总在最后几行 —— 与 AI 归因只取日志尾部同理。
const errorTailLines = 40

// rePushDigest 从 `docker push` 输出里抓 digest。
//
// 输出形态：`<tag>: digest: sha256:<64hex> size: 1234`
// 只认 `sha256:` 加 64 位十六进制 —— 宁可不匹配，也不能匹配到半截字符串。
var rePushDigest = regexp.MustCompile(`(?i)\bdigest:\s*(sha256:[0-9a-f]{64})`)

// tailBuffer 保留最近 N 行输出。
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) reset() {
	t.mu.Lock()
	t.lines = t.lines[:0]
	t.mu.Unlock()
}

func (t *tailBuffer) push(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// displayCmd 把一条命令渲染成可读、可复制的单行，并把凭据换掉。
func displayCmd(c Cmd) string {
	parts := make([]string, 0, len(c.Args)+1)
	parts = append(parts, c.Name)
	for _, a := range c.Args {
		if strings.ContainsAny(a, " \t\"'$") {
			parts = append(parts, "'"+a+"'")
			continue
		}
		parts = append(parts, a)
	}
	return redact(strings.Join(parts, " "), c.Secrets)
}

// redact 把敏感子串换成 ***。
func redact(s string, secrets []string) string {
	for _, sec := range secrets {
		if len(sec) < 4 {
			// 太短的值（比如一个字符的密码）替换起来会误伤正常文本，
			// 反而把日志搞乱；这类值应从源头就不允许设置。
			continue
		}
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}

// commitEqual 比较两个提交号，支持短号（8 位）与全长（40 位）混用。
func commitEqual(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// ---------------------------------------------------------------- 检出凭据

// gitAskpassScript 让 git 从**环境变量**取凭据，而不是把凭据写进命令行。
//
// 为什么不直接把凭据拼进 URL（`http://user:pass@host/x.git`），那是两条真实的泄漏路径：
//   - 进程列表（ps）里能看到完整命令行；
//   - 失败时 git 会把**完整 URL** 打进 stderr（`fatal: unable to access 'http://user:pass@…'`），
//     这行会进构建日志、进构建记录、再被送给 AI 归因。
//
// askpass 只用环境变量传，且调用方仍会把凭据登记进 Cmd.Secrets 做兜底替换。
const gitAskpassScript = `#!/bin/sh
case "$1" in
  *Username*) printf '%s\n' "$YUNNIAN_GIT_USERNAME" ;;
  *Password*) printf '%s\n' "$YUNNIAN_GIT_PASSWORD" ;;
  *) printf '\n' ;;
esac
`

// prepareAskpass 在需要凭据时准备一个临时的 askpass 脚本。
//
// 返回的 cleanup 必须被调用（defer），否则脚本会留在工作根目录里 ——
// 它本身不含凭据（凭据在环境变量里），但留着没有任何好处。
func prepareAskpass(workRoot, username, password string) (env []string, cleanup func(), err error) {
	if strings.TrimSpace(username) == "" && strings.TrimSpace(password) == "" {
		return nil, nil, nil
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(workRoot, ".git-askpass")
	// 0700：只有构建进程自己能读。多用户的构建机上这行是必要的。
	if err := os.WriteFile(path, []byte(gitAskpassScript), 0o700); err != nil {
		return nil, nil, err
	}
	return []string{
		"GIT_ASKPASS=" + path,
		// 关掉交互式提示：否则 git 在非 tty 环境下可能挂住等输入，直到整个构建超时。
		"GIT_TERMINAL_PROMPT=0",
		// 这两个变量名是 gitAskpassScript 里读的那两个，**改动必须同步**：
		// 只改一处会让凭据变成空串，而 git 在空凭据下的报错是
		// `Authentication failed`——看起来像密码错了。
		"YUNNIAN_GIT_USERNAME=" + username,
		"YUNNIAN_GIT_PASSWORD=" + password,
	}, func() { _ = os.Remove(path) }, nil
}
