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
//	GET  /api/v1/admin/error-diagnostics              列表（仅元数据，分页）
//	GET  /api/v1/admin/error-diagnostics/:id          详情（仅元数据，不含正文与头值）
//	POST /api/v1/admin/error-diagnostics/:id/body     显式揭示未过期的出站正文
//	POST /api/v1/admin/error-diagnostics/:id/headers  显式揭示未过期的 429 头值
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
	// ReadErrorDiagnosticHeaderValues 与正文分开：两者是不同的留存事实，
	// 一个读取失败不得被解释成另一个的状态。
	ReadErrorDiagnosticHeaderValues(ctx context.Context, id string) (service.ErrorDiagnosticHeaderValues, error)
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

	// 429 头值（Claude Messages 专属）与正文同构但独立：状态、原因、条数与到期分开披露，
	// 使运维能分辨「没有头值」与「没有正文」，也能看出头值什么时候会读不到。
	HeaderState      string     `json:"header_state"`
	HeaderReason     string     `json:"header_reason"`
	HeaderEntryCount int        `json:"header_entry_count"`
	HeaderExpiresAt  *time.Time `json:"header_expires_at,omitempty"`

	// 留存格式（票据 08／09）：encrypted＝旧密文层（自有七天窗口、需要旧密钥），
	// plaintext＝新明文层（明文落库、随 usage 或三十天、不需要密钥）。
	//
	// 与状态分开披露：只给状态会让「明文留在库里」与「密文等着旧密钥」看起来一样。
	BodyFormat   string `json:"body_format"`
	HeaderFormat string `json:"header_format"`
	// UsageLinked 报告新明文行是否已被可靠关联到一条使用记录。
	//
	// 为真时这一行**没有**自有到期窗口：它随使用记录删除，metadata_expires_at 不再是它的
	// 读取上限（旧列的三十天只是「未关联时」的截止）。界面据此显示真实规则。
	UsageLinked bool `json:"usage_linked"`
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

	// 到期规则随**格式**不同：旧密文是自有七天窗口，新明文是「关联则随 usage，
	// 未关联则三十天整点拒绝」。用旧窗口判新行会让仍可读的明文被拒。
	bodyState := record.BodyState
	bodyReadable := record.BodyReadableAt(now)
	if record.BodyFormat() == service.ErrorDiagnosticFormatPlaintext {
		bodyState = record.PlainBodyState
		bodyReadable = record.PlainBodyReadableAt(now)
	}
	switch bodyState {
	case service.ErrorDiagnosticBodyStateStored:
		// 正文可能在到期时刻与本次读取之间不可读，这里再判一次。
		if !bodyReadable {
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
		// 格式随载荷一起披露：管理员必须知道这份正文是明文留在库里（没有加密保护、
		// 随 usage 或三十天），还是旧密文（需要旧密钥、七天）。
		response.Success(c, gin.H{
			"body_text":    string(body),
			"body_bytes":   len(body),
			"body_format":  discloseErrorDiagnosticFormat(record.BodyFormat()),
			"usage_linked": record.PlainLinked,
		})
	case service.ErrorDiagnosticBodyStateExpired, service.ErrorDiagnosticBodyStatePurged:
		response.ErrorFrom(c, errDiagnosticBodyGone)
	default:
		// not_observed／skipped 以及任何未知状态：一律按「没有可揭示的正文」处理，
		// 绝不在状态未知时声称存在正文。
		response.ErrorFrom(c, errDiagnosticBodyNotRetained)
	}
}

