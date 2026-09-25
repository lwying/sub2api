package httpattempt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// DiagnosticRequestBodyLimit bounds the opted-in request-body tee. The tee keeps at
// most DiagnosticRequestBodyLimit+1 bytes so a body that is exactly at the limit can
// still be told apart from a truncated one.
const DiagnosticRequestBodyLimit int64 = 1 << 20

// DiagnosticBodyVerdict reports whether the bytes a diagnostic carries are the
// complete outbound request body of one attempt. The values are stable reason codes
// for a diagnostic that must not retain the body.
type DiagnosticBodyVerdict uint8

const (
	// DiagnosticBodyNotRequested: the request did not opt in to body capture.
	DiagnosticBodyNotRequested DiagnosticBodyVerdict = iota
	// DiagnosticBodyComplete: every byte the transport consumed was kept and is within the limit.
	DiagnosticBodyComplete
	// DiagnosticBodyTooLarge: the outbound body exceeded DiagnosticRequestBodyLimit bytes.
	DiagnosticBodyTooLarge
	// DiagnosticBodyIncomplete: the transport never consumed the outbound body to EOF, so
	// the send did not complete and the bytes must not be treated as the outbound body.
	DiagnosticBodyIncomplete
)

// String returns the stable reason code for a diagnostic that is not retaining the body.
func (v DiagnosticBodyVerdict) String() string {
	switch v {
	case DiagnosticBodyNotRequested:
		return "not_requested"
	case DiagnosticBodyComplete:
		return "complete"
	case DiagnosticBodyTooLarge:
		return "too_large"
	case DiagnosticBodyIncomplete:
		return "incomplete"
	default:
		return "unknown"
	}
}

// DiagnosticHeaderVerdict reports what one observation can say about the upstream 429
// header values of an attempt. Header values are a Messages-only, 429-only capability and
// are captured only for a request that explicitly opted in.
type DiagnosticHeaderVerdict uint8

const (
	// DiagnosticHeaderNotRequested: the request did not opt in to header-value capture
	// (protocol out of scope, or header-value retention switched off).
	DiagnosticHeaderNotRequested DiagnosticHeaderVerdict = iota
	// DiagnosticHeaderNotApplicable: the request opted in, but this observation is not an
	// upstream 429, so by contract no header is read or kept.
	DiagnosticHeaderNotApplicable
	// DiagnosticHeaderCaptured: this observation carries sanitized header values.
	DiagnosticHeaderCaptured
	// DiagnosticHeaderEmpty: in scope, but nothing survived sanitization, so there is no
	// value to keep. It is not the same fact as "not requested".
	DiagnosticHeaderEmpty
	// DiagnosticHeaderOmitted: in scope, and the sanitizer dropped at least one observed
	// entry that was eligible for the snapshot (an allowlisted name whose value failed its
	// shape check, or an allowlisted entry over the size budget), so the values that did
	// survive are a partial view of the eligible set. A partial snapshot read as a complete
	// one is worse than no snapshot, so this observation carries no header values at all and
	// the caller records the fact, not the fragment.
	//
	// Names outside the closed set never produce this verdict: the closed set is the trust
	// boundary, so unlisted names are excluded by design (never read into the snapshot,
	// never stored, never named) rather than treated as something that was lost.
	DiagnosticHeaderOmitted
)

// String returns the stable reason code for this header verdict.
func (v DiagnosticHeaderVerdict) String() string {
	switch v {
	case DiagnosticHeaderNotRequested:
		return "not_requested"
	case DiagnosticHeaderNotApplicable:
		return "not_applicable"
	case DiagnosticHeaderCaptured:
		return "captured"
	case DiagnosticHeaderEmpty:
		return "empty"
	case DiagnosticHeaderOmitted:
		return "sanitizer_omitted"
	default:
		return "unknown"
	}
}

// DiagnosticObservation is one real upstream HTTP 4xx/5xx attempt, observed after the
// RoundTrip that received it. It deliberately carries no credentials, no raw headers, no
// account identity and no model name: those stay with the caller that bound the
// observer, which is also the only place that knows the covered branch. The only header
// material it can carry is the sanitizer's already-restricted value set (see
// RequestHeaderValues), never an http.Header.
type DiagnosticObservation struct {
	ObservedAt time.Time
	// AttemptOrdinal is the 1-based position of this send among the logical request's
	// real upstream attempts. Zero means the logical request is not tracked.
	AttemptOrdinal int
	StatusCode     int
	// RequestBytes is the number of bytes the transport consumed from the outbound
	// request body for this attempt.
	RequestBytes int64
	BodyVerdict  DiagnosticBodyVerdict
	// RequestBody holds the bytes the transport actually consumed, and only when
	// BodyVerdict is DiagnosticBodyComplete. It is valid for the duration of the
	// callback only: the seam zeroes the buffer as soon as the callback returns, so a
	// caller that needs the bytes must copy them inside the callback.
	RequestBody []byte

	// HeaderVerdict reports whether this observation carries sanitized 429 header values.
	// DiagnosticHeaderOmitted means it deliberately carries none, because the sanitizer had
	// to drop at least one eligible (allowlisted) entry: a partial snapshot must never be
	// read as the complete eligible set.
	HeaderVerdict DiagnosticHeaderVerdict
	// RequestHeaderValues and ResponseHeaderValues hold the sanitizer output for an
	// upstream 429 on an opted-in Messages attempt: allowlisted names with bounded values,
	// and presence markers (never values) for known credential headers. They are the
	// sanitizer's own copies, so they stay valid after the callback returns, and they are
	// all-or-nothing over the eligible set: nil unless HeaderVerdict is
	// DiagnosticHeaderCaptured. They are the allowlisted subset of the wire headers, never
	// a claim that the upstream sent nothing else — unlisted names are excluded by design.
	RequestHeaderValues  map[string]any
	ResponseHeaderValues map[string]any
}

