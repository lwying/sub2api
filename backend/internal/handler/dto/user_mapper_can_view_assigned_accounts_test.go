package dto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 登录、注册与 2FA 完成的认证响应序列化的正是 dto.User，所以该 DTO 必须和
// /auth/me 的 profile 响应一样带上 can_view_assigned_accounts；否则前端登录后
// 只能等 profile 或缓存刷新，/keys 深链在刷新前会被判定为无查看资格。
func TestUserFromService_MapsCanViewAssignedAccounts(t *testing.T) {
	t.Parallel()

	granted := UserFromService(&service.User{ID: 1, Email: "granted@example.com", CanViewAssignedAccounts: true})
	require.NotNil(t, granted)
	require.True(t, granted.CanViewAssignedAccounts)

	denied := UserFromService(&service.User{ID: 2, Email: "denied@example.com"})
	require.NotNil(t, denied)
	require.False(t, denied.CanViewAssignedAccounts, "未授权用户必须显式映射为 false")
}

// Shallow 映射被 API Key、兑换码、用量日志等嵌套用户对象复用；能力开关在
// 用户行上是权威值，这里不做角色或其他推断。
func TestUserFromServiceShallow_MapsCanViewAssignedAccounts(t *testing.T) {
	t.Parallel()

	granted := UserFromServiceShallow(&service.User{ID: 3, Email: "shallow@example.com", CanViewAssignedAccounts: true})
	require.NotNil(t, granted)
	require.True(t, granted.CanViewAssignedAccounts)

	denied := UserFromServiceShallow(&service.User{ID: 4, Email: "shallow2@example.com"})
	require.NotNil(t, denied)
	require.False(t, denied.CanViewAssignedAccounts)
}

// 前端以 `=== true` 判定能力，所以字段必须始终出现在 JSON 里且不能被
// omitempty 省略，否则「未授权」与「字段缺失」在前端无法区分。
func TestUserFromService_SerializesCapabilityKeyEvenWhenFalse(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(UserFromService(&service.User{ID: 5, Email: "json@example.com"}))
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))

	value, ok := payload["can_view_assigned_accounts"]
	require.True(t, ok, "JSON 必须包含 can_view_assigned_accounts 键")
	require.Equal(t, false, value)

	rawGranted, err := json.Marshal(UserFromService(&service.User{ID: 6, Email: "json2@example.com", CanViewAssignedAccounts: true}))
	require.NoError(t, err)
	require.Contains(t, string(rawGranted), `"can_view_assigned_accounts":true`)
}

// AdminUser 内嵌 dto.User，因此管理员查询接口也会带上该字段。管理员菜单的
// 可见性由前端按 role 判定，DTO 层不按角色改写能力值，避免出现第二个事实来源。
func TestUserFromServiceAdmin_DoesNotInferCapabilityFromRole(t *testing.T) {
	t.Parallel()

	admin := UserFromServiceAdmin(&service.User{ID: 7, Email: "root@example.com", Role: service.RoleAdmin})
	require.NotNil(t, admin)
	require.False(t, admin.CanViewAssignedAccounts)

	adminGranted := UserFromServiceAdmin(&service.User{
		ID:                      8,
		Email:                   "root2@example.com",
		Role:                    service.RoleAdmin,
		CanViewAssignedAccounts: true,
	})
	require.NotNil(t, adminGranted)
	require.True(t, adminGranted.CanViewAssignedAccounts)
}

// userProfileResponse 同时内嵌 dto.User 并自声明同名字段；外层字段按 Go 的
// 嵌入规则遮蔽内层字段，JSON 中该键只能出现一次。这里用同形的本地结构锁住
// 该不变量，避免 /auth/me 的严格契约（JSONEq）因重复键或取值不一致而失败。
func TestUserDTO_ProfileStyleEmbeddingEmitsSingleCapabilityKey(t *testing.T) {
	t.Parallel()

	type profileShapedResponse struct {
		User
		CanViewAssignedAccounts bool `json:"can_view_assigned_accounts"`
	}

	user := &service.User{ID: 9, Email: "profile@example.com", CanViewAssignedAccounts: true}
	base := UserFromService(user)
	require.NotNil(t, base)

	raw, err := json.Marshal(profileShapedResponse{
		User:                    *base,
		CanViewAssignedAccounts: user.CanViewAssignedAccounts,
	})
	require.NoError(t, err)

	require.Equal(t, 1, strings.Count(string(raw), `"can_view_assigned_accounts"`), "该键只能出现一次")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, true, payload["can_view_assigned_accounts"])
}
