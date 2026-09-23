package httpattempt

import (
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var requestHeaderNames = map[string]string{
	"host":                        "Host",
	"user-agent":                  "User-Agent",
	"content-type":                "Content-Type",
	"accept":                      "Accept",
	"accept-encoding":             "Accept-Encoding",
	"anthropic-version":           "Anthropic-Version",
	"anthropic-beta":              "Anthropic-Beta",
	"x-app":                       "X-App",
	"x-request-id":                "X-Request-ID",
	"x-client-request-id":         "X-Client-Request-ID",
	"x-stainless-retry-count":     "X-Stainless-Retry-Count",
	"x-stainless-timeout":         "X-Stainless-Timeout",
	"x-stainless-lang":            "X-Stainless-Lang",
	"x-stainless-package-version": "X-Stainless-Package-Version",
	"x-stainless-os":              "X-Stainless-OS",
	"x-stainless-arch":            "X-Stainless-Arch",
	"x-stainless-runtime":         "X-Stainless-Runtime",
	"x-stainless-runtime-version": "X-Stainless-Runtime-Version",
	"x-stainless-helper-method":   "x-stainless-helper-method",
}

// These recognized names carry free text or opaque identifiers, so only their presence is retained.
var requestPresenceHeaders = map[string]struct{}{
	"user-agent":          {},
	"anthropic-beta":      {},
	"x-app":               {},
	"x-request-id":        {},
	"x-client-request-id": {},
}

var requestSecretHeaders = map[string]string{
	"authorization":       "Authorization",
	"cookie":              "Cookie",
	"api-key":             "API-Key",
	"x-api-key":           "X-Api-Key",
	"proxy-authorization": "Proxy-Authorization",
}

var responseValueHeaders = map[string]string{
	"content-type":  "Content-Type",
	"cache-control": "Cache-Control",
}

var responseRateLimitHeaders = map[string]string{
	"requests-limit":          "Anthropic-Ratelimit-Requests-Limit",
	"requests-remaining":      "Anthropic-Ratelimit-Requests-Remaining",
	"requests-reset":          "Anthropic-Ratelimit-Requests-Reset",
	"input-tokens-limit":      "Anthropic-Ratelimit-Input-Tokens-Limit",
	"input-tokens-remaining":  "Anthropic-Ratelimit-Input-Tokens-Remaining",
	"input-tokens-reset":      "Anthropic-Ratelimit-Input-Tokens-Reset",
	"output-tokens-limit":     "Anthropic-Ratelimit-Output-Tokens-Limit",
	"output-tokens-remaining": "Anthropic-Ratelimit-Output-Tokens-Remaining",
	"output-tokens-reset":     "Anthropic-Ratelimit-Output-Tokens-Reset",
}

var responsePresenceHeaders = map[string]string{
	"x-request-id":       "X-Request-ID",
	"request-id":         "Request-ID",
	"set-cookie":         "Set-Cookie",
	"www-authenticate":   "Www-Authenticate",
	"location":           "Location",
	"proxy-authenticate": "Proxy-Authenticate",
}

const (
	// headerPresenceLiteral is the only value a reconstructed aggregate marker may carry.
	headerPresenceLiteral = "present"

	sensitiveHeaderPresentMarker = "Sensitive-Header-Present"
	otherHeaderPresentMarker     = "Other-Header-Present"
)

// syntheticPresenceMarkers collapses attacker-controlled unknown header names into fixed markers.
// They are the only names allowed to cross a snapshot boundary: re-injection accepts the exact
// presence marker and nothing else, so a snapshot can neither smuggle a raw name nor a raw value
// through these keys, and a marker can never degrade into a different marker.
var syntheticPresenceMarkers = map[string]string{
	"sensitive-header-present": sensitiveHeaderPresentMarker,
	"other-header-present":     otherHeaderPresentMarker,
}

// SanitizeRequestHeaders retains only safe protocol metadata from an outbound request.
// Free-text values are represented by presence only; credentials are presence-only, and
// arbitrary header names are collapsed to one bounded aggregate marker to avoid persisting
// untrusted names that may themselves contain prompts or credentials. Host values are
// syntax-checked but can still reveal internal domains; callers must apply instance isolation.
func SanitizeRequestHeaders(headers http.Header) map[string]any {
	out := make(map[string]any)
	if headers == nil {
		return out
	}
	for rawName, values := range headers {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if canonical, ok := syntheticPresenceMarkers[name]; ok {
			if isPresenceLiteral(values) {
				out[canonical] = map[string]any{"present": true}
			}
			continue
		}
		if canonical, ok := requestHeaderNames[name]; ok {
			if _, presenceOnly := requestPresenceHeaders[name]; presenceOnly {
				if hasNonEmptyValue(values) {
					out[canonical] = map[string]any{"present": true}
				}
				continue
			}
			if safe := safeRequestHeaderValues(name, values); len(safe) > 0 {
				out[canonical] = headerValue(safe)
			}
			continue
		}
		if canonical, ok := requestSecretHeaders[name]; ok {
			if hasNonEmptyValue(values) {
				out[canonical] = map[string]any{"present": true}
			}
			continue
		}
		if hasNonEmptyValue(values) {
			recordUnknownHeader(out, name)
		}
	}
	return out
}

// SanitizeResponseHeaders retains bounded response protocol facts, known request IDs as
// presence-only, numeric values for a closed Anthropic rate-limit field set, and no raw
// arbitrary header names or credential-like values.
func SanitizeResponseHeaders(headers http.Header) map[string]any {
	out := make(map[string]any)
	if headers == nil {
		return out
	}
	for rawName, values := range headers {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if canonical, ok := syntheticPresenceMarkers[name]; ok {
			if isPresenceLiteral(values) {
				out[canonical] = map[string]any{"present": true}
			}
			continue
		}
		if canonical, ok := responseValueHeaders[name]; ok {
			if safe := safeResponseHeaderValues(name, values); len(safe) > 0 {
				out[canonical] = headerValue(safe)
			}
			continue
		}
		if canonical, ok := responsePresenceHeaders[name]; ok {
			if hasNonEmptyValue(values) {
				out[canonical] = map[string]any{"present": true}
			}
			continue
		}
		if canonical, ok := responseRateLimitHeaderName(name); ok {
			if safe := safeUnsignedIntegerValues(values, 20); len(safe) > 0 {
				out[canonical] = headerValue(safe)
			}
			continue
		}
		if hasNonEmptyValue(values) {
			recordUnknownHeader(out, name)
		}
	}
	return out
}

// HeaderFromSanitizedMap reconstructs only recognized safe or presence-only request headers plus the
// synthetic aggregate presence markers. It intentionally drops unknown names and reduces historical
// free-text values to presence.
func HeaderFromSanitizedMap(values map[string]any) http.Header {
	return headerFromSanitizedMap(values, false)
}

// ResponseHeaderFromSanitizedMap reconstructs only recognized safe or presence-only response headers
// plus the synthetic aggregate presence markers.
func ResponseHeaderFromSanitizedMap(values map[string]any) http.Header {
	return headerFromSanitizedMap(values, true)
}

// SanitizeResponseHeaderMap validates a response-header snapshot at another trust boundary.
func SanitizeResponseHeaderMap(values map[string]any) map[string]any {
	out := SanitizeResponseHeaders(ResponseHeaderFromSanitizedMap(values))
	reinjectSyntheticPresenceMarkers(out, values)
	return out
}

// SanitizeRequestHeaderMap validates a request-header snapshot at another trust boundary.
func SanitizeRequestHeaderMap(values map[string]any) map[string]any {
	out := SanitizeRequestHeaders(HeaderFromSanitizedMap(values))
	reinjectSyntheticPresenceMarkers(out, values)
	return out
}

// reinjectSyntheticPresenceMarkers restores only the exact aggregate presence markers. Any other
// value (false, raw text, extra keys, non-presence shapes) is rejected, so a stale or forged
// marker value can never cross the boundary as a marker.
func reinjectSyntheticPresenceMarkers(out, values map[string]any) {
	for lower, canonical := range syntheticPresenceMarkers {
		if !isPresentOnly(presenceMarkerValue(values, lower)) {
			continue
		}
		out[canonical] = map[string]any{"present": true}
	}
}

func presenceMarkerValue(values map[string]any, lower string) any {
	for rawName, value := range values {
		if strings.EqualFold(strings.TrimSpace(rawName), lower) {
			return value
		}
	}
	return nil
}

func headerFromSanitizedMap(values map[string]any, response bool) http.Header {
	out := make(http.Header)
	for rawName, value := range values {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if canonical, ok := syntheticPresenceMarkers[name]; ok {
			if isPresentOnly(value) {
				out[canonical] = []string{headerPresenceLiteral}
			}
			continue
		}
		if response {
			if canonical, ok := responseValueHeaders[name]; ok {
				if safe := safeResponseHeaderValues(name, stringValues(value)); len(safe) > 0 {
					out[canonical] = safe
				}
				continue
			}
			if canonical, ok := responsePresenceHeaders[name]; ok {
				if valueIndicatesPresence(value) {
					out[canonical] = []string{"present"}
				}
				continue
			}
			if canonical, ok := responseRateLimitHeaderName(name); ok {
				if safe := safeUnsignedIntegerValues(stringValues(value), 20); len(safe) > 0 {
					out[canonical] = safe
				}
				continue
			}
			continue
		}
		if canonical, ok := requestHeaderNames[name]; ok {
			if _, presenceOnly := requestPresenceHeaders[name]; presenceOnly {
				if valueIndicatesPresence(value) {
					out[canonical] = []string{"present"}
				}
				continue
			}
			if safe := safeRequestHeaderValues(name, stringValues(value)); len(safe) > 0 {
				out[canonical] = safe
			}
			continue
		}
		if canonical, ok := requestSecretHeaders[name]; ok && valueIndicatesPresence(value) {
			out[canonical] = []string{"present"}
		}
	}
	return out
}

func stringValues(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []string:
		return append([]string(nil), typed...)
	case []any:
		if len(typed) > 4 {
			return nil
		}
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil
			}
			values = append(values, text)
		}
		return values
	default:
		return nil
	}
}

