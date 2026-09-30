package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	RequestTraceExportDownloadWindow = 7 * 24 * time.Hour
	requestTraceExportPageSize       = 128
	requestTraceExportDefaultRows    = 10_000
	requestTraceExportDefaultBytes   = 128 << 20
	requestTraceExportDefaultRuntime = 10 * time.Minute
	// requestTraceExportCleanupScanPage bounds how many expired rows one sweep
	// may inspect. It is deliberately independent of the caller's deletion
	// limit: a page full of non-deletable entries (for example unexpected
	// nonregular files) must never hide the deletable rows behind them. It is
	// also the store's maximum accepted page size.
	requestTraceExportCleanupScanPage = 500

	// 单分片缺省上限：分片是为了让大结果流式落盘而不是一次性写入一个文件。
	requestTraceExportDefaultShardRows  = 2_000
	requestTraceExportDefaultShardBytes = 32 << 20
)

var (
	ErrRequestTraceExportDisabled        = errors.New("request trace export disabled")
	ErrRequestTraceExportSessionRequired = errors.New("admin login session required for request trace export")
	ErrRequestTraceExportForbidden       = errors.New("request trace export belongs to a different admin session")
	ErrRequestTraceExportNotFound        = errors.New("request trace export task not found")
	ErrRequestTraceExportGone            = errors.New("request trace export download expired")
	// ErrRequestTraceExportNotReady 表示任务还没有产出可下载的文件：轮询重试即可，
	// 它不是"过期"也不是"丢失"。
	ErrRequestTraceExportNotReady    = errors.New("request trace export is not ready for download")
	ErrRequestTraceExportFileLost    = errors.New("request trace export file lost")
	ErrRequestTraceExportLimit       = errors.New("request trace export capacity limit exceeded")
	ErrRequestTraceExportUnavailable = errors.New("request trace export temporarily unavailable")
	// ErrRequestTraceExportInvalidCursor 表示找回的续页游标不可读（畸形、超长或
	// 版本不认识）。它是调用方的问题（400），既不是"容量不足"，也不是"没有更多"：
	// 把畸形游标当成"读完了"会让缺失的任务看起来像不存在。
	ErrRequestTraceExportInvalidCursor = errors.New("request trace export recall cursor is not readable")
)

var requestTraceExportIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

type RequestTraceExportStatus string

const (
	RequestTraceExportPending   RequestTraceExportStatus = "pending"
	RequestTraceExportRunning   RequestTraceExportStatus = "running"
	RequestTraceExportCompleted RequestTraceExportStatus = "completed"
	RequestTraceExportFailed    RequestTraceExportStatus = "failed"
)

// RequestTraceExportFilter is a bounded metadata-only selection. It never accepts
// body text or arbitrary SQL. The source decides which Trace IDs match it.
type RequestTraceExportFilter struct {
	TraceID      string     `json:"trace_id,omitempty"`
	RouteFamily  string     `json:"route_family,omitempty"`
	ClientStatus *int       `json:"client_status,omitempty"`
	CreatedFrom  *time.Time `json:"created_from,omitempty"`
	CreatedTo    *time.Time `json:"created_to,omitempty"`
	UsageLinked  *bool      `json:"usage_linked,omitempty"`
	// 以下检索条件与 Trace 列表保持一致：导出必须覆盖"当前查询的全部结果"，
	// 因此列表能用的条件导出也要能用，否则导出就不是同一批记录。
	UsageLogID      *int64 `json:"usage_log_id,omitempty"`
	AccountID       *int64 `json:"account_id,omitempty"`
	GroupID         *int64 `json:"group_id,omitempty"`
	GroupUnknown    *bool  `json:"group_unknown,omitempty"`
	RequestedModel  string `json:"requested_model,omitempty"`
	ModelUnknown    *bool  `json:"model_unknown,omitempty"`
	Platform        string `json:"platform,omitempty"`
	PlatformUnknown *bool  `json:"platform_unknown,omitempty"`
	UserID          *int64 `json:"user_id,omitempty"`
	UserUnknown     *bool  `json:"user_unknown,omitempty"`
	APIKeyID        *int64 `json:"api_key_id,omitempty"`
	APIKeyUnknown   *bool  `json:"api_key_unknown,omitempty"`
	Keyword         string `json:"q,omitempty"`

	// TraceIDs 是"导出所选"的明确成员集合；非空时只导出这些 Trace，
	// 且与上面的条件互斥（由调用方保证）。集合必须有界。
	TraceIDs []string `json:"trace_ids,omitempty"`
}

// The session ID is used only at the trust boundary. Only its digest is persisted.
type RequestTraceExportActor struct {
	AdminUserID int64
	SessionID   string
}

type RequestTraceExportTask struct {
	ID            string                   `json:"id"`
	Status        RequestTraceExportStatus `json:"status"`
	Filter        RequestTraceExportFilter `json:"filter"`
	AdminUserID   int64                    `json:"-"`
	SessionDigest string                   `json:"-"`
	InstanceID    string                   `json:"-"`
	Filename      string                   `json:"-"`
	RowsExported  int64                    `json:"rows_exported"`
	RowsSkipped   int64                    `json:"rows_skipped"`
	BytesExported int64                    `json:"bytes_exported"`
	// SkippedByReason 是 RowsSkipped 的分类型形态（规格 §2.4）：它保留"读失败"
	// 与"记录消失"的区别，聚合数仍是 RowsSkipped、首个原因仍是 IncompleteReason。
	// 它只在本次运行的内存态与清单里存在——任务行没有这一列，读回任务时从清单
	// 恢复（见 GetTask），因此不需要数据库迁移。
	SkippedByReason RequestTraceExportSkipCounts `json:"skipped_by_reason,omitempty"`
	CreatedAt       time.Time                    `json:"created_at"`
	CompletedAt     *time.Time                   `json:"completed_at"`
	DownloadUntil   *time.Time                   `json:"download_until"`

	// LimitsSnapshot 是任务创建时固定的资源上限（规格 §2.4）。它随任务一起被
	// 接纳：Create 必须在写入任务行的同时把它落库，因此从"任务可被认领"的那一刻
	// 起它的预算就已经确定，不存在"先排队、后补快照"的窗口。
	// 指针为 nil 表示这一行没有快照——升级前创建的任务，或部署从未注入过配置
	// 来源；执行时退回当前生效配置，绝不退化成无界。
	LimitsSnapshot *RequestTraceExportLimits `json:"-"`

	// Shards 是本次任务实际生成的分片（升序）；Manifest 是清单文件名。
	// 两者都只在服务端使用，下载始终经由任务 ID 校验。
	Shards   []string `json:"-"`
	Manifest string   `json:"-"`
	// Truncated 为真表示任务在完成前触达了资源上限或发现了缺失/失败，
	// 结果只能按"不完整"交付；此时绝不声称它是全量。
	Truncated bool `json:"truncated"`
	// IncompleteReason 是给管理端看的封闭原因码（例如 "limit_rows"），不含任何正文。
	IncompleteReason string `json:"incomplete_reason,omitempty"`
}

// RequestTraceExportStore 是导出任务的存储：创建／认领／收尾／过期／删除，
// 以及"本会话找回"（单独声明在 RequestTraceExportPagedRecallStore 上）。
type RequestTraceExportStore interface {
	Create(ctx context.Context, task RequestTraceExportTask, maxInFlight int) error
	Get(ctx context.Context, id string) (RequestTraceExportTask, error)
	ListStale(ctx context.Context, instanceID string, before time.Time, limit int) ([]RequestTraceExportTask, error)
	Claim(ctx context.Context, instanceID string) (RequestTraceExportTask, error)
	Finish(ctx context.Context, task RequestTraceExportTask) error
	Expired(ctx context.Context, before time.Time, limit int) ([]RequestTraceExportTask, error)
	Delete(ctx context.Context, id string) error
}

// RequestTraceExportRecallCursor 是一次找回的键集边界：上一页最后一行的排序键
// (created_at, export_id)。它是一个**值**而不是行引用——边界行随后被清理后，
// 续页依然从它之后继续，不会重头开始。
type RequestTraceExportRecallCursor struct {
	CreatedAt time.Time
	ExportID  string
}

// RequestTraceExportPagedRecallStore 是"同会话找回"的存储能力：按创建管理员 +
// 会话摘要 + 实例三个条件过滤（缺一不可，且都由已验签的主体推出），最近创建的在前，
// 从键集边界之后继续读一页，并回答"边界之后还有行"。
//
// 它单独成一个接口，因为它是唯一必须能翻页的读：一次只读回一页的存储会让第 21 个
// 任务永远不可达（这正是 ticket09 的召回缺口）。本仓库的存储实现了它；服务层在
// 没有它的存储上**明确失败**，绝不退回"读回一页且不给出下一页"——那等于把
// "还有更早的任务"悄悄说成"就这么多"。
type RequestTraceExportPagedRecallStore interface {
	ListForSession(ctx context.Context, adminUserID int64, sessionDigest, instanceID string, after *RequestTraceExportRecallCursor, limit int) ([]RequestTraceExportTask, bool, error)
}

