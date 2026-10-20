package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/crypto"
	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/llm"
	"github.com/2777484478/yunnian/internal/model"
	"github.com/2777484478/yunnian/internal/store"
)

// SystemService 系统管理：统计、集成状态、配置、审计、用户角色、通知、模型通道、凭证。
type SystemService struct {
	*base
}

// --------------------------------------------------------------------------- 概览统计

// Stats 概览页统计。
//
// 全部用聚合查询算，且接受一定程度的"近似"：
// 概览页要的是量级感觉（今天构建了多少次、成功率多高），
// 不值得为绝对精确去加一堆索引或物化表。
func (s *SystemService) Stats(ctx context.Context) (*StatsDTO, error) {
	out := &StatsDTO{}
	db := s.db(ctx)

	since := time.Date(now().Year(), now().Month(), now().Day(), 0, 0, 0, 0, now().Location())

	// 项目数
	if err := db.Model(&model.Project{}).Where("enabled = ?", true).Count(&out.Projects).Error; err != nil {
		return nil, errs.Internalf(err, "统计项目数失败")
	}
	// 今日构建
	if err := db.Model(&model.BuildRecord{}).Where("created_at >= ?", since).Count(&out.TodayBuilds).Error; err != nil {
		return nil, errs.Internalf(err, "统计今日构建失败")
	}
	// 今日成功率：以"已结束的构建"为分母，排队中/进行中的不能算失败
	var today struct {
		Done    int64
		Success int64
	}
	if err := db.Model(&model.BuildRecord{}).
		Select("SUM(CASE WHEN status IN ('success','failed','canceled') THEN 1 ELSE 0 END) AS done, "+
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END) AS success").
		Where("created_at >= ?", since).
		Scan(&today).Error; err != nil {
		return nil, errs.Internalf(err, "统计今日构建成功率失败")
	}
	if today.Done > 0 {
		out.SuccessRate = roundTo(float64(today.Success)/float64(today.Done)*100, 1)
	} else {
		out.SuccessRate = 100
	}
	// 成功/失败计数直接复用上面那条聚合的结果，不再单独查库。
	// 注意分母是「已结束的构建」：todayBuilds 含进行中，两者不等是正常的。
	out.TodaySuccess = today.Success
	out.TodayFailed = today.Done - today.Success
	// 环境
	if err := db.Model(&model.Environment{}).Where("enabled = ?", true).Count(&out.Environments).Error; err != nil {
		return nil, errs.Internalf(err, "统计环境数失败")
	}
	// 进行中的构建 / 部署：优先用任务池的实时值（更准），池不可用时退回数据库计数。
	out.RunningBuilds = int64(s.deps.Builds.Running())
	if out.RunningBuilds == 0 {
		if err := db.Model(&model.BuildRecord{}).Where("status IN ?", []string{"queued", "running"}).
			Count(&out.RunningBuilds).Error; err != nil {
			return nil, errs.Internalf(err, "统计进行中构建失败")
		}
	}
	out.RunningDeploys = int64(s.deps.Deploys.Running())
	if out.RunningDeploys == 0 {
		if err := db.Model(&model.DeployRecord{}).Where("status IN ?", []string{"queued", "progressing"}).
			Count(&out.RunningDeploys).Error; err != nil {
			return nil, errs.Internalf(err, "统计进行中部署失败")
		}
	}
	// 待审批
	if err := db.Model(&model.DeployRecord{}).Where("status = ?", "pending_approval").
		Count(&out.PendingApproval).Error; err != nil {
		return nil, errs.Internalf(err, "统计待审批失败")
	}
	// 镜像 / 部署 / 回滚
	if err := db.Model(&model.ImageRecord{}).Count(&out.TotalImages).Error; err != nil {
		return nil, errs.Internalf(err, "统计镜像数失败")
	}
	if err := db.Model(&model.DeployRecord{}).Count(&out.TotalDeploys).Error; err != nil {
		return nil, errs.Internalf(err, "统计部署数失败")
	}
	if err := db.Model(&model.RollbackRecord{}).Count(&out.TotalRollbacks).Error; err != nil {
		return nil, errs.Internalf(err, "统计回滚数失败")
	}
	// 服务实例
	if err := db.Model(&model.ServiceInstance{}).Count(&out.ActiveServices).Error; err != nil {
		return nil, errs.Internalf(err, "统计服务数失败")
	}
	// ⚠️ 这里**曾经**还有一个 abnormalServices（`status IN ('error','pending')` 的库内计数）。
	// 2026-10-09 退役，理由是它答不了问的那个问题：`service_instance.status` 是"平台期望"
	// （纳管时按默认值写入，之后只有部署成功才更新），与集群实况会漂移 ——
	// 实测 demoai 库里 `pending`、集群里 running 1/1，于是它报 1 而真实是 0。
	// 前端一律按**集群实时状态**数（`utils/live.ts` 的 `isServiceAbnormal`），
	// 这个字段已无任何页面读它。留着只会成为"同一个判断的第二份口径"。
	return out, nil
}

// Integrations 第三方集成状态。
//
// Ready 与 Enabled 分开表达是刻意的：enabled 表示"配置里是否启用真实实现"，
// ready 表示"当前是否真的能用"。两者不一致时（配了地址但连不上）
// 前端要能一眼看出来，否则运维会以为"已经接上了"。
func (s *SystemService) Integrations(ctx context.Context) ([]IntegrationStatusDTO, error) {
	// GitLab 的实现可以在界面上在线切换，所以读实现名之前先让它重新解析一次配置；
	// 否则卡片上写的是启动那一刻的实现名——用户刚打开开关，界面却仍显示"本地模拟"。
	s.deps.Up.Refresh(ctx)

	ic := s.deps.Cfg.Integration
	out := []IntegrationStatusDTO{}

	// GitLab 的连接参数可能已被界面改写，状态卡片必须显示**当前**值，
	// 不能继续用 config.yaml 那份，否则会出现"地址显示 A、结论来自 B"。
	// 读失败就退回配置文件并如实反映（这里不报错：本接口的职责是展示可用性，
	// 不是暴露配置读取故障——那会连 Harbor/K8s 的状态一起看不到）。
	glCfg := ic.GitLab
	if cur, err := store.LoadGitLabConfig(ctx, s.deps.DB, s.deps.Sealer, ic.GitLab); err == nil {
		glCfg = cur.Cfg
	} else {
		s.log("system").Warn("读取 GitLab 配置失败，状态展示回退到配置文件", "err", err)
	}

	// Jenkins 同理：地址与开关都可能已被界面改写。
	jkCfg := ic.Jenkins
	if cur, err := store.LoadJenkinsConfig(ctx, s.deps.DB, s.deps.Sealer, ic.Jenkins); err == nil {
		jkCfg = cur.Cfg
	} else {
		s.log("system").Warn("读取 Jenkins 配置失败，状态展示回退到配置文件", "err", err)
	}

	// Harbor 也从库里读：它原先**没有界面配置**、只读 config.yaml，
	// 于是"界面上改了什么"与"这个卡片显示什么"必然对不上。
	hbCfg := ic.Harbor
	if cur, err := store.LoadHarborConfig(ctx, s.deps.DB, s.deps.Sealer, ic.Harbor); err == nil {
		hbCfg = cur.Cfg
	} else {
		s.log("system").Warn("读取 Harbor 配置失败，状态展示回退到配置文件", "err", err)
	}

	add := func(key, name string, enabled bool, ping func(context.Context) error, detail, disabledDetail string) {
		item := IntegrationStatusDTO{Key: key, Name: name, Enabled: enabled}
		switch key {
		case "harbor":
			item.Impl = s.deps.Up.Registry.Name()
		case "gitlab":
			item.Impl = s.deps.Up.Repository.Name()
		case "kubernetes":
			item.Impl = s.deps.Up.Cluster.Name()
		case "jenkins":
			item.Impl = s.deps.Up.Jenkins.Name()
		}
		if !enabled {
			item.Detail = disabledDetail
			out = append(out, item)
			return
		}
		if err := ping(ctx); err != nil {
			item.Detail = err.Error()
		} else {
			item.Ready = true
			item.Detail = detail
		}
		out = append(out, item)
	}

	// Harbor 的两条通道分别探，**都通才算"已连通"**：
	// 只探 registry 的话，Core 地址指错（比如指向一个真的 registry-only 端口）会显示成绿的。
	// Core 地址留空时按 registry 地址探 —— 两者常常就是同一个入口（见 NewHarborCore）。
	add("harbor", "镜像仓库 Harbor", hbCfg.Enabled, func(ctx context.Context) error {
		base, core := strings.TrimSpace(hbCfg.BaseURL), strings.TrimSpace(hbCfg.CoreAPIURL)
		if base == "" && core == "" {
			return fmt.Errorf("未填写地址：registry 组件（读 digest）与 Core API（读 tag 列表）至少要填一个")
		}
		if base != "" {
			reg := integration.NewHarborRegistry(hbCfg)
			if err := reg.Ping(ctx); err != nil {
				// 报**实际探测的地址**：用户若把 `/v2` 连在字段里一起填了，
				// 客户端会把它削掉，回显配置原文会让人以为那个路径真被用上了。
				return fmt.Errorf("registry 组件（%s）：%w", reg.Base(), err)
			}
		}
		addr := core
		if addr == "" {
			addr = base
		}
		client := integration.NewHarborCore(hbCfg)
		if client == nil {
			return fmt.Errorf("Core API 地址无法解析：%s", addr)
		}
		probe, err := client.Ping(ctx)
		if err != nil {
			return fmt.Errorf("Core API（%s）：%w", client.Base(), err)
		}
		if !probe.OK {
			return fmt.Errorf("Core API（%s）：%s", client.Base(), probe.Message)
		}
		return nil
	},
		harborDetail(hbCfg),
		"未启用真实实现，当前使用内置本地实现")
	add("gitlab", "代码托管 GitLab", glCfg.Enabled, s.deps.Up.Repository.Ping,
		fmt.Sprintf("已连通：%s", glCfg.BaseURL),
		"未启用真实实现，当前使用内置本地实现")
	// Jenkins 是唯一"没有本地兜底"的下游：未接入时平台不调用任何 Jenkins，
	// 而不是切到一个本地假实现——所以这里的措辞必须与别人不同，
	// 否则会让人以为"没接 Jenkins 也能用 Jenkins"。
	add("jenkins", "构建执行 Jenkins", jkCfg.Enabled, s.deps.Up.Jenkins.Ping,
		fmt.Sprintf("已连通：%s", jkCfg.BaseURL),
		"未接入：平台不会调用 Jenkins（CI 仍走平台自身的 docker 构建）")

	// Kubernetes 与上面几个**不是同一回事**，所以不复用 add()。
	//
	// 它的"接入"不在于一个全局开关，而在于「系统管理 › 集群接入」里登记的那批集群：
	// 每个集群是导入 kubeconfig 后**真拨通一次**才算接入的。
	// 原先这里打的是 s.deps.Up.Cluster.Ping，而那是 LocalCluster 的实现——恒返回 nil，
	// 于是只要 config.yaml 里 switches 打开就显示"已连通 N 个集群"，
	// 而 config.yaml 的那几条集群压根没人读过（kubeconfig 字段代码里零引用）。
	// 现在结论来自库里真实的接入记录与探测状态。
	//
	// Impl 写 client-go 而不是 Up.Cluster.Name()：后者说的是**部署执行**那条路径
	// （仍是 local 模拟），拿它标注"集群接入"会让人以为接入也是模拟的。
	// 部署执行仍是本地实现这件事，写在 Detail 里，不靠 impl 字段暗示。
	{
		var total, connected int64
		if err := s.db(ctx).Model(&model.K8sCluster{}).Count(&total).Error; err != nil {
			s.log("system").Warn("统计已接入集群失败", "err", err)
		}
		if total > 0 {
			if err := s.db(ctx).Model(&model.K8sCluster{}).
				Where("status = ?", model.ClusterStatusConnected).Count(&connected).Error; err != nil {
				s.log("system").Warn("统计连通集群失败", "err", err)
			}
		}
		item := IntegrationStatusDTO{
			Key:     "kubernetes",
			Name:    "容器集群 Kubernetes",
			Impl:    "client-go（只读探测）",
			Enabled: total > 0,
		}
		switch {
		case total == 0:
			item.Detail = "尚未接入任何集群：请到「系统管理 › 集群接入」导入 kubeconfig"
		case connected == total:
			item.Ready = true
			item.Detail = fmt.Sprintf("已接入 %d 个集群，全部连通；部署执行仍由平台内置实现完成（尚未接真实下发）", total)
		default:
			item.Detail = fmt.Sprintf("已接入 %d 个集群，%d 个连通、%d 个异常或未探测；部署执行仍由平台内置实现完成",
				total, connected, total-connected)
		}
		out = append(out, item)
	}

	// 数据库与 Redis 不是"集成"，而是平台自身的依赖，但归到同一个接口里
	// 让前端一个请求就能画出完整的依赖健康面板。
	dbItem := IntegrationStatusDTO{Key: "database", Name: "平台数据库", Enabled: true, Impl: s.deps.Cfg.Database.Driver}
	dbSQL, err := s.deps.DB.DB()
	if err != nil {
		dbItem.Detail = err.Error()
	} else if err := dbSQL.PingContext(ctx); err != nil {
		dbItem.Detail = err.Error()
	} else {
		dbItem.Ready = true
		dbItem.Detail = "连接正常"
	}
	out = append(out, dbItem)

	redisItem := IntegrationStatusDTO{Key: "redis", Name: "缓存 Redis", Enabled: s.deps.Cfg.Redis.Enabled, Impl: s.deps.Cache.Name()}
	if !s.deps.Cfg.Redis.Enabled {
		redisItem.Detail = "未启用，队列/锁/日志流使用进程内实现（多副本部署需启用）"
	} else if err := s.deps.Cache.Ping(ctx); err != nil {
		redisItem.Detail = err.Error()
	} else {
		redisItem.Ready = true
		redisItem.Detail = "连接正常：" + s.deps.Cfg.Redis.Addr
	}
	out = append(out, redisItem)

	return out, nil
}

// --------------------------------------------------------------------------- GitLab 接入配置

// GitLabSettingsDTO 「第三方集成」页上 GitLab 连接的当前配置。
//
// 令牌只回掩码，即便调用方是管理员——"谁能看到明文"应该是零个人。
// 需要换令牌就重新填一次；这与模型通道密钥一直以来的口径一致。
type GitLabSettingsDTO struct {
	Enabled  bool   `json:"enabled"`
	BaseURL  string `json:"baseUrl"`
	Timeout  int    `json:"timeout"`
	HasToken bool   `json:"hasToken"`
	// TokenMasked 形如 glp****************a3f9，只用于确认"填的是哪一把"。
	TokenMasked string `json:"tokenMasked"`
	// Source：db = 界面保存过；config = 尚未保存，用的是 configs/config.yaml 的值。
	// 必须显式暴露——否则运维会对着配置文件改半天，其实值早被界面覆盖了。
	Source string `json:"source"`
	// Identity 是上次测试识别到的令牌账号；TokenExpiresAt 是令牌到期日（可能为空）。
	Identity       string `json:"identity"`
	TokenExpiresAt string `json:"tokenExpiresAt"`
	UpdatedAt      string `json:"updatedAt"`
	// Impl 当前实际干活的实现（gitlab / local）。它与 Enabled 是两回事：
	// 开了开关但探活不通时，这里仍是 gitlab，故障由「集成状态」卡片体现。
	Impl string `json:"impl"`
	// TokenPageURL 由地址派生的"新建访问令牌"页面，省得用户自己翻设置菜单。
	TokenPageURL string `json:"tokenPageUrl"`
}

