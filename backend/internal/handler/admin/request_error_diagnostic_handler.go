package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// request_error_diagnostic_handler.go 提供上游错误诊断（每次真实上游 4xx/5xx 尝试的出站正文
// 诊断）的管理员只读入口。
//
// 这是与 usage-owned request audit 完全独立的入口：诊断可以没有 usage_log_id（无 usage 的
// 失败同样可检索），也绝不把模型正文塞进现有审计。列表与详情只返回净化元数据，正文只能由
// 管理员显式 POST 请求返回，且响应禁止任何中间缓存。
//
// 路由（挂在既有 admin 组，由 adminAuth 中间件保证仅管理员可访问；在串行 pass 中注册）：
//
//	GET  /api/v1/admin/error-diagnostics          列表（仅元数据，分页）
//	GET  /api/v1/admin/error-diagnostics/:id      详情（仅元数据，不含正文）
//	POST /api/v1/admin/error-diagnostics/:id/body 显式揭示未过期的出站正文
type RequestErrorDiagnosticHandler struct {
	diagnostics ErrorDiagnosticReader
	// now 可注入，用于在 handler 侧按 7／30 天到期规则做二次校验（存储层已同样拒绝）。
	now func() time.Time
}

// ErrorDiagnosticReader 是本能力读取侧的窄接缝，由 service.ErrorDiagnosticService 直接满足
// （与 UsageHandler 取 service.RequestAuditRepository 的用法一致）。
type ErrorDiagnosticReader interface {
	GetErrorDiagnostic(ctx context.Context, id string) (service.ErrorDiagnosticRecord, error)
	ListRecentErrorDiagnosticPage(ctx context.Context, protocol string, offset, limit int) ([]service.ErrorDiagnosticRecord, error)
	CountRecentErrorDiagnostics(ctx context.Context, protocol string) (int64, error)
	ReadErrorDiagnosticBody(ctx context.Context, id string) ([]byte, error)
}

// RequestErrorDiagnosticView 是单条诊断对外披露的完整白名单：opaque id、时间、协议、
// 尝试序号、上游状态、可选 usage 关联、正文状态与原因、到期时间。
//
// 它就是响应 DTO 本身：类型里没有正文字段，所以「列表与默认详情不返回正文」是类型保证，
// 而不是调用约定；也刻意不含账号／用户／API Key／模型等身份、原始 URL、请求头与错误消息。
type RequestErrorDiagnosticView struct {
	ID                string     `json:"id"`
	CreatedAt         time.Time  `json:"created_at"`
	Protocol          string     `json:"protocol"`
	AttemptIndex      int        `json:"attempt_index"`
	UpstreamStatus    int        `json:"upstream_status"`
	UsageLogID        *int64     `json:"usage_log_id,omitempty"`
	BodyState         string     `json:"body_state"`
	Reason            string     `json:"reason"`
	BodyExpiresAt     *time.Time `json:"body_expires_at,omitempty"`
	MetadataExpiresAt time.Time  `json:"metadata_expires_at"`
}

// NewRequestErrorDiagnosticHandler 构造错误诊断只读处理器。
func NewRequestErrorDiagnosticHandler(diagnostics ErrorDiagnosticReader) *RequestErrorDiagnosticHandler {
	return &RequestErrorDiagnosticHandler{diagnostics: diagnostics, now: time.Now}
}

func (h *RequestErrorDiagnosticHandler) clockNow() time.Time {
	if h == nil || h.now == nil {
		return time.Now()
	}
	return h.now()
}

