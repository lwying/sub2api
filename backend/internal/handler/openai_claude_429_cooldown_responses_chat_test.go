//go:build unit

package handler

// Ticket 03 — OpenAI 平台 handler 的 Responses / ChatCompletions 入口的条件冷却覆盖。
//
// 覆盖入口：OpenAIGatewayHandler.Responses（/v1/responses）与
// OpenAIGatewayHandler.ChatCompletions（/v1/chat/completions）。两者已有单次请求
// 429 账号上限（本文件用 N=2）与 OpenAI 平台路由分派；本票要求它们在入站 OpenAI
// JSON 同时携带完整且一致的原始 Claude 身份（正文 metadata.user_id + 与之匹配的
// X-Claude-Code-Session-Id）时，沿用与既有 Messages 入口相同的跨请求冷却作用域；
// 缺少这份原始身份的普通 OpenAI 客户端按旧流程继续（只受已有 N 与上游账号回避）。
//
// 断言口径全部落在外部可见行为上：本地 429 与本入口协议的错误体、Retry-After
// 剩余秒数、上游实际被调用的账号序列，以及冷却存储的读写次数（身份缺失/不一致
// 时必须不读不写、不退化成 Key 级冷却）。夹具复用 Ticket 02 建立的
// newOpenAI429MatrixEnv / openAI429MatrixUpstream / openAI429MatrixContext /
// newOpenAI429MatrixSettingRepo / claude429TestStore 及 claude429DeviceA/B、
// claude429SessionA/B 常量，不新增平行夹具。
//
// 不覆盖：count_tokens（票面明确不读不写该冷却，且不属于本文件的消息入口）、
// ResponsesWebSocket（需要真实上游 WS 服务端）。出站身份收敛（off/device/session/full）
// 属于另一维度，本文件只断言冷却键取自入站身份。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	// claude429OpenAIEntryLimit 与既有夹具一致：每个逻辑请求允许 2 个不同上游账号 429。
	claude429OpenAIEntryLimit = 2
	// claude429OpenAIEntrySessionScopeJSON 是 Ticket 01 同一个后台控件的取值。
	claude429OpenAIEntrySessionScopeJSON = `{"enabled":true,"scope":"session","cooldown_seconds":60}`
)

// claude429OpenAIEntry 描述一个 OpenAI 平台承接的 Claude 兼容消息入口。
type claude429OpenAIEntry struct {
	name string
	path string
	call func(h *OpenAIGatewayHandler, c *gin.Context)
	// group 每次返回同 ID 的新分组，避免子用例之间共享可变分组对象。
	group func() *service.Group
	// body 生成入站请求体；identity 为空串表示正文完全没有 metadata.user_id。
	body func(identity string) string
}

func claude429OpenAIEntries() []claude429OpenAIEntry {
	return []claude429OpenAIEntry{
		{
			name:  "responses",
			path:  "/v1/responses",
			call:  func(h *OpenAIGatewayHandler, c *gin.Context) { h.Responses(c) },
			group: func() *service.Group { return openAI429MatrixGroup(4291) },
			body:  claude429OpenAIEntryResponsesBody,
		},
		{
			name:  "chat_completions",
			path:  "/v1/chat/completions",
			call:  func(h *OpenAIGatewayHandler, c *gin.Context) { h.ChatCompletions(c) },
			group: func() *service.Group { return openAI429MatrixGroup(4292) },
			body:  claude429OpenAIEntryChatCompletionsBody,
		},
	}
}

// claude429OpenAIEntryIdentity 生成与真实 Claude Code 客户端一致的原始
// metadata.user_id（legacy 形态，与 Ticket 01/02 夹具同名同形）。
func claude429OpenAIEntryIdentity(deviceID, sessionID string) string {
	return "user_" + deviceID + "_account__session_" + sessionID
}

// claude429OpenAIEntryResponsesBody 生成 /v1/responses 入站正文。
func claude429OpenAIEntryResponsesBody(identity string) string {
	body := `{"model":"gpt-5.1","stream":false,"input":"hello"`
	if identity != "" {
		body += `,"metadata":{"user_id":"` + identity + `"}`
	}
	return body + `}`
}

// claude429OpenAIEntryChatCompletionsBody 生成 /v1/chat/completions 入站正文。
func claude429OpenAIEntryChatCompletionsBody(identity string) string {
	body := `{"model":"gpt-5.1","stream":false,"messages":[{"role":"user","content":"hi"}]`
	if identity != "" {
		body += `,"metadata":{"user_id":"` + identity + `"}`
	}
	return body + `}`
}