// GitLabSettingsInput 保存 GitLab 配置的入参。
type GitLabSettingsInput struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"baseUrl" binding:"omitempty,max=255"`
	// Token 为空表示保持原令牌不变：前端只看得到掩码、回传不了明文，
	// 若把空串当成"清空令牌"，每次改地址都会顺手把令牌删掉。
	Token string `json:"token" binding:"omitempty,max=1024"`
	// ClearToken 是显式的删除意图。必须与"留空"分开，否则要么删不掉，
	// 要么每次保存都在删——两种都是凭据管理里最难查的故障。
	ClearToken bool `json:"clearToken"`
	Timeout    int  `json:"timeout" binding:"omitempty,min=1,max=300"`
}

// TestGitLabInput 连通性测试入参。
//
// 允许带上还没保存的值，这样"先测再存"才走得通；留空则测已保存的配置
// （也只有这种情况才会把识别到的身份/到期时间写回库）。
type TestGitLabInput struct {
	BaseURL string `json:"baseUrl" binding:"omitempty,max=255"`
	Token   string `json:"token" binding:"omitempty,max=1024"`
	Timeout int    `json:"timeout" binding:"omitempty,min=1,max=300"`
}

// GitLabTestResult GitLab 连通性测试结果。
type GitLabTestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Latency int    `json:"latency"`
	Version string `json:"version"`
	// 令牌代表谁：PAT 的身份完全由令牌自己决定，界面上要能看清用的是谁的账号。
	Username  string   `json:"username"`
	Name      string   `json:"name"`
	IsAdmin   bool     `json:"isAdmin"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expiresAt"`
	// UsedSavedValues 说明本次测的是库里那份，还是表单里还没保存的值。
	UsedSavedValues bool `json:"usedSavedValues"`
	// Warnings 是"现在能连上、但迟早出事"的提醒（作用域不足、令牌即将到期）。
	Warnings []string `json:"warnings"`
}

// GitLabSettings 读取当前 GitLab 连接配置（不含令牌明文）。
func (s *SystemService) GitLabSettings(ctx context.Context) (*GitLabSettingsDTO, error) {
	cur, err := store.LoadGitLabConfig(ctx, s.deps.DB, s.deps.Sealer, s.deps.Cfg.Integration.GitLab)
	if err != nil {
		return nil, errs.Internalf(err, "读取 GitLab 配置失败")
	}
	// 顺手让桥接重新解析，这样 Impl 字段反映的是最新状态而不是启动时的状态。
	s.deps.Up.Refresh(ctx)
	return gitLabSettingsDTO(cur, s.deps.Up.Repository.Name()), nil
}

// UpdateGitLabSettings 保存 GitLab 连接配置。
//
// 令牌为空表示不修改；改过令牌则清掉之前识别到的身份与到期时间
// （它们属于旧令牌，留着会让界面显示一个已经失效的账号）。
func (s *SystemService) UpdateGitLabSettings(ctx context.Context, in GitLabSettingsInput, c Caller) (*GitLabSettingsDTO, error) {
	base, err := normalizeGitLabBaseURL(in.BaseURL)
	if err != nil {
		return nil, err
	}
	if in.Enabled && base == "" {
		return nil, errs.Invalid("启用 GitLab 接入前请先填写服务地址，例如 https://gitlab.example.com")
	}
	if in.ClearToken && strings.TrimSpace(in.Token) != "" {
		return nil, errs.Invalid("不能同时提交新令牌与清除令牌，请二选一")
	}

	replacedToken := strings.TrimSpace(in.Token) != ""
	if err := store.SaveGitLabConfig(ctx, s.deps.DB, s.deps.Sealer, store.GitLabSaveInput{
		Enabled: in.Enabled, BaseURL: base, Timeout: in.Timeout,
		Token: in.Token, ClearToken: in.ClearToken,
		// 传入配置文件里的令牌：首次保存且未填令牌时，它会被搬进加密存储，
		// 让"留空=不修改"在首次保存时也字面成立。
		FallbackToken: s.deps.Cfg.Integration.GitLab.Token,
		UserID:        c.UserID(),
	}); err != nil {
		return nil, errs.Internalf(err, "保存 GitLab 配置失败")
	}

	// 审计里只记"改了哪几项"，绝不记令牌本身——审计日志是给人翻的，不是给人抄的。
	changes := []string{}
	if in.Enabled {
		changes = append(changes, "启用接入")
	} else {
		changes = append(changes, "停用接入")
	}
	if base != "" {
		changes = append(changes, "地址="+base)
	}
	switch {
	case replacedToken:
		changes = append(changes, "更新访问令牌")
	case in.ClearToken:
		changes = append(changes, "清除访问令牌")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "integration:gitlab-update", TargetType: "integration",
		TargetName: "gitlab", Detail: "更新 GitLab 接入配置（" + strings.Join(changes, "，") + "）",
	})

	return s.GitLabSettings(ctx)
}

// TestGitLabConnection 真实发起一次 GitLab API 探测。
//
// 返回 `ok=false` 是**正常响应**（HTTP 200）：表示"请求发出去了但没通过鉴权/网络"，
// 与"接口本身出错"是两回事，前端必须读 ok 与 message，不能只看有没有抛异常。
func (s *SystemService) TestGitLabConnection(ctx context.Context, in TestGitLabInput, c Caller) (*GitLabTestResult, error) {
	cur, err := store.LoadGitLabConfig(ctx, s.deps.DB, s.deps.Sealer, s.deps.Cfg.Integration.GitLab)
	if err != nil {
		return nil, errs.Internalf(err, "读取 GitLab 配置失败")
	}

	cfg := cur.Cfg
	usingSaved := true
	if v := strings.TrimSpace(in.BaseURL); v != "" {
		base, err := normalizeGitLabBaseURL(v)
		if err != nil {
			return nil, err
		}
		cfg.BaseURL, usingSaved = base, false
	}
	if v := strings.TrimSpace(in.Token); v != "" {
		cfg.Token, usingSaved = v, false
	}
	if in.Timeout > 0 {
		cfg.Timeout = config.Duration(time.Duration(in.Timeout) * time.Second)
	}
	if cfg.Timeout.D() <= 0 {
		cfg.Timeout = config.Duration(10 * time.Second)
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errs.Invalid("请先填写 GitLab 服务地址")
	}
	// 测试的语义就是"假设已启用"：若照 Enabled 走桥接，未启用时调用会被
	// 透传给本地实现，于是测出一个什么都没连的"连通正常"。
	cfg.Enabled = true

	repo := integration.NewGitLabRepository(cfg)
	start := time.Now()
	probe, probeErr := repo.Probe(ctx)
	latency := int(time.Since(start).Milliseconds())

	res := &GitLabTestResult{Latency: latency, UsedSavedValues: usingSaved}
	if probeErr != nil {
		res.Message = probeErr.Error()
		s.audit(ctx, c, auditInput{
			Module: "系统", Action: "integration:gitlab-test", TargetType: "integration",
			TargetName: "gitlab", Detail: "测试 GitLab 连通性失败：" + res.Message,
			Result: "failed", ErrMsg: res.Message,
		})
		return res, nil
	}

	res.OK = true
	res.Version, res.Username, res.Name, res.IsAdmin = probe.Version, probe.Username, probe.Name, probe.IsAdmin
	res.Scopes, res.ExpiresAt = probe.Scopes, probe.ExpiresAt
	res.Message = fmt.Sprintf("连通正常：GitLab %s，令牌身份 %s，耗时 %dms", probe.Version, probe.Username, latency)
	res.Warnings = gitLabProbeWarnings(probe)

	if usingSaved && probe.Username != "" {
		var expires *time.Time
		if t, ok := parseGitLabDate(probe.ExpiresAt); ok {
			expires = &t
		}
		if err := store.SaveGitLabProbeResult(ctx, s.deps.DB, probe.Username, expires); err != nil {
			// 记录失败不影响测试结论本身，只留一条告警。
			s.log("system").Warn("记录 GitLab 令牌探测结果失败", "err", err)
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "integration:gitlab-test", TargetType: "integration",
		TargetName: "gitlab", Detail: "测试 GitLab 连通性：" + res.Message,
	})
	return res, nil
}

// gitLabSettingsDTO 组装对外视图。令牌在这里被"降级"成掩码，
// 保证明文不出 service 层。
func gitLabSettingsDTO(cur *store.GitLabSettings, impl string) *GitLabSettingsDTO {
	source := "config"
	if cur.FromDB {
		source = "db"
	}
	dto := &GitLabSettingsDTO{
		Enabled:      cur.Cfg.Enabled,
		BaseURL:      cur.Cfg.BaseURL,
		Timeout:      int(cur.Cfg.Timeout.D().Seconds()),
		HasToken:     strings.TrimSpace(cur.Cfg.Token) != "",
		Source:       source,
		Identity:     cur.Identity,
		UpdatedAt:    formatTime(cur.UpdatedAt),
		Impl:         impl,
		TokenPageURL: gitLabTokenPage(cur.Cfg.BaseURL),
	}
	if dto.Timeout <= 0 {
		dto.Timeout = 10
	}
	if dto.HasToken {
		dto.TokenMasked = crypto.Mask(cur.Cfg.Token)
	} else {
		dto.TokenMasked = "未配置"
	}
	if cur.TokenExpiresAt != nil {
		dto.TokenExpiresAt = cur.TokenExpiresAt.Format("2006-01-02")
	}
	return dto
}

// normalizeGitLabBaseURL 校验并归一化 GitLab 服务地址。
//
// 只接受 http/https：这里要的是 **REST API 的基地址**，不是仓库的 SSH 地址。
// 把 `git@host:group/repo.git` 填进来是极常见的混淆（项目表单里另一个字段正是这个格式），
// 必须当场拦下并说清区别——否则报错会变成让人摸不着头脑的 dial 超时。
func normalizeGitLabBaseURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if strings.HasPrefix(v, "git@") || strings.HasPrefix(v, "ssh://") {
		return "", errs.Invalid("这里要填 GitLab 服务地址（如 https://gitlab.example.com），不是仓库的 SSH 地址")
	}
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		return "", errs.Invalid("地址必须以 http:// 或 https:// 开头，例如 https://gitlab.example.com")
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", errs.Invalidf("无法解析地址 %q", raw)
	}
	// 去掉尾部斜杠：客户端拼接的是 base + "/api/v4/…"，留着会出现双斜杠。
	return strings.TrimRight(v, "/"), nil
}

// gitLabTokenPage 由服务地址推导"新建访问令牌"页面。
//
// GitLab 调整过这个路径（旧版 /-/profile/personal_access_tokens），
// 这里指向新版；老版本会自动重定向过去。
func gitLabTokenPage(base string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		return ""
	}
	return b + "/-/user_settings/personal_access_tokens"
}

