package service

// 上游错误诊断接缝的通用绑定处（契约见
// docs/adr/0005-short-lived-error-body-diagnostics-scope.md）。
//
// 传输层只在请求上下文显式携带 httpattempt.DiagnosticObserver 时才观察「真实发往上游」的
// 4xx/5xx（见 internal/pkg/httpattempt/diagnostic.go 与
// internal/repository/http_upstream.go 的 roundTripAttempt）。本文件是本包内唯一的绑定处与
// 唯一的写入口：
//
//   - 绑定：每条真实发送路径在 bindRequestAuditHTTPAttempt 之后显式绑定观察者，使诊断与请求
//     审计看到的是同一次尝试。插件、WS 与没有真实 RoundTrip 的路径拿不到观察者，因此天然
//     不采集，也不会被伪造成上游 HTTP 失败。
//   - 协议：调用方必须显式给出被覆盖分支的协议（messages／chat_completions／responses），
//     且协议按**入站路由**推导，不跟随上游 wire 协议或请求审计的协议值；未知协议一律不绑定。
//   - 采集边界：观察者只在传输层真的收到 4xx/5xx 时被调用一次；连接故障、客户端取消与本地
//     拒绝都不会产生观察结果。
//   - 写入：回调内只做有界快照（正文 ≤1 MiB 且必须读完整，由传输层保证），随后交给诊断服务
//     异步写入。写入上下文脱离客户端取消并带短超时，任何失败都被丢弃（fail-open），既不改变
//     上游／客户端结果，也不会在缺密钥时退化为明文，更不会把原始错误或正文写日志。
//     缺密钥时不是「没采集」而是「要求过、做不到」：接缝因此带上封闭抑制结论
//     （ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable），元数据留下稳定原因码
//     skipped_encryption_unavailable，而不是 not_observed。
//   - 记录内容：只有协议枚举、尝试序号、阶段、上游状态与正文状态／原因。账号、账号 ID、
//     原始 URL、请求头、错误文本、模型名与凭据一律不进入诊断。
//
// 生产默认关闭：只有显式注入接缝（SetErrorDiagnosticRecorder）且门控 Enabled 与
// RiskAcknowledged 同时为真时才会绑定。

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

const (
	// errorDiagnosticSettingsTTL 是进程内门控快照的刷新间隔。门控是进程级配置，热路径只读
	// 快照：与既有网关设置的进程内缓存同一约定
	// （见 setting_gateway_runtime.go 的 getGatewayForwardingSettingsCached）。
	errorDiagnosticSettingsTTL = 30 * time.Second
	// errorDiagnosticSettingsDBTimeout 是刷新门控时的读取超时（脱离客户端取消）。
	errorDiagnosticSettingsDBTimeout = 2 * time.Second
	// errorDiagnosticWriteTimeout 是一次诊断写入的上限；超时即放弃本次写入，不影响原请求。
	errorDiagnosticWriteTimeout = 3 * time.Second
	// errorDiagnosticMaxInflightWrites 是并发诊断写入的上限。上游错误风暴时超出即丢弃观察
	// 结果（fail-open），不会无界堆积待加密正文。
	errorDiagnosticMaxInflightWrites = 16
)

// ErrorDiagnosticRecorder 是错误诊断接缝唯一的写入契约，由 *ErrorDiagnosticService 满足。
type ErrorDiagnosticRecorder interface {
	RecordErrorDiagnostic(ctx context.Context, attempt ErrorDiagnosticAttempt) (ErrorDiagnosticRecord, error)
}

// ErrorDiagnosticDropRecorder 是写入契约的可选能力：在写入积压时报告一次丢弃。
//
// 单独成接口而不是并入 ErrorDiagnosticRecorder，是为了不强迫既有测试替身实现它；
// *ErrorDiagnosticService 满足它，接缝在溢出分支用类型断言调用，失败即静默跳过。
type ErrorDiagnosticDropRecorder interface {
	RecordDroppedErrorDiagnostic()
}

// errorDiagnosticObserver 是一个网关上已注入的诊断接缝。
//
// 门控快照、singleflight 与并发写入槽位都是实例状态：每个服务实例自带一份缓存，
// 测试之间天然隔离，不存在共享的进程级状态。
type errorDiagnosticObserver struct {
	recorder ErrorDiagnosticRecorder
	settings ErrorDiagnosticSettingsReader

	settingsCache atomic.Pointer[errorDiagnosticSettingsSnapshot]
	settingsSF    singleflight.Group
	writeSlots    chan struct{}
}

