//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type forcedAuditRepoStub struct {
	RequestAuditRepository
	err          error
	reserveErr   error
	finalizeErr  error
	markErr      error
	reserved     []RequestAuditAttempt
	scopes       []RequestAuditReservationScope
	finalized    *RequestAuditRecord
	finalKey     string
	markedKey    string
	markedReason string
	markCalls    int
}

func (s *forcedAuditRepoStub) ReserveAttempt(_ context.Context, scope RequestAuditReservationScope, attempt RequestAuditAttempt) error {
	s.scopes = append(s.scopes, scope)
	s.reserved = append(s.reserved, attempt)
	if s.reserveErr != nil {
		return s.reserveErr
	}
	return s.err
}

func (s *forcedAuditRepoStub) FinalizeReservation(_ context.Context, key string, usageLogID int64, rec *RequestAuditRecord) error {
	s.finalKey = key
	copy := *rec
	copy.UsageLogID = usageLogID
	s.finalized = &copy
	if s.finalizeErr != nil {
		return s.finalizeErr
	}
	return s.err
}
func (s *forcedAuditRepoStub) MarkReservationIncomplete(_ context.Context, key string, _ int64, reason string) error {
	s.markedKey = key
	s.markedReason = reason
	s.markCalls++
	return s.markErr
}
func (s *forcedAuditRepoStub) DeleteExpiredReservations(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func TestPrepareForcedRequestAuditGatesEachTransportAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := NewSettingService(&contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRequestAuditForceResponses: "true",
	}}, &config.Config{})
	repo := &forcedAuditRepoStub{}
	svc := &OpenAIGatewayService{settingService: settings, requestAuditRepo: repo}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Authorization", "Bearer secret")

	ctx, err := svc.prepareForcedRequestAudit(c.Request.Context(), c, RequestAuditRouteResponses)
	require.NoError(t, err)
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{AccountID: 77, Model: "gpt-5.4", Protocol: RequestAuditProtocolOpenAIResp})
	require.NoError(t, httpattempt.Before(ctx))
	require.Len(t, repo.reserved, 1)
	require.Equal(t, int64(77), repo.reserved[0].AccountID)
	require.True(t, repo.scopes[0].Forced)
	require.Equal(t, RequestAuditRouteResponses, repo.scopes[0].RouteFamily)
	require.Equal(t, []string{"present"}, repo.scopes[0].Headers.Values("Authorization"))
	require.NotContains(t, repo.scopes[0].Headers.Values("Authorization"), "Bearer secret")
	require.Empty(t, repo.reserved[0].Model, "caller-selected model aliases must not enter forced reservations")
	require.Empty(t, repo.reserved[0].ModelFingerprint, "attempts without a request fingerprint must not persist model identifiers")
}

func TestPrepareForcedRequestAuditFingerprintsModelBeforeReservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := NewSettingService(&contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRequestAuditForceResponses: "true",
	}}, &config.Config{})
	fingerprinter, err := NewRequestAuditFingerprinter("0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	fingerprint, err := fingerprinter.BeginForUser(77)
	require.NoError(t, err)

	repo := &forcedAuditRepoStub{}
	svc := &OpenAIGatewayService{settingService: settings, requestAuditRepo: repo}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	requestCtx := WithRequestAuditFingerprint(c.Request.Context(), fingerprint)

	ctx, err := svc.prepareForcedRequestAudit(requestCtx, c, RequestAuditRouteResponses)
	require.NoError(t, err)
	const modelCanary = "private-model-alias-canary"
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{
		AccountID: 77, Model: modelCanary, Protocol: RequestAuditProtocolOpenAIResp,
	})
	require.NoError(t, httpattempt.Before(ctx))
	require.Len(t, repo.reserved, 1)
	require.Equal(t, fingerprint.DigestModel(modelCanary), repo.reserved[0].ModelFingerprint)
	require.Empty(t, repo.reserved[0].Model, "raw model aliases must not enter forced reservations")
	require.NotContains(t, repo.reserved[0].ModelFingerprint, modelCanary)
}

func TestRequiredAuditTransportErrorsBypassFailoverClassification(t *testing.T) {
	required := &httpattempt.RequiredAuditError{Cause: errors.New("db down")}
	gateway := (&GatewayService{}).handleUpstreamTransportError(context.Background(), nil, nil, required, OpsUpstreamErrorEvent{})
	require.True(t, IsRequestAuditRequiredError(gateway))
	openAI := (&OpenAIGatewayService{}).handleOpenAIUpstreamTransportError(context.Background(), nil, nil, required, false)
	require.True(t, IsRequestAuditRequiredError(openAI))
}

