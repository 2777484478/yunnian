package service

import (
	"context"
	"errors"
	"testing"

	"github.com/2777484478/yunnian/internal/model"
)

// 单号生成的基本语义：DP + 日期 + 当日序号，从已有的最大号往下排。
//
// ⚠️ 这里**故意不测"并发调用得到不同号"** —— 那个假设是错的，我第一版就是这么写的，
// 结果红给我看了一遍：`nextDeployNo` 只**查**最大号、**不写入**，
// 号是在调用方 INSERT 时才落库的。所以并发（甚至串行连调两次）都会拿到同一个号，
// 这不是 bug，是它的契约 —— 防撞靠的是调用方 `CreateDeployment` 里的**重试**
// （撞了重算，那时前一笔已经落库，重算自然拿到新号）。
//
// 并发那条链路的验证放在实机做（真 MySQL 的错误文本才会被判成撞号），见提交说明。
func TestNextDeployNo(t *testing.T) {
	db := newTestDB(t)
	s := &CDService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	// 日期用 now() 取、与 nextDeployNo 内部同一口径 —— 写死日期会在跨天时莫名其妙地红。
	prefix := "DP" + now().Format("20060102")

	t.Run("空库从 0001 起", func(t *testing.T) {
		got, err := s.nextDeployNo(ctx)
		if err != nil {
			t.Fatalf("生成单号失败：%v", err)
		}
		if want := prefix + "0001"; got != want {
			t.Errorf("空库应给出 %s，实际 %s", want, got)
		}
	})

	t.Run("接着已有的最大号往下排", func(t *testing.T) {
		if err := db.Create(&model.DeployRecord{DeployNo: prefix + "0007"}).Error; err != nil {
			t.Fatalf("预置部署单失败：%v", err)
		}
		got, err := s.nextDeployNo(ctx)
		if err != nil {
			t.Fatalf("生成单号失败：%v", err)
		}
		if want := prefix + "0008"; got != want {
			t.Errorf("已有 0007 时应给出 %s，实际 %s", want, got)
		}
	})

	t.Run("昨天的号不影响今天的", func(t *testing.T) {
		if err := db.Create(&model.DeployRecord{DeployNo: "DP202001019999"}).Error; err != nil {
			t.Fatalf("预置历史部署单失败：%v", err)
		}
		got, err := s.nextDeployNo(ctx)
		if err != nil {
			t.Fatalf("生成单号失败：%v", err)
		}
		// 序号按**天**计：今天只有 0007，所以是 0008。
		// 注意这里仍然是 0008、而不是 0009 —— 上一个子测试算出的 0008 **没有落库**
		// （这正是 nextDeployNo 的契约：它只查不算占位，防撞靠调用方的重试）。
		if want := prefix + "0008"; got != want {
			t.Errorf("跨天不该沿用历史序号，期望 %s，实际 %s", want, got)
		}
		if len(got) != len(prefix)+4 {
			t.Errorf("序号应是 4 位定长，实际 %s（长度 %d）", got, len(got))
		}
	})
}

// 撞号与"幂等键撞了"必须分得开：前者要**重算重试**，后者要**返回已有那一单**。
// 判据是错误文本里的索引名，所以两种索引的报错都钉住。
func TestIsDupKeyOn(t *testing.T) {
	dupNo := errors.New("Error 1062 (23000): Duplicate entry 'DP202610160039' for key 'deploy_record.uk_deploy_no'")
	dupIdem := errors.New("Error 1062 (23000): Duplicate entry 'DP261016-395993:pre-aliyun:aio-mcp-server' for key 'deploy_record.uk_idempotency_key'")

	if !isDupKeyOn(dupNo, "uk_deploy_no") {
		t.Error("单号冲突应被认出来")
	}
	if isDupKeyOn(dupNo, "uk_idempotency_key") {
		t.Error("单号冲突不该被当成幂等键冲突 —— 两者的处置完全不同")
	}
	if !isDupKeyOn(dupIdem, "uk_idempotency_key") {
		t.Error("幂等键冲突应被认出来")
	}
	if isDupKeyOn(dupIdem, "uk_deploy_no") {
		t.Error("幂等键冲突不该被当成单号冲突 —— 那会去重算单号，而问题根本不在单号")
	}
	if isDupKeyOn(nil, "uk_deploy_no") {
		t.Error("nil 不该算冲突")
	}
	if isDupKeyOn(errors.New("dial tcp: connection refused"), "uk_deploy_no") {
		t.Error("连接错误不该算冲突 —— 那会触发 5 次无意义的重试，把一次网络抖动放大成 5 倍延迟")
	}
}