// errorDiagnosticSettingsSnapshot 是带到期时刻的门控快照。
type errorDiagnosticSettingsSnapshot struct {
	settings  ErrorDiagnosticSettings
	expiresAt int64
}

// newErrorDiagnosticObserver 构造接缝；recorder 或 settings 缺失时返回 nil（关闭采集）。
func newErrorDiagnosticObserver(recorder ErrorDiagnosticRecorder, settings ErrorDiagnosticSettingsReader) *errorDiagnosticObserver {
	if recorder == nil || settings == nil {
		return nil
	}
	return &errorDiagnosticObserver{
		recorder:   recorder,
		settings:   settings,
		writeSlots: make(chan struct{}, errorDiagnosticMaxInflightWrites),
	}
}

// SetErrorDiagnosticRecorder 注入诊断接缝（nil 表示关闭采集）。
//
// 由装配层在构造网关服务之后调用一次；门控读取复用同一服务的 SettingService，
// 因此不需要改动构造函数签名，也不发布任何进程级状态。
func (s *GatewayService) SetErrorDiagnosticRecorder(recorder ErrorDiagnosticRecorder) {
	if s == nil {
		return
	}
	s.errorDiagnostics = newErrorDiagnosticObserver(recorder, s.settingService)
}

// SetErrorDiagnosticRecorder 注入诊断接缝（nil 表示关闭采集）。
func (s *OpenAIGatewayService) SetErrorDiagnosticRecorder(recorder ErrorDiagnosticRecorder) {
	if s == nil {
		return
	}
	s.errorDiagnostics = newErrorDiagnosticObserver(recorder, s.settingService)
}

// bindErrorDiagnosticObserver 为一次真实发往上游的请求显式绑定诊断观察者。
//
// protocol 必须是本包白名单里的协议枚举（messages／chat_completions／responses），
// 由调用方按**入站路由**给出；未知协议不绑定（fail closed）。
//
// 必须在 bindRequestAuditHTTPAttempt 之后调用：诊断消费的是请求审计真实记录下来的尝试序号，
// 不能在发送前伪造，也不能从最终 usage 反推。未注入接缝或门控未开启时原样返回，
// 请求上下文不会多出任何值。
func (s *GatewayService) bindErrorDiagnosticObserver(req *http.Request, c *gin.Context, protocol string) *http.Request {
	if s == nil {
		return req
	}
	return s.errorDiagnostics.bindRequest(req, c, protocol)
}

// bindErrorDiagnosticObserver 为一次真实发往上游的请求显式绑定诊断观察者（协议同上）。
func (s *OpenAIGatewayService) bindErrorDiagnosticObserver(req *http.Request, c *gin.Context, protocol string) *http.Request {
	if s == nil {
		return req
	}
	return s.errorDiagnostics.bindRequest(req, c, protocol)
}

// bindMessagesErrorDiagnosticObserver 是 Messages 分支的协议收口点。
func (s *GatewayService) bindMessagesErrorDiagnosticObserver(req *http.Request, c *gin.Context) *http.Request {
	return s.bindErrorDiagnosticObserver(req, c, ErrorDiagnosticProtocolMessages)
}

// bindMessagesErrorDiagnosticObserver 是 Messages 分支的协议收口点。
func (s *OpenAIGatewayService) bindMessagesErrorDiagnosticObserver(req *http.Request, c *gin.Context) *http.Request {
	return s.bindErrorDiagnosticObserver(req, c, ErrorDiagnosticProtocolMessages)
}

// bindResponsesErrorDiagnosticBranch 在入站分支的上下文上绑定 Responses 诊断观察者。
func (s *GatewayService) bindResponsesErrorDiagnosticBranch(ctx context.Context, c *gin.Context) context.Context {
	if s == nil {
		return ctx
	}
	return s.errorDiagnostics.bindContext(ctx, c, ErrorDiagnosticProtocolResponses)
}

// bindResponsesErrorDiagnosticBranch 在入站分支的上下文上绑定 Responses 诊断观察者。
func (s *OpenAIGatewayService) bindResponsesErrorDiagnosticBranch(ctx context.Context, c *gin.Context) context.Context {
	if s == nil {
		return ctx
	}
	return s.errorDiagnostics.bindContext(ctx, c, ErrorDiagnosticProtocolResponses)
}

