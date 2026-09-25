package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// UsageHandler handles admin usage-related requests
type UsageHandler struct {
	usageService     *service.UsageService
	apiKeyService    *service.APIKeyService
	adminService     service.AdminService
	cleanupService   *service.UsageCleanupService
	requestAuditRepo service.RequestAuditRepository
	// requestAuditValueDetail 是默认关闭的 Claude /v1/messages 值明细接缝。
	// 通过 SetRequestAuditValueDetailService 显式注入；为 nil 时入口按不可用处理，
	// 不使用构造函数参数以免改动既有装配调用。
	requestAuditValueDetail *service.RequestAuditValueDetailService
}

// NewUsageHandler creates a new admin usage handler
func NewUsageHandler(
	usageService *service.UsageService,
	apiKeyService *service.APIKeyService,
	adminService service.AdminService,
	cleanupService *service.UsageCleanupService,
	requestAuditRepo service.RequestAuditRepository,
) *UsageHandler {
	return &UsageHandler{
		usageService:     usageService,
		apiKeyService:    apiKeyService,
		adminService:     adminService,
		cleanupService:   cleanupService,
		requestAuditRepo: requestAuditRepo,
	}
}

// GetRequestAudit returns protocol metadata attached to a usage log. It never includes model body.
func (h *UsageHandler) GetRequestAudit(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid id")
		return
	}
	if h == nil || h.requestAuditRepo == nil {
		response.NotFound(c, "Request audit not found")
		return
	}
	rec, err := h.requestAuditRepo.GetByUsageLogID(c.Request.Context(), id)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "Failed to get request audit")
		return
	}
	if rec == nil {
		response.NotFound(c, "Request audit not found")
		return
	}
	rec = service.SanitizeRequestAuditRecord(rec)
	if rec == nil {
		response.NotFound(c, "Request audit not found")
		return
	}
	response.Success(c, gin.H{
		"usage_log_id":            rec.UsageLogID,
		"headers":                 rec.Headers,
		"events":                  rec.Events,
		"attempts":                rec.Attempts,
		"capture_completeness":    rec.CaptureCompleteness,
		"capture_reason":          rec.CaptureReason,
		"request_fingerprint":     rec.RequestFingerprint,
		"fingerprint_key_version": rec.FingerprintKeyVersion,
		"metadata":                rec.Metadata,
	})
}

// SetRequestAuditValueDetailService 注入值明细接缝（采集与读取共用同一份门控判定）。
//
// 用显式 setter 而不是构造函数参数：既有装配调用点保持不变，nil 时入口按不可用处理。
func (h *UsageHandler) SetRequestAuditValueDetailService(valueDetail *service.RequestAuditValueDetailService) {
	if h == nil {
		return
	}
	h.requestAuditValueDetail = valueDetail
}

// 值明细披露层错误：稳定 reason 码 + 固定文案，供 response.ErrorFrom 输出。
//
// 「没有行」「从未留存」「曾经留存但已不可揭示」「存储不可用」四者必须分开：
// 把它们合并成一个 404 会让管理员无法区分「这个部署没开」与「值已经到期被清掉了」。
var (
	errValueDetailNotFound        = infraerrors.New(http.StatusNotFound, "REQUEST_AUDIT_VALUE_DETAIL_NOT_FOUND", "Request audit value detail not found")
	errValueDetailNotRetained     = infraerrors.New(http.StatusConflict, "REQUEST_AUDIT_VALUE_DETAIL_NOT_RETAINED", "Request audit value details were not retained for this usage log")
	errValueDetailGone            = infraerrors.New(http.StatusGone, "REQUEST_AUDIT_VALUE_DETAIL_GONE", "Retained request audit value details are no longer available")
	errValueDetailUnavailable     = infraerrors.New(http.StatusServiceUnavailable, "REQUEST_AUDIT_VALUE_DETAIL_UNAVAILABLE", "Request audit value details are temporarily unavailable")
	errValueDetailStorageFailed   = infraerrors.New(http.StatusInternalServerError, "REQUEST_AUDIT_VALUE_DETAIL_STORAGE_FAILED", "Failed to read request audit value details")
	errValueDetailSettingsUnavail = infraerrors.New(http.StatusServiceUnavailable, "REQUEST_AUDIT_VALUE_DETAIL_SETTINGS_UNAVAILABLE", "Request audit value detail settings are temporarily unavailable")
	errValueDetailAPIKeyForbidden = infraerrors.Forbidden("REQUEST_AUDIT_VALUE_DETAIL_ADMIN_API_KEY_FORBIDDEN", "enabling request audit value details requires an admin session, not an admin API key")
)

