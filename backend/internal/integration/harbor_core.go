package integration

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/2777484478/yunnian/internal/config"
)

// 本文件是 Harbor 的 **Core 管理接口**（/api/v2.0/）客户端。
//
// 为什么另起一个客户端，而不是把 registry_v2.go 扩一扩：
// 两者读的是**两套互不相同的 HTTP 接口**，能拿到的东西也不同 ——
//
//	/v2/（OCI Distribution）  能拿 manifest / digest / 架构；**拿不到推送时间，也不保证 tag 顺序**
//	/api/v2.0/（Harbor Core） 能拿 tag 列表 + push_time + pusher + size；列不了别的仓库的 manifest
//
// 所以要同时接两个地址、分别探活。实测目标 Harbor 只对外暴露了 registry 组件
// （/api/v2.0/* 在 :5000 / :443 / :80 上全是 nginx 404），所以"Core 那个地址到底是哪个"
// 必须由用户在界面上填、并且**能立刻测出来** —— 见 Ping 对 404 的专门措辞。
//
// 刻意只做**读**：列项目、列 artifact/tag。推送与删除不在平台职责内
// （镜像由各个构建平台自己推），所以这里不提供任何写方法。
type HarborCore struct {
	base     string // https://host[:port]，不带尾斜杠，已含 /api/v2.0 之前的部分
	username string
	password string
	client   *http.Client
}

// HarborTag 一个 tag 及其元数据。
type HarborTag struct {
	Name string
	// Digest 是 artifact 的摘要（sha256:...）。部署时记下来，可以回答"当时拉的是哪一份"。
	Digest string
	// PushTime 推送到仓库的时间。**这是 Core 接口独有的**，/v2/ 给不出。
	PushTime time.Time
	// Pusher 推送人（Harbor 记的账号名）；匿名推送或权限不足时为空。
	Pusher string
	Size   int64
}

// HarborCoreProbe 一次 Core 接口探测的结果。
type HarborCoreProbe struct {
	OK bool
	// Version 是 Harbor 版本（systeminfo 给的 harbor_version）。
	Version string
	// AuthMode 是 Harbor 的认证模式（db_auth / ldap_auth / oidc_auth…）。
	AuthMode string
	// ProjectsVisible 当前凭据能看到多少个项目（page_size=1 时看 total）。
	ProjectsVisible int
	// Account 本次认证用的账号（配置里的用户名）。匿名可读时为空。
	Account string
	// AuthOK 账号口令是否被接受。**与 OK 分开**：能连上但账号不对是常见状态，
	// 报成一句"连接失败"会让人以为地址写错了。
	AuthOK bool
	// Message 给人看的一句话。
	Message string
}

// NewHarborCore 由一份配置构造 Core 客户端。
// 地址为空时返回 nil —— 调用方据此区分"没配 Core 地址"与"配了但连不上"。
func NewHarborCore(cfg config.HarborConfig) *HarborCore {
	base := strings.TrimRight(strings.TrimSpace(cfg.CoreAPIURL), "/")
	if base == "" {
		return nil
	}
	timeout := cfg.Timeout.D()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &HarborCore{
		base:     base,
		username: strings.TrimSpace(cfg.Username),
		password: cfg.Password,
		client:   &http.Client{Timeout: timeout, Transport: tr},
	}
}

// Name 实现名，用于「集成状态」显示"当前是谁在干活"。
func (c *HarborCore) Name() string { return "harbor-core-api" }

// Ping 探一次 Core 接口。**不做"能连上就算通"的判断**，两步都过了才算：
//
//	① /api/v2.0/systeminfo   —— 证明这个地址上确实是 Harbor Core（404 是最常见的坑）
//	② /api/v2.0/projects     —— 证明凭据被接受、且真的能读到东西
//
// 两步的失败原因必须分开说：地址填成 registry 组件（:5000）时会 404，
// 而账号错会是 401 —— 合成一句"连接失败"，用户只能靠猜。
func (c *HarborCore) Ping(ctx context.Context) (*HarborCoreProbe, error) {
	res := &HarborCoreProbe{Account: c.username}

	var info struct {
		HarborVersion string `json:"harbor_version"`
		AuthMode      string `json:"auth_mode"`
	}
	status, err := c.getJSON(ctx, "/api/v2.0/systeminfo", &info)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusNotFound:
		res.Message = fmt.Sprintf("%s 上没有 /api/v2.0/systeminfo（HTTP 404）："+
			"这通常是填成了 registry 组件地址 —— Core 管理接口一般在另一个端口/网关上，"+
			"请填 Harbor 的访问入口（如 https://harbor.example.com）", c.base)
		return res, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// 有的部署把 systeminfo 也保护起来，这时先不判死，继续用 projects 判断。
		res.Message = "systeminfo 需要鉴权，继续检查凭据"
	case status >= 400:
		res.Message = fmt.Sprintf("读取 %s/api/v2.0/systeminfo 失败：HTTP %d", c.base, status)
		return res, nil
	default:
		res.Version, res.AuthMode = info.HarborVersion, info.AuthMode
	}

	var projects struct {
		Total int `json:"total"`
	}
	status, err = c.getJSON(ctx, "/api/v2.0/projects?page_size=1", &projects)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUnauthorized:
		res.Message = "Harbor 拒绝了这组账号密码（HTTP 401），请检查用户名与密码" +
			"（建议用只读 robot 账号）"
		return res, nil
	case status == http.StatusForbidden:
		res.Message = "账号密码被接受，但没有读取项目的权限（HTTP 403）：" +
			"robot 账号请至少给「项目 › 只读」"
		return res, nil
	case status == http.StatusNotFound:
		res.Message = fmt.Sprintf("%s 上没有 /api/v2.0/projects（HTTP 404）："+
			"地址不是 Harbor Core 接口", c.base)
		return res, nil
	case status >= 400:
		res.Message = fmt.Sprintf("读取项目列表失败：HTTP %d", status)
		return res, nil
	}

	res.AuthOK = true
	res.ProjectsVisible = projects.Total
	res.OK = true
	ver := res.Version
	if ver == "" {
		ver = "版本未知"
	}
	if c.username == "" {
		res.Message = fmt.Sprintf("已连通 Harbor Core（%s，匿名只读），可见 %d 个项目", ver, res.ProjectsVisible)
	} else {
		res.Message = fmt.Sprintf("已连通 Harbor Core（%s，账号 %s），可见 %d 个项目",
			ver, c.username, res.ProjectsVisible)
	}
	return res, nil
}

