package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/llm"
	"github.com/2777484478/yunnian/internal/model"
)

// AIService AI 运维助手（只读 + 只读工具）。
//
// 四条硬约束，与 docs/03 §12 的安全约束对应：
//  1. **只读**：本服务只查平台自己的表与集群的读接口，没有任何写路径。
//     模型想说"我帮你重启了"也做不到——它连那个工具都没有（见 ai_tools.go）。
//  2. **变更靠卡片**：需要动集群时，模型只能输出 attachment(kind=action)，
//     由用户在界面上点，再走正常 REST 接口（带权限校验与审计）。
//  3. **上下文是快照**：每次提问都现查一次事实塞进提示词，而不是让模型"记住"上次的数字。
//     运维场景里过期数字比没有数字更危险。
//  4. **模型可以自己取数**（2026-10-09 起）：除了一份起始快照，模型还拿到一组
//     只读工具，能按需查集群。这一条是"接了模型"与"接了 AI"的分界线 ——
//     用户问「测试区192 的 demouser 状态」时，快照里根本没有这两个实体，
//     只有工具能让它自己把口语对上标识、再去集群读一次实时值。
type AIService struct {
	*base
	// sys 复用系统服务的模型通道解析逻辑（含密钥解密），避免第二份实现。
	sys *SystemService
}

// 一次提问的预算。
const (
	// aiMaxToolRounds 最多走几轮工具调用。
	//
	// 3 轮足够覆盖真实路径：「对标识 → 查状态 →（发现异常）取证」。
	// 不设上限的风险不是花钱，而是**打转**：查不到时模型会换个参数接着查，
	// 把用户晾在那儿。到顶就强制它用手上的信息作答。
	aiMaxToolRounds = 4

	// aiToolTimeout 单个工具的执行上限。
	//
	// 集群读接口在凭据失效或集群不可达时会一直等（kubectl 默认重试到超时），
	// 不设限会让整条对话卡死在一个工具上。
	aiToolTimeout = 25 * time.Second
)

// ChatContext 提问时携带的页面上下文。
//
// 有了它，用户在"环境详情页"点问一句"为什么这个服务起不来"就不必再把环境名打一遍。
type ChatContext struct {
	EnvKey     string `json:"envKey"`
	ProjectKey string `json:"projectKey"`
}

// ChatInput 提问入参。
type ChatInput struct {
	ConversationID string       `json:"conversationId" binding:"omitempty,max=64"`
	Message        string       `json:"message" binding:"required,min=1,max=2000"`
	Context        *ChatContext `json:"context"`
}

// ChatAttachment SSE 里下发的结构化卡片。
//
// kind=kv 用于"事实展示"，kind=action 用于"可执行操作"——
// action 只是**建议**，真正的调用由前端在用户点击后发起。
type ChatAttachment struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Data  any    `json:"data"`
}

// ChatSession 一次问答的准备结果。
//
// 把「准备」与「流式输出」拆成两步，是为了让 HTTP 层能在**开流之前**
// 发现"模型通道不可用"这类问题并以正常错误码返回——一旦开始写 SSE，
// HTTP 状态码就锁死在 200 了，用户只能看到一个 200 加一条错误事件。
type ChatSession struct {
	ConversationID string
	// convPK 会话主键。0 表示这次提问没有归属会话（未登录等异常路径），
	// 此时不落库。对外只给 ConversationID（conv_key），主键不外泄。
	convPK uint64
	// Attachments 本轮起始事实的结构化卡片，落库时挂在这一轮的回答上。
	Attachments []ChatAttachment

	client   *llm.Client
	messages []llm.Message
	// tools 只读工具的声明。模型据此决定要不要自己取数。
	tools []llm.Tool
	// toolTraces 本轮**真正执行过**的工具调用留痕（一轮里可能查好几个）。
	//
	// 只用于落库回放，**不参与 prompt**：工具返回值已经作为 tool 消息进过
	// messages 了，再塞一遍就是重复上下文。
	toolTraces []AIToolResult
	model      string
}

