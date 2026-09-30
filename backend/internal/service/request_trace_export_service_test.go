//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type traceExportStoreStub struct {
	mu                sync.Mutex
	tasks             map[string]RequestTraceExportTask
	expiredScanLimits []int          // page sizes the service requested through ExpiredAfter
	listScanLimits    []int          // page sizes the service requested through ListForSession
	deleteFails       map[string]int // remaining injected Delete failures per export_id
}

func (s *traceExportStoreStub) Create(ctx context.Context, task RequestTraceExportTask, maxInFlight int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = make(map[string]RequestTraceExportTask)
	}
	inFlight := 0
	for _, current := range s.tasks {
		if current.InstanceID == task.InstanceID && (current.Status == RequestTraceExportPending || current.Status == RequestTraceExportRunning) {
			inFlight++
		}
	}
	if inFlight >= maxInFlight {
		return ErrRequestTraceExportLimit
	}
	s.tasks[task.ID] = task
	return nil
}
func (s *traceExportStoreStub) Get(ctx context.Context, id string) (RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return RequestTraceExportTask{}, ErrRequestTraceExportNotFound
	}
	return t, nil
}
func (s *traceExportStoreStub) Claim(ctx context.Context, instanceID string) (RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, task := range s.tasks {
		if task.InstanceID != instanceID || task.Status != RequestTraceExportPending {
			continue
		}
		task.Status = RequestTraceExportRunning
		s.tasks[id] = task
		return task, nil
	}
	return RequestTraceExportTask{}, ErrRequestTraceExportNotFound
}
func (s *traceExportStoreStub) Finish(ctx context.Context, task RequestTraceExportTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = task
	return nil
}
func (s *traceExportStoreStub) ListStale(ctx context.Context, instanceID string, before time.Time, limit int) ([]RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var tasks []RequestTraceExportTask
	for _, task := range s.tasks {
		if (task.Status == RequestTraceExportRunning || task.Status == RequestTraceExportPending) && task.CreatedAt.Before(before) {
			tasks = append(tasks, task)
			if len(tasks) >= limit {
				break
			}
		}
	}
	return tasks, nil
}

// ListForSession mirrors the real store's contract: all three filters are
// applied (admin, session digest, instance), rows come back newest-first with
// export_id descending as the tie-break, and the page is capped. It records the
// limit the service asked for so a test can prove the bound is passed through.
func (s *traceExportStoreStub) ListForSession(ctx context.Context, adminUserID int64, sessionDigest, instanceID string, limit int) ([]RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listScanLimits = append(s.listScanLimits, limit)
	if limit <= 0 || limit > RequestTraceExportMaxListedTasks {
		return nil, ErrRequestTraceExportLimit
	}
	matched := make([]RequestTraceExportTask, 0)
	for _, task := range s.tasks {
		if task.AdminUserID != adminUserID || task.SessionDigest != sessionDigest || task.InstanceID != instanceID {
			continue
		}
		matched = append(matched, task)
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID > matched[j].ID
	})
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}
func (s *traceExportStoreStub) Expired(ctx context.Context, before time.Time, limit int) ([]RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []RequestTraceExportTask
	for _, task := range s.tasks {
		if task.DownloadUntil != nil && !task.DownloadUntil.After(before) ||
			task.Status == RequestTraceExportFailed && !task.CreatedAt.Add(RequestTraceExportDownloadWindow).After(before) {
			expired = append(expired, task)
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].ID < expired[j].ID })
	if len(expired) > limit {
		expired = expired[:limit]
	}
	return expired, nil
}
func (s *traceExportStoreStub) ExpiredAfter(ctx context.Context, before time.Time, after string, limit int) ([]RequestTraceExportTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiredScanLimits = append(s.expiredScanLimits, limit)
	var expired []RequestTraceExportTask
	for _, task := range s.tasks {
		if task.ID <= after {
			continue
		}
		if task.DownloadUntil != nil && !task.DownloadUntil.After(before) ||
			task.Status == RequestTraceExportFailed && !task.CreatedAt.Add(RequestTraceExportDownloadWindow).After(before) {
			expired = append(expired, task)
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].ID < expired[j].ID })
	if len(expired) > limit {
		expired = expired[:limit]
	}
	return expired, nil
}

func (s *traceExportStoreStub) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteFails[id] > 0 {
		s.deleteFails[id]--
		return errors.New("synthetic delete failure")
	}
	delete(s.tasks, id)
	return nil
}

type traceExportSourceStub struct {
	ids     []string
	deleted map[string]bool
	details map[string]RequestTraceExportApprovedDetail
	fails   map[string]bool // ids whose read fails rather than reporting "gone"
}

func (s *traceExportSourceStub) NextTracePage(ctx context.Context, filter RequestTraceExportFilter, after string, limit int) ([]string, string, error) {
	start := 0
	if after != "" {
		for i, id := range s.ids {
			if id == after {
				start = i + 1
				break
			}
		}
	}
	if start >= len(s.ids) {
		return nil, "", nil
	}
	end := min(start+limit, len(s.ids))
	return s.ids[start:end], s.ids[end-1], nil
}
func (s *traceExportSourceStub) ReadApprovedDetail(ctx context.Context, id string) (RequestTraceExportApprovedDetail, bool, error) {
	if s.fails[id] {
		return RequestTraceExportApprovedDetail{}, false, errors.New("synthetic detail read failure")
	}
	if s.deleted[id] {
		return RequestTraceExportApprovedDetail{}, false, nil
	}
	return s.details[id], true, nil
}

// traceExportUnfaithfulSnapshotStoreStub 模拟没有履约的存储：任务创建成功，但
// 创建时固定的上限快照没有如实落库（漏写，或被写成另一份）。服务必须发现这一
// 点并如实拒绝，而不是留下一个"看起来排好队、实际会按别的预算执行"的任务。
type traceExportUnfaithfulSnapshotStoreStub struct {
	traceExportStoreStub
	dropSnapshots    int                       // 剩余要注入"快照没落库"的创建次数
	snapshotOverride *RequestTraceExportLimits // 非 nil 时用它顶替任务自带的快照
}

func (s *traceExportUnfaithfulSnapshotStoreStub) Create(ctx context.Context, task RequestTraceExportTask, maxInFlight int) error {
	switch {
	case s.dropSnapshots > 0:
		s.dropSnapshots--
		task.LimitsSnapshot = nil
	case s.snapshotOverride != nil:
		override := *s.snapshotOverride
		task.LimitsSnapshot = &override
	}
	return s.traceExportStoreStub.Create(ctx, task, maxInFlight)
}

// traceExportManyIDSource 造出 n 个升序、形状合法的 Trace ID 及其详情。
func traceExportManyIDSource(n int) *traceExportSourceStub {
	ids := make([]string, 0, n)
	details := make(map[string]RequestTraceExportApprovedDetail, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%032x", i+1)
		ids = append(ids, id)
		details[id] = RequestTraceExportApprovedDetail{TraceID: id}
	}
	return &traceExportSourceStub{ids: ids, details: details}
}

// 回归：任务创建时固定的上限快照决定它的预算。创建之后管理员把配置改成更大
// 的值，RunOnce 必须仍按创建时的上限执行，而不是按改后的配置全量导出。
func TestRequestTraceExportRunUsesCreationTimeLimitsSnapshotAfterSettingChange(t *testing.T) {
	store := &traceExportStoreStub{}
	source := traceExportManyIDSource(150)
	// A：创建任务时生效的上限——整任务条数取允许的最小值 100。
	live := NormalizeRequestTraceExportLimits(RequestTraceExportLimits{
		MaxRows: RequestTraceExportMinRows, MaxBytes: 1 << 20, MaxRuntimeSec: 60,
		MaxShardRows: RequestTraceExportMinShardRow, MaxShardBytes: 1 << 20, Configured: true,
	})
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.SetLimitsProvider(func() RequestTraceExportLimits { return live })
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)

	// 排队期间管理员改配置：这个值一旦生效就会把全部 150 条导出完。
	live = NormalizeRequestTraceExportLimits(RequestTraceExportLimits{
		MaxRows: RequestTraceExportMaxRows, MaxBytes: RequestTraceExportMaxBytes, MaxRuntimeSec: 600,
		MaxShardRows: 2_000, MaxShardBytes: 32 << 20, Configured: true,
	})

	updated, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportCompleted, updated.Status)
	require.Equal(t, int64(100), updated.RowsExported, "a queued task must keep the limits fixed at creation")
	require.True(t, updated.Truncated)
	require.Equal(t, RequestTraceExportIncompleteLimitRows, updated.IncompleteReason)

	// 落库的仍是创建时那一份，没有被后来的配置改写。
	stored, err := store.Get(context.Background(), task.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.LimitsSnapshot)
	require.Equal(t, int64(RequestTraceExportMinRows), stored.LimitsSnapshot.MaxRows)
}

