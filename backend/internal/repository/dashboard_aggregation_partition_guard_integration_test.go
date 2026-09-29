//go:build integration

package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 本文件用真实 PostgreSQL 合成数据核对 ADR 0007 / 票据 10 的「月分区 DROP vs 逐行
// DELETE」结论，结论分三条：
//
//  1. 受支持的非分区形态：`CleanupUsageLogs` 的逐行 DELETE 继续推进保留期，随 usage
//     的明文值明细与已关联明文诊断由所有权外键级联同步消失，无 usage 的独立诊断不受
//     影响。这是当前迁移能表达的唯一「明文 + usage」形态。
//  2. 受害分区仍持有随 usage 明文时，分区 DROP 必须失败安全：报错、分区与数据原样
//     保留，别的分区的关联明文不得被误判成本次受害者。
//  3. 受支持的明文所有权外键（迁移 253/255 的单列 `REFERENCES usage_logs(id)`
//     ON DELETE CASCADE`）在分区父表上根本建不出来（PostgreSQL 要求被引用键在整张
//     分区表上唯一）。因此「分区 usage_logs + 明文能力」不是迁移能表达的受支持配置：
//     真要走分区部署，部署方必须改写或去掉这些外键，而两种改写都不是受支持形态
//     （改写 ⇒ 分区 DROP 被 PostgreSQL 依赖检查拒绝；去掉 ⇒ 逐行 DELETE 失去级联）。
//     按 ADR 0007 的口径，这种部署组合应当在开启新明文之前就被阻止（采集侧门禁由
//     对应票证负责）；清理侧在这类形态下必须失败关闭：先在同一事务里核对所有权外键，
//     只要旁路表已部署而外键缺失，就既不 DROP 分区也不删 usage 行，而是报错并保留
//     数据，两轮清理都不允许留下孤儿。
//
// 集成测试库里的 usage_logs 是普通表（迁移 035 只在 usage_logs 已是分区表时建分区），
// 所以这里全部用独立 schema 复刻最小结构，避免动到共享表；分区场景额外让仓储拿到
// search_path 固定在该 schema 的 *sql.DB（只有 *sql.DB 分支才会真正走 DROP 路径）。

const partitionGuardAprilStart = "2026-04-01"

// partitionGuardShape 选择复刻 schema 的形态。
type partitionGuardShape int

const (
	// partitionGuardSupported：与迁移 253/255 一致的非分区 usage_logs + 单列所有权外键。
	partitionGuardSupported partitionGuardShape = iota
	// partitionGuardSupportedNoFK：非分区 usage_logs，但部署方手工去掉了两张明文旁路
	// 的所有权外键。usage 行的逐行 DELETE 从此不再级联，用于证明清理必须失败关闭。
	partitionGuardSupportedNoFK
	// partitionGuardPartitionedCompositeFK：分区 usage_logs + (id, created_at) 复合
	// 外键。复合形式是分区父表上唯一可能的外键形状，但它会被 PostgreSQL 的分区
	// DROP 依赖检查挡住。
	partitionGuardPartitionedCompositeFK
	// partitionGuardPartitionedNoFK：分区 usage_logs 且不带任何所有权外键，用于证明
	// 受支持的单列外键在分区父表上无法建立，以及外键被去掉后清理必须失败关闭。
	partitionGuardPartitionedNoFK
)

