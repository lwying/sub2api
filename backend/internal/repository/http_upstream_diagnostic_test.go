package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// diagnosticRecorder keeps what an observer saw. The seam zeroes the captured buffer
// as soon as the callback returns, so the callback-copied bytes are the only durable
// record of it; the observations themselves stay for the metadata-only facts.
type diagnosticRecorder struct {
	observations []httpattempt.DiagnosticObservation
	bodies       [][]byte
	callbackCopy []string
}

func newDiagnosticRecorder() (*httpattempt.DiagnosticObserver, *diagnosticRecorder) {
	recorder := &diagnosticRecorder{}
	observer := &httpattempt.DiagnosticObserver{
		CaptureRequestBody: true,
		OnUpstreamError: func(o httpattempt.DiagnosticObservation) {
			recorder.observations = append(recorder.observations, o)
			recorder.bodies = append(recorder.bodies, bytes.Clone(o.RequestBody))
			recorder.callbackCopy = append(recorder.callbackCopy, string(o.RequestBody))
		},
	}
	return observer, recorder
}

// diagnosticUpstreamRequest mirrors the forwarder: one outbound request for one real
// upstream attempt, bound to the shared per-logical-request attempt counter and to the
// explicitly opted-in diagnostic observer. A nil observer means no diagnostic opt-in.
func diagnosticUpstreamRequest(
	t *testing.T,
	counter *httpattempt.Counter,
	observer *httpattempt.DiagnosticObserver,
	target string,
	payload string,
) *http.Request {
	t.Helper()
	ctx := httpattempt.WithCounter(t.Context(), counter)
	ctx = httpattempt.WithDiagnosticObserver(ctx, observer)
	ctx = httpattempt.WithMetadata(ctx, httpattempt.Metadata{
		AccountID: 11, Model: "claude-sonnet-4-5", Protocol: "anthropic.messages",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer oauth-token")
	return req
}

// closeRecordingBody counts how often a response body was closed, so a test can assert
// that the body behind an observed upstream error is released instead of leaked.
type closeRecordingBody struct {
	io.ReadCloser
	closes atomic.Int64
}

func (b *closeRecordingBody) Close() error {
	b.closes.Add(1)
	return b.ReadCloser.Close()
}

func fixUpstreamResponse(req *http.Request, status int, body string) *http.Response {
	response := &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       http.NoBody,
		Request:    req,
	}
	if body != "" {
		response.Body = io.NopCloser(strings.NewReader(body))
	}
	return response
}

func TestTransportObservesEachRealUpstreamFailurePerLogicalRequest(t *testing.T) {
	counter := httpattempt.NewCounter()
	observer, recorder := newDiagnosticRecorder()

	statuses := []int{http.StatusInternalServerError, http.StatusOK}
	var calls atomic.Int64
	var received [][]byte
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		index := int(calls.Add(1)) - 1
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		received = append(received, body)
		if statuses[index] == http.StatusOK {
			return fixUpstreamResponse(req, http.StatusOK, ""), nil
		}
		return fixUpstreamResponse(req, statuses[index], `{"error":{"message":"upstream failed"}}`), nil
	})}

	payload := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"diagnostic-canary-input"}]}`
	failed := diagnosticUpstreamRequest(t, counter, observer, "https://upstream.example/v1/messages", payload)
	resp, _, err := transport.roundTripAttempt(failed)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, recorder.observations, 1)
	require.Equal(t, 1, recorder.observations[0].AttemptOrdinal)
	require.Equal(t, http.StatusInternalServerError, recorder.observations[0].StatusCode)
	require.Equal(t, httpattempt.DiagnosticBodyComplete, recorder.observations[0].BodyVerdict)
	require.Equal(t, int64(len(payload)), recorder.observations[0].RequestBytes)
	require.Equal(t, payload, recorder.callbackCopy[0])
	require.Equal(t, payload, string(received[0]), "the diagnostic must carry the bytes the upstream actually received")

	// A later successful retry of the same logical request neither erases nor
	// duplicates the earlier failure, and its own success is not observed.
	retried := diagnosticUpstreamRequest(t, counter, observer, "https://upstream.example/v1/messages", payload)
	resp, _, err = transport.roundTripAttempt(retried)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, recorder.observations, 1)
	require.Len(t, received, 2)
	require.Len(t, counter.Metadata(), 2)
	require.Equal(t, uint64(2), counter.Load())
	require.Equal(t, int64(len(payload)), *counter.Metadata()[1].RequestBytes)
}

func TestTransportObservesDistinctGrokFallbackAttemptsExactlyOnce(t *testing.T) {
	counter := httpattempt.NewCounter()
	observer, recorder := newDiagnosticRecorder()
	payload := `{"model":"grok-4.5","input":"grok-fallback-canary"}`

	var sends atomic.Int64
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, payload, string(body), "both real sends carry the same outbound JSON")
		switch req.URL.Hostname() {
		case grokCLIProxyHost:
			return fixUpstreamResponse(req, http.StatusForbidden, `{"error":"Access denied"}`), nil
		case grokOfficialAPIHost:
			return fixUpstreamResponse(req, http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`), nil
		}
		t.Fatalf("unexpected upstream host %q", req.URL.Hostname())
		return nil, nil
	})}

	req := diagnosticUpstreamRequest(t, counter, observer, "https://cli-chat-proxy.grok.com/v1/responses", payload)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, int64(2), sends.Load())
	require.Len(t, recorder.observations, 2, "each real send is observed exactly once, with no duplicate callback")
	require.Equal(t, []int{1, 2}, []int{
		recorder.observations[0].AttemptOrdinal, recorder.observations[1].AttemptOrdinal,
	}, "the CLI send and the api.x.ai fallback send are distinct attempts")
	require.Equal(t, []int{http.StatusForbidden, http.StatusTooManyRequests}, []int{
		recorder.observations[0].StatusCode, recorder.observations[1].StatusCode,
	})
	for i := range recorder.observations {
		require.Equal(t, httpattempt.DiagnosticBodyComplete, recorder.observations[i].BodyVerdict, "attempt %d", i)
		require.Equal(t, payload, recorder.callbackCopy[i], "attempt %d", i)
	}
}

