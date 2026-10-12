package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --------------------------------------------------------------------------- explainGitError
//
// explainGitError 此前**一条测试都没有**——这正是它漏掉一种真实措辞却没人发现的原因：
// 端口没人监听时，macOS 上的 git 报的是
//
//	fatal: unable to access '…': Failed to connect to 127.0.0.1 port 8899 after 0 ms: Couldn't connect to server
//
// 里面既没有 "connection refused" 也没有 "connection timed out"，于是落进兜底分支，
// 把英文原文原样端给了用户。Linux 上的 git 说的是 "Connection refused"，
// 所以这个坑只在 macOS 上出现，更容易被漏掉。
func TestExplainGitError(t *testing.T) {
	cases := []struct {
		name string
		// output 是本机实测抓到的 git 原文，不是照着代码猜的。
		output string
		// recognized 为 true 表示这条错误**应该**被认出来（命中某个具名分支），
		// 于是它绝不能吐 grep 式原文，也绝不能带进程级的噪音。
		recognized  bool
		contains    []string
		notContains []string
	}{
		{
			name: "macOS 端口没人监听（本轮实测原文）",
			output: "fatal: unable to access 'http://127.0.0.1:8899/maven-script.git/': " +
				"Failed to connect to 127.0.0.1 port 8899 after 0 ms: Couldn't connect to server",
			recognized:  true,
			contains:    []string{"连不上", "端口"},
			notContains: []string{"Failed to connect", "Couldn't connect"},
		},
		{
			name:        "Linux 端口没人监听（措辞不同，同一件事）",
			output:      "fatal: unable to access 'http://10.0.0.1:8080/x.git/': Failed to connect to 10.0.0.1 port 8080: Connection refused",
			recognized:  true,
			contains:    []string{"连不上"},
			notContains: []string{"Connection refused"},
		},
		{
			name:        "凭据不对",
			output:      "fatal: Authentication failed for 'http://10.0.0.1/x.git/'",
			recognized:  true,
			contains:    []string{"账号或密码不正确"},
			notContains: []string{"Authentication failed"},
		},
		{
			name:        "仓库不存在",
			output:      "remote: The project you were looking for could not be found or you don't have permission to view it.\nfatal: repository 'http://10.0.0.1/x/y.git/' not found",
			recognized:  true,
			contains:    []string{"仓库不存在"},
			notContains: []string{"not found"},
		},
		{
			name:        "域名解析不了",
			output:      "fatal: unable to access 'http://gitlab.internal/x/y.git/': Could not resolve host: gitlab.internal",
			recognized:  true,
			contains:    []string{"域名解析不了"},
			notContains: []string{"Could not resolve host"},
		},
		{
			name:        "证书校验失败",
			output:      "fatal: unable to access 'https://10.0.0.1/x/y.git/': SSL certificate problem: self signed certificate",
			recognized:  true,
			contains:    []string{"证书校验失败"},
			notContains: []string{"self signed certificate"},
		},
		{
			name:        "不是一个 git 仓库",
			output:      "fatal: 'http://10.0.0.1/x' does not appear to be a git repository",
			recognized:  true,
			contains:    []string{"不是一个 Git 仓库"},
			notContains: []string{"does not appear to be"},
		},
		{
			name:        "仓库是空的",
			output:      "warning: You appear to have cloned an empty repository.",
			recognized:  true,
			contains:    []string{"仓库是空的"},
			notContains: []string{"empty repository"},
		},
		{
			// ⚠️ 顺序回归：这条同时含 "not found" 吗？不含。但它含 502，
			// 必须**不能**被 404 那条抢走。
			name:       "经代理拿到 502（实测踩过：走了不通的代理时不是 401 而是 502）",
			output:     "fatal: unable to access 'https://gitlab.example.com/x/y.git/': The requested URL returned error: 502",
			recognized: true,
			contains:   []string{"错误响应", "502"},
		},
		{
			// ⚠️ 顺序回归：含 "not found" 的仓库地址必须仍然被认成"仓库不存在"，
			// 不能被上面那条 5xx 分支提前截胡。
			name:        "404 仍归到「仓库不存在」而不是泛化的错误响应",
			output:      "fatal: unable to access 'https://gitlab.example.com/x/y.git/': The requested URL returned error: 404",
			recognized:  true,
			contains:    []string{"仓库不存在"},
			notContains: []string{"错误响应"},
		},
		{
			name:   "认不出来的错误：必须附上原文",
			output: "fatal: something nobody has seen before",
			// 未识别 → 兜底会给原文，这是**刻意的**：认不出来的错误恰恰最需要原文。
			contains:    []string{"连接失败", "fatal: something nobody has seen before"},
			notContains: []string{"exit status"},
		},
		{
			name:     "认不出来的错误且没有任何输出",
			output:   "   ",
			contains: []string{"连接失败"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 传一个真实的进程级错误：兜底分支曾把它拼进结论，用户看到 "exit status 128"。
			got := explainGitError(tc.output, errors.New("exit status 128"))

			for _, s := range tc.contains {
				if !strings.Contains(got, s) {
					t.Errorf("结论里缺少 %q\n实际：%s", s, got)
				}
			}
			for _, s := range tc.notContains {
				if strings.Contains(got, s) {
					t.Errorf("结论里不该出现 %q（原文泄漏）\n实际：%s", s, got)
				}
			}
			if got == "" {
				t.Error("结论是空串：界面会显示一片空白，比报错更难排查")
			}
			if tc.recognized {
				// 被认出来的错误不该再冒出进程级噪音。
				if strings.Contains(got, "exit status") {
					t.Errorf("已识别的错误仍带上了进程级噪音\n实际：%s", got)
				}
				if strings.Contains(got, "code=") {
					t.Errorf("结论里混进了日志格式（code= 是给日志看的）\n实际：%s", got)
				}
			}
		})
	}
}

