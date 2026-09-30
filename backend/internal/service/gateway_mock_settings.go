package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

// 下游测试请求 mock：全局默认关闭，规则为「关键词完整匹配 → 管理员配置的固定回复」。
// 该能力与账号级预热拦截相互独立；命中不产生上游尝试，也不产生使用记录。

const (
	SettingKeyGatewayMock = "gateway_mock_settings"
)

// 规则与回复的有界上限。这些数值是保守取值，不是用户逐字确认的产品承诺。
const (
	GatewayMockMaxRules        = 200
	GatewayMockMaxKeywordRunes = 200
	GatewayMockMaxReplyRunes   = 4000
	// GatewayMockReplyHeader 是本地 mock 响应的标识头，便于下游识别该回复未经上游推理。
	GatewayMockReplyHeader = "X-Sub2api-Mock"
	// GatewayMockReplyHeaderValue 是该标识头的固定值。
	GatewayMockReplyHeaderValue = "downstream-test"
)

// GatewayMockProtocol 是网关入口的协议族，决定请求体解析与响应形态。
type GatewayMockProtocol string

const (
	GatewayMockProtocolMessages        GatewayMockProtocol = "messages"
	GatewayMockProtocolChatCompletions GatewayMockProtocol = "chat_completions"
	GatewayMockProtocolResponses       GatewayMockProtocol = "responses"
)

var (
	ErrGatewayMockSettingsUnavailable = infraerrors.New(503, "GATEWAY_MOCK_SETTINGS_UNAVAILABLE", "gateway mock settings are temporarily unavailable")
	ErrGatewayMockRuleInvalid         = infraerrors.BadRequest("GATEWAY_MOCK_RULE_INVALID", "invalid gateway mock rule")
	ErrGatewayMockRuleKeywordEmpty    = infraerrors.BadRequest("GATEWAY_MOCK_RULE_KEYWORD_EMPTY", "gateway mock rule keyword must contain at least one non-whitespace character")
	ErrGatewayMockRuleKeywordTooLong  = infraerrors.BadRequest("GATEWAY_MOCK_RULE_KEYWORD_TOO_LONG", "gateway mock rule keyword is too long")
	ErrGatewayMockRuleReplyEmpty      = infraerrors.BadRequest("GATEWAY_MOCK_RULE_REPLY_EMPTY", "gateway mock rule reply must contain at least one non-whitespace character")
	ErrGatewayMockRuleReplyTooLong    = infraerrors.BadRequest("GATEWAY_MOCK_RULE_REPLY_TOO_LONG", "gateway mock rule reply is too long")
	ErrGatewayMockRuleKeywordTaken    = infraerrors.BadRequest("GATEWAY_MOCK_RULE_KEYWORD_TAKEN", "another enabled gateway mock rule already uses this keyword")
	ErrGatewayMockRuleNotFound        = infraerrors.NotFound("GATEWAY_MOCK_RULE_NOT_FOUND", "gateway mock rule not found")
	ErrGatewayMockTooManyRules        = infraerrors.BadRequest("GATEWAY_MOCK_TOO_MANY_RULES", "too many gateway mock rules")
)

