// Package llm 封装大模型调用，统一走 OpenAI 兼容协议。
//
// 为什么只做一种协议：平台的模型通道（百炼 compatible-mode、DeepSeek、内网 vLLM）
// 都提供 OpenAI 兼容的 /chat/completions。统一到一条协议，前端与业务只关心
// "选哪个通道、哪个模型"，不需要为每家写一套适配。
//
// 通道配置来自数据库表 ai_model_config（见「系统管理 › 模型设置」），
// 不在配置文件里——因为密钥要脱敏回显、要能随时停用、要能在线测连通性。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotConfigured 没有可用的模型通道。
var ErrNotConfigured = errors.New("未配置可用的大模型通道")

// Message 一条对话消息。
//
// 支持 OpenAI 兼容的函数调用三段式：
//
//	assistant（带 ToolCalls）→ tool（带 ToolCallID 回结果）→ assistant（正文）
//
// ToolCalls / ToolCallID 都用了 omitempty：普通对话消息不该凭空多出这两个字段，
// 有的网关对不认识的空字段比对人话更挑剔。
type Message struct {
	Role    string `json:"role"` // system / user / assistant / tool
	Content string `json:"content"`
	// ToolCalls 仅 assistant 消息使用：模型要求调用哪些函数。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 仅 tool 消息使用：回应的是哪一次调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall 一次函数调用请求。
//
// Index 只在**流式**响应里有意义：同一轮可能请求多个函数，它们的参数分片
// 交错到达，只能按 index 归并（实测百炼：首帧带 id 与函数名，后续帧 id 为空串、
// 只带 arguments 分片，收尾还有一帧 arguments 为 null）。
type ToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 函数名与参数（参数是模型给的 JSON 文本，不是对象）。
//
// 刻意保持 string：模型偶尔会给出**不合法**的 JSON，收成 map 会在解析阶段
// 直接失败，丢掉"它到底想调用什么、传了什么"。保持原文才能在工具层
// 给出可读的"参数不合法"而不是一个 unmarshal 错误。
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool 提供给模型的工具声明。
type Tool struct {
	Type     string         `json:"type"` // 固定 "function"
	Function ToolDefinition `json:"function"`
}

// ToolDefinition 工具的名字、用途与入参 JSON Schema。
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
}

// Usage token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Config 一次调用的通道参数。
type Config struct {
	BaseURL     string
	APIKey      string
	Model       string
	Timeout     time.Duration
	MaxTokens   int
	Temperature float64
	// Name 通道名，用于错误提示里指明是哪个通道挂了。
	Name string
	// Provider 厂商标识（dashscope / deepseek / custom…）。
	Provider string
}

// Endpoint 拼出 chat/completions 地址。
//
// 约定 baseUrl 填到 /v1 这一级（前端模型设置页有同名约束），
// 这里只做去尾部斜杠 + 补路径，不猜厂商差异。
func (c Config) Endpoint() string {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// Client 模型客户端。
type Client struct {
	cfg Config
	hc  *http.Client
}

// New 构造客户端。
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Client{cfg: cfg, hc: &http.Client{Timeout: timeout}}
}

// Config 返回当前配置。
func (c *Client) Config() Config { return c.cfg }

// Label 返回便于排障的通道标识。
func (c *Client) Label() string {
	if c.cfg.Name != "" {
		return c.cfg.Name
	}
	if c.cfg.Provider != "" {
		return c.cfg.Provider
	}
	return "默认通道"
}

// --------------------------------------------------------------------------- 请求/响应结构

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	// ToolChoice 只在需要时下发（空则完全不出现在请求体里）。
	// "required" = 强制模型这一轮必须发起工具调用。
	ToolChoice string `json:"tool_choice,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		Delta        Message `json:"delta"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// --------------------------------------------------------------------------- 调用

