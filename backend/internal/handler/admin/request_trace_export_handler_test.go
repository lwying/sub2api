//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	requestTraceExportTestID     = "0123456789abcdef0123456789abcdef"
	requestTraceExportOtherID    = "ffffffffffffffffffffffffffffffff"
	requestTraceExportCanary     = "synthetic_export_prompt_canary"
	requestTraceExportDBError    = `pq: relation "request_trace_export_tasks" does not exist`
	requestTraceExportRouteBase  = "/api/v1/admin/request-traces/exports"
	requestTraceExportReasonGone = "REQUEST_TRACE_EXPORT_DOWNLOAD_EXPIRED"
)

// requestTraceExportStoreStub 只实现 handler 测试需要的任务存储语义，
// 含服务约定的单实例并发上限（maxInFlight）。
type requestTraceExportStoreStub struct {
	mu      sync.Mutex
	tasks   map[string]service.RequestTraceExportTask
	getErr  error
	creates int
}

func (s *requestTraceExportStoreStub) Create(_ context.Context, task service.RequestTraceExportTask, maxInFlight int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = make(map[string]service.RequestTraceExportTask)
	}
	inFlight := 0
	for _, current := range s.tasks {
		if current.InstanceID == task.InstanceID &&
			(current.Status == service.RequestTraceExportPending || current.Status == service.RequestTraceExportRunning) {
			inFlight++
		}
	}
	if inFlight >= maxInFlight {
		return service.ErrRequestTraceExportLimit
	}
	s.creates++
	s.tasks[task.ID] = task
	return nil
}

func (s *requestTraceExportStoreStub) Get(_ context.Context, id string) (service.RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return service.RequestTraceExportTask{}, s.getErr
	}
	task, ok := s.tasks[id]
	if !ok {
		return service.RequestTraceExportTask{}, service.ErrRequestTraceExportNotFound
	}
	return task, nil
}

func (s *requestTraceExportStoreStub) ListStale(context.Context, string, time.Time, int) ([]service.RequestTraceExportTask, error) {
	return nil, nil
}

// ListForSession 复制真实存储的过滤语义：管理员、会话摘要、实例三者必须同时匹配，
// 最近创建的在前，export_id 降序作为稳定次序键，页大小有界。handler 的找回测试
// 因此断言的是"这个会话能看到什么"，而不是一个什么都返回的替身。
func (s *requestTraceExportStoreStub) ListForSession(_ context.Context, adminUserID int64, sessionDigest, instanceID string, limit int) ([]service.RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > service.RequestTraceExportMaxListedTasks {
		return nil, service.ErrRequestTraceExportLimit
	}
	matched := make([]service.RequestTraceExportTask, 0)
	for _, task := range s.tasks {
		if task.AdminUserID != adminUserID || task.SessionDigest != sessionDigest || task.InstanceID != instanceID {
			continue
		}
		matched = append(matched, task)
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID > matched[j].ID
	})
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

func (s *requestTraceExportStoreStub) Claim(_ context.Context, instanceID string) (service.RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, task := range s.tasks {
		if task.InstanceID != instanceID || task.Status != service.RequestTraceExportPending {
			continue
		}
		task.Status = service.RequestTraceExportRunning
		s.tasks[id] = task
		return task, nil
	}
	return service.RequestTraceExportTask{}, service.ErrRequestTraceExportNotFound
}

func (s *requestTraceExportStoreStub) Finish(_ context.Context, task service.RequestTraceExportTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = make(map[string]service.RequestTraceExportTask)
	}
	s.tasks[task.ID] = task
	return nil
}

func (s *requestTraceExportStoreStub) Expired(context.Context, time.Time, int) ([]service.RequestTraceExportTask, error) {
	return nil, nil
}

func (s *requestTraceExportStoreStub) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
	return nil
}

func (s *requestTraceExportStoreStub) createCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

func (s *requestTraceExportStoreStub) failGetWith(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getErr = err
}

func (s *requestTraceExportStoreStub) task(t *testing.T, id string) service.RequestTraceExportTask {
	t.Helper()
	task, err := s.Get(context.Background(), id)
	require.NoError(t, err)
	return task
}

// requestTraceExportSourceStub 返回一条已净化明细和一条“导出时已被删除”的记录。
type requestTraceExportSourceStub struct {
	ids     []string
	deleted map[string]bool
	fails   map[string]bool // 枚举之后读取失败的 ID：与“已删除”是两类事实
	details map[string]service.RequestTraceExportApprovedDetail
}

func (s *requestTraceExportSourceStub) NextTracePage(_ context.Context, _ service.RequestTraceExportFilter, after string, limit int) ([]string, string, error) {
	start := 0
	if after != "" {
		for i, id := range s.ids {
			if id == after {
				start = i + 1
				break
			}
		}
	}
	if start >= len(s.ids) {
		return nil, "", nil
	}
	end := min(start+limit, len(s.ids))
	return s.ids[start:end], s.ids[end-1], nil
}

