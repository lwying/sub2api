//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 信封上的"实际选中平台历史"是列表/导出按任一平台检索、并把"选中但未发出"的请求
// 排除在 platform_unknown 之外的依据，因此它的归一化与落库校验必须逐条固定：
// 未知只能用 nil 表达，绝不返回空数组；非法取值丢弃而不是猜；顺序按首次观察；
// 有界截断保留首个平台（首个平台是采集范围的判定依据）。
func TestNormalizeRequestTraceObservedPlatforms(t *testing.T) {
	require.Nil(t, NormalizeRequestTraceObservedPlatforms(nil), "从未选到账号时必须保持 nil")
	require.Nil(t, NormalizeRequestTraceObservedPlatforms([]string{}), "空输入不能变成空数组")
	require.Nil(t, NormalizeRequestTraceObservedPlatforms([]string{"", "   "}), "空白项要被丢弃而不是当平台")

	require.Equal(t, []string{PlatformAnthropic},
		NormalizeRequestTraceObservedPlatforms([]string{"  anthropic  "}))
	require.Equal(t, []string{PlatformAnthropic, PlatformOpenAI},
		NormalizeRequestTraceObservedPlatforms([]string{PlatformAnthropic, PlatformOpenAI, PlatformAnthropic}),
		"去重同时保留首次观察顺序：多平台请求按任一平台都可检索")
	require.Equal(t, []string{PlatformOpenAI},
		NormalizeRequestTraceObservedPlatforms([]string{"anthropic bad", PlatformOpenAI}),
		"形状非法的条目被丢弃，不阻止其余合法平台落库")
	require.Nil(t, NormalizeRequestTraceObservedPlatforms([]string{"has space", "has\ttab"}),
		"全部条目都非法时结果仍是 nil（未知），不能退化成空数组")

	long := make([]string, 0, RequestTraceObservedPlatformLimit+4)
	for i := 0; i < RequestTraceObservedPlatformLimit+4; i++ {
		long = append(long, "platform_"+string(rune('a'+i)))
	}
	normalized := NormalizeRequestTraceObservedPlatforms(long)
	require.Len(t, normalized, RequestTraceObservedPlatformLimit)
	require.Equal(t, long[0], normalized[0], "截断必须保留首次观察到的平台")
}

func TestValidRequestTraceObservedPlatforms(t *testing.T) {
	require.True(t, ValidRequestTraceObservedPlatforms(nil), "nil 表示未知，是合法取值")
	require.True(t, ValidRequestTraceObservedPlatforms([]string{PlatformAnthropic, PlatformOpenAI}))

	require.False(t, ValidRequestTraceObservedPlatforms([]string{}), "空数组不能冒充已观察")
	require.False(t, ValidRequestTraceObservedPlatforms([]string{PlatformAnthropic, PlatformAnthropic}),
		"重复条目必须被拒绝（数据库侧 CHECK 同样拒绝）")
	require.False(t, ValidRequestTraceObservedPlatforms([]string{"bad platform"}))
	require.False(t, ValidRequestTraceObservedPlatforms([]string{""}))
	require.False(t, ValidRequestTraceObservedPlatforms([]string{strings.Repeat("a", 129)}),
		"超长 token 必须被拒绝")

	over := make([]string, 0, RequestTraceObservedPlatformLimit+1)
	for i := 0; i <= RequestTraceObservedPlatformLimit; i++ {
		over = append(over, "platform_"+string(rune('a'+i)))
	}
	require.False(t, ValidRequestTraceObservedPlatforms(over), "超过上限必须被拒绝")
}
