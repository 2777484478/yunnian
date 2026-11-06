package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/model"
)

// --------------------------------------------------------------------------- 环境巡检
//
// 为什么单独做一份"巡检"，而不是让模型自己拼 prompt 去问几个工具：
//
//  1. **判读规则必须固定**。内存 93% 是"警告"还是"正常"不能每次由模型发挥 ——
//     同一个环境今天说"略有压力"、明天说"严重告警"，运维就不会再信它。
//     规则写在代码里，模型只负责组织语言。
//  2. **要能存档、能对比**。一次问答看完就没了，而"这个内存从什么时候开始涨的"
//     只能靠历史报告回答。
//  3. **要能脱离对话使用**。不进 AI 页面的人也应该能生成一份报告。
//
// 采集口径（哪些是实时、哪些是平台记录）在报告页脚都写明了 —— 混着看不出来源
// 是运维报告最容易被诟病的地方（"这数哪来的？"）。

// InspectInput 巡检入参。
type InspectInput struct {
	EnvKey string `json:"envKey" binding:"required,max=64"`
	// Hours 活动统计窗口（部署记录），默认 168（7 天）。
	Hours int `json:"hours" binding:"omitempty,min=1,max=720"`
}

// InspectFinding 一条检查结论。
type InspectFinding struct {
	// Level critical / warning / info
	Level string `json:"level"`
	// Kind 机器可读的分类，用于前端图标与"上次 vs 这次"对比
	Kind string `json:"kind"`
	// Title 一行结论
	Title string `json:"title"`
	// Detail 依据（具体数字、哪个节点、哪张证书）
	Detail string `json:"detail"`
	// Action 建议动作，尽量具体到可执行的一步
	Action string `json:"action,omitempty"`
	// Penalty 这一项扣了多少分（0 表示只是提示不扣分）。
	// 必须给出来：健康分不可解释就只是个数字。
	Penalty int `json:"penalty,omitempty"`
}

// InspectResult 一次巡检的结果。
type InspectResult struct {
	ReportID uint64 `json:"reportId"`
	EnvKey   string `json:"envKey"`
	EnvName  string `json:"envName"`
	Score    int    `json:"score"`
	Level    string `json:"level"`
	Summary  string `json:"summary"`
	// Findings 所有发现（含 info 级）
	Findings []InspectFinding `json:"findings"`
	// Stats 概要计数，供前端/AI 直接用，不必自己数
	Stats InspectStats `json:"stats"`
	// HTML 彩色报告正文（自包含）
	HTML    string    `json:"html,omitempty"`
	Size    int64     `json:"size"`
	Created time.Time `json:"createdAt"`
	// Notes 采集过程中的"没读到"说明（集群不可达等）。有它就说明这份报告
	// **不是全量巡检**，不能当作"环境很干净"。
	Notes []string `json:"notes,omitempty"`
}

// InspectStats 报告里要显示的计数。
type InspectStats struct {
	SvcTotal    int `json:"svcTotal"`
	SvcReady    int `json:"svcReady"`
	SvcNotReady int `json:"svcNotReady"`
	// SvcDegraded 平台记录与集群不一致的服务数（**不影响健康分**）。
	SvcDegraded int `json:"svcDegraded"`
	// SvcIdle 期望副本为 0、当前未运行的服务数（**不影响健康分**）。
	SvcIdle      int `json:"svcIdle"`
	Nodes        int `json:"nodes"`
	Deploys      int `json:"deploys"`
	DeployFail   int `json:"deployFail"`
	Certs        int `json:"certs"`
	CertExpiring int `json:"certExpiring"`
}

// svcProbe 一个服务的集群实时状态。
//
// 巡检**只认这个**：平台库里的 status/ready_replicas 是部署时的历史值，
// 2026-11-05 实测 aio-security-gateway 库里 pending 0/1、集群里已经 1/1 运行两天。
// 拿历史值判健康，等于每次都在报一个早就修好的故障。
type svcProbe struct {
	Svc      model.ServiceInstance
	Exists   bool
	Replicas int
	Actual   int
	Ready    int
	// Err 探不到的原因（权限 / 网络 / 超时）。有它就不是"正常"。
	Err string
}

