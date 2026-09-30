//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ticket09：刷新或离开页面后，同一个管理员登录会话必须能找回自己的导出任务，
// 而且**找得回全部**——任务多到超过一页时，最早的那个不能因为"一次只读一页"
// 就消失。这些用例盯住四件事：找回的范围只由已验签的主体决定（别人的任务不出现），
// 续页只认服务自己给出的不透明游标（畸形令牌明确拒绝，绝不当成"读完了"），
// 句柄里不含文件名/路径/会话摘要，页大小有界且不静默截断。

// recallPagedStore 是找回测试的存储替身：它复用基础替身的其余语义，只把"一页"
// 这件事换成本用例关心的行为——返回哪一页、边界之后还有没有行，以及是否失败。
// 它同时记录服务层交下来的三个过滤条件与页大小，用来证明这些值来自已验签的主体。
type recallPagedStore struct {
	*traceExportStoreStub
	page func(call int, after *RequestTraceExportRecallCursor, limit int) ([]RequestTraceExportTask, bool, error)

	adminUserIDs []int64
	digests      []string
	instances    []string
	boundaries   []*RequestTraceExportRecallCursor
	limits       []int
}

func (s *recallPagedStore) ListForSession(_ context.Context, adminUserID int64, sessionDigest, instanceID string, after *RequestTraceExportRecallCursor, limit int) ([]RequestTraceExportTask, bool, error) {
	call := len(s.limits)
	s.adminUserIDs = append(s.adminUserIDs, adminUserID)
	s.digests = append(s.digests, sessionDigest)
	s.instances = append(s.instances, instanceID)
	s.boundaries = append(s.boundaries, after)
	s.limits = append(s.limits, limit)
	if s.page == nil {
		return nil, false, nil
	}
	return s.page(call, after, limit)
}

func recallService(t *testing.T, store RequestTraceExportStore, mutate func(*RequestTraceExportOptions)) *RequestTraceExportService {
	t.Helper()
	svc, _ := recallServiceWithDir(t, store, mutate)
	return svc
}

// recallServiceWithDir 额外交回服务的临时目录，用来放**真实格式**的清单文件。
func recallServiceWithDir(t *testing.T, store RequestTraceExportStore, mutate func(*RequestTraceExportOptions)) (*RequestTraceExportService, string) {
	t.Helper()
	dir := t.TempDir()
	options := RequestTraceExportOptions{
		Enabled: true, SingleInstanceDeclared: true, InstanceID: "instance-one", TempDir: dir,
	}
	if mutate != nil {
		mutate(&options)
	}
	svc := NewRequestTraceExportService(store, traceExportManyIDSource(1), options)
	svc.SetAcknowledgementSatisfiedForTest(true)
	return svc, dir
}

// recallWriteManifest 把一份清单按生产格式写到服务自己的临时目录里，路径与文件名
// 都由服务的导出命名规则决定——测试读回的是真实读路径，不是替身。
func recallWriteManifest(t *testing.T, dir string, manifest RequestTraceExportManifest) {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, exportManifestBase(manifest.TaskID)), encoded, 0600))
}

// recallTaskRow 造出一行已结束的找回结果（状态与本层无关，这里只需要它的排序键）。
func recallTaskRow(id string, created time.Time) RequestTraceExportTask {
	return RequestTraceExportTask{ID: id, Status: RequestTraceExportCompleted, CreatedAt: created.UTC()}
}

func recallIDs(tasks []RequestTraceExportTask) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

var recallActor = RequestTraceExportActor{AdminUserID: 12, SessionID: "session"}

// 找回只认"本会话 + 本实例"：三个过滤条件都由已验签的主体推出，调用方给不出别的
// 取值；文件名（以及分片/清单名）在返回前被剥掉——列表给出的是句柄，不是路径。
func TestRequestTraceExportListTasksScopesToActorSessionAndInstance(t *testing.T) {
	now := time.Now().UTC()
	store := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	store.page = func(int, *RequestTraceExportRecallCursor, int) ([]RequestTraceExportTask, bool, error) {
		return []RequestTraceExportTask{{
			ID: "11111111111111111111111111111111", Status: RequestTraceExportCompleted,
			Filename: "sub2api-request-trace-export-11111111111111111111111111111111.jsonl",
			Shards:   []string{"shard-0001.jsonl"}, Manifest: "manifest.json",
			CreatedAt: now.Add(-time.Hour),
		}}, false, nil
	}

	tasks, next, err := recallService(t, store, nil).ListTasks(context.Background(), recallActor, "", 0)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Empty(t, tasks[0].Filename, "a recalled task handle must not carry a file path")
	require.Empty(t, tasks[0].Shards)
	require.Empty(t, tasks[0].Manifest)
	require.Empty(t, next, "no further rows means no cursor at all")
	// 三个过滤条件来自已验签的主体与服务自己的实例标识，与请求无关。
	require.Equal(t, []int64{12}, store.adminUserIDs)
	require.Equal(t, []string{sessionDigest("session")}, store.digests)
	require.Equal(t, []string{"instance-one"}, store.instances)
	// 未指定页大小时用默认值，而不是把"没给"当成无界。
	require.Equal(t, []int{RequestTraceExportDefaultListedTasks}, store.limits)
	// 第一页没有边界。
	require.Equal(t, []*RequestTraceExportRecallCursor{nil}, store.boundaries)
}

