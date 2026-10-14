package integration

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/2777484478/yunnian/internal/config"
)

// 本文件是 Harbor 的 **Core 管理接口**客户端。
//
// 为什么另起一个客户端，而不是把 registry_v2.go 扩一扩：
// 两者读的是**两套互不相同的 HTTP 接口**，能拿到的东西也不同 ——
//
//	/v2/（OCI Distribution）  能拿 manifest / digest / 架构；拿不到 tag 的推送时间
//	/api/…（Harbor Core）     能拿 tag 列表；v2 还带推送时间与大小，但列不了别的仓库的 manifest
//
// ⚠️ **Core 接口有两个互不兼容的大版本，路径与能力都不一样**：
//
//	Harbor v2.x  /api/v2.0/…  artifacts?with_tag=true&sort=-push_time
//	                          → tag + **推送时间** + digest + 大小；支持分页
//	Harbor v1.x  /api/…       repositories/{repo}/tags?detail=1
//	                          → 一次返回**全部** tag + manifest；**没有推送时间**，
//	                            只能从 manifest.history 里推出**镜像构建时间**
//
// 版本**自动识别**（见 detect），调用方不必关心路径；但**不能假设"一定有推送时间"**，
// 所以 HarborTag 带 TimeSource，界面必须按它措辞 ——
// 在 v1 上把构建时间显示成"推送时间"就是编数据。
//
// 实测目标仓库为 **Harbor v1.1.2**：/api/v2.0/* 全是 404，/api/systeminfo 返回 200 且
// harbor_version=v1.1.2。（早期 registry_v2.go 文件头把它判断成"只暴露了 registry 组件"，
// 那是错的 —— 真正的区别是版本太老，没有 v2 接口。）
//
// 刻意只做**读**：列项目、列 tag。推送与删除不在平台职责内
// （镜像由各个构建平台自己推），所以这里不提供任何写方法。
type HarborCore struct {
	base string // https://host[:port]，不带尾斜杠；Core 与 registry 可以同端口
	// apiVersion 探测结果："v1" / "v2"；空 = 还没探过（ListTags 会先探一次）。
	apiVersion string
	username   string
	password   string
	client     *http.Client
}

// HarborTag 的时间来源取值。**界面必须按它措辞**，见 HarborTag.TimeSource。
const (
	// HarborTimePush 时间来自 Harbor v2 的 push_time —— 是真的推送时间。
	HarborTimePush = "push"
	// HarborTimeBuild 时间只能取到镜像构建时间（Harbor v1 的上限）。
	HarborTimeBuild = "build"
)

// HarborTag 一个 tag 及其元数据。
type HarborTag struct {
	Name string
	// Digest 只有 Harbor v2 的 artifacts 接口给得出；v1 上为空
	// （那时 digest 要走 registry 通道的 Resolve —— 这正是两条通道并存的原因之一）。
	Digest string
	// Time 与 TimeSource **必须一起看**。
	Time time.Time
	// TimeSource 说明 Time 是什么时间：HarborTimePush / HarborTimeBuild / ""（拿不到）。
	TimeSource string
	Size       int64
}

// HarborCoreProbe 一次 Core 接口探测的结果。
type HarborCoreProbe struct {
	OK bool
	// APIVersion 探测到的 Core 接口版本："v1" / "v2"。
	APIVersion string
	// Version 是 Harbor 版本（systeminfo 给的 harbor_version，如 v1.1.2）。
	Version string
	// AuthMode 是 Harbor 的认证模式（db_auth / ldap_auth / oidc_auth…）。
	AuthMode string
	// ProjectsVisible 当前凭据能看到多少个项目。
	ProjectsVisible int
	// ReposVisible 可见的镜像仓库数 —— 只有 v1 匿名探测给得出（走 /api/search）。
	ReposVisible int
	// Account 本次认证用的账号（配置里的用户名）。匿名可读时为空。
	Account string
	// AuthOK 账号口令是否被接受。**与 OK 分开**：能连上但账号不对是常见状态，
	// 报成一句"连接失败"会让人以为地址写错了。
	AuthOK bool
	// NoPushTime 这个 Harbor 版本给不出 tag 的推送时间（v1 就是）。
	// 调用方据此在界面上把"最新"的判据说清楚，别假装有推送时间。
	NoPushTime bool
	// Message 给人看的一句话。
	Message string
}

