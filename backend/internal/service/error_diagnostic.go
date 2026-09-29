package service

// 独立错误诊断的**历史留存契约**（ADR 0005；票据 10 退役）。
//
// 新采集、运维开关与揭示入口已在票据 10 一并移除：新版本不再写入任何新的诊断行，
// 也不再暴露读取旧正文／旧 429 头值的接口。这里只保留仍然必需的部分——
// 按原期限清理已存在的在线主库副本，并上报清理积压与失败：
//
//   - 第 7 天起：物理清除正文密文列与 429 头值密文列，保留整行元数据；
//   - 每轮先补 usage 关联：把随后才形成使用记录的明文诊断绑定到它（有界批量）；
//   - 第 30 天起（仅未关联的明文行）：清空明文载荷并记 purged；
//   - 第 30 天起（未关联的行）：物理删除整行；已关联的明文行**不在**其中，
//     它们的所有者是使用记录，随 usage 删除。
//
// 清理是周期批量作业：停机、积压或单轮未取完都会推迟物理清除，因此不承诺
// 「到期即删」，也不承诺确切时刻。清理失败与积压只上报不含正文、头值与凭据的计数与时长。
//
// 迁移 251–257 的字节不因本次退役回写；旧表不因升级自动 DROP 或清空。

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

var (
	// ErrErrorDiagnosticUnavailable 表示存储或依赖不可用。
	ErrErrorDiagnosticUnavailable = errors.New("error diagnostics are temporarily unavailable")
	// ErrErrorDiagnosticBacklogUnsupported 表示存储层未提供清理积压观测能力。
	//
	// 「无法观测」与「积压为 0」是两个不同的事实，不得互相冒充。
	ErrErrorDiagnosticBacklogUnsupported = errors.New("error diagnostic cleanup backlog is not observable")
)

// ErrorDiagnosticRepository 是诊断存储的**留存契约**：只保留到期物理清除。
//
// 采集（CreateErrorDiagnostic）与读取方法随票据 10 一并移除，因此任何调用方都无法再
// 通过这个接口写入或读出正文与头值——旧副本只可能被清除，不会被重新读出或覆盖。
// 整行删除只针对**未关联**的到期行；已关联的使用记录所有者语义不变。
type ErrorDiagnosticRepository interface {
	// ClearExpiredErrorDiagnosticBodies 在在线主库物理清除第 7 天到期的正文密文列。
	ClearExpiredErrorDiagnosticBodies(ctx context.Context, now time.Time, batch int) (int64, error)
	// ClearExpiredErrorDiagnosticHeaderValues 在在线主库物理清除第 7 天到期的头值密文列。
	ClearExpiredErrorDiagnosticHeaderValues(ctx context.Context, now time.Time, batch int) (int64, error)
	// DeleteExpiredErrorDiagnostics 在在线主库物理删除第 30 天到期的整行。
	DeleteExpiredErrorDiagnostics(ctx context.Context, now time.Time, batch int) (int64, error)
}

// ErrorDiagnosticPlaintextReader 是存储层的**可选**能力：清理历史上的明文载荷。
//
// 单独成接口而不是并入 ErrorDiagnosticRepository（与 ErrorDiagnosticCleanupBacklogReader
// 同一理由）：旧格式的存储实现与既有测试替身不该被迫长出明文层的方法。真实仓储实现它；
// 未实现时明文层的清理不可用，调用方必须把它当作明确的装配选择，既不假装清过，也不报错。
type ErrorDiagnosticPlaintextReader interface {
	// ClearExpiredErrorDiagnosticPlainBodies 在在线主库物理清除已过三十天的未关联明文正文。
	ClearExpiredErrorDiagnosticPlainBodies(ctx context.Context, now time.Time, batch int) (int64, error)
	// ClearExpiredErrorDiagnosticPlainHeaderValues 同形，属 429 头值层。
	ClearExpiredErrorDiagnosticPlainHeaderValues(ctx context.Context, now time.Time, batch int) (int64, error)
}

// ErrorDiagnosticLinkReconciler 是存储层的**可选**能力：把随后才形成使用记录的诊断补关联。
//
// 关联决定的是所有权：已关联的明文随使用记录删除，未关联的才按三十天截止。历史行里
// 可能仍有「当时没等到 usage」的明文，因此清理必须先跑这条有界批量的补偿路径，
// 否则它们会在下一轮之前被三十天清理带走，而不是随其使用记录消失。
//
// 实现必须自己验证「同一逻辑请求 + 真实尝试序号 + 上游状态」三项，绝不能用时间、账号或
// 客户端可控的原始请求 ID 兜底猜测。
type ErrorDiagnosticLinkReconciler interface {
	ReconcilePlainErrorDiagnosticLinks(ctx context.Context, now time.Time, batch int) (int64, error)
}

