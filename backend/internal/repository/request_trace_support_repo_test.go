//go:build unit

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceSupportRequiresItsOwnTableAndOwnership(t *testing.T) {
	probe := NewRequestTraceSupportProbe(nil)
	support, err := probe.ProbeRequestTraceSupport(context.Background())
	require.Error(t, err)
	require.False(t, support.Supported)
	require.NotEqual(t, service.PlaintextCaptureSupportReasonSupported, support.Reason)
}