// parseGitLabDate 解析令牌到期日。GitLab 两种形态都出现过：
// 元数据接口返回 "2026-10-28"（纯日期），部分版本返回 RFC3339。
func parseGitLabDate(raw string) (time.Time, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	if len(v) >= 10 {
		if t, err := time.Parse("2006-01-02", v[:10]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// gitLabProbeWarnings 把"能连上但迟早出事"的情况翻译成运维看得懂的提醒。
//
// 这两类问题不表现为连接失败，而是表现为**过一阵子突然全挂**：
// 作用域不足要等到真正去读分支时才暴露；令牌到期更是定时炸弹。
func gitLabProbeWarnings(p *integration.GitLabProbe) []string {
	out := []string{}
	has := func(scope string) bool {
		for _, s := range p.Scopes {
			if s == scope {
				return true
			}
		}
		return false
	}

	switch {
	case len(p.Scopes) == 0:
		if p.TokenMetaNote != "" {
			out = append(out, p.TokenMetaNote)
		}
	case !has("api") && !has("read_api"):
		out = append(out, fmt.Sprintf(
			"令牌作用域为 %s，缺少 api / read_api：读取分支与提交会失败，请补足作用域后重新生成令牌",
			strings.Join(p.Scopes, "、")))
	case !has("api"):
		out = append(out, fmt.Sprintf(
			"令牌作用域为 %s，只够读取。若之后要让平台自动配置 webhook 等写操作，需要 api 作用域",
			strings.Join(p.Scopes, "、")))
	}

	if t, ok := parseGitLabDate(p.ExpiresAt); ok {
		days := int(time.Until(t).Hours() / 24)
		switch {
		case days < 0:
			out = append(out, "令牌已过期，请立即重新生成")
		case days <= 30:
			out = append(out, fmt.Sprintf(
				"令牌将于 %s 到期（还剩 %d 天）：到期后构建与部署会全线失败，建议现在轮换或改用长期令牌",
				t.Format("2006-01-02"), days))
		}
	}
	return out
}

// --------------------------------------------------------------------------- Jenkins 接入配置

// JenkinsSettingsDTO 「第三方集成」页上 Jenkins 连接的当前配置。
//
// 密钥只回掩码，即便调用方是管理员——"谁能看到明文"应该是零个人。
// 需要换密钥就重新填一次；这与 GitLab 令牌、模型通道密钥一直以来的口径一致。
type JenkinsSettingsDTO struct {
	Enabled  bool   `json:"enabled"`
	BaseURL  string `json:"baseUrl"`
	Username string `json:"username"`
	Timeout  int    `json:"timeout"`
	HasToken bool   `json:"hasToken"`
	// TokenMasked 形如 ****3456，只用于确认"填的是哪一个"。
	TokenMasked string `json:"tokenMasked"`
	// Source：db = 界面保存过；config = 尚未保存，用的是 configs/config.yaml 的值。
	// 必须显式暴露——否则运维会对着配置文件改半天，其实值早被界面覆盖了。
	Source string `json:"source"`
	// Identity 是上次测试识别到的账号（可能为空：还没测过）。
	Identity  string `json:"identity"`
	UpdatedAt string `json:"updatedAt"`
	// Impl 当前实际干活的实现（jenkins / disabled）。它与 Enabled 是两回事：
	// 开了开关但探活不通时，这里仍是 jenkins，故障由「集成状态」卡片体现。
	Impl string `json:"impl"`
	// CrumbEnabled 上次探测到的是否启用了 CSRF 保护。开启时触发构建需要 crumb，平台自动处理。
	CrumbEnabled bool `json:"crumbEnabled"`
	// TokenPageURL 由地址与用户名派生的"API Token"页面，省得用户自己翻设置菜单。
	TokenPageURL string `json:"tokenPageUrl"`
}

// JenkinsSettingsInput 保存 Jenkins 配置的入参。
type JenkinsSettingsInput struct {
	Enabled  bool   `json:"enabled"`
	BaseURL  string `json:"baseUrl" binding:"omitempty,max=255"`
	Username string `json:"username" binding:"omitempty,max=128"`
	// Token 为空表示保持原密钥不变：前端只看得到掩码、回传不了明文，
	// 若把空串当成"清空"，每次改地址都会顺手把凭据删掉。
	Token string `json:"token" binding:"omitempty,max=1024"`
	// ClearToken 是显式的删除意图。必须与"留空"分开，否则要么删不掉，
	// 要么每次保存都在删——两种都是凭据管理里最难查的故障。
	ClearToken bool `json:"clearToken"`
	Timeout    int  `json:"timeout" binding:"omitempty,min=1,max=300"`
}

// TestJenkinsInput 连通性测试入参。
//
// 允许带上还没保存的值，这样"先测再存"才走得通；留空则测已保存的配置
// （也只有这种情况才会把识别到的账号写回库）。
type TestJenkinsInput struct {
	BaseURL  string `json:"baseUrl" binding:"omitempty,max=255"`
	Username string `json:"username" binding:"omitempty,max=128"`
	Token    string `json:"token" binding:"omitempty,max=1024"`
	Timeout  int    `json:"timeout" binding:"omitempty,min=1,max=300"`
}

// JenkinsTestResult Jenkins 连通性测试结果。
type JenkinsTestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Latency int    `json:"latency"`
	Version string `json:"version"`
	// Username 凭据代表的账号（匿名访问时为空）。
	Username string `json:"username"`
	// Permissions 账号权限点。"能连上但没权限拉 job"要靠它暴露。
	Permissions  []string `json:"permissions"`
	CrumbEnabled bool     `json:"crumbEnabled"`
	// JobCount 顶层 job 数；用了文件夹时只统计顶层，由 Warnings 说明。
	JobCount int `json:"jobCount"`
	// Warnings 是"现在能连上、但迟早出事"的提醒（权限不足、job 数为 0）。
	Warnings []string `json:"warnings"`
	// UsedSavedValues 说明本次测的是库里那份，还是表单里还没保存的值。
	UsedSavedValues bool `json:"usedSavedValues"`
}

// JenkinsSettings 读取当前 Jenkins 连接配置（不含密钥明文）。
func (s *SystemService) JenkinsSettings(ctx context.Context) (*JenkinsSettingsDTO, error) {
	cur, err := store.LoadJenkinsConfig(ctx, s.deps.DB, s.deps.Sealer, s.deps.Cfg.Integration.Jenkins)
	if err != nil {
		return nil, errs.Internalf(err, "读取 Jenkins 配置失败")
	}
	// 顺手让桥接重新解析，这样 Impl 字段反映的是最新状态而不是启动时的状态。
	s.deps.Up.Refresh(ctx)
	return jenkinsSettingsDTO(cur, s.deps.Up.Jenkins.Name()), nil
}

// UpdateJenkinsSettings 保存 Jenkins 连接配置。
//
// 密钥为空表示不修改；改过密钥则清掉之前识别到的账号与 CSRF 状态
// （它们属于旧凭据，留着会让界面显示一个已经失效的账号）。
func (s *SystemService) UpdateJenkinsSettings(ctx context.Context, in JenkinsSettingsInput, c Caller) (*JenkinsSettingsDTO, error) {
	base, err := normalizeJenkinsBaseURL(in.BaseURL)
	if err != nil {
		return nil, err
	}
	if in.Enabled && base == "" {
		return nil, errs.Invalid("启用 Jenkins 接入前请先填写服务地址，例如 http://jenkins.example.com:8080")
	}
	if in.ClearToken && strings.TrimSpace(in.Token) != "" {
		return nil, errs.Invalid("不能同时提交新密钥与清除密钥，请二选一")
	}

	replacedToken := strings.TrimSpace(in.Token) != ""
	if err := store.SaveJenkinsConfig(ctx, s.deps.DB, s.deps.Sealer, store.JenkinsSaveInput{
		Enabled: in.Enabled, BaseURL: base, Username: in.Username, Timeout: in.Timeout,
		Token: in.Token, ClearToken: in.ClearToken,
		// 传入配置文件里的凭据：首次保存且未填密钥时，它会被搬进加密存储，
		// 让"留空=不修改"在首次保存时也字面成立。
		FallbackToken: s.deps.Cfg.Integration.Jenkins.Token,
		UserID:        c.UserID(),
	}); err != nil {
		return nil, errs.Internalf(err, "保存 Jenkins 配置失败")
	}

	// 审计里只记"改了哪几项"，绝不记密钥本身——审计日志是给人翻的，不是给人抄的。
	changes := []string{}
	if in.Enabled {
		changes = append(changes, "启用接入")
	} else {
		changes = append(changes, "停用接入")
	}
	if base != "" {
		changes = append(changes, "地址="+base)
	}
	if u := strings.TrimSpace(in.Username); u != "" {
		changes = append(changes, "用户名="+u)
	}
	switch {
	case replacedToken:
		changes = append(changes, "更新密钥")
	case in.ClearToken:
		changes = append(changes, "清除密钥")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "integration:jenkins-update", TargetType: "integration",
		TargetName: "jenkins", Detail: "更新 Jenkins 接入配置（" + strings.Join(changes, "，") + "）",
	})

	return s.JenkinsSettings(ctx)
}

// TestJenkinsConnection 真实发起一次 Jenkins API 探测。
//
// 返回 `ok=false` 是**正常响应**（HTTP 200）：表示"请求发出去了但没通过鉴权/网络"，
// 与"接口本身出错"是两回事，前端必须读 ok 与 message，不能只看有没有抛异常。
func (s *SystemService) TestJenkinsConnection(ctx context.Context, in TestJenkinsInput, c Caller) (*JenkinsTestResult, error) {
	cur, err := store.LoadJenkinsConfig(ctx, s.deps.DB, s.deps.Sealer, s.deps.Cfg.Integration.Jenkins)
	if err != nil {
		return nil, errs.Internalf(err, "读取 Jenkins 配置失败")
	}

	cfg := cur.Cfg
	usingSaved := true
	if v := strings.TrimSpace(in.BaseURL); v != "" {
		base, err := normalizeJenkinsBaseURL(v)
		if err != nil {
			return nil, err
		}
		cfg.BaseURL, usingSaved = base, false
	}
	if v := strings.TrimSpace(in.Username); v != "" {
		cfg.Username, usingSaved = v, false
	}
	if v := strings.TrimSpace(in.Token); v != "" {
		cfg.Token, usingSaved = v, false
	}
	if in.Timeout > 0 {
		cfg.Timeout = config.Duration(time.Duration(in.Timeout) * time.Second)
	}
	if cfg.Timeout.D() <= 0 {
		cfg.Timeout = config.Duration(15 * time.Second)
	}

	res := &JenkinsTestResult{UsedSavedValues: usingSaved}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		res.Message = "请先填写 Jenkins 服务地址"
		return res, nil
	}
	// "只填了一个"是最常见的手误，本地就能判出来，没必要发一次注定 403 的请求
	// 再让用户去猜原因。
	hasUser := strings.TrimSpace(cfg.Username) != ""
	hasSecret := strings.TrimSpace(cfg.Token) != ""
	if hasUser != hasSecret {
		if hasUser {
			res.Message = "只填了用户名、没填密钥：Jenkins 的 Basic 认证需要「用户名 + 密钥」两个值同时提供"
		} else {
			res.Message = "只填了密钥、没填用户名：Jenkins 的 Basic 认证需要「用户名 + 密钥」两个值同时提供"
		}
		return res, nil
	}
	// 测试的语义就是"假设已启用"：未启用的配置也要能测通，
	// 否则用户必须"先保存再测试"，而保存就意味着把一份没验过的配置写进了库。
	cfg.Enabled = true

	client := integration.NewJenkinsClient(cfg)
	start := time.Now()
	probe, probeErr := client.Probe(ctx)
	latency := int(time.Since(start).Milliseconds())
	res.Latency = latency

	if probeErr != nil {
		res.Message = probeErr.Error()
		s.audit(ctx, c, auditInput{
			Module: "系统", Action: "integration:jenkins-test", TargetType: "integration",
			TargetName: "jenkins", Detail: "测试 Jenkins 连通性失败：" + res.Message,
			Result: "failed", ErrMsg: res.Message,
		})
		return res, nil
	}

	res.OK = true
	res.Version, res.Username = probe.Version, probe.Username
	res.Permissions, res.CrumbEnabled, res.JobCount = probe.Permissions, probe.CrumbEnabled, probe.JobCount
	res.Warnings = jenkinsProbeWarnings(probe, cfg.Username)

	who := probe.Username
	if who == "" {
		who = "匿名"
	}
	res.Message = fmt.Sprintf("连通正常：Jenkins %s，账号 %s，可见 %d 个 Job，耗时 %dms",
		probe.Version, who, probe.JobCount, latency)

	if usingSaved && probe.Username != "" {
		if err := store.SaveJenkinsProbeResult(ctx, s.deps.DB, probe.Username, probe.CrumbEnabled); err != nil {
			// 记录失败不影响测试结论本身，只留一条告警。
			s.log("system").Warn("记录 Jenkins 探测结果失败", "err", err)
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "integration:jenkins-test", TargetType: "integration",
		TargetName: "jenkins", Detail: "测试 Jenkins 连通性：" + res.Message,
	})
	return res, nil
}

// jenkinsSettingsDTO 组装对外视图。密钥在这里被"降级"成掩码，
// 保证明文不出 service 层。
func jenkinsSettingsDTO(cur *store.JenkinsSettings, impl string) *JenkinsSettingsDTO {
	source := "config"
	if cur.FromDB {
		source = "db"
	}
	dto := &JenkinsSettingsDTO{
		Enabled:      cur.Cfg.Enabled,
		BaseURL:      cur.Cfg.BaseURL,
		Username:     cur.Cfg.Username,
		Timeout:      int(cur.Cfg.Timeout.D().Seconds()),
		HasToken:     strings.TrimSpace(cur.Cfg.Token) != "",
		Source:       source,
		Identity:     cur.Identity,
		UpdatedAt:    formatTime(cur.UpdatedAt),
		Impl:         impl,
		CrumbEnabled: cur.CrumbEnabled,
		TokenPageURL: jenkinsTokenPageURL(cur.Cfg.BaseURL, cur.Cfg.Username),
	}
	if dto.Timeout <= 0 {
		dto.Timeout = 15
	}
	if dto.HasToken {
		dto.TokenMasked = crypto.Mask(cur.Cfg.Token)
	} else {
		dto.TokenMasked = "未配置"
	}
	return dto
}

// normalizeJenkinsBaseURL 校验并归一化 Jenkins 服务地址。
//
// 只接受 http/https：这里要的是 **REST API 的基地址**，不是仓库地址。
// 另外顺手处理两个几乎人人都会踩的输入：
//
//   - 以 `/login` 结尾 —— 那是"已登录"后浏览器地址栏里的样子，去掉即可；
//   - 含 `/job/...` —— 那是某个 Job 的深链，不是服务地址，**当场拒绝**并说清原因，
//     否则会得到一个莫名其妙的 404，而用户会以为是自己权限不够。
func normalizeJenkinsBaseURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if strings.HasPrefix(v, "git@") || strings.HasPrefix(v, "ssh://") {
		return "", errs.Invalid("这里要填 Jenkins 服务地址（如 http://jenkins.example.com:8080），不是仓库的 SSH 地址")
	}
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		return "", errs.Invalid("地址必须以 http:// 或 https:// 开头，例如 http://jenkins.example.com:8080")
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", errs.Invalidf("无法解析地址 %q", raw)
	}
	if strings.Contains(u.Path, "/job/") {
		return "", errs.Invalid("这里要填 Jenkins 服务地址（含 context path，如 http://host:8080/jenkins），不要填某个 Job 的链接")
	}
	path := strings.TrimRight(u.Path, "/")
	path = strings.TrimSuffix(path, "/login")
	u.Path = strings.TrimRight(path, "/")
	u.RawQuery, u.Fragment = "", ""
	// 去掉尾部斜杠：客户端拼接的是 base + "/api/json"，留着会出现双斜杠。
	return strings.TrimRight(u.String(), "/"), nil
}

// jenkinsTokenPageURL 由服务地址与用户名推导"API Token"页面。
//
// Jenkins 2.x 的这个页面在 `/user/<用户名>/configure` 上；
// 不知道用户名时退回到根路径，而不是猜一个 admin 出来。
func jenkinsTokenPageURL(base, username string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		return ""
	}
	u := strings.TrimSpace(username)
	if u == "" {
		return b
	}
	return b + "/user/" + url.PathEscape(u) + "/configure"
}

// jenkinsProbeWarnings 把"能连上但迟早出事"的情况翻译成运维看得懂的提醒。
//
// 这类问题不表现为连接失败，而是表现为**过一阵子突然构建全挂**：
// 权限不足要等到真正列 Job 或触发构建时才暴露；匿名访问更是如此。
func jenkinsProbeWarnings(p *integration.JenkinsProbe, configuredUser string) []string {
	out := append([]string{}, p.Warnings...)

	if p.Anonymous || strings.TrimSpace(configuredUser) == "" {
		out = append(out, "没有配置用户名，当前按匿名访问：Jenkins 可能拒绝读取 Job 或触发构建，"+
			"建议在「用户名」里填写一个有 Overall/Read 与 Job/Build 权限的账号")
	} else {
		hasAuth := false
		hasAnonymous := false
		for _, a := range p.Permissions {
			if a == "authenticated" {
				hasAuth = true
			}
			if a == "anonymous" {
				hasAnonymous = true
			}
		}
		if hasAnonymous && !hasAuth {
			out = append(out, "凭据未被接受：Jenkins 把本次访问当成了匿名用户，请检查用户名与密钥（密码是否被改过、是否启用了双因素）")
		} else if len(p.Permissions) == 0 {
			out = append(out, "该 Jenkins 版本未返回账号权限点，无法自动确认权限是否足够")
		}
	}

	if p.JobCount == 0 {
		out = append(out, "能连上但一个 Job 都看不到：通常是账号缺少 Overall/Read 或 Job/Read 权限"+
			"（也可能是该 Jenkins 确实还没有 Job）")
	}
	return out
}

// --------------------------------------------------------------------------- 系统配置

// ListConfig 系统配置列表。
func (s *SystemService) ListConfig(ctx context.Context, group string) ([]SystemConfigDTO, error) {
	db := s.db(ctx).Order(`group_name, config_key`)
	if g := strings.TrimSpace(group); g != "" {
		db = db.Where("group_name = ?", g)
	}
	var rows []model.SystemConfig
	if err := db.Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询系统配置失败")
	}
	out := make([]SystemConfigDTO, 0, len(rows))
	for i := range rows {
		out = append(out, SystemConfigDTO{
			Key: rows[i].ConfigKey, Value: rows[i].ConfigValue, ValueType: rows[i].ValueType,
			Group: rows[i].GroupName, Description: rows[i].Description,
			UpdatedAt: formatTime(rows[i].UpdatedAt),
		})
	}
	return out, nil
}

// UpdateConfigInput 更新系统配置入参。
type UpdateConfigInput struct {
	Items []SystemConfigDTO `json:"items" binding:"required,min=1,dive"`
}

