package integration

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/2777484478/yunnian/internal/config"
)

// ErrTagNotFound 表示仓库**明确回答**"没有这个 tag"（HTTP 404）。
//
// 必须与"网络不通 / 权限不足 / 超时"严格区分：只有 404 才能用来判定
// "这次发布注定失败"。其余错误重试一次可能就好了 —— 拿它们去拦发布，
// 等于把仓库抖动变成"不能发版"，比放过一个坏 tag 更糟。
// 调用方用 errors.Is(err, ErrTagNotFound) 判定。
var ErrTagNotFound = errors.New("镜像 tag 在仓库中不存在")

// 本文件实现 Docker Registry v2（OCI Distribution）客户端，用来从镜像仓库**读回**元数据。
//
// 为什么不靠 Harbor 的 Core 管理接口拿这些：
// 本文件只需要 OCI 规范里的东西（manifest / digest / 层大小），
// /v2/ 这套 Harbor 的 registry 组件、原生 Docker Registry、Nexus 都实现，换仓库不用改代码。
//
// ⚠️ 早期本文件头曾断言"实测某内网 Harbor 只对外暴露了 registry 组件，
// /api/v2.0/systeminfo 全是 nginx 404" —— **那个结论是错的**。
// 2026-10-14 复测清楚了：那台 Harbor 是 **v1.1.2**，/api/v2.0/* 404 是因为版本太老，
// 它的 Core 接口在 **/api/**（`/api/systeminfo` 返回 200）。Core 客户端见 harbor_core.go，
// 已按 v1/v2 自动识别。所以"404"≠"这个端口上没有管理接口"，别再据此下结论。
//
// 刻意不提供的能力（做不到就是做不到，不编）：
//   - **"最新 tag"**：tags/list **不保证任何顺序**（实测 v1.1.2 返回的是乱序），
//     只有 Core 接口才可能带上时间语义。所以不提供 LatestTag。
//   - **推送时间**：registry v2 拿不到，Resolve 返回的 PushedAt 恒为零值。

// RegistryV2 通过 /v2/ 接口读镜像元数据。
type RegistryV2 struct {
	base     string // https://host:port，不带尾斜杠
	username string
	password string
	client   *http.Client
	// acceptHosts 本实现"认得"的仓库地址（host[:port] 归一化后）。
	// 见 Accepts：不是同一个仓库就不能去问，否则会对错东西。
	acceptHosts []string
}

// manifestAccept 一次请求同时接受四种 manifest 类型。
//
// 列表类（manifest list / OCI index）必须一起列出来，否则多架构镜像会拿到
// 一个没有 layers 的文档，大小算出来是 0 —— 静默算错，比报错更难查。
const manifestAccept = "application/vnd.docker.distribution.manifest.v2+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.oci.image.index.v1+json"