// ErrorDiagnosticCleanupBacklogReader 是存储层的**可选**能力：读取清理积压。
//
// 单独成接口而不是并入 ErrorDiagnosticRepository：监控探针不该强迫每个存储实现
// （以及所有测试替身）都长出这个方法。真实仓储实现它；不支持时清理照常运行，
// 只是没有积压观测，且读取方会得到明确的不支持错误，而不是被伪装成「积压为 0」。
type ErrorDiagnosticCleanupBacklogReader interface {
	ReadErrorDiagnosticCleanupBacklog(ctx context.Context, now time.Time) (ErrorDiagnosticCleanupBacklog, error)
}

// ErrorDiagnosticCleanupBacklog 是清理积压的只读监控视图。
//
// 它是「物理残留」而不是「仍可读取」：应用层在到期时刻就已拒绝读取（该读取入口已随
// 退役移除）。积压只说明清理还没跑完，因此这里也不承诺确切的物理删除时刻。
type ErrorDiagnosticCleanupBacklog struct {
	BodiesOverdue         int64
	RecordsOverdue        int64
	HeaderValuesOverdue   int64
	OldestBodyOverdueAt   time.Time
	OldestRecordOverdueAt time.Time
	OldestHeaderOverdueAt time.Time

	// 明文层的积压单独计：它们是**明文**残留，与旧密文残留的处置与风险都不同，
	// 合并成一个数会让运维看不出哪一层在落后。
	//
	// 只统计「未关联且已过 metadata_expires_at」的行：已关联的明文随使用记录存在，
	// 没有自有到期时刻，不构成清理积压。
	PlainBodiesOverdue         int64
	PlainHeaderValuesOverdue   int64
	OldestPlainBodyOverdueAt   time.Time
	OldestPlainHeaderOverdueAt time.Time
}

// OldestOverdueSeconds 返回最老的超期时长（秒），供监控直接上报。
//
// 积压为空或无有效时间时返回 0。上报秒数而不上报时刻：时长足以判断清理是否卡住，
// 又不把内部时间线细节带进监控系统。
func (b ErrorDiagnosticCleanupBacklog) OldestOverdueSeconds(now time.Time) int64 {
	oldest := time.Time{}
	for _, candidate := range []time.Time{
		b.OldestBodyOverdueAt,
		b.OldestRecordOverdueAt,
		b.OldestHeaderOverdueAt,
		b.OldestPlainBodyOverdueAt,
		b.OldestPlainHeaderOverdueAt,
	} {
		if !candidate.IsZero() && (oldest.IsZero() || candidate.Before(oldest)) {
			oldest = candidate
		}
	}
	if oldest.IsZero() {
		return 0
	}
	if seconds := now.Sub(oldest).Seconds(); seconds > 0 {
		return int64(seconds)
	}
	return 0
}

// ErrorDiagnosticMetricsSnapshot 是不含正文与凭据的计数快照。
//
// 只保留清理作业实际累加的计数：写入侧与读取侧的计数随采集／揭开入口一并移除，
// 免得快照里出现永远为 0、只能误导运维的字段。
type ErrorDiagnosticMetricsSnapshot struct {
	BodiesCleared       int64
	HeaderValuesCleared int64
	RecordsDeleted      int64
	// PlainBodiesCleared／PlainHeaderValuesCleared 是明文层的物理清除数：与旧密文层分开计，
	// 合并成一个数就无法判断「哪一层没有在清」。
	PlainBodiesCleared       int64
	PlainHeaderValuesCleared int64
	// LinksReconciled 是补上的 usage 关联数（迟到的使用记录被逐项验证后绑定）。
	LinksReconciled int64
	// CleanupFailures 是清理轮次的失败计数。
	CleanupFailures int64
	// Overdue* 是最近一次观测到的清理积压量（物理残留，不是可读范围）。
	OverdueBodies            int64
	OverdueRecords           int64
	OverdueHeaderValues      int64
	OverduePlainBodies       int64
	OverduePlainHeaderValues int64
	// OldestOverdueSeconds 是最近一次观测到的最老超期时长。
	OldestOverdueSeconds int64
}

// ErrorDiagnosticMetrics 是进程级计数，只累加离散事件，不记录正文、凭据或标识。
type ErrorDiagnosticMetrics struct {
	bodiesCleared       atomic.Int64
	headerValuesCleared atomic.Int64
	recordsDeleted      atomic.Int64
	plainBodiesCleared  atomic.Int64
	plainHeaderCleared  atomic.Int64
	linksReconciled     atomic.Int64
	cleanupFailures     atomic.Int64
	overdueBodies       atomic.Int64
	overdueRecords      atomic.Int64
	overdueHeaders      atomic.Int64
	plainBodiesOverdue  atomic.Int64
	plainHeadersOverdue atomic.Int64
	oldestOverdue       atomic.Int64
}

// NewErrorDiagnosticMetrics 创建一个进程级计数集。
func NewErrorDiagnosticMetrics() *ErrorDiagnosticMetrics { return &ErrorDiagnosticMetrics{} }

