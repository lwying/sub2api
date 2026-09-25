//go:build unit

package service_test

// Responses 分支错误诊断的端到端接缝测试（票据 04）。
//
// 与同目录的 in-package 测试（error_diagnostic_responses_branch_test.go）不同，这里用的是
// 生产实现 repository.NewHTTPUpstream 与真实的 HTTP RoundTrip，因此证明的是「Responses 分支
// 绑定的观察者确实被 internal/repository/http_upstream.go 的发送接缝看到」这件事，而不是
// 一个复刻替身的行为。上游是本机 httptest 服务器：管理员最终取到的正文与服务器实际收到的
// 字节逐字节比对。
//
// service 包的 in-package 测试无法 import repository（repository 依赖 service，会形成环），
// 因此本文件放在外部测试包 service_test 中。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository" //nolint:depguard // real transport black-box seam requires external service_test import
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const seamAnthropicStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_seam","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":"","usage":{"input_tokens":9}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// seamSettingRepo 提供错误诊断门控读取。
type seamSettingRepo struct{ values map[string]string }

func (r *seamSettingRepo) Get(context.Context, string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}
func (r *seamSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}
func (r *seamSettingRepo) Set(context.Context, string, string) error { return nil }
func (r *seamSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return r.values, nil
}
func (r *seamSettingRepo) SetMultiple(context.Context, map[string]string) error { return nil }
func (r *seamSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return r.values, nil
}
func (r *seamSettingRepo) Delete(context.Context, string) error { return nil }

// seamStableEncryptionKey 是夹具用的稳定 AES-256 密钥（64 位 hex = 32 字节）。
//
// 接缝只在门控允许**且**这把密钥可用时才 tee 出站正文；本用例逐字节比对留存正文，
// 因此门控读取器必须带上它。
const seamStableEncryptionKey = "5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c5c"