// NewHarborRegistry 构造镜像仓库客户端。
//
// 名字保留 "Harbor" 是因为配置与界面口径都叫 Harbor；实现走的是通用 registry 协议。
func NewHarborRegistry(cfg config.HarborConfig) *RegistryV2 {
	timeout := cfg.Timeout.D()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if cfg.Insecure {
		// 自签证书的内网仓库很常见，这里按配置放行
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	// 能接受哪些"仓库地址"写法，见 Accepts。
	var hosts []string
	for _, raw := range []string{cfg.Registry, cfg.BaseURL} {
		h := normalizeRegistryHost(raw)
		if h == "" {
			continue
		}
		dup := false
		for _, x := range hosts {
			if x == h {
				dup = true
				break
			}
		}
		if !dup {
			hosts = append(hosts, h)
		}
	}
	return &RegistryV2{
		// 削掉误填进来的 `/v2` 尾巴（界面字段标着"走 /v2/"，很容易连后缀一起贴），
		// 逻辑与 Core 客户端共用一份：见 stripHarborEntrySuffix。
		base:        stripHarborEntrySuffix(cfg.BaseURL),
		username:    cfg.Username,
		password:    cfg.Password,
		client:      &http.Client{Timeout: timeout, Transport: transport},
		acceptHosts: hosts,
	}
}

// Name 实现名，用于在接口响应里标注数据来源。
func (r *RegistryV2) Name() string { return "harbor-registry-v2" }

// Base 本客户端实际在问的地址（已去掉误填的 `/v2` 尾巴与尾斜杠）。
//
// 状态页与测试结论必须报**实际探测的地址**：用户填了 `…:5000/v2` 时，
// 若照配置原文回显"…:5000/v2 连通"，看起来像是那个路径被真的用上了。
func (r *RegistryV2) Base() string { return r.base }

// Accepts 判断一个仓库地址是不是本实现管的那一个。
//
// 为什么必须有这道闸：登记的镜像记录里带着**日志里那串地址的 registry 部分**，
// 它未必是配置里的这个仓库（可能是别的仓库、镜像加速站，也可能是脏数据）。
// 不比对就去问，最坏的情况是"同名仓库恰好存在" —— 拿回来的 digest 会被挂到
// 一次完全不相干的推送记录上。那种错不报任何错，只是数据悄悄变错。
func (r *RegistryV2) Accepts(registry string) bool {
	got := normalizeRegistryHost(registry)
	if got == "" {
		return false
	}
	for _, want := range r.acceptHosts {
		if got == want {
			return true
		}
	}
	return false
}

// normalizeRegistryHost 把各种写法归一成 host[:port] 的小写形式，用于比较。
//
// 同一个仓库在不同地方有三种写法：项目配置里是 `https://host:5000`，
// image_record 里是 `host:5000`，完整镜像串里是 `host:5000/proj/repo:tag`。
// 不归一的话同一个仓库会被判成三个，比对直接失效。
func normalizeRegistryHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	// 只取第一个斜杠之前：后面是路径（项目/仓库），不属于 registry 标识
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// Ping 探 /v2/ 根路径。
//
// 200（无需鉴权）与 401（要鉴权但服务在）**都算连通** ——
// 401 恰恰证明仓库在正常工作、只是要先换 token；把它判成失败会让状态页永远报红。
func (r *RegistryV2) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return UpstreamError("Harbor", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnauthorized:
		return nil
	default:
		return UpstreamError("Harbor", fmt.Errorf("GET /v2/ 返回 HTTP %d", resp.StatusCode))
	}
}

// ListTags 列出仓库里的 tag 名。
//
// 只返回名字，digest / 大小留空：那要给每个 tag 各发两三个请求，列表场景不值当，
// 需要明细时对具体 tag 调 Resolve。
// **没有排序语义**：不要拿它取"最新"，原因见文件头。
func (r *RegistryV2) ListTags(ctx context.Context, repo string, limit int) ([]TagInfo, error) {
	repo = strings.Trim(repo, "/")
	if r.base == "" || repo == "" {
		return nil, nil
	}
	token, err := r.tokenFor(ctx, repo, "pull")
	if err != nil {
		return nil, err
	}
	var out struct {
		Tags []string `json:"tags"`
	}
	path := fmt.Sprintf("/v2/%s/tags/list", escapeRepoPath(repo))
	if err := r.getJSON(ctx, path, token, &out); err != nil {
		return nil, err
	}
	tags := make([]TagInfo, 0, len(out.Tags))
	for _, t := range out.Tags {
		tags = append(tags, TagInfo{Repo: repo, Tag: t})
	}
	// limit 只做截断，不代表"最新 N 个"
	if limit > 0 && len(tags) > limit {
		tags = tags[:limit]
	}
	return tags, nil
}

