package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yunnian/yunnian/internal/auth"
	"github.com/yunnian/yunnian/internal/buildscript"
	"github.com/yunnian/yunnian/internal/errs"
	"github.com/yunnian/yunnian/internal/httpx"
	"github.com/yunnian/yunnian/internal/integration"
	"github.com/yunnian/yunnian/internal/llm"
	"github.com/yunnian/yunnian/internal/logstream"
	"github.com/yunnian/yunnian/internal/model"
	"github.com/yunnian/yunnian/internal/queue"
)

// CIService 持续集成：项目配置、构建、镜像。
type CIService struct {
	*base
	// sys 复用系统服务的「默认模型通道」解析（含密钥解密、超时兜底与"未配置通道"的提示文案），
	// 与 AIService 同一个理由：多一份实现就多一个漏改点，换加密方案时必漏一处。
	sys *SystemService
}

// --------------------------------------------------------------------------- 入参

// ProjectQuery 项目列表查询。
type ProjectQuery struct {
	Keyword  string
	Type     string // frontend / backend / 空=全部
	Enabled  *bool
	Team     string
	Page     int
	PageSize int
	SortBy   string
	Order    string
}

// ProjectInput 项目新建/编辑入参。
type ProjectInput struct {
	Key         string `json:"key" binding:"omitempty,max=64"`
	Name        string `json:"name" binding:"required,max=128"`
	Type        string `json:"type" binding:"required,oneof=frontend backend"`
	GitProvider string `json:"gitProvider" binding:"omitempty,oneof=gitlab github gitee"`
	// GitURL 不是无条件必填：构建方式为 Jenkins Job 的项目由 Jenkins 自己管仓库，
	// 平台侧没有仓库地址是合法的。这里放宽到"可空"，真正的必填规则交给
	// validateProjectRepo 按构建方式判——把条件规则写在 binding tag 里做不到。
	GitURL        string `json:"gitUrl" binding:"omitempty,max=255"`
	DefaultBranch string `json:"defaultBranch" binding:"omitempty,max=96"`

	// GitUsername / GitPassword 这个项目拉代码用的账号密码。
	//
	// 与 `gitUrl` 一样按项目存，而不是只有一份"平台级 GitLab 通道"：
	// 不同仓库可能在不同的 GitLab 上、用不同的机器人账号，平台级一份配置
	// 会把它们绑死在同一套凭据上（而且那个通道关掉之后，所有项目都变成匿名检出）。
	//
	// ⚠️ GitPassword **只在入参里出现**，落库走 secret_credential 加密，
	// 出参一律不回显（留空 = 不修改，见 UpdateProject）。
	GitUsername string `json:"gitUsername" binding:"omitempty,max=128"`
	GitPassword string `json:"gitPassword" binding:"omitempty,max=512"`

	BuildMethod    string `json:"buildMethod" binding:"omitempty,oneof=dockerfile jenkins script"`
	JenkinsJob     string `json:"jenkinsJob" binding:"omitempty,max=128"`
	DockerfilePath string `json:"dockerfilePath" binding:"omitempty,max=255"`
	BuildContext   string `json:"buildContext" binding:"omitempty,max=255"`

	// ---------------------------------------------------------- 自建构建（buildMethod=script）
	//
	// BuildScript 是「自定义脚本」模式下那段 shell 的正文，构建流程整个由它定义。
	// 它只在 buildMethod=script 时有意义；其余模式下存着不读（界面上也不展示），
	// 这样用户切换构建方式时原来写的内容不会被清掉。
	//
	// 上限给得很宽（64KB）：它是一份可执行的构建定义，不是备注。
	BuildScript string `json:"buildScript" binding:"omitempty,max=65536"`

	Registry   string `json:"registry" binding:"omitempty,max=128"`
	ImageName  string `json:"imageName" binding:"omitempty,max=128"`
	TagPattern string `json:"tagPattern" binding:"omitempty,max=128"`

	BuildParams []BuildParamDTO `json:"buildParams"`
	Owner       string          `json:"owner"`
	Team        string          `json:"team" binding:"omitempty,max=64"`
	Description string          `json:"description" binding:"omitempty,max=512"`
	Enabled     *bool           `json:"enabled"`
}

// validateBuildConfig 校验「构建方式」与「构建配置」是否自洽。
//
// 与 validateProjectRepo 分开放，因为它们回答两个不同的问题：
// 前者问"仓库由谁管"，这里问"这套构建配置到底能不能跑起来"。
//
// 只有一条规则：自定义脚本不能为空。
//
// ⚠️ 为什么空脚本必须在**保存时**就拦：一段空脚本执行起来是"退出码 0、什么也没发生"，
// 构建记录会显示成功、日志一片空白、镜像列表里没有新条目。用户要等到点了构建、
// 翻了日志才反应过来，而那时他已经以为项目建好了 —— 存下一个"看着配好了、
// 一跑就 silently 什么都没发生"的项目，比当场拒收糟糕得多。
//
// dockerfile 模式不在这里校验：Dockerfile 路径有默认值，真正缺文件要到构建时才知道
// （平台不该去读用户仓库里的文件来"提前"判断）。
func validateBuildConfig(in ProjectInput, method string) error {
	if method != "script" {
		return nil
	}
	if strings.TrimSpace(in.BuildScript) == "" {
		return errs.Invalid("「自定义脚本」不能为空：构建流程完全由它定义（拉代码、编译、打包、推送）")
	}
	return nil
}

// validateProjectRepo 校验「仓库地址」与「构建方式」是否自洽。
//
// 两条规则都来自同一个事实：**仓库由谁管**。
//   - Dockerfile / 自定义脚本：平台自己去拉代码构建，没仓库地址就永远构建失败。
//     放行等于让用户存下一个"看起来配好了、一构建就炸"的项目。
//   - Jenkins Job：仓库、构建参数、镜像地址全在 Job 里，平台侧不读这份配置，
//     所以仓库地址可空；但必须绑一个 Job，否则平台不知道去触发谁。
//
// 写成函数是为了新建与编辑走同一套判断——只改一处，不会两边不一致。
func validateProjectRepo(gitURL, buildMethod, jenkinsJob string) error {
	if buildMethod == "jenkins" {
		if strings.TrimSpace(jenkinsJob) == "" {
			return errs.Invalid("构建方式为 Jenkins Job 时必须绑定一个 Jenkins Job")
		}
	} else if gitURL == "" {
		return errs.Invalid("构建方式为 Dockerfile / 自定义脚本时必须填写仓库地址")
	}
	if gitURL != "" && !strings.HasPrefix(gitURL, "http") && !strings.Contains(gitURL, "@") {
		return errs.Invalid("仓库地址需为 http(s) 或 git@host:group/repo 形式")
	}
	return nil
}

// TriggerBuildInput 触发构建入参。
type TriggerBuildInput struct {
	ProjectKey string            `json:"projectKey" binding:"required"`
	Branch     string            `json:"branch" binding:"omitempty,max=96"`
	Params     map[string]string `json:"params"`
	// IdempotencyKey 由前端生成（同一次点击复用同一个），防重复提交。
	IdempotencyKey string `json:"idempotencyKey" binding:"omitempty,max=128"`

	// 以下字段供 Webhook / 定时任务使用：此时"要构建哪个提交"由事件给出，
	// 不能再去问代码托管"分支最新提交是什么"——那可能已经是下一个提交了，
	// 构建出来的镜像会与这次推送对不上（排查时是灾难）。
	CommitID  string `json:"commitId" binding:"omitempty,max=40"`
	CommitMsg string `json:"commitMsg" binding:"omitempty,max=512"`
	Author    string `json:"author" binding:"omitempty,max=64"`
	// TriggerType manual / webhook / schedule / retry，留空按 manual。
	TriggerType string `json:"triggerType" binding:"omitempty,oneof=manual webhook schedule retry"`
	// TriggerName 触发者展示名，留空则用调用方姓名。
	TriggerName string `json:"triggerName" binding:"omitempty,max=64"`
}

// HandlePushInput 一次代码推送事件。
type HandlePushInput struct {
	Provider  string // gitlab / github
	GitURL    string
	Branch    string
	CommitID  string
	CommitMsg string
	Author    string
}

// PushOutcome 推送处理结果。
//
// 明确区分「没匹配到项目」「匹配到但没开自动构建」「已触发」三种结局，
// 而不是统一返回 200 {}。Webhook 出错时唯一能看的就是响应体，
// 含混的响应会让联调变成猜谜。
type PushOutcome struct {
	Matched    bool      `json:"matched"`
	ProjectKey string    `json:"projectKey,omitempty"`
	Triggered  bool      `json:"triggered"`
	Reason     string    `json:"reason"`
	Build      *BuildDTO `json:"build,omitempty"`
}

// HandlePush 处理代码托管平台的 push 回调。
func (s *CIService) HandlePush(ctx context.Context, in HandlePushInput) (*PushOutcome, error) {
	branch := strings.TrimSpace(in.Branch)
	gitURL := strings.TrimSpace(in.GitURL)
	if gitURL == "" || branch == "" {
		return nil, errs.Invalid("推送事件缺少仓库地址或分支")
	}

	// 按规范化后的仓库地址匹配项目。远端给的可能是 https://host/a/b.git、
	// http://host/a/b、或带末尾斜杠的形态，必须归一化后再比。
	var candidates []model.Project
	if err := s.db(ctx).Where("deleted_at IS NULL").Find(&candidates).Error; err != nil {
		return nil, errs.Internalf(err, "查询项目失败")
	}
	var matched *model.Project
	want := normalizeGitURL(gitURL)
	for i := range candidates {
		if normalizeGitURL(candidates[i].GitURL) == want {
			matched = &candidates[i]
			break
		}
	}
	if matched == nil {
		return &PushOutcome{Matched: false, Reason: fmt.Sprintf("没有项目关联仓库 %s，已忽略", gitURL)}, nil
	}
	if !matched.Enabled {
		return &PushOutcome{Matched: true, ProjectKey: matched.ProjectKey, Reason: "项目已停用，跳过自动构建"}, nil
	}
	if !matched.AutoBuild {
		return &PushOutcome{Matched: true, ProjectKey: matched.ProjectKey, Reason: "项目未开启自动构建"}, nil
	}
	if !matchAutoBuildRef(matched, branch) {
		return &PushOutcome{
			Matched: true, ProjectKey: matched.ProjectKey,
			Reason: fmt.Sprintf("分支 %s 不在自动构建范围内，跳过", branch),
		}, nil
	}
	if !matchAllowedBranch(s.deps.Cfg.Integration.Webhook.AllowedBranches, branch) {
		return &PushOutcome{
			Matched: true, ProjectKey: matched.ProjectKey,
			Reason: fmt.Sprintf("分支 %s 不在全局允许列表内，跳过", branch),
		}, nil
	}

	provider := stringOr(strings.TrimSpace(in.Provider), "gitlab")
	// Webhook 没有真实登录用户，但审计与构建记录里必须能看出
	// "这次构建是 gitlab 推送触发的"。用一个合成身份表达，
	// 好过在审计里留下一行"匿名"——那等于这条记录白记。
	webhookCaller := Caller{User: &auth.CurrentUser{
		Username: provider + "-webhook",
		Name:     provider + " 推送",
		RoleCode: "system",
	}}

	build, err := s.TriggerBuild(ctx, TriggerBuildInput{
		ProjectKey:  matched.ProjectKey,
		Branch:      branch,
		CommitID:    in.CommitID,
		CommitMsg:   in.CommitMsg,
		Author:      in.Author,
		TriggerType: "webhook",
		TriggerName: provider + "-webhook",
		// 幂等键带上 commit：同一提交因重发/重试多次到达时只建一个构建；
		// 换成新提交（新 SHA）就是另一次构建，符合预期。
		IdempotencyKey: fmt.Sprintf("push:%s:%s:%s", matched.ProjectKey, branch, in.CommitID),
	}, webhookCaller)
	if err != nil {
		return &PushOutcome{Matched: true, ProjectKey: matched.ProjectKey, Reason: "触发构建失败: " + err.Error()}, err
	}
	return &PushOutcome{Matched: true, ProjectKey: matched.ProjectKey, Triggered: true, Reason: "已触发自动构建", Build: build}, nil
}

// normalizeGitURL 归一化仓库地址：去掉协议、末尾斜杠与 .git 后缀，统一小写。
//
// 只保留 host/path 部分做比较，而不做精确字符串比对：
// 同一仓库在不同来源下写法五花八门（http/https、带不带 .git、大小写），
// 精确比对会让 90% 的推送匹配不上项目，而这类问题只会表现为"webhook 没反应"。
func normalizeGitURL(raw string) string {
	v := strings.TrimSpace(strings.ToLower(raw))
	v = strings.TrimSuffix(v, "/")
	v = strings.TrimSuffix(v, ".git")
	for _, p := range []string{"https://", "http://", "ssh://", "git://", "git@"} {
		v = strings.TrimPrefix(v, p)
	}
	// git@host:group/repo 形态里的冒号等价于斜杠
	v = strings.Replace(v, ":", "/", 1)
	return strings.TrimSuffix(v, "/")
}

// matchAutoBuildRef 判断分支是否命中项目的自动构建范围。
//
// 支持 releases/* 这类通配；范围为空时视为"所有分支都自动构建"——
// 因为 autoBuild=true 本身已经是明确意图，再要求必须填范围会让用户配不出能用的规则。
func matchAutoBuildRef(p *model.Project, branch string) bool {
	refs := decodeStringList(p.AutoBuildRefs)
	if len(refs) == 0 {
		return true
	}
	for _, ref := range refs {
		if matchRefPattern(ref, branch) {
			return true
		}
	}
	return false
}

// matchRefPattern 单条分支规则匹配，支持末尾 * 通配。
func matchRefPattern(pattern, branch string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(branch, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == branch
}

// matchAllowedBranch 全局分支白名单；列表为空表示不限。
func matchAllowedBranch(allowed []string, branch string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, ref := range allowed {
		if matchRefPattern(ref, branch) {
			return true
		}
	}
	return false
}

// BuildQuery 构建列表查询。
type BuildQuery struct {
	ProjectKey string
	// ProjectType 按项目类型（frontend/backend）过滤。
	// 构建记录表本身没有类型字段，靠 project.type 子查询关联——
	// 前端列表页的「前端/后端」分段筛选需要它，否则只能把该筛选项删掉。
	ProjectType string
	// Status 支持逗号分隔的多值（如 `running,queued`），单值同样是合法输入。
	// 列表页的「进行中」面板要同时看这两种状态，多值能省掉一次请求。
	Status   string
	Branch   string
	Keyword  string
	Page     int
	PageSize int
	SortBy   string
	Order    string
}

// ImageQuery 镜像列表查询。
type ImageQuery struct {
	ProjectKey string
	Env        string
	Keyword    string
	Page       int
	PageSize   int
	SortBy     string
	Order      string
}

// --------------------------------------------------------------------------- 项目

// 排序列白名单：sortBy 无法参数化，必须用白名单映射到真实列名，否则就是注入点。
var projectSortColumns = map[string]string{
	"name":      "name",
	"key":       "project_key",
	"createdAt": "created_at",
	"updatedAt": "updated_at",
}

// ListProjects 项目列表（含最近构建与统计）。
func (s *CIService) ListProjects(ctx context.Context, q ProjectQuery) (*httpx.PageQuery, any, int64, error) {
	pq := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize, SortBy: q.SortBy, Order: q.Order, Keyword: q.Keyword}
	column, desc := pq.Normalize(projectSortColumns, "updated_at")

	db := s.db(ctx).Model(&model.Project{})
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("name LIKE ? OR project_key LIKE ? OR description LIKE ?", like, like, like)
	}
	if t := strings.TrimSpace(q.Type); t != "" {
		db = db.Where("type = ?", t)
	}
	if q.Enabled != nil {
		db = db.Where("enabled = ?", *q.Enabled)
	}
	if team := strings.TrimSpace(q.Team); team != "" {
		db = db.Where("team = ?", team)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "统计项目数量失败")
	}

	var projects []model.Project
	if err := db.Order(column + " " + orderWord(desc)).
		Offset(pq.Offset()).Limit(pq.PageSize).Find(&projects).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "查询项目失败")
	}
	if len(projects) == 0 {
		return &pq, []ProjectDTO{}, total, nil
	}

	ids := make([]uint64, 0, len(projects))
	keys := make([]string, 0, len(projects))
	for i := range projects {
		ids = append(ids, projects[i].ID)
		keys = append(keys, projects[i].ProjectKey)
	}

	ownerNames, err := s.userNames(ctx, projectOwnerIDs(projects))
	if err != nil {
		return nil, nil, 0, err
	}
	stats, err := s.projectStats(ctx, keys)
	if err != nil {
		return nil, nil, 0, err
	}
	lastBuilds, err := s.lastBuilds(ctx, keys)
	if err != nil {
		return nil, nil, 0, err
	}
	buildImages, err := s.imagesOfBuilds(ctx, buildIDsOf(lastBuilds))
	if err != nil {
		return nil, nil, 0, err
	}
	byKey := indexProjects(projects)

	out := make([]ProjectDTO, 0, len(projects))
	for i := range projects {
		p := &projects[i]
		st := stats[p.ProjectKey]
		dto := projectToDTO(p, ownerNames[p.OwnerID], st, nil)
		if b, ok := lastBuilds[p.ProjectKey]; ok {
			bd := buildDTO(&b, byKey[p.ProjectKey], buildImages[b.ID], nil)
			dto.LastBuild = &bd
		}
		out = append(out, dto)
	}
	return &pq, out, total, nil
}

