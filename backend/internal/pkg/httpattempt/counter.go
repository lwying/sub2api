package httpattempt

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
)

type counterContextKey struct{}
type metadataContextKey struct{}

// Metadata describes one upstream HTTP transport attempt without request or response bodies.
//
// RequestHeaders / ResponseHeaders 是**长期审计**用的存在性与闭集摘要，永远只有受限信息。
// Transport never persists values; persistence is gated and validated downstream.
type Metadata struct {
	AccountID int64
	Model     string
	// Protocol 是本次尝试的**长期审计协议家族**（它的持久化形态）。
	Protocol string
	// ValueProtocol 是本次尝试**真实发出的 wire 协议族**：它与 Protocol 只在
	// 「审计家族与真实上游不同」时才有区别（当前只有 Bedrock：审计家族是
	// messages，真实上游是 AWS 的 invoke 形态）。它不落长期审计库，也不是任何
	// 采集能力的开关声明——值快照能力已退役，Trace 是它唯一的消费方。
	ValueProtocol        string
	RequestHeaders       map[string]any
	ResponseHeaders      map[string]any
	StatusCode           *int
	RequestBytes         *int64
	ResponseBytes        *int64
	ResponseReadComplete *bool
	// LatencyMillis 是本次尝试的 RoundTrip 耗时（毫秒）；未测量时为 nil。
	LatencyMillis *int64
	// ProxyID 是本次尝试实际使用的代理内部 ID；未使用代理时为 0。它只在服务层
	// 的加密值明细里落库，传输层与长期审计都不写它。
	ProxyID int64
}

// Counter tracks upstream HTTP transport attempts for one logical request.
type BeforeAttemptFunc func(context.Context, Metadata) error

type Counter struct {
	n                 atomic.Uint64
	lastPluginHandled atomic.Bool

	mu            sync.Mutex
	metadata      []Metadata
	beforeAttempt BeforeAttemptFunc
	forced        bool
}

func NewCounter() *Counter {
	return &Counter{}
}

func WithCounter(ctx context.Context, counter *Counter) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if counter == nil {
		return ctx
	}
	return context.WithValue(ctx, counterContextKey{}, counter)
}

func WithMetadata(ctx context.Context, metadata Metadata) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, metadataContextKey{}, cloneMetadata(metadata))
}

// WithRequestHeaders stores only the transport-safe request header summary for the next attempt.
func WithRequestHeaders(ctx context.Context, headers http.Header) context.Context {
	metadata, _ := MetadataFromContext(ctx)
	metadata.RequestHeaders = SanitizeRequestHeaders(headers)
	return WithMetadata(ctx, metadata)
}

// MetadataFromContext returns an isolated copy of the pending attempt metadata.
func MetadataFromContext(ctx context.Context) (Metadata, bool) {
	if ctx == nil {
		return Metadata{}, false
	}
	metadata, ok := ctx.Value(metadataContextKey{}).(Metadata)
	if !ok {
		return Metadata{}, false
	}
	return cloneMetadata(metadata), true
}

func cloneMetadata(metadata Metadata) Metadata {
	metadata.RequestHeaders = cloneHeaderMap(metadata.RequestHeaders)
	metadata.ResponseHeaders = cloneHeaderMap(metadata.ResponseHeaders)
	metadata.StatusCode = cloneInt(metadata.StatusCode)
	metadata.RequestBytes = cloneInt64(metadata.RequestBytes)
	metadata.ResponseBytes = cloneInt64(metadata.ResponseBytes)
	metadata.ResponseReadComplete = cloneBool(metadata.ResponseReadComplete)
	metadata.LatencyMillis = cloneInt64(metadata.LatencyMillis)
	return metadata
}