// 快照随任务在同一次创建里落库：任务刚创建、还没执行时被认领，认领回来的任务
// 就已经带着创建时的上限——不存在"先排队、后补快照"的窗口，因此认领方不可能
// 读到"没有快照"而退回当时的配置。
func TestRequestTraceExportCreatedTaskIsClaimableWithItsSnapshot(t *testing.T) {
	store := &traceExportStoreStub{}
	svc := NewRequestTraceExportService(store, traceExportManyIDSource(1), RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.SetLimitsProvider(func() RequestTraceExportLimits {
		return NormalizeRequestTraceExportLimits(RequestTraceExportLimits{
			MaxRows: RequestTraceExportMinRows, MaxBytes: 1 << 20, MaxRuntimeSec: 60,
			MaxShardRows: RequestTraceExportMinShardRow, MaxShardBytes: 1 << 20, Configured: true,
		})
	})
	_, err := svc.CreateTask(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}, RequestTraceExportFilter{})
	require.NoError(t, err)
	claimed, err := store.Claim(context.Background(), "instance-one")
	require.NoError(t, err)
	require.NotNil(t, claimed.LimitsSnapshot, "a task is claimable only together with its creation-time budget")
	require.Equal(t, int64(RequestTraceExportMinRows), claimed.LimitsSnapshot.MaxRows)
}

// 升级安全：升级前创建的任务没有快照（列为 NULL）。它必须按当前生效配置有界
// 执行——既不因为"读不到快照"而拒绝运行，也不被当成无界。
func TestRequestTraceExportWithoutSnapshotFallsBackToLiveLimits(t *testing.T) {
	store := &traceExportStoreStub{tasks: make(map[string]RequestTraceExportTask)}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.tasks[id] = RequestTraceExportTask{ID: id, Status: RequestTraceExportPending, InstanceID: "instance-one", CreatedAt: time.Now().UTC()}
	source := traceExportManyIDSource(150)
	live := NormalizeRequestTraceExportLimits(RequestTraceExportLimits{
		MaxRows: RequestTraceExportMinRows, MaxBytes: 1 << 20, MaxRuntimeSec: 60,
		MaxShardRows: RequestTraceExportMinShardRow, MaxShardBytes: 1 << 20, Configured: true,
	})
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.SetLimitsProvider(func() RequestTraceExportLimits { return live })

	updated, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportCompleted, updated.Status)
	require.Equal(t, int64(100), updated.RowsExported, "a task with no snapshot runs under the currently effective limits")
	require.Equal(t, RequestTraceExportIncompleteLimitRows, updated.IncompleteReason)
	stored, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Nil(t, stored.LimitsSnapshot, "no snapshot is invented for a legacy row")
}

// "查询全部"的时间上界在**任务创建时**固定：留空时由服务盖章并随任务落库，
// 之后新产生的 Trace 不会被还在排队的任务卷进来；调用方自带范围时不覆盖，
// "导出所选"只认 ID 集合、不加时间上界。
func TestRequestTraceExportCreateStampsTimeUpperBound(t *testing.T) {
	frozen := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	newService := func(store RequestTraceExportStore) *RequestTraceExportService {
		svc := NewRequestTraceExportService(store, traceExportManyIDSource(1), RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
		svc.SetAcknowledgementSatisfiedForTest(true)
		svc.now = func() time.Time { return frozen }
		return svc
	}

	// 留空：盖上创建时刻的界，并随任务持久化。
	store := &traceExportStoreStub{}
	svc := newService(store)
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	require.NotNil(t, task.Filter.CreatedTo)
	require.True(t, task.Filter.CreatedTo.Equal(frozen))
	stored, err := store.Get(context.Background(), task.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Filter.CreatedTo, "the fixed upper bound must be persisted with the task")
	require.True(t, stored.Filter.CreatedTo.Equal(frozen))

	// 调用方给了上界：原样保留。
	given := frozen.Add(-time.Hour)
	svc = newService(&traceExportStoreStub{})
	task, err = svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{CreatedTo: &given})
	require.NoError(t, err)
	require.NotNil(t, task.Filter.CreatedTo)
	require.True(t, task.Filter.CreatedTo.Equal(given))

	// 导出所选：范围是 ID 集合，不加时间上界。
	svc = newService(&traceExportStoreStub{})
	task, err = svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{TraceIDs: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}})
	require.NoError(t, err)
	require.Nil(t, task.Filter.CreatedTo)
}

// 规格 §2.4「筛选与导出使用同一过滤语义」：调用方只给了一个**落在未来**的下界
// 而没有上界时，列表对同一查询返回 200 空集；导出必须同样成功，而不是因为服务
// 自己盖的"创建时刻上界"把区间变成倒置，就在创建时被拒成容量错误（429）。
// 上界仍然要盖章（未来新产生的 Trace 不得被卷进来），显式倒置窗也仍然要被拒。
func TestRequestTraceExportCreateAcceptsFutureCreatedFromWithoutCreatedTo(t *testing.T) {
	frozen := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	future := frozen.Add(time.Hour)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	newService := func() *RequestTraceExportService {
		svc := NewRequestTraceExportService(&traceExportStoreStub{}, traceExportManyIDSource(3), RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
		svc.SetAcknowledgementSatisfiedForTest(true)
		svc.now = func() time.Time { return frozen }
		return svc
	}

	task, err := newService().CreateTask(context.Background(), actor, RequestTraceExportFilter{CreatedFrom: &future})
	require.NoError(t, err, "a future lower bound is an empty result, not a capacity failure")
	require.NotNil(t, task.Filter.CreatedFrom)
	require.True(t, task.Filter.CreatedFrom.Equal(future))
	// 内部上界仍然盖章为创建时刻：新产生的 Trace（created_at >= 现在）不会进入。
	require.NotNil(t, task.Filter.CreatedTo, "the creation-time upper bound must still be stamped")
	require.True(t, task.Filter.CreatedTo.Equal(frozen))
	// 有效窗口就是空交集：[future, now) 为空。
	require.False(t, task.Filter.CreatedFrom.Before(*task.Filter.CreatedTo))

	// 显式倒置窗（调用方同时给出两端且 From >= To）仍然被拒：不因上面的放宽而消失。
	invertedFrom := frozen.Add(-time.Hour)
	invertedTo := frozen.Add(-2 * time.Hour)
	_, err = newService().CreateTask(context.Background(), actor, RequestTraceExportFilter{CreatedFrom: &invertedFrom, CreatedTo: &invertedTo})
	require.ErrorIs(t, err, ErrRequestTraceExportLimit, "an explicit inverted window stays rejected")
}

// traceExportWindowSourceStub 按 filter 的 created 窗口真实筛选，用来验证"未来
// 下界 + 无上界"的查询在导出侧确实产出 0 行，而不是被不筛选的 stub 掩盖成全量。
type traceExportWindowSourceStub struct {
	entries map[string]time.Time // id -> created_at
	order   []string
}

func (s *traceExportWindowSourceStub) NextTracePage(_ context.Context, filter RequestTraceExportFilter, after string, limit int) ([]string, string, error) {
	start := 0
	if after != "" {
		for i, id := range s.order {
			if id == after {
				start = i + 1
				break
			}
		}
	}
	var ids []string
	for _, id := range s.order[start:] {
		createdAt := s.entries[id]
		if filter.CreatedFrom != nil && createdAt.Before(*filter.CreatedFrom) {
			continue
		}
		// 上界为"不含"，与 SQL 侧 `created_at < $5` 一致。
		if filter.CreatedTo != nil && !createdAt.Before(*filter.CreatedTo) {
			continue
		}
		ids = append(ids, id)
		if len(ids) >= limit {
			break
		}
	}
	if len(ids) == 0 {
		return nil, "", nil
	}
	return ids, ids[len(ids)-1], nil
}

func (s *traceExportWindowSourceStub) ReadApprovedDetail(_ context.Context, id string) (RequestTraceExportApprovedDetail, bool, error) {
	if _, ok := s.entries[id]; !ok {
		return RequestTraceExportApprovedDetail{}, false, nil
	}
	return RequestTraceExportApprovedDetail{TraceID: id}, true, nil
}

// 未来下界 + 无上界的"查询全部"必须端到端产出 0 行、清单标"完整"：这不是
// "被上限截断"，而是"这个查询匹配不到任何记录"，与列表的 200 空集同义。
func TestRequestTraceExportFutureCreatedFromDeliversEmptyCompleteExport(t *testing.T) {
	frozen := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	future := frozen.Add(time.Hour)
	idA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// 已有记录都产生在过去；未来下界匹配不到任何一条。
	source := &traceExportWindowSourceStub{
		entries: map[string]time.Time{idA: frozen.Add(-2 * time.Hour), idB: frozen.Add(-time.Hour)},
		order:   []string{idA, idB},
	}
	store := &traceExportStoreStub{}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return frozen }
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}

	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{CreatedFrom: &future})
	require.NoError(t, err, "the same query returns 200 empty on the list; export must not 429")
	done, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportCompleted, done.Status)
	require.Equal(t, int64(0), done.RowsExported)
	require.False(t, done.Truncated, "an empty match is not a truncation")

	file, _, err := svc.OpenDownload(context.Background(), actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	content, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	var manifest RequestTraceExportManifest
	require.NoError(t, json.Unmarshal(content, &manifest))
	require.True(t, manifest.Complete, "0 rows because the query matched nothing is a complete result")
	require.Zero(t, manifest.Rows)
	require.NotEmpty(t, manifest.Shards, "0 rows still ships a real empty shard plus manifest")
}

