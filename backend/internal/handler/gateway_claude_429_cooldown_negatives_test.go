//go:build unit

package handler

// Ticket 01 — /v1/messages 原生入口的跨请求冷却「精确触发负控」。
//
// 本文件只覆盖「N 并未真正触顶」时不得写入跨请求冷却这一类行为，断言全部落在真实
// Messages handler HTTP 入口 + 假上游上：上游被调用的账号序列、客户端可见状态码与
// Retry-After、错误体，以及冷却存储的读写次数。
//
// 已由其它文件覆盖、本文不重复的：触顶后同身份被本地 429 拦截、共用 Key 的另一台设备
// 放行、粒度差异（gateway_claude_429_cooldown_test.go）、开关默认关闭与 TTL 生命周期
// （gateway_claude_429_cooldown_lifecycle_test.go）、桥接入口的同类负控
// （gateway_claude_429_cooldown_bridges_test.go）、响应体不泄漏原始标识
// （gateway_claude_429_cooldown_test.go）。
//
// 夹具复用 gateway_429_account_limit_test.go 的 newGateway429TestHandler /
// gateway429TestAccounts / newOpenAI429MatrixSettingRepo，以及
// gateway_claude_429_cooldown_test.go 的 claude429TestStore / claude429DeviceA 等常量。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// claude429NegSessionScopeJSON 是 Ticket 01 同一个后台控件的会话级取值（60 秒）。
const claude429NegSessionScopeJSON = `{"enabled":true,"scope":"session","cooldown_seconds":60}`

// claude429NegUpstream 是按「第 n 次上游调用」脚本化返回状态码的假上游。
// statuses 不足时沿用最后一个状态码；onCall 允许用例注入「本次上游调用期间发生的事」
// （模拟响应已经提交，或客户端取消请求）。
type claude429NegUpstream struct {
	service.HTTPUpstream
	statuses []int
	hits     []int64
	// okBody 是成功响应体；空串时返回一条合法的非流式 message。
	okBody string
	onCall func()
}

func (u *claude429NegUpstream) DoWithTLS(_ *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.hits = append(u.hits, accountID)
	call := len(u.hits) - 1
	if u.onCall != nil {
		u.onCall()
	}
	status := http.StatusTooManyRequests
	if len(u.statuses) > 0 {
		status = u.statuses[len(u.statuses)-1]
		if call < len(u.statuses) {
			status = u.statuses[call]
		}
	}
	switch status {
	case http.StatusOK:
		body := u.okBody
		if body == "" {
			body = `{"id":"msg_neg","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
		}
		return claude429NegResponse(status, body), nil
	case http.StatusInternalServerError:
		return claude429NegResponse(status, `{"type":"error","error":{"type":"api_error","message":"upstream boom"}}`), nil
	default:
		return claude429NegResponse(status, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`), nil
	}
}

func claude429NegResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

// claude429NegMessagesBody 生成与真实 Claude Code 客户端一致的入站正文：
// legacy 形态 metadata.user_id（原始 device_id + 正文 session_id）。
func claude429NegMessagesBody(deviceID, sessionID string) string {
	return `{"model":"claude-sonnet-4-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"user_` +
		deviceID + `_account__session_` + sessionID + `"}}`
}

// claude429NegContext 构造一个身份完整（正文 metadata.user_id + 匹配的 Claude 会话头）
// 且可取消的入口请求。
func claude429NegContext(t *testing.T, group *service.Group, keyID int64, path, body, headerSession, requestID string) (*gin.Context, *httptest.ResponseRecorder, context.CancelFunc) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", headerSession)
	req.Header.Set("X-Request-Id", requestID)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = req.WithContext(context.WithValue(ctx, ctxkey.Group, group))
	groupID := group.ID
	apiKey := &service.APIKey{ID: keyID, UserID: 10, GroupID: &groupID, Status: service.StatusActive, User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 10, Concurrency: 10})
	return c, rec, cancel
}