// Prepare 校验模型通道、聚合起始事实、拼装提示词与工具声明。
//
// c 用来定位会话归属（会话是私有数据，见 ai_conversation.go 的文件头）。
func (a *AIService) Prepare(ctx context.Context, in ChatInput, c Caller) (*ChatSession, error) {
	question := strings.TrimSpace(in.Message)
	if question == "" {
		return nil, errs.Invalid("提问内容不能为空")
	}

	client, cfg, err := a.sys.DefaultModelClient(ctx)
	if err != nil {
		return nil, err
	}

	// 会话：传了 key 就续上那段对话（并校验归属），没传就现建一个。
	// 建在"第一句提问"这一刻，而不是打开页面时 —— 否则用户进来又离开会攒一堆空会话。
	conv, err := a.ensureConversation(ctx, c, in.ConversationID, question)
	if err != nil {
		return nil, err
	}
	history, err := a.loadHistory(ctx, conv.ID)
	if err != nil {
		return nil, err
	}

	facts, attachments := a.collectFacts(ctx, in.Context)

	msgs := []llm.Message{
		{Role: "system", Content: aiSystemPrompt},
	}
	// 历史在系统提示之后、事实快照之前。
	//
	// ⚠️ **事实快照必须紧贴当前问题**：它描述的是"此刻集群是什么样"，
	// 若把它排在历史前面，模型会把它读成"很久以前的一条记录"，
	// 转而用历史里那段更靠近问题的旧结论来回答 —— 而旧结论往往已经过期。
	if len(history) > 0 {
		msgs = append(msgs, history...)
	}
	if len(facts) > 0 {
		msgs = append(msgs, llm.Message{
			Role: "system",
			Content: "以下是平台**此刻**的只读事实快照（发起本次提问时查询所得）。" +
				"它比上文历史里的任何结论都新，两者冲突时以它为准。" +
				"它只是**起点**：快照里没有的数据去调工具查，不要因为快照里没有就说「查不到」。\n" +
				strings.Join(facts, "\n"),
		})
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: question})

	return &ChatSession{
		ConversationID: conv.ConvKey,
		convPK:         conv.ID,
		Attachments:    attachments,
		client:         client,
		messages:       msgs,
		tools:          a.aiToolDecls(),
		model:          cfg.Name,
	}, nil
}

// Run 跑一轮问答，必要时由模型自己调用只读工具取数。
//
// 循环：流式请求 → 若模型要求调用工具就执行并回灌 → 再请求 → 直到它给出正文。
// 正文边收边推（onDelta），工具执行前推一条 onTool 让界面能显示"正在查询集群"——
// 否则用户在工具执行的那几秒里看到的是一片空白，像是卡住了。
//
// 返回累积正文与 token 用量。
func (a *AIService) Run(
	ctx context.Context, sess *ChatSession,
	onDelta func(string) error, onTool func(AIToolResult) error,
) (string, int, error) {
	if sess == nil || sess.client == nil {
		return "", 0, errs.Internal(errors.New("会话未初始化"))
	}

	var text strings.Builder
	emit := func(delta string) error {
		text.WriteString(delta)
		if onDelta != nil {
			return onDelta(delta)
		}
		return nil
	}

	total := 0
	for round := 0; round < aiMaxToolRounds; round++ {
		step, err := sess.client.ChatStreamStep(ctx, sess.messages, sess.tools, emit)
		total += step.Usage.TotalTokens
		if err != nil {
			return text.String(), total, upstreamError(err, "模型通道 %s 调用失败", sess.model)
		}
		if len(step.ToolCalls) == 0 {
			// 没有工具调用 = 这一轮就是最终答复，正文已经流完了。
			return text.String(), total, nil
		}

		// 把这一轮的 assistant 消息（含 tool_calls）记进历史，再逐个执行回灌。
		// 顺序不能乱：消息列表里 assistant(tool_calls) 必须紧跟与它配对的
		// tool 消息，否则上游会判成非法历史直接 400。
		sess.messages = append(sess.messages, llm.Message{
			Role: "assistant", Content: step.Content, ToolCalls: step.ToolCalls,
		})
		for _, tc := range step.ToolCalls {
			res := a.runTool(ctx, tc)
			// 留痕：回看历史时要把"当时查了什么工具、拿到了什么"**原样重放**。
			// 只存一段正文的话，用户翻记录时最想看的那部分恰好没有 ——
			// 他翻记录多半就是想看"当时到底查了哪几个服务、集群回了什么"。
			sess.toolTraces = append(sess.toolTraces, res)
			if onTool != nil {
				if err := onTool(res); err != nil {
					return text.String(), total, err
				}
			}
			sess.messages = append(sess.messages, llm.Message{
				Role: "tool", ToolCallID: tc.ID, Content: res.Result,
			})
		}
	}

	// 轮次用尽。再发一轮**不带工具**的请求，逼它用手上已有的信息作答 ——
	// 否则模型会继续要工具，而我们已经不打算再给了，对话就停在一半。
	step, err := sess.client.ChatStreamStep(ctx, sess.messages, nil, emit)
	total += step.Usage.TotalTokens
	if err != nil {
		return text.String(), total, upstreamError(err, "模型通道 %s 调用失败", sess.model)
	}
	return text.String(), total, nil
}