// requestTraceExportPagedExpiryStore is an optional capability of
// RequestTraceExportStore. A store that provides it lets the expiry sweep
// resume strictly after the last examined export_id in ascending id order and
// wrap when it reaches the tail, so any number of non-deletable entries can be
// walked past without ever starving the valid rows behind them. A store without
// it is swept through the base Expired head page only; the sweep stays bounded
// either way.
type requestTraceExportPagedExpiryStore interface {
	ExpiredAfter(ctx context.Context, before time.Time, after string, limit int) ([]RequestTraceExportTask, error)
}

// Only these fields can enter a disk export. Source implementations must call
// the same redacted/validated disclosure as the session-only Trace detail API.
// Only typed, bounded and revalidated redacted facts may include URL or headers;
// no free-form metadata or raw []byte field is exported.
type RequestTraceExportApprovedDetail struct {
	// 信封的 omitempty 逐字段对齐 Trace 详情（RequestTrace 的 JSON 标签）：
	// 详情恒定输出的键（route_family / inbound_endpoint / capture_state /
	// usage_log_id / completed_at / cleanup_after）导出也恒定输出，nil / 空就是
	// null / ""；详情带 omitempty 的键（group_id / requested_model）导出同样允许
	// 缺席。JSON 键契约必须一致，否则"详情同款字段"在缺值时就变成缺键。
	TraceID         string `json:"trace_id"`
	RouteFamily     string `json:"route_family"`
	InboundEndpoint string `json:"inbound_endpoint"`
	CaptureState    string `json:"capture_state"`
	ClientStatus    int    `json:"client_status"`
	UsageLogID      *int64 `json:"usage_log_id"`
	// 以下信封字段与 Trace 详情（RequestTrace）同款同义：导出是"详情同款字段契约"，
	// 不能因为先前导出遗漏就把它们删掉。nil / 空表示该事实未被观察到。
	CreatedAt      time.Time                         `json:"created_at"`
	CompletedAt    *time.Time                        `json:"completed_at"`
	CleanupAfter   *time.Time                        `json:"cleanup_after"`
	GroupID        *int64                            `json:"group_id,omitempty"`
	UserID         *int64                            `json:"user_id"`
	APIKeyID       *int64                            `json:"api_key_id"`
	UserEmail      string                            `json:"user_email,omitempty"`
	APIKeyName     string                            `json:"api_key_name,omitempty"`
	RequestedModel string                            `json:"requested_model,omitempty"`
	Stages         []RequestTraceExportApprovedStage `json:"stages"`

	// ObservedPlatforms 与 Trace 详情（RequestTrace）同款同义：本次逻辑请求**实际选中过**
	// 的上游账号平台（去重、按首次观察顺序、有界），含"已选中但还没发出上游就失败"的账号。
	// 它是请求时事实，不随账号后来更换平台而变化；nil 表示从未选到账号（未知），
	// 与 group_id / requested_model 一样用缺席键表达。
	ObservedPlatforms []string `json:"observed_platforms,omitempty"`
}

type RequestTraceExportApprovedStage struct {
	Ordinal             int    `json:"ordinal"`
	Stage               string `json:"stage"`
	AttemptIndex        int    `json:"attempt_index"`
	ViewName            string `json:"view_name"`
	State               string `json:"state"`
	Reason              string `json:"reason"`
	ObservedBytes       int64  `json:"observed_bytes"`
	RetainedBytes       int    `json:"retained_bytes"`
	DroppedEvents       int    `json:"dropped_events"`
	RedactionUnverified bool   `json:"redaction_unverified"`
	PayloadText         string `json:"payload_text,omitempty"`
	// 阶段字段与管理员详情视图（requestTraceStageView）逐字段一致：详情没有的
	// 字段这里也不加，导出不能比详情多出一层它无法解释的事实。
	Facts    *RequestTraceStageFacts    `json:"facts,omitempty"`
	Decision *RequestTraceDecisionFacts `json:"decision,omitempty"`
}

// A disappearing row returns available=false; it is counted as skipped rather
// than resurrected. Pages follow the Trace list order (created_at DESC, id DESC),
// with a bounded opaque cursor that remains usable if the last row is deleted.
type RequestTraceExportSource interface {
	NextTracePage(ctx context.Context, filter RequestTraceExportFilter, after string, limit int) (ids []string, next string, err error)
	ReadApprovedDetail(ctx context.Context, id string) (detail RequestTraceExportApprovedDetail, available bool, err error)
}

type RequestTraceExportOptions struct {
	// Enabled 已废弃且不再参与任何判定：导出的授权来自管理员在界面上的动作
	// （开启 Trace + 确认明文副本风险），部署前提是 SingleInstanceDeclared。
	// 保留字段是为了不让既有装配点因为删字段而编译失败；设成什么值都不改变行为。
	Enabled                bool
	SingleInstanceDeclared bool
	InstanceID             string
	TempDir                string
	MaxRows                int64
	MaxBytes               int64
	MaxRuntime             time.Duration
	// MaxShardRows / MaxShardBytes 是单个分片的上限：超过就封口开下一个分片，
	// 而不是让整个任务失败。整任务上限仍是 MaxRows / MaxBytes。
	MaxShardRows  int64
	MaxShardBytes int64
}

type RequestTraceExportService struct {
	store  RequestTraceExportStore
	source RequestTraceExportSource
	opts   RequestTraceExportOptions
	now    func() time.Time
	mu     sync.Mutex // one export writer per process, even when RunOnce is called concurrently

	cleanupMu     sync.Mutex // serialises expiry sweeps and guards cleanupCursor
	cleanupCursor string     // last expired export_id examined; "" restarts a sweep cycle

	// acknowledged 报告管理员是否已接受**当前版本**的明文导出风险声明。
	// 未注入时按"未确认"处理：不确认就不产生明文副本，这是能力边界而不是开关。
	acknowledged func() bool
	// acknowledgementBypassed 只允许测试显式声明"确认已满足"，
	// 生产装配必须走 SetAcknowledgementChecker 读取真实确认记录。
	acknowledgementBypassed bool
	// limitsProvider 返回管理员当前配置的任务上限；未注入时用构造时的缺省值。
	// 它只在两个时刻被读取：创建任务时（固定这次的快照）和任务没有快照时的
	// 退回取值。执行中的预算来自任务自己的快照，见 boundsForTask。
	limitsProvider func() RequestTraceExportLimits
}

// SetLimitsProvider 注入任务上限来源。
func (s *RequestTraceExportService) SetLimitsProvider(provider func() RequestTraceExportLimits) {
	if s != nil {
		s.limitsProvider = provider
	}
}

// boundsForTask 返回这个任务这次执行真正使用的预算。
//
// 顺序是刻意的：**任务自己的快照优先**。创建时固定的上限决定它的预算，之后
// 管理员改配置不再影响它，这样"改配置只影响之后创建的任务"才是真的。快照缺席
// （升级前创建的任务，或部署从未注入过配置来源）时退回当前生效配置：这类任务
// 本来就没有被承诺过某个快照，按当时配置有界执行即可，绝不因为读不到快照就
// 变成无界。
func (s *RequestTraceExportService) boundsForTask(task RequestTraceExportTask) requestTraceExportBounds {
	if task.LimitsSnapshot != nil {
		return boundsFromLimits(*task.LimitsSnapshot)
	}
	return s.currentBounds()
}

// currentBounds 是按此刻的生效配置执行所用的预算，不看任何任务快照。
// 它给"没有快照可用的任务"和过期判定一个统一、有界的取值来源。
func (s *RequestTraceExportService) currentBounds() requestTraceExportBounds {
	if s.limitsProvider != nil {
		return boundsFromLimits(s.limitsProvider())
	}
	return boundsFromOptions(s.opts)
}

// SetAcknowledgementChecker 注入导出风险确认的判据。
func (s *RequestTraceExportService) SetAcknowledgementChecker(check func() bool) {
	if s != nil {
		s.acknowledged = check
	}
}

// SetAcknowledgementSatisfiedForTest 让测试夹具表达"管理员已接受当前声明"。
// 它不影响生产装配路径，也不放宽任何权限或期限。
func (s *RequestTraceExportService) SetAcknowledgementSatisfiedForTest(satisfied bool) {
	if s != nil {
		s.acknowledgementBypassed = satisfied
	}
}

