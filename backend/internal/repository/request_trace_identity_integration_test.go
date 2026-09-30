//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceRepositoryPreservesObservedIdentityAndHistoricalUnknown(t *testing.T) {
	ctx := context.Background()
	tx := newRequestTraceTestTx(t)
	repo := &requestTraceRepository{q: tx}
	userID, keyID := int64(23), int64(19)
	observed, err := repo.CreateRequestTrace(ctx, service.RequestTrace{
		TraceID: "12341234123412341234123412341234", RouteFamily: service.RequestTraceMessages,
		InboundEndpoint: "/v1/messages", UserID: &userID, APIKeyID: &keyID,
	})
	require.NoError(t, err)
	require.Equal(t, &userID, observed.UserID)
	require.Equal(t, &keyID, observed.APIKeyID)
	_, err = repo.CreateRequestTrace(ctx, service.RequestTrace{
		TraceID: "56785678567856785678567856785678", RouteFamily: service.RequestTraceMessages,
		InboundEndpoint: "/v1/messages",
	})
	require.NoError(t, err)
	unknown, err := repo.GetRequestTrace(ctx, "56785678567856785678567856785678")
	require.NoError(t, err)
	require.Nil(t, unknown.UserID)
	require.Nil(t, unknown.APIKeyID)
}
