package handler

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type requestTraceIDContextKey struct{}

// RequestTraceGateForCapture 构造只带采集结论的门控，范围按"全部分组/所有模型/所有平台"。
// 测试与只关心开关的调用方用它表达"开关打开"，不必重复拼范围字段。
func RequestTraceGateForCapture(allowed bool) service.RequestTraceGate {
	if !allowed {
		return service.RequestTraceGate{}
	}
	return service.RequestTraceGate{
		CaptureAllowed: true,
		Scope: service.RequestTraceSettings{
			Enabled: true, RiskAcknowledged: true,
			AllGroups:     true,
			ModelScope:    service.RequestTraceScopeAll,
			PlatformScope: service.RequestTraceScopeAll,
		},
	}
}

func RequestTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestTraceIDContextKey{}).(string)
	return id
}

// RequestTraceCaptureMiddleware observes only selected, opted-in HTTP requests.
// Authentication rejects still get a Trace but are never made to read a body.
//
// resolveGate 返回门控与采集范围快照。范围在请求结束时按实际观察到的事实复核，
// 因此无法在入口处一次判定；这里只决定"是否可能采集"。
func RequestTraceCaptureMiddleware(resolveGate func(context.Context) service.RequestTraceGate, repo service.RequestTraceRepository, queues ...*service.RequestTraceCaptureQueue) gin.HandlerFunc {
	var queue *service.RequestTraceCaptureQueue
	if len(queues) > 0 {
		queue = queues[0]
	}
	return RequestTraceMiddleware(func(c *gin.Context) bool {
		if c.Request == nil || resolveGate == nil || repo == nil || queue == nil {
			return false
		}
		_, _, supported := requestTraceRoute(c.Request.Method, c.Request.URL.Path)
		return supported && resolveGate(c.Request.Context()).CaptureAllowed
	}, func(c *gin.Context, id string, _ bool) {
		requestTraceGate := service.RequestTraceGate{}
		if resolveGate != nil && c.Request != nil {
			requestTraceGate = resolveGate(c.Request.Context())
		}
		family, endpoint, ok := requestTraceRoute(c.Request.Method, c.Request.URL.Path)
		if !ok {
			return
		}
		completed := time.Now().UTC()
		apiKey, authenticated := middleware.GetAPIKeyFromContext(c)
		state := service.RequestTraceNotObserved
		if authenticated {
			state = service.RequestTracePartial
		}
		groupID, requestedModel, platforms := requestTraceScopeFacts(c, apiKey)
		// 采集范围在请求结束时按**实际观察到的事实**复核：鉴权前无法确定分组，
		// 模型要等请求体解析，平台要等选号。范围不匹配就不落库，避免留下不该采的明文。
		if !requestTraceGate.Scope.InScope(service.RequestTraceScopeFacts{
			GroupID: groupID, RequestedModel: requestedModel, Platforms: platforms,
		}) {
			return
		}
		trace := service.RequestTrace{
			TraceID: id, RouteFamily: family, InboundEndpoint: endpoint,
			CaptureState: state, ClientStatus: c.Writer.Status(),
			CreatedAt: completed, CompletedAt: &completed,
			GroupID: groupID, RequestedModel: requestedModel,
		}
		reason := "body_not_observed"
		if !authenticated {
			reason = "auth_rejected_body_not_observed"
		}
		stages := []service.RequestTraceStage{{TraceID: id, Ordinal: 1, Stage: "client_entry", State: service.RequestTraceNotObserved, Reason: reason}}
		if value, exists := c.Get("request_trace_flow"); exists {
			if flow, ok := value.(*requestTraceFlow); ok {
				if rejected, exists := middleware.GetIngressRejectReason(c); exists {
					flow.setRejectReason(string(rejected))
				}
				outcome := service.RequestTraceDecisionRejected
				if authenticated {
					outcome = service.RequestTraceDecisionAccepted
				}
				flow.recordDecision(0, "auth_"+string(outcome), service.RequestTraceDecisionFacts{
					Decision: service.RequestTraceDecisionAuth, Outcome: outcome,
					Source: service.RequestTraceDecisionSourceAPIKey,
				})
				stages = flow.finish(id, authenticated)
			}
		}
		if authenticated && len(stages) > 0 {
			stored := false
			for _, stage := range stages {
				if stage.State == service.RequestTraceStored {
					stored = true
				}
				if stage.State == service.RequestTraceUnsupported || stage.State == service.RequestTraceWriteFailed || stage.State == service.RequestTraceTruncated || stage.State == service.RequestTraceUnverified || (stage.State == service.RequestTraceNotObserved && stage.Reason != "wire_observed" && stage.Reason != "metadata_observed" && stage.Stage != service.RequestTraceDecisionStage) {
					stored = false
					break
				}
			}
			if stored {
				trace.CaptureState = service.RequestTraceStored
			}
		}
		queue.Enqueue(trace, stages)
	})
}

