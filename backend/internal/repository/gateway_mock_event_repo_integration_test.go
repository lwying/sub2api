//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 真实迁移（263，未改动）建出的 gateway_mock_events：清理按"当次"使用记录保留策略算出
// 的 cutoff 与 occurred_at 比较。写入时记下的 cleanup_after 只是 legacy 内部字段，
// 既不能把清理拖长，也不能让已经早于 cutoff 的记录因为期限在未来而留下。
// 这里刻意让被测记录用上该列原本的 730 天默认值：它是最保守的"期限在未来"。
func TestGatewayMockEventCleanupFollowsCurrentRetentionPolicy(t *testing.T) {
	ctx := context.Background()
	repo := NewGatewayMockEventRepo(integrationDB)
	require.NotNil(t, repo)

	// 本测试自己的记录用唯一 rule_id 前缀标识，结束后按前缀清掉，不动其它数据。
	prefix := fmt.Sprintf("gmr_it_cleanup_%d_", time.Now().UnixNano())
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(),
			`DELETE FROM gateway_mock_events WHERE rule_id LIKE $1`, prefix+"%")
		require.NoError(t, err)
	})

	now := time.Now().UTC().Truncate(time.Second)
	// cleanup_after 一律省略：走发布版本的 NOT NULL 默认值（now + 730 天）。
	insert := func(suffix string, occurredAt time.Time) {
		t.Helper()
		_, err := integrationDB.ExecContext(ctx, `
			INSERT INTO gateway_mock_events (occurred_at, rule_id, protocol)
			VALUES ($1, $2, 'messages')`, occurredAt, prefix+suffix)
		require.NoError(t, err)
	}
	count := func() int64 {
		t.Helper()
		var total int64
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM gateway_mock_events WHERE rule_id LIKE $1`, prefix+"%").Scan(&total))
		return total
	}
	legacyDeadlineIsFuture := func() bool {
		t.Helper()
		var soonest time.Time
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
			SELECT MIN(cleanup_after) FROM gateway_mock_events WHERE rule_id LIKE $1`, prefix+"%").Scan(&soonest))
		return soonest.After(now)
	}

	insert("lagged", now.AddDate(0, 0, -40))   // 早于 cutoff，但期限在很远的未来
	insert("older", now.AddDate(0, 0, -35))    // 更早，用来验证批量上限
	insert("boundary", now.AddDate(0, 0, -30)) // 正好落在 cutoff 上：边界包含
	insert("young", now.AddDate(0, 0, -10))    // 比 cutoff 新：必须留下

	require.True(t, legacyDeadlineIsFuture(), "the fixture keeps the legacy future deadline")

	cutoff := now.AddDate(0, 0, -30)

	deleted, err := repo.DeleteMockEventsBefore(ctx, cutoff, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted, "a bounded batch must not exceed its limit")
	require.Equal(t, int64(2), count())

	deleted, err = repo.DeleteMockEventsBefore(ctx, cutoff, 2)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	require.Equal(t, int64(1), count(), "a hit newer than the cutoff must survive its future deadline")

	// 策略调短：已存在的记录下一轮就被清掉，不必等任何写入时期限。
	deleted, err = repo.DeleteMockEventsBefore(ctx, now.AddDate(0, 0, -7), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	require.Zero(t, count())
}

// 发布版本的列形状必须保持不变：NOT NULL、有默认值，旧实例会把它读成 time.Time。
// 本进程的写入也必须给它一个合法时间戳，而不是 NULL。
func TestGatewayMockEventLegacyDeadlineColumnStaysWritable(t *testing.T) {
	ctx := context.Background()
	prefix := fmt.Sprintf("gmr_it_legacy_%d_", time.Now().UnixNano())
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(),
			`DELETE FROM gateway_mock_events WHERE rule_id LIKE $1`, prefix+"%")
		require.NoError(t, err)
	})

	var nullable string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'gateway_mock_events' AND column_name = 'cleanup_after'`).
		Scan(&nullable))
	require.Equal(t, "NO", nullable, "the legacy column must stay NOT NULL for old readers")

	// 本进程的写入路径：写入后该列必须是合法时间戳，且没有一行是 NULL。
	repo := NewGatewayMockEventRepo(integrationDB)
	require.NoError(t, repo.RecordGatewayMockEvent(ctx, service.GatewayMockEventInput{
		RuleID: prefix + "store", Protocol: "responses",
	}))

	var stored time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT cleanup_after FROM gateway_mock_events WHERE rule_id = $1`, prefix+"store").Scan(&stored))
	require.False(t, stored.IsZero(), "a zero value would be a 0001-01-01 date to old readers")

	var nulls int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gateway_mock_events WHERE rule_id LIKE $1 AND cleanup_after IS NULL`,
		prefix+"%").Scan(&nulls))
	require.Zero(t, nulls)

	// 显式写 NULL 必须被列约束拒绝：如果它被接受，旧实例读到该行就会失败。
	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO gateway_mock_events (rule_id, protocol, cleanup_after)
		VALUES ($1, 'messages', NULL)`, prefix+"null")
	require.Error(t, err, "the NOT NULL column must reject a NULL write")
}
