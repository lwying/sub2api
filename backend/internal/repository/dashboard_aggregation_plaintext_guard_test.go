//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// plaintextGuardNonTxExecutor 把 *sql.DB 包一层，使它不再是 *sql.DB（走非事务分支），
// 也不是 *sql.Tx（无法确认事务）——即「连接池包装」这类无法证明持锁的适配器。
type plaintextGuardNonTxExecutor struct {
	*sql.DB
}

// TestDashboardAggregationCallerTransactionPinsOwnershipBeforeDelete 覆盖非事务分支里
// 调用方明确传入 *sql.Tx 的情形：守卫取 ACCESS SHARE 锁、二次核对之后才删除。
func TestDashboardAggregationCallerTransactionPinsOwnershipBeforeDelete(t *testing.T) {
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)

	expectPlaintextOwnershipPinned(mock)
	mock.ExpectExec(`(?s)DELETE FROM usage_logs`).
		WithArgs(sqlmock.AnyArg(), usageLogsCleanupBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 3))

	require.NoError(t, newDashboardAggregationRepositoryWithSQL(tx).
		cleanupUsageLogsBatches(context.Background(), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)))

	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestDashboardAggregationUnconfirmableAdapterRefusesDeployedPlaintextSidecars 覆盖无法
// 确认事务的适配器：旁路表已部署时它证明不了所有权锁能留到 DELETE，必须失败关闭、不删
// 任何 usage 行（只发生一次目录核对，没有任何 DELETE）。
func TestDashboardAggregationUnconfirmableAdapterRefusesDeployedPlaintextSidecars(t *testing.T) {
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()

	expectPlaintextOwnershipVerified(mock)

	err := newDashboardAggregationRepositoryWithSQL(plaintextGuardNonTxExecutor{DB: db}).
		cleanupUsageLogsBatches(context.Background(), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	require.ErrorIs(t, err, errPlaintextOwnershipLockUnpinned)
	require.ErrorContains(t, err, "request_audit_value_details")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestDashboardAggregationUnconfirmableAdapterKeepsUndeployedPlaintextCleanup 覆盖边界：
// 无法确认事务的适配器在两张旁路表都未部署（该能力未部署/未迁移）时照常推进保留期。
func TestDashboardAggregationUnconfirmableAdapterKeepsUndeployedPlaintextCleanup(t *testing.T) {
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()

	expectPlaintextOwnershipProbe(mock, false, true, false, true)
	mock.ExpectExec(`(?s)DELETE FROM usage_logs`).
		WithArgs(sqlmock.AnyArg(), usageLogsCleanupBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 3))

	require.NoError(t, newDashboardAggregationRepositoryWithSQL(plaintextGuardNonTxExecutor{DB: db}).
		cleanupUsageLogsBatches(context.Background(), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)))
	require.NoError(t, mock.ExpectationsWereMet())
}

// expectPlaintextOwnershipProbe 满足一次旁路所有权目录核对（列顺序与
// probePlaintextSidecarOwnership 的扫描顺序一致）。
func expectPlaintextOwnershipProbe(
	mock sqlmock.Sqlmock,
	valueDetailsExists, valueDetailsOwned, diagnosticsExists, diagnosticsOwned bool,
	traceState ...bool,
) {
	traceExists, traceOwned := false, true
	if len(traceState) == 2 {
		traceExists, traceOwned = traceState[0], traceState[1]
	}
	mock.ExpectQuery(`to_regclass\('request_audit_value_details'\)`).
		WillReturnRows(sqlmock.NewRows([]string{
			"value_details_exists", "value_details_owned", "diagnostics_exists", "diagnostics_owned",
			"traces_exists", "traces_owned",
		}).AddRow(valueDetailsExists, valueDetailsOwned, diagnosticsExists, diagnosticsOwned, traceExists, traceOwned))
}

