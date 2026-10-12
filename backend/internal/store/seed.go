package store

import (
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/yunnian/yunnian/internal/buildscript"
	"github.com/yunnian/yunnian/internal/config"
	"github.com/yunnian/yunnian/internal/model"
)

// 种子数据中的固定常量。
const (
	seedRegistry = "harbor.yunnian.internal"
	seedGitLab   = "gitlab.yunnian.internal"
	// seedPassword 是全部种子用户的初始密码。
	// 演示账号与前端 auth.ts 的 DEMO_ACCOUNT 保持一致，便于前后端联调时直接登录。
	seedPassword     = "changeme"
	seedDemoUsername = "ops"
)

// projectSpec 项目种子规格。20 条与前端 src/mock/projects.ts 完全一致，
// 保证前端切到真实接口后列表内容不变，只有数据来源变了。
type projectSpec struct {
	Key     string
	Name    string
	Type    string // frontend / backend
	Team    string
	Owner   string
	Desc    string
	Method  string // dockerfile / jenkins / script
	Rate    int    // 期望的构建成功率，用于生成统计数据
	Enabled bool
}

var projectSpecs = []projectSpec{
	// ---------------------------------------------------------------- 前端 10 个
	{"user-web", "用户中心前端", "frontend", "用户增长组", "张伟", "C 端用户中心、账号设置、实名认证等页面", "dockerfile", 96, true},
	{"admin-portal", "运营后台管理端", "frontend", "前端体验组", "李静", "运营人员使用的后台管理控制台", "dockerfile", 93, true},
	{"mall-h5", "商城 H5", "frontend", "交易中台组", "王强", "移动端商城活动页与下单流程", "dockerfile", 88, true},
	{"merchant-web", "商户工作台", "frontend", "交易中台组", "刘洋", "商户入驻、商品管理、结算对账", "dockerfile", 91, true},
	{"data-board", "实时数据大屏", "frontend", "数据平台组", "陈晨", "业务实时指标大屏，支持投屏展示", "dockerfile", 85, true},
	{"open-platform-web", "开放平台门户", "frontend", "基础架构组", "赵敏", "开放平台文档、应用管理、API 调试台", "dockerfile", 94, true},
	{"cms-web", "内容管理后台", "frontend", "前端体验组", "孙磊", "文章、Banner、活动位配置管理", "dockerfile", 92, true},
	{"finance-web", "财务结算前端", "frontend", "支付结算组", "周婷", "账单、发票、结算单管理界面", "dockerfile", 90, true},
	{"workbench-web", "员工工作台", "frontend", "基础架构组", "吴迪", "内部员工统一工作台与待办中心", "dockerfile", 95, true},
	{"docs-site", "帮助文档站", "frontend", "前端体验组", "郑凯", "产品帮助文档与 FAQ 静态站点", "script", 99, true},

	// ---------------------------------------------------------------- 后端 10 个
	{"user-service", "用户服务", "backend", "用户增长组", "冯雪", "用户账户、登录鉴权、权限数据服务", "dockerfile", 97, true},
	{"order-service", "订单服务", "backend", "交易中台组", "韩磊", "订单创建、状态流转、履约调度", "dockerfile", 92, true},
	{"payment-service", "支付结算服务", "backend", "支付结算组", "曹阳", "支付渠道对接、退款、对账", "jenkins", 89, true},
	{"product-service", "商品服务", "backend", "交易中台组", "谢琳", "商品、SKU、库存与价格中心", "dockerfile", 94, true},
	{"gateway-service", "API 网关", "backend", "基础架构组", "运维示例", "统一入口、路由转发、限流熔断", "dockerfile", 98, true},
	{"notify-service", "消息通知服务", "backend", "基础架构组", "邓超", "短信、邮件、站内信、推送通道聚合", "jenkins", 90, true},
	{"search-service", "搜索服务", "backend", "数据平台组", "罗静", "商品与内容检索，基于 ES 的搜索能力", "dockerfile", 87, true},
	{"report-service", "报表统计服务", "backend", "数据平台组", "高鹏", "离线与实时报表计算、指标聚合", "dockerfile", 86, true},
	{"auth-service", "统一认证服务", "backend", "基础架构组", "林曦", "OAuth2 / OIDC 统一身份认证", "dockerfile", 96, true},
	{"file-service", "文件存储服务", "backend", "基础架构组", "何俊", "对象存储接入、图片处理、断点续传", "script", 93, false},
}