func valueIndicatesPresence(value any) bool {
	if isPresentOnly(value) {
		return true
	}
	return hasNonEmptyValue(stringValues(value))
}

// isPresenceLiteral accepts only the reconstructed marker form emitted by headerFromSanitizedMap.
func isPresenceLiteral(values []string) bool {
	return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), headerPresenceLiteral)
}

func isPresentOnly(value any) bool {
	mapValue, ok := value.(map[string]any)
	if !ok || len(mapValue) != 1 {
		return false
	}
	present, ok := mapValue["present"].(bool)
	return ok && present
}

func safeRequestHeaderValues(name string, values []string) []string {
	if len(values) == 0 || len(values) > 4 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if len(value) == 0 || len(value) > 128 {
			return nil
		}
		safe, ok := safeRequestHeaderValue(name, value)
		if !ok {
			return nil
		}
		out = append(out, safe)
	}
	return out
}

func safeRequestHeaderValue(name, value string) (string, bool) {
	switch name {
	case "host":
		return strings.ToLower(value), validHost(value)
	case "content-type":
		return safeMediaType(value, "application/json", "application/x-ndjson", "application/octet-stream")
	case "accept":
		return safeAccept(value)
	case "accept-encoding":
		return safeAcceptEncoding(value)
	case "anthropic-version":
		_, err := time.Parse("2006-01-02", value)
		return value, err == nil
	case "x-stainless-retry-count":
		n, err := strconv.Atoi(value)
		return value, err == nil && n >= 0 && n <= 100
	case "x-stainless-timeout":
		n, err := strconv.Atoi(value)
		return value, err == nil && n > 0 && n <= 120000
	case "x-stainless-lang":
		return value, oneOfFold(value, "js", "python", "ruby", "go", "java", "php", "dotnet", "rust", "swift", "kotlin", "unknown")
	case "x-stainless-package-version", "x-stainless-runtime-version":
		return value, validNumericVersion(value)
	case "x-stainless-os":
		return value, oneOfFold(value, "linux", "macos", "windows", "freebsd", "openbsd", "netbsd", "unknown")
	case "x-stainless-arch":
		return value, oneOfFold(value, "x86_64", "amd64", "arm64", "aarch64", "x86", "i386", "i686", "ia32", "unknown")
	case "x-stainless-runtime":
		return value, oneOfFold(value, "node", "deno", "bun", "python", "ruby", "go", "java", "php", "dotnet", "rust", "swift", "kotlin", "unknown")
	case "x-stainless-helper-method":
		return value, oneOfFold(value, "stream", "create", "retrieve", "list", "delete", "update")
	default:
		return "", false
	}
}

