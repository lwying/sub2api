package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Claude429CooldownStore 是「跨请求冷却」临时状态的存取接口。
// repository 层 gatewayCache 附带实现（调用方按可选接口断言探测接入，共享
// GatewayCache 接口刻意不新增方法）；存储缺失或失败时新增冷却整体跳过，
// 原有单请求 429 账号上限（N）继续生效。
type Claude429CooldownStore interface {
	// SetClaude429Cooldown 以单条原子 SET（同时携带 TTL）写入冷却键。
	SetClaude429Cooldown(ctx context.Context, key string, ttl time.Duration) error
	// Claude429CooldownTTL 返回剩余 TTL；键不存在或已无过期时间时返回 (0, nil)。
	Claude429CooldownTTL(ctx context.Context, key string) (time.Duration, error)
}

// Claude429CooldownStoreFromCache 从共享 GatewayCache 探测可选的冷却存储能力。
// cache 为 nil、其实现未提供该能力（含未来以装饰器包装的情况）时返回 nil，
// 门禁自动降级为不读不写。
func Claude429CooldownStoreFromCache(cache GatewayCache) Claude429CooldownStore {
	if cache == nil {
		return nil
	}
	store, ok := cache.(Claude429CooldownStore)
	if !ok {
		return nil
	}
	return store
}

// Claude429CooldownGate 是 N 触顶后可选的跨请求冷却门禁。
//
// 与上游账号级 429 回避（rate_limit_429_cooldown_settings）无关：它只按本请求
// 传入的入站身份判定，从不读取或改写上游账号调度状态。
// 用 NewClaude429CooldownGate 构造；零值/未注入时保持惰性（不读不写）。
// 所有方法在配置关闭、存储缺失、身份不完整或存储出错时一律 fail-open
// （不读不写、不拦截）。
type Claude429CooldownGate struct {
	settings *SettingService
	store    Claude429CooldownStore
}

// NewClaude429CooldownGate 构造门禁。store 可为 nil（例如 cache 未实现该可选
// 接口），此时只读配置、永不读写下发。
func NewClaude429CooldownGate(settings *SettingService, store Claude429CooldownStore) *Claude429CooldownGate {
	return &Claude429CooldownGate{settings: settings, store: store}
}

// Runtime 读取本次逻辑请求的运行期配置。开关默认关闭，读取失败同样返回默认
// （关闭、会话级、60 秒），因此调用方无需区分错误。handler 应在一次请求内只调用
// 一次，并把返回值传给 Remaining/Mark，避免热路径重复读设置。
func (g *Claude429CooldownGate) Runtime(ctx context.Context) RateLimit429AccountLimitCooldown {
	if g == nil || g.settings == nil || g.settings.settingRepo == nil {
		return DefaultRateLimit429AccountLimitCooldown()
	}
	rt, err := g.settings.GetRateLimit429AccountLimitCooldown(ctx)
	if err != nil {
		logger.FromContext(ctx).Warn("gateway.claude_429_cooldown_setting_unavailable", zap.String("error_class", claude429CooldownErrorClass(err)))
		return DefaultRateLimit429AccountLimitCooldown()
	}
	if !isValidRateLimit429CooldownScope(rt.Scope) {
		rt.Scope = defaultRateLimit429AccountLimitCooldownScope
	}
	if rt.CooldownSeconds < minRateLimit429AccountLimitCooldownSeconds || rt.CooldownSeconds > maxRateLimit429AccountLimitCooldownSeconds {
		rt.CooldownSeconds = defaultRateLimit429AccountLimitCooldownSeconds
	}
	return rt
}

// Remaining 返回该入站身份在当前粒度下的剩余冷却秒数（向上取整，至少 1）。
// 第二个返回值表示是否命中冷却；未命中、开关关闭、身份不完整或存储失败时返回
// (0, false)，调用方据此放行。
func (g *Claude429CooldownGate) Remaining(ctx context.Context, apiKeyID int64, rt RateLimit429AccountLimitCooldown, deviceID, sessionID string) (int, bool) {
	if g == nil || !rt.Enabled || g.store == nil {
		return 0, false
	}
	key := claude429CooldownKey(apiKeyID, rt, deviceID, sessionID)
	if key == "" {
		return 0, false
	}
	ttl, err := g.store.Claude429CooldownTTL(ctx, key)
	if err != nil {
		logger.FromContext(ctx).Warn("gateway.claude_429_cooldown_read_failed", zap.String("error_class", claude429CooldownErrorClass(err)))
		return 0, false
	}
	if ttl <= 0 {
		return 0, false
	}
	return claude429CooldownRemainingSeconds(ttl), true
}