// bindRequest 在协议白名单与门控都允许时，把一个显式观察者绑到本次出站请求上。
func (o *errorDiagnosticObserver) bindRequest(req *http.Request, c *gin.Context, protocol string) *http.Request {
	if o == nil || req == nil || c == nil {
		return req
	}
	observer, ok := o.observerFor(req.Context(), c, protocol)
	if !ok {
		return req
	}
	return req.WithContext(httpattempt.WithDiagnosticObserver(req.Context(), observer))
}

// bindContext 在协议白名单与门控都允许时，把一个显式观察者绑到分支上下文上。
func (o *errorDiagnosticObserver) bindContext(ctx context.Context, c *gin.Context, protocol string) context.Context {
	if o == nil || c == nil {
		return ctx
	}
	observer, ok := o.observerFor(ctx, c, protocol)
	if !ok {
		return ctx
	}
	return httpattempt.WithDiagnosticObserver(ctx, observer)
}

// observerFor 按协议与门控构造本次绑定的观察者；不满足条件时返回 ok=false（不绑定）。
//
// 协议由调用方在绑定时给出并随观察者闭包携带，因此同一份注入状态可以服务多个协议分支，
// 不存在把 Responses 记成 Messages 的共享硬编码。
func (o *errorDiagnosticObserver) observerFor(ctx context.Context, c *gin.Context, protocol string) (*httpattempt.DiagnosticObserver, bool) {
	ordinalKey := errorDiagnosticOrdinalContextKey(protocol)
	if ordinalKey == "" {
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 门控读取失败一律按关闭处理（fail closed），不做隐式开启。
	settings := o.settingsSnapshot(ctx)
	if !settings.CaptureAllowed() {
		return nil, false
	}
	// 密钥可用性只读一次并复用到两处判定：它既决定 tee 与否，也决定要不要带出封闭抑制结论。
	// 两处用同一次读取，避免绑定当次出现「tee 了却说自己没 tee」这类自相矛盾的组合。
	keyAvailable := o.bodyRetentionKeyAvailable()
	// 门控要求过留存而密钥缺席时，本次观察必须显式表达「按策略扣住了字节」：
	// 否则传输层只能报 not_requested，这一行会落成 not_observed，
	// 把配置故障显示成「未观察到正文」，并与运维界面的 key-unavailable 状态自相矛盾
	// （票据 02 要求缺密钥留下安全元数据与稳定原因码）。
	suppressedVerdict := ""
	if settings.BodyRetentionSuppressedByMissingKey(keyAvailable) {
		suppressedVerdict = ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable
	}
	return &httpattempt.DiagnosticObserver{
		// 正文留存是票 02 的分阶段 opt-in：未开启时只采集净化元数据。
		// 而且必须真有稳定密钥才 tee：只有布尔值而密钥缺席时，这些字节注定被判为
		// skipped_encryption_unavailable，传输层不该为它们读走并复制最多 1 MiB 明文。
		CaptureRequestBody: settings.BodyCaptureAllowed() && keyAvailable,
		OnUpstreamError: func(observation httpattempt.DiagnosticObservation) {
			o.record(ctx, protocol, observation, c, ordinalKey, suppressedVerdict)
		},
	}, true
}

// bodyRetentionKeyAvailable 报告「此刻是否真的有稳定密钥可以留存正文」。
//
// 判定复用运维状态里 BodyRetentionAllowed 的同一来源
// （SettingService.ErrorDiagnosticBodyEncryptionKeyAvailable），不在这里另写一份密钥规则。
// 读取器没有声明该能力时按不可用处理（fail closed）：没有稳定密钥就绝不 tee 出站正文，
// 元数据采集不受影响。
func (o *errorDiagnosticObserver) bodyRetentionKeyAvailable() bool {
	if o == nil || o.settings == nil {
		return false
	}
	availability, ok := o.settings.(ErrorDiagnosticBodyRetentionKeyAvailability)
	return ok && availability.ErrorDiagnosticBodyEncryptionKeyAvailable()
}

// settingsSnapshot 返回当前门控；热路径只读快照，到期后由单个 goroutine 刷新。
func (o *errorDiagnosticObserver) settingsSnapshot(ctx context.Context) ErrorDiagnosticSettings {
	if cached := o.settingsCache.Load(); cached != nil && time.Now().UnixNano() < cached.expiresAt {
		return cached.settings
	}
	value, _, _ := o.settingsSF.Do("error_diagnostic_settings", func() (any, error) {
		if cached := o.settingsCache.Load(); cached != nil && time.Now().UnixNano() < cached.expiresAt {
			return cached.settings, nil
		}
		// 读取本身也必须脱离客户端取消，否则客户端一断开就会把门控读成「关闭」。
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), errorDiagnosticSettingsDBTimeout)
		defer cancel()
		settings, err := o.settings.GetErrorDiagnosticSettings(dbCtx)
		if err != nil {
			// 读取失败或未配置时按全关处理，并照旧缓存：fail closed 不得退化成隐式开启。
			settings = ErrorDiagnosticSettings{}
		}
		o.settingsCache.Store(&errorDiagnosticSettingsSnapshot{
			settings:  settings,
			expiresAt: time.Now().Add(errorDiagnosticSettingsTTL).UnixNano(),
		})
		return settings, nil
	})
	settings, _ := value.(ErrorDiagnosticSettings)
	return settings
}