// TestTransportGrokFallbackBlockedByForcedAuditKeepsOneObservedCLI403 covers the
// highest-risk Grok CLI shape end to end at the transport's public entry point: the CLI
// proxy really answers 403 (observed exactly once), then forced audit mode rejects the
// official-API fallback before it is sent. The caller must receive no response at all —
// replaying the 403 would silently defeat the pre-send block — the blocked send must add
// no observation, and the observed 403 body must be closed rather than leaked.
func TestTransportGrokFallbackBlockedByForcedAuditKeepsOneObservedCLI403(t *testing.T) {
	observer, recorder := newDiagnosticRecorder()
	payload := `{"model":"grok-4.5","input":"blocked-fallback-canary"}`

	cli403Body := &closeRecordingBody{ReadCloser: io.NopCloser(strings.NewReader(`{"error":"Access denied"}`))}
	var sends atomic.Int64
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		require.Equal(t, grokCLIProxyHost, req.URL.Hostname(), "the rejected fallback must never reach api.x.ai")
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, payload, string(body))
		response := fixUpstreamResponse(req, http.StatusForbidden, "")
		response.Body = cli403Body
		return response, nil
	})}

	gateCause := errors.New("audit store down")
	var gateCalls atomic.Int64
	counter := httpattempt.NewCounter()
	counter.SetBeforeAttempt(func(context.Context, httpattempt.Metadata) error {
		if gateCalls.Add(1) == 1 {
			return nil
		}
		return gateCause
	}, true)

	req := diagnosticUpstreamRequest(t, counter, observer, "https://cli-chat-proxy.grok.com/v1/responses", payload)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")

	resp, err := transport.RoundTrip(req)
	require.Nil(t, resp, "the observed CLI 403 must not be handed back over a pre-send block")
	require.True(t, httpattempt.IsRequiredAuditError(err))
	require.ErrorIs(t, err, gateCause)

	require.Equal(t, int64(1), sends.Load(), "only the CLI proxy send reaches the network")
	require.Equal(t, int64(2), gateCalls.Load(), "the gate runs for the CLI send and the rejected fallback")
	require.Equal(t, uint64(1), counter.Load(), "a pre-send block starts no usage-owned attempt")
	require.Len(t, counter.Metadata(), 1)

	require.Len(t, recorder.observations, 1, "the blocked fallback is not an upstream HTTP failure")
	require.Equal(t, 1, recorder.observations[0].AttemptOrdinal)
	require.Equal(t, http.StatusForbidden, recorder.observations[0].StatusCode)
	require.Equal(t, httpattempt.DiagnosticBodyComplete, recorder.observations[0].BodyVerdict)
	require.Equal(t, int64(len(payload)), recorder.observations[0].RequestBytes)
	require.Equal(t, payload, recorder.callbackCopy[0])

	require.Equal(t, int64(1), cli403Body.closes.Load(), "the observed 403 body is closed exactly once")
}

