package httpattempt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func newDiagnosticRequest(t *testing.T, ctx context.Context, payload []byte) *http.Request {
	t.Helper()
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/messages", body)
	require.NoError(t, err)
	return req
}

func newDiagnosticResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func drainLikeTransport(t *testing.T, body io.Reader, chunk int) []byte {
	t.Helper()
	var consumed []byte
	buffer := make([]byte, chunk)
	for {
		n, err := body.Read(buffer)
		consumed = append(consumed, buffer[:n]...)
		if errors.Is(err, io.EOF) {
			return consumed
		}
		require.NoError(t, err)
	}
}

func TestObserveUpstreamErrorOnlyForRealUpstreamHTTPFailures(t *testing.T) {
	var observed []int
	observer := &DiagnosticObserver{OnUpstreamError: func(o DiagnosticObservation) {
		observed = append(observed, o.StatusCode)
	}}
	counter := NewCounter()
	ctx := WithCounter(context.Background(), counter)
	ctx = WithDiagnosticObserver(ctx, observer)
	req := newDiagnosticRequest(t, ctx, []byte(`{"input":"canary"}`))
	attempt := StartRequestAttempt(req)
	require.NotNil(t, attempt)

	for _, status := range []int{200, 201, 302, 399} {
		ObserveUpstreamError(req, attempt, newDiagnosticResponse(status, "ok"), nil)
	}
	require.Empty(t, observed, "only real upstream HTTP failures may be observed")

	for _, status := range []int{400, 403, 429, 500, 503, 599} {
		ObserveUpstreamError(req, attempt, newDiagnosticResponse(status, "failed"), nil)
	}
	require.Equal(t, []int{400, 403, 429, 500, 503, 599}, observed)

	ObserveUpstreamError(req, attempt, nil, nil)
	require.Len(t, observed, 6, "a transport failure without an HTTP response is not an upstream HTTP failure")
}

func TestObserveUpstreamErrorWithoutOptInDoesNothing(t *testing.T) {
	payload := []byte(`{"input":"opt-out"}`)

	// No observer at all: nothing is observed and nothing is captured.
	unobserved := newDiagnosticRequest(t, context.Background(), payload)
	require.Nil(t, NewDiagnosticBodyCapture(unobserved))
	ObserveUpstreamError(unobserved, nil, newDiagnosticResponse(500, "boom"), nil)

	// A metadata-only opt-in must not retain bytes and must tolerate a nil callback.
	metadataOnly := newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), &DiagnosticObserver{}), payload)
	require.Nil(t, NewDiagnosticBodyCapture(metadataOnly))
	ObserveUpstreamError(metadataOnly, nil, newDiagnosticResponse(500, "boom"), nil)
}

func TestObservationCarriesPerLogicalRequestAttemptOrdinalAndRequestBytes(t *testing.T) {
	payload := []byte(`{"model":"claude-sonnet-4-5","input":"canary-input"}`)
	counter := NewCounter()
	var observed []DiagnosticObservation
	observer := &DiagnosticObserver{OnUpstreamError: func(o DiagnosticObservation) {
		observed = append(observed, o)
	}}
	ctx := WithCounter(context.Background(), counter)
	ctx = WithDiagnosticObserver(ctx, observer)
	ctx = WithMetadata(ctx, Metadata{AccountID: 7, Model: "claude-sonnet-4-5", Protocol: "anthropic.messages"})

	for range 2 {
		req := newDiagnosticRequest(t, ctx, payload)
		attempt := StartRequestAttempt(req)
		require.NotNil(t, attempt)
		require.Equal(t, payload, drainLikeTransport(t, &CountingReadCloser{ReadCloser: req.Body, OnRead: attempt.AddRequestBytes}, 9))
		ObserveUpstreamError(req, attempt, &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"X-Upstream-Credential": {"upstream-credential-canary"}},
			Body:       io.NopCloser(strings.NewReader("bad gateway")),
		}, nil)
	}

	require.Len(t, observed, 2)
	require.Equal(t, []int{1, 2}, []int{observed[0].AttemptOrdinal, observed[1].AttemptOrdinal},
		"repeated attempts of one logical request must carry distinct ordinals")
	for _, observation := range observed {
		require.Equal(t, http.StatusBadGateway, observation.StatusCode)
		require.False(t, observation.ObservedAt.IsZero())
		require.Equal(t, int64(len(payload)), observation.RequestBytes)
		require.Equal(t, DiagnosticBodyNotRequested, observation.BodyVerdict)
		require.Nil(t, observation.RequestBody, "metadata-only observation must never carry body bytes")
	}
	require.Equal(t, uint64(2), counter.Load())
}

func TestObservationWithoutLogicalRequestCounterIsUntracked(t *testing.T) {
	var observed []DiagnosticObservation
	observer := &DiagnosticObserver{OnUpstreamError: func(o DiagnosticObservation) {
		observed = append(observed, o)
	}}
	req := newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), observer), nil)
	require.Nil(t, StartRequestAttempt(req))

	ObserveUpstreamError(req, nil, newDiagnosticResponse(500, "boom"), nil)
	require.Len(t, observed, 1)
	require.Zero(t, observed[0].AttemptOrdinal, "an untracked attempt has no ordinal to report")
	require.Equal(t, 500, observed[0].StatusCode)
	require.Zero(t, observed[0].RequestBytes)
}

