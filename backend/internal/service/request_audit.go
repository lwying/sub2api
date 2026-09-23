package service

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// RequestAuditInput 是从逻辑请求收集的审计输入。Body 只用于调用方持有，不得写入记录。
type RequestAuditInput struct {
	UsageLogID        int64
	Headers           http.Header
	Body              []byte
	SSEEvents         []RequestAuditSSEEvent
	Attempts          []RequestAuditAttempt
	NotCapturedReason string
	// PartialReason 表示采集不完整但仍落了部分事实（如 cyber 拒绝路径只有上游传输尝试、
	// 没有响应事实）。与 NotCapturedReason 互斥且优先级更低：NotCapturedReason 表示范围外
	// 未采集，一旦设置就不再附加尝试或头。设置 PartialReason 的审计永不为 complete。
	PartialReason    string
	ClientDisconnect bool
	StreamIncomplete bool
	Fingerprint      *RequestAuditFingerprintInput
	Metadata         RequestAuditMetadata
}

// RequestAuditSSEEvent 是流式事件的原始采集输入。Data 不得写入审计记录。
type RequestAuditSSEEvent struct {
	Type         string
	Data         []byte
	Bytes        int
	capture      *requestAuditSSECaptureState
	auditSummary *RequestAuditSSESummary
}

// RequestAuditSSESummary is internal accounting for a capped streaming skeleton.
type RequestAuditSSESummary struct {
	Original      int
	OriginalBytes int
	Kept          int
	KeptBytes     int
	Dropped       int
	DroppedBytes  int
	Reason        string
}

// RequestAuditFingerprintInput contains precomputed keyed digests and the public key version.
// The raw key and per-record salt never leave the service layer.
type RequestAuditFingerprintInput struct {
	RequestDigest string
	KeyVersion    int
	Events        map[int]string
	record        *requestAuditFingerprintRecord
}

