package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newTestGinContext builds a bare gin.Context backed by an httptest recorder.
func newTestGinContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c
}

// TestRecordCyberPolicyIfMarked_NoMark verifies that when no cyber mark is set,
// the function returns immediately and does NOT set the recorded flag.
func TestRecordCyberPolicyIfMarked_NoMark(t *testing.T) {
	c := newTestGinContext()
	h := &OpenAIGatewayHandler{}

	h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", true, false, nil, service.ChannelUsageFields{}, "")

	// Flag must NOT be set when there was no mark.
	require.False(t, c.GetBool(cyberPolicyRecordedKey),
		"cyberPolicyRecordedKey must remain false when no cyber mark is present")
}

// TestRecordCyberPolicyIfMarked_WithMark verifies that:
//  1. When a cyber mark is present, the recorded flag is set (guard activated).
//  2. A second call is a no-op (idempotent guard).
//  3. Nil services do not panic.
func TestRecordCyberPolicyIfMarked_WithMark(t *testing.T) {
	c := newTestGinContext()
	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{
		Message:        "flagged",
		Body:           `{"error":{"code":"cyber_policy"}}`,
		UpstreamStatus: 400,
	})

	h := &OpenAIGatewayHandler{} // nil services — must not panic

	// First call: should set the flag.
	require.NotPanics(t, func() {
		h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", true, false, nil, service.ChannelUsageFields{}, "")
	})
	require.True(t, c.GetBool(cyberPolicyRecordedKey),
		"cyberPolicyRecordedKey must be true after first call with a mark")

	// Second call: flag already set — must be a no-op (idempotent).
	require.NotPanics(t, func() {
		h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", false, false, nil, service.ChannelUsageFields{}, "")
	})
	// Flag should still be true (not toggled or cleared).
	require.True(t, c.GetBool(cyberPolicyRecordedKey),
		"cyberPolicyRecordedKey must remain true after second call (guard)")
}

// TestRecordCyberPolicyIfMarked_ForwardSuccessSkipsUsageLog verifies the semantic:
// when forwardErrored=false the function still sets the guard flag (mark present),
// but the cyber usage row is NOT requested (only RecordCyberPolicyEvent fires).
// Since services are nil here we only verify the guard flag and no panic.
func TestRecordCyberPolicyIfMarked_ForwardSuccessSkipsUsageLog(t *testing.T) {
	c := newTestGinContext()
	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{
		Message:        "flagged",
		UpstreamStatus: 200,
	})

	h := &OpenAIGatewayHandler{}

	require.NotPanics(t, func() {
		h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", false /* forwardErrored=false */, false, nil, service.ChannelUsageFields{}, "")
	})
	require.True(t, c.GetBool(cyberPolicyRecordedKey))
}

// TestClearCyberPolicyTurnState verifies F1 at the handler level: after a turn
// is finalized, both the mark and the recorded guard are reset so the next WS
// turn detects/records independently.
func TestClearCyberPolicyTurnState(t *testing.T) {
	c := newTestGinContext()
	h := &OpenAIGatewayHandler{}

	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "turn1", UpstreamStatus: 200})
	h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", false, false, nil, service.ChannelUsageFields{}, "")
	require.True(t, c.GetBool(cyberPolicyRecordedKey))

	clearCyberPolicyTurnState(c)
	require.Nil(t, service.GetOpsCyberPolicy(c))
	require.False(t, c.GetBool(cyberPolicyRecordedKey))

	// turn2: a fresh cyber hit must be recordable again.
	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "turn2", UpstreamStatus: 200})
	h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", false, false, nil, service.ChannelUsageFields{}, "")
	require.True(t, c.GetBool(cyberPolicyRecordedKey))
	require.Equal(t, "turn2", service.GetOpsCyberPolicy(c).Message)
}