func TestDiagnosticBodyCaptureRequiresExplicitPerRequestOptIn(t *testing.T) {
	payload := []byte(`{"input":"opt-out"}`)
	require.Nil(t, NewDiagnosticBodyCapture(newDiagnosticRequest(t, context.Background(), payload)),
		"no observer means no capture")
	require.Nil(t, NewDiagnosticBodyCapture(newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), &DiagnosticObserver{
		OnUpstreamError: func(DiagnosticObservation) {},
	}), payload)), "an observer without body capture must not retain bytes")
	require.NotNil(t, NewDiagnosticBodyCapture(newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), &DiagnosticObserver{
		CaptureRequestBody: true,
	}), payload)), "an explicit opt-in enables the bounded capture")
}

func TestDiagnosticBodyCaptureOfBodylessAttemptIsCompleteAndEmpty(t *testing.T) {
	var observed []DiagnosticObservation
	observer := &DiagnosticObserver{CaptureRequestBody: true, OnUpstreamError: func(o DiagnosticObservation) {
		observed = append(observed, o)
	}}
	req := newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), observer), nil)
	capture := NewDiagnosticBodyCapture(req)
	require.NotNil(t, capture)

	ObserveUpstreamError(req, nil, newDiagnosticResponse(503, "unavailable"), capture)
	require.Len(t, observed, 1)
	require.Equal(t, DiagnosticBodyComplete, observed[0].BodyVerdict)
	require.Nil(t, observed[0].RequestBody)
	require.Zero(t, observed[0].RequestBytes)
}

func TestDiagnosticBodyCaptureKeepsExactlyTheBytesTheTransportConsumed(t *testing.T) {
	payload := []byte(`{"model":"claude-sonnet-4-5","input":"canary-input"}`)
	var observed []DiagnosticObservation
	observer := &DiagnosticObserver{CaptureRequestBody: true, OnUpstreamError: func(o DiagnosticObservation) {
		observed = append(observed, o)
	}}
	req := newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), observer), payload)
	capture := NewDiagnosticBodyCapture(req)
	require.NotNil(t, capture)
	defer capture.Release()

	consumed := drainLikeTransport(t, &CountingReadCloser{ReadCloser: req.Body, Capture: capture}, 7)
	require.Equal(t, payload, consumed)

	ObserveUpstreamError(req, nil, newDiagnosticResponse(500, "boom"), capture)
	require.Len(t, observed, 1)
	require.Equal(t, DiagnosticBodyComplete, observed[0].BodyVerdict)
	require.Equal(t, int64(len(payload)), observed[0].RequestBytes)
	require.Equal(t, payload, observed[0].RequestBody)
}

func TestDiagnosticBodyCaptureIsBoundedToOneMiBPlusOne(t *testing.T) {
	atLimit := bytes.Repeat([]byte("a"), int(DiagnosticRequestBodyLimit))
	overLimit := append(bytes.Clone(atLimit), 'b')
	farOverLimit := bytes.Repeat([]byte("c"), int(DiagnosticRequestBodyLimit)+4096)

	for _, tt := range []struct {
		name    string
		payload []byte
		verdict DiagnosticBodyVerdict
		kept    bool
	}{
		{name: "exactly at the limit", payload: atLimit, verdict: DiagnosticBodyComplete, kept: true},
		{name: "one byte over the limit", payload: overLimit, verdict: DiagnosticBodyTooLarge},
		{name: "far over the limit", payload: farOverLimit, verdict: DiagnosticBodyTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var observed []DiagnosticObservation
			observer := &DiagnosticObserver{CaptureRequestBody: true, OnUpstreamError: func(o DiagnosticObservation) {
				observed = append(observed, o)
			}}
			req := newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), observer), tt.payload)
			capture := NewDiagnosticBodyCapture(req)
			require.NotNil(t, capture)
			defer capture.Release()

			require.Len(t, drainLikeTransport(t, &CountingReadCloser{ReadCloser: req.Body, Capture: capture}, 4096), len(tt.payload))
			ObserveUpstreamError(req, nil, newDiagnosticResponse(413, "too large"), capture)

			require.Len(t, observed, 1)
			require.Equal(t, tt.verdict, observed[0].BodyVerdict)
			require.Equal(t, int64(len(tt.payload)), observed[0].RequestBytes)
			if tt.kept {
				require.Equal(t, tt.payload, observed[0].RequestBody)
			} else {
				require.Nil(t, observed[0].RequestBody, "an oversized body must never be truncated into a complete diagnostic")
			}
		})
	}
}