// 没有注入配置来源时，生效值就是构造缺省、永不改变：不必为任务冻结快照，执行
// 直接按缺省（而不是归一化后的另一份值）有界进行。
func TestRequestTraceExportWithoutConfiguredSourceUsesDeploymentDefaults(t *testing.T) {
	store := &traceExportStoreStub{}
	svc := NewRequestTraceExportService(store, traceExportManyIDSource(150), RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one", MaxRows: RequestTraceExportMinRows})
	svc.SetAcknowledgementSatisfiedForTest(true)
	task, err := svc.CreateTask(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}, RequestTraceExportFilter{})
	require.NoError(t, err)
	require.Nil(t, task.LimitsSnapshot, "a deployment whose limits can never change has nothing to freeze")
	stored, err := store.Get(context.Background(), task.ID)
	require.NoError(t, err)
	require.Nil(t, stored.LimitsSnapshot)

	updated, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(RequestTraceExportMinRows), updated.RowsExported)
	require.Equal(t, RequestTraceExportIncompleteLimitRows, updated.IncompleteReason)
}

// 创建时固定的快照随任务落库，并把归一化后的值写入（落库上限永远是有限正数）。
func TestRequestTraceExportCreatePersistsNormalizedLimitsSnapshot(t *testing.T) {
	store := &traceExportStoreStub{}
	svc := NewRequestTraceExportService(store, traceExportManyIDSource(1), RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	// 越界值（条数低于下限、时长远超上限）在落库前被夹到允许区间。
	svc.SetLimitsProvider(func() RequestTraceExportLimits {
		return RequestTraceExportLimits{
			MaxRows: RequestTraceExportMinRows / 2, MaxBytes: 8 << 20, MaxRuntimeSec: 999_999,
			MaxShardRows: 3, MaxShardBytes: 4 << 20, Configured: true,
		}
	})
	task, err := svc.CreateTask(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}, RequestTraceExportFilter{})
	require.NoError(t, err)
	stored, err := store.Get(context.Background(), task.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.LimitsSnapshot)
	require.Equal(t, RequestTraceExportMinRows, stored.LimitsSnapshot.MaxRows)
	require.Equal(t, int64(RequestTraceExportMaxRuntimeCap/time.Second), stored.LimitsSnapshot.MaxRuntimeSec)
	require.Equal(t, RequestTraceExportMinShardRow, stored.LimitsSnapshot.MaxShardRows)
}

// 快照没有随任务落库时 CreateTask 必须失败，并且不留下一行占着"每实例一个
// 活动任务"名额的排队任务——否则后续创建会一直被容量上限挡住。
func TestRequestTraceExportCreateRejectsTaskWhenSnapshotCannotPersist(t *testing.T) {
	store := &traceExportUnfaithfulSnapshotStoreStub{dropSnapshots: 1}
	svc := NewRequestTraceExportService(store, traceExportManyIDSource(1), RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.SetLimitsProvider(func() RequestTraceExportLimits { return DefaultRequestTraceExportLimits() })

	_, err := svc.CreateTask(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}, RequestTraceExportFilter{})
	require.ErrorIs(t, err, ErrRequestTraceExportUnavailable)
	require.Empty(t, store.tasks, "a task whose snapshot was not persisted must not be left queued")

	// 名额没有被占住：注入的失败只发生一次，下一次创建应当成功。
	task, err := svc.CreateTask(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}, RequestTraceExportFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, task.ID)
}

// 快照"存在但不等于"创建时的值（存储写错了或写进了另一份配置）同样必须被拒绝：
// 只检查存在性挡不住"预算被悄悄换成别的"。
func TestRequestTraceExportCreateRejectsTaskWhenSnapshotDiffers(t *testing.T) {
	wrong := NormalizeRequestTraceExportLimits(RequestTraceExportLimits{MaxRows: 5_000_000, MaxBytes: 64 << 30, MaxRuntimeSec: 600, MaxShardRows: 2_000, MaxShardBytes: 32 << 20})
	store := &traceExportUnfaithfulSnapshotStoreStub{snapshotOverride: &wrong}
	svc := NewRequestTraceExportService(store, traceExportManyIDSource(1), RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.SetLimitsProvider(func() RequestTraceExportLimits { return DefaultRequestTraceExportLimits() })

	_, err := svc.CreateTask(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}, RequestTraceExportFilter{})
	require.ErrorIs(t, err, ErrRequestTraceExportUnavailable)
	require.Empty(t, store.tasks, "a task whose budget was not persisted exactly must not be left queued")
}

// 过期判定跟随当前生效的运行时长，而不是构造缺省：允许跑几小时的运行中任务
// 不会被十分钟的缺省当成崩溃残留清理掉。
func TestRequestTraceExportCleanupStaleThresholdFollowsEffectiveRuntime(t *testing.T) {
	now := time.Now().UTC()
	id := strings.Repeat("a", 32)
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{id: {
		ID: id, Status: RequestTraceExportRunning, InstanceID: "instance-one", CreatedAt: now.Add(-15 * time.Minute),
	}}}
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: t.TempDir()})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }
	svc.SetLimitsProvider(func() RequestTraceExportLimits {
		return RequestTraceExportLimits{
			MaxRows: 10_000, MaxBytes: 128 << 20, MaxRuntimeSec: int64(6 * time.Hour / time.Second),
			MaxShardRows: 2_000, MaxShardBytes: 32 << 20, Configured: true,
		}
	})
	cleaned, err := svc.CleanupExpired(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, int64(0), cleaned, "a task inside the configured runtime is not stale")
	stored, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportRunning, stored.Status)
}

func TestRequestTraceExportCleanupManyNonregularEntriesDoNotStarveValidExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	until := now.Add(-time.Hour)
	store := &traceExportStoreStub{tasks: make(map[string]RequestTraceExportTask)}
	for i := 0; i < 33; i++ {
		id := fmt.Sprintf("%032x", i)
		store.tasks[id] = RequestTraceExportTask{ID: id, Status: RequestTraceExportCompleted, Filename: exportManifestBase(id), DownloadUntil: &until}
		require.NoError(t, os.Mkdir(filepath.Join(dir, exportManifestBase(id)), 0700))
	}
	goodID := fmt.Sprintf("%032x", 33)
	goodPath := filepath.Join(dir, exportManifestBase(goodID))
	require.NoError(t, os.WriteFile(goodPath, []byte("synthetic"), 0600))
	store.tasks[goodID] = RequestTraceExportTask{ID: goodID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(goodID), DownloadUntil: &until}
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }
	cleaned, err := svc.CleanupExpired(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleaned)
	require.NoFileExists(t, goodPath)
}

