package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/model"
)

// --------------------------------------------------------------------------- k8s 资源定义
//
// 这一期只做「存 + 看」：把项目在各环境的 Deployment / Service / ConfigMap …
// 收进平台，随时可查可改。**不碰集群**（apply 是下一期的事）。
//
// 为什么值得先做这一步：现在发布前必须手工把整个项目的资源 apply 到集群，
// 平台对这些资源一无所知 —— 不知道它长什么样、也不知道某个环境到底有没有它。
// 没有这份数据，"部署时发现没资源就引导先配 YAML"这件事无从谈起。

// ProjectManifestDTO 一份资源定义。
type ProjectManifestDTO struct {
	// ID 用字符串：雪花/自增都能安全过 JSON，不会被前端当 number 截断精度。
	ID     string `json:"id"`
	EnvKey string `json:"envKey"`
	// Kind 从 YAML 解析得来，不是用户填的 —— 单一事实来源。
	Kind string `json:"kind"`
	// Name 从 metadata.name 解析得来，同上。
	Name string `json:"name"`
	// APIVersion 原样带回来，界面上显示 `apps/v1 Deployment` 这种完整身份。
	APIVersion string `json:"apiVersion"`
	Content    string `json:"content"`
	Note       string `json:"note"`
	Sort       int    `json:"sort"`
	UpdatedAt  time.Time `json:"updatedAt"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ManifestInput 新建/编辑入参。
//
// **没有 kind / name**：它们由服务端从 Content 解析。让前端传就等于给了
// 一次"标的是 Service、存的是 Deployment"的机会，而这种错要到 apply 时才炸。
type ManifestInput struct {
	EnvKey  string `json:"envKey" binding:"required,max=64"`
	Content string `json:"content" binding:"required"`
	Note    string `json:"note" binding:"max=255"`
	Sort    int    `json:"sort"`
}

// ManifestQuery 列表过滤。
type ManifestQuery struct {
	// EnvKey 为空表示"所有环境"。界面默认按环境筛，但也允许一次看全部。
	EnvKey string
	Kind   string
}

// parsedManifest 解析结果。只包含平台需要知道的三样，正文原样保留。
type parsedManifest struct {
	APIVersion string
	Kind       string
	Name       string
}

// parseManifest 解析并校验一份 YAML，得出它的身份。
//
// 校验原则：**只校验"平台自己必须知道的事"，不做 k8s schema 校验**。
// 平台需要 kind / name 来定位这份资源、需要确保它是单文档，
// 至于 spec 里写得对不对，平台这一期没有能力也没有必要判断 ——
// 越权校验反而会在用户写了一个新版本才有的字段时误拒。
//
// 但有三条必须卡住：
//  1. 能解析（否则存进去的就是一坨坏文本，后期 apply 必然失败，且失败点离原因很远）
//  2. **只有一个文档**（一条记录一个资源；多文档会把"逐条报告成败"这件事做废）
//  3. apiVersion / kind / metadata.name 齐备（缺任何一个都无法定位资源）
func parseManifest(content string) (parsedManifest, error) {
	var out parsedManifest
	if strings.TrimSpace(content) == "" {
		return out, errs.Invalid("YAML 内容不能为空")
	}

	dec := yaml.NewDecoder(strings.NewReader(content))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return out, errs.Invalid("YAML 内容为空（只有注释？）")
		}
		return out, errs.Invalidf("YAML 解析失败：%v", err)
	}

	// 第二个文档能解出来 → 说明用了 --- 分隔。
	// 注意这里不能只看 err == nil：`---` 后面跟空内容时解出的是空 map，也算多段。
	var second map[string]any
	if err := dec.Decode(&second); err == nil {
		return out, errs.Invalid("一次只能保存一个资源：检测到多个 YAML 文档（--- 分隔），" +
			"请拆成多条记录分别保存 —— 后期 apply 时要能逐条报告是哪个资源失败")
	} else if !errors.Is(err, io.EOF) {
		// 第二个文档解析出错：同样按"多文档"处理，因为第一段已经足够构成一份资源。
		return out, errs.Invalidf("发现第二个 YAML 文档但解析失败：%v；请一次只保存一个资源", err)
	}

	out.APIVersion = strings.TrimSpace(stringOf(doc["apiVersion"]))
	out.Kind = strings.TrimSpace(stringOf(doc["kind"]))
	if md, ok := doc["metadata"].(map[string]any); ok {
		out.Name = strings.TrimSpace(stringOf(md["name"]))
	}

	if out.APIVersion == "" {
		return out, errs.Invalid("缺少 apiVersion（例如 apps/v1、v1）")
	}
	if out.Kind == "" {
		return out, errs.Invalid("缺少 kind（例如 Deployment、Service）")
	}

	if strings.EqualFold(out.Kind, "List") {
		// `kubectl get -o yaml` 导出的就是一个 kind: List、items 里装多个资源。
		// 它形式上是一个文档，实质是多资源 —— 与 `---` 分隔同样的问题，
		// 所以按多文档处理，让用户拆开。
		return out, errs.Invalid("这是一份 kind: List（items 里装了多个资源），" +
			"请拆成多条记录分别保存")
	}
	if out.Name == "" {
		return out, errs.Invalid("缺少 metadata.name：平台要靠它定位集群里的这份资源")
	}
	if len(out.Kind) > 48 {
		return out, errs.Invalid("kind 过长（超过 48 字符）")
	}
	if len(out.Name) > 191 {
		return out, errs.Invalid("metadata.name 过长（超过 191 字符）")
	}
	return out, nil
}

// stringOf 把 YAML 解出来的 any 当字符串取。
//
// 只接受真正的字符串：`kind: 123` 这种写法在 YAML 里合法但毫无意义，
// 静默转成 "123" 会让我们存下一个永远 apply 不成功的资源。
func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ListProjectManifests 某项目的资源定义列表（可按环境 / kind 过滤）。
func (s *CIService) ListProjectManifests(ctx context.Context, projectKey string, q ManifestQuery) ([]ProjectManifestDTO, error) {
	p, err := s.projectByKey(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	db := s.db(ctx).Where("project_id = ?", p.ID)
	if envKey := strings.TrimSpace(q.EnvKey); envKey != "" {
		db = db.Where("env_key = ?", envKey)
	}
	if kind := strings.TrimSpace(q.Kind); kind != "" {
		db = db.Where("kind = ?", kind)
	}
	var rows []model.ProjectManifest
	if err := db.Order("env_key, kind, sort, name").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询资源定义失败")
	}
	out := make([]ProjectManifestDTO, 0, len(rows))
	for i := range rows {
		out = append(out, manifestDTO(&rows[i]))
	}
	return out, nil
}

// GetProjectManifest 取一份资源定义（详情用，也供后续 apply 复用）。
func (s *CIService) GetProjectManifest(ctx context.Context, projectKey string, id uint64) (*ProjectManifestDTO, error) {
	m, err := s.manifestOf(ctx, projectKey, id)
	if err != nil {
		return nil, err
	}
	d := manifestDTO(m)
	return &d, nil
}

// CreateProjectManifest 新建一份资源定义。
func (s *CIService) CreateProjectManifest(ctx context.Context, projectKey string, in ManifestInput, c Caller) (*ProjectManifestDTO, error) {
	p, err := s.projectByKey(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	envKey, err := s.requireEnvKey(ctx, in.EnvKey)
	if err != nil {
		return nil, err
	}
	pm, err := parseManifest(in.Content)
	if err != nil {
		return nil, err
	}

	// 唯一键 (project_id, env_key, kind, name) 不含 deleted_at：
	// 软删过的行仍然占着这个位置，所以新建时必须**先查含软删的**。
	// 查到软删的就地复活并覆盖（与 reviveProject 同一套口径），
	// 而不是给用户报一句"已存在"却看不见它在哪。
	var existing model.ProjectManifest
	err = s.db(ctx).Unscoped().
		Where("project_id = ? AND env_key = ? AND kind = ? AND name = ?", p.ID, envKey, pm.Kind, pm.Name).
		First(&existing).Error
	switch {
	case err == nil:
		if !existing.DeletedAt.Valid {
			return nil, errs.StateConflict(fmt.Sprintf(
				"%s %s 在这个环境里已经有一份定义了，请直接编辑它", pm.Kind, pm.Name))
		}
		// 复活软删的行
		uid := c.UserID()
		updates := map[string]any{
			"content":    in.Content,
			"note":       strings.TrimSpace(in.Note),
			"sort":       in.Sort,
			"updated_by": uid,
			"deleted_at": nil,
		}
		if err := s.db(ctx).Unscoped().Model(&model.ProjectManifest{}).
			Where("id = ?", existing.ID).Updates(updates).Error; err != nil {
			return nil, errs.Internalf(err, "保存资源定义失败")
		}
		got, err := s.manifestOf(ctx, projectKey, existing.ID)
		if err != nil {
			return nil, err
		}
		d := manifestDTO(got)
		return &d, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 正常新建
	default:
		return nil, errs.Internalf(err, "查询资源定义失败")
	}

	uid := c.UserID()
	m := &model.ProjectManifest{
		ProjectID: p.ID,
		EnvKey:    envKey,
		Kind:      pm.Kind,
		Name:      pm.Name,
		Content:   in.Content,
		Note:      strings.TrimSpace(in.Note),
		Sort:      in.Sort,
		CreatedBy: uid,
		UpdatedBy: uid,
	}
	if err := s.db(ctx).Create(m).Error; err != nil {
		return nil, errs.Internalf(err, "保存资源定义失败")
	}
	d := manifestDTO(m)
	return &d, nil
}

// UpdateProjectManifest 编辑一份资源定义。
//
// ⚠️ **Kind / Name 可能被改**（用户把 Deployment 的 name 改了）。所以更新后要
// 重新检查唯一键 —— 否则会把两条记录写成同一个身份，DB 唯一索引一报错，
// 用户只会看到"保存失败"。
func (s *CIService) UpdateProjectManifest(ctx context.Context, projectKey string, id uint64, in ManifestInput, c Caller) (*ProjectManifestDTO, error) {
	m, err := s.manifestOf(ctx, projectKey, id)
	if err != nil {
		return nil, err
	}
	envKey, err := s.requireEnvKey(ctx, in.EnvKey)
	if err != nil {
		return nil, err
	}
	pm, err := parseManifest(in.Content)
	if err != nil {
		return nil, err
	}

	var clash int64
	if err := s.db(ctx).Model(&model.ProjectManifest{}).
		Where("project_id = ? AND env_key = ? AND kind = ? AND name = ? AND id <> ?",
			m.ProjectID, envKey, pm.Kind, pm.Name, m.ID).
		Count(&clash).Error; err != nil {
		return nil, errs.Internalf(err, "查询资源定义失败")
	}
	if clash > 0 {
		return nil, errs.StateConflict(fmt.Sprintf(
			"%s %s 在这个环境里已经有一份定义了", pm.Kind, pm.Name))
	}

	uid := c.UserID()
	if err := s.db(ctx).Model(&model.ProjectManifest{}).Where("id = ?", m.ID).
		Updates(map[string]any{
			"env_key":    envKey,
			"kind":       pm.Kind,
			"name":       pm.Name,
			"content":    in.Content,
			"note":       strings.TrimSpace(in.Note),
			"sort":       in.Sort,
			"updated_by": uid,
		}).Error; err != nil {
		return nil, errs.Internalf(err, "保存资源定义失败")
	}
	got, err := s.manifestOf(ctx, projectKey, id)
	if err != nil {
		return nil, err
	}
	d := manifestDTO(got)
	return &d, nil
}

// DeleteProjectManifest 删除一份资源定义（软删）。
//
// 只删平台里的记录：**集群里的资源不会动**（这一期平台本来就没有写集群的能力）。
// 这一点必须让用户知道，否则删完发现"服务还在跑"会觉得没生效。
func (s *CIService) DeleteProjectManifest(ctx context.Context, projectKey string, id uint64, c Caller) error {
	m, err := s.manifestOf(ctx, projectKey, id)
	if err != nil {
		return err
	}
	uid := c.UserID()
	if err := s.db(ctx).Model(&model.ProjectManifest{}).Where("id = ?", m.ID).
		Updates(map[string]any{"updated_by": uid}).Error; err != nil {
		return errs.Internalf(err, "删除资源定义失败")
	}
	if err := s.db(ctx).Delete(&model.ProjectManifest{}, m.ID).Error; err != nil {
		return errs.Internalf(err, "删除资源定义失败")
	}
	return nil
}

// --------------------------------------------------------------------------- 内部助手

// manifestOf 按项目 key + 记录 id 取一条，并校验归属。
//
// 必须校验 project_id：否则改一个 id 就能操作别的项目的资源定义。
func (s *CIService) manifestOf(ctx context.Context, projectKey string, id uint64) (*model.ProjectManifest, error) {
	p, err := s.projectByKey(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	var m model.ProjectManifest
	if err := s.db(ctx).Where("id = ? AND project_id = ?", id, p.ID).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("资源定义不存在（可能已被删除）")
		}
		return nil, errs.Internalf(err, "查询资源定义失败")
	}
	return &m, nil
}

// requireEnvKey 校验环境存在，并归一化 env_key。
//
// **不要求项目已纳管到该环境**：这条路径的前提场景恰恰是"还没资源、也还没纳管"，
// 要求纳管会把用户挡在门外。但环境本身必须存在 —— 否则存进去的东西永远无法使用。
func (s *CIService) requireEnvKey(ctx context.Context, raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", errs.Invalid("请选择环境：同一份资源在不同环境通常不一样")
	}
	var n int64
	if err := s.db(ctx).Model(&model.Environment{}).Where("env_key = ?", key).Count(&n).Error; err != nil {
		return "", errs.Internalf(err, "查询环境失败")
	}
	if n == 0 {
		return "", errs.NotFoundf("环境 %s 不存在", key)
	}
	return key, nil
}

func manifestDTO(m *model.ProjectManifest) ProjectManifestDTO {
	// 身份从内容里现解析一遍：库里没有单独存 apiVersion，
	// 而它在界面上要显示（`apps/v1 Deployment` 才是完整身份）。
	// 解析失败时留空，由前端降级显示 kind。
	pm, _ := parseManifest(m.Content)
	return ProjectManifestDTO{
		ID:         strconv.FormatUint(m.ID, 10),
		EnvKey:     m.EnvKey,
		Kind:       m.Kind,
		Name:       m.Name,
		APIVersion: pm.APIVersion,
		Content:    m.Content,
		Note:       m.Note,
		Sort:       m.Sort,
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
	}
}