// record 把一次观察到的上游 4xx/5xx 交给诊断服务。
//
// 回调是同步的，所以这里只做有界快照：接缝在回调返回后立即清零缓冲，正文只能在回调内复制，
// 且只在「读完整且不超过上限」时才带上，多余字节一并丢弃而不是截断冒充完整。快照之后交给
// 独立 goroutine 写入，写入结果一律忽略，不改变上游／客户端结果。
//
// 顺序保证：先非阻塞地占住写入槽，再复制正文。槽位满时整条观察（含正文副本）都不会产生，
// 因此有界队列同时也是明文内存的边界，而不是只在写入阶段才受限。
//
// 尝试序号优先用传输层记录的 observation.AttemptOrdinal（请求审计绑定的 Counter 给出真实
// 发送序号）；只有在计数缺席时才按「本协议已观察到的失败次数」兜底。兜底在观察时推进而不是
// 绑定时推进：一次发送可能被绑两次（分支级 + 请求级，内层生效），绑定即推进会让序号漂移，
// 而绑定却可能根本没有真实发送（插件/WS 分支），因此只有真的观察到失败才占一个序号。
//
// suppressedVerdict 是本协议绑定当时算好的封闭抑制结论（没有时为 ""）：它只描述「门控要求过
// 留存而稳定密钥缺席」这一次绑定的事实，因此随闭包传入，不在观察时重新读取设置。
func (o *errorDiagnosticObserver) record(
	baseCtx context.Context,
	protocol string,
	observation httpattempt.DiagnosticObservation,
	c *gin.Context,
	ordinalKey string,
	suppressedVerdict string,
) {
	// 必须先占写入槽、再复制正文：槽位已满时这次观察注定被丢弃，绝不能为它分配一份
	// 1 MiB 级别的副本（4xx 风暴下会变成按次放大的分配）。复制只发生在已持有槽位之后，
	// 因此同一时刻在内存里的待加密明文上限就是槽位数 × 单次上限。
	select {
	case o.writeSlots <- struct{}{}:
	default:
		// 写入积压：丢弃这次观察（fail-open），不阻塞上游响应路径，也不无界堆积正文。
		//
		// 丢弃必须留下不含正文的计数，否则运维无法区分「没有错误」和「诊断一直在被丢弃」。
		// 这里只累加计数（不读设置、不复制、不加密、不访问数据库），因此异常路径上仍然是
		// O(1) 且零分配的。
		if dropped, ok := o.recorder.(ErrorDiagnosticDropRecorder); ok {
			dropped.RecordDroppedErrorDiagnostic()
		}
		return
	}
	ordinal := observation.AttemptOrdinal
	if ordinal <= 0 {
		ordinal = nextErrorDiagnosticOrdinal(c, ordinalKey)
	}
	verdict := errorDiagnosticBodyVerdict(observation.BodyVerdict)
	if verdict == ErrorDiagnosticBodyVerdictNotRequested && suppressedVerdict != "" {
		// 传输层这次没有 opt-in 正文采集（因此一个字节都没读到）。若绑定当时门控要求过留存
		// 而稳定密钥缺席，那这次就不是「没要求」而是「要求了、做不到」：改报封闭抑制结论，
		// 让元数据留下稳定原因码，而不是退化成 not_observed。
		//
		// 只在这一种情形改写结论：传输层真的观察到字节（complete／too_large／incomplete）时，
		// 它的结论永远优先，接缝不替它改口。
		verdict = suppressedVerdict
	}
	attempt := ErrorDiagnosticAttempt{
		Protocol:           protocol,
		AttemptIndex:       ordinal,
		Stage:              ErrorDiagnosticStageWire,
		UpstreamStatusCode: observation.StatusCode,
		// 传输层扣住字节的情况（超限／未读完）必须显式上报，否则只能被当成「未观察到正文」。
		BodyVerdict:      verdict,
		BodyReadComplete: diagnosticBodyReadComplete(observation.BodyVerdict),
	}
	if body := observation.RequestBody; len(body) > 0 && len(body) <= ErrorDiagnosticMaxBodyBytes {
		attempt.Body = append([]byte(nil), body...)
	}
	go func() {
		defer func() { <-o.writeSlots }()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(baseCtx), errorDiagnosticWriteTimeout)
		defer cancel()
		_, _ = o.recorder.RecordErrorDiagnostic(ctx, attempt)
	}()
}

