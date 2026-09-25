//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 编译期断言：存储服务必须直接满足读取接缝，接线时不需要适配器。
// 服务方法签名一旦变化，这里会先于接线失败。
var _ ErrorDiagnosticReader = (*service.ErrorDiagnosticService)(nil)

const errorDiagnosticTestID = "0123456789abcdef0123456789abcdef"

// headerValueCanary 是头值哨兵：刻意不含数字 429 这类子串，
// 避免「错误文案里恰好包含它」把「没有回显值」的断言变成假阳性。
const headerValueCanary = "sentinel-header-value-do-not-echo"

var errorDiagnosticTestNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

const (
	errorDiagnosticTestMethod = "/admin/error-diagnostics"
)

type errorDiagnosticReaderStub struct {
	records  []service.ErrorDiagnosticRecord
	listErr  error
	total    int64
	countErr error

	record    service.ErrorDiagnosticRecord
	recordErr error

	body    []byte
	bodyErr error

	// 头值与正文各有独立的替身状态：两者是不同的留存事实，替身混用会让
	// 「正文没留但头值留了」这类正交结论在测试里失去区分度。
	headerValues service.ErrorDiagnosticHeaderValues
	headerErr    error

	listCalls   int
	listLimit   int
	listOffset  int
	listProto   string
	countCalls  int
	getCalls    int
	bodyCalls   int
	headerCalls int
}

func (s *errorDiagnosticReaderStub) GetErrorDiagnostic(context.Context, string) (service.ErrorDiagnosticRecord, error) {
	s.getCalls++
	if s.recordErr != nil {
		return service.ErrorDiagnosticRecord{}, s.recordErr
	}
	return s.record, nil
}

func (s *errorDiagnosticReaderStub) CountRecentErrorDiagnostics(_ context.Context, protocol string) (int64, error) {
	s.countCalls++
	s.listProto = protocol
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.total, nil
}

func (s *errorDiagnosticReaderStub) ListRecentErrorDiagnosticPage(_ context.Context, protocol string, offset, limit int) ([]service.ErrorDiagnosticRecord, error) {
	s.listCalls++
	s.listProto = protocol
	s.listOffset = offset
	s.listLimit = limit
	if s.listErr != nil {
		return nil, s.listErr
	}
	// 与真实读取侧一致：按 OFFSET/LIMIT 切页。
	if offset >= len(s.records) {
		return nil, nil
	}
	end := offset + limit
	if limit <= 0 || end > len(s.records) {
		end = len(s.records)
	}
	return s.records[offset:end], nil
}

func (s *errorDiagnosticReaderStub) ReadErrorDiagnosticBody(context.Context, string) ([]byte, error) {
	s.bodyCalls++
	if s.bodyErr != nil {
		return nil, s.bodyErr
	}
	return s.body, nil
}

func (s *errorDiagnosticReaderStub) ReadErrorDiagnosticHeaderValues(context.Context, string) (service.ErrorDiagnosticHeaderValues, error) {
	s.headerCalls++
	if s.headerErr != nil {
		return service.ErrorDiagnosticHeaderValues{}, s.headerErr
	}
	return s.headerValues, nil
}

func errorDiagnosticRecordFixture() service.ErrorDiagnosticRecord {
	return service.ErrorDiagnosticRecord{
		ID:                 errorDiagnosticTestID,
		UsageLogID:         77,
		HasUsage:           true,
		Protocol:           service.ErrorDiagnosticProtocolMessages,
		AttemptIndex:       1,
		Stage:              service.ErrorDiagnosticStageWire,
		UpstreamStatusCode: 429,
		BodyState:          service.ErrorDiagnosticBodyStateStored,
		BodyReason:         service.ErrorDiagnosticBodyRetained,
		CreatedAt:          errorDiagnosticTestNow.Add(-time.Hour),
		MetadataExpiresAt:  errorDiagnosticTestNow.Add(30 * 24 * time.Hour),
		BodyExpiresAt:      errorDiagnosticTestNow.Add(7 * 24 * time.Hour),
		BodyBytes:          42,
		BodyKeyVersion:     1,
		BodyStored:         true,

		HeaderState:      service.ErrorDiagnosticHeaderStateStored,
		HeaderReason:     service.ErrorDiagnosticHeaderRetained,
		HeaderEntryCount: 2,
		HeaderBytes:      96,
		HeaderKeyVersion: 1,
		HeaderStored:     true,
		HeaderExpiresAt:  errorDiagnosticTestNow.Add(7 * 24 * time.Hour),
	}
}

func newErrorDiagnosticTestRouter(reader ErrorDiagnosticReader) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewRequestErrorDiagnosticHandler(reader)
	handler.now = func() time.Time { return errorDiagnosticTestNow }
	router.GET(errorDiagnosticTestMethod, handler.List)
	router.GET(errorDiagnosticTestMethod+"/:id", handler.Get)
	router.POST(errorDiagnosticTestMethod+"/:id/body", handler.RevealBody)
	router.POST(errorDiagnosticTestMethod+"/:id/headers", handler.RevealHeaderValues)
	return router
}

type errorDiagnosticListEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Items    []map[string]json.RawMessage `json:"items"`
		Total    int64                        `json:"total"`
		Page     int                          `json:"page"`
		PageSize int                          `json:"page_size"`
		Pages    int                          `json:"pages"`
	} `json:"data"`
}

type errorDiagnosticErrorEnvelope struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func decodeErrorDiagnosticList(t *testing.T, body []byte) errorDiagnosticListEnvelope {
	t.Helper()
	var envelope errorDiagnosticListEnvelope
	require.NoError(t, json.Unmarshal(body, &envelope))
	return envelope
}

func decodeErrorDiagnosticError(t *testing.T, body []byte) errorDiagnosticErrorEnvelope {
	t.Helper()
	var envelope errorDiagnosticErrorEnvelope
	require.NoError(t, json.Unmarshal(body, &envelope))
	return envelope
}

func serveErrorDiagnostic(t *testing.T, router *gin.Engine, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func requireErrorDiagnosticKeys(t *testing.T, item map[string]json.RawMessage, want ...string) {
	t.Helper()
	keys := make([]string, 0, len(item))
	for key := range item {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	require.Equal(t, sorted, keys, "disclosed fields must be exactly the allowlist")
}

// 列表只披露白名单字段：没有正文字段，也没有账号／用户／Key／模型等身份字段。
func TestErrorDiagnosticsListDisclosesOnlyAllowlistedFields(t *testing.T) {
	stub := &errorDiagnosticReaderStub{records: []service.ErrorDiagnosticRecord{errorDiagnosticRecordFixture()}}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod)
	require.Equal(t, http.StatusOK, recorder.Code)

	envelope := decodeErrorDiagnosticList(t, recorder.Body.Bytes())
	require.Len(t, envelope.Data.Items, 1)
	requireErrorDiagnosticKeys(t, envelope.Data.Items[0],
		"id", "created_at", "protocol", "attempt_index", "upstream_status",
		"usage_log_id", "body_state", "reason", "body_expires_at", "metadata_expires_at",
		"header_state", "header_reason", "header_entry_count", "header_expires_at")

	// 正文字段、头值内容字段与常见身份字段一律不得出现。
	for _, forbidden := range []string{"body_text", "body_bytes", "account_id", "user_id", "api_key_id", "model", "headers",
		"request_headers", "response_headers", "header_text", "header_ciphertext"} {
		require.NotContains(t, recorder.Body.String(), `"`+forbidden+`"`)
	}
}

// usage_log_id 只由 HasUsage 决定：UsageLogID 非零但 HasUsage 为假时不得披露。
// BodyExpiresAt 为零值时该字段整个省略。
func TestErrorDiagnosticsListOmitsUsageAndBodyExpiryWhenAbsent(t *testing.T) {
	record := errorDiagnosticRecordFixture()
	record.HasUsage = false
	record.UsageLogID = 77
	record.BodyExpiresAt = time.Time{}
	record.BodyState = service.ErrorDiagnosticBodyStateNotObserved
	record.BodyReason = service.ErrorDiagnosticBodyNotObserved
	record.BodyStored = false
	// 头值同理：没留过头值时到期字段整个省略，但状态／原因／条数仍然披露
	// （否则运维无法区分「没有头值」与「接口不认识这个事实」）。
	record.HeaderExpiresAt = time.Time{}
	record.HeaderStored = false
	record.HeaderState = service.ErrorDiagnosticHeaderStateNotObserved
	record.HeaderReason = service.ErrorDiagnosticHeaderNotObserved
	record.HeaderEntryCount = 0
	stub := &errorDiagnosticReaderStub{records: []service.ErrorDiagnosticRecord{record}}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod)
	require.Equal(t, http.StatusOK, recorder.Code)

	envelope := decodeErrorDiagnosticList(t, recorder.Body.Bytes())
	require.Len(t, envelope.Data.Items, 1)
	requireErrorDiagnosticKeys(t, envelope.Data.Items[0],
		"id", "created_at", "protocol", "attempt_index", "upstream_status",
		"body_state", "reason", "metadata_expires_at",
		"header_state", "header_reason", "header_entry_count")
}

// 已过 30 天到期的元数据即使仍被存储层返回，也不得披露。
func TestErrorDiagnosticsListDropsExpiredMetadata(t *testing.T) {
	expired := errorDiagnosticRecordFixture()
	expired.MetadataExpiresAt = errorDiagnosticTestNow.Add(-time.Minute)
	stub := &errorDiagnosticReaderStub{records: []service.ErrorDiagnosticRecord{expired}}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod)
	require.Equal(t, http.StatusOK, recorder.Code)

	envelope := decodeErrorDiagnosticList(t, recorder.Body.Bytes())
	require.Empty(t, envelope.Data.Items)
}

