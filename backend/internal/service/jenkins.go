// Jenkins 接入在 CI 侧的实现：浏览 Job、触发构建、把 Jenkins 的状态与日志同步回平台。
//
// 这一层要解决的三个问题是 Jenkins 自己**不提供**的：
//
//	① 触发返回的是队列项、不是构建号       → 轮询 /queue/item/{id} 把构建号换回来
//	② Jenkins 没有日志推送，只能轮询       → 增量拉 progressiveText，转写进平台日志流
//	③ Jenkins 的终态词表与平台不一致       → 映射成平台状态，让构建记录能收敛
//
// 写在这里而不是 handler 里，是因为三件事都需要"长时间、跨请求"地跑：
// handler 早返回了，轮询必须在任务池里继续。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/logstream"
	"github.com/2777484478/yunnian/internal/model"
	"github.com/2777484478/yunnian/internal/queue"
)

const (
	// jenkinsQueueWait 等 Jenkins 分配执行器的最长时间。超过它说明构建队列被占满了，
	// 继续等对用户没有意义——Jenkins 自己也会一直挂着，平台如实报出来更好。
	jenkinsQueueWait = 10 * time.Minute
	// jenkinsPollInterval 构建中查状态与拉日志的间隔。3s 是折中：
	// 更密会给 Jenkins 增加无谓压力（progressiveText 每次都要读构建日志文件），
	// 更稀日志就会一段一段地跳。
	jenkinsPollInterval = 3 * time.Second
	// jenkinsFinishGrace 收尾写库用的超时。任务上下文此时可能已被取消，
	// 必须换一个独立上下文，否则终态写不进去、构建永远停在"构建中"。
	jenkinsFinishGrace = 20 * time.Second
)

// --------------------------------------------------------------------------- 浏览

// JenkinsJobDTO 列表里的一个 Job：Jenkins 的字段 + 平台侧的绑定关系。
type JenkinsJobDTO struct {
	integration.JenkinsJob
	// BoundProjectKey 已绑定到哪个平台项目（未绑定为空串）。
	// 这一项由平台补，不是 Jenkins 的字段——Jenkins 不知道平台的存在。
	BoundProjectKey string `json:"boundProjectKey"`
}

// JenkinsJobListDTO Job 列表响应。
type JenkinsJobListDTO struct {
	Jobs []JenkinsJobDTO `json:"jobs"`
	// Source 固定 real：这些数据真的来自 Jenkins。前端留了 demo 分支，
	// 后端一旦接上就不可能再返回 demo，保留字段是为了让契约稳定。
	Source         string `json:"source"`
	FetchedAt      string `json:"fetchedAt"`
	Truncated      bool   `json:"truncated"`
	SkippedFolders int    `json:"skippedFolders"`
}

// jenkinsClient 取当前生效的 Jenkins 客户端。
//
// "未接入"必须翻译成人话并指明去哪配，而不是把内部错误原样抛出去——
// 用户看到 "Jenkins 接入未启用" 只会更困惑：去哪启用？
func (s *CIService) jenkinsClient(ctx context.Context) (*integration.JenkinsClient, error) {
	client, err := s.deps.Up.Jenkins.Client(ctx)
	if err == nil {
		return client, nil
	}
	if errors.Is(err, integration.ErrJenkinsDisabled) {
		return nil, errs.StateConflict("尚未接入 Jenkins：请先到「系统管理 › 第三方集成」填写 Jenkins 地址与凭据并启用")
	}
	return nil, upstreamError(err, "读取 Jenkins 配置失败")
}

// ListJenkinsJobs 拉取 Jenkins 上的 Job 列表（含文件夹内的），并标注绑定关系。
func (s *CIService) ListJenkinsJobs(ctx context.Context, refresh bool) (*JenkinsJobListDTO, error) {
	client, err := s.jenkinsClient(ctx)
	if err != nil {
		return nil, err
	}
	// refresh 目前不影响后端行为：每次调用都是实时去 Jenkins 拉，
	// 平台这一层**不做缓存**——"点刷新看到的是旧数据"比慢一点更难解释。
	_ = refresh

	res, err := client.ListJobs(ctx, s.deps.Cfg.Integration.Jenkins.MaxJobs)
	if err != nil {
		return nil, upstreamError(err, "拉取 Jenkins Job 列表失败")
	}
	bound, err := s.jenkinsBindings(ctx)
	if err != nil {
		return nil, err
	}

	out := &JenkinsJobListDTO{
		Jobs:           make([]JenkinsJobDTO, 0, len(res.Jobs)),
		Source:         "real",
		FetchedAt:      formatTime(now()),
		Truncated:      res.Truncated,
		SkippedFolders: res.SkippedFolders,
	}
	for _, j := range res.Jobs {
		out.Jobs = append(out.Jobs, JenkinsJobDTO{JenkinsJob: j, BoundProjectKey: bound[j.FullName]})
	}
	return out, nil
}

// jenkinsBindings 返回 jenkinsJob（含文件夹路径）→ 平台项目 key 的映射。
//
// 用 fullName 而不是裸名匹配：用了 Folders 插件之后，不同文件夹下可以有同名 Job，
// 拿裸名匹配会把两个不同的 Job 当成同一个。
//
// 一个 Job 被多个项目绑定时取 ID 最小的那个并记一条告警，而不是直接报错：
// 报错会让人一时无法触发任何构建，而多数情况下那几个项目的构建配置本来就一样。
func (s *CIService) jenkinsBindings(ctx context.Context) (map[string]string, error) {
	var rows []model.Project
	if err := s.db(ctx).Select("id", "project_key", "jenkins_job").
		Where("jenkins_job <> ''").Order("id").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询 Jenkins Job 绑定关系失败")
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		job := strings.TrimSpace(rows[i].JenkinsJob)
		if job == "" {
			continue
		}
		if prev, dup := out[job]; dup {
			s.log("ci.jenkins").Warn("同一个 Jenkins Job 被多个项目绑定，触发时以 ID 最小的为准",
				"jenkinsJob", job, "keep", prev, "skip", rows[i].ProjectKey)
			continue
		}
		out[job] = rows[i].ProjectKey
	}
	return out, nil
}

// jenkinsLookupError 把"按名字找 Jenkins Job"的失败翻译成对用户有用的错误。
//
// Job 不存在（Jenkins 404）回 404 并给出下一步动作；其余（网络不通、认证被拒、
// 响应解析失败…）才回 502。这个区分很实际：前者用户改个名字/重新绑定就好，
// 后者才需要去找 Jenkins 的运维。混为一谈会把人引到错误的排查方向上。
func jenkinsLookupError(err error, fullName string) error {
	if errors.Is(err, integration.ErrJobNotFound) {
		return errs.NotFoundf(
			"Jenkins 上不存在名为 %s 的 Job（可能已被删除或改名）；请点「刷新」重新拉取列表，或到项目里改绑其它 Job",
			fullName)
	}
	return upstreamError(err, "读取 Jenkins Job %s 的构建参数失败", fullName)
}