// A sweep must keep making bounded progress when far more than one page of
// expired rows is undeletable: it walks the whole set across ticks instead of
// rereading the same first page, and it wraps so a row that becomes deletable
// later is still reached.
func TestRequestTraceExportCleanupWalksPastManyNonregularEntriesAcrossSweeps(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	until := now.Add(-time.Hour)
	const nonregular = 600
	store := &traceExportStoreStub{tasks: make(map[string]RequestTraceExportTask)}
	for i := 0; i < nonregular; i++ {
		id := fmt.Sprintf("%032x", i)
		store.tasks[id] = RequestTraceExportTask{ID: id, Status: RequestTraceExportCompleted, Filename: exportManifestBase(id), DownloadUntil: &until}
		require.NoError(t, os.Mkdir(filepath.Join(dir, exportManifestBase(id)), 0700))
	}
	validID := fmt.Sprintf("%032x", nonregular-1)
	validPath := filepath.Join(dir, exportManifestBase(validID))
	require.NoError(t, os.Remove(validPath))
	require.NoError(t, os.WriteFile(validPath, []byte("synthetic"), 0600))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }

	// One page holds only the unremovable head: nothing is deletable yet, and the
	// sweep must not reread that same page forever.
	cleaned, err := svc.CleanupExpired(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(0), cleaned)
	require.FileExists(t, validPath)

	// The next sweep resumes past the head and reaches the valid row behind it.
	cleaned, err = svc.CleanupExpired(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleaned)
	require.NoFileExists(t, validPath)

	// Once the tail is consumed the cursor wraps to the head, so a head row that
	// becomes deletable is not stranded behind the cursor.
	headID := fmt.Sprintf("%032x", 0)
	headPath := filepath.Join(dir, exportManifestBase(headID))
	require.NoError(t, os.Remove(headPath))
	require.NoError(t, os.WriteFile(headPath, []byte("synthetic"), 0600))
	cleaned, err = svc.CleanupExpired(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleaned)
	require.NoFileExists(t, headPath)

	// Nonregular entries and their task rows are never touched.
	keptID := fmt.Sprintf("%032x", 1)
	require.DirExists(t, filepath.Join(dir, exportManifestBase(keptID)))
	_, err = store.Get(context.Background(), keptID)
	require.NoError(t, err)

	// Every page request stays within the sweep bound: no unbounded query.
	store.mu.Lock()
	limits := append([]int(nil), store.expiredScanLimits...)
	store.mu.Unlock()
	require.NotEmpty(t, limits)
	for _, requested := range limits {
		require.Greater(t, requested, 0)
		require.LessOrEqual(t, requested, requestTraceExportCleanupScanPage)
	}
}

// A short page means the whole expired set was consumed, so the next sweep must
// restart from the head rather than pinning the cursor at the tail: a row
// skipped as non-deletable and then made deletable is still reached.
func TestRequestTraceExportCleanupCursorWrapsToRevisitSkippedEntries(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	until := now.Add(-time.Hour)
	headID, tailID := strings.Repeat("0", 32), strings.Repeat("1", 32)
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{
		headID: {ID: headID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(headID), DownloadUntil: &until},
		tailID: {ID: tailID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(tailID), DownloadUntil: &until},
	}}
	require.NoError(t, os.Mkdir(filepath.Join(dir, exportManifestBase(headID)), 0700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, exportManifestBase(tailID)), 0700))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }

	cleaned, err := svc.CleanupExpired(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(0), cleaned)
	require.Empty(t, svc.cleanupCursor, "a fully consumed sweep must wrap back to the head")

	headPath := filepath.Join(dir, exportManifestBase(headID))
	require.NoError(t, os.Remove(headPath))
	require.NoError(t, os.WriteFile(headPath, []byte("synthetic"), 0600))
	cleaned, err = svc.CleanupExpired(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleaned)
	require.NoFileExists(t, headPath)
	require.DirExists(t, filepath.Join(dir, exportManifestBase(tailID)), "nonregular entry is never removed")
}

// A sweep that stops at its deletion budget has not reached the tail, so it must
// keep its cursor and resume right after the row it last examined. Wrapping to
// the head there would reread rows already checked. With one-per-sweep pages and
// a nonregular head entry, every valid row must still be deleted in order.
func TestRequestTraceExportCleanupShortPagesResumeWithoutResettingCursor(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	until := now.Add(-time.Hour)
	dirID, firstID, secondID := strings.Repeat("0", 32), strings.Repeat("1", 32), strings.Repeat("2", 32)
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{
		dirID:    {ID: dirID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(dirID), DownloadUntil: &until},
		firstID:  {ID: firstID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(firstID), DownloadUntil: &until},
		secondID: {ID: secondID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(secondID), DownloadUntil: &until},
	}}
	require.NoError(t, os.Mkdir(filepath.Join(dir, exportManifestBase(dirID)), 0700))
	firstPath := filepath.Join(dir, exportManifestBase(firstID))
	secondPath := filepath.Join(dir, exportManifestBase(secondID))
	require.NoError(t, os.WriteFile(firstPath, []byte("synthetic"), 0600))
	require.NoError(t, os.WriteFile(secondPath, []byte("synthetic"), 0600))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }

	cleaned, err := svc.CleanupExpired(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleaned)
	require.Equal(t, firstID, svc.cleanupCursor, "a sweep stopped by its deletion budget must resume after the last examined row, not wrap to the head")

	cleaned, err = svc.CleanupExpired(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleaned)
	require.NoFileExists(t, firstPath)
	require.NoFileExists(t, secondPath)
	require.DirExists(t, filepath.Join(dir, exportManifestBase(dirID)), "nonregular entry is never removed")
}

// A store error partway through a sweep must not wedge cleanup: the sweep stays
// bounded, the row whose deletion failed is left for a later sweep, and it is
// still deleted once the cursor reaches it again after wrapping.
func TestRequestTraceExportCleanupRetriesAfterStoreErrorWithoutWedging(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	until := now.Add(-time.Hour)
	firstID, secondID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	store := &traceExportStoreStub{
		tasks: map[string]RequestTraceExportTask{
			firstID:  {ID: firstID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(firstID), DownloadUntil: &until},
			secondID: {ID: secondID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(secondID), DownloadUntil: &until},
		},
		deleteFails: map[string]int{firstID: 1},
	}
	firstPath := filepath.Join(dir, exportManifestBase(firstID))
	secondPath := filepath.Join(dir, exportManifestBase(secondID))
	require.NoError(t, os.WriteFile(firstPath, []byte("synthetic"), 0600))
	require.NoError(t, os.WriteFile(secondPath, []byte("synthetic"), 0600))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }

	cleaned, err := svc.CleanupExpired(context.Background(), 1)
	require.ErrorIs(t, err, ErrRequestTraceExportUnavailable)
	require.Equal(t, int64(0), cleaned)

	// Bounded retries must still drain both rows: the errored row is not skipped
	// forever and the sweep does not stall on it.
	for i := 0; i < 3; i++ {
		if _, retryErr := svc.CleanupExpired(context.Background(), 1); retryErr != nil {
			require.ErrorIs(t, retryErr, ErrRequestTraceExportUnavailable)
		}
	}
	_, err = store.Get(context.Background(), firstID)
	require.ErrorIs(t, err, ErrRequestTraceExportNotFound)
	_, err = store.Get(context.Background(), secondID)
	require.ErrorIs(t, err, ErrRequestTraceExportNotFound)
	require.NoFileExists(t, firstPath)
	require.NoFileExists(t, secondPath)
}

func TestRequestTraceExportCleanupNonregularFirstDoesNotStarveExpiredPages(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	idBad, idGood := strings.Repeat("0", 32), strings.Repeat("1", 32)
	until := now.Add(-time.Hour)
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{
		idBad:  {ID: idBad, Status: RequestTraceExportCompleted, Filename: exportManifestBase(idBad), DownloadUntil: &until},
		idGood: {ID: idGood, Status: RequestTraceExportCompleted, Filename: exportManifestBase(idGood), DownloadUntil: &until},
	}}
	require.NoError(t, os.Mkdir(filepath.Join(dir, exportManifestBase(idBad)), 0700))
	pathGood := filepath.Join(dir, exportManifestBase(idGood))
	require.NoError(t, os.WriteFile(pathGood, []byte("synthetic"), 0600))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }
	cleaned, err := svc.CleanupExpired(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleaned)
	require.NoFileExists(t, pathGood)
}

