//go:build unit

package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 管理端列表是有界的、新到在前的读取：页大小收敛到上限，投影只有落库的最小事实。
func TestGatewayMockEventListIsBoundedAndProjectsOnlyStoredFacts(t *testing.T) {
	// 未接线（没有数据库）按"不可读"处理，而不是回答一个空列表。
	var missing *GatewayMockEventRepo
	_, _, err := missing.ListGatewayMockEvents(context.Background(), service.GatewayMockEventListFilter{Page: 1, PageSize: 20})
	require.ErrorIs(t, err, service.ErrGatewayMockEventsUnavailable)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	occurred := time.Date(2026, 9, 30, 3, 4, 5, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM gateway_mock_events`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))
	// 越界的页大小在这里就被收窄成上限；第 1 页的偏移必须是 0。
	mock.ExpectQuery(regexp.QuoteMeta(`ORDER BY occurred_at DESC, id DESC`)).
		WithArgs(service.GatewayMockEventMaxPageSize, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"occurred_at", "rule_id", "rule_version", "protocol", "model",
			"api_key_id", "user_id", "group_id", "account_id", "client_ip", "trace_id",
		}).AddRow(
			occurred, "gmr_0123456789abcdef", "2026-09-30T03:00:00Z", "messages", "claude-sonnet-4-5",
			int64(7), int64(3), int64(2), int64(11), "203.0.113.7",
			"0123456789abcdef0123456789abcdef",
		))

	repo := NewGatewayMockEventRepo(db)
	records, total, err := repo.ListGatewayMockEvents(context.Background(), service.GatewayMockEventListFilter{Page: 1, PageSize: 5000})

	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, records, 1)
	require.Equal(t, "gmr_0123456789abcdef", records[0].RuleID)
	require.Equal(t, "messages", records[0].Protocol)
	require.Equal(t, int64(11), records[0].AccountID)
	require.Equal(t, "203.0.113.7", records[0].ClientIP)
	require.True(t, records[0].OccurredAt.Equal(occurred))
	require.NoError(t, mock.ExpectationsWereMet())

	// 投影不得点名配置内容：关键词与回复正文根本没有落库列可读；legacy 的清理期限
	// 列也不读——它是内部估算，不该出现在任何对外投影里。
	for _, statement := range []string{gatewayMockEventListColumns, gatewayMockEventListSQL} {
		for _, forbidden := range []string{"keyword", "reply", "request_digest", "cleanup_after"} {
			require.NotContains(t, statement, forbidden, "the list must not read %q", forbidden)
		}
	}
}

// 读取失败原样上抛，由调用方按不可读处理；仓储不在这里编造空结果。
func TestGatewayMockEventListSurfacesReadFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM gateway_mock_events`)).
		WillReturnError(context.DeadlineExceeded)

	repo := NewGatewayMockEventRepo(db)
	_, _, err = repo.ListGatewayMockEvents(context.Background(), service.GatewayMockEventListFilter{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
