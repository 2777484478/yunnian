package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yunnian/yunnian/internal/config"
)

// ErrJobNotFound 目标 Job 在 Jenkins 上不存在（HTTP 404）。
//
// 必须与"上游故障"分开报，因为两者要用户做的事完全不同：名字写错/Job 已被删除，
// 改一个字符或去 Jenkins 上看一眼就完了；Jenkins 真的挂了，得去找运维。
// 都笼统报成"调用失败"，用户会对着一个根本不存在的 Job 反复重试、
// 翻平台的错误日志找一场不存在的故障。
var ErrJobNotFound = errors.New("jenkins job not found")

// 本文件是 Jenkins 的 HTTP 客户端（REST + Stapler JSON API），只用标准库。
//
// 与 GitLab 客户端的四处关键差异，每一处都对应一个会让人排查很久的坑：
//
//  1. **认证是「用户名 + 密钥」两个值**。Jenkins 走 HTTP Basic，只给密钥无法认证。
//     密钥既可以是 API Token，也可以是账号密码——Basic 认证两种都收。
//
//  2. **CSRF crumb 与会话绑定**。Jenkins 的默认 crumb 用会话 ID 加盐，
//     所以必须带 cookie jar：先 GET crumb 拿到会话 cookie，再用同一个会话发 POST。
//     不带 jar 会出现"crumb 取到了、用上去仍然 403"这种看起来毫无道理的现象。
//     未启用 CSRF 时 /crumbIssuer/api/json 返回 404——那不是错误，是"不需要 crumb"。
//
//  3. **触发构建拿不到构建号**。POST build 只返回 201 与一个队列项地址
//     （`/queue/item/{id}`）；构建号要等执行器空闲、job 真正开跑后才出现，
//     得再轮询队列项。所以触发接口的返回值是 queueId，不是 buildNumber。
//
//  4. **GitParameter 的可选值要单独问**。参数定义在 `property[parameterDefinitions]`，
//     但"版本号/分支"列表不在里面，要再调一次 `descriptorByName/<类名>/fillValueItems`。
//     这个接口**每次都会真的去读一遍远端仓库**（实测 0.68s、311 个值），
//     所以只能按需调，绝不能进列表页批量拉。
//
// 还有一处坑不在客户端、在调用方：`fillValueItems` 的查询串必须是 `param=<参数名>`。
// 写成 `name=<参数名>` 时 Jenkins 返回 **HTTP 200 + `values: []`** ——
// 不报错、不警告，就是空的。参数名根本不存在时也一样。见 ParamValues 的注释。

// --------------------------------------------------------------------------- 类型

// JenkinsBuildRef Jenkins 里一次构建的引用。
type JenkinsBuildRef struct {
	Number int `json:"number"`
	// Result 只有构建结束后才由 Jenkins 填写；构建中为 null。
	Result    *string `json:"result"`
	Building  bool    `json:"building"`
	Timestamp int64   `json:"timestamp"`
	Duration  int64   `json:"duration"`
	URL       string  `json:"url"`
	// EstimatedDuration Jenkins 自己估的耗时（毫秒）。用它算进度只能是估算，
	// 所以平台最多报到 95%——估错时报 100% 会变成"明明在跑却显示已完成"。
	EstimatedDuration int64 `json:"estimatedDuration,omitempty"`
}

// JenkinsJobSnapshot 触发构建前对 job 拍的一张快照。
//
// 用途只有一个但很关键：**兜底认领构建号**。
// 老版本 Jenkins 在构建真正开跑之后会把队列项移除，此时 /queue/item/{id} 返回 404，
// 平台就必须换一条路把"这一次构建"认出来。认错比认不出更糟——
// 会把上一次的构建当成这一次，于是日志看着对、其实是别人的。
type JenkinsJobSnapshot struct {
	NextBuildNumber int              `json:"nextBuildNumber"`
	LastBuild       *JenkinsBuildRef `json:"lastBuild"`
}

// JenkinsJob Jenkins 里的一个 Job。
//
// FullName 才是触发时使用的标识：用了 Folders 插件之后 job 的唯一名是
// `backend/pay/ci-pay` 这种带文件夹前缀的路径，拿裸名去触发会 404。
type JenkinsJob struct {
	FullName string `json:"fullName"`
	Name     string `json:"name"`
	// Folder 所在文件夹路径；顶层 job 为空串。
	Folder string `json:"folder"`
	// Color Jenkins 的状态色：blue 成功 / red 失败 / yellow 不稳定 / grey 未构建，
	// 后缀 `_anime` 表示正在构建。这套颜色是反直觉的，语义以 Jenkins 为准，不要按常规理解。
	Color string `json:"color"`
	URL   string `json:"url"`
	// Parameterized 是否参数化 job。参数化的触发时走 buildWithParameters。
	Parameterized bool `json:"parameterized"`
	// ParamNames 只带参数名，用来在列表页显示"有哪几个参数"。
	// 可选值要另外去仓库读，不能在这里带（531 个 job 全读一遍是分钟级）。
	ParamNames []string `json:"paramNames,omitempty"`
	// NextBuildNumber 下一次构建的编号。只是**预期值**：并发触发时以实际为准。
	NextBuildNumber int             `json:"nextBuildNumber,omitempty"`
	LastBuild       *JenkinsBuildRef `json:"lastBuild"`
}

