package service

// Claude Messages 上游 429 错误诊断的请求／响应头**值**采集（ADR 0005 的头值例外）。
//
// 范围比正文诊断更窄，四个「只」缺一不可：
//   - 只对 Messages 协议（Anthropic 路径）的真实上游尝试；
//   - 只在该次尝试恰好收到 HTTP 429 时（其它 4xx/5xx 一行头值都不写）；
//   - 只保留通过**闭集白名单 + 有界校验**的头**值**，绝不保留原始 http.Header、
//     未知头名，也不保留 Cookie／Set-Cookie／Authorization／X-Api-Key／
//     Proxy-Authorization／WWW-Authenticate 等凭据类头的值；
//   - 只与「本站点的加密留存」绑定：明文不进内存快照之外的任何位置，缺密钥一律不留。
//
// 与正文的关系是**正交**而非包含：正文留存开关（body_retention_enabled）关闭时头值照常
// 采集，头值开关（header_values_enabled）关闭时正文行为一字不改。两者共用同一把加密密钥，
// 但各自有独立的到期时刻、状态与原因码。
//
// 本文件的名单是**持久化边界**的二次收窄，不是净化器：净化器（internal/pkg/httpattempt
// 的 SanitizeClaude*HeaderValues）负责按语义判定「哪个头值得看」并规范化取值；本层负责
// 「即使净化器给了什么，也只肯落库白名单内的名字与有界取值」。两者不一致时本层 fail closed
// （整份快照判为不合格，留下稳定原因码 skipped_invalid_values），绝不部分写入——
// 半个快照会被管理员读成「上游只返回了这些头」，比没有记录更危险。
//
// 「整份或全无」在传输层就已经执行，且范围是**白名单内、有资格的头**：净化器只要丢掉过一个
// 有资格的取值或条目（取值没过取值族校验、超出行数／条目／字节预算），传输层就一个取值都
// 不交出来（DiagnosticHeaderOmitted），本层据此留下稳定原因码 skipped_invalid_values。
// 两类排除不算省略、也不会让快照作废：闭集外的头名（有意排除，真实 429 常见的 Date／
// Content-Length 就在此列）与凭据类头的存在性标记（设计内排除）。因此带 Authorization／
// Cookie、响应带 Date／Content-Length 的正常请求仍然能交出快照；快照只声明「白名单内允许
// 记录的头值」，不断言上游只发了这些头。
//
// 范围判定除了「Messages + 恰好 429」，还有一条**上游形态**条件：Bedrock 之类经
// /v1/messages 入站、真实上游却不是 Claude Messages 的尝试按未采集处理（见
// ErrorDiagnosticHeaderVerdictOutOfScope）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// 头值状态（header_state）：与正文状态同构，管理端只看到状态，不看到内容。
	ErrorDiagnosticHeaderStateNotObserved = "not_observed"
	ErrorDiagnosticHeaderStateStored      = "stored"
	ErrorDiagnosticHeaderStateSkipped     = "skipped"
	ErrorDiagnosticHeaderStateExpired     = "expired"
	ErrorDiagnosticHeaderStatePurged      = "purged"

	// 头值原因码（header_reason）：稳定枚举，各种「没留存」的事实必须分开。
	//
	// 刻意没有 skipped_out_of_scope：不在范围内（非 Messages、非 429）按领域词汇就是
	// **未采集**（not_observed），不是一种跳过；协议与上游状态本来就在同一行上，
	// 运维据此自可分辨。把「不在范围」写成 skipped 会让未采集与采集失败混为一谈。
	ErrorDiagnosticHeaderNotObserved                  = "not_observed"
	ErrorDiagnosticHeaderRetained                     = "retained"
	ErrorDiagnosticHeaderSkippedRetentionDisabled     = "skipped_header_retention_disabled"
	ErrorDiagnosticHeaderSkippedEncryptionUnavailable = "skipped_encryption_unavailable"
	ErrorDiagnosticHeaderSkippedInvalidValues         = "skipped_invalid_values"

	// ErrorDiagnosticHeaderRetention 是加密头值在在线主库的物理保留期（7 天）。
	ErrorDiagnosticHeaderRetention = 7 * 24 * time.Hour

	// ErrorDiagnosticMaxHeaderEntries 是单行允许留存的头值条目上限。
	ErrorDiagnosticMaxHeaderEntries = 24
	// ErrorDiagnosticMaxHeaderNameBytes 是允许留存的头名长度上限。
	ErrorDiagnosticMaxHeaderNameBytes = 64
	// ErrorDiagnosticMaxHeaderValueBytes 是单条头值的长度上限。
	ErrorDiagnosticMaxHeaderValueBytes = 256
	// ErrorDiagnosticMaxHeaderPayloadBytes 是加密前 JSON 载荷的字节上限。
	ErrorDiagnosticMaxHeaderPayloadBytes = 4096
	// ErrorDiagnosticMaxHeaderValuesReadBytes 是解密后允许返回的载荷上限，超出即视为异常。
	ErrorDiagnosticMaxHeaderValuesReadBytes = ErrorDiagnosticMaxHeaderPayloadBytes
)

