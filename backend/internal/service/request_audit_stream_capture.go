package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// requestAuditSSECaptureState is shared only by the bounded slice produced for one
// stream. It retains accounting for the complete upstream event sequence while
// the slice itself contains only the first events that fit the audit limits.
type requestAuditSSECaptureState struct {
	original      int
	originalBytes int
	kept          int
	keptBytes     int
	retainedJSON  int
	stopped       bool
	reason        string
	summary       RequestAuditSSESummary
	fingerprints  map[int]string
}

func appendBoundedRequestAuditSSEEvent(ctx context.Context, dst []RequestAuditSSEEvent, eventName, data string) []RequestAuditSSEEvent {
	if eventName == "" && data == "" {
		return dst
	}

	fp := RequestAuditFingerprintFromContext(ctx)
	state := requestAuditSSECaptureStateFromEvents(dst, fp)
	if state == nil {
		state = newRequestAuditSSECaptureState(dst, fp)
		dst = attachRequestAuditSSECaptureState(dst, state)
	}

	eventBytes := len(data)
	state.original = addRequestAuditSSECount(state.original, 1)
	state.originalBytes = addRequestAuditSSECount(state.originalBytes, eventBytes)

	if state.stopped {
		return appendRequestAuditSSETruncationMarker(dst, state)
	}

	if eventName == "" && data != "[DONE]" {
		eventName = requestAuditSSETypeFromData(data)
	}
	if eventName == "" && data == "[DONE]" {
		eventName = "done"
	}
	if eventName == "" {
		eventName = "unknown"
	}

	index := state.kept
	fingerprinted := requestAuditFingerprintCanDigest(fp)
	skeletonSize := requestAuditSSEEventSkeletonJSONSize(eventName, index, eventBytes, fingerprinted)
	exceedsEvents := state.kept+1 > requestAuditMaxSSEEvents
	nextJSONSize := state.retainedJSON
	if state.kept > 0 {
		nextJSONSize++ // comma between JSON array entries
	}
	nextJSONSize += skeletonSize
	exceedsBytes := nextJSONSize > requestAuditMaxSSEBytes
	if exceedsEvents || exceedsBytes {
		state.stopped = true
		state.reason = "max_bytes"
		if exceedsEvents {
			state.reason = "max_events"
		}
		return appendRequestAuditSSETruncationMarker(dst, state)
	}

	event := RequestAuditSSEEvent{Type: eventName, Bytes: eventBytes, capture: state}
	if fp != nil {
		if digest := fp.DigestEvent(index, eventName, []byte(data)); digest != "" {
			state.fingerprints = fp.Events
		}
	}
	dst = append(dst, event)
	state.kept++
	state.keptBytes = addRequestAuditSSECount(state.keptBytes, eventBytes)
	state.retainedJSON = nextJSONSize
	return dst
}

func requestAuditSSETypeFromData(data string) string {
	if data == "" {
		return ""
	}
	return gjson.Get(data, "type").String()
}

func requestAuditSSECaptureStateFromEvents(events []RequestAuditSSEEvent, fp *RequestAuditFingerprintInput) *requestAuditSSECaptureState {
	if len(events) == 0 {
		return nil
	}
	state := events[0].capture
	if state == nil {
		state = events[len(events)-1].capture
	}
	if state != nil && fp != nil && fp.Events != nil {
		state.fingerprints = fp.Events
	}
	return state
}

func newRequestAuditSSECaptureState(events []RequestAuditSSEEvent, fp *RequestAuditFingerprintInput) *requestAuditSSECaptureState {
	state := &requestAuditSSECaptureState{retainedJSON: 2, fingerprints: nil}
	if fp != nil {
		state.fingerprints = fp.Events
	}
	for i, event := range events {
		state.original = addRequestAuditSSECount(state.original, 1)
		eventBytes := requestAuditSSEEventBytes(event)
		state.originalBytes = addRequestAuditSSECount(state.originalBytes, eventBytes)
		state.kept = addRequestAuditSSECount(state.kept, 1)
		state.keptBytes = addRequestAuditSSECount(state.keptBytes, eventBytes)
		if i > 0 {
			state.retainedJSON++
		}
		fingerprint := ""
		if fp != nil {
			fingerprint = fp.Events[i]
		}
		state.retainedJSON += requestAuditSSEEventSkeletonJSONSizeWithFingerprint(event.Type, i, eventBytes, fingerprint)
	}
	return state
}

