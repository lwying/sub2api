//go:build unit

package service

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/stretchr/testify/require"
)

// blockingErrorDiagnosticRecorder 占住写入槽并阻塞，直到 release 被关闭，
// 用来确定性地把有界写入队列填满，从而走到溢出分支。
type blockingErrorDiagnosticRecorder struct {
	release   chan struct{}
	writes    int
	drops     int
	mu        sync.Mutex
	wroteOnce chan struct{}
}

func newBlockingErrorDiagnosticRecorder() *blockingErrorDiagnosticRecorder {
	return &blockingErrorDiagnosticRecorder{
		release:   make(chan struct{}),
		wroteOnce: make(chan struct{}),
	}
}

func (r *blockingErrorDiagnosticRecorder) RecordErrorDiagnostic(_ context.Context, _ ErrorDiagnosticAttempt) (ErrorDiagnosticRecord, error) {
	r.mu.Lock()
	first := r.writes == 0
	r.writes++
	r.mu.Unlock()
	if first {
		close(r.wroteOnce)
	}
	<-r.release
	return ErrorDiagnosticRecord{}, nil
}

func (r *blockingErrorDiagnosticRecorder) RecordDroppedErrorDiagnostic() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drops++
}

func (r *blockingErrorDiagnosticRecorder) counts() (writes, drops int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writes, r.drops
}

// TestErrorDiagnosticObserver_OverflowIsCountedWithoutBlocking 覆盖「上游错误风暴」路径：
// 写入积压时接缝必须立刻返回（不阻塞上游响应），并且为每次丢弃留下一个不含正文的计数。
func TestErrorDiagnosticObserver_OverflowIsCountedWithoutBlocking(t *testing.T) {
	recorder := newBlockingErrorDiagnosticRecorder()
	observer := newErrorDiagnosticObserver(recorder, errorDiagnosticSettingsFixed{
		settings: ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true},
	})

	observation := httpattempt.DiagnosticObservation{
		ObservedAt:  time.Now(),
		StatusCode:  500,
		BodyVerdict: httpattempt.DiagnosticBodyComplete,
		RequestBody: []byte(`{"model":"claude","messages":[{"role":"user","content":"sentinel"}]}`),
	}

	// 先填满所有写入槽：每次调用同步占槽后交给 goroutine，槽位因此在回调返回时即被占用。
	for i := 0; i < errorDiagnosticMaxInflightWrites; i++ {
		observer.record(context.Background(), errorDiagnosticBinding{protocol: ErrorDiagnosticProtocolMessages}, observation, nil, errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolMessages))
	}

	// 此时槽位已满：后续观察必须在溢出分支立即返回，而不是阻塞等待。
	// 槽位是被同步占用的，而占住它们的 goroutine 会一直阻塞在 recorder 里，
	// 因此在释放之前这 3 次观察必然每次都走溢出分支，计数是确定的。
	start := time.Now()
	for i := 0; i < 3; i++ {
		observer.record(context.Background(), errorDiagnosticBinding{protocol: ErrorDiagnosticProtocolMessages}, observation, nil, errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolMessages))
	}
	require.Less(t, time.Since(start).Seconds(), 2.0, "写入积压不得阻塞上游响应路径")

	_, drops := recorder.counts()
	require.Equal(t, 3, drops, "每次溢出都必须留下一个计数")

	// 释放被占住的写入，等它们真正落库并归还全部槽位。
	close(recorder.release)
	require.Eventually(t, func() bool {
		writes, _ := recorder.counts()
		return writes >= errorDiagnosticMaxInflightWrites
	}, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		return len(observer.writeSlots) == 0
	}, 5*time.Second, 10*time.Millisecond, "写入完成后必须归还所有槽位")

	// 槽位归还后新的观察正常入队，不再被计入丢弃。
	observer.record(context.Background(), errorDiagnosticBinding{protocol: ErrorDiagnosticProtocolMessages}, observation, nil, errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolMessages))
	_, dropsAfter := recorder.counts()
	require.Equal(t, 3, dropsAfter, "槽位归还后不得再记为丢弃")
	require.Eventually(t, func() bool {
		return len(observer.writeSlots) == 0
	}, 5*time.Second, 10*time.Millisecond)
}

