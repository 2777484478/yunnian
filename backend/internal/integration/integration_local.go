package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/2777484478/yunnian/internal/config"
)

// 本文件提供各下游的「本地实现」（local implementation）。
//
// 它们返回**结构化且确定性的**数据：同一个入参永远得到同一份结果。
// 这一点很重要——如果返回随机数据，前端联调时每次刷新都变，问题无法复现。
// 确定性由「入参哈希」保证。

// --------------------------------------------------------------------------- Registry

// LocalRegistry 本地镜像仓库实现。
// 用入参哈希生成确定性的 tag 列表，模拟 Harbor 里已有的历史版本。
type LocalRegistry struct {
	registry string
}

// NewLocalRegistry 构造。
func NewLocalRegistry(cfg *config.Config) *LocalRegistry {
	reg := cfg.Integration.Harbor.Registry
	if reg == "" {
		reg = "harbor.yunnian.internal"
	}
	return &LocalRegistry{registry: reg}
}

// Name 实现名。
func (l *LocalRegistry) Name() string { return "local" }

// Ping 本地实现恒可用。
func (l *LocalRegistry) Ping(context.Context) error { return nil }

// ListTags 生成确定性版本列表。
func (l *LocalRegistry) ListTags(_ context.Context, repo string, limit int) ([]TagInfo, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	now := time.Now()
	out := make([]TagInfo, 0, limit)
	for i := 0; i < limit; i++ {
		sum := hashOf(fmt.Sprintf("%s#%d", repo, i))
		tag := fmt.Sprintf("master-%s-%s", sum[:7], now.Add(-time.Duration(i*7+3)*time.Hour).Format("20060102150405"))
		out = append(out, TagInfo{
			Repo:      repo,
			Tag:       tag,
			Digest:    "sha256:" + hashOf(repo+tag),
			SizeBytes: uint64(20+int(sum[0])%180) << 20,
			PushedAt:  now.Add(-time.Duration(i*7+3) * time.Hour),
		})
	}
	return out, nil
}

// Resolve 本地实现没有真实仓库，返回 (nil, nil) 表示"这次不适用"。
//
// 刻意不返回一条编出来的 digest：调用方会拿它写进 image_record，
// 假值一旦落库就会被 CD 用到部署上。宁可留空。
func (l *LocalRegistry) Resolve(_ context.Context, repo, tag string) (*TagInfo, error) {
	return nil, nil
}

// Accepts 恒为 false：本地实现不接任何真实仓库，谁都不认。
//
// 若这里按 l.registry 的名字返回 true，调用方就会以为"能问"，然后 Resolve 又
// 永远返回空 —— 变成一段看起来在跑、实际什么都不做的逻辑。不如直接说不认。
func (l *LocalRegistry) Accepts(string) bool { return false }

// --------------------------------------------------------------------------- Repository

// LocalRepository 本地代码托管实现。
type LocalRepository struct{}

// NewLocalRepository 构造。
func NewLocalRepository() *LocalRepository { return &LocalRepository{} }

// Name 实现名。
func (l *LocalRepository) Name() string { return "local" }

// Ping 恒可用。
func (l *LocalRepository) Ping(context.Context) error { return nil }

// ErrNoCodeHost 未接入代码托管通道 —— 读不到远端的分支与提交。
//
// 这是 LocalRepository 的**如实结论**，不是故障：平台没配 GitLab 时，
// 它就是不知道远端有什么。以前它靠编数据把这件事盖住了。
const ErrNoCodeHost = "未接入代码托管通道，读不到远端的分支与提交"

// Branches 已不再编造分支列表 —— 如实报"读不到"。
//
// ⚠️ 这里从前返回写死的 {master, develop, release/2.6, release/2.5, hotfix/login}，
// 仅按仓库地址里有没有 "docs-site" 把第一个换成 main。**那五个分支全是不存在的**：
// 用户在项目表单的下拉里选得出，构建时 clone 必然失败，而报错指向"仓库有问题"
// （`couldn't find remote ref`），真因是平台给了一个编出来的分支名。
//
// 分支列表请走 `Set.ProbeGit`：真跑 `git ls-remote`、用**项目自己的凭据**，
// 与构建完全同源（理由见 git_probe.go 文件头）。
func (l *LocalRepository) Branches(context.Context, string) ([]string, error) {
	return nil, errors.New(ErrNoCodeHost)
}