// envSpec 环境种子规格。
//
// 逐条与前端 src/mock/environments.ts 的 ENV_SPECS 对齐（名称、集群、namespace、
// 域名、审批、健康度、资源用量、环境变量、关联项目），保证前端切到真实接口后
// 页面内容不变，只有数据来源变了。
//
// 注意 projectKeys 是**显式枚举**而不是按规则推导：环境里有几个服务是业务决策，
// 不是可以算出来的——原型里这份清单也是硬编码的，照抄才能保证一致。
type envSpec struct {
	Key          string
	Name         string
	Cluster      string
	ClusterVer   string
	Namespace    string
	DeployMethod string
	Domain       string
	Owner        string
	Description  string
	Approval     bool
	Approvers    []string
	Health       string
	CPU          uint8
	Mem          uint8
	Nodes        uint32
	Enabled      bool
	Vars         [][3]string // {key, value, "secret"|""}
	ProjectKeys  []string
}

var envSpecs = []envSpec{
	{
		Key: "test", Name: "测试区", Cluster: "k8s-test-01", ClusterVer: "v1.29.6",
		Namespace: "yunnian-test", DeployMethod: "k8s", Domain: "*.test.yunnian.cn",
		Owner: "王强", Description: "研发自测与联调环境，允许高频发布，不做审批",
		Health: "healthy", CPU: 42, Mem: 57, Nodes: 6, Enabled: true,
		Vars: [][3]string{
			{"SPRING_PROFILES_ACTIVE", "test", ""},
			{"LOG_LEVEL", "DEBUG", ""},
			{"DB_HOST", "mysql-test.yunnian.internal", ""},
			{"DB_PASSWORD", "test_pwd_2026", "secret"},
			{"REDIS_ADDR", "redis-test.yunnian.internal:6379", ""},
		},
		ProjectKeys: []string{
			"user-web", "admin-portal", "mall-h5", "user-service", "order-service",
			"product-service", "gateway-service", "notify-service", "search-service", "auth-service",
		},
	},
	{
		Key: "staging", Name: "预发布区", Cluster: "k8s-staging-01", ClusterVer: "v1.29.6",
		Namespace: "yunnian-staging", DeployMethod: "helm", Domain: "*.staging.yunnian.cn",
		Owner: "李静", Description: "与生产配置对齐的预发环境，回归验证通过后方可上线",
		Health: "warning", CPU: 68, Mem: 79, Nodes: 4, Enabled: true,
		Vars: [][3]string{
			{"SPRING_PROFILES_ACTIVE", "staging", ""},
			{"LOG_LEVEL", "INFO", ""},
			{"DB_HOST", "mysql-staging.yunnian.internal", ""},
			{"DB_PASSWORD", "stg_pwd_2026", "secret"},
			{"REDIS_ADDR", "redis-staging.yunnian.internal:6379", ""},
			{"TRACE_SAMPLE_RATE", "0.5", ""},
		},
		ProjectKeys: []string{"user-web", "admin-portal", "user-service", "order-service", "gateway-service", "payment-service"},
	},
	{
		Key: "canary", Name: "灰度区", Cluster: "k8s-prod-01", ClusterVer: "v1.29.6",
		Namespace: "yunnian-canary", DeployMethod: "k8s", Domain: "*.canary.yunnian.cn",
		Owner: "运维示例", Description: "生产双活集群内的灰度命名空间，承接 5% 真实流量",
		Health: "healthy", CPU: 23, Mem: 31, Nodes: 3, Enabled: true,
		Vars: [][3]string{
			{"SPRING_PROFILES_ACTIVE", "canary", ""},
			{"GRAY_WEIGHT", "5", ""},
			{"DB_HOST", "mysql-prod.yunnian.internal", ""},
			{"DB_PASSWORD", "prd_pwd_2026", "secret"},
		},
		ProjectKeys: []string{"mall-h5", "order-service"},
	},
	{
		Key: "prod", Name: "正式区", Cluster: "k8s-prod-01（双可用区）", ClusterVer: "v1.29.6",
		Namespace: "yunnian-prod", DeployMethod: "helm", Domain: "*.yunnian.cn",
		Owner: "运维示例", Description: "生产环境，部署需运维审批，支持一键回滚",
		Approval: true, Approvers: []string{"运维示例", "林曦"},
		Health: "healthy", CPU: 51, Mem: 64, Nodes: 12, Enabled: true,
		Vars: [][3]string{
			{"SPRING_PROFILES_ACTIVE", "prod", ""},
			{"LOG_LEVEL", "WARN", ""},
			{"DB_HOST", "mysql-prod.yunnian.internal", ""},
			{"DB_PASSWORD", "prd_pwd_2026", "secret"},
			{"REDIS_ADDR", "redis-prod.yunnian.internal:6379", ""},
			{"SENTRY_DSN", "https://token@sentry.yunnian.cn/12", "secret"},
			{"HPA_ENABLED", "true", ""},
		},
		ProjectKeys: []string{
			"user-web", "admin-portal", "user-service", "order-service",
			"payment-service", "product-service", "gateway-service", "auth-service",
		},
	},
	{
		Key: "perf", Name: "压测区", Cluster: "k8s-test-01", ClusterVer: "v1.29.6",
		Namespace: "yunnian-perf", DeployMethod: "compose", Domain: "*.perf.yunnian.cn",
		Owner: "高鹏", Description: "性能压测专用环境，与测试区共享集群但资源隔离",
		Health: "error", CPU: 91, Mem: 88, Nodes: 2, Enabled: false,
		Vars: [][3]string{
			{"SPRING_PROFILES_ACTIVE", "perf", ""},
			{"DB_HOST", "mysql-perf.yunnian.internal", ""},
			{"MOCK_ENABLED", "true", ""},
		},
		ProjectKeys: []string{"order-service", "product-service", "report-service"},
	},
}

