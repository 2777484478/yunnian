package integration

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/yunnian/yunnian/internal/config"
)

// GitLabConfigSource 运行期解析 GitLab 连接配置。
//
// 返回的 Enabled 决定平台此刻该走真实 GitLab 还是内置本地实现。
// 之所以做成"每次调用解析一次"而不是启动时固化：配置固化在启动参数里，
// 是这类平台最常见的运维抱怨——界面上改了地址或换了令牌，
// 却发现要重启整个平台才生效，于是没人敢改。
type GitLabConfigSource func(ctx context.Context) (config.GitLabConfig, error)

// GitLabBridge 让 GitLab 接入可以在运行期切换实现，而不必重启平台。
//
// 它做一件事：**二选一**。配置里 enabled=true 时走真实 GitLab REST API，
// 否则把调用透传给内置本地实现——保留"缺少下游时平台仍能跑通编排"的原有语义。
//
// 与直接把 *GitLabRepository 装进 Set 的区别有三点，每一点都对应一个具体故障：
//
//  1. **每个方法调用时都重新解析配置** —— 否则在线换令牌后仍在用旧令牌，
//     表现为"明明改了却一直 401"。
//  2. **未启用时透传本地实现** —— 否则按启动时的 enabled 二选一，
//     用户刚在界面上打开开关，页面却仍显示"本地模拟"，只能靠重启自证。
//  3. **Name() 用最近一次解析结果**（它没有 ctx）—— 由 Refresh 主动更新，
//     System.Integrations 在读实现名之前会先调一次 Refresh，
//     否则状态卡片上写的是启动那一刻的实现名。
type GitLabBridge struct {
	fallback Repository
	source   GitLabConfigSource

	mu   sync.RWMutex
	last config.GitLabConfig
}

// NewGitLabBridge 构造。
//
// source 为 nil 时退化为"只用传入的静态配置"，与加桥接之前的行为完全一致
// （static 即 config.yaml 里的 integration.gitlab）。这条退化路径是刻意保留的：
// 单元测试与不使用 DB 配置的部署形态都不需要动态来源。
func NewGitLabBridge(fallback Repository, static config.GitLabConfig, source GitLabConfigSource) *GitLabBridge {
	return &GitLabBridge{fallback: fallback, source: source, last: static}
}

// resolve 解析当前配置。
//
// 解析失败时**不退回上一次的值**，而是把错误上抛：宁可让调用方看到
// "配置读不出来"，也不要静默拿一个过期的地址去请求——后者会让人
// 对着一个其实没生效的配置排查半天。
func (b *GitLabBridge) resolve(ctx context.Context) (config.GitLabConfig, error) {
	if b.source == nil {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.last, nil
	}
	cfg, err := b.source(ctx)
	if err != nil {
		return config.GitLabConfig{}, err
	}
	b.mu.Lock()
	b.last = cfg
	b.mu.Unlock()
	return cfg, nil
}

// Refresh 重新解析一次配置，只为把最新结果记进 Name() 用的缓存。
//
// 不返回错误：调用方是「集成状态」接口，它本身就要容错展示
// （读不到配置时靠 Ping 报故障，而不是整个接口 500）。
func (b *GitLabBridge) Refresh(ctx context.Context) { _, _ = b.resolve(ctx) }

// Auth 返回检出代码（git clone）时该用的凭据。
//
// 为什么不直接把 token 拼进 clone 的 URL 让调用方自己填：
// 凭据的**取值方式**（配置里只存一个 token）属于 GitLab 这条通道的知识，
// 让上层去拼 `oauth2:<token>@host` 等于把 GitLab 的认证约定泄漏到 CI 服务里，
// 将来换一种认证方式就得回来改 CI 代码。
//
// 用户名固定 oauth2：GitLab 对「任意非空用户名 + 令牌作为密码」都接受，
// 但这个值是官方文档里的写法，有 2FA 的账号不会因此被拒。
//
// 返回 ok=false 表示"没有可用凭据"（未启用 / 未配置 token），
// 调用方应当**继续用匿名方式检出**，而不是把这次构建判失败 ——
// 有的仓库就是公开可读的。
func (b *GitLabBridge) Auth(ctx context.Context) (username, password string, ok bool) {
	cfg, err := b.resolve(ctx)
	if err != nil || !cfg.Enabled {
		return "", "", false
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return "", "", false
	}
	return "oauth2", token, true
}

// Name 实现名。Enabled 时是 gitlab，否则跟随兜底实现（local）。
func (b *GitLabBridge) Name() string {
	b.mu.RLock()
	enabled := b.last.Enabled
	b.mu.RUnlock()
	if enabled {
		return "gitlab"
	}
	return b.fallback.Name()
}

// live 按当前配置返回真实实现；未启用时返回 (nil, nil) 表示"该走兜底"。
func (b *GitLabBridge) live(ctx context.Context) (*GitLabRepository, error) {
	cfg, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, UpstreamError("GitLab", errors.New("已启用 GitLab 接入但未填写服务地址"))
	}
	return NewGitLabRepository(cfg), nil
}

// Ping 连通性探测。
func (b *GitLabBridge) Ping(ctx context.Context) error {
	repo, err := b.live(ctx)
	if err != nil {
		return err
	}
	if repo == nil {
		return b.fallback.Ping(ctx)
	}
	return repo.Ping(ctx)
}

// Branches 列出分支名。
func (b *GitLabBridge) Branches(ctx context.Context, gitURL string) ([]string, error) {
	repo, err := b.live(ctx)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return b.fallback.Branches(ctx, gitURL)
	}
	return repo.Branches(ctx, gitURL)
}

// LatestCommit 取分支最新提交。
func (b *GitLabBridge) LatestCommit(ctx context.Context, gitURL, branch string) (*Commit, error) {
	repo, err := b.live(ctx)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return b.fallback.LatestCommit(ctx, gitURL, branch)
	}
	return repo.LatestCommit(ctx, gitURL, branch)
}