// RequestAuditEventSkeleton 是流式事件的协议骨架，不含事件文本或增量。
type RequestAuditEventSkeleton struct {
	Type          string `json:"type"`
	Index         int    `json:"index"`
	Bytes         int    `json:"bytes"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	Original      int    `json:"original,omitempty"`
	OriginalBytes int    `json:"original_bytes,omitempty"`
	Kept          int    `json:"kept,omitempty"`
	KeptBytes     int    `json:"kept_bytes,omitempty"`
	Dropped       int    `json:"dropped,omitempty"`
	DroppedBytes  int    `json:"dropped_bytes,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// 采集完整性取值（CONTEXT.md）：完整、截断、响应中途不完整、写入失败、第一阶段未采集。
const (
	RequestAuditCaptureComplete    = "complete"
	RequestAuditCaptureTruncated   = "truncated"
	RequestAuditCaptureIncomplete  = "incomplete"
	RequestAuditCaptureWriteFailed = "write_failed"
	RequestAuditCaptureNotCaptured = "not_captured"
)

const RequestAuditNotCapturedReasonPhase1Uncovered = "phase1_uncovered"

// RequestAuditPartialReasonCyberPolicy 是 cyber 拒绝路径的稳定不完整原因：
// 该路径 forward 返回 nil result，只有上游传输尝试事实（状态/头/字节），没有响应事实
// （事件骨架、终止状态），因此审计按 incomplete 落库并带这个原因码，绝不标 complete。
const RequestAuditPartialReasonCyberPolicy = "cyber_policy_audit_partial"

var requestAuditOrdinaryWriteFailures atomic.Uint64

// RequestAuditOrdinaryWriteFailureCount counts audit write failures in this process.
func RequestAuditOrdinaryWriteFailureCount() uint64 {
	return requestAuditOrdinaryWriteFailures.Load()
}

// RequestAuditAttempt 是一次上游尝试的协议元数据，不含模型正文。
type RequestAuditAttempt struct {
	AccountID               int64          `json:"account_id,omitempty"`
	Model                   string         `json:"model,omitempty"`
	ModelFingerprint        string         `json:"model_fingerprint,omitempty"`
	Protocol                string         `json:"protocol,omitempty"`
	Stage                   string         `json:"stage,omitempty"`
	WireRequestHeaders      map[string]any `json:"wire_request_headers,omitempty"`
	UpstreamResponseHeaders map[string]any `json:"upstream_response_headers,omitempty"`
	UpstreamStatus          *int           `json:"upstream_status,omitempty"`
	RequestPayloadBytes     *int64         `json:"request_payload_bytes,omitempty"`
	ResponsePayloadBytes    *int64         `json:"response_payload_bytes,omitempty"`
	ResponseReadComplete    *bool          `json:"response_read_complete,omitempty"`
}

const (
	RequestAuditStageClientEntry   = "client_entry"
	RequestAuditStagePostNormalize = "post_normalize"
	RequestAuditStageWire          = "wire"
	RequestAuditProtocolOpenAIChat = "openai.chat.completions"
	RequestAuditProtocolOpenAIResp = "openai.responses"
	RequestAuditProtocolAnthropic  = "anthropic.messages"
)

// RequestAuditClientResponseKey 是 Status / Bytes 唯一的白名单键：四阶段中
// 「返回客户端的响应」阶段的事实。Status 为已提交的 HTTP 状态码，Bytes 为已写
// 到客户端的响应体字节数（观测到的 0 是事实，不是缺值）。其他 Status / Bytes
// 键一律丢弃，避免任意阶段自造键名。
const RequestAuditClientResponseKey = "client_response"

// RequestAuditRecord 挂在使用记录上的协议元数据，不含模型正文。
type RequestAuditMetadata struct {
	Routes         map[string]string           `json:"routes,omitempty"`
	IDs            map[string]string           `json:"ids,omitempty"`
	Status         map[string]int              `json:"status,omitempty"`
	Bytes          map[string]int64            `json:"bytes,omitempty"`
	Tokens         map[string]int              `json:"tokens,omitempty"`
	ProtocolFields *RequestAuditProtocolFields `json:"protocol_fields,omitempty"`
}

// RequestAuditProtocolFields contains only closed, non-body protocol metadata.
type RequestAuditProtocolFields struct {
	Stream           *bool    `json:"stream,omitempty"`
	ThinkingType     string   `json:"thinking_type,omitempty"`
	PresentFields    []string `json:"present_fields,omitempty"`
	NormalizedFields []string `json:"normalized_fields,omitempty"`
}

type RequestAuditRecord struct {
	UsageLogID            int64                       `json:"usage_log_id"`
	Headers               map[string]any              `json:"headers"`
	Events                []RequestAuditEventSkeleton `json:"events"`
	Attempts              []RequestAuditAttempt       `json:"attempts,omitempty"`
	CaptureCompleteness   string                      `json:"capture_completeness,omitempty"`
	CaptureReason         string                      `json:"capture_reason,omitempty"`
	RequestFingerprint    string                      `json:"request_fingerprint,omitempty"`
	FingerprintKeyVersion int                         `json:"fingerprint_key_version,omitempty"`
	FingerprintSalt       []byte                      `json:"-"`
	Metadata              RequestAuditMetadata        `json:"metadata,omitempty"`
}

// RequestAuditRepository 写入并按使用记录读取请求审计。
type RequestAuditRepository interface {
	CreateRequestAudit(ctx context.Context, rec *RequestAuditRecord) error
	GetByUsageLogID(ctx context.Context, usageLogID int64) (*RequestAuditRecord, error)
}

type RequestAuditReservationScope struct {
	LogicalKey  string
	RouteFamily RequestAuditRouteFamily
	Forced      bool
	Headers     http.Header
	ExpiresAt   time.Time
}

type RequestAuditReservationRepository interface {
	ReserveAttempt(ctx context.Context, scope RequestAuditReservationScope, attempt RequestAuditAttempt) error
	FinalizeReservation(ctx context.Context, logicalKey string, usageLogID int64, rec *RequestAuditRecord) error
	MarkReservationIncomplete(ctx context.Context, logicalKey string, usageLogID int64, reason string) error
	DeleteExpiredReservations(ctx context.Context, before time.Time, limit int) (int64, error)
}

const (
	requestAuditMaxSSEEvents = 2000
	requestAuditMaxSSEBytes  = 256 * 1024
)

// SanitizeRequestAuditHeaders keeps only protocol-safe request header values and credential presence.
func SanitizeRequestAuditHeaders(h http.Header) map[string]any {
	return httpattempt.SanitizeRequestHeaders(h)
}

// SanitizeRequestAuditHeadersSnapshot removes credentials before an async worker or reservation receives headers.
func SanitizeRequestAuditHeadersSnapshot(h http.Header) http.Header {
	return httpattempt.HeaderFromSanitizedMap(SanitizeRequestAuditHeaders(h))
}

// SanitizeRequestAuditHeaderMap validates a stored or derived request header map again at the persistence boundary.
func SanitizeRequestAuditHeaderMap(values map[string]any) map[string]any {
	return httpattempt.SanitizeRequestHeaderMap(values)
}

// SanitizeRequestAuditResponseHeaderMap validates a response header map again before storage or disclosure.
func SanitizeRequestAuditResponseHeaderMap(values map[string]any) map[string]any {
	return httpattempt.SanitizeResponseHeaderMap(values)
}

// BuildRequestAuditRecord 从输入构造审计记录，丢弃模型正文。
func BuildRequestAuditRecord(in RequestAuditInput) *RequestAuditRecord {
	if in.NotCapturedReason != "" {
		metadata := SanitizeRequestAuditMetadata(in.Metadata)
		metadata.ProtocolFields = nil
		return &RequestAuditRecord{
			UsageLogID:            in.UsageLogID,
			Headers:               map[string]any{},
			CaptureCompleteness:   RequestAuditCaptureNotCaptured,
			CaptureReason:         sanitizeRequestAuditCaptureReason(requestAuditCaptureReason(in)),
			RequestFingerprint:    fingerprintDigest(in.Fingerprint),
			FingerprintKeyVersion: fingerprintKeyVersion(in.Fingerprint),
			FingerprintSalt:       requestAuditFingerprintSalt(in.Fingerprint),
			Metadata:              metadata,
		}
	}
	events := buildRequestAuditEventSkeletonsWithFingerprint(in.SSEEvents, in.Fingerprint)
	return &RequestAuditRecord{
		UsageLogID:            in.UsageLogID,
		Headers:               SanitizeRequestAuditHeaders(in.Headers),
		Events:                events,
		Attempts:              buildRequestAuditAttempts(in.Attempts),
		CaptureCompleteness:   requestAuditCaptureCompleteness(in, events),
		CaptureReason:         sanitizeRequestAuditCaptureReason(requestAuditCaptureReason(in)),
		RequestFingerprint:    fingerprintDigest(in.Fingerprint),
		FingerprintKeyVersion: fingerprintKeyVersion(in.Fingerprint),
		FingerprintSalt:       requestAuditFingerprintSalt(in.Fingerprint),
		Metadata:              SanitizeRequestAuditMetadata(in.Metadata),
	}
}

func shouldAttachRequestAudit(in RequestAuditInput) bool {
	if in.NotCapturedReason != "" {
		return true
	}
	if in.PartialReason != "" {
		return true
	}
	if in.ClientDisconnect {
		return true
	}
	if len(in.Attempts) > 0 || len(in.SSEEvents) > 0 {
		return true
	}
	return len(SanitizeRequestAuditHeaders(in.Headers)) > 0
}

// requestAuditCaptureReason 返回落库的原因码：未采集优先，其次是局部采集的不完整原因。
func requestAuditCaptureReason(in RequestAuditInput) string {
	if in.NotCapturedReason != "" {
		return in.NotCapturedReason
	}
	return in.PartialReason
}

func requestAuditCaptureCompleteness(in RequestAuditInput, events []RequestAuditEventSkeleton) string {
	if in.NotCapturedReason != "" {
		return RequestAuditCaptureNotCaptured
	}
	if in.ClientDisconnect || in.StreamIncomplete || in.PartialReason != "" {
		return RequestAuditCaptureIncomplete
	}
	for _, ev := range events {
		if ev.Truncated || ev.Type == "truncated" {
			return RequestAuditCaptureTruncated
		}
	}
	return RequestAuditCaptureComplete
}

func fingerprintDigest(in *RequestAuditFingerprintInput) string {
	if in == nil {
		return ""
	}
	return in.RequestDigest
}

func fingerprintKeyVersion(in *RequestAuditFingerprintInput) int {
	if in == nil {
		return 0
	}
	return in.KeyVersion
}

func requestAuditFingerprintSalt(in *RequestAuditFingerprintInput) []byte {
	if in == nil {
		return nil
	}
	return in.Salt()
}

func requestAuditSSEEventBytes(ev RequestAuditSSEEvent) int {
	if ev.Bytes > 0 {
		return ev.Bytes
	}
	return len(ev.Data)
}

// SanitizeRequestAuditMetadata accepts only known endpoint literals, token counter keys
// and the closed client response status/bytes key; arbitrary IDs and arbitrary
// status/bytes keys are omitted.
func SanitizeRequestAuditMetadata(in RequestAuditMetadata) RequestAuditMetadata {
	out := RequestAuditMetadata{}
	for _, key := range []string{"inbound", "upstream"} {
		if route := in.Routes[key]; requestAuditCanonicalRoute(route) {
			if out.Routes == nil {
				out.Routes = make(map[string]string)
			}
			out.Routes[key] = route
		}
	}
	for _, key := range []string{"session_fingerprint", "response_fingerprint", "local_request_fingerprint", "upstream_request_fingerprint"} {
		if digest := in.IDs[key]; len(digest) == 64 && isLowerHexDigest(digest) {
			if out.IDs == nil {
				out.IDs = make(map[string]string)
			}
			out.IDs[key] = digest
		}
	}
	for _, key := range []string{"input_tokens", "output_tokens"} {
		if count, ok := in.Tokens[key]; ok && count > 0 {
			if out.Tokens == nil {
				out.Tokens = make(map[string]int)
			}
			out.Tokens[key] = count
		}
	}
	// 返回客户端的响应阶段只允许一个封闭键。状态与字节各自独立校验：一个非法
	// 不会连带丢掉另一个已观测到的事实。
	if status, ok := in.Status[RequestAuditClientResponseKey]; ok && status >= 100 && status <= 599 {
		out.Status = map[string]int{RequestAuditClientResponseKey: status}
	}
	if size, ok := in.Bytes[RequestAuditClientResponseKey]; ok && size >= 0 {
		out.Bytes = map[string]int64{RequestAuditClientResponseKey: size}
	}
	if in.ProtocolFields != nil {
		out.ProtocolFields = SanitizeRequestAuditProtocolFields(*in.ProtocolFields)
	}
	return out
}

// AddRequestAuditUsageTokens adds only known, positive token counts from the linked usage record.
// UsageLog counters are non-pointer values, so zero cannot distinguish unknown from a reported zero.
func AddRequestAuditUsageTokens(metadata RequestAuditMetadata, usage *UsageLog) RequestAuditMetadata {
	// Token counts must come from the linked billing record, not caller metadata.
	metadata.Tokens = nil
	if usage == nil {
		return metadata
	}
	if usage.InputTokens > 0 {
		if metadata.Tokens == nil {
			metadata.Tokens = make(map[string]int)
		}
		metadata.Tokens["input_tokens"] = usage.InputTokens
	}
	if usage.OutputTokens > 0 {
		if metadata.Tokens == nil {
			metadata.Tokens = make(map[string]int)
		}
		metadata.Tokens["output_tokens"] = usage.OutputTokens
	}
	return metadata
}

// SanitizeRequestAuditProtocolFields accepts only closed protocol enum and field-name values.
func SanitizeRequestAuditProtocolFields(in RequestAuditProtocolFields) *RequestAuditProtocolFields {
	out := &RequestAuditProtocolFields{}
	if in.Stream != nil {
		stream := *in.Stream
		out.Stream = &stream
	}
	switch in.ThinkingType {
	case "disabled", "enabled", "adaptive":
		out.ThinkingType = in.ThinkingType
	}
	for _, field := range in.PresentFields {
		switch field {
		case "model", "messages", "input", "tools", "stream", "thinking":
			out.PresentFields = appendUniqueRequestAuditString(out.PresentFields, field)
		}
	}
	for _, field := range in.NormalizedFields {
		if field == "thinking.extra_fields_removed" {
			out.NormalizedFields = appendUniqueRequestAuditString(out.NormalizedFields, field)
		}
	}
	if out.Stream == nil && out.ThinkingType == "" && len(out.PresentFields) == 0 && len(out.NormalizedFields) == 0 {
		return nil
	}
	return out
}

func appendUniqueRequestAuditString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func requestAuditCanonicalRoute(route string) bool {
	switch route {
	case "/v1/messages", "/v1/chat/completions", "/v1/responses", "/v1/responses/compact", "/v1/responses/input_tokens":
		return true
	default:
		return false
	}
}

// SanitizeRequestAuditAttempt never persists a caller-supplied model name: routing permits arbitrary aliases.
func SanitizeRequestAuditAttempt(a RequestAuditAttempt) RequestAuditAttempt {
	out := RequestAuditAttempt{}
	if a.AccountID > 0 {
		out.AccountID = a.AccountID
	}
	switch a.Protocol {
	case RequestAuditProtocolAnthropic, RequestAuditProtocolOpenAIChat, RequestAuditProtocolOpenAIResp:
		out.Protocol = a.Protocol
	}
	switch a.Stage {
	case RequestAuditStageClientEntry, RequestAuditStagePostNormalize, RequestAuditStageWire:
		out.Stage = a.Stage
	}
	if len(a.ModelFingerprint) == 64 && isLowerHexDigest(a.ModelFingerprint) {
		out.ModelFingerprint = a.ModelFingerprint
	}
	if out.Stage != RequestAuditStageWire {
		return out
	}
	if len(a.WireRequestHeaders) > 0 {
		out.WireRequestHeaders = SanitizeRequestAuditHeaderMap(a.WireRequestHeaders)
		if len(out.WireRequestHeaders) == 0 {
			out.WireRequestHeaders = nil
		}
	}
	if len(a.UpstreamResponseHeaders) > 0 {
		out.UpstreamResponseHeaders = SanitizeRequestAuditResponseHeaderMap(a.UpstreamResponseHeaders)
		if len(out.UpstreamResponseHeaders) == 0 {
			out.UpstreamResponseHeaders = nil
		}
	}
	if a.UpstreamStatus != nil && *a.UpstreamStatus >= 100 && *a.UpstreamStatus <= 599 {
		status := *a.UpstreamStatus
		out.UpstreamStatus = &status
	}
	if a.RequestPayloadBytes != nil && *a.RequestPayloadBytes >= 0 {
		n := *a.RequestPayloadBytes
		out.RequestPayloadBytes = &n
	}
	if out.UpstreamStatus != nil && a.ResponsePayloadBytes != nil && *a.ResponsePayloadBytes >= 0 {
		n := *a.ResponsePayloadBytes
		out.ResponsePayloadBytes = &n
	}
	if out.UpstreamStatus == nil {
		out.UpstreamResponseHeaders = nil
	}
	if a.ResponseReadComplete != nil && out.ResponsePayloadBytes != nil {
		complete := *a.ResponseReadComplete
		out.ResponseReadComplete = &complete
	}
	return out
}

func sanitizeRequestAuditEventType(typ string) string {
	switch typ {
	case "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop", "ping", "error", "done", "truncated", "unknown",
		"response.created", "response.in_progress", "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled",
		"response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return typ
	default:
		return "unknown"
	}
}

func sanitizeRequestAuditCaptureReason(reason string) string {
	switch reason {
	case RequestAuditNotCapturedReasonPhase1Uncovered, RequestAuditPartialReasonCyberPolicy, "write_failed_after_response_started", "finalization_failed", "missing_reservation", "audit_write_failed", "reservation_missing_after_response_started":
		return reason
	default:
		return ""
	}
}

// SanitizeRequestAuditRecord is applied at build, persistence and disclosure boundaries.
// It creates independent maps and slices so callers cannot accidentally expose the original record.
func SanitizeRequestAuditRecord(rec *RequestAuditRecord) *RequestAuditRecord {
	if rec == nil {
		return nil
	}
	out := *rec
	out.FingerprintSalt = append([]byte(nil), rec.FingerprintSalt...)
	out.Headers = SanitizeRequestAuditHeaderMap(rec.Headers)
	out.Metadata = SanitizeRequestAuditMetadata(rec.Metadata)
	out.Attempts = buildRequestAuditAttempts(rec.Attempts)
	if len(rec.Events) > 0 {
		out.Events = make([]RequestAuditEventSkeleton, len(rec.Events))
		for i, event := range rec.Events {
			out.Events[i] = event
			out.Events[i].Type = sanitizeRequestAuditEventType(event.Type)
			switch event.Reason {
			case "max_events", "max_bytes":
			default:
				out.Events[i].Reason = ""
			}
			if out.Events[i].Bytes < 0 {
				out.Events[i].Bytes = 0
			}
			if len(out.Events[i].Fingerprint) != 64 || !isLowerHexDigest(out.Events[i].Fingerprint) {
				out.Events[i].Fingerprint = ""
			}
		}
	}
	switch rec.CaptureCompleteness {
	case RequestAuditCaptureComplete, RequestAuditCaptureIncomplete, RequestAuditCaptureTruncated, RequestAuditCaptureNotCaptured, RequestAuditCaptureWriteFailed:
	default:
		out.CaptureCompleteness = RequestAuditCaptureIncomplete
	}
	out.CaptureReason = sanitizeRequestAuditCaptureReason(rec.CaptureReason)
	if len(out.RequestFingerprint) != 64 || !isLowerHexDigest(out.RequestFingerprint) || out.FingerprintKeyVersion <= 0 || out.FingerprintKeyVersion > 9999 {
		out.RequestFingerprint = ""
		out.FingerprintKeyVersion = 0
		out.FingerprintSalt = nil
	}
	// A nullable salt represents historical fingerprints that cannot be verified post hoc.
	return &out
}

func isLowerHexDigest(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' && s[i] < 'a' || s[i] > 'f' {
			return false
		}
	}
	return true
}