// LatestCommit 已不再编造提交号 —— 如实报"读不到"。
//
// ⚠️ 这里从前用 `hashOf(gitURL + "#" + branch)[:7]` 编一个提交号，连提交信息
// （"feat: 支持按环境下发配置"）和作者（"CI Bot"）一起编出来，还按哈希编了个提交时间。
// 那个 sha 长得完全像真的（前缀、长度都对），会被写进 `build_record.commit_id` /
// `commit_msg`，还可能进 Tag 模板 —— **一旦落库就洗不掉**，事后排查时谁也不会怀疑它。
//
// 提交号请走 `Set.ResolveCommit`：真跑 `git ls-remote`。
func (l *LocalRepository) LatestCommit(context.Context, string, string) (*Commit, error) {
	return nil, errors.New(ErrNoCodeHost)
}

// --------------------------------------------------------------------------- Cluster

// LocalCluster 未接入真实集群时的降级实现。
//
// ⚠️ 它**刻意不模拟成功**。此前这里的 ApplyImage 是「四段 sleep 然后返回 nil」，
// 于是点一次部署，平台显示"部署完成"、集群里毫无变化 —— 运维据此以为线上已经恢复。
// 在所有假数据形态里这一种危害最大：它不是一个显示不准的字段，而是一次**假的写成功**。
//
// 现在的口径严格区分读与写：
//   - 读（Describe / Pods / Nodes）如实返回"读不到"，并带上原因；
//   - 写（ApplyImage / Scale / Restart）一律返回明确错误。
//
// 想要真实能力，请接入集群（integration.kubernetes.enabled=true）。
type LocalCluster struct{}

// NewLocalCluster 构造。
func NewLocalCluster(*config.Config) *LocalCluster { return &LocalCluster{} }

// localClusterHint 未接入集群时的统一说明。
//
// 写全「开关名 + 去哪开」：只说"未接入集群"会让用户以为是环境配错了，
// 而真正要做的是打开配置开关。
const localClusterHint = "平台未接入真实集群（配置项 integration.kubernetes.enabled 为 false），无法执行集群操作"

// Name 实现名。
func (l *LocalCluster) Name() string { return "local" }

// Ping 未接入时如实报错。
//
// 此前它恒返回 nil，于是「测试连接」永远全绿——平台说"连通"没有任何意义，
// 结论必须由集群回答。
func (l *LocalCluster) Ping(context.Context, ClusterRef) error {
	return errors.New(localClusterHint)
}

// Describe 未接入时**返回错误**，不返回"不存在"。
//
// 这两个结论的后果完全不同：调用方看到"确认不存在"会据此清零平台里的副本数，
// 而"没读到"只该让界面显示一个读不到的原因。混为一谈就会在未接入集群时
// 把库里所有服务的副本数清成 0——一次静默的数据破坏。
func (l *LocalCluster) Describe(context.Context, WorkloadRef) (*WorkloadState, error) {
	return nil, errors.New(localClusterHint)
}

// WorkloadUsages 同 Describe：本地实现没有集群可读，明确拒绝。
//
// ⚠️ 这里**不能返回空 map + nil**：那会被上层读成"这些服务都不占资源"，
// 而事实是"根本读不到"。用量这种数字宁可显示不出来，也不能给个 0。
func (l *LocalCluster) WorkloadUsages(context.Context, ClusterRef, string, []string) (map[string]WorkloadUsage, error) {
	return nil, errors.New(localClusterHint)
}

// ApplyImage 明确拒绝，不假装成功。
func (l *LocalCluster) ApplyImage(context.Context, ApplyRequest, func(string, uint8) error) error {
	return errors.New(localClusterHint)
}

// Scale 明确拒绝，不假装成功。
func (l *LocalCluster) Scale(context.Context, WorkloadRef, int) error {
	return errors.New(localClusterHint)
}

