package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/model"
	"github.com/2777484478/yunnian/internal/store"
)

// 从镜像仓库读 tag（部署向导的「仓库 tag」那一组）。
//
// 与「平台构建」那一组的分工：
//
//	image_record   平台自己构建过的东西，带 commit / 构建号 / 推送时间 —— 能回答"这是谁在哪一次构建里产出的"
//	仓库 tag       仓库里**实际存在**的 tag，包括别的平台推的 —— 能回答"仓库里现在有什么可以发"
//
// 两者**都要保留、二选一**（用户 2026-10-14 明确要求）：不是替换，是并列。
// 所以这里只做"把仓库里有什么读出来"，不碰 image_record。

// defaultRegistryTagLimit 默认只回最近几个 tag。
//
// 定 5 是用户的要求（2026-10-14）：仓库里 60+ 个 tag 对"发一次版"这个动作没有意义，
// 反倒要在一堆历史版本里找。确实要找更早的版本时，走「镜像版本」页看全量。
const defaultRegistryTagLimit = 5

// maxRegistryTagLimit 一次最多回多少（防止有人拿它当"列表接口"用）。
const maxRegistryTagLimit = 50

// registryTagTimeout 读一次 tag 的超时。比单条 manifest 查询宽一点：
// v1 的 tags?detail=1 是**一次把全部 tag 连 manifest 一起返回**，大仓库会明显更慢。
const registryTagTimeout = 12 * time.Second

// RegistryTagDTO 仓库里的一个 tag。
type RegistryTagDTO struct {
	Tag string `json:"tag"`
	// Time 与 TimeKind 必须一起看：**界面按 kind 措辞**，别一律写成"推送时间"。
	Time string `json:"time"`
	// TimeKind 见 integration.HarborTimePush / HarborTimeBuild，空串 = 这个 tag 没有时间。
	TimeKind string `json:"timeKind"`
}

// RegistryTagsDTO 一次「仓库 tag」查询的结果。
//
// ⚠️ 读不到时**不用空列表表达**，而是填 Unavailable 说明原因 ——
// 空数组在界面上会被读成"这个仓库没有 tag"，那是个会误导人的结论
// （真实的可能原因是：没接仓库、项目在仓库里找不到对应仓库、账号没权限、仓库不通）。
type RegistryTagsDTO struct {
	ProjectKey string `json:"projectKey"`
	// Repo 是仓库全名（escloud/aioai）；ImageBase 是拼好的镜像前缀（host:port/escloud/aioai）。
	Repo      string `json:"repo"`
	ImageBase string `json:"imageBase"`
	// Source 说明仓库是**怎么定位到的**：image_record（平台推过，最可信）/ search（仓库检索）。
	Source string `json:"source"`
	// APIVersion Harbor Core 接口版本（v1 / v2）—— 决定有没有推送时间。
	APIVersion string `json:"apiVersion"`
	// TimeKind 这一批 tag 的时间含义（push / build / ""）。
	TimeKind string `json:"timeKind"`
	// Total 仓库里一共有多少个 tag；Tags 只是其中最近的 Limit 个。
	Total int `json:"total"`
	Limit int `json:"limit"`
	// Note 给用户看的一句说明（截断了 / v1 没有推送时间 / 仓库是检索出来的）。
	Note string `json:"note"`
	// Unavailable 非空表示这次读不到，值是可读的原因。
	Unavailable string           `json:"unavailable"`
	Tags        []RegistryTagDTO `json:"tags"`
}

// registryClient 取**当前生效**的镜像仓库客户端。
//
// ⚠️ 不能直接用 `s.deps.Up.Registry` 了事：那是**进程启动时**按 config.yaml 装配好的
// （见 integration.NewSetWithSources），而 Harbor 的地址/凭据从 2026-10-13 起可以在
// 「第三方集成」页改并落库 —— 装配层那份**不会跟着变**，现象是"界面显示新地址，
// 实际还在问旧仓库"，而且不报任何错。
//
// 所以启用真实实现时按运行期配置新建一个（只是拼个 http.Client，很便宜）；
// 未启用时保持装配层那个 —— 那是 Local 实现，语义是"什么都不认"，
// 换成真实的会把"没接仓库"变成"接上了但连不上"。
func (b *base) registryClient(ctx context.Context) (integration.Registry, config.HarborConfig, error) {
	cfg, err := b.harborCfg(ctx)
	if err != nil {
		return nil, config.HarborConfig{}, err
	}
	if !cfg.Enabled {
		return b.deps.Up.Registry, cfg, nil
	}
	return integration.NewHarborRegistry(cfg), cfg, nil
}

