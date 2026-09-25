//go:build unit

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---- 测试替身 ----

type valueDetailSettingRepoStub struct {
	values map[string]string
}

func (r *valueDetailSettingRepoStub) Get(_ context.Context, key string) (*service.Setting, error) {
	if value, ok := r.values[key]; ok {
		return &service.Setting{Key: key, Value: value}, nil
	}
	return nil, service.ErrSettingNotFound
}

func (r *valueDetailSettingRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	setting, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (r *valueDetailSettingRepoStub) Set(ctx context.Context, key, value string) error {
	return r.SetMultiple(ctx, map[string]string{key: value})
}

func (r *valueDetailSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *valueDetailSettingRepoStub) SetMultiple(_ context.Context, settings map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *valueDetailSettingRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	out := map[string]string{}
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *valueDetailSettingRepoStub) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

type valueDetailRepoStub struct {
	detail  service.RequestAuditValueDetail
	getErr  error
	values  service.RequestAuditValueDetailValues
	readErr error
}

func (s *valueDetailRepoStub) CreateRequestAuditValueDetail(context.Context, service.RequestAuditValueDetailWrite) (service.RequestAuditValueDetail, error) {
	return service.RequestAuditValueDetail{}, nil
}

func (s *valueDetailRepoStub) GetRequestAuditValueDetail(context.Context, int64) (service.RequestAuditValueDetail, error) {
	if s.getErr != nil {
		return service.RequestAuditValueDetail{}, s.getErr
	}
	return s.detail, nil
}

func (s *valueDetailRepoStub) ReadRequestAuditValueDetailValues(context.Context, int64, time.Time) (service.RequestAuditValueDetailValues, error) {
	if s.readErr != nil {
		return service.RequestAuditValueDetailValues{}, s.readErr
	}
	return s.values, nil
}

func (s *valueDetailRepoStub) ClearExpiredRequestAuditValueDetails(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func valueDetailConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{Totp: config.TotpConfig{
		EncryptionKey:           "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
		EncryptionKeyConfigured: true,
	}}
}

func newValueDetailRouter(t *testing.T, repo service.RequestAuditValueDetailRepository, settingsRepo *valueDetailSettingRepoStub) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	settings := service.NewSettingService(settingsRepo, valueDetailConfig(t))
	handler := &UsageHandler{}
	handler.SetRequestAuditValueDetailService(service.NewRequestAuditValueDetailService(repo, settings))

	router := gin.New()
	// 管理员身份由 adminAuth 中间件注入；这里用一个固定主体代替真实鉴权栈。
	router.Use(func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 7})
		c.Next()
	})
	router.GET("/usage/:id/request-audit/value-detail", handler.GetRequestAuditValueDetail)
	router.POST("/usage/:id/request-audit/value-detail", handler.RevealRequestAuditValueDetail)
	router.GET("/usage/request-audit-value-detail-settings", handler.GetRequestAuditValueDetailSettings)
	router.PUT("/usage/request-audit-value-detail-settings", handler.UpdateRequestAuditValueDetailSettings)
	return router
}

func valueDetailStoredDetail(now time.Time) service.RequestAuditValueDetail {
	return service.RequestAuditValueDetail{
		UsageLogID:   123,
		State:        service.RequestAuditValueDetailStateStored,
		Reason:       service.RequestAuditValueDetailRetained,
		Stored:       true,
		ExpiresAt:    now.Add(6 * 24 * time.Hour),
		CreatedAt:    now,
		EntryCount:   9,
		AttemptCount: 2,
		PayloadBytes: 512,
		Fields: service.RequestAuditValueDetailFields{
			Route:        service.RequestAuditValueDetailRouteMessages,
			Protocol:     service.RequestAuditProtocolAnthropic,
			ClientStatus: 200,
			StartedAt:    now,
		},
	}
}