func (s *requestTraceExportSourceStub) ReadApprovedDetail(_ context.Context, id string) (service.RequestTraceExportApprovedDetail, bool, error) {
	if s.fails[id] {
		return service.RequestTraceExportApprovedDetail{}, false, errors.New("synthetic detail read failure")
	}
	if s.deleted[id] {
		return service.RequestTraceExportApprovedDetail{}, false, nil
	}
	return s.details[id], true, nil
}

// requestTraceExportServiceStub 记录 HTTP 层是否真的把请求交给了 service。
// 它用来证明会话门禁由 handler 自己执行，而不是靠 service 兜底。
type requestTraceExportServiceStub struct {
	calls int
	// listLimit 记录 handler 交给 service 的页大小：handler 只解析参数，边界由
	// service 拥有，所以这里断言的是"原样透传"，不是"handler 自己截断"。
	listLimit int
	listErr   error
}

func (s *requestTraceExportServiceStub) CreateTask(context.Context, service.RequestTraceExportActor, service.RequestTraceExportFilter) (service.RequestTraceExportTask, error) {
	s.calls++
	return service.RequestTraceExportTask{}, nil
}

func (s *requestTraceExportServiceStub) GetTask(context.Context, service.RequestTraceExportActor, string) (service.RequestTraceExportTask, error) {
	s.calls++
	return service.RequestTraceExportTask{}, nil
}

func (s *requestTraceExportServiceStub) ListTasks(_ context.Context, _ service.RequestTraceExportActor, _ string, limit int) ([]service.RequestTraceExportTask, string, error) {
	s.calls++
	s.listLimit = limit
	if s.listErr != nil {
		return nil, "", s.listErr
	}
	return nil, "", nil
}

func (s *requestTraceExportServiceStub) OpenDownload(context.Context, service.RequestTraceExportActor, string) (*os.File, service.RequestTraceExportTask, error) {
	s.calls++
	return nil, service.RequestTraceExportTask{}, nil
}

func (s *requestTraceExportServiceStub) OpenShardDownload(context.Context, service.RequestTraceExportActor, string, int) (*os.File, service.RequestTraceExportShard, service.RequestTraceExportTask, error) {
	s.calls++
	return nil, service.RequestTraceExportShard{}, service.RequestTraceExportTask{}, nil
}

func newRequestTraceExportTestSource() *requestTraceExportSourceStub {
	return &requestTraceExportSourceStub{
		ids:     []string{requestTraceExportTestID, requestTraceExportOtherID},
		deleted: map[string]bool{requestTraceExportOtherID: true},
		details: map[string]service.RequestTraceExportApprovedDetail{
			requestTraceExportTestID: {
				TraceID:     requestTraceExportTestID,
				RouteFamily: "messages",
				Stages: []service.RequestTraceExportApprovedStage{{
					Ordinal: 1, Stage: "client_entry", State: "redaction_unverified",
					PayloadText: requestTraceExportCanary, RedactionUnverified: true,
				}},
			},
		},
	}
}

func newRequestTraceExportTestHandler(t *testing.T, store *requestTraceExportStoreStub, source *requestTraceExportSourceStub, mutate func(*service.RequestTraceExportOptions)) (*RequestTraceExportHandler, *service.RequestTraceExportService) {
	t.Helper()
	options := service.RequestTraceExportOptions{
		Enabled:                true,
		SingleInstanceDeclared: true,
		InstanceID:             "instance-one",
		TempDir:                t.TempDir(),
	}
	if mutate != nil {
		mutate(&options)
	}
	svc := service.NewRequestTraceExportService(store, source, options)
	// 夹具显式声明"管理员已接受当前版本的导出风险声明"；未确认时创建任务按设计被拒。
	svc.SetAcknowledgementSatisfiedForTest(true)
	return NewRequestTraceExportHandler(svc), svc
}

func requestTraceExportContext(method, target string, params gin.Params) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = params
	c.Request = httptest.NewRequest(method, target, nil)
	return c, recorder
}

// requestTraceExportContextWithBody 构造带显式请求体的 POST。
// "导出所选"的结构化范围只走请求体，handler 从 JSON body 读取有界 ID 集合。
func requestTraceExportContextWithBody(target, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	return c, recorder
}

func requestTraceExportAdminSession(c *gin.Context, userID int64, sessionID string) {
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID})
	c.Set(middleware.ContextKeySessionID, sessionID)
}

// requestTraceExportAudit 读取中间件写入的审计附加字段（SetAuditExtra 的落点）。
func requestTraceExportAudit(c *gin.Context) map[string]any {
	value, ok := c.Get("audit_extra")
	if !ok {
		return map[string]any{}
	}
	extras, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return extras
}

func requestTraceExportReason(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	return payload.Reason
}

func requireRequestTraceExportAuditHasNoInput(t *testing.T, c *gin.Context, inputs ...string) {
	t.Helper()
	for key, value := range requestTraceExportAudit(c) {
		text := strings.ToLower(fmt.Sprint(value))
		for _, input := range inputs {
			require.NotContains(t, text, strings.ToLower(input), "audit field %q echoed caller input", key)
		}
	}
}

