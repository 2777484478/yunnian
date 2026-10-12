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

	"github.com/yunnian/yunnian/internal/config"
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
	base  string
	token string
	client *http.Client
}

// NewGitLabRepository 构造。
func NewGitLabRepository(cfg config.GitLabConfig) *GitLabRepository {
	timeout := cfg.Timeout.D()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &GitLabRepository{
		base:   strings.TrimRight(cfg.BaseURL, "/"),
		token:  cfg.Token,
		client: &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
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
		out.TokenMetaNote = "当前 GitLab 版本不支持 /personal_access_tokens/self（需 13.10+），无法自动检查令牌作用域与到期时间"
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
	u := g.base + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	if g.token != "" {
		req.Header.Set("PRIVATE-TOKEN", g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return false, UpstreamError("GitLab", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= 300 {
		return false, UpstreamError("GitLab", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200)))
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
	u := g.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if g.token != "" {
		// GitLab 同时接受 PRIVATE-TOKEN 与 Bearer，这里用前者（机器人令牌的通用写法）
		req.Header.Set("PRIVATE-TOKEN", g.token)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return UpstreamError("GitLab", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return UpstreamError("GitLab", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return UpstreamError("GitLab", fmt.Errorf("响应解析失败: %w", err))
	}
	return nil
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
