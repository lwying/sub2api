package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	claudeFingerprintTestSeed  = "11111111-1111-4111-8111-111111111111"
	claudeFingerprintTestSeed2 = "22222222-2222-4222-8222-222222222222"
	// 64 位十六进制，与真实 Claude Code device_id 形态一致
	claudeFingerprintClientDeviceID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	claudeFingerprintClientSession  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	claudeFingerprintClientAccount  = "99999999-8888-4777-8666-555555555555"
)

func newTestClaudeOAuthAccount(id int64, extra map[string]any) *Account {
	merged := map[string]any{}
	for k, v := range extra {
		merged[k] = v
	}
	return &Account{
		ID:       id,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    merged,
	}
}

// claudeFingerprintClientBody 构造带真实形态 metadata.user_id 的 /v1/messages 请求体。
func claudeFingerprintClientBody(deviceID, accountUUID, sessionID string) []byte {
	userID := `{"device_id":"` + deviceID + `","account_uuid":"` + accountUUID + `","session_id":"` + sessionID + `"}`
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	next, ok := setJSONValueBytes(body, "metadata.user_id", userID)
	if !ok {
		panic("test fixture: failed to set metadata.user_id")
	}
	return next
}

func newClaudeFingerprintTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

func newClaudeFingerprintTestService() *GatewayService {
	return &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
		},
	}
}

// --- 模式门控：收敛是显式 opt-in ---

func TestGetClaudeFingerprintMode(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected claudeFingerprintMode
	}{
		{"nil account", nil, claudeFingerprintOff},
		{"missing key defaults off", newTestClaudeOAuthAccount(1, nil), claudeFingerprintOff},
		{"blank defaults off", newTestClaudeOAuthAccount(1, map[string]any{"claude_fingerprint_mode": "  "}), claudeFingerprintOff},
		{"invalid defaults off", newTestClaudeOAuthAccount(1, map[string]any{"claude_fingerprint_mode": "bogus"}), claudeFingerprintOff},
		{"explicit off", newTestClaudeOAuthAccount(1, map[string]any{"claude_fingerprint_mode": "off"}), claudeFingerprintOff},
		{"device", newTestClaudeOAuthAccount(1, map[string]any{"claude_fingerprint_mode": "device"}), claudeFingerprintDevice},
		{"session", newTestClaudeOAuthAccount(1, map[string]any{"claude_fingerprint_mode": "session"}), claudeFingerprintSession},
		{"full", newTestClaudeOAuthAccount(1, map[string]any{"claude_fingerprint_mode": "full"}), claudeFingerprintFull},
		{"api key account is off", &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{"claude_fingerprint_mode": "full"}}, claudeFingerprintOff},
		{"openai account is off", &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"claude_fingerprint_mode": "full"}}, claudeFingerprintOff},
		{"setup token is eligible", &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Extra: map[string]any{"claude_fingerprint_mode": "device"}}, claudeFingerprintDevice},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, tc.account.GetClaudeFingerprintMode())
		})
	}
}

// --- 确定性派生 ---

func TestDeriveStableClaudeDeviceID_DeterministicHex64(t *testing.T) {
	first := deriveStableClaudeDeviceID("seed-a")
	require.Equal(t, first, deriveStableClaudeDeviceID("seed-a"))
	require.NotEqual(t, first, deriveStableClaudeDeviceID("seed-b"))
	require.Len(t, first, 64, "device_id 必须是 32 字节随机数的 hex 编码")
	require.Regexp(t, "^[0-9a-f]{64}$", first)
}