// TestPartitionedUsageLogsCannotCarrySupportedPlaintextOwnershipForeignKey 用真实
// PostgreSQL 证明：只要 usage_logs 按月分区，迁移 253/255 里的单列所有权外键就建不
// 出来。这是「分区部署下不能安全开启明文」这一结论的实证，不是待修的代码缺陷。
func TestPartitionedUsageLogsCannotCarrySupportedPlaintextOwnershipForeignKey(t *testing.T) {
	ctx := context.Background()
	_, db := newPartitionGuardReplica(t, ctx, partitionGuardPartitionedNoFK)

	for name, stmt := range map[string]string{
		"request_audit_value_details.usage_log_id": `ALTER TABLE request_audit_value_details
			ADD CONSTRAINT probe_value_detail_usage_fkey
			FOREIGN KEY (usage_log_id) REFERENCES usage_logs (id) ON DELETE CASCADE`,
		"error_diagnostic_records.usage_log_id": `ALTER TABLE error_diagnostic_records
			ADD CONSTRAINT probe_diagnostic_usage_fkey
			FOREIGN KEY (usage_log_id) REFERENCES usage_logs (id) ON DELETE SET NULL`,
		"error_diagnostic_records.plain_owner_usage_log_id": `ALTER TABLE error_diagnostic_records
			ADD CONSTRAINT probe_diagnostic_plain_owner_fkey
			FOREIGN KEY (plain_owner_usage_log_id) REFERENCES usage_logs (id) ON DELETE CASCADE`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.ErrorContains(t, err, "no unique constraint matching given keys",
			"%s：分区 usage_logs 无法承载迁移 253/255 的受支持单列外键", name)
	}
}

// TestCleanupUsageLogsRowDeleteCascadesUsageOwnedPlaintext 覆盖受支持的非分区形态：
// 逐行 DELETE 兜底推进保留期时，两类随 usage 明文都由所有权外键级联删除。
func TestCleanupUsageLogsRowDeleteCascadesUsageOwnedPlaintext(t *testing.T) {
	ctx := context.Background()
	repo, db := newPartitionGuardReplica(t, ctx, partitionGuardSupported)

	agedDetail := time.Date(2020, 1, 5, 3, 0, 0, 0, time.UTC)
	seedPartitionGuardUsage(t, ctx, db, 1, agedDetail)
	seedPartitionGuardValueDetail(t, ctx, db, 1, agedDetail, "plaintext_usage_bound")
	seedPartitionGuardLinkedDiagnostic(t, ctx, db, 1, agedDetail)
	seedPartitionGuardUnlinkedDiagnostic(t, ctx, db)
	// 未过期的 usage：不能被本次清理带走。
	recent := time.Date(2026, 5, 20, 3, 0, 0, 0, time.UTC)
	seedPartitionGuardUsage(t, ctx, db, 2, recent)
	seedPartitionGuardValueDetail(t, ctx, db, 2, recent, "plaintext_usage_bound")

	require.NoError(t, repo.CleanupUsageLogs(ctx, time.Date(2020, 1, 6, 0, 0, 0, 0, time.UTC)))

	require.Zero(t, partitionGuardCount(t, ctx, db, "usage_logs WHERE id = 1"),
		"过期 usage 行必须被逐行 DELETE 清掉")
	require.Zero(t, partitionGuardCount(t, ctx, db,
		"request_audit_value_details WHERE usage_log_id = 1"),
		"随 usage 的明文值明细必须随 usage 删除")
	require.Zero(t, partitionGuardCount(t, ctx, db,
		"error_diagnostic_records WHERE plain_owner_usage_log_id IS NOT NULL"),
		"已关联明文诊断必须整行随 usage 删除")
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
		"error_diagnostic_records WHERE plain_owner_usage_log_id IS NULL"),
		"无 usage 的独立诊断保留自己的三十天期限，不由 usage 清理删除")
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
		"request_audit_value_details WHERE usage_log_id = 2"),
		"未到期的随 usage 明文不受影响")
	require.Zero(t, partitionGuardValueDetailOrphans(t, ctx, db))
	require.Zero(t, partitionGuardDiagnosticOrphans(t, ctx, db))
}

