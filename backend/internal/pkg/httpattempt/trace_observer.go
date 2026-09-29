package httpattempt

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
)

// TraceObserver is an opt-in for observing actual HTTP transport attempts.
// Callbacks run on the request path and must be bounded, nonblocking and copy
// bytes they retain. Neither the transport nor this observer persists values.
// The caller must sanitize any headers, URL or body before storage.
type TraceObserver struct {
	OnAttempt func(TraceAttemptStart) TraceAttemptSink
	n         atomic.Uint64
}

// TraceAttemptStart describes one actual RoundTrip after the pre-send audit gate.
// URL and headers may contain secrets. They are copied only for opted-in requests.
type TraceAttemptStart struct {
	Ordinal       int
	Method        string
	URL           *url.URL
	Header        http.Header
	AccountID     int64
	Model         string
	Protocol      string
	ValueProtocol string
}

// TraceAttemptSink receives bytes as the transport consumes/supplies them.
// A callback's bytes are valid only during that callback. RequestBodyClosed and
// ResponseBodyClosed report real Close calls; only a read's io.EOF proves EOF.
// RoundTripResult reports status and response headers when present, or status
// zero and a transport error when there is no upstream HTTP response.
type TraceAttemptSink interface {
	RequestBodyChunk([]byte, error)
	RequestBodyClosed(error)
	RoundTripResult(int, http.Header, error)
	ResponseBodyChunk([]byte, error)
	ResponseBodyClosed(error)
}

type traceObserverContextKey struct{}

func WithTraceObserver(ctx context.Context, observer *TraceObserver) context.Context {
	if observer == nil || observer.OnAttempt == nil {
		if ctx == nil {
			return context.Background()
		}
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, traceObserverContextKey{}, observer)
}

func TraceObserverFromContext(ctx context.Context) (*TraceObserver, bool) {
	if ctx == nil {
		return nil, false
	}
	observer, ok := ctx.Value(traceObserverContextKey{}).(*TraceObserver)
	return observer, ok && observer != nil && observer.OnAttempt != nil
}

// StartTraceAttempt never runs for a send rejected by the pre-send audit gate.
func StartTraceAttempt(req *http.Request) TraceAttemptSink {
	if req == nil {
		return nil
	}
	observer, ok := TraceObserverFromContext(req.Context())
	if !ok {
		return nil
	}
	metadata, _ := MetadataFromContext(req.Context())
	start := TraceAttemptStart{
		Ordinal: int(observer.n.Add(1)), Method: req.Method, Header: req.Header.Clone(),
		AccountID: metadata.AccountID, Model: metadata.Model,
		Protocol: metadata.Protocol, ValueProtocol: metadata.ValueProtocol,
	}
	if req.URL != nil {
		copyURL := *req.URL
		start.URL = &copyURL
	}
	return safeTraceStart(observer, start)
}

func safeTraceStart(observer *TraceObserver, start TraceAttemptStart) (sink TraceAttemptSink) {
	defer func() {
		if recover() != nil {
			sink = nil
		}
	}()
	return observer.OnAttempt(start)
}

// TraceRequestBody observes the bytes that the actual transport reads, without
// pre-reading or replaying the body. No wrapper is allocated while the gate is off.
func TraceRequestBody(body io.ReadCloser, sink TraceAttemptSink) io.ReadCloser {
	if sink == nil || body == nil || body == http.NoBody {
		return body
	}
	return &traceRequestBody{ReadCloser: body, sink: sink}
}

type traceRequestBody struct {
	io.ReadCloser
	sink TraceAttemptSink
	once sync.Once
}

func (r *traceRequestBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 || err != nil {
		safeTraceNotify(func() { r.sink.RequestBodyChunk(p[:n], err) })
	}
	return n, err
}

func (r *traceRequestBody) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() { safeTraceNotify(func() { r.sink.RequestBodyClosed(err) }) })
	return err
}

// TraceResponseBody observes bytes actually read by the caller. Closing early is
// not reported as EOF, including when an upstream stream is cancelled.
func TraceResponseBody(body io.ReadCloser, sink TraceAttemptSink) io.ReadCloser {
	if sink == nil || body == nil || body == http.NoBody {
		return body
	}
	return &traceResponseBody{ReadCloser: body, sink: sink}
}

type traceResponseBody struct {
	io.ReadCloser
	sink TraceAttemptSink
	once sync.Once
}

func (r *traceResponseBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 || err != nil {
		safeTraceNotify(func() { r.sink.ResponseBodyChunk(p[:n], err) })
	}
	return n, err
}

func (r *traceResponseBody) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() { safeTraceNotify(func() { r.sink.ResponseBodyClosed(err) }) })
	return err
}

func NotifyTraceRoundTrip(sink TraceAttemptSink, resp *http.Response, err error) {
	if sink == nil {
		return
	}
	status := 0
	var headers http.Header
	if resp != nil {
		status = resp.StatusCode
		headers = resp.Header.Clone()
	}
	safeTraceNotify(func() { sink.RoundTripResult(status, headers, err) })
}

func safeTraceNotify(fn func()) {
	defer func() { _ = recover() }()
	fn()
}
