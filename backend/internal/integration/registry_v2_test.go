package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/2777484478/yunnian/internal/config"
)

// 本文件的桩注册表按 Docker Registry v2 的真实交互来应答：
//   GET /v2/                                → 401 + WWW-Authenticate
//   GET <realm>?service=&scope=             → {"token":"..."}
//   GET /v2/{name}/tags/list                → {"name":..,"tags":[..]}
//   GET /v2/{name}/manifests/{ref}          → manifest + Docker-Content-Digest
//   GET /v2/{name}/blobs/{digest}           → config blob（架构在里头）
//
// 断言的都是"从仓库读到的事实"，不涉及项目配置 —— 这条链路的价值就在于
// 只写确定读到的东西。

const (
	testDigestSingle = "sha256:aaaa000000000000000000000000000000000000000000000000000000000001"
	testDigestMulti  = "sha256:bbbb000000000000000000000000000000000000000000000000000000000002"
	testDigestAMD64  = "sha256:cccc000000000000000000000000000000000000000000000000000000000003"
	testDigestARM64  = "sha256:dddd000000000000000000000000000000000000000000000000000000000004"
	testDigestConfig = "sha256:eeee000000000000000000000000000000000000000000000000000000000005"
)

// newStubRegistry 起一个假的注册表，返回其地址与收到的请求记录。
func newStubRegistry(t *testing.T, single bool) (*httptest.Server, *[]string, *[]string) {
	t.Helper()
	var paths []string
	var auths []string

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		// 认证挑战：Harbor 的形态，realm 指向它自己的 token 服务
		w.Header().Set("Www-Authenticate",
			fmt.Sprintf(`Bearer realm="http://%s/service/token",service="harbor-registry"`, r.Host))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/service/token", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		if got := r.URL.Query().Get("service"); got != "harbor-registry" {
			t.Errorf("service 参数应为 harbor-registry，实际 %q", got)
		}
		if got := r.URL.Query().Get("scope"); got != "repository:escloud/demowarning:pull" {
			t.Errorf("scope 参数不对：%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
	})
	mux.HandleFunc("/v2/escloud/demowarning/tags/list", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("列 tag 应带 bearer token，实际 %q", got)
		}
		// 故意乱序 —— 真实 Harbor 的 tags/list 就不排序，别让测试假装它有顺序
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "escloud/demowarning",
			"tags": []string{"1.2.0.1044", "1.2.0.1058", "1.2.0.1049"},
		})
	})

	writeManifest := func(w http.ResponseWriter, digest string, body map[string]any) {
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		_ = json.NewEncoder(w).Encode(body)
	}

	if single {
		mux.HandleFunc("/v2/escloud/demowarning/manifests/1.2.0.1058", func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			// 单架构清单也必须带上列表类 Accept，否则多架构镜像会静默算成 0
			if acc := r.Header.Get("Accept"); !strings.Contains(acc, "manifest.list.v2+json") {
				t.Errorf("Accept 头缺少 manifest list 类型：%q", acc)
			}
			writeManifest(w, testDigestSingle, map[string]any{
				"schemaVersion": 2,
				"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
				"config":        map[string]any{"digest": testDigestConfig, "size": 8512},
				"layers": []map[string]any{
					{"size": 1000}, {"size": 2000}, {"size": 3000},
				},
			})
		})
		mux.HandleFunc("/v2/escloud/demowarning/blobs/"+testDigestConfig, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"os": "linux", "architecture": "amd64",
			})
		})
	} else {
		mux.HandleFunc("/v2/escloud/demowarning/manifests/1.2.0.1058", func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			writeManifest(w, testDigestMulti, map[string]any{
				"schemaVersion": 2,
				"mediaType":     "application/vnd.docker.distribution.manifest.list.v2+json",
				"manifests": []map[string]any{
					{"digest": testDigestAMD64, "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
					{"digest": testDigestARM64, "platform": map[string]string{"os": "linux", "architecture": "arm64"}},
				},
			})
		})
		mux.HandleFunc("/v2/escloud/demowarning/manifests/"+testDigestAMD64, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			writeManifest(w, testDigestAMD64, map[string]any{
				"layers": []map[string]any{{"size": 296 << 20}},
			})
		})
		mux.HandleFunc("/v2/escloud/demowarning/manifests/"+testDigestARM64, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			writeManifest(w, testDigestARM64, map[string]any{
				"layers": []map[string]any{{"size": 280 << 20}},
			})
		})
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &paths, &auths
}

func newClientFor(srv *httptest.Server, user, pass string) *RegistryV2 {
	return NewHarborRegistry(config.HarborConfig{
		Enabled:  true,
		BaseURL:  srv.URL,
		Registry: strings.TrimPrefix(srv.URL, "http://"),
		Username: user,
		Password: pass,
		Timeout:  config.Duration(5 * time.Second),
	})
}

