package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func compressedBody(t *testing.T, encoding string, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch encoding {
	case "gzip":
		writer := gzip.NewWriter(&buf)
		if _, err := writer.Write(plain); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	case "deflate":
		writer := zlib.NewWriter(&buf)
		if _, err := writer.Write(plain); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	case "zstd":
		writer, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(plain); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unexpected encoding: %s", encoding)
	}
	return buf.Bytes()
}

func TestInboundBodyObservationReportsOnlyFirstReadWithBothViews(t *testing.T) {
	plain := []byte(`{"metadata":{"user_id":"{\"device_id\":\"abc\"}"},"messages":["hello"]}`)
	for _, encoding := range []string{"gzip", "zstd", "deflate"} {
		t.Run(encoding, func(t *testing.T) {
			compressed := compressedBody(t, encoding, plain)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(compressed))
			req.Header.Set("Content-Encoding", encoding)
			var events []InboundBodyObservation
			req = req.WithContext(WithInboundBodyObserver(req.Context(), func(event InboundBodyObservation) {
				events = append(events, event)
			}))
			got, err := ReadRequestBodyWithPrealloc(req)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("decoded body mismatch: %q", got)
			}
			if req.Header.Get("Content-Encoding") != "" {
				t.Fatal("normal decoder must still clear Content-Encoding")
			}
			if len(events) != 1 {
				t.Fatalf("expected one observation, got %d", len(events))
			}
			first := events[0]
			if first.ContentEncoding != encoding || first.Outcome != InboundBodyComplete {
				t.Fatalf("wrong source/outcome: %#v", first)
			}
			if first.RawBytes != int64(len(compressed)) || !bytes.Equal(first.RawPrefix, compressed) {
				t.Fatalf("wire bytes mismatch: %d, %q", first.RawBytes, first.RawPrefix)
			}
			if first.DecodedBytes != int64(len(plain)) || !bytes.Equal(first.DecodedPrefix, plain) {
				t.Fatalf("decoded bytes mismatch: %d, %q", first.DecodedBytes, first.DecodedPrefix)
			}
			req.Body = NewPrereadBody(got)
			_, err = ReadRequestBodyWithPrealloc(req)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 {
				t.Fatalf("PrereadBody should not reobserve, got %d", len(events))
			}
			req.Body = io.NopCloser(bytes.NewReader([]byte(`{"model":"rewritten"}`)))
			_, err = ReadRequestBodyWithPrealloc(req)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 {
				t.Fatalf("later body rewrite should not reobserve, got %d", len(events))
			}
		})
	}
}

func TestInboundBodyObservationRequiresExplicitMarker(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte("unobserved")))
	got, err := ReadRequestBodyWithPrealloc(req)
	if err != nil || string(got) != "unobserved" {
		t.Fatalf("request behavior changed: %q, %v", got, err)
	}
	var called bool
	req.Body = NewPrereadBody(got)
	req = req.WithContext(WithInboundBodyObserver(req.Context(), func(InboundBodyObservation) { called = true }))
	_, err = ReadRequestBodyWithPrealloc(req)
	if err != nil || called {
		t.Fatalf("marker added after first read must not recast preread bytes as client ingress: %v", err)
	}
}

func TestInboundBodyObservationLimitsCopiedBytes(t *testing.T) {
	plain := bytes.Repeat([]byte("x"), InboundBodyObservationLimit+23)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(plain))
	var event InboundBodyObservation
	req = req.WithContext(WithInboundBodyObserver(req.Context(), func(observed InboundBodyObservation) { event = observed }))
	got, err := ReadRequestBodyWithPrealloc(req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("body altered by observer")
	}
	if event.RawBytes != int64(len(plain)) || len(event.RawPrefix) != InboundBodyObservationLimit ||
		event.DecodedBytes != int64(len(plain)) || len(event.DecodedPrefix) != InboundBodyObservationLimit {
		t.Fatalf("snapshot should be bounded while byte counts remain exact: %+v", event)
	}
	event.RawPrefix[0] = 'z'
	if got[0] != 'x' {
		t.Fatal("observer snapshot must not alias forwarded bytes")
	}
}

type ingressFailReader struct {
	err error
}

func (r *ingressFailReader) Read(p []byte) (int, error) {
	return copy(p, []byte("observed-prefix")), r.err
}
func (*ingressFailReader) Close() error { return nil }

func TestInboundBodyObservationRetainsActualPartialBytesOnReadFailure(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Body = &ingressFailReader{err: io.ErrUnexpectedEOF}
	req.ContentLength = -1
	var event InboundBodyObservation
	req = req.WithContext(WithInboundBodyObserver(req.Context(), func(observed InboundBodyObservation) { event = observed }))
	got, err := ReadRequestBodyWithPrealloc(req)
	if !errors.Is(err, io.ErrUnexpectedEOF) || got != nil {
		t.Fatalf("read error changed: %q %v", got, err)
	}
	if event.Outcome != InboundBodyReadFailed || event.RawBytes != int64(len("observed-prefix")) || string(event.RawPrefix) != "observed-prefix" {
		t.Fatalf("partial bytes lost or marked complete: %#v", event)
	}
	if event.DecodedBytes != 0 || len(event.DecodedPrefix) != 0 {
		t.Fatalf("must not invent decoded view: %#v", event)
	}
}

func TestInboundBodyObservationKeepsDecodeFailureAndRawView(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte("invalid gzip payload")))
	req.Header.Set("Content-Encoding", "gzip")
	var event InboundBodyObservation
	req = req.WithContext(WithInboundBodyObserver(req.Context(), func(observed InboundBodyObservation) { event = observed }))
	got, err := ReadRequestBodyWithPrealloc(req)
	if err == nil || got != nil {
		t.Fatalf("decode failure changed: %q %v", got, err)
	}
	if event.Outcome != InboundBodyDecodeFailed || string(event.RawPrefix) != "invalid gzip payload" || event.DecodedBytes != 0 {
		t.Fatalf("decode failure must not invent plaintext: %#v", event)
	}
	if req.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("decoder must leave failed request unchanged")
	}
}

func TestInboundBodyObserverPanicNeverChangesGatewayRead(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(samplePayload)))
	req = req.WithContext(WithInboundBodyObserver(req.Context(), func(InboundBodyObservation) {
		panic("observer failure")
	}))
	got, err := ReadRequestBodyWithPrealloc(req)
	if err != nil || string(got) != samplePayload {
		t.Fatalf("capture callback must not alter gateway body or result: %q %v", got, err)
	}
}

func TestInboundBodyObservationNeverRunsBeforeBodyRead(t *testing.T) {
	var calls int
	ctx := WithInboundBodyObserver(context.Background(), func(InboundBodyObservation) { calls++ })
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"model":"x"}`))).WithContext(ctx)
	if calls != 0 {
		t.Fatalf("installing marker read the body: %d", calls)
	}
	_, err := ReadRequestBodyWithPrealloc(req)
	if err != nil || calls != 1 {
		t.Fatalf("first normal read should observe once: %d %v", calls, err)
	}
}
