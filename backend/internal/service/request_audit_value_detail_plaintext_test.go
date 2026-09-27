//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValueDetailPlaintextFollowsUsageWithoutEncryptionKey(t *testing.T) {
	in := valueDetailScopeInput()
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	write := BuildRequestAuditValueDetailWrite(in, RequestAuditValueDetailGate{CaptureAllowed: true}, now)
	require.NotNil(t, write)
	require.True(t, write.Retained())
	require.Equal(t, RequestAuditValueDetailStateStored, write.State)
	require.Equal(t, RequestAuditValueDetailRetained, write.Reason)
	require.True(t, write.ExpiresAt.IsZero(), "new plaintext values must have no independent expiry")
	values, err := DecodeRequestAuditValueDetailValues(write.Payload)
	require.NoError(t, err)
	require.Equal(t, in.Model, values.Model)
}
