package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/logstream"
	"github.com/2777484478/yunnian/internal/model"
	"github.com/2777484478/yunnian/internal/queue"
)

// CDService 持续部署：环境、服务实例、部署单、回滚。
type CDService struct {
	*base
	// sys 复用系统服务的「默认模型通道」解析（密钥解密、超时兜底、"未配置通道"的提示文案）。
	// 与 CIService / AIService 同一个理由：多一份实现就多一个漏改点。
	// 部署失败的「AI 诊断」要走它。
	sys *SystemService
}

// --------------------------------------------------------------------------- 入参

// EnvironmentQuery 环境列表查询。
type EnvironmentQuery struct {
	Keyword  string
	Status   string
	Enabled  *bool
	Page     int
	PageSize int
	SortBy   string
	Order    string
}

// EnvironmentInput 环境新建/编辑入参。
type EnvironmentInput struct {
	Key            string `json:"key" binding:"omitempty,max=64"`
	Name           string `json:"name" binding:"required,max=64"`
	Cluster        string `json:"cluster" binding:"required,max=128"`
	ClusterVersion string `json:"clusterVersion" binding:"omitempty,max=32"`
	Namespace      string `json:"namespace" binding:"required,max=128"`
	// DeployMethod 收窄成只认 "k8s"：平台没有别的部署方式 —— 改集群一律写 AppRelease CR，
	// 由 operator 下发到 Deployment。此前这里是 `oneof=k8s helm compose script`，
	// 但选了 helm/compose 行为完全一样，是个假选项（前端已不再提供选择、也不再提交本字段）。
	//
	// 字段本身留着，是因为下面两处映射还要用它兜底：
	// 新建时 `stringOr(in.DeployMethod, "k8s")`、编辑时 `stringOr(in.DeployMethod, e.DeployMethod)`
	// —— 不传即"没提这件事"，编辑不会把库里可能残留的历史值顺手改掉。
	DeployMethod  string   `json:"deployMethod" binding:"omitempty,oneof=k8s"`
	Registry      string   `json:"registry" binding:"omitempty,max=128"`
	IngressDomain string   `json:"ingressDomain" binding:"omitempty,max=191"`
	NeedApproval  *bool    `json:"needApproval"`
	Approvers     []string `json:"approvers"`
	Owner         string   `json:"owner"`
	Description   string   `json:"description" binding:"omitempty,max=512"`
	ProjectKeys   []string `json:"projectKeys"`
	Enabled       *bool    `json:"enabled"`

	// 发布阶段与链路序号：环境自带（见 model.Environment 的 StageLabel/StageOrder）。
	// StageOrder 为指针，nil 表示"没设"，与 0 区分开。
	StageLabel string `json:"stageLabel" binding:"omitempty,max=32"`
	StageOrder *int   `json:"stageOrder" binding:"omitempty,min=1,max=99"`

	// 集群凭据来源：platform（默认）= 引用「系统管理 › 集群接入」里已接入集群的凭据；
	// own = 该环境自带一份 kubeconfig，最小化后单独加密挂在这个环境上
	// （用上 environment.kubeconfig_credential_id 那一列）。
	//
	// 之所以允许两种：用户口径是「两者都允许」——既可以从平台已接入的集群里选，
	// 也要能接临时集群、别人托管的集群（那些没法走平台统一接入）。
	ClusterSource string `json:"clusterSource" binding:"omitempty,oneof=platform own"`
	Kubeconfig    string `json:"kubeconfig"`
}

// EnvVarsInput 环境变量覆盖式保存入参。
type EnvVarsInput struct {
	Vars []EnvVarDTO `json:"vars"`
}

// EnvServiceQuery 环境内服务查询。
type EnvServiceQuery struct {
	Status  string
	Type    string
	Keyword string
}

// DeploymentQuery 部署列表查询。
//
// 同一个结构体被 ListDeployments 与 ListRollbacks 复用（两者筛选维度高度重合）。
// TriggerType 只对回滚列表有意义——deployment 表没有这个字段，ListDeployments 会忽略它。
type DeploymentQuery struct {
	EnvKey     string
	ProjectKey string
	Status     string
	// TriggerType 回滚触发方式：manual / ai_suggested
	TriggerType string
	Keyword     string
	Page        int
	PageSize    int
	SortBy      string
	Order       string
}

// CreateDeploymentInput 创建部署单入参。
type CreateDeploymentInput struct {
	EnvKey     string `json:"envKey" binding:"required"`
	ProjectKey string `json:"projectKey" binding:"required"`
	// ImageSource 镜像来源，**二选一**：
	//
	//	platform（默认）用 ImageRecordID / Tag 从 image_record 里找（平台自己构建过的）
	//	registry        用 RegistryTag 去镜像仓库取（仓库里实际存在的，含别人推的）
	//
	// 两个来源**都要保留、只能选一个**（用户 2026-10-14 明确要求）。都选、都不选都由
	// CreateDeployment 拒绝 —— 让"发的是哪一份"永远只有一个答案。
	ImageSource   string `json:"imageSource" binding:"omitempty,oneof=platform registry"`
	ImageRecordID string `json:"imageRecordId"`
	Tag           string `json:"tag"`
	// RegistryTag 仓库里的标签名（ImageSource=registry 时必填）。
	RegistryTag string `json:"registryTag" binding:"omitempty,max=191"`
	// ImageOverride 只给**平台内部**复用（重新部署 / 重试）用：把上一单实际用的镜像
	// 原样再来一次。刻意 `json:"-"` —— 它是不受项目校验约束的完整镜像串，
	// 不能让请求体传得进来。内部路径拿到的值都出自本平台的部署记录，是可信的。
	//
	// 为什么必须有它：仓库标签来源的部署 `image_record_id = 0`，若只靠 ImageRecordID
	// 传下去，重试会退化成"取该项目最新镜像"= **静默换版本**（实测这是原有行为）。
	ImageOverride  string `json:"-"`
	Replicas       *int   `json:"replicas"`
	Strategy       string `json:"strategy" binding:"omitempty,oneof=RollingUpdate Recreate"`
	Remark         string `json:"remark" binding:"omitempty,max=512"`
	IdempotencyKey string `json:"idempotencyKey" binding:"omitempty,max=128"`
}

// ServiceParamsInput 服务参数（副本/资源/环境变量/ConfigMap/Secret）入参。
type ServiceParamsInput struct {
	Replicas   *int        `json:"replicas"`
	CPURequest string      `json:"cpuRequest" binding:"omitempty,max=16"`
	CPULimit   string      `json:"cpuLimit" binding:"omitempty,max=16"`
	MemRequest string      `json:"memRequest" binding:"omitempty,max=16"`
	MemLimit   string      `json:"memLimit" binding:"omitempty,max=16"`
	HPAEnabled *bool       `json:"hpaEnabled"`
	EnvVars    []EnvVarDTO `json:"envVars"`
	ConfigMaps []string    `json:"configMaps"`
	Secrets    []string    `json:"secrets"`
	Note       string      `json:"note" binding:"omitempty,max=255"`
}

// CreateRollbackInput 执行回滚入参。
type CreateRollbackInput struct {
	ServiceID   string   `json:"serviceId" binding:"required"`
	ToImageID   string   `json:"toImageId"`
	ToImage     string   `json:"toImage"`
	Reason      string   `json:"reason" binding:"required,max=512"`
	TriggerType string   `json:"triggerType" binding:"omitempty,oneof=manual ai_suggested"`
	Confidence  *float64 `json:"confidence"`
}

// --------------------------------------------------------------------------- 环境

var envSortColumns = map[string]string{
	"name":      "name",
	"key":       "env_key",
	"createdAt": "created_at",
	"updatedAt": "updated_at",
	"cpuUsage":  "cpu_usage",
	"memUsage":  "mem_usage",
}

// ListEnvironments 环境列表。
func (s *CDService) ListEnvironments(ctx context.Context, q EnvironmentQuery) (*httpx.PageQuery, any, int64, error) {
	pq := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize, SortBy: q.SortBy, Order: q.Order, Keyword: q.Keyword}
	column, desc := pq.Normalize(envSortColumns, "created_at")

	db := s.db(ctx).Model(&model.Environment{})
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("name LIKE ? OR env_key LIKE ? OR namespace LIKE ?", like, like, like)
	}
	if st := strings.TrimSpace(q.Status); st != "" {
		db = db.Where("health_status = ?", st)
	}
	if q.Enabled != nil {
		db = db.Where("enabled = ?", *q.Enabled)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "统计环境数量失败")
	}
	var rows []model.Environment
	if err := db.Order(column + " " + orderWord(desc)).
		Offset(pq.Offset()).Limit(pq.PageSize).Find(&rows).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "查询环境失败")
	}
	if len(rows) == 0 {
		return &pq, []EnvironmentDTO{}, total, nil
	}

	ids := make([]uint64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	ownerNames, err := s.userNames(ctx, envOwnerIDs(rows))
	if err != nil {
		return nil, nil, 0, err
	}
	projectKeysByEnv, err := s.projectsOfEnvs(ctx, ids)
	if err != nil {
		return nil, nil, 0, err
	}
	varsByEnv, err := s.varsOfEnvs(ctx, ids)
	if err != nil {
		return nil, nil, 0, err
	}
	approversByEnv, err := s.envApprovers(ctx, rows)
	if err != nil {
		return nil, nil, 0, err
	}

	out := make([]EnvironmentDTO, 0, len(rows))
	for i := range rows {
		e := &rows[i]
		out = append(out, envToDTO(e, ownerNames[e.OwnerID],
			approversByEnv[e.ID], projectKeysByEnv[e.ID], varsByEnv[e.ID]))
	}
	return &pq, out, total, nil
}

// GetEnvironment 环境详情。
func (s *CDService) GetEnvironment(ctx context.Context, key string) (*EnvironmentDTO, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	ownerNames, err := s.userNames(ctx, []uint64{e.OwnerID})
	if err != nil {
		return nil, err
	}
	projectKeysByEnv, err := s.projectsOfEnvs(ctx, []uint64{e.ID})
	if err != nil {
		return nil, err
	}
	varsByEnv, err := s.varsOfEnvs(ctx, []uint64{e.ID})
	if err != nil {
		return nil, err
	}
	approversByEnv, err := s.envApprovers(ctx, []model.Environment{*e})
	if err != nil {
		return nil, err
	}
	dto := envToDTO(e, ownerNames[e.OwnerID], approversByEnv[e.ID], projectKeysByEnv[e.ID], varsByEnv[e.ID])
	return &dto, nil
}

// CreateEnvironment 新建环境。
func (s *CDService) CreateEnvironment(ctx context.Context, in EnvironmentInput, c Caller) (*EnvironmentDTO, error) {
	key := strings.TrimSpace(in.Key)
	if key == "" {
		key = slugify(in.Name)
	}
	if !isValidKey(key) {
		// 消息必须写全约束：只说"只允许小写字母、数字与中划线"会让用户拿 `9abc`
		// 反复试 —— 真正的拒绝理由是**必须以字母开头**。
		return nil, errs.Invalidf("环境标识 %q 不合法：必须以小写字母开头，且只含小写字母、数字与中划线", key)
	}
	ownerID, err := s.resolveOwner(ctx, in.Owner, c)
	if err != nil {
		return nil, err
	}
	approverIDs, err := s.resolveUsers(ctx, in.Approvers)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(approverIDs)

	// 自带 kubeconfig 的环境：先按 current-context **最小化**，再加密落库。
	// 顺序不能反 —— 若先把原文探测通过、却存下另一个 context，
	// "这个环境能用"的结论就与最终存下的凭据无关了。
	if strings.EqualFold(strings.TrimSpace(in.ClusterSource), "own") && strings.TrimSpace(in.Kubeconfig) == "" {
		return nil, errs.Invalid("选择「独立 kubeconfig」时必须提供 kubeconfig 内容")
	}
	minimal, err := envOwnKubeconfig(in)
	if err != nil {
		return nil, err
	}

	e := model.Environment{
		EnvKey:        key,
		Name:          strings.TrimSpace(in.Name),
		Cluster:       strings.TrimSpace(in.Cluster),
		ClusterVer:    in.ClusterVersion,
		Namespace:     strings.TrimSpace(in.Namespace),
		DeployMethod:  stringOr(in.DeployMethod, "k8s"),
		Registry:      stringOr(strings.TrimSpace(in.Registry), s.defaultRegistry()),
		IngressDomain: in.IngressDomain,
		StageLabel:    strings.TrimSpace(in.StageLabel),
		StageOrder:    in.StageOrder,
		NeedApproval:  in.NeedApproval != nil && *in.NeedApproval,
		ApproverIDs:   model.JSONText(raw),
		OwnerID:       ownerID,
		Description:   in.Description,
		Enabled:       in.Enabled == nil || *in.Enabled,
		HealthStatus:  "unknown",
		CreatedBy:     c.UserID(),
		UpdatedBy:     c.UserID(),
	}
	if e.NeedApproval && len(approverIDs) == 0 {
		return nil, errs.Invalid("需要审批的环境至少要指定一名审批人")
	}

	// 封凭据与建行放同一个事务：否则会出现"环境建出来了、凭据没存上"的半成品，
	// 而界面上那个环境看起来一切正常。
	err = s.db(ctx).Transaction(func(tx *gorm.DB) error {
		if minimal != "" {
			credID, cerr := sealKubeconfigCredential(tx, s.deps.Sealer, envCredScope, key, minimal, e.Name, c.UserID())
			if cerr != nil {
				return cerr
			}
			e.KubeCredID = credID
		}
		if cerr := tx.Create(&e).Error; cerr != nil {
			if isDuplicate(cerr) {
				return s.reviveEnvironment(tx, &e)
			}
			return errs.Internalf(cerr, "创建环境失败")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := s.linkEnvProjects(ctx, e.ID, in.ProjectKeys); err != nil {
		return nil, err
	}

	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "env:create", TargetType: "environment",
		TargetID: &e.ID, TargetName: e.Name,
		Detail: fmt.Sprintf("新建环境 %s（%s，集群 %s）", e.EnvKey, e.Name, e.Cluster),
	})
	return s.GetEnvironment(ctx, key)
}

// envOwnKubeconfig 处理环境自带的 kubeconfig：按 current-context 最小化后返回。
// 返回空串表示"本次不设置凭据"（没选 own，或编辑时没有重新粘贴）。
func envOwnKubeconfig(in EnvironmentInput) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(in.ClusterSource), "own") {
		return "", nil
	}
	raw := strings.TrimSpace(in.Kubeconfig)
	if raw == "" {
		return "", nil
	}
	parsed, err := integration.ParseKubeconfig(raw)
	if err != nil {
		return "", errs.Invalid("kubeconfig 解析失败：" + err.Error())
	}
	ctxName := parsed.CurrentContext
	if ctxName == "" && len(parsed.Contexts) > 0 {
		ctxName = parsed.Contexts[0].Context
	}
	if ctxName == "" {
		return "", errs.Invalid("kubeconfig 里没有任何 context，无法确定要用哪份凭据")
	}
	minimal, err := integration.MinimalKubeconfig(raw, ctxName)
	if err != nil {
		return "", errs.Invalid("kubeconfig 处理失败：" + err.Error())
	}
	return minimal, nil
}

// reviveEnvironment 捞回同标识的软删行复活。
//
// `uk_env_key` 只含 `env_key`、**不含 `deleted_at`**，所以列表里看不到的行仍占着标识：
// 用户删掉环境后想用同一个标识重建，会撞 40900「环境标识已被占用」。
// 这里是复用那一行而不是让用户换个标识 —— 界面说"已删除"，用户不该为此改设计。
func (s *CDService) reviveEnvironment(tx *gorm.DB, e *model.Environment) error {
	var old model.Environment
	if err := tx.Unscoped().Where("env_key = ?", e.EnvKey).Order("id DESC").First(&old).Error; err != nil {
		return errs.Conflict("环境标识已被占用：" + e.EnvKey)
	}
	// **必须判它是不是软删的**：`Unscoped` 把软删与活跃行一起捞出来，
	// 而唯一索引冲突也可能来自一个**活跃**的同名环境（真并发插入，或同一请求重放）。
	// 少了这一判，新建一个与现存环境同标识的环境会把那个环境**原地改写成新配置**
	// 并回「环境已创建」—— 用户以为只是"撞了个标识"，实际是另一个环境被悄悄换了配置。
	// （同一处缺陷在 `reviveProject` 里被端到端验证抓到，这里是同源的第二处。）
	if !old.DeletedAt.Valid {
		return errs.Conflict("环境标识已被占用：" + e.EnvKey)
	}
	updates := map[string]any{
		"deleted_at":               nil,
		"name":                     e.Name,
		"cluster":                  e.Cluster,
		"cluster_version":          e.ClusterVer,
		"namespace":                e.Namespace,
		"deploy_method":            e.DeployMethod,
		"registry":                 e.Registry,
		"ingress_domain":           e.IngressDomain,
		"stage_label":              e.StageLabel,
		"stage_order":              e.StageOrder,
		"need_approval":            e.NeedApproval,
		"approver_ids":             e.ApproverIDs,
		"owner_id":                 e.OwnerID,
		"description":              e.Description,
		"enabled":                  e.Enabled,
		"kubeconfig_credential_id": e.KubeCredID,
		"health_status":            "unknown",
		"updated_by":               e.UpdatedBy,
		"updated_at":               now(),
	}
	if err := tx.Unscoped().Model(&model.Environment{}).Where("id = ?", old.ID).Updates(updates).Error; err != nil {
		return errs.Internalf(err, "复用已删除的环境失败")
	}
	e.ID = old.ID
	return nil
}

// UpdateEnvironment 编辑环境。
func (s *CDService) UpdateEnvironment(ctx context.Context, key string, in EnvironmentInput, c Caller) (*EnvironmentDTO, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	ownerID := e.OwnerID
	if strings.TrimSpace(in.Owner) != "" {
		if ownerID, err = s.resolveOwner(ctx, in.Owner, c); err != nil {
			return nil, err
		}
	}
	updates := map[string]any{
		"name":            strings.TrimSpace(in.Name),
		"cluster":         strings.TrimSpace(in.Cluster),
		"cluster_version": in.ClusterVersion,
		"namespace":       strings.TrimSpace(in.Namespace),
		"deploy_method":   stringOr(in.DeployMethod, e.DeployMethod),
		"registry":        stringOr(strings.TrimSpace(in.Registry), e.Registry),
		"ingress_domain":  in.IngressDomain,
		"stage_label":     strings.TrimSpace(in.StageLabel),
		"stage_order":     in.StageOrder,
		"owner_id":        ownerID,
		"description":     in.Description,
		"updated_by":      c.UserID(),
	}

	// 审批人：**没提就不覆盖**（`nil` = 没提；空切片 = 明确清空）。
	//
	// 原先是无条件 `resolveUsers(in.Approvers)` 之后写回 `approver_ids`：
	// 调用方没带这个字段时，解析结果为空 → 写进 `[]` → 已有审批人被整批清掉。
	// 这不是理论风险：`UpdateEnvironment` 不是部分更新，任何一次编辑（改个备注也算）
	// 都会走到这里。而审批人一旦被清空，环境如果还开着"需要审批"，
	// 部署单就会卡在没有人能确认的状态。
	approverCount := countIDs(e.ApproverIDs)
	if in.Approvers != nil {
		ids, uerr := s.resolveUsers(ctx, in.Approvers)
		if uerr != nil {
			return nil, uerr
		}
		raw, _ := json.Marshal(ids)
		updates["approver_ids"] = model.JSONText(raw)
		approverCount = len(ids)
	}

	// "需要审批"的校验必须看**改完之后**的状态：
	// 只看本次传进来的那批，会漏掉"这次没传审批人、但库里本来就没有"的情况。
	needApproval := e.NeedApproval
	if in.NeedApproval != nil {
		needApproval = *in.NeedApproval
		updates["need_approval"] = needApproval
	}
	if needApproval && approverCount == 0 {
		return nil, errs.Invalid("需要审批的环境至少要指定一名审批人")
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}

	// 凭据变更必须与主更新同一个事务：换 kubeconfig 时"新凭据已加密 + 环境已指向它"
	// 要原子完成，否则中途失败会留下一个指向旧凭据（或悬空）的环境。
	//
	// 注意 ClusterSource 只认显式的 "own" / "platform"：空串表示调用方没提这件事，
	// 此时**不动凭据**——把默认值当"清空"，会让每次改个备注就顺手删掉凭据。
	var minimal string
	if in.ClusterSource == "own" {
		m, err := envOwnKubeconfig(in)
		if err != nil {
			return nil, err
		}
		minimal = m
	}
	err = s.db(ctx).Transaction(func(tx *gorm.DB) error {
		switch in.ClusterSource {
		case "own":
			if minimal != "" {
				credID, cerr := sealKubeconfigCredential(tx, s.deps.Sealer, envCredScope, key, minimal, e.Name, c.UserID())
				if cerr != nil {
					return cerr
				}
				updates["kubeconfig_credential_id"] = credID
			}
		case "platform":
			// 明确切回"引用平台已接入集群的凭据"：解绑自带的那份
			updates["kubeconfig_credential_id"] = nil
		}
		return tx.Model(&model.Environment{}).Where("id = ?", e.ID).Updates(updates).Error
	})
	if err != nil {
		return nil, errs.Internalf(err, "更新环境失败")
	}
	if in.ProjectKeys != nil {
		if err := s.replaceEnvProjects(ctx, e.ID, in.ProjectKeys); err != nil {
			return nil, err
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "env:update", TargetType: "environment",
		TargetID: &e.ID, TargetName: e.Name,
		Detail: fmt.Sprintf("编辑环境 %s（%s）", e.EnvKey, e.Name),
	})
	return s.GetEnvironment(ctx, key)
}