func TestDeriveStableClaudeSessionID_PerClientSession(t *testing.T) {
	derived := deriveStableClaudeSessionID(claudeFingerprintTestSeed, claudeFingerprintClientSession)
	require.Equal(t, derived, deriveStableClaudeSessionID(claudeFingerprintTestSeed, claudeFingerprintClientSession),
		"同一 seed + 同一客户端会话必须稳定")
	require.NotEqual(t, derived, deriveStableClaudeSessionID(claudeFingerprintTestSeed, claudeFingerprintClientSession+"x"),
		"不同客户端会话必须派生出不同的 session_id")
	require.NotEqual(t, derived, deriveStableClaudeSessionID(claudeFingerprintTestSeed2, claudeFingerprintClientSession),
		"不同 seed 必须派生出不同的 session_id")
	require.Regexp(t, "^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$", derived,
		"派生结果必须是合法 UUIDv4 形态")

	// 无客户端会话时派生账号级恒定值，且必须非空（full 模式依赖它）
	accountLevel := deriveStableClaudeSessionID(claudeFingerprintTestSeed, "")
	require.NotEmpty(t, accountLevel)
	require.Equal(t, accountLevel, deriveStableClaudeSessionID(claudeFingerprintTestSeed, ""))
	require.NotEqual(t, accountLevel, derived)
	require.Empty(t, deriveStableClaudeSessionID("", claudeFingerprintClientSession))
}

// --- ID 集合解析 ---

func TestResolveClaudeFingerprintIDs_Modes(t *testing.T) {
	account := newTestClaudeOAuthAccount(7001, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})

	require.Nil(t, resolveClaudeFingerprintIDs(account, "s", claudeFingerprintOff), "off 模式必须返回 nil")

	device := resolveClaudeFingerprintIDs(account, claudeFingerprintClientSession, claudeFingerprintDevice)
	require.NotNil(t, device)
	require.NotEmpty(t, device.deviceID)
	require.Empty(t, device.sessionID, "device 模式只收敛 device_id")
	require.Empty(t, device.accountUUID)

	session := resolveClaudeFingerprintIDs(account, claudeFingerprintClientSession, claudeFingerprintSession)
	require.NotNil(t, session)
	require.NotEmpty(t, session.deviceID)
	require.NotEmpty(t, session.sessionID)
	require.Equal(t, session.deviceID, device.deviceID, "device_id 是账号级恒定值，模式间不得漂移")

	full := resolveClaudeFingerprintIDs(account, claudeFingerprintClientSession, claudeFingerprintFull)
	require.NotNil(t, full)
	require.Equal(t, full.sessionID, deriveStableClaudeSessionID(claudeFingerprintTestSeed, ""),
		"full 模式的 session_id 与客户端会话无关")
	require.NotEqual(t, full.sessionID, session.sessionID)

	// 缺 seed 时一律不收敛，避免把账号身份写成一个可预测的常量
	seedless := newTestClaudeOAuthAccount(7002, map[string]any{"claude_fingerprint_mode": "full"})
	require.Nil(t, resolveClaudeFingerprintIDs(seedless, "s", claudeFingerprintFull))
	require.Nil(t, resolveClaudeFingerprintIDsFromRawBody(seedless, claudeFingerprintClientBody(claudeFingerprintClientDeviceID, "", claudeFingerprintClientSession)))
}

func TestResolveClaudeFingerprintIDsFromRawBody_ReadsClientSessionFromBody(t *testing.T) {
	account := newTestClaudeOAuthAccount(7003, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})

	ids := resolveClaudeFingerprintIDsFromRawBody(account, claudeFingerprintClientBody(
		claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession))
	require.NotNil(t, ids)
	require.Equal(t, deriveStableClaudeSessionID(claudeFingerprintTestSeed, claudeFingerprintClientSession), ids.sessionID)

	// 无 metadata 的请求体不得让会话派生退化成账号级常量
	require.Nil(t, resolveClaudeFingerprintIDsFromRawBody(account, []byte(`{"model":"claude-sonnet-5"}`)))
	require.Nil(t, resolveClaudeFingerprintIDsFromRawBody(account, nil))
}

// --- 接线核心：四条出站路径共用的暂存入口 ---

