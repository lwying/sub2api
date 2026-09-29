//go:build unit

package handler

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestClassifyRequestTraceRouteAuditedHTTPEntrypoints(t *testing.T) {
	for _, tc := range []struct {
		path     string
		family   service.RequestTraceRouteFamily
		endpoint string
	}{
		{httpPathMessages, service.RequestTraceMessages, httpPathMessages},
		{"/v1/chat/completions", service.RequestTraceChatCompletions, "/v1/chat/completions"},
		{"/chat/completions", service.RequestTraceChatCompletions, "/v1/chat/completions"},
		{"/v1/responses", service.RequestTraceResponses, "/v1/responses"},
		{"/responses", service.RequestTraceResponses, "/v1/responses"},
		{"/backend-api/codex/responses", service.RequestTraceResponses, "/v1/responses"},
		{"/v1/responses/compact", service.RequestTraceResponses, "/v1/responses/compact"},
		{"/responses/compact", service.RequestTraceResponses, "/v1/responses/compact"},
		{"/backend-api/codex/responses/compact", service.RequestTraceResponses, "/v1/responses/compact"},
		{"/v1/responses/compact/detail", service.RequestTraceResponses, "/v1/responses/compact"},
		{"/responses/compact/detail", service.RequestTraceResponses, "/v1/responses/compact"},
		{"/backend-api/codex/responses/compact/detail", service.RequestTraceResponses, "/v1/responses/compact"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			family, endpoint, ok := ClassifyRequestTraceRoute(http.MethodPost, tc.path)
			require.True(t, ok, "candidate route should be classified without fabricating a wire attempt")
			require.Equal(t, tc.family, family)
			require.Equal(t, tc.endpoint, endpoint)
		})
	}
}

func TestClassifyRequestTraceRouteDoesNotPromoteUnobservedOrUnsafePaths(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/responses"}, // Responses WebSocket is not HTTP request audit.
		{http.MethodGet, "/responses"},
		{http.MethodGet, "/backend-api/codex/responses"},
		{http.MethodPost, "/v1/messages/count_tokens"},
		{http.MethodPost, "/messages/count_tokens"},
		{http.MethodPost, "/v1/responses/input_tokens"},
		{http.MethodPost, "/responses/input_tokens"},
		{http.MethodPost, "/backend-api/codex/responses/input_tokens"},
		{http.MethodPost, "/v1beta/models/gemini:generateContent"},
		{http.MethodPost, "/antigravity/v1/messages"},
		{http.MethodPost, "/openai/v1/responses"},         // Not registered by gateway route table.
		{http.MethodPost, "/v1/responses/my-response-id"}, // Forwardable, but not audited matrix-proven.
		{http.MethodPost, "/responses/other/compact"},
		{http.MethodPost, "/v1/responses/compacted"},
		{http.MethodPost, "/v1/responses/compactness"},
		{http.MethodPost, "/v1/responses/compact/../detail"},
		{http.MethodPost, "/responses/compact/a?api_key=x"},
		{http.MethodPost, "/responses/compact//detail"},
		{http.MethodPost, "/responses/compact/"},
		{http.MethodPost, "/responses/compact/こんにちは"},
		{http.MethodPost, "/responses/compact/a;b"},
		{http.MethodPost, "/responses/compact/abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh_abcdefgh"},
		{http.MethodPut, "/v1/messages"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			family, endpoint, ok := ClassifyRequestTraceRoute(tc.method, tc.path)
			require.False(t, ok)
			require.Empty(t, family)
			require.Empty(t, endpoint)
		})
	}
}

const httpPathMessages = "/v1/messages"
