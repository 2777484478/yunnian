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
	// 这里只等"建单"这一步（入队即返回），不等构建跑完，所以 2 分钟很宽裕。
	scheduleExecTimeout = 2 * time.Minute
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
	ID         string `json:"id"`
	Name       string `json:"name"`
	TaskType   string `json:"taskType"`
	ProjectKey string `json:"projectKey"`
	EnvKey     string `json:"envKey"`
	Cron       string `json:"cron"`
	Enabled    bool   `json:"enabled"`

	Branch      string            `json:"branch"`
	BuildParams map[string]string `json:"buildParams,omitempty"`

	ImageSource   string `json:"imageSource"`
	ImageTag      string `json:"imageTag"`
	ImageRecordID string `json:"imageRecordId"`
	Replicas      *int   `json:"replicas"`

	Remark string `json:"remark"`

	NextRunAt   *time.Time `json:"nextRunAt"`
	LastRunAt   *time.Time `json:"lastRunAt"`
	LastStatus  string     `json:"lastStatus"`
	LastError   string     `json:"lastError"`
	MissedCount int        `json:"missedCount"`
	LastBuildID string     `json:"lastBuildId"`
	LastDeployID string    `json:"lastDeployId"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ScheduleInput 新建 / 编辑入参。
type ScheduleInput struct {
	Name     string `json:"name" binding:"required,max=64"`
	TaskType string `json:"taskType" binding:"required,oneof=build deploy"`
	// ProjectKey / EnvKey 在定时任务里**不做**"已纳管"校验：
	// 定时部署的典型场景恰恰是"资源还没纳管、先把发布排上"。
	// 但项目和环境本身必须存在，否则任务永远跑不起来。
	ProjectKey string `json:"projectKey" binding:"required,max=64"`
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
	Valid  bool        `json:"valid"`
	Error  string      `json:"error"`
	Next   []time.Time `json:"next"`
}

// --------------------------------------------------------------------------- 查询

// ListSchedules 定时任务列表（可按类型 / 项目筛）。
func (s *ScheduleService) ListSchedules(ctx context.Context, taskType, projectKey string) ([]ScheduleDTO, error) {
	db := s.db(ctx).Model(&model.ScheduleTask{})
	if t := strings.TrimSpace(taskType); t != "" {
		db = db.Where("task_type = ?", t)
	}
	if p := strings.TrimSpace(projectKey); p != "" {
		db = db.Where("project_key = ?", p)
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
	t := &model.ScheduleTask{
		Name:          norm.Name,
		TaskType:      norm.TaskType,
		ProjectKey:    norm.ProjectKey,
		EnvKey:        norm.EnvKey,
		Cron:          norm.Cron,
		// 新建默认启用（用户点了"创建"就是要它跑起来）
		Enabled:       true,
		Branch:        norm.Branch,
		BuildParams:   encodeScheduleParams(norm.BuildParams),
		ImageSource:   norm.ImageSource,
		ImageTag:      norm.ImageTag,
		ImageRecordID: norm.ImageRecordID,
		Replicas:      norm.Replicas,
		Remark:        norm.Remark,
		CreatedBy:     uid,
		UpdatedBy:     uid,
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
	updates := map[string]any{
		"name":            norm.Name,
		"task_type":       norm.TaskType,
		"project_key":     norm.ProjectKey,
		"env_key":         norm.EnvKey,
		"cron":            norm.Cron,
		"build_params":    encodeScheduleParams(norm.BuildParams),
		"branch":          norm.Branch,
		"image_source":    norm.ImageSource,
		"image_tag":       norm.ImageTag,
		"image_record_id": norm.ImageRecordID,
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
			"next_run_at":  next,
			"last_status":  "running",
			"last_error":   "",
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
				s.finishTask(ectx, taskID, "skipped", "", 0, 0, "已被停用，本次不执行")
				return
			}
			if busy, reason := s.stillRunning(ectx, &row); busy {
				s.finishTask(ectx, taskID, "skipped", "", 0, 0, reason)
				return
			}
			_ = s.execute(ectx, &row, "")
		}()
	}
}

// --------------------------------------------------------------------------- 执行

// execute 建单。triggerNote 非空时附加到备注（用于区分手动立即执行）。
func (s *ScheduleService) execute(ctx context.Context, t *model.ScheduleTask, triggerNote string) error {
	// 用 DisplayName 而不是冒充某个用户：
	// 定时任务没有"当前登录人"，冒用创建者的身份会让审计失真。
	// 同时 User 为 nil 意味着跳过部署的权限校验 —— 权限在**创建任务时**校验过。
	caller := Caller{DisplayName: "定时任务：" + t.Name}

	var (
		buildID  uint64
		deployID uint64
		err      error
	)

	switch t.TaskType {
	case "build":
		buildID, err = s.triggerBuild(ctx, t, caller)
	case "deploy":
		var in CreateDeploymentInput
		in, err = s.buildDeployInput(ctx, t, triggerNote)
		if err == nil {
			var dto *DeployDTO
			dto, err = s.cd.CreateDeployment(ctx, in, caller)
			if err == nil && dto != nil {
				deployID = parseUint64(dto.ID)
			}
		}
	default:
		err = fmt.Errorf("未知的任务类型 %q", t.TaskType)
	}

	next := nextRunAfter(t.Cron, now())
	if err != nil {
		msg := userMessage(err)
		s.log("schedule").Warn("定时任务执行失败", "task", t.ID, "name", t.Name, "err", msg)
		s.finishTask(ctx, t.ID, "failed", msg, buildID, deployID, "")
		return err
	}
	s.log("schedule").Info("定时任务已建单", "task", t.ID, "name", t.Name,
		"buildId", buildID, "deployId", deployID)
	s.finishTask(ctx, t.ID, "success", "", buildID, deployID, "")
	_ = next
	return nil
}

// triggerBuild 触发一次构建，**按项目的构建方式分流**。
//
// ⚠️ 这里必须分流，否则定时构建对绝大多数项目是死的：
// 平台有两条完全独立的构建链路 ——
//   · `dockerfile` / `tag`：平台自己拉代码、跑 docker build、推镜像 → TriggerBuild
//   · `jenkins`：平台只负责调 Jenkins API，产物由 Jenkins 自己打包推送 → TriggerJenkinsBuild
// 而 `TriggerBuild` 对 jenkins 项目是**兜底拒绝**的（防止平台拿 Jenkins 的参数
// 当自己的分支名去跑一次 docker 构建，最后落一条与 Jenkins 对不上的记录）。
// 实测（2026-10-29 本机）：只调 TriggerBuild 时，Jenkins 项目直接 409
// 「平台不会代为执行 docker 构建」—— 功能等于只对少数项目可用。
func (s *ScheduleService) triggerBuild(ctx context.Context, t *model.ScheduleTask, caller Caller) (uint64, error) {
	var p model.Project
	if err := s.db(ctx).Where("project_key = ?", t.ProjectKey).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, errs.NotFoundf("项目 %s 不存在（可能已被删除）", t.ProjectKey)
		}
		return 0, errs.Internalf(err, "查询项目失败")
	}
	params := decodeScheduleParams(t.BuildParams)

	if strings.EqualFold(strings.TrimSpace(p.BuildMethod), "jenkins") {
		job := strings.TrimSpace(p.JenkinsJob)
		if job == "" {
			return 0, errs.Invalid(fmt.Sprintf(
				"项目 %s 的构建方式是 Jenkins Job，但还没绑定具体 Job。"+
					"请先到「项目管理」里选好 Job，否则定时任务无法触发任何构建", t.ProjectKey))
		}
		res, err := s.ci.TriggerJenkinsBuild(ctx, JenkinsTriggerInput{
			FullName:   job,
			Parameters: jenkinsParams(params),
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
		ProjectKey:  t.ProjectKey,
		Branch:      t.Branch,
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

// jenkinsParams 把任务里存的参数表转成 Jenkins 的 Name/Value 列表。
//
// 顺序按 key 排序，仅为了同样的任务每次发出的请求体一致（便于比对日志）。
func jenkinsParams(m map[string]string) []integration.JenkinsBuildParam {
	if len(m) == 0 {
		// 空 = 全部用 Jenkins 自己配的默认值，与在 Jenkins 页面直接点 Build 一致
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]integration.JenkinsBuildParam, 0, len(keys))
	for _, k := range keys {
		out = append(out, integration.JenkinsBuildParam{Name: k, Value: m[k]})
	}
	return out
}

// buildDeployInput 把任务配置翻译成一次部署的入参。
func (s *ScheduleService) buildDeployInput(ctx context.Context, t *model.ScheduleTask, triggerNote string) (CreateDeploymentInput, error) {
	in := CreateDeploymentInput{
		EnvKey:     t.EnvKey,
		ProjectKey: t.ProjectKey,
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
		err := s.db(ctx).Where("project_key = ?", t.ProjectKey).Order("id DESC").First(&rec).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return in, errs.Invalid(fmt.Sprintf(
					"项目 %s 在平台上还没有任何构建记录，「每次取最新」发不出去。请先构建一次，或把任务改成指定版本",
					t.ProjectKey))
			}
			return in, errs.Internalf(err, "查询最新构建记录失败")
		}
		in.ImageSource = "platform"
		in.ImageRecordID = strconv.FormatUint(rec.ID, 10)
		in.Remark += "（取最新构建 " + rec.Tag + "）"
	default: // fixed
		if id := strings.TrimSpace(t.ImageRecordID); id != "" {
			in.ImageSource = "platform"
			in.ImageRecordID = id
			if t.ImageTag != "" {
				in.Remark += "（" + t.ImageTag + "）"
			}
		} else if tag := strings.TrimSpace(t.ImageTag); tag != "" {
			in.ImageSource = "registry"
			in.RegistryTag = tag
			in.Remark += "（仓库标签 " + tag + "）"
		} else {
			return in, errs.Invalid("任务没有指定版本：请选一个具体版本，或改成「每次取最新」")
		}
	}
	return in, nil
}

// stillRunning 上次建的单是不是还在跑。
//
// 用**实际记录的 status** 判断，而不是任务的 LastStatus：
// 后者在进程重启后可能是残留的 "running"，而那条构建其实早就结束了 ——
// 用残留状态判断会让任务从此被永久跳过，且没有任何报错。
func (s *ScheduleService) stillRunning(ctx context.Context, t *model.ScheduleTask) (bool, string) {
	switch t.TaskType {
	case "build":
		if t.LastBuildID == 0 {
			return false, ""
		}
		var n int64
		if err := s.db(ctx).Model(&model.BuildRecord{}).
			Where("id = ? AND status IN ?", t.LastBuildID, []string{"queued", "running"}).
			Count(&n).Error; err != nil {
			return false, ""
		}
		if n > 0 {
			return true, fmt.Sprintf("上次构建（#%d）还在执行中，本次跳过", t.LastBuildID)
		}
	case "deploy":
		if t.LastDeployID == 0 {
			return false, ""
		}
		var n int64
		if err := s.db(ctx).Model(&model.DeployRecord{}).
			Where("id = ? AND status IN ?", t.LastDeployID,
				[]string{"queued", "progressing", "pending_approval"}).
			Count(&n).Error; err != nil {
			return false, ""
		}
		if n > 0 {
			return true, fmt.Sprintf("上次部署（#%d）还没结束，本次跳过", t.LastDeployID)
		}
	}
	return false, ""
}

// finishTask 写回执行结果。
//
// ⚠️ 不碰 next_run_at：它已经在 tick 里推进过了。
// 这里再算一次会与 tick 的结果不一致，导致"下次执行时间"来回跳。
func (s *ScheduleService) finishTask(ctx context.Context, id uint64, status, errMsg string, buildID, deployID uint64, note string) {
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
	out.ProjectKey = strings.TrimSpace(in.ProjectKey)
	out.EnvKey = strings.TrimSpace(in.EnvKey)
	out.Cron = strings.TrimSpace(in.Cron)
	out.Branch = strings.TrimSpace(in.Branch)
	out.ImageSource = strings.TrimSpace(in.ImageSource)
	out.ImageTag = strings.TrimSpace(in.ImageTag)
	out.ImageRecordID = strings.TrimSpace(in.ImageRecordID)
	out.Remark = strings.TrimSpace(in.Remark)

	if out.Name == "" {
		return out, errs.Invalid("任务名不能为空（它是构建/部署记录上显示的操作人名字）")
	}
	if _, err := scheduleCronParser.Parse(out.Cron); err != nil {
		return out, errs.Invalidf("cron 表达式不合法：%v。格式是「分 时 日 月 周」，"+
			"例如 0 2 * * * 表示每天 02:00", err)
	}

	// 项目必须存在（否则任务永远跑不起来，而用户要到点才发现）
	var pn int64
	if err := s.db(ctx).Model(&model.Project{}).Where("project_key = ?", out.ProjectKey).Count(&pn).Error; err != nil {
		return out, errs.Internalf(err, "查询项目失败")
	}
	if pn == 0 {
		return out, errs.NotFoundf("项目 %s 不存在", out.ProjectKey)
	}

	switch out.TaskType {
	case "build":
		out.EnvKey = "" // build 不涉及环境，防止误存一个不相干的值
		out.ImageSource, out.ImageTag, out.ImageRecordID, out.Replicas = "", "", "", nil
		if c.User != nil && !c.User.HasAnyPerm("build:trigger", "*") {
			return out, errs.Forbidden("你没有触发构建的权限（build:trigger），无法配置定时构建")
		}
		// Jenkins 项目必须已经绑定了 Job —— 提前拦住。
		// 不拦的话任务能建成功、界面显示"已启用/下次执行时间"，但每次执行都失败，
		// 而用户要等到点之后才发现（配置期本该是最容易发现的时候）。
		var p model.Project
		if err := s.db(ctx).Where("project_key = ?", out.ProjectKey).First(&p).Error; err == nil {
			if strings.EqualFold(strings.TrimSpace(p.BuildMethod), "jenkins") &&
				strings.TrimSpace(p.JenkinsJob) == "" {
				return out, errs.Invalid(fmt.Sprintf(
					"项目 %s 的构建方式是 Jenkins Job，但还没有绑定具体 Job。"+
						"请先到「项目管理」里选好 Job，再来配定时构建", out.ProjectKey))
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
		if out.ImageSource == "fixed" && out.ImageTag == "" && out.ImageRecordID == "" {
			return out, errs.Invalid("定时部署要指定发哪个版本：选一个具体版本，或改成「每次取最新」")
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
