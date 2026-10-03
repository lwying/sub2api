package handler

import (
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
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
//	GET  /api/v1/accounts                 用户 JWT
//	GET  /api/v1/accounts/groups          用户 JWT（分组候选，静态路由）
//	POST /api/v1/accounts/runtime/batch   用户 JWT（一页运行期快照，静态路由）
//	GET  /api/v1/accounts/:id             用户 JWT
//	GET  /api/v1/accounts/:id/stats       用户 JWT
//	GET  /api/v1/accounts/:id/usage       用户 JWT（被动快照，只读）
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
// 包含名称、平台、类型、状态、分组摘要、容量与安全额度字段；
// 刻意不含备注、凭据、extra、代理、原始错误消息与完整上游身份；
// 上游身份只以服务端掩码结果出现（缺失时为空串）。
// 运行期字段（usage / today_stats / current_concurrency）由 RuntimeBatch 单独返回。
type visibleAccountDTO struct {
	ID          int64                    `json:"id"`
	Name        string                   `json:"name"`
	Platform    string                   `json:"platform"`
	AccountType string                   `json:"account_type"`
	Status      string                   `json:"status"`
	Schedulable bool                     `json:"schedulable"`
	Concurrency int                      `json:"concurrency"`
	Groups      []visibleAccountGroupDTO `json:"groups"`

	RateLimitResetAt       *time.Time `json:"rate_limit_reset_at"`
	OverloadUntil          *time.Time `json:"overload_until"`
	TempUnschedulableUntil *time.Time `json:"temp_unschedulable_until"`

	WindowCostLimit         *float64 `json:"window_cost_limit,omitempty"`
	WindowCostStickyReserve *float64 `json:"window_cost_sticky_reserve,omitempty"`
	MaxSessions             *int     `json:"max_sessions,omitempty"`
	SessionIdleTimeoutMin   *int     `json:"session_idle_timeout_minutes,omitempty"`
	BaseRPM                 *int     `json:"base_rpm,omitempty"`
	RPMStrategy             *string  `json:"rpm_strategy,omitempty"`

	QuotaLimit       *float64 `json:"quota_limit,omitempty"`
	QuotaUsed        *float64 `json:"quota_used,omitempty"`
	QuotaDailyLimit  *float64 `json:"quota_daily_limit,omitempty"`
	QuotaDailyUsed   *float64 `json:"quota_daily_used,omitempty"`
	QuotaWeeklyLimit *float64 `json:"quota_weekly_limit,omitempty"`
	QuotaWeeklyUsed  *float64 `json:"quota_weekly_used,omitempty"`

	EmailMasked             string `json:"email_masked"`
	UsernameMasked          string `json:"username_masked"`
	UpstreamAccountIDMasked string `json:"upstream_account_id_masked"`
}

// visibleAccountGroupDTO 是分组只读摘要。
type visibleAccountGroupDTO struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Platform         string `json:"platform"`
	SubscriptionType string `json:"subscription_type"`
}

func toVisibleAccountGroupDTO(group service.VisibleAccountGroup) visibleAccountGroupDTO {
	return visibleAccountGroupDTO{
		ID:               group.ID,
		Name:             group.Name,
		Platform:         group.Platform,
		SubscriptionType: group.SubscriptionType,
	}
}

func toVisibleAccountDTO(view service.VisibleAccountView) visibleAccountDTO {
	groups := make([]visibleAccountGroupDTO, 0, len(view.Groups))
	for _, group := range view.Groups {
		groups = append(groups, toVisibleAccountGroupDTO(group))
	}
	return visibleAccountDTO{
		ID:                      view.ID,
		Name:                    view.Name,
		Platform:                view.Platform,
		AccountType:             view.AccountType,
		Status:                  view.Status,
		Schedulable:             view.Schedulable,
		Concurrency:             view.Concurrency,
		Groups:                  groups,
		RateLimitResetAt:        view.RateLimitResetAt,
		OverloadUntil:           view.OverloadUntil,
		TempUnschedulableUntil:  view.TempUnschedulableUntil,
		WindowCostLimit:         view.WindowCostLimit,
		WindowCostStickyReserve: view.WindowCostStickyReserve,
		MaxSessions:             view.MaxSessions,
		SessionIdleTimeoutMin:   view.SessionIdleTimeoutMin,
		BaseRPM:                 view.BaseRPM,
		RPMStrategy:             view.RPMStrategy,
		QuotaLimit:              view.QuotaLimit,
		QuotaUsed:               view.QuotaUsed,
		QuotaDailyLimit:         view.QuotaDailyLimit,
		QuotaDailyUsed:          view.QuotaDailyUsed,
		QuotaWeeklyLimit:        view.QuotaWeeklyLimit,
		QuotaWeeklyUsed:         view.QuotaWeeklyUsed,
		EmailMasked:             view.EmailMasked,
		UsernameMasked:          view.UsernameMasked,
		UpstreamAccountIDMasked: view.UpstreamAccountIDMasked,
	}
}

