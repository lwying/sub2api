package httpattempt

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	claudeHeaderMaxEntries = 40
	claudeHeaderMaxBytes   = 4096
	claudeHeaderMaxValues  = 4
)

var (
	claudeUserAgentPattern = regexp.MustCompile(`(?i)^(?:claude-cli|claude-code)/[0-9]+\.[0-9]+(?:\.[0-9]+)?(?: \([a-z0-9 ,._/-]{1,80}\))?$`)
	claudeBetaPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,55}-20[0-9]{6}$|^[a-z][a-z0-9-]{0,55}-20[0-9]{2}-[0-9]{2}-[0-9]{2}$`)
	claudeAppPattern       = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9._/-]{0,63}$`)
	claudeRequestIDPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|req_[a-zA-Z0-9_-]{1,100})$`)
	claudeUUIDPattern      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	claudeLanguagePattern  = regexp.MustCompile(`(?i)^[a-z]{2,3}(?:-[a-z]{2,4})?(?:;q=(?:0(?:\.[0-9]{1,3})?|1(?:\.0{1,3})?))?(?:,[a-z]{2,3}(?:-[a-z]{2,4})?(?:;q=(?:0(?:\.[0-9]{1,3})?|1(?:\.0{1,3})?))?){0,3}$`)
)

var claudeRequestValueNames = map[string]string{
	"host":              "Host",
	"user-agent":        "User-Agent",
	"anthropic-beta":    "Anthropic-Beta",
	"anthropic-version": "Anthropic-Version",
	"anthropic-dangerous-direct-browser-access": "Anthropic-Dangerous-Direct-Browser-Access",
	"accept":                      "Accept",
	"accept-encoding":             "Accept-Encoding",
	"accept-language":             "Accept-Language",
	"content-type":                "Content-Type",
	"x-app":                       "X-App",
	"x-request-id":                "X-Request-Id",
	"x-client-request-id":         "X-Client-Request-Id",
	"x-claude-code-session-id":    "X-Claude-Code-Session-Id",
	"x-stainless-retry-count":     "X-Stainless-Retry-Count",
	"x-stainless-timeout":         "X-Stainless-Timeout",
	"x-stainless-lang":            "X-Stainless-Lang",
	"x-stainless-package-version": "X-Stainless-Package-Version",
	"x-stainless-os":              "X-Stainless-OS",
	"x-stainless-arch":            "X-Stainless-Arch",
	"x-stainless-runtime":         "X-Stainless-Runtime",
	"x-stainless-runtime-version": "X-Stainless-Runtime-Version",
	"x-stainless-helper-method":   "X-Stainless-Helper-Method",
}

var claudeResponseValueNames = map[string]string{
	"content-type":                                "Content-Type",
	"cache-control":                               "Cache-Control",
	"retry-after":                                 "Retry-After",
	"request-id":                                  "Request-Id",
	"x-request-id":                                "X-Request-Id",
	"anthropic-ratelimit-requests-limit":          "Anthropic-Ratelimit-Requests-Limit",
	"anthropic-ratelimit-requests-remaining":      "Anthropic-Ratelimit-Requests-Remaining",
	"anthropic-ratelimit-requests-reset":          "Anthropic-Ratelimit-Requests-Reset",
	"anthropic-ratelimit-input-tokens-limit":      "Anthropic-Ratelimit-Input-Tokens-Limit",
	"anthropic-ratelimit-input-tokens-remaining":  "Anthropic-Ratelimit-Input-Tokens-Remaining",
	"anthropic-ratelimit-input-tokens-reset":      "Anthropic-Ratelimit-Input-Tokens-Reset",
	"anthropic-ratelimit-output-tokens-limit":     "Anthropic-Ratelimit-Output-Tokens-Limit",
	"anthropic-ratelimit-output-tokens-remaining": "Anthropic-Ratelimit-Output-Tokens-Remaining",
	"anthropic-ratelimit-output-tokens-reset":     "Anthropic-Ratelimit-Output-Tokens-Reset",
}

// Claude value snapshots are an opt-in, short-lived diagnostic surface. They are
// separate from the long-lived request audit header summary, which stays presence-only.
// Unknown names are not recorded: an unknown header can be an authentication channel.
func SanitizeClaudeRequestHeaderValues(headers http.Header) map[string]any {
	return sanitizeClaudeHeaderValues(headers, false)
}

func SanitizeClaudeResponseHeaderValues(headers http.Header) map[string]any {
	return sanitizeClaudeHeaderValues(headers, true)
}

// SanitizeClaudeRequestHeaderValuesWithOmission 与 SanitizeClaudeRequestHeaderValues **同源**：
// 可落库的取值完全一致，额外返回一份**省略摘要**。
//
// 需要它是因为净化器会把闭集外的头名与没通过校验的取值直接丢掉：调用方（服务层）
// 看到的只有筛过之后的结果，无法自己判断「客户端只发了这些」还是「我们只收了这些」。
// 摘要只含计数，因此它可以在不记录任何名字与取值的前提下把这个事实传下去。
func SanitizeClaudeRequestHeaderValuesWithOmission(headers http.Header) (map[string]any, ClaudeHeaderValueOmission) {
	return sanitizeClaudeHeaderValuesWithOmission(headers, false, claudeHeaderValueOmissionAllObserved)
}

// SanitizeClaudeResponseHeaderValuesWithOmission 是响应侧的同上变体。
func SanitizeClaudeResponseHeaderValuesWithOmission(headers http.Header) (map[string]any, ClaudeHeaderValueOmission) {
	return sanitizeClaudeHeaderValuesWithOmission(headers, true, claudeHeaderValueOmissionAllObserved)
}

// SanitizeClaudeRequestHeaderValuesWithEligibleOmission 是 429 错误诊断（ADR 0005 的头值例外）
// 使用的变体：可落库的取值与 SanitizeClaudeRequestHeaderValues 完全一致，省略摘要的口径则
// 只把**有资格进入快照的头名**（闭集内的名字）算成省略。
//
// 两种口径服务两个消费方，刻意不共用：
//   - 旁路（sidecar）值快照的 WithOmission 变体用「观察到但没进快照」的全量口径，表达
//     「这次快照没覆盖全部入站头」；
//   - 429 错误诊断用**有资格**的口径：闭集外的头名是名单本身的信任边界（设计内不采，
//     本能力从不保存它们），因此它们既不能让整份快照作废，也不能被读成「丢了该有的头」。
//     诊断侧据此执行「整份或全无」——**针对白名单内、有资格的头**，而不是针对原始
//     http.Header 的全部头名。
//
// 无论是哪种口径，闭集外的头名与凭据类头的存在性标记都不会进入快照，也不会被记名字。
func SanitizeClaudeRequestHeaderValuesWithEligibleOmission(headers http.Header) (map[string]any, ClaudeHeaderValueOmission) {
	return sanitizeClaudeHeaderValuesWithOmission(headers, false, claudeHeaderValueOmissionEligibleOnly)
}

// SanitizeClaudeResponseHeaderValuesWithEligibleOmission 是响应侧的同上变体。
func SanitizeClaudeResponseHeaderValuesWithEligibleOmission(headers http.Header) (map[string]any, ClaudeHeaderValueOmission) {
	return sanitizeClaudeHeaderValuesWithOmission(headers, true, claudeHeaderValueOmissionEligibleOnly)
}

// claudeHeaderValueOmissionScope 决定「哪些被观察到的条目算省略」。
type claudeHeaderValueOmissionScope uint8

const (
	// claudeHeaderValueOmissionAllObserved 是旁路值快照的口径：闭集外的头名也算省略。
	claudeHeaderValueOmissionAllObserved claudeHeaderValueOmissionScope = iota
	// claudeHeaderValueOmissionEligibleOnly 是 429 错误诊断的口径：只有闭集内的头名
	// （有资格进快照的名字）没进快照才算省略。
	claudeHeaderValueOmissionEligibleOnly
)

// ClaudeHeaderValueOmission 是一次值快照的**省略摘要**：它只回答「有多少被观察到的头
// 没能进入快照」，因此**只含计数**——头名与取值都不会出现在这里，也不会出现在任何
// 日志或载荷里。未知头名可能承载认证通道，「被省略」这个事实可以传达，「省略的是什么」不可以。
//
// 计数口径由产出它的函数决定：WithOmission 变体把闭集外的头名也算成省略（旁路口径），
// WithEligibleOmission 变体只把闭集内、有资格进快照的头名算成省略（429 错误诊断口径）。
// 消费方必须按自己拿到的口径解释，不能把两种口径的计数混用。
//
// 凭据类头（Cookie／Authorization／X-Api-Key／Proxy-Authorization／Set-Cookie／
// WWW-Authenticate）得到的存在性标记是**设计内**的排除，不计入省略：否则每个正常
// Claude Code 请求都会被标成「快照不完整」，这个标记就失去了意义。
type ClaudeHeaderValueOmission struct {
	// OmittedNames 是观察到、但没能进入快照的**头名个数**：闭集外的名字、
	// 无法唯一表示的取值形状，以及超出条目／字节预算而被丢弃的头。
	OmittedNames int
	// RejectedValues 是观察到、但没有通过净化器取值校验的**取值个数**。
	RejectedValues int
}

// Any 报告是否有被观察到的条目没进入快照。
func (o ClaudeHeaderValueOmission) Any() bool {
	return o.OmittedNames > 0 || o.RejectedValues > 0
}

// Merge 合并两处省略摘要（只累加计数），用于把请求与响应两个方向的事实并成一个结论。
func (o ClaudeHeaderValueOmission) Merge(other ClaudeHeaderValueOmission) ClaudeHeaderValueOmission {
	return ClaudeHeaderValueOmission{
		OmittedNames:   o.OmittedNames + other.OmittedNames,
		RejectedValues: o.RejectedValues + other.RejectedValues,
	}
}

// Revalidate after persistence or reconstruction. Presence markers are accepted only
// for named credential headers, never in place of a permitted value.
func SanitizeClaudeRequestHeaderValueMap(values map[string]any) map[string]any {
	return sanitizeClaudeHeaderValueMap(values, false)
}

func SanitizeClaudeResponseHeaderValueMap(values map[string]any) map[string]any {
	return sanitizeClaudeHeaderValueMap(values, true)
}

func sanitizeClaudeHeaderValueMap(values map[string]any, response bool) map[string]any {
	head := make(http.Header, len(values))
	for raw, value := range values {
		name := strings.ToLower(raw)
		if isPresentOnly(value) {
			if response {
				if name == "set-cookie" || name == "www-authenticate" || name == "proxy-authenticate" {
					head[raw] = []string{"present"}
				}
			} else if _, ok := requestSecretHeaders[name]; ok {
				head[raw] = []string{"present"}
			}
			continue
		}
		switch text := value.(type) {
		case string:
			head[raw] = []string{text}
		case []string:
			head[raw] = append([]string(nil), text...)
		case []any:
			if len(text) == 0 || len(text) > claudeHeaderMaxValues {
				continue
			}
			out := make([]string, 0, len(text))
			for _, item := range text {
				str, ok := item.(string)
				if !ok {
					out = nil
					break
				}
				out = append(out, str)
			}
			if out != nil {
				head[raw] = out
			}
		}
	}
	return sanitizeClaudeHeaderValues(head, response)
}

func sanitizeClaudeHeaderValues(headers http.Header, response bool) map[string]any {
	out, _ := sanitizeClaudeHeaderValuesWithOmission(headers, response, claudeHeaderValueOmissionAllObserved)
	return out
}

// sanitizeClaudeHeaderValuesWithOmission 是净化器的唯一实现：它既产出可落库的取值，
// 也产出**只含计数**的省略摘要。scope 决定哪些被观察到的条目算省略（见
// claudeHeaderValueOmissionScope），因此旁路与 429 错误诊断可以各自解释同一份净化结果。
//
// 计数规则刻意保守：只有「观察到、也确实有东西没被收下」的头才计入省略。
//   - 闭集外的头名：**全量口径**计入 OmittedNames（名字本身不记）；有资格口径下不计入，
//     因为名单就是信任边界，闭集外的名字是本能力设计上不采的；
//   - 无法唯一表示的取值个数（0 个或超过 4 个）：闭集内的名字计入 OmittedNames；
//   - 取值没通过校验：按取值个数计入 RejectedValues；
//   - 超出条目／字节预算而整头丢弃：计入 OmittedNames；
//   - 凭据类头的存在性标记：**不计入**，那是设计内的排除，不是能力不足。
func sanitizeClaudeHeaderValuesWithOmission(headers http.Header, response bool, scope claudeHeaderValueOmissionScope) (map[string]any, ClaudeHeaderValueOmission) {
	out := make(map[string]any)
	var omission ClaudeHeaderValueOmission
	if len(headers) == 0 {
		return out, omission
	}
	if len(headers) > claudeHeaderMaxEntries {
		// 整份都收不下：只记有多少个被观察到的头没进快照，不记名字。
		// 有资格口径下只数闭集内、本来能进快照的那些名字。
		omission.OmittedNames = len(headers)
		if scope == claudeHeaderValueOmissionEligibleOnly {
			omission = ClaudeHeaderValueOmission{}
			for rawName := range headers {
				if claudeHeaderValueNameIsEligible(strings.ToLower(strings.TrimSpace(rawName)), response) {
					omission.OmittedNames++
				}
			}
		}
		return out, omission
	}
	keys := make([]string, 0, len(headers))
	for name := range headers {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	bytesUsed := 0
	for _, rawName := range keys {
		name := strings.ToLower(strings.TrimSpace(rawName))
		values := headers[rawName]
		if len(values) == 0 {
			// 空头没有「取值没被收下」这回事，只算「没有观察」。
			continue
		}
		var canonical string
		var eligible bool
		if response {
			canonical, eligible = claudeResponseValueNames[name]
		} else {
			canonical, eligible = claudeRequestValueNames[name]
		}
		if len(values) > claudeHeaderMaxValues {
			if eligible || scope == claudeHeaderValueOmissionAllObserved {
				omission.OmittedNames++
			}
			continue
		}
		if response {
			if name == "set-cookie" || name == "www-authenticate" || name == "proxy-authenticate" {
				if hasNonEmptyValue(values) {
					out[http.CanonicalHeaderKey(name)] = map[string]any{"present": true}
				}
				continue
			}
		} else if secretCanonical, ok := requestSecretHeaders[name]; ok {
			if hasNonEmptyValue(values) {
				out[secretCanonical] = map[string]any{"present": true}
			}
			continue
		}
		if !eligible {
			// 闭集外的名字：观察到了但收不下，也不记名字。
			// 全量口径把它算成「快照没覆盖全部入站头」；
			// 有资格口径下它是名单本身的信任边界，不是「本该留住却丢了」。
			if scope == claudeHeaderValueOmissionAllObserved {
				omission.OmittedNames++
			}
			continue
		}
		safe := make([]string, 0, len(values))
		for _, raw := range values {
			value, valid := safeClaudeHeaderValue(name, strings.TrimSpace(raw), response)
			if !valid {
				safe = nil
				break
			}
			safe = append(safe, value)
		}
		if safe == nil {
			omission.RejectedValues += len(values)
			continue
		}
		newBytes := len(canonical)
		for _, value := range safe {
			newBytes += len(value)
		}
		if bytesUsed+newBytes > claudeHeaderMaxBytes {
			omission.OmittedNames++
			continue
		}
		bytesUsed += newBytes
		out[canonical] = headerValue(safe)
	}
	return out, omission
}

// claudeHeaderValueNameIsEligible 报告一个头名是否有资格进入值快照（闭集内的名字）。
//
// 凭据类头不在其中：它们只做存在性判断，是设计内的排除，从来不是「本该留住的取值」。
func claudeHeaderValueNameIsEligible(name string, response bool) bool {
	if response {
		_, ok := claudeResponseValueNames[name]
		return ok
	}
	_, ok := claudeRequestValueNames[name]
	return ok
}

func safeClaudeHeaderValue(name, value string, response bool) (string, bool) {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
		return "", false
	}
	if response {
		switch name {
		case "request-id", "x-request-id":
			return value, claudeRequestIDPattern.MatchString(value)
		case "retry-after":
			// RFC 9110 允许秒数或 HTTP-date 两种形态，上游两种都发过；两者都是闭集形状。
			return value, validClaudeUnsigned(value, 6) || validClaudeHTTPDate(value)
		case "content-type", "cache-control":
			return safeResponseHeaderValueSingle(name, value)
		default:
			if strings.HasPrefix(name, "anthropic-ratelimit-") {
				if strings.HasSuffix(name, "-reset") {
					return value, validClaudeResetValue(value)
				}
				return value, validClaudeUnsigned(value, 20)
			}
		}
		return "", false
	}
	switch name {
	case "anthropic-dangerous-direct-browser-access":
		return value, value == "true" || value == "false"
	case "user-agent":
		return value, claudeUserAgentPattern.MatchString(value)
	case "anthropic-beta":
		parts := strings.Split(value, ",")
		if len(parts) == 0 || len(parts) > 12 {
			return "", false
		}
		for _, token := range parts {
			if !claudeBetaPattern.MatchString(strings.TrimSpace(token)) {
				return "", false
			}
		}
		return value, true
	case "accept-language":
		return value, claudeLanguagePattern.MatchString(value)
	case "x-app":
		return value, claudeAppPattern.MatchString(value) && !looksLikeCredential(value)
	case "x-request-id", "x-client-request-id":
		return value, claudeRequestIDPattern.MatchString(value)
	case "x-claude-code-session-id":
		return value, claudeUUIDPattern.MatchString(value)
	default:
		return safeRequestHeaderValue(name, value)
	}
}

func safeResponseHeaderValueSingle(name, value string) (string, bool) {
	values := safeResponseHeaderValues(name, []string{value})
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

func validClaudeUnsigned(value string, maxDigits int) bool {
	if len(value) == 0 || len(value) > maxDigits {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	if maxDigits == 6 {
		seconds, err := strconv.ParseUint(value, 10, 32)
		return err == nil && seconds <= 86400
	}
	return true
}

// validClaudeHTTPDate 校验 HTTP-date 形态（RFC 9110 的三种格式之一）。
//
// 只承认这个闭集形状，并加一条年份健全性边界：误把任意自由文本当成时间戳会让
// 「值是时间」这个判断失去意义，而超出该范围的时间戳也不构成可解释的限流事实。
func validClaudeHTTPDate(value string) bool {
	parsed, err := http.ParseTime(value)
	return err == nil && parsed.Year() >= 2020 && parsed.Year() <= 2100
}

// validClaudeResetValue 校验 Anthropic-Ratelimit-*-Reset 的取值：无符号整数（秒数）、
// RFC3339（绝对时刻）或 HTTP-date（绝对时刻）三种形态之一。
func validClaudeResetValue(value string) bool {
	if validClaudeUnsigned(value, 20) {
		return true
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.Year() >= 2020 && parsed.Year() <= 2100
	}
	return validClaudeHTTPDate(value)
}

func looksLikeCredential(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "bearer") || strings.Contains(lower, "sk-")
}
