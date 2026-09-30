//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const requestTraceExportSnapshotTestDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// 上限快照必须与任务行在同一条 INSERT 里落库，并能从 Get/Claim 原样读回：
// 任务一旦可被认领，它的预算就已经确定，不存在"先排队、后补快照"的窗口。
func TestRequestTraceExportRepositoryPersistsLimitsSnapshotAtomically(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	store := &requestTraceExportRepository{q: tx}
	now := time.Now().UTC()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	snapshot := service.RequestTraceExportLimits{
		MaxRows: 500, MaxBytes: 1 << 20, MaxRuntimeSec: 120,
		MaxShardRows: 100, MaxShardBytes: 1 << 20, Configured: true,
	}
	require.NoError(t, store.Create(ctx, service.RequestTraceExportTask{
		ID: id, Status: service.RequestTraceExportPending,
		AdminUserID: 12, SessionDigest: requestTraceExportSnapshotTestDigest,
		InstanceID: "node-one", CreatedAt: now, LimitsSnapshot: &snapshot,
	}, 1))

	stored, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, stored.LimitsSnapshot)
	require.Equal(t, snapshot, *stored.LimitsSnapshot)

	// 认领时一并带回：执行侧不需要再查一次库就知道了创建时的预算。
	claimed, err := store.Claim(ctx, "node-one")
	require.NoError(t, err)
	require.NotNil(t, claimed.LimitsSnapshot)
	require.Equal(t, snapshot, *claimed.LimitsSnapshot)

	// 完成写回不改变快照：预算是创建时固定的。
	claimed.Status = service.RequestTraceExportCompleted
	claimed.Filename = "sub2api-request-trace-export-" + id + ".jsonl"
	when := now
	claimed.CompletedAt = &when
	until := when.Add(7 * 24 * time.Hour)
	claimed.DownloadUntil = &until
	require.NoError(t, store.Finish(ctx, claimed))
	stored, err = store.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, stored.LimitsSnapshot)
	require.Equal(t, snapshot, *stored.LimitsSnapshot)
}

// 升级前创建的任务没有快照（列为 NULL）：读回来必须是"没有快照"，而不是一份
// 零值上限——零值会被当成"没有限制"。
func TestRequestTraceExportRepositoryAbsentLimitsSnapshotStaysAbsent(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	store := &requestTraceExportRepository{q: tx}
	now := time.Now().UTC()
	id := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	require.NoError(t, store.Create(ctx, service.RequestTraceExportTask{
		ID: id, Status: service.RequestTraceExportPending,
		AdminUserID: 12, SessionDigest: requestTraceExportSnapshotTestDigest,
		InstanceID: "node-one", CreatedAt: now,
	}, 1))
	stored, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Nil(t, stored.LimitsSnapshot)
	claimed, err := store.Claim(ctx, "node-one")
	require.NoError(t, err)
	require.Nil(t, claimed.LimitsSnapshot)
}

// "有值但读不出来"不是"没有快照"：NULL 才允许退回当前配置，损坏的预算必须
// 如实报错，否则执行侧会静默按别的配置跑一个本该有固定预算的任务。
func TestRequestTraceExportRepositoryUndecodableLimitsSnapshotIsSurfaced(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	store := &requestTraceExportRepository{q: tx}
	now := time.Now().UTC()
	id := "cccccccccccccccccccccccccccccccc"
	require.NoError(t, store.Create(ctx, service.RequestTraceExportTask{
		ID: id, Status: service.RequestTraceExportPending,
		AdminUserID: 12, SessionDigest: requestTraceExportSnapshotTestDigest,
		InstanceID: "node-one", CreatedAt: now,
	}, 1))
	// 小数是合法数字、也落在区间内，但读不进 int64 字段。
	_, err := tx.ExecContext(ctx,
		`UPDATE request_trace_exports SET limits_snapshot=$2::jsonb WHERE export_id=$1`,
		id, `{"max_rows":500.5,"max_bytes":1048576,"max_runtime_seconds":60,"max_shard_rows":10,"max_shard_bytes":1048576,"configured":true}`)
	require.NoError(t, err)
	_, err = store.Get(ctx, id)
	require.ErrorIs(t, err, service.ErrRequestTraceExportUnavailable)
	_, err = store.Claim(ctx, "node-one")
	require.ErrorIs(t, err, service.ErrRequestTraceExportUnavailable)
}