// UpdateConfig 批量更新系统配置（只允许改已存在的键）。
//
// 不接受新增键：系统配置项是被代码读取的固定契约，
// 允许运行时新增只会产生"写进去了但没人读"的幽灵配置。
func (s *SystemService) UpdateConfig(ctx context.Context, in UpdateConfigInput, c Caller) ([]SystemConfigDTO, error) {
	var existing []model.SystemConfig
	if err := s.db(ctx).Find(&existing).Error; err != nil {
		return nil, errs.Internalf(err, "查询系统配置失败")
	}
	byKey := make(map[string]model.SystemConfig, len(existing))
	for i := range existing {
		byKey[existing[i].ConfigKey] = existing[i]
	}

	if err := s.tx(ctx, func(tx *gorm.DB) error {
		for _, item := range in.Items {
			cur, ok := byKey[item.Key]
			if !ok {
				return errs.Invalidf("配置项 %s 不存在，不支持在线新增", item.Key)
			}
			if err := validateConfigValue(item.ValueType, item.Value); err != nil {
				return errs.Invalidf("配置项 %s：%s", item.Key, err.Error())
			}
			// 运行参数（并发上限）还要过范围校验：通用校验只看"是不是数字"，
			// 而 0 会让任务池彻底不再调度、上千会把构建机与集群打满。
			if err := checkRuntimeValue(item.Key, item.Value); err != nil {
				return errs.Invalidf("配置项 %s：%s", item.Key, err.Error())
			}
			if cur.ConfigValue == item.Value {
				continue
			}
			if err := tx.Model(&model.SystemConfig{}).Where("id = ?", cur.ID).Updates(map[string]any{
				"config_value": item.Value, "updated_by": c.UserID(), "updated_at": now(),
			}).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, wrapDB(err, "更新系统配置")
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "config:update", TargetType: "system_config",
		Detail: fmt.Sprintf("更新系统配置 %d 项", len(in.Items)),
	})

	// 改了运行参数就**立刻**应用到任务池：并发上限是"用着用着才发现要调"的旋钮，
	// 需要重启才生效等于它不存在（用户 2026-10-14 的原话：我想自己设置，任意发几个不排队）。
	// 失败只告警不报错：值已经落库、重启后照样生效，没必要让整次保存回滚。
	for _, item := range in.Items {
		if !store.IsRuntimeKey(item.Key) {
			continue
		}
		if err := s.ApplyRuntimeSettings(ctx); err != nil {
			s.log("system").Warn("运行参数已保存，但应用到任务池失败（重启后生效）", "err", err)
		}
		break
	}

	return s.ListConfig(ctx, "")
}

// validateConfigValue 按声明类型校验取值。
// 配置项类型写错（比如把开关写成 "yes"）会在运行期以奇怪的方式失效，这里提前拦住。
func validateConfigValue(valueType, value string) error {
	switch valueType {
	case "number":
		if _, err := parseFloat64(value); err != nil {
			return fmt.Errorf("应为数字，收到 %q", value)
		}
	case "bool":
		v := strings.ToLower(strings.TrimSpace(value))
		if v != "true" && v != "false" {
			return fmt.Errorf("应为 true 或 false，收到 %q", value)
		}
	case "json":
		var tmp any
		if err := json.Unmarshal([]byte(value), &tmp); err != nil {
			return fmt.Errorf("应为合法 JSON：%v", err)
		}
	}
	return nil
}

// --------------------------------------------------------------------------- 审计日志

// AuditQuery 审计检索条件。
type AuditQuery struct {
	Module   string
	User     string
	Action   string
	Result   string
	Keyword  string
	StartAt  string
	EndAt    string
	Page     int
	PageSize int
	SortBy   string
	Order    string
}

// ListAuditLogs 审计检索。
func (s *SystemService) ListAuditLogs(ctx context.Context, q AuditQuery) (*httpx.PageQuery, any, int64, error) {
	pq := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize, SortBy: q.SortBy, Order: q.Order, Keyword: q.Keyword}
	column, desc := pq.Normalize(map[string]string{
		"at": "created_at",
	}, "created_at")

	db := s.db(ctx).Model(&model.AuditLog{})
	if v := strings.TrimSpace(q.Module); v != "" {
		db = db.Where("module = ?", v)
	}
	if v := strings.TrimSpace(q.User); v != "" {
		db = db.Where("username = ? OR user_name = ?", v, v)
	}
	if v := strings.TrimSpace(q.Action); v != "" {
		db = db.Where("action = ?", v)
	}
	if v := strings.TrimSpace(q.Result); v != "" {
		db = db.Where("result = ?", v)
	}
	if ws := strings.TrimSpace(q.StartAt); ws != "" {
		if t, err := parseTimeParam(ws); err == nil {
			db = db.Where("created_at >= ?", t)
		}
	}
	if we := strings.TrimSpace(q.EndAt); we != "" {
		if t, err := parseTimeParam(we); err == nil {
			db = db.Where("created_at <= ?", t)
		}
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("detail LIKE ? OR target_name LIKE ? OR ip LIKE ?", like, like, like)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "统计审计日志失败")
	}
	var rows []model.AuditLog
	if err := db.Order(column + " " + orderWord(desc)).
		Offset(pq.Offset()).Limit(pq.PageSize).Find(&rows).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "查询审计日志失败")
	}
	out := make([]AuditLogDTO, 0, len(rows))
	for i := range rows {
		out = append(out, auditToDTO(&rows[i]))
	}
	return &pq, out, total, nil
}

// GetAuditLog 审计详情。
func (s *SystemService) GetAuditLog(ctx context.Context, id uint64) (*AuditLogDTO, error) {
	var rec model.AuditLog
	if err := s.db(ctx).First(&rec, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("审计记录不存在")
		}
		return nil, errs.Internalf(err, "查询审计记录失败")
	}
	dto := auditToDTO(&rec)
	return &dto, nil
}

// AuditExportInput 审计导出条件。
type AuditExportInput struct {
	Module  string `json:"module"`
	User    string `json:"user"`
	Action  string `json:"action"`
	Result  string `json:"result"`
	StartAt string `json:"startAt"`
	EndAt   string `json:"endAt"`
	// Limit 上限，防止一次导出把内存打满。
	Limit int `json:"limit"`
}

// AuditExportResult 导出结果。
//
// 同步返回内容而不是返回任务号：当前数据量（万级）下同步生成 CSV 只需几十毫秒，
// 引入异步任务+对象存储的复杂度不值得。超过 Limit 时截断并明确告知，
// 而不是静默只导一部分。
type AuditExportResult struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	Rows      int    `json:"rows"`
	Truncated bool   `json:"truncated"`
	Filename  string `json:"filename"`
	Content   string `json:"content"`
}

// ExportAuditLogs 导出审计日志为 CSV。
func (s *SystemService) ExportAuditLogs(ctx context.Context, in AuditExportInput, c Caller) (*AuditExportResult, error) {
	limit := in.Limit
	if limit <= 0 || limit > 50000 {
		limit = 20000
	}
	q := AuditQuery{
		Module: in.Module, User: in.User, Action: in.Action, Result: in.Result,
		StartAt: in.StartAt, EndAt: in.EndAt, Page: 1, PageSize: limit,
	}
	_, data, total, err := s.ListAuditLogs(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, _ := data.([]AuditLogDTO)

	var b strings.Builder
	// 加 BOM：Excel 打开 utf8 CSV 时中文不乱码的唯一稳妥办法。
	b.WriteString("\uFEFF")
	b.WriteString("时间,模块,操作,操作人,姓名,角色,对象类型,对象,结果,IP,详情\n")
	for i := range rows {
		b.WriteString(strings.Join([]string{
			csvField(rows[i].At), csvField(rows[i].Module), csvField(rows[i].Action),
			csvField(rows[i].User), csvField(rows[i].UserName), csvField(rows[i].Role),
			csvField(rows[i].TargetType), csvField(rows[i].Target), csvField(rows[i].Result),
			csvField(rows[i].IP), csvField(rows[i].Detail),
		}, ","))
		b.WriteByte('\n')
	}

	res := &AuditExportResult{
		TaskID:    "export-" + now().Format("20060102150405"),
		Status:    "done",
		Rows:      len(rows),
		Truncated: total > int64(len(rows)),
		Filename:  fmt.Sprintf("yunnian-audit-%s.csv", now().Format("20060102")),
		Content:   b.String(),
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "audit:export", TargetType: "audit_log",
		Detail: fmt.Sprintf("导出审计日志 %d 条（命中 %d 条）", len(rows), total),
	})
	return res, nil
}

// --------------------------------------------------------------------------- 用户与角色

// UserQuery 用户列表查询。
type UserQuery struct {
	Keyword  string
	Role     string
	Enabled  *bool
	Page     int
	PageSize int
}

// ListUsers 用户列表。
func (s *SystemService) ListUsers(ctx context.Context, q UserQuery) (*httpx.PageQuery, any, int64, error) {
	pq := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize, Keyword: q.Keyword}
	pq.Normalize(map[string]string{}, "created_at")

	db := s.db(ctx).Model(&model.User{})
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("username LIKE ? OR name LIKE ? OR email LIKE ?", like, like, like)
	}
	if q.Enabled != nil {
		db = db.Where("enabled = ?", *q.Enabled)
	}
	// 按角色过滤需要联表：user_role → role.code
	if role := strings.TrimSpace(q.Role); role != "" {
		db = db.Where("id IN (?)", s.db(ctx).Table("user_role").
			Select("user_role.user_id").
			Joins("JOIN role ON role.id = user_role.role_id").
			Where("role.code = ?", role))
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "统计用户失败")
	}
	var users []model.User
	if err := db.Order("id").Offset(pq.Offset()).Limit(pq.PageSize).Find(&users).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "查询用户失败")
	}

	roleByUser, err := s.roleCodesOf(ctx, userIDs(users))
	if err != nil {
		return nil, nil, 0, err
	}
	teamByUser, err := s.teamsOf(ctx, userIDs(users))
	if err != nil {
		return nil, nil, 0, err
	}
	out := make([]SysUserDTO, 0, len(users))
	for i := range users {
		out = append(out, userToDTO(&users[i], roleByUser[users[i].ID], teamByUser[users[i].ID]))
	}
	return &pq, out, total, nil
}

// ListUserOptions 「选人」下拉用的精简用户名录（见 UserOptionDTO 的注释）。
//
// 与 ListUsers 的差别只有两点：不分页、不带任何敏感字段。
//
// 刻意**不按 enabled 过滤**：历史数据里可能已经有后来被停用的负责人 / 审批人，
// 一旦过滤掉，编辑表单的预填值就会在候选里消失（AntD 只能把原值当孤儿标签渲染，
// 用户看到的是"值明明在、下拉里却找不到"）。谁被停用了要不要拦，是保存时的校验问题，
// 不是候选清单的问题。
func (s *SystemService) ListUserOptions(ctx context.Context) ([]UserOptionDTO, error) {
	var users []model.User
	if err := s.db(ctx).Model(&model.User{}).Order("id").Find(&users).Error; err != nil {
		return nil, errs.Internalf(err, "查询用户失败")
	}
	out := make([]UserOptionDTO, 0, len(users))
	for i := range users {
		out = append(out, UserOptionDTO{
			ID:       httpx.IDString(users[i].ID),
			Username: users[i].Username,
			Name:     users[i].Name,
		})
	}
	return out, nil
}

// InviteUserInput 邀请（创建）用户入参。
type InviteUserInput struct {
	Username string `json:"username" binding:"required,min=3,max=64,alphanum"`
	Name     string `json:"name" binding:"required,max=64"`
	Email    string `json:"email" binding:"required,email,max=128"`
	Phone    string `json:"phone" binding:"omitempty,max=20"`
	Role     string `json:"role" binding:"required,oneof=admin ops dev qa readonly"`
	Team     string `json:"team" binding:"omitempty,max=64"`
	// Password 留空则使用平台初始密码。
	Password string `json:"password" binding:"omitempty,min=8,max=128"`
}

// InviteUser 创建用户。
func (s *SystemService) InviteUser(ctx context.Context, in InviteUserInput, c Caller) (*SysUserDTO, error) {
	role, err := s.roleByCode(ctx, in.Role)
	if err != nil {
		return nil, err
	}
	plain := in.Password
	if strings.TrimSpace(plain) == "" {
		plain = defaultInitialPassword
	}
	hash, err := s.hashPassword(plain)
	if err != nil {
		return nil, err
	}

	u := model.User{
		Username:     strings.TrimSpace(in.Username),
		Name:         strings.TrimSpace(in.Name),
		Email:        strings.TrimSpace(in.Email),
		Phone:        in.Phone,
		PasswordHash: hash,
		Enabled:      true,
	}
	if err := s.db(ctx).Create(&u).Error; err != nil {
		switch {
		case isDuplicate(err):
			return nil, errs.Conflict("用户名或邮箱已被占用")
		default:
			return nil, errs.Internalf(err, "创建用户失败")
		}
	}
	if err := s.db(ctx).Create(&model.UserRole{UserID: u.ID, RoleID: role.ID}).Error; err != nil {
		return nil, errs.Internalf(err, "分配角色失败")
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "user:invite", TargetType: "user",
		TargetID: &u.ID, TargetName: u.Name,
		Detail: fmt.Sprintf("新增用户 %s（%s，角色 %s）", u.Username, u.Name, role.Code),
	})
	dto := userToDTO(&u, role.Code, in.Team)
	return &dto, nil
}

// UpdateUserInput 修改用户入参。
type UpdateUserInput struct {
	Name    *string `json:"name"`
	Email   *string `json:"email"`
	Phone   *string `json:"phone"`
	Role    *string `json:"role"`
	Enabled *bool   `json:"enabled"`
}

// UpdateUser 改角色 / 启停 / 改资料。
//
// 两个必须拦住的场景：
//  1. 把自己停用或降权 → 会把最后一个管理员锁在门外；
//  2. 把最后一个启用的管理员降权 → 平台再也无人可管。
func (s *SystemService) UpdateUser(ctx context.Context, id uint64, in UpdateUserInput, c Caller) (*SysUserDTO, error) {
	var u model.User
	if err := s.db(ctx).First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("用户不存在")
		}
		return nil, errs.Internalf(err, "查询用户失败")
	}
	if c.User != nil && c.User.ID == u.ID {
		if in.Enabled != nil && !*in.Enabled {
			return nil, errs.Invalid("不能停用当前登录账号")
		}
		if in.Role != nil && *in.Role != "" {
			cur, _ := s.roleCodesOf(ctx, []uint64{u.ID})
			if cur[u.ID] == "admin" && *in.Role != "admin" {
				return nil, errs.Invalid("不能降低当前登录账号的管理员角色")
			}
		}
	}
	if in.Enabled != nil && !*in.Enabled {
		if err := s.ensureNotLastAdmin(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	if in.Role != nil && *in.Role != "" && *in.Role != "admin" {
		cur, err := s.roleCodesOf(ctx, []uint64{u.ID})
		if err != nil {
			return nil, err
		}
		if cur[u.ID] == "admin" {
			if err := s.ensureNotLastAdmin(ctx, u.ID); err != nil {
				return nil, err
			}
		}
	}

	updates := map[string]any{}
	if in.Name != nil {
		updates["name"] = strings.TrimSpace(*in.Name)
	}
	if in.Email != nil {
		updates["email"] = strings.TrimSpace(*in.Email)
	}
	if in.Phone != nil {
		updates["phone"] = *in.Phone
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if len(updates) > 0 {
		if err := s.db(ctx).Model(&model.User{}).Where("id = ?", u.ID).Updates(updates).Error; err != nil {
			return nil, errs.Internalf(err, "更新用户失败")
		}
	}

	if in.Role != nil && *in.Role != "" {
		role, err := s.roleByCode(ctx, *in.Role)
		if err != nil {
			return nil, err
		}
		if err := s.tx(ctx, func(tx *gorm.DB) error {
			if err := tx.Where("user_id = ?", u.ID).Delete(&model.UserRole{}).Error; err != nil {
				return err
			}
			return tx.Create(&model.UserRole{UserID: u.ID, RoleID: role.ID}).Error
		}); err != nil {
			return nil, errs.Internalf(err, "更新用户角色失败")
		}
	}

	// 权限或启用状态变了，必须把该用户的所有会话踢掉：
	// 否则旧令牌里的权限快照还有效，降权要等令牌过期才生效（最长 2 小时）。
	if (in.Role != nil && *in.Role != "") || (in.Enabled != nil && !*in.Enabled) {
		if _, err := s.deps.Cache.DropUserSessions(ctx, u.ID); err != nil {
			s.log("system").Warn("清除用户会话失败", "userId", u.ID, "err", err)
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "user:update", TargetType: "user",
		TargetID: &u.ID, TargetName: u.Name,
		Detail: describeUserUpdate(in),
	})

	var fresh model.User
	if err := s.db(ctx).First(&fresh, u.ID).Error; err != nil {
		return nil, errs.Internalf(err, "查询用户失败")
	}
	roles, err := s.roleCodesOf(ctx, []uint64{fresh.ID})
	if err != nil {
		return nil, err
	}
	teams, err := s.teamsOf(ctx, []uint64{fresh.ID})
	if err != nil {
		return nil, err
	}
	dto := userToDTO(&fresh, roles[fresh.ID], teams[fresh.ID])
	return &dto, nil
}

// ResetPassword 重置密码。返回新密码（仅在本次响应里出现一次）。
func (s *SystemService) ResetPassword(ctx context.Context, id uint64, newPassword string, c Caller) (string, error) {
	var u model.User
	if err := s.db(ctx).First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", errs.NotFoundf("用户不存在")
		}
		return "", errs.Internalf(err, "查询用户失败")
	}
	plain := strings.TrimSpace(newPassword)
	if plain == "" {
		plain = defaultInitialPassword
	}
	if len(plain) < 8 {
		return "", errs.Invalid("密码至少 8 位")
	}
	hash, err := s.hashPassword(plain)
	if err != nil {
		return "", err
	}
	if err := s.db(ctx).Model(&model.User{}).Where("id = ?", u.ID).
		Update("password_hash", hash).Error; err != nil {
		return "", errs.Internalf(err, "重置密码失败")
	}
	// 重置密码必须踢掉所有会话，否则"密码改了但别人还登着"。
	if _, err := s.deps.Cache.DropUserSessions(ctx, u.ID); err != nil {
		s.log("system").Warn("清除用户会话失败", "userId", u.ID, "err", err)
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "user:reset-password", TargetType: "user",
		TargetID: &u.ID, TargetName: u.Name, Detail: "重置用户密码",
	})
	return plain, nil
}

