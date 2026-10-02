//go:build unit

package repository

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 清理判定必须按 occurred_at 与"当次"使用记录保留策略算出的 cutoff 比较，而不是按
// 写入时记下的 cleanup_after。后者是 legacy 内部字段：拿它比较会把实际清理拖长到该
// 期限之后，策略调短也不能立即生效，还会让留得更久的记录先被清掉。
func TestGatewayMockEventCleanupJudgesByOccurrenceNotRecordedDeadline(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	cutoff := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	// 仓储只推进一批（limit 就是批大小）；继续清理是调用方保留清理轮次的事。
	mock.ExpectExec(regexp.QuoteMeta("WHERE occurred_at <= $1")).WithArgs(cutoff, 500).
		WillReturnResult(sqlmock.NewResult(0, 500))

	deleted, err := NewGatewayMockEventRepo(db).DeleteMockEventsBefore(context.Background(), cutoff, 500)
	require.NoError(t, err)
	require.Equal(t, int64(500), deleted)
	require.NoError(t, mock.ExpectationsWereMet())

	require.Contains(t, gatewayMockEventDeleteSQL, "WHERE occurred_at <= $1")
	require.Contains(t, gatewayMockEventDeleteSQL, "ORDER BY occurred_at ASC, id ASC")
	require.Contains(t, gatewayMockEventDeleteSQL, "LIMIT $2")
	require.NotContains(t, gatewayMockEventDeleteSQL, "cleanup_after",
		"the legacy internal column must not decide what gets deleted")
}

// futureTimestamp 断言写入的 cleanup_after 是 NOT NULL 列上的合法时间戳：
// 旧实例（滚动发布中的旧进程、回滚后的旧二进制）会把它读成 time.Time，
// 写入 NULL 或零值时间都会让它们读不动。
type futureTimestamp struct{}

func (futureTimestamp) Match(value driver.Value) bool {
	stamp, ok := value.(time.Time)
	return ok && !stamp.IsZero() && stamp.After(time.Now())
}

// 清理期限保持发布版本的行为：始终写一个合法时间戳，不回退成 NULL。
func TestGatewayMockEventInsertAlwaysWritesAValidTimestamp(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO gateway_mock_events")).
		WithArgs("gmr_0123456789abcdef", "v1", "messages", "claude-sonnet-4-5",
			int64(7), int64(3), int64(2), int64(11), "203.0.113.7", "0123456789abcdef0123456789abcdef",
			futureTimestamp{}, service.GatewayMockContentAuditUnknown).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = NewGatewayMockEventRepo(db).RecordGatewayMockEvent(context.Background(), service.GatewayMockEventInput{
		RuleID: "gmr_0123456789abcdef", RuleVersion: "v1", Protocol: "messages", Model: "claude-sonnet-4-5",
		APIKeyID: 7, UserID: 3, GroupID: 2, AccountID: 11, ClientIP: "203.0.113.7",
		TraceID: "0123456789abcdef0123456789abcdef",
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// 调用方确实给出期限时照原样落库，不做二次加工。
	explicit := time.Date(2026, 12, 29, 3, 4, 5, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO gateway_mock_events")).
		WithArgs("gmr_explicit", "", "responses", "", int64(0), int64(0), int64(0), int64(0), "", "", explicit,
			service.GatewayMockContentAuditUnknown).
		WillReturnResult(sqlmock.NewResult(2, 1))
	err = NewGatewayMockEventRepo(db).insertGatewayMockEvent(context.Background(), GatewayMockEvent{
		RuleID: "gmr_explicit", Protocol: "responses", CleanupAfter: explicit,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 写入侧把内容审计状态收敛到闭集：调用方给出的合法状态照原样落库；未给出（空串）或闭集
// 之外的值写 unknown，绝不伪造成 skipped_local_mock。
func TestGatewayMockEventInsertNormalizesContentAuditState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"skipped local mock round-trips", service.GatewayMockContentAuditSkippedLocalMock, service.GatewayMockContentAuditSkippedLocalMock},
		{"unknown round-trips", service.GatewayMockContentAuditUnknown, service.GatewayMockContentAuditUnknown},
		{"missing becomes unknown, never skipped", "", service.GatewayMockContentAuditUnknown},
		{"unknown enum becomes unknown", "invented_state", service.GatewayMockContentAuditUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()

			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO gateway_mock_events")).
				WithArgs("gmr_state", "", "messages", "", int64(0), int64(0), int64(0), int64(0), "", "",
					futureTimestamp{}, tc.want).
				WillReturnResult(sqlmock.NewResult(1, 1))

			err = NewGatewayMockEventRepo(db).insertGatewayMockEvent(context.Background(),
				GatewayMockEvent{RuleID: "gmr_state", Protocol: "messages", ContentAuditState: tc.input})
			require.NoError(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// 对外投影里不能有读出该列的路径：写入时的估算不是可披露的实际清理时间。
func TestGatewayMockEventProjectionNeverReadsTheLegacyDeadline(t *testing.T) {
	for _, statement := range []string{gatewayMockEventListColumns, gatewayMockEventListSQL} {
		require.NotContains(t, statement, "cleanup_after",
			"the list must not read the internal estimate")
	}
}
