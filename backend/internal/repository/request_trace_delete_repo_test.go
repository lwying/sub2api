//go:build unit

package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceDeleteFilterValidationMirrorsList(t *testing.T) {
	require.NoError(t, validateRequestTraceDeleteFilter(service.RequestTraceExportFilter{RouteFamily: "messages"}))
	require.ErrorIs(t, validateRequestTraceDeleteFilter(service.RequestTraceExportFilter{RouteFamily: "unknown"}), service.ErrRequestTraceInvalidRecord)

	badStatus := 700
	require.ErrorIs(t, validateRequestTraceDeleteFilter(service.RequestTraceExportFilter{ClientStatus: &badStatus}), service.ErrRequestTraceInvalidRecord)

	// 具体值与"未知"互斥：两者同时给出是列表也会拒绝的筛选。
	groupID := int64(3)
	unknown := true
	require.ErrorIs(t, validateRequestTraceDeleteFilter(service.RequestTraceExportFilter{GroupID: &groupID, GroupUnknown: &unknown}), service.ErrRequestTraceInvalidRecord)

	// 纯通配符关键字等价于全表扫描，列表会拒绝，删除也必须拒绝。
	require.ErrorIs(t, validateRequestTraceDeleteFilter(service.RequestTraceExportFilter{Keyword: "%%__"}), service.ErrRequestTraceInvalidRecord)
}

func TestRequestTraceMetadataFilterArgsKeepsListParameterOrder(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	args := requestTraceMetadataFilterArgs(service.RequestTraceExportFilter{
		TraceID: "0123456789abcdef0123456789abcdef", RouteFamily: "messages", CreatedFrom: &from, CreatedTo: &to, Keyword: "abc",
	})
	require.Len(t, args, 19, "the shared clause binds exactly $1..$19")
	require.Equal(t, "0123456789abcdef0123456789abcdef", args[0])
	require.Equal(t, "messages", args[1])
	require.Equal(t, from.UTC(), args[3])
	require.Equal(t, to.UTC(), args[4])
	require.Equal(t, "abc", args[18])
}