// healthy 集群侧是否真的就绪。探不到一律不算健康。
func (p svcProbe) healthy() bool {
	return p.Err == "" && p.Exists && p.Replicas > 0 && p.Ready >= p.Replicas
}

// platStatus 平台记录里的状态（仅用于展示差异，不参与判定）。
func (p svcProbe) platStatus() string {
	if p.Svc.Status == "" {
		return "—"
	}
	return p.Svc.Status
}

// inspectData 采集到的原始数据（评分与渲染都基于它）。
type inspectData struct {
	Env    *model.Environment
	Window int

	Svcs []model.ServiceInstance
	// Probes 每个服务的**集群实时**探活结果。巡检以它为准，不看平台 status。
	Probes []svcProbe
	// NotReady 集群侧确实没就绪的服务
	NotReady []svcProbe
	// Stale 集群侧其实是好的、但平台记录不一致的服务。
	// 它**不扣分**，只作为提示 —— 平台的 status 是部署时的历史值
	// （只在手动「同步」时才刷新，而且同步并不覆盖 status），拿它判健康必然误报。
	Stale []svcProbe
	// Idle 副本数被设为 0 的服务（Deployment 存在但 spec.replicas=0）。
	//
	// 它**不是故障**：0/0 既不是"起不来"也不是"崩了"，而是"当前没有实例"——
	// 通常是有意停用（省资源、临时下线）。2026-11-05 实测 escdatastorageservice
	// 就是这种，被判成"未就绪 -15"，白白把 77 分的环境压到 62。
	//
	// 但也不能当没看见：一个服务没在跑，用户看 KPI 时会以为 18 个都在服务。
	// 所以单独一档、不扣分、报告里明确写"副本数为 0（未运行）"。
	Idle []svcProbe
	// ProbeFailed 多少个服务没能从集群探到（报告要说明"未探到"，不能当"正常"）
	ProbeFailed int

	Nodes    *integration.ClusterState
	NodesErr string

	Certs    []integration.TLSCertInfo
	CertsErr string

	DeployTotal int
	DeployFail  int

	Notes []string
	Now   time.Time
}

const (
	inspectionDefaultHours = 168
	// 取证上限：一次最多列多少个未就绪服务。报告是给人看的，
	// 50 个红色条目没人会读 —— 超出时明确说"还有 N 个未列出"。
	inspectionMaxListed = 8
	// inspectProbeConcurrency 探活并发度（见 probeServices 的说明）。
	inspectProbeConcurrency = 6
)

// InspectEnvironment 采集 + 评分 + 渲染 + 存档。
func (s *CDService) InspectEnvironment(ctx context.Context, in InspectInput, c Caller) (*InspectResult, error) {
	envKey := strings.TrimSpace(in.EnvKey)
	if envKey == "" {
		return nil, errs.Invalid("缺少 envKey")
	}
	if err := s.AssertEnvVisible(ctx, envKey, c); err != nil {
		return nil, err
	}
	env, err := s.envByKey(ctx, envKey)
	if err != nil {
		return nil, err
	}
	hours := in.Hours
	if hours <= 0 {
		hours = inspectionDefaultHours
	}

	d := s.collectInspection(ctx, env, hours)
	score, level, findings := scoreInspection(d)
	summary := inspectSummary(findings, score, level)
	st := inspectStats(d)
	doc := renderInspectionHTML(d, score, level, summary, findings, st, c)

	rep := model.InspectionReport{
		EnvKey: env.EnvKey, EnvName: env.Name,
		Score: score, Level: level, Summary: trimRunes(summary, 250),
		FindingsJSON:    model.JSONText(mustJSONText(findings)),
		HTML:            doc,
		Size:            int64(len(doc)),
		ClusterReadable: d.NodesErr == "",
		CreatedBy:       c.UserID(),
		CreatedAt:       d.Now,
	}
	if err := s.db(ctx).Create(&rep).Error; err != nil {
		return nil, errs.Internalf(err, "保存巡检报告失败")
	}

	res := &InspectResult{
		ReportID: rep.ID, EnvKey: env.EnvKey, EnvName: env.Name,
		Score: score, Level: level, Summary: summary,
		Findings: findings, Stats: st, Size: rep.Size,
		Created: d.Now, Notes: d.Notes,
	}
	s.audit(ctx, c, auditInput{
		Module: "CD", Action: "inspect:run", TargetType: "environment", TargetName: env.Name,
		Detail: fmt.Sprintf("生成巡检报告 #%d（%s）：%d 分 / %s", rep.ID, env.EnvKey, score, level),
	})
	return res, nil
}