// JenkinsJobParams 读取某个 Job 的参数定义（不含 GitParameter 的可选值）。
func (s *CIService) JenkinsJobParams(ctx context.Context, fullName string) ([]integration.JenkinsParamDef, error) {
	fullName = strings.TrimSpace(fullName)
	if fullName == "" {
		return nil, errs.Invalid("缺少 Job 名称")
	}
	client, err := s.jenkinsClient(ctx)
	if err != nil {
		return nil, err
	}
	defs, err := client.JobParams(ctx, fullName)
	if err != nil {
		return nil, jenkinsLookupError(err, fullName)
	}
	return defs, nil
}

// JenkinsParamValues 读取 GitParameter 的可选值（版本标签 / 分支）。
func (s *CIService) JenkinsParamValues(ctx context.Context, fullName, param string) (*integration.JenkinsParamValues, error) {
	fullName, param = strings.TrimSpace(fullName), strings.TrimSpace(param)
	if fullName == "" || param == "" {
		return nil, errs.Invalid("缺少 Job 名称或参数名")
	}
	client, err := s.jenkinsClient(ctx)
	if err != nil {
		return nil, err
	}

	// 先读一次参数定义。这不是多余的一步：
	//   - 类名要拿来做 descriptorByName 的路径（写死 GitParameter 的包名，插件改名就废）；
	//   - 类型决定取到的值怎么分组（版本标签 / 分支）；
	//   - 顺便把"这个参数根本不存在"当场判出来——Jenkins 对不存在的参数名
	//     返回的是 200 + 空数组，不判就变成界面上一个空的版本号下拉。
	defs, err := client.JobParams(ctx, fullName)
	if err != nil {
		return nil, jenkinsLookupError(err, fullName)
	}
	var target *integration.JenkinsParamDef
	for i := range defs {
		if defs[i].Name == param {
			target = &defs[i]
			break
		}
	}
	if target == nil {
		return nil, errs.NotFoundf("Job %s 上不存在名为 %s 的构建参数", fullName, param)
	}
	if !integration.IsJenkinsGitParam(target.Kind) {
		return nil, errs.Invalidf("参数 %s 的类型是 %s，它的选项是固定的、不需要去仓库读取", param, target.Kind)
	}

	vals, err := client.ParamValues(ctx, fullName, param, target.ClassName, target.Kind)
	if err != nil {
		return nil, upstreamError(err, "读取参数 %s 的可选值失败", param)
	}
	return vals, nil
}

// --------------------------------------------------------------------------- 触发构建

// JenkinsTriggerInput 触发入参。
type JenkinsTriggerInput struct {
	FullName string `json:"fullName" binding:"required,max=512"`
	// Parameters 只传用户**明确选择过**的参数。
	// 不传 → Jenkins 用它自己配置的默认值，与在 Jenkins 页面上直接点 Build 一致；
	// 传一个平台猜的默认值 → 才可能真的构建错版本，而且整个过程不报错。
	Parameters []integration.JenkinsBuildParam `json:"parameters"`
	// IdempotencyKey 可选。重试同一个请求时用它换回同一次构建，避免连点排两次队。
	IdempotencyKey string `json:"idempotencyKey" binding:"omitempty,max=128"`
}

// JenkinsTriggerResultDTO 触发结果。
//
// 注意 QueueID 是**队列项**而不是构建号：Jenkins 接受请求时只给这个，
// 真正的构建号要等执行器空闲才出现，由后台任务补齐并写进构建记录。
//
// BuildNo / Status / Stage 是为**批量提交**补的：批量面板拿到单据后要立刻
// 如实显示"排队中"还是"执行中"，不能一律显示执行中（构建池满时后端其实在排队）。
// 它们描述的是**平台这条构建记录**，不是 Jenkins 侧的状态。
type JenkinsTriggerResultDTO struct {
	QueueID    int                             `json:"queueId"`
	FullName   string                          `json:"fullName"`
	BuildID    uint64                          `json:"buildId"`
	BuildNo    uint32                          `json:"buildNo"`
	Status     string                          `json:"status"`
	Stage      string                          `json:"stage"`
	Message    string                          `json:"message"`
	Parameters []integration.JenkinsBuildParam `json:"parameters"`
}