// newClaude429NegHandler 组装「N=limit + 会话级跨请求冷却开启 + 可观察存储」的
// Anthropic 主网关 handler。
func newClaude429NegHandler(t *testing.T, group *service.Group, accounts []*service.Account, upstream service.HTTPUpstream, limit int) (*GatewayHandler, *claude429TestStore) {
	t.Helper()
	h := newGateway429TestHandler(t, upstream, accounts, group)
	settings := newOpenAI429MatrixSettingRepo(limit)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown, claude429NegSessionScopeJSON))
	h.settingService = service.NewSettingService(settings, h.cfg)
	store := newClaude429TestStore()
	h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, store)
	return h, store
}

func claude429NegGroup(id int64) *service.Group {
	return &service.Group{ID: id, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
}

// TestGatewayClaude429CooldownNegativesNoWriteWhenNextAccountSucceeds 覆盖「429 → 下个账号成功」：
// 逻辑请求最终成功，绝不能写入跨请求冷却；随后同一身份的下一次请求仍可正常访问上游。
func TestGatewayClaude429CooldownNegativesNoWriteWhenNextAccountSucceeds(t *testing.T) {
	group := claude429NegGroup(9401)
	upstream := &claude429NegUpstream{statuses: []int{http.StatusTooManyRequests, http.StatusOK}}
	h, store := newClaude429NegHandler(t, group, gateway429TestAccounts(group.ID, 3), upstream, 2)
	body := claude429NegMessagesBody(claude429DeviceA, claude429SessionA)

	for i := 0; i < 2; i++ {
		c, rec, cancel := claude429NegContext(t, group, 9008, "/v1/messages", body, claude429SessionA, "turn-"+strconv.Itoa(i))
		h.Messages(c)
		cancel()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Empty(t, rec.Header().Get("Retry-After"), "成功结束的逻辑请求不得被本地冷却命中")
		require.NotContains(t, rec.Body.String(), claude429CooldownMessage)
	}
	// 第一次请求：账号 1 的 429 在 N=2 内换到账号 2 成功；第二次请求继续访问上游（账号 1 成功）。
	require.Equal(t, []int64{1, 2, 1}, upstream.hits)
	require.Zero(t, store.writes, "429 后换号成功不得写入跨请求冷却")
}

// TestGatewayClaude429CooldownNegativesNoWriteOn5xx 覆盖 5xx 失败：
// 非 429 失败既不计入 N，也不得写入跨请求冷却；同一身份的下一次请求仍按既有规则访问上游。
func TestGatewayClaude429CooldownNegativesNoWriteOn5xx(t *testing.T) {
	group := claude429NegGroup(9402)
	upstream := &claude429NegUpstream{statuses: []int{http.StatusInternalServerError}}
	h, store := newClaude429NegHandler(t, group, gateway429TestAccounts(group.ID, 2), upstream, 2)
	body := claude429NegMessagesBody(claude429DeviceA, claude429SessionA)

	for i := 0; i < 2; i++ {
		c, rec, cancel := claude429NegContext(t, group, 9008, "/v1/messages", body, claude429SessionA, "turn-"+strconv.Itoa(i))
		h.Messages(c)
		cancel()
		require.NotEqual(t, http.StatusOK, rec.Code)
		require.Empty(t, rec.Header().Get("Retry-After"), "5xx 耗尽不得被伪装成本地冷却")
		require.NotContains(t, rec.Body.String(), claude429CooldownMessage)
	}
	require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "5xx 不得让下一次请求在入口就被跳过")
	require.Zero(t, store.writes, "5xx 不得写入跨请求冷却")
}

