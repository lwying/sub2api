//go:build unit

package handler

// Ticket 02 — Anthropic 支撑的 Responses / Chat Completions 消息入口沿用 429 跨请求冷却。
//
// 这些入口在协议转换/伪装前已经读到客户端原始请求体，因此冷却作用域必须取自
// 「转换前的入站身份」（正文 metadata.user_id + 匹配的 Claude 会话头），而不是转换后
// 出现在出站 wire 上的任何身份。本文件的用例都在真实 handler HTTP 入口 + 假上游上断言
// 外部可见行为：本地 429、Retry-After 剩余秒数、错误格式、上游被调用的账号序列，
// 以及冷却存储的读写次数（缺失/矛盾身份必须不读不写）。
//
// 夹具复用 Ticket 01 建立的 newGateway429TestHandler / gateway429TestAccounts /
// newOpenAI429MatrixSettingRepo / claude429TestStore / claude429TestContext。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	// claude429BridgeLimit 与既有夹具一致：每个逻辑请求允许 2 个不同上游账号返回 429。
	claude429BridgeLimit = 2
	// claude429BridgeSessionScopeJSON / DeviceScopeJSON 是 Ticket 01 同一个后台控件的取值。
	claude429BridgeSessionScopeJSON = `{"enabled":true,"scope":"session","cooldown_seconds":60}`
	claude429BridgeDeviceScopeJSON  = `{"enabled":true,"scope":"device","cooldown_seconds":60}`
)

// claude429BridgeEntry 描述一个 Anthropic 支撑的兼容消息入口。
type claude429BridgeEntry struct {
	name string
	path string
	// errorCode 是该入口错误响应里的协议格式标记，用于区分原生 Messages JSON。
	errorCode string
	call      func(h *GatewayHandler, c *gin.Context)
	body      func(identity string) string
}

func claude429BridgeEntries() []claude429BridgeEntry {
	return []claude429BridgeEntry{
		{
			name:      "responses",
			path:      "/v1/responses",
			errorCode: `"code":"claude_429_account_limit_cooldown"`,
			call:      func(h *GatewayHandler, c *gin.Context) { h.Responses(c) },
			body:      claude429ResponsesBridgeBody,
		},
		{
			name:      "chat",
			path:      "/v1/chat/completions",
			errorCode: `"type":"rate_limit_error"`,
			call:      func(h *GatewayHandler, c *gin.Context) { h.ChatCompletions(c) },
			body:      claude429ChatCompletionsBridgeBody,
		},
	}
}

// claude429BridgeIdentity 生成 legacy 形态的原始 metadata.user_id（与真实客户端一致）。
func claude429BridgeIdentity(deviceID, sessionID string) string {
	return "user_" + deviceID + "_account__session_" + sessionID
}

// claude429ResponsesBridgeBody / claude429ChatCompletionsBridgeBody 生成入口请求体。
// identity 为空串表示入站完全没有 metadata.user_id（转换前就没有原始身份）。
func claude429ResponsesBridgeBody(identity string) string {
	body := `{"model":"claude-sonnet-4-5","input":"hello","stream":false`
	if identity != "" {
		body += `,"metadata":{"user_id":"` + identity + `"}`
	}
	return body + `}`
}

func claude429ChatCompletionsBridgeBody(identity string) string {
	body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}],"stream":false`
	if identity != "" {
		body += `,"metadata":{"user_id":"` + identity + `"}`
	}
	return body + `}`
}

// claude429BridgeUpstream 是记录上游调用序列、出站正文与出站头的 429 假上游。
// onCall 允许用例注入「本次上游调用期间发生的事」（例如向客户端写入已提交的流内容，
// 或取消请求 context），用于负控。
type claude429BridgeUpstream struct {
	service.HTTPUpstream
	hits     []int64
	outbound [][]byte
	headers  []http.Header
	onCall   func()
}

func (u *claude429BridgeUpstream) DoWithTLS(req *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	if req != nil {
		if req.Body != nil {
			if raw, err := io.ReadAll(req.Body); err == nil {
				u.outbound = append(u.outbound, raw)
			}
		}
		u.headers = append(u.headers, req.Header.Clone())
	}
	if u.onCall != nil {
		u.onCall()
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`)),
	}, nil
}