// TriggerJenkinsBuild 从平台触发一次 Jenkins 构建。
//
// 只允许触发**已绑定平台项目**的 Job。这不是顺手加的限制：平台要靠项目
// 记录这次构建属于谁、镜像该登记到哪、日志与历史挂在谁名下。没有项目就只能
// 建一条无主记录，那还不如当场说清楚。前端也按这个口径把未绑定的按钮置灰了
// （见 JenkinsJobList）——"能勾选"必须等于"后端会受理"。
func (s *CIService) TriggerJenkinsBuild(ctx context.Context, in JenkinsTriggerInput, c Caller) (*JenkinsTriggerResultDTO, error) {
	fullName := strings.TrimSpace(in.FullName)
	if fullName == "" {
		return nil, errs.Invalid("缺少 Jenkins Job 名称")
	}

	client, err := s.jenkinsClient(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.jenkinsBoundProject(ctx, fullName)
	if err != nil {
		return nil, err
	}
	if !p.Enabled {
		return nil, errs.StateConflict("项目已停用，无法触发构建")
	}

	// 幂等：同一个 key 重复提交直接返回已有记录，避免连点排两次队。
	if k := strings.TrimSpace(in.IdempotencyKey); k != "" {
		var exist model.BuildRecord
		ferr := s.db(ctx).Where("idempotency_key = ?", k).First(&exist).Error
		if ferr == nil {
			return &JenkinsTriggerResultDTO{
				FullName: fullName, BuildID: exist.ID,
				BuildNo: exist.BuildNo,
				Status:  exist.Status,
				Stage:   exist.Stage,
				QueueID: int(uint64Value(exist.JenkinsQueueID)),
				Message: fmt.Sprintf("该请求已受理过，返回已有的构建记录 #%d（没有重复排队）", exist.BuildNo),
			}, nil
		}
		if !errors.Is(ferr, gorm.ErrRecordNotFound) {
			return nil, errs.Internalf(ferr, "查询幂等记录失败")
		}
	}

	// 触发前先拍一张快照：nextBuildNumber 就是这一次会拿到的编号。
	// 队列项被 Jenkins 提前清掉时，靠它把属于本次的构建认出来——
	// 没有它就只能按时间猜，而猜错的后果是"日志看着对、其实是上一次的"。
	snapshot, snapErr := client.JobSnapshot(ctx, fullName)
	expectedBuildNo := 0
	switch {
	case errors.Is(snapErr, integration.ErrJobNotFound):
		// 绑定关系过期了（Job 在 Jenkins 上被改名/删除）。**在这里就返回**，
		// 不要往下走去触发：那只会让用户等到一个同样失败的 404，
		// 而且失败发生在"已经排队"之后，更难理解为"绑定失效"。
		s.audit(ctx, c, auditInput{
			Module: "CI", Action: "build:trigger-jenkins", TargetType: "build",
			TargetName: fullName, Detail: "绑定已失效：Jenkins 上不存在该 Job",
			Result: "failed", ErrMsg: snapErr.Error(),
		})
		return nil, notFoundErr(
			"项目 %s 绑定的 Jenkins Job「%s」已不存在（可能已被改名或删除）；请到项目里重新选择 Job",
			p.ProjectKey, fullName)
	case snapErr == nil && snapshot != nil:
		expectedBuildNo = snapshot.NextBuildNumber
	case snapErr != nil:
		// 读快照失败不影响触发本身，只是失去构建号兜底，降级为告警继续。
		s.log("ci.jenkins").Warn("读取 Job 快照失败，将无法用构建号兜底", "job", fullName, "err", snapErr)
	}

	// 先在 Jenkins 上排队，再在平台建记录。顺序是刻意选的：
	// 反过来（先建记录）一旦触发失败，就要留一条"其实什么都没发生"的失败记录；
	// 而先触发、建记录失败时，平台可以把队列项撤掉，做到真正的"要么都成、要么都没发生"。
	queueID, err := client.Trigger(ctx, fullName, in.Parameters)
	if err != nil {
		s.audit(ctx, c, auditInput{
			Module: "CI", Action: "build:trigger-jenkins", TargetType: "build",
			TargetName: fullName, Detail: "触发 Jenkins 构建失败：" + err.Error(),
			Result: "failed", ErrMsg: err.Error(),
		})
		return nil, upstreamError(err, "触发 Jenkins 构建失败")
	}

	params, err := s.jenkinsBranchAndSnapshot(p, fullName, in.Parameters)
	if err != nil {
		_ = client.CancelQueueItem(ctx, queueID)
		return nil, err
	}

	buildNo, err := s.nextBuildNo(ctx, p.ID)
	if err != nil {
		_ = client.CancelQueueItem(ctx, queueID)
		return nil, err
	}

	nowT := now()
	qid := uint64(queueID)
	rec := model.BuildRecord{
		BuildNo:    buildNo,
		ProjectID:  p.ID,
		ProjectKey: p.ProjectKey,
		Branch:     params.Branch,
		// CommitID 刻意留空：这次构建检出的是哪个提交，由 **Jenkins 侧 Job 的配置**决定
		// （它可能检出一个标签、或某个远端分支的最新提交）。平台按 GitLab 查一个
		// 分支 tip 填进来，会得到一个"看起来对、其实不对"的提交号，
		// 事后排查时比空着危险得多。
		CommitMsg:      fmt.Sprintf("由 Jenkins Job %s 构建（检出哪个提交由 Jenkins 侧决定）", fullName),
		TriggerType:    "jenkins",
		TriggerName:    c.Name(),
		ParamsSnapshot: params.Snapshot,
		Status:         "queued",
		Stage:          "已提交 Jenkins，等待执行器",
		Progress:       0,
		JenkinsJob:     fullName,
		JenkinsQueueID: &qid,
		CreatedAt:      nowT,
		UpdatedAt:      nowT,
	}
	if k := strings.TrimSpace(in.IdempotencyKey); k != "" {
		rec.IdempotencyKey = &k
	}
	if uid := c.UserID(); uid != nil && *uid > 0 {
		rec.TriggerBy = uid
	}
	if err := s.db(ctx).Create(&rec).Error; err != nil {
		// 补偿：把已经排上的那次构建撤掉。不撤的话用户会看到
		// "平台报错了，但 Jenkins 上莫名其妙多了一次构建"。
		if cerr := client.CancelQueueItem(ctx, queueID); cerr != nil {
			s.log("ci.jenkins").Error("撤销 Jenkins 队列项失败，请在 Jenkins 上手动确认",
				"queueId", queueID, "job", fullName, "err", cerr)
		}
		return nil, errs.Internalf(err, "创建构建记录失败（已尝试撤销 Jenkins 上的排队，队列项 #%d）", queueID)
	}

	s.audit(ctx, c, auditInput{
		Module: "CI", Action: "build:trigger-jenkins", TargetType: "build",
		TargetID: &rec.ID, TargetName: fmt.Sprintf("%s#%d", p.ProjectKey, rec.BuildNo),
		Detail: fmt.Sprintf("触发 Jenkins 构建：%s / Job %s（队列项 #%d）", p.ProjectKey, fullName, queueID),
	})

	streamKey := BuildLogStreamKey(rec.ID)
	s.deps.Stream.Reset(ctx, streamKey)
	// 先写一行说明：这次构建的日志是从 Jenkins 转写过来的，
	// 不写的话用户会以为平台自己跑了构建、却在平台日志里找不到 docker 的痕迹。
	s.appendJenkinsLog(ctx, streamKey, "info",
		fmt.Sprintf("本次构建由 Jenkins 执行：Job %s（平台构建记录 #%d）", fullName, rec.BuildNo))
	s.appendJenkinsLog(ctx, streamKey, "info",
		"提示：镜像的构建与推送由该 Jenkins Job 自己完成，平台不代为登记镜像版本")
	for _, p2 := range in.Parameters {
		s.appendJenkinsLog(ctx, streamKey, "info", fmt.Sprintf("参数 %s = %s", p2.Name, p2.Value))
	}

	pos, err := s.deps.Builds.Submit(rec.ID, func(taskCtx context.Context) {
		s.runJenkinsBuild(taskCtx, rec.ID, p.ProjectKey, fullName, queueID, expectedBuildNo)
	})
	if err != nil {
		failMsg := "任务提交失败"
		switch {
		case errors.Is(err, queue.ErrDuplicate):
			failMsg = "该构建已在队列中"
		case errors.Is(err, queue.ErrQueueFull):
			failMsg = "构建队列已满，请稍后重试"
		}
		if cerr := client.CancelQueueItem(ctx, queueID); cerr != nil {
			s.log("ci.jenkins").Error("撤销 Jenkins 队列项失败，请在 Jenkins 上手动确认",
				"queueId", queueID, "job", fullName, "err", cerr)
		}
		fin := now()
		_ = s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", rec.ID).
			Updates(map[string]any{"status": "failed", "stage": "提交失败", "error_code": "QUEUE_REJECTED",
				"error_message": failMsg, "finished_at": &fin, "updated_at": fin}).Error
		_ = s.deps.Stream.Finish(ctx, streamKey, "failed")
		if errors.Is(err, queue.ErrQueueFull) {
			return nil, errs.RateLimited(failMsg)
		}
		return nil, errs.StateConflict(failMsg)
	}

	if pos > 0 {
		// 平台调度排队：构建**已经**交给 Jenkins 了，只是平台的构建池满了，
		// 要等一个槽位才能开始转写日志、同步状态。
		//
		// 文案必须点明是"平台调度"：写成「等待执行器」会让人以为 Jenkins 那边还没跑，
		// 而事实往往相反——Jenkins 可能已经在构建，只是平台还没接管同步。
		// 这两件事说反了，用户就会去 Jenkins 页面上怀疑人生。
		rec.Stage = fmt.Sprintf("已提交 Jenkins；平台调度排队中（前面还有 %d 个任务）", pos-1)
		_ = s.deps.Stream.SetStatus(ctx, streamKey, logstream.Status{
			Status: "queued", Progress: 0, Stage: rec.Stage,
		})
		_ = s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", rec.ID).
			Updates(map[string]any{"stage": rec.Stage, "updated_at": now()}).Error
	}

	return &JenkinsTriggerResultDTO{
		QueueID:  queueID,
		FullName: fullName,
		BuildID:  rec.ID,
		BuildNo:  rec.BuildNo,
		// 记录此刻刚建成，状态就是 queued。如实下发，批量面板据此先显示"排队中"，
		// 而不是先显示"执行中"再被 SSE 的状态事件纠正回来（那一下闪烁会让人以为已经开跑）。
		Status:     rec.Status,
		Stage:      rec.Stage,
		Parameters: in.Parameters,
		Message: fmt.Sprintf("已提交 Jenkins：Job %s，队列项 #%d。平台构建记录 #%d 已创建，"+
			"构建号与实时日志会在 Jenkins 开跑后自动同步。", fullName, queueID, rec.BuildNo),
	}, nil
}

// jenkinsBoundProject 找出 Job 绑定的平台项目。
func (s *CIService) jenkinsBoundProject(ctx context.Context, fullName string) (*model.Project, error) {
	var p model.Project
	err := s.db(ctx).Where("jenkins_job = ?", fullName).Order("id").First(&p).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, errs.StateConflict("该 Jenkins Job 还没有绑定平台项目，无法从平台触发：" +
			"请先到「项目管理」把项目的构建方式设为 Jenkins Job 并选中它" +
			"（平台需要项目来登记这次构建的镜像、日志与历史）")
	case err != nil:
		return nil, errs.Internalf(err, "查询 Job 绑定的项目失败")
	}
	return &p, nil
}

