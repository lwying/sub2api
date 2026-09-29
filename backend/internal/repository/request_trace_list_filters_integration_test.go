//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// The account filter runs against real JSONB, so its containment predicate and its
// stage restriction must be proven on PostgreSQL, not only against a recording stub.
// A decision that names the same account is not an upstream attempt and must not
// match; only a wire_attempt stage's typed fact is an account this Trace used.
func TestRequestTraceRepositoryAccountAndUsageFiltersAgainstRealPostgres(t *testing.T) {
	ctx := context.Background()
	tx := newRequestTraceTestTx(t)
	repo := &requestTraceRepository{q: tx}

	const (
		linkedTraceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		otherTraceID  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	// The lookup filters take optional ids, so these must be addressable values.
	wiredAccount := int64(73)
	otherAccount := int64(42)
	usageID := int64(9001)
	unused := int64(999)

	_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs (id) VALUES ($1)`, usageID)
	require.NoError(t, err)

	_, err = repo.CreateRequestTrace(ctx, service.RequestTrace{
		TraceID: linkedTraceID, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages", ClientStatus: 200,
	})
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE request_traces SET usage_log_id = $1 WHERE trace_id = $2`, usageID, linkedTraceID)
	require.NoError(t, err)
	_, err = repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{
		TraceID: linkedTraceID, Ordinal: 1, Stage: "wire_attempt", State: service.RequestTraceNotObserved, Reason: "wire_observed",
		Metadata: &service.RequestTraceStageFacts{Method: "POST", AccountID: wiredAccount, Protocol: "anthropic.messages", Status: 200},
	})
	require.NoError(t, err)

	_, err = repo.CreateRequestTrace(ctx, service.RequestTrace{
		TraceID: otherTraceID, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages", ClientStatus: 200,
	})
	require.NoError(t, err)
	// A decision naming the same account is not an attempt: it must not satisfy the account lookup.
	_, err = repo.AppendRequestTraceStage(ctx, service.NewRequestTraceGatewayDecisionStage(otherTraceID, 1, 1, "route_selected",
		&service.RequestTraceDecisionFacts{
			Decision: service.RequestTraceDecisionRoute, Outcome: service.RequestTraceDecisionSelected,
			Source: service.RequestTraceDecisionSourceAccount, Sequence: 1, AccountID: wiredAccount,
		}))
	require.NoError(t, err)
	_, err = repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{
		TraceID: otherTraceID, Ordinal: 2, Stage: "wire_attempt", State: service.RequestTraceNotObserved, Reason: "wire_observed",
		Metadata: &service.RequestTraceStageFacts{Method: "POST", AccountID: otherAccount, Protocol: "anthropic.messages", Status: 200},
	})
	require.NoError(t, err)

	lookup := func(filter service.RequestTraceListFilter) []string {
		t.Helper()
		filter.Page, filter.PageSize = 1, 10
		list, count, err := repo.ListRequestTraces(ctx, filter)
		require.NoError(t, err)
		require.Equal(t, int64(len(list)), count)
		ids := make([]string, 0, len(list))
		for _, trace := range list {
			ids = append(ids, trace.TraceID)
		}
		return ids
	}

	require.ElementsMatch(t, []string{linkedTraceID, otherTraceID}, lookup(service.RequestTraceListFilter{}))
	require.Equal(t, []string{linkedTraceID}, lookup(service.RequestTraceListFilter{UsageLogID: &usageID}))
	require.Equal(t, []string{linkedTraceID}, lookup(service.RequestTraceListFilter{AccountID: &wiredAccount}),
		"only the wire_attempt account fact may match, never a decision that names the account")
	require.Equal(t, []string{otherTraceID}, lookup(service.RequestTraceListFilter{AccountID: &otherAccount}))

	require.Empty(t, lookup(service.RequestTraceListFilter{AccountID: &unused}))
	require.Empty(t, lookup(service.RequestTraceListFilter{UsageLogID: &unused}))

	// Both lookups together still intersect: the linked Trace is the one that used the account.
	require.Equal(t, []string{linkedTraceID}, lookup(service.RequestTraceListFilter{UsageLogID: &usageID, AccountID: &wiredAccount}))
	require.Empty(t, lookup(service.RequestTraceListFilter{UsageLogID: &usageID, AccountID: &otherAccount}))
}
