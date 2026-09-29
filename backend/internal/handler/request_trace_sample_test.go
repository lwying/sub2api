//go:build unit

package handler

// 合成 Trace 样例（synthetic trace sample）接缝。
//
// 目的：用一个完全合成的 Claude Messages 逻辑请求，走真实装配
// （真实 GatewayHandler.Messages -> 真实 service.GatewayService ->
// repository.NewHTTPUpstream 的真实 RoundTrip -> 本地 httptest 上游），
// 把捕获到的 envelope + 阶段按生产导出实际使用的类型
// （service.RequestTraceExportApprovedDetail）发射成一段样例 JSON，
// 并断言它对凭据安全、容量有界、跨运行稳定。
//
// 这段 JSON 是最终本地沙箱要用的合成样例来源；本文件只把发射与断言固定下来，
// 不落地样例文件（下一次任务再决定样例文件的位置），也不复制任何真实或公开
// 请求里的字段值。所有 canary、身份值都是本文件自造的常量。
//
// 没有生产侧的“样例发射”接缝：生产代码只把 envelope/阶段写库，导出读回时
// 才经 repository.requestTraceExportSource.ReadApprovedDetail 映射成导出 DTO。
// 因此下面的 requestTraceSampleJSON 在测试内复刻同一份映射（同一 DTO、同一
// 有效性校验），不新增生产接口。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	requestTraceSampleModel     = "claude-sonnet-4-5"
	requestTraceSampleGroupID   = int64(7791)
	requestTraceSampleAccountID = int64(9301)

	// 阶段哨兵：分别证明入站正文、上游响应、最终下游响应被真的观察到。
	requestTraceSamplePromptCanary   = "TRACE_SAMPLE_PROMPT_CANARY"
	requestTraceSampleSystemCanary   = "TRACE_SAMPLE_SYSTEM_CANARY"
	requestTraceSampleResponseCanary = "TRACE_SAMPLE_RESPONSE_CANARY"

	// 头值不是凭据时才应保留。
	requestTraceSampleInboundHeaderCanary  = "sample-inbound-visible"
	requestTraceSampleUpstreamHeaderCanary = "sample-upstream-visible"

	// 凭据 canary：任何一个出现在样例 JSON 里都算泄漏。
	requestTraceSampleAccountCredential    = "sk-sample-account-CREDENTIAL_CANARY"
	requestTraceSampleInboundAuthCanary    = "TRACE_SAMPLE_INBOUND_AUTH_CANARY"
	requestTraceSampleInboundQueryCanary   = "TRACE_SAMPLE_INBOUND_QUERY_CANARY"
	requestTraceSampleBodyCredentialCanary = "TRACE_SAMPLE_BODY_CREDENTIAL_CANARY"

	// 样例 JSON 必须小到可以被人工复核；真实 1 MiB/8 MiB 预算是生产上限，
	// 不是合成样例应该逼近的尺寸。
	requestTraceSampleFixtureBudget = 64 << 10

	// requestTraceSampleTraceID 是服务端随机 Trace ID 的固定替身：真实 ID
	// 每次请求都不同，样例要稳定就不能带上它。
	requestTraceSampleTraceID = "00000000000000000000000000000000"

	// requestTraceSampleUpstreamOrigin 是本地 httptest 上游 origin 的固定替身：
	// 真实运行里它是每次都会变的 127.0.0.1:临时端口。用 .invalid 保留域，
	// 让样例里的上游明显是合成的。
	requestTraceSampleUpstreamOrigin = "https://sample-upstream.invalid"
)

// 全部为合成身份：64 位十六进制 device id + 两个 UUID 形状的分量。
const (
	requestTraceSampleDeviceID    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	requestTraceSampleAccountUUID = "11111111-2222-3333-4444-555555555555"
	requestTraceSampleSessionUUID = "66666666-7777-8888-9999-aaaaaaaaaaaa"
)

var (
	requestTraceSampleStartedAt = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	requestTraceSampleEndedAt   = requestTraceSampleStartedAt.Add(120 * time.Millisecond)
)

