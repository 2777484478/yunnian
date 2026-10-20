package api

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// --------------------------------------------------------------------------- AI 对话

// aiChat POST /ai/chat（SSE 流式回答）
//
// 为什么是 SSE 而不是 WebSocket：与日志流同构（单向、纯 HTTP、免网关改造），
// 一套连接管理就能覆盖"看日志"与"问 AI"两个场景。
//
// 事件有四类：attachment（结构化卡片）/ tool（工具调用留痕）/ token（正文增量）/ end。
// tool 事件是 2026-10-09 加工具调用时补的：工具在集群上真读一次要几百毫秒到几秒，
// 这段时间里**一个 token 都没有**，界面上就是一片空白。不推这条事件，
// 用户会以为卡死了 —— 而它其实正在查集群。
//
// 安全约束（docs/03 §12）：背后只挂只读查询能力，模型**不能**直接触发变更；
// 变更一律由模型输出 action 卡片、用户点击后走正常 REST（含权限校验与审计）。
func (h *handler) aiChat(c *gin.Context) {
	var in service.ChatInput
	if !httpx.Bind(c, &in) {
		return
	}

	// 流已经开始了就无法再改状态码，所以"检查前置条件"必须放在 newSSEStream 之前。
	// 这里让 service 先做一次轻量预检（模型通道是否可用），避免开流后才发现没配通道。
	pre, err := h.svc.AI.Prepare(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}

	s := newSSEStream(c)
	if len(pre.Attachments) > 0 {
		for _, att := range pre.Attachments {
			if !s.send("attachment", att) {
				return
			}
		}
	}

	// ⚠️ 把 Caller 放进 ctx：AI 工具的可见性判断（哪些环境能查）要靠它。
	// 不注入的话，工具侧拿到的是零值 Caller（User=nil）→ 一律不限制 →
	// 有 ai:query 的人能通过「查一下正式区 xx 服务」读到 prod，绕过环境可见性。
	ctx := service.WithCaller(c.Request.Context(), caller(c))
	text, _, err := h.svc.AI.Run(ctx, pre,
		func(delta string) error {
			if !s.send("token", gin.H{"delta": delta}) {
				return context.Canceled
			}
			return nil
		},
		func(res service.AIToolResult) error {
			if !s.send("tool", res) {
				return context.Canceled
			}
			return nil
		},
	)
	if err != nil {
		// 流已经开始，HTTP 状态码改不了了，用一条 error 事件把原因交给前端。
		s.send("error", gin.H{"message": err.Error()})
	} else if strings.TrimSpace(text) == "" {
		// 一个字都没吐出来，但也没报错。**不能就这么结束**：前端会渲染一个
		// 空白气泡，用户只能盯着它猜。给一句能行动的话。
		s.send("token", gin.H{
			"delta": "模型这次没有返回任何内容。可以换个说法再问一次；" +
				"若反复如此，请到「系统管理 › 模型设置」确认当前通道的连通性。",
		})
	}

	// 落库放在 end 之前、**且不区分成功失败**：失败的这一轮同样要留痕 ——
	// 用户回头翻记录，恰恰常常是想再看一遍"当时那次为什么没答上来"。
	// 它内部自己开 ctx（客户端此刻可能已经断开，见 FinishConversation 的注释），
	// 失败也只记日志，不会影响已经推给用户的内容。
	h.svc.AI.FinishConversation(c.Request.Context(), pre, in.Message, text)

	// end 事件即使出错也要发：前端据此关闭 EventSource，
	// 否则它只能靠心跳超时才断开，表现为"回答完了但连接不动"。
	s.send("end", gin.H{"reason": "finished", "conversationId": pre.ConversationID})
}

// aiConversations GET /ai/conversations
//
// 只返回**当前账号自己的**会话。2026-10-15 之前这里是个空壳（一期不落库），
// 现在真查库 —— 归属过滤在 service 层，见 ai_conversation.go 的文件头。
func (h *handler) aiConversations(c *gin.Context) {
	q := pageQuery(c)
	list, total, err := h.svc.AI.ListConversations(c.Request.Context(), caller(c), q.Page, q.PageSize)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list, "total": total})
}

// aiConversation GET /ai/conversations/{key}
func (h *handler) aiConversation(c *gin.Context) {
	conv, msgs, err := h.svc.AI.ConversationDetail(c.Request.Context(), caller(c), c.Param("id"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"conversation": conv, "messages": msgs})
}

// aiDeleteConversation DELETE /ai/conversations/{key}
func (h *handler) aiDeleteConversation(c *gin.Context) {
	if err := h.svc.AI.DeleteConversation(c.Request.Context(), caller(c), c.Param("id")); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}

// --------------------------------------------------------------------------- AI 归因

// aiAnalyzeBuild POST /ai/analyze-build/{buildId}
//
// 与 GET /builds/{id}/ai-diagnosis 同源：保留 POST 形态是为了兼容
// "前端想强制重算一次"的场景（服务层按需重算，命中缓存则直接返回）。
func (h *handler) aiAnalyzeBuild(c *gin.Context) {
	id, ok := pathUint(c, "buildId")
	if !ok {
		return
	}
	d, err := h.svc.CI.BuildAIDiagnosis(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, d)
}

// aiAnalyzeDeploy POST /ai/analyze-deploy/{deployId}
func (h *handler) aiAnalyzeDeploy(c *gin.Context) {
	id, ok := pathUint(c, "deployId")
	if !ok {
		return
	}
	d, err := h.svc.CD.DeploymentAIDiagnosis(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, d)
}
