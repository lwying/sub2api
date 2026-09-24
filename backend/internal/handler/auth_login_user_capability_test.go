//go:build unit

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 登录、注册与 2FA 完成都经由 respondWithTokenPair 返回 AuthResponse，
// 其 user 字段是 dto.User。该响应必须带上 can_view_assigned_accounts，
// 否则前端在登录后到 profile 刷新前拿不到能力值，/keys 深链会被误判为
// 无查看资格。
func TestRespondWithTokenPair_CarriesCanViewAssignedAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name     string
		granted  bool
		expected bool
	}{
		{name: "已授权用户为 true", granted: true, expected: true},
		{name: "未授权用户为 false", granted: false, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &userHandlerRepoStub{
				user: &service.User{
					ID:                      41,
					Email:                   "viewer@example.com",
					Username:                "viewer",
					Role:                    service.RoleUser,
					Status:                  service.StatusActive,
					CanViewAssignedAccounts: tt.granted,
				},
			}
			cfg := &config.Config{
				JWT: config.JWTConfig{
					Secret:     "test-secret",
					ExpireHour: 1,
				},
			}
			authService := service.NewAuthService(
				nil, repo, nil, &userHandlerRefreshTokenCacheStub{}, cfg,
				nil, nil, nil, nil, nil, nil, nil, nil,
			)

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)

			respondWithTokenPair(c, authService, repo.user)

			require.Equal(t, http.StatusOK, recorder.Code)

			var body struct {
				Code int `json:"code"`
				Data AuthResponse
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			require.NotNil(t, body.Data.User, "认证响应必须包含 user")
			require.Equal(t, tt.expected, body.Data.User.CanViewAssignedAccounts)
			require.Equal(t, service.RoleUser, body.Data.User.Role)
		})
	}
}

// 能力撤销后重新登录必须立即拿到 false；认证响应不能沿用旧 JWT 或浏览器
// 缓存里的旧值（ticket05 验收：撤销立即生效）。
func TestRespondWithTokenPair_ReflectsFreshCapabilityAfterRevoke(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &userHandlerRepoStub{
		user: &service.User{
			ID:                      42,
			Email:                   "revoked@example.com",
			Role:                    service.RoleUser,
			Status:                  service.StatusActive,
			CanViewAssignedAccounts: true,
		},
	}
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "test-secret", ExpireHour: 1}}
	authService := service.NewAuthService(
		nil, repo, nil, &userHandlerRefreshTokenCacheStub{}, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)

	readCapability := func() bool {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		respondWithTokenPair(c, authService, repo.user)

		var body struct {
			Data AuthResponse
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		require.NotNil(t, body.Data.User)
		return body.Data.User.CanViewAssignedAccounts
	}

	require.True(t, readCapability())

	repo.user.CanViewAssignedAccounts = false
	require.False(t, readCapability(), "撤销后新的认证响应必须立即为 false")
}
