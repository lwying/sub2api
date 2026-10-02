package service

import (
	"bytes"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	RequestTraceBodyLimit         = 1 << 20
	RequestTraceTotalBodyLimit    = 8 << 20
	RequestTraceStageCountLimit   = 128
	requestTraceLaterStageReserve = 2 << 20
)

// RequestTraceBodyKey distinguishes every observed phase from each real wire
// attempt. It is a value type: callers cannot overwrite an earlier attempt.
type RequestTraceBodyKey struct {
	Stage        string
	AttemptIndex int
	View         string
}

// RequestTraceBodySnapshot deliberately has no arbitrary metadata map. All
// body bytes must pass through a redaction verdict before they reach this type.
type RequestTraceBodySnapshot struct {
	Key                 RequestTraceBodyKey
	State               string
	Reason              string
	ObservedBytes       int64
	RetainedBytes       int
	DroppedEvents       int
	RedactionUnverified bool
	Payload             []byte
}

type requestTraceBodySlot struct {
	key            RequestTraceBodyKey
	contentType    string
	stream         bool
	finished       bool
	observed       int64
	budgetExceeded bool
	buf            []byte
	ssePending     []byte
	sseSkipping    bool
	sseLastNewline bool
	sseCRPending   bool
	sseDropped     int
	result         RequestTraceBodySnapshot
}

func requestTraceBodyKeyValid(key RequestTraceBodyKey) bool {
	if key.AttemptIndex < 0 || key.AttemptIndex > 1000 || key.Stage == "" || len(key.Stage) > 64 {
		return false
	}
	for _, ch := range key.Stage {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' {
			continue
		}
		return false
	}
	switch key.View {
	case "", "transmitted", "decoded", "wire", "received", "downstream":
		return true
	default:
		return false
	}
}

// RequestTraceCollector owns at most an 8 MiB bounded body snapshot, including
// unverified fragments. No collector is constructed when the operator gate is off.
// Callbacks should consume slices immediately; no unredacted content is logged.
type RequestTraceCollector struct {
	mu    sync.Mutex
	order []*requestTraceBodySlot
	slots map[RequestTraceBodyKey]*requestTraceBodySlot
	used  int
	// bodyLimit 是单阶段正文留存上限。默认硬上限 RequestTraceBodyLimit；可选实例预算
	// 只能把它调低，永远不能调高，也不改变总预算与下游预留。
	bodyLimit int
}

// NewRequestTraceCollector 可选地接受一个单阶段正文预算。旧的无参调用行为不变；
// 传入值只允许调低硬上限（<=0 或 >= 硬上限都保持 1 MiB），绝不绕过 queue／数据库保护。
func NewRequestTraceCollector(bodyBudget ...int64) *RequestTraceCollector {
	collector := &RequestTraceCollector{
		slots:     make(map[RequestTraceBodyKey]*requestTraceBodySlot),
		bodyLimit: RequestTraceBodyLimit,
	}
	if len(bodyBudget) > 0 && bodyBudget[0] > 0 && bodyBudget[0] < RequestTraceBodyLimit {
		collector.bodyLimit = int(bodyBudget[0])
	}
	return collector
}

func (c *RequestTraceCollector) startLocked(key RequestTraceBodyKey, contentType string, stream bool) *requestTraceBodySlot {
	if !requestTraceBodyKeyValid(key) {
		return nil
	}
	if existing := c.slots[key]; existing != nil {
		return existing
	}
	if len(c.order) >= RequestTraceStageCountLimit && key.Stage != "client_response" {
		return nil
	}
	slot := &requestTraceBodySlot{key: key, contentType: strings.ToLower(contentType), stream: stream}
	c.order = append(c.order, slot)
	c.slots[key] = slot
	return slot
}

func (c *RequestTraceCollector) StartStage(key RequestTraceBodyKey, contentType string, stream bool) {
	if c == nil || key.Stage == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startLocked(key, contentType, stream)
}

// MemoryBytes reports the bounded byte count currently owned by the collector,
// including unfinished SSE frames. It never returns or logs the bytes themselves.
func (c *RequestTraceCollector) MemoryBytes() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

