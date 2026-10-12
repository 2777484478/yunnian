package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// --------------------------------------------------------------------------- 仓库连通性探测
//
// 这个文件回答一个问题：**这个地址 + 这套凭据，现在能不能真的读到代码。**
//
// 为什么不复用平台级 GitLab 通道（`GitLabBridge.Branches`）：
//
//   - 那条路走的是 GitLab **REST API**，用的是平台级配置里的令牌。用户在这里
//     输入的是一套只属于这个项目的账号密码，两者不是一回事 —— 用平台令牌测通了，
//     构建时用项目凭据照样可能 401。
//   - 更要紧的是"测得准"：构建用的是 `git clone`（HTTP 协议 + 凭据助手），
//     所以探测也必须走 `git ls-remote`。**测同一件事**才有意义；测另一条路
//     通不通，只会给用户一个"测试通过但构建失败"的假承诺。

// RepositoryProbe 一次连通性探测的输入。
type RepositoryProbe struct {
	URL      string
	Username string
	Password string
	// Branch 非空时顺带核实这个分支存在 —— 分支名写错是"构建必失败"的第一大来源，
	// 而它恰恰是最容易在保存前发现的一类。
	Branch string
}

// ProbeResult 探测结果。
//
// OK 与 Message 是给用户看的；Detail 只给运维定位，且**构造时已打码**。
type ProbeResult struct {
	OK            bool
	Message       string
	Detail        string
	Branches      []string
	DefaultBranch string
}

// GitProbe 真跑一次 `git ls-remote`。
//
// 与构建共用 `prepareAskpass`：凭据只经环境变量传给 git，不进命令行，
// 因此不会出现在 `ps` 里、也不会被 git 打进失败信息。
//
// ⚠️ 返回的 Message 必须是**人话**。把 git 的原始 stderr 端出去，用户看到的是
// `fatal: Authentication failed for 'http://10.0.0.1/x.git/'` —— 他第一反应会是
// "平台坏了"，而不是"密码打错了一个字符"。
func GitProbe(ctx context.Context, run CmdRunner, workRoot string, req RepositoryProbe) ProbeResult {
	env, cleanup, err := prepareAskpass(workRoot, req.Username, req.Password)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return ProbeResult{OK: false, Message: "准备凭据失败：" + err.Error()}
	}

	args := []string{"ls-remote", "--symref", req.URL, "HEAD", "refs/heads/*"}
	var out strings.Builder
	runErr := run(ctx, Cmd{
		Dir:     workRoot,
		Env:     env,
		Name:    "git",
		Args:    args,
		Secrets: secretList(req.Username, req.Password),
	}, func(_, line string) error {
		out.WriteString(line)
		out.WriteString("\n")
		return nil
	})

	raw := strings.TrimSpace(out.String())
	if runErr != nil {
		// 超时单独认：ctx 被掐断时 runErr 是一句 `signal: killed`，
		// 直接端出去没人看得懂"是网络慢还是地址错"。
		if ctx.Err() == context.DeadlineExceeded {
			return ProbeResult{OK: false,
				Message: fmt.Sprintf("连接超时（%s 内没有响应）：检查地址与端口，以及这台机器到它的网络是否通", probeTimeoutLabel()),
				Detail:  raw}
		}
		return ProbeResult{OK: false, Message: explainGitError(raw, runErr), Detail: raw}
	}

	branches, def := parseLsRemote(raw)
	return ProbeResult{OK: true, Message: "连接成功", Branches: branches, DefaultBranch: def}
}

func probeTimeoutLabel() string { return probeTimeout.Round(time.Second).String() }

// GitResolveCommit 用 `git ls-remote` 取某个分支当前指向的提交号。
//
// 与 GitProbe 共用 `prepareAskpass`：凭据只经环境变量传给 git，不进命令行。
//
// ⚠️ 只回**提交号**，另外三个字段（Message / Author / At）一律留空 ——
// `ls-remote` 的输出只有 `<sha>\t<refname>`，它给不出提交信息与作者。
// 这里**不编**：从前 LocalRepository 会连提交信息（"feat: 支持按环境下发配置"）
// 和作者（"CI Bot"）一起编出来，然后被写进 `build_record.commit_msg` ——
// 事后排查时没人分得清那条信息是真的还是平台编的。
//
// ⚠️ 分支不存在时 ls-remote 返回**空输出且退出码为 0**，所以不能只看 runErr：
// 必须校验解析结果，否则"分支写错了"会一路走到构建才炸。
func GitResolveCommit(ctx context.Context, run CmdRunner, workRoot string, req RepositoryProbe) (*Commit, error) {
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		return nil, errors.New("未指定分支")
	}

	env, cleanup, err := prepareAskpass(workRoot, req.Username, req.Password)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, fmt.Errorf("准备凭据失败：%w", err)
	}

	var out strings.Builder
	runErr := run(ctx, Cmd{
		Dir:     workRoot,
		Env:     env,
		Name:    "git",
		Args:    []string{"ls-remote", req.URL, "refs/heads/" + branch},
		Secrets: secretList(req.Username, req.Password),
	}, func(_, line string) error {
		out.WriteString(line)
		out.WriteString("\n")
		return nil
	})

	raw := strings.TrimSpace(out.String())
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("连接超时（%s 内没有响应）：检查地址与端口，以及这台机器到它的网络是否通", probeTimeoutLabel())
		}
		return nil, errors.New(explainGitError(raw, runErr))
	}

	want := "refs/heads/" + branch
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		sha := strings.TrimSpace(parts[0])
		if sha != "" && strings.TrimSpace(parts[1]) == want {
			// Message / Author / At 留空 —— 见函数头，不编。
			return &Commit{ID: sha}, nil
		}
	}
	return nil, fmt.Errorf("分支 %q 在远端不存在（也可能是该账号看不到它）", branch)
}

