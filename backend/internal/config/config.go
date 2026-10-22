// Package config 负责加载、覆盖与校验 YunNian 平台的运行配置。
//
// 加载顺序（后者覆盖前者）：
//  1. 内置默认值（Default）
//  2. configs/config.yaml（路径可由 -config 指定）
//  3. 环境变量（YUNNIAN_ 前缀）
//
// 配置文件支持 ${VAR} 与环境变量插值，便于把密码等敏感项留在环境里而不入库、不进 Git。
// 注意：未定义的环境变量会**原样保留**（不会静默变成空串），避免"密码被悄悄清空"这类坑。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 让 YAML 里可以直接写 "30s" / "10m" / "2h" / "336h" 这种可读时长。
type Duration time.Duration

// UnmarshalYAML 实现 yaml.Unmarshaler。标准库的 time.Duration 不支持从字符串解析，
// 直接用它会导致 "cannot unmarshal !!str `2h` into time.Duration"。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("时长字段必须是字符串（如 \"2h\"）: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("无法解析时长 %q（示例：30s / 10m / 2h / 336h）: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// D 返回标准库类型，便于直接参与运算。
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config 平台配置总入口。
type Config struct {
	App         AppConfig         `yaml:"app"`
	Log         LogConfig         `yaml:"log"`
	Database    DatabaseConfig    `yaml:"database"`
	Redis       RedisConfig       `yaml:"redis"`
	Auth        AuthConfig        `yaml:"auth"`
	Security    SecurityConfig    `yaml:"security"`
	Runner      RunnerConfig      `yaml:"runner"`
	Integration IntegrationConfig `yaml:"integration"`
	Notify      NotifyConfig      `yaml:"notify"`

	// source 记录实际加载的配置文件路径，仅用于启动日志。
	source string
}

// Source 返回实际加载的配置文件路径（未加载到则为空）。
func (c *Config) Source() string { return c.source }

// AppConfig 进程与 HTTP 服务参数。
type AppConfig struct {
	Name            string   `yaml:"name"`
	Env             string   `yaml:"env"` // dev / test / prod
	HTTPAddr        string   `yaml:"httpAddr"`
	BasePath        string   `yaml:"basePath"`
	StaticDir       string   `yaml:"staticDir"` // 前端单文件产物目录；为空则不托管静态资源
	CORSOrigins     []string `yaml:"corsOrigins"`
	ShutdownTimeout Duration `yaml:"shutdownTimeout"`
}

// LogConfig 日志参数（使用标准库 log/slog，零第三方依赖）。
type LogConfig struct {
	Level  string `yaml:"level"`  // debug / info / warn / error
	Format string `yaml:"format"` // text / json
}

// DatabaseConfig 数据源参数。driver 支持 mysql（生产）与 sqlite（本地零依赖开发）。
type DatabaseConfig struct {
	Driver          string   `yaml:"driver"`
	DSN             string   `yaml:"dsn"`
	SQLitePath      string   `yaml:"sqlitePath"`
	MaxOpenConns    int      `yaml:"maxOpenConns"`
	MaxIdleConns    int      `yaml:"maxIdleConns"`
	ConnMaxLifetime Duration `yaml:"connMaxLifetime"`
	SlowThreshold   Duration `yaml:"slowThreshold"`
	AutoMigrate     bool     `yaml:"autoMigrate"`
	Seed            bool     `yaml:"seed"`
	// SeedDemo 是否在首次启动时写入演示数据（20 个示例项目 / 环境 / 构建记录）。
	// 默认 false：真实部署只需要表结构 + 管理员账号。
	SeedDemo        bool     `yaml:"seedDemo"`
}

// RedisConfig Redis 参数。enabled=false 时全部能力退回进程内实现
// （队列 / 日志流 / 分布式锁），平台仍可完整运行，便于本地与单机部署。
type RedisConfig struct {
	Enabled       bool     `yaml:"enabled"`
	Addr          string   `yaml:"addr"`
	Username      string   `yaml:"username"`
	Password      string   `yaml:"password"`
	DB            int      `yaml:"db"`
	Prefix        string   `yaml:"prefix"`
	LogStreamTTL  Duration `yaml:"logStreamTTL"`  // 日志流保留时长（SSE 断线续传依赖它）
	DeployLockTTL Duration `yaml:"deployLockTTL"` // 部署互斥锁 TTL
	SessionTTL    Duration `yaml:"sessionTTL"`    // 会话键 TTL，与 JWT 双校验
}