func createRequestTraceExportTask(t *testing.T, handler *RequestTraceExportHandler) string {
	t.Helper()
	c, recorder := requestTraceExportContext(http.MethodPost, requestTraceExportRouteBase, nil)
	requestTraceExportAdminSession(c, 12, "session-a")
	handler.Create(c)
	require.Equal(t, http.StatusAccepted, recorder.Code)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &created))
	require.NotEmpty(t, created.Data.ID)
	return created.Data.ID
}

// 只有管理员登录会话（JWT + 认证主体 + 绑定会话）可以创建导出；没有回退身份。
func TestRequestTraceExportHandlerCreateRequiresAdminLoginSessionWithoutFallback(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)

	cases := []struct {
		name       string
		handler    *RequestTraceExportHandler
		prepare    func(*gin.Context)
		wantStatus int
		wantReason string
	}{
		{
			name:       "admin api key is rejected",
			handler:    handler,
			prepare:    func(c *gin.Context) { c.Set("auth_method", service.AuditAuthMethodAdminAPIKey) },
			wantStatus: http.StatusForbidden,
			wantReason: "REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED",
		},
		{
			name:    "missing auth method falls back to nothing",
			handler: handler,
			prepare: func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 12})
				c.Set(middleware.ContextKeySessionID, "session-a")
			},
			wantStatus: http.StatusForbidden,
			wantReason: "REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED",
		},
		{
			name:    "authenticated subject is required",
			handler: handler,
			prepare: func(c *gin.Context) {
				c.Set("auth_method", service.AuditAuthMethodJWT)
				c.Set(middleware.ContextKeySessionID, "session-a")
			},
			wantStatus: http.StatusForbidden,
			wantReason: "REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED",
		},
		{
			name:    "zero subject id is rejected",
			handler: handler,
			prepare: func(c *gin.Context) {
				requestTraceExportAdminSession(c, 0, "session-a")
			},
			wantStatus: http.StatusForbidden,
			wantReason: "REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED",
		},
		{
			name:    "bound session id is required",
			handler: handler,
			prepare: func(c *gin.Context) {
				c.Set("auth_method", service.AuditAuthMethodJWT)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 12})
			},
			wantStatus: http.StatusForbidden,
			wantReason: "REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED",
		},
		{
			name:       "unwired service is unavailable",
			handler:    NewRequestTraceExportHandler(nil),
			prepare:    func(c *gin.Context) { requestTraceExportAdminSession(c, 12, "session-a") },
			wantStatus: http.StatusServiceUnavailable,
			wantReason: "REQUEST_TRACE_EXPORT_UNAVAILABLE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := requestTraceExportContext(http.MethodPost, requestTraceExportRouteBase, nil)
			tc.prepare(c)
			tc.handler.Create(c)
			require.Equal(t, tc.wantStatus, recorder.Code)
			require.Equal(t, tc.wantReason, requestTraceExportReason(t, recorder))
			require.Equal(t, "failed", requestTraceExportAudit(c)["result"])
			require.Equal(t, tc.wantStatus, requestTraceExportAudit(c)["http_status"])
		})
	}
	require.Zero(t, store.createCount(), "rejected callers must never reach the export service")
}

// 创建成功返回 202 与纯元数据任务视图：没有公开 URL、路径或文件名。
func TestRequestTraceExportHandlerCreateReturns202MetadataOnly(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	query := "?route_family=messages&client_status=200&usage_linked=true" +
		"&created_from=" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) +
		"&created_to=" + time.Now().UTC().Format(time.RFC3339Nano)
	c, recorder := requestTraceExportContext(http.MethodPost, requestTraceExportRouteBase+query, nil)
	requestTraceExportAdminSession(c, 12, "session-a")

	handler.Create(c)

	require.Equal(t, http.StatusAccepted, recorder.Code)
	require.Equal(t, "no-store, private", recorder.Header().Get("Cache-Control"))
	require.Equal(t, 1, store.createCount())

	var payload struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, string(service.RequestTraceExportPending), payload.Data["status"])
	require.Equal(t, "messages", payload.Data["filter"].(map[string]any)["route_family"])
	require.NotEmpty(t, payload.Data["id"])
	for _, forbidden := range []string{"download_url", "url", "path", "filename", "instance_id", "session_digest"} {
		require.NotContains(t, payload.Data, forbidden)
	}
	require.NotContains(t, recorder.Body.String(), requestTraceExportCanary)

	extras := requestTraceExportAudit(c)
	require.Equal(t, "success", extras["result"])
	require.Equal(t, http.StatusAccepted, extras["http_status"])
	requireRequestTraceExportAuditHasNoInput(t, c, requestTraceExportCanary)
}