// Ping 连通性测试：调用 /models 列表接口，返回耗时。
//
// 用 /models 而不是发一条真实推理：测试连通性不应该消耗 token，
// 也不该因为模型名写错而误判成"通道不通"。
func (c *Client) Ping(ctx context.Context) (time.Duration, error) {
	base := strings.TrimRight(strings.TrimSpace(c.cfg.BaseURL), "/")
	if base == "" {
		return 0, fmt.Errorf("BaseURL 为空")
	}
	endpoint := base
	if !strings.HasSuffix(endpoint, "/models") {
		endpoint += "/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	c.auth(req)

	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("请求 %s 失败: %w", endpoint, err)
	}
	defer resp.Body.Close()
	// 读取少量 body 用于错误提示（有的网关把错误写在 body 里）
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 400 {
		return time.Since(start), fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return time.Since(start), nil
}

// Chat 一次性问答。
func (c *Client) Chat(ctx context.Context, msgs []Message) (string, Usage, error) {
	var out chatResponse
	if err := c.do(ctx, chatRequest{
		Model:       c.modelName(),
		Messages:    msgs,
		MaxTokens:   c.cfg.MaxTokens,
		Temperature: c.cfg.Temperature,
	}, &out); err != nil {
		return "", Usage{}, err
	}
	if len(out.Choices) == 0 {
		return "", out.Usage, fmt.Errorf("通道 %s 未返回任何内容", c.Label())
	}
	return out.Choices[0].Message.Content, out.Usage, nil
}

// ChatStream 流式问答。每收到一个增量片段回调一次 onDelta。
//
// 返回累积的完整文本与用量（流式响应里用量只在最后一帧出现，可能为空）。
// 不支持工具调用；需要工具的调用方请用 ChatStreamStep。
func (c *Client) ChatStream(ctx context.Context, msgs []Message, onDelta func(string) error) (string, Usage, error) {
	st, err := c.ChatStreamStep(ctx, msgs, nil, onDelta)
	return st.Content, st.Usage, err
}

// Step 一轮流式调用的结果。
//
// **一轮请求可能同时产出正文与工具调用**：模型常常先说一句"我先查一下集群状态"，
// 再给出 tool_calls。两者都要留住 —— 正文已经边收边推给前端了，工具调用则要
// 回灌进消息列表再开下一轮。
type Step struct {
	// Content 本轮累积的正文。
	Content string
	// ToolCalls 本轮模型要求调用的函数（可能为空 = 本轮就是最终答复）。
	ToolCalls []ToolCall
	Usage     Usage
	// Finish 结束原因：stop / tool_calls / length …。为空表示流里没给。
	Finish string
}

// ChatStreamStep 一轮流式调用，支持函数调用。
//
// 为什么坚持"每轮都流式"而不是"前几轮非流式探路、最后一轮流式"：
// 后者在**不需要工具**的普通提问上会退化成"等模型全部生成完再一次性吐出来"，
// 打字机效果和首字延迟都要赔进去。而实测百炼 compatible-mode 的流式响应
// 会按 index 分片下发 tool_calls，所以前者完全可行。
func (c *Client) ChatStreamStep(
	ctx context.Context, msgs []Message, tools []Tool, onDelta func(string) error,
) (Step, error) {
	return c.ChatStreamStepWithChoice(ctx, msgs, tools, "", onDelta)
}

// ChatStreamStepWithChoice 与 ChatStreamStep 相同，但可以指定 tool_choice。
//
// 为什么保留原方法：绝大多数调用点不需要强制调用工具，签名里多一个恒空的参数
// 只会让每个调用点都要想一下"这里该填什么"。
//
// toolChoice 传 "required" 的意义：让模型这一轮**必须**发起工具调用。
// 用来兜住"模型把工具调用过程写进正文、然后编数据"这类行为 —— 
// 提示词约束只能降低概率，tool_choice 是机制层面的保证。
// 传空串则与原来完全一致（请求体里不会出现 tool_choice 字段）。
func (c *Client) ChatStreamStepWithChoice(
	ctx context.Context, msgs []Message, tools []Tool, toolChoice string, onDelta func(string) error,
) (Step, error) {
	var step Step
	body, err := json.Marshal(chatRequest{
		Model:       c.modelName(),
		Messages:    msgs,
		Stream:      true,
		MaxTokens:   c.cfg.MaxTokens,
		Temperature: c.cfg.Temperature,
		Tools:       tools,
		ToolChoice:  toolChoice,
	})
	if err != nil {
		return step, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint(), bytes.NewReader(body))
	if err != nil {
		return step, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	c.auth(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return step, fmt.Errorf("请求模型通道 %s 失败: %w", c.Label(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return step, fmt.Errorf("模型通道 %s 返回 HTTP %d: %s", c.Label(), resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var (
		full strings.Builder
		// 工具调用参数是按 index 分片到达的，必须先攒后拼。
		pending = map[int]*ToolCall{}
		order   []int
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk chatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单帧解析失败不中断整条流：网关可能插入心跳或注释帧。
			continue
		}
		if chunk.Error != nil {
			return step, fmt.Errorf("模型通道 %s 报错: %s", c.Label(), chunk.Error.Message)
		}
		if chunk.Usage.TotalTokens > 0 {
			step.Usage = chunk.Usage
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != "" {
				step.Finish = ch.FinishReason
			}
			delta := ch.Delta.Content
			if delta == "" && ch.Delta.ToolCalls == nil {
				delta = ch.Message.Content
			}
			if delta != "" {
				full.WriteString(delta)
				if onDelta != nil {
					if err := onDelta(delta); err != nil {
						step.Content = full.String()
						return step, err
					}
				}
			}
			mergeToolCallDeltas(pending, &order, ch.Delta.ToolCalls)
			mergeToolCallDeltas(pending, &order, ch.Message.ToolCalls)
		}
	}
	if err := scanner.Err(); err != nil {
		return step, fmt.Errorf("读取模型通道 %s 流失败: %w", c.Label(), err)
	}

	step.Content = full.String()
	for _, i := range order {
		tc := pending[i]
		if tc == nil {
			continue
		}
		// 没有函数名的分片是残缺的（网关截断/只有参数），回灌进消息列表会被
		// 上游判成非法历史。丢弃并留给轮次上限去兜底，比发出去撞 400 好。
		if strings.TrimSpace(tc.Function.Name) == "" {
			continue
		}
		step.ToolCalls = append(step.ToolCalls, *tc)
	}
	return step, nil
}

// mergeToolCallDeltas 把一个分片里的 tool_calls 归并进 pending。
//
// 三条实测规则（百炼 compatible-mode 的流式分片）：
//   - 首帧给 id 与函数名，arguments 为空串；
//   - 中间帧 id 为空串、函数名为空，只有 arguments 的一小段；
//   - 收尾帧的 arguments 是 **null**（不是空串），解到 string 得 ""，直接跳过即可。
//
// 另有一条防御：有的网关不发 index（恒为 0），那就退化成"按出现顺序追加"，
// 至少不会把两个函数的参数拼进同一个。
func mergeToolCallDeltas(pending map[int]*ToolCall, order *[]int, deltas []ToolCall) {
	for _, d := range deltas {
		idx := d.Index
		tc, ok := pending[idx]
		if !ok {
			tc = &ToolCall{Index: idx, Type: "function"}
			pending[idx] = tc
			*order = append(*order, idx)
		}
		if d.ID != "" {
			tc.ID = d.ID
		}
		if d.Type != "" {
			tc.Type = d.Type
		}
		if d.Function.Name != "" {
			tc.Function.Name = d.Function.Name
		}
		if d.Function.Arguments != "" {
			tc.Function.Arguments += d.Function.Arguments
		}
	}
}

// do 发起一次普通 POST。
func (c *Client) do(ctx context.Context, in chatRequest, out *chatResponse) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("请求模型通道 %s 失败: %w", c.Label(), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("模型通道 %s 返回 HTTP %d: %s", c.Label(), resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析模型通道 %s 响应失败: %w", c.Label(), err)
	}
	if out.Error != nil {
		return fmt.Errorf("模型通道 %s 报错: %s", c.Label(), out.Error.Message)
	}
	return nil
}

// auth 按需加上 Bearer 头。内网自建通道常免鉴权，此时不加头。
func (c *Client) auth(req *http.Request) {
	if key := strings.TrimSpace(c.cfg.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// modelName 取模型名。未指定时给一个保守的默认值，避免请求体缺字段直接 400。
func (c *Client) modelName() string {
	if m := strings.TrimSpace(c.cfg.Model); m != "" {
		return m
	}
	return "gpt-3.5-turbo"
}
