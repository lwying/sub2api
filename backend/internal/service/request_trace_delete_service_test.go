//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type requestTraceDeleteCipherStub struct{}

func (requestTraceDeleteCipherStub) Encrypt(plaintext string) (string, error) {
	return "enc:" + plaintext, nil
}

func (requestTraceDeleteCipherStub) Decrypt(ciphertext string) (string, error) {
	if !strings.HasPrefix(ciphertext, "enc:") {
		return "", errors.New("unreadable token")
	}
	return strings.TrimPrefix(ciphertext, "enc:"), nil
}

type requestTraceDeleteRepoStub struct {
	matched       int64
	snapshotMaxID int64
	previewErr    error

	byFilterDeleted  int64
	byFilterDone     bool
	byFilterErr      error
	receivedFilter   RequestTraceExportFilter
	receivedSnapshot int64
	receivedBatch    int

	byIDsDeleted int64
	byIDsErr     error
	receivedIDs  []string
	deadline     time.Time
}

func (s *requestTraceDeleteRepoStub) PreviewRequestTraceDelete(_ context.Context, _ RequestTraceExportFilter) (int64, int64, error) {
	return s.matched, s.snapshotMaxID, s.previewErr
}

func (s *requestTraceDeleteRepoStub) DeleteRequestTracesByIDs(_ context.Context, traceIDs []string) (int64, error) {
	s.receivedIDs = traceIDs
	return s.byIDsDeleted, s.byIDsErr
}

func (s *requestTraceDeleteRepoStub) DeleteRequestTracesByFilter(ctx context.Context, filter RequestTraceExportFilter, snapshotMaxID int64, batchSize int) (int64, bool, error) {
	s.deadline, _ = ctx.Deadline()
	s.receivedFilter, s.receivedSnapshot, s.receivedBatch = filter, snapshotMaxID, batchSize
	return s.byFilterDeleted, s.byFilterDone, s.byFilterErr
}

func newRequestTraceDeleteServiceForTest() (*RequestTraceDeleteService, *requestTraceDeleteRepoStub) {
	repo := &requestTraceDeleteRepoStub{matched: 42, snapshotMaxID: 900, byFilterDeleted: 42, byFilterDone: true}
	return NewRequestTraceDeleteService(repo, requestTraceDeleteCipherStub{}), repo
}

func TestRequestTraceDeletePreviewBindsAdminFilterAndSnapshot(t *testing.T) {
	svc, repo := newRequestTraceDeleteServiceForTest()
	svc.now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }

	preview, err := svc.PreviewDelete(context.Background(), RequestTraceExportFilter{Platform: "openai"}, 7)
	require.NoError(t, err)
	require.Equal(t, int64(42), preview.MatchedCount)
	require.Equal(t, int64(900), preview.SnapshotMaxID)
	require.NotEmpty(t, preview.FilterHash)
	require.NotEmpty(t, preview.ConfirmationToken)
	require.Equal(t, svc.now().Add(RequestTraceDeleteConfirmationTTL), preview.ExpiresAt)
	require.Equal(t, preview.FilterHash, RequestTraceDeleteFilterHash(RequestTraceExportFilter{Platform: "openai"}, 900))

	// The preview is read-only: it never deletes anything.
	require.Zero(t, repo.receivedIDs)
}

func TestRequestTraceDeleteByFilterAcceptsBoundToken(t *testing.T) {
	svc, repo := newRequestTraceDeleteServiceForTest()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	filter := RequestTraceExportFilter{RequestedModel: "gpt-5"}
	preview, err := svc.PreviewDelete(context.Background(), filter, 7)
	require.NoError(t, err)

	result, err := svc.DeleteByFilter(context.Background(), RequestTraceDeleteByFilterRequest{
		Filter: filter, SnapshotMaxID: preview.SnapshotMaxID, FilterHash: preview.FilterHash,
		ConfirmationToken: preview.ConfirmationToken, Confirm: true,
	}, 7)
	require.NoError(t, err)
	require.Equal(t, int64(42), result.DeletedCount)
	require.True(t, result.Completed)
	require.Equal(t, int64(900), repo.receivedSnapshot)
	require.Equal(t, RequestTraceDeleteBatchSize, repo.receivedBatch)
}

