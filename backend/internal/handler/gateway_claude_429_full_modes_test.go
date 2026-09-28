//go:build unit

package handler

// Ticket 01 / 父规格主链：四种 Anthropic OAuth 出站身份收敛（off / device /
// session / full）不得改变 /v1/messages 跨请求 429 冷却的**入站**作用域。
//
// 本文件在最高外部行为接缝上验证（真实 GatewayHandler.Messages、真实
// GatewayService.Forward、真实 IdentityService——仅缓存为内存桩——与记录型假
// HTTP 上游）：
//  1. A 首次请求真正触顶 N（默认 2，两个上游账号各自最终 429）；
//  2. A 换 request_id、同设备同会话再请求 → B2 本地 429，新增上游尝试为 0；
//  3. B 共用同一把 B1 API Key、但入站设备/会话不同 → 不被本地冷却（新增 2 次
//     上游尝试），**即使 full 模式下 B2 出站身份已被收敛成与 A 完全相同**；
//  4. 顺带抓取真实出站 metadata.user_id 与 X-Claude-Code-Session-Id，核对每种
//     收敛模式各自的既有出站行为（头体一致、full 收敛成单设备单会话、
//     session 按客户端会话派生、off/device 不收敛会话）。
//
// 诚实边界：入站身份来自客户端原始 body + Claude 会话头，先于 OAuth 身份层与
// 收敛层；本用例不依赖 .scratch 夹具、真实 Anthropic 凭据或数据库。收敛种子按
// 账号各有其一（与生产一致），因此出站身份只在**同一账号的多次尝试**之间比较
// （每次都取先被选中的账号 1 的那次尝试）。
//
// 复用：gateway_429_account_limit_test.go 的 gateway429NoCooldownRepo /
// openAIImagesFailoverAccountRepo / fakeGroupRepo / fakeSchedulerCache /
// fakeConcurrencyCache，gateway_claude_429_cooldown_test.go 的
// claude429TestStore 与设置形态，openai_429_account_limit_matrix_test.go 的
// newOpenAI429MatrixSettingRepo。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	// 与真实 Claude Code 一致的 64 位十六进制 device_id 形态。
	claude429ModeDeviceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	claude429ModeDeviceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// 会话 ID 只含十六进制与连字符，保证旧拼接格式可解析。
	claude429ModeSessionA = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	claude429ModeSessionB = "11111111-2222-4333-8444-555555555555"
	claude429ModeSessionC = "cccccccc-dddd-4eee-8fff-111111111111"
	// 账号侧真实 account_uuid（OAuth extra「account_uuid」），也是收敛目标。
	claude429ModeAccountUUID = "77777777-6666-4555-8444-333333333333"
	// 合成 B1 中继形态的真实 Claude Code UA（官方 B1 出站即此形态）。
	claude429ModeClientUA = "claude-cli/2.1.283"
)

// 每个上游账号一个独立收敛种子（生产语义：种子按账号隔离）。
var claude429ModeSeeds = []string{
	"11111111-1111-4111-8111-111111111111",
	"22222222-2222-4222-8222-222222222222",
	"33333333-3333-4333-8333-333333333333",
}

// --- 记录型假上游 ---

type claude429ModeWire struct {
	accountID     int64
	sessionHeader string
	body          []byte
}

type claude429ModeUpstream struct {
	service.HTTPUpstream
	hits []int64
	wire []claude429ModeWire
}