func TestStageClaudeFingerprintForBody_ConvergesBodyAndStagesSharedIDs(t *testing.T) {
	c := newClaudeFingerprintTestContext(t)
	account := newTestClaudeOAuthAccount(7600, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
		"account_uuid":            "real-account-uuid",
	})
	body := claudeFingerprintClientBody(claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession)

	next := stageClaudeFingerprintForBody(c, account, body)

	converged := parseClaudeUserIDJSON(gjson.GetBytes(next, "metadata.user_id").String())
	require.NotNil(t, converged)
	require.Equal(t, "real-account-uuid", converged["account_uuid"], "account_uuid 必须取自账号真实元数据")
	require.NotEqual(t, claudeFingerprintClientSession, converged["session_id"], "客户端会话必须被收敛")

	// 暂存的必须是同一份 IDs：出站头与请求体由此保持一致
	staged := stagedClaudeFingerprintIDs(c, account)
	require.NotNil(t, staged)
	require.Equal(t, converged["session_id"], staged.sessionID)

	// 同一客户端会话必须每次都派生出同一个 session_id（会话模式的核心不变量）：
	// 这才是「反复看到同一会话」的保证，而不是「对已收敛 body 再收敛一次不动」。
	// 本函数按约定每个 attempt 只对客户端原始 body 调用一次（handler 侧
	// parsedReq.CloneForBody 每次 attempt 都从原始 body 重新克隆），
	// 已收敛 body 里 session_id 已被改写，再跑一次会得到第二级派生值。
	replayed := stageClaudeFingerprintForBody(newClaudeFingerprintTestContext(t), account, body)
	replayedIDs := parseClaudeUserIDJSON(gjson.GetBytes(replayed, "metadata.user_id").String())
	require.Equal(t, converged["session_id"], replayedIDs["session_id"])
}

func TestStageClaudeFingerprintForBody_NoMetadataUserIDLeavesBodyUntouched(t *testing.T) {
	c := newClaudeFingerprintTestContext(t)
	account := newTestClaudeOAuthAccount(7601, map[string]any{
		"claude_fingerprint_mode": "full",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})
	body := []byte(`{"model":"claude-sonnet-5","messages":[]}`)

	require.Equal(t, string(body), string(stageClaudeFingerprintForBody(c, account, body)))
	require.Nil(t, stagedClaudeFingerprintIDs(c, account), "没有可收敛的客户端身份时不得暂存 ID")
}

func TestStageClaudeFingerprintForBody_ClearsStaleIDsOnOffAccount(t *testing.T) {
	c := newClaudeFingerprintTestContext(t)
	converging := newTestClaudeOAuthAccount(7602, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})
	body := claudeFingerprintClientBody(claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession)
	stageClaudeFingerprintForBody(c, converging, body)
	require.NotNil(t, stagedClaudeFingerprintIDs(c, converging))

	// failover：本次改由 off 账号服务，上一账号的 IDs 不得残留
	offAccount := newTestClaudeOAuthAccount(7603, map[string]any{"claude_fingerprint_mode": "off"})
	require.Equal(t, string(body), string(stageClaudeFingerprintForBody(c, offAccount, body)))
	require.Nil(t, stagedClaudeFingerprintIDs(c, converging), "残留 ID 会被误应用到新账号的出站头")
}

func TestStageClaudeFingerprintForBody_RequiresCanonicalSeed(t *testing.T) {
	c := newClaudeFingerprintTestContext(t)
	account := newTestClaudeOAuthAccount(7604, map[string]any{
		"claude_fingerprint_mode": "full",
		"claude_fingerprint_seed": "not-a-uuid",
	})
	body := claudeFingerprintClientBody(claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession)

	require.Equal(t, string(body), string(stageClaudeFingerprintForBody(c, account, body)),
		"非法 seed 不得让客户端身份被写成可预测的常量")
	require.Nil(t, stagedClaudeFingerprintIDs(c, account))
}

// --- user_id JSON 改写 ---