// runTool 执行一次工具调用（带超时）。
func (a *AIService) runTool(ctx context.Context, tc llm.ToolCall) AIToolResult {
	tctx, cancel := context.WithTimeout(ctx, aiToolTimeout)
	defer cancel()
	return a.callTool(tctx, tc.Function.Name, tc.Function.Arguments)
}

// --------------------------------------------------------------------------- 起始事实

// aiSystemPrompt 系统提示词。
//
// 三条要点，每条都是被真实事故定出来的：
//
//   - 「先查再答」：不写这句，模型拿到一份不相关的快照就会答"当前上下文没有这项数据"，
//     明明手上有工具却不用 —— 用户看到的就是"这 AI 不太对劲"。工具的存在必须在
//     提示词里**显式**告知，模型不会自己去猜一组 function 之外的用法。
//   - 「口语要对标识」：用户说"测试区192"，库里是 env_key=test。这层映射只能靠
//     名录 + list_environments 完成，不能让模型猜（猜错就是查到别人的数据）。
//   - 「不能执行变更」：模型无法区分"用户希望我动手"与"用户只是问一句"，
//     而它输出的任何"已为你重启"都会被当成事实。把边界写进提示词，
//     再配合工具层根本没有写接口，两道闸一起才靠谱。
const aiSystemPrompt = `你是 YunNian 云辇平台的运维助手，服务于公司内部的 CI/CD 平台。

你可以调用一组**只读**工具自己去查数据。规则：

1. 只读。你没有任何执行变更的能力，不要声称"已部署 / 已重启 / 已回滚"。
2. **先查再答。** 涉及某个环境、服务、构建、部署的具体状态时，必须先调用工具拿到数据再回答。
   不要因为「起始快照」里没有就直接说"当前上下文没有这项数据"——
   至少先调 list_environments / list_projects 把用户说的名字对上标识，再往下查。
3. 用户用口语描述环境或服务（"测试区192"、"正式区"、"用户服务"）时，
   用工具把它们对应到准确的 env_key / project_key，**不要凭猜测填标识**。
   起始快照里的名录可以直接用来对照。
4. 集群的实时状态（副本数、是否健康、为什么起不来）必须用 get_service_status /
   diagnose_service 查，不要用平台数据库里可能滞后的登记值代替。
5. 工具返回 ok=false 时，那是"这次没查到"，不是"没有问题"。如实把它转述给用户，
   并说明是权限、网络还是标识不对。**绝不能**把查不到说成"一切正常"。
   同理，metricsAvailable=false 表示没读到用量指标，不能说成"资源空闲"。
6. 回答里出现时间、版本、副本数这类关键信息时，原样引用工具返回值（含镜像 tag）。
7. 关于故障：按「现象 → 依据（引用具体的 Pod 状态 / 事件 / 日志行）→ 建议排查动作」组织，
   建议要具体到可执行的一步。证据不足时说清楚还缺什么，不要编造原因。
8. 若用户明显想做变更操作（发布、回滚、扩容），说明平台里对应的入口，并提醒需要相应权限。
9. 回答用中文，分点陈述，不要长篇铺垫。`

// collectFacts 采集起始只读事实。
//
// 无论问什么都带上平台概览与名录：概览提供对照（整体是不是都在失败），
// 名录提供"口语 → 标识"的对照表。这两样都不大，但能让绝大多数提问
// 少花一轮工具调用 —— 少一轮就是少几秒等待。
func (a *AIService) collectFacts(ctx context.Context, in *ChatContext) ([]string, []ChatAttachment) {
	var facts []string
	var attachments []ChatAttachment

	facts = append(facts, a.platformOverview(ctx)...)
	facts = append(facts, a.roster(ctx)...)

	envKey := ""
	projectKey := ""
	if in != nil {
		envKey = strings.TrimSpace(in.EnvKey)
		projectKey = strings.TrimSpace(in.ProjectKey)
	}

	if projectKey != "" {
		lines, att := a.projectFacts(ctx, projectKey)
		facts = append(facts, lines...)
		if att != nil {
			attachments = append(attachments, *att)
		}
	}
	if envKey != "" {
		lines, att := a.envFacts(ctx, envKey, projectKey)
		facts = append(facts, lines...)
		if att != nil {
			attachments = append(attachments, *att)
		}
	}
	return facts, attachments
}

