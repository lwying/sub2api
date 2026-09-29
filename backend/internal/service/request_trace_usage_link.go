package service

import (
	"context"
	"strings"
)

type requestTraceIDContextKey struct{}

// WithRequestTraceID carries a server-generated Trace identity across detached
// usage billing contexts; it must never be populated from a client request ID.
func WithRequestTraceID(ctx context.Context, traceID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(traceID) != 32 {
		return ctx
	}
	for _, char := range traceID {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return ctx
		}
	}
	return context.WithValue(ctx, requestTraceIDContextKey{}, traceID)
}

func RequestTraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(requestTraceIDContextKey{}).(string)
	if len(value) != 32 || strings.Trim(value, "0123456789abcdef") != "" {
		return ""
	}
	return value
}

// RequestTraceUsageLinker is the narrow verified ownership writer. A caller may
// invoke it only after the usage write reports an actual new INSERT, not after an
// existing row was found by deduplication or a best-effort recent-key cache.
type RequestTraceUsageLinker interface {
	LinkRequestTraceUsage(ctx context.Context, traceID string, usageLogID int64) (bool, error)
}