// newClaude429OpenAIEntryEnv 组装 N=2 + 会话级 60 秒跨请求冷却的入口 handler，
// 并返回可观察的冷却存储。
func newClaude429OpenAIEntryEnv(t *testing.T, upstream service.HTTPUpstream) (*openAI429MatrixEnv, *claude429TestStore) {
	t.Helper()
	env := newOpenAI429MatrixEnv(t, openAI429MatrixAPIKeyAccounts(3, service.AccountTypeAPIKey), upstream, claude429OpenAIEntryLimit)
	settings := newOpenAI429MatrixSettingRepo(claude429OpenAIEntryLimit)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, claude429OpenAIEntrySessionScopeJSON))
	store := newClaude429TestStore()
	env.handler.claude429Cooldown = service.NewClaude429CooldownGate(
		service.NewSettingService(settings, &config.Config{RunMode: config.RunModeSimple}),
		store,
	)
	return env, store
}

// claude429OpenAIEntryRequest 构造并执行一次入口请求：入站正文 + Claude 会话头 +
// 独立 request ID（用于证明换 request ID 不影响冷却判定）。
func claude429OpenAIEntryRequest(t *testing.T, entry claude429OpenAIEntry, env *openAI429MatrixEnv, body, session, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := openAI429MatrixContext(t, openAI429MatrixAPIKey(entry.group()), entry.path, []byte(body), nil)
	if session != "" {
		c.Request.Header.Set("X-Claude-Code-Session-Id", session)
	}
	c.Request.Header.Set("X-Request-Id", requestID)
	entry.call(env.handler, c)
	return rec
}

// claude429OpenAIEntryMessagesRequest 执行一次 OpenAI 平台承接的 /v1/messages 请求，
// 用于验证同一 API Key + 同一入站身份在各消息入口之间共享同一个冷却作用域。
func claude429OpenAIEntryMessagesRequest(t *testing.T, env *openAI429MatrixEnv, body, session, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	group := openAI429MatrixGroup(4293)
	group.AllowMessagesDispatch = true
	c, rec := openAI429MatrixContext(t, openAI429MatrixAPIKey(group), "/v1/messages", []byte(body), nil)
	if session != "" {
		c.Request.Header.Set("X-Claude-Code-Session-Id", session)
	}
	c.Request.Header.Set("X-Request-Id", requestID)
	env.handler.Messages(c)
	return rec
}

// claude429OpenAIEntryMessagesBody 是 /v1/messages 入站正文（Anthropic 形态）。
func claude429OpenAIEntryMessagesBody(deviceID, sessionID string) string {
	return `{"model":"gpt-5.1","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"` +
		claude429OpenAIEntryIdentity(deviceID, sessionID) + `"}}`
}

// ---------------------------------------------------------------------------
// 正例：A 触顶后换 request ID 命中本入口的本地冷却；共用 Key 的 B 放行
// ---------------------------------------------------------------------------