// TestCleanupUsageLogsPartitionDropRefusesLinkedPlaintextOnRealPostgres 覆盖受害分区
// 仍持有随 usage 明文时的 DROP 门禁与失败安全语义。复刻表保留所有权外键（分区父表
// 只能写成 (id, created_at) 复合形式），因此这条形态下逐行 DELETE 仍有级联、而分区
// DROP 本身会被 PostgreSQL 的依赖检查拒绝。
func TestCleanupUsageLogsPartitionDropRefusesLinkedPlaintextOnRealPostgres(t *testing.T) {
	ctx := context.Background()
	repo, db := newPartitionGuardReplica(t, ctx, partitionGuardPartitionedCompositeFK)

	aprilDetail := time.Date(2026, 4, 3, 1, 0, 0, 0, time.UTC)
	aprilDiagnostic := time.Date(2026, 4, 4, 1, 0, 0, 0, time.UTC)
	mayDetail := time.Date(2026, 5, 20, 1, 0, 0, 0, time.UTC)
	seedPartitionGuardUsage(t, ctx, db, 1, aprilDetail)
	seedPartitionGuardUsage(t, ctx, db, 2, aprilDiagnostic)
	seedPartitionGuardUsage(t, ctx, db, 3, mayDetail)
	seedPartitionGuardValueDetail(t, ctx, db, 1, aprilDetail, "plaintext_usage_bound")
	seedPartitionGuardLinkedDiagnostic(t, ctx, db, 2, aprilDiagnostic)
	// 无 usage 的独立诊断不由 usage 清理负责，也不能算作本次 DROP 的受害者。
	seedPartitionGuardUnlinkedDiagnostic(t, ctx, db)
	seedPartitionGuardValueDetail(t, ctx, db, 3, mayDetail, "plaintext_usage_bound")

	// 2026-04 分区已过期（月份早于 cut-off 月），2026-05 分区仍在保留期内。
	cutoff := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	err := repo.CleanupUsageLogs(ctx, cutoff)
	require.ErrorContains(t, err, "usage_logs_202604", "受害分区仍有随 usage 明文时必须拒绝 DROP")
	require.ErrorContains(t, err, "refusing DROP")

	require.True(t, partitionGuardRelationExists(t, ctx, db, "usage_logs_202604"),
		"被拒绝的 DROP 不得留下半个删除：分区必须原样存在")
	require.True(t, partitionGuardRelationExists(t, ctx, db, "usage_logs_202605"),
		"未到期的分区不受影响")
	require.Zero(t, partitionGuardUsageRows(t, ctx, db, "usage_logs_202604"),
		"被拒绝的 DROP 之后，行级 DELETE 兜底必须继续推进保留期")
	require.Zero(t, partitionGuardValueDetailOrphans(t, ctx, db),
		"usage 行真删除时明文值明细必须同批消失，不能留成孤儿")
	require.Zero(t, partitionGuardDiagnosticOrphans(t, ctx, db),
		"usage 行真删除时已关联明文诊断必须整行消失，不能留成孤儿")
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
		"request_audit_value_details WHERE usage_log_id = 3"),
		"另一分区的关联明文不得被误判为本次 DROP 的受害者")
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
		"error_diagnostic_records WHERE plain_owner_usage_log_id IS NULL"),
		"无 usage 的独立诊断不由 usage 清理负责")

	// 明文已清空，但保留所有权外键时分区 DROP 仍会被 PostgreSQL 拒绝；关键是它必须
	// 失败安全：报错并保持分区原样，而不是静默丢掉分区里的旁路行。
	err = repo.CleanupUsageLogs(ctx, cutoff)
	require.Error(t, err, "保留所有权外键时 PostgreSQL 拒绝 DROP 分区，必须如实报错而不是静默成功")
	require.True(t, partitionGuardRelationExists(t, ctx, db, "usage_logs_202604"))
	require.True(t, partitionGuardRelationExists(t, ctx, db, "usage_logs_202605"))
	require.Zero(t, partitionGuardValueDetailOrphans(t, ctx, db))
	require.Zero(t, partitionGuardDiagnosticOrphans(t, ctx, db))
}

