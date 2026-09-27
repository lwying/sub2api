//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 值明细清理的告警必须可运维但不含值：
//   - 稳定事件名 + 原因码 + 计数，运维才能按事件聚合、看出是哪一段卡住；
//   - 绝不输出头名、头值、模型名、标识或原始数据库错误值（约束冲突可能回显涉及的行值）；
//   - 持续积压或反复失败下日志量有上界，被抑制的次数不丢；
//   - 「无法观测」不得被静默当成「没有积压」。

const valueDetailAlertErrorSentinel = "sentinel-raw-db-error-do-not-log"

type valueDetailBacklogRepo struct {
	RequestAuditValueDetailRepository
	clearResult int64
	clearErr    error
	count       int64
	oldest      time.Time
	probeErr    error
	clearCalls  int
	probes      int
}

func (r *valueDetailBacklogRepo) ClearExpiredRequestAuditValueDetails(context.Context, time.Time, int) (int64, error) {
	r.clearCalls++
	return r.clearResult, r.clearErr
}

func (r *valueDetailBacklogRepo) ReadRequestAuditValueDetailCleanupBacklog(context.Context, time.Time) (int64, time.Time, error) {
	r.probes++
	return r.count, r.oldest, r.probeErr
}

// valueDetailNoProbeRepo 有清理能力但没有积压探针：这是装配选择，不是故障。
type valueDetailNoProbeRepo struct {
	RequestAuditValueDetailRepository
	clearResult int64
	clearCalls  int
}

func (r *valueDetailNoProbeRepo) ClearExpiredRequestAuditValueDetails(context.Context, time.Time, int) (int64, error) {
	r.clearCalls++
	return r.clearResult, nil
}

// requireValueDetailCleanupAlertsValueFree 断言告警只带允许的数值字段。
//
// service/env 是日志初始化时注入的基础字段，不是清理服务自己带出来的内容。
func requireValueDetailCleanupAlertsValueFree(t *testing.T, sink *inMemoryLogSink) {
	t.Helper()
	allowed := map[string]bool{
		"audit":                     true,
		"code":                      true,
		"count_since_last_alert":    true,
		"total":                     true,
		"legacy_ciphertext_overdue": true,
		"oldest_overdue_seconds":    true,
		"service":                   true,
		"env":                       true,
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, event := range sink.events {
		if event == nil {
			continue
		}
		require.NotContains(t, event.Message, valueDetailAlertErrorSentinel, "告警不得包含原始数据库错误值")
		for key, value := range event.Fields {
			require.True(t, allowed[key], "告警字段名超出有界集合：%s", key)
			require.NotContains(t, fmt.Sprint(value), valueDetailAlertErrorSentinel, "告警字段不得包含原始数据库错误值")
		}
	}
}

func TestValueDetailCleanupObservesLegacyBacklog(t *testing.T) {
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	repo := &valueDetailBacklogRepo{clearResult: 3, count: 2, oldest: now.Add(-time.Hour)}
	svc := NewRequestAuditValueDetailCleanupService(repo)
	svc.now = func() time.Time { return now }
	cleared, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 3, cleared)
	require.Equal(t, 1, repo.probes)
	require.EqualValues(t, 3, svc.ClearedCount())
}

// TestValueDetailCleanupBacklogAlertIsBoundedAndAggregated 覆盖「落后要能度量，且日志量有上界」：
// 同一间隔内重复跑清理只允许一行积压告警，被抑制的轮数必须聚合到下一次告警里。
func TestValueDetailCleanupBacklogAlertIsBoundedAndAggregated(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	repo := &valueDetailBacklogRepo{count: 2, oldest: now.Add(-time.Hour)}
	svc := NewRequestAuditValueDetailCleanupService(repo)
	svc.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		_, err := svc.RunOnce(context.Background())
		require.NoError(t, err)
	}
	require.Equal(t, 3, repo.probes, "每轮都必须真正探测积压")
	require.Equal(t, 1, len(sink.events), "同一间隔内不得重复输出积压告警")
	require.True(t, sink.ContainsMessageAtLevel(RequestAuditValueDetailAlertCleanupBacklog, "warn"))
	require.True(t, sink.ContainsFieldValue("code", RequestAuditValueDetailAlertCodeBacklog))
	require.True(t, sink.ContainsFieldValue("count_since_last_alert", "1"))
	require.True(t, sink.ContainsFieldValue("legacy_ciphertext_overdue", "2"))
	require.True(t, sink.ContainsFieldValue("oldest_overdue_seconds", "3600"), "最老超期时长必须上报")
	requireValueDetailCleanupAlertsValueFree(t, sink)

	// 间隔到期后重新输出，并带上期间被抑制的 2 次 + 本次 1 次。
	now = now.Add(RequestAuditValueDetailAlertInterval + time.Second)
	_, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, len(sink.events), "间隔到期后必须重新输出")
	require.True(t, sink.ContainsFieldValue("count_since_last_alert", "3"), "被抑制的轮数必须聚合到下一次告警")
	requireValueDetailCleanupAlertsValueFree(t, sink)
}

