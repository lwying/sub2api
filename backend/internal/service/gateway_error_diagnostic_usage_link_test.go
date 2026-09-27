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

func TestAttachErrorDiagnosticsAfterUsageBestEffortRestoresFingerprint(t *testing.T) {
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("f", 32))
	require.NoError(t, err)
	fp, err := fingerprinter.BeginForUser(42)
	require.NoError(t, err)

	store := &diagnosticUsageAttacherStub{}
	workerCtx := context.WithValue(context.Background(), ctxkey.RequestID, "request-42")
	attachErrorDiagnosticsAfterUsageBestEffort(workerCtx, store, 73, fp)
	require.Equal(t, 1, store.calls)
	require.EqualValues(t, 73, store.usage)
	require.Equal(t, fp.DigestIdentifier("local_request", "request-42"), store.digest)

	store.calls = 0
	attachErrorDiagnosticsAfterUsageBestEffort(workerCtx, store, 0, fp)
	require.Zero(t, store.calls)
	attachErrorDiagnosticsAfterUsageBestEffort(workerCtx, store, 73, nil)
	require.Zero(t, store.calls)
	attachErrorDiagnosticsAfterUsageBestEffort(context.Background(), store, 73, fp)
	require.Zero(t, store.calls)
}

func TestAttachErrorDiagnosticsAfterUsageBestEffortDoesNotBlockBilling(t *testing.T) {
	store := &diagnosticUsageAttacherErrorStub{}
	fingerprinter, err := NewRequestAuditFingerprinter(strings.Repeat("f", 32))
	require.NoError(t, err)
	fp, err := fingerprinter.BeginForUser(42)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "request-42")
	attachErrorDiagnosticsAfterUsageBestEffort(ctx, store, 73, fp)
	require.Equal(t, 1, store.calls)
}

type diagnosticUsageAttacherErrorStub struct{ calls int }

func (s *diagnosticUsageAttacherErrorStub) LinkPlainErrorDiagnostics(context.Context, int64, string, time.Time) (int64, error) {
	s.calls++
	return 0, context.DeadlineExceeded
}
