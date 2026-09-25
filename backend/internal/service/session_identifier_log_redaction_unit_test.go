//go:build unit

package service

// 会话标识的日志 sentinel 测试。
//
// 契约：sessionHash 是粘性路由键，取值可能直接来自客户端 metadata.user_id 的会话段
// （GenerateSessionHash 第 1 级），shortSessionHash 只是前 8 字符截断、长度不足时整串
// 返回，不是摘要。两者都是会话标识，一律不得进入普通日志；metadata.user_id 的
// device_id / session_id 段同理（发往上游的请求身份）。
//
// 每个用例注入合成哨兵值并断言它既不以原值、也不以 8 字符截断值出现在日志里，同时
// 断言选择与缓存行为未变：缓存键必须仍是完整原值（脱敏只作用于日志，不作用于键）。
// 期望值全部写在测试内；唯一例外是被生成值（随机 client_id / 伪装 session ID）——那种
// 断言要证明的正是「刚生成的值不得出现在日志」，只能取函数产物比对。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
)

// 合成哨兵：不来自任何真实请求、会话或账号。
const (
	// sentinelSessionHash 模拟 metadata.user_id 里提取出的会话标识；
	// 前 8 字符 "c4n4ry52" 是本测试专门挑选的可检索前缀。
	sentinelSessionHash = "c4n4ry52-9f31-4a7e-8b6d-0e5a1b2c3d4e"
	// sentinelShortSessionHash 长度恰为 8：修复前 shortSessionHash 会整串返回该值。
	sentinelShortSessionHash = "c4n4ry77"
	// metadata.user_id 的组成段。
	sentinelDeviceID      = "c4n4ry00device0000000000000000000000000000000000000000000000000000"
	sentinelUserSessionID = "c4n4ry11-9f31-4a7e-8b6d-0e5a1b2c3d4e"
	// sentinelMaskedSessionID 模拟会话ID伪装功能注入的固定 session UUID。
	sentinelMaskedSessionID = "c4n4ry33-1111-4222-8333-444455556666"
)

// sentinelMetadataUserID 是含哨兵段的 JSON 格式 metadata.user_id。
const sentinelMetadataUserID = `{"device_id":"` + sentinelDeviceID + `","account_uuid":"","session_id":"` + sentinelUserSessionID + `"}`

var sessionLogCaptureMu sync.Mutex

type sessionLogCapture struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (c *sessionLogCapture) WriteLogEvent(event *logger.LogEvent) {
	if event == nil {
		return
	}
	cloned := *event
	if event.Fields != nil {
		cloned.Fields = make(map[string]any, len(event.Fields))
		for k, v := range event.Fields {
			cloned.Fields[k] = v
		}
	}
	c.mu.Lock()
	c.events = append(c.events, &cloned)
	c.mu.Unlock()
}

func (c *sessionLogCapture) snapshot() []*logger.LogEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*logger.LogEvent(nil), c.events...)
}

func (c *sessionLogCapture) containsMessage(substr string) bool {
	for _, event := range c.snapshot() {
		if strings.Contains(event.Message, substr) {
			return true
		}
	}
	return false
}

// containsAnywhere 同时扫描消息与结构化字段：slog 的字段值不进入 Message，
// 只断言 Message 会漏掉结构化日志里的会话标识。
func (c *sessionLogCapture) containsAnywhere(substr string) bool {
	if substr == "" {
		return false
	}
	for _, event := range c.snapshot() {
		if strings.Contains(event.Message, substr) {
			return true
		}
		for key, value := range event.Fields {
			if strings.Contains(key, substr) || strings.Contains(fmt.Sprint(value), substr) {
				return true
			}
		}
	}
	return false
}

func (c *sessionLogCapture) fieldOf(message, key string) (any, bool) {
	for _, event := range c.snapshot() {
		if event.Message != message {
			continue
		}
		if value, ok := event.Fields[key]; ok {
			return value, true
		}
	}
	return nil, false
}

