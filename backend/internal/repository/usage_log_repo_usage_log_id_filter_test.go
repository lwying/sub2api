package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

// 从请求 Trace 跳转而来的 usage_log_id 是精确记录定位：必须作为 id = $n 绑定进 SQL，
// 而不是被忽略后返回整个列表让管理员误以为定位生效。id 过滤下结果集至多一行，
// 因此即使未要求 exact_total 也必须走精确计数，否则分页总数会骗人。
func TestUsageLogRepositoryListWithFiltersUsageLogID(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	filters := usagestats.UsageLogFilters{UsageLogID: 4242}

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM usage_logs WHERE id = \\$1").
		WithArgs(int64(4242)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectQuery("SELECT .* FROM usage_logs WHERE id = \\$1 ORDER BY id DESC LIMIT \\$2 OFFSET \\$3").
		WithArgs(int64(4242), 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	logs, page, err := repo.ListWithFilters(context.Background(), pagination.PaginationParams{Page: 1, PageSize: 20}, filters)

	require.NoError(t, err)
	require.Empty(t, logs)
	require.NotNil(t, page)
	require.Equal(t, int64(1), page.Total)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 缺省使用记录定位时必须保持"不过滤"：不能把 0 当成"筛选 0 号使用记录"，
// 也不能顺手改变其它筛选的 SQL 形态。
func TestUsageLogRepositoryListWithFiltersOmitsAbsentUsageLogID(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	start := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	filters := usagestats.UsageLogFilters{StartTime: &start, ExactTotal: true}

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM usage_logs WHERE created_at >= \\$1").
		WithArgs(start).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery("SELECT .* FROM usage_logs WHERE created_at >= \\$1 ORDER BY id DESC LIMIT \\$2 OFFSET \\$3").
		WithArgs(start, 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	logs, page, err := repo.ListWithFilters(context.Background(), pagination.PaginationParams{Page: 1, PageSize: 20}, filters)

	require.NoError(t, err)
	require.Empty(t, logs)
	require.NotNil(t, page)
	require.NoError(t, mock.ExpectationsWereMet())
}