// GetProject 项目详情。
func (s *CIService) GetProject(ctx context.Context, key string) (*ProjectDTO, error) {
	p, err := s.projectByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	ownerNames, err := s.userNames(ctx, []uint64{p.OwnerID})
	if err != nil {
		return nil, err
	}
	stats, err := s.projectStats(ctx, []string{p.ProjectKey})
	if err != nil {
		return nil, err
	}
	lastBuilds, err := s.lastBuilds(ctx, []string{p.ProjectKey})
	if err != nil {
		return nil, err
	}
	buildImages, err := s.imagesOfBuilds(ctx, buildIDsOf(lastBuilds))
	if err != nil {
		return nil, err
	}
	dto := projectToDTO(p, ownerNames[p.OwnerID], stats[p.ProjectKey], nil)
	if b, ok := lastBuilds[p.ProjectKey]; ok {
		bd := buildDTO(&b, p, buildImages[b.ID], nil)
		dto.LastBuild = &bd
	}
	return &dto, nil
}

// CreateProject 新建项目。
func (s *CIService) CreateProject(ctx context.Context, in ProjectInput, c Caller) (*ProjectDTO, error) {
	key := strings.TrimSpace(in.Key)
	if key == "" {
		key = slugify(in.Name)
	}
	if !isValidKey(key) {
		return nil, errs.Invalidf("项目标识 %q 不合法：只允许小写字母、数字与中划线，且以字母开头", key)
	}

	gitURL := strings.TrimSpace(in.GitURL)
	buildMethod := stringOr(in.BuildMethod, "dockerfile")
	if err := validateProjectRepo(gitURL, buildMethod, in.JenkinsJob); err != nil {
		return nil, err
	}
	if err := validateBuildConfig(in, buildMethod); err != nil {
		return nil, err
	}

	ownerID, err := s.resolveOwner(ctx, in.Owner, c)
	if err != nil {
		return nil, err
	}

	params, err := encodeBuildParams(in.BuildParams)
	if err != nil {
		return nil, err
	}

	// Registry 是"镜像地址前缀"，不是下游实现名：
	// 默认取配置里的 harbor 地址，避免把 "local" 这种实现名写进镜像地址。
	registry := stringOr(strings.TrimSpace(in.Registry), s.defaultRegistry())
	imageName := stringOr(in.ImageName, defaultImageName(in.Type, key))

	p := model.Project{
		ProjectKey:     key,
		Name:           strings.TrimSpace(in.Name),
		Type:           in.Type,
		GitProvider:    stringOr(in.GitProvider, "gitlab"),
		GitURL:         gitURL,
		GitUsername:    strings.TrimSpace(in.GitUsername),
		DefaultBranch:  stringOr(in.DefaultBranch, "master"),
		BuildMethod:    buildMethod,
		JenkinsJob:     in.JenkinsJob,
		DockerfilePath: stringOr(in.DockerfilePath, "Dockerfile"),
		BuildContext:   stringOr(in.BuildContext, "."),

		// 自定义脚本：没填就按项目类型给一份默认（后端 Java / 前端 Node）。
		// 给默认而不是留空，是因为这个字段的语义是"构建流程是什么"，留空等于
		// 存了一个跑不出任何东西的项目；而默认脚本本身是能跑通的最小完整例子。
		BuildScript: stringOr(strings.TrimSpace(in.BuildScript), buildscript.DefaultScript(in.Type)),

		Registry:      registry,
		ImageName:     imageName,
		TagPattern:    stringOr(in.TagPattern, buildscript.DefaultTagPattern),
		BuildParams:   params,
		TimeoutSec:    1800,
		OwnerID:       ownerID,
		Team:          in.Team,
		Description:   in.Description,
		Enabled:       in.Enabled == nil || *in.Enabled,
		AutoBuild:     false,
		AutoBuildRefs: model.JSONText(`["master"]`),
		CreatedBy:     c.UserID(),
		UpdatedBy:     c.UserID(),
	}

	// 建行与"捞回软删行"放同一个事务：复活是一次 Unscoped 更新，
	// 中途失败要么全成要么全不成，不留半成品。
	err = s.db(ctx).Transaction(func(tx *gorm.DB) error {
		if cerr := tx.Create(&p).Error; cerr != nil {
			if !isDuplicate(cerr) {
				return errs.Internalf(cerr, "创建项目失败")
			}
			if rerr := s.reviveProject(tx, &p); rerr != nil {
				return rerr
			}
		}
		// 凭据与项目行放**同一个事务**：分开写会出现"项目建好了、密码没存上"，
		// 表现为首次构建报 401 —— 而用户刚刚才点过「测试连接成功」，这会让他
		// 去怀疑凭据本身，而不是怀疑平台没存。宁可整个创建失败让他重来。
		return s.bindProjectGitCredential(tx, &p, strings.TrimSpace(in.GitPassword), c.UserID())
	})
	if err != nil {
		return nil, err
	}

	// 建项目的人默认成为项目成员，避免"自己建的项目自己没权限"。
	// 用 FirstOrCreate：走"捞回软删行"这条路时，那条成员记录**还在**
	// （删项目只软删 project 一行，成员行不动），再插一次会撞 `uk_pm`，
	// 虽然后面能建出来，但会平白打一条 WARN 日志。
	if c.User != nil {
		if err := s.db(ctx).Where(model.ProjectMember{ProjectID: p.ID, UserID: c.User.ID}).
			FirstOrCreate(&model.ProjectMember{ProjectID: p.ID, UserID: c.User.ID, Role: "owner"}).Error; err != nil {
			s.log("ci").Warn("写入项目成员失败", "projectId", p.ID, "err", err)
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "project:create", TargetType: "project",
		TargetID: &p.ID, TargetName: p.Name,
		Detail: fmt.Sprintf("新建项目 %s（%s）", p.ProjectKey, p.Name),
	})

	ownerNames, _ := s.userNames(ctx, []uint64{p.OwnerID})
	dto := projectToDTO(&p, ownerNames[p.OwnerID], ProjectStats{}, nil)
	return &dto, nil
}

// UpdateProject 编辑项目。
func (s *CIService) UpdateProject(ctx context.Context, key string, in ProjectInput, c Caller) (*ProjectDTO, error) {
	p, err := s.projectByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	// 负责人变更需要额外权限（相当于把项目交出去），只有运维/管理员能做。
	if in.Owner != "" {
		newOwner, err := s.resolveOwner(ctx, in.Owner, c)
		if err != nil {
			return nil, err
		}
		if newOwner != p.OwnerID && !c.User.HasAnyPerm("project:write", "*") {
			return nil, errs.Forbidden("变更项目负责人需要 project:write 权限")
		}
		p.OwnerID = newOwner
	}

	params, err := encodeBuildParams(in.BuildParams)
	if err != nil {
		return nil, err
	}

	// 校验用的是**改完之后**的构建方式（没传就沿用原值）：把构建方式从 Jenkins
	// 改成 Dockerfile、同时又不填仓库地址，是必然构建失败的一种组合，必须在这里拦。
	buildMethod := stringOr(in.BuildMethod, p.BuildMethod)
	if err := validateProjectRepo(strings.TrimSpace(in.GitURL), buildMethod, in.JenkinsJob); err != nil {
		return nil, err
	}
	// 构建配置的校验必须用**合并后**的值：精简表单可能不提交某几格，
	// 拿空值去校验会把"没改这一项"误判成"把它清空了"，用户保存就会莫名被拒。
	// 这与下面 updates 里 `stringOr(x, p.X)` 的兜底口径保持一致。
	merged := in
	merged.BuildScript = stringOr(in.BuildScript, p.BuildScript)
	if err := validateBuildConfig(merged, buildMethod); err != nil {
		return nil, err
	}

	updates := map[string]any{
		"name":            strings.TrimSpace(in.Name),
		"type":            in.Type,
		"git_provider":    stringOr(in.GitProvider, p.GitProvider),
		"git_url":         strings.TrimSpace(in.GitURL),
		"git_username":    strings.TrimSpace(in.GitUsername),
		"default_branch":  stringOr(in.DefaultBranch, p.DefaultBranch),
		"build_method":    buildMethod,
		"jenkins_job":     in.JenkinsJob,
		"dockerfile_path": stringOr(in.DockerfilePath, p.DockerfilePath),
		"build_context":   stringOr(in.BuildContext, p.BuildContext),
		// 空值保留原值：精简表单不渲染脚本这一格时传空是常态，
		// 直接覆盖会把用户写好的脚本清掉。
		"build_script": stringOr(in.BuildScript, p.BuildScript),
		"registry":     stringOr(in.Registry, p.Registry),
		"image_name":   stringOr(in.ImageName, p.ImageName),
		"tag_pattern":  stringOr(in.TagPattern, p.TagPattern),
		"build_params": params,
		"team":         in.Team,
		"description":  in.Description,
		"owner_id":     p.OwnerID,
		"updated_by":   c.UserID(),
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}

	// 配置更新与凭据更新放同一个事务：理由与新建时相同（不能出现"看着改好了、
	// 密码还是旧的"）。密码留空 = 不修改 —— 界面上看不到原文、回传不了，
	// 把空串当"清空"会让"只改了个负责人"顺手把密码删掉。
	err = s.db(ctx).Transaction(func(tx *gorm.DB) error {
		if uerr := tx.Model(&model.Project{}).Where("id = ?", p.ID).Updates(updates).Error; uerr != nil {
			return errs.Internalf(uerr, "更新项目失败")
		}
		p.GitURL = updates["git_url"].(string)
		p.GitUsername = updates["git_username"].(string)
		return s.bindProjectGitCredential(tx, p, strings.TrimSpace(in.GitPassword), c.UserID())
	})
	if err != nil {
		return nil, err
	}

	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "project:update", TargetType: "project",
		TargetID: &p.ID, TargetName: p.Name,
		Detail: fmt.Sprintf("编辑项目 %s（%s）", p.ProjectKey, p.Name),
	})
	return s.GetProject(ctx, key)
}

// SetProjectEnabled 启用/停用项目。
func (s *CIService) SetProjectEnabled(ctx context.Context, key string, enabled bool, c Caller) error {
	p, err := s.projectByKey(ctx, key)
	if err != nil {
		return err
	}
	if err := s.db(ctx).Model(&model.Project{}).Where("id = ?", p.ID).
		Updates(map[string]any{"enabled": enabled, "updated_by": c.UserID()}).Error; err != nil {
		return errs.Internalf(err, "更新项目状态失败")
	}
	action := "项目已停用"
	if enabled {
		action = "项目已启用"
	}
	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "project:enabled", TargetType: "project",
		TargetID: &p.ID, TargetName: p.Name,
		Detail: fmt.Sprintf("%s：%s", action, p.ProjectKey),
	})
	return nil
}

// DeleteProject 删除项目（软删）。
//
// 有部署中的服务时拒绝：项目一删，正在跑的部署单就失去了操作对象，
// 状态机无法收敛，只能靠人工介入。宁可让用户先去处理掉。
func (s *CIService) DeleteProject(ctx context.Context, key string, c Caller) error {
	p, err := s.projectByKey(ctx, key)
	if err != nil {
		return err
	}
	var active int64
	if err := s.db(ctx).Model(&model.ServiceInstance{}).
		Where("project_id = ? AND status IN ?", p.ID, []string{"deploying", "pending"}).
		Count(&active).Error; err != nil {
		return errs.Internalf(err, "检查服务状态失败")
	}
	if active > 0 {
		return errs.StateConflict(fmt.Sprintf("该项目在 %d 个环境中有部署进行中，请先处理后再删除", active))
	}

	if err := s.db(ctx).Delete(&model.Project{}, p.ID).Error; err != nil {
		return errs.Internalf(err, "删除项目失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "project:delete", TargetType: "project",
		TargetID: &p.ID, TargetName: p.Name,
		Detail: fmt.Sprintf("删除项目 %s（%s）", p.ProjectKey, p.Name),
	})
	return nil
}

// reviveProject 捞回同标识的软删行复活。
//
// `uk_project_key` 只含 `project_key`、**不含 `deleted_at`**，所以列表里看不到的行
// 仍占着标识：用户删掉项目后用同一个标识重建，会撞唯一索引，原样翻译出去就是
// `40900 项目标识已被占用：xxx`。用户 2026-09-28 实测撞过两次（重建 demoitms），
// 当时的运维解法是物理删掉那条软删行 —— 那不该是正解。
//
// 这里复用那一行而不是让用户换个标识：界面说"已删除"，用户不该为此改设计。
// 与 `image_record`（已有 Unscoped 捞回）、`environment`（`reviveEnvironment`）同一口径。
//
// 注意**只复活、不搬历史**：`image_record` / `deploy_record` 里挂着旧 project_id 的行
// 因此重新可见。这是刻意的 —— 用户重建的就是同一个项目，把历史留在原地比
// 让它们跟着消失更符合预期（旧代码物理删库反而是把历史留成了孤儿行）。
func (s *CIService) reviveProject(tx *gorm.DB, p *model.Project) error {
	var old model.Project
	if err := tx.Unscoped().Where("project_key = ?", p.ProjectKey).Order("id DESC").First(&old).Error; err != nil {
		return errs.Conflict("项目标识已被占用：" + p.ProjectKey)
	}
	// **必须判它是不是软删的**。`Unscoped` 把软删与活跃行一起捞出来，
	// 而唯一索引冲突也可能来自一个**活跃**的同名项目（真并发插入，或同一请求重放）。
	// 少了这一判，我们会把别人正在用的项目**原地改写成自己的配置**，
	// 还回一句"项目已创建" —— 这比报冲突危险得多（实测就是这条被端到端验证抓到的）。
	if !old.DeletedAt.Valid {
		return errs.Conflict("项目标识已被占用：" + p.ProjectKey)
	}
	updates := map[string]any{
		"deleted_at":      nil,
		"name":            p.Name,
		"type":            p.Type,
		"git_provider":    p.GitProvider,
		"git_url":         p.GitURL,
		"default_branch":  p.DefaultBranch,
		"build_method":    p.BuildMethod,
		"jenkins_job":     p.JenkinsJob,
		"dockerfile_path": p.DockerfilePath,
		"build_context":   p.BuildContext,
		"build_script":    p.BuildScript,
		"registry":        p.Registry,
		"image_name":      p.ImageName,
		"tag_pattern":     p.TagPattern,
		"build_params":    p.BuildParams,
		"timeout_sec":     p.TimeoutSec,
		"owner_id":        p.OwnerID,
		"team":            p.Team,
		"description":     p.Description,
		"enabled":         p.Enabled,
		"auto_build":      p.AutoBuild,
		// 列名是自定义的（`auto_build_branches`），不是字段名的蛇形 —— 写错这里
		// 会静默丢字段，所以显式写出来而不是靠 GORM 推。
		"auto_build_branches": p.AutoBuildRefs,
		"updated_by":          p.UpdatedBy,
		"updated_at":          now(),
	}
	if err := tx.Unscoped().Model(&model.Project{}).Where("id = ?", old.ID).Updates(updates).Error; err != nil {
		return errs.Internalf(err, "复用已删除的项目失败")
	}
	p.ID = old.ID
	return nil
}