// ListRoles 角色与权限点。
func (s *SystemService) ListRoles(ctx context.Context) ([]SysRoleDTO, error) {
	var roles []model.Role
	if err := s.db(ctx).Order("sort, id").Find(&roles).Error; err != nil {
		return nil, errs.Internalf(err, "查询角色失败")
	}
	permByRole, err := s.permsOfRoles(ctx, roleIDs(roles))
	if err != nil {
		return nil, err
	}
	countByRole, err := s.userCountOfRoles(ctx, roleIDs(roles))
	if err != nil {
		return nil, err
	}
	out := make([]SysRoleDTO, 0, len(roles))
	for i := range roles {
		out = append(out, SysRoleDTO{
			Key:   roles[i].Code,
			Name:  roles[i].Name,
			Desc:  roles[i].Description,
			Users: countByRole[roles[i].ID],
			Color: roleColor(roles[i].Code),
			Perms: permByRole[roles[i].ID],
		})
	}
	return out, nil
}

// UpdateRolePermissionsInput 调整角色权限入参。
type UpdateRolePermissionsInput struct {
	Perms []string `json:"perms" binding:"required"`
}

// UpdateRolePermissions 调整角色权限。
//
// 拦住两类危险操作：
//  1. 改 admin 角色的权限（会把自己锁死）；
//  2. 把某角色权限改成空数组时，如果该角色下还有用户 → 这些人会瞬间"什么都看不了"，
//     要求显式确认（用 allowEmpty=true 表达）。
func (s *SystemService) UpdateRolePermissions(ctx context.Context, code string, in UpdateRolePermissionsInput, allowEmpty bool, c Caller) (*SysRoleDTO, error) {
	role, err := s.roleByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if role.Code == "admin" {
		return nil, errs.Forbidden("超级管理员的权限不可调整（避免把自己锁在平台外）")
	}
	if len(in.Perms) == 0 && !allowEmpty {
		return nil, errs.Invalid("将角色权限清空会影响该角色下所有用户，请显式确认（allowEmpty=true）")
	}

	// ⚠️ 动态权限点：`env:view:<envKey>` 是按环境生成的，不预先在 permSeed 里登记。
	// 保存前先把缺的补进 permission 表 —— 否则会被下面"权限点必须存在"的校验直接拒掉，
	// 表现是"在角色页选了环境 → 保存报错"，而用户完全看不出为什么。
	// 名字带环境名，好在权限清单里读得懂（如「查看：阿里云预生产环境」）。
	for _, code := range dedupeStrings(in.Perms) {
		rest, ok := strings.CutPrefix(code, "env:view:")
		if !ok || rest == "" {
			continue
		}
		var cnt int64
		if err := s.db(ctx).Model(&model.Permission{}).Where("code = ?", code).Count(&cnt).Error; err != nil {
			return nil, errs.Internalf(err, "检查权限点失败")
		}
		if cnt > 0 {
			continue
		}
		name := rest
		var env model.Environment
		if err := s.db(ctx).Where("env_key = ?", rest).First(&env).Error; err == nil && env.Name != "" {
			name = env.Name
		}
		if err := s.db(ctx).Create(&model.Permission{
			Code: code, Name: "查看：" + name, Module: "environment",
		}).Error; err != nil {
			// 并发下另一个请求刚插进去，跳过即可
			continue
		}
	}

	// 权限点必须存在于权限表：写错一个 code 就会静默失效（鉴权时匹配不上），
	// 而用户会以为"配好了"。这里逐一校验。
	var perms []model.Permission
	if len(in.Perms) > 0 {
		if err := s.db(ctx).Where("code IN ?", dedupeStrings(in.Perms)).Find(&perms).Error; err != nil {
			return nil, errs.Internalf(err, "查询权限点失败")
		}
	}
	found := map[string]bool{}
	for i := range perms {
		found[perms[i].Code] = true
	}
	missing := []string{}
	for _, want := range dedupeStrings(in.Perms) {
		if !found[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return nil, errs.Invalidf("以下权限点不存在：%s", strings.Join(missing, "、"))
	}

	if err := s.tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", role.ID).Delete(&model.RolePermission{}).Error; err != nil {
			return err
		}
		if len(perms) == 0 {
			return nil
		}
		links := make([]model.RolePermission, 0, len(perms))
		for i := range perms {
			links = append(links, model.RolePermission{RoleID: role.ID, PermissionID: perms[i].ID})
		}
		return tx.Create(&links).Error
	}); err != nil {
		return nil, wrapDB(err, "更新角色权限")
	}

	// 该角色下所有用户的会话都要失效，让新权限立即生效。
	var uids []uint64
	if err := s.db(ctx).Model(&model.UserRole{}).Where("role_id = ?", role.ID).
		Pluck("user_id", &uids).Error; err != nil {
		s.log("system").Warn("查询角色用户失败", "err", err)
	}
	for _, uid := range dedupeUint64(uids) {
		if _, err := s.deps.Cache.DropUserSessions(ctx, uid); err != nil {
			s.log("system").Warn("清除用户会话失败", "userId", uid, "err", err)
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "role:update-perms", TargetType: "role",
		TargetID: &role.ID, TargetName: role.Name,
		Detail: fmt.Sprintf("调整角色 %s 权限为 %d 项，影响 %d 名用户", role.Code, len(perms), len(dedupeUint64(uids))),
	})
	roles, err := s.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	for i := range roles {
		if roles[i].Key == role.Code {
			return &roles[i], nil
		}
	}
	return nil, errs.NotFoundf("角色不存在")
}

// ListPermissions 全部权限点（供角色编辑弹窗选择）。
func (s *SystemService) ListPermissions(ctx context.Context) ([]PermissionDTO, error) {
	var rows []model.Permission
	if err := s.db(ctx).Order("module, code").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询权限点失败")
	}
	out := make([]PermissionDTO, 0, len(rows))
	for i := range rows {
		out = append(out, PermissionDTO{
			Code: rows[i].Code, Name: rows[i].Name, Module: rows[i].Module,
			Description: rows[i].Description,
		})
	}
	return out, nil
}

// PermissionDTO 权限点。
type PermissionDTO struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Module      string `json:"module"`
	Description string `json:"description"`
}

// --------------------------------------------------------------------------- 通知渠道

// NotifyInput 通知渠道入参。
//
// Type 的枚举**只剩两个**是有意的：平台一期只实现了邮件与钉钉的发送器。
// 以前这里列了 5 个类型、events 也放行任意字符串，于是"能配置 == 能收到"
// 这条用户默认成立的等式其实不成立 —— 配一个企微渠道、勾上「回滚执行」，
// 两样都会安安静静躺在库里，然后永远不会发生。
// 现在入口只留真能走通的组合：配不出来的东西，不该让人配出来。
type NotifyInput struct {
	Name    string   `json:"name" binding:"required,max=128"`
	Type    string   `json:"type" binding:"required,oneof=email dingtalk"`
	Target  string   `json:"target" binding:"required,max=512"`
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled"`
}

// ListNotifyChannels 通知渠道列表。
func (s *SystemService) ListNotifyChannels(ctx context.Context) ([]NotifyChannelDTO, error) {
	var rows []model.NotifyChannel
	if err := s.db(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询通知渠道失败")
	}
	out := make([]NotifyChannelDTO, 0, len(rows))
	for i := range rows {
		out = append(out, notifyToDTO(&rows[i]))
	}
	return out, nil
}

// CreateNotifyChannel 新建通知渠道。
func (s *SystemService) CreateNotifyChannel(ctx context.Context, in NotifyInput, c Caller) (*NotifyChannelDTO, error) {
	if err := validateNotifyTarget(in.Type, in.Target); err != nil {
		return nil, err
	}
	events := defaultEvents(in.Events)
	if err := validateNotifyEvents(events); err != nil {
		return nil, err
	}
	n := model.NotifyChannel{
		Name:      strings.TrimSpace(in.Name),
		Type:      in.Type,
		Target:    strings.TrimSpace(in.Target),
		Events:    mustJSON(events),
		Enabled:   in.Enabled == nil || *in.Enabled,
		CreatedBy: c.UserID(),
	}
	if err := s.db(ctx).Create(&n).Error; err != nil {
		return nil, errs.Internalf(err, "创建通知渠道失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "notify:create", TargetType: "notify_channel",
		TargetID: &n.ID, TargetName: n.Name,
		Detail: fmt.Sprintf("新建通知渠道 %s（%s）", n.Name, n.Type),
	})
	dto := notifyToDTO(&n)
	return &dto, nil
}

// UpdateNotifyChannel 编辑通知渠道。
func (s *SystemService) UpdateNotifyChannel(ctx context.Context, id uint64, in NotifyInput, c Caller) (*NotifyChannelDTO, error) {
	var n model.NotifyChannel
	if err := s.db(ctx).First(&n, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("通知渠道不存在")
		}
		return nil, errs.Internalf(err, "查询通知渠道失败")
	}
	if err := validateNotifyTarget(in.Type, in.Target); err != nil {
		return nil, err
	}
	events := defaultEvents(in.Events)
	if err := validateNotifyEvents(events); err != nil {
		return nil, err
	}
	updates := map[string]any{
		"name":   strings.TrimSpace(in.Name),
		"type":   in.Type,
		"target": strings.TrimSpace(in.Target),
		"events": mustJSON(events),
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if err := s.db(ctx).Model(&model.NotifyChannel{}).Where("id = ?", n.ID).Updates(updates).Error; err != nil {
		return nil, errs.Internalf(err, "更新通知渠道失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "notify:update", TargetType: "notify_channel",
		TargetID: &n.ID, TargetName: n.Name, Detail: "编辑通知渠道 " + n.Name,
	})
	var fresh model.NotifyChannel
	if err := s.db(ctx).First(&fresh, n.ID).Error; err != nil {
		return nil, errs.Internalf(err, "查询通知渠道失败")
	}
	dto := notifyToDTO(&fresh)
	return &dto, nil
}

// DeleteNotifyChannel 删除通知渠道。
func (s *SystemService) DeleteNotifyChannel(ctx context.Context, id uint64, c Caller) error {
	var n model.NotifyChannel
	if err := s.db(ctx).First(&n, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errs.NotFoundf("通知渠道不存在")
		}
		return errs.Internalf(err, "查询通知渠道失败")
	}
	if err := s.db(ctx).Delete(&model.NotifyChannel{}, n.ID).Error; err != nil {
		return errs.Internalf(err, "删除通知渠道失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "notify:delete", TargetType: "notify_channel",
		TargetID: &n.ID, TargetName: n.Name, Detail: "删除通知渠道 " + n.Name,
	})
	return nil
}

// SetNotifyEnabled 启停通知渠道。
func (s *SystemService) SetNotifyEnabled(ctx context.Context, id uint64, enabled bool, c Caller) error {
	var n model.NotifyChannel
	if err := s.db(ctx).First(&n, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errs.NotFoundf("通知渠道不存在")
		}
		return errs.Internalf(err, "查询通知渠道失败")
	}
	if err := s.db(ctx).Model(&model.NotifyChannel{}).Where("id = ?", n.ID).
		Update("enabled", enabled).Error; err != nil {
		return errs.Internalf(err, "更新通知渠道失败")
	}
	word := "停用"
	if enabled {
		word = "启用"
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "notify:enabled", TargetType: "notify_channel",
		TargetID: &n.ID, TargetName: n.Name, Detail: fmt.Sprintf("%s通知渠道 %s", word, n.Name),
	})
	return nil
}

// TestNotifyChannel 发送测试消息。
//
// 与旧版"只做目标格式预检"的区别在于：现在**真的投递**，并把对端的原话带回来。
// 用户点「测试」想知道的只有一件事 —— 到底通不通。一句"配置格式校验通过"
// 对他们没有任何价值，反而会被读成"配好了"。
//
// ok=false 是**正常结果**而不是接口错误（对端拒收、签名不对、网络不通都会走到这里），
// 所以不返回 error，而是把真因写在 message 里原样展示。
func (s *SystemService) TestNotifyChannel(ctx context.Context, id uint64, c Caller) (*NotifyTestResult, error) {
	var n model.NotifyChannel
	if err := s.db(ctx).First(&n, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("通知渠道不存在")
		}
		return nil, errs.Internalf(err, "查询通知渠道失败")
	}
	if err := validateNotifyTarget(n.Type, n.Target); err != nil {
		return &NotifyTestResult{OK: false, Message: err.Error()}, nil
	}

	sendErr := s.notify.SendTest(ctx, &n)
	res := &NotifyTestResult{OK: sendErr == nil}
	if sendErr != nil {
		res.Message = "投递失败：" + sendErr.Error()
	} else {
		res.Message = fmt.Sprintf("已通过%s发出测试消息，请到%s确认收到",
			notifyTypeWord(n.Type), notifyTestTargetHint(n))
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "notify:test", TargetType: "notify_channel",
		TargetID: &n.ID, TargetName: n.Name,
		Detail: "测试通知渠道：" + res.Message, Result: resultWord(res.OK),
	})
	return res, nil
}

// notifyTypeWord 渠道类型的中文说法。
func notifyTypeWord(typ string) string {
	if typ == "email" {
		return "邮件"
	}
	return "钉钉"
}

// notifyTestTargetHint 告诉用户"去哪查收"。
//
// 测试消息发出去之后，如果没有这句，用户面对一个绿色的"测试通过"仍然不知道该看哪 ——
// 尤其邮件渠道：目标可能是一串逗号分隔的多个地址。
func notifyTestTargetHint(ch model.NotifyChannel) string {
	if ch.Type == "email" {
		return "收件箱「" + ch.Target + "」"
	}
	return "该机器人所在的群"
}

// NotifyTestResult 通知测试结果。
type NotifyTestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// --------------------------------------------------------------------------- 模型通道

// ModelInput 模型通道入参。
type ModelInput struct {
	Name           string   `json:"name" binding:"required,max=64"`
	Vendor         string   `json:"vendor" binding:"required,oneof=dashscope qwen deepseek openai custom"`
	BaseURL        string   `json:"baseUrl" binding:"required,url,max=255"`
	APIKey         string   `json:"apiKey" binding:"omitempty,max=512"`
	Models         []string `json:"models"`
	DefaultModel   string   `json:"defaultModel" binding:"omitempty,max=96"`
	Enabled        *bool    `json:"enabled"`
	IsDefault      *bool    `json:"isDefault"`
	Timeout        int      `json:"timeout" binding:"omitempty,min=5,max=600"`
	MaxConcurrency int      `json:"maxConcurrency" binding:"omitempty,min=1,max=50"`
	Temperature    float64  `json:"temperature" binding:"omitempty,min=0,max=2"`
	MaxTokens      int      `json:"maxTokens" binding:"omitempty,min=1,max=100000"`
	Desc           string   `json:"desc" binding:"omitempty,max=512"`
}

// ModelQuery 模型通道查询。
type ModelQuery struct {
	Keyword string
	Vendor  string
	Enabled *bool
}

