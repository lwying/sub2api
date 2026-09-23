package repository

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpattempt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

func TestGrokFallbackForcedGateRunsForBothActualSends(t *testing.T) {
	var calls atomic.Int64
	counter := httpattempt.NewCounter()
	var gateCalls atomic.Int64
	counter.SetBeforeAttempt(func(_ context.Context, metadata httpattempt.Metadata) error {
		gateCalls.Add(1)
		if header, ok := metadata.RequestHeaders["Authorization"].(map[string]any); ok && header["present"] == true {
			return nil
		}
		return errors.New("reservation unavailable")
	}, true)
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.Hostname() == grokCLIProxyHost {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"error":"Access denied"}`)),
				Request:    req,
			}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	})}
	payload := []byte(`{"model":"grok-4.5"}`)
	req, err := http.NewRequestWithContext(httpattempt.WithCounter(t.Context(), counter), http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer oauth-token")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int64(2), calls.Load())
	require.Equal(t, int64(2), gateCalls.Load())
	require.Equal(t, uint64(2), counter.Load())
	require.Len(t, counter.Metadata(), 2)
}

func TestGrokFallbackTransportCapturesSanitizedHTTPAttemptFacts(t *testing.T) {
	payload := []byte(`{"model":"grok-4.5","input":"private prompt"}`)
	responseBody := `{"id":"response-ok"}`
	counter := httpattempt.NewCounter()
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotPayload, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, payload, gotPayload)
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header: http.Header{
				"Content-Type":     {"application/json; charset=utf-8"},
				"Cache-Control":    {"no-store"},
				"Set-Cookie":       {"session=secret"},
				"Www-Authenticate": {"Bearer realm=private"},
				"X-Upstream-Token": {"opaque-token"},
				"X-Request-Id":     {"attacker-controlled"},
			},
			Body: io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})}
	req, err := http.NewRequestWithContext(httpattempt.WithCounter(t.Context(), counter), http.MethodPost, "https://upstream.example/v1/responses", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header = http.Header{
		"X-Stainless-Lang":        {"go", "rust"},
		"X-Stainless-OS":          {"Windows\\r\\nAuthorization: Bearer leaked"},
		"X-Stainless-Retry-Count": {"2"},
		"Authorization":           {"Bearer request-secret"},
		"X-Internal-Token":        {"request-token"},
		"X-Request-Id":            {"attacker-controlled"},
	}
	req.ContentLength = int64(len(payload) + 17)
	req = req.WithContext(httpattempt.WithMetadata(req.Context(), httpattempt.Metadata{
		AccountID: 7, Model: "unpersisted-model-alias", Protocol: "openai.responses",
	}))

	resp, _, err := transport.roundTripAttempt(req)
	require.NoError(t, err)
	gotResponse, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, responseBody, string(gotResponse))
	require.NoError(t, resp.Body.Close())

	got := counter.Metadata()
	require.Len(t, got, 1)
	require.Equal(t, int64(len(payload)), *got[0].RequestBytes)
	require.Equal(t, http.StatusCreated, *got[0].StatusCode)
	require.Equal(t, int64(len(responseBody)), *got[0].ResponseBytes)
	require.True(t, *got[0].ResponseReadComplete)
	require.Equal(t, map[string]any{
		"X-Stainless-Lang":         []string{"go", "rust"},
		"X-Stainless-Retry-Count":  "2",
		"Authorization":            map[string]any{"present": true},
		"Sensitive-Header-Present": map[string]any{"present": true},
		"X-Request-ID":             map[string]any{"present": true},
	}, got[0].RequestHeaders)
	require.Equal(t, map[string]any{
		"Content-Type":             "application/json",
		"Cache-Control":            "no-store",
		"Set-Cookie":               map[string]any{"present": true},
		"Www-Authenticate":         map[string]any{"present": true},
		"Sensitive-Header-Present": map[string]any{"present": true},
		"X-Request-ID":             map[string]any{"present": true},
	}, got[0].ResponseHeaders)
}

func TestGrokFallbackTransportHonorsForcedAuditGateBeforeRoundTrip(t *testing.T) {
	var calls atomic.Int64
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	})}
	counter := httpattempt.NewCounter()
	counter.SetBeforeAttempt(func(context.Context, httpattempt.Metadata) error {
		return errors.New("audit store down")
	}, true)
	req, err := http.NewRequestWithContext(httpattempt.WithCounter(t.Context(), counter), http.MethodGet, "https://example.com", nil)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.Error(t, err)
	require.Nil(t, resp)
	require.Zero(t, calls.Load())
}

// When forced audit mode rejects the official-API fallback attempt before it is
// sent, the transport must not swallow that verdict and hand back the CLI 403:
// doing so would silently defeat the pre-send block (the request would look like
// a plain entitlement denial) and no attempt row would exist for it.
func TestGrokFallbackForcedAuditFailureOnFallbackAttemptIsPropagated(t *testing.T) {
	var sends atomic.Int64
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		require.Equal(t, grokCLIProxyHost, req.URL.Hostname(), "only the CLI proxy attempt may reach the network")
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"Access denied"}`)),
			Request:    req,
		}, nil
	})}

	requiredCause := errors.New("reservation unavailable")
	var gateCalls atomic.Int64
	counter := httpattempt.NewCounter()
	counter.SetBeforeAttempt(func(context.Context, httpattempt.Metadata) error {
		if gateCalls.Add(1) == 1 {
			return nil
		}
		return requiredCause
	}, true)

	req, err := http.NewRequestWithContext(
		httpattempt.WithCounter(t.Context(), counter),
		http.MethodPost,
		"https://cli-chat-proxy.grok.com/v1/responses",
		bytes.NewReader([]byte(`{"model":"grok-4.5"}`)),
	)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer oauth-token")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")

	resp, err := transport.RoundTrip(req)
	require.Error(t, err)
	require.Nil(t, resp)
	require.True(t, httpattempt.IsRequiredAuditError(err))
	require.ErrorIs(t, err, requiredCause)
	require.Equal(t, int64(1), sends.Load(), "the blocked fallback attempt must not be sent")
	require.Equal(t, int64(2), gateCalls.Load(), "the gate runs for both actual sends")
	require.Equal(t, uint64(1), counter.Load())
}

