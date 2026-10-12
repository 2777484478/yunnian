package service

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"github.com/yunnian/yunnian/internal/errs"
	"github.com/yunnian/yunnian/internal/httpx"
	"github.com/yunnian/yunnian/internal/model"
)

// TestResolveRollbackTarget_RejectsImageOfAnotherProject 回滚目标必须属于本服务所在的项目。
//
// 为什么这条要单独钉住：`toImage` 是**全仓库唯一的镜像字符串，它自己不携带项目信息**。
// 前端曾因"切换项目后回滚弹窗的预选值串台"（`useApi` 换依赖不清旧 data + 预选 effect
// 拿旧值先跑一次）把**上一个项目的镜像**原样发过来 —— 而 `ToImage` 分支**只按
// `image = ?` 查表、没有项目校验**（只有 `ToImageID` 分支判了 `ProjectKey`）。
// 两者口径不一致时，这个请求会把别的项目的镜像真回滚进本服务。前端那个洞已堵，
// 但服务端这层校验才是"这个目标能不能用"的最终判据，必须自己站得住。
func TestResolveRollbackTarget_RejectsImageOfAnotherProject(t *testing.T) {
	db := newTestDB(t)
	s := &CDService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	mine := model.ImageRecord{
		ProjectKey: "demouser", Registry: "harbor.test", Repo: "escloud/demouser",
		Tag: "1.2.0.1471", Image: "harbor.test/escloud/demouser:1.2.0.1471",
	}
	other := model.ImageRecord{
		ProjectKey: "demobasic", Registry: "harbor.test", Repo: "escloud/demobasic",
		Tag: "1.2.0.2761", Image: "harbor.test/escloud/demobasic:1.2.0.2761",
	}
	for _, r := range []*model.ImageRecord{&mine, &other} {
		if err := db.Create(r).Error; err != nil {
			t.Fatalf("造镜像记录失败: %v", err)
		}
	}
	svc := &model.ServiceInstance{EnvKey: "test", ProjectKey: "demouser", CurrentImage: mine.Image}

	// ① 传别的项目的镜像 → 必须拒（这就是串台时前端会发出来的报文）
	_, err := s.resolveRollbackTarget(ctx, svc, CreateRollbackInput{ToImage: other.Image})
	if err == nil {
		t.Fatal("别的项目的镜像被放行了 —— 会把不属于本项目的版本回滚进本服务")
	}
	if code := errs.From(err).Code; code != errs.CodeInvalidParam {
		t.Fatalf("拒绝理由应为参数错误(%d)，得到 %d（%v）", errs.CodeInvalidParam, code, err)
	}

	// ② ToImageID 分支同一口径。这条修复前就有，钉住免得以后被"顺手简化"掉。
	_, err = s.resolveRollbackTarget(ctx, svc, CreateRollbackInput{ToImageID: httpx.IDString(other.ID)})
	if err == nil {
		t.Fatal("ToImageID 分支放行了别的项目的镜像")
	}
	if code := errs.From(err).Code; code != errs.CodeInvalidParam {
		t.Fatalf("ToImageID 分支拒绝理由应为参数错误(%d)，得到 %d（%v）", errs.CodeInvalidParam, code, err)
	}

	// ③ 本项目自己的镜像必须**能选中**。少了这条，前面两条"全是拒"是可以通过
	//    把整个分支写成 return error 来满足的 —— 那种"修复"会让回滚彻底不可用。
	got, err := s.resolveRollbackTarget(ctx, svc, CreateRollbackInput{ToImage: mine.Image})
	if err != nil {
		t.Fatalf("本项目的镜像被误拒: %v", err)
	}
	if got.ID != mine.ID {
		t.Fatalf("解析到的镜像不是目标那条: got id=%d want %d", got.ID, mine.ID)
	}

	// ④ 镜像记录根本不存在 → 404。与 ① 的 40000 分开断言：
	//    "查不到这个镜像" 和 "这个镜像不是你的" 处理动作不同（前者要先去构建）。
	_, err = s.resolveRollbackTarget(ctx, svc, CreateRollbackInput{ToImage: "harbor.test/escloud/nope:1.0"})
	if err == nil {
		t.Fatal("不存在的镜像被放行")
	}
	if code := errs.From(err).Code; code != errs.CodeNotFound {
		t.Fatalf("不存在的镜像应为 404(%d)，得到 %d（%v）", errs.CodeNotFound, code, err)
	}
}