// 枚举只回声存储层封闭集合：未知的 body_state／body_reason 归一到最保守的 not_observed；
// 协议不在已覆盖的三个分支内时整条不披露。
func TestErrorDiagnosticsListNormalizesUnknownEnums(t *testing.T) {
	record := errorDiagnosticRecordFixture()
	record.BodyState = "weird_state"
	record.BodyReason = "weird_reason"
	unknownProtocol := errorDiagnosticRecordFixture()
	unknownProtocol.ID = "ffffffffffffffffffffffffffffffff"
	unknownProtocol.Protocol = "anthropic.messages"
	unknownProtocol.BodyExpiresAt = time.Time{}

	stub := &errorDiagnosticReaderStub{records: []service.ErrorDiagnosticRecord{record, unknownProtocol}}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod)
	require.Equal(t, http.StatusOK, recorder.Code)

	envelope := decodeErrorDiagnosticList(t, recorder.Body.Bytes())
	require.Len(t, envelope.Data.Items, 1)

	var bodyState, reason string
	require.NoError(t, json.Unmarshal(envelope.Data.Items[0]["body_state"], &bodyState))
	require.NoError(t, json.Unmarshal(envelope.Data.Items[0]["reason"], &reason))
	require.Equal(t, service.ErrorDiagnosticBodyStateNotObserved, bodyState)
	require.Equal(t, service.ErrorDiagnosticBodyNotObserved, reason)
	require.NotContains(t, recorder.Body.String(), "weird_state")
	require.NotContains(t, recorder.Body.String(), "weird_reason")
}

// 不提供账号／用户／Key／模型等身份过滤：这些参数必须被忽略，且协议过滤不被下推。
func TestErrorDiagnosticsListIgnoresIdentityFilters(t *testing.T) {
	stub := &errorDiagnosticReaderStub{records: []service.ErrorDiagnosticRecord{errorDiagnosticRecordFixture()}}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet,
		errorDiagnosticTestMethod+"?page=1&page_size=5&account_id=9&user_id=8&api_key_id=7&model=gpt&usage_log_id=6&protocol=messages")
	require.Equal(t, http.StatusOK, recorder.Code)

	require.Equal(t, 1, stub.listCalls)
	require.Equal(t, "", stub.listProto, "no caller-supplied protocol filter may reach storage")
	require.Equal(t, 5, stub.listLimit)
	require.Equal(t, 0, stub.listOffset)

	envelope := decodeErrorDiagnosticList(t, recorder.Body.Bytes())
	require.Len(t, envelope.Data.Items, 1)
}

func TestErrorDiagnosticsListPagesByOffset(t *testing.T) {
	first := errorDiagnosticRecordFixture()
	first.ID = "11111111111111111111111111111111"
	second := errorDiagnosticRecordFixture()
	second.ID = "22222222222222222222222222222222"
	third := errorDiagnosticRecordFixture()
	third.ID = "33333333333333333333333333333333"
	stub := &errorDiagnosticReaderStub{records: []service.ErrorDiagnosticRecord{first, second, third}, total: 3}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod+"?page=2&page_size=1")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, stub.listOffset, "second page must be fetched by SQL offset")
	require.Equal(t, 1, stub.listLimit)

	envelope := decodeErrorDiagnosticList(t, recorder.Body.Bytes())
	require.Len(t, envelope.Data.Items, 1)
	require.Equal(t, 2, envelope.Data.Page)
	require.Equal(t, 1, envelope.Data.PageSize)
	require.EqualValues(t, 3, envelope.Data.Total)

	var id string
	require.NoError(t, json.Unmarshal(envelope.Data.Items[0]["id"], &id))
	require.Equal(t, second.ID, id)
}

// 超过 offset 上界的页码必须显式拒绝：存储层在上界之上会夹取到「最后一页」，
// 那会把另一页的数据当成这一页渲染。空页同样会撒谎，所以两者都不用。
func TestErrorDiagnosticsListBeyondOffsetBoundIsRejected(t *testing.T) {
	stub := &errorDiagnosticReaderStub{records: []service.ErrorDiagnosticRecord{errorDiagnosticRecordFixture()}, total: 5000}
	router := newErrorDiagnosticTestRouter(stub)

	for _, query := range []string{
		"?page=1002&page_size=100",               // offset 100_100，刚过上界
		"?page=9223372036854775807&page_size=20", // 极大页码：不得因溢出变成合法偏移
	} {
		recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod+query)
		require.Equal(t, http.StatusBadRequest, recorder.Code, query)
		require.Equal(t, "ERROR_DIAGNOSTIC_PAGE_OUT_OF_RANGE", decodeErrorDiagnosticError(t, recorder.Body.Bytes()).Reason, query)
	}
	require.Zero(t, stub.listCalls, "an out-of-range page must not be fetched at all")
	require.Zero(t, stub.countCalls, "reject before spending a count query")
}