// Ordinary fallback transport failures keep the existing contract: the original
// CLI 403 is replayed to the caller so account scheduling semantics do not change.
func TestGrokFallbackOrdinaryTransportFailurePreservesCLI403(t *testing.T) {
	var sends atomic.Int64
	fallbackCause := errors.New("dial tcp: connection refused")
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if sends.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":"Access denied"}`)),
				Request:    req,
			}, nil
		}
		return nil, fallbackCause
	})}

	counter := httpattempt.NewCounter()
	counter.SetBeforeAttempt(func(context.Context, httpattempt.Metadata) error { return nil }, true)
	req, err := http.NewRequestWithContext(
		httpattempt.WithCounter(t.Context(), counter),
		http.MethodPost,
		"https://cli-chat-proxy.grok.com/v1/responses",
		bytes.NewReader([]byte(`{"model":"grok-4.5"}`)),
	)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer oauth-token")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.JSONEq(t, `{"error":"Access denied"}`, string(body))
	require.Equal(t, int64(2), sends.Load())
}

func TestHTTPAttemptReadFailureRecordsPartialBytesWithoutStatusOnTransportError(t *testing.T) {
	t.Run("response read error", func(t *testing.T) {
		counter := httpattempt.NewCounter()
		transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(&errorAfterBytesReader{data: []byte("partial response"), err: errors.New("read failed")}),
				Request:    req,
			}, nil
		})}
		req, err := http.NewRequestWithContext(httpattempt.WithCounter(t.Context(), counter), http.MethodGet, "https://upstream.example/v1/responses", nil)
		require.NoError(t, err)

		resp, _, err := transport.roundTripAttempt(req)
		require.NoError(t, err)
		buffer := make([]byte, 64)
		n, err := resp.Body.Read(buffer)
		require.Equal(t, "read failed", err.Error())
		require.Equal(t, len("partial response"), n)
		require.NoError(t, resp.Body.Close())

		attempt := counter.Metadata()[0]
		require.Equal(t, int64(0), *attempt.RequestBytes, "nil request body has a known zero payload")
		require.Equal(t, int64(n), *attempt.ResponseBytes)
		require.False(t, *attempt.ResponseReadComplete)
		require.Equal(t, http.StatusOK, *attempt.StatusCode)
	})

	t.Run("transport error has no status", func(t *testing.T) {
		counter := httpattempt.NewCounter()
		transportErr := errors.New("dial failed")
		transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, transportErr
		})}
		req, err := http.NewRequestWithContext(httpattempt.WithCounter(t.Context(), counter), http.MethodGet, "https://upstream.example/v1/responses", nil)
		require.NoError(t, err)

		resp, _, err := transport.roundTripAttempt(req)
		require.ErrorIs(t, err, transportErr)
		require.Nil(t, resp)
		attempt := counter.Metadata()[0]
		require.Nil(t, attempt.StatusCode)
		require.Nil(t, attempt.ResponseBytes)
		require.Nil(t, attempt.ResponseReadComplete)
		require.Equal(t, int64(0), *attempt.RequestBytes)
	})
}

type errorAfterBytesReader struct {
	data []byte
	err  error
}

func (r *errorAfterBytesReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestHTTPAttemptResponseCloseBeforeEOFRecordsPartialBytes(t *testing.T) {
	counter := httpattempt.NewCounter()
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("partial-response")),
			Request:    req,
		}, nil
	})}
	req, err := http.NewRequestWithContext(httpattempt.WithCounter(t.Context(), counter), http.MethodGet, "https://upstream.example/v1/responses", nil)
	require.NoError(t, err)

	resp, _, err := transport.roundTripAttempt(req)
	require.NoError(t, err)
	buffer := make([]byte, 3)
	n, err := resp.Body.Read(buffer)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.NoError(t, resp.Body.Close())

	attempt := counter.Metadata()[0]
	require.Equal(t, int64(3), *attempt.ResponseBytes)
	require.False(t, *attempt.ResponseReadComplete)
}

func TestGrokFallbackTransportOrdinaryAuditFailureStillSends(t *testing.T) {
	var calls atomic.Int64
	transport := &grokAccessDeniedFallbackTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	})}
	counter := httpattempt.NewCounter()
	counter.SetBeforeAttempt(func(context.Context, httpattempt.Metadata) error {
		return errors.New("audit store down")
	}, false)
	req, err := http.NewRequestWithContext(httpattempt.WithCounter(t.Context(), counter), http.MethodGet, "https://example.com", nil)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, int64(1), calls.Load())
}

func TestHTTPUpstreamDoCanDisableRedirectsPerRequest(t *testing.T) {
	var redirectedCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	upstream := NewHTTPUpstream(nil)
	req, err := http.NewRequestWithContext(
		service.WithHTTPUpstreamRedirectsDisabled(t.Context()),
		http.MethodGet,
		redirector.URL,
		nil,
	)
	require.NoError(t, err)
	counter := httpattempt.NewCounter()
	req = req.WithContext(httpattempt.WithCounter(req.Context(), counter))

	resp, err := upstream.Do(req, "", 1, 1)
	require.NoError(t, err)
	require.Equal(t, http.StatusFound, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Zero(t, redirectedCalls.Load())
	require.Equal(t, uint64(1), counter.Load())
}

func TestHTTPUpstreamDoWithTLSPlainHTTPUsesConfiguredHTTPProxy(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(upstream.Close)
	var proxyCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(proxy.Close)

	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	require.NoError(t, err)
	client := NewHTTPUpstream(nil)
	resp, err := client.DoWithTLS(req, proxy.URL, 41, 1, &tlsfingerprint.Profile{Name: "unused-for-http"})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int64(1), proxyCalls.Load())
	require.Zero(t, upstreamCalls.Load(), "plain HTTP must not bypass the configured proxy")
}

func TestHTTPUpstreamDoWithTLSPlainHTTPUsesConfiguredSOCKSProxy(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	proxyURL, proxyCalls := startTestSOCKS5Proxy(t)

	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	require.NoError(t, err)
	client := NewHTTPUpstream(nil)
	resp, err := client.DoWithTLS(req, proxyURL, 42, 1, &tlsfingerprint.Profile{Name: "unused-for-http"})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int64(1), proxyCalls.Load())
	require.Equal(t, int64(1), upstreamCalls.Load())
}

func TestTLSFingerprintHTTPSProxyFallsBackWithoutBypassingProxy(t *testing.T) {
	proxyURL, err := url.Parse("https://user:pass@proxy.example:8443")
	require.NoError(t, err)
	transport, err := buildUpstreamTransportWithTLSFingerprint(poolSettings{}, proxyURL, &tlsfingerprint.Profile{Name: "test"})
	require.NoError(t, err)
	require.NotNil(t, transport.Proxy)
	require.Nil(t, transport.DialTLSContext)
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "upstream.example"}}
	resolved, err := transport.Proxy(req)
	require.NoError(t, err)
	require.Equal(t, "https://user:pass@proxy.example:8443", resolved.String())
}