// expectPlaintextOwnershipVerified 满足「两张旁路表都已部署且所有权外键完好」。
func expectPlaintextOwnershipVerified(mock sqlmock.Sqlmock) {
	expectPlaintextOwnershipProbe(mock, true, true, true, true)
}

// expectPlaintextOwnershipPinned 满足逐行 DELETE 分支的加固序列：核对通过后取
// ACCESS SHARE 锁把结论钉住，再重新核对一次。
func expectPlaintextOwnershipPinned(mock sqlmock.Sqlmock) {
	expectPlaintextOwnershipVerified(mock)
	mock.ExpectExec(`LOCK TABLE "request_audit_value_details" IN ACCESS SHARE MODE`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`LOCK TABLE "error_diagnostic_records" IN ACCESS SHARE MODE`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectPlaintextOwnershipVerified(mock)
}

func TestDashboardAggregationRefusesTraceWithoutOwnership(t *testing.T) {
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	expectPlaintextOwnershipProbe(mock, false, true, false, true, true, false)
	err = ensureUsageCleanupPreservesPlaintextOwnership(context.Background(), tx, true)
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.ErrorContains(t, err, "request_traces.usage_log_id")
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationPartitionDropRefusesUsageOwnedTrace(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()
	month := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs(month, "Asia/Shanghai").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`LOCK TABLE error_diagnostic_records`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`LOCK TABLE request_audit_value_details`).WillReturnResult(sqlmock.NewResult(0, 0))
	expectPlaintextOwnershipProbe(mock, true, true, true, true, true, true)
	mock.ExpectQuery(`SELECT to_regclass\('request_traces'\) IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`LOCK TABLE request_traces`).WillReturnResult(sqlmock.NewResult(0, 0))
	expectPlaintextOwnershipProbe(mock, true, true, true, true, true, true)
	mock.ExpectQuery(`SELECT EXISTS.*request_traces`).WithArgs("usage_logs_202604").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()
	err := dropUsageLogsPartitionWithRollupInvalidation(context.Background(), db, "usage_logs_202604", month)
	require.ErrorContains(t, err, "request traces")
	require.ErrorContains(t, err, "refusing DROP")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDashboardAggregationPartitionDropRefusesUsageOwnedPlaintext(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()
	month := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs(month, "Asia/Shanghai").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`LOCK TABLE error_diagnostic_records`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`LOCK TABLE request_audit_value_details`).WillReturnResult(sqlmock.NewResult(0, 0))
	expectPlaintextOwnershipVerified(mock)
	mock.ExpectQuery(`SELECT to_regclass\('request_traces'\) IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`SELECT EXISTS.*request_audit_value_details`).
		WithArgs("usage_logs_202604").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()

	err := dropUsageLogsPartitionWithRollupInvalidation(context.Background(), db, "usage_logs_202604", month)
	require.ErrorContains(t, err, "refusing DROP")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestDashboardAggregationPartitionDropRefusesOrphanablePlaintextWithoutOwnershipForeignKey
// 覆盖所有权外键被手工去掉后的 DROP 路径：受害分区此刻已经没有可关联的 usage 行
// （前一轮清理留下的孤儿），旧的 EXISTS 检查会误判为「干净」，因此必须在 DROP 前
// 用同一事务内的所有权核对拦住它，报错并保留分区。
func TestDashboardAggregationPartitionDropRefusesOrphanablePlaintextWithoutOwnershipForeignKey(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()
	month := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs(month, "Asia/Shanghai").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`LOCK TABLE error_diagnostic_records`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`LOCK TABLE request_audit_value_details`).WillReturnResult(sqlmock.NewResult(0, 0))
	expectPlaintextOwnershipProbe(mock, true, false, true, true)
	mock.ExpectRollback()

	err := dropUsageLogsPartitionWithRollupInvalidation(context.Background(), db, "usage_logs_202604", month)
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.ErrorContains(t, err, "usage_logs_202604")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestDashboardAggregationRowDeleteRefusesOrphanablePlaintextWithoutOwnershipForeignKey
// 覆盖逐行 DELETE 兜底路径（非分区形态）：所有权外键缺失时必须拒绝删除并回滚整批，
// 而不是先删 usage 行再留下孤儿。错误要指出是哪些列失去了所有权保证。
func TestDashboardAggregationRowDeleteRefusesOrphanablePlaintextWithoutOwnershipForeignKey(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	expectPlaintextOwnershipProbe(mock, true, false, true, false)
	mock.ExpectRollback()

	err := newDashboardAggregationRepositoryWithSQL(db).CleanupUsageLogs(context.Background(), cutoff)
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.ErrorContains(t, err, "request_audit_value_details.usage_log_id")
	require.ErrorContains(t, err, "error_diagnostic_records.plain_owner_usage_log_id")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestDashboardAggregationRowDeleteRechecksPlaintextOwnershipUnderSidecarLock 覆盖加固：
// 核对通过后必须先用 ACCESS SHARE 锁把「外键仍在」钉住再重新核对，只有第二次核对仍然
// 成立才允许删除。这里让第二次核对看到并发提交的 DROP CONSTRAINT，必须回滚。
func TestDashboardAggregationRowDeleteRechecksPlaintextOwnershipUnderSidecarLock(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	expectPlaintextOwnershipVerified(mock)
	mock.ExpectExec(`LOCK TABLE "request_audit_value_details" IN ACCESS SHARE MODE`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`LOCK TABLE "error_diagnostic_records" IN ACCESS SHARE MODE`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectPlaintextOwnershipProbe(mock, true, false, true, true)
	mock.ExpectRollback()

	err := newDashboardAggregationRepositoryWithSQL(db).CleanupUsageLogs(context.Background(), cutoff)
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestDashboardAggregationRowDeletePinsOwnershipBeforeDelete 覆盖受支持形态的正常路径：
// 一次核对、两把 ACCESS SHARE 锁、二次核对之后才执行 DELETE，保留期照常推进。
func TestDashboardAggregationRowDeletePinsOwnershipBeforeDelete(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	expectPlaintextOwnershipPinned(mock)
	mock.ExpectQuery(`(?s)SELECT tableoid, ctid.*RETURNING created_at`).
		WithArgs(cutoff, usageLogsCleanupBatchSize).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(cutoff.Add(-time.Hour)))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs(cutoff.Add(-time.Hour), "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), time.Unix(0, 0).UTC(), "Asia/Shanghai"))
	mock.ExpectCommit()

	require.NoError(t, repo.CleanupUsageLogs(context.Background(), cutoff))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestDashboardAggregationRowDeleteSkipsSidecarLockWhenPlaintextNotDeployed 覆盖边界：
// 两张旁路表都不存在（明文能力未部署）时核对即通过，不取锁，清理照常推进保留期。
func TestDashboardAggregationRowDeleteSkipsSidecarLockWhenPlaintextNotDeployed(t *testing.T) {
	setGroupUsageRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	defer func() { _ = db.Close() }()
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	repo := newDashboardAggregationRepositoryWithSQL(db)
	repo.clock = func() time.Time { return fixedNow }
	todayStart := service.GroupUsageTodayStart(fixedNow)

	mock.ExpectQuery(`SELECT EXISTS`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	expectPlaintextOwnershipProbe(mock, false, true, false, true)
	mock.ExpectQuery(`(?s)SELECT tableoid, ctid.*RETURNING created_at`).
		WithArgs(cutoff, usageLogsCleanupBatchSize).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT closed_before::text, retained_from.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"closed_before", "retained_from", "timezone_name"}).
			AddRow(service.GroupUsageDate(todayStart), time.Unix(0, 0).UTC(), "Asia/Shanghai"))
	mock.ExpectCommit()

	require.NoError(t, repo.CleanupUsageLogs(context.Background(), cutoff))
	require.NoError(t, mock.ExpectationsWereMet())
}