// requestAuditValueDetailDisclosureError 把服务层哨兵映射成对外错误。
//
// 未知错误按 500 处理并给固定文案，不回显内部细节；存储层的哨兵是普通 error，
// 管理员信封需要 {code, reason, message}，因此在这里包装，而不是让 service 依赖 HTTP 错误包。
func requestAuditValueDetailDisclosureError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, service.ErrRequestAuditValueDetailNotFound):
		return errValueDetailNotFound
	case errors.Is(err, service.ErrRequestAuditValueDetailNotRetained):
		return errValueDetailNotRetained
	case errors.Is(err, service.ErrRequestAuditValueDetailGone):
		return errValueDetailGone
	case errors.Is(err, service.ErrRequestAuditValueDetailUnavailable):
		return errValueDetailUnavailable
	default:
		return errValueDetailStorageFailed
	}
}

// GetRequestAuditValueDetail 返回值明细的**信封**，永不返回值本身。
// GET /api/v1/admin/usage/:id/request-audit/value-detail
//
// 默认视图只有「有没有留、为什么没留、还能量多久」：真实值必须由管理员显式 POST 揭示。
// 响应禁止任何中间缓存。
func (h *UsageHandler) GetRequestAuditValueDetail(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.requestAuditValueDetail == nil {
		response.ErrorFrom(c, errValueDetailUnavailable)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid id")
		return
	}
	envelope, err := h.requestAuditValueDetail.GetRequestAuditValueDetail(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, requestAuditValueDetailDisclosureError(err))
		return
	}
	response.Success(c, envelope)
}

// RevealRequestAuditValueDetail 仅在管理员显式请求时返回未过期的值。
// POST /api/v1/admin/usage/:id/request-audit/value-detail
//
// 使用 POST 而非 GET：这是显式的非安全动作，既不会被浏览器／代理预取或缓存，
// 也会被 admin 组的审计中间件记录。响应禁止任何中间缓存。
func (h *UsageHandler) RevealRequestAuditValueDetail(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
	if h == nil || h.requestAuditValueDetail == nil {
		response.ErrorFrom(c, errValueDetailUnavailable)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid id")
		return
	}
	values, err := h.requestAuditValueDetail.RevealRequestAuditValueDetail(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, requestAuditValueDetailDisclosureError(err))
		return
	}
	response.Success(c, service.RequestAuditValueDetailReveal{UsageLogID: id, Values: values})
}

// GetRequestAuditValueDetailSettings 读取值明细的运维开关状态（存量值与校验结论）。
// GET /api/v1/admin/usage/request-audit-value-detail-settings
func (h *UsageHandler) GetRequestAuditValueDetailSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.requestAuditValueDetail == nil {
		response.ErrorFrom(c, errValueDetailSettingsUnavail)
		return
	}
	status, err := h.requestAuditValueDetail.GetRequestAuditValueDetailOperatorStatus(c.Request.Context())
	if err != nil {
		// 读取失败不是「关闭」：显式返回不可用，不假装门控是关的。
		response.ErrorFrom(c, errValueDetailSettingsUnavail)
		return
	}
	response.Success(c, status)
}

// requestAuditValueDetailSettingsRequest 是一次运维开关更新请求。
//
// 这是整体状态更新：未提交的字段按关闭处理，不做隐式保留——开启必须由本次请求显式表达。
type requestAuditValueDetailSettingsRequest struct {
	Enabled  bool   `json:"enabled"`
	Language string `json:"language"`
	Phrase   string `json:"phrase"`
}