// GET 默认视图必须只返回信封：类型里没有值字段，且响应禁止缓存。
func TestGetRequestAuditValueDetailReturnsEnvelopeWithoutValues(t *testing.T) {
	now := time.Now().UTC()
	repo := &valueDetailRepoStub{detail: valueDetailStoredDetail(now)}
	router := newValueDetailRouter(t, repo, &valueDetailSettingRepoStub{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/usage/123/request-audit/value-detail", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store, private", recorder.Header().Get("Cache-Control"))
	require.Contains(t, recorder.Body.String(), `"state":"stored"`)
	require.Contains(t, recorder.Body.String(), `"reason":"retained"`)
	require.Contains(t, recorder.Body.String(), `"capability_enabled":false`)
	require.NotContains(t, recorder.Body.String(), "claude-sonnet")
	require.NotContains(t, recorder.Body.String(), "request_headers")
}

// 显式 POST 才返回值，且同样禁止任何中间缓存。
func TestRevealRequestAuditValueDetailReturnsValuesOnlyOnExplicitPost(t *testing.T) {
	now := time.Now().UTC()
	repo := &valueDetailRepoStub{
		detail: valueDetailStoredDetail(now),
		values: service.RequestAuditValueDetailValues{
			Model: "claude-sonnet-4-5",
			Inbound: service.RequestAuditValueDetailInboundValues{
				RequestHeaders: map[string][]string{"Host": {"api.anthropic.com"}},
				DeviceID:       "device-1",
			},
		},
	}
	router := newValueDetailRouter(t, repo, &valueDetailSettingRepoStub{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/usage/123/request-audit/value-detail", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store, private", recorder.Header().Get("Cache-Control"))
	require.Equal(t, "no-cache", recorder.Header().Get("Pragma"))
	require.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))

	// 显式揭示的响应信封恰好两个字段：usage_log_id 与 values（truncated 在 values 里）。
	var envelope struct {
		Code    int                                    `json:"code"`
		Data    *service.RequestAuditValueDetailReveal `json:"data"`
		Message string                                 `json:"message"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.NotNil(t, envelope.Data)
	require.Equal(t, int64(123), envelope.Data.UsageLogID)
	require.Equal(t, "claude-sonnet-4-5", envelope.Data.Values.Model)
	require.Equal(t, []string{"api.anthropic.com"}, envelope.Data.Values.Inbound.RequestHeaders["Host"])
}

// 四种「读不到值」必须是不同的 HTTP 结论。
func TestRevealRequestAuditValueDetailErrorMapping(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name       string
		repo       *valueDetailRepoStub
		wantStatus int
		wantReason string
	}{
		{
			name:       "not-found",
			repo:       &valueDetailRepoStub{},
			wantStatus: http.StatusNotFound,
			wantReason: "REQUEST_AUDIT_VALUE_DETAIL_NOT_FOUND",
		},
		{
			name: "not-retained",
			repo: &valueDetailRepoStub{detail: service.RequestAuditValueDetail{
				UsageLogID: 123, Reason: service.RequestAuditValueDetailSkippedRetentionDisabled, CreatedAt: now,
			}},
			wantStatus: http.StatusConflict,
			wantReason: "REQUEST_AUDIT_VALUE_DETAIL_NOT_RETAINED",
		},
		{
			name: "expired",
			repo: &valueDetailRepoStub{detail: service.RequestAuditValueDetail{
				UsageLogID: 123, Reason: service.RequestAuditValueDetailRetained, Stored: true,
				ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-8 * 24 * time.Hour),
			}},
			wantStatus: http.StatusGone,
			wantReason: "REQUEST_AUDIT_VALUE_DETAIL_GONE",
		},
		{
			name: "purged",
			repo: &valueDetailRepoStub{detail: service.RequestAuditValueDetail{
				UsageLogID: 123, Reason: service.RequestAuditValueDetailRetained, CreatedAt: now.Add(-8 * 24 * time.Hour),
			}},
			wantStatus: http.StatusGone,
			wantReason: "REQUEST_AUDIT_VALUE_DETAIL_GONE",
		},
		{
			name:       "storage-failure",
			repo:       &valueDetailRepoStub{getErr: service.ErrRequestAuditValueDetailUnavailable},
			wantStatus: http.StatusServiceUnavailable,
			wantReason: "REQUEST_AUDIT_VALUE_DETAIL_UNAVAILABLE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newValueDetailRouter(t, tc.repo, &valueDetailSettingRepoStub{})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/usage/123/request-audit/value-detail", nil))
			require.Equal(t, tc.wantStatus, recorder.Code)
			require.Contains(t, recorder.Body.String(), tc.wantReason)
		})
	}
}

// 网关未注入时入口必须是明确的不可用，而不是 panic 或 200。
func TestValueDetailEndpointsAreUnavailableWithoutService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &UsageHandler{}
	router := gin.New()
	router.GET("/usage/:id/request-audit/value-detail", handler.GetRequestAuditValueDetail)
	router.POST("/usage/:id/request-audit/value-detail", handler.RevealRequestAuditValueDetail)
	router.GET("/usage/request-audit-value-detail-settings", handler.GetRequestAuditValueDetailSettings)
	router.PUT("/usage/request-audit-value-detail-settings", handler.UpdateRequestAuditValueDetailSettings)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/usage/1/request-audit/value-detail"},
		{http.MethodPost, "/usage/1/request-audit/value-detail"},
		{http.MethodGet, "/usage/request-audit-value-detail-settings"},
		{http.MethodPut, "/usage/request-audit-value-detail-settings"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code, tc.method+" "+tc.path)
	}
}

func TestValueDetailEndpointsRejectInvalidID(t *testing.T) {
	router := newValueDetailRouter(t, &valueDetailRepoStub{}, &valueDetailSettingRepoStub{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/usage/abc/request-audit/value-detail"},
		{http.MethodPost, "/usage/0/request-audit/value-detail"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	}
}

// 运维开关默认关闭；开启必须逐字确认，且管理员 API Key 不得代替操作员会话。
func TestValueDetailOperatorSettingsEndpoints(t *testing.T) {
	settingsRepo := &valueDetailSettingRepoStub{}
	router := newValueDetailRouter(t, &valueDetailRepoStub{}, settingsRepo)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/usage/request-audit-value-detail-settings", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store, private", recorder.Header().Get("Cache-Control"))
	require.Contains(t, recorder.Body.String(), `"enabled":false`)
	require.Contains(t, recorder.Body.String(), `"capture_allowed":false`)
	require.Contains(t, recorder.Body.String(), `"encryption_key_available":true`)
	require.Contains(t, recorder.Body.String(), `"risk_phrase_en":`)

	// 错误原文不通过。
	recorder = httptest.NewRecorder()
	body, err := json.Marshal(map[string]any{"enabled": true, "phrase": "sure"})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPut, "/usage/request-audit-value-detail-settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "REQUEST_AUDIT_VALUE_DETAIL_RISK_ACK_INVALID")
	require.False(t, service.NewSettingService(settingsRepo, valueDetailConfig(t)).
		RequestAuditValueDetailGate(context.Background()).CaptureAllowed)

	// 机器凭证可以读、可以关，但不能开。
	apiKeyRouter := gin.New()
	apiKeyRouter.Use(func(c *gin.Context) {
		c.Set("auth_method", service.AuditAuthMethodAdminAPIKey)
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 7})
		c.Next()
	})
	apiKeyHandler := &UsageHandler{}
	apiKeyHandler.SetRequestAuditValueDetailService(service.NewRequestAuditValueDetailService(
		&valueDetailRepoStub{}, service.NewSettingService(settingsRepo, valueDetailConfig(t))))
	apiKeyRouter.PUT("/usage/request-audit-value-detail-settings", apiKeyHandler.UpdateRequestAuditValueDetailSettings)

	recorder = httptest.NewRecorder()
	body, err = json.Marshal(map[string]any{
		"enabled": true, "phrase": service.RequestAuditValueDetailRiskAcknowledgementPhraseEN,
	})
	require.NoError(t, err)
	request = httptest.NewRequest(http.MethodPut, "/usage/request-audit-value-detail-settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	apiKeyRouter.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), "REQUEST_AUDIT_VALUE_DETAIL_ADMIN_API_KEY_FORBIDDEN")

	// 正确原文 + 管理员身份：开启成功，且结论与采集侧一致。
	recorder = httptest.NewRecorder()
	body, err = json.Marshal(map[string]any{
		"enabled": true, "language": "en",
		"phrase": service.RequestAuditValueDetailRiskAcknowledgementPhraseEN,
	})
	require.NoError(t, err)
	request = httptest.NewRequest(http.MethodPut, "/usage/request-audit-value-detail-settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"capture_allowed":true`)
	require.True(t, service.NewSettingService(settingsRepo, valueDetailConfig(t)).
		RequestAuditValueDetailGate(context.Background()).CaptureAllowed)

	// 关闭不需要确认或身份。
	recorder = httptest.NewRecorder()
	body, err = json.Marshal(map[string]any{"enabled": false})
	require.NoError(t, err)
	request = httptest.NewRequest(http.MethodPut, "/usage/request-audit-value-detail-settings", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.False(t, service.NewSettingService(settingsRepo, valueDetailConfig(t)).
		RequestAuditValueDetailGate(context.Background()).CaptureAllowed)
}