// TestReviveProject_ReusesSoftDeletedRow 删掉的项目要用同一个标识重建得回来。
//
// `uk_project_key` 只含 `project_key`、**不含 `deleted_at`**，所以软删行的标识仍被占着：
// 修复前 `Create` 撞唯一索引后直接翻译成 `40900 项目标识已被占用`，
// 用户看到的却是"列表里根本没有这个项目"（2026-09-28 实测撞过两次，
// 当时的解法是手工物理删库）。
//
// 这里直接测 `reviveProject` 而不跑完整的 `CreateProject`：后者的前置校验
// （负责人解析、Git 地址、仓库默认值、审计）与本条无关，掺进来只会让失败原因模糊。
func TestReviveProject_ReusesSoftDeletedRow(t *testing.T) {
	db := newTestDB(t)
	s := &CIService{base: &base{deps: Deps{DB: db}}}

	old := model.Project{
		ProjectKey: "revive-probe", Name: "重建前的名字", Type: "backend",
		GitProvider: "gitlab", DefaultBranch: "master", BuildMethod: "dockerfile",
		Enabled: true,
	}
	if err := db.Create(&old).Error; err != nil {
		t.Fatalf("造项目失败: %v", err)
	}
	if err := db.Delete(&model.Project{}, old.ID).Error; err != nil {
		t.Fatalf("软删项目失败: %v", err)
	}

	// 软删后普通查询看不到它 —— 这正是用户不解的地方（"列表里没有啊"）。
	var visible int64
	if err := db.Model(&model.Project{}).Where("project_key = ?", "revive-probe").Count(&visible).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if visible != 0 {
		t.Fatalf("软删后不该出现在普通查询里，得到 %d 条", visible)
	}

	p := model.Project{
		ProjectKey: "revive-probe", Name: "重建后的名字", Type: "backend",
		GitProvider: "gitlab", DefaultBranch: "master", BuildMethod: "dockerfile",
		Enabled: true, Team: "基础架构组",
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return s.reviveProject(tx, &p) }); err != nil {
		t.Fatalf("复活软删行失败（用户会看到「标识已被占用」）: %v", err)
	}
	if p.ID != old.ID {
		t.Fatalf("应复用原行 id=%d，得到 %d（重建出了一条新行，历史就断了）", old.ID, p.ID)
	}

	var got model.Project
	if err := db.First(&got, old.ID).Error; err != nil {
		t.Fatalf("复活后查不到该行: %v", err)
	}
	if got.DeletedAt.Valid {
		t.Fatal("复活后 deleted_at 仍非空 —— 列表里还是看不到")
	}
	if got.Name != "重建后的名字" || got.Team != "基础架构组" {
		t.Fatalf("复活时应写入本次提交的新值，得到 name=%q team=%q", got.Name, got.Team)
	}
}

// TestReviveProject_RealConflictStaysConflict 真并发占用时**不能**伪装成"已复活"。
//
// 没有软删行可捞时（别人刚用同一标识建好），必须如实报冲突；
// 否则会返回一个"成功"、但用户拿到的其实不是自己建的项目。
func TestReviveProject_RealConflictStaysConflict(t *testing.T) {
	db := newTestDB(t)
	s := &CIService{base: &base{deps: Deps{DB: db}}}

	// 库里没有任何该标识的行（连带软删的也没有）
	p := model.Project{ProjectKey: "no-such-key", Name: "x"}
	err := db.Transaction(func(tx *gorm.DB) error { return s.reviveProject(tx, &p) })
	if err == nil {
		t.Fatal("没有可复活的软删行时不该返回成功")
	}
	if code := errs.From(err).Code; code != errs.CodeConflict {
		t.Fatalf("应为冲突(%d)，得到 %d（%v）", errs.CodeConflict, code, err)
	}
}