func (c *RequestTraceCollector) availableLocked(key RequestTraceBodyKey, usedByStage int) int {
	available := RequestTraceTotalBodyLimit - c.used
	if key.Stage != "client_response" {
		available -= requestTraceLaterStageReserve
	}
	if stageRoom := c.bodyLimit - usedByStage; available > stageRoom {
		available = stageRoom
	}
	if available < 0 {
		return 0
	}
	return available
}

func (c *RequestTraceCollector) AppendStage(key RequestTraceBodyKey, bytesIn []byte) {
	if c == nil || key.Stage == "" || len(bytesIn) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	slot := c.startLocked(key, "", false)
	if slot == nil || slot.finished {
		return
	}
	slot.observed += int64(len(bytesIn))
	if slot.stream {
		// Normalize CRLF across transport Read boundaries. A trailing CR is
		// held separately (one byte) until the next callback arrives, so it
		// cannot merge two complete events when split as "\r" + "\n".
		normalized := make([]byte, 0, len(bytesIn)+1)
		for _, b := range bytesIn {
			if slot.sseCRPending {
				normalized = append(normalized, '\n')
				slot.sseCRPending = false
				if b == '\n' {
					continue
				}
			}
			if b == '\r' {
				slot.sseCRPending = true
				continue
			}
			normalized = append(normalized, b)
		}
		if len(normalized) > 0 {
			c.appendSSELocked(slot, normalized)
		}
		return
	}
	allowed := c.availableLocked(key, len(slot.buf))
	if allowed < len(bytesIn) {
		slot.budgetExceeded = true
	}
	if allowed > len(bytesIn) {
		allowed = len(bytesIn)
	}
	if allowed > 0 {
		slot.buf = append(slot.buf, bytesIn[:allowed]...)
		c.used += allowed
	}
}

// A stream is stored as complete frames only. Until a blank line is seen, the
// fragment is never attached to the retained event bytes. Keep one bounded
// pending frame; count later complete frames as dropped when the budget is gone.
func (c *RequestTraceCollector) appendSSELocked(slot *requestTraceBodySlot, chunk []byte) {
	for len(chunk) > 0 {
		if slot.sseSkipping {
			// An oversized frame must be consumed through its next separator,
			// including separators split across Read boundaries. Only subsequent
			// complete events are eligible for retention.
			consumed := 0
			if slot.sseLastNewline && chunk[0] == '\n' {
				// The separator opened at the end of the previous chunk; this
				// leading newline closes it, so this chunk needs no scan.
				consumed = 1
			} else if boundary := bytes.Index(chunk, []byte("\n\n")); boundary >= 0 {
				consumed = boundary + 2
			}
			if consumed > 0 {
				slot.sseSkipping = false
				slot.sseLastNewline = false
				slot.sseDropped++
				chunk = chunk[consumed:]
				continue
			}
			slot.sseLastNewline = chunk[len(chunk)-1] == '\n'
			return
		}
		next := bytes.Index(chunk, []byte("\n\n"))
		if next < 0 && len(slot.ssePending) > 0 && slot.ssePending[len(slot.ssePending)-1] == '\n' && chunk[0] == '\n' {
			// The terminator crossed two body reads. Finish the event without
			// treating the second newline as part of the following frame.
			if c.availableLocked(slot.key, len(slot.ssePending)+len(slot.buf)) == 0 {
				c.used -= len(slot.ssePending)
				slot.ssePending = nil
				slot.sseDropped++
				slot.budgetExceeded = true
				chunk = chunk[1:]
				continue
			}
			slot.ssePending = append(slot.ssePending, '\n')
			c.used++
			chunk = chunk[1:]
			event := slot.ssePending
			c.used -= len(event)
			slot.ssePending = nil
			redacted, _, verified := redactTraceSSEEvent(event)
			if !verified {
				slot.result.RedactionUnverified = true
			}
			if room := c.availableLocked(slot.key, len(slot.buf)); len(redacted) <= room {
				slot.buf = append(slot.buf, redacted...)
				c.used += len(redacted)
			} else {
				slot.budgetExceeded = true
				slot.sseDropped++
			}
			continue
		}
		if next < 0 {
			room := c.availableLocked(slot.key, len(slot.ssePending)+len(slot.buf))
			if len(chunk) > room {
				slot.budgetExceeded = true
				slot.sseSkipping = true
				slot.sseLastNewline = chunk[len(chunk)-1] == '\n'
				c.used -= len(slot.ssePending)
				slot.ssePending = nil
				return
			}
			slot.ssePending = append(slot.ssePending, chunk...)
			c.used += len(chunk)
			return
		}
		if next+2 > c.availableLocked(slot.key, len(slot.ssePending)+len(slot.buf)) {
			slot.budgetExceeded = true
			slot.sseDropped++
			c.used -= len(slot.ssePending)
			slot.ssePending = nil
			chunk = chunk[next+2:]
			continue
		}
		slot.ssePending = append(slot.ssePending, chunk[:next+2]...)
		c.used += next + 2
		chunk = chunk[next+2:]
		event := slot.ssePending
		c.used -= len(event)
		slot.ssePending = nil
		if len(event) > c.bodyLimit {
			slot.budgetExceeded = true
			slot.sseDropped++
			continue
		}
		redacted, _, verified := redactTraceSSEEvent(event)
		if !verified {
			slot.result.RedactionUnverified = true
		}
		room := c.availableLocked(slot.key, len(slot.buf))
		if len(redacted) > room {
			slot.budgetExceeded = true
			slot.sseDropped++
			continue
		}
		slot.buf = append(slot.buf, redacted...)
		c.used += len(redacted)
	}
}

