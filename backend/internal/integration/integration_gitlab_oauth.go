package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/2777484478/yunnian/internal/config"
)

// --------------------------------------------------------------------------- GitLab 鉴权
//
// 两种方式，同一套接口：
//
//  1. **访问令牌（PAT）** —— 固定值，放 `PRIVATE-TOKEN` 头。
//  2. **账号密码** —— 平台自己用 **OAuth2 password grant** 换 access_token，
//     放 `Authorization: Bearer`。
//
// ⚠️ 为什么不是 `POST /api/v4/session`（很多老资料写的做法）：
// 实测目标 GitLab（47.100.239.15:22691）该端点已是 **404** —— 新版 GitLab 把它移除了。
// 而 `POST /oauth/token` 的 password grant **不需要 client_id** 就能成功：
//
//	POST /oauth/token  grant_type=password&username=..&password=..
//	→ {"access_token":"..","refresh_token":"..","scope":"api","created_at":..}
//
// ⚠️ 换来的 token **只能用 Bearer**：实测同一个 token 放 `PRIVATE-TOKEN` 头会稳定 401
// （`{"message":"401 Unauthorized"}`）。这就是为什么下面要区分 authMode，
// 而不是"拿到 token 就当 PAT 用"。
//
// ⚠️ 响应里**没有 `expires_in`**，只有 `created_at`。所以不能靠它判过期，
// 只能：① 按 GitLab 默认 2 小时提前刷新；② 请求遇 401 时强制重取一次。
// 两条都要，缺一条就会出现"明明能续期却报一次 401"或者"一直用过期 token"。

// gitlabAuth 一次请求要用的鉴权头。
//
// 抽成接口而不是 if/else：取头的代价不同（静态令牌零成本，OAuth 可能触发一次网络往返），
// 调用方不该关心这件事。
type gitlabAuth interface {
	// Header 返回本次请求的鉴权头；key 为空表示匿名。
	Header(ctx context.Context) (key, value string, err error)
	// Invalidate 丢弃缓存的令牌，让下一次 Header 重新取。
	// 调用时机是"服务端说这个令牌不认了"（401）。
	Invalidate()
	// Mode 返回认证方式，供探测结果生成说明文案。
	Mode() string
}

// staticTokenAuth 固定的访问令牌（PAT / 项目令牌 / 组令牌都走这条）。
type staticTokenAuth struct{ token string }

func (a staticTokenAuth) Header(context.Context) (string, string, error) {
	if a.token == "" {
		return "", "", nil
	}
	return "PRIVATE-TOKEN", a.token, nil
}
func (a staticTokenAuth) Invalidate() {}
func (a staticTokenAuth) Mode() string { return config.GitLabAuthToken }

// oauthAuth 账号密码模式：每次取头时确保有一个可用 token。
type oauthAuth struct{ m *oauthTokenManager }

func (a oauthAuth) Header(ctx context.Context) (string, string, error) {
	tok, err := a.m.Token(ctx)
	if err != nil {
		return "", "", err
	}
	return "Authorization", "Bearer " + tok, nil
}
func (a oauthAuth) Invalidate() { a.m.Invalidate() }
func (a oauthAuth) Mode() string {
	if a.m == nil {
		return config.GitLabAuthPassword
	}
	return config.GitLabAuthPassword
}

// oauthTokenTTL GitLab OAuth access_token 的默认有效期。
//
// 响应里不给 expires_in，所以这里按官方默认值（2 小时）算，并提前
// oauthRefreshAhead 刷新 —— 提前量是给"续期请求本身要走网络"留的余量。
const (
	oauthTokenTTL     = 2 * time.Hour
	oauthRefreshAhead = 15 * time.Minute
)

// oauthTokenManager 账号密码换 token、并负责续期。
//
// 必须**跨请求复用**：每次请求新建一个 manager 就等于每次重新登录，
// 既打爆 GitLab 的登录接口，又让"2 小时有效"这件事失去意义。
type oauthTokenManager struct {
	base     string
	username string
	password string
	client   *http.Client

	mu       sync.Mutex
	access   string
	refresh  string
	obtained time.Time
}

func (m *oauthTokenManager) Mode() string { return config.GitLabAuthPassword }

// Invalidate 丢弃当前 token。下一次 Token() 会先试 refresh，再退回账号密码登录。
func (m *oauthTokenManager) Invalidate() {
	m.mu.Lock()
	m.access = ""
	m.mu.Unlock()
}

// Token 取一个可用的 access_token。
func (m *oauthTokenManager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.access != "" && time.Since(m.obtained) < oauthTokenTTL-oauthRefreshAhead {
		return m.access, nil
	}
	// 先试 refresh：它比"重新提交密码"更轻，也不会因为账号密码被改而连带失效。
	if m.refresh != "" {
		if err := m.exchangeLocked(ctx, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {m.refresh},
		}); err == nil {
			return m.access, nil
		}
		// refresh 失败（refresh_token 已被用过 / 过期 / 被撤销）→ 直接退回密码登录。
		// ⚠️ refresh_token 是**轮换**的：每次续期都会换发新的，旧的立即失效。
		// 所以这里必须清掉，否则会拿着一个已知失效的值反复重试。
		m.refresh = ""
	}
	if err := m.loginLocked(ctx); err != nil {
		return "", err
	}
	return m.access, nil
}

