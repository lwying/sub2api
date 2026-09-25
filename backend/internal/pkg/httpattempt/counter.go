package httpattempt

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
)

type counterContextKey struct{}
type metadataContextKey struct{}
type claudeHeaderValueCaptureContextKey struct{}

// WithClaudeHeaderValueCapture 标记本次逻辑请求允许复制 Claude 头值与 metadata.user_id
// 的**值**快照（默认关闭的短期诊断旁路，见 ADR 0006）。
//
// 只有显式打上这个标记，传输层才会在最终 wire 请求与上游响应上构建值快照；
// 未打标记时快照字段保持 nil，热路径上不会读取或复制任何明文值。
// 标记是**请求级**的：它只表示「这次请求可以采」，不表示「这次请求一定不留明文」——
// 落库一律由 service 层加密完成，且服务层会再做一次独立的门控判定。
func WithClaudeHeaderValueCapture(ctx context.Context, enabled bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, claudeHeaderValueCaptureContextKey{}, enabled)
}

// ClaudeHeaderValueCaptureEnabled 报告本次请求是否允许复制值快照。
//
// 没有标记（含 nil 上下文）一律按关闭处理：默认关闭是 fail-closed 的默认值。
func ClaudeHeaderValueCaptureEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(claudeHeaderValueCaptureContextKey{}).(bool)
	return enabled
}

// WithClaudeMetadataUserID 在允许值快照时记录本次尝试实际发往上游的 metadata.user_id。
//
// 未打开标记时是**空操作**：调用方即使忘记先判断标记，也不会把明文写进尝试元数据。
// 记录的是重写之后真正发出去的值，而不是客户端原始值。
func WithClaudeMetadataUserID(ctx context.Context, userID string) context.Context {
	if !ClaudeHeaderValueCaptureEnabled(ctx) {
		return ctx
	}
	metadata, _ := MetadataFromContext(ctx)
	metadata.MetadataUserID = userID
	return WithMetadata(ctx, metadata)
}

// Metadata describes one upstream HTTP transport attempt without request or response bodies.
//
// RequestHeaders / ResponseHeaders 是**长期审计**用的存在性与闭集摘要，永远只有受限信息。
// RequestHeaderValues / ResponseHeaderValues / MetadataUserID 是**默认关闭**的短期值快照
// （Claude /v1/messages 的值明细旁路）：它们只在调用方显式打开时才被填充，
// 未打开时保持 nil，因此关闭态下热路径不会读取或复制任何明文值。
// RequestHeaderValueOmission / ResponseHeaderValueOmission 是与它们**同源同刻**的省略摘要
// （只含计数），关闭态下同样保持零值。
// 值快照只在内存中短暂存在，落库由 service 层加密完成——传输层从不写库。
type Metadata struct {
	AccountID            int64
	Model                string
	Protocol             string
	RequestHeaders       map[string]any
	ResponseHeaders      map[string]any
	StatusCode           *int
	RequestBytes         *int64
	ResponseBytes        *int64
	ResponseReadComplete *bool

	// RequestHeaderValues 是本次尝试最终发出的请求头值快照（净化器输出，默认 nil）。
	RequestHeaderValues map[string]any
	// ResponseHeaderValues 是本次尝试收到的响应头值快照（净化器输出，默认 nil）。
	ResponseHeaderValues map[string]any
	// RequestHeaderValueOmission / ResponseHeaderValueOmission 是上面两份快照的**省略摘要**：
	// 净化器在采集那一刻丢掉了多少个被观察到的头名／取值。它**只含计数**，不含名字与取值，
	// 因此可以在不记录任何未知头名（可能承载认证通道）的前提下，让服务层如实标出
	// 「客户端只发了这些」与「我们只收了这些」的差别。关闭态下保持零值。
	RequestHeaderValueOmission  ClaudeHeaderValueOmission
	ResponseHeaderValueOmission ClaudeHeaderValueOmission
	// MetadataUserID 是本次尝试实际发往上游的 metadata.user_id 原始字符串（默认空）。
	MetadataUserID string
	// LatencyMillis 是本次尝试的 RoundTrip 耗时（毫秒）；未测量时为 nil。
	LatencyMillis *int64
	// ProxyID 是本次尝试实际使用的代理内部 ID；未使用代理时为 0。它只在服务层
	// 的加密值明细里落库，传输层与长期审计都不写它。
	ProxyID int64

	// captureHeaderValues 是本次尝试的值快照开关：由请求上下文标记在尝试开始时**固化**，
	// 因此响应阶段（SetResponse 拿不到上下文）也按同一结论处理，不会中途改变判据。
	captureHeaderValues bool
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
	metadata.RequestHeaderValues = cloneHeaderMap(metadata.RequestHeaderValues)
	metadata.ResponseHeaderValues = cloneHeaderMap(metadata.ResponseHeaderValues)
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
// 值快照（RequestHeaderValues）只在请求上下文显式打开 WithClaudeHeaderValueCapture 时才构建：
// 默认关闭的部署在热路径上不会读取或复制任何明文头值。省略摘要（RequestHeaderValueOmission）
// 必须与快照同源同刻：净化器丢掉的未知头名与没通过校验的取值在这里之后就不复存在。
func StartRequestAttempt(req *http.Request) *Attempt {
	if req == nil {
		return nil
	}
	metadata := metadataFromContext(req.Context())
	metadata.RequestHeaders = SanitizeRequestHeaders(req.Header)
	if ClaudeHeaderValueCaptureEnabled(req.Context()) {
		metadata.RequestHeaderValues, metadata.RequestHeaderValueOmission = SanitizeClaudeRequestHeaderValuesWithOmission(req.Header)
	}
	return startAttempt(req.Context(), metadata)
}

func startAttempt(ctx context.Context, metadata Metadata) *Attempt {
	counter, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	zero := int64(0)
	metadata.RequestBytes = &zero
	// 把请求级的开关固化到这次尝试上：响应阶段不再有上下文可查。
	metadata.captureHeaderValues = ClaudeHeaderValueCaptureEnabled(ctx)
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
//
// 值快照（ResponseHeaderValues）只在本次尝试开始时已打开值快照开关时才构建；
// 省略摘要与快照同源同刻，理由见 StartRequestAttempt。
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
		if metadata.captureHeaderValues {
			metadata.ResponseHeaderValues, metadata.ResponseHeaderValueOmission = SanitizeClaudeResponseHeaderValuesWithOmission(headers)
		}
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
