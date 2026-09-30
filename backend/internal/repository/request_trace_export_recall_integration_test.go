//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// ticket09：离开/刷新页面后，同一管理员登录会话必须能找回自己的导出任务。
// 找回的边界是**行级**的：只按创建管理员 + 会话摘要 + 本实例过滤，其它会话、
// 其它管理员、其它实例的行一律不出现，且页大小有界、绝不静默截断。

const (
	recallDigestOwner  = "1111111111111111111111111111111111111111111111111111111111111111"
	recallDigestOther  = "2222222222222222222222222222222222222222222222222222222222222222"
	recallDigestThird  = "3333333333333333333333333333333333333333333333333333333333333333"
	recallInstanceMain = "recall-node-one"
	recallInstanceElse = "recall-node-two"
)

// recallTask 造出一行**已结束**的任务。Create 每个实例只接纳一个活动任务，
// 所以先建后立即收尾（failed 不需要文件名），才能在同一实例上排出多行历史。
func recallTask(t *testing.T, store *requestTraceExportRepository, id string, adminUserID int64, digest, instance string, created time.Time) {
	t.Helper()
	task := service.RequestTraceExportTask{
		ID: id, Status: service.RequestTraceExportPending,
		AdminUserID: adminUserID, SessionDigest: digest, InstanceID: instance,
		CreatedAt: created.UTC(),
	}
	require.NoError(t, store.Create(context.Background(), task, 1))
	task.Status = service.RequestTraceExportFailed
	require.NoError(t, store.Finish(context.Background(), task))
}

func recallIDs(tasks []service.RequestTraceExportTask) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func TestRequestTraceExportRepositoryRecallIsScopedToActorSessionAndInstance(t *testing.T) {
	ctx := context.Background()
	store := &requestTraceExportRepository{q: testTx(t)}
	now := time.Now().UTC()

	mine := []string{
		"11111111111111111111111111111111",
		"22222222222222222222222222222222",
		"33333333333333333333333333333333",
	}
	recallTask(t, store, mine[2], 12, recallDigestOwner, recallInstanceMain, now.Add(-3*time.Hour))
	recallTask(t, store, mine[1], 12, recallDigestOwner, recallInstanceMain, now.Add(-2*time.Hour))
	recallTask(t, store, mine[0], 12, recallDigestOwner, recallInstanceMain, now.Add(-time.Hour))
	// 同一管理员的**另一个会话**：同一登录人，但不是找回这次动作的会话。
	recallTask(t, store, "44444444444444444444444444444444", 12, recallDigestOther, recallInstanceMain, now.Add(-30*time.Minute))
	// 另一个管理员，会话摘要不同。
	recallTask(t, store, "55555555555555555555555555555555", 13, recallDigestThird, recallInstanceMain, now.Add(-30*time.Minute))
	// 同一管理员、同一会话，但属于另一个实例：本实例不得列出别人的任务。
	recallTask(t, store, "66666666666666666666666666666666", 12, recallDigestOwner, recallInstanceElse, now.Add(-30*time.Minute))

	tasks, more, err := store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceMain, nil, 10)
	require.NoError(t, err)
	// 最近创建的在前；其它会话、其它管理员、其它实例的行都不在其中。
	require.Equal(t, mine, recallIDs(tasks))
	require.False(t, more, "three rows read with a page of ten is the whole set")

	// 另一个实例上的同名会话看不到本实例的任务。
	other, _, err := store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceElse, nil, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"66666666666666666666666666666666"}, recallIDs(other))

	// 没有匹配行是空结果，不是错误。
	empty, more, err := store.ListForSession(ctx, 99, recallDigestOwner, recallInstanceMain, nil, 10)
	require.NoError(t, err)
	require.Empty(t, empty)
	require.False(t, more)
}

// recallWalk 模拟界面上的"加载更多"：从第一页开始按游标逐页读完，拼成一条有序列表。
// 它同时证明续页既不重复也不漏行——这正是旧实现（只有一页）拿不回第 21 行的原因。
func recallWalk(t *testing.T, store *requestTraceExportRepository, limit int) []string {
	t.Helper()
	walked := make([]string, 0, 32)
	var after *service.RequestTraceExportRecallCursor
	for page := 0; page < 50; page++ {
		tasks, more, err := store.ListForSession(context.Background(), 12, recallDigestOwner, recallInstanceMain, after, limit)
		require.NoError(t, err)
		walked = append(walked, recallIDs(tasks)...)
		if !more {
			return walked
		}
		require.NotEmpty(t, tasks, "a page claiming a next one must return the rows it scanned")
		last := tasks[len(tasks)-1]
		after = &service.RequestTraceExportRecallCursor{CreatedAt: last.CreatedAt, ExportID: last.ID}
	}
	t.Fatal("recall walk did not terminate")
	return nil
}