func (c *sessionLogCapture) describe() string {
	var b strings.Builder
	for _, event := range c.snapshot() {
		b.WriteString(event.Level)
		b.WriteString(" ")
		b.WriteString(event.Message)
		for key, value := range event.Fields {
			b.WriteString(" ")
			b.WriteString(key)
			b.WriteString("=")
			b.WriteString(fmt.Sprint(value))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// captureSessionLogs 复用 logger sink 接缝捕获结构化日志事件（与既有 sentinel 测试同一机制）。
func captureSessionLogs(t *testing.T) *sessionLogCapture {
	t.Helper()
	sessionLogCaptureMu.Lock()
	t.Cleanup(func() {
		logger.SetSink(nil)
		sessionLogCaptureMu.Unlock()
	})
	require.NoError(t, logger.Init(logger.InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output: logger.OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
		Sampling: logger.SamplingOptions{Enabled: false},
	}))
	capture := &sessionLogCapture{}
	logger.SetSink(capture)
	return capture
}

// sessionHashRecordingCache 记录粘性会话缓存收到的 sessionHash 原值，用于证明
// 日志脱敏没有改变缓存键（键必须仍是完整原值）。
type sessionHashRecordingCache struct {
	stickyGatewayCacheHotpathStub

	mu            sync.Mutex
	sessionHashes []string
	setErr        error
	deleteErr     error
}

func (c *sessionHashRecordingCache) record(sessionHash string) {
	c.mu.Lock()
	c.sessionHashes = append(c.sessionHashes, sessionHash)
	c.mu.Unlock()
}

func (c *sessionHashRecordingCache) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sessionHashes...)
}

func (c *sessionHashRecordingCache) GetSessionAccountID(_ context.Context, _ int64, sessionHash string) (int64, error) {
	c.record(sessionHash)
	if c.stickyID > 0 {
		return c.stickyID, nil
	}
	return 0, ErrStickySessionNotFound
}

func (c *sessionHashRecordingCache) SetSessionAccountID(_ context.Context, _ int64, sessionHash string, _ int64, _ time.Duration) error {
	c.record(sessionHash)
	return c.setErr
}

func (c *sessionHashRecordingCache) RefreshSessionTTL(_ context.Context, _ int64, sessionHash string, _ time.Duration) error {
	c.record(sessionHash)
	return nil
}

func (c *sessionHashRecordingCache) DeleteSessionAccountID(_ context.Context, _ int64, sessionHash string) error {
	c.record(sessionHash)
	return c.deleteErr
}

func sessionLogSchedulingFixture() (*Account, stubOpenAIAccountRepo, *ConcurrencyService, *config.Config, context.Context) {
	now := time.Now().Add(-time.Minute)
	account := &Account{
		ID:          88,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 4,
		Priority:    1,
		LastUsedAt:  &now,
	}
	repo := stubOpenAIAccountRepo{accounts: []Account{*account}}
	concurrency := NewConcurrencyService(stubConcurrencyCache{})
	cfg := &config.Config{
		RunMode: config.RunModeStandard,
		Gateway: config.GatewayConfig{
			Scheduling: config.GatewaySchedulingConfig{
				LoadBatchEnabled:         true,
				StickySessionMaxWaiting:  3,
				StickySessionWaitTimeout: time.Second,
				FallbackWaitTimeout:      time.Second,
				FallbackMaxWaiting:       10,
			},
		},
	}
	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformAnthropic)
	return account, repo, concurrency, cfg, ctx
}

// sessionLogState 只允许返回存在性标记，绝不回显入参。
func TestSessionLogState_NeverEchoesSessionValue(t *testing.T) {
	require.Equal(t, "absent", sessionLogState(""))
	for _, value := range []string{sentinelSessionHash, sentinelShortSessionHash} {
		got := sessionLogState(value)
		require.Equal(t, "present", got)
		require.NotContains(t, got, value)
	}
}