// 传输层对「本次 429 头值」的观察结论。与正文 verdict 同一风格：稳定字符串，
// 未知取值一律按不合格处理（fail closed），不新增存储枚举。
const (
	// ErrorDiagnosticHeaderVerdictNotRequested：本次没有 opt-in 头值采集（协议不在范围内，
	// 或头值留存开关关闭）。
	ErrorDiagnosticHeaderVerdictNotRequested = "not_requested"
	// ErrorDiagnosticHeaderVerdictNotApplicable：本次 opt-in 了，但这条观察不是上游 429
	// （例如同一 Messages 分支上的 500），按契约不采集头值。
	ErrorDiagnosticHeaderVerdictNotApplicable = "not_applicable"
	// ErrorDiagnosticHeaderVerdictCaptured：本次确实观察到可留存的头值。
	ErrorDiagnosticHeaderVerdictCaptured = "captured"
	// ErrorDiagnosticHeaderVerdictEmpty：本次在范围内，但净化后一条可留存的值都没有
	// （上游没发、或都在白名单之外）。它与「没要求」是不同的事实。
	ErrorDiagnosticHeaderVerdictEmpty = "empty"
	// ErrorDiagnosticHeaderVerdictInvalidValues：调用方给的净化结果形状不可用
	// （例如出现非字符串取值），或传输层无法为这批值背书（净化器丢掉过**有资格**的取值，
	// 因此剩下的取值只是半份快照）。封闭结论：只会导致跳过（skipped_invalid_values）。
	ErrorDiagnosticHeaderVerdictInvalidValues = "invalid_values"
	// ErrorDiagnosticHeaderVerdictOutOfScope：本次尝试的**上游形态**不在头值能力范围内。
	//
	// 它与「不在范围内（非 Messages／非 429）」是同一件事的另一种来路：Bedrock 的入站路由
	// 同样是 /v1/messages、同样可能收到 429，但真实上游是 AWS Bedrock，它发的是 AWS 形态的
	// 请求／响应头，不是 Claude Messages 的限流事实。按领域词汇这仍是**未采集**
	// （not_observed）——不是跳过，因此本文件里也刻意不新增 skipped_out_of_scope 这类原因码。
	ErrorDiagnosticHeaderVerdictOutOfScope = "out_of_scope"
	// ErrorDiagnosticHeaderVerdictSuppressedEncryptionUnavailable：要求过头值留存，但没有
	// 可用稳定密钥，因此调用方按策略**主动扣住**了头值（从未读取、从未复制）。
	// 它只导致跳过，映射到既有原因码 skipped_encryption_unavailable。
	ErrorDiagnosticHeaderVerdictSuppressedEncryptionUnavailable = "suppressed_encryption_unavailable"
)

// ErrorDiagnosticHeaderVerdictAllowed 报告 verdict 是否在允许集合内。
func ErrorDiagnosticHeaderVerdictAllowed(verdict string) bool {
	switch verdict {
	case "", ErrorDiagnosticHeaderVerdictNotRequested, ErrorDiagnosticHeaderVerdictNotApplicable,
		ErrorDiagnosticHeaderVerdictCaptured, ErrorDiagnosticHeaderVerdictEmpty,
		ErrorDiagnosticHeaderVerdictInvalidValues, ErrorDiagnosticHeaderVerdictOutOfScope,
		ErrorDiagnosticHeaderVerdictSuppressedEncryptionUnavailable:
		return true
	default:
		return false
	}
}

// ErrorDiagnosticHeaderValues 是一次尝试的 429 头值快照，按方向分开。
//
// 它只承载**净化并校验后**的值：键是白名单里的规范头名，值是有界的字符串。
// 类型本身不携带任何凭据头的值，也不携带未知头名与存在性标记。
type ErrorDiagnosticHeaderValues struct {
	Request  map[string]string
	Response map[string]string
}

// Empty 报告两个方向都没有可留存的值。
func (v ErrorDiagnosticHeaderValues) Empty() bool {
	return len(v.Request) == 0 && len(v.Response) == 0
}

// EntryCount 返回两个方向的条目总数。
func (v ErrorDiagnosticHeaderValues) EntryCount() int {
	return len(v.Request) + len(v.Response)
}

// errorDiagnosticHeaderValueKind 是头值的校验族。取值按**形状**判定，
// 不做语义猜测：族内规则是闭集与有界长度，而不是「看起来像密钥」这类启发式。
type errorDiagnosticHeaderValueKind uint8