// AuthConfig 认证与令牌参数。
type AuthConfig struct {
	JWTSecret       string     `yaml:"jwtSecret"`
	Issuer          string     `yaml:"issuer"`
	AccessTokenTTL  Duration   `yaml:"accessTokenTTL"`
	RefreshTokenTTL Duration   `yaml:"refreshTokenTTL"`
	BcryptCost      int        `yaml:"bcryptCost"`
	LoginFailLimit  int        `yaml:"loginFailLimit"`  // 同一账号窗口内失败上限
	LoginFailWindow Duration   `yaml:"loginFailWindow"` // 失败计数窗口
	OIDC            OIDCConfig `yaml:"oidc"`
}

// OIDCConfig 单点登录参数。
//
// 未配置（enabled=false）时 /auth/oidc/* 会返回明确的「未接入」错误，
// 而不是静默失败——前端据此可以隐藏或置灰 SSO 入口。
type OIDCConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Issuer       string `yaml:"issuer"`
	ClientID     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
	RedirectURL  string `yaml:"redirectUrl"`
	Scopes       string `yaml:"scopes"`
}

// SecurityConfig 密钥与凭证加密参数。
//
// MasterKey 用于加密 secret_credential / ai_model_config 里的密钥字段（AES-256-GCM）。
// 它本身不入库：库里只存密文 + IV + AuthTag。
type SecurityConfig struct {
	MasterKey string `yaml:"masterKey"`
}

// RunnerConfig 任务执行参数。平台与 Runner Agent 共用这份配置。
type RunnerConfig struct {
	// Mode: inline = 平台进程内直接执行（单机/原型阶段）；agent = 由独立 Agent 领取任务。
	Mode string `yaml:"mode"`
	// Executor 决定 inline 模式下**构建这件事怎么发生**：
	//
	//	docker    真跑 docker build / push（本机即构建机）；
	//	simulated 本地模拟：只打日志，不产出镜像，**也不产出 digest**。
	//
	// 为什么单独一项、而不复用 Mode：Mode 说的是"任务在哪个进程里跑"，
	// Executor 说的是"跑的是真家伙还是模拟"。混成一个字段必然出现
	// 「inline + 真跑 docker」这种组合表达不出来的情况。
	//
	// ⚠️ simulated 不是"降级"，它产出的构建记录**是假的**（状态成功、但仓库里没有镜像）。
	// 它存在的意义只有"本地开发时把编排链路跑通"，界面上会标「本地模拟」，
	// 且不写 image_record —— 见 integration.BuildSourceLocal。
	Executor string `yaml:"executor"`
	// BuildWorkRoot 构建工作根目录，每次构建在它下面开 build-<id> 子目录。
	// 留空则用系统临时目录。⚠️ 以容器方式挂 docker.sock 时，这个目录必须以
	// **完全相同的路径**挂进 Agent 容器，否则 daemon 找不到它。
	BuildWorkRoot string `yaml:"buildWorkRoot"`
	// BuildPlatform 目标平台，如 linux/amd64。
	//
	// 必须显式配置、不给默认值兜底：开发机是 arm64、目标服务跑在 x86_64 上，
	// 猜错了会构建出 arm64 镜像，推到生产直接 `exec format error` ——
	// 而这个错要等容器起不来才发现，那时坏镜像已经推出去了。
	BuildPlatform     string   `yaml:"buildPlatform"`
	AgentID           string   `yaml:"agentId"`
	BuildConcurrency  int      `yaml:"buildConcurrency"`
	DeployConcurrency int      `yaml:"deployConcurrency"`
	PollInterval      Duration `yaml:"pollInterval"`
	BuildTimeout      Duration `yaml:"buildTimeout"`
	DeployTimeout     Duration `yaml:"deployTimeout"`
	MaxLogBytes       int64    `yaml:"maxLogBytes"` // 单次任务日志上限
	KeepAlive         Duration `yaml:"keepAlive"`   // Agent 心跳间隔
}