func cloneHeaderMap(headers map[string]any) map[string]any {
	if headers == nil {
		return nil
	}
	clone := make(map[string]any, len(headers))
	for key, value := range headers {
		switch typed := value.(type) {
		case []string:
			clone[key] = append([]string(nil), typed...)
		case map[string]any:
			inner := make(map[string]any, len(typed))
			for innerKey, innerValue := range typed {
				inner[innerKey] = innerValue
			}
			clone[key] = inner
		default:
			clone[key] = value
		}
	}
	return clone
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func FromContext(ctx context.Context) (*Counter, bool) {
	if ctx == nil {
		return nil, false
	}
	counter, ok := ctx.Value(counterContextKey{}).(*Counter)
	return counter, ok && counter != nil
}

func Increment(ctx context.Context) {
	counter, ok := FromContext(ctx)
	if !ok {
		return
	}
	if metadata, hasMetadata := MetadataFromContext(ctx); hasMetadata {
		counter.incrementMetadata(metadata)
		return
	}
	counter.n.Add(1)
	counter.lastPluginHandled.Store(false)
}

// Attempt identifies one transport RoundTrip and accepts facts collected before it returns.
type Attempt struct {
	counter *Counter
	index   int
}

// StartAttempt increments the count and snapshots pending safe metadata for one RoundTrip.
func StartAttempt(ctx context.Context) *Attempt {
	return startAttempt(ctx, metadataFromContext(ctx))
}

// StartRequestAttempt snapshots sanitized headers from the final outbound request.
//
// 只记长期审计的闭集摘要：传输层在任何路径上都不读取或复制明文头值。
func StartRequestAttempt(req *http.Request) *Attempt {
	if req == nil {
		return nil
	}
	metadata := metadataFromContext(req.Context())
	metadata.RequestHeaders = SanitizeRequestHeaders(req.Header)
	return startAttempt(req.Context(), metadata)
}

func startAttempt(ctx context.Context, metadata Metadata) *Attempt {
	counter, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	zero := int64(0)
	metadata.RequestBytes = &zero
	index := counter.incrementMetadata(metadata)
	return &Attempt{counter: counter, index: index}
}

// ordinal is the 1-based position of this attempt among the logical request's attempts.
func (a *Attempt) ordinal() int {
	if a == nil || a.counter == nil {
		return 0
	}
	return a.index + 1
}

// requestBytes is the payload byte count recorded for this attempt so far.
func (a *Attempt) requestBytes() int64 {
	if a == nil || a.counter == nil {
		return 0
	}
	a.counter.mu.Lock()
	defer a.counter.mu.Unlock()
	if a.index < 0 || a.index >= len(a.counter.metadata) {
		return 0
	}
	return dereferenceInt64(a.counter.metadata[a.index].RequestBytes)
}

func dereferenceInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (a *Attempt) AddRequestBytes(n int64) {
	if a == nil || a.counter == nil || n <= 0 {
		return
	}
	a.counter.updateMetadata(a.index, func(metadata *Metadata) {
		value := int64(0)
		if metadata.RequestBytes != nil {
			value = *metadata.RequestBytes
		}
		value += n
		metadata.RequestBytes = &value
	})
}

// SetResponse records only status and sanitized headers; body bytes are counted as read.
func (a *Attempt) SetResponse(statusCode int, headers http.Header, bodyPresent bool) {
	if a == nil || a.counter == nil {
		return
	}
	status := statusCode
	zero := int64(0)
	complete := !bodyPresent
	a.counter.updateMetadata(a.index, func(metadata *Metadata) {
		metadata.StatusCode = &status
		metadata.ResponseHeaders = SanitizeResponseHeaders(headers)
		metadata.ResponseBytes = &zero
		metadata.ResponseReadComplete = &complete
	})
}

func (a *Attempt) AddResponseBytes(n int64) {
	if a == nil || a.counter == nil || n <= 0 {
		return
	}
	a.counter.updateMetadata(a.index, func(metadata *Metadata) {
		value := int64(0)
		if metadata.ResponseBytes != nil {
			value = *metadata.ResponseBytes
		}
		value += n
		metadata.ResponseBytes = &value
	})
}

// SetResponseReadComplete records whether this attempt's response body was read to its
// end. A complete verdict is authoritative: the Close that follows an upstream which
// flushes its terminal event and then keeps the connection open is a normal end of
// stream, not an incomplete read, so it must not downgrade an already complete verdict.
func (a *Attempt) SetResponseReadComplete(complete bool) {
	if a == nil || a.counter == nil {
		return
	}
	a.counter.updateMetadata(a.index, func(metadata *Metadata) {
		if !complete && metadata.ResponseReadComplete != nil && *metadata.ResponseReadComplete {
			return
		}
		metadata.ResponseReadComplete = &complete
	})
}

// MarkLastResponseReadComplete records that the most recent recorded attempt was read to
// its protocol terminal, without retaining any body bytes. Callers use it when they stop
// reading a stream right after a successful terminal event instead of waiting for EOF.
// Attempts are sequential, so the newest entry is the one still open: earlier attempts
// keep their own verdict and are never retroactively completed. It is a no-op when the
// newest attempt has no recorded HTTP response — no attempt at all, a transport failure,
// or an attempt handled by a plugin, which records no transport metadata — because
// completing an earlier attempt's record instead would be a retroactive lie.
func (c *Counter) MarkLastResponseReadComplete() {
	if c == nil || c.lastPluginHandled.Load() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.metadata) == 0 {
		return
	}
	last := &c.metadata[len(c.metadata)-1]
	if last.StatusCode == nil {
		return
	}
	complete := true
	last.ResponseReadComplete = &complete
}