// offset 上界本身仍然可取：它是保护值，不是可见窗口。
func TestErrorDiagnosticsListFetchesPageAtOffsetBound(t *testing.T) {
	stub := &errorDiagnosticReaderStub{total: 5000}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod+"?page=1001&page_size=100")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, service.ErrorDiagnosticMaxListOffset, stub.listOffset)
	require.Equal(t, 100, stub.listLimit)
}

// 第 200 行之后的分页必须如实按 SQL OFFSET 取回：逐页走完，每页与预期切片一致，
// 且跨页无重复 id、不漏行。
func TestErrorDiagnosticsListPagesBeyondTwoHundred(t *testing.T) {
	const rows = 305
	records := make([]service.ErrorDiagnosticRecord, 0, rows)
	for i := 0; i < rows; i++ {
		record := errorDiagnosticRecordFixture()
		record.ID = fmt.Sprintf("%032x", i)
		records = append(records, record)
	}
	stub := &errorDiagnosticReaderStub{records: records, total: rows}
	router := newErrorDiagnosticTestRouter(stub)

	seen := make(map[string]bool, rows)
	for page := 1; page <= 4; page++ {
		recorder := serveErrorDiagnostic(t, router, http.MethodGet,
			fmt.Sprintf("%s?page=%d&page_size=100", errorDiagnosticTestMethod, page))
		require.Equal(t, http.StatusOK, recorder.Code, "page %d", page)
		require.Equal(t, (page-1)*100, stub.listOffset, "page %d must be fetched by its own offset", page)

		envelope := decodeErrorDiagnosticList(t, recorder.Body.Bytes())
		require.EqualValues(t, rows, envelope.Data.Total, "page %d", page)

		want := 100
		if remaining := rows - (page-1)*100; remaining < want {
			want = remaining
		}
		require.Len(t, envelope.Data.Items, want, "page %d", page)

		for _, item := range envelope.Data.Items {
			var id string
			require.NoError(t, json.Unmarshal(item["id"], &id))
			require.False(t, seen[id], "id %s repeated across pages", id)
			seen[id] = true
		}
	}
	require.Len(t, seen, rows, "the traversal must cover every row exactly once")
}

// 响应形状与前端 PaginatedResponse 对齐，total 取真实计数而不是本页长度。
func TestErrorDiagnosticsListReportsCountedTotalNotPageLength(t *testing.T) {
	first := errorDiagnosticRecordFixture()
	second := errorDiagnosticRecordFixture()
	second.ID = "22222222222222222222222222222222"
	stub := &errorDiagnosticReaderStub{records: []service.ErrorDiagnosticRecord{first, second}, total: 37}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod+"?page=1&page_size=1")
	require.Equal(t, http.StatusOK, recorder.Code)

	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	for _, key := range []string{"items", "total", "page", "page_size", "pages"} {
		require.Contains(t, envelope.Data, key, "PaginatedResponse field must always be present")
	}

	list := decodeErrorDiagnosticList(t, recorder.Body.Bytes())
	require.Len(t, list.Data.Items, 1)
	require.EqualValues(t, 37, list.Data.Total, "total is the counted number, never the page length")
	require.Equal(t, 37, list.Data.Pages)
}

// 计数失败必须是真实错误，不能被渲染成 total=0（0 表示「没有可读记录」，不是「未知」）。
func TestErrorDiagnosticsListCountFailureIsNotRenderedAsZero(t *testing.T) {
	for _, tc := range []struct {
		name     string
		countErr error
		wantCode int
		wantReas string
	}{
		{name: "unavailable", countErr: service.ErrErrorDiagnosticUnavailable, wantCode: http.StatusServiceUnavailable, wantReas: "ERROR_DIAGNOSTIC_UNAVAILABLE"},
		{name: "unknown", countErr: errors.New("count canary"), wantCode: http.StatusInternalServerError, wantReas: "ERROR_DIAGNOSTIC_STORAGE_FAILED"},
	} {
		stub := &errorDiagnosticReaderStub{records: []service.ErrorDiagnosticRecord{errorDiagnosticRecordFixture()}, countErr: tc.countErr}
		router := newErrorDiagnosticTestRouter(stub)

		recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod)
		require.Equal(t, tc.wantCode, recorder.Code, tc.name)
		require.Equal(t, tc.wantReas, decodeErrorDiagnosticError(t, recorder.Body.Bytes()).Reason, tc.name)
		require.Zero(t, stub.listCalls, tc.name)
		require.NotContains(t, recorder.Body.String(), "count canary")
	}
}