// DeleteEnvironment 删除环境。有服务在跑时拒绝——环境一删，
// 里面的服务实例就失去归属，后续无法在平台上操作它们（集群里还活着），
// 这种情况比"删不掉"更麻烦。
func (s *CDService) DeleteEnvironment(ctx context.Context, key string, c Caller) error {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return err
	}
	var n int64
	if err := s.db(ctx).Model(&model.ServiceInstance{}).Where("env_id = ?", e.ID).Count(&n).Error; err != nil {
		return errs.Internalf(err, "检查环境内服务失败")
	}
	if n > 0 {
		return errs.StateConflict(fmt.Sprintf("该环境下还有 %d 个服务实例，请先下线服务再删除环境", n))
	}
	// 环境自带的 kubeconfig 是环境私有的：环境没了它就再无引用方，
	// 留在库里只是一条谁也够不着的密文（还占着 scope+name 的唯一键）。
	// 与软删环境放同一个事务，避免"环境没了、密文还在"。
	err = s.db(ctx).Transaction(func(tx *gorm.DB) error {
		if e.KubeCredID != nil {
			if derr := tx.Where("id = ?", *e.KubeCredID).Delete(&model.SecretCredential{}).Error; derr != nil {
				return derr
			}
		}
		return tx.Delete(&model.Environment{}, e.ID).Error
	})
	if err != nil {
		return errs.Internalf(err, "删除环境失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "env:delete", TargetType: "environment",
		TargetID: &e.ID, TargetName: e.Name, Detail: "删除环境 " + e.EnvKey,
	})
	return nil
}

// ProjectCandidate 可关联的项目。
type ProjectCandidate struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Team     string `json:"team"`
	Linked   bool   `json:"linked"`
	HasBuild bool   `json:"hasBuild"`
}

// EnvProjectCandidates 环境可关联的项目列表（一次查完，避免前端 N+1）。
func (s *CDService) EnvProjectCandidates(ctx context.Context, key string) ([]ProjectCandidate, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	var projects []model.Project
	if err := s.db(ctx).Where("enabled = ?", true).Order("type, name").Find(&projects).Error; err != nil {
		return nil, errs.Internalf(err, "查询项目失败")
	}
	linked, err := s.projectsOfEnvs(ctx, []uint64{e.ID})
	if err != nil {
		return nil, err
	}
	linkedSet := map[string]bool{}
	for _, k := range linked[e.ID] {
		linkedSet[k] = true
	}

	// 有成功构建产物的项目，才能真的部署——列表里标出来，
	// 避免用户选了没镜像的项目然后困惑"为什么部署失败"。
	var built []string
	if err := s.db(ctx).Model(&model.ImageRecord{}).
		Distinct("project_key").Pluck("project_key", &built).Error; err != nil {
		return nil, errs.Internalf(err, "查询已构建项目失败")
	}
	builtSet := map[string]bool{}
	for _, k := range built {
		builtSet[k] = true
	}

	out := make([]ProjectCandidate, 0, len(projects))
	for i := range projects {
		out = append(out, ProjectCandidate{
			Key:      projects[i].ProjectKey,
			Name:     projects[i].Name,
			Type:     projects[i].Type,
			Team:     projects[i].Team,
			Linked:   linkedSet[projects[i].ProjectKey],
			HasBuild: builtSet[projects[i].ProjectKey],
		})
	}
	return out, nil
}

// EnvProjectsInput 纳管项目的覆盖式入参：请求体就是"保存后该环境纳管哪些项目"。
type EnvProjectsInput struct {
	ProjectKeys []string `json:"projectKeys"`
}

// SetEnvProjects 覆盖式设置环境纳管的项目。
//
// 为什么单开一个接口，而不是让前端复用 PUT /environments/{key}：
// 那个接口是**整实体覆盖**——`name` / `cluster` / `namespace` / `approver_ids`
// 都是直接写（见 UpdateEnvironment），只带 projectKeys 去打它会把一堆字段清空。
// 纳管是与环境配置正交的一件事，就该有自己的入口，与 PUT /environments/{key}/vars
// 同一个口径（覆盖式、路径里带 key、返回保存后的完整集合）。
//
// 语义与编辑抽屉**完全一致**（共用 replaceEnvProjects）：新增的项目会补建服务实例；
// **取消勾选的项目会连同它的服务实例一起删掉**（集群里的负载不会动，平台不再跟踪）。
func (s *CDService) SetEnvProjects(ctx context.Context, key string, in EnvProjectsInput, c Caller) ([]ProjectCandidate, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := s.replaceEnvProjects(ctx, e.ID, in.ProjectKeys); err != nil {
		return nil, err
	}
	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "env:link_projects", TargetType: "environment",
		TargetID: &e.ID, TargetName: e.Name,
		Detail: fmt.Sprintf("纳管项目 %s（%s）：%s", e.EnvKey, e.Name, strings.Join(in.ProjectKeys, "、")),
	})
	return s.EnvProjectCandidates(ctx, key)
}

// EnvVars 读取环境变量（密文脱敏）。
func (s *CDService) EnvVars(ctx context.Context, key string) ([]EnvVarDTO, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	var rows []model.EnvironmentVar
	if err := s.db(ctx).Where("env_id = ?", e.ID).Order("var_key").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询环境变量失败")
	}
	out := make([]EnvVarDTO, 0, len(rows))
	for i := range rows {
		v := rows[i].VarValue
		if rows[i].IsSecret {
			v = maskValue(v)
		}
		out = append(out, EnvVarDTO{Key: rows[i].VarKey, Value: v, Secret: rows[i].IsSecret})
	}
	return out, nil
}

// SaveEnvVars 覆盖式保存环境变量。
//
// 密文项的特殊处理：前端回传的是掩码（******），不能把掩码当新值存进去——
// 那会把真实密钥覆盖掉且无法恢复。这里遇到掩码就沿用库里的旧值。
func (s *CDService) SaveEnvVars(ctx context.Context, key string, in EnvVarsInput, c Caller) ([]EnvVarDTO, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}

	var existing []model.EnvironmentVar
	if err := s.db(ctx).Where("env_id = ?", e.ID).Find(&existing).Error; err != nil {
		return nil, errs.Internalf(err, "查询环境变量失败")
	}
	old := make(map[string]model.EnvironmentVar, len(existing))
	for i := range existing {
		old[existing[i].VarKey] = existing[i]
	}

	seen := map[string]bool{}
	err = s.tx(ctx, func(tx *gorm.DB) error {
		for _, v := range in.Vars {
			key := strings.TrimSpace(v.Key)
			if key == "" {
				continue
			}
			seen[key] = true
			value := v.Value
			if v.Secret && (value == "" || value == maskValue("x")) {
				if prev, ok := old[key]; ok && prev.IsSecret {
					value = prev.VarValue // 沿用旧密钥
				}
			}
			if prev, ok := old[key]; ok {
				if err := tx.Model(&model.EnvironmentVar{}).Where("id = ?", prev.ID).
					Updates(map[string]any{"var_value": value, "is_secret": v.Secret}).Error; err != nil {
					return err
				}
				continue
			}
			if err := tx.Create(&model.EnvironmentVar{
				EnvID: e.ID, VarKey: key, VarValue: value, IsSecret: v.Secret,
			}).Error; err != nil {
				return err
			}
		}
		// 覆盖式语义：没出现在入参里的变量要删掉，否则前端"删掉一行"不会生效。
		for k, prev := range old {
			if seen[k] {
				continue
			}
			if err := tx.Delete(&model.EnvironmentVar{}, prev.ID).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, wrapDB(err, "保存环境变量")
	}

	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "env:vars", TargetType: "environment",
		TargetID: &e.ID, TargetName: e.Name,
		Detail: fmt.Sprintf("保存环境变量 %s，共 %d 项", e.EnvKey, len(in.Vars)),
	})
	return s.EnvVars(ctx, key)
}

// EnvServices 环境内服务列表（CD 主视图「发布拓扑」的数据源）。
func (s *CDService) EnvServices(ctx context.Context, key string, q EnvServiceQuery) ([]EnvServiceDTO, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	db := s.db(ctx).Where("env_id = ?", e.ID)
	// 状态**不在 SQL 里过滤**：界面显示的是集群实时状态，用库里那份
	// （纳管时写死、之后只有部署成功才更新）来筛，会出现"筛'运行中'，
	// 结果列表里写着 failed"。改为读完之后按实时状态过滤，见下方。
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		db = db.Where("project_key LIKE ?", "%"+kw+"%")
	}
	var rows []model.ServiceInstance
	if err := db.Order("project_key").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询环境内服务失败")
	}
	if len(rows) == 0 {
		return []EnvServiceDTO{}, nil
	}

	projs, err := s.projectsByKeys(ctx, projectKeysOfServices(rows))
	if err != nil {
		return nil, err
	}
	if t := strings.TrimSpace(q.Type); t != "" {
		filtered := rows[:0]
		for i := range rows {
			if p := projs[rows[i].ProjectKey]; p != nil && p.Type == t {
				filtered = append(filtered, rows[i])
			}
		}
		rows = filtered
	}

	updaterNames, err := s.userNames(ctx, serviceUpdaterIDs(rows))
	if err != nil {
		return nil, err
	}
	// 每个圆圈的状态必须来自集群，不能来自库里那份"纳管时按默认值写死"的记录。
	// Pod 明细与版本历史仍然不查：那会把一次列表请求变成 3N 次下游调用。
	liveMap := s.liveStates(ctx, e, rows)

	out := make([]EnvServiceDTO, 0, len(rows))
	for i := range rows {
		out = append(out, serviceToDTO(&rows[i], projs[rows[i].ProjectKey],
			updaterNames[derefUint64(rows[i].UpdatedBy)], nil, nil, liveMap[rows[i].ID]))
	}
	if st := strings.TrimSpace(q.Status); st != "" {
		kept := out[:0]
		for i := range out {
			if statusForFilter(&out[i]) == st {
				kept = append(kept, out[i])
			}
		}
		out = kept
	}
	return out, nil
}

// statusForFilter 取用于筛选的状态词：优先实时状态，读不到才退回库里的值。
func statusForFilter(d *EnvServiceDTO) string {
	if d.Live != nil && d.Live.Available && d.Live.Exists {
		return d.Live.Status
	}
	return d.Status
}

// liveStates 并发读各服务在集群里的实时状态。
//
// 为什么并发：一个环境纳管十几个服务时串行读就是十几次集群往返，列表页会肉眼可见地卡住。
// 并发上限压在 6，避免把集群 API 打出限流。
//
// 为什么超时不报错：**列表页不能因为集群慢就整体打不开**。读不到的服务其 live
// 会带上原因，界面据此显示"读取失败"，其余照常显示——这比整页报错有用得多。
func (s *CDService) liveStates(ctx context.Context, env *model.Environment, svcs []model.ServiceInstance) map[uint64]*ServiceLiveDTO {
	out := make(map[uint64]*ServiceLiveDTO, len(svcs))
	if len(svcs) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	// 用量**先批量取一次**：整个命名空间的 Deployment / Pod / 指标各查一遍，
	// 然后在下面的并发循环里按名字取用。绝不能改成在循环里各查一次 ——
	// 那会变成 N 倍的下游请求，而这几个列表本来就是整个命名空间一份。
	//
	// 取不到就当没有（每项 Available=false，界面显示 "—"），不让它把"状态"一起拖坏：
	// 负载列看不到是遗憾，状态看不到是故障。
	names := make([]string, 0, len(svcs))
	for i := range svcs {
		if w := strings.TrimSpace(svcs[i].WorkloadName); w != "" {
			names = append(names, w)
		}
	}
	usages, uerr := s.deps.Up.Cluster.WorkloadUsages(ctx, clusterRefOf(env), env.Namespace, names)
	if uerr != nil {
		s.log("cd").Warn("读取服务实时用量失败，负载列将显示为 —", "env", env.EnvKey, "err", uerr)
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, 6)
	)
	for i := range svcs {
		wg.Add(1)
		go func(svc model.ServiceInstance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			state, err := s.deps.Up.Cluster.Describe(ctx, workloadRefOf(env, svc.WorkloadName))
			dto := liveOf(state, usages[svc.WorkloadName], err)
			mu.Lock()
			out[svc.ID] = dto
			mu.Unlock()
		}(svcs[i])
	}
	wg.Wait()
	return out
}

// clusterRefOf 构造集群引用。
//
// EnvID **只在环境自带 kubeconfig 时**才填。它一旦非 nil，凭据解析就绕过平台集群表、
// 直接用环境自己那份；若不看情况地一律填上，引用平台集群的环境会全部变成
// "找不到自带凭据"而失败。反之若一律不填，自带凭据的环境会被拿平台集群的凭据去读——
// 拿到的是另一个集群的数据，而且看不出错。两种都错，所以这里必须看环境的实际形态。
func clusterRefOf(e *model.Environment) integration.ClusterRef {
	ref := integration.ClusterRef{Cluster: e.Cluster}
	if e.KubeCredID != nil {
		id := e.ID
		ref.EnvID = &id
	}
	return ref
}

// workloadRefOf 构造工作负载引用。
func workloadRefOf(e *model.Environment, workload string) integration.WorkloadRef {
	return integration.WorkloadRef{
		ClusterRef: clusterRefOf(e),
		Namespace:  e.Namespace,
		Workload:   workload,
	}
}

// EnvResources 集群资源用量。
func (s *CDService) EnvResources(ctx context.Context, key string) (*integration.ClusterState, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	state, err := s.deps.Up.Cluster.Nodes(ctx, clusterRefOf(e))
	if err != nil {
		return nil, upstreamError(err, "查询集群 %s 资源失败", e.Cluster)
	}
	return state, nil
}

// SyncEnvironment 主动同步集群状态。
//
// 为什么需要手动同步：平台不可能每秒去轮询每个集群（成本高、也被集群限流），
// 所以这里写下的 `health_status` 是"上次同步时的**资源水位快照**"。
// 它**只反映该集群节点的 CPU / 内存用量**（下面那两条阈值），不是服务健康；
// 也正因如此，用户想立刻看准确值时用这个接口 —— 而这个快照没有任何定时刷新，
// 顶栏那句「N 个环境资源紧张」可能来自几小时前，看之前先想一下它有多旧。
func (s *CDService) SyncEnvironment(ctx context.Context, key string, c Caller) (*EnvironmentDTO, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}

	state, err := s.deps.Up.Cluster.Nodes(ctx, clusterRefOf(e))
	if err != nil {
		s.audit(ctx, c, auditInput{
			Module: "CD", Action: "env:sync", TargetType: "environment",
			TargetID: &e.ID, TargetName: e.Name,
			Detail: "同步集群状态失败", Result: "failed", ErrMsg: err.Error(),
		})
		return nil, upstreamError(err, "同步环境 %s 失败", e.EnvKey)
	}

	at := now()
	// 健康判定**必须先看用量是否真的读到了**。
	// metrics-server 缺失或凭据无权读时 CPUUsage/MemUsage 都是 0，
	// 直接套用阈值会得出一句"healthy"——那是拿"没读到"冒充"一切正常"。
	health := "unknown"
	if state.MetricsAvailable {
		health = "healthy"
		switch {
		case state.CPUUsage >= 90 || state.MemUsage >= 90:
			health = "error"
		case state.CPUUsage >= 75 || state.MemUsage >= 75:
			health = "warning"
		}
	}

	if err := s.db(ctx).Model(&model.Environment{}).Where("id = ?", e.ID).Updates(map[string]any{
		"cpu_usage": state.CPUUsage, "mem_usage": state.MemUsage,
		"node_count": state.Nodes, "health_status": health,
		"cluster_version": stringOr(state.Version, e.ClusterVer),
		"last_sync_at":    &at, "updated_at": at,
	}).Error; err != nil {
		return nil, errs.Internalf(err, "更新环境状态失败")
	}

	// 顺带把服务实例的真实副本数同步回来：集群里有人手动改过副本数时，
	// 平台视图会与集群不一致，这是最常见的一类"数据对不上"投诉。
	s.syncServiceReplicas(ctx, e)

	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "env:sync", TargetType: "environment",
		TargetID: &e.ID, TargetName: e.Name,
		Detail: fmt.Sprintf("同步环境 %s：CPU %d%%、内存 %d%%、节点 %d",
			e.EnvKey, state.CPUUsage, state.MemUsage, state.Nodes),
	})
	return s.GetEnvironment(ctx, key)
}

// EnvEvent 集群事件。
type EnvEvent struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Object  string `json:"object"`
	Message string `json:"message"`
	Count   int    `json:"count"`
	At      string `json:"at"`
}

// EnvEvents 集群 Warning 事件。
//
// 当前恒返回空列表并带 available=false 语义（由前端按空数组处理）：
// 事件列表来自 K8s Event API，未接入集群时没有任何真实来源，
// 编造事件比返回空更糟——运维会照着假事件去排查不存在的问题。
func (s *CDService) EnvEvents(ctx context.Context, key string) ([]EnvEvent, error) {
	env, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Up.Cluster.Ping(ctx, clusterRefOf(env)); err != nil {
		return []EnvEvent{}, nil
	}
	return []EnvEvent{}, nil
}

// TestEnvConnection 环境连通性测试。
func (s *CDService) TestEnvConnection(ctx context.Context, key string, c Caller) (*ConnectivityResult, error) {
	e, err := s.envByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	res := &ConnectivityResult{EnvKey: e.EnvKey, Cluster: e.Cluster, Namespace: e.Namespace}

	if err := s.deps.Up.Cluster.Ping(ctx, clusterRefOf(e)); err != nil {
		res.ClusterOK = false
		res.ClusterMessage = err.Error()
	} else {
		res.ClusterOK = true
		res.ClusterMessage = "集群连通"
	}

	if res.ClusterOK {
		if state, err := s.deps.Up.Cluster.Nodes(ctx, clusterRefOf(e)); err == nil {
			res.Nodes = state.Nodes
			res.ClusterVersion = state.Version
		}
	}
	if err := s.deps.Up.Registry.Ping(ctx); err != nil {
		res.RegistryOK = false
		res.RegistryMessage = err.Error()
	} else {
		res.RegistryOK = true
		res.RegistryMessage = "镜像仓库连通"
	}

	res.OK = res.ClusterOK && res.RegistryOK
	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "env:test-connection", TargetType: "environment",
		TargetID: &e.ID, TargetName: e.Name,
		Detail: fmt.Sprintf("连通性测试：集群=%v，镜像仓库=%v", res.ClusterOK, res.RegistryOK),
		Result: resultWord(res.OK),
	})
	return res, nil
}

// ConnectivityResult 连通性测试结果。
type ConnectivityResult struct {
	EnvKey          string `json:"envKey"`
	Cluster         string `json:"cluster"`
	Namespace       string `json:"namespace"`
	OK              bool   `json:"ok"`
	ClusterOK       bool   `json:"clusterOk"`
	ClusterMessage  string `json:"clusterMessage"`
	RegistryOK      bool   `json:"registryOk"`
	RegistryMessage string `json:"registryMessage"`
	Nodes           int    `json:"nodes"`
	ClusterVersion  string `json:"clusterVersion"`
}

// --------------------------------------------------------------------------- 服务实例

// GetService 服务实例详情（含 Pod、版本历史）。
func (s *CDService) GetService(ctx context.Context, id uint64) (*EnvServiceDTO, error) {
	svc, err := s.serviceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	projs, err := s.projectsByKeys(ctx, []string{svc.ProjectKey})
	if err != nil {
		return nil, err
	}
	updaterNames, err := s.userNames(ctx, []uint64{derefUint64(svc.UpdatedBy)})
	if err != nil {
		return nil, err
	}
	pods, err := s.servicePods(ctx, svc)
	if err != nil {
		// Pod 列表拿不到不该让详情页整体 500：其余信息（镜像、副本、参数）仍然有用。
		s.log("cd").Warn("查询 Pod 失败", "serviceId", id, "err", err)
		pods = nil
	}
	history, err := s.serviceVersions(ctx, svc)
	if err != nil {
		return nil, err
	}
	// 详情页同样以集群为准：库里那份是"平台期望"，集群里才是"现在实际"。
	env, err := s.envByID(ctx, svc.EnvID)
	if err != nil {
		return nil, err
	}
	state, derr := s.deps.Up.Cluster.Describe(ctx, workloadRefOf(env, svc.WorkloadName))
	// 详情页也带上实时用量：点名单个调**同一个**批量方法，而不是从上面那份
	// pods 列表的字符串里再解析回来 —— 口径只有一份，不会出现
	// "列表页和详情页把同一个服务算成两个数"。
	var usage integration.WorkloadUsage
	if us, uerr := s.deps.Up.Cluster.WorkloadUsages(ctx, clusterRefOf(env), env.Namespace, []string{svc.WorkloadName}); uerr != nil {
		s.log("cd").Warn("读取服务实时用量失败，负载列将显示为 —", "serviceId", id, "err", uerr)
	} else {
		usage = us[svc.WorkloadName]
	}
	dto := serviceToDTO(svc, projs[svc.ProjectKey], updaterNames[derefUint64(svc.UpdatedBy)], pods, history, liveOf(state, usage, derr))
	return &dto, nil
}