func startTestSOCKS5Proxy(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	calls := &atomic.Int64{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			calls.Add(1)
			go serveTestSOCKS5Conn(conn)
		}
	}()
	return "socks5h://" + listener.Addr().String(), calls
}

func serveTestSOCKS5Conn(client net.Conn) {
	defer func() { _ = client.Close() }()
	header := make([]byte, 2)
	if _, err := io.ReadFull(client, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(client, request); err != nil || request[0] != 5 || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(client, address); err != nil {
			return
		}
		host = net.IP(address).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(client, length); err != nil {
			return
		}
		address := make([]byte, int(length[0]))
		if _, err := io.ReadFull(client, address); err != nil {
			return
		}
		host = string(address)
	case 4:
		address := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(client, address); err != nil {
			return
		}
		host = net.IP(address).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(client, portBytes); err != nil {
		return
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(portBytes))))
	if err != nil {
		_, _ = client.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = target.Close() }()
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(target, client); _ = target.Close() }()
	_, _ = io.Copy(client, target)
}

func TestHTTPUpstreamDoAppliesGrokCLIIdentityBeforeOAuthRoundTrip(t *testing.T) {
	t.Setenv("XAI_GROK_CLI_VERSION", "")

	for _, endpoint := range []string{"responses", "chat/completions"} {
		t.Run(endpoint, func(t *testing.T) {
			upstream := NewHTTPUpstream(nil)
			svc, ok := upstream.(*httpUpstreamService)
			require.True(t, ok)

			const accountID int64 = 4084
			isolation := svc.getIsolationMode()
			profile := service.HTTPUpstreamProfileDefault
			proxyKey := directProxyKey
			protocolMode := svc.resolveProtocolMode(profile, proxyKey, nil)
			settings := svc.resolvePoolSettings(isolation, 1)
			settings = svc.applyProfilePoolSettings(settings, profile)
			cacheKey := buildCacheKey(isolation, proxyKey, accountID, protocolMode)

			var capturedHeaders http.Header
			svc.clients[cacheKey] = &upstreamClientEntry{
				client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					capturedHeaders = req.Header.Clone()
					statusCode := http.StatusOK
					if req.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" {
						statusCode = http.StatusForbidden
					}
					return &http.Response{
						StatusCode: statusCode,
						Header:     make(http.Header),
						Body:       http.NoBody,
						Request:    req,
					}, nil
				})},
				proxyKey:     proxyKey,
				poolKey:      buildPoolKey(settings, protocolMode),
				protocolMode: protocolMode,
			}

			req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/"+endpoint, nil)
			require.NoError(t, err)
			req.Header.Set("User-Agent", "legacy-client/1.0")

			resp, err := svc.Do(req, "", accountID, 1)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())

			require.Equal(t, xai.CLIClientVersion, capturedHeaders.Get("x-grok-client-version"))
			require.Equal(t, "xai-grok-cli", capturedHeaders.Get("X-XAI-Token-Auth"))
			require.Equal(t, xai.CLIUserAgent(xai.CLIClientVersion), capturedHeaders.Get("User-Agent"))
		})
	}
}

func TestHTTPUpstreamDoFallsBackToOfficialGrokAPIOnCLIAccessDenied(t *testing.T) {
	upstream := NewHTTPUpstream(nil)
	svc, ok := upstream.(*httpUpstreamService)
	require.True(t, ok)

	const accountID int64 = 4421
	isolation := svc.getIsolationMode()
	profile := service.HTTPUpstreamProfileDefault
	proxyKey := directProxyKey
	protocolMode := svc.resolveProtocolMode(profile, proxyKey, nil)
	settings := svc.resolvePoolSettings(isolation, 1)
	settings = svc.applyProfilePoolSettings(settings, profile)
	cacheKey := buildCacheKey(isolation, proxyKey, accountID, protocolMode)

	payload := []byte(`{"model":"grok-4.5","input":"hello"}`)
	var calls int
	var fallbackBody []byte
	var fallbackHeaders http.Header
	svc.clients[cacheKey] = &upstreamClientEntry{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			if calls == 1 {
				require.Equal(t, grokCLIProxyHost, req.URL.Hostname())
				require.Equal(t, "xai-grok-cli", req.Header.Get("X-XAI-Token-Auth"))
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"Access denied"}`)),
					Request:    req,
				}, nil
			}

			fallbackBody = body
			fallbackHeaders = req.Header.Clone()
			require.Equal(t, grokOfficialAPIHost, req.URL.Hostname())
			require.Equal(t, "/v1/responses", req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"id":"response-ok"}`)),
				Request:    req,
			}, nil
		})},
		proxyKey:     proxyKey,
		poolKey:      buildPoolKey(settings, protocolMode),
		protocolMode: protocolMode,
	}

	req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", bytes.NewReader(payload))
	require.NoError(t, err)
	counter := httpattempt.NewCounter()
	ctx := httpattempt.WithCounter(req.Context(), counter)
	metadata := httpattempt.Metadata{AccountID: accountID, Model: "grok-4.5", Protocol: "openai.responses"}
	req = req.WithContext(httpattempt.WithMetadata(ctx, metadata))
	req.Header.Set("Authorization", "Bearer oauth-token")

	resp, err := svc.Do(req, "", accountID, 1)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.JSONEq(t, `{"id":"response-ok"}`, string(responseBody))
	require.Equal(t, 2, calls)
	require.Equal(t, uint64(2), counter.Load())
	attempts := counter.Metadata()
	require.Len(t, attempts, 2)
	for _, attempt := range attempts {
		require.Equal(t, accountID, attempt.AccountID)
		require.Equal(t, metadata.Model, attempt.Model)
		require.Equal(t, metadata.Protocol, attempt.Protocol)
		wantHeaders := map[string]any{
			"Authorization":        map[string]any{"present": true},
			"Other-Header-Present": map[string]any{"present": true},
		}
		if attempt.StatusCode != nil && *attempt.StatusCode == http.StatusForbidden {
			wantHeaders["User-Agent"] = map[string]any{"present": true}
			wantHeaders["Sensitive-Header-Present"] = map[string]any{"present": true}
		}
		require.Equal(t, wantHeaders, attempt.RequestHeaders)
		require.NotNil(t, attempt.StatusCode)
		require.NotNil(t, attempt.RequestBytes)
		require.Equal(t, int64(len(payload)), *attempt.RequestBytes)
		require.NotContains(t, attempt.RequestHeaders, "prompt")
		require.NotContains(t, attempt.ResponseHeaders, "Set-Cookie")
	}
	require.Equal(t, http.StatusForbidden, *attempts[0].StatusCode)
	require.Equal(t, http.StatusOK, *attempts[1].StatusCode)
	require.Equal(t, payload, fallbackBody)
	require.Equal(t, "Bearer oauth-token", fallbackHeaders.Get("Authorization"))
	require.Empty(t, fallbackHeaders.Get("X-XAI-Token-Auth"))
	require.Empty(t, fallbackHeaders.Get("x-grok-client-version"))
	require.Empty(t, fallbackHeaders.Get("User-Agent"))
}

