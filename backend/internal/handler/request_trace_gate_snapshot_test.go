//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// gateSnapshotTraceRepo 只记录采集结果，供"入口快照"用例断言采/不采。
// 单独定义而不复用其它测试文件里的 stub，避免本文件依赖同包其他文件的改动。
type gateSnapshotTraceRepo struct {
	mu     sync.Mutex
	traces []service.RequestTrace
	stages []service.RequestTraceStage
}

func (r *gateSnapshotTraceRepo) CreateRequestTrace(_ context.Context, trace service.RequestTrace) (service.RequestTrace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.traces = append(r.traces, trace)
	return trace, nil
}

func (r *gateSnapshotTraceRepo) AppendRequestTraceStage(_ context.Context, stage service.RequestTraceStage) (service.RequestTraceStage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages = append(r.stages, stage)
	return stage, nil
}

func (*gateSnapshotTraceRepo) ListRequestTraces(context.Context, service.RequestTraceListFilter) ([]service.RequestTrace, int64, error) {
	return nil, 0, nil
}

func (*gateSnapshotTraceRepo) GetRequestTrace(context.Context, string) (*service.RequestTraceDetail, error) {
	return nil, nil
}

func (*gateSnapshotTraceRepo) DeleteExpiredUnlinkedRequestTraces(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (r *gateSnapshotTraceRepo) storedTraces() []service.RequestTrace {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]service.RequestTrace(nil), r.traces...)
}

// gateSnapshotUnreadableBody 记录正文是否被读取；鉴权前任何预读都会暴露出来。
type gateSnapshotUnreadableBody struct{ reads int32 }

func (b *gateSnapshotUnreadableBody) Read([]byte) (int, error) {
	atomic.AddInt32(&b.reads, 1)
	return 0, nil
}
func (b *gateSnapshotUnreadableBody) Close() error { return nil }

func (b *gateSnapshotUnreadableBody) readCount() int32 { return atomic.LoadInt32(&b.reads) }

func waitForGateSnapshotTraces(t *testing.T, repo *gateSnapshotTraceRepo, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(repo.storedTraces()) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.FailNowf(t, "Trace 未按预期落库", "want >= %d traces, got %d", want, len(repo.storedTraces()))
}

// changedScopeGate 模拟请求执行中管理员把范围收窄为"仅指定分组"，且该分组与本次请求观察到的事实不符。
func changedScopeGate() service.RequestTraceGate {
	return service.RequestTraceGate{
		CaptureAllowed: true,
		Scope: service.RequestTraceSettings{
			Enabled: true, RiskAcknowledged: true,
			AllGroups: false, GroupIDs: []int64{424242},
			ModelScope:    service.RequestTraceScopeAll,
			PlatformScope: service.RequestTraceScopeAll,
		},
	}
}

// 核心用例：一次请求内门控先返回"可采（全范围）"再返回"收窄后的范围"，
// 采集结论必须用入口时的第一份快照，且门控整个请求只解析一次。
func TestRequestTraceCaptureFreezesEntryGateSnapshotForRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &gateSnapshotTraceRepo{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()

	var mu sync.Mutex
	resolves := 0
	resolveGate := func(context.Context) service.RequestTraceGate {
		mu.Lock()
		defer mu.Unlock()
		resolves++
		if resolves == 1 {
			return RequestTraceGateForCapture(true)
		}
		return changedScopeGate()
	}

	r := gin.New()
	r.POST("/v1/messages",
		RequestTraceCaptureMiddleware(resolveGate, repo, queue),
		func(c *gin.Context) { c.JSON(http.StatusUnauthorized, gin.H{"error": "rejected"}) },
	)

	body := &gateSnapshotUnreadableBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("no key"))
	req.Body = body
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, body.readCount(), "判定采集范围不得预读鉴权前的请求正文")

	waitForGateSnapshotTraces(t, repo, 1)

	mu.Lock()
	gotResolves := resolves
	mu.Unlock()
	require.Equal(t, 1, gotResolves, "门控在一次请求内只解析一次，范围按入口快照冻结")
	require.Len(t, repo.storedTraces(), 1,
		"在途请求必须沿用入口快照：请求中途管理员改范围不得改变本次采集结论")
}