// ProjectBranches 拉取远端分支列表。
//
// ⚠️ 走 git 协议**真读**（`git ls-remote` + 项目自己的凭据），不走 `Repository.Branches`。
// 后者在未接 GitLab 时会落到 LocalRepository 上，而它从前返回写死的
// {master, develop, release/2.6, release/2.5, hotfix/login} —— 那五个分支根本不存在。
// 结果是"下拉里选得出、构建时 clone 失败"，而报错指向仓库，真因是平台给了一个假分支名。
func (s *CIService) ProjectBranches(ctx context.Context, key string) ([]string, error) {
	p, err := s.projectByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.GitURL) == "" {
		// 仓库由 Jenkins Job 自己管的项目，平台侧没有可读的对象。
		// 硬跑一次探测必然失败，而那句失败会被读成"项目配置有问题"。
		return []string{}, nil
	}
	return s.probeGitBranches(ctx, p)
}

// probeGitBranches 用项目自己的凭据真读一次远端分支。
//
// 抽成一处，是因为两个调用方（项目表单的分支下拉、项目自检）**必须给出同一个答案**：
// 界面说"读到 4 个分支"、自检却说"凭据不通"，是同一套凭据拿到了两个结论，
// 用户只会更困惑 —— 而且他没法判断该信哪个。
//
// 与「测试连接」、与构建同源：三条路都是 git 协议 + 项目凭据（理由见 git_probe.go 文件头）。
func (s *CIService) probeGitBranches(ctx context.Context, p *model.Project) ([]string, error) {
	user, pass, credErr := s.openProjectGitCredential(ctx, p)
	if credErr != nil {
		return nil, errs.Internalf(credErr, "读取项目仓库凭据失败")
	}

	probeCtx, cancel := context.WithTimeout(ctx, gitProbeTimeout)
	defer cancel()

	probe := s.deps.Up.ProbeGit(probeCtx, integration.RepositoryProbe{
		URL:      p.GitURL,
		Username: user,
		Password: pass,
	})
	if !probe.OK {
		return nil, upstreamError(nil, "读取分支列表失败：%s", probe.Message)
	}

	out := probe.Branches
	if out == nil {
		// 一律给数组：前端拿到 null 会因为 `.map` 直接崩掉整页。
		out = []string{}
	}
	if len(out) > branchesLimit {
		out = out[:branchesLimit]
	}
	return out, nil
}

// ValidateResult 项目校验结果。
type ValidateResult struct {
	Repository   bool     `json:"repository"`
	Credential   bool     `json:"credential"`
	Dockerfile   bool     `json:"dockerfile"`
	Registry     bool     `json:"registry"`
	Branches     []string `json:"branches"`
	Message      string   `json:"message"`
	FailedChecks []string `json:"failedChecks"`
}

// ValidateProject 校验仓库连通性、Dockerfile 与镜像仓库可用性。
//
// 这是"保存前先自检"，避免用户配错了到构建时才发现。
// 三项检查相互独立，全跑完再汇总——不做短路，这样用户一次就能看到所有问题。
func (s *CIService) ValidateProject(ctx context.Context, key string, c Caller) (*ValidateResult, error) {
	p, err := s.projectByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	res := &ValidateResult{Branches: []string{}, FailedChecks: []string{}}
	// repoProbeErr 保存仓库探测的原文，最后拼进 Message ——
	// FailedChecks 里的 repository / credential 是给前端做标记用的机器码，
	// 不能让它成为用户看到的全部结论。
	var repoProbeErr string

	// Jenkins 项目可能根本没有仓库地址（「新建 Jenkins Job 项目」建出来的就是这种形态），
	// 因为仓库由那个 Job 自己管，平台侧没有可校验的对象。硬跑一次"拉分支"必然失败，
	// 而那句失败会被读成"项目配置有问题"——如实跳过，并在结论里说明跳过了什么。
	repoManagedByJenkins := p.BuildMethod == "jenkins" && strings.TrimSpace(p.GitURL) == ""

	if repoManagedByJenkins {
		res.Repository = true
		res.Credential = true
	} else {
		// 一次探测同时回答"仓库通不通"和"凭据对不对"。
		//
		// 从前这里是 `Repository.Ping` + `Repository.Branches` 两步 —— 那条路走
		// GitLab REST API + **平台级令牌**，而构建用的是 git 协议 + **项目自己的凭据**。
		// 用平台令牌测通了、构建时照样 401，正是这条不一致造成的假承诺。
		branches, perr := s.probeGitBranches(ctx, p)
		if perr != nil {
			// 真因必须带出去：只说"repository、credential 未通过"，用户无法区分
			// 是地址写错了、密码不对，还是这台机器到不了那台机器 —— 处置完全不同。
			res.FailedChecks = append(res.FailedChecks, "repository", "credential")
			// ⚠️ 取 .Message，不能取 .Error()。
			//
			// `*errs.Error.Error()` 的格式是 `code=50200 人话: 底层原因` ——
			// 那个 `code=` 前缀是**给日志看的**（见 errs.Error 的实现）。塞进
			// res.Message 会被前端原样渲染，用户看到的是一句
			// "以下检查未通过：repository、credential；code=50200 读取分支列表失败：…"。
			// 实测踩过一次。
			repoProbeErr = errs.From(perr).Message
		} else {
			res.Repository = true
			res.Credential = true
			res.Branches = branches
		}
	}

	// Dockerfile 检查：本地实现下无法真的读仓库文件，
	// 只能校验"路径已配置且是 Dockerfile 构建方式"，如实说明而不是假报成功。
	if p.BuildMethod == "dockerfile" {
		res.Dockerfile = strings.TrimSpace(p.DockerfilePath) != ""
	} else {
		res.Dockerfile = true
	}
	if !res.Dockerfile {
		res.FailedChecks = append(res.FailedChecks, "dockerfile")
	}

	if err := s.deps.Up.Registry.Ping(ctx); err != nil {
		res.FailedChecks = append(res.FailedChecks, "registry")
	} else {
		res.Registry = true
	}

	if len(res.FailedChecks) == 0 {
		res.Message = "校验通过"
		if repoManagedByJenkins {
			res.Message = "校验通过（仓库由 Jenkins Job 管理，未检查仓库连通性）"
		}
	} else {
		res.Message = "以下检查未通过：" + strings.Join(res.FailedChecks, "、")
		if repoProbeErr != "" {
			res.Message += "；" + repoProbeErr
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "project:validate", TargetType: "project",
		TargetID: &p.ID, TargetName: p.Name,
		Detail: "校验项目配置：" + res.Message,
		Result: resultWord(len(res.FailedChecks) == 0),
	})
	return res, nil
}

// --------------------------------------------------------------------------- 构建

// ListBuilds 全局构建列表。
func (s *CIService) ListBuilds(ctx context.Context, q BuildQuery) (*httpx.PageQuery, any, int64, error) {
	pq := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize, SortBy: q.SortBy, Order: q.Order, Keyword: q.Keyword}
	column, desc := pq.Normalize(buildSortColumns, "created_at")

	db := s.db(ctx).Model(&model.BuildRecord{})
	if k := strings.TrimSpace(q.ProjectKey); k != "" {
		db = db.Where("project_key = ?", k)
	}
	if tp := strings.TrimSpace(q.ProjectType); tp != "" {
		db = db.Where("project_key IN (SELECT project_key FROM project WHERE type = ?)", tp)
	}
	if st := strings.TrimSpace(q.Status); st != "" {
		if vals := splitCSV(st); len(vals) > 0 {
			db = db.Where("status IN ?", vals)
		}
	}
	if br := strings.TrimSpace(q.Branch); br != "" {
		db = db.Where("branch = ?", br)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("project_key LIKE ? OR commit_msg LIKE ? OR commit_id LIKE ?", like, like, like)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "统计构建数量失败")
	}
	var records []model.BuildRecord
	if err := db.Order(column + " " + orderWord(desc)).
		Offset(pq.Offset()).Limit(pq.PageSize).Find(&records).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "查询构建记录失败")
	}
	// 列表也要带镜像信息：构建列表的核心信息就是「这次构建产出了哪个镜像」，
	// 用户要靠它挑版本去部署。代价只是 imagesOfBuilds 里一条 `build_id IN (...)`
	// 的批量查询（与页大小无关，不是 N+1），所以不值得为省这一次查询把该列留空。
	return &pq, s.decorateBuilds(ctx, records, true), total, nil
}

var buildSortColumns = map[string]string{
	"buildNo":   "build_no",
	"createdAt": "created_at",
	"duration":  "duration_sec",
	"status":    "status",
}

// ListProjectBuilds 某项目的构建历史。
func (s *CIService) ListProjectBuilds(ctx context.Context, key string, q BuildQuery) (*httpx.PageQuery, any, int64, error) {
	if _, err := s.projectByKey(ctx, key); err != nil {
		return nil, nil, 0, err
	}
	q.ProjectKey = key
	return s.ListBuilds(ctx, q)
}

// GetBuild 构建详情（含日志尾巴，便于前端直接渲染而不必再请求一次）。
func (s *CIService) GetBuild(ctx context.Context, id uint64) (*BuildDTO, error) {
	var rec model.BuildRecord
	if err := firstOrNotFound(s.db(ctx), id, &rec, "构建记录"); err != nil {
		return nil, err
	}
	logs, err := s.buildLogLines(ctx, rec.ID, 0, 200)
	if err != nil {
		return nil, err
	}
	decorated := s.decorateBuilds(ctx, []model.BuildRecord{rec}, true)
	dto := decorated[0]
	dto.Logs = logs
	return &dto, nil
}

// BuildLogs 分页拉取全量日志。
func (s *CIService) BuildLogs(ctx context.Context, id uint64, afterSeq uint32, limit int) ([]map[string]any, error) {
	var rec model.BuildRecord
	if err := firstOrNotFound(s.db(ctx), id, &rec, "构建记录"); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 2000 {
		limit = 1000
	}
	var rows []model.BuildLog
	if err := s.db(ctx).Where("build_id = ? AND seq > ?", rec.ID, afterSeq).
		Order("seq asc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询构建日志失败")
	}
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		out = append(out, map[string]any{
			"seq":   rows[i].Seq,
			"level": rows[i].Level,
			"line":  rows[i].Content,
			"ts":    formatTime(rows[i].LoggedAt),
		})
	}
	return out, nil
}

// BuildLogEvents 读取已落库的构建日志并转成日志流事件，供 SSE 订阅时补发历史。
//
// 与 BuildLogs 的区别：那个给的是 HTTP 响应体（map），这个是给日志流用的内部形状。
// 之所以要单独一个方法而不是让 handler 自己查：日志表的字段名（content / logged_at）
// 与流事件的字段名（line / at）不一样，映射关系应该只存在于一处，
// 否则以后改列名要同时改 handler 和 service。
func (s *CIService) BuildLogEvents(ctx context.Context, buildID uint64, afterSeq uint32, limit int) []logstream.Event {
	// 上限只作为兜底：调用方（SSE 补发）自己会传一个合理的值。
	// 这里再夹一次是防止将来有人拿它当分页接口用，一次把几万行读进内存。
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	var rows []model.BuildLog
	if err := s.db(ctx).Where("build_id = ? AND seq > ?", buildID, afterSeq).
		Order("seq asc").Limit(limit).Find(&rows).Error; err != nil {
		// 补发失败不该让整个流挂掉：实时部分仍然有价值，用户至少能看到后续输出。
		s.log("ci").WarnContext(ctx, "补发构建历史日志失败", "buildId", buildID, "err", err)
		return nil
	}
	out := make([]logstream.Event, 0, len(rows))
	for i := range rows {
		out = append(out, logstream.Event{
			Seq:   rows[i].Seq,
			Kind:  logstream.KindLog,
			Level: rows[i].Level,
			Line:  rows[i].Content,
			At:    rows[i].LoggedAt,
		})
	}
	return out
}

// buildLogLines 取日志文本（仅正文），供详情页内嵌。
func (s *CIService) buildLogLines(ctx context.Context, buildID uint64, afterSeq uint32, limit int) ([]string, error) {
	var rows []model.BuildLog
	if err := s.db(ctx).Where("build_id = ? AND seq > ?", buildID, afterSeq).
		Order("seq asc").Limit(limit).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询构建日志失败")
	}
	out := make([]string, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].Content)
	}
	return out, nil
}

// BuildLogStreamKey 返回构建日志流的键，供 handler 订阅 SSE 使用。
func BuildLogStreamKey(buildID uint64) string { return logstream.Key("build", buildID) }

