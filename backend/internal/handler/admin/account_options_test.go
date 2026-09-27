//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountOptionsZeroGroupMeansNoFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	h := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/api/v1/admin/accounts/options", h.ListOptions)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/options?group=0", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, svc.lastListAccounts.groupID)
}

func TestAccountOptionsReturnOnlyCandidateFieldsAndFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	svc.accounts = []service.Account{{
		ID: 9, Name: "safe-option", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusDisabled, Credentials: map[string]any{"access_token": "never-export-this"},
		Extra: map[string]any{"private": "never-export-this"},
	}}
	h := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/api/v1/admin/accounts/options", h.ListOptions)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/options?platform=openai&type=oauth&status=inactive&group=21&search=safe&page=1&page_size=20", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "openai", svc.lastListAccounts.platform)
	require.Equal(t, "oauth", svc.lastListAccounts.accountType)
	require.Equal(t, "safe", svc.lastListAccounts.search)
	require.Equal(t, service.StatusInactive, svc.lastListAccounts.status)
	require.EqualValues(t, 21, svc.lastListAccounts.groupID)
	var body struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Data.Items, 1)
	require.Equal(t, map[string]any{
		"id": float64(9), "name": "safe-option", "platform": "openai", "type": "oauth", "status": "disabled",
	}, body.Data.Items[0])
	require.NotContains(t, rec.Body.String(), "never-export-this")
}