// Release 未接入集群时也明确拒绝：这里删的是集群里的变更单，
// 没接入就无从删起，假装成功会让"取消纳管"看起来做过了。
func (l *LocalCluster) Release(context.Context, WorkloadRef) error {
	return errors.New(localClusterHint)
}

// Restart 明确拒绝，不假装成功。
func (l *LocalCluster) Restart(context.Context, WorkloadRef) error {
	return errors.New(localClusterHint)
}

// Pods 未接入时没有 Pod 可列，返回错误让调用方把原因说出去。
func (l *LocalCluster) Pods(context.Context, WorkloadRef) ([]PodInfo, error) {
	return nil, errors.New(localClusterHint)
}

// PodLogs 未接入时没有日志源。
//
// 此前它生成的是一段形状正确的**编造日志**：前端日志窗看着一切正常，
// 内容却与集群毫无关系。比空更糟——运维会照着假日志排查。
func (l *LocalCluster) PodLogs(context.Context, WorkloadRef, string, int) (io.ReadCloser, error) {
	return nil, errors.New(localClusterHint)
}

// DiagnoseFailure 未接入时没有任何证据可取，**返回错误**。
//
// 不能返回一个空证据：下游会把它读成"采集过了，容器侧没有异常"，
// 于是把一个"没看到"说成"没问题"。返回错误才能让它如实降级为错误码规则，
// 并在结论里标明"本次未由模型分析"。
func (l *LocalCluster) DiagnoseFailure(context.Context, WorkloadRef) (*FailureEvidence, error) {
	return nil, errors.New(localClusterHint)
}

// Nodes 未接入时读不到容量，MetricsAvailable 保持 false。
//
// 返回 error 而不是零值结构体：零值会被前端渲染成"0 个节点 / 用量 0%"，
// 那是"没读到"，不是"集群是空的"。
func (l *LocalCluster) Nodes(context.Context, ClusterRef) (*ClusterState, error) {
	return nil, errors.New(localClusterHint)
}

// --------------------------------------------------------------------------- Builder

// failProjects 是本地实现里「刻意失败」的项目集合。
// 目的是让失败路径（错误码 + AI 归因 + 日志尾部报错）可复现，
// 便于演示与联调，而不是每次随机失败（那样无法复现问题）。
var failProjects = map[string]string{
	"mall-h5":        "NPM_INSTALL",
	"report-service": "COMPILE_ERROR",
}

// LocalBuilder 本地构建实现，生成与真实 CI 同构的日志流。
type LocalBuilder struct{}

// UnavailableBuilder 表示"所选执行方式当前不可用"。
//
// 它只为一种情况存在：`runner.mode=agent` 而 Agent 通道尚未接上。
// 这时**必须**让构建明确失败，不能退回 LocalBuilder ——
// 退回的结果是构建记录写「成功」，而用户以为自己用的是构建机，
// 于是他把一次假成功当成了真成功。一个说得出原因的失败，远好过一个假成功。
type UnavailableBuilder struct{ Reason string }

// Name 实现名。界面据此显示"这次构建由谁执行"，不能谎报成 docker。
func (u *UnavailableBuilder) Name() string { return "unavailable" }

// Build 直接返回带真因的失败。
func (u *UnavailableBuilder) Build(context.Context, BuildRequest, func(level, line string) error) (*BuildResult, error) {
	reason := strings.TrimSpace(u.Reason)
	if reason == "" {
		reason = "所选构建执行方式当前不可用"
	}
	return nil, &BuildFailure{Code: "RUNNER_UNAVAILABLE", Stage: "领取任务", Detail: reason}
}

// NewLocalBuilder 构造。
func NewLocalBuilder() *LocalBuilder { return &LocalBuilder{} }

// Name 实现名。
func (l *LocalBuilder) Name() string { return "local" }