// Jenkins 参数类型归一化后的稳定取值。
//
// 为什么不直接把 Jenkins 返回的 Java 类名透给前端：那是插件实现细节
// （`net.uaznia.lukanus.hudson.plugins.gitparameter.GitParameterDefinition`），
// 换一个插件版本就可能变。归一化放在后端，插件升级不会把前端打散。
const (
	JenkinsParamGitBranchTag  = "git_branch_tag"
	JenkinsParamGitBranch     = "git_branch"
	JenkinsParamGitTag        = "git_tag"
	JenkinsParamGitRevision   = "git_revision"
	JenkinsParamGitPullReq    = "git_pull_request"
	JenkinsParamGitBuild      = "git_build"
	JenkinsParamChoice        = "choice"
	JenkinsParamBoolean       = "boolean"
	JenkinsParamString        = "string"
	JenkinsParamPassword      = "password"
	JenkinsParamText          = "text"
	JenkinsParamUnknown       = "unknown"
	jenkinsGitParameterClass  = "net.uaznia.lukanus.hudson.plugins.gitparameter.GitParameterDefinition"
	jenkinsMaxFolderDepth     = 8
	jenkinsProgressiveTextCap = 4 << 20 // 单次读取的日志上限，避免异常响应把内存吃满
)

// IsJenkinsGitParam 判断该参数的可选值是否需要去仓库读（而不是静态 choices）。
func IsJenkinsGitParam(kind string) bool { return strings.HasPrefix(kind, "git_") }

// JenkinsParamDef 一个构建参数的定义（**不含 GitParameter 的可选值**）。
type JenkinsParamDef struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	ClassName string `json:"className"`
	// Description GitParameter 常把"默认用哪个分支"写在这里（如「默认master」）。
	Description string `json:"description"`
	// DefaultValue ⚠️ GitParameter 这一项恒为 null：插件不通过 API 暴露配置页里的
	// defaultValue（`tree=*` 拿全字段也只有 null）。所以界面**不要假装知道默认值**，
	// 统一按"不选则不传该参数 → Jenkins 用自己配置的默认值"处理。
	DefaultValue *string `json:"defaultValue"`
	// Choices 静态选项（kind=choice 时才有）。为 null 表示这是动态取值类型。
	Choices  []string `json:"choices"`
	Multiple bool     `json:"multiple"`
}

// JenkinsParamValues GitParameter 的可选值。
//
// Tags 与 Branches 分开返回是刻意的：实测目标 job 共 311 个值，
// 其中 84 个是版本标签、227 个是 `origin/xxx` 远端分支引用。混成一个列表，
// 找版本号的人要在 311 条里翻。分成两组才符合"选版本号"这个用途。
type JenkinsParamValues struct {
	Param    string   `json:"param"`
	Tags     []string `json:"tags"`
	Branches []string `json:"branches"`
	// Other 既不像标签也不像分支的原值，兜底展示，避免静默丢值。
	Other   []string `json:"other"`
	Total   int      `json:"total"`
	Errors  []string `json:"errors"`
	FetchedAt string `json:"fetchedAt"`
	// ElapsedMs 拉取耗时。这个接口每次真的去读仓库，耗时波动很大，值得显示。
	ElapsedMs int `json:"elapsedMs"`
	Source    string `json:"source"`
}

// JenkinsBuildParam 提交给 Jenkins 的一个参数。
type JenkinsBuildParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// JenkinsProbe 一次连通性探测的结果。
//
// 一次拿版本、账号、权限、CSRF 与 job 数，是因为运维配错时最需要的不是
// "失败"两个字，而是**错在哪**：地址错 / 密码错 / 能连上但没权限拉 job。
type JenkinsProbe struct {
	Version string `json:"version"`
	// Username 凭据代表的账号。匿名访问时为空。
	Username string `json:"username"`
	// Permissions 账号权限点（取自 whoAmI 的 authorities）。能连上但权限不足时靠它暴露。
	Permissions []string `json:"permissions"`
	// CrumbEnabled 是否启用了 CSRF 保护。启用则触发构建必须先取 crumb（平台自动处理）。
	CrumbEnabled bool `json:"crumbEnabled"`
	// JobCount 顶层 job 数。该 Jenkins 若使用了 Folders 插件，这个数**不含文件夹内**，
	// 由 Warning 明确说明——不说明的话会被当成"权限只够看到一部分"。
	JobCount int `json:"jobCount"`
	// Anonymous 未配置用户名（匿名访问）。
	Anonymous bool     `json:"anonymous"`
	Warnings  []string `json:"warnings"`
}

// JenkinsListResult 一次 Job 列表拉取的结果。
type JenkinsListResult struct {
	Jobs []JenkinsJob
	// Truncated 是否因超出上限被截断。
	Truncated bool
	// SkippedFolders 因权限不足被跳过的文件夹数量。>0 时界面要提示，
	// 否则用户会以为"我的 job 没了"。
	SkippedFolders int
}

// --------------------------------------------------------------------------- 客户端

// JenkinsClient 通过 Jenkins REST / Stapler JSON API 访问 Jenkins。
type JenkinsClient struct {
	base     string
	username string
	secret   string
	client   *http.Client

	mu sync.Mutex
	// crumb 缓存与 known：known=true 且 crumb=="" 表示"探测过了，这台 Jenkins 没开 CSRF"，
	// 就不必每次写请求都先去问一遍。
	crumb      string
	crumbField string
	crumbKnown bool
}