// IntegrationConfig 下游系统接入参数。全部 enabled=false 时走内置的本地实现，
// 保证在没有 Harbor / GitLab / K8s 的环境里也能把编排闭环跑通。
type IntegrationConfig struct {
	Harbor     HarborConfig     `yaml:"harbor"`
	GitLab     GitLabConfig     `yaml:"gitlab"`
	Jenkins    JenkinsConfig    `yaml:"jenkins"`
	Kubernetes KubernetesConfig `yaml:"kubernetes"`
	LLM        LLMConfig        `yaml:"llm"`
	Webhook    WebhookConfig    `yaml:"webhook"`
}

// WebhookConfig 代码托管平台的推送回调密钥。
//
// 与 gitlab.token 分开：token 是平台**调用** GitLab API 用的，
// webhook 密钥是平台**校验** GitLab 回调用的，两者权限与轮换周期都不同，
// 混用会导致"改 API token 就把 webhook 打挂"这种莫名其妙的故障。
type WebhookConfig struct {
	// GitLabSecret 对应回调请求头 X-Gitlab-Token。
	GitLabSecret string `yaml:"gitlabSecret"`
	// GitHubSecret 用于校验 X-Hub-Signature-256（HMAC-SHA256）。
	GitHubSecret string `yaml:"githubSecret"`
	// AllowedBranches 未命中项目自身 autoBuildBranches 时的兜底白名单，留空表示不限。
	AllowedBranches []string `yaml:"allowedBranches"`
}

// HarborConfig 镜像仓库。
type HarborConfig struct {
	Enabled  bool   `yaml:"enabled"`
	BaseURL  string `yaml:"baseUrl"`
	Registry string `yaml:"registry"` // 用于拼装完整镜像地址的对外地址
	// Project 是 Harbor 上的**仓库项目**（如 escloud）。
	//
	// 为什么单独一项、而不是直接用项目的 `image_name`：
	// 平台的 image_name 分前后端（`backend/userv2`、`frontend/aio-doc`），
	// 而 Harbor 上这些镜像**统一放在同一个 project 下**（`escloud/userv2`）。
	// 拿 image_name 的第一段当 project 会变成去查 `backend/userv2` —— 那仓库不存在，
	// 表现是"读取失败"，而真因只是路径拼错了（2026-10-20 用户报的）。
	//
	// 留空按 escloud。
	Project string   `yaml:"project"`
	// CoreAPIURL 是 Harbor 的 **Core 管理接口**地址，与上面的 BaseURL 是两条通道：
	//   BaseURL   说 registry 组件（如 https://host:5000），走 /v2/，拿 manifest/digest；
	//   CoreAPIURL 说 Core，拿 tag 列表（v2 还带推送时间）。
	//
	// **留空则按 BaseURL 探** —— 两者常常就是同一个入口：Harbor v2 在一个端口上同时提供
	// /v2/ 与 /api/v2.0/；实测那台 Harbor **v1.1.2** 的 /v2/ 与 /api/ 也都在 :5000。
	// 所以这个字段是给"registry 走独立端口/镜像站"的部署留的**覆盖项**，不是必填。
	// 接口路径由 Harbor 版本决定（v1 是 /api/，v2 是 /api/v2.0/），平台自动识别，
	// 这里只填**入口地址**，不要带 /api 或 /api/v2.0 后缀。
	CoreAPIURL string   `yaml:"coreApiUrl"`
	Username   string   `yaml:"username"`
	Password   string   `yaml:"password"`
	Insecure   bool     `yaml:"insecure"` // 自签证书时跳过校验
	Timeout    Duration `yaml:"timeout"`
}

// GitLabConfig 代码托管。
type GitLabConfig struct {
	Enabled bool     `yaml:"enabled"`
	BaseURL string   `yaml:"baseUrl"`
	Token   string   `yaml:"token"`
	Timeout Duration `yaml:"timeout"`
}

