//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 诊断故障的告警必须可运维但不含敏感内容：
//   - 稳定事件名 + 原因码 + 计数，便于按事件聚合告警；
//   - 绝不输出模型正文、凭据、诊断标识或数据库错误值（约束冲突可能回显涉及的行值）；
//   - 上游错误风暴下日志量有上界，被抑制的次数不丢。

const (
	alertTestBodySentinel  = "sentinel-outbound-body-do-not-log"
	alertTestErrorSentinel = "sentinel-raw-db-error-do-not-log"
)

// errorDiagnosticFailingRepo 让写入恒定失败，且错误文本里带一个 sentinel，
// 用来证明原始错误值不会进入日志。
type errorDiagnosticFailingRepo struct {
	ErrorDiagnosticRepository
	err error
}

func (r *errorDiagnosticFailingRepo) CreateErrorDiagnostic(context.Context, ErrorDiagnosticWrite, time.Time) (ErrorDiagnosticRecord, error) {
	return ErrorDiagnosticRecord{}, r.err
}

func newErrorDiagnosticAlertTestService(t *testing.T, repo ErrorDiagnosticRepository, nowFn func() time.Time) *ErrorDiagnosticService {
	t.Helper()
	svc := NewErrorDiagnosticService(repo, errorDiagnosticSettingsFixed{settings: enabledErrorDiagnostics()}, &errorDiagnosticCipherFake{version: 1})
	require.NotNil(t, svc)
	// 用可控时钟替换限速时钟，使速率限制与聚合计数可确定地断言。
	svc.alerts = newErrorDiagnosticAlerter(ErrorDiagnosticAlertInterval, nowFn)
	svc.now = nowFn
	return svc
}

func alertTestAttempt() ErrorDiagnosticAttempt {
	return ErrorDiagnosticAttempt{
		Protocol:           ErrorDiagnosticProtocolMessages,
		Stage:              ErrorDiagnosticStageWire,
		UpstreamStatusCode: 500,
		Body:               []byte(`{"model":"claude","messages":[{"role":"user","content":"` + alertTestBodySentinel + `"}]}`),
		BodyReadComplete:   true,
		BodyVerdict:        ErrorDiagnosticBodyVerdictComplete,
	}
}

// errorDiagnosticSinkScan 汇总捕获到的日志事件，用于断言「什么没有出现」。
func errorDiagnosticSinkScan(sink *inMemoryLogSink) (messages []string, fieldKeys []string, fieldValues []string) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, event := range sink.events {
		messages = append(messages, event.Message)
		for key, value := range event.Fields {
			fieldKeys = append(fieldKeys, key)
			fieldValues = append(fieldValues, fmt.Sprint(value))
		}
	}
	return messages, fieldKeys, fieldValues
}

func requireNoSensitiveDiagnosticLogContent(t *testing.T, sink *inMemoryLogSink) {
	t.Helper()
	messages, keys, values := errorDiagnosticSinkScan(sink)
	for _, message := range messages {
		require.NotContains(t, message, alertTestBodySentinel, "告警不得包含正文")
		require.NotContains(t, message, alertTestErrorSentinel, "告警不得包含原始数据库错误值")
	}
	for _, value := range values {
		require.NotContains(t, value, alertTestBodySentinel, "告警字段不得包含正文")
		require.NotContains(t, value, alertTestErrorSentinel, "告警字段不得包含原始数据库错误值")
	}
	// 字段名本身也不得暗示正文／凭据／错误原文。
	for _, key := range keys {
		lower := strings.ToLower(key)
		for _, forbidden := range []string{"body", "err", "token", "secret", "credential", "message"} {
			require.NotContains(t, lower, forbidden, "告警字段名不得暗示敏感内容：%s", key)
		}
	}
}

