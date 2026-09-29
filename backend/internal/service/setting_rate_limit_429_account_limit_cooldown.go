package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// 跨请求冷却粒度。两者都先要求入站身份完整（metadata.user_id 可解析且会话头与正文一致），
// 区别只在冷却键是否包含会话标识：会话级（默认）允许 A 新开会话，设备级不允许。
const (
	// RateLimit429CooldownScopeSession keys the cooldown by inbound device + session identity.
	RateLimit429CooldownScopeSession = "session"
	// RateLimit429CooldownScopeDevice keys the cooldown by inbound device identity only.
	RateLimit429CooldownScopeDevice = "device"
)

// 这些默认值/边界属于本功能的跨请求部分，与上游账号级回避（defaultRateLimit429CooldownSeconds 等）无关。
const (
	defaultRateLimit429AccountLimitCooldownScope   = RateLimit429CooldownScopeSession
	defaultRateLimit429AccountLimitCooldownSeconds = 60
	minRateLimit429AccountLimitCooldownSeconds     = 1
	maxRateLimit429AccountLimitCooldownSeconds     = 7200
)

// RateLimit429AccountLimitCooldown 是「单次请求 429 账号上限」（N）同一功能的可选跨请求部分：
// 只在 N 真正触顶后写入，且与上游账号级 429 回避（rate_limit_429_cooldown_settings）无关。
type RateLimit429AccountLimitCooldown struct {
	Enabled         bool   `json:"enabled"`
	Scope           string `json:"scope"`
	CooldownSeconds int    `json:"cooldown_seconds"`
}

// DefaultRateLimit429AccountLimitCooldown 是首次升级的默认值：关闭、会话级、60 秒。
func DefaultRateLimit429AccountLimitCooldown() RateLimit429AccountLimitCooldown {
	return RateLimit429AccountLimitCooldown{
		Enabled:         false,
		Scope:           defaultRateLimit429AccountLimitCooldownScope,
		CooldownSeconds: defaultRateLimit429AccountLimitCooldownSeconds,
	}
}

// GetRateLimit429AccountLimitCooldown 读取跨请求冷却配置。键缺失、无法解析或单个字段越界时
// 按字段回落到默认值，不报错也不隐式写库（无 schema 迁移、无 backfill）。
func (s *SettingService) GetRateLimit429AccountLimitCooldown(ctx context.Context) (RateLimit429AccountLimitCooldown, error) {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyRateLimit429AccountLimitCooldown)
	if errors.Is(err, ErrSettingNotFound) {
		return DefaultRateLimit429AccountLimitCooldown(), nil
	}
	if err != nil {
		return RateLimit429AccountLimitCooldown{}, fmt.Errorf("get 429 account limit cooldown: %w", err)
	}
	return parseRateLimit429AccountLimitCooldown(value), nil
}

// SetRateLimit429AccountLimitCooldown 只写跨请求冷却键，保持 N 不变。
func (s *SettingService) SetRateLimit429AccountLimitCooldown(ctx context.Context, cooldown RateLimit429AccountLimitCooldown) error {
	raw, err := encodeRateLimit429AccountLimitCooldown(cooldown)
	if err != nil {
		return err
	}
	return s.settingRepo.SetMultiple(ctx, map[string]string{
		SettingKeyRateLimit429AccountLimitCooldown: raw,
	})
}

// SetRateLimit429AccountLimitWithCooldown 在同一个功能区域内保存 N 与跨请求冷却配置。
// cooldown 为 nil 表示调用方（旧客户端）只提交了 N：此时既不重建也不清空冷却键。
// 两者都提供时使用单次 SetMultiple 原子写入同一个设置区域。
func (s *SettingService) SetRateLimit429AccountLimitWithCooldown(ctx context.Context, limit int, cooldown *RateLimit429AccountLimitCooldown) error {
	if err := validateRateLimit429AccountLimit(limit); err != nil {
		return err
	}
	updates := map[string]string{SettingKeyRateLimit429AccountLimit: strconv.Itoa(limit)}
	if cooldown != nil {
		raw, err := encodeRateLimit429AccountLimitCooldown(*cooldown)
		if err != nil {
			return err
		}
		updates[SettingKeyRateLimit429AccountLimitCooldown] = raw
	}
	return s.settingRepo.SetMultiple(ctx, updates)
}

func encodeRateLimit429AccountLimitCooldown(cooldown RateLimit429AccountLimitCooldown) (string, error) {
	// 校验与开关状态无关：关闭时也拒绝非法粒度与 0/7201 等越界秒数。
	if !isValidRateLimit429CooldownScope(cooldown.Scope) {
		return "", fmt.Errorf("scope must be %s or %s", RateLimit429CooldownScopeSession, RateLimit429CooldownScopeDevice)
	}
	if cooldown.CooldownSeconds < minRateLimit429AccountLimitCooldownSeconds || cooldown.CooldownSeconds > maxRateLimit429AccountLimitCooldownSeconds {
		return "", fmt.Errorf("cooldown_seconds must be between %d-%d", minRateLimit429AccountLimitCooldownSeconds, maxRateLimit429AccountLimitCooldownSeconds)
	}
	raw, err := json.Marshal(cooldown)
	if err != nil {
		return "", fmt.Errorf("encode 429 account limit cooldown: %w", err)
	}
	return string(raw), nil
}

func parseRateLimit429AccountLimitCooldown(value string) RateLimit429AccountLimitCooldown {
	cooldown := DefaultRateLimit429AccountLimitCooldown()
	if value == "" {
		return cooldown
	}
	var stored RateLimit429AccountLimitCooldown
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return cooldown
	}
	cooldown.Enabled = stored.Enabled
	if isValidRateLimit429CooldownScope(stored.Scope) {
		cooldown.Scope = stored.Scope
	}
	if stored.CooldownSeconds >= minRateLimit429AccountLimitCooldownSeconds && stored.CooldownSeconds <= maxRateLimit429AccountLimitCooldownSeconds {
		cooldown.CooldownSeconds = stored.CooldownSeconds
	}
	return cooldown
}

func isValidRateLimit429CooldownScope(scope string) bool {
	return scope == RateLimit429CooldownScopeSession || scope == RateLimit429CooldownScopeDevice
}
