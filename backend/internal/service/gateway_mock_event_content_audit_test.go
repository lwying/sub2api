//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 内容审计状态是一个有界闭集。合法取值原样保留；旧记录（空串）与闭集之外的取值一律
// 降级为 unknown：既不猜测旧数据是"未执行审计"，也不把未知枚举回显出去。
func TestNormalizeGatewayMockContentAuditState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"skipped local mock is preserved", GatewayMockContentAuditSkippedLocalMock, GatewayMockContentAuditSkippedLocalMock},
		{"unknown is preserved", GatewayMockContentAuditUnknown, GatewayMockContentAuditUnknown},
		{"empty legacy value is unknown, never skipped", "", GatewayMockContentAuditUnknown},
		{"unknown enum is unknown", "invented_state", GatewayMockContentAuditUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, NormalizeGatewayMockContentAuditState(tc.input))
		})
	}
	// 常量取值本身必须稳定，数据库 CHECK 约束与前端闭集都以它们为准。
	require.Equal(t, "unknown", GatewayMockContentAuditUnknown)
	require.Equal(t, "skipped_local_mock", GatewayMockContentAuditSkippedLocalMock)
}