func safeMediaType(value string, allowed ...string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || !oneOfFold(mediaType, allowed...) {
		return "", false
	}
	return strings.ToLower(mediaType), true
}

func safeAccept(value string) (string, bool) {
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > 4 {
		return "", false
	}
	allowed := []string{"application/json", "application/x-ndjson", "application/octet-stream", "text/event-stream", "text/plain", "application/*", "text/*", "*/*"}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		mediaRange, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || !oneOfFold(mediaRange, allowed...) {
			return "", false
		}
		canonical := strings.ToLower(mediaRange)
		if len(params) > 1 {
			return "", false
		}
		if quality, ok := params["q"]; ok {
			q, err := strconv.ParseFloat(quality, 64)
			if err != nil || q < 0 || q > 1 || len(quality) > 5 {
				return "", false
			}
			canonical += ";q=" + strconv.FormatFloat(q, 'f', -1, 64)
		}
		out = append(out, canonical)
	}
	return strings.Join(out, ", "), true
}

func safeAcceptEncoding(value string) (string, bool) {
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > 4 {
		return "", false
	}
	allowed := []string{"gzip", "deflate", "br", "zstd", "identity", "*"}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		encoding, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || !oneOfFold(encoding, allowed...) || len(params) > 1 {
			return "", false
		}
		canonical := strings.ToLower(encoding)
		if quality, ok := params["q"]; ok {
			q, err := strconv.ParseFloat(quality, 64)
			if err != nil || q < 0 || q > 1 || len(quality) > 5 {
				return "", false
			}
			canonical += ";q=" + strconv.FormatFloat(q, 'f', -1, 64)
		}
		out = append(out, canonical)
	}
	return strings.Join(out, ", "), true
}

