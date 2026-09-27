//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestErrorDiagnosticLinkDigestIsRequestAndUserScoped(t *testing.T) {
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("f", 32))
	require.NoError(t, err)
	fp, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)
	ctx := WithRequestAuditFingerprint(context.WithValue(context.Background(), ctxkey.RequestID, "local-123"), fp)
	require.Equal(t, fp.DigestIdentifier("local_request", "local-123"), ErrorDiagnosticLinkDigest(ctx))
	require.Empty(t, ErrorDiagnosticLinkDigest(context.WithValue(context.Background(), ctxkey.RequestID, "local-123")))
	require.Empty(t, ErrorDiagnosticLinkDigest(WithRequestAuditFingerprint(context.WithValue(context.Background(), ctxkey.RequestID, "bad\nrequest"), fp)))
	require.Empty(t, ErrorDiagnosticLinkDigest(WithRequestAuditFingerprint(context.WithValue(context.Background(), ctxkey.RequestID, strings.Repeat("a", 65)), fp)))
	require.Empty(t, ErrorDiagnosticLinkDigest(WithRequestAuditFingerprint(context.WithValue(context.Background(), ctxkey.RequestID, "badrequest"), fp)),
		"C1 control characters must match the audit metadata filter")
}

type diagnosticUsageAttacherStub struct {
	calls  int
	usage  int64
	digest string
}

func (s *diagnosticUsageAttacherStub) LinkPlainErrorDiagnostics(_ context.Context, usageID int64, digest string, _ time.Time) (int64, error) {
	s.calls++
	s.usage = usageID
	s.digest = digest
	return 2, nil
}
func TestAttachErrorDiagnosticsAfterUsageRequiresAuditableCorrelation(t *testing.T) {
	stub := &diagnosticUsageAttacherStub{}
	_, err := AttachErrorDiagnosticsAfterUsage(context.Background(), stub, 15, time.Now())
	require.NoError(t, err)
	require.Zero(t, stub.calls)
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("f", 32))
	require.NoError(t, err)
	fp, err := fingerprinter.BeginForUser(7)
	require.NoError(t, err)
	ctx := WithRequestAuditFingerprint(context.WithValue(context.Background(), ctxkey.RequestID, "local-123"), fp)
	count, err := AttachErrorDiagnosticsAfterUsage(ctx, stub, 15, time.Now())
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	require.Equal(t, 1, stub.calls)
	require.Equal(t, fp.DigestIdentifier("local_request", "local-123"), stub.digest)
}
