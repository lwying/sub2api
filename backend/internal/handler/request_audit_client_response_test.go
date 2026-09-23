//go:build unit

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// clientResponseAuditWriterStub lets a test present a committed writer whose
// status or size is not the factual pair gin would produce, so the two facts can
// be proven to be validated independently. The embedded interface satisfies the
// rest of gin.ResponseWriter; only the read methods under test are called.
type clientResponseAuditWriterStub struct {
	gin.ResponseWriter
	status  int
	size    int
	written bool
}

func (w clientResponseAuditWriterStub) Status() int   { return w.status }
func (w clientResponseAuditWriterStub) Size() int     { return w.size }
func (w clientResponseAuditWriterStub) Written() bool { return w.written }

// clientResponseAuditSpyWriter records every attempt to mutate or read response
// headers/body so the snapshot can be proven to be a pure read of committed state.
type clientResponseAuditSpyWriter struct {
	gin.ResponseWriter
	status    int
	size      int
	written   bool
	mutations []string
}

func (w *clientResponseAuditSpyWriter) record(name string) { w.mutations = append(w.mutations, name) }

func (w *clientResponseAuditSpyWriter) Status() int   { return w.status }
func (w *clientResponseAuditSpyWriter) Size() int     { return w.size }
func (w *clientResponseAuditSpyWriter) Written() bool { return w.written }

func (w *clientResponseAuditSpyWriter) Header() http.Header {
	w.record("Header")
	return nil
}

func (w *clientResponseAuditSpyWriter) Write(data []byte) (int, error) {
	w.record("Write")
	return len(data), nil
}

func (w *clientResponseAuditSpyWriter) WriteString(s string) (int, error) {
	w.record("WriteString")
	return len(s), nil
}

func (w *clientResponseAuditSpyWriter) WriteHeader(int) { w.record("WriteHeader") }
func (w *clientResponseAuditSpyWriter) WriteHeaderNow() { w.record("WriteHeaderNow") }
func (w *clientResponseAuditSpyWriter) Flush()          { w.record("Flush") }

func TestSnapshotClientResponseAuditIsAPureReadOfCommittedState(t *testing.T) {
	spy := &clientResponseAuditSpyWriter{status: http.StatusOK, size: 5, written: true}
	c := newClientResponseAuditTestContext()
	c.Writer = spy

	metadata := snapshotClientResponseAudit(service.RequestAuditMetadata{}, c)

	require.Empty(t, spy.mutations, "the snapshot must not read headers, write a body, commit or flush a response")
	require.Same(t, spy, c.Writer, "the snapshot must not replace the response writer")
	require.Equal(t, map[string]int{service.RequestAuditClientResponseKey: http.StatusOK}, metadata.Status)
	require.Equal(t, map[string]int64{service.RequestAuditClientResponseKey: 5}, metadata.Bytes)
}

func newClientResponseAuditTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

func TestSnapshotClientResponseAuditOmitsUncommittedWriter(t *testing.T) {
	c := newClientResponseAuditTestContext()
	require.False(t, c.Writer.Written())
	require.Equal(t, http.StatusOK, c.Writer.Status(), "gin reports a default 200 before the response is committed")

	metadata := snapshotClientResponseAudit(service.RequestAuditMetadata{
		Routes: map[string]string{"inbound": "/v1/messages"},
	}, c)

	require.NotContains(t, metadata.Status, service.RequestAuditClientResponseKey,
		"an uncommitted writer must not contribute gin's default 200")
	require.NotContains(t, metadata.Bytes, service.RequestAuditClientResponseKey,
		"an uncommitted writer must not contribute a size")
	require.Empty(t, metadata.Status)
	require.Empty(t, metadata.Bytes)
	require.Equal(t, "/v1/messages", metadata.Routes["inbound"], "an uncommitted response must not disturb other phases")
}

func TestSnapshotClientResponseAuditRecordsCommittedStatusAndBytes(t *testing.T) {
	const body = `{"error":"client-visible-body-canary"}`

	c := newClientResponseAuditTestContext()
	c.String(http.StatusAccepted, body)
	require.True(t, c.Writer.Written())

	metadata := snapshotClientResponseAudit(service.RequestAuditMetadata{}, c)

	require.Equal(t, map[string]int{service.RequestAuditClientResponseKey: http.StatusAccepted}, metadata.Status)
	require.Equal(t, map[string]int64{service.RequestAuditClientResponseKey: int64(len(body))}, metadata.Bytes)

	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	dump := string(encoded)
	require.NotContains(t, dump, "client-visible-body-canary", "response bodies are never recorded")
	require.NotContains(t, dump, "Content-Type", "response headers are never recorded")
}