// TestGatewayClaude429CooldownNegativesNoWriteWhenSwitchCapExhaustsBeforeN 覆盖
// 「总换号上限先于 N 触发」：N=3 但本次请求只允许 1 次换号时，停止换号的原因是总换号
// 上限而不是 429 账号上限，绝不能写入冷却；下一次请求必须重新扫描账号。
func TestGatewayClaude429CooldownNegativesNoWriteWhenSwitchCapExhaustsBeforeN(t *testing.T) {
	group := claude429NegGroup(9403)
	upstream := &claude429NegUpstream{}
	h, store := newClaude429NegHandler(t, group, gateway429TestAccounts(group.ID, 3), upstream, 3)
	h.maxAccountSwitches = 1
	body := claude429NegMessagesBody(claude429DeviceA, claude429SessionA)

	for i := 0; i < 2; i++ {
		c, rec, cancel := claude429NegContext(t, group, 9008, "/v1/messages", body, claude429SessionA, "turn-"+strconv.Itoa(i))
		h.Messages(c)
		cancel()
		require.Empty(t, rec.Header().Get("Retry-After"), "换号上限触发不得被当作 N 触顶")
		require.NotContains(t, rec.Body.String(), claude429CooldownMessage)
	}
	require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "换号上限耗尽只终止本次逻辑请求，不得冷却下一次")
	require.Zero(t, store.writes, "总换号上限先触发时不得写入冷却")
}

// TestGatewayClaude429CooldownNegativesNoWriteWhenStreamAlreadyWritten 覆盖「流已写出」：
// 响应内容已经发给客户端后无法再改成本地 429，此时既不得写入冷却，也不得继续换号。
func TestGatewayClaude429CooldownNegativesNoWriteWhenStreamAlreadyWritten(t *testing.T) {
	group := claude429NegGroup(9404)
	upstream := &claude429NegUpstream{}
	h, store := newClaude429NegHandler(t, group, gateway429TestAccounts(group.ID, 3), upstream, 2)
	body := claude429NegMessagesBody(claude429DeviceA, claude429SessionA)

	c1, _, cancel1 := claude429NegContext(t, group, 9008, "/v1/messages", body, claude429SessionA, "turn-1")
	defer cancel1()
	// 与 Forward 后 c.Writer.Size() 增加的既有守卫等价：内容已提交，禁止 failover。
	upstream.onCall = func() {
		_, _ = c1.Writer.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}
	h.Messages(c1)
	require.Equal(t, []int64{1}, upstream.hits, "流已写出时必须终止 failover，不再尝试下一个账号")
	require.Zero(t, store.writes, "流已写出时不得写入跨请求冷却")

	upstream.onCall = nil
	c2, rec2, cancel2 := claude429NegContext(t, group, 9008, "/v1/messages", body, claude429SessionA, "turn-2")
	defer cancel2()
	h.Messages(c2)
	require.Empty(t, rec2.Header().Get("Retry-After"), "没有冷却记录时下一次请求不得在入口被拦截")
	require.NotContains(t, rec2.Body.String(), claude429CooldownMessage)
	require.Equal(t, []int64{1, 1, 2}, upstream.hits, "未写入冷却时下一次请求仍按既有 N 访问上游")
}

// TestGatewayClaude429CooldownNegativesNoWriteOnClientCancel 覆盖「客户端断开」：
// 取消不是账号耗尽，不得写入冷却，也不得消耗 429 账号额度。
func TestGatewayClaude429CooldownNegativesNoWriteOnClientCancel(t *testing.T) {
	group := claude429NegGroup(9405)
	upstream := &claude429NegUpstream{}
	h, store := newClaude429NegHandler(t, group, gateway429TestAccounts(group.ID, 3), upstream, 2)
	body := claude429NegMessagesBody(claude429DeviceA, claude429SessionA)

	c1, _, cancel1 := claude429NegContext(t, group, 9008, "/v1/messages", body, claude429SessionA, "turn-1")
	upstream.onCall = cancel1
	h.Messages(c1)
	require.Equal(t, []int64{1}, upstream.hits, "客户端已断开时不得继续换号")
	require.Zero(t, store.writes, "客户端取消时不得写入跨请求冷却")

	upstream.onCall = nil
	c2, rec2, cancel2 := claude429NegContext(t, group, 9008, "/v1/messages", body, claude429SessionA, "turn-2")
	defer cancel2()
	h.Messages(c2)
	require.Empty(t, rec2.Header().Get("Retry-After"), "取消不应留下任何冷却记录")
	require.NotContains(t, rec2.Body.String(), claude429CooldownMessage)
	require.Equal(t, []int64{1, 1, 2}, upstream.hits)
}

