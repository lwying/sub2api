package admin

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ticket09 管理员后台批量导出 Trace 的 HTTP 入口。
//
// 信任边界：只有管理员登录会话（JWT 认证方式 + 认证主体 + 绑定的会话 ID）可以创建、
// 查看和下载导出任务。管理员 API Key、普通用户和其它管理员会话一律被拒，且不做任何
// 身份回退。这里不签发任何公开链接，响应与下载均禁止中间缓存。
//
// 该 handler 只做参数解析、身份门禁和错误映射；分页读取、跳过的删除记录计数、
// 容量预算与文件生命周期都由 service.RequestTraceExportService 负责。

var requestTraceExportIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// requestTraceExportRouteFamilies 与 service 的过滤器取值保持一致。
// handler 先做一次显式校验，非法筛选返回 400，而不是把服务的容量超限错误误报成 429。
var requestTraceExportRouteFamilies = map[string]struct{}{
	"messages": {}, "chat_completions": {}, "responses": {},
}

// HTTP 层错误：消息固定且不含任何 DB／正文／路径细节，响应体只暴露稳定的 reason。
var (
	errRequestTraceExportDisabled        = infraerrors.ServiceUnavailable("REQUEST_TRACE_EXPORT_DISABLED", "request trace export is not enabled on this instance")
	errRequestTraceExportSessionRequired = infraerrors.Forbidden("REQUEST_TRACE_EXPORT_ADMIN_SESSION_REQUIRED", "an admin login session is required to export request traces")
	errRequestTraceExportForbidden       = infraerrors.Forbidden("REQUEST_TRACE_EXPORT_FORBIDDEN", "this request trace export belongs to another admin session")
	errRequestTraceExportNotFound        = infraerrors.NotFound("REQUEST_TRACE_EXPORT_NOT_FOUND", "request trace export task not found")
	errRequestTraceExportLimit           = infraerrors.TooManyRequests("REQUEST_TRACE_EXPORT_CAPACITY_LIMIT", "request trace export capacity limit reached")
	errRequestTraceExportUnavailable     = infraerrors.ServiceUnavailable("REQUEST_TRACE_EXPORT_UNAVAILABLE", "request trace export is temporarily unavailable")
	errRequestTraceExportInvalidFilter   = infraerrors.BadRequest("REQUEST_TRACE_EXPORT_INVALID_FILTER", "invalid request trace export filter")
	// 410：任务存在且属于当前会话，但文件已过期或已从本机临时目录丢失。
	errRequestTraceExportExpired  = infraerrors.New(http.StatusGone, "REQUEST_TRACE_EXPORT_DOWNLOAD_EXPIRED", "request trace export download window has ended")
	errRequestTraceExportFileLost = infraerrors.New(http.StatusGone, "REQUEST_TRACE_EXPORT_FILE_LOST", "request trace export file is no longer available")
)

type requestTraceExportService interface {
	CreateTask(ctx context.Context, actor service.RequestTraceExportActor, filter service.RequestTraceExportFilter) (service.RequestTraceExportTask, error)
	GetTask(ctx context.Context, actor service.RequestTraceExportActor, id string) (service.RequestTraceExportTask, error)
	OpenDownload(ctx context.Context, actor service.RequestTraceExportActor, id string) (*os.File, service.RequestTraceExportTask, error)
}

type RequestTraceExportHandler struct {
	service requestTraceExportService
}

func NewRequestTraceExportHandler(svc requestTraceExportService) *RequestTraceExportHandler {
	return &RequestTraceExportHandler{service: svc}
}

// requestTraceExportTaskView 是任务对外视图：只有元数据与计数。
// 生成实例、会话摘要、文件名／路径一律不出响应，也不提供任何下载 URL——
// 客户端自行按会话内路由拼下载地址，因此不存在可长期复用的公开链接。
type requestTraceExportTaskView struct {
	ID            string                           `json:"id"`
	Status        service.RequestTraceExportStatus `json:"status"`
	Filter        service.RequestTraceExportFilter `json:"filter"`
	RowsExported  int64                            `json:"rows_exported"`
	RowsSkipped   int64                            `json:"rows_skipped"`
	BytesExported int64                            `json:"bytes_exported"`
	CreatedAt     time.Time                        `json:"created_at"`
	CompletedAt   *time.Time                       `json:"completed_at,omitempty"`
	DownloadUntil *time.Time                       `json:"download_until,omitempty"`
	Downloadable  bool                             `json:"downloadable"`
}

func newRequestTraceExportTaskView(task service.RequestTraceExportTask, now time.Time) requestTraceExportTaskView {
	return requestTraceExportTaskView{
		ID:            task.ID,
		Status:        task.Status,
		Filter:        task.Filter,
		RowsExported:  task.RowsExported,
		RowsSkipped:   task.RowsSkipped,
		BytesExported: task.BytesExported,
		CreatedAt:     task.CreatedAt,
		CompletedAt:   task.CompletedAt,
		DownloadUntil: task.DownloadUntil,
		Downloadable: task.Status == service.RequestTraceExportCompleted &&
			task.CompletedAt != nil && task.DownloadUntil != nil && now.Before(*task.DownloadUntil),
	}
}