// 负载感知调度路径（粘性命中、粘性未命中回退负载均衡）的日志都不得出现会话标识，
// 且缓存键必须仍是完整原值。
func TestSelectAccountWithLoadAwareness_LogsCarryNoSessionValue(t *testing.T) {
	cases := []struct {
		name        string
		sessionHash string
		stickyID    int64
	}{
		{name: "full_hash_sticky_hit", sessionHash: sentinelSessionHash, stickyID: 88},
		{name: "full_hash_sticky_miss", sessionHash: sentinelSessionHash, stickyID: 0},
		{name: "short_hash_sticky_hit", sessionHash: sentinelShortSessionHash, stickyID: 88},
		{name: "short_hash_sticky_miss", sessionHash: sentinelShortSessionHash, stickyID: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := captureSessionLogs(t)
			account, repo, concurrency, cfg, ctx := sessionLogSchedulingFixture()

			cache := &sessionHashRecordingCache{}
			cache.stickyID = tc.stickyID
			svc := &GatewayService{
				accountRepo:        repo,
				cache:              cache,
				cfg:                cfg,
				concurrencyService: concurrency,
				userGroupRateCache: gocache.New(time.Minute, time.Minute),
				modelsListCache:    gocache.New(time.Minute, time.Minute),
				modelsListCacheTTL: time.Minute,
			}

			result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, tc.sessionHash, "", nil, "", int64(0))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, result.Account)
			require.Equal(t, account.ID, result.Account.ID, "选择行为必须保持不变")

			// 缓存键必须仍是完整原值：脱敏只作用于日志。
			require.Contains(t, cache.recorded(), tc.sessionHash,
				"缓存读写必须继续使用完整 sessionHash，日志脱敏不得改变键")

			require.True(t, capture.containsMessage("sticky.scheduler_entry"),
				"调度入口日志必须保留，否则丢失排查能力: %s", capture.describe())
			require.False(t, capture.containsAnywhere(tc.sessionHash),
				"sessionHash 原值不得进入日志: %s", capture.describe())
			require.False(t, capture.containsAnywhere(tc.sessionHash[:8]),
				"sessionHash 的 8 字符截断值（shortSessionHash 的取值）不得进入日志: %s", capture.describe())

			value, ok := capture.fieldOf("sticky.scheduler_entry", "session")
			require.True(t, ok, "scheduler_entry 必须保留非敏感的会话存在性字段: %s", capture.describe())
			require.Equal(t, "present", value)
		})
	}
}

