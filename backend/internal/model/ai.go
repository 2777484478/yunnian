package model

import (
	"time"

	"gorm.io/gorm"
)

// --------------------------------------------------------------------------- AI 会话
//
// 这两张表是**相对 docs/02 的新增表**：原 DDL 里没有"会话"这个概念，
// 一期（2026-10-12）明确拍板不落库，`/ai/conversations` 当时只返回一个空壳。
// 2026-10-15 用户要求"历史要留着，像 DeepSeek 那样左边能看" —— 于是补上。
// docs/02 的表清单需要同步补一节（原 22 张 + ai_model_config / k8s_cluster
// + 这里的 ai_conversation / ai_message）。

// AIConversation 一次连续的 AI 对话。
//
// ⚠️ **归属靠 UserID，且列表与详情都必须按当前用户过滤**。
// 不做过滤就是一个数据泄露：任何有 `ai:query` 的人都能读到别人问过什么，
// 而问句里恰恰常带着环境名、服务名、失败现象这类内部信息。
//
// ⚠️ 对外只暴露 `ConvKey`，不暴露自增主键：主键可枚举，
// 改一个数字就能试探出"这个会话存不存在"（以及别人的会话是否被删了）。
type AIConversation struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	// ConvKey 对外标识（形如 `conv_a1b2c3`）。前端全程用它，不用主键。
	ConvKey string `gorm:"column:conv_key;size:64;not null;uniqueIndex:uk_ai_conv_key" json:"key"`
	// UserID 归属用户。ai:query 是登录后即有的权限，所以每个账号都可能有自己的会话。
	UserID uint64 `gorm:"column:user_id;not null;index:idx_ai_conv_user" json:"-"`
	// Title 会话标题：取首问的前若干字。
	// **不生成标题的话左侧列表只能显示时间**，用户认不出哪条是哪条 ——
	// DeepSeek 那套列表之所以好用，全靠标题这一列。
	Title string `gorm:"size:120;not null" json:"title"`
	// MsgCount 消息条数（user + assistant 合计），列表上显示"12 条"。
	MsgCount int `gorm:"column:msg_count;not null;default:0" json:"msgCount"`
	// LastMsgAt 最后一条消息的时间：列表按它倒序，也按它分"今天 / 7 天内 / 30 天内 / 更早"。
	//
	// 单独一列而不用 UpdatedAt：将来若做"重命名会话"之类的元数据更新，
	// 不该把它顶到列表最前面 —— 用户看到的是"这条会话最近聊过"，
	// 而不是"这条记录的数据库行被改过"。
	LastMsgAt time.Time      `gorm:"column:last_msg_at;not null;index:idx_ai_conv_user" json:"lastMsgAt"`
	CreatedAt time.Time      `json:"-"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// TableName 显式指定表名（GORM 的 snake_case 转换对连续大写 AI 结果虽对，但显式更稳）。
func (AIConversation) TableName() string { return "ai_conversation" }

// AIMessage 会话里的一条消息。
//
// 只追加、不修改：消息是"当时到底说了什么"的记录，改它等于改历史。
// 会话软删时消息**保留**（恢复会话还能看到），所以这张表不做软删。
type AIMessage struct {
	ID             uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	ConversationID uint64 `gorm:"column:conversation_id;not null;index:idx_ai_msg_conv" json:"-"`
	// Role 只有 user / assistant 两个取值（system 提示词不落库：
	// 它由代码按当次的事实快照现拼，存下来的那份第二天就是错的）。
	Role string `gorm:"size:16;not null" json:"role"`
	// Content 正文。用 mediumtext：一轮回答常常几千字（含工具取回的事实罗列），
	// 用 text（64KB）勉强够，但没人愿意在一个"存聊天记录"的表上跟长度搏斗。
	Content string `gorm:"type:mediumtext" json:"content"`
	// Attachments / Tools 结构化留痕（JSON 数组）。
	//
	// 存下来是为了回看时能把"当时查了什么工具、拿到了什么卡片"**原样重放**，
	// 而不是只剩一段干巴巴的正文 —— 用户回头翻记录，多半就是想知道
	// "当时到底查了哪几个服务、集群回了什么"。
	Attachments JSONText  `gorm:"type:json" json:"attachments,omitempty"`
	Tools       JSONText  `gorm:"type:json" json:"tools,omitempty"`
	CreatedAt   time.Time `json:"at"`
}

// TableName 显式指定表名。
func (AIMessage) TableName() string { return "ai_message" }
