package service

import (
	"encoding/json"
	"testing"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/model"
	"github.com/2777484478/yunnian/internal/store"
)

// 种子数据的产品约束：**能配置 == 能收到**。
//
// 为什么值得钉一条测试：种子是**全新部署**时用户看到的第一屏数据（`Seed` 以
// user 表为空为判据，只在空库跑）。2026-10-10 之前它种了四条渠道，其中
// `wecom` / `webhook` 两种**发送器根本不存在** —— `TestNotifyChannel` 会回
// "尚未接入发送器"、编辑会被 `NotifyInput.Type` 的 `oneof=email dingtalk` 拒掉。
// 其中 webhook 那条还 `enabled=true` 且订阅 `build_failed`：每次构建失败都会去投递、
// 每次必然失败、把 `fail_count` 一路加上去。
//
// 事件码是同一个毛病：原种子里的 `env_unhealthy` / `deploy_success` /
// `deploy_approval` / `rollback_executed` **没有任何代码会触发**。
//
// 这类缺陷在界面上**看起来完全正常**（渠道在列表里、事件是勾上的、没有报错），
// 只能靠断言拦。删掉那两条渠道只是删了当下库里的行，**换个空库种子就会再造出来**。
func TestSeedNotifyChannelsAreDeliverable(t *testing.T) {
	db := newTestDB(t)

	// 与真实启动同一个顺序：先 Seed（角色/权限/管理员），再 SeedDemo（演示数据）。
	// 单跑 SeedDemo 会报「用户 ops 引用了未定义的角色 ops」—— 角色是 Seed 建的。
	if err := store.Seed(db, config.Default()); err != nil {
		t.Fatalf("执行种子失败: %v", err)
	}
	if err := store.SeedDemo(db, config.Default()); err != nil {
		t.Fatalf("写入演示数据失败: %v", err)
	}

	var rows []model.NotifyChannel
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("读取通知渠道失败: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("种子没有写入任何通知渠道：全新部署的通知页会是空的")
	}

	// 有发送器的类型只有这两种（见 notify.go 的 newNotifyService 注册表）。
	deliverable := map[string]bool{"email": true, "dingtalk": true}

	// 与 SupportedNotifyEvents 同源，避免两处清单各自漂移。
	supported := make(map[string]bool, len(SupportedNotifyEvents))
	for _, e := range SupportedNotifyEvents {
		supported[string(e)] = true
	}

	for _, c := range rows {
		if !deliverable[c.Type] {
			t.Errorf("种子渠道 %q 的类型 %q 没有发送器：用户配得出来、却永远发不出去，"+
				"点「测试」必然报错。要么补上发送器，要么别种这条。", c.Name, c.Type)
		}

		var events []string
		if err := json.Unmarshal([]byte(c.Events), &events); err != nil {
			t.Errorf("种子渠道 %q 的 events 不是合法 JSON: %v（原值 %s）", c.Name, err, c.Events)
			continue
		}
		for _, e := range events {
			if !supported[e] {
				t.Errorf("种子渠道 %q 订阅了事件 %q，但没有任何代码会触发它："+
					"用户勾上、存库、然后永远收不到。", c.Name, e)
			}
		}
	}
}