// Seed 写入初始数据。
//
// 幂等：以 user 表是否有数据为判据，已有数据直接跳过（不会覆盖用户改动）。
// 关闭方式：配置 database.seed=false。
func Seed(db *gorm.DB, cfg *config.Config) error {
	var count int64
	if err := db.Model(&model.User{}).Count(&count).Error; err != nil {
		return fmt.Errorf("统计用户失败: %w", err)
	}
	if count > 0 {
		slog.Info("种子数据已存在，跳过初始化", "users", count)
		return nil
	}

	// 全部种子用户共用同一个密码哈希：只算一次 bcrypt，避免启动时做 20+ 次哈希运算。
	// 生产环境务必给每个账号分配独立密码。
	hash, err := bcrypt.GenerateFromPassword([]byte(seedPassword), bcryptCost(cfg))
	if err != nil {
		return fmt.Errorf("生成初始密码失败: %w", err)
	}
	seeder := &seeder{hash: string(hash), now: time.Now()}

	steps := []struct {
		name string
		fn   func(*gorm.DB) error
	}{
		{"角色与权限", seeder.rbac},
		{"用户", seeder.users},
		{"项目", seeder.projects},
		{"环境", seeder.environments},
		{"构建与镜像", seeder.buildsAndImages},
		{"服务实例", seeder.services},
		{"部署记录", seeder.deploys},
		{"回滚记录", seeder.rollbacks},
		{"审计与系统配置", seeder.system},
	}

	for _, s := range steps {
		if err := db.Transaction(s.fn); err != nil {
			return fmt.Errorf("初始化「%s」失败: %w", s.name, err)
		}
		slog.Info("种子数据写入完成", "step", s.name)
	}

	slog.Info("种子数据初始化完成",
		"projects", len(projectSpecs), "environments", len(envSpecs), "demoUser", seedDemoUsername)
	return nil
}

func bcryptCost(cfg *config.Config) int {
	if cfg.Auth.BcryptCost < 10 || cfg.Auth.BcryptCost > 15 {
		return 10
	}
	return cfg.Auth.BcryptCost
}

