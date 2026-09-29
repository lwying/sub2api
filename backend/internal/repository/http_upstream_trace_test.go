package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/stretchr/testify/require"
)

type traceWireSink struct {
	mu              sync.Mutex
	request         bytes.Buffer
	response        bytes.Buffer
	requestEvents   []error
	responseEvents  []error
	closedResponses int
	status          int
	resultError     error
}

func (s *traceWireSink) RequestBodyChunk(chunk []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.request.Write(chunk)
	s.requestEvents = append(s.requestEvents, err)
}
func (s *traceWireSink) RequestBodyClosed(error) {}
func (s *traceWireSink) RoundTripResult(status int, _ http.Header, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.resultError = err
}
func (s *traceWireSink) ResponseBodyChunk(chunk []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.response.Write(chunk)
	s.responseEvents = append(s.responseEvents, err)
}
func (s *traceWireSink) ResponseBodyClosed(error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closedResponses++
}

func TestTransportTraceOptInObservesActualRequestAndResponseReads(t *testing.T) {
	var starts []httpattempt.TraceAttemptStart
	var sinks []*traceWireSink
	observer := &httpattempt.TraceObserver{OnAttempt: func(start httpattempt.TraceAttemptStart) httpattempt.TraceAttemptSink {
		starts = append(starts, start)
		sink := &traceWireSink{}
		sinks = append(sinks, sink)
		return sink
	}}
	payload := `{"model":"synthetic","messages":[{"role":"user","content":"sentinel"}]}`
	body := `{"content":[{"type":"text","text":"answer"}]}`
	var observedUpstream string
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		observedUpstream = string(got)
		return fixUpstreamResponse(req, http.StatusOK, body), nil
	})}

	ctx := httpattempt.WithTraceObserver(context.Background(), observer)
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{AccountID: 11, Model: "synthetic-model", Protocol: "anthropic.messages"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader(payload))
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, body, string(got))
	require.Equal(t, payload, observedUpstream)
	require.Len(t, starts, 1)
	require.Equal(t, http.MethodPost, starts[0].Method)
	require.Equal(t, "upstream.example", starts[0].URL.Host)
	require.Equal(t, int64(11), starts[0].AccountID)
	require.Equal(t, "synthetic-model", starts[0].Model)
	require.Equal(t, "anthropic.messages", starts[0].Protocol)
	require.Len(t, sinks, 1)
	require.Equal(t, payload, sinks[0].request.String())
	require.Equal(t, body, sinks[0].response.String())
	require.Equal(t, http.StatusOK, sinks[0].status)
	require.Equal(t, 1, sinks[0].closedResponses)
	require.ErrorIs(t, sinks[0].responseEvents[len(sinks[0].responseEvents)-1], io.EOF)
}

func TestTransportTraceDoesNotObserveWithoutOptIn(t *testing.T) {
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		return fixUpstreamResponse(req, http.StatusOK, "hi"), nil
	})}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader("request"))
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	_, ok := httpattempt.TraceObserverFromContext(req.Context())
	require.False(t, ok)
}

func TestTransportTraceReportsFailedRoundTripWithoutFabricatingResponse(t *testing.T) {
	var sink *traceWireSink
	observer := &httpattempt.TraceObserver{OnAttempt: func(_ httpattempt.TraceAttemptStart) httpattempt.TraceAttemptSink {
		sink = &traceWireSink{}
		return sink
	}}
	failure := errors.New("synthetic network failure")
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		return nil, failure
	})}
	ctx := httpattempt.WithTraceObserver(context.Background(), observer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader("request"))
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.Nil(t, resp)
	require.ErrorIs(t, err, failure)
	require.NotNil(t, sink)
	require.Equal(t, 0, sink.status)
	require.ErrorIs(t, sink.resultError, failure)
	require.Empty(t, sink.response.String())
}

func TestTransportTraceCountsGrokFallbackAsTwoRealAttempts(t *testing.T) {
	var starts []httpattempt.TraceAttemptStart
	var sinks []*traceWireSink
	observer := &httpattempt.TraceObserver{OnAttempt: func(start httpattempt.TraceAttemptStart) httpattempt.TraceAttemptSink {
		starts = append(starts, start)
		sink := &traceWireSink{}
		sinks = append(sinks, sink)
		return sink
	}}
	payload := `{"model":"grok-4.5","input":"trace-fallback"}`
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, payload, string(body))
		switch req.URL.Hostname() {
		case grokCLIProxyHost:
			return fixUpstreamResponse(req, http.StatusForbidden, `{"error":"Access denied"}`), nil
		case grokOfficialAPIHost:
			return fixUpstreamResponse(req, http.StatusTooManyRequests, `{"error":"try again"}`), nil
		}
		t.Fatalf("unexpected upstream host %q", req.URL.Hostname())
		return nil, nil
	})}
	ctx := httpattempt.WithTraceObserver(context.Background(), observer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("Authorization", "Bearer synthetic-test-token")
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, starts, 2)
	require.Equal(t, []int{1, 2}, []int{starts[0].Ordinal, starts[1].Ordinal})
	require.Equal(t, grokCLIProxyHost, starts[0].URL.Hostname())
	require.Equal(t, grokOfficialAPIHost, starts[1].URL.Hostname())
	require.Len(t, sinks, 2)
	require.Equal(t, payload, sinks[0].request.String())
	require.Equal(t, payload, sinks[1].request.String())
	require.Equal(t, http.StatusForbidden, sinks[0].status)
	require.Equal(t, http.StatusTooManyRequests, sinks[1].status)
	require.Contains(t, sinks[0].response.String(), "Access denied")
	require.Empty(t, sinks[1].response.String(), "unread upstream responses must not be fabricated as observed")
}

