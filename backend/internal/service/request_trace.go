package service

import (
	"context"
	"errors"
	"time"
)

const (
	RequestTraceStagePayloadLimit = 1 << 20
	RequestTraceUnlinkedRetention = 30 * 24 * time.Hour
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
	UsageLinked  *bool
	// UsageLogID / AccountID 也是可选检索：nil 表示"不过滤"，只有 >0 的 ID 才是
	// 有效条件，0 或负数会被拒绝而不是退化成"返回全部"。
	//
	// UsageLogID 直接匹配 Trace 信封上的关联使用记录，供从使用记录跳转而来时定位。
	// AccountID 只匹配 wire_attempt 阶段的类型化事实（stage facts 里的 account_id），
	// 不是对任意元数据 JSONB 的全文匹配：一次 Trace 只要有一条真实上游尝试使用了该
	// 账号即命中，列表响应本身仍只返回元数据信封，不回传阶段 JSONB。
	UsageLogID *int64
	AccountID  *int64
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