// 行数超过一页时，最早的任务必须仍然可达：旧实现按一次 20 行读回，第 21 行永远不出现。
func TestRequestTraceExportRepositoryRecallPagesPastTheSinglePageCeiling(t *testing.T) {
	ctx := context.Background()
	store := &requestTraceExportRepository{q: testTx(t)}
	now := time.Now().UTC()
	want := make([]string, 0, 21)
	for i := 0; i < 21; i++ {
		id := fmt.Sprintf("%032x", i)
		want = append(want, id)
		recallTask(t, store, id, 12, recallDigestOwner, recallInstanceMain, now.Add(-time.Duration(i)*time.Minute))
	}

	first, more, err := store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceMain, nil, 20)
	require.NoError(t, err)
	require.Len(t, first, 20)
	require.True(t, more, "twenty rows out of twenty-one is not the whole set")
	require.Equal(t, want[:20], recallIDs(first))

	last := first[len(first)-1]
	second, more, err := store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceMain,
		&service.RequestTraceExportRecallCursor{CreatedAt: last.CreatedAt, ExportID: last.ID}, 20)
	require.NoError(t, err)
	require.Equal(t, want[20:], recallIDs(second))
	require.False(t, more, "the last page must not claim a next one")

	// 逐页走完与一次读回得到同一条列表：没有重复，也没有被跳过的行。
	require.Equal(t, want, recallWalk(t, store, 20))
}

// 同一创建时间的多行靠 export_id 降序定序，跨页时次序不变；作为边界的那一行即使
// 在这两页之间被清理，续页也不会重头开始——边界是值，不是行引用。
func TestRequestTraceExportRepositoryRecallKeepsOrderAcrossTiesAndADeletedBoundaryRow(t *testing.T) {
	ctx := context.Background()
	store := &requestTraceExportRepository{q: testTx(t)}
	created := time.Now().UTC()
	ids := []string{
		"0000000000000000000000000000000a",
		"0000000000000000000000000000000b",
		"0000000000000000000000000000000c",
	}
	for _, id := range ids {
		recallTask(t, store, id, 12, recallDigestOwner, recallInstanceMain, created)
	}

	first, more, err := store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceMain, nil, 2)
	require.NoError(t, err)
	require.Equal(t, []string{ids[2], ids[1]}, recallIDs(first))
	require.True(t, more)

	require.NoError(t, store.Delete(ctx, ids[1]))
	second, more, err := store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceMain,
		&service.RequestTraceExportRecallCursor{CreatedAt: first[1].CreatedAt, ExportID: first[1].ID}, 2)
	require.NoError(t, err)
	require.Equal(t, []string{ids[0]}, recallIDs(second))
	require.False(t, more)
}

func TestRequestTraceExportRepositoryRecallTieBreaksByIDAndStaysBounded(t *testing.T) {
	ctx := context.Background()
	store := &requestTraceExportRepository{q: testTx(t)}
	created := time.Now().UTC()
	// 同一创建时间的三行：次序必须稳定（export_id 降序），不能被插入顺序左右。
	ids := []string{
		"0000000000000000000000000000000a",
		"0000000000000000000000000000000b",
		"0000000000000000000000000000000c",
	}
	for _, id := range ids {
		recallTask(t, store, id, 12, recallDigestOwner, recallInstanceMain, created)
	}

	tasks, _, err := store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceMain, nil, 10)
	require.NoError(t, err)
	require.Equal(t, []string{ids[2], ids[1], ids[0]}, recallIDs(tasks))

	// 页大小有界：上限之内的请求被接受，超过上限与非法值一律拒绝而不是截断。
	page, _, err := store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceMain, nil, 2)
	require.NoError(t, err)
	require.Equal(t, []string{ids[2], ids[1]}, recallIDs(page))
	_, _, err = store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceMain, nil, requestTraceExportMaxRecallTasks+1)
	require.ErrorIs(t, err, service.ErrRequestTraceExportLimit)
	_, _, err = store.ListForSession(ctx, 12, recallDigestOwner, recallInstanceMain, nil, 0)
	require.ErrorIs(t, err, service.ErrRequestTraceExportLimit)
}