// TestValueDetailCleanupFailureAlertCarriesReasonCode 覆盖清理失败：
// 必须留下稳定原因码与累计计数，绝不把原始数据库错误值写进日志。
func TestValueDetailCleanupFailureAlertCarriesReasonCode(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	repo := &valueDetailBacklogRepo{clearErr: fmt.Errorf("clear failed: %w", errors.New(valueDetailAlertErrorSentinel))}
	svc := NewRequestAuditValueDetailCleanupService(repo)
	svc.now = func() time.Time { return now }

	cleared, err := svc.RunOnce(context.Background())
	require.Error(t, err, "清理失败必须冒泡")
	require.Zero(t, cleared)

	require.True(t, sink.ContainsMessageAtLevel(RequestAuditValueDetailAlertCleanupFailed, "warn"))
	require.True(t, sink.ContainsFieldValue("code", RequestAuditValueDetailAlertCodeClearFailed))
	require.True(t, sink.ContainsFieldValue("total", "1"))
	require.Zero(t, repo.probes, "清理段已经失败时不再探测积压")
	require.EqualValues(t, 1, svc.FailureCount())
	require.EqualValues(t, 1, svc.Snapshot().CleanupFailures)
	require.False(t, svc.Snapshot().BacklogObserved)
	requireValueDetailCleanupAlertsValueFree(t, sink)
}

// TestValueDetailCleanupProbeFailureIsVisible 覆盖探针自身失败：
// 必须留下原因码，而不是静默地把「无法观测」当成「没有积压」，且不得改变清理结果。
func TestValueDetailCleanupProbeFailureIsVisible(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	repo := &valueDetailBacklogRepo{clearResult: 2, probeErr: errors.New("probe failed: " + valueDetailAlertErrorSentinel)}
	svc := NewRequestAuditValueDetailCleanupService(repo)
	svc.now = func() time.Time { return now }

	cleared, err := svc.RunOnce(context.Background())
	require.NoError(t, err, "探针失败不得改变清理结果")
	require.EqualValues(t, 2, cleared)

	require.True(t, sink.ContainsMessageAtLevel(RequestAuditValueDetailAlertBacklogProbeFailed, "warn"))
	require.True(t, sink.ContainsFieldValue("code", RequestAuditValueDetailAlertCodeBacklogProbeFailed))
	requireValueDetailCleanupAlertsValueFree(t, sink)

	snapshot := svc.Snapshot()
	require.False(t, snapshot.BacklogObserved, "探针失败必须表现为无法观测，而不是零积压")
	require.EqualValues(t, 1, snapshot.CleanupFailures, "探针失败是失败，必须计数")
	require.EqualValues(t, 2, snapshot.ClearedRows, "探针失败不得掩盖已经发生的物理清除")
}

// TestValueDetailCleanupSnapshotReportsBacklog 覆盖无日志采集的部署也能读到积压：
// 计数快照必须给出待清理数与最老超期时长，且与失败计数分开。
func TestValueDetailCleanupSnapshotReportsBacklog(t *testing.T) {
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	repo := &valueDetailBacklogRepo{clearResult: 3, count: 4, oldest: now.Add(-90 * time.Minute)}
	svc := NewRequestAuditValueDetailCleanupService(repo)
	svc.now = func() time.Time { return now }

	_, err := svc.RunOnce(context.Background())
	require.NoError(t, err)

	snapshot := svc.Snapshot()
	require.EqualValues(t, 3, snapshot.ClearedRows)
	require.EqualValues(t, 4, snapshot.BacklogRows)
	require.EqualValues(t, 5400, snapshot.OldestOverdueSeconds)
	require.True(t, snapshot.BacklogObserved)
	require.Zero(t, snapshot.CleanupFailures, "积压不是失败，两个信号不得混用")

	// 积压清空后必须如实归零，而不是停留在上一轮读数。
	repo.count = 0
	repo.oldest = time.Time{}
	_, err = svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, svc.Snapshot().BacklogRows)
	require.Zero(t, svc.Snapshot().OldestOverdueSeconds)
	require.True(t, svc.Snapshot().BacklogObserved)
}

// TestValueDetailCleanupWithoutBacklogReaderNeverFakesZeroBacklog 覆盖存储层未提供探针时：
// 清理照常执行，不告警，也不用 0 冒充「没有积压」。
func TestValueDetailCleanupWithoutBacklogReaderNeverFakesZeroBacklog(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	repo := &valueDetailNoProbeRepo{clearResult: 5}
	svc := NewRequestAuditValueDetailCleanupService(repo)

	cleared, err := svc.RunOnce(context.Background())
	require.NoError(t, err, "缺少积压观测不得让清理失败")
	require.EqualValues(t, 5, cleared)
	require.Equal(t, 1, repo.clearCalls)
	require.Empty(t, sink.events, "缺少观测能力不是故障，不得产生告警")

	snapshot := svc.Snapshot()
	require.False(t, snapshot.BacklogObserved, "无法观测必须与零积压可区分")
	require.Zero(t, snapshot.BacklogRows)
	require.EqualValues(t, 5, snapshot.ClearedRows)
	require.Zero(t, snapshot.CleanupFailures, "缺少探针不是失败")
}