// visibleAccountUsageDTO 是被动用量快照的安全投影。
//
// 只保留窗口用量与采样时间；丢弃 UsageInfo 里的 error / forbidden_reason /
// validation_url 等原始上游错误与链接，普通用户响应绝不携带原始错误文本。
type visibleAccountUsageDTO struct {
	Source         string                 `json:"source,omitempty"`
	UpdatedAt      *time.Time             `json:"updated_at,omitempty"`
	FiveHour       *service.UsageProgress `json:"five_hour,omitempty"`
	SevenDay       *service.UsageProgress `json:"seven_day,omitempty"`
	SevenDaySonnet *service.UsageProgress `json:"seven_day_sonnet,omitempty"`
	SevenDayFable  *service.UsageProgress `json:"seven_day_fable,omitempty"`
}

func toVisibleAccountUsageDTO(usage *service.UsageInfo) *visibleAccountUsageDTO {
	if usage == nil {
		return nil
	}
	return &visibleAccountUsageDTO{
		Source:         usage.Source,
		UpdatedAt:      usage.UpdatedAt,
		FiveHour:       usage.FiveHour,
		SevenDay:       usage.SevenDay,
		SevenDaySonnet: usage.SevenDaySonnet,
		SevenDayFable:  usage.SevenDayFable,
	}
}

// visibleAccountRuntimeDTO 是单个账号的运行期只读快照。
// usage / today_stats / current_concurrency 为 null 表示未知，不补零冒充已知。
type visibleAccountRuntimeDTO struct {
	Usage              *visibleAccountUsageDTO `json:"usage"`
	TodayStats         *service.WindowStats    `json:"today_stats"`
	CurrentConcurrency *int                    `json:"current_concurrency"`
	Concurrency        int                     `json:"concurrency"`
}

// visibleAccountRuntimeBatchDTO 是运行时批量读取的响应体（按账号 id 键控）。
type visibleAccountRuntimeBatchDTO struct {
	Accounts map[string]visibleAccountRuntimeDTO `json:"accounts"`
}

// visibleAccountStatsDTO 是账号整体统计的安全投影：与
// usagestats.AccountUsageStatsResponse 同结构，但刻意不含 upstream_endpoints
// （原始上游端点明细）。models / endpoints 为聚合口径，不含原始错误与正文。
type visibleAccountStatsDTO struct {
	History   []usagestats.AccountUsageHistory `json:"history"`
	Summary   usagestats.AccountUsageSummary   `json:"summary"`
	Models    []usagestats.ModelStat           `json:"models"`
	Endpoints []usagestats.EndpointStat        `json:"endpoints"`
}

func toVisibleAccountStatsDTO(stats *usagestats.AccountUsageStatsResponse) visibleAccountStatsDTO {
	if stats == nil {
		return visibleAccountStatsDTO{
			History:   []usagestats.AccountUsageHistory{},
			Models:    []usagestats.ModelStat{},
			Endpoints: []usagestats.EndpointStat{},
		}
	}
	history := stats.History
	if history == nil {
		history = []usagestats.AccountUsageHistory{}
	}
	models := stats.Models
	if models == nil {
		models = []usagestats.ModelStat{}
	}
	endpoints := stats.Endpoints
	if endpoints == nil {
		endpoints = []usagestats.EndpointStat{}
	}
	return visibleAccountStatsDTO{
		History:   history,
		Summary:   stats.Summary,
		Models:    models,
		Endpoints: endpoints,
	}
}