// ServicePods Pod 列表。
func (s *CDService) ServicePods(ctx context.Context, id uint64) ([]ServicePodDTO, error) {
	svc, err := s.serviceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	pods, err := s.servicePods(ctx, svc)
	if err != nil {
		return nil, upstreamError(err, "查询 Pod 列表失败")
	}
	if pods == nil {
		pods = []ServicePodDTO{}
	}
	return pods, nil
}

// ServiceVersions 历史版本列表（含部署时间、部署人、状态）。
func (s *CDService) ServiceVersions(ctx context.Context, id uint64) ([]DeployVersionDTO, error) {
	svc, err := s.serviceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.serviceVersions(ctx, svc)
}

// ServicePodLogKey Pod 日志流的键。
func ServicePodLogKey(serviceID uint64, podName string) string {
	return logstream.Key("pod", serviceID) + ":" + podName
}

// StreamPodLogs 把 Pod 日志推进日志流，供 SSE 消费。
//
// 先尝试真实集群；集群未接入时返回明确错误而不是空流——
// 前端会看到"该环境未接入集群，无法读取 Pod 日志"，比一个永远空白的日志窗有用。
func (s *CDService) StreamPodLogs(ctx context.Context, id uint64, podName string, tailLines int) (string, error) {
	svc, err := s.serviceByID(ctx, id)
	if err != nil {
		return "", err
	}
	env, err := s.envByID(ctx, svc.EnvID)
	if err != nil {
		return "", err
	}
	rc, err := s.deps.Up.Cluster.PodLogs(ctx, workloadRefOf(env, svc.WorkloadName), podName, tailLines)
	if err != nil {
		return "", upstreamError(err, "读取 Pod %s 日志失败", podName)
	}

	key := ServicePodLogKey(id, podName)
	s.deps.Stream.Reset(ctx, key)
	go func() {
		// 流的关闭必须放在这个 goroutine 里，**不能**用 defer 挂在 StreamPodLogs 上：
		// PodLogs 返回的是一条网络流，函数一返回就关掉它，正在读它的这个 goroutine
		// 会立刻读到 EOF —— 表现为"日志窗一片空白，却没有任何报错"。
		// 接本地实现时看不出来（那边的 ReadCloser 是 NopCloser，Close 是空操作），
		// 一接真实集群就必然踩到。
		defer rc.Close()
		defer s.deps.Stream.Finish(context.WithoutCancel(ctx), key, "finished")
		// 按行推送：Pod 日志是"追加式"的，逐行给前端体验最好。
		// 缓冲区放宽到 1MB：Java 服务的堆栈或大 JSON 日志单行很容易超过默认 64KB。
		scanner := bufio.NewScanner(rc)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := scanner.Text()
			level := "info"
			switch {
			case strings.Contains(line, "ERROR"), strings.Contains(line, "Exception"):
				level = "error"
			case strings.Contains(line, "WARN"):
				level = "warn"
			}
			if _, err := s.deps.Stream.Append(ctx, key, level, line, now()); err != nil {
				if errors.Is(err, logstream.ErrClosed) {
					return
				}
			}
		}
	}()
	return key, nil
}

// UpdateServiceParams 设置服务参数，并写入一版参数历史。
//
// 每次变更留一版是刻意的：K8s 上"这个服务为什么内存不够"这类问题，
// 最常见的原因就是有人改过资源配置而没人记得。有版本历史才能回答"什么时候改的、改成什么"。
func (s *CDService) UpdateServiceParams(ctx context.Context, id uint64, in ServiceParamsInput, c Caller) (*EnvServiceDTO, error) {
	svc, err := s.serviceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Replicas != nil {
		if err := ensurePositive(*in.Replicas, "副本数"); err != nil {
			return nil, err
		}
	}
	cpuReq := stringOr(in.CPURequest, svc.CPURequest)
	cpuLim := stringOr(in.CPULimit, svc.CPULimit)
	memReq := stringOr(in.MemRequest, svc.MemRequest)
	memLim := stringOr(in.MemLimit, svc.MemLimit)
	if err := validateResources(cpuReq, cpuLim, memReq, memLim); err != nil {
		return nil, err
	}

	replicas := svc.Replicas
	if in.Replicas != nil {
		replicas = *in.Replicas
	}
	hpa := svc.HPAEnabled
	if in.HPAEnabled != nil {
		hpa = *in.HPAEnabled
	}

	// 环境变量里的密文项：入参给了掩码或留空时沿用旧值（同环境变量保存的取舍）。
	newVars := svc.EnvVars
	if in.EnvVars != nil {
		old := map[string]string{}
		var cur []EnvVarDTO
		if !svc.EnvVars.Empty() {
			_ = json.Unmarshal(svc.EnvVars, &cur)
		}
		for _, v := range cur {
			old[v.Key] = v.Value
		}
		encoded, err := json.Marshal(mergeSecretEnvVars(in.EnvVars, old))
		if err != nil {
			return nil, errs.Internalf(err, "序列化服务变量失败")
		}
		newVars = model.JSONText(encoded)
	}

	cfgMaps := svc.ConfigMaps
	if in.ConfigMaps != nil {
		cfgMaps = mustJSON(in.ConfigMaps)
	}
	secrets := svc.Secrets
	if in.Secrets != nil {
		secrets = mustJSON(in.Secrets)
	}

	err = s.tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Model(&model.ServiceInstance{}).Where("id = ?", svc.ID).Updates(map[string]any{
			"replicas": replicas, "cpu_request": cpuReq, "cpu_limit": cpuLim,
			"mem_request": memReq, "mem_limit": memLim, "hpa_enabled": hpa,
			"env_vars": newVars, "config_maps": cfgMaps, "secrets": secrets,
			"updated_by": c.UserID(), "updated_at": now(),
		}).Error; err != nil {
			return err
		}
		var maxVer *uint32
		if err := tx.Model(&model.DeployParam{}).Where("service_id = ?", svc.ID).
			Select("MAX(version)").Scan(&maxVer).Error; err != nil {
			return err
		}
		ver := uint32(1)
		if maxVer != nil {
			ver = *maxVer + 1
		}
		param := model.DeployParam{
			ServiceID:  svc.ID,
			EnvKey:     svc.EnvKey,
			ProjectKey: svc.ProjectKey,
			Version:    ver,
			Replicas:   &replicas,
			CPURequest: cpuReq, CPULimit: cpuLim,
			MemRequest: memReq, MemLimit: memLim,
			HPAEnabled: &hpa,
			EnvVars:    newVars, ConfigMaps: cfgMaps, Secrets: secrets,
			Diff:      diffOf(svc, cpuReq, cpuLim, memReq, memLim, replicas, hpa),
			ChangedBy: c.UserID(),
			ChangedAt: now(),
			Note:      stringOr(in.Note, "通过控制台调整服务参数"),
		}
		return tx.Create(&param).Error
	})
	if err != nil {
		return nil, wrapDB(err, "保存服务参数")
	}

	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "service:params", TargetType: "service",
		TargetID: &svc.ID, TargetName: svc.ProjectKey,
		Detail: fmt.Sprintf("调整服务参数：%s/%s（副本 %d→%d）", svc.EnvKey, svc.ProjectKey, svc.Replicas, replicas),
	})
	return s.GetService(ctx, id)
}

// ScaleService 扩缩容。
func (s *CDService) ScaleService(ctx context.Context, id uint64, replicas int, c Caller) (*EnvServiceDTO, error) {
	if err := ensurePositive(replicas, "副本数"); err != nil {
		return nil, err
	}
	svc, env, err := s.serviceWithEnv(ctx, id)
	if err != nil {
		return nil, err
	}
	if svc.Replicas == replicas {
		return s.GetService(ctx, id)
	}

	release, ok, err := s.deps.Locker.TryLock(ctx, queue.LockKey(svc.EnvKey, svc.ProjectKey), s.deployLockTTL())
	if err != nil {
		return nil, errs.Internalf(err, "获取部署锁失败")
	}
	if !ok {
		return nil, errs.StateConflict("该服务正在部署中，请稍后再试")
	}
	defer release()

	if err := s.deps.Up.Cluster.Scale(ctx, workloadRefOf(env, svc.WorkloadName), replicas); err != nil {
		s.audit(ctx, c, auditInput{
			Module: "CD", Action: "service:scale", TargetType: "service",
			TargetID: &svc.ID, TargetName: svc.ProjectKey,
			Detail: fmt.Sprintf("扩缩容失败：%d → %d", svc.Replicas, replicas),
			Result: "failed", ErrMsg: err.Error(),
		})
		return nil, upstreamError(err, "调整副本数失败")
	}

	ready := svc.ReadyReplicas
	if ready > replicas {
		ready = replicas
	}
	if err := s.db(ctx).Model(&model.ServiceInstance{}).Where("id = ?", svc.ID).Updates(map[string]any{
		"replicas": replicas, "ready_replicas": ready, "updated_by": c.UserID(), "updated_at": now(),
	}).Error; err != nil {
		return nil, errs.Internalf(err, "更新副本数失败")
	}

	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "service:scale", TargetType: "service",
		TargetID: &svc.ID, TargetName: svc.ProjectKey,
		Detail: fmt.Sprintf("扩缩容：%s/%s 副本 %d → %d", svc.EnvKey, svc.ProjectKey, svc.Replicas, replicas),
	})
	return s.GetService(ctx, id)
}

// RestartService 滚动重启。
func (s *CDService) RestartService(ctx context.Context, id uint64, c Caller) error {
	svc, env, err := s.serviceWithEnv(ctx, id)
	if err != nil {
		return err
	}
	if err := s.deps.Up.Cluster.Restart(ctx, workloadRefOf(env, svc.WorkloadName)); err != nil {
		s.audit(ctx, c, auditInput{
			Module: "CD", Action: "service:restart", TargetType: "service",
			TargetID: &svc.ID, TargetName: svc.ProjectKey,
			Detail: "滚动重启失败", Result: "failed", ErrMsg: err.Error(),
		})
		return upstreamError(err, "重启服务失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "service:restart", TargetType: "service",
		TargetID: &svc.ID, TargetName: svc.ProjectKey,
		Detail: fmt.Sprintf("滚动重启 %s/%s", svc.EnvKey, svc.ProjectKey),
	})
	return nil
}

// RedeployService 用当前镜像重新部署（走完整部署单，保证有日志与步骤）。
func (s *CDService) RedeployService(ctx context.Context, id uint64, c Caller) (*DeployDTO, error) {
	svc, err := s.serviceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if svc.CurrentImage == "" {
		return nil, errs.StateConflict("该服务还没有部署过任何镜像，请先选择版本部署")
	}
	// 找当前镜像对应的镜像记录，拿不到就按镜像地址原样部署。
	var img model.ImageRecord
	_ = s.db(ctx).Where("image = ?", svc.CurrentImage).Order("pushed_at desc").First(&img).Error

	in := CreateDeploymentInput{
		EnvKey:     svc.EnvKey,
		ProjectKey: svc.ProjectKey,
		Replicas:   &svc.Replicas,
		Remark:     "重新部署（当前镜像）",
	}
	if img.ID != 0 {
		in.ImageRecordID = httpx.IDString(img.ID)
	} else {
		// 当前镜像没有平台构建记录（从仓库标签发的）。**必须原样指定它**：
		// 什么都不传会落到"该项目最新镜像"，那就是"重新部署"把版本换掉了。
		in.ImageOverride = svc.CurrentImage
	}
	return s.CreateDeployment(ctx, in, c)
}

// DeleteService 下线服务（软删实例，同时从集群移除工作负载）。
func (s *CDService) DeleteService(ctx context.Context, id uint64, c Caller) error {
	svc, env, err := s.serviceWithEnv(ctx, id)
	if err != nil {
		return err
	}
	// 取消纳管 = 删掉平台下发的变更单（AppRelease）+ 删平台记录，**绝不动集群里的负载**。
	//
	// ⚠️ 原先这里调的是 `Scale(..., 0)`：那是往变更单里写 `spec.replicas = 0`，
	// operator 会把副本缩到 0 —— **线上真的停服**。一个发布平台不该有这种副作用
	// （2026-10-14 用户指出："你只是发布平台，只能取消纳管…怎么能直接改集群让服务不可用"）。
	// 依据 operator 源码，删除变更单时的清理动作只摘掉 Deployment 上的接管标记，
	// 既不删 Deployment、也不改镜像与副本，所以下面这一步是"只取消纳管"。
	//
	// 失败就整体中止（含"多张变更单"那种必须先人工处理的硬约束），
	// 避免出现"平台里没了、变更单还在、operator 继续按旧声明覆盖"的半截状态。
	if err := s.deps.Up.Cluster.Release(ctx, workloadRefOf(env, svc.WorkloadName)); err != nil {
		return upstreamError(err, "解除纳管失败，已中止操作")
	}
	if err := s.db(ctx).Delete(&model.ServiceInstance{}, svc.ID).Error; err != nil {
		return errs.Internalf(err, "删除服务实例失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "service:delete", TargetType: "service",
		TargetID: &svc.ID, TargetName: svc.ProjectKey,
		Detail: fmt.Sprintf("取消纳管 %s/%s（已删除集群里的变更单，未改动运行中的负载）", svc.EnvKey, svc.ProjectKey),
	})
	return nil
}

// ParamVersionDTO 参数历史版本。
type ParamVersionDTO struct {
	ID         string      `json:"id"`
	Version    uint32      `json:"version"`
	Replicas   *int        `json:"replicas"`
	CPURequest string      `json:"cpuRequest"`
	CPULimit   string      `json:"cpuLimit"`
	MemRequest string      `json:"memRequest"`
	MemLimit   string      `json:"memLimit"`
	HPAEnabled *bool       `json:"hpaEnabled"`
	EnvVars    []EnvVarDTO `json:"envVars"`
	Diff       []DiffItem  `json:"diff"`
	ChangedBy  string      `json:"changedBy"`
	ChangedAt  string      `json:"changedAt"`
	Note       string      `json:"note"`
}

// DiffItem 一处变更。
type DiffItem struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// ServiceParamHistory 参数变更历史。
func (s *CDService) ServiceParamHistory(ctx context.Context, id uint64) ([]ParamVersionDTO, error) {
	svc, err := s.serviceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	var rows []model.DeployParam
	if err := s.db(ctx).Where("service_id = ?", svc.ID).Order("version desc").Limit(50).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询参数历史失败")
	}
	names, err := s.userNames(ctx, paramChangerIDs(rows))
	if err != nil {
		return nil, err
	}
	out := make([]ParamVersionDTO, 0, len(rows))
	for i := range rows {
		p := &rows[i]
		dto := ParamVersionDTO{
			ID: httpx.IDString(p.ID), Version: p.Version,
			Replicas: p.Replicas, CPURequest: p.CPURequest, CPULimit: p.CPULimit,
			MemRequest: p.MemRequest, MemLimit: p.MemLimit, HPAEnabled: p.HPAEnabled,
			EnvVars:   decodeEnvVars(p.EnvVars),
			ChangedBy: names[derefUint64(p.ChangedBy)],
			ChangedAt: formatTime(p.ChangedAt),
			Note:      p.Note,
			Diff:      []DiffItem{},
		}
		if !p.Diff.Empty() {
			var items []DiffItem
			if err := json.Unmarshal(p.Diff, &items); err == nil && items != nil {
				dto.Diff = items
			}
		}
		out = append(out, dto)
	}
	return out, nil
}

// --------------------------------------------------------------------------- 部署

var deploySortColumns = map[string]string{
	"createdAt": "created_at",
	"startedAt": "started_at",
	"status":    "status",
	"deployNo":  "deploy_no",
}

// CreateDeployment 创建部署单。
//
// 与构建最大的不同：**部署有并发禁忌**。同一个环境同一个服务并发部署，
// K8s 会以最后一次 apply 为准，但平台里会留下两条互相矛盾的记录，
// 用户无法判断"现在线上到底是哪个版本"。所以这里有三道闸：
//  1. 数据库层检查是否已有进行中的部署单（给用户明确的错误）
//  2. 分布式锁（跨实例，多副本部署时生效）
//  3. 任务池并发上限（保护集群与镜像仓库）
func (s *CDService) CreateDeployment(ctx context.Context, in CreateDeploymentInput, c Caller) (*DeployDTO, error) {
	env, err := s.envByKey(ctx, in.EnvKey)
	if err != nil {
		return nil, err
	}
	if !env.Enabled {
		return nil, errs.New(errs.CodeEnvDisabled, "环境已停用，禁止部署")
	}
	proj, err := s.projectByKey(ctx, in.ProjectKey)
	if err != nil {
		return nil, err
	}
	if !proj.Enabled {
		return nil, errs.StateConflict("项目已停用，无法部署")
	}
	svc, err := s.serviceByEnvProject(ctx, env.ID, proj.ID)
	if err != nil {
		return nil, err
	}

	// 部署权限是按环境分级的：能发测试区不代表能发正式区。
	// 这里再校验一次（中间件已按 deploy:{env} 拦过，但 service 可能被其他入口调用）。
	if c.User != nil && !c.User.HasAnyPerm("deploy:"+env.EnvKey, "deploy:*", "*") {
		return nil, errs.Forbidden(fmt.Sprintf("缺少 %s 环境的部署权限", env.EnvKey))
	}

	// 幂等：同一 key 直接返回已有单。
	if k := strings.TrimSpace(in.IdempotencyKey); k != "" {
		var exist model.DeployRecord
		if err := s.db(ctx).Where("idempotency_key = ?", k).First(&exist).Error; err == nil {
			return s.GetDeployment(ctx, exist.ID)
		}
	}

	// 镜像来源**二选一**：都选、都不选都要在这里拦住。
	//
	// 以前"两个都不给"会静默落到"该项目最新镜像"——那是把"用户没选"翻译成
	// "我替你挑了一个"，发出去的版本和界面上显示的可能完全不是一回事。
	// 所以宁可拒。
	if err := checkImageSource(in); err != nil {
		return nil, err
	}

	img, err := s.resolveDeployImage(ctx, proj, in)
	if err != nil {
		return nil, err
	}

	// 发单前镜像预检：仓库里没有这个 tag，这次发布**必然失败**。
	// 与其建单后跑到"拉取镜像"再等一轮超时，不如在发单这一步就如实拒绝 ——
	// 这就是"能勾选 == 后端受理"那条口径：受理不了，就别让它发得出来。
	//
	// 仓库标签来源的镜像**刚刚才从仓库连摘要一起读回来**（resolveRegistryImage），
	// 不必再问一遍同样的接口。
	if strings.TrimSpace(in.RegistryTag) == "" {
		if err := s.preflightImage(ctx, img.Image); err != nil {
			return nil, err
		}
	}

	var active int64
	if err := s.db(ctx).Model(&model.DeployRecord{}).
		Where("service_id = ? AND status IN ?", svc.ID, []string{"pending_approval", "queued", "progressing"}).
		Count(&active).Error; err != nil {
		return nil, errs.Internalf(err, "检查进行中的部署失败")
	}
	if active > 0 {
		return nil, errs.StateConflict("该服务已有进行中的部署，请等待完成或先取消")
	}

	replicas := svc.Replicas
	if in.Replicas != nil && *in.Replicas > 0 {
		replicas = *in.Replicas
	}
	strategy := stringOr(in.Strategy, "RollingUpdate")

	// 需要审批的环境先落 pending_approval，不占任务池。
	status := "queued"
	if env.NeedApproval {
		status = "pending_approval"
	}

	rec := model.DeployRecord{
		EnvID:        env.ID,
		EnvKey:       env.EnvKey,
		ProjectID:    proj.ID,
		ProjectKey:   proj.ProjectKey,
		ServiceID:    svc.ID,
		ImageRecID:   img.ID,
		Image:        img.Image,
		ImageDigest:  img.Digest,
		Replicas:     replicas,
		Strategy:     strategy,
		Remark:       in.Remark,
		Status:       status,
		Progress:     0,
		OperatorID:   derefUint64(c.UserID()),
		OperatorName: c.Name(),
		CreatedAt:    now(),
		UpdatedAt:    now(),
	}
	if k := strings.TrimSpace(in.IdempotencyKey); k != "" {
		rec.IdempotencyKey = &k
	}

	/**
	 * **算号与插入放在同一个临界区里**（deployNoMu 由外层持有）。
	 *
	 * 部署单号是"查当天最大号 + 1"，而号要 INSERT 时才落库 —— 中间留窗口的话，
	 * 并发请求会先后拿到同一个号，由唯一索引 `uk_deploy_no` 拒掉其中一个，
	 * 而用户看到的是一句"创建部署单失败"：既没有信息量、又指向错误方向的 500。
	 * （2026-10-16 批量发布改成并发建单后实测撞到：6 个服务挂掉 2 个。）
	 *
	 * 这里的**重试**是兜底（跨实例、或锁外的其他意外），主要防线是那把锁 ——
	 * 让撞号一开始就不发生，而不是撞了再补救。
	 */
	deployNoMu.Lock()
	defer deployNoMu.Unlock()
	for attempt := 0; ; attempt++ {
		no, err := s.nextDeployNoLocked(ctx)
		if err != nil {
			return nil, err
		}
		rec.DeployNo = no
		// 上一次失败可能给模型留了主键，清掉再插。
		rec.ID = 0
		err = s.db(ctx).Create(&rec).Error
		if err == nil {
			break
		}
		// 幂等键撞了：另一笔同键请求已经建好了 —— 那正是"幂等"想要的结果。
		// 并发双击、客户端重试都会走到这里，直接把已存在那一单返回，不是报错。
		if rec.IdempotencyKey != nil && isDupKeyOn(err, "uk_idempotency_key") {
			var exist model.DeployRecord
			if e := s.db(ctx).Where("idempotency_key = ?", *rec.IdempotencyKey).First(&exist).Error; e == nil {
				return s.GetDeployment(ctx, exist.ID)
			}
		}
		if !isDupKeyOn(err, "uk_deploy_no") || attempt >= 5 {
			return nil, errs.Internalf(err, "创建部署单失败")
		}
		// 撞号：重算再试。（同实例下有了上面那把锁基本不会走到这里。）
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	if err := s.initDeploySteps(ctx, rec.ID); err != nil {
		return nil, err
	}

	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "deploy:create", TargetType: "deployment",
		TargetID: &rec.ID, TargetName: rec.DeployNo,
		Detail: fmt.Sprintf("创建部署单 %s：%s/%s → %s（副本 %d，%s）",
			rec.DeployNo, env.EnvKey, proj.ProjectKey, img.Tag, replicas, statusWordDeploy(status)),
	})

	if status == "pending_approval" {
		// 待审批的单不进任务池，没人点它就会一直停在那里 ——
		// 所以要在**建单这一刻**就叫审批人，而不是等他明天想起来打开平台。
		// 那句话"审批流卡在起点"说的就是这里：没有通知的待审批等于没人知道有这单。
		s.notify.Dispatch(ctx, s.notify.DeployPendingMessage(&rec))
		return s.GetDeployment(ctx, rec.ID)
	}
	if err := s.submitDeploy(ctx, &rec); err != nil {
		return nil, err
	}
	return s.GetDeployment(ctx, rec.ID)
}

