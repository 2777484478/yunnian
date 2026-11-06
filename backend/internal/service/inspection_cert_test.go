package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/model"
)

// TestCollectCerts_BothQueried 两类证书必须**都查**，不能二选一。
//
// 这是 2026-11-06 用户报的那个 bug 的回归测试。旧实现是
// "先查 Ingress，结果为空才退到 API Server"，于是 **Ingress 一旦配了 TLS，
// 集群 API Server 证书就永远查不到**：
// aliyun-prd 的报告只显示已过期 2497 天的业务证书 aicard，
// 而真正要紧的 47.110.211.151:6443（2053-12 到期）一次都没检查过。
func TestCollectCerts_BothQueried(t *testing.T) {
	now := time.Now()
	env := &model.Environment{
		EnvKey: "aliyun-prd", Name: "阿里云生产环境",
		Cluster: "kubernetes-prd", Namespace: "default",
	}

	t.Run("Ingress 有证书时，集群证书仍必须查到", func(t *testing.T) {
		stub := &stubCluster{
			apiCert: &integration.TLSCertInfo{
				Kind: "apiserver", Secret: "（集群 API Server）",
				Hosts:    []string{"47.110.211.151:6443"},
				NotAfter: now.Add(9000 * 24 * time.Hour), DaysLeft: 9000,
			},
			ingressCerts: []integration.TLSCertInfo{{
				Kind: "ingress", Secret: "aicard",
				Hosts:    []string{"es-api.digiwincloud.com.cn"},
				NotAfter: now.Add(-2497 * 24 * time.Hour), DaysLeft: -2497,
			}},
		}
		certs, errMsg, notes := collectCerts(context.Background(), stub, env)
		if errMsg != "" {
			t.Errorf("不该有全局错误：%s", errMsg)
		}
		if len(certs) != 2 {
			t.Fatalf("应当两类都返回（2 张），实际 %d 张：%+v", len(certs), certs)
		}
		// 按剩余天数升序：最差的（业务证书 -2497）排第一，评分取它
		if certs[0].Kind != "ingress" {
			t.Errorf("排序应按 DaysLeft 升序，第一张应是已过期的业务证书，实际 %s", certs[0].Kind)
		}
		found := false
		for _, c := range certs {
			if c.Kind == "apiserver" {
				found = true
			}
		}
		if !found {
			t.Fatal("Ingress 配了 TLS 就把集群证书吞掉了 —— 这正是本次要修的 bug")
		}
		if len(notes) != 0 {
			t.Errorf("两处都成功时不该有附加说明：%v", notes)
		}
	})

	t.Run("集群证书探不到：算全局错误，但 Ingress 证书仍要展示", func(t *testing.T) {
		stub := &stubCluster{
			apiCertErr: errors.New("connection refused"),
			ingressCerts: []integration.TLSCertInfo{{
				Kind: "ingress", Secret: "aicard", Hosts: []string{"a.example.com"}, DaysLeft: 20,
			}},
		}
		certs, errMsg, _ := collectCerts(context.Background(), stub, env)
		if errMsg == "" {
			t.Error("集群证书探不到必须报全局错误：体检缺了主项，不能当正常")
		}
		if len(certs) != 1 || certs[0].Kind != "ingress" {
			t.Errorf("业务证书仍应展示，实际 %+v", certs)
		}
	})

	t.Run("Ingress 读不到：不算全局错误，集群证书照常", func(t *testing.T) {
		stub := &stubCluster{
			apiCert:    &integration.TLSCertInfo{Kind: "apiserver", Hosts: []string{"1.2.3.4:6443"}, DaysLeft: 100},
			ingressErr: errors.New("forbidden"),
		}
		certs, errMsg, notes := collectCerts(context.Background(), stub, env)
		if errMsg != "" {
			t.Errorf("业务证书读不到不该让整个证书节判失败（会让人以为集群证书也没查）：%s", errMsg)
		}
		if len(certs) != 1 || certs[0].Kind != "apiserver" {
			t.Errorf("集群证书应照常返回，实际 %+v", certs)
		}
		if len(notes) != 1 {
			t.Errorf("应当留一条口径说明，实际 %v", notes)
		}
	})

	t.Run("探到但没返回证书也要算失败", func(t *testing.T) {
		stub := &stubCluster{apiCert: nil}
		_, errMsg, _ := collectCerts(context.Background(), stub, env)
		if errMsg == "" {
			t.Error("API Server 返回空证书必须报出来，不能当成「这集群没证书」")
		}
	})
}

// TestScoreInspection_CertTitleShowsSource 证书问题的标题与建议必须区分来源。
//
// 集群证书与业务域名证书的处置完全不同：一个动 kube-apiserver，一个换 Secret。
// 只说"证书 X 天后到期"，用户不知道该找谁。
func TestScoreInspection_CertTitleShowsSource(t *testing.T) {
	now := time.Now()

	t.Run("集群证书", func(t *testing.T) {
		d := baseInspectData()
		d.Certs = []integration.TLSCertInfo{{
			Kind: "apiserver", Hosts: []string{"47.110.211.151:6443"},
			NotAfter: now.Add(48 * time.Hour), DaysLeft: 2,
		}}
		_, _, findings := scoreInspection(d)
		found := false
		for _, f := range findings {
			if f.Kind != "cert_expiring" {
				continue
			}
			found = true
			if !strings.Contains(f.Title, "集群 API Server") {
				t.Errorf("标题应带「集群 API Server」，实际 %q", f.Title)
			}
			if !strings.Contains(f.Action, "kube-apiserver") || !strings.Contains(f.Action, "kubeadm") {
				t.Errorf("处置建议应指向集群证书续期，而不是改 Ingress Secret：%q", f.Action)
			}
		}
		if !found {
			t.Fatal("缺少证书到期的 finding")
		}
	})

	t.Run("业务域名证书", func(t *testing.T) {
		d := baseInspectData()
		d.Certs = []integration.TLSCertInfo{{
			Kind: "ingress", Secret: "tls-a", Hosts: []string{"a.example.com"},
			NotAfter: now.Add(24 * time.Hour), DaysLeft: 1,
		}}
		_, _, findings := scoreInspection(d)
		found := false
		for _, f := range findings {
			if f.Kind != "cert_expiring" {
				continue
			}
			found = true
			if !strings.Contains(f.Title, "业务域名") {
				t.Errorf("标题应标明是业务域名证书，实际 %q", f.Title)
			}
			if !strings.Contains(f.Action, "Secret") {
				t.Errorf("处置建议应提到更新 Secret：%q", f.Action)
			}
		}
		if !found {
			t.Fatal("缺少证书到期的 finding")
		}
	})
}