func TestRequestTraceDeleteByFilterLimitsOverallRuntime(t *testing.T) {
	svc, repo := newRequestTraceDeleteServiceForTest()
	filter := RequestTraceExportFilter{Platform: "openai"}
	preview, err := svc.PreviewDelete(context.Background(), filter, 7)
	require.NoError(t, err)
	started := time.Now()
	_, err = svc.DeleteByFilter(context.Background(), RequestTraceDeleteByFilterRequest{
		Filter: filter, SnapshotMaxID: preview.SnapshotMaxID, FilterHash: preview.FilterHash,
		ConfirmationToken: preview.ConfirmationToken, Confirm: true,
	}, 7)
	require.NoError(t, err)
	require.False(t, repo.deadline.IsZero(), "批量清理必须有整体执行时限")
	require.LessOrEqual(t, repo.deadline.Sub(started), 26*time.Second)
}

func TestRequestTraceDeleteByFilterRejectsMismatchedBindings(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	filter := RequestTraceExportFilter{RequestedModel: "gpt-5"}

	makePreview := func(t *testing.T, svc *RequestTraceDeleteService, adminID int64) *RequestTraceDeletePreview {
		t.Helper()
		preview, err := svc.PreviewDelete(context.Background(), filter, adminID)
		require.NoError(t, err)
		return preview
	}

	t.Run("tampered filter hash", func(t *testing.T) {
		svc, _ := newRequestTraceDeleteServiceForTest()
		svc.now = func() time.Time { return now }
		preview := makePreview(t, svc, 7)
		_, err := svc.DeleteByFilter(context.Background(), RequestTraceDeleteByFilterRequest{
			Filter: filter, SnapshotMaxID: preview.SnapshotMaxID, FilterHash: "deadbeef",
			ConfirmationToken: preview.ConfirmationToken, Confirm: true,
		}, 7)
		require.ErrorIs(t, err, ErrRequestTraceDeleteConfirmationInvalid)
	})

	t.Run("changed filter", func(t *testing.T) {
		svc, _ := newRequestTraceDeleteServiceForTest()
		svc.now = func() time.Time { return now }
		preview := makePreview(t, svc, 7)
		_, err := svc.DeleteByFilter(context.Background(), RequestTraceDeleteByFilterRequest{
			Filter: RequestTraceExportFilter{RequestedModel: "gpt-4"}, SnapshotMaxID: preview.SnapshotMaxID,
			FilterHash: preview.FilterHash, ConfirmationToken: preview.ConfirmationToken, Confirm: true,
		}, 7)
		require.ErrorIs(t, err, ErrRequestTraceDeleteConfirmationInvalid)
	})

	t.Run("different admin", func(t *testing.T) {
		svc, _ := newRequestTraceDeleteServiceForTest()
		svc.now = func() time.Time { return now }
		preview := makePreview(t, svc, 7)
		_, err := svc.DeleteByFilter(context.Background(), RequestTraceDeleteByFilterRequest{
			Filter: filter, SnapshotMaxID: preview.SnapshotMaxID, FilterHash: preview.FilterHash,
			ConfirmationToken: preview.ConfirmationToken, Confirm: true,
		}, 8)
		require.ErrorIs(t, err, ErrRequestTraceDeleteConfirmationInvalid)
	})

	t.Run("expired token", func(t *testing.T) {
		svc, _ := newRequestTraceDeleteServiceForTest()
		svc.now = func() time.Time { return now }
		preview := makePreview(t, svc, 7)
		svc.now = func() time.Time { return now.Add(RequestTraceDeleteConfirmationTTL) }
		_, err := svc.DeleteByFilter(context.Background(), RequestTraceDeleteByFilterRequest{
			Filter: filter, SnapshotMaxID: preview.SnapshotMaxID, FilterHash: preview.FilterHash,
			ConfirmationToken: preview.ConfirmationToken, Confirm: true,
		}, 7)
		require.ErrorIs(t, err, ErrRequestTraceDeleteConfirmationInvalid)
	})
}

