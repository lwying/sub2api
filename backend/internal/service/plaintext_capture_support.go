package service

// 三类新明文采集的**部署前提**：值明细、独立诊断正文、独立诊断 429 头值（ADR 0007；
// 票据 10）。三者的共同点是「明文落库，并在可靠关联到使用记录后随该使用记录删除」。
//
// 这个生命周期不是应用代码兜出来的：数据库里真正的保证是两张旁路表到 usage_logs 的
// ON DELETE CASCADE 所有权外键（迁移 253 的 request_audit_value_details.usage_log_id、
// 迁移 255 的 error_diagnostic_records.plain_owner_usage_log_id）。一旦部署把这些外键
// 去掉，usage 行的逐行 DELETE 就不再级联，明文会变成任何外键动作都回收不了的孤儿。
//
// 为什么必须**在开启之前**就拦住，而不是事后扫描：
//   - PostgreSQL 要求被引用键在整张分区表上唯一，因此迁移 253/255 的单列所有权外键在
//     **分区 usage_logs** 上根本建不出来。想走分区部署，部署方只能二选一：改写成
//     (id, created_at) 复合外键（分区 DROP 被 PostgreSQL 的依赖检查拒绝，保留期推进
//     卡死），或者把外键去掉（逐行 DELETE 失去级联 ⇒ 孤儿明文）。
//   - 两条都不受支持。ADR 0007 的验收口径是：不能保证「删除 usage 当次即不可读」的
//     部署组合，必须禁止在该部署下开启新明文，而不是退化成「稍后由异步孤儿扫描补」。
//
// 因此本判定被三处消费：
//   - 采集侧（fail closed）：值明细门控与两个新明文诊断层都要求 Supported；
//   - 开启侧：运维更新要求 Supported，否则拒绝开启（关闭永远放行，不查探针）；
//   - 运维状态：把 Supported 与**原因码**一起回显，使「存量开着但不允许采集」永远有
//     一条可解释的理由（分区 vs 外键缺失 vs 探针查不出来），且不泄露数据库错误原文。
//
// 边界：只影响上述三类**新明文采集**。诊断元数据采集、旧密文正文/头值留存、任何旧记录
// 的读取都不经过这里——旧格式的七天/三十天读取不受部署形态影响。

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 支持结论的原因码（闭集）。回显给管理员的就是这些字符串，绝不包含数据库错误原文。
const (
	// PlaintextCaptureSupportReasonSupported 表示数据库形态满足三类新明文采集的前提：
	// usage_logs 是普通表，且已部署的明文旁路表都还持有级联所有权外键。
	PlaintextCaptureSupportReasonSupported = "supported"
	// PlaintextCaptureSupportReasonPartitionedUsageLogs 表示 usage_logs 是分区表。
	//
	// 这不是「暂时查不到」：单列所有权外键在分区父表上无法建立，分区 DROP 又不会触发行级
	// 外键动作，因此该形态无法保证明文随 usage 消失。
	PlaintextCaptureSupportReasonPartitionedUsageLogs = "unsupported_partitioned_usage_logs"
	// PlaintextCaptureSupportReasonMissingOwnership 表示旁路表仍在，但它的级联所有权外键
	// 已经被去掉：usage 行的逐行 DELETE 不再带走明文。
	PlaintextCaptureSupportReasonMissingOwnership = "unsupported_missing_ownership_foreign_key"
	// PlaintextCaptureSupportReasonUnknownDeployment 表示探针给出了闭集外的形态，或无法判定
	// 为受支持。未知一律按不支持处理，并保留一个稳定原因码而不是回落成「支持」。
	PlaintextCaptureSupportReasonUnknownDeployment = "unsupported_unknown_deployment"
	// PlaintextCaptureSupportReasonProbeFailed 表示探针查不出来（连接/权限/目录查询失败）。
	//
	// 它与「部署形态不支持」是两件不同的事实，必须分开显示；但两者结论同向：fail closed。
	PlaintextCaptureSupportReasonProbeFailed = "probe_failed"
	// PlaintextCaptureSupportReasonProbeUnavailable 表示进程里根本没有注入探针（配置缺失）。
	//
	// 与 probe_failed 分开：一个是「这台部署没接上探针」，一个是「接上了但此刻查不出来」。
	PlaintextCaptureSupportReasonProbeUnavailable = "probe_unavailable"
)