// 粘性绑定失败的日志路径容易顺手带上 sessionHash；失败原因与账号 ID 要保留，
// 会话标识不得出现。
func TestSelectAccountForModelWithPlatform_BindFailureLogCarriesNoSessionValue(t *testing.T) {
	capture := captureSessionLogs(t)
	// 必须带 ForcePlatform：否则混合调度分支会去查 antigravity 平台账号列表。
	account, repo, concurrency, cfg, ctx := sessionLogSchedulingFixture()

	cache := &sessionHashRecordingCache{setErr: errors.New("sticky cache unavailable")}
	svc := &GatewayService{
		accountRepo:        repo,
		cache:              cache,
		cfg:                cfg,
		concurrencyService: concurrency,
		userGroupRateCache: gocache.New(time.Minute, time.Minute),
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
	}

	selected, err := svc.selectAccountForModelWithPlatform(ctx, nil, sentinelSessionHash, "", nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.Equal(t, account.ID, selected.ID, "绑定失败不得影响选择结果")

	require.Contains(t, cache.recorded(), sentinelSessionHash,
		"绑定仍必须尝试完整 sessionHash")

	require.True(t, capture.containsMessage("set session account failed"),
		"绑定失败必须仍然记录: %s", capture.describe())
	require.True(t, capture.containsMessage("account_id=88"),
		"账号 ID 必须保留: %s", capture.describe())
	require.True(t, capture.containsMessage("session=present"),
		"只允许保留会话存在性: %s", capture.describe())
	require.True(t, capture.containsMessage("sticky cache unavailable"),
		"失败原因必须保留: %s", capture.describe())
	require.False(t, capture.containsAnywhere(sentinelSessionHash),
		"sessionHash 原值不得进入日志: %s", capture.describe())
	require.False(t, capture.containsAnywhere(sentinelSessionHash[:8]),
		"sessionHash 的 8 字符截断值不得进入日志: %s", capture.describe())
}

// antigravity 清除粘性会话失败路径：group_id 与 err 保留，会话标识不得出现。
func TestAntigravityClearStickySession_LogsNoSessionValue(t *testing.T) {
	capture := captureSessionLogs(t)

	cache := &sessionHashRecordingCache{deleteErr: errors.New("redis unavailable")}
	svc := &AntigravityGatewayService{cache: cache}

	svc.clearStickySession(context.Background(), 7, sentinelSessionHash)

	require.Contains(t, cache.recorded(), sentinelSessionHash,
		"删除粘性会话仍必须使用完整 sessionHash")
	require.True(t, capture.containsMessage("sticky_session_clear_failed"),
		"清除失败必须仍然记录: %s", capture.describe())
	require.True(t, capture.containsMessage("group_id=7"),
		"分组 ID 必须保留: %s", capture.describe())
	require.True(t, capture.containsMessage("redis unavailable"),
		"失败原因必须保留: %s", capture.describe())
	require.False(t, capture.containsAnywhere(sentinelSessionHash),
		"sessionHash 原值不得进入日志: %s", capture.describe())
	require.False(t, capture.containsAnywhere(sentinelSessionHash[:8]),
		"sessionHash 的 8 字符截断值不得进入日志: %s", capture.describe())
}

// sessionLogIdentityCacheStub 记录伪装会话 ID 的读写，用于证明标识只进缓存不进日志。
type sessionLogIdentityCacheStub struct {
	maskedSessionID string
	fingerprint     *Fingerprint
	generatedUUIDs  []string
}

func (s *sessionLogIdentityCacheStub) GetFingerprint(_ context.Context, _ int64) (*Fingerprint, error) {
	return s.fingerprint, nil
}

func (s *sessionLogIdentityCacheStub) SetFingerprint(_ context.Context, _ int64, fp *Fingerprint) error {
	s.fingerprint = fp
	return nil
}

func (s *sessionLogIdentityCacheStub) GetMaskedSessionID(_ context.Context, _ int64) (string, error) {
	return s.maskedSessionID, nil
}

func (s *sessionLogIdentityCacheStub) SetMaskedSessionID(_ context.Context, _ int64, sessionID string) error {
	if s.maskedSessionID != sessionID {
		s.generatedUUIDs = append(s.generatedUUIDs, sessionID)
	}
	s.maskedSessionID = sessionID
	return nil
}

func sentinelMaskingAccount() *Account {
	return &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"session_id_masking_enabled": true},
	}
}

func sentinelMaskingBody() []byte {
	return []byte(`{"model":"claude-sonnet-4-6","messages":[],"metadata":{"user_id":` +
		jsonQuote(sentinelMetadataUserID) + `}}`)
}

func jsonQuote(raw string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(raw) + `"`
}