const (
	errorDiagnosticHeaderValueBoundedText errorDiagnosticHeaderValueKind = iota
	errorDiagnosticHeaderValueUnsignedInteger
	errorDiagnosticHeaderValueTimestampOrInteger
	errorDiagnosticHeaderValueRetryAfter
	errorDiagnosticHeaderValueOpaqueID
	errorDiagnosticHeaderValueHost
	errorDiagnosticHeaderValueISODate
	errorDiagnosticHeaderValueBoolean
	errorDiagnosticHeaderValueMediaType
)

// errorDiagnosticHeaderValueRule 是白名单里的一条规则。
type errorDiagnosticHeaderValueRule struct {
	// Canonical 是落库与披露时使用的规范头名。
	Canonical string
	Request   bool
	Response  bool
	Kind      errorDiagnosticHeaderValueKind
	MaxLen    int
}

// errorDiagnosticUnsignedIntegerMaxDigits 是无符号整数值的位数上限。
//
// 与既有响应头净化器的 20 位保持一致：本层只负责「不至于把任意长文本当成数字」，
// 不负责判断上游给的重置时间合不合理。
const errorDiagnosticUnsignedIntegerMaxDigits = 20

// errorDiagnosticHeaderValueRules 是**闭集**白名单：不在这张表里的头名一律不落库。
//
// 名单必须与 httpattempt 的净化器名单保持一致；两边不同步时本层 fail closed
// （整份判不合格并留下 skipped_invalid_values），既不部分写入，也不会把陌生名字写进库。
// 每个头名都按**小写**比较，因此净化器回显的规范大小写差异（例如
// x-stainless-helper-method 与 X-Stainless-Helper-Method）不会造成误判。
var errorDiagnosticHeaderValueRules = map[string]errorDiagnosticHeaderValueRule{
	// 请求侧：客户端与 SDK 形态。它们是 429 复现的关键上下文，但不是凭据。
	"host":              {Canonical: "Host", Request: true, Kind: errorDiagnosticHeaderValueHost, MaxLen: 253},
	"user-agent":        {Canonical: "User-Agent", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 128},
	"anthropic-version": {Canonical: "Anthropic-Version", Request: true, Kind: errorDiagnosticHeaderValueISODate, MaxLen: 10},
	"anthropic-dangerous-direct-browser-access": {Canonical: "Anthropic-Dangerous-Direct-Browser-Access", Request: true, Kind: errorDiagnosticHeaderValueBoolean, MaxLen: 5},
	"anthropic-beta":              {Canonical: "Anthropic-Beta", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: ErrorDiagnosticMaxHeaderValueBytes},
	"accept-language":             {Canonical: "Accept-Language", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 128},
	"accept":                      {Canonical: "Accept", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: ErrorDiagnosticMaxHeaderValueBytes},
	"accept-encoding":             {Canonical: "Accept-Encoding", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 64},
	"content-type":                {Canonical: "Content-Type", Request: true, Response: true, Kind: errorDiagnosticHeaderValueMediaType, MaxLen: 128},
	"x-app":                       {Canonical: "X-App", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 128},
	"x-request-id":                {Canonical: "X-Request-Id", Request: true, Response: true, Kind: errorDiagnosticHeaderValueOpaqueID, MaxLen: 128},
	"x-client-request-id":         {Canonical: "X-Client-Request-Id", Request: true, Kind: errorDiagnosticHeaderValueOpaqueID, MaxLen: 128},
	"x-claude-code-session-id":    {Canonical: "X-Claude-Code-Session-Id", Request: true, Kind: errorDiagnosticHeaderValueOpaqueID, MaxLen: 64},
	"x-stainless-retry-count":     {Canonical: "X-Stainless-Retry-Count", Request: true, Kind: errorDiagnosticHeaderValueUnsignedInteger, MaxLen: errorDiagnosticUnsignedIntegerMaxDigits},
	"x-stainless-timeout":         {Canonical: "X-Stainless-Timeout", Request: true, Kind: errorDiagnosticHeaderValueUnsignedInteger, MaxLen: errorDiagnosticUnsignedIntegerMaxDigits},
	"x-stainless-lang":            {Canonical: "X-Stainless-Lang", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 32},
	"x-stainless-package-version": {Canonical: "X-Stainless-Package-Version", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 32},
	"x-stainless-os":              {Canonical: "X-Stainless-OS", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 32},
	"x-stainless-arch":            {Canonical: "X-Stainless-Arch", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 32},
	"x-stainless-runtime":         {Canonical: "X-Stainless-Runtime", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 32},
	"x-stainless-runtime-version": {Canonical: "X-Stainless-Runtime-Version", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 32},
	"x-stainless-helper-method":   {Canonical: "X-Stainless-Helper-Method", Request: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 32},

	// 响应侧：429 的限流事实。数字与时间戳都是闭集形状，不含自由文本。
	"retry-after":                                 {Canonical: "Retry-After", Response: true, Kind: errorDiagnosticHeaderValueRetryAfter, MaxLen: 32},
	"request-id":                                  {Canonical: "Request-Id", Response: true, Kind: errorDiagnosticHeaderValueOpaqueID, MaxLen: 128},
	"cache-control":                               {Canonical: "Cache-Control", Response: true, Kind: errorDiagnosticHeaderValueBoundedText, MaxLen: 128},
	"anthropic-ratelimit-requests-limit":          {Canonical: "Anthropic-Ratelimit-Requests-Limit", Response: true, Kind: errorDiagnosticHeaderValueUnsignedInteger, MaxLen: errorDiagnosticUnsignedIntegerMaxDigits},
	"anthropic-ratelimit-requests-remaining":      {Canonical: "Anthropic-Ratelimit-Requests-Remaining", Response: true, Kind: errorDiagnosticHeaderValueUnsignedInteger, MaxLen: errorDiagnosticUnsignedIntegerMaxDigits},
	"anthropic-ratelimit-requests-reset":          {Canonical: "Anthropic-Ratelimit-Requests-Reset", Response: true, Kind: errorDiagnosticHeaderValueTimestampOrInteger, MaxLen: 32},
	"anthropic-ratelimit-input-tokens-limit":      {Canonical: "Anthropic-Ratelimit-Input-Tokens-Limit", Response: true, Kind: errorDiagnosticHeaderValueUnsignedInteger, MaxLen: errorDiagnosticUnsignedIntegerMaxDigits},
	"anthropic-ratelimit-input-tokens-remaining":  {Canonical: "Anthropic-Ratelimit-Input-Tokens-Remaining", Response: true, Kind: errorDiagnosticHeaderValueUnsignedInteger, MaxLen: errorDiagnosticUnsignedIntegerMaxDigits},
	"anthropic-ratelimit-input-tokens-reset":      {Canonical: "Anthropic-Ratelimit-Input-Tokens-Reset", Response: true, Kind: errorDiagnosticHeaderValueTimestampOrInteger, MaxLen: 32},
	"anthropic-ratelimit-output-tokens-limit":     {Canonical: "Anthropic-Ratelimit-Output-Tokens-Limit", Response: true, Kind: errorDiagnosticHeaderValueUnsignedInteger, MaxLen: errorDiagnosticUnsignedIntegerMaxDigits},
	"anthropic-ratelimit-output-tokens-remaining": {Canonical: "Anthropic-Ratelimit-Output-Tokens-Remaining", Response: true, Kind: errorDiagnosticHeaderValueUnsignedInteger, MaxLen: errorDiagnosticUnsignedIntegerMaxDigits},
	"anthropic-ratelimit-output-tokens-reset":     {Canonical: "Anthropic-Ratelimit-Output-Tokens-Reset", Response: true, Kind: errorDiagnosticHeaderValueTimestampOrInteger, MaxLen: 32},
}