func TestErrorDiagnosticWriteFailureAlertsAreBodyFreeAndCounted(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	repo := &errorDiagnosticFailingRepo{err: fmt.Errorf("insert failed: %w", errors.New(alertTestErrorSentinel))}
	svc := newErrorDiagnosticAlertTestService(t, repo, func() time.Time { return now })

	_, err := svc.RecordErrorDiagnostic(context.Background(), alertTestAttempt())
	require.Error(t, err, "写入失败必须对调用方可见")
	require.EqualValues(t, 1, svc.Counters().WriteFailures)
	require.EqualValues(t, 0, svc.Counters().StoredRecords, "写入失败不得计入已落库")

	// 一条稳定事件，带原因码与计数；正文与错误原文都不出现。
	require.True(t, sink.ContainsMessageAtLevel(ErrorDiagnosticAlertWriteFailed, "warn"), "必须输出稳定事件名")
	require.True(t, sink.ContainsFieldValue("code", ErrorDiagnosticAlertCodeDBError))
	require.True(t, sink.ContainsFieldValue("count_since_last_alert", "1"))
	require.True(t, sink.ContainsFieldValue("total", "1"))
	requireNoSensitiveDiagnosticLogContent(t, sink)

	// 限速：同一间隔内的后续失败只累加，不再新开日志行。
	for i := 0; i < 4; i++ {
		_, err := svc.RecordErrorDiagnostic(context.Background(), alertTestAttempt())
		require.Error(t, err)
	}
	require.EqualValues(t, 5, svc.Counters().WriteFailures)
	require.Equal(t, 1, len(sink.events), "同一间隔内不得重复输出告警")
	requireNoSensitiveDiagnosticLogContent(t, sink)

	// 间隔到期后输出一条，并带上被抑制的累计次数（4 次被抑制 + 本次 1 次）。
	now = now.Add(ErrorDiagnosticAlertInterval + time.Second)
	_, err = svc.RecordErrorDiagnostic(context.Background(), alertTestAttempt())
	require.Error(t, err)
	require.Equal(t, 2, len(sink.events), "间隔到期后必须重新输出")
	require.True(t, sink.ContainsFieldValue("count_since_last_alert", "5"), "被抑制的次数必须聚合到下一次告警")
	require.True(t, sink.ContainsFieldValue("total", "6"))
	requireNoSensitiveDiagnosticLogContent(t, sink)
}

func TestErrorDiagnosticDropAlertsAreBodyFreeAndRateLimited(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	svc := newErrorDiagnosticAlertTestService(t, newErrorDiagnosticRepoFake(), func() time.Time { return now })

	for i := 0; i < 7; i++ {
		svc.RecordDroppedErrorDiagnostic()
	}
	require.EqualValues(t, 7, svc.Counters().Dropped)
	require.Equal(t, 1, len(sink.events), "丢弃告警必须限速，不得每次丢弃都写日志")
	require.True(t, sink.ContainsMessageAtLevel(ErrorDiagnosticAlertDropped, "warn"))
	require.True(t, sink.ContainsFieldValue("code", ErrorDiagnosticAlertCodeQueueOverflow))
	requireNoSensitiveDiagnosticLogContent(t, sink)

	now = now.Add(ErrorDiagnosticAlertInterval + time.Second)
	svc.RecordDroppedErrorDiagnostic()
	require.Equal(t, 2, len(sink.events))
	require.True(t, sink.ContainsFieldValue("count_since_last_alert", "7"), "被抑制的丢弃次数必须聚合")
	require.True(t, sink.ContainsFieldValue("total", "8"))
	requireNoSensitiveDiagnosticLogContent(t, sink)

	// 丢弃不是写入失败，两个计数不得混用。
	require.Zero(t, svc.Counters().WriteFailures)
}

// errorDiagnosticStageRecordingRepo 记录两段清理各自的调用次数，并可按需注入失败，
// 用来证明失败不会让另一段被跳过。错误文本带 sentinel，证明原始错误不进日志。
type errorDiagnosticStageRecordingRepo struct {
	ErrorDiagnosticRepository

	bodyClearResult    int64
	bodyClearErr       error
	recordDeleteResult int64
	recordDeleteErr    error

	bodyClearCalls    int
	recordDeleteCalls int
}

func (r *errorDiagnosticStageRecordingRepo) ClearExpiredErrorDiagnosticBodies(context.Context, time.Time, int) (int64, error) {
	r.bodyClearCalls++
	return r.bodyClearResult, r.bodyClearErr
}

func (r *errorDiagnosticStageRecordingRepo) DeleteExpiredErrorDiagnostics(context.Context, time.Time, int) (int64, error) {
	r.recordDeleteCalls++
	return r.recordDeleteResult, r.recordDeleteErr
}