// 页大小有界：上限之内原样透传，超过上限即拒绝（不是截断），非法页大小同样拒绝。
func TestRequestTraceExportListTasksBoundsThePageWithoutTruncating(t *testing.T) {
	store := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	svc := recallService(t, store, nil)

	_, _, err := svc.ListTasks(context.Background(), recallActor, "", RequestTraceExportMaxListedTasks)
	require.NoError(t, err)
	require.Equal(t, []int{RequestTraceExportMaxListedTasks}, store.limits)

	_, _, err = svc.ListTasks(context.Background(), recallActor, "", RequestTraceExportMaxListedTasks+1)
	require.ErrorIs(t, err, ErrRequestTraceExportLimit)

	_, _, err = svc.ListTasks(context.Background(), recallActor, "", -1)
	require.NoError(t, err, "a negative limit means 'use the default', not 'refuse'")
	require.Equal(t, RequestTraceExportDefaultListedTasks, store.limits[len(store.limits)-1])
}

// 超过一页时，最早的任务必须仍然可达：第一页给出不透明游标，把它交回来就拿到下一页，
// 而且游标里携带的正是这一页最后一行的排序键——它是**值**，不是行引用。
func TestRequestTraceExportListTasksPagesUntilEveryTaskIsReachable(t *testing.T) {
	now := time.Now().UTC()
	// 21 行：旧实现一页最多 20 行，第 21 行永远看不到。
	rows := make([]RequestTraceExportTask, 0, 21)
	for i := 0; i < 21; i++ {
		rows = append(rows, recallTaskRow(fmtRecallID(i), now.Add(-time.Duration(i)*time.Minute)))
	}
	store := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	store.page = func(call int, after *RequestTraceExportRecallCursor, limit int) ([]RequestTraceExportTask, bool, error) {
		switch call {
		case 0:
			require.Nil(t, after, "the first page carries no boundary")
			return rows[:20], true, nil
		case 1:
			// 第二页收到的边界必须等于第一页最后一行的排序键。
			require.NotNil(t, after)
			require.Equal(t, rows[19].CreatedAt, after.CreatedAt)
			require.Equal(t, rows[19].ID, after.ExportID)
			return rows[20:], false, nil
		default:
			t.Fatalf("unexpected extra page request %d", call)
			return nil, false, nil
		}
	}
	svc := recallService(t, store, nil)

	first, next, err := svc.ListTasks(context.Background(), recallActor, "", 20)
	require.NoError(t, err)
	require.Equal(t, recallIDs(rows[:20]), recallIDs(first))
	require.NotEmpty(t, next, "twenty rows out of twenty-one must offer a next page")

	// 游标是不透明的，但它只承载这一页最后一行的排序键：没有文件名、路径或会话摘要。
	decoded, err := decodeRequestTraceExportRecallCursor(next)
	require.NoError(t, err)
	require.Equal(t, rows[19].CreatedAt.UTC(), decoded.CreatedAt)
	require.Equal(t, rows[19].ID, decoded.ExportID)

	second, last, err := svc.ListTasks(context.Background(), recallActor, next, 20)
	require.NoError(t, err)
	require.Equal(t, recallIDs(rows[20:]), recallIDs(second))
	require.Empty(t, last, "the last page must not claim a next one")
	require.Equal(t, []int{20, 20}, store.limits)
}

// fmtRecallID 造一个符合 export_id 形状（32 位小写十六进制）的稳定 ID。
func fmtRecallID(i int) string {
	return fmt.Sprintf("%032x", i)
}