func TestAdvanceOpenAIWSCyberBlockStateDefersBlockAcrossFailover(t *testing.T) {
	failoverErr := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}

	blocked, pending := advanceOpenAIWSCyberBlockState(false, false, true, failoverErr)
	require.False(t, blocked, "the replacement account must receive the current turn")
	require.True(t, pending, "the cyber hit must still block later turns")

	blocked, pending = advanceOpenAIWSCyberBlockState(blocked, pending, false, failoverErr)
	require.False(t, blocked, "additional failover attempts must remain eligible")
	require.True(t, pending)

	blocked, pending = advanceOpenAIWSCyberBlockState(blocked, pending, false, nil)
	require.True(t, blocked, "the next client turn must be blocked after failover finishes")
	require.False(t, pending)
}

func TestClearCyberPolicyAttemptStatePreservesRecordedGuardDuringFailover(t *testing.T) {
	c := newTestGinContext()
	h := &OpenAIGatewayHandler{}

	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "account-a", UpstreamStatus: http.StatusOK})
	h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", true, false, nil, service.ChannelUsageFields{}, "")
	require.True(t, c.GetBool(cyberPolicyRecordedKey))

	clearCyberPolicyAttemptState(c, false)
	require.Nil(t, service.GetOpsCyberPolicy(c))
	require.True(t, c.GetBool(cyberPolicyRecordedKey), "the same logical turn must not record again after failover")

	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "account-b", UpstreamStatus: http.StatusOK})
	h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", true, false, nil, service.ChannelUsageFields{}, "")
	require.Equal(t, "account-b", service.GetOpsCyberPolicy(c).Message)
	require.True(t, c.GetBool(cyberPolicyRecordedKey))

	clearCyberPolicyAttemptState(c, true)
	require.Nil(t, service.GetOpsCyberPolicy(c))
	require.False(t, c.GetBool(cyberPolicyRecordedKey), "a completed logical turn must reset the guard")
}

// TestBuildCyberSessionBlockedOpsEntry verifies the locally-rejected request is
// auditable: 403 / phase=request / type=cyber_policy_session_blocked — distinct
// from upstream cyber_policy hits, and it must NOT touch moderation/violation.
func TestBuildCyberSessionBlockedOpsEntry(t *testing.T) {
	entry := buildCyberSessionBlockedOpsEntry(cyberPolicyOpsErrorMeta{
		RequestID: "req-9", Model: "gpt-5", RequestPath: "/openai/v1/responses",
	})
	require.Equal(t, 403, entry.StatusCode)
	require.Equal(t, "cyber_policy_session_blocked", entry.ErrorType)
	require.Equal(t, "request", entry.ErrorPhase)
	require.True(t, entry.IsBusinessLimited)
	require.Equal(t, "gateway_local", entry.ErrorSource)
	require.Equal(t, "platform", entry.ErrorOwner)
	require.Empty(t, entry.ErrorBody, "no session block key → ErrorBody must be empty")

	entryWithKey := buildCyberSessionBlockedOpsEntry(cyberPolicyOpsErrorMeta{
		RequestID: "req-9", Model: "gpt-5", RequestPath: "/openai/v1/responses",
		SessionBlockKey: "abc123",
	})
	require.Equal(t, "session_block_key=abc123", entryWithKey.ErrorBody)
}

// TestRejectIfCyberSessionBlocked_FailOpen verifies fail-open paths: nil handler
// services, no explicit session signal, and (implicitly) disabled switch all
// pass the request through.
func TestRejectIfCyberSessionBlocked_FailOpen(t *testing.T) {
	c := newTestGinContext()
	c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(`{}`))

	h := &OpenAIGatewayHandler{}
	require.False(t, h.rejectIfCyberSessionBlocked(c, nil, []byte(`{}`), "gpt-5", cyberBlockFormatResponses), "nil apiKey → pass")

	h2 := &OpenAIGatewayHandler{gatewayService: nil}
	key := &service.APIKey{ID: 1}
	require.False(t, h2.rejectIfCyberSessionBlocked(c, key, []byte(`{}`), "gpt-5", cyberBlockFormatResponses), "nil gateway service → pass")
}

