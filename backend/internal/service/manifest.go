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

// ManifestImportInput 批量导入入参。
//
// 和单条创建的差别：这里**接受多文档**（`---` 分隔）和 `kind: List`，
// 由服务端拆开逐条校验 —— 用户手上就是一份 `kubectl apply -f` 用的文件，
// 让他先手工切成一条条再逐个粘，正是这次要消掉的麻烦。
type ManifestImportInput struct {
	EnvKey string `json:"envKey" binding:"required,max=64"`
	// Content 文件原文，可以含多个 `---` 文档、也可以是 kind: List。
	Content string `json:"content" binding:"required"`
	// OnConflict 同 kind+name 已存在时怎么办：
	//   ""/"skip"  跳过（默认）—— 导入是"把文件灌进来"，不该悄悄盖掉已有的编辑
	//   "overwrite" 用文件里的内容覆盖
	OnConflict string `json:"onConflict" binding:"max=16"`
	Note       string `json:"note" binding:"max=255"`
}

// ManifestImportItem 导入结果里的一项（逐条报告，不接受"成功 5 个"这种笼统结论）。
type ManifestImportItem struct {
	// Index 在文件里是第几个资源（从 1 开始），解析失败时靠它定位。
	Index int    `json:"index"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	// Action created / updated / skipped / failed
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// ManifestImportResult 导入结果汇总 + 明细。
type ManifestImportResult struct {
	Total   int                  `json:"total"`
	Created int                  `json:"created"`
	Updated int                  `json:"updated"`
	Skipped int                  `json:"skipped"`
	Failed  int                  `json:"failed"`
	Items   []ManifestImportItem `json:"items"`
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

// maxManifestImportDocs 一次导入最多几个资源。
//
// 不是性能限制（几百条也就几十毫秒），是**防误传**：
// 有人把整个集群的 `kubectl get all -o yaml` 拖进来时会生成上千条记录，
// 之后再想清理就得一条条删。200 条足够覆盖一个中型项目。
const maxManifestImportDocs = 200

// splitManifests 把一份文件拆成若干个"单资源 YAML 文本"。
//
// ⚠️ 这里**按 `---` 做文本切分，不解码再编码**。
// 用 yaml.Decoder 解码后 Marshal 回去，注释、字段顺序、引号风格、
// 缩进全都会变 —— 用户下次打开看到的就不是他写的那份东西了。
// 平台存的是"原文"，这一点必须守住。
//
// 唯一的例外是 `kind: List`：一份 List 要变成 N 份单资源，
// 没有原文可保留（原文里它们是一个数组），只能重新编码。
func splitManifests(content string) []string {
	isSep := func(s string) bool {
		if s == "---" {
			return true
		}
		// `--- # 注释` 也是合法分隔符
		if strings.HasPrefix(s, "---") {
			rest := strings.TrimSpace(s[3:])
			return rest == "" || strings.HasPrefix(rest, "#")
		}
		return false
	}

	// hasContent 判断一段里有没有"真的东西"。
	//
	// 只含空行和 `#` 注释的段要丢掉：用户手上那份文件常常在开头写一段说明、
	// 或者在两个资源之间留一句注释分隔，那些都不是资源。
	// 不丢的话它们会各自变成一条记录，然后逐条报"内容为空"——
	// 用户看到的是 N 条莫名其妙的失败，而实际原因只是文件里有注释。
	hasContent := func(s string) bool {
		for _, line := range strings.Split(s, "\n") {
			t := strings.TrimSpace(line)
			if t != "" && !strings.HasPrefix(t, "#") {
				return true
			}
		}
		return false
	}

	var docs []string
	var buf []string
	flush := func() {
		t := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = buf[:0]
		if hasContent(t) {
			docs = append(docs, t+"\n")
		}
	}
	for _, line := range strings.Split(content, "\n") {
		if isSep(strings.TrimSpace(line)) {
			flush()
			continue
		}
		buf = append(buf, line)
	}
	flush()

	// 展开 kind: List（`kubectl get -o yaml` 的导出形式）。
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		var probe map[string]any
		if err := yaml.Unmarshal([]byte(d), &probe); err == nil {
			if k, ok := probe["kind"].(string); ok && strings.EqualFold(strings.TrimSpace(k), "List") {
				items, _ := probe["items"].([]any)
				for _, it := range items {
					b, err := yaml.Marshal(it)
					if err != nil {
						continue
					}
					out = append(out, string(b))
				}
				continue
			}
		}
		out = append(out, d)
	}
	return out
}