// claude429BridgeContext 构造入口请求：入站正文 + Claude 会话头 + 独立 request ID。
func claude429BridgeContext(t *testing.T, group *service.Group, keyID int64, path, body, session, requestID string) (*gin.Context, *httptest.ResponseRecorder, context.CancelFunc) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", session)
	req.Header.Set("X-Request-Id", requestID)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = req.WithContext(context.WithValue(ctx, ctxkey.Group, group))
	groupID := group.ID
	apiKey := &service.APIKey{ID: keyID, UserID: 10, GroupID: &groupID, Status: service.StatusActive, User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 10, Concurrency: 10})
	return c, rec, cancel
}

// newClaude429BridgeHandler 组装入口 handler：N=2 + 指定的跨请求冷却配置 + 可观察的存储。
// cooldownJSON 为空串表示后台从未写入该键（首次默认关闭）。
func newClaude429BridgeHandler(t *testing.T, group *service.Group, upstream service.HTTPUpstream, cooldownJSON string) (*GatewayHandler, *claude429TestStore, *openAI429MatrixSettingRepo) {
	t.Helper()
	h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
	settings := newOpenAI429MatrixSettingRepo(claude429BridgeLimit)
	if cooldownJSON != "" {
		require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, cooldownJSON))
	}
	h.settingService = service.NewSettingService(settings, &config.Config{RunMode: config.RunModeSimple})
	store := newClaude429TestStore()
	h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
	return h, store, settings
}

func claude429BridgeGroup(id int64) *service.Group {
	return &service.Group{ID: id, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
}

func TestGatewayClaude429CooldownBridgesShareScopeAndRespectFormat(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		body     string
		wantCode string
	}{
		{"responses", "/v1/responses", `{"model":"claude-sonnet-4-5","input":"hello","stream":false,"metadata":{"user_id":"user_` + claude429DeviceA + `_account__session_` + claude429SessionA + `"}}`, `"code":"claude_429_account_limit_cooldown"`},
		{"chat", "/v1/chat/completions", `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}],"stream":false,"metadata":{"user_id":"user_` + claude429DeviceA + `_account__session_` + claude429SessionA + `"}}`, `"type":"rate_limit_error"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &service.Group{ID: 9301, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			upstream := &gateway429AccountLimitUpstream{}
			h := newGateway429TestHandler(t, upstream, gateway429TestAccounts(group.ID, 3), group)
			settings := newOpenAI429MatrixSettingRepo(2)
			require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":true,"scope":"session","cooldown_seconds":60}`))
			h.settingService = service.NewSettingService(settings, &config.Config{RunMode: config.RunModeSimple})
			store := newClaude429TestStore()
			h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)

			first, _ := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "turn-1")
			h.Messages(first)
			require.Equal(t, []int64{1, 2}, upstream.hits)
			bridge, rec, cancel := claude429BridgeContext(t, group, 9008, tc.path, tc.body, claude429SessionA, "turn-2")
			defer cancel()
			if tc.name == "responses" {
				h.Responses(bridge)
			} else {
				h.ChatCompletions(bridge)
			}
			require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
			require.Equal(t, "60", rec.Header().Get("Retry-After"))
			require.Contains(t, rec.Body.String(), tc.wantCode)
			require.Contains(t, rec.Body.String(), `"code":"claude_429_account_limit_cooldown"`)
			require.Equal(t, []int64{1, 2}, upstream.hits, "bridge must reject before any upstream attempt")
		})
	}
}

