//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// requestTraceFilterRecorder captures the filter the handler hands to the reader so
// the lookup parameters can be proven to reach the service unchanged.
type requestTraceFilterRecorder struct {
	filter    service.RequestTraceListFilter
	callCount int
}

func (r *requestTraceFilterRecorder) ListRequestTraces(_ context.Context, filter service.RequestTraceListFilter) ([]service.RequestTrace, int64, error) {
	r.filter = filter
	r.callCount++
	return []service.RequestTrace{}, 0, nil
}

func (r *requestTraceFilterRecorder) GetRequestTrace(context.Context, string) (*service.RequestTraceDetail, error) {
	return nil, nil
}

func listTracesWithQuery(t *testing.T, query string) (*httptest.ResponseRecorder, service.RequestTraceListFilter, int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := &requestTraceFilterRecorder{}
	h := NewRequestTraceHandler(recorder)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces?"+query, nil)
	h.List(c)
	return w, recorder.filter, recorder.callCount
}

// account_id 与 usage_log_id 是可选检索条件：出现时必须原样进入服务层筛选，
// 而不是被忽略后返回"全部 Trace"让管理员误以为筛选生效。
func TestRequestTraceAdminListPassesOptionalLookupFilters(t *testing.T) {
	recorder, filter, calls := listTracesWithQuery(t, "account_id=73&usage_log_id=4242")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, calls)
	require.NotNil(t, filter.AccountID)
	require.Equal(t, int64(73), *filter.AccountID)
	require.NotNil(t, filter.UsageLogID)
	require.Equal(t, int64(4242), *filter.UsageLogID)
}

// 缺省时必须保持 nil（"不过滤"），不能把 0 当成"筛选 0 号账号/使用记录"。
func TestRequestTraceAdminListOmitsAbsentLookupFilters(t *testing.T) {
	recorder, filter, calls := listTracesWithQuery(t, "")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, calls)
	require.Nil(t, filter.AccountID)
	require.Nil(t, filter.UsageLogID)
}

// 非法/非正的检索 ID 必须在进服务层之前以 400 拒绝，不静默退化为无筛选。
func TestRequestTraceAdminListRejectsInvalidLookupFilters(t *testing.T) {
	for _, query := range []string{
		"account_id=0", "account_id=-1", "account_id=abc", "account_id=1.5",
		"usage_log_id=0", "usage_log_id=-9", "usage_log_id=notanumber",
	} {
		t.Run(query, func(t *testing.T) {
			recorder, _, calls := listTracesWithQuery(t, query)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Zero(t, calls, "an invalid lookup filter must never reach the reader")
			require.Contains(t, recorder.Body.String(), "REQUEST_TRACE_INVALID_FILTER")
		})
	}
}

// 从使用记录跳转而来的列表页仍是明文读取入口：管理员 API Key 不能用它检索，
// 登录会话门槛与详情一致，导航本身不额外放宽权限。
func TestRequestTraceAdminListRejectsAdminAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &requestTraceFilterRecorder{}
	h := NewRequestTraceHandler(recorder)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("auth_method", service.AuditAuthMethodAdminAPIKey)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/request-traces?usage_log_id=4242", nil)
	h.List(c)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Zero(t, recorder.callCount)
}
