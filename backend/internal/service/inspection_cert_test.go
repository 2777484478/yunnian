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

// TestCollectCerts_OnlyAPIServer 巡检只查**集群 API Server** 的证书，
// 不碰 Ingress 的业务域名证书。
//
// 用户 2026-11-06 明确要求过两次："我要的巡检是集群的证书，又不是业务的域名证书。"
// 这条用 ingressCalls 计数器钉住：collectCerts 一次都不许调 IngressTLS。
// （光看返回值不够 —— 如果 IngressTLS 返回空列表，len 也是 1，测不出区别。）
func TestCollectCerts_OnlyAPIServer(t *testing.T) {
	now := time.Now()
	env := &model.Environment{
		EnvKey: "aliyun-prd", Name: "阿里云生产环境",
		Cluster: "kubernetes-prd", Namespace: "default",
	}

	t.Run("只调 APIServerCert，不碰 IngressTLS", func(t *testing.T) {
		stub := &stubCluster{
			apiCert: &integration.TLSCertInfo{
				Kind: "apiserver", Secret: "（集群 API Server）",
				Hosts:    []string{"47.110.211.151:6443"},
				NotAfter: now.Add(9891 * 24 * time.Hour), DaysLeft: 9891,
			},
			// 故意给一张已过期的业务证书：它绝不该出现在结果里，更不该参与评分。
			ingressCerts: []integration.TLSCertInfo{{
				Kind: "ingress", Secret: "aicard",
				Hosts:    []string{"es-api.digiwincloud.com.cn"},
				NotAfter: now.Add(-2497 * 24 * time.Hour), DaysLeft: -2497,
			}},
		}
		certs, errMsg := collectCerts(context.Background(), stub, env)
		if errMsg != "" {
			t.Errorf("不该报错：%s", errMsg)
		}
		if stub.ingressCalls != 0 {
			t.Errorf("巡检不该检查 Ingress 证书（用户明确要求），实际调用了 %d 次", stub.ingressCalls)
		}
		if len(certs) != 1 {
			t.Fatalf("只该有一张集群证书，实际 %d 张：%+v", len(certs), certs)
		}
		if certs[0].Kind != "apiserver" {
			t.Errorf("应当是集群 API Server 证书，实际 %s（%s）", certs[0].Kind, certs[0].Secret)
		}
		if certs[0].DaysLeft != 9891 {
			t.Errorf("应是没过期的那张（9891 天），实际 %d 天", certs[0].DaysLeft)
		}
	})

	t.Run("探不到：显式报错，不能当成「这集群没证书」", func(t *testing.T) {
		stub := &stubCluster{apiCertErr: errors.New("connection refused")}
		certs, errMsg := collectCerts(context.Background(), stub, env)
		if len(certs) != 0 {
			t.Errorf("探不到时不该有证书，实际 %+v", certs)
		}
		if errMsg == "" {
			t.Error("探不到必须显式说明，否则会被读成「这个集群没有证书」")
		}
	})

	t.Run("返回空也算失败", func(t *testing.T) {
		stub := &stubCluster{apiCert: nil}
		_, errMsg := collectCerts(context.Background(), stub, env)
		if errMsg == "" {
			t.Error("API Server 返回空证书必须报出来")
		}
	})
}

// TestScoreInspection_CertIsClusterScoped 证书问题的标题与建议都指向集群证书。
//
// 即使是构造数据（Kind 为空），也必须按集群证书表述 —— 因为巡检只查这一类，
// 说成"Secret xxx"会把人引去改 Ingress，而那是另一个东西。
func TestScoreInspection_CertIsClusterScoped(t *testing.T) {
	now := time.Now()
	d := baseInspectData()
	d.Certs = []integration.TLSCertInfo{{
		Hosts:    []string{"47.110.211.151:6443"},
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
		if !strings.Contains(f.Action, "kubeadm") || !strings.Contains(f.Action, "kube-apiserver") {
			t.Errorf("建议应指向集群证书续期，实际 %q", f.Action)
		}
		if strings.Contains(f.Action, "更新这个 Secret") {
			t.Errorf("不该让人去改 Ingress 的 Secret：%q", f.Action)
		}
		if !strings.Contains(f.Detail, "集群 API Server") {
			t.Errorf("明细应说明是集群证书，实际 %q", f.Detail)
		}
	}
	if !found {
		t.Fatal("缺少证书到期的 finding")
	}
}

// TestCertAlertLine_ClusterOnly 证书快到期时的置顶提醒只说集群证书。
func TestCertAlertLine_ClusterOnly(t *testing.T) {
	now := time.Now()
	d := baseInspectData()
	d.Certs = []integration.TLSCertInfo{{
		Hosts:    []string{"47.110.211.151:6443"},
		NotAfter: now.Add(72 * time.Hour), DaysLeft: 3,
	}}
	line := certAlertLine(d)
	if line == "" {
		t.Fatal("快到期时置顶提醒不能为空（用户要求过「快到期要提醒」）")
	}
	if !strings.Contains(line, "集群 API Server") {
		t.Errorf("提醒应说明是集群证书：%q", line)
	}
	if strings.Contains(line, "Secret") {
		t.Errorf("集群证书没有 Secret，不该出现 Secret 字样：%q", line)
	}
	if !strings.Contains(line, "整个集群") {
		t.Errorf("后果应说清是整集群级：%q", line)
	}
}
