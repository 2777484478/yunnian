package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/llm"
	"github.com/2777484478/yunnian/internal/model"
)

// 部署失败归因：模型优先、规则兜底。**这是 CI 侧 CIService.BuildAIDiagnosis 的姊妹实现**，
// 口径（两级来源、如实标注 source、失败降级不报错、模型结论落库缓存）完全一致。
//
// 为什么单独写而不是抽成一个共用函数：**两者的证据完全不同**。
// CI 的证据是构建日志（一份文本）；CD 的证据分成三块 —— 部署上下文、平台部署日志
// （易失）、**集群侧事实**（容器状态 / 退出码 / 崩溃前那一轮日志 / Pod 事件）。
// 抽共用函数就得让调用方先把它压成一段文本，那等于把"证据从哪来"这件事藏起来，
// 而这恰恰是决定结论对不对的地方。共用的部分是下游：提示词骨架、解析、落库形状。

const (
	// deployDiagnosisLogLines 送给模型的平台部署日志行数。
	// 部署日志与构建日志不是一个量级（几十行 vs 上万行），取尾部就够。
	deployDiagnosisLogLines = 60
	// deployEvidenceTimeout 集群侧取证的独立上限。
	//
	// 独立于模型那 45 秒：取证是"读 3 个 Pod 的状态 + 拉两次日志 + 列一轮事件"，
	// 亚秒级就够；给足余量只是为了防止集群抖一下把整次诊断拖没。
	deployEvidenceTimeout = 20 * time.Second
)

// deployModelDiagnosisResult 模型给出的部署失败归因。
type deployModelDiagnosisResult struct {
	Model      string
	Reason     string
	Suggestion string
	Confidence float64
}

// deployModelDiagnosis 走模型通道做一次部署失败归因。
//
// 返回错误表示"模型这次没用上"，由调用方降级 —— 错误原因会原样进 notice，
// 所以这里别包一层"操作失败"这种废话。
func (s *CDService) deployModelDiagnosis(
	ctx context.Context, rec *model.DeployRecord, svc *model.ServiceInstance, env *model.Environment,
) (*deployModelDiagnosisResult, error) {
	if s.sys == nil {
		return nil, errors.New("模型通道未注入")
	}
	client, cfg, err := s.sys.DefaultModelClient(ctx)
	if err != nil {
		return nil, err
	}
	ev := s.deployEvidence(ctx, rec, svc, env)

	cctx, cancel := context.WithTimeout(ctx, modelDiagnosisTimeout)
	defer cancel()

	text, _, err := client.Chat(cctx, deployDiagnosisMessages(rec, svc, env, ev))
	if err != nil {
		return nil, err
	}
	// 解析复用 CI 的那一份：模型不听话的两种典型偏差（包 ```json 围栏、前面加一句
	// "好的，分析如下"）都在里面抹平了，没必要写第二遍。
	d, err := parseModelDiagnosis(text)
	if err != nil {
		return nil, err
	}
	return &deployModelDiagnosisResult{
		Model: cfg.Name, Reason: d.Reason, Suggestion: d.Suggestion, Confidence: d.Confidence,
	}, nil
}

// deployEvidence 三块证据的集合。
type deployEvidence struct {
	// PlatformLog 平台侧部署日志尾部。**可能为空**：日志流是易失的，
	// 平台重启之后历史部署的日志就取不回来了。
	PlatformLog string
	// Cluster 集群侧证据。nil 表示没采集到（未接入集群 / 取证失败），
	// 不是"采集了但没发现异常" —— 两者要分开说。
	Cluster *integration.FailureEvidence
	// Notes 如实记录"哪一块没拿到、为什么"。它会原样进提示词：
	// 模型必须知道证据是不全的，否则会把"没看到"当成"没问题"。
	Notes []string
}

