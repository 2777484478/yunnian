package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/2777484478/yunnian/internal/config"
)

// 本文件提供真实下游的 HTTP 实现。
//
// 都直接用标准库 net/http 调 REST 接口，不引各家的 SDK。原因：
//   - 平台用到的接口面很窄（Harbor 只需列 artifacts、GitLab 只需分支与提交），
//     引 SDK 会带进远超所需的依赖与版本约束；
//   - 内部系统常自签证书、走网关改写路径，裸 HTTP 更容易按现场情况调整。

// --------------------------------------------------------------------------- GitLab

// GitLabRepository 通过 GitLab REST API v4 读取分支与提交。
type GitLabRepository struct {
	base   string
	client *http.Client
	// auth 决定每次请求带什么鉴权头。两种模式的头**不一样**，这不是风格问题：
	// OAuth 换来的 token 放 PRIVATE-TOKEN 会稳定 401（实测），必须走 Bearer。
	// 为 nil 表示匿名（未配置任何凭据）。
	auth gitlabAuth
}

// NewGitLabRepository 构造。
func NewGitLabRepository(cfg config.GitLabConfig) *GitLabRepository {
	timeout := cfg.Timeout.D()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}

	var auth gitlabAuth
	if cfg.NormalizedAuthMode() == config.GitLabAuthPassword {
		// 账号密码：用户名密码是否填全由 bridge.live() 负责拦，
		// 这里只负责把 auth 装上 —— 让"能连上但缺凭据"报出具体缺什么，
		// 而不是静默退化成匿名请求（匿名访问私有仓库只会得到 404，更难查）。
		auth = oauthAuth{m: oauthManagerFor(cfg, client)}
	} else if tok := strings.TrimSpace(cfg.Token); tok != "" {
		auth = staticTokenAuth{token: tok}
	}

	return &GitLabRepository{
		base:   strings.TrimRight(cfg.BaseURL, "/"),
		client: client,
		auth:   auth,
	}
}

// Name 实现名。
func (g *GitLabRepository) Name() string { return "gitlab" }

// Ping 调用 version 接口探测连通性与令牌有效性。
func (g *GitLabRepository) Ping(ctx context.Context) error {
	var out map[string]any
	return g.get(ctx, "/api/v4/version", nil, &out)
}

// GitLabProbe 一次连通性探测的结果。
//
// 为什么要一次拿三样东西（版本、令牌身份、令牌元数据），而不是只 Ping 一下：
// 运维配错时最需要的不是"失败"两个字，而是**错在哪**。
//
//   - 地址写错 / 网络不通 → 请求本身报错；
//   - 令牌无效或已过期   → 401；
//   - 令牌有效但作用域不足（例如只给了 read_user）→ **测试时一切正常**，
//     真正列分支/拉代码时才失败。
//
// 所以把作用域与过期时间一并读回来：前者让"配了但不够用"当场暴露，
// 后者让"一周后构建突然全挂"提前可见（PAT 到期是这类平台最常见的定时故障）。
type GitLabProbe struct {
	Version  string `json:"version"`
	Revision string `json:"revision"`
	// 令牌代表谁：PAT 的身份完全由令牌自己决定，页面上要能看清"用的是谁的账号"。
	Username string `json:"username"`
	Name     string `json:"name"`
	IsAdmin  bool   `json:"isAdmin"`
	// 令牌元数据。老版本 GitLab（< 13.10）没有这个接口，取不到时留空，不影响测试结论。
	TokenName     string   `json:"tokenName"`
	Scopes        []string `json:"scopes"`
	ExpiresAt     string   `json:"expiresAt"`
	TokenMetaNote string   `json:"tokenMetaNote"`
}

// Probe 依次探测版本、令牌身份与令牌元数据。
//
// 版本与身份任一失败即判定连通性不合格（它们决定"能不能用"）；
// 令牌元数据失败只记一句说明——不能因为一个可选接口不存在，
// 就把一个其实能正常工作的连接报成故障。
func (g *GitLabRepository) Probe(ctx context.Context) (*GitLabProbe, error) {
	out := &GitLabProbe{}

	var ver struct {
		Version  string `json:"version"`
		Revision string `json:"revision"`
	}
	if err := g.get(ctx, "/api/v4/version", nil, &ver); err != nil {
		return nil, err
	}
	out.Version, out.Revision = ver.Version, ver.Revision

	var me struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if err := g.get(ctx, "/api/v4/user", nil, &me); err != nil {
		return nil, fmt.Errorf("令牌身份读取失败（令牌可能无效、已过期或作用域不足）: %w", err)
	}
	out.Username, out.Name, out.IsAdmin = me.Username, me.Name, me.IsAdmin

	var tok struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresAt string   `json:"expires_at"`
	}
	ok, err := g.getOptional(ctx, "/api/v4/personal_access_tokens/self", &tok)
	switch {
	case err != nil:
		out.TokenMetaNote = "令牌元数据读取失败：" + err.Error()
	case !ok:
		if g.auth != nil && g.auth.Mode() == config.GitLabAuthPassword {
			// 账号密码模式拿到的是 OAuth token，不是 PAT，这个接口天然查不到 ——
			// 说成"GitLab 版本太老"会把人往错方向带。
			out.TokenMetaNote = "账号密码模式：令牌由平台自动换取并续期（2 小时一轮），没有作用域/到期时间可读"
		} else {
			out.TokenMetaNote = "当前 GitLab 版本不支持 /personal_access_tokens/self（需 13.10+），无法自动检查令牌作用域与到期时间"
		}
	default:
		out.TokenName, out.Scopes, out.ExpiresAt = tok.Name, tok.Scopes, tok.ExpiresAt
		// 用组 / 项目令牌时该接口会返回 404，此时作用域同样读不到。
		if len(tok.Scopes) == 0 {
			out.TokenMetaNote = "该令牌不是个人访问令牌（可能是项目/组令牌或 CI Job 令牌），平台读不到它的作用域"
		}
	}
	return out, nil
}

