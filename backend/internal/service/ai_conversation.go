package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/llm"
	"github.com/2777484478/yunnian/internal/model"
)

// --------------------------------------------------------------------------- AI 会话历史
//
// 2026-10-15 新增。原先 `/ai/conversations` 是一个**空壳**：一期（2026-10-12）
// 明确拍板"不持久化会话历史"，那个 handler 直接返回空列表 + 一句说明。
// 用户的原话是"换一个页面再回来就没了，能不能改成像 DeepSeek 那样左边能看到历史会话"。
//
// 三条纪律，改这个文件前先读：
//
//  1. **会话是私有的**：列表与详情一律按 `user_id` 过滤。漏一次就是数据泄露 ——
//     `ai:query` 是登录即有的权限，而问句里常带着环境名、服务名、失败现象这类内部信息。
//  2. **对外只暴露 ConvKey**，不暴露自增主键：主键可枚举，改个数字就能试探存在性。
//  3. **历史只当对话上下文用，事实快照每次提问都重新注入**。否则用户今天接着问
//     "那现在呢"，模型会拿昨天快照里的结论当现状回答 —— 而它读起来毫无破绽。
const (
	// aiHistoryTurns 带多少轮（一问一答算一轮）历史进 prompt。
	//
	// 不设上限的话，一个聊了几十轮的长会话每问一句都要把全文重发一遍：既慢又贵，
	// 而且**越聊越容易跑偏** —— 模型会去纠正自己很早以前说错的一句话。
	// 10 轮足够覆盖"接着往下问"的真实场景。
	aiHistoryTurns = 10

	// aiTitleRunes 会话标题取首问的前多少个字。
	// 太长列表里放不下（会被截成一模一样的前缀），太短又认不出是哪条。
	aiTitleRunes = 24
)

// AIConversationDTO 会话列表项，也是详情页的会话头。
type AIConversationDTO struct {
	// Key 对外标识（conv_xxxxxx），前端全程用它。
	Key   string `json:"key"`
	Title string `json:"title"`
	// MsgCount 消息条数（user + assistant 合计）。
	MsgCount int `json:"msgCount"`
	// LastMsgAt 最后一条消息的时间。前端按它分"今天 / 7 天内 / 30 天内 / 更早"，
	// 所以必须是 **ISO8601 带时区**，不能只给日期。
	LastMsgAt string `json:"lastMsgAt"`
	CreatedAt string `json:"createdAt"`
}

// AIMessageDTO 会话里的一条消息。
type AIMessageDTO struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	// Attachments / Tools 当时的结构化留痕，回看时原样重放。
	Attachments []ChatAttachment `json:"attachments"`
	Tools       []AIToolResult   `json:"tools"`
	At          string           `json:"at"`
}

// aiUserID 取当前用户。
//
// ai:query 是登录后才有的权限，正常路径下这里一定有值；但仍然显式判一次 ——
// 会话是私有数据，"没认出你是谁"绝不能退化成"于是给你看所有人的"。
func aiUserID(c Caller) (uint64, error) {
	if c.User == nil || c.User.ID == 0 {
		return 0, errs.Unauthorized("需要登录后才能使用 AI 助手")
	}
	return c.User.ID, nil
}

// ListConversations 当前账号的会话列表，按最后一条消息倒序。
func (a *AIService) ListConversations(ctx context.Context, c Caller, page, pageSize int) ([]AIConversationDTO, int64, error) {
	uid, err := aiUserID(c)
	if err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}

	db := a.db(ctx).Model(&model.AIConversation{}).Where("user_id = ?", uid)
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, errs.Internalf(err, "统计会话数失败")
	}
	var rows []model.AIConversation
	if err := db.Order("last_msg_at DESC, id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error; err != nil {
		return nil, 0, errs.Internalf(err, "查询会话列表失败")
	}
	out := make([]AIConversationDTO, 0, len(rows))
	for i := range rows {
		out = append(out, convToDTO(&rows[i]))
	}
	return out, total, nil
}

