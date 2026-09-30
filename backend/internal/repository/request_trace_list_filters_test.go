//go:build unit

package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// usage_log_id 与 account_id 都是可选筛选：nil 表示"不过滤"，非 nil 时才绑定。
// 它们必须作为绑定参数进入 SQL，且 account_id 只从 wire_attempt 阶段的事实里判定，
// 不能把整条元数据 JSONB 取回列表响应，也不能把筛选值格式化进语句。
func TestRequestTraceRepositoryBindsOptionalUsageAndAccountFilters(t *testing.T) {
	ctx := context.Background()
	usageID := int64(4242)
	accountID := int64(73)
	for _, tc := range []struct {
		name    string
		filter  service.RequestTraceListFilter
		present bool
	}{
		{name: "absent binds NULL", filter: service.RequestTraceListFilter{Page: 1, PageSize: 20}},
		{name: "present binds both values", present: true, filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, UsageLogID: &usageID, AccountID: &accountID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &recordingRequestTraceQueryer{}
			repo := &requestTraceRepository{q: stub}
			_, _, err := repo.ListRequestTraces(ctx, tc.filter)
			require.Error(t, err, "the stub returns no rows")
			require.Len(t, stub.args, 1)
			require.Len(t, stub.args[0], 19)

			boundUsage, usageOptional := stub.args[0][6].(*int64)
			require.True(t, usageOptional, "usage_log_id must be bound as an optional value")
			boundAccount, accountOptional := stub.args[0][7].(*int64)
			require.True(t, accountOptional, "account_id must be bound as an optional value")

			if tc.present {
				require.NotNil(t, boundUsage)
				require.Equal(t, usageID, *boundUsage)
				require.NotNil(t, boundAccount)
				require.Equal(t, accountID, *boundAccount)
			} else {
				require.Nil(t, boundUsage, "an absent usage filter must bind SQL NULL")
				require.Nil(t, boundAccount, "an absent account filter must bind SQL NULL")
			}

			query := stub.queries[0]
			require.Contains(t, query, "$7::bigint IS NULL OR usage_log_id = $7")
			// 账号筛选只匹配 wire_attempt 阶段的类型化事实，不匹配任意元数据。
			require.Contains(t, query, "s.stage = 'wire_attempt'")
			require.Contains(t, query, "s.metadata @> jsonb_build_object('account_id', $8::bigint)")
			// 绑定值绝不能出现在语句文本里。
			require.NotContains(t, query, "4242")
			require.NotContains(t, query, "73")
			require.False(t, strings.Contains(query, "SELECT s.metadata"), "the list must not select raw metadata")
		})
	}
}