// acknowledgementSatisfied 报告当前是否允许创建新的导出任务。
func (s *RequestTraceExportService) acknowledgementSatisfied() bool {
	if s == nil {
		return false
	}
	if s.acknowledgementBypassed {
		return true
	}
	return s.acknowledged != nil && s.acknowledged()
}

func NewRequestTraceExportService(store RequestTraceExportStore, source RequestTraceExportSource, opts RequestTraceExportOptions) *RequestTraceExportService {
	if opts.MaxRows <= 0 {
		opts.MaxRows = requestTraceExportDefaultRows
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = requestTraceExportDefaultBytes
	}
	if opts.MaxRuntime <= 0 {
		opts.MaxRuntime = requestTraceExportDefaultRuntime
	}
	// 单分片上限缺省独立于整任务上限：分片存在的意义就是让大结果不必一次性写入。
	if opts.MaxShardRows <= 0 {
		opts.MaxShardRows = requestTraceExportDefaultShardRows
	}
	if opts.MaxShardBytes <= 0 {
		opts.MaxShardBytes = requestTraceExportDefaultShardBytes
	}
	// 分片上限不得超过整任务上限：否则单分片永远写不满，上限形同虚设。
	if opts.MaxShardRows > opts.MaxRows {
		opts.MaxShardRows = opts.MaxRows
	}
	if opts.MaxShardBytes > opts.MaxBytes {
		opts.MaxShardBytes = opts.MaxBytes
	}
	if opts.TempDir == "" {
		opts.TempDir = os.TempDir()
	}
	return &RequestTraceExportService{store: store, source: source, opts: opts, now: time.Now}
}

// allowed 报告本实例是否具备导出能力：存储、来源、进程标识与**显式的单实例声明**。
//
// 这里刻意不看配置文件开关：导出的授权来自管理员在界面上的动作（开启 Trace +
// 确认明文副本风险），部署前提是单实例声明。曾经额外要求 `request_trace_export.enabled`
// 会让管理员按设计操作后仍然拿到 503，而界面上没有任何地方能开启那个键。
//
// 未确认风险、未声明单实例时仍会分别被 CreateTask 与这里拒绝。
func (s *RequestTraceExportService) allowed() bool {
	return s != nil && s.opts.SingleInstanceDeclared &&
		s.opts.InstanceID != "" && s.store != nil && s.source != nil
}

func sessionDigest(session string) string {
	hash := sha256.Sum256([]byte(session))
	return hex.EncodeToString(hash[:])
}

func exportActorValid(actor RequestTraceExportActor) bool {
	return actor.AdminUserID > 0 && strings.TrimSpace(actor.SessionID) != ""
}

// RequestTraceExportMaxSelectedIDs 是"导出所选"一次能携带的最大 Trace 数。
// 超出即拒绝并要求改用"导出当前查询全部"，绝不静默截断。
const RequestTraceExportMaxSelectedIDs = 2000

// RequestTraceExportMaxListedTasks 是"同会话找回"一页能读回的最大任务数。
// 它是有界的**页**上限：超过即拒绝，绝不静默截断成"看起来就这么多"；再早的任务
// 由不透明游标翻页取回，见 RequestTraceExportRecallCursorMaxLength。
const RequestTraceExportMaxListedTasks = 100

// RequestTraceExportDefaultListedTasks 是调用方未指定页大小时的默认值。
const RequestTraceExportDefaultListedTasks = 20

// requestTraceExportRecallCursorVersion 是游标信封的版本前缀。客户端从不解析载荷，
// 但服务端要能拒绝自己不认识、或含义已经改过的令牌。
const requestTraceExportRecallCursorVersion = "v1"

// RequestTraceExportRecallCursorMaxLength 是游标令牌的硬长度上限。长度在解码之前
// 先检查：超长输入按畸形处理，既不拿去解析也不回显。
const RequestTraceExportRecallCursorMaxLength = 128

// requestTraceExportRecallCursorSeparator 分隔游标载荷里的两个排序键。base64url 的
// 字母表里不含它，所以"按第一个分隔符切开"是唯一解，后面的内容会被整段当作 ID 校验。
const requestTraceExportRecallCursorSeparator = "|"

// encodeRequestTraceExportRecallCursor 把键集边界封成不透明令牌。
// 载荷只有这一页最后一行的排序键：没有文件名、路径、会话摘要、实例名或任何正文。
// 游标会出现在 URL 里，因此它必须什么都不泄露。
//
// 它是"不透明"，不是"机密"：没有签名，因为伪造它得不到任何越权的东西——三个过滤
// 条件仍然来自已验签的主体，客户端能换到的至多是**自己会话**里某一行的排序键。
// 因此校验只覆盖"可读性"（版本、长度、形状、可解析），不做时间窗或新鲜度检查。
func encodeRequestTraceExportRecallCursor(cursor RequestTraceExportRecallCursor) string {
	payload := cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + requestTraceExportRecallCursorSeparator + cursor.ExportID
	return requestTraceExportRecallCursorVersion + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// decodeRequestTraceExportRecallCursor 解出键集边界。任何畸形输入都折叠成同一个
// 哨兵错误：无法解释的令牌绝不能被当成"读完了"。空串代表"没有边界"（第一页），
// 由调用方在解码之前判定。
func decodeRequestTraceExportRecallCursor(raw string) (*RequestTraceExportRecallCursor, error) {
	if len(raw) > RequestTraceExportRecallCursorMaxLength {
		return nil, ErrRequestTraceExportInvalidCursor
	}
	version, encoded, found := strings.Cut(raw, ".")
	if !found || version != requestTraceExportRecallCursorVersion {
		return nil, ErrRequestTraceExportInvalidCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrRequestTraceExportInvalidCursor
	}
	created, exportID, found := strings.Cut(string(payload), requestTraceExportRecallCursorSeparator)
	if !found || !requestTraceExportIDShape.MatchString(exportID) {
		return nil, ErrRequestTraceExportInvalidCursor
	}
	createdAt, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return nil, ErrRequestTraceExportInvalidCursor
	}
	return &RequestTraceExportRecallCursor{CreatedAt: createdAt.UTC(), ExportID: exportID}, nil
}

func exportFilterValid(filter RequestTraceExportFilter) bool {
	if filter.TraceID != "" && !requestTraceExportIDShape.MatchString(filter.TraceID) {
		return false
	}
	switch filter.RouteFamily {
	case "", "messages", "chat_completions", "responses":
	default:
		return false
	}
	if filter.ClientStatus != nil && (*filter.ClientStatus < 0 || *filter.ClientStatus > 599) {
		return false
	}
	if filter.UsageLogID != nil && *filter.UsageLogID <= 0 {
		return false
	}
	if filter.AccountID != nil && *filter.AccountID <= 0 {
		return false
	}
	if filter.GroupID != nil && *filter.GroupID <= 0 {
		return false
	}
	if (filter.UserID != nil && (*filter.UserID <= 0 || filter.UserUnknown != nil)) ||
		(filter.APIKeyID != nil && (*filter.APIKeyID <= 0 || filter.APIKeyUnknown != nil)) ||
		(filter.Keyword != "" && !ValidRequestTraceKeyword(filter.Keyword)) {
		return false
	}
	// 具体值与"未知"互斥：同时给出说明调用方把两种范围的记录混在一起，
	// 这是无法解释的筛选，必须拒绝而不是退化成任意匹配。
	if filter.GroupID != nil && filter.GroupUnknown != nil {
		return false
	}
	if strings.TrimSpace(filter.RequestedModel) != "" && filter.ModelUnknown != nil {
		return false
	}
	if strings.TrimSpace(filter.Platform) != "" && filter.PlatformUnknown != nil {
		return false
	}
	if len(filter.TraceIDs) > 0 {
		if len(filter.TraceIDs) > RequestTraceExportMaxSelectedIDs {
			return false
		}
		seen := make(map[string]struct{}, len(filter.TraceIDs))
		for _, id := range filter.TraceIDs {
			if !requestTraceExportIDShape.MatchString(id) {
				return false
			}
			// 重复 ID 会让"所选 N 条"与实际导出行数对不上。
			if _, ok := seen[id]; ok {
				return false
			}
			seen[id] = struct{}{}
		}
	}
	return filter.CreatedFrom == nil || filter.CreatedTo == nil || filter.CreatedFrom.Before(*filter.CreatedTo)
}