// 列表与详情同样不得被中间缓存。
func TestErrorDiagnosticsListAndDetailAreNotCacheable(t *testing.T) {
	stub := &errorDiagnosticReaderStub{
		records: []service.ErrorDiagnosticRecord{errorDiagnosticRecordFixture()},
		record:  errorDiagnosticRecordFixture(),
	}
	router := newErrorDiagnosticTestRouter(stub)

	for _, path := range []string{errorDiagnosticTestMethod, errorDiagnosticTestMethod + "/" + errorDiagnosticTestID} {
		recorder := serveErrorDiagnostic(t, router, http.MethodGet, path)
		require.Equal(t, http.StatusOK, recorder.Code, path)
		require.Contains(t, recorder.Header().Get("Cache-Control"), "no-store", path)
	}
}

func TestErrorDiagnosticsGetReturnsMetadataOnly(t *testing.T) {
	stub := &errorDiagnosticReaderStub{record: errorDiagnosticRecordFixture()}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID)
	require.Equal(t, http.StatusOK, recorder.Code)

	var envelope struct {
		Code int                        `json:"code"`
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	requireErrorDiagnosticKeys(t, envelope.Data,
		"id", "created_at", "protocol", "attempt_index", "upstream_status",
		"usage_log_id", "body_state", "reason", "body_expires_at", "metadata_expires_at",
		"header_state", "header_reason", "header_entry_count", "header_expires_at")
	require.NotContains(t, recorder.Body.String(), `"body_text"`)
	// 详情同样只披露头值状态，绝不带出任何头值内容。
	require.NotContains(t, recorder.Body.String(), `"request_headers"`)
	require.NotContains(t, recorder.Body.String(), `"response_headers"`)
}

func TestErrorDiagnosticsGetUnknownIDIsNotFound(t *testing.T) {
	stub := &errorDiagnosticReaderStub{recordErr: service.ErrErrorDiagnosticNotFound}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Equal(t, "ERROR_DIAGNOSTIC_NOT_FOUND", decodeErrorDiagnosticError(t, recorder.Body.Bytes()).Reason)
}

// 形状非法的 id 在 handler 内就被拒绝，不进入存储层。
func TestErrorDiagnosticsMalformedIDNeverReachesReader(t *testing.T) {
	stub := &errorDiagnosticReaderStub{record: errorDiagnosticRecordFixture()}
	router := newErrorDiagnosticTestRouter(stub)

	for _, path := range []string{
		errorDiagnosticTestMethod + "/short",
		errorDiagnosticTestMethod + "/0123456789ABCDEF0123456789ABCDEF",
		errorDiagnosticTestMethod + "/0123456789abcdef0123456789abcde-",
	} {
		recorder := serveErrorDiagnostic(t, router, http.MethodGet, path)
		require.Equal(t, http.StatusNotFound, recorder.Code, path)
	}
	require.Zero(t, stub.getCalls)
	require.Zero(t, stub.bodyCalls)
}