// NewJenkinsClient 构造。BaseURL 允许带 context path（如 http://host:1143/jenkins）。
func NewJenkinsClient(cfg config.JenkinsConfig) *JenkinsClient {
	timeout := cfg.Timeout.D()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	// cookie jar 不是可选项：Jenkins 默认 crumb 用会话 ID 加盐，
	// 没有 jar 就会出现"crumb 取到了、用上去仍 403"。
	jar, _ := cookiejar.New(nil)
	return &JenkinsClient{
		base:     strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		username: strings.TrimSpace(cfg.Username),
		secret:   cfg.Token,
		client: &http.Client{
			Timeout:   timeout,
			Jar:       jar,
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
		},
	}
}

// Name 实现名。
func (c *JenkinsClient) Name() string { return "jenkins" }

// Ping 连通性测试：拉一次根 api/json。
func (c *JenkinsClient) Ping(ctx context.Context) error {
	var out map[string]any
	_, err := c.get(ctx, "/api/json", nil, &out)
	return err
}

// Probe 依次探测版本、凭据身份、CSRF 与 job 数。
//
// 与 GitLab 的 Probe 同一个思路：版本与身份任一失败即判定连通性不合格
// （它们决定"能不能用"）；CSRF 与 job 数是**信息**，取不到不影响结论。
func (c *JenkinsClient) Probe(ctx context.Context) (*JenkinsProbe, error) {
	out := &JenkinsProbe{Anonymous: c.username == ""}

	// ① 根接口：版本从 X-Jenkins 响应头拿，顺手数一次顶层 job。
	resp, err := c.getRaw(ctx, "/api/json", url.Values{"tree": {"jobs[name,url,_class]"}})
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if readErr != nil {
		return nil, UpstreamError("Jenkins", readErr)
	}
	if resp.StatusCode >= 300 {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg += "（凭据被拒或权限不足：请检查用户名与密钥，以及该账号是否有 Overall/Read 权限）"
		}
		return nil, UpstreamError("Jenkins", fmt.Errorf("%s: %s", msg, truncate(string(body), 200)))
	}
	out.Version = strings.TrimSpace(resp.Header.Get("X-Jenkins"))
	var root struct {
		Jobs []struct {
			Class string `json:"_class"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, UpstreamError("Jenkins", fmt.Errorf("响应解析失败: %w", err))
	}
	out.JobCount = len(root.Jobs)
	folderSeen := false
	for _, j := range root.Jobs {
		if isJenkinsFolder(j.Class) {
			folderSeen = true
			break
		}
	}
	if folderSeen {
		out.Warnings = append(out.Warnings,
			"该 Jenkins 使用了文件夹（Folders）插件：上面的 Job 数只统计了顶层，不含文件夹内的 Job")
	}

	// ② 凭据身份。老版本 Jenkins 没有 /whoAmI，退回 /me；两者都没有就只记一条说明。
	if err := c.probeIdentity(ctx, out); err != nil {
		return nil, err
	}

	// ③ CSRF：404 表示未启用，不是故障。
	_, crumb, err := c.fetchCrumb(ctx)
	switch {
	case err != nil:
		out.Warnings = append(out.Warnings, "CSRF 状态探测失败（不影响读取，触发构建时会再试一次）："+err.Error())
	default:
		out.CrumbEnabled = crumb != ""
	}
	return out, nil
}

// probeIdentity 读取凭据代表的账号；两种接口都取不到时退化为"只知道版本"。
//
// 区分"接口不存在"（404，老版本）与"凭据无效"（401/403）：
// 前者不该让整个测试失败，后者必须失败——那正是测试要找的问题。
func (c *JenkinsClient) probeIdentity(ctx context.Context, out *JenkinsProbe) error {
	var who struct {
		Name          string   `json:"name"`
		Authenticated bool     `json:"authenticated"`
		Authorities   []string `json:"authorities"`
	}
	code, err := c.getWithCode(ctx, "/whoAmI/api/json", nil, &who)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		// 老版本 Jenkins（< 2.204 附近）没有 /whoAmI，退回读取当前用户。
		var me struct {
			ID       string `json:"id"`
			FullName string `json:"fullName"`
		}
		code, err = c.getWithCode(ctx, "/me/api/json", nil, &me)
		if err != nil {
			return err
		}
		if code == http.StatusNotFound {
			out.Warnings = append(out.Warnings,
				"该 Jenkins 版本不支持 /whoAmI 与 /me，无法自动确认凭据身份（不影响连通性结论）")
			return nil
		}
		out.Username = strings.TrimSpace(me.FullName)
		if out.Username == "" {
			out.Username = strings.TrimSpace(me.ID)
		}
		return nil
	}
	out.Username = strings.TrimSpace(who.Name)
	out.Permissions = who.Authorities
	if !who.Authenticated && out.Username == "" {
		out.Warnings = append(out.Warnings,
			"当前是匿名访问：没有配置用户名，或凭据被拒。读取 Job 与触发构建都可能因权限失败。")
	}
	return nil
}

// --------------------------------------------------------------------------- Job 列表

// jenkinsJobTree 一次请求里要拿的字段。
//
// `_class` 是必需的：凭它才能判断一个条目是普通 job 还是文件夹，
// 也才能把参数定义归一化成稳定类型。
const jenkinsJobTree = "jobs[name,url,color,_class,nextBuildNumber," +
	"lastBuild[number,result,building,timestamp,duration,url]," +
	"property[parameterDefinitions[*]]]"

// jenkinsJobRaw Jenkins 返回的 job 条目原始结构。
type jenkinsJobRaw struct {
	Class           string          `json:"_class"`
	Name            string          `json:"name"`
	URL             string          `json:"url"`
	Color           string          `json:"color"`
	NextBuildNumber int             `json:"nextBuildNumber"`
	LastBuild       *JenkinsBuildRef `json:"lastBuild"`
	Property        []struct {
		ParameterDefinitions []jenkinsParamRaw `json:"parameterDefinitions"`
	} `json:"property"`
}

// ListJobs 列出全部 job，递归进入文件夹。
//
// max<=0 表示不限。超限时置 Truncated，让界面能明说"只显示了一部分"，
// 而不是默默少给几条——后者会让人以为 job 不存在。
func (c *JenkinsClient) ListJobs(ctx context.Context, max int) (*JenkinsListResult, error) {
	out := &JenkinsListResult{Jobs: []JenkinsJob{}}
	stat := &listStat{max: max}
	if err := c.listJobsAt(ctx, "", "", 0, out, stat); err != nil {
		return nil, err
	}
	out.Truncated = stat.truncated
	out.SkippedFolders = stat.skippedFolders
	return out, nil
}

type listStat struct {
	max            int
	truncated      bool
	skippedFolders int
}

// listJobsAt 拉取某一层的 job；encodedPath 是已经转义好的 job 路径（顶层为空）。
func (c *JenkinsClient) listJobsAt(ctx context.Context, encodedPath, namePrefix string, depth int,
	out *JenkinsListResult, stat *listStat) error {
	if depth > jenkinsMaxFolderDepth {
		return nil
	}
	path := "/api/json"
	if encodedPath != "" {
		path = encodedPath + "/api/json"
	}

	var raw struct {
		Jobs []jenkinsJobRaw `json:"jobs"`
	}
	if _, err := c.get(ctx, path, url.Values{"tree": {jenkinsJobTree}}, &raw); err != nil {
		return err
	}

	for _, j := range raw.Jobs {
		fullName := j.Name
		if namePrefix != "" {
			fullName = namePrefix + "/" + j.Name
		}
		childPath := encodedPath + "/job/" + url.PathEscape(j.Name)

		if isJenkinsFolder(j.Class) {
			err := c.listJobsAt(ctx, childPath, fullName, depth+1, out, stat)
			if err != nil {
				// 文件夹读不动通常是权限问题。跳过它并把数量报上去，
				// 而不是让整个列表请求失败——一个读不到的文件夹不该让人看不到自己的 job。
				if isJenkinsDenied(err) {
					stat.skippedFolders++
					continue
				}
				return err
			}
			continue
		}
		if stat.max > 0 && len(out.Jobs) >= stat.max {
			stat.truncated = true
			return nil
		}

		job := JenkinsJob{
			FullName:        fullName,
			Name:            j.Name,
			Folder:          namePrefix,
			Color:           j.Color,
			URL:             c.rewriteJenkinsURL(j.URL),
			NextBuildNumber: j.NextBuildNumber,
			LastBuild:       j.LastBuild,
		}
		if job.LastBuild != nil {
			job.LastBuild.URL = c.rewriteJenkinsURL(job.LastBuild.URL)
		}
		for _, p := range j.Property {
			for _, pd := range p.ParameterDefinitions {
				if name := strings.TrimSpace(pd.Name); name != "" {
					job.ParamNames = append(job.ParamNames, name)
				}
			}
		}
		job.Parameterized = len(job.ParamNames) > 0
		out.Jobs = append(out.Jobs, job)

		if stat.max > 0 && len(out.Jobs) >= stat.max {
			// 还要看后面是否真的有更多 job 才能确定"被截断"，
			// 这里先标记，循环结束前若还有条目就会被置位。
			if len(raw.Jobs) > 0 {
				stat.truncated = true
			}
			return nil
		}
	}
	return nil
}

// isJenkinsFolder 判断一个条目是否是"容器型"条目（文件夹 / 多分支流水线）。
//
// 用类名包含判断而不是枚举类名：Folders 插件、OrganizationFolder、
// MultiBranchProject 的类名各不相同且会随插件版本变化，枚举一定会有漏的。
func isJenkinsFolder(class string) bool {
	return strings.Contains(class, "Folder") || strings.Contains(class, "MultiBranch")
}

// --------------------------------------------------------------------------- 参数定义

type jenkinsParamRaw struct {
	Class string `json:"_class"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	// Description GitParameter 把"默认用哪个分支"写在这里。
	Description string `json:"description"`
	// 下面三个用 RawMessage：Jenkins 各插件对同一字段的类型并不统一
	// （有的给字符串、有的给对象）。直接声明成 string 会在对象形态上解码失败，
	// 而那是**整个 job 列表**解码失败，代价太大。
	DefaultValue          json.RawMessage `json:"defaultValue"`
	DefaultParameterValue json.RawMessage `json:"defaultParameterValue"`
	Choices               json.RawMessage `json:"choices"`
	Multiple              bool            `json:"multiple"`
}

// JobParams 读取某个 job 的参数定义（不含 GitParameter 的可选值）。
//
// 轻量：纯读 job 配置，不碰远端仓库，适合构建弹窗一打开就调。
//
// Job 不存在时返回 ErrJobNotFound（而不是笼统的上游错误）——
// 服务层据此回 404，让用户知道该去改名字，而不是去查 Jenkins 的故障。
func (c *JenkinsClient) JobParams(ctx context.Context, fullName string) ([]JenkinsParamDef, error) {
	path := jobPath(fullName) + "/api/json"
	var raw struct {
		Property []struct {
			ParameterDefinitions []jenkinsParamRaw `json:"parameterDefinitions"`
		} `json:"property"`
	}
	code, err := c.getWithCode(ctx, path, url.Values{"tree": {"property[parameterDefinitions[*]]"}}, &raw)
	if err != nil {
		if code == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrJobNotFound, fullName)
		}
		return nil, err
	}
	out := []JenkinsParamDef{}
	for _, p := range raw.Property {
		for _, pd := range p.ParameterDefinitions {
			out = append(out, normalizeJenkinsParam(pd))
		}
	}
	return out, nil
}