// TestGatewayClaude429CooldownNegativesNoWriteOnSameAccountRetryThenSuccess 覆盖
// 「同账号重试后成功」：池模式账号的同账号重试不占用 N 额度，重试成功后逻辑请求成功，
// 不得写入跨请求冷却。
func TestGatewayClaude429CooldownNegativesNoWriteOnSameAccountRetryThenSuccess(t *testing.T) {
	group := claude429NegGroup(9406)
	accounts := gateway429TestAccounts(group.ID, 1)
	// 单账号 + 池模式同账号重试：重试必须落在同一个账号上，无法用换号蒙混通过。
	accounts[0].Credentials["pool_mode"] = true
	accounts[0].Credentials["pool_mode_retry_count"] = 1
	upstream := &claude429NegUpstream{statuses: []int{http.StatusTooManyRequests, http.StatusOK}}
	h, store := newClaude429NegHandler(t, group, accounts, upstream, 2)
	body := claude429NegMessagesBody(claude429DeviceA, claude429SessionA)

	for i := 0; i < 2; i++ {
		c, rec, cancel := claude429NegContext(t, group, 9008, "/v1/messages", body, claude429SessionA, "turn-"+strconv.Itoa(i))
		h.Messages(c)
		cancel()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Empty(t, rec.Header().Get("Retry-After"))
	}
	// 第一次请求：账号 1 的 429 先触发同账号重试（而不是计入 N 后换号），第二次尝试成功；
	// 第二次请求：账号 1 首次尝试即成功。
	require.Equal(t, []int64{1, 1, 1}, upstream.hits, "池模式同账号重试必须先重试同一账号，而不是计入 N 后换号")
	require.Zero(t, store.writes, "同账号重试成功后不得写入跨请求冷却")
}

// TestGatewayClaude429CooldownNegativesCountTokensNeverReadsOrWrites 覆盖
// 「count_tokens 不读不写」：即使同一身份的消息请求已经触顶并写入冷却，
// count_tokens 也不得读取该冷却（不拦截）、不得写入新冷却，仍按既有流程访问上游。
func TestGatewayClaude429CooldownNegativesCountTokensNeverReadsOrWrites(t *testing.T) {
	group := claude429NegGroup(9407)
	// 第 1、2 次上游调用（两个账号的 429）让消息请求触顶 N=2；第 3 次是 count_tokens。
	upstream := &claude429NegUpstream{
		statuses: []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusOK},
		okBody:   `{"input_tokens":5}`,
	}
	h, store := newClaude429NegHandler(t, group, gateway429TestAccounts(group.ID, 3), upstream, 2)
	body := claude429NegMessagesBody(claude429DeviceA, claude429SessionA)

	c1, rec1, cancel1 := claude429NegContext(t, group, 9008, "/v1/messages", body, claude429SessionA, "turn-1")
	h.Messages(c1)
	cancel1()
	require.Equal(t, http.StatusTooManyRequests, rec1.Code, rec1.Body.String())
	require.Equal(t, 1, store.writes, "前置条件：消息请求触顶 N 后已写入冷却")

	reads, writes := store.reads, store.writes
	c2, rec2, cancel2 := claude429NegContext(t, group, 9008, "/v1/messages/count_tokens", body, claude429SessionA, "count-1")
	h.CountTokens(c2)
	cancel2()
	require.Equal(t, reads, store.reads, "count_tokens 不得读取跨请求冷却（不参与这层门禁）")
	require.Equal(t, writes, store.writes, "count_tokens 不得写入跨请求冷却")
	require.Empty(t, rec2.Header().Get("Retry-After"), "count_tokens 不得被本地冷却拦截")
	require.NotContains(t, rec2.Body.String(), claude429CooldownMessage)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Equal(t, []int64{1, 2, 1}, upstream.hits, "count_tokens 仍应访问上游")
}