// submitDeploy 把部署单投进任务池。审批通过后也走这里。
func (s *CDService) submitDeploy(ctx context.Context, rec *model.DeployRecord) error {
	streamKey := DeployLogStreamKey(rec.ID)
	s.deps.Stream.Reset(ctx, streamKey)

	pos, err := s.deps.Deploys.Submit(rec.ID, func(taskCtx context.Context) {
		s.runDeploy(taskCtx, rec.ID)
	})
	if err != nil {
		fin := now()
		// 队列拒收也必须有真因 —— 这条单子会以 failed 永久留在列表里，
		// 只给一个 QUEUE_REJECTED 机器码，用户看不出"是队列满了还是重复提交"。
		rejMsg := "该部署单已在队列中"
		if errors.Is(err, queue.ErrQueueFull) {
			rejMsg = "部署队列已满，请稍后重试"
		}
		_ = s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", rec.ID).Updates(map[string]any{
			"status": "failed", "error_code": "QUEUE_REJECTED", "error_message": rejMsg,
			"finished_at": &fin, "updated_at": fin,
		}).Error
		s.deps.Stream.Finish(ctx, streamKey, "failed")
		if errors.Is(err, queue.ErrQueueFull) {
			return errs.RateLimited(rejMsg)
		}
		return errs.StateConflict(rejMsg)
	}
	if pos > 0 {
		_ = s.deps.Stream.SetStatus(ctx, streamKey, logstream.Status{
			Status: "queued", Progress: 0, Stage: fmt.Sprintf("排队中（前面还有 %d 个任务）", pos-1),
		})
	}
	return nil
}

// runDeploy 执行部署。这是平台最关键的一段：它要保证"要么线上真的换版了，要么明确失败"。
func (s *CDService) runDeploy(taskCtx context.Context, deployID uint64) {
	streamKey := DeployLogStreamKey(deployID)
	log := s.log("cd.deploy").With("deployId", deployID)

	timeout := s.deps.Cfg.Runner.DeployTimeout.D()
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(taskCtx, timeout)
	defer cancel()

	var rec model.DeployRecord
	if err := s.db(ctx).First(&rec, deployID).Error; err != nil {
		log.Error("读取部署单失败", "err", err)
		return
	}
	var env model.Environment
	if err := s.db(ctx).First(&env, rec.EnvID).Error; err != nil {
		log.Error("读取环境失败", "err", err)
		s.failDeploy(ctx, deployID, "ENV_NOT_FOUND", "环境不存在", nil)
		return
	}
	var svc model.ServiceInstance
	if err := s.db(ctx).First(&svc, rec.ServiceID).Error; err != nil {
		log.Error("读取服务实例失败", "err", err)
		s.failDeploy(ctx, deployID, "SERVICE_NOT_FOUND", "服务实例不存在", nil)
		return
	}

	// 互斥锁：同一环境同一服务不允许并发部署。
	// 注意这是**跨实例**的锁（Redis），因此多副本部署平台时同样有效。
	lockKey := queue.LockKey(rec.EnvKey, rec.ProjectKey)
	release, ok, err := s.deps.Locker.TryLock(ctx, lockKey, s.deployLockTTL())
	if err != nil {
		log.Error("获取部署锁失败", "err", err)
		s.failDeploy(ctx, deployID, "LOCK_ERROR", "获取部署锁失败："+err.Error(), nil)
		return
	}
	if !ok {
		s.failDeploy(ctx, deployID, "DEPLOY_LOCKED", "该服务正在部署中（互斥锁被占用）", nil)
		return
	}
	defer release()

	started := now()
	if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", deployID).Updates(map[string]any{
		"status": "progressing", "progress": 5, "started_at": &started, "updated_at": started,
	}).Error; err != nil {
		log.Error("更新部署状态失败", "err", err)
		return
	}
	if err := s.db(ctx).Model(&model.ServiceInstance{}).Where("id = ?", svc.ID).
		Update("status", "deploying").Error; err != nil {
		log.Warn("更新服务状态失败", "err", err)
	}
	s.appendDeployLog(ctx, streamKey, "info", fmt.Sprintf("开始部署 %s：%s/%s → %s",
		rec.DeployNo, rec.EnvKey, rec.ProjectKey, rec.Image))
	_ = s.deps.Stream.SetStatus(ctx, streamKey, logstream.Status{Status: "progressing", Progress: 5, Stage: "准备中"})

	req := integration.ApplyRequest{
		WorkloadRef: workloadRefOf(&env, svc.WorkloadName),
		Image:       rec.Image,
		Replicas:    rec.Replicas,
		Strategy:    rec.Strategy,
		EnvVars:     serviceEnvMap(svc.EnvVars),
	}

	applyErr := s.deps.Up.Cluster.ApplyImage(ctx, req, func(stage string, pct uint8) error {
		if err := s.markStep(ctx, deployID, stage, pct); err != nil {
			log.Warn("更新部署步骤失败", "err", err)
		}
		if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", deployID).
			Update("progress", pct).Error; err != nil {
			log.Warn("更新部署进度失败", "err", err)
		}
		s.appendDeployLog(ctx, streamKey, "info", fmt.Sprintf("[%s] 进度 %d%%", stage, pct))
		return s.deps.Stream.SetStatus(ctx, streamKey, logstream.Status{
			Status: "progressing", Progress: pct, Stage: stage,
		})
	})

	fin := now()
	duration := uint32(fin.Sub(started).Seconds())

	if applyErr != nil {
		code := "APPLY_FAILED"
		reason := applyErr.Error()
		switch {
		case errors.Is(applyErr, context.Canceled):
			code, reason = "CANCELED", "部署已被取消"
		case errors.Is(applyErr, context.DeadlineExceeded):
			code, reason = "TIMEOUT", "部署超时"
		case errors.Is(applyErr, integration.ErrRolloutStuck):
			// 单列一个错误码，因为它与「超时」的处置完全不同：
			// 超时要查"为什么慢"，卡死要查"为什么起不来"。混在一个码里，
			// 规则兜底就只能给一段两头都对的废话。
			code, reason = "ROLLOUT_STUCK", applyErr.Error()
		}

		// 收尾一律用**摘掉取消信号的 ctx**。
		// 理由：失败原因里有两种就是"本次 ctx 超时/被取消"，那时 ctx 已 done，
		// 拿它去写库会失败 —— 表现是"部署卡在 progressing 永不收尾"，
		// 比部署失败本身更难发现。收尾是必须做成的事，不该被进来时的 ctx 左右。
		finCtx := context.WithoutCancel(ctx)

		// 失败**不回滚**，工作负载就留在"新镜像 + 卡住的 rollout"上（2026-10-08 用户拍板）。
		//
		// 这是刻意不做，不是漏了：`RollingUpdate` 在 1 副本时 `maxUnavailable`
		// 向下取整为 0，旧 Pod 不会被杀，服务通常不中断；此时"谁在跑、跑的是哪一版"
		// 是确定的，比平台自作主张再改一次镜像更可控 —— 回滚本身也是一次发版，
		// 它失败就会把一个本来还能用的负载改坏，而平台只能再报一次错。
		//
		// 代价必须说清楚（下面那行 warn 日志就是给用户看的）：失败的 spec 会留在集群上，
		// 直到有人回滚或重新发布。恢复动作交给人，用的是早已实现的手工回滚链路。
		s.appendDeployLog(finCtx, streamKey, "error", reason)
		s.appendDeployLog(finCtx, streamKey, "warn",
			"平台不会自动回滚，工作负载当前停在本次变更后的镜像上。"+
				"确认需要恢复时，请在「AI 诊断」里按建议回滚，或重新发布一个可用版本。")

		// 把集群侧的**原始证据**（容器状态 / Pod 事件 / 崩溃日志）搬进这一屏。
		// 平台日志只会说"等待就绪探针"，用户想知道"为什么起不来"仍然得开终端
		// 跑 kubectl —— 那正是这次失败最该省掉的一步。见 appendDeployEvidenceLogs。
		s.appendDeployEvidenceLogs(finCtx, streamKey, req.WorkloadRef)

		// 回滚单（若有）也要收尾，否则会永远停在 progressing。
		s.finishRollbackOf(finCtx, deployID, "failed", duration)

		s.failDeploy(finCtx, deployID, code, reason, diagnoseDeploy(code))
		// 「滚动更新卡死」特有：K8s 到点就宣布卡死，而镜像拉得慢时 Pod 常常
		// 在宣布之后才起来。起一个后台观察，真自愈了就把时间记下来 ——
		// 状态不动，只让页面能标「失败后已就绪」。
		if code == "ROLLOUT_STUCK" {
			go s.watchRecoveredAfterFailure(deployID, req.WorkloadRef)
		}
		// 服务回到"异常"而不是"部署中"：部署中是个瞬态，留着会让用户以为还在进行。
		if err := s.db(finCtx).Model(&model.ServiceInstance{}).Where("id = ?", svc.ID).
			Update("status", "error").Error; err != nil {
			log.Warn("更新服务状态失败", "err", err)
		}
		log.Warn("部署失败", "code", code, "err", applyErr)
		return
	}

	// 成功：更新服务实例为最新版本。
	err = s.tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Model(&model.ServiceInstance{}).Where("id = ?", svc.ID).Updates(map[string]any{
			"current_image":    rec.Image,
			"current_tag":      tagOfImage(rec.Image),
			"current_image_id": rec.ImageRecID,
			"replicas":         rec.Replicas,
			"ready_replicas":   rec.Replicas,
			"status":           "running",
			"liveness_ok":      true,
			"readiness_ok":     true,
			"last_deploy_id":   rec.ID,
			"updated_at":       fin,
		}).Error; err != nil {
			return err
		}
		// 回滚产生的部署单终态用 rolled_back（见 model.DeployRecord 注释），
		// 其余是 success。这样部署列表能一眼区分"这次是回滚"。
		finalStatus := "success"
		if rec.ParentDeploy != nil {
			finalStatus = "rolled_back"
		}
		return tx.Model(&model.DeployRecord{}).Where("id = ?", deployID).Updates(map[string]any{
			"status": finalStatus, "progress": 100, "finished_at": &fin,
			"duration_sec": duration, "updated_at": fin,
		}).Error
	})
	if err != nil {
		log.Error("写部署终态失败", "err", err)
		s.failDeploy(ctx, deployID, "PERSIST_FAILED", "部署已完成但状态写入失败："+err.Error(), nil)
		return
	}

	s.markAllStepsDone(ctx, deployID)
	s.markImageDeployed(ctx, rec.ImageRecID, rec.EnvKey)
	s.finishRollbackOf(ctx, deployID, "success", duration)

	// 部署成功通知。放在这里（终态已落库 + 回滚单也收尾了），
	// 而不是 ApplyImage 刚返回 nil 时 —— 见 notifyDeploySucceeded 的注释。
	s.notifyDeploySucceeded(ctx, deployID)

	s.appendDeployLog(ctx, streamKey, "info", fmt.Sprintf("部署完成，%s/%s 已更新为 %s",
		rec.EnvKey, rec.ProjectKey, tagOfImage(rec.Image)))
	_ = s.deps.Stream.Finish(ctx, streamKey, "finished")
	log.Info("部署成功", "image", rec.Image)
}

// failDeploy 统一写部署失败终态并关流。
// watchRecoveredAfterFailure 在部署被判「滚动更新卡死」之后，继续看一阵集群。
//
// 为什么需要它：K8s 到 progressDeadlineSeconds（未显式设置时默认 600s）就宣布卡死，
// 而"镜像拉得慢"的场景里，Pod 往往在宣布**之后**才拉完并起来 ——
// 用户看到的就是"后台其实成功了、页面说失败"（2026-10-19 报的）。
//
// 刻意**不改 Status**（理由见 model.DeployRecord.RecoveredAt）。
//
// 观察窗口 5 分钟、20 秒一次：足够覆盖"差一点拉完"，又不至于为一个已失败的单
// 长期占着 goroutine。读不到状态时**继续看**（读不到 ≠ 没好）。
func (s *CDService) watchRecoveredAfterFailure(deployID uint64, ref integration.WorkloadRef) {
	// 独立 ctx：这个观察与部署任务的生死无关 —— 走到这里时部署 ctx 往往已经超时/取消了。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			st, err := s.deps.Up.Cluster.Describe(ctx, ref)
			if err != nil || st == nil || st.Status != "running" {
				continue
			}
			if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", deployID).
				Update("recovered_at", now()).Error; err != nil {
				s.log("cd").Warn("记录部署自愈时间失败", "deployId", deployID, "err", err)
			}
			return
		}
	}
}

func (s *CDService) failDeploy(ctx context.Context, deployID uint64, code, reason string, diag *AIDiagnosisDTO) {
	fin := now()
	updates := map[string]any{
		"status": "failed", "finished_at": &fin, "updated_at": fin,
	}
	if code != "" {
		updates["error_code"] = code
	}
	// 真因落库。「部署失败不回滚」之后现场只剩"集群停在新镜像"这一个事实，
	// 解释它的线索以前只进了下面的日志流 Stage —— 而日志流易失，平台一重启就没了。
	// 这条路径有 5 个调用点，其中 4 个传 diag=nil，所以不能指望 ai_diagnosis 兜住原因。
	if reason != "" {
		updates["error_message"] = clipRunes(reason, 512)
	}
	if diag != nil {
		if raw, err := json.Marshal(diag); err == nil {
			updates["ai_diagnosis"] = model.JSONText(raw)
		}
	}
	if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", deployID).Updates(updates).Error; err != nil {
		s.log("cd.deploy").Error("写部署失败终态失败", "deployId", deployID, "err", err)
	}
	_ = s.deps.Stream.SetStatus(ctx, DeployLogStreamKey(deployID), logstream.Status{Status: "failed", Progress: 0, Stage: reason})
	_ = s.deps.Stream.Finish(ctx, DeployLogStreamKey(deployID), "failed")
	// 部署失败必须主动通知。「失败不回滚」之后集群就停在新镜像上，
	// 而下一个发现这件事的人可能是明天的用户 —— 中间那段空窗正是通知要填的。
	s.notifyDeployFailed(ctx, deployID)
}

// notifyDeployFailed 部署失败时推送通知。
func (s *CDService) notifyDeployFailed(ctx context.Context, deployID uint64) {
	var rec model.DeployRecord
	if err := s.db(ctx).First(&rec, deployID).Error; err != nil {
		s.log("cd.deploy").Warn("读取部署记录用于通知失败", "deployId", deployID, "err", err)
		return
	}
	s.notify.Dispatch(ctx, s.notify.DeployFailedMessage(&rec))
}

// notifyDeploySucceeded 部署成功后推送通知 —— "发布成功、可以验收了"的那一条。
//
// ⚠️ 是**发布成功**，不是"发布流程结束"（用户 2026-10-15 特意纠正过措辞）：
// 结束只说明流程停了，失败也是结束；收件人要拿这条消息判断"项目没问题"。
// 所以调用点有两个硬条件：
//
//  1. **终态已经落库**（那个 tx 成功返回）。提前到 `ApplyImage` 刚返回 nil 就发，
//     会出现"群里说部署成功、界面上还停在部署中"—— 写终态失败时平台会走
//     PERSIST_FAILED 报失败，而消息已经发出去了；
//  2. **`ApplyImage` 返回了 nil**，即它内部 `judgeRollout` 已经确认
//     "期望镜像生效 + 就绪副本到位"。这正是用户要的"知道项目没问题"的依据 ——
//     挂在"spec 提交完成"上会在 rollout 失败之前就报成功，比没有还糟。
//
// 回滚单（`ParentDeploy != nil`，终态 `rolled_back`）也走这里：回滚就是一次换镜像的
// 发布，群里同样要知道"回滚成功、服务恢复了"，事件码不另开（见 DeploySucceededMessage）。
func (s *CDService) notifyDeploySucceeded(ctx context.Context, deployID uint64) {
	var rec model.DeployRecord
	if err := s.db(ctx).First(&rec, deployID).Error; err != nil {
		s.log("cd.deploy").Warn("读取部署记录用于通知成功", "deployId", deployID, "err", err)
		return
	}
	s.notify.Dispatch(ctx, s.notify.DeploySucceededMessage(&rec))
}

// ListDeployments 部署记录列表。
func (s *CDService) ListDeployments(ctx context.Context, q DeploymentQuery) (*httpx.PageQuery, any, int64, error) {
	pq := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize, SortBy: q.SortBy, Order: q.Order, Keyword: q.Keyword}
	column, desc := pq.Normalize(deploySortColumns, "created_at")

	db := s.db(ctx).Model(&model.DeployRecord{})
	if v := strings.TrimSpace(q.EnvKey); v != "" {
		db = db.Where("env_key = ?", v)
	}
	if v := strings.TrimSpace(q.ProjectKey); v != "" {
		db = db.Where("project_key = ?", v)
	}
	if v := strings.TrimSpace(q.Status); v != "" {
		db = s.applyDeployStatusFilter(ctx, db, v)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("deploy_no LIKE ? OR project_key LIKE ? OR image LIKE ?", like, like, like)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "统计部署记录失败")
	}
	var rows []model.DeployRecord
	if err := db.Order(column + " " + orderWord(desc)).
		Offset(pq.Offset()).Limit(pq.PageSize).Find(&rows).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "查询部署记录失败")
	}
	return &pq, s.decorateDeploys(ctx, rows, false), total, nil
}

// applyDeployStatusFilter 支持传入逗号分隔的多个状态（前端常用"进行中"= 三个状态的并集）。
func (s *CDService) applyDeployStatusFilter(_ context.Context, db *gorm.DB, raw string) *gorm.DB {
	parts := strings.Split(raw, ",")
	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			clean = append(clean, v)
		}
	}
	if len(clean) == 1 {
		return db.Where("status = ?", clean[0])
	}
	return db.Where("status IN ?", clean)
}

// GetDeployment 部署详情（含步骤与日志）。
func (s *CDService) GetDeployment(ctx context.Context, id uint64) (*DeployDTO, error) {
	rec, err := s.deployByID(ctx, id)
	if err != nil {
		return nil, err
	}
	rows := s.decorateDeploys(ctx, []model.DeployRecord{*rec}, false)
	logs := s.deployLogTail(ctx, id, 200)
	dto := rows[0]
	dto.Logs = logs
	return &dto, nil
}

// DeploymentLogs 分页拉取部署日志（复用构建日志表：都是"任务日志"，
// 拆两张表只会让查询与清理逻辑翻倍）。
func (s *CDService) DeploymentLogs(ctx context.Context, id uint64, afterSeq uint32, limit int) ([]map[string]any, error) {
	if _, err := s.deployByID(ctx, id); err != nil {
		return nil, err
	}
	return s.deployLogRows(ctx, id, afterSeq, limit)
}

// DeployLogStreamKey 部署日志流的键。
func DeployLogStreamKey(deployID uint64) string { return logstream.Key("deploy", deployID) }