// Build 逐行输出构建日志。
//
// 说明：这里不真的执行 docker build —— 平台进程内不该跑构建（会拖垮平台自身，
// 也无法并发隔离）。真实构建由 Runner Agent 执行，本实现只保证「编排 → 日志 → 产物」
// 这条链路的形状正确，接上 Runner 后替换实现即可。
func (l *LocalBuilder) Build(ctx context.Context, req BuildRequest, sink func(level, line string) error) (*BuildResult, error) {
	start := time.Now()
	lines := buildLogScript(req)

	for _, ln := range lines {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ln.pause):
		}
		if err := sink(ln.level, ln.line); err != nil {
			return nil, err
		}
	}

	if code, willFail := failProjects[req.ProjectKey]; willFail {
		return nil, &BuildFailure{Code: code, Stage: "构建"}
	}

	// ⚠️ 这里**不产出 digest，也不产出 size**。
	//
	// 以前它会返回 `"sha256:" + hash(镜像名+提交号)` 和按镜像名猜出来的大小。
	// 那个 digest 看起来完全像真的（前缀、长度都对），会被写进 image_record.digest，
	// 而 EnrichMissingImageMeta 只补空值、**不覆盖** —— 一条假 digest 一旦落库就洗不掉，
	// 界面上「镜像版本」这一列之后一直是编的，而且谁也不会怀疑它。
	//
	// 本地模拟就是本地模拟：没有真的产出镜像，就没有 digest。宁可显示「—」。
	return &BuildResult{
		Duration: time.Since(start),
		Source:   BuildSourceLocal,
	}, nil
}

// BuildFailure 构建失败，携带失败分类码。
type BuildFailure struct {
	Code  string
	Stage string
	// Detail 失败阶段输出的**尾部原文**。
	//
	// 为什么必须带着它，而不是只留一个码：以前 Error() 只回一句
	// 「构建失败（阶段=X，错误码=Y）」，于是界面上的「失败原因」只有机器码，
	// 而真因（哪一行报了什么）只活在日志流里 —— 日志流是进程内 + TTL 的，
	// 平台一重启就什么都不剩，没人再能回答"当时到底为什么失败"。
	Detail string
	// Err 底层错误（外部命令退出码 / ctx 错误）。
	//
	// 必须保留并实现 Unwrap：调用方靠 `errors.Is(err, context.Canceled)` 区分
	// 「用户取消」与「构建失败」，被包一层就判不出来了 —— 那会把取消显示成失败，
	// 两者在前端的后续处理完全不同。
	Err error
}

// Error 实现 error。
//
// 只拼尾部原文的**最后一行有意义的内容**，不是整段：
// error_message 列是 512 字节，整段塞进去会被截断，而截断掉的恰恰是最末尾那行真因。
//
// 阶段为空时不打印"阶段="：Agent（构建机）上报的失败**没有阶段这个字段**
// （协议里只有 status / digest / size / arch / errorCode / errorMessage），
// 硬拼出来就是界面上一句 `构建失败（阶段=，错误码=SCRIPT_FAILED）` ——
// 一个逗号后面什么都没有的机器残渣，读的人只会以为平台坏了。
func (e *BuildFailure) Error() string {
	head := "构建失败（错误码=" + e.Code + "）"
	if e.Stage != "" {
		head = fmt.Sprintf("构建失败（阶段=%s，错误码=%s）", e.Stage, e.Code)
	}
	if line := lastMeaningfulLine(e.Detail); line != "" {
		return head + "：" + line
	}
	if e.Err != nil {
		return head + "：" + e.Err.Error()
	}
	return head
}

// Unwrap 让 errors.Is / errors.As 能穿透到底层的 ctx 错误与退出错误。
func (e *BuildFailure) Unwrap() error { return e.Err }

// lastMeaningfulLine 取尾部输出里最后一行非空内容，并截断到合理长度。
func lastMeaningfulLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if v := strings.TrimSpace(lines[i]); v != "" {
			return truncate(v, 300)
		}
	}
	return ""
}

type logLine struct {
	level string
	line  string
	pause time.Duration
}