func TestApplyClaudeFingerprintToUserIDJSON_ModeSemantics(t *testing.T) {
	account := newTestClaudeOAuthAccount(7004, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
		"account_uuid":            "real-account-uuid",
	})
	ids := resolveClaudeFingerprintIDs(account, claudeFingerprintClientSession, claudeFingerprintSession)
	require.NotNil(t, ids)

	metadata := map[string]any{"user_id": `{"device_id":"` + claudeFingerprintClientDeviceID +
		`","account_uuid":"` + claudeFingerprintClientAccount +
		`","session_id":"` + claudeFingerprintClientSession + `"}`}

	require.True(t, applyClaudeFingerprintToUserIDJSON(metadata, ids))
	convergedRaw, ok := metadata["user_id"].(string)
	require.True(t, ok)
	converged := parseClaudeUserIDJSON(convergedRaw)
	require.Equal(t, ids.deviceID, converged["device_id"], "device_id 收敛为账号级恒定值")
	require.Equal(t, "real-account-uuid", converged["account_uuid"], "session 模式收敛 account_uuid 为账号真实值")
	require.Equal(t, ids.sessionID, converged["session_id"])

	// 幂等：已经收敛的 user_id 再跑一次不得报告变更（否则每轮都会重写 body）
	require.False(t, applyClaudeFingerprintToUserIDJSON(metadata, ids))

	// device 模式不得触碰 session_id / account_uuid
	deviceIDs := resolveClaudeFingerprintIDs(account, claudeFingerprintClientSession, claudeFingerprintDevice)
	deviceMetadata := map[string]any{"user_id": `{"device_id":"` + claudeFingerprintClientDeviceID +
		`","account_uuid":"` + claudeFingerprintClientAccount +
		`","session_id":"` + claudeFingerprintClientSession + `"}`}
	require.True(t, applyClaudeFingerprintToUserIDJSON(deviceMetadata, deviceIDs))
	deviceRaw, ok := deviceMetadata["user_id"].(string)
	require.True(t, ok)
	deviceResult := parseClaudeUserIDJSON(deviceRaw)
	require.Equal(t, deviceIDs.deviceID, deviceResult["device_id"])
	require.Equal(t, claudeFingerprintClientAccount, deviceResult["account_uuid"])
	require.Equal(t, claudeFingerprintClientSession, deviceResult["session_id"])
}

func TestApplyClaudeFingerprintToUserIDJSON_RebuildsMalformedValue(t *testing.T) {
	account := newTestClaudeOAuthAccount(7005, map[string]any{
		"claude_fingerprint_mode": "full",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})
	ids := resolveClaudeFingerprintIDs(account, "", claudeFingerprintFull)
	require.NotNil(t, ids)

	for _, raw := range []string{"", "not-json", `["array"]`, `{"unknown":"only"}`} {
		metadata := map[string]any{"user_id": raw}
		require.True(t, applyClaudeFingerprintToUserIDJSON(metadata, ids), "raw=%q", raw)
		rebuiltRaw, ok := metadata["user_id"].(string)
		require.True(t, ok, "raw=%q", raw)
		rebuilt := parseClaudeUserIDJSON(rebuiltRaw)
		require.NotNil(t, rebuilt, "raw=%q", raw)
		require.Equal(t, ids.deviceID, rebuilt["device_id"], "raw=%q", raw)
		require.Equal(t, ids.sessionID, rebuilt["session_id"], "raw=%q", raw)
	}
}

// --- raw 字节改写：热路径不得对整 body 全量 Unmarshal ---

func TestApplyClaudeFingerprintClientMetadataRaw_MatchesMapVariant(t *testing.T) {
	account := newTestClaudeOAuthAccount(7006, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
		"account_uuid":            "real-account-uuid",
	})
	ids := resolveClaudeFingerprintIDs(account, claudeFingerprintClientSession, claudeFingerprintSession)
	require.NotNil(t, ids)

	body := claudeFingerprintClientBody(claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession)
	next, changed, err := applyClaudeFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)
	rewritten := parseClaudeUserIDJSON(gjson.GetBytes(next, "metadata.user_id").String())
	require.Equal(t, ids.deviceID, rewritten["device_id"])
	require.Equal(t, ids.sessionID, rewritten["session_id"])
	require.Equal(t, "claude-sonnet-5", gjson.GetBytes(next, "model").String(), "无关字段必须原样保留")

	// 幂等：二次改写不得报告变更
	_, changedAgain, err := applyClaudeFingerprintClientMetadataRaw(next, ids)
	require.NoError(t, err)
	require.False(t, changedAgain)

	// 无 metadata 的 body 原样返回
	untouched, changedEmpty, err := applyClaudeFingerprintClientMetadataRaw([]byte(`{"model":"x"}`), ids)
	require.NoError(t, err)
	require.False(t, changedEmpty)
	require.Equal(t, `{"model":"x"}`, string(untouched))
}