// TestOpenAIClaude429CooldownResponsesAndChatCompletionsSameIdentity 覆盖 Ticket 03
// 验收标准 1 的每入口正例：入口自身在 2 个账号上触顶 N 后写入会话级冷却，A 换
// request ID 再请求被本地 429 拦截且零上游尝试、Retry-After 为剩余秒数；同一把
// Key 下不同 device/session 的 B 不命中该冷却（仍按既有 N 访问上游），且 B 的放行
// 不解除 A 的冷却。
func TestOpenAIClaude429CooldownResponsesAndChatCompletionsSameIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, entry := range claude429OpenAIEntries() {
		t.Run(entry.name, func(t *testing.T) {
			upstream := &openAI429MatrixUpstream{}
			env, store := newClaude429OpenAIEntryEnv(t, upstream)
			bodyA := entry.body(claude429OpenAIEntryIdentity(claude429DeviceA, claude429SessionA))

			recA1 := claude429OpenAIEntryRequest(t, entry, env, bodyA, claude429SessionA, "turn-1")
			require.Equal(t, http.StatusTooManyRequests, recA1.Code, recA1.Body.String())
			require.Equal(t, []int64{1, 2}, upstream.calls(), "本入口第一次请求应在 2 个账号上触顶 N")
			require.Equal(t, 1, store.writes, "N 触顶必须写入本入口入站身份的冷却键")

			recA2 := claude429OpenAIEntryRequest(t, entry, env, bodyA, claude429SessionA, "turn-2")
			require.Equal(t, http.StatusTooManyRequests, recA2.Code, recA2.Body.String())
			require.Equal(t, "60", recA2.Header().Get("Retry-After"))
			require.Contains(t, recA2.Body.String(), claude429CooldownMessage, "必须是本地冷却，而不是上游耗尽的错误")
			require.Equal(t, []int64{1, 2}, upstream.calls(), "同一身份换 request ID 后必须零上游尝试")
			require.NotContains(t, recA2.Body.String(), claude429DeviceA)
			require.NotContains(t, recA2.Body.String(), claude429SessionA)

			bodyB := entry.body(claude429OpenAIEntryIdentity(claude429DeviceB, claude429SessionB))
			recB := claude429OpenAIEntryRequest(t, entry, env, bodyB, claude429SessionB, "turn-3")
			require.Equal(t, http.StatusTooManyRequests, recB.Code, recB.Body.String())
			require.Empty(t, recB.Header().Get("Retry-After"), "同一把 Key 下的另一台设备不得命中本地冷却")
			require.NotContains(t, recB.Body.String(), claude429CooldownMessage)
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.calls(), "B 仍按既有 N 规则访问上游")

			recA3 := claude429OpenAIEntryRequest(t, entry, env, bodyA, claude429SessionA, "turn-4")
			require.Equal(t, http.StatusTooManyRequests, recA3.Code, recA3.Body.String())
			require.Equal(t, "60", recA3.Header().Get("Retry-After"))
			require.Equal(t, []int64{1, 2, 1, 2}, upstream.calls(), "B 的放行不得解除 A 的冷却")
		})
	}
}

// ---------------------------------------------------------------------------
// 负例：缺少完整、头体一致的原始入站身份时跳过新增冷却
// ---------------------------------------------------------------------------