// Resolve 取某个 tag 的确定信息：digest、镜像总大小、架构。
//
// 只写仓库里读到的确定值：读不到就留空，绝不拿项目配置或当前时间去凑。
// 返回 nil 表示"这次不适用"（未配置仓库、repo/tag 为空），调用方应跳过而不是写空值。
func (r *RegistryV2) Resolve(ctx context.Context, repo, tag string) (*TagInfo, error) {
	repo = strings.Trim(repo, "/")
	tag = strings.TrimSpace(tag)
	if r.base == "" || repo == "" || tag == "" {
		return nil, nil
	}
	token, err := r.tokenFor(ctx, repo, "pull")
	if err != nil {
		return nil, err
	}
	digest, body, err := r.fetchManifest(ctx, repo, tag, token)
	if err != nil {
		return nil, err
	}

	out := &TagInfo{Repo: repo, Tag: tag, Digest: digest}

	if len(body.Manifests) > 0 {
		// 多架构：大小按各平台之和（与 Harbor 界面口径一致），架构列写成 os/arch 列表。
		arches := make([]string, 0, len(body.Manifests))
		for i, m := range body.Manifests {
			if i >= maxPlatforms {
				// 平台多到不合理就停手，别把一次登记拖成几十个请求
				break
			}
			_, child, err := r.fetchManifest(ctx, repo, m.Digest, token)
			if err != nil {
				// 某个平台读不到不影响其余；宁可少算也不编
				continue
			}
			out.SizeBytes += sumLayerSizes(child.Layers)
			if s := platformString(m.Platform.OS, m.Platform.Architecture); s != "" {
				arches = append(arches, s)
			}
		}
		out.Arch = strings.Join(dedupeStrings(arches), ",")
		return out, nil
	}

	out.SizeBytes = sumLayerSizes(body.Layers)
	// 单架构清单本身不带架构信息，在 config blob 里。
	if body.Config.Digest != "" {
		out.Arch = r.archFromConfig(ctx, repo, body.Config.Digest, token)
	}
	return out, nil
}

// maxPlatforms 多架构镜像最多看几个平台，防止异常清单把登记拖成几十个请求。
const maxPlatforms = 8