// 非元数据筛选一律 400，且审计只记录稳定错误码，不回显被拒输入。
func TestRequestTraceExportHandlerCreateRejectsNonMetadataFilter(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	queries := []string{
		"?trace_id=not-a-trace-id",
		"?trace_id=" + requestTraceExportCanary,
		"?route_family=" + requestTraceExportCanary,
		"?route_family=gemini",
		"?client_status=600",
		"?client_status=abc",
		"?created_from=not-a-time",
		"?created_to=2026-01-01T00:00:00Z&created_from=2026-02-01T00:00:00Z",
		// Equal bounds are not an interval: from must be strictly before to.
		"?created_from=2026-01-01T00:00:00Z&created_to=2026-01-01T00:00:00Z",
		"?usage_linked=maybe",
	}
	for index, query := range queries {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			c, recorder := requestTraceExportContext(http.MethodPost, requestTraceExportRouteBase+query, nil)
			requestTraceExportAdminSession(c, 12, "session-a")
			handler.Create(c)

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Equal(t, "REQUEST_TRACE_EXPORT_INVALID_FILTER", requestTraceExportReason(t, recorder))

			extras := requestTraceExportAudit(c)
			require.Equal(t, "failed", extras["result"])
			require.Equal(t, "request_trace_export_invalid_filter", extras["error_code"])
			requireRequestTraceExportAuditHasNoInput(t, c, requestTraceExportCanary, "not-a-time", "gemini")
		})
	}
	require.Zero(t, store.createCount())
}

// 容量／并发超限映射为 429，而不是 500 或 503。
func TestRequestTraceExportHandlerCreateMapsCapacityLimitTo429(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	require.NotEmpty(t, createRequestTraceExportTask(t, handler))

	second, secondRecorder := requestTraceExportContext(http.MethodPost, requestTraceExportRouteBase, nil)
	requestTraceExportAdminSession(second, 12, "session-a")
	handler.Create(second)

	require.Equal(t, http.StatusTooManyRequests, secondRecorder.Code)
	require.Equal(t, "REQUEST_TRACE_EXPORT_CAPACITY_LIMIT", requestTraceExportReason(t, secondRecorder))
	require.Equal(t, "failed", requestTraceExportAudit(second)["result"])
	require.Equal(t, "request_trace_export_capacity_limit", requestTraceExportAudit(second)["error_code"])
}

// 任务状态只对发起会话可见；未知任务 404；DB 错误折叠为 503 且不外泄错误文本。
func TestRequestTraceExportHandlerGetIsScopedToOwningSession(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	id := createRequestTraceExportTask(t, handler)

	owner, ownerRecorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+id, gin.Params{{Key: "id", Value: id}})
	requestTraceExportAdminSession(owner, 12, "session-a")
	handler.Get(owner)
	require.Equal(t, http.StatusOK, ownerRecorder.Code)
	require.Equal(t, "no-store, private", ownerRecorder.Header().Get("Cache-Control"))
	require.Equal(t, "success", requestTraceExportAudit(owner)["result"])
	require.Equal(t, http.StatusOK, requestTraceExportAudit(owner)["http_status"])

	for _, tc := range []struct {
		name       string
		userID     int64
		sessionID  string
		params     gin.Params
		wantStatus int
		wantReason string
	}{
		{
			name: "other session of same admin", userID: 12, sessionID: "session-b",
			params: gin.Params{{Key: "id", Value: id}}, wantStatus: http.StatusForbidden, wantReason: "REQUEST_TRACE_EXPORT_FORBIDDEN",
		},
		{
			name: "other admin", userID: 13, sessionID: "session-a",
			params: gin.Params{{Key: "id", Value: id}}, wantStatus: http.StatusForbidden, wantReason: "REQUEST_TRACE_EXPORT_FORBIDDEN",
		},
		{
			name: "unknown task", userID: 12, sessionID: "session-a",
			params: gin.Params{{Key: "id", Value: requestTraceExportOtherID}}, wantStatus: http.StatusNotFound, wantReason: "REQUEST_TRACE_EXPORT_NOT_FOUND",
		},
		{
			name: "malformed task id", userID: 12, sessionID: "session-a",
			params: gin.Params{{Key: "id", Value: "not-an-id"}}, wantStatus: http.StatusNotFound, wantReason: "REQUEST_TRACE_EXPORT_NOT_FOUND",
		},
		{
			name: "task_id param name also works", userID: 12, sessionID: "session-a",
			params: gin.Params{{Key: "task_id", Value: id}}, wantStatus: http.StatusOK, wantReason: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+tc.params[0].Value, tc.params)
			requestTraceExportAdminSession(c, tc.userID, tc.sessionID)
			handler.Get(c)
			require.Equal(t, tc.wantStatus, recorder.Code)
			if tc.wantReason != "" {
				require.Equal(t, tc.wantReason, requestTraceExportReason(t, recorder))
			}
		})
	}

	store.failGetWith(errors.New(requestTraceExportDBError))
	failing, failingRecorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+id, gin.Params{{Key: "id", Value: id}})
	requestTraceExportAdminSession(failing, 12, "session-a")
	handler.Get(failing)
	require.Equal(t, http.StatusServiceUnavailable, failingRecorder.Code)
	require.NotContains(t, failingRecorder.Body.String(), "pq:")
	require.NotContains(t, failingRecorder.Body.String(), "request_trace_export_tasks")
	require.Equal(t, "failed", requestTraceExportAudit(failing)["result"])
	require.Equal(t, "request_trace_export_unavailable", requestTraceExportAudit(failing)["error_code"])
}