// PlaintextCaptureSupport 是数据库层面对三类新明文采集的支持结论。
type PlaintextCaptureSupport struct {
	Supported bool `json:"supported"`
	// Reason 取上面闭集之一。Supported 为真时固定是 supported。
	Reason string `json:"reason"`
}

// PlaintextCaptureSupportProbe 由仓储层实现：只读系统目录（pg_class/pg_constraint），
// 不读业务行，也不把数据库错误原文交给响应面。
//
// 返回 (不支持, 原因码) 表示形态可判定但不支持；返回 error 表示**判不出来**，
// 调用方一律按 probe_failed 处理，且不把 error 带进采集热路径。
type PlaintextCaptureSupportProbe interface {
	ProbePlaintextCaptureSupport(ctx context.Context) (PlaintextCaptureSupport, error)
}

// plaintextCaptureSupportCacheTTL 是支持结论的进程内缓存时长。
//
// 采集接缝按上游尝试调用本判定，不能每次请求都去查系统目录；缓存对「不支持」同样生效，
// 避免在不支持的部署上反复打库。代价是结论最多滞后一个 TTL：部署刚被改成不支持时，
// 最多再多采 TTL 时长的明文——门控要挡的是**开启**，不是把运行期数据库形态当成逐请求事实。
const plaintextCaptureSupportCacheTTL = 30 * time.Second

const plaintextCaptureSupportSFKey = "plaintext_capture_support"

type cachedPlaintextCaptureSupport struct {
	support   PlaintextCaptureSupport
	expiresAt time.Time
}

var (
	// ErrRequestAuditValueDetailDeploymentUnsupported 表示本部署的数据库形态无法保证明文
	// 值明细随 usage 消失，因此拒绝开启。
	//
	// 关闭永不受它影响（关闭路径不查探针）。原因码通过 metadata（plaintext_capture_support）
	// 带回，错误正文里不含任何数据库错误原文。
	ErrRequestAuditValueDetailDeploymentUnsupported = infraerrors.Conflict(
		"REQUEST_AUDIT_VALUE_DETAIL_DEPLOYMENT_UNSUPPORTED",
		"plaintext value detail capture cannot be enabled on this deployment: the database cannot guarantee that plaintext rows disappear together with their usage record",
	)
	// ErrErrorDiagnosticPlaintextDeploymentUnsupported 同形，属于两个新明文诊断层。
	//
	// 与值明细分开成两个哨兵：被拒绝的是两张表、两条流水线，界面必须能说清是哪一层开不了。
	ErrErrorDiagnosticPlaintextDeploymentUnsupported = infraerrors.Conflict(
		"ERROR_DIAGNOSTIC_PLAINTEXT_DEPLOYMENT_UNSUPPORTED",
		"plaintext error diagnostic capture cannot be enabled on this deployment: the database cannot guarantee that plaintext rows disappear together with their usage record",
	)
)

// SetPlaintextCaptureSupportProbe 注入部署探针（启动时一次；未注入时三类新明文采集一律
// 关闭，见 probePlaintextCaptureSupportFresh）：探针是明文能力的前置依赖，不是可选优化。
func (s *SettingService) SetPlaintextCaptureSupportProbe(probe PlaintextCaptureSupportProbe) {
	if s == nil {
		return
	}
	s.plaintextCaptureSupportProbe = probe
}

// PlaintextCaptureSupport 返回本部署对新明文采集的支持结论（带短 TTL 缓存，fail closed）。
//
// 缓存与单飞只影响「查询频率」，不影响「不支持时一定不放行」：探针缺失、探针报错、
// 给出闭集外的形态，全部落到不支持。
func (s *SettingService) PlaintextCaptureSupport(ctx context.Context) PlaintextCaptureSupport {
	if s == nil {
		return PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonProbeUnavailable}
	}
	if cached, ok := s.plaintextCaptureSupportCache.Load().(*cachedPlaintextCaptureSupport); ok && cached != nil {
		if time.Now().Before(cached.expiresAt) {
			return cached.support
		}
	}
	value, _, _ := s.plaintextCaptureSupportSF.Do(plaintextCaptureSupportSFKey, func() (any, error) {
		support := s.probePlaintextCaptureSupportFresh(ctx)
		s.plaintextCaptureSupportCache.Store(&cachedPlaintextCaptureSupport{
			support:   support,
			expiresAt: time.Now().Add(plaintextCaptureSupportCacheTTL),
		})
		return support, nil
	})
	support, ok := value.(PlaintextCaptureSupport)
	if !ok {
		// 单飞只可能返回上面存进去的结论；取不出来按「问不到探针」处理，绝不猜「支持」。
		return PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonProbeUnavailable}
	}
	return support
}

