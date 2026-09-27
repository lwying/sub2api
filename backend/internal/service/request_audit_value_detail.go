package service

// Usage-owned request-audit value detail sidecar. Long-lived request_audits
// contains only protocol metadata and event skeletons, not values or bodies.
// New allowlisted header values, identifiers, and bounded caller-controlled model
// aliases are retained as plaintext only while their usage log exists. Legacy
// encrypted rows keep their original seven-day deadline and decryption key.
// Default-off capture requires a current written acknowledgement. Each captured
// attempt uses its actual wire protocol and closed header-value contract; unknown
// protocols or mixed-wire attempts are marked unsupported rather than guessed.
//
// 本文件的名字是**持久化边界**的二次收窄，不是净化器：净化器（internal/pkg/httpattempt 的
// SanitizeClaude*HeaderValues）负责按语义判定「哪个头值得看」并规范化取值；本层负责
// 「即使净化器给了什么，也只肯落库闭集内的名字与有界取值」。收窄的做法是把候选值**交回
// 同一套净化器**再取一次结果，因此：
//   - 不在闭集里的头名（例如客户端自带的未知头）由净化器丢弃，**不会**让整份快照失败；
//   - 凭据类头（Cookie／Authorization／X-Api-Key／Proxy-Authorization 等）只会得到存在性
//     标记，而存在性不是值，本层直接丢弃，绝不落库；
//   - 多值头没有唯一取值可存，取第一行是一次静默截断，因此整份判不合格（fail closed），
//     与净化器约定一致——半个快照会被管理员读成「客户端只发了这些」，比没有记录更危险。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	// 存储状态（state）：写入只产生 not_observed／stored／skipped；
	// expired 与 purged 由读取派生与清理产生，使「已到期」与「已物理清除」
	// 都不会被误报成「仍可读」。
	RequestAuditValueDetailStateNotObserved = "not_observed"
	RequestAuditValueDetailStateStored      = "stored"
	RequestAuditValueDetailStateSkipped     = "skipped"
	RequestAuditValueDetailStateExpired     = "expired"
	RequestAuditValueDetailStatePurged      = "purged"

	// 原因码（reason）：稳定枚举，五种「没有留存」的事实必须分开。
	RequestAuditValueDetailNotObserved                  = "not_observed"
	RequestAuditValueDetailRetained                     = "retained"
	RequestAuditValueDetailSkippedOutOfScope            = "skipped_out_of_scope"
	RequestAuditValueDetailSkippedRetentionDisabled     = "skipped_value_retention_disabled"
	RequestAuditValueDetailSkippedEncryptionUnavailable = "skipped_encryption_unavailable"
	RequestAuditValueDetailSkippedInvalidValues         = "skipped_invalid_values"
	RequestAuditValueDetailSkippedTooManyAttempts       = "skipped_too_many_attempts"
	RequestAuditValueDetailSkippedUnsupportedProtocol   = "skipped_unsupported_protocol"
	RequestAuditValueDetailStorageEncryptedV1           = "encrypted_v1"
	RequestAuditValueDetailStoragePlaintextUsageBound   = "plaintext_usage_bound"

	// 本能力覆盖的入站路由闭集。入站路由同时也是**入站 wire 协议**的来源
	// （见 RequestAuditValueDetailInboundProtocolForRoute）。
	RequestAuditValueDetailRouteMessages         = "/v1/messages"
	RequestAuditValueDetailRouteChatCompletions  = "/v1/chat/completions"
	RequestAuditValueDetailRouteResponses        = "/v1/responses"
	RequestAuditValueDetailRouteResponsesCompact = "/v1/responses/compact"

	// RequestAuditValueDetailRetention 是加密值明细在在线主库的保留期（7 天）。
	RequestAuditValueDetailRetention = 7 * 24 * time.Hour

	// 有界校验：尝试数、条目数、单值与整体体量都有上限。
	RequestAuditValueDetailMaxAttempts = 8
	// RequestAuditValueDetailMaxStoredAttemptCount 是 attempt_count 列的**存储上限**，与迁移 253
	// 的 request_audit_value_details_attempt_count_range 检查约束（attempt_count <= 16）逐值一致
	// （由 TestRequestAuditValueDetailStorageBoundMatchesMigrationCheck 钉住）。
	//
	// attempt_count 因此是**有界观察值**：边界内它是精确的实际尝试数（9..16 次也如实记下，
	// 不会被冒充成采集上限 8）；超过边界时它饱和在边界上，「尝试数超过采集上限」这一事实
	// 由 reason=skipped_too_many_attempts 表达。饱和不是可选的美化：越界的计数会让整条
	// skipped_too_many_attempts 事实被数据库检查约束拒收，管理员那里看到的是「没有行」。
	RequestAuditValueDetailMaxStoredAttemptCount = 16
	// RequestAuditValueDetailMaxEntryCount 是单行允许留存的值条目总数。
	RequestAuditValueDetailMaxEntryCount = 256
	// RequestAuditValueDetailMaxHeaderEntries 是单个方向允许留存的头值条目上限。
	RequestAuditValueDetailMaxHeaderEntries = 40
	// RequestAuditValueDetailMaxHeaderValueBytes 是单条头值的长度上限。
	RequestAuditValueDetailMaxHeaderValueBytes = 256
	// RequestAuditValueDetailMaxHeaderBytesPerDirection 是单个方向头值载荷的字节上限。
	RequestAuditValueDetailMaxHeaderBytesPerDirection = 4096
	// RequestAuditValueDetailMaxIdentifierBytes 是解析出的标识值长度上限。
	RequestAuditValueDetailMaxIdentifierBytes = 128
	// RequestAuditValueDetailMaxModelBytes 是模型名的长度上限。
	RequestAuditValueDetailMaxModelBytes = 200
	// RequestAuditValueDetailMaxLatencyMillis 是单次尝试耗时的上限（24 小时）：
	// 超过它的值不是测量结果，按「未测量」处理。
	RequestAuditValueDetailMaxLatencyMillis = int64(24 * 60 * 60 * 1000)
	// RequestAuditValueDetailMaxPayloadBytes 是加密前 JSON 载荷的字节上限。
	RequestAuditValueDetailMaxPayloadBytes = 32 * 1024
	// RequestAuditValueDetailMaxReadBytes 是解密后允许返回的载荷上限。
	RequestAuditValueDetailMaxReadBytes = RequestAuditValueDetailMaxPayloadBytes

	// requestAuditValueDetailWriteTimeout 是值明细写入的独立超时：
	// 它永远不拖慢已经交付给客户端的请求。
	requestAuditValueDetailWriteTimeout = 5 * time.Second
)

// 值明细读取侧的稳定错误。四种「读不到值」必须分开：
//   - NotFound：没有这条使用记录的值明细行（未采集／未开启）；
//   - NotRetained：有行，但从未留存过值（被跳过或本来就没有可留存的值）；
//   - Gone：曾经留存过，但已到期或已被物理清除（不再可揭示）；
//   - Unavailable：存储或密钥此刻不可用（可重试）。
var (
	ErrRequestAuditValueDetailNotFound    = errors.New("request audit value detail not found")
	ErrRequestAuditValueDetailNotRetained = errors.New("request audit value detail values were not retained")
	ErrRequestAuditValueDetailGone        = errors.New("request audit value detail values are gone")
	ErrRequestAuditValueDetailUnavailable = errors.New("request audit value detail unavailable")
)

