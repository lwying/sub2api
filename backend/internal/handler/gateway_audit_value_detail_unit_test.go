//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type gatewayAuditValueUpstream struct{ hits []int64 }

func (u *gatewayAuditValueUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	return u.DoWithTLS(req, "", accountID, 0, nil)
}
func (u *gatewayAuditValueUpstream) DoWithTLS(req *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	attempt := httpattempt.StartRequestAttempt(req)
	status := http.StatusTooManyRequests
	responseBody := `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`
	responseHeaders := http.Header{"Retry-After": {"42"}, "Anthropic-Ratelimit-Requests-Remaining": {"0"}}
	if accountID == 2 {
		status = http.StatusOK
		responseHeaders = http.Header{"Content-Type": {"application/json"}}
		responseBody = `{"id":"msg_429_limit","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	}
	if attempt != nil {
		attempt.SetLatencyMillis(1)
		attempt.SetResponse(status, responseHeaders, true)
	}
	return &http.Response{StatusCode: status, Header: responseHeaders, Body: io.NopCloser(strings.NewReader(responseBody))}, nil
}

type gatewayAuditValueUsageRepo struct{ service.UsageLogRepository }

func (gatewayAuditValueUsageRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	log.ID = 9001
	return true, nil
}

type gatewayAuditValueAuditRepo struct {
	mu      sync.Mutex
	created *service.RequestAuditRecord
}

func (r *gatewayAuditValueAuditRepo) CreateRequestAudit(_ context.Context, rec *service.RequestAuditRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.created = service.SanitizeRequestAuditRecord(rec)
	return nil
}
func (r *gatewayAuditValueAuditRepo) GetByUsageLogID(_ context.Context, id int64) (*service.RequestAuditRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.created != nil && r.created.UsageLogID == id {
		return r.created, nil
	}
	return nil, nil
}

type gatewayAuditValueRepo struct {
	mu      sync.Mutex
	written []service.RequestAuditValueDetailWrite
	audit   *gatewayAuditValueAuditRepo
}

func (r *gatewayAuditValueRepo) CreateRequestAuditValueDetail(_ context.Context, write service.RequestAuditValueDetailWrite) (service.RequestAuditValueDetail, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.audit != nil {
		audit, err := r.audit.GetByUsageLogID(context.Background(), write.UsageLogID)
		if err != nil || audit == nil {
			return service.RequestAuditValueDetail{}, errors.New("value detail written without audit row")
		}
	}
	r.written = append(r.written, write)
	return service.RequestAuditValueDetail{UsageLogID: write.UsageLogID}, nil
}
func (*gatewayAuditValueRepo) GetRequestAuditValueDetail(context.Context, int64) (service.RequestAuditValueDetail, error) {
	return service.RequestAuditValueDetail{}, nil
}
func (*gatewayAuditValueRepo) ReadRequestAuditValueDetailValues(context.Context, int64, time.Time) (service.RequestAuditValueDetailValues, error) {
	return service.RequestAuditValueDetailValues{}, nil
}
func (*gatewayAuditValueRepo) ClearExpiredRequestAuditValueDetails(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}
func (r *gatewayAuditValueRepo) snapshot() []service.RequestAuditValueDetailWrite {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]service.RequestAuditValueDetailWrite(nil), r.written...)
}

type gatewayAuditValueGate struct{}

func (gatewayAuditValueGate) RequestAuditValueDetailGate(context.Context) service.RequestAuditValueDetailGate {
	return service.RequestAuditValueDetailGate{CaptureAllowed: true, EncryptionAvailable: true}
}

func TestGatewayMessagesA429BSuccessCapturesWireValueDetails(t *testing.T) {
	groupID := int64(7999)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	accounts := gateway429TestAccounts(groupID, 2)
	upstream := &gatewayAuditValueUpstream{}
	auditRepo := &gatewayAuditValueAuditRepo{}
	valueRepo := &gatewayAuditValueRepo{audit: auditRepo}
	base := newGateway429TestHandler(t, upstream, accounts, group)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: []service.Account{*accounts[0], *accounts[1]}}
	rateLimit := service.NewRateLimitService(gateway429NoCooldownRepo{accountRepo}, nil, cfg, nil, nil)
	gw := service.NewGatewayService(nil, &fakeGroupRepo{group: group}, gatewayAuditValueUsageRepo{}, auditRepo, nil, nil, nil, nil, nil,
		cfg, service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil), nil, service.NewBillingService(cfg, nil), rateLimit, nil, nil, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	gw.SetRequestAuditValueDetailCapture(service.NewRequestAuditValueDetailCapture(valueRepo, gatewayAuditValueGate{}))
	base.gatewayService = gw
	c, rec := newGateway429TestContext(t, group)
	const userID = `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"","session_id":"123e4567-e89b-12d3-a456-426614174000"}`
	body := `{"model":"claude-sonnet-4-5","max_tokens":32,"metadata":{"user_id":` + strconv.Quote(userID) + `},"messages":[{"role":"user","content":"do not store this prompt"}]}`
	c.Request.Body = io.NopCloser(bytes.NewBufferString(body))
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.258 (external, cli)")
	base.Messages(c)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []int64{1, 2}, upstream.hits)
	audit, err := auditRepo.GetByUsageLogID(context.Background(), 9001)
	require.NoError(t, err)
	require.NotNil(t, audit, "the usage-owned audit must be written before its value detail")
	require.Equal(t, map[string]any{"present": true}, audit.Headers["User-Agent"])
	writes := valueRepo.snapshot()
	require.Len(t, writes, 1, "usage-owned audit value detail must be attached after A429/B200")
	require.Equal(t, int64(9001), writes[0].UsageLogID)
	require.Equal(t, service.RequestAuditValueDetailStateStored, writes[0].State)
	values, err := service.DecodeRequestAuditValueDetailValues(writes[0].Payload)
	require.NoError(t, err)
	require.Len(t, values.Attempts, 2)
	require.Equal(t, int64(1), values.Attempts[0].AccountID)
	require.Equal(t, []string{"42"}, values.Attempts[0].ResponseHeaders["Retry-After"])
	require.Equal(t, int64(2), values.Attempts[1].AccountID)
	require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", values.Inbound.DeviceID)
	require.NotContains(t, string(writes[0].Payload), "do not store this prompt")
}
