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

// ErrorDiagnosticCleanupResult 是一轮清理的结果。
//
// 每一段都是「在线主库上的物理动作」计数，不是「到期即可读」的声明：应用读取在到期时刻
// 就已经拒绝，这里只说明这一轮真的清掉了多少。
type ErrorDiagnosticCleanupResult struct {
	BodiesCleared       int64
	HeaderValuesCleared int64
	RecordsDeleted      int64
	// 新明文层（票据 08／09）的清除数单独计：明文残留与密文残留是两件事，
	// 合并成一个数就无法判断「哪一层没有在清」。
	PlainBodiesCleared       int64
	PlainHeaderValuesCleared int64
	// LinksReconciled 是本轮补上的 usage 关联数（迟到的使用记录被验证后绑定）。
	LinksReconciled int64
}

// ErrorDiagnosticCleanupService 按保留期清理错误诊断的在线主库副本。
//
// 每一段分开做，因为期限与归属都不同（都按 created_at 计算）：
//   - 第 7 天起：把 body_ciphertext 置空，物理清除在线主库上的正文密文列，保留整行元数据；
//   - 第 7 天起：把 header_ciphertext 置空，物理清除在线主库上的 429 头值密文列；
//   - 每轮先补 usage 关联：把随后才形成使用记录的**新明文**诊断绑定到它（有界批量）；
//   - 第 30 天起（仅未关联的新明文行）：置空 plain_*_payload 并记 purged；
//   - 第 30 天起（未关联的行）：物理删除整行元数据；已关联的新明文行**不在**其中，
//     它们的所有者是使用记录，随 usage 删除。
//
// 这是**周期批量**作业：默认每 10 分钟一轮、每轮最多 ErrorDiagnosticCleanupBatch 行，
// 停机、积压或单轮未取完都会推迟物理清除，因此没有「到期即删」的确切保证。
// 与之相对，应用读取在到期时刻就已经拒绝——两者是不同的事实，不得混为一谈。
//
// 它不删除使用记录，也不因使用记录被删除而提前删除诊断。
// 清理失败与积压都只上报不含正文、头值与凭据的计数与时长：失败要能发现，落后要能度量。
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