// 固化派生值：这些值会作为设备/会话身份发往上游。一旦算法或命名空间前缀变化，
// 所有已开收敛的账号在上游都会「换一台设备、换一个会话」——正是 seed 生命周期
// 与禁用重开不轮换 seed 那些规则在避免的事。等价重构不得改变它们。
func TestDerivedClaudeIdentitiesArePinned(t *testing.T) {
	require.Equal(t, "e1851c2775298c37c50b3aa1b6aa758ce5b083d127637ca5c87eb99f0d7625d5",
		deriveStableClaudeDeviceID("sub2api:claude-device-id:v1:"+claudeFingerprintTestSeed))
	require.Equal(t, "6c55c8fb-01f8-4bc6-b25e-ceb08dadfaf4",
		deriveStableClaudeSessionID(claudeFingerprintTestSeed, ""))
	require.Equal(t, "d415f23d-9244-4e44-a880-0d4f1b9bed50",
		deriveStableClaudeSessionID(claudeFingerprintTestSeed, claudeFingerprintClientSession))
}

// --- 出站头改写 ---

func TestApplyClaudeFingerprintHeaders(t *testing.T) {
	account := newTestClaudeOAuthAccount(7007, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})
	ids := resolveClaudeFingerprintIDs(account, claudeFingerprintClientSession, claudeFingerprintSession)
	require.NotNil(t, ids)

	h := http.Header{}
	h.Set("x-claude-code-session-id", claudeFingerprintClientSession)
	h.Set("x-session-id", claudeFingerprintClientSession)
	applyClaudeFingerprintHeaders(h, ids)
	require.Equal(t, ids.sessionID, h.Get("x-claude-code-session-id"))
	require.Equal(t, ids.sessionID, h.Get("x-session-id"))

	// device 模式不改写会话头（只收敛 device_id）
	deviceIDs := resolveClaudeFingerprintIDs(account, claudeFingerprintClientSession, claudeFingerprintDevice)
	deviceHeaders := http.Header{}
	deviceHeaders.Set("x-claude-code-session-id", claudeFingerprintClientSession)
	applyClaudeFingerprintHeaders(deviceHeaders, deviceIDs)
	require.Equal(t, claudeFingerprintClientSession, deviceHeaders.Get("x-claude-code-session-id"))

	// nil 安全
	applyClaudeFingerprintHeaders(nil, ids)
	applyClaudeFingerprintHeaders(h, nil)
}

// --- 接线：buildUpstreamRequest 必须应用暂存的收敛 ID ---

func TestBuildUpstreamRequest_AppliesStagedClaudeFingerprintHeaders(t *testing.T) {
	c := newClaudeFingerprintTestContext(t)
	account := newTestClaudeOAuthAccount(7100, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})
	c.Request.Header.Set("x-claude-code-session-id", claudeFingerprintClientSession)

	body := claudeFingerprintClientBody(claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession)
	ids := resolveClaudeFingerprintIDsFromRawBody(account, body)
	require.NotNil(t, ids)
	next, changed, err := applyClaudeFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)
	stageClaudeFingerprintIDs(c, ids)

	req, wireBody, err := newClaudeFingerprintTestService().buildUpstreamRequest(
		context.Background(), c, account, next, "oauth-token", "oauth", "claude-sonnet-5", false, false)
	require.NoError(t, err)

	require.Equal(t, ids.sessionID, getHeaderRaw(req.Header, "X-Claude-Code-Session-Id"),
		"出站会话头必须与请求体共享同一份收敛 ID")
	require.Equal(t, ids.sessionID, getHeaderRaw(req.Header, "x-session-id"))
	require.Equal(t, ids.sessionID, parseClaudeUserIDJSON(gjson.GetBytes(wireBody, "metadata.user_id").String())["session_id"],
		"请求体与出站头必须一致")
}