// TestCleanupUsageLogsRefusesOrphaningPlaintextInForeignKeyLessPartition 覆盖「手工去掉
// 所有权外键的分区 usage_logs」这一不受支持、但必须失败关闭的组合：第一轮清理在 DROP
// 前就因所有权外键缺失而拒绝；此时逐行 DELETE 兜底已经失去级联，若照旧执行就会把明文
// 留成孤儿，第二轮清理又会因为「分区里再没有可关联的 usage 行」而放行 DROP。因此两轮
// 都必须报错、保留数据，且任何时刻都不得出现孤儿。
func TestCleanupUsageLogsRefusesOrphaningPlaintextInForeignKeyLessPartition(t *testing.T) {
	ctx := context.Background()
	repo, db := newPartitionGuardReplica(t, ctx, partitionGuardPartitionedNoFK)

	aprilDetail := time.Date(2026, 4, 3, 1, 0, 0, 0, time.UTC)
	aprilDiagnostic := time.Date(2026, 4, 4, 1, 0, 0, 0, time.UTC)
	mayDetail := time.Date(2026, 5, 20, 1, 0, 0, 0, time.UTC)
	seedPartitionGuardUsage(t, ctx, db, 1, aprilDetail)
	seedPartitionGuardUsage(t, ctx, db, 2, aprilDiagnostic)
	seedPartitionGuardUsage(t, ctx, db, 3, mayDetail)
	seedPartitionGuardValueDetail(t, ctx, db, 1, aprilDetail, "plaintext_usage_bound")
	seedPartitionGuardLinkedDiagnostic(t, ctx, db, 2, aprilDiagnostic)
	seedPartitionGuardUnlinkedDiagnostic(t, ctx, db)
	seedPartitionGuardValueDetail(t, ctx, db, 3, mayDetail, "plaintext_usage_bound")

	cutoff := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	for cycle := 1; cycle <= 2; cycle++ {
		err := repo.CleanupUsageLogs(ctx, cutoff)
		require.ErrorIs(t, err, errPlaintextOwnershipUnverified,
			"第 %d 轮清理必须在所有权外键缺失时失败关闭", cycle)
		require.ErrorContains(t, err, "usage_logs_202604",
			"错误必须指出被拒绝的分区，便于告警定位")

		require.True(t, partitionGuardRelationExists(t, ctx, db, "usage_logs_202604"),
			"第 %d 轮清理不得 DROP 受害分区", cycle)
		require.True(t, partitionGuardRelationExists(t, ctx, db, "usage_logs_202605"),
			"第 %d 轮清理不得动到未到期的分区", cycle)
		require.EqualValues(t, 2, partitionGuardUsageRows(t, ctx, db, "usage_logs_202604"),
			"第 %d 轮清理不得删除 usage 行：失去级联的 DELETE 就是制造孤儿", cycle)
		require.EqualValues(t, 1, partitionGuardUsageRows(t, ctx, db, "usage_logs_202605"),
			"未过期的 usage 行不受影响")
		require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
			"request_audit_value_details WHERE usage_log_id = 1"),
			"明文值明细必须原样保留")
		require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
			"error_diagnostic_records WHERE plain_owner_usage_log_id IS NOT NULL"),
			"已关联明文诊断必须原样保留")
		require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
			"error_diagnostic_records WHERE plain_owner_usage_log_id IS NULL"),
			"无 usage 的独立诊断不由 usage 清理负责")
		require.Zero(t, partitionGuardValueDetailOrphans(t, ctx, db))
		require.Zero(t, partitionGuardDiagnosticOrphans(t, ctx, db))
	}
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
		"request_audit_value_details WHERE usage_log_id = 3"),
		"另一分区的关联明文不得被误判或误删")
}