// RequestAuditValueDetailAttempt 是一次真实上游尝试的采集输入。
//
// RequestHeaderValues / ResponseHeaderValues 是净化器输出（map[string]any），
// MetadataUserID 是该次尝试实际发往上游的 metadata.user_id 原始字符串。
// HeaderOmission 是同一时刻由传输层记下的**省略摘要**（只含计数）。
type RequestAuditValueDetailAttempt struct {
	Index                int
	AccountID            int64
	Protocol             string
	Model                string
	MetadataUserID       string
	UpstreamStatus       *int
	LatencyMillis        *int64
	ProxyID              int64
	RequestHeaderValues  map[string]any
	ResponseHeaderValues map[string]any
	// HeaderOmission 是本次尝试头值快照的省略摘要（请求与响应合并，只含计数）：
	// 传输层观察到、但没能进入快照的头名／取值个数。它不含名字与取值。
	HeaderOmission httpattempt.ClaudeHeaderValueOmission
}

// RequestAuditValueDetailInput 是一次逻辑请求的值明细采集输入。
//
// Route 是入站路由；Protocol 是这次逻辑请求实际发出的上游协议形态。
// 两者共同决定是否在范围内（见 RequestAuditValueDetailScopeApplies）。
//
// 三个协议事实各归其位：
//   - Route → 入站 wire 协议（RequestAuditValueDetailInboundProtocolForRoute），
//     用于解释 InboundHeaderValues 与入站正文里的标识；
//   - Protocol 与每次 Attempts[i].Protocol → 真实上游 wire 协议，只用于尝试级取值
//     与范围判定，**不得**用来解释入站头值或入站正文；
//   - 入站与出站在转换分支里可以不同（Chat Completions→Anthropic、Messages→OpenAI）。
type RequestAuditValueDetailInput struct {
	Route               string
	Protocol            string
	InboundHeaderValues map[string]any
	// InboundHeaderOmission 是入站头值快照的省略摘要（只含计数）。入站的未知头名与
	// 没通过校验的取值在净化之后就不存在了，因此这个事实必须由采集侧带进来
	// （见 RequestAuditValueDetailInboundHeaders）。
	InboundHeaderOmission httpattempt.ClaudeHeaderValueOmission
	MetadataUserID        string
	Model                 string
	ClientStatus          int
	StartedAt             time.Time
	CompletedAt           time.Time
	Attempts              []RequestAuditValueDetailAttempt
}

// RequestAuditValueDetailFields 是**明文信封**里的标量字段。
//
// The default GET envelope contains only bounded server facts. Caller-controlled
// model aliases remain in the explicit reveal payload, not this response.
type RequestAuditValueDetailFields struct {
	Route        string
	Protocol     string
	ClientStatus int
	StartedAt    time.Time
	CompletedAt  time.Time
}

// RequestAuditValueDetailInboundValues 是入站阶段校验后的值。
//
// 头值是**有界的值列表**：单值头是单元素列表，多值头按原顺序保留（上限由
// httpattempt 的净化器给出，最多 4 个）。取第一行或截断到单个值会让管理员把
// 「客户端只发了这一行」当成事实，因此多值在这里必须原样保留。
type RequestAuditValueDetailInboundValues struct {
	RequestHeaders map[string][]string `json:"request_headers,omitempty"`
	DeviceID       string              `json:"device_id,omitempty"`
	AccountUUID    string              `json:"account_uuid,omitempty"`
	SessionID      string              `json:"session_id,omitempty"`
}

// RequestAuditValueDetailAttemptValues 是单次上游尝试校验后的值。
//
// LatencyMillis and ProxyID are bounded attempt facts. They remain in the
// reveal payload, not in the default envelope, for both legacy and new rows.
type RequestAuditValueDetailAttemptValues struct {
	Index           int                 `json:"index"`
	AccountID       int64               `json:"account_id,omitempty"`
	Protocol        string              `json:"protocol,omitempty"`
	Model           string              `json:"model,omitempty"`
	UpstreamStatus  *int                `json:"upstream_status,omitempty"`
	LatencyMillis   *int64              `json:"latency_ms,omitempty"`
	ProxyID         int64               `json:"proxy_id,omitempty"`
	RequestHeaders  map[string][]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	DeviceID        string              `json:"device_id,omitempty"`
	AccountUUID     string              `json:"account_uuid,omitempty"`
	SessionID       string              `json:"session_id,omitempty"`
}

// RequestAuditValueDetailValues is the bounded, allowlisted reveal payload.
// Model is the caller-selected alias where available; the real wire model is
// recorded per attempt. No free-text model is present in the default envelope.
//
// Truncated 是**采集侧**的事实：表示「调用方提供的值里有条目没被收下」（不在闭集里的
// 头名，或取值没通过净化器）。它不是失败，也不能藏起来：没有这个标记，管理员无法区分
// 「客户端只发了这些」与「我们只收了这些」。
//
// 这个事实有两个来源，且**两者都必须写进载荷**：
//   - 净化器在采集那一刻丢掉的头名／取值（由 httpattempt.ClaudeHeaderValueOmission 计数带入，
//     因为它在服务层已经看不见了）；
//   - 采集侧白名单复核时丢掉的条目（见 NormalizeRequestAuditValueDetailValues）。
//
// 它只表达采集侧。**读侧复核丢掉的条目是另一条事实**（ADR 0006：读侧自己的丢弃必须显式
// 告警，且与载荷自带的 truncated 相互独立）：读侧不得把读侧的丢弃并进这个字段——否则
// 管理员会把「读侧没收下」读成「采集时客户端就没发」，而这正是本条要防的误读。读侧的
// 事实由 ValidationDropped 承载，见 DecodeRequestAuditValueDetailValuesForProtocol。
type RequestAuditValueDetailValues struct {
	Model     string                                 `json:"model,omitempty"`
	Inbound   RequestAuditValueDetailInboundValues   `json:"inbound"`
	Attempts  []RequestAuditValueDetailAttemptValues `json:"attempts,omitempty"`
	Truncated bool                                   `json:"truncated,omitempty"`
	// ValidationDropped 是**读侧**的事实：读侧复核拒收了载荷里的条目。
	//
	// 它刻意是**传输专用**的，不进存储格式：
	//   - `json:"-"` 让它既不被 EncodeRequestAuditValueDetailValues 写进存储载荷，
	//     也不能由载荷设置（该键对解码器是未知字段，DisallowUnknownFields 会拒绝），
	//     因此「载荷说了算」的永远只有 Truncated 一个；
	//   - 读取路径把它带到揭示 DTO（RequestAuditValueDetailReveal.ValidationDropped），
	//     使管理员能同时看到「采集时没收下」（Truncated）与「读侧没收下」两条独立事实，
	//     而不是把后者冒充成前者，也不是静默丢掉。
	ValidationDropped bool `json:"-"`
}

// requestAuditValueDetailBoundedAttemptCount 把观察到的尝试数收敛到存储上限
// （见 RequestAuditValueDetailMaxStoredAttemptCount）。
func requestAuditValueDetailBoundedAttemptCount(count int) int {
	if count <= 0 {
		return 0
	}
	if count > RequestAuditValueDetailMaxStoredAttemptCount {
		return RequestAuditValueDetailMaxStoredAttemptCount
	}
	return count
}

// EntryCount 返回留存**事实**条目总数（多值头按值个数计，模型、标识、每次尝试的
// 耗时与代理内部 ID 各计一条），用于有界校验与对外披露。
func (v RequestAuditValueDetailValues) EntryCount() int {
	count := 0
	if v.Model != "" {
		count++
	}
	count += requestAuditValueDetailHeaderValueCount(v.Inbound.RequestHeaders)
	for _, id := range []string{v.Inbound.DeviceID, v.Inbound.AccountUUID, v.Inbound.SessionID} {
		if id != "" {
			count++
		}
	}
	for _, attempt := range v.Attempts {
		count += requestAuditValueDetailHeaderValueCount(attempt.RequestHeaders)
		count += requestAuditValueDetailHeaderValueCount(attempt.ResponseHeaders)
		if attempt.Model != "" {
			count++
		}
		if attempt.LatencyMillis != nil {
			count++
		}
		if attempt.ProxyID > 0 {
			count++
		}
		for _, id := range []string{attempt.DeviceID, attempt.AccountUUID, attempt.SessionID} {
			if id != "" {
				count++
			}
		}
	}
	return count
}

