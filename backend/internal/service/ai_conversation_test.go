package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/auth"
	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/logstream"
	"github.com/2777484478/yunnian/internal/model"
)

// 本文件盯的是「AI 会话历史」三件必须成立的事：
//
//	1. **归属隔离** —— 别人的会话既看不到、也打不开、更删不掉；
//	2. **删除是真看不见** —— 软删的会话不许再从列表或详情里冒出来，消息要一并清掉；
//	3. **历史进 prompt 的顺序与截断** —— 倒序取、正序给，跳过空正文。
//
// 前两条是数据泄露与"删了还在"这类问题，测试是唯一能长期盯住它们的地方
// （手工点两下看不出来）；第三条错了**不会报任何错**，只会让模型把回答当成
// 提问的前文来读，越聊越乱。

func newAIForTest(t *testing.T) (*AIService, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	svc := New(Deps{
		Cfg:    &config.Config{},
		DB:     db,
		Log:    discardLogger(),
		Stream: logstream.NewMemStream(),
		Up:     &integration.Set{},
	})
	return svc.AI, db
}

func aiCaller(id uint64) Caller {
	return Caller{User: &auth.CurrentUser{ID: id, Username: "u" + idString(id)}}
}

// seedConversation 直接写库造一条会话（不走 HTTP，测的是 service 语义）。
func seedConversation(t *testing.T, db *gorm.DB, userID uint64, key, title string) uint64 {
	t.Helper()
	c := &model.AIConversation{ConvKey: key, UserID: userID, Title: title, LastMsgAt: now()}
	if err := db.Create(c).Error; err != nil {
		t.Fatalf("造会话失败: %v", err)
	}
	return c.ID
}

// TestConversationOwnershipIsolated 别人的会话：看不到、打不开、删不掉。
//
// `ai:query` 是登录即有的权限，而问句里常带环境名、服务名、失败现象 ——
// 归属过滤漏一次就是数据泄露，且**不会报错**（列表照样返回，只是多了几条别人的）。
func TestConversationOwnershipIsolated(t *testing.T) {
	ai, db := newAIForTest(t)
	ctx := context.Background()

	seedConversation(t, db, 1, "conv_aaa", "甲的问题")
	seedConversation(t, db, 2, "conv_bbb", "乙的问题")

	list, total, err := ai.ListConversations(ctx, aiCaller(1), 1, 50)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].Key != "conv_aaa" {
		t.Errorf("甲应只看到自己的 1 条会话，实际 total=%d list=%+v", total, list)
	}

	if _, _, err := ai.ConversationDetail(ctx, aiCaller(1), "conv_bbb"); err == nil {
		t.Error("甲不该能打开乙的会话")
	}
	if err := ai.DeleteConversation(ctx, aiCaller(1), "conv_bbb"); err == nil {
		t.Error("甲不该能删除乙的会话")
	}

	// 乙的东西必须原封不动
	if _, total, _ := ai.ListConversations(ctx, aiCaller(2), 1, 50); total != 1 {
		t.Errorf("乙的会话被动了：仍应有 1 条，实际 %d", total)
	}
	if _, msgs, err := ai.ConversationDetail(ctx, aiCaller(2), "conv_bbb"); err != nil || len(msgs) != 0 {
		t.Errorf("乙应能正常打开自己的会话，实际 err=%v msgs=%d", err, len(msgs))
	}
}

