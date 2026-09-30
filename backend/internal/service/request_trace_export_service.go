package service

import (
	"context"
	"crypto/sha256"
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
	CreatedAt     time.Time                `json:"created_at"`
	CompletedAt   *time.Time               `json:"completed_at"`
	DownloadUntil *time.Time               `json:"download_until"`

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

type RequestTraceExportStore interface {
	Create(ctx context.Context, task RequestTraceExportTask, maxInFlight int) error
	Get(ctx context.Context, id string) (RequestTraceExportTask, error)
	ListStale(ctx context.Context, instanceID string, before time.Time, limit int) ([]RequestTraceExportTask, error)
	Claim(ctx context.Context, instanceID string) (RequestTraceExportTask, error)
	Finish(ctx context.Context, task RequestTraceExportTask) error
	Expired(ctx context.Context, before time.Time, limit int) ([]RequestTraceExportTask, error)
	Delete(ctx context.Context, id string) error
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
	TraceID         string                            `json:"trace_id"`
	RouteFamily     string                            `json:"route_family,omitempty"`
	InboundEndpoint string                            `json:"inbound_endpoint,omitempty"`
	CaptureState    string                            `json:"capture_state,omitempty"`
	ClientStatus    int                               `json:"client_status"`
	UsageLogID      *int64                            `json:"usage_log_id,omitempty"`
	Stages          []RequestTraceExportApprovedStage `json:"stages"`
}

type RequestTraceExportApprovedStage struct {
	Ordinal             int                        `json:"ordinal"`
	Stage               string                     `json:"stage"`
	AttemptIndex        int                        `json:"attempt_index"`
	ViewName            string                     `json:"view_name,omitempty"`
	State               string                     `json:"state"`
	Reason              string                     `json:"reason,omitempty"`
	ObservedBytes       int64                      `json:"observed_bytes"`
	RetainedBytes       int                        `json:"retained_bytes"`
	DroppedEvents       int                        `json:"dropped_events"`
	RedactionUnverified bool                       `json:"redaction_unverified"`
	PayloadText         string                     `json:"payload_text,omitempty"`
	Facts               *RequestTraceStageFacts    `json:"facts,omitempty"`
	Decision            *RequestTraceDecisionFacts `json:"decision,omitempty"`
}

// A disappearing row returns available=false; it is counted as skipped rather
// than resurrected. ID iteration must be monotonic and bounded by limit.
type RequestTraceExportSource interface {
	NextTraceIDs(ctx context.Context, filter RequestTraceExportFilter, after string, limit int) ([]string, error)
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
	// 它在每次执行任务前读取一次：改配置只影响之后开始的任务。
	limitsProvider func() RequestTraceExportLimits
}

// SetLimitsProvider 注入任务上限来源。
func (s *RequestTraceExportService) SetLimitsProvider(provider func() RequestTraceExportLimits) {
	if s != nil {
		s.limitsProvider = provider
	}
}

// applyLimits 在任务开始前把当次上限拍成快照：任务执行期间改配置不影响它。
func (s *RequestTraceExportService) applyLimits() {
	if s == nil || s.limitsProvider == nil {
		return
	}
	limits := NormalizeRequestTraceExportLimits(s.limitsProvider())
	if limits.MaxRows > 0 {
		s.opts.MaxRows = limits.MaxRows
	}
	if limits.MaxBytes > 0 {
		s.opts.MaxBytes = limits.MaxBytes
	}
	if limits.Runtime() > 0 {
		s.opts.MaxRuntime = limits.Runtime()
	}
	if limits.MaxShardRows > 0 {
		s.opts.MaxShardRows = limits.MaxShardRows
	}
	if limits.MaxShardBytes > 0 {
		s.opts.MaxShardBytes = limits.MaxShardBytes
	}
	if s.opts.MaxShardRows > s.opts.MaxRows {
		s.opts.MaxShardRows = s.opts.MaxRows
	}
	if s.opts.MaxShardBytes > s.opts.MaxBytes {
		s.opts.MaxShardBytes = s.opts.MaxBytes
	}
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
	if !exportFilterValid(filter) {
		return RequestTraceExportTask{}, ErrRequestTraceExportLimit
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	task := RequestTraceExportTask{
		ID: id, Status: RequestTraceExportPending, Filter: filter, AdminUserID: actor.AdminUserID,
		SessionDigest: sessionDigest(actor.SessionID), InstanceID: s.opts.InstanceID,
		CreatedAt: s.now().UTC(),
	}
	if err := s.store.Create(ctx, task, 1); err != nil {
		if errors.Is(err, ErrRequestTraceExportLimit) {
			return RequestTraceExportTask{}, err
		}
		return RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
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
	// 分片列表与完整性结论只存在于清单里：读得到就回填，读不到就保持为空
	// （客户端据此显示"文件已丢失"，而不是把空分片列表当成"0 条"）。
	if task.Status == RequestTraceExportCompleted {
		if manifest, readErr := s.readManifest(id); readErr == nil {
			names := make([]string, 0, len(manifest.Shards))
			for _, shard := range manifest.Shards {
				names = append(names, shard.Name)
			}
			task.Shards = names
			task.Manifest = exportManifestBase(id)
			task.Truncated = !manifest.Complete
			task.IncompleteReason = manifest.Reason
		}
	}
	return task, nil
}

func (s *RequestTraceExportService) RunOnce(ctx context.Context) (RequestTraceExportTask, error) {
	if !s.allowed() {
		return RequestTraceExportTask{}, ErrRequestTraceExportDisabled
	}
	if !s.mu.TryLock() {
		return RequestTraceExportTask{}, ErrRequestTraceExportLimit
	}
	defer s.mu.Unlock()
	// 上限在任务开始时取一次快照：之后改配置不影响这个已经在跑的任务。
	s.applyLimits()
	ctx, cancel := context.WithTimeout(ctx, s.opts.MaxRuntime)
	defer cancel()
	task, err := s.store.Claim(ctx, s.opts.InstanceID)
	if err != nil {
		return RequestTraceExportTask{}, err
	}
	if task.Status != RequestTraceExportRunning || !requestTraceExportIDShape.MatchString(task.ID) || task.InstanceID != s.opts.InstanceID {
		return RequestTraceExportTask{}, ErrRequestTraceExportUnavailable
	}
	filename, err := s.generate(ctx, &task)
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
// 行为约定：
//   - 每条记录按与 Trace 详情相同的允许披露投影写一行 JSONL；
//   - 达到单分片上限就封口并开下一个分片，而不是失败；
//   - 达到整任务上限、Runtime 用尽，或发现源记录消失/读取失败时，**保留已完成分片**，
//     把结果标为"不完整"并写明封闭原因码，绝不假装是全量。
func (s *RequestTraceExportService) generate(ctx context.Context, task *RequestTraceExportTask) (string, error) {
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
	openShard := func() error {
		// 分片数本身也有上限：否则"任务上限很大"会变成无限多的文件。
		if len(shards) >= requestTraceExportMaxShards {
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

	for {
		if ctx.Err() != nil {
			markIncomplete(RequestTraceExportIncompleteLimitRuntime)
			break
		}
		ids, err := s.source.NextTraceIDs(ctx, task.Filter, cursor, requestTraceExportPageSize)
		if err != nil {
			return "", ErrRequestTraceExportUnavailable
		}
		if len(ids) == 0 {
			break
		}
		if len(ids) > requestTraceExportPageSize {
			return "", ErrRequestTraceExportLimit
		}
		stop := false
		for _, id := range ids {
			if !requestTraceExportIDShape.MatchString(id) || id <= cursor {
				return "", ErrRequestTraceExportUnavailable
			}
			cursor = id
			if task.RowsExported+task.RowsSkipped >= s.opts.MaxRows {
				markIncomplete(RequestTraceExportIncompleteLimitRows)
				stop = true
				break
			}
			detail, available, err := s.source.ReadApprovedDetail(ctx, id)
			if err != nil {
				return "", ErrRequestTraceExportUnavailable
			}
			if !available {
				// 枚举之后消失的源记录：计入跳过数并标不完整，不假装它从未存在。
				task.RowsSkipped++
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
			if lineBytes > s.opts.MaxBytes-task.BytesExported {
				markIncomplete(RequestTraceExportIncompleteLimitBytes)
				stop = true
				break
			}
			// 单分片写满即封口：分片边界不影响内容，只影响文件切分。
			if shardRows >= s.opts.MaxShardRows || shardBytes+lineBytes > s.opts.MaxShardBytes {
				if err := sealShard(); err != nil {
					return "", err
				}
				sealCurrent()
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
)

// RequestTraceExportManifest 是一份导出的清单：它回答"这次到底导了什么、是否完整"。
type RequestTraceExportManifest struct {
	TaskID  string                    `json:"task_id"`
	Filter  RequestTraceExportFilter  `json:"filter"`
	Shards  []RequestTraceExportShard `json:"shards"`
	Rows    int64                     `json:"rows"`
	Skipped int64                     `json:"skipped"`
	Bytes   int64                     `json:"bytes"`
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
		Complete: !task.Truncated, Reason: task.IncompleteReason,
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
	stale, err := s.store.ListStale(ctx, s.opts.InstanceID, s.now().Add(-s.opts.MaxRuntime), limit)
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