// DiagnosticObserver is an explicit, per-request opt-in to upstream error
// diagnostics. Nothing is observed without it.
type DiagnosticObserver struct {
	// CaptureRequestBody opts in to a bounded copy of the outbound request body. It is
	// false for metadata-only diagnostics, in which case no request bytes are retained.
	CaptureRequestBody bool
	// CaptureErrorHeaders opts in to sanitized 429 header VALUES (the Messages-only,
	// 429-only diagnostic capability). It is deliberately independent of
	// CaptureRequestBody: header values are sanitized on an upstream 429 even when body
	// capture is off, and no body is read just because headers were requested.
	//
	// Only the sanitizer's restricted value set is read: allowlisted names with bounded
	// values, and presence markers instead of values for known credential headers. The
	// snapshot is all-or-nothing over that eligible set: if the sanitizer dropped any
	// allowlisted entry (a rejected value, a size-budget overflow), no value is handed over
	// at all, so a caller never persists a fragment as a complete snapshot. Unlisted header
	// names are excluded by design and neither fail the snapshot nor appear in it.
	CaptureErrorHeaders bool
	// OnUpstreamError is invoked synchronously once for every real RoundTrip that
	// received an HTTP 4xx/5xx. It must stay cheap and must not block the request path,
	// and it must copy RequestBody if it needs it beyond the call.
	OnUpstreamError func(DiagnosticObservation)
}

type diagnosticObserverContextKey struct{}

// WithDiagnosticObserver opts one request in to upstream error diagnostics. A nil
// observer leaves the context unchanged, so callers can gate the opt-in directly.
func WithDiagnosticObserver(ctx context.Context, observer *DiagnosticObserver) context.Context {
	if observer == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, diagnosticObserverContextKey{}, observer)
}

// DiagnosticObserverFromContext reports the observer this request opted in with.
func DiagnosticObserverFromContext(ctx context.Context) (*DiagnosticObserver, bool) {
	if ctx == nil {
		return nil, false
	}
	observer, ok := ctx.Value(diagnosticObserverContextKey{}).(*DiagnosticObserver)
	return observer, ok && observer != nil
}

// DiagnosticBodyCapture holds the bounded copy of one attempt's outbound body. Only an
// explicitly opted-in request gets one, it is never shared across attempts, it never
// starts a goroutine and it never reads the body through GetBody.
type DiagnosticBodyCapture struct {
	limit    int64
	buf      []byte
	consumed int64
	overflow bool
	eof      bool
	released bool
}

// NewDiagnosticBodyCapture returns the tee for one attempt, or nil when the request did
// not explicitly opt in to body capture.
func NewDiagnosticBodyCapture(req *http.Request) *DiagnosticBodyCapture {
	if req == nil {
		return nil
	}
	observer, ok := DiagnosticObserverFromContext(req.Context())
	if !ok || !observer.CaptureRequestBody {
		return nil
	}
	capture := &DiagnosticBodyCapture{limit: DiagnosticRequestBodyLimit}
	if req.Body == nil || req.Body == http.NoBody {
		// Nothing is sent, so the outbound body is complete and empty.
		capture.eof = true
	}
	return capture
}

// Consume records exactly what the transport read from the outbound body. A read error
// other than io.EOF leaves the capture incomplete: the bytes were never fully sent, so
// they can never be reported as the outbound body.
func (c *DiagnosticBodyCapture) Consume(p []byte, err error) {
	if c == nil || c.released {
		return
	}
	c.consumed += int64(len(p))
	if len(p) > 0 {
		if remaining := c.limit + 1 - int64(len(c.buf)); remaining > 0 {
			if int64(len(p)) > remaining {
				c.buf = append(c.buf, p[:remaining]...)
				c.overflow = true
			} else {
				c.buf = append(c.buf, p...)
			}
		} else {
			c.overflow = true
		}
	}
	if errors.Is(err, io.EOF) {
		c.eof = true
	}
}

