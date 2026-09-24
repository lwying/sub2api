package httpattempt

import (
	"io"
	"net/http"
	"sync/atomic"
)

// CountingReadCloser counts bytes returned to the transport without retaining them.
type CountingReadCloser struct {
	io.ReadCloser
	OnRead func(int64)
	// Capture, when set, receives exactly the bytes the transport consumed so an
	// explicitly opted-in diagnostic can keep a bounded copy. It is nil unless the
	// request opted in to body capture.
	Capture *DiagnosticBodyCapture
}

func (r *CountingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 && r.OnRead != nil {
		r.OnRead(int64(n))
	}
	if r.Capture != nil {
		r.Capture.Consume(p[:n], err)
	}
	return n, err
}

func (r *CountingReadCloser) Close() error {
	return r.ReadCloser.Close()
}

// ResponseBody counts bytes consumed by the caller. It never buffers or retains body data.
type ResponseBody struct {
	io.ReadCloser
	attempt  *Attempt
	complete atomic.Bool
}

func NewResponseBody(body io.ReadCloser, attempt *Attempt) io.ReadCloser {
	if body == nil || body == http.NoBody {
		return body
	}
	return &ResponseBody{ReadCloser: body, attempt: attempt}
}

func (r *ResponseBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 && r.attempt != nil {
		r.attempt.AddResponseBytes(int64(n))
	}
	if err == io.EOF && r.attempt != nil {
		r.complete.Store(true)
		r.attempt.SetResponseReadComplete(true)
	}
	return n, err
}

// Close ends the read. Closing before EOF is normally an incomplete read, but a stream
// stopped right after an explicit terminal event is not: SetResponseReadComplete keeps
// the already complete verdict recorded for this attempt (see MarkLastResponseReadComplete).
func (r *ResponseBody) Close() error {
	err := r.ReadCloser.Close()
	if r.attempt != nil && !r.complete.Load() {
		r.complete.Store(true)
		r.attempt.SetResponseReadComplete(false)
	}
	return err
}
