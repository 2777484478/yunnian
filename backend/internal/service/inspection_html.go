package service

import (
	"fmt"
	"html"
	"strings"
)

// renderInspectionHTML 生成可下载的彩色巡检报告。
//
// 三个约束：
//  1. **自包含**。样式内联、不引外部资源 —— 用户会把它下载下来双击打开，
//     甚至发到群里。依赖外链的 HTML 换个环境就变成白底黑字。
//  2. **彩色但不花哨**。红/黄/绿只用来表达状态，其余保持中性灰；
//     整页都是色块反而看不出哪里要紧。
//  3. **"没读到"要显式画出来**。集群不可达时给一行灰底说明，
//     绝不能渲染成 0% —— 那会被读成"资源很闲"。
func renderInspectionHTML(
	d inspectData, score int, level, summary string,
	findings []InspectFinding, st InspectStats, c Caller,
) string {
	var b strings.Builder
	esc := html.EscapeString

	lvColor, lvBg, lvText, lvIcon := levelStyle(level)

	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>` + esc(d.Env.Name) + ` 巡检报告</title><style>`)
	b.WriteString(inspectionCSS())
	b.WriteString(`</style></head><body><div class="wrap">`)

	// ---------------- 头部：环境 + 健康分
	b.WriteString(`<div class="hero">`)
	b.WriteString(`<div class="hero__main">`)
	b.WriteString(`<div class="hero__env">` + esc(d.Env.Name) + `</div>`)
	b.WriteString(`<div class="hero__key"><code>` + esc(d.Env.EnvKey) + `</code>`)
	if d.Env.Namespace != "" {
		b.WriteString(` · 命名空间 <code>` + esc(d.Env.Namespace) + `</code>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<div class="hero__time">巡检时间 ` + d.Now.Format("2006-01-02 15:04") +
		` · 统计窗口 ` + fmt.Sprintf("%d 天", d.Window/24) + `</div>`)
	b.WriteString(`</div>`)
	b.WriteString(fmt.Sprintf(
		`<div class="hero__score" style="background:%s;color:%s"><div class="num">%d</div><div class="lbl">健康分</div></div>`,
		lvBg, lvColor, score))
	b.WriteString(`</div>`)

	// ---------------- 结论条
	b.WriteString(fmt.Sprintf(`<div class="verdict" style="background:%s;color:%s;border-color:%s">`,
		lvBg, lvText, lvColor))
	b.WriteString(`<span class="verdict__icon">` + lvIcon + `</span>` + esc(summary))
	b.WriteString(`</div>`)

	// ---------------- KPI 条
	b.WriteString(`<div class="kpis">`)
	b.WriteString(kpi(fmt.Sprintf("%d", st.SvcTotal), "服务总数", ""))
	b.WriteString(kpi(fmt.Sprintf("%d", st.SvcNotReady), "未就绪", pick(st.SvcNotReady > 0, "bad", "")))
	// 读不到证书时**不能显示 0**：那会被读成"这个环境没有证书"，
	// 与"有但没读到"是两件事。用 — 明确表示未知。
	certKPI := fmt.Sprintf("%d", st.Certs)
	if d.CertsErr != "" {
		certKPI = "—"
	}
	b.WriteString(kpi(certKPI, "集群证书", pick(st.CertExpiring > 0, "warn", "")))
	b.WriteString(kpi(fmt.Sprintf("%d", st.Deploys), fmt.Sprintf("近 %d 天部署", d.Window/24), ""))
	b.WriteString(`</div>`)

	// ---------------- 证书快到期时置顶提醒（用户特别要求：快到期要提醒）
	if warn := certAlertLine(d); warn != "" {
		b.WriteString(`<div class="alert alert--warn"><b>证书即将到期</b>` + esc(warn) + `</div>`)
	}

	// ---------------- 需要处理 / 提示
	crit := filterFindings(findings, "critical")
	warnF := filterFindings(findings, "warning")
	info := filterFindings(findings, "info")

	if len(crit) > 0 {
		b.WriteString(`<h2 class="sec sec--crit">需要处理</h2>`)
		for _, f := range crit {
			b.WriteString(renderFinding(f, "critical"))
		}
	}
	if len(warnF) > 0 {
		b.WriteString(`<h2 class="sec sec--warn">值得关注</h2>`)
		for _, f := range warnF {
			b.WriteString(renderFinding(f, "warning"))
		}
	}
	if len(crit) == 0 && len(warnF) == 0 {
		b.WriteString(`<div class="alert alert--ok"><b>未发现异常</b>本次巡检没有需要处理的项目。</div>`)
	}
	if len(info) > 0 {
		b.WriteString(`<h2 class="sec">说明</h2>`)
		for _, f := range info {
			b.WriteString(renderFinding(f, "info"))
		}
	}

	// ---------------- 集群资源
	b.WriteString(`<h2 class="sec">集群资源</h2>`)
	if d.NodesErr != "" {
		b.WriteString(`<div class="alert alert--muted"><b>没读到集群资源</b>` + esc(d.NodesErr) +
			`<div class="mt6">这不代表资源充足 —— 是这次没能从集群取到数据。</div></div>`)
	} else {
		b.WriteString(`<table><thead><tr><th>节点</th><th>状态</th><th>CPU</th><th>内存</th></tr></thead><tbody>`)
		for _, n := range d.Nodes.NodeList {
			cpuPct, memPct := 0.0, 0.0
			if n.CPUCores > 0 {
				cpuPct = n.CPUUsage / n.CPUCores * 100
			}
			if n.MemBytes > 0 {
				memPct = float64(n.MemUsage) / float64(n.MemBytes) * 100
			}
			status := `<span class="dot dot--ok"></span>Ready`
			if !n.Ready {
				status = `<span class="dot dot--bad"></span>NotReady`
			}
			cpuCell := gauge(cpuPct, fmt.Sprintf("%.1f / %.1f 核", n.CPUUsage, n.CPUCores))
			memCell := gauge(memPct, fmt.Sprintf("%s / %s", humanBytes(float64(n.MemUsage)), humanBytes(float64(n.MemBytes))))
			if !n.HasMetrics {
				cpuCell, memCell = `<span class="muted">读不到</span>`, `<span class="muted">读不到</span>`
			}
			b.WriteString(`<tr><td><code>` + esc(n.Name) + `</code></td><td>` + status + `</td><td>` + cpuCell + `</td><td>` + memCell + `</td></tr>`)
		}
		if len(d.Nodes.NodeList) == 0 {
			b.WriteString(`<tr><td colspan="4" class="muted">该集群没有可列出节点</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
		if !d.Nodes.MetricsAvailable {
			b.WriteString(`<div class="hint">集群没有读到用量指标（未装 metrics-server 或凭据无权读），上表的 CPU/内存显示为「读不到」。</div>`)
		}
	}

	// ---------------- 服务健康
	b.WriteString(`<h2 class="sec">服务健康</h2>`)
	b.WriteString(fmt.Sprintf(
		`<div class="hint">共 <b>%d</b> 个服务：就绪 <b>%d</b>，未就绪 <b class="%s">%d</b>，状态未刷新 <b class="%s">%d</b>。</div>`,
		st.SvcTotal, st.SvcReady, pick(st.SvcNotReady > 0, "t-bad", ""), st.SvcNotReady,
		pick(st.SvcDegraded > 0, "t-warn", ""), st.SvcDegraded))
	if len(d.NotReady) > 0 || len(d.Stale) > 0 || len(d.Idle) > 0 {
		b.WriteString(`<table><thead><tr><th>服务</th><th>集群状态</th><th>副本</th><th>镜像</th><th>说明</th></tr></thead><tbody>`)
		for _, pr := range d.NotReady {
			note := fmt.Sprintf("集群 %d/%d 就绪", pr.Ready, pr.Replicas)
			if !pr.Exists {
				note = "集群中不存在该工作负载"
			}
			b.WriteString(probeRow(pr, "bad", note))
		}
		for _, pr := range d.Stale {
			// 集群是好的，只是平台记录没跟上 —— 用黄色，绝不显示成红色故障
			b.WriteString(probeRow(pr, "warn", fmt.Sprintf("集群已就绪（%d/%d），平台记为 %s", pr.Ready, pr.Replicas, pr.platStatus())))
		}
		for _, pr := range d.Idle {
			// 期望 0 副本：灰色，不是告警。写清"未运行"而不是"未就绪"，
			// 因为前者可能是有意的，后者一定是问题。
			b.WriteString(probeRow(pr, "muted", "副本数为 0，当前未运行（若本应在跑，请调回 1 副本以上）"))
		}
		b.WriteString(`</tbody></table>`)
	}
	if d.ProbeFailed > 0 {
		b.WriteString(fmt.Sprintf(`<div class="hint t-warn">另有 %d 个服务没能从集群探到状态（权限 / 网络 / 超时），未计入上表 —— 不等于正常。</div>`, d.ProbeFailed))
	}

	// ---------------- 集群证书
	//
	// **只查集群 API Server 自身那一张**（用户 2026-11-06 两次明确：不检查 Ingress
	// 业务域名证书）。所以表里没有「来源 / Secret」列 —— 只有一种来源，列出来是噪音。
	b.WriteString(`<h2 class="sec">集群证书</h2>`)
	if d.CertsErr != "" {
		b.WriteString(`<div class="alert alert--muted"><b>没读到集群证书</b>` + esc(d.CertsErr) +
			`<div class="mt6">这不代表证书没问题 —— 是这次没能从 kubeconfig 的 server 地址取到证书。</div></div>`)
	}
	if len(d.Certs) > 0 {
		b.WriteString(`<table><thead><tr><th>API Server 地址</th><th>签发者</th><th>到期时间</th><th>剩余</th></tr></thead><tbody>`)
		for _, ct := range d.Certs {
			// 逐个 esc 再拼 <br>：先 join 再 esc 会把换行标签转义成 &lt;br&gt; 原样显示。
			addr := `<span class="muted">—</span>`
			if len(ct.Hosts) > 0 {
				parts := make([]string, 0, len(ct.Hosts))
				for _, h := range ct.Hosts {
					parts = append(parts, esc(h))
				}
				addr = strings.Join(parts, `<br>`)
			}
			issuer := `<span class="muted">—</span>`
			if v := strings.TrimSpace(ct.Issuer); v != "" {
				issuer = esc(v)
			}
			if ct.Err != "" {
				b.WriteString(`<tr><td>` + addr + `</td><td colspan="3"><span class="t-warn">读不出来：` + esc(ct.Err) + `</span></td></tr>`)
				continue
			}
			left := fmt.Sprintf("%d 天", ct.DaysLeft)
			cls := "dot--ok"
			switch {
			case ct.DaysLeft < 0:
				left, cls = fmt.Sprintf("已过期 %d 天", -ct.DaysLeft), "dot--bad"
			case ct.DaysLeft <= 7:
				cls = "dot--bad"
			case ct.DaysLeft <= 30:
				cls = "dot--warn"
			}
			b.WriteString(`<tr><td>` + addr + `</td><td>` + issuer +
				`</td><td>` + ct.NotAfter.Format("2006-01-02") +
				`</td><td><span class="dot ` + cls + `"></span>` + left + `</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	} else if d.CertsErr == "" {
		b.WriteString(`<div class="alert alert--muted">没有可检查的集群证书。</div>`)
	}

	// ---------------- 页脚：口径
	b.WriteString(`<div class="foot">`)
	b.WriteString(`<div><b>数据口径</b></div>`)
	b.WriteString(`<div>· 集群资源：巡检当时从集群实时读取（<code>` + esc(d.Env.Cluster) + `</code>）</div>`)
	b.WriteString(`<div>· 集群证书：取 kubeconfig 里 server 的地址做 TLS 握手（等价于 <code>curl -k -v https://&lt;server&gt;:6443</code>），` +
		`巡检当时现场读取。<b>不检查 Ingress 的业务域名证书</b></div>`)
	b.WriteString(`<div>· 服务状态与部署记录：平台数据库。服务的「就绪数」由平台定期同步，可能略滞后于集群</div>`)
	if d.NodesErr != "" || d.CertsErr != "" {
		b.WriteString(`<div class="t-warn">· 本次有项目没读到（见上方灰色说明），报告不完整</div>`)
	}
	by := "未知"
	if c.User != nil {
		by = c.User.Name
		if by == "" {
			by = c.User.Username
		}
	}
	b.WriteString(`<div class="mt6">生成人 ` + esc(by) + ` · 报告编号在同名下载文件中不易变更，如需追溯请以平台「巡检报告」页为准</div>`)
	b.WriteString(`</div>`)

	b.WriteString(`</div></body></html>`)
	return b.String()
}

func renderFinding(f InspectFinding, cls string) string {
	var b strings.Builder
	b.WriteString(`<div class="finding finding--` + cls + `">`)
	b.WriteString(`<div class="finding__head"><span class="finding__title">` + html.EscapeString(f.Title) + `</span>`)
	if f.Penalty > 0 {
		b.WriteString(fmt.Sprintf(`<span class="pen">-%d</span>`, f.Penalty))
	}
	b.WriteString(`</div>`)
	if f.Detail != "" {
		b.WriteString(`<div class="finding__detail">` + html.EscapeString(f.Detail) + `</div>`)
	}
	if f.Action != "" {
		b.WriteString(`<div class="finding__action"><b>建议</b>` + html.EscapeString(f.Action) + `</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

// probeRow 一个服务的一行。**状态列写的是集群实测**，不是平台记录。
func probeRow(pr svcProbe, cls, note string) string {
	esc := html.EscapeString
	tag := `<span class="muted">无镜像记录</span>`
	if t := strings.TrimSpace(pr.Svc.CurrentTag); t != "" {
		tag = `<code>` + esc(t) + `</code>`
	}
	statusCls := "t-bad"
	switch cls {
	case "warn":
		statusCls = "t-warn"
	case "muted":
		statusCls = "muted"
	}
	status := "未就绪"
	switch {
	case !pr.Exists:
		status = "不存在"
	case pr.Replicas == 0:
		// 期望 0 副本：不是"未就绪"，也不是"运行中"
		status = "未运行"
	case pr.healthy():
		status = "运行中"
	}
	return fmt.Sprintf(
		`<tr><td><code>%s</code></td><td><span class="%s">%s</span></td><td>%d / %d</td><td>%s</td><td class="muted">%s</td></tr>`,
		esc(pr.Svc.ProjectKey), statusCls, esc(status), pr.Ready, pr.Replicas, tag, esc(note))
}

func filterFindings(in []InspectFinding, level string) []InspectFinding {
	out := make([]InspectFinding, 0, len(in))
	for _, f := range in {
		if f.Level == level {
			out = append(out, f)
		}
	}
	return out
}

func levelStyle(level string) (color, bg, text, icon string) {
	switch level {
	case "critical":
		return "#cf1322", "#fff1f0", "#a8071a", "✕"
	case "warning":
		return "#d46b08", "#fffbe6", "#ad4e00", "!"
	}
	return "#237804", "#f6ffed", "#135200", "✓"
}

func kpi(value, label, cls string) string {
	return `<div class="kpi ` + cls + `"><b>` + value + `</b><span>` + label + `</span></div>`
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// gauge 一格"百分比 + 明细"的用量条。75/90 是全局判读阈值，与 scoreInspection 同源。
func gauge(pct float64, detail string) string {
	cls := "bar--ok"
	switch {
	case pct >= 90:
		cls = "bar--bad"
	case pct >= 75:
		cls = "bar--warn"
	}
	if pct > 100 {
		pct = 100
	}
	return fmt.Sprintf(
		`<div class="gauge"><div class="bar"><i class="%s" style="width:%.0f%%"></i></div><span>%.0f%% · %s</span></div>`,
		cls, pct, pct, html.EscapeString(detail))
}

func humanBytes(v float64) string {
	const gb = 1024 * 1024 * 1024
	if v >= gb {
		return fmt.Sprintf("%.1f GB", v/gb)
	}
	return fmt.Sprintf("%.0f MB", v/(1024*1024))
}

// certAlertLine 证书快到期时的一句话（放在最上面，不用翻到下面才看见）。
func certAlertLine(d inspectData) string {
	if len(d.Certs) == 0 {
		return ""
	}
	w := d.Certs[0]
	if w.Err != "" || w.DaysLeft > 30 {
		return ""
	}
	// 只报集群证书：过期后果是整个集群不可访问，说法必须与业务域名证书区分开
	// （不能写成"HTTPS 入口坏了"，那会让人以为只是某个域名的事）。
	host := "未取到地址"
	if len(w.Hosts) > 0 {
		host = strings.Join(w.Hosts, "、")
	}
	if w.DaysLeft < 0 {
		return fmt.Sprintf("集群 API Server（%s）的证书已过期 %d 天 —— 平台与 kubectl 都已经连不上集群。", host, -w.DaysLeft)
	}
	return fmt.Sprintf("集群 API Server（%s）的证书还有 %d 天到期（%s），过期后整个集群将无法访问。",
		host, w.DaysLeft, w.NotAfter.Format("2006-01-02"))
}

func inspectionCSS() string {
	return `
*{box-sizing:border-box}
body{margin:0;background:#f4f6f9;font-family:-apple-system,BlinkMacSystemFont,"PingFang SC","Microsoft YaHei",sans-serif;color:#2c313a;line-height:1.6}
.wrap{max-width:860px;margin:0 auto;padding:28px 20px 40px}
.hero{display:flex;align-items:center;justify-content:space-between;gap:20px;background:#fff;border:1px solid #eceef2;border-radius:14px;padding:22px 24px}
.hero__env{font-size:20px;font-weight:600}
.hero__key{color:#8a909a;font-size:13px;margin-top:2px}
.hero__key code{background:#f4f6f9;padding:1px 6px;border-radius:4px;font-size:12px}
.hero__time{color:#a0a6b0;font-size:12.5px;margin-top:8px}
.hero__score{width:96px;height:96px;border-radius:50%;display:flex;flex-direction:column;align-items:center;justify-content:center;flex:none}
.hero__score .num{font-size:30px;font-weight:700;line-height:1}
.hero__score .lbl{font-size:11px;opacity:.8;margin-top:2px}
.verdict{display:flex;align-items:center;gap:8px;margin-top:14px;padding:12px 16px;border:1px solid;border-radius:10px;font-size:14px;font-weight:500}
.verdict__icon{width:18px;height:18px;border-radius:50%;background:rgba(0,0,0,.06);display:inline-flex;align-items:center;justify-content:center;font-size:11px;flex:none}
.kpis{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin-top:14px}
.kpi{background:#fff;border:1px solid #eceef2;border-radius:12px;padding:14px 16px}
.kpi b{display:block;font-size:22px;font-weight:600;line-height:1.2}
.kpi span{font-size:12px;color:#8a909a}
.kpi.bad b{color:#cf1322}
.kpi.warn b{color:#d46b08}
h2.sec{font-size:14px;font-weight:600;margin:26px 0 10px;padding-left:9px;border-left:3px solid #1677ff;line-height:1.2}
h2.sec--crit{border-color:#ff4d4f}
h2.sec--warn{border-color:#faad14}
.finding{background:#fff;border:1px solid #eceef2;border-left:3px solid #d9dde3;border-radius:10px;padding:14px 16px;margin-bottom:10px}
.finding--critical{border-left-color:#ff4d4f}
.finding--warning{border-left-color:#faad14}
.finding--info{border-left-color:#1677ff}
.finding__head{display:flex;align-items:center;gap:8px}
.finding__title{font-weight:600;font-size:14px}
.pen{background:#fff1f0;color:#cf1322;font-size:11px;padding:1px 6px;border-radius:4px;font-weight:500}
.finding__detail{color:#5c6470;font-size:13px;margin-top:6px}
.finding__action{color:#5c6470;font-size:13px;margin-top:8px;background:#f8f9fb;border-radius:8px;padding:8px 10px}
.finding__action b{color:#2c313a;margin-right:6px;font-weight:600}
.alert{border-radius:10px;padding:12px 16px;font-size:13px;margin:10px 0}
.alert b{display:block;margin-bottom:2px;font-weight:600}
.alert--warn{background:#fffbe6;border:1px solid #ffe58f;color:#ad4e00}
.alert--ok{background:#f6ffed;border:1px solid #b7eb8f;color:#135200}
.alert--muted{background:#fafafa;border:1px solid #eceef2;color:#8a909a}
table{width:100%;border-collapse:separate;border-spacing:0;background:#fff;border:1px solid #eceef2;border-radius:10px;overflow:hidden;font-size:13px}
th{background:#f8f9fb;text-align:left;font-weight:500;color:#8a909a;font-size:12px;padding:9px 12px;border-bottom:1px solid #eceef2}
td{padding:10px 12px;border-bottom:1px solid #f3f4f7;vertical-align:middle}
tr:last-child td{border-bottom:none}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;color:#5c6470}
.dot{display:inline-block;width:7px;height:7px;border-radius:50%;margin-right:5px;vertical-align:1px}
.dot--ok{background:#52c41a}.dot--warn{background:#faad14}.dot--bad{background:#ff4d4f}
.t-bad{color:#cf1322}.t-warn{color:#d46b08}.muted{color:#a0a6b0}
.gauge{display:flex;flex-direction:column;gap:3px;min-width:110px}
.bar{height:5px;background:#f0f1f4;border-radius:3px;overflow:hidden}
.bar i{display:block;height:100%;border-radius:3px}
.bar--ok i{background:#52c41a}.bar--warn i{background:#faad14}.bar--bad i{background:#ff4d4f}
.gauge span{font-size:11.5px;color:#8a909a}
.hint{font-size:12.5px;color:#8a909a;margin:8px 0}
.foot{margin-top:30px;padding-top:14px;border-top:1px solid #eceef2;font-size:12px;color:#a0a6b0;line-height:1.9}
.foot b{color:#8a909a;font-weight:600}
.mt6{margin-top:6px}
@media (max-width:640px){.kpis{grid-template-columns:repeat(2,1fr)}.hero{flex-direction:column;align-items:flex-start}}
`
}
