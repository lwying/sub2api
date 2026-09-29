package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	requestTraceRedacted        = "[REDACTED]"
	requestTraceMaxDepth        = 32
	requestTraceMaxHeaderFields = 128
	requestTraceMaxHeaderValue  = 4096
	requestTraceMaxURLBytes     = 8192
)

// RequestTraceRedactedHeaders contains independently copied header values. Credentials
// are fully replaced rather than partially masked; omission is explicitly counted.
// Neither the caller's header map nor its string slice is retained.
type RequestTraceRedactedHeaders struct {
	Values  http.Header
	Omitted int
}

// requestTraceProtocolCounterNames lists protocol usage counters (Anthropic,
// OpenAI and Gemini/Vertex shapes) whose names contain "token". The generic
// audit heuristic treats such names as credential channels and would erase
// precisely the usage facts a trace exists to explain, so they are restored
// here. The list is closed on purpose: a structural "ends with tokens /
// tokencount / tokensdetails" rule would also exempt access_tokens,
// refresh_tokens and session_tokens, which are credentials. A counter that is
// missing here is redacted, which is fail-closed and visible.
var requestTraceProtocolCounterNames = func() map[string]struct{} {
	names := []string{
		// Limits.
		"max_tokens", "max_output_tokens", "max_completion_tokens", "max_prompt_tokens", "budget_tokens",
		// Responses / Messages / Chat Completions usage.
		"input_tokens", "output_tokens", "prompt_tokens", "completion_tokens", "total_tokens",
		"cached_tokens", "cache_creation_tokens", "cache_read_tokens", "reasoning_tokens", "audio_tokens",
		"text_tokens", "image_tokens",
		// Prompt caching details.
		"cache_creation_input_tokens", "cache_read_input_tokens",
		"cache_creation_5m_tokens", "cache_creation_1h_tokens",
		"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens",
		// Prediction and moderation breakdowns.
		"accepted_prediction_tokens", "rejected_prediction_tokens",
		// Nested *_details objects and their counters.
		"input_tokens_details", "output_tokens_details", "prompt_tokens_details", "completion_tokens_details",
		"cache_creation_input_tokens_details", "cache_read_input_tokens_details",
		"cached_tokens_details", "audio_tokens_details", "reasoning_tokens_details",
		"accepted_prediction_tokens_details", "rejected_prediction_tokens_details",
		// Gemini / Vertex token counts.
		"prompt_token_count", "candidates_token_count", "total_token_count",
		"cached_content_token_count", "thoughts_token_count", "tool_use_prompt_token_count",
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[auditNormalizeBodyKey(name)] = struct{}{}
	}
	return set
}()

func requestTraceCredentialName(name string) bool {
	normalized := auditNormalizeBodyKey(name)
	// The credential deny-list wins over the counter allow-list. The two are
	// disjoint today; this ordering keeps a future name collision fail-closed.
	switch normalized {
	case "authorization", "proxyauthorization", "authentication", "xauthentication", "xauth", "xproxyauth", "cookie", "setcookie",
		"wwwenticate", "wwwauthenticate", "proxyauthenticate",
		"apikey", "xapikey", "xgoogapikey", "xamzsignature", "xamzsecuritytoken",
		"xamzcredential", "xgoogsignature", "xgoogcredential", "awsaccesskeyid",
		"signature", "xsignature", "credential", "xcredential", "bearer", "xbearer",
		"sig", "se", "sp", "proxy", "proxyurl", "token", "key",
		"session", "accesskey", "clientsecret", "sessionkey", "password", "passwd":
		return true
	}
	if _, isCounter := requestTraceProtocolCounterNames[normalized]; isCounter {
		return false
	}
	return isAuditSensitiveBodyKey(name)
}