// errorDiagnosticHeaderValueCredentialMarkers 是**值内**的凭据形态标记（按小写比较）。
//
// 这是对净化器之外的第二道防线：即使净化器误放行了一个看起来像凭据的值，本层也拒绝它。
// 标记刻意选得足够具体，避免误伤正常值——例如 `token-counting-2024-11-01` 这类
// beta 特性名不会被判为凭据，但 `token=`、`Bearer `、`sk-` 会。
var errorDiagnosticHeaderValueCredentialMarkers = []string{
	"bearer ",
	"basic ",
	"sk-",
	"sk_",
	"ghp_",
	"gho_",
	"xoxb-",
	"xoxp-",
	"-----begin",
	"apikey",
	"api_key",
	"api-key",
	"password",
	"passwd",
	"secret",
	"cookie",
	"session=",
	"access_token",
	"refresh_token",
}

// ErrorDiagnosticHeaderScopeApplies 报告本次尝试是否在 429 头值采集范围内。
//
// 范围是「Messages 协议 + 恰好 429」：其它协议与其它 4xx/5xx 一律不采头值。
// 这与正文诊断的覆盖范围（三个协议的全部 4xx/5xx）**互不影响**，
// 因此这里既不读取正文开关，也不改写正文结论。
func ErrorDiagnosticHeaderScopeApplies(protocol string, upstreamStatusCode int) bool {
	return protocol == ErrorDiagnosticProtocolMessages && upstreamStatusCode == http.StatusTooManyRequests
}