// TestCleanupUsageLogsRefusesOrphaningLinkedPlaintextWithoutOwnershipForeignKey 覆盖非分区
// 形态下所有权外键被手工去掉、逐行 DELETE 会把已关联明文诊断留成孤儿的情形：清理必须
// 在删除前拒绝，usage 行与明文诊断都原样保留。值明细一侧由上一支测试覆盖。
func TestCleanupUsageLogsRefusesOrphaningLinkedPlaintextWithoutOwnershipForeignKey(t *testing.T) {
	ctx := context.Background()
	repo, db := newPartitionGuardReplica(t, ctx, partitionGuardSupportedNoFK)

	aged := time.Date(2020, 1, 5, 3, 0, 0, 0, time.UTC)
	seedPartitionGuardUsage(t, ctx, db, 1, aged)
	seedPartitionGuardLinkedDiagnostic(t, ctx, db, 1, aged)
	seedPartitionGuardUnlinkedDiagnostic(t, ctx, db)

	err := repo.CleanupUsageLogs(ctx, time.Date(2020, 1, 6, 0, 0, 0, 0, time.UTC))
	require.ErrorIs(t, err, errPlaintextOwnershipUnverified)
	require.ErrorContains(t, err, "error_diagnostic_records.plain_owner_usage_log_id",
		"错误必须指出是哪一张旁路表失去了所有权保证")

	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db, "usage_logs WHERE id = 1"),
		"失去级联时不得删除 usage 行")
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
		"error_diagnostic_records WHERE plain_owner_usage_log_id IS NOT NULL"),
		"已关联明文诊断不得被留成孤儿")
	require.EqualValues(t, 1, partitionGuardCount(t, ctx, db,
		"error_diagnostic_records WHERE plain_owner_usage_log_id IS NULL"),
		"无 usage 的独立诊断不受影响")
	require.Zero(t, partitionGuardValueDetailOrphans(t, ctx, db))
	require.Zero(t, partitionGuardDiagnosticOrphans(t, ctx, db))
}