func redactTraceSSEEvent(frame []byte) ([]byte, bool, bool) {
	if !utf8.Valid(frame) {
		return append([]byte(nil), frame...), false, false
	}
	lines := bytes.Split(frame, []byte("\n"))
	output := make([][]byte, 0, len(lines))
	var credential bool
	verified := true
	var hasData bool
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			hasData = true
			value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(value, []byte("[DONE]")) {
				output = append(output, line)
				continue
			}
			if redacted, found, ok := RedactRequestTraceJSON(value); ok {
				line = append([]byte("data: "), redacted...)
				credential = credential || found
			} else {
				verified = false
			}
		}
		output = append(output, line)
	}
	return bytes.Join(output, []byte("\n")), credential, verified && (hasData || bytes.HasPrefix(frame, []byte(":")))
}

func (c *RequestTraceCollector) FinishStage(key RequestTraceBodyKey, complete bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	slot := c.slots[key]
	if slot == nil || slot.finished {
		return
	}
	slot.finished = true
	result := RequestTraceBodySnapshot{Key: key, ObservedBytes: slot.observed, DroppedEvents: slot.sseDropped}
	if slot.stream {
		if slot.sseCRPending {
			c.appendSSELocked(slot, []byte{'\n'})
			slot.sseCRPending = false
			result.DroppedEvents = slot.sseDropped
		}
		if len(slot.ssePending) > 0 || slot.sseSkipping {
			result.DroppedEvents++
		}
		result.Payload = append([]byte(nil), slot.buf...)
		result.RedactionUnverified = slot.result.RedactionUnverified
		c.used -= len(slot.ssePending)
	} else {
		redacted, knownCredential, valid := RedactRequestTraceJSON(slot.buf)
		if valid && !slot.budgetExceeded {
			// An upstream may close before its transport-level EOF. A complete
			// valid JSON prefix can still be redacted independently of that fact;
			// the read-completeness verdict is kept in Reason below.
			result.Payload = redacted
		} else if knownCredential {
			// Sanitizing can increase the output size (thousands of small secret
			// fields become thousands of placeholders). If the safe copy cannot
			// fit, never fall back to the original parseable credential values.
			result.Payload = nil
			result.RedactionUnverified = true
		} else {
			result.Payload = append([]byte(nil), slot.buf...)
			result.RedactionUnverified = len(slot.buf) > 0
		}
	}
	if !slot.stream && len(result.Payload) > c.bodyLimit {
		result.Payload = result.Payload[:c.bodyLimit]
		result.RedactionUnverified = true
		slot.budgetExceeded = true
	}
	if slot.observed > 0 && len(result.Payload) == 0 && slot.budgetExceeded && !result.RedactionUnverified {
		result.State = "truncated"
		result.Reason = "truncated"
	} else if slot.observed > 0 && len(result.Payload) == 0 && result.RedactionUnverified {
		result.State = "not_observed"
		result.Reason = "credential_redaction_unverified"
	} else if slot.observed == 0 && len(result.Payload) == 0 {
		result.State = "not_observed"
		result.Reason = "body_not_observed"
	} else if slot.stream && result.DroppedEvents > 0 {
		result.State = "truncated"
		result.Reason = "incomplete_event"
		if !complete {
			result.Reason = "incomplete_read"
		}
		if slot.budgetExceeded {
			result.Reason = "truncated"
		}
		if result.RedactionUnverified {
			result.Reason = "truncated_unverified"
		}
	} else if !complete {
		result.State = "truncated"
		result.Reason = "incomplete_read"
		if result.RedactionUnverified {
			result.State = "redaction_unverified"
		}
	} else if slot.budgetExceeded {
		result.State = "truncated"
		result.Reason = "truncated"
		if result.RedactionUnverified {
			result.Reason = "truncated_unverified"
		}
	} else if result.RedactionUnverified {
		result.State = "redaction_unverified"
		result.Reason = "credential_redaction_unverified"
	} else {
		result.State = "stored"
		result.Reason = "retained"
	}
	if len(result.Payload) > c.bodyLimit {
		result.Payload = result.Payload[:c.bodyLimit]
		result.State = "truncated"
		result.Reason = "truncated_unverified"
		result.RedactionUnverified = true
	}
	result.RetainedBytes = len(result.Payload)
	slot.result = result
	slot.buf = nil
	slot.ssePending = nil
}

