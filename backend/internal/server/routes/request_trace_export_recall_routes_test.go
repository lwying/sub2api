package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ticket09 的找回路由：GET /request-traces/exports 与 /exports/:id 是两条不同的
// 路径，前者是"我这个会话的任务"，后者是"按 ID 读一个任务"。它们必须各自解析到
// 各自的 handler：静态段先于参数段解析，因此找回永远不会被当成"按 id=exports 读"。
// 注册本身也不得 panic（冲突的路由在启动时就该炸，而不是等第一个请求）。

type recallRouteServiceStub struct {
	calls int
}

func (s *recallRouteServiceStub) CreateTask(context.Context, service.RequestTraceExportActor, service.RequestTraceExportFilter) (service.RequestTraceExportTask, error) {
	s.calls++
	return service.RequestTraceExportTask{}, nil
}

func (s *recallRouteServiceStub) GetTask(context.Context, service.RequestTraceExportActor, string) (service.RequestTraceExportTask, error) {
	s.calls++
	return service.RequestTraceExportTask{
		ID: "0123456789abcdef0123456789abcdef", Status: service.RequestTraceExportCompleted,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (s *recallRouteServiceStub) ListTasks(context.Context, service.RequestTraceExportActor, string, int) ([]service.RequestTraceExportTask, string, error) {
	s.calls++
	return []service.RequestTraceExportTask{{
		ID: "0123456789abcdef0123456789abcdef", Status: service.RequestTraceExportCompleted,
		CreatedAt: time.Now().UTC(),
	}}, "", nil
}

func (s *recallRouteServiceStub) OpenDownload(context.Context, service.RequestTraceExportActor, string) (*os.File, service.RequestTraceExportTask, error) {
	s.calls++
	return nil, service.RequestTraceExportTask{}, nil
}

func (s *recallRouteServiceStub) OpenShardDownload(context.Context, service.RequestTraceExportActor, string, int) (*os.File, service.RequestTraceExportShard, service.RequestTraceExportTask, error) {
	s.calls++
	return nil, service.RequestTraceExportShard{}, service.RequestTraceExportTask{}, nil
}

func TestRequestTraceExportRecallRouteIsDistinctFromReadingOneTask(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	stub := &recallRouteServiceStub{}
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		RequestTraceExport: adminhandler.NewRequestTraceExportHandler(stub),
	}}
	// 管理员登录会话：找回只对这种凭据开放。
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		c.Set("auth_method", service.AuditAuthMethodJWT)
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 12})
		c.Set(servermiddleware.ContextKeySessionID, "session-a")
		c.Next()
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	recallHandlers := map[string]string{}
	for _, route := range router.Routes() {
		if strings.Contains(route.Path, "/request-traces/exports") {
			recallHandlers[route.Method+" "+route.Path] = route.Handler
		}
	}
	require.Contains(t, recallHandlers[http.MethodGet+" /api/v1/admin/request-traces/exports"], "RequestTraceExportHandler).List")
	require.Contains(t, recallHandlers[http.MethodGet+" /api/v1/admin/request-traces/exports/:id"], "RequestTraceExportHandler).Get")

	// 找回真的走 List：响应是有界的 items 数组。
	recall := httptest.NewRecorder()
	router.ServeHTTP(recall, httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/exports", nil))
	require.Equal(t, http.StatusOK, recall.Code)
	require.Contains(t, recall.Body.String(), `"items"`)

	// 同一段路径加一个 id 仍然解析到按 ID 读取，而不是被找回吞掉。
	one := httptest.NewRecorder()
	router.ServeHTTP(one, httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces/exports/0123456789abcdef0123456789abcdef", nil))
	require.Equal(t, http.StatusOK, one.Code)
	require.NotContains(t, one.Body.String(), `"items"`, "reading one task is not a recall page")
}