type jenkinsBuildSeed struct {
	Branch   string
	Snapshot model.JSONText
}

// jenkinsBranchAndSnapshot 从提交的参数里推出这次构建的"分支"并生成参数快照。
//
// 分支只是为了在构建列表里能一眼看出"这次构建的是哪个版本"，取不到就退回项目默认分支；
// 真正决定检出什么的是 Jenkins 自己，平台不越权解释。
func (s *CIService) jenkinsBranchAndSnapshot(p *model.Project, fullName string,
	params []integration.JenkinsBuildParam) (*jenkinsBuildSeed, error) {

	branch := ""
	for _, param := range params {
		if isJenkinsBranchParam(param.Name) && strings.TrimSpace(param.Value) != "" {
			branch = strings.TrimSpace(param.Value)
			break
		}
	}
	if branch == "" {
		branch = p.DefaultBranch
	}

	snapshot, err := json.Marshal(map[string]any{
		"jenkinsJob": fullName,
		"parameters": params,
	})
	if err != nil {
		return nil, errs.Internalf(err, "序列化构建参数失败")
	}
	return &jenkinsBuildSeed{Branch: branch, Snapshot: model.JSONText(snapshot)}, nil
}

// isJenkinsBranchParam 判断参数名是否表达"用哪个分支/版本"。
//
// 只认几个通用写法，不做模糊匹配：猜错会导致构建列表上的"分支"显示成别的参数值，
// 而这种错看起来完全合理，几乎不会被发现。
func isJenkinsBranchParam(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "git_branch", "branch", "branchname", "ref", "ref_name", "git_ref", "git_tag", "tag", "version":
		return true
	}
	return false
}

// --------------------------------------------------------------------------- 后台同步

