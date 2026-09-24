package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// user_account_view_handler.go 提供管理员对「普通用户可见账号」的分配接口。
//
// 逐用户显式分配，默认关闭且默认零个；分配只授予只读查看，不改变该用户的
// API Key、模型调用、个人用量，也不授予任何账号写操作。
//
// 路由（均在 /api/v1/admin 下，受管理员鉴权保护）：
//
//	GET /api/v1/admin/users/:id/account-view
//	PUT /api/v1/admin/users/:id/account-view
type adminAccountViewAccount struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	AccountType string `json:"account_type"`
	Status      string `json:"status"`
}

type adminAccountViewResponse struct {
	UserID     int64                     `json:"user_id"`
	Enabled    bool                      `json:"enabled"`
	AccountIDs []int64                   `json:"account_ids"`
	Accounts   []adminAccountViewAccount `json:"accounts"`
}

// adminAccountViewUpdateRequest 是分配更新载荷。
//
// 两个字段都可省略：省略表示保持不变；account_ids 传空数组表示清空。
// 同时提供时在同一请求内原子生效。
type adminAccountViewUpdateRequest struct {
	Enabled    *bool    `json:"enabled"`
	AccountIDs *[]int64 `json:"account_ids"`
}

// SetVisibleAccountService 注入账号查看服务（构造后装配，保持既有构造签名）。
func (h *UserHandler) SetVisibleAccountService(visibleAccountService *service.VisibleAccountService) {
	h.visibleAccountService = visibleAccountService
}

// GetAccountView 返回某用户的账号查看能力与已分配账号。
// GET /api/v1/admin/users/:id/account-view
func (h *UserHandler) GetAccountView(c *gin.Context) {
	userID, ok := parseAdminAccountViewUserID(c)
	if !ok {
		return
	}
	if h.visibleAccountService == nil {
		response.InternalError(c, "Account view service unavailable")
		return
	}

	view, err := h.visibleAccountService.AdminView(c.Request.Context(), userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, toAdminAccountViewResponse(view))
}

// UpdateAccountView 更新某用户的账号查看能力与分配集合。
// PUT /api/v1/admin/users/:id/account-view
func (h *UserHandler) UpdateAccountView(c *gin.Context) {
	userID, ok := parseAdminAccountViewUserID(c)
	if !ok {
		return
	}
	if h.visibleAccountService == nil {
		response.InternalError(c, "Account view service unavailable")
		return
	}

	var req adminAccountViewUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	// 两个字段都可省略，但不能都省略：没有有效载荷的请求（含 JSON 里的显式
	// null）不应被当成「清空」或「关闭」，避免误操作造成隐式撤销。
	if req.Enabled == nil && req.AccountIDs == nil {
		response.BadRequest(c, "enabled or account_ids is required")
		return
	}

	var grantedBy *int64
	if subject, ok := middleware2.GetAuthSubjectFromContext(c); ok && subject.UserID > 0 {
		actor := subject.UserID
		grantedBy = &actor
	}

	view, err := h.visibleAccountService.AdminUpdate(
		c.Request.Context(), userID, req.Enabled, req.AccountIDs, grantedBy)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, toAdminAccountViewResponse(view))
}

func parseAdminAccountViewUserID(c *gin.Context) (int64, bool) {
	userID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || userID <= 0 {
		response.ErrorFrom(c, service.ErrAccountViewUserNotFound)
		return 0, false
	}
	return userID, true
}

func toAdminAccountViewResponse(view service.AdminAccountView) adminAccountViewResponse {
	ids := make([]int64, 0, len(view.AccountIDs))
	ids = append(ids, view.AccountIDs...)
	accounts := make([]adminAccountViewAccount, 0, len(view.Accounts))
	for _, item := range view.Accounts {
		accounts = append(accounts, adminAccountViewAccount{
			ID:          item.ID,
			Name:        item.Name,
			Platform:    item.Platform,
			AccountType: item.Type,
			Status:      item.Status,
		})
	}
	return adminAccountViewResponse{
		UserID:     view.UserID,
		Enabled:    view.Enabled,
		AccountIDs: ids,
		Accounts:   accounts,
	}
}