// Snapshot 返回可直接序列化的计数快照。
func (m *ErrorDiagnosticMetrics) Snapshot() ErrorDiagnosticMetricsSnapshot {
	if m == nil {
		return ErrorDiagnosticMetricsSnapshot{}
	}
	return ErrorDiagnosticMetricsSnapshot{
		BodiesCleared:            m.bodiesCleared.Load(),
		HeaderValuesCleared:      m.headerValuesCleared.Load(),
		RecordsDeleted:           m.recordsDeleted.Load(),
		PlainBodiesCleared:       m.plainBodiesCleared.Load(),
		PlainHeaderValuesCleared: m.plainHeaderCleared.Load(),
		LinksReconciled:          m.linksReconciled.Load(),
		CleanupFailures:          m.cleanupFailures.Load(),
		OverdueBodies:            m.overdueBodies.Load(),
		OverdueRecords:           m.overdueRecords.Load(),
		OverdueHeaderValues:      m.overdueHeaders.Load(),
		OverduePlainBodies:       m.plainBodiesOverdue.Load(),
		OverduePlainHeaderValues: m.plainHeadersOverdue.Load(),
		OldestOverdueSeconds:     m.oldestOverdue.Load(),
	}
}

// 清理故障的稳定告警标识：运维按事件名与原因码聚合，不依赖日志正文。
const (
	ErrorDiagnosticAlertCleanupFailed = "error_diagnostic.cleanup_failed"

	ErrorDiagnosticAlertCodeBodyClearFailed = "body_clear_failed"
	// ErrorDiagnosticAlertCodeHeaderValueClearFailed 与正文清除失败分开成不同的原因码：
	// 两段卡住的处置不同（一个是模型正文，一个是 429 头值），合并会让运维看不出是哪一段。
	ErrorDiagnosticAlertCodeHeaderValueClearFailed = "header_value_clear_failed"
	// 明文层的清除失败同样是独立原因码：明文残留与密文残留不是同一件事，
	// 而两段可以各自单独卡住。
	ErrorDiagnosticAlertCodePlainBodyClearFailed        = "plain_body_clear_failed"
	ErrorDiagnosticAlertCodePlainHeaderValueClearFailed = "plain_header_value_clear_failed"
	// ErrorDiagnosticAlertCodeLinkReconcileFailed 是补关联失败的独立原因码：
	// 关联卡住与清理卡住的后果不同（前者会让明文失去 owner，后者只影响物理残留）。
	ErrorDiagnosticAlertCodeLinkReconcileFailed = "link_reconcile_failed"
	ErrorDiagnosticAlertCodeRecordDeleteFailed  = "record_delete_failed"
	ErrorDiagnosticAlertCodeBacklog             = "cleanup_backlog"
	ErrorDiagnosticAlertCodeBacklogProbeFailed  = "backlog_probe_failed"
)

// ErrorDiagnosticAlertCleanupBacklog 是清理积压的稳定事件名。
const ErrorDiagnosticAlertCleanupBacklog = "error_diagnostic.cleanup_backlog"

// ErrorDiagnosticAlertInterval 是同一告警事件的最小输出间隔。
//
// 期间只累加被抑制的次数，不发新行；下一次到期时把累计次数一次性带出。
// 这样清理持续失败时日志量有上界，而计数不会丢。
const ErrorDiagnosticAlertInterval = 30 * time.Second

// errorDiagnosticAlerter 以有界速率输出不含敏感内容的清理故障告警。
//
// 只输出事件名、原因码与计数三类信息：绝不输出数据库错误值（约束冲突可能回显涉及的行值）、
// 正文、头值、凭据、诊断标识或协议以外的请求字段。
type errorDiagnosticAlerter struct {
	mu         sync.Mutex
	interval   time.Duration
	now        func() time.Time
	lastAt     map[string]time.Time
	suppressed map[string]int64
}

func newErrorDiagnosticAlerter(interval time.Duration, now func() time.Time) *errorDiagnosticAlerter {
	if interval <= 0 {
		interval = ErrorDiagnosticAlertInterval
	}
	if now == nil {
		now = time.Now
	}
	return &errorDiagnosticAlerter{
		interval:   interval,
		now:        now,
		lastAt:     map[string]time.Time{},
		suppressed: map[string]int64{},
	}
}

// warn 在允许的时刻输出一条告警；被抑制的次数会累积到下一次输出。
//
// 这是唯一的日志出口，且只接受稳定原因码，因此任何调用方都无法把正文或原始错误带进日志。
func (a *errorDiagnosticAlerter) warn(event, code string, total int64, extra ...zap.Field) {
	if a == nil {
		return
	}
	// 限速按「事件 + 原因码」分桶，而不是只按事件分桶：
	// 否则同一事件下两种不同的故障会互相压制，运维只会看到先出现的那一种，
	// 可能因此错过「两段清理都在失败」这类组合故障。
	key := event + "|" + code
	a.mu.Lock()
	now := a.now()
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

// warnBacklog 输出积压告警，附带积压明细（数量与最老超期时长，均非敏感内容）。
func (a *errorDiagnosticAlerter) warnBacklog(event, code string, total int64, extra ...zap.Field) {
	a.warn(event, code, total, extra...)
}