func TestRequestTraceExportCleanupSkipsNonregularEntriesWithoutBlockingOthers(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	badID := strings.Repeat("a", 32)
	goodID := strings.Repeat("b", 32)
	until := now.Add(-time.Hour)
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{
		badID:  {ID: badID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(badID), DownloadUntil: &until},
		goodID: {ID: goodID, Status: RequestTraceExportCompleted, Filename: exportManifestBase(goodID), DownloadUntil: &until},
	}}
	require.NoError(t, os.Mkdir(filepath.Join(dir, exportManifestBase(badID)), 0700))
	goodPath := filepath.Join(dir, exportManifestBase(goodID))
	require.NoError(t, os.WriteFile(goodPath, []byte("synthetic export"), 0600))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }
	count, err := svc.CleanupExpired(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	_, err = os.Stat(goodPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = store.Get(context.Background(), goodID)
	require.ErrorIs(t, err, ErrRequestTraceExportNotFound)
	require.DirExists(t, filepath.Join(dir, exportManifestBase(badID)), "nonregular entry is never removed")
}

func TestRequestTraceExportCleanupSweepsAfterSwitchIsDisabled(t *testing.T) {
	now := time.Now().UTC()
	id := strings.Repeat("e", 32)
	until := now.Add(-time.Hour)
	dir := t.TempDir()
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{id: {
		ID: id, Status: RequestTraceExportCompleted, Filename: exportManifestBase(id), DownloadUntil: &until,
	}}}
	path := filepath.Join(dir, exportManifestBase(id))
	require.NoError(t, os.WriteFile(path, []byte("synthetic"), 0600))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }
	count, err := svc.CleanupExpired(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.NoFileExists(t, path)
}

func TestRequestTraceExportCleanupRemovesExpiredFailedRow(t *testing.T) {
	now := time.Now().UTC()
	id := strings.Repeat("d", 32)
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{id: {
		ID: id, Status: RequestTraceExportFailed, CreatedAt: now.Add(-8 * 24 * time.Hour),
	}}}
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: t.TempDir()})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.now = func() time.Time { return now }
	count, err := svc.CleanupExpired(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	_, err = store.Get(context.Background(), id)
	require.ErrorIs(t, err, ErrRequestTraceExportNotFound)
}

func TestRequestTraceExportCleanupRequiresSingleInstanceDeclaration(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	id := strings.Repeat("c", 32)
	until := now.Add(-time.Hour)
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{id: {
		ID: id, Status: RequestTraceExportCompleted, Filename: exportManifestBase(id), DownloadUntil: &until,
	}}}
	path := filepath.Join(dir, exportManifestBase(id))
	require.NoError(t, os.WriteFile(path, []byte("synthetic export"), 0600))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{InstanceID: "instance-one", TempDir: dir})
	svc.SetAcknowledgementSatisfiedForTest(true)
	_, err := svc.CleanupExpired(context.Background(), 10)
	require.ErrorIs(t, err, ErrRequestTraceExportDisabled)
	require.FileExists(t, path)
}

func TestRequestTraceExportRestartReapsPendingTaskWithoutFile(t *testing.T) {
	store := &traceExportStoreStub{}
	old := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, InstanceID: "old", TempDir: t.TempDir()})
	old.SetAcknowledgementSatisfiedForTest(true)
	task, err := old.CreateTask(context.Background(), RequestTraceExportActor{AdminUserID: 1, SessionID: "synthetic-session"}, RequestTraceExportFilter{})
	require.NoError(t, err)
	newProcess := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, InstanceID: "new", TempDir: t.TempDir()})
	newProcess.SetAcknowledgementSatisfiedForTest(true)
	newProcess.now = func() time.Time { return task.CreatedAt.Add(11 * time.Minute) }
	count, err := newProcess.CleanupExpired(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	stored, err := store.Get(context.Background(), task.ID)
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportFailed, stored.Status)
}

func TestRequestTraceExportRejectsUnverifiedSingleInstanceAndAbsentAdminSession(t *testing.T) {
	svc := NewRequestTraceExportService(&traceExportStoreStub{}, &traceExportSourceStub{}, RequestTraceExportOptions{TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	_, err := svc.CreateTask(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}, RequestTraceExportFilter{})
	require.ErrorIs(t, err, ErrRequestTraceExportDisabled)
	svc = NewRequestTraceExportService(&traceExportStoreStub{}, &traceExportSourceStub{}, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	_, err = svc.CreateTask(context.Background(), RequestTraceExportActor{AdminUserID: 12}, RequestTraceExportFilter{})
	require.ErrorIs(t, err, ErrRequestTraceExportSessionRequired)
}

func TestRequestTraceExportCompletesAndDeniesOtherAdminSession(t *testing.T) {
	store := &traceExportStoreStub{}
	source := &traceExportSourceStub{ids: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, deleted: map[string]bool{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": true}, details: map[string]RequestTraceExportApprovedDetail{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": {TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Stages: []RequestTraceExportApprovedStage{{Ordinal: 1, Stage: "client_entry", State: "redaction_unverified", PayloadText: "synthetic_prompt", RedactionUnverified: true}}}}}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "admin-session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	updated, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportCompleted, updated.Status)
	require.Equal(t, int64(1), updated.RowsExported)
	require.Equal(t, int64(1), updated.RowsSkipped)
	_, _, err = svc.OpenDownload(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "other-session"}, task.ID)
	require.ErrorIs(t, err, ErrRequestTraceExportForbidden)
	manifestFile, result, err := svc.OpenDownload(context.Background(), actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = manifestFile.Close() })
	manifestContent, err := os.ReadFile(manifestFile.Name())
	require.NoError(t, err)
	var manifest RequestTraceExportManifest
	require.NoError(t, json.Unmarshal(manifestContent, &manifest))
	require.Equal(t, task.ID, manifest.TaskID)
	require.Equal(t, int64(1), manifest.Rows)
	require.Equal(t, int64(1), manifest.Skipped, "a source that disappeared after enumeration is counted, not hidden")
	require.False(t, manifest.Complete, "a vanished source makes the result explicitly incomplete")
	require.Equal(t, RequestTraceExportIncompleteSourceGone, manifest.Reason)
	require.NotEmpty(t, manifest.Shards)
	require.Equal(t, int64(1), result.RowsExported)
	// 清单必须能回答"每一片有多少条"：否则下载方无法核对交付内容。
	var manifestRows int64
	for _, shard := range manifest.Shards {
		require.NotZero(t, shard.Bytes, "a written shard must report its real size")
		manifestRows += shard.Rows
	}
	require.Equal(t, manifest.Rows, manifestRows, "per-shard rows must add up to the manifest total")

	shardFile, _, shardTask, err := svc.OpenShardDownload(context.Background(), actor, task.ID, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shardFile.Close() })
	shardContent, err := os.ReadFile(shardFile.Name())
	require.NoError(t, err)
	require.Contains(t, string(shardContent), "synthetic_prompt")
	require.Contains(t, string(shardContent), `"redaction_unverified":true`)
	require.Equal(t, task.ID, shardTask.ID)
}

// 单条记录读不出来不等于整次导出失败：已完成的（可读的）记录照常交付，清单标
// "不完整"并给出读失败原因与计数，而不是丢掉全部结果、把任务标成失败。
func TestRequestTraceExportReadFailureDeliversPartialAsIncomplete(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{}
	good, bad := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	source := &traceExportSourceStub{
		ids:     []string{good, bad},
		details: map[string]RequestTraceExportApprovedDetail{good: {TraceID: good}, bad: {TraceID: bad}},
		fails:   map[string]bool{bad: true},
	}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)

	updated, err := svc.RunOnce(context.Background())
	require.NoError(t, err, "a per-record read failure is a delivered partial result, not a failed task")
	require.Equal(t, RequestTraceExportCompleted, updated.Status)
	require.Equal(t, int64(1), updated.RowsExported)
	require.Equal(t, int64(1), updated.RowsSkipped, "an unreadable record is counted, not hidden")
	require.True(t, updated.Truncated)
	require.Equal(t, RequestTraceExportIncompleteReadFailed, updated.IncompleteReason)

	// 清单与已写出的分片都能下载：分片里只有读成功的那条。
	manifestFile, _, err := svc.OpenDownload(context.Background(), actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = manifestFile.Close() })
	manifestContent, err := os.ReadFile(manifestFile.Name())
	require.NoError(t, err)
	var manifest RequestTraceExportManifest
	require.NoError(t, json.Unmarshal(manifestContent, &manifest))
	require.False(t, manifest.Complete, "a read failure must never be presented as a full export")
	require.Equal(t, RequestTraceExportIncompleteReadFailed, manifest.Reason)
	require.Equal(t, int64(1), manifest.Skipped)

	shard, _, _, err := svc.OpenShardDownload(context.Background(), actor, task.ID, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = shard.Close() })
	content, err := os.ReadFile(shard.Name())
	require.NoError(t, err)
	require.Contains(t, string(content), good)
	require.NotContains(t, string(content), bad)
}

