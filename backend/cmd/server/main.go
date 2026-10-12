// Command server 是 YunNian 云辇平台的服务端入口。
//
// 启动顺序刻意固定为「配置 → 日志 → 数据库 → 缓存 → 加密 → 下游 → 队列 → 服务 → HTTP」，
// 每一步失败都直接退出并说明原因。理由：一个 CI/CD 平台最怕的是"半启动"——
// 端口起来了但数据库没迁移、或者任务池没建起来，用户点下去只会得到各种莫名错误。
// 宁可在启动阶段就崩掉，也不要带着缺件对外服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/glebarez/sqlite"

	"github.com/yunnian/yunnian/internal/api"
	"github.com/yunnian/yunnian/internal/auth"
	"github.com/yunnian/yunnian/internal/cache"
	"github.com/yunnian/yunnian/internal/config"
	"github.com/yunnian/yunnian/internal/crypto"
	"github.com/yunnian/yunnian/internal/integration"
	"github.com/yunnian/yunnian/internal/logstream"
	"github.com/yunnian/yunnian/internal/queue"
	"github.com/yunnian/yunnian/internal/service"
	"github.com/yunnian/yunnian/internal/store"
)

func main() {
	if err := run(); err != nil {
		// 启动失败用 stderr 直出：此时日志器可能还没建好。
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "configs/config.yaml", "配置文件路径")
		showVersion = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("yunnian-server v1.0.0")
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log)

	log.Info("YunNian 云辇平台启动中",
		"env", cfg.App.Env,
		"configFile", cfg.Source(),
		"httpAddr", cfg.App.HTTPAddr,
		"basePath", cfg.App.BasePath,
		"dbDriver", cfg.Database.Driver,
		"redisEnabled", cfg.Redis.Enabled,
		"runnerMode", cfg.Runner.Mode,
	)
	warnInsecureDefaults(cfg, log)

	startedAt := time.Now()

	// ------------------------------------------------------------------ 数据库
	db, err := openDB(cfg, log)
	if err != nil {
		return err
	}
	if cfg.Database.AutoMigrate {
		if err := store.AutoMigrate(db); err != nil {
			return fmt.Errorf("自动迁移失败: %w", err)
		}
		log.Info("数据库结构已就绪", "tables", len(store.Models()))
	}
	if cfg.Database.Seed {
		if err := store.Seed(db, cfg); err != nil {
			return fmt.Errorf("种子数据初始化失败: %w", err)
		}
	}

	ctx := context.Background()

	// ------------------------------------------------------------------ 缓存 / 锁 / 日志通道
	c, err := cache.New(ctx, cfg.Redis)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	// 日志流：Redis 可用时用共享通道（支持多实例、Runner 与平台分离），
	// 不可用时退回进程内实现，平台仍然完整可用。
	var stream logstream.Stream = c
	if !c.Enabled() {
		log.Warn("Redis 未启用：日志流、分布式锁与失败计数退回进程内实现（仅适用于单实例部署）")
	}

	// ------------------------------------------------------------------ 认证与加密
	jwt := auth.NewManager(cfg.Auth)
	sealer, err := crypto.NewSealer(cfg.Security.MasterKey, cfg.UsingDefaultMasterKey())
	if err != nil {
		return fmt.Errorf("初始化凭证加密器失败: %w", err)
	}
	if sealer.UsingFallbackKey() && cfg.IsProd() {
		return errors.New("生产环境不允许使用内置开发主密钥（security.masterKey）")
	}

	// ------------------------------------------------------------------ 下游系统
	//
	// GitLab 的连接配置可以在「第三方集成」页在线修改，所以这里注入一个
	// "每次用到时去库里读一遍"的来源，而不是把启动时的配置固化下去——
	// 否则改完地址、换完令牌必须重启平台才生效（这类平台的典型运维抱怨）。
	//
	// 集群的 kubeconfig 走同一条路数：它含 client-key，加密落库，也由这里注入。
	//
	// 必须先于本步构造 sealer：落库的令牌与 kubeconfig 都是密文，读出来要解密。
	up := integration.NewSetWithSources(cfg,
		func(ctx context.Context) (config.GitLabConfig, error) {
			cur, err := store.LoadGitLabConfig(ctx, db, sealer, cfg.Integration.GitLab)
			if err != nil {
				return config.GitLabConfig{}, err
			}
			return cur.Cfg, nil
		},
		// Jenkins 同理：地址、用户名与密钥都能在界面上改，改完必须立刻生效。
		// 与 GitLab 的区别是这里没有本地兜底实现——未接入就是未接入。
		func(ctx context.Context) (config.JenkinsConfig, error) {
			cur, err := store.LoadJenkinsConfig(ctx, db, sealer, cfg.Integration.Jenkins)
			if err != nil {
				return config.JenkinsConfig{}, err
			}
			return cur.Cfg, nil
		},
		// 集群凭据与前两个不同：它不是一个"要不要用真实实现"的开关，而是**凭据从哪来**。
		// kubeconfig 里含 client-key，加密存在 secret_credential 表，只有这里
		// （拿着 db 与 sealer）解得开，集成层自己够不着。
		// 环境引用平台集群凭据、或自带一份独立 kubeconfig，都由这个闭包统一裁决。
		func(ctx context.Context, ref integration.ClusterRef) ([]byte, error) {
			return store.LoadClusterKubeconfig(ctx, db, sealer, ref.Cluster, ref.EnvID)
		})
	src := up.Source()
	log.Info("下游系统接入情况",
		"registry", src["registry"], "repository", src["repository"],
		"jenkins", src["jenkins"], "cluster", src["cluster"], "builder", src["builder"])

	// ------------------------------------------------------------------ 任务池
	builds := queue.NewPool("build", cfg.Runner.BuildConcurrency, 0)
	deploys := queue.NewPool("deploy", cfg.Runner.DeployConcurrency, 0)
	// ⚠️ 这段告警的文案必须与装配层的实际行为一致（见 integration.NewSetWithSources）：
	// mode=agent 时用的是 UnavailableBuilder，构建会**明确失败**，不会退回平台进程内执行。
	// 从前这里写的是"任务仍会在平台进程内运行"—— 那是 UnavailableBuilder 引入之前的行为，
	// 文案没跟着改；运维看到会以为"配了 agent 也照跑不误"，直到第一次发版才发现。
	//
	// 判据与装配层对齐（EqualFold + TrimSpace），否则 "Inline" / "inline " 这类写法会出现
	// "这里告警、那里却按 inline 正常跑"的错位。
	if !strings.EqualFold(strings.TrimSpace(cfg.Runner.Mode), "inline") {
		log.Warn("runner.mode 非 inline：Runner Agent 通道未接入，构建会直接失败（不会退回平台进程内执行）",
			"configured", cfg.Runner.Mode)
	}

	// ------------------------------------------------------------------ 服务层
	svc := service.New(service.Deps{
		Cfg:     cfg,
		DB:      db,
		Log:     log,
		JWT:     jwt,
		Stream:  stream,
		Up:      up,
		Cache:   c,
		Sealer:  sealer,
		Builds:  builds,
		Deploys: deploys,
		// Redis 可用时是分布式锁；不可用时 cache 内部退回进程内锁。
		// 两种形态都实现 queue.Locker，所以这里无需分支。
		Locker:    c,
		StartedAt: startedAt,
	})

	// ------------------------------------------------------------------ HTTP
	router := api.NewRouter(api.Deps{
		Cfg:      cfg,
		Svc:      svc,
		JWT:      jwt,
		Cache:    c,
		Sessions: c,
		Log:      log,
	})

	srv := &http.Server{
		Addr:              cfg.App.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：日志类接口是长时间挂着的 SSE 流，
		// 一旦设置写超时，超过阈值的构建日志会被强制掐断。
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("HTTP 服务已监听", "addr", cfg.App.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// ------------------------------------------------------------------ 启动补偿
	//
	// 三步，顺序不能反：
	//  0) 把"重启那一刻还没结束"的构建 / 部署标成 interrupted ——
	//     队列是进程内的，重启后那些记录再也没人会去写终态（详见各 ReconcileInterrupted）。
	//     纯本地读库 + 一次 UPDATE，幂等；必须先于第 1 步跑，因为第 1 步会读构建记录。
	//  1) 把"构建成功、日志已落库、但没登记镜像版本"的历史 Jenkins 构建补登记 ——
	//     Jenkins 的产物只存在于日志里，而解析发生在构建收尾那一刻，进程重启就错过了。
	//     纯本地读库、幂等。
	//  2) 给 digest 还空着的镜像记录补仓库元数据（digest / 大小 / 架构）——
	//     第 1 步刚补出来的记录顺手在这一步一起补上。这一步要连镜像仓库。
	// 都放后台跑，不阻塞启动；各步有自己的超时，外部服务不通也只是多一条日志。
	go func() {
		// 0) 先收拢"重启后永远收不了尾"的任务。
		//
		// 构建 / 部署队列都是**进程内**的：平台一重启，队列与执行协程一起消失，
		// 那些还没结束的记录再也没人会去写终态，界面上就是一个转不完的圈。
		// 纯本地读库 + 一次 UPDATE，必须先于其它补偿跑（后面两步会读构建记录）。
		rctx, rcancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer rcancel()
		bn, berr := svc.CI.ReconcileInterrupted(rctx)
		dn, derr := svc.CD.ReconcileInterrupted(rctx)
		if berr != nil {
			log.Warn("收拢中断的构建失败", "err", berr)
		}
		if derr != nil {
			log.Warn("收拢中断的部署失败", "err", derr)
		}
		if bn+dn > 0 {
			log.Info("已把重启时中断的任务标记为 interrupted", "builds", bn, "deploys", dn)
		}

		bctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		n, err := svc.CI.BackfillJenkinsImages(bctx, 200)
		switch {
		case err != nil:
			log.Warn("补登记历史构建的镜像版本失败", "err", err)
		case n > 0:
			log.Info("已为历史 Jenkins 构建补登记镜像版本", "count", n)
		}

		mctx, mcancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer mcancel()
		m, merr := svc.CI.EnrichMissingImageMeta(mctx, 200)
		switch {
		case merr != nil:
			log.Warn("补镜像元数据（digest / 大小）失败", "err", merr)
		case m > 0:
			log.Info("已为镜像记录补上仓库元数据", "count", m)
		}
	}()

	// ------------------------------------------------------------------ 优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	case sig := <-quit:
		log.Info("收到退出信号，开始优雅关闭", "signal", sig.String())
	}

	shutdownTimeout := cfg.App.ShutdownTimeout.D()
	if shutdownTimeout <= 0 {
		shutdownTimeout = 10 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅关闭失败: %w", err)
	}
	log.Info("平台已退出")
	return nil
}

