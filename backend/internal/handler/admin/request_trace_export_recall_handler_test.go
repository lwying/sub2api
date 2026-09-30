//go:build unit

package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ticket09 的 HTTP 契约：找回是**会话**找回，不是"按 ID 找回"。
// 会话就是过滤器——调用方给不出另一个会话的参数；同一个管理员的另一个会话
// 看到的是空列表，而不是别人的任务；管理员 API Key 与普通用户被直接拒。
//
// 一页装不下时，响应给出下一页的不透明游标：第 21 个任务必须仍然可以取回，
// 而不是因为"一次只读 20 条"就从列表里消失。畸形游标是 400，不是"没有更多"。

const recallSessionCanary = "synthetic_recall_session_canary"

// recallSessionID 与 recallListRequest 默认使用的登录会话一致；库里只存摘要。
const recallSessionID = "session-a"

func recallSessionDigest(session string) string {
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:])
}

// recallStore 让 handler 层能走通真实的 service：它复用基础替身的生命周期语义，
// 只补上 service 要求的翻页读。过滤（管理员 + 会话摘要 + 实例）、次序
// （created_at DESC, export_id DESC）与键集边界都按真实存储的语义实现，
// 因此这里断言的是"这个会话能看到什么"，而不是一个什么都返回的替身。
type recallStore struct {
	*requestTraceExportStoreStub
}

