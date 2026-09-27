//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 票据 10 / ADR 0007：三类新明文采集的部署探针必须问真实系统目录，而不是靠迁移文件推断。
// 本文件用真实 PostgreSQL 复刻受支持与不受支持的库形态，断言探针给出**同一套**结论，
// 因为服务层的采集/开启门控完全建立在这条结论上（合成替身覆盖门控本身，
// 见 internal/service/plaintext_capture_support_test.go）。
//
// 复刻的形态直接对应「明文可能成为孤儿」的两条现实路径：
//   - 分区 usage_logs：单列所有权外键在分区父表上建不出来（PostgreSQL 要求被引用键在整张
//     分区表上唯一），分区 DROP 又不触发行级外键动作；
//   - 旁路表的所有权外键被手工去掉：usage 行的逐行 DELETE 不再级联。
//
// 每个形态都在独立 schema 里复刻，事务内 SET LOCAL search_path，测试结束回滚（DDL 在
// PostgreSQL 里可回滚），因此不会动到共享表。

// plaintextCaptureSupportProbeTx 把探针绑在一个固定 search_path 的事务上。
func plaintextCaptureSupportProbeTx(t *testing.T, ctx context.Context, ddl string) (*plaintextCaptureSupportProbe, *sql.Tx) {
	t.Helper()
	schema := fmt.Sprintf("plaintext_capture_support_%d", time.Now().UnixNano())
	quoted := pq.QuoteIdentifier(schema)

	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	// 回滚即撤销建表与建 schema，测试之间互不影响。
	t.Cleanup(func() { _ = tx.Rollback() })

	_, err = tx.ExecContext(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+quoted)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, ddl)
	require.NoError(t, err)
	return &plaintextCaptureSupportProbe{q: tx}, tx
}

// plaintextCaptureSupportUsageLogsDDL 是普通（非分区）usage_logs 的最小形态。
const plaintextCaptureSupportUsageLogsDDL = `
	CREATE TABLE usage_logs (
		id BIGINT PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL
	);`

// plaintextCaptureSupportPartitionedUsageLogsDDL 是分区 usage_logs 的最小形态：
// 唯一索引必须含分区键，因此单列所有权外键在这张表上无法建立。
const plaintextCaptureSupportPartitionedUsageLogsDDL = `
	CREATE TABLE usage_logs (
		id BIGINT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL
	) PARTITION BY RANGE (created_at);
	CREATE UNIQUE INDEX usage_logs_id_created_at_key ON usage_logs (id, created_at);
	CREATE TABLE usage_logs_202609 PARTITION OF usage_logs
		FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');`

// TestPlaintextCaptureSupportProbeOnMigratedSchema 覆盖真实迁移后的形态：探针必须报告受支持，
// 否则线上永远开启不了三类新明文。它同时是「探针查错表/查错列」的回归哨兵。
func TestPlaintextCaptureSupportProbeOnMigratedSchema(t *testing.T) {
	ctx := context.Background()
	probe := NewPlaintextCaptureSupportProbe(integrationDB)

	support, err := probe.ProbePlaintextCaptureSupport(ctx)
	require.NoError(t, err)
	require.True(t, support.Supported,
		"迁移 253/255 建出的单列所有权外键必须被判为受支持，否则任何部署都无法开启新明文")
	require.Equal(t, service.PlaintextCaptureSupportReasonSupported, support.Reason)
}