// TestGatewayClaude429CooldownBridgesCapThenBlockSameIdentity 覆盖 Ticket 02 验收标准 1 的
// 每入口前半段：入口自身触顶 N=2 后，A 换 request ID 再请求被本地 429 拦截、零上游尝试。
func TestGatewayClaude429CooldownBridgesCapThenBlockSameIdentity(t *testing.T) {
	for _, entry := range claude429BridgeEntries() {
		t.Run(entry.name, func(t *testing.T) {
			group := claude429BridgeGroup(9310)
			upstream := &claude429BridgeUpstream{}
			h, store, _ := newClaude429BridgeHandler(t, group, upstream, claude429BridgeSessionScopeJSON)

			identity := claude429BridgeIdentity(claude429DeviceA, claude429SessionA)
			c1, rec1, cancel1 := claude429BridgeContext(t, group, 9008, entry.path, entry.body(identity), claude429SessionA, "turn-1")
			defer cancel1()
			entry.call(h, c1)
			require.Equal(t, http.StatusTooManyRequests, rec1.Code, rec1.Body.String())
			require.Equal(t, []int64{1, 2}, upstream.hits, "本入口第一次请求应在 2 个账号上触顶 N")
			require.Equal(t, 1, store.writes, "N 触顶必须写入本入口作用域的冷却")

			c2, rec2, cancel2 := claude429BridgeContext(t, group, 9008, entry.path, entry.body(identity), claude429SessionA, "turn-2")
			defer cancel2()
			entry.call(h, c2)
			require.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())
			require.Equal(t, "60", rec2.Header().Get("Retry-After"))
			require.Contains(t, rec2.Body.String(), claude429CooldownMessage, "必须是 B2 本地冷却，而不是上游耗尽的错误")
			require.Contains(t, rec2.Body.String(), entry.errorCode, "错误响应必须保持本入口的协议格式")
			require.Contains(t, rec2.Body.String(), `"code":"claude_429_account_limit_cooldown"`)
			require.Equal(t, []int64{1, 2}, upstream.hits, "命中本地冷却后不得再产生任何上游尝试")
			require.NotContains(t, rec2.Body.String(), claude429DeviceA)
			require.NotContains(t, rec2.Body.String(), claude429SessionA)
		})
	}
}

// TestGatewayClaude429CooldownBridgesKeepOtherDeviceOnSharedKey 覆盖验收标准 1 的后半段：
// 同一把 B1 Key 下另一台设备/会话不因 A 触顶而被新增冷却，且 A 的冷却仍然有效。
func TestGatewayClaude429CooldownBridgesKeepOtherDeviceOnSharedKey(t *testing.T) {
	for _, entry := range claude429BridgeEntries() {
		t.Run(entry.name, func(t *testing.T) {
			group := claude429BridgeGroup(9311)
			upstream := &claude429BridgeUpstream{}
			h, store, _ := newClaude429BridgeHandler(t, group, upstream, claude429BridgeSessionScopeJSON)

			prime := func(device, session, requestID string) (*httptest.ResponseRecorder, context.CancelFunc) {
				c, rec, cancel := claude429BridgeContext(t, group, 9008, entry.path, entry.body(claude429BridgeIdentity(device, session)), session, requestID)
				entry.call(h, c)
				return rec, cancel
			}

			recA1, cancelA1 := prime(claude429DeviceA, claude429SessionA, "a-1")
			defer cancelA1()
			require.Equal(t, http.StatusTooManyRequests, recA1.Code, recA1.Body.String())
			require.Equal(t, []int64{1, 2}, upstream.hits)
			require.Equal(t, 1, store.writes)

			recB, cancelB := prime(claude429DeviceB, claude429SessionB, "b-1")
			defer cancelB()
			require.Equal(t, http.StatusTooManyRequests, recB.Code, recB.Body.String())
			require.Empty(t, recB.Header().Get("Retry-After"), "同一 Key 的另一台设备不得命中本地冷却")
			require.NotContains(t, recB.Body.String(), claude429CooldownMessage)
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "B 仍按既有 N 规则访问上游")

			recA2, cancelA2 := prime(claude429DeviceA, claude429SessionA, "a-2")
			defer cancelA2()
			require.Equal(t, http.StatusTooManyRequests, recA2.Code, recA2.Body.String())
			require.Equal(t, "60", recA2.Header().Get("Retry-After"))
			require.Contains(t, recA2.Body.String(), claude429CooldownMessage)
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "B 的放行不得解除 A 的冷却")
		})
	}
}