// --------------------------------------------------------------------------- 采集

func (s *CDService) collectInspection(ctx context.Context, env *model.Environment, hours int) inspectData {
	d := inspectData{Env: env, Window: hours, Now: time.Now()}

	var svcs []model.ServiceInstance
	if err := s.db(ctx).Where("env_key = ? AND deleted_at IS NULL", env.EnvKey).
		Order("project_key asc").Find(&svcs).Error; err != nil {
		d.Notes = append(d.Notes, "服务清单读取失败："+err.Error())
	}
	d.Svcs = svcs
	// **全部以集群实时状态为准**，不用平台的 status 判健康。
	//
	// 为什么不看平台标记（2026-11-05 用户明确要求）：`service_instance.status`
	// 是那次部署写下的历史值 —— 只有手动点「同步」才会刷新，而那个同步
	// 只覆盖 replicas / ready_replicas，**从不更新 status**。
	// 实测后果：aio-security-gateway 库里 pending 0/1，集群里 1/1 运行了两天。
	// 拿它判健康等于每次都在报一个早就修好的故障。
	d.Probes = s.probeServices(ctx, env, svcs)
	for _, pr := range d.Probes {
		switch {
		case pr.Err != "":
			// 探不到 ≠ 正常。单独计数，由评分说明"这份报告有盲区"。
			d.ProbeFailed++
		case pr.healthy():
			// 集群里是好的。平台记录若不符，只作提示、**不扣分**。
			if pr.Svc.Status != "running" || pr.Svc.ReadyReplicas != pr.Ready || pr.Svc.Replicas != pr.Replicas {
				d.Stale = append(d.Stale, pr)
			}
		case pr.Exists && pr.Replicas == 0:
			// **期望就是 0 副本**：已缩容/停用，不是"未就绪"。
			// 放在 healthy() 之后判，是因为 Replicas==0 时 healthy() 本来就 false。
			d.Idle = append(d.Idle, pr)
		default:
			d.NotReady = append(d.NotReady, pr)
		}
	}
	if d.ProbeFailed > 0 {
		d.Notes = append(d.Notes, fmt.Sprintf(
			"有 %d 个服务没能从集群探到状态（权限 / 网络 / 超时），它们**未被计入健康判定** —— 不等于正常。",
			d.ProbeFailed))
	}

	// 集群资源（实时）
	st, nerr := s.deps.Up.Cluster.Nodes(ctx, clusterRefOf(env))
	if nerr != nil {
		d.NodesErr = nerr.Error()
		d.Notes = append(d.Notes, "集群资源没读到（权限 / 网络 / 超时），报告里的 CPU/内存水位为空 —— 这不代表资源充足。")
	} else {
		d.Nodes = st
	}

	// 证书（实时）。读不到时**不要**当成"没有证书"。
	//
	// 采集逻辑抽到 collectCerts：一是两边（集群证书 / 业务证书）失败语义不同，
	// 二是这样能单测钉住"两类都查"这条 —— 之前写成"先 Ingress、空才退 API Server"，
	// 结果 Ingress 一配 TLS，集群证书就成了永久盲区。
	certs, certsErr, certNotes := collectCerts(ctx, s.deps.Up.Cluster, env)
	d.Certs, d.CertsErr = certs, certsErr
	d.Notes = append(d.Notes, certNotes...)

	// 部署活动（平台记录）
	since := d.Now.Add(-time.Duration(hours) * time.Hour)
	var total, failed int64
	s.db(ctx).Model(&model.DeployRecord{}).
		Where("env_key = ? AND created_at > ?", env.EnvKey, since).Count(&total)
	s.db(ctx).Model(&model.DeployRecord{}).
		Where("env_key = ? AND created_at > ? AND status <> ?", env.EnvKey, since, "success").Count(&failed)
	d.DeployTotal, d.DeployFail = int(total), int(failed)
	return d
}

