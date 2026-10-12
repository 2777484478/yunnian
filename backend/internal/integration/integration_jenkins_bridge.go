package integration

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/yunnian/yunnian/internal/config"
)

// ErrJenkinsDisabled 表示"平台没有启用 Jenkins 接入"。
//
// 单独做成哨兵错误而不是普通字符串，是为了让上层能稳定地把它翻译成
// 一句人话（"请在「系统管理 › 第三方集成」启用 Jenkins 接入"），
// 而不是把内部错误原样抛给用户。
var ErrJenkinsDisabled = errors.New("Jenkins 接入未启用")

// JenkinsConfigSource 运行期解析 Jenkins 连接配置。
//
// 与 GitLab 同一个理由：配置固化在启动参数里，是这类平台最常见的运维抱怨——
// 界面上改了地址或换了密钥，却要重启整个平台才生效，于是没人敢改。
type JenkinsConfigSource func(ctx context.Context) (config.JenkinsConfig, error)

// JenkinsBridge 每次用到时解析一次配置，返回一个按当前配置构造的客户端。
//
// 与 GitLabBridge 的关键差别：**Jenkins 没有本地兜底实现**。
// GitLab 未启用时透传给 local（平台仍能跑通编排闭环），但"没有 Jenkins"
// 就是没有——假装有一个，只会把"未接入"报成"连通正常"，
// 而构建会以最不透明的方式失败。所以这里未启用时返回 ErrJenkinsDisabled。
type JenkinsBridge struct {
	source JenkinsConfigSource

	mu   sync.RWMutex
	last config.JenkinsConfig
}

// NewJenkinsBridge 构造。source 为 nil 时退化为只用传入的静态配置
// （static 即 config.yaml 里的 integration.jenkins），
// 这条退化路径供单元测试与不使用 DB 配置的部署形态使用。
func NewJenkinsBridge(static config.JenkinsConfig, source JenkinsConfigSource) *JenkinsBridge {
	return &JenkinsBridge{source: source, last: static}
}

// resolve 解析当前配置。解析失败时**不退回上一次的值**，
// 宁可让调用方看到"配置读不出来"，也不要静默拿一个过期的地址去请求。
func (b *JenkinsBridge) resolve(ctx context.Context) (config.JenkinsConfig, error) {
	if b.source == nil {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.last, nil
	}
	cfg, err := b.source(ctx)
	if err != nil {
		return config.JenkinsConfig{}, err
	}
	b.mu.Lock()
	b.last = cfg
	b.mu.Unlock()
	return cfg, nil
}

// Refresh 重新解析一次配置，只为把最新结果记进 Name() 用的缓存。
// 不返回错误：调用方是「集成状态」接口，它本身就要容错展示。
func (b *JenkinsBridge) Refresh(ctx context.Context) { _, _ = b.resolve(ctx) }

// Enabled 最近一次解析结果里接入是否启用。
func (b *JenkinsBridge) Enabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.last.Enabled
}

// Name 实现名。未启用时返回 disabled——**不要写成 local**：
// 平台没有任何本地 Jenkins 实现，写 local 会让人以为有一个可用的兜底。
func (b *JenkinsBridge) Name() string {
	if b.Enabled() {
		return "jenkins"
	}
	return "disabled"
}

// Client 按当前配置构造客户端；未启用或未填地址时返回明确错误。
//
// 注意这里**每次构造一个新的客户端**：配置改了（地址、密钥）立刻生效，
// 也顺带重置了 cookie jar 与缓存的 crumb —— 换了账号还用旧会话的 crumb，
// 会得到一个与真实原因无关的 403。
func (b *JenkinsBridge) Client(ctx context.Context) (*JenkinsClient, error) {
	cfg, err := b.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrJenkinsDisabled
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, UpstreamError("Jenkins", errors.New("已启用 Jenkins 接入但未填写服务地址"))
	}
	return NewJenkinsClient(cfg), nil
}

// Ping 连通性探测。未启用时返回 ErrJenkinsDisabled，由上层决定怎么展示。
func (b *JenkinsBridge) Ping(ctx context.Context) error {
	client, err := b.Client(ctx)
	if err != nil {
		return err
	}
	return client.Ping(ctx)
}