// TestErrorDiagnosticCleanupStagesAreDecoupled 覆盖两段清理互不阻塞：
// 正文清除失败时，第 30 天的整行删除仍必须被真正尝试——
// 否则一条清理不掉的正文会让元数据无限期留在在线主库上，直接违反 30 天上限。
func TestErrorDiagnosticCleanupStagesAreDecoupled(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	repo := &errorDiagnosticStageRecordingRepo{
		bodyClearErr:    errors.New(alertTestErrorSentinel),
		recordDeleteErr: errors.New(alertTestErrorSentinel),
	}
	cleanup := NewErrorDiagnosticCleanupService(repo, NewErrorDiagnosticMetrics())

	_, _, err := cleanup.RunOnce(context.Background())
	require.Error(t, err, "清理失败必须冒泡")

	require.Equal(t, 1, repo.bodyClearCalls, "正文清除必须被调用")
	require.Equal(t, 1, repo.recordDeleteCalls, "正文清除失败后，整行删除仍必须被尝试")
	require.EqualValues(t, 2, cleanup.Counters().CleanupFailures, "两段失败各自计数")

	// 两段各自的稳定原因码都要出现，运维才能区分是哪一段坏了。
	require.True(t, sink.ContainsMessageAtLevel(ErrorDiagnosticAlertCleanupFailed, "warn"))
	require.True(t, sink.ContainsFieldValue("code", ErrorDiagnosticAlertCodeBodyClearFailed))
	require.True(t, sink.ContainsFieldValue("code", ErrorDiagnosticAlertCodeRecordDeleteFailed))
	requireNoSensitiveDiagnosticLogContent(t, sink)
}

// TestErrorDiagnosticCleanupDeleteFailureStillReportsClearedBodies 覆盖
// 「删除整行失败时不得隐瞒已经发生的正文物理清除」。
func TestErrorDiagnosticCleanupDeleteFailureStillReportsClearedBodies(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	repo := &errorDiagnosticStageRecordingRepo{bodyClearResult: 3, recordDeleteErr: errors.New(alertTestErrorSentinel)}
	cleanup := NewErrorDiagnosticCleanupService(repo, NewErrorDiagnosticMetrics())

	cleared, deleted, err := cleanup.RunOnce(context.Background())
	require.Error(t, err)
	require.EqualValues(t, 3, cleared, "已经物理清除的正文数量必须如实返回")
	require.Zero(t, deleted)
	require.EqualValues(t, 1, cleanup.Counters().CleanupFailures)
	require.True(t, sink.ContainsFieldValue("code", ErrorDiagnosticAlertCodeRecordDeleteFailed))
	requireNoSensitiveDiagnosticLogContent(t, sink)
}

// TestErrorDiagnosticCleanupDeleteFailureUsesItsOwnCode 覆盖「删除整行」失败的独立原因码，
// 使运维能区分正文清除失败与元数据删除失败。

// TestErrorDiagnosticAlerterConcurrentUseIsBounded 覆盖并发丢弃下的上界：
// 无论多少并发，同一间隔内最多输出一行，且计数不丢。
func TestErrorDiagnosticAlerterConcurrentUseIsBounded(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	svc := newErrorDiagnosticAlertTestService(t, newErrorDiagnosticRepoFake(), func() time.Time { return now })

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.RecordDroppedErrorDiagnostic()
		}()
	}
	wg.Wait()

	require.EqualValues(t, 32, svc.Counters().Dropped)
	require.Equal(t, 1, len(sink.events), "并发丢弃在同一间隔内只允许一行告警")
	requireNoSensitiveDiagnosticLogContent(t, sink)
}

// TestErrorDiagnosticCleanupReportsBacklog 覆盖「落后要能度量」：
// 清理积压必须上报数量与最老超期时长，且这些字段仍不含正文、凭据或诊断标识。
func TestErrorDiagnosticCleanupReportsBacklog(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	repo := newErrorDiagnosticRepoFake()
	repo.backlog = ErrorDiagnosticCleanupBacklog{
		BodiesOverdue:         4,
		RecordsOverdue:        2,
		OldestRecordOverdueAt: now.Add(-90 * time.Minute),
	}
	cleanup := NewErrorDiagnosticCleanupService(repo, NewErrorDiagnosticMetrics())
	cleanup.now = func() time.Time { return now }

	bodiesCleared, recordsDeleted, err := cleanup.RunOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, bodiesCleared)
	require.Zero(t, recordsDeleted)

	require.True(t, sink.ContainsMessageAtLevel(ErrorDiagnosticAlertCleanupBacklog, "warn"))
	require.True(t, sink.ContainsFieldValue("code", ErrorDiagnosticAlertCodeBacklog))
	require.True(t, sink.ContainsFieldValue("bodies_overdue", "4"))
	require.True(t, sink.ContainsFieldValue("records_overdue", "2"))
	require.True(t, sink.ContainsFieldValue("oldest_overdue_seconds", "5400"), "最老超期时长必须上报")
	requireNoSensitiveDiagnosticLogContent(t, sink)

	// 计数快照同样暴露积压，供没有日志采集的部署读取。
	snapshot := cleanup.Counters()
	require.EqualValues(t, 4, snapshot.OverdueBodies)
	require.EqualValues(t, 2, snapshot.OverdueRecords)
	require.EqualValues(t, 5400, snapshot.OldestOverdueSeconds)
	require.Zero(t, snapshot.CleanupFailures, "积压不是失败，两个信号不得混用")
}

