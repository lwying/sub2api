package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// usageLogIDLookupRepo 记录 List 处理器交给仓储的筛选与调用次数，用来证明
// usage_log_id 原样到达仓储，且非法取值根本不会到达仓储。
type usageLogIDLookupRepo struct {
	service.UsageLogRepository
	listCalls  int
	listFilter usagestats.UsageLogFilters
}

func (r *usageLogIDLookupRepo) ListWithFilters(_ context.Context, params pagination.PaginationParams, filters usagestats.UsageLogFilters) ([]service.UsageLog, *pagination.PaginationResult, error) {
	r.listCalls++
	r.listFilter = filters
	return []service.UsageLog{}, &pagination.PaginationResult{Page: params.Page, PageSize: params.PageSize}, nil
}

func newUsageLogIDLookupRouter(repo *usageLogIDLookupRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewUsageHandler(service.NewUsageService(repo, nil, nil, nil), nil, nil, nil, nil)
	router := gin.New()
	router.GET("/admin/usage", handler.List)
	return router
}

func listUsageWithQuery(t *testing.T, query string, repo *usageLogIDLookupRepo) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	newUsageLogIDLookupRouter(repo).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/usage?"+query, nil))
	return recorder
}

// usage_log_id 是从请求 Trace 跳转过来的精确记录定位：出现时必须原样进入列表筛选，
// 而不是被忽略后返回"全部使用记录"让管理员以为定位到了那一条。
func TestAdminUsageListPassesExactUsageLogID(t *testing.T) {
	repo := &usageLogIDLookupRepo{}
	recorder := listUsageWithQuery(t, "usage_log_id=4242", repo)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, repo.listCalls)
	require.Equal(t, int64(4242), repo.listFilter.UsageLogID)
}

// 缺省时必须保持"不过滤"（0），不能把 0 当成"筛选 0 号使用记录"。
func TestAdminUsageListOmitsAbsentUsageLogID(t *testing.T) {
	repo := &usageLogIDLookupRepo{}
	recorder := listUsageWithQuery(t, "", repo)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, repo.listCalls)
	require.Zero(t, repo.listFilter.UsageLogID)
}

// 非法或非正的记录 ID 必须在进仓储之前以 400 拒绝，不静默退化成无筛选的整表列表。
func TestAdminUsageListRejectsInvalidUsageLogID(t *testing.T) {
	for _, query := range []string{
		"usage_log_id=0", "usage_log_id=-9", "usage_log_id=abc", "usage_log_id=1.5",
	} {
		t.Run(query, func(t *testing.T) {
			repo := &usageLogIDLookupRepo{}
			recorder := listUsageWithQuery(t, query, repo)

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Zero(t, repo.listCalls, "an invalid usage log id must never reach the repository")
		})
	}
}
