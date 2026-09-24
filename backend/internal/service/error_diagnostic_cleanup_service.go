package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrorDiagnosticCleanupBatch 是单轮清理的批量上限，避免一次删除持有长时间锁。
const ErrorDiagnosticCleanupBatch = 500

// ErrorDiagnosticCleanupInterval 是后台清理的默认间隔。
const ErrorDiagnosticCleanupInterval = 10 * time.Minute

// ErrorDiagnosticCleanupService 按保留期清理错误诊断的在线主库副本。
//
// 两件事分开做，因为期限不同（按 created_at 计算）：
//   - 第 7 天起：把 body_ciphertext 置空，物理清除在线主库上的密文列，保留整行元数据；
//   - 第 30 天起：物理删除整行元数据。
//
// 这是**周期批量**作业：默认每 10 分钟一轮、每轮最多 ErrorDiagnosticCleanupBatch 行，
// 停机、积压或单轮未取完都会推迟物理清除，因此没有「到期即删」的确切保证。
// 与之相对，应用读取在到期时刻就已经拒绝——两者是不同的事实，不得混为一谈。
//
// 它不删除使用记录，也不因使用记录被删除而提前删除诊断。
// 清理失败与积压都只上报不含正文的计数与时长：失败要能发现，落后要能度量。
type ErrorDiagnosticCleanupService struct {
	repo     ErrorDiagnosticRepository
	metrics  *ErrorDiagnosticMetrics
	alerts   *errorDiagnosticAlerter
	interval time.Duration
	batch    int
	now      func() time.Time

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// NewErrorDiagnosticCleanupService 构造清理服务；metrics 可省略。
func NewErrorDiagnosticCleanupService(repo ErrorDiagnosticRepository, metrics ...*ErrorDiagnosticMetrics) *ErrorDiagnosticCleanupService {
	var shared *ErrorDiagnosticMetrics
	if len(metrics) > 0 {
		shared = metrics[0]
	}
	return &ErrorDiagnosticCleanupService{
		repo:     repo,
		metrics:  shared,
		alerts:   newErrorDiagnosticAlerter(ErrorDiagnosticAlertInterval, time.Now),
		interval: ErrorDiagnosticCleanupInterval,
		batch:    ErrorDiagnosticCleanupBatch,
		now:      time.Now,
		stopCh:   make(chan struct{}),
	}
}

// RunOnce 执行一轮清理：先清正文（第 7 天），再删整行（第 30 天）。
//
// 这是清理的公开接缝，供测试与运维直接调用；两段都执行完才返回，
// 两段互不阻塞：正文清除失败不能连带跳过第 30 天的整行删除，否则一条清理不掉的正文
// 会让元数据无限期留在在线主库上，直接违反 30 天上限。两段各自的失败都会单独告警，
// 返回值用 errors.Join 合并，且不隐瞒已经发生的物理删除。
func (s *ErrorDiagnosticCleanupService) RunOnce(ctx context.Context) (bodiesCleared int64, recordsDeleted int64, err error) {
	if s == nil || s.repo == nil {
		return 0, 0, ErrErrorDiagnosticUnavailable
	}
	now := s.now()

	bodiesCleared, bodyErr := s.repo.ClearExpiredErrorDiagnosticBodies(ctx, now, s.batch)
	if s.metrics != nil {
		s.metrics.bodiesCleared.Add(bodiesCleared)
	}
	if bodyErr != nil {
		// 只报原因码与计数：数据库错误值可能回显涉及的行值，绝不进入日志。
		s.alerts.warn(ErrorDiagnosticAlertCleanupFailed, ErrorDiagnosticAlertCodeBodyClearFailed, s.recordCleanupFailure())
	}

	recordsDeleted, deleteErr := s.repo.DeleteExpiredErrorDiagnostics(ctx, now, s.batch)
	if s.metrics != nil {
		s.metrics.recordsDeleted.Add(recordsDeleted)
	}
	if deleteErr != nil {
		s.alerts.warn(ErrorDiagnosticAlertCleanupFailed, ErrorDiagnosticAlertCodeRecordDeleteFailed, s.recordCleanupFailure())
	}

	// 积压观测：批量与间隔都不保证「到期即删」，因此必须能看出清理是否落后。
	// 探针失败只告警，不改变清理结果，也不影响返回值。
	s.observeBacklog(ctx, now)
	return bodiesCleared, recordsDeleted, errors.Join(bodyErr, deleteErr)
}

// observeBacklog 读取并上报清理积压；积压只代表在线主库上的物理残留，
// 不代表内容可读（应用层在到期时刻就已拒绝）。失败与积压都只报计数与时长。
func (s *ErrorDiagnosticCleanupService) observeBacklog(ctx context.Context, now time.Time) {
	reader, ok := s.repo.(ErrorDiagnosticCleanupBacklogReader)
	if !ok {
		// 存储层没有提供积压观测：这是装配选择，不是故障，因此不告警也不留 0 值误导。
		return
	}
	backlog, err := reader.ReadErrorDiagnosticCleanupBacklog(ctx, now)
	if err != nil {
		s.alerts.warn(ErrorDiagnosticAlertCleanupFailed, ErrorDiagnosticAlertCodeBacklogProbeFailed, s.recordCleanupFailure())
		return
	}
	overdue := backlog.BodiesOverdue + backlog.RecordsOverdue
	oldestSeconds := backlog.OldestOverdueSeconds(now)
	if s.metrics != nil {
		s.metrics.overdueBodies.Store(backlog.BodiesOverdue)
		s.metrics.overdueRecords.Store(backlog.RecordsOverdue)
		s.metrics.oldestOverdue.Store(oldestSeconds)
	}
	if overdue == 0 {
		return
	}
	s.alerts.warnBacklog(ErrorDiagnosticAlertCleanupBacklog, ErrorDiagnosticAlertCodeBacklog, overdue,
		zap.Int64("bodies_overdue", backlog.BodiesOverdue),
		zap.Int64("records_overdue", backlog.RecordsOverdue),
		zap.Int64("oldest_overdue_seconds", oldestSeconds),
	)
}

// Backlog 读取当前清理积压，供运维与测试直接查询。
//
// 存储层不支持观测时返回 ErrErrorDiagnosticBacklogUnsupported，
// 而不是零值——「无法观测」与「没有积压」必须可区分。
func (s *ErrorDiagnosticCleanupService) Backlog(ctx context.Context) (ErrorDiagnosticCleanupBacklog, error) {
	if s == nil || s.repo == nil {
		return ErrorDiagnosticCleanupBacklog{}, ErrErrorDiagnosticUnavailable
	}
	reader, ok := s.repo.(ErrorDiagnosticCleanupBacklogReader)
	if !ok {
		return ErrorDiagnosticCleanupBacklog{}, ErrErrorDiagnosticBacklogUnsupported
	}
	return reader.ReadErrorDiagnosticCleanupBacklog(ctx, s.now())
}

func (s *ErrorDiagnosticCleanupService) recordCleanupFailure() int64 {
	if s.metrics == nil {
		return 1
	}
	return s.metrics.cleanupFailures.Add(1)
}

// Start 启动后台清理循环；重复调用是安全的。
func (s *ErrorDiagnosticCleanupService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.runLoop()
	})
}

// Stop 停止后台清理循环；重复调用是安全的。
func (s *ErrorDiagnosticCleanupService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

func (s *ErrorDiagnosticCleanupService) runLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.cleanupOnce()
	for {
		select {
		case <-ticker.C:
			s.cleanupOnce()
		case <-s.stopCh:
			return
		}
	}
}

func (s *ErrorDiagnosticCleanupService) cleanupOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// RunOnce 已经把失败写成结构化告警（含原因码与计数），这里不再重复输出，
	// 也不输出正文、凭据或诊断标识。
	_, _, _ = s.RunOnce(ctx)
}

// Counters 返回共享计数快照；未共享计数时返回空快照。
func (s *ErrorDiagnosticCleanupService) Counters() ErrorDiagnosticMetricsSnapshot {
	if s == nil {
		return ErrorDiagnosticMetricsSnapshot{}
	}
	return s.metrics.Snapshot()
}