// TestGatewayClaude429CooldownBridgesSkipIncompleteInboundIdentity 覆盖验收标准 2：
// 入站身份缺失/非法/无会话头/头体冲突时必须跳过新增冷却（不读不写、不拦截），原单请求
// 预算继续生效。冲突用例同时断言出站 wire 上确实带着一个 Claude 会话身份——该身份
// 只是被透传的客户端头，绝不是冷却作用域的来源，因此仍然不得写入任何冷却键。
func TestGatewayClaude429CooldownBridgesSkipIncompleteInboundIdentity(t *testing.T) {
	cases := []struct {
		name     string
		identity string
		header   string
	}{
		{"missing metadata", "", claude429SessionA},
		{"malformed metadata", "user_invalid-device_account__session_" + claude429SessionA, claude429SessionA},
		{"missing session header", claude429BridgeIdentity(claude429DeviceA, claude429SessionA), ""},
		{"conflicting session header", claude429BridgeIdentity(claude429DeviceA, claude429SessionA), claude429SessionB},
	}
	for _, entry := range claude429BridgeEntries() {
		for _, tc := range cases {
			t.Run(entry.name+"/"+tc.name, func(t *testing.T) {
				group := claude429BridgeGroup(9312)
				upstream := &claude429BridgeUpstream{}
				h, store, _ := newClaude429BridgeHandler(t, group, upstream, claude429BridgeSessionScopeJSON)

				for i := 0; i < 2; i++ {
					c, rec, cancel := claude429BridgeContext(t, group, 9008, entry.path, entry.body(tc.identity), tc.header, "request-"+strconv.Itoa(i))
					entry.call(h, c)
					cancel()
					require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
					require.Empty(t, rec.Header().Get("Retry-After"), "身份不完整时不得命中新增冷却")
					require.NotContains(t, rec.Body.String(), claude429CooldownMessage)
				}
				require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "身份无效时原单请求预算 N 必须继续生效")
				require.Zero(t, store.writes, "身份无效时不得写入冷却")
				require.Zero(t, store.reads, "身份无效时不得读取冷却（不得退化成 Key 级或空作用域）")
				require.Contains(t, string(upstream.outbound[0]), "hello",
					"身份无效只跳过新增冷却：请求仍必须完成协议转换并到达上游")

				if tc.header == "" {
					// 本夹具是 API-key 账号，入口不做 OAuth 伪装：客户端没带会话头时出站也不会凭空出现一个。
					require.Empty(t, upstream.headers[0].Get("X-Claude-Code-Session-Id"))
					return
				}
				// 转换前的入站头原样出现在出站 wire 上：冷却不能以它（或转换合成值）为键。
				for i, header := range upstream.headers {
					require.Equal(t, tc.header, header.Get("X-Claude-Code-Session-Id"),
						"出站 wire 上的会话身份只来自入站头，第 %d 次上游调用", i+1)
				}
			})
		}
	}
}

// TestGatewayClaude429CooldownBridgesSkipWriteOnCommittedStreamAndCancel 覆盖验收标准 2/4 的
// 负控：响应流已经提交（无法再回退为 HTTP 429）或客户端取消时不得写入跨请求冷却。
// 已提交流由假上游在调用期间写入客户端 writer 模拟（与 Forward 已 flush 部分 SSE 后
// 才拿到 failover error 的守卫条件等价：c.Writer.Size() 相对 Forward 前发生变化）。
func TestGatewayClaude429CooldownBridgesSkipWriteOnCommittedStreamAndCancel(t *testing.T) {
	for _, entry := range claude429BridgeEntries() {
		t.Run(entry.name+"/stream-committed", func(t *testing.T) {
			group := claude429BridgeGroup(9313)
			upstream := &claude429BridgeUpstream{}
			h, store, _ := newClaude429BridgeHandler(t, group, upstream, claude429BridgeSessionScopeJSON)
			identity := claude429BridgeIdentity(claude429DeviceA, claude429SessionA)

			c1, _, cancel1 := claude429BridgeContext(t, group, 9008, entry.path, entry.body(identity), claude429SessionA, "turn-1")
			defer cancel1()
			upstream.onCall = func() {
				_, _ = c1.Writer.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
			}
			entry.call(h, c1)
			require.Equal(t, []int64{1}, upstream.hits, "流已提交时必须终止 failover，不再尝试下一个账号")
			require.Zero(t, store.writes, "流已提交时不得写入冷却")

			// 未写入 → 同一身份的下一次请求必须继续按原 N 规则访问上游，而不是被本地拦截。
			upstream.onCall = nil
			c2, rec2, cancel2 := claude429BridgeContext(t, group, 9008, entry.path, entry.body(identity), claude429SessionA, "turn-2")
			defer cancel2()
			entry.call(h, c2)
			require.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())
			require.Empty(t, rec2.Header().Get("Retry-After"))
			require.NotContains(t, rec2.Body.String(), claude429CooldownMessage)
			require.Equal(t, []int64{1, 1, 2}, upstream.hits)
		})

		t.Run(entry.name+"/client-cancel", func(t *testing.T) {
			group := claude429BridgeGroup(9314)
			upstream := &claude429BridgeUpstream{}
			h, store, _ := newClaude429BridgeHandler(t, group, upstream, claude429BridgeSessionScopeJSON)
			identity := claude429BridgeIdentity(claude429DeviceA, claude429SessionA)

			c1, _, cancel1 := claude429BridgeContext(t, group, 9008, entry.path, entry.body(identity), claude429SessionA, "turn-1")
			upstream.onCall = cancel1
			entry.call(h, c1)
			require.Equal(t, []int64{1}, upstream.hits, "客户端已断开时不得继续换号")
			require.Zero(t, store.writes, "客户端取消时不得写入冷却")

			upstream.onCall = nil
			c2, rec2, cancel2 := claude429BridgeContext(t, group, 9008, entry.path, entry.body(identity), claude429SessionA, "turn-2")
			defer cancel2()
			entry.call(h, c2)
			require.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())
			require.Empty(t, rec2.Header().Get("Retry-After"))
			require.NotContains(t, rec2.Body.String(), claude429CooldownMessage)
			require.Equal(t, []int64{1, 1, 2}, upstream.hits)
		})
	}
}