// ApproveDeployment 审批通过并投池执行。
func (s *CDService) ApproveDeployment(ctx context.Context, id uint64, remark string, c Caller) (*DeployDTO, error) {
	rec, err := s.deployByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.Status != "pending_approval" {
		return nil, errs.StateConflict("该部署单当前状态为「" + statusWordDeploy(rec.Status) + "」，无需审批")
	}
	if c.User != nil {
		if err := s.ensureApprover(ctx, rec.EnvID, c); err != nil {
			return nil, err
		}
		// 自己提交自己审批要拦：审批的意义就在于第二双眼睛。
		if rec.OperatorID == c.User.ID {
			return nil, errs.Forbidden("不能审批自己提交的部署单")
		}
	}

	at := now()
	if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", id).Updates(map[string]any{
		"status": "queued", "approver_id": c.UserID(), "approver_name": c.Name(),
		"approved_at": &at, "updated_at": at,
	}).Error; err != nil {
		return nil, errs.Internalf(err, "更新审批状态失败")
	}
	rec.Status = "queued"
	rec.ApproverName = c.Name()

	if err := s.submitDeploy(ctx, rec); err != nil {
		// 投池失败要把状态退回待审批，否则这张单会卡在 queued 永远不动。
		_ = s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", id).
			Update("status", "pending_approval").Error
		return nil, err
	}

	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "deploy:approve", TargetType: "deployment",
		TargetID: &id, TargetName: rec.DeployNo,
		Detail: fmt.Sprintf("审批通过 %s%s", rec.DeployNo, remarkSuffix(remark)),
	})
	return s.GetDeployment(ctx, id)
}

// RejectDeployment 审批驳回。
func (s *CDService) RejectDeployment(ctx context.Context, id uint64, reason string, c Caller) (*DeployDTO, error) {
	rec, err := s.deployByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.Status != "pending_approval" {
		return nil, errs.StateConflict("只有待审批的部署单可以驳回")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, errs.Invalid("驳回必须填写原因")
	}
	if c.User != nil {
		if err := s.ensureApprover(ctx, rec.EnvID, c); err != nil {
			return nil, err
		}
	}

	at := now()
	if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", id).Updates(map[string]any{
		"status": "canceled", "approver_id": c.UserID(), "approver_name": c.Name(),
		"reject_reason": reason, "approved_at": &at, "finished_at": &at, "updated_at": at,
	}).Error; err != nil {
		return nil, errs.Internalf(err, "更新审批状态失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "deploy:reject", TargetType: "deployment",
		TargetID: &id, TargetName: rec.DeployNo,
		Detail: fmt.Sprintf("审批驳回 %s：%s", rec.DeployNo, reason),
	})
	return s.GetDeployment(ctx, id)
}

// CancelDeployment 取消部署。
func (s *CDService) CancelDeployment(ctx context.Context, id uint64, c Caller) error {
	rec, err := s.deployByID(ctx, id)
	if err != nil {
		return err
	}
	switch rec.Status {
	case "success", "failed", "canceled", "rolled_back":
		return errs.StateConflict("部署单已结束（" + statusWordDeploy(rec.Status) + "），无法取消")
	}
	if rec.Status == "pending_approval" {
		// 待审批的单还没进池，直接落终态即可。
		at := now()
		if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", id).Updates(map[string]any{
			"status": "canceled", "finished_at": &at, "updated_at": at,
			"reject_reason": "提交人主动撤回",
		}).Error; err != nil {
			return errs.Internalf(err, "取消部署失败")
		}
	} else {
		state, inPool := s.deps.Deploys.CancelState(id)
		// 与 CI 同理：两种情况必须由这里补写终态，否则部署单会一直卡在"排队中"——
		//   1. 池里找不到（任务已结束但状态未落库）；
		//   2. 任务当时在排队（移除后不会再执行，没人写它的终态）。
		// 部署池默认并发 1，排队是常态，这条路径比 CI 更容易走到。
		if !inPool || state == queue.StateQueued {
			at := now()
			if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", id).Updates(map[string]any{
				"status": "canceled", "finished_at": &at, "updated_at": at,
			}).Error; err != nil {
				return errs.Internalf(err, "取消部署失败")
			}
			s.deps.Stream.Finish(ctx, DeployLogStreamKey(id), "canceled")
		}
	}
	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "deploy:cancel", TargetType: "deployment",
		TargetID: &id, TargetName: rec.DeployNo, Detail: "取消部署 " + rec.DeployNo,
	})
	return nil
}

// ReconcileInterrupted 把重启后**永远收不了尾**的部署标成 interrupted（启动时跑一次）。
//
// 与 `CIService.ReconcileInterrupted` 同一个理由：部署队列也是进程内的，平台一重启，
// 那些 `queued` / `progressing` 的单子再也没有人会去写终态。
//
// ⚠️ **`pending_approval` 绝不能算进来**：它是合法的长期等待态 —— 审批人可能明天才点，
// 它既没有执行实体、也不依赖进程存活。把它标成"中断"等于**凭空替用户撤单**。
// 判据刻意只取 `queued` / `progressing`，与 `/system/stats` 数"进行中部署"的口径一致。
func (s *CDService) ReconcileInterrupted(ctx context.Context) (int64, error) {
	// 取**完整记录**：除了算真实耗时（见下面的 duration_sec）要用的 started_at / updated_at，
	// 收尾还要拿 env_key / project_key / deploy_no / image / operator_name 去发通知。
	// **必须在 UPDATE 前取** —— 那次 UPDATE 会改写 updated_at，也会把 status 改掉。
	var rows []model.DeployRecord
	if err := s.db(ctx).Model(&model.DeployRecord{}).
		Where("status IN ?", []string{"queued", "progressing"}).
		Find(&rows).Error; err != nil {
		return 0, errs.Internalf(err, "查询中断的部署失败")
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	fin := now()
	if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id IN ?", ids).
		Updates(map[string]any{
			"status":        "interrupted",
			"error_code":    "INTERRUPTED",
			"error_message": "平台重启，部署执行随进程中消失而被中断",
			"finished_at":   &fin,
			"updated_at":    fin,
		}).Error; err != nil {
		return 0, errs.Internalf(err, "标记中断的部署失败")
	}
	// 同 CI：显式写 duration_sec，否则 `dto.go` 会拿 finished_at - started_at 兜底，
	// 把平台宕机的那几个小时算进"部署耗时"里，界面上就是一个凭空冒出来的大数字。
	for _, r := range rows {
		if r.StartedAt == nil {
			continue
		}
		sec := int64(r.UpdatedAt.Sub(*r.StartedAt).Seconds())
		if sec < 0 {
			sec = 0
		}
		if err := s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", r.ID).
			Update("duration_sec", uint32(sec)).Error; err != nil {
			continue
		}
	}
	for _, id := range ids {
		_ = s.deps.Stream.Finish(ctx, DeployLogStreamKey(id), "interrupted")
	}
	// 回滚记录要**跟着一起收**。
	//
	// 回滚复用部署状态机：`rollback_record.status` 唯一的收尾写入点是
	// `finishRollbackOf`（由部署执行器在跑完时调用）。进程一重启，那次调用就没有了，
	// 于是部署单被标成「已中断」，而回滚记录仍停在默认值 `progressing` ——
	// 「回滚记录」页上就是一条永远转不完的「执行中」，和刚修掉的是同一个毛病。
	//
	// ⚠️ 只扫 `deploy_id IN ids`（本次确实被打断的那些部署单），不按 status 全量扫：
	// 一条 `progressing` 但部署单早已终态的回滚记录，成因是"收尾写入失败"，
	// 那是另一件事，不该由启动补偿顺手替它下结论。
	if err := s.db(ctx).Model(&model.RollbackRecord{}).Where("deploy_id IN ?", ids).
		Update("status", "interrupted").Error; err != nil {
		s.log("cd.reconcile").Warn("标记中断的回滚记录失败", "err", err)
	}

	// 最后才告警：库先写对，再发通知。
	//
	// 顺序颠倒的话，通知已经发出去、改库却失败，就成了"群里说中断了、界面上还在跑"。
	// 与 CI 侧同一个理由（见 `CIService.ReconcileInterrupted` 末尾）。
	s.notify.Dispatch(ctx, s.notify.DeployInterruptedMessage(ptrRecords(rows)))
	return int64(len(ids)), nil
}

// RetryDeployment 失败重试。
func (s *CDService) RetryDeployment(ctx context.Context, id uint64, c Caller) (*DeployDTO, error) {
	rec, err := s.deployByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// `interrupted`（平台重启时中断）也要放行：它不是"这次发布有问题"，
	// 而是"根本没跑完"，用户最自然的动作就是原样再来一次。
	if rec.Status != "failed" && rec.Status != "interrupted" {
		return nil, errs.StateConflict("只有失败或中断的部署单可以重试")
	}
	in := CreateDeploymentInput{
		EnvKey:     rec.EnvKey,
		ProjectKey: rec.ProjectKey,
		Replicas:   &rec.Replicas,
		Strategy:   rec.Strategy,
		Remark:     fmt.Sprintf("重试 %s", rec.DeployNo),
	}
	if rec.ImageRecID != 0 {
		in.ImageRecordID = httpx.IDString(rec.ImageRecID)
	} else {
		// 这一单是**从仓库标签发的**（没有镜像记录）。此时**不能什么都不传** ——
		// 那会落到 resolveDeployImage 的"取该项目最新镜像"，也就是重试时
		// **悄悄把版本换成了另一个**，而界面上还写着"重试 DPxxxx"。
		in.ImageOverride = rec.Image
	}
	dto, err := s.CreateDeployment(ctx, in, c)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "deploy:retry", TargetType: "deployment",
		TargetID: &id, TargetName: rec.DeployNo,
		Detail: fmt.Sprintf("重试部署 %s → 新单 %s", rec.DeployNo, dto.DeployNo),
	})
	return dto, nil
}

// PendingApprovals 待我审批列表。
func (s *CDService) PendingApprovals(ctx context.Context, c Caller) ([]DeployDTO, error) {
	if c.User == nil {
		return nil, errs.Unauthorized("请先登录")
	}
	// 只看"需要审批的环境"里的待审单：不需要审批的环境不会产生 pending_approval。
	db := s.db(ctx).Model(&model.DeployRecord{}).Where("status = ?", "pending_approval")
	// 管理员 / 运维能看到全部；其余人只看自己是审批人的环境。
	if !c.User.HasAnyPerm("*", "deploy:approve") {
		envIDs, err := s.envIDsApprovedBy(ctx, c.User.ID)
		if err != nil {
			return nil, err
		}
		if len(envIDs) == 0 {
			return []DeployDTO{}, nil
		}
		db = db.Where("env_id IN ?", envIDs)
	}
	var rows []model.DeployRecord
	if err := db.Order("created_at desc").Limit(100).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询待审批部署失败")
	}
	return s.decorateDeploys(ctx, rows, false), nil
}

// DeploymentAIDiagnosis 部署异常诊断与回滚建议。
//
// 两级来源，与 CIService.BuildAIDiagnosis **同一套口径**：
//
//  1. model —— 首选。把部署上下文、平台部署日志尾部与**集群侧证据**
//     （容器状态 / 退出码 / 崩溃前那一轮日志 / Pod 事件）交给平台默认模型通道。
//  2. rule  —— 降级。通道未配置 / 调用失败 / 输出无法解析时，退回按错误码的确定性结论，
//     并用 notice 说明模型为什么没参与。
//
// 为什么降级而不是把错误抛给前端：错误码本身已经指明了方向（TIMEOUT 与 ROLLOUT_STUCK
// 的处置完全不同），让用户拿不到任何结论，比拿到一个**标注了来源**的结论更糟。
//
// 回滚建议里带上"上一版镜像"，让前端可以直接给出"一键回滚"按钮 ——
// 平台不再自动回滚之后，这个按钮就是失败后的主要恢复通道。
func (s *CDService) DeploymentAIDiagnosis(ctx context.Context, id uint64) (*DeployDiagnosis, error) {
	rec, err := s.deployByID(ctx, id)
	if err != nil {
		return nil, err
	}
	var svc model.ServiceInstance
	_ = s.db(ctx).First(&svc, rec.ServiceID).Error
	var env model.Environment
	_ = s.db(ctx).First(&env, rec.EnvID).Error

	// 找上一版：同一服务、成功部署过、镜像与本次不同、时间在本次之前。
	var prev model.DeployRecord
	prevErr := s.db(ctx).Where("service_id = ? AND status IN ? AND image <> ? AND created_at < ?",
		rec.ServiceID, []string{"success", "rolled_back"}, rec.Image, rec.CreatedAt).
		Order("created_at desc").First(&prev).Error

	out := &DeployDiagnosis{
		DeployNo:     rec.DeployNo,
		CurrentImage: rec.Image,
	}
	s.fillDeployDiagnosis(ctx, out, rec, &svc, &env)
	if prevErr == nil {
		out.RollbackAvailable = true
		out.RollbackImage = prev.Image
		out.RollbackDeployNo = prev.DeployNo
		if svc.ID != 0 {
			out.ServiceID = httpx.IDString(svc.ID)
		}
	}
	return out, nil
}

// fillDeployDiagnosis 填结论与来源：模型优先、规则兜底，且**如实标注**。
func (s *CDService) fillDeployDiagnosis(
	ctx context.Context, out *DeployDiagnosis,
	rec *model.DeployRecord, svc *model.ServiceInstance, env *model.Environment,
) {
	if rec.Status != "failed" {
		out.Source = diagnosisSourceRule
		out.Reason = "该部署单当前没有异常"
		out.Suggestion = "无需回滚"
		out.Confidence = 1
		return
	}

	// 缓存命中：上一轮模型已经归因过。同一张失败单的日志与错误码不会再变，
	// 重算只是重复花钱与重复等待。⚠️ 只认 `source=model` 的那条 ——
	// 失败时 failDeploy 写进去的是**规则**结论，把它当模型结论返回就是谎报来源。
	if d := decodeAIDiagnosis(rec.AIDiagnosis); d != nil && d.Source == diagnosisSourceModel {
		out.Source = diagnosisSourceModel
		out.Model = d.Model
		out.Reason, out.Suggestion, out.Confidence = d.Reason, d.Suggestion, d.Confidence
		return
	}

	rule := ruleDeployDiagnosis(rec)
	res, merr := s.deployModelDiagnosis(ctx, rec, svc, env)
	if merr != nil {
		s.log("cd.deploy").Warn("模型归因未生效，已降级为错误码规则", "deploy", rec.ID, "err", merr)
		out.Source = diagnosisSourceRule
		out.Reason, out.Suggestion, out.Confidence = rule.Reason, rule.Suggestion, rule.Confidence
		out.Notice = "本次未由模型分析（" + merr.Error() + "），以下结论来自平台错误码规则。"
		return
	}
	out.Source = diagnosisSourceModel
	out.Model = res.Model
	out.Reason, out.Suggestion, out.Confidence = res.Reason, res.Suggestion, res.Confidence
	if err := s.saveDeployModelDiagnosis(ctx, rec.ID, res); err != nil {
		// 缓存写失败不影响本次结论：用户已经拿到东西了，只是下次要重算。
		s.log("cd.deploy").Warn("写入模型归因结果失败", "deploy", rec.ID, "err", err)
	}
}

// ruleDeployDiagnosis 错误码规则归因：先看库里存的（失败时写进去的那条），再看规则表。
//
// 刻意跳过 `source=model` 的记录：那条是模型的结论，规则路径不能拿它冒充自己的输出
// （否则降级发生时，用户会看到"本地规则"标签配着模型写的文案）。
func ruleDeployDiagnosis(rec *model.DeployRecord) *AIDiagnosisDTO {
	if d := decodeAIDiagnosis(rec.AIDiagnosis); d != nil && d.Source != diagnosisSourceModel {
		return d
	}
	if d := diagnoseDeploy(rec.ErrorCode); d != nil {
		return d
	}
	return &AIDiagnosisDTO{
		Reason:     "部署失败，未识别到已知原因",
		Suggestion: "请查看部署日志中的最后几条错误输出定位问题",
		Confidence: 0.3,
	}
}

// DeployDiagnosis 部署诊断结果。
type DeployDiagnosis struct {
	// Source 结论来源：model（模型分析）/ rule（平台错误码规则）。前端按它分叉措辞。
	Source string `json:"source"`
	// Model 模型通道名，仅 source=model 时有值。
	Model string `json:"model,omitempty"`
	// Notice 需要如实告知用户的附加说明 —— 目前用于"这次为什么没用上模型"。
	Notice            string  `json:"notice,omitempty"`
	DeployNo          string  `json:"deployNo"`
	CurrentImage      string  `json:"currentImage"`
	Reason            string  `json:"reason"`
	Suggestion        string  `json:"suggestion"`
	Confidence        float64 `json:"confidence"`
	RollbackAvailable bool    `json:"rollbackAvailable"`
	RollbackImage     string  `json:"rollbackImage,omitempty"`
	RollbackDeployNo  string  `json:"rollbackDeployNo,omitempty"`
	ServiceID         string  `json:"serviceId,omitempty"`
}

// --------------------------------------------------------------------------- 回滚

// CreateRollback 执行回滚。
//
// 回滚复用部署状态机（docs/01 的设计），所以这里做的是：
// 建一条 DeployRecord（parent 指向被回滚的那次）+ 一条 RollbackRecord（记录语义），
// 然后走与普通部署完全相同的执行路径。
// 这样做的好处是日志、步骤、审批、并发控制全部自动继承，不需要为回滚写第二套。
func (s *CDService) CreateRollback(ctx context.Context, in CreateRollbackInput, c Caller) (*RollbackDTO, error) {
	svcID, err := httpx.ParseIDString(in.ServiceID)
	if err != nil {
		return nil, err
	}
	svc, env, err := s.serviceWithEnv(ctx, svcID)
	if err != nil {
		return nil, err
	}
	if !env.Enabled {
		return nil, errs.New(errs.CodeEnvDisabled, "环境已停用，禁止回滚部署")
	}

	target, err := s.resolveRollbackTarget(ctx, svc, in)
	if err != nil {
		return nil, err
	}
	if target.Image == svc.CurrentImage {
		return nil, errs.StateConflict("目标版本与当前版本相同，无需回滚")
	}

	var active int64
	if err := s.db(ctx).Model(&model.DeployRecord{}).
		Where("service_id = ? AND status IN ?", svc.ID, []string{"pending_approval", "queued", "progressing"}).
		Count(&active).Error; err != nil {
		return nil, errs.Internalf(err, "检查进行中的部署失败")
	}
	if active > 0 {
		return nil, errs.StateConflict("该服务已有进行中的部署，请等待完成后再回滚")
	}

	// 被回滚的那次部署：优先取"当前生效的那一单"，用它作为 parent。
	var current model.DeployRecord
	hasCurrent := s.db(ctx).Where("service_id = ? AND status IN ?", svc.ID, []string{"success", "rolled_back"}).
		Order("created_at desc").First(&current).Error == nil

	deployNo, err := s.nextDeployNo(ctx)
	if err != nil {
		return nil, err
	}

	nowT := now()
	status := "queued"
	if env.NeedApproval {
		status = "pending_approval"
	}

	rec := model.DeployRecord{
		DeployNo:     deployNo,
		EnvID:        env.ID,
		EnvKey:       env.EnvKey,
		ProjectID:    svc.ProjectID,
		ProjectKey:   svc.ProjectKey,
		ServiceID:    svc.ID,
		ImageRecID:   target.ID,
		Image:        target.Image,
		Replicas:     svc.Replicas,
		Strategy:     "RollingUpdate",
		Remark:       "回滚：" + in.Reason,
		Status:       status,
		Progress:     0,
		OperatorID:   derefUint64(c.UserID()),
		OperatorName: c.Name(),
		CreatedAt:    nowT,
		UpdatedAt:    nowT,
	}
	if hasCurrent {
		rec.ParentDeploy = &current.ID
	}
	if err := s.db(ctx).Create(&rec).Error; err != nil {
		return nil, errs.Internalf(err, "创建回滚部署单失败")
	}
	if err := s.initDeploySteps(ctx, rec.ID); err != nil {
		return nil, err
	}

	rb := model.RollbackRecord{
		DeployID:     rec.ID,
		DeployNo:     deployNo,
		EnvID:        env.ID,
		EnvKey:       env.EnvKey,
		ProjectID:    svc.ProjectID,
		ProjectKey:   svc.ProjectKey,
		ServiceID:    svc.ID,
		FromImage:    svc.CurrentImage,
		ToImage:      target.Image,
		Reason:       in.Reason,
		TriggerType:  stringOr(in.TriggerType, "manual"),
		AIConfidence: in.Confidence,
		OperatorID:   derefUint64(c.UserID()),
		OperatorName: c.Name(),
		Status:       "progressing",
		CreatedAt:    nowT,
		UpdatedAt:    nowT,
	}
	if hasCurrent {
		rb.FromImageID = current.ImageRecID
	}
	rb.ToImageID = target.ID
	if err := s.db(ctx).Create(&rb).Error; err != nil {
		return nil, errs.Internalf(err, "创建回滚记录失败")
	}

	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "rollback:execute", TargetType: "rollback",
		TargetID: &rb.ID, TargetName: deployNo,
		Detail: fmt.Sprintf("执行回滚：%s/%s %s → %s（%s）",
			env.EnvKey, svc.ProjectKey, tagOfImage(svc.CurrentImage), tagOfImage(target.Image), in.Reason),
	})

	if status == "pending_approval" {
		// 回滚也会落待审批（目标环境开了审批）。同建单：先叫人，否则这单没人推进。
		s.notify.Dispatch(ctx, s.notify.DeployPendingMessage(&rec))
	} else {
		if err := s.submitDeploy(ctx, &rec); err != nil {
			_ = s.db(ctx).Model(&model.RollbackRecord{}).Where("id = ?", rb.ID).
				Update("status", "failed").Error
			return nil, err
		}
	}
	return s.GetRollback(ctx, rb.ID)
}

