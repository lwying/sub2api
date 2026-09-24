//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type openAI429AccountLimitUpstream struct {
	service.HTTPUpstream
	hits []int64
}

func (u *openAI429AccountLimitUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"rate limited"}}`)),
	}, nil
}

func TestOpenAIResponsesStopsAfterTwoAPIKeyAccountsReturn429(t *testing.T) {
	accounts := make([]service.Account, 0, 3)
	for id := int64(1); id <= 3; id++ {
		accounts = append(accounts, service.Account{
			ID: id, Name: "upstream", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Priority: int(id), Concurrency: 0,
			Credentials: map[string]any{"api_key": "sk-upstream", "base_url": "https://example.com"},
		})
	}
	upstream := &openAI429AccountLimitUpstream{}
	repo := openAIImagesFailoverAccountRepo{accounts: accounts}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 10
	c, rec := newOpenAIResponsesFailoverTestContext(t, context.Background())
	h.Responses(c)

	require.Equal(t, []int64{1, 2}, upstream.hits)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}