func TestGrokAccessDeniedFallbackRecognizesChatEndpointPermissionDenied(t *testing.T) {
	var hosts []string
	transport := &grokAccessDeniedFallbackTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hosts = append(hosts, req.URL.Hostname())
			if req.URL.Hostname() == grokCLIProxyHost {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`{"code":"permission_denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please contact support."}`,
					)),
					Request: req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"id":"response-ok"}`)),
				Request:    req,
			}, nil
		}),
	}

	req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", strings.NewReader(`{"model":"grok-4.5"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer oauth-token")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{grokCLIProxyHost, grokOfficialAPIHost}, hosts)
}

func TestIsGrokCLICompatibilityAccessDenied(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "legacy compatibility wording", body: `{"error":"Access denied"}`, want: true},
		{
			name: "observed chat endpoint permission denial",
			body: `{"code":"permission_denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please contact support."}`,
			want: true,
		},
		{
			name: "entitlement denial using the same broad terms",
			body: `{"code":"permission_denied","error":"Access to the chat endpoint is denied because a subscription is required"}`,
			want: false,
		},
		{
			name: "different permission denied endpoint",
			body: `{"code":"permission_denied","error":"Access to the billing endpoint is denied."}`,
			want: false,
		},
		{
			name: "wrong structured error code",
			body: `{"code":"subscription_required","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please contact support."}`,
			want: false,
		},
		{name: "malformed response", body: `permission_denied: chat endpoint denied`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isGrokCLICompatibilityAccessDenied([]byte(tt.body)))
		})
	}
}

func TestIsGrokCLIAccessDeniedFallbackCandidateRequiresAuthenticatedReplayableCLI403(t *testing.T) {
	newRequest := func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", strings.NewReader(`{"model":"grok-4.5"}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer oauth-token")
		req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
		return req
	}
	newResponse := func() *http.Response { return &http.Response{StatusCode: http.StatusForbidden} }

	t.Run("valid candidate", func(t *testing.T) {
		require.True(t, isGrokCLIAccessDeniedFallbackCandidate(newRequest(), newResponse()))
	})
	t.Run("non CLI host", func(t *testing.T) {
		req := newRequest()
		req.URL.Host = "api.x.ai"
		require.False(t, isGrokCLIAccessDeniedFallbackCandidate(req, newResponse()))
	})
	t.Run("missing CLI identity", func(t *testing.T) {
		req := newRequest()
		req.Header.Del("X-XAI-Token-Auth")
		require.False(t, isGrokCLIAccessDeniedFallbackCandidate(req, newResponse()))
	})
	t.Run("missing bearer authentication", func(t *testing.T) {
		req := newRequest()
		req.Header.Del("Authorization")
		require.False(t, isGrokCLIAccessDeniedFallbackCandidate(req, newResponse()))
	})
	t.Run("non forbidden response", func(t *testing.T) {
		resp := newResponse()
		resp.StatusCode = http.StatusUnauthorized
		require.False(t, isGrokCLIAccessDeniedFallbackCandidate(newRequest(), resp))
	})
	t.Run("non replayable request", func(t *testing.T) {
		req := newRequest()
		req.GetBody = nil
		require.False(t, isGrokCLIAccessDeniedFallbackCandidate(req, newResponse()))
	})
}

func TestHTTPUpstreamDoDoesNotFallbackForGrokEntitlementDenial(t *testing.T) {
	transport := &grokAccessDeniedFallbackTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":"subscription required"}`)),
				Request:    req,
			}, nil
		}),
	}
	req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", strings.NewReader(`{"model":"grok-4.5"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer oauth-token")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.JSONEq(t, `{"error":"subscription required"}`, string(body))
}

func TestApplyGrokCLIProxyHeaders(t *testing.T) {
	t.Run("uses pinned stable version for the CLI proxy", func(t *testing.T) {
		t.Setenv("XAI_GROK_CLI_VERSION", "")
		req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
		require.NoError(t, err)
		req.Header.Set("User-Agent", "legacy-client/1.0")

		applyGrokCLIProxyHeaders(req)

		require.Equal(t, xai.CLIClientVersion, req.Header.Get("x-grok-client-version"))
		require.Equal(t, "xai-grok-cli", req.Header.Get("X-XAI-Token-Auth"))
		require.Equal(t, xai.CLIUserAgent(xai.CLIClientVersion), req.Header.Get("User-Agent"))
	})

	t.Run("accepts a valid operator override", func(t *testing.T) {
		t.Setenv("XAI_GROK_CLI_VERSION", "0.2.121-alpha.1")
		req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/chat/completions", nil)
		require.NoError(t, err)

		applyGrokCLIProxyHeaders(req)

		require.Equal(t, "0.2.121-alpha.1", req.Header.Get("x-grok-client-version"))
		require.Equal(t, xai.CLIUserAgent("0.2.121-alpha.1"), req.Header.Get("User-Agent"))
	})

	t.Run("rejects an unsafe override", func(t *testing.T) {
		t.Setenv("XAI_GROK_CLI_VERSION", "0.2.121\r\nX-Injected: true")
		req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
		require.NoError(t, err)

		applyGrokCLIProxyHeaders(req)

		require.Equal(t, xai.CLIClientVersion, req.Header.Get("x-grok-client-version"))
		require.Empty(t, req.Header.Get("X-Injected"))
	})

	t.Run("rejects an override below the supported minimum", func(t *testing.T) {
		t.Setenv("XAI_GROK_CLI_VERSION", "0.2.119")
		req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
		require.NoError(t, err)

		applyGrokCLIProxyHeaders(req)

		require.Equal(t, xai.CLIClientVersion, req.Header.Get("x-grok-client-version"))
		require.Equal(t, xai.CLIUserAgent(xai.CLIClientVersion), req.Header.Get("User-Agent"))
	})

	t.Run("rejects a prerelease override at the minimum version", func(t *testing.T) {
		t.Setenv("XAI_GROK_CLI_VERSION", "0.2.120-beta.1")
		req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
		require.NoError(t, err)

		applyGrokCLIProxyHeaders(req)

		require.Equal(t, xai.CLIClientVersion, req.Header.Get("x-grok-client-version"))
		require.Equal(t, xai.CLIUserAgent(xai.CLIClientVersion), req.Header.Get("User-Agent"))
	})

	// Every entry sits above the pinned minimum, so a rejection here can only be
	// caused by the malformed semver and never by the version being too old.
	for _, version := range []string{
		"0.2.0121",
		"0.2.121-alpha..1",
		"0.3",
		"1",
		"0.2.121+build.1",
	} {
		t.Run("rejects invalid semver "+version, func(t *testing.T) {
			t.Setenv("XAI_GROK_CLI_VERSION", version)
			req, err := http.NewRequest(http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", nil)
			require.NoError(t, err)

			applyGrokCLIProxyHeaders(req)

			require.Equal(t, xai.CLIClientVersion, req.Header.Get("x-grok-client-version"))
			require.Equal(t, xai.CLIUserAgent(xai.CLIClientVersion), req.Header.Get("User-Agent"))
		})
	}

	t.Run("leaves direct xAI API requests unchanged", func(t *testing.T) {
		t.Setenv("XAI_GROK_CLI_VERSION", "0.2.95")
		req, err := http.NewRequest(http.MethodPost, "https://api.x.ai/v1/responses", nil)
		require.NoError(t, err)
		req.Header.Set("User-Agent", "direct-api-client/1.0")

		applyGrokCLIProxyHeaders(req)

		require.Empty(t, req.Header.Get("x-grok-client-version"))
		require.Empty(t, req.Header.Get("X-XAI-Token-Auth"))
		require.Equal(t, "direct-api-client/1.0", req.Header.Get("User-Agent"))
	})
}

// HTTPUpstreamSuite HTTP 上游服务测试套件
// 使用 testify/suite 组织测试，支持 SetupTest 初始化
type HTTPUpstreamSuite struct {
	suite.Suite
	cfg *config.Config // 测试用配置
}

// SetupTest 每个测试用例执行前的初始化
// 创建空配置，各测试用例可按需覆盖
func (s *HTTPUpstreamSuite) SetupTest() {
	s.cfg = &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				AllowPrivateHosts: true,
			},
		},
	}
}