// Release zeroes the captured bytes and drops them. It is safe to call more than once
// and must run as soon as the observation returns.
func (c *DiagnosticBodyCapture) Release() {
	if c == nil || c.released {
		return
	}
	c.released = true
	for i := range c.buf {
		c.buf[i] = 0
	}
	c.buf = nil
}

// snapshot returns the verdict, the consumed byte count and the bytes to hand over. The
// bytes are nil unless the whole outbound body was consumed within the limit.
func (c *DiagnosticBodyCapture) snapshot() (DiagnosticBodyVerdict, int64, []byte) {
	if c == nil || c.released {
		return DiagnosticBodyNotRequested, 0, nil
	}
	switch {
	case c.overflow || int64(len(c.buf)) > c.limit:
		return DiagnosticBodyTooLarge, c.consumed, nil
	case !c.eof:
		return DiagnosticBodyIncomplete, c.consumed, nil
	case len(c.buf) == 0:
		return DiagnosticBodyComplete, c.consumed, nil
	default:
		return DiagnosticBodyComplete, c.consumed, c.buf
	}
}

// ObserveUpstreamError emits the diagnostic for one real RoundTrip that received an
// HTTP 4xx/5xx. It must only be called after a RoundTrip returned a response: it never
// runs before a send, never invents a status for a failed send, and never infers
// eligibility from the request URL. It does nothing when the request did not opt in.
func ObserveUpstreamError(req *http.Request, attempt *Attempt, resp *http.Response, capture *DiagnosticBodyCapture) {
	if req == nil || resp == nil || resp.StatusCode < 400 || resp.StatusCode > 599 {
		return
	}
	observer, ok := DiagnosticObserverFromContext(req.Context())
	if !ok || observer.OnUpstreamError == nil {
		return
	}

	observation := DiagnosticObservation{
		ObservedAt:  time.Now(),
		StatusCode:  resp.StatusCode,
		BodyVerdict: DiagnosticBodyNotRequested,
	}
	if attempt != nil {
		observation.AttemptOrdinal = attempt.ordinal()
		observation.RequestBytes = attempt.requestBytes()
	}
	if capture != nil {
		verdict, consumed, body := capture.snapshot()
		if verdict != DiagnosticBodyNotRequested {
			observation.BodyVerdict = verdict
			observation.RequestBytes = consumed
			observation.RequestBody = body
		}
	}
	observeUpstreamErrorHeaderValues(&observation, observer, req, resp)
	observer.OnUpstreamError(observation)
}

// observeUpstreamErrorHeaderValues fills the sanitized 429 header values of one observation.
//
// It runs only when the request explicitly opted in, and it only ever reads an upstream
// 429: other statuses produce the stable not_applicable verdict so a caller can tell
// "out of scope" from "opted out". Nothing here re-reads the outbound body, and the
// sanitizer is the single place that decides which header names and value shapes may
// survive, so no raw http.Header ever reaches the observer.
//
// All or nothing over the eligible set: the sanitizer's *eligible* omission summary reports
// how many allowlisted entries it had to drop (a value that failed its shape check, an entry
// over the size budget). Any such omission makes the surviving values a partial view of the
// eligible set, so this observation carries none of them and reports the fact instead
// (DiagnosticHeaderOmitted).
//
// Two kinds of observed headers never trigger that verdict, because both are exclusions the
// capability chose rather than entries it lost:
//   - names outside the closed set: the closed set is the trust boundary, so they are never
//     read into the snapshot, never stored and never named (see the eligible omission scope);
//   - credential headers (Authorization/Cookie/…): presence markers by design.
//
// An ordinary 429 from a real upstream therefore still yields a snapshot (it may carry
// Date/Server/Content-Length and other unlisted headers), while a rejected value on an
// allowlisted header still invalidates the whole snapshot.
func observeUpstreamErrorHeaderValues(observation *DiagnosticObservation, observer *DiagnosticObserver, req *http.Request, resp *http.Response) {
	if observation == nil || observer == nil || !observer.CaptureErrorHeaders || req == nil || resp == nil {
		return
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		observation.HeaderVerdict = DiagnosticHeaderNotApplicable
		return
	}
	requestValues, requestOmission := SanitizeClaudeRequestHeaderValuesWithEligibleOmission(req.Header)
	responseValues, responseOmission := SanitizeClaudeResponseHeaderValuesWithEligibleOmission(resp.Header)
	if requestOmission.Merge(responseOmission).Any() {
		observation.HeaderVerdict = DiagnosticHeaderOmitted
		return
	}
	if len(requestValues) == 0 && len(responseValues) == 0 {
		observation.HeaderVerdict = DiagnosticHeaderEmpty
		return
	}
	observation.HeaderVerdict = DiagnosticHeaderCaptured
	observation.RequestHeaderValues = requestValues
	observation.ResponseHeaderValues = responseValues
}
