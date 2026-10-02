//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type traceCandidatesAdminServiceStub struct {
	service.AdminService
	models      []string
	err         error
	channelKeys []string
	calls       int
}

func (s *traceCandidatesAdminServiceStub) GetRequestTraceModelCandidates(_ context.Context, channelModelKeys []string) ([]string, error) {
	s.calls++
	s.channelKeys = append([]string(nil), channelModelKeys...)
	return s.models, s.err
}

type traceCandidatesChannelReaderStub struct {
	channels []service.Channel
	err      error
	calls    int
}

func (s *traceCandidatesChannelReaderStub) ListAll(context.Context) ([]service.Channel, error) {
	s.calls++
	return s.channels, s.err
}

// traceCandidatesChannelSourceKeyReaderStub exposes the narrow source-only
// reader and fails the test if the full channel list is loaded instead.
type traceCandidatesChannelSourceKeyReaderStub struct {
	keys           []string
	err            error
	sourceKeyCalls int
	listAllCalls   int
}

func (s *traceCandidatesChannelSourceKeyReaderStub) ListAll(context.Context) ([]service.Channel, error) {
	s.listAllCalls++
	return nil, errors.New("full channel list must not be loaded when the source-only reader exists")
}

func (s *traceCandidatesChannelSourceKeyReaderStub) ListModelMappingSourceKeys(context.Context) ([]string, error) {
	s.sourceKeyCalls++
	return s.keys, s.err
}

func newTraceCandidatesRouter(h *GroupHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/admin/settings/request-trace/model-candidates", h.GetRequestTraceModelCandidates)
	return router
}

func decodeTraceCandidateModels(t *testing.T, body []byte) []string {
	t.Helper()
	var envelope struct {
		Data struct {
			Models []string `json:"models"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	return envelope.Data.Models
}

func TestGroupHandlerGetRequestTraceModelCandidatesAggregatesChannelSources(t *testing.T) {
	svc := &traceCandidatesAdminServiceStub{models: []string{"alpha", "beta"}}
	reader := &traceCandidatesChannelReaderStub{channels: []service.Channel{
		{ID: 1, ModelMapping: map[string]map[string]string{
			"openai": {"channel-src-a": "channel-dst-a"},
		}},
		{ID: 2, ModelMapping: map[string]map[string]string{
			"gemini": {"channel-src-b": "channel-dst-b"},
		}},
	}}
	handler := NewGroupHandlerWithConfigAndChannelReader(svc, nil, nil, nil, reader)
	router := newTraceCandidatesRouter(handler)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/settings/request-trace/model-candidates", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"alpha", "beta"}, decodeTraceCandidateModels(t, rec.Body.Bytes()))
	require.ElementsMatch(t, []string{"channel-src-a", "channel-src-b"}, svc.channelKeys, "only mapping source keys are forwarded")
	require.Equal(t, 1, svc.calls)
	require.Equal(t, 1, reader.calls)
}

func TestGroupHandlerGetRequestTraceModelCandidatesPrefersSourceOnlyChannelReader(t *testing.T) {
	svc := &traceCandidatesAdminServiceStub{models: []string{"alpha"}}
	reader := &traceCandidatesChannelSourceKeyReaderStub{keys: []string{"chan-narrow-a", "chan-narrow-b"}}
	handler := NewGroupHandlerWithConfigAndChannelReader(svc, nil, nil, nil, reader)
	router := newTraceCandidatesRouter(handler)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/settings/request-trace/model-candidates", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.ElementsMatch(t, []string{"chan-narrow-a", "chan-narrow-b"}, svc.channelKeys)
	require.Equal(t, 1, reader.sourceKeyCalls)
	require.Zero(t, reader.listAllCalls, "the full channel list is never loaded")
}

func TestGroupHandlerGetRequestTraceModelCandidatesWithoutChannelReader(t *testing.T) {
	svc := &traceCandidatesAdminServiceStub{models: []string{}}
	handler := NewGroupHandlerWithConfigAndChannelReader(svc, nil, nil, nil, nil)
	router := newTraceCandidatesRouter(handler)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/settings/request-trace/model-candidates", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, svc.channelKeys)
	require.Equal(t, []string{}, decodeTraceCandidateModels(t, rec.Body.Bytes()))
}

func TestGroupHandlerGetRequestTraceModelCandidatesReportsChannelReadFailure(t *testing.T) {
	svc := &traceCandidatesAdminServiceStub{models: []string{"alpha"}}
	reader := &traceCandidatesChannelReaderStub{err: errors.New("channels unavailable")}
	handler := NewGroupHandlerWithConfigAndChannelReader(svc, nil, nil, nil, reader)
	router := newTraceCandidatesRouter(handler)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/settings/request-trace/model-candidates", nil))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, 0, svc.calls, "the aggregation is skipped when the channel read fails")
}

func TestGroupHandlerGetRequestTraceModelCandidatesRejectsMissingProvider(t *testing.T) {
	handler := NewGroupHandler(&stubAdminService{}, nil, nil)
	router := newTraceCandidatesRouter(handler)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/settings/request-trace/model-candidates", nil))

	require.Equal(t, http.StatusNotImplemented, rec.Code)
}
