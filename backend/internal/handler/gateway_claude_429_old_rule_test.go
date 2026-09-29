//go:build unit

package handler

// Ticket 01 验收标准 6：把「新冷却误拦」与「上游账号级 429 回避」分开归因。
//
// 两个用例在同一个真实 Messages 入口（最高外部行为接缝）上使用同一份
// 「A 触顶 → B 同 Key 不同身份」合成输入，只切换账号级 429 回避的仓储：
//
//   - 旧规则被置空（gateway429NoCooldownRepo）时，账号池仍然可用，B 照旧重试 [1,2]
//     且不命中本地冷却 —— 证明新冷却不会把共享 Key 的 B 误拦；
//   - 旧规则生效时，SetRateLimited 把真实共享的调度账号指针标为 RateLimitResetAt，
//     B 依然不命中本地冷却，但选号只能落到未被标记的账号 3 并成功 —— 证明 B 遇到的
//     「账号 1/2 不可用」来自旧规则，而不是新冷却。
//
// 断言只使用外部可观测量：上游被调用的账号序列、HTTP 状态码、以及本地冷却特有的
// Retry-After 头；不对不可观测的存储内部状态做假设。

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// claude429OldRuleRepo 是上游账号级 429 回避的仓储替身：SetRateLimited 会写入
// 调度快照共享的同一批真实账号指针，使下一次选号（fakeSchedulerCache 快照在
// derefAccounts 时按值复制指针当前内容）能观察到 RateLimitResetAt 并跳过该账号。
// 这是旧规则的既有语义，与被测的新冷却无关。
type claude429OldRuleRepo struct {
	openAIImagesFailoverAccountRepo
	mu     sync.Mutex
	shared []*service.Account
	marked []int64
}

func (r *claude429OldRuleRepo) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.marked = append(r.marked, id)
	for _, account := range r.shared {
		if account == nil || account.ID != id {
			continue
		}
		limitedAt := time.Now()
		reset := resetAt
		account.RateLimitedAt = &limitedAt
		account.RateLimitResetAt = &reset
	}
	return nil
}

// markedAccountIDs 返回旧规则实际标记过的账号，用于把「B 选不到账号」归因到旧规则。
func (r *claude429OldRuleRepo) markedAccountIDs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.marked...)
}

// claude429AccountValueCopies 复制调度账号的按值版本，供仓储的 GetByID/列表方法使用；
// 指针版本仍留给调度快照与被测的旧规则写入。
func claude429AccountValueCopies(shared []*service.Account) []service.Account {
	out := make([]service.Account, 0, len(shared))
	for _, account := range shared {
		if account != nil {
			out = append(out, *account)
		}
	}
	return out
}

// newClaude429OldRuleHandler 组装最小可用的 Anthropic Messages handler。
// 与 newGateway429TestHandler 相同，但由调用方注入账号级 429 回避的仓储，
// 并让调度快照持有调用方的账号指针，以便区分两种规则。
func newClaude429OldRuleHandler(
	t *testing.T,
	upstream service.HTTPUpstream,
	accountRepo service.AccountRepository,
	shared []*service.Account,
	group *service.Group,
) *GatewayHandler {
	t.Helper()
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: shared}, nil, nil, nil, nil)
	rateLimit := service.NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	gw := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		&config.Config{}, snapshot, nil, nil, rateLimit, nil, nil, upstream,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	return &GatewayHandler{gatewayService: gw, billingCacheService: billing, concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0), maxAccountSwitches: 10, cfg: cfg}
}

// enableClaude429LocalCooldown 打开跨请求冷却（会话级）并保留原 N=limit，
// 使新冷却在两个用例中都处于可命中的开启状态。
func enableClaude429LocalCooldown(t *testing.T, h *GatewayHandler, limit int) {
	t.Helper()
	settings := newOpenAI429MatrixSettingRepo(limit)
	require.NoError(t, settings.Set(context.Background(), service.SettingKeyRateLimit429AccountLimitCooldown,
		`{"enabled":true,"scope":"`+service.RateLimit429CooldownScopeSession+`","cooldown_seconds":60}`))
	h.settingService = service.NewSettingService(settings, h.cfg)
	h.claude429Cooldown = service.NewClaude429CooldownGate(h.settingService, newClaude429TestStore())
}

