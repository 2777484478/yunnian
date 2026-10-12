package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 下面这一串是**真实抓到的**响应帧（阿里云百炼 compatible-mode，
// 2026-10-09 用 cmd/llmprobe 探测 4 采的原文，未改一个字节）。
//
// 用原文而不是手写"理想帧"：这段代码要治的恰恰是真实协议的怪癖 ——
// id 只在首帧出现、arguments 分片到达、收尾一帧 arguments 是 null。
// 手写的理想帧会让测试绿着、线上错着。
const realStreamingToolCallSSE = `data: {"model":"qwen-plus","id":"chatcmpl-x","created":1,"object":"chat.completion.chunk","usage":null,"choices":[{"logprobs":null,"index":0,"delta":{"content":"","role":"assistant","tool_calls":[{"index":0,"id":"call_69bc1cc10fec4aca93626d","type":"function","function":{"name":"get_service_status","arguments":""}}]},"finish_reason":null}]}

data: {"model":"qwen-plus","id":"chatcmpl-x","choices":[{"delta":{"content":"","tool_calls":[{"index":0,"id":"","type":"function","function":{"arguments":"{\"envKey"}}]},"index":0,"finish_reason":null,"logprobs":null}],"created":1,"object":"chat.completion.chunk","usage":null}

data: {"model":"qwen-plus","id":"chatcmpl-x","choices":[{"delta":{"content":"","tool_calls":[{"index":0,"id":"","type":"function","function":{"arguments":"\": \"test\", \"project"}}]},"index":0,"finish_reason":null,"logprobs":null}],"created":1,"object":"chat.completion.chunk","usage":null}

data: {"model":"qwen-plus","id":"chatcmpl-x","choices":[{"delta":{"content":"","tool_calls":[{"index":0,"id":"","type":"function","function":{"arguments":"Key\": \"demouser\"}"}}]},"index":0,"finish_reason":null,"logprobs":null}],"created":1,"object":"chat.completion.chunk","usage":null}

data: {"model":"qwen-plus","id":"chatcmpl-x","choices":[{"delta":{"tool_calls":[{"function":{"arguments":null},"index":0,"id":"","type":"function"}],"content":""},"index":0,"finish_reason":null,"logprobs":null}],"created":1,"object":"chat.completion.chunk","usage":null}

data: {"model":"qwen-plus","id":"chatcmpl-x","choices":[{"delta":{},"index":0,"finish_reason":"tool_calls","logprobs":null}],"created":1,"object":"chat.completion.chunk","usage":null}

data: [DONE]
`

// newSSEServer 起一个假通道，按给定帧原文回应。
func newSSEServer(t *testing.T, body string) (*httptest.Server, *Client) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts, New(Config{BaseURL: ts.URL + "/v1", Model: "fake", Name: "假通道"})
}

// TestChatStreamStep_RealToolCallFrames 用真实帧验证：分片能拼回一个完整的调用。
//
// 这是整条工具链路的命门：拼错一个字符，模型下一轮就会收到
// `{"envKey":"test","projectKey":"userserv` 这种半截 JSON，
// 而错误信息会指向工具参数而不是解析逻辑，极难反查。
func TestChatStreamStep_RealToolCallFrames(t *testing.T) {
	_, c := newSSEServer(t, realStreamingToolCallSSE)

	var deltas []string
	step, err := c.ChatStreamStep(context.Background(), []Message{
		{Role: "user", Content: "帮我查一下 test 环境 demouser 服务现在健康吗"},
	}, nil, func(d string) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamStep 报错: %v", err)
	}
	if step.Finish != "tool_calls" {
		t.Errorf("Finish 应为 tool_calls，得到 %q", step.Finish)
	}
	if len(step.ToolCalls) != 1 {
		t.Fatalf("应解析出 1 个工具调用，得到 %d 个", len(step.ToolCalls))
	}
	tc := step.ToolCalls[0]
	if tc.ID != "call_69bc1cc10fec4aca93626d" {
		t.Errorf("id 只在首帧出现，必须留住；得到 %q", tc.ID)
	}
	if tc.Function.Name != "get_service_status" {
		t.Errorf("函数名解析错: %q", tc.Function.Name)
	}
	if got, want := tc.Function.Arguments, `{"envKey": "test", "projectKey": "demouser"}`; got != want {
		t.Errorf("参数分片拼接错:\n  得到 %s\n  期望 %s", got, want)
	}
	// 这一轮只有工具调用、没有正文 —— 别把 arguments 分片当成正文推给用户。
	if step.Content != "" {
		t.Errorf("本轮不该有正文，得到 %q", step.Content)
	}
	if len(deltas) != 0 {
		t.Errorf("本轮不该向前端推 delta，得到 %v", deltas)
	}
}