func (s *RequestTraceExportService) CreateTask(ctx context.Context, actor RequestTraceExportActor, filter RequestTraceExportFilter) (RequestTraceExportTask, error) {
	if !s.allowed() {
		return RequestTraceExportTask{}, ErrRequestTraceExportDisabled
	}
	if !exportActorValid(actor) {
		return RequestTraceExportTask{}, ErrRequestTraceExportSessionRequired
	}
	// 明文副本需要管理员对**当前版本**的导出风险声明的确认；
	// 采集开关与采集确认都不构成这一确认。
	if !s.acknowledgementSatisfied() {
		return RequestTraceExportTask{}, ErrRequestTraceExportRiskAcknowledgementRequired
	}
	// 先校验**调用方给出的**筛选，再盖内部时间上界，顺序是刻意的。
	// exportFilterValid 里的"时间窗必须是严格正区间"只针对调用方同时给出的两端：
	// 这与列表/HTTP 层同一口径（见 request_trace_handler 的 `fromGiven && toGiven`、
	// 导出 handler 的同等校验），只有两端都由调用方给出时才可能"倒置"。
	// 内部上界是"查询全部"的非严格快照边界，不是调用方给的筛选，因此必须先校验后
	// 盖章：否则一个只给了**落在未来的下界**的合法查询，会被这次盖章变成倒置区间
	// 而在创建时被拒成 ErrRequestTraceExportLimit（HTTP 429"容量不足"），而同一个
	// 查询在列表里返回 200 空集。规格 §2.4 要求两者同一过滤语义。
	if !exportFilterValid(filter) {
		return RequestTraceExportTask{}, ErrRequestTraceExportLimit
	}
	// "查询全部"的时间上界（不含）在**任务创建这一刻**固定下来，之后新产生的 Trace
	// 不会因为任务还在排队执行而被卷进来。下界落在未来时，加盖的上界（现在）早于它，
	// 交集为空：导出仍然成功，产出 0 行且清单标"完整"，与列表的 200 空集一致；上界
	// 本身必须在，所以这里绝不为"未来下界"跳过盖章、留出无上界的窗口。
	// 调用方已经给了范围就不覆盖；"导出所选"只认勾选 ID，它自己的范围是 ID 集合。
	if filter.CreatedTo == nil && len(filter.TraceIDs) == 0 {
		bound := s.now().UTC()
		filter.CreatedTo = &bound
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	task := RequestTraceExportTask{
		ID: id, Status: RequestTraceExportPending, Filter: filter, AdminUserID: actor.AdminUserID,
		SessionDigest: sessionDigest(actor.SessionID), InstanceID: s.opts.InstanceID,
		CreatedAt: s.now().UTC(),
	}
	// 上限在**创建时**随任务一起固定：注入过配置来源就把当次生效值冻结进这一行，
	// 存储必须在同一次 Create 里落库（真实存储把它写进 INSERT），因此从任务可被
	// 认领的那一刻起预算就已确定，不存在"先排队、后补快照"的窗口。没有来源时
	// 生效值就是构造缺省、永不改变，冻结与不冻结等价，不必落库。
	if s.limitsProvider != nil {
		snapshot := NormalizeRequestTraceExportLimits(s.limitsProvider())
		task.LimitsSnapshot = &snapshot
	}
	if err := s.store.Create(ctx, task, 1); err != nil {
		if errors.Is(err, ErrRequestTraceExportLimit) {
			return RequestTraceExportTask{}, err
		}
		return RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
	}
	// 快照必须真的随任务落库，而且必须就是这一份：任务一旦可被认领，它的预算
	// 就只能来自创建时固定的值。只检查"读回来存在"是不够的——存储可能漏写、
	// 写错，或写进了另一个时刻的配置。这里逐字段比对归一化后的预算，不一致就
	// 删掉刚创建的行并如实报错，而不是留下一个"看起来排好队、实际会按别的
	// 预算执行"的任务，也不让它占着"每实例一个活动任务"的名额。
	if task.LimitsSnapshot != nil {
		want := boundsFromLimits(*task.LimitsSnapshot)
		stored, err := s.store.Get(ctx, task.ID)
		if err != nil || stored.LimitsSnapshot == nil || boundsFromLimits(*stored.LimitsSnapshot) != want {
			_ = s.store.Delete(ctx, task.ID)
			return RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
		}
	}
	return task, nil
}

func (s *RequestTraceExportService) GetTask(ctx context.Context, actor RequestTraceExportActor, id string) (RequestTraceExportTask, error) {
	if !s.allowed() {
		return RequestTraceExportTask{}, ErrRequestTraceExportDisabled
	}
	if !exportActorValid(actor) {
		return RequestTraceExportTask{}, ErrRequestTraceExportSessionRequired
	}
	if !requestTraceExportIDShape.MatchString(id) {
		return RequestTraceExportTask{}, ErrRequestTraceExportNotFound
	}
	task, err := s.store.Get(ctx, id)
	if err != nil {
		return RequestTraceExportTask{}, err
	}
	if task.AdminUserID != actor.AdminUserID || task.SessionDigest != sessionDigest(actor.SessionID) {
		return RequestTraceExportTask{}, ErrRequestTraceExportForbidden
	}
	if task.InstanceID != s.opts.InstanceID {
		return RequestTraceExportTask{}, ErrRequestTraceExportFileLost
	}
	// 分片列表与完整性结论只存在于清单里。结论由与服务端找回**共用**的那条路径给出：
	// 读得到清单就按它说，读不到就是"无从判断"（Truncated=true + manifest_lost），
	// 而不是保持零值让客户端把丢失的交付当成完成。分片名只在读得到清单时回填——
	// 读不到就没有可交付的分片可列，空列表不是"0 条"。
	if manifest, ok := s.manifestCompletenessFromManifest(&task); ok {
		names := make([]string, 0, len(manifest.Shards))
		for _, shard := range manifest.Shards {
			names = append(names, shard.Name)
		}
		task.Shards = names
		task.Manifest = exportManifestBase(id)
	}
	return task, nil
}

// manifestCompletenessFromManifest 用清单补全一条**已完成**任务的完整性结论，并交回
// 读到的清单（读不到时 ok 为假）。
//
// 分片清单与完整性只存在于清单文件里（任务行没有这些列），所以只看任务行会把一条被上限
// 截断的完成导出显示成"完成"——规格明确禁止把不完整交付说成成功。因此**读回任务**的
// 两条路径（GetTask 与 ListTasks）都走这里，而不是各写一份判定：同一行任务在两个界面里
// 给出不同结论，本身就是一种谎。
//
// 清单读不到（缺失、损坏、不在本机）时结论是"无从判断"，而不是"完成"：既不知道交付了
// 几个分片，也不知道是否被截断。它写成 Truncated=true 加封闭原因码
// RequestTraceExportIncompleteManifestLost，客户端据此显示"文件已丢失"。
//
// 分片**名字**不由这里回填：哪些名字可以离开服务端取决于调用方（GetTask 是下载路径，
// ListTasks 只给句柄），因此由调用方按读到的清单决定。
func (s *RequestTraceExportService) manifestCompletenessFromManifest(task *RequestTraceExportTask) (RequestTraceExportManifest, bool) {
	if task == nil || task.Status != RequestTraceExportCompleted {
		return RequestTraceExportManifest{}, false
	}
	manifest, err := s.readManifest(task.ID)
	if err != nil {
		task.Truncated = true
		task.IncompleteReason = RequestTraceExportIncompleteManifestLost
		task.SkippedByReason = nil
		return RequestTraceExportManifest{}, false
	}
	// 完整性结论与分类型计数都只存在于清单里：读得到就按它说，读不到就是上面那条
	// "无从判断"，绝不用零值冒充满分交付。
	task.Truncated = !manifest.Complete
	task.IncompleteReason = manifest.Reason
	task.SkippedByReason = normalizeRequestTraceExportSkipCounts(manifest.SkippedByReason)
	return manifest, true
}

// ListTasks 返回**本管理员登录会话**在本实例上创建过的导出任务的一页，最近创建的在前。
//
// 它是找回接缝：刷新或离开页面后，操作员仍能拿回自己的句柄。会话就是凭据——
// 调用方给不出"另一个会话"的参数，服务端也永不接受任务 ID 之外的定位方式：
// 过滤条件由已验签的主体加上服务自己派生的会话摘要组成。
//
// 续页只认服务自己给出的不透明游标（cursor 为空就是第一页）：畸形、超长或版本
// 不认识的令牌是调用方的问题，必须明确拒绝——绝不能把它当成"没有下一页"，那会把
// 仍然存在的任务说成不存在。第二个返回值是下一页的游标，空串表示"没有下一页了"；
// 只有真的多出一行可读时才给出游标，因此"刚好装满一页"不会被说成"还有更多"。
//
// 与 GetTask 同一门禁：能力、主体、实例三者缺一不可；被拒不是"空列表"，
// 而是各自明确的原因。返回的行剥掉文件名（以及分片/清单名）：句柄里不含任何
// 路径，下载仍要在那一刻按任务 ID 与创建会话重新校验。
// 页大小有界：未给出用默认值，超过上限即拒绝而不是截断。
func (s *RequestTraceExportService) ListTasks(ctx context.Context, actor RequestTraceExportActor, cursor string, limit int) ([]RequestTraceExportTask, string, error) {
	if !s.allowed() {
		return nil, "", ErrRequestTraceExportDisabled
	}
	if !exportActorValid(actor) {
		return nil, "", ErrRequestTraceExportSessionRequired
	}
	if limit <= 0 {
		limit = RequestTraceExportDefaultListedTasks
	}
	if limit > RequestTraceExportMaxListedTasks {
		return nil, "", ErrRequestTraceExportLimit
	}
	var after *RequestTraceExportRecallCursor
	if cursor != "" {
		decoded, err := decodeRequestTraceExportRecallCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		after = decoded
	}
	// 找回必须能翻页：读回一页却不给下一页的存储会把更早的任务悄悄藏起来。
	// 没有这个能力的存储在这里明确失败，而不是退化成"看起来就这么多了"。
	paged, ok := s.store.(RequestTraceExportPagedRecallStore)
	if !ok {
		return nil, "", ErrRequestTraceExportUnavailable
	}
	tasks, more, err := paged.ListForSession(ctx, actor.AdminUserID, sessionDigest(actor.SessionID), s.opts.InstanceID, after, limit)
	if err != nil {
		if errors.Is(err, ErrRequestTraceExportLimit) {
			return nil, "", err
		}
		return nil, "", ErrRequestTraceExportUnavailable
	}
	// 列表只回答"有哪些任务、现在什么状态"，但状态必须是真的：文件名、分片与清单名
	// 是下载那一刻才按任务 ID 换取的，不随列表外泄（这里清空而不是依赖视图漏掉），
	// 而完整性结论按行从清单读回——否则一条被截断的完成导出会显示成"完成"。
	for i := range tasks {
		tasks[i].Filename = ""
		tasks[i].Shards = nil
		tasks[i].Manifest = ""
		s.manifestCompletenessFromManifest(&tasks[i])
	}
	// 下一页的游标由**这一页最后一行**的排序键组成。没有更多行时不给游标，
	// 客户端据此停止，而不是先请求一个空页再自己判断。
	next := ""
	if more && len(tasks) > 0 {
		last := tasks[len(tasks)-1]
		next = encodeRequestTraceExportRecallCursor(RequestTraceExportRecallCursor{CreatedAt: last.CreatedAt, ExportID: last.ID})
	}
	return tasks, next, nil
}

func (s *RequestTraceExportService) RunOnce(ctx context.Context) (RequestTraceExportTask, error) {
	if !s.allowed() {
		return RequestTraceExportTask{}, ErrRequestTraceExportDisabled
	}
	if !s.mu.TryLock() {
		return RequestTraceExportTask{}, ErrRequestTraceExportLimit
	}
	defer s.mu.Unlock()
	// 先认领任务，再按**这个任务创建时固定的快照**确定预算，最后才起运行超时：
	// 预算属于任务本身，不属于这一刻的全局配置，也不能由另一个任务或会话改写。
	task, err := s.store.Claim(ctx, s.opts.InstanceID)
	if err != nil {
		return RequestTraceExportTask{}, err
	}
	if task.Status != RequestTraceExportRunning || !requestTraceExportIDShape.MatchString(task.ID) || task.InstanceID != s.opts.InstanceID {
		return RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
	}
	bounds := s.boundsForTask(task)
	ctx, cancel := context.WithTimeout(ctx, bounds.MaxRuntime)
	defer cancel()
	filename, err := s.generate(ctx, &task, bounds)
	if err != nil {
		task.Status = RequestTraceExportFailed
		if filename != "" {
			_ = os.Remove(filename)
		}
		// Persist only a state, never a path, body, URL or DB error text.
		finishCtx, finishCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer finishCancel()
		if persistErr := s.store.Finish(finishCtx, task); persistErr != nil {
			return task, ErrRequestTraceExportUnavailable
		}
		return task, err
	}
	task.Filename = filepath.Base(filename)
	task.Status = RequestTraceExportCompleted
	completed := s.now().UTC()
	task.CompletedAt = &completed
	until := completed.Add(RequestTraceExportDownloadWindow)
	task.DownloadUntil = &until
	if err := s.store.Finish(ctx, task); err != nil {
		_ = os.Remove(filename)
		return task, ErrRequestTraceExportUnavailable
	}
	return task, nil
}

// generate 写出这次导出的分片与清单，返回清单路径（任务凭 ID 校验后读取分片）。
//
// bounds 是这次任务实际使用的预算，来自任务创建时固定的快照（见 RunOnce 与
// boundsForTask）：它随调用传入，不放在服务字段上，因此一个任务的边界不会被
// 另一个任务或并发调用改写。
//
// 行为约定：
//   - 每条记录按与 Trace 详情相同的允许披露投影写一行 JSONL；
//   - 达到单分片上限就封口并开下一个分片，而不是失败；
//   - 达到整任务上限、分片数上限、Runtime 用尽，或发现源记录消失/读取失败时，
//     **保留已完成分片**，把结果标为"不完整"并写明封闭原因码，绝不假装是全量。
//     分片数上限与其它资源上限一样只是"到此为止"：它不把整次任务判成失败，
//     已写出的分片照常交付。
func (s *RequestTraceExportService) generate(ctx context.Context, task *RequestTraceExportTask, bounds requestTraceExportBounds) (string, error) {
	manifestPath := filepath.Join(s.opts.TempDir, exportManifestBase(task.ID))
	// 上一次进程可能在任意位置中断：只清理属于本任务 ID 的普通文件，绝不跟随链接。
	if err := removeOwnedExportFile(manifestPath); err != nil {
		return "", ErrRequestTraceExportUnavailable
	}
	shards := make([]RequestTraceExportShard, 0, 4)
	cursor := ""
	shardRows, shardBytes := int64(0), int64(0)
	var file *os.File
	// sealCurrent 把当前分片已经写出的行数与字节数记进清单条目：
	// 清单必须能回答"每一片有多少条"，否则无法核对交付内容。
	sealCurrent := func() {
		if len(shards) == 0 {
			return
		}
		shards[len(shards)-1].Rows = shardRows
		shards[len(shards)-1].Bytes = shardBytes
	}
	// shardCapReached 报告是否已经写满了允许的分片数。它是"写到哪为止"的界限，
	// 与整任务上限同类：触顶只决定这次导出到此为止，不决定整次任务算失败。
	shardCapReached := func() bool { return len(shards) >= requestTraceExportMaxShards }
	openShard := func() error {
		// 分片数本身也有上限：否则"任务上限很大"会变成无限多的文件。
		// 写入路径在封口开新片之前就先判一次（见下面的 limit_shards 分支），
		// 这里保留的是同一界限的最后一道闸：任何调用点都不能绕过它开片。
		if shardCapReached() {
			return ErrRequestTraceExportLimit
		}
		name := exportShardBase(task.ID, len(shards)+1)
		path := filepath.Join(s.opts.TempDir, name)
		if err := removeOwnedExportFile(path); err != nil {
			return ErrRequestTraceExportUnavailable
		}
		opened, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return ErrRequestTraceExportUnavailable
		}
		file = opened
		shards = append(shards, RequestTraceExportShard{Name: name})
		shardRows, shardBytes = 0, 0
		return nil
	}
	sealShard := func() error {
		if file == nil {
			return nil
		}
		if err := file.Sync(); err != nil {
			return ErrRequestTraceExportUnavailable
		}
		if err := file.Close(); err != nil {
			return ErrRequestTraceExportUnavailable
		}
		file = nil
		return nil
	}
	// delivered 只在"分片与清单都写成功"时置位：未交付之前任何退出路径都必须
	// 清掉半成品，否则下一次同 ID 的运行或运维手工翻目录都会看到不完整文件。
	delivered := false
	defer func() {
		if delivered {
			return
		}
		if file != nil {
			_ = file.Close()
			file = nil
		}
		for _, shard := range shards {
			_ = os.Remove(filepath.Join(s.opts.TempDir, shard.Name))
		}
		_ = os.Remove(manifestPath)
	}()

	if err := openShard(); err != nil {
		return "", err
	}
	markIncomplete := func(reason string) {
		if task.IncompleteReason == "" {
			task.IncompleteReason = reason
		}
		task.Truncated = true
	}
	// 分类型的跳过计数与聚合数一起交付：它只在清单里持久化，任务行没有这一列，
	// 读回任务时从清单恢复，因此不需要数据库迁移（见 GetTask / readManifest）。
	task.SkippedByReason = newRequestTraceExportSkipCounts()

	for {
		if ctx.Err() != nil {
			markIncomplete(RequestTraceExportIncompleteLimitRuntime)
			break
		}
		ids, next, err := s.source.NextTracePage(ctx, task.Filter, cursor, requestTraceExportPageSize)
		if err != nil {
			return "", ErrRequestTraceExportUnavailable
		}
		if len(ids) == 0 {
			break
		}
		if len(ids) > requestTraceExportPageSize || next == "" || next == cursor {
			return "", ErrRequestTraceExportLimit
		}
		stop := false
		for _, id := range ids {
			if !requestTraceExportIDShape.MatchString(id) {
				return "", ErrRequestTraceExportUnavailable
			}
			if task.RowsExported+task.RowsSkipped >= bounds.MaxRows {
				markIncomplete(RequestTraceExportIncompleteLimitRows)
				stop = true
				break
			}
			detail, available, err := s.source.ReadApprovedDetail(ctx, id)
			if err != nil {
				// 单条记录读不出来不等于整次导出失败：跳过它、计入跳过数、标不完整，
				// 继续把后续能读到的记录写完。已经写出的分片照常交付，清单用封闭
				// 原因码区分"读失败"与"记录消失"，绝不假装它是全量。
				task.RowsSkipped++
				task.SkippedByReason.record(RequestTraceExportIncompleteReadFailed)
				markIncomplete(RequestTraceExportIncompleteReadFailed)
				continue
			}
			if !available {
				// 枚举之后消失的源记录：计入跳过数并标不完整，不假装它从未存在。
				task.RowsSkipped++
				task.SkippedByReason.record(RequestTraceExportIncompleteSourceGone)
				markIncomplete(RequestTraceExportIncompleteSourceGone)
				continue
			}
			if detail.TraceID != id || len(detail.Stages) > 1000 {
				return "", ErrRequestTraceExportUnavailable
			}
			for _, stage := range detail.Stages {
				if stage.Ordinal <= 0 || stage.Ordinal > 1000 || stage.AttemptIndex < 0 || stage.AttemptIndex > 1000 ||
					len(stage.PayloadText) > 1<<20 || stage.ObservedBytes < 0 || stage.RetainedBytes < 0 || stage.RetainedBytes > 1<<20 ||
					stage.DroppedEvents < 0 || len(stage.Stage) > 64 || len(stage.Reason) > 96 || len(stage.ViewName) > 48 ||
					!ValidRequestTraceStageFacts(stage.Stage, stage.Facts) ||
					(stage.Stage == RequestTraceDecisionStage && !ValidRequestTraceDecisionFacts(stage.Decision)) ||
					(stage.Stage != RequestTraceDecisionStage && stage.Decision != nil) {
					return "", ErrRequestTraceExportLimit
				}
			}
			line, err := json.Marshal(detail)
			if err != nil {
				return "", ErrRequestTraceExportUnavailable
			}
			lineBytes := int64(len(line)) + 1
			if lineBytes > bounds.MaxBytes-task.BytesExported {
				markIncomplete(RequestTraceExportIncompleteLimitBytes)
				stop = true
				break
			}
			// 单分片写满即封口：分片边界不影响内容，只影响文件切分。
			if shardRows >= bounds.MaxShardRows || shardBytes+lineBytes > bounds.MaxShardBytes {
				if err := sealShard(); err != nil {
					return "", err
				}
				sealCurrent()
				// 分片数上限与整任务上限同类：它只说明"这一条放不下了"，不说明
				// "整次导出失败"。先封口并记下当前这一片，再把结果标成不完整
				// （封闭原因码 limit_shards）后停止，让已经写好的分片照常交付；
				// 绝不为开第 1001 片把任务判成失败、连已写出的分片一起删掉。
				// 判在开片之前，所以那条记录不会被写进任何分片。
				if shardCapReached() {
					markIncomplete(RequestTraceExportIncompleteLimitShards)
					stop = true
					break
				}
				if err := openShard(); err != nil {
					return "", err
				}
			}
			if _, err := file.Write(line); err != nil {
				return "", ErrRequestTraceExportUnavailable
			}
			if _, err := file.Write([]byte{'\n'}); err != nil {
				return "", ErrRequestTraceExportUnavailable
			}
			task.RowsExported++
			task.BytesExported += lineBytes
			shardRows++
			shardBytes += lineBytes
		}
		if stop {
			break
		}
		cursor = next
		if len(ids) < requestTraceExportPageSize {
			break
		}
	}
	if err := sealShard(); err != nil {
		return "", err
	}
	sealCurrent()
	// 空结果也保留一个空分片：清单描述的"0 行"必须对应真实存在的文件。
	if len(shards) == 0 {
		if err := openShard(); err != nil {
			return "", err
		}
		if err := sealShard(); err != nil {
			return "", err
		}
		sealCurrent()
	}
	manifest, err := s.buildManifest(task, shards)
	if err != nil {
		return "", err
	}
	if err := writeExportManifest(manifestPath, manifest); err != nil {
		return "", ErrRequestTraceExportUnavailable
	}
	shardNames := make([]string, 0, len(shards))
	for _, shard := range shards {
		shardNames = append(shardNames, shard.Name)
	}
	task.Shards = shardNames
	task.Manifest = filepath.Base(manifestPath)
	delivered = true
	return manifestPath, nil
}