// getOptional 与 get 相同，但把 404 视作"这个接口不存在"而不是失败。
// 用于探测那些随 GitLab 版本增减的可选接口。
func (g *GitLabRepository) getOptional(ctx context.Context, path string, out any) (bool, error) {
	status, body, err := g.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if status >= 300 {
		return false, UpstreamError("GitLab", fmt.Errorf("HTTP %d: %s", status, truncate(string(body), 200)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return false, UpstreamError("GitLab", fmt.Errorf("响应解析失败: %w", err))
	}
	return true, nil
}

// Branches 列出分支名。
func (g *GitLabRepository) Branches(ctx context.Context, gitURL string) ([]string, error) {
	project, err := projectPath(gitURL)
	if err != nil {
		return nil, err
	}
	var branches []struct {
		Name string `json:"name"`
	}
	q := url.Values{}
	q.Set("per_page", "100")
	if err := g.get(ctx, fmt.Sprintf("/api/v4/projects/%s/repository/branches", url.PathEscape(project)), q, &branches); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(branches))
	for _, b := range branches {
		out = append(out, b.Name)
	}
	return out, nil
}

// LatestCommit 取分支最新提交。
func (g *GitLabRepository) LatestCommit(ctx context.Context, gitURL, branch string) (*Commit, error) {
	project, err := projectPath(gitURL)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("ref_name", branch)
	q.Set("per_page", "1")

	var commits []struct {
		ID            string `json:"id"`
		Message       string `json:"title"`
		AuthorName    string `json:"author_name"`
		CommittedDate string `json:"committed_date"`
	}
	path := fmt.Sprintf("/api/v4/projects/%s/repository/commits", url.PathEscape(project))
	if err := g.get(ctx, path, q, &commits); err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("分支 %s 没有提交记录", branch)
	}
	at, _ := time.Parse(time.RFC3339, commits[0].CommittedDate)
	return &Commit{
		ID:      commits[0].ID,
		Message: commits[0].Message,
		Author:  commits[0].AuthorName,
		At:      at,
	}, nil
}

func (g *GitLabRepository) get(ctx context.Context, path string, q url.Values, out any) error {
	status, body, err := g.do(ctx, http.MethodGet, path, q)
	if err != nil {
		return err
	}
	if status >= 300 {
		return UpstreamError("GitLab", fmt.Errorf("HTTP %d: %s", status, truncate(string(body), 200)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return UpstreamError("GitLab", fmt.Errorf("响应解析失败: %w", err))
	}
	return nil
}

// do 发一次请求，返回状态码与响应体。
//
// **401 会换一次令牌重试**：OAuth 的 access_token 响应里没有 expires_in（只有 created_at），
// 平台只能按默认 2 小时提前刷新 —— 提前量估错、或者令牌被 GitLab 侧提前撤销时，
// 就会先用一个过期 token 撞一次 401。没有这个重试，用户看到的是"偶尔一次莫名失败"。
//
// 只对 password 模式重试：静态令牌重试没有意义（它不会因为再取一次而变有效），
// 多打一次只会让人以为是网络抖动。
func (g *GitLabRepository) do(ctx context.Context, method, path string, q url.Values) (int, []byte, error) {
	u := g.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Accept", "application/json")
		if g.auth != nil {
			key, val, herr := g.auth.Header(ctx)
			if herr != nil {
				// 已经是 UpstreamError（换令牌失败的那条会自带"GitLab 调用失败"前缀），
				// 再包一层会得到 "GitLab 调用失败: GitLab 调用失败: ..." —— 实测踩过。
				return 0, nil, herr
			}
			if key != "" {
				req.Header.Set(key, val)
			}
		}

		resp, err := g.client.Do(req)
		if err != nil {
			return 0, nil, UpstreamError("GitLab", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 &&
			g.auth != nil && g.auth.Mode() == config.GitLabAuthPassword {
			g.auth.Invalidate()
			continue
		}
		return resp.StatusCode, body, nil
	}
	// 两轮都 401：说明问题不在"令牌过期"，而是账号密码本身或权限有问题。
	return http.StatusUnauthorized, nil, UpstreamError("GitLab",
		fmt.Errorf("换到新令牌后仍被拒绝（401）：请确认该账号是否仍有 API 权限，或改用访问令牌"))
}

// projectPath 从各种 Git 地址形态里抽出 GitLab 的 project path。
// 支持 git@host:group/repo.git、ssh://git@host/group/repo.git、https://host/group/repo.git。
func projectPath(gitURL string) (string, error) {
	s := strings.TrimSpace(gitURL)
	s = strings.TrimSuffix(s, ".git")

	switch {
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("无法解析仓库地址 %q: %w", gitURL, err)
		}
		return strings.Trim(u.Path, "/"), nil
	case strings.HasPrefix(s, "ssh://"):
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("无法解析仓库地址 %q: %w", gitURL, err)
		}
		return strings.Trim(u.Path, "/"), nil
	default:
		// scp 形式：git@host:group/repo
		i := strings.Index(s, ":")
		if i < 0 {
			return "", fmt.Errorf("无法识别的仓库地址 %q", gitURL)
		}
		return strings.Trim(s[i+1:], "/"), nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