func buildRequestAuditAttempts(attempts []RequestAuditAttempt) []RequestAuditAttempt {
	if len(attempts) == 0 {
		return nil
	}
	out := make([]RequestAuditAttempt, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, SanitizeRequestAuditAttempt(a))
	}
	return out
}

func buildRequestAuditEventSkeletons(events []RequestAuditSSEEvent) []RequestAuditEventSkeleton {
	return buildRequestAuditEventSkeletonsWithFingerprint(events, nil)
}

func buildRequestAuditEventSkeletonsWithFingerprint(events []RequestAuditSSEEvent, fingerprint *RequestAuditFingerprintInput) []RequestAuditEventSkeleton {
	if len(events) == 0 {
		return nil
	}
	if summary := events[len(events)-1].auditSummary; summary != nil {
		retained := events[:len(events)-1]
		out := buildRequestAuditEventSkeletonsWithFingerprint(retained, fingerprint)
		if len(out) > 0 && out[len(out)-1].Truncated {
			out = out[:len(out)-1]
		}
		for len(out) > 0 && len(out) > requestAuditMaxSSEEvents-1 {
			out = out[:len(out)-1]
		}
		for {
			keptBytes := 0
			for _, event := range out {
				keptBytes += event.Bytes
			}
			marker := RequestAuditEventSkeleton{
				Type: "truncated", Index: len(out), Truncated: true,
				Original: summary.Original, OriginalBytes: summary.OriginalBytes,
				Kept: len(out), KeptBytes: keptBytes,
				Dropped: summary.Original - len(out), DroppedBytes: summary.OriginalBytes - keptBytes,
				Reason: summary.Reason,
			}
			candidate := append(append([]RequestAuditEventSkeleton(nil), out...), marker)
			encoded, err := json.Marshal(candidate)
			if err == nil && len(encoded) <= requestAuditMaxSSEBytes || len(out) == 0 {
				return candidate
			}
			out = out[:len(out)-1]
		}
	}
	originalBytes := 0
	for _, ev := range events {
		originalBytes += requestAuditSSEEventBytes(ev)
	}
	out := make([]RequestAuditEventSkeleton, 0, min(len(events), requestAuditMaxSSEEvents+1))
	keptBytes := 0
	keptJSONSize := 2 // []
	appendTruncated := func(reason string) {
		for {
			kept := len(out)
			marker := RequestAuditEventSkeleton{
				Type:          "truncated",
				Index:         kept,
				Truncated:     true,
				Original:      len(events),
				OriginalBytes: originalBytes,
				Kept:          kept,
				KeptBytes:     keptBytes,
				Dropped:       len(events) - kept,
				DroppedBytes:  originalBytes - keptBytes,
				Reason:        reason,
			}
			candidate := append(append([]RequestAuditEventSkeleton(nil), out...), marker)
			encoded, err := json.Marshal(candidate)
			if err == nil && len(encoded) <= requestAuditMaxSSEBytes {
				out = append(out, marker)
				return
			}
			if len(out) == 0 {
				out = []RequestAuditEventSkeleton{marker}
				return
			}
			last := out[len(out)-1]
			out = out[:len(out)-1]
			keptBytes -= last.Bytes
		}
	}
	for i, ev := range events {
		n := requestAuditSSEEventBytes(ev)
		sk := RequestAuditEventSkeleton{Type: sanitizeRequestAuditEventType(ev.Type), Index: i, Bytes: n}
		if fingerprint != nil {
			sk.Fingerprint = fingerprint.Events[i]
		}
		encoded, err := json.Marshal(sk)
		if err != nil {
			encoded = []byte{}
		}
		nextJSONSize := keptJSONSize
		if len(out) > 0 {
			nextJSONSize++
		}
		nextJSONSize += len(encoded)
		exceedsEvents := len(out) >= requestAuditMaxSSEEvents
		exceedsBytes := nextJSONSize > requestAuditMaxSSEBytes
		if exceedsEvents || exceedsBytes {
			reason := "max_events"
			if !exceedsEvents {
				reason = "max_bytes"
			}
			appendTruncated(reason)
			return out
		}
		out = append(out, sk)
		keptBytes += n
		keptJSONSize = nextJSONSize
	}
	return out
}