// harborCfg 读**当前生效**的 Harbor 配置（库优先、逐键回退 config.yaml）。
//
// 收敛成一处：状态卡、连通性测试、tag 列表都用它。之前"界面改了配置、某个地方还在读
// config.yaml"的漂移已经在集成状态卡上出现过一次（见 system.go 的注释），不再重复。
//
// 没有库可读时直接用 config.yaml 那份（单测装配的 service 就没有 DB）——
// 真实进程里 DB 一定在，这条分支只是让"读不到配置"不至于变成 panic。
func (b *base) harborCfg(ctx context.Context) (config.HarborConfig, error) {
	fallback := b.deps.Cfg.Integration.Harbor
	if b.deps.DB == nil {
		return fallback, nil
	}
	cur, err := store.LoadHarborConfig(ctx, b.deps.DB, b.deps.Sealer, fallback)
	if err != nil {
		return config.HarborConfig{}, err
	}
	return cur.Cfg, nil
}

// ListRegistryTags 的其余部分见本文件下方。

// registryTagHost 取出"镜像地址里的仓库部分"（如 es-repo.example.com:5000）。
//
// 配置里那一项可能没填，此时从 registry 地址推 —— 两者指同一个仓库，
// 拼镜像引用时少了它就只能报错，而这不是用户"没配"的意思。
func registryTagHost(cfg config.HarborConfig) string {
	if h := strings.Trim(strings.TrimSpace(cfg.Registry), "/"); h != "" {
		return h
	}
	reg, _, _ := splitImageRef(strings.TrimSpace(cfg.BaseURL))
	return reg
}

// ListRegistryTags 读某项目在镜像仓库里最近的若干 tag。
//
// limit<=0 取默认 5；上限 50。**total 是仓库里的全量条数**，不是这一页的条数。
func (s *CDService) ListRegistryTags(ctx context.Context, projectKey string, limit int) (*RegistryTagsDTO, error) {
	projectKey = strings.TrimSpace(projectKey)
	if projectKey == "" {
		return nil, errs.Invalid("缺少项目标识：无法定位要读哪个仓库")
	}
	if limit <= 0 {
		limit = defaultRegistryTagLimit
	}
	if limit > maxRegistryTagLimit {
		limit = maxRegistryTagLimit
	}
	proj, err := s.projectByKey(ctx, projectKey)
	if err != nil {
		return nil, err
	}

	out := &RegistryTagsDTO{ProjectKey: projectKey, Limit: limit, Tags: []RegistryTagDTO{}}

	cfg, err := s.harborCfg(ctx)
	if err != nil {
		return nil, errs.Internalf(err, "读取 Harbor 配置失败")
	}
	if !cfg.Enabled {
		out.Unavailable = "Harbor 接入未启用（系统管理 › 第三方集成）"
		return out, nil
	}
	core := integration.NewHarborCore(cfg)
	if core == nil {
		out.Unavailable = "还没配 Harbor 地址（系统管理 › 第三方集成）"
		return out, nil
	}

	host := registryTagHost(cfg)
	repo, source, reason, err := s.harborRepoForProject(ctx, proj, core, host)
	if err != nil {
		return nil, err
	}
	if repo == "" {
		out.Unavailable = reason
		return out, nil
	}
	out.Repo = repo
	out.Source = source
	out.ImageBase = strings.Trim(host+"/"+repo, "/")

	projName, repoName, err := integration.SplitImageRepo(repo)
	if err != nil {
		out.Unavailable = fmt.Sprintf("仓库路径 %q 解析不出「项目/仓库」两段", repo)
		return out, nil
	}

	// 剥掉调用方 ctx 的取消信号、只留短超时：读 tag 不该被上一层的超时牵连，
	// 也不该把接口拖长（与 preflightImage 同一口径）。
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), registryTagTimeout)
	defer cancel()
	tags, total, err := core.ListTags(rctx, projName, repoName, 1, limit)
	if err != nil {
		// 读不到就**明说真因**，不给空列表。
		out.Unavailable = err.Error()
		return out, nil
	}

	out.APIVersion = core.APIVersion()
	out.Total = total
	for _, t := range tags {
		item := RegistryTagDTO{Tag: t.Name, TimeKind: t.TimeSource}
		if !t.Time.IsZero() {
			item.Time = t.Time.UTC().Format(time.RFC3339)
		}
		if out.TimeKind == "" {
			out.TimeKind = t.TimeSource
		}
		out.Tags = append(out.Tags, item)
	}
	if len(out.Tags) > 0 && out.TimeKind == "" {
		// 一条时间都没拿到。**不编**：把 TimeKind 留空，界面显示"—"。
		out.TimeKind = ""
	}
	out.Note = registryTagNote(out, source, core.APIVersion())
	return out, nil
}

