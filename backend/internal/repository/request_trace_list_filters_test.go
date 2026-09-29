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
			require.Len(t, stub.args[0], 8)

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
