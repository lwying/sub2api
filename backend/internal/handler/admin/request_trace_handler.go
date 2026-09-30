package admin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

var (
	requestTraceIDPattern          = regexp.MustCompile(`^[0-9a-f]{32}$`)
	errRequestTraceSessionRequired = infraerrors.Forbidden("REQUEST_TRACE_ADMIN_SESSION_REQUIRED", "an admin login session is required to read request traces")
	errRequestTraceUnavailable     = infraerrors.New(http.StatusServiceUnavailable, "REQUEST_TRACE_UNAVAILABLE", "request traces are temporarily unavailable")
	errRequestTraceNotFound        = infraerrors.NotFound("REQUEST_TRACE_NOT_FOUND", "request trace not found")
	errRequestTraceInvalidFilter   = infraerrors.BadRequest("REQUEST_TRACE_INVALID_FILTER", "invalid request trace filter")
)

type requestTraceReader interface {
	ListRequestTraces(ctx context.Context, filter service.RequestTraceListFilter) ([]service.RequestTrace, int64, error)
	GetRequestTrace(ctx context.Context, traceID string) (*service.RequestTraceDetail, error)
}

type RequestTraceHandler struct {
	reader requestTraceReader
}

func NewRequestTraceHandler(repo requestTraceReader) *RequestTraceHandler {
	return &RequestTraceHandler{reader: repo}
}

// requestTraceQueryParam 读取一个列表与导出共用的筛选关键字。
//
// present 表示调用方**显式给出了**这个键（包括 `?key=` 与 `?key=%20`），ok 表示它的值
// 去掉首尾空白后仍然可用。空白不是"没给"：把显式给出的空白当成缺省，会让管理员以为筛了
// 一项，列表却回答另一个问题、导出把"全部"写成一个文件。只有键真正缺失才是"不过滤"。
//
// 两个入口用同一个读取口径，筛选语义才不会各写一套而漂移。
func requestTraceQueryParam(query url.Values, key string) (value string, present, ok bool) {
	if _, present = query[key]; !present {
		return "", false, true
	}
	value = strings.TrimSpace(query.Get(key))
	return value, true, value != ""
}

// requestTraceRouteFamilyValid 与仓储层保持同一封闭枚举：未列出的 route_family
// 在入口即拒绝，而不是让仓储把它当成"非法记录"，更不是静默返回空结果。
func requestTraceRouteFamilyValid(family service.RequestTraceRouteFamily) bool {
	switch family {
	case service.RequestTraceMessages, service.RequestTraceChatCompletions, service.RequestTraceResponses:
		return true
	default:
		return false
	}
}