// logstreamStream 已由 logstream.Stream 直接表达，无需本地别名。

// --------------------------------------------------------------------------- 数据库

// openDB 打开数据库连接池并设置连接参数。
func openDB(cfg *config.Config, log *slog.Logger) (*gorm.DB, error) {
	gormCfg := &gorm.Config{
		// 表名单数化，与 docs/02 的 DDL 以及代码里的原生 JOIN 保持一致。
		// 详见 store.NamingStrategy 的注释——漏掉这一行会在运行期才炸。
		NamingStrategy: store.NamingStrategy(),
		Logger: logger.New(gormSlogWriter{log: log}, logger.Config{
			SlowThreshold:             cfg.Database.SlowThreshold.D(),
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		}),
		// 关闭默认事务：单条写入不需要包事务，包了反而多一次 BEGIN/COMMIT。
		SkipDefaultTransaction: true,
	}

	var (
		db  *gorm.DB
		err error
	)
	switch strings.ToLower(cfg.Database.Driver) {
	case "mysql":
		db, err = gorm.Open(mysql.Open(cfg.Database.DSN), gormCfg)
	case "sqlite":
		if mkErr := os.MkdirAll(dirOf(cfg.Database.SQLitePath), 0o755); mkErr != nil {
			return nil, fmt.Errorf("创建 SQLite 目录失败: %w", mkErr)
		}
		db, err = gorm.Open(sqlite.Open(cfg.Database.SQLitePath), gormCfg)
	default:
		return nil, fmt.Errorf("不支持的数据库驱动 %q", cfg.Database.Driver)
	}
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取数据库连接池失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime.D())

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("数据库连通性检查失败: %w", err)
	}
	log.Info("数据库连接成功", "driver", cfg.Database.Driver, "maxOpenConns", cfg.Database.MaxOpenConns)
	return db, nil
}