// TestGatewayClaude429CooldownBridgesShareScopeWithNativeMessages 覆盖验收标准 3：
// 桥接入口与原生 /v1/messages 共用同一冷却作用域与同一后台粒度配置。
func TestGatewayClaude429CooldownBridgesShareScopeWithNativeMessages(t *testing.T) {
	// 方向一：入口自身触顶后，原生 Messages 的同一身份命中同一作用域。
	for _, entry := range claude429BridgeEntries() {
		t.Run(entry.name+"/entry-then-native", func(t *testing.T) {
			group := claude429BridgeGroup(9320)
			upstream := &claude429BridgeUpstream{}
			h, store, _ := newClaude429BridgeHandler(t, group, upstream, claude429BridgeSessionScopeJSON)

			c1, rec1, cancel1 := claude429BridgeContext(t, group, 9008, entry.path, entry.body(claude429BridgeIdentity(claude429DeviceA, claude429SessionA)), claude429SessionA, "turn-1")
			defer cancel1()
			entry.call(h, c1)
			require.Equal(t, http.StatusTooManyRequests, rec1.Code, rec1.Body.String())
			require.Equal(t, []int64{1, 2}, upstream.hits)
			require.Equal(t, 1, store.writes)

			native, recNative := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "turn-2")
			h.Messages(native)
			require.Equal(t, http.StatusTooManyRequests, recNative.Code, recNative.Body.String())
			require.Equal(t, "60", recNative.Header().Get("Retry-After"))
			require.Contains(t, recNative.Body.String(), claude429CooldownMessage)
			require.Equal(t, []int64{1, 2}, upstream.hits, "共享作用域下原生 Messages 也必须零上游尝试")
		})
	}

	// 方向二：原生 Messages 触顶后，两个粒度在入口上的命中差异与 01 完全一致。
	for _, entry := range claude429BridgeEntries() {
		for _, tc := range []struct {
			name    string
			scope   string
			device  string
			session string
			blocked bool
		}{
			{"session scope new session", "session", claude429DeviceA, claude429SessionB, false},
			{"device scope new session", "device", claude429DeviceA, claude429SessionB, true},
			{"device scope other device", "device", claude429DeviceB, claude429SessionB, false},
		} {
			t.Run(entry.name+"/native-then-entry/"+tc.name, func(t *testing.T) {
				group := claude429BridgeGroup(9321)
				upstream := &claude429BridgeUpstream{}
				h, store, _ := newClaude429BridgeHandler(t, group, upstream, `{"enabled":true,"scope":"`+tc.scope+`","cooldown_seconds":60}`)

				native, recNative := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "turn-1")
				h.Messages(native)
				require.Equal(t, http.StatusTooManyRequests, recNative.Code, recNative.Body.String())
				require.Equal(t, []int64{1, 2}, upstream.hits)
				require.Equal(t, 1, store.writes)

				c2, rec2, cancel2 := claude429BridgeContext(t, group, 9008, entry.path, entry.body(claude429BridgeIdentity(tc.device, tc.session)), tc.session, "turn-2")
				defer cancel2()
				entry.call(h, c2)
				require.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())
				if tc.blocked {
					require.Equal(t, "60", rec2.Header().Get("Retry-After"))
					require.Contains(t, rec2.Body.String(), claude429CooldownMessage)
					require.Equal(t, []int64{1, 2}, upstream.hits, "同一粒度下必须零上游尝试")
				} else {
					require.Empty(t, rec2.Header().Get("Retry-After"))
					require.NotContains(t, rec2.Body.String(), claude429CooldownMessage)
					require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits)
				}
			})
		}
	}
}