// NewHarborCore 由一份配置构造 Core 客户端。
//
// 地址优先用 CoreAPIURL；**留空则退回 BaseURL**。这不是偷懒：
// Harbor 的常规部署本来就是一个入口同时提供两套协议 ——
// v2 的 Harbor 在同一端口上挂 /v2/ 与 /api/v2.0/，
// 而实测那台 v1.1.2 更是只有一个 nginx 同时挂 /v2/ 与 /api/（都在 :5000）。
// 所以第二个地址是**可选的覆盖项**（给"registry 走独立端口/镜像站"的部署用），不是必填。
//
// 两个都为空时返回 nil —— 调用方据此区分"没配地址"与"配了但连不上"。
func NewHarborCore(cfg config.HarborConfig) *HarborCore {
	base := strings.TrimRight(strings.TrimSpace(cfg.CoreAPIURL), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	}
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

// Base 本客户端实际在问的地址（CoreAPIURL 或退回的 BaseURL）。
func (c *HarborCore) Base() string { return c.base }

// APIVersion 已探测到的接口版本；未探测时为空。
func (c *HarborCore) APIVersion() string { return c.apiVersion }

// harborSysInfo /api[/v2.0]/systeminfo 里我们关心的字段。
type harborSysInfo struct {
	HarborVersion string `json:"harbor_version"`
	AuthMode      string `json:"auth_mode"`
}

// apiPath 当前版本的接口前缀。
func (c *HarborCore) apiPath() string {
	if c.apiVersion == "v1" {
		return "/api"
	}
	return "/api/v2.0"
}

// detect 判断这个地址上的 Harbor Core 是哪一代，顺带把 systeminfo 一起带回来。
//
// 顺序是先 v2 后 v1：v2 上 /api/systeminfo 是 404、v1 上 /api/v2.0/systeminfo 是 404，
// 两者不冲突；v2 是当前主流，先问它能少一次往返。
//
// 只有**两个路径都 404** 才判"这不是 Harbor 管理接口"。401/403 也算"这个版本在"：
// 有的部署把 systeminfo 也保护起来，那时"需要鉴权"是结论、不是地址错。
func (c *HarborCore) detect(ctx context.Context) (string, harborSysInfo, error) {
	var info harborSysInfo
	if c.apiVersion != "" {
		status, err := c.getJSON(ctx, c.apiPath()+"/systeminfo", &info)
		if err != nil {
			return "", info, err
		}
		if status >= 400 {
			return "", info, fmt.Errorf("读取 %s%s/systeminfo 失败：HTTP %d", c.base, c.apiPath(), status)
		}
		return c.apiVersion, info, nil
	}

	if ok, status, err := c.systemInfoAt(ctx, "/api/v2.0", &info); err != nil {
		return "", info, err
	} else if ok {
		c.apiVersion = "v2"
		return "v2", info, nil
	} else if status == http.StatusNotFound {
		// 继续问 v1
	} else {
		// v2 路径上返回了别的错（500 之类）—— 不要顺着去猜 v1，那会把一次故障
		// 翻译成"版本是 v1"，然后所有后续请求都打错路径。
		return "", info, fmt.Errorf("读取 %s/api/v2.0/systeminfo 失败：HTTP %d", c.base, status)
	}

	var v1info harborSysInfo
	if ok, _, err := c.systemInfoAt(ctx, "/api", &v1info); err != nil {
		return "", v1info, err
	} else if ok {
		c.apiVersion = "v1"
		return "v1", v1info, nil
	}
	return "", v1info, nil
}

// systemInfoAt 问一次某个前缀下的 systeminfo。
// 返回的 status 只在**不 ok** 时才有意义（调用方要据此区分 404 与故障）。
func (c *HarborCore) systemInfoAt(ctx context.Context, prefix string, out *harborSysInfo) (bool, int, error) {
	status, err := c.getJSON(ctx, prefix+"/systeminfo", out)
	if err != nil {
		return false, status, err
	}
	switch status {
	case http.StatusOK:
		return true, status, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		// 版本在，只是要鉴权；凭据是否被接受交给后面的读探测判断。
		return true, status, nil
	default:
		return false, status, nil
	}
}

// Ping 探一次 Core 接口。**不做"能连上就算通"的判断**，两步都过了才算：
//
//	① systeminfo        —— 证明这个地址上确实是 Harbor Core，并定出 v1 还是 v2（404 是最常见的坑）
//	② 读一次列表        —— 证明凭据被接受、且真的能读到东西
//
// 两步的失败原因必须分开说：地址不是 Harbor 入口会 404，账号错会是 401 ——
// 合成一句"连接失败"，用户只能靠猜。
func (c *HarborCore) Ping(ctx context.Context) (*HarborCoreProbe, error) {
	res := &HarborCoreProbe{Account: c.username}

	ver, info, err := c.detect(ctx)
	if err != nil {
		return nil, err
	}
	if ver == "" {
		res.Message = fmt.Sprintf("%s 上没有 Harbor 管理接口（/api/v2.0/systeminfo 与 /api/systeminfo 都返回 404）："+
			"要么这个地址不是 Harbor 的入口（只填了 registry 的话，把 Harbor 的访问地址补上），"+
			"要么这套仓库不是 Harbor", c.base)
		return res, nil
	}
	res.APIVersion = ver
	res.Version = info.HarborVersion
	res.AuthMode = info.AuthMode
	res.NoPushTime = ver == "v1"

	if ver == "v1" {
		return c.pingReadV1(ctx, res)
	}
	return c.pingReadV2(ctx, res)
}

// pingReadV2 用 Harbor v2 的 projects 接口验证凭据与可读性。
func (c *HarborCore) pingReadV2(ctx context.Context, res *HarborCoreProbe) (*HarborCoreProbe, error) {
	var out struct {
		Total int `json:"total"`
	}
	status, err := c.getJSON(ctx, "/api/v2.0/projects?page_size=1", &out)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUnauthorized:
		res.Message = "Harbor 拒绝了这组账号密码（HTTP 401），请检查用户名与密码（建议用只读 robot 账号）"
		return res, nil
	case status == http.StatusForbidden:
		res.Message = "账号密码被接受，但没有读取项目的权限（HTTP 403）：robot 账号请至少给「项目 › 只读」"
		return res, nil
	case status == http.StatusNotFound:
		res.Message = fmt.Sprintf("%s 上自称 v2，但没有 /api/v2.0/projects（HTTP 404）", c.base)
		return res, nil
	case status >= 400:
		res.Message = fmt.Sprintf("读取项目列表失败：HTTP %d", status)
		return res, nil
	}

	res.AuthOK, res.OK = true, true
	res.ProjectsVisible = out.Total
	res.Message = fmt.Sprintf("已连通 %s（%s，v2 接口），可见 %d 个项目：tag 带推送时间",
		harborVersionPhrase(res.Version), harborAccountPhrase(res.Account), res.ProjectsVisible)
	return res, nil
}