// ListModelProviders 模型通道列表。
//
// 一律不返回明文密钥：库里是密文，回显用 api_key_masked。
// 即使调用方是管理员也不回明文——"谁能看到密钥"应该是零个人，
// 需要换密钥就重新填一次。
func (s *SystemService) ListModelProviders(ctx context.Context, q ModelQuery) ([]ModelProviderDTO, error) {
	db := s.db(ctx).Order("is_default desc, id")
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("name LIKE ? OR base_url LIKE ? OR default_model LIKE ?", like, like, like)
	}
	if v := strings.TrimSpace(q.Vendor); v != "" {
		db = db.Where("vendor = ?", v)
	}
	if q.Enabled != nil {
		db = db.Where("enabled = ?", *q.Enabled)
	}
	var rows []model.AIModelConfig
	if err := db.Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询模型通道失败")
	}
	out := make([]ModelProviderDTO, 0, len(rows))
	for i := range rows {
		out = append(out, modelToDTO(&rows[i]))
	}
	return out, nil
}

// CreateModelProvider 新建模型通道。
func (s *SystemService) CreateModelProvider(ctx context.Context, in ModelInput, c Caller) (*ModelProviderDTO, error) {
	if err := validateModelBaseURL(in.BaseURL); err != nil {
		return nil, err
	}
	m := model.AIModelConfig{
		Name:           strings.TrimSpace(in.Name),
		Vendor:         in.Vendor,
		BaseURL:        normalizeBaseURL(in.BaseURL),
		Models:         mustJSON(defaultModels(in.Models, in.DefaultModel)),
		DefaultModel:   in.DefaultModel,
		Enabled:        in.Enabled == nil || *in.Enabled,
		IsDefault:      in.IsDefault != nil && *in.IsDefault,
		TimeoutSec:     defaultInt(in.Timeout, 60),
		MaxConcurrency: defaultInt(in.MaxConcurrency, 3),
		Temperature:    defaultFloat(in.Temperature, 0.7),
		MaxTokens:      defaultInt(in.MaxTokens, 2048),
		Status:         "unchecked",
		Description:    in.Desc,
	}
	if key := strings.TrimSpace(in.APIKey); key != "" {
		if err := s.sealAPIKey(&m, key); err != nil {
			return nil, err
		}
	}
	if err := s.db(ctx).Create(&m).Error; err != nil {
		return nil, errs.Internalf(err, "创建模型通道失败")
	}
	if m.IsDefault {
		if err := s.setSoleDefault(ctx, m.ID); err != nil {
			return nil, err
		}
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "model:create", TargetType: "ai_model_config",
		TargetID: &m.ID, TargetName: m.Name,
		Detail: fmt.Sprintf("新建模型通道 %s（%s，模型 %s）", m.Name, m.Vendor, m.DefaultModel),
	})
	dto := modelToDTO(&m)
	return &dto, nil
}

// UpdateModelProvider 编辑模型通道。
func (s *SystemService) UpdateModelProvider(ctx context.Context, id uint64, in ModelInput, c Caller) (*ModelProviderDTO, error) {
	var m model.AIModelConfig
	if err := s.db(ctx).First(&m, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("模型通道不存在")
		}
		return nil, errs.Internalf(err, "查询模型通道失败")
	}
	if err := validateModelBaseURL(in.BaseURL); err != nil {
		return nil, err
	}

	updates := map[string]any{
		"name":            strings.TrimSpace(in.Name),
		"vendor":          in.Vendor,
		"base_url":        normalizeBaseURL(in.BaseURL),
		"models":          mustJSON(defaultModels(in.Models, in.DefaultModel)),
		"default_model":   in.DefaultModel,
		"timeout_sec":     defaultInt(in.Timeout, m.TimeoutSec),
		"max_concurrency": defaultInt(in.MaxConcurrency, m.MaxConcurrency),
		"temperature":     defaultFloat(in.Temperature, m.Temperature),
		"max_tokens":      defaultInt(in.MaxTokens, m.MaxTokens),
		"description":     in.Desc,
		"updated_at":      now(),
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	// 密钥留空表示"不变"（前端回显的是掩码，无法回传原文）。
	if key := strings.TrimSpace(in.APIKey); key != "" {
		sealed, err := s.deps.Sealer.Seal(key)
		if err != nil {
			return nil, errs.Internalf(err, "加密密钥失败")
		}
		updates["api_key_cipher"] = sealed.Cipher
		updates["api_key_iv"] = sealed.IV
		updates["api_key_auth_tag"] = sealed.AuthTag
		updates["api_key_masked"] = crypto.Mask(key)
		updates["status"] = "unchecked" // 换密钥后需要重测连通性
	}
	if err := s.db(ctx).Model(&model.AIModelConfig{}).Where("id = ?", m.ID).Updates(updates).Error; err != nil {
		return nil, errs.Internalf(err, "更新模型通道失败")
	}
	if in.IsDefault != nil && *in.IsDefault {
		if err := s.setSoleDefault(ctx, m.ID); err != nil {
			return nil, err
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "model:update", TargetType: "ai_model_config",
		TargetID: &m.ID, TargetName: m.Name, Detail: "编辑模型通道 " + m.Name,
	})
	var fresh model.AIModelConfig
	if err := s.db(ctx).First(&fresh, m.ID).Error; err != nil {
		return nil, errs.Internalf(err, "查询模型通道失败")
	}
	dto := modelToDTO(&fresh)
	return &dto, nil
}

// DeleteModelProvider 删除模型通道。
func (s *SystemService) DeleteModelProvider(ctx context.Context, id uint64, c Caller) error {
	var m model.AIModelConfig
	if err := s.db(ctx).First(&m, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errs.NotFoundf("模型通道不存在")
		}
		return errs.Internalf(err, "查询模型通道失败")
	}
	if err := s.db(ctx).Delete(&model.AIModelConfig{}, m.ID).Error; err != nil {
		return errs.Internalf(err, "删除模型通道失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "model:delete", TargetType: "ai_model_config",
		TargetID: &m.ID, TargetName: m.Name, Detail: "删除模型通道 " + m.Name,
	})
	return nil
}

// SetDefaultModelProvider 设为平台默认通道。
func (s *SystemService) SetDefaultModelProvider(ctx context.Context, id uint64, c Caller) (*ModelProviderDTO, error) {
	var m model.AIModelConfig
	if err := s.db(ctx).First(&m, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("模型通道不存在")
		}
		return nil, errs.Internalf(err, "查询模型通道失败")
	}
	if !m.Enabled {
		return nil, errs.StateConflict("已停用的通道不能设为默认通道")
	}
	if err := s.setSoleDefault(ctx, m.ID); err != nil {
		return nil, err
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "model:set-default", TargetType: "ai_model_config",
		TargetID: &m.ID, TargetName: m.Name, Detail: "设为默认模型通道 " + m.Name,
	})
	var fresh model.AIModelConfig
	if err := s.db(ctx).First(&fresh, m.ID).Error; err != nil {
		return nil, errs.Internalf(err, "查询模型通道失败")
	}
	dto := modelToDTO(&fresh)
	return &dto, nil
}

// TestModelProvider 连通性测试。
func (s *SystemService) TestModelProvider(ctx context.Context, id uint64, c Caller) (*ModelTestResult, error) {
	var m model.AIModelConfig
	if err := s.db(ctx).First(&m, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("模型通道不存在")
		}
		return nil, errs.Internalf(err, "查询模型通道失败")
	}

	apiKey := ""
	if len(m.APIKeyCipher) > 0 {
		plain, err := s.deps.Sealer.Open(crypto.Sealed{Cipher: m.APIKeyCipher, IV: m.APIKeyIV, AuthTag: m.APIKeyAuthTag})
		if err != nil {
			return nil, errs.Internalf(err, "解密通道密钥失败")
		}
		apiKey = plain
	}

	timeout := time.Duration(m.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := llm.New(llm.Config{
		BaseURL: m.BaseURL, APIKey: apiKey, Model: m.DefaultModel,
		Timeout: timeout, Name: m.Name, Provider: m.Vendor,
	})

	// 先落 testing 状态，让前端按钮转圈有据可依（多实例下别的请求也能看到）。
	if err := s.db(ctx).Model(&model.AIModelConfig{}).Where("id = ?", m.ID).Update("status", "testing").Error; err != nil {
		s.log("system").Warn("更新通道状态失败", "err", err)
	}

	at := now()
	latency, pingErr := client.Ping(ctx)
	updates := map[string]any{"checked_at": &at, "updated_at": at}
	res := &ModelTestResult{ProviderID: httpx.IDString(m.ID), Name: m.Name}

	if pingErr != nil {
		updates["status"] = "failed"
		updates["fail_count"] = m.FailCount + 1
		res.OK = false
		res.Message = pingErr.Error()
	} else {
		ms := int(latency.Milliseconds())
		updates["status"] = "connected"
		updates["latency_ms"] = ms
		res.OK = true
		res.Latency = ms
		res.Message = fmt.Sprintf("连通正常，耗时 %dms", ms)
	}
	if err := s.db(ctx).Model(&model.AIModelConfig{}).Where("id = ?", m.ID).Updates(updates).Error; err != nil {
		s.log("system").Warn("更新通道状态失败", "err", err)
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "model:test", TargetType: "ai_model_config",
		TargetID: &m.ID, TargetName: m.Name,
		Detail: "测试模型通道：" + res.Message, Result: resultWord(res.OK),
	})
	return res, nil
}

// ModelTestResult 模型连通性结果。
type ModelTestResult struct {
	ProviderID string `json:"providerId"`
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	Latency    int    `json:"latency"`
}

// defaultModelClient 取平台默认通道的客户端，供 AI 能力使用。
func (s *SystemService) defaultModelClient(ctx context.Context) (*llm.Client, *model.AIModelConfig, error) {
	if !s.deps.Cfg.Integration.LLM.Enabled {
		return nil, nil, errs.NotFoundf("AI 能力未启用（integration.llm.enabled=false）")
	}
	var m model.AIModelConfig
	err := s.db(ctx).Where("enabled = ?", true).Order("is_default desc, id").First(&m).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, errs.NotFoundf("尚未配置可用的模型通道，请到「系统管理 › 模型设置」添加")
		}
		return nil, nil, errs.Internalf(err, "查询模型通道失败")
	}

	apiKey := ""
	if len(m.APIKeyCipher) > 0 {
		plain, oerr := s.deps.Sealer.Open(crypto.Sealed{Cipher: m.APIKeyCipher, IV: m.APIKeyIV, AuthTag: m.APIKeyAuthTag})
		if oerr != nil {
			return nil, nil, errs.Internalf(oerr, "解密通道密钥失败")
		}
		apiKey = plain
	}
	timeout := time.Duration(m.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = s.deps.Cfg.Integration.LLM.Timeout.D()
	}
	return llm.New(llm.Config{
		BaseURL: m.BaseURL, APIKey: apiKey, Model: m.DefaultModel,
		Timeout: timeout, MaxTokens: m.MaxTokens, Temperature: m.Temperature,
		Name: m.Name, Provider: m.Vendor,
	}), &m, nil
}

// DefaultModelClient 暴露默认模型通道给 AI 会话服务复用。
//
// 之所以导出而不是让 AIService 自带一份实现：密钥解密、超时兜底、
// "未配置通道"的提示文案都在下面这个函数里，多一份实现就多一个漏改点。
func (s *SystemService) DefaultModelClient(ctx context.Context) (*llm.Client, *model.AIModelConfig, error) {
	return s.defaultModelClient(ctx)
}

// --------------------------------------------------------------------------- 平台自检

// HealthInfo 存活信息。
type HealthInfo struct {
	Status    string            `json:"status"`
	Version   string            `json:"version"`
	Env       string            `json:"env"`
	Uptime    string            `json:"uptime"`
	Source    map[string]string `json:"source"`
	CacheImpl string            `json:"cacheImpl"`
	DBDriver  string            `json:"dbDriver"`
}

// Health 存活探针：进程活着就返回 ok，不检查下游（否则下游抖动会导致 Pod 被反复重启）。
func (s *SystemService) Health() *HealthInfo {
	uptime := time.Since(s.deps.StartedAt)
	if s.deps.StartedAt.IsZero() {
		uptime = 0
	}
	return &HealthInfo{
		Status:    "ok",
		Version:   version,
		Env:       s.deps.Cfg.App.Env,
		Uptime:    uptime.Truncate(time.Second).String(),
		Source:    s.deps.Up.Source(),
		CacheImpl: s.deps.Cache.Name(),
		DBDriver:  s.deps.Cfg.Database.Driver,
	}
}

// ReadinessItem 单项依赖就绪状态。
type ReadinessItem struct {
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Message string `json:"message"`
}

// Readiness 就绪探针：检查依赖，未就绪时 K8s 不会把流量导进来。
//
// 只有"平台自身能不能服务请求"的依赖才算（数据库、Redis）。
// Harbor / GitLab / K8s 未接入不算未就绪——平台在没有它们时依然能提供
// 项目/环境/审计等能力，把它们算进去会导致 Pod 永远不 Ready。
func (s *SystemService) Readiness(ctx context.Context) (bool, []ReadinessItem) {
	items := []ReadinessItem{}

	if sqlDB, err := s.deps.DB.DB(); err != nil {
		items = append(items, ReadinessItem{Name: "database", Ready: false, Message: err.Error()})
	} else if err := sqlDB.PingContext(ctx); err != nil {
		items = append(items, ReadinessItem{Name: "database", Ready: false, Message: err.Error()})
	} else {
		items = append(items, ReadinessItem{Name: "database", Ready: true, Message: "ok"})
	}

	if !s.deps.Cfg.Redis.Enabled {
		items = append(items, ReadinessItem{Name: "redis", Ready: true, Message: "未启用（使用进程内实现）"})
	} else if err := s.deps.Cache.Ping(ctx); err != nil {
		items = append(items, ReadinessItem{Name: "redis", Ready: false, Message: err.Error()})
	} else {
		items = append(items, ReadinessItem{Name: "redis", Ready: true, Message: "ok"})
	}

	ready := true
	for _, it := range items {
		if !it.Ready {
			ready = false
			break
		}
	}
	return ready, items
}

// MetricsText 输出 Prometheus 文本格式的精简指标。
//
// 刻意不引 prometheus client 库：一期真正需要被监控的只有四件事——
// 进程活着、内存与协程数正常、任务队列堵没堵、数据库连接池有没有打满。
// 这些用运行时自带的数据就能算出来，多一个依赖就多一份版本与注册表的维护成本。
//
// 输出格式遵循 text/plain; version=0.0.4，Prometheus 可直接抓取。
func (s *SystemService) MetricsText() string {
	var b strings.Builder

	gauge := func(name, help string, value float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, value)
	}

	gauge("yunnian_up", "平台进程是否存活（恒为 1）", 1)
	if !s.deps.StartedAt.IsZero() {
		gauge("yunnian_uptime_seconds", "进程运行时长（秒）", time.Since(s.deps.StartedAt).Seconds())
	}

	// 任务队列：这两组指标是"平台卡住了"的第一现场——
	// running 长期打满 + queued 持续增长，说明并发上限或下游变慢了。
	if s.deps.Builds != nil {
		gauge("yunnian_build_queue_running", "正在执行的构建数", float64(s.deps.Builds.Running()))
		gauge("yunnian_build_queue_queued", "排队中的构建数", float64(s.deps.Builds.Queued()))
	}
	if s.deps.Deploys != nil {
		gauge("yunnian_deploy_queue_running", "正在执行的部署数", float64(s.deps.Deploys.Running()))
		gauge("yunnian_deploy_queue_queued", "排队中的部署数", float64(s.deps.Deploys.Queued()))
	}

	// 数据库连接池：in_use 持续贴近 max_open 会造成请求排队等待，
	// 表现是"接口莫名变慢"，只看数据库自身指标是看不出来的。
	if sqlDB, err := s.deps.DB.DB(); err == nil && sqlDB != nil {
		st := sqlDB.Stats()
		gauge("yunnian_db_open_connections", "已建立的数据库连接数", float64(st.OpenConnections))
		gauge("yunnian_db_in_use_connections", "正在被使用的连接数", float64(st.InUse))
		gauge("yunnian_db_idle_connections", "空闲连接数", float64(st.Idle))
		gauge("yunnian_db_wait_count_total", "等待连接的累计次数", float64(st.WaitCount))
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	gauge("yunnian_go_goroutines", "当前协程数", float64(runtime.NumGoroutine()))
	gauge("yunnian_go_memstats_alloc_bytes", "当前堆上已分配内存（字节）", float64(mem.Alloc))
	gauge("yunnian_go_memstats_sys_bytes", "向系统申请的内存总量（字节）", float64(mem.Sys))
	gauge("yunnian_go_gc_count_total", "GC 累计执行次数", float64(mem.NumGC))

	enabled := 0.0
	if s.deps.Cache.Enabled() {
		enabled = 1
	}
	gauge("yunnian_redis_enabled", "是否启用 Redis（1=是，0=退回进程内实现）", enabled)

	return b.String()
}