// newPartitionGuardReplica 在独立 schema 里复刻 usage_logs 与两张明文旁路的最小结构，
// 返回仓储与该 schema 绑定的 *sql.DB（search_path 固定到该 schema）。
func newPartitionGuardReplica(
	t *testing.T,
	ctx context.Context,
	shape partitionGuardShape,
) (*dashboardAggregationRepository, *sql.DB) {
	t.Helper()

	schema := fmt.Sprintf("usage_partition_guard_%d", time.Now().UnixNano())
	quoted := pq.QuoteIdentifier(schema)
	_, err := integrationDB.ExecContext(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)

	usageLogsDDL := `
		CREATE TABLE usage_logs (
			id BIGINT PRIMARY KEY,
			created_at TIMESTAMPTZ NOT NULL,
			group_id BIGINT,
			actual_cost NUMERIC(20, 10) NOT NULL DEFAULT 0
		);`
	valueDetailDDL := `
		CREATE TABLE request_audit_value_details (
			id BIGSERIAL PRIMARY KEY,
			usage_log_id BIGINT NOT NULL UNIQUE REFERENCES usage_logs (id) ON DELETE CASCADE,
			usage_created_at TIMESTAMPTZ NOT NULL,
			state TEXT NOT NULL,
			storage_format TEXT NOT NULL,
			expires_at TIMESTAMPTZ
		);`
	diagnosticDDL := `
		CREATE TABLE error_diagnostic_records (
			diagnostic_id TEXT PRIMARY KEY,
			usage_log_id BIGINT REFERENCES usage_logs (id) ON DELETE SET NULL,
			plain_record BOOLEAN NOT NULL DEFAULT FALSE,
			plain_owner_usage_log_id BIGINT REFERENCES usage_logs (id) ON DELETE CASCADE,
			plain_owner_created_at TIMESTAMPTZ,
			metadata_expires_at TIMESTAMPTZ NOT NULL
		);`
	traceDDL := `
		CREATE TABLE request_traces (
			id BIGINT PRIMARY KEY,
			usage_log_id BIGINT REFERENCES usage_logs (id) ON DELETE CASCADE,
			payload BYTEA
		);`

	if shape != partitionGuardSupported {
		usageLogsDDL = `
		CREATE TABLE usage_logs (
			id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			group_id BIGINT,
			actual_cost NUMERIC(20, 10) NOT NULL DEFAULT 0
		) PARTITION BY RANGE (created_at);
		CREATE UNIQUE INDEX usage_logs_id_created_at_key ON usage_logs (id, created_at);
		CREATE TABLE usage_logs_202604 PARTITION OF usage_logs
			FOR VALUES FROM ('` + partitionGuardAprilStart + `') TO ('2026-05-01');
		CREATE TABLE usage_logs_202605 PARTITION OF usage_logs
			FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');`

		// 分区父表只能承载 (id, created_at) 复合外键。partitionGuardPartitionedNoFK
		// 刻意不带任何外键，用于证明受支持的单列外键在此形态下无法建立。
		valueDetailFK := ""
		diagnosticFK := ""
		if shape == partitionGuardPartitionedCompositeFK {
			valueDetailFK = `,
			FOREIGN KEY (usage_log_id, usage_created_at)
				REFERENCES usage_logs (id, created_at) ON DELETE CASCADE`
			diagnosticFK = `,
			FOREIGN KEY (plain_owner_usage_log_id, plain_owner_created_at)
				REFERENCES usage_logs (id, created_at) ON DELETE CASCADE`
		}
		valueDetailDDL = `
		CREATE TABLE request_audit_value_details (
			id BIGSERIAL PRIMARY KEY,
			usage_log_id BIGINT NOT NULL,
			usage_created_at TIMESTAMPTZ NOT NULL,
			state TEXT NOT NULL,
			storage_format TEXT NOT NULL,
			expires_at TIMESTAMPTZ` + valueDetailFK + `
		);`
		diagnosticDDL = `
		CREATE TABLE error_diagnostic_records (
			diagnostic_id TEXT PRIMARY KEY,
			usage_log_id BIGINT,
			plain_record BOOLEAN NOT NULL DEFAULT FALSE,
			plain_owner_usage_log_id BIGINT,
			plain_owner_created_at TIMESTAMPTZ,
			metadata_expires_at TIMESTAMPTZ NOT NULL` + diagnosticFK + `
		);`
	}
	if shape == partitionGuardSupportedNoFK {
		// 与 partitionGuardSupported 相同的形状，只是部署方把迁移 253/255 的所有权
		// 外键手工去掉了：usage 行的逐行 DELETE 不再级联，DROP 也不再被依赖检查挡住。
		usageLogsDDL = `
		CREATE TABLE usage_logs (
			id BIGINT PRIMARY KEY,
			created_at TIMESTAMPTZ NOT NULL,
			group_id BIGINT,
			actual_cost NUMERIC(20, 10) NOT NULL DEFAULT 0
		);`
		valueDetailDDL = `
		CREATE TABLE request_audit_value_details (
			id BIGSERIAL PRIMARY KEY,
			usage_log_id BIGINT NOT NULL UNIQUE,
			usage_created_at TIMESTAMPTZ NOT NULL,
			state TEXT NOT NULL,
			storage_format TEXT NOT NULL,
			expires_at TIMESTAMPTZ
		);`
		diagnosticDDL = `
		CREATE TABLE error_diagnostic_records (
			diagnostic_id TEXT PRIMARY KEY,
			usage_log_id BIGINT,
			plain_record BOOLEAN NOT NULL DEFAULT FALSE,
			plain_owner_usage_log_id BIGINT,
			plain_owner_created_at TIMESTAMPTZ,
			metadata_expires_at TIMESTAMPTZ NOT NULL
		);`
		traceDDL = `
		CREATE TABLE request_traces (
			id BIGINT PRIMARY KEY,
			usage_log_id BIGINT,
			payload BYTEA
		);`
	}

	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+quoted)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, usageLogsDDL+`
		CREATE TABLE usage_group_rollup_state (
			id SMALLINT PRIMARY KEY,
			closed_before DATE NOT NULL,
			retained_from TIMESTAMPTZ NOT NULL,
			timezone_name TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		INSERT INTO usage_group_rollup_state (id, closed_before, retained_from, timezone_name)
		VALUES (1, DATE '1970-01-01', TIMESTAMPTZ '1970-01-01 00:00:00+00', 'UTC');

		CREATE TABLE usage_group_daily_rollups (
			bucket_date DATE NOT NULL,
			group_id BIGINT NOT NULL,
			actual_cost NUMERIC(20, 10) NOT NULL DEFAULT 0,
			computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (bucket_date, group_id)
		);
	`+valueDetailDDL+diagnosticDDL)
	require.NoError(t, err)
	if shape == partitionGuardSupported {
		_, err = tx.ExecContext(ctx, traceDDL)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	wrapped := sql.OpenDB(searchPathConnector{pool: integrationDB, schema: schema})
	t.Cleanup(func() {
		_ = wrapped.Close()
		_, _ = integrationDB.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+quoted+" CASCADE")
	})

	// 只有 *sql.DB 才会走带门禁的 DROP 分支；*sql.Tx 分支按设计直接失败关闭。
	return newDashboardAggregationRepositoryWithSQL(wrapped), wrapped
}