// --------------------------------------------------------------------------- parseLsRemote

func TestParseLsRemote(t *testing.T) {
	const sample = "ref: refs/heads/master\tHEAD\n" +
		"674da0871beb8d8088b3c0717b66554a160a3305\tHEAD\n" +
		"674da0871beb8d8088b3c0717b66554a160a3305\trefs/heads/master\n" +
		"1111111111111111111111111111111111111111\trefs/heads/release/2.6\n" +
		"2222222222222222222222222222222222222222\trefs/tags/v1.0\n"

	branches, def := parseLsRemote(sample)

	if def != "master" {
		t.Errorf("默认分支 = %q，期望 master", def)
	}
	// 标签**不是**分支：refs/tags/* 必须被滤掉，否则下拉里会出现选不了的"分支"。
	want := []string{"master", "release/2.6"}
	if len(branches) != len(want) {
		t.Fatalf("分支数 = %d（%v），期望 %d（%v）", len(branches), branches, len(want), want)
	}
	for i := range want {
		if branches[i] != want[i] {
			t.Errorf("分支[%d] = %q，期望 %q", i, branches[i], want[i])
		}
	}
}

func TestParseLsRemoteDetachedHead(t *testing.T) {
	// HEAD 不是符号引用（分离头指针的裸仓库）时没有 `ref:` 那一行，
	// 此时只能给分支列表、给不出默认分支 —— 不能编一个。
	branches, def := parseLsRemote("abc\trefs/heads/main\n")
	if def != "" {
		t.Errorf("读不到默认分支时应为空，实际 %q", def)
	}
	if len(branches) != 1 || branches[0] != "main" {
		t.Errorf("分支 = %v，期望 [main]", branches)
	}
}

// --------------------------------------------------------------------------- GitResolveCommit

// fakeRunner 假执行器：把预置的每一行喂给 onLine，最后返回 err。
func fakeRunner(lines []string, err error) CmdRunner {
	return func(_ context.Context, _ Cmd, onLine func(level, line string) error) error {
		for _, l := range lines {
			if e := onLine("stdout", l); e != nil {
				return e
			}
		}
		return err
	}
}

func TestGitResolveCommitReturnsSHAOnly(t *testing.T) {
	const sha = "674da0871beb8d8088b3c0717b66554a160a3305"
	run := fakeRunner([]string{sha + "\trefs/heads/master"}, nil)

	c, err := GitResolveCommit(context.Background(), run, t.TempDir(), RepositoryProbe{
		URL: "http://127.0.0.1:8900/maven-script.git", Branch: "master",
	})
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if c.ID != sha {
		t.Errorf("提交号 = %q，期望 %q", c.ID, sha)
	}
	// ⚠️ 这三条是"不编"的锁：`ls-remote` 只给得出 sha，给不出提交信息与作者。
	// 从前 LocalRepository 会把 "feat: 支持按环境下发配置" 和 "CI Bot" 一起编出来，
	// 然后写进 build_record.commit_msg —— 事后没人分得清哪条是真的。
	if c.Message != "" {
		t.Errorf("提交信息必须留空（ls-remote 给不出），实际 %q", c.Message)
	}
	if c.Author != "" {
		t.Errorf("作者必须留空（ls-remote 给不出），实际 %q", c.Author)
	}
	if !c.At.IsZero() {
		t.Errorf("提交时间必须留空（ls-remote 给不出），实际 %v", c.At)
	}
}

func TestGitResolveCommitMissingBranch(t *testing.T) {
	// 分支不存在时 ls-remote **输出为空且退出码为 0** —— 所以不能只看 runErr，
	// 否则"分支名写错了"会一路走到构建才炸。
	run := fakeRunner(nil, nil)

	_, err := GitResolveCommit(context.Background(), run, t.TempDir(), RepositoryProbe{
		URL: "http://127.0.0.1:8900/maven-script.git", Branch: "no-such-branch",
	})
	if err == nil {
		// 这里正是「编一个提交号」的入口：拿不到就绝不能往下走。
		t.Fatal("分支不存在却返回了成功")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误信息要能看出是分支不存在，实际：%v", err)
	}
}

func TestGitResolveCommitTranslateFailure(t *testing.T) {
	// 探测失败时错误必须是**人话**，不能把 git 原文端出去。
	run := fakeRunner(
		[]string{"fatal: Authentication failed for 'http://127.0.0.1:8900/maven-script.git/'"},
		errors.New("exit status 128"))

	_, err := GitResolveCommit(context.Background(), run, t.TempDir(), RepositoryProbe{
		URL: "http://127.0.0.1:8900/maven-script.git", Branch: "master",
	})
	if err == nil {
		t.Fatal("探测失败却返回了成功")
	}
	if !strings.Contains(err.Error(), "账号或密码不正确") {
		t.Errorf("错误信息没翻成人话，实际：%v", err)
	}
	if strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("git 原文泄漏到结论里了：%v", err)
	}
}

func TestGitResolveCommitRequiresBranch(t *testing.T) {
	_, err := GitResolveCommit(context.Background(), fakeRunner(nil, nil), t.TempDir(),
		RepositoryProbe{URL: "http://x/y.git", Branch: "  "})
	if err == nil {
		t.Fatal("未指定分支却继续执行")
	}
}
