package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	requestBodyReadInitCap    = 512
	requestBodyReadMaxInitCap = 1 << 20
	jsonUTF8BOMLen            = 3
	// maxDecompressedBodySize limits the decompressed request body to 64 MB
	// to prevent decompression bomb attacks.
	maxDecompressedBodySize = 64 << 20
	// InboundBodyObservationLimit bounds each view copied into the opt-in callback.
	// Counts still describe the whole body read by the normal gateway reader.
	InboundBodyObservationLimit = 1 << 20
)

// InboundBodyOutcome reports which of the existing gateway read steps completed.
type InboundBodyOutcome string

const (
	InboundBodyComplete     InboundBodyOutcome = "complete"
	InboundBodyReadFailed   InboundBodyOutcome = "read_failed"
	InboundBodyDecodeFailed InboundBodyOutcome = "decode_failed"
)

// InboundBodyObservation contains bounded, isolated copies of bytes already read
// by the normal gateway reader. These values have NOT been redacted. Callers must
// redact or explicitly mark any stored raw fragment as unverified. No observer is
// installed by default, and installing one never reads the request body.
type InboundBodyObservation struct {
	ContentEncoding string
	Outcome         InboundBodyOutcome
	RawBytes        int64
	RawPrefix       []byte
	DecodedBytes    int64
	DecodedPrefix   []byte
}

type inboundBodyObserverContextKey struct{}

type inboundBodyObserverState struct {
	once     sync.Once
	observer func(InboundBodyObservation)
}

// WithInboundBodyObserver opts into observing the first normal ingress body read.
// Bind this only after successful authentication. The first invocation wins even
// if the body is later replaced by another middleware before the handler runs.
func WithInboundBodyObserver(ctx context.Context, observer func(InboundBodyObservation)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, inboundBodyObserverContextKey{}, &inboundBodyObserverState{observer: observer})
}

func observeInboundBody(req *http.Request, encoding string, outcome InboundBodyOutcome, raw, decoded []byte, observedRaw int64) {
	if req == nil {
		return
	}
	state, _ := req.Context().Value(inboundBodyObserverContextKey{}).(*inboundBodyObserverState)
	if state == nil || state.observer == nil {
		return
	}
	state.once.Do(func() {
		copyPrefix := func(body []byte) []byte {
			if len(body) > InboundBodyObservationLimit {
				body = body[:InboundBodyObservationLimit]
			}
			return append([]byte(nil), body...)
		}
		// The optional Trace observer is a diagnostic side effect. A broken
		// observer must not turn an otherwise valid gateway body into a 500.
		defer func() { _ = recover() }()
		state.observer(InboundBodyObservation{
			ContentEncoding: encoding,
			Outcome:         outcome,
			RawBytes:        observedRaw,
			RawPrefix:       copyPrefix(raw),
			DecodedBytes:    int64(len(decoded)),
			DecodedPrefix:   copyPrefix(decoded),
		})
	})
}

// PrereadBody 回填已读取完成的请求体：作为 io.ReadCloser 可被再次顺序消费
// （multipart 流式解析），同时暴露 Bytes() 让 ReadRequestBodyWithPrealloc
// 直接返回原始切片，避免二次分配与复制。
//
// 注意：ReadRequestBodyWithPrealloc 对 PrereadBody 的快速路径不检查内部
// reader 是否已被（部分）消费——包装的字节完整且不可变，即使 reader 已被
// 流式消费过，Bytes() 也始终返回完整请求体。
type PrereadBody struct {
	body   []byte
	reader *bytes.Reader
}

// NewPrereadBody 包装一段已读取的请求体。
func NewPrereadBody(body []byte) *PrereadBody {
	return &PrereadBody{body: body, reader: bytes.NewReader(body)}
}

// Read 实现 io.Reader（转发给内部 bytes.Reader）。
func (p *PrereadBody) Read(b []byte) (int, error) {
	if p == nil {
		return 0, io.EOF
	}
	return p.reader.Read(b)
}