// RunOnce 执行一轮清理：先清正文与头值密文（第 7 天），再删整行（第 30 天）。
//
// 这是清理的公开接缝，供测试与运维直接调用；三段都执行完才返回，
// 三段互不阻塞：正文清除失败不能连带跳过头值清除或第 30 天的整行删除，
// 否则一条清理不掉的密文会让元数据无限期留在在线主库上，直接违反 30 天上限。
// 三段各自的失败都会单独告警（原因码分开，运维可分辨是哪一段卡住），
// 返回值用 errors.Join 合并，且不隐瞒已经发生的物理删除。
func (s *ErrorDiagnosticCleanupService) RunOnce(ctx context.Context) (ErrorDiagnosticCleanupResult, error) {
	if s == nil || s.repo == nil {
		return ErrorDiagnosticCleanupResult{}, ErrErrorDiagnosticUnavailable
	}
	now := s.now()
	var result ErrorDiagnosticCleanupResult
	var linkErr, plainBodyErr, plainHeaderErr error

	bodiesCleared, bodyErr := s.repo.ClearExpiredErrorDiagnosticBodies(ctx, now, s.batch)
	result.BodiesCleared = bodiesCleared
	if s.metrics != nil {
		s.metrics.bodiesCleared.Add(result.BodiesCleared)
	}
	if bodyErr != nil {
		// 只报原因码与计数：数据库错误值可能回显涉及的行值，绝不进入日志。
		s.alerts.warn(ErrorDiagnosticAlertCleanupFailed, ErrorDiagnosticAlertCodeBodyClearFailed, s.recordCleanupFailure())
	}

	headerValuesCleared, headerErr := s.repo.ClearExpiredErrorDiagnosticHeaderValues(ctx, now, s.batch)
	result.HeaderValuesCleared = headerValuesCleared
	if s.metrics != nil {
		s.metrics.headerValuesCleared.Add(result.HeaderValuesCleared)
	}
	if headerErr != nil {
		s.alerts.warn(ErrorDiagnosticAlertCleanupFailed, ErrorDiagnosticAlertCodeHeaderValueClearFailed, s.recordCleanupFailure())
	}

	// 先补关联、再清载荷、最后删行。顺序有实质意义：
	//   - 补关联必须在清载荷与删行之前跑，否则「这次失败属于哪条使用记录」可能在下一轮之前
	//     就被三十天清理带走；
	//   - 补偿是有界批量，且只做「已能逐项验证」的绑定，绝不猜测。
	//
	// 存储层没有这个能力时静默跳过：旧实现没有明文列，也就没有迟关联这回事。
	if reconciler, ok := s.repo.(ErrorDiagnosticLinkReconciler); ok {
		reconciled, err := reconciler.ReconcilePlainErrorDiagnosticLinks(ctx, now, s.batch)
		result.LinksReconciled = reconciled
		if s.metrics != nil {
			s.metrics.linksReconciled.Add(reconciled)
		}
		if err != nil {
			s.alerts.warn(ErrorDiagnosticAlertCleanupFailed, ErrorDiagnosticAlertCodeLinkReconcileFailed, s.recordCleanupFailure())
			linkErr = err
		}
	}

	// 新明文层的载荷清除与旧密文层分开成两段，且**在整行删除之前**执行：
	// 未关联的明文行到第三十天就已经拒绝读取，清列的时机只影响在线主库上的物理残留，
	// 但它必须发生，否则「到期」只落在 API 上，明文本身还留在表里。
	//
	// 存储层没有这一层能力时不假装清过、也不报错：它是装配选择（旧实现没有明文列），
	// 与「清理失败」不同。真正的风险由运维状态与部署说明暴露。
	if plaintextRepo, ok := s.repo.(ErrorDiagnosticPlaintextReader); ok {
		plainBodiesCleared, err := plaintextRepo.ClearExpiredErrorDiagnosticPlainBodies(ctx, now, s.batch)
		result.PlainBodiesCleared = plainBodiesCleared
		if s.metrics != nil {
			s.metrics.plainBodiesCleared.Add(plainBodiesCleared)
		}
		if err != nil {
			s.alerts.warn(ErrorDiagnosticAlertCleanupFailed, ErrorDiagnosticAlertCodePlainBodyClearFailed, s.recordCleanupFailure())
			plainBodyErr = err
		}

		plainHeaderValuesCleared, err := plaintextRepo.ClearExpiredErrorDiagnosticPlainHeaderValues(ctx, now, s.batch)
		result.PlainHeaderValuesCleared = plainHeaderValuesCleared
		if s.metrics != nil {
			s.metrics.plainHeaderCleared.Add(plainHeaderValuesCleared)
		}
		if err != nil {
			s.alerts.warn(ErrorDiagnosticAlertCleanupFailed, ErrorDiagnosticAlertCodePlainHeaderValueClearFailed, s.recordCleanupFailure())
			plainHeaderErr = err
		}
	}

	recordsDeleted, deleteErr := s.repo.DeleteExpiredErrorDiagnostics(ctx, now, s.batch)
	result.RecordsDeleted = recordsDeleted
	if s.metrics != nil {
		s.metrics.recordsDeleted.Add(result.RecordsDeleted)
	}
	if deleteErr != nil {
		s.alerts.warn(ErrorDiagnosticAlertCleanupFailed, ErrorDiagnosticAlertCodeRecordDeleteFailed, s.recordCleanupFailure())
	}

	// 积压观测：批量与间隔都不保证「到期即删」，因此必须能看出清理是否落后。
	// 探针失败只告警，不改变清理结果，也不影响返回值。
	s.observeBacklog(ctx, now)
	return result, errors.Join(linkErr, bodyErr, headerErr, plainBodyErr, plainHeaderErr, deleteErr)
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
	overdue := backlog.BodiesOverdue + backlog.RecordsOverdue + backlog.HeaderValuesOverdue +
		backlog.PlainBodiesOverdue + backlog.PlainHeaderValuesOverdue
	oldestSeconds := backlog.OldestOverdueSeconds(now)
	if s.metrics != nil {
		s.metrics.overdueBodies.Store(backlog.BodiesOverdue)
		s.metrics.overdueRecords.Store(backlog.RecordsOverdue)
		s.metrics.overdueHeaders.Store(backlog.HeaderValuesOverdue)
		s.metrics.plainBodiesOverdue.Store(backlog.PlainBodiesOverdue)
		s.metrics.plainHeadersOverdue.Store(backlog.PlainHeaderValuesOverdue)
		s.metrics.oldestOverdue.Store(oldestSeconds)
	}
	if overdue == 0 {
		return
	}
	// 只报计数与时长：绝不含正文、头值、头名或凭据。
	s.alerts.warnBacklog(ErrorDiagnosticAlertCleanupBacklog, ErrorDiagnosticAlertCodeBacklog, overdue,
		zap.Int64("bodies_overdue", backlog.BodiesOverdue),
		zap.Int64("records_overdue", backlog.RecordsOverdue),
		zap.Int64("header_values_overdue", backlog.HeaderValuesOverdue),
		zap.Int64("plain_bodies_overdue", backlog.PlainBodiesOverdue),
		zap.Int64("plain_header_values_overdue", backlog.PlainHeaderValuesOverdue),
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
	_, _ = s.RunOnce(ctx)
}

// Counters 返回共享计数快照；未共享计数时返回空快照。
func (s *ErrorDiagnosticCleanupService) Counters() ErrorDiagnosticMetricsSnapshot {
	if s == nil {
		return ErrorDiagnosticMetricsSnapshot{}
	}
	return s.metrics.Snapshot()
}
