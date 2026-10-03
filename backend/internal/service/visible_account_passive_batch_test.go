//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

// passiveBatchRepoStub 实现 accountWindowStatsBatchReader，并分别记录批量/单账号调用，
// 用于验证批量被动读取复用已加载账号且不逐账号回表。
type passiveBatchRepoStub struct {
	usageBatchLogRepoStub
	batchCalls  int
	singleCalls int
	stats       map[int64]*usagestats.AccountStats
}

func (r *passiveBatchRepoStub) GetAccountWindowStatsBatch(_ context.Context, _ []int64, _ time.Time) (map[int64]*usagestats.AccountStats, error) {
	r.batchCalls++
	return r.stats, nil
}

func (r *passiveBatchRepoStub) GetAccountWindowStats(_ context.Context, _ int64, _ time.Time) (*usagestats.AccountStats, error) {
	r.singleCalls++
	return &usagestats.AccountStats{}, nil
}

// TestAccountUsageService_GetPassiveUsageBatch_ReusesPreloadedAccounts 验证：
//   - 直接消费调用方已加载的账号对象，不再 GetByID；
//   - 窗口统计走一次批量查询，不逐账号回表，也不外呼/写入；
//   - 非 Anthropic 账号被跳过；窗口统计按账号归位到 FiveHour。
func TestAccountUsageService_GetPassiveUsageBatch_ReusesPreloadedAccounts(t *testing.T) {
	t.Parallel()

	repo := &passiveBatchRepoStub{stats: map[int64]*usagestats.AccountStats{
		10: {Requests: 5, Tokens: 50},
	}}
	svc := &AccountUsageService{usageLogRepo: repo, cache: NewUsageCache()}

	now := time.Now()
	start := now.Add(-time.Hour)
	end := now.Add(time.Hour)
	accounts := []*Account{
		{
			ID: 10, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
			SessionWindowStart: &start, SessionWindowEnd: &end,
			Extra: map[string]any{"session_window_utilization": 0.5},
		},
		{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		// 未采样的 Anthropic 账号：没有 session 窗口也没有 7d 采样，必须整体缺席（未知而非 5h=0）。
		{ID: 12, Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		// 仅有 7d 采样：保留 7d，但不得带合成的假 5h。
		{
			ID: 13, Platform: PlatformAnthropic, Type: AccountTypeSetupToken,
			Extra: map[string]any{
				"passive_usage_7d_utilization": 0.4,
				"passive_usage_7d_reset":       float64(now.Add(48 * time.Hour).Unix()),
			},
		},
	}

	result, err := svc.GetPassiveUsageBatch(context.Background(), accounts)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{10, 13}, keysOfPassiveUsage(result),
		"未采样账号不返回，仅 7d 采样账号保留")
	require.NotContains(t, result, int64(11))
	require.NotContains(t, result, int64(12))

	info := result[10]
	require.NotNil(t, info)
	require.Equal(t, "passive", info.Source)
	require.NotNil(t, info.FiveHour)
	require.NotNil(t, info.FiveHour.WindowStats)
	require.Equal(t, int64(5), info.FiveHour.WindowStats.Requests)

	only7d := result[13]
	require.NotNil(t, only7d)
	require.Nil(t, only7d.FiveHour, "只有 7d 事实时不得带假 5h=0")
	require.NotNil(t, only7d.SevenDay)

	require.Equal(t, 1, repo.batchCalls, "同一窗口起点只批量查询一次")
	require.Zero(t, repo.singleCalls, "批量成功时不得逐账号回表")

	// 第二次调用命中 1 分钟缓存：不再产生任何数据库查询。
	result2, err := svc.GetPassiveUsageBatch(context.Background(), accounts)
	require.NoError(t, err)
	require.Equal(t, 1, repo.batchCalls, "缓存命中不得再次查询")
	require.Equal(t, int64(5), result2[10].FiveHour.WindowStats.Requests)
}

func keysOfPassiveUsage(m map[int64]*UsageInfo) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return out
}