// ListTags 列出某个仓库的 tag（含推送时间与推送人），按推送时间**倒序**。
//
// 分页是硬要求：Harbor 的 page_size 默认 10、最大 100，**一次拿到的不是全部**。
// 所以返回值里带 total，调用方必须自己判断"这只是第几页"，不能拿一页当"全部"。
//
// project 与 repo 必须分开给：Harbor 的 project 是路径第一段，repo 是其余部分。
// 例：镜像 `es-repo:5000/escloud/aioai` → project=escloud, repo=aioai。
func (c *HarborCore) ListTags(ctx context.Context, project, repo string, page, pageSize int) ([]HarborTag, int, error) {
	project = strings.Trim(strings.TrimSpace(project), "/")
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if project == "" || repo == "" {
		return nil, 0, fmt.Errorf("仓库路径不完整（project=%q repo=%q）", project, repo)
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 100 // Harbor 上限
	}

	// Harbor 要求 repository 名整体再编码一次（含其中的斜杠），
	// 否则形如 `escloud/sub/app` 的仓库会被当成三段路径、404。
	escapedRepo := url.PathEscape(repo)
	path := fmt.Sprintf("/api/v2.0/projects/%s/repositories/%s/artifacts?page=%d&page_size=%d&with_tag=true&sort=-push_time",
		url.PathEscape(project), escapedRepo, page, pageSize)

	var artifacts []struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
		Tags   []struct {
			Name     string `json:"name"`
			PushTime string `json:"push_time"`
		} `json:"tags"`
	}
	status, err := c.getJSON(ctx, path, &artifacts)
	if err != nil {
		return nil, 0, err
	}
	if status == http.StatusNotFound {
		return nil, 0, fmt.Errorf("Harbor 里没有 %s/%s 这个仓库（HTTP 404）", project, repo)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, 0, fmt.Errorf("读取 %s/%s 的 tag 被拒绝（HTTP %d）：检查账号权限",
			project, repo, status)
	}
	if status >= 400 {
		return nil, 0, fmt.Errorf("读取 %s/%s 的 tag 失败：HTTP %d", project, repo, status)
	}

	out := make([]HarborTag, 0, len(artifacts))
	for _, a := range artifacts {
		for _, t := range a.Tags {
			item := HarborTag{Name: t.Name, Digest: a.Digest, Size: a.Size}
			if ts, err := time.Parse(time.RFC3339, t.PushTime); err == nil {
				item.PushTime = ts
			}
			out = append(out, item)
		}
	}
	// total 取不到就不编：返回 0 会让调用方以为"没有更多了"，
	// 所以宁可用"本页条数"表达"至少还有这么多"，由调用方按 page_size 判断是否翻页。
	return out, len(out), nil
}

// getJSON 发一次 GET 并把 JSON 解到 out。返回 HTTP 状态码 ——
// 401/403/404 都是**有信息量的答案**，不能笼统当错误丢掉。
func (c *HarborCore) getJSON(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, fmt.Errorf("构造请求失败：%w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("请求 %s%s 失败：%w", c.base, path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil // 状态码本身就是结论，交给调用方翻译
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("解析 %s 的响应失败（不是 Harbor JSON？）：%w", path, err)
	}
	return resp.StatusCode, nil
}

// SplitImageRepo 把镜像路径拆成 (project, repo)。
//
// `es-repo:5000/escloud/aioai` → (escloud, aioai)；
// `es-repo:5000/escloud/sub/aioai` → (escloud, sub/aioai)。
// 少于两段（没有项目名）时返回错误，而不是猜一个 —— 猜错会去读别的项目的仓库。
func SplitImageRepo(imagePath string) (string, string, error) {
	p := strings.Trim(strings.TrimSpace(imagePath), "/")
	if i := strings.IndexByte(p, ':'); i >= 0 && !strings.Contains(p[:i], "/") {
		p = p[i+1:] // 去掉 registry 的 host:port
	}
	i := strings.IndexByte(p, '/')
	if i <= 0 || i == len(p)-1 {
		return "", "", fmt.Errorf("镜像路径 %q 里没有「项目/仓库」两段，无法定位 Harbor 仓库", imagePath)
	}
	return p[:i], p[i+1:], nil
}

// ParseHarborInt 便于调用方把 Harbor 返回的数字字段（有时是字符串）转成 int。
func ParseHarborInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	default:
		return 0
	}
}