// ConversationDetail 取一个会话及其全部消息。
func (a *AIService) ConversationDetail(ctx context.Context, c Caller, key string) (*AIConversationDTO, []AIMessageDTO, error) {
	conv, err := a.conversationByKey(ctx, c, key)
	if err != nil {
		return nil, nil, err
	}
	var rows []model.AIMessage
	// 正序：翻记录要从头看。负数页大小那种"倒序取最近 N 条"是给 prompt 用的，
	// 展示路径不能倒着来（用户看到的第一条会是最后一句话）。
	if err := a.db(ctx).Where("conversation_id = ?", conv.ID).
		Order("id ASC").Find(&rows).Error; err != nil {
		return nil, nil, errs.Internalf(err, "查询会话消息失败")
	}
	msgs := make([]AIMessageDTO, 0, len(rows))
	for i := range rows {
		msgs = append(msgs, msgToDTO(&rows[i]))
	}
	dto := convToDTO(conv)
	return &dto, msgs, nil
}

// DeleteConversation 删除一个会话。
//
// 会话**软删**（对外不可见、也查不回来），消息**硬删** —— 用户点删除的意思是
// "这段对话我不要了"，把正文继续留在库里不符合这个预期。
func (a *AIService) DeleteConversation(ctx context.Context, c Caller, key string) error {
	conv, err := a.conversationByKey(ctx, c, key)
	if err != nil {
		return err
	}
	if err := a.db(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("conversation_id = ?", conv.ID).Delete(&model.AIMessage{}).Error; err != nil {
			return err
		}
		return tx.Delete(&model.AIConversation{}, conv.ID).Error
	}); err != nil {
		return errs.Internalf(err, "删除会话失败")
	}
	a.audit(ctx, c, auditInput{
		Module: "AI", Action: "conversation:delete", TargetType: "ai_conversation",
		TargetID: &conv.ID, TargetName: conv.Title,
		Detail: "删除 AI 会话（消息已一并清除）",
	})
	return nil
}

// --------------------------------------------------------------------------- 内部

// conversationByKey 按对外标识取会话，**并校验归属**。
//
// 不属于当前账号时统一报"不存在"，而不是"无权访问"：后者等于告诉对方
// "这个 ID 是存在的，只是不属于你" —— 那就是一个可枚举的信标。
func (a *AIService) conversationByKey(ctx context.Context, c Caller, key string) (*model.AIConversation, error) {
	uid, err := aiUserID(c)
	if err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errs.Invalid("会话标识不能为空")
	}
	var conv model.AIConversation
	if err := a.db(ctx).Where("conv_key = ? AND user_id = ?", key, uid).First(&conv).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("会话不存在，或不属于当前账号")
		}
		return nil, errs.Internalf(err, "查询会话失败")
	}
	return &conv, nil
}

// ensureConversation 取出本次提问所属的会话；没给 key 就现建一个。
//
// 前端只在"接着某个会话聊"时传 key，新话题不传 —— 所以新建发生在**第一句提问时**，
// 而不是打开页面时（否则用户点进来又离开，就会攒下一堆空会话）。
func (a *AIService) ensureConversation(ctx context.Context, c Caller, key, firstQuestion string) (*model.AIConversation, error) {
	if k := strings.TrimSpace(key); k != "" {
		return a.conversationByKey(ctx, c, k)
	}
	uid, err := aiUserID(c)
	if err != nil {
		return nil, err
	}
	conv := &model.AIConversation{
		ConvKey:   "conv_" + randomToken(6),
		UserID:    uid,
		Title:     convTitle(firstQuestion),
		LastMsgAt: now(),
	}
	if err := a.db(ctx).Create(conv).Error; err != nil {
		return nil, errs.Internalf(err, "创建会话失败")
	}
	return conv, nil
}

// convTitle 用首问生成标题。
//
// 折掉换行、压掉连续空白：问句里带换行的话，列表那一行会显示成一段空白，
// 用户看到的是"这条会话没有标题"。
func convTitle(q string) string {
	t := strings.Join(strings.Fields(q), " ")
	if t == "" {
		return "新会话"
	}
	return clipRunes(t, aiTitleRunes)
}