// RevealHeaderValues 仅在被管理员显式请求时返回未过期的 429 头值。
// POST /api/v1/admin/error-diagnostics/:id/headers
//
// 与 RevealBody 同一约定：POST 而非 GET（显式、不可预取、进入 admin 审计中间件），
// 响应禁止任何中间缓存，并显式披露到期时刻。头值与正文是两个独立的揭示动作：
// 揭示正文不会顺带返回头值，反之亦然。
func (h *RequestErrorDiagnosticHandler) RevealHeaderValues(c *gin.Context) {
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

	// 与正文同一约定：头值的到期规则也随格式不同（旧密文七天，新明文随 usage／三十天）。
	headerState := record.HeaderState
	headerReadable := record.HeaderValuesReadableAt(now)
	if record.HeaderFormat() == service.ErrorDiagnosticFormatPlaintext {
		headerState = record.PlainHeaderState
		headerReadable = record.PlainHeaderValuesReadableAt(now)
	}
	switch headerState {
	case service.ErrorDiagnosticHeaderStateStored:
		// 头值可能在到期时刻与本次读取之间不可读，这里再判一次。
		if !headerReadable {
			response.ErrorFrom(c, errDiagnosticHeaderValuesGone)
			return
		}
		values, readErr := h.diagnostics.ReadErrorDiagnosticHeaderValues(c.Request.Context(), id)
		if readErr != nil {
			// 读取期到期／密文已清除与存储不可用要分开：前者是稳定的 410，后者才是可重试的 503。
			if errors.Is(readErr, service.ErrErrorDiagnosticUnavailable) {
				response.ErrorFrom(c, errDiagnosticUnavailable)
				return
			}
			response.ErrorFrom(c, errDiagnosticHeaderValuesGone)
			return
		}
		// 「只含白名单内允许记录的头值」这一披露口径随格式一起给出：快照本身从不断言
		// 「上游只发了这些头」，两种格式下都不放宽这一点。
		headerBytes := record.HeaderBytes
		var headerExpiresAt any = record.HeaderExpiresAt
		if record.HeaderFormat() == service.ErrorDiagnosticFormatPlaintext {
			// 新明文层没有自有七天窗口；不构造假的公元 1 年到期时刻。
			// 未关联时的截止由 metadata_expires_at 表达，已关联时随 usage 删除。
			headerBytes, headerExpiresAt = record.PlainHeaderBytes, nil
		}
		response.Success(c, gin.H{
			"request_headers":    values.Request,
			"response_headers":   values.Response,
			"header_entry_count": values.EntryCount(),
			"header_bytes":       headerBytes,
			"header_expires_at":  headerExpiresAt,
			"header_format":      discloseErrorDiagnosticFormat(record.HeaderFormat()),
			"usage_linked":       record.PlainLinked,
		})
	case service.ErrorDiagnosticHeaderStateExpired, service.ErrorDiagnosticHeaderStatePurged:
		response.ErrorFrom(c, errDiagnosticHeaderValuesGone)
	default:
		// not_observed／skipped 以及任何未知状态：一律按「没有可揭示的头值」处理，
		// 绝不在状态未知时声称存在头值。
		response.ErrorFrom(c, errDiagnosticHeaderValuesNotRetained)
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
	// 新明文行的事实来自明文列，旧行来自密文列：披露必须跟随**格式**，
	// 否则一条明文存活的诊断会显示成「未观察到正文」。
	bodyState, bodyReason := record.BodyState, record.BodyReason
	headerState, headerReason := record.HeaderState, record.HeaderReason
	headerEntryCount := record.HeaderEntryCount
	if record.BodyFormat() == service.ErrorDiagnosticFormatPlaintext {
		bodyState, bodyReason = record.PlainBodyState, record.PlainBodyReason
	}
	if record.HeaderFormat() == service.ErrorDiagnosticFormatPlaintext {
		headerState, headerReason = record.PlainHeaderState, record.PlainHeaderReason
		headerEntryCount = record.PlainHeaderEntryCount
	}
	view := RequestErrorDiagnosticView{
		ID:                record.ID,
		CreatedAt:         record.CreatedAt,
		Protocol:          protocol,
		AttemptIndex:      record.AttemptIndex,
		UpstreamStatus:    record.UpstreamStatusCode,
		BodyState:         discloseErrorDiagnosticBodyState(bodyState),
		Reason:            discloseErrorDiagnosticBodyReason(bodyReason),
		MetadataExpiresAt: record.MetadataExpiresAt,
		HeaderState:       discloseErrorDiagnosticHeaderState(headerState),
		HeaderReason:      discloseErrorDiagnosticHeaderReason(headerReason),
		HeaderEntryCount:  headerEntryCount,
		BodyFormat:        discloseErrorDiagnosticFormat(record.BodyFormat()),
		HeaderFormat:      discloseErrorDiagnosticFormat(record.HeaderFormat()),
		UsageLinked:       record.PlainLinked,
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
	if !record.HeaderExpiresAt.IsZero() {
		// 头值的到期时刻同样必须披露：运维据此知道头值还剩多久可读。
		headerExpiresAt := record.HeaderExpiresAt
		view.HeaderExpiresAt = &headerExpiresAt
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
		service.ErrorDiagnosticPlainBodyRetained,
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

// discloseErrorDiagnosticFormat 只回声两种已知留存格式；未知值按旧密文处理——
// 它是最保守的说法（需要密钥、有七天窗口），不会把未知的行说成「明文可读」。
func discloseErrorDiagnosticFormat(format string) string {
	if format == service.ErrorDiagnosticFormatPlaintext {
		return service.ErrorDiagnosticFormatPlaintext
	}
	return service.ErrorDiagnosticFormatEncrypted
}

// discloseErrorDiagnosticHeaderState 只回声存储层的封闭集合；未知值按最保守的
// not_observed 处理（永不暗示存在可读取的 429 头值）。
func discloseErrorDiagnosticHeaderState(state string) string {
	switch state {
	case service.ErrorDiagnosticHeaderStateStored,
		service.ErrorDiagnosticHeaderStateSkipped,
		service.ErrorDiagnosticHeaderStateExpired,
		service.ErrorDiagnosticHeaderStatePurged:
		return state
	default:
		return service.ErrorDiagnosticHeaderStateNotObserved
	}
}

// discloseErrorDiagnosticHeaderReason 只回声存储层的封闭原因码集合；未知值同样按
// not_observed 处理，不回显任意存储字符串。
func discloseErrorDiagnosticHeaderReason(reason string) string {
	switch reason {
	case service.ErrorDiagnosticHeaderRetained,
		service.ErrorDiagnosticPlainHeaderRetained,
		service.ErrorDiagnosticHeaderSkippedRetentionDisabled,
		service.ErrorDiagnosticHeaderSkippedEncryptionUnavailable,
		service.ErrorDiagnosticHeaderSkippedInvalidValues:
		return reason
	default:
		return service.ErrorDiagnosticHeaderNotObserved
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
	case errors.Is(err, service.ErrErrorDiagnosticHeaderValuesGone):
		return errDiagnosticHeaderValuesGone
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
	// 头值与正文分开成不同的错误码：客户端与运维据此能分辨「没有头值」与「没有正文」。
	errDiagnosticHeaderValuesNotRetained = infraerrors.New(http.StatusConflict, "ERROR_DIAGNOSTIC_HEADER_VALUES_NOT_RETAINED", "429 header values were not retained for this attempt")
	errDiagnosticHeaderValuesGone        = infraerrors.New(http.StatusGone, "ERROR_DIAGNOSTIC_HEADER_VALUES_GONE", "Retained 429 header values are no longer available")
	errDiagnosticUnavailable             = infraerrors.New(http.StatusServiceUnavailable, "ERROR_DIAGNOSTIC_UNAVAILABLE", "Error diagnostics are temporarily unavailable")
	errDiagnosticStorageFailure          = infraerrors.New(http.StatusInternalServerError, "ERROR_DIAGNOSTIC_STORAGE_FAILED", "Failed to read error diagnostics")
)

const (
	errorDiagnosticDefaultPageSize = 20
	errorDiagnosticMaxPageSize     = 100
)
