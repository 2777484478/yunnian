package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/model"
)

// --------------------------------------------------------------------------- 定时任务
//
// 定位：**到点替用户"点一下按钮"**，不自己执行构建 / 部署。
//
// 这条边界是整个设计的地基。如果定时器自己去跑构建，它就成了第二套 CI，
// 日志、状态机、中断补偿、失败通知、并发控制全要再实现一遍，
// 而且两条路的行为迟早分叉（比如一边记得写 image_record、另一边忘了）。
// 现在它只做一件事：算出时间、调已有的 `TriggerBuild` / `CreateDeployment`。
// 于是排队、串行、通知、超时、中断补偿全部自动复用，且与手动触发完全同构。

const (
	// scheduleTick 扫描间隔。30 秒足够：cron 的最小粒度是**分钟**，
	// 而延迟 30 秒对"每天 2 点构建"没有任何影响。
	scheduleTick = 30 * time.Second

	// scheduleGrace 宽限窗口：`next_run_at` 落后超过这个值就判定为"错过"。
	//
	// 为什么需要它：tick 本身有 30 秒误差，正常触发时 lag 一般 < 30s。
	// 而服务停机 3 小时后重启，lag 会是几小时 —— 那种情况**不能补跑**
	// （用户明确要求：不补跑，只记错过次数）。
	// 尤其部署：半夜 2 点该发的，早上 10 点补发到生产，是事故不是功能。
	scheduleGrace = 2 * time.Minute

	// maxMissedScan 统计"错过几次"时最多往回数这么多次，
	// 防止有人配了 `*/1 * * * *` 又停了半年，把 CPU 耗在数数上。
	maxMissedScan = 500

	// scheduleExecTimeout 单次执行的超时。
	//
	// 这里等的是"逐条建单"，不等构建 / 部署跑完（建单即入队就返回）。
	// 单条最坏情况要解析仓库标签（一次 Harbor 查询），批量 20~30 个目标时
	// 串起来可能到一分钟以上 —— 2 分钟会不够，5 分钟留足余量，
	// 又不至于在数据库卡死时把 goroutine 拖太久。
	scheduleExecTimeout = 5 * time.Minute

	// maxScheduleTargets 一条任务最多带多少个目标。
	//
	// 上限是"防手滑"用的，不是容量极限：一次 tick 里循环建单，
	// 每条建单只是一次 INSERT + 入队。50 已经远超"一次铺满一个环境"的量，
	// 真给到几千条，用户更该分批配置，而不是指望一条任务跑一整晚。
	maxScheduleTargets = 50
)

// scheduleCronParser 标准 5 字段（分 时 日 月 周）。
//
// 用成熟库而不是自己写：cron 的边界语义（月份天数、周几与日期的"或"关系、
// 闰年）坑很多，而它只影响一件事 —— "下次什么时候跑"。
// 这个算错了，定时任务会静默地在错误的时间发版。
var scheduleCronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// ScheduleService 定时任务。
//
// 需要同时持有 CI / CD 两个服务：它自己不做构建和部署，只负责"调用它们"。
// 这也是为什么它不嵌在 CIService 里 —— 那样 CIService 就得反向依赖 CDService。
type ScheduleService struct {
	*base
	ci *CIService
	cd *CDService

	// tickMu 保证两次扫描不重叠。
	// 执行本身是异步的（goroutine），这里只保护"扫描 + 判定 + 推进 next_run_at"。
	tickMu sync.Mutex
}

// --------------------------------------------------------------------------- 入参 / 出参

type ScheduleDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	TaskType string `json:"taskType"`
	// ProjectKey / Branch / ImageTag / ImageRecordID 是 Targets 里**第一个**的镜像，
	// 只为兼容旧界面与列表展示；判断"这条任务都干些什么"请看 Targets。
	ProjectKey string `json:"projectKey"`
	EnvKey     string `json:"envKey"`
	Cron       string `json:"cron"`
	Enabled    bool   `json:"enabled"`

	// Targets 目标清单（至少 1 条）。旧数据无此字段时由镜像列还原。
	Targets []model.ScheduleTarget `json:"targets"`

	Branch      string            `json:"branch"`
	BuildParams map[string]string `json:"buildParams,omitempty"`

	ImageSource   string `json:"imageSource"`
	ImageTag      string `json:"imageTag"`
	ImageRecordID string `json:"imageRecordId"`
	Replicas      *int   `json:"replicas"`

	Remark string `json:"remark"`

	NextRunAt    *time.Time `json:"nextRunAt"`
	LastRunAt    *time.Time `json:"lastRunAt"`
	LastStatus   string     `json:"lastStatus"`
	LastError    string     `json:"lastError"`
	MissedCount  int        `json:"missedCount"`
	LastBuildID  string     `json:"lastBuildId"`
	LastDeployID string     `json:"lastDeployId"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ScheduleInput 新建 / 编辑入参。
type ScheduleInput struct {
	Name     string `json:"name" binding:"required,max=64"`
	TaskType string `json:"taskType" binding:"required,oneof=build deploy"`
	// Targets 多目标（批量构建 / 批量部署）。
	//
	// 兼容旧的单目标写法：只给 ProjectKey/Branch/ImageTag 时，normalize 会把它
	// 折叠成一条目标。两条路最终汇成同一个 targets 切片，不会出现"两套执行逻辑"。
	Targets []model.ScheduleTarget `json:"targets"`

	ProjectKey string `json:"projectKey" binding:"max=64"`
	EnvKey     string `json:"envKey" binding:"max=64"`
	Cron       string `json:"cron" binding:"required,max=64"`
	// Enabled 指针：不传 = 新建默认启用，编辑时不动。
	Enabled *bool `json:"enabled"`

	Branch      string            `json:"branch" binding:"max=96"`
	BuildParams map[string]string `json:"buildParams"`

	// ImageSource fixed / latest，见 model.ScheduleTask 的说明。
	ImageSource   string `json:"imageSource" binding:"omitempty,oneof=fixed latest"`
	ImageTag      string `json:"imageTag" binding:"max=191"`
	ImageRecordID string `json:"imageRecordId" binding:"max=32"`
	Replicas      *int   `json:"replicas"`

	Remark string `json:"remark" binding:"max=255"`
}

// SchedulePreview 校验表达式并预览接下来几次执行时间（建任务时给用户看）。
type SchedulePreview struct {
	Valid bool        `json:"valid"`
	Error string      `json:"error"`
	Next  []time.Time `json:"next"`
}

// --------------------------------------------------------------------------- 查询

// ListSchedules 定时任务列表（可按类型 / 项目筛）。
func (s *ScheduleService) ListSchedules(ctx context.Context, taskType, projectKey string) ([]ScheduleDTO, error) {
	db := s.db(ctx).Model(&model.ScheduleTask{})
	if t := strings.TrimSpace(taskType); t != "" {
		db = db.Where("task_type = ?", t)
	}
	if p := strings.TrimSpace(projectKey); p != "" {
		// ⚠️ 不能只查 project_key：那是**第一个**目标的镜像列，
		// 按第 2 个目标去筛会漏掉这条任务（"我明明配了它" 却搜不到）。
		// targets 是 json 列，这里用 LIKE 匹配序列化后的键值对；
		// 项目标识只含 [a-z0-9-]，不含需要转义的字符。
		db = db.Where("project_key = ? OR targets LIKE ?", p, `%"projectKey":"`+p+`"%`)
	}
	var rows []model.ScheduleTask
	if err := db.Order("enabled DESC, next_run_at, id").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询定时任务失败")
	}
	out := make([]ScheduleDTO, 0, len(rows))
	for i := range rows {
		out = append(out, scheduleDTO(&rows[i]))
	}
	return out, nil
}

// GetSchedule 单条。
func (s *ScheduleService) GetSchedule(ctx context.Context, id uint64) (*ScheduleDTO, error) {
	t, err := s.scheduleOf(ctx, id)
	if err != nil {
		return nil, err
	}
	d := scheduleDTO(t)
	return &d, nil
}

// --------------------------------------------------------------------------- 增删改

// CreateSchedule 新建。
func (s *ScheduleService) CreateSchedule(ctx context.Context, in ScheduleInput, c Caller) (*ScheduleDTO, error) {
	norm, err := s.normalize(ctx, in, c)
	if err != nil {
		return nil, err
	}
	uid := c.UserID()
	first := norm.Targets[0]
	t := &model.ScheduleTask{
		Name:     norm.Name,
		TaskType: norm.TaskType,
		Targets:  encodeScheduleTargets(norm.Targets),
		// 镜像列取第一个目标，只为"按项目筛任务"与旧界面读得到
		ProjectKey:    first.ProjectKey,
		Branch:        first.Branch,
		ImageTag:      first.ImageTag,
		ImageRecordID: first.ImageRecordID,
		EnvKey:        norm.EnvKey,
		Cron:          norm.Cron,
		// 新建默认启用（用户点了"创建"就是要它跑起来）
		Enabled:     true,
		BuildParams: encodeScheduleParams(norm.BuildParams),
		ImageSource: norm.ImageSource,
		Replicas:    norm.Replicas,
		Remark:      norm.Remark,
		CreatedBy:   uid,
		UpdatedBy:   uid,
	}
	if norm.Enabled != nil {
		t.Enabled = *norm.Enabled
	}
	if t.Enabled {
		t.NextRunAt = nextRunAfter(t.Cron, now())
	}
	if err := s.db(ctx).Create(t).Error; err != nil {
		return nil, errs.Internalf(err, "创建定时任务失败")
	}
	d := scheduleDTO(t)
	return &d, nil
}

// UpdateSchedule 编辑。
//
// ⚠️ 改了 cron 或启停状态都必须**重算 next_run_at**：
// 不重算的话，任务从"每天 2 点"改成"每天 5 点"，界面会显示 5 点，
// 但调度器仍按旧的 2 点触发 —— 这种"显示与行为不一致"极难排查。
func (s *ScheduleService) UpdateSchedule(ctx context.Context, id uint64, in ScheduleInput, c Caller) (*ScheduleDTO, error) {
	old, err := s.scheduleOf(ctx, id)
	if err != nil {
		return nil, err
	}
	norm, err := s.normalize(ctx, in, c)
	if err != nil {
		return nil, err
	}
	uid := c.UserID()
	first := norm.Targets[0]
	updates := map[string]any{
		"name":            norm.Name,
		"task_type":       norm.TaskType,
		"targets":         encodeScheduleTargets(norm.Targets),
		"project_key":     first.ProjectKey,
		"branch":          first.Branch,
		"image_tag":       first.ImageTag,
		"image_record_id": first.ImageRecordID,
		"env_key":         norm.EnvKey,
		"cron":            norm.Cron,
		"build_params":    encodeScheduleParams(norm.BuildParams),
		"image_source":    norm.ImageSource,
		"replicas":        norm.Replicas,
		"remark":          norm.Remark,
		"updated_by":      uid,
	}
	enabled := norm.Enabled == nil || *norm.Enabled
	updates["enabled"] = enabled
	if enabled {
		// 表达式没变、且原来就是启用的 → 保留原 next_run_at（否则每改一次备注
		// 就把"下次执行"往后推，用户会以为任务没生效）。
		if old.Enabled && old.Cron == norm.Cron && old.NextRunAt != nil && old.NextRunAt.After(now()) {
			updates["next_run_at"] = old.NextRunAt
		} else {
			updates["next_run_at"] = nextRunAfter(norm.Cron, now())
		}
	} else {
		updates["next_run_at"] = nil
	}
	if err := s.db(ctx).Model(&model.ScheduleTask{}).Where("id = ?", old.ID).Updates(updates).Error; err != nil {
		return nil, errs.Internalf(err, "保存定时任务失败")
	}
	return s.GetSchedule(ctx, id)
}

// DeleteSchedule 删除（软删）。
func (s *ScheduleService) DeleteSchedule(ctx context.Context, id uint64, c Caller) error {
	t, err := s.scheduleOf(ctx, id)
	if err != nil {
		return err
	}
	uid := c.UserID()
	if err := s.db(ctx).Model(&model.ScheduleTask{}).Where("id = ?", t.ID).
		Updates(map[string]any{"updated_by": uid, "enabled": false, "next_run_at": nil}).Error; err != nil {
		return errs.Internalf(err, "删除定时任务失败")
	}
	if err := s.db(ctx).Delete(&model.ScheduleTask{}, t.ID).Error; err != nil {
		return errs.Internalf(err, "删除定时任务失败")
	}
	return nil
}

// SetScheduleEnabled 启 / 停。
//
// 单独一个动作而不是"编辑里勾选"：停用是**运维动作**（正在发版、先停掉定时），
// 不该陪着一起改 cron、版本这些配置项。
func (s *ScheduleService) SetScheduleEnabled(ctx context.Context, id uint64, enabled bool, c Caller) (*ScheduleDTO, error) {
	t, err := s.scheduleOf(ctx, id)
	if err != nil {
		return nil, err
	}
	updates := map[string]any{"enabled": enabled, "updated_by": c.UserID()}
	if enabled {
		updates["next_run_at"] = nextRunAfter(t.Cron, now())
	} else {
		updates["next_run_at"] = nil
	}
	if err := s.db(ctx).Model(&model.ScheduleTask{}).Where("id = ?", t.ID).Updates(updates).Error; err != nil {
		return nil, errs.Internalf(err, "切换定时任务失败")
	}
	return s.GetSchedule(ctx, id)
}

// PreviewSchedule 校验 cron 并返回接下来 5 次执行时间。
//
// 对用户的价值：cron 表达式写错（`0 25 * * *` 的 25 点）能当场看出来，
// 而不是等任务建好、界面上一片空白、才发现"下次执行"是空的。
func (s *ScheduleService) PreviewSchedule(expr string) *SchedulePreview {
	out := &SchedulePreview{Next: []time.Time{}}
	sch, err := scheduleCronParser.Parse(strings.TrimSpace(expr))
	if err != nil {
		out.Error = fmt.Sprintf("表达式不合法：%v。格式是「分 时 日 月 周」，例如 0 2 * * * = 每天 02:00", err)
		return out
	}
	out.Valid = true
	cur := now()
	for i := 0; i < 5; i++ {
		t := sch.Next(cur)
		if t.IsZero() {
			break
		}
		out.Next = append(out.Next, t)
		cur = t
	}
	return out
}

// RunScheduleNow 立即执行一次（调试用，不影响下次计划时间）。
//
// 为什么需要：配一个"每天 2 点"的任务，用户不可能等到凌晨验证它对不对。
// 立即执行只建单、不推进 next_run_at —— 否则"点一下试试"会把当晚的执行吃掉。
func (s *ScheduleService) RunScheduleNow(ctx context.Context, id uint64, c Caller) (*ScheduleDTO, error) {
	t, err := s.scheduleOf(ctx, id)
	if err != nil {
		return nil, err
	}
	if busy, reason := s.stillRunning(ctx, t); busy {
		return nil, errs.StateConflict(reason)
	}
	if err := s.execute(ctx, t, "手动触发"); err != nil {
		return nil, err
	}
	return s.GetSchedule(ctx, id)
}

// --------------------------------------------------------------------------- 调度循环

// RunScheduler 启动调度器。由 main 在起服务后调用一次。
//
// 单实例假设：app 目前只跑一个容器，所以不需要分布式锁。
// 若将来扩副本，必须补一层"抢占"（`UPDATE ... WHERE next_run_at = <旧值>`），
// 否则每个副本都会触发一次 —— 同一时间发出两份构建单。
func (s *ScheduleService) RunScheduler(ctx context.Context) {
	// 启动时先对齐一次：停机期间 next_run_at 可能全是过去时间，
	// 不处理的话启动瞬间会一次性把"错过的"全跑掉（正是用户要求避免的）。
	s.catchUpOnBoot(ctx)

	go func() {
		t := time.NewTicker(scheduleTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.tick(ctx)
			}
		}
	}()
}

// catchUpOnBoot 启动时把"已错过"的任务记数并推进到下一次。
func (s *ScheduleService) catchUpOnBoot(ctx context.Context) {
	var tasks []model.ScheduleTask
	if err := s.db(ctx).Where("enabled = ?", true).Find(&tasks).Error; err != nil {
		s.log("schedule").Warn("启动对齐定时任务失败", "err", err)
		return
	}
	at := now()
	for i := range tasks {
		t := &tasks[i]
		if t.NextRunAt == nil {
			// 历史数据 / 刚被启用但没算过：补算
			s.db(ctx).Model(&model.ScheduleTask{}).Where("id = ?", t.ID).
				Update("next_run_at", nextRunAfter(t.Cron, at))
			continue
		}
		dec := decideDue(t.Cron, *t.NextRunAt, at)
		if dec.Run {
			// 没超窗口：留给 tick 正常处理
			continue
		}
		missed := dec.Missed
		s.log("schedule").Info("定时任务错过执行（不补跑）",
			"task", t.ID, "name", t.Name, "missed", missed, "lag", dec.Lag.String())
		s.db(ctx).Model(&model.ScheduleTask{}).Where("id = ?", t.ID).Updates(map[string]any{
			"next_run_at":  nextRunAfter(t.Cron, at),
			"last_status":  "missed",
			"last_error":   fmt.Sprintf("服务在 %s 时未运行，错过 %d 次（按配置不补跑）", t.NextRunAt.Format("01-02 15:04"), missed),
			"missed_count": gorm.Expr("missed_count + ?", missed),
		})
	}
}

// dueDecision "这一轮到点任务该怎么处理"的判定结果。
//
// 抽成纯函数是为了能脱离数据库测：这段逻辑决定了"跑/不跑/记几次错过"，
// 而它的错误方式都很难在集成测试里发现（要造停机时间、要等 tick）。
type dueDecision struct {
	// Run 本次是否真的执行
	Run bool
	// Missed 本次判定为"错过了几次"（Run=true 时恒为 0）
	Missed int
	// Lag next_run_at 落后当前时间多久
	Lag time.Duration
}

// decideDue 判断一次到点该跑还是该记错过。
//
// 规则（用户 2026-10-29 拍板）：
//   - 落后在宽限窗口内 → 正常执行
//   - 落后超过窗口（服务停机 / 卡顿）→ **不补跑**，只累计错过次数
//
// 为什么部署尤其不能补跑：半夜 2 点该发的，早上 10 点服务恢复后突然补发到生产，
// 是事故不是功能。构建补跑同样没意义 —— 补出来的产物对应的是旧提交。
func decideDue(expr string, nextRunAt, at time.Time) dueDecision {
	lag := at.Sub(nextRunAt)
	if lag <= scheduleGrace {
		return dueDecision{Run: true, Lag: lag}
	}
	missed := countMissed(expr, nextRunAt, at)
	if missed < 1 {
		// 表达式解析不出（理论上创建时就校验过），至少算 1 次，
		// 否则界面会显示"错过了 0 次"，等于没发生。
		missed = 1
	}
	return dueDecision{Run: false, Missed: missed, Lag: lag}
}

// tick 扫描到期任务。
func (s *ScheduleService) tick(ctx context.Context) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()

	at := now()
	var tasks []model.ScheduleTask
	if err := s.db(ctx).Where("enabled = ? AND next_run_at IS NOT NULL AND next_run_at <= ?", true, at).
		Order("next_run_at").Limit(20).Find(&tasks).Error; err != nil {
		s.log("schedule").Warn("扫描定时任务失败", "err", err)
		return
	}
	for i := range tasks {
		t := &tasks[i]
		dueAt := *t.NextRunAt
		dec := decideDue(t.Cron, dueAt, at)

		// 先把下次时间推进到未来 —— 无论这次跑不跑。
		// 不推进的话下一轮 tick 会再次命中同一条，反复建单。
		next := nextRunAfter(t.Cron, at)

		if !dec.Run {
			// 宽限窗口外：停机/卡顿导致的错过。记数，不跑。
			_ = s.db(ctx).Model(&model.ScheduleTask{}).Where("id = ?", t.ID).Updates(map[string]any{
				"next_run_at":  next,
				"last_status":  "missed",
				"last_error":   fmt.Sprintf("错过 %s 的执行（服务当时未运行），共错过 %d 次", dueAt.Format("01-02 15:04"), dec.Missed),
				"missed_count": gorm.Expr("missed_count + ?", dec.Missed),
			})
			continue
		}

		// 正常到点：先写 running 再异步执行。
		// 先写库是为了"上次还在跑就跳过"这个判断在下一轮能生效。
		_ = s.db(ctx).Model(&model.ScheduleTask{}).Where("id = ?", t.ID).Updates(map[string]any{
			"next_run_at": next,
			"last_status": "running",
			"last_error":  "",
		})
		taskID := t.ID
		go func() {
			// 独立 ctx：执行不能挂在调度循环的 ctx 上，
			// 否则进程退出时正在建的单会半途而废。
			ectx, cancel := context.WithTimeout(context.Background(), scheduleExecTimeout)
			defer cancel()
			var row model.ScheduleTask
			if err := s.db(ectx).First(&row, taskID).Error; err != nil {
				return
			}
			if !row.Enabled {
				// 扫描之后、执行之前被人停了 —— 尊重这个动作
				s.finishTask(ectx, taskID, "skipped", "", 0, 0, nil, "已被停用，本次不执行")
				return
			}
			if busy, reason := s.stillRunning(ectx, &row); busy {
				s.finishTask(ectx, taskID, "skipped", "", 0, 0, nil, reason)
				return
			}
			_ = s.execute(ectx, &row, "")
		}()
	}
}

// --------------------------------------------------------------------------- 执行

// execute 逐条建单（批量）。
//
// 编排放在后端而不是前端，与批量发布同一口径（2026-10-16 `ca7c60c` 的教训）：
// 前端逐个调接口的话，关掉页面后面的目标就再也不会被创建。
// 这里是一次性把**全部**目标建成单子，建完就与浏览器无关。
//
// 目标之间**互相独立**：一个项目失败不影响其余继续建（这正好是运维要的 ——
// 8 个包里有 1 个分支写错，不该连累另外 7 个）。最终按三档汇报结果。
func (s *ScheduleService) execute(ctx context.Context, t *model.ScheduleTask, triggerNote string) error {
	// 用 DisplayName 而不是冒充某个用户：
	// 定时任务没有"当前登录人"，冒用创建者的身份会让审计失真。
	// 同时 User 为 nil 意味着跳过部署的权限校验 —— 权限在**创建任务时**校验过。
	caller := Caller{DisplayName: "定时任务：" + t.Name}

	targets := scheduleTargetsOf(t)
	if len(targets) == 0 {
		// 理论上 normalize 已经拦住；这里兜底是为了不让一条坏任务把调度器搞崩。
		msg := "任务没有配置任何目标，请重新编辑"
		s.finishTask(ctx, t.ID, "failed", msg, 0, 0, nil, "")
		return errs.Invalid(msg)
	}

	var (
		refs                    []scheduleRef
		failures                []string
		firstBuild, firstDeploy uint64
	)

	for _, tg := range targets {
		bID, dID, err := s.executeTarget(ctx, t, tg, caller, triggerNote)
		if err != nil {
			// 只取 .Message：`*errs.Error.Error()` 带 `code=NNNNN` 前缀，那是给日志看的
			failures = append(failures, fmt.Sprintf("%s：%s", tg.ProjectKey, userMessage(err)))
			s.log("schedule").Warn("定时任务某个目标失败（其余继续）",
				"task", t.ID, "name", t.Name, "project", tg.ProjectKey, "err", userMessage(err))
			continue
		}
		if bID != 0 {
			refs = append(refs, scheduleRef{Kind: "build", ID: bID})
			if firstBuild == 0 {
				firstBuild = bID
			}
		}
		if dID != 0 {
			refs = append(refs, scheduleRef{Kind: "deploy", ID: dID})
			if firstDeploy == 0 {
				firstDeploy = dID
			}
		}
	}

	status := aggregateScheduleStatus(len(targets), len(failures))
	summary := ""
	if len(failures) > 0 {
		summary = fmt.Sprintf("%d 个目标中 %d 个失败 —— %s",
			len(targets), len(failures), strings.Join(failures, "；"))
	}

	s.log("schedule").Info("定时任务已建单", "task", t.ID, "name", t.Name,
		"targets", len(targets), "failed", len(failures), "status", status)
	s.finishTask(ctx, t.ID, status, summary, firstBuild, firstDeploy, refs, "")
	if summary != "" {
		// ⚠️ 必须带业务码返回（Upstream），不能直接 `errors.New(summary)`：
		// 裸 error 会被 errs.From 归一化成 50000「服务端内部错误，请联系管理员」，
		// 那句人话（哪个服务、为什么失败）就**整段丢掉**了。
		// 手动"立即执行"时这就是用户唯一能看到的原因。
		return errs.Upstream(nil, "%s", summary)
	}
	return nil
}

// aggregateScheduleStatus 批量执行的结果档位。
//
// 三档而不是两档：批量的失败通常是"一半成功、一半失败"（某个分支写错、
// 某个服务没构建过）。并进 success 会让人以为全发完了，
// 并进 failed 又会让"已经发上去那一半"看起来像没发生 —— 回滚 / 重试前
// 必须先知道是哪些成功。
func aggregateScheduleStatus(total, failed int) string {
	switch {
	case total <= 0 || failed >= total:
		return "failed"
	case failed > 0:
		return "partial"
	default:
		return "success"
	}
}

// executeTarget 建一个目标对应的单。
//
// 返回的 id 是**本次新建的那条记录**，不是任务上的历史值：
// 调用方要用它拼 LastIDs，供"上次还没跑完就跳过"逐条判断。
func (s *ScheduleService) executeTarget(ctx context.Context, t *model.ScheduleTask, tg model.ScheduleTarget,
	caller Caller, triggerNote string) (buildID, deployID uint64, err error) {
	switch t.TaskType {
	case "build":
		buildID, err = s.triggerBuild(ctx, t, tg, caller)
		return buildID, 0, err
	case "deploy":
		in, ierr := s.buildDeployInput(ctx, t, tg, triggerNote)
		if ierr != nil {
			return 0, 0, ierr
		}
		dto, derr := s.cd.CreateDeployment(ctx, in, caller)
		if derr != nil {
			return 0, 0, derr
		}
		if dto != nil {
			deployID = parseUint64(dto.ID)
		}
		return 0, deployID, nil
	default:
		return 0, 0, fmt.Errorf("未知的任务类型 %q", t.TaskType)
	}
}

// triggerBuild 触发一次构建，**按项目的构建方式分流**。
//
// ⚠️ 这里必须分流，否则定时构建对绝大多数项目是死的：
// 平台有两条完全独立的构建链路 ——
//
//	· `dockerfile` / `tag`：平台自己拉代码、跑 docker build、推镜像 → TriggerBuild
//	· `jenkins`：平台只负责调 Jenkins API，产物由 Jenkins 自己打包推送 → TriggerJenkinsBuild
//
// 而 `TriggerBuild` 对 jenkins 项目是**兜底拒绝**的（防止平台拿 Jenkins 的参数
// 当自己的分支名去跑一次 docker 构建，最后落一条与 Jenkins 对不上的记录）。
// 实测（2026-10-29 本机）：只调 TriggerBuild 时，Jenkins 项目直接 409
// 「平台不会代为执行 docker 构建」—— 功能等于只对少数项目可用。
func (s *ScheduleService) triggerBuild(ctx context.Context, t *model.ScheduleTask, tg model.ScheduleTarget, caller Caller) (uint64, error) {
	var p model.Project
	if err := s.db(ctx).Where("project_key = ?", tg.ProjectKey).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, errs.NotFoundf("项目 %s 不存在（可能已被删除）", tg.ProjectKey)
		}
		return 0, errs.Internalf(err, "查询项目失败")
	}
	params := decodeScheduleParams(t.BuildParams)

	if strings.EqualFold(strings.TrimSpace(p.BuildMethod), "jenkins") {
		job := strings.TrimSpace(p.JenkinsJob)
		if job == "" {
			return 0, errs.Invalid(fmt.Sprintf(
				"项目 %s 的构建方式是 Jenkins Job，但还没绑定具体 Job。"+
					"请先到「项目管理」里选好 Job，否则定时任务无法触发任何构建", tg.ProjectKey))
		}
		res, err := s.ci.TriggerJenkinsBuild(ctx, JenkinsTriggerInput{
			FullName:   job,
			Parameters: jenkinsParams(params, tg),
		}, caller)
		if err != nil {
			return 0, err
		}
		if res == nil {
			return 0, errs.Internalf(nil, "触发 Jenkins 构建未返回结果")
		}
		return res.BuildID, nil
	}

	dto, err := s.ci.TriggerBuild(ctx, TriggerBuildInput{
		ProjectKey:  tg.ProjectKey,
		Branch:      tg.Branch,
		Params:      params,
		TriggerType: "schedule",
		TriggerName: "定时任务：" + t.Name,
	}, caller)
	if err != nil {
		return 0, err
	}
	if dto == nil {
		return 0, errs.Internalf(nil, "触发构建未返回结果")
	}
	return parseUint64(dto.ID), nil
}

// jenkinsParams 把任务里存的参数表 + 目标上的分支转成 Jenkins 的 Name/Value 列表。
//
// 顺序按 key 排序，仅为了同样的任务每次发出的请求体一致（便于比对日志）。
//
// ⚠️ 分支（tg.Branch + tg.BranchParam）**优先于同名的任务级参数**：
// 目标上写的是"这次要用哪个分支"，是更具体的意图。反过来会让用户在行里改了分支
// 却仍按任务级参数构建 —— 界面显示与结果不一致，而且不报错。
func jenkinsParams(m map[string]string, tg model.ScheduleTarget) []integration.JenkinsBuildParam {
	branchParam := strings.TrimSpace(tg.BranchParam)
	branch := strings.TrimSpace(tg.Branch)

	out := make([]integration.JenkinsBuildParam, 0, len(m)+1)
	if branch != "" && branchParam != "" {
		out = append(out, integration.JenkinsBuildParam{Name: branchParam, Value: branch})
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if branchParam != "" && k == branchParam {
			continue // 已被目标上的分支覆盖
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, integration.JenkinsBuildParam{Name: k, Value: m[k]})
	}
	if len(out) == 0 {
		// 空 = 全部用 Jenkins 自己配的默认值，与在 Jenkins 页面直接点 Build 一致
		return nil
	}
	return out
}

// buildDeployInput 把任务配置 + 一个目标翻译成一次部署的入参。
func (s *ScheduleService) buildDeployInput(ctx context.Context, t *model.ScheduleTask, tg model.ScheduleTarget,
	triggerNote string) (CreateDeploymentInput, error) {
	in := CreateDeploymentInput{
		EnvKey:     t.EnvKey,
		ProjectKey: tg.ProjectKey,
		Replicas:   t.Replicas,
		Remark:     "定时任务：" + t.Name,
	}
	if triggerNote != "" {
		in.Remark += "（" + triggerNote + "）"
	}

	switch strings.TrimSpace(t.ImageSource) {
	case "latest":
		// "每次取最新" = 该项目在平台上**最新一次成功构建**。
		//
		// 刻意不走镜像仓库：与批量发布的口径一致（2026-10-27 拍板），
		// 而且平台构建记录是"我们自己造出来的产物"，可追溯；
		// 仓库里可能有别人手工推的 tag，拿它发版等于把一个来历不明的版本推到线上。
		var rec model.ImageRecord
		err := s.db(ctx).Where("project_key = ?", tg.ProjectKey).Order("id DESC").First(&rec).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return in, errs.Invalid(fmt.Sprintf(
					"项目 %s 在平台上还没有任何构建记录，「每次取最新」发不出去。请先构建一次，或把任务改成指定版本",
					tg.ProjectKey))
			}
			return in, errs.Internalf(err, "查询最新构建记录失败")
		}
		in.ImageSource = "platform"
		in.ImageRecordID = strconv.FormatUint(rec.ID, 10)
		in.Remark += "（取最新构建 " + rec.Tag + "）"
	default: // fixed
		if id := strings.TrimSpace(tg.ImageRecordID); id != "" {
			in.ImageSource = "platform"
			in.ImageRecordID = id
			if tg.ImageTag != "" {
				in.Remark += "（" + tg.ImageTag + "）"
			}
		} else if tag := strings.TrimSpace(tg.ImageTag); tag != "" {
			in.ImageSource = "registry"
			in.RegistryTag = tag
			in.Remark += "（仓库标签 " + tag + "）"
		} else {
			return in, errs.Invalid(fmt.Sprintf(
				"项目 %s 没有指定版本：给它选一个具体版本，或把整个任务改成「每次取最新」", tg.ProjectKey))
		}
	}
	return in, nil
}

// stillRunning 上次建的单是不是还在跑。
//
// 用**实际记录的 status** 判断，而不是任务的 LastStatus：
// 后者在进程重启后可能是残留的 "running"，而那条构建其实早就结束了 ——
// 用残留状态判断会让任务从此被永久跳过，且没有任何报错。
//
// 多目标必须**逐条**看（LastIDs）：只看第一条会出现"第一条早结束了、
// 第二条还在跑，于是又发一轮"，同一服务的部署单会撞锁、构建会白跑一遍。
func (s *ScheduleService) stillRunning(ctx context.Context, t *model.ScheduleTask) (bool, string) {
	refs := scheduleRefsOf(t)
	if len(refs) == 0 {
		return false, ""
	}
	var builds, deploys []uint64
	for _, r := range refs {
		switch r.Kind {
		case "build":
			builds = append(builds, r.ID)
		case "deploy":
			deploys = append(deploys, r.ID)
		}
	}
	if len(builds) > 0 {
		var n int64
		err := s.db(ctx).Model(&model.BuildRecord{}).
			Where("id IN ? AND status IN ?", builds, []string{"queued", "running"}).
			Count(&n).Error
		// 查询失败时放行：宁可多发一次，也不要因为一次查询抖动把任务永久跳过。
		if err == nil && n > 0 {
			return true, fmt.Sprintf("上次还有 %d 个构建没结束，本次跳过（不并发发两轮）", n)
		}
	}
	if len(deploys) > 0 {
		var n int64
		err := s.db(ctx).Model(&model.DeployRecord{}).
			Where("id IN ? AND status IN ?", deploys,
				[]string{"queued", "progressing", "pending_approval"}).
			Count(&n).Error
		if err == nil && n > 0 {
			return true, fmt.Sprintf("上次还有 %d 个部署没结束，本次跳过（不并发发两轮）", n)
		}
	}
	return false, ""
}

// finishTask 写回执行结果。
//
// ⚠️ 不碰 next_run_at：它已经在 tick 里推进过了。
// 这里再算一次会与 tick 的结果不一致，导致"下次执行时间"来回跳。
//
// refs 是本次建出的**全部**记录，落进 last_ids 供下一轮逐条判断"是否还没跑完"；
// last_build_id / last_deploy_id 仍取第一条，只用于界面上的"看构建 / 看部署"跳转。
func (s *ScheduleService) finishTask(ctx context.Context, id uint64, status, errMsg string,
	buildID, deployID uint64, refs []scheduleRef, note string) {
	at := now()
	updates := map[string]any{
		"last_status": status,
		"last_run_at": at,
		"last_error":  truncate(errMsg+note, 500),
	}
	if buildID != 0 {
		updates["last_build_id"] = buildID
	}
	if deployID != 0 {
		updates["last_deploy_id"] = deployID
	}
	if len(refs) > 0 {
		updates["last_ids"] = encodeScheduleRefs(refs)
	} else if status != "success" && status != "partial" && status != "running" {
		// 本次一个单都没建出来（全失败 / 跳过）：清掉上一轮的记录，
		// 否则"上次还没跑完就跳过"会用上一轮的 id 一直误判。
		updates["last_ids"] = nil
	}
	if status != "missed" {
		// 跑过一次就说明"错过计数"该归零：否则错过了 3 次之后恢复正常，
		// 界面上一直挂着"错过 3 次"，用户会以为还在出问题。
		updates["missed_count"] = 0
	}
	if err := s.db(ctx).Model(&model.ScheduleTask{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		s.log("schedule").Warn("写回定时任务结果失败", "task", id, "err", err)
	}
}

// --------------------------------------------------------------------------- 内部助手

// normalize 归一化 + 校验入参（新建和编辑共用，避免两处口径分家）。
func (s *ScheduleService) normalize(ctx context.Context, in ScheduleInput, c Caller) (ScheduleInput, error) {
	out := in
	out.Name = strings.TrimSpace(in.Name)
	out.TaskType = strings.TrimSpace(in.TaskType)
	out.EnvKey = strings.TrimSpace(in.EnvKey)
	out.Cron = strings.TrimSpace(in.Cron)
	out.ImageSource = strings.TrimSpace(in.ImageSource)
	out.Remark = strings.TrimSpace(in.Remark)

	if out.Name == "" {
		return out, errs.Invalid("任务名不能为空（它是构建/部署记录上显示的操作人名字）")
	}
	if _, err := scheduleCronParser.Parse(out.Cron); err != nil {
		return out, errs.Invalidf("cron 表达式不合法：%v。格式是「分 时 日 月 周」，"+
			"例如 0 2 * * * 表示每天 02:00", err)
	}

	// 目标归一化 + 项目存在性（一次查完，别 N+1）
	targets, projs, err := s.normalizeTargets(ctx, in)
	if err != nil {
		return out, err
	}
	out.Targets = targets
	// 镜像列统一从第一个目标派生，别信客户端传来的旧字段
	out.ProjectKey = targets[0].ProjectKey
	out.Branch = targets[0].Branch
	out.ImageTag = targets[0].ImageTag
	out.ImageRecordID = targets[0].ImageRecordID

	switch out.TaskType {
	case "build":
		out.EnvKey = "" // build 不涉及环境，防止误存一个不相干的值
		out.ImageSource, out.Replicas = "", nil
		if c.User != nil && !c.User.HasAnyPerm("build:trigger", "*") {
			return out, errs.Forbidden("你没有触发构建的权限（build:trigger），无法配置定时构建")
		}
		for i := range out.Targets {
			tg := &out.Targets[i]
			// build 任务不带版本（版本是交付物，不是输入）
			tg.ImageTag, tg.ImageRecordID = "", ""
			p := projs[tg.ProjectKey]
			jenkins := strings.EqualFold(strings.TrimSpace(p.BuildMethod), "jenkins")
			if jenkins {
				// Jenkins 项目必须已经绑定了 Job —— 提前拦住。
				// 不拦的话任务能建成功、界面显示"已启用/下次执行时间"，但每次执行都失败，
				// 而用户要等到点之后才发现（配置期本该是最容易发现的时候）。
				if strings.TrimSpace(p.JenkinsJob) == "" {
					return out, errs.Invalid(fmt.Sprintf(
						"项目 %s 的构建方式是 Jenkins Job，但还没有绑定具体 Job。"+
							"请先到「项目管理」里选好 Job，再来配定时构建", tg.ProjectKey))
				}
				// Jenkins 的分支是"某个参数的值"：说了分支就必须说清传给哪个参数，
				// 否则到点只能不传 —— 那等于悄悄用了 Jenkins 的默认分支。
				if tg.Branch != "" && tg.BranchParam == "" {
					return out, errs.Invalid(fmt.Sprintf(
						"项目 %s 选了分支 %s，但没说明要传给 Jenkins 的哪个参数。"+
							"请在编辑时重新选一次分支（界面会自动带上参数名）", tg.ProjectKey, tg.Branch))
				}
				continue
			}
			// 平台自建构建：分支必填。
			//
			// 用户明确要求（2026-10-29）："定时构建肯定要填写项目的分支啊，不用默认参数，
			// 手动可选"。理由很实在 —— 定时任务半夜跑，如果分支靠默认值，
			// 项目默认分支一改，第二天构建的就不是你排的那条了，而且没人知道。
			if tg.Branch == "" {
				return out, errs.Invalid(fmt.Sprintf(
					"项目 %s 没有指定分支。定时构建请明确写上分支，"+
						"不要让它在到点时才由默认值决定", tg.ProjectKey))
			}
		}
	case "deploy":
		if out.EnvKey == "" {
			return out, errs.Invalid("定时部署必须选择环境")
		}
		var en int64
		if err := s.db(ctx).Model(&model.Environment{}).Where("env_key = ?", out.EnvKey).Count(&en).Error; err != nil {
			return out, errs.Internalf(err, "查询环境失败")
		}
		if en == 0 {
			return out, errs.NotFoundf("环境 %s 不存在", out.EnvKey)
		}
		if out.ImageSource == "" {
			out.ImageSource = "fixed"
		}
		for i := range out.Targets {
			tg := &out.Targets[i]
			// 部署不用分支（分支是构建的输入）
			tg.Branch, tg.BranchParam = "", ""
			if out.ImageSource == "fixed" {
				if tg.ImageTag == "" && tg.ImageRecordID == "" {
					return out, errs.Invalid(fmt.Sprintf(
						"项目 %s 还没选要发的版本：给它选一个具体版本，"+
							"或把整个任务改成「每次取最新」", tg.ProjectKey))
				}
			} else {
				// latest：目标上的固定版本一律清掉，避免"看着写了固定版本、实际发的是最新"
				tg.ImageTag, tg.ImageRecordID = "", ""
			}
		}
		// 权限按环境校验，与手动部署同一口径
		if c.User != nil && !c.User.HasAnyPerm("deploy:"+out.EnvKey, "deploy:*", "*") {
			return out, errs.Forbidden(fmt.Sprintf(
				"你没有 %s 环境的部署权限，无法在那里配置定时部署", out.EnvKey))
		}
	default:
		return out, errs.Invalid("任务类型只能是 build 或 deploy")
	}

	// Enabled 保持指针语义（nil = "没传"，由调用方决定默认值），
	// 这里不强行赋默认值 —— 编辑时不传就表示"不改启停状态"。
	return out, nil
}

// normalizeTargets 把入参折叠成目标清单，并顺手校验项目存在。
//
// 兼容两种写法：新界面给 Targets（多目标），旧调用只给 ProjectKey（单目标）。
// 两条路在这里汇成一个切片 —— 后面只有一套执行逻辑。
func (s *ScheduleService) normalizeTargets(ctx context.Context, in ScheduleInput) ([]model.ScheduleTarget, map[string]model.Project, error) {
	raw := in.Targets
	if len(raw) == 0 {
		if k := strings.TrimSpace(in.ProjectKey); k != "" {
			raw = []model.ScheduleTarget{{
				ProjectKey:    k,
				Branch:        in.Branch,
				ImageTag:      in.ImageTag,
				ImageRecordID: in.ImageRecordID,
			}}
		}
	}
	if len(raw) == 0 {
		return nil, nil, errs.Invalid("请至少选择一个项目：定时任务要明确对谁做")
	}
	if len(raw) > maxScheduleTargets {
		return nil, nil, errs.Invalidf("一条任务最多 %d 个目标，请分批配置", maxScheduleTargets)
	}

	out := make([]model.ScheduleTarget, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for i, t := range raw {
		k := strings.TrimSpace(t.ProjectKey)
		if k == "" {
			return nil, nil, errs.Invalidf("第 %d 个目标没有选项目", i+1)
		}
		if seen[k] {
			// 同一个项目在一条任务里出现两次：部署会撞同一把锁、构建会白跑一遍，
			// 而且界面上一行一行长得一样，用户根本看不出重复。
			return nil, nil, errs.Invalidf("项目 %s 重复了：同一个任务里同一个项目只需出现一次", k)
		}
		seen[k] = true
		out = append(out, model.ScheduleTarget{
			ProjectKey:    k,
			Branch:        strings.TrimSpace(t.Branch),
			BranchParam:   strings.TrimSpace(t.BranchParam),
			ImageTag:      strings.TrimSpace(t.ImageTag),
			ImageRecordID: strings.TrimSpace(t.ImageRecordID),
		})
	}

	keys := make([]string, 0, len(out))
	for _, t := range out {
		keys = append(keys, t.ProjectKey)
	}
	var projs []model.Project
	if err := s.db(ctx).Where("project_key IN ?", keys).Find(&projs).Error; err != nil {
		return nil, nil, errs.Internalf(err, "查询项目失败")
	}
	byKey := make(map[string]model.Project, len(projs))
	for _, p := range projs {
		byKey[p.ProjectKey] = p
	}
	for _, t := range out {
		if _, ok := byKey[t.ProjectKey]; !ok {
			return nil, nil, errs.NotFoundf("项目 %s 不存在", t.ProjectKey)
		}
	}
	return out, byKey, nil
}

// scheduleOf 按 id 取，并校验存在。
func (s *ScheduleService) scheduleOf(ctx context.Context, id uint64) (*model.ScheduleTask, error) {
	var t model.ScheduleTask
	if err := s.db(ctx).First(&t, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("定时任务不存在（可能已被删除）")
		}
		return nil, errs.Internalf(err, "查询定时任务失败")
	}
	return &t, nil
}

func scheduleDTO(t *model.ScheduleTask) ScheduleDTO {
	d := ScheduleDTO{
		ID:            strconv.FormatUint(t.ID, 10),
		Name:          t.Name,
		TaskType:      t.TaskType,
		Targets:       scheduleTargetsOf(t),
		ProjectKey:    t.ProjectKey,
		EnvKey:        t.EnvKey,
		Cron:          t.Cron,
		Enabled:       t.Enabled,
		Branch:        t.Branch,
		BuildParams:   decodeScheduleParams(t.BuildParams),
		ImageSource:   t.ImageSource,
		ImageTag:      t.ImageTag,
		ImageRecordID: t.ImageRecordID,
		Replicas:      t.Replicas,
		Remark:        t.Remark,
		NextRunAt:     t.NextRunAt,
		LastRunAt:     t.LastRunAt,
		LastStatus:    t.LastStatus,
		LastError:     t.LastError,
		MissedCount:   t.MissedCount,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
	}
	if t.LastBuildID != 0 {
		d.LastBuildID = strconv.FormatUint(t.LastBuildID, 10)
	}
	if t.LastDeployID != 0 {
		d.LastDeployID = strconv.FormatUint(t.LastDeployID, 10)
	}
	return d
}

// nextRunAfter 从 from 往后算下一次执行时间。表达式非法时返回 nil（不 panic）。
func nextRunAfter(expr string, from time.Time) *time.Time {
	sch, err := scheduleCronParser.Parse(strings.TrimSpace(expr))
	if err != nil {
		return nil
	}
	t := sch.Next(from)
	if t.IsZero() {
		return nil
	}
	return &t
}

// countMissed 统计 from 到 to 之间本该执行几次（不含 to 之后）。
func countMissed(expr string, from, to time.Time) int {
	sch, err := scheduleCronParser.Parse(strings.TrimSpace(expr))
	if err != nil {
		return 0
	}
	n, cur := 0, from
	for n < maxMissedScan {
		t := sch.Next(cur)
		if t.IsZero() || t.After(to) {
			break
		}
		n++
		cur = t
	}
	return n
}

// encodeScheduleParams 把构建参数存成 JSONText。
func encodeScheduleParams(m map[string]string) model.JSONText {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return model.JSONText(b)
}

func decodeScheduleParams(raw model.JSONText) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// scheduleRef 上次执行建出的一条记录（用于"上次还没跑完就跳过"逐条判断）。
type scheduleRef struct {
	Kind string `json:"k"` // build / deploy
	ID   uint64 `json:"id"`
}

// scheduleTargetsOf 从任务里取目标清单。
//
// 优先读 targets 列；旧数据（这一列是 NULL）由镜像列还原成一条 ——
// 这样升级后老任务照常跑，不需要人工迁移。
// **绝不能反过来**（先看镜像列）：多目标任务的镜像列只有第一个，
// 那样会静默地只执行一个目标，而界面上明明写着 6 个。
func scheduleTargetsOf(t *model.ScheduleTask) []model.ScheduleTarget {
	var list []model.ScheduleTarget
	if len(t.Targets) > 0 {
		if err := json.Unmarshal(t.Targets, &list); err != nil {
			// 坏 JSON 不能让调度器崩：退回镜像列，至少还能跑一个。
			list = nil
		}
	}
	out := make([]model.ScheduleTarget, 0, len(list))
	for _, x := range list {
		if k := strings.TrimSpace(x.ProjectKey); k != "" {
			x.ProjectKey = k
			out = append(out, x)
		}
	}
	if len(out) > 0 {
		return out
	}
	if t.ProjectKey == "" {
		return nil
	}
	return []model.ScheduleTarget{{
		ProjectKey:    t.ProjectKey,
		Branch:        t.Branch,
		ImageTag:      t.ImageTag,
		ImageRecordID: t.ImageRecordID,
	}}
}

func encodeScheduleTargets(list []model.ScheduleTarget) model.JSONText {
	if len(list) == 0 {
		return nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return model.JSONText(b)
}

// scheduleRefsOf 上次建出的全部记录。旧数据（last_ids 为空）退回单条 id。
func scheduleRefsOf(t *model.ScheduleTask) []scheduleRef {
	var list []scheduleRef
	if len(t.LastIDs) > 0 {
		if err := json.Unmarshal(t.LastIDs, &list); err != nil {
			list = nil
		}
	}
	if len(list) > 0 {
		return list
	}
	out := make([]scheduleRef, 0, 2)
	if t.LastBuildID != 0 {
		out = append(out, scheduleRef{Kind: "build", ID: t.LastBuildID})
	}
	if t.LastDeployID != 0 {
		out = append(out, scheduleRef{Kind: "deploy", ID: t.LastDeployID})
	}
	return out
}

func encodeScheduleRefs(list []scheduleRef) model.JSONText {
	if len(list) == 0 {
		return nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return model.JSONText(b)
}

func parseUint64(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
