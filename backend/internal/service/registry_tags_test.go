package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/logstream"
	"github.com/2777484478/yunnian/internal/model"
)

// 本文件盯的是「仓库标签」这条新链路里最容易**静默出错**的两处：
//
//	① 项目 → 仓库路径的映射：映射错了会去读**别人的仓库**，然后一路绿灯地发出去；
//	② 「平台构建 / 仓库标签」二选一：两个都给、或一个都不给，都必须被拒。
//
// 这两件事真仓库给不出确定性复现（它只反映此刻事实），所以用 sqlite 测试库 + 桩 Harbor。

// --------------------------------------------------------------------------- 桩 Harbor（v1 形状）

// newStubHarborV1 起一个按 Harbor v1.1.2 形状应答的桩：
//
//	/api/v2.0/systeminfo                 → 404（v1 没有 v2 接口）
//	/api/systeminfo                      → 200
//	/api/search?q=<keyword>              → 按关键字给出仓库
//	/api/repositories/{repo}/tags?detail=1 → 全部 tag + manifest（无推送时间）
func newStubHarborV1(t *testing.T, repos map[string][]string, tags map[string][]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2.0/systeminfo", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/systeminfo", func(w http.ResponseWriter, r *http.Request) {
		writeJSONStub(t, w, map[string]any{"harbor_version": "v1.1.2", "auth_mode": "db_auth"})
	})
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		kw := r.URL.Query().Get("q")
		list := []any{}
		for _, full := range repos[kw] {
			list = append(list, map[string]any{"repository_name": full, "tags_count": len(tags[full])})
		}
		writeJSONStub(t, w, map[string]any{"project": []any{}, "repository": list})
	})
	mux.HandleFunc("/api/repositories/", func(w http.ResponseWriter, r *http.Request) {
		// 路径形如 /api/repositories/escloud/aioai/tags
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/repositories/"), "/tags")
		list, ok := tags[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		out := make([]any, 0, len(list))
		for _, tg := range list {
			out = append(out, stubV1TagEntry(tg))
		}
		writeJSONStub(t, w, out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// stubV1TagEntry 造一条 tags?detail=1 的记录。history **第一个**才是这版镜像的构建时间
// （实测确认：`history[0]` 随 tag 变化，`history[-1]` 恒为基础镜像层）。
//
// 时间与 tag 末段数字**同向**（基准时间 + n 分钟），这样"按时间倒序"就能被断言，
// 也不会出现两个 tag 撞上同一个时间（撞上就分不清是排序错了还是取错层了）。
func stubV1TagEntry(tag string) map[string]any {
	n := 1
	if i := strings.LastIndex(tag, "."); i >= 0 {
		if v, err := parseTrailingInt(tag[i+1:]); err == nil {
			n = v
		}
	}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(n) * time.Minute).Format(time.RFC3339)
	top, _ := json.Marshal(map[string]string{"created": created})
	base, _ := json.Marshal(map[string]string{"created": "2020-01-01T00:00:00Z"})
	return map[string]any{
		"tag": tag,
		"manifest": map[string]any{
			"schemaVersion": 1,
			"history": []any{
				map[string]any{"v1Compatibility": string(top)},
				map[string]any{"v1Compatibility": string(base)},
			},
		},
	}
}

func writeJSONStub(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("桩写响应失败：%v", err)
	}
}

// --------------------------------------------------------------------------- 装配

// newCDWithHarbor 装配一个"接了桩 Harbor"的 CDService。
//
// 注意 Up.Registry 用的是**按同一份配置新建**的客户端，与 registryClient 的取值一致 ——
// 这样测试里不会出现"预检看另一个仓库、部署看这个仓库"的分叉。
func newCDWithHarbor(t *testing.T, db *gorm.DB, baseURL string) *CDService {
	t.Helper()
	cfg := &config.Config{}
	cfg.Integration.Harbor = config.HarborConfig{
		Enabled:    true,
		BaseURL:    baseURL,
		CoreAPIURL: baseURL,
		Registry:   "srv.test:5000",
		Timeout:    config.Duration(5 * time.Second),
	}
	reg := integration.NewHarborRegistry(cfg.Integration.Harbor)
	return New(Deps{
		Cfg:    cfg,
		DB:     db,
		Log:    discardLogger(),
		Stream: logstream.NewMemStream(),
		Up:     &integration.Set{Cluster: &stubCluster{}, Registry: reg},
	}).CD
}

func seedProject(t *testing.T, db *gorm.DB, key string) *model.Project {
	t.Helper()
	p := &model.Project{ProjectKey: key, Name: key, Enabled: true}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("建项目失败: %v", err)
	}
	return p
}

func seedImageRecord(t *testing.T, db *gorm.DB, proj *model.Project, registry, repo, tag string) {
	t.Helper()
	rec := &model.ImageRecord{
		ProjectID: proj.ID, ProjectKey: proj.ProjectKey,
		Registry: registry, Repo: repo, Tag: tag,
		Image: registry + "/" + repo + ":" + tag, Digest: "sha256:fromrecord",
		PushedAt: time.Now(),
	}
	if err := db.Create(rec).Error; err != nil {
		t.Fatalf("建镜像记录失败: %v", err)
	}
}

// --------------------------------------------------------------------------- 二选一

func TestCheckImageSource(t *testing.T) {
	cases := []struct {
		name    string
		in      CreateDeploymentInput
		wantErr string // 期望错误里出现的关键片段，空 = 应通过
	}{
		{"平台构建（镜像记录）", CreateDeploymentInput{ImageRecordID: "7"}, ""},
		{"平台构建（tag）", CreateDeploymentInput{Tag: "1.2.0.1"}, ""},
		{"仓库标签", CreateDeploymentInput{ImageSource: "registry", RegistryTag: "latest"}, ""},
		{"两个都给 → 拒", CreateDeploymentInput{ImageRecordID: "7", RegistryTag: "latest"}, "只能选一个"},
		{"两个都不给 → 拒", CreateDeploymentInput{}, "请选择要部署的镜像"},
		{"说好是仓库却没给标签 → 拒", CreateDeploymentInput{ImageSource: "registry"}, "没有给出标签名"},
		{"说好是平台构建却给了仓库标签 → 拒", CreateDeploymentInput{ImageSource: "platform", RegistryTag: "latest"}, "提交的是仓库标签"},
		// 平台内部复用（重新部署 / 重试）不受约束：它本来就是"照上一单原样再来一次"。
		{"内部复用（镜像串）", CreateDeploymentInput{ImageOverride: "h/r:1"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkImageSource(c.in)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("应当通过，实际：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应当被拒绝，实际通过了")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("错误文案应含 %q，实际：%v", c.wantErr, err)
			}
		})
	}
}

