package httpattempt

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIValueHeadersExcludeCredentialsAndUnknownNames(t *testing.T) {
	request := http.Header{
		"Content-Type":      {"application/json"},
		"Accept":            {"text/event-stream"},
		"Authorization":     {"Bearer private-canary"},
		"Cookie":            {"session=private-canary"},
		"X-Unknown-Private": {"private-canary"},
	}
	values, omission, supported := SanitizeProtocolRequestHeaderValues("openai.responses", request)
	require.True(t, supported)
	require.Equal(t, "application/json", values["Content-Type"])
	require.NotContains(t, values, "Cookie")
	require.NotContains(t, values, "Authorization")
	require.NotContains(t, values, "X-Unknown-Private")
	require.True(t, omission.Any())

	response := http.Header{"Content-Type": {"application/json"}, "Retry-After": {"2"}, "Set-Cookie": {"secret"}}
	result, _, supported := SanitizeProtocolResponseHeaderValues("openai.responses", response)
	require.True(t, supported)
	require.Equal(t, "2", result["Retry-After"])
	require.NotContains(t, result, "Set-Cookie")
}

func TestValueHeadersRejectUnknownWireProtocol(t *testing.T) {
	values, _, supported := SanitizeProtocolRequestHeaderValues("bedrock", http.Header{"Content-Type": {"application/json"}})
	require.False(t, supported)
	require.Empty(t, values)
}