// probeServices 并发探活每个服务的集群实时状态。
//
// 为什么并发：18 个服务串行 Describe 要 3~6 秒，巡检本来就慢，没必要再等。
// 并发 6 是折中：够快（约 1 秒），又不会把集群 API 打出一片 429。
func (s *CDService) probeServices(ctx context.Context, env *model.Environment, svcs []model.ServiceInstance) []svcProbe {
	out := make([]svcProbe, len(svcs))
	sem := make(chan struct{}, inspectProbeConcurrency)
	var wg sync.WaitGroup
	for i := range svcs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			p := svcProbe{Svc: svcs[i]}
			name := strings.TrimSpace(svcs[i].WorkloadName)
			if name == "" {
				name = svcs[i].ProjectKey
			}
			st, err := s.deps.Up.Cluster.Describe(ctx, workloadRefOf(env, name))
			if err != nil {
				p.Err = err.Error()
				out[i] = p
				return
			}
			if st != nil {
				p.Exists = st.Exists
				p.Replicas, p.Actual, p.Ready = st.Replicas, st.ActualReplicas, st.ReadyReplicas
			}
			out[i] = p
		}(i)
	}
	wg.Wait()
	return out
}

// collectCerts 采集巡检要看的证书。**两类都查**，且失败语义分开处理。
//
//	① 集群 API Server 自身的证书（必查、主项）
//	   用户 2026-11-06 明确："我要的巡检是集群的证书，又不是业务的域名证书。"
//	   这张过期 = 整个集群连不上，比任何业务域名证书都严重；对应操作是
//	   `curl -k -v https://<server>:6443`（server 取自 kubeconfig）。
//
//	② Ingress 引用的业务域名证书（附加项）
//	   过期只影响某个域名的 HTTPS，换 Secret 即可。
//
// ⚠️ 2026-11-06 之前的实现是"先查 Ingress，结果为空才退到 API Server"，
// 于是 **Ingress 一旦配了 TLS，API Server 证书就永远查不到**：
// aliyun-prd 的报告只显示了已过期 2497 天的业务证书 `aicard`，
// 而真正要紧的 47.110.211.151:6443（2053-12 到期）一次都没查过。
//
// 返回：按剩余天数升序的证书、集群证书的错误（只有这类才算全局失败）、
// 需要写进报告口径说明的附加提示。
func collectCerts(ctx context.Context, cl integration.Cluster, env *model.Environment) ([]integration.TLSCertInfo, string, []string) {
	var certs []integration.TLSCertInfo
	var notes []string
	var certsErr string

	if ap, err := cl.APIServerCert(ctx, clusterRefOf(env)); err != nil {
		certsErr = "集群 API Server 证书没探到：" + err.Error()
	} else if ap == nil {
		certsErr = "集群 API Server 没有返回证书"
	} else {
		certs = append(certs, *ap)
	}

	if ing, err := cl.IngressTLS(ctx, workloadRefOf(env, "")); err != nil {
		// 业务证书读不到不影响集群那一项，只记一句说明。把整个证书节判成
		// "没读到"会让人以为连集群证书也没查。
		notes = append(notes, "业务域名（Ingress）证书没读到："+err.Error())
	} else {
		certs = append(certs, ing...)
	}

	// 按剩余天数升序：评分取第一张（最差），表格也从最紧急的排起。
	sort.SliceStable(certs, func(i, j int) bool { return certs[i].DaysLeft < certs[j].DaysLeft })
	return certs, certsErr, notes
}

// --------------------------------------------------------------------------- 评分