// --------------------------------------------------------------------------- 项目 → 仓库映射

func TestHarborRepoForProject(t *testing.T) {
	// 桩仓库：escloud/aioai 与 escloud/aioai-2（后者会造出"末段匹配"的歧义场景）
	srv := newStubHarborV1(t,
		map[string][]string{
			"aioai":  {"escloud/aioai"},
			"dup":    {"escloud/dup", "other/dup"},
			"norows": {},
		},
		map[string][]string{"escloud/aioai": {"1.2.0.3401"}},
	)
	core := integration.NewHarborCore(config.HarborConfig{BaseURL: srv.URL, CoreAPIURL: srv.URL})

	t.Run("有平台构建记录 → 用记录里的路径，不检索", func(t *testing.T) {
		db := newTestDB(t)
		cd := newCDWithHarbor(t, db, srv.URL)
		proj := seedProject(t, db, "aioai")
		seedImageRecord(t, db, proj, "srv.test:5000", "escloud/aioai", "1.2.0.3401")

		repo, source, reason, err := cd.harborRepoForProject(context.Background(), proj, core, "srv.test:5000")
		if err != nil {
			t.Fatalf("意外错误：%v", err)
		}
		if repo != "escloud/aioai" || source != "image_record" || reason != "" {
			t.Fatalf("应走 image_record 且路径为 escloud/aioai，实际 repo=%q source=%q reason=%q", repo, source, reason)
		}
	})

	t.Run("记录的仓库不是当前配置那个 → 不信它，改检索", func(t *testing.T) {
		db := newTestDB(t)
		cd := newCDWithHarbor(t, db, srv.URL)
		proj := seedProject(t, db, "aioai")
		// 换了 Harbor 之后的旧记录：路径在**别的**仓库里可能指向同名的别人家的仓库
		seedImageRecord(t, db, proj, "old-repo.test:5000", "escloud/aioai", "1.2.0.3401")

		repo, source, _, err := cd.harborRepoForProject(context.Background(), proj, core, "srv.test:5000")
		if err != nil {
			t.Fatalf("意外错误：%v", err)
		}
		if repo != "escloud/aioai" || source != "search" {
			t.Fatalf("应退回检索，实际 repo=%q source=%q", repo, source)
		}
	})

	t.Run("没有记录 → 检索到唯一匹配就用它", func(t *testing.T) {
		db := newTestDB(t)
		cd := newCDWithHarbor(t, db, srv.URL)
		proj := seedProject(t, db, "aioai")

		repo, source, reason, err := cd.harborRepoForProject(context.Background(), proj, core, "srv.test:5000")
		if err != nil {
			t.Fatalf("意外错误：%v", err)
		}
		if repo != "escloud/aioai" || source != "search" {
			t.Fatalf("实际 repo=%q source=%q reason=%q", repo, source, reason)
		}
	})

	t.Run("检索到多个同名末段 → 拒绝且说清有几个", func(t *testing.T) {
		db := newTestDB(t)
		cd := newCDWithHarbor(t, db, srv.URL)
		proj := seedProject(t, db, "dup")

		repo, _, reason, err := cd.harborRepoForProject(context.Background(), proj, core, "srv.test:5000")
		if err != nil {
			t.Fatalf("意外错误：%v", err)
		}
		if repo != "" {
			t.Fatalf("有歧义时不能猜，实际给出了 %q", repo)
		}
		// 把候选列出来，用户才知道该去改哪一个
		if !strings.Contains(reason, "escloud/dup") || !strings.Contains(reason, "other/dup") {
			t.Errorf("原因里应列出候选，实际：%s", reason)
		}
	})

	t.Run("检索不到 → 拒绝并指向可操作的动作", func(t *testing.T) {
		db := newTestDB(t)
		cd := newCDWithHarbor(t, db, srv.URL)
		proj := seedProject(t, db, "norows")

		repo, _, reason, err := cd.harborRepoForProject(context.Background(), proj, core, "srv.test:5000")
		if err != nil {
			t.Fatalf("意外错误：%v", err)
		}
		if repo != "" || !strings.Contains(reason, "先在 CI 里构建一次") {
			t.Fatalf("实际 repo=%q reason=%q", repo, reason)
		}
	})
}

