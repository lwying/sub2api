//go:build unit

package handler

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type hijackTraceResponseWriter struct {
	gin.ResponseWriter
	conn net.Conn
	buf  *bufio.ReadWriter
}

func (w *hijackTraceResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.buf, nil
}

type partialTraceResponseWriter struct {
	gin.ResponseWriter
	writeErr error
}

func (w *partialTraceResponseWriter) Write(b []byte) (int, error) {
	n, _ := w.ResponseWriter.Write(b[:3])
	return n, w.writeErr
}

func (w *partialTraceResponseWriter) WriteString(s string) (int, error) {
	n, _ := w.ResponseWriter.WriteString(s[:2])
	return n, w.writeErr
}

func TestRequestTraceWriterMarksHijackedConnectionAsUnobservable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	underlying := &hijackTraceResponseWriter{ResponseWriter: ctx.Writer, conn: left, buf: bufio.NewReadWriter(bufio.NewReader(left), bufio.NewWriter(left))}
	wrapped := NewRequestTraceWriter(underlying, nil)
	w, ok := wrapped.(*requestTraceWriter)
	require.True(t, ok)
	require.False(t, w.Hijacked())
	conn, _, err := wrapped.Hijack()
	require.NoError(t, err)
	require.Same(t, left, conn)
	require.True(t, w.Hijacked())
}

func TestRequestTraceWriterStreamsSSEWithoutChangingFlush(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	var observed []string
	var statuses []int
	w := NewRequestTraceWriter(ctx.Writer, func(chunk []byte, status int) {
		observed = append(observed, string(chunk))
		statuses = append(statuses, status)
	})
	w.WriteHeader(http.StatusAccepted)
	n, err := w.WriteString("event: message\\ndata: first\\n\\n")
	require.NoError(t, err)
	require.Equal(t, len("event: message\\ndata: first\\n\\n"), n)
	w.Flush()
	require.True(t, recorder.Flushed)
	require.Equal(t, http.StatusAccepted, recorder.Code)
	require.Equal(t, []string{"event: message\\ndata: first\\n\\n"}, observed)
	require.Equal(t, []int{http.StatusAccepted}, statuses)
}

func TestRequestTraceWriterCapturesOnlyActuallyWrittenBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	writeErr := errors.New("synthetic partial write")
	var observed []string
	var statuses []int
	w := NewRequestTraceWriter(&partialTraceResponseWriter{ResponseWriter: ctx.Writer, writeErr: writeErr}, func(chunk []byte, status int) {
		observed = append(observed, string(chunk))
		statuses = append(statuses, status)
	})

	n, err := w.Write([]byte("abcdef"))
	require.Equal(t, 3, n)
	require.ErrorIs(t, err, writeErr)
	n, err = w.WriteString("wxyz")
	require.Equal(t, 2, n)
	require.ErrorIs(t, err, writeErr)
	require.Equal(t, []string{"abc", "wx"}, observed)
	require.Equal(t, []int{http.StatusOK, http.StatusOK}, statuses)
	require.Equal(t, "abcwx", recorder.Body.String())
	require.Equal(t, 5, w.Size())
}