// 刚好装满一页不等于"还有更多"：存储说没有下一行时，服务端不给游标，客户端据此停止，
// 而不是去请求一个空页再自己判断。
func TestRequestTraceExportListTasksOmitsTheCursorWhenThePageIsTheLastOne(t *testing.T) {
	store := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	store.page = func(int, *RequestTraceExportRecallCursor, int) ([]RequestTraceExportTask, bool, error) {
		rows := make([]RequestTraceExportTask, 0, 20)
		for i := 0; i < 20; i++ {
			rows = append(rows, recallTaskRow(fmtRecallID(i), time.Now().UTC()))
		}
		return rows, false, nil
	}

	tasks, next, err := recallService(t, store, nil).ListTasks(context.Background(), recallActor, "", 20)
	require.NoError(t, err)
	require.Len(t, tasks, 20)
	require.Empty(t, next, "a full page with nothing behind it is not 'more'")
}

// 畸形游标是调用方的问题，不是"读完了"：它们一律被拒，且绝不触达存储——把无法解释的
// 令牌当成"没有下一页"会让仍然存在的任务看起来不存在。
func TestRequestTraceExportListTasksRefusesUnreadableCursors(t *testing.T) {
	validShape := fmtRecallID(3)
	encoded := func(payload string) string {
		return requestTraceExportRecallCursorVersion + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	}
	cases := map[string]string{
		"no version prefix":       "not-a-token",
		"unknown version":         "v2." + base64.RawURLEncoding.EncodeToString([]byte("2026-09-30T00:00:00Z|"+validShape)),
		"not base64":              requestTraceExportRecallCursorVersion + ".!!!not-base64!!!",
		"missing separator":       encoded("2026-09-30T00:00:00Z"),
		"export id of wrong size": encoded("2026-09-30T00:00:00Z|" + strings.Repeat("a", 31)),
		"export id not hex":       encoded("2026-09-30T00:00:00Z|" + strings.Repeat("z", 32)),
		"timestamp not a time":    encoded("yesterday|" + validShape),
		"over the length bound":   requestTraceExportRecallCursorVersion + "." + strings.Repeat("a", RequestTraceExportRecallCursorMaxLength),
		"trailing separator":      encoded("2026-09-30T00:00:00Z|" + validShape + "|extra"),
	}

	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			store := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
			store.page = func(int, *RequestTraceExportRecallCursor, int) ([]RequestTraceExportTask, bool, error) {
				t.Fatal("an unreadable cursor must never reach the store")
				return nil, false, nil
			}
			_, next, err := recallService(t, store, nil).ListTasks(context.Background(), recallActor, token, 20)
			require.ErrorIs(t, err, ErrRequestTraceExportInvalidCursor)
			require.Empty(t, next)
			require.Empty(t, store.limits, "a refused cursor must never be turned into a read")
		})
	}
}

// 找回必须能翻页：只有一页能力的存储会让更早的任务永远不可达，因此服务层在没有
// 翻页能力的存储上明确失败，而不是退回"读回一页且不给下一页"。
func TestRequestTraceExportListTasksFailsClearlyWithoutAPageableStore(t *testing.T) {
	// 基础替身只有生命周期语义，没有翻页能力（它的 ListForSession 是另一套签名）。
	svc := recallService(t, &traceExportStoreStub{}, nil)

	_, _, err := svc.ListTasks(context.Background(), recallActor, "", 20)
	require.ErrorIs(t, err, ErrRequestTraceExportUnavailable)

	_, _, err = svc.ListTasks(context.Background(), recallActor, encodeRequestTraceExportRecallCursor(RequestTraceExportRecallCursor{
		CreatedAt: time.Now().UTC(), ExportID: fmtRecallID(1),
	}), 20)
	require.ErrorIs(t, err, ErrRequestTraceExportUnavailable)
}