func TestResolveSingleArch(t *testing.T) {
	srv, paths, auths := newStubRegistry(t, true)
	r := newClientFor(srv, "robot$yunnian", "s3cret")

	got, err := r.Resolve(context.Background(), "escloud/demowarning", "1.2.0.1058")
	if err != nil {
		t.Fatalf("Resolve 报错：%v", err)
	}
	if got == nil {
		t.Fatal("Resolve 返回 nil，应为单架构元数据")
	}
	if got.Digest != testDigestSingle {
		t.Errorf("digest = %q，应为响应头里的 %q", got.Digest, testDigestSingle)
	}
	if got.SizeBytes != 6000 {
		t.Errorf("大小 = %d，应为各层之和 6000", got.SizeBytes)
	}
	if got.Arch != "linux/amd64" {
		t.Errorf("架构 = %q，应从 config blob 读出 linux/amd64", got.Arch)
	}
	if !got.PushedAt.IsZero() {
		t.Errorf("PushedAt 应为零值（registry v2 给不出推送时间），实际 %v", got.PushedAt)
	}
	if len(*auths) != 1 || !strings.HasPrefix((*auths)[0], "Basic ") {
		t.Errorf("配置了账号时应带 Basic 认证，实际 %v", *auths)
	}
	if len(*paths) < 4 {
		t.Errorf("请求次数偏少，可能有步骤被跳过：%v", *paths)
	}
}

func TestResolveMultiArchSumsPlatforms(t *testing.T) {
	srv, paths, _ := newStubRegistry(t, false)
	r := newClientFor(srv, "", "")

	got, err := r.Resolve(context.Background(), "escloud/demowarning", "1.2.0.1058")
	if err != nil {
		t.Fatalf("Resolve 报错：%v", err)
	}
	if got.Digest != testDigestMulti {
		t.Errorf("digest = %q，应为清单列表本身的 digest", got.Digest)
	}
	want := uint64((296 + 280) << 20)
	if got.SizeBytes != want {
		t.Errorf("大小 = %d，应为两个平台之和 %d", got.SizeBytes, want)
	}
	if got.Arch != "linux/amd64,linux/arm64" {
		t.Errorf("架构 = %q，应为逗号分隔的平台列表", got.Arch)
	}
	// 清单列表本身不能当镜像层，必须真的去拉两个子清单
	joined := strings.Join(*paths, " ")
	if !strings.Contains(joined, testDigestAMD64) || !strings.Contains(joined, testDigestARM64) {
		t.Errorf("没有去拉子清单，路径：%v", *paths)
	}
}

func TestResolveSkipsWhenNotApplicable(t *testing.T) {
	srv, paths, _ := newStubRegistry(t, true)
	r := newClientFor(srv, "", "")

	for _, c := range []struct{ repo, tag string }{
		{"", "1.2.0.1058"},
		{"escloud/demowarning", ""},
	} {
		got, err := r.Resolve(context.Background(), c.repo, c.tag)
		if err != nil || got != nil {
			t.Errorf("repo=%q tag=%q 应返回 (nil, nil)，实际 (%v, %v)", c.repo, c.tag, got, err)
		}
	}
	if len(*paths) != 0 {
		t.Errorf("不适用时不该发任何请求，实际发了 %v", *paths)
	}
}

func TestResolveNotFoundIsError(t *testing.T) {
	srv, _, _ := newStubRegistry(t, true)
	r := newClientFor(srv, "", "")

	if _, err := r.Resolve(context.Background(), "escloud/demowarning", "9.9.9-nope"); err == nil {
		t.Fatal("tag 不存在时应报错，不能静默返回空元数据")
	}
}

// 404 必须映射成 ErrTagNotFound 哨兵，而不是一条普通的上游错误。
//
// 这条契约是"发单前镜像预检"的地基：平台只允许拿 404 去判定"这次发布必然失败"，
// 其余错误（网络/超时/权限）一律放行。若哪天有人把 404 改成走通用 UpstreamError，
// 预检会静默失效 —— 它不再拦任何东西，而没有任何地方会报错。
func TestResolveMissingTagIsSentinel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate",
			fmt.Sprintf(`Bearer realm="http://%s/service/token",service="harbor-registry"`, r.Host))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/service/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tk"})
	})
	// 仓库认这个 repo，但没有这个 tag → 真实仓库回 404。
	// /v2/ 这个前缀太宽，会把具体路径先抢走答 401，所以必须显式注册这一条才测得到。
	// （注册要在起服务之前：ServeMux 不支持边服务边注册。）
	mux.HandleFunc("/v2/escloud/demowarning/manifests/gone", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r := newClientFor(srv, "", "")

	_, err := r.Resolve(context.Background(), "escloud/demowarning", "gone")
	if err == nil {
		t.Fatal("tag 不存在时应报错")
	}
	if !errors.Is(err, ErrTagNotFound) {
		t.Fatalf("404 应映射成 ErrTagNotFound，实际：%v", err)
	}
}