// normalizeJenkinsParam 把插件原始结构归一化成稳定类型。
func normalizeJenkinsParam(raw jenkinsParamRaw) JenkinsParamDef {
	def := JenkinsParamDef{
		Name:        strings.TrimSpace(raw.Name),
		ClassName:   raw.Class,
		Kind:        jenkinsParamKind(raw.Class, raw.Type),
		Description: strings.TrimSpace(raw.Description),
		Choices:     rawStrings(raw.Choices),
		Multiple:    raw.Multiple,
	}
	// 默认值两种形态：GitParameter / String 直接给标量；
	// 标准参数包成 {"value": ...}。任一非空即采用，都空则保持 null。
	if v := rawScalar(raw.DefaultValue); v != "" {
		def.DefaultValue = &v
	} else if v := rawField(raw.DefaultParameterValue, "value"); v != "" {
		def.DefaultValue = &v
	}
	if len(def.Choices) == 0 {
		// 留 null 而不是空数组：前端据此区分"这是静态选项类型但没有选项"
		// 与"这是动态取值类型，要另外去问 Jenkins"。
		def.Choices = nil
	}
	return def
}

// jenkinsParamKind 把 Java 类名归一化成稳定取值。
func jenkinsParamKind(class, typ string) string {
	short := class
	if i := strings.LastIndex(class, "."); i >= 0 {
		short = class[i+1:]
	}
	switch short {
	case "GitParameterDefinition":
		switch strings.ToUpper(strings.TrimSpace(typ)) {
		case "PT_BRANCH":
			return JenkinsParamGitBranch
		case "PT_TAG":
			return JenkinsParamGitTag
		case "PT_BRANCH_TAG":
			return JenkinsParamGitBranchTag
		case "PT_REVISION":
			return JenkinsParamGitRevision
		case "PT_PULL_REQUEST":
			return JenkinsParamGitPullReq
		case "PT_BUILD":
			return JenkinsParamGitBuild
		default:
			// 认不出的子类型按"分支+标签"处理：取值接口返回什么就展示什么，
			// 比归到 unknown（会被禁止提交）更可用。
			return JenkinsParamGitBranchTag
		}
	case "ChoiceParameterDefinition":
		return JenkinsParamChoice
	case "BooleanParameterDefinition":
		return JenkinsParamBoolean
	case "StringParameterDefinition":
		return JenkinsParamString
	case "PasswordParameterDefinition":
		return JenkinsParamPassword
	case "TextParameterDefinition":
		return JenkinsParamText
	default:
		// 平台还不认识的类型：只读展示、**不允许提交**。
		// 猜着渲染成文本框会把错的值送进 Jenkins，而且不会有任何报错。
		return JenkinsParamUnknown
	}
}