func seedPartitionGuardUsage(t *testing.T, ctx context.Context, db *sql.DB, id int64, createdAt time.Time) {
	t.Helper()
	_, err := db.ExecContext(ctx, `INSERT INTO usage_logs (id, created_at) VALUES ($1, $2)`, id, createdAt.UTC())
	require.NoError(t, err)
}

func seedPartitionGuardValueDetail(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	usageID int64,
	usageCreatedAt time.Time,
	storageFormat string,
) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
		INSERT INTO request_audit_value_details
			(usage_log_id, usage_created_at, state, storage_format, expires_at)
		VALUES ($1, $2, 'stored', $3, NULL)`,
		usageID, usageCreatedAt.UTC(), storageFormat)
	require.NoError(t, err)
}

func seedPartitionGuardLinkedDiagnostic(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	usageID int64,
	usageCreatedAt time.Time,
) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
		INSERT INTO error_diagnostic_records
			(diagnostic_id, plain_record, plain_owner_usage_log_id, plain_owner_created_at, metadata_expires_at)
		VALUES ($1, TRUE, $2, $3, $4)`,
		partitionGuardDiagnosticID(), usageID, usageCreatedAt.UTC(), usageCreatedAt.UTC().Add(30*24*time.Hour))
	require.NoError(t, err)
}

func seedPartitionGuardUnlinkedDiagnostic(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
		INSERT INTO error_diagnostic_records
			(diagnostic_id, plain_record, metadata_expires_at)
		VALUES ($1, TRUE, $2)`, partitionGuardDiagnosticID(), time.Now().UTC().Add(30*24*time.Hour))
	require.NoError(t, err)
}

// partitionGuardDiagnosticID 生成 32 位 hex 的诊断主键。
func partitionGuardDiagnosticID() string {
	return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff) + strings.Repeat("c", 24)
}

// partitionGuardRowQueryer 同时覆盖 *sql.DB 与 *sql.Tx。
type partitionGuardRowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// partitionGuardValueDetailOrphans 数出「usage 行已不存在但明文值明细还在」的孤儿。
func partitionGuardValueDetailOrphans(t *testing.T, ctx context.Context, q partitionGuardRowQueryer) int64 {
	t.Helper()
	var count int64
	require.NoError(t, q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM request_audit_value_details v
		WHERE v.storage_format = 'plaintext_usage_bound'
		  AND NOT EXISTS (SELECT 1 FROM usage_logs u WHERE u.id = v.usage_log_id)`).Scan(&count))
	return count
}

// partitionGuardDiagnosticOrphans 数出「usage 行已不存在但已关联明文诊断还在」的孤儿。
func partitionGuardDiagnosticOrphans(t *testing.T, ctx context.Context, q partitionGuardRowQueryer) int64 {
	t.Helper()
	var count int64
	require.NoError(t, q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM error_diagnostic_records d
		WHERE d.plain_record AND d.plain_owner_usage_log_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM usage_logs u WHERE u.id = d.plain_owner_usage_log_id)`).Scan(&count))
	return count
}

func partitionGuardRelationExists(t *testing.T, ctx context.Context, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relname = $1 AND n.nspname = current_schema()
		)`, name).Scan(&exists))
	return exists
}