// seeder 持有种子过程共享的上下文。
type seeder struct {
	hash        string
	now         time.Time
	projectIDs  map[string]uint64 // project_key → id
	envIDs      map[string]uint64 // env_key → id
	serviceIDs  map[string]uint64 // "envKey/projectKey" → service_instance.id
	imageIDs    map[string]uint64 // project_key → 最新镜像 id
	imageRefs   map[string]string // project_key → 最新镜像完整地址
	latestTag   map[string]string // project_key → 最新 tag
	userIDs     map[string]uint64 // name → user.id
	ownerIDs    []uint64          // 可作为负责人的用户 id 列表
	buildNoSeen map[string]uint32 // project_key → 当前最大构建号

	// rnd 是确定性随机源。固定种子保证每次重建数据库得到完全一样的数据，
	// 便于前端联调时比对与复现问题。
	rnd *rand.Rand
}

func (s *seeder) init() {
	if s.rnd == nil {
		s.rnd = rand.New(rand.NewSource(20260923))
	}
	if s.projectIDs == nil {
		s.projectIDs = map[string]uint64{}
		s.envIDs = map[string]uint64{}
		s.serviceIDs = map[string]uint64{}
		s.imageIDs = map[string]uint64{}
		s.imageRefs = map[string]string{}
		s.latestTag = map[string]string{}
		s.userIDs = map[string]uint64{}
		s.buildNoSeen = map[string]uint32{}
	}
}

// --------------------------------------------------------------------------- 角色与权限

// permSeed 权限点，逐条对应 docs/02 的 permission 初始化数据。
// permSeed 权限点目录。
//
// 这里**同时收录通配形态**（env:* / build:* / *:read）。
// 原因：role_permission 以 permission_id 做关联，"角色持有 env:*" 必须在目录里
// 有一条可关联的记录才能落库；只收精确权限点的话，
// 种子角色里写的通配全部会被判为"未定义的权限点"而拒绝启动。
// 通配的展开由 auth.Match 在鉴权时完成（见 auth 包注释），不在这里展开成几十条记录。
var permSeed = []struct{ Code, Name, Module string }{
	{"*", "全部权限", "system"},
	{"project:read", "查看项目", "project"},
	{"project:write", "维护项目", "project"},
	{"project:write:own", "维护负责项目", "project"},
	{"project:delete", "删除项目", "project"},
	{"build:read", "查看构建", "build"},
	{"build:trigger", "触发构建", "build"},
	{"build:cancel", "取消构建", "build"},
	{"build:*", "构建全部权限", "build"},
	{"image:read", "查看镜像", "image"},
	{"image:delete", "清理镜像", "image"},
	{"env:read", "查看环境", "environment"},
	{"env:write", "维护环境", "environment"},
	{"env:*", "环境全部权限", "environment"},
	{"deploy:read", "查看部署", "deploy"},
	{"deploy:test", "测试区部署", "deploy"},
	{"deploy:test,staging", "测试/预发部署", "deploy"},
	{"deploy:approve", "部署审批", "deploy"},
	{"deploy:*", "部署全部权限", "deploy"},
	{"rollback:read", "查看回滚", "rollback"},
	{"rollback:execute", "执行回滚", "rollback"},
	{"rollback:*", "回滚全部权限", "rollback"},
	{"log:read", "查看日志", "log"},
	{"audit:read", "查看审计", "audit"},
	{"user:manage", "用户管理", "system"},
	{"role:manage", "角色管理", "system"},
	{"notify:manage", "通知管理", "system"},
	{"ai:query", "AI 查询", "ai"},
	{"*:read", "全部只读权限", "system"},
}

// roleSeed 角色，以及各角色默认持有的权限点。
// 权限点里的通配（env:* / build:* / *:read）由 auth.Match 在鉴权时展开。
var roleSeed = []struct {
	Code, Name, Desc string
	Sort             int
	Perms            []string
}{
	{"admin", "超级管理员", "拥有全部权限，可管理用户、角色与系统配置", 1,
		[]string{"*"}},
	{"ops", "运维", "可管理环境、执行部署/回滚/扩缩容，审批正式区发布", 2,
		// ai:query 必须给：ops 能管部署、能看部署记录，而 CI 构建记录页与 CD 部署记录页的
		// AI 归因都挂在 ai:query 上（router.go 的 /:id/ai-diagnosis 与 /ai 分组）。
		// 缺了它，运维点自己刚失败的部署单做归因会直接 403，功能对最该用它的人不可见。
		[]string{"env:*", "deploy:*", "rollback:*", "project:read", "build:*", "image:read", "audit:read", "log:read", "ai:query"}},
	{"dev", "开发", "可维护自己负责的项目、触发构建，测试/预发环境部署", 3,
		[]string{"project:read", "project:write:own", "build:*", "deploy:test,staging", "image:read", "log:read"}},
	{"qa", "测试", "可触发测试区部署与重启，查看构建/部署日志", 4,
		[]string{"project:read", "build:read", "deploy:test", "log:read"}},
	{"readonly", "只读用户", "仅可查看所有资源，不能执行任何变更操作", 5,
		[]string{"*:read"}},
}