// loginLocked 用账号密码换 token。调用方必须已持锁。
func (m *oauthTokenManager) loginLocked(ctx context.Context) error {
	return m.exchangeLocked(ctx, url.Values{
		"grant_type": {"password"},
		"username":   {m.username},
		"password":   {m.password},
	})
}

// exchangeLocked 调 /oauth/token 换 token。调用方必须已持锁。
func (m *oauthTokenManager) exchangeLocked(ctx context.Context, form url.Values) error {
	u := m.base + "/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return UpstreamError("GitLab", fmt.Errorf("换取令牌失败（网络 / 证书 / 地址）：%w", err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		TokenType        string `json:"token_type"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return UpstreamError("GitLab", fmt.Errorf("换取令牌响应无法解析（HTTP %d）：%s",
			resp.StatusCode, truncate(string(body), 160)))
	}

	if resp.StatusCode >= 300 || out.Error != "" {
		return UpstreamError("GitLab", fmt.Errorf("%s", oauthTokenErrorText(resp.StatusCode, out.Error, out.ErrorDescription)))
	}
	if out.AccessToken == "" {
		return UpstreamError("GitLab", fmt.Errorf("换取令牌成功但响应里没有 access_token（HTTP %d）", resp.StatusCode))
	}

	m.access = out.AccessToken
	// 有的情况（如续期失败降级）不返回新的 refresh_token，此时保留原值。
	if out.RefreshToken != "" {
		m.refresh = out.RefreshToken
	}
	m.obtained = time.Now()
	return nil
}

// oauthTokenErrorText 把 OAuth 错误翻成人话。
//
// 这几句是运维真正会撞上的，直接给人看：
//   - invalid_grant + 密码模式 → 账号密码错 **或** 账号开了两步验证
//     （GitLab 对 2FA 账号直接拒绝 password grant，报错信息却只说 invalid_grant，
//      不点明就会被当成"密码填错了"反复试）
//   - invalid_client → 网关/代理改写了请求，或这台 GitLab 关掉了 password grant
func oauthTokenErrorText(status int, code, desc string) string {
	switch code {
	case "invalid_grant":
		return "GitLab 拒绝了账号密码（invalid_grant）：账号或密码不对，" +
			"或者该账号启用了两步验证（2FA）—— 开 2FA 的账号不支持账号密码登录，请改用访问令牌"
	case "invalid_client":
		return "GitLab 拒绝了客户端（invalid_client）：该实例可能关闭了密码模式，" +
			"或被网关改写了请求，请改用访问令牌"
	case "unsupported_grant_type":
		return "该 GitLab 实例不支持这种换取方式（unsupported_grant_type），请改用访问令牌"
	}
	if desc != "" {
		return fmt.Sprintf("换取令牌失败（HTTP %d）：%s", status, desc)
	}
	return fmt.Sprintf("换取令牌失败（HTTP %d）", status)
}

// --------------------------------------------------------------------------- manager 复用

var (
	oauthMu       sync.Mutex
	oauthManagers = map[string]*oauthTokenManager{}
)

// oauthManagerFor 按「地址 + 用户名 + 密码指纹」复用 manager。
//
// key 里用密码的 SHA-256 前 16 位而不是明文：map 会长驻内存，
// 明文密码放在 key 里等于多一份长期驻留的副本，没有任何好处。
// 密码一改 key 就变，自然拿到一个新 manager（旧的那个不再被引用）。
func oauthManagerFor(cfg config.GitLabConfig, client *http.Client) *oauthTokenManager {
	sum := sha256.Sum256([]byte(cfg.Password))
	key := strings.TrimRight(cfg.BaseURL, "/") + "|" + strings.TrimSpace(cfg.Username) + "|" +
		hex.EncodeToString(sum[:8])

	oauthMu.Lock()
	defer oauthMu.Unlock()
	if m, ok := oauthManagers[key]; ok {
		// 复用 client：超时可能被改过，跟最新的走。
		m.client = client
		return m
	}
	m := &oauthTokenManager{
		base:     strings.TrimRight(cfg.BaseURL, "/"),
		username: strings.TrimSpace(cfg.Username),
		password: cfg.Password,
		client:   client,
	}
	oauthManagers[key] = m
	return m
}

// ResetGitLabOAuthCache 清空所有缓存的 OAuth 令牌。
//
// 配置保存后要调一次：否则"改了密码但平台还在用旧 token"会持续到缓存过期
// （最长将近 2 小时），表现是"我明明改了密码，它还在用旧账号跑"。
func ResetGitLabOAuthCache() {
	oauthMu.Lock()
	oauthManagers = map[string]*oauthTokenManager{}
	oauthMu.Unlock()
}