// buildLogScript 生成构建日志脚本。行内容与 docs/05 时序图里描述的阶段一致。
func buildLogScript(req BuildRequest) []logLine {
	head := []logLine{
		{"info", fmt.Sprintf("Runner 领取任务 build#%d", req.BuildID), 200 * time.Millisecond},
		{"cmd", fmt.Sprintf("$ git clone --depth 1 -b %s %s", req.Branch, req.GitURL), 260 * time.Millisecond},
		{"info", "Cloning into '" + req.ProjectKey + "'...", 320 * time.Millisecond},
		{"info", "remote: Enumerating objects: 1284, done.", 220 * time.Millisecond},
		{"info", "Receiving objects: 100% (1284/1284), 2.41 MiB | 8.20 MiB/s, done.", 380 * time.Millisecond},
		{"info", fmt.Sprintf("HEAD is now at %s", shortID(req.CommitID)), 200 * time.Millisecond},
		{"cmd", fmt.Sprintf("$ docker build -f %s -t %s %s", req.Dockerfile, req.Image, req.Context), 260 * time.Millisecond},
	}

	var mid []logLine
	if strings.Contains(req.Image, "/frontend/") {
		mid = []logLine{
			{"info", "#4 [deps 1/3] COPY package*.json ./", 200 * time.Millisecond},
			{"info", "#5 [deps 2/3] RUN npm ci", 700 * time.Millisecond},
			{"info", "added 1284 packages in 42s", 500 * time.Millisecond},
			{"info", "#8 [build 1/2] RUN npm run build", 260 * time.Millisecond},
			{"info", "vite v5.4.2 building for production...", 620 * time.Millisecond},
			{"info", "✓ 1284 modules transformed.", 480 * time.Millisecond},
			{"info", "dist/assets/index-8fJ2k.js   412.38 kB │ gzip: 128.44 kB", 300 * time.Millisecond},
			{"info", "✓ built in 18.42s", 300 * time.Millisecond},
		}
	} else {
		mid = []logLine{
			{"info", "#4 [builder 2/6] COPY pom.xml .", 200 * time.Millisecond},
			{"info", "#5 [builder 3/6] RUN mvn -B -DskipTests -U dependency:go-offline", 700 * time.Millisecond},
			{"info", "[INFO] BUILD SUCCESS", 420 * time.Millisecond},
			{"info", "#8 [builder 6/6] RUN mvn -B -DskipTests package", 640 * time.Millisecond},
			{"info", "[INFO] Building jar: /build/target/app.jar", 380 * time.Millisecond},
			{"info", "[INFO] Total time:  01:12 min", 260 * time.Millisecond},
		}
	}

	if code, willFail := failProjects[req.ProjectKey]; willFail {
		failures := map[string][]logLine{
			"NPM_INSTALL": {
				{"error", "npm ERR! code ETARGET", 200 * time.Millisecond},
				{"error", "npm ERR! notarget No matching version found for @yunnian/shared@2.6.1", 240 * time.Millisecond},
				{"error", "npm ERR! notarget In most cases you or one of your dependencies are requesting", 200 * time.Millisecond},
				{"error", "npm ERR! A complete log of this run can be found in: /root/.npm/_logs/2026-09-23T03_12_44_118Z-debug.log", 200 * time.Millisecond},
			},
			"COMPILE_ERROR": {
				{"error", "[ERROR] /src/main/java/com/yunnian/report/OrderController.java:[42,18] cannot find symbol", 200 * time.Millisecond},
				{"error", "[ERROR]   symbol:   class PageQuery", 200 * time.Millisecond},
				{"error", "[ERROR]   location: class com.yunnian.report.OrderController", 200 * time.Millisecond},
				{"error", "[ERROR] BUILD FAILURE", 200 * time.Millisecond},
			},
		}
		tail := failures[code]
		return append(append(head, mid...), tail...)
	}

	tail := []logLine{
		{"info", fmt.Sprintf("#12 pushing %s", req.Image), 300 * time.Millisecond},
		{"info", "#12 pushing layers", 700 * time.Millisecond},
		{"info", "#12 pushing manifest", 320 * time.Millisecond},
		{"info", "#12 DONE 3.2s", 260 * time.Millisecond},
		{"info", "构建成功，镜像已推送至镜像仓库", 200 * time.Millisecond},
	}
	return append(append(head, mid...), tail...)
}

func shortID(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

// hashOf 生成确定性摘要，用于本地实现造数据。
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