// newService 创建测试用的 httpUpstreamService 实例
// 返回具体类型以便访问内部状态进行断言
func (s *HTTPUpstreamSuite) newService() *httpUpstreamService {
	up := NewHTTPUpstream(s.cfg)
	svc, ok := up.(*httpUpstreamService)
	require.True(s.T(), ok, "expected *httpUpstreamService")
	return svc
}

// TestDefaultResponseHeaderTimeout 测试默认响应头超时配置
// 验证显式 0 会禁用等待响应头超时
func (s *HTTPUpstreamSuite) TestDefaultResponseHeaderTimeout() {
	svc := s.newService()
	entry := mustGetOrCreateClient(s.T(), svc, "", 0, 0)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.Equal(s.T(), time.Duration(0), transport.ResponseHeaderTimeout, "ResponseHeaderTimeout mismatch")
}

// TestNilConfigResponseHeaderTimeoutFallback 验证 nil 配置使用代码级兜底值。
func (s *HTTPUpstreamSuite) TestNilConfigResponseHeaderTimeoutFallback() {
	up := NewHTTPUpstream(nil)
	svc, ok := up.(*httpUpstreamService)
	require.True(s.T(), ok, "expected *httpUpstreamService")
	entry := mustGetOrCreateClient(s.T(), svc, "", 0, 0)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.Equal(s.T(), 300*time.Second, transport.ResponseHeaderTimeout, "ResponseHeaderTimeout mismatch")
}

// TestCustomResponseHeaderTimeout 测试自定义响应头超时配置
// 验证配置值能正确应用到 Transport
func (s *HTTPUpstreamSuite) TestCustomResponseHeaderTimeout() {
	s.cfg.Gateway = config.GatewayConfig{ResponseHeaderTimeout: 7}
	svc := s.newService()
	entry := mustGetOrCreateClient(s.T(), svc, "", 0, 0)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.Equal(s.T(), 7*time.Second, transport.ResponseHeaderTimeout, "ResponseHeaderTimeout mismatch")
}

// TestGetOrCreateClient_InvalidURLReturnsError 测试无效代理 URL 返回错误
// 验证解析失败时拒绝回退到直连模式
func (s *HTTPUpstreamSuite) TestGetOrCreateClient_InvalidURLReturnsError() {
	svc := s.newService()
	_, err := svc.getClientEntry("://bad-proxy-url", 1, 1, service.HTTPUpstreamProfileDefault, false, false)
	require.Error(s.T(), err, "expected error for invalid proxy URL")
}