// TestGatewayClaude429OldRuleDisabledBStillRetriesAccountPool 覆盖验收标准 6 的前半段：
// 旧的上游账号级回避被置空时，A 触顶后 B（同 B1 Key、不同入站身份）不命中本地冷却，
// 也仍然能使用账号池 —— 新冷却没有把 B 误拦。
func TestGatewayClaude429OldRuleDisabledBStillRetriesAccountPool(t *testing.T) {
	group := &service.Group{ID: 9041, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	shared := gateway429TestAccounts(group.ID, 3)
	upstream := &gateway429AccountLimitUpstream{}
	repo := gateway429NoCooldownRepo{openAIImagesFailoverAccountRepo{accounts: claude429AccountValueCopies(shared)}}
	h := newClaude429OldRuleHandler(t, upstream, repo, shared, group)
	enableClaude429LocalCooldown(t, h, 2)

	// A 第一次：账号 1、2 先后最终 429，达到原 N=2 后停止换号。
	c1, rec1 := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "turn-1")
	h.Messages(c1)
	require.Equal(t, http.StatusTooManyRequests, rec1.Code)
	require.Equal(t, []int64{1, 2}, upstream.hits)
	require.Empty(t, rec1.Header().Get("Retry-After"), "触顶终止本身不是本地冷却命中")

	// A 换 request ID：同一身份命中本地冷却，零上游尝试。
	c2, rec2 := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "turn-2")
	h.Messages(c2)
	require.Equal(t, http.StatusTooManyRequests, rec2.Code)
	require.Equal(t, []int64{1, 2}, upstream.hits, "同一身份的新 request ID 不应产生新的上游尝试")
	require.NotEmpty(t, rec2.Header().Get("Retry-After"), "A 应收到本地冷却的剩余秒数")

	// B：同 B1 Key、不同设备/会话。既不命中本地冷却，也因旧规则被置空而未被账号池排除，
	// 于是照旧重试 [1,2]。
	c3, rec3 := claude429TestContext(t, group, 9008, claude429DeviceB, claude429SessionB, claude429SessionB, "turn-3")
	h.Messages(c3)
	require.Equal(t, http.StatusTooManyRequests, rec3.Code)
	require.Empty(t, rec3.Header().Get("Retry-After"), "B 未命中本地冷却")
	require.Equal(t, []int64{1, 2, 1, 2}, upstream.hits, "B 应继续按原规则尝试账号池")
}

// TestGatewayClaude429OldRuleEnabledBMissesAccountsMarkedRateLimited 覆盖验收标准 6 的后半段：
// 旧规则生效时，A 触顶会通过 SetRateLimited 把真实共享的调度账号标为限流；
// B 仍不命中本地冷却，但选号会跳过这些账号并在账号 3 上成功 ——
// 即 B 遇到的账号不可用可正确归因于旧规则。
func TestGatewayClaude429OldRuleEnabledBMissesAccountsMarkedRateLimited(t *testing.T) {
	group := &service.Group{ID: 9042, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	shared := gateway429TestAccounts(group.ID, 3)
	upstream := &gateway429ThenSuccessUpstream{successAccountID: 3}
	repo := &claude429OldRuleRepo{
		openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: claude429AccountValueCopies(shared)},
		shared:                          shared,
	}
	h := newClaude429OldRuleHandler(t, upstream, repo, shared, group)
	enableClaude429LocalCooldown(t, h, 2)

	// A 触顶：账号 1、2 各自最终 429，旧规则把这两个调度账号标记为限流。
	c1, rec1 := claude429TestContext(t, group, 9008, claude429DeviceA, claude429SessionA, claude429SessionA, "turn-1")
	h.Messages(c1)
	require.Equal(t, http.StatusTooManyRequests, rec1.Code)
	require.Equal(t, []int64{1, 2}, upstream.hits)
	require.ElementsMatch(t, []int64{1, 2}, repo.markedAccountIDs(), "旧规则只应标记本次触顶真正用掉的账号")

	// B：不同入站身份，不命中新冷却；账号 1/2 已被旧规则标为不可调度，
	// 因此选号直接落到账号 3 并成功。
	c2, rec2 := claude429TestContext(t, group, 9008, claude429DeviceB, claude429SessionB, claude429SessionB, "turn-2")
	h.Messages(c2)
	require.Equal(t, http.StatusOK, rec2.Code, "B 应成功，而不是被任何冷却拦截")
	require.Equal(t, []int64{1, 2, 3}, upstream.hits, "B 只能选到未被旧规则标记的账号 3")
	require.Empty(t, rec2.Header().Get("Retry-After"), "B 未命中本地冷却")
	require.Contains(t, rec2.Body.String(), "msg_429_limit", "应把账号 3 的上游成功响应回给客户端")
}