// ParamValues 读取 GitParameter 的可选值（版本标签 / 分支）。
//
// ⚠️ 查询串必须是 `param=<参数名>`。写成 `name=<参数名>` 时 Jenkins 返回
// **HTTP 200 + `values: []`**——不报错、不警告，就是空的；参数名根本不存在时也一样。
// 所以调用方拿到 0 个值时必须给出排查方向，不能只显示"暂无数据"。
//
// className 用参数定义里回传的类名（Jenkins 的 Stapler 约定：
// descriptorByName/<definition 类名>），不写死 GitParameter 的类名，
// 这样插件改包名也能跟着走。kind 由调用方从参数定义里带进来——
// 它决定"值怎么分组"，但本函数**不会**为了拿它再发一次请求。
func (c *JenkinsClient) ParamValues(ctx context.Context, fullName, param, className, kind string) (*JenkinsParamValues, error) {
	start := time.Now()
	out := &JenkinsParamValues{Param: param, Source: "real"}

	if strings.TrimSpace(param) == "" {
		return nil, fmt.Errorf("参数名不能为空")
	}
	cls := strings.TrimSpace(className)
	if cls == "" {
		cls = jenkinsGitParameterClass
	}
	path := jobPath(fullName) + "/descriptorByName/" + url.PathEscape(cls) + "/fillValueItems"

	var raw struct {
		Values json.RawMessage `json:"values"`
		Errors json.RawMessage `json:"errors"`
	}
	if _, err := c.get(ctx, path, url.Values{"param": {param}}, &raw); err != nil {
		return nil, err
	}
	out.ElapsedMs = int(time.Since(start).Milliseconds())
	out.FetchedAt = time.Now().Format(time.RFC3339)
	out.Errors = rawStrings(raw.Errors)

	for _, v := range rawStrings(raw.Values) {
		v = strings.TrimSpace(v)
		switch {
		case v == "":
			out.Other = append(out.Other, v)
		case strings.HasPrefix(v, "origin/"), strings.HasPrefix(v, "refs/"):
			out.Branches = append(out.Branches, v)
		case kind == JenkinsParamGitTag:
			// 明确是"只列标签"的参数：就算值里带斜杠也当标签，
			// 否则一个名为 v_1.2/rc 的标签会被错分到分支组。
			out.Tags = append(out.Tags, v)
		case kind == JenkinsParamGitBranch:
			out.Branches = append(out.Branches, v)
		case strings.Contains(v, "/"):
			out.Branches = append(out.Branches, v)
		default:
			out.Tags = append(out.Tags, v)
		}
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if out.Branches == nil {
		out.Branches = []string{}
	}
	if out.Other == nil {
		out.Other = []string{}
	}
	out.Total = len(out.Tags) + len(out.Branches) + len(out.Other)
	return out, nil
}

// --------------------------------------------------------------------------- 触发与构建

// Trigger 触发一次构建，返回 Jenkins 的队列项 id。
//
// params 为空时走 `/build`（= 用 Jenkins 侧配置的默认值），
// 非空时走 `/buildWithParameters`。
//
// 这个选择不是省事，是正确性：GitParameter 的真实默认值平台读不到
// （见 JenkinsParamDef.DefaultValue 的说明），所以"用户没选"就**不该传**——
// 不传与在 Jenkins 页面上直接点 Build 完全一致；
// 传一个平台猜的默认值过去，才可能真的构建错版本，而且不会报错。
func (c *JenkinsClient) Trigger(ctx context.Context, fullName string, params []JenkinsBuildParam) (int, error) {
	path := jobPath(fullName)
	form := url.Values{}
	for _, p := range params {
		if strings.TrimSpace(p.Name) == "" {
			continue
		}
		form.Add(p.Name, p.Value)
	}

	endpoint := path + "/build"
	if len(form) > 0 {
		endpoint = path + "/buildWithParameters"
	}

	loc, err := c.post(ctx, endpoint, form)
	if err != nil {
		return 0, err
	}
	qid := jenkinsQueueIDFromLocation(loc)
	if qid <= 0 {
		return 0, UpstreamError("Jenkins", fmt.Errorf(
			"触发已发出但未能从响应头 Location（%q）解析出队列项 id，请检查 Jenkins 是否被反向代理改写了响应头", loc))
	}
	return qid, nil
}

// QueueItem 查询队列项，返回构建号。
//
// found=false 表示队列项已经不在队列里了。**不要把它当成错误**：
// 老版本 Jenkins 在构建真正开始后就把队列项移除，此时 /queue/item/{id} 返回 404。
// 那种情况下构建号要由调用方用 job 的 lastBuild 兜底（见 service 层）。
func (c *JenkinsClient) QueueItem(ctx context.Context, id int) (buildNumber int, found bool, cancelled bool, err error) {
	var raw struct {
		Cancelled  bool `json:"cancelled"`
		Executable *struct {
			Number int    `json:"number"`
			URL    string `json:"url"`
		} `json:"executable"`
		Why string `json:"why"`
	}
	code, err := c.getWithCode(ctx, fmt.Sprintf("/queue/item/%d/api/json", id),
		url.Values{"tree": {"cancelled,why,executable[number,url]"}}, &raw)
	if err != nil {
		return 0, false, false, err
	}
	if code == http.StatusNotFound {
		return 0, false, false, nil
	}
	if raw.Cancelled {
		return 0, true, true, nil
	}
	if raw.Executable != nil && raw.Executable.Number > 0 {
		return raw.Executable.Number, true, false, nil
	}
	return 0, true, false, nil
}

// JobSnapshot 取 job 的 nextBuildNumber 与 lastBuild。
//
// 触发**之前**调用：nextBuildNumber 就是这一次构建将要拿到的编号，
// 队列项被 Jenkins 清掉时靠它把构建认出来。
//
// Job 不存在时返回 ErrJobNotFound：这是"绑定关系过期了"（Job 被改名或删除），
// 服务层回 404 并提示重新绑定，而不是报一个无法执行的"上游故障"。
func (c *JenkinsClient) JobSnapshot(ctx context.Context, fullName string) (*JenkinsJobSnapshot, error) {
	path := jobPath(fullName) + "/api/json"
	var out JenkinsJobSnapshot
	code, err := c.getWithCode(ctx, path,
		url.Values{"tree": {"nextBuildNumber,lastBuild[number,result,building,timestamp,duration,url]"}}, &out)
	if err != nil {
		if code == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrJobNotFound, fullName)
		}
		return nil, err
	}
	if out.LastBuild != nil {
		out.LastBuild.URL = c.rewriteJenkinsURL(out.LastBuild.URL)
	}
	return &out, nil
}