func RedactRequestTraceHeaders(headers http.Header) RequestTraceRedactedHeaders {
	out := RequestTraceRedactedHeaders{Values: make(http.Header)}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		values := headers[name]
		if !utf8.ValidString(name) || len(name) == 0 || len(name) > 128 || !httpgutsValidHeaderFieldName(name) {
			out.Omitted += len(values)
			continue
		}
		name = http.CanonicalHeaderKey(name)
		if _, exists := out.Values[name]; !exists && len(out.Values) >= requestTraceMaxHeaderFields {
			out.Omitted += len(values)
			continue
		}
		if requestTraceCredentialName(name) {
			if len(values) > 0 {
				out.Values[name] = []string{requestTraceRedacted}
			}
			continue
		}
		for _, value := range values {
			if !utf8.ValidString(value) || len(value) > requestTraceMaxHeaderValue || strings.ContainsAny(value, "\r\n\x00") {
				out.Omitted++
				continue
			}
			if len(out.Values[name]) >= 8 {
				out.Omitted++
				continue
			}
			out.Values.Add(name, value)
		}
	}
	return out
}

// Explicitly check ASCII HTTP token syntax; names containing control or non-ASCII
// characters must not travel into metadata, logs or persisted JSON as names.
func httpgutsValidHeaderFieldName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		}
		return false
	}
	return true
}

// RedactRequestTraceURL returns an absolute URL or path with known credential
// channels replaced. It does not retain userinfo, fragments or a query that
// cannot be parsed without ambiguity. The boolean means the URL was omitted.
func RedactRequestTraceURL(input *url.URL) (string, bool) {
	if input == nil {
		return "", true
	}
	u := *input
	u.User = nil
	u.Fragment = ""
	u.RawFragment = ""
	if len(u.String()) > requestTraceMaxURLBytes || strings.ContainsAny(u.String(), "\r\n\x00") {
		return "", true
	}
	if u.RawQuery != "" {
		values, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return "", true
		}
		for key := range values {
			if requestTraceCredentialName(key) {
				values[key] = []string{requestTraceRedacted}
			}
		}
		u.RawQuery = values.Encode()
	}
	if u.Opaque != "" {
		// An opaque URL can hide query-like credentials outside RawQuery.
		return "", true
	}
	for _, segment := range strings.Split(u.EscapedPath(), "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return "", true
		}
		if requestTraceCredentialName(decoded) {
			// A following path segment may be the value of a credential name.
			// Omit the URL instead of guessing whether the secret is embedded.
			return "", true
		}
	}
	out := u.String()
	if len(out) > requestTraceMaxURLBytes {
		return "", true
	}
	return out, false
}

// RedactRequestTraceJSON returns a bounded JSON copy and whether a known
// structured credential was seen. verified=false means no whole-body assertion
// can be made (malformed JSON, invalid UTF-8, excessive nesting/keys).
// The caller is allowed to keep such raw bytes only as redaction_unverified.
func RedactRequestTraceJSON(body []byte) (redacted []byte, credential, verified bool) {
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, false, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false, false
	}
	budget := 100_000
	value, credential, verified = requestTraceRedactJSONValue(value, 0, &budget)
	if !verified {
		return nil, credential, false
	}
	redacted, err := json.Marshal(value)
	if err != nil || len(redacted) > RequestTraceBodyLimit {
		return nil, credential, false
	}
	return redacted, credential, true
}

func requestTraceRedactJSONValue(input any, depth int, budget *int) (any, bool, bool) {
	if depth > requestTraceMaxDepth || *budget < 0 {
		return nil, false, false
	}
	var credential bool
	switch value := input.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for name, item := range value {
			*budget--
			if *budget < 0 {
				return nil, credential, false
			}
			if requestTraceCredentialName(name) {
				out[name] = requestTraceRedacted
				credential = true
				continue
			}
			redacted, found, ok := requestTraceRedactJSONValue(item, depth+1, budget)
			if !ok {
				return nil, credential || found, false
			}
			out[name] = redacted
			credential = credential || found
		}
		return out, credential, true
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			redacted, found, ok := requestTraceRedactJSONValue(item, depth+1, budget)
			if !ok {
				return nil, credential || found, false
			}
			out[i] = redacted
			credential = credential || found
		}
		return out, credential, true
	default:
		return input, false, true
	}
}