// List 返回最近错误诊断的净化元数据分页。
// GET /api/v1/admin/error-diagnostics?page=&page_size=
//
// 只接受 page／page_size：不提供账号、用户、Key、模型或关联标识过滤，避免把诊断入口
// 变成按身份检索客户数据的通道；其它查询参数一律忽略。
func (h *RequestErrorDiagnosticHandler) List(c *gin.Context) {
	// 诊断元数据同样不得被中间缓存或预取。
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.diagnostics == nil {
		response.ErrorFrom(c, errDiagnosticUnavailable)
		return
	}
	page, pageSize := response.ParsePagination(c)
	pageSize = errorDiagnosticPageSize(pageSize)
	offset := errorDiagnosticOffset(page, pageSize)

	// ErrorDiagnosticMaxListOffset 只是无界 OFFSET 的扫描放大保护，不是可见窗口：
	// 在其范围内（100_000）深页由存储层按 SQL OFFSET 如实返回。超过该上界时存储层
	// 会夹取到「最后一页」，那会把另一页的数据当成这一页渲染，因此这里显式拒绝，
	// 而不是返回空页或错页。
	if offset > service.ErrorDiagnosticMaxListOffset {
		response.ErrorFrom(c, errDiagnosticPageOutOfRange)
		return
	}

	// 计数与列表使用同一谓词（未过第 30 天）。计数失败时返回真实错误，
	// 绝不用 0 或 len(items) 冒充总数。
	total, err := h.diagnostics.CountRecentErrorDiagnostics(c.Request.Context(), "")
	if err != nil {
		response.ErrorFrom(c, errorDiagnosticDisclosureError(err))
		return
	}

	now := h.clockNow()
	records, err := h.diagnostics.ListRecentErrorDiagnosticPage(c.Request.Context(), "", offset, pageSize)
	if err != nil {
		response.ErrorFrom(c, errorDiagnosticDisclosureError(err))
		return
	}

	items := make([]RequestErrorDiagnosticView, 0, pageSize)
	for i := range records {
		// 到期元数据不再披露；存储层同样拒绝，这里是二次校验。
		if records[i].ExpiredAt(now) {
			continue
		}
		view, ok := discloseErrorDiagnosticRecord(records[i])
		if !ok {
			continue
		}
		items = append(items, view)
	}

	response.Paginated(c, items, total, page, pageSize)
}

// Get 返回单条诊断的净化元数据，永不包含正文。
// GET /api/v1/admin/error-diagnostics/:id
//
// 未知 id、形状非法的 id 与元数据已过 30 天到期者返回同一个 404，不区分「不存在」「越权」
// 与「已到期」，避免成为存在性探针。
func (h *RequestErrorDiagnosticHandler) Get(c *gin.Context) {
	// 详情同样不得被缓存：诊断元数据会随到期与清理变化。
	c.Header("Cache-Control", "no-store, private")
	if h == nil || h.diagnostics == nil {
		response.ErrorFrom(c, errDiagnosticUnavailable)
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if !service.ValidErrorDiagnosticID(id) {
		response.ErrorFrom(c, errDiagnosticNotFound)
		return
	}
	record, err := h.diagnostics.GetErrorDiagnostic(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, errorDiagnosticDisclosureError(err))
		return
	}
	if record.ExpiredAt(h.clockNow()) {
		response.ErrorFrom(c, errDiagnosticNotFound)
		return
	}
	view, ok := discloseErrorDiagnosticRecord(record)
	if !ok {
		response.ErrorFrom(c, errDiagnosticNotFound)
		return
	}
	response.Success(c, view)
}