// gormSlogWriter 把 GORM 的日志接到 slog 上。
//
// 好处是慢 SQL 与错误会带上 traceId 之外的统一字段（component=gorm），
// 可以直接用现有日志管道按级别过滤，不需要再配一套日志文件。
type gormSlogWriter struct{ log *slog.Logger }

func (w gormSlogWriter) Printf(format string, args ...any) {
	w.log.Warn(strings.TrimSpace(fmt.Sprintf(format, args...)), "component", "gorm")
}

// --------------------------------------------------------------------------- 日志

// newLogger 构造结构日志器。
func newLogger(cfg config.LogConfig) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if strings.EqualFold(cfg.Format, "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// warnInsecureDefaults 对"用着开发默认密钥"给出显式告警。
//
// 必须吵：这两个默认值在代码库里是公开的，一旦跟着配置上到共享环境，
// 等于任何人都能伪造管理员令牌、解开库里所有凭证密文。
func warnInsecureDefaults(cfg *config.Config, log *slog.Logger) {
	if cfg.UsingDefaultJWTSecret() {
		log.Warn("正在使用内置开发 JWT 密钥，请通过 YUNNIAN_JWT_SECRET 注入随机密钥后重启")
	}
	if cfg.UsingDefaultMasterKey() {
		log.Warn("正在使用内置开发加密主密钥，凭证加密强度不达标，请通过 YUNNIAN_MASTER_KEY 注入")
	}
}

// dirOf 取路径的目录部分，用于启动前建目录。
func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i > 0 {
		return path[:i]
	}
	return "."
}