// scoreInspection 健康分。**扣分制、规则少、每一项都能说出理由**。
//
// 刻意不用"加权平均"那类算法：权重调起来没有手感，而扣分制的每一条都能
// 直接翻译成人话（"1 个服务没起来，扣 15"）。运维信不信这个分，取决于
// 他能不能一眼看出分是怎么算出来的。
func scoreInspection(d inspectData) (int, string, []InspectFinding) {
	score := 100
	findings := make([]InspectFinding, 0, 8)

	add := func(f InspectFinding) {
		score -= f.Penalty
		findings = append(findings, f)
	}

	// 1. 未就绪服务：每个 -15，最多 -45（三个以上就"满盘皆输"了，再扣没意义）
	if n := len(d.NotReady); n > 0 {
		pen := n * 15
		if pen > 45 {
			pen = 45
		}
		listed := d.NotReady
		more := 0
		if len(listed) > inspectionMaxListed {
			more = len(listed) - inspectionMaxListed
			listed = listed[:inspectionMaxListed]
		}
		var parts []string
		for _, pr := range listed {
			if !pr.Exists {
				parts = append(parts, pr.Svc.ProjectKey+"（集群中不存在该工作负载）")
				continue
			}
			parts = append(parts, fmt.Sprintf("%s（集群 %d/%d 就绪）", pr.Svc.ProjectKey, pr.Ready, pr.Replicas))
		}
		detail := strings.Join(parts, "；")
		if more > 0 {
			detail += fmt.Sprintf("；另有 %d 个未列出", more)
		}
		add(InspectFinding{
			Level: "critical", Kind: "service_not_ready",
			Title:   fmt.Sprintf("%d 个服务在集群里未就绪", n),
			Detail:  detail,
			Action:  "逐个确认：是镜像没构建成功、配置缺项，还是资源不足。可用 AI 助手问「为什么 X 起不来」拿到容器侧证据。",
			Penalty: pen,
		})
	}

	// 2. 平台记录与集群不一致：**只提示、不扣分**。
	//
	// 用户明确要求"报告只看实际环境"。平台 status 是部署时的历史值，
	// 它不准是平台自己的问题，不该算进服务的健康分里。
	// 但它仍要说 —— 否则用户会以为平台上那些红色标记是准的。
	if n := len(d.Stale); n > 0 {
		var names []string
		for i, pr := range d.Stale {
			if i >= inspectionMaxListed {
				break
			}
			names = append(names, fmt.Sprintf("%s（集群 %d/%d 就绪，平台记为 %s）",
				pr.Svc.ProjectKey, pr.Ready, pr.Replicas, pr.platStatus()))
		}
		findings = append(findings, InspectFinding{
			Level: "info", Kind: "platform_stale",
			Title:  fmt.Sprintf("%d 个服务的平台记录与集群不一致（不影响健康分）", n),
			Detail: strings.Join(names, "；"),
			Action: "服务的实际状态是好的，是平台记录没跟上。" +
				"可在环境详情点「同步」刷新副本数；status 字段目前不会自动更新，属平台已知问题。",
		})
	}

	// 2.05 副本数为 0 的服务：不扣分，但要说出来。
	if n := len(d.Idle); n > 0 {
		var names []string
		for i, pr := range d.Idle {
			if i >= inspectionMaxListed {
				break
			}
			names = append(names, pr.Svc.ProjectKey)
		}
		findings = append(findings, InspectFinding{
			Level: "info", Kind: "scaled_to_zero",
			Title:  fmt.Sprintf("%d 个服务的副本数为 0（当前未运行）", n),
			Detail: strings.Join(names, "、"),
			Action: "这些服务在集群里期望副本就是 0，通常是有意停用，所以**不计入异常、不扣分**。" +
				"如果其中某个本应在跑，去环境详情把副本数调回 1 以上。",
		})
	}

	// 2.1 有服务探不到：这是**报告的盲区**，要扣分提醒
	if n := d.ProbeFailed; n > 0 {
		pen := 5
		if pen > 10 {
			pen = 10
		}
		add(InspectFinding{
			Level: "warning", Kind: "probe_failed",
			Title:   fmt.Sprintf("%d 个服务的状态没能探到", n),
			Detail:  "读集群时这些服务返回错误（权限 / 网络 / 超时），它们未被计入健康判定。",
			Action:  "确认集群凭据是否覆盖该命名空间；这部分服务在这份报告里是盲区，不能当作正常。",
			Penalty: pen,
		})
	}

	// 3. 节点资源：取**最差的那个节点**，不是平均值 —— 平均值会把一台打满的机器藏起来
	if d.Nodes != nil {
		worstMem, worstMemNode := 0.0, ""
		worstCPU, worstCPUNode := 0.0, ""
		for _, n := range d.Nodes.NodeList {
			if !n.HasMetrics {
				continue
			}
			// ⚠️ NodeStat 里 CPUUsage 是「核」、MemUsage 是「字节」，**不是百分比**
			// （百分比只有 ClusterState 顶层的汇总才是）。直接拿来比 75 会把
			// 25GB 当成 25% 用过 —— 这正是"集群看着很闲其实快满了"的来源。
			memPct := 0.0
			if n.MemBytes > 0 {
				memPct = float64(n.MemUsage) / float64(n.MemBytes) * 100
			}
			cpuPct := 0.0
			if n.CPUCores > 0 {
				cpuPct = n.CPUUsage / n.CPUCores * 100
			}
			if memPct > worstMem {
				worstMem, worstMemNode = memPct, n.Name
			}
			if cpuPct > worstCPU {
				worstCPU, worstCPUNode = cpuPct, n.Name
			}
		}
		if worstMem >= 75 {
			pen := 4
			level := "warning"
			switch {
			case worstMem >= 95:
				pen, level = 15, "critical"
			case worstMem >= 85:
				pen, level = 8, "critical"
			}
			add(InspectFinding{
				Level: level, Kind: "node_memory",
				Title: fmt.Sprintf("节点内存使用 %.0f%%", worstMem),
				Detail: fmt.Sprintf("节点 %s 已用 %.1f%%（超过 75%% 即视为有压力）；%d 个节点中最高值。",
					worstMemNode, worstMem, len(d.Nodes.NodeList)),
				Action:  "先看谁在吃内存：kubectl top pods -n " + d.Env.Namespace + " --sort-by=memory。持续高于 90% 会开始 OOMKill 或调度失败。",
				Penalty: pen,
			})
		}
		if worstCPU >= 85 {
			pen := 5
			level := "warning"
			if worstCPU >= 95 {
				pen, level = 10, "critical"
			}
			add(InspectFinding{
				Level: level, Kind: "node_cpu",
				Title:   fmt.Sprintf("节点 CPU 使用 %.0f%%", worstCPU),
				Detail:  fmt.Sprintf("节点 %s 已用 %.1f%%。", worstCPUNode, worstCPU),
				Action:  "确认是否有异常进程或突发流量；CPU 打满会让就绪探针超时，进而触发反复重启。",
				Penalty: pen,
			})
		}
		if !d.Nodes.MetricsAvailable {
			findings = append(findings, InspectFinding{
				Level: "info", Kind: "metrics_missing",
				Title:  "没有读到用量指标",
				Detail: "该集群未安装 metrics-server 或当前凭据无权读取 —— 这不是「资源空闲」，而是「读不到」。",
				Action: "如果要看真实水位，需要给集群装 metrics-server。",
			})
		}
	}

	// 4. 证书：只报最快到期的那张（全部列出来会把报告撑爆）
	if len(d.Certs) > 0 {
		worst := d.Certs[0] // 已按 DaysLeft 升序
		// 标题必须带来源：集群证书和业务域名证书的处置完全不同，
		// 只说"证书已过期"用户没法判断该去动 kube-apiserver 还是改 Ingress 的 Secret。
		scope := certScope(worst)
		switch {
		case worst.DaysLeft < 0:
			add(InspectFinding{
				Level: "critical", Kind: "cert_expired",
				Title:   fmt.Sprintf("%s证书已过期 %d 天", scope, -worst.DaysLeft),
				Detail:  certDetail(worst),
				Action:  certAction(worst, true),
				Penalty: 30,
			})
		case worst.DaysLeft <= 7:
			add(InspectFinding{
				Level: "critical", Kind: "cert_expiring",
				Title:   fmt.Sprintf("%s证书 %d 天后到期", scope, worst.DaysLeft),
				Detail:  certDetail(worst),
				Action:  certAction(worst, false),
				Penalty: 15,
			})
		case worst.DaysLeft <= 30:
			add(InspectFinding{
				Level: "warning", Kind: "cert_expiring",
				Title:   fmt.Sprintf("%s证书 %d 天后到期", scope, worst.DaysLeft),
				Detail:  certDetail(worst),
				Action:  certAction(worst, false),
				Penalty: 8,
			})
		}
		// 读不出来的证书也要报：那是"有但读不到"，不是"没有"
		for _, c := range d.Certs {
			if c.Err == "" {
				continue
			}
			findings = append(findings, InspectFinding{
				Level: "warning", Kind: "cert_unreadable",
				Title:  "有证书读不出来：" + c.Secret,
				Detail: fmt.Sprintf("Ingress %s 引用了它，但读取/解析失败：%s", strings.Join(c.Ingresses, "、"), c.Err),
				Action: "确认 Secret 是否存在、tls.crt 是否为合法 PEM。这类问题不会立刻爆，但续期时一定卡住。",
			})
		}
	}

	// 5. 部署活动：失败率高才提 —— 偶尔一次失败是正常的
	if d.DeployTotal >= 3 {
		failRate := float64(d.DeployFail) / float64(d.DeployTotal)
		if failRate > 0.3 {
			add(InspectFinding{
				Level: "warning", Kind: "deploy_fail_rate",
				Title:   fmt.Sprintf("近 %d 天部署失败率 %.0f%%", d.Window/24, failRate*100),
				Detail:  fmt.Sprintf("%d 次部署里 %d 次未成功。", d.DeployTotal, d.DeployFail),
				Action:  "去部署记录看失败单的共同原因；如果集中在同一个服务，多半是配置或镜像的问题。",
				Penalty: 10,
			})
		}
	}

	if score < 0 {
		score = 0
	}
	return score, scoreLevel(score), findings
}

