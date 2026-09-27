package httpattempt

import (
	"net/http"
	"sort"
	"strings"
)

// Protocol-specific value contracts are closed sets. A new header may be copied
// only after its value shape is independently validated for that wire protocol.
var openAIRequestValueNames = map[string]string{
	"host": "Host", "content-type": "Content-Type", "accept": "Accept", "accept-encoding": "Accept-Encoding",
}

var openAIResponseValueNames = map[string]string{
	"content-type": "Content-Type", "cache-control": "Cache-Control", "retry-after": "Retry-After",
}

func SanitizeProtocolRequestHeaderValues(protocol string, headers http.Header) (map[string]any, ClaudeHeaderValueOmission, bool) {
	switch protocol {
	case "anthropic.messages":
		values, omission := SanitizeClaudeRequestHeaderValuesWithOmission(headers)
		return values, omission, true
	case "openai.responses", "openai.chat.completions":
		values, omission := sanitizeOpenAIHeaderValues(headers, openAIRequestValueNames, false)
		return values, omission, true
	default:
		return nil, ClaudeHeaderValueOmission{}, false
	}
}

func SanitizeProtocolResponseHeaderValues(protocol string, headers http.Header) (map[string]any, ClaudeHeaderValueOmission, bool) {
	switch protocol {
	case "anthropic.messages":
		values, omission := SanitizeClaudeResponseHeaderValuesWithOmission(headers)
		return values, omission, true
	case "openai.responses", "openai.chat.completions":
		values, omission := sanitizeOpenAIHeaderValues(headers, openAIResponseValueNames, true)
		return values, omission, true
	default:
		return nil, ClaudeHeaderValueOmission{}, false
	}
}

func SanitizeProtocolRequestHeaderValueMap(protocol string, headers map[string]any) (map[string]any, bool) {
	if protocol == "anthropic.messages" {
		return SanitizeClaudeRequestHeaderValueMap(headers), true
	}
	if protocol != "openai.responses" && protocol != "openai.chat.completions" {
		return nil, false
	}
	return sanitizeOpenAIHeaderValueMap(headers, openAIRequestValueNames, false), true
}

func SanitizeProtocolResponseHeaderValueMap(protocol string, headers map[string]any) (map[string]any, bool) {
	if protocol == "anthropic.messages" {
		return SanitizeClaudeResponseHeaderValueMap(headers), true
	}
	if protocol != "openai.responses" && protocol != "openai.chat.completions" {
		return nil, false
	}
	return sanitizeOpenAIHeaderValueMap(headers, openAIResponseValueNames, true), true
}

func sanitizeOpenAIHeaderValueMap(values map[string]any, allow map[string]string, response bool) map[string]any {
	h := make(http.Header, len(values))
	for name, raw := range values {
		if _, ok := allow[strings.ToLower(name)]; !ok {
			continue
		}
		switch value := raw.(type) {
		case string:
			h[name] = []string{value}
		case []string:
			h[name] = append([]string(nil), value...)
		case []any:
			if len(value) == 0 || len(value) > claudeHeaderMaxValues {
				continue
			}
			out := make([]string, 0, len(value))
			for _, item := range value {
				text, ok := item.(string)
				if !ok {
					out = nil
					break
				}
				out = append(out, text)
			}
			if out != nil {
				h[name] = out
			}
		}
	}
	out, _ := sanitizeOpenAIHeaderValues(h, allow, response)
	return out
}

func sanitizeOpenAIHeaderValues(headers http.Header, allow map[string]string, response bool) (map[string]any, ClaudeHeaderValueOmission) {
	out := map[string]any{}
	omission := ClaudeHeaderValueOmission{}
	if len(headers) == 0 {
		return out, omission
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	bytesUsed := 0
	for _, rawName := range names {
		name := strings.ToLower(rawName)
		canonical, ok := allow[name]
		if !ok {
			if !isOpenAICredentialHeader(name) {
				omission.OmittedNames++
			}
			continue
		}
		if len(out) >= claudeHeaderMaxEntries {
			omission.OmittedNames++
			continue
		}
		values := headers[rawName]
		if len(values) == 0 || len(values) > claudeHeaderMaxValues {
			omission.OmittedNames++
			continue
		}
		safe := make([]string, 0, len(values))
		for _, value := range values {
			value = strings.TrimSpace(value)
			if len(value) == 0 || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") || !validOpenAIHeaderValue(name, value, response) {
				omission.RejectedValues++
				safe = nil
				break
			}
			safe = append(safe, value)
		}
		if safe == nil {
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
		if len(safe) == 1 {
			out[canonical] = safe[0]
		} else {
			out[canonical] = safe
		}
	}
	return out, omission
}

func validOpenAIHeaderValue(name, value string, response bool) bool {
	if response {
		switch name {
		case "retry-after":
			return validClaudeUnsigned(value, 6) || validClaudeHTTPDate(value)
		case "content-type", "cache-control":
			_, ok := safeResponseHeaderValueSingle(name, value)
			return ok
		}
		return false
	}
	switch name {
	case "host":
		return validHost(value)
	case "content-type", "accept", "accept-encoding":
		_, ok := safeRequestHeaderValue(name, value)
		return ok
	}
	return false
}

func isOpenAICredentialHeader(name string) bool {
	_, ok := requestSecretHeaders[name]
	return ok || name == "set-cookie" || name == "www-authenticate" || name == "proxy-authenticate"
}