// TestPlaintextCaptureSupportProbeRejectsUnsupportedDatabaseShapes 覆盖探针的失败关闭方向：
// 只要「明文能随 usage 消失」得不到证明，就不能报告受支持。
func TestPlaintextCaptureSupportProbeRejectsUnsupportedDatabaseShapes(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		ddl  string
		want string
	}{
		{
			name: "non-partitioned usage_logs with single-column cascade ownership",
			ddl: plaintextCaptureSupportUsageLogsDDL + `
				CREATE TABLE request_audit_value_details (
					id BIGSERIAL PRIMARY KEY,
					usage_log_id BIGINT NOT NULL UNIQUE REFERENCES usage_logs (id) ON DELETE CASCADE
				);
				CREATE TABLE error_diagnostic_records (
					id TEXT PRIMARY KEY,
					usage_log_id BIGINT REFERENCES usage_logs (id) ON DELETE SET NULL,
					plain_owner_usage_log_id BIGINT REFERENCES usage_logs (id) ON DELETE CASCADE
				);`,
			want: service.PlaintextCaptureSupportReasonSupported,
		},
		{
			name: "plaintext sidecars not deployed yet",
			ddl:  plaintextCaptureSupportUsageLogsDDL,
			want: service.PlaintextCaptureSupportReasonSupported,
		},
		{
			name: "partitioned usage_logs cannot carry the ownership key",
			ddl: plaintextCaptureSupportPartitionedUsageLogsDDL + `
				CREATE TABLE request_audit_value_details (
					id BIGSERIAL PRIMARY KEY,
					usage_log_id BIGINT NOT NULL,
					usage_created_at TIMESTAMPTZ NOT NULL
				);
				CREATE TABLE error_diagnostic_records (
					id TEXT PRIMARY KEY,
					plain_owner_usage_log_id BIGINT,
					plain_owner_created_at TIMESTAMPTZ
				);`,
			want: service.PlaintextCaptureSupportReasonPartitionedUsageLogs,
		},
		{
			name: "composite ownership key on a partitioned parent is still partitioned",
			ddl: plaintextCaptureSupportPartitionedUsageLogsDDL + `
				CREATE TABLE request_audit_value_details (
					id BIGSERIAL PRIMARY KEY,
					usage_log_id BIGINT NOT NULL,
					usage_created_at TIMESTAMPTZ NOT NULL,
					FOREIGN KEY (usage_log_id, usage_created_at)
						REFERENCES usage_logs (id, created_at) ON DELETE CASCADE
				);
				CREATE TABLE error_diagnostic_records (
					id TEXT PRIMARY KEY,
					plain_owner_usage_log_id BIGINT,
					plain_owner_created_at TIMESTAMPTZ
				);`,
			want: service.PlaintextCaptureSupportReasonPartitionedUsageLogs,
		},
		{
			name: "ownership foreign key dropped from the value-detail sidecar",
			ddl: plaintextCaptureSupportUsageLogsDDL + `
				CREATE TABLE request_audit_value_details (
					id BIGSERIAL PRIMARY KEY,
					usage_log_id BIGINT NOT NULL UNIQUE
				);
				CREATE TABLE error_diagnostic_records (
					id TEXT PRIMARY KEY,
					plain_owner_usage_log_id BIGINT REFERENCES usage_logs (id) ON DELETE CASCADE
				);`,
			want: service.PlaintextCaptureSupportReasonMissingOwnership,
		},
		{
			name: "ownership foreign key dropped from the diagnostic sidecar",
			ddl: plaintextCaptureSupportUsageLogsDDL + `
				CREATE TABLE request_audit_value_details (
					id BIGSERIAL PRIMARY KEY,
					usage_log_id BIGINT NOT NULL UNIQUE REFERENCES usage_logs (id) ON DELETE CASCADE
				);
				CREATE TABLE error_diagnostic_records (
					id TEXT PRIMARY KEY,
					plain_owner_usage_log_id BIGINT
				);`,
			want: service.PlaintextCaptureSupportReasonMissingOwnership,
		},
		{
			name: "ownership column keeps a foreign key but loses the cascade",
			ddl: plaintextCaptureSupportUsageLogsDDL + `
				CREATE TABLE request_audit_value_details (
					id BIGSERIAL PRIMARY KEY,
					usage_log_id BIGINT NOT NULL UNIQUE REFERENCES usage_logs (id) ON DELETE SET NULL
				);
				CREATE TABLE error_diagnostic_records (
					id TEXT PRIMARY KEY,
					plain_owner_usage_log_id BIGINT REFERENCES usage_logs (id) ON DELETE CASCADE
				);`,
			want: service.PlaintextCaptureSupportReasonMissingOwnership,
			// SET NULL 不是所有权：usage 行被删时明文只是失去关联，仍然留在库里。
		},
		{
			name: "foreign key points at another parent table",
			ddl: plaintextCaptureSupportUsageLogsDDL + `
				CREATE TABLE other_parent (id BIGINT PRIMARY KEY);
				CREATE TABLE request_audit_value_details (
					id BIGSERIAL PRIMARY KEY,
					usage_log_id BIGINT NOT NULL REFERENCES other_parent (id) ON DELETE CASCADE
				);
				CREATE TABLE error_diagnostic_records (
					id TEXT PRIMARY KEY,
					plain_owner_usage_log_id BIGINT REFERENCES usage_logs (id) ON DELETE CASCADE
				);`,
			want: service.PlaintextCaptureSupportReasonMissingOwnership,
		},
		{
			name: "usage_logs missing entirely",
			ddl: `
				CREATE TABLE request_audit_value_details (
					id BIGSERIAL PRIMARY KEY,
					usage_log_id BIGINT
				);`,
			want: service.PlaintextCaptureSupportReasonUnknownDeployment,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe, _ := plaintextCaptureSupportProbeTx(t, ctx, tc.ddl)
			support, err := probe.ProbePlaintextCaptureSupport(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.want, support.Reason)
			require.Equal(t, tc.want == service.PlaintextCaptureSupportReasonSupported, support.Supported)
		})
	}
}

// TestPlaintextCaptureSupportProbeFailsClosedWhenItCannotAsk 锁定「判不出来」与「判出来不支持」
// 是两条不同的路径，但都不允许报告支持：探针没有查询接缝时返回 error（服务层收敛成 probe_failed）。
func TestPlaintextCaptureSupportProbeFailsClosedWhenItCannotAsk(t *testing.T) {
	ctx := context.Background()

	_, err := (&plaintextCaptureSupportProbe{}).ProbePlaintextCaptureSupport(ctx)
	require.Error(t, err, "没有查询接缝时必须报错，而不是猜一个结论")

	closed, err := sql.Open("postgres", "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	_, err = NewPlaintextCaptureSupportProbe(closed).ProbePlaintextCaptureSupport(ctx)
	require.Error(t, err, "连接不可用时必须报错，由服务层收敛成 probe_failed 并 fail closed")
}