// TestOpenAIClaude429CooldownResponsesAndChatCompletionsSkipWithoutOriginalIdentity
// 覆盖 Ticket 03 验收标准 2：入站身份缺失、非法、无会话头或头体冲突时，入口必须
// 跳过新增冷却——不读不写冷却存储、不返回本地 429，原有单请求预算 N 继续生效。
// 现有落点是刻意不把「只有会话头」或「正文单方面声明」当成可用身份。
func TestOpenAIClaude429CooldownResponsesAndChatCompletionsSkipWithoutOriginalIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name     string
		identity string
		header   string
	}{
		{"missing metadata", "", claude429SessionA},
		{"malformed metadata", "user_invalid-device_account__session_" + claude429SessionA, claude429SessionA},
		{"missing session header", claude429OpenAIEntryIdentity(claude429DeviceA, claude429SessionA), ""},
		{"conflicting session header", claude429OpenAIEntryIdentity(claude429DeviceA, claude429SessionA), claude429SessionB},
	}

	for _, entry := range claude429OpenAIEntries() {
		for _, tc := range cases {
			t.Run(entry.name+"/"+tc.name, func(t *testing.T) {
				upstream := &openAI429MatrixUpstream{}
				env, store := newClaude429OpenAIEntryEnv(t, upstream)
				body := entry.body(tc.identity)

				for i := 0; i < 2; i++ {
					rec := claude429OpenAIEntryRequest(t, entry, env, body, tc.header, "turn-"+strconv.Itoa(i))
					require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
					require.Empty(t, rec.Header().Get("Retry-After"), "身份不完整时不得命中新增冷却")
					require.NotContains(t, rec.Body.String(), claude429CooldownMessage)
				}
				require.Equal(t, []int64{1, 2, 1, 2}, upstream.calls(), "身份无效时原单请求预算 N 必须继续生效")
				require.Zero(t, store.writes, "身份无效时不得写入冷却")
				require.Zero(t, store.reads, "身份无效时不得读取冷却（不得退化成 Key 级或空作用域）")
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 负例：出站 wire 上出现的 Claude 身份不得成为冷却键
// ---------------------------------------------------------------------------

// claude429OpenAIEntryWireUpstream 是记录出站 wire（正文与请求头）的 429 脚本上游：
// 复用 openAI429MatrixUpstream 的 429/成功响应脚本与账号调用序列记录。
type claude429OpenAIEntryWireUpstream struct {
	openAI429MatrixUpstream

	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
}

func (u *claude429OpenAIEntryWireUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.mu.Lock()
	if req != nil {
		if req.Body != nil {
			if raw, err := io.ReadAll(req.Body); err == nil {
				u.bodies = append(u.bodies, raw)
			}
		}
		u.headers = append(u.headers, req.Header.Clone())
	}
	u.mu.Unlock()
	return u.openAI429MatrixUpstream.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *claude429OpenAIEntryWireUpstream) outboundBodies() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

func (u *claude429OpenAIEntryWireUpstream) outboundHeaders() []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]http.Header(nil), u.headers...)
}

// TestOpenAIClaude429CooldownResponsesAndChatCompletionsIgnoreOutboundWireIdentity
// 覆盖 Ticket 03 验收标准 2 的对照：普通 OpenAI 客户端只在头里带了 Claude 会话标识、
// 正文没有任何 metadata.user_id 时，即使出站 wire 上确实出现了这个 Claude 身份
// （伪装/协议转换可以在出站合成或透传身份），它也不得成为冷却键——断言不读不写、
// 不拦截、N 照旧。冷却身份只能取自鉴权后的原始入站正文。
func TestOpenAIClaude429CooldownResponsesAndChatCompletionsIgnoreOutboundWireIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, entry := range claude429OpenAIEntries() {
		t.Run(entry.name, func(t *testing.T) {
			upstream := &claude429OpenAIEntryWireUpstream{}
			env, store := newClaude429OpenAIEntryEnv(t, upstream)
			// 入站正文没有原始 Claude 身份，只有会话头（普通 OpenAI/第三方客户端的形态）。
			body := entry.body("")

			for i := 0; i < 2; i++ {
				rec := claude429OpenAIEntryRequest(t, entry, env, body, claude429SessionA, "turn-"+strconv.Itoa(i))
				require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
				require.Empty(t, rec.Header().Get("Retry-After"))
				require.NotContains(t, rec.Body.String(), claude429CooldownMessage)
			}

			require.Equal(t, []int64{1, 2, 1, 2}, upstream.calls(), "无原始入站身份时只受既有 N 约束")
			require.Zero(t, store.reads, "无原始入站身份时不得读取冷却（出站合成身份不是冷却键）")
			require.Zero(t, store.writes, "无原始入站身份时不得写入冷却")

			wireHeaders := upstream.outboundHeaders()
			require.Len(t, wireHeaders, 4, "断言必须建立在真实的上游尝试上")
			for i, wireBody := range upstream.outboundBodies() {
				wireIdentity := gjson.GetBytes(wireBody, "metadata.user_id").String()
				if wireIdentity == "" {
					wireIdentity = wireHeaders[i].Get("X-Claude-Code-Session-Id")
				}
				t.Logf("outbound attempt %d carried claude wire identity %q", i+1, wireIdentity)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 跨入口作用域：与 OpenAI 平台承接的 Messages 入口共享同一冷却键空间
// ---------------------------------------------------------------------------

// TestOpenAIClaude429CooldownResponsesAndChatCompletionsShareScopeWithMessages 覆盖
// Ticket 03 验收标准 3 在 OpenAI 平台入口的部分：/v1/messages 触顶写入的冷却，对
// 同一 API Key、同一入站身份的 Responses / ChatCompletions 入口立即生效且零上游尝试。
func TestOpenAIClaude429CooldownResponsesAndChatCompletionsShareScopeWithMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, entry := range claude429OpenAIEntries() {
		t.Run(entry.name, func(t *testing.T) {
			upstream := &openAI429MatrixUpstream{}
			env, store := newClaude429OpenAIEntryEnv(t, upstream)

			recMessages := claude429OpenAIEntryMessagesRequest(t, env,
				claude429OpenAIEntryMessagesBody(claude429DeviceA, claude429SessionA), claude429SessionA, "turn-1")
			require.Equal(t, http.StatusTooManyRequests, recMessages.Code, recMessages.Body.String())
			require.Equal(t, []int64{1, 2}, upstream.calls())
			require.Equal(t, 1, store.writes)

			recEntry := claude429OpenAIEntryRequest(t, entry, env,
				entry.body(claude429OpenAIEntryIdentity(claude429DeviceA, claude429SessionA)), claude429SessionA, "turn-2")
			require.Equal(t, http.StatusTooManyRequests, recEntry.Code, recEntry.Body.String())
			require.Equal(t, "60", recEntry.Header().Get("Retry-After"))
			require.Contains(t, recEntry.Body.String(), claude429CooldownMessage,
				"跨入口共享作用域时本入口必须返回自己的本地冷却错误")
			require.Equal(t, []int64{1, 2}, upstream.calls(), "共享作用域下本入口必须零上游尝试")
		})
	}
}