// pingReadV1 用 Harbor v1 的接口验证可读性。
//
// v1 的 /api/projects **必须鉴权**，没配账号时一律 401 —— 但"没配账号"不等于"地址不对"：
// 公开项目在 v1 上可以匿名读 /api/search。所以两条路分开走，
// 否则最常见的"先不配账号试一下"会被一句 401 指到错方向去。
func (c *HarborCore) pingReadV1(ctx context.Context, res *HarborCoreProbe) (*HarborCoreProbe, error) {
	if c.username == "" {
		var out struct {
			Project    []json.RawMessage `json:"project"`
			Repository []json.RawMessage `json:"repository"`
		}
		status, err := c.getJSON(ctx, "/api/search?q=", &out)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			res.Message = fmt.Sprintf("读取 %s/api/search 失败：HTTP %d", c.base, status)
			return res, nil
		}
		res.AuthOK, res.OK = true, true
		res.ProjectsVisible = len(out.Project)
		res.ReposVisible = len(out.Repository)
		res.Message = fmt.Sprintf("已连通 %s（匿名只读）：可见 %d 个公开项目 / %d 个镜像仓库%s",
			harborVersionPhrase(res.Version), res.ProjectsVisible, res.ReposVisible, harborV1NoPushTimeNote)
		return res, nil
	}

	var projects []json.RawMessage
	status, err := c.getJSON(ctx, "/api/projects", &projects)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUnauthorized:
		res.Message = "Harbor 拒绝了这组账号密码（HTTP 401），请检查用户名与密码（建议用只读账号）"
		return res, nil
	case status == http.StatusForbidden:
		res.Message = "账号密码被接受，但没有读取项目的权限（HTTP 403）：至少给它「项目 › 只读」"
		return res, nil
	case status >= 400:
		res.Message = fmt.Sprintf("读取项目列表失败：HTTP %d", status)
		return res, nil
	}

	res.AuthOK, res.OK = true, true
	res.ProjectsVisible = len(projects)
	res.Message = fmt.Sprintf("已连通 %s（账号 %s），可见 %d 个项目%s",
		harborVersionPhrase(res.Version), res.Account, res.ProjectsVisible, harborV1NoPushTimeNote)
	return res, nil
}