// probePlaintextCaptureSupportFresh 直接问一次探针，**绕过缓存**。
//
// 开启侧（运维更新）必须走这条路径：缓存里的 supported 可能来自几分钟前的数据库形态，
// 用它放行一次不可逆的开启，等于让缓存替数据库做决定。
func (s *SettingService) probePlaintextCaptureSupportFresh(ctx context.Context) PlaintextCaptureSupport {
	if s == nil || s.plaintextCaptureSupportProbe == nil {
		// 没有探针就没有「明文能随 usage 消失」的证明。这是配置缺失，不是数据库故障，
		// 因此与 probe_failed 分开报告。
		return PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonProbeUnavailable}
	}
	support, err := s.plaintextCaptureSupportProbe.ProbePlaintextCaptureSupport(ctx)
	if err != nil {
		// 数据库错误原文不出这个函数：调用方只拿稳定原因码，响应面与日志都不带库内细节。
		return PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonProbeFailed}
	}
	return normalizePlaintextCaptureSupport(support)
}

// rejectPlaintextCaptureEnablement 是开启三类新明文前的唯一前置校验。
//
// 返回 nil 表示可以开启；否则返回带原因码的稳定错误（metadata.plaintext_capture_support），
// 让管理员在 PUT 的响应里就能看到是哪一侧挡住了，而不是只能事后去读状态。
// 它**只在开启路径上被调用**：关闭永远不需要探针，也永远不会被它挡住。
func (s *SettingService) rejectPlaintextCaptureEnablement(ctx context.Context, sentinel *infraerrors.ApplicationError) error {
	if s == nil {
		return withPlaintextCaptureSupportReason(sentinel, PlaintextCaptureSupportReasonProbeUnavailable)
	}
	support := s.probePlaintextCaptureSupportFresh(ctx)
	// 把刚刚这次**新鲜**结论写回缓存：更新响应里随后回显的运维状态因此与本次判定同源，
	// 不会出现「刚放行开启、状态却按 30 秒前的旧结论说不能采集」。
	s.rememberPlaintextCaptureSupport(support)
	if support.Supported {
		return nil
	}
	return withPlaintextCaptureSupportReason(sentinel, support.Reason)
}

// rememberPlaintextCaptureSupport 把一个新鲜结论写进进程内缓存。
func (s *SettingService) rememberPlaintextCaptureSupport(support PlaintextCaptureSupport) {
	if s == nil {
		return
	}
	s.plaintextCaptureSupportCache.Store(&cachedPlaintextCaptureSupport{
		support:   support,
		expiresAt: time.Now().Add(plaintextCaptureSupportCacheTTL),
	})
}

// normalizePlaintextCaptureSupport 把探针结论收敛到闭集：不支持却给了闭集外的原因码时改判
// 为未知形态，使运维状态里永远只出现可枚举、可翻译的原因码。
func normalizePlaintextCaptureSupport(support PlaintextCaptureSupport) PlaintextCaptureSupport {
	if support.Supported {
		return PlaintextCaptureSupport{Supported: true, Reason: PlaintextCaptureSupportReasonSupported}
	}
	switch support.Reason {
	case PlaintextCaptureSupportReasonPartitionedUsageLogs,
		PlaintextCaptureSupportReasonMissingOwnership,
		PlaintextCaptureSupportReasonUnknownDeployment:
		return PlaintextCaptureSupport{Reason: support.Reason}
	default:
		return PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonUnknownDeployment}
	}
}

// withPlaintextCaptureSupportReason 把原因码挂到开启被拒的错误上，供响应体的 metadata 使用。
//
// 只挂原因码：数据库错误原文、库名、连接串都不进响应面。errors.Is 仍按 code+reason 匹配
// 原哨兵（ApplicationError.Is 只比较这两项），因此调用方与测试都能照常判断错误类别。
func withPlaintextCaptureSupportReason(sentinel *infraerrors.ApplicationError, reason string) error {
	return sentinel.WithMetadata(map[string]string{"plaintext_capture_support": reason})
}