// 导出与 Trace 详情同款字段：信封时间（创建/完成/清理）、请求时分组与客户端请求
// 模型都必须真的写进 JSONL，不能因为"先前导出遗漏"就把它们丢掉；同时不能比
// 详情多出字段（阶段没有 created_at，导出也不加）。
func TestRequestTraceExportJsonlCarriesDetailEnvelopeFields(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	created := time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)
	completed := created.Add(2 * time.Second)
	cleanup := created.Add(30 * 24 * time.Hour)
	groupID := int64(7)
	source := &traceExportSourceStub{ids: []string{id}, details: map[string]RequestTraceExportApprovedDetail{id: {
		TraceID: id, RouteFamily: "messages", InboundEndpoint: "/v1/messages", CaptureState: "stored",
		ClientStatus: 200, CreatedAt: created, CompletedAt: &completed, CleanupAfter: &cleanup,
		GroupID: &groupID, RequestedModel: "claude-sonnet-4-5",
		Stages: []RequestTraceExportApprovedStage{{
			Ordinal: 1, Stage: "client_entry", State: "stored", PayloadText: "synthetic_prompt",
		}},
	}}}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	_, err = svc.RunOnce(context.Background())
	require.NoError(t, err)
	file, _, _, err := svc.OpenShardDownload(context.Background(), actor, task.ID, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	content, err := os.ReadFile(file.Name())
	require.NoError(t, err)

	var line map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(string(content))), &line))
	for _, key := range []string{"created_at", "completed_at", "cleanup_after", "group_id", "requested_model", "stages"} {
		require.Contains(t, line, key, "the export line must carry %s", key)
	}
	require.JSONEq(t, `"2026-09-30T01:02:03Z"`, string(line["created_at"]))
	require.JSONEq(t, `"2026-09-30T01:02:05Z"`, string(line["completed_at"]))
	require.JSONEq(t, `"2026-10-30T01:02:03Z"`, string(line["cleanup_after"]))
	require.JSONEq(t, `7`, string(line["group_id"]))
	require.JSONEq(t, `"claude-sonnet-4-5"`, string(line["requested_model"]))
	// 详情视图的阶段没有 created_at，导出也不许凭空多出它。
	require.NotContains(t, string(line["stages"]), "created_at")
}

// 详情同款字段契约是**键**层面的契约，不是"有值才给"。Trace 详情恒定输出的信封键
// （route_family / inbound_endpoint / capture_state / usage_log_id / completed_at
// / cleanup_after）在导出里也必须恒定存在，nil 或空时输出 null / ""；只有详情本身
// 带 omitempty 的字段（group_id / requested_model / payload_text / facts / decision）
// 才允许在导出里同样缺席。阶段同款：view_name 与 reason 恒定存在。
func TestRequestTraceExportJsonlKeepsDetailKeysWhenFactsAreMissing(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{}
	id := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	created := time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)
	// 这条记录刻意"什么都没有"：未观察到分组与客户端模型、没有关联 usage、也没有
	// 完成/清理时间。详情在同样输入下仍然输出那些键，导出必须逐键一致。
	source := &traceExportSourceStub{ids: []string{id}, details: map[string]RequestTraceExportApprovedDetail{id: {
		TraceID: id, CreatedAt: created,
		Stages: []RequestTraceExportApprovedStage{{Ordinal: 1, Stage: "client_entry", State: "not_observed"}},
	}}}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	_, err = svc.RunOnce(context.Background())
	require.NoError(t, err)
	file, _, _, err := svc.OpenShardDownload(context.Background(), actor, task.ID, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	content, err := os.ReadFile(file.Name())
	require.NoError(t, err)

	var line map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(string(content))), &line))
	// GET Trace detail 恒定输出的信封键，导出一个都不能少。
	for _, key := range []string{"trace_id", "route_family", "inbound_endpoint", "capture_state", "client_status", "usage_log_id", "created_at", "completed_at", "cleanup_after", "stages"} {
		require.Contains(t, line, key, "the export line must carry %s even when the fact is absent", key)
	}
	require.JSONEq(t, `null`, string(line["usage_log_id"]))
	require.JSONEq(t, `null`, string(line["completed_at"]))
	require.JSONEq(t, `null`, string(line["cleanup_after"]))
	require.JSONEq(t, `""`, string(line["route_family"]))
	require.JSONEq(t, `""`, string(line["inbound_endpoint"]))
	require.JSONEq(t, `""`, string(line["capture_state"]))
	// 详情带 omitempty 的字段，导出同样保持缺席：不多一个键，也不少一个键。
	for _, key := range []string{"group_id", "requested_model"} {
		require.NotContains(t, line, key, "the export line must omit %s exactly like the detail view", key)
	}

	var stages []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(line["stages"], &stages))
	require.Len(t, stages, 1)
	for _, key := range []string{"ordinal", "stage", "attempt_index", "view_name", "state", "reason", "observed_bytes", "retained_bytes", "dropped_events", "redaction_unverified"} {
		require.Contains(t, stages[0], key, "the export stage must carry %s even when it is empty", key)
	}
	require.JSONEq(t, `""`, string(stages[0]["view_name"]))
	require.JSONEq(t, `""`, string(stages[0]["reason"]))
	for _, key := range []string{"payload_text", "facts", "decision"} {
		require.NotContains(t, stages[0], key, "the export stage must omit %s exactly like the detail view", key)
	}
	// 详情视图的阶段没有 created_at，导出也不许凭空多出它。
	require.NotContains(t, stages[0], "created_at")
}

// 分片序号必须落在本次任务真正生成的分片里：越界或跨任务枚举一律拒绝，
// 不能借它读目录里的别的文件。
func TestRequestTraceExportShardDownloadRejectsUnlistedOrdinal(t *testing.T) {
	store := &traceExportStoreStub{}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	source := &traceExportSourceStub{ids: []string{id}, details: map[string]RequestTraceExportApprovedDetail{id: {TraceID: id}}}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	_, err = svc.RunOnce(context.Background())
	require.NoError(t, err)

	_, _, _, err = svc.OpenShardDownload(context.Background(), actor, task.ID, 0)
	require.ErrorIs(t, err, ErrRequestTraceInvalidRecord, "an out-of-range shard index is an invalid parameter, not a capacity limit")
	_, _, _, err = svc.OpenShardDownload(context.Background(), actor, task.ID, 2)
	require.ErrorIs(t, err, ErrRequestTraceInvalidRecord)
	// 换一个会话也不能借任务 ID 读到分片。
	_, _, _, err = svc.OpenShardDownload(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "other"}, task.ID, 1)
	require.ErrorIs(t, err, ErrRequestTraceExportForbidden)
}

func TestRequestTraceExportMissingFileAndExpiredDownload(t *testing.T) {
	store := &traceExportStoreStub{}
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	_, err = svc.RunOnce(context.Background())
	require.NoError(t, err)
	path := svc.ExportPath(task.ID)
	require.NoError(t, os.Remove(path))
	_, _, err = svc.OpenDownload(context.Background(), actor, task.ID)
	require.ErrorIs(t, err, ErrRequestTraceExportFileLost)
	// A physical file left behind after its deadline must not remain downloadable.
	task, err = store.Get(context.Background(), task.ID)
	require.NoError(t, err)
	past := time.Now().Add(-time.Second)
	task.DownloadUntil = &past
	require.NoError(t, store.Finish(context.Background(), task))
	_, _, err = svc.OpenDownload(context.Background(), actor, task.ID)
	require.ErrorIs(t, err, ErrRequestTraceExportGone)
}

func TestRequestTraceExportCannotExtendDownloadWindowPastSevenDays(t *testing.T) {
	store := &traceExportStoreStub{}
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	_, err = svc.RunOnce(context.Background())
	require.NoError(t, err)
	task, err = store.Get(context.Background(), task.ID)
	require.NoError(t, err)
	past := time.Now().Add(-8 * 24 * time.Hour)
	farFuture := time.Now().Add(time.Hour)
	task.CompletedAt = &past
	task.DownloadUntil = &farFuture
	require.NoError(t, store.Finish(context.Background(), task))
	_, _, err = svc.OpenDownload(context.Background(), actor, task.ID)
	require.ErrorIs(t, err, ErrRequestTraceExportGone)
}

func TestRequestTraceExportAfterRestartReplacesOnlyItsOwnAbandonedPartial(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{}}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	task := RequestTraceExportTask{ID: id, Status: RequestTraceExportPending, InstanceID: "instance-one", CreatedAt: time.Now()}
	store.tasks[id] = task
	// 上一次进程中断时留下的第一个分片：重启后同一任务必须把它替换掉，
	// 否则会把别人的旧内容当成这次导出的一部分交付。
	partial := filepath.Join(dir, exportShardBase(id, 1))
	require.NoError(t, os.WriteFile(partial, []byte("abandoned_prompt"), 0600))
	source := &traceExportSourceStub{ids: []string{id}, details: map[string]RequestTraceExportApprovedDetail{id: {TraceID: id, Stages: []RequestTraceExportApprovedStage{}}}}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	_, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	content, err := os.ReadFile(partial)
	require.NoError(t, err)
	require.NotContains(t, string(content), "abandoned_prompt")
	require.Contains(t, string(content), id)
}