// TestErrorDiagnosticObserver_DropIsSkippedWhenRecorderLacksTheCapability 覆盖
// 只实现写入契约的替身：溢出分支不得 panic，也不得因此阻塞。
func TestErrorDiagnosticObserver_DropIsSkippedWhenRecorderLacksTheCapability(t *testing.T) {
	recorder := &errorDiagnosticWriteOnlyRecorder{fail: make(chan struct{})}
	observer := newErrorDiagnosticObserver(recorder, errorDiagnosticSettingsFixed{
		settings: ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true},
	})

	observation := httpattempt.DiagnosticObservation{StatusCode: 503, BodyVerdict: httpattempt.DiagnosticBodyNotRequested}

	for i := 0; i < errorDiagnosticMaxInflightWrites; i++ {
		observer.record(context.Background(), errorDiagnosticBinding{protocol: ErrorDiagnosticProtocolMessages}, observation, nil, errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolMessages))
	}
	require.NotPanics(t, func() {
		for i := 0; i < 3; i++ {
			observer.record(context.Background(), errorDiagnosticBinding{protocol: ErrorDiagnosticProtocolMessages}, observation, nil, errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolMessages))
		}
	})
	close(recorder.fail)
}

// errorDiagnosticWriteOnlyRecorder 只实现必需的写入契约，刻意不提供丢弃能力。
type errorDiagnosticWriteOnlyRecorder struct {
	fail chan struct{}
}

func (r *errorDiagnosticWriteOnlyRecorder) RecordErrorDiagnostic(_ context.Context, _ ErrorDiagnosticAttempt) (ErrorDiagnosticRecord, error) {
	<-r.fail
	return ErrorDiagnosticRecord{}, nil
}

// blockingErrorDiagnosticRepo 在 Create 上阻塞，用来确定性地填满有界写入槽；
// 它让真实 *ErrorDiagnosticService 参与溢出路径，从而验证丢弃计数确实落在服务指标上。
type blockingErrorDiagnosticRepo struct {
	ErrorDiagnosticRepository
	release chan struct{}

	mu      sync.Mutex
	created int
}

func (r *blockingErrorDiagnosticRepo) CreateErrorDiagnostic(_ context.Context, write ErrorDiagnosticWrite, now time.Time) (ErrorDiagnosticRecord, error) {
	r.mu.Lock()
	r.created++
	r.mu.Unlock()
	<-r.release
	return ErrorDiagnosticRecord{
		ID:                 write.ID,
		Protocol:           write.Attempt.Protocol,
		AttemptIndex:       write.Attempt.AttemptIndex,
		Stage:              write.Attempt.Stage,
		UpstreamStatusCode: write.Attempt.UpstreamStatusCode,
		BodyState:          write.BodyState,
		BodyReason:         write.BodyReason,
		CreatedAt:          now,
		MetadataExpiresAt:  now.Add(ErrorDiagnosticMetadataRetention),
	}, nil
}

func (r *blockingErrorDiagnosticRepo) createdCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created
}

