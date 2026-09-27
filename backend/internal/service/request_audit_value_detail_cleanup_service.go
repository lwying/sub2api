package service

// 值明细旁路的周期清理（ADR 0006）。
//
// 只做一件事：把已到第 7 天的密文在**在线主库**上物理清除，并把该行记成 purged。
// 行本身与 expires_at 保留，因此「曾留存、现已清除」与「从未留存」在状态上可区分，
// 使用记录被删除时整行随外键级联消失。
//
// 这是**周期批量**作业：默认约每 10 分钟一轮、每轮最多 RequestAuditValueDetailCleanupBatch 行。
// 停机、积压或单轮未取完都会推迟物理清除，因此**没有**「到期即删」的确切保证，
// 也不承诺任何最大延迟。与之相对，应用读取在到期时刻就已经拒绝——两者是不同的事实，
// 不得混为一谈：拒绝读取不等于删除。
//
// 可观测性：清理结果、失败与积压都只上报不含值的计数、原因码与时长（见 Snapshot），
// 告警按「事件 + 原因码」限速并把被抑制的次数聚合进下一次输出；「无法观测积压」
// 与「积压为 0」是两个不同的事实，不得互相冒充。

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// RequestAuditValueDetailCleanupBatch 是单轮清理的批量上限，避免一次删除持有长时间锁。
const RequestAuditValueDetailCleanupBatch = 500

// RequestAuditValueDetailCleanupInterval 是后台清理的默认间隔。
const RequestAuditValueDetailCleanupInterval = 10 * time.Minute

// RequestAuditValueDetailCleanupTimeout 是单轮清理的超时：清理永远不与请求争用连接。
const RequestAuditValueDetailCleanupTimeout = 30 * time.Second

// RequestAuditValueDetailAlertInterval 是同一「事件 + 原因码」桶的最小告警间隔。
//
// 期间只累加被抑制的次数，不发新行；下一次到期时把累计次数一次性带出。
// 周期循环本身是 10 分钟一轮，但 RunOnce 是公开的运维/测试接缝，可被反复直接调用，
// 因此告警必须自带上界：重复调用不会把日志量放大，而计数一次也不丢。
const RequestAuditValueDetailAlertInterval = 30 * time.Second

// 清理故障的稳定告警标识：运维按事件名与原因码聚合，不依赖日志正文。
const (
	// RequestAuditValueDetailAlertCleanupFailed 是单轮清理失败的稳定事件名。
	RequestAuditValueDetailAlertCleanupFailed = "request_audit_value_detail.cleanup_failed"
	// RequestAuditValueDetailAlertCleanupBacklog 是遗留密文清理积压的稳定事件名。
	RequestAuditValueDetailAlertCleanupBacklog = "request_audit_value_detail.cleanup_backlog"
	// RequestAuditValueDetailAlertBacklogProbeFailed 是积压探针自身失败的稳定事件名。
	RequestAuditValueDetailAlertBacklogProbeFailed = "request_audit_value_detail.backlog_probe_failed"

	// RequestAuditValueDetailAlertCodeClearFailed 是物理清除失败的稳定原因码。
	RequestAuditValueDetailAlertCodeClearFailed = "clear_failed"
	// RequestAuditValueDetailAlertCodeBacklog 是清理落后的稳定原因码。
	RequestAuditValueDetailAlertCodeBacklog = "cleanup_backlog"
	// RequestAuditValueDetailAlertCodeBacklogProbeFailed 是探针失败的稳定原因码。
	//
	// 与「清理失败」分开：读不到积压不等于清除卡住，合并成一个码会让运维看不出是哪一段坏了。
	RequestAuditValueDetailAlertCodeBacklogProbeFailed = "backlog_probe_failed"
)

// requestAuditValueDetailAlerter 以有界速率输出不含值的清理告警。
//
// 只输出事件名、原因码与计数三类信息：绝不输出头名、头值、模型名、使用记录标识，
// 也绝不输出数据库错误值。
type requestAuditValueDetailAlerter struct {
	mu         sync.Mutex
	interval   time.Duration
	lastAt     map[string]time.Time
	suppressed map[string]int64
}

func newRequestAuditValueDetailAlerter(interval time.Duration) *requestAuditValueDetailAlerter {
	if interval <= 0 {
		interval = RequestAuditValueDetailAlertInterval
	}
	return &requestAuditValueDetailAlerter{
		interval:   interval,
		lastAt:     map[string]time.Time{},
		suppressed: map[string]int64{},
	}
}