// deployEvidence 采集三块证据。它**永不返回错误**：取证失败本身就是要如实告诉模型的一条事实。
func (s *CDService) deployEvidence(
	ctx context.Context, rec *model.DeployRecord, svc *model.ServiceInstance, env *model.Environment,
) deployEvidence {
	var ev deployEvidence

	ev.PlatformLog = strings.Join(s.deployLogTail(ctx, rec.ID, deployDiagnosisLogLines), "\n")
	if strings.TrimSpace(ev.PlatformLog) == "" {
		ev.Notes = append(ev.Notes,
			"平台侧部署日志已取不到（日志流是易失的，平台重启后历史日志不再保留）")
	}

	if s.deps.Up == nil || s.deps.Up.Cluster == nil {
		ev.Notes = append(ev.Notes, "集群通道未接入，无法采集容器侧证据")
		return ev
	}

	// 取证用**摘掉取消信号**的 ctx + 独立预算：这次诊断本身可能就是因为
	// "进来时的 ctx 已经超时"而触发的，拿那个 ctx 去取证必然立刻失败。
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deployEvidenceTimeout)
	defer cancel()

	found, err := s.deps.Up.Cluster.DiagnoseFailure(ectx, workloadRefOf(env, svc.WorkloadName))
	if err != nil {
		// 读不到就说读不到。绝不留一个空证据让下游理解成"容器侧没有异常"。
		ev.Notes = append(ev.Notes, fmt.Sprintf("集群侧取证失败：%v", err))
		return ev
	}
	ev.Cluster = found
	ev.Notes = append(ev.Notes, found.Notes...)
	return ev
}

// saveDeployModelDiagnosis 把模型结论写回 deploy_record.ai_diagnosis 当缓存。
//
// 只缓存**模型**结论，理由与 CI 侧一致：规则结论是纯函数、重算零成本；
// 模型结论重算要花钱要等，而同一条失败记录的日志与错误码不会再变。
func (s *CDService) saveDeployModelDiagnosis(
	ctx context.Context, deployID uint64, r *deployModelDiagnosisResult,
) error {
	payload, err := json.Marshal(AIDiagnosisDTO{
		Reason: r.Reason, Suggestion: r.Suggestion, Confidence: r.Confidence,
		Source: diagnosisSourceModel, Model: r.Model,
	})
	if err != nil {
		return err
	}
	return s.db(ctx).Model(&model.DeployRecord{}).Where("id = ?", deployID).
		Update("ai_diagnosis", model.JSONText(payload)).Error
}