func (h *RequestTraceHandler) List(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if c.GetString("auth_method") != service.AuditAuthMethodJWT {
		response.ErrorFrom(c, errRequestTraceSessionRequired)
		return
	}
	if h == nil || h.reader == nil {
		response.ErrorFrom(c, errRequestTraceUnavailable)
		return
	}
	page, pageSize := response.ParsePagination(c)
	if pageSize > 100 {
		pageSize = 100
	}
	filter := service.RequestTraceListFilter{Page: page, PageSize: pageSize}
	query := c.Request.URL.Query()

	if raw, present, ok := requestTraceQueryParam(query, "trace_id"); present {
		if !ok || !requestTraceIDPattern.MatchString(raw) {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.TraceID = raw
	}
	if raw, present, ok := requestTraceQueryParam(query, "route_family"); present {
		family := service.RequestTraceRouteFamily(raw)
		if !ok || !requestTraceRouteFamilyValid(family) {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.RouteFamily = family
	}
	if raw, present, ok := requestTraceQueryParam(query, "client_status"); present {
		status, err := strconv.Atoi(raw)
		if !ok || err != nil || status < 0 || status > 599 {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.ClientStatus = &status
	}
	if raw, present, ok := requestTraceQueryParam(query, "usage_linked"); present {
		linked, err := strconv.ParseBool(raw)
		if !ok || err != nil {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.UsageLinked = &linked
	}
	fromGiven, toGiven := false, false
	for _, entry := range []struct {
		key   string
		given *bool
		out   *time.Time
	}{{"created_from", &fromGiven, &filter.CreatedFrom}, {"created_to", &toGiven, &filter.CreatedTo}} {
		raw, present, ok := requestTraceQueryParam(query, entry.key)
		if !present {
			continue
		}
		if !ok {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		*entry.out = parsed
		*entry.given = true
	}
	// 时间窗必须是严格正区间：上界等于或早于下界都不是有意义的筛选，导出已按此拒绝。
	// 列表必须给出同一个 400，而不是静默返回空集或全部，否则"导出当前查询全部"
	// 会与列表看到的范围不同。
	if fromGiven && toGiven && !filter.CreatedFrom.Before(filter.CreatedTo) {
		response.ErrorFrom(c, errRequestTraceInvalidFilter)
		return
	}
	// account_id / usage_log_id 是可选检索：出现时必须是正整数；非法值直接拒绝，
	// 不静默退化成"无筛选"，否则从使用记录跳转过来会看到全部 Trace。
	for _, entry := range []struct {
		key string
		out **int64
	}{{"usage_log_id", &filter.UsageLogID}, {"account_id", &filter.AccountID}} {
		raw, present, ok := requestTraceQueryParam(query, entry.key)
		if !present {
			continue
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if !ok || err != nil || id <= 0 {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		value := id
		*entry.out = &value
	}
	// 分组：具体 ID 与"未知"互斥；两者同时出现视为非法筛选而不是任意匹配。
	rawGroupID, groupIDGiven, groupIDOK := requestTraceQueryParam(query, "group_id")
	rawGroupUnknown, groupUnknownGiven, groupUnknownOK := requestTraceQueryParam(query, "group_unknown")
	if (groupIDGiven && !groupIDOK) || (groupUnknownGiven && !groupUnknownOK) || (groupIDGiven && groupUnknownGiven) {
		response.ErrorFrom(c, errRequestTraceInvalidFilter)
		return
	}
	if groupIDGiven {
		id, err := strconv.ParseInt(rawGroupID, 10, 64)
		if err != nil || id <= 0 {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		value := id
		filter.GroupID = &value
	} else if groupUnknownGiven {
		unknown, err := strconv.ParseBool(rawGroupUnknown)
		if err != nil {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.GroupUnknown = &unknown
	}
	// 客户端请求模型：具体名称与"未知"同样互斥。模型名只修剪首尾空白后原样使用，
	// " gpt-5 " 这类合法的带空格取值仍然可用。
	requestedModel, modelGiven, modelOK := requestTraceQueryParam(query, "requested_model")
	rawModelUnknown, modelUnknownGiven, modelUnknownOK := requestTraceQueryParam(query, "model_unknown")
	if (modelGiven && !modelOK) || (modelUnknownGiven && !modelUnknownOK) || (modelGiven && modelUnknownGiven) {
		response.ErrorFrom(c, errRequestTraceInvalidFilter)
		return
	}
	filter.RequestedModel = requestedModel
	if modelUnknownGiven {
		unknown, err := strconv.ParseBool(rawModelUnknown)
		if err != nil {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.ModelUnknown = &unknown
	}
	// 平台：按任一真实上游尝试选中的账号平台，"未知"表示没有任何平台事实。
	platform, platformGiven, platformOK := requestTraceQueryParam(query, "platform")
	rawPlatformUnknown, platformUnknownGiven, platformUnknownOK := requestTraceQueryParam(query, "platform_unknown")
	if (platformGiven && !platformOK) || (platformUnknownGiven && !platformUnknownOK) || (platformGiven && platformUnknownGiven) {
		response.ErrorFrom(c, errRequestTraceInvalidFilter)
		return
	}
	filter.Platform = platform
	if platformUnknownGiven {
		unknown, err := strconv.ParseBool(rawPlatformUnknown)
		if err != nil {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.PlatformUnknown = &unknown
	}
	for _, identity := range []struct {
		idKey      string
		unknownKey string
		id         **int64
		unknown    **bool
	}{
		{"user_id", "user_unknown", &filter.UserID, &filter.UserUnknown},
		{"api_key_id", "api_key_unknown", &filter.APIKeyID, &filter.APIKeyUnknown},
	} {
		value, present, ok := requestTraceQueryParam(query, identity.idKey)
		unknownValue, unknownPresent, unknownOK := requestTraceQueryParam(query, identity.unknownKey)
		if (present && !ok) || (unknownPresent && !unknownOK) || (present && unknownPresent) {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		if present {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 {
				response.ErrorFrom(c, errRequestTraceInvalidFilter)
				return
			}
			*identity.id = &id
		}
		if unknownPresent {
			unknown, err := strconv.ParseBool(unknownValue)
			if err != nil {
				response.ErrorFrom(c, errRequestTraceInvalidFilter)
				return
			}
			*identity.unknown = &unknown
		}
	}
	if value, present, ok := requestTraceQueryParam(query, "q"); present {
		if !ok || !service.ValidRequestTraceKeyword(value) {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.Keyword = value
	}
	if page < 1 || page > 100000 || pageSize <= 0 {
		response.ErrorFrom(c, errRequestTraceInvalidFilter)
		return
	}
	includeStats := false
	if raw, present, ok := requestTraceQueryParam(query, "include_stats"); present {
		parsed, err := strconv.ParseBool(raw)
		if !ok || err != nil || !parsed {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		includeStats = true
	}
	filter.SkipCount = includeStats
	items, count, err := h.reader.ListRequestTraces(c.Request.Context(), filter)
	if err != nil {
		if errors.Is(err, service.ErrRequestTraceInvalidRecord) {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
		} else {
			response.ErrorFrom(c, errRequestTraceUnavailable)
		}
		return
	}
	if includeStats {
		reader, hasStats := h.reader.(interface {
			RequestTraceQueryStats(context.Context, service.RequestTraceListFilter) (service.RequestTraceQueryStats, error)
		})
		if !hasStats {
			response.ErrorFrom(c, errRequestTraceUnavailable)
			return
		}
		stats, err := reader.RequestTraceQueryStats(c.Request.Context(), filter)
		if err != nil {
			response.ErrorFrom(c, errRequestTraceUnavailable)
			return
		}
		// Use the aggregate as the one displayed denominator for both the cards and
		// pagination. Concurrent writes may still change the page between reads;
		// neither result claims to be a fixed-time snapshot.
		count = stats.MatchedTotal
		pages := int((count + int64(pageSize) - 1) / int64(pageSize))
		if pages < 1 {
			pages = 1
		}
		response.Success(c, gin.H{"items": items, "total": count, "page": page, "page_size": pageSize, "pages": pages, "stats": stats})
		return
	}
	response.Paginated(c, items, count, page, pageSize)
}

type requestTraceStageView struct {
	Ordinal             int                                `json:"ordinal"`
	Stage               string                             `json:"stage"`
	AttemptIndex        int                                `json:"attempt_index"`
	ViewName            string                             `json:"view_name"`
	State               service.RequestTraceCaptureState   `json:"state"`
	Reason              string                             `json:"reason"`
	ObservedBytes       int64                              `json:"observed_bytes"`
	RetainedBytes       int                                `json:"retained_bytes"`
	DroppedEvents       int                                `json:"dropped_events"`
	RedactionUnverified bool                               `json:"redaction_unverified"`
	PayloadText         *string                            `json:"payload_text,omitempty"`
	Facts               *service.RequestTraceStageFacts    `json:"facts,omitempty"`
	Decision            *service.RequestTraceDecisionFacts `json:"decision,omitempty"`
}

type requestTraceDetailView struct {
	service.RequestTrace
	Stages []requestTraceStageView `json:"stages"`
}

func (h *RequestTraceHandler) Get(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
	if c.GetString("auth_method") != service.AuditAuthMethodJWT {
		response.ErrorFrom(c, errRequestTraceSessionRequired)
		return
	}
	if h == nil || h.reader == nil {
		response.ErrorFrom(c, errRequestTraceUnavailable)
		return
	}
	id := strings.TrimSpace(c.Param("trace_id"))
	if !requestTraceIDPattern.MatchString(id) {
		response.ErrorFrom(c, errRequestTraceNotFound)
		return
	}
	detail, err := h.reader.GetRequestTrace(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, errRequestTraceUnavailable)
		return
	}
	if detail == nil {
		response.ErrorFrom(c, errRequestTraceNotFound)
		return
	}
	view := requestTraceDetailView{RequestTrace: detail.RequestTrace, Stages: make([]requestTraceStageView, 0, len(detail.Stages))}
	for _, stage := range detail.Stages {
		item := requestTraceStageView{
			Ordinal: stage.Ordinal, Stage: stage.Stage, AttemptIndex: stage.AttemptIndex, ViewName: stage.View,
			State: stage.State, Reason: stage.Reason, ObservedBytes: stage.ObservedBytes,
			RetainedBytes: stage.RetainedBytes, DroppedEvents: stage.DroppedEvents, RedactionUnverified: stage.RedactionUnverified,
		}
		if len(stage.Payload) > 0 && utf8.Valid(stage.Payload) {
			text := string(stage.Payload)
			item.PayloadText = &text
		}
		item.Facts = service.CloneRequestTraceStageFacts(stage.Stage, stage.Metadata)
		// The decision is the stage's other typed projection and is disclosed on the
		// same terms: only a body-less gateway_decision stage may carry one, and only
		// a validated closed-set value is copied out. A metadata map or a free-text
		// decision can therefore never reach this DTO.
		if service.ValidRequestTraceDecisionStage(stage) {
			item.Decision = service.CloneRequestTraceDecisionFacts(stage.Decision)
		}
		view.Stages = append(view.Stages, item)
	}
	response.Success(c, view)
}
