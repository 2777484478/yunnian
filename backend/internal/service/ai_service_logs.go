package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/2777484478/yunnian/internal/integration"
)

// --------------------------------------------------------------------------- 日志检索（只读）
//
// 为什么需要它（2026-11-04 用户实测提出）：
//
// 对话里问「aio-mcp-server 的日志里有什么错误」，助手如实回答"没有查日志的工具，
// diagnose_service 只在 Pod 未就绪时采崩溃前那一轮日志，当前所有 Pod 都就绪所以取不到"。
// 那句回答是**对的**，但用户真正需要的东西没被满足 —— 服务好好跑着的时候，
// 恰恰是最常要查日志的时刻（报错但没崩、接口 500、慢查询、连接池耗尽）。
//
// 原有工具里，`diagnose_service` 的日志是**故障取证的副产品**：
// 它只采「未就绪 Pod」，而且采的是崩溃前那一轮。这不是能力不够，是定位不同 ——
// 取证要回答"为什么起不来"，日志检索要回答"它现在在报什么错"。
//
// 三条前提，改这个文件前先读：
//
//  1. **只读**。只调 Pods / PodLogs 两个读接口，没有任何写动作。
//  2. **只回片段，不回全量日志**。一次几千行塞给模型，既烧 token 又会把真正
//     那行根因淹没掉。所以：先按关键词筛，再带前后几行上下文，并**显式告诉模型
//     命中了几处、只回了几处** —— 否则模型会把"回了 8 条"读成"只有 8 条错误"。
//  3. **没命中 ≠ 没问题**。扫完 0 命中时必须说清扫了多少行、用的是哪组关键词、
//     以及"日志可能在 stdout 之外"。回一个空数组等于告诉模型"服务很干净"，
//     与事实可能正好相反（这条与 FailureEvidence.Notes 是同一个道理）。

const (
	// logScanDefaultLines 每个 Pod 默认取多少行尾部日志。
	//
	// 400 是折中：Spring 服务一次启动就能刷几千行，取太少会漏掉启动后的报错；
	// 取太多则单次工具返回过大。真正的上限由用户/模型通过 lines 参数抬。
	logScanDefaultLines = 400
	logScanMaxLines     = 2000
	// logScanMaxPods 一次最多扫几个 Pod。
	//
	// 10 副本的服务全扫一遍 = 10 次 API 调用 × 400 行，而副本之间日志高度重复
	// （同一个镜像、同一份配置）。按「重启次数降序」取前几个，命中率最高。
	logScanMaxPods = 4
	// logScanReadCap 从 API 读多少字节就停（防某行超长把内存吃光）。
	logScanReadCap = 1 << 20
	// logScanContext 命中行前后各保留几行上下文。
	//
	// 2 行是能看清 Java 堆栈 `Caused by:` 链的最小值；再多会让 8 条命中膨胀成 80 行。
	logScanContext = 2
	// logScanHitsPerPod / logScanHitsTotal 命中片段上限。
	logScanHitsPerPod = 8
	logScanHitsTotal  = 24
)

// defaultLogPatterns 默认异常关键词（全部小写，匹配时把日志行也转小写）。
//
// 选的是一批"在日志里出现就值得看一眼"的词，而不是严格的正则：
// 运维搜日志时用的也是这几个词。中文词单独列，因为不少服务在打中文日志。
//
// 已知取舍：`error` 会命中 "0 errors" / "no error" 这类**正常**日志。
// 不做否定词排除 —— 那需要理解句子，规则一多就会误杀真错误；
// 宁可多回几条让人一眼扫过，也不要漏掉真问题。
var defaultLogPatterns = []string{
	"error", "exception", "fatal", "panic", "caused by",
	"failed", "failure", "timeout", "timed out", "refused", "denied",
	"out of memory", "oom", "stacktrace", "traceback",
	"异常", "错误", "失败", "超时", "拒绝",
}