// platformOverview 平台概览。
func (a *AIService) platformOverview(ctx context.Context) []string {
	db := a.db(ctx)
	var projectTotal, projectEnabled int64
	_ = db.Model(&model.Project{}).Count(&projectTotal).Error
	_ = db.Model(&model.Project{}).Where("enabled = ?", true).Count(&projectEnabled).Error

	var envTotal int64
	_ = db.Model(&model.Environment{}).Count(&envTotal).Error

	since := time.Now().Add(-24 * time.Hour)
	type row struct {
		Status string
		N      int64
	}
	var rows []row
	_ = db.Model(&model.BuildRecord{}).
		Select("status, count(*) as n").
		Where("created_at >= ?", since).
		Group("status").Scan(&rows).Error

	byStatus := make(map[string]int64, len(rows))
	for _, r := range rows {
		byStatus[r.Status] = r.N
	}
	var runningSvc int64
	_ = db.Model(&model.ServiceInstance{}).Where("status <> ?", "running").Count(&runningSvc).Error

	var pendingApprove int64
	_ = db.Model(&model.DeployRecord{}).Where("status = ?", "pending_approval").Count(&pendingApprove).Error

	return []string{
		fmt.Sprintf("【平台概览】项目 %d 个（启用 %d），环境 %d 个。",
			projectTotal, projectEnabled, envTotal),
		fmt.Sprintf("【近 24 小时构建】总计 %d：成功 %d，失败 %d，进行中/排队 %d。",
			byStatus["success"]+byStatus["failed"]+byStatus["canceled"]+byStatus["running"]+byStatus["queued"],
			byStatus["success"], byStatus["failed"], byStatus["running"]+byStatus["queued"]),
		fmt.Sprintf("【服务实例登记】状态非 running 的 %d 个；待审批部署单 %d 个。（登记值，可能滞后于集群）",
			runningSvc, pendingApprove),
	}
}

// roster 名录：口语 → 标识 的对照表。
//
// 为什么塞进起始快照而不是全靠工具：用户嘴里的"测试区192"与库里的 env_key=test
// 之间**没有任何字符串关系**，只靠 list_environments 也能对上，但那要多花一轮往返。
// 名录很小（环境个位数、项目几十条），一次给全比多一轮便宜得多。
func (a *AIService) roster(ctx context.Context) []string {
	db := a.db(ctx)
	var envs []model.Environment
	_ = db.Order("stage_order asc, id asc").Limit(50).Find(&envs).Error
	envParts := make([]string, 0, len(envs))
	for _, e := range envs {
		envParts = append(envParts, fmt.Sprintf("%s=%s", e.EnvKey, e.Name))
	}

	var projects []model.Project
	_ = db.Order("project_key asc").Limit(80).Find(&projects).Error
	projParts := make([]string, 0, len(projects))
	for _, p := range projects {
		projParts = append(projParts, fmt.Sprintf("%s=%s", p.ProjectKey, p.Name))
	}

	out := []string{}
	if len(envParts) > 0 {
		out = append(out, "【环境名录】env_key=名称："+strings.Join(envParts, "；"))
	}
	if len(projParts) > 0 {
		out = append(out, "【项目名录】project_key=名称："+strings.Join(projParts, "；"))
	}
	return out
}

// projectFacts 项目相关事实。
func (a *AIService) projectFacts(ctx context.Context, projectKey string) ([]string, *ChatAttachment) {
	db := a.db(ctx)
	var p model.Project
	if err := db.Where("project_key = ?", projectKey).First(&p).Error; err != nil {
		return []string{fmt.Sprintf("【项目】未找到项目 %s。", projectKey)}, nil
	}

	var builds []model.BuildRecord
	_ = db.Where("project_key = ?", projectKey).Order("id desc").Limit(5).Find(&builds).Error

	lines := []string{
		fmt.Sprintf("【项目】%s（%s），类型 %s，负责人团队 %s，默认分支 %s，启用状态 %v，自动构建 %v。",
			p.Name, p.ProjectKey, p.Type, stringOr(p.Team, "未设置"), p.DefaultBranch, p.Enabled, p.AutoBuild),
	}
	if len(builds) == 0 {
		lines = append(lines, "【该项目构建】暂无构建记录。")
	} else {
		parts := make([]string, 0, len(builds))
		for _, b := range builds {
			parts = append(parts, fmt.Sprintf("#%d %s/%s %s（%s）",
				b.BuildNo, b.Branch, shortCommit(b.CommitID), b.Status, stringOr(b.Stage, "-")))
		}
		lines = append(lines, "【该项目最近构建】"+strings.Join(parts, "；"))
	}

	att := ChatAttachment{
		Kind:  "kv",
		Title: "项目 · " + p.Name,
		Data: map[string]string{
			"项目标识":  p.ProjectKey,
			"类型":    p.Type,
			"默认分支":  p.DefaultBranch,
			"自动构建":  fmt.Sprintf("%v", p.AutoBuild),
			"仓库地址":  p.GitURL,
			"最近构建数": fmt.Sprintf("%d", len(builds)),
		},
	}
	return lines, &att
}

