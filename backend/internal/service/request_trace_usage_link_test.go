//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type usageTraceLinkerStub struct {
	calls   int
	id      string
	usageID int64
}

func (l *usageTraceLinkerStub) LinkRequestTraceUsage(_ context.Context, id string, usageID int64) (bool, error) {
	l.calls++
	l.id = id
	l.usageID = usageID
	return true, nil
}
func (*usageTraceLinkerStub) ReconcileRequestTraceUsage(context.Context, string) (bool, error) {
	return false, nil
}
func (*usageTraceLinkerStub) DeleteExpiredRequestTraceUsageClaims(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func TestRequestTraceUsageLinkRequiresNewlyInsertedUsage(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	ctx := WithRequestTraceID(context.Background(), id)
	link := &usageTraceLinkerStub{}
	repo := &openAIRecordUsageLogRepoStub{inserted: false}
	log := &UsageLog{RequestID: "client:reused", APIKeyID: 7}
	inserted := writeUsageLogBestEffortWithTraceLink(ctx, repo, log, "test", link)
	require.False(t, inserted)
	require.Zero(t, link.calls, "an ON CONFLICT existing row is not this request's usage")

	repo.inserted = true
	log = &UsageLog{RequestID: "client:new", APIKeyID: 7}
	inserted = writeUsageLogBestEffortWithTraceLink(ctx, repo, log, "test", link)
	require.True(t, inserted)
	require.Equal(t, 1, link.calls)
	require.Equal(t, id, link.id)
	require.Equal(t, int64(42), link.usageID)
}

func TestRequestTraceUsageLinkOptionalWriterPreservesBatchProvenance(t *testing.T) {
	id := "1234567890abcdef1234567890abcdef"
	link := &usageTraceLinkerStub{}
	repo := &traceAwareUsageStub{inserted: true}
	log := &UsageLog{RequestID: "client:one", APIKeyID: 9}
	linked := writeUsageLogBestEffortWithTraceLink(WithRequestTraceID(context.Background(), id), repo, log, "trace-test", link)
	require.True(t, linked)
	require.Equal(t, 1, repo.bestEffortCalls)
	require.Zero(t, repo.createCalls, "batch path must remain in use when Trace is enabled")
	require.Equal(t, 1, link.calls)

	repo.inserted = false
	log = &UsageLog{RequestID: "client:reused", APIKeyID: 9}
	linked = writeUsageLogBestEffortWithTraceLink(WithRequestTraceID(context.Background(), id), repo, log, "trace-test", link)
	require.False(t, linked)
	require.Equal(t, 1, link.calls)
}

type traceAwareUsageStub struct {
	UsageLogRepository
	inserted        bool
	bestEffortCalls int
	createCalls     int
}

func (r *traceAwareUsageStub) CreateBestEffortWithInserted(_ context.Context, log *UsageLog) (bool, error) {
	r.bestEffortCalls++
	if log != nil {
		log.ID = 12
	}
	return r.inserted, nil
}
func (r *traceAwareUsageStub) Create(_ context.Context, _ *UsageLog) (bool, error) {
	r.createCalls++
	return false, nil
}

func TestGatewayAndOpenAIRecordUsageLinkOnlyFreshUsage(t *testing.T) {
	id := "abababababababababababababababab"
	for _, tc := range []struct {
		name   string
		openAI bool
	}{{"messages", false}, {"responses", true}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &openAIRecordUsageLogRepoStub{inserted: true}
			linker := &usageTraceLinkerStub{}
			ctx := WithRequestTraceID(context.Background(), id)
			var err error
			if tc.openAI {
				svc := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
				svc.SetRequestTraceUsageLinker(linker)
				err = svc.RecordUsage(ctx, &OpenAIRecordUsageInput{
					Result: &OpenAIForwardResult{RequestID: "response-link", Usage: OpenAIUsage{InputTokens: 2, OutputTokens: 3}, Model: "gpt-test", Duration: time.Second},
					APIKey: &APIKey{ID: 7, Group: &Group{RateMultiplier: 1}}, User: &User{ID: 8}, Account: &Account{ID: 9},
				})
			} else {
				svc := newGatewayRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
				svc.SetRequestTraceUsageLinker(linker)
				err = svc.RecordUsage(ctx, &RecordUsageInput{
					Result: &ForwardResult{RequestID: "message-link", Usage: ClaudeUsage{InputTokens: 2, OutputTokens: 3}, Model: "claude-test", Duration: time.Second},
					APIKey: &APIKey{ID: 7, Group: &Group{RateMultiplier: 1}}, User: &User{ID: 8}, Account: &Account{ID: 9},
				})
			}
			require.NoError(t, err)
			require.Equal(t, 1, linker.calls)
			require.Equal(t, id, linker.id)
		})
	}
}

func TestRequestTraceUsageLinkContextRejectsClientIDShapes(t *testing.T) {
	ctx := WithRequestTraceID(context.Background(), "client-supplied")
	require.Empty(t, RequestTraceIDFromContext(ctx))
	ctx = WithRequestTraceID(ctx, "0123456789abcdef0123456789abcdef")
	require.Equal(t, "0123456789abcdef0123456789abcdef", RequestTraceIDFromContext(ctx))
}