// seamRiskAckJSON 造一条覆盖当前语句版本的有效书面确认。
//
// 采集门控只在「存量布尔值 且 存在当前版本的有效书面确认」时才放行
// （见 ApplyErrorDiagnosticRiskAcknowledgement），因此只写门控键会让本测试永远采不到。
// 语句原文必须与当前版本语句逐字相同（见 CoversCurrentStatement），故这里直接用常量。
func seamRiskAckJSON(t *testing.T) string {
	t.Helper()
	payload, err := json.Marshal(service.ErrorDiagnosticRiskAcknowledgement{
		Version:     service.ErrorDiagnosticRiskAcknowledgementVersion,
		Phrase:      service.ErrorDiagnosticRiskAcknowledgementPhraseEN,
		AdminUserID: 7,
		AcceptedAt:  time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	return string(payload)
}

// seamBodyCipher 用可逆标记代替真实加密，只用于逐字节比对。
type seamBodyCipher struct{}

func (seamBodyCipher) Encrypt(plaintext []byte) ([]byte, error) {
	return append([]byte("seam:"), plaintext...), nil
}
func (seamBodyCipher) Decrypt(ciphertext []byte) ([]byte, error) { return ciphertext[5:], nil }
func (seamBodyCipher) KeyVersion() int                           { return 1 }

// seamDiagnosticRepo 是 ErrorDiagnosticRepository 的最小内存实现。
type seamDiagnosticRepo struct {
	mu      sync.Mutex
	records map[string]service.ErrorDiagnosticRecord
	bodies  map[string][]byte
	// headerValues 与 bodies 分开：头值与正文是两条独立留存路径，替身不得把它们混成一份。
	headerValues map[string][]byte
}

func newSeamDiagnosticRepo() *seamDiagnosticRepo {
	return &seamDiagnosticRepo{
		records: map[string]service.ErrorDiagnosticRecord{},
		bodies:  map[string][]byte{},
	}
}

func (r *seamDiagnosticRepo) CreateErrorDiagnostic(_ context.Context, write service.ErrorDiagnosticWrite, now time.Time) (service.ErrorDiagnosticRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := service.ErrorDiagnosticRecord{
		ID:                 write.ID,
		UsageLogID:         write.Attempt.UsageLogID,
		HasUsage:           write.Attempt.UsageLogID > 0,
		Protocol:           write.Attempt.Protocol,
		AttemptIndex:       write.Attempt.AttemptIndex,
		Stage:              write.Attempt.Stage,
		UpstreamStatusCode: write.Attempt.UpstreamStatusCode,
		BodyState:          write.BodyState,
		BodyReason:         write.BodyReason,
		CreatedAt:          now,
		MetadataExpiresAt:  now.Add(service.ErrorDiagnosticMetadataRetention),
	}
	if len(write.BodyCiphertext) > 0 {
		record.BodyStored = true
		record.BodyExpiresAt = now.Add(service.ErrorDiagnosticBodyRetention)
		record.BodyBytes = len(write.Attempt.Body)
		r.bodies[write.ID] = write.BodyCiphertext
	}
	record.HeaderState = write.HeaderState
	record.HeaderReason = write.HeaderReason
	record.HeaderEntryCount = write.HeaderEntryCount
	if len(write.HeaderCiphertext) > 0 {
		record.HeaderStored = true
		record.HeaderExpiresAt = now.Add(service.ErrorDiagnosticHeaderRetention)
		record.HeaderBytes = write.HeaderPayloadBytes
		if r.headerValues == nil {
			r.headerValues = map[string][]byte{}
		}
		r.headerValues[write.ID] = write.HeaderCiphertext
	}
	r.records[write.ID] = record
	return record, nil
}

func (r *seamDiagnosticRepo) ReadErrorDiagnosticHeaderValues(_ context.Context, id string, now time.Time) (service.ErrorDiagnosticHeaderValues, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok || !record.HeaderValuesReadableAt(now) {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	ciphertext, ok := r.headerValues[id]
	if !ok {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	plaintext, err := seamBodyCipher{}.Decrypt(ciphertext)
	if err != nil {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	values, err := service.DecodeErrorDiagnosticHeaderValues(plaintext)
	if err != nil {
		return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
	}
	return values, nil
}

func (r *seamDiagnosticRepo) ClearExpiredErrorDiagnosticHeaderValues(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (r *seamDiagnosticRepo) GetErrorDiagnostic(_ context.Context, id string) (service.ErrorDiagnosticRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok {
		return service.ErrorDiagnosticRecord{}, service.ErrErrorDiagnosticNotFound
	}
	return record, nil
}

func (r *seamDiagnosticRepo) ListErrorDiagnosticsByUsageLog(context.Context, int64, int) ([]service.ErrorDiagnosticRecord, error) {
	return nil, nil
}

func (r *seamDiagnosticRepo) ListRecentErrorDiagnostics(context.Context, string, int) ([]service.ErrorDiagnosticRecord, error) {
	return nil, nil
}

func (r *seamDiagnosticRepo) ListRecentErrorDiagnosticPage(context.Context, string, time.Time, int, int) ([]service.ErrorDiagnosticRecord, error) {
	return nil, nil
}

func (r *seamDiagnosticRepo) CountRecentErrorDiagnostics(context.Context, string, time.Time) (int64, error) {
	return 0, nil
}

func (r *seamDiagnosticRepo) ReadErrorDiagnosticBody(_ context.Context, id string, now time.Time) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok || !record.BodyReadableAt(now) {
		return nil, service.ErrErrorDiagnosticBodyGone
	}
	ciphertext, ok := r.bodies[id]
	if !ok {
		return nil, service.ErrErrorDiagnosticBodyGone
	}
	return seamBodyCipher{}.Decrypt(ciphertext)
}

func (r *seamDiagnosticRepo) ClearExpiredErrorDiagnosticBodies(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (r *seamDiagnosticRepo) DeleteExpiredErrorDiagnostics(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (r *seamDiagnosticRepo) created() []service.ErrorDiagnosticRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]service.ErrorDiagnosticRecord, 0, len(r.records))
	for _, record := range r.records {
		out = append(out, record)
	}
	return out
}

// seamRecorder 让异步的接缝写入变成可等待的事实。
type seamRecorder struct {
	service *service.ErrorDiagnosticService
	done    chan struct{}
}

func (r *seamRecorder) RecordErrorDiagnostic(ctx context.Context, attempt service.ErrorDiagnosticAttempt) (service.ErrorDiagnosticRecord, error) {
	record, err := r.service.RecordErrorDiagnostic(ctx, attempt)
	select {
	case r.done <- struct{}{}:
	default:
	}
	return record, err
}

// TestResponsesBranchErrorDiagnostic_RealTransportSeamDrivesRepository 固定票据 04 的端到端
// 事实：Responses 分支的真实发送（生产 HTTPUpstream + 真实 RoundTrip）先 5xx 再成功时，
// 管理员能取到那次失败尝试的真实出站字节，且成功那次不产生诊断。
func TestResponsesBranchErrorDiagnostic_RealTransportSeamDrivesRepository(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var (
		mu       sync.Mutex
		received [][]byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		mu.Lock()
		received = append(received, body)
		index := len(received) - 1
		mu.Unlock()

		if index == 0 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream boom"}}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(seamAnthropicStream))
	}))
	defer upstream.Close()

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	// 门控读取器单独带一把稳定密钥：接缝只在密钥可用时才 tee 出站正文，
	// 而本用例正是要逐字节比对留存下来的出站正文。
	settingCfg := &config.Config{Totp: config.TotpConfig{
		EncryptionKey:           seamStableEncryptionKey,
		EncryptionKeyConfigured: true,
	}}
	settingService := service.NewSettingService(&seamSettingRepo{values: map[string]string{
		service.SettingKeyErrorDiagnostic: mustSeamJSON(t, service.ErrorDiagnosticSettings{
			Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true,
		}),
		service.SettingKeyErrorDiagnosticRiskAcknowledgement: seamRiskAckJSON(t),
	}}, settingCfg)

	repo := newSeamDiagnosticRepo()
	diagnostics := service.NewErrorDiagnosticService(repo, settingService, seamBodyCipher{})
	recorder := &seamRecorder{service: diagnostics, done: make(chan struct{}, 8)}

	// 参数顺序与 NewGatewayService 的签名一致；未使用的依赖显式传 nil，避免为本测试
	// 构造完整装配图。
	gateway := service.NewGatewayService(
		nil, // accountRepo
		nil, // groupRepo
		nil, // usageLogRepo
		nil, // requestAuditRepo
		nil, // usageBillingRepo
		nil, // userRepo
		nil, // userSubRepo
		nil, // userGroupRateRepo
		nil, // cache
		cfg,
		nil, // schedulerSnapshot
		nil, // concurrencyService
		nil, // billingService
		nil, // rateLimitService
		nil, // billingCacheService
		nil, // identityService
		repository.NewHTTPUpstream(nil),
		nil, // deferredService
		nil, // claudeTokenProvider
		nil, // sessionLimitCache
		nil, // rpmCache
		nil, // digestStore
		settingService,
		&service.TLSFingerprintProfileService{},
		nil, // channelService
		nil, // resolver
		nil, // compositeResolver
		nil, // balanceNotifyService
		nil, // userPlatformQuotaRepo
	)
	gateway.SetErrorDiagnosticRecorder(recorder)

	out := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(out)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	account := &service.Account{
		ID:       21,
		Name:     "responses-seam-account",
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-seam",
			"base_url": upstream.URL,
		},
	}
	body := []byte(`{"model":"claude-sonnet-4-5","input":"seam byte exact"}`)

	_, err := gateway.ForwardAsResponses(context.Background(), c, account, body, nil)
	var failover *service.UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusInternalServerError, failover.StatusCode)

	result, err := gateway.ForwardAsResponses(context.Background(), c, account, body, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	select {
	case <-recorder.done:
	case <-time.After(5 * time.Second):
		t.Fatal("expected one stored error diagnostic for the real failed attempt")
	}

	records := repo.created()
	require.Len(t, records, 1, "the successful retry must not add a diagnostic")
	record := records[0]
	require.Equal(t, service.ErrorDiagnosticProtocolResponses, record.Protocol)
	require.Equal(t, 1, record.AttemptIndex)
	require.Equal(t, http.StatusInternalServerError, record.UpstreamStatusCode)
	require.Equal(t, service.ErrorDiagnosticBodyStateStored, record.BodyState)

	mu.Lock()
	wireBytes := append([]byte(nil), received[0]...)
	mu.Unlock()

	stored, err := diagnostics.ReadErrorDiagnosticBody(context.Background(), record.ID)
	require.NoError(t, err)
	require.Equal(t, string(wireBytes), string(stored),
		"the administrable body must equal the bytes the real transport put on the wire")
	require.NotEqual(t, string(body), string(stored), "the inbound client body is not the outbound body")
	require.Contains(t, string(stored), "seam byte exact")
}

func mustSeamJSON(t *testing.T, settings service.ErrorDiagnosticSettings) string {
	t.Helper()
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	return string(raw)
}