// scoreLevel 分数分档。90 以上健康，70 以下需要处理。
func scoreLevel(score int) string {
	switch {
	case score >= 90:
		return "healthy"
	case score >= 70:
		return "warning"
	}
	return "critical"
}

// certScope 证书来源的称谓前缀。
//
// 为什么必须在标题里带来源：集群证书和业务域名证书长得一模一样（都是
// "证书 X 天后到期"），但处置动作完全不同 —— 一个要动 kube-apiserver /
// kubeadm，另一个只要换 Ingress 引用的 Secret。不带来源等于让人猜。
func certScope(c integration.TLSCertInfo) string {
	switch c.Kind {
	case "apiserver":
		return "集群 API Server 的"
	case "ingress":
		return "业务域名（Ingress）的"
	}
	return ""
}

// certAction 续期动作。两类证书的处置方式不同，不能共用一句话。
//
// 注意：这里的文案会进 HTML（后端纯文本，不解析 Markdown），不要写 **加粗**。
func certAction(c integration.TLSCertInfo, expired bool) string {
	if c.Kind == "apiserver" {
		base := "这是集群自身的证书，过期后平台、kubectl 与所有控制器都连不上 API Server。" +
			"它不由 Ingress 的 Secret 管理，改业务证书没用；" +
			"续期方式取决于集群搭建方式（kubeadm 集群用 kubeadm certs renew 后重启 kube-apiserver）。"
		if expired {
			return "已经过期：" + base + "必须先恢复 API Server 证书，其它排查都建立在这之上。"
		}
		return base
	}
	if expired {
		return "立刻续期并更新这个 Secret；已过期的 HTTPS 入口在浏览器侧会直接报错。"
	}
	return "安排续期并更新对应 Secret 即可，不需要改 Ingress；若用 cert-manager，确认自动续期没被关掉。"
}