func TestTransportObservesNothingForPreSendBlockOrTransportFailure(t *testing.T) {
	t.Run("forced audit gate blocks before sending", func(t *testing.T) {
		observer, recorder := newDiagnosticRecorder()
		counter := httpattempt.NewCounter()
		counter.SetBeforeAttempt(func(context.Context, httpattempt.Metadata) error {
			return errors.New("audit store down")
		}, true)

		var sends atomic.Int64
		transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			sends.Add(1)
			return fixUpstreamResponse(req, http.StatusBadGateway, ""), nil
		})}

		req := diagnosticUpstreamRequest(t, counter, observer, "https://upstream.example/v1/messages", `{"input":"blocked-canary"}`)
		_, err := transport.RoundTrip(req)
		require.True(t, httpattempt.IsRequiredAuditError(err))
		require.Zero(t, sends.Load())
		require.Zero(t, counter.Load())
		require.Empty(t, recorder.observations, "nothing was sent, so there is no upstream HTTP failure to observe")
	})

	t.Run("transport failure without a response", func(t *testing.T) {
		observer, recorder := newDiagnosticRecorder()
		counter := httpattempt.NewCounter()
		dialErr := errors.New("dial tcp 203.0.113.7:443: connect: connection refused")
		transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, dialErr
		})}

		req := diagnosticUpstreamRequest(t, counter, observer, "https://upstream.example/v1/messages", `{"input":"dial-canary"}`)
		_, _, err := transport.roundTripAttempt(req)
		require.ErrorIs(t, err, dialErr)
		require.Empty(t, recorder.observations, "a connection failure is not an upstream HTTP 4xx/5xx")
		require.Len(t, counter.Metadata(), 1)
		require.Zero(t, *counter.Metadata()[0].RequestBytes,
			"a send that never happened consumes no outbound bytes")
	})
}

