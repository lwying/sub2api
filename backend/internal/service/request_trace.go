package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	RequestTraceStagePayloadLimit = 1 << 20
	RequestTraceUnlinkedRetention = 30 * 24 * time.Hour
	// RequestTraceObservedPlatformLimit 是一次逻辑请求在信封上保留的"实际选中平台"历史条数上限。
	// 平台取值来自封闭的账号平台枚举，实际远小于该上限；超出时按首次观察顺序截断——
	// 首个平台是采集范围的判定依据，必须保留。
	RequestTraceObservedPlatformLimit = 16
)

type RequestTraceRouteFamily string

const (
	RequestTraceMessages        RequestTraceRouteFamily = "messages"
	RequestTraceChatCompletions RequestTraceRouteFamily = "chat_completions"
	RequestTraceResponses       RequestTraceRouteFamily = "responses"
)

type RequestTraceCaptureState string

const (
	RequestTraceNotObserved RequestTraceCaptureState = "not_observed"
	RequestTracePartial     RequestTraceCaptureState = "partial"
	RequestTraceStored      RequestTraceCaptureState = "stored"
	RequestTraceTruncated   RequestTraceCaptureState = "truncated"
	RequestTraceUnsupported RequestTraceCaptureState = "unsupported"
	RequestTraceUnverified  RequestTraceCaptureState = "redaction_unverified"
	RequestTraceWriteFailed RequestTraceCaptureState = "write_failed"
)

var (
	ErrRequestTraceRepositoryUnavailable = errors.New("request trace repository unavailable")
	ErrRequestTraceInvalidRecord         = errors.New("invalid request trace record")
)

// RequestTrace is the metadata-only envelope. In particular, list responses cannot
// carry body bytes because this type has no stages or payload fields.
type RequestTrace struct {
	ID              int64                    `json:"-"`
	TraceID         string                   `json:"trace_id"`
	RouteFamily     RequestTraceRouteFamily  `json:"route_family"`
	InboundEndpoint string                   `json:"inbound_endpoint"`
	CaptureState    RequestTraceCaptureState `json:"capture_state"`
	ClientStatus    int                      `json:"client_status"`
	UsageLogID      *int64                   `json:"usage_log_id"`
	CreatedAt       time.Time                `json:"created_at"`
	CompletedAt     *time.Time               `json:"completed_at"`
	CleanupAfter    *time.Time               `json:"cleanup_after"`
	// GroupID 是下游 API Key 在**请求当时**所属的分组；nil 表示该事实未被观察到
	// （例如鉴权前被拒）。它是请求时事实，不随分组改名或删除而变化。
	GroupID *int64 `json:"group_id,omitempty"`
	// Authenticated downstream identity at request time. Nil is not observed,
	// including pre-upgrade traces and requests rejected before authentication.
	UserID   *int64 `json:"user_id"`
	APIKeyID *int64 `json:"api_key_id"`
	// Current display labels resolved only for administrator reads, not snapshotted.
	UserEmail  string `json:"user_email,omitempty"`
	APIKeyName string `json:"api_key_name,omitempty"`
	// RequestedModel 是客户端**请求的**模型名；空表示未观察到。
	// 它不是出站映射后的模型名（那属于上游尝试事实）。
	RequestedModel string `json:"requested_model,omitempty"`
	// ObservedPlatforms 是本次逻辑请求**实际选中过**的上游账号平台（去重、按首次观察顺序、有界）。
	// 它包含"账号已选中、但还没发出上游就失败"的平台：按规格 §2.2，那同样是一个已知事实，
	// 因此这条没有上游尝试的错误 Trace 也必须能被"任一实际选中平台"检索到，且不能被当成
	// "平台未知"。它同样是请求时事实，不随账号后来更换平台而变化。
	// nil 表示从未选到任何账号（未知）；空数组不是合法取值，未知只能用 nil 表达。
	ObservedPlatforms []string `json:"observed_platforms,omitempty"`
}

// RequestTraceObservedPlatformTokenOK 报告一个平台取值是否是可按原样落库的请求时 token。
// 与 wire_attempt 阶段事实共用同一条形状规则：不改写大小写、不做归一化猜测。
func RequestTraceObservedPlatformTokenOK(platform string) bool {
	return platform != "" && requestTraceFactTokenOrEmpty(platform) == platform
}