// 分组与客户端模型筛选按"请求时事实"绑定：具体值与"未知"各自独立，
// 未知用 IS NULL 判定而不是伪装成某个具体值；两套条件都不把值格式化进语句。
func TestRequestTraceRepositoryBindsGroupAndModelScopeFilters(t *testing.T) {
	ctx := context.Background()
	groupID := int64(91)
	unknown := true
	for _, tc := range []struct {
		name   string
		filter service.RequestTraceListFilter
		// assert 在绑定参数上做本行独有的断言。
		assert func(t *testing.T, args []any, query string)
	}{
		{
			name:   "specific group and model",
			filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, GroupID: &groupID, RequestedModel: "Claude-Sonnet-4-5"},
			assert: func(t *testing.T, args []any, _ string) {
				boundGroup, ok := args[8].(*int64)
				require.True(t, ok)
				require.Equal(t, groupID, *boundGroup)
				boundModel, ok := args[10].(string)
				require.True(t, ok, "a present model filter must bind a value")
				require.Equal(t, "Claude-Sonnet-4-5", boundModel)
			},
		},
		{
			name:   "unknown group only",
			filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, GroupUnknown: &unknown},
			assert: func(t *testing.T, args []any, _ string) {
				require.Nil(t, args[8], "unknown group must not bind a concrete id")
				boundUnknown, ok := args[9].(bool)
				require.True(t, ok, "a present unknown flag must bind a boolean")
				require.True(t, boundUnknown)
			},
		},
		{
			name:   "specific platform",
			filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, Platform: "antigravity"},
			assert: func(t *testing.T, args []any, query string) {
				boundPlatform, ok := args[12].(string)
				require.True(t, ok, "a present platform filter must bind a value")
				require.Equal(t, "antigravity", *&boundPlatform)
				require.Contains(t, query, "s2.metadata @> jsonb_build_object('platform', $13::text)")
				// 并集：真实发出的尝试（wire_attempt）与信封上的实际选中平台历史都必须匹配，
				// 否则"选中但未发出"的错误 Trace 会漏检（规格 §2.2/§2.3）。
				require.Contains(t, query, "OR observed_platforms @> jsonb_build_array($13::text)")
				require.NotContains(t, query, "antigravity")
			},
		},
		{
			name:   "unknown platform",
			filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, PlatformUnknown: &unknown},
			assert: func(t *testing.T, args []any, query string) {
				require.Nil(t, args[12], "unknown platform must not bind a concrete value")
				boundUnknown, ok := args[13].(bool)
				require.True(t, ok, "a present platform-unknown flag must bind a boolean")
				require.True(t, boundUnknown)
				require.Contains(t, query, "s3.metadata ? 'platform'")
				// 未知必须两处都没有平台事实：没有带 platform 的 wire_attempt，且信封为 NULL。
				require.Contains(t, query, "AND observed_platforms IS NULL")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &recordingRequestTraceQueryer{}
			repo := &requestTraceRepository{q: stub}
			_, _, err := repo.ListRequestTraces(ctx, tc.filter)
			require.Error(t, err, "the stub returns no rows")
			require.Len(t, stub.args, 1)
			args := stub.args[0]
			require.Len(t, args, 19)

			query := stub.queries[0]
			require.Contains(t, query, "$9::bigint IS NULL OR group_id = $9")
			require.Contains(t, query, "$10::boolean IS NULL OR (group_id IS NULL) = $10")
			require.Contains(t, query, "lower(requested_model) = lower($11)")
			require.Contains(t, query, "$12::boolean IS NULL OR (requested_model IS NULL) = $12")
			require.Contains(t, query, "$13::text IS NULL OR (")
			require.Contains(t, query, "$14::boolean IS NULL OR ((NOT EXISTS (")
			require.NotContains(t, query, "91")
			require.NotContains(t, query, "Claude-Sonnet-4-5")
			tc.assert(t, args, query)
		})
	}
}

func TestRequestTraceListSkipsRedundantCountWhenAggregateSuppliesTotal(t *testing.T) {
	stub := &recordingRequestTraceQueryer{}
	repo := &requestTraceRepository{q: stub}
	_, _, err := repo.ListRequestTraces(context.Background(), service.RequestTraceListFilter{Page: 1, PageSize: 20, SkipCount: true})
	require.Error(t, err)
	require.Len(t, stub.queries, 1)
	require.NotContains(t, stub.queries[0], "COUNT(*)")
}

// 可选化不等于放宽校验：0 或负数是无效的 ID，必须在进 SQL 之前拒绝。
func TestRequestTraceRepositoryRejectsNonPositiveLookupIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter service.RequestTraceListFilter
	}{
		{name: "zero usage id", filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, UsageLogID: ptrInt64(0)}},
		{name: "negative usage id", filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, UsageLogID: ptrInt64(-1)}},
		{name: "zero account id", filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, AccountID: ptrInt64(0)}},
		{name: "negative account id", filter: service.RequestTraceListFilter{Page: 1, PageSize: 20, AccountID: ptrInt64(-7)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &requestTraceRepository{q: &recordingRequestTraceQueryer{}}
			_, _, err := repo.ListRequestTraces(context.Background(), tc.filter)
			require.ErrorIs(t, err, service.ErrRequestTraceInvalidRecord)
		})
	}
}

func ptrInt64(v int64) *int64 { return &v }