// TestErrorDiagnosticCleanupBacklogProbeFailureIsReported 覆盖探针自身失败：
// 必须留下原因码，而不是静默地把「无法观测」当成「没有积压」。
func TestErrorDiagnosticCleanupBacklogProbeFailureIsReported(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	repo := newErrorDiagnosticRepoFake()
	repo.backlogErr = errors.New("probe failed: " + alertTestErrorSentinel)
	cleanup := NewErrorDiagnosticCleanupService(repo, NewErrorDiagnosticMetrics())

	_, _, err := cleanup.RunOnce(context.Background())
	require.NoError(t, err, "探针失败不得改变清理结果")
	require.True(t, sink.ContainsFieldValue("code", ErrorDiagnosticAlertCodeBacklogProbeFailed))
	requireNoSensitiveDiagnosticLogContent(t, sink)

	// 无法观测与「积压为 0」必须可区分。
	_, err = cleanup.Backlog(context.Background())
	require.Error(t, err)
}

// TestErrorDiagnosticCleanupWithoutBacklogReaderStillRuns 覆盖存储层未提供积压观测时：
// 清理照常执行，不告警、不伪报 0，查询接口给出明确的不支持错误。
func TestErrorDiagnosticCleanupWithoutBacklogReaderStillRuns(t *testing.T) {
	sink, release := captureStructuredLog(t)
	defer release()

	repo := &errorDiagnosticStageRecordingRepo{bodyClearResult: 3, recordDeleteResult: 4}
	cleanup := NewErrorDiagnosticCleanupService(repo, NewErrorDiagnosticMetrics())

	cleared, deleted, err := cleanup.RunOnce(context.Background())
	require.NoError(t, err, "缺少积压观测不得让清理失败")
	require.EqualValues(t, 3, cleared)
	require.EqualValues(t, 4, deleted)
	require.Empty(t, sink.events, "缺少观测能力不是故障，不得产生告警")

	_, err = cleanup.Backlog(context.Background())
	require.ErrorIs(t, err, ErrErrorDiagnosticBacklogUnsupported)
}

// TestErrorDiagnosticCleanupStartRunsAnImmediateCatchUp 覆盖启动即追赶：
// 停机或积压之后重启，必须在进入 10 分钟周期之前先跑一轮，而不是等第一个 tick。
func TestErrorDiagnosticCleanupStartRunsAnImmediateCatchUp(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	cleanup := NewErrorDiagnosticCleanupService(repo, NewErrorDiagnosticMetrics())
	// 把间隔放大到远超测试时长，确保观察到的调用只可能来自启动追赶。
	cleanup.interval = time.Hour
	t.Cleanup(cleanup.Stop)

	cleanup.Start()
	require.Eventually(t, func() bool {
		return repo.clearCalls > 0 && repo.deleteCalls > 0
	}, 5*time.Second, 5*time.Millisecond, "Start 必须立刻跑一轮清理（含第 30 天删除）")
}

// TestErrorDiagnosticCleanupStopIsIdempotent 覆盖生命周期：重复停止不得 panic 或死锁。
func TestErrorDiagnosticCleanupStopIsIdempotent(t *testing.T) {
	cleanup := NewErrorDiagnosticCleanupService(newErrorDiagnosticRepoFake(), NewErrorDiagnosticMetrics())
	cleanup.interval = time.Hour
	cleanup.Start()
	cleanup.Start()
	require.NotPanics(t, func() {
		cleanup.Stop()
		cleanup.Stop()
	})
}