// CaptureInboundViews receives already-observed bounded prefixes, not a new
// socket read. If decoded JSON contains a known credential, its compressed raw
// source must not be kept because decompression would recover the secret.
func (c *RequestTraceCollector) CaptureInboundViews(encoding string, raw []byte, rawBytes int64, decoded []byte, decodedBytes int64, complete bool) {
	if c == nil {
		return
	}
	rawKey := RequestTraceBodyKey{Stage: "client_entry", View: "transmitted"}
	decodedKey := RequestTraceBodyKey{Stage: "client_entry", View: "decoded"}
	_, foundCredential, verified := RedactRequestTraceJSON(decoded)
	// Identity encoding has no transformation; a second identical body view
	// would spend another stage and another MiB without explaining new facts.
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		c.StartStage(decodedKey, "application/json", false)
		c.AppendStage(decodedKey, decoded)
		c.FinishStage(decodedKey, complete && decodedBytes <= int64(len(decoded)))
		c.mu.Lock()
		if slot := c.slots[decodedKey]; slot != nil {
			slot.result.ObservedBytes = decodedBytes
			if decodedBytes > int64(len(decoded)) && len(slot.result.Payload) > 0 {
				slot.result.State = "truncated"
				slot.result.Reason = "truncated_unverified"
				slot.result.RedactionUnverified = true
			}
		}
		c.mu.Unlock()
		return
	}
	// A saved compressed source can reconstruct credentials located beyond the
	// decoded view's observed prefix. If the entire decoded representation could
	// not be inspected and verified, the compressed source is omitted. A small
	// complete but malformed decoded body is the explicit unverified-raw exception.
	decodedIncomplete := decodedBytes > int64(len(decoded)) || (!verified && complete && decodedBytes > RequestTraceBodyLimit)
	if encoding != "" && !strings.EqualFold(encoding, "identity") && (foundCredential || decodedIncomplete) {
		c.StartStage(rawKey, "application/octet-stream", false)
		c.mu.Lock()
		rawSlot := c.slots[rawKey]
		if rawSlot == nil {
			c.mu.Unlock()
			return
		}
		rawSlot.finished = true
		rawSlot.observed = rawBytes
		rawSlot.result = RequestTraceBodySnapshot{Key: rawKey, State: "not_observed", Reason: "credential_recoverable_omitted", ObservedBytes: rawBytes}
		c.mu.Unlock()
	} else {
		c.StartStage(rawKey, "application/octet-stream", false)
		c.AppendStage(rawKey, raw)
		c.FinishStage(rawKey, complete && rawBytes <= int64(len(raw)))
		c.mu.Lock()
		if slot := c.slots[rawKey]; slot != nil {
			slot.result.ObservedBytes = rawBytes
			if rawBytes > int64(len(raw)) {
				slot.result.State = "truncated"
				slot.result.Reason = "truncated_unverified"
				slot.result.RedactionUnverified = true
			}
		}
		c.mu.Unlock()
	}
	c.StartStage(decodedKey, "application/json", false)
	c.mu.Lock()
	decodedSlotExists := c.slots[decodedKey] != nil
	c.mu.Unlock()
	if !decodedSlotExists {
		return
	}
	if verified && complete && decodedBytes <= int64(len(decoded)) {
		// The full decoded view has already been verified and redacted. Do not
		// pass a capped prefix through the JSON verifier a second time.
		redacted, _, _ := RedactRequestTraceJSON(decoded)
		c.mu.Lock()
		slot := c.slots[decodedKey]
		allowed := c.availableLocked(decodedKey, 0)
		if allowed > len(redacted) {
			allowed = len(redacted)
		}
		if allowed < len(redacted) {
			slot.budgetExceeded = true
		}
		if allowed > 0 {
			slot.buf = append(slot.buf, redacted[:allowed]...)
			c.used += allowed
		}
		slot.observed = decodedBytes
		slot.finished = true
		slot.result = RequestTraceBodySnapshot{
			Key: decodedKey, State: "stored", Reason: "retained", ObservedBytes: decodedBytes,
			RetainedBytes: len(slot.buf), Payload: append([]byte(nil), slot.buf...),
		}
		if slot.budgetExceeded {
			slot.result.State = "truncated"
			slot.result.Reason = "truncated"
		}
		slot.buf = nil
		c.mu.Unlock()
	} else if foundCredential {
		// Full-body inspection found a known secret, but the sanitized JSON
		// cannot fit the stage budget. Neither the raw prefix nor the compressed
		// source is safe to keep: disclose the gap instead.
		c.mu.Lock()
		slot := c.slots[decodedKey]
		slot.finished = true
		slot.observed = decodedBytes
		slot.result = RequestTraceBodySnapshot{
			Key: decodedKey, State: "not_observed", Reason: "credential_redaction_unverified",
			ObservedBytes: decodedBytes, RedactionUnverified: true,
		}
		c.mu.Unlock()
	} else {
		c.AppendStage(decodedKey, decoded)
		c.FinishStage(decodedKey, complete && decodedBytes <= int64(len(decoded)))
	}
	c.mu.Lock()
	if slot := c.slots[decodedKey]; slot != nil {
		slot.result.ObservedBytes = decodedBytes
		if decodedBytes > int64(len(decoded)) && len(slot.result.Payload) > 0 {
			slot.result.State = "truncated"
			slot.result.Reason = "truncated_unverified"
			slot.result.RedactionUnverified = true
		}
	}
	c.mu.Unlock()
}