// toolSearchServiceLogs 检索运行中服务的容器日志，只回异常片段。
func (a *AIService) toolSearchServiceLogs() aiTool {
	return aiTool{
		Name: "search_service_logs",
		Description: "检索某个服务**当前运行中** Pod 的容器日志，只返回**异常相关片段**" +
			"（error / exception / panic / timeout / 异常 等，可用 keyword 换词），每条带前后各 2 行上下文。" +
			"回答「服务在报什么错」「日志里有没有异常」「接口为什么 500」用它。" +
			"⚠️ 它与 diagnose_service 的分工：diagnose_service 只在 Pod **未就绪**时才有日志可采（崩溃取证），" +
			"而本工具**不需要 Pod 崩溃**，服务正常运行也能查。" +
			"返回的是**片段**不是全量日志，行号是「尾部窗口内的相对行号」（1 = 窗口第一行），不是容器全量行号。",
		Parameters: schema(map[string]any{
			"envKey":     map[string]any{"type": "string", "description": "环境的 env_key，先用 list_environments 确认"},
			"projectKey": map[string]any{"type": "string", "description": "项目的 project_key"},
			"keyword": map[string]any{"type": "string",
				"description": "可选。自定义关键词，逗号分隔（如 \"NullPointer,504,deadlock\"）。给了它就只按它筛，不再用默认异常词"},
			"lines": map[string]any{"type": "integer",
				"description": "可选。每个 Pod 取尾部多少行，默认 400，范围 50~2000"},
			"pod": map[string]any{"type": "string",
				"description": "可选。只看某个 Pod（名字可从 get_service_status / diagnose_service 拿）。默认自动挑重启次数最多的几个"},
			"container": map[string]any{"type": "string",
				"description": "可选。Pod 里有多个容器时**必须**指定，否则 K8s 会拒绝返回日志"},
		}, []string{"envKey", "projectKey"}),
		Run: func(ctx context.Context, c Caller, args map[string]any) (string, error) {
			envKey, projectKey := argStr(args, "envKey"), argStr(args, "projectKey")
			if err := assertToolEnvVisible(c, envKey); err != nil {
				return "", err
			}
			if envKey == "" || projectKey == "" {
				return "", errors.New("缺少 envKey 或 projectKey")
			}
			env, err := a.aiEnvByKey(ctx, envKey)
			if err != nil {
				return jsonText(failResult(err))
			}
			workload, linked, err := a.aiWorkloadOf(ctx, envKey, projectKey)
			if err != nil {
				return jsonText(failResult(err))
			}
			ref := workloadRefOf(env, workload)

			lines := argInt(args, "lines", logScanDefaultLines, 50, logScanMaxLines)
			container := argStr(args, "container")
			patterns := defaultLogPatterns
			if kw := argStr(args, "keyword"); kw != "" {
				patterns = splitLogKeywords(kw)
				if len(patterns) == 0 {
					patterns = defaultLogPatterns
				}
			}

			// 选要扫哪些 Pod。
			var candidates []integration.PodInfo
			wanted := argStr(args, "pod")
			if wanted != "" {
				candidates = []integration.PodInfo{{Name: wanted}}
			} else {
				all, err := a.deps.Up.Cluster.Pods(ctx, ref)
				if err != nil {
					return jsonText(failResult(fmt.Errorf(
						"列出工作负载 %s/%s 的 Pod 失败（权限 / 网络 / 超时）：%w", env.Namespace, workload, err)))
				}
				if len(all) == 0 {
					// 「没有 Pod」与「日志里没有错误」是两件事，必须分开说。
					return jsonText(map[string]any{
						"ok": true, "envKey": envKey, "projectKey": projectKey,
						"workload": workload, "linked": linked,
						"podsTotal": 0, "hits": []any{},
						"note": "该工作负载下**当前没有任何 Pod**（可能是 0 副本，或集群里根本没有它）。" +
							"这不等于「日志里没有错误」—— 没有 Pod 就没有日志可查。",
					})
				}
				// 重启次数多的优先：那才是真正在出问题的实例。
				sort.SliceStable(all, func(i, j int) bool {
					if all[i].Restarts != all[j].Restarts {
						return all[i].Restarts > all[j].Restarts
					}
					return all[i].Name < all[j].Name
				})
				candidates = all
			}
			podsTotal := len(candidates)
			if len(candidates) > logScanMaxPods {
				candidates = candidates[:logScanMaxPods]
			}

			scans := make([]map[string]any, 0, len(candidates))
			hits := make([]logHit, 0, logScanHitsTotal)
			matchedTotal, scannedTotal := 0, 0
			for _, p := range candidates {
				// follow=false：这里只扫尾部若干行就要出结论，跟随会永远读不完。
				rc, err := a.deps.Up.Cluster.PodLogs(ctx, ref, p.Name, container, lines, false)
				if err != nil {
					// 读不到就如实说（含 K8s 原文：多容器 Pod 的报错里带着可选容器名）。
					scans = append(scans, map[string]any{"pod": p.Name, "readError": err.Error()})
					continue
				}
				txt, truncated := readLogCapped(rc, logScanReadCap)
				_ = rc.Close()
				hs, scanned, matched := scanLogText(p.Name, txt, patterns, logScanContext, logScanHitsPerPod)
				scannedTotal += scanned
				matchedTotal += matched
				item := map[string]any{
					"pod": p.Name, "restarts": p.Restarts,
					"scannedLines": scanned, "matched": matched,
				}
				if truncated {
					item["truncated"] = true
				}
				scans = append(scans, item)
				// 总量封顶：前几个 Pod 就把额度用完时，后面的 Pod 只报计数不回片段。
				for _, h := range hs {
					if len(hits) >= logScanHitsTotal {
						break
					}
					hits = append(hits, h)
				}
			}

			res := map[string]any{
				"ok": true, "envKey": envKey, "projectKey": projectKey,
				"workload": workload, "linked": linked,
				"podsTotal": podsTotal, "podsScanned": scans,
				"patterns": patterns, "linesPerPod": lines,
				"scannedLines": scannedTotal,
				"matchedTotal": matchedTotal, "hitsShown": len(hits),
				"hits": hits,
			}
			if !linked {
				res["linkedNote"] = "该服务没有纳管到本环境，这里是用 project_key 当工作负载名查到的日志；" +
					"它不受本平台的发布管理。"
			}
			if podsTotal > len(candidates) {
				res["podScopeNote"] = fmt.Sprintf(
					"该工作负载共 %d 个 Pod，只扫了其中 %d 个（按重启次数从多到少）。"+
						"其余 Pod 未扫到，若要全量请用 pod 参数逐个指定。", podsTotal, len(candidates))
			}
			// 三句话，决定模型会不会把结论说过头。
			switch {
			case matchedTotal == 0:
				res["note"] = fmt.Sprintf(
					"扫了 %d 行日志（%d 个 Pod）**没有匹配到**这些关键词。这不代表服务没问题："+
						"① 关键词可能不覆盖你的日志格式，可用 keyword 参数换词再试；"+
						"② 日志可能不在容器 stdout（文件日志、另一个容器的日志）；"+
						"③ 时间窗口只有尾部 %d 行，更早的报错不在里面。",
					scannedTotal, len(candidates), lines)
			case len(hits) < matchedTotal:
				res["note"] = fmt.Sprintf(
					"共命中 %d 处，只回了前 %d 处（每 Pod 最多 %d 处）。"+
						"要定位更多请缩小 keyword 或指定 pod。",
					matchedTotal, len(hits), logScanHitsPerPod)
			}
			return jsonText(res)
		},
	}
}