func (s *seeder) rbac(tx *gorm.DB) error {
	s.init()

	perms := make([]model.Permission, 0, len(permSeed))
	for _, p := range permSeed {
		perms = append(perms, model.Permission{Code: p.Code, Name: p.Name, Module: p.Module})
	}
	if err := tx.Create(&perms).Error; err != nil {
		return err
	}
	permID := make(map[string]uint64, len(perms))
	for _, p := range perms {
		permID[p.Code] = p.ID
	}

	for _, r := range roleSeed {
		role := model.Role{
			Code: r.Code, Name: r.Name, Description: r.Desc,
			Builtin: true, Sort: r.Sort,
		}
		if err := tx.Create(&role).Error; err != nil {
			return err
		}
		links := make([]model.RolePermission, 0, len(r.Perms))
		for _, code := range r.Perms {
			id, ok := permID[code]
			if !ok {
				return fmt.Errorf("角色 %s 引用了未定义的权限点 %s", r.Code, code)
			}
			links = append(links, model.RolePermission{RoleID: role.ID, PermissionID: id})
		}
		if err := tx.Create(&links).Error; err != nil {
			return err
		}
	}
	return nil
}

// --------------------------------------------------------------------------- 用户

func (s *seeder) users(tx *gorm.DB) error {
	type userSpec struct {
		Username, Name, Email, Phone, Role, Team string
		Enabled                                  bool
	}

	specs := []userSpec{
		{seedDemoUsername, "运维示例", "xufeng@yunnian.cn", "13800000001", "ops", "基础架构组", true},
		{"admin", "系统管理员", "admin@yunnian.cn", "13800000002", "admin", "基础架构组", true},
		{"guest", "访客账号", "guest@yunnian.cn", "", "readonly", "", true},
	}

	// 为 20 个项目的负责人各建一个开发账号，这样项目列表能显示真实姓名。
	// 运维示例已作为演示账号存在，直接复用，不重复创建。
	seen := map[string]bool{"运维示例": true}
	devIndex := 0
	for _, p := range projectSpecs {
		if seen[p.Owner] {
			continue
		}
		seen[p.Owner] = true
		devIndex++
		specs = append(specs, userSpec{
			Username: fmt.Sprintf("dev%02d", devIndex),
			Name:     p.Owner,
			Email:    fmt.Sprintf("dev%02d@yunnian.cn", devIndex),
			Role:     "dev",
			Team:     p.Team,
			Enabled:  true,
		})
	}

	roleIDs := map[string]uint64{}
	var roles []model.Role
	if err := tx.Find(&roles).Error; err != nil {
		return err
	}
	for _, r := range roles {
		roleIDs[r.Code] = r.ID
	}

	for _, sp := range specs {
		last := s.now.AddDate(0, 0, -1).Add(time.Duration(-devIndex*37) * time.Minute)
		u := model.User{
			Username:     sp.Username,
			PasswordHash: s.hash,
			Name:         sp.Name,
			Email:        sp.Email,
			Phone:        sp.Phone,
			Enabled:      sp.Enabled,
			LastLoginAt:  &last,
			LastLoginIP:  "10.20.31." + fmt.Sprint(20+devIndex%60),
		}
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		s.userIDs[sp.Name] = u.ID
		s.ownerIDs = append(s.ownerIDs, u.ID)

		rid, ok := roleIDs[sp.Role]
		if !ok {
			return fmt.Errorf("用户 %s 引用了未定义的角色 %s", sp.Username, sp.Role)
		}
		if err := tx.Create(&model.UserRole{UserID: u.ID, RoleID: rid}).Error; err != nil {
			return err
		}
	}
	return nil
}

