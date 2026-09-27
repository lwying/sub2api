package service

// 独立错误诊断的**新明文层**（ADR 0007 决定 2／3；票据 08 与 09）。
//
// 旧层与新层是两套格式，各自解释、互不代替：
//
//   - 旧格式：密文列（body_ciphertext／header_ciphertext）＋ 7 天自有到期，缺密钥即不可写、
//     不可读。旧行保持原样，不回填、不延长、不升级。
//   - 新格式：明文列（plain_body_payload／plain_header_payload）＋ 随 usage 的生命周期。
//     有关联 usage 时整行随 usage 删除；没有关联时在 metadata_expires_at（创建 + 30 天）
//     整点立即拒绝读取，随后由有界周期清理物理清行。新格式**不需要任何加密密钥**。
//
// 两个新层（明文正文 / 明文 429 头值）各自默认关闭、各自有一条版本化逐字风险确认，
// 互不打开；旧共享确认不覆盖新层（见 setting_error_diagnostic_plaintext.go）。
//
// 范围与准入与旧层完全一致：只有现有诊断覆盖的三条 HTTP 分支（Messages／Chat Completions／
// Responses）的真实上游 4xx/5xx 尝试、合格（完整、≤1 MiB、文本 JSON、无附件、无已知结构化
// 凭据）的请求正文才可能落明文；429 头值仍然只对真实 Claude Messages 上游 429 的白名单头名。
// 本文件不放松任何一条准入，也不新增任何采集入口。

import (
	"time"
)

const (
	// ErrorDiagnosticPlainBodyRetained 是**新明文**正文留存的原因码。
	//
	// 与旧密文层的 retained 分开：管理端据此能看出这一行是明文格式，
	// 而不是「密文但没写密钥代」。
	ErrorDiagnosticPlainBodyRetained = "plain_body_retained"

	// ErrorDiagnosticPlainHeaderRetained 是**新明文** 429 头值留存的原因码。
	ErrorDiagnosticPlainHeaderRetained = "plain_header_retained"
)

// ErrorDiagnosticPlainBodyDecision 是对一次尝试的新明文正文结论。
//
// Payload 只在 State == stored 时非空。它是**明文**：调用方直接把字节交给存储层，
// 不经任何加密，也不得在缺密钥时改写成「未留存」——新格式本来就没有密钥这一环。
type ErrorDiagnosticPlainBodyDecision struct {
	State   string
	Reason  string
	Payload []byte
}

// Retained 报告本次结论是否允许写入明文正文。
func (d ErrorDiagnosticPlainBodyDecision) Retained() bool {
	return d.State == ErrorDiagnosticBodyStateStored && len(d.Payload) > 0
}

// DecideErrorDiagnosticPlainBody 判定该次尝试的新明文正文状态与原因。
//
// 判定顺序与旧密文层一致（verdict → 体量 → 读取完整性 → 内容分类 → 开关），
// 只有一点不同：**没有密钥这一步**。密钥缺席在旧层是「要求过、做不到」的配置故障，
// 在新层根本不成立，因此这里也不接受、不解释任何密钥状态。
//
// 准入一字未改：不合格的正文整份不留，且必须留下稳定原因码，绝不做截断、
// 绝不脱敏后仍称「完整」，也绝不用明文回退去掩盖准入失败。
//
// 传输层的 suppressed_encryption_unavailable 结论只描述**旧密文层**「要求过留存而稳定密钥
// 缺席」，与新明文层无关：它在这里按未采集（not_observed）处理，因为调用方确实一个字节都
// 没交出来——把它写成 skipped_encryption_unavailable 既不在新层的封闭原因码集合内，
// 也会把「旧层的密钥问题」错误地记到新层上。
func DecideErrorDiagnosticPlainBody(attempt ErrorDiagnosticAttempt, captureAllowed bool) ErrorDiagnosticPlainBodyDecision {
	switch attempt.BodyVerdict {
	case ErrorDiagnosticBodyVerdictNotRequested, ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable:
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateNotObserved, Reason: ErrorDiagnosticBodyNotObserved}
	case ErrorDiagnosticBodyVerdictTooLarge:
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateSkipped, Reason: ErrorDiagnosticBodySkippedTooLarge}
	case ErrorDiagnosticBodyVerdictIncomplete:
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateSkipped, Reason: ErrorDiagnosticBodySkippedIncompleteRead}
	}
	if len(attempt.Body) == 0 {
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateNotObserved, Reason: ErrorDiagnosticBodyNotObserved}
	}
	if len(attempt.Body) > ErrorDiagnosticMaxBodyBytes {
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateSkipped, Reason: ErrorDiagnosticBodySkippedTooLarge}
	}
	if !attempt.BodyReadComplete {
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateSkipped, Reason: ErrorDiagnosticBodySkippedIncompleteRead}
	}
	classification := ClassifyErrorDiagnosticBody(attempt.Body)
	if !classification.IsTextJSON {
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateSkipped, Reason: ErrorDiagnosticBodySkippedNotTextJSON}
	}
	if classification.HasAttachment {
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateSkipped, Reason: ErrorDiagnosticBodySkippedAttachment}
	}
	if classification.HasKnownCredential {
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateSkipped, Reason: ErrorDiagnosticBodySkippedKnownCredential}
	}
	if !captureAllowed {
		return ErrorDiagnosticPlainBodyDecision{State: ErrorDiagnosticBodyStateSkipped, Reason: ErrorDiagnosticBodySkippedRetentionDisabled}
	}
	// 明文留存是**复制**：调用方在回调返回后立即清零自己的缓冲，因此这里必须持有一份字节。
	// 只在结论确定要留存时复制，绝不「先复制再判断」。
	payload := append([]byte(nil), attempt.Body...)
	return ErrorDiagnosticPlainBodyDecision{
		State:   ErrorDiagnosticBodyStateStored,
		Reason:  ErrorDiagnosticPlainBodyRetained,
		Payload: payload,
	}
}

