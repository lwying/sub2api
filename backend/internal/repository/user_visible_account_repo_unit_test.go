package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMissingVisibleAccountIDsIsolatesStaleEntries 覆盖「保存失败时指出具体失败项」的
// 增量判定：一次批量保存里，允许保留的既有分配（对应账号已被软删除）不算失败，
// 只有真正不可用的「新增」项被单独列出，且保持输入顺序供错误契约使用。
func TestMissingVisibleAccountIDsIsolatesStaleEntries(t *testing.T) {
	t.Parallel()

	live := map[int64]struct{}{10: {}, 12: {}}

	// 全部可用：没有失败项（不能因为集合为空就报错）。
	require.Empty(t, missingVisibleAccountIDs([]int64{}, live))
	require.Empty(t, missingVisibleAccountIDs([]int64{10, 12}, live))

	// 只有一个失效（在勾选后被删除）：必须只报这一个。
	require.Equal(t, []int64{999}, missingVisibleAccountIDs([]int64{10, 999}, live))

	// 多个失效：逐个列出并保持输入顺序，不合并成一句泛化失败。
	require.Equal(t, []int64{999, 4242}, missingVisibleAccountIDs([]int64{999, 10, 4242}, live))
}

// TestNonPositiveAccountIDsRejectsExplicitly 覆盖非法 id 的判定：<=0 的取值只可能
// 来自错误客户端或篡改载荷，必须被点名而不是静默丢弃成「清空分配」。
func TestNonPositiveAccountIDsRejectsExplicitly(t *testing.T) {
	t.Parallel()

	require.Empty(t, nonPositiveAccountIDs([]int64{1, 2, 3}))
	require.Equal(t, []int64{0}, nonPositiveAccountIDs([]int64{5, 0}))
	require.Equal(t, []int64{0, -3}, nonPositiveAccountIDs([]int64{0, -3}))
}
