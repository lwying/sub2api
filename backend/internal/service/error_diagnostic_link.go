package service

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// ErrorDiagnosticLinkDigest is scoped to one authenticated user's logical request.
// The client-supplied correlation ID is never stored in the diagnostic row.
func ErrorDiagnosticLinkDigest(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	if len(requestID) == 0 || len(requestID) > 64 || strings.TrimSpace(requestID) == "" {
		return ""
	}
	for _, char := range requestID {
		if unicode.IsControl(char) {
			return ""
		}
	}
	return RequestAuditFingerprintFromContext(ctx).DigestIdentifier("local_request", requestID)
}

// ErrorDiagnosticUsageAttacher is implemented only by stores that can verify
// the request fingerprint and each wire attempt against an actual usage audit.
// No guessed association is made when a fingerprint or real attempt is missing.
type ErrorDiagnosticUsageAttacher interface {
	LinkPlainErrorDiagnostics(ctx context.Context, usageLogID int64, linkDigest string, now time.Time) (int64, error)
}

// AttachErrorDiagnosticsAfterUsage can be called after usage and its audit have
// been written. A later diagnostic insert is repaired by the bounded reconciler.
func AttachErrorDiagnosticsAfterUsage(ctx context.Context, store ErrorDiagnosticUsageAttacher, usageLogID int64, now time.Time) (int64, error) {
	if store == nil || usageLogID <= 0 {
		return 0, nil
	}
	digest := ErrorDiagnosticLinkDigest(ctx)
	if digest == "" {
		return 0, nil
	}
	return store.LinkPlainErrorDiagnostics(ctx, usageLogID, digest, now)
}