// ErrorDiagnosticPlainHeaderDecision 是对一次尝试的新明文 429 头值结论。
type ErrorDiagnosticPlainHeaderDecision struct {
	State      string
	Reason     string
	Payload    []byte
	EntryCount int
}

// Retained 报告本次结论是否允许写入明文头值。
func (d ErrorDiagnosticPlainHeaderDecision) Retained() bool {
	return d.State == ErrorDiagnosticHeaderStateStored && len(d.Payload) > 0
}

// DecideErrorDiagnosticPlainHeaderValues 判定该次尝试的新明文 429 头值结论。
//
// 范围、白名单与「整份或全无」策略与旧密文层完全一致（同一个净化结果、同一份闭集名单、
// 同一套有界校验），只有两点不同：
//
//   - **没有密钥这一步**：新格式不加密，因此密钥缺席不影响它，也不产生任何原因码；
//   - 传输层的 suppressed_encryption_unavailable 结论按未采集处理，理由与正文层相同。
//
// 因此同一次 429 在两处（usage-owned 值明细与独立诊断）可以有不同结论是允许的，
// 但同一处的新旧两层之间不存在「净化结论不同」——净化器只有一份。
func DecideErrorDiagnosticPlainHeaderValues(attempt ErrorDiagnosticAttempt, captureAllowed bool) ErrorDiagnosticPlainHeaderDecision {
	if attempt.HeaderVerdict == ErrorDiagnosticHeaderVerdictOutOfScope {
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateNotObserved, Reason: ErrorDiagnosticHeaderNotObserved}
	}
	if !ErrorDiagnosticHeaderScopeApplies(attempt.Protocol, attempt.UpstreamStatusCode) {
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateNotObserved, Reason: ErrorDiagnosticHeaderNotObserved}
	}
	switch attempt.HeaderVerdict {
	case ErrorDiagnosticHeaderVerdictSuppressedEncryptionUnavailable,
		ErrorDiagnosticHeaderVerdictNotApplicable,
		ErrorDiagnosticHeaderVerdictEmpty:
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateNotObserved, Reason: ErrorDiagnosticHeaderNotObserved}
	case ErrorDiagnosticHeaderVerdictInvalidValues:
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedInvalidValues}
	case ErrorDiagnosticHeaderVerdictNotRequested:
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedRetentionDisabled}
	case ErrorDiagnosticHeaderVerdictCaptured, "":
		// 空 verdict 表示调用方只给值、由服务自行判定（与正文 verdict 的约定一致）。
	default:
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedInvalidValues}
	}
	normalized, count, ok := NormalizeErrorDiagnosticHeaderValues(attempt.HeaderValues)
	if !ok {
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedInvalidValues}
	}
	if count == 0 {
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateNotObserved, Reason: ErrorDiagnosticHeaderNotObserved}
	}
	if !captureAllowed {
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedRetentionDisabled}
	}
	payload, err := EncodeErrorDiagnosticHeaderValues(normalized)
	if err != nil || len(payload) == 0 {
		return ErrorDiagnosticPlainHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedInvalidValues}
	}
	return ErrorDiagnosticPlainHeaderDecision{
		State:      ErrorDiagnosticHeaderStateStored,
		Reason:     ErrorDiagnosticPlainHeaderRetained,
		Payload:    payload,
		EntryCount: count,
	}
}

// DescribePlainBodyState 把新明文正文的原因码与可读性映射成对外的 body_state。
//
// 与 DescribeBodyState 同构，但到期判据换成新格式的两条规则：
//   - 已关联 usage：没有自有窗口，只要载荷还在就是 stored；
//   - 未关联：过了 metadata_expires_at 即为 expired（即使清理还没跑）。
func DescribePlainBodyState(reason string, stored bool, linked bool, metadataExpiresAt, now time.Time) string {
	if stored {
		if !linked && !metadataExpiresAt.After(now) {
			return ErrorDiagnosticBodyStateExpired
		}
		return ErrorDiagnosticBodyStateStored
	}
	switch reason {
	case ErrorDiagnosticPlainBodyRetained:
		// 曾留存但明文已被清理：在线主库已无正文。
		return ErrorDiagnosticBodyStatePurged
	case ErrorDiagnosticBodyNotObserved, "":
		return ErrorDiagnosticBodyStateNotObserved
	default:
		return ErrorDiagnosticBodyStateSkipped
	}
}

// DescribePlainHeaderState 是新明文 429 头值层的同构函数。
func DescribePlainHeaderState(reason string, stored bool, linked bool, metadataExpiresAt, now time.Time) string {
	if stored {
		if !linked && !metadataExpiresAt.After(now) {
			return ErrorDiagnosticHeaderStateExpired
		}
		return ErrorDiagnosticHeaderStateStored
	}
	switch reason {
	case ErrorDiagnosticPlainHeaderRetained:
		return ErrorDiagnosticHeaderStatePurged
	case ErrorDiagnosticHeaderNotObserved, "":
		return ErrorDiagnosticHeaderStateNotObserved
	default:
		return ErrorDiagnosticHeaderStateSkipped
	}
}
