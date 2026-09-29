package handler

import (
	"context"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	claude429CooldownMessage = "This Claude session is temporarily cooled down by the local 429 account limit; please retry later"
	claude429CooldownCode    = "claude_429_account_limit_cooldown"
)

type claude429CooldownRequest struct {
	gate      *service.Claude429CooldownGate
	runtime   service.RateLimit429AccountLimitCooldown
	apiKeyID  int64
	deviceID  string
	sessionID string
}

// captureClaude429Cooldown only considers authenticated requests with a complete,
// matching identity from the inbound body and Claude session header.
func captureClaude429Cooldown(c *gin.Context, apiKeyID int64, body []byte, gate *service.Claude429CooldownGate) claude429CooldownRequest {
	if gate == nil || c == nil || c.Request == nil || apiKeyID <= 0 {
		return claude429CooldownRequest{}
	}
	runtime := gate.Runtime(c.Request.Context())
	if !runtime.Enabled {
		return claude429CooldownRequest{}
	}
	raw := gjson.GetBytes(body, "metadata.user_id")
	if raw.Type != gjson.String {
		requestLogger(c, "handler.claude_429_cooldown").Debug("gateway.claude_429_cooldown_identity_skipped", zap.String("reason", "missing_metadata"))
		return claude429CooldownRequest{}
	}
	parsed := service.ParseMetadataUserID(raw.String())
	if parsed == nil {
		requestLogger(c, "handler.claude_429_cooldown").Debug("gateway.claude_429_cooldown_identity_skipped", zap.String("reason", "invalid_metadata"))
		return claude429CooldownRequest{}
	}
	// The sanitized accessor is already used elsewhere to select the Claude
	// session header. Require exactly one raw header and compare it too: whitespace
	// or conflicting duplicate values must not alias another client's scope.
	headerValues := c.Request.Header.Values("X-Claude-Code-Session-Id")
	headerSession := service.ClaudeCodeSessionIDFromHeader(c)
	if len(headerValues) == 0 {
		requestLogger(c, "handler.claude_429_cooldown").Debug("gateway.claude_429_cooldown_identity_skipped", zap.String("reason", "missing_header"))
		return claude429CooldownRequest{}
	}
	if len(headerValues) != 1 || headerSession == "" || headerValues[0] != headerSession || headerSession != parsed.SessionID {
		requestLogger(c, "handler.claude_429_cooldown").Debug("gateway.claude_429_cooldown_identity_skipped", zap.String("reason", "invalid_or_mismatched_header"))
		return claude429CooldownRequest{}
	}
	return claude429CooldownRequest{gate: gate, runtime: runtime, apiKeyID: apiKeyID, deviceID: parsed.DeviceID, sessionID: parsed.SessionID}
}

func (r claude429CooldownRequest) retryAfter(ctx context.Context) (int, bool) {
	if r.gate == nil || ctx == nil || ctx.Err() != nil {
		return 0, false
	}
	return r.gate.Remaining(ctx, r.apiKeyID, r.runtime, r.deviceID, r.sessionID)
}

func (r claude429CooldownRequest) markIfCapReached(c *gin.Context, reached bool) {
	// Slot-wait SSE pings can commit HTTP 200 without model output. Callers
	// already guard forward-produced bytes; allow heartbeat-only responses to
	// record a genuine N cap, but never write after semantic output or cancel.
	if !reached || c == nil || c.Request == nil || r.gate == nil || c.Request.Context().Err() != nil || (c.Writer.Written() && !gatewayStreamHasOnlyHeartbeats(c)) {
		return
	}
	if r.gate.Mark(c.Request.Context(), r.apiKeyID, r.runtime, r.deviceID, r.sessionID) {
		requestLogger(c, "handler.claude_429_cooldown").Info("gateway.claude_429_cooldown_written",
			zap.String("scope", r.runtime.Scope))
	}
}

func (r claude429CooldownRequest) prepareLocalResponse(c *gin.Context, seconds int) {
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
	c.Header("Retry-After", strconv.Itoa(seconds))
	requestLogger(c, "handler.claude_429_cooldown").Info("gateway.claude_429_cooldown_hit",
		zap.String("scope", r.runtime.Scope))
}