// loadHistory 取最近若干轮历史，拼成模型能吃的消息序列。
func (a *AIService) loadHistory(ctx context.Context, convPK uint64) ([]llm.Message, error) {
	var rows []model.AIMessage
	// 倒着查是为了拿"最近的 N 条"，拼给模型时要**正序**：
	// 顺序错了，模型会把回答当成提问的前文来读，越聊越乱。
	if err := a.db(ctx).Where("conversation_id = ?", convPK).
		Order("id DESC").Limit(aiHistoryTurns * 2).Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "读取会话历史失败")
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]llm.Message, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		role := rows[i].Role
		if role != "user" && role != "assistant" {
			continue
		}
		// 空正文的 assistant 消息（上一轮被中断、模型没吐字）要跳过：
		// 上游看到一条空的 assistant 消息会当成畸形历史。
		if strings.TrimSpace(rows[i].Content) == "" {
			continue
		}
		out = append(out, llm.Message{Role: role, Content: rows[i].Content})
	}
	return out, nil
}

// appendMessage 落一条消息，并同步会话的计数与"最后消息时间"。
func (a *AIService) appendMessage(ctx context.Context, convPK uint64, role, content string, atts []ChatAttachment, tools []AIToolResult) error {
	msg := &model.AIMessage{ConversationID: convPK, Role: role, Content: content}
	if len(atts) > 0 {
		if b, err := json.Marshal(atts); err == nil {
			msg.Attachments = model.JSONText(b)
		}
	}
	if len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			msg.Tools = model.JSONText(b)
		}
	}
	at := now()
	return a.db(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(msg).Error; err != nil {
			return err
		}
		return tx.Model(&model.AIConversation{}).Where("id = ?", convPK).
			Updates(map[string]any{
				"msg_count":   gorm.Expr("msg_count + 1"),
				"last_msg_at": at,
			}).Error
	})
}

// FinishConversation 把本轮的提问与回答写进历史。
//
// ⚠️ **必须用独立的 ctx**（`WithoutCancel` + 从此刻起算的宽限）：
// 它在 SSE 流收尾时调用，而那时用户很可能已经切走页面 —— 请求 ctx 已取消。
// 拿一个已取消的 ctx 去写库，表现是"回答看到了、历史却没留下"，
// 而且**三件事（写库、记日志、收尾）会一起静默失败**，不报任何错。
//
// 失败只记日志、不往上抛：回答已经推给用户了，此时再报错只会弹一个
// 用户无法处理的提示；而历史丢一条不该影响这次问答的结论。
func (a *AIService) FinishConversation(ctx context.Context, sess *ChatSession, question, answer string) {
	if sess == nil || sess.convPK == 0 {
		return
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := a.appendMessage(bg, sess.convPK, "user", question, nil, nil); err != nil {
		a.log("ai").Warn("落库用户提问失败", "conv", sess.ConversationID, "err", err)
	}
	if strings.TrimSpace(answer) != "" {
		if err := a.appendMessage(bg, sess.convPK, "assistant", answer, sess.Attachments, sess.toolTraces); err != nil {
			a.log("ai").Warn("落库模型回答失败", "conv", sess.ConversationID, "err", err)
		}
	}
}

// convToDTO 会话 → DTO。
func convToDTO(c *model.AIConversation) AIConversationDTO {
	return AIConversationDTO{
		Key:       c.ConvKey,
		Title:     c.Title,
		MsgCount:  c.MsgCount,
		LastMsgAt: formatTime(c.LastMsgAt),
		CreatedAt: formatTime(c.CreatedAt),
	}
}

// msgToDTO 消息 → DTO。留痕 JSON 解析失败时**留空而不是报错**：
// 一条坏掉的留痕不该让整个会话打不开。
func msgToDTO(m *model.AIMessage) AIMessageDTO {
	dto := AIMessageDTO{
		ID:          idString(m.ID),
		Role:        m.Role,
		Content:     m.Content,
		Attachments: []ChatAttachment{},
		Tools:       []AIToolResult{},
		At:          formatTime(m.CreatedAt),
	}
	if len(m.Attachments) > 0 {
		_ = json.Unmarshal(m.Attachments, &dto.Attachments)
	}
	if len(m.Tools) > 0 {
		_ = json.Unmarshal(m.Tools, &dto.Tools)
	}
	return dto
}