func TestSnapshotClientResponseAuditPreservesObservedEmptyBody(t *testing.T) {
	c := newClientResponseAuditTestContext()
	c.Writer.WriteHeader(http.StatusNoContent)
	c.Writer.WriteHeaderNow()
	require.True(t, c.Writer.Written())
	require.Equal(t, 0, c.Writer.Size())

	metadata := snapshotClientResponseAudit(service.RequestAuditMetadata{}, c)

	require.Equal(t, map[string]int{service.RequestAuditClientResponseKey: http.StatusNoContent}, metadata.Status)
	require.Equal(t, map[string]int64{service.RequestAuditClientResponseKey: 0}, metadata.Bytes,
		"a committed empty body must not look like missing data")
}

func TestSnapshotClientResponseAuditOmitsInvalidStatusIndependently(t *testing.T) {
	for _, status := range []int{-1, 0, 99, 600, 1000} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			c := newClientResponseAuditTestContext()
			c.Writer = clientResponseAuditWriterStub{status: status, size: 12, written: true}

			metadata := snapshotClientResponseAudit(service.RequestAuditMetadata{}, c)

			require.Empty(t, metadata.Status, "an out-of-range status is not an observed fact")
			require.Equal(t, map[string]int64{service.RequestAuditClientResponseKey: 12}, metadata.Bytes,
				"a valid size survives an invalid status")
		})
	}
}

func TestSnapshotClientResponseAuditOmitsNegativeSizeIndependently(t *testing.T) {
	for _, size := range []int{-1, -100} {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			c := newClientResponseAuditTestContext()
			c.Writer = clientResponseAuditWriterStub{status: http.StatusServiceUnavailable, size: size, written: true}

			metadata := snapshotClientResponseAudit(service.RequestAuditMetadata{}, c)

			require.Equal(t, map[string]int{service.RequestAuditClientResponseKey: http.StatusServiceUnavailable}, metadata.Status)
			require.Empty(t, metadata.Bytes, "a negative byte count is not an observed fact")
		})
	}
}

func TestSnapshotClientResponseAuditDoesNotMutateCallerMetadata(t *testing.T) {
	statuses := map[string]int{service.RequestAuditClientResponseKey: 200}
	bytes := map[string]int64{service.RequestAuditClientResponseKey: 1}
	routes := map[string]string{"inbound": "/v1/messages"}
	in := service.RequestAuditMetadata{Routes: routes, Status: statuses, Bytes: bytes}

	c := newClientResponseAuditTestContext()
	c.Writer.WriteHeader(http.StatusBadGateway)
	c.Writer.WriteHeaderNow()

	out := snapshotClientResponseAudit(in, c)

	require.Equal(t, map[string]int{service.RequestAuditClientResponseKey: 200}, statuses, "the caller's status map is not rewritten")
	require.Equal(t, map[string]int64{service.RequestAuditClientResponseKey: 1}, bytes, "the caller's bytes map is not rewritten")
	require.Equal(t, map[string]int{service.RequestAuditClientResponseKey: http.StatusBadGateway}, out.Status,
		"the snapshot supersedes a stale client response fact")
	require.Equal(t, map[string]int64{service.RequestAuditClientResponseKey: 0}, out.Bytes)
	require.Equal(t, routes, out.Routes)

	out.Status[service.RequestAuditClientResponseKey] = 1
	require.Equal(t, 200, statuses[service.RequestAuditClientResponseKey], "the returned map is not aliased to the caller's")
}

func TestSnapshotClientResponseAuditToleratesMissingWriter(t *testing.T) {
	metadata := service.RequestAuditMetadata{Routes: map[string]string{"inbound": "/v1/messages"}}

	require.Equal(t, metadata, snapshotClientResponseAudit(metadata, nil))
	require.Equal(t, metadata, snapshotClientResponseAudit(metadata, &gin.Context{}))
}
