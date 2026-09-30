//go:build unit

package repository

import (
	"database/sql"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpsInsertErrorLogArgsPreservesExplicitZeroUpstreamStatus(t *testing.T) {
	zero := 0
	args := opsInsertErrorLogArgs(&service.OpsInsertErrorLogInput{UpstreamStatusCode: &zero})

	require.Len(t, args, 39)
	encoded, ok := args[27].(sql.NullInt64)
	require.True(t, ok)
	require.True(t, encoded.Valid)
	require.Zero(t, encoded.Int64)
}

// 服务端 Trace ID 是新增的最后一个绑定参数：空值落 SQL NULL（历史行与未关联错误），
// 有值时按原样绑定，绝不接受客户端可重复的请求 ID 作为关联依据。
func TestOpsInsertErrorLogArgsBindsRequestTraceID(t *testing.T) {
	absent := opsInsertErrorLogArgs(&service.OpsInsertErrorLogInput{})
	require.Len(t, absent, 39)
	missing, ok := absent[38].(sql.NullString)
	require.True(t, ok)
	require.False(t, missing.Valid, "an absent trace id must bind SQL NULL")

	present := opsInsertErrorLogArgs(&service.OpsInsertErrorLogInput{RequestTraceID: "0123456789abcdef0123456789abcdef"})
	bound, ok := present[38].(sql.NullString)
	require.True(t, ok)
	require.True(t, bound.Valid)
	require.Equal(t, "0123456789abcdef0123456789abcdef", bound.String)
}

func TestOpsNullableIntPointerDistinguishesNilZeroAndStatus(t *testing.T) {
	missing := opsNullableIntPointer(nil).(sql.NullInt64)
	require.False(t, missing.Valid)

	zeroValue := 0
	zero := opsNullableIntPointer(&zeroValue).(sql.NullInt64)
	require.True(t, zero.Valid)
	require.Zero(t, zero.Int64)

	statusValue := 503
	status := opsNullableIntPointer(&statusValue).(sql.NullInt64)
	require.True(t, status.Valid)
	require.EqualValues(t, 503, status.Int64)
}