// UpdateRequestAuditValueDetailSettings 更新值明细的运维开关。
// PUT /api/v1/admin/usage/request-audit-value-detail-settings
//
// 开启由服务层强制逐字风险确认（每次更新都校验）并记录管理员 ID；
// 关闭不需要确认、不需要操作员身份，也不能被任何前置校验挡住。
func (h *UsageHandler) UpdateRequestAuditValueDetailSettings(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.requestAuditValueDetail == nil {
		response.ErrorFrom(c, errValueDetailSettingsUnavail)
		return
	}
	var req requestAuditValueDetailSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	// 机器凭证可以读、可以关，但不能开：书面确认是不可由常量代替的操作员动作。
	if req.Enabled && c.GetString("auth_method") == service.AuditAuthMethodAdminAPIKey {
		response.ErrorFrom(c, errValueDetailAPIKeyForbidden)
		return
	}
	adminUserID := int64(0)
	if subject, ok := middleware.GetAuthSubjectFromContext(c); ok {
		adminUserID = subject.UserID
	}
	status, err := h.requestAuditValueDetail.UpdateRequestAuditValueDetailOperatorSettings(c.Request.Context(), service.RequestAuditValueDetailOperatorUpdateInput{
		Enabled:     req.Enabled,
		Language:    req.Language,
		Phrase:      req.Phrase,
		AdminUserID: adminUserID,
		IPAddress:   ip.GetClientIP(c),
		UserAgent:   strings.TrimSpace(c.GetHeader("User-Agent")),
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

// CreateUsageCleanupTaskRequest represents cleanup task creation request
type CreateUsageCleanupTaskRequest struct {
	StartDate   string  `json:"start_date"`
	EndDate     string  `json:"end_date"`
	UserID      *int64  `json:"user_id"`
	APIKeyID    *int64  `json:"api_key_id"`
	AccountID   *int64  `json:"account_id"`
	GroupID     *int64  `json:"group_id"`
	Model       *string `json:"model"`
	RequestType *string `json:"request_type"`
	Stream      *bool   `json:"stream"`
	BillingType *int8   `json:"billing_type"`
	Timezone    string  `json:"timezone"`
}

// List handles listing all usage records with filters
// GET /api/v1/admin/usage
func (h *UsageHandler) List(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	exactTotal := false
	if exactTotalRaw := strings.TrimSpace(c.Query("exact_total")); exactTotalRaw != "" {
		parsed, err := strconv.ParseBool(exactTotalRaw)
		if err != nil {
			response.BadRequest(c, "Invalid exact_total value, use true or false")
			return
		}
		exactTotal = parsed
	}

	// Parse filters
	var userID, apiKeyID, accountID, groupID int64
	if userIDStr := c.Query("user_id"); userIDStr != "" {
		id, err := strconv.ParseInt(userIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid user_id")
			return
		}
		userID = id
	}

	if apiKeyIDStr := c.Query("api_key_id"); apiKeyIDStr != "" {
		id, err := strconv.ParseInt(apiKeyIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid api_key_id")
			return
		}
		apiKeyID = id
	}

	if accountIDStr := c.Query("account_id"); accountIDStr != "" {
		id, err := strconv.ParseInt(accountIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid account_id")
			return
		}
		accountID = id
	}

	if groupIDStr := c.Query("group_id"); groupIDStr != "" {
		id, err := strconv.ParseInt(groupIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		groupID = id
	}

	model := c.Query("model")
	requestID := strings.TrimSpace(c.Query("request_id"))
	billingMode := strings.TrimSpace(c.Query("billing_mode"))

	var requestType *int16
	var stream *bool
	if requestTypeStr := strings.TrimSpace(c.Query("request_type")); requestTypeStr != "" {
		parsed, err := service.ParseUsageRequestType(requestTypeStr)
		if err != nil {
			response.BadRequest(c, err.Error())
			return
		}
		value := int16(parsed)
		requestType = &value
	} else if streamStr := c.Query("stream"); streamStr != "" {
		val, err := strconv.ParseBool(streamStr)
		if err != nil {
			response.BadRequest(c, "Invalid stream value, use true or false")
			return
		}
		stream = &val
	}

	nativeCompactionV2, err := parseOptionalBoolDashboardFilter(c, "native_compaction_v2")
	if err != nil {
		response.BadRequest(c, "Invalid native_compaction_v2 value, use true or false")
		return
	}

	var billingType *int8
	if billingTypeStr := c.Query("billing_type"); billingTypeStr != "" {
		val, err := strconv.ParseInt(billingTypeStr, 10, 8)
		if err != nil {
			response.BadRequest(c, "Invalid billing_type")
			return
		}
		bt := int8(val)
		billingType = &bt
	}

	var upstreamModelMismatch *bool
	if raw := strings.TrimSpace(c.Query("upstream_model_mismatch")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			response.BadRequest(c, "Invalid upstream_model_mismatch value, use true or false")
			return
		}
		upstreamModelMismatch = &value
	}

	// Parse date range
	var startTime, endTime *time.Time
	userTZ := c.Query("timezone") // Get user's timezone from request
	if startDateStr := c.Query("start_date"); startDateStr != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", startDateStr, userTZ)
		if err != nil {
			response.BadRequest(c, "Invalid start_date format, use YYYY-MM-DD")
			return
		}
		startTime = &t
	}

	if endDateStr := c.Query("end_date"); endDateStr != "" {
		t, err := timezone.ParseInUserLocation("2006-01-02", endDateStr, userTZ)
		if err != nil {
			response.BadRequest(c, "Invalid end_date format, use YYYY-MM-DD")
			return
		}
		// Use half-open range [start, end), move to next calendar day start (DST-safe).
		t = t.AddDate(0, 0, 1)
		endTime = &t
	}

	params := pagination.PaginationParams{
		Page:      page,
		PageSize:  pageSize,
		SortBy:    c.DefaultQuery("sort_by", "created_at"),
		SortOrder: c.DefaultQuery("sort_order", "desc"),
	}
	filters := usagestats.UsageLogFilters{
		UserID:                userID,
		APIKeyID:              apiKeyID,
		AccountID:             accountID,
		GroupID:               groupID,
		RequestID:             requestID,
		Model:                 model,
		ModelFilterSource:     usagestats.ModelSourceRequested,
		RequestType:           requestType,
		Stream:                stream,
		NativeCompactionV2:    nativeCompactionV2,
		BillingType:           billingType,
		BillingMode:           billingMode,
		UpstreamModelMismatch: upstreamModelMismatch,
		StartTime:             startTime,
		EndTime:               endTime,
		ExactTotal:            exactTotal,
	}

	records, result, err := h.usageService.ListWithFilters(c.Request.Context(), params, filters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]dto.AdminUsageLog, 0, len(records))
	for i := range records {
		out = append(out, *dto.UsageLogFromServiceAdmin(&records[i]))
	}
	response.Paginated(c, out, result.Total, page, pageSize)
}

// Stats handles getting usage statistics with filters
// GET /api/v1/admin/usage/stats
func (h *UsageHandler) Stats(c *gin.Context) {
	// Parse filters - same as List endpoint
	var userID, apiKeyID, accountID, groupID int64
	if userIDStr := c.Query("user_id"); userIDStr != "" {
		id, err := strconv.ParseInt(userIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid user_id")
			return
		}
		userID = id
	}

	if apiKeyIDStr := c.Query("api_key_id"); apiKeyIDStr != "" {
		id, err := strconv.ParseInt(apiKeyIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid api_key_id")
			return
		}
		apiKeyID = id
	}

	if accountIDStr := c.Query("account_id"); accountIDStr != "" {
		id, err := strconv.ParseInt(accountIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid account_id")
			return
		}
		accountID = id
	}

	if groupIDStr := c.Query("group_id"); groupIDStr != "" {
		id, err := strconv.ParseInt(groupIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		groupID = id
	}

	model := c.Query("model")
	billingMode := strings.TrimSpace(c.Query("billing_mode"))

	var requestType *int16
	var stream *bool
	if requestTypeStr := strings.TrimSpace(c.Query("request_type")); requestTypeStr != "" {
		parsed, err := service.ParseUsageRequestType(requestTypeStr)
		if err != nil {
			response.BadRequest(c, err.Error())
			return
		}
		value := int16(parsed)
		requestType = &value
	} else if streamStr := c.Query("stream"); streamStr != "" {
		val, err := strconv.ParseBool(streamStr)
		if err != nil {
			response.BadRequest(c, "Invalid stream value, use true or false")
			return
		}
		stream = &val
	}

	nativeCompactionV2, err := parseOptionalBoolDashboardFilter(c, "native_compaction_v2")
	if err != nil {
		response.BadRequest(c, "Invalid native_compaction_v2 value, use true or false")
		return
	}

	var billingType *int8
	if billingTypeStr := c.Query("billing_type"); billingTypeStr != "" {
		val, err := strconv.ParseInt(billingTypeStr, 10, 8)
		if err != nil {
			response.BadRequest(c, "Invalid billing_type")
			return
		}
		bt := int8(val)
		billingType = &bt
	}

	var upstreamModelMismatch *bool
	if raw := strings.TrimSpace(c.Query("upstream_model_mismatch")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			response.BadRequest(c, "Invalid upstream_model_mismatch value, use true or false")
			return
		}
		upstreamModelMismatch = &value
	}

	// Parse date range
	userTZ := c.Query("timezone")
	now := timezone.NowInUserLocation(userTZ)
	var startTime, endTime time.Time

	startDateStr := c.Query("start_date")
	endDateStr := c.Query("end_date")

	if startDateStr != "" && endDateStr != "" {
		var err error
		startTime, err = timezone.ParseInUserLocation("2006-01-02", startDateStr, userTZ)
		if err != nil {
			response.BadRequest(c, "Invalid start_date format, use YYYY-MM-DD")
			return
		}
		endTime, err = timezone.ParseInUserLocation("2006-01-02", endDateStr, userTZ)
		if err != nil {
			response.BadRequest(c, "Invalid end_date format, use YYYY-MM-DD")
			return
		}
		// 与 SQL 条件 created_at < end 对齐，使用次日 00:00 作为上边界（DST-safe）。
		endTime = endTime.AddDate(0, 0, 1)
	} else {
		period := c.DefaultQuery("period", "today")
		switch period {
		case "today":
			startTime = timezone.StartOfDayInUserLocation(now, userTZ)
		case "week":
			startTime = now.AddDate(0, 0, -7)
		case "month":
			startTime = now.AddDate(0, -1, 0)
		default:
			startTime = timezone.StartOfDayInUserLocation(now, userTZ)
		}
		endTime = now
	}

	// Build filters and call GetStatsWithFilters
	filters := usagestats.UsageLogFilters{
		UserID:                userID,
		APIKeyID:              apiKeyID,
		AccountID:             accountID,
		GroupID:               groupID,
		Model:                 model,
		ModelFilterSource:     usagestats.ModelSourceRequested,
		RequestType:           requestType,
		Stream:                stream,
		NativeCompactionV2:    nativeCompactionV2,
		BillingType:           billingType,
		BillingMode:           billingMode,
		UpstreamModelMismatch: upstreamModelMismatch,
		StartTime:             &startTime,
		EndTime:               &endTime,
	}

	var stats *usagestats.UsageStats
	// nocache: 绕过缓存直接回源,刷新者本人拿最新;不回写缓存(管理台"我刷新我自己拿最新"语义,非全局失效)。
	if parseBoolQueryWithDefault(c.Query("nocache"), false) {
		s, err := h.usageService.GetStatsWithFilters(c.Request.Context(), filters)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		stats = s
		c.Header("X-Usage-Stats-Cache", "bypass")
	} else {
		s, hit, err := h.getStatsCached(c.Request.Context(), filters)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		stats = s
		c.Header("X-Usage-Stats-Cache", cacheStatusValue(hit))
	}

	response.Success(c, stats)
}

// SearchUsers handles searching users by email keyword
// GET /api/v1/admin/usage/search-users
func (h *UsageHandler) SearchUsers(c *gin.Context) {
	keyword := c.Query("q")
	if keyword == "" {
		response.Success(c, []any{})
		return
	}

	// Limit to 30 results
	users, _, err := h.adminService.ListUsers(c.Request.Context(), 1, 30, service.UserListFilters{Search: keyword, IncludeDeleted: true}, "email", "asc")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// Return simplified user list (only id, email and deleted flag)
	type SimpleUser struct {
		ID      int64  `json:"id"`
		Email   string `json:"email"`
		Deleted bool   `json:"deleted"`
	}

	result := make([]SimpleUser, len(users))
	for i, u := range users {
		result[i] = SimpleUser{
			ID:      u.ID,
			Email:   u.Email,
			Deleted: u.DeletedAt != nil,
		}
	}

	response.Success(c, result)
}

// SearchAPIKeys handles searching API keys by user
// GET /api/v1/admin/usage/search-api-keys
func (h *UsageHandler) SearchAPIKeys(c *gin.Context) {
	userIDStr := c.Query("user_id")
	keyword := c.Query("q")

	var userID int64
	if userIDStr != "" {
		id, err := strconv.ParseInt(userIDStr, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid user_id")
			return
		}
		userID = id
	}

	keys, err := h.apiKeyService.SearchAPIKeys(c.Request.Context(), userID, keyword, 30)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// Return simplified API key list (only id and name)
	type SimpleAPIKey struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		UserID int64  `json:"user_id"`
	}

	result := make([]SimpleAPIKey, len(keys))
	for i, k := range keys {
		result[i] = SimpleAPIKey{
			ID:     k.ID,
			Name:   k.Name,
			UserID: k.UserID,
		}
	}

	response.Success(c, result)
}

