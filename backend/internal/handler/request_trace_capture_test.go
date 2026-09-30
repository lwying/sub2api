//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type requestTraceWriterRepoStub struct {
	mu     sync.Mutex
	traces []service.RequestTrace
	stages []service.RequestTraceStage
	fail   error
}

func (r *requestTraceWriterRepoStub) CreateRequestTrace(_ context.Context, trace service.RequestTrace) (service.RequestTrace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return service.RequestTrace{}, r.fail
	}
	r.traces = append(r.traces, trace)
	return trace, nil
}
func (r *requestTraceWriterRepoStub) AppendRequestTraceStage(_ context.Context, stage service.RequestTraceStage) (service.RequestTraceStage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages = append(r.stages, stage)
	return stage, nil
}
func (*requestTraceWriterRepoStub) ListRequestTraces(context.Context, service.RequestTraceListFilter) ([]service.RequestTrace, int64, error) {
	return nil, 0, nil
}
func (*requestTraceWriterRepoStub) GetRequestTrace(context.Context, string) (*service.RequestTraceDetail, error) {
	return nil, nil
}
func (*requestTraceWriterRepoStub) DeleteExpiredUnlinkedRequestTraces(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func TestRequestTraceCaptureDoesNotReadBodyIfGateClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	r := gin.New()
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(false) }, repo, queue), func(c *gin.Context) {
		c.Status(http.StatusUnauthorized)
	})
	body := &unreadableTraceBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body"))
	req.Body = body
	r.ServeHTTP(httptest.NewRecorder(), req)
	require.Zero(t, body.reads)
	require.Empty(t, repo.traces)
}

func waitForRequestTraceWrites(t *testing.T, repo *requestTraceWriterRepoStub, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		repo.mu.Lock()
		got := len(repo.stages)
		repo.mu.Unlock()
		if got >= count {
			return
		}
		runtime.Gosched()
	}
	require.FailNow(t, "Trace queue did not persist expected stage")
}

func TestRequestTraceCaptureDoesNotTouchUncoveredRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	r := gin.New()
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r.POST("/v1/messages/count_tokens", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.Status(http.StatusUnauthorized)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil))
	require.Empty(t, repo.traces)
}

func TestRequestTraceCapturePersistsAuthRejectionWithoutBodyRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &requestTraceWriterRepoStub{}
	r := gin.New()
	queue := service.NewRequestTraceCaptureQueue(repo)
	defer queue.Stop()
	r.POST("/v1/messages", RequestTraceCaptureMiddleware(func(context.Context) service.RequestTraceGate { return RequestTraceGateForCapture(true) }, repo, queue), func(c *gin.Context) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "rejected"})
	})
	body := &unreadableTraceBody{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body"))
	req.Body = body
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, body.reads)
	waitForRequestTraceWrites(t, repo, 1)
	require.Len(t, repo.traces, 1)
	require.Equal(t, service.RequestTraceMessages, repo.traces[0].RouteFamily)
	require.Equal(t, http.StatusUnauthorized, repo.traces[0].ClientStatus)
	require.Nil(t, repo.traces[0].UsageLogID)
	require.NotEmpty(t, repo.stages)
	require.Equal(t, service.RequestTraceNotObserved, repo.stages[0].State)
}