func (s *RequestTraceExportService) ExportPath(id string) string {
	if s == nil || !requestTraceExportIDShape.MatchString(id) || s.store == nil {
		return ""
	}
	task, err := s.store.Get(context.Background(), id)
	if err != nil || task.Filename == "" || !validExportFilename(id, task.Filename) {
		return ""
	}
	return filepath.Join(s.opts.TempDir, task.Filename)
}

// 交付形态：若干 JSONL 分片 + 一个清单。清单说明范围、上限与是否完整。
const (
	// RequestTraceExportIncompleteLimitRows 等是给管理端看的封闭原因码：
	// 它们只说明"为什么结果不完整"，不含任何正文或凭据。
	RequestTraceExportIncompleteLimitRows    = "limit_rows"
	RequestTraceExportIncompleteLimitBytes   = "limit_bytes"
	RequestTraceExportIncompleteLimitRuntime = "limit_runtime"
	RequestTraceExportIncompleteSourceGone   = "source_gone"
	// RequestTraceExportIncompleteReadFailed 表示某条记录被枚举后读取失败。
	// 它与"记录消失"是两类事实：前者是读取出问题，后者是行已经不在了。
	RequestTraceExportIncompleteReadFailed = "read_failed"
	// RequestTraceExportIncompleteLimitShards 表示任务已经写满了允许的分片数，
	// 还有记录没导出。它与其它资源上限同类：已写出的分片是完整交付物，缺的是
	// 放不下的那部分，因此绝不把整次任务判成失败。
	RequestTraceExportIncompleteLimitShards = "limit_shards"
	// RequestTraceExportIncompleteManifestLost 不是对源记录的观察，而是对**交付物
	// 自身**的结论：记录完整性的那份清单读不到（被清理、损坏或不在本机），因此既
	// 不知道交付了几个分片，也不知道是否被截断。
	//
	// 它与上面几个原因码同类陈列，但含义不同：前者说"哪里没导完"，它说"无从判断"。
	// 它必须与 Truncated=true 一起出现，任何客户端都不能把这样一行渲染成"完成"。
	RequestTraceExportIncompleteManifestLost = "manifest_lost"
)

