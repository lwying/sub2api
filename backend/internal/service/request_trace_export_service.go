package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
)

var (
	ErrRequestTraceExportDisabled        = errors.New("request trace export disabled")
	ErrRequestTraceExportSessionRequired = errors.New("admin login session required for request trace export")
	ErrRequestTraceExportForbidden       = errors.New("request trace export belongs to a different admin session")
	ErrRequestTraceExportNotFound        = errors.New("request trace export task not found")
	ErrRequestTraceExportGone            = errors.New("request trace export download expired")
	ErrRequestTraceExportFileLost        = errors.New("request trace export file lost")
	ErrRequestTraceExportLimit           = errors.New("request trace export capacity limit exceeded")
	ErrRequestTraceExportUnavailable     = errors.New("request trace export temporarily unavailable")
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
	ClientStatus int        `json:"client_status,omitempty"`
	CreatedFrom  *time.Time `json:"created_from,omitempty"`
	CreatedTo    *time.Time `json:"created_to,omitempty"`
	UsageLinked  *bool      `json:"usage_linked,omitempty"`
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
	Enabled                bool
	SingleInstanceDeclared bool
	InstanceID             string
	TempDir                string
	MaxRows                int64
	MaxBytes               int64
	MaxRuntime             time.Duration
}

type RequestTraceExportService struct {
	store  RequestTraceExportStore
	source RequestTraceExportSource
	opts   RequestTraceExportOptions
	now    func() time.Time
	mu     sync.Mutex // one export writer per process, even when RunOnce is called concurrently

	cleanupMu     sync.Mutex // serialises expiry sweeps and guards cleanupCursor
	cleanupCursor string     // last expired export_id examined; "" restarts a sweep cycle
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
	if opts.TempDir == "" {
		opts.TempDir = os.TempDir()
	}
	return &RequestTraceExportService{store: store, source: source, opts: opts, now: time.Now}
}

func (s *RequestTraceExportService) allowed() bool {
	return s != nil && s.opts.Enabled && s.opts.SingleInstanceDeclared &&
		s.opts.InstanceID != "" && s.store != nil && s.source != nil
}

func sessionDigest(session string) string {
	hash := sha256.Sum256([]byte(session))
	return hex.EncodeToString(hash[:])
}