// TriggerBuild 一键编译。
//
// 流程（每一步失败都要让状态可收敛，不能留下"永远 running"的记录）：
//
//	建记录(queued) → 投池 → [池内] running → 逐行日志 → 成功建镜像 / 失败写归因 → 终态 + 关流
//
// 记录先建后投池，是为了让前端能立刻拿到 buildId 去订阅 SSE；
// 若先投池再建记录，会出现"日志已经在推但你还不知道去订阅哪个 id"的竞态。
func (s *CIService) TriggerBuild(ctx context.Context, in TriggerBuildInput, c Caller) (*BuildDTO, error) {
	p, err := s.projectByKey(ctx, in.ProjectKey)
	if err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, errs.StateConflict("项目已停用，无法触发构建")
	}

	// Jenkins 项目**不走平台构建链路**。
	//
	// 少了这道校验，平台会拿"刚从 Jenkins 版本标签下拉里选出来的值"当自己的分支名，
	// 去代码托管查一个提交、再跑一次平台的 docker 构建，最后落一条与 Jenkins
	// 毫无关系的"成功"记录——这种错看起来完全正常，是最难被发现的一类。
	// 前端的两条入口（项目编译弹窗）都已改走 Jenkins 触发接口，这里是兜底：
	// 批量编译向导、外部调用、以后新增的入口都会撞上它。
	if p.BuildMethod == "jenkins" {
		return nil, errs.StateConflict(fmt.Sprintf(
			"项目 %s 的构建方式为 Jenkins Job（%s），平台不会代为执行 docker 构建；"+
				"请到「项目管理」的项目编译里选择构建分支，由平台触发 Jenkins 执行",
			p.ProjectKey, stringOr(p.JenkinsJob, "未选择 Job")))
	}

	branch := stringOr(strings.TrimSpace(in.Branch), p.DefaultBranch)

	// 幂等：同一个 idempotencyKey 重复提交直接返回已有记录，
	// 避免用户连点两次产生两个构建（既浪费 Runner 也会打乱 buildNo 序列）。
	if k := strings.TrimSpace(in.IdempotencyKey); k != "" {
		var exist model.BuildRecord
		err := s.db(ctx).Where("idempotency_key = ?", k).First(&exist).Error
		if err == nil {
			return s.GetBuild(ctx, exist.ID)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.Internalf(err, "查询幂等记录失败")
		}
	}

	// Webhook / 定时任务已经把"要构建哪个提交"带来了，就不要再问远端。
	var commit integration.Commit
	if id := strings.TrimSpace(in.CommitID); id != "" {
		commit = integration.Commit{
			ID:      id,
			Message: strings.TrimSpace(in.CommitMsg),
			Author:  strings.TrimSpace(in.Author),
		}
	} else {
		// 真读：`git ls-remote` 取该分支当前指向的 sha，用**项目自己的凭据**。
		//
		// 从前这里调 `Repository.LatestCommit`，未接 GitLab 时会落到 LocalRepository ——
		// 它编一个 sha 出来（前缀、长度都像真的）。两条后果：写进 build_record.commit_id
		// 之后**洗不掉**；交给构建器 `git fetch origin <sha>` 又必然报 `couldn't find remote ref`。
		//
		// 拿不到就**不发单**：构建本来也要 clone 代码，读不到提交号意味着构建必然失败，
		// 提前失败比发一条注定失败的构建单更省事。commit message / author 留空，不编。
		user, pass, credErr := s.openProjectGitCredential(ctx, p)
		if credErr != nil {
			return nil, errs.Internalf(credErr, "读取项目仓库凭据失败")
		}
		resolveCtx, cancel := context.WithTimeout(ctx, gitProbeTimeout)
		latest, lerr := s.deps.Up.ResolveCommit(resolveCtx, integration.RepositoryProbe{
			URL:      p.GitURL,
			Username: user,
			Password: pass,
			Branch:   branch,
		})
		cancel()
		if lerr != nil {
			// 把下游真因一并带进 message：只给一句"解析提交失败"，用户分不清
			// 是分支名写错了、凭据不对、还是网络不通 —— 这三件事的处置完全不同。
			return nil, upstreamError(lerr, "解析 %s 分支的提交失败：%s", branch, lerr.Error())
		}
		if latest == nil {
			return nil, upstreamError(nil, "代码托管未返回 %s 分支的提交信息", branch)
		}
		commit = *latest
	}

	params, err := s.mergeBuildParams(p, in.Params)
	if err != nil {
		return nil, err
	}

	buildNo, err := s.nextBuildNo(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	nowT := now()
	tag := renderTag(p.TagPattern, branch, commit.ID, int64(buildNo), nowT)
	image := buildImageRef(p.Registry, p.ImageName, tag)

	rec := model.BuildRecord{
		BuildNo:        buildNo,
		ProjectID:      p.ID,
		ProjectKey:     p.ProjectKey,
		Branch:         branch,
		CommitID:       commit.ID,
		CommitMsg:      commit.Message,
		TriggerType:    stringOr(strings.TrimSpace(in.TriggerType), "manual"),
		TriggerName:    stringOr(strings.TrimSpace(in.TriggerName), c.Name()),
		ParamsSnapshot: params,
		Status:         "queued",
		Stage:          "排队中",
		Progress:       0,
		CreatedAt:      nowT,
		UpdatedAt:      nowT,
	}
	if k := strings.TrimSpace(in.IdempotencyKey); k != "" {
		rec.IdempotencyKey = &k
	}
	// 触发人只在有真实登录用户时写：Webhook 场景下 UserID 是合成的 0，
	// 写进去会让"谁触发的"这个问题得到一个无意义的答案。
	if uid := c.UserID(); uid != nil && *uid > 0 {
		rec.TriggerBy = uid
	}
	if err := s.db(ctx).Create(&rec).Error; err != nil {
		if isDuplicate(err) {
			return nil, errs.Internalf(err, "构建记录写入冲突，请重试")
		}
		return nil, errs.Internalf(err, "创建构建记录失败")
	}

	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "build:trigger", TargetType: "build",
		TargetID: &rec.ID, TargetName: fmt.Sprintf("%s#%d", p.ProjectKey, rec.BuildNo),
		Detail: fmt.Sprintf("触发构建：%s / %s（%s）", p.ProjectKey, branch, commit.ID[:min(7, len(commit.ID))]),
	})

	// 日志流先重置：同一个 buildID 不会重复执行，但幂等重试的场景下可能残留。
	streamKey := BuildLogStreamKey(rec.ID)
	s.deps.Stream.Reset(ctx, streamKey)

	// commit.ID 现在一定是**真的**：要么来自 webhook 携带的推送提交号，
	// 要么来自上面 `git ls-remote` 的实时解析，构建器可以放心去 fetch。
	//
	// 从前这里还有一层"本地模拟就置空"的兜底，那是为 LocalRepository 编出来的
	// 假提交号准备的 —— 编造来源已退役（它现在如实报错），兜底随之删除。
	commitID := commit.ID

	// 检出凭据**按项目取**（project.git_credential_id → secret_credential）。
	//
	// 以前是从平台级 GitLab 通道取的，那条路有两个问题：一是关掉通道之后所有项目
	// 一起退化成匿名检出（私有仓库报 401，而报错指向"仓库地址不对"）；
	// 二是"这个项目用哪个账号拉的代码"变成一个查不到的问题。
	// 取不到凭据时**匿名检出**：公开仓库本来就不需要，把它当失败是错的。
	gitUser, gitPass, credErr := s.openProjectGitCredential(ctx, p)
	if credErr != nil {
		// 凭据**存在但解不开**（记录丢失 / 主密钥换过）必须当场失败。
		// 悄悄退回匿名，用户看到的会是"拉取代码失败"，而根因是凭据没了。
		return nil, errs.Internalf(credErr, "读取项目仓库凭据失败")
	}

	req := integration.BuildRequest{
		BuildID:     rec.ID,
		BuildNo:     int64(rec.BuildNo),
		ProjectKey:  p.ProjectKey,
		GitURL:      p.GitURL,
		Branch:      branch,
		CommitID:    commitID,
		Method:      p.BuildMethod,
		Dockerfile:  p.DockerfilePath,
		Context:     p.BuildContext,
		BuildScript: p.BuildScript,
		Registry:    p.Registry,
		ImageName:   p.ImageName,
		Tag:         tag,
		Image:       image,
		Params:      decodeParamsMap(params),
		Timeout:     time.Duration(p.TimeoutSec) * time.Second,

		// 目标平台必须显式给出：构建机是 arm64 还是 x86_64，和"镜像要跑在哪"是两件事。
		// 靠猜的后果是构建出架构不对的镜像，而这个错要等容器起不来才发现 ——
		// 那时坏镜像已经推到仓库里了。
		Platform: s.deps.Cfg.Runner.BuildPlatform,

		GitUsername: gitUser,
		GitPassword: gitPass,
	}

	pos, err := s.deps.Builds.Submit(rec.ID, func(taskCtx context.Context) {
		s.runBuild(taskCtx, rec.ID, p.ProjectKey, image, req)
	})
	if err != nil {
		// 投池失败必须把记录落到失败态，否则会留下永远 queued 的僵尸记录。
		failMsg := "任务提交失败"
		switch {
		case errors.Is(err, queue.ErrDuplicate):
			failMsg = "该构建已在队列中"
		case errors.Is(err, queue.ErrQueueFull):
			failMsg = "构建队列已满，请稍后重试"
		}
		fin := now()
		_ = s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", rec.ID).
			Updates(map[string]any{"status": "failed", "stage": "提交失败", "error_code": "QUEUE_REJECTED",
				"error_message": failMsg, "finished_at": &fin, "updated_at": fin}).Error
		s.deps.Stream.Finish(ctx, streamKey, "failed")
		if errors.Is(err, queue.ErrQueueFull) {
			return nil, errs.RateLimited(failMsg)
		}
		return nil, errs.StateConflict(failMsg)
	}

	if pos > 0 {
		// 排队中：把位置推给前端，让用户知道"前面还有几个"。
		_ = s.deps.Stream.SetStatus(ctx, streamKey, logstream.Status{
			Status: "queued", Progress: 0, Stage: fmt.Sprintf("排队中（前面还有 %d 个任务）", pos-1),
		})
	}

	return s.GetBuild(ctx, rec.ID)
}

// ---------------------------------------------------------------------------
// 一次构建的三段式：起态 / 执行 / 收终态
//
// 为什么要拆：inline 与 agent 两种执行方式下，**日志与结果来自完全不同的地方** ——
// inline 来自本进程里这个 goroutine 的返回值，agent 来自远端机器的 HTTP 上报。
// 但"构建结束之后要做什么"必须**完全一致**：写哪个状态、什么时机登记镜像记录、
// 用哪个错误码、怎么收口日志流、什么时候发失败通知。拆开之后 inline 只负责"跑"，
// 收尾统一走 finalizeBuild —— 两条路径不存在第二份"成功/失败怎么写"的实现。
// 两份实现的分叉只会在"换成 agent 跑"的那天才显形，是最难查的一类。
// ---------------------------------------------------------------------------

const (
	// defaultBuildTimeout 项目没配超时时长时的兜底。
	defaultBuildTimeout = 30 * time.Minute

	// buildFinishGrace 收尾（写终态 / 落日志 / 封流）自己的时间预算，**从收尾这一刻起算**。
	//
	// ⚠️ 既不能复用构建自己的 ctx，也不能在任务开头建好：
	//   - 构建因**取消 / 超时**结束时，任务 ctx 已经 Done，用它写库会一行都写不进去
	//     （实测：`DB.WithContext(已取消的 ctx)` 的 UPDATE 直接返回 context canceled，
	//     记录留在 status=running、finished_at=NULL，日志里只有一条容易略过的 WARN）；
	//   - 预算若从任务开头起算，构建只要跑得比预算久，走到收尾时它早已过期。
	// 两种写法的表现完全一样：**日志停在最后一行、状态卡在"构建中"，且不报任何错**。
	//
	// Jenkins 侧（`jenkinsFinishGrace`）与部署侧（`failDeploy` 里的 `WithoutCancel`）
	// 都已按这个口径修过，构建这条是最后一处。agent 模式下这条更不是可选项：
	// 结果上报是一次 HTTP 请求，它的 ctx 在响应写完就结束了，沿用请求 ctx
	// 等于**每一次收尾都失败**，而不是偶尔失败。
	buildFinishGrace = 20 * time.Second

	// 起态的两个值只在这里定义一次：日志收集器的进度单调性要拿它做起点，
	// 写成两份字面量迟早会出现"起点与写库值不一致 → 第一次真实阶段跃迁被判成后退"。
	buildStagePreparing    = "准备中"
	buildProgressPreparing = uint8(2)
)

// buildRun 一次构建的执行上下文：跑的是什么、什么时候开始的、日志收到哪里去。
type buildRun struct {
	// ctx 与触发这次构建的请求 / 任务**解绑**（`WithoutCancel`，且不带 deadline）：
	// 它只用于日志收集与收尾，不受"取消 / 超时"影响。
	// 收尾写库借的就是它 —— 见 finalizeBuild。
	ctx        context.Context
	buildID    uint64
	projectKey string
	image      string
	req        integration.BuildRequest
	timeout    time.Duration
	startedAt  time.Time
	sink       *buildSink
}

// newBuildRun 装配一次构建。base 通常是任务池给的 taskCtx。
func (s *CIService) newBuildRun(base context.Context, buildID uint64, projectKey, image string,
	req integration.BuildRequest) *buildRun {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultBuildTimeout
	}
	ctx := context.WithoutCancel(base)
	return &buildRun{
		ctx:        ctx,
		buildID:    buildID,
		projectKey: projectKey,
		image:      image,
		req:        req,
		timeout:    timeout,
		sink:       s.newBuildSink(ctx, buildID, projectKey),
	}
}

// buildSink 一次构建的日志与进度收集器。
//
// 三件事绑在一个东西上，少一件就会在某条路径上出现"推了流没落库"或"落了库没推流"：
//  1. 推流（SSE 实时看日志）；
//  2. 攒一份落库（含上限保护，终态之后写，供事后查阅）；
//  3. 从日志内容推导阶段与进度（只许前进）。
type buildSink struct {
	svc        *CIService
	ctx        context.Context
	log        *slog.Logger
	buildID    uint64
	projectKey string
	streamKey  string

	maxBytes  int64
	collected []logLineRecord
	logBytes  int64

	lastStage string
	lastPct   uint8
}

// newBuildSink 建收集器。ctx 必须是**与触发方解绑**的那个（见 buildRun.ctx）：
// agent 模式下日志由多次 HTTP 上报推进，用请求 ctx 会在第一个请求结束时全体失效。
func (s *CIService) newBuildSink(ctx context.Context, buildID uint64, projectKey string) *buildSink {
	return &buildSink{
		svc:        s,
		ctx:        ctx,
		log:        s.log("ci.build").With("buildId", buildID, "project", projectKey),
		buildID:    buildID,
		projectKey: projectKey,
		streamKey:  BuildLogStreamKey(buildID),
		maxBytes:   s.deps.Cfg.Runner.MaxLogBytes,
		// 起点必须等于 beginBuild 写进去的那两个值，否则第一次真实阶段跃迁会被
		// 单调性判定当成"后退"而丢掉（进度条第一次动弹就没了）。
		lastStage: buildStagePreparing,
		lastPct:   buildProgressPreparing,
	}
}

// Append 收下一行构建日志。签名与 integration.Builder 的 sink 参数一致，
// 因此可以直接把它当回调用（agent 模式下它由日志上报接口调用）。
func (k *buildSink) Append(level, line string) error {
	at := now()
	if _, err := k.svc.deps.Stream.Append(k.ctx, k.streamKey, level, line, at); err != nil &&
		!errors.Is(err, logstream.ErrClosed) {
		k.log.Warn("推送日志失败", "err", err)
	}
	if k.logBytes < k.maxBytes {
		k.collected = append(k.collected, logLineRecord{level: level, line: line, at: at})
		k.logBytes += int64(len(line))
	}
	k.advance(line, at)
	return nil
}

// advance 从日志行里识别阶段跃迁并推进进度。
func (k *buildSink) advance(line string, at time.Time) {
	st := stageOfLine(line)
	if st == "" || st == k.lastStage {
		return
	}
	pct := progressOfStage(st)
	k.lastStage = st
	// 进度只许前进，不许后退。
	//
	// 多阶段构建会把**编译过程的输出原样打在 docker build 的日志里**：
	// 于是"构建镜像(78%)"之后紧跟着一行 Maven 的 `Downloading ...` 会被
	// stageOfLine 判成"安装依赖(40%)"，进度条当场往回跳一大截。
	// 判定规则是启发式的，就不可能保证单调 —— 所以单调性必须由**状态更新**
	// 这一层来兜底，而不是指望判定规则不出错。
	if pct <= k.lastPct {
		return
	}
	k.lastPct = pct
	if err := k.svc.db(k.ctx).Model(&model.BuildRecord{}).Where("id = ?", k.buildID).
		Updates(map[string]any{"stage": st, "progress": pct, "updated_at": at}).Error; err != nil {
		k.log.Warn("更新构建阶段失败", "err", err)
	}
	_ = k.svc.deps.Stream.SetStatus(k.ctx, k.streamKey,
		logstream.Status{Status: "running", Progress: pct, Stage: st})
}

// runBuild 任务池入口（inline 模式）：装配 → 起态 → 跑 → 收尾。
func (s *CIService) runBuild(taskCtx context.Context, buildID uint64, projectKey, image string,
	req integration.BuildRequest) {
	run := s.newBuildRun(taskCtx, buildID, projectKey, image, req)

	// 执行体受构建自己的超时与取消约束 —— 这正是它该受的约束。
	// 收尾**不受**它约束：见 buildFinishGrace。
	execCtx, cancel := context.WithTimeout(taskCtx, run.timeout)
	defer cancel()

	s.startInlineBuild(execCtx, run)
}

// startInlineBuild 在平台进程内跑一次构建。
//
// agent 模式下平台不做这一步（远端机器领单后自己跑），但**收终态一定走 finalizeBuild**。
func (s *CIService) startInlineBuild(execCtx context.Context, run *buildRun) {
	if err := s.beginBuild(run); err != nil {
		s.log("ci.build").Error("更新构建状态失败", "buildId", run.buildID, "err", err)
		return
	}
	result, buildErr := s.deps.Up.Builder.Build(execCtx, run.req, run.sink.Append)
	s.finalizeBuild(run, result, buildErr)
}