// normalizeErrorDiagnosticHeaderValue 按规则校验一条头值并返回规范形式。
//
// 任何一项不合格都返回 ok=false：本层不做截断、不脱敏、不做「看起来还行就放行」的弱判定。
func normalizeErrorDiagnosticHeaderValue(rule errorDiagnosticHeaderValueRule, value string) (string, bool) {
	if len(value) == 0 || len(value) > rule.MaxLen || len(value) > ErrorDiagnosticMaxHeaderValueBytes {
		return "", false
	}
	if !utf8.ValidString(value) {
		return "", false
	}
	if errorDiagnosticHeaderValueHasControlBytes(value) {
		return "", false
	}
	if errorDiagnosticHeaderValueHasCredentialMarker(value) {
		return "", false
	}
	switch rule.Kind {
	case errorDiagnosticHeaderValueBoundedText:
		return value, true
	case errorDiagnosticHeaderValueUnsignedInteger:
		return value, errorDiagnosticUnsignedDigits(value, errorDiagnosticUnsignedIntegerMaxDigits)
	case errorDiagnosticHeaderValueTimestampOrInteger:
		return value, errorDiagnosticUnsignedDigits(value, errorDiagnosticUnsignedIntegerMaxDigits) ||
			errorDiagnosticValidTimestamp(value)
	case errorDiagnosticHeaderValueRetryAfter:
		return value, errorDiagnosticUnsignedDigits(value, errorDiagnosticUnsignedIntegerMaxDigits) ||
			errorDiagnosticValidHTTPDate(value)
	case errorDiagnosticHeaderValueOpaqueID:
		return value, errorDiagnosticValidOpaqueID(value)
	case errorDiagnosticHeaderValueHost:
		return strings.ToLower(value), errorDiagnosticValidHostValue(value)
	case errorDiagnosticHeaderValueISODate:
		_, err := time.Parse("2006-01-02", value)
		return value, err == nil
	case errorDiagnosticHeaderValueBoolean:
		return value, value == "true" || value == "false"
	case errorDiagnosticHeaderValueMediaType:
		return value, errorDiagnosticValidMediaTypeValue(value)
	default:
		// 未知校验族：不认识的规则一律不合格，绝不放行。
		return "", false
	}
}

// NormalizeErrorDiagnosticHeaderValues 对整份快照做持久化边界校验。
//
// 返回值为可在库外安全传递的副本与条目总数；ok=false 表示整份不合格
// （出现未知头名、取值超界、含控制字符、含凭据形态，或条目数超上限）。
// 不合格时**不返回任何部分结果**：调用方必须整份丢弃并留下稳定原因码。
func NormalizeErrorDiagnosticHeaderValues(values ErrorDiagnosticHeaderValues) (ErrorDiagnosticHeaderValues, int, bool) {
	if values.Empty() {
		return ErrorDiagnosticHeaderValues{}, 0, true
	}
	if values.EntryCount() > ErrorDiagnosticMaxHeaderEntries {
		return ErrorDiagnosticHeaderValues{}, 0, false
	}
	normalized := ErrorDiagnosticHeaderValues{}
	var err error
	normalized.Request, err = normalizeErrorDiagnosticHeaderDirection(values.Request, true)
	if err != nil {
		return ErrorDiagnosticHeaderValues{}, 0, false
	}
	normalized.Response, err = normalizeErrorDiagnosticHeaderDirection(values.Response, false)
	if err != nil {
		return ErrorDiagnosticHeaderValues{}, 0, false
	}
	return normalized, normalized.EntryCount(), true
}

func normalizeErrorDiagnosticHeaderDirection(values map[string]string, request bool) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for rawName, value := range values {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if len(name) == 0 || len(name) > ErrorDiagnosticMaxHeaderNameBytes {
			return nil, errors.New("error diagnostic header name out of bounds")
		}
		rule, ok := errorDiagnosticHeaderValueRules[name]
		if !ok || !rule.appliesTo(request) {
			return nil, errors.New("error diagnostic header name is not allowlisted")
		}
		safe, ok := normalizeErrorDiagnosticHeaderValue(rule, value)
		if !ok {
			return nil, errors.New("error diagnostic header value is not acceptable")
		}
		out[rule.Canonical] = safe
	}
	return out, nil
}

func (r errorDiagnosticHeaderValueRule) appliesTo(request bool) bool {
	if request {
		return r.Request
	}
	return r.Response
}

