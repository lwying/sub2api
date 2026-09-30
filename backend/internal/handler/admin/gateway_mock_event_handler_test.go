//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type gatewayMockEventReaderStub struct {
	records []service.GatewayMockEventRecord
	total   int64
	err     error
	filters []service.GatewayMockEventListFilter
}

func (s *gatewayMockEventReaderStub) ListGatewayMockEvents(_ context.Context, filter service.GatewayMockEventListFilter) ([]service.GatewayMockEventRecord, int64, error) {
	s.filters = append(s.filters, filter)
	if s.err != nil {
		return nil, 0, s.err
	}
	return s.records, s.total, nil
}

func serveGatewayMockEvents(t *testing.T, handler *GatewayMockEventHandler, target string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	handler.List(c)
	return recorder
}

type gatewayMockEventPageBody struct {
	Data struct {
		Items    []map[string]any `json:"items"`
		Total    int64            `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
		Pages    int              `json:"pages"`
	} `json:"data"`
}

// 列表是管理端唯一会成批读出最小事件的入口：它必须只给出落库的元数据，
// 并且一次请求的条数有界。
func TestGatewayMockEventHandlerListsOnlyMinimalFactsWithinBounds(t *testing.T) {
	occurred := time.Date(2026, 9, 30, 3, 4, 5, 0, time.UTC)
	cleanup := occurred.AddDate(0, 0, 90)
	stub := &gatewayMockEventReaderStub{
		total: 1,
		records: []service.GatewayMockEventRecord{{
			OccurredAt: occurred, RuleID: "gmr_0123456789abcdef", RuleVersion: "2026-09-30T03:00:00Z",
			Protocol: "messages", Model: "claude-sonnet-4-5", APIKeyID: 7, UserID: 3, GroupID: 2,
			AccountID: 11, ClientIP: "203.0.113.7", TraceID: "0123456789abcdef0123456789abcdef", CleanupAfter: cleanup,
		}},
	}

	// page_size=500 仍是通用分页解析接受的范围（上限 1000），但本列表自己的上限是 100。
	recorder := serveGatewayMockEvents(t, NewGatewayMockEventHandler(stub), "/api/v1/admin/settings/gateway-mock/events?page=2&page_size=500")

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store, private", recorder.Header().Get("Cache-Control"))
	require.Len(t, stub.filters, 1)
	require.Equal(t, 2, stub.filters[0].Page)
	require.Equal(t, service.GatewayMockEventMaxPageSize, stub.filters[0].PageSize,
		"a page size above the bound must be narrowed before it reaches the store")

	var body gatewayMockEventPageBody
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, int64(1), body.Data.Total)
	require.Equal(t, 2, body.Data.Page)
	require.Equal(t, service.GatewayMockEventMaxPageSize, body.Data.PageSize)
	require.Len(t, body.Data.Items, 1)

	keys := make([]string, 0, len(body.Data.Items[0]))
	for key := range body.Data.Items[0] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	require.Equal(t, []string{
		"account_id", "api_key_id", "cleanup_after", "client_ip", "group_id", "model",
		"occurred_at", "protocol", "rule_id", "rule_version", "trace_id", "user_id",
	}, keys, "the list projection is exactly the stored minimal facts")

	require.Equal(t, "gmr_0123456789abcdef", body.Data.Items[0]["rule_id"])
	require.Equal(t, "203.0.113.7", body.Data.Items[0]["client_ip"])

	// 关键词与回复正文从未落库，也就不可能出现在这里；数据库主键同样不属于管理端事实。
	lowered := strings.ToLower(recorder.Body.String())
	for _, forbidden := range []string{"keyword", "reply", "prompt", "completion", "payload", "authorization", "credential", "secret", "password"} {
		require.NotContains(t, lowered, forbidden, "the event list must not carry %q", forbidden)
	}
}

// 未接线与读取失败都不能渲染成"没有命中过"：零条事件与不可读是两回事。
func TestGatewayMockEventHandlerReportsUnreadableListAsUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		handler *GatewayMockEventHandler
	}{
		{"nil handler", nil},
		{"nil reader", NewGatewayMockEventHandler(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveGatewayMockEvents(t, tc.handler, "/api/v1/admin/settings/gateway-mock/events")
			require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			require.Contains(t, recorder.Body.String(), "GATEWAY_MOCK_EVENTS_UNAVAILABLE")
			require.NotContains(t, recorder.Body.String(), "occurred_at")
		})
	}

	t.Run("reader failure", func(t *testing.T) {
		stub := &gatewayMockEventReaderStub{err: errors.New(`pq: relation "gateway_mock_events" does not exist`)}
		recorder := serveGatewayMockEvents(t, NewGatewayMockEventHandler(stub), "/api/v1/admin/settings/gateway-mock/events")
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		require.Contains(t, recorder.Body.String(), "GATEWAY_MOCK_EVENTS_UNAVAILABLE")
		// 数据库报文不回传：管理端拿到的只有有界原因。
		require.NotContains(t, recorder.Body.String(), "does not exist")
		require.NotContains(t, recorder.Body.String(), "gateway_mock_events")
	})

	t.Run("empty list is a normal answer", func(t *testing.T) {
		stub := &gatewayMockEventReaderStub{records: nil, total: 0}
		recorder := serveGatewayMockEvents(t, NewGatewayMockEventHandler(stub), "/api/v1/admin/settings/gateway-mock/events")
		require.Equal(t, http.StatusOK, recorder.Code)

		var body gatewayMockEventPageBody
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		require.NotNil(t, body.Data.Items, "an empty page is still a list, not a missing field")
		require.Empty(t, body.Data.Items)
		require.Equal(t, int64(0), body.Data.Total)
	})
}