func (u *claude429ModeUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (u *claude429ModeUpstream) DoWithTLS(req *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	record := claude429ModeWire{accountID: accountID}
	if req != nil {
		record.sessionHeader = req.Header.Get("X-Claude-Code-Session-Id")
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			_ = req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(raw))
			record.body = raw
		}
	}
	u.wire = append(u.wire, record)
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`)),
	}, nil
}

// accountWires 返回发往指定账号的全部出站记录（保持调用顺序）。
func (u *claude429ModeUpstream) accountWires(accountID int64) []claude429ModeWire {
	out := make([]claude429ModeWire, 0, len(u.wire))
	for _, record := range u.wire {
		if record.accountID == accountID {
			out = append(out, record)
		}
	}
	return out
}

// --- OAuth 账号夹具 ---

// claude429ModeAccountExtra 组装 OAuth 账号 extra：真实 account_uuid、按账号隔离的
// 收敛种子，以及本次要验证的收敛模式（OAuth/SetupToken 专属开关）。
func claude429ModeAccountExtra(id int64, mode string) map[string]any {
	seed := claude429ModeSeeds[0]
	if id >= 1 && int(id) <= len(claude429ModeSeeds) {
		seed = claude429ModeSeeds[id-1]
	}
	return map[string]any{
		"account_uuid":            claude429ModeAccountUUID,
		"claude_fingerprint_seed": seed,
		"claude_fingerprint_mode": mode,
	}
}

// claude429ModeAccounts 构造 count 个可调度的 Anthropic OAuth 上游账号。
// 不注入 claudeTokenProvider，GetAccessToken 会退回 credentials.access_token。
func claude429ModeAccounts(groupID int64, count int, mode string) []*service.Account {
	accounts := make([]*service.Account, 0, count)
	for id := int64(1); id <= int64(count); id++ {
		accounts = append(accounts, &service.Account{
			ID:          id,
			Name:        fmt.Sprintf("claude-oauth-%d", id),
			Platform:    service.PlatformAnthropic,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Priority:    int(id),
			Concurrency: 1,
			Credentials: map[string]any{"access_token": fmt.Sprintf("local-oauth-token-%d", id)},
			Extra:       claude429ModeAccountExtra(id, mode),
			AccountGroups: []service.AccountGroup{{
				AccountID: id,
				GroupID:   groupID,
			}},
		})
	}
	return accounts
}

// --- 身份缓存桩 ---

// claude429ModeIdentityCache 是 service.IdentityCache 的确定性内存实现：
// 每个账号预置一份合法指纹，避免随机 ClientID 影响断言；会话伪装保持关闭默认值。
type claude429ModeIdentityCache struct {
	mu           sync.Mutex
	fingerprints map[int64]*service.Fingerprint
}

// claude429ModeFingerprintClientID 是夹具为账号预置的身份层 device_id（64 位十六进制）；
// 断言用它区分「off 模式下身份层是 device 的最后写者」与「收敛层接管 device」。
func claude429ModeFingerprintClientID(accountID int64) string {
	return fmt.Sprintf("%064x", uint64(0x4290000)+uint64(accountID))
}

func newClaude429ModeIdentityCache(accountIDs ...int64) *claude429ModeIdentityCache {
	cache := &claude429ModeIdentityCache{fingerprints: map[int64]*service.Fingerprint{}}
	for _, id := range accountIDs {
		cache.fingerprints[id] = &service.Fingerprint{
			ClientID:  claude429ModeFingerprintClientID(id),
			UserAgent: claude429ModeClientUA,
			UpdatedAt: 4102444800, // 2100-01-01：不触发 24 小时续期写
		}
	}
	return cache
}

func (c *claude429ModeIdentityCache) GetFingerprint(_ context.Context, accountID int64) (*service.Fingerprint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fp, ok := c.fingerprints[accountID]
	if !ok {
		return nil, nil
	}
	clone := *fp
	return &clone, nil
}

func (c *claude429ModeIdentityCache) SetFingerprint(_ context.Context, accountID int64, fp *service.Fingerprint) error {
	if fp == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	clone := *fp
	c.fingerprints[accountID] = &clone
	return nil
}

func (c *claude429ModeIdentityCache) GetMaskedSessionID(context.Context, int64) (string, error) {
	return "", nil
}

func (c *claude429ModeIdentityCache) SetMaskedSessionID(context.Context, int64, string) error {
	return nil
}

// --- handler / 请求装配 ---

// newClaude429ModeHandler 与 newGateway429TestHandler 同构，但额外接入真实
// IdentityService——没有它，OAuth 账号的指纹与 metadata 改写不会发生，就验证不了
// 「出站身份被改写后入站冷却作用域仍不变」。
func newClaude429ModeHandler(t *testing.T, upstream service.HTTPUpstream, accounts []*service.Account, group *service.Group) *GatewayHandler {
	t.Helper()
	repoAccounts := make([]service.Account, 0, len(accounts))
	accountIDs := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		repoAccounts = append(repoAccounts, *account)
		accountIDs = append(accountIDs, account.ID)
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: repoAccounts}
	rateLimit := service.NewRateLimitService(gateway429NoCooldownRepo{accountRepo}, nil, &config.Config{}, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
	gw := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		&config.Config{}, snapshot, nil, nil, rateLimit, nil,
		service.NewIdentityService(newClaude429ModeIdentityCache(accountIDs...)),
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	return &GatewayHandler{
		gatewayService:      gw,
		billingCacheService: billing,
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
		maxAccountSwitches:  10,
		cfg:                 cfg,
	}
}

// claude429ModeBody 构造合成 B1 入站请求体：真实 Claude Code 旧拼接格式
// metadata.user_id（device / account_uuid / session 三段均可解析）。
func claude429ModeBody(deviceID, sessionID string) []byte {
	userID := "user_" + deviceID + "_account_" + claude429ModeAccountUUID + "_session_" + sessionID
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":32,` +
		`"system":[{"text":"You are Claude Code, Anthropic's official CLI for Claude."}],` +
		`"messages":[{"role":"user","content":"hello"}],` +
		`"metadata":{"user_id":"` + userID + `"}}`)
}