// logHit 一处命中的日志片段。
type logHit struct {
	Pod  string `json:"pod"`
	Line int    `json:"line"`
	// Text 命中行原文（右侧空白已裁掉，左侧缩进保留：堆栈是靠缩进看层级的）。
	Text string `json:"text"`
	// Before / After 命中行前后各若干行，用于看清上下文（如 `Caused by:` 链）。
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// scanLogText 在一段日志里按关键词找异常片段。
//
// 三个返回值都不是装饰：
//   - hits 是要回给模型的片段（最多 maxHits 条）；
//   - scanned 是**扫过的行数**，它是"0 命中"这个结论可信度的凭据 ——
//     只说"没找到错误"而不说扫了多少行，模型无法判断这是"扫了一大片很干净"
//     还是"根本没读到日志"；
//   - matched 是**命中的段数（未截断）**。必须与 len(hits) 分开：相等的话
//     "只回了前 N 处、还有更多"这句话就永远说不出来，模型会把 8 条读成全部。
//     2026-11-04 实测踩过：两个数都按 len(hits) 算，note 分支成了死代码。
//
// maxHits 按 Pod 计数。达到上限后**仍然继续扫**（只计数不收集）：
// 提前 break 会让 matched 恰好等于上限，与"刚好只有这么多"无法区分。
func scanLogText(pod, text string, patterns []string, ctxLines, maxHits int) (hits []logHit, scanned, matched int) {
	if strings.TrimSpace(text) == "" {
		return nil, 0, 0
	}
	text = strings.ToValidUTF8(text, "")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return nil, 0, 0
	}
	if len(patterns) == 0 || maxHits <= 0 {
		return nil, len(lines), 0
	}
	lowered := make([]string, len(lines))
	for i, l := range lines {
		lowered[i] = strings.ToLower(l)
	}

	hits = make([]logHit, 0, maxHits)
	// lastEnd 上一段**上下文窗口**的末尾行号。
	//
	// 合并条件是「新命中行的上文与上一段的下文相接或重叠」（i-ctx <= lastEnd+1），
	// 而不是简单的 i <= lastEnd：Java 堆栈里 `ERROR` 与 `Caused by` 之间常夹着
	// 一到两行不命中的 `at ...` 帧行，只判重叠会把同一处报错拆成两段，
	// 两段的 before/after 还会大面积重复，白占命中额度。
	lastEnd := -1
	for i := range lines {
		// lastEnd >= 0 才能判"与上一段相接"：初值 -1 表示还没有段，
		// 少了这个条件，第一行就会因为 i-ctx <= 0 被整段跳过（一个都命中不了）。
		if lastEnd >= 0 && i-ctxLines <= lastEnd+1 {
			continue
		}
		if !logLineMatches(lowered[i], patterns) {
			continue
		}
		from, to := i-ctxLines, i+ctxLines
		if from < 0 {
			from = 0
		}
		if to > len(lines)-1 {
			to = len(lines) - 1
		}
		matched++
		if len(hits) < maxHits {
			h := logHit{Pod: pod, Line: i + 1, Text: strings.TrimRight(lines[i], " \t")}
			if ctxLines > 0 {
				if i > from {
					h.Before = append(h.Before, lines[from:i]...)
				}
				if to > i {
					h.After = append(h.After, lines[i+1:to+1]...)
				}
			}
			hits = append(hits, h)
		}
		lastEnd = to
	}
	return hits, len(lines), matched
}