// Create 创建有界后台导出任务，返回 202 与任务元数据。
func (h *RequestTraceExportHandler) Create(c *gin.Context) {
	setRequestTraceExportHeaders(c)
	actor, ok := h.sessionActor(c)
	if !ok {
		return
	}
	if h == nil || h.service == nil {
		response.ErrorFrom(c, errRequestTraceExportUnavailable)
		setRequestTraceExportAudit(c, "failed", "request_trace_export_unavailable", nil)
		return
	}
	filter, ok := parseRequestTraceExportFilter(c)
	if !ok {
		setRequestTraceExportAudit(c, "failed", "request_trace_export_invalid_filter", nil)
		response.ErrorFrom(c, errRequestTraceExportInvalidFilter)
		return
	}
	task, err := h.service.CreateTask(c.Request.Context(), actor, filter)
	if err != nil {
		appErr := requestTraceExportError(err)
		response.ErrorFrom(c, appErr)
		setRequestTraceExportAudit(c, "failed", strings.ToLower(appErr.Reason), nil)
		return
	}
	response.Accepted(c, newRequestTraceExportTaskView(task, time.Now().UTC()))
	setRequestTraceExportAudit(c, "success", "", &task)
}

// Get 只返回发起该任务的管理员会话可见的任务状态。
func (h *RequestTraceExportHandler) Get(c *gin.Context) {
	setRequestTraceExportHeaders(c)
	actor, ok := h.sessionActor(c)
	if !ok {
		return
	}
	if h == nil || h.service == nil {
		response.ErrorFrom(c, errRequestTraceExportUnavailable)
		setRequestTraceExportAudit(c, "failed", "request_trace_export_unavailable", nil)
		return
	}
	task, err := h.service.GetTask(c.Request.Context(), actor, requestTraceExportTaskID(c))
	if err != nil {
		appErr := requestTraceExportError(err)
		response.ErrorFrom(c, appErr)
		setRequestTraceExportAudit(c, "failed", strings.ToLower(appErr.Reason), nil)
		return
	}
	response.Success(c, newRequestTraceExportTaskView(task, time.Now().UTC()))
	setRequestTraceExportAudit(c, "success", "", &task)
}

// Download 以流式、不可缓存的方式返回已完成任务的明文文件。
// 已过七天窗口或本机文件丢失时返回 410，不伪造任何替代内容。
func (h *RequestTraceExportHandler) Download(c *gin.Context) {
	setRequestTraceExportHeaders(c)
	actor, ok := h.sessionActor(c)
	if !ok {
		return
	}
	if h == nil || h.service == nil {
		response.ErrorFrom(c, errRequestTraceExportUnavailable)
		setRequestTraceExportAudit(c, "failed", "request_trace_export_unavailable", nil)
		return
	}
	file, task, err := h.service.OpenDownload(c.Request.Context(), actor, requestTraceExportTaskID(c))
	if err != nil {
		appErr := requestTraceExportError(err)
		response.ErrorFrom(c, appErr)
		setRequestTraceExportAudit(c, "failed", strings.ToLower(appErr.Reason), nil)
		return
	}
	defer func() { _ = file.Close() }()

	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Content-Disposition", "attachment; filename=\""+requestTraceExportDownloadFilename(file)+"\"")
	if info, statErr := file.Stat(); statErr == nil && info.Mode().IsRegular() && info.Size() >= 0 {
		c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
	}
	c.Status(http.StatusOK)
	if streamErr := service.ConsumeExport(c.Writer, file); streamErr != nil {
		// 响应头已发出，只能留下不含内容的失败审计，不能改写状态码。
		setRequestTraceExportAudit(c, "failed", "request_trace_export_stream_interrupted", &task)
		return
	}
	setRequestTraceExportAudit(c, "success", "", &task)
}

// sessionActor 实施 ticket09 的信任边界：必须是管理员登录会话。
// 缺失任一项即拒绝，不退化到 API Key、普通用户或任何其它身份来源；
// 被拒的尝试同样留下不含内容的失败审计。
func (h *RequestTraceExportHandler) sessionActor(c *gin.Context) (service.RequestTraceExportActor, bool) {
	reject := func() (service.RequestTraceExportActor, bool) {
		response.ErrorFrom(c, errRequestTraceExportSessionRequired)
		setRequestTraceExportAudit(c, "failed", "request_trace_export_admin_session_required", nil)
		return service.RequestTraceExportActor{}, false
	}
	if c.GetString("auth_method") != service.AuditAuthMethodJWT {
		return reject()
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		return reject()
	}
	sessionID := strings.TrimSpace(c.GetString(middleware.ContextKeySessionID))
	if sessionID == "" {
		return reject()
	}
	return service.RequestTraceExportActor{AdminUserID: subject.UserID, SessionID: sessionID}, true
}