// 存储层返回的已到期元数据不得披露。
func TestErrorDiagnosticsGetExpiredMetadataIsNotFound(t *testing.T) {
	record := errorDiagnosticRecordFixture()
	record.MetadataExpiresAt = errorDiagnosticTestNow.Add(-time.Second)
	stub := &errorDiagnosticReaderStub{record: record}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID)
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestErrorDiagnosticsRevealBodyReturnsRetainedText(t *testing.T) {
	canary := `{"messages":[{"role":"user","content":"diagnostic-canary"}]}`
	stub := &errorDiagnosticReaderStub{record: errorDiagnosticRecordFixture(), body: []byte(canary)}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, stub.bodyCalls)
	require.Contains(t, recorder.Header().Get("Cache-Control"), "no-store")
	require.Equal(t, "no-cache", recorder.Header().Get("Pragma"))
	require.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			BodyText  string `json:"body_text"`
			BodyBytes int    `json:"body_bytes"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, canary, envelope.Data.BodyText)
	require.Equal(t, len(canary), envelope.Data.BodyBytes)
}

// 未留存正文（skipped／not_observed）是稳定的 409，并且不触发解密读取。
func TestErrorDiagnosticsRevealBodyNotRetainedIsConflict(t *testing.T) {
	for _, state := range []string{service.ErrorDiagnosticBodyStateSkipped, service.ErrorDiagnosticBodyStateNotObserved} {
		record := errorDiagnosticRecordFixture()
		record.BodyState = state
		record.BodyReason = service.ErrorDiagnosticBodySkippedAttachment
		record.BodyExpiresAt = time.Time{}
		record.BodyStored = false
		stub := &errorDiagnosticReaderStub{record: record}
		router := newErrorDiagnosticTestRouter(stub)

		recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body")
		require.Equal(t, http.StatusConflict, recorder.Code, state)
		require.Equal(t, "ERROR_DIAGNOSTIC_BODY_NOT_RETAINED", decodeErrorDiagnosticError(t, recorder.Body.Bytes()).Reason)
		require.Zero(t, stub.bodyCalls, "skipped bodies must not be read or decrypted")
		require.Contains(t, recorder.Header().Get("Cache-Control"), "no-store")
	}
}

// 曾留存但已到期／已被清理是 410。
func TestErrorDiagnosticsRevealBodyExpiredOrPurgedIsGone(t *testing.T) {
	for _, state := range []string{service.ErrorDiagnosticBodyStateExpired, service.ErrorDiagnosticBodyStatePurged} {
		record := errorDiagnosticRecordFixture()
		record.BodyState = state
		record.BodyReason = service.ErrorDiagnosticBodyRetained
		// expired：密文仍在但已过第 7 天；purged：密文已被清理。
		record.BodyStored = state == service.ErrorDiagnosticBodyStateExpired
		record.BodyExpiresAt = errorDiagnosticTestNow.Add(-time.Hour)
		stub := &errorDiagnosticReaderStub{record: record}
		router := newErrorDiagnosticTestRouter(stub)

		recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body")
		require.Equal(t, http.StatusGone, recorder.Code, state)
		require.Equal(t, "ERROR_DIAGNOSTIC_BODY_GONE", decodeErrorDiagnosticError(t, recorder.Body.Bytes()).Reason)
		require.Zero(t, stub.bodyCalls)
	}
}

// 状态仍为 stored 但已过第 7 天：handler 侧先拒绝，不进入解密。
func TestErrorDiagnosticsRevealBodyPastBodyTTLIsGone(t *testing.T) {
	record := errorDiagnosticRecordFixture()
	record.BodyExpiresAt = errorDiagnosticTestNow.Add(-time.Second)
	stub := &errorDiagnosticReaderStub{record: record, body: []byte("canary")}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body")
	require.Equal(t, http.StatusGone, recorder.Code)
	require.Zero(t, stub.bodyCalls)
	require.NotContains(t, recorder.Body.String(), "canary")
}

// 读取期的 gone（到期／密文已清除）与存储不可用必须分开：410 与 503。
func TestErrorDiagnosticsRevealBodyReadGoneIsGone(t *testing.T) {
	stub := &errorDiagnosticReaderStub{record: errorDiagnosticRecordFixture(), bodyErr: service.ErrErrorDiagnosticBodyGone}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body")
	require.Equal(t, http.StatusGone, recorder.Code)
}

func TestErrorDiagnosticsRevealBodyReadUnavailableIsUnavailable(t *testing.T) {
	stub := &errorDiagnosticReaderStub{record: errorDiagnosticRecordFixture(), bodyErr: service.ErrErrorDiagnosticUnavailable}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestErrorDiagnosticsStorageUnavailableIs503(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stub     *errorDiagnosticReaderStub
		method   string
		path     string
		wantCode int
	}{
		{name: "list", stub: &errorDiagnosticReaderStub{listErr: service.ErrErrorDiagnosticUnavailable}, method: http.MethodGet, path: errorDiagnosticTestMethod},
		{name: "detail", stub: &errorDiagnosticReaderStub{recordErr: service.ErrErrorDiagnosticUnavailable}, method: http.MethodGet, path: errorDiagnosticTestMethod + "/" + errorDiagnosticTestID},
		{name: "body", stub: &errorDiagnosticReaderStub{recordErr: service.ErrErrorDiagnosticUnavailable}, method: http.MethodPost, path: errorDiagnosticTestMethod + "/" + errorDiagnosticTestID + "/body"},
		{name: "headers", stub: &errorDiagnosticReaderStub{recordErr: service.ErrErrorDiagnosticUnavailable}, method: http.MethodPost, path: errorDiagnosticTestMethod + "/" + errorDiagnosticTestID + "/headers"},
	} {
		router := newErrorDiagnosticTestRouter(tc.stub)
		recorder := serveErrorDiagnostic(t, router, tc.method, tc.path)
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, tc.name)
		require.Equal(t, "ERROR_DIAGNOSTIC_UNAVAILABLE", decodeErrorDiagnosticError(t, recorder.Body.Bytes()).Reason, tc.name)
	}
}

// 未知存储错误按 500 处理，且不得回显内部错误文本。
func TestErrorDiagnosticsUnknownStorageErrorIsInternal(t *testing.T) {
	stub := &errorDiagnosticReaderStub{listErr: errors.New("storage stack trace canary")}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "storage stack trace canary")
}

func TestErrorDiagnosticsNilReaderIsUnavailable(t *testing.T) {
	router := newErrorDiagnosticTestRouter(nil)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: errorDiagnosticTestMethod},
		{method: http.MethodGet, path: errorDiagnosticTestMethod + "/" + errorDiagnosticTestID},
		{method: http.MethodPost, path: errorDiagnosticTestMethod + "/" + errorDiagnosticTestID + "/body"},
		{method: http.MethodPost, path: errorDiagnosticTestMethod + "/" + errorDiagnosticTestID + "/headers"},
	} {
		recorder := serveErrorDiagnostic(t, router, tc.method, tc.path)
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, tc.path)
	}
}

// 显式揭示的响应在成功与失败路径上都必须禁止缓存。
func TestErrorDiagnosticsRevealBodyAlwaysNoStore(t *testing.T) {
	stub := &errorDiagnosticReaderStub{recordErr: service.ErrErrorDiagnosticNotFound}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body")
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Header().Get("Cache-Control"), "no-store")
}

// 429 头值的显式揭示：成功路径返回两个方向的净化值、条目数、载荷字节与到期时刻，
// 且与正文同样禁止任何中间缓存。
func TestErrorDiagnosticsRevealHeaderValuesReturnsSanitizedValues(t *testing.T) {
	stub := &errorDiagnosticReaderStub{
		record: errorDiagnosticRecordFixture(),
		headerValues: service.ErrorDiagnosticHeaderValues{
			Request:  map[string]string{"Anthropic-Version": "2023-06-01", "User-Agent": "claude-cli/2.1.78"},
			Response: map[string]string{"Retry-After": "42", "Anthropic-Ratelimit-Requests-Remaining": "0"},
		},
	}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/headers")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, stub.headerCalls)
	// 揭示头值不得顺带读取正文。
	require.Zero(t, stub.bodyCalls)

	require.Contains(t, recorder.Header().Get("Cache-Control"), "no-store")
	require.Equal(t, "no-cache", recorder.Header().Get("Pragma"))
	require.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			RequestHeaders   map[string]string `json:"request_headers"`
			ResponseHeaders  map[string]string `json:"response_headers"`
			HeaderEntryCount int               `json:"header_entry_count"`
			HeaderBytes      int               `json:"header_bytes"`
			HeaderExpiresAt  time.Time         `json:"header_expires_at"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, "2023-06-01", envelope.Data.RequestHeaders["Anthropic-Version"])
	require.Equal(t, "42", envelope.Data.ResponseHeaders["Retry-After"])
	require.Equal(t, 4, envelope.Data.HeaderEntryCount)
	require.Equal(t, 96, envelope.Data.HeaderBytes)
	// 到期时刻必须显式披露，运维才能判断还剩多久可读。
	require.True(t, envelope.Data.HeaderExpiresAt.After(errorDiagnosticTestNow))
}

