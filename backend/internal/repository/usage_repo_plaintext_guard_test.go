package repository

// 本文件覆盖 usage 清理两条自有路径（管理端清理任务的批量 DELETE 与单行 DELETE）在
// 「随 usage 明文旁路表已部署、但所有权外键被手工去掉」这一不受支持的分区配置下的行为：
// 删除必须失败关闭、保留数据，而不是把值明细/已关联明文诊断留成仍可读的孤儿。
// 受支持形态（外键完好或旁路表未部署）必须照常推进。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// expectUsageCleanupOwnershipProbe 满足一次随 usage 明文旁路表的所有权目录核对
// （列顺序与 probePlaintextSidecarOwnership 的扫描顺序一致）。
func expectUsageCleanupOwnershipProbe(
	mock sqlmock.Sqlmock,
	valueDetailsExists, valueDetailsOwned, diagnosticsExists, diagnosticsOwned bool,
) {
	mock.ExpectQuery(`to_regclass\('request_audit_value_details'\)`).
		WillReturnRows(sqlmock.NewRows([]string{
			"value_details_exists", "value_details_owned", "diagnostics_exists", "diagnostics_owned",
		}).AddRow(valueDetailsExists, valueDetailsOwned, diagnosticsExists, diagnosticsOwned))
}

// expectUsageCleanupOwnershipPinned 满足逐行 DELETE 分支的加固序列：核对通过后取
// ACCESS SHARE 锁把「外键仍在」钉到事务结束，再重新核对一次。
func expectUsageCleanupOwnershipPinned(mock sqlmock.Sqlmock) {
	expectUsageCleanupOwnershipProbe(mock, true, true, true, true)
	mock.ExpectExec(`LOCK TABLE "request_audit_value_details" IN ACCESS SHARE MODE`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`LOCK TABLE "error_diagnostic_records" IN ACCESS SHARE MODE`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectUsageCleanupOwnershipProbe(mock, true, true, true, true)
}

// plaintextGuardSQLExecutor 把 *sql.DB 包成 sqlExecutor，但**不是** *sql.DB，
// 用来走批量删除的非事务兜底分支。
type plaintextGuardSQLExecutor struct{ db *sql.DB }

func (e plaintextGuardSQLExecutor) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return e.db.ExecContext(ctx, query, args...)
}

func (e plaintextGuardSQLExecutor) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return e.db.QueryContext(ctx, query, args...)
}