// parseRequestTraceExportFilter 只接受 Trace 元数据筛选：
// trace_id / route_family / client_status / created_from / created_to / usage_linked。
// 不接受任何正文、SQL 片段或自由文本；非法取值一律 400。
func parseRequestTraceExportFilter(c *gin.Context) (service.RequestTraceExportFilter, bool) {
	filter := service.RequestTraceExportFilter{}
	if raw := strings.TrimSpace(c.Query("trace_id")); raw != "" {
		if !requestTraceExportIDPattern.MatchString(raw) {
			return service.RequestTraceExportFilter{}, false
		}
		filter.TraceID = raw
	}
	if raw := strings.TrimSpace(c.Query("route_family")); raw != "" {
		if _, ok := requestTraceExportRouteFamilies[raw]; !ok {
			return service.RequestTraceExportFilter{}, false
		}
		filter.RouteFamily = raw
	}
	if raw := strings.TrimSpace(c.Query("client_status")); raw != "" {
		status, err := strconv.Atoi(raw)
		if err != nil || status < 0 || status > 599 {
			return service.RequestTraceExportFilter{}, false
		}
		filter.ClientStatus = status
	}
	if raw := strings.TrimSpace(c.Query("usage_linked")); raw != "" {
		linked, err := strconv.ParseBool(raw)
		if err != nil {
			return service.RequestTraceExportFilter{}, false
		}
		filter.UsageLinked = &linked
	}
	for _, entry := range []struct {
		key string
		out **time.Time
	}{{"created_from", &filter.CreatedFrom}, {"created_to", &filter.CreatedTo}} {
		parsed, ok := parseRequestTraceExportTime(c, entry.key)
		if !ok {
			return service.RequestTraceExportFilter{}, false
		}
		*entry.out = parsed
	}
	if filter.CreatedFrom != nil && filter.CreatedTo != nil && !filter.CreatedFrom.Before(*filter.CreatedTo) {
		return service.RequestTraceExportFilter{}, false
	}
	return filter, true
}

// parseRequestTraceExportTime 解析 RFC3339Nano 时间筛选项；缺省时返回 nil。
func parseRequestTraceExportTime(c *gin.Context, key string) (*time.Time, bool) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return nil, true
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, false
	}
	return &parsed, true
}

// requestTraceExportError 把服务哨兵错误映射为稳定的 HTTP 状态。
// 未识别的错误（含任何 DB／文件系统错误）一律折叠为 503，绝不外泄原始错误文本。
func requestTraceExportError(err error) *infraerrors.ApplicationError {
	switch {
	case errors.Is(err, service.ErrRequestTraceExportDisabled):
		return errRequestTraceExportDisabled
	case errors.Is(err, service.ErrRequestTraceExportSessionRequired):
		return errRequestTraceExportSessionRequired
	case errors.Is(err, service.ErrRequestTraceExportForbidden):
		return errRequestTraceExportForbidden
	case errors.Is(err, service.ErrRequestTraceExportNotFound):
		return errRequestTraceExportNotFound
	case errors.Is(err, service.ErrRequestTraceExportGone):
		return errRequestTraceExportExpired
	case errors.Is(err, service.ErrRequestTraceExportFileLost):
		return errRequestTraceExportFileLost
	case errors.Is(err, service.ErrRequestTraceExportLimit):
		return errRequestTraceExportLimit
	case errors.Is(err, service.ErrRequestTraceExportUnavailable):
		return errRequestTraceExportUnavailable
	default:
		return errRequestTraceExportUnavailable
	}
}

func setRequestTraceExportHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
}

// requestTraceExportTaskID 兼容 :id 与 :task_id 两种路由参数命名，
// 避免该 handler 被换绑到另一种参数名时静默失效。
func requestTraceExportTaskID(c *gin.Context) string {
	for _, key := range []string{"id", "task_id"} {
		if value := strings.TrimSpace(c.Param(key)); value != "" {
			return value
		}
	}
	return ""
}

// requestTraceExportDownloadFilename 只用服务已校验过的文件名，并剥离任何
// 可能进入响应头的引号／换行，文件名不携带任何用户数据。
func requestTraceExportDownloadFilename(file *os.File) string {
	name := ""
	if file != nil {
		name = filepath.Base(file.Name())
	}
	if name == "" || name == "." || name == string(filepath.Separator) ||
		strings.ContainsAny(name, "\"\\\r\n") {
		return "request-trace-export.jsonl"
	}
	return name
}

// setRequestTraceExportAudit 只写标量、非敏感的操作摘要。
// 未被中间件 allowlist 放行的键会被静默丢弃，因此这里不可能带出正文、
// 头值、query 原值或自由文本。审计在响应写出后调用，http_status 反映最终状态码。
func setRequestTraceExportAudit(c *gin.Context, result, errorCode string, task *service.RequestTraceExportTask) {
	fields := map[string]any{"result": result}
	if errorCode != "" {
		fields["error_code"] = errorCode
	}
	if status := c.Writer.Status(); status > 0 {
		fields["http_status"] = status
	}
	if task != nil {
		fields["export_task_id"] = task.ID
		fields["rows_exported"] = task.RowsExported
		fields["rows_skipped"] = task.RowsSkipped
		fields["bytes_exported"] = task.BytesExported
	}
	middleware.SetAuditExtra(c, fields)
}