// ListCleanupTasks handles listing usage cleanup tasks
// GET /api/v1/admin/usage/cleanup-tasks
func (h *UsageHandler) ListCleanupTasks(c *gin.Context) {
	if h.cleanupService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Usage cleanup service unavailable")
		return
	}
	operator := int64(0)
	if subject, ok := middleware.GetAuthSubjectFromContext(c); ok {
		operator = subject.UserID
	}
	page, pageSize := response.ParsePagination(c)
	logger.LegacyPrintf("handler.admin.usage", "[UsageCleanup] 请求清理任务列表: operator=%d page=%d page_size=%d", operator, page, pageSize)
	params := pagination.PaginationParams{Page: page, PageSize: pageSize}
	tasks, result, err := h.cleanupService.ListTasks(c.Request.Context(), params)
	if err != nil {
		logger.LegacyPrintf("handler.admin.usage", "[UsageCleanup] 查询清理任务列表失败: operator=%d page=%d page_size=%d err=%v", operator, page, pageSize, err)
		response.ErrorFrom(c, err)
		return
	}
	out := make([]dto.UsageCleanupTask, 0, len(tasks))
	for i := range tasks {
		out = append(out, *dto.UsageCleanupTaskFromService(&tasks[i]))
	}
	logger.LegacyPrintf("handler.admin.usage", "[UsageCleanup] 返回清理任务列表: operator=%d total=%d items=%d page=%d page_size=%d", operator, result.Total, len(out), page, pageSize)
	response.Paginated(c, out, result.Total, page, pageSize)
}