// --------------------------------------------------------------------------- 项目

func (s *seeder) projects(tx *gorm.DB) error {
	s.init()

	for i, sp := range projectSpecs {
		ownerID := s.userIDs[sp.Owner]
		if ownerID == 0 {
			ownerID = s.ownerIDs[0]
		}
		p := model.Project{
			ProjectKey:     sp.Key,
			Name:           sp.Name,
			Type:           sp.Type,
			GitProvider:    "gitlab",
			GitURL:         fmt.Sprintf("git@%s:%s/%s.git", seedGitLab, sp.Type, sp.Key),
			DefaultBranch:  defaultBranchOf(sp.Key),
			BuildMethod:    sp.Method,
			DockerfilePath: "Dockerfile",
			BuildContext:   ".",
			Registry:       seedRegistry,
			ImageName:      fmt.Sprintf("%s/%s", sp.Type, sp.Key),
			TagPattern:     "{branch}-{commitId}-{timestamp}",
			BuildParams:    model.JSONText(buildParamsJSON(sp)),
			TimeoutSec:     1800,
			OwnerID:        ownerID,
			Team:           sp.Team,
			Description:    sp.Desc,
			Enabled:        sp.Enabled,
			AutoBuild:      sp.Key == "user-service" || sp.Key == "gateway-service",
			AutoBuildRefs:  model.JSONText(`["master","release/*"]`),
			CreatedAt:      s.now.Add(-time.Duration(120+i*36) * time.Hour),
			UpdatedAt:      s.now.Add(-time.Duration(2+i%48) * time.Hour),
		}
		if sp.Method == "jenkins" {
			p.JenkinsJob = "ci-" + sp.Key
		}
		if sp.Method == "script" {
			// 「自定义脚本」模式下构建流程整个由这段脚本定义，所以种子必须给它一份
			// **能跑通的**默认值：留空的话它会在构建时安静地跑完然后什么都不产出
			// （见 integration.ErrCodeScriptNoImage），而演示数据里出现一个
			// "看着配好了、一构建就没反应"的项目，比不生成它更糟。
			p.BuildScript = buildscript.DefaultScript(sp.Type)
		}
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		s.projectIDs[sp.Key] = p.ID

		// 项目成员：负责人 + 管理员
		members := []model.ProjectMember{
			{ProjectID: p.ID, UserID: ownerID, Role: "owner"},
		}
		if adminID, ok := s.userIDs["系统管理员"]; ok {
			members = append(members, model.ProjectMember{ProjectID: p.ID, UserID: adminID, Role: "maintainer"})
		}
		if err := tx.Create(&members).Error; err != nil {
			return err
		}
	}
	return nil
}

func defaultBranchOf(key string) string {
	if key == "docs-site" {
		return "main"
	}
	return "master"
}

// buildParamsJSON 生成构建参数，前端与后端使用同一套默认值。
func buildParamsJSON(sp projectSpec) string {
	if sp.Type == "frontend" {
		apiBase := "/api"
		if sp.Key == "data-board" {
			apiBase = "/gateway"
		}
		return fmt.Sprintf(
			`[{"key":"NPM_REGISTRY","value":"https://registry.npmmirror.com","desc":"npm 私服/镜像源"},`+
				`{"key":"NODE_ENV","value":"production","desc":"构建环境标识"},`+
				`{"key":"VITE_API_BASE","value":"%s","desc":"前端注入的接口前缀"},`+
				`{"key":"BUILD_MODE","value":"release","desc":"构建模式（release/debug）"}]`, apiBase)
	}
	return `[{"key":"MAVEN_SETTINGS","value":"/opt/ci/settings.xml","desc":"Maven settings 路径"},` +
		`{"key":"MVN_ARGS","value":"-DskipTests -U","desc":"Maven 构建参数"},` +
		`{"key":"JAVA_OPTS","value":"-Xms512m -Xmx2048m","desc":"JVM 参数"},` +
		`{"key":"SPRING_PROFILES","value":"dev","desc":"Spring 生效配置"},` +
		`{"key":"DINGTALK_TOKEN","value":"dt_****e9f2","desc":"构建通知钉钉机器人 Token","secret":true}]`
}