// Close 实现 io.Closer；请求体已在内存中，无需释放资源。
func (p *PrereadBody) Close() error { return nil }

// Bytes 返回完整的原始请求体切片。
func (p *PrereadBody) Bytes() []byte {
	if p == nil {
		return nil
	}
	return p.body
}

// ReadRequestBodyWithPrealloc reads request body with preallocated buffer based
// on content length, transparently decoding any Content-Encoding the upstream
// client used to compress the body (zstd, gzip, deflate).
// 已由 PrereadBody 回填的请求体直接返回其完整切片（零拷贝），不检查内部
// reader 是否已被消费——见 PrereadBody 的文档说明。
func ReadRequestBodyWithPrealloc(req *http.Request) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, nil
	}
	if preread, ok := req.Body.(*PrereadBody); ok {
		return preread.Bytes(), nil
	}

	capHint := requestBodyReadInitCap
	if req.ContentLength > 0 {
		switch {
		case req.ContentLength < int64(requestBodyReadInitCap):
			capHint = requestBodyReadInitCap
		case req.ContentLength > int64(requestBodyReadMaxInitCap):
			capHint = requestBodyReadMaxInitCap
		default:
			capHint = int(req.ContentLength)
		}
	}

	enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	reader := io.Reader(req.Body)
	var counted *inboundBodyCountingReader
	if state, _ := req.Context().Value(inboundBodyObserverContextKey{}).(*inboundBodyObserverState); state != nil && state.observer != nil {
		// The normal reader returns nil on error, but the opt-in observer still
		// needs the bytes actually consumed before that error. Never expose an
		// incomplete body to the gateway handler as a successful read.
		counted = &inboundBodyCountingReader{Reader: reader}
		reader = counted
	}
	raw, err := readRequestBodyChunks(reader, capHint, req.ContentLength)
	if err != nil {
		if counted != nil {
			observeInboundBody(req, enc, InboundBodyReadFailed, counted.prefix, nil, counted.count)
		}
		return nil, err
	}
	if enc == "" || enc == "identity" {
		observeInboundBody(req, enc, InboundBodyComplete, raw, raw, int64(len(raw)))
		return raw, nil
	}

	decoded, err := decompressRequestBody(enc, raw)
	if err != nil {
		observeInboundBody(req, enc, InboundBodyDecodeFailed, raw, nil, int64(len(raw)))
		return nil, fmt.Errorf("decode Content-Encoding %q: %w", enc, err)
	}
	observeInboundBody(req, enc, InboundBodyComplete, raw, decoded, int64(len(raw)))

	req.Header.Del("Content-Encoding")
	req.Header.Del("Content-Length")
	req.ContentLength = int64(len(decoded))

	return decoded, nil
}

type inboundBodyCountingReader struct {
	io.Reader
	count  int64
	prefix []byte
}

func (r *inboundBodyCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.count += int64(n)
	if remaining := InboundBodyObservationLimit - len(r.prefix); n > 0 && remaining > 0 {
		if n < remaining {
			remaining = n
		}
		r.prefix = append(r.prefix, p[:remaining]...)
	}
	return n, err
}