// tokenFor 换一个指定动作的 bearer token。
//
// scope 必须是 `repository:<repo>:pull` 这种形式，且 service 参数要填**固定值**
// （Harbor 是 harbor-registry），填主机名会得到 `Unable to handle service: <host>`。
// 优先用 401 挑战头里的 realm/service（规范做法，换仓库不用改代码），
// 拿不到再退回 Harbor 的约定路径 /service/token。
func (r *RegistryV2) tokenFor(ctx context.Context, repo, action string) (string, error) {
	realm, service := "", ""
	if c, err := r.challenge(ctx); err == nil {
		realm, service = c.realm, c.service
	}
	if realm == "" {
		realm, service = r.base+"/service/token", "harbor-registry"
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", UpstreamError("Harbor", fmt.Errorf("认证地址 %q 非法: %w", realm, err))
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	q.Set("scope", fmt.Sprintf("repository:%s:%s", repo, action))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if r.username != "" {
		req.SetBasicAuth(r.username, r.password)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", UpstreamError("Harbor", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", UpstreamError("Harbor", fmt.Errorf("换 token 返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200)))
	}
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", UpstreamError("Harbor", fmt.Errorf("token 响应解析失败: %w", err))
	}
	token := out.Token
	if token == "" {
		token = out.AccessToken
	}
	if token == "" {
		return "", UpstreamError("Harbor", fmt.Errorf("token 响应里没有 token 字段"))
	}
	return token, nil
}

// challenge 读 /v2/ 的 401 挑战头，拿到 realm 与 service。
func (r *RegistryV2) challenge(ctx context.Context) (authChallenge, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/v2/", nil)
	if err != nil {
		return authChallenge{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return authChallenge{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return parseChallenge(resp.Header.Get("Www-Authenticate")), nil
}

// authChallenge 从认证挑战头里解析出的内容。
type authChallenge struct {
	realm   string
	service string
}

// parseChallenge 解析 `Bearer realm="...",service="..."`。
//
// 拆键值对前要先把开头的 `Bearer` 摘掉，否则第一对的 key 会变成 "bearer realm"，
// realm 永远取不到 —— 那样就只能退回硬编码路径，等于白解析。
func parseChallenge(h string) authChallenge {
	var out authChallenge
	h = strings.TrimSpace(h)
	if len(h) < len("bearer") || !strings.EqualFold(h[:len("bearer")], "bearer") {
		// Basic 或不认识的方案都不给 realm
		return out
	}
	for _, part := range strings.Split(h[len("bearer"):], ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "realm":
			out.realm = v
		case "service":
			out.service = v
		}
	}
	return out
}

// manifestBody 一次 manifest 响应里我们关心的字段。
type manifestBody struct {
	MediaType string          `json:"mediaType"`
	Config    manifestConfig  `json:"config"`
	Layers    []manifestLayer `json:"layers"`
	Manifests []manifestEntry `json:"manifests"`
}

type manifestConfig struct {
	Digest string `json:"digest"`
}

type manifestLayer struct {
	Size int64 `json:"size"`
}

type manifestEntry struct {
	Digest   string `json:"digest"`
	Platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform"`
}

// fetchManifest 取一个 manifest，返回响应头里的 digest 与解析后的 body。
//
// 用 GET 而不是 HEAD：digest 在响应头里、layers 在 body 里，一次拿齐省一趟往返。
func (r *RegistryV2) fetchManifest(ctx context.Context, repo, ref, token string) (string, manifestBody, error) {
	var body manifestBody
	path := fmt.Sprintf("/v2/%s/manifests/%s", escapeRepoPath(repo), url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path, nil)
	if err != nil {
		return "", body, err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", body, UpstreamError("Harbor", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound {
		// 404 = 仓库认得这个 repo、但没有这个 tag。这是确定性的答案，不是故障，
		// 所以单独返回哨兵错误让调用方能据此判定"必然失败"（见 ErrTagNotFound）。
		return "", body, fmt.Errorf("%w：%s:%s", ErrTagNotFound, repo, ref)
	}
	if resp.StatusCode >= 300 {
		return "", body, UpstreamError("Harbor", fmt.Errorf("读 manifest %s:%s 返回 HTTP %d: %s",
			repo, ref, resp.StatusCode, truncate(string(raw), 160)))
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", body, UpstreamError("Harbor", fmt.Errorf("manifest 解析失败: %w", err))
	}
	return resp.Header.Get("Docker-Content-Digest"), body, nil
}

// archFromConfig 从 config blob 里读架构（如 linux/amd64）。读不到返回空串。
//
// 这是"锦上添花"的一个请求：失败不影响 digest 与大小，所以一路吞错。
func (r *RegistryV2) archFromConfig(ctx context.Context, repo, digest, token string) string {
	path := fmt.Sprintf("/v2/%s/blobs/%s", escapeRepoPath(repo), url.PathEscape(digest))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path, nil)
	if err != nil {
		return ""
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return ""
	}
	var cfg struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	return platformString(cfg.OS, cfg.Architecture)
}

// getJSON 带 bearer token 读一个 JSON 响应。
func (r *RegistryV2) getJSON(ctx context.Context, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return UpstreamError("Harbor", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return UpstreamError("Harbor", fmt.Errorf("GET %s 返回 HTTP %d: %s", path, resp.StatusCode, truncate(string(raw), 160)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return UpstreamError("Harbor", fmt.Errorf("响应解析失败: %w", err))
	}
	return nil
}

// escapeRepoPath 逐段转义仓库路径，保留分隔用的斜杠。
// 与 Harbor Core API 不同：Core 把「项目/仓库」整体塞进一个路径参数，斜杠要编成 %2F；
// registry v2 的 /v2/{name}/... 里斜杠就是路径分隔符，编掉反而 404。
// 同一个 repo 名在两套接口下要两种转义方式，混用是最容易踩的坑。
func escapeRepoPath(repo string) string {
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// sumLayerSizes 镜像总大小 = 各层压缩后大小之和。
//
// 注意别拿 manifest 自己的 Content-Length 当镜像大小 —— 那是几 KB 的清单文件。
func sumLayerSizes(layers []manifestLayer) uint64 {
	var total uint64
	for _, l := range layers {
		if l.Size > 0 {
			total += uint64(l.Size)
		}
	}
	return total
}

// platformString 拼成 linux/amd64 这种形态，两端都有才算数。
func platformString(os, arch string) string {
	if os == "" || arch == "" {
		return ""
	}
	return os + "/" + arch
}

// dedupeStrings 去重并保持原顺序。
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