func (s *HTTPUpstreamSuite) TestOpenAIProfileDefaultsToHTTP2AndNoHeaderTimeout() {
	s.cfg.Gateway = config.GatewayConfig{
		ResponseHeaderTimeout: 600,
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{
			Enabled:                   true,
			AllowProxyFallbackToHTTP1: true,
		},
	}
	svc := s.newService()
	entry, err := svc.getClientEntry("", 1, 1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.Equal(s.T(), time.Duration(0), transport.ResponseHeaderTimeout, "OpenAI profile should not inherit generic header timeout")
	require.True(s.T(), transport.ForceAttemptHTTP2, "OpenAI profile should prefer HTTP/2")
	require.Equal(s.T(), upstreamProtocolModeOpenAIH2, entry.protocolMode)
}

func (s *HTTPUpstreamSuite) TestLongStreamProfileUsesSharedHTTP2KeepAlive() {
	s.cfg.Gateway = config.GatewayConfig{
		ResponseHeaderTimeout: 600,
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{
			Enabled: false,
		},
	}
	svc := s.newService()
	entry, err := svc.getClientEntry("", 1, 1, service.HTTPUpstreamProfileLongStream, false, false)
	require.NoError(s.T(), err)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.Equal(s.T(), 600*time.Second, transport.ResponseHeaderTimeout, "long-stream profile should retain the generic header timeout")
	require.True(s.T(), transport.ForceAttemptHTTP2, "long-stream profile must enable HTTP/2 independently of OpenAI settings")
	require.True(s.T(), transport.Protocols.HTTP2(), "long-stream profile must install HTTP/2 PING health checks")
	require.Equal(s.T(), upstreamProtocolModeLongStreamH2, entry.protocolMode)
}

func (s *HTTPUpstreamSuite) TestOpenAIProfileCustomHeaderTimeout() {
	s.cfg.Gateway = config.GatewayConfig{
		ResponseHeaderTimeout:       600,
		OpenAIResponseHeaderTimeout: 1800,
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{
			Enabled: true,
		},
	}
	svc := s.newService()
	entry, err := svc.getClientEntry("", 1, 1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.Equal(s.T(), 1800*time.Second, transport.ResponseHeaderTimeout)
}

func (s *HTTPUpstreamSuite) TestOpenAIProfileTLSFingerprintDoesNotInheritGenericHeaderTimeout() {
	s.cfg.Gateway = config.GatewayConfig{
		ResponseHeaderTimeout: 600,
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{
			Enabled: true,
		},
	}
	svc := s.newService()
	entry, err := svc.getClientEntryWithTLS("", 1, 1, &tlsfingerprint.Profile{Name: "test"}, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.Equal(s.T(), time.Duration(0), transport.ResponseHeaderTimeout, "OpenAI TLS path should not inherit generic header timeout")
}

func (s *HTTPUpstreamSuite) TestOpenAIProfileHTTP2DisabledUsesHTTP1Transport() {
	s.cfg.Gateway = config.GatewayConfig{
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{Enabled: false},
	}
	svc := s.newService()
	entry, err := svc.getClientEntry("", 1, 1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.False(s.T(), transport.ForceAttemptHTTP2, "OpenAI HTTP/2 disabled should not force H2")
	require.NotNil(s.T(), transport.TLSNextProto, "HTTP/1 mode should disable automatic H2 negotiation")
	require.Equal(s.T(), upstreamProtocolModeOpenAIH1, entry.protocolMode)
}

func (s *HTTPUpstreamSuite) TestOpenAIHarvestProfileDisablesKeepAlives() {
	svc := s.newService()
	entry, err := svc.getClientEntry("socks5h://user:pass@harvest.example:31", 41, 5, service.HTTPUpstreamProfileOpenAIHarvest, false, false)
	require.NoError(s.T(), err)
	require.Equal(s.T(), upstreamProtocolModeOpenAIH1NoReuse, entry.protocolMode)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.True(s.T(), transport.DisableKeepAlives)
	require.False(s.T(), transport.ForceAttemptHTTP2)
	require.Equal(s.T(), 0, transport.MaxIdleConns)
}

func (s *HTTPUpstreamSuite) TestOpenAIHeaderTimeoutChangeRebuildsClient() {
	s.cfg.Gateway = config.GatewayConfig{
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{Enabled: true},
	}
	svc := s.newService()
	entry1, err := svc.getClientEntry("", 1, 1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)

	s.cfg.Gateway.OpenAIResponseHeaderTimeout = 1800
	entry2, err := svc.getClientEntry("", 1, 1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	require.NotSame(s.T(), entry1, entry2, "OpenAI header timeout changes must rebuild cached client")
	transport, ok := entry2.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.Equal(s.T(), 1800*time.Second, transport.ResponseHeaderTimeout)
}

func (s *HTTPUpstreamSuite) TestOpenAIHTTP2TimeoutDoesNotActivateProxyFallback() {
	s.cfg.Gateway = config.GatewayConfig{
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{
			Enabled:                   true,
			AllowProxyFallbackToHTTP1: true,
			FallbackErrorThreshold:    1,
			FallbackWindowSeconds:     60,
			FallbackTTLSeconds:        600,
		},
	}
	svc := s.newService()
	proxyURL := "http://proxy.local:8080"
	svc.recordOpenAIHTTP2Failure(service.HTTPUpstreamProfileOpenAI, upstreamProtocolModeOpenAIH2, proxyURL, errors.New("http2: timeout awaiting response headers"))
	require.False(s.T(), svc.isOpenAIHTTP2FallbackActive(proxyURL), "header timeout should not be treated as H2 compatibility failure")
}

func (s *HTTPUpstreamSuite) TestOpenAIHTTP2ProxyCompatibilityErrorActivatesFallback() {
	s.cfg.Gateway = config.GatewayConfig{
		OpenAIHTTP2: config.GatewayOpenAIHTTP2Config{
			Enabled:                   true,
			AllowProxyFallbackToHTTP1: true,
			FallbackErrorThreshold:    1,
			FallbackWindowSeconds:     60,
			FallbackTTLSeconds:        600,
		},
	}
	svc := s.newService()
	proxyURL := "http://proxy.local:8080"
	svc.recordOpenAIHTTP2Failure(service.HTTPUpstreamProfileOpenAI, upstreamProtocolModeOpenAIH2, proxyURL, errors.New("http2: protocol error"))
	require.True(s.T(), svc.isOpenAIHTTP2FallbackActive(proxyURL))

	entry, err := svc.getClientEntry(proxyURL, 1, 1, service.HTTPUpstreamProfileOpenAI, false, false)
	require.NoError(s.T(), err)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.False(s.T(), transport.ForceAttemptHTTP2)
	require.NotNil(s.T(), transport.TLSNextProto)
	require.Equal(s.T(), upstreamProtocolModeOpenAIH1Fallback, entry.protocolMode)
}

// TestNormalizeProxyURL_Canonicalizes 测试代理 URL 规范化
// 验证等价地址能够映射到同一缓存键
func (s *HTTPUpstreamSuite) TestNormalizeProxyURL_Canonicalizes() {
	key1, _, err1 := normalizeProxyURL("http://proxy.local:8080")
	require.NoError(s.T(), err1)
	key2, _, err2 := normalizeProxyURL("http://proxy.local:8080/")
	require.NoError(s.T(), err2)
	require.Equal(s.T(), key1, key2, "expected normalized proxy keys to match")
}

// TestAcquireClient_OverLimitReturnsError 测试连接池缓存上限保护
// 验证超限且无可淘汰条目时返回错误
func (s *HTTPUpstreamSuite) TestAcquireClient_OverLimitReturnsError() {
	s.cfg.Gateway = config.GatewayConfig{
		ConnectionPoolIsolation: config.ConnectionPoolIsolationAccountProxy,
		MaxUpstreamClients:      1,
	}
	svc := s.newService()
	entry1, err := svc.acquireClient("http://proxy-a:8080", 1, 1)
	require.NoError(s.T(), err, "expected first acquire to succeed")
	require.NotNil(s.T(), entry1, "expected entry")

	entry2, err := svc.acquireClient("http://proxy-b:8080", 2, 1)
	require.Error(s.T(), err, "expected error when cache limit reached")
	require.Nil(s.T(), entry2, "expected nil entry when cache limit reached")
}

// TestDo_WithoutProxy_GoesDirect 测试无代理时直连
// 验证空代理 URL 时请求直接发送到目标服务器
func (s *HTTPUpstreamSuite) TestDo_WithoutProxy_GoesDirect() {
	// 创建模拟上游服务器
	upstream := newLocalTestServer(s.T(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	s.T().Cleanup(upstream.Close)

	up := NewHTTPUpstream(s.cfg)

	req, err := http.NewRequest(http.MethodGet, upstream.URL+"/x", nil)
	require.NoError(s.T(), err, "NewRequest")
	resp, err := up.Do(req, "", 1, 1)
	require.NoError(s.T(), err, "Do")
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(s.T(), "direct", string(b), "unexpected body")
}

// TestDo_WithHTTPProxy_UsesProxy 测试 HTTP 代理功能
// 验证请求通过代理服务器转发，使用绝对 URI 格式
func (s *HTTPUpstreamSuite) TestDo_WithHTTPProxy_UsesProxy() {
	// 用于接收代理请求的通道
	seen := make(chan string, 1)
	// 创建模拟代理服务器
	proxySrv := newLocalTestServer(s.T(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.RequestURI // 记录请求 URI
		_, _ = io.WriteString(w, "proxied")
	}))
	s.T().Cleanup(proxySrv.Close)

	s.cfg.Gateway = config.GatewayConfig{ResponseHeaderTimeout: 1}
	up := NewHTTPUpstream(s.cfg)

	// 发送请求到外部地址，应通过代理
	req, err := http.NewRequest(http.MethodGet, "http://example.com/test", nil)
	require.NoError(s.T(), err, "NewRequest")
	resp, err := up.Do(req, proxySrv.URL, 1, 1)
	require.NoError(s.T(), err, "Do")
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(s.T(), "proxied", string(b), "unexpected body")

	// 验证代理收到的是绝对 URI 格式（HTTP 代理规范要求）
	select {
	case uri := <-seen:
		require.Equal(s.T(), "http://example.com/test", uri, "expected absolute-form request URI")
	default:
		require.Fail(s.T(), "expected proxy to receive request")
	}
}

// TestDo_EmptyProxy_UsesDirect 测试空代理字符串
// 验证空字符串代理等同于直连
func (s *HTTPUpstreamSuite) TestDo_EmptyProxy_UsesDirect() {
	upstream := newLocalTestServer(s.T(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "direct-empty")
	}))
	s.T().Cleanup(upstream.Close)

	up := NewHTTPUpstream(s.cfg)
	req, err := http.NewRequest(http.MethodGet, upstream.URL+"/y", nil)
	require.NoError(s.T(), err, "NewRequest")
	resp, err := up.Do(req, "", 1, 1)
	require.NoError(s.T(), err, "Do with empty proxy")
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(s.T(), "direct-empty", string(b))
}

// TestAccountIsolation_DifferentAccounts 测试账户隔离模式
// 验证不同账户使用独立的连接池
func (s *HTTPUpstreamSuite) TestAccountIsolation_DifferentAccounts() {
	s.cfg.Gateway = config.GatewayConfig{ConnectionPoolIsolation: config.ConnectionPoolIsolationAccount}
	svc := s.newService()
	// 同一代理，不同账户
	entry1 := mustGetOrCreateClient(s.T(), svc, "http://proxy.local:8080", 1, 3)
	entry2 := mustGetOrCreateClient(s.T(), svc, "http://proxy.local:8080", 2, 3)
	require.NotSame(s.T(), entry1, entry2, "不同账号不应共享连接池")
	require.Equal(s.T(), 2, len(svc.clients), "账号隔离应缓存两个客户端")
}

// TestAccountProxyIsolation_DifferentProxy 测试账户+代理组合隔离模式
// 验证同一账户使用不同代理时创建独立连接池
func (s *HTTPUpstreamSuite) TestAccountProxyIsolation_DifferentProxy() {
	s.cfg.Gateway = config.GatewayConfig{ConnectionPoolIsolation: config.ConnectionPoolIsolationAccountProxy}
	svc := s.newService()
	// 同一账户，不同代理
	entry1 := mustGetOrCreateClient(s.T(), svc, "http://proxy-a:8080", 1, 3)
	entry2 := mustGetOrCreateClient(s.T(), svc, "http://proxy-b:8080", 1, 3)
	require.NotSame(s.T(), entry1, entry2, "账号+代理隔离应区分不同代理")
	require.Equal(s.T(), 2, len(svc.clients), "账号+代理隔离应缓存两个客户端")
}

// TestAccountModeProxyChangeClearsPool 测试账户模式下代理变更
// 验证账户切换代理时清理旧连接池，避免复用错误代理
func (s *HTTPUpstreamSuite) TestAccountModeProxyChangeClearsPool() {
	s.cfg.Gateway = config.GatewayConfig{ConnectionPoolIsolation: config.ConnectionPoolIsolationAccount}
	svc := s.newService()
	// 同一账户，先后使用不同代理
	entry1 := mustGetOrCreateClient(s.T(), svc, "http://proxy-a:8080", 1, 3)
	entry2 := mustGetOrCreateClient(s.T(), svc, "http://proxy-b:8080", 1, 3)
	require.NotSame(s.T(), entry1, entry2, "账号切换代理应创建新连接池")
	require.Equal(s.T(), 1, len(svc.clients), "账号模式下应仅保留一个连接池")
	require.False(s.T(), hasEntry(svc, entry1), "旧连接池应被清理")
}

// TestAccountConcurrencyOverridesPoolSettings 测试账户并发数覆盖连接池配置
// 验证账户隔离模式下，连接池大小与账户并发数对应
func (s *HTTPUpstreamSuite) TestAccountConcurrencyOverridesPoolSettings() {
	s.cfg.Gateway = config.GatewayConfig{ConnectionPoolIsolation: config.ConnectionPoolIsolationAccount}
	svc := s.newService()
	// 账户并发数为 12
	entry := mustGetOrCreateClient(s.T(), svc, "", 1, 12)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	// 连接池参数应与并发数一致
	require.Equal(s.T(), 12, transport.MaxConnsPerHost, "MaxConnsPerHost mismatch")
	require.Equal(s.T(), 12, transport.MaxIdleConns, "MaxIdleConns mismatch")
	require.Equal(s.T(), 12, transport.MaxIdleConnsPerHost, "MaxIdleConnsPerHost mismatch")
}

// TestAccountConcurrencyFallbackToDefault 测试账户并发数为 0 时回退到默认配置
// 验证未指定并发数时使用全局配置值
func (s *HTTPUpstreamSuite) TestAccountConcurrencyFallbackToDefault() {
	s.cfg.Gateway = config.GatewayConfig{
		ConnectionPoolIsolation: config.ConnectionPoolIsolationAccount,
		MaxIdleConns:            77,
		MaxIdleConnsPerHost:     55,
		MaxConnsPerHost:         66,
	}
	svc := s.newService()
	// 账户并发数为 0，应使用全局配置
	entry := mustGetOrCreateClient(s.T(), svc, "", 1, 0)
	transport, ok := entry.client.Transport.(*http.Transport)
	require.True(s.T(), ok, "expected *http.Transport")
	require.Equal(s.T(), 66, transport.MaxConnsPerHost, "MaxConnsPerHost fallback mismatch")
	require.Equal(s.T(), 77, transport.MaxIdleConns, "MaxIdleConns fallback mismatch")
	require.Equal(s.T(), 55, transport.MaxIdleConnsPerHost, "MaxIdleConnsPerHost fallback mismatch")
}

// TestEvictOverLimitRemovesOldestIdle 测试超出数量限制时的 LRU 淘汰
// 验证优先淘汰最久未使用的空闲客户端
func (s *HTTPUpstreamSuite) TestEvictOverLimitRemovesOldestIdle() {
	s.cfg.Gateway = config.GatewayConfig{
		ConnectionPoolIsolation: config.ConnectionPoolIsolationAccountProxy,
		MaxUpstreamClients:      2, // 最多缓存 2 个客户端
	}
	svc := s.newService()
	// 创建两个客户端，设置不同的最后使用时间
	entry1 := mustGetOrCreateClient(s.T(), svc, "http://proxy-a:8080", 1, 1)
	entry2 := mustGetOrCreateClient(s.T(), svc, "http://proxy-b:8080", 2, 1)
	atomic.StoreInt64(&entry1.lastUsed, time.Now().Add(-2*time.Hour).UnixNano()) // 最久
	atomic.StoreInt64(&entry2.lastUsed, time.Now().Add(-time.Hour).UnixNano())
	// 创建第三个客户端，触发淘汰
	_ = mustGetOrCreateClient(s.T(), svc, "http://proxy-c:8080", 3, 1)

	require.LessOrEqual(s.T(), len(svc.clients), 2, "应保持在缓存上限内")
	require.False(s.T(), hasEntry(svc, entry1), "最久未使用的连接池应被清理")
}

// TestIdleTTLDoesNotEvictActive 测试活跃请求保护
// 验证有进行中请求的客户端不会被空闲超时淘汰
func (s *HTTPUpstreamSuite) TestIdleTTLDoesNotEvictActive() {
	s.cfg.Gateway = config.GatewayConfig{
		ConnectionPoolIsolation: config.ConnectionPoolIsolationAccount,
		ClientIdleTTLSeconds:    1, // 1 秒空闲超时
	}
	svc := s.newService()
	entry1 := mustGetOrCreateClient(s.T(), svc, "", 1, 1)
	// 设置为很久之前使用，但有活跃请求
	atomic.StoreInt64(&entry1.lastUsed, time.Now().Add(-2*time.Minute).UnixNano())
	atomic.StoreInt64(&entry1.inFlight, 1) // 模拟有活跃请求
	// 创建新客户端，触发淘汰检查
	_, _ = svc.getOrCreateClient("", 2, 1)

	require.True(s.T(), hasEntry(svc, entry1), "有活跃请求时不应回收")
}

// TestHTTPUpstreamSuite 运行测试套件
func TestHTTPUpstreamSuite(t *testing.T) {
	suite.Run(t, new(HTTPUpstreamSuite))
}

// mustGetOrCreateClient 测试辅助函数，调用 getOrCreateClient 并断言无错误
func mustGetOrCreateClient(t *testing.T, svc *httpUpstreamService, proxyURL string, accountID int64, concurrency int) *upstreamClientEntry {
	t.Helper()
	entry, err := svc.getOrCreateClient(proxyURL, accountID, concurrency)
	require.NoError(t, err, "getOrCreateClient(%q, %d, %d)", proxyURL, accountID, concurrency)
	return entry
}

// hasEntry 检查客户端是否存在于缓存中
// 辅助函数，用于验证淘汰逻辑
func hasEntry(svc *httpUpstreamService, target *upstreamClientEntry) bool {
	for _, entry := range svc.clients {
		if entry == target {
			return true
		}
	}
	return false
}

func TestHTTPUpstreamDoPublicHostsOnlyRejectsPrivateDestinationBeforeConnecting(t *testing.T) {
	var calls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	upstream := NewHTTPUpstream(nil)

	plain, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.URL, nil)
	require.NoError(t, err)
	plainCounter := httpattempt.NewCounter()
	plain = plain.WithContext(httpattempt.WithCounter(plain.Context(), plainCounter))
	resp, err := upstream.Do(plain, "", 1, 1)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, int64(1), calls.Load(), "loopback stays reachable for requests without the marker")
	require.Equal(t, uint64(1), plainCounter.Load())

	guarded, err := http.NewRequestWithContext(service.WithHTTPUpstreamPublicHostsOnly(t.Context()), http.MethodGet, target.URL, nil)
	require.NoError(t, err)
	guardedCounter := httpattempt.NewCounter()
	guarded = guarded.WithContext(httpattempt.WithCounter(guarded.Context(), guardedCounter))
	resp, err = upstream.Do(guarded, "", 1, 1)
	require.Error(t, err)
	require.Nil(t, resp)
	require.Contains(t, err.Error(), "not allowed")
	require.Equal(t, int64(1), calls.Load(), "marked request must be rejected before any connection is made")
	require.Zero(t, guardedCounter.Load())
}

func TestHTTPUpstreamPublicHostsOnlyValidatesEveryRedirectHop(t *testing.T) {
	upstream, ok := NewHTTPUpstream(nil).(*httpUpstreamService)
	require.True(t, ok)
	base := &http.Client{}

	plain, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://cdn.example.com/a.png", nil)
	require.NoError(t, err)
	require.Same(t, base, upstream.httpClientForUpstreamRequest(base, plain))

	guarded, err := http.NewRequestWithContext(service.WithHTTPUpstreamPublicHostsOnly(t.Context()), http.MethodGet, "https://cdn.example.com/a.png", nil)
	require.NoError(t, err)
	client := upstream.httpClientForUpstreamRequest(base, guarded)
	require.NotSame(t, base, client)
	require.NotNil(t, client.CheckRedirect)
	require.Nil(t, base.CheckRedirect, "the cached client must stay untouched")

	via := []*http.Request{guarded}
	for _, hop := range []string{
		"http://127.0.0.1:8080/a.png",
		"http://[::1]:8080/a.png",
		"http://10.0.0.8/a.png",
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0/a.png",
	} {
		hopReq, err := http.NewRequestWithContext(guarded.Context(), http.MethodGet, hop, nil)
		require.NoError(t, err)
		require.Error(t, client.CheckRedirect(hopReq, via), "hop=%s", hop)
	}
	publicHop, err := http.NewRequestWithContext(guarded.Context(), http.MethodGet, "http://93.184.216.34/a.png", nil)
	require.NoError(t, err)
	require.NoError(t, client.CheckRedirect(publicHop, via))
	require.Error(t, client.CheckRedirect(publicHop, make([]*http.Request, 10)), "redirect chain stays capped")
}