func TestRequestTraceExportRestartCleansStaleOtherInstanceWithoutFollowingTaskPath(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{}}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.tasks[id] = RequestTraceExportTask{ID: id, Status: RequestTraceExportRunning, InstanceID: "old-instance", Filename: "../other-file", CreatedAt: time.Now().Add(-time.Hour)}
	partial := filepath.Join(dir, exportFileBase(id))
	require.NoError(t, os.WriteFile(partial, []byte("interrupted-plaintext"), 0600))
	unrelated := filepath.Join(dir, "other-file")
	require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0600))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "new-instance"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	count, err := svc.CleanupExpired(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	_, err = os.Stat(partial)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(unrelated)
	require.NoError(t, err)
}

func TestRequestTraceExportCleansAbandonedPartialOnlyWithinOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{tasks: map[string]RequestTraceExportTask{}}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	task := RequestTraceExportTask{ID: id, Status: RequestTraceExportRunning, InstanceID: "instance-one", CreatedAt: time.Now().Add(-time.Hour)}
	store.tasks[id] = task
	partial := filepath.Join(dir, exportFileBase(id))
	require.NoError(t, os.WriteFile(partial, []byte("synthetic_prompt"), 0600))
	unrelated := filepath.Join(dir, "leave-this-file")
	require.NoError(t, os.WriteFile(unrelated, []byte("safe"), 0600))
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	count, err := svc.CleanupExpired(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	_, err = os.Stat(partial)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(unrelated)
	require.NoError(t, err)
	updated, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportFailed, updated.Status)
}

func TestRequestTraceExportRefusesSymlinkInsteadOfDownloadingOtherFile(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows symlink creation may require privilege")
	}
	dir := t.TempDir()
	store := &traceExportStoreStub{}
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	_, err = svc.RunOnce(context.Background())
	require.NoError(t, err)
	path := svc.ExportPath(task.ID)
	require.NoError(t, os.Remove(path))
	secret := filepath.Join(dir, "not-an-export")
	require.NoError(t, os.WriteFile(secret, []byte("credential"), 0600))
	require.NoError(t, os.Symlink(secret, path))
	_, _, err = svc.OpenDownload(context.Background(), actor, task.ID)
	require.ErrorIs(t, err, ErrRequestTraceExportUnavailable)
}

func TestRequestTraceExportAdmitsAtMostOnePendingTaskPerInstance(t *testing.T) {
	store := &traceExportStoreStub{}
	svc := NewRequestTraceExportService(store, &traceExportSourceStub{}, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	_, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	_, err = svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.ErrorIs(t, err, ErrRequestTraceExportLimit)
}

func TestRequestTraceExportRejectsUnboundedApprovedDetail(t *testing.T) {
	store := &traceExportStoreStub{}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	source := &traceExportSourceStub{ids: []string{id}, details: map[string]RequestTraceExportApprovedDetail{id: {TraceID: id, Stages: []RequestTraceExportApprovedStage{{Ordinal: 1, Stage: "client_entry", State: "stored", PayloadText: strings.Repeat("x", 1<<20+1)}}}}}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: t.TempDir(), InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	_, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)
	updated, err := svc.RunOnce(context.Background())
	require.ErrorIs(t, err, ErrRequestTraceExportLimit)
	require.Equal(t, RequestTraceExportFailed, updated.Status)
}

// 达到整任务上限不再丢弃一切：已完成的分片照常交付，但清单必须标明不完整。
func TestRequestTraceExportLimitDeliversPartialAsIncomplete(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	source := &traceExportSourceStub{ids: []string{id}, details: map[string]RequestTraceExportApprovedDetail{id: {TraceID: id, Stages: []RequestTraceExportApprovedStage{{Ordinal: 1, Stage: "client_entry", State: "stored", PayloadText: strings.Repeat("x", 100)}}}}}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one", MaxBytes: 64})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)

	updated, err := svc.RunOnce(context.Background())
	require.NoError(t, err, "hitting the task bound is a delivered partial result, not a failure")
	require.Equal(t, RequestTraceExportCompleted, updated.Status)
	require.True(t, updated.Truncated)
	require.Equal(t, int64(0), updated.RowsExported, "a row that would exceed the task bound is not written")

	file, _, err := svc.OpenDownload(context.Background(), actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	content, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	var manifest RequestTraceExportManifest
	require.NoError(t, json.Unmarshal(content, &manifest))
	require.False(t, manifest.Complete)
	require.Equal(t, RequestTraceExportIncompleteLimitBytes, manifest.Reason)
	require.NotEmpty(t, manifest.Shards, "an empty but real shard still ships with the manifest")
}

// 分片数触顶（第 1001 片）与其它资源上限同类：它是"到这里为止"，不是"整次任务
// 失败"。已经封口的 1000 个分片是完整、可下载的交付物，清单标"不完整"并写明
// limit_shards；第 1001 个分片既不产生文件，也不出现在任何列表里。
//
// 这条路径必须真的开出 1000 个文件（每片一次 Sync），所以用最小预算跑：每片只
// 放 1 行、其余上限足够大，1001 条记录即可触顶，而不必写满 10001 行；用例串行，
// 不在任何 t.Parallel 分组里。
func TestRequestTraceExportShardCapDeliversCompletedShardsAsIncomplete(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{}
	// 比上限多一条：第 1001 条就是"放不下"的那条。
	source := traceExportManyIDSource(requestTraceExportMaxShards + 1)
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{
		Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one",
		MaxRows: RequestTraceExportMaxRows, MaxBytes: 1 << 30, MaxRuntime: time.Minute,
		MaxShardRows: 1, MaxShardBytes: 1 << 30,
	})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)

	updated, err := svc.RunOnce(context.Background())
	require.NoError(t, err, "reaching the shard budget is a delivered partial result, not a failed task")
	require.Equal(t, RequestTraceExportCompleted, updated.Status)
	require.True(t, updated.Truncated)
	require.Equal(t, RequestTraceExportIncompleteLimitShards, updated.IncompleteReason)
	require.Equal(t, int64(requestTraceExportMaxShards), updated.RowsExported, "the row that would need a 1001st shard is not written")

	// 清单与全部分片都还在：交付的是这 1000 片，缺的是放不下的那部分。
	file, result, err := svc.OpenDownload(context.Background(), actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	manifestContent, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	var manifest RequestTraceExportManifest
	require.NoError(t, json.Unmarshal(manifestContent, &manifest))
	require.False(t, manifest.Complete, "a shard-cap stop must never be presented as a full export")
	require.Equal(t, RequestTraceExportIncompleteLimitShards, manifest.Reason)
	require.Equal(t, int64(requestTraceExportMaxShards), result.RowsExported)
	require.Len(t, manifest.Shards, requestTraceExportMaxShards)
	var manifestRows int64
	for index, shard := range manifest.Shards {
		require.Equal(t, exportShardBase(task.ID, index+1), shard.Name, "shards stay in the ascending order they were written")
		require.NotZero(t, shard.Bytes, "a written shard must report its real size")
		manifestRows += shard.Rows
	}
	require.Equal(t, int64(requestTraceExportMaxShards), manifestRows)

	// 最后一片照常可下载；第 1001 片既没有文件也不可请求。
	last, _, _, err := svc.OpenShardDownload(context.Background(), actor, task.ID, requestTraceExportMaxShards)
	require.NoError(t, err)
	t.Cleanup(func() { _ = last.Close() })
	_, _, _, err = svc.OpenShardDownload(context.Background(), actor, task.ID, requestTraceExportMaxShards+1)
	require.ErrorIs(t, err, ErrRequestTraceInvalidRecord)
	require.NoFileExists(t, filepath.Join(dir, exportShardBase(task.ID, requestTraceExportMaxShards+1)))
}