func TestDiagnosticBodyCaptureIsIncompleteUntilTheTransportConsumesTheWholeBody(t *testing.T) {
	payload := []byte(`{"input":"partial-canary"}`)
	var observed []DiagnosticObservation
	observer := &DiagnosticObserver{CaptureRequestBody: true, OnUpstreamError: func(o DiagnosticObservation) {
		observed = append(observed, o)
	}}
	req := newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), observer), payload)
	capture := NewDiagnosticBodyCapture(req)
	require.NotNil(t, capture)
	defer capture.Release()

	buffer := make([]byte, 5)
	n, err := (&CountingReadCloser{ReadCloser: req.Body, Capture: capture}).Read(buffer)
	require.NoError(t, err)
	require.Equal(t, 5, n)

	ObserveUpstreamError(req, nil, newDiagnosticResponse(500, "boom"), capture)
	require.Len(t, observed, 1)
	require.Equal(t, DiagnosticBodyIncomplete, observed[0].BodyVerdict)
	require.Equal(t, int64(5), observed[0].RequestBytes)
	require.Nil(t, observed[0].RequestBody, "a partially sent body must never be reported as the complete outbound body")
}

func TestDiagnosticBodyCaptureStaysIncompleteWhenTheSendingReadFails(t *testing.T) {
	var observed []DiagnosticObservation
	observer := &DiagnosticObserver{CaptureRequestBody: true, OnUpstreamError: func(o DiagnosticObservation) {
		observed = append(observed, o)
	}}
	req := newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), observer), []byte(`{"input":"broken-canary"}`))
	req.Body = io.NopCloser(&failingReader{payload: []byte(`{"input":"bro`), err: errors.New("connection reset by peer")})
	capture := NewDiagnosticBodyCapture(req)
	require.NotNil(t, capture)
	defer capture.Release()

	_, err := drainLikeTransportNoError(t, &CountingReadCloser{ReadCloser: req.Body, Capture: capture}, 16)
	require.Error(t, err)

	ObserveUpstreamError(req, nil, newDiagnosticResponse(502, "bad gateway"), capture)
	require.Len(t, observed, 1)
	require.Equal(t, DiagnosticBodyIncomplete, observed[0].BodyVerdict,
		"an outbound body that was never fully consumed is incomplete even when the upstream answered")
	require.Nil(t, observed[0].RequestBody)
}

func TestDiagnosticBodyCaptureNeverUsesGetBody(t *testing.T) {
	payload := []byte(`{"input":"no-getbody-canary"}`)
	var getBodyCalls atomic.Int64
	var observed []DiagnosticObservation
	observer := &DiagnosticObserver{CaptureRequestBody: true, OnUpstreamError: func(o DiagnosticObservation) {
		observed = append(observed, o)
	}}
	req := newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), observer), payload)
	req.GetBody = func() (io.ReadCloser, error) {
		getBodyCalls.Add(1)
		return nil, errors.New("diagnostic capture must not replay the request body")
	}
	capture := NewDiagnosticBodyCapture(req)
	require.NotNil(t, capture)
	defer capture.Release()

	consumed := drainLikeTransport(t, &CountingReadCloser{ReadCloser: req.Body, Capture: capture}, 64)
	ObserveUpstreamError(req, nil, newDiagnosticResponse(500, "boom"), capture)

	require.Zero(t, getBodyCalls.Load())
	require.Equal(t, payload, consumed)
	require.Len(t, observed, 1)
	require.Equal(t, DiagnosticBodyComplete, observed[0].BodyVerdict)
	require.Equal(t, payload, observed[0].RequestBody)
}

func TestDiagnosticBodyCaptureZeroesAndDropsTheBufferOnRelease(t *testing.T) {
	payload := []byte(`{"input":"release-canary"}`)
	var observed DiagnosticObservation
	observer := &DiagnosticObserver{CaptureRequestBody: true, OnUpstreamError: func(o DiagnosticObservation) {
		observed = o
	}}
	req := newDiagnosticRequest(t, WithDiagnosticObserver(context.Background(), observer), payload)
	capture := NewDiagnosticBodyCapture(req)
	require.NotNil(t, capture)

	require.Equal(t, payload, drainLikeTransport(t, &CountingReadCloser{ReadCloser: req.Body, Capture: capture}, 32))
	ObserveUpstreamError(req, nil, newDiagnosticResponse(500, "boom"), capture)
	require.Equal(t, payload, observed.RequestBody)

	capture.Release()
	require.NotEmpty(t, observed.RequestBody)
	for i, b := range observed.RequestBody {
		require.Zero(t, b, "captured plaintext must be zeroed once the observation returns (byte %d)", i)
	}
	capture.Release()
}

type failingReader struct {
	payload []byte
	err     error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.payload) == 0 {
		return 0, r.err
	}
	n := copy(p, r.payload)
	r.payload = r.payload[n:]
	return n, nil
}

func drainLikeTransportNoError(t *testing.T, body io.Reader, chunk int) ([]byte, error) {
	t.Helper()
	var consumed []byte
	buffer := make([]byte, chunk)
	for {
		n, err := body.Read(buffer)
		consumed = append(consumed, buffer[:n]...)
		if err != nil {
			return consumed, err
		}
	}
}