// diagnosticBodyReadComplete 报告本次是否存在「被完整读出的出站正文」。
//
// 只有 Complete 为真：TooLarge 虽然也读到了 EOF，但字节被留存上限扣住、正文并未被保留，
// 若在这里报 true 会让元数据出现「已完整读取」的假象。它的结论由 BodyVerdict 单独表达，
// 服务侧也以 verdict 为准（verdict 优先于本字段），因此本字段只描述字节事实。
// Incomplete 是发送中途失败，NotRequested 表示本次没有 opt-in 正文采集。
func diagnosticBodyReadComplete(verdict httpattempt.DiagnosticBodyVerdict) bool {
	return verdict == httpattempt.DiagnosticBodyComplete
}

// errorDiagnosticBodyVerdict 把传输层的观察结论原样映射到诊断服务的稳定枚举。
//
// 未知结论返回空串：服务对空 verdict 的语义是「调用方只给字节，由服务自行判定」，
// 因此未来新增结论只会退化成按字节判定，不会被误报成 complete。
func errorDiagnosticBodyVerdict(verdict httpattempt.DiagnosticBodyVerdict) string {
	switch verdict {
	case httpattempt.DiagnosticBodyNotRequested:
		return ErrorDiagnosticBodyVerdictNotRequested
	case httpattempt.DiagnosticBodyComplete:
		return ErrorDiagnosticBodyVerdictComplete
	case httpattempt.DiagnosticBodyTooLarge:
		return ErrorDiagnosticBodyVerdictTooLarge
	case httpattempt.DiagnosticBodyIncomplete:
		return ErrorDiagnosticBodyVerdictIncomplete
	default:
		return ""
	}
}

// errorDiagnosticOrdinalContextKey 把协议枚举映射到该协议在 gin 上下文里的兜底序号 key。
//
// 显式 switch 而不是拼字符串：协议是封闭白名单，未知协议必须在这里就被拒绝，
// 避免任意字符串被当成可采集协议写进诊断。
func errorDiagnosticOrdinalContextKey(protocol string) string {
	switch protocol {
	case ErrorDiagnosticProtocolMessages:
		return "error_diagnostic_messages_attempt_ordinal"
	case ErrorDiagnosticProtocolChatCompletions:
		return "error_diagnostic_chat_completions_attempt_ordinal"
	case ErrorDiagnosticProtocolResponses:
		return "error_diagnostic_responses_attempt_ordinal"
	default:
		return ""
	}
}

// nextErrorDiagnosticOrdinal 返回本逻辑请求内该协议下一个 1-based 兜底序号。
//
// 它只是兜底：正常路径上尝试序号来自请求审计绑定的 httpattempt.Counter（传输层在真实
// RoundTrip 时记录）。计数缺席时用它而不是写 0，避免把「第几次尝试」丢成未知；
// 每个协议各自计数，互不串号。调用点在「观察成立」时（record 内部），因此同一逻辑请求里
// 成功的发送不占号，同一发送被绑两次也不会重复占号。
func nextErrorDiagnosticOrdinal(c *gin.Context, ordinalKey string) int {
	next := 1
	if c == nil || ordinalKey == "" {
		return next
	}
	if value, ok := c.Get(ordinalKey); ok {
		if current, ok := value.(int); ok && current > 0 {
			next = current + 1
		}
	}
	c.Set(ordinalKey, next)
	return next
}