// List 返回当前用户的可见账号分页。
// GET /api/v1/accounts?page=&page_size=&platform=&account_type=&search=
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
		Platform:    c.Query("platform"),
		AccountType: c.Query("account_type"),
		Status:      c.Query("status"),
		GroupID:     parseVisibleAccountGroupID(c.Query("group")),
		Search:      truncateSearch(c.Query("search")),
		Page:        page,
		PageSize:    pageSize,
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
// 未分配、已软删除或用户失去资格的账号一律返回同一 404（ACCOUNT_NOT_FOUND），
// 猜测 ID 无法区分「不存在」与「不可见」；手动停用账号仍然可见。
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

// Groups 返回当前用户可见账号实际所属的分组候选。
// GET /api/v1/accounts/groups
//
// 只从已授权账号的分组派生，返回数组（不是分页对象），供筛选下拉使用。
func (h *VisibleAccountHandler) Groups(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	c.Header("Cache-Control", "no-store")

	groups, err := h.service.Groups(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	items := make([]visibleAccountGroupDTO, 0, len(groups))
	for _, group := range groups {
		items = append(items, toVisibleAccountGroupDTO(group))
	}
	response.Success(c, items)
}

// RuntimeBatch 批量返回当前用户一页账号的运行期只读快照。
// POST /api/v1/accounts/runtime/batch  body: {"account_ids":[1,2,3]}
//
// 只返回授权集合内命中的账号；未授权 id 被静默忽略，不暴露其存在性。
// 数量上限与列表分页上限一致（100），超出返回 400。
func (h *VisibleAccountHandler) RuntimeBatch(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	c.Header("Cache-Control", "no-store")

	var req struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.service.RuntimeBatch(c.Request.Context(), subject.UserID, req.AccountIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	accounts := make(map[string]visibleAccountRuntimeDTO, len(result))
	for id, runtime := range result {
		accounts[strconv.FormatInt(id, 10)] = visibleAccountRuntimeDTO{
			Usage:              toVisibleAccountUsageDTO(runtime.Usage),
			TodayStats:         runtime.TodayStats,
			CurrentConcurrency: runtime.CurrentConcurrency,
			Concurrency:        runtime.Concurrency,
		}
	}
	response.Success(c, visibleAccountRuntimeBatchDTO{Accounts: accounts})
}

// Stats 返回单个可见账号的整体使用统计。
// GET /api/v1/accounts/:id/stats?days=30
//
// 先做对象级授权（未授权与不存在同为 404），再读取聚合统计；
// 响应刻意不含原始上游端点明细。
func (h *VisibleAccountHandler) Stats(c *gin.Context) {
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

	// days 省略时默认 30；显式提供但非法（非数字或超出 1..90）一律 400，
	// 不做静默回退——静默回退会让调用方以为拿到了自己要的区间。
	days := 30
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 90 {
			response.BadRequest(c, "Invalid days: must be an integer between 1 and 90")
			return
		}
		days = parsed
	}
	now := timezone.Now()
	endTime := timezone.StartOfDay(now.AddDate(0, 0, 1))
	startTime := timezone.StartOfDay(now.AddDate(0, 0, -days+1))

	stats, err := h.service.Stats(c.Request.Context(), subject.UserID, accountID, startTime, endTime)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, toVisibleAccountStatsDTO(stats))
}

// Usage 返回单个可见账号的被动用量快照（只读）。
// GET /api/v1/accounts/:id/usage
//
// 仅 Anthropic OAuth / Setup-Token 账号有被动数据；其它平台或暂无采样时返回 null，
// 不触发上游探测、Token 刷新或任何写操作。
func (h *VisibleAccountHandler) Usage(c *gin.Context) {
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
	usage, err := h.service.Usage(c.Request.Context(), subject.UserID, accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, toVisibleAccountUsageDTO(usage))
}

// parseVisibleAccountGroupID 解析分组过滤参数：空或非法一律视为不过滤（0）。
// 不返回错误：非法分组取值只会得到该用户范围内的普通结果，不会扩大范围。
func parseVisibleAccountGroupID(raw string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
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
