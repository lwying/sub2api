package admin

import (
	"context"
	"errors"
	"net/http"
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
	filter := service.RequestTraceListFilter{Page: page, PageSize: pageSize, TraceID: c.Query("trace_id"), RouteFamily: service.RequestTraceRouteFamily(c.Query("route_family"))}
	if filter.TraceID != "" && !requestTraceIDPattern.MatchString(filter.TraceID) {
		response.ErrorFrom(c, errRequestTraceInvalidFilter)
		return
	}
	if raw := c.Query("client_status"); raw != "" {
		status, err := strconv.Atoi(raw)
		if err != nil || status < 0 || status > 599 {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.ClientStatus = &status
	}
	for _, entry := range []struct {
		key string
		out *time.Time
	}{{"created_from", &filter.CreatedFrom}, {"created_to", &filter.CreatedTo}} {
		if raw := c.Query(entry.key); raw != "" {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				response.ErrorFrom(c, errRequestTraceInvalidFilter)
				return
			}
			*entry.out = parsed
		}
	}
	if raw := c.Query("usage_linked"); raw != "" {
		linked, err := strconv.ParseBool(raw)
		if err != nil {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		filter.UsageLinked = &linked
	}
	// account_id / usage_log_id 是可选检索：出现时必须是正整数；非法值直接拒绝，
	// 不静默退化成"无筛选"，否则从使用记录跳转过来会看到全部 Trace。
	for _, entry := range []struct {
		key string
		out **int64
	}{{"usage_log_id", &filter.UsageLogID}, {"account_id", &filter.AccountID}} {
		raw := c.Query(entry.key)
		if raw == "" {
			continue
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
			return
		}
		value := id
		*entry.out = &value
	}
	if page < 1 || page > 100000 || pageSize <= 0 {
		response.ErrorFrom(c, errRequestTraceInvalidFilter)
		return
	}
	items, count, err := h.reader.ListRequestTraces(c.Request.Context(), filter)
	if err != nil {
		if errors.Is(err, service.ErrRequestTraceInvalidRecord) {
			response.ErrorFrom(c, errRequestTraceInvalidFilter)
		} else {
			response.ErrorFrom(c, errRequestTraceUnavailable)
		}
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