// GatewayMockRule 是一条管理员配置的「关键词完整匹配 → 固定回复」规则。
// Keyword 始终保存归一化后的形式（去首尾空白、拉丁字母小写）。
//
// UpdatedAt 是规则内容的版本标记：最小命中事件只保存它与规则 ID，
// 规则被修改或删除后仍能解释"当时是怎么配的"，而不必留存关键词与回复正文。
type GatewayMockRule struct {
	ID        string `json:"id"`
	Keyword   string `json:"keyword"`
	Reply     string `json:"reply"`
	Enabled   bool   `json:"enabled"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// GatewayMockSettings 是该能力的持久化形态。缺省即关闭。
type GatewayMockSettings struct {
	Enabled bool              `json:"enabled"`
	Rules   []GatewayMockRule `json:"rules"`
}

// GatewayMockRuleView 是管理端可见的规则投影，附归一化关键词以便解释命中。
type GatewayMockRuleView struct {
	ID                string `json:"id"`
	Keyword           string `json:"keyword"`
	NormalizedKeyword string `json:"normalized_keyword"`
	Reply             string `json:"reply"`
	Enabled           bool   `json:"enabled"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

// GatewayMockOperatorStatus 是管理端读取的状态。
type GatewayMockOperatorStatus struct {
	Enabled         bool                  `json:"enabled"`
	Rules           []GatewayMockRuleView `json:"rules"`
	PresetAvailable bool                  `json:"preset_available"`
	PresetCreated   int                   `json:"preset_created"`
}

// GatewayMockRuleInput 是管理端提交的一条规则。
type GatewayMockRuleInput struct {
	ID      string `json:"id"`
	Keyword string `json:"keyword"`
	Reply   string `json:"reply"`
	Enabled bool   `json:"enabled"`
}

// GatewayMockOperatorUpdateInput 是管理端一次提交的完整规则集合与总开关。
type GatewayMockOperatorUpdateInput struct {
	Enabled bool                   `json:"enabled"`
	Rules   []GatewayMockRuleInput `json:"rules"`
}

// GatewayMockRuleset 是网关热路径使用的只读快照：关键词已归一化，仅含启用规则。
type GatewayMockRuleset struct {
	Enabled bool
	Rules   []GatewayMockRule
}

// NormalizeGatewayMockKeyword 归一化关键词：去首尾空白并把拉丁字母转为小写。
// 不做标点剔除、内部空白折叠、全半角或其它 Unicode 兼容折叠。
func NormalizeGatewayMockKeyword(keyword string) string {
	return strings.ToLower(strings.TrimSpace(keyword))
}

// gatewayMockRuleID 为没有 ID 的新规则生成稳定 ID。
func gatewayMockRuleID() string {
	return "gmr_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
}

// validateGatewayMockKeyword 校验归一化前后的关键词。
func validateGatewayMockKeyword(normalized string) error {
	if normalized == "" {
		return ErrGatewayMockRuleKeywordEmpty
	}
	if len([]rune(normalized)) > GatewayMockMaxKeywordRunes {
		return ErrGatewayMockRuleKeywordTooLong
	}
	return nil
}

// validateGatewayMockReply 校验回复文本：必须含非空白字符且在长度上限内。
func validateGatewayMockReply(reply string) error {
	if strings.TrimSpace(reply) == "" {
		return ErrGatewayMockRuleReplyEmpty
	}
	if len([]rune(reply)) > GatewayMockMaxReplyRunes {
		return ErrGatewayMockRuleReplyTooLong
	}
	return nil
}

// NormalizeGatewayMockSettings 校验并归一化完整规则集合：
// 关键词去空白加小写、按关键词去重（后到者拒绝）、丢弃空 ID 的新规则补 ID、按关键词排序保证稳定。
func NormalizeGatewayMockSettings(settings GatewayMockSettings) (GatewayMockSettings, error) {
	if len(settings.Rules) > GatewayMockMaxRules {
		return GatewayMockSettings{}, ErrGatewayMockTooManyRules
	}
	seen := make(map[string]string, len(settings.Rules))
	normalized := make([]GatewayMockRule, 0, len(settings.Rules))
	for index, rule := range settings.Rules {
		keyword := NormalizeGatewayMockKeyword(rule.Keyword)
		if err := validateGatewayMockKeyword(keyword); err != nil {
			return GatewayMockSettings{}, fmt.Errorf("rule[%d]: %w", index, err)
		}
		if err := validateGatewayMockReply(rule.Reply); err != nil {
			return GatewayMockSettings{}, fmt.Errorf("rule[%d]: %w", index, err)
		}
		id := strings.TrimSpace(rule.ID)
		if id == "" {
			id = gatewayMockRuleID()
		}
		if previous, ok := seen[keyword]; ok && previous != id {
			return GatewayMockSettings{}, fmt.Errorf("rule[%d]: %w", index, ErrGatewayMockRuleKeywordTaken)
		}
		seen[keyword] = id
		normalized = append(normalized, GatewayMockRule{
			ID:        id,
			Keyword:   keyword,
			Reply:     rule.Reply,
			Enabled:   rule.Enabled,
			UpdatedAt: rule.UpdatedAt,
		})
	}
	sort.SliceStable(normalized, func(i, j int) bool { return normalized[i].Keyword < normalized[j].Keyword })
	return GatewayMockSettings{Enabled: settings.Enabled, Rules: normalized}, nil
}

// GatewayMockDefaultPreset 返回预置但默认不生效的规则集合。这些词是保守的短探测词；
// 存在数字、emoji、语气词等都不预置，必须由管理员显式添加后才会命中。
func GatewayMockDefaultPreset() []GatewayMockRule {
	keywords := []string{"hi", "hello", "你好", "您好", "test", "测试", "ping"}
	rules := make([]GatewayMockRule, 0, len(keywords))
	for _, keyword := range keywords {
		rules = append(rules, GatewayMockRule{
			ID:      gatewayMockRuleID(),
			Keyword: keyword,
			Reply:   GatewayMockPresetReply(keyword),
			Enabled: false,
		})
	}
	return rules
}

// GatewayMockPresetReply 返回预置关键词的固定回复。
func GatewayMockPresetReply(keyword string) string {
	switch NormalizeGatewayMockKeyword(keyword) {
	case "你好", "您好":
		return "你好！我是本地返回的测试响应。"
	default:
		return "Hello! This is a locally generated test response."
	}
}

// ToView 把持久化规则投影为管理端可见形态。
func (s GatewayMockSettings) ToView() GatewayMockOperatorStatus {
	views := make([]GatewayMockRuleView, 0, len(s.Rules))
	for _, rule := range s.Rules {
		views = append(views, GatewayMockRuleView{
			ID:                rule.ID,
			Keyword:           rule.Keyword,
			NormalizedKeyword: rule.Keyword,
			Reply:             rule.Reply,
			Enabled:           rule.Enabled,
			UpdatedAt:         rule.UpdatedAt,
		})
	}
	return GatewayMockOperatorStatus{Enabled: s.Enabled, Rules: views}
}

// Ruleset 构造网关热路径使用的快照：仅保留启用规则，关键词已归一化。
func (s GatewayMockSettings) Ruleset() GatewayMockRuleset {
	ruleset := GatewayMockRuleset{Enabled: s.Enabled}
	if !s.Enabled {
		return ruleset
	}
	ruleset.Rules = make([]GatewayMockRule, 0, len(s.Rules))
	for _, rule := range s.Rules {
		if !rule.Enabled {
			continue
		}
		keyword := NormalizeGatewayMockKeyword(rule.Keyword)
		if keyword == "" {
			continue
		}
		ruleset.Rules = append(ruleset.Rules, GatewayMockRule{
			ID: rule.ID, Keyword: keyword, Reply: rule.Reply, UpdatedAt: rule.UpdatedAt,
		})
	}
	return ruleset
}

// MatchReply 在快照中按归一化关键词精确查找回复。
func (r GatewayMockRuleset) MatchReply(text string) (GatewayMockRule, bool) {
	if !r.Enabled {
		return GatewayMockRule{}, false
	}
	keyword := NormalizeGatewayMockKeyword(text)
	if keyword == "" {
		return GatewayMockRule{}, false
	}
	for _, rule := range r.Rules {
		if rule.Keyword == keyword {
			return rule, true
		}
	}
	return GatewayMockRule{}, false
}

// gatewayMockCacheTTL 是网关读到设置的传播延迟上限；本实例写入会立即失效缓存。
const gatewayMockCacheTTL = 5 * time.Second

// gatewayMockDBTimeout 独立于请求上下文：客户端断连不得把读取变成"关闭"。
const gatewayMockDBTimeout = 5 * time.Second

type cachedGatewayMockRuleset struct {
	ruleset   GatewayMockRuleset
	expiresAt time.Time
	version   uint64
}

// readGatewayMockSettings 读取持久化的规则集合。缺省、未配置或解码失败都按关闭处理。
func (s *SettingService) readGatewayMockSettings(ctx context.Context) (GatewayMockSettings, error) {
	if s == nil || s.settingRepo == nil {
		return GatewayMockSettings{}, ErrGatewayMockSettingsUnavailable
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyGatewayMock)
	if errors.Is(err, ErrSettingNotFound) {
		return GatewayMockSettings{}, nil
	}
	if err != nil {
		return GatewayMockSettings{}, fmt.Errorf("get gateway mock settings: %w", err)
	}
	var stored GatewayMockSettings
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return GatewayMockSettings{}, nil
	}
	return stored, nil
}