func TestRequestTraceDeleteRequiresConfirmAndRealFilter(t *testing.T) {
	svc, _ := newRequestTraceDeleteServiceForTest()

	_, err := svc.DeleteByFilter(context.Background(), RequestTraceDeleteByFilterRequest{Confirm: false}, 7)
	require.ErrorIs(t, err, ErrRequestTraceDeleteConfirmRequired)

	_, err = svc.PreviewDelete(context.Background(), RequestTraceExportFilter{}, 7)
	require.ErrorIs(t, err, ErrRequestTraceDeleteInvalidFilter)

	// A whitespace-only keyword is an explicit filter that resolves to empty: reject it.
	_, err = svc.PreviewDelete(context.Background(), RequestTraceExportFilter{Keyword: "   "}, 7)
	require.ErrorIs(t, err, ErrRequestTraceDeleteInvalidFilter)

	// Selected ids belong to the batch endpoint, not the filter endpoint.
	_, err = svc.PreviewDelete(context.Background(), RequestTraceExportFilter{TraceIDs: []string{"0123456789abcdef0123456789abcdef"}}, 7)
	require.ErrorIs(t, err, ErrRequestTraceDeleteInvalidFilter)
}

func TestRequestTraceDeleteRejectsFalseOnlyUnknownFilter(t *testing.T) {
	svc, _ := newRequestTraceDeleteServiceForTest()
	no := false
	// `*_unknown=false` is the UI default, not a real selection: it must not be
	// accepted on its own, otherwise a default-valued request would delete broadly.
	for name, filter := range map[string]RequestTraceExportFilter{
		"group_unknown":    {GroupUnknown: &no},
		"model_unknown":    {ModelUnknown: &no},
		"platform_unknown": {PlatformUnknown: &no},
		"user_unknown":     {UserUnknown: &no},
		"api_key_unknown":  {APIKeyUnknown: &no},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.PreviewDelete(context.Background(), filter, 7)
			require.ErrorIs(t, err, ErrRequestTraceDeleteInvalidFilter)
		})
	}

	// `*_unknown=true` is a real positive selection and must be accepted.
	yes := true
	_, err := svc.PreviewDelete(context.Background(), RequestTraceExportFilter{GroupUnknown: &yes}, 7)
	require.NoError(t, err)
}

func TestRequestTraceDeleteByIDsValidatesSelection(t *testing.T) {
	svc, repo := newRequestTraceDeleteServiceForTest()
	id := "0123456789abcdef0123456789abcdef"

	result, err := svc.DeleteByIDs(context.Background(), []string{id}, true)
	require.NoError(t, err)
	require.True(t, result.Completed)
	require.Equal(t, []string{id}, repo.receivedIDs)

	_, err = svc.DeleteByIDs(context.Background(), nil, true)
	require.ErrorIs(t, err, ErrRequestTraceDeleteSelectionInvalid)

	_, err = svc.DeleteByIDs(context.Background(), []string{id, id}, true)
	require.ErrorIs(t, err, ErrRequestTraceDeleteSelectionInvalid)

	_, err = svc.DeleteByIDs(context.Background(), []string{"not-a-trace-id"}, true)
	require.ErrorIs(t, err, ErrRequestTraceDeleteSelectionInvalid)

	_, err = svc.DeleteByIDs(context.Background(), []string{id}, false)
	require.ErrorIs(t, err, ErrRequestTraceDeleteConfirmRequired)
}

func TestRequestTraceDeleteByFilterReportsPartialCompletion(t *testing.T) {
	svc, repo := newRequestTraceDeleteServiceForTest()
	svc.now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }
	filter := RequestTraceExportFilter{Keyword: "abc"}
	preview, err := svc.PreviewDelete(context.Background(), filter, 7)
	require.NoError(t, err)

	repo.byFilterDeleted, repo.byFilterDone = 5, false
	repo.byFilterErr = errors.New("deadline exceeded")
	result, err := svc.DeleteByFilter(context.Background(), RequestTraceDeleteByFilterRequest{
		Filter: filter, SnapshotMaxID: preview.SnapshotMaxID, FilterHash: preview.FilterHash,
		ConfirmationToken: preview.ConfirmationToken, Confirm: true,
	}, 7)
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, int64(5), result.DeletedCount, "partial deletion must report the real count")
	require.False(t, result.Completed)
}

func TestRequestTraceDeleteAvailableWithoutCipher(t *testing.T) {
	svc := NewRequestTraceDeleteService(&requestTraceDeleteRepoStub{}, nil)
	_, err := svc.PreviewDelete(context.Background(), RequestTraceExportFilter{Keyword: "x"}, 7)
	require.ErrorIs(t, err, ErrRequestTraceRepositoryUnavailable)
}
