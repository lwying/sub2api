package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	// 400：找回的续页游标不可读。它与"没有下一页"是两件不同的事，不能混成一个空列表。
	errRequestTraceExportInvalidCursor = infraerrors.BadRequest("REQUEST_TRACE_EXPORT_INVALID_CURSOR", "invalid request trace export recall cursor")
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
	// ListTasks 是 ticket09 的找回接缝：只返回 actor 自己这个登录会话的任务的一页，
	// 游标为空就是第一页。第二个返回值是下一页的游标，空串表示没有下一页。
	ListTasks(ctx context.Context, actor service.RequestTraceExportActor, cursor string, limit int) ([]service.RequestTraceExportTask, string, error)
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
	ID           string                           `json:"id"`
	Status       service.RequestTraceExportStatus `json:"status"`
	Filter       service.RequestTraceExportFilter `json:"filter"`
	RowsExported int64                            `json:"rows_exported"`
	RowsSkipped  int64                            `json:"rows_skipped"`
	// SkippedByReason 是 RowsSkipped 的分类型形态：按封闭原因码给出"读失败"与
	// "记录消失"各多少条。聚合数仍是 rows_skipped，首个原因仍是 incomplete_reason；
	// 没有跳过时这个键缺席。它只含封闭原因码与计数，不含任何错误串。
	SkippedByReason service.RequestTraceExportSkipCounts `json:"skipped_by_reason,omitempty"`
	BytesExported   int64                                `json:"bytes_exported"`
	CreatedAt       time.Time                            `json:"created_at"`
	CompletedAt     *time.Time                           `json:"completed_at,omitempty"`
	DownloadUntil   *time.Time                           `json:"download_until,omitempty"`
	Downloadable    bool                                 `json:"downloadable"`
	// ShardCount 是本次导出实际生成的分片数；客户端按序号逐个下载。
	ShardCount int `json:"shard_count"`
	// Truncated / IncompleteReason 说明结果是否完整。管理端必须如实呈现，
	// 不能把不完整的导出当作全量结果。
	Truncated        bool   `json:"truncated"`
	IncompleteReason string `json:"incomplete_reason,omitempty"`
}

func newRequestTraceExportTaskView(task service.RequestTraceExportTask, now time.Time) requestTraceExportTaskView {
	return requestTraceExportTaskView{
		ID:           task.ID,
		Status:       task.Status,
		Filter:       task.Filter,
		RowsExported: task.RowsExported,
		RowsSkipped:  task.RowsSkipped,
		// 分类型跳过计数随任务一起给出；它不是聚合数的替代，而是它的拆解。
		SkippedByReason: task.SkippedByReason,
		BytesExported:   task.BytesExported,
		CreatedAt:       task.CreatedAt,
		CompletedAt:     task.CompletedAt,
		DownloadUntil:   task.DownloadUntil,
		// "现在这个会话能否下载"是服务端的结论，不能只看时钟：清单读不到的完成导出
		// 已经没有可下载的文件（下载只会以 file_lost 结束），因此这里必须为 false。
		// 真实的上限截断不受影响——它们的分片确实还在。
		Downloadable: task.Status == service.RequestTraceExportCompleted &&
			task.CompletedAt != nil && task.DownloadUntil != nil && now.Before(*task.DownloadUntil) &&
			task.IncompleteReason != service.RequestTraceExportIncompleteManifestLost,
		ShardCount:       len(task.Shards),
		Truncated:        task.Truncated,
		IncompleteReason: task.IncompleteReason,
	}
}