// RevealBody 仅在被管理员显式请求时返回未过期的出站正文。
// POST /api/v1/admin/error-diagnostics/:id/body
//
// 使用 POST 而非 GET：这是显式的非安全动作，既不会被浏览器／代理预取或缓存，也会被
// admin 组的审计中间件记录（GET 需要额外白名单才留痕）。响应禁止任何中间缓存。
func (h *RequestErrorDiagnosticHandler) RevealBody(c *gin.Context) {
	// 任何分支（含错误）都不得被缓存。
	c.Header("Cache-Control", "no-store, private")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")

	if h == nil || h.diagnostics == nil {
		response.ErrorFrom(c, errDiagnosticUnavailable)
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if !service.ValidErrorDiagnosticID(id) {
		response.ErrorFrom(c, errDiagnosticNotFound)
		return
	}

	record, err := h.diagnostics.GetErrorDiagnostic(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, errorDiagnosticDisclosureError(err))
		return
	}
	now := h.clockNow()
	if record.ExpiredAt(now) {
		response.ErrorFrom(c, errDiagnosticNotFound)
		return
	}

	switch record.BodyState {
	case service.ErrorDiagnosticBodyStateStored:
		// 正文可能在第 7 天后的清理与本次读取之间到期，这里再判一次。
		if !record.BodyReadableAt(now) {
			response.ErrorFrom(c, errDiagnosticBodyGone)
			return
		}
		body, readErr := h.diagnostics.ReadErrorDiagnosticBody(c.Request.Context(), id)
		if readErr != nil {
			// 读取期到期／密文已清除与存储不可用要分开：前者是稳定的 410，
			// 后者才是可重试的 503，不把「缺密钥」与「存储故障」混为一谈。
			if errors.Is(readErr, service.ErrErrorDiagnosticUnavailable) {
				response.ErrorFrom(c, errDiagnosticUnavailable)
				return
			}
			response.ErrorFrom(c, errDiagnosticBodyGone)
			return
		}
		response.Success(c, gin.H{
			"body_text":  string(body),
			"body_bytes": len(body),
		})
	case service.ErrorDiagnosticBodyStateExpired, service.ErrorDiagnosticBodyStatePurged:
		response.ErrorFrom(c, errDiagnosticBodyGone)
	default:
		// not_observed／skipped 以及任何未知状态：一律按「没有可揭示的正文」处理，
		// 绝不在状态未知时声称存在正文。
		response.ErrorFrom(c, errDiagnosticBodyNotRetained)
	}
}

// discloseErrorDiagnosticRecord 把存储记录映射成披露白名单。
//
// 第二个返回值为 false 表示该记录不应披露（协议不在已覆盖的三个分支内，属于存储异常），
// 由调用方按「不存在」处理，而不是临时造一个新的枚举值。
func discloseErrorDiagnosticRecord(record service.ErrorDiagnosticRecord) (RequestErrorDiagnosticView, bool) {
	protocol, ok := discloseErrorDiagnosticProtocol(record.Protocol)
	if !ok {
		return RequestErrorDiagnosticView{}, false
	}
	view := RequestErrorDiagnosticView{
		ID:                record.ID,
		CreatedAt:         record.CreatedAt,
		Protocol:          protocol,
		AttemptIndex:      record.AttemptIndex,
		UpstreamStatus:    record.UpstreamStatusCode,
		BodyState:         discloseErrorDiagnosticBodyState(record.BodyState),
		Reason:            discloseErrorDiagnosticBodyReason(record.BodyReason),
		MetadataExpiresAt: record.MetadataExpiresAt,
	}
	// HasUsage 才是「有关联使用记录」的事实，绝不从 UsageLogID == 0 反推。
	if record.HasUsage && record.UsageLogID > 0 {
		usageLogID := record.UsageLogID
		view.UsageLogID = &usageLogID
	}
	if !record.BodyExpiresAt.IsZero() {
		bodyExpiresAt := record.BodyExpiresAt
		view.BodyExpiresAt = &bodyExpiresAt
	}
	return view, true
}

func discloseErrorDiagnosticProtocol(protocol string) (string, bool) {
	switch protocol {
	case service.ErrorDiagnosticProtocolMessages,
		service.ErrorDiagnosticProtocolChatCompletions,
		service.ErrorDiagnosticProtocolResponses:
		return protocol, true
	default:
		return "", false
	}
}

// discloseErrorDiagnosticBodyState 只回声存储层的封闭集合；未知值按最保守的
// not_observed 处理（永不暗示存在可读取的正文）。
func discloseErrorDiagnosticBodyState(state string) string {
	switch state {
	case service.ErrorDiagnosticBodyStateStored,
		service.ErrorDiagnosticBodyStateSkipped,
		service.ErrorDiagnosticBodyStateExpired,
		service.ErrorDiagnosticBodyStatePurged:
		return state
	default:
		return service.ErrorDiagnosticBodyStateNotObserved
	}
}