// CancelQueueItem 撤销一个还没开跑的队列项。
//
// 用它做补偿：平台"先在 Jenkins 排队、再在本地建记录"，若建记录失败，
// 就必须把已经排上的这一项撤掉，否则用户会看到"平台说失败了，
// 但 Jenkins 上莫名其妙多了一次构建"。
func (c *JenkinsClient) CancelQueueItem(ctx context.Context, id int) error {
	if id <= 0 {
		return nil
	}
	_, err := c.post(ctx, fmt.Sprintf("/queue/cancelItem?id=%d", id), nil)
	return err
}

// BuildStatus 查询一次构建的状态。
func (c *JenkinsClient) BuildStatus(ctx context.Context, fullName string, number int) (*JenkinsBuildRef, error) {
	path := fmt.Sprintf("%s/%d/api/json", jobPath(fullName), number)
	var out JenkinsBuildRef
	if _, err := c.get(ctx, path,
		url.Values{"tree": {"number,result,building,timestamp,duration,url,estimatedDuration"}}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BuildLog 增量读取构建日志。
//
// Jenkins 没有日志推送，只能轮询 progressiveText：从 start 开始取新内容，
// 响应头 X-Text-Size 给出下次的起点、X-More-Data 表示"还有更多"。
// 平台把它转写成自己的日志流，前端因此零改动（复用构建日志页与 SSE）。
//
// found=false 表示该构建号还不存在（刚触发、还没开跑），不是错误。
func (c *JenkinsClient) BuildLog(ctx context.Context, fullName string, number int, start int64) (
	text string, next int64, more bool, found bool, err error) {

	if number <= 0 {
		return "", start, true, false, nil
	}
	path := fmt.Sprintf("%s/%d/logText/progressiveText", jobPath(fullName), number)
	resp, err := c.getRaw(ctx, path, url.Values{"start": {strconv.FormatInt(start, 10)}})
	if err != nil {
		return "", start, false, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", start, true, false, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, jenkinsProgressiveTextCap))
	if readErr != nil {
		return "", start, false, false, UpstreamError("Jenkins", readErr)
	}
	if resp.StatusCode >= 300 {
		return "", start, false, false, UpstreamError("Jenkins", fmt.Errorf(
			"HTTP %d: %s", resp.StatusCode, truncate(string(body), 200)))
	}
	next = start
	if v := strings.TrimSpace(resp.Header.Get("X-Text-Size")); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
			next = n
		}
	}
	if next <= start {
		// 头缺失时按"读到多少就推进多少"兜底，否则会原地死循环。
		next = start + int64(len(body))
	}
	more = strings.EqualFold(strings.TrimSpace(resp.Header.Get("X-More-Data")), "true")
	return string(body), next, more, true, nil
}

// BuildURL 返回某次构建在 Jenkins 上的完整地址。
//
// 用**平台配置的**地址前缀而不是 Jenkins 自报的 rootUrl：
// 实测目标 Jenkins 配的 rootUrl 是内网 IP，拿它拼出来的链接在用户浏览器上打不开。
func (c *JenkinsClient) BuildURL(fullName string, number int) string {
	if c.base == "" || number <= 0 {
		return ""
	}
	return fmt.Sprintf("%s%s/%d/", c.base, jobPath(fullName), number)
}

// StopBuild 中止一次正在执行的构建。
func (c *JenkinsClient) StopBuild(ctx context.Context, fullName string, number int) error {
	if number <= 0 {
		return nil
	}
	_, err := c.post(ctx, fmt.Sprintf("%s/%d/stop", jobPath(fullName), number), nil)
	return err
}

// --------------------------------------------------------------------------- HTTP 底座

func (c *JenkinsClient) applyAuth(req *http.Request) {
	if c.username != "" {
		// Jenkins 的 Basic 认证：用户名 + （API Token 或密码）。
		req.SetBasicAuth(c.username, c.secret)
	}
}

func (c *JenkinsClient) get(ctx context.Context, path string, q url.Values, out any) (int, error) {
	// getWithCode 已经把 4xx/5xx 转成错误了，这里只是让它读起来像"读一个对象"。
	return c.getWithCode(ctx, path, q, out)
}

// getWithCode 发一次 GET，返回状态码与解析结果。
// 4xx 不直接转成错误，是为了让调用方能区分"接口不存在"（老版本）与"凭据无效"。
func (c *JenkinsClient) getWithCode(ctx context.Context, path string, q url.Values, out any) (int, error) {
	resp, err := c.getRaw(ctx, path, q)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return resp.StatusCode, UpstreamError("Jenkins", readErr)
	}
	if resp.StatusCode >= 300 {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg += "（凭据被拒或权限不足：请检查用户名与密钥，以及该账号是否有 Overall/Read 权限）"
		}
		return resp.StatusCode, UpstreamError("Jenkins", fmt.Errorf("%s: %s", msg, truncate(string(body), 200)))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, UpstreamError("Jenkins", fmt.Errorf("响应解析失败: %w", err))
		}
	}
	return resp.StatusCode, nil
}