// harborV1NoPushTimeNote 是 v1 能力的硬边界，必须在每次"已连通 v1"的结论里出现 ——
// 否则用户会以为拿得到推送时间，然后在"最新 tag"上被一个按构建时间排的列表误导。
//
// 这段会被**原样**渲染到页面上（不是 Markdown），所以别写 `**加粗**` / `_斜体_` 这类标记，
// 那会连星号一起显示出来 —— 项目里踩过这个坑。
const harborV1NoPushTimeNote = "；注意：v1 接口不提供 tag 推送时间，" +
	"只能给出镜像构建时间（tag 名另有 registry 通道可读）"

func harborVersionPhrase(v string) string {
	if strings.TrimSpace(v) == "" {
		return "Harbor（版本未知）"
	}
	return "Harbor " + v
}

func harborAccountPhrase(account string) string {
	if strings.TrimSpace(account) == "" {
		return "匿名只读"
	}
	return "账号 " + account
}

// ListTags 列出某个仓库的 tag。
//
// project 与 repo 必须分开给：Harbor 的 project 是路径第一段，repo 是其余部分。
// 例：镜像 `es-repo:5000/escloud/aioai` → project=escloud, repo=aioai。
//
// 返回的 total 是**真实全量条数**：
//   - v1 没有服务端分页，一次拿全，total 就是全部（分页在本地切）；
//   - v2 走服务端分页，一页之外的拿不到，所以 total 只报"本页条数"
//     —— 拿一页当"全部"是这两个接口上最容易犯的错，调用方必须按返回值判断能不能翻页。
func (c *HarborCore) ListTags(ctx context.Context, project, repo string, page, pageSize int) ([]HarborTag, int, error) {
	project = strings.Trim(strings.TrimSpace(project), "/")
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if project == "" || repo == "" {
		return nil, 0, fmt.Errorf("仓库路径不完整（project=%q repo=%q）", project, repo)
	}
	if _, _, err := c.detect(ctx); err != nil {
		return nil, 0, err
	}
	switch c.apiVersion {
	case "v1":
		return c.listTagsV1(ctx, project, repo, page, pageSize)
	case "v2":
		return c.listTagsV2(ctx, project, repo, page, pageSize)
	default:
		return nil, 0, fmt.Errorf("%s 上没有 Harbor 管理接口（/api/v2.0/systeminfo 与 /api/systeminfo 都返回 404）：无法列出 tag", c.base)
	}
}

