package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
)

func TestRequestLogRetention_RuntimePolicy(t *testing.T) {
	for _, tt := range []struct {
		name        string
		raw         string
		enabled     bool
		wantDays    int
		wantCleanup bool
	}{
		{"legacy uses deployment", `{"retention_days":30}`, true, 90, true},
		{"short window", `{"request_retention_days":7}`, true, 7, true},
		{"long window extends dedup", `{"request_retention_days":730}`, true, 730, true},
		{"forever skips request and dedup deletion", `{"request_retention_days":0}`, true, 0, false},
		{"works without aggregation", `{"request_retention_days":14}`, false, 14, true},
		{"legacy disabled stays disabled", `{"retention_days":30}`, false, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			settings := newRuntimeSettingRepoStub()
			settings.values[SettingKeyOpsRuntimeLogConfig] = tt.raw
			repo := &dashboardAggregationRepoTestStub{}
			svc := NewDashboardAggregationService(repo, nil, &config.Config{DashboardAgg: config.DashboardAggregationConfig{
				Enabled:   tt.enabled,
				Retention: config.DashboardAggregationRetentionConfig{UsageLogsDays: 90, UsageBillingDedupDays: 365, HourlyDays: 180, DailyDays: 730},
			}})
			svc.settingRepo = settings
			now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			svc.maybeCleanupRetention(context.Background(), now)
			if tt.wantCleanup {
				require.Equal(t, 1, repo.cleanupUsageCalls)
				require.Equal(t, now.AddDate(0, 0, -tt.wantDays), repo.cleanupUsageCutoff)
				require.Equal(t, now.AddDate(0, 0, -max(365, tt.wantDays)), repo.cleanupDedupCutoff)
			} else {
				require.Zero(t, repo.cleanupUsageCalls)
				require.Zero(t, repo.cleanupDedupCalls)
			}
			if !tt.enabled {
				require.Zero(t, repo.cleanupAggregateCalls)
			}
			// Read the updated policy at the next rolling cleanup.
			settings.values[SettingKeyOpsRuntimeLogConfig] = `{"request_retention_days":180}`
			next := now.Add(dashboardAggregationRetentionInterval)
			svc.maybeCleanupRetention(context.Background(), next)
			require.Equal(t, next.AddDate(0, 0, -180), repo.cleanupUsageCutoff)
		})
	}
}

// mock 事件没有 usage 外键，所以它的清理必须动态跟随当次使用记录保留策略：
// 策略生效时用同一 cutoff 清理；策略被停用（0 天 = 永久保留）时不得再用任何兜底期限
// 删除，否则会违背"使用记录自动清理关闭时此类事件也不自动清理"的已确认行为。
func TestRequestLogRetention_MockEventsFollowUsagePolicy(t *testing.T) {
	for _, tt := range []struct {
		name        string
		enabled     bool
		raw         string
		wantDeleted bool
		wantDays    int
	}{
		{"cleans with the usage cutoff", true, `{"request_retention_days":30}`, true, 30},
		{"forever keeps mock events", true, `{"request_retention_days":0}`, false, 0},
		{"aggregation disabled keeps mock events", false, `{"retention_days":30}`, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			settings := newRuntimeSettingRepoStub()
			settings.values[SettingKeyOpsRuntimeLogConfig] = tt.raw
			repo := &dashboardAggregationRepoTestStub{}
			svc := NewDashboardAggregationService(repo, nil, &config.Config{DashboardAgg: config.DashboardAggregationConfig{
				Enabled:   tt.enabled,
				Retention: config.DashboardAggregationRetentionConfig{UsageLogsDays: 90, UsageBillingDedupDays: 365, HourlyDays: 180, DailyDays: 730},
			}})
			svc.settingRepo = settings
			var mockCutoffs []time.Time
			svc.SetGatewayMockEventCleaner(func(_ context.Context, cutoff time.Time, _ int) (int64, error) {
				mockCutoffs = append(mockCutoffs, cutoff)
				return 0, nil
			})

			now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			svc.maybeCleanupRetention(context.Background(), now)

			if tt.wantDeleted {
				require.Equal(t, []time.Time{now.AddDate(0, 0, -tt.wantDays)}, mockCutoffs)
			} else {
				require.Empty(t, mockCutoffs, "使用记录自动清理关闭时不得删除 mock 事件")
			}
		})
	}
}

func TestRequestLogRetention_ReadFailureSkipsDeletion(t *testing.T) {
	for _, raw := range []string{`{broken`, `{"request_retention_days":-1}`, `{"request_retention_days":3651}`} {
		settings := newRuntimeSettingRepoStub()
		settings.values[SettingKeyOpsRuntimeLogConfig] = raw
		repo := &dashboardAggregationRepoTestStub{}
		svc := &DashboardAggregationService{repo: repo, settingRepo: settings}
		var mockCleanerCalls int
		svc.SetGatewayMockEventCleaner(func(context.Context, time.Time, int) (int64, error) {
			mockCleanerCalls++
			return 0, nil
		})
		svc.runScheduledRetention()
		require.Zero(t, repo.cleanupUsageCalls)
		require.Zero(t, repo.cleanupDedupCalls)
		// 读设置失败时同样不能用兜底期限删 mock 事件。
		require.Zero(t, mockCleanerCalls)
		require.Nil(t, svc.lastRetentionCleanup.Load())
	}
	settings := newRuntimeSettingRepoStub()
	settings.getValueFn = func(string) (string, error) { return "", errors.New("database unavailable") }
	svc := &DashboardAggregationService{repo: &dashboardAggregationRepoTestStub{}, settingRepo: settings}
	svc.runScheduledRetention()
	require.Nil(t, svc.lastRetentionCleanup.Load())
}

func TestRuntimeLogConfig_RequestRetentionCompatibility(t *testing.T) {
	settings := newRuntimeSettingRepoStub()
	settings.values[SettingKeyOpsRuntimeLogConfig] = `{"level":"info","retention_days":7,"request_retention_days":180}`
	svc := &OpsService{settingRepo: settings}
	require.NoError(t, logger.Init(logger.InitOptions{Level: "info", Format: "json", Output: logger.OutputOptions{ToStdout: true}}))
	t.Cleanup(logger.Sync)
	legacy := defaultOpsRuntimeLogConfig(nil)
	legacy.RequestRetentionDays = nil
	updated, err := svc.UpdateRuntimeLogConfig(context.Background(), legacy, 1)
	require.NoError(t, err)
	require.Equal(t, 180, *updated.RequestRetentionDays)
	for _, days := range []int{0, 1, 3650} {
		updated.RequestRetentionDays = &days
		saved, err := svc.UpdateRuntimeLogConfig(context.Background(), updated, 1)
		require.NoError(t, err)
		require.Equal(t, days, *saved.RequestRetentionDays)
		var persisted OpsRuntimeLogConfig
		require.NoError(t, json.Unmarshal([]byte(settings.values[SettingKeyOpsRuntimeLogConfig]), &persisted))
		require.Equal(t, days, *persisted.RequestRetentionDays)
	}
	for _, days := range []int{-1, 3651} {
		updated.RequestRetentionDays = &days
		before := settings.values[SettingKeyOpsRuntimeLogConfig]
		_, err := svc.UpdateRuntimeLogConfig(context.Background(), updated, 1)
		require.ErrorContains(t, err, "request_retention_days")
		require.Equal(t, before, settings.values[SettingKeyOpsRuntimeLogConfig])
	}
	reset, err := svc.ResetRuntimeLogConfig(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, 90, *reset.RequestRetentionDays)
}