// 会话ID伪装：注入的 user_id 会发往上游，原值与注入值（device_id / session_id）都不得进日志。
func TestIdentityService_SessionIDMaskingLogsNoUserIDValues(t *testing.T) {
	ctx := context.Background()
	account := sentinelMaskingAccount()
	body := sentinelMaskingBody()

	t.Run("reused_masked_session", func(t *testing.T) {
		capture := captureSessionLogs(t)
		cache := &sessionLogIdentityCacheStub{maskedSessionID: sentinelMaskedSessionID}
		svc := NewIdentityService(cache)

		result, err := svc.RewriteUserIDWithMasking(ctx, body, account, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
		require.NoError(t, err)

		// 行为不变：伪装 session 仍写进 body。
		require.Contains(t, string(result), sentinelMaskedSessionID)
		require.NotContains(t, string(result), sentinelUserSessionID)
		require.Equal(t, sentinelMaskedSessionID, cache.maskedSessionID)
		require.Empty(t, cache.generatedUUIDs, "缓存命中时不得重新生成伪装会话")

		require.True(t, capture.containsMessage("session_id_masking_applied"),
			"伪装日志必须保留，否则丢失排查能力: %s", capture.describe())
		accountID, ok := capture.fieldOf("session_id_masking_applied", "account_id")
		require.True(t, ok)
		require.Equal(t, int64(123), accountID)
		reused, ok := capture.fieldOf("session_id_masking_applied", "masked_session_reused")
		require.True(t, ok)
		require.Equal(t, true, reused)
		changed, ok := capture.fieldOf("session_id_masking_applied", "metadata_user_id_changed")
		require.True(t, ok)
		require.Equal(t, true, changed)

		require.False(t, capture.containsAnywhere(sentinelMetadataUserID),
			"metadata.user_id 原文不得进入日志: %s", capture.describe())
		require.False(t, capture.containsAnywhere(sentinelDeviceID),
			"metadata.user_id 的 device_id 段不得进入日志: %s", capture.describe())
		require.False(t, capture.containsAnywhere(sentinelUserSessionID),
			"客户端会话标识不得进入日志: %s", capture.describe())
		require.False(t, capture.containsAnywhere(sentinelMaskedSessionID),
			"注入的伪装会话标识不得进入日志: %s", capture.describe())
	})

	t.Run("generated_masked_session", func(t *testing.T) {
		capture := captureSessionLogs(t)
		cache := &sessionLogIdentityCacheStub{}
		svc := NewIdentityService(cache)

		result, err := svc.RewriteUserIDWithMasking(ctx, body, account, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
		require.NoError(t, err)

		require.Len(t, cache.generatedUUIDs, 1, "缓存未命中时必须生成新的伪装会话")
		generated := cache.generatedUUIDs[0]
		require.Contains(t, string(result), generated, "新生成的伪装会话必须写进 body")
		require.Equal(t, generated, cache.maskedSessionID)

		require.True(t, capture.containsMessage("Generated new masked session ID for account 123"),
			"生成事件必须保留且带账号 ID: %s", capture.describe())
		require.False(t, capture.containsAnywhere(generated),
			"新生成的伪装会话标识不得进入日志: %s", capture.describe())
		require.False(t, capture.containsAnywhere(sentinelDeviceID),
			"metadata.user_id 的 device_id 段不得进入日志: %s", capture.describe())
		require.False(t, capture.containsAnywhere(sentinelUserSessionID),
			"客户端会话标识不得进入日志: %s", capture.describe())
	})
}

// 账号指纹首次创建会生成 client_id（metadata.user_id 的 device_id 段），
// 该值不得进日志；账号 ID 必须保留。
func TestIdentityService_FingerprintCreationLogsNoClientID(t *testing.T) {
	capture := captureSessionLogs(t)
	cache := &sessionLogIdentityCacheStub{}
	svc := NewIdentityService(cache)

	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.190 (external, cli)")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 42, headers)
	require.NoError(t, err)
	require.NotNil(t, fp)
	require.NotEmpty(t, fp.ClientID)

	require.True(t, capture.containsMessage("Created new fingerprint for account 42"),
		"创建事件必须保留且带账号 ID: %s", capture.describe())
	require.False(t, capture.containsAnywhere(fp.ClientID),
		"生成的 client_id 不得进入日志: %s", capture.describe())
}

// 结构化日志路径（slog）与 printf 路径都必须走 sessionLogState。
func TestSessionLogStateUsedBySlogFields(t *testing.T) {
	capture := captureSessionLogs(t)
	_, repo, concurrency, cfg, ctx := sessionLogSchedulingFixture()

	cache := &sessionHashRecordingCache{}
	svc := &GatewayService{
		accountRepo:        repo,
		cache:              cache,
		cfg:                cfg,
		concurrencyService: concurrency,
		userGroupRateCache: gocache.New(time.Minute, time.Minute),
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
	}

	_, err := svc.SelectAccountWithLoadAwareness(ctx, nil, sentinelSessionHash, "", nil, "", int64(0))
	require.NoError(t, err)

	// 记一条信息级事件，确认字段值确实进了 capture（否则上面的断言会假通过）。
	slog.Info("session_log_state_probe", "session", sessionLogState(sentinelSessionHash))
	require.False(t, capture.containsAnywhere(sentinelSessionHash),
		"包含字段值的日志事件都必须被扫描到: %s", capture.describe())
	require.True(t, capture.containsMessage("session_log_state_probe"),
		"探针事件必须被 capture 捕获: %s", capture.describe())
}
