package service

// 新明文采集的**部署前提**结论：这是给部署探针及其消费方共用的一组词汇
// （结论结构、原因码闭集、把探针原始结论收敛到闭集、把原因码挂到开启被拒的错误上）。
//
// 唯一还在使用这套词汇的探针是 Trace 的 RequestTraceSupportProbe
// （见 request_trace_settings.go；实现见 repository/request_trace_support_repo.go）。
// 旧的「三类新明文采集部署探针」（值明细、独立诊断正文、独立诊断 429 头值）已随旧值明细／
// 错误诊断采集一同退役（票据 10）：本文件不再声明探针接口，也不再保留任何旧探针的调用接缝。
//
// 生命周期保证不是应用代码兜出来的：数据库里真正的保证是旁路表到 usage_logs 的
// ON DELETE CASCADE 所有权外键（迁移 258 的 request_traces.usage_log_id）。一旦部署把这些
// 外键去掉，usage 行的逐行 DELETE 就不再级联，明文会变成任何外键动作都回收不了的孤儿。
//
// 为什么必须**在开启之前**就拦住，而不是事后扫描：
//   - PostgreSQL 要求被引用键在整张分区表上唯一，因此单列所有权外键在**分区 usage_logs**
//     上根本建不出来。想走分区部署，部署方只能二选一：改写成 (id, created_at) 复合外键
//     （分区 DROP 被 PostgreSQL 的依赖检查拒绝，保留期推进卡死），或者把外键去掉
//     （逐行 DELETE 失去级联 ⇒ 孤儿明文）。
//   - 两条都不受支持。不能保证「删除 usage 当次即不可读」的部署组合，必须禁止在该部署下
//     开启新明文，而不是退化成「稍后由异步孤儿扫描补」。
//
// 因此本结论被三处消费：
//   - 采集侧（fail closed）：Trace 门控要求 Supported；
//   - 开启侧：运维更新要求 Supported，否则拒绝开启（关闭永远放行，不查探针）；
//   - 运维状态：把 Supported 与**原因码**一起回显，使「存量开着但不允许采集」永远有
//     一条可解释的理由（分区 vs 外键缺失 vs 探针查不出来），且不泄露数据库错误原文。
//
// 边界：只影响**新明文采集**。诊断元数据采集、旧密文正文/头值留存、任何旧记录的读取都不
// 经过这里——旧格式的七天/三十天读取不受部署形态影响。

import (
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 支持结论的原因码（闭集）。回显给管理员的就是这些字符串，绝不包含数据库错误原文。
const (
	// PlaintextCaptureSupportReasonSupported 表示数据库形态满足新明文采集的前提：
	// usage_logs 是普通表，且探针所验证的明文旁路表（Trace 表）还持有级联所有权外键。
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

// PlaintextCaptureSupport 是数据库层面对新明文采集的支持结论。
type PlaintextCaptureSupport struct {
	Supported bool `json:"supported"`
	// Reason 取上面闭集之一。Supported 为真时固定是 supported。
	Reason string `json:"reason"`
}

// plaintextCaptureSupportCacheTTL 是支持结论的进程内缓存时长。
//
// 采集接缝按上游尝试调用本判定，不能每次请求都去查系统目录；缓存对「不支持」同样生效，
// 避免在不支持的部署上反复打库。代价是结论最多滞后一个 TTL：部署刚被改成不支持时，
// 最多再多采 TTL 时长的明文——门控要挡的是**开启**，不是把运行期数据库形态当成逐请求事实。
const plaintextCaptureSupportCacheTTL = 30 * time.Second

type cachedPlaintextCaptureSupport struct {
	support   PlaintextCaptureSupport
	expiresAt time.Time
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