// --------------------------------------------------------------------------- tag 列表

func TestListRegistryTags(t *testing.T) {
	tags := []string{"1.2.0.3401", "1.2.0.3405", "1.2.0.3409", "1.2.0.3413", "1.2.0.3417", "1.2.0.3421", "1.2.0.3425"}
	srv := newStubHarborV1(t,
		map[string][]string{"aioai": {"escloud/aioai"}, "norows": {}},
		map[string][]string{"escloud/aioai": tags},
	)

	t.Run("默认只回最近 5 个，total 是全量", func(t *testing.T) {
		db := newTestDB(t)
		cd := newCDWithHarbor(t, db, srv.URL)
		seedProject(t, db, "aioai")

		dto, err := cd.ListRegistryTags(context.Background(), "aioai", 0)
		if err != nil {
			t.Fatalf("意外错误：%v", err)
		}
		if dto.Unavailable != "" {
			t.Fatalf("不该不可用：%s", dto.Unavailable)
		}
		if len(dto.Tags) != 5 {
			t.Fatalf("limit 缺省应为 5，实际 %d 条", len(dto.Tags))
		}
		if dto.Total != len(tags) {
			t.Errorf("total 应是仓库全量 %d，实际 %d", len(tags), dto.Total)
		}
		// 时间来源必须是 build：v1 给不出推送时间，界面要照着措辞
		if dto.TimeKind != integration.HarborTimeBuild {
			t.Errorf("v1 的时间来源应为 build，实际 %q", dto.TimeKind)
		}
		if !strings.Contains(dto.Note, "推送时间") {
			t.Errorf("说明里要点明没有推送时间，实际：%s", dto.Note)
		}
		if dto.ImageBase != "srv.test:5000/escloud/aioai" {
			t.Errorf("imageBase 不对：%s", dto.ImageBase)
		}
		// 倒序：3401 的"第 1 天"最小 → 排最后；3425 → %28=25 → 最大 → 排最前
		if dto.Tags[0].Tag != "1.2.0.3425" {
			t.Errorf("应按构建时间倒序，首条应为 1.2.0.3425，实际 %s", dto.Tags[0].Tag)
		}
		// 每个 tag 的时间必须各不相同 —— 全都一样说明取错了 manifest 的层
		seen := map[string]bool{}
		for _, x := range dto.Tags {
			if x.Time == "" {
				t.Errorf("tag %s 没拿到时间", x.Tag)
			}
			if seen[x.Time] {
				t.Errorf("多个 tag 给出同一个时间 %s：十有八九取错层了", x.Time)
			}
			seen[x.Time] = true
		}
	})

	t.Run("定位不到仓库 → unavailable 讲原因，而不是给空列表", func(t *testing.T) {
		db := newTestDB(t)
		cd := newCDWithHarbor(t, db, srv.URL)
		seedProject(t, db, "norows")

		dto, err := cd.ListRegistryTags(context.Background(), "norows", 5)
		if err != nil {
			t.Fatalf("意外错误：%v", err)
		}
		if dto.Unavailable == "" {
			t.Fatal("应给出不可用原因")
		}
		if len(dto.Tags) != 0 {
			t.Errorf("不可用时不该编出 tag：%+v", dto.Tags)
		}
	})

	t.Run("Harbor 未启用 → unavailable 指向配置入口", func(t *testing.T) {
		db := newTestDB(t)
		cd := newCDWithHarbor(t, db, srv.URL)
		seedProject(t, db, "aioai")
		cd.deps.Cfg.Integration.Harbor.Enabled = false

		dto, err := cd.ListRegistryTags(context.Background(), "aioai", 5)
		if err != nil {
			t.Fatalf("意外错误：%v", err)
		}
		if !strings.Contains(dto.Unavailable, "第三方集成") {
			t.Errorf("应指向配置入口，实际：%s", dto.Unavailable)
		}
	})

	t.Run("项目不存在 → 报错而不是空列表", func(t *testing.T) {
		db := newTestDB(t)
		cd := newCDWithHarbor(t, db, srv.URL)
		if _, err := cd.ListRegistryTags(context.Background(), "bu-cun-zai", 5); err == nil {
			t.Fatal("不存在的项目应报错")
		}
	})
}

// parseTrailingInt 把 "3401" 这类尾段转成整数（写在测试里，避免依赖被测代码）。
func parseTrailingInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errNotDigits
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

var errNotDigits = errNotDigitsErr{}

type errNotDigitsErr struct{}

func (errNotDigitsErr) Error() string { return "不是纯数字" }