// 非采集路径：不在支持路由上时，门控一次都不该解析（与采集路径区分开）。
func TestRequestTraceCaptureSkipsGateResolutionOffSupportedRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &gateSnapshotTraceRepo{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()

	var resolves int32
	r := gin.New()
	r.POST("/v1/messages/count_tokens",
		RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate {
			atomic.AddInt32(&resolves, 1)
			return RequestTraceGateForCapture(true)
		}, repo, queue),
		func(c *gin.Context) { c.Status(http.StatusUnauthorized) },
	)

	body := &gateSnapshotUnreadableBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader("body"))
	req.Body = body
	r.ServeHTTP(httptest.NewRecorder(), req)

	require.Zero(t, atomic.LoadInt32(&resolves), "未支持的入口不应解析门控")
	require.Empty(t, repo.storedTraces())
	require.Zero(t, body.readCount())
}

// 非采集路径：门控关闭时入口只解析一次，不落库、不读正文。
func TestRequestTraceCaptureClosedGateResolvesOnceAndStoresNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &gateSnapshotTraceRepo{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()

	var resolves int32
	r := gin.New()
	r.POST("/v1/messages",
		RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate {
			atomic.AddInt32(&resolves, 1)
			return RequestTraceGateForCapture(false)
		}, repo, queue),
		func(c *gin.Context) { c.Status(http.StatusUnauthorized) },
	)

	body := &gateSnapshotUnreadableBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body"))
	req.Body = body
	r.ServeHTTP(httptest.NewRecorder(), req)

	require.Equal(t, int32(1), atomic.LoadInt32(&resolves))
	require.Empty(t, repo.storedTraces())
	require.Zero(t, body.readCount())
}

// 缺失门控解析器时必须 fail-closed：不 panic、不采集、不读正文。
func TestRequestTraceCaptureFailsClosedWithoutGateResolver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &gateSnapshotTraceRepo{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()

	r := gin.New()
	r.POST("/v1/messages",
		RequestTraceCaptureMiddleware(nil, repo, queue),
		func(c *gin.Context) { c.Status(http.StatusUnauthorized) },
	)

	body := &gateSnapshotUnreadableBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body"))
	req.Body = body
	require.NotPanics(t, func() { r.ServeHTTP(httptest.NewRecorder(), req) })

	require.Empty(t, repo.storedTraces())
	require.Zero(t, body.readCount())
}

// 入口快照在请求上下文中丢失时同样 fail-closed：不能用"当前配置"补判后照常采集。
func TestRequestTraceCaptureFailsClosedWhenEntrySnapshotLost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &gateSnapshotTraceRepo{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()

	r := gin.New()
	r.POST("/v1/messages",
		RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate {
			return RequestTraceGateForCapture(true)
		}, repo, queue),
		// 模拟下游中间件丢弃请求上下文（入口快照随之丢失）。
		func(c *gin.Context) { c.Request = c.Request.WithContext(context.Background()) },
		func(c *gin.Context) { c.Status(http.StatusUnauthorized) },
	)

	body := &gateSnapshotUnreadableBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body"))
	req.Body = body
	r.ServeHTTP(httptest.NewRecorder(), req)

	time.Sleep(50 * time.Millisecond)
	require.Empty(t, repo.storedTraces(), "丢失入口快照必须按不采集处理")
	require.Zero(t, body.readCount())
}

type gateSnapshotModeKey struct{}

// 并发用例：快照必须按请求存放，不能落在跨请求共享的闭包状态里。
func TestRequestTraceCaptureGateSnapshotIsPerRequestUnderConcurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &gateSnapshotTraceRepo{}
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()

	var resolves int32
	resolveGate := func(ctx context.Context) service.RequestTraceGate {
		atomic.AddInt32(&resolves, 1)
		if mode, _ := ctx.Value(gateSnapshotModeKey{}).(string); mode == "allow" {
			return RequestTraceGateForCapture(true)
		}
		return RequestTraceGateForCapture(false)
	}

	r := gin.New()
	r.POST("/v1/messages",
		func(c *gin.Context) {
			mode := c.GetHeader("X-Trace-Mode")
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), gateSnapshotModeKey{}, mode))
			c.Next()
		},
		RequestTraceCaptureMiddleware(resolveGate, repo, queue),
		func(c *gin.Context) { c.Status(http.StatusUnauthorized) },
	)

	const total, allowed = 24, 12
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		mode := "deny"
		if i%2 == 0 {
			mode = "allow"
		}
		wg.Add(1)
		go func(mode string) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body"))
			req.Header.Set("X-Trace-Mode", mode)
			r.ServeHTTP(httptest.NewRecorder(), req)
		}(mode)
	}
	wg.Wait()

	waitForGateSnapshotTraces(t, repo, allowed)
	require.Len(t, repo.storedTraces(), allowed, "每个请求的采集结论只能由自己的入口快照决定")
	require.Equal(t, int32(total), atomic.LoadInt32(&resolves), "每个请求恰好解析一次门控")
}