func partitionGuardUsageRows(t *testing.T, ctx context.Context, db *sql.DB, partition string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+pq.QuoteIdentifier(partition)).Scan(&count))
	return count
}

func partitionGuardCount(t *testing.T, ctx context.Context, q partitionGuardRowQueryer, from string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+from).Scan(&count))
	return count
}

// searchPathConnector 把共享集成连接池里的连接借出来，并把 search_path 固定到复刻
// schema，使仓储代码里的未限定表名解析到复刻表。这是让被测代码拿到真正的 *sql.DB
// （而非 *sql.Tx）以便执行分区 DROP 分支的最小手段。
type searchPathConnector struct {
	pool   *sql.DB
	schema string
}

func (c searchPathConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.pool.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "SET search_path TO "+pq.QuoteIdentifier(c.schema)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &searchPathConn{conn: conn}, nil
}

func (c searchPathConnector) Driver() driver.Driver { return searchPathDriver{} }

type searchPathDriver struct{}

func (searchPathDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("searchPathDriver 只能通过 Connector 使用")
}

type searchPathConn struct {
	conn *sql.Conn
}

func (c *searchPathConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("searchPathConn 只支持 Context 形式的执行")
}

// Begin 直接在被借用会话上执行 BEGIN，而不是再套一层 *sql.Tx：仓储拿到的外层是
// *sql.DB，事务与后续语句必须落在同一会话上，显式 BEGIN 最不容易踩
// database/sql 的连接占用规则。
func (c *searchPathConn) Begin() (driver.Tx, error) {
	if _, err := c.conn.ExecContext(context.Background(), "BEGIN"); err != nil {
		return nil, err
	}
	return searchPathTx{conn: c.conn}, nil
}

func (c *searchPathConn) ExecContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	return c.conn.ExecContext(ctx, query, namedValueArgs(args)...)
}

func (c *searchPathConn) QueryContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	rows, err := c.conn.QueryContext(ctx, query, namedValueArgs(args)...)
	if err != nil {
		return nil, err
	}
	columns, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	return &searchPathRows{rows: rows, columns: columns}, nil
}

func (c *searchPathConn) Close() error {
	// 被借用的会话会回到共享连接池继续服务其它测试，必须在归还前清掉 schema 固定，
	// 否则后续测试的未限定表名会解析到即将被 DROP 的复刻 schema。
	_, _ = c.conn.ExecContext(context.Background(), "RESET search_path")
	return c.conn.Close()
}

type searchPathTx struct {
	conn *sql.Conn
}

func (t searchPathTx) Commit() error {
	_, err := t.conn.ExecContext(context.Background(), "COMMIT")
	return err
}

func (t searchPathTx) Rollback() error {
	_, err := t.conn.ExecContext(context.Background(), "ROLLBACK")
	return err
}

type searchPathRows struct {
	rows    *sql.Rows
	columns []string
}

func (r *searchPathRows) Columns() []string { return r.columns }

func (r *searchPathRows) Close() error { return r.rows.Close() }

func (r *searchPathRows) Next(dest []driver.Value) error {
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	// 必须经 *any 中转：database/sql 的 convertAssign 只在 dest 是 *any 时接受 NULL，
	// 而 driver.Value 是定义类型，*driver.Value 不匹配该分支。
	holders := make([]any, len(dest))
	scanned := make([]any, len(dest))
	for i := range holders {
		holders[i] = &scanned[i]
	}
	if err := r.rows.Scan(holders...); err != nil {
		return err
	}
	for i := range dest {
		dest[i] = scanned[i]
	}
	return nil
}

func namedValueArgs(args []driver.NamedValue) []any {
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	return values
}