// SetLatencyMillis 记录本次尝试的 RoundTrip 耗时（毫秒）。
//
// 负值与非正数一律忽略：没有测量结果时保持 nil，而不是写一个 0 冒充「瞬间返回」。
func (a *Attempt) SetLatencyMillis(millis int64) {
	if a == nil || a.counter == nil || millis < 0 {
		return
	}
	a.counter.updateMetadata(a.index, func(metadata *Metadata) {
		value := millis
		metadata.LatencyMillis = &value
	})
}

// SetProxyID 记录本次尝试实际使用的代理内部 ID；0 表示未使用代理。
func (a *Attempt) SetProxyID(proxyID int64) {
	if a == nil || a.counter == nil || proxyID <= 0 {
		return
	}
	a.counter.updateMetadata(a.index, func(metadata *Metadata) {
		metadata.ProxyID = proxyID
	})
}

func metadataFromContext(ctx context.Context) Metadata {
	if metadata, ok := MetadataFromContext(ctx); ok {
		return metadata
	}
	return Metadata{}
}

func (c *Counter) incrementMetadata(metadata Metadata) int {
	c.lastPluginHandled.Store(false)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metadata = append(c.metadata, cloneMetadata(metadata))
	c.n.Add(1)
	return len(c.metadata) - 1
}

func (c *Counter) updateMetadata(index int, update func(*Metadata)) {
	if c == nil || update == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if index < 0 || index >= len(c.metadata) {
		return
	}
	update(&c.metadata[index])
}

func IsForced(ctx context.Context) bool {
	counter, ok := FromContext(ctx)
	if !ok {
		return false
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	return counter.forced
}

func (c *Counter) SetBeforeAttempt(fn BeforeAttemptFunc, forced bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.beforeAttempt = fn
	c.forced = forced
	c.mu.Unlock()
}

func Before(ctx context.Context) error {
	return before(ctx, metadataFromContext(ctx))
}

// BeforeRequest invokes the pre-send gate with safe headers from the final wire request.
func BeforeRequest(req *http.Request) error {
	if req == nil {
		return nil
	}
	metadata := metadataFromContext(req.Context())
	metadata.RequestHeaders = SanitizeRequestHeaders(req.Header)
	return before(req.Context(), metadata)
}

func before(ctx context.Context, metadata Metadata) error {
	counter, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	counter.mu.Lock()
	fn := counter.beforeAttempt
	forced := counter.forced
	counter.mu.Unlock()
	if fn == nil {
		if forced {
			return &RequiredAuditError{}
		}
		return nil
	}
	if err := fn(ctx, cloneMetadata(metadata)); err != nil && forced {
		return &RequiredAuditError{Cause: err}
	}
	return nil
}

func MarkPluginHandled(ctx context.Context) {
	if counter, ok := FromContext(ctx); ok {
		counter.lastPluginHandled.Store(true)
	}
}

func (c *Counter) LastPluginHandled() bool {
	return c != nil && c.lastPluginHandled.Load()
}

func (c *Counter) Load() uint64 {
	if c == nil {
		return 0
	}
	return c.n.Load()
}

func (c *Counter) Metadata() []Metadata {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Metadata, len(c.metadata))
	for i, metadata := range c.metadata {
		out[i] = cloneMetadata(metadata)
	}
	return out
}