// --------------------------------------------------------------------------- 内部助手

const (
	version                = "1.0.0"
	defaultInitialPassword = "changeme"
)

func (s *SystemService) hashPassword(plain string) (string, error) {
	cost := s.deps.Cfg.Auth.BcryptCost
	if cost < 10 || cost > 15 {
		cost = 10
	}
	b, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return "", errs.Internalf(err, "生成密码哈希失败")
	}
	return string(b), nil
}

func (s *SystemService) roleByCode(ctx context.Context, code string) (*model.Role, error) {
	var r model.Role
	if err := s.db(ctx).Where("code = ?", strings.TrimSpace(code)).First(&r).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("角色 %s 不存在", code)
		}
		return nil, errs.Internalf(err, "查询角色失败")
	}
	return &r, nil
}

func (s *SystemService) roleCodesOf(ctx context.Context, ids []uint64) (map[uint64]string, error) {
	out := map[uint64]string{}
	uniq := dedupeUint64(ids)
	if len(uniq) == 0 {
		return out, nil
	}
	type row struct {
		UserID uint64 `gorm:"column:user_id"`
		Code   string `gorm:"column:code"`
	}
	var rows []row
	if err := s.db(ctx).Table("user_role").
		Select("user_role.user_id AS user_id, role.code AS code").
		Joins("JOIN role ON role.id = user_role.role_id").
		Where("user_role.user_id IN ?", uniq).
		Scan(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询用户角色失败")
	}
	for _, r := range rows {
		out[r.UserID] = r.Code
	}
	return out, nil
}

func (s *SystemService) teamsOf(ctx context.Context, ids []uint64) (map[uint64]string, error) {
	out := map[uint64]string{}
	uniq := dedupeUint64(ids)
	if len(uniq) == 0 {
		return out, nil
	}
	type row struct {
		OwnerID uint64 `gorm:"column:owner_id"`
		Team    string `gorm:"column:team"`
	}
	var rows []row
	if err := s.db(ctx).Table("project").
		Select("DISTINCT owner_id, team").
		Where("owner_id IN ? AND team <> '' AND deleted_at IS NULL", uniq).
		Scan(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询用户团队失败")
	}
	for _, r := range rows {
		if _, ok := out[r.OwnerID]; !ok {
			out[r.OwnerID] = r.Team
		}
	}
	return out, nil
}

func (s *SystemService) permsOfRoles(ctx context.Context, ids []uint64) (map[uint64][]string, error) {
	out := map[uint64][]string{}
	uniq := dedupeUint64(ids)
	if len(uniq) == 0 {
		return out, nil
	}
	type row struct {
		RoleID uint64 `gorm:"column:role_id"`
		Code   string `gorm:"column:code"`
	}
	var rows []row
	if err := s.db(ctx).Table("role_permission").
		Select("role_permission.role_id AS role_id, permission.code AS code").
		Joins("JOIN permission ON permission.id = role_permission.permission_id").
		Where("role_permission.role_id IN ?", uniq).
		Order("permission.module, permission.code").
		Scan(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询角色权限失败")
	}
	for _, r := range rows {
		out[r.RoleID] = append(out[r.RoleID], r.Code)
	}
	return out, nil
}

func (s *SystemService) userCountOfRoles(ctx context.Context, ids []uint64) (map[uint64]int64, error) {
	out := map[uint64]int64{}
	uniq := dedupeUint64(ids)
	if len(uniq) == 0 {
		return out, nil
	}
	type row struct {
		RoleID uint64 `gorm:"column:role_id"`
		Total  int64  `gorm:"column:total"`
	}
	var rows []row
	if err := s.db(ctx).Table("user_role").
		Select("role_id, COUNT(*) AS total").
		Where("role_id IN ?", uniq).
		Group("role_id").
		Scan(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "统计角色用户数失败")
	}
	for _, r := range rows {
		out[r.RoleID] = r.Total
	}
	return out, nil
}

// ensureNotLastAdmin 校验"这是最后一个启用的管理员"。
func (s *SystemService) ensureNotLastAdmin(ctx context.Context, userID uint64) error {
	var adminRole model.Role
	if err := s.db(ctx).Where("code = ?", "admin").First(&adminRole).Error; err != nil {
		return nil // 没有 admin 角色说明是定制部署，不做限制
	}
	var adminIDs []uint64
	if err := s.db(ctx).Table("user_role").Where("role_id = ?", adminRole.ID).
		Pluck("user_id", &adminIDs).Error; err != nil {
		return errs.Internalf(err, "查询管理员失败")
	}
	if len(adminIDs) == 0 {
		return nil
	}
	var enabledAdmins int64
	if err := s.db(ctx).Model(&model.User{}).
		Where("id IN ? AND enabled = ?", dedupeUint64(adminIDs), true).
		Count(&enabledAdmins).Error; err != nil {
		return errs.Internalf(err, "统计管理员失败")
	}
	// 目标用户本身是管理员，且它是唯一启用的那个
	if enabledAdmins <= 1 {
		for _, id := range adminIDs {
			if id == userID {
				return errs.Invalid("这是最后一个启用的管理员账号，操作后平台将无人可管理")
			}
		}
	}
	return nil
}

// sealAPIKey 加密密钥并回填掩码/密文字段。
func (s *SystemService) sealAPIKey(m *model.AIModelConfig, plain string) error {
	sealed, err := s.deps.Sealer.Seal(plain)
	if err != nil {
		return errs.Internalf(err, "加密密钥失败")
	}
	m.APIKeyCipher = sealed.Cipher
	m.APIKeyIV = sealed.IV
	m.APIKeyAuthTag = sealed.AuthTag
	m.APIKeyMasked = crypto.Mask(plain)
	return nil
}

// setSoleDefault 保证全局只有一个默认通道。
//
// 先清空再设置，且放在一个事务里：否则并发设置可能产生两个默认通道，
// 而"AI 用哪个模型"就会变成看运气的事。
func (s *SystemService) setSoleDefault(ctx context.Context, id uint64) error {
	return s.tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Model(&model.AIModelConfig{}).Where("is_default = ?", true).
			Update("is_default", false).Error; err != nil {
			return err
		}
		return tx.Model(&model.AIModelConfig{}).Where("id = ?", id).
			Updates(map[string]any{"is_default": true, "enabled": true, "updated_at": now()}).Error
	})
}

func validateModelBaseURL(raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" {
		return errs.Invalid("请填写 API Base 地址")
	}
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		return errs.Invalid("API Base 地址需以 http:// 或 https:// 开头")
	}
	if strings.HasSuffix(v, "/chat/completions") {
		return errs.Invalid("API Base 地址填到 /v1 这一级即可，不需要带 /chat/completions（后端会补）")
	}
	return nil
}

// normalizeBaseURL 去掉尾部斜杠，避免拼出 //chat/completions。
func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func validateNotifyTarget(typ, target string) error {
	t := strings.TrimSpace(target)
	if t == "" {
		return errs.Invalid("请填写通知目标")
	}
	switch typ {
	case "email":
		if !strings.Contains(t, "@") {
			return errs.Invalid("邮件通知的目标应为邮箱地址（多个用英文逗号分隔）")
		}
	case "dingtalk":
		if !strings.HasPrefix(t, "http://") && !strings.HasPrefix(t, "https://") {
			return errs.Invalid("钉钉机器人的目标应为完整 Webhook 地址（以 https:// 开头）")
		}
		// 机器人若开了「加签」，光有 access_token 会被钉钉拒成 310000。
		// 在这里就提醒，好过等用户点「测试」才从对端的错误里反推。
		if !strings.Contains(t, "access_token=") {
			return errs.Invalid("钉钉机器人的地址里缺少 access_token，请从机器人设置里复制完整的 Webhook 地址")
		}
	case "wecom", "feishu", "webhook":
		// 类型枚举已在入参 binding 里拦掉这三种（见 NotifyInput），
		// 这里再兜一层是防"库里已有的历史渠道"被直接拿来发消息。
		return errs.Invalid("渠道类型 " + typ + " 尚未接入发送器，平台当前只支持邮件与钉钉")
	default:
		return errs.Invalid("渠道类型 " + typ + " 尚未接入发送器，平台当前只支持邮件与钉钉")
	}
	return nil
}

// validateNotifyEvents 校验订阅事件是否都是平台真会发出的事件。
//
// ⚠️ 这条校验是"能勾选 == 后端受理"唯一的执行点。此前 defaultEvents 会把任意
// 字符串原样存库：界面上有 8 个事件码可选，其中 5 个（build_success、
// deploy_success、rollback_executed、env_unhealthy、deploy_approval 的旧写法）
// 没有任何代码会触发。用户勾了、看到勾上了、以为配好了 —— 然后永远收不到。
// 这种"安静的假承诺"比报错难发现得多：没有任何地方会亮红灯。
//
// 2026-10-15 成功侧（build_succeeded / deploy_succeeded）**带着触发点**加回了清单，
// 所以现在可选的是七个码。具体几个以 SupportedNotifyEvents 为准 —— 别在这里写死数字，
// 白名单、前端选项、错误提示里的可选值全部由那一份清单派生。
func validateNotifyEvents(events []string) error {
	labels := make([]string, 0, len(SupportedNotifyEvents))
	for _, e := range SupportedNotifyEvents {
		labels = append(labels, fmt.Sprintf("%s（%s）", eventLabel(e), e))
	}
	for _, e := range events {
		if !IsSupportedNotifyEvent(e) {
			return errs.Invalid(fmt.Sprintf(
				"事件「%s」平台尚未实现，订阅了也不会推送。目前可订阅：%s",
				e, strings.Join(labels, "、")))
		}
	}
	return nil
}

// defaultEvents 未指定事件时的默认订阅。
//
// 直接取平台会推送的**全部**事件，而不是维护一份"默认三项"的硬编码清单：
// 那份清单每加一个事件码就得回来补一次，漏了就会让 API 建的渠道静默少收一类告警 ——
// 而"少收"没有任何地方会亮红灯。取全集至少不会漏；用户嫌吵可以在界面上取消勾选。
func defaultEvents(in []string) []string {
	if len(in) > 0 {
		return in
	}
	out := make([]string, 0, len(SupportedNotifyEvents))
	for _, e := range SupportedNotifyEvents {
		out = append(out, string(e))
	}
	return out
}

func defaultModels(in []string, def string) []string {
	if len(in) > 0 {
		return in
	}
	if strings.TrimSpace(def) != "" {
		return []string{def}
	}
	return []string{}
}

func describeUserUpdate(in UpdateUserInput) string {
	parts := []string{}
	if in.Name != nil {
		parts = append(parts, "姓名")
	}
	if in.Email != nil {
		parts = append(parts, "邮箱")
	}
	if in.Phone != nil {
		parts = append(parts, "手机号")
	}
	if in.Role != nil {
		parts = append(parts, "角色="+*in.Role)
	}
	if in.Enabled != nil {
		if *in.Enabled {
			parts = append(parts, "启用")
		} else {
			parts = append(parts, "停用")
		}
	}
	if len(parts) == 0 {
		return "更新用户信息"
	}
	return "更新用户：" + strings.Join(parts, "、")
}

func roleColor(code string) string {
	switch code {
	case "admin":
		return "red"
	case "ops":
		return "orange"
	case "dev":
		return "blue"
	case "qa":
		return "green"
	case "readonly":
		return "default"
	}
	return "default"
}

// parseTimeParam 解析时间参数，兼容 RFC3339 与 "2006-01-02 15:04:05"。
func parseTimeParam(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间 %q", v)
}

// --------------------------------------------------------------------------- Harbor 接入
//
// 与 Jenkins 同一套形状（配置读 / 配置写 / 连通性测试），差别只在**两条通道**：
// registry 组件（/v2/，读 digest）与 Core 管理接口（/api/ 或 /api/v2.0/，读 tag 列表）。
// 它们通常在同一个入口上，但**不一定**，所以测试连接分别出结论；
// Core 地址留空时按 registry 地址探（见 integration.NewHarborCore）。

// HarborSettingsDTO 对外视图：口令只回"配没配"，绝不回明文。
type HarborSettingsDTO struct {
	Enabled    bool   `json:"enabled"`
	BaseURL    string `json:"baseUrl"`
	CoreAPIURL string `json:"coreApiUrl"`
	Registry   string `json:"registry"`
	Username   string `json:"username"`
	// HasPassword 只回布尔 —— 与 kubeconfig 同一口径：明文没有读出口。
	HasPassword bool `json:"hasPassword"`
	Insecure    bool `json:"insecure"`
	Timeout     int  `json:"timeout"`
	// Impl 当前实际干活的实现（harbor-registry-v2 / local）。
	Impl string `json:"impl"`
	// Source 这份值来自 db 还是 config.yaml —— 让用户知道自己在改哪一份。
	Source string `json:"source"`
	// Identity 上次连通性测试识别到的账号。
	Identity  string `json:"identity"`
	UpdatedAt string `json:"updatedAt"`
}

// HarborSettingsInput 保存入参。
type HarborSettingsInput struct {
	Enabled    bool   `json:"enabled"`
	BaseURL    string `json:"baseUrl"`
	CoreAPIURL string `json:"coreApiUrl"`
	Registry   string `json:"registry"`
	Username   string `json:"username"`
	// Password 空 = 不修改；要清空必须显式 clearPassword（前端只看得到掩码）。
	Password      string `json:"password"`
	ClearPassword bool   `json:"clearPassword"`
	Insecure      bool   `json:"insecure"`
	Timeout       int    `json:"timeout"`
}