// runJenkinsBuild 在任务池中把一次 Jenkins 构建同步到底。
func (s *CIService) runJenkinsBuild(taskCtx context.Context, buildID uint64, projectKey, fullName string,
	queueID, expectedBuildNo int) {

	streamKey := BuildLogStreamKey(buildID)
	log := s.log("ci.jenkins").With("buildId", buildID, "job", fullName)

	timeout := s.deps.Cfg.Runner.BuildTimeout.D()
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(taskCtx, timeout)
	defer cancel()

	started := now()
	if err := s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", buildID).
		Updates(map[string]any{"status": "running", "stage": "等待 Jenkins 执行器",
			"progress": 2, "started_at": &started, "updated_at": started}).Error; err != nil {
		log.Error("更新构建状态失败", "err", err)
		return
	}
	_ = s.deps.Stream.SetStatus(ctx, streamKey, logstream.Status{Status: "running", Progress: 2, Stage: "等待 Jenkins 执行器"})

	// 日志：随打随推（SSE），同时攒一份落库（构建列表的历史日志）。
	var (
		collected []logLineRecord
		logBytes  int64
		maxBytes  = s.deps.Cfg.Runner.MaxLogBytes
		// pushedLines 只攒"出现过镜像推送"的那几行。这次构建到底推了哪个镜像
		// 只能从日志里读出来，而日志落库那份受 MaxLogBytes 限制、可能被截断，
		// 所以单独留一份极小的证据（一个构建里这种行通常也就一两行）。
		pushedLines []string
	)
	sink := func(level, line string) {
		at := now()
		if ref, _ := integration.ParsePushedImage(line, ""); ref != "" {
			pushedLines = append(pushedLines, line)
		}
		s.appendJenkinsLog(ctx, streamKey, level, line)
		if logBytes < maxBytes {
			collected = append(collected, logLineRecord{level: level, line: line, at: at})
			logBytes += int64(len(line))
		}
	}

	// finish 写终态。所有分支都走它，避免各处漏字段导致状态不一致。
	//
	// ⚠️ 收尾上下文必须**在收尾这一刻现开**，不能提前建好复用。
	// 曾经的写法是在任务开头 WithTimeout(20s) 建一次，于是那个 20 秒从任务
	// 开始就算起：构建只要跑满 20 秒（实测触发的那次跑了 63 秒），走到这里
	// 时上下文早已过期，写库 / 落日志 / 封流三件事**全部静默失败**——
	// 界面上表现为"日志停在最后一行、状态卡在构建中"，且不报任何错。
	finish := func(status, stage, code, msg string) {
		fctx, cancel := context.WithTimeout(context.Background(), jenkinsFinishGrace)
		defer cancel()

		fin := now()
		duration := uint32(fin.Sub(started).Seconds())

		// ⚠️ Jenkins 上"构建成功"≠ 平台拿到了产物。
		//
		// 这类 Job 的核心产物就是镜像，而平台**只能从日志里**认出它。认不出来有两种可能：
		// Job 根本没推镜像，或者推了但日志里没有可识别的地址。**两种都不该显示"成功"**——
		// 界面上会留下「成功 + 镜像 —」，项目列表 / 镜像列表 / CD 选版本三处都看不到它，
		// 用户直到要发版、发现一个版本都选不到时才会撞上（实测就是这么撞上的）。
		//
		// 所以：先尝试登记产物，登记不到就把这次构建改判失败，并给出可操作的下一步。
		// 顺序很关键——必须在写终态**之前**改，否则库里会先落一个 success 再改成 failed，
		// 日志里出现两个互相矛盾的终态。
		//
		// 为什么不再要求 `len(pushedLines) > 0`：以前那条件是"没有候选就不白跑一次解析"的
		// 优化，而现在"解析不出结果"本身就是要判定的结论，必须每次都试。
		if status == "success" {
			if !s.recordJenkinsImage(fctx, buildID, projectKey, strings.Join(pushedLines, "\n"), log) {
				status = "failed"
				stage = "未识别到镜像产物"
				code = "JENKINS_NO_IMAGE"
				msg = jenkinsNoImageMessage
			}
		}

		pct := uint8(0)
		if status == "success" {
			pct = 100
		}
		s.finishBuild(fctx, buildID, status, stage, pct, duration, code, msg, nil, fin)
		s.persistBuildLogs(fctx, buildID, collected)
		_ = s.deps.Stream.Finish(fctx, streamKey, status)
	}

	client, err := s.jenkinsClient(ctx)
	if err != nil {
		sink("error", "无法连接 Jenkins："+err.Error())
		finish("failed", "已失败", "JENKINS_UNAVAILABLE", err.Error())
		log.Error("解析 Jenkins 配置失败", "err", err)
		return
	}

	// ① 队列项 → 构建号
	buildNo, ok := s.waitJenkinsBuildNo(ctx, client, fullName, queueID, expectedBuildNo,
		started, sink, log)
	if !ok {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			sink("error", fmt.Sprintf("等待 Jenkins 执行器超时（超过 %s），平台停止跟踪", timeout))
			finish("failed", "超时", "JENKINS_QUEUE_TIMEOUT", "等待 Jenkins 执行器超时")
		case errors.Is(taskCtx.Err(), context.Canceled):
			// 用户取消：撤掉 Jenkins 上排的队，否则平台显示"已取消"、
			// Jenkins 那边还在跑，两边说法不一致。
			if err := client.CancelQueueItem(ctx, queueID); err != nil {
				log.Warn("撤销 Jenkins 队列项失败", "queueId", queueID, "err", err)
			}
			sink("warn", "构建已取消")
			finish("canceled", "已取消", "", "")
		default:
			sink("error", "未能从 Jenkins 取到构建号")
			finish("failed", "已失败", "JENKINS_QUEUE_LOST", "未能从 Jenkins 取到构建号")
		}
		return
	}

	// ② 构建号落库 + 给出 Jenkins 上这次构建的直达地址。
	//    地址必须能打开，所以用平台配置的地址而不是 Jenkins 自报的 rootUrl
	//    （实测目标 Jenkins 报的是内网 IP，点开是打不开的）。
	buildURL := client.BuildURL(fullName, buildNo)
	if err := s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", buildID).
		Updates(map[string]any{"jenkins_build_no": buildNo, "log_url": buildURL,
			"stage": "构建中", "progress": 5, "updated_at": now()}).Error; err != nil {
		log.Warn("回写 Jenkins 构建号失败", "err", err)
	}
	_ = s.deps.Stream.SetStatus(ctx, streamKey, logstream.Status{Status: "running", Progress: 5, Stage: "构建中"})
	sink("info", fmt.Sprintf("Jenkins 已开始执行：构建 #%d", buildNo))
	if buildURL != "" {
		sink("info", "原始日志："+buildURL)
	}

	// ③ 边拉日志边等终态
	var (
		logOffset int64
		partial   string // 跨轮次的半行缓冲：一次拉到的最后一段可能不是完整行
		lastPct   uint8  = 5
	)

	pullLogs := func() {
		text, next, _, found, lerr := client.BuildLog(ctx, fullName, buildNo, logOffset)
		if lerr != nil {
			if ctx.Err() == nil {
				log.Warn("拉取 Jenkins 日志失败", "err", lerr)
			}
			return
		}
		if !found {
			return
		}
		logOffset = next
		partial += text
		lines := strings.Split(partial, "\n")
		partial = lines[len(lines)-1]
		for _, ln := range lines[:len(lines)-1] {
			sink(jenkinsLogLevel(ln), ln)
		}
	}

	for {
		pullLogs()

		st, serr := client.BuildStatus(ctx, fullName, buildNo)
		if serr != nil {
			if ctx.Err() == nil {
				log.Warn("查询 Jenkins 构建状态失败", "err", serr)
			}
		} else if !st.Building {
			// 结束：再拉一次日志把尾巴收干净，然后按 Jenkins 的 result 落终态。
			pullLogs()
			if partial != "" {
				sink(jenkinsLogLevel(partial), partial)
				partial = ""
			}
			result := strPtrValue(st.Result)
			status, stage, code := jenkinsStatusView(result)
			sink(jenkinsLogLevelForStatus(status),
				fmt.Sprintf("Jenkins 构建 #%d 结束：result=%s", buildNo, orDash(result)))
			finish(status, stage, code, fmt.Sprintf("Jenkins 构建 result=%s", orDash(result)))
			log.Info("Jenkins 构建结束", "result", result, "status", status)
			return
		} else {
			// 进度只用比 estimate 估出来的数字，且**最多到 95%**：
			// Jenkins 的预估经常偏离很远，报 100% 会变成"明明在跑却显示已完成"。
			pct := uint8(50)
			if st.EstimatedDuration > 0 {
				elapsed := time.Since(started).Milliseconds()
				pct = uint8(min(int64(95), elapsed*100/max(int64(1), st.EstimatedDuration)))
				if pct < 5 {
					pct = 5
				}
			}
			if pct != lastPct {
				lastPct = pct
				_ = s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", buildID).
					Updates(map[string]any{"progress": pct, "stage": "构建中", "updated_at": now()}).Error
				_ = s.deps.Stream.SetStatus(ctx, streamKey,
					logstream.Status{Status: "running", Progress: pct, Stage: "构建中"})
			}
		}

		select {
		case <-ctx.Done():
			// 收尾：把已拿到的日志落库，并如实写出中止原因。
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				sink("error", fmt.Sprintf("构建超时（超过 %s），平台停止跟踪（Jenkins 上的构建仍在继续）", timeout))
				finish("failed", "超时", "TIMEOUT", "Jenkins 构建超时")
			} else {
				// 用户点了取消：同时停掉 Jenkins 上的构建，不然两边说法不一致。
				if err := client.StopBuild(context.WithoutCancel(taskCtx), fullName, buildNo); err != nil {
					log.Warn("中止 Jenkins 构建失败，请在 Jenkins 上手动确认", "err", err)
				}
				sink("warn", "构建已取消，已向 Jenkins 发出中止请求")
				finish("canceled", "已取消", "", "")
			}
			return
		case <-time.After(jenkinsPollInterval):
		}
	}
}