// 快照列的形状与取值由数据库兜底：标量、缺字段、越界值都不能落库，而不是被
// 静默存成无法解释的值。事务内的约束违反会中止整个事务，所以每个用例各自
// 用一条注定失败的语句（另见 TestRequestTraceExportLimitsSnapshotTableConstraints）。
func TestRequestTraceExportRepositoryRejectsScalarLimitsSnapshot(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	store := &requestTraceExportRepository{q: tx}
	now := time.Now().UTC()
	id := "dddddddddddddddddddddddddddddddd"
	require.NoError(t, store.Create(ctx, service.RequestTraceExportTask{
		ID: id, Status: service.RequestTraceExportPending,
		AdminUserID: 12, SessionDigest: requestTraceExportSnapshotTestDigest,
		InstanceID: "node-one", CreatedAt: now,
	}, 1))
	_, err := tx.ExecContext(ctx,
		`UPDATE request_trace_exports SET limits_snapshot=$2::jsonb WHERE export_id=$1`,
		id, `"not-an-object"`)
	require.Error(t, err)
}

// 形状/取值护栏逐条验证：缺一个数值字段、越界、单片上限超过整任务上限、
// 出现键集之外的键、值不是对象——都必须被数据库拒绝。
func TestRequestTraceExportLimitsSnapshotTableConstraints(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		snapshot string
	}{
		{"scalar", `"not-an-object"`},
		{"missing_key", `{"max_rows":500,"max_bytes":1048576,"max_runtime_seconds":60,"max_shard_rows":10}`},
		{"unknown_key", `{"max_rows":500,"max_bytes":1048576,"max_runtime_seconds":60,"max_shard_rows":10,"max_shard_bytes":1048576,"extra":1}`},
		{"non_numeric", `{"max_rows":"500","max_bytes":1048576,"max_runtime_seconds":60,"max_shard_rows":10,"max_shard_bytes":1048576}`},
		{"rows_below_floor", `{"max_rows":99,"max_bytes":1048576,"max_runtime_seconds":60,"max_shard_rows":10,"max_shard_bytes":1048576}`},
		{"rows_above_ceiling", `{"max_rows":5000001,"max_bytes":1048576,"max_runtime_seconds":60,"max_shard_rows":10,"max_shard_bytes":1048576}`},
		{"runtime_above_ceiling", `{"max_rows":500,"max_bytes":1048576,"max_runtime_seconds":21601,"max_shard_rows":10,"max_shard_bytes":1048576}`},
		{"bytes_above_ceiling", `{"max_rows":500,"max_bytes":68719476737,"max_runtime_seconds":60,"max_shard_rows":10,"max_shard_bytes":1048576}`},
		{"shard_rows_above_total", `{"max_rows":100,"max_bytes":1048576,"max_runtime_seconds":60,"max_shard_rows":101,"max_shard_bytes":1048576}`},
		{"shard_bytes_above_total", `{"max_rows":500,"max_bytes":1048576,"max_runtime_seconds":60,"max_shard_rows":10,"max_shard_bytes":2097152}`},
		{"configured_not_boolean", `{"max_rows":500,"max_bytes":1048576,"max_runtime_seconds":60,"max_shard_rows":10,"max_shard_bytes":1048576,"configured":"yes"}`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			tx := testTx(t)
			store := &requestTraceExportRepository{q: tx}
			id := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			require.NoError(t, store.Create(ctx, service.RequestTraceExportTask{
				ID: id, Status: service.RequestTraceExportPending,
				AdminUserID: 12, SessionDigest: requestTraceExportSnapshotTestDigest,
				InstanceID: "node-one", CreatedAt: time.Now().UTC(),
			}, 1))
			_, err := tx.ExecContext(ctx,
				`UPDATE request_trace_exports SET limits_snapshot=$2::jsonb WHERE export_id=$1`,
				id, fixture.snapshot)
			require.Error(t, err, "the database must reject %s", fixture.name)
		})
	}
}

// 合法快照（服务端归一化后的形状与取值）必须能落库并原样读回。
func TestRequestTraceExportLimitsSnapshotTableAcceptsNormalizedShape(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	store := &requestTraceExportRepository{q: tx}
	id := "fffffffffffffffffffffffffffffffe"
	snapshot := service.RequestTraceExportLimits{
		MaxRows: 500, MaxBytes: 1 << 20, MaxRuntimeSec: 60,
		MaxShardRows: 10, MaxShardBytes: 1 << 20, Configured: true,
	}
	require.NoError(t, store.Create(ctx, service.RequestTraceExportTask{
		ID: id, Status: service.RequestTraceExportPending,
		AdminUserID: 12, SessionDigest: requestTraceExportSnapshotTestDigest,
		InstanceID: "node-one", CreatedAt: time.Now().UTC(), LimitsSnapshot: &snapshot,
	}, 1))
	stored, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, stored.LimitsSnapshot)
	require.Equal(t, snapshot, *stored.LimitsSnapshot)
}