// JenkinsConfig 构建执行（Jenkins）。
//
// 与 GitLab 的关键差别：**认证是两个值**。Jenkins 的 REST API 走 HTTP Basic，
// 要「用户名 + 密钥」一起提交，只给密钥无法完成认证。
// Token 这一项**既接受 API Token 也接受账号密码**——Basic 认证本身两种都收，
// 实测目标 Jenkins（2.150.2）用账号密码即可 200。平台推荐令牌（可单独吊销、
// 不受改密码与双因素影响），但不阻拦密码。
type JenkinsConfig struct {
	Enabled bool   `yaml:"enabled"`
	BaseURL string `yaml:"baseUrl"`
	// Username Basic 认证的用户名。它不是机密（界面本来就要显示），
	// 所以与密码/令牌分开存：前者进 system_config，后者进加密凭证表。
	Username string   `yaml:"username"`
	Token    string   `yaml:"token"`
	Timeout  Duration `yaml:"timeout"`
	// MaxJobs 单次列出的 Job 上限。目标 Jenkins 实测 531 个 Job，
	// 上限是为了防止"某台 Jenkins 有几万个 Job"把一次列表请求拖成分钟级响应。
	MaxJobs int `yaml:"maxJobs"`
}

// K8sCluster 一套被纳管的集群。
type K8sCluster struct {
	Key        string `yaml:"key"` // 与环境表的 cluster 字段对应
	Name       string `yaml:"name"`
	APIServer  string `yaml:"apiServer"`
	Kubeconfig string `yaml:"kubeconfig"` // 文件路径；留空则用 in-cluster 配置
	Context    string `yaml:"context"`
	DefaultNS  string `yaml:"defaultNamespace"`
}

// KubernetesConfig 平台纳管的集群集合。
type KubernetesConfig struct {
	Enabled  bool         `yaml:"enabled"`
	Clusters []K8sCluster `yaml:"clusters"`
	Timeout  Duration     `yaml:"timeout"`
}

// LLMConfig AI 能力总开关。
// 具体的模型通道（百炼 / 千问 / DeepSeek …）由「系统管理 › 模型设置」落库管理，
// 不走配置文件——因为密钥需要脱敏回显、启停与连通性测试都需要运行时变更。
type LLMConfig struct {
	Enabled         bool     `yaml:"enabled"`
	DefaultProvider string   `yaml:"defaultProvider"`
	Timeout         Duration `yaml:"timeout"`
	MaxConcurrency  int      `yaml:"maxConcurrency"`
}

// NotifyConfig 通知投递链路。
//
// 这一节是「通知渠道」从"只配不发"变成"真能发"之后新增的配置位：
// 渠道本身（收件人 / 机器人地址 / 订阅哪些事件）仍然在
// 「系统管理 › 通知渠道」里按渠道维护，落在 notify_channel 表；
// 这里放的是**平台级**的发信身份 —— 邮件服务器、链接前缀、重试策略。
// 两者分开是因为它们的变更节奏完全不同：渠道是运营随时改的，发信服务器不是。
type NotifyConfig struct {
	// PublicBaseURL 通知正文里「查看详情」链接的前缀（如 http://yunnian.internal:8080）。
	// 留空则不带链接 —— 一个点不开的链接比没有链接更让人困惑。
	PublicBaseURL string `yaml:"publicBaseURL"`
	// Timeout 单次投递超时。
	Timeout Duration `yaml:"timeout"`
	// Retry 失败后的额外重试次数（0 表示只投一次）。仅对"可能自己好"的失败生效。
	Retry int `yaml:"retry"`
	// Concurrency 同时在建的投递连接上限，防止一批事件把对端打满。
	Concurrency int               `yaml:"concurrency"`
	Email       NotifyEmailConfig `yaml:"email"`
}