// parseLsRemote 从 `git ls-remote --symref <url> HEAD refs/heads/*` 的输出里
// 取分支列表与默认分支。
//
// 输出形态：
//
//	ref: refs/heads/master	HEAD     ← 默认分支（HEAD 是个符号引用时才有这行）
//	<sha>	HEAD
//	<sha>	refs/heads/master
//	<sha>	refs/heads/release/2.6
func parseLsRemote(raw string) (branches []string, defaultBranch string) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "ref:"); ok {
			// `ref: refs/heads/master\tHEAD`
			fields := strings.Fields(rest)
			if len(fields) >= 2 && fields[1] == "HEAD" {
				defaultBranch = strings.TrimPrefix(fields[0], "refs/heads/")
			}
			continue
		}
		// `<sha>\t<refname>`
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		if name, ok := strings.CutPrefix(strings.TrimSpace(parts[1]), "refs/heads/"); ok {
			branches = append(branches, name)
		}
	}
	return branches, defaultBranch
}

// explainGitError 把 git 的失败输出翻译成用户能据此**动手**的一句话。
//
// 按"用户下一步该改什么"分类，而不是按 git 的错误码分类 ——
// 同一个 `fatal:` 前缀下，改密码和改地址是两件完全不同的事。
func explainGitError(output string, err error) string {
	low := strings.ToLower(output)
	switch {
	case containsAny(low, "authentication failed", "could not read username",
		"invalid username or password", "401", "403"):
		return "账号或密码不正确（也可能是令牌已过期、或该账号没有这个仓库的权限）"
	case containsAny(low, "repository not found", "not found", "404", "project not found"):
		return "仓库不存在：地址路径写错了，或该账号看不到这个仓库"
	case containsAny(low, "could not resolve host", "name or service not known",
		"nodename nor servname", "temporary failure in name resolution"):
		return "域名解析不了：确认 GitLab 地址（含端口）写对了"
	case containsAny(low, "connection refused", "connection timed out", "operation timed out",
		"network is unreachable", "no route to host",
		// ⚠️ 下面三个不是凑数，是**实测漏掉过**的：
		// 端口没人监听时，macOS 上的 git 报的是
		//   `fatal: unable to access '…': Failed to connect to 127.0.0.1 port 8899 after 0 ms: Couldn't connect to server`
		// —— 里面既没有 "connection refused" 也没有 "connection timed out"，
		// 于是落进兜底分支，把英文原文原样端给了用户。
		// Linux 上的 git 措辞是 "Connection refused"，所以这个坑只在 macOS 上出现，
		// 更容易被漏掉。
		"failed to connect", "couldn't connect to server", "could not connect to server",
		"unable to connect"):
		return "连不上：检查地址与端口，以及这台机器到它的网络是否通"
	case containsAny(low, "ssl certificate problem", "certificate verify failed",
		"self signed certificate", "server certificate verification failed"):
		return "HTTPS 证书校验失败：这台机器不信任该 GitLab 的证书（自签名证书需先把 CA 装上）"
	case containsAny(low, "not a git repository", "does not appear to be a git repository"):
		return "这个地址不是一个 Git 仓库：确认地址以 .git 结尾、且指向仓库根"
	case strings.Contains(low, "empty repository"):
		return "仓库是空的：还没有任何提交，先推一次代码再来构建"
	// 放在 404 / 401 之后是有意的：HTTP 状态码本身不说明任何事，
	// "仓库不存在"（404）和"账号不对"（401）都有更贴切的答案，得让它们先命中。
	// 走到这里的是 5xx —— 多半是中间代理或网关挡在了前面
	// （本机实测踩过：走了不通的代理时拿到的是 502，而不是 401）。
	case strings.Contains(low, "the requested url returned error"):
		return "仓库服务返回了错误响应（" + tailLines(output, 1) + "）：确认地址写对了，以及这台机器访问它是否需要经过代理"
	default:
		// 兜底也要给一句结论 + 原文，不能只丢一句"失败"：
		// 认不出来的错误恰恰是最需要原文的。
		//
		// 原文**优先取 git 自己的输出**，`err.Error()` 只在没有任何输出时才补 ——
		// 它通常是 `exit status 128` 这种进程级结果，对用户毫无可操作性，
		// 混在结论里只会把真正有用的那句话挤到括号深处。
		raw := tailLines(output, 3)
		if raw == "" && err != nil {
			raw = err.Error()
		}
		if raw == "" {
			return "连接失败"
		}
		return "连接失败：" + raw
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// tailLines 取最后 n 个非空行，拼成一行。给"认不出来的错误"附一句原文。
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	kept := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if v := strings.TrimSpace(lines[i]); v != "" {
			kept = append([]string{v}, kept...)
		}
	}
	return strings.Join(kept, " / ")
}

// secretList 只把**够长**的凭据列入替换表。
//
// 一个字符的密码替换起来会误伤正常文本（比如把日志里所有 "a" 都换成 ***），
// 反而让人看不出发生了什么。这类值应从源头就不允许设置。
func secretList(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if len(strings.TrimSpace(v)) >= 4 {
			out = append(out, v)
		}
	}
	return out
}

// probeTimeout 探测超时。与 service 层的 gitProbeTimeout 同值 —— 一处超时、
// 一处不超时会表现成"界面转了 20 秒然后成功"，比直接失败更让人困惑。
var probeTimeout = 20 * time.Second