// TestUsageCleanupRepositoryDeleteUsageLogsBatchRefusesOrphanablePlaintextWithoutOwnershipForeignKey
// 覆盖管理端清理任务的事务批量删除：值明细表已部署但所有权外键缺失时，必须在同一事务里
// 拒绝整批并回滚，不能先删 usage 行再留下孤儿。
func TestUsageCleanupRepositoryDeleteUsageLogsBatchRefusesOrphanablePlaintextWithoutOwnershipForeignKey(t *testing.T) {
	setUsageCleanupRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := &usageCleanupRepository{sql: db}

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	filters := service.UsageCleanupFilters{StartTime: start, EndTime: end}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	expectUsageCleanupOwnershipProbe(mock, true, false, true, true)
	mock.ExpectRollback()

	_, err := repo.DeleteUsageLogsBatch(context.Background(), filters, 5)
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.ErrorContains(t, err, "request_audit_value_details.usage_log_id")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageCleanupRepositoryDeleteUsageLogsBatchNonTransactionalRefusesOrphanablePlaintext
// 覆盖没有可持锁事务的兜底分支：仍然必须在删除前核对所有权，缺失即失败关闭。
func TestUsageCleanupRepositoryDeleteUsageLogsBatchNonTransactionalRefusesOrphanablePlaintext(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageCleanupRepository{sql: plaintextGuardSQLExecutor{db: db}}

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	filters := service.UsageCleanupFilters{StartTime: start, EndTime: end}

	expectUsageCleanupOwnershipProbe(mock, true, true, true, false)

	_, err := repo.DeleteUsageLogsBatch(context.Background(), filters, 5)
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.ErrorContains(t, err, "error_diagnostic_records.plain_owner_usage_log_id")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageCleanupRepositoryDeleteUsageLogsBatchNonTransactionalRefusesDeployedPlaintextItCannotPin
// 兜底分支拿不到可确认的事务，留不住 ACCESS SHARE 锁，因而即使这次核对看到所有权外键完好，
// 也无法保证它在 DELETE 之前不被并发的 DROP CONSTRAINT 移除；旁路表已部署时必须失败关闭，
// 而不是先删后留孤儿。
func TestUsageCleanupRepositoryDeleteUsageLogsBatchNonTransactionalRefusesDeployedPlaintextItCannotPin(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageCleanupRepository{sql: plaintextGuardSQLExecutor{db: db}}

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	filters := service.UsageCleanupFilters{StartTime: start, EndTime: end}

	expectUsageCleanupOwnershipProbe(mock, true, true, true, true)

	_, err := repo.DeleteUsageLogsBatch(context.Background(), filters, 5)
	require.ErrorIs(t, err, errPlaintextOwnershipUnpinned)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageCleanupRepositoryDeleteUsageLogsBatchNonTransactionalProceedsWhenPlaintextNotDeployed
// 向后兼容：明文旁路表都未部署时核对即通过，清理照常推进保留期。
func TestUsageCleanupRepositoryDeleteUsageLogsBatchNonTransactionalProceedsWhenPlaintextNotDeployed(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageCleanupRepository{sql: plaintextGuardSQLExecutor{db: db}}

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	filters := service.UsageCleanupFilters{StartTime: start, EndTime: end}

	expectUsageCleanupOwnershipProbe(mock, false, true, false, true)
	mock.ExpectQuery(`(?s)DELETE FROM usage_logs.*RETURNING created_at`).
		WithArgs(start, end, 2).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(start.Add(time.Hour)))

	deleted, err := repo.DeleteUsageLogsBatch(context.Background(), filters, 2)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageCleanupRepositoryDeleteUsageLogsBatchSkipsSidecarLocksWhenPlaintextNotDeployed
// 边界：两张旁路表都未部署时核对即通过、不取锁，清理照常推进保留期。
func TestUsageCleanupRepositoryDeleteUsageLogsBatchSkipsSidecarLocksWhenPlaintextNotDeployed(t *testing.T) {
	setUsageCleanupRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := &usageCleanupRepository{sql: db}

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	filters := service.UsageCleanupFilters{StartTime: start, EndTime: end}
	deletedAt := start.Add(time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	expectUsageCleanupOwnershipProbe(mock, false, true, false, true)
	mock.ExpectQuery(`(?s)DELETE FROM usage_logs.*RETURNING created_at`).
		WithArgs(start, end, 2).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(deletedAt))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs(deletedAt, "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	deleted, err := repo.DeleteUsageLogsBatch(context.Background(), filters, 2)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageLogRepositoryDeleteRefusesOrphanablePlaintextWithoutOwnershipForeignKey
// 覆盖单行使用记录删除：所有权外键缺失时必须回滚，不能删掉 usage 行后留下明文孤儿。
func TestUsageLogRepositoryDeleteRefusesOrphanablePlaintextWithoutOwnershipForeignKey(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newUsageLogRepositoryWithSQL(nil, db)

	mock.ExpectBegin()
	expectUsageCleanupOwnershipProbe(mock, true, false, true, true)
	mock.ExpectRollback()

	err := repo.Delete(context.Background(), 7)
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.ErrorContains(t, err, "request_audit_value_details.usage_log_id")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageLogRepositoryDeletePinsOwnershipBeforeDelete 受支持形态：核对、取锁、二次核对
// 之后才执行单行 DELETE，删除照常成功。
func TestUsageLogRepositoryDeletePinsOwnershipBeforeDelete(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newUsageLogRepositoryWithSQL(nil, db)

	mock.ExpectBegin()
	expectUsageCleanupOwnershipPinned(mock)
	mock.ExpectExec(`DELETE FROM usage_logs WHERE id = \$1`).
		WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.Delete(context.Background(), 7))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageLogRepositoryDeleteCallerTransactionPinsOwnershipBeforeDelete 覆盖调用方传入事务
// 的适配器（例如 ent.Tx）：必须在那个事务里取锁钉住结论，随后单行 DELETE 照常成功。
func TestUsageLogRepositoryDeleteCallerTransactionPinsOwnershipBeforeDelete(t *testing.T) {
	db, mock := newSQLMock(t)
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	repo := newUsageLogRepositoryWithSQL(nil, tx)

	expectUsageCleanupOwnershipPinned(mock)
	mock.ExpectExec(`DELETE FROM usage_logs WHERE id = \$1`).
		WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.Delete(context.Background(), 7))
	// 传入事务由调用方收尾：Delete 只负责在删除前把所有权结论钉住。
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageLogRepositoryDeleteAdapterWithoutPoolRefusesOrphanablePlaintext 覆盖没有连接池
// 可自建事务、也不是事务的适配器：只读核对发现所有权缺失时必须失败关闭。
func TestUsageLogRepositoryDeleteAdapterWithoutPoolRefusesOrphanablePlaintext(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newUsageLogRepositoryWithSQL(nil, plaintextGuardSQLExecutor{db: db})

	expectUsageCleanupOwnershipProbe(mock, true, true, true, false)

	err := repo.Delete(context.Background(), 7)
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageLogRepositoryDeleteAdapterWithoutPoolRefusesUnpinnableDeployedPlaintext 覆盖同一
// 适配器下的另一半边界：本次核对看到外键完好也不能删已部署的旁路表，因为该执行器留不住锁。
func TestUsageLogRepositoryDeleteAdapterWithoutPoolRefusesUnpinnableDeployedPlaintext(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newUsageLogRepositoryWithSQL(nil, plaintextGuardSQLExecutor{db: db})

	expectUsageCleanupOwnershipProbe(mock, true, true, true, true)

	err := repo.Delete(context.Background(), 7)
	require.ErrorIs(t, err, errPlaintextOwnershipUnpinned)
	require.NoError(t, mock.ExpectationsWereMet())
}
