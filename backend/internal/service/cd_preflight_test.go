package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/logstream"
)

// 本文件验证一件打真实仓库**做不出确定性复现**的事：发单前镜像预检的判定口径。
//
// 为什么用桩：预检的全部风险都在"仓库明确 404"与"仓库不可达"这两类必须区分开的
// 分支上——真仓库只能给出它此刻是什么，给不出这两类。而判错任何一边都要付代价：
// 一边是挡掉正常发版，另一边是把必然失败的发布放出去。
//
// 打真仓库的那一路在 `cd_preflight_live_test.go`（build tag `live`），只做只读探测。

// --------------------------------------------------------------------------- 桩

// stubCluster 占住 Deps.Up.Cluster：预检不碰集群，诊断只读它给出的证据。
type stubCluster struct {
	// evidence / evidenceErr 供诊断测试注入，预检不用。
	evidence    *integration.FailureEvidence
	evidenceErr error
	// 调用计数：用来钉住"取消纳管不缩容"这条纪律（Scale 会真的停服，Release 不会）。
	scaleCalls   int
	releaseCalls int
}

func (c *stubCluster) Describe(context.Context, integration.WorkloadRef) (*integration.WorkloadState, error) {
	return &integration.WorkloadState{}, nil
}

func (c *stubCluster) ApplyImage(context.Context, integration.ApplyRequest, func(string, uint8) error) error {
	return nil
}

func (c *stubCluster) Scale(context.Context, integration.WorkloadRef, int) error {
	c.scaleCalls++
	return nil
}

// Release 解除纳管（删变更单）。**故意与 Scale 分开计数**：
// `DeleteService` 曾经调 Scale(ref, 0) 把线上副本缩到 0，那是一个发布平台不该有的副作用。
func (c *stubCluster) Release(context.Context, integration.WorkloadRef) error {
	c.releaseCalls++
	return nil
}

func (c *stubCluster) Restart(context.Context, integration.WorkloadRef) error { return nil }
func (c *stubCluster) Pods(context.Context, integration.WorkloadRef) ([]integration.PodInfo, error) {
	return nil, nil
}
func (c *stubCluster) PodLogs(context.Context, integration.WorkloadRef, string, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (c *stubCluster) DiagnoseFailure(context.Context, integration.WorkloadRef) (*integration.FailureEvidence, error) {
	if c.evidenceErr != nil {
		return nil, c.evidenceErr
	}
	if c.evidence != nil {
		return c.evidence, nil
	}
	return &integration.FailureEvidence{}, nil
}
func (c *stubCluster) Nodes(context.Context, integration.ClusterRef) (*integration.ClusterState, error) {
	return &integration.ClusterState{}, nil
}
func (c *stubCluster) Ping(context.Context, integration.ClusterRef) error { return nil }
func (c *stubCluster) Name() string                                       { return "stub" }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newCDForTest 组装一个只够跑 preflightImage 的 CDService。
func newCDForTest(cluster integration.Cluster, registry integration.Registry) *CDService {
	return New(Deps{
		Cfg:    &config.Config{Runner: config.RunnerConfig{DeployTimeout: config.Duration(2 * time.Second)}},
		Log:    discardLogger(),
		Stream: logstream.NewMemStream(),
		Up:     &integration.Set{Cluster: cluster, Registry: registry},
	}).CD
}

// --------------------------------------------------------------------------- 镜像预检

// newStubRegistryForPreflight 起一个最小可用的 registry：能发 token、能给出 404。
func newStubRegistryForPreflight(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		// 未注册的具体路径也会落到这里：一律 401 挑战，与真实 Harbor 一致
		w.Header().Set("Www-Authenticate",
			fmt.Sprintf(`Bearer realm="http://%s/service/token",service="harbor-registry"`, r.Host))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/service/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tk"})
	})
	mux.HandleFunc("/v2/escloud/ynprobe/manifests/1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:deadbeef")
		_, _ = w.Write([]byte(`{"schemaVersion":2,"layers":[]}`))
	})
	mux.HandleFunc("/v2/escloud/ynprobe/manifests/9.9.9-gone", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(srv.URL, "http://")
}

func TestPreflightImage(t *testing.T) {
	srv, host := newStubRegistryForPreflight(t)
	reg := integration.NewHarborRegistry(config.HarborConfig{
		Enabled: true, BaseURL: srv.URL, Registry: host, Timeout: config.Duration(5 * time.Second),
	})
	cd := newCDForTest(&stubCluster{}, reg)

	t.Run("tag 在仓库里 → 放行", func(t *testing.T) {
		if err := cd.preflightImage(context.Background(), host+"/escloud/ynprobe:1.0.0"); err != nil {
			t.Fatalf("应当放行，实际拒绝：%v", err)
		}
	})

	t.Run("仓库明确 404 → 拒绝，且理由是可操作的", func(t *testing.T) {
		err := cd.preflightImage(context.Background(), host+"/escloud/ynprobe:9.9.9-gone")
		if err == nil {
			t.Fatal("tag 不存在时必须拒绝，不能放它去发布")
		}
		var be *errs.Error
		if !asErrs(err, &be) || be.Code != errs.CodeInvalidParam {
			t.Fatalf("应当是 40000 参数错误，实际 %v", err)
		}
		if !strings.Contains(err.Error(), "不在仓库中") {
			t.Errorf("错误信息要说清原因：%v", err)
		}
	})

	t.Run("不是本平台管的仓库 → 放行（不越界去问别的仓库）", func(t *testing.T) {
		if err := cd.preflightImage(context.Background(), "docker.io/library/nginx:1.25"); err != nil {
			t.Fatalf("别的仓库不该拦，实际：%v", err)
		}
	})

	t.Run("仓库不可达 → 放行（不能把仓库抖动变成不能发版）", func(t *testing.T) {
		// 指向一个没人监听的端口
		dead := integration.NewHarborRegistry(config.HarborConfig{
			Enabled: true, BaseURL: "http://127.0.0.1:1", Registry: host,
			Timeout: config.Duration(1 * time.Second),
		})
		cd2 := newCDForTest(&stubCluster{}, dead)
		if err := cd2.preflightImage(context.Background(), host+"/escloud/ynprobe:1.0.0"); err != nil {
			t.Fatalf("仓库不可达时应当放行并记日志，实际拒绝：%v", err)
		}
	})
}

// asErrs 把错误断言成 *errs.Error（比 errors.As 少写一个泛型参数）。
func asErrs(err error, target **errs.Error) bool {
	e, ok := err.(*errs.Error)
	if ok {
		*target = e
	}
	return ok
}