func TestOpenAIChatForcedReservationFailurePreventsUpstreamSend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := NewSettingService(&contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRequestAuditForceChatCompletions: "true",
	}}, &config.Config{})
	repo := &forcedAuditRepoStub{err: errors.New("audit store down")}
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{StatusCode: http.StatusOK, Body: http.NoBody}}}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{}, settingService: settings,
		requestAuditRepo: repo, httpUpstream: upstream,
	}
	account := &Account{
		ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "acct"},
	}
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	ctx, err := svc.prepareForcedRequestAudit(c.Request.Context(), c, RequestAuditRouteChatCompletions)
	require.NoError(t, err)
	c.Request = c.Request.WithContext(ctx)
	result, err := svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
	require.Nil(t, result)
	require.True(t, IsRequestAuditRequiredError(err))
	require.Zero(t, upstream.callCount)
}

func TestOpenAIRecordUsageFinalizesForcedReservationAfterUsageID(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	forcedRepo := &forcedAuditRepoStub{}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.requestAuditRepo = forcedRepo

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "forced-audit-result", Usage: OpenAIUsage{InputTokens: 123, OutputTokens: 45},
			Model: "gpt-5.4", Duration: time.Second,
		},
		APIKey:          &APIKey{ID: 1, Quota: 100, Group: &Group{RateMultiplier: 1}},
		User:            &User{ID: 2},
		Account:         &Account{ID: 3},
		AuditLogicalKey: "logical-key-1",
		RequestAuditAttempts: []RequestAuditAttempt{{
			AccountID: 3, Model: "gpt-5.4", Protocol: RequestAuditProtocolOpenAIResp, Stage: RequestAuditStageWire,
		}},
	})
	require.NoError(t, err)
	require.Equal(t, "logical-key-1", forcedRepo.finalKey)
	require.NotNil(t, forcedRepo.finalized)
	require.Equal(t, int64(42), forcedRepo.finalized.UsageLogID)
	require.Equal(t, map[string]int{"input_tokens": 123, "output_tokens": 45}, forcedRepo.finalized.Metadata.Tokens)
	require.Len(t, forcedRepo.finalized.Attempts, 1)
}

func TestFinalizeForcedRequestAuditAddsOnlyKnownPositiveUsageTokens(t *testing.T) {
	for _, tc := range []struct {
		name         string
		inputTokens  int
		outputTokens int
		wantTokens   map[string]int
	}{
		{
			name:         "positive input and output",
			inputTokens:  123,
			outputTokens: 45,
			wantTokens:   map[string]int{"input_tokens": 123, "output_tokens": 45},
		},
		{
			name:         "zero output omitted",
			inputTokens:  123,
			outputTokens: 0,
			wantTokens:   map[string]int{"input_tokens": 123},
		},
		{
			name:         "zero input omitted",
			inputTokens:  0,
			outputTokens: 45,
			wantTokens:   map[string]int{"output_tokens": 45},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forcedRepo := &forcedAuditRepoStub{}
			logicalKey := "logical-key-token-" + tc.name
			finalizeRequestAuditBestEffort(context.Background(), forcedRepo, nil, &UsageLog{
				ID: 42, InputTokens: tc.inputTokens, OutputTokens: tc.outputTokens,
			}, logicalKey, RequestAuditInput{
				Metadata: RequestAuditMetadata{Tokens: map[string]int{
					"input_tokens": 999, "output_tokens": 999, "custom": 99,
				}},
			})

			require.NotNil(t, forcedRepo.finalized)
			require.Equal(t, int64(42), forcedRepo.finalized.UsageLogID)
			require.Equal(t, tc.wantTokens, forcedRepo.finalized.Metadata.Tokens)
		})
	}
}

func TestFinalizeForcedRequestAuditWithoutUsageDoesNotCreateAuditRow(t *testing.T) {
	forcedRepo := &forcedAuditRepoStub{}
	const logicalKey = "logical-key-without-usage"
	t.Cleanup(func() { clearForcedRequestAuditIncomplete(logicalKey) })

	finalizeRequestAuditBestEffort(context.Background(), forcedRepo, nil, nil, logicalKey, RequestAuditInput{})

	require.Nil(t, forcedRepo.finalized)
	require.Equal(t, 1, forcedRepo.markCalls, "reservation is marked incomplete without creating a linked audit row")
}

