//go:build unit

package service

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBedrockValueProtocolIsUnsupportedWithoutChangingAuditProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	counter := httpattempt.NewCounter()
	c.Set(requestAuditHTTPAttemptCounterKey, counter)
	ctx := httpattempt.WithClaudeHeaderValueCapture(t.Context(), true)
	ctx = WithRequestAuditValueWireProtocolOverride(ctx, "bedrock")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://bedrock.example/converse", bytes.NewReader(nil))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer PRIVATE_TOKEN")
	req.Header.Set("X-Amz-Security-Token", "PRIVATE_TOKEN")
	req = bindRequestAuditHTTPAttempt(req, c, 5, "model", RequestAuditProtocolAnthropic)
	attempt := httpattempt.StartRequestAttempt(req)
	attempt.SetResponse(429, http.Header{"X-Amzn-Requestid": {"test-request-id"}}, false)
	snapshot := counter.Metadata()
	require.Len(t, snapshot, 1)
	require.Equal(t, RequestAuditProtocolAnthropic, snapshot[0].Protocol)
	require.Equal(t, "bedrock", RequestAuditValueWireProtocolForMetadata(snapshot[0]))
	require.Empty(t, snapshot[0].RequestHeaderValues)
	require.Empty(t, snapshot[0].ResponseHeaderValues)

	write := BuildRequestAuditValueDetailWrite(RequestAuditValueDetailInput{
		Route: "/v1/messages", Protocol: RequestAuditProtocolAnthropic, Model: "model",
		Attempts: RequestAuditValueDetailAttemptsFromHTTPMetadata(snapshot),
	}, RequestAuditValueDetailGate{CaptureAllowed: true}, time.Now().UTC())
	require.NotNil(t, write)
	require.Equal(t, RequestAuditValueDetailSkippedUnsupportedProtocol, write.Reason)
	require.Empty(t, write.Payload)
}
