//go:build unit

package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceIdentitySurvivesUsageWorkerContext(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	parent := service.WithRequestTraceID(context.Background(), id)
	worker := usageRecordContext(parent, context.Background())
	require.Equal(t, id, service.RequestTraceIDFromContext(worker))
	require.Empty(t, service.RequestTraceIDFromContext(usageRecordContext(context.Background(), context.Background())))
}