func TestBuildCyberSessionBlockWritePlanCombinesExplicitAndTranscriptKeys(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`)
	c := newTestGinContext()
	c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(string(body)))
	c.Request.RemoteAddr = "203.0.113.44:12345"
	c.Request.Header.Set("User-Agent", "client/1.2.3")

	plan := buildCyberSessionBlockWritePlan(7, c, body)
	require.Len(t, plan.keys, 2)
	require.NotEmpty(t, plan.scopeKey)

	c.Request.Header.Set("session_id", "sess-explicit")
	plan = buildCyberSessionBlockWritePlan(7, c, body)
	require.Len(t, plan.keys, 3)
	require.NotEmpty(t, plan.scopeKey)
}

// TestRecordCyberPolicyIfMarked_BlockKeyPlumbed verifies the 6th param is
// accepted and a non-empty key with nil gateway service does not panic
// (write-side guards live in the service layer).
func TestRecordCyberPolicyIfMarked_BlockKeyPlumbed(t *testing.T) {
	c := newTestGinContext()
	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{Message: "x", UpstreamStatus: 400})
	h := &OpenAIGatewayHandler{}
	require.NotPanics(t, func() {
		h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", true, false, []byte(`{"input":"deadbeef"}`), service.ChannelUsageFields{}, "")
	})
}

// TestCyberPolicyUsageAuditCapturesSanitizedWireAttempts 验证 cyber 审计只取传输尝试事实：
// 账号/协议/状态保留，模型名与凭据原文、自由文本头值都不进审计。
func TestCyberPolicyUsageAuditCapturesSanitizedWireAttempts(t *testing.T) {
	c := newTestGinContext()
	c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(`{}`))
	c.Request.Header.Set("Authorization", "Bearer sk-cyber-secret")
	c.Request.Header.Set("User-Agent", "codex/1.2.3")

	counterCtx := service.WithRequestAuditHTTPAttemptCounter(context.Background(), c)
	attemptCtx := httpattempt.WithMetadata(counterCtx, httpattempt.Metadata{
		AccountID: 3, Model: "gpt-5.1", Protocol: service.RequestAuditProtocolOpenAIResp,
	})
	attempt := httpattempt.StartAttempt(attemptCtx)
	require.NotNil(t, attempt)
	attempt.SetResponse(http.StatusBadRequest, http.Header{"Content-Type": []string{"application/json"}}, true)

	attempts, headers := cyberPolicyUsageAudit(c)

	require.Len(t, attempts, 1)
	require.Equal(t, int64(3), attempts[0].AccountID)
	require.Equal(t, service.RequestAuditProtocolOpenAIResp, attempts[0].Protocol)
	require.Equal(t, service.RequestAuditStageWire, attempts[0].Stage)
	require.Empty(t, attempts[0].Model, "审计尝试不落模型名")
	require.NotNil(t, attempts[0].UpstreamStatus)
	require.Equal(t, http.StatusBadRequest, *attempts[0].UpstreamStatus)
	require.Equal(t, []string{"present"}, headers["Authorization"], "凭据头只记存在")
	require.Equal(t, []string{"present"}, headers["User-Agent"], "自由文本头只记存在")

	encoded, err := json.Marshal(map[string]any{"attempts": attempts, "headers": headers})
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, "sk-cyber-secret", "审计不得出现凭据原文")
	require.NotContains(t, dump, "codex/1.2.3", "审计不得出现头值原文")
	require.NotContains(t, dump, "gpt-5.1", "审计不得出现模型名")
}

// TestCyberPolicyUsageAuditEmptyWithoutWireAttempts 验证未覆盖传输（WS/插件无传输尝试元数据）
// 不伪造尝试：由服务层标未采集，而不是给出空尝试列表冒充已采集。
func TestCyberPolicyUsageAuditEmptyWithoutWireAttempts(t *testing.T) {
	attempts, headers := cyberPolicyUsageAudit(nil)
	require.Empty(t, attempts)
	require.Empty(t, headers)

	c := newTestGinContext()
	c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(`{}`))
	c.Request.Header.Set("Authorization", "Bearer sk-cyber-secret")

	attempts, headers = cyberPolicyUsageAudit(c)
	require.Empty(t, attempts)
	require.Empty(t, headers)
}

type cyberPolicyUsageLogRepoStub struct {
	service.UsageLogRepository

	mu    sync.Mutex
	calls int
	last  *service.UsageLog
}

func (s *cyberPolicyUsageLogRepoStub) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if log != nil && log.ID == 0 {
		log.ID = 42
	}
	s.last = log
	return true, nil
}

func (s *cyberPolicyUsageLogRepoStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newCyberPolicyHandlerForUsageTest(usageRepo service.UsageLogRepository) *OpenAIGatewayHandler {
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gateway := service.NewOpenAIGatewayService(
		nil, usageRepo, nil, nil, nil, nil, nil, cfg,
		nil, nil, service.NewBillingService(cfg, nil), nil, nil, nil, &service.DeferredService{},
		nil, nil, nil, nil, nil, nil, nil,
	)
	return NewOpenAIGatewayHandler(gateway, nil, nil, nil, nil, nil, nil, nil, cfg)
}

// TestRecordCyberPolicyIfMarked_CyberUsageOnlyWhenNormalSubmitterDoesNot 守护去重边界：
// result 为 nil 的 cyber 透传路径由 cyber 侧落用量（因此有审计）；result 非 nil 时正常
// submitter 拥有同一条使用记录与审计，cyber 侧不得再写（否则 usage 行同一、审计行唯一约束冲突，
// 还会把更完整的记录换成局部记录）。
func TestRecordCyberPolicyIfMarked_CyberUsageOnlyWhenNormalSubmitterDoesNot(t *testing.T) {
	usageRepo := &cyberPolicyUsageLogRepoStub{}
	h := newCyberPolicyHandlerForUsageTest(usageRepo)
	apiKey := &service.APIKey{ID: 2, User: &service.User{ID: 1}}
	account := &service.Account{ID: 3}

	c := newTestGinContext()
	c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(`{}`))
	service.MarkOpsCyberPolicy(c, service.CyberPolicyMark{
		Code: "cyber_policy", Message: "blocked", UpstreamStatus: http.StatusBadRequest,
	})
	h.recordCyberPolicyIfMarked(c, apiKey, account, nil, "gpt-5", true, false, nil, service.ChannelUsageFields{}, "")

	require.Eventually(t, func() bool { return usageRepo.callCount() == 1 }, 3*time.Second, 10*time.Millisecond,
		"result=nil 的 cyber 透传路径必须由 cyber 侧落用量，否则审计缺失")

	c2 := newTestGinContext()
	c2.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(`{}`))
	service.MarkOpsCyberPolicy(c2, service.CyberPolicyMark{
		Code: "cyber_policy", Message: "blocked", UpstreamStatus: http.StatusOK,
	})
	h.recordCyberPolicyIfMarked(c2, apiKey, account, nil, "gpt-5", true, true, nil, service.ChannelUsageFields{}, "")

	require.Never(t, func() bool { return usageRepo.callCount() != 1 }, 300*time.Millisecond, 20*time.Millisecond,
		"result 非 nil 时正常 submitter 拥有 usage，cyber 侧不得双写")
	require.True(t, c2.GetBool(cyberPolicyRecordedKey), "风控记录标记仍须置位，避免重复记录")
}

// TestBuildCyberPolicyOpsErrorEntry_StatusCode verifies F6: the ops error log
// records the status the codex client actually received (400 non-stream / 200 stream),
// not a hardcoded 403.
func TestBuildCyberPolicyOpsErrorEntry_StatusCode(t *testing.T) {
	for _, tc := range []struct {
		name           string
		upstreamStatus int
	}{
		{"non_stream_400", 400},
		{"stream_200", 200},
		{"zero_value", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mark := &service.CyberPolicyMark{
				Code:           "cyber_policy",
				Message:        "blocked",
				UpstreamStatus: tc.upstreamStatus,
			}
			entry := buildCyberPolicyOpsErrorEntry(cyberPolicyOpsErrorMeta{
				RequestID: "req-1", Model: "gpt-5", RequestPath: "/openai/v1/responses",
			}, mark)
			require.Equal(t, tc.upstreamStatus, entry.StatusCode)
			require.Equal(t, "cyber_policy", entry.ErrorType)
			require.Equal(t, "request", entry.ErrorPhase)
		})
	}
}