// 找回的每一行都必须带着它**真实**的完整性结论：分片清单与完整性只存在于清单文件里
// （任务行没有这些列），所以列表要按行读回清单，而不是把零值当成"完整"。
// 一条被上限截断的完成导出在列表里必须显示为不完整。
func TestRequestTraceExportListTasksReadsCompletenessFromEachManifest(t *testing.T) {
	now := time.Now().UTC()
	truncatedID := fmtRecallID(1)
	completeID := fmtRecallID(2)
	failedID := fmtRecallID(3)

	store := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	store.page = func(int, *RequestTraceExportRecallCursor, int) ([]RequestTraceExportTask, bool, error) {
		return []RequestTraceExportTask{
			recallTaskRow(truncatedID, now),
			recallTaskRow(completeID, now.Add(-time.Minute)),
			recallTaskRow(failedID, now.Add(-2*time.Minute)),
		}, false, nil
	}
	svc, dir := recallServiceWithDir(t, store, nil)
	recallWriteManifest(t, dir, RequestTraceExportManifest{
		TaskID: truncatedID, Complete: false, Reason: "limit_rows",
		SkippedByReason: RequestTraceExportSkipCounts{RequestTraceExportIncompleteSourceGone: 2},
	})
	recallWriteManifest(t, dir, RequestTraceExportManifest{TaskID: completeID, Complete: true})
	// 一行 failed 任务旁边留着一份清单：状态不是 completed 就不能被清单改写成别的结论。
	recallWriteManifest(t, dir, RequestTraceExportManifest{TaskID: failedID, Complete: false, Reason: "read_failed"})

	tasks, _, err := svc.ListTasks(context.Background(), recallActor, "", 20)
	require.NoError(t, err)
	require.Len(t, tasks, 3)

	require.True(t, tasks[0].Truncated, "a completed export cut off by a task limit is not a success")
	require.Equal(t, "limit_rows", tasks[0].IncompleteReason)
	require.Equal(t, int64(2), tasks[0].SkippedByReason[RequestTraceExportIncompleteSourceGone])

	require.False(t, tasks[1].Truncated)
	require.Empty(t, tasks[1].IncompleteReason)
	require.True(t, tasks[2].Truncated, "the fixture's failed row keeps its own verdict")
	require.Equal(t, "read_failed", tasks[2].IncompleteReason)

	// 清单是**读**的，不是外泄的：列表里没有文件名、分片名或清单名。
	for _, task := range tasks {
		require.Empty(t, task.Filename)
		require.Empty(t, task.Shards)
		require.Empty(t, task.Manifest)
	}
}

// 清单读不到（被清理、损坏、或根本不在本机）时，列表绝不能把它报成"完成"：
// 没有清单就既不知道交付了几个分片，也不知道是否被截断。它必须是一个明确的不完整/
// 未知结论，且原因码是封闭的。
func TestRequestTraceExportListTasksNeverReportsACompletedTaskWhoseManifestIsLost(t *testing.T) {
	now := time.Now().UTC()
	lostID := fmtRecallID(4)
	corruptID := fmtRecallID(5)

	store := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	store.page = func(int, *RequestTraceExportRecallCursor, int) ([]RequestTraceExportTask, bool, error) {
		return []RequestTraceExportTask{
			recallTaskRow(lostID, now),
			recallTaskRow(corruptID, now.Add(-time.Minute)),
		}, false, nil
	}
	svc, dir := recallServiceWithDir(t, store, nil)
	// corruptID 的清单存在但不是清单：损坏与缺失一样，都是"无法声称完整"。
	require.NoError(t, os.WriteFile(filepath.Join(dir, exportManifestBase(corruptID)), []byte("{not json"), 0600))

	tasks, _, err := svc.ListTasks(context.Background(), recallActor, "", 20)
	require.NoError(t, err)
	require.Len(t, tasks, 2)

	for _, task := range tasks {
		require.True(t, task.Truncated, "a completed row with no readable manifest must never read as complete")
		require.Equal(t, RequestTraceExportIncompleteManifestLost, task.IncompleteReason)
		require.Empty(t, task.Shards)
		require.Empty(t, task.Manifest)
	}
}