// 未留存头值（skipped／not_observed）是稳定的 409，并且不触发解密读取。
func TestErrorDiagnosticsRevealHeaderValuesNotRetainedIsConflict(t *testing.T) {
	for _, state := range []string{service.ErrorDiagnosticHeaderStateSkipped, service.ErrorDiagnosticHeaderStateNotObserved} {
		record := errorDiagnosticRecordFixture()
		record.HeaderState = state
		record.HeaderReason = service.ErrorDiagnosticHeaderSkippedInvalidValues
		record.HeaderExpiresAt = time.Time{}
		record.HeaderStored = false
		stub := &errorDiagnosticReaderStub{record: record}
		router := newErrorDiagnosticTestRouter(stub)

		recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/headers")
		require.Equal(t, http.StatusConflict, recorder.Code, state)
		require.Equal(t, "ERROR_DIAGNOSTIC_HEADER_VALUES_NOT_RETAINED", decodeErrorDiagnosticError(t, recorder.Body.Bytes()).Reason)
		require.Zero(t, stub.headerCalls, "skipped header values must not be read or decrypted")
		require.Contains(t, recorder.Header().Get("Cache-Control"), "no-store")
	}
}

// 曾留存但已到期／已被清理是 410；状态仍为 stored 但已过第 7 天同样先拒绝，不进入解密。
func TestErrorDiagnosticsRevealHeaderValuesExpiredOrPurgedIsGone(t *testing.T) {
	for _, state := range []string{service.ErrorDiagnosticHeaderStateExpired, service.ErrorDiagnosticHeaderStatePurged} {
		record := errorDiagnosticRecordFixture()
		record.HeaderState = state
		record.HeaderStored = state == service.ErrorDiagnosticHeaderStateExpired
		record.HeaderExpiresAt = errorDiagnosticTestNow.Add(-time.Hour)
		stub := &errorDiagnosticReaderStub{record: record}
		router := newErrorDiagnosticTestRouter(stub)

		recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/headers")
		require.Equal(t, http.StatusGone, recorder.Code, state)
		require.Equal(t, "ERROR_DIAGNOSTIC_HEADER_VALUES_GONE", decodeErrorDiagnosticError(t, recorder.Body.Bytes()).Reason)
		require.Zero(t, stub.headerCalls)
	}

	record := errorDiagnosticRecordFixture()
	record.HeaderExpiresAt = errorDiagnosticTestNow.Add(-time.Second)
	stub := &errorDiagnosticReaderStub{record: record, headerValues: service.ErrorDiagnosticHeaderValues{
		Response: map[string]string{"Retry-After": headerValueCanary},
	}}
	router := newErrorDiagnosticTestRouter(stub)
	recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/headers")
	require.Equal(t, http.StatusGone, recorder.Code)
	require.Zero(t, stub.headerCalls)
	// 用不会与状态码文本（例如 429）撞车的哨兵，证明已过期时一个值都不回显。
	require.NotContains(t, recorder.Body.String(), headerValueCanary)
}

