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
	// 400：明文导出需要管理员接受当前版本的导出风险声明，缺确认不是"服务暂时不可用"。
	errRequestTraceExportRiskAckRequired = infraerrors.BadRequest("REQUEST_TRACE_EXPORT_RISK_ACK_REQUIRED", "creating a plaintext export requires the written export risk acknowledgement")
	// 409：任务尚未产出可下载的文件。它与 410"已过期"是不同事实，轮询方据此决定重试。
	errRequestTraceExportNotReady = infraerrors.Conflict("REQUEST_TRACE_EXPORT_NOT_READY", "request trace export has not produced a downloadable file yet")
)

type requestTraceExportService interface {
	CreateTask(ctx context.Context, actor service.RequestTraceExportActor, filter service.RequestTraceExportFilter) (service.RequestTraceExportTask, error)
	GetTask(ctx context.Context, actor service.RequestTraceExportActor, id string) (service.RequestTraceExportTask, error)
	OpenDownload(ctx context.Context, actor service.RequestTraceExportActor, id string) (*os.File, service.RequestTraceExportTask, error)
	// OpenShardDownload 打开本次导出的第 ordinal 个分片（从 1 开始）。
	OpenShardDownload(ctx context.Context, actor service.RequestTraceExportActor, id string, ordinal int) (*os.File, service.RequestTraceExportShard, service.RequestTraceExportTask, error)
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
	// ShardCount 是本次导出实际生成的分片数；客户端按序号逐个下载。
	ShardCount int `json:"shard_count"`
	// Truncated / IncompleteReason 说明结果是否完整。管理端必须如实呈现，
	// 不能把不完整的导出当作全量结果。
	Truncated        bool   `json:"truncated"`
	IncompleteReason string `json:"incomplete_reason,omitempty"`
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
		ShardCount:       len(task.Shards),
		Truncated:        task.Truncated,
		IncompleteReason: task.IncompleteReason,
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
		response.ErrorFrom(c, errRequestTraceExportInvalidFilter)
		// 审计必须在响应写出后调用，否则 http_status 会记成默认的 200。
		setRequestTraceExportAudit(c, "failed", "request_trace_export_invalid_filter", nil)
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
	// 无 part 参数时下载清单（这次导出的范围与完整性说明）；带 part 时下载对应分片。
	// 两者都按任务 ID 与创建会话校验，序号越界一律拒绝。
	var file *os.File
	var task service.RequestTraceExportTask
	var err error
	rawPart := strings.TrimSpace(c.Query("part"))
	if rawPart == "" {
		file, task, err = h.service.OpenDownload(c.Request.Context(), actor, requestTraceExportTaskID(c))
	} else {
		ordinal, convErr := strconv.Atoi(rawPart)
		if convErr != nil {
			// 非法序号是筛选/参数错误（400），不是"容量达到上限"。
			response.ErrorFrom(c, errRequestTraceExportInvalidFilter)
			setRequestTraceExportAudit(c, "failed", "request_trace_export_invalid_shard", nil)
			return
		}
		file, _, task, err = h.service.OpenShardDownload(c.Request.Context(), actor, requestTraceExportTaskID(c), ordinal)
	}
	if err != nil {
		appErr := requestTraceExportError(err)
		response.ErrorFrom(c, appErr)
		setRequestTraceExportAudit(c, "failed", strings.ToLower(appErr.Reason), nil)
		return
	}
	defer func() { _ = file.Close() }()

	// 清单是 JSON，分片是 JSONL：按真实交付物给出正确的 Content-Type。
	// file 为 nil 只可能出现在未接线的测试替身上，此处不做任何解引用。
	contentType := "application/x-ndjson"
	if file != nil && strings.HasSuffix(file.Name(), ".json") {
		contentType = "application/json"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", "attachment; filename=\""+requestTraceExportDownloadFilename(file)+"\"")
	if file != nil {
		if info, statErr := file.Stat(); statErr == nil && info.Mode().IsRegular() && info.Size() >= 0 {
			c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
		}
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
		filter.ClientStatus = &status
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
	// 可选检索 ID：出现时必须是正整数，非法值直接拒绝而不是退化成"导出全部"。
	for _, entry := range []struct {
		key string
		out **int64
	}{{"usage_log_id", &filter.UsageLogID}, {"account_id", &filter.AccountID}} {
		raw := strings.TrimSpace(c.Query(entry.key))
		if raw == "" {
			continue
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return service.RequestTraceExportFilter{}, false
		}
		value := id
		*entry.out = &value
	}
	// 分组：具体 ID 与"未知"互斥。
	rawGroupID := strings.TrimSpace(c.Query("group_id"))
	rawGroupUnknown := strings.TrimSpace(c.Query("group_unknown"))
	if rawGroupID != "" && rawGroupUnknown != "" {
		return service.RequestTraceExportFilter{}, false
	}
	if rawGroupID != "" {
		id, err := strconv.ParseInt(rawGroupID, 10, 64)
		if err != nil || id <= 0 {
			return service.RequestTraceExportFilter{}, false
		}
		value := id
		filter.GroupID = &value
	} else if rawGroupUnknown != "" {
		unknown, err := strconv.ParseBool(rawGroupUnknown)
		if err != nil {
			return service.RequestTraceExportFilter{}, false
		}
		filter.GroupUnknown = &unknown
	}
	// 客户端请求模型：具体名称与"未知"互斥。
	filter.RequestedModel = strings.TrimSpace(c.Query("requested_model"))
	rawModelUnknown := strings.TrimSpace(c.Query("model_unknown"))
	if filter.RequestedModel != "" && rawModelUnknown != "" {
		return service.RequestTraceExportFilter{}, false
	}
	if rawModelUnknown != "" {
		unknown, err := strconv.ParseBool(rawModelUnknown)
		if err != nil {
			return service.RequestTraceExportFilter{}, false
		}
		filter.ModelUnknown = &unknown
	}
	// 上游账号平台：具体名称与"未知"互斥。
	filter.Platform = strings.TrimSpace(c.Query("platform"))
	rawPlatformUnknown := strings.TrimSpace(c.Query("platform_unknown"))
	if filter.Platform != "" && rawPlatformUnknown != "" {
		return service.RequestTraceExportFilter{}, false
	}
	if rawPlatformUnknown != "" {
		unknown, err := strconv.ParseBool(rawPlatformUnknown)
		if err != nil {
			return service.RequestTraceExportFilter{}, false
		}
		filter.PlatformUnknown = &unknown
	}
	// 导出所选：有界的明确 Trace ID 集合，逗号分隔；与其它条件互斥。
	if raw := strings.TrimSpace(c.Query("trace_ids")); raw != "" {
		parts := strings.Split(raw, ",")
		if len(parts) > service.RequestTraceExportMaxSelectedIDs {
			return service.RequestTraceExportFilter{}, false
		}
		ids := make([]string, 0, len(parts))
		for _, part := range parts {
			id := strings.TrimSpace(part)
			if !requestTraceExportIDPattern.MatchString(id) {
				return service.RequestTraceExportFilter{}, false
			}
			ids = append(ids, id)
		}
		filter.TraceIDs = ids
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
	case errors.Is(err, service.ErrRequestTraceExportNotReady):
		// 409：任务还没好，重试即可。报成 410"已过期"会让轮询中的任务看起来像已失败。
		return errRequestTraceExportNotReady
	case errors.Is(err, service.ErrRequestTraceExportGone):
		return errRequestTraceExportExpired
	case errors.Is(err, service.ErrRequestTraceExportFileLost):
		return errRequestTraceExportFileLost
	case errors.Is(err, service.ErrRequestTraceExportLimit):
		return errRequestTraceExportLimit
	case errors.Is(err, service.ErrRequestTraceExportRiskAcknowledgementRequired):
		// 未确认是调用方必须先解决的 400，不能折叠成 503——否则界面无法告诉管理员
		// "去读声明并确认"，只会显示一句"暂时不可用"。
		return errRequestTraceExportRiskAckRequired
	case errors.Is(err, service.ErrRequestTraceInvalidRecord):
		// 非法参数（例如越界的分片序号）必须先于其它分支判定：它是调用方的问题，
		// 不是容量或服务状态。
		return errRequestTraceExportInvalidFilter
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
