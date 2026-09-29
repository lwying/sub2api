//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceExportRepositoryExpiresFailedRowsAfterSevenDays(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	store := &requestTraceExportRepository{q: tx}
	now := time.Now().UTC()
	for _, fixture := range []struct { id string; created time.Time }{
		{"cccccccccccccccccccccccccccccccc", now.Add(-8 * 24 * time.Hour)},
		{"dddddddddddddddddddddddddddddddd", now.Add(-time.Hour)},
	} {
		task := service.RequestTraceExportTask{
			ID: fixture.id, Status: service.RequestTraceExportPending,
			AdminUserID: 12, SessionDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			InstanceID: fixture.id, CreatedAt: fixture.created,
		}
		require.NoError(t, store.Create(ctx, task, 1))
		task.Status = service.RequestTraceExportFailed
		require.NoError(t, store.Finish(ctx, task))
	}
	expired, err := store.Expired(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	require.Equal(t, "cccccccccccccccccccccccccccccccc", expired[0].ID)
	require.NoError(t, store.Delete(ctx, expired[0].ID))
	_, err = store.Get(ctx, expired[0].ID)
	require.ErrorIs(t, err, service.ErrRequestTraceExportNotFound)
	_, err = store.Get(ctx, "dddddddddddddddddddddddddddddddd")
	require.NoError(t, err)
}

// ExpiredAfter is the cursor form the sweep uses: rows come back in ascending
// export_id order strictly after the cursor, so a run of non-deletable rows can
// never shadow the ones behind them. Insert order must not leak into page order.
func TestRequestTraceExportRepositoryExpiredAfterPagesByExportIDCursor(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	store := &requestTraceExportRepository{q: tx}
	now := time.Now().UTC()
	until := now.Add(-time.Hour)
	ids := []string{
		"0000000000000000000000000000000c",
		"0000000000000000000000000000000a",
		"0000000000000000000000000000000e",
		"0000000000000000000000000000000b",
		"0000000000000000000000000000000d",
	}
	for _, id := range ids {
		task := service.RequestTraceExportTask{
			ID: id, Status: service.RequestTraceExportPending,
			AdminUserID: 12, SessionDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			InstanceID: id, CreatedAt: now,
		}
		require.NoError(t, store.Create(ctx, task, 1))
		task.Status = service.RequestTraceExportCompleted
		task.Filename = "sub2api-request-trace-export-" + id + ".jsonl"
		completed := now
		task.CompletedAt = &completed
		task.DownloadUntil = &until
		require.NoError(t, store.Finish(ctx, task))
	}
	// 0a < 0b < 0c < 0d < 0e by export_id regardless of insert order.
	first, err := store.ExpiredAfter(ctx, now, "", 2)
	require.NoError(t, err)
	require.Equal(t, []string{ids[1], ids[3]}, []string{first[0].ID, first[1].ID})
	second, err := store.ExpiredAfter(ctx, now, first[len(first)-1].ID, 2)
	require.NoError(t, err)
	require.Equal(t, []string{ids[0], ids[4]}, []string{second[0].ID, second[1].ID})
	third, err := store.ExpiredAfter(ctx, now, second[len(second)-1].ID, 10)
	require.NoError(t, err)
	require.Equal(t, []string{ids[2]}, []string{third[0].ID})
	// Nothing is left after the cursor, so the sweep can wrap to the head.
	tail, err := store.ExpiredAfter(ctx, now, third[len(third)-1].ID, 10)
	require.NoError(t, err)
	require.Empty(t, tail)
	// The page size is bounded: an oversized request is refused, not clamped.
	_, err = store.ExpiredAfter(ctx, now, "", 501)
	require.ErrorIs(t, err, service.ErrRequestTraceExportLimit)
}

func TestRequestTraceExportRepositoryPersistsTaskAndClaimsOnlyOwnerInstance(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	store := &requestTraceExportRepository{q: tx}
	task := service.RequestTraceExportTask{
		ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: service.RequestTraceExportPending,
		AdminUserID: 12, SessionDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		InstanceID: "node-one", CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, store.Create(ctx, task, 1))
	second := task
	second.ID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	require.ErrorIs(t, store.Create(ctx, second, 1), service.ErrRequestTraceExportLimit)
	stored, err := store.Get(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, service.RequestTraceExportPending, stored.Status)
	_, err = store.Claim(ctx, "node-two")
	require.ErrorIs(t, err, service.ErrRequestTraceExportNotFound)
	claimed, err := store.Claim(ctx, "node-one")
	require.NoError(t, err)
	require.Equal(t, service.RequestTraceExportRunning, claimed.Status)
	stored, err = store.Get(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, service.RequestTraceExportRunning, stored.Status)
	claimed.Status = service.RequestTraceExportCompleted
	claimed.Filename = "sub2api-request-trace-export-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.jsonl"
	when := time.Now().UTC()
	claimed.CompletedAt = &when
	until := when.Add(7 * 24 * time.Hour)
	claimed.DownloadUntil = &until
	require.NoError(t, store.Finish(ctx, claimed))
	stored, err = store.Get(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, claimed.Filename, stored.Filename)
	expired, err := store.Expired(ctx, until.Add(time.Second), 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	require.NoError(t, store.Delete(ctx, task.ID))
	_, err = store.Get(ctx, task.ID)
	require.ErrorIs(t, err, service.ErrRequestTraceExportNotFound)
}