// 下载：仅发起会话可流式获取，响应不可缓存，且不携带任何公开 URL 或路径。
func TestRequestTraceExportHandlerDownloadStreamsOnlyForOwningSession(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, svc := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	id := createRequestTraceExportTask(t, handler)

	completed, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, service.RequestTraceExportCompleted, completed.Status)
	require.Equal(t, int64(1), completed.RowsExported)
	require.Equal(t, int64(1), completed.RowsSkipped, "rows removed before the page was read are skipped and counted")

	owner, ownerRecorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+id+"/download", gin.Params{{Key: "id", Value: id}})
	requestTraceExportAdminSession(owner, 12, "session-a")
	handler.Download(owner)

	require.Equal(t, http.StatusOK, ownerRecorder.Code)
	require.Equal(t, "no-store, private", ownerRecorder.Header().Get("Cache-Control"))
	require.Contains(t, ownerRecorder.Header().Get("Content-Disposition"), "attachment;")
	require.NotContains(t, ownerRecorder.Header().Get("Content-Disposition"), requestTraceExportCanary)
	// 无 part 参数下载的是清单：它是 JSON，说明这次导出的范围与完整性。
	require.Equal(t, "application/json", ownerRecorder.Header().Get("Content-Type"))
	manifestBody := ownerRecorder.Body.String()
	require.Contains(t, manifestBody, `"shards"`)
	require.Contains(t, manifestBody, `"complete"`)
	require.NotContains(t, manifestBody, "http://")
	require.NotContains(t, manifestBody, "https://")
	require.NotContains(t, manifestBody, requestTraceExportCanary)
	require.Equal(t, "success", requestTraceExportAudit(owner)["result"])
	require.Equal(t, http.StatusOK, requestTraceExportAudit(owner)["http_status"])

	// 带 part 参数下载的是分片：JSONL，逐行是允许披露的详情。
	shardCtx, shardRecorder := requestTraceExportContext(http.MethodGet,
		requestTraceExportRouteBase+"/"+id+"/download?part=1", gin.Params{{Key: "id", Value: id}})
	requestTraceExportAdminSession(shardCtx, 12, "session-a")
	handler.Download(shardCtx)
	require.Equal(t, http.StatusOK, shardRecorder.Code)
	require.Equal(t, "application/x-ndjson", shardRecorder.Header().Get("Content-Type"))
	shardBody := shardRecorder.Body.String()
	require.Contains(t, shardBody, requestTraceExportCanary)
	require.NotContains(t, shardBody, "http://")
	require.NotContains(t, shardBody, "https://")

	// 越界序号不返回任何内容。
	badCtx, badRecorder := requestTraceExportContext(http.MethodGet,
		requestTraceExportRouteBase+"/"+id+"/download?part=9999", gin.Params{{Key: "id", Value: id}})
	requestTraceExportAdminSession(badCtx, 12, "session-a")
	handler.Download(badCtx)
	require.NotEqual(t, http.StatusOK, badRecorder.Code)
	require.NotContains(t, badRecorder.Body.String(), requestTraceExportCanary)

	for _, tc := range []struct {
		name      string
		authMeth  string
		userID    int64
		sessionID string
	}{
		{name: "other session of same admin", authMeth: service.AuditAuthMethodJWT, userID: 12, sessionID: "session-b"},
		{name: "other admin session", authMeth: service.AuditAuthMethodJWT, userID: 13, sessionID: "session-a"},
		{name: "admin api key", authMeth: service.AuditAuthMethodAdminAPIKey, userID: 12, sessionID: "session-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+id+"/download", gin.Params{{Key: "id", Value: id}})
			c.Set("auth_method", tc.authMeth)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tc.userID})
			c.Set(middleware.ContextKeySessionID, tc.sessionID)
			handler.Download(c)
			require.Equal(t, http.StatusForbidden, recorder.Code)
			require.NotContains(t, recorder.Body.String(), requestTraceExportCanary)
		})
	}
}