func validHost(value string) bool {
	if len(value) == 0 || len(value) > 253 || strings.ContainsAny(value, " \t\r\n/@?#") {
		return false
	}
	host := value
	if parsedHost, port, err := net.SplitHostPort(value); err == nil {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
		host = parsedHost
	} else if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	} else if strings.Contains(value, ":") && net.ParseIP(value) == nil {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func validNumericVersion(value string) bool {
	value = strings.TrimPrefix(strings.ToLower(value), "v")
	if value == "" || len(value) > 32 {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 9 {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func safeResponseHeaderValues(name string, values []string) []string {
	if len(values) == 0 || len(values) > 4 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if len(value) == 0 || len(value) > 128 {
			return nil
		}
		var safe string
		switch name {
		case "content-type":
			mediaType, _, err := mime.ParseMediaType(value)
			if err != nil || !oneOfFold(mediaType, "application/json", "application/x-ndjson", "text/event-stream", "text/plain", "application/octet-stream") {
				return nil
			}
			safe = strings.ToLower(mediaType)
		case "cache-control":
			safe = safeCacheControl(value)
			if safe == "" {
				return nil
			}
		default:
			return nil
		}
		out = append(out, safe)
	}
	return out
}

func safeUnsignedIntegerValues(values []string, maxDigits int) []string {
	if len(values) == 0 || len(values) > 4 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maxDigits {
			return nil
		}
		for _, r := range value {
			if r < '0' || r > '9' {
				return nil
			}
		}
		out = append(out, value)
	}
	return out
}

func responseRateLimitHeaderName(name string) (string, bool) {
	const prefix = "anthropic-ratelimit-"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	canonical, ok := responseRateLimitHeaders[strings.TrimPrefix(name, prefix)]
	return canonical, ok
}

func safeCacheControl(value string) string {
	allowed := map[string]string{
		"no-cache":        "no-cache",
		"no-store":        "no-store",
		"private":         "private",
		"public":          "public",
		"must-revalidate": "must-revalidate",
		"immutable":       "immutable",
	}
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > 4 {
		return ""
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		directive := strings.ToLower(strings.TrimSpace(part))
		canonical, ok := allowed[directive]
		if !ok {
			return ""
		}
		out = append(out, canonical)
	}
	return strings.Join(out, ", ")
}

func oneOfFold(value string, choices ...string) bool {
	for _, choice := range choices {
		if strings.EqualFold(value, choice) {
			return true
		}
	}
	return false
}

func isSecretHeaderName(name string) bool {
	return strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "pass")
}

// All unknown names are intentionally collapsed to fixed markers. Even a header name may
// contain attacker-controlled prompt or credential text, so it is never persisted verbatim.
func recordUnknownHeader(out map[string]any, name string) {
	if isSecretHeaderName(name) {
		out[sensitiveHeaderPresentMarker] = map[string]any{"present": true}
		return
	}
	out[otherHeaderPresentMarker] = map[string]any{"present": true}
}

func hasNonEmptyValue(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func headerValue(values []string) any {
	if len(values) == 1 {
		return values[0]
	}
	return append([]string(nil), values...)
}