// beginBuild 把构建推进到 running。
//
// 起态写不进去就往下跑没有意义：后面每一次阶段更新都会同样失败，最后只留下一条
// 永远 queued 的记录，而那次构建其实已经跑完（跑得越久，浪费越大）。
func (s *CIService) beginBuild(run *buildRun) error {
	at := now()
	run.startedAt = at
	if err := s.db(run.ctx).Model(&model.BuildRecord{}).Where("id = ?", run.buildID).
		Updates(map[string]any{
			"status": "running", "stage": buildStagePreparing, "progress": buildProgressPreparing,
			"started_at": &at, "updated_at": at,
		}).Error; err != nil {
		return err
	}
	_ = s.deps.Stream.SetStatus(run.ctx, run.sink.streamKey, logstream.Status{
		Status: "running", Progress: buildProgressPreparing, Stage: buildStagePreparing,
	})
	return nil
}

// finalizeBuild 写终态。**inline 与 agent 两条路径共用这一份。**
//
// ⚠️ 它开**自己的** ctx：与构建上下文解绑、时间预算从这一刻起算（见 buildFinishGrace）。
// 构建被取消 / 超时时，构建自己的 ctx 已经 Done，拿它写库、落日志、封流会**三件事
// 全部静默失败**，记录永远停在"构建中"。这一点本文件原先是错的，探针实测过
// （取消与超时各一条用例，见 ci_build_finalize_test.go）。
func (s *CIService) finalizeBuild(run *buildRun, result *integration.BuildResult, buildErr error) {
	ctx, cancel := context.WithTimeout(run.ctx, buildFinishGrace)
	defer cancel()

	log := run.sink.log
	streamKey := run.sink.streamKey
	fin := now()
	duration := uint32(fin.Sub(run.startedAt).Seconds())

	// 取消与超时要区分开：前端展示与后续处理都不同（取消是用户意图，超时是故障）。
	switch {
	case errors.Is(buildErr, context.Canceled):
		s.finishBuild(ctx, run.buildID, "canceled", "已取消", 0, duration, "", "", nil, fin)
		_ = s.deps.Stream.Finish(ctx, streamKey, "canceled")
		s.persistBuildLogs(ctx, run.buildID, run.sink.collected)
		log.Info("构建已取消")
		return
	case errors.Is(buildErr, context.DeadlineExceeded):
		s.finishBuild(ctx, run.buildID, "failed", "超时", 0, duration, "TIMEOUT", "", nil, fin)
		// 走 sink 而不是直接推流：**这句是超时唯一的解释**，必须和其余日志一起落库
		// （日志流是进程内 + TTL 的，重启后只剩一个 TIMEOUT 机器码，没人说得清是超了多少）。
		_ = run.sink.Append("error", fmt.Sprintf("构建超时（超过 %s），已中止", run.timeout))
		_ = s.deps.Stream.Finish(ctx, streamKey, "timeout")
		s.persistBuildLogs(ctx, run.buildID, run.sink.collected)
		log.Warn("构建超时")
		return
	}

	if buildErr != nil {
		code := "BUILD_ERROR"
		var bf *integration.BuildFailure
		if errors.As(buildErr, &bf) {
			code = bf.Code
		}
		_ = run.sink.Append("error", buildErr.Error())
		s.finishBuild(ctx, run.buildID, "failed", "已失败", 0, duration, code, buildErr.Error(),
			diagnoseBuild(code, run.projectKey), fin)
		_ = s.deps.Stream.Finish(ctx, streamKey, "failed")
		s.persistBuildLogs(ctx, run.buildID, run.sink.collected)
		log.Warn("构建失败", "code", code, "err", buildErr)
		return
	}

	if result != nil && result.Digest != "" {
		if err := s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", run.buildID).
			Update("log_size", run.sink.logBytes).Error; err != nil {
			log.Warn("更新日志大小失败", "err", err)
		}
	}

	imageRec, err := s.recordImage(ctx, run.buildID, run.projectKey, run.image, run.req, result, run.startedAt)
	if err != nil {
		log.Error("写入镜像记录失败", "err", err)
		s.finishBuild(ctx, run.buildID, "failed", "已失败", 100, duration, "IMAGE_RECORD_FAILED", err.Error(), nil, fin)
		_ = s.deps.Stream.Finish(ctx, streamKey, "failed")
		s.persistBuildLogs(ctx, run.buildID, run.sink.collected)
		return
	}

	s.finishBuild(ctx, run.buildID, "success", "已完成", 100, duration, "", "", nil, fin)
	if imageRec != nil {
		if err := s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", run.buildID).
			Update("image_record_id", imageRec.ID).Error; err != nil {
			log.Warn("回写镜像记录失败", "err", err)
		}
	}
	_, _ = s.deps.Stream.Append(ctx, streamKey, "info", "构建完成", fin)
	_ = s.deps.Stream.Finish(ctx, streamKey, "finished")
	s.persistBuildLogs(ctx, run.buildID, run.sink.collected)
	log.Info("构建成功", "image", run.image)
}

// finishBuild 统一写终态。集中一处，避免各分支漏字段导致状态不一致。
func (s *CIService) finishBuild(ctx context.Context, buildID uint64, status, stage string, progress uint8,
	duration uint32, errCode, errMsg string, ai *AIDiagnosisDTO, at time.Time) {
	updates := map[string]any{
		"status": status, "stage": stage, "progress": progress,
		"duration_sec": duration, "finished_at": &at, "updated_at": at,
	}
	if errCode != "" {
		updates["error_code"] = errCode
	}
	// 失败真因必须落库。以前 errMsg 只出现在下面那条 WARN 日志里，而日志流是进程内 + TTL 的，
	// 平台一重启这条构建就只剩一个机器码（如 NPM_INSTALL），没人说得清当时为什么失败。
	// 只在非成功终态写：成功时 errMsg 可能是"构建完成"这类正常文案，写进去会被误读成异常。
	if errMsg != "" && status != "success" {
		updates["error_message"] = clipRunes(errMsg, 512)
	}
	if ai != nil {
		if raw, err := json.Marshal(ai); err == nil {
			updates["ai_diagnosis"] = model.JSONText(raw)
		}
	}
	if err := s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", buildID).Updates(updates).Error; err != nil {
		s.log("ci.build").Error("写构建终态失败", "buildId", buildID, "err", err)
	}
	if errMsg != "" && status != "success" {
		s.log("ci.build").Warn("构建未成功", "buildId", buildID, "status", status, "code", errCode, "reason", errMsg)
	}
	// 失败了要主动告诉人，而不是让发起者靠刷新页面才发现。
	if status == "failed" {
		s.notifyBuildFailed(ctx, buildID)
	}
}

// notifyBuildFailed 构建失败时推送通知。
//
// 单独回查一次记录，而不是给 finishBuild 加参数：它的调用点有好几处，
// 为一条只在失败时走的通知去改所有调用方不划算；而终态路径多一次主键查询可以忽略。
func (s *CIService) notifyBuildFailed(ctx context.Context, buildID uint64) {
	var rec model.BuildRecord
	if err := s.db(ctx).First(&rec, buildID).Error; err != nil {
		s.log("ci.build").Warn("读取构建记录用于通知失败", "buildId", buildID, "err", err)
		return
	}
	s.notify.Dispatch(ctx, s.notify.BuildFailedMessage(&rec))
}

// recordImage 构建成功后登记镜像版本。
func (s *CIService) recordImage(ctx context.Context, buildID uint64, projectKey, image string,
	req integration.BuildRequest, result *integration.BuildResult, pushedAt time.Time) (*model.ImageRecord, error) {
	var rec model.BuildRecord
	if err := s.db(ctx).First(&rec, buildID).Error; err != nil {
		return nil, err
	}
	registry, repo, tag := splitImageRef(image)

	// size / digest / arch **只写真的读到的值**。
	//
	// 原来这里 arch 写死 "linux/amd64"、size 拿不到就写 0 —— 两者都是"看着像真的"：
	//   - arch 恰好对了，只是因为目标确实是 amd64。项目换成 arm64 时会显示错的，
	//     而它看起来一如既往地正常；
	//   - size=0 在界面上渲染成 "0 B"，把"没读到"伪装成"镜像大小是 0"。
	// 一个恒为常数的列就是一句无源的承诺，它比留空难查得多。读不到就留空，
	// 由 Harbor 回读补齐，补不上界面显示 "—"。
	size := (*uint64)(nil)
	digest, arch := "", ""
	if result != nil {
		if result.SizeBytes > 0 {
			v := result.SizeBytes
			size = &v
		}
		digest, arch = result.Digest, result.Arch
	}

	img := model.ImageRecord{
		ProjectID:   rec.ProjectID,
		ProjectKey:  projectKey,
		Registry:    registry,
		Repo:        repo,
		Tag:         tag,
		Image:       image,
		Digest:      digest,
		SizeBytes:   size,
		Arch:        arch,
		Branch:      req.Branch,
		CommitID:    req.CommitID,
		BuildID:     buildID,
		BuildNo:     rec.BuildNo,
		PushedAt:    pushedAt,
		PullCount:   0,
		DeployedEnv: model.JSONText(`[]`),
	}
	return s.upsertImageRecord(ctx, img)
}

// upsertImageRecord 登记一个镜像版本，同一个 tag 重复推送（重复构建、分支回滚重推、
// Jenkins 侧重跑同一版本）都只更新那一行，不报错也不新增副本。
//
// 唯一键是 (registry, repo, tag)，而 image_record 又是软删除表——被删掉的行**仍然占着**
// 这个唯一键。所以这里不能简单地"先查可见行、没有就新建"：如果那行是被软删的，
// 新建会直接撞唯一键报 duplicate，而那句报错会长成"构建成功但写库失败"，
// 让人完全看不出真因（删过的镜像再也推不回来）。因此撞键时要 Unscoped 把这行捞回来复活。
func (s *CIService) upsertImageRecord(ctx context.Context, img model.ImageRecord) (*model.ImageRecord, error) {
	key := func(tx *gorm.DB) *gorm.DB {
		return tx.Where("registry = ? AND repo = ? AND tag = ?", img.Registry, img.Repo, img.Tag)
	}
	updates := map[string]any{
		"digest": img.Digest, "size_bytes": img.SizeBytes, "pushed_at": img.PushedAt,
		"build_id": img.BuildID, "build_no": img.BuildNo, "arch": img.Arch,
		"branch": img.Branch, "commit_id": img.CommitID,
	}
	tx := key(s.db(ctx).Model(&model.ImageRecord{})).Updates(updates)
	if tx.Error != nil {
		return nil, tx.Error
	}
	if tx.RowsAffected == 0 {
		// 没有可见的同 tag 行才新建；撞唯一键说明是软删行占位，把它恢复出来。
		if err := s.db(ctx).Create(&img).Error; err != nil {
			if !isDuplicate(err) {
				return nil, err
			}
			updates["deleted_at"] = nil
			if uerr := key(s.db(ctx).Unscoped().Model(&model.ImageRecord{})).Updates(updates).Error; uerr != nil {
				return nil, uerr
			}
		}
	}
	var exist model.ImageRecord
	if err := key(s.db(ctx)).First(&exist).Error; err != nil {
		return nil, err
	}
	return &exist, nil
}

// CancelBuild 取消构建。
func (s *CIService) CancelBuild(ctx context.Context, id uint64, c Caller) error {
	rec, err := s.build(ctx, id)
	if err != nil {
		return err
	}
	switch rec.Status {
	case "success", "failed", "canceled":
		return errs.StateConflict("构建已结束（" + statusWord(rec.Status) + "），无法取消")
	}
	state, inPool := s.deps.Builds.CancelState(id)
	// 两种情况下必须由这里补写终态，否则记录会一直卡着不动：
	//   1. 池里找不到 —— 任务已结束但状态还没落库（窗口极短）；
	//   2. 任务当时**在排队** —— 被移出队列后它再也不会被执行，
	//      没有任何人会去写它的终态。批量打包一次提交十几个任务时最常见：
	//      前面几个还没跑完，用户就把后面排队的取消了。
	// 执行中的任务不在此列：它会收到 ctx 取消信号，由自身写终态。
	if !inPool || state == queue.StateQueued {
		fin := now()
		if err := s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", id).
			Updates(map[string]any{"status": "canceled", "stage": "已取消", "finished_at": &fin, "updated_at": fin}).Error; err != nil {
			return errs.Internalf(err, "更新构建状态失败")
		}
		s.deps.Stream.Finish(ctx, BuildLogStreamKey(id), "canceled")
	}
	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "build:cancel", TargetType: "build",
		TargetID: &id, TargetName: fmt.Sprintf("%s#%d", rec.ProjectKey, rec.BuildNo),
		Detail: "取消构建",
	})
	return nil
}