// --------------------------------------------------------------------------- 产物识别

// recordJenkinsImage 把 Jenkins 构建日志里读到的镜像登记成平台的一个镜像版本。
//
// 为什么非做不可：Jenkins 不提供任何接口能问出"这次产出的是哪个镜像"，日志里那句
// `docker push`（或一行完整的镜像地址）是唯一的事实来源。不解析它，这次构建在平台上
// 就只有"成功"两个字——项目列表的「最近镜像版本」、镜像列表、CD 选镜像版本全都看不到它。
//
// digest、镜像大小与架构日志里没有，只能问仓库（见 resolveImageMeta）。
// 问不到就留空（界面显示"—"），也不要填一个看起来合理的假值——
// 那种值会被 CD 当真用到部署上去。
//
// 返回值有两处用途，**不要只看成"统计"**：
//   - 调用方（finish）据它决定这次构建是 success 还是 failed ——
//     认不出产物就不能算成功，否则界面会出现「成功 + 镜像 —」；
//   - 启动补偿 BackfillJenkinsImages 据它统计补登记了多少条。
func (s *CIService) recordJenkinsImage(ctx context.Context, buildID uint64, projectKey, pushLog string, log *slog.Logger) bool {
	var rec model.BuildRecord
	if err := s.db(ctx).First(&rec, buildID).Error; err != nil {
		log.Warn("读取构建记录失败，跳过镜像登记", "err", err)
		return false
	}

	// 项目仓库名用来在多个 push 里挑出真正属于它的那个（Job 中途可能推基础镜像/缓存）。
	// 取不到就不挑，退回"最后一个候选"——不能因为项目配置有问题就什么都不登记。
	wantRepo := ""
	var p model.Project
	switch err := s.db(ctx).Where("project_key = ?", projectKey).First(&p).Error; {
	case err == nil:
		wantRepo = p.ImageName
	case !errors.Is(err, gorm.ErrRecordNotFound):
		log.Warn("读取项目失败，将不做仓库名匹配", "err", err)
	}

	ref, tag := integration.ParsePushedImage(pushLog, wantRepo)
	if ref == "" {
		// 认不出产物。两个调用方都据此做判断，所以这里只说"没登记"，不下结论：
		//   - runJenkinsBuild 会把它变成一次失败（界面上不能留「成功 + 镜像 —」）；
		//   - BackfillJenkinsImages 只是补登记，历史构建不会被改判。
		log.Warn("日志里没有可识别的镜像推送，未登记镜像版本", "project", projectKey)
		return false
	}

	registry, repo, _ := splitImageRef(ref)
	size := uint64(0)
	pushedAt := now()
	if rec.FinishedAt != nil {
		pushedAt = *rec.FinishedAt
	}

	// digest / 大小 / 架构日志里读不到，去仓库问一次。
	// 这一步是"锦上添花"：问不到就留空，不影响登记本身，所以失败只记 warn。
	digest, arch := "", ""
	if meta, err := s.resolveImageMeta(ctx, registry, repo, tag); err != nil {
		log.Warn("读仓库元数据失败，digest/大小留空", "image", ref, "err", err)
	} else if meta != nil {
		digest, arch = meta.Digest, meta.Arch
		if meta.SizeBytes > 0 {
			size = meta.SizeBytes
		}
	}

	img := model.ImageRecord{
		ProjectID:  rec.ProjectID,
		ProjectKey: projectKey,
		// registry / repo / tag 全部取**日志里那串地址**，而不是项目配置：
		// 日志是实际推送的结果，项目上配的可能带 scheme 或写错，实际推的才算数。
		Registry:    registry,
		Repo:        repo,
		Tag:         tag,
		Image:       ref,
		Digest:      digest,
		SizeBytes:   &size,
		Arch:        arch,
		Branch:      rec.Branch,
		CommitID:    rec.CommitID,
		BuildID:     buildID,
		BuildNo:     rec.BuildNo,
		PushedAt:    pushedAt,
		PullCount:   0,
		DeployedEnv: model.JSONText(`[]`),
	}
	saved, err := s.upsertImageRecord(ctx, img)
	if err != nil {
		log.Warn("登记 Jenkins 构建产出的镜像失败", "image", ref, "err", err)
		return false
	}
	// 挂到这次构建上。构建列表与项目列表的「最近镜像版本」都是按 build_id 查的，
	// 少了这一步就会出现"镜像列表里有、构建记录上却是空的"。
	if err := s.db(ctx).Model(&model.BuildRecord{}).Where("id = ?", buildID).
		Update("image_record_id", saved.ID).Error; err != nil {
		log.Warn("回写构建的镜像记录失败", "imageId", saved.ID, "err", err)
	}
	log.Info("已从日志识别出本次构建的镜像版本", "image", ref, "tag", tag)
	return true
}