func requestAuditValueDetailHeaderValueCount(headers map[string][]string) int {
	count := 0
	for _, values := range headers {
		count += len(values)
	}
	return count
}

// Empty 报告整份值视图一个条目都没有。
func (v RequestAuditValueDetailValues) Empty() bool {
	return v.EntryCount() == 0
}

// RequestAuditValueDetailWrite contains the format-bound payload and metadata.
// New writes use plaintext_usage_bound; existing encrypted_v1 rows retain their
// original seven-day window and require the old cipher for disclosure.
type RequestAuditValueDetailWrite struct {
	UsageLogID    int64
	State         string
	Reason        string
	StorageFormat string
	Fields        RequestAuditValueDetailFields
	Payload       []byte
	AttemptCount  int
	EntryCount    int
	ExpiresAt     time.Time
}

// Retained reports whether the write contains values in its selected format.
func (w RequestAuditValueDetailWrite) Retained() bool {
	return w.State == RequestAuditValueDetailStateStored && len(w.Payload) > 0
}

// RequestAuditValueDetail 是值明细旁路的存储信封，不含明文值。
type RequestAuditValueDetail struct {
	UsageLogID    int64
	State         string
	Reason        string
	StorageFormat string
	Fields        RequestAuditValueDetailFields
	Stored        bool
	KeyVersion    int
	AttemptCount  int
	EntryCount    int
	PayloadBytes  int
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

// Expired applies only to legacy ciphertext. New plaintext follows usage deletion.
func (d RequestAuditValueDetail) Expired(now time.Time) bool {
	return d.StorageFormat != RequestAuditValueDetailStoragePlaintextUsageBound && !d.ExpiresAt.IsZero() && !d.ExpiresAt.After(now)
}

// Readable requires an existing stored payload; only legacy rows can expire independently.
func (d RequestAuditValueDetail) Readable(now time.Time) bool {
	return d.Stored && !d.Expired(now)
}

// RequestAuditValueDetailEnvelope 是**默认（GET）**披露的白名单。
//
// 它刻意不含任何值：默认视图只能说明「有没有留、为什么没留、还能看多久」，
// 真实值必须由管理员显式 POST 揭示（见 handler 层）。类型里没有值字段，
// 因此「默认 GET 不返回值」是类型保证，而不是调用约定。
type RequestAuditValueDetailEnvelope struct {
	UsageLogID        int64      `json:"usage_log_id"`
	StorageFormat     string     `json:"storage_format,omitempty"`
	State             string     `json:"state"`
	Reason            string     `json:"reason"`
	Route             string     `json:"route,omitempty"`
	Protocol          string     `json:"protocol,omitempty"`
	ClientStatus      int        `json:"client_status,omitempty"`
	AttemptCount      int        `json:"attempt_count"`
	EntryCount        int        `json:"entry_count"`
	PayloadBytes      int        `json:"payload_bytes"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	CapabilityEnabled bool       `json:"capability_enabled"`
}

// RequestAuditValueDetailRepository 写入、读取并清理值明细旁路。
//
// CreateRequestAuditValueDetail 必须只在同一 usage_log_id 的 request_audits 行已存在时
// 才真正写入（同一条语句内的 EXISTS 守卫），因此「先有审计行、再有值明细」是存储层
// 不变量，而不是调用方的约定。
type RequestAuditValueDetailRepository interface {
	CreateRequestAuditValueDetail(ctx context.Context, write RequestAuditValueDetailWrite) (RequestAuditValueDetail, error)
	GetRequestAuditValueDetail(ctx context.Context, usageLogID int64) (RequestAuditValueDetail, error)
	ReadRequestAuditValueDetailValues(ctx context.Context, usageLogID int64, now time.Time) (RequestAuditValueDetailValues, error)
	ClearExpiredRequestAuditValueDetails(ctx context.Context, now time.Time, limit int) (int64, error)
}

// RequestAuditValueDetailGate 是采集门控结论。
//
// CaptureAllowed requires the stored switch and current written risk confirmation.
// EncryptionAvailable reports only whether legacy ciphertext can be decrypted;
// new plaintext capture needs no key.
type RequestAuditValueDetailGate struct {
	CaptureAllowed      bool
	EncryptionAvailable bool
}

// RequestAuditValueDetailGateReader 报告采集门控结论；由 *SettingService 实现。
//
// 门控结论必须在服务层强制的理由：设置键可以被其它管理路径或直接改库写成开启，
// 因此「存量布尔值为真」不足以证明操作员做过书面确认——采集侧只认这里的结论。
type RequestAuditValueDetailGateReader interface {
	RequestAuditValueDetailGate(ctx context.Context) RequestAuditValueDetailGate
}

// RequestAuditValueDetailScopeApplies 报告这次逻辑请求是否在本能力范围内。
//
// Only actually audited Messages, Chat Completions and Responses HTTP routes
// with a supported real wire protocol qualify. Unsupported signed or mixed
// protocols are reported without copying values; uncovered routes write no row.
func RequestAuditValueDetailScopeApplies(route, protocol string) bool {
	return requestAuditValueDetailRouteSupported(route) && requestAuditValueDetailProtocolSupported(protocol)
}

func requestAuditValueDetailRouteSupported(route string) bool {
	_, ok := RequestAuditValueDetailInboundProtocolForRoute(route)
	return ok
}

// RequestAuditValueDetailInboundProtocolForRoute 报告某个入站路由承载的**入站 wire 协议**。
//
// 入站头值与入站正文里的事实（头值闭集、metadata.user_id 形态）由**客户端实际使用的入站协议**
// 决定，绝不能拿「这次逻辑请求最终发往上游的形态」来解释：Chat Completions 入站 → Anthropic
// 出站、Messages 入站 → OpenAI 出站这类转换里，入站 wire 与出站 wire 是两个不同的闭集，把出站
// 白名单套到入站 wire 上会把客户端的合法头值判成「闭集外的名字」而整份丢弃（ADR 0007、
// 规格「逐协议白名单：入站允许头」）。
//
// 与之相对的是**真实上游协议**：它只能来自传输层逐次观察到的尝试，不得由入站路由字符串推断
// （规格已明确点名 count_tokens 与 Antigravity 兼容路径会被路径规范化误映射）。
func RequestAuditValueDetailInboundProtocolForRoute(route string) (string, bool) {
	switch strings.TrimSpace(route) {
	case RequestAuditValueDetailRouteMessages:
		return RequestAuditProtocolAnthropic, true
	case RequestAuditValueDetailRouteChatCompletions:
		return RequestAuditProtocolOpenAIChat, true
	case RequestAuditValueDetailRouteResponses, RequestAuditValueDetailRouteResponsesCompact:
		return RequestAuditProtocolOpenAIResp, true
	default:
		return "", false
	}
}

func requestAuditValueDetailProtocolSupported(protocol string) bool {
	switch protocol {
	case RequestAuditProtocolAnthropic, RequestAuditProtocolOpenAIChat, RequestAuditProtocolOpenAIResp:
		return true
	default:
		return false
	}
}

// BuildRequestAuditValueDetailWrite validates the scoped, protocol-specific
// plaintext payload. An unsupported wire protocol, closed gate, or invalid
// snapshot affects only this sidecar; it never blocks a model request.
// 返回 nil 表示「未采集」：入站路由不在范围内时一个字节都不写（由「无行」表达未采集）。
func BuildRequestAuditValueDetailWrite(in RequestAuditValueDetailInput, gate RequestAuditValueDetailGate, now time.Time) *RequestAuditValueDetailWrite {
	route := strings.TrimSpace(in.Route)
	if !requestAuditValueDetailRouteSupported(route) {
		// 路由不在范围内：这是「未采集」，不是失败。返回 nil 表示不落任何行,
		// 避免为其它协议的流量写空壳记录。
		return nil
	}

	// 入站头值按**入站路由**对应的协议校验；in.Protocol 是这次逻辑请求真实发出的上游形态，
	// 只用于尝试级取值与范围判定。两者在转换分支（Chat Completions→Anthropic、
	// Messages→OpenAI）里不是同一个闭集，不能互相解释。
	inboundProtocol, inboundSupported := RequestAuditValueDetailInboundProtocolForRoute(route)
	if !inboundSupported {
		// 与上面的路由闭集同源，因此这里不可达；显式 fail closed 是为了将来两者漂移时
		// 不会悄悄退化成「用出站协议猜入站 wire」。
		return &RequestAuditValueDetailWrite{
			State:         RequestAuditValueDetailStateSkipped,
			Reason:        RequestAuditValueDetailSkippedUnsupportedProtocol,
			StorageFormat: RequestAuditValueDetailStoragePlaintextUsageBound,
			Fields:        requestAuditValueDetailFields(in, route),
		}
	}

	write := &RequestAuditValueDetailWrite{
		State:         RequestAuditValueDetailStateSkipped,
		Reason:        RequestAuditValueDetailSkippedOutOfScope,
		StorageFormat: RequestAuditValueDetailStoragePlaintextUsageBound,
		Fields:        requestAuditValueDetailFields(in, route),
	}

	if !RequestAuditValueDetailScopeApplies(route, strings.TrimSpace(in.Protocol)) {
		write.Reason = RequestAuditValueDetailSkippedUnsupportedProtocol
		return write
	}
	if !gate.CaptureAllowed {
		write.Reason = RequestAuditValueDetailSkippedRetentionDisabled
		return write
	}

	for _, attempt := range in.Attempts {
		protocol := strings.TrimSpace(attempt.Protocol)
		if protocol != "" && (!requestAuditValueDetailProtocolSupported(protocol) || protocol != in.Protocol) {
			// An unsupported or mixed real wire attempt must not be reported as
			// malformed values or decoded under the final account's protocol.
			write.Reason = RequestAuditValueDetailSkippedUnsupportedProtocol
			return write
		}
	}

	if len(in.Attempts) > RequestAuditValueDetailMaxAttempts {
		// 计数必须收敛到存储上限：越界的 attempt_count 会让这一行被检查约束拒收，
		// 「尝试过多」这个事实就只剩日志，管理员看到的是「没有行」。
		write.AttemptCount = requestAuditValueDetailBoundedAttemptCount(len(in.Attempts))
		write.Reason = RequestAuditValueDetailSkippedTooManyAttempts
		return write
	}

	values, ok := NormalizeRequestAuditValueDetailValuesForProtocols(requestAuditValueDetailValuesFromInput(in), inboundProtocol, strings.TrimSpace(in.Protocol))
	if !ok {
		write.Reason = RequestAuditValueDetailSkippedInvalidValues
		return write
	}
	entryCount := values.EntryCount()
	write.AttemptCount = requestAuditValueDetailBoundedAttemptCount(len(in.Attempts))
	if entryCount > RequestAuditValueDetailMaxEntryCount {
		write.Reason = RequestAuditValueDetailSkippedInvalidValues
		return write
	}
	if entryCount == 0 {
		if values.Truncated {
			write.Reason = RequestAuditValueDetailSkippedInvalidValues
			return write
		}
		// 完全没有可留存事实且采集侧没有省略时，是未观察到值；
		// 若存在省略，已在上方作为拒留处理，不能假报为「未采集」。
		write.State = RequestAuditValueDetailStateNotObserved
		write.Reason = RequestAuditValueDetailNotObserved
		return write
	}
	payload, err := EncodeRequestAuditValueDetailValues(values)
	if err != nil || len(payload) == 0 {
		write.Reason = RequestAuditValueDetailSkippedInvalidValues
		return write
	}
	write.State = RequestAuditValueDetailStateStored
	write.Reason = RequestAuditValueDetailRetained
	write.Payload = payload
	write.EntryCount = entryCount
	return write
}

// DescribeRequestAuditValueDetailState 把原因码与到期映射成对外的 state。
//
// 已到期与已物理清除都不得被误报为仍可读；「曾留存、现已清除」与「从未留存」
// 必须是两个不同的结论。
func DescribeRequestAuditValueDetailState(detail RequestAuditValueDetail, now time.Time) string {
	if detail.Stored {
		if detail.Expired(now) {
			return RequestAuditValueDetailStateExpired
		}
		return RequestAuditValueDetailStateStored
	}
	switch detail.Reason {
	case RequestAuditValueDetailRetained:
		return RequestAuditValueDetailStatePurged
	case RequestAuditValueDetailNotObserved, "":
		return RequestAuditValueDetailStateNotObserved
	default:
		return RequestAuditValueDetailStateSkipped
	}
}

// NormalizeRequestAuditValueDetailValues 对整份值视图做持久化边界校验。
//
// 返回可在库外安全传递的副本（含「是否有条目没被收下」的标记）；ok=false 表示整份不合格
// （出现无法唯一表示的形状、取值超界、含控制字符，或含凭据形态）。
// 不合格时**不返回任何部分结果**。不在闭集里的头名由净化器丢弃，只置 Truncated，
// 不算整份不合格；采集侧带进来的省略摘要（values.Truncated）同样原样保留，
// 因此本层不会把「客户端发了我们没收的东西」这个事实抹掉。
func NormalizeRequestAuditValueDetailValues(values RequestAuditValueDetailValues) (RequestAuditValueDetailValues, bool) {
	return NormalizeRequestAuditValueDetailValuesForProtocol(values, RequestAuditProtocolAnthropic)
}

// NormalizeRequestAuditValueDetailValuesForProtocol 是**采集／持久化边界**的收窄：
// 它把本层在这次收窄中丢掉的条目并入载荷自带的 truncated（两者都是采集侧的事实，
// 见 RequestAuditValueDetailValues.Truncated）。
//
// 这个合并只对采集侧成立。读侧不得复用合并后的结果：读侧的丢弃是另一条事实，
// 必须与载荷自带的 truncated 分开表达（见 DecodeRequestAuditValueDetailValuesForProtocol）。
func NormalizeRequestAuditValueDetailValuesForProtocol(values RequestAuditValueDetailValues, protocol string) (RequestAuditValueDetailValues, bool) {
	return NormalizeRequestAuditValueDetailValuesForProtocols(values, protocol, protocol)
}

// NormalizeRequestAuditValueDetailValuesForProtocols 是本边界**双协议**形式：入站头值按
// inboundProtocol 复核，逐次尝试的请求/响应头值按 wireProtocol 复核。
//
// 两者必须分开的理由：Chat Completions 入站 → Anthropic 出站、Messages 入站 → OpenAI 出站
// 时，入站 wire 与出站 wire 的闭集不同；用一个协议同时解释两边，必然把其中一边的合法取值
// 判成「闭集外的名字」。inboundProtocol 由入站路由决定（RequestAuditValueDetailInboundProtocolForRoute），
// wireProtocol 只来自逐次真实尝试。
func NormalizeRequestAuditValueDetailValuesForProtocols(values RequestAuditValueDetailValues, inboundProtocol, wireProtocol string) (RequestAuditValueDetailValues, bool) {
	normalized, dropped, ok := normalizeRequestAuditValueDetailValuesForProtocols(values, inboundProtocol, wireProtocol)
	if !ok {
		return RequestAuditValueDetailValues{}, false
	}
	if dropped {
		normalized.Truncated = true
	}
	return normalized, true
}

// normalizeRequestAuditValueDetailValuesForProtocol 是单协议形式（入站与出站同形时使用）。
// normalizeRequestAuditValueDetailValuesForProtocols 是白名单与有界复核的唯一实现。
//
// 第二个返回值是**本层这次复核丢掉的条目**（闭集外的头名、没通过净化器的取值）。
// 它刻意不并进 Truncated：采集侧据此把丢弃记进载荷（那是采集时的事实），读侧则必须
// 把它当作另一条事实处理——读侧调用方（Decode）因此拿得到这个布尔值，而不是只看一个
// 已被合并的 Truncated。ok=false 表示整份不合格，此时不返回任何部分结果。
func normalizeRequestAuditValueDetailValuesForProtocols(values RequestAuditValueDetailValues, inboundProtocol, wireProtocol string) (RequestAuditValueDetailValues, bool, bool) {
	if !requestAuditValueDetailProtocolSupported(inboundProtocol) || !requestAuditValueDetailProtocolSupported(wireProtocol) {
		return RequestAuditValueDetailValues{}, false, false
	}
	model, ok := normalizeRequestAuditValueDetailModel(values.Model)
	if !ok {
		return RequestAuditValueDetailValues{}, false, false
	}
	inboundHeaders, inboundDropped, ok := normalizeRequestAuditValueDetailHeaderValuesForProtocol(values.Inbound.RequestHeaders, inboundProtocol, false)
	if !ok {
		return RequestAuditValueDetailValues{}, false, false
	}
	deviceID, ok := normalizeRequestAuditValueDetailIdentifier(values.Inbound.DeviceID)
	if !ok {
		return RequestAuditValueDetailValues{}, false, false
	}
	accountUUID, ok := normalizeRequestAuditValueDetailIdentifier(values.Inbound.AccountUUID)
	if !ok {
		return RequestAuditValueDetailValues{}, false, false
	}
	sessionID, ok := normalizeRequestAuditValueDetailIdentifier(values.Inbound.SessionID)
	if !ok {
		return RequestAuditValueDetailValues{}, false, false
	}
	out := RequestAuditValueDetailValues{
		Model: model,
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: inboundHeaders,
			DeviceID:       deviceID,
			AccountUUID:    accountUUID,
			SessionID:      sessionID,
		},
		// 载荷自带的 truncated 原样带出：本层不替调用方决定怎么处理自己的丢弃事实。
		Truncated: values.Truncated,
	}
	dropped := inboundDropped
	for _, attempt := range values.Attempts {
		protocol := attempt.Protocol
		if protocol == "" {
			protocol = wireProtocol
		}
		if !requestAuditValueDetailProtocolSupported(protocol) || protocol != wireProtocol {
			// Mixed wire protocols cannot reuse one outbound contract. Fail closed
			// until the payload carries an independently validated contract for each.
			return RequestAuditValueDetailValues{}, false, false
		}
		normalized, attemptDropped, ok := normalizeRequestAuditValueDetailAttemptForProtocol(attempt, protocol)
		if !ok {
			return RequestAuditValueDetailValues{}, false, false
		}
		dropped = dropped || attemptDropped
		out.Attempts = append(out.Attempts, normalized)
	}
	return out, dropped, true
}

func normalizeRequestAuditValueDetailAttemptForProtocol(attempt RequestAuditValueDetailAttemptValues, protocol string) (RequestAuditValueDetailAttemptValues, bool, bool) {
	model, ok := normalizeRequestAuditValueDetailModel(attempt.Model)
	if !ok {
		return RequestAuditValueDetailAttemptValues{}, false, false
	}
	requestHeaders, requestDropped, ok := normalizeRequestAuditValueDetailHeaderValuesForProtocol(attempt.RequestHeaders, protocol, false)
	if !ok {
		return RequestAuditValueDetailAttemptValues{}, false, false
	}
	responseHeaders, responseDropped, ok := normalizeRequestAuditValueDetailHeaderValuesForProtocol(attempt.ResponseHeaders, protocol, true)
	if !ok {
		return RequestAuditValueDetailAttemptValues{}, false, false
	}
	deviceID, ok := normalizeRequestAuditValueDetailIdentifier(attempt.DeviceID)
	if !ok {
		return RequestAuditValueDetailAttemptValues{}, false, false
	}
	accountUUID, ok := normalizeRequestAuditValueDetailIdentifier(attempt.AccountUUID)
	if !ok {
		return RequestAuditValueDetailAttemptValues{}, false, false
	}
	sessionID, ok := normalizeRequestAuditValueDetailIdentifier(attempt.SessionID)
	if !ok {
		return RequestAuditValueDetailAttemptValues{}, false, false
	}
	out := RequestAuditValueDetailAttemptValues{
		Index:           attempt.Index,
		AccountID:       attempt.AccountID,
		Protocol:        protocol,
		Model:           model,
		RequestHeaders:  requestHeaders,
		ResponseHeaders: responseHeaders,
		DeviceID:        deviceID,
		AccountUUID:     accountUUID,
		SessionID:       sessionID,
	}
	if attempt.UpstreamStatus != nil && *attempt.UpstreamStatus >= 100 && *attempt.UpstreamStatus <= 599 {
		status := *attempt.UpstreamStatus
		out.UpstreamStatus = &status
	}
	if attempt.LatencyMillis != nil && *attempt.LatencyMillis >= 0 && *attempt.LatencyMillis <= RequestAuditValueDetailMaxLatencyMillis {
		latency := *attempt.LatencyMillis
		out.LatencyMillis = &latency
	}
	if attempt.ProxyID > 0 {
		out.ProxyID = attempt.ProxyID
	}
	return out, requestDropped || responseDropped, true
}

// normalizeRequestAuditValueDetailHeaderValues 把候选头值交回**同一套** httpattempt
// 净化器再取一次结果，并保留**有界的值列表**。
//
// 收窄来自净化器本身，因此：
//   - 不在闭集里的头名（客户端自带的未知头）与没通过净化器的取值只是被丢弃，
//     整份快照依然成立，并通过 dropped 标记如实表示「有条目没被收下」；
//   - 凭据类头（Cookie／Authorization／X-Api-Key／Proxy-Authorization 等）只会得到
//     存在性标记，而存在性不是值，本层直接丢弃——它同样不算「丢过条目」，
//     因为本能力从设计上就只保存值。
//
// 只有「无法唯一表示」的形状才整份不合格：取值个数为 0 或超过净化器上限（4）、
// 元素不是字符串、取值或名字超界、含控制字符。额外还有一层**值内凭据形态**检查：
// 即使净化器误放行了看起来像凭据的值，本层也拒绝整份快照，绝不落库。
func normalizeRequestAuditValueDetailHeaderValuesForProtocol(values map[string][]string, protocol string, response bool) (map[string][]string, bool, bool) {
	if len(values) == 0 {
		return nil, false, true
	}
	if len(values) > RequestAuditValueDetailMaxHeaderEntries {
		return nil, false, false
	}
	candidate := make(map[string]any, len(values))
	provided := 0
	bytesUsed := 0
	for rawName, rawValues := range values {
		if strings.ContainsAny(rawName, "\r\n\x00") {
			return nil, false, false
		}
		if len(rawValues) == 0 || len(rawValues) > requestAuditValueDetailMaxHeaderValueList {
			return nil, false, false
		}
		trimmed := make([]string, 0, len(rawValues))
		for _, raw := range rawValues {
			text := strings.TrimSpace(raw)
			if text == "" || len(text) > RequestAuditValueDetailMaxHeaderValueBytes {
				return nil, false, false
			}
			trimmed = append(trimmed, text)
			bytesUsed += len(rawName) + len(text)
		}
		if bytesUsed > RequestAuditValueDetailMaxHeaderBytesPerDirection {
			return nil, false, false
		}
		candidate[rawName] = trimmed
		provided += len(trimmed)
	}
	// 这里刻意**不**再做一次「值内凭据形态」的模糊扫描：闭集里的每一个头都有自己的
	// 语义规则（闭集枚举、数字、媒体类型、主机语法、不透明标识形状），净化器已经逐条
	// 校验过。再加一层子串匹配只会误伤合法取值——例如 anthropic-beta 里的特性名
	// `token-counting-2024-11-01`，或将来任何含 `secret`／`cookie` 之类的特性名，
	// 一旦命中就会把整份快照判为不合格。凭据类头本身根本不在闭集里，它们只会得到
	// 存在性标记并被丢弃。
	var revalidated map[string]any
	var supported bool
	if response {
		revalidated, supported = httpattempt.SanitizeProtocolResponseHeaderValueMap(protocol, candidate)
	} else {
		revalidated, supported = httpattempt.SanitizeProtocolRequestHeaderValueMap(protocol, candidate)
	}
	if !supported {
		return nil, false, false
	}
	out := make(map[string][]string, len(revalidated))
	accepted := 0
	for name, value := range revalidated {
		if requestAuditValueDetailPresenceOnly(value) {
			// 凭据头的存在性标记：本能力只保存值，因此不落库，也不算丢过条目。
			continue
		}
		safe, ok := requestAuditValueDetailStringList(value)
		if !ok {
			return nil, false, false
		}
		out[name] = safe
		accepted += len(safe)
	}
	if len(out) == 0 {
		return nil, provided > 0, true
	}
	return out, accepted < provided, true
}

// requestAuditValueDetailMaxHeaderValueList 与净化器的取值个数上限一致（最多 4 个）。
const requestAuditValueDetailMaxHeaderValueList = 4

// requestAuditValueDetailStringList 接受净化器的单值字符串或**有界**多值切片。
func requestAuditValueDetailStringList(value any) ([]string, bool) {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" || len(text) > RequestAuditValueDetailMaxHeaderValueBytes {
			return nil, false
		}
		return []string{text}, true
	case []string:
		return requestAuditValueDetailBoundedStrings(typed)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return requestAuditValueDetailBoundedStrings(out)
	default:
		return nil, false
	}
}

func requestAuditValueDetailBoundedStrings(values []string) ([]string, bool) {
	if len(values) == 0 || len(values) > requestAuditValueDetailMaxHeaderValueList {
		return nil, false
	}
	out := make([]string, 0, len(values))
	for _, raw := range values {
		text := strings.TrimSpace(raw)
		if text == "" || len(text) > RequestAuditValueDetailMaxHeaderValueBytes {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

// normalizeRequestAuditValueDetailIdentifier 校验解析出的客户端标识（device_id／
// account_uuid／session_id）的形状：受字符集约束的有界不透明标识，不含空白与控制字符。
//
// 空值是合法事实（account_uuid 常常为空），但不允许承载任意自由文本：
// 未知形状一律判不合格，整份丢弃。
func normalizeRequestAuditValueDetailIdentifier(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	if len(value) > RequestAuditValueDetailMaxIdentifierBytes {
		return "", false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':', c == '+', c == '/':
		default:
			return "", false
		}
	}
	if requestAuditValueDetailLooksLikeCredential(value) {
		return "", false
	}
	return value, true
}

// normalizeRequestAuditValueDetailModel 校验模型名的形状：有界、无控制字符、不含凭据形态。
//
// The caller controls this alias. Shape bounds and a risk acknowledgement
// limit, but cannot eliminate, the risk of short secret-like content in plaintext.
func normalizeRequestAuditValueDetailModel(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	if len(value) > RequestAuditValueDetailMaxModelBytes {
		return "", false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x20 || c == 0x7f {
			return "", false
		}
	}
	if requestAuditValueDetailLooksLikeCredential(value) {
		return "", false
	}
	return value, true
}

// requestAuditValueDetailCredentialPrefixes 是**无歧义的凭据前缀**（按小写比较）。
//
// 它们只用在两个「取值本身没有闭集语义」的字段上：模型名与 metadata.user_id 解析出的标识。
// 头值路径**不使用**它们——闭集里的每个头都有净化器给的语义规则，再加子串匹配只会误伤
// 合法取值（`anthropic-beta: token-counting-2024-11-01` 这类特性名就是例子）。
//
// 标记刻意只保留前缀形态：`secret`／`apikey`／`password` 这类**通用词**不做子串匹配，
// 否则一个不透明客户端标识只要碰巧含 `key` 或 `secret` 就会被整份拒绝——那是误伤，
// 不是防护。标识的字符集与长度上限才是拦住自由文本的那一道。
var requestAuditValueDetailCredentialPrefixes = []string{
	"bearer ",
	"basic ",
	"sk-",
	"sk_",
	"ghp_",
	"gho_",
	"xoxb-",
	"xoxp-",
	"-----begin",
}

func requestAuditValueDetailLooksLikeCredential(value string) bool {
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	for _, prefix := range requestAuditValueDetailCredentialPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// EncodeRequestAuditValueDetailValues creates bounded JSON with stable key
// ordering. The legacy cipher and new plaintext store share the same payload
// shape but never share each other's lifetime rules.
func EncodeRequestAuditValueDetailValues(values RequestAuditValueDetailValues) ([]byte, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if len(encoded) > RequestAuditValueDetailMaxPayloadBytes {
		return nil, errors.New("request audit value detail payload is too large")
	}
	return encoded, nil
}

// DecodeRequestAuditValueDetailValues revalidates before reveal. A successful
// DB read or legacy decrypt does not make payload data trustworthy; no partial
// values are returned when validation fails.
//
// 读侧复核拒收的条目**不是**校验失败：那是读侧自己的一条事实。它不并进载荷自带的
// truncated（那会把「读侧没收下」冒充成「采集时就没收下」），也不静默消失（那会让
// 剩下的列表被读成「客户端只发了这些」），而是通过 RequestAuditValueDetailValues.
// ValidationDropped（传输专用，不进存储载荷）交给揭示 DTO，与 Truncated 各自独立
// 呈现，符合 ADR 0006 对读侧告警的要求。载荷自带的 truncated 原样带出，不改写。
func DecodeRequestAuditValueDetailValues(payload []byte) (RequestAuditValueDetailValues, error) {
	return DecodeRequestAuditValueDetailValuesForProtocol(payload, RequestAuditProtocolAnthropic)
}

func DecodeRequestAuditValueDetailValuesForProtocol(payload []byte, protocol string) (RequestAuditValueDetailValues, error) {
	return DecodeRequestAuditValueDetailValuesForProtocols(payload, protocol, protocol)
}

// DecodeRequestAuditValueDetailValuesForProtocols 是读侧的双协议形式：入站头值按
// inboundProtocol 复核，逐次尝试的头值按 wireProtocol 复核（与采集侧同一套协议校验，
// 见 NormalizeRequestAuditValueDetailValuesForProtocols）。
//
// 调用方（仓储层）按**存储格式**决定入站协议：新明文行用入站路由推导出的协议；
// 旧密文行在写下的那一刻还没有这个事实，因此仍按原「单协议」语义（inbound=wire）解读，
// 不能把旧行重新按路由解释——那会把旧行里合法的入站头值判成读侧丢弃而整份拒绝。
func DecodeRequestAuditValueDetailValuesForProtocols(payload []byte, inboundProtocol, wireProtocol string) (RequestAuditValueDetailValues, error) {
	if len(payload) == 0 || len(payload) > RequestAuditValueDetailMaxReadBytes {
		return RequestAuditValueDetailValues{}, errors.New("request audit value detail payload is out of bounds")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded RequestAuditValueDetailValues
	if err := decoder.Decode(&decoded); err != nil {
		return RequestAuditValueDetailValues{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return RequestAuditValueDetailValues{}, errors.New("request audit value detail payload has trailing data")
	}
	normalized, dropped, ok := normalizeRequestAuditValueDetailValuesForProtocols(decoded, inboundProtocol, wireProtocol)
	if !ok {
		return RequestAuditValueDetailValues{}, errors.New("request audit value detail payload failed validation")
	}
	// 收到的载荷里有条目没通过本视图校验。这是**读侧自己的事实**，与载荷自带的
	// truncated（采集时就没收下）是两条独立事实（ADR 0006）：既不并进 Truncated，
	// 也不静默消失——静默丢掉会让剩下的列表被当成「客户端只发了这些」。它作为
	// 传输专用的标记随返回值带出去，由揭示 DTO 与 Truncated 各自独立呈现。
	// 被拒收的条目本身已经被丢掉，因此这里返回的仍然只有通过白名单与有界校验的值。
	normalized.ValidationDropped = dropped
	return normalized, nil
}

// requestAuditValueDetailValuesFromInput 把采集输入收敛成待校验的值视图。
//
// 它只做形状收敛（把净化器输出降为「一个名字一个字符串」），不做白名单判定：
// 白名单与有界校验由 NormalizeRequestAuditValueDetailValues 负责。
//
// 它同时还语义化采集侧带进来的省略摘要：净化器在服务层之前就丢掉了未知头名与
// 没通过校验的取值，因此「有条目没被收下」这个事实只能在这里被写进值视图，
// 再由 Normalize 原样保留，否则载荷里的 truncated 在生产上永远是 false。
func requestAuditValueDetailValuesFromInput(in RequestAuditValueDetailInput) RequestAuditValueDetailValues {
	values := RequestAuditValueDetailValues{
		Model: in.Model,
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: requestAuditValueDetailHeaderStrings(in.InboundHeaderValues),
		},
		Truncated: in.InboundHeaderOmission.Any(),
	}
	requestAuditValueDetailApplyMetadataUserID(in.MetadataUserID, &values.Inbound.DeviceID, &values.Inbound.AccountUUID, &values.Inbound.SessionID)
	for _, attempt := range in.Attempts {
		item := RequestAuditValueDetailAttemptValues{
			Index:           attempt.Index,
			AccountID:       attempt.AccountID,
			Protocol:        attempt.Protocol,
			Model:           attempt.Model,
			UpstreamStatus:  attempt.UpstreamStatus,
			LatencyMillis:   attempt.LatencyMillis,
			ProxyID:         attempt.ProxyID,
			RequestHeaders:  requestAuditValueDetailHeaderStrings(attempt.RequestHeaderValues),
			ResponseHeaders: requestAuditValueDetailHeaderStrings(attempt.ResponseHeaderValues),
		}
		requestAuditValueDetailApplyMetadataUserID(attempt.MetadataUserID, &item.DeviceID, &item.AccountUUID, &item.SessionID)
		values.Truncated = values.Truncated || attempt.HeaderOmission.Any()
		values.Attempts = append(values.Attempts, item)
	}
	return values
}

// RequestAuditValueDetailInboundHeaders 把入站请求头收敛成值明细输入的入站部分：
// 净化后的头值快照 + **只含计数**的省略摘要。
//
// 两个返回值必须成对使用：只取快照会让「未知头被净化器丢弃」这个事实在到达服务层之前
// 消失，载荷就永远报不出 truncated（ADR 0006 要求「客户端只发了这些」与「我们只收了这些」
// 可区分）。摘要只含计数，因此调用方绝不会把未知头名（可能承载认证通道）写成任何形式的记录。
func RequestAuditValueDetailInboundHeaders(headers http.Header) (map[string]any, httpattempt.ClaudeHeaderValueOmission) {
	values, omission, _ := RequestAuditValueDetailInboundHeadersForProtocol(headers, RequestAuditProtocolAnthropic)
	return values, omission
}

func RequestAuditValueDetailInboundHeadersForProtocol(headers http.Header, protocol string) (map[string]any, httpattempt.ClaudeHeaderValueOmission, bool) {
	return httpattempt.SanitizeProtocolRequestHeaderValues(protocol, headers)
}

// requestAuditValueDetailHeaderStrings 把净化器输出降为「一个名字一串有界值」。
//
// 存在性标记（已知凭据头）不是值，直接丢弃；多值头原样保留（上限 4）。
// 无法唯一表示的形状用哨兵条目标记，让整份快照在后续白名单复核里必然失败
// （fail closed），而不是被静默截断成「客户端只发了这一行」。
func requestAuditValueDetailHeaderStrings(sanitized map[string]any) map[string][]string {
	if len(sanitized) == 0 {
		return nil
	}
	out := make(map[string][]string, len(sanitized))
	for name, value := range sanitized {
		if requestAuditValueDetailPresenceOnly(value) {
			continue
		}
		values, ok := requestAuditValueDetailStringList(value)
		if !ok {
			// 超出 4 个取值、元素非字符串等：用哨兵标记整份不合格。
			out[requestAuditValueDetailInvalidShapeKey] = []string{""}
			continue
		}
		out[strings.TrimSpace(name)] = values
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// requestAuditValueDetailInvalidShapeKey 是不带可打印字符的哨兵名字：
// 它让「形状不合格」在后续白名单复核里必然失败，从而整份丢弃，而不是部分写入。
const requestAuditValueDetailInvalidShapeKey = "\x00invalid-shape"

// requestAuditValueDetailPresenceOnly 只承认净化器定义的存在性标记形状：
// 恰好一个键 `present`，值为 true。其余对象形状一律不认。
func requestAuditValueDetailPresenceOnly(value any) bool {
	mapValue, ok := value.(map[string]any)
	if !ok || len(mapValue) != 1 {
		return false
	}
	present, ok := mapValue["present"].(bool)
	return ok && present
}

// requestAuditValueDetailApplyMetadataUserID 解析 metadata.user_id 并写回三个组件。
//
// 解析失败时三个组件保持空：无法解析不是「有值但没存」，而是「没有可用的标识事实」。
func requestAuditValueDetailApplyMetadataUserID(raw string, deviceID, accountUUID, sessionID *string) {
	parsed := ParseMetadataUserID(raw)
	if parsed == nil {
		return
	}
	*deviceID = parsed.DeviceID
	*accountUUID = parsed.AccountUUID
	*sessionID = parsed.SessionID
}

// RequestAuditValueDetailAttemptsFromHTTPMetadata 把传输层尝试元数据按序转成值明细尝试。
//
// 只保留携带**任何**可留存事实的尝试：Claude 值快照、metadata.user_id、耗时、代理内部 ID，
// 或**省略摘要**（「客户端发了我们没收的东西」本身就是这条能力要回答的问题）。
// 两者皆无的尝试不会造出空条目——记下一条什么都没有的尝试只会让界面看起来「采到了」。
// 耗时与代理 ID 通常每次尝试都有，因此 Anthropic 路径上的真实尝试会被完整保留，
// 这正是「尝试级耗时」与「代理内部 ID」的来源。
//
// 省略摘要按尝试合并请求与响应两个方向（只累加计数），它最终决定载荷里的 truncated。
//
// 超出 RequestAuditValueDetailMaxAttempts 的部分**不在这里截断**：调用方应把完整尝试数
// 交给 BuildRequestAuditValueDetailWrite，由它落成 skipped_too_many_attempts，
// 从而不会把前 8 次冒充成「全部尝试」。
func RequestAuditValueDetailAttemptsFromHTTPMetadata(metadata []httpattempt.Metadata) []RequestAuditValueDetailAttempt {
	if len(metadata) == 0 {
		return nil
	}
	attempts := make([]RequestAuditValueDetailAttempt, 0, len(metadata))
	for index, item := range metadata {
		omission := item.RequestHeaderValueOmission.Merge(item.ResponseHeaderValueOmission)
		if len(item.RequestHeaderValues) == 0 && len(item.ResponseHeaderValues) == 0 &&
			strings.TrimSpace(item.MetadataUserID) == "" && item.LatencyMillis == nil && item.ProxyID <= 0 &&
			!omission.Any() && item.ValueProtocol == "" {
			continue
		}
		attempts = append(attempts, RequestAuditValueDetailAttempt{
			Index:                index + 1,
			AccountID:            item.AccountID,
			Protocol:             RequestAuditValueWireProtocolForMetadata(item),
			Model:                item.Model,
			MetadataUserID:       item.MetadataUserID,
			UpstreamStatus:       cloneAuditInt(item.StatusCode),
			LatencyMillis:        cloneAuditInt64(item.LatencyMillis),
			ProxyID:              item.ProxyID,
			RequestHeaderValues:  item.RequestHeaderValues,
			ResponseHeaderValues: item.ResponseHeaderValues,
			HeaderOmission:       omission,
		})
	}
	return attempts
}

func requestAuditValueDetailFields(in RequestAuditValueDetailInput, route string) RequestAuditValueDetailFields {
	protocol := strings.TrimSpace(in.Protocol)
	switch protocol {
	case RequestAuditProtocolAnthropic, RequestAuditProtocolOpenAIChat, RequestAuditProtocolOpenAIResp, "bedrock":
	default:
		protocol = ""
	}
	fields := RequestAuditValueDetailFields{
		Route:     route,
		Protocol:  protocol,
		StartedAt: in.StartedAt.UTC(),
	}
	if in.StartedAt.IsZero() {
		fields.StartedAt = time.Time{}
	}
	if !in.CompletedAt.IsZero() && !in.CompletedAt.Before(in.StartedAt) {
		fields.CompletedAt = in.CompletedAt.UTC()
	}
	if in.ClientStatus >= 100 && in.ClientStatus <= 599 {
		fields.ClientStatus = in.ClientStatus
	}
	return fields
}

// RequestAuditValueDetailCapture 是采集接缝：调用方只需在请求审计行落库之后调用 Capture。
//
// nil 接收者、nil 仓储与 nil 使用记录都是安全的空操作。本接缝 fail-open：
// 任何失败都只写一条不含值的日志，绝不影响已经交付给客户端的请求，
// 也绝不返回错误给调用方。「先有 request_audits 行」由仓储层守卫，
// 因此即使调用方顺序有误，也不会写出没有审计行的值明细。
type RequestAuditValueDetailCapture struct {
	repo RequestAuditValueDetailRepository
	gate RequestAuditValueDetailGateReader
	now  func() time.Time
}

var requestAuditValueDetailWrittenRows atomic.Uint64
var requestAuditValueDetailWrittenBytes atomic.Uint64
var requestAuditValueDetailInvalidRows atomic.Uint64
var requestAuditValueDetailWriteFailures atomic.Uint64

func RequestAuditValueDetailCaptureCounts() (writtenRows, writtenBytes, invalidRows, failures uint64) {
	return requestAuditValueDetailWrittenRows.Load(), requestAuditValueDetailWrittenBytes.Load(),
		requestAuditValueDetailInvalidRows.Load(), requestAuditValueDetailWriteFailures.Load()
}

// NewRequestAuditValueDetailCapture 构造采集接缝；gate 可为 nil（等价于全关）。
func NewRequestAuditValueDetailCapture(repo RequestAuditValueDetailRepository, gate RequestAuditValueDetailGateReader) *RequestAuditValueDetailCapture {
	return &RequestAuditValueDetailCapture{repo: repo, gate: gate, now: time.Now}
}

func (c *RequestAuditValueDetailCapture) gateFor(ctx context.Context) RequestAuditValueDetailGate {
	if c == nil || c.gate == nil {
		return RequestAuditValueDetailGate{}
	}
	return c.gate.RequestAuditValueDetailGate(ctx)
}

func (c *RequestAuditValueDetailCapture) clockNow() time.Time {
	if c == nil || c.now == nil {
		return time.Now()
	}
	return c.now()
}

// Enabled 报告此刻是否值得为本次逻辑请求复制值快照。
//
// 只有当前已确认的运维门控允许时才复制值；新明文不要求旧解密密钥。
//
// 调用方（gateway 绑定阶段）用它决定是否在请求上下文上打
// httpattempt.WithClaudeHeaderValueCapture 标记；传输层只认那个标记，
// 因此这个判定点是关闭态下不复制明文的唯一开关。
func (c *RequestAuditValueDetailCapture) Enabled(ctx context.Context) bool {
	return c.gateFor(ctx).CaptureAllowed
}

// Capture 在请求审计行已落库后尽力写入值明细。
//
// 调用约定：
//   - 必须在 AttachRequestAuditAfterUsageLog／finalizeRequestAuditBestEffort 之后调用；
//   - gate 关闭、路由不在范围内或没有任何可留存值时，不会读取或复制任何值；
//   - 失败只影响值明细，绝不影响使用记录、请求审计或上游调用。
func (c *RequestAuditValueDetailCapture) Capture(ctx context.Context, usageLog *UsageLog, in RequestAuditValueDetailInput) {
	if c == nil || c.repo == nil || usageLog == nil || usageLog.ID <= 0 {
		return
	}
	gate := c.gateFor(ctx)
	if !gate.CaptureAllowed {
		// 默认关闭意味着这条能力在这个部署上**不存在**：一个字节都不写，也不为每次
		// /v1/messages 都留一行「没留存」的空壳。「未采集」由「没有行」与信封里的
		// capability_enabled=false 表达，绝不伪造一条记录。
		return
	}
	write := BuildRequestAuditValueDetailWrite(in, gate, c.clockNow())
	if write == nil {
		return
	}
	write.UsageLogID = usageLog.ID
	if write.Reason == RequestAuditValueDetailSkippedInvalidValues || write.Reason == RequestAuditValueDetailSkippedTooManyAttempts {
		requestAuditValueDetailInvalidRows.Add(1)
	}
	captureCtx, cancel := context.WithTimeout(detachedRequestAuditValueDetailContext(ctx), requestAuditValueDetailWriteTimeout)
	defer cancel()
	saved, err := c.repo.CreateRequestAuditValueDetail(captureCtx, *write)
	if err != nil {
		requestAuditValueDetailWriteFailures.Add(1)
		// 只记原因码与状态：任何值、模型名或标识都不进日志。
		logger.LegacyPrintf("service.request_audit_value_detail",
			"Request audit value detail write failed: usage_log_id=%d state=%s reason=%s", usageLog.ID, write.State, write.Reason)
		return
	}
	if saved.Stored && saved.StorageFormat == RequestAuditValueDetailStoragePlaintextUsageBound {
		requestAuditValueDetailWrittenRows.Add(1)
		requestAuditValueDetailWrittenBytes.Add(uint64(saved.PayloadBytes))
	}
}

// detachedRequestAuditValueDetailContext 让写入不受客户端断开影响，但保留请求内的
// trace／日志关联值。与既有审计写入使用同一约定。
func detachedRequestAuditValueDetailContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}