// listTagsV2 走 Harbor v2 的 artifacts 接口（带推送时间、支持服务端分页）。
func (c *HarborCore) listTagsV2(ctx context.Context, project, repo string, page, pageSize int) ([]HarborTag, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 100 // Harbor v2 的 page_size 上限
	}

	// Harbor 要求 repository 名整体再编码一次（含其中的斜杠），
	// 否则形如 `escloud/sub/app` 的仓库会被当成三段路径、404。
	path := fmt.Sprintf("/api/v2.0/projects/%s/repositories/%s/artifacts?page=%d&page_size=%d&with_tag=true&sort=-push_time",
		url.PathEscape(project), url.PathEscape(repo), page, pageSize)

	var artifacts []struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
		Tags   []struct {
			Name     string          `json:"name"`
			PushTime json.RawMessage `json:"push_time"`
		} `json:"tags"`
	}
	status, err := c.getJSON(ctx, path, &artifacts)
	if err != nil {
		return nil, 0, err
	}
	switch {
	case status == http.StatusNotFound:
		return nil, 0, fmt.Errorf("Harbor 里没有 %s/%s 这个仓库（HTTP 404）", project, repo)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, 0, fmt.Errorf("读取 %s/%s 的 tag 被拒绝（HTTP %d）：检查账号权限", project, repo, status)
	case status >= 400:
		return nil, 0, fmt.Errorf("读取 %s/%s 的 tag 失败：HTTP %d", project, repo, status)
	}

	out := make([]HarborTag, 0, len(artifacts))
	for _, a := range artifacts {
		for _, t := range a.Tags {
			if t.Name == "" {
				continue
			}
			item := HarborTag{Name: t.Name, Digest: a.Digest, Size: a.Size}
			if ts, ok := harborTime(t.PushTime); ok {
				item.Time, item.TimeSource = ts, HarborTimePush
			}
			out = append(out, item)
		}
	}
	return out, len(out), nil
}

