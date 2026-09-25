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

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// RequestAuditValueDetailCleanupBatch 是单轮清理的批量上限，避免一次删除持有长时间锁。
const RequestAuditValueDetailCleanupBatch = 500

// RequestAuditValueDetailCleanupInterval 是后台清理的默认间隔。
const RequestAuditValueDetailCleanupInterval = 10 * time.Minute

// RequestAuditValueDetailCleanupTimeout 是单轮清理的超时：清理永远不与请求争用连接。
const RequestAuditValueDetailCleanupTimeout = 30 * time.Second

// RequestAuditValueDetailCleanupService 按 7 天保留期清理在线主库上的值密文。
type RequestAuditValueDetailCleanupService struct {
	repo     RequestAuditValueDetailRepository
	interval time.Duration
	batch    int
	now      func() time.Time

	cleared  atomic.Uint64
	failures atomic.Uint64

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// NewRequestAuditValueDetailCleanupService 构造清理服务。
func NewRequestAuditValueDetailCleanupService(repo RequestAuditValueDetailRepository) *RequestAuditValueDetailCleanupService {
	return &RequestAuditValueDetailCleanupService{
		repo:     repo,
		interval: RequestAuditValueDetailCleanupInterval,
		batch:    RequestAuditValueDetailCleanupBatch,
		now:      time.Now,
		stopCh:   make(chan struct{}),
	}
}

// RunOnce 执行一轮清理，返回本轮物理清除的密文行数。
//
// 这是清理的公开接缝，供测试与运维直接调用。失败只上报计数：数据库错误值可能回显
// 涉及的行值，绝不进入日志。
func (s *RequestAuditValueDetailCleanupService) RunOnce(ctx context.Context) (int64, error) {
	if s == nil || s.repo == nil {
		return 0, ErrRequestAuditValueDetailUnavailable
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	cleared, err := s.repo.ClearExpiredRequestAuditValueDetails(ctx, now(), s.batch)
	if err != nil {
		s.failures.Add(1)
		return 0, err
	}
	if cleared > 0 {
		s.cleared.Add(uint64(cleared))
	}
	return cleared, nil
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