// TestChatStreamStep_PreambleThenToolCall 模型先说话再调工具：两者都要留住。
//
// 实测常见形态：模型先说一句"我先查一下集群"，再给出 tool_calls。
// 把那句话丢掉，用户会看到回答从"根据查询结果…"开始，凭空少了一段。
func TestChatStreamStep_PreambleThenToolCall(t *testing.T) {
	const body = `data: {"choices":[{"index":0,"delta":{"content":"我先查一下集群状态。"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_service_status","arguments":"{}"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	_, c := newSSEServer(t, body)
	var got strings.Builder
	step, err := c.ChatStreamStep(context.Background(), nil, nil, func(d string) error {
		got.WriteString(d)
		return nil
	})
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if step.Content != "我先查一下集群状态。" {
		t.Errorf("正文应为前言，得到 %q", step.Content)
	}
	if got.String() != step.Content {
		t.Errorf("推给前端的增量应与累积正文一致: %q vs %q", got.String(), step.Content)
	}
	if len(step.ToolCalls) != 1 {
		t.Fatalf("应同时留住工具调用，得到 %d 个", len(step.ToolCalls))
	}
}

// TestChatStreamStep_PlainAnswerHasNoToolCalls 不带工具的普通回答不应被改动。
//
// 这条是回归护栏：加工具调用最容易的回退是"把正常回答也解析成工具轮"，
// 表现就是普通提问不再输出正文。
func TestChatStreamStep_PlainAnswerHasNoToolCalls(t *testing.T) {
	const body = `data: {"choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"，有什么可以帮你"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":42}}

data: [DONE]
`
	_, c := newSSEServer(t, body)
	step, err := c.ChatStreamStep(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(step.ToolCalls) != 0 {
		t.Errorf("不该有工具调用，得到 %+v", step.ToolCalls)
	}
	if step.Content != "你好，有什么可以帮你" {
		t.Errorf("正文错: %q", step.Content)
	}
	if step.Finish != "stop" {
		t.Errorf("Finish 应为 stop，得到 %q", step.Finish)
	}
	if step.Usage.TotalTokens != 42 {
		t.Errorf("用量应透传，得到 %d", step.Usage.TotalTokens)
	}
}

// TestMergeToolCallDeltas_ParallelCalls 并行调用多个函数时按 index 归位。
func TestMergeToolCallDeltas_ParallelCalls(t *testing.T) {
	pending := map[int]*ToolCall{}
	var order []int
	mergeToolCallDeltas(pending, &order, []ToolCall{
		{Index: 0, ID: "a", Type: "function", Function: ToolFunction{Name: "f0"}},
		{Index: 1, ID: "b", Type: "function", Function: ToolFunction{Name: "f1"}},
	})
	mergeToolCallDeltas(pending, &order, []ToolCall{
		{Index: 0, Function: ToolFunction{Arguments: `{"a":1}`}},
		{Index: 1, Function: ToolFunction{Arguments: `{"b":2}`}},
	})
	if len(order) != 2 {
		t.Fatalf("应有两个调用，得到 %d", len(order))
	}
	if pending[0].Function.Arguments != `{"a":1}` || pending[1].Function.Arguments != `{"b":2}` {
		t.Errorf("参数串位了: %+v / %+v", pending[0], pending[1])
	}
	if pending[0].ID != "a" || pending[1].ID != "b" {
		t.Errorf("id 串位了: %q / %q", pending[0].ID, pending[1].ID)
	}
}

// TestChatStreamStep_DropsCallWithoutName 残缺调用（没有函数名）必须丢弃。
//
// 网关截断时会出现"只有参数、没有函数名"的残片。把它回灌进消息列表，
// 上游会判成非法历史直接 400，整条对话就废了 —— 宁可少一轮，不能崩一轮。
func TestChatStreamStep_DropsCallWithoutName(t *testing.T) {
	const body = `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
`
	_, c := newSSEServer(t, body)
	step, err := c.ChatStreamStep(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if len(step.ToolCalls) != 0 {
		t.Errorf("没有函数名的调用应被丢弃，得到 %+v", step.ToolCalls)
	}
}