// RequestTraceExportSkipCounts 是这次导出按封闭原因码分类型的"跳过/失败"计数。
//
// 它补的是聚合数 RowsSkipped 丢掉的区分：一条被枚举后**读取失败**的记录与一条
// **已经消失**的记录都计入跳过，但它们是两类不同的事实。键只能是
// requestTraceExportSkipReasonCodes 里的原因码，值是该类型的条数——它不含任何
// 正文、凭据、路径或原始错误串。聚合总数仍是 RowsSkipped，清单的首个原因仍是
// IncompleteReason，这个映射只在上面的基础上补上分类型计数。
type RequestTraceExportSkipCounts map[string]int64

// requestTraceExportSkipReasonCodes 是允许出现在跳过计数里的封闭原因码。
// 只有"逐条落空/读失败"这两类事实按行计数；limit_* 说明的是"任务为什么停下"，
// 不是某一行的跳过类型，因此不进这个映射。
var requestTraceExportSkipReasonCodes = []string{
	RequestTraceExportIncompleteReadFailed,
	RequestTraceExportIncompleteSourceGone,
}

func requestTraceExportSkipReasonKnown(code string) bool {
	for _, known := range requestTraceExportSkipReasonCodes {
		if known == code {
			return true
		}
	}
	return false
}

func newRequestTraceExportSkipCounts() RequestTraceExportSkipCounts {
	return make(RequestTraceExportSkipCounts, len(requestTraceExportSkipReasonCodes))
}

// record 给一个封闭原因码加一。未定义的原因码不会被写入：这个映射只回答
// "已知的跳过类型各有多少条"，不接受任何未经定义的原因。
func (c RequestTraceExportSkipCounts) record(code string) {
	if c == nil || !requestTraceExportSkipReasonKnown(code) {
		return
	}
	c[code]++
}