// ListRollbacks 回滚记录列表。
func (s *CDService) ListRollbacks(ctx context.Context, q DeploymentQuery) (*httpx.PageQuery, any, int64, error) {
	pq := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize, SortBy: q.SortBy, Order: q.Order, Keyword: q.Keyword}
	column, desc := pq.Normalize(map[string]string{
		"createdAt": "created_at",
		"status":    "status",
	}, "created_at")

	db := s.db(ctx).Model(&model.RollbackRecord{})
	if v := strings.TrimSpace(q.EnvKey); v != "" {
		db = db.Where("env_key = ?", v)
	}
	if v := strings.TrimSpace(q.ProjectKey); v != "" {
		db = db.Where("project_key = ?", v)
	}
	if v := strings.TrimSpace(q.Status); v != "" {
		db = db.Where("status = ?", v)
	}
	// 触发方式（manual / ai_suggested）：前端"AI 建议"筛选靠它，
	// 否则只能把全量记录拉到前端过滤——那和有分页的列表是矛盾的。
	if v := strings.TrimSpace(q.TriggerType); v != "" {
		db = db.Where("trigger_type = ?", v)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("project_key LIKE ? OR reason LIKE ? OR deploy_no LIKE ?", like, like, like)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "统计回滚记录失败")
	}
	var rows []model.RollbackRecord
	if err := db.Order(column + " " + orderWord(desc)).
		Offset(pq.Offset()).Limit(pq.PageSize).Find(&rows).Error; err != nil {
		return nil, nil, 0, errs.Internalf(err, "查询回滚记录失败")
	}

	envNames, err := s.envNames(ctx, envKeysOfRollbacks(rows))
	if err != nil {
		return nil, nil, 0, err
	}
	projNames, err := s.projectNames(ctx, projectKeysOfRollbacks(rows))
	if err != nil {
		return nil, nil, 0, err
	}
	out := make([]RollbackDTO, 0, len(rows))
	for i := range rows {
		out = append(out, rollbackDTO(&rows[i], envNames[rows[i].EnvKey], projNames[rows[i].ProjectKey]))
	}
	return &pq, out, total, nil
}

// GetRollback 回滚详情。
func (s *CDService) GetRollback(ctx context.Context, id uint64) (*RollbackDTO, error) {
	var rb model.RollbackRecord
	if err := s.db(ctx).First(&rb, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("回滚记录不存在")
		}
		return nil, errs.Internalf(err, "查询回滚记录失败")
	}
	envNames, err := s.envNames(ctx, []string{rb.EnvKey})
	if err != nil {
		return nil, err
	}
	projNames, err := s.projectNames(ctx, []string{rb.ProjectKey})
	if err != nil {
		return nil, err
	}
	dto := rollbackDTO(&rb, envNames[rb.EnvKey], projNames[rb.ProjectKey])
	return &dto, nil
}

// --------------------------------------------------------------------------- 内部助手

func (s *CDService) envByKey(ctx context.Context, key string) (*model.Environment, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errs.Invalid("环境标识不能为空")
	}
	var e model.Environment
	if err := s.db(ctx).Where("env_key = ?", strings.TrimSpace(key)).First(&e).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("环境 %s 不存在", key)
		}
		return nil, errs.Internalf(err, "查询环境失败")
	}
	return &e, nil
}

func (s *CDService) envByID(ctx context.Context, id uint64) (*model.Environment, error) {
	var e model.Environment
	if err := s.db(ctx).First(&e, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("环境不存在")
		}
		return nil, errs.Internalf(err, "查询环境失败")
	}
	return &e, nil
}

func (s *CDService) serviceByID(ctx context.Context, id uint64) (*model.ServiceInstance, error) {
	var svc model.ServiceInstance
	if err := s.db(ctx).First(&svc, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("服务实例不存在")
		}
		return nil, errs.Internalf(err, "查询服务实例失败")
	}
	return &svc, nil
}

func (s *CDService) serviceByEnvProject(ctx context.Context, envID, projectID uint64) (*model.ServiceInstance, error) {
	var svc model.ServiceInstance
	if err := s.db(ctx).Where("env_id = ? AND project_id = ?", envID, projectID).First(&svc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("该环境尚未纳管此项目：请先在环境详情里关联该项目")
		}
		return nil, errs.Internalf(err, "查询服务实例失败")
	}
	return &svc, nil
}

func (s *CDService) serviceWithEnv(ctx context.Context, id uint64) (*model.ServiceInstance, *model.Environment, error) {
	svc, err := s.serviceByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	env, err := s.envByID(ctx, svc.EnvID)
	if err != nil {
		return nil, nil, err
	}
	return svc, env, nil
}

func (s *CDService) deployByID(ctx context.Context, id uint64) (*model.DeployRecord, error) {
	var rec model.DeployRecord
	if err := s.db(ctx).First(&rec, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("部署单不存在")
		}
		return nil, errs.Internalf(err, "查询部署单失败")
	}
	return &rec, nil
}

func (s *CDService) projectByKey(ctx context.Context, key string) (*model.Project, error) {
	var p model.Project
	if err := s.db(ctx).Where("project_key = ?", strings.TrimSpace(key)).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("项目 %s 不存在", key)
		}
		return nil, errs.Internalf(err, "查询项目失败")
	}
	return &p, nil
}

// resolveDeployImage 决定本次部署用哪个镜像版本。
//
// 优先级：显式 imageRecordId > tag 匹配 > 该项目最新推送的镜像。
// 显式指定优先是刻意的——回滚与"部署指定历史版本"都依赖它，
// 若被"最新版"覆盖，回滚就永远回不到旧版本。
// checkImageSource 校验「平台构建 / 仓库标签」二选一。
//
// 单独一个函数是为了让这条约束**只写一遍**：它同时约束 HTTP 入口与内部复用路径，
// 散在 CreateDeployment 里改一处漏一处，最后又会变成"某个入口能发两个"。
//
// ImageOverride 是平台内部复用（重新部署 / 重试），不参与这个判断 ——
// 它本来就是"照上一单原样再来一次"，不是一次新的选择。
func checkImageSource(in CreateDeploymentInput) error {
	if strings.TrimSpace(in.ImageOverride) != "" {
		return nil
	}
	platformGiven := strings.TrimSpace(in.ImageRecordID) != "" || strings.TrimSpace(in.Tag) != ""
	registryGiven := strings.TrimSpace(in.RegistryTag) != ""

	switch {
	case platformGiven && registryGiven:
		return errs.Invalid("镜像来源只能选一个：要么「平台构建」的镜像记录，要么「仓库」里的标签，不能同时选")
	// 下面两条放在"两个都没给"之前：用户已经声明了来源却没给值，
	// 说"请选择要部署的镜像"会显得答非所问（他选了，只是漏填了值）。
	case strings.EqualFold(strings.TrimSpace(in.ImageSource), "registry") && !registryGiven:
		return errs.Invalid("镜像来源选了「仓库」，但没有给出标签名")
	case strings.EqualFold(strings.TrimSpace(in.ImageSource), "platform") && registryGiven:
		return errs.Invalid("镜像来源选了「平台构建」，但提交的是仓库标签")
	case !platformGiven && !registryGiven:
		return errs.Invalid("请选择要部署的镜像：平台构建的镜像记录，或仓库里的标签")
	}
	return nil
}

// imageRecordFromRef 把一串完整镜像引用包成一条**不入库**的 ImageRecord。
//
// ID 留 0：它的唯一作用是"把这次要发的镜像串与摘要带下去"。
// 部署单靠 Image / ImageDigest 两个字段记住事实，不靠外键。
func imageRecordFromRef(ref, digest string) *model.ImageRecord {
	registry, repo, tag := splitImageRef(ref)
	return &model.ImageRecord{
		Registry: registry, Repo: repo, Tag: tag, Image: ref, Digest: digest,
	}
}

// resolveDeployImage 解析出这次要部署的镜像。
func (s *CDService) resolveDeployImage(ctx context.Context, proj *model.Project, in CreateDeploymentInput) (*model.ImageRecord, error) {
	// ① 平台内部复用（重新部署 / 重试）：**原样**用那个镜像串。
	//    不能退回"去库里找记录"——找不到就落进"取最新"，那是静默换版本。
	if raw := strings.TrimSpace(in.ImageOverride); raw != "" {
		return imageRecordFromRef(raw, ""), nil
	}

	// ② 仓库标签来源
	if strings.TrimSpace(in.RegistryTag) != "" ||
		strings.EqualFold(strings.TrimSpace(in.ImageSource), "registry") {
		return s.resolveRegistryImage(ctx, proj, in)
	}

	// ③ 平台构建来源（原有路径）
	if raw := strings.TrimSpace(in.ImageRecordID); raw != "" {
		id, err := httpx.ParseIDString(raw)
		if err != nil {
			return nil, err
		}
		var img model.ImageRecord
		if err := s.db(ctx).First(&img, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errs.NotFoundf("指定的镜像版本不存在")
			}
			return nil, errs.Internalf(err, "查询镜像失败")
		}
		if img.ProjectKey != proj.ProjectKey {
			return nil, errs.Invalidf("镜像 %s 不属于项目 %s", img.Tag, proj.ProjectKey)
		}
		return &img, nil
	}

	db := s.db(ctx).Where("project_key = ?", proj.ProjectKey)
	if tag := strings.TrimSpace(in.Tag); tag != "" {
		db = db.Where("tag = ?", tag)
	}
	var img model.ImageRecord
	if err := db.Order("pushed_at desc").First(&img).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.StateConflict("该项目还没有可部署的镜像，请先执行一次构建")
		}
		return nil, errs.Internalf(err, "查询镜像失败")
	}
	return &img, nil
}

// resolveRegistryImage 按「仓库里的标签」解析镜像。
//
// image_record 保留它自己的语义（**平台构建过的记录**），所以别的平台推上来的 tag
// **不写进去** —— 写进去的话，镜像版本列表里会出现一堆平台从没构建过、
// 也没有 commit / 构建号可溯源的条目。这里只借 ImageRecord 的形状往下传，
// ID 留 0，事实记在部署单的 Image / ImageDigest 上。
//
// 与 preflightImage 的纪律一致：**只有仓库明确回答 404 才拒绝**。
// 仓库不可达 / 无权读时放行但记日志 —— 把一次仓库抖动变成"不能发版"，
// 比放过一个可能不存在的 tag 更糟（后者最多失败一次，还有回滚兜底）。
func (s *CDService) resolveRegistryImage(ctx context.Context, proj *model.Project, in CreateDeploymentInput) (*model.ImageRecord, error) {
	tag := strings.TrimSpace(in.RegistryTag)
	if tag == "" {
		return nil, errs.Invalid("镜像来源是「仓库」时必须指定标签名")
	}
	reg, cfg, err := s.registryClient(ctx)
	if err != nil {
		return nil, errs.Internalf(err, "读取 Harbor 配置失败")
	}
	if reg == nil || cfg.BaseURL == "" {
		return nil, errs.StateConflict("还没有可用的镜像仓库配置：请先在「系统管理 › 第三方集成」里接入 Harbor")
	}
	core := integration.NewHarborCore(cfg)
	if core == nil {
		return nil, errs.StateConflict("还没有可用的镜像仓库配置：请先在「系统管理 › 第三方集成」里接入 Harbor")
	}

	host := registryTagHost(cfg)
	repo, _, reason, err := s.harborRepoForProject(ctx, proj, core, host)
	if err != nil {
		return nil, err
	}
	if repo == "" {
		// 定位不到仓库就是"发不了"，原因原样给用户（见 harborRepoForProject 的措辞）。
		return nil, errs.StateConflict(reason)
	}

	image := strings.Trim(host+"/"+repo, "/") + ":" + tag

	digest := ""
	if reg.Accepts(host) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), imageResolveTimeout)
		defer cancel()
		info, rerr := reg.Resolve(rctx, repo, tag)
		switch {
		case rerr == nil && info != nil:
			digest = info.Digest
		case errors.Is(rerr, integration.ErrTagNotFound):
			return nil, errs.Invalidf("仓库里没有 %s:%s 这个标签，"+
				"请重新选择（列表可能已过期，别人删过这个标签）", repo, tag)
		default:
			// 仓库不可达 / 无权读：放行，但把"没能拿到摘要"如实记下来。
			s.log("cd.deploy").Warn("读仓库标签的摘要未完成（仓库不可达或无权读取），已放行",
				"repo", repo, "tag", tag, "err", rerr)
		}
	} else {
		s.log("cd.deploy").Warn("镜像仓库地址不在平台管辖范围内，跳过摘要读取",
			"registry", host)
	}

	return imageRecordFromRef(image, digest), nil
}

// preflightImage 在发单前确认镜像 tag 真的在仓库里。
//
// 判定刻意保守 —— **只有仓库明确回答 404 才拒绝**，其余一律放行（只记一条日志）：
//   - 没接真实仓库（本地实现谁都不认）；
//   - registry 不是本平台管的那个（问错仓库会拿到别人的事实）；
//   - 网络不通 / 超时 / 凭据无权读。
//
// 因为预检的价值是挡掉"必然失败"，不是给发版新添一个外部依赖。
// 把仓库抖动变成"不能发版"，比放过一个坏 tag 更糟 —— 后者最多失败一次并且
// 有自动回滚兜底，前者是整条发布链路停摆。
func (s *CDService) preflightImage(ctx context.Context, image string) error {
	registry, repo, tag := splitImageRef(image)
	if repo == "" || tag == "" {
		return nil
	}
	// ⚠️ 这里用的是装配层的 Registry（启动时按 config.yaml 建好的那份），
	// 而不是 registryClient(ctx) 的运行期客户端。原因有两条，都刻意：
	//   - 预检是**尽力而为**的守卫（只拦"仓库明确 404"），为它多读一次库不划算；
	//   - 用哪份配置对它的结论影响很小：地址换了之后 Accepts() 会直接不认，
	//     结果是"跳过预检"，**不会**误拦（详见 harborCfg/registryClient 的注释）。
	// 新增的「仓库标签」那两条路径必须与配置页一致，所以它们用 registryClient。
	reg := s.deps.Up.Registry
	if reg == nil || !reg.Accepts(registry) {
		return nil
	}
	// 剥掉调用方 ctx 的取消信号、只留一个短超时：预检不该被上一层超时牵连，
	// 也不该把发单接口拖长（8s 是仓库元数据查询的统一上限，与 CI 侧一致）。
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), imageResolveTimeout)
	defer cancel()
	if _, err := reg.Resolve(rctx, repo, tag); err != nil {
		if errors.Is(err, integration.ErrTagNotFound) {
			return errs.Invalidf("镜像 %s:%s 不在仓库中，这次发布必然失败："+
				"请先在 CI 里构建出这个 tag，或改选一个仓库里已有的版本", repo, tag)
		}
		s.log("cd.deploy").Warn("镜像预检未完成（仓库不可达或无权读取），已放行",
			"image", image, "err", err)
	}
	return nil
}

// resolveRollbackTarget 解析回滚目标版本。
func (s *CDService) resolveRollbackTarget(ctx context.Context, svc *model.ServiceInstance, in CreateRollbackInput) (*model.ImageRecord, error) {
	if raw := strings.TrimSpace(in.ToImageID); raw != "" {
		id, err := httpx.ParseIDString(raw)
		if err != nil {
			return nil, err
		}
		var img model.ImageRecord
		if err := s.db(ctx).First(&img, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errs.NotFoundf("回滚目标版本不存在")
			}
			return nil, errs.Internalf(err, "查询镜像失败")
		}
		if img.ProjectKey != svc.ProjectKey {
			return nil, errs.Invalidf("回滚目标镜像不属于项目 %s", svc.ProjectKey)
		}
		return &img, nil
	}
	if image := strings.TrimSpace(in.ToImage); image != "" {
		var img model.ImageRecord
		if err := s.db(ctx).Where("image = ?", image).First(&img).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// 这一版可能是**从仓库标签发的**（没有镜像记录）。此时**不能**直接报
				// "不在镜像记录中"——记录从来没存在过，不是丢了。
				// 改从本服务的部署历史里认这一版：既拿到了事实，又保留了"必须属于本项目
				// 本服务"的关键校验（这正是这个分支存在的理由）。
				var past model.DeployRecord
				perr := s.db(ctx).Where("service_id = ? AND project_key = ? AND image = ? AND status IN ?",
					svc.ID, svc.ProjectKey, image, []string{"success", "rolled_back"}).
					Order("created_at desc").First(&past).Error
				if perr != nil {
					return nil, errs.NotFoundf("回滚目标镜像 %s 不在本项目 %s 的部署历史中",
						image, svc.ProjectKey)
				}
				return imageRecordFromRef(past.Image, past.ImageDigest), nil
			}
			return nil, errs.Internalf(err, "查询镜像失败")
		}
		// 与上面的 ToImageID 分支**同口径**的项目校验，不能只判 ID 那条路。
		//
		// `image` 是全仓库唯一的字符串，**不含项目信息**：拿另一个项目的 tag 照样能
		// 查出这条记录。前端曾因"切换项目后 Select 的 value 留在上一个项目"
		// （useApi 换依赖不清空旧 data + 预选 effect 拿旧值先跑）把上个项目的镜像原样
		// 发过来，而这里不校验 → 真把别的项目的镜像回滚进本服务。前端那个洞已堵，
		// 但校验必须留在服务端：它才是"这个目标能不能用"的最终判据。
		if img.ProjectKey != svc.ProjectKey {
			return nil, errs.Invalidf("回滚目标镜像不属于项目 %s", svc.ProjectKey)
		}
		return &img, nil
	}
	// 都没给：取该服务最近一次成功部署之外的上一版。
	var prev model.DeployRecord
	if err := s.db(ctx).
		Where("service_id = ? AND status IN ? AND image <> ?", svc.ID, []string{"success", "rolled_back"}, svc.CurrentImage).
		Order("created_at desc").First(&prev).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.StateConflict("找不到可回滚的历史版本")
		}
		return nil, errs.Internalf(err, "查询历史版本失败")
	}
	if prev.ImageRecID == 0 {
		// 上一版是「仓库标签」来源，本来就没有镜像记录，按它当时用的镜像原样回滚。
		// 这里如果照旧去查镜像记录，用户看到的会是"记录不存在（可能已被清理）"——
		// 一个与服务状态无关、也帮不上任何忙的原因。
		return imageRecordFromRef(prev.Image, prev.ImageDigest), nil
	}
	var img model.ImageRecord
	if err := s.db(ctx).First(&img, prev.ImageRecID).Error; err != nil {
		return nil, errs.NotFoundf("上一版镜像记录不存在（可能已被清理）")
	}
	return &img, nil
}

// isDupKeyOn 判断这个错误是不是**某个具名唯一索引**的冲突（MySQL 1062）。
//
// 为什么不用 `gorm.ErrDuplicatedKey`：它需要开启 `TranslateError`，而本项目没开 ——
// 用一个恒为 false 的判断当守卫，比不判断更危险（错误会被静默地归到"别的原因"）。
// 这里按错误文本判，同时要求出现索引名，避免把"幂等键冲突"与"单号冲突"混为一谈
//（两者的处置完全不同：前者返回已有单，后者重算重试）。
func isDupKeyOn(err error, index string) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Error 1062") && strings.Contains(s, index)
}

// deployNoMu 串行化"**算号 + 落库**"这一整段。
//
// ⚠️ 必须包住**两者**，只包住"算号"是没用的：号要 INSERT 时才落库，
// 中间留窗口的话两个并发请求会先后拿到同一个号。实测（2026-10-16）：
// 只锁算号时，一次 6 并发的批量发布仍有 2 笔撞号 —— 虽然重试把它们兜住了，
// 但每次并发都撞几下是长期的日志噪音，而且重试有次数上限。
//
// 代价是"建单"这一段串行化：一次 INSERT 几毫秒，而部署单本来就按
// `DeployConcurrency` 串行执行，所以这点串行对整体吞吐没有影响。
//
// 跨实例（多副本部署）时锁各管各的，那时仍然靠调用处的重试兜底。
var deployNoMu sync.Mutex

// nextDeployNo 生成部署单号：DP + 日期 + 当日序号。
// 可读性优先于实现复杂度——运维在群里沟通时说的就是这个号。
//
// 它自己持锁，供"只想要个号"的调用方使用。**要在同一临界区里插入时**
// （如 CreateDeployment），请用 nextDeployNoLocked 自己持锁包住两段。
func (s *CDService) nextDeployNo(ctx context.Context) (string, error) {
	deployNoMu.Lock()
	defer deployNoMu.Unlock()
	return s.nextDeployNoLocked(ctx)
}

