//go:build unit

package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// requestTraceAdminSettingsRepo 是管理端 Trace 设置用例的最小设置仓库。
type requestTraceAdminSettingsRepo struct {
	values map[string]string
}

func (r *requestTraceAdminSettingsRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	if value, ok := r.values[key]; ok {
		return &service.Setting{Key: key, Value: value}, nil
	}
	return nil, service.ErrSettingNotFound
}

func (r *requestTraceAdminSettingsRepo) GetValue(ctx context.Context, key string) (string, error) {
	setting, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (r *requestTraceAdminSettingsRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

func (r *requestTraceAdminSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *requestTraceAdminSettingsRepo) SetMultiple(_ context.Context, values map[string]string) error {
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func (r *requestTraceAdminSettingsRepo) GetAll(context.Context) (map[string]string, error) {
	return r.values, nil
}

func (r *requestTraceAdminSettingsRepo) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

type requestTraceAdminProbe struct{}

func (requestTraceAdminProbe) ProbeRequestTraceSupport(context.Context) (service.PlaintextCaptureSupport, error) {
	return service.PlaintextCaptureSupport{Supported: true, Reason: service.PlaintextCaptureSupportReasonSupported}, nil
}

func newRequestTraceAdminHandler() (*SettingHandler, *requestTraceAdminSettingsRepo) {
	repo := &requestTraceAdminSettingsRepo{values: make(map[string]string)}
	svc := service.NewSettingService(repo, &config.Config{})
	svc.SetRequestTraceSupportProbe(requestTraceAdminProbe{})
	return NewSettingHandler(svc, nil, nil, nil, nil, nil, nil), repo
}

// putRequestTraceSettings 走管理端接缝提交一次 Trace 设置（带管理员登录会话）。
func putRequestTraceSettings(t *testing.T, h *SettingHandler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/request-trace", bytes.NewBufferString(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateRequestTraceOperatorSettings(c)
	return w
}

// 票据 05/06：管理端提交"仅指定/排除指定"范围却给出空列表或空白项时，
// 服务端必须拒绝（400），而不是把它们归一化后存成"什么都不采"的静默空转。
func TestRequestTraceOperatorSettingsRejectEmptyOrBlankSelections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	phrase := service.RequestTraceRiskAcknowledgementPhraseEN
	for _, tc := range []struct {
		name, payload string
	}{
		{"model include without models", `{"enabled":true,"language":"en","phrase":"` + phrase + `","scope_provided":true,"all_groups":true,"model_scope":"include","models":[]}`},
		{"model exclude with blank entries", `{"enabled":true,"language":"en","phrase":"` + phrase + `","scope_provided":true,"all_groups":true,"model_scope":"exclude","models":["  ","\t"]}`},
		{"platform include without platforms", `{"enabled":true,"language":"en","phrase":"` + phrase + `","scope_provided":true,"all_groups":true,"model_scope":"all","platform_scope":"include","platforms":[]}`},
		{"platform exclude with blank entries", `{"enabled":true,"language":"en","phrase":"` + phrase + `","scope_provided":true,"all_groups":true,"model_scope":"all","platform_scope":"exclude","platforms":[" "]}`},
		{"selected groups without ids", `{"enabled":true,"language":"en","phrase":"` + phrase + `","scope_provided":true,"all_groups":false,"group_ids":[],"model_scope":"all","platform_scope":"all"}`},
		{"selected groups with non positive id", `{"enabled":true,"language":"en","phrase":"` + phrase + `","scope_provided":true,"all_groups":false,"group_ids":[0,7],"model_scope":"all","platform_scope":"all"}`},
		{"unknown model scope kind", `{"enabled":true,"language":"en","phrase":"` + phrase + `","scope_provided":true,"all_groups":true,"model_scope":"only","models":["gpt-5"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newRequestTraceAdminHandler()
			w := putRequestTraceSettings(t, h, tc.payload)
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.NotContains(t, repo.values, service.SettingKeyRequestTrace,
				"被拒绝的取值范围不得落库")
		})
	}
}

// 合法范围照常保存：全部/所有不需要列表，收窄的范围只要自带可用值就通过。
func TestRequestTraceOperatorSettingsAcceptValidScopes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	phrase := service.RequestTraceRiskAcknowledgementPhraseEN
	h, repo := newRequestTraceAdminHandler()
	w := putRequestTraceSettings(t, h,
		`{"enabled":true,"language":"en","phrase":"`+phrase+`","scope_provided":true,"all_groups":true,"model_scope":"all","platform_scope":"all","models":[],"platforms":[]}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, repo.values, service.SettingKeyRequestTrace)

	w = putRequestTraceSettings(t, h,
		`{"enabled":true,"language":"en","phrase":"`+phrase+`","scope_provided":true,"all_groups":false,"group_ids":[7],"model_scope":"include","models":["gpt-5"],"platform_scope":"exclude","platforms":["anthropic"]}`)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRequestTraceSettingsRejectAPIKeyEnableButAllowDisable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewSettingHandler(nil, nil, nil, nil, nil, nil, nil)
	for _, tc := range []struct {
		name, authMethod, payload string
		wantStatus                int
	}{
		{"api key enable", service.AuditAuthMethodAdminAPIKey, `{"enabled":true}`, http.StatusForbidden},
		{"no auth method enable", "", `{"enabled":true}`, http.StatusForbidden},
		{"api key disable", service.AuditAuthMethodAdminAPIKey, `{"enabled":false}`, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("auth_method", tc.authMethod)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/request-trace", bytes.NewBufferString(tc.payload))
			c.Request.Header.Set("Content-Type", "application/json")
			h.UpdateRequestTraceOperatorSettings(c)
			require.Equal(t, tc.wantStatus, w.Code)
		})
	}
}