// ReconcileInterrupted 把重启后**永远收不了尾**的构建标成 interrupted（启动时跑一次）。
//
// 为什么必须有这一步：构建队列是**进程内**的（`internal/queue`），平台一重启，队列与执行
// 协程一起消失 —— 没人会再去写那些记录的终态，于是它们永远停在 `queued` / `running`，
// 界面上就是一个转不完的圈。实测踩过：库里留下过 3 条这样的孤儿。
//
// 判据只用"非终态"，不看心跳：重启这一刻**任何**还没结束的构建都已经不可能再继续了，
// 它依赖的执行实体随进程消失了 —— 不存在"还在跑所以别动它"的情形。
//
// 状态用独立的 `interrupted`，而不是复写 `failed` / `canceled`：
//   - `failed` 会让人去查构建本身哪里错了，可实际原因是平台重启；
//   - `canceled` 会把"平台重启"谎报成"有人点了取消"。
//
// 同时写 `error_code=INTERRUPTED` —— 这是唯一会**落库**的原因说明（日志流带 TTL，
// 重启后大概率已经随内存一起没了），不写它就等于这次中断不留任何可查痕迹。
//
// 收尾再发一条汇总通知（`build_interrupted`）。这个状态是**终态**、不会自行恢复，
// 唯一该做的事是"重新触发"—— 而在这个通知补上之前，没有任何地方会主动告诉人
// 这件事发生过（界面上的筛选器与重试按钮都是被动等人来看的）。
func (s *CIService) ReconcileInterrupted(ctx context.Context) (int64, error) {
	// 取**完整记录**，不只是 id：除了下面算 duration_sec 要用的 started_at / updated_at，
	// 收尾还要拿 project_key / build_no / branch / commit_id / trigger_name 去发通知。
	// ⚠️ 必须**在 UPDATE 之前**取 —— 那次 UPDATE 会把 updated_at 改写成扫描时刻，
	// 也会把 status 改掉；先取才能同时拿到"改之前"的时间与完整的展示字段。
	var rows []model.BuildRecord
	if err := s.db(ctx).Model(&model.BuildRecord{}).
		Where("status IN ?", []string{"queued", "running"}).
		Find(&rows).Error; err != nil {
		return 0, errs.Internalf(err, "查询中断的构建失败")
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	fin := now()
	if err := s.db(ctx).Model(&model.BuildRecord{}).Where("id IN ?", ids).
		Updates(map[string]any{
			"status":        "interrupted",
			"stage":         "已中断",
			"error_code":    "INTERRUPTED",
			"error_message": "平台重启，构建执行随进程中消失而被中断",
			"finished_at":   &fin,
			"updated_at":    fin,
		}).Error; err != nil {
		return 0, errs.Internalf(err, "标记中断的构建失败")
	}
	// 显式写 duration_sec = 「开始 → 最后一次有进展」的时长。
	//
	// 不写会踩一个**显示成假数字**的坑：`dto.go:185-193` 在 DurationSec 为空、
	// StartedAt/FinishedAt 都在时会**拿 finished_at - started_at 兜底**，
	// 而这里的 finished_at 是"扫描时刻"。平台若是挂了 8 小时才重启，
	// 一个只跑了 30 秒的构建就会理直气壮地显示「耗时 8 小时」。
	// 用 updated_at（进程消失前最后一次写库）当终点，才是它真实跑过的时间。
	for _, r := range rows {
		if r.StartedAt == nil {
			continue // 还没开跑（queued），没有"跑了多久"可言
		}
		sec := int64(r.UpdatedAt.Sub(*r.StartedAt).Seconds())
		if sec < 0 {
			sec = 0
		}
		if err := s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", r.ID).
			Update("duration_sec", uint32(sec)).Error; err != nil {
			// 只是展示用的一个数字，失败不该让整轮补偿中断；下面日志流照收。
			continue
		}
	}
	// 日志流可能还留在 Redis 里（带 TTL），一并收口 —— 否则面板会一直转，
	// 记录说"已中断"、日志面板还说"进行中"，又是一处自相矛盾。
	for _, id := range ids {
		_ = s.deps.Stream.Finish(ctx, BuildLogStreamKey(id), "interrupted")
	}

	// 最后才告警。
	//
	// ⚠️ 顺序不能颠倒：库先写对，再发通知。反过来的话，通知已经发出去了、改库却失败，
	// 就成了"群里说中断了 3 个构建、界面上还在转圈"——两个数据源互相打脸，
	// 而这种矛盾比少发一条通知难查得多。
	//
	// 这也是 `interrupted` 这个状态此前唯一的告警缺口：它不会自行恢复（终态），
	// 界面上的筛选器、重试入口都是被动等人来看的，没有通知就等于没人知道。
	s.notify.Dispatch(ctx, s.notify.BuildInterruptedMessage(ptrRecords(rows)))
	return int64(len(ids)), nil
}

// ptrRecords 把值切片转成指针切片。
//
// 需要它是因为 GORM 的 `Find` 只吃值切片（`[]model.BuildRecord`），而消息构造器
// 收的是指针切片（与 `BuildFailedMessage` 等既有构造器保持同一签名）。
// 用取下标的方式取地址：`for _, r := range rows { &r }` 在旧版 Go 里会全部指向同一个循环变量，
// 那是个"每条消息都显示最后一条记录"的经典坑。
func ptrRecords[T any](rows []T) []*T {
	out := make([]*T, 0, len(rows))
	for i := range rows {
		out = append(out, &rows[i])
	}
	return out
}

// RetryBuild 重新构建（同分支同参数）。
func (s *CIService) RetryBuild(ctx context.Context, id uint64, c Caller) (*BuildDTO, error) {
	rec, err := s.build(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.Status == "queued" || rec.Status == "running" {
		return nil, errs.StateConflict("构建正在进行中，无需重新构建")
	}
	return s.TriggerBuild(ctx, TriggerBuildInput{
		ProjectKey: rec.ProjectKey,
		Branch:     rec.Branch,
		Params:     decodeParamsMap(rec.ParamsSnapshot),
	}, c)
}

const (
	// 归因结论的来源，原样下发给前端。
	//
	// 一定要区分，且一定要让用户看见：他有知情权知道这段结论是模型读了日志说的，
	// 还是错误码查表查出来的。把规则结果渲染成"模型置信度 92%"，
	// 比不给结论更糟——它会让人以为日志已经被看过一遍。
	diagnosisSourceModel = "model"
	diagnosisSourceRule  = "rule"

	// modelDiagnosisTimeout 模型归因的独立上限。
	//
	// 刻意短于通道自身 timeout（配置里 60~120s）：诊断是"点一下就要等"的交互，
	// 与其让抽屉转两分钟，不如早点降级到规则结论**并说明为什么没用上模型**。
	// 通道 timeout 留给流式问答那种用户能看着字往外蹦的场景。
	modelDiagnosisTimeout = 45 * time.Second

	// 送给模型的日志上限。只取**尾部**：构建失败的根因几乎总在最后几十行，
	// 从头截只会留下"正在拉取依赖…"这类没信息量的前缀，白烧 token。
	diagnosisLogLines = 120
	diagnosisLogBytes = 16 << 10
)

// BuildAIDiagnosis 构建失败归因。
//
// 两级来源，逐级降级，且**如实标注**：
//  1. model —— 首选。把失败上下文与日志尾部交给平台默认模型通道，
//     要求它输出结构化结论。成功则落库缓存（同一条失败构建的日志不会再变，
//     重算只是重复花钱与重复等待）。
//  2. rule  —— 降级。通道未配置 / 调用失败 / 输出无法解析时，退回按错误码的
//     确定性结论，并用 notice 说明模型为什么没参与。
//
// 之所以降级而不是直接把错误抛给前端：错误码（如 NPM_INSTALL）本身已经指明了原因，
// 让用户拿不到任何结论，比拿到一个**标注了来源**的结论更糟。
func (s *CIService) BuildAIDiagnosis(ctx context.Context, id uint64) (*DiagnosisResult, error) {
	rec, err := s.build(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.Status != "failed" {
		return nil, errs.StateConflict("仅失败构建支持 AI 归因分析")
	}

	// 缓存命中：上一轮模型已经归因过，直接返回，不重复计费。
	if d := decodeAIDiagnosis(rec.AIDiagnosis); d != nil && d.Source == diagnosisSourceModel {
		return &DiagnosisResult{
			Source: diagnosisSourceModel, Model: d.Model,
			Reason: d.Reason, Suggestion: d.Suggestion,
			Confidence: d.Confidence, ErrorCode: rec.ErrorCode,
		}, nil
	}

	rule := ruleDiagnosis(rec)
	res, merr := s.modelDiagnosis(ctx, rec)
	if merr != nil {
		s.log("ci").Warn("模型归因未生效，已降级为错误码规则", "build", rec.ID, "err", merr)
		rule.Notice = "本次未由模型分析（" + merr.Error() + "），以下结论来自平台错误码规则。"
		return rule, nil
	}
	if err := s.saveModelDiagnosis(ctx, rec, res); err != nil {
		// 缓存写失败不影响本次结论：用户已经拿到东西了，只是下次要重算。
		s.log("ci").Warn("写入模型归因结果失败", "build", rec.ID, "err", err)
	}
	return res, nil
}

// ruleDiagnosis 错误码规则归因：先看库里存的（种子/执行期写入的），再看规则表。
func ruleDiagnosis(rec *model.BuildRecord) *DiagnosisResult {
	if d := decodeAIDiagnosis(rec.AIDiagnosis); d != nil {
		return &DiagnosisResult{Source: diagnosisSourceRule, Reason: d.Reason, Suggestion: d.Suggestion, Confidence: d.Confidence, ErrorCode: rec.ErrorCode}
	}
	if d := diagnoseBuild(rec.ErrorCode, rec.ProjectKey); d != nil {
		return &DiagnosisResult{Source: diagnosisSourceRule, Reason: d.Reason, Suggestion: d.Suggestion, Confidence: d.Confidence, ErrorCode: rec.ErrorCode}
	}
	return &DiagnosisResult{
		Source: diagnosisSourceRule,
		Reason: "未识别的失败原因", Suggestion: "请查看日志定位具体报错",
		Confidence: 0.3, ErrorCode: rec.ErrorCode,
	}
}

// modelDiagnosis 走模型通道做一次归因。返回错误表示"模型这次没用上"，
// 由调用方降级——错误原因会原样进 notice，所以别在这里包一层"操作失败"这种废话。
func (s *CIService) modelDiagnosis(ctx context.Context, rec *model.BuildRecord) (*DiagnosisResult, error) {
	if s.sys == nil {
		return nil, errors.New("模型通道未注入")
	}
	client, cfg, err := s.sys.DefaultModelClient(ctx)
	if err != nil {
		return nil, err
	}
	logs, err := s.recentBuildLogText(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(logs) == "" {
		return nil, errors.New("该构建没有日志内容可供分析")
	}

	cctx, cancel := context.WithTimeout(ctx, modelDiagnosisTimeout)
	defer cancel()

	text, _, err := client.Chat(cctx, buildDiagnosisMessages(rec, logs))
	if err != nil {
		return nil, err
	}
	d, err := parseModelDiagnosis(text)
	if err != nil {
		return nil, err
	}
	return &DiagnosisResult{
		Source: diagnosisSourceModel, Model: cfg.Name,
		Reason: d.Reason, Suggestion: d.Suggestion, Confidence: d.Confidence,
		ErrorCode: rec.ErrorCode,
	}, nil
}

// recentBuildLogText 取构建日志尾部拼成纯文本。
//
// 按 seq 倒序取 N 行再翻正，而不是正序取前 N 行：日志可能上万行，
// 正序 LIMIT 只会拿到最早的"Cloning into…"。
func (s *CIService) recentBuildLogText(ctx context.Context, buildID uint64) (string, error) {
	var rows []model.BuildLog
	if err := s.db(ctx).Where("build_id = ?", buildID).
		Order("seq desc").Limit(diagnosisLogLines).Find(&rows).Error; err != nil {
		return "", errs.Internalf(err, "查询构建日志失败")
	}
	if len(rows) == 0 {
		return "", nil
	}
	lines := make([]string, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		lines = append(lines, strings.TrimRight(rows[i].Content, "\r\n"))
	}
	text := strings.Join(lines, "\n")
	if len(text) > diagnosisLogBytes {
		// 从尾部截断后开头可能落在多字节字符中间，ToValidUTF8 会把这半个字符清掉。
		text = strings.ToValidUTF8(text[len(text)-diagnosisLogBytes:], "")
	}
	return text, nil
}

// saveModelDiagnosis 把模型结论写回 build_record.ai_diagnosis 当缓存。
//
// 只缓存**模型**结论：规则结论是纯函数、重算零成本；模型结论重算要花钱要等，
// 而同一条失败构建的日志与错误码不会再变，缓存它才有意义。
func (s *CIService) saveModelDiagnosis(ctx context.Context, rec *model.BuildRecord, res *DiagnosisResult) error {
	payload, err := json.Marshal(AIDiagnosisDTO{
		Reason: res.Reason, Suggestion: res.Suggestion, Confidence: res.Confidence,
		Source: diagnosisSourceModel, Model: res.Model,
	})
	if err != nil {
		return err
	}
	return s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", rec.ID).
		Update("ai_diagnosis", model.JSONText(payload)).Error
}

// buildDiagnosisMessages 组装归因提示词。
func buildDiagnosisMessages(rec *model.BuildRecord, logs string) []llm.Message {
	ctxLines := []string{
		fmt.Sprintf("项目：%s", rec.ProjectKey),
		fmt.Sprintf("分支：%s（commit %s）", rec.Branch, shortCommit(rec.CommitID)),
		fmt.Sprintf("构建号：#%d，触发方式 %s", rec.BuildNo, rec.TriggerType),
		fmt.Sprintf("失败阶段：%s", stringOr(rec.Stage, "未知")),
	}
	if code := strings.TrimSpace(rec.ErrorCode); code != "" {
		ctxLines = append(ctxLines, "平台记录的错误码："+code)
	}
	if job := strings.TrimSpace(rec.JenkinsJob); job != "" {
		ctxLines = append(ctxLines, "Jenkins Job："+job)
	}

	user := "【构建上下文】\n" + strings.Join(ctxLines, "\n") +
		"\n\n【构建日志尾部】\n" + logs

	return []llm.Message{
		{Role: "system", Content: diagnosisSystemPrompt},
		{Role: "user", Content: user},
	}
}

// diagnosisSystemPrompt 归因提示词。
//
// 刻意把"不要编造日志里没有的东西"写在第一条：模型在没有足够信息时最自然的反应
// 是补一个像样的包名/文件名，而运维看到"@yunnian/shared@2.6.1 不存在"这种
// 具体到版本号的假原因，会真的去查那个版本——比不给结论更浪费时间。
const diagnosisSystemPrompt = `你是 YunNian 云辇平台（公司内部 CI/CD 平台）的构建失败归因助手。

任务：依据给出的「构建上下文」与「构建日志尾部」，判断这次构建最可能的失败原因，并给出下一步动作。

规则：
1. 只依据给定内容判断。日志与上下文里没出现的文件名、包名、版本号一律不要写——编造一个具体到版本号的假原因，会让排查方向整体跑偏。
2. 原因要具体到"哪一步、哪个东西、什么现象"，不要写"构建失败可能有多种原因"这类空话。
3. 建议要给到可执行的一步（改哪个文件、查哪条配置、跑哪条命令），不要写"请检查配置"。
4. 信息不足以定位时，把 confidence 压到 0.3 以下，并在原因里直说要再看什么。
5. 用中文。

输出格式（只输出这个 JSON 对象，不要代码块、不要任何额外说明）：
{"reason":"失败原因","suggestion":"修复建议","confidence":0.0~1.0 之间的小数}`

// modelDiagnosisPayload 模型输出的目标结构。
type modelDiagnosisPayload struct {
	Reason     string  `json:"reason"`
	Suggestion string  `json:"suggestion"`
	Confidence float64 `json:"confidence"`
}

// parseModelDiagnosis 解析模型输出。
//
// 不假设模型严格听话：即便提示词写了"不要代码块"，实测仍常见两种偏差——
// 包了一层 ```json 围栏，或在 JSON 前后加一句"好的，分析如下"。
// 两种都在这里抹平，比让用户看到"诊断不可用"划算。
func parseModelDiagnosis(text string) (*modelDiagnosisPayload, error) {
	raw := strings.TrimSpace(stripCodeFence(strings.TrimSpace(text)))
	if raw == "" {
		return nil, errors.New("模型没有返回内容")
	}
	// 从第一个 { 开始流式解码**一个** JSON 对象：尾随的"希望有帮助"之类会被自动忽略，
	// 而 reason 里出现 `}`（如报错文本原样带进来）也不会像按最后一个 `}` 截取那样截错。
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	var out modelDiagnosisPayload
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&out); err != nil {
		return nil, fmt.Errorf("模型输出不是合法 JSON：%w", err)
	}
	out.Reason = strings.TrimSpace(out.Reason)
	out.Suggestion = strings.TrimSpace(out.Suggestion)
	if out.Reason == "" || out.Suggestion == "" {
		return nil, errors.New("模型输出缺少 reason 或 suggestion")
	}
	// 模型常把 0.9 写成 90，这里只做钳制不做换算——90 钳成 1 也仍是"高置信"，
	// 而擅自除以 100 会把一个真的 0.9 变成 0.009。
	if out.Confidence < 0 {
		out.Confidence = 0
	}
	if out.Confidence > 1 {
		out.Confidence = 1
	}
	return &out, nil
}

// stripCodeFence 剥掉 markdown 代码围栏（含 ```json 这种带语言标记的写法）。
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:] // 去掉语言标记那一行
	}
	if j := strings.LastIndex(s, "```"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

// DiagnosisResult 归因结果。
//
// source 必须随结论一起下发：前端按它决定怎么措辞（"模型分析" vs "错误码规则"）。
// notice 用于承载"模型为什么这次没参与"——这类信息不该被吞掉，
// 否则用户会以为自己看到的就是模型给的。
type DiagnosisResult struct {
	Source     string  `json:"source"`           // model / rule
	Model      string  `json:"model,omitempty"`  // source=model 时的通道名
	Notice     string  `json:"notice,omitempty"` // 需要如实告知用户的附加说明
	Reason     string  `json:"reason"`
	Suggestion string  `json:"suggestion"`
	Confidence float64 `json:"confidence"`
	ErrorCode  string  `json:"errorCode,omitempty"`
}

// --------------------------------------------------------------------------- 镜像

// ListImages 镜像列表。
func (s *CIService) ListImages(ctx context.Context, q ImageQuery) (*httpx.PageQuery, any, int64, error) {
	pq := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize, SortBy: q.SortBy, Order: q.Order, Keyword: q.Keyword}
	column, desc := pq.Normalize(imageSortColumns, "pushed_at")

	db := s.db(ctx).Model(&model.ImageRecord{})
	if k := strings.TrimSpace(q.ProjectKey); k != "" {
		db = db.Where("project_key = ?", k)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("project_key LIKE ? OR tag LIKE ? OR image LIKE ?", like, like, like)
	}
	if env := strings.TrimSpace(q.Env); env != "" {
		// deployed_envs 是 JSON 数组列，用 LIKE 做包含判断。
		// MySQL 5.7 没有 JSON_CONTAINS 的可索引优化，这里数据量小（万级）足够。
		db = db.Where("deployed_envs LIKE ?", "%\""+env+"\"%")
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "统计镜像数量失败")
	}
	var rows []model.ImageRecord
	if err := db.Order(column + " " + orderWord(desc)).
		Offset(pq.Offset()).Limit(pq.PageSize).Find(&rows).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "查询镜像失败")
	}

	names, err := s.projectNames(ctx, projectKeysOfImages(rows))
	if err != nil {
		return nil, nil, 0, err
	}
	out := make([]ImageDTO, 0, len(rows))
	for i := range rows {
		out = append(out, imageDTO(&rows[i], names[rows[i].ProjectKey]))
	}
	return &pq, out, total, nil
}