// warn 在允许的时刻输出一条告警；被抑制的次数会累积到下一次输出。
//
// now 由调用方按服务自己的时钟传入，从而使速率限制在测试中可确定地断言。
// 限速按「事件 + 原因码」分桶，而不是只按事件分桶：否则同一事件下两种不同的故障
// 会互相压制，运维只会看到先出现的那一种。
func (a *requestAuditValueDetailAlerter) warn(now time.Time, event, code string, total int64, extra ...zap.Field) {
	if a == nil {
		return
	}
	key := event + "|" + code
	a.mu.Lock()
	last, seen := a.lastAt[key]
	if seen && now.Sub(last) < a.interval {
		a.suppressed[key]++
		a.mu.Unlock()
		return
	}
	sinceLast := a.suppressed[key] + 1
	a.suppressed[key] = 0
	a.lastAt[key] = now
	a.mu.Unlock()

	logger.L().Warn(event, append([]zap.Field{
		zap.String("code", code),
		zap.Int64("count_since_last_alert", sinceLast),
		zap.Int64("total", total),
	}, extra...)...)
}

// RequestAuditValueDetailCleanupSnapshot 是本进程清理作业的可观测快照。
//
// 全部字段都是计数与时长，不含任何头名、头值、模型名或标识，可供没有日志采集的部署读取。
type RequestAuditValueDetailCleanupSnapshot struct {
	// ClearedRows 是本进程累计物理清除的遗留密文行数。
	ClearedRows uint64
	// CleanupFailures 是本进程累计失败的轮数（清除段与积压探针各自计数）。
	CleanupFailures uint64
	// BacklogRows 是最近一次成功探针读到的、已过期仍未清除的遗留密文行数。
	//
	// 新明文跟随其使用记录，不在这个数里。
	BacklogRows int64
	// OldestOverdueSeconds 是最近一次成功探针读到的最老超期行的超期秒数。
	OldestOverdueSeconds int64
	// BacklogObserved 表示 BacklogRows 与 OldestOverdueSeconds 是否来自一次成功的探针。
	//
	// false 表示「无法观测」——存储层没有提供探针，或上一次探针失败，
	// 绝不表示「积压为 0」：两者必须可区分。
	BacklogObserved bool
}

// RequestAuditValueDetailCleanupBacklogReader is optional. A repository without
// it does not falsely report zero backlog.
type RequestAuditValueDetailCleanupBacklogReader interface {
	ReadRequestAuditValueDetailCleanupBacklog(ctx context.Context, now time.Time) (count int64, oldest time.Time, err error)
}

