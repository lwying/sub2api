//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUsageLogBestEffortTraceProofReportsOnlyActualInsert(t *testing.T) {
	ctx := context.Background()
	repo := newUsageLogRepositoryWithSQL(testEntClient(t), integrationDB)
	user := mustCreateUser(t, testEntClient(t), &service.User{Email: fmt.Sprintf("trace-proof-%d@example.com", time.Now().UnixNano())})
	key := mustCreateApiKey(t, testEntClient(t), &service.APIKey{UserID: user.ID, Key: "sk-trace-proof-" + uuid.NewString(), Name: "proof"})
	account := mustCreateAccount(t, testEntClient(t), &service.Account{Name: "trace-proof-" + uuid.NewString()})
	makeLog := func(requestID string) *service.UsageLog {
		return &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: requestID,
			Model: "claude-test", InputTokens: 3, OutputTokens: 2, CreatedAt: time.Now().UTC()}
	}
	requestID := uuid.NewString()
	first := makeLog(requestID)
	inserted, err := repo.CreateBestEffortWithInserted(ctx, first)
	require.NoError(t, err)
	require.True(t, inserted)
	require.Positive(t, first.ID)

	second := makeLog(requestID)
	inserted, err = repo.CreateBestEffortWithInserted(ctx, second)
	require.NoError(t, err)
	require.False(t, inserted, "recent-key or ON CONFLICT dedup is not this request's usage")

	third := makeLog(uuid.NewString())
	inserted, err = repo.CreateBestEffortWithInserted(ctx, third)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NotEqual(t, first.ID, third.ID)
}