// GetGatewayMockOperatorStatus 返回管理端可见的完整规则集合（含未启用规则）。
func (s *SettingService) GetGatewayMockOperatorStatus(ctx context.Context) (GatewayMockOperatorStatus, error) {
	stored, err := s.readGatewayMockSettings(ctx)
	if err != nil {
		return GatewayMockOperatorStatus{}, err
	}
	return stored.ToView(), nil
}

// UpdateGatewayMockOperatorSettings 原子替换完整规则集合。
// 保存成功后本实例的网关缓存立即失效，后续新请求不再按旧规则命中。
func (s *SettingService) UpdateGatewayMockOperatorSettings(ctx context.Context, input GatewayMockOperatorUpdateInput) (GatewayMockOperatorStatus, error) {
	if s == nil || s.settingRepo == nil {
		return GatewayMockOperatorStatus{}, ErrGatewayMockSettingsUnavailable
	}
	current, err := s.readGatewayMockSettings(ctx)
	if err != nil {
		return GatewayMockOperatorStatus{}, err
	}
	existingUpdatedAt := make(map[string]string, len(current.Rules))
	for _, rule := range current.Rules {
		if rule.UpdatedAt != "" {
			existingUpdatedAt[rule.ID] = rule.UpdatedAt
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	next := make([]GatewayMockRule, 0, len(input.Rules))
	for _, rule := range input.Rules {
		next = append(next, GatewayMockRule{
			ID:      rule.ID,
			Keyword: rule.Keyword,
			Reply:   rule.Reply,
			Enabled: rule.Enabled,
		})
	}
	normalized, err := NormalizeGatewayMockSettings(GatewayMockSettings{Enabled: input.Enabled, Rules: next})
	if err != nil {
		return GatewayMockOperatorStatus{}, err
	}
	for index := range normalized.Rules {
		if previous, ok := existingUpdatedAt[normalized.Rules[index].ID]; ok {
			normalized.Rules[index].UpdatedAt = previous
			continue
		}
		normalized.Rules[index].UpdatedAt = now
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return GatewayMockOperatorStatus{}, fmt.Errorf("encode gateway mock settings: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyGatewayMock, string(encoded)); err != nil {
		return GatewayMockOperatorStatus{}, fmt.Errorf("save gateway mock settings: %w", err)
	}
	s.invalidateGatewayMockRuleset()
	return normalized.ToView(), nil
}

// SeedGatewayMockPresets 在规则集合为空时写入预置词（默认不启用）。
func (s *SettingService) SeedGatewayMockPresets(ctx context.Context) (GatewayMockOperatorStatus, int, error) {
	if s == nil || s.settingRepo == nil {
		return GatewayMockOperatorStatus{}, 0, ErrGatewayMockSettingsUnavailable
	}
	current, err := s.readGatewayMockSettings(ctx)
	if err != nil {
		return GatewayMockOperatorStatus{}, 0, err
	}
	if len(current.Rules) > 0 {
		return current.ToView(), 0, nil
	}
	preset, err := NormalizeGatewayMockSettings(GatewayMockSettings{Enabled: current.Enabled, Rules: GatewayMockDefaultPreset()})
	if err != nil {
		return GatewayMockOperatorStatus{}, 0, err
	}
	encoded, err := json.Marshal(preset)
	if err != nil {
		return GatewayMockOperatorStatus{}, 0, fmt.Errorf("encode gateway mock presets: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyGatewayMock, string(encoded)); err != nil {
		return GatewayMockOperatorStatus{}, 0, fmt.Errorf("save gateway mock presets: %w", err)
	}
	s.invalidateGatewayMockRuleset()
	return preset.ToView(), len(preset.Rules), nil
}

// GatewayMockRuleset 返回本请求可用的规则快照。它在每条推理请求的准入路径上被调用，
// 因此带进程内缓存与 singleflight。读取失败按"关闭"处理且只缓存一个短结论，绝不 fail-open。
func (s *SettingService) GatewayMockRuleset(ctx context.Context) GatewayMockRuleset {
	if s == nil || s.settingRepo == nil {
		return GatewayMockRuleset{}
	}
	version := s.gatewayMockVersion.Load()
	if cached, ok := s.gatewayMockCache.Load().(*cachedGatewayMockRuleset); ok && cached != nil &&
		cached.version == version && time.Now().Before(cached.expiresAt) {
		return cached.ruleset
	}
	value, _, _ := s.gatewayMockSF.Do("gateway_mock_ruleset", func() (any, error) {
		ruleset, readVersion := s.gatewayMockRulesetUncached(ctx)
		s.gatewayMockCache.Store(&cachedGatewayMockRuleset{
			ruleset:   ruleset,
			expiresAt: time.Now().Add(gatewayMockCacheTTL),
			version:   readVersion,
		})
		return ruleset, nil
	})
	if ruleset, ok := value.(GatewayMockRuleset); ok {
		return ruleset
	}
	return GatewayMockRuleset{}
}

// invalidateGatewayMockRuleset 让本实例的下一次读取看到新设置。
func (s *SettingService) invalidateGatewayMockRuleset() {
	if s == nil {
		return
	}
	s.gatewayMockVersion.Add(1)
	s.gatewayMockSF.Forget("gateway_mock_ruleset")
}

func (s *SettingService) gatewayMockRulesetUncached(ctx context.Context) (GatewayMockRuleset, uint64) {
	readCtx := ctx
	if readCtx == nil {
		readCtx = context.Background()
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(readCtx), gatewayMockDBTimeout)
	defer cancel()
	version := s.gatewayMockVersion.Load()
	stored, err := s.readGatewayMockSettings(readCtx)
	if err != nil {
		return GatewayMockRuleset{}, version
	}
	return stored.Ruleset(), version
}