func TestListTagsIsNotSorted(t *testing.T) {
	srv, _, _ := newStubRegistry(t, true)
	r := newClientFor(srv, "", "")

	tags, err := r.ListTags(context.Background(), "escloud/demowarning", 0)
	if err != nil {
		t.Fatalf("ListTags 报错：%v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("tag 数 = %d，应为 3", len(tags))
	}
	// 原样透传仓库给的顺序 —— 别在这里自作主张排序，那会造出"有排序"的假象
	if tags[0].Tag != "1.2.0.1044" || tags[1].Tag != "1.2.0.1058" {
		t.Errorf("顺序被改动了：%v, %v", tags[0].Tag, tags[1].Tag)
	}
	// 列表只给名字，不给 digest/大小（那要给每个 tag 各发两三个请求）
	if tags[0].Digest != "" || tags[0].SizeBytes != 0 {
		t.Errorf("列表项不该带 digest/大小，实际 %+v", tags[0])
	}

	limited, err := r.ListTags(context.Background(), "escloud/demowarning", 1)
	if err != nil || len(limited) != 1 {
		t.Errorf("limit=1 应为 1 项，实际 %d 项（err=%v）", len(limited), err)
	}
}

func TestPingTreats401AsReachable(t *testing.T) {
	srv, _, _ := newStubRegistry(t, true)
	r := newClientFor(srv, "", "")

	// /v2/ 返回 401 恰恰证明仓库在正常工作，把它判成失败会让状态页永远报红
	if err := r.Ping(context.Background()); err != nil {
		t.Errorf("401 应算连通，实际报错：%v", err)
	}
}

func TestParseChallenge(t *testing.T) {
	got := parseChallenge(`Bearer realm="https://es-repo.example.com:5000/service/token",service="harbor-registry"`)
	if got.realm != "https://es-repo.example.com:5000/service/token" {
		t.Errorf("realm 解析错：%q", got.realm)
	}
	if got.service != "harbor-registry" {
		t.Errorf("service 解析错：%q", got.service)
	}
	// 不认识的方案不该吐出 realm，否则会拿一个错地址去换 token
	if c := parseChallenge(`Basic realm="whatever"`); c.realm != "" {
		t.Errorf("Basic 挑战不该给出 realm，实际 %q", c.realm)
	}
	if c := parseChallenge(""); c.realm != "" || c.service != "" {
		t.Errorf("空头应解析为空，实际 %+v", c)
	}
}

func TestEscapeRepoPathKeepsSlashes(t *testing.T) {
	// registry v2 的 /v2/{name}/ 里斜杠是路径分隔符，编成 %2F 会 404
	if got := escapeRepoPath("escloud/demowarning"); got != "escloud/demowarning" {
		t.Errorf("斜杠应保留，实际 %q", got)
	}
	if got := escapeRepoPath("/escloud/demowarning/"); got != "escloud/demowarning" {
		t.Errorf("前后斜杠应剪掉，实际 %q", got)
	}
	if got := escapeRepoPath("escloud/a b"); !strings.Contains(got, "%20") {
		t.Errorf("段内非法字符应转义，实际 %q", got)
	}
}

func TestRegistryName(t *testing.T) {
	srv, _, _ := newStubRegistry(t, true)
	if got := newClientFor(srv, "", "").Name(); got != "harbor-registry-v2" {
		t.Errorf("Name() = %q", got)
	}
}

func TestNormalizeRegistryHost(t *testing.T) {
	// 同一个仓库在三个地方有三种写法，必须归一成同一个值，否则比对直接失效
	want := "es-repo.example.com:5000"
	for _, in := range []string{
		"es-repo.example.com:5000",
		"https://es-repo.example.com:5000",
		"http://es-repo.example.com:5000/",
		"  ES-Repo.Example.com:5000  ",
		"https://es-repo.example.com:5000/escloud/demowarning:1.2.0.1058",
	} {
		if got := normalizeRegistryHost(in); got != want {
			t.Errorf("normalizeRegistryHost(%q) = %q，应为 %q", in, got, want)
		}
	}
	if got := normalizeRegistryHost(""); got != "" {
		t.Errorf("空串应归一成空串，实际 %q", got)
	}
}

func TestAcceptsRejectsOtherRegistries(t *testing.T) {
	srv, _, _ := newStubRegistry(t, true)
	r := newClientFor(srv, "", "")

	// 认自己（配置里 registry 与 baseUrl 都算数）
	if !r.Accepts(strings.TrimPrefix(srv.URL, "http://")) {
		t.Error("配置里的 registry 应当被认")
	}
	if !r.Accepts(srv.URL) {
		t.Error("baseUrl 的同源写法应当被认")
	}
	// 不认别人 —— 问错仓库会把别人的 digest 挂到本次构建上
	for _, other := range []string{
		"harbor.other.internal",
		"es-repo.example.com:5001",
		"",
	} {
		if r.Accepts(other) {
			t.Errorf("不该认 %q", other)
		}
	}
}