func exportActorValid(actor RequestTraceExportActor) bool {
	return actor.AdminUserID > 0 && strings.TrimSpace(actor.SessionID) != ""
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
	if filter.ClientStatus < 0 || filter.ClientStatus > 599 {
		return false
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

func (s *RequestTraceExportService) generate(ctx context.Context, task *RequestTraceExportTask) (string, error) {
	// A deterministic, ID-scoped basename lets a restarted process clean up an
	// interrupted writer even before the completed file is committed in the task.
	filename := filepath.Join(s.opts.TempDir, exportFileBase(task.ID))
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		// A prior process could have died mid-write. Never follow or overwrite a
		// conflicting file; only remove a verified regular file for this task ID.
		info, statErr := os.Lstat(filename)
		if statErr != nil || !info.Mode().IsRegular() {
			return "", ErrRequestTraceExportUnavailable
		}
		if removeErr := os.Remove(filename); removeErr != nil {
			return "", ErrRequestTraceExportUnavailable
		}
		file, err = os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	}
	if err != nil {
		return "", ErrRequestTraceExportUnavailable
	}
	fail := func(err error) (string, error) { _ = file.Close(); _ = os.Remove(filename); return "", err }
	defer func() { _ = file.Close() }()
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return fail(ErrRequestTraceExportLimit)
		}
		ids, err := s.source.NextTraceIDs(ctx, task.Filter, cursor, requestTraceExportPageSize)
		if err != nil {
			return fail(ErrRequestTraceExportUnavailable)
		}
		if len(ids) == 0 {
			break
		}
		if len(ids) > requestTraceExportPageSize {
			return fail(ErrRequestTraceExportLimit)
		}
		for _, id := range ids {
			if !requestTraceExportIDShape.MatchString(id) || id <= cursor {
				return fail(ErrRequestTraceExportUnavailable)
			}
			cursor = id
			if task.RowsExported+task.RowsSkipped >= s.opts.MaxRows {
				return fail(ErrRequestTraceExportLimit)
			}
			detail, available, err := s.source.ReadApprovedDetail(ctx, id)
			if err != nil {
				return fail(ErrRequestTraceExportUnavailable)
			}
			if !available {
				task.RowsSkipped++
				continue
			}
			if detail.TraceID != id || len(detail.Stages) > 1000 {
				return fail(ErrRequestTraceExportUnavailable)
			}
			for _, stage := range detail.Stages {
				if stage.Ordinal <= 0 || stage.Ordinal > 1000 || stage.AttemptIndex < 0 || stage.AttemptIndex > 1000 ||
					len(stage.PayloadText) > 1<<20 || stage.ObservedBytes < 0 || stage.RetainedBytes < 0 || stage.RetainedBytes > 1<<20 ||
					stage.DroppedEvents < 0 || len(stage.Stage) > 64 || len(stage.Reason) > 96 || len(stage.ViewName) > 48 ||
					!ValidRequestTraceStageFacts(stage.Stage, stage.Facts) ||
					(stage.Stage == RequestTraceDecisionStage && !ValidRequestTraceDecisionFacts(stage.Decision)) ||
					(stage.Stage != RequestTraceDecisionStage && stage.Decision != nil) {
					return fail(ErrRequestTraceExportLimit)
				}
			}
			line, err := json.Marshal(detail)
			if err != nil {
				return fail(ErrRequestTraceExportUnavailable)
			}
			if int64(len(line))+1 > s.opts.MaxBytes-task.BytesExported {
				return fail(ErrRequestTraceExportLimit)
			}
			if _, err := file.Write(line); err != nil {
				return fail(ErrRequestTraceExportUnavailable)
			}
			if _, err := file.Write([]byte{'\n'}); err != nil {
				return fail(ErrRequestTraceExportUnavailable)
			}
			task.RowsExported++
			task.BytesExported += int64(len(line)) + 1
		}
		if len(ids) < requestTraceExportPageSize {
			break
		}
	}
	if err := file.Sync(); err != nil {
		return fail(ErrRequestTraceExportUnavailable)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(filename)
		return "", ErrRequestTraceExportUnavailable
	}
	return filename, nil
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

func exportFileBase(id string) string { return "sub2api-request-trace-export-" + id + ".jsonl" }

func validExportFilename(id, filename string) bool {
	return requestTraceExportIDShape.MatchString(id) && filename == exportFileBase(id)
}

func (s *RequestTraceExportService) OpenDownload(ctx context.Context, actor RequestTraceExportActor, id string) (*os.File, RequestTraceExportTask, error) {
	task, err := s.GetTask(ctx, actor, id)
	if err != nil {
		return nil, RequestTraceExportTask{}, err
	}
	if task.Status != RequestTraceExportCompleted || task.CompletedAt == nil || task.DownloadUntil == nil ||
		!s.now().Before(*task.DownloadUntil) || !s.now().Before(task.CompletedAt.Add(RequestTraceExportDownloadWindow)) {
		return nil, RequestTraceExportTask{}, ErrRequestTraceExportGone
	}
	if !validExportFilename(id, task.Filename) {
		return nil, RequestTraceExportTask{}, ErrRequestTraceExportFileLost
	}
	path := filepath.Join(s.opts.TempDir, exportFileBase(id))
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
		path := filepath.Join(s.opts.TempDir, exportFileBase(task.ID))
		info, statErr := os.Lstat(path)
		if statErr == nil && !info.Mode().IsRegular() {
			// Never follow or remove an unexpected entry, but do not let it
			// indefinitely block cleanup of every other expired task.
			continue
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return cleaned, ErrRequestTraceExportUnavailable
		}
		if statErr == nil {
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
			path := filepath.Join(s.opts.TempDir, exportFileBase(task.ID))
			info, statErr := os.Lstat(path)
			if statErr == nil && !info.Mode().IsRegular() {
				// Keep the unexpected entry and its task, but let other files
				// expire rather than blocking the entire cleanup worker.
				continue
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