// ImportManifests 从一份文件批量导入资源定义。
//
// 逐条独立处理：一条失败不影响其他条。这是刻意的 ——
// 用户拿来的是一份 `kubectl apply -f` 用的文件，里面常见几个已经存在、
// 几个是新的、个别写错。全有或全无的话，他得先把文件改到完美才能导进来，
// 那还不如一条条手工粘。
func (s *CIService) ImportManifests(ctx context.Context, projectKey string, in ManifestImportInput, c Caller) (*ManifestImportResult, error) {
	p, err := s.projectByKey(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	envKey, err := s.requireEnvKey(ctx, in.EnvKey)
	if err != nil {
		return nil, err
	}

	docs := splitManifests(in.Content)
	if len(docs) == 0 {
		return nil, errs.Invalid("文件里没有可导入的资源（只有注释或空文档？）")
	}
	if len(docs) > maxManifestImportDocs {
		return nil, errs.Invalidf("一次最多导入 %d 个资源，这份文件有 %d 个。"+
			"请拆成几份再导，或确认是不是把整个集群的导出拖进来了", maxManifestImportDocs, len(docs))
	}

	overwrite := strings.EqualFold(strings.TrimSpace(in.OnConflict), "overwrite")
	uid := c.UserID()
	res := &ManifestImportResult{Total: len(docs), Items: make([]ManifestImportItem, 0, len(docs))}
	note := strings.TrimSpace(in.Note)

	for i, doc := range docs {
		item := ManifestImportItem{Index: i + 1}
		pm, perr := parseManifest(doc)
		if perr != nil {
			item.Action = "failed"
			item.Reason = userMessage(perr)
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		item.Kind, item.Name = pm.Kind, pm.Name

		// 唯一键不含 deleted_at，所以查询必须 Unscoped —— 软删的行仍占着这个位置。
		var existing model.ProjectManifest
		qerr := s.db(ctx).Unscoped().
			Where("project_id = ? AND env_key = ? AND kind = ? AND name = ?", p.ID, envKey, pm.Kind, pm.Name).
			First(&existing).Error

		switch {
		case qerr == nil && !existing.DeletedAt.Valid && !overwrite:
			// 已存在且用户没要求覆盖 → 跳过，并说清"为什么跳过、怎么才能覆盖"。
			item.Action = "skipped"
			item.Reason = "平台里已有同名同类型的定义（未覆盖）。要更新请勾选「覆盖已存在的」或单独编辑它"
			res.Skipped++
		case qerr == nil:
			// 已存在：覆盖（含软删行的复活 —— 复活就是"清掉 deleted_at 并写入新内容"）。
			if err := s.db(ctx).Unscoped().Model(&model.ProjectManifest{}).
				Where("id = ?", existing.ID).
				Updates(map[string]any{
					"content":    doc,
					"note":       note,
					"updated_by": uid,
					"deleted_at": nil,
				}).Error; err != nil {
				item.Action = "failed"
				item.Reason = "写入失败：" + err.Error()
				res.Failed++
				break
			}
			if existing.DeletedAt.Valid {
				item.Action = "created"
				item.Reason = "恢复了此前删除的同名定义"
				res.Created++
			} else {
				item.Action = "updated"
				res.Updated++
			}
		case errors.Is(qerr, gorm.ErrRecordNotFound):
			m := &model.ProjectManifest{
				ProjectID: p.ID,
				EnvKey:    envKey,
				Kind:      pm.Kind,
				Name:      pm.Name,
				Content:   doc,
				Note:      note,
				CreatedBy: uid,
				UpdatedBy: uid,
			}
			if err := s.db(ctx).Create(m).Error; err != nil {
				item.Action = "failed"
				item.Reason = "写入失败：" + err.Error()
				res.Failed++
				break
			}
			item.Action = "created"
			res.Created++
		default:
			item.Action = "failed"
			item.Reason = "查询已有定义失败：" + qerr.Error()
			res.Failed++
		}
		res.Items = append(res.Items, item)
	}
	return res, nil
}

// userMessage 从业务错误里取"给人看的那句话"。
//
// `*errs.Error.Error()` 是 `code=NNNNN 人话: 因` 的拼接形式，
// 直接返回会把内部错误码漏到界面上（这条纪律项目里踩过）。
func userMessage(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
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