// NotifyEmailConfig 邮件通道的发送身份。
//
// 字段名与常见的邮件网关环境变量（MAIL_SERVER_URL / MAIL_SERVER_PORT /
// MAIL_SMTP_AUTH / MAIL_TRANSPORT_PROTOCOL …）是对应的，迁移时照抄即可：
//
//	MAIL_SERVER_URL        → host
//	MAIL_SERVER_PORT       → port
//	MAIL_SERVER_USERNAME   → username
//	MAIL_SERVER_PASSWORD   → password
//	MAIL_SENDER_ADDRESS    → from
//	MAIL_TRANSPORT_PROTOCOL→ protocol（smtps = 465 隐式 TLS；starttls = 587）
//	MAIL_SMTP_AUTH         → auth
type NotifyEmailConfig struct {
	// Protocol: smtps（连上即 TLS，通常 465）/ starttls（明文连上再升级，通常 587）。
	// 留空按 smtps 处理 —— 默认加密，而不是默认明文。
	Protocol string `yaml:"protocol"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// From 发件人地址。留空则回退到 Username（多数邮件网关两者相同）。
	From string `yaml:"from"`
	// FromName 收件人看到的名字，会做 MIME 编码，可以写中文。
	FromName string `yaml:"fromName"`
	// Auth 是否使用 LOGIN/PLAIN 鉴权。自建内网中继常关掉它。
	Auth bool `yaml:"auth"`
	// InsecureSkipVerify 跳过证书校验，仅用于自签证书的内网 SMTP。
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
}

// 开发默认值。生产环境必须通过配置文件或环境变量覆盖。
const (
	// DefaultDevJWTSecret 仅用于本地零配置启动；Validate 会在 prod 下拒绝它。
	DefaultDevJWTSecret = "yunnian-dev-only-jwt-secret-please-change"
	// DefaultDevMasterKey 仅用于本地零配置启动（加密凭证用）。
	DefaultDevMasterKey = "yunnian-dev-only-master-key-please-change"
)

// Default 返回可直接运行的默认配置（本地开发形态：SQLite + 无 Redis）。
func Default() *Config {
	return &Config{
		App: AppConfig{
			Name:            "yunnian",
			Env:             "dev",
			HTTPAddr:        ":8080",
			BasePath:        "/api/v1",
			CORSOrigins:     []string{"*"},
			ShutdownTimeout: Duration(10 * time.Second),
		},
		Log: LogConfig{Level: "info", Format: "text"},
		Database: DatabaseConfig{
			// 默认 SQLite：本地无需装 MySQL 即可启动，切生产只需把 driver 改成 mysql 并提供 DSN。
			Driver:          "sqlite",
			SQLitePath:      "./data/yunnian.db",
			MaxOpenConns:    50,
			MaxIdleConns:    10,
			ConnMaxLifetime: Duration(time.Hour),
			SlowThreshold:   Duration(300 * time.Millisecond),
			AutoMigrate:     true,
			Seed:            true,
			SeedDemo:        false,
		},
		Redis: RedisConfig{
			Addr:          "127.0.0.1:6379",
			Prefix:        "yn:",
			LogStreamTTL:  Duration(24 * time.Hour),
			DeployLockTTL: Duration(30 * time.Minute),
			SessionTTL:    Duration(2 * time.Hour),
		},
		Auth: AuthConfig{
			JWTSecret:       DefaultDevJWTSecret,
			Issuer:          "yunnian",
			AccessTokenTTL:  Duration(2 * time.Hour),
			RefreshTokenTTL: Duration(14 * 24 * time.Hour),
			BcryptCost:      10,
			LoginFailLimit:  5,
			LoginFailWindow: Duration(5 * time.Minute),
			OIDC: OIDCConfig{
				Scopes: "openid profile email",
			},
		},
		Security: SecurityConfig{
			MasterKey: DefaultDevMasterKey,
		},
		Runner: RunnerConfig{
			Mode:              "inline",
			Executor:          "docker",
			BuildPlatform:     "linux/amd64",
			BuildConcurrency:  3,
			DeployConcurrency: 1,
			PollInterval:      Duration(2 * time.Second),
			BuildTimeout:      Duration(30 * time.Minute),
			DeployTimeout:     Duration(15 * time.Minute),
			MaxLogBytes:       50 << 20, // 50MB
			KeepAlive:         Duration(10 * time.Second),
		},
		Integration: IntegrationConfig{
			Harbor:     HarborConfig{Timeout: Duration(10 * time.Second)},
			GitLab:     GitLabConfig{Timeout: Duration(10 * time.Second)},
			Jenkins:    JenkinsConfig{Timeout: Duration(15 * time.Second), MaxJobs: 2000},
			Kubernetes: KubernetesConfig{Timeout: Duration(15 * time.Second)},
			LLM:        LLMConfig{Enabled: true, Timeout: Duration(60 * time.Second), MaxConcurrency: 3},
		},
		Notify: NotifyConfig{
			Timeout:     Duration(10 * time.Second),
			Retry:       2,
			Concurrency: 4,
			Email: NotifyEmailConfig{
				Protocol: "smtps",
				Port:     465,
				FromName: "云辇平台",
				Auth:     true,
			},
		},
	}
}

// Load 按「默认值 → 文件 → 环境变量」的顺序装配配置。
// path 为空或文件不存在时不报错，直接用默认值 + 环境变量。
func Load(path string) (*Config, error) {
	cfg := Default()

	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			expanded := expandKeepUnknown(string(raw))
			if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
				return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
			}
			cfg.source = path
		case os.IsNotExist(err):
			// 允许无配置文件启动，交给默认值与环境变量
		default:
			return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
		}
	}

	applyEnvOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// expandKeepUnknown 展开 ${VAR}；未定义的变量原样保留，避免敏感配置被静默清空。
func expandKeepUnknown(s string) string {
	return os.Expand(s, func(key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return "${" + key + "}"
	})
}

// applyEnvOverrides 用环境变量覆盖配置项。这是部署时注入密钥的推荐方式。
func applyEnvOverrides(c *Config) {
	setString(&c.App.Env, "YUNNIAN_ENV")
	setString(&c.App.HTTPAddr, "YUNNIAN_HTTP_ADDR")
	setString(&c.App.BasePath, "YUNNIAN_BASE_PATH")
	setString(&c.App.StaticDir, "YUNNIAN_STATIC_DIR")

	setString(&c.Log.Level, "YUNNIAN_LOG_LEVEL")
	setString(&c.Log.Format, "YUNNIAN_LOG_FORMAT")

	setString(&c.Database.Driver, "YUNNIAN_DB_DRIVER")
	setString(&c.Database.DSN, "YUNNIAN_DB_DSN")
	setString(&c.Database.SQLitePath, "YUNNIAN_DB_SQLITE_PATH")
	setBool(&c.Database.AutoMigrate, "YUNNIAN_DB_AUTO_MIGRATE")
	setBool(&c.Database.Seed, "YUNNIAN_DB_SEED")
	setBool(&c.Database.SeedDemo, "YUNNIAN_DB_SEED_DEMO")

	setBool(&c.Redis.Enabled, "YUNNIAN_REDIS_ENABLED")
	setString(&c.Redis.Addr, "YUNNIAN_REDIS_ADDR")
	setString(&c.Redis.Username, "YUNNIAN_REDIS_USERNAME")
	setString(&c.Redis.Password, "YUNNIAN_REDIS_PASSWORD")
	setInt(&c.Redis.DB, "YUNNIAN_REDIS_DB")

	setString(&c.Auth.JWTSecret, "YUNNIAN_JWT_SECRET")
	setBool(&c.Auth.OIDC.Enabled, "YUNNIAN_OIDC_ENABLED")
	setString(&c.Auth.OIDC.Issuer, "YUNNIAN_OIDC_ISSUER")
	setString(&c.Auth.OIDC.ClientID, "YUNNIAN_OIDC_CLIENT_ID")
	setString(&c.Auth.OIDC.ClientSecret, "YUNNIAN_OIDC_CLIENT_SECRET")
	setString(&c.Auth.OIDC.RedirectURL, "YUNNIAN_OIDC_REDIRECT_URL")

	setString(&c.Security.MasterKey, "YUNNIAN_MASTER_KEY")

	setString(&c.Runner.Mode, "YUNNIAN_RUNNER_MODE")
	setString(&c.Runner.Executor, "YUNNIAN_RUNNER_EXECUTOR")
	setString(&c.Runner.BuildWorkRoot, "YUNNIAN_BUILD_WORK_ROOT")
	setString(&c.Runner.BuildPlatform, "YUNNIAN_BUILD_PLATFORM")
	setString(&c.Runner.AgentID, "YUNNIAN_AGENT_ID")
	setInt(&c.Runner.BuildConcurrency, "YUNNIAN_BUILD_CONCURRENCY")
	setInt(&c.Runner.DeployConcurrency, "YUNNIAN_DEPLOY_CONCURRENCY")

	setBool(&c.Integration.Harbor.Enabled, "YUNNIAN_HARBOR_ENABLED")
	setString(&c.Integration.Harbor.BaseURL, "YUNNIAN_HARBOR_URL")
	setString(&c.Integration.Harbor.Registry, "YUNNIAN_HARBOR_REGISTRY")
	setString(&c.Integration.Harbor.Username, "YUNNIAN_HARBOR_USERNAME")
	setString(&c.Integration.Harbor.Password, "YUNNIAN_HARBOR_PASSWORD")

	setBool(&c.Integration.GitLab.Enabled, "YUNNIAN_GITLAB_ENABLED")
	setString(&c.Integration.GitLab.BaseURL, "YUNNIAN_GITLAB_URL")
	setString(&c.Integration.GitLab.Token, "YUNNIAN_GITLAB_TOKEN")

	setBool(&c.Integration.Jenkins.Enabled, "YUNNIAN_JENKINS_ENABLED")
	setString(&c.Integration.Jenkins.BaseURL, "YUNNIAN_JENKINS_URL")
	setString(&c.Integration.Jenkins.Username, "YUNNIAN_JENKINS_USERNAME")
	setString(&c.Integration.Jenkins.Token, "YUNNIAN_JENKINS_TOKEN")

	setBool(&c.Integration.Kubernetes.Enabled, "YUNNIAN_K8S_ENABLED")

	setString(&c.Integration.Webhook.GitLabSecret, "YUNNIAN_WEBHOOK_GITLAB_SECRET")
	setString(&c.Integration.Webhook.GitHubSecret, "YUNNIAN_WEBHOOK_GITHUB_SECRET")
}

func setString(dst *string, key string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = v
	}
}

func setBool(dst *bool, key string) {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

func setInt(dst *int, key string) {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// Validate 校验配置自洽性，并把明显越界的数值收敛到安全范围。
// 失败时返回的 message 是给运维看的可执行提示（说明缺哪个字段、怎么补）。
func (c *Config) Validate() error {
	// 数据库
	switch c.Database.Driver {
	case "mysql":
		if strings.TrimSpace(c.Database.DSN) == "" {
			return fmt.Errorf("database.driver=mysql 需要 database.dsn；" +
				"可在 configs/config.yaml 里填写，或设置环境变量 YUNNIAN_DB_DSN")
		}
	case "sqlite":
		if strings.TrimSpace(c.Database.SQLitePath) == "" {
			return fmt.Errorf("database.driver=sqlite 需要 database.sqlitePath")
		}
	default:
		return fmt.Errorf("不支持的 database.driver=%q（可选 mysql / sqlite）", c.Database.Driver)
	}

	// 令牌密钥：太短则 HS256 形同虚设
	if len(c.Auth.JWTSecret) < 16 {
		return fmt.Errorf("auth.jwtSecret 至少 16 位（当前 %d 位）；"+
			"请通过环境变量 YUNNIAN_JWT_SECRET 提供，不要写进仓库", len(c.Auth.JWTSecret))
	}

	// 生产环境不允许沿用开发默认密钥：这两个值在仓库里是公开的，
	// 用了就等于没有鉴权 / 凭证明文可解。
	if c.IsProd() {
		if c.Auth.JWTSecret == DefaultDevJWTSecret {
			return fmt.Errorf("生产环境（app.env=prod）不允许使用内置的开发默认 auth.jwtSecret，" +
				"请通过 YUNNIAN_JWT_SECRET 注入随机密钥")
		}
		if c.Security.MasterKey == DefaultDevMasterKey {
			return fmt.Errorf("生产环境（app.env=prod）不允许使用内置的开发默认 security.masterKey，" +
				"请通过 YUNNIAN_MASTER_KEY 注入随机密钥")
		}
	}

	// 单点登录：开了就得给全参数
	if c.Auth.OIDC.Enabled {
		switch {
		case strings.TrimSpace(c.Auth.OIDC.Issuer) == "":
			return fmt.Errorf("auth.oidc.enabled=true 需要 auth.oidc.issuer")
		case strings.TrimSpace(c.Auth.OIDC.ClientID) == "":
			return fmt.Errorf("auth.oidc.enabled=true 需要 auth.oidc.clientId")
		case strings.TrimSpace(c.Auth.OIDC.RedirectURL) == "":
			return fmt.Errorf("auth.oidc.enabled=true 需要 auth.oidc.redirectUrl")
		}
	}

	if strings.TrimSpace(c.Security.MasterKey) == "" {
		c.Security.MasterKey = DefaultDevMasterKey
	}

	// 构建执行方式与目标平台。给默认值是为了"配置文件里没写也能跑"，
	// 但这两个值的默认方向都必须偏**保守**：executor 默认 simulated 会让
	// "构建成功"变成一句空话，默认 docker 则会在没装 docker 的机器上报错 ——
	// 后者是能被发现的问题，前者不能。所以默认 docker。
	if strings.TrimSpace(c.Runner.Executor) == "" {
		c.Runner.Executor = "docker"
	}
	if strings.TrimSpace(c.Runner.BuildPlatform) == "" {
		// 目标服务跑在 x86_64 上，这与"构建机是什么架构"无关。
		c.Runner.BuildPlatform = "linux/amd64"
	}

	// 数值收敛，避免 0 并发导致任务永不执行
	if c.Runner.BuildConcurrency < 1 {
		c.Runner.BuildConcurrency = 1
	}
	if c.Runner.DeployConcurrency < 1 {
		c.Runner.DeployConcurrency = 1
	}
	if c.Runner.MaxLogBytes <= 0 {
		c.Runner.MaxLogBytes = 50 << 20
	}
	if c.Database.MaxOpenConns < 1 {
		c.Database.MaxOpenConns = 10
	}
	if c.Database.MaxIdleConns < 0 {
		c.Database.MaxIdleConns = 0
	}
	if c.Auth.BcryptCost < 10 || c.Auth.BcryptCost > 15 {
		c.Auth.BcryptCost = 10
	}

	// 开了就得给地址，否则启动后才报错更难排查
	if c.Redis.Enabled && strings.TrimSpace(c.Redis.Addr) == "" {
		return fmt.Errorf("redis.enabled=true 需要 redis.addr")
	}
	if c.Integration.Harbor.Enabled && strings.TrimSpace(c.Integration.Harbor.BaseURL) == "" {
		return fmt.Errorf("integration.harbor.enabled=true 需要 integration.harbor.baseUrl")
	}
	if c.Integration.GitLab.Enabled && strings.TrimSpace(c.Integration.GitLab.BaseURL) == "" {
		return fmt.Errorf("integration.gitlab.enabled=true 需要 integration.gitlab.baseUrl")
	}
	if c.Integration.Jenkins.Enabled && strings.TrimSpace(c.Integration.Jenkins.BaseURL) == "" {
		return fmt.Errorf("integration.jenkins.enabled=true 需要 integration.jenkins.baseUrl")
	}
	if c.Integration.Jenkins.MaxJobs < 0 {
		c.Integration.Jenkins.MaxJobs = 0 // 0 表示不限
	}
	if c.Integration.Kubernetes.Enabled && len(c.Integration.Kubernetes.Clusters) == 0 {
		return fmt.Errorf("integration.kubernetes.enabled=true 需要至少一个 integration.kubernetes.clusters 条目")
	}

	// 基础路径规范化
	if !strings.HasPrefix(c.App.BasePath, "/") {
		c.App.BasePath = "/" + c.App.BasePath
	}

	return nil
}

// IsProd 供日志、错误详情与 Cookie 策略判断。
func (c *Config) IsProd() bool { return strings.EqualFold(c.App.Env, "prod") }

// UsingDefaultJWTSecret 判断是否仍在用内置开发密钥，供启动日志给出显式告警。
func (c *Config) UsingDefaultJWTSecret() bool { return c.Auth.JWTSecret == DefaultDevJWTSecret }

// UsingDefaultMasterKey 判断是否仍在用内置开发主密钥。
func (c *Config) UsingDefaultMasterKey() bool { return c.Security.MasterKey == DefaultDevMasterKey }