// claude429ModeContext 构造一次「B1 已鉴权 → B2」的入站请求：
// 真实 Claude Code UA + 严格校验所需头，使 B2 走 Claude Code 客户端路径
// （即不进入 mimicry），把变量收敛到「身份层 + 收敛层」本身。
func claude429ModeContext(t *testing.T, group *service.Group, keyID int64, deviceID, sessionID, requestID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := group.ID
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBuffer(claude429ModeBody(deviceID, sessionID)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", claude429ModeClientUA)
	req.Header.Set("X-App", "claude-code")
	req.Header.Set("anthropic-beta", "message-batches-2024-09-24")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	req.Header.Set("X-Request-Id", requestID)
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	apiKey := &service.APIKey{ID: keyID, UserID: 10, GroupID: &groupID, Status: service.StatusActive, User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	return c, rec
}

// claude429ModeOutbound 是从真实出站报文里读出的身份三元组。
type claude429ModeOutbound struct {
	deviceID    string
	accountUUID string
	sessionID   string
}

func parseClaude429ModeOutbound(t *testing.T, record claude429ModeWire) claude429ModeOutbound {
	t.Helper()
	raw := gjson.GetBytes(record.body, "metadata.user_id").String()
	require.NotEmpty(t, raw, "every OAuth attempt must carry metadata.user_id")
	parsed := service.ParseMetadataUserID(raw)
	require.NotNil(t, parsed, "outbound metadata.user_id must stay parseable: %s", raw)
	require.NotEmpty(t, parsed.DeviceID)
	require.NotEmpty(t, parsed.SessionID)
	require.Equal(t, parsed.SessionID, record.sessionHeader,
		"outbound Claude session header must equal the body session_id (头体一致性)")
	return claude429ModeOutbound{deviceID: parsed.DeviceID, accountUUID: parsed.AccountUUID, sessionID: parsed.SessionID}
}

// TestGatewayClaude429CooldownMessagesAcrossOutboundConvergenceModes 是 Ticket 01
// 主链在四种出站收敛模式下的回归：A 触顶 → A 同会话再请求本地 429（零新增上游
// 尝试）→ B 同 Key 不同身份仍可尝试两个账号 → A 新会话仍可尝试两个账号；
// 同时核对本模式下出站身份的真实形态。
func TestGatewayClaude429CooldownMessagesAcrossOutboundConvergenceModes(t *testing.T) {
	for _, mode := range []string{"off", "device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			groupID := int64(9050)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			upstream := &claude429ModeUpstream{}
			accounts := claude429ModeAccounts(groupID, 3, mode)
			h := newClaude429ModeHandler(t, upstream, accounts, group)

			settings := newOpenAI429MatrixSettingRepo(2)
			require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown,
				`{"enabled":true,"scope":"session","cooldown_seconds":60}`))
			h.settingService = service.NewSettingService(settings, h.cfg)
			store := newClaude429TestStore()
			h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)

			// 1) A 首次：真正触顶 N=2，两个上游账号各被尝试一次。
			c1, rec1 := claude429ModeContext(t, group, 9008, claude429ModeDeviceA, claude429ModeSessionA, "turn-1")
			h.Messages(c1)
			require.Equal(t, http.StatusTooManyRequests, rec1.Code)
			require.Equal(t, []int64{1, 2}, upstream.hits, "A 首次必须真正触顶 N=2")

			// 2) A 换 request_id、同设备同会话：B2 本地 429，新增上游尝试为 0。
			c2, rec2 := claude429ModeContext(t, group, 9008, claude429ModeDeviceA, claude429ModeSessionA, "turn-2")
			h.Messages(c2)
			require.Equal(t, http.StatusTooManyRequests, rec2.Code)
			require.Equal(t, []int64{1, 2}, upstream.hits, "同会话冷却命中后不得产生任何新增上游尝试")
			require.Equal(t, "60", rec2.Header().Get("Retry-After"))
			require.Contains(t, rec2.Body.String(), claude429CooldownCode)
			require.NotContains(t, rec2.Body.String(), claude429ModeDeviceA, "本地冷却响应不得回显原始身份")
			require.NotContains(t, rec2.Body.String(), claude429ModeSessionA)

			// 3) B 共用 B1 Key、设备/会话不同：不被本地冷却（完整再换号一轮）。
			c3, rec3 := claude429ModeContext(t, group, 9008, claude429ModeDeviceB, claude429ModeSessionB, "turn-3")
			h.Messages(c3)
			require.Equal(t, http.StatusTooManyRequests, rec3.Code)
			require.Empty(t, rec3.Header().Get("Retry-After"), "B 未被本功能冷却，不应带本地 Retry-After")
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "B 同 Key 不同身份必须仍可尝试两个上游账号")

			// 4) A 开新会话：会话级粒度下同样不被冷却（即使 full 让出站会话收敛成同一个）。
			c4, rec4 := claude429ModeContext(t, group, 9008, claude429ModeDeviceA, claude429ModeSessionC, "turn-4")
			h.Messages(c4)
			require.Equal(t, http.StatusTooManyRequests, rec4.Code)
			require.Empty(t, rec4.Header().Get("Retry-After"))
			require.Equal(t, []int64{1, 2, 1, 2, 1, 2}, upstream.hits, "冷却作用域是入站会话，不是出站会话")

			// 出站身份：每次逻辑请求的首次尝试都落在账号 1（顺序由既有用例固定），
			// 因此这三条记录分别是 A-first / B-other / A-new-session 在同账号上的出站形态。
			wires := upstream.accountWires(1)
			require.Len(t, wires, 3, "A-first / B / A-new-session 各应在账号 1 上留下一条出站记录")
			a := parseClaude429ModeOutbound(t, wires[0])
			b := parseClaude429ModeOutbound(t, wires[1])
			other := parseClaude429ModeOutbound(t, wires[2])

			// 收敛是显式 opt-in：模式未生效时出站仍是身份层产物，绝不是客户端原值。
			require.NotEqual(t, claude429ModeDeviceA, a.deviceID)
			require.NotEqual(t, claude429ModeSessionA, a.sessionID)
			// OAuth 身份层对同一账号统一设备：四种模式下 A/B 的出站设备都相同。
			require.Equal(t, a.deviceID, b.deviceID, "同一 OAuth 账号下 A/B 的出站设备由账号身份决定")

			switch mode {
			case "off":
				// 收敛层不启用：device 仍是身份层写入的账号指纹 ClientID，
				// 会话是身份层按「账号 + 原会话」哈希的结果，A/B 不同。
				require.Equal(t, claude429ModeFingerprintClientID(1), a.deviceID,
					"off 模式下身份层是 device 的最后写者")
				require.NotEqual(t, a.sessionID, b.sessionID)
				require.NotEqual(t, a.sessionID, other.sessionID)
			case "device":
				// 只收敛设备：device 由种子派生、取代身份层；会话仍由身份层哈希决定，A/B 不同。
				require.NotEqual(t, claude429ModeFingerprintClientID(1), a.deviceID,
					"device 模式下收敛层是 device 的最后写者")
				require.NotEqual(t, a.sessionID, b.sessionID)
				require.NotEqual(t, a.sessionID, other.sessionID)
			case "session":
				// 会话按「种子 + 客户端会话」派生：A/B 不同，A 新会话也不同。
				require.NotEqual(t, claude429ModeFingerprintClientID(1), a.deviceID)
				require.NotEqual(t, a.sessionID, b.sessionID)
				require.NotEqual(t, a.sessionID, other.sessionID)
				require.Equal(t, claude429ModeAccountUUID, a.accountUUID, "session 模式收敛到账号真实 UUID")
			case "full":
				// 全账号归一：A/B/A 新会话出站设备与会话完全相同——
				// 而上面第 3、4 步已断言 B 与 A 新会话都没有被本地冷却。
				require.NotEqual(t, claude429ModeFingerprintClientID(1), a.deviceID)
				require.Equal(t, a.sessionID, b.sessionID, "full 模式下 A/B 出站会话在单账号内被归一")
				require.Equal(t, a.sessionID, other.sessionID, "full 模式下 A 新会话也被归一")
				require.Equal(t, claude429ModeAccountUUID, a.accountUUID)
			}
		})
	}
}