// listTagsV1 走 Harbor v1 的 tags?detail=1（一次给全部，但没有推送时间）。
func (c *HarborCore) listTagsV1(ctx context.Context, project, repo string, page, pageSize int) ([]HarborTag, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	// v1 的 tags 接口没有分页参数（实测传 page/page_size 也被忽略），一次返回全部 ——
	// 所以这里的分页是**本地切**，total 也就是真实全量。这对调用方是好事：
	// 不会出现 v2 那种"只知道这一页有多少"的不确定。
	//
	// 仓库名要**原样带上斜杠**：v1 的 route 认整段（实测 `escloud/nginx` 与
	// `escloud%2Fnginx` 都通，但编成 %2F 属于碰巧，不依赖它）。
	path := "/api/repositories/" + project + "/" + repo + "/tags?detail=1"
	var raw []struct {
		Tag      string          `json:"tag"`
		Manifest json.RawMessage `json:"manifest"`
	}
	status, err := c.getJSON(ctx, path, &raw)
	if err != nil {
		return nil, 0, err
	}
	switch {
	case status == http.StatusNotFound:
		return nil, 0, fmt.Errorf("Harbor 里没有 %s/%s 这个仓库（HTTP 404）", project, repo)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, 0, fmt.Errorf("读取 %s/%s 的 tag 被拒绝（HTTP %d）：检查账号权限", project, repo, status)
	case status >= 400:
		return nil, 0, fmt.Errorf("读取 %s/%s 的 tag 失败：HTTP %d", project, repo, status)
	}

	out := make([]HarborTag, 0, len(raw))
	for _, t := range raw {
		if t.Tag == "" {
			continue
		}
		item := HarborTag{Name: t.Tag}
		if ts, ok := builtAtFromV1Manifest(t.Manifest); ok {
			item.Time, item.TimeSource = ts, HarborTimeBuild
		}
		out = append(out, item)
	}
	sortTagsByTime(out)

	total := len(out)
	start := (page - 1) * pageSize
	if start >= total {
		return nil, total, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return out[start:end], total, nil
}

// builtAtFromV1Manifest 从 v1 的 manifest 里取**镜像构建时间**。
//
// 取 history 的**最后一层**：schemaVersion=1 的 history 从底向上排列，
// 最后一层是构建时写下的 config 层，它的 created 就是这版镜像的构建时间。
// （实测：平台推的 `v4`，这里给出与推送同日的 02:27。）
//
// ⚠️ **只看最后一层，不向前找"第一个有时间的那层"**。
// 往前找看着更宽容，实际是把基础镜像的构建时间当成这版镜像的 ——
// 实测那台仓库里基础层是 2020 年的，会给出一个差好几年的时间。
// 这是最典型的静默错误：有值、格式对、就是错的。宁可返回 false（界面显示"—"）。
//
// 两种编码都要认：schemaVersion=1 把这一层塞在 v1Compatibility 字符串里，
// schemaVersion=2 是直接一个 created 字段。
//
// ⚠️ 它是**构建时间**，不是推送时间 —— 一份很久以前构建的镜像今天被推上来，
// 这里给的是很久以前。调用方绝不能把它当推送时间显示（见 HarborTimeBuild）。
func builtAtFromV1Manifest(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var m struct {
		History []struct {
			Created         string `json:"created"`
			V1Compatibility string `json:"v1Compatibility"`
		} `json:"history"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || len(m.History) == 0 {
		return time.Time{}, false
	}
	last := m.History[len(m.History)-1]

	var candidates []string
	if last.V1Compatibility != "" {
		var inner struct {
			Created string `json:"created"`
		}
		if err := json.Unmarshal([]byte(last.V1Compatibility), &inner); err == nil {
			candidates = append(candidates, inner.Created)
		}
	}
	candidates = append(candidates, last.Created)

	for _, c := range candidates {
		ts, err := time.Parse(time.RFC3339, c)
		if err != nil {
			continue
		}
		// 早于 Unix 纪元的时间是垃圾值（Docker 对某些镜像会写 0001-01-01），
		// 宁可给空，也不给一个看着像时间的假时间。
		if ts.Before(time.Unix(0, 0)) {
			return time.Time{}, false
		}
		return ts, true
	}
	return time.Time{}, false
}

// sortTagsByTime 把**有时间的排前面**（时间倒序），没时间的按名字倒序垫后。
//
// 为什么没时间时用名字倒序：版本号形如 1.2.0.3406，同一个前缀下倒序能把大号排前，
// 比正序更接近"新的在前"的直觉。它**只是兜底**，不代表任何时间语义 ——
// 界面必须标明这一列到底是什么。
func sortTagsByTime(tags []HarborTag) {
	sort.SliceStable(tags, func(i, j int) bool {
		a, b := tags[i], tags[j]
		ak, bk := !a.Time.IsZero(), !b.Time.IsZero()
		if ak != bk {
			return ak
		}
		if ak && !a.Time.Equal(b.Time) {
			return a.Time.After(b.Time)
		}
		return a.Name > b.Name
	})
}

// harborTime 解析 Harbor 的时间字段，两种编码都认：
// v2 的 artifacts 接口给 RFC3339 字符串（"2026-10-14T02:27:54.855Z"），
// 个别版本/网关会退化成 unix 秒。解析不出来就返回 false，绝不拿当前时间凑。
func harborTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts, true
		}
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return time.Unix(int64(n), 0), true
	}
	return time.Time{}, false
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
		return 0, UpstreamError("Harbor", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
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