// nextDeployNoLocked 与 nextDeployNo 同逻辑，但**不加锁**：调用方必须已持有 deployNoMu。
func (s *CDService) nextDeployNoLocked(ctx context.Context) (string, error) {
	day := now().Format("20060102")
	prefix := "DP" + day
	var last model.DeployRecord
	err := s.db(ctx).Where("deploy_no LIKE ?", prefix+"%").Order("deploy_no desc").First(&last).Error
	seq := 1
	if err == nil {
		if n, convErr := parseUint32(last.DeployNo[len(prefix):]); convErr == nil {
			seq = int(n) + 1
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", errs.Internalf(err, "生成部署单号失败")
	}
	return fmt.Sprintf("%s%04d", prefix, seq), nil
}

// initDeploySteps 预置四个步骤，与下游 ApplyImage 的阶段名一一对应。
// 前端因此可以在部署刚创建时就把四个步骤灰显出来，而不是等回调才知道有几步。
func (s *CDService) initDeploySteps(ctx context.Context, deployID uint64) error {
	names := []string{"拉取镜像", "更新工作负载", "就绪探针", "流量接入"}
	rows := make([]model.DeployStep, 0, len(names))
	for i, n := range names {
		rows = append(rows, model.DeployStep{
			DeployID: deployID,
			Seq:      uint8(i + 1),
			Name:     n,
			Status:   "pending",
		})
	}
	if err := s.db(ctx).Create(&rows).Error; err != nil {
		return errs.Internalf(err, "初始化部署步骤失败")
	}
	return nil
}

// markStep 按阶段名把对应步骤置为进行中，之前的置为成功。
func (s *CDService) markStep(ctx context.Context, deployID uint64, stage string, _ uint8) error {
	at := now()
	return s.tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Model(&model.DeployStep{}).
			Where("deploy_id = ? AND name = ?", deployID, stage).
			Updates(map[string]any{"status": "running", "started_at": &at}).Error; err != nil {
			return err
		}
		// 该步骤之前的全部标记成功（阶段是顺序推进的）。
		var cur model.DeployStep
		if err := tx.Where("deploy_id = ? AND name = ?", deployID, stage).First(&cur).Error; err != nil {
			return err
		}
		return tx.Model(&model.DeployStep{}).
			Where("deploy_id = ? AND seq < ? AND status <> ?", deployID, cur.Seq, "success").
			Updates(map[string]any{"status": "success", "finished_at": &at}).Error
	})
}

func (s *CDService) markAllStepsDone(ctx context.Context, deployID uint64) {
	at := now()
	if err := s.db(ctx).Model(&model.DeployStep{}).
		Where("deploy_id = ? AND status <> ?", deployID, "success").
		Updates(map[string]any{"status": "success", "finished_at": &at}).Error; err != nil {
		s.log("cd.deploy").Warn("更新部署步骤失败", "deployId", deployID, "err", err)
	}
}

// markImageDeployed 把环境追加到镜像的 deployedEnvs。
//
// 用"读-改-写"而不是 JSON 函数拼接：跨 MySQL/SQLite 的 JSON 语法差异大，
// 而这条记录是低频写入（每次部署一次），并发冲突风险可忽略。
func (s *CDService) markImageDeployed(ctx context.Context, imageID uint64, envKey string) {
	if imageID == 0 {
		return
	}
	var img model.ImageRecord
	if err := s.db(ctx).First(&img, imageID).Error; err != nil {
		return
	}
	envs := decodeStringList(img.DeployedEnv)
	for _, e := range envs {
		if e == envKey {
			return
		}
	}
	envs = append(envs, envKey)
	raw, err := json.Marshal(envs)
	if err != nil {
		return
	}
	if err := s.db(ctx).Model(&model.ImageRecord{}).Where("id = ?", imageID).
		Update("deployed_envs", model.JSONText(raw)).Error; err != nil {
		s.log("cd.deploy").Warn("更新镜像部署环境失败", "imageId", imageID, "err", err)
	}
}

// finishRollbackOf 回滚单收尾。回滚复用部署状态机，所以状态要从部署单同步过来。
func (s *CDService) finishRollbackOf(ctx context.Context, deployID uint64, status string, duration uint32) {
	var rb model.RollbackRecord
	if err := s.db(ctx).Where("deploy_id = ?", deployID).First(&rb).Error; err != nil {
		return // 不是回滚产生的部署单，正常
	}
	if err := s.db(ctx).Model(&model.RollbackRecord{}).Where("id = ?", rb.ID).
		Updates(map[string]any{"status": status, "duration_sec": duration, "updated_at": now()}).Error; err != nil {
		s.log("cd.deploy").Warn("更新回滚记录失败", "rollbackId", rb.ID, "err", err)
	}
}

// appendDeployLog 把一行部署日志推进日志流。
//
// 只写流、**不落库**：部署日志在设计上就没有持久化表（docs/02 的 22 张表里
// 只有 build_log，没有 deploy_log）。理由是部署日志量远小于构建日志、
// 排障主要看当次，且部署期间应尽量避免数据库写入。
//
// 由此带来一个必须知情的约束：流缓冲区是易失的（进程内实现只留最近若干行，
// Redis 实现带 TTL），**平台一重启，历史部署的日志就取不回来了**。
// 详情接口只会返回空日志，而不是报错——这是刻意的，不是 bug。
func (s *CDService) appendDeployLog(ctx context.Context, streamKey, level, line string) {
	_, _ = s.deps.Stream.Append(ctx, streamKey, level, line, now())
}

// deployLogTail 取最近若干行日志（详情页内嵌）。
func (s *CDService) deployLogTail(ctx context.Context, deployID uint64, limit int) []string {
	rows, err := s.deployLogRows(ctx, deployID, 0, limit)
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if line, ok := r["line"].(string); ok {
			out = append(out, line)
		}
	}
	return out
}

// deployLogRows 从日志流快照读取部署日志。
//
// 只有流快照这一路来源——部署日志没有落库表（详见 appendDeployLog 的说明）。
// 流已被清理或进程重启后这里会返回空数组，**这是预期行为，不要当异常处理**；
// 前端拿到的就是"该部署暂无日志"，而不是 500。
func (s *CDService) deployLogRows(ctx context.Context, deployID uint64, afterSeq uint32, limit int) ([]map[string]any, error) {
	events := s.deps.Stream.Snapshot(ctx, DeployLogStreamKey(deployID))
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		if ev.Kind != logstream.KindLog || ev.Seq <= afterSeq {
			continue
		}
		out = append(out, map[string]any{
			"seq":   ev.Seq,
			"level": ev.Level,
			"line":  ev.Line,
			"ts":    formatTime(ev.At),
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// servicePods 取 Pod 列表。
func (s *CDService) servicePods(ctx context.Context, svc *model.ServiceInstance) ([]ServicePodDTO, error) {
	env, err := s.envByID(ctx, svc.EnvID)
	if err != nil {
		return nil, err
	}
	pods, err := s.deps.Up.Cluster.Pods(ctx, workloadRefOf(env, svc.WorkloadName))
	if err != nil {
		return nil, err
	}
	out := make([]ServicePodDTO, 0, len(pods))
	for _, p := range pods {
		out = append(out, ServicePodDTO{
			Name: p.Name, Status: p.Status, Node: p.Node, Restarts: p.Restarts,
			Age: p.Age, IP: p.IP, CPU: p.CPU, Mem: p.Mem,
		})
	}
	return out, nil
}

// serviceVersions 由部署记录推导历史版本列表。
//
// 不从镜像表推导：镜像表里可能有"构建了但从未部署"的版本，
// 把它们列进"历史版本"会让用户以为曾经上线过。
func (s *CDService) serviceVersions(ctx context.Context, svc *model.ServiceInstance) ([]DeployVersionDTO, error) {
	var rows []model.DeployRecord
	if err := s.db(ctx).Where("service_id = ? AND status IN ?", svc.ID,
		[]string{"success", "rolled_back", "failed"}).
		Order("created_at desc").Limit(30).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询部署历史失败")
	}
	out := make([]DeployVersionDTO, 0, len(rows))
	for i := range rows {
		status := "success"
		if rows[i].Status == "failed" {
			status = "failed"
		}
		out = append(out, DeployVersionDTO{
			Tag:        tagOfImage(rows[i].Image),
			Image:      rows[i].Image,
			DeployedAt: formatTimePtr(rows[i].StartedAt),
			Operator:   rows[i].OperatorName,
			Status:     status,
			Note:       rows[i].Remark,
		})
	}
	return out, nil
}

// decorateDeploys 批量补齐部署记录的环境名/项目名/步骤。
func (s *CDService) decorateDeploys(ctx context.Context, rows []model.DeployRecord, _ bool) []DeployDTO {
	out := make([]DeployDTO, 0, len(rows))
	if len(rows) == 0 {
		return out
	}
	envKeys := make([]string, 0, len(rows))
	projKeys := make([]string, 0, len(rows))
	ids := make([]uint64, 0, len(rows))
	for i := range rows {
		envKeys = append(envKeys, rows[i].EnvKey)
		projKeys = append(projKeys, rows[i].ProjectKey)
		ids = append(ids, rows[i].ID)
	}
	envNames, err := s.envNames(ctx, envKeys)
	if err != nil {
		s.log("cd").Warn("补齐环境名失败", "err", err)
	}
	projNames, err := s.projectNames(ctx, projKeys)
	if err != nil {
		s.log("cd").Warn("补齐项目名失败", "err", err)
	}
	steps, err := s.stepsOfDeploys(ctx, ids)
	if err != nil {
		s.log("cd").Warn("查询部署步骤失败", "err", err)
	}
	for i := range rows {
		out = append(out, deployDTO(&rows[i], envNames[rows[i].EnvKey], projNames[rows[i].ProjectKey],
			steps[rows[i].ID], nil))
	}
	return out
}

func (s *CDService) stepsOfDeploys(ctx context.Context, ids []uint64) (map[uint64][]DeployStepDTO, error) {
	out := map[uint64][]DeployStepDTO{}
	uniq := dedupeUint64(ids)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []model.DeployStep
	if err := s.db(ctx).Where("deploy_id IN ?", uniq).Order("deploy_id, seq").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询部署步骤失败")
	}
	for i := range rows {
		out[rows[i].DeployID] = append(out[rows[i].DeployID], DeployStepDTO{
			Name:   rows[i].Name,
			Status: rows[i].Status,
			At:     formatTimePtr(rows[i].StartedAt),
			Detail: rows[i].Detail,
		})
	}
	return out, nil
}

func (s *CDService) envNames(ctx context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	uniq := dedupeStrings(keys)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []model.Environment
	if err := s.db(ctx).Select("env_key", "name").Where("env_key IN ?", uniq).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询环境失败")
	}
	for i := range rows {
		out[rows[i].EnvKey] = rows[i].Name
	}
	return out, nil
}

func (s *CDService) projectNames(ctx context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	uniq := dedupeStrings(keys)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []model.Project
	if err := s.db(ctx).Select("project_key", "name").Where("project_key IN ?", uniq).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询项目失败")
	}
	for i := range rows {
		out[rows[i].ProjectKey] = rows[i].Name
	}
	return out, nil
}

func (s *CDService) projectsByKeys(ctx context.Context, keys []string) (map[string]*model.Project, error) {
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

func (s *CDService) userNames(ctx context.Context, ids []uint64) (map[uint64]string, error) {
	out := map[uint64]string{}
	uniq := dedupeUint64(ids)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []model.User
	if err := s.db(ctx).Select("id", "name").Where("id IN ?", uniq).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询用户失败")
	}
	for i := range rows {
		out[rows[i].ID] = rows[i].Name
	}
	return out, nil
}

// usernames 与 userNames 同构，取的是**登录名**。
// 编辑表单的负责人 / 审批人选择器的 value 是用户名，必须回这一份才能预填。
func (s *CDService) usernames(ctx context.Context, ids []uint64) (map[uint64]string, error) {
	out := map[uint64]string{}
	uniq := dedupeUint64(ids)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []model.User
	if err := s.db(ctx).Select("id", "username").Where("id IN ?", uniq).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询用户失败")
	}
	for i := range rows {
		out[rows[i].ID] = rows[i].Username
	}
	return out, nil
}

func (s *CDService) projectsOfEnvs(ctx context.Context, envIDs []uint64) (map[uint64][]string, error) {
	out := map[uint64][]string{}
	uniq := dedupeUint64(envIDs)
	if len(uniq) == 0 {
		return out, nil
	}
	var links []model.EnvironmentProject
	if err := s.db(ctx).Where("env_id IN ?", uniq).Find(&links).Error; err != nil {
		return nil, errs.Internalf(err, "查询环境关联项目失败")
	}
	projIDs := make([]uint64, 0, len(links))
	for i := range links {
		projIDs = append(projIDs, links[i].ProjectID)
	}
	var projs []model.Project
	if len(projIDs) > 0 {
		if err := s.db(ctx).Select("id", "project_key").Where("id IN ?", dedupeUint64(projIDs)).Find(&projs).Error; err != nil {
			return nil, errs.Internalf(err, "查询项目失败")
		}
	}
	keyByID := make(map[uint64]string, len(projs))
	for i := range projs {
		keyByID[projs[i].ID] = projs[i].ProjectKey
	}
	for _, l := range links {
		if k, ok := keyByID[l.ProjectID]; ok {
			out[l.EnvID] = append(out[l.EnvID], k)
		}
	}
	return out, nil
}

func (s *CDService) varsOfEnvs(ctx context.Context, envIDs []uint64) (map[uint64][]EnvVarDTO, error) {
	out := map[uint64][]EnvVarDTO{}
	uniq := dedupeUint64(envIDs)
	if len(uniq) == 0 {
		return out, nil
	}
	var rows []model.EnvironmentVar
	if err := s.db(ctx).Where("env_id IN ?", uniq).Order("var_key").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询环境变量失败")
	}
	for i := range rows {
		v := rows[i].VarValue
		if rows[i].IsSecret {
			v = maskValue(v)
		}
		out[rows[i].EnvID] = append(out[rows[i].EnvID], EnvVarDTO{
			Key: rows[i].VarKey, Value: v, Secret: rows[i].IsSecret,
		})
	}
	return out, nil
}

// countIDs 数一下 JSONText 里存了几个 ID。
//
// 解析失败按 0 计：与 `envApprovers` / `approverIDs` 的读入口径一致
// （那边解析失败也是当空处理），不制造"写入时算 1 个、读出时算 0 个"的分歧。
func countIDs(raw model.JSONText) int {
	if raw.Empty() {
		return 0
	}
	var ids []uint64
	if err := json.Unmarshal(raw, &ids); err != nil {
		return 0
	}
	return len(ids)
}

// envApprovers 环境 → 审批人（**两套标识**：展示名 + 用户名），下标一一对应。
//
// 为什么必须同时回两套：`Approvers` 是给界面显示的（`env.approvers.join('、')`），
// 而编辑表单提交的是用户名（后端 `resolveUsers` 按 name/username 查 user 表）。
// 历史实现只回展示名 → 表单无法预填 → `openEdit` 只能留空让用户重选，
// 否则"没重选"就等于把审批人清空（前端注释里承认过这个坑）。
type envApprovers struct {
	Names     []string
	Usernames []string
}

func (s *CDService) envApprovers(ctx context.Context, envs []model.Environment) (map[uint64]envApprovers, error) {
	out := map[uint64]envApprovers{}
	allIDs := []uint64{}
	idsByEnv := map[uint64][]uint64{}
	for i := range envs {
		var ids []uint64
		if !envs[i].ApproverIDs.Empty() {
			if err := json.Unmarshal(envs[i].ApproverIDs, &ids); err != nil {
				ids = nil
			}
		}
		idsByEnv[envs[i].ID] = ids
		allIDs = append(allIDs, ids...)
	}
	names, err := s.userNames(ctx, allIDs)
	if err != nil {
		return nil, err
	}
	usernames, err := s.usernames(ctx, allIDs)
	if err != nil {
		return nil, err
	}
	for envID, ids := range idsByEnv {
		var ref envApprovers
		for _, id := range ids {
			// 名字与用户名要么都有、要么都没有（同一次查询同一批 ID）；
			// 真缺一个也不各补一半 —— 那会让两个切片下标错位。
			n, okN := names[id]
			u, okU := usernames[id]
			if !okN || !okU {
				continue
			}
			ref.Names = append(ref.Names, n)
			ref.Usernames = append(ref.Usernames, u)
		}
		out[envID] = ref
	}
	return out, nil
}

// resolveUsers 把姓名列表解析成用户 ID 列表。
func (s *CDService) resolveUsers(ctx context.Context, names []string) ([]uint64, error) {
	out := []uint64{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		var u model.User
		if err := s.db(ctx).Where("name = ? OR username = ?", n, n).First(&u).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errs.Invalidf("用户 %q 不存在", n)
			}
			return nil, errs.Internalf(err, "查询用户失败")
		}
		out = append(out, u.ID)
	}
	return out, nil
}