// normalizeRequestTraceExportSkipCounts 只保留封闭原因码上的正计数：读回清单时，
// 本版本不认识的原因码（例如更新版本写出的清单）被丢弃，而不是冒充成一个本
// 版本无法解释的类别；聚合跳过数 RowsSkipped 不受影响，缺失的类型就是 0 条。
func normalizeRequestTraceExportSkipCounts(raw RequestTraceExportSkipCounts) RequestTraceExportSkipCounts {
	counts := make(RequestTraceExportSkipCounts, len(requestTraceExportSkipReasonCodes))
	for code, value := range raw {
		if !requestTraceExportSkipReasonKnown(code) || value <= 0 {
			continue
		}
		counts[code] = value
	}
	return counts
}

// RequestTraceExportManifest 是一份导出的清单：它回答"这次到底导了什么、是否完整"。
type RequestTraceExportManifest struct {
	TaskID  string                    `json:"task_id"`
	Filter  RequestTraceExportFilter  `json:"filter"`
	Shards  []RequestTraceExportShard `json:"shards"`
	Rows    int64                     `json:"rows"`
	Skipped int64                     `json:"skipped"`
	Bytes   int64                     `json:"bytes"`
	// SkippedByReason 按封闭原因码给出跳过/失败的分类型计数（规格 §2.4）。
	// 没有跳过时它是空的（旧清单没有这个键，读回同样是空），聚合总数见 Skipped。
	SkippedByReason RequestTraceExportSkipCounts `json:"skipped_by_reason,omitempty"`
	// Complete 为真才代表"没有发现缺失或上限截断"；非严格快照仍然成立。
	Complete      bool      `json:"complete"`
	Reason        string    `json:"incomplete_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	DownloadUntil time.Time `json:"download_until"`
}

// RequestTraceExportShard 描述一个分片及其在清单里的状态。
type RequestTraceExportShard struct {
	Name  string `json:"name"`
	Rows  int64  `json:"rows"`
	Bytes int64  `json:"bytes"`
}

func exportFileBase(id string) string { return "sub2api-request-trace-export-" + id + ".jsonl" }

// exportShardBase 是分片文件名：序号从 1 开始且固定宽度，便于按名称排序。
func exportShardBase(id string, ordinal int) string {
	return "sub2api-request-trace-export-" + id + "-part" + fmt.Sprintf("%04d", ordinal) + ".jsonl"
}

// exportManifestBase 是清单文件名。
func exportManifestBase(id string) string {
	return "sub2api-request-trace-export-" + id + ".manifest.json"
}

// removeOwnedExportFile 只删除属于本任务、且确实是普通文件的目标；
// 冲突路径、符号链接与目录一律拒绝，避免把别的东西删掉。
func removeOwnedExportFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrRequestTraceExportUnavailable
	}
	return os.Remove(path)
}

// writeExportManifest 以独占方式写清单：已存在就说明冲突，绝不覆盖。
func writeExportManifest(path string, manifest RequestTraceExportManifest) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrRequestTraceExportUnavailable
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return ErrRequestTraceExportUnavailable
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return ErrRequestTraceExportUnavailable
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return ErrRequestTraceExportUnavailable
	}
	return file.Close()
}

// buildManifest 汇总这次导出的分片、条数与完整性结论。
func (s *RequestTraceExportService) buildManifest(task *RequestTraceExportTask, shards []RequestTraceExportShard) (RequestTraceExportManifest, error) {
	manifest := RequestTraceExportManifest{
		TaskID: task.ID, Filter: task.Filter,
		Rows: task.RowsExported, Skipped: task.RowsSkipped, Bytes: task.BytesExported,
		SkippedByReason: normalizeRequestTraceExportSkipCounts(task.SkippedByReason),
		Complete:        !task.Truncated, Reason: task.IncompleteReason,
		CreatedAt: s.now().UTC(),
	}
	manifest.DownloadUntil = manifest.CreatedAt.Add(RequestTraceExportDownloadWindow)
	for _, shard := range shards {
		info, err := os.Stat(filepath.Join(s.opts.TempDir, shard.Name))
		if err != nil || !info.Mode().IsRegular() {
			return RequestTraceExportManifest{}, ErrRequestTraceExportUnavailable
		}
		// 字节数以真实文件为准；行数来自写入计数（那是内容事实，文件系统读不到）。
		manifest.Shards = append(manifest.Shards, RequestTraceExportShard{
			Name: shard.Name, Rows: shard.Rows, Bytes: info.Size(),
		})
	}
	return manifest, nil
}

// validExportFilename 只接受本任务 ID 对应的清单名：文件名来自服务端生成，
// 任何其它取值（含路径分隔符或别的任务 ID）都视为文件丢失而不是"去找找看"。
func validExportFilename(id, filename string) bool {
	return requestTraceExportIDShape.MatchString(id) && filename == exportManifestBase(id)
}

func (s *RequestTraceExportService) OpenDownload(ctx context.Context, actor RequestTraceExportActor, id string) (*os.File, RequestTraceExportTask, error) {
	task, err := s.GetTask(ctx, actor, id)
	if err != nil {
		return nil, RequestTraceExportTask{}, err
	}
	// 三种"还没好"要分开：任务已失败是终态（重试无用），排队/进行中是"稍后再试"，
	// 而已经完成的任务才有窗口与文件可谈。把它们混成一个错误会让失败的任务永远
	// 提示"稍后重试"，也会让轮询中的任务看起来像已过期。
	if task.Status == RequestTraceExportFailed {
		return nil, RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
	}
	if task.Status != RequestTraceExportCompleted || task.CompletedAt == nil || task.DownloadUntil == nil {
		return nil, RequestTraceExportTask{}, ErrRequestTraceExportNotReady
	}
	if !s.now().Before(*task.DownloadUntil) || !s.now().Before(task.CompletedAt.Add(RequestTraceExportDownloadWindow)) {
		return nil, RequestTraceExportTask{}, ErrRequestTraceExportGone
	}
	if !validExportFilename(id, task.Filename) {
		return nil, RequestTraceExportTask{}, ErrRequestTraceExportFileLost
	}
	// 交付清单：它本身是这次导出的一部分，必须能被下载，否则管理员无法判断
	// 结果是否完整、缺了多少。URL 仍由任务 ID 校验，不接受任意路径。
	path := filepath.Join(s.opts.TempDir, exportManifestBase(id))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, RequestTraceExportTask{}, ErrRequestTraceExportFileLost
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, RequestTraceExportTask{}, ErrRequestTraceExportFileLost
		}
		return nil, RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
	}
	return file, task, nil
}

// OpenShardDownload 打开本次导出的一个分片文件。
//
// 分片名由服务端生成、以任务 ID 为前缀，因此调用方只需给出序号；任何不在
// 该任务已生成分片列表里的序号都被拒绝，不能用它枚举目录或读取别的文件。
func (s *RequestTraceExportService) OpenShardDownload(ctx context.Context, actor RequestTraceExportActor, id string, ordinal int) (*os.File, RequestTraceExportShard, RequestTraceExportTask, error) {
	task, err := s.GetTask(ctx, actor, id)
	if err != nil {
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, err
	}
	if task.Status == RequestTraceExportFailed {
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
	}
	if task.Status != RequestTraceExportCompleted || task.CompletedAt == nil || task.DownloadUntil == nil {
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceExportNotReady
	}
	if !s.now().Before(*task.DownloadUntil) || !s.now().Before(task.CompletedAt.Add(RequestTraceExportDownloadWindow)) {
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceExportGone
	}
	if !validExportFilename(id, task.Filename) {
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceExportFileLost
	}
	if ordinal < 1 || ordinal > requestTraceExportMaxShards {
		// 序号本身非法是参数错误，与"容量达到上限"不是同一件事。
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceInvalidRecord
	}
	// 任务行只持久化清单文件名：进程重启后分片列表从清单本身恢复，
	// 这样"刷新页面再下载"不需要把文件名清单塞进数据库。
	shards := task.Shards
	if len(shards) == 0 {
		manifest, readErr := s.readManifest(id)
		if readErr != nil {
			return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, readErr
		}
		for _, shard := range manifest.Shards {
			shards = append(shards, shard.Name)
		}
	}
	if ordinal > len(shards) {
		// 请求了一个本次任务没有生成的分片序号：越界即非法参数。
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceInvalidRecord
	}
	name := shards[ordinal-1]
	if name != exportShardBase(id, ordinal) {
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
	}
	path := filepath.Join(s.opts.TempDir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceExportFileLost
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, RequestTraceExportShard{}, RequestTraceExportTask{}, ErrRequestTraceExportFileLost
	}
	return file, RequestTraceExportShard{Name: name, Bytes: info.Size()}, task, nil
}

// requestTraceExportMaxShards 是一次任务允许生成的分片数上限：
// 它把"任务上限很大但分片无限增长"的情况挡在文件系统之外。
const requestTraceExportMaxShards = 1000

// readManifest 读回本任务已写出的清单。清单缺失或损坏按"文件已丢失"处理：
// 没有清单就无法判断交付是否完整，也就不能给出任何分片。
func (s *RequestTraceExportService) readManifest(id string) (RequestTraceExportManifest, error) {
	if s == nil || !requestTraceExportIDShape.MatchString(id) {
		return RequestTraceExportManifest{}, ErrRequestTraceExportNotFound
	}
	path := filepath.Join(s.opts.TempDir, exportManifestBase(id))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return RequestTraceExportManifest{}, ErrRequestTraceExportFileLost
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return RequestTraceExportManifest{}, ErrRequestTraceExportUnavailable
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return RequestTraceExportManifest{}, ErrRequestTraceExportFileLost
	}
	var manifest RequestTraceExportManifest
	if err := json.Unmarshal(content, &manifest); err != nil || manifest.TaskID != id {
		return RequestTraceExportManifest{}, ErrRequestTraceExportUnavailable
	}
	if len(manifest.Shards) > requestTraceExportMaxShards {
		return RequestTraceExportManifest{}, ErrRequestTraceExportUnavailable
	}
	return manifest, nil
}

func (s *RequestTraceExportService) CleanupExpired(ctx context.Context, limit int) (int64, error) {
	if s == nil || s.store == nil {
		return 0, ErrRequestTraceExportUnavailable
	}
	if !s.opts.SingleInstanceDeclared || s.opts.InstanceID == "" {
		return 0, ErrRequestTraceExportDisabled
	}
	if limit <= 0 || limit > 500 {
		return 0, ErrRequestTraceExportLimit
	}
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	// Running rows abandoned by a process crash have no completed filename yet.
	// Their deterministic ID-scoped partial files are removed by the same sweeper.
	//
	// 判定"卡住"用的运行时长跟随当前生效配置：执行期的上限已经改成按任务快照
	// 取值、不再改写服务字段，所以这里必须自己取配置，否则一个允许跑几小时的
	// 任务会被构造缺省的十分钟阈值当成崩溃残留反复清理。
	stale, err := s.store.ListStale(ctx, s.opts.InstanceID, s.now().Add(-s.currentBounds().MaxRuntime), limit)
	if err != nil {
		return 0, ErrRequestTraceExportUnavailable
	}
	var cleaned int64
	for _, task := range stale {
		if !requestTraceExportIDShape.MatchString(task.ID) {
			continue
		}
		if task.Status == RequestTraceExportPending {
			// A process restart before the first claim leaves no file and an
			// unclaimable instance ID. Mark it terminal without inventing a file.
			task.Status = RequestTraceExportFailed
			if err := s.store.Finish(ctx, task); err != nil {
				return cleaned, ErrRequestTraceExportUnavailable
			}
			cleaned++
			continue
		}
		if task.Status != RequestTraceExportRunning {
			continue
		}
		// 中断的写入器可能留下清单、分片与旧命名的单文件三处残留，全部按本任务 ID
		// 有界清掉；非普通文件一律跳过，不让它拖住其它过期任务的清理。
		blocked := false
		for _, name := range []string{exportManifestBase(task.ID), exportFileBase(task.ID)} {
			path := filepath.Join(s.opts.TempDir, name)
			info, statErr := os.Lstat(path)
			if statErr == nil && !info.Mode().IsRegular() {
				blocked = true
				break
			}
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return cleaned, ErrRequestTraceExportUnavailable
			}
			if statErr == nil {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return cleaned, ErrRequestTraceExportUnavailable
				}
			}
		}
		if blocked {
			continue
		}
		for ordinal := 1; ordinal <= requestTraceExportMaxShards; ordinal++ {
			path := filepath.Join(s.opts.TempDir, exportShardBase(task.ID, ordinal))
			info, statErr := os.Lstat(path)
			if errors.Is(statErr, os.ErrNotExist) {
				break
			}
			if statErr != nil {
				return cleaned, ErrRequestTraceExportUnavailable
			}
			if !info.Mode().IsRegular() {
				break
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return cleaned, ErrRequestTraceExportUnavailable
			}
		}
		task.Status = RequestTraceExportFailed
		if err := s.store.Finish(ctx, task); err != nil {
			return cleaned, ErrRequestTraceExportUnavailable
		}
		cleaned++
	}
	// Walk the expired set from the remembered cursor rather than always
	// rereading the ordering head: one unsafe nonregular entry must not be able
	// to occupy the head of every page and hide the deletable rows behind it,
	// however many of them there are. The cursor advances only past rows this
	// sweep actually examined and wraps once the tail is reached, so each sweep
	// stays bounded by requestTraceExportCleanupScanPage while still eventually
	// visiting every expired row. Deletions remain capped by the caller's limit.
	paged, pagedStore := s.store.(requestTraceExportPagedExpiryStore)
	var tasks []RequestTraceExportTask
	if pagedStore {
		tasks, err = paged.ExpiredAfter(ctx, s.now(), s.cleanupCursor, requestTraceExportCleanupScanPage)
	} else {
		tasks, err = s.store.Expired(ctx, s.now(), requestTraceExportCleanupScanPage)
	}
	if err != nil {
		return cleaned, ErrRequestTraceExportUnavailable
	}
	var expiredDeleted int
	examinedAll := true
	for _, task := range tasks {
		if expiredDeleted >= limit {
			// Stopped by the deletion budget, not by the end of the fetched page:
			// keep the cursor so the next sweep resumes here instead of rereading
			// rows this sweep already examined.
			examinedAll = false
			break
		}
		if pagedStore {
			// Advance past every examined row, including the non-deletable ones,
			// so the next sweep resumes beyond them. A row whose own store call
			// fails is left behind the cursor too: retrying it immediately would
			// wedge the sweep on one unremovable row, so it is retried after the
			// cursor wraps around.
			s.cleanupCursor = task.ID
		}
		if task.Status == RequestTraceExportFailed {
			// Failed tasks never have a download window or completed file.
			if s.now().Before(task.CreatedAt.Add(RequestTraceExportDownloadWindow)) {
				continue
			}
			if err := s.store.Delete(ctx, task.ID); err != nil {
				return cleaned, ErrRequestTraceExportUnavailable
			}
			cleaned++
			expiredDeleted++
			continue
		}
		if task.DownloadUntil == nil || s.now().Before(*task.DownloadUntil) {
			continue
		}
		if validExportFilename(task.ID, task.Filename) {
			// 到期删文件同样覆盖清单与全部分片：只删清单会把明文分片留在临时目录。
			blocked := false
			for _, name := range []string{exportManifestBase(task.ID), exportFileBase(task.ID)} {
				path := filepath.Join(s.opts.TempDir, name)
				info, statErr := os.Lstat(path)
				if statErr == nil && !info.Mode().IsRegular() {
					blocked = true
					break
				}
				if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
					return cleaned, ErrRequestTraceExportUnavailable
				}
				if statErr == nil {
					if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
						return cleaned, ErrRequestTraceExportUnavailable
					}
				}
			}
			if blocked {
				// Keep the unexpected entry and its task, but let other files
				// expire rather than blocking the entire cleanup worker.
				continue
			}
			for ordinal := 1; ordinal <= requestTraceExportMaxShards; ordinal++ {
				path := filepath.Join(s.opts.TempDir, exportShardBase(task.ID, ordinal))
				info, statErr := os.Lstat(path)
				if errors.Is(statErr, os.ErrNotExist) {
					break
				}
				if statErr != nil || !info.Mode().IsRegular() {
					break
				}
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return cleaned, ErrRequestTraceExportUnavailable
				}
			}
		}
		if err := s.store.Delete(ctx, task.ID); err != nil {
			return cleaned, ErrRequestTraceExportUnavailable
		}
		cleaned++
		expiredDeleted++
	}
	if pagedStore && examinedAll && len(tasks) < requestTraceExportCleanupScanPage {
		// The whole fetched page was examined and it was short, so the tail of the
		// expired set was reached: restart the round-robin so rows skipped as
		// non-deletable, and rows that became expired since, are revisited on a
		// later sweep instead of being stranded behind the cursor.
		s.cleanupCursor = ""
	}
	return cleaned, nil
}

// ConsumeExport writes a previously approved response directly to an HTTP writer.
// The caller still owns session authentication and no-store response headers.
func ConsumeExport(dst io.Writer, file *os.File) error {
	if file == nil {
		return ErrRequestTraceExportFileLost
	}
	_, err := io.Copy(dst, file)
	return err
}
