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

// DiagnosticObservation is one real upstream HTTP 4xx/5xx attempt, observed after the
// RoundTrip that received it. It deliberately carries no credentials, no headers, no
// account identity and no model name: those stay with the caller that bound the
// observer, which is also the only place that knows the covered branch.
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
}

// DiagnosticObserver is an explicit, per-request opt-in to upstream error
// diagnostics. Nothing is observed without it.
type DiagnosticObserver struct {
	// CaptureRequestBody opts in to a bounded copy of the outbound request body. It is
	// false for metadata-only diagnostics, in which case no request bytes are retained.
	CaptureRequestBody bool
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
	observer.OnUpstreamError(observation)
}
