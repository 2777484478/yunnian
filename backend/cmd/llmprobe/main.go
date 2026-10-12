// Command llmprobe 是一次性诊断工具：探测当前默认模型通道**是否支持工具调用**。
//
// 为什么需要它：给 AI 助手加"自己动手查集群"的能力，前提是通道能接受
// OpenAI 兼容的 `tools` + `role:"tool"` 消息。这一点**不能假设**——
// 阿里云百炼的 Responses 路径实测会拒收（400 InvalidParameter: role ... must be
// one of user,assistant,system,function），而它的 compatible-mode 虽然标称兼容，
// 是否同样受限必须实测。设计走哪条路（原生 function call / 折叠进 user 消息）
// 完全取决于这个探测结果。
//
// 用法：go run ./cmd/llmprobe
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/crypto"
)

type row struct {
	BaseURL string `gorm:"column:base_url"`
	Model   string `gorm:"column:default_model"`
	Cipher  []byte `gorm:"column:api_key_cipher"`
	IV      []byte `gorm:"column:api_key_iv"`
	Tag     []byte `gorm:"column:api_key_auth_tag"`
	Name    string `gorm:"column:name"`
	Vendor  string `gorm:"column:vendor"`
}

func post(url, key string, body any) (int, string) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	hc := &http.Client{Timeout: 90 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

func main() {
	cfg, err := config.Load("configs/config.yaml")
	if err != nil {
		fmt.Println("加载配置失败:", err)
		os.Exit(1)
	}
	sealer, err := crypto.NewSealer(cfg.Security.MasterKey, true)
	if err != nil {
		fmt.Println("初始化解密器失败:", err)
		os.Exit(1)
	}
	db, err := gorm.Open(mysql.Open(cfg.Database.DSN), &gorm.Config{})
	if err != nil {
		fmt.Println("连接数据库失败:", err)
		os.Exit(1)
	}
	var r row
	if err := db.Raw(`SELECT name, vendor, base_url, default_model,
		api_key_cipher, api_key_iv, api_key_auth_tag
		FROM ai_model_config WHERE enabled = 1
		ORDER BY is_default DESC, id LIMIT 1`).Scan(&r).Error; err != nil {
		fmt.Println("查询模型通道失败:", err)
		os.Exit(1)
	}
	key := ""
	if len(r.Cipher) > 0 {
		if p, e := sealer.Open(crypto.Sealed{Cipher: r.Cipher, IV: r.IV, AuthTag: r.Tag}); e == nil {
			key = p
		} else {
			fmt.Println("解密密钥失败:", e)
			os.Exit(1)
		}
	}
	base := strings.TrimRight(r.BaseURL, "/")
	endpoint := base + "/chat/completions"
	fmt.Printf("通道=%s vendor=%s model=%s\nendpoint=%s\nkey=%s\n\n", r.Name, r.Vendor, r.Model, endpoint, crypto.Mask(key))

	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "get_service_status",
			"description": "查询某个项目在某个环境的集群实时状态（副本数、镜像 tag、健康）",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"envKey":     map[string]any{"type": "string"},
					"projectKey": map[string]any{"type": "string"},
				},
				"required": []string{"envKey", "projectKey"},
			},
		},
	}}

	fmt.Println("========== 探测 1：请求里带 tools（Auto），看模型会不会真的发 tool_calls ==========")
	st, body := post(endpoint, key, map[string]any{
		"model": r.Model,
		"messages": []map[string]any{
			{"role": "user", "content": "帮我查一下 test 环境 demouser 服务现在健康吗"},
		},
		"tools": tools,
	})
	fmt.Printf("HTTP %d\n%s\n\n", st, truncate(body, 1800))

	fmt.Println("========== 探测 2：消息列表里带 role:\"tool\"（带完整配对），看通道收不收 ==========")
	st2, body2 := post(endpoint, key, map[string]any{
		"model": r.Model,
		"messages": []map[string]any{
			{"role": "user", "content": "帮我查一下 test 环境 demouser 服务现在健康吗"},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
				"id": "call_probe_1", "type": "function",
				"function": map[string]any{"name": "get_service_status", "arguments": `{"envKey":"test","projectKey":"demouser"}`},
			}}},
			{"role": "tool", "tool_call_id": "call_probe_1", "content": `{"ready":1,"total":2,"status":"CrashLoopBackOff"}`},
		},
		"tools": tools,
	})
	fmt.Printf("HTTP %d\n%s\n\n", st2, truncate(body2, 1800))

	fmt.Println("========== 探测 3：探测 2 的等价物——工具结果折进 user 消息 ==========")
	st3, body3 := post(endpoint, key, map[string]any{
		"model": r.Model,
		"messages": []map[string]any{
			{"role": "user", "content": "帮我查一下 test 环境 demouser 服务现在健康吗"},
			{"role": "user", "content": "（系统记录，非助手发言）工具 get_service_status 已被调用。\n参数：{\"envKey\":\"test\",\"projectKey\":\"demouser\"}\n返回：{\"ready\":1,\"total\":2,\"status\":\"CrashLoopBackOff\"}"},
		},
		"tools": tools,
	})
	fmt.Printf("HTTP %d\n%s\n\n", st3, truncate(body3, 1800))

	// 探测 4 才是设计的分水岭：能不能**流式**拿到 tool_calls。
	// 能 → 一轮请求就是一条流，用户看到的是真打字机，工具调用后接着往下流；
	// 不能 → 只能"前几轮非流式探路、最后一轮流式"，正文的首字延迟会明显变大。
	fmt.Println("========== 探测 4：流式响应里 tool_calls 长什么样（原始 SSE 帧）==========")
	raw, _ := json.Marshal(map[string]any{
		"model": r.Model,
		"messages": []map[string]any{
			{"role": "user", "content": "帮我查一下 test 环境 demouser 服务现在健康吗"},
		},
		"tools":  tools,
		"stream": true,
	})
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		fmt.Println("请求失败:", err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Printf("HTTP %d\n%s\n", resp.StatusCode, truncate(string(b), 2500))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n…（截断，共 %d 字节）", len(s))
}
