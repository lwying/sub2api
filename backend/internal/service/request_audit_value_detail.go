package service

// Claude Messages /v1/messages 请求审计的**值**明细旁路（ADR 0006）。
//
// 既有请求审计只保存协议元数据与事件骨架（ADR 0001），长期存活的 request_audits 行
// 永远不含模型正文、认证凭据原文，也不含任何头**值**。本文件实现一个**有范围**的例外：
//
//   - 只对入站路由为 /v1/messages、且这次逻辑请求真的走了 Anthropic 上游的请求；
//   - 只保存通过**闭集白名单 + 有界校验**的**值**：入站请求头值、每次真实上游尝试的
//     请求／响应头值，metadata.user_id 解析出的 device_id／session_id／account_uuid，
//     以及最终模型与每次尝试的模型；
//   - 值一律加密（专用 HKDF 子密钥，见 repository 层），缺稳定密钥时一律不留存，
//     **不存在明文回退**；
//   - 默认关闭：门控与风险确认都在服务层判定（见
//     request_audit_value_detail_settings.go），调用方不得自称可以采集；
//   - 只在使用记录与 request_audits 行都已存在时写入，且与使用记录同生共死。
//
// 明文信封（存储列）刻意**不含模型名**：模型别名由调用方任意指定，既有审计因此从不落库
// 模型名（见 request_audit.go 的 SanitizeRequestAuditAttempt）。本能力把模型名放进密文载荷，
// 因此它和其它值一样只受 7 天披露窗口保护，不会在到期后以任何形式残留可读明文。
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

	// RequestAuditValueDetailRouteMessages 是本能力唯一覆盖的入站路由。
	RequestAuditValueDetailRouteMessages = "/v1/messages"

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
// 这里只放调用方无法任意构造的封闭事实：路由与协议都是闭集枚举，状态码有界，
// 时间是服务端事实。模型名刻意不在这里（它由调用方任意指定，只进密文）。
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
// LatencyMillis 与 ProxyID 是尝试级的标量事实（耗时、代理内部 ID）：它们不是敏感值，
// 但仍然只存在于**密文载荷**里，因此同样受 7 天披露窗口保护，不会以明文列残留。
type RequestAuditValueDetailAttemptValues struct {
	Index           int                 `json:"index"`
	AccountID       int64               `json:"account_id,omitempty"`
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

// RequestAuditValueDetailValues 是解密后的值视图（白名单收敛结果）。
//
// Model 是最终模型名：它在这里而不是明文列，正是因为它由调用方任意指定。
//
// Truncated 表示「调用方提供的值里有条目没被收下」（不在闭集里的头名，或取值没通过
// 净化器）。它不是失败，也不能藏起来：没有这个标记，管理员无法区分
// 「客户端只发了这些」与「我们只收了这些」。
//
// 这个事实有两个来源，且**两者都必须写进载荷**：
//   - 净化器在采集那一刻丢掉的头名／取值（由 httpattempt.ClaudeHeaderValueOmission 计数带入，
//     因为它在服务层已经看不见了）；
//   - 本层白名单复核时丢掉的条目（见 NormalizeRequestAuditValueDetailValues）。
type RequestAuditValueDetailValues struct {
	Model     string                                 `json:"model,omitempty"`
	Inbound   RequestAuditValueDetailInboundValues   `json:"inbound"`
	Attempts  []RequestAuditValueDetailAttemptValues `json:"attempts,omitempty"`
	Truncated bool                                   `json:"truncated,omitempty"`
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

// RequestAuditValueDetailWrite 是写入路径的完整输入：标量字段 + 待加密载荷。
//
// Payload 只在 State == stored 时非空；加密由仓储层用专用密钥完成，
// 失败即退化为 skipped_encryption_unavailable，绝不回退为明文。
type RequestAuditValueDetailWrite struct {
	UsageLogID   int64
	State        string
	Reason       string
	Fields       RequestAuditValueDetailFields
	Payload      []byte
	AttemptCount int
	EntryCount   int
	ExpiresAt    time.Time
}

// Retained 报告本次写入是否携带可加密的值载荷。
func (w RequestAuditValueDetailWrite) Retained() bool {
	return w.State == RequestAuditValueDetailStateStored && len(w.Payload) > 0
}

// RequestAuditValueDetail 是值明细旁路的存储信封，不含明文值。
type RequestAuditValueDetail struct {
	UsageLogID   int64
	State        string
	Reason       string
	Fields       RequestAuditValueDetailFields
	Stored       bool
	KeyVersion   int
	AttemptCount int
	EntryCount   int
	PayloadBytes int
	ExpiresAt    time.Time
	CreatedAt    time.Time
}

// Expired 报告该行是否已过 7 天披露窗口（按到期时刻精确判定）。
func (d RequestAuditValueDetail) Expired(now time.Time) bool {
	return !d.ExpiresAt.IsZero() && !d.ExpiresAt.After(now)
}

// Readable 报告此刻是否可以读取值：有未到期密文，且未过 7 天窗口。
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
// CaptureAllowed 是「运维允许不允许」（默认关闭，且必须携带书面风险确认）；
// EncryptionAvailable 是「部署做不做得到」（有没有跨重启稳定的配置密钥）。
// 两者都不为真时一个字节的值都不会被读取或复制。
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
// 范围是「入站 /v1/messages」**并且**「真实上游尝试走 Anthropic 协议形态」：
// 只有这种情况下的头值与 metadata.user_id 才对应官方 Claude Code 客户端的形态。
// /v1/messages 入站但被改写到别的上游协议时属于范围内但不可采，记 skipped_out_of_scope；
// 其它入站路由直接不在范围内（未采集）。
func RequestAuditValueDetailScopeApplies(route, protocol string) bool {
	return route == RequestAuditValueDetailRouteMessages && protocol == RequestAuditProtocolAnthropic
}

// BuildRequestAuditValueDetailWrite 判定本次采集的状态与原因，并给出可加密的载荷。
//
// 判定顺序固定：路由范围 → 协议范围 → 门控 → 尝试数 → 规范化与有界校验 →
// 密钥可用性 → 编码。任何一步不合格都只影响值明细，不影响使用记录、请求审计或本次上游调用。
// 返回 nil 表示「未采集」：入站路由不在范围内时一个字节都不写（由「无行」表达未采集）。
func BuildRequestAuditValueDetailWrite(in RequestAuditValueDetailInput, gate RequestAuditValueDetailGate, now time.Time) *RequestAuditValueDetailWrite {
	route := strings.TrimSpace(in.Route)
	if route != RequestAuditValueDetailRouteMessages {
		// 路由不在范围内：这是「未采集」，不是失败。返回 nil 表示不落任何行，
		// 避免为其它协议的流量写空壳记录。
		return nil
	}

	write := &RequestAuditValueDetailWrite{
		State:     RequestAuditValueDetailStateSkipped,
		Reason:    RequestAuditValueDetailSkippedOutOfScope,
		Fields:    requestAuditValueDetailFields(in, route),
		ExpiresAt: now.UTC().Add(RequestAuditValueDetailRetention),
	}

	if !RequestAuditValueDetailScopeApplies(route, strings.TrimSpace(in.Protocol)) {
		// 范围内路由、但这次真实上游尝试不是 Anthropic 形态：明确记「不在范围」。
		return write
	}

	if !gate.CaptureAllowed {
		write.Reason = RequestAuditValueDetailSkippedRetentionDisabled
		return write
	}

	if len(in.Attempts) > RequestAuditValueDetailMaxAttempts {
		// 计数必须收敛到存储上限：越界的 attempt_count 会让这一行被检查约束拒收，
		// 「尝试过多」这个事实就只剩日志，管理员看到的是「没有行」。
		write.AttemptCount = requestAuditValueDetailBoundedAttemptCount(len(in.Attempts))
		write.Reason = RequestAuditValueDetailSkippedTooManyAttempts
		return write
	}

	values, ok := NormalizeRequestAuditValueDetailValues(requestAuditValueDetailValuesFromInput(in))
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
		// 在范围内、开关也开着，但一条可留存的值都没有：这是「未采集」，
		// 不是「采集失败」，也不能写成「已留存」。省略摘要（有头没被收下）在这里
		// 不单独成行：truncated 只在载荷里有地方表达，而空载荷不是一次留存。
		// 真实 Claude 请求总有最终模型名，因此这一分支不会把「丢过条目」这个事实盖掉。
		write.State = RequestAuditValueDetailStateNotObserved
		write.Reason = RequestAuditValueDetailNotObserved
		return write
	}
	if !gate.EncryptionAvailable {
		// 开关开着但部署拿不出稳定密钥：留下稳定原因码，而不是退化成「未采集」。
		write.EntryCount = entryCount
		write.Reason = RequestAuditValueDetailSkippedEncryptionUnavailable
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
	model, ok := normalizeRequestAuditValueDetailModel(values.Model)
	if !ok {
		return RequestAuditValueDetailValues{}, false
	}
	inboundHeaders, inboundDropped, ok := normalizeRequestAuditValueDetailHeaderValues(values.Inbound.RequestHeaders, false)
	if !ok {
		return RequestAuditValueDetailValues{}, false
	}
	deviceID, ok := normalizeRequestAuditValueDetailIdentifier(values.Inbound.DeviceID)
	if !ok {
		return RequestAuditValueDetailValues{}, false
	}
	accountUUID, ok := normalizeRequestAuditValueDetailIdentifier(values.Inbound.AccountUUID)
	if !ok {
		return RequestAuditValueDetailValues{}, false
	}
	sessionID, ok := normalizeRequestAuditValueDetailIdentifier(values.Inbound.SessionID)
	if !ok {
		return RequestAuditValueDetailValues{}, false
	}
	out := RequestAuditValueDetailValues{
		Model: model,
		Inbound: RequestAuditValueDetailInboundValues{
			RequestHeaders: inboundHeaders,
			DeviceID:       deviceID,
			AccountUUID:    accountUUID,
			SessionID:      sessionID,
		},
		// 任一方向丢过条目，整行就带着「不完整」这个事实。
		Truncated: values.Truncated || inboundDropped,
	}
	for _, attempt := range values.Attempts {
		normalized, dropped, ok := normalizeRequestAuditValueDetailAttempt(attempt)
		if !ok {
			return RequestAuditValueDetailValues{}, false
		}
		out.Truncated = out.Truncated || dropped
		out.Attempts = append(out.Attempts, normalized)
	}
	return out, true
}

func normalizeRequestAuditValueDetailAttempt(attempt RequestAuditValueDetailAttemptValues) (RequestAuditValueDetailAttemptValues, bool, bool) {
	model, ok := normalizeRequestAuditValueDetailModel(attempt.Model)
	if !ok {
		return RequestAuditValueDetailAttemptValues{}, false, false
	}
	requestHeaders, requestDropped, ok := normalizeRequestAuditValueDetailHeaderValues(attempt.RequestHeaders, false)
	if !ok {
		return RequestAuditValueDetailAttemptValues{}, false, false
	}
	responseHeaders, responseDropped, ok := normalizeRequestAuditValueDetailHeaderValues(attempt.ResponseHeaders, true)
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
func normalizeRequestAuditValueDetailHeaderValues(values map[string][]string, response bool) (map[string][]string, bool, bool) {
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
	if response {
		revalidated = httpattempt.SanitizeClaudeResponseHeaderValueMap(candidate)
	} else {
		revalidated = httpattempt.SanitizeClaudeRequestHeaderValueMap(candidate)
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
// 模型名由调用方任意指定，因此它只进密文；这里再收一次形状，
// 避免它成为一条绕过正文禁令的自由文本通道。
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

// EncodeRequestAuditValueDetailValues 把校验后的值编码成确定性的 JSON 载荷。
//
// 确定性很重要：同一份值必须产生同一份密文，否则「以密文对比是否重复」会成为
// 一条本不存在的旁路。map 的键由 encoding/json 排序。
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

// DecodeRequestAuditValueDetailValues 解码并**重新校验**已解密的载荷。
//
// 解密成功不等于内容可信（密钥错配、密文被替换、旧版本载荷都可能落到这里），
// 因此解码结果必须再过一遍白名单与有界校验；不合格一律返回错误，绝不返回部分结果。
func DecodeRequestAuditValueDetailValues(payload []byte) (RequestAuditValueDetailValues, error) {
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
	normalized, ok := NormalizeRequestAuditValueDetailValues(decoded)
	if !ok {
		return RequestAuditValueDetailValues{}, errors.New("request audit value detail payload failed validation")
	}
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
	return httpattempt.SanitizeClaudeRequestHeaderValuesWithOmission(headers)
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
			!omission.Any() {
			continue
		}
		attempts = append(attempts, RequestAuditValueDetailAttempt{
			Index:                index + 1,
			AccountID:            item.AccountID,
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
	case RequestAuditProtocolAnthropic, "bedrock":
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
// 只有「运维允许」且「部署有稳定密钥」同时成立才为真。缺密钥时构建快照是纯粹的明文
// 复制，注定被判为 skipped_encryption_unavailable，因此不应发生。
//
// 调用方（gateway 绑定阶段）用它决定是否在请求上下文上打
// httpattempt.WithClaudeHeaderValueCapture 标记；传输层只认那个标记，
// 因此这个判定点是关闭态下不复制明文的唯一开关。
func (c *RequestAuditValueDetailCapture) Enabled(ctx context.Context) bool {
	gate := c.gateFor(ctx)
	return gate.CaptureAllowed && gate.EncryptionAvailable
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
	captureCtx, cancel := context.WithTimeout(detachedRequestAuditValueDetailContext(ctx), requestAuditValueDetailWriteTimeout)
	defer cancel()
	if _, err := c.repo.CreateRequestAuditValueDetail(captureCtx, *write); err != nil {
		// 只记原因码与状态：任何值、模型名或标识都不进日志。
		logger.LegacyPrintf("service.request_audit_value_detail",
			"Request audit value detail write failed: usage_log_id=%d state=%s reason=%s", usageLog.ID, write.State, write.Reason)
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
