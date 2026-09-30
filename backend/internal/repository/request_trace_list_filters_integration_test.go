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

// 平台筛选必须覆盖"账号已选中、但还没发出上游就失败"的请求（规格 §2.2/§2.3）：
// 这类错误 Trace 没有 wire_attempt 阶段，平台事实只存在信封的 observed_platforms 上，
// 所以筛选要取两处请求时事实的并集，而不是只看阶段。
//
// 同时固定四条边界：
//   - 任一处的平台都算"已观察"，不能被 platform_unknown 命中；
//   - 两处都没有平台事实才算未知；
//   - A→B 换号后两个平台都能检索到（多平台逻辑请求按任一命中平台出现）；
//   - 请求时事实按 Trace 自己记录，不联查账号当前值：删掉该 Trace 的 wire_attempt
//     阶段后，仅凭信封平台历史仍命中同一平台筛选，历史结果不被源记录变化重写。
func TestRequestTraceRepositoryPlatformFiltersUseRequestTimeFacts(t *testing.T) {
	ctx := context.Background()
	tx := newRequestTraceTestTx(t)
	repo := &requestTraceRepository{q: tx}

	const (
		selectedNotSent = "cccccccccccccccccccccccccccccccc"
		multiPlatform   = "dddddddddddddddddddddddddddddddd"
		stageOnly       = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		noPlatform      = "ffffffffffffffffffffffffffffffff"
	)

	create := func(id string, observed []string) {
		t.Helper()
		_, err := repo.CreateRequestTrace(ctx, service.RequestTrace{
			TraceID: id, RouteFamily: service.RequestTraceMessages, InboundEndpoint: "/v1/messages",
			ClientStatus: 429, ObservedPlatforms: observed,
		})
		require.NoError(t, err)
	}
	// 1) 选中 anthropic 后账号槽位等待超时，从未发出上游：只有信封平台历史。
	create(selectedNotSent, []string{service.PlatformAnthropic})
	// 2) A→B 换号：两个平台都是请求时事实。
	create(multiPlatform, []string{service.PlatformAnthropic, service.PlatformOpenAI})
	// 3) 270 之前的历史行：只有 wire_attempt 阶段平台，信封为 NULL。
	create(stageOnly, nil)
	_, err := repo.AppendRequestTraceStage(ctx, service.RequestTraceStage{
		TraceID: stageOnly, Ordinal: 1, Stage: "wire_attempt", State: service.RequestTraceNotObserved,
		Reason: "wire_observed", Metadata: &service.RequestTraceStageFacts{Method: "POST", Platform: service.PlatformGemini},
	})
	require.NoError(t, err)
	// 4) 鉴权前被拒：两处都没有平台事实 -> 未知。
	create(noPlatform, nil)

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
	unknown := true

	// 信封平台历史（含选中但未发出）与阶段平台都能命中具体平台。
	require.ElementsMatch(t, []string{selectedNotSent, multiPlatform},
		lookup(service.RequestTraceListFilter{Platform: service.PlatformAnthropic}))
	require.Equal(t, []string{multiPlatform},
		lookup(service.RequestTraceListFilter{Platform: service.PlatformOpenAI}))
	require.Equal(t, []string{stageOnly},
		lookup(service.RequestTraceListFilter{Platform: service.PlatformGemini}))
	require.Empty(t, lookup(service.RequestTraceListFilter{Platform: service.PlatformZhipu}))

	// 未知只匹配两处都没有平台事实的那条：已选中但未发出的平台是已知事实。
	require.Equal(t, []string{noPlatform},
		lookup(service.RequestTraceListFilter{PlatformUnknown: &unknown}))

	// 历史结果不被源记录变化重写：清掉 wire_attempt 阶段后，信封平台事实仍在，
	// 阶段平台事实随之消失——两者各自独立、都不联查账号当前值。
	_, err = tx.ExecContext(ctx, `DELETE FROM request_trace_stages`)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{selectedNotSent, multiPlatform},
		lookup(service.RequestTraceListFilter{Platform: service.PlatformAnthropic}))
	require.Empty(t, lookup(service.RequestTraceListFilter{Platform: service.PlatformGemini}))
	require.ElementsMatch(t, []string{noPlatform, stageOnly},
		lookup(service.RequestTraceListFilter{PlatformUnknown: &unknown}))
}
