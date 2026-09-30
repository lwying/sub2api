//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 采集范围是"该不该保存明文"的边界，必须有直接覆盖：
// 只有"全部"才覆盖未观察到的事实，任何"指定/排除"都必须先拿到事实。
func TestRequestTraceScopeRequiresKnownFactsWhenNarrowing(t *testing.T) {
	groupID := int64(7)
	otherGroup := int64(8)

	t.Run("group scope", func(t *testing.T) {
		all := RequestTraceSettings{AllGroups: true}
		require.True(t, all.InGroupScope(nil), "全部分组覆盖无法确定分组的请求")
		require.True(t, all.InGroupScope(&groupID))

		only := RequestTraceSettings{GroupIDs: []int64{groupID}}
		require.True(t, only.InGroupScope(&groupID))
		require.False(t, only.InGroupScope(&otherGroup))
		require.False(t, only.InGroupScope(nil), "指定分组时无法确定分组即不采集")
	})

	t.Run("model scope", func(t *testing.T) {
		all := RequestTraceSettings{ModelScope: RequestTraceScopeAll}
		require.True(t, all.InModelScope(""), "所有模型覆盖无法确定模型名的请求")

		include := RequestTraceSettings{ModelScope: RequestTraceScopeInclude, Models: []string{"claude-sonnet-4-5"}}
		require.True(t, include.InModelScope("CLAUDE-Sonnet-4-5"), "大小写不敏感")
		require.False(t, include.InModelScope("gpt-5"))
		require.False(t, include.InModelScope(""), "仅指定模型时必须先拿到模型名")

		exclude := RequestTraceSettings{ModelScope: RequestTraceScopeExclude, Models: []string{"gpt-5"}}
		require.True(t, exclude.InModelScope("claude-sonnet-4-5"))
		require.False(t, exclude.InModelScope("gpt-5"))
		require.False(t, exclude.InModelScope(""), "排除指定模型时模型未知同样不采集")
	})

	t.Run("platform scope", func(t *testing.T) {
		all := RequestTraceSettings{PlatformScope: RequestTraceScopeAll}
		require.True(t, all.InPlatformScope(nil), "所有平台覆盖未选到账号的请求")

		include := RequestTraceSettings{PlatformScope: RequestTraceScopeInclude, Platforms: []string{"anthropic"}}
		require.True(t, include.InPlatformScope([]string{"anthropic"}))
		require.False(t, include.InPlatformScope([]string{"antigravity"}))
		require.False(t, include.InPlatformScope(nil), "仅指定平台时无法确定平台即不采集")

		exclude := RequestTraceSettings{PlatformScope: RequestTraceScopeExclude, Platforms: []string{"antigravity"}}
		require.True(t, exclude.InPlatformScope([]string{"anthropic"}))
		require.False(t, exclude.InPlatformScope([]string{"antigravity"}))
		require.False(t, exclude.InPlatformScope(nil),
			"排除指定平台时平台未知不能放行：否则排除形同虚设")
	})

	t.Run("combined scope takes the intersection", func(t *testing.T) {
		scope := RequestTraceSettings{AllGroups: true, ModelScope: RequestTraceScopeAll, PlatformScope: RequestTraceScopeAll}
		require.True(t, scope.InScope(RequestTraceScopeFacts{}))

		narrowed := RequestTraceSettings{GroupIDs: []int64{groupID}, ModelScope: RequestTraceScopeAll, PlatformScope: RequestTraceScopeAll}
		require.False(t, narrowed.InScope(RequestTraceScopeFacts{GroupID: nil}), "任一维度不满足即整条不采集")
		require.True(t, narrowed.InScope(RequestTraceScopeFacts{GroupID: &groupID}))
	})
}

// 范围字段的归一化：越界类型落回 all，列表去空白去重，全部分组丢弃分组列表。
func TestRequestTraceScopeNormalization(t *testing.T) {
	normalized := NormalizeRequestTraceSettings(RequestTraceSettings{
		AllGroups:     true,
		GroupIDs:      []int64{0, 7, 7, -1, 8},
		ModelScope:    "Include",
		Models:        []string{"  ", "gpt-5", "GPT-5", " gpt-5 "},
		PlatformScope: "nonsense",
		Platforms:     []string{"anthropic", "anthropic"},
	})

	require.Equal(t, RequestTraceScopeInclude, normalized.ModelScope, "取值大小写归一")
	require.Equal(t, []string{"gpt-5"}, normalized.Models, "去空白、去重且保留原大小写")
	require.Equal(t, RequestTraceScopeAll, normalized.PlatformScope, "未知取值落回 all")
	require.Empty(t, normalized.Platforms, "all 不保留具体平台列表")
	require.Empty(t, normalized.GroupIDs, "全部分组不保留具体分组列表")
}