// deployDiagnosisMessages 组装归因提示词。
func deployDiagnosisMessages(
	rec *model.DeployRecord, svc *model.ServiceInstance, env *model.Environment, ev deployEvidence,
) []llm.Message {
	ctxLines := []string{
		fmt.Sprintf("环境：%s（namespace %s，集群 %s）", env.EnvKey, env.Namespace, env.Cluster),
		fmt.Sprintf("服务：%s（工作负载 %s）", svc.ProjectKey, svc.WorkloadName),
		fmt.Sprintf("本次要发布的镜像：%s", rec.Image),
		fmt.Sprintf("期望副本数 / 更新策略：%d / %s", rec.Replicas, stringOr(rec.Strategy, "未指定")),
		fmt.Sprintf("部署单号：%s", rec.DeployNo),
	}
	if code := strings.TrimSpace(rec.ErrorCode); code != "" {
		ctxLines = append(ctxLines, "平台记录的错误码："+code)
	}
	if rec.DurationSec != nil {
		ctxLines = append(ctxLines, fmt.Sprintf("平台侧耗时：%d 秒", *rec.DurationSec))
	}
	if prev := strings.TrimSpace(svc.CurrentImage); prev != "" && prev != rec.Image {
		// 把变更前的版本给出来，"这次失败是不是由本次发布引入的"才有依据可判。
		ctxLines = append(ctxLines, "该服务变更前在跑的版本："+prev)
	}

	var b strings.Builder
	b.WriteString("【部署上下文】\n")
	b.WriteString(strings.Join(ctxLines, "\n"))

	b.WriteString("\n\n【平台部署日志尾部】\n")
	if strings.TrimSpace(ev.PlatformLog) == "" {
		b.WriteString("（无。日志流是易失的，平台重启后取不回来）\n")
	} else {
		b.WriteString(ev.PlatformLog)
		b.WriteString("\n")
	}

	b.WriteString("\n【集群侧证据】\n")
	if ev.Cluster == nil {
		b.WriteString("（未采集到：集群通道不可用或取证失败，见下面的取证说明）\n")
	} else {
		b.WriteString(renderFailureEvidence(ev.Cluster))
	}

	if len(ev.Notes) > 0 {
		b.WriteString("\n【取证说明（哪些证据缺失、为什么）】\n")
		for _, n := range ev.Notes {
			b.WriteString("· " + n + "\n")
		}
	}

	return []llm.Message{
		{Role: "system", Content: deployDiagnosisSystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// renderFailureEvidence 把集群侧证据渲染成提示词里的文本块。
func renderFailureEvidence(ev *integration.FailureEvidence) string {
	var b strings.Builder
	if len(ev.Pods) == 0 {
		b.WriteString("（没有未就绪的 Pod）\n")
	}
	for _, p := range ev.Pods {
		fmt.Fprintf(&b, "Pod %s：phase=%s，重启 %d 次", p.Name, p.Phase, p.Restarts)
		if p.Container != "" {
			fmt.Fprintf(&b, "，容器=%s", p.Container)
		}
		b.WriteString("\n")
		if p.Reason != "" {
			fmt.Fprintf(&b, "  容器状态：%s", p.Reason)
			if p.ExitCode != 0 {
				fmt.Fprintf(&b, "（退出码 %d）", p.ExitCode)
			}
			b.WriteString("\n")
		}
		if msg := strings.TrimSpace(p.Message); msg != "" {
			fmt.Fprintf(&b, "  kubelet 说明：%s\n", msg)
		}
		if logs := strings.TrimSpace(p.Logs); logs != "" {
			fmt.Fprintf(&b, "  %s的容器日志尾部：\n", evidenceLogsLabel(p.LogsFrom))
			// 逐行缩进：让日志块在一堆上下文中一眼可辨（对模型和人都是）。
			for _, ln := range strings.Split(logs, "\n") {
				b.WriteString("    " + ln + "\n")
			}
		} else {
			b.WriteString("  容器日志：没取到\n")
		}
	}
	if len(ev.Events) > 0 {
		b.WriteString("相关事件（按时间倒序）：\n")
		for _, e := range ev.Events {
			b.WriteString("  " + e + "\n")
		}
	}
	return b.String()
}

// evidenceLogsLabel 说明这段日志取自哪一次实例。必须区分：
// CrashLoopBackOff 时当前实例常常一行都没输出，证据在 previous 那一轮里。
func evidenceLogsLabel(from string) string {
	switch from {
	case "previous":
		return "崩溃前那一轮"
	case "current":
		return "当前这一轮"
	default:
		return "容器"
	}
}

// deployDiagnosisSystemPrompt 部署失败归因提示词。
//
// 与 CI 的 diagnosisSystemPrompt 共享前两条精神（不许编造、要具体到可执行），
// 但从第三条起是 CD 特有的 —— 那三条正是"平台日志什么都说不出来"这个现实逼出来的。
const deployDiagnosisSystemPrompt = `你是 YunNian 云辇平台（公司内部 CI/CD 平台）的**部署失败**归因助手。

任务：依据给出的「部署上下文」「平台部署日志尾部」「集群侧证据」，判断这次部署失败最可能的原因，并给出下一步动作。

规则：
1. 只依据给定内容判断。证据里没出现的镜像 tag、环境变量名、配置文件名一律不要写——编造一个具体到配置项名字的假原因，会让排查方向整体跑偏，比不给结论更浪费时间。
2. **平台日志没写，不等于没发生**。平台只看得到"下发成功 / 等待滚动 / 超时"这几个动作；应用为什么起不来，只在集群侧证据（容器状态、退出码、崩溃前后的日志、Pod 事件）里。不要因为平台日志没有报错就说"没有发现异常"。
3. 分清两类失败，不要混为一谈：
   - **容器起不来**：集群侧证据里有 CrashLoopBackOff / OOMKilled / ImagePullBackOff / CreateContainerConfigError / 明确的退出码，或日志里有具体报错 → 原因要从那里读出来，具体到"缺哪个配置、哪个依赖连不上、什么以什么退出码结束"。
   - **平台等超时**：Pod 没有明确报错、只是慢（镜像拉取久、启动耗时长） → 就说清是慢还是卡，不要硬编一个崩溃原因。
4. 判断"是新版本自身的问题，还是环境/配置的问题"时要给出依据：如果旧版本仍在正常服务，说明集群与依赖整体是好的，问题更可能出在这次发布引入的东西上。
5. 建议要给到可执行的一步（补哪个配置项、改哪个探针参数、回滚到哪一版），不要写"请检查配置"。
6. 如果取证说明里写了某块证据缺失（例如容器日志没取到、平台日志已流失），要在原因里说明这一点，并给出"拿到那块证据后该看什么"。
7. 信息不足以定位时，把 confidence 压到 0.3 以下，并在原因里直说要再看什么。
8. 用中文，用运维能直接照做的说法，不要复述日志原文。

输出格式（只输出这个 JSON 对象，不要代码块、不要任何额外说明）：
{"reason":"失败原因","suggestion":"修复建议","confidence":0.0~1.0 之间的小数}`

// ─────────────────── 部署失败时把集群证据写进日志流 ───────────────────
//
// 与上面的 AI 归因是**两件事**，别混：
//   归因（DeploymentAIDiagnosis）给的是**结论** —— 按需触发、可能走模型、可能落库缓存；
//   这里给的是**原始证据** —— 部署一失败就写，人是自己看。
// 两样都要的理由：结论会降级（模型通道没 Key 时退回错误码规则，那几句是套话），
// 而容器状态与 Pod 事件永远是真的。排查顺序应当是"先看证据，再信结论"。

const (
	// deployEvidenceLogTailLines 崩溃日志在**部署日志流**里最多回放几行。
	//
	// 只给最后 20 行：日志流是给人当场看的，不是归档。实测一次 Java 崩溃的
	// 80 行会在面板里把前面所有内容冲走，而根因（`Caused by:` 那一行）就在
	// 最后几行里。要看完整原文去「AI 诊断」抽屉，那边不截断。
	deployEvidenceLogTailLines = 20
)

// deployLogLine 一条待写入部署日志流的内容。
type deployLogLine struct {
	Level string
	Text  string
}

// appendDeployEvidenceLogs 采集集群侧证据并写进部署日志流。
//
// 为什么要有这一步：平台日志翻来覆去只会说"等待就绪探针"，用户想知道
// "到底为什么起不来"仍然得开终端跑 kubectl get events / describe。把集群侧的
// 原话（BackOff、Unhealthy、镜像拉取结果、容器退出原因）直接搬进失败的那一屏，
// 排查就不必离开平台。
//
// 三条纪律：
//  1. **只读**。取证发生在一次已经失败的发布之后，此时任何写动作都是把事故扩大。
//  2. **取不到不报错，但必须说出来**。写成一行 warn 而不是静默：静默会让人把
//     "我们没看到"读成"集群里没有问题"，那正是这套日志最该避免的误解。
//  3. **独立 ctx**。进来的 ctx 很可能正是超时或被取消的那一个（部署超时是最常见的
//     失败原因之一），拿它去取证必然立刻失败。
func (s *CDService) appendDeployEvidenceLogs(
	ctx context.Context, streamKey string, ref integration.WorkloadRef,
) {
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deployEvidenceTimeout)
	defer cancel()

	found, err := s.deps.Up.Cluster.DiagnoseFailure(ectx, ref)
	if err != nil {
		s.appendDeployLog(ctx, streamKey, "warn", fmt.Sprintf(
			"集群侧证据没取到（%v）—— 这条日志给不出容器侧的原始原因，"+
				"排查请到集群看该工作负载的 Pod 事件与容器日志。", err))
		return
	}
	for _, ln := range deployEvidenceLogLines(found) {
		s.appendDeployLog(ctx, streamKey, ln.Level, ln.Text)
	}
}

// deployEvidenceLogLines 把集群侧证据渲染成**给人看**的日志行。
//
// 与 renderFailureEvidence（喂模型的那份）刻意不共用：读者不同，取舍就不同。
// 人这一份要短、先给结论（哪个 Pod、坏在哪）、日志块截到 20 行；
// 模型那一份要完整铺开 —— 它自己会挑重点，替它截等于替它丢证据。
//
// 顺序照抄人排查时的习惯（对齐 kubectl describe 的心理顺序）：
// 取证说明 → 容器状态 → 事件 → 崩溃日志。
func deployEvidenceLogLines(ev *integration.FailureEvidence) []deployLogLine {
	if ev == nil {
		return []deployLogLine{{"warn", "集群侧取证没有返回任何内容（这本身是异常，请反馈）。"}}
	}
	out := []deployLogLine{{"info", "———— 以下为集群侧证据（只读采集，原文未加工）————"}}

	// 取证说明放最前：它决定下面这些证据能信几分（哪块没拿到、为什么）。
	// 压到末尾一定会被长日志冲掉，而那恰好是最需要被看见的一句。
	for _, n := range ev.Notes {
		out = append(out, deployLogLine{"warn", "取证说明：" + n})
	}

	if len(ev.Pods) == 0 {
		out = append(out, deployLogLine{"info",
			"没有未就绪的 Pod：这次失败更像平台侧等待超时，而不是容器起不来。"})
	}
	for _, p := range ev.Pods {
		head := fmt.Sprintf("Pod %s：phase=%s，重启 %d 次", p.Name, p.Phase, p.Restarts)
		if p.Container != "" {
			head += "，容器 " + p.Container
		}
		out = append(out, deployLogLine{"error", head})
		if p.Reason != "" {
			state := "  容器状态：" + p.Reason
			if p.ExitCode != 0 {
				state += fmt.Sprintf("（退出码 %d）", p.ExitCode)
			}
			out = append(out, deployLogLine{"error", state})
		}
		if msg := strings.TrimSpace(p.Message); msg != "" {
			out = append(out, deployLogLine{"error", "  kubelet 说明：" + msg})
		}
	}
	for _, e := range ev.Events {
		out = append(out, deployLogLine{"warn", "  事件 " + e})
	}
	for _, p := range ev.Pods {
		logs := strings.TrimSpace(p.Logs)
		if logs == "" {
			// 空日志必须说成"没取到"，不能让读的人以为"日志里没有报错"。
			out = append(out, deployLogLine{"warn", fmt.Sprintf(
				"Pod %s 的容器日志没取到 —— 别把这条读成「日志里没有报错」。", p.Name)})
			continue
		}
		lines := strings.Split(logs, "\n")
		cut := len(lines) - deployEvidenceLogTailLines
		if cut < 0 {
			cut = 0
		}
		out = append(out, deployLogLine{"info", fmt.Sprintf(
			"Pod %s 的容器日志（%s，末 %d 行；完整原文见「AI 诊断」）：",
			p.Name, evidenceLogsLabel(p.LogsFrom), len(lines)-cut)})
		for _, ln := range lines[cut:] {
			out = append(out, deployLogLine{"info", "  " + ln})
		}
	}
	return out
}