// Read bounded chunks as bytes arrive, then assemble the exact-size result.
// This avoids doubling large buffers or eagerly allocating an untrusted
// Content-Length before the corresponding bytes have arrived.
func readRequestBodyChunks(reader io.Reader, initialCapacity int, contentLength int64) ([]byte, error) {
	capacity := initialCapacity
	var chunks [][]byte
	total := 0
	for {
		chunkCapacity := capacity
		if remaining := contentLength - int64(total); remaining >= 0 && remaining < int64(chunkCapacity) {
			chunkCapacity = int(remaining) + 1
		}
		chunk := make([]byte, chunkCapacity)
		n := 0
		var err error
		for n < len(chunk) && err == nil {
			var read int
			read, err = reader.Read(chunk[n:])
			n += read
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		if n > 0 {
			chunks = append(chunks, chunk[:n])
			total += n
		}
		if err != nil {
			if len(chunks) == 0 {
				return chunk[:0], nil
			}
			if len(chunks) == 1 {
				return chunks[0], nil
			}
			body := make([]byte, total)
			offset := 0
			for _, part := range chunks {
				offset += copy(body[offset:], part)
			}
			return body, nil
		}
		if capacity < requestBodyReadMaxInitCap {
			capacity *= 2
			if capacity > requestBodyReadMaxInitCap {
				capacity = requestBodyReadMaxInitCap
			}
		}
	}
}

// ReadLenientJSONRequestBodyWithPrealloc reads a request body and normalizes
// JSON string control bytes before strict validation.
func ReadLenientJSONRequestBodyWithPrealloc(req *http.Request, maxNormalizedBytes int64) ([]byte, error) {
	body, err := ReadRequestBodyWithPrealloc(req)
	if err != nil {
		return nil, err
	}
	return NormalizeLenientJSONRequestBody(body, maxNormalizedBytes)
}

func decompressRequestBody(encoding string, raw []byte) ([]byte, error) {
	switch encoding {
	case "zstd":
		dec, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		return io.ReadAll(io.LimitReader(dec, maxDecompressedBodySize))
	case "gzip", "x-gzip":
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = gr.Close() }()
		return io.ReadAll(io.LimitReader(gr, maxDecompressedBodySize))
	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return io.ReadAll(io.LimitReader(zr, maxDecompressedBodySize))
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
}

// NormalizeLenientJSONRequestBody escapes raw control bytes that broken
// OpenAI-compatible clients sometimes place inside JSON strings.
func NormalizeLenientJSONRequestBody(body []byte, maxNormalizedBytes int64) ([]byte, error) {
	if maxNormalizedBytes <= 0 {
		maxNormalizedBytes = maxDecompressedBodySize
	}

	body = trimUTF8BOM(body)
	if len(body) == 0 {
		return body, nil
	}
	if int64(len(body)) > maxNormalizedBytes {
		return nil, &http.MaxBytesError{Limit: maxNormalizedBytes}
	}

	var out []byte
	inString := false
	escaped := false
	for i, b := range body {
		if inString && isJSONControlByte(b) {
			if out == nil {
				capHint := len(body) + 6
				if int64(capHint) > maxNormalizedBytes {
					capHint = int(maxNormalizedBytes)
				}
				out = make([]byte, 0, capHint)
				out = append(out, body[:i]...)
			}
			if int64(len(out)+6) > maxNormalizedBytes {
				return nil, &http.MaxBytesError{Limit: maxNormalizedBytes}
			}
			out = appendJSONUnicodeEscape(out, b)
			escaped = false
			continue
		}

		switch {
		case escaped:
			escaped = false
		case inString && b == '\\':
			escaped = true
		case b == '"':
			inString = !inString
		}

		if out != nil {
			if int64(len(out)+1) > maxNormalizedBytes {
				return nil, &http.MaxBytesError{Limit: maxNormalizedBytes}
			}
			out = append(out, b)
		}
	}
	if out != nil {
		return out, nil
	}
	return body, nil
}

func trimUTF8BOM(body []byte) []byte {
	if len(body) >= jsonUTF8BOMLen && body[0] == 0xef && body[1] == 0xbb && body[2] == 0xbf {
		return body[jsonUTF8BOMLen:]
	}
	return body
}

func isJSONControlByte(b byte) bool {
	return b < 0x20 || b == 0x7f
}

func appendJSONUnicodeEscape(dst []byte, b byte) []byte {
	const hex = "0123456789abcdef"
	return append(dst, '\\', 'u', '0', '0', hex[b>>4], hex[b&0x0f])
}