func (s *CDService) resolveOwner(ctx context.Context, name string, c Caller) (uint64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if c.User == nil {
			return 0, errs.Invalid("负责人不能为空")
		}
		return c.User.ID, nil
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

func (s *CDService) linkEnvProjects(ctx context.Context, envID uint64, keys []string) error {
	ids, err := s.projectIDsByKeys(ctx, keys)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	links := make([]model.EnvironmentProject, 0, len(ids))
	for _, pid := range ids {
		links = append(links, model.EnvironmentProject{EnvID: envID, ProjectID: pid})
	}
	if err := s.db(ctx).Create(&links).Error; err != nil {
		return wrapDB(err, "关联环境项目")
	}
	// 关联的同时建出服务实例：否则用户"关联了项目"却看不到服务，
	// 还得再点一次"纳管"，多一步无意义的操作。
	if err := s.ensureServices(ctx, envID, ids); err != nil {
		return err
	}
	return nil
}

func (s *CDService) replaceEnvProjects(ctx context.Context, envID uint64, keys []string) error {
	ids, err := s.projectIDsByKeys(ctx, keys)
	if err != nil {
		return err
	}
	want := map[uint64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var existing []model.EnvironmentProject
	if err := s.db(ctx).Where("env_id = ?", envID).Find(&existing).Error; err != nil {
		return errs.Internalf(err, "查询环境关联失败")
	}
	have := map[uint64]bool{}
	for i := range existing {
		have[existing[i].ProjectID] = true
	}
	toAdd := []model.EnvironmentProject{}
	for _, id := range ids {
		if !have[id] {
			toAdd = append(toAdd, model.EnvironmentProject{EnvID: envID, ProjectID: id})
		}
	}
	toRemove := []uint64{}
	for i := range existing {
		if !want[existing[i].ProjectID] {
			toRemove = append(toRemove, existing[i].ProjectID)
		}
	}
	if err := s.tx(ctx, func(tx *gorm.DB) error {
		if len(toAdd) > 0 {
			if err := tx.Create(&toAdd).Error; err != nil {
				return err
			}
		}
		if len(toRemove) > 0 {
			// 取消关联时同时删掉服务实例：留着会变成"环境里有个操作不了的服务"。
			if err := tx.Where("env_id = ? AND project_id IN ?", envID, toRemove).Delete(&model.ServiceInstance{}).Error; err != nil {
				return err
			}
			if err := tx.Where("env_id = ? AND project_id IN ?", envID, toRemove).Delete(&model.EnvironmentProject{}).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// 关联建好之后**必须补齐服务实例**，否则会留下一个最难查的状态：
	// `environment_project` 有行（前端 `linked=true`、编辑抽屉里勾得上、界面显示"已纳管"），
	// 但 `service_instances` 没行 —— 而 `POST /deployments` 认的恰恰是后者，
	// 于是用户"纳管成功了"却依然发布不了，报的还是"该环境尚未纳管此项目"。
	//
	// 这不是假设：`linkEnvProjects`（新建环境走它）本来就成对建，只有这条编辑路径漏了，
	// 两边的差异正是种子数据里"关联 8 个、实例另 15 个、互不包含"那个漂移的来源。
	//
	// **传期望的全集（`ids`）而不是本次新增**：`ensureServices` 本身幂等，传全集才能顺手治好
	// 上面那种"关联有行、实例没行"的历史漂移。只传新增的话，用户在这条路上重试第二次
	// （关联行已经在了）就是彻底的空操作 —— 接口回 200、界面说"保存成功"、实例照旧 0 条，
	// 表现就是"点击纳管以后不行，还是不让我发布"。
	return s.ensureServices(ctx, envID, ids)
}

// ensureServices 让某环境的服务实例集合收敛到 projectIDs。
//
// 对已有存活实例是空操作，因此**可以反复调用**。正因为幂等，调用方应当传「期望的全集」
// 而不是「本次新增的那几个」：只传增量的话，一旦出现「关联有行、实例没行」的漂移就永远补不回来
// （用户 2026-09-29 报的 bug 有一半出在这里，详见 replaceEnvProjects 的注释）。
func (s *CDService) ensureServices(ctx context.Context, envID uint64, projectIDs []uint64) error {
	if len(projectIDs) == 0 {
		return nil
	}
	env, err := s.envByID(ctx, envID)
	if err != nil {
		return err
	}
	var projs []model.Project
	if err := s.db(ctx).Where("id IN ?", projectIDs).Find(&projs).Error; err != nil {
		return errs.Internalf(err, "查询项目失败")
	}
	// 存活行：已经有实例，不用动。
	var live []model.ServiceInstance
	if err := s.db(ctx).Where("env_id = ?", envID).Find(&live).Error; err != nil {
		return errs.Internalf(err, "查询服务实例失败")
	}
	have := map[uint64]bool{}
	for i := range live {
		have[live[i].ProjectID] = true
	}

	// **必须连软删行一起捞**：`uk_svc_env_project(env_id, project_id)` 这个唯一索引只含业务键、
	// **不含 `deleted_at`**，所以一条软删行会把 (环境, 项目) 这个二元组永久占住；而取消纳管走的
	// 恰恰是软删（见 replaceEnvProjects 的 toRemove 分支）。于是「取消勾选后再勾回来」这条最普通的
	// 路径会撞 `Error 1062 Duplicate entry '25-17' for key 'uk_svc_env_project'`，整次保存 500。
	//
	// 更坏的是它**只在第一次撞**：第一次 500 时关联行已经提交了，用户再点一次保存，本次新增为空、
	// 这个函数被整个跳过、接口返回 **200** —— 界面显示"保存成功"、候选接口回 `linked=true`、
	// 编辑抽屉里勾得上，但实例永远是 0 条；而 `POST /deployments` 认的就是实例行，于是
	// "纳管成功了却发不出去"，报的还是"该环境尚未纳管此项目"。
	//
	// 与 `reviveEnvironment` / `reviveK8sCluster` / `image_record` 是同一族修法。
	var trashed []model.ServiceInstance
	if err := s.db(ctx).Unscoped().
		Where("env_id = ? AND deleted_at IS NOT NULL", envID).Find(&trashed).Error; err != nil {
		return errs.Internalf(err, "查询已删除的服务实例失败")
	}
	trashID := map[uint64]uint64{}
	for i := range trashed {
		if _, ok := trashID[trashed[i].ProjectID]; !ok {
			trashID[trashed[i].ProjectID] = trashed[i].ID
		}
	}

	fresh := make([]model.ServiceInstance, 0, len(projs))
	reuse := make([]model.ServiceInstance, 0, len(projs))
	// 新实例的副本数**以集群里实际的为准**，而不是拍一个默认值。
	//
	// 为什么非改不可（2026-10-16 用户报的"首次批量发布怎么给我起了 2 个实例"）：
	// 原先这里写死 2，而集群里那个 Deployment 是 CI / 手工部署的、副本数是 1 ——
	// 库里的 2 一直没生效，直到**第一次通过平台发布**时
	// （`CreateDeployment` 里的 `replicas := svc.Replicas`）被写进集群。
	// 用户看到的就是"我明明只有 1 个，一发就成了 2 个"，而那个 2 从纳管那天就存着了。
	replicasOf := s.clusterReplicasOf(ctx, env, projs)
	for i := range projs {
		if have[projs[i].ID] {
			continue
		}
		svc := s.newServiceInstance(ctx, env, projs[i], replicasOf[projs[i].ProjectKey])
		if id, ok := trashID[projs[i].ID]; ok {
			svc.ID = id
			reuse = append(reuse, svc)
			continue
		}
		fresh = append(fresh, svc)
	}
	if len(fresh) > 0 {
		if err := s.db(ctx).Create(&fresh).Error; err != nil {
			return wrapDB(err, "创建服务实例")
		}
	}
	for i := range reuse {
		if err := s.reviveServiceInstance(ctx, &reuse[i]); err != nil {
			return err
		}
	}
	return nil
}

// clusterReplicasOf 读这批项目在集群里**实际**的副本数。
//
// 只用于"纳管时给副本数一个合理初值"：读得到的用它；读不到的（没接集群、凭据无权、
// 工作负载还不存在）**不写进结果**，由调用方退回保守默认值 ——
// 把读不到当成 0 会让纳管出来的服务一发布就被缩成 0 个实例。
func (s *CDService) clusterReplicasOf(ctx context.Context, env *model.Environment, projs []model.Project) map[string]int {
	out := make(map[string]int, len(projs))
	// 环境没接集群时直接返回：那种情况下每个 Describe 都会因同样的原因失败，
	// 逐个试只是白等一轮超时。
	if strings.TrimSpace(env.Namespace) == "" || strings.TrimSpace(env.Cluster) == "" {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	// ⚠️ **总超时**：纳管是用户在前台等着的操作，而"读副本数"只是为了给个更好的初值。
	// 集群不通时每个 Describe 都要等到连接超时，几十个项目叠起来就是分钟级 ——
	// 那是拿一个可选优化去卡住一次保存。超时就退回默认值（replicaInit 兜 1）。
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// 并发上限：纳管可能一次几十个项目，不限并发会把集群 apiserver 打满。
	sem := make(chan struct{}, 6)
	for i := range projs {
		wg.Add(1)
		go func(workload string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			st, err := s.deps.Up.Cluster.Describe(ctx, workloadRefOf(env, workload))
			if err != nil || st == nil || !st.Exists || st.Replicas <= 0 {
				return
			}
			mu.Lock()
			out[workload] = st.Replicas
			mu.Unlock()
		}(projs[i].ProjectKey)
	}
	wg.Wait()
	return out
}

// replicaInit 纳管时的初始副本数：**读得到集群就用集群的**，读不到退回 1。
//
// 两个"不用什么"都是刻意的：
//   - 不用 0 —— 那是停服，不是"还没定"；
//   - 不用 2 —— 凭空多占一份资源，而且与用户"我就一个实例"的预期相反。
//     这个 2 正是 2026-10-16 那次投诉的来源（见 clusterReplicasOf 的注释）。
func replicaInit(fromCluster int) int {
	if fromCluster > 0 {
		return fromCluster
	}
	return 1
}

// newServiceInstance 按「新建」的默认值造一行服务实例；项目已有可用镜像时直接置成运行态。
//
// `replicas` 由调用方从**集群实况**取（见 clusterReplicasOf），取不到时传 0。
func (s *CDService) newServiceInstance(ctx context.Context, env *model.Environment, p model.Project, replicas int) model.ServiceInstance {
	svc := model.ServiceInstance{
		EnvID:        env.ID,
		ProjectID:    p.ID,
		EnvKey:       env.EnvKey,
		ProjectKey:   p.ProjectKey,
		WorkloadKind: "Deployment",
		WorkloadName: p.ProjectKey,
		Replicas:     replicaInit(replicas),
		Status:       "pending",
		CPURequest:   "200m", CPULimit: "1000m",
		MemRequest: "256Mi", MemLimit: "1Gi",
		EnvVars:    model.JSONText(`[]`),
		ConfigMaps: model.JSONText(`[]`),
		Secrets:    model.JSONText(`[]`),
		Entrypoint: "/app/entrypoint.sh",
		LivenessOK: true, ReadinessOK: true,
	}
	if img := s.latestImageOf(ctx, p.ProjectKey); img != nil {
		svc.CurrentImgID = &img.ID
		svc.CurrentImage = img.Image
		svc.CurrentTag = img.Tag
		svc.Status = "running"
		svc.ReadyReplicas = svc.Replicas
	}
	return svc
}

// reviveServiceInstance 复活一条软删的服务实例，并把各字段**重置成与新建完全一致**。
//
// 为什么重置而不是保留旧值：复活出来的行必须与重新建出来的行无法区分。若保留旧的副本数、镜像、
// 环境变量，那「取消纳管再勾回来」就会得到一个「看着刚建好、实际带着上次配置」的实例 —— 又是一条
// 只有踩过才知道的分支，而这类不对称正是本轮 bug 的来源。代价是重新纳管后要重配环境变量，
// 这与「新建」的行为一致，不意外。
func (s *CDService) reviveServiceInstance(ctx context.Context, svc *model.ServiceInstance) error {
	updates := map[string]any{
		"deleted_at":       nil,
		"env_key":          svc.EnvKey,
		"project_key":      svc.ProjectKey,
		"workload_kind":    svc.WorkloadKind,
		"workload_name":    svc.WorkloadName,
		"current_image_id": svc.CurrentImgID,
		"current_image":    svc.CurrentImage,
		"current_tag":      svc.CurrentTag,
		"replicas":         svc.Replicas,
		"ready_replicas":   svc.ReadyReplicas,
		"status":           svc.Status,
		"cpu_request":      svc.CPURequest,
		"cpu_limit":        svc.CPULimit,
		"mem_request":      svc.MemRequest,
		"mem_limit":        svc.MemLimit,
		"env_vars":         svc.EnvVars,
		"config_maps":      svc.ConfigMaps,
		"secrets":          svc.Secrets,
		"entrypoint":       svc.Entrypoint,
		"hpa_enabled":      svc.HPAEnabled,
		"liveness_ok":      svc.LivenessOK,
		"readiness_ok":     svc.ReadinessOK,
		"applied_revision": nil,
		"last_deploy_id":   nil,
		"updated_at":       now(),
	}
	if err := s.db(ctx).Unscoped().Model(&model.ServiceInstance{}).
		Where("id = ?", svc.ID).Updates(updates).Error; err != nil {
		return wrapDB(err, "恢复服务实例")
	}
	return nil
}

func (s *CDService) latestImageOf(ctx context.Context, projectKey string) *model.ImageRecord {
	var img model.ImageRecord
	if err := s.db(ctx).Where("project_key = ?", projectKey).Order("pushed_at desc").First(&img).Error; err != nil {
		return nil
	}
	return &img
}

func (s *CDService) projectIDsByKeys(ctx context.Context, keys []string) ([]uint64, error) {
	uniq := dedupeStrings(keys)
	if len(uniq) == 0 {
		return nil, nil
	}
	var rows []model.Project
	if err := s.db(ctx).Select("id", "project_key").Where("project_key IN ?", uniq).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询项目失败")
	}
	if len(rows) != len(uniq) {
		found := map[string]bool{}
		for i := range rows {
			found[rows[i].ProjectKey] = true
		}
		missing := []string{}
		for _, k := range uniq {
			if !found[k] {
				missing = append(missing, k)
			}
		}
		return nil, errs.Invalidf("以下项目不存在：%s", strings.Join(missing, "、"))
	}
	out := make([]uint64, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ID)
	}
	return out, nil
}

// syncServiceReplicas 从集群同步副本数（尽力而为，失败只记日志）。
func (s *CDService) syncServiceReplicas(ctx context.Context, env *model.Environment) {
	var svcs []model.ServiceInstance
	if err := s.db(ctx).Where("env_id = ?", env.ID).Find(&svcs).Error; err != nil {
		return
	}
	for i := range svcs {
		state, err := s.deps.Up.Cluster.Describe(ctx, workloadRefOf(env, svcs[i].WorkloadName))
		if err != nil {
			continue
		}
		// 集群里没有这个工作负载时不再"跳过"：把副本数写成 0 才能让界面如实显示
		// "集群中不存在"，否则库里那个纳管时写死的 2/2 会一直赖在那里。
		if !state.Exists {
			if svcs[i].Replicas != 0 || svcs[i].ReadyReplicas != 0 {
				if err := s.db(ctx).Model(&model.ServiceInstance{}).Where("id = ?", svcs[i].ID).Updates(map[string]any{
					"replicas": 0, "ready_replicas": 0,
				}).Error; err != nil {
					s.log("cd").Warn("清零副本数失败", "serviceId", svcs[i].ID, "err", err)
				}
			}
			continue
		}
		if state.Replicas == svcs[i].Replicas && state.ReadyReplicas == svcs[i].ReadyReplicas {
			continue
		}
		if err := s.db(ctx).Model(&model.ServiceInstance{}).Where("id = ?", svcs[i].ID).Updates(map[string]any{
			"replicas": state.Replicas, "ready_replicas": state.ReadyReplicas,
		}).Error; err != nil {
			s.log("cd").Warn("同步副本数失败", "serviceId", svcs[i].ID, "err", err)
		}
	}
}

// ensureApprover 校验当前用户是否为该环境的审批人。
func (s *CDService) ensureApprover(ctx context.Context, envID uint64, c Caller) error {
	// 持有通配权限（admin）或 deploy:approve 的运维可以直接审批。
	if c.User.HasAnyPerm("*", "deploy:approve") {
		return nil
	}
	var env model.Environment
	if err := s.db(ctx).First(&env, envID).Error; err != nil {
		return errs.Internalf(err, "查询环境失败")
	}
	var ids []uint64
	if !env.ApproverIDs.Empty() {
		if err := json.Unmarshal(env.ApproverIDs, &ids); err != nil {
			ids = nil
		}
	}
	for _, id := range ids {
		if id == c.User.ID {
			return nil
		}
	}
	return errs.Forbidden("你不是该环境的审批人")
}

func (s *CDService) envIDsApprovedBy(ctx context.Context, userID uint64) ([]uint64, error) {
	var envs []model.Environment
	if err := s.db(ctx).Select("id", "approver_ids").Where("need_approval = ?", true).Find(&envs).Error; err != nil {
		return nil, errs.Internalf(err, "查询环境审批人失败")
	}
	out := []uint64{}
	for i := range envs {
		var ids []uint64
		if envs[i].ApproverIDs.Empty() {
			continue
		}
		if err := json.Unmarshal(envs[i].ApproverIDs, &ids); err != nil {
			continue
		}
		for _, id := range ids {
			if id == userID {
				out = append(out, envs[i].ID)
				break
			}
		}
	}
	return out, nil
}

// deployLockTTL 部署锁有效期。默认 30 分钟，略大于部署超时。
func (s *CDService) deployLockTTL() time.Duration {
	if t := s.deps.Cfg.Redis.DeployLockTTL.D(); t > 0 {
		return t
	}
	return 30 * time.Minute
}

func (s *CDService) defaultRegistry() string {
	if r := strings.TrimSpace(s.deps.Cfg.Integration.Harbor.Registry); r != "" {
		return r
	}
	return "harbor.yunnian.internal"
}

func (s *CDService) latestImage(ctx context.Context, projectKey string) *model.ImageRecord {
	return s.latestImageOf(ctx, projectKey)
}

// --------------------------------------------------------------------------- 小工具

// serviceEnvMap 把服务实例的环境变量转成注入用的 map。
func serviceEnvMap(raw model.JSONText) map[string]string {
	out := map[string]string{}
	if raw.Empty() {
		return out
	}
	var vars []EnvVarDTO
	if err := json.Unmarshal(raw, &vars); err != nil {
		return out
	}
	for _, v := range vars {
		out[v.Key] = v.Value
	}
	return out
}

// mergeSecretEnvVars 合并服务环境变量：密文项留空/掩码时沿用旧值。
func mergeSecretEnvVars(in []EnvVarDTO, old map[string]string) []EnvVarDTO {
	out := make([]EnvVarDTO, 0, len(in))
	for _, v := range in {
		if v.Secret && (v.Value == "" || v.Value == maskValue("x")) {
			if prev, ok := old[v.Key]; ok {
				v.Value = prev
			}
		}
		out = append(out, v)
	}
	return out
}

func mustJSON(v any) model.JSONText {
	raw, err := json.Marshal(v)
	if err != nil {
		return model.JSONText(`[]`)
	}
	return model.JSONText(raw)
}

// diffOf 生成参数变更的字段级差异，供参数历史页展示。
func diffOf(svc *model.ServiceInstance, cpuReq, cpuLim, memReq, memLim string, replicas int, hpa bool) model.JSONText {
	items := []DiffItem{}
	add := func(field, from, to string) {
		if from != to {
			items = append(items, DiffItem{Field: field, From: from, To: to})
		}
	}
	add("replicas", fmt.Sprint(svc.Replicas), fmt.Sprint(replicas))
	add("cpuRequest", svc.CPURequest, cpuReq)
	add("cpuLimit", svc.CPULimit, cpuLim)
	add("memRequest", svc.MemRequest, memReq)
	add("memLimit", svc.MemLimit, memLim)
	add("hpaEnabled", fmt.Sprint(svc.HPAEnabled), fmt.Sprint(hpa))
	raw, err := json.Marshal(items)
	if err != nil {
		return model.JSONText(`[]`)
	}
	return model.JSONText(raw)
}

// validateResources 校验资源配置格式与 request/limit 的相对关系。
//
// 为什么要校验 request ≤ limit：K8s 会直接拒绝这种 Pod 定义，
// 但它给的是"Invalid value"这种没头没尾的报错，用户很难对上。
// 在这里拦住并说清哪个字段错了，比让用户去猜集群的报错有用得多。
func validateResources(cpuReq, cpuLim, memReq, memLim string) error {
	if cpuReq != "" && cpuLim != "" {
		r, err1 := parseQuantityMilli(cpuReq)
		l, err2 := parseQuantityMilli(cpuLim)
		if err1 == nil && err2 == nil && r > l {
			return errs.Invalidf("CPU request（%s）不能大于 limit（%s）", cpuReq, cpuLim)
		}
	}
	if memReq != "" && memLim != "" {
		r, err1 := parseQuantityBytes(memReq)
		l, err2 := parseQuantityBytes(memLim)
		if err1 == nil && err2 == nil && r > l {
			return errs.Invalidf("内存 request（%s）不能大于 limit（%s）", memReq, memLim)
		}
	}
	return nil
}

// parseQuantityMilli 解析 CPU 数量为"毫核"。
// 支持 500m / 0.5 / 2 三种写法（K8s 的合法写法）。
func parseQuantityMilli(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, errors.New("空值")
	}
	if strings.HasSuffix(v, "m") {
		n, err := parseInt64(v[:len(v)-1])
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	f, err := parseFloat64(v)
	if err != nil {
		return 0, err
	}
	return int64(f * 1000), nil
}

// parseQuantityBytes 解析内存数量为字节。支持 Ki/Mi/Gi 与纯数字。
func parseQuantityBytes(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, errors.New("空值")
	}
	mult := int64(1)
	for _, suf := range []struct {
		S string
		M int64
	}{
		{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
		{"G", 1000000000}, {"M", 1000000}, {"K", 1000},
	} {
		if strings.HasSuffix(v, suf.S) {
			mult = suf.M
			v = strings.TrimSuffix(v, suf.S)
			break
		}
	}
	n, err := parseFloat64(v)
	if err != nil {
		return 0, err
	}
	return int64(n * float64(mult)), nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

func parseFloat64(s string) (float64, error) {
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f); err != nil {
		return 0, err
	}
	return f, nil
}

func parseUint32(s string) (uint32, error) {
	var n uint32
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

func tagOfImage(image string) string {
	if i := strings.LastIndex(image, ":"); i > 0 && !strings.Contains(image[i+1:], "/") {
		return image[i+1:]
	}
	return image
}

func derefUint64(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

func remarkSuffix(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "（备注：" + s + "）"
}

func statusWordDeploy(s string) string {
	switch s {
	case "pending_approval":
		return "待审批"
	case "queued":
		return "排队中"
	case "progressing":
		return "部署中"
	case "success":
		return "成功"
	case "failed":
		return "失败"
	case "rolled_back":
		return "已回滚"
	case "canceled":
		return "已取消"
	}
	return s
}

// diagnoseDeploy 部署失败的归因。
func diagnoseDeploy(code string) *AIDiagnosisDTO {
	switch code {
	case "TIMEOUT":
		return &AIDiagnosisDTO{
			Reason:     "部署超时：滚动更新在平台的等待上限内没有收敛，通常是新 Pod 起不来（镜像拉取慢、探针不过、资源不足）。",
			Suggestion: "先在集群里看新 ReplicaSet 的 Pod 事件；确认探针阈值与启动耗时是否匹配，必要时放宽 initialDelaySeconds 后重试。",
			Confidence: 0.75,
		}
	case "ROLLOUT_STUCK":
		return &AIDiagnosisDTO{
			Reason: "滚动更新卡死：集群在进度截止时间内始终没有看到任何进展，新 Pod 一直没有就绪。" +
				"与「部署超时」的区别是这次不是慢，而是确定起不来。",
			Suggestion: "看本次新起的那个 Pod（不是还在服务流量的旧 Pod）的容器状态与崩溃前的日志：" +
				"CrashLoopBackOff 去补齐缺失的配置项，OOMKilled 去调内存上限，ImagePullBackOff 去修镜像地址或拉取凭据。",
			Confidence: 0.7,
		}
	case "DEPLOY_LOCKED":
		return &AIDiagnosisDTO{
			Reason:     "同一服务已有部署在进行中，平台按互斥规则拒绝了本次执行，线上版本未发生变化。",
			Suggestion: "等待进行中的部署结束，或先在部署记录里把那一单取消，然后重试本次部署。",
			Confidence: 0.95,
		}
	case "CANCELED":
		return &AIDiagnosisDTO{
			Reason:     "部署被取消。若取消发生在「更新工作负载」之后，集群里可能已经切了一部分流量。",
			Suggestion: "请确认当前线上版本是否符合预期；如已变化，用上一版镜像执行一次回滚收敛。",
			Confidence: 0.8,
		}
	case "APPLY_FAILED":
		return &AIDiagnosisDTO{
			Reason:     "工作负载更新被集群拒绝，常见于镜像地址不可达、命名空间资源配额不足或配置项缺失。",
			Suggestion: "检查镜像是否存在且当前集群可拉取；确认目标命名空间的 ResourceQuota 与 LimitRange 是否拦截了该规格。",
			Confidence: 0.7,
		}
	case "QUEUE_REJECTED":
		return &AIDiagnosisDTO{
			Reason:     "部署任务未能进入执行队列：并发槽位已满或队列积压。",
			Suggestion: "稍后重试；若持续出现，检查是否有部署卡住未释放并发槽位。",
			Confidence: 0.8,
		}
	default:
		return nil
	}
}

// newLineScanner 已由 bufio.Scanner 直接承担（见 StreamPodLogs）。

func serviceUpdaterIDs(rows []model.ServiceInstance) []uint64 {
	out := make([]uint64, 0, len(rows))
	for i := range rows {
		out = append(out, derefUint64(rows[i].UpdatedBy))
	}
	return out
}

func envOwnerIDs(rows []model.Environment) []uint64 {
	out := make([]uint64, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].OwnerID)
	}
	return out
}

func paramChangerIDs(rows []model.DeployParam) []uint64 {
	out := make([]uint64, 0, len(rows))
	for i := range rows {
		out = append(out, derefUint64(rows[i].ChangedBy))
	}
	return out
}

func envKeysOfRollbacks(rows []model.RollbackRecord) []string {
	out := make([]string, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].EnvKey)
	}
	return out
}

func projectKeysOfRollbacks(rows []model.RollbackRecord) []string {
	out := make([]string, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ProjectKey)
	}
	return out
}