// RequestAuditValueDetailCleanupService clears only legacy seven-day ciphertext;
// new plaintext follows the owning usage row, not this cleanup timer.
//
// 失败与积压都只上报不含值的计数与时长：失败要能发现，落后要能度量。
type RequestAuditValueDetailCleanupService struct {
	repo     RequestAuditValueDetailRepository
	alerts   *requestAuditValueDetailAlerter
	interval time.Duration
	batch    int
	now      func() time.Time

	cleared  atomic.Uint64
	failures atomic.Uint64

	backlogRows     atomic.Int64
	oldestOverdue   atomic.Int64
	backlogObserved atomic.Bool

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// NewRequestAuditValueDetailCleanupService 构造清理服务。
func NewRequestAuditValueDetailCleanupService(repo RequestAuditValueDetailRepository) *RequestAuditValueDetailCleanupService {
	return &RequestAuditValueDetailCleanupService{
		repo:     repo,
		alerts:   newRequestAuditValueDetailAlerter(RequestAuditValueDetailAlertInterval),
		interval: RequestAuditValueDetailCleanupInterval,
		batch:    RequestAuditValueDetailCleanupBatch,
		now:      time.Now,
		stopCh:   make(chan struct{}),
	}
}

// RunOnce 执行一轮清理，返回本轮物理清除的密文行数。
//
// 这是清理的公开接缝，供测试与运维直接调用。失败只上报计数与原因码：数据库错误值
// 可能回显涉及的行值，绝不进入日志。
func (s *RequestAuditValueDetailCleanupService) RunOnce(ctx context.Context) (int64, error) {
	if s == nil || s.repo == nil {
		return 0, ErrRequestAuditValueDetailUnavailable
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	current := now()
	cleared, err := s.repo.ClearExpiredRequestAuditValueDetails(ctx, current, s.batch)
	if err != nil {
		s.alerts.warn(current, RequestAuditValueDetailAlertCleanupFailed,
			RequestAuditValueDetailAlertCodeClearFailed, int64(s.recordFailure()), zap.Bool("audit", true))
		return 0, err
	}
	if cleared > 0 {
		s.cleared.Add(uint64(cleared))
	}
	s.observeBacklog(ctx, current)
	return cleared, nil
}

// observeBacklog 读取并上报遗留密文的清理积压。
//
// 积压只代表在线主库上的物理残留，不代表内容可读（应用层在到期时刻就已拒绝）。
// 探针失败必须可见，但不能改变清理结果。积压与新明文无关：新明文跟随其使用记录。
func (s *RequestAuditValueDetailCleanupService) observeBacklog(ctx context.Context, now time.Time) {
	reader, ok := s.repo.(RequestAuditValueDetailCleanupBacklogReader)
	if !ok {
		// 存储层没有提供积压观测：这是装配选择，不是故障，因此不告警，
		// 也不用 0 冒充「没有积压」——快照里的 BacklogObserved 保持 false。
		return
	}
	count, oldest, probeErr := reader.ReadRequestAuditValueDetailCleanupBacklog(ctx, now)
	if probeErr != nil {
		s.alerts.warn(now, RequestAuditValueDetailAlertBacklogProbeFailed,
			RequestAuditValueDetailAlertCodeBacklogProbeFailed, int64(s.recordFailure()), zap.Bool("audit", true))
		return
	}
	oldestSeconds := requestAuditValueDetailOldestOverdueSeconds(now, oldest)
	s.backlogRows.Store(count)
	s.oldestOverdue.Store(oldestSeconds)
	s.backlogObserved.Store(true)
	if count <= 0 {
		return
	}
	// 只报计数与时长：绝不含头名、头值、模型名或标识。
	s.alerts.warn(now, RequestAuditValueDetailAlertCleanupBacklog,
		RequestAuditValueDetailAlertCodeBacklog, count, zap.Bool("audit", true),
		zap.Int64("legacy_ciphertext_overdue", count),
		zap.Int64("oldest_overdue_seconds", oldestSeconds))
}

// requestAuditValueDetailOldestOverdueSeconds 把最老超期时刻换算成秒数。
//
// 零值（没有可读回的最老行）与时钟倒退都按 0 处理，不上报负数。
func requestAuditValueDetailOldestOverdueSeconds(now, oldest time.Time) int64 {
	if oldest.IsZero() {
		return 0
	}
	seconds := int64(now.Sub(oldest).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}

// Snapshot 返回本进程清理作业的快照；未共享计数时返回零值快照。
//
// 供没有日志采集的运维读取：待清理数、最老超期时长与「是否可观测」都在这里，
// 且与失败计数分开——积压不是失败。
func (s *RequestAuditValueDetailCleanupService) Snapshot() RequestAuditValueDetailCleanupSnapshot {
	if s == nil {
		return RequestAuditValueDetailCleanupSnapshot{}
	}
	return RequestAuditValueDetailCleanupSnapshot{
		ClearedRows:          s.cleared.Load(),
		CleanupFailures:      s.failures.Load(),
		BacklogRows:          s.backlogRows.Load(),
		OldestOverdueSeconds: s.oldestOverdue.Load(),
		BacklogObserved:      s.backlogObserved.Load(),
	}
}

// recordFailure 累加失败轮数并返回累计值，供告警的 total 字段使用。
func (s *RequestAuditValueDetailCleanupService) recordFailure() uint64 {
	return s.failures.Add(1)
}

// ClearedCount 返回本进程累计物理清除的密文行数；未共享计数时返回 0。
func (s *RequestAuditValueDetailCleanupService) ClearedCount() uint64 {
	if s == nil {
		return 0
	}
	return s.cleared.Load()
}

// FailureCount 返回本进程累计的清理失败轮数。
func (s *RequestAuditValueDetailCleanupService) FailureCount() uint64 {
	if s == nil {
		return 0
	}
	return s.failures.Load()
}

// Start 启动后台清理循环；重复调用是安全的。
func (s *RequestAuditValueDetailCleanupService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.runLoop()
	})
}

// Stop 停止后台清理循环；重复调用是安全的。
func (s *RequestAuditValueDetailCleanupService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

func (s *RequestAuditValueDetailCleanupService) runLoop() {
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

func (s *RequestAuditValueDetailCleanupService) cleanupOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), RequestAuditValueDetailCleanupTimeout)
	defer cancel()
	// 失败已经计入 FailureCount，这里不重复输出，也不输出任何值或标识。
	_, _ = s.RunOnce(ctx)
}
