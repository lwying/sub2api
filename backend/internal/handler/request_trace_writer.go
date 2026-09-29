package handler

import (
	"bufio"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// requestTraceWriter observes bytes accepted by the downstream writer. It does
// not buffer bodies: the optional observer is responsible for bounding and
// sanitizing any snapshot it retains. A nil observer leaves writes unchanged.
type requestTraceWriter struct {
	gin.ResponseWriter
	observer func([]byte, int)
	hijacked atomic.Bool
}

func NewRequestTraceWriter(original gin.ResponseWriter, observer func([]byte, int)) gin.ResponseWriter {
	if original == nil {
		return nil
	}
	return &requestTraceWriter{ResponseWriter: original, observer: observer}
}

func (w *requestTraceWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if n > 0 && w.observer != nil {
		observed := min(n, len(b))
		w.notify(b[:observed])
	}
	return n, err
}

func (w *requestTraceWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	if n > 0 && w.observer != nil {
		observed := min(n, len(s))
		w.notify([]byte(s[:observed]))
	}
	return n, err
}

func (w *requestTraceWriter) notify(b []byte) {
	defer func() { _ = recover() }()
	w.observer(b, w.ResponseWriter.Status())
}

func (w *requestTraceWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.Hijack()
	if err == nil {
		w.hijacked.Store(true)
	}
	return conn, rw, err
}

// Hijacked means direct connection writes are no longer observable as HTTP
// response body writes through this wrapper.
func (w *requestTraceWriter) Hijacked() bool { return w.hijacked.Load() }

var _ gin.ResponseWriter = (*requestTraceWriter)(nil)
var _ http.Flusher = (*requestTraceWriter)(nil)