// 读取期的 gone 与存储不可用必须分开：410 与 503。
func TestErrorDiagnosticsRevealHeaderValuesReadFailureMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantReason string
	}{
		{name: "gone", err: service.ErrErrorDiagnosticHeaderValuesGone, wantStatus: http.StatusGone, wantReason: "ERROR_DIAGNOSTIC_HEADER_VALUES_GONE"},
		{name: "unavailable", err: service.ErrErrorDiagnosticUnavailable, wantStatus: http.StatusServiceUnavailable, wantReason: "ERROR_DIAGNOSTIC_UNAVAILABLE"},
	} {
		stub := &errorDiagnosticReaderStub{record: errorDiagnosticRecordFixture(), headerErr: tc.err}
		router := newErrorDiagnosticTestRouter(stub)

		recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/headers")
		require.Equal(t, tc.wantStatus, recorder.Code, tc.name)
		require.Equal(t, tc.wantReason, decodeErrorDiagnosticError(t, recorder.Body.Bytes()).Reason, tc.name)
	}
}

// 未知头值状态按最保守的「没有可揭示的头值」处理，绝不声称存在头值。
func TestErrorDiagnosticsRevealHeaderValuesUnknownStateIsConflict(t *testing.T) {
	record := errorDiagnosticRecordFixture()
	record.HeaderState = "something_unknown"
	stub := &errorDiagnosticReaderStub{record: record}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/headers")
	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Zero(t, stub.headerCalls)
}

// 形状非法的 id 在 handler 内就被拒绝，不进入存储层。
func TestErrorDiagnosticsRevealHeaderValuesMalformedIDNeverReachesReader(t *testing.T) {
	stub := &errorDiagnosticReaderStub{record: errorDiagnosticRecordFixture()}
	router := newErrorDiagnosticTestRouter(stub)

	recorder := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/short/headers")
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Zero(t, stub.getCalls)
	require.Zero(t, stub.headerCalls)
}

// 两条揭示路径互不影响：揭示正文不读头值，揭示头值不读正文。
func TestErrorDiagnosticsBodyAndHeaderRevealsAreIndependent(t *testing.T) {
	stub := &errorDiagnosticReaderStub{
		record:       errorDiagnosticRecordFixture(),
		body:         []byte(`{"canary":true}`),
		headerValues: service.ErrorDiagnosticHeaderValues{Response: map[string]string{"Retry-After": "42"}},
	}
	router := newErrorDiagnosticTestRouter(stub)

	body := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/body")
	require.Equal(t, http.StatusOK, body.Code)
	require.Equal(t, 1, stub.bodyCalls)
	require.Zero(t, stub.headerCalls)

	headers := serveErrorDiagnostic(t, router, http.MethodPost, errorDiagnosticTestMethod+"/"+errorDiagnosticTestID+"/headers")
	require.Equal(t, http.StatusOK, headers.Code)
	require.Equal(t, 1, stub.headerCalls)
	require.Equal(t, 1, stub.bodyCalls)
}

// page/page_size 折算出的 offset/limit 必须落在读取侧窗口内。
func TestErrorDiagnosticsListPagingStaysWithinServiceWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		query      string
		wantOffset int
		wantLimit  int
		wantCalls  int
	}{
		{name: "oversized page size is clamped", query: "?page=1&page_size=1000", wantOffset: 0, wantLimit: 100, wantCalls: 1},
		{name: "second page uses sql offset", query: "?page=2&page_size=100", wantOffset: 100, wantLimit: 100, wantCalls: 1},
	} {
		stub := &errorDiagnosticReaderStub{}
		router := newErrorDiagnosticTestRouter(stub)

		recorder := serveErrorDiagnostic(t, router, http.MethodGet, errorDiagnosticTestMethod+tc.query)
		require.Equal(t, http.StatusOK, recorder.Code, tc.name)
		require.Equal(t, tc.wantCalls, stub.listCalls, tc.name)
		if tc.wantCalls > 0 {
			require.Equal(t, tc.wantOffset, stub.listOffset, tc.name)
			require.Equal(t, tc.wantLimit, stub.listLimit, tc.name)
		}
	}
}