// TestHarborInput 测试入参。全空 = 测库里已保存的那份。
type TestHarborInput struct {
	BaseURL    string `json:"baseUrl"`
	CoreAPIURL string `json:"coreApiUrl"`
	Registry   string `json:"registry"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Insecure   *bool  `json:"insecure"`
	Timeout    int    `json:"timeout"`
}

// HarborTestResult 两条通道各自的结论 + 一句总述。
type HarborTestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Latency int    `json:"latency"`
	// UsedSavedValues 说明本次测的是库里那份，还是表单里还没保存的值。
	UsedSavedValues bool `json:"usedSavedValues"`

	RegistryOK      bool   `json:"registryOk"`
	RegistryMessage string `json:"registryMessage"`
	CoreOK          bool   `json:"coreOk"`
	CoreMessage     string `json:"coreMessage"`
	CoreVersion     string `json:"coreVersion"`
	// CoreAPIVersion 探测到的 Core 接口版本："v1" / "v2"。两者的路径与能力都不同，
	// 界面要按它说明"能不能拿到推送时间"。
	CoreAPIVersion string `json:"coreApiVersion"`
	// CoreNoPushTime 这台 Harbor 给不出 tag 推送时间（v1 就是）。
	// **必须传到界面上**：否则用户会以为"最新 tag"是按推送时间排的。
	CoreNoPushTime bool `json:"coreNoPushTime"`
	// CoreProjects 当前凭据可见的项目数（Core 通道通时才有意义）。
	CoreProjects int `json:"coreProjects"`
	// CoreRepos 可见的镜像仓库数 —— 只有 v1 匿名探测给得出（走 /api/search）。
	CoreRepos int `json:"coreRepos"`
}

// HarborSettings 读取当前 Harbor 连接配置（不含口令明文）。
func (s *SystemService) HarborSettings(ctx context.Context) (*HarborSettingsDTO, error) {
	cur, err := store.LoadHarborConfig(ctx, s.deps.DB, s.deps.Sealer, s.deps.Cfg.Integration.Harbor)
	if err != nil {
		return nil, errs.Internalf(err, "读取 Harbor 配置失败")
	}
	return harborSettingsDTO(cur, s.deps.Up.Registry.Name()), nil
}

// UpdateHarborSettings 保存 Harbor 连接配置。
func (s *SystemService) UpdateHarborSettings(ctx context.Context, in HarborSettingsInput, c Caller) (*HarborSettingsDTO, error) {
	base, err := normalizeHarborURL(in.BaseURL, "registry 组件地址", "https://es-repo.example.com:5000")
	if err != nil {
		return nil, err
	}
	core, err := normalizeHarborURL(in.CoreAPIURL, "Core API 地址", "https://harbor.example.com")
	if err != nil {
		return nil, err
	}
	// 启用前至少要有一个地址：否则开关是"开了但什么都不会发生"，界面上却显示已接入。
	if in.Enabled && base == "" {
		return nil, errs.Invalid("启用 Harbor 接入前请先填写 registry 组件地址 —— " +
			"部署时的镜像存在性校验走的是这条通道（/v2/）")
	}
	if in.ClearPassword && strings.TrimSpace(in.Password) != "" {
		return nil, errs.Invalid("不能同时提交新密码与清除密码，请二选一")
	}
	registry, err := normalizeHarborRegistry(in.Registry)
	if err != nil {
		return nil, err
	}
	// Core 地址与 registry 地址填成同一个**不是错**：两者常常就在同一个入口上
	// （实测那台 Harbor v1.1.2：/v2/ 与 /api/ 都在 :5000；Harbor v2 也是一个入口同时提供
	// /v2/ 与 /api/v2.0/）。所以既不拦也不提示，只把它当"显式覆盖 registry 地址"。
	replacePwd := strings.TrimSpace(in.Password) != ""
	if err := store.SaveHarborConfig(ctx, s.deps.DB, s.deps.Sealer, store.HarborSaveInput{
		Enabled: in.Enabled, BaseURL: base, CoreURL: core, Registry: registry,
		Username: in.Username, Insecure: in.Insecure, Timeout: in.Timeout,
		Password: in.Password, ClearPassword: in.ClearPassword,
		// 传入配置文件里的口令：首次保存且未填密码时它会被搬进加密存储，
		// 让"留空=不修改"在首次保存时也字面成立。
		FallbackPassword: s.deps.Cfg.Integration.Harbor.Password,
		UserID:           c.UserID(),
	}); err != nil {
		return nil, errs.Internalf(err, "保存 Harbor 配置失败")
	}

	// 审计只记"改了哪几项"，绝不记口令本身。
	changes := []string{}
	if in.Enabled {
		changes = append(changes, "启用接入")
	} else {
		changes = append(changes, "停用接入")
	}
	if base != "" {
		changes = append(changes, "registry 地址="+base)
	}
	if core != "" {
		changes = append(changes, "Core 地址="+core)
	}
	if u := strings.TrimSpace(in.Username); u != "" {
		changes = append(changes, "账号="+u)
	}
	switch {
	case replacePwd:
		changes = append(changes, "更新密码")
	case in.ClearPassword:
		changes = append(changes, "清除密码")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "integration:harbor-update", TargetType: "integration",
		TargetName: "harbor", Detail: "更新 Harbor 接入配置（" + strings.Join(changes, "，") + "）",
	})

	return s.HarborSettings(ctx)
}

// TestHarborConnection 分别探测两条通道。
//
// `ok=false` 是**正常响应**（HTTP 200）：表示"请求发出去了但没通"，
// 与"接口本身出错"是两回事 —— 前端必须读 ok 与两条通道各自的 message。
func (s *SystemService) TestHarborConnection(ctx context.Context, in TestHarborInput, c Caller) (*HarborTestResult, error) {
	cur, err := store.LoadHarborConfig(ctx, s.deps.DB, s.deps.Sealer, s.deps.Cfg.Integration.Harbor)
	if err != nil {
		return nil, errs.Internalf(err, "读取 Harbor 配置失败")
	}
	cfg := cur.Cfg
	usingSaved := true

	if v := strings.TrimSpace(in.BaseURL); v != "" {
		u, err := normalizeHarborURL(v, "registry 组件地址", "https://es-repo.example.com:5000")
		if err != nil {
			return nil, err
		}
		cfg.BaseURL, usingSaved = u, false
	}
	if v := strings.TrimSpace(in.CoreAPIURL); v != "" {
		u, err := normalizeHarborURL(v, "Core API 地址", "https://harbor.example.com")
		if err != nil {
			return nil, err
		}
		cfg.CoreAPIURL, usingSaved = u, false
	}
	if v := strings.TrimSpace(in.Registry); v != "" {
		r, err := normalizeHarborRegistry(v)
		if err != nil {
			return nil, err
		}
		cfg.Registry, usingSaved = r, false
	}
	if v := strings.TrimSpace(in.Username); v != "" {
		cfg.Username, usingSaved = v, false
	}
	if v := strings.TrimSpace(in.Password); v != "" {
		cfg.Password, usingSaved = v, false
	}
	if in.Insecure != nil {
		cfg.Insecure, usingSaved = *in.Insecure, false
	}
	if in.Timeout > 0 {
		cfg.Timeout, usingSaved = config.Duration(time.Duration(in.Timeout)*time.Second), false
	}
	if cfg.Timeout.D() <= 0 {
		cfg.Timeout = config.Duration(15 * time.Second)
	}

	res := &HarborTestResult{UsedSavedValues: usingSaved}
	base, core := strings.TrimSpace(cfg.BaseURL), strings.TrimSpace(cfg.CoreAPIURL)
	if base == "" && core == "" {
		res.Message = "两个地址都没填：registry 组件地址（读 digest）与 Core API 地址（读 tag 列表）至少要填一个"
		return res, nil
	}
	// Core 地址留空 ⇒ **按 registry 地址探**，不是"跳过 Core 通道"。
	// 依据（实测）：那台 Harbor 是 v1.1.2，Core(/api/) 与 registry(/v2/) 就在同一个 :5000 上；
	// Harbor v2 的常规部署也是一个入口同时提供 /v2/ 与 /api/v2.0/。
	// 所以第二个地址只是给"registry 走独立端口/镜像站"的部署留的覆盖项。
	// 不这么回退的话，用户会看到一句"未填写 Core API 地址"，以为还得再找一个地址。
	coreProbeAddr := core
	if coreProbeAddr == "" {
		coreProbeAddr = base
	}
	cfg.CoreAPIURL = coreProbeAddr
	// 测试的语义就是"假设已启用"：未启用的配置也要能测通，
	// 否则用户必须"先保存再测试"，而保存意味着把一份没验过的配置写进了库。
	cfg.Enabled = true

	start := time.Now()

	if base != "" {
		client := integration.NewHarborRegistry(cfg)
		if perr := client.Ping(ctx); perr != nil {
			res.RegistryMessage = perr.Error()
		} else {
			res.RegistryOK = true
			res.RegistryMessage = fmt.Sprintf("%s 连通（/v2/ 可读）", client.Base())
		}
	} else {
		res.RegistryMessage = "未填写 registry 组件地址"
	}

	if coreProbeAddr != "" {
		client := integration.NewHarborCore(cfg)
		if client == nil {
			res.CoreMessage = "Core API 地址无法解析"
		} else if probe, perr := client.Ping(ctx); perr != nil {
			res.CoreMessage = perr.Error()
		} else {
			res.CoreOK = probe.OK
			res.CoreMessage = probe.Message
			if core == "" && base != "" {
				// 说明白"这不是你没填，是平台按同一个地址探的"，
				// 且报**实际探测的地址**（可能已削掉误填的 /v2 尾巴）。
				res.CoreMessage += fmt.Sprintf("（未单独填 Core 地址，按 registry 地址 %s 探测）", client.Base())
			}
			res.CoreVersion = probe.Version
			res.CoreAPIVersion = probe.APIVersion
			res.CoreNoPushTime = probe.NoPushTime
			res.CoreProjects = probe.ProjectsVisible
			res.CoreRepos = probe.ReposVisible
		}
	} else {
		res.CoreMessage = "未填写 Core API 地址"
	}

	res.Latency = int(time.Since(start).Milliseconds())

	// 总述：**填了的通道全通**才算通；没填的通道不算失败，但要说明它没被验证。
	switch {
	case res.RegistryOK && res.CoreOK:
		res.OK = true
		if res.CoreNoPushTime {
			// v1 的能力边界必须说出来：不然后面做"最新 tag"时会被一个
			// 按构建时间排的列表误导（用户以为那是推送时间）。
			// ⚠️ 这段是**原样渲染**到页面上的，不要用 `**加粗**` 这类 Markdown 标记。
			res.Message = fmt.Sprintf("两条通道都通（耗时 %dms）：registry 读 digest，Core 读 tag 列表。"+
				"⚠️ 这台是 Harbor %s 的 v1 接口，不提供推送时间 —— \"最新\"只能按镜像构建时间或 tag 名排",
				res.Latency, res.CoreVersion)
		} else {
			res.Message = fmt.Sprintf("两条通道都通（耗时 %dms）：registry 读 digest，Core 读 tag 的推送时间", res.Latency)
		}
	case base == "":
		res.OK = res.CoreOK
		res.Message = fmt.Sprintf("只有 Core 通道（%s）：镜像存在性校验将没有 registry 可读", ternary(res.CoreOK, "已连通", "不通"))
	default:
		res.Message = "至少有一条通道不通，见下面两条各自的结论"
	}

	if usingSaved {
		who := strings.TrimSpace(cfg.Username)
		if err := store.SaveHarborProbeResult(ctx, s.deps.DB, who); err != nil {
			s.log("system").Warn("记录 Harbor 探测结果失败", "err", err)
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "integration:harbor-test", TargetType: "integration",
		TargetName: "harbor",
		Detail: fmt.Sprintf("测试 Harbor 连通性：registry=%v，Core=%v（%s）",
			res.RegistryOK, res.CoreOK, res.Message),
	})
	return res, nil
}

// harborSettingsDTO 组装对外视图。口令在这里被"降级"成布尔，保证明文不出 service 层。
func harborSettingsDTO(cur *store.HarborSettings, impl string) *HarborSettingsDTO {
	source := "config"
	if cur.FromDB {
		source = "db"
	}
	dto := &HarborSettingsDTO{
		Enabled:     cur.Cfg.Enabled,
		BaseURL:     cur.Cfg.BaseURL,
		CoreAPIURL:  cur.Cfg.CoreAPIURL,
		Registry:    cur.Cfg.Registry,
		Username:    cur.Cfg.Username,
		HasPassword: cur.Cfg.Password != "",
		Insecure:    cur.Cfg.Insecure,
		Timeout:     int(cur.Cfg.Timeout.D().Seconds()),
		Impl:        impl,
		Source:      source,
		Identity:    cur.Identity,
	}
	if !cur.UpdatedAt.IsZero() {
		dto.UpdatedAt = cur.UpdatedAt.Format(time.RFC3339)
	}
	return dto
}

// normalizeHarborURL 规范化一个 http(s) 地址。
//
// 报错时说清"要填什么"，而不是只说"格式不对"—— 用户看到的地址栏提示词就是这里给的例子。
func normalizeHarborURL(v, what, example string) (string, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return "", errs.Invalid(fmt.Sprintf("%s 必须以 http:// 或 https:// 开头，例如 %s", what, example))
	}
	return strings.TrimRight(s, "/"), nil
}

// normalizeHarborRegistry 规范化"镜像地址里出现的仓库部分"（如 es-repo.example.com:5000）。
//
// 这一段**不能带 scheme**：它要原样拼进完整镜像引用（`<registry>/<imageName>:<tag>`），
// 带上 `https://` 会拼出一个 Kubernetes 认不出的镜像地址。
func normalizeHarborRegistry(v string) (string, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return "", nil
	}
	if strings.Contains(s, "://") {
		return "", errs.Invalid("镜像地址里的仓库部分不要带 http(s):// 前缀：" +
			"它会被原样拼进镜像引用，例如 es-repo.example.com:5000")
	}
	return strings.TrimRight(s, "/"), nil
}

func ternary(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}

// harborDetail 说清"连通的到底是哪条通道"，而不是只报一个地址 ——
// 这个集成有两条互不相通的通道，只写一个地址会让人以为两件事都通了。
func harborDetail(cfg config.HarborConfig) string {
	parts := []string{}
	base := strings.TrimSpace(cfg.BaseURL)
	core := strings.TrimSpace(cfg.CoreAPIURL)
	if base != "" {
		parts = append(parts, "registry="+base)
	}
	switch {
	case core != "":
		parts = append(parts, "Core="+core)
	case base != "":
		// 没单独填 Core 时按 registry 地址探，这里必须写出来——
		// 否则"状态页通着、配置里只有一行地址"看起来像少配了什么。
		parts = append(parts, "Core="+base+"（同 registry 地址）")
	}
	if len(parts) == 0 {
		return "已连通"
	}
	return "已连通：" + strings.Join(parts, "，")
}

// csvField 做 CSV 转义（含逗号/引号/换行时用双引号包裹）。
// 不引 encoding/csv 是因为我们要拼一整块字符串直接返回，而不是写文件。
func csvField(v string) string {
	if strings.ContainsAny(v, ",\"\n\r") {
		return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
	}
	return v
}

func userIDs(rows []model.User) []uint64 {
	out := make([]uint64, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ID)
	}
	return out
}

func roleIDs(rows []model.Role) []uint64 {
	out := make([]uint64, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ID)
	}
	return out
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func defaultFloat(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

// DeleteUser 删除用户。**硬删**（`user` 表没有 deleted_at，一期就是按行删除设计的）。
//
// 两条防锁死约束，缺一条就可能把平台弄成没人能管：
//  ① **不能删自己** —— 手滑一次就把自己踢出去，而且没有任何界面能救回来。
//  ② **不能删最后一个超级管理员** —— 删完就没有人能再进"用户/角色"页，
//     连"再把谁设成管理员"这个动作都做不了。
//
// 删之前先清 user_role：表上没有外键，不清就会留下指向空用户的授权行。
func (s *SystemService) DeleteUser(ctx context.Context, id uint64, c Caller) error {
	if uid := c.UserID(); uid != nil && *uid == id {
		return errs.Invalid("不能删除当前登录的账号")
	}
	var u model.User
	if err := s.db(ctx).First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errs.NotFoundf("用户不存在")
		}
		return errs.Internalf(err, "查询用户失败")
	}

	// 该用户是否持有 admin 角色
	var isAdmin int64
	if err := s.db(ctx).Table("user_role AS ur").
		Joins("JOIN role r ON r.id = ur.role_id").
		Where("ur.user_id = ? AND r.code = ?", id, "admin").Count(&isAdmin).Error; err != nil {
		return errs.Internalf(err, "查询用户角色失败")
	}
	if isAdmin > 0 {
		var remain int64
		if err := s.db(ctx).Table("user_role AS ur").
			Joins("JOIN role r ON r.id = ur.role_id").
			Where("r.code = ? AND ur.user_id <> ?", "admin", id).Count(&remain).Error; err != nil {
			return errs.Internalf(err, "统计管理员数量失败")
		}
		if remain == 0 {
			return errs.StateConflict("这是最后一个超级管理员，删除后将没有人能管理用户与角色")
		}
	}

	return s.db(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", id).Delete(&model.UserRole{}).Error; err != nil {
			return errs.Internalf(err, "清理用户角色失败")
		}
		if err := tx.Delete(&model.User{}, id).Error; err != nil {
			return errs.Internalf(err, "删除用户失败")
		}
		return nil
	})
}