func errorDiagnosticHeaderValueHasControlBytes(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

func errorDiagnosticHeaderValueHasCredentialMarker(value string) bool {
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	for _, marker := range errorDiagnosticHeaderValueCredentialMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func errorDiagnosticUnsignedDigits(value string, maxDigits int) bool {
	if len(value) == 0 || len(value) > maxDigits {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// errorDiagnosticValidTimestamp 接受 RFC3339 或 HTTP-date 形态的重置时间。
//
// 上游把 `Anthropic-Ratelimit-*-Reset` 既发过秒数也发过绝对时刻，两种都是闭集形状；
// 其余自由文本一律不合格。
func errorDiagnosticValidTimestamp(value string) bool {
	if _, err := time.Parse(time.RFC3339, value); err == nil {
		return true
	}
	return errorDiagnosticValidHTTPDate(value)
}

func errorDiagnosticValidHTTPDate(value string) bool {
	_, err := http.ParseTime(value)
	return err == nil
}

// errorDiagnosticValidOpaqueID 校验不透明标识（请求 ID、会话 ID）的形状。
//
// 只接受 UUID／`req_` 这类常见字符集内的短标识：既不做前缀假设（避免上游换格式就采不到），
// 也不放进任意标点与空白（避免承载自由文本）。
func errorDiagnosticValidOpaqueID(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':', c == '+', c == '/', c == '=':
		default:
			return false
		}
	}
	return true
}

// errorDiagnosticValidHostValue 校验 Host 头的形状（不做 DNS 解析，只看语法）。
func errorDiagnosticValidHostValue(value string) bool {
	if len(value) == 0 || len(value) > 253 || strings.ContainsAny(value, " \t\r\n/@?#") {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == ':', c == '[', c == ']':
		default:
			return false
		}
	}
	return true
}

// errorDiagnosticValidMediaTypeValue 校验媒体类型形状：必须是 `type/subtype` 加可选参数，
// 字符集受限，不含引号与反斜杠。
func errorDiagnosticValidMediaTypeValue(value string) bool {
	if value == "" {
		return false
	}
	slash := strings.IndexByte(value, '/')
	if slash <= 0 || slash == len(value)-1 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '/', c == '-', c == '+', c == '.', c == ';', c == '=', c == ' ', c == ',':
		default:
			return false
		}
	}
	return true
}

// ErrorDiagnosticHeaderValuesFromSanitized 把净化器输出（map[string]any）收敛成本层类型。
//
// 两种形状被明确理解：
//   - 字符串取值：进入待校验集合；
//   - `{"present": true}`：已知凭据头的**存在性**标记，不是值，因此**不落库**（本能力只保存值）。
//
// 其余形状视为契约不一致，返回 ok=false（fail closed）。注意本函数只做形状收敛，
// 不做白名单校验：白名单与有界校验由 NormalizeErrorDiagnosticHeaderValues 负责。
func ErrorDiagnosticHeaderValuesFromSanitized(request, response map[string]any) (ErrorDiagnosticHeaderValues, bool) {
	values := ErrorDiagnosticHeaderValues{}
	var err error
	if values.Request, err = errorDiagnosticHeaderValueStrings(request, true); err != nil {
		return ErrorDiagnosticHeaderValues{}, false
	}
	if values.Response, err = errorDiagnosticHeaderValueStrings(response, false); err != nil {
		return ErrorDiagnosticHeaderValues{}, false
	}
	return values, true
}

func errorDiagnosticHeaderValueStrings(sanitized map[string]any, request bool) (map[string]string, error) {
	if len(sanitized) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(sanitized))
	for name, value := range sanitized {
		if marker, isMap := value.(map[string]any); isMap {
			if errorDiagnosticHeaderValuePresenceOnly(marker) {
				// 存在性标记不是值：本能力只保存值，因此这里既不落库也不当作错误。
				continue
			}
			return nil, errors.New("unexpected error diagnostic header value shape")
		}
		text, ok := errorDiagnosticHeaderValuesText(value, name, request)
		if !ok {
			return nil, errors.New("unexpected error diagnostic header value shape")
		}
		if len(text) == 0 {
			// 空值不构成「观察到一个值」，但也不是形状错误：跳过即可。
			continue
		}
		out[name] = text
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// errorDiagnosticHeaderValuesText 把净化器给出的一个头名取值收敛成一个字符串。
//
// 多行同名头按 HTTP 的列表语义合并成 ", " 分隔的一个取值（RFC 9110：多个同名字段行
// 等价于逗号分隔的列表），因此**不会**丢掉除第一行之外的安全取值，也不做任何静默截断。
//
// 但合并只对**按列表取值**的那一族的头名成立（boundedText：Accept／Accept-Language／
// Cache-Control／Anthropic-Beta 等）。结构化取值的头名（不透明 ID、整数、日期、媒体类型、
// 主机名）出现多行时无法用一个取值表达，此时整份快照判为不合格——由调用方留下稳定原因码
// skipped_invalid_values，而不是悄悄只留第一行或拼出一个不在任何取值形状内的字符串。
func errorDiagnosticHeaderValuesText(value any, name string, request bool) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case []string:
		return errorDiagnosticJoinHeaderValues(typed, name, request)
	case []any:
		if len(typed) > errorDiagnosticHeaderValueMaxLines {
			return "", false
		}
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return "", false
			}
			values = append(values, text)
		}
		return errorDiagnosticJoinHeaderValues(values, name, request)
	default:
		return "", false
	}
}

