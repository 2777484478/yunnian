package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/2777484478/yunnian/internal/config"
)

// 本文件盯的是 Harbor Core 客户端的**版本差异**：v1 与 v2 的接口路径、能力都不同，
// 而这两条路径只有真的打一遍才分得清（2026-10-14 在这上面栽过一次：
// 按 v2 写的客户端打在一台 v1.1.2 上，永远 404，还被解释成"地址填错了"）。

func harborCoreForTest(t *testing.T, base string) *HarborCore {
	t.Helper()
	c := NewHarborCore(config.HarborConfig{BaseURL: base, Timeout: config.Duration(5 * time.Second)})
	if c == nil {
		t.Fatal("NewHarborCore 返回 nil，但地址非空")
	}
	return c
}

// newV1Stub 起一个按 Harbor v1.1.2 真实形状应答的桩：
//
//	/api/v2.0/systeminfo            → 404（v1 没有 v2 接口，这是版本区别的关键证据）
//	/api/systeminfo                 → 200 + harbor_version
//	/api/search?q=                  → 项目 + 仓库（匿名可读）
//	/api/repositories/{repo}/tags   → 全部 tag + manifest（**没有推送时间**）
func newV1Stub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2.0/systeminfo", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/systeminfo", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"harbor_version": "v1.1.2", "auth_mode": "db_auth"})
	})
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"project":    []any{map[string]any{"project_id": 2, "name": "escloud", "public": 1}},
			"repository": []any{map[string]any{"repository_name": "escloud/nginx", "tags_count": 16}},
		})
	})
	mux.HandleFunc("/api/repositories/escloud/nginx/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("detail") != "1" {
			// v1 不带 detail 时只回名字数组；本客户端应当总是带 detail=1
			writeJSON(t, w, []string{"1.28.2"})
			return
		}
		writeJSON(t, w, []any{
			v1TagEntry("zui-xin", "2026-10-13T02:27:54.855081721Z"),
			v1TagEntry("jiu", "2026-06-27T04:19:03.809048389Z"),
			v1TagEntry("wu-shi-jian", ""),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// v1TagEntry 造一条 tags?detail=1 的记录。history 里刻意放两层：
// 底层是 2020 年的基础镜像层，最后一层才是这版镜像的构建时间 ——
// 取错层次会把所有镜像的时间都算成基础镜像的构建时间。
func v1TagEntry(name, created string) map[string]any {
	base, _ := json.Marshal(map[string]string{"created": "2020-01-01T00:00:00Z"})
	var last map[string]any
	if created == "" {
		last = map[string]any{}
	} else {
		top, _ := json.Marshal(map[string]string{"created": created})
		last = map[string]any{"v1Compatibility": string(top)}
	}
	return map[string]any{
		"tag": name,
		"manifest": map[string]any{
			"schemaVersion": 1,
			"history":       []any{map[string]any{"v1Compatibility": string(base)}, last},
		},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("桩写响应失败：%v", err)
	}
}

func TestHarborCoreV1_DetectsVersionAndSaysNoPushTime(t *testing.T) {
	srv := newV1Stub(t)
	c := harborCoreForTest(t, srv.URL)

	probe, err := c.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping 失败：%v", err)
	}
	if !probe.OK || !probe.AuthOK {
		t.Fatalf("应判连通：ok=%v authOk=%v msg=%s", probe.OK, probe.AuthOK, probe.Message)
	}
	if probe.APIVersion != "v1" {
		t.Errorf("应识别为 v1，实际 %q", probe.APIVersion)
	}
	if probe.Version != "v1.1.2" {
		t.Errorf("版本号应为 v1.1.2，实际 %q", probe.Version)
	}
	// 这条是重点：v1 拿不到推送时间，必须**明说**，
	// 否则下游会把构建时间当推送时间展示（编数据）。
	if !probe.NoPushTime {
		t.Error("v1 必须标记 NoPushTime=true")
	}
	// v1 匿名探测走 /api/search，项目数与仓库数各自报，别拿仓库数冒充项目数。
	if probe.ProjectsVisible != 1 {
		t.Errorf("匿名探测应看到 1 个公开项目，实际 %d", probe.ProjectsVisible)
	}
	if probe.ReposVisible != 1 {
		t.Errorf("匿名探测应看到 1 个镜像仓库，实际 %d", probe.ReposVisible)
	}
	if !strings.Contains(probe.Message, "v1.1.2") {
		t.Errorf("结论里应带版本号，实际：%s", probe.Message)
	}
}

func TestHarborCoreV1_ListTagsUsesBuildTimeAndLocalPagination(t *testing.T) {
	srv := newV1Stub(t)
	c := harborCoreForTest(t, srv.URL)

	tags, total, err := c.ListTags(context.Background(), "escloud", "nginx", 1, 10)
	if err != nil {
		t.Fatalf("ListTags 失败：%v", err)
	}
	if total != 3 {
		t.Errorf("v1 一次拿全，total 应为 3，实际 %d", total)
	}
	if len(tags) != 3 {
		t.Fatalf("应返回 3 条，实际 %d", len(tags))
	}
	// 有时间的按时间倒序在前，没时间的垫后 —— 取的是**最后一层**的 created，
	// 不是 2020 年那一层。
	if tags[0].Name != "zui-xin" || tags[1].Name != "jiu" || tags[2].Name != "wu-shi-jian" {
		t.Errorf("排序不符（应为 有时间的倒序在前、没时间的垫后）：%v", tagNames(tags))
	}
	want := time.Date(2026, 10, 13, 2, 27, 54, 855081721, time.UTC)
	if !tags[0].Time.Equal(want) {
		t.Errorf("时间应取最后一层 created=%s，实际 %s", want, tags[0].Time)
	}
	if tags[0].TimeSource != HarborTimeBuild {
		t.Errorf("v1 的时间来源必须是 build，实际 %q", tags[0].TimeSource)
	}
	if !tags[2].Time.IsZero() || tags[2].TimeSource != "" {
		t.Errorf("没有 history 的 tag 不该编出时间：%s / %q", tags[2].Time, tags[2].TimeSource)
	}
	// v1 的 tags 接口给不出 digest —— 那就是 registry 通道的活。
	if tags[0].Digest != "" {
		t.Errorf("v1 不该凭空给出 digest，实际 %q", tags[0].Digest)
	}

	// 本地分页：total 仍是全量，翻页切的是本地切片。
	page2, total2, err := c.ListTags(context.Background(), "escloud", "nginx", 2, 2)
	if err != nil {
		t.Fatalf("ListTags 第二页失败：%v", err)
	}
	if total2 != 3 {
		t.Errorf("翻页后 total 应仍是全量 3，实际 %d", total2)
	}
	if len(page2) != 1 || page2[0].Name != "wu-shi-jian" {
		t.Errorf("第二页应只剩最后一条，实际 %v", tagNames(page2))
	}
}

// newV2Stub 起一个按 Harbor v2 形状应答的桩。
func newV2Stub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2.0/systeminfo", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"harbor_version": "v2.11.0", "auth_mode": "db_auth"})
	})
	mux.HandleFunc("/api/v2.0/projects", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"total": 4})
	})
	mux.HandleFunc("/api/v2.0/projects/escloud/repositories/aioai/artifacts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []any{
			map[string]any{"digest": "sha256:bb", "size": 200,
				"tags": []any{map[string]any{"name": "1.1", "push_time": "2026-10-14T03:00:00Z"}}},
			map[string]any{"digest": "sha256:aa", "size": 100,
				"tags": []any{map[string]any{"name": "1.0", "push_time": "2026-10-14T02:00:00Z"}}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHarborCoreV2_DetectsVersionAndKeepsPushTime(t *testing.T) {
	srv := newV2Stub(t)
	c := harborCoreForTest(t, srv.URL)

	probe, err := c.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping 失败：%v", err)
	}
	if !probe.OK || probe.APIVersion != "v2" {
		t.Fatalf("应识别为 v2 且连通：ok=%v ver=%q msg=%s", probe.OK, probe.APIVersion, probe.Message)
	}
	if probe.NoPushTime {
		t.Error("v2 有推送时间，不该标 NoPushTime")
	}
	if probe.ProjectsVisible != 4 {
		t.Errorf("项目数应为 4，实际 %d", probe.ProjectsVisible)
	}

	tags, _, err := c.ListTags(context.Background(), "escloud", "aioai", 1, 100)
	if err != nil {
		t.Fatalf("ListTags 失败：%v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("应返回 2 条，实际 %d", len(tags))
	}
	if tags[0].Name != "1.1" || tags[0].TimeSource != HarborTimePush {
		t.Errorf("v2 的时间来源应是 push：%s / %q", tags[0].Name, tags[0].TimeSource)
	}
	if tags[0].Digest != "sha256:bb" || tags[0].Size != 200 {
		t.Errorf("v2 应带回 digest 与大小：%+v", tags[0])
	}
}

func TestHarborCore_NotAHarborEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	probe, err := harborCoreForTest(t, srv.URL).Ping(context.Background())
	if err != nil {
		t.Fatalf("404 不该当成调用错误返回：%v", err)
	}
	if probe.OK {
		t.Error("两个路径都 404 时不能判连通")
	}
	// 措辞必须同时给出"这不是入口"和"你填的哪个地址"，否则用户只能猜。
	if !strings.Contains(probe.Message, "404") || !strings.Contains(probe.Message, srv.URL) {
		t.Errorf("结论应指出 404 与所测地址，实际：%s", probe.Message)
	}
}

func TestNewHarborCore_FallsBackToRegistryAddress(t *testing.T) {
	// 留空 Core 时按 registry 地址探 —— 实测那台 Harbor 两条通道都在 :5000。
	got := NewHarborCore(config.HarborConfig{BaseURL: "https://es-repo.example.com:5000/"})
	if got == nil || got.Base() != "https://es-repo.example.com:5000" {
		t.Fatalf("应回退到 BaseURL 并去掉尾斜杠，实际 %v", got)
	}
	// 显式填了就以它为准（registry 走独立端口的部署）。
	got = NewHarborCore(config.HarborConfig{
		BaseURL: "https://es-repo.example.com:5000", CoreAPIURL: "https://harbor.example.com",
	})
	if got.Base() != "https://harbor.example.com" {
		t.Errorf("显式 Core 地址应优先，实际 %q", got.Base())
	}
	// 都没填 → nil，调用方据此区分"没配"与"配了但连不上"。
	if NewHarborCore(config.HarborConfig{}) != nil {
		t.Error("两个地址都为空时应返回 nil")
	}
}

func TestHarborTime_AcceptsRFC3339AndUnixSeconds(t *testing.T) {
	if ts, ok := harborTime(json.RawMessage(`"2026-10-14T02:00:00Z"`)); !ok || !ts.Equal(time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("RFC3339 解析失败：%s / %v", ts, ok)
	}
	if ts, ok := harborTime(json.RawMessage(`1750000000`)); !ok || ts.Unix() != 1750000000 {
		t.Errorf("unix 秒解析失败：%s / %v", ts, ok)
	}
	if _, ok := harborTime(json.RawMessage(`""`)); ok {
		t.Error("空串不该被解析成时间")
	}
	if _, ok := harborTime(nil); ok {
		t.Error("nil 不该被解析成时间")
	}
}

func TestBuiltAtFromV1Manifest_HandlesSchemaV2Shape(t *testing.T) {
	// schemaVersion=2 的 history 是直接一个 created 字段，不在 v1Compatibility 里。
	raw := json.RawMessage(`{"schemaVersion":2,"history":[{"created":"2025-01-01T00:00:00Z"},{"created":"2026-10-14T05:00:00Z"}]}`)
	ts, ok := builtAtFromV1Manifest(raw)
	if !ok || !ts.Equal(time.Date(2026, 10, 14, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("应取最后一层的 created，实际 %s / %v", ts, ok)
	}
	if _, ok := builtAtFromV1Manifest(json.RawMessage(`{"schemaVersion":1}`)); ok {
		t.Error("没有 history 时不该返回时间")
	}
	if _, ok := builtAtFromV1Manifest(json.RawMessage(`不是 JSON`)); ok {
		t.Error("非 JSON 不该返回时间")
	}
	// 最后一层没有 created、更早的层有 —— **不许向前兜底**：
	// 那会把基础镜像的构建时间（下面这条是 2020 年）当成这版镜像的时间。
	raw = json.RawMessage(`{"schemaVersion":1,"history":[{"v1Compatibility":"{\"created\":\"2020-01-01T00:00:00Z\"}"},{}]}`)
	if ts, ok := builtAtFromV1Manifest(raw); ok {
		t.Errorf("最后一层没有时间时不该拿更早的层顶替，实际 %s", ts)
	}
	// Docker 对某些镜像会写 0001-01-01，那是垃圾值，宁可给空。
	raw = json.RawMessage(`{"schemaVersion":2,"history":[{"created":"0001-01-01T00:00:00Z"}]}`)
	if ts, ok := builtAtFromV1Manifest(raw); ok {
		t.Errorf("早于纪元的时间应判为无效，实际 %s", ts)
	}
}

func TestStripHarborEntrySuffix(t *testing.T) {
	cases := map[string]string{
		"https://harbor.example.com/api/v2.0": "https://harbor.example.com",
		"https://harbor.example.com/api":      "https://harbor.example.com",
		"https://harbor.example.com/api/":     "https://harbor.example.com",
		// registry 字段也走同一个归一化：/v2 尾巴要削
		// （Core 从 BaseURL 回退时会继承它，不削就把 Core 探测一起带坏）。
		"https://es-repo.example.com:5000/v2":  "https://es-repo.example.com:5000",
		"https://es-repo.example.com:5000/v2/": "https://es-repo.example.com:5000",
		"https://es-repo.example.com:5000":     "https://es-repo.example.com:5000",
		"https://es-repo.example.com:5000/":    "https://es-repo.example.com:5000",
		// ⚠️ 网关子路径必须保留：无脑切最后一个斜杠会把 /harbor 一起切掉。
		"https://gw.example.com/harbor/api":      "https://gw.example.com/harbor",
		"https://gw.example.com/harbor/api/v2.0": "https://gw.example.com/harbor",
		"https://gw.example.com/harbor/v2":       "https://gw.example.com/harbor",
		"https://gw.example.com/harbor":          "https://gw.example.com/harbor",
		// 只是名字里含 api 的路径不该被动。
		"https://gw.example.com/harbor-api": "https://gw.example.com/harbor-api",
		"":                                  "",
	}
	for in, want := range cases {
		if got := stripHarborEntrySuffix(in); got != want {
			t.Errorf("stripHarborEntrySuffix(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 用户把界面提示里的"（走 /api/v2.0/）"当真、连后缀一起贴进来时也必须能用 ——
// 否则现象是"地址明明就是它给的，却报这不是 Harbor 入口"。
func TestHarborCore_ToleratesPastedAPISuffix(t *testing.T) {
	v1srv, v2srv := newV1Stub(t), newV2Stub(t)

	c1 := NewHarborCore(config.HarborConfig{CoreAPIURL: v1srv.URL + "/api/v2.0"})
	if c1.Base() != v1srv.URL {
		t.Fatalf("应削掉后缀，实际 %q", c1.Base())
	}
	if probe, err := c1.Ping(context.Background()); err != nil || probe.APIVersion != "v1" {
		t.Fatalf("带后缀仍应识别出 v1：err=%v ver=%q", err, probe.APIVersion)
	}

	// v2 那一侧同理（v1 与 v2 两条路径都要能被识别）。
	c2 := NewHarborCore(config.HarborConfig{BaseURL: v2srv.URL + "/api"})
	if c2.Base() != v2srv.URL {
		t.Fatalf("应削掉后缀，实际 %q", c2.Base())
	}
	probe, err := c2.Ping(context.Background())
	if err != nil || probe.APIVersion != "v2" || probe.NoPushTime {
		t.Fatalf("带后缀仍应识别出 v2：err=%v ver=%q noPushTime=%v", err, probe.APIVersion, probe.NoPushTime)
	}

	// Core 地址留空时会**继承 registry 地址**，包括它误填的 `/v2` 尾巴。
	// 实测踩到：registry 填 `…:5000/v2` 时 registry 通道绿、Core 通道却报"不是 Harbor 入口"。
	c3 := NewHarborCore(config.HarborConfig{BaseURL: v1srv.URL + "/v2"})
	if c3.Base() != v1srv.URL {
		t.Fatalf("回退时也要削掉 /v2，实际 %q", c3.Base())
	}
	if probe3, err := c3.Ping(context.Background()); err != nil || probe3.APIVersion != "v1" {
		t.Fatalf("回退带 /v2 仍应识别出 v1：err=%v ver=%q", err, probe3.APIVersion)
	}
}

func tagNames(tags []HarborTag) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, t.Name)
	}
	return out
}
