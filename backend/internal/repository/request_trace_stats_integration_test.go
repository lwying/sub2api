//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceStatsFollowSameQueryAsListWithoutCountingOnlyCurrentPage(t *testing.T) {
	ctx := context.Background()
	tx := newRequestTraceTestTx(t)
	repo := &requestTraceRepository{q: tx}
	for _, item := range []struct {
		id     string
		status int
		state  service.RequestTraceCaptureState
	}{
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 200, service.RequestTraceStored},
		{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 503, service.RequestTracePartial},
		{"cccccccccccccccccccccccccccccccc", 0, service.RequestTraceNotObserved},
	} {
		_, err := repo.CreateRequestTrace(ctx, service.RequestTrace{TraceID: item.id, RouteFamily: service.RequestTraceMessages,
			InboundEndpoint: "/v1/messages", ClientStatus: item.status, CaptureState: item.state})
		require.NoError(t, err)
	}
	filter := service.RequestTraceListFilter{RouteFamily: service.RequestTraceMessages, Page: 1, PageSize: 1}
	rows, total, err := repo.ListRequestTraces(ctx, filter)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(3), total)
	stats, err := repo.RequestTraceQueryStats(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, total, stats.MatchedTotal)
	require.Equal(t, int64(1), stats.Status.OK)
	require.Equal(t, int64(1), stats.Status.Server)
	require.Equal(t, int64(1), stats.Status.Other)
	require.Equal(t, int64(1), stats.Capture.Stored)
	require.Equal(t, int64(1), stats.Capture.Partial)
	require.Equal(t, int64(1), stats.Capture.NotObserved)
	require.Equal(t, int64(3), stats.Usage.Unlinked)
	withoutCount := filter
	withoutCount.SkipCount = true
	rows, skippedTotal, err := repo.ListRequestTraces(ctx, withoutCount)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Zero(t, skippedTotal)
	invalid := filter
	invalid.AccountID = new(int64)
	_, _, err = repo.ListRequestTraces(ctx, invalid)
	require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
	_, err = repo.RequestTraceQueryStats(ctx, invalid)
	require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
}