// identityService 非 nil 是默认生产配置：buildUpstreamRequest 会在应用收敛 ID 之前
// 用 RewriteUserIDWithMasking 再改写一次 metadata.user_id（账号有 account_uuid 时必然触发）。
// 出站头与 wire body 必须仍然共享同一份收敛 ID——这正是收敛模块自己声明的铁律。
func TestBuildUpstreamRequest_ConvergedIdentitySurvivesIdentityRewrite(t *testing.T) {
	c := newClaudeFingerprintTestContext(t)
	account := newTestClaudeOAuthAccount(7700, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
		"account_uuid":            "real-account-uuid",
	})
	svc := newClaudeFingerprintTestService()
	svc.identityService = NewIdentityService(&stubIdentityCache{})

	body := stageClaudeFingerprintForBody(c, account, claudeFingerprintClientBody(
		claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession))
	ids := stagedClaudeFingerprintIDs(c, account)
	require.NotNil(t, ids)

	req, wireBody, err := svc.buildUpstreamRequest(
		context.Background(), c, account, body, "oauth-token", "oauth", "claude-sonnet-5", false, false)
	require.NoError(t, err)

	require.Equal(t, ids.sessionID, getHeaderRaw(req.Header, "X-Claude-Code-Session-Id"))
	wire := parseClaudeUserIDJSON(gjson.GetBytes(wireBody, "metadata.user_id").String())
	require.NotNil(t, wire)
	require.Equal(t, ids.sessionID, wire["session_id"],
		"下游身份重写不得让 wire body 与出站头指向不同的会话")
	require.Equal(t, ids.deviceID, wire["device_id"],
		"device_id 收敛同样不得被下游身份重写覆盖")
}

func TestBuildCountTokensRequest_ConvergedIdentitySurvivesIdentityRewrite(t *testing.T) {
	c := newClaudeFingerprintTestContext(t)
	account := newTestClaudeOAuthAccount(7701, map[string]any{
		"claude_fingerprint_mode": "full",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
		"account_uuid":            "real-account-uuid",
	})
	svc := newClaudeFingerprintTestService()
	svc.identityService = NewIdentityService(&stubIdentityCache{})

	body := stageClaudeFingerprintForBody(c, account, claudeFingerprintClientBody(
		claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession))
	ids := stagedClaudeFingerprintIDs(c, account)
	require.NotNil(t, ids)

	req, wireBody, err := svc.buildCountTokensRequest(
		context.Background(), c, account, body, "oauth-token", "oauth", "claude-sonnet-5", false)
	require.NoError(t, err)

	require.Equal(t, ids.sessionID, getHeaderRaw(req.Header, "X-Claude-Code-Session-Id"))
	wire := parseClaudeUserIDJSON(gjson.GetBytes(wireBody, "metadata.user_id").String())
	require.NotNil(t, wire)
	require.Equal(t, ids.sessionID, wire["session_id"])
	require.Equal(t, ids.deviceID, wire["device_id"])
}

func TestBuildUpstreamRequest_IgnoresStaleClaudeFingerprintIDsOnFailover(t *testing.T) {
	c := newClaudeFingerprintTestContext(t)
	converging := newTestClaudeOAuthAccount(7200, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})
	ids := resolveClaudeFingerprintIDs(converging, claudeFingerprintClientSession, claudeFingerprintSession)
	require.NotNil(t, ids)
	stageClaudeFingerprintIDs(c, ids)

	// failover：本次请求改由另一个账号服务，暂存 ID 必须按账号失配作废
	other := newTestClaudeOAuthAccount(7201, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed2,
	})
	c.Request.Header.Set("x-claude-code-session-id", claudeFingerprintClientSession)
	body := claudeFingerprintClientBody(claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession)

	req, _, err := newClaudeFingerprintTestService().buildUpstreamRequest(
		context.Background(), c, other, body, "oauth-token", "oauth", "claude-sonnet-5", false, false)
	require.NoError(t, err)
	require.Equal(t, claudeFingerprintClientSession, getHeaderRaw(req.Header, "X-Claude-Code-Session-Id"),
		"失配账号不得读到上一账号的收敛 ID")
}

// --- 接线：count_tokens 路径同样必须收敛 ---