// 文件丢失与七天到期都映射为 410，并且不伪造替代内容。
func TestRequestTraceExportHandlerDownloadReportsExpiredAndLostFileAsGone(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, svc := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	id := createRequestTraceExportTask(t, handler)
	_, err := svc.RunOnce(context.Background())
	require.NoError(t, err)

	path := svc.ExportPath(id)
	require.NotEmpty(t, path)
	require.NoError(t, os.Remove(path))

	lost, lostRecorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+id+"/download", gin.Params{{Key: "id", Value: id}})
	requestTraceExportAdminSession(lost, 12, "session-a")
	handler.Download(lost)
	require.Equal(t, http.StatusGone, lostRecorder.Code)
	require.Equal(t, "REQUEST_TRACE_EXPORT_FILE_LOST", requestTraceExportReason(t, lostRecorder))
	require.NotContains(t, lostRecorder.Body.String(), requestTraceExportCanary)

	task := store.task(t, id)
	past := time.Now().Add(-time.Second)
	task.DownloadUntil = &past
	require.NoError(t, store.Finish(context.Background(), task))

	expired, expiredRecorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+id+"/download", gin.Params{{Key: "id", Value: id}})
	requestTraceExportAdminSession(expired, 12, "session-a")
	handler.Download(expired)
	require.Equal(t, http.StatusGone, expiredRecorder.Code)
	require.Equal(t, requestTraceExportReasonGone, requestTraceExportReason(t, expiredRecorder))
	require.NotContains(t, expiredRecorder.Body.String(), requestTraceExportCanary)
}

// 尚未产出文件的任务返回 409 + 专用原因码，而不是 410"已过期"。
// 这条映射是给轮询方看的：它必须能区分"还没好，重试"与"窗口已关，别再试了"。
func TestRequestTraceExportHandlerDownloadReportsNotReadyAsConflict(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, svc := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	actor := service.RequestTraceExportActor{AdminUserID: 12, SessionID: "session-a"}

	// 直接建一个仍处于 pending 的任务：后台尚未领取，也就还没有任何文件。
	task, err := svc.CreateTask(context.Background(), actor, service.RequestTraceExportFilter{})
	require.NoError(t, err)

	for _, target := range []string{
		requestTraceExportRouteBase + "/" + task.ID + "/download",
		requestTraceExportRouteBase + "/" + task.ID + "/download?part=1",
	} {
		c, recorder := requestTraceExportContext(http.MethodGet, target, gin.Params{{Key: "id", Value: task.ID}})
		requestTraceExportAdminSession(c, 12, "session-a")
		handler.Download(c)
		require.Equal(t, http.StatusConflict, recorder.Code)
		require.Equal(t, "REQUEST_TRACE_EXPORT_NOT_READY", requestTraceExportReason(t, recorder))
		require.NotContains(t, recorder.Body.String(), requestTraceExportCanary)
	}
}

// HTTP 层自行执行会话门禁：身份不完整时既不写响应之外的任何东西，
// 也绝不把请求转交给 service（不依赖 service 的会话校验兜底）。
func TestRequestTraceExportHandlerEnforcesSessionWithoutDelegatingToService(t *testing.T) {
	entries := []struct {
		name   string
		method string
		path   string
		params gin.Params
		call   func(*RequestTraceExportHandler, *gin.Context)
	}{
		{name: "create", method: http.MethodPost, path: requestTraceExportRouteBase, call: (*RequestTraceExportHandler).Create},
		{name: "status", method: http.MethodGet, path: requestTraceExportRouteBase + "/" + requestTraceExportTestID, params: gin.Params{{Key: "id", Value: requestTraceExportTestID}}, call: (*RequestTraceExportHandler).Get},
		{name: "download", method: http.MethodGet, path: requestTraceExportRouteBase + "/" + requestTraceExportTestID + "/download", params: gin.Params{{Key: "id", Value: requestTraceExportTestID}}, call: (*RequestTraceExportHandler).Download},
	}
	identities := []struct {
		name    string
		prepare func(*gin.Context)
	}{
		{name: "admin api key", prepare: func(c *gin.Context) { c.Set("auth_method", service.AuditAuthMethodAdminAPIKey) }},
		{
			name: "no auth method",
			prepare: func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 12})
				c.Set(middleware.ContextKeySessionID, "session-a")
			},
		},
		{name: "no subject", prepare: func(c *gin.Context) {
			c.Set("auth_method", service.AuditAuthMethodJWT)
			c.Set(middleware.ContextKeySessionID, "session-a")
		}},
		{name: "no bound session", prepare: func(c *gin.Context) {
			c.Set("auth_method", service.AuditAuthMethodJWT)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 12})
		}},
		{name: "blank bound session", prepare: func(c *gin.Context) { requestTraceExportAdminSession(c, 12, "   ") }},
		{name: "zero subject", prepare: func(c *gin.Context) { requestTraceExportAdminSession(c, 0, "session-a") }},
	}

	for _, entry := range entries {
		for _, identity := range identities {
			t.Run(entry.name+"/"+identity.name, func(t *testing.T) {
				stub := &requestTraceExportServiceStub{}
				handler := NewRequestTraceExportHandler(stub)
				c, recorder := requestTraceExportContext(entry.method, entry.path, entry.params)
				identity.prepare(c)
				entry.call(handler, c)
				require.Equal(t, http.StatusForbidden, recorder.Code)
				require.Equal(t, "REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED", requestTraceExportReason(t, recorder))
				require.Zero(t, stub.calls, "the handler must reject before the service is consulted")
			})
		}
		t.Run(entry.name+"/admin login session reaches service", func(t *testing.T) {
			stub := &requestTraceExportServiceStub{}
			handler := NewRequestTraceExportHandler(stub)
			c, recorder := requestTraceExportContext(entry.method, entry.path, entry.params)
			requestTraceExportAdminSession(c, 12, "session-a")
			entry.call(handler, c)
			require.NotEqual(t, http.StatusForbidden, recorder.Code)
			require.Equal(t, 1, stub.calls, "a valid admin login session must reach the service")
		})
	}
}

