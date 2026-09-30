//go:build unit

package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type unreadableTraceBody struct{ reads int }

func (b *unreadableTraceBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *unreadableTraceBody) Close() error             { return nil }

func TestRequestTraceMiddlewareCreatesIndependentIDWithoutReadingDeniedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var traces []string
	var bodiesObserved []bool
	r := gin.New()
	r.POST("/v1/messages", RequestTraceMiddleware(func(c *gin.Context) bool { return true }, func(c *gin.Context, id string, hasBody bool) {
		traces = append(traces, id)
		bodiesObserved = append(bodiesObserved, hasBody)
	}), func(c *gin.Context) { c.JSON(http.StatusUnauthorized, gin.H{"error": "rejected"}) })

	for i := 0; i < 2; i++ {
		body := &unreadableTraceBody{}
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("no key"))
		req.Header.Set("X-Request-ID", "reused-client-id")
		req.Body = body
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Zero(t, body.reads)
	}
	require.Len(t, traces, 2)
	require.NotEqual(t, traces[0], traces[1])
	require.True(t, regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(traces[0]))
	require.Equal(t, []bool{false, false}, bodiesObserved)
}
