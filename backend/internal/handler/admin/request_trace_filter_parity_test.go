//go:build unit

package admin

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// requestTraceSharedFilterKeys 是列表与导出共用的全部筛选键。
// 两边的解析必须逐键一致，否则"导出当前查询全部"就不等于列表看到的同一批记录。
var requestTraceSharedFilterKeys = []string{
	"trace_id", "route_family", "client_status", "usage_linked", "created_from", "created_to",
	"usage_log_id", "account_id", "group_id", "group_unknown", "requested_model", "model_unknown",
	"platform", "platform_unknown", "user_id", "user_unknown", "api_key_id", "api_key_unknown", "q",
}

// 显式给出但为空白（`?key=`、`?key=%20` 或 `?key=++`）的筛选必须 400，不能被当成"没给"：
// 当成"没给"会让管理员以为筛了一项，列表却回答另一个问题、导出把"全部"写成文件。
// 关键字缺失才是"不过滤"，空白不是。
func TestRequestTraceAdminListRejectsBlankFilterValues(t *testing.T) {
	for _, key := range requestTraceSharedFilterKeys {
		for _, value := range []string{"", "%20", "++"} {
			query := key + "=" + value
			t.Run(query, func(t *testing.T) {
				recorder, _, calls := listTracesWithQuery(t, query)
				require.Equal(t, http.StatusBadRequest, recorder.Code)
				require.Zero(t, calls, "a blank filter must never reach the reader as 'no filter'")
				require.Contains(t, recorder.Body.String(), "REQUEST_TRACE_INVALID_FILTER")
			})
		}
	}
}

// 时间窗必须是严格正区间：上界等于下界或早于下界都是无意义的筛选，
// 导出当前拒绝它们，列表必须同样拒绝，而不是静默返回空集或全部。
func TestRequestTraceAdminListRejectsEqualOrInvertedCreatedWindow(t *testing.T) {
	const base = "2026-01-01T00:00:00Z"
	const later = "2026-02-01T00:00:00Z"

	for name, query := range map[string]string{
		"equal bounds":    "created_from=" + base + "&created_to=" + base,
		"inverted bounds": "created_from=" + later + "&created_to=" + base,
	} {
		t.Run(name, func(t *testing.T) {
			recorder, _, calls := listTracesWithQuery(t, query)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Zero(t, calls, "a closed or inverted window must never reach the reader")
			require.Contains(t, recorder.Body.String(), "REQUEST_TRACE_INVALID_FILTER")
		})
	}

	// 合法区间仍然透传两个边界。
	recorder, filter, calls := listTracesWithQuery(t, "created_from="+base+"&created_to="+later)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, calls)
	wantFrom, err := time.Parse(time.RFC3339Nano, base)
	require.NoError(t, err)
	wantTo, err := time.Parse(time.RFC3339Nano, later)
	require.NoError(t, err)
	require.True(t, filter.CreatedFrom.Equal(wantFrom))
	require.True(t, filter.CreatedTo.Equal(wantTo))

	// 单边出现时不做区间比较：它只是"从某时刻起"，不是闭合窗口。
	recorder, filter, calls = listTracesWithQuery(t, "created_from="+later)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, calls)
	require.False(t, filter.CreatedFrom.IsZero())
	require.True(t, filter.CreatedTo.IsZero())
}

// 合法的带空白输入只去首尾空白后按原义使用：模型名/平台名的映射不受影响，
// 数字与布尔量也要与导出一致地接受同样的补齐形式。
func TestRequestTraceAdminListTrimsFormattedFilterValues(t *testing.T) {
	recorder, filter, calls := listTracesWithQuery(t,
		"requested_model=%20gpt-5%20&platform=%20anthropic%20&group_id=%207%20&client_status=%20200%20&usage_linked=%20true%20&usage_log_id=%204242%20")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, calls)
	require.Equal(t, "gpt-5", filter.RequestedModel)
	require.Equal(t, "anthropic", filter.Platform)
	require.NotNil(t, filter.GroupID)
	require.Equal(t, int64(7), *filter.GroupID)
	require.NotNil(t, filter.ClientStatus)
	require.Equal(t, 200, *filter.ClientStatus)
	require.NotNil(t, filter.UsageLinked)
	require.True(t, *filter.UsageLinked)
	require.NotNil(t, filter.UsageLogID)
	require.Equal(t, int64(4242), *filter.UsageLogID)
}

// 具体值与"未知"互斥：同时给出必须 400，而不是任意匹配或悄悄丢掉一边。
// 单边（含显式的 false）仍是合法筛选。
func TestRequestTraceAdminListRejectsConcreteAndUnknownTogether(t *testing.T) {
	for _, query := range []string{
		"group_id=7&group_unknown=true",
		"requested_model=gpt-5&model_unknown=true",
		"platform=anthropic&platform_unknown=true",
	} {
		t.Run(query, func(t *testing.T) {
			recorder, _, calls := listTracesWithQuery(t, query)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Zero(t, calls)
		})
	}

	recorder, filter, calls := listTracesWithQuery(t, "group_id=7&model_unknown=true&platform_unknown=false")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, calls)
	require.NotNil(t, filter.GroupID)
	require.Equal(t, int64(7), *filter.GroupID)
	require.Nil(t, filter.GroupUnknown)
	require.NotNil(t, filter.ModelUnknown)
	require.True(t, *filter.ModelUnknown)
	require.NotNil(t, filter.PlatformUnknown, "an explicit false still states the platform is known")
	require.False(t, *filter.PlatformUnknown)
	require.Empty(t, filter.Platform)
}

// route_family 是封闭枚举：列表必须在入口就与导出一样拒掉未知取值，
// 不能等仓储层把它当成"非法记录"，更不能静默返回空结果。
func TestRequestTraceAdminListRejectsUnknownRouteFamily(t *testing.T) {
	recorder, _, calls := listTracesWithQuery(t, "route_family=gemini")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Zero(t, calls)
	require.Contains(t, recorder.Body.String(), "REQUEST_TRACE_INVALID_FILTER")

	recorder, filter, calls := listTracesWithQuery(t, "route_family=%20messages%20")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, calls)
	require.Equal(t, service.RequestTraceMessages, filter.RouteFamily)
}