// logLineMatches 一行是否命中任一关键词。
//
// 大小写不敏感（日志里 ERROR / Error / error 都有）。patterns 由调用方保证已小写。
func logLineMatches(loweredLine string, patterns []string) bool {
	for _, p := range patterns {
		if p != "" && strings.Contains(loweredLine, p) {
			return true
		}
	}
	return false
}

// splitLogKeywords 把模型给的关键词串切成列表。
//
// 分隔符认英文逗号、中文逗号、顿号、分号与空白 —— 模型与人都会混着用，
// 只认逗号会让 `NullPointer 504` 被当成一个词，从而一条都匹配不上（静默 0 命中）。
func splitLogKeywords(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ',', '，', '、', ';', '；', ' ', '\t', '\n', '|':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// readLogCapped 读完整个流，但**最多**读 cap 字节；返回是否被截断。
//
// 与 integration 里取证用的 tailLogText 方向不同：那里保**尾部**字节（崩溃根因在最后几行），
// 这里保**头部**字节 —— 因为 TailLines 已经限定了窗口，窗口第一行就是"最近的开始"，
// 再按字节裁尾部反而会把最新那几行（也就是最该看的）切掉。
//
// 多读 1 字节用于判断"是不是还有更多"，否则刚好读满 cap 时无法区分
// 「正好 cap 字节」与「被截断」。
func readLogCapped(rc io.ReadCloser, capBytes int64) (string, bool) {
	if rc == nil {
		return "", false
	}
	buf, err := io.ReadAll(io.LimitReader(rc, capBytes+1))
	if err != nil && len(buf) == 0 {
		return "", false
	}
	truncated := int64(len(buf)) > capBytes
	if truncated {
		buf = buf[:capBytes]
	}
	return strings.ToValidUTF8(string(buf), ""), truncated
}