func TestTransportObservationIsExplicitOptInAndIndependentOfURL(t *testing.T) {
	payload := `{"input":"url-agnostic-canary"}`
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		return fixUpstreamResponse(req, http.StatusServiceUnavailable, `{"error":"unavailable"}`), nil
	})}

	optOutCounter := httpattempt.NewCounter()
	noOptOut := diagnosticUpstreamRequest(t, optOutCounter, nil, "https://upstream.example/v1/messages", payload)
	require.Nil(t, httpattempt.NewDiagnosticBodyCapture(noOptOut), "without an observer no diagnostic capture exists")
	resp, _, err := transport.roundTripAttempt(noOptOut)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Len(t, optOutCounter.Metadata(), 1, "the attempt is still counted for the logical request")
	require.Equal(t, int64(len(payload)), *optOutCounter.Metadata()[0].RequestBytes,
		"the logical-request attempt count is unaffected by the diagnostic opt-in")

	observer, recorder := newDiagnosticRecorder()
	optedInCounter := httpattempt.NewCounter()
	for _, target := range []string{
		"https://upstream.example/v1/messages",
		"https://api.anthropic.com/v1/messages",
		"https://generativelanguage.googleapis.com/v1beta/models/gemini:generateContent",
		"https://example.invalid/not-a-known-route",
	} {
		req := diagnosticUpstreamRequest(t, optedInCounter, observer, target, payload)
		resp, _, err := transport.roundTripAttempt(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	require.Len(t, recorder.observations, 4, "eligibility is bound by the caller, never inferred from the request URL")
	require.Equal(t, []int{1, 2, 3, 4}, []int{
		recorder.observations[0].AttemptOrdinal, recorder.observations[1].AttemptOrdinal,
		recorder.observations[2].AttemptOrdinal, recorder.observations[3].AttemptOrdinal,
	})
}

// injectCachedUpstreamClient pre-populates the service client cache the same way the
// service would, so the public Do/DoWithTLS entry points can be exercised without a
// socket. The cache key must match what the service computes for the given request.
func injectCachedUpstreamClient(
	t *testing.T,
	svc *httpUpstreamService,
	accountID int64,
	roundTripper http.RoundTripper,
	tlsFingerprint bool,
) {
	t.Helper()
	isolation := svc.getIsolationMode()
	profile := service.HTTPUpstreamProfileDefault
	proxyKey := directProxyKey
	protocolMode := svc.resolveProtocolMode(profile, proxyKey, nil)
	settings := svc.applyProfilePoolSettings(svc.resolvePoolSettings(isolation, 1), profile)

	cacheKey := buildCacheKey(isolation, proxyKey, accountID, protocolMode)
	poolKey := buildPoolKey(settings, protocolMode)
	if tlsFingerprint {
		cacheKey = "tls:" + buildCacheKey(isolation, proxyKey, accountID, upstreamProtocolModeDefault)
		poolKey += ":tls"
	}
	svc.clients[cacheKey] = &upstreamClientEntry{
		client:       &http.Client{Transport: roundTripper},
		proxyKey:     proxyKey,
		poolKey:      poolKey,
		protocolMode: protocolMode,
		inFlight:     1,
	}
}

// newTestHTTPUpstreamService returns the concrete service behind NewHTTPUpstream for the
// tests that need to inject a cached client. The comma-ok assertion keeps the shape check
// explicit instead of panicking if the constructor's return type ever changes.
func newTestHTTPUpstreamService(t *testing.T) *httpUpstreamService {
	t.Helper()
	svc, ok := NewHTTPUpstream(nil).(*httpUpstreamService)
	require.True(t, ok, "NewHTTPUpstream must return *httpUpstreamService")
	return svc
}

func TestHTTPUpstreamDoObservesOrdinaryUpstreamFailuresThroughTheCommonSeam(t *testing.T) {
	payload := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"ordinary-canary"}]}`

	ordinaryUpstream := func(t *testing.T, status int, observedBodies *[][]byte) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			*observedBodies = append(*observedBodies, body)
			// An ordinary Anthropic/OpenAI-shaped send: no Grok identity, no CLI proxy host.
			require.NotEqual(t, grokCLIProxyHost, req.URL.Hostname())
			return fixUpstreamResponse(req, status, `{"error":{"message":"upstream rejected"}}`), nil
		})
	}

	t.Run("Do", func(t *testing.T) {
		svc := newTestHTTPUpstreamService(t)
		const accountID int64 = 7311
		var received [][]byte
		injectCachedUpstreamClient(t, svc, accountID, ordinaryUpstream(t, http.StatusBadRequest, &received), false)

		observer, recorder := newDiagnosticRecorder()
		counter := httpattempt.NewCounter()
		req := diagnosticUpstreamRequest(t, counter, observer, "https://api.anthropic.com/v1/messages", payload)

		resp, err := svc.Do(req, "", accountID, 1)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Len(t, recorder.observations, 1, "an ordinary upstream send must reach the same diagnostic seam")
		require.Equal(t, 1, recorder.observations[0].AttemptOrdinal)
		require.Equal(t, http.StatusBadRequest, recorder.observations[0].StatusCode)
		require.Equal(t, payload, recorder.callbackCopy[0])
		require.Equal(t, payload, string(received[0]))
		require.Equal(t, uint64(1), counter.Load())
	})

	t.Run("DoWithTLS", func(t *testing.T) {
		svc := newTestHTTPUpstreamService(t)
		const accountID int64 = 7312
		var received [][]byte
		injectCachedUpstreamClient(t, svc, accountID, ordinaryUpstream(t, http.StatusInternalServerError, &received), true)

		observer, recorder := newDiagnosticRecorder()
		counter := httpattempt.NewCounter()
		req := diagnosticUpstreamRequest(t, counter, observer, "https://api.openai.com/v1/responses", payload)

		resp, err := svc.DoWithTLS(req, "", accountID, 1, &tlsfingerprint.Profile{Name: "diagnostic-test"})
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Len(t, recorder.observations, 1, "the TLS-fingerprint entry point must reach the same diagnostic seam")
		require.Equal(t, 1, recorder.observations[0].AttemptOrdinal)
		require.Equal(t, http.StatusInternalServerError, recorder.observations[0].StatusCode)
		require.Equal(t, payload, recorder.callbackCopy[0])
		require.Equal(t, payload, string(received[0]))
	})

	t.Run("no opt-in keeps ordinary sends silent", func(t *testing.T) {
		svc := newTestHTTPUpstreamService(t)
		const accountID int64 = 7313
		var received [][]byte
		injectCachedUpstreamClient(t, svc, accountID, ordinaryUpstream(t, http.StatusServiceUnavailable, &received), false)

		counter := httpattempt.NewCounter()
		req := diagnosticUpstreamRequest(t, counter, nil, "https://api.anthropic.com/v1/messages", payload)

		resp, err := svc.Do(req, "", accountID, 1)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, payload, string(received[0]))
		require.Equal(t, uint64(1), counter.Load(), "the usage-owned attempt count is unchanged")
	})
}

// TestHTTPUpstreamDoReportsNonRetainableBodiesAsReasonCodes covers the ticket 02 reason
// paths end to end: a body over the retention limit and a body the transport never
// consumed to EOF both reach the observer as a verdict with no bytes, so the diagnostic
// service can store a stable skip reason instead of a truncated body.
func TestHTTPUpstreamDoReportsNonRetainableBodiesAsReasonCodes(t *testing.T) {
	t.Run("over the retention limit", func(t *testing.T) {
		payload := `{"input":"` + strings.Repeat("a", int(httpattempt.DiagnosticRequestBodyLimit)) + `"}`

		var received []byte
		transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			received = body
			return fixUpstreamResponse(req, http.StatusRequestEntityTooLarge, `{"error":"payload too large"}`), nil
		})}

		observer, recorder := newDiagnosticRecorder()
		counter := httpattempt.NewCounter()
		req := diagnosticUpstreamRequest(t, counter, observer, "https://api.anthropic.com/v1/messages", payload)

		resp, _, err := transport.roundTripAttempt(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, payload, string(received), "an oversized body is still sent in full, never truncated")
		require.Len(t, recorder.observations, 1)
		require.Equal(t, httpattempt.DiagnosticBodyTooLarge, recorder.observations[0].BodyVerdict)
		require.Equal(t, "too_large", recorder.observations[0].BodyVerdict.String())
		require.Nil(t, recorder.observations[0].RequestBody)
		require.Equal(t, int64(len(payload)), recorder.observations[0].RequestBytes)
	})

	t.Run("outbound body never consumed to EOF", func(t *testing.T) {
		payload := `{"model":"claude-sonnet-4-5","input":"partial-send-canary"}`

		transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			// The upstream answers early; the transport never drains the outbound body.
			_, err := io.ReadFull(req.Body, make([]byte, 8))
			require.NoError(t, err)
			return fixUpstreamResponse(req, http.StatusBadRequest, `{"error":"bad request"}`), nil
		})}

		observer, recorder := newDiagnosticRecorder()
		counter := httpattempt.NewCounter()
		req := diagnosticUpstreamRequest(t, counter, observer, "https://api.anthropic.com/v1/messages", payload)

		resp, _, err := transport.roundTripAttempt(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Len(t, recorder.observations, 1)
		require.Equal(t, http.StatusBadRequest, recorder.observations[0].StatusCode)
		require.Equal(t, httpattempt.DiagnosticBodyIncomplete, recorder.observations[0].BodyVerdict)
		require.Equal(t, "incomplete", recorder.observations[0].BodyVerdict.String())
		require.Nil(t, recorder.observations[0].RequestBody,
			"a partially sent body must reach the diagnostic as a reason, never as bytes")
		require.Equal(t, int64(8), recorder.observations[0].RequestBytes)
	})
}

// TestHTTPUpstreamDoObservesRealLocalUpstreamFailThenSuccess is the end-to-end check for
// the ordinary (non-Grok) path through the public entry point and a real local upstream:
// a 4xx is observed with the bytes the server received, the retry that succeeds adds no
// observation, and a send that gets no response at all is never reported as a 4xx/5xx.
func TestHTTPUpstreamDoObservesRealLocalUpstreamFailThenSuccess(t *testing.T) {
	payload := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"local-upstream-canary"}]}`

	t.Run("400 then 200", func(t *testing.T) {
		var mu sync.Mutex
		var received [][]byte
		var calls atomic.Int64
		server := newLocalTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			mu.Lock()
			received = append(received, body)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"ok"}`))
		}))
		defer server.Close()

		svc := newTestHTTPUpstreamService(t)
		const accountID int64 = 7401
		injectCachedUpstreamClient(t, svc, accountID, &http.Transport{}, false)

		observer, recorder := newDiagnosticRecorder()
		counter := httpattempt.NewCounter()

		resp, err := svc.Do(diagnosticUpstreamRequest(t, counter, observer, server.URL+"/v1/messages", payload), "", accountID, 1)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())

		require.Len(t, recorder.observations, 1, "an ordinary non-Grok upstream must reach the diagnostic seam")
		require.Equal(t, 1, recorder.observations[0].AttemptOrdinal)
		require.Equal(t, http.StatusBadRequest, recorder.observations[0].StatusCode)
		require.Equal(t, httpattempt.DiagnosticBodyComplete, recorder.observations[0].BodyVerdict)
		require.Equal(t, payload, recorder.callbackCopy[0])

		// Retry of the same logical request succeeds: the earlier failure stays.
		resp, err = svc.Do(diagnosticUpstreamRequest(t, counter, observer, server.URL+"/v1/messages", payload), "", accountID, 1)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())

		require.Len(t, recorder.observations, 1, "a later success must not add or replace an observation")
		require.Equal(t, uint64(2), counter.Load())

		mu.Lock()
		defer mu.Unlock()
		require.Len(t, received, 2)
		require.Equal(t, payload, string(received[0]), "the diagnostic matches what the local upstream received")
	})

	t.Run("no response at all", func(t *testing.T) {
		server := newLocalTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack support", http.StatusInternalServerError)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		}))
		defer server.Close()

		svc := newTestHTTPUpstreamService(t)
		const accountID int64 = 7402
		injectCachedUpstreamClient(t, svc, accountID, &http.Transport{}, false)

		observer, recorder := newDiagnosticRecorder()
		counter := httpattempt.NewCounter()

		_, err := svc.Do(diagnosticUpstreamRequest(t, counter, observer, server.URL+"/v1/messages", payload), "", accountID, 1)
		require.Error(t, err, "the upstream closed the connection without answering")
		require.Empty(t, recorder.observations, "a send with no response is not an upstream HTTP 4xx/5xx")
		require.Equal(t, uint64(1), counter.Load(), "the attempt is still counted for the logical request")
	})
}

func TestTransportDiagnosticWorksWithoutTheLogicalRequestCounter(t *testing.T) {
	observer, recorder := newDiagnosticRecorder()
	payload := `{"input":"untracked-canary"}`
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, payload, string(body))
		return fixUpstreamResponse(req, http.StatusBadRequest, `{"error":"bad request"}`), nil
	})}

	ctx := httpattempt.WithDiagnosticObserver(t.Context(), observer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader(payload))
	require.NoError(t, err)

	resp, attempt, err := transport.roundTripAttempt(req)
	require.NoError(t, err)
	require.Nil(t, attempt, "no usage-owned attempt counter is bound")
	require.NoError(t, resp.Body.Close())

	require.Len(t, recorder.observations, 1, "the diagnostic is independent of the usage-owned audit")
	require.Zero(t, recorder.observations[0].AttemptOrdinal)
	require.Equal(t, http.StatusBadRequest, recorder.observations[0].StatusCode)
	require.Equal(t, payload, recorder.callbackCopy[0])
}

func TestTransportDiagnosticNeverLogsTheOutboundBody(t *testing.T) {
	const canary = "diagnostic-log-canary-8f2c"
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	observer, recorder := newDiagnosticRecorder()
	counter := httpattempt.NewCounter()
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		return fixUpstreamResponse(req, http.StatusInternalServerError, `{"error":"boom"}`), nil
	})}

	payload := `{"model":"claude-sonnet-4-5","input":"` + canary + `"}`
	req := diagnosticUpstreamRequest(t, counter, observer, "https://upstream.example/v1/messages", payload)
	resp, _, err := transport.roundTripAttempt(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, recorder.observations, 1)
	require.Contains(t, recorder.callbackCopy[0], canary, "the opted-in diagnostic holds the bytes")
	require.NotContains(t, logs.String(), canary, "the outbound body must never reach the logs")
}

func TestTransportReleasesTheCapturedOutboundBodyAfterObservation(t *testing.T) {
	observer, recorder := newDiagnosticRecorder()
	counter := httpattempt.NewCounter()
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		return fixUpstreamResponse(req, http.StatusBadGateway, `{"error":"boom"}`), nil
	})}

	payload := `{"model":"claude-sonnet-4-5","input":"release-canary"}`
	req := diagnosticUpstreamRequest(t, counter, observer, "https://upstream.example/v1/messages", payload)
	resp, _, err := transport.roundTripAttempt(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, payload, recorder.callbackCopy[0], "the callback sees exactly the bytes the upstream received")
	captured := recorder.observations[0].RequestBody
	require.NotEmpty(t, captured)
	for i, b := range captured {
		require.Zero(t, b, "the seam must zero the captured plaintext once the attempt is over (byte %d)", i)
	}
}
