package integration

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TLSCertInfo 一张从集群里读出来的 TLS 证书。
//
// 为什么从 Ingress 读而不是去探域名：探域名（`tls.Dial`）要求平台能访问那个域名，
// 而集群往往在内网、域名只在集群内解析。读 Ingress → Secret 是**不需要额外网络**
// 就能拿到到期时间的办法，而且拿到的正是集群实际在用的那张证书。
type TLSCertInfo struct {
	// Secret 证书所在的 Secret 名（同一张证书常被多个 Ingress 引用，按 Secret 去重）
	Secret string `json:"secret"`
	// Hosts 该 Secret 覆盖的域名（来自 Ingress 的 tls.hosts）
	Hosts []string `json:"hosts"`
	// Ingresses 引用它的 Ingress 名，便于定位改哪里
	Ingresses []string `json:"ingresses"`
	// Subject / Issuer 证书主体与签发者（CN）
	Subject string `json:"subject"`
	Issuer  string `json:"issuer"`
	// NotAfter 到期时间；NotBefore 生效时间
	NotAfter  time.Time `json:"notAfter"`
	NotBefore time.Time `json:"notBefore"`
	// DaysLeft 距到期天数（负数表示已过期）。由 NotAfter 算出，方便调用方直接用。
	DaysLeft int `json:"daysLeft"`
	// Err 读不到/解析失败时的原因。**有它就不是"没有证书"**，
	// 而是"有这个证书但我们读不出来"——两者在巡检结论里必须分开。
	Err string `json:"err,omitempty"`
}

// IngressTLS 列出命名空间内所有 Ingress 引用的 TLS 证书及到期时间。
//
// 只读：List Ingress + Get Secret，没有任何写动作。
//
// 三种"查不到"必须区分，否则巡检报告会把它们混成一句"无证书"：
//   - 没有 Ingress 或 Ingress 没配 tls → 返回空列表 + 无错误（这个环境确实没上 HTTPS）
//   - Ingress 配了 tls 但 Secret 不存在 → 该项带 Err，不是空列表
//   - 根本没有权限读 Ingress/Secret → 返回 error（调用方应把整节标成"读取失败"）
func (k *K8sCluster) IngressTLS(ctx context.Context, ref WorkloadRef) ([]TLSCertInfo, error) {
	// ⚠️ 只校验命名空间，**不校验工作负载名**：证书是按命名空间读的，
	// 与具体工作负载无关。用 ref.validate() 会把"扫整个环境的证书"
	// 误判成"工作负载名为空"而直接失败（实测踩过：证书一节永远显示读不到）。
	if strings.TrimSpace(ref.Namespace) == "" {
		return nil, errors.New("命名空间为空，无法读取 Ingress 证书")
	}
	cs, _, err := k.clientset(ctx, ref.ClusterRef)
	if err != nil {
		return nil, err
	}

	list, err := cs.NetworkingV1().Ingresses(ref.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%s", classifyK8sErrText(err))
	}

	// 先按 Secret 聚合：同一张证书挂多个 Ingress 是常态，逐 Ingress 报会重复。
	type agg struct {
		hosts     []string
		ingresses []string
	}
	bySecret := map[string]*agg{}
	order := make([]string, 0, len(list.Items))
	for i := range list.Items {
		ing := &list.Items[i]
		for _, t := range ing.Spec.TLS {
			name := strings.TrimSpace(t.SecretName)
			if name == "" {
				continue
			}
			a, ok := bySecret[name]
			if !ok {
				a = &agg{}
				bySecret[name] = a
				order = append(order, name)
			}
			a.ingresses = append(a.ingresses, ing.Name)
			for _, h := range t.Hosts {
				if h = strings.TrimSpace(h); h != "" && !containsStr(a.hosts, h) {
					a.hosts = append(a.hosts, h)
				}
			}
		}
	}
	if len(order) == 0 {
		return []TLSCertInfo{}, nil
	}

	out := make([]TLSCertInfo, 0, len(order))
	for _, secretName := range order {
		a := bySecret[secretName]
		info := TLSCertInfo{Secret: secretName, Hosts: a.hosts, Ingresses: a.ingresses}

		sec, err := cs.CoreV1().Secrets(ref.Namespace).Get(ctx, secretName, metav1.GetOptions{})
		if err != nil {
			// 带 Err 返回而不是跳过：Secret 被删了/名字写错，正是需要注意的问题。
			info.Err = classifyK8sErrText(err)
			out = append(out, info)
			continue
		}
		raw, ok := sec.Data["tls.crt"]
		if !ok || len(raw) == 0 {
			info.Err = "Secret 里没有 tls.crt"
			out = append(out, info)
			continue
		}
		// tls.crt 可能是证书链（叶子在前）。只解析第一张 —— 到期判定看叶子证书。
		block, _ := pem.Decode(raw)
		if block == nil {
			info.Err = "tls.crt 不是 PEM 格式"
			out = append(out, info)
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			info.Err = "证书解析失败：" + err.Error()
			out = append(out, info)
			continue
		}
		info.Subject = cert.Subject.CommonName
		if info.Subject == "" {
			info.Subject = cert.Subject.String()
		}
		info.Issuer = cert.Issuer.CommonName
		info.NotBefore = cert.NotBefore
		info.NotAfter = cert.NotAfter
		info.DaysLeft = int(time.Until(cert.NotAfter).Hours() / 24)
		out = append(out, info)
	}

	// 快到期的排前面：巡检报告里最该先看到的就是"哪张证书要到期了"。
	sort.SliceStable(out, func(i, j int) bool { return out[i].DaysLeft < out[j].DaysLeft })
	return out, nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