// CreateCleanupTask handles creating a usage cleanup task
// POST /api/v1/admin/usage/cleanup-tasks
func (h *UsageHandler) CreateCleanupTask(c *gin.Context) {
	if h.cleanupService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Usage cleanup service unavailable")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Unauthorized")
		return
	}

	var req CreateUsageCleanupTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	req.StartDate = strings.TrimSpace(req.StartDate)
	req.EndDate = strings.TrimSpace(req.EndDate)
	if req.StartDate == "" || req.EndDate == "" {
		response.BadRequest(c, "start_date and end_date are required")
		return
	}

	startTime, err := timezone.ParseInUserLocation("2006-01-02", req.StartDate, req.Timezone)
	if err != nil {
		response.BadRequest(c, "Invalid start_date format, use YYYY-MM-DD")
		return
	}
	endTime, err := timezone.ParseInUserLocation("2006-01-02", req.EndDate, req.Timezone)
	if err != nil {
		response.BadRequest(c, "Invalid end_date format, use YYYY-MM-DD")
		return
	}
	endTime = endTime.Add(24*time.Hour - time.Nanosecond)

	var requestType *int16
	stream := req.Stream
	if req.RequestType != nil {
		parsed, err := service.ParseUsageRequestType(*req.RequestType)
		if err != nil {
			response.BadRequest(c, err.Error())
			return
		}
		value := int16(parsed)
		requestType = &value
		stream = nil
	}

	filters := service.UsageCleanupFilters{
		StartTime:   startTime,
		EndTime:     endTime,
		UserID:      req.UserID,
		APIKeyID:    req.APIKeyID,
		AccountID:   req.AccountID,
		GroupID:     req.GroupID,
		Model:       req.Model,
		RequestType: requestType,
		Stream:      stream,
		BillingType: req.BillingType,
	}

	var userID any
	if filters.UserID != nil {
		userID = *filters.UserID
	}
	var apiKeyID any
	if filters.APIKeyID != nil {
		apiKeyID = *filters.APIKeyID
	}
	var accountID any
	if filters.AccountID != nil {
		accountID = *filters.AccountID
	}
	var groupID any
	if filters.GroupID != nil {
		groupID = *filters.GroupID
	}
	var model any
	if filters.Model != nil {
		model = *filters.Model
	}
	var streamValue any
	if filters.Stream != nil {
		streamValue = *filters.Stream
	}
	var requestTypeName any
	if filters.RequestType != nil {
		requestTypeName = service.RequestTypeFromInt16(*filters.RequestType).String()
	}
	var billingType any
	if filters.BillingType != nil {
		billingType = *filters.BillingType
	}

	idempotencyPayload := struct {
		OperatorID int64                         `json:"operator_id"`
		Body       CreateUsageCleanupTaskRequest `json:"body"`
	}{
		OperatorID: subject.UserID,
		Body:       req,
	}
	executeAdminIdempotentJSON(c, "admin.usage.cleanup_tasks.create", idempotencyPayload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		logger.LegacyPrintf("handler.admin.usage", "[UsageCleanup] 请求创建清理任务: operator=%d start=%s end=%s user_id=%v api_key_id=%v account_id=%v group_id=%v model=%v request_type=%v stream=%v billing_type=%v tz=%q",
			subject.UserID,
			filters.StartTime.Format(time.RFC3339),
			filters.EndTime.Format(time.RFC3339),
			userID,
			apiKeyID,
			accountID,
			groupID,
			model,
			requestTypeName,
			streamValue,
			billingType,
			req.Timezone,
		)

		task, err := h.cleanupService.CreateTask(ctx, filters, subject.UserID)
		if err != nil {
			logger.LegacyPrintf("handler.admin.usage", "[UsageCleanup] 创建清理任务失败: operator=%d err=%v", subject.UserID, err)
			return nil, err
		}
		logger.LegacyPrintf("handler.admin.usage", "[UsageCleanup] 清理任务已创建: task=%d operator=%d status=%s", task.ID, subject.UserID, task.Status)
		return dto.UsageCleanupTaskFromService(task), nil
	})
}

// CancelCleanupTask handles canceling a usage cleanup task
// POST /api/v1/admin/usage/cleanup-tasks/:id/cancel
func (h *UsageHandler) CancelCleanupTask(c *gin.Context) {
	if h.cleanupService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Usage cleanup service unavailable")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Unauthorized")
		return
	}
	idStr := strings.TrimSpace(c.Param("id"))
	taskID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || taskID <= 0 {
		response.BadRequest(c, "Invalid task id")
		return
	}
	logger.LegacyPrintf("handler.admin.usage", "[UsageCleanup] 请求取消清理任务: task=%d operator=%d", taskID, subject.UserID)
	if err := h.cleanupService.CancelTask(c.Request.Context(), taskID, subject.UserID); err != nil {
		logger.LegacyPrintf("handler.admin.usage", "[UsageCleanup] 取消清理任务失败: task=%d operator=%d err=%v", taskID, subject.UserID, err)
		response.ErrorFrom(c, err)
		return
	}
	logger.LegacyPrintf("handler.admin.usage", "[UsageCleanup] 清理任务已取消: task=%d operator=%d", taskID, subject.UserID)
	response.Success(c, gin.H{"id": taskID, "status": service.UsageCleanupStatusCanceled})
}