// getRaw 发一次 GET，返回未读的响应。调用方负责关闭 Body。
func (c *JenkinsClient) getRaw(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c.applyAuth(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, UpstreamError("Jenkins", err)
	}
	return resp, nil
}

// post 发一次写请求（触发构建 / 中止构建），返回 Location 头。
//
// 403 的处理是这里最绕的一处：CSRF 开启且缺少 crumb 时，Jenkins 返回的是
// **403 而不是 401**，与"权限不足"同码。所以先补 crumb 重试一次，
// 补完仍然 403 才判定为真的没权限——否则一次 csrf 缺失会被报成"账号没权限"，
// 引导用户去查错的方向。
func (c *JenkinsClient) post(ctx context.Context, path string, form url.Values) (string, error) {
	hdr, val := c.cachedCrumb()
	loc, code, body, err := c.doPost(ctx, path, form, hdr, val)
	if err != nil {
		return "", err
	}
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		if field, crumb, cerr := c.fetchCrumb(ctx); cerr == nil && crumb != "" {
			loc, code, body, err = c.doPost(ctx, path, form, field, crumb)
			if err != nil {
				return "", err
			}
		}
	}
	if code >= 300 {
		return "", UpstreamError("Jenkins", fmt.Errorf("HTTP %d: %s", code, truncate(string(body), 200)))
	}
	return loc, nil
}