func attachRequestAuditSSECaptureState(events []RequestAuditSSEEvent, state *requestAuditSSECaptureState) []RequestAuditSSEEvent {
	for i := range events {
		events[i].capture = state
	}
	return events
}

func appendRequestAuditSSETruncationMarker(events []RequestAuditSSEEvent, state *requestAuditSSECaptureState) []RequestAuditSSEEvent {
	if len(events) > 0 && events[len(events)-1].auditSummary != nil {
		events = events[:len(events)-1]
	}
	state.refreshSummary()
	for state.kept > 0 && (state.kept+1 > requestAuditMaxSSEEvents || state.truncatedJSONSize() > requestAuditMaxSSEBytes) {
		lastIndex := state.kept - 1
		last := events[len(events)-1]
		state.retainedJSON = removeLastRequestAuditSSEEventJSONSize(state.retainedJSON, state.kept-1, last.Type, lastIndex, requestAuditSSEEventBytes(last), state.fingerprints)
		if state.fingerprints != nil {
			delete(state.fingerprints, lastIndex)
		}
		events = events[:len(events)-1]
		state.kept--
		state.keptBytes -= requestAuditSSEEventBytes(last)
		state.refreshSummary()
	}
	marker := RequestAuditSSEEvent{Type: "truncated", capture: state, auditSummary: &state.summary}
	return append(events, marker)
}

func (state *requestAuditSSECaptureState) refreshSummary() {
	state.summary = RequestAuditSSESummary{
		Original:      state.original,
		OriginalBytes: state.originalBytes,
		Kept:          state.kept,
		KeptBytes:     state.keptBytes,
		Dropped:       state.original - state.kept,
		DroppedBytes:  state.originalBytes - state.keptBytes,
		Reason:        state.reason,
	}
}

func (state *requestAuditSSECaptureState) truncatedJSONSize() int {
	marker := RequestAuditEventSkeleton{
		Type: "truncated", Index: state.kept, Truncated: true,
		Original: state.summary.Original, OriginalBytes: state.summary.OriginalBytes,
		Kept: state.summary.Kept, KeptBytes: state.summary.KeptBytes,
		Dropped: state.summary.Dropped, DroppedBytes: state.summary.DroppedBytes,
		Reason: state.summary.Reason,
	}
	encoded, _ := json.Marshal(marker)
	size := state.retainedJSON + len(encoded)
	if state.kept > 0 {
		size++
	}
	return size
}

func requestAuditSSEEventSkeletonJSONSize(eventName string, index, eventBytes int, fingerprinted bool) int {
	fingerprint := ""
	if fingerprinted {
		fingerprint = strings.Repeat("0", 64)
	}
	return requestAuditSSEEventSkeletonJSONSizeWithFingerprint(eventName, index, eventBytes, fingerprint)
}

func requestAuditSSEEventSkeletonJSONSizeWithFingerprint(eventName string, index, eventBytes int, fingerprint string) int {
	skeleton := RequestAuditEventSkeleton{Type: sanitizeRequestAuditEventType(eventName), Index: index, Bytes: eventBytes, Fingerprint: fingerprint}
	encoded, _ := json.Marshal(skeleton)
	return len(encoded)
}

func removeLastRequestAuditSSEEventJSONSize(currentJSONSize, keptAfterRemoval int, eventName string, index, eventBytes int, fingerprints map[int]string) int {
	if keptAfterRemoval == 0 {
		return 2 // []
	}
	fingerprint := ""
	if fingerprints != nil {
		fingerprint = fingerprints[index]
	}
	return currentJSONSize - 1 - requestAuditSSEEventSkeletonJSONSizeWithFingerprint(eventName, index, eventBytes, fingerprint)
}

func requestAuditFingerprintCanDigest(fp *RequestAuditFingerprintInput) bool {
	return fp != nil && fp.record != nil
}

func addRequestAuditSSECount(current, increment int) int {
	maxInt := int(^uint(0) >> 1)
	if increment > 0 && current > maxInt-increment {
		return maxInt
	}
	return current + increment
}