func certDetail(c integration.TLSCertInfo) string {
	hosts := "未标注域名"
	if len(c.Hosts) > 0 {
		hosts = strings.Join(c.Hosts, "、")
	}
	if c.Kind == "apiserver" {
		// API Server 证书过期 = 整个集群连不上，比业务证书更要紧，说清楚
		s := fmt.Sprintf("集群 API Server（%s）的证书，到期 %s", hosts, c.NotAfter.Format("2006-01-02"))
		if c.Issuer != "" {
			s += "，签发者 " + c.Issuer
		}
		return s
	}
	s := fmt.Sprintf("Secret %s（%s），到期 %s", c.Secret, hosts, c.NotAfter.Format("2006-01-02"))
	if c.Issuer != "" {
		s += "，签发者 " + c.Issuer
	}
	return s
}

// inspectSummary 一句话结论，给列表页和 AI 直接用。
func inspectSummary(findings []InspectFinding, score int, level string) string {
	crit, warn := 0, 0
	var firstCrit string
	for _, f := range findings {
		switch f.Level {
		case "critical":
			crit++
			if firstCrit == "" {
				firstCrit = f.Title
			}
		case "warning":
			warn++
		}
	}
	if crit > 0 {
		s := fmt.Sprintf("%d 项需处理：%s", crit, firstCrit)
		if warn > 0 {
			s += fmt.Sprintf("（另有 %d 项告警）", warn)
		}
		return s
	}
	if warn > 0 {
		return fmt.Sprintf("%d 项告警，无严重问题", warn)
	}
	if level == "healthy" {
		return "未发现异常"
	}
	return "巡检完成"
}