var imageSortColumns = map[string]string{
	"pushedAt":  "pushed_at",
	"size":      "size_bytes",
	"pullCount": "pull_count",
	"tag":       "tag",
}

// GetImage 镜像详情。
func (s *CIService) GetImage(ctx context.Context, id uint64) (*ImageDTO, error) {
	var img model.ImageRecord
	if err := firstOrNotFound(s.db(ctx), id, &img, "镜像"); err != nil {
		return nil, err
	}
	names, err := s.projectNames(ctx, []string{img.ProjectKey})
	if err != nil {
		return nil, err
	}
	dto := imageDTO(&img, names[img.ProjectKey])
	return &dto, nil
}

// ImageUsage 镜像被哪些服务使用。
type ImageUsage struct {
	ImageID  string      `json:"imageId"`
	InUse    bool        `json:"inUse"`
	Services []ImageUser `json:"services"`
}

// ImageUser 一处使用关系。
type ImageUser struct {
	ServiceID  string `json:"serviceId"`
	EnvKey     string `json:"envKey"`
	ProjectKey string `json:"projectKey"`
	Name       string `json:"name"`
	Image      string `json:"image"`
}

// ImageUsedBy 清理前的影响面校验。
//
// 这是删除镜像前的必要一步：镜像一旦被删，正在用它的服务重启就会 ImagePullBackOff，
// 而这个错误发生在集群里、不在平台上，排查成本很高。
func (s *CIService) ImageUsedBy(ctx context.Context, id uint64) (*ImageUsage, error) {
	var img model.ImageRecord
	if err := firstOrNotFound(s.db(ctx), id, &img, "镜像"); err != nil {
		return nil, err
	}
	var svcs []model.ServiceInstance
	if err := s.db(ctx).Where("current_image = ?", img.Image).Find(&svcs).Error; err != nil {
		return nil, errs.Internalf(err, "查询镜像使用情况失败")
	}
	names, err := s.projectNames(ctx, projectKeysOfServices(svcs))
	if err != nil {
		return nil, err
	}
	out := &ImageUsage{ImageID: httpx.IDString(img.ID), InUse: len(svcs) > 0, Services: []ImageUser{}}
	for i := range svcs {
		out.Services = append(out.Services, ImageUser{
			ServiceID:  httpx.IDString(svcs[i].ID),
			EnvKey:     svcs[i].EnvKey,
			ProjectKey: svcs[i].ProjectKey,
			Name:       names[svcs[i].ProjectKey],
			Image:      svcs[i].CurrentImage,
		})
	}
	return out, nil
}

// DeleteImage 清理镜像。被使用时拒绝（见 ImageUsedBy 的说明）。
func (s *CIService) DeleteImage(ctx context.Context, id uint64, c Caller) error {
	usage, err := s.ImageUsedBy(ctx, id)
	if err != nil {
		return err
	}
	if usage.InUse {
		return errs.StateConflict(fmt.Sprintf("该镜像仍被 %d 个服务使用，清理后这些服务将无法重新拉取", len(usage.Services)))
	}
	var img model.ImageRecord
	if err := firstOrNotFound(s.db(ctx), id, &img, "镜像"); err != nil {
		return err
	}
	if err := s.db(ctx).Delete(&model.ImageRecord{}, img.ID).Error; err != nil {
		return errs.Internalf(err, "删除镜像失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "image:delete", TargetType: "image",
		TargetID: &img.ID, TargetName: img.Image, Detail: "清理镜像 " + img.Image,
	})
	return nil
}

// CleanupInput 批量清理入参。
type CleanupInput struct {
	// KeepPerProject 每个项目保留最近 N 个版本。
	KeepPerProject int    `json:"keepPerProject" binding:"omitempty,min=1,max=100"`
	ProjectKey     string `json:"projectKey"`
	// DryRun 只统计不删除，供前端先给用户看一眼影响面。
	DryRun bool `json:"dryRun"`
}

// CleanupResult 批量清理结果。
type CleanupResult struct {
	DryRun      bool     `json:"dryRun"`
	Scanned     int      `json:"scanned"`
	Deleted     int      `json:"deleted"`
	SkippedUsed int      `json:"skippedUsed"`
	Tags        []string `json:"tags"`
}

// CleanupImages 按保留策略批量清理。
//
// 两道保险：**在使用中的镜像一律跳过**（不因为批量就放宽），
// 以及支持 dry-run 让用户先看清单。清理是不可逆操作，宁可多确认一次。
func (s *CIService) CleanupImages(ctx context.Context, in CleanupInput, c Caller) (*CleanupResult, error) {
	keep := in.KeepPerProject
	if keep <= 0 {
		keep = 10
	}
	res := &CleanupResult{DryRun: in.DryRun, Tags: []string{}}

	db := s.db(ctx).Model(&model.ImageRecord{})
	if k := strings.TrimSpace(in.ProjectKey); k != "" {
		db = db.Where("project_key = ?", k)
	}
	var rows []model.ImageRecord
	if err := db.Order("project_key asc, pushed_at desc").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询镜像失败")
	}
	res.Scanned = len(rows)

	// 当前在用的镜像集合，一次查完避免逐条查库。
	var usedImages []string
	if err := s.db(ctx).Model(&model.ServiceInstance{}).Distinct("current_image").Pluck("current_image", &usedImages).Error; err != nil {
		return nil, errs.Internalf(err, "查询在用镜像失败")
	}
	used := make(map[string]bool, len(usedImages))
	for _, v := range usedImages {
		used[v] = true
	}

	seen := map[string]int{}
	for i := range rows {
		img := rows[i]
		seen[img.ProjectKey]++
		if seen[img.ProjectKey] <= keep {
			continue // 保留窗口内
		}
		if used[img.Image] {
			res.SkippedUsed++
			continue
		}
		res.Tags = append(res.Tags, img.Image)
		if in.DryRun {
			continue
		}
		if err := s.db(ctx).Delete(&model.ImageRecord{}, img.ID).Error; err != nil {
			s.log("ci").Warn("清理镜像失败", "imageId", img.ID, "err", err)
			continue
		}
		res.Deleted++
	}

	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "image:cleanup", TargetType: "image",
		TargetName: in.ProjectKey,
		Detail: fmt.Sprintf("批量清理镜像：扫描 %d，清理 %d，跳过在用 %d（dryRun=%v）",
			res.Scanned, res.Deleted, res.SkippedUsed, in.DryRun),
	})
	return res, nil
}

// --------------------------------------------------------------------------- 项目关联的构建/镜像/部署

// ListProjectImages 某项目的镜像版本。
func (s *CIService) ListProjectImages(ctx context.Context, key string, q ImageQuery) (*httpx.PageQuery, any, int64, error) {
	if _, err := s.projectByKey(ctx, key); err != nil {
		return nil, nil, 0, err
	}
	q.ProjectKey = key
	return s.ListImages(ctx, q)
}

// --------------------------------------------------------------------------- 内部助手

func (s *CIService) projectByKey(ctx context.Context, key string) (*model.Project, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errs.Invalid("项目标识不能为空")
	}
	var p model.Project
	if err := s.db(ctx).Where("project_key = ?", strings.TrimSpace(key)).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("项目 %s 不存在", key)
		}
		return nil, errs.Internalf(err, "查询项目失败")
	}
	return &p, nil
}

func (s *CIService) build(ctx context.Context, id uint64) (*model.BuildRecord, error) {
	var rec model.BuildRecord
	if err := s.db(ctx).First(&rec, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("构建记录不存在")
		}
		return nil, errs.Internalf(err, "查询构建记录失败")
	}
	return &rec, nil
}

// decorateBuilds 批量补齐构建记录的项目名/类型与镜像信息（避免 N+1）。
func (s *CIService) decorateBuilds(ctx context.Context, records []model.BuildRecord, withImage bool) []BuildDTO {
	out := make([]BuildDTO, 0, len(records))
	if len(records) == 0 {
		return out
	}
	keys := make([]string, 0, len(records))
	for i := range records {
		keys = append(keys, records[i].ProjectKey)
	}
	projs, err := s.projectsByKeys(ctx, keys)
	if err != nil {
		s.log("ci").Warn("补齐项目信息失败", "err", err)
	}
	images := map[uint64]*model.ImageRecord{}
	if withImage {
		images, err = s.imagesOfBuilds(ctx, buildIDsOfRecords(records))
		if err != nil {
			s.log("ci").Warn("补齐镜像信息失败", "err", err)
		}
	}
	for i := range records {
		rec := &records[i]
		out = append(out, buildDTO(rec, projs[rec.ProjectKey], images[rec.ID], nil))
	}
	return out
}

func (s *CIService) projectsByKeys(ctx context.Context, keys []string) (map[string]*model.Project, error) {
	out := map[string]*model.Project{}
	uniq := dedupeStrings(keys)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []model.Project
	if err := s.db(ctx).Where("project_key IN ?", uniq).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询项目失败")
	}
	for i := range rows {
		out[rows[i].ProjectKey] = &rows[i]
	}
	return out, nil
}

func (s *CIService) projectNames(ctx context.Context, keys []string) (map[string]string, error) {
	projs, err := s.projectsByKeys(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(projs))
	for k, p := range projs {
		out[k] = p.Name
	}
	return out, nil
}

func (s *CIService) userNames(ctx context.Context, ids []uint64) (map[uint64]string, error) {
	out := map[uint64]string{}
	uniq := dedupeUint64(ids)
	if len(uniq) == 0 {
		return out, nil
	}
	var users []model.User
	if err := s.db(ctx).Select("id", "name").Where("id IN ?", uniq).Find(&users).Error; err != nil {
		return nil, errs.Internalf(err, "查询用户失败")
	}
	for i := range users {
		out[users[i].ID] = users[i].Name
	}
	return out, nil
}

// projectStats 一次聚合出列表页需要的三个统计值。
//
// 为什么要聚合：项目列表默认 20 条，如果逐条查统计就是 60 次查询（经典 N+1）。
// 这里用一条 GROUP BY 拿回全部。
func (s *CIService) projectStats(ctx context.Context, keys []string) (map[string]ProjectStats, error) {
	out := map[string]ProjectStats{}
	uniq := dedupeStrings(keys)
	if len(uniq) == 0 {
		return out, nil
	}
	type row struct {
		ProjectKey string
		Total      int64
		Success    int64
		AvgDur     float64
	}
	var rows []row
	if err := s.db(ctx).
		Table("build_record").
		Select("project_key, COUNT(*) AS total, "+
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END) AS success, "+
			"COALESCE(AVG(NULLIF(duration_sec, 0)), 0) AS avg_dur").
		Where("project_key IN ?", uniq).
		Group("project_key").
		Scan(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "统计项目构建数据失败")
	}
	for _, r := range rows {
		st := ProjectStats{Total: r.Total, AvgDuration: r.AvgDur}
		if r.Total > 0 {
			st.Success = r.Success
		}
		out[r.ProjectKey] = st
	}
	return out, nil
}

// lastBuilds 每个项目最近一条构建。
//
// 用「按 created_at 倒序取第一条」的方式在内存里去重，而不是相关子查询：
// 项目列表最多 200 条，取回每个项目的少量记录比 20 次子查询便宜，也更好读。
func (s *CIService) lastBuilds(ctx context.Context, keys []string) (map[string]model.BuildRecord, error) {
	out := map[string]model.BuildRecord{}
	uniq := dedupeStrings(keys)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []model.BuildRecord
	if err := s.db(ctx).Where("project_key IN ?", uniq).
		Order("created_at desc").Limit(len(uniq) * 5).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询最近构建失败")
	}
	for i := range rows {
		if _, ok := out[rows[i].ProjectKey]; ok {
			continue
		}
		out[rows[i].ProjectKey] = rows[i]
	}
	return out, nil
}

func (s *CIService) imagesOfBuilds(ctx context.Context, buildIDs []uint64) (map[uint64]*model.ImageRecord, error) {
	out := map[uint64]*model.ImageRecord{}
	uniq := dedupeUint64(buildIDs)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []model.ImageRecord
	if err := s.db(ctx).Where("build_id IN ?", uniq).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询构建镜像失败")
	}
	for i := range rows {
		out[rows[i].BuildID] = &rows[i]
	}
	return out, nil
}

// nextBuildNo 取该项目下一个构建号。
//
// 说明：这里用 MAX+1 而不是自增序列，是为了让 buildNo 在项目内连续可读
// （用户会说"用户服务的 #38 号构建"）。并发下极小概率撞号，
// 由 uk_build_no 唯一索引兜底，调用方收到重复错误重试即可。
func (s *CIService) nextBuildNo(ctx context.Context, projectID uint64) (uint32, error) {
	// 用带别名的结构体接收 MAX()：直接 Scan 到 *uint32 会得到 **uint32，
	// 空项目时 NULL 的语义也表达不清楚。
	var row struct {
		MaxNo *uint32 `gorm:"column:max_no"`
	}
	if err := s.db(ctx).Model(&model.BuildRecord{}).
		Where("project_id = ?", projectID).
		Select("MAX(build_no) AS max_no").
		Scan(&row).Error; err != nil {
		return 0, errs.Internalf(err, "计算构建号失败")
	}
	if row.MaxNo == nil {
		return 1, nil
	}
	return *row.MaxNo + 1, nil
}

// mergeBuildParams 合并项目默认参数与本次覆盖参数。
//
// 密文参数不落进 ParamsSnapshot 之外的任何地方；快照本身会入库，
// 所以这里把 secret 项的值替换成掩码后再存——审计要的是"用了哪些参数"，
// 不是"参数值是什么"。
func (s *CIService) mergeBuildParams(p *model.Project, override map[string]string) (model.JSONText, error) {
	var base []BuildParamDTO
	if !p.BuildParams.Empty() {
		if err := json.Unmarshal(p.BuildParams, &base); err != nil {
			base = nil
		}
	}
	merged := make([]BuildParamDTO, 0, len(base))
	for _, b := range base {
		if v, ok := override[b.Key]; ok {
			b.Value = v
		}
		merged = append(merged, b)
	}
	for k, v := range override {
		found := false
		for _, m := range merged {
			if m.Key == k {
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, BuildParamDTO{Key: k, Value: v})
		}
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, errs.Internalf(err, "序列化构建参数失败")
	}
	return model.JSONText(raw), nil
}

func (s *CIService) defaultRegistry() string {
	if r := strings.TrimSpace(s.deps.Cfg.Integration.Harbor.Registry); r != "" {
		return r
	}
	return "harbor.yunnian.internal"
}

// resolveOwner 把"负责人姓名"解析成用户 ID。空则落到当前登录人。
func (s *CIService) resolveOwner(ctx context.Context, name string, c Caller) (uint64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if c.User == nil {
			return 0, errs.Invalid("负责人不能为空")
		}
		return c.User.ID, nil
	}
	// 允许直接填用户 ID
	if id, err := httpx.ParseIDString(name); err == nil {
		var cnt int64
		if err := s.db(ctx).Model(&model.User{}).Where("id = ?", id).Count(&cnt).Error; err != nil {
			return 0, errs.Internalf(err, "校验负责人失败")
		}
		if cnt > 0 {
			return id, nil
		}
	}
	var u model.User
	if err := s.db(ctx).Where("name = ? OR username = ?", name, name).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, errs.Invalidf("负责人 %q 不存在：请填写平台用户名或姓名", name)
		}
		return 0, errs.Internalf(err, "查询负责人失败")
	}
	return u.ID, nil
}

