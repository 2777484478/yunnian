package service

import (
	"context"
	"testing"

	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/model"
)

// 纳管服务时，初始副本数必须**跟随集群现状**，不能拍一个默认值。
//
// 这条链路上曾经有个很贵的默认值：纳管时写死 2，而集群里那个 Deployment 是
// CI / 手工部署的、副本数是 1 —— 库里的 2 一直躺在那里不生效，直到**第一次通过
// 平台发布**时（`CreateDeployment` 里的 `replicas := svc.Replicas`）被写进集群。
// 用户看到的就是"我明明只有 1 个实例，一发就成了 2 个"，而这个 2 从纳管那天就存着了。
// （2026-10-16 用户报的"第一次批量发布为什么默认给我起 2 个"。）

func TestClusterReplicasOf(t *testing.T) {
	ctx := context.Background()
	env := &model.Environment{EnvKey: "test", Cluster: "cluster-1", Namespace: "default"}
	projs := []model.Project{{ProjectKey: "svc-a"}, {ProjectKey: "svc-b"}}

	t.Run("读得到就用集群的值", func(t *testing.T) {
		cd := newCDForTest(&stubCluster{
			describeState: &integration.WorkloadState{Exists: true, Replicas: 3},
		}, nil)
		got := cd.clusterReplicasOf(ctx, env, projs)
		if got["svc-a"] != 3 || got["svc-b"] != 3 {
			t.Fatalf("应取集群实际的 3，得到 %+v", got)
		}
	})

	t.Run("集群里没有这个工作负载时不写进结果", func(t *testing.T) {
		// 关键：**不能当成 0**。0 会被下游当"缩到 0 个实例"，那是停服。
		// 不写进结果，调用方才能退回默认值。
		cd := newCDForTest(&stubCluster{
			describeState: &integration.WorkloadState{Exists: false, Replicas: 0},
		}, nil)
		got := cd.clusterReplicasOf(ctx, env, projs)
		if len(got) != 0 {
			t.Fatalf("不存在的工作负载不该给出副本数，得到 %+v", got)
		}
	})

	t.Run("环境没接集群时不做徒劳的逐个查询", func(t *testing.T) {
		// 每个 Describe 都会因同样的原因失败，逐个试只是白等一轮超时。
		cd := newCDForTest(&stubCluster{
			describeState: &integration.WorkloadState{Exists: true, Replicas: 5},
		}, nil)
		got := cd.clusterReplicasOf(ctx, &model.Environment{EnvKey: "test"}, projs)
		if len(got) != 0 {
			t.Fatalf("环境没配集群/namespace 时应直接返回空，得到 %+v", got)
		}
	})
}

func TestReplicaInit(t *testing.T) {
	cases := []struct {
		name        string
		fromCluster int
		want        int
	}{
		{"集群里是 3 就跟着 3", 3, 3},
		{"集群里是 1 就跟着 1（不放大）", 1, 1},
		// 读不到集群时退回 1：不用 0（那是停服），也不用 2（凭空多占一份资源，
		// 且与用户"我就一个实例"的预期相反 —— 那个 2 就是本函数存在的原因）。
		{"读不到时退回 1，不是 2", 0, 1},
		{"负数同样退回 1", -5, 1},
	}
	for _, c := range cases {
		if got := replicaInit(c.fromCluster); got != c.want {
			t.Errorf("%s：replicaInit(%d) = %d，期望 %d", c.name, c.fromCluster, got, c.want)
		}
	}
}