// 同一个界限也必须能在**管理员配置**的预算下触达：那条路径先经
// NormalizeRequestTraceExportLimits，单分片行数被抬到允许的最小值 10（构造参数
// 里那种"每片 1 行"的测试预算并不存在于管理端）。每片 10 行时，写满 1000 片
// 需要 10001 条记录，第 10001 条仍然是不完整的真实原因，而不是任务失败。
func TestRequestTraceExportShardCapReachableAtConfiguredMinimumShardRows(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{}
	source := traceExportManyIDSource(10_001)
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{
		Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one",
	})
	svc.SetAcknowledgementSatisfiedForTest(true)
	svc.SetLimitsProvider(func() RequestTraceExportLimits {
		return NormalizeRequestTraceExportLimits(RequestTraceExportLimits{
			MaxRows: 10_100, MaxBytes: 64 << 20, MaxRuntimeSec: 120,
			MaxShardRows: RequestTraceExportMinShardRow, MaxShardBytes: 8 << 20, Configured: true,
		})
	})
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)

	updated, err := svc.RunOnce(context.Background())
	require.NoError(t, err, "the shard budget is a delivered partial result under the admin's own limits too")
	require.Equal(t, RequestTraceExportCompleted, updated.Status)
	require.True(t, updated.Truncated)
	require.Equal(t, RequestTraceExportIncompleteLimitShards, updated.IncompleteReason)
	require.Equal(t, int64(requestTraceExportMaxShards*RequestTraceExportMinShardRow), updated.RowsExported)

	manifestFile, result, err := svc.OpenDownload(context.Background(), actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = manifestFile.Close() })
	require.Len(t, result.Shards, requestTraceExportMaxShards)
	require.Equal(t, int64(requestTraceExportMaxShards*RequestTraceExportMinShardRow), result.RowsExported)
	require.NoFileExists(t, filepath.Join(dir, exportShardBase(task.ID, requestTraceExportMaxShards+1)))
}

// 跳过不是一个总数：一条被枚举后**读取失败**的记录与一条**已经消失**的记录都
// 计入 RowsSkipped，但它们是两类不同的事实。清单必须按封闭原因码分别给出计数，
// 而不是只交付"跳过 5 条"和一个首个原因。
func TestRequestTraceExportMixedSkipReasonsKeepTypedCounts(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{}
	ids := []string{
		"00000000000000000000000000000001",
		"00000000000000000000000000000002",
		"00000000000000000000000000000003",
		"00000000000000000000000000000004",
		"00000000000000000000000000000005",
	}
	source := &traceExportSourceStub{
		ids:     ids,
		details: map[string]RequestTraceExportApprovedDetail{},
		// 前两条读取失败，后三条在枚举之后已经消失：两类事实各自计数。
		fails:   map[string]bool{ids[0]: true, ids[1]: true},
		deleted: map[string]bool{ids[2]: true, ids[3]: true, ids[4]: true},
	}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)

	updated, err := svc.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportCompleted, updated.Status)
	require.Zero(t, updated.RowsExported)
	require.Equal(t, int64(5), updated.RowsSkipped, "both skip classes still add up to the aggregate")
	require.Equal(t, RequestTraceExportSkipCounts{"read_failed": 2, "source_gone": 3}, updated.SkippedByReason,
		"the task result must keep the two facts apart, not collapse them into one aggregate")
	// 首个原因仍是第一条被观察到的事实，不因为多了分类型计数就被改写。
	require.Equal(t, RequestTraceExportIncompleteReadFailed, updated.IncompleteReason)

	manifestFile, manifestTask, err := svc.OpenDownload(context.Background(), actor, task.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = manifestFile.Close() })
	manifestContent, err := os.ReadFile(manifestFile.Name())
	require.NoError(t, err)
	var manifest RequestTraceExportManifest
	require.NoError(t, json.Unmarshal(manifestContent, &manifest))
	require.Equal(t, int64(5), manifest.Skipped)
	require.Equal(t, RequestTraceExportSkipCounts{"read_failed": 2, "source_gone": 3}, manifest.SkippedByReason)
	require.False(t, manifest.Complete)
	require.Equal(t, RequestTraceExportIncompleteReadFailed, manifest.Reason)
	require.Equal(t, RequestTraceExportSkipCounts{"read_failed": 2, "source_gone": 3}, manifestTask.SkippedByReason)
	// 清单里的计数是逐字交付的封闭原因码，不是自由文本。
	require.Contains(t, string(manifestContent), `"skipped_by_reason":{"read_failed":2,"source_gone":3}`)

	// 任务行没有这一列：进程重启后必须从清单恢复分类型计数，而不是只留下聚合数。
	reloaded, err := svc.GetTask(context.Background(), actor, task.ID)
	require.NoError(t, err)
	require.Equal(t, RequestTraceExportSkipCounts{"read_failed": 2, "source_gone": 3}, reloaded.SkippedByReason)
	require.Equal(t, int64(5), reloaded.RowsSkipped)
	require.Equal(t, RequestTraceExportIncompleteReadFailed, reloaded.IncompleteReason)
}

// 计数只接受封闭原因码：本版本不认识的原因码（未来版本写出的清单）不会被冒充成
// 一个可展示的类别，非正计数也不入库；聚合数与首个原因不受影响。
func TestRequestTraceExportSkipCountsOnlyAcceptClosedPositiveReasons(t *testing.T) {
	counts := newRequestTraceExportSkipCounts()
	counts.record(RequestTraceExportIncompleteReadFailed)
	counts.record(RequestTraceExportIncompleteSourceGone)
	counts.record(RequestTraceExportIncompleteLimitRows)
	counts.record("some_future_reason")
	require.Equal(t, RequestTraceExportSkipCounts{"read_failed": 1, "source_gone": 1}, counts,
		"only the closed per-row skip reasons may be counted; limit codes are why the task stopped")

	require.Equal(t, RequestTraceExportSkipCounts{"read_failed": 2},
		normalizeRequestTraceExportSkipCounts(RequestTraceExportSkipCounts{
			"read_failed": 2, "some_future_reason": 9, "source_gone": -1, "limit_rows": 0,
		}))
	require.Empty(t, normalizeRequestTraceExportSkipCounts(nil))

	// 零值（nil map）不能被 record 写成 panic，也不能凭空产生计数。
	var empty RequestTraceExportSkipCounts
	empty.record(RequestTraceExportIncompleteReadFailed)
	require.Empty(t, empty)
}

// 旧版本写出的清单没有 skipped_by_reason 键：读回时按"没有已知的跳过类型"处理，
// 不能因此报错、丢分片或把任务说成完整。聚合数与首个原因仍是权威事实。
func TestRequestTraceExportLegacyManifestWithoutTypedCountsStaysReadable(t *testing.T) {
	dir := t.TempDir()
	store := &traceExportStoreStub{}
	source := &traceExportSourceStub{}
	svc := NewRequestTraceExportService(store, source, RequestTraceExportOptions{Enabled: true, SingleInstanceDeclared: true, TempDir: dir, InstanceID: "instance-one"})
	svc.SetAcknowledgementSatisfiedForTest(true)
	actor := RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}
	task, err := svc.CreateTask(context.Background(), actor, RequestTraceExportFilter{})
	require.NoError(t, err)

	completed := svc.now().UTC()
	until := completed.Add(RequestTraceExportDownloadWindow)
	legacy := task
	legacy.Status = RequestTraceExportCompleted
	legacy.Filename = exportManifestBase(task.ID)
	legacy.CompletedAt = &completed
	legacy.DownloadUntil = &until
	legacy.RowsSkipped = 4
	require.NoError(t, store.Finish(context.Background(), legacy))

	// 旧清单：没有 skipped_by_reason 键，只有聚合数与首个原因。
	raw := fmt.Sprintf(`{"task_id":%q,"filter":{},"shards":[{"name":%q,"rows":1,"bytes":2}],`+
		`"rows":1,"skipped":4,"bytes":2,"complete":false,"incomplete_reason":"source_gone",`+
		`"created_at":%q,"download_until":%q}`,
		task.ID, exportShardBase(task.ID, 1), completed.Format(time.RFC3339Nano), until.Format(time.RFC3339Nano))
	require.NoError(t, os.WriteFile(filepath.Join(dir, exportManifestBase(task.ID)), []byte(raw), 0o600))

	reloaded, err := svc.GetTask(context.Background(), actor, task.ID)
	require.NoError(t, err)
	require.Empty(t, reloaded.SkippedByReason, "an older manifest reports no known skip types rather than failing or inventing one")
	require.Equal(t, int64(4), reloaded.RowsSkipped, "the aggregate is still the authoritative count")
	require.True(t, reloaded.Truncated)
	require.Equal(t, RequestTraceExportIncompleteSourceGone, reloaded.IncompleteReason)
	require.Equal(t, []string{exportShardBase(task.ID, 1)}, reloaded.Shards)
}
