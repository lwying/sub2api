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

func RequestTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestTraceIDContextKey{}).(string)
	return id
}

// RequestTraceCaptureMiddleware observes only selected, opted-in HTTP requests.
// Authentication rejects still get a Trace but are never made to read a body.
func RequestTraceCaptureMiddleware(enabled func(context.Context) bool, repo service.RequestTraceRepository, queues ...*service.RequestTraceCaptureQueue) gin.HandlerFunc {
	var queue *service.RequestTraceCaptureQueue
	if len(queues) > 0 {
		queue = queues[0]
	}
	return RequestTraceMiddleware(func(c *gin.Context) bool {
		if c.Request == nil || enabled == nil || repo == nil || queue == nil {
			return false
		}
		_, _, supported := requestTraceRoute(c.Request.Method, c.Request.URL.Path)
		return supported && enabled(c.Request.Context())
	}, func(c *gin.Context, id string, _ bool) {
		family, endpoint, ok := requestTraceRoute(c.Request.Method, c.Request.URL.Path)
		if !ok {
			return
		}
		completed := time.Now().UTC()
		_, authenticated := middleware.GetAPIKeyFromContext(c)
		state := service.RequestTraceNotObserved
		if authenticated {
			state = service.RequestTracePartial
		}
		trace := service.RequestTrace{
			TraceID: id, RouteFamily: family, InboundEndpoint: endpoint,
			CaptureState: state, ClientStatus: c.Writer.Status(),
			CreatedAt: completed, CompletedAt: &completed,
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
