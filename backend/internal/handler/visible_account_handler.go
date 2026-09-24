package handler

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// visible_account_handler.go 提供普通用户「已分配账号只读查看」的 HTTP 入口。
//
// 这是与管理员账号接口完全独立的只读入口：响应为纯白名单 DTO，
// 不复用管理员宽度 DTO、账号导出或代理接口，也不提供任何写操作。
// 管理员侧的分配入口在 internal/handler/admin/user_account_view_handler.go。
//
// 路由：
//
//	GET /api/v1/accounts          用户 JWT
//	GET /api/v1/accounts/:id      用户 JWT
type VisibleAccountHandler struct {
	service *service.VisibleAccountService
}

// maxVisibleAccountSearchRunes 是搜索词的最大字符数（不是字节数）。
const maxVisibleAccountSearchRunes = 100

// NewVisibleAccountHandler 构造账号只读查看处理器。
func NewVisibleAccountHandler(visibleAccountService *service.VisibleAccountService) *VisibleAccountHandler {
	return &VisibleAccountHandler{service: visibleAccountService}
}

// visibleAccountDTO 是普通用户可见的账号字段白名单。
//
// 刻意不含名称、备注、凭据、extra、代理、余额、错误消息与运行状态；
// 上游身份只以服务端掩码结果出现（缺失时为空串）。
type visibleAccountDTO struct {
	ID                      int64  `json:"id"`
	Platform                string `json:"platform"`
	AccountType             string `json:"account_type"`
	EmailMasked             string `json:"email_masked"`
	UsernameMasked          string `json:"username_masked"`
	UpstreamAccountIDMasked string `json:"upstream_account_id_masked"`
}

func toVisibleAccountDTO(view service.VisibleAccountView) visibleAccountDTO {
	return visibleAccountDTO{
		ID:                      view.ID,
		Platform:                view.Platform,
		AccountType:             view.AccountType,
		EmailMasked:             view.EmailMasked,
		UsernameMasked:          view.UsernameMasked,
		UpstreamAccountIDMasked: view.UpstreamAccountIDMasked,
	}
}

// List 返回当前用户的可见账号分页。
// GET /api/v1/accounts?page=&page_size=&platform=&search=
func (h *VisibleAccountHandler) List(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	// 撤销、禁用与能力开关都必须在下一次请求立即生效，禁止任何中间缓存。
	c.Header("Cache-Control", "no-store")

	page, pageSize := response.ParsePagination(c)
	filter := service.VisibleAccountFilter{
		Platform: c.Query("platform"),
		Search:   truncateSearch(c.Query("search")),
		Page:     page,
		PageSize: pageSize,
	}

	result, err := h.service.List(c.Request.Context(), subject.UserID, filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	items := make([]visibleAccountDTO, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, toVisibleAccountDTO(item))
	}
	response.Paginated(c, items, result.Total, result.Page, result.PageSize)
}

// Get 返回单个可见账号。
// GET /api/v1/accounts/:id
//
// 未分配、已删除或已手动禁用的账号一律返回同一 404（ACCOUNT_NOT_FOUND），
// 猜测 ID 无法区分「不存在」与「不可见」。
func (h *VisibleAccountHandler) Get(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	c.Header("Cache-Control", "no-store")

	accountID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || accountID <= 0 {
		response.ErrorFrom(c, service.ErrVisibleAccountNotFound)
		return
	}

	view, err := h.service.Get(c.Request.Context(), subject.UserID, accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, toVisibleAccountDTO(view))
}

// truncateSearch 按「字符」而非字节截断搜索词：字节切片会切断多字节 rune，
// 产生非法 UTF-8 传给 Postgres。与其它 handler 的 rune 截断口径保持一致。
func truncateSearch(search string) string {
	trimmed := strings.TrimSpace(search)
	if runes := []rune(trimmed); len(runes) > maxVisibleAccountSearchRunes {
		return string(runes[:maxVisibleAccountSearchRunes])
	}
	return trimmed
}
