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
}

func (s *traceExportSourceStub) NextTraceIDs(ctx context.Context, filter RequestTraceExportFilter, after string, limit int) ([]string, error) {
	var result []string
	for _, id := range s.ids {
		if id > after {
			result = append(result, id)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}
func (s *traceExportSourceStub) ReadApprovedDetail(ctx context.Context, id string) (RequestTraceExportApprovedDetail, bool, error) {
	if s.deleted[id] {
		return RequestTraceExportApprovedDetail{}, false, nil
	}
	return s.details[id], true, nil
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