// 读回单个任务（GetTask）必须与找回给出同一个结论：同一条完成导出在列表里说
// "不完整"，在详情里就不能说"完成"。清单读不到的完成导出在两条路径上都是
// "无从判断"，而且没有可交付的分片可列。
func TestRequestTraceExportGetTaskReportsTheSameCompletenessVerdict(t *testing.T) {
	now := time.Now().UTC()
	completeID := fmtRecallID(11)
	truncatedID := fmtRecallID(12)
	lostID := fmtRecallID(13)
	nearID := fmtRecallID(14)
	corruptID := fmtRecallID(15)

	store := &traceExportStoreStub{tasks: make(map[string]RequestTraceExportTask)}
	for _, id := range []string{completeID, truncatedID, lostID, nearID, corruptID} {
		store.tasks[id] = RequestTraceExportTask{
			ID: id, Status: RequestTraceExportCompleted, AdminUserID: 12,
			SessionDigest: sessionDigest("session"), InstanceID: "instance-one", CreatedAt: now,
		}
	}
	svc, dir := recallServiceWithDir(t, store, nil)
	recallWriteManifest(t, dir, RequestTraceExportManifest{
		TaskID: completeID, Complete: true,
		Shards: []RequestTraceExportShard{{Name: "shard-0001.jsonl", Rows: 2, Bytes: 64}},
	})
	recallWriteManifest(t, dir, RequestTraceExportManifest{
		TaskID: truncatedID, Complete: false, Reason: RequestTraceExportIncompleteLimitShards,
		Shards: []RequestTraceExportShard{{Name: "shard-0001.jsonl", Rows: 2, Bytes: 64}},
	})
	// 名字像清单、内容不是：损坏与缺失一样，都是"无从判断"。
	require.NoError(t, os.WriteFile(filepath.Join(dir, exportManifestBase(corruptID)), []byte("{not json"), 0600))

	cases := []struct {
		name          string
		id            string
		truncated     bool
		reason        string
		wantShardName string
	}{
		{name: "complete", id: completeID, truncated: false, wantShardName: "shard-0001.jsonl"},
		{name: "truncated", id: truncatedID, truncated: true, reason: RequestTraceExportIncompleteLimitShards, wantShardName: "shard-0001.jsonl"},
		{name: "manifest missing", id: lostID, truncated: true, reason: RequestTraceExportIncompleteManifestLost},
		{name: "manifest corrupt", id: corruptID, truncated: true, reason: RequestTraceExportIncompleteManifestLost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task, err := svc.GetTask(context.Background(), recallActor, tc.id)
			require.NoError(t, err)
			require.Equal(t, tc.truncated, task.Truncated, "the detail must not call an unverifiable delivery complete")
			require.Equal(t, tc.reason, task.IncompleteReason)
			if tc.wantShardName == "" {
				require.Empty(t, task.Shards, "no readable manifest means no shard list, not an empty one")
				require.Empty(t, task.Manifest)
				return
			}
			require.Equal(t, []string{tc.wantShardName}, task.Shards)
			require.Equal(t, exportManifestBase(tc.id), task.Manifest)
		})
	}
}

// 没有管理员会话就没有找回：缺主体或缺会话都被拒，且不是"空列表"。
func TestRequestTraceExportListTasksRequiresAdminLoginSession(t *testing.T) {
	store := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	svc := recallService(t, store, nil)

	_, _, err := svc.ListTasks(context.Background(), RequestTraceExportActor{SessionID: "session"}, "", 0)
	require.ErrorIs(t, err, ErrRequestTraceExportSessionRequired)
	_, _, err = svc.ListTasks(context.Background(), RequestTraceExportActor{AdminUserID: 12}, "", 0)
	require.ErrorIs(t, err, ErrRequestTraceExportSessionRequired)
	_, _, err = svc.ListTasks(context.Background(), RequestTraceExportActor{AdminUserID: 12, SessionID: "   "}, "", 0)
	require.ErrorIs(t, err, ErrRequestTraceExportSessionRequired)
	require.Empty(t, store.limits, "a rejected actor must never reach the store")
}

// 单实例声明缺失时，找回与创建/读取同样不可用：句柄属于本机文件，本实例不导出就
// 没有可找回的东西。
func TestRequestTraceExportListTasksIsDisabledWithoutSingleInstanceDeclaration(t *testing.T) {
	store := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	svc := recallService(t, store, func(o *RequestTraceExportOptions) { o.SingleInstanceDeclared = false })

	_, _, err := svc.ListTasks(context.Background(), recallActor, "", 0)
	require.ErrorIs(t, err, ErrRequestTraceExportDisabled)
	require.Empty(t, store.limits)
}

// 存储故障折叠成"暂时不可用"，不把 DB 细节带出去；容量错误则原样透传，因为它们是
// 两种不同的事实。
func TestRequestTraceExportListTasksMapsStoreFailures(t *testing.T) {
	failing := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	failing.page = func(int, *RequestTraceExportRecallCursor, int) ([]RequestTraceExportTask, bool, error) {
		return nil, false, errors.New("synthetic list failure")
	}
	_, _, err := recallService(t, failing, nil).ListTasks(context.Background(), recallActor, "", 0)
	require.ErrorIs(t, err, ErrRequestTraceExportUnavailable)

	limited := &recallPagedStore{traceExportStoreStub: &traceExportStoreStub{}}
	limited.page = func(int, *RequestTraceExportRecallCursor, int) ([]RequestTraceExportTask, bool, error) {
		return nil, false, ErrRequestTraceExportLimit
	}
	_, _, err = recallService(t, limited, nil).ListTasks(context.Background(), recallActor, "", 0)
	require.ErrorIs(t, err, ErrRequestTraceExportLimit)
}