// registryTagNote 拼一句"这批 tag 到底是什么"，把三件容易误解的事讲清楚：
// 时间是什么时间、列的是不是全部、仓库是怎么找到的。
func registryTagNote(out *RegistryTagsDTO, source, apiVersion string) string {
	parts := []string{}
	switch out.TimeKind {
	case integration.HarborTimePush:
		parts = append(parts, "按推送时间倒序")
	case integration.HarborTimeBuild:
		parts = append(parts, "按镜像构建时间倒序（Harbor v1 不提供推送时间，构建时间≠推送顺序）")
	default:
		parts = append(parts, "按标签名倒序（这台 Harbor 给不出时间）")
	}
	if out.Total > len(out.Tags) {
		parts = append(parts, fmt.Sprintf("仓库里共 %d 个标签，这里只列最近 %d 个", out.Total, len(out.Tags)))
	}
	if source == "search" {
		parts = append(parts, "该项目在平台里还没有构建记录，仓库是按项目标识检索出来的")
	}
	return strings.Join(parts, "；")
}

// harborRepoForProject 找出一个项目对应仓库里的哪条「项目/仓库」路径。
//
// 两条来源，按可信度排序：
//
//	① image_record：那是**平台自己推过**的地址，带 Registry+Repo，最可信；
//	   但只认 **Registry 与当前配置同一个仓库**的那些 —— 换了 Harbor 之后，
//	   旧记录里的路径在新仓库里可能指向别人的同名仓库，读出来就是别人的版本。
//	② 仓库检索 /search?q=<项目标识>：取末段**恰好等于**项目标识的仓库。
//	   恰好一个才用；0 个或多个都返回可读原因，**不猜** ——
//	   猜错会去读别的项目的仓库，而那种错不会报任何错，只是显示成别人的版本。
//
// wantHost 是当前配置的仓库地址（host[:port]），用于上面那道过滤。
func (s *CDService) harborRepoForProject(ctx context.Context, proj *model.Project, core *integration.HarborCore, wantHost string) (repo, source, reason string, err error) {
	// ① 平台构建过的镜像记录里有现成的仓库路径
	want := integration.NormalizeRegistryHost(wantHost)
	var recs []model.ImageRecord
	if e := s.db(ctx).Where("project_key = ? AND repo <> ''", proj.ProjectKey).
		Order("pushed_at desc").Limit(20).Find(&recs).Error; e != nil {
		return "", "", "", errs.Internalf(e, "查询镜像记录失败")
	}
	for i := range recs {
		if want != "" && integration.NormalizeRegistryHost(recs[i].Registry) != want {
			continue
		}
		return strings.Trim(strings.TrimSpace(recs[i].Repo), "/"), "image_record", "", nil
	}

	// ② 退回仓库检索
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), registryTagTimeout)
	defer cancel()
	refs, serr := core.SearchRepos(rctx, proj.ProjectKey)
	if serr != nil {
		return "", "", fmt.Sprintf("该项目在平台里没有构建记录，去仓库检索也失败了：%v", serr), nil
	}
	matches := make([]string, 0, 2)
	for _, r := range refs {
		_, last, splitErr := integration.SplitImageRepo(r.FullName)
		if splitErr != nil {
			continue
		}
		if strings.EqualFold(last, proj.ProjectKey) {
			matches = append(matches, r.FullName)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], "search", "", nil
	case 0:
		return "", "", fmt.Sprintf("该项目在平台里没有构建记录，仓库里也找不到名字以 %q 结尾的仓库："+
			"请先在 CI 里构建一次，或在「第三方集成」里确认仓库地址", proj.ProjectKey), nil
	default:
		return "", "", fmt.Sprintf("仓库里有 %d 个名字以 %q 结尾的仓库（%s），无法确定该用哪一个："+
			"请先在 CI 里构建一次，平台会以那次推送的地址为准",
			len(matches), proj.ProjectKey, strings.Join(matches, "、")), nil
	}
}