func (s *recallStore) ListForSession(_ context.Context, adminUserID int64, sessionDigest, instanceID string, after *service.RequestTraceExportRecallCursor, limit int) ([]service.RequestTraceExportTask, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > service.RequestTraceExportMaxListedTasks {
		return nil, false, service.ErrRequestTraceExportLimit
	}
	matched := make([]service.RequestTraceExportTask, 0)
	for _, task := range s.tasks {
		if task.AdminUserID != adminUserID || task.SessionDigest != sessionDigest || task.InstanceID != instanceID {
			continue
		}
		// 键集边界是**值**：严格晚于边界（即排序上排在边界之后）的行才属于续页，
		// 边界行本身即使已被清理也不影响比较。
		if after != nil && !(task.CreatedAt.Before(after.CreatedAt) ||
			(task.CreatedAt.Equal(after.CreatedAt) && task.ID < after.ExportID)) {
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
	more := len(matched) > limit
	if more {
		matched = matched[:limit]
	}
	return matched, more, nil
}

// newRecallHandler 用真实的 service 装配 handler，选项与其它导出测试保持一致。
func newRecallHandler(t *testing.T, store service.RequestTraceExportStore) *RequestTraceExportHandler {
	t.Helper()
	handler, _ := newRecallHandlerWithDir(t, store)
	return handler
}

// newRecallHandlerWithDir 额外交回服务的临时目录，用来放**真实格式**的清单文件。
func newRecallHandlerWithDir(t *testing.T, store service.RequestTraceExportStore) (*RequestTraceExportHandler, string) {
	t.Helper()
	dir := t.TempDir()
	svc := service.NewRequestTraceExportService(store, newRequestTraceExportTestSource(), service.RequestTraceExportOptions{
		Enabled: true, SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir,
	})
	svc.SetAcknowledgementSatisfiedForTest(true)
	return NewRequestTraceExportHandler(svc), dir
}

// recallManifestFileName 是服务端的清单命名规则（`<export file>.manifest.json`）：
// 「文件已丢失」与「交付不完整」的区别就落在这份文件上，所以测试要按真实名字写它。
func recallManifestFileName(id string) string {
	return "sub2api-request-trace-export-" + id + ".manifest.json"
}

func recallSeedTask(store *recallStore, task service.RequestTraceExportTask) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.tasks == nil {
		store.tasks = make(map[string]service.RequestTraceExportTask)
	}
	store.tasks[task.ID] = task
}

func recallListRequest(t *testing.T, handler *RequestTraceExportHandler, target string, prepare func(*gin.Context)) (int, []byte) {
	t.Helper()
	c, recorder := requestTraceExportContext(http.MethodGet, target, nil)
	if prepare == nil {
		requestTraceExportAdminSession(c, 12, recallSessionID)
	} else {
		prepare(c)
	}
	handler.List(c)
	return recorder.Code, recorder.Body.Bytes()
}

func recallListItemIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var payload struct {
		Data struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	ids := make([]string, 0, len(payload.Data.Items))
	for _, item := range payload.Data.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// recallListNextCursor 读出下一页游标。键必须**恒在**：null 是"没有下一页"这个事实，
// 不是缺席的字段——缺席会让客户端无法区分"读完了"与"服务端忘了给"。
func recallListNextCursor(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	raw, present := payload.Data["next_cursor"]
	require.True(t, present, "the list must always state whether a next page exists")
	if string(raw) == "null" {
		return ""
	}
	var token string
	require.NoError(t, json.Unmarshal(raw, &token))
	return token
}

// 找回只给出**这个会话**的任务，且响应里没有文件名、路径或会话摘要；
// 只有一页时不给游标，因为确实没有下一页。
func TestRequestTraceExportHandlerListReturnsOnlyCallerSessionTasks(t *testing.T) {
	store := &recallStore{requestTraceExportStoreStub: &requestTraceExportStoreStub{}}
	handler := newRecallHandler(t, store)
	created := createRequestTraceExportTask(t, handler)

	// 同一管理员的另一个会话、另一个管理员的任务都不属于这次找回。
	recallSeedTask(store, service.RequestTraceExportTask{
		ID: "44444444444444444444444444444444", Status: service.RequestTraceExportCompleted,
		AdminUserID: 12, SessionDigest: recallSessionDigest("session-b"), InstanceID: "instance-one",
		Filename: recallSessionCanary + ".jsonl", CreatedAt: time.Now().UTC(),
	})
	recallSeedTask(store, service.RequestTraceExportTask{
		ID: "55555555555555555555555555555555", Status: service.RequestTraceExportCompleted,
		AdminUserID: 13, SessionDigest: recallSessionDigest(recallSessionID), InstanceID: "instance-one",
		Filename: recallSessionCanary + ".jsonl", CreatedAt: time.Now().UTC(),
	})

	status, body := recallListRequest(t, handler, requestTraceExportRouteBase, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{created}, recallListItemIDs(t, body))
	require.Empty(t, recallListNextCursor(t, body), "one row is the whole set")
	text := string(body)
	require.NotContains(t, text, recallSessionCanary, "no filename or session material may leave the list")
	require.NotContains(t, text, "filename")
	require.NotContains(t, text, "session_digest")
	require.NotContains(t, text, "instance_id")
}

// 超过一页时，最早的任务必须仍然取得到：第一页给出游标，把它交回来就拿到剩下的一行。
func TestRequestTraceExportHandlerListWalksPastOnePageWithItsCursor(t *testing.T) {
	store := &recallStore{requestTraceExportStoreStub: &requestTraceExportStoreStub{}}
	handler := newRecallHandler(t, store)
	now := time.Now().UTC()
	want := make([]string, 0, 21)
	for i := 0; i < 21; i++ {
		id := fmt.Sprintf("%032x", i)
		want = append(want, id)
		recallSeedTask(store, service.RequestTraceExportTask{
			ID: id, Status: service.RequestTraceExportCompleted,
			AdminUserID: 12, SessionDigest: recallSessionDigest(recallSessionID), InstanceID: "instance-one",
			CreatedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}

	status, body := recallListRequest(t, handler, requestTraceExportRouteBase, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, want[:20], recallListItemIDs(t, body))
	token := recallListNextCursor(t, body)
	require.NotEmpty(t, token, "the twenty-first task must stay reachable")

	status, body = recallListRequest(t, handler, requestTraceExportRouteBase+"?cursor="+token, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, want[20:], recallListItemIDs(t, body))
	require.Empty(t, recallListNextCursor(t, body), "the last page must not offer another one")
}

// 畸形游标是调用方的问题（400），不是"没有更多"，也不是"容量不足"：
// 把无法解释的令牌当成"读完了"会让仍然存在的任务看起来不存在。
func TestRequestTraceExportHandlerListRefusesUnreadableCursors(t *testing.T) {
	stub := &recallPagedServiceStub{requestTraceExportServiceStub: &requestTraceExportServiceStub{}}
	handler := NewRequestTraceExportHandler(stub)

	// 形状就不对的游标由 handler 直接拒绝，根本不会触达 service。
	for _, invalid := range []string{
		"cursor=",
		"cursor=%20",
		"cursor=" + strings.Repeat("a", service.RequestTraceExportRecallCursorMaxLength+1),
	} {
		status, body := recallListRequest(t, handler, requestTraceExportRouteBase+"?"+invalid, nil)
		require.Equal(t, http.StatusBadRequest, status, invalid)
		require.Contains(t, string(body), "REQUEST_TRACE_EXPORT_INVALID_CURSOR", invalid)
	}
	require.Zero(t, stub.calls, "an unreadable cursor is refused before the service is asked")

	// 形状合法但含义不可读的游标由 service 判定：handler 只负责把它原样交过去，
	// 并把 service 的结论映射成 400。
	stub.err = service.ErrRequestTraceExportInvalidCursor
	status, body := recallListRequest(t, handler, requestTraceExportRouteBase+"?cursor=v1.not-a-real-token", nil)
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, string(body), "REQUEST_TRACE_EXPORT_INVALID_CURSOR")
	require.Equal(t, []string{"v1.not-a-real-token"}, stub.cursors, "the token is passed through untouched")

	// 合法令牌与合法页大小一样，原样交给 service，由 service 拥有它自己的边界。
	stub.err = nil
	stub.next = "v1.next-page"
	stub.page = []service.RequestTraceExportTask{{
		ID: requestTraceExportTestID, Status: service.RequestTraceExportCompleted, CreatedAt: time.Now().UTC(),
	}}
	status, body = recallListRequest(t, handler, requestTraceExportRouteBase+"?cursor=v1.first-page&limit=5", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"v1.first-page"}, stub.cursors[len(stub.cursors)-1:])
	require.Equal(t, 5, stub.listLimit)
	require.Equal(t, "v1.next-page", recallListNextCursor(t, body))
}

// 找回的响应必须带上**真实**的完整性结论：一页二十行里，一条被上限截断的完成导出
// 不能显示为完成，清单读不到的完成导出也不能。
func TestRequestTraceExportHandlerListCarriesTheCompletenessVerdict(t *testing.T) {
	store := &recallStore{requestTraceExportStoreStub: &requestTraceExportStoreStub{}}
	handler, dir := newRecallHandlerWithDir(t, store)
	now := time.Now().UTC()
	until := now.Add(service.RequestTraceExportDownloadWindow)
	truncatedID := fmt.Sprintf("%032x", 1)
	lostID := fmt.Sprintf("%032x", 2)
	for _, id := range []string{truncatedID, lostID} {
		recallSeedTask(store, service.RequestTraceExportTask{
			ID: id, Status: service.RequestTraceExportCompleted,
			AdminUserID: 12, SessionDigest: recallSessionDigest(recallSessionID), InstanceID: "instance-one",
			CreatedAt: now, CompletedAt: &now, DownloadUntil: &until,
		})
	}
	// 只有第一行有清单：它说这次交付被任务上限截断了。
	manifest, err := json.Marshal(service.RequestTraceExportManifest{
		TaskID: truncatedID, Complete: false, Reason: service.RequestTraceExportIncompleteLimitRows,
		SkippedByReason: service.RequestTraceExportSkipCounts{service.RequestTraceExportIncompleteSourceGone: 3},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, recallManifestFileName(truncatedID)), manifest, 0600))

	status, body := recallListRequest(t, handler, requestTraceExportRouteBase, nil)
	require.Equal(t, http.StatusOK, status)

	var payload struct {
		Data struct {
			Items []struct {
				ID               string           `json:"id"`
				Downloadable     bool             `json:"downloadable"`
				Truncated        bool             `json:"truncated"`
				IncompleteReason *string          `json:"incomplete_reason"`
				SkippedByReason  map[string]int64 `json:"skipped_by_reason"`
				ShardCount       int              `json:"shard_count"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	require.Len(t, payload.Data.Items, 2)
	type verdict struct {
		truncated    bool
		reason       string
		skipped      int64
		downloadable bool
	}
	verdicts := map[string]verdict{}
	for _, item := range payload.Data.Items {
		reason := ""
		if item.IncompleteReason != nil {
			reason = *item.IncompleteReason
		}
		verdicts[item.ID] = verdict{
			truncated:    item.Truncated,
			reason:       reason,
			skipped:      item.SkippedByReason[service.RequestTraceExportIncompleteSourceGone],
			downloadable: item.Downloadable,
		}
		require.Zero(t, item.ShardCount, "the list hands out handles, not shard names")
	}

	require.True(t, verdicts[truncatedID].truncated, "a task cut off by a limit is not a success")
	require.Equal(t, service.RequestTraceExportIncompleteLimitRows, verdicts[truncatedID].reason)
	require.Equal(t, int64(3), verdicts[truncatedID].skipped)
	require.True(t, verdicts[truncatedID].downloadable, "a real truncation still has its shards to download")

	require.True(t, verdicts[lostID].truncated, "a completed row with no readable manifest is not a success either")
	require.Equal(t, service.RequestTraceExportIncompleteManifestLost, verdicts[lostID].reason)
	require.False(t, verdicts[lostID].downloadable, "a delivery whose file is gone is not download-available")
}

// 同一个管理员的另一个会话看到的是空列表：它没有别人的任务可看，也没有被"拒绝"
// ——会话本来就决定了范围。用任务 ID 直接读别人的句柄才是越权，那是 403。
func TestRequestTraceExportHandlerOtherSessionSeesNothingAndCannotReadTheHandle(t *testing.T) {
	store := &recallStore{requestTraceExportStoreStub: &requestTraceExportStoreStub{}}
	handler := newRecallHandler(t, store)
	created := createRequestTraceExportTask(t, handler)

	status, body := recallListRequest(t, handler, requestTraceExportRouteBase, func(c *gin.Context) {
		requestTraceExportAdminSession(c, 12, "session-b")
	})
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, recallListItemIDs(t, body))
	require.Empty(t, recallListNextCursor(t, body))

	c, recorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"/"+created, gin.Params{{Key: "id", Value: created}})
	requestTraceExportAdminSession(c, 12, "session-b")
	handler.Get(c)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Equal(t, "REQUEST_TRACE_EXPORT_FORBIDDEN", requestTraceExportReason(t, recorder))
}

// 管理员 API Key、缺少认证方式、无主体、无绑定会话：一律 403，且都不触达 service。
func TestRequestTraceExportHandlerListRequiresAdminLoginSessionWithoutFallback(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*gin.Context)
	}{
		{
			name: "admin api key is rejected",
			prepare: func(c *gin.Context) {
				c.Set("auth_method", service.AuditAuthMethodAdminAPIKey)
			},
		},
		{
			name: "missing auth method falls back to nothing",
			prepare: func(c *gin.Context) {
				requestTraceExportAdminSession(c, 12, recallSessionID)
				c.Set("auth_method", "")
			},
		},
		{
			name:    "zero subject id is rejected",
			prepare: func(c *gin.Context) { requestTraceExportAdminSession(c, 0, recallSessionID) },
		},
		{
			name: "bound session id is required",
			prepare: func(c *gin.Context) {
				c.Set("auth_method", service.AuditAuthMethodJWT)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 12})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &requestTraceExportServiceStub{}
			handler := NewRequestTraceExportHandler(stub)
			status, body := recallListRequest(t, handler, requestTraceExportRouteBase, tc.prepare)
			require.Equal(t, http.StatusForbidden, status)
			require.Contains(t, string(body), "REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED")
			require.Zero(t, stub.calls, "a rejected caller must never reach the service")
		})
	}

	// 未接线的 service 是"暂时不可用"，不是"你没有权限"。
	handler := NewRequestTraceExportHandler(nil)
	status, body := recallListRequest(t, handler, requestTraceExportRouteBase, nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Contains(t, string(body), "REQUEST_TRACE_EXPORT_UNAVAILABLE")
}

// 非法页大小是调用方的问题（400），不是容量问题（429）：两者会被界面渲染成不同
// 的说法，所以不能混。合法的页大小原样交给 service，由 service 拥有边界。
func TestRequestTraceExportHandlerListValidatesLimitWithoutOwningTheBound(t *testing.T) {
	stub := &recallPagedServiceStub{requestTraceExportServiceStub: &requestTraceExportServiceStub{}}
	handler := NewRequestTraceExportHandler(stub)

	for _, invalid := range []string{"limit=0", "limit=101", "limit=abc", "limit=%20", "limit=-1"} {
		status, body := recallListRequest(t, handler, requestTraceExportRouteBase+"?"+invalid, nil)
		require.Equal(t, http.StatusBadRequest, status, invalid)
		require.Contains(t, string(body), "REQUEST_TRACE_EXPORT_INVALID_FILTER", invalid)
	}
	require.Zero(t, stub.calls, "an invalid limit is refused before the service is asked")

	status, _ := recallListRequest(t, handler, requestTraceExportRouteBase+"?limit=5", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 5, stub.listLimit)

	// 缺席时交给 service 取默认值：handler 不发明一个自己的默认值，也不发明一个游标。
	stub.calls = 0
	status, _ = recallListRequest(t, handler, requestTraceExportRouteBase, nil)
	require.Equal(t, http.StatusOK, status)
	require.Zero(t, stub.listLimit)
	require.Equal(t, "", stub.cursors[len(stub.cursors)-1], "an absent cursor is 'the first page', not an empty token")

	// 服务端的容量错误仍然映射成 429：它确实可能来自真实的容量边界。
	stub.err = service.ErrRequestTraceExportLimit
	status, body := recallListRequest(t, handler, requestTraceExportRouteBase, nil)
	require.Equal(t, http.StatusTooManyRequests, status)
	require.Contains(t, string(body), "REQUEST_TRACE_EXPORT_CAPACITY_LIMIT")

	// 存储故障折叠成 503，且不把原始错误文本带出去。
	stub.err = service.ErrRequestTraceExportUnavailable
	status, body = recallListRequest(t, handler, requestTraceExportRouteBase, nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.NotContains(t, string(body), "synthetic")
}

// 找回的响应不可缓存，且审计只写标量、不回显调用方输入——尤其是游标本身。
func TestRequestTraceExportHandlerListIsUncacheableAndAuditsWithoutContent(t *testing.T) {
	store := &recallStore{requestTraceExportStoreStub: &requestTraceExportStoreStub{}}
	handler := newRecallHandler(t, store)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		recallSeedTask(store, service.RequestTraceExportTask{
			ID: fmt.Sprintf("%032x", i), Status: service.RequestTraceExportCompleted,
			AdminUserID: 12, SessionDigest: recallSessionDigest(recallSessionID), InstanceID: "instance-one",
			CreatedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}

	// 先取一个服务端**真正签出**的游标：断言它不会进审计，而不是断言一个伪造的串。
	_, first := recallListRequest(t, handler, requestTraceExportRouteBase+"?limit=1", nil)
	cursor := recallListNextCursor(t, first)
	require.NotEmpty(t, cursor, "the fixture needs a real cursor to be worth auditing")

	c, recorder := requestTraceExportContext(http.MethodGet, requestTraceExportRouteBase+"?limit=1&cursor="+cursor, nil)
	requestTraceExportAdminSession(c, 12, recallSessionID)
	handler.List(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Header().Get("Cache-Control"), "no-store")

	extras := requestTraceExportAudit(c)
	require.Equal(t, "success", extras["result"])
	// 审计里没有 query 原值（含游标），也没有任何正文。
	require.NotContains(t, strings.ToLower(fmt.Sprint(extras)), "limit")
	require.NotContains(t, fmt.Sprint(extras), cursor)
}

// recallPagedServiceStub 记录 handler 交下来的游标与页大小，并可注入下一页与失败。
// handler 只解析参数，令牌的含义与页边界都由 service 拥有。
type recallPagedServiceStub struct {
	*requestTraceExportServiceStub
	cursors []string
	page    []service.RequestTraceExportTask
	next    string
	err     error
}

func (s *recallPagedServiceStub) ListTasks(_ context.Context, _ service.RequestTraceExportActor, cursor string, limit int) ([]service.RequestTraceExportTask, string, error) {
	s.calls++
	s.listLimit = limit
	s.cursors = append(s.cursors, cursor)
	if s.err != nil {
		return nil, "", s.err
	}
	return s.page, s.next, nil
}