// NormalizeRequestTraceObservedPlatforms 归一化"本次实际选中过的平台"历史：
// 去首尾空白、丢弃形状不合法的条目、按首次观察顺序去重，并在上限处截断。
// 没有任何可用条目时返回 nil——未知只能用 nil 表达，空数组不能冒充"已观察但为空"。
// 它绝不按当前账号资料或使用记录补值。
func NormalizeRequestTraceObservedPlatforms(platforms []string) []string {
	if len(platforms) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(platforms))
	out := make([]string, 0, len(platforms))
	for _, platform := range platforms {
		trimmed := strings.TrimSpace(platform)
		if !RequestTraceObservedPlatformTokenOK(trimmed) {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
		if len(out) == RequestTraceObservedPlatformLimit {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ValidRequestTraceObservedPlatforms 是落库与读取边界的第二道校验：随阶段事实一起，
// 在队列、仓储与读回三处证明投影本身合法。nil（未知）合法；非 nil 必须非空、不超上限、
// 无重复，且每个条目都是合法 token。
func ValidRequestTraceObservedPlatforms(platforms []string) bool {
	if platforms == nil {
		return true
	}
	if len(platforms) == 0 || len(platforms) > RequestTraceObservedPlatformLimit {
		return false
	}
	seen := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		if !RequestTraceObservedPlatformTokenOK(platform) {
			return false
		}
		if _, ok := seen[platform]; ok {
			return false
		}
		seen[platform] = struct{}{}
	}
	return true
}

type RequestTraceStage struct {
	ID                  int64                    `json:"-"`
	TraceID             string                   `json:"-"`
	Ordinal             int                      `json:"ordinal"`
	Stage               string                   `json:"stage"`
	AttemptIndex        int                      `json:"attempt_index"`
	View                string                   `json:"view"`
	State               RequestTraceCaptureState `json:"state"`
	Reason              string                   `json:"reason"`
	ObservedBytes       int64                    `json:"observed_bytes"`
	RetainedBytes       int                      `json:"retained_bytes"`
	DroppedEvents       int                      `json:"dropped_events"`
	RedactionUnverified bool                     `json:"redaction_unverified"`
	Payload             []byte                   `json:"payload,omitempty"`
	// Metadata and Decision are the two typed projections a stage may carry. They
	// are never serialized from this envelope: the repository writes the one that
	// belongs to the stage into the single JSONB column, and the readers rebuild
	// the typed value for that stage.
	Metadata  *RequestTraceStageFacts    `json:"-"`
	Decision  *RequestTraceDecisionFacts `json:"-"`
	CreatedAt time.Time                  `json:"created_at"`
}

type RequestTraceDetail struct {
	RequestTrace
	Stages []RequestTraceStage `json:"stages"`
}

type RequestTraceListFilter struct {
	TraceID     string
	RouteFamily RequestTraceRouteFamily
	// ClientStatus 是指针：nil 表示"不过滤"，0 是合法值（列默认 0）。
	ClientStatus *int
	CreatedFrom  time.Time
	CreatedTo    time.Time
	Page         int
	PageSize     int
	// SkipCount is only used when the same request computes filter-scoped stats;
	// the aggregate supplies the displayed total instead of a redundant COUNT.
	SkipCount   bool
	UsageLinked *bool
	// Request-time authenticated identity; unknown means no authenticated identity was observed.
	UserID        *int64
	UserUnknown   *bool
	APIKeyID      *int64
	APIKeyUnknown *bool
	// Keyword searches only approved envelope metadata and current identity labels.
	Keyword string
	// UsageLogID / AccountID 也是可选检索：nil 表示"不过滤"，只有 >0 的 ID 才是
	// 有效条件，0 或负数会被拒绝而不是退化成"返回全部"。
	//
	// UsageLogID 直接匹配 Trace 信封上的关联使用记录，供从使用记录跳转而来时定位。
	// AccountID 只匹配 wire_attempt 阶段的类型化事实（stage facts 里的 account_id），
	// 不是对任意元数据 JSONB 的全文匹配：一次 Trace 只要有一条真实上游尝试使用了该
	// 账号即命中，列表响应本身仍只返回元数据信封，不回传阶段 JSONB。
	UsageLogID *int64
	AccountID  *int64
	// GroupID / RequestedModel 按"请求时事实"检索：分组是下游 API Key 在请求当时
	// 所属的分组，模型是客户端请求的模型名（非出站映射结果）。
	//
	// 两者各自配套一个 *Unknown 开关：为 true 时只匹配该事实**未观察到**的行，
	// nil 表示不加该条件。未知不等于任何一个具体值，也不从使用记录反推。
	GroupID        *int64
	GroupUnknown   *bool
	RequestedModel string
	ModelUnknown   *bool
	// Platform 按任一**实际选中**的上游账号平台检索，取两处请求时事实的并集：
	// wire_attempt 阶段的 platform（真实发出的尝试）与信封上的 ObservedPlatforms
	// （本次实际选中过的全部平台，含"已选中但还没发出上游就失败"的账号）。
	// PlatformUnknown 为 true 时只匹配两处都没有平台事实的 Trace（平台未知）。
	// 两者互斥。
	Platform        string
	PlatformUnknown *bool
}

// RequestTraceRepository stores the independent envelope and on-demand per-stage
// details. Failed writes never become a reason to alter a gateway response.
type RequestTraceRepository interface {
	CreateRequestTrace(ctx context.Context, trace RequestTrace) (RequestTrace, error)
	AppendRequestTraceStage(ctx context.Context, stage RequestTraceStage) (RequestTraceStage, error)
	ListRequestTraces(ctx context.Context, filter RequestTraceListFilter) ([]RequestTrace, int64, error)
	GetRequestTrace(ctx context.Context, traceID string) (*RequestTraceDetail, error)
	DeleteExpiredUnlinkedRequestTraces(ctx context.Context, before time.Time, limit int) (int64, error)
}