// 未声明单实例（无共享文件系统）时导出能力不可用：三个入口都返回 503。
func TestRequestTraceExportHandlerDisabledInstanceReturns503(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), func(options *service.RequestTraceExportOptions) {
		options.SingleInstanceDeclared = false
	})

	create, createRecorder := requestTraceExportContext(http.MethodPost, requestTraceExportRouteBase, nil)
	requestTraceExportAdminSession(create, 12, "session-a")
	handler.Create(create)
	require.Equal(t, http.StatusServiceUnavailable, createRecorder.Code)
	require.Equal(t, "REQUEST_TRACE_EXPORT_DISABLED", requestTraceExportReason(t, createRecorder))

	get, getRecorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+requestTraceExportTestID, gin.Params{{Key: "id", Value: requestTraceExportTestID}})
	requestTraceExportAdminSession(get, 12, "session-a")
	handler.Get(get)
	require.Equal(t, http.StatusServiceUnavailable, getRecorder.Code)
	require.Equal(t, "REQUEST_TRACE_EXPORT_DISABLED", requestTraceExportReason(t, getRecorder))

	download, downloadRecorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+requestTraceExportTestID+"/download", gin.Params{{Key: "id", Value: requestTraceExportTestID}})
	requestTraceExportAdminSession(download, 12, "session-a")
	handler.Download(download)
	require.Equal(t, http.StatusServiceUnavailable, downloadRecorder.Code)
	require.Equal(t, "REQUEST_TRACE_EXPORT_DISABLED", requestTraceExportReason(t, downloadRecorder))

	require.Zero(t, store.createCount())
}