// TestRevive_ActiveRowIsConflictNotOverwrite 同标识的**活跃**行必须报冲突，不能被"复活"掉。
//
// 这条是端到端验证抓出来的真缺陷：`reviveProject` / `reviveEnvironment` 原先只用
// `Unscoped().Where(key).First()` 找"可复活的软删行"，而 **`Unscoped` 会把活跃行一起捞出来**。
// 于是拿一个已被占用的标识创建时，代码不会报 40900，而是把那个**正在使用的**项目
// 原地改写成新提交的配置，还回一句"创建成功"。用户以为"我新建了一个"，
// 实际是别人的项目被悄悄换了名字、仓库、构建方式 —— 比直接报冲突危险得多。
//
// 两处同源（`environment` 与 `project` 是同一套写法），所以一起钉。
func TestRevive_ActiveRowIsConflictNotOverwrite(t *testing.T) {
	db := newTestDB(t)
	cd := &CDService{base: &base{deps: Deps{DB: db}}}
	ci := &CIService{base: &base{deps: Deps{DB: db}}}

	// ---- 项目 ----
	liveProj := model.Project{
		ProjectKey: "busy", Name: "正在用的项目", Type: "backend", GitProvider: "gitlab",
		DefaultBranch: "master", BuildMethod: "dockerfile", Enabled: true,
	}
	if err := db.Create(&liveProj).Error; err != nil {
		t.Fatalf("造活跃项目失败: %v", err)
	}
	attempt := model.Project{ProjectKey: "busy", Name: "我的新项目", Type: "frontend"}
	err := db.Transaction(func(tx *gorm.DB) error { return ci.reviveProject(tx, &attempt) })
	if err == nil {
		t.Fatal("活跃的同名项目被当成软删行『复活』了 —— 正在用的项目会被原地改写")
	}
	if code := errs.From(err).Code; code != errs.CodeConflict {
		t.Fatalf("活跃占用应为冲突(%d)，得到 %d（%v）", errs.CodeConflict, code, err)
	}
	var gotProj model.Project
	if err := db.First(&gotProj, liveProj.ID).Error; err != nil {
		t.Fatalf("活跃行查不到了: %v", err)
	}
	if gotProj.Name != "正在用的项目" || gotProj.Type != "backend" {
		t.Fatalf("活跃行被改写了: name=%q type=%q", gotProj.Name, gotProj.Type)
	}

	// ---- 环境（同一处写法）----
	liveEnv := model.Environment{EnvKey: "busy-env", Name: "正在用的环境", HealthStatus: "unknown", Enabled: true}
	if err := db.Create(&liveEnv).Error; err != nil {
		t.Fatalf("造活跃环境失败: %v", err)
	}
	envAttempt := model.Environment{EnvKey: "busy-env", Name: "我的新环境"}
	err = db.Transaction(func(tx *gorm.DB) error { return cd.reviveEnvironment(tx, &envAttempt) })
	if err == nil {
		t.Fatal("活跃的同名环境被当成软删行『复活』了")
	}
	if code := errs.From(err).Code; code != errs.CodeConflict {
		t.Fatalf("活跃占用应为冲突(%d)，得到 %d（%v）", errs.CodeConflict, code, err)
	}
	var gotEnv model.Environment
	if err := db.First(&gotEnv, liveEnv.ID).Error; err != nil {
		t.Fatalf("活跃环境查不到了: %v", err)
	}
	if gotEnv.Name != "正在用的环境" {
		t.Fatalf("活跃环境被改写了: name=%q", gotEnv.Name)
	}
}