func requestTraceRoute(method, path string) (service.RequestTraceRouteFamily, string, bool) {
	return ClassifyRequestTraceRoute(method, path)
}

// markRequestTraceSelectedPlatform 记录一次实际选中的上游账号平台，
// 供请求结束时复核 Trace 采集范围（首次可确定的平台决定整条结论）。
func markRequestTraceSelectedPlatform(c *gin.Context, platform string) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(service.WithRequestTraceSelectedPlatform(c.Request.Context(), platform))
	// 同时写入采集流程：平台要作为每条 wire_attempt 的请求时事实落库。
	if value, exists := c.Get("request_trace_flow"); exists {
		if flow, ok := value.(*requestTraceFlow); ok {
			flow.setSelectedPlatform(platform)
		}
	}
}

// markRequestTraceRequestedModel 记录客户端请求的模型名，供采集范围按客户端模型复核。
func markRequestTraceRequestedModel(c *gin.Context, model string) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(service.WithRequestTraceRequestedModel(c.Request.Context(), model))
}

// requestTraceScopeFacts 汇总本次请求**实际观察到**的范围事实：
// 下游 API Key 的分组（鉴权成功才有）、客户端请求模型与选中过的上游账号平台。
// 无法观察到的事实保持零值，交由采集范围判定按"不确定"处理，绝不猜测。
func requestTraceScopeFacts(c *gin.Context, apiKey *service.APIKey) (*int64, string, []string) {
	var groupID *int64
	if apiKey != nil && apiKey.GroupID != nil {
		value := *apiKey.GroupID
		groupID = &value
	}
	if c == nil || c.Request == nil {
		return groupID, "", nil
	}
	model, platforms := service.RequestTraceScopeFactsFromContext(c.Request.Context())
	return groupID, model, platforms
}

// RequestTraceMiddleware gives each gated request a unique server-only ID. It does
// not read the body and never waits for Trace persistence on the response path.
func RequestTraceMiddleware(enabled func(*gin.Context) bool, finished func(*gin.Context, string, bool)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil || enabled == nil || !enabled(c) {
			c.Next()
			return
		}
		id := strings.ReplaceAll(uuid.NewString(), "-", "")
		ctx := service.WithRequestTraceID(c.Request.Context(), id)
		c.Request = c.Request.WithContext(context.WithValue(ctx, requestTraceIDContextKey{}, id))
		flow := newRequestTraceFlow()
		flow.inboundFacts(c.Request.Method, c.Request.URL, c.Request.Header)
		if _, endpoint, supported := requestTraceRoute(c.Request.Method, c.Request.URL.Path); supported && endpoint != "" {
			flow.recordDecision(0, "route_selected", service.RequestTraceDecisionFacts{
				Decision: service.RequestTraceDecisionRoute,
				Outcome:  service.RequestTraceDecisionSelected,
				Source:   service.RequestTraceDecisionSourceInbound,
			})
		}
		c.Set("request_trace_flow", flow)
		originalWriter := c.Writer
		writer := NewRequestTraceWriter(originalWriter, func(chunk []byte, status int) {
			flow.setClientStream(strings.HasPrefix(strings.ToLower(originalWriter.Header().Get("Content-Type")), "text/event-stream"))
			flow.downstreamChunk(chunk, status)
		})
		if writer != nil {
			c.Writer = writer
		}
		c.Next()
		flow.setClientStream(strings.HasPrefix(strings.ToLower(c.Writer.Header().Get("Content-Type")), "text/event-stream"))
		if concrete, ok := writer.(*requestTraceWriter); ok && concrete.Hijacked() {
			flow.markHijacked()
		}
		flow.setClientWrittenComplete(c.Request.Context().Err() == nil)
		if writer != nil && c.Writer == writer {
			c.Writer = originalWriter
		}
		if finished != nil {
			finished(c, id, false)
		}
	}
}

// BindRequestTraceAfterAuth is placed after successful API-key authentication but
// before any allowlist/composite middleware that can consume or rewrite the body.
func BindRequestTraceAfterAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request != nil && service.RequestTraceIDFromContext(c.Request.Context()) != "" {
			if _, authenticated := middleware.GetAPIKeyFromContext(c); authenticated {
				if value, exists := c.Get("request_trace_flow"); exists {
					if flow, ok := value.(*requestTraceFlow); ok {
						ctx := httputil.WithInboundBodyObserver(c.Request.Context(), flow.inboundBody)
						ctx = httpattempt.WithTraceObserver(ctx, flow.traceObserver())
						ctx = service.WithRequestTraceIdentityVerdictObserver(ctx, flow.identityVerdict)
						c.Request = c.Request.WithContext(ctx)
					}
				}
			}
		}
		c.Next()
	}
}
