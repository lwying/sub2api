package service

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const requestTraceFactsLimit = 3072

var requestTraceFactToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./:+-]{0,127}$`)

// RequestTraceStageFacts is the only persisted projection of client and wire
// scalars. It never contains observer-owned raw URLs, headers or free-form JSON.
// A missing value is omitted rather than presented as an observed empty value.
type RequestTraceStageFacts struct {
	Method                 string      `json:"method,omitempty"`
	URL                    string      `json:"url,omitempty"`
	URLOmitted             bool        `json:"url_omitted,omitempty"`
	RequestHeaders         http.Header `json:"request_headers,omitempty"`
	RequestHeadersOmitted  int         `json:"request_headers_omitted,omitempty"`
	ResponseHeaders        http.Header `json:"response_headers,omitempty"`
	ResponseHeadersOmitted int         `json:"response_headers_omitted,omitempty"`
	AccountID              int64       `json:"account_id,omitempty"`
	Model                  string      `json:"model,omitempty"`
	Protocol               string      `json:"protocol,omitempty"`
	ValueProtocol          string      `json:"value_protocol,omitempty"`
	Status                 int         `json:"status,omitempty"`
	StartedAt              *time.Time  `json:"started_at,omitempty"`
	EndedAt                *time.Time  `json:"ended_at,omitempty"`
}

func requestTraceFactTokenOrEmpty(value string) string {
	if requestTraceFactToken.MatchString(value) {
		return value
	}
	return ""
}

func newRequestTraceStageFacts(method string, target *url.URL, headers http.Header) RequestTraceStageFacts {
	redactedURL, omitted := RedactRequestTraceURL(target)
	redactedHeaders := RedactRequestTraceHeaders(headers)
	facts := RequestTraceStageFacts{
		Method: requestTraceFactTokenOrEmpty(method), URL: redactedURL, URLOmitted: omitted,
		RequestHeaders: redactedHeaders.Values, RequestHeadersOmitted: redactedHeaders.Omitted,
	}
	facts.TrimToBudget()
	return facts
}

func NewRequestTraceClientFacts(method string, target *url.URL, headers http.Header) RequestTraceStageFacts {
	return newRequestTraceStageFacts(method, target, headers)
}

func NewRequestTraceAttemptFacts(method string, target *url.URL, requestHeaders http.Header, responseHeaders http.Header,
	accountID int64, model, protocol, valueProtocol string, status int, startedAt, endedAt *time.Time) RequestTraceStageFacts {
	facts := newRequestTraceStageFacts(method, target, requestHeaders)
	redacted := RedactRequestTraceHeaders(responseHeaders)
	facts.ResponseHeaders = redacted.Values
	facts.ResponseHeadersOmitted = redacted.Omitted
	if accountID > 0 {
		facts.AccountID = accountID
	}
	facts.Model = requestTraceFactTokenOrEmpty(model)
	facts.Protocol = requestTraceFactTokenOrEmpty(protocol)
	facts.ValueProtocol = requestTraceFactTokenOrEmpty(valueProtocol)
	if status >= 100 && status <= 599 {
		facts.Status = status
	}
	if startedAt != nil && !startedAt.IsZero() {
		started := startedAt.UTC()
		facts.StartedAt = &started
	}
	if endedAt != nil && !endedAt.IsZero() && facts.StartedAt != nil && !endedAt.Before(*facts.StartedAt) {
		ended := endedAt.UTC()
		facts.EndedAt = &ended
	}
	facts.TrimToBudget()
	return facts
}

// TrimToBudget reserves a bounded, omission-marked projection before it leaves
// the request path; no original header or URL bytes survive this projection.
func (f *RequestTraceStageFacts) TrimToBudget() {
	for f != nil {
		encoded, err := json.Marshal(f)
		if err == nil && len(encoded) <= requestTraceFactsLimit {
			return
		}
		// Header names are sorted by the redactor. Drop the lexicographically last
		// field and report its values rather than silently losing observations.
		if len(f.ResponseHeaders) > 0 {
			f.ResponseHeadersOmitted += dropRequestTraceHeader(f.ResponseHeaders)
		} else if len(f.RequestHeaders) > 0 {
			f.RequestHeadersOmitted += dropRequestTraceHeader(f.RequestHeaders)
		} else if f.URL != "" {
			f.URL = ""
			f.URLOmitted = true
		} else if f.Model != "" {
			f.Model = ""
		} else if f.Protocol != "" {
			f.Protocol = ""
		} else if f.ValueProtocol != "" {
			f.ValueProtocol = ""
		} else {
			return
		}
	}
}

func dropRequestTraceHeader(headers http.Header) int {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	key := keys[len(keys)-1]
	count := len(headers[key])
	delete(headers, key)
	return count
}

// ValidRequestTraceStageFacts is a second trust boundary, applied by the queue,
// repository and readers. An arbitrary map or caller-supplied credential field
// cannot be smuggled into the stage's JSONB column or disclosure DTO.
func ValidRequestTraceStageFacts(stage string, facts *RequestTraceStageFacts) bool {
	if facts == nil {
		return true
	}
	if stage != "client_metadata" && stage != "wire_attempt" || facts.RequestHeadersOmitted < 0 || facts.ResponseHeadersOmitted < 0 ||
		facts.AccountID < 0 || facts.Status < 0 || facts.Status > 599 ||
		facts.Method != requestTraceFactTokenOrEmpty(facts.Method) || facts.Model != requestTraceFactTokenOrEmpty(facts.Model) ||
		facts.Protocol != requestTraceFactTokenOrEmpty(facts.Protocol) || facts.ValueProtocol != requestTraceFactTokenOrEmpty(facts.ValueProtocol) {
		return false
	}
	if stage == "client_metadata" && (facts.AccountID != 0 || facts.Protocol != "" || facts.ValueProtocol != "" || facts.Model != "" ||
		facts.Status != 0 || facts.StartedAt != nil || facts.EndedAt != nil || len(facts.ResponseHeaders) != 0) {
		return false
	}
	if facts.URL != "" && facts.URLOmitted {
		return false
	}
	if facts.URL != "" {
		if !utf8.ValidString(facts.URL) || len(facts.URL) > requestTraceMaxURLBytes || strings.ContainsAny(facts.URL, "\r\n\x00") {
			return false
		}
		parsed, err := url.Parse(facts.URL)
		if err != nil {
			return false
		}
		clean, omitted := RedactRequestTraceURL(parsed)
		if omitted || clean != facts.URL {
			return false
		}
	}
	for _, headers := range []http.Header{facts.RequestHeaders, facts.ResponseHeaders} {
		if len(headers) == 0 {
			continue
		}
		// A fact set must already be the redacted projection of itself: a header
		// that redaction would rewrite or drop means a raw credential survived
		// into the projection.
		clean := RedactRequestTraceHeaders(headers)
		if clean.Omitted != 0 || !reflect.DeepEqual(clean.Values, headers) {
			return false
		}
	}
	if facts.EndedAt != nil && (facts.StartedAt == nil || facts.EndedAt.Before(*facts.StartedAt)) {
		return false
	}
	encoded, err := json.Marshal(facts)
	return err == nil && len(encoded) <= requestTraceFactsLimit
}

// CloneRequestTraceStageFacts validates the typed projection before copying it
// into the asynchronous queue and again when reading a stored JSONB value.
func CloneRequestTraceStageFacts(stage string, facts *RequestTraceStageFacts) *RequestTraceStageFacts {
	if facts == nil || !ValidRequestTraceStageFacts(stage, facts) {
		return nil
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil
	}
	var copied RequestTraceStageFacts
	if json.Unmarshal(encoded, &copied) != nil {
		return nil
	}
	return &copied
}
