package service

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
)

const requestAuditHTTPAttemptCounterKey = "request_audit_http_attempt_counter"

func requestAuditHTTPAttemptCounter(c *gin.Context) *httpattempt.Counter {
	if c == nil {
		return nil
	}
	if value, ok := c.Get(requestAuditHTTPAttemptCounterKey); ok {
		if counter, ok := value.(*httpattempt.Counter); ok {
			return counter
		}
	}
	counter := httpattempt.NewCounter()
	c.Set(requestAuditHTTPAttemptCounterKey, counter)
	return counter
}

func RequestAuditHTTPAttemptCount(c *gin.Context) uint64 {
	counter := requestAuditHTTPAttemptCounter(c)
	if counter == nil {
		return 0
	}
	return counter.Load()
}

func RequestAuditLastTransportWasPlugin(c *gin.Context) bool {
	counter := requestAuditHTTPAttemptCounter(c)
	return counter != nil && counter.LastPluginHandled()
}

func RequestAuditHTTPAttemptMetadata(c *gin.Context) []httpattempt.Metadata {
	counter := requestAuditHTTPAttemptCounter(c)
	if counter == nil {
		return nil
	}
	return counter.Metadata()
}

func RequestAuditAttemptsFromHTTPMetadata(metadata []httpattempt.Metadata, fp *RequestAuditFingerprintInput) []RequestAuditAttempt {
	if len(metadata) == 0 {
		return nil
	}
	attempts := make([]RequestAuditAttempt, 0, len(metadata))
	for _, item := range metadata {
		attempts = append(attempts, SanitizeRequestAuditAttempt(RequestAuditAttempt{
			AccountID:               item.AccountID,
			ModelFingerprint:        fp.DigestModel(item.Model),
			Protocol:                item.Protocol,
			Stage:                   RequestAuditStageWire,
			WireRequestHeaders:      httpattempt.SanitizeRequestHeaders(httpattempt.HeaderFromSanitizedMap(item.RequestHeaders)),
			UpstreamResponseHeaders: httpattempt.SanitizeResponseHeaders(httpattempt.ResponseHeaderFromSanitizedMap(item.ResponseHeaders)),
			UpstreamStatus:          cloneAuditInt(item.StatusCode),
			RequestPayloadBytes:     cloneAuditInt64(item.RequestBytes),
			ResponsePayloadBytes:    cloneAuditInt64(item.ResponseBytes),
			ResponseReadComplete:    cloneAuditBool(item.ResponseReadComplete),
		}))
	}
	return attempts
}

func cloneAuditInt(value *int) *int {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneAuditInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneAuditBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func WithRequestAuditHTTPAttemptCounter(ctx context.Context, c *gin.Context) context.Context {
	counter := requestAuditHTTPAttemptCounter(c)
	if counter == nil {
		return ctx
	}
	return httpattempt.WithCounter(ctx, counter)
}

func bindRequestAuditHTTPAttemptCounter(req *http.Request, c *gin.Context) *http.Request {
	if req == nil {
		return nil
	}
	counter := requestAuditHTTPAttemptCounter(c)
	if counter == nil {
		return req
	}
	return req.WithContext(httpattempt.WithCounter(req.Context(), counter))
}

func bindRequestAuditHTTPAttempt(
	req *http.Request,
	c *gin.Context,
	accountID int64,
	model string,
	protocol string,
) *http.Request {
	req = bindRequestAuditHTTPAttemptCounter(req, c)
	if req == nil {
		return nil
	}
	metadata := httpattempt.Metadata{AccountID: accountID, Model: model, Protocol: protocol}
	return req.WithContext(httpattempt.WithMetadata(req.Context(), metadata))
}