// TestErrorDiagnosticObserver_OverflowReachesRealServiceDropCounter 覆盖运维可观测性：
// 有界队列溢出必须体现在 *ErrorDiagnosticService 的无正文计数上，而不是被静默丢弃。
func TestErrorDiagnosticObserver_OverflowReachesRealServiceDropCounter(t *testing.T) {
	repo := &blockingErrorDiagnosticRepo{release: make(chan struct{})}
	settings := errorDiagnosticSettingsFixed{settings: ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true,
	}}
	svc := NewErrorDiagnosticService(repo, settings, nil)
	observer := newErrorDiagnosticObserver(svc, settings)

	observation := httpattempt.DiagnosticObservation{
		ObservedAt:  time.Now(),
		StatusCode:  500,
		BodyVerdict: httpattempt.DiagnosticBodyIncomplete,
	}

	// 槽位由 record 同步占用，因此 16 次观察后队列必定已满；真实写入则被仓储挡住。
	for i := 0; i < errorDiagnosticMaxInflightWrites; i++ {
		observer.record(context.Background(), errorDiagnosticBinding{protocol: ErrorDiagnosticProtocolMessages}, observation, nil, errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolMessages))
	}
	require.Eventually(t, func() bool {
		return repo.createdCount() == errorDiagnosticMaxInflightWrites
	}, 5*time.Second, 5*time.Millisecond, "真实服务必须已经发出 16 次写入")

	start := time.Now()
	observer.record(context.Background(), errorDiagnosticBinding{protocol: ErrorDiagnosticProtocolMessages}, observation, nil, errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolMessages))
	require.Less(t, time.Since(start).Seconds(), 2.0, "队列满时必须立刻返回，不得阻塞上游响应路径")
	require.Equal(t, int64(1), svc.Counters().Dropped, "每次溢出都必须累加服务侧的无正文计数")
	require.Zero(t, svc.Counters().WriteFailures, "丢弃不是写入失败，两个计数不得混用")

	close(repo.release)
	require.Eventually(t, func() bool {
		return svc.Counters().StoredRecords == errorDiagnosticMaxInflightWrites
	}, 5*time.Second, 10*time.Millisecond, "释放后积压的写入必须全部落库")
}

// TestErrorDiagnosticObserver_OverflowDropsWithoutCopyingTheBody 固定明文内存预算：
// 槽位满时的丢弃必须是零复制的——绝不能先复制一份 1 MiB 正文再把它扔掉，
// 否则 4xx 风暴下的分配量会随丢弃次数线性放大（每次丢弃 1 MiB）。
func TestErrorDiagnosticObserver_OverflowDropsWithoutCopyingTheBody(t *testing.T) {
	recorder := newBlockingErrorDiagnosticRecorder()
	observer := newErrorDiagnosticObserver(recorder, errorDiagnosticSettingsFixed{
		settings: ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true},
	})
	// 释放被占住的写入，避免测试结束时留下阻塞的 goroutine。
	defer close(recorder.release)

	// 正好等于单次留存上限：修复前每次丢弃都会复制这么多字节。
	observation := httpattempt.DiagnosticObservation{
		ObservedAt:  time.Now(),
		StatusCode:  500,
		BodyVerdict: httpattempt.DiagnosticBodyComplete,
		RequestBody: bytes.Repeat([]byte("x"), int(ErrorDiagnosticMaxBodyBytes)),
	}
	ordinalKey := errorDiagnosticOrdinalContextKey(ErrorDiagnosticProtocolMessages)

	// 先占满写入槽；这一步的复制属于设计内的有界明文（槽位数 × 单次上限）。
	for i := 0; i < errorDiagnosticMaxInflightWrites; i++ {
		observer.record(context.Background(), errorDiagnosticBinding{protocol: ErrorDiagnosticProtocolMessages}, observation, nil, ordinalKey)
	}
	_, dropsBefore := recorder.counts()
	require.Zero(t, dropsBefore)

	const droppedAttempts = 500
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < droppedAttempts; i++ {
		observer.record(context.Background(), errorDiagnosticBinding{protocol: ErrorDiagnosticProtocolMessages}, observation, nil, ordinalKey)
	}
	runtime.ReadMemStats(&after)

	_, drops := recorder.counts()
	require.Equal(t, droppedAttempts, drops, "每次溢出都必须留下计数")
	// 修复前：500 × 1 MiB ≈ 500 MiB。预算留足背景噪声余量，仍只有零复制路径能满足。
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<20),
		"溢出丢弃不得复制出站正文")
}