// errorDiagnosticHeaderValueMaxLines 是同一头名允许合并的行数上限（与净化器一致）。
const errorDiagnosticHeaderValueMaxLines = 4

func errorDiagnosticJoinHeaderValues(values []string, name string, request bool) (string, bool) {
	if len(values) == 0 || len(values) > errorDiagnosticHeaderValueMaxLines {
		return "", false
	}
	if len(values) == 1 {
		return values[0], true
	}
	rule, ok := errorDiagnosticHeaderValueRules[strings.ToLower(strings.TrimSpace(name))]
	if !ok || rule.Kind != errorDiagnosticHeaderValueBoundedText {
		// 结构化取值 + 多行：没有可合并成的合法取值，整份不合格（绝不只留第一行）。
		return "", false
	}
	return strings.Join(values, ", "), true
}

// errorDiagnosticHeaderValuePresenceOnly 只承认净化器定义的存在性标记形状：
// 恰好一个键 `present`，值为 true。其余对象形状一律不认。
func errorDiagnosticHeaderValuePresenceOnly(value map[string]any) bool {
	if len(value) != 1 {
		return false
	}
	present, ok := value["present"].(bool)
	return ok && present
}

// EncodeErrorDiagnosticHeaderValues 把校验后的头值编码成确定性的 JSON 载荷。
//
// 确定性很重要：同一份头值必须产生同一份密文，否则「以密文对比是否重复」会成为
// 一条本不存在的旁路。map 的键由 encoding/json 排序，两个方向固定为 request／response。
func EncodeErrorDiagnosticHeaderValues(values ErrorDiagnosticHeaderValues) ([]byte, error) {
	// 字段名与类型一一对应（只有 JSON 标签不同），因此直接转换，避免逐字段抄写。
	payload := errorDiagnosticHeaderValuesPayload(values)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(encoded) > ErrorDiagnosticMaxHeaderPayloadBytes {
		return nil, errors.New("error diagnostic header payload is too large")
	}
	return encoded, nil
}

// DecodeErrorDiagnosticHeaderValues 解码并**重新校验**已解密的载荷。
//
// 解密成功不等于内容可信（密钥错配、密文被替换、旧版本载荷都可能落到这里），
// 因此解码结果必须再过一遍白名单与有界校验；不合格一律返回错误，绝不返回部分结果。
func DecodeErrorDiagnosticHeaderValues(payload []byte) (ErrorDiagnosticHeaderValues, error) {
	if len(payload) == 0 || len(payload) > ErrorDiagnosticMaxHeaderValuesReadBytes {
		return ErrorDiagnosticHeaderValues{}, errors.New("error diagnostic header payload is out of bounds")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded errorDiagnosticHeaderValuesPayload
	if err := decoder.Decode(&decoded); err != nil {
		return ErrorDiagnosticHeaderValues{}, err
	}
	if err := errorDiagnosticHeaderValuesPayloadTrailing(decoder); err != nil {
		return ErrorDiagnosticHeaderValues{}, err
	}
	values := ErrorDiagnosticHeaderValues(decoded)
	normalized, count, ok := NormalizeErrorDiagnosticHeaderValues(values)
	if !ok || count == 0 {
		// 空载荷也是不合格：留存只在确实有条目时才发生，能解出空值说明密文被替换过。
		return ErrorDiagnosticHeaderValues{}, errors.New("error diagnostic header payload failed validation")
	}
	return normalized, nil
}

// errorDiagnosticHeaderValuesPayloadTrailing 拒绝「一个合法 JSON 后面还跟着东西」的载荷。
func errorDiagnosticHeaderValuesPayloadTrailing(decoder *json.Decoder) error {
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("error diagnostic header payload has trailing data")
	}
	return nil
}

type errorDiagnosticHeaderValuesPayload struct {
	Request  map[string]string `json:"request,omitempty"`
	Response map[string]string `json:"response,omitempty"`
}

// ErrorDiagnosticHeaderDecision 是对一条尝试的头值留存结论。
//
// Payload 只在 State == stored 时非空（已编码但**尚未加密**的 JSON）；
// 加密由调用方用同一把密钥完成，失败即退化为 skipped_encryption_unavailable，
// 绝不回退为明文。
type ErrorDiagnosticHeaderDecision struct {
	State      string
	Reason     string
	Payload    []byte
	EntryCount int
}