// TestGatewayClaude429CooldownBridgesSwitchOffAndReenable 覆盖验收标准 5 在桥接入口的部分：
// 首次默认关闭（键缺失）时完全不读不写、保持原单请求 N；关闭后不再命中；同粒度且 TTL
// 未到期的键在重新启用后恢复命中，Retry-After 为剩余秒数。
func TestGatewayClaude429CooldownBridgesSwitchOffAndReenable(t *testing.T) {
	for _, entry := range claude429BridgeEntries() {
		t.Run(entry.name+"/default-off", func(t *testing.T) {
			group := claude429BridgeGroup(9322)
			upstream := &claude429BridgeUpstream{}
			// 后台从未写入冷却键 → 首次默认关闭。
			h, store, _ := newClaude429BridgeHandler(t, group, upstream, "")
			identity := claude429BridgeIdentity(claude429DeviceA, claude429SessionA)
			for i := 0; i < 2; i++ {
				c, rec, cancel := claude429BridgeContext(t, group, 9008, entry.path, entry.body(identity), claude429SessionA, "turn-"+strconv.Itoa(i))
				entry.call(h, c)
				cancel()
				require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
				require.Empty(t, rec.Header().Get("Retry-After"))
				require.NotContains(t, rec.Body.String(), claude429CooldownMessage)
			}
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "默认关闭时仅原单请求 N 生效")
			require.Zero(t, store.reads)
			require.Zero(t, store.writes)
		})

		t.Run(entry.name+"/off-then-on", func(t *testing.T) {
			group := claude429BridgeGroup(9323)
			upstream := &claude429BridgeUpstream{}
			h, store, settings := newClaude429BridgeHandler(t, group, upstream, claude429BridgeSessionScopeJSON)
			identity := claude429BridgeIdentity(claude429DeviceA, claude429SessionA)
			request := func(requestID string) *httptest.ResponseRecorder {
				c, rec, cancel := claude429BridgeContext(t, group, 9008, entry.path, entry.body(identity), claude429SessionA, requestID)
				entry.call(h, c)
				cancel()
				return rec
			}

			require.Equal(t, http.StatusTooManyRequests, request("turn-1").Code)
			require.Equal(t, []int64{1, 2}, upstream.hits)
			require.Equal(t, 1, store.writes)

			// 后台关闭：已写的键不再命中，N 照常生效。
			require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, `{"enabled":false,"scope":"session","cooldown_seconds":60}`))
			recOff := request("turn-2")
			require.Equal(t, http.StatusTooManyRequests, recOff.Code, recOff.Body.String())
			require.Empty(t, recOff.Header().Get("Retry-After"))
			require.NotContains(t, recOff.Body.String(), claude429CooldownMessage)
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits)
			require.Equal(t, 1, store.writes, "关闭时不得写入新记录")

			// 重新启用同一粒度：TTL 未到期的旧键恢复命中，Retry-After 为剩余秒数。
			require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, claude429BridgeSessionScopeJSON))
			recOn := request("turn-3")
			require.Equal(t, http.StatusTooManyRequests, recOn.Code, recOn.Body.String())
			remaining, err := strconv.Atoi(recOn.Header().Get("Retry-After"))
			require.NoError(t, err, "重新启用后必须命中并返回剩余秒数")
			require.GreaterOrEqual(t, remaining, 1)
			require.LessOrEqual(t, remaining, 60)
			require.Contains(t, recOn.Body.String(), claude429CooldownMessage)
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "恢复命中后不得再访问上游")
		})
	}
}