// discloseErrorDiagnosticBodyReason 只回声存储层的封闭原因码集合；未知值同样按
// not_observed 处理，不回显任意存储字符串。
func discloseErrorDiagnosticBodyReason(reason string) string {
	switch reason {
	case service.ErrorDiagnosticBodyRetained,
		service.ErrorDiagnosticBodySkippedNotTextJSON,
		service.ErrorDiagnosticBodySkippedTooLarge,
		service.ErrorDiagnosticBodySkippedAttachment,
		service.ErrorDiagnosticBodySkippedKnownCredential,
		service.ErrorDiagnosticBodySkippedIncompleteRead,
		service.ErrorDiagnosticBodySkippedEncryptionUnavailable,
		service.ErrorDiagnosticBodySkippedRetentionDisabled:
		return reason
	default:
		return service.ErrorDiagnosticBodyNotObserved
	}
}

// errorDiagnosticPageSize 收敛 page_size 到本入口的上限（100，小于存储层窗口上限），
// 避免一次拉取超过存储层的最近窗口。
func errorDiagnosticPageSize(pageSize int) int {
	if pageSize < 1 {
		return errorDiagnosticDefaultPageSize
	}
	if pageSize > errorDiagnosticMaxPageSize {
		return errorDiagnosticMaxPageSize
	}
	return pageSize
}

// errorDiagnosticOffset 把页码折算成 SQL 偏移。
//
// page 由查询串解析而来，可能是任意正整数，因此先把页码收敛到上界之上的「必然越界」
// 值再相乘，避免 (page-1)*pageSize 溢出成一个负数——那会让越界页码看起来像合法偏移。
func errorDiagnosticOffset(page, pageSize int) int {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = errorDiagnosticDefaultPageSize
	}
	// 上界之上刻意留出 2 页，保证夹取后的乘积仍大于 ErrorDiagnosticMaxListOffset。
	if maxPage := service.ErrorDiagnosticMaxListOffset/pageSize + 2; page > maxPage {
		page = maxPage
	}
	return (page - 1) * pageSize
}

// errorDiagnosticDisclosureError 把存储层错误映射成对外错误。
//
// 存储层的哨兵是普通 error；管理员信封需要 {code, reason, message}，因此在这里包装，
// 而不是让 service 依赖 HTTP 错误包。未知错误按 500 处理并给固定文案，不回显内部细节。
func errorDiagnosticDisclosureError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, service.ErrErrorDiagnosticNotFound),
		errors.Is(err, service.ErrErrorDiagnosticInvalidAttempt),
		errors.Is(err, service.ErrErrorDiagnosticDisabled):
		return errDiagnosticNotFound
	case errors.Is(err, service.ErrErrorDiagnosticBodyGone):
		return errDiagnosticBodyGone
	case errors.Is(err, service.ErrErrorDiagnosticUnavailable):
		return errDiagnosticUnavailable
	default:
		return errDiagnosticStorageFailure
	}
}

// 披露层错误：稳定 reason 码 + 固定文案，供 response.ErrorFrom 输出。
var (
	errDiagnosticNotFound        = infraerrors.New(http.StatusNotFound, "ERROR_DIAGNOSTIC_NOT_FOUND", "Error diagnostic not found or expired")
	errDiagnosticPageOutOfRange  = infraerrors.New(http.StatusBadRequest, "ERROR_DIAGNOSTIC_PAGE_OUT_OF_RANGE", "Requested page is beyond the supported error diagnostic window")
	errDiagnosticBodyNotRetained = infraerrors.New(http.StatusConflict, "ERROR_DIAGNOSTIC_BODY_NOT_RETAINED", "Request body was not retained for this attempt")
	errDiagnosticBodyGone        = infraerrors.New(http.StatusGone, "ERROR_DIAGNOSTIC_BODY_GONE", "Retained request body is no longer available")
	errDiagnosticUnavailable     = infraerrors.New(http.StatusServiceUnavailable, "ERROR_DIAGNOSTIC_UNAVAILABLE", "Error diagnostics are temporarily unavailable")
	errDiagnosticStorageFailure  = infraerrors.New(http.StatusInternalServerError, "ERROR_DIAGNOSTIC_STORAGE_FAILED", "Failed to read error diagnostics")
)

const (
	errorDiagnosticDefaultPageSize = 20
	errorDiagnosticMaxPageSize     = 100
)