// TestConversationSoftDeleteHidesIt 删掉的会话不许再从任何入口冒出来。
func TestConversationSoftDeleteHidesIt(t *testing.T) {
	ai, db := newAIForTest(t)
	ctx := context.Background()

	seedConversation(t, db, 1, "conv_gone", "要删的")
	seedConversation(t, db, 1, "conv_keep", "要留的")

	if err := ai.DeleteConversation(ctx, aiCaller(1), "conv_gone"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	list, total, _ := ai.ListConversations(ctx, aiCaller(1), 1, 50)
	if total != 1 || len(list) != 1 || list[0].Key != "conv_keep" {
		t.Errorf("删除后应只剩 conv_keep，实际 total=%d list=%+v", total, list)
	}
	if _, _, err := ai.ConversationDetail(ctx, aiCaller(1), "conv_gone"); err == nil {
		t.Error("已删除的会话不该还能按 key 打开")
	}
}

// TestDeleteConversationPurgesMessages 删会话要**连消息一起清掉**。
//
// 用户点"删除"的意思是"这段对话我不要了"，把正文继续留在库里不符合这个预期
// （而会话行本身软删，方便日后排查"谁在什么时候删过"）。
func TestDeleteConversationPurgesMessages(t *testing.T) {
	ai, db := newAIForTest(t)
	ctx := context.Background()

	id := seedConversation(t, db, 1, "conv_aaa", "带消息的")
	for _, role := range []string{"user", "assistant"} {
		if err := ai.appendMessage(ctx, id, role, "内容-"+role, nil, nil); err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
	}
	var before int64
	db.Model(&model.AIMessage{}).Where("conversation_id = ?", id).Count(&before)
	if before != 2 {
		t.Fatalf("应有 2 条消息，实际 %d", before)
	}

	if err := ai.DeleteConversation(ctx, aiCaller(1), "conv_aaa"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	var after int64
	db.Model(&model.AIMessage{}).Where("conversation_id = ?", id).Count(&after)
	if after != 0 {
		t.Errorf("删会话必须一并清掉消息，实际还剩 %d 条", after)
	}
}

// TestConversationRequiresLogin 未登录不许读会话。
//
// 宁可报"需要登录"，也不能把它退化成"那就给你看所有人的"。
func TestConversationRequiresLogin(t *testing.T) {
	ai, _ := newAIForTest(t)
	ctx := context.Background()

	if _, _, err := ai.ListConversations(ctx, Caller{}, 1, 10); err == nil {
		t.Error("未登录不该能读会话列表")
	}
	if _, _, err := ai.ConversationDetail(ctx, Caller{}, "conv_aaa"); err == nil {
		t.Error("未登录不该能读会话详情")
	}
	if err := ai.DeleteConversation(ctx, Caller{}, "conv_aaa"); err == nil {
		t.Error("未登录不该能删会话")
	}
}

// TestConvTitle 标题：折换行、压空白、按**字符**截断。
func TestConvTitle(t *testing.T) {
	cases := map[string]string{
		"":             "新会话",
		"   ":          "新会话",
		"帮我看看\n测试区的问题": "帮我看看 测试区的问题",
		"短问题":          "短问题",
	}
	for in, want := range cases {
		if got := convTitle(in); got != want {
			t.Errorf("convTitle(%q) = %q，期望 %q", in, got, want)
		}
	}

	// 超长按 rune 截断：按字节截会把汉字切成半个，在列表里显示成乱码方块。
	got := convTitle(strings.Repeat("测", aiTitleRunes+10))
	if n := len([]rune(got)); n != aiTitleRunes {
		t.Errorf("超长标题应截到 %d 个字符，实际 %d（%q）", aiTitleRunes, n, got)
	}
}

// TestLoadHistoryOrderLimitAndSkipEmpty 历史：倒序取最近的、**正序**交出去、跳过空正文。
func TestLoadHistoryOrderLimitAndSkipEmpty(t *testing.T) {
	ai, db := newAIForTest(t)
	ctx := context.Background()

	id := seedConversation(t, db, 1, "conv_aaa", "长对话")
	total := aiHistoryTurns*2 + 4
	for i := 0; i < total; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := ai.appendMessage(ctx, id, role, fmt.Sprintf("第 %d 条", i), nil, nil); err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
	}

	h, err := ai.loadHistory(ctx, id)
	if err != nil {
		t.Fatalf("取历史失败: %v", err)
	}
	if len(h) != aiHistoryTurns*2 {
		t.Fatalf("应截断到 %d 条，实际 %d", aiHistoryTurns*2, len(h))
	}
	// 正序：最早的一条在前。反了的话模型会把回答当成提问的前文读，越聊越乱。
	if h[0].Content != fmt.Sprintf("第 %d 条", total-aiHistoryTurns*2) {
		t.Errorf("历史应为正序（最早在前），实际首条 = %q", h[0].Content)
	}
	if last := h[len(h)-1].Content; last != fmt.Sprintf("第 %d 条", total-1) {
		t.Errorf("末条应为最新那条，实际 %q", last)
	}
	if h[0].Role != "user" {
		t.Errorf("首条应为 user（截断点正好落在一问一答的边界上），实际 %q", h[0].Role)
	}
}

// TestLoadHistorySkipsEmptyContent 空正文的消息要跳过。
//
// 上一轮被用户中断、模型一个字都没吐时，会留下一条空 assistant 消息；
// 把它塞进 prompt，上游会当成畸形历史（某些通道直接 400）。
func TestLoadHistorySkipsEmptyContent(t *testing.T) {
	ai, db := newAIForTest(t)
	ctx := context.Background()

	id := seedConversation(t, db, 1, "conv_aaa", "有中断的")
	_ = ai.appendMessage(ctx, id, "user", "第一个问题", nil, nil)
	_ = ai.appendMessage(ctx, id, "assistant", "   ", nil, nil)
	_ = ai.appendMessage(ctx, id, "user", "第二个问题", nil, nil)

	h, err := ai.loadHistory(ctx, id)
	if err != nil {
		t.Fatalf("取历史失败: %v", err)
	}
	if len(h) != 2 {
		t.Fatalf("空正文应被跳过，期望 2 条，实际 %d：%+v", len(h), h)
	}
	if h[1].Content != "第二个问题" {
		t.Errorf("顺序错了：末条应为「第二个问题」，实际 %q", h[1].Content)
	}
}

// TestAppendMessageCountsAndMovesSessionUp 写一条消息要同时更新计数与"最后消息时间"。
//
// 只写消息不更新会话的话，左侧列表的排序与"3 分钟前"会永远停在创建那一刻。
func TestAppendMessageCountsAndMovesSessionUp(t *testing.T) {
	ai, db := newAIForTest(t)
	ctx := context.Background()

	id := seedConversation(t, db, 1, "conv_aaa", "会动的")
	before := now().Add(-time.Hour)

	var old model.AIConversation
	db.First(&old, id)
	if old.MsgCount != 0 {
		t.Fatalf("新会话计数应为 0，实际 %d", old.MsgCount)
	}

	if err := ai.appendMessage(ctx, id, "user", "你好", nil, nil); err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	var after model.AIConversation
	db.First(&after, id)
	if after.MsgCount != 1 {
		t.Errorf("计数应为 1，实际 %d", after.MsgCount)
	}
	if !after.LastMsgAt.After(before) {
		t.Errorf("最后消息时间应被推进，实际 %v（原为 %v）", after.LastMsgAt, before)
	}
}

// TestConversationByKeyRejectsEmptyKey 空 key 要明确拒绝，不许当成"查第一个"。
func TestConversationByKeyRejectsEmptyKey(t *testing.T) {
	ai, _ := newAIForTest(t)
	if _, _, err := ai.ConversationDetail(context.Background(), aiCaller(1), "   "); err == nil {
		t.Error("空会话标识应被拒绝")
	}
}