// AttachRequestAuditAfterUsageLog 在已有使用记录后尽力写入审计。没有使用记录则不写。
// 普通审计模式 fail-open：写入失败仅在使用记录已存在且主库恢复可写时尽力补记失败状态。
func AttachRequestAuditAfterUsageLog(ctx context.Context, repo RequestAuditRepository, usageLog *UsageLog, in RequestAuditInput) error {
	if repo == nil || usageLog == nil || usageLog.ID <= 0 {
		return nil
	}
	if !shouldAttachRequestAudit(in) {
		return nil
	}
	in.UsageLogID = usageLog.ID
	in.Metadata = AddRequestAuditUsageTokens(in.Metadata, usageLog)
	rec := BuildRequestAuditRecord(in)
	if rec == nil {
		return nil
	}
	if err := repo.CreateRequestAudit(ctx, rec); err != nil {
		requestAuditOrdinaryWriteFailures.Add(1)
		logger.LegacyPrintf("service.request_audit", "Request audit write failed: usage_log_id=%d code=audit_write_failed completeness=%s events=%d", rec.UsageLogID, rec.CaptureCompleteness, len(rec.Events))
		marker := &RequestAuditRecord{
			UsageLogID: usageLog.ID, Headers: map[string]any{},
			CaptureCompleteness: RequestAuditCaptureWriteFailed,
			CaptureReason:       "audit_write_failed",
		}
		markerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if markerErr := repo.CreateRequestAudit(markerCtx, marker); markerErr != nil {
			logger.LegacyPrintf("service.request_audit", "Request audit failure marker unavailable: usage_log_id=%d code=audit_write_failed", usageLog.ID)
		}
		return nil
	}
	if rec.CaptureCompleteness == RequestAuditCaptureTruncated || rec.CaptureCompleteness == RequestAuditCaptureIncomplete {
		logger.LegacyPrintf("service.request_audit", "Request audit captured: usage_log_id=%d completeness=%s events=%d", rec.UsageLogID, rec.CaptureCompleteness, len(rec.Events))
	}
	return nil
}