func inspectStats(d inspectData) InspectStats {
	st := InspectStats{SvcTotal: len(d.Probes), Deploys: d.DeployTotal, DeployFail: d.DeployFail}
	st.SvcNotReady = len(d.NotReady)
	st.SvcDegraded = len(d.Stale)
	st.SvcIdle = len(d.Idle)
	// 探不到的不算"就绪"（那是盲区）；副本 0 的也不算（它确实没在服务）。
	st.SvcReady = st.SvcTotal - st.SvcNotReady - d.ProbeFailed - st.SvcIdle
	if st.SvcReady < 0 {
		st.SvcReady = 0
	}
	if d.Nodes != nil {
		st.Nodes = d.Nodes.Nodes
	}
	st.Certs = len(d.Certs)
	for _, c := range d.Certs {
		if c.Err == "" && c.DaysLeft <= 30 {
			st.CertExpiring++
		}
	}
	return st
}

// --------------------------------------------------------------------------- 存档查询

// InspectionReportDTO 列表项（不带 HTML，列表不需要几十 KB 的正文）。
type InspectionReportDTO struct {
	ID        string `json:"id"`
	EnvKey    string `json:"envKey"`
	EnvName   string `json:"envName"`
	Score     int    `json:"score"`
	Level     string `json:"level"`
	Summary   string `json:"summary"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"createdAt"`
}

// ListInspectionReports 报告列表。按环境过滤，可选。
func (s *CDService) ListInspectionReports(ctx context.Context, envKey string, c Caller) ([]InspectionReportDTO, error) {
	q := s.db(ctx).Model(&model.InspectionReport{})
	if k := strings.TrimSpace(envKey); k != "" {
		if err := s.AssertEnvVisible(ctx, k, c); err != nil {
			return nil, err
		}
		q = q.Where("env_key = ?", k)
	} else if allowed, restricted := c.VisibleEnvKeys(); restricted {
		// 不带环境过滤时也要收敛到可见范围：否则有 ai:query 的人能翻到
		// 自己看不到的环境的巡检结论（含服务名与资源水位）。
		if len(allowed) == 0 {
			q = q.Where("1 = 0")
		} else {
			q = q.Where("env_key IN ?", allowed)
		}
	}
	var rows []model.InspectionReport
	if err := q.Order("id desc").Limit(100).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询巡检报告失败")
	}
	out := make([]InspectionReportDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, InspectionReportDTO{
			ID: httpx.IDString(r.ID), EnvKey: r.EnvKey, EnvName: r.EnvName,
			Score: r.Score, Level: r.Level, Summary: r.Summary,
			Size: r.Size, CreatedAt: formatTime(r.CreatedAt),
		})
	}
	return out, nil
}

// InspectionReportDetail 单个报告（含 HTML，供详情页与下载）。
func (s *CDService) InspectionReportDetail(ctx context.Context, id uint64, c Caller) (*model.InspectionReport, error) {
	var r model.InspectionReport
	if err := s.db(ctx).First(&r, id).Error; err != nil {
		return nil, errs.NotFoundf("巡检报告不存在")
	}
	if err := s.AssertEnvVisible(ctx, r.EnvKey, c); err != nil {
		return nil, err
	}
	return &r, nil
}

// --------------------------------------------------------------------------- 小工具

func mustJSONText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// trimRunes 按**字符**截断（不是字节）：按字节切会把中文切成半个字，
// 界面上出现乱码比截短更糟。
func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