func (c *JenkinsClient) doPost(ctx context.Context, path string, form url.Values, crumbField, crumb string) (
	loc string, code int, body []byte, err error) {

	payload := ""
	if len(form) > 0 {
		payload = form.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(payload))
	if err != nil {
		return "", 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if crumbField != "" && crumb != "" {
		req.Header.Set(crumbField, crumb)
	}
	c.applyAuth(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, nil, UpstreamError("Jenkins", err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.Header.Get("Location"), resp.StatusCode, body, nil
}

// cachedCrumb 返回当前缓存的 crumb。已知"这台没开 CSRF"时返回空值。
func (c *JenkinsClient) cachedCrumb() (field, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.crumbField, c.crumb
}

// fetchCrumb 取一次 CSRF crumb，返回 (请求头名, crumb 值, 错误)。
//
// **crumb 为空且 err 为 nil 表示"这台没启用 CSRF"** —— 404 是正常状态，不是错误，
// 并会被缓存下来，免得每次写请求都先去问一遍。
func (c *JenkinsClient) fetchCrumb(ctx context.Context) (field, crumb string, err error) {
	var raw struct {
		Crumb             string `json:"crumb"`
		CrumbRequestField string `json:"crumbRequestField"`
	}
	code, err := c.getWithCode(ctx, "/crumbIssuer/api/json", nil, &raw)
	switch {
	case code == http.StatusNotFound:
		c.mu.Lock()
		c.crumb, c.crumbField, c.crumbKnown = "", "", true
		c.mu.Unlock()
		return "", "", nil
	case err != nil:
		return "", "", err
	}
	if raw.Crumb == "" {
		return "", "", nil
	}
	field = raw.CrumbRequestField
	if field == "" {
		field = "Jenkins-Crumb"
	}
	c.mu.Lock()
	c.crumb, c.crumbField, c.crumbKnown = raw.Crumb, field, true
	c.mu.Unlock()
	return field, raw.Crumb, nil
}

// --------------------------------------------------------------------------- 小工具

// jobPath 把 fullName（如 `backend/pay/ci-x`）翻成 Jenkins 的 job 路径。
//
// Jenkins 用 `/job/` 把每一级串起来并对每段单独转义，
// 中文 job 名（实测有 `镜像打包-demoitms` 这类）必须逐段转义才可用。
func jobPath(fullName string) string {
	segs := strings.Split(strings.Trim(strings.TrimSpace(fullName), "/"), "/")
	out := make([]string, 0, len(segs)*2)
	for _, s := range segs {
		if s == "" {
			continue
		}
		out = append(out, "job", url.PathEscape(s))
	}
	return "/" + strings.Join(out, "/")
}

// jenkinsQueueIDFromLocation 从 Location 头里取出队列项 id。
func jenkinsQueueIDFromLocation(loc string) int {
	s := strings.TrimRight(strings.TrimSpace(loc), "/")
	if s == "" {
		return 0
	}
	i := strings.LastIndex(s, "/")
	n, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// rewriteJenkinsURL 把 Jenkins 自报的地址改写成平台配置里的地址。
//
// 为什么必须改：Jenkins 返回的 job.url 用的是它自己配置的 rootUrl，
// 实测目标 Jenkins 报的是 `http://47.96.181.128:1143/jenkins/...`（内网 IP），
// 而平台是通过 `http://es-repo.example.com:1143/jenkins` 访问的。
// 原样透到界面上，就是一条**点了打不开**的"在 Jenkins 中打开"链接——
// 而且这种坏链在开发机上往往还能打开，只有换个人访问才暴露。
//
// 只替换 origin（协议+主机+端口），保留 Jenkins 自己的路径，
// 这样带 context path 的部署（路径含 /jenkins）也能正确拼接。
func (c *JenkinsClient) rewriteJenkinsURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || c.base == "" {
		return raw
	}
	base, err := url.Parse(c.base)
	if err != nil {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// 路径与配置的 context path 对不上（例如 Jenkins 配了完全不同的前缀）
	// 说明这不是同一套路径规则，宁可不改，也不拼出一个更错的地址。
	if !strings.HasPrefix(u.Path, base.Path+"/") && u.Path != base.Path {
		return raw
	}
	u.Scheme, u.Host = base.Scheme, base.Host
	return u.String()
}

// isJenkinsDenied 判断错误是否源于权限不足。
func isJenkinsDenied(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 403") || strings.Contains(s, "HTTP 401")
}

// rawScalar 从 RawMessage 里取出标量（字符串 / 布尔 / 数字）。
func rawScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// rawField 取对象里的某个标量字段。不是对象或字段不存在时返回空串。
func rawField(raw json.RawMessage, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return rawScalar(m[field])
}

// rawStrings 把 ["a","b"] 或 [{"value":"a"}] / [{"name":"a"}] 都取成 []string。
func rawStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		// 也可能是单个字符串
		if v := rawScalar(raw); v != "" {
			return []string{v}
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if v := rawScalar(it); v != "" {
			out = append(out, v)
			continue
		}
		if v := rawField(it, "value"); v != "" {
			out = append(out, v)
			continue
		}
		if v := rawField(it, "name"); v != "" {
			out = append(out, v)
		}
	}
	return out
}