// persistBuildLogs 构建结束后把日志批量落库。
//
// 落库时机放在终态之后：日志是"事后查阅"用的，实时性靠 SSE，
// 而落库放在终态前会让任务收尾变慢（用户看到状态迟迟不变）。
func (s *CIService) persistBuildLogs(ctx context.Context, buildID uint64, lines []logLineRecord) {
	if len(lines) == 0 {
		return
	}
	const batch = 200
	rows := make([]model.BuildLog, 0, batch)
	var seq uint32
	for i := range lines {
		seq++
		rows = append(rows, model.BuildLog{
			BuildID:  buildID,
			Seq:      seq,
			Level:    normalLevel(lines[i].level),
			Content:  clampLogLine(lines[i].line, 2000),
			LoggedAt: lines[i].at,
		})
		if len(rows) >= batch {
			s.flushBuildLogs(ctx, buildID, rows)
			rows = rows[:0]
		}
	}
	s.flushBuildLogs(ctx, buildID, rows)
}

// clampLogLine 把一行日志压到 max 字节以内，并保证结果是**合法 UTF-8**。
//
// 不能直接 `content[:2000]` —— 那是按字节切。一个中文字符占 3 字节，切点很容易落在
// 字符中间，留下半个字符（非法 UTF-8）。MySQL 的 utf8mb4 列遇到它会直接拒绝整条 INSERT：
//
//	Error 1366 (HY000): Incorrect string value: '\xBC\x8C\xE6\x94\xB9...' for column 'content'
//
// 实测踩到过一次：某个构建的日志因此**一行都没落库**，用户看到的是
// "构建失败了、日志是空的" —— 恰恰是最需要证据的时候没有证据。
//
// 上游也可能给出半截：docker 的输出是按块读的，多字节字符会被切在两个块之间，
// 所以最后还要过一遍 ToValidUTF8，把没法还原的残字节清掉。
func clampLogLine(s string, max int) string {
	if len(s) > max {
		// 切点若落在续字节（0b10xxxxxx）上，说明切在了字符中间，向前退到字符边界。
		cut := max
		for cut > 0 && s[cut]&0xC0 == 0x80 {
			cut--
		}
		s = s[:cut]
	}
	return strings.ToValidUTF8(s, "")
}

// flushBuildLogs 批量写日志；批量失败时**逐条重试**。
//
// 为什么要有逐条兜底：一条内容有问题的行会让整批 200 行被 MySQL 一起拒绝。
// 早先的实现遇到这种情况直接 return，于是那次构建的日志全部丢失。
// 逐条写把损失限制在真正有问题的那几行上 —— 剩下 199 行照样能看。
func (s *CIService) flushBuildLogs(ctx context.Context, buildID uint64, rows []model.BuildLog) {
	if len(rows) == 0 {
		return
	}
	batchErr := s.db(ctx).Create(&rows).Error
	if batchErr == nil {
		return
	}
	s.log("ci.build").Warn("批量落库构建日志失败，改为逐条写",
		"buildId", buildID, "count", len(rows), "err", batchErr)
	saved, dropped := 0, 0
	for i := range rows {
		one := rows[i]
		if err := s.db(ctx).Create(&one).Error; err != nil {
			dropped++
			continue
		}
		saved++
	}
	s.log("ci.build").Warn("构建日志逐条落库完成",
		"buildId", buildID, "saved", saved, "dropped", dropped)
}

type logLineRecord struct {
	level string
	line  string
	at    time.Time
}

// --------------------------------------------------------------------------- 小工具

func projectToDTO(p *model.Project, owner string, st ProjectStats, last *BuildDTO) ProjectDTO {
	out := ProjectDTO{
		ID:             httpx.IDString(p.ID),
		Key:            p.ProjectKey,
		Name:           p.Name,
		Type:           p.Type,
		GitProvider:    p.GitProvider,
		GitURL:         p.GitURL,
		GitUsername:    p.GitUsername,
		DefaultBranch:  p.DefaultBranch,
		BuildMethod:    p.BuildMethod,
		JenkinsJob:     p.JenkinsJob,
		DockerfilePath: p.DockerfilePath,
		BuildContext:   p.BuildContext,
		BuildScript:    p.BuildScript,
		Registry:       p.Registry,
		ImageName:      p.ImageName,
		TagPattern:     p.TagPattern,
		BuildParams:    decodeBuildParams(p.BuildParams),
		Owner:          owner,
		Team:           p.Team,
		Description:    p.Description,
		Enabled:        p.Enabled,
		CreatedAt:      formatTime(p.CreatedAt),
		UpdatedAt:      formatTime(p.UpdatedAt),
		LastBuild:      last,
		TotalBuilds:    st.Total,
		AvgDuration:    int64(st.AvgDuration),
	}
	if st.Total > 0 {
		out.SuccessRate = roundTo(float64(st.Success)/float64(st.Total)*100, 1)
	}
	return out
}

func decodeBuildParams(raw model.JSONText) []BuildParamDTO {
	if raw.Empty() {
		return []BuildParamDTO{}
	}
	var out []BuildParamDTO
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []BuildParamDTO{}
	}
	// 密文参数的值不外发，只回掩码。
	for i := range out {
		if out[i].Secret {
			out[i].Value = maskValue(out[i].Value)
		}
	}
	return out
}

func encodeBuildParams(in []BuildParamDTO) (model.JSONText, error) {
	if len(in) == 0 {
		return model.JSONText(`[]`), nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, errs.Internalf(err, "序列化构建参数失败")
	}
	return model.JSONText(raw), nil
}

// decodeParamsMap 把参数快照转成 map，供构建实现使用。
func decodeParamsMap(raw model.JSONText) map[string]string {
	out := map[string]string{}
	if raw.Empty() {
		return out
	}
	var list []BuildParamDTO
	if err := json.Unmarshal(raw, &list); err != nil {
		return out
	}
	for _, p := range list {
		out[p.Key] = p.Value
	}
	return out
}

// renderTag 渲染 tag 模板。支持 {buildNo} / {branch} / {commitId} / {timestamp} / {date}。
//
// {buildNo} 是**项目级构建序号**（每构建一次 +1，与 build_record.build_no 同源），
// 默认规则 `v{buildNo}` 就是靠它每次自动递增一个版本号。
//
// ⚠️ 它取的是**本次构建的序号**，不是"成功构建的次数"：一次失败会占掉一个号，
// 于是 tag 序列里会出现空洞（v1、v2、v4）。这是刻意的 —— 换成"只数成功次数"，
// 就要在构建成功那一刻回写 tag，而那时镜像已经推出去了；一旦回写失败，
// 下一个成功构建会拿到同一个 tag **覆盖掉上一个** —— 版本号不再唯一，比空洞严重得多。
//
// 渲染结果必须能当 Docker tag 用：只保留 [A-Za-z0-9_.-]，且截断到 128 字节。
func renderTag(pattern, branch, commitID string, buildNo int64, at time.Time) string {
	if strings.TrimSpace(pattern) == "" {
		pattern = buildscript.DefaultTagPattern
	}
	rep := strings.NewReplacer(
		"{buildNo}", sanitizeTagPart(strconv.FormatInt(buildNo, 10)),
		"{branch}", sanitizeTagPart(branch),
		"{commitId}", sanitizeTagPart(shortCommit(commitID)),
		"{timestamp}", at.Format("20060102150405"),
		"{date}", at.Format("20060102"),
	)
	out := rep.Replace(pattern)
	if out == "" {
		// 用户写了一个全是非法字符的模板（比如 `///`）。留空会让镜像地址变成
		// `repo/name:` —— 一个 docker 会拒绝、但报错完全指不到模板上去的地址。
		out = sanitizeTagPart(strconv.FormatInt(buildNo, 10))
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

// sanitizeTagPart 清洗成合法的 Docker tag 片段。
// Docker tag 只允许 [A-Za-z0-9_.-]，分支名里的 / 是最常见的坑（release/2.6）。
func sanitizeTagPart(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		return "unknown"
	}
	return out
}

func buildImageRef(registry, imageName, tag string) string {
	registry = strings.TrimRight(strings.TrimSpace(registry), "/")
	imageName = strings.Trim(strings.TrimSpace(imageName), "/")
	if registry == "" {
		registry = "harbor.yunnian.internal"
	}
	return registry + "/" + imageName + ":" + tag
}

// splitImageRef 拆开 repo:tag。registry 与 repo 的边界是第一个 /。
func splitImageRef(image string) (registry, repo, tag string) {
	image = strings.TrimSpace(image)
	// 完整的镜像引用里不允许出现 scheme，但项目上配的 registry 可能是带 scheme 的
	// （实测有 `https://es-repo.example.com:5000`）。不剥掉的话，下面按
	// 第一个斜杠切分会把 `https:` 当成 registry、把 `//es-repo...` 当成 repo，
	// 存出一条 registry='https:' 的坏记录——已经真实发生过一次。
	// 镜像地址本身不需要协议，剥掉不丢任何信息。
	if i := strings.Index(image, "://"); i > 0 {
		image = image[i+3:]
	}
	// 前后多余的斜杠也不构成镜像地址的一部分，切分前先削掉，
	// 否则 `host:5000//escloud/x` 会把 repo 切成 `/escloud/x`。
	image = strings.Trim(image, "/")
	if i := strings.LastIndex(image, ":"); i > 0 && !strings.Contains(image[i+1:], "/") {
		tag = image[i+1:]
		image = image[:i]
	}
	if i := strings.Index(image, "/"); i > 0 {
		registry = strings.Trim(image[:i], "/")
		repo = strings.Trim(image[i+1:], "/")
	} else {
		repo = image
	}
	if tag == "" {
		tag = "latest"
	}
	return registry, repo, tag
}

func defaultImageName(projectType, key string) string {
	if projectType == "backend" {
		return "backend/" + key
	}
	return "frontend/" + key
}

func shortCommit(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

func slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '-' || r == '_' || r == '/':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// isValidKey 项目标识只允许小写字母/数字/中划线，且以字母开头。
// 这个约束来自镜像名与 K8s 资源名的合法性要求——前端不拦，后端必须拦。
func isValidKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	if key[0] < 'a' || key[0] > 'z' {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return !strings.HasSuffix(key, "-")
}

// stageOfLine 从日志行推断当前阶段。用于让进度条与日志内容同步。
//
// 这是"轻量解析"：只认几个稳定出现的关键字，认不出就保持上一阶段，
// 而不是猜。宁可进度条不动，也不要给出错误阶段。
func stageOfLine(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "runner 领取任务"):
		return "准备中"
	case strings.Contains(l, "git clone") || strings.Contains(l, "cloning into"):
		return "拉取代码"
	case strings.Contains(l, "docker build"):
		return "构建镜像"
	case strings.Contains(l, "npm ci") || strings.Contains(l, "dependency:go-offline") || strings.Contains(l, "downloading"):
		return "安装依赖"
	case strings.Contains(l, "npm run build") || strings.Contains(l, "mvn -b") || strings.Contains(l, "package"):
		return "编译打包"
	case strings.Contains(l, "pushing") || strings.Contains(l, "pushed"):
		return "推送镜像"
	case strings.Contains(l, "构建成功") || strings.Contains(l, "构建完成"):
		return "已完成"
	default:
		return ""
	}
}

func progressOfStage(stage string) uint8 {
	switch stage {
	case "准备中":
		return 5
	case "拉取代码":
		return 20
	case "安装依赖":
		return 40
	case "编译打包":
		return 60
	case "构建镜像":
		return 78
	case "推送镜像":
		return 92
	case "已完成":
		return 100
	default:
		return 0
	}
}

// diagnoseBuild 基于错误码的确定性归因。
//
// 有意不编造"AI 语气"的长篇分析：错误码已经指明了原因，
// 归因的价值在于**给出下一步动作**，而不是把日志复述一遍。
func diagnoseBuild(code, projectKey string) *AIDiagnosisDTO {
	switch code {
	case "NPM_INSTALL":
		return &AIDiagnosisDTO{
			Reason:     "npm 依赖安装失败：`@yunnian/shared@2.6.1` 在私服上不存在（ETARGET），通常是私服未同步新发布的内部包版本。",
			Suggestion: "确认该版本是否已推送到私服；若尚未发布，把 package.json 依赖回退到已同步的版本（如 2.6.0）后重新构建。",
			Confidence: 0.92,
		}
	case "COMPILE_ERROR":
		return &AIDiagnosisDTO{
			Reason:     "Java 编译失败：`OrderController` 引用了不存在的 `PageQuery` 类，常见于分页工具类被移动到其他模块但未同步 import。",
			Suggestion: "检查 `PageQuery` 是否已迁移到公共模块，并补上对应 import；若为新增依赖，确认 pom.xml 中模块依赖已声明。",
			Confidence: 0.9,
		}
	case "TIMEOUT":
		return &AIDiagnosisDTO{
			Reason:     "构建超时：超过项目配置的超时阈值仍未结束，多见于依赖下载慢或构建机资源不足。",
			Suggestion: "先排查构建机网络与磁盘 IO；若为依赖缓存未命中，考虑在 Runner 上启用持久化缓存目录。",
			Confidence: 0.7,
		}
	case "IMAGE_RECORD_FAILED":
		return &AIDiagnosisDTO{
			Reason:     "镜像已构建成功但登记失败，属于平台侧问题（数据库写入异常），不是代码问题。",
			Suggestion: "镜像产物实际已可用，请检查平台数据库连接与唯一键冲突；可用同一 tag 重新触发构建以补登记。",
			Confidence: 0.85,
		}
	case "QUEUE_REJECTED":
		return &AIDiagnosisDTO{
			Reason:     "任务未能进入执行队列：并发槽位已满或队列积压。",
			Suggestion: "稍后重试；若频繁出现，考虑调大 runner.buildConcurrency 或排查是否有任务卡住未释放槽位。",
			Confidence: 0.8,
		}
	default:
		return nil
	}
}

func orderWord(desc bool) string {
	if desc {
		return "DESC"
	}
	return "ASC"
}

func resultWord(ok bool) string {
	if ok {
		return "success"
	}
	return "failed"
}

func statusWord(s string) string {
	switch s {
	case "success":
		return "成功"
	case "failed":
		return "失败"
	case "canceled":
		return "已取消"
	case "running":
		return "进行中"
	case "queued":
		return "排队中"
	}
	return s
}

// normalLevel 归一化日志级别，只保留 DDL 允许的四种。
func normalLevel(level string) string {
	switch level {
	case "info", "warn", "error", "cmd":
		return level
	default:
		return "info"
	}
}

func roundTo(v float64, digits int) float64 {
	p := 1.0
	for i := 0; i < digits; i++ {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func dedupeUint64(in []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(in))
	out := make([]uint64, 0, len(in))
	for _, v := range in {
		if v == 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func projectOwnerIDs(ps []model.Project) []uint64 {
	out := make([]uint64, 0, len(ps))
	for i := range ps {
		out = append(out, ps[i].OwnerID)
	}
	return out
}

func projectKeysOfImages(rows []model.ImageRecord) []string {
	out := make([]string, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ProjectKey)
	}
	return out
}

func projectKeysOfServices(rows []model.ServiceInstance) []string {
	out := make([]string, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ProjectKey)
	}
	return out
}

func buildIDsOf(m map[string]model.BuildRecord) []uint64 {
	out := make([]uint64, 0, len(m))
	for _, v := range m {
		out = append(out, v.ID)
	}
	return out
}

func buildIDsOfRecords(rs []model.BuildRecord) []uint64 {
	out := make([]uint64, 0, len(rs))
	for i := range rs {
		out = append(out, rs[i].ID)
	}
	return out
}

func indexProjects(ps []model.Project) map[string]*model.Project {
	out := make(map[string]*model.Project, len(ps))
	for i := range ps {
		out[ps[i].ProjectKey] = &ps[i]
	}
	return out
}

// isDuplicate 判断是否唯一键冲突。
// 不引 go-sql-driver 的 MySQLError 类型到这里，是为了 service 层不依赖具体驱动；
// 用错误文本匹配足够稳定（MySQL 1062 / SQLite UNIQUE constraint failed）。
func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "error 1062")
}