func TestBuildCountTokensRequest_AppliesStagedClaudeFingerprintHeaders(t *testing.T) {
	c := newClaudeFingerprintTestContext(t)
	account := newTestClaudeOAuthAccount(7300, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})
	c.Request.Header.Set("x-claude-code-session-id", claudeFingerprintClientSession)

	body := claudeFingerprintClientBody(claudeFingerprintClientDeviceID, claudeFingerprintClientAccount, claudeFingerprintClientSession)
	ids := resolveClaudeFingerprintIDsFromRawBody(account, body)
	require.NotNil(t, ids)
	next, changed, err := applyClaudeFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)
	stageClaudeFingerprintIDs(c, ids)

	req, _, err := newClaudeFingerprintTestService().buildCountTokensRequest(
		context.Background(), c, account, next, "oauth-token", "oauth", "claude-sonnet-5", false)
	require.NoError(t, err)
	require.Equal(t, ids.sessionID, getHeaderRaw(req.Header, "X-Claude-Code-Session-Id"))
}

// --- seed 生命周期 ---

func TestPrepareClaudeFingerprintExtraLifecycle(t *testing.T) {
	// create：开启收敛时铸新 seed，并剥离调用方伪造的 seed
	created := prepareClaudeFingerprintExtraForCreate(PlatformAnthropic, AccountTypeOAuth, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})
	seed, ok := claudeFingerprintSeed(created)
	require.True(t, ok)
	require.NotEqual(t, claudeFingerprintTestSeed, seed, "调用方提供的 seed 不得被采信")

	// create：off 模式不铸 seed
	offCreated := prepareClaudeFingerprintExtraForCreate(PlatformAnthropic, AccountTypeOAuth, map[string]any{
		"claude_fingerprint_mode": "off",
	})
	_, ok = claudeFingerprintSeed(offCreated)
	require.False(t, ok)

	// update：已有合法 seed 必须保留
	existing := newTestClaudeOAuthAccount(7400, map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	})
	updated := prepareClaudeFingerprintExtraForUpdate(existing, map[string]any{
		"claude_fingerprint_mode": "full",
		"claude_fingerprint_seed": "99999999-9999-4999-8999-999999999999",
	})
	require.Equal(t, claudeFingerprintTestSeed, updated["claude_fingerprint_seed"], "轮换 seed 会重置上游可见身份")

	// update：此前无 seed（存量账号）且本次开启收敛 → 补铸
	legacy := newTestClaudeOAuthAccount(7401, map[string]any{"claude_fingerprint_mode": "off"})
	backfilled := prepareClaudeFingerprintExtraForUpdate(legacy, map[string]any{"claude_fingerprint_mode": "device"})
	_, ok = claudeFingerprintSeed(backfilled)
	require.True(t, ok)

	// 非 OAuth-like 账号不得获得 seed
	apiKey := &Account{ID: 7402, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{}}
	_, ok = claudeFingerprintSeed(prepareClaudeFingerprintExtraForCreate(PlatformAnthropic, AccountTypeAPIKey, apiKey.Extra))
	require.False(t, ok)
}

func TestClaudeFingerprintExtraUpdateHelpers(t *testing.T) {
	updates := map[string]any{
		"claude_fingerprint_mode": "session",
		"claude_fingerprint_seed": claudeFingerprintTestSeed,
	}
	sanitized := sanitizedClaudeFingerprintExtraUpdates(updates)
	_, hasSeed := sanitized["claude_fingerprint_seed"]
	require.False(t, hasSeed, "key 级 JSONB 更新不得让调用方写入 seed")
	require.Equal(t, "session", sanitized["claude_fingerprint_mode"])

	require.True(t, ShouldEnsureClaudeFingerprintSeedForExtraUpdates(updates))
	require.True(t, ShouldEnsureClaudeFingerprintSeedForExtraUpdates(map[string]any{"claude_fingerprint_mode": "full"}))
	require.False(t, ShouldEnsureClaudeFingerprintSeedForExtraUpdates(map[string]any{"claude_fingerprint_mode": "off"}))
	require.False(t, ShouldEnsureClaudeFingerprintSeedForExtraUpdates(nil))
}

// --- account_uuid 必须取自账号真实 OAuth 元数据 ---

func TestGetClaudeAccountUUID(t *testing.T) {
	account := newTestClaudeOAuthAccount(7500, map[string]any{"account_uuid": "real-account-uuid"})
	require.Equal(t, "real-account-uuid", account.GetClaudeAccountUUID())

	require.Empty(t, newTestClaudeOAuthAccount(7501, nil).GetClaudeAccountUUID())
	require.Empty(t, (*Account)(nil).GetClaudeAccountUUID())
}