func TestForcedAuditPostStartSuccessfulReserveDoesNotMarkIncomplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := NewSettingService(&contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRequestAuditForceResponses: "true",
	}}, &config.Config{})
	repo := &forcedAuditRepoStub{}
	svc := &OpenAIGatewayService{settingService: settings, requestAuditRepo: repo}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx, err := svc.prepareForcedRequestAudit(c.Request.Context(), c, RequestAuditRouteResponses)
	require.NoError(t, err)
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.WriteHeaderNow()
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{AccountID: 1, Protocol: RequestAuditProtocolOpenAIResp})

	require.NoError(t, httpattempt.Before(ctx))
	require.Len(t, repo.reserved, 1)
	require.Empty(t, repo.markedKey)
	require.Zero(t, repo.markCalls)
	reason, attempts := forcedRequestAuditIncompleteForKey(repo.scopes[0].LogicalKey)
	require.Empty(t, reason)
	require.Empty(t, attempts)
}

func TestForcedAuditPostStartMarkFailureContinuesIncomplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := NewSettingService(&contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRequestAuditForceResponses: "true",
	}}, &config.Config{})
	repo := &forcedAuditRepoStub{
		markErr: errors.New("mark details must not leak"),
	}
	svc := &OpenAIGatewayService{settingService: settings, requestAuditRepo: repo}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx, err := svc.prepareForcedRequestAudit(c.Request.Context(), c, RequestAuditRouteResponses)
	require.NoError(t, err)
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{AccountID: 1, Protocol: RequestAuditProtocolOpenAIResp})
	require.NoError(t, httpattempt.Before(ctx))
	repo.reserveErr = errors.New("reserve details must not leak")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.WriteHeaderNow()

	// A post-start marker failure must not interrupt the response in flight.
	require.NoError(t, httpattempt.Before(ctx))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotEmpty(t, repo.markedKey)
	require.Equal(t, "write_failed_after_response_started", repo.markedReason)
	require.Equal(t, 1, repo.markCalls)

	finalizeRequestAuditBestEffort(ctx, repo, nil, &UsageLog{ID: 42}, repo.scopes[0].LogicalKey, RequestAuditInput{
		Attempts: []RequestAuditAttempt{{AccountID: 1, Protocol: RequestAuditProtocolOpenAIResp, Stage: RequestAuditStageWire}},
	})
	require.NotNil(t, repo.finalized)
	require.Equal(t, RequestAuditCaptureIncomplete, repo.finalized.CaptureCompleteness)
	require.Equal(t, "write_failed_after_response_started", repo.finalized.CaptureReason)
}

func TestForcedAuditReserveFailureAfterResponseStartContinuesIncomplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := NewSettingService(&contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRequestAuditForceResponses: "true",
	}}, &config.Config{})
	repo := &forcedAuditRepoStub{
		reserveErr: errors.New("reserve details must not leak"),
		markErr:    errors.New("mark details must not leak"),
	}
	svc := &OpenAIGatewayService{settingService: settings, requestAuditRepo: repo}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx, err := svc.prepareForcedRequestAudit(c.Request.Context(), c, RequestAuditRouteResponses)
	require.NoError(t, err)
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.WriteHeaderNow()
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{AccountID: 1, Protocol: RequestAuditProtocolOpenAIResp})

	require.NoError(t, httpattempt.Before(ctx))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "write_failed_after_response_started", repo.markedReason)
	require.Equal(t, 1, repo.markCalls)

	finalizeRequestAuditBestEffort(ctx, repo, nil, &UsageLog{ID: 42}, repo.scopes[0].LogicalKey, RequestAuditInput{
		Attempts: []RequestAuditAttempt{{AccountID: 1, Protocol: RequestAuditProtocolOpenAIResp, Stage: RequestAuditStageWire}},
	})
	require.NotNil(t, repo.finalized)
	require.Equal(t, RequestAuditCaptureIncomplete, repo.finalized.CaptureCompleteness)
	require.Equal(t, "write_failed_after_response_started", repo.finalized.CaptureReason)
}

func TestPrepareForcedRequestAuditBlocksReservationFailureButOrdinaryModeContinues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reservationErr := errors.New("audit store down")

	for _, tc := range []struct {
		name     string
		forced   bool
		wantFail bool
	}{
		{name: "forced", forced: true, wantFail: true},
		{name: "ordinary", forced: false, wantFail: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{}
			if tc.forced {
				values[SettingKeyRequestAuditForceChatCompletions] = "true"
			}
			settings := NewSettingService(&contentModerationTestSettingRepo{values: values}, &config.Config{})
			repo := &forcedAuditRepoStub{err: reservationErr}
			svc := &OpenAIGatewayService{settingService: settings, requestAuditRepo: repo}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			ctx, err := svc.prepareForcedRequestAudit(c.Request.Context(), c, RequestAuditRouteChatCompletions)
			require.NoError(t, err)
			ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{AccountID: 1})
			err = httpattempt.Before(ctx)
			if tc.wantFail {
				require.True(t, httpattempt.IsRequiredAuditError(err))
			} else {
				require.NoError(t, err)
			}
		})
	}
}
