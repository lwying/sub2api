//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 本文件保留「数据库形态」的测试夹具：在独立 schema 里复刻受支持与不受支持的
// usage_logs / 明文旁路表形态，供部署探针的集成测试使用。
//
// 历史：这里原本还放着旧的「三类新明文采集部署探针」的集成用例（值明细 + 独立诊断正文 +
// 独立诊断 429 头值）。该探针已随旧值明细／错误诊断采集退役（票据 10），因此本文件只剩夹具。
// 夹具本身**保留**：Trace 部署探针的集成用例仍要用同一批形态，见
// request_trace_support_integration_test.go。
//
// 复刻的形态直接对应「明文可能成为孤儿」的两条现实路径：
//   - 分区 usage_logs：单列所有权外键在分区父表上建不出来（PostgreSQL 要求被引用键在整张
//     分区表上唯一），分区 DROP 又不触发行级外键动作；
//   - 旁路表的所有权外键被手工去掉：usage 行的逐行 DELETE 不再级联。
//
// 每个形态都在独立 schema 里复刻，事务内 SET LOCAL search_path，测试结束回滚（DDL 在
// PostgreSQL 里可回滚），因此不会动到共享表。

// plaintextCaptureSupportDDLTx 把给定 DDL 建在一个固定 search_path 的事务上，返回该事务，
// 供探针直接当只读查询接缝使用。
func plaintextCaptureSupportDDLTx(t *testing.T, ctx context.Context, ddl string) *sql.Tx {
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
	return tx
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