// BackfillJenkinsImages 为"平台还没登记镜像版本"的历史 Jenkins 构建补登记。
//
// 为什么需要：解析发生在构建收尾那一刻，所以这个能力上线之前跑完的构建
// （以及收尾时进程正好被打断的那些）在平台上都是空的。好在日志已经落库，
// 补登记不需要再连 Jenkins——纯本地读库，代价很小，所以放在启动时跑一次。
//
// 只补 status=success 且记了 jenkins_job 的：失败/取消的构建没有可信产物。
// 幂等：已经有了 image_record_id 的不再进候选，重复启动不会重复登记。
func (s *CIService) BackfillJenkinsImages(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	var builds []model.BuildRecord
	if err := s.db(ctx).
		Where("trigger_type = ? AND status = ? AND jenkins_job <> '' AND image_record_id IS NULL",
			"jenkins", "success").
		Order("id").Limit(limit).Find(&builds).Error; err != nil {
		return 0, errs.Internalf(err, "查询待补登记镜像的 Jenkins 构建失败")
	}
	if len(builds) == 0 {
		return 0, nil
	}

	log := s.log("ci.jenkins.backfill")
	done := 0
	for i := range builds {
		b := &builds[i]
		var rows []model.BuildLog
		if err := s.db(ctx).Where("build_id = ?", b.ID).Order("seq").Find(&rows).Error; err != nil {
			log.Warn("读取构建日志失败", "buildId", b.ID, "err", err)
			continue
		}
		if len(rows) == 0 {
			// 日志没落库（例如收尾时进程被杀）——没有事实来源，不猜。
			continue
		}
		lines := make([]string, 0, len(rows))
		for j := range rows {
			lines = append(lines, rows[j].Content)
		}
		if s.recordJenkinsImage(ctx, b.ID, b.ProjectKey, strings.Join(lines, "\n"), log) {
			done++
		}
	}
	return done, nil
}

// imageResolveTimeout 单次问仓库元数据的超时。
//
// 为什么不用调用方的 ctx 直接问：这个调用发生在构建收尾路径上，
// 而收尾上下文历史上出过"提前过期、整段静默失败"的事故（见 finish 的注释）。
// 剥掉取消信号、只留一个短超时，既不受调用方影响，也不会把收尾拖长。
const imageResolveTimeout = 8 * time.Second

// resolveImageMeta 问镜像仓库要某个 tag 的确定元数据（digest / 大小 / 架构）。
//
// 三重否掉，核心都是"别问错、别硬来"：
//   - 没登记 repo 或 tag：没得问；
//   - 仓库没接真实实现（本地实现谁都不认）：问了也是空；
//   - **registry 不是配置里那一个**：不同仓库里同名 repo 的内容可能完全不同，
//     问错了会把别人的 digest 挂到这次构建上 —— 这种错不报任何错，数据直接变假。
func (s *CIService) resolveImageMeta(ctx context.Context, registry, repo, tag string) (*integration.TagInfo, error) {
	repo, tag = strings.Trim(repo, "/"), strings.TrimSpace(tag)
	if repo == "" || tag == "" {
		return nil, nil
	}
	reg := s.deps.Up.Registry
	if reg == nil || !reg.Accepts(registry) {
		return nil, nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), imageResolveTimeout)
	defer cancel()
	return reg.Resolve(rctx, repo, tag)
}

// EnrichMissingImageMeta 给"已登记 tag、但元数据不全"的镜像记录补仓库元数据。
//
// 与 BackfillJenkinsImages 分开放：那个纯本地读库（不依赖任何外部服务），
// 这个要连仓库。两者都在启动时跑，顺序是先补登记、再补元数据 ——
// 这样上一步刚补出来的记录，下一步顺手把 digest 一起补上。
//
// ⚠️ 筛选条件不能只看 digest 是否为空。这段代码要补的是 **digest / 大小 / 架构** 三个字段，
// 而它们可以各自独立缺失：平台自建构建（含 script 模式）的 digest 是从 `docker push`
// 的输出里直接抓到的，**本来就是有值的**，于是这些行永远进不了候选集、
// size_bytes 一直停在 NULL，界面上一直是"读不到大小"。
// 所以条件放宽成"任一元数据缺失"。
//
// 只处理 registry 与配置里那个仓库匹配的行：其它 registry 上的记录不管是别人的
// 仓库还是脏数据（库里就有一条 registry='https:' 的历史坏数据），都不去问。
func (s *CIService) EnrichMissingImageMeta(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	var rows []model.ImageRecord
	if err := s.db(ctx).
		Where("repo <> '' AND tag <> ''").
		// size_bytes = 0 也算"缺失"：真实的镜像不可能 0 字节（至少有一个 config blob），
		// 库里出现 0 只有一个来源 —— 早期版本"读不到就写 0"留下的痕迹。
		// 不把它一起捞出来，那些行会永远停在 0，界面上显示"0 B"。
		Where("(digest IS NULL OR digest = '' OR size_bytes IS NULL OR size_bytes = 0 OR arch IS NULL OR arch = '')").
		Order("id").Limit(limit).Find(&rows).Error; err != nil {
		return 0, errs.Internalf(err, "查询元数据不全的镜像记录失败")
	}
	if len(rows) == 0 {
		return 0, nil
	}

	log := s.log("ci.image.enrich")
	done := 0
	for i := range rows {
		rec := &rows[i]
		if !s.deps.Up.Registry.Accepts(rec.Registry) {
			// 不是配置里那个仓库：不猜，也不问
			continue
		}
		meta, err := s.resolveImageMeta(ctx, rec.Registry, rec.Repo, rec.Tag)
		if err != nil {
			log.Warn("补镜像元数据失败", "imageId", rec.ID, "image", rec.Image, "err", err)
			continue
		}
		if meta == nil {
			continue
		}
		// 只写真正读到的字段：仓库没给出大小就保留原值，不要用 0 覆盖。
		updates := map[string]any{}
		if meta.Digest != "" {
			updates["digest"] = meta.Digest
		}
		if meta.SizeBytes > 0 {
			updates["size_bytes"] = meta.SizeBytes
		}
		if meta.Arch != "" {
			updates["arch"] = meta.Arch
		}
		if len(updates) == 0 {
			continue
		}
		if err := s.db(ctx).Model(&model.ImageRecord{}).Where("id = ?", rec.ID).Updates(updates).Error; err != nil {
			log.Warn("回写镜像元数据失败", "imageId", rec.ID, "err", err)
			continue
		}
		done++
	}
	return done, nil
}