// Mark 在 N 真正触顶后写入冷却，返回是否成功写入。关闭、身份不完整
// 或存储失败时返回 false；存储失败仅记脱敏错误分类，不影响原单请求处理。
func (g *Claude429CooldownGate) Mark(ctx context.Context, apiKeyID int64, rt RateLimit429AccountLimitCooldown, deviceID, sessionID string) bool {
	if g == nil || !rt.Enabled || g.store == nil {
		return false
	}
	key := claude429CooldownKey(apiKeyID, rt, deviceID, sessionID)
	if key == "" {
		return false
	}
	ttl := time.Duration(rt.CooldownSeconds) * time.Second
	if err := g.store.SetClaude429Cooldown(ctx, key, ttl); err != nil {
		logger.FromContext(ctx).Warn("gateway.claude_429_cooldown_write_failed", zap.String("error_class", claude429CooldownErrorClass(err)))
		return false
	}
	return true
}

// claude429CooldownKey 返回冷却键摘要；apiKeyID 非法、粒度非法、秒数越界或入站
// 身份缺失/无效时返回空串。两种粒度都要求完整的会话身份：缺失即跳过新增冷却，
// 绝不退化成仅 Key 级或仅设备的宽范围键。
func claude429CooldownKey(apiKeyID int64, rt RateLimit429AccountLimitCooldown, deviceID, sessionID string) string {
	if apiKeyID <= 0 || !isValidRateLimit429CooldownScope(rt.Scope) {
		return ""
	}
	if rt.CooldownSeconds < minRateLimit429AccountLimitCooldownSeconds || rt.CooldownSeconds > maxRateLimit429AccountLimitCooldownSeconds {
		return ""
	}
	deviceID, ok := claude429CooldownIdentityToken(deviceID)
	if !ok {
		return ""
	}
	sessionID, ok = claude429CooldownIdentityToken(sessionID)
	if !ok {
		return ""
	}
	return claude429CooldownDigest(apiKeyID, rt.Scope, deviceID, sessionID)
}

// claude429CooldownIdentityToken 校验单个原始入站标识，返回是否可用。
// 刻意不做任何归一化（TrimSpace 之类会让两个不同的原始标识映射到同一个键，
// 等于把 A 的冷却施加到别的客户端）：首尾空白或含控制字符时按"身份无效"跳过
// 新增冷却，而不是把它改写成一个可能与他人冲突的键。
func claude429CooldownIdentityToken(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", false
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return raw, true
}

// claude429CooldownDigest 计算带域分隔的不可反查摘要：经鉴权的 API Key ID 保证
// B1 不同 Key 之间互不污染，粒度进入域分隔串，因此切换粒度后旧键不再匹配
// （未被匹配的旧键自然过期）。设备/会话语义：会话级含会话标识，设备级不含。
//
// 每个字段以「名称=长度:值」编码而不是简单拼接加分隔符：设备与会话都是客户端自报的
// 任意字符串，仅靠 "device=...|session=..." 拼接时 (device="x", session="y|session=z")
// 与 (device="x|session=y", session="z") 会产生完全相同的原文，使两个不同身份共用
// 同一个冷却键。长度前缀让字段切分唯一，从而排除这类别名碰撞。
func claude429CooldownDigest(apiKeyID int64, scope, deviceID, sessionID string) string {
	var raw strings.Builder
	writeField := func(name, value string) {
		_, _ = raw.WriteString("\x00")
		_, _ = raw.WriteString(name)
		_, _ = raw.WriteString("=")
		_, _ = raw.WriteString(strconv.Itoa(len(value)))
		_, _ = raw.WriteString(":")
		_, _ = raw.WriteString(value)
	}
	_, _ = raw.WriteString("claude-429-cooldown:v1")
	writeField("scope", scope)
	writeField("api_key", strconv.FormatInt(apiKeyID, 10))
	writeField("device", deviceID)
	if scope == RateLimit429CooldownScopeSession {
		writeField("session", sessionID)
	}
	sum := sha256.Sum256([]byte(raw.String()))
	return hex.EncodeToString(sum[:])
}

func claude429CooldownRemainingSeconds(ttl time.Duration) int {
	seconds := int(math.Ceil(ttl.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// claude429CooldownErrorClass 只返回错误分类，供日志定位；不记录原始设备/会话
// 标识、摘要键或上游错误正文。
func claude429CooldownErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	default:
		return "store_error"
	}
}