// "导出所选"以有界结构化请求体提交明确 Trace ID 集合：既不进 URL query，
// 也不夹带任何元数据条件；服务端按原样保存这批成员。
func TestRequestTraceExportHandlerCreateSelectedTraceIDsUseStructuredBody(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	first := strings.Repeat("1", 32)
	second := strings.Repeat("2", 32)

	c, recorder := requestTraceExportContextWithBody(requestTraceExportRouteBase,
		fmt.Sprintf(`{"trace_ids":["%s","%s"]}`, first, second))
	requestTraceExportAdminSession(c, 12, "session-a")
	handler.Create(c)

	require.Equal(t, http.StatusAccepted, recorder.Code)
	require.Equal(t, 1, store.createCount())

	var payload struct {
		Data struct {
			ID     string `json:"id"`
			Filter struct {
				TraceIDs []string `json:"trace_ids"`
			} `json:"filter"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, []string{first, second}, payload.Data.Filter.TraceIDs)
	require.Equal(t, []string{first, second}, store.task(t, payload.Data.ID).Filter.TraceIDs)
	requireRequestTraceExportAuditHasNoInput(t, c, first, second)
}

// 所选集合的畸形输入一律 400 invalid_filter，绝不静默截断、去重或退化成
// 429"容量不足"；空集合也不能被当成"导出全部"。
func TestRequestTraceExportHandlerCreateSelectedRejectsMalformedSelection(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	valid := strings.Repeat("a", 32)

	overBound := make([]string, 0, service.RequestTraceExportMaxSelectedIDs+1)
	for index := 0; index <= service.RequestTraceExportMaxSelectedIDs; index++ {
		overBound = append(overBound, `"`+fmt.Sprintf("%032x", index)+`"`)
	}

	cases := []struct{ name, target, body string }{
		{name: "empty selection", target: requestTraceExportRouteBase, body: `{"trace_ids":[]}`},
		{name: "null selection", target: requestTraceExportRouteBase, body: `{"trace_ids":null}`},
		{name: "malformed id", target: requestTraceExportRouteBase, body: `{"trace_ids":["not-a-trace-id"]}`},
		{name: "duplicate id", target: requestTraceExportRouteBase, body: fmt.Sprintf(`{"trace_ids":["%s","%s"]}`, valid, valid)},
		{name: "selection above the bound", target: requestTraceExportRouteBase, body: `{"trace_ids":[` + strings.Join(overBound, ",") + `]}`},
		{name: "unknown body field", target: requestTraceExportRouteBase, body: fmt.Sprintf(`{"trace_ids":["%s"],"route_family":"messages"}`, valid)},
		{name: "metadata field smuggled into the body", target: requestTraceExportRouteBase, body: fmt.Sprintf(`{"trace_ids":["%s"],"trace_id":"%s"}`, valid, valid)},
		{name: "query filter beside a selection", target: requestTraceExportRouteBase + "?route_family=messages", body: fmt.Sprintf(`{"trace_ids":["%s"]}`, valid)},
		{name: "single trace query beside a selection", target: requestTraceExportRouteBase + "?trace_id=" + valid, body: fmt.Sprintf(`{"trace_ids":["%s"]}`, valid)},
		{name: "malformed json", target: requestTraceExportRouteBase, body: `{"trace_ids":[`},
		{name: "trailing json value", target: requestTraceExportRouteBase, body: fmt.Sprintf(`{"trace_ids":["%s"]}{}`, valid)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := requestTraceExportContextWithBody(tc.target, tc.body)
			requestTraceExportAdminSession(c, 12, "session-a")
			handler.Create(c)

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Equal(t, "REQUEST_TRACE_EXPORT_INVALID_FILTER", requestTraceExportReason(t, recorder))
			require.Equal(t, "failed", requestTraceExportAudit(c)["result"])
			require.Equal(t, "request_trace_export_invalid_filter", requestTraceExportAudit(c)["error_code"])
			requireRequestTraceExportAuditHasNoInput(t, c, valid, "not-a-trace-id")
		})
	}
	require.Zero(t, store.createCount(), "a malformed selection must never reach the export service")
}

// URL 上的 CSV 形式（旧的无界 trace_ids query）不再接受：它既不是所选范围，
// 也不能被默默忽略成"没有条件、导出全部"。
func TestRequestTraceExportHandlerCreateRejectsTraceIDsInQuery(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)
	valid := strings.Repeat("a", 32)

	for _, query := range []string{"?trace_ids=" + valid, "?trace_ids=" + valid + "," + strings.Repeat("b", 32)} {
		c, recorder := requestTraceExportContext(http.MethodPost, requestTraceExportRouteBase+query, nil)
		requestTraceExportAdminSession(c, 12, "session-a")
		handler.Create(c)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Equal(t, "REQUEST_TRACE_EXPORT_INVALID_FILTER", requestTraceExportReason(t, recorder))
	}
	require.Zero(t, store.createCount())
}

// 显式给出但为空白（`?route_family=` 或 `%20`）的条件必须拒绝：把它当成"没给"
// 会让调用方以为筛了一项，导出却变成全部。
func TestRequestTraceExportHandlerCreateRejectsBlankQueryFilterValues(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	handler, _ := newRequestTraceExportTestHandler(t, store, newRequestTraceExportTestSource(), nil)

	for _, query := range []string{
		"?trace_id=%20",
		"?route_family=%20",
		"?client_status=%20",
		"?usage_linked=%20",
		"?created_from=%20",
		"?created_to=",
		"?usage_log_id=",
		"?account_id=%20",
		"?group_id=%20",
		"?group_unknown=%20",
		"?requested_model=%20",
		"?model_unknown=%20",
		"?platform=%20",
		"?platform_unknown=%20",
	} {
		t.Run(query, func(t *testing.T) {
			c, recorder := requestTraceExportContext(http.MethodPost, requestTraceExportRouteBase+query, nil)
			requestTraceExportAdminSession(c, 12, "session-a")
			handler.Create(c)

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Equal(t, "REQUEST_TRACE_EXPORT_INVALID_FILTER", requestTraceExportReason(t, recorder))
		})
	}
	require.Zero(t, store.createCount())
}

// 任务视图必须带上分类型的跳过计数：管理员要能分辨"读取失败"与"记录消失"
// 各多少条，而不是只看到一个聚合数和一个首个原因。计数值只允许封闭原因码。
func TestRequestTraceExportHandlerTaskViewCarriesTypedSkipCounts(t *testing.T) {
	store := &requestTraceExportStoreStub{}
	ids := []string{
		"00000000000000000000000000000001",
		"00000000000000000000000000000002",
		"00000000000000000000000000000003",
		"00000000000000000000000000000004",
		"00000000000000000000000000000005",
	}
	source := &requestTraceExportSourceStub{
		ids:     ids,
		details: map[string]service.RequestTraceExportApprovedDetail{},
		fails:   map[string]bool{ids[0]: true, ids[1]: true},
		deleted: map[string]bool{ids[2]: true, ids[3]: true, ids[4]: true},
	}
	handler, svc := newRequestTraceExportTestHandler(t, store, source, nil)
	id := createRequestTraceExportTask(t, handler)

	completed, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(5), completed.RowsSkipped)
	require.Equal(t, service.RequestTraceExportSkipCounts{"read_failed": 2, "source_gone": 3}, completed.SkippedByReason)

	owner, recorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+id, gin.Params{{Key: "id", Value: id}})
	requestTraceExportAdminSession(owner, 12, "session-a")
	handler.Get(owner)

	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	require.Contains(t, body, `"rows_skipped":5`)
	require.Contains(t, body, `"skipped_by_reason":{"read_failed":2,"source_gone":3}`)
	require.Contains(t, body, `"truncated":true`)
	// 响应里只有封闭原因码与计数，绝不外泄原始错误串、路径或正文。
	require.NotContains(t, body, "synthetic detail read failure")
	require.NotContains(t, body, "sub2api-request-trace-export-")
}