// Retained 报告本次结论是否允许写入加密头值。
func (d ErrorDiagnosticHeaderDecision) Retained() bool {
	return d.State == ErrorDiagnosticHeaderStateStored && len(d.Payload) > 0
}

// DecideErrorDiagnosticHeaderValues 判定该次尝试的头值状态与原因，并给出可加密的载荷。
//
// 判定顺序固定：范围（Messages + 429）→ 传输层 verdict → 白名单与有界校验 →
// 留存开关 → 密钥可用性 → 编码。越界与不合格只影响头值，不影响该行元数据落库。
//
// captureAllowed 是头值自己的开关（与正文开关无关），cipherAvailable 表示此刻有可用密钥。
// 调用方不能自称 stored，也不能在缺密钥时退化为明文。
func DecideErrorDiagnosticHeaderValues(attempt ErrorDiagnosticAttempt, captureAllowed bool, cipherAvailable bool) ErrorDiagnosticHeaderDecision {
	if attempt.HeaderVerdict == ErrorDiagnosticHeaderVerdictOutOfScope {
		// 上游形态出界（例如 Bedrock）：入站路由与状态码看起来符合也不算采集。
		// 判定刻意放在范围判定**之前**，这样夹带进来的取值也一并作废，而不是走后面的
		// 「有值就按值留存」路径。结论是未采集，不是跳过。
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateNotObserved, Reason: ErrorDiagnosticHeaderNotObserved}
	}
	if !ErrorDiagnosticHeaderScopeApplies(attempt.Protocol, attempt.UpstreamStatusCode) {
		// 不在范围内：这是「未采集」，不是失败，也不能用空字段冒充已采集。
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateNotObserved, Reason: ErrorDiagnosticHeaderNotObserved}
	}
	switch attempt.HeaderVerdict {
	case ErrorDiagnosticHeaderVerdictSuppressedEncryptionUnavailable:
		// 封闭抑制：要求过头值留存、部署拿不出稳定密钥，调用方因此一个字节都没读。
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedEncryptionUnavailable}
	case ErrorDiagnosticHeaderVerdictNotApplicable, ErrorDiagnosticHeaderVerdictEmpty:
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateNotObserved, Reason: ErrorDiagnosticHeaderNotObserved}
	case ErrorDiagnosticHeaderVerdictInvalidValues:
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedInvalidValues}
	case ErrorDiagnosticHeaderVerdictNotRequested:
		// 在范围内却没采：只有一种可能，就是头值留存开关关闭（协议不合适的情形已被上面的范围判定挡掉）。
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedRetentionDisabled}
	case ErrorDiagnosticHeaderVerdictCaptured, "":
		// 空 verdict 表示调用方只给值、由服务自行判定（与正文 verdict 的约定一致）。
	default:
		// 未知 verdict：不认识的结论一律按不合格处理，绝不默认留存。
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedInvalidValues}
	}
	normalized, count, ok := NormalizeErrorDiagnosticHeaderValues(attempt.HeaderValues)
	if !ok {
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedInvalidValues}
	}
	if count == 0 {
		// 声称观察到值却一条都没通过：这是「未采集」，不是「已采集但没留存」。
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateNotObserved, Reason: ErrorDiagnosticHeaderNotObserved}
	}
	if !captureAllowed {
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedRetentionDisabled}
	}
	if !cipherAvailable {
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedEncryptionUnavailable}
	}
	payload, err := EncodeErrorDiagnosticHeaderValues(normalized)
	if err != nil || len(payload) == 0 {
		return ErrorDiagnosticHeaderDecision{State: ErrorDiagnosticHeaderStateSkipped, Reason: ErrorDiagnosticHeaderSkippedInvalidValues}
	}
	return ErrorDiagnosticHeaderDecision{
		State:      ErrorDiagnosticHeaderStateStored,
		Reason:     ErrorDiagnosticHeaderRetained,
		Payload:    payload,
		EntryCount: count,
	}
}

// DescribeHeaderState 把原因码与到期映射成对外的 header_state。
//
// 与正文的 DescribeBodyState 同构：已到期与已物理清除都不得被误报为仍可读。
func DescribeHeaderState(reason string, stored bool, headerExpiresAt time.Time, now time.Time) string {
	if stored {
		if !headerExpiresAt.After(now) {
			return ErrorDiagnosticHeaderStateExpired
		}
		return ErrorDiagnosticHeaderStateStored
	}
	switch reason {
	case ErrorDiagnosticHeaderRetained:
		return ErrorDiagnosticHeaderStatePurged
	case ErrorDiagnosticHeaderNotObserved, "":
		return ErrorDiagnosticHeaderStateNotObserved
	default:
		return ErrorDiagnosticHeaderStateSkipped
	}
}