// waitJenkinsBuildNo 把队列项换成构建号；成功返回 (构建号, true)。
//
// 两条路：
//   - 主路径：轮询 /queue/item/{id}，等它给出 executable.number；
//   - 兜底：队列项消失（老版本 Jenkins 在构建开跑后会移除它）时，
//     用触发前拍下的 nextBuildNumber 去 lastBuild 上认领。
//
// 兜底必须"认得出才算"，不能拿到 lastBuild 就用：认错会把上一次的构建
// 当成这一次，于是日志看着完全正常、其实是别人的构建——这类错误极难发现。
func (s *CIService) waitJenkinsBuildNo(ctx context.Context, client *integration.JenkinsClient,
	fullName string, queueID, expectedBuildNo int, startedAt time.Time,
	sink func(level, line string), log interface{ Warn(string, ...any) }) (int, bool) {

	deadline := time.Now().Add(jenkinsQueueWait)
	lastNotice := time.Now()

	for {
		n, found, cancelled, err := client.QueueItem(ctx, queueID)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				log.Warn("查询 Jenkins 队列项失败", "queueId", queueID, "err", err)
			}
		case cancelled:
			// 别人（或有权限的人）在 Jenkins 上把它取消了。
			sink("warn", "Jenkins 上的队列项已被取消")
			return 0, false
		case n > 0:
			return n, true
		case !found:
			// 队列项没了：老版本 Jenkins 在构建开始后就会移除它。
			// 用构建号兜底认领——
			if snap, serr := client.JobSnapshot(ctx, fullName); serr == nil && snap != nil && snap.LastBuild != nil {
				lb := snap.LastBuild
				claimed := (expectedBuildNo > 0 && lb.Number >= expectedBuildNo) ||
					// 没有快照时退一步：时间上必须晚于本次触发，且与它在同一分钟内，
					// 否则很可能是别的构建，宁可继续等。
					(expectedBuildNo == 0 && lb.Timestamp >= startedAt.UnixMilli()-5000 &&
						lb.Timestamp <= startedAt.Add(jenkinsQueueWait).UnixMilli())
				if claimed {
					sink("info", "队列项已被 Jenkins 回收，按构建号兜底认领")
					return lb.Number, true
				}
			} else if serr != nil && ctx.Err() == nil {
				log.Warn("查询 Job 快照失败", "err", serr)
			}
		}

		if time.Now().After(deadline) {
			return 0, false
		}
		// 每 30 秒说一次"还在等"，免得界面看着像卡住了。
		if time.Since(lastNotice) >= 30*time.Second {
			lastNotice = time.Now()
			sink("info", "仍在等待 Jenkins 分配执行器…")
		}

		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(jenkinsPollInterval):
		}
	}
}

// appendJenkinsLog 推一行日志到平台日志流。推送失败只告警：日志丢一行
// 也远好过因此中断整个构建跟踪。
func (s *CIService) appendJenkinsLog(ctx context.Context, streamKey, level, line string) {
	if _, err := s.deps.Stream.Append(ctx, streamKey, normalLevel(level), line, now()); err != nil &&
		!errors.Is(err, logstream.ErrClosed) {
		s.log("ci.jenkins").Warn("推送 Jenkins 日志失败", "err", err)
	}
}

// --------------------------------------------------------------------------- 小工具

// jenkinsNoImageMessage：Jenkins 构建成功、但平台认不出产物镜像时，判失败的依据。
//
// 必须给出**可操作的下一步**：只写"失败"用户不知道要改什么。平台读日志的规则是
// 两种形态任一即可 —— 有 `docker push <完整地址>` 那一行（`set -x` 回显也算），
// 或者**有一行只放完整镜像地址**。后者不依赖任何 shell 选项，是最稳的做法。
const jenkinsNoImageMessage = "Jenkins 上这次构建执行成功，但平台没能从日志里认出产出的镜像地址，" +
	"因此没有登记任何版本 —— 这样部署时一个版本都选不到，所以本次判为失败。" +
	"请让 Job 在推送完成后把**完整镜像地址单独打印一行**（形如 " +
	"<仓库地址>/<镜像名>:<版本号>，整行只放这一个地址、不要带别的前后缀），" +
	"或保留 `docker push <完整地址>` 那一行（脚本里 set -x 回显出来的也算）。"

// jenkinsStatusView 把 Jenkins 的 result 映射成平台的状态、阶段文案与错误码。
//
// UNSTABLE 归到 **failed** 而不是 success：它表示"构建跑完了但有检查没通过"。
// 对镜像构建来说，这常常意味着产物不可用；归成成功会让问题一路绿灯
// 到部署阶段才炸，而那时已经很难倒推回是构建的问题。
func jenkinsStatusView(result string) (status, stage, code string) {
	switch strings.ToUpper(strings.TrimSpace(result)) {
	case "SUCCESS":
		return "success", "已完成", ""
	case "FAILURE":
		return "failed", "已失败", "JENKINS_FAILURE"
	case "UNSTABLE":
		return "failed", "不稳定（构建完成但有检查未通过）", "JENKINS_UNSTABLE"
	case "ABORTED":
		return "canceled", "已取消", ""
	case "NOT_BUILT":
		return "failed", "未构建（被上游跳过）", "JENKINS_NOT_BUILT"
	case "":
		// 构建结束了却没有 result：Jenkins 在某种异常中止下会这样。
		// 不能当成功——那会让人以为镜像好了。
		return "failed", "已失败", "JENKINS_NO_RESULT"
	default:
		return "failed", "已失败", "JENKINS_" + strings.ToUpper(result)
	}
}

// jenkinsLogLevel 从日志行猜一个展示级别。
//
// Jenkins 的 console 输出没有级别概念，只能靠常见标记猜。猜错的代价只是
// 一行日志的颜色，所以这里只认非常明确的标记，不做模糊匹配。
func jenkinsLogLevel(line string) string {
	u := strings.ToUpper(line)
	switch {
	case strings.Contains(u, "[ERROR]"), strings.Contains(u, "ERROR:"),
		strings.Contains(u, "FAILURE"), strings.Contains(u, "BUILD FAILED"):
		return "error"
	case strings.Contains(u, "[WARNING]"), strings.Contains(u, "WARNING:"):
		return "warn"
	case strings.HasPrefix(strings.TrimSpace(line), "+ "):
		return "cmd"
	}
	return "info"
}

// jenkinsLogLevelForStatus 给终态那一行挑个级别，让它在日志里显眼。
func jenkinsLogLevelForStatus(status string) string {
	if status == "success" {
		return "info"
	}
	if status == "canceled" {
		return "warn"
	}
	return "error"
}

func strPtrValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func uint64Value(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