func TestTransportTraceMatchesAuditedAttemptOrdinalWhenCounterPresent(t *testing.T) {
	counter := httpattempt.NewCounter()
	var ordinals []int
	observer := &httpattempt.TraceObserver{OnAttempt: func(start httpattempt.TraceAttemptStart) httpattempt.TraceAttemptSink {
		ordinals = append(ordinals, start.Ordinal)
		return &traceWireSink{}
	}}
	ctx := httpattempt.WithCounter(httpattempt.WithTraceObserver(context.Background(), observer), counter)
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{AccountID: 4, Protocol: "anthropic.messages"})
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		return fixUpstreamResponse(req, http.StatusOK, "ok"), nil
	})}
	for i := 0; i < 2; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader("request"))
		require.NoError(t, err)
		resp, err := transport.RoundTrip(req)
		require.NoError(t, err)
		_, err = io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	require.Equal(t, []int{1, 2}, ordinals)
	require.Len(t, counter.Metadata(), 2)
}

func TestTransportTraceEarlyCloseDoesNotInventResponseEOF(t *testing.T) {
	var sink *traceWireSink
	observer := &httpattempt.TraceObserver{OnAttempt: func(httpattempt.TraceAttemptStart) httpattempt.TraceAttemptSink {
		sink = &traceWireSink{}
		return sink
	}}
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		return fixUpstreamResponse(req, http.StatusOK, "upstream-partial-data"), nil
	})}
	ctx := httpattempt.WithTraceObserver(context.Background(), observer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader("request"))
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	buf := make([]byte, 3)
	n, err := resp.Body.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "ups", sink.response.String())
	require.Equal(t, 1, sink.closedResponses)
	for _, observedErr := range sink.responseEvents {
		require.NotErrorIs(t, observedErr, io.EOF)
	}
}

func TestTransportTracePanicDoesNotChangeUpstreamResponse(t *testing.T) {
	observer := &httpattempt.TraceObserver{OnAttempt: func(httpattempt.TraceAttemptStart) httpattempt.TraceAttemptSink {
		return tracePanickingSink{}
	}}
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		return fixUpstreamResponse(req, http.StatusOK, "response"), nil
	})}
	ctx := httpattempt.WithTraceObserver(context.Background(), observer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader("request"))
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "response", string(body))
	require.NoError(t, resp.Body.Close())
}

type tracePanickingSink struct{}

func (tracePanickingSink) RequestBodyChunk([]byte, error) { panic("observer request callback") }
func (tracePanickingSink) RequestBodyClosed(error)        { panic("observer request close") }
func (tracePanickingSink) RoundTripResult(int, http.Header, error) {
	panic("observer response metadata")
}
func (tracePanickingSink) ResponseBodyChunk([]byte, error) { panic("observer response callback") }
func (tracePanickingSink) ResponseBodyClosed(error)        { panic("observer response close") }

func TestTransportTraceForcedPreSendFailureCreatesNoAttempt(t *testing.T) {
	var starts int
	observer := &httpattempt.TraceObserver{OnAttempt: func(httpattempt.TraceAttemptStart) httpattempt.TraceAttemptSink {
		starts++
		return &traceWireSink{}
	}}
	counter := httpattempt.NewCounter()
	gateFailure := errors.New("audit reservation failed")
	counter.SetBeforeAttempt(func(context.Context, httpattempt.Metadata) error { return gateFailure }, true)
	ctx := httpattempt.WithTraceObserver(httpattempt.WithCounter(context.Background(), counter), observer)
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{AccountID: 2, Protocol: "anthropic.messages"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader("body"))
	require.NoError(t, err)
	var sends int
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sends++
		return nil, nil
	})}
	resp, err := transport.RoundTrip(req)
	require.Nil(t, resp)
	require.True(t, httpattempt.IsRequiredAuditError(err))
	require.Zero(t, sends)
	require.Zero(t, starts)
}