// requestTraceSampleCredentials 是必须被遮蔽、绝不允许出现在样例里的凭据：
// 账号凭证（网关以 x-api-key 发往上游）、入站 Authorization、入站 query 凭据、
// 以及正文里的结构化凭据字段。
var requestTraceSampleCredentials = map[string]string{
	"account credential":  requestTraceSampleAccountCredential,
	"inbound auth header": requestTraceSampleInboundAuthCanary,
	"inbound query key":   requestTraceSampleInboundQueryCanary,
	"body credential":     requestTraceSampleBodyCredentialCanary,
}

// requestTraceSamplePayloadValue 返回 value 在正文 JSON 文本里的写法：正文是
// 结构化 JSON，字符串值内部的双引号在文本里带反斜杠（JSON 形态的
// metadata.user_id 正是这种嵌套字符串），所以不能用原始 Go 字符串直接比对。
func requestTraceSamplePayloadValue(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

// requestTraceSampleJSONUserIDPayload 是 CLI >= 2.1.78 的 JSON 形态
// metadata.user_id，字段顺序与生产 FormatMetadataUserID 一致。
type requestTraceSampleJSONUserIDPayload struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

// requestTraceSampleLegacyUserID 是旧形态：user_{64hex}_account_{uuid}_session_{uuid}。
// 三个分量长度固定为 5+64+9+36+9+36 = 159 字节，测试会断言这一点。
func requestTraceSampleLegacyUserID() string {
	return "user_" + requestTraceSampleDeviceID + "_account_" + requestTraceSampleAccountUUID +
		"_session_" + requestTraceSampleSessionUUID
}

func requestTraceSampleJSONUserID(t *testing.T) string {
	t.Helper()
	encoded, err := json.Marshal(requestTraceSampleJSONUserIDPayload{
		DeviceID:    requestTraceSampleDeviceID,
		AccountUUID: requestTraceSampleAccountUUID,
		SessionID:   requestTraceSampleSessionUUID,
	})
	require.NoError(t, err)
	return string(encoded)
}

// requestTraceSampleBody 是一个合成 Messages 正文：包含 system、thinking
// budget、tools、max_tokens 与 metadata.user_id。这些字段都是脱敏误报的高发区
// （max_tokens / budget_tokens 不能被 "token" 子串规则擦掉），因此样例里要能看见。
func requestTraceSampleBody(t *testing.T, userID string) string {
	t.Helper()
	encodedUserID, err := json.Marshal(userID)
	require.NoError(t, err)
	return `{"model":"` + requestTraceSampleModel + `",` +
		`"max_tokens":256,` +
		`"metadata":{"user_id":` + string(encodedUserID) + `},` +
		`"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"system":[{"type":"text","text":"` + requestTraceSampleSystemCanary + `"},` +
		`{"type":"text","text":"synthetic-second-system-block"}],` +
		`"tools":[{"name":"sample_tool","description":"synthetic tool","input_schema":{"type":"object"}}],` +
		`"api_key":"` + requestTraceSampleBodyCredentialCanary + `",` +
		`"messages":[{"role":"user","content":"` + requestTraceSamplePromptCanary + `"}]}`
}

type requestTraceSampleRun struct {
	response *httptest.ResponseRecorder
	trace    service.RequestTrace
	// finalState 是队列在全部阶段写完后回传的终态；捕获信封里的 CaptureState
	// 只是入队时的 pending 值（partial），导出读回的是终态。
	finalState service.RequestTraceCaptureState
	stages     []service.RequestTraceStage
	// upstreamOrigin 是本次运行本地 httptest 上游的 origin（含临时端口）。
	upstreamOrigin string
	upstreamPaths  []string
	upstreamBodies [][]byte
}

// requestTraceSampleDiff 报告首个不同字节的位置与两侧上下文，让“样例不稳定”
// 的失败直接指向差异，而不是两份十六进制字节数组。
func requestTraceSampleDiff(a, b []byte) string {
	limit := min(len(a), len(b))
	at := 0
	for at < limit && a[at] == b[at] {
		at++
	}
	window := func(in []byte) string {
		return string(in[max(0, at-48):min(len(in), at+48)])
	}
	return fmt.Sprintf("first difference at byte %d (len %d vs %d)\n  first:  %q\n  second: %q",
		at, len(a), len(b), window(a), window(b))
}

// runRequestTraceSample 用真实 Messages 网关 + 真实 transport + 本地 httptest
// 上游跑一次合成请求，返回该逻辑请求捕获到的 envelope 与阶段。
func runRequestTraceSample(t *testing.T, userID string) requestTraceSampleRun {
	t.Helper()

	capture := &requestTraceRealUpstream{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		capture.record(req, body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Canary", requestTraceSampleUpstreamHeaderCanary)
		_, _ = w.Write([]byte(`{"id":"msg_sample","type":"message","role":"assistant","model":"` +
			requestTraceSampleModel + `","content":[{"type":"text","text":"` +
			requestTraceSampleResponseCanary + `"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":11,"output_tokens":7}}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	group := &service.Group{ID: requestTraceSampleGroupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := &service.Account{
		ID: requestTraceSampleAccountID, Name: "sample-upstream", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Priority: 1, Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  requestTraceSampleAccountCredential,
			"base_url": upstream.URL,
		},
		AccountGroups: []service.AccountGroup{{AccountID: requestTraceSampleAccountID, GroupID: requestTraceSampleGroupID}},
	}
	h := newRequestTraceRealGatewayMessages(t, group, account, cfg)

	repo := &finalizingRequestTraceRepoStub{finalized: make(chan service.RequestTraceCaptureState, 1)}
	queue := service.NewRequestTraceCaptureQueue(repo)
	t.Cleanup(queue.Stop)

	r := gin.New()
	r.POST("/v1/messages",
		RequestTraceCaptureMiddleware(func(context.Context) bool { return true }, repo, queue),
		func(c *gin.Context) {
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
			keyGroupID := requestTraceSampleGroupID
			apiKey := &service.APIKey{
				ID: 8, UserID: 10, GroupID: &keyGroupID, Status: service.StatusActive,
				User: &service.User{ID: 10, Concurrency: 10, Balance: 100}, Group: group,
			}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 10, Concurrency: 10})
			c.Next()
		},
		BindRequestTraceAfterAuth(),
		h.Messages,
	)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/messages?api_key="+requestTraceSampleInboundQueryCanary+"&source=synthetic",
		strings.NewReader(requestTraceSampleBody(t, userID)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+requestTraceSampleInboundAuthCanary)
	req.Header.Set("X-Inbound-Canary", requestTraceSampleInboundHeaderCanary)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "real gateway must answer the client: %s", w.Body.String())

	var finalState service.RequestTraceCaptureState
	select {
	case state := <-repo.finalized:
		require.Equal(t, service.RequestTraceStored, state, "a fully observed real attempt must finalize as stored")
		finalState = state
	case <-time.After(2 * time.Second):
		t.Fatalf("real gateway trace was never finalized (queue stored=%d rejected=%d)",
			queue.Stats().Stored, queue.Stats().Rejected)
	}

	repo.mu.Lock()
	traces := append([]service.RequestTrace(nil), repo.traces...)
	stages := append([]service.RequestTraceStage(nil), repo.stages...)
	repo.mu.Unlock()
	require.Len(t, traces, 1, "one logical request must produce exactly one trace")

	paths, bodies := capture.snapshot()
	return requestTraceSampleRun{
		response: w, trace: traces[0], finalState: finalState, stages: stages,
		upstreamOrigin: upstream.URL, upstreamPaths: paths, upstreamBodies: bodies,
	}
}

// requestTraceSampleStage 取唯一匹配的阶段，缺失或不唯一都直接失败。
func requestTraceSampleStage(t *testing.T, run requestTraceSampleRun, stage, view string, attempt int) service.RequestTraceStage {
	t.Helper()
	var found []service.RequestTraceStage
	for _, item := range run.stages {
		if item.Stage == stage && item.View == view && item.AttemptIndex == attempt {
			found = append(found, item)
		}
	}
	require.Len(t, found, 1, "stage %q view %q attempt %d must be observed exactly once", stage, view, attempt)
	return found[0]
}

// requestTraceSampleJSON 复刻生产导出映射（repository.ReadApprovedDetail ->
// service.RequestTraceExportApprovedDetail -> encoding/json 一行 JSONL），
// 并把请求级易变值（Trace ID、尝试起止时刻）换成固定常量，使样例可稳定提交。
func requestTraceSampleJSON(t *testing.T, run requestTraceSampleRun) []byte {
	t.Helper()
	detail := service.RequestTraceExportApprovedDetail{
		TraceID:         requestTraceSampleTraceID,
		RouteFamily:     string(run.trace.RouteFamily),
		InboundEndpoint: run.trace.InboundEndpoint,
		CaptureState:    string(run.finalState),
		ClientStatus:    run.trace.ClientStatus,
		UsageLogID:      run.trace.UsageLogID,
		Stages:          make([]service.RequestTraceExportApprovedStage, 0, len(run.stages)),
	}
	for _, stage := range run.stages {
		item := service.RequestTraceExportApprovedStage{
			Ordinal: stage.Ordinal, Stage: stage.Stage, AttemptIndex: stage.AttemptIndex, ViewName: stage.View,
			State: string(stage.State), Reason: stage.Reason, ObservedBytes: stage.ObservedBytes,
			RetainedBytes: stage.RetainedBytes, DroppedEvents: stage.DroppedEvents,
			RedactionUnverified: stage.RedactionUnverified,
		}
		if len(stage.Payload) > 0 && utf8.Valid(stage.Payload) {
			item.PayloadText = string(stage.Payload)
		}
		// 与生产同一道校验：无效的 facts 不会被发射出去。
		item.Facts = requestTraceSampleStableFacts(service.CloneRequestTraceStageFacts(stage.Stage, stage.Metadata), run.upstreamOrigin)
		item.Decision = service.CloneRequestTraceDecisionFacts(stage.Decision)
		if item.Decision != nil && item.Decision.DecidedAt != nil {
			item.Decision.DecidedAt = service.RequestTraceDecisionTimestamp(requestTraceSampleStartedAt)
		}
		detail.Stages = append(detail.Stages, item)
	}
	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	return encoded
}

// requestTraceSampleStableFacts 只归一化请求级易变值：观察到的尝试起止时刻，
// 以及本次运行本地 httptest 上游的 origin（临时端口）。URL 的脱敏已由生产在
// 写入前完成，这里只在脱敏结果上替换 origin，不改变凭据遮蔽结果。
func requestTraceSampleStableFacts(facts *service.RequestTraceStageFacts, upstreamOrigin string) *service.RequestTraceStageFacts {
	if facts == nil {
		return nil
	}
	stable := *facts
	if stable.StartedAt != nil {
		started := requestTraceSampleStartedAt
		stable.StartedAt = &started
	}
	if stable.EndedAt != nil {
		ended := requestTraceSampleEndedAt
		stable.EndedAt = &ended
	}
	if stable.URL != "" && upstreamOrigin != "" {
		stable.URL = strings.Replace(stable.URL, upstreamOrigin, requestTraceSampleUpstreamOrigin, 1)
	}
	// Date 是上游按秒下发的时钟值，不是凭据。样例要跨运行字节稳定，就必须
	// 把它换成固定时刻；这正是“稳定样例”与“原样审计”的区别。
	if stable.ResponseHeaders != nil {
		if _, ok := stable.ResponseHeaders["Date"]; ok {
			stable.ResponseHeaders = stable.ResponseHeaders.Clone()
			stable.ResponseHeaders.Set("Date", requestTraceSampleStartedAt.Format(http.TimeFormat))
		}
	}
	return &stable
}

// assertRequestTraceSampleSafe 断言样例 JSON 里没有任何凭据 canary，且确实留下了
// 脱敏标记、保留了非凭据头值。
func assertRequestTraceSampleSafe(t *testing.T, sample []byte, run requestTraceSampleRun) {
	t.Helper()
	text := string(sample)
	for name, canary := range requestTraceSampleCredentials {
		require.NotContains(t, text, canary, "%s must never appear in an emitted sample", name)
	}
	// 原始阶段负载本身也不能带凭据：样例安全不能只靠发射时过滤。
	for _, stage := range run.stages {
		for name, canary := range requestTraceSampleCredentials {
			require.NotContains(t, string(stage.Payload), canary,
				"%s leaked into stage %q view %q", name, stage.Stage, stage.View)
		}
	}
	require.Contains(t, text, "[REDACTED]", "the sample must show where a known credential was replaced")
	require.Contains(t, text, requestTraceSampleInboundHeaderCanary, "a non-credential header value stays observable")
	require.Contains(t, text, requestTraceSampleUpstreamHeaderCanary, "a non-credential upstream header stays observable")

	// 带凭据的阶段必须是已脱敏验证过的，不能是 redaction_unverified 原始字节。
	for _, stage := range []service.RequestTraceStage{
		requestTraceSampleStage(t, run, "client_entry", "decoded", 0),
		requestTraceSampleStage(t, run, "wire_request", "wire", 1),
	} {
		require.False(t, stage.RedactionUnverified,
			"stage %q with a known credential must be verified, not unverified raw bytes", stage.Stage)
		require.Equal(t, service.RequestTraceStored, stage.State, "stage %q must be stored after redaction", stage.Stage)
	}
}

// assertRequestTraceSampleBounded 断言样例同时满足生产上限与“可复核”的样例预算。
func assertRequestTraceSampleBounded(t *testing.T, sample []byte, run requestTraceSampleRun) {
	t.Helper()
	require.LessOrEqual(t, len(run.stages), service.RequestTraceStageCountLimit, "stage count must stay within the production cap")
	var retained int64
	for _, stage := range run.stages {
		require.LessOrEqual(t, len(stage.Payload), service.RequestTraceStagePayloadLimit,
			"stage %q view %q must respect the per-stage payload budget", stage.Stage, stage.View)
		retained += int64(len(stage.Payload))
	}
	require.LessOrEqual(t, retained, int64(service.RequestTraceTotalBodyLimit),
		"the whole trace must respect the total body budget")
	require.Less(t, len(sample), requestTraceSampleFixtureBudget,
		"a committable sample must stay reviewably small, got %d bytes", len(sample))
	require.True(t, utf8.Valid(sample), "a sample must be valid UTF-8 to be reviewable")
	require.True(t, json.Valid(sample), "a sample must be valid JSON")
}

// TestRequestTraceSampleExampleIsSafeBoundedAndStable 是本文件的唯一用例：
// 真实 Messages 链路跑两次合成请求，发射同一份样例 JSON，断言
// 1) 安全（无凭据 canary、脱敏标记可见、身份值可见）；
// 2) 有界（阶段数/单阶段/总量/样例尺寸都在界内，且是合法 JSON）；
// 3) 稳定（两次独立运行发射出的字节完全相同，Trace ID 与时刻已归一化）；
// 4) 完整（入站/每次 wire 尝试/上游响应/下游响应阶段齐全，尝试的账号与协议正确）。
func TestRequestTraceSampleExampleIsSafeBoundedAndStable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	variants := []struct {
		name   string
		userID func(*testing.T) string
	}{
		// 旧形态必须正好 159 字节，这是公开 Claude Code 请求里出现的那种形态
		// （本用例只复刻长度与形状，不使用任何真实值）。
		{name: "legacy_underscore_user_id", userID: func(*testing.T) string {
			legacy := requestTraceSampleLegacyUserID()
			require.Len(t, legacy, 159, "the legacy metadata.user_id shape is 159 bytes")
			return legacy
		}},
		{name: "json_string_user_id", userID: requestTraceSampleJSONUserID},
	}

	var samples [][]byte
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			userID := variant.userID(t)

			first := runRequestTraceSample(t, userID)
			second := runRequestTraceSample(t, userID)

			firstSample := requestTraceSampleJSON(t, first)
			secondSample := requestTraceSampleJSON(t, second)

			// 稳定：两次独立运行（不同 Trace ID、不同时刻）发射同一段字节。
			require.NotEqual(t, first.trace.TraceID, second.trace.TraceID,
				"each request must get a fresh server-side Trace ID")
			require.NotContains(t, string(firstSample), first.trace.TraceID,
				"the volatile server-only Trace ID must not survive into a stable sample")
			require.NotContains(t, string(firstSample), first.upstreamOrigin,
				"the per-run local upstream origin must not survive into a stable sample")
			require.Truef(t, bytes.Equal(firstSample, secondSample),
				"the emitted sample must be byte-stable across two independent real runs: %s",
				requestTraceSampleDiff(firstSample, secondSample))

			// 样例可以被消费方直接反序列化成导出类型。
			var decoded service.RequestTraceExportApprovedDetail
			require.NoError(t, json.Unmarshal(firstSample, &decoded))
			require.Equal(t, requestTraceSampleTraceID, decoded.TraceID)
			require.Equal(t, "messages", decoded.RouteFamily)
			require.Equal(t, "/v1/messages", decoded.InboundEndpoint)
			require.Equal(t, service.RequestTraceStored, service.RequestTraceCaptureState(decoded.CaptureState))
			require.Equal(t, http.StatusOK, decoded.ClientStatus)
			require.Nil(t, decoded.UsageLogID)
			require.Len(t, decoded.Stages, len(first.stages), "emission must not drop observed stages")

			assertRequestTraceSampleSafe(t, firstSample, first)
			assertRequestTraceSampleBounded(t, firstSample, first)
			assertRequestTraceSampleStableShape(t, userID, firstSample, first)
			samples = append(samples, firstSample)
		})
	}

	require.Len(t, samples, 2)
	require.NotEqual(t, samples[0], samples[1],
		"the two metadata.user_id variants must remain distinguishable in the emitted sample")
}

// assertRequestTraceSampleStableShape 断言样例的阶段序列、入站/wire/出站事实与尝试归属。
func assertRequestTraceSampleStableShape(t *testing.T, userID string, sample []byte, run requestTraceSampleRun) {
	t.Helper()

	// 真实上游只被调用一次，且真的收到了合成提示词：wire 阶段不是伪造的。
	require.Equal(t, []string{"/v1/messages"}, run.upstreamPaths)
	require.Len(t, run.upstreamBodies, 1)
	require.Contains(t, string(run.upstreamBodies[0]), requestTraceSamplePromptCanary)

	// 阶段序列在两次运行之间固定，样例内容因此可比对。顺序取真实链路实际产出的
	// 顺序：flow.finish 先放入站、已观察的网关决策与类型化尝试事实，再放收集器里已完成的
	// 正文快照（按观察顺序），最后是下游响应。
	//
	// 入站正文只有 decoded 一个视图：合成的请求没有 Content-Encoding，收集器对
	// identity 编码不再重复保存一份与 decoded 完全相同的 transmitted 视图。
	sequence := make([]string, 0, len(run.stages))
	described := make([]string, 0, len(run.stages))
	for _, stage := range run.stages {
		sequence = append(sequence, stage.Stage+":"+stage.View+":"+strconv.Itoa(stage.AttemptIndex))
		described = append(described, fmt.Sprintf("%s/%s#%d=%s(%s,%dB/%dB)",
			stage.Stage, stage.View, stage.AttemptIndex, stage.State, stage.Reason, stage.ObservedBytes, stage.RetainedBytes))
	}
	require.Equalf(t, []string{
		"client_metadata::0", "gateway_decision::0", "gateway_decision::1", "gateway_decision::0", "wire_attempt::1",
		"client_entry:decoded:0",
		"wire_request:wire:1", "upstream_response:received:1", "client_response:downstream:0",
	}, sequence, "observed stages: %s", strings.Join(described, " "))

	identity := requestTraceSamplePayloadValue(t, userID)

	// 发射出的样例文本里也要能读到身份分量：样例是排障时真正被看到的形态。
	require.Contains(t, string(sample), requestTraceSampleDeviceID)
	require.Contains(t, string(sample), requestTraceSampleAccountUUID)
	require.Contains(t, string(sample), requestTraceSampleSessionUUID)

	// 入站：客户端真的发来的字节 + 脱敏后的入站元数据。
	decoded := requestTraceSampleStage(t, run, "client_entry", "decoded", 0)
	require.Contains(t, string(decoded.Payload), requestTraceSamplePromptCanary)
	require.Contains(t, string(decoded.Payload), requestTraceSampleSystemCanary)
	require.Contains(t, string(decoded.Payload), `"max_tokens":256`,
		"max_tokens is a protocol counter, not a credential; over-redaction would erase an observed fact")
	require.Contains(t, string(decoded.Payload), `"budget_tokens":1024`,
		"thinking budget_tokens must survive the token-substring redaction heuristic")
	require.Contains(t, string(decoded.Payload), identity,
		"the client-supplied metadata.user_id must stay observable at the inbound stage")

	clientMetadata := requestTraceSampleStage(t, run, "client_metadata", "", 0)
	require.NotNil(t, clientMetadata.Metadata)
	require.Equal(t, http.MethodPost, clientMetadata.Metadata.Method)
	require.Contains(t, clientMetadata.Metadata.URL, "api_key=%5BREDACTED%5D")
	require.Contains(t, clientMetadata.Metadata.URL, "source=synthetic")
	require.Equal(t, "[REDACTED]", clientMetadata.Metadata.RequestHeaders.Get("Authorization"))
	require.Equal(t, requestTraceSampleInboundHeaderCanary, clientMetadata.Metadata.RequestHeaders.Get("X-Inbound-Canary"))

	// wire：真实发出的字节，且本次尝试的账号/协议/状态/时刻都在。
	wireRequest := requestTraceSampleStage(t, run, "wire_request", "wire", 1)
	require.Contains(t, string(wireRequest.Payload), requestTraceSamplePromptCanary)
	require.Contains(t, string(wireRequest.Payload), identity,
		"the wire bytes must be shown as observed, so a rewritten or dropped identity would be visible here")

	attempt := requestTraceSampleStage(t, run, "wire_attempt", "", 1)
	require.NotNil(t, attempt.Metadata)
	require.EqualValues(t, requestTraceSampleAccountID, attempt.Metadata.AccountID)
	require.Equal(t, "anthropic.messages", attempt.Metadata.Protocol)
	require.Equal(t, requestTraceSampleModel, attempt.Metadata.Model)
	require.Equal(t, http.StatusOK, attempt.Metadata.Status)
	require.Equal(t, requestTraceSampleUpstreamHeaderCanary, attempt.Metadata.ResponseHeaders.Get("X-Upstream-Canary"))
	require.NotNil(t, attempt.Metadata.StartedAt)
	require.NotNil(t, attempt.Metadata.EndedAt)

	// 上游响应与最终下游响应都被观察到，下游与客户端实际收到的内容一致。
	upstreamResponse := requestTraceSampleStage(t, run, "upstream_response", "received", 1)
	require.Contains(t, string(upstreamResponse.Payload), requestTraceSampleResponseCanary)

	clientResponse := requestTraceSampleStage(t, run, "client_response", "downstream", 0)
	require.Contains(t, string(clientResponse.Payload), requestTraceSampleResponseCanary)
	require.JSONEq(t, run.response.Body.String(), string(clientResponse.Payload),
		"the retained downstream body must describe what the client actually received")
	require.Equal(t, service.RequestTraceStored, clientResponse.State)
}