// requestTraceExportTaskListView 是"同会话找回"的响应体：一页有界数组加一个游标。
// 它没有总数——找回回答的是"我这个会话还能看到哪些任务"，不是可分页归档；
// 总数会让"只读了一页"看起来像"一共就这么多"。
//
// next_cursor 恒在：null 是"没有下一页了"这个事实本身，而不是缺席的字段。它是服务端
// 给出的不透明令牌，客户端只回传、不解析，也不落任何存储。游标里没有文件名、路径、
// 会话摘要或实例名。
type requestTraceExportTaskListView struct {
	Items      []requestTraceExportTaskView `json:"items"`
	NextCursor *string                      `json:"next_cursor"`
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

// List 返回**本管理员登录会话**在本实例上创建过的导出任务的一页，最近创建的在前。
//
// 与 Create/Get/Download 完全同一信任边界：只有管理员登录会话可读，管理员 API Key、
// 普通用户、其它管理员会话以及其它实例一律被拒，且不做任何身份回退。会话本身就是
// 过滤器，因此这里不接受任务 ID、管理员 ID 或会话句柄——调用方无法"指定"别人的任务。
//
// 续页只认服务端自己给出的 cursor：畸形、超长或版本不认识的令牌是 400，不是
// "没有更多"。缺省一页 20 条，最多 100 条（服务端拥有该边界）。
//
// 响应是任务视图数组加 next_cursor，字段与单任务读回完全一致：没有文件名、路径或
// 会话摘要，因此找回只给出句柄，下载仍要按任务 ID 与创建会话重新校验。
func (h *RequestTraceExportHandler) List(c *gin.Context) {
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
	limit, ok := requestTraceExportListLimit(c)
	if !ok {
		response.ErrorFrom(c, errRequestTraceExportInvalidFilter)
		setRequestTraceExportAudit(c, "failed", "request_trace_export_invalid_limit", nil)
		return
	}
	cursor, ok := requestTraceExportListCursor(c)
	if !ok {
		response.ErrorFrom(c, errRequestTraceExportInvalidCursor)
		setRequestTraceExportAudit(c, "failed", "request_trace_export_invalid_cursor", nil)
		return
	}
	tasks, next, err := h.service.ListTasks(c.Request.Context(), actor, cursor, limit)
	if err != nil {
		appErr := requestTraceExportError(err)
		response.ErrorFrom(c, appErr)
		setRequestTraceExportAudit(c, "failed", strings.ToLower(appErr.Reason), nil)
		return
	}
	items := make([]requestTraceExportTaskView, 0, len(tasks))
	now := time.Now().UTC()
	for _, task := range tasks {
		items = append(items, newRequestTraceExportTaskView(task, now))
	}
	view := requestTraceExportTaskListView{Items: items}
	if next != "" {
		view.NextCursor = &next
	}
	response.Success(c, view)
	setRequestTraceExportAudit(c, "success", "", nil)
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

// requestTraceExportQueryFilterKeys 是 query 上允许的元数据筛选键（与 Trace 列表口径一致）。
// trace_ids 不在其中："导出所选"的有界 ID 集合只能走结构化请求体。
var requestTraceExportQueryFilterKeys = []string{
	"trace_id", "route_family", "client_status", "usage_linked", "created_from", "created_to",
	"usage_log_id", "account_id", "group_id", "group_unknown", "requested_model", "model_unknown",
	"platform", "platform_unknown", "user_id", "user_unknown", "api_key_id", "api_key_unknown", "q",
}

// requestTraceExportSelectedBodyLimit 是"导出所选"请求体的硬上限。
// 2000 个 32 位十六进制 ID 的 JSON 数组远小于它；超出即视为畸形请求，
// 不会为了解析而把任意大小的请求体读进内存。
const requestTraceExportSelectedBodyLimit = 1 << 20

// requestTraceExportSelectedBody 是"导出所选"的结构化请求体。
// 只允许一个 trace_ids 数组：未知字段一律拒绝，因此请求体不可能夹带
// 筛选条件、正文或任何其它范围。
type requestTraceExportSelectedBody struct {
	TraceIDs []string `json:"trace_ids"`
}

// parseRequestTraceExportFilter 只接受 Trace 元数据筛选，并且只接受两种互斥范围：
//   - "导出当前查询全部"：query 上的元数据条件，沿用列表口径；
//   - "导出所选"：结构化 JSON 请求体里的有界 trace_ids 集合（不进 URL）。
//
// 显式给出但为空白（`?route_family=` 或 `%20`）的条件一律拒绝：把它当成"没给"
// 会让调用方以为筛了一项，导出却变成全部。非法取值、超限、重复或空集合同样
// 一律 400 invalid_filter，绝不静默截断、去重或退化成 429"容量不足"。
// 不接受任何正文、SQL 片段或自由文本。
func parseRequestTraceExportFilter(c *gin.Context) (service.RequestTraceExportFilter, bool) {
	// URL 上的 CSV trace_ids 是无界的旧形式，不再接受：出现即拒绝，
	// 而不是被默默忽略成"没有条件、导出全部"。
	if _, present := c.GetQuery("trace_ids"); present {
		return service.RequestTraceExportFilter{}, false
	}
	// 收集真正给出的 query 条件。键存在但值为空白不是"未筛选"，必须拒绝。
	values := make(map[string]string, len(requestTraceExportQueryFilterKeys))
	for _, key := range requestTraceExportQueryFilterKeys {
		raw, present := c.GetQuery(key)
		if !present {
			continue
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return service.RequestTraceExportFilter{}, false
		}
		values[key] = trimmed
	}

	filter := service.RequestTraceExportFilter{}
	if raw := values["trace_id"]; raw != "" {
		if !requestTraceExportIDPattern.MatchString(raw) {
			return service.RequestTraceExportFilter{}, false
		}
		filter.TraceID = raw
	}
	if raw := values["route_family"]; raw != "" {
		if _, ok := requestTraceExportRouteFamilies[raw]; !ok {
			return service.RequestTraceExportFilter{}, false
		}
		filter.RouteFamily = raw
	}
	if raw := values["client_status"]; raw != "" {
		status, err := strconv.Atoi(raw)
		if err != nil || status < 0 || status > 599 {
			return service.RequestTraceExportFilter{}, false
		}
		filter.ClientStatus = &status
	}
	if raw := values["usage_linked"]; raw != "" {
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
		raw := values[entry.key]
		if raw == "" {
			continue
		}
		parsed, ok := parseRequestTraceExportTime(raw)
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
		raw := values[entry.key]
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
	rawGroupID := values["group_id"]
	rawGroupUnknown := values["group_unknown"]
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
	filter.RequestedModel = values["requested_model"]
	rawModelUnknown := values["model_unknown"]
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
	filter.Platform = values["platform"]
	rawPlatformUnknown := values["platform_unknown"]
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

	for _, identity := range []struct {
		idKey      string
		unknownKey string
		id         **int64
		unknown    **bool
	}{
		{"user_id", "user_unknown", &filter.UserID, &filter.UserUnknown},
		{"api_key_id", "api_key_unknown", &filter.APIKeyID, &filter.APIKeyUnknown},
	} {
		value := values[identity.idKey]
		unknownValue := values[identity.unknownKey]
		if value != "" && unknownValue != "" {
			return service.RequestTraceExportFilter{}, false
		}
		if value != "" {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 {
				return service.RequestTraceExportFilter{}, false
			}
			*identity.id = &id
		}
		if unknownValue != "" {
			unknown, err := strconv.ParseBool(unknownValue)
			if err != nil {
				return service.RequestTraceExportFilter{}, false
			}
			*identity.unknown = &unknown
		}
	}
	if value := values["q"]; value != "" {
		if !service.ValidRequestTraceKeyword(value) {
			return service.RequestTraceExportFilter{}, false
		}
		filter.Keyword = value
	}

	// 导出所选：有界结构化请求体，与上面任何 query 条件互斥。
	selectedIDs, selected, ok := requestTraceExportSelectedIDs(c)
	if !ok {
		return service.RequestTraceExportFilter{}, false
	}
	if selected {
		if len(values) > 0 {
			return service.RequestTraceExportFilter{}, false
		}
		ids, valid := validRequestTraceExportSelectedIDs(selectedIDs)
		if !valid {
			return service.RequestTraceExportFilter{}, false
		}
		filter.TraceIDs = ids
	}
	return filter, true
}

// validRequestTraceExportSelectedIDs 校验"导出所选"集合：必须有界、非空、
// 逐个形状合法且没有重复。重复 ID 会让"所选 N 条"与实际导出条数对不上，
// 因此直接拒绝而不是静默去重。
func validRequestTraceExportSelectedIDs(selected []string) ([]string, bool) {
	if len(selected) == 0 || len(selected) > service.RequestTraceExportMaxSelectedIDs {
		return nil, false
	}
	seen := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		if !requestTraceExportIDPattern.MatchString(id) {
			return nil, false
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, false
		}
		seen[id] = struct{}{}
	}
	return selected, true
}

// requestTraceExportSelectedIDs 读取"导出所选"的结构化请求体。
// present 为真表示调用方用请求体给出了所选集合；ok 为假表示请求体畸形
// （未知字段、非数组、尾随内容、超长），调用方必须拒绝。
// 空请求体不是畸形：那是"导出当前查询全部"。
func requestTraceExportSelectedIDs(c *gin.Context) (ids []string, present bool, ok bool) {
	if c.Request == nil || c.Request.Body == nil {
		return nil, false, true
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, requestTraceExportSelectedBodyLimit+1))
	if err != nil || len(raw) > requestTraceExportSelectedBodyLimit {
		return nil, false, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// 结构化的范围不接受未知字段：正文、筛选条件或任何其它范围都不能夹带进来。
	decoder.DisallowUnknownFields()
	var body requestTraceExportSelectedBody
	if err := decoder.Decode(&body); err != nil {
		return nil, false, false
	}
	// 只允许一个 JSON 值：尾随内容一律拒绝。
	if err := decoder.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, false, false
	}
	return body.TraceIDs, true, true
}

// parseRequestTraceExportTime 解析 RFC3339Nano 时间筛选值。
func parseRequestTraceExportTime(raw string) (*time.Time, bool) {
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
	case errors.Is(err, service.ErrRequestTraceExportInvalidCursor):
		// 畸形游标是调用方的问题（400）：报成 429 或"空列表"都会让缺失的任务
		// 看起来像不存在。
		return errRequestTraceExportInvalidCursor
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

// requestTraceExportListLimit 解析可选的 limit 查询参数。
//
// 缺席（或整段参数缺失）交给服务层取默认值；显式给出却不是 [1, 上限] 内的整数
// 一律 400。它不会被夹成更小的值，也不会退化成 429"容量不足"——非法参数是调用方
// 的问题，429 只留给真正的容量边界，否则界面会把两者混成一句"暂时不可用"。
func requestTraceExportListLimit(c *gin.Context) (int, bool) {
	raw, present := c.GetQuery("limit")
	if !present {
		return 0, true
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	limit, err := strconv.Atoi(trimmed)
	if err != nil || limit < 1 || limit > service.RequestTraceExportMaxListedTasks {
		return 0, false
	}
	return limit, true
}

// requestTraceExportListCursor 解析可选的 cursor 查询参数。
//
// 缺席（或整段参数缺失）交给服务层取第一页；显式给出却为空、超长或超过服务端的
// 令牌长度上限一律 400。handler 只做形状与长度的把关，令牌本身由服务层解码：
// 它是不透明令牌，只有签发它的那一层能判断它是否可读。
func requestTraceExportListCursor(c *gin.Context) (string, bool) {
	raw, present := c.GetQuery("cursor")
	if !present {
		return "", true
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > service.RequestTraceExportRecallCursorMaxLength {
		return "", false
	}
	return trimmed, true
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