// TestGatewayClaude429CooldownMessagesDeviceScopeAcrossOutboundConvergenceModes
// 验证粒度本身也由**入站**身份决定、不随出站收敛改变：同一批 A 请求在设备级
// 粒度下，A 同设备开新会话同样被本地冷却（上游尝试为 0），而 B 换设备仍放行；
// 这在 full 模式下尤其反直觉——A 新会话的出站身份此时与 A 首次完全相同。
func TestGatewayClaude429CooldownMessagesDeviceScopeAcrossOutboundConvergenceModes(t *testing.T) {
	for _, mode := range []string{"off", "device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			groupID := int64(9051)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			upstream := &claude429ModeUpstream{}
			accounts := claude429ModeAccounts(groupID, 3, mode)
			h := newClaude429ModeHandler(t, upstream, accounts, group)

			settings := newOpenAI429MatrixSettingRepo(2)
			require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown,
				`{"enabled":true,"scope":"device","cooldown_seconds":60}`))
			h.settingService = service.NewSettingService(settings, h.cfg)
			h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, newClaude429TestStore())

			// A 首次触顶 N=2。
			c1, rec1 := claude429ModeContext(t, group, 9008, claude429ModeDeviceA, claude429ModeSessionA, "turn-1")
			h.Messages(c1)
			require.Equal(t, http.StatusTooManyRequests, rec1.Code)
			require.Equal(t, []int64{1, 2}, upstream.hits)

			// A 同会话换 request_id：本地 429，零新增上游尝试。
			c2, rec2 := claude429ModeContext(t, group, 9008, claude429ModeDeviceA, claude429ModeSessionA, "turn-2")
			h.Messages(c2)
			require.Equal(t, http.StatusTooManyRequests, rec2.Code)
			require.Equal(t, []int64{1, 2}, upstream.hits)
			require.Equal(t, "60", rec2.Header().Get("Retry-After"))

			// A 同设备开新会话：设备级粒度下仍被本地冷却，零新增上游尝试。
			c3, rec3 := claude429ModeContext(t, group, 9008, claude429ModeDeviceA, claude429ModeSessionC, "turn-3")
			h.Messages(c3)
			require.Equal(t, http.StatusTooManyRequests, rec3.Code)
			require.Equal(t, []int64{1, 2}, upstream.hits, "设备级冷却不得因换会话而放行")
			require.NotEmpty(t, rec3.Header().Get("Retry-After"))

			// B 换设备：仍可完整再换号一轮。
			c4, rec4 := claude429ModeContext(t, group, 9008, claude429ModeDeviceB, claude429ModeSessionB, "turn-4")
			h.Messages(c4)
			require.Equal(t, http.StatusTooManyRequests, rec4.Code)
			require.Empty(t, rec4.Header().Get("Retry-After"))
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits)
		})
	}
}