// envFacts 环境相关事实。
func (a *AIService) envFacts(ctx context.Context, envKey, projectKey string) ([]string, *ChatAttachment) {
	db := a.db(ctx)
	var env model.Environment
	if err := db.Where("env_key = ?", envKey).First(&env).Error; err != nil {
		return []string{fmt.Sprintf("【环境】未找到环境 %s。", envKey)}, nil
	}

	lines := []string{
		// 措辞是「集群资源水位」不是「健康状态」：`env.HealthStatus` **只由该集群节点的
		// CPU / 内存用量**决定（见 `SyncEnvironment`：≥90 → error、≥75 → warning、
		// 读不到 → unknown），与服务是否健康无关。叫"健康状态"会让模型答出
		// "该环境健康状态异常"这种把人吓一跳、又不指向任何具体毛病的话。
		// 用户要的是服务健康 —— 那是 `get_service_status` 去集群读的。
		fmt.Sprintf("【环境】%s（%s）所在集群资源水位 %s（仅节点 CPU/内存，越线不等于服务故障），集群 %s，命名空间 %s，需审批 %v。",
			env.Name, env.EnvKey, stringOr(env.HealthStatus, "unknown"), env.Cluster, env.Namespace,
			env.NeedApproval),
	}

	q := db.Where("env_key = ?", envKey)
	if projectKey != "" {
		q = q.Where("project_key = ?", projectKey)
	}
	var svcs []model.ServiceInstance
	_ = q.Order("id").Limit(20).Find(&svcs).Error

	if len(svcs) == 0 {
		lines = append(lines, "【环境内服务】该范围内没有服务实例。")
	} else {
		abnormal := make([]string, 0, 4)
		for _, s := range svcs {
			if s.Status != "running" {
				abnormal = append(abnormal, fmt.Sprintf("%s(%s %d/%d)", s.ProjectKey, s.Status, s.ReadyReplicas, s.Replicas))
			}
		}
		lines = append(lines, fmt.Sprintf("【环境内服务】共 %d 个；登记状态异常 %d 个%s。",
			len(svcs), len(abnormal), condition(len(abnormal) > 0, "："+strings.Join(abnormal, "、"), "")))
	}

	// 近期部署
	var deploys []model.DeployRecord
	dq := db.Where("env_key = ?", envKey)
	if projectKey != "" {
		dq = dq.Where("project_key = ?", projectKey)
	}
	_ = dq.Order("id desc").Limit(5).Find(&deploys).Error
	if len(deploys) == 0 {
		lines = append(lines, "【近期部署】该范围内暂无部署记录。")
	} else {
		parts := make([]string, 0, len(deploys))
		for _, d := range deploys {
			parts = append(parts, fmt.Sprintf("%s %s@%s %s（%s，%s）",
				d.DeployNo, d.ProjectKey, tagOf(d.Image), d.Status, d.OperatorName, formatTime(d.CreatedAt)))
		}
		lines = append(lines, "【近期部署】"+strings.Join(parts, "；"))
	}

	data := map[string]string{
		"环境":     env.Name,
		"集群资源水位": stringOr(env.HealthStatus, "unknown"),
		"集群":     env.Cluster,
		"命名空间":   env.Namespace,
		"服务数":    fmt.Sprintf("%d", len(svcs)),
	}
	if len(deploys) > 0 {
		d := deploys[0]
		data["最近部署单"] = d.DeployNo
		data["最近部署镜像"] = tagOf(d.Image)
		data["最近部署状态"] = d.Status
		data["最近部署人"] = d.OperatorName
	}
	return lines, &ChatAttachment{Kind: "kv", Title: "环境 · " + env.Name, Data: data}
}

// --------------------------------------------------------------------------- 小工具

// tagOf 从完整镜像地址取 tag。
func tagOf(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i+1:], "/") {
		return image[i+1:]
	}
	return image
}

// condition 三元表达式，避免为了一个分支写四行 if。
func condition(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}

// randomToken 生成 n 字节随机十六进制串（2n 个字符），用于会话标识。
//
// 用 crypto/rand 而非时间戳：会话 ID 会回给前端并出现在日志里，
// 可预测的 ID 会让"别人的会话"变得可猜测。
func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
