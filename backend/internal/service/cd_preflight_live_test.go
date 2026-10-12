//go:build live

// 打**真实镜像仓库**验证发单前预检的两条分支。全程只读：只发 GET，不推不改。
//
// 为什么非要在真仓库上再验一遍：单元测试用的是自己搭的桩，而"仓库对不存在的 tag
// 究竟回 404 还是回 401/200"是仓库实现决定的 —— 猜错的话预检要么永不生效、
// 要么把正常发布全拦下来。这条测试就是拿真话来钉死这个假设。
//
// 跑法：cd backend && go test -tags live ./internal/service/ -run TestLivePreflight -v
// 前置：能直连配置里的 Harbor（本机需清 HTTP_PROXY 并让该域名走 NO_PROXY）。
package service

import (
	"context"
	"errors"
	"testing"

	"github.com/yunnian/yunnian/internal/config"
	"github.com/yunnian/yunnian/internal/integration"
)

func TestLivePreflight(t *testing.T) {
	cfg, err := config.Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	if !cfg.Integration.Harbor.Enabled {
		t.Skip("配置里没开 Harbor，跳过")
	}
	reg := integration.NewHarborRegistry(cfg.Integration.Harbor)
	cd := newCDForTest(&stubCluster{}, reg)
	host := cfg.Integration.Harbor.Registry
	if host == "" {
		t.Skip("配置里没写 registry 地址，跳过")
	}

	// 这个 repo 现在确实存在（demobasic 是纳管项目之一）；换成别的也行，
	// 只要它在仓库里。测试只读，不需要它一定存在多久。
	const repo = "escloud/demobasic"
	ctx := context.Background()

	t.Run("存在的 tag 必须放行", func(t *testing.T) {
		info, rerr := reg.Resolve(ctx, repo, "1.2.0.2761")
		if rerr != nil || info == nil {
			t.Skipf("拿不到参考 tag（%v），跳过本节", rerr)
		}
		if perr := cd.preflightImage(ctx, host+"/"+repo+":1.2.0.2761"); perr != nil {
			t.Fatalf("仓库里确实有这个 tag，预检不该拦：%v", perr)
		}
		t.Logf("已放行：%s 的 digest=%s arch=%s", info.Tag, info.Digest, info.Arch)
	})

	t.Run("仓库里没有的 tag 必须拒绝，且是 404 这一种", func(t *testing.T) {
		const bogus = "9.9.9-preflight-probe-not-exist"
		// 先确认仓库对它的回答确实是 404（而不是 401/200）——这条是整件事的前提
		_, rerr := reg.Resolve(ctx, repo, bogus)
		if rerr == nil {
			t.Fatalf("仓库竟然回了一个不存在的 tag（%s）：预检的前提不成立，"+
				"请复核仓库行为后再改判定", bogus)
		}
		if !errors.Is(rerr, integration.ErrTagNotFound) {
			t.Fatalf("仓库没回 404（%v）：预检不敢用它判定'必然失败'，"+
				"否则仓库一抖动就会把正常发布全拦下来", rerr)
		}

		perr := cd.preflightImage(ctx, host+"/"+repo+":"+bogus)
		if perr == nil {
			t.Fatal("仓库明确说没有这个 tag，预检必须拒绝")
		}
		t.Logf("已拒绝：%v", perr)
	})

	t.Run("平台不认的仓库必须放行（不越界去问别人）", func(t *testing.T) {
		if perr := cd.preflightImage(ctx, "docker.io/library/nginx:1.25"); perr != nil {
			t.Fatalf("别的仓库不该拦：%v", perr)
		}
	})
}