// MarkStageEncoding records that the retained upstream payload is still the
// transmitted encoded representation, not a verified decoded SSE stream. Only
// call after the transport reports a Content-Encoding other than identity.
func (c *RequestTraceCollector) MarkStageEncoding(key RequestTraceBodyKey) {
	if c == nil || key.Stage != "upstream_response" || key.View != "received" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if slot := c.slots[key]; slot != nil && slot.finished {
		if len(slot.result.Payload) > 0 {
			slot.result.State = "redaction_unverified"
			slot.result.Reason = "encoded_body_unverified"
			slot.result.RedactionUnverified = true
		} else if slot.result.ObservedBytes > 0 {
			slot.result.State = "truncated"
			slot.result.Reason = "encoded_body_unverified"
			slot.result.RedactionUnverified = true
		}
	}
}

// MarkStageGap attaches a bounded, code-only reason to an already finalized
// observation. It never reads or persists the failed raw body a second time.
func (c *RequestTraceCollector) MarkStageGap(key RequestTraceBodyKey, reason string) {
	if c == nil || reason != "decode_failed" && reason != "read_failed" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if slot := c.slots[key]; slot != nil && slot.finished && slot.result.State == "not_observed" {
		slot.result.Reason = reason
	}
}

// Snapshots returns copies with no arbitrary stage.Metadata field. Persist only
// these redaction-checked values, never an untrusted observer's raw metadata.
func (c *RequestTraceCollector) Snapshots() []RequestTraceBodySnapshot {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]RequestTraceBodySnapshot, 0, len(c.order))
	for _, slot := range c.order {
		if !slot.finished {
			continue
		}
		copyResult := slot.result
		copyResult.Payload = append([]byte(nil), slot.result.Payload...)
		out = append(out, copyResult)
	}
	return out
}
