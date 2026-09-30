package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	SettingKeyRequestTrace                    = "request_trace_settings"
	SettingKeyRequestTraceRiskAcknowledgement = "request_trace_risk_acknowledgement"
	RequestTraceRiskAcknowledgementVersion    = "v2026.09.28"

	RequestTraceRiskAcknowledgementPhraseEN = "Request Traces store client requests, upstream attempts and responses, and downstream responses in plaintext, including prompts, tool data, metadata.user_id and streaming events. Known credentials are redacted when identifiable, but unverified raw body fragments can still contain known credentials and unknown secrets. Linked Traces live with their usage record and can remain indefinitely if usage cleanup is disabled; unlinked Traces remain readable until physically cleaned, which is scheduled from day 30. Admin sessions can read Traces without step-up verification. Local plaintext export files are not encrypted or given extra file permissions, can outlive their source records, and can be downloaded for up to seven days. Backups, replicas, PITR and downloaded copies may persist after deletion. This is not a compliance erasure mechanism."
	RequestTraceRiskAcknowledgementPhraseZH = "请求 Trace 将客户端请求、逐次上游尝试与响应、最终下游响应以明文保存，包括提示词、工具数据、metadata.user_id 和流事件。可识别的已知凭据会被遮蔽，但脱敏未验证的原始正文片段仍可能含已知凭据与未知秘密。已关联 Trace 随使用记录保留，若停用使用记录清理则可能无限期存在；未关联 Trace 从第 30 天起计划清理，在实际清理前仍可读取。管理员登录会话无需二次验证即可查看 Trace。本机明文导出文件不加密、不额外设置文件权限，可在源记录删除后继续存在，最长七天内可下载。备份、副本、PITR 与已下载副本在删除后可能继续留存。本功能不是合规擦除手段。"
)

var (
	ErrRequestTraceRiskAcknowledgementRequired = infraerrors.BadRequest("REQUEST_TRACE_RISK_ACK_REQUIRED", "enabling request trace capture requires the written risk acknowledgement phrase")
	ErrRequestTraceRiskAcknowledgementInvalid  = infraerrors.BadRequest("REQUEST_TRACE_RISK_ACK_INVALID", "the request trace risk acknowledgement does not match the current statement")
	ErrRequestTraceOperatorIdentityRequired    = infraerrors.Forbidden("REQUEST_TRACE_OPERATOR_SESSION_REQUIRED", "enabling request traces requires an authenticated admin session")
	ErrRequestTraceSettingsUnavailable         = infraerrors.New(503, "REQUEST_TRACE_SETTINGS_UNAVAILABLE", "request trace settings are temporarily unavailable")
	ErrRequestTraceDeploymentUnsupported       = infraerrors.Conflict("REQUEST_TRACE_DEPLOYMENT_UNSUPPORTED", "request trace capture cannot be enabled without verified usage ownership")
)

type RequestTraceSettings struct {
	Enabled          bool `json:"enabled"`
	RiskAcknowledged bool `json:"risk_acknowledged"`

	// 采集范围：只决定后续请求是否进入采集，不追溯改变已存 Trace。
	// 分组按下游 API Key 所属分组匹配；模型按**客户端请求的**模型匹配（不是出站映射结果）；
	// 平台按首次可确定的实际选中上游账号平台匹配。
	AllGroups     bool     `json:"all_groups"`
	GroupIDs      []int64  `json:"group_ids,omitempty"`
	ModelScope    string   `json:"model_scope,omitempty"` // all | include | exclude
	Models        []string `json:"models,omitempty"`
	PlatformScope string   `json:"platform_scope,omitempty"` // all | include | exclude
	Platforms     []string `json:"platforms,omitempty"`
	// 注：平台无法确定的请求（鉴权前拒绝、未选到账号）在“仅指定/排除指定”下一律不采集，
	// 因此这里不需要单独的“是否排除未知”开关；只有“所有平台”才覆盖未知。
}

type RequestTraceRiskAcknowledgement struct {
	Version     string    `json:"version"`
	Phrase      string    `json:"phrase"`
	AdminUserID int64     `json:"admin_user_id"`
	IPAddress   string    `json:"ip_address,omitempty"`
	UserAgent   string    `json:"user_agent,omitempty"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

func (a RequestTraceRiskAcknowledgement) CoversCurrentStatement() bool {
	return a.Version == RequestTraceRiskAcknowledgementVersion && a.AdminUserID > 0 && !a.AcceptedAt.IsZero() &&
		(a.Phrase == RequestTraceRiskAcknowledgementPhraseEN || a.Phrase == RequestTraceRiskAcknowledgementPhraseZH)
}

type RequestTraceRiskAcknowledgementView struct {
	Version     string    `json:"version"`
	Phrase      string    `json:"phrase"`
	AdminUserID int64     `json:"admin_user_id"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

type RequestTraceOperatorStatus struct {
	Enabled                       bool                                 `json:"enabled"`
	RiskAcknowledged              bool                                 `json:"risk_acknowledged"`
	RiskAcknowledgementCurrent    bool                                 `json:"risk_acknowledgement_current"`
	RiskAcknowledgement           *RequestTraceRiskAcknowledgementView `json:"risk_acknowledgement,omitempty"`
	RiskVersion                   string                               `json:"risk_version"`
	RiskPhraseEN                  string                               `json:"risk_phrase_en"`
	RiskPhraseZH                  string                               `json:"risk_phrase_zh"`
	CaptureAllowed                bool                                 `json:"capture_allowed"`
	PlaintextCaptureSupported     bool                                 `json:"plaintext_capture_supported"`
	PlaintextCaptureSupportReason string                               `json:"plaintext_capture_support_reason"`

	// 采集范围：管理端据此回显当前生效条件（与开关同一条记录）。
	AllGroups     bool     `json:"all_groups"`
	GroupIDs      []int64  `json:"group_ids"`
	ModelScope    string   `json:"model_scope"`
	Models        []string `json:"models"`
	PlatformScope string   `json:"platform_scope"`
	Platforms     []string `json:"platforms"`
}

type RequestTraceOperatorUpdateInput struct {
	Enabled bool

	// ScopeProvided 为真时按本次提交替换采集范围；否则保留既有范围。
	ScopeProvided bool
	AllGroups     bool
	GroupIDs      []int64
	ModelScope    string
	Models        []string
	PlatformScope string
	Platforms     []string

	Language    string
	Phrase      string
	AdminUserID int64
	IPAddress   string
	UserAgent   string
}

// 采集范围里的模型/平台过滤类型，沿用内容审计的既有取值语义。
const (
	RequestTraceScopeAll     = "all"
	RequestTraceScopeInclude = "include"
	RequestTraceScopeExclude = "exclude"
)

// RequestTraceScopeFacts 是判断一条逻辑请求是否落在采集范围内所需的事实。
// 值为零值表示该事实**尚未确定**（例如鉴权前被拒时只知道请求体未读）。
type RequestTraceScopeFacts struct {
	// GroupID 是下游 API Key 所属分组；nil 表示无法确定。
	GroupID *int64
	// RequestedModel 是客户端请求的模型名；空表示无法确定。
	RequestedModel string
	// Platforms 是本次逻辑请求实际选中过的上游账号平台（去重）；空表示无法确定。
	Platforms []string
}

type RequestTraceGate struct {
	CaptureAllowed bool
	// Scope 是本次求值用的采集范围；调用方在请求结束时用它复核实际观察到的事实。
	Scope RequestTraceSettings
}

// InGroupScope 报告分组是否落在采集范围内。
// “全部分组”覆盖无法确定分组的请求；指定分组时无法确定即不采集。
func (s RequestTraceSettings) InGroupScope(groupID *int64) bool {
	if s.AllGroups {
		return true
	}
	if groupID == nil {
		return false
	}
	for _, id := range s.GroupIDs {
		if id == *groupID {
			return true
		}
	}
	return false
}

// InModelScope 报告客户端请求模型是否落在采集范围内。
// 沿用内容审计口径：仅“所有模型”覆盖无法确定模型名的请求；
// 仅指定/排除指定都必须先拿到有效模型名，且不做归一化以外的猜测。
func (s RequestTraceSettings) InModelScope(model string) bool {
	switch s.ModelScope {
	case RequestTraceScopeInclude:
		return model != "" && gatewayMockModelListContains(s.Models, model)
	case RequestTraceScopeExclude:
		return model != "" && !gatewayMockModelListContains(s.Models, model)
	default:
		return true
	}
}

// InPlatformScope 报告实际选中过的上游账号平台是否落在采集范围内。
//
// 首次可确定的实际账号平台决定整条逻辑请求的结论（见 Trace 采集范围定义）：
// 只有第一个平台参与判定，后来重试切到别的平台不改变结论。因此这里的入参
// 只包含首个平台，多元素时按"任一被排除即整条不采"从严处理。
//
// “仅指定”与“排除指定”都**必须**先拿到平台事实：排除是指定值时，无法确定的
// 请求不能被当成"没在排除列表里"而放行，否则排除形同虚设。
func (s RequestTraceSettings) InPlatformScope(platforms []string) bool {
	switch s.PlatformScope {
	case RequestTraceScopeInclude:
		for _, platform := range platforms {
			if gatewayMockModelListContains(s.Platforms, platform) {
				return true
			}
		}
		return false
	case RequestTraceScopeExclude:
		if len(platforms) == 0 {
			return false
		}
		for _, platform := range platforms {
			if gatewayMockModelListContains(s.Platforms, platform) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// InScope 报告给定事实是否整体落在采集范围内。
func (s RequestTraceSettings) InScope(facts RequestTraceScopeFacts) bool {
	return s.InGroupScope(facts.GroupID) &&
		s.InModelScope(facts.RequestedModel) &&
		s.InPlatformScope(facts.Platforms)
}

// gatewayMockModelListContains 做去首尾空白、不区分大小写的相等比较。
func gatewayMockModelListContains(values []string, target string) bool {
	normalized := NormalizeGatewayMockKeyword(target)
	if normalized == "" {
		return false
	}
	for _, value := range values {
		if NormalizeGatewayMockKeyword(value) == normalized {
			return true
		}
	}
	return false
}

// NormalizeRequestTraceSettings 归一化范围字段：过滤类型落回封闭取值，
// 列表去空白去重；模型/平台列表不做大小写改写，比较时再归一。
func NormalizeRequestTraceSettings(settings RequestTraceSettings) RequestTraceSettings {
	settings.ModelScope = normalizeRequestTraceScope(settings.ModelScope)
	settings.PlatformScope = normalizeRequestTraceScope(settings.PlatformScope)
	if settings.ModelScope == RequestTraceScopeAll {
		settings.Models = nil
	} else {
		settings.Models = normalizeRequestTraceScopeList(settings.Models)
	}
	if settings.PlatformScope == RequestTraceScopeAll {
		settings.Platforms = nil
	} else {
		settings.Platforms = normalizeRequestTraceScopeList(settings.Platforms)
	}
	settings.GroupIDs = normalizeRequestTraceGroupIDs(settings.GroupIDs)
	if settings.AllGroups {
		settings.GroupIDs = nil
	}
	return settings
}

// normalizeRequestTraceScope 把取值收敛到封闭集合。
// 大小写与首尾空白不参与判定：手写或导入的 "Include" 不应被静默降级成 "all"
// ——那会把一次限制悄悄变成"采集全部"。
func normalizeRequestTraceScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case RequestTraceScopeInclude:
		return RequestTraceScopeInclude
	case RequestTraceScopeExclude:
		return RequestTraceScopeExclude
	default:
		return RequestTraceScopeAll
	}
}

func normalizeRequestTraceScopeList(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		key := strings.ToLower(trimmed)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func normalizeRequestTraceGroupIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// RequestTraceSupportProbe must verify the trace table itself and its cascading usage FK.
// Unlike the legacy probe, a missing trace table is not a supported deployment.
type RequestTraceSupportProbe interface {
	ProbeRequestTraceSupport(ctx context.Context) (PlaintextCaptureSupport, error)
}

func (s *SettingService) SetRequestTraceSupportProbe(probe RequestTraceSupportProbe) {
	if s != nil {
		s.requestTraceSupportProbe = probe
	}
}

func (s *SettingService) requestTraceSupportFresh(ctx context.Context) PlaintextCaptureSupport {
	if s == nil || s.requestTraceSupportProbe == nil {
		return PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonProbeUnavailable}
	}
	support, err := s.requestTraceSupportProbe.ProbeRequestTraceSupport(ctx)
	if err != nil {
		return PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonProbeFailed}
	}
	return normalizePlaintextCaptureSupport(support)
}

func (s *SettingService) requestTraceSupport(ctx context.Context) PlaintextCaptureSupport {
	if s == nil {
		return PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonProbeUnavailable}
	}
	if cached, ok := s.requestTraceSupportCache.Load().(*cachedPlaintextCaptureSupport); ok && cached != nil && time.Now().Before(cached.expiresAt) {
		return cached.support
	}
	value, _, _ := s.requestTraceSupportSF.Do("request_trace_support", func() (any, error) {
		support := s.requestTraceSupportFresh(ctx)
		s.requestTraceSupportCache.Store(&cachedPlaintextCaptureSupport{support: support, expiresAt: time.Now().Add(plaintextCaptureSupportCacheTTL)})
		return support, nil
	})
	if support, ok := value.(PlaintextCaptureSupport); ok {
		return support
	}
	return PlaintextCaptureSupport{Reason: PlaintextCaptureSupportReasonProbeUnavailable}
}

func (s *SettingService) readRequestTraceSettings(ctx context.Context) (RequestTraceSettings, error) {
	if s == nil || s.settingRepo == nil {
		return RequestTraceSettings{}, ErrRequestTraceSettingsUnavailable
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRequestTrace)
	if errors.Is(err, ErrSettingNotFound) {
		return RequestTraceSettings{}, nil
	}
	if err != nil {
		return RequestTraceSettings{}, fmt.Errorf("get request trace settings: %w", err)
	}
	var stored RequestTraceSettings
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return RequestTraceSettings{}, nil
	}
	return NormalizeRequestTraceSettings(stored), nil
}

func (s *SettingService) readRequestTraceAcknowledgement(ctx context.Context) (*RequestTraceRiskAcknowledgement, error) {
	if s == nil || s.settingRepo == nil {
		return nil, ErrRequestTraceSettingsUnavailable
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRequestTraceRiskAcknowledgement)
	if errors.Is(err, ErrSettingNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get request trace acknowledgement: %w", err)
	}
	var ack RequestTraceRiskAcknowledgement
	if err := json.Unmarshal([]byte(raw), &ack); err != nil {
		return nil, fmt.Errorf("decode request trace acknowledgement: %w", err)
	}
	return &ack, nil
}

// requestTraceGateCacheTTL 是采集门控结论的进程内缓存时长，只影响其它实例写入设置
// 后的传播延迟；本实例的开启／关闭写入会立即失效缓存。取小值以便运维改动快速生效。
const requestTraceGateCacheTTL = 5 * time.Second

// requestTraceGateDBTimeout 独立于请求上下文：客户端断连不得把门控读数变成"关闭"。
const requestTraceGateDBTimeout = 5 * time.Second

type cachedRequestTraceGate struct {
	gate      RequestTraceGate
	expiresAt time.Time
	version   uint64
}

// RequestTraceGate 报告本请求是否允许采集 Trace。它在每条推理请求的准入路径上被调用，
// 因此结论带进程内缓存与 singleflight：关闭状态（默认）也不会每请求回读设置表。
// 读取失败按"关闭"处理并且只缓存一个短结论，绝不 fail-open。
func (s *SettingService) RequestTraceGate(ctx context.Context) RequestTraceGate {
	if s == nil || s.settingRepo == nil {
		return RequestTraceGate{}
	}
	current := s.requestTraceGateVersion.Load()
	if cached, ok := s.requestTraceGateCache.Load().(*cachedRequestTraceGate); ok && cached != nil &&
		cached.version == current && time.Now().Before(cached.expiresAt) {
		return cached.gate
	}
	value, _, _ := s.requestTraceGateSF.Do("request_trace_gate", func() (any, error) {
		version := s.requestTraceGateVersion.Load()
		if cached, ok := s.requestTraceGateCache.Load().(*cachedRequestTraceGate); ok && cached != nil &&
			cached.version == version && time.Now().Before(cached.expiresAt) {
			return *cached, nil
		}
		if ctx == nil {
			ctx = context.Background()
		}
		// 每次求值都带上自己的超时，不让断连取消影响其它排队请求。
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTraceGateDBTimeout)
		defer cancel()
		gate := s.requestTraceGateUncached(dbCtx)
		if version != s.requestTraceGateVersion.Load() {
			// 写入设置在查询期间完成了：旧读数绝不能覆盖开关，也不能把
			// "允许采集"返回给刚刚紧急关闭后的等待者。
			return cachedRequestTraceGate{}, nil
		}
		cached := cachedRequestTraceGate{gate: gate, expiresAt: time.Now().Add(requestTraceGateCacheTTL), version: version}
		s.requestTraceGateCache.Store(&cached)
		return cached, nil
	})
	if cached, ok := value.(cachedRequestTraceGate); ok && cached.version == s.requestTraceGateVersion.Load() &&
		time.Now().Before(cached.expiresAt) {
		return cached.gate
	}
	return RequestTraceGate{}
}

// invalidateRequestTraceGate 让下一次门控重新求值。写入设置后立即调用。
func (s *SettingService) invalidateRequestTraceGate() {
	if s != nil {
		s.requestTraceGateVersion.Add(1)
	}
}

func (s *SettingService) requestTraceGateUncached(ctx context.Context) RequestTraceGate {
	stored, err := s.readRequestTraceSettings(ctx)
	if err != nil || !stored.Enabled || !stored.RiskAcknowledged {
		return RequestTraceGate{}
	}
	ack, err := s.readRequestTraceAcknowledgement(ctx)
	if err != nil || ack == nil || !ack.CoversCurrentStatement() {
		return RequestTraceGate{}
	}
	// 范围随门控一起冻结：一次逻辑请求内的所有判断都使用同一份范围快照，
	// 避免执行中修改配置导致同一条请求前后结论不一致。
	return RequestTraceGate{CaptureAllowed: s.requestTraceSupport(ctx).Supported, Scope: stored}
}

func (s *SettingService) GetRequestTraceOperatorStatus(ctx context.Context) (RequestTraceOperatorStatus, error) {
	status := RequestTraceOperatorStatus{
		RiskVersion:  RequestTraceRiskAcknowledgementVersion,
		RiskPhraseEN: RequestTraceRiskAcknowledgementPhraseEN,
		RiskPhraseZH: RequestTraceRiskAcknowledgementPhraseZH,
	}
	stored, err := s.readRequestTraceSettings(ctx)
	if err != nil {
		return RequestTraceOperatorStatus{}, err
	}
	ack, err := s.readRequestTraceAcknowledgement(ctx)
	if err != nil {
		return RequestTraceOperatorStatus{}, err
	}
	status.Enabled = stored.Enabled
	status.RiskAcknowledged = stored.RiskAcknowledged
	status.AllGroups = stored.AllGroups
	status.GroupIDs = stored.GroupIDs
	status.ModelScope = stored.ModelScope
	status.Models = stored.Models
	status.PlatformScope = stored.PlatformScope
	status.Platforms = stored.Platforms
	if ack != nil {
		status.RiskAcknowledgement = &RequestTraceRiskAcknowledgementView{Version: ack.Version, Phrase: ack.Phrase, AdminUserID: ack.AdminUserID, AcceptedAt: ack.AcceptedAt}
		status.RiskAcknowledgementCurrent = ack.CoversCurrentStatement()
	}
	support := s.requestTraceSupport(ctx)
	status.PlaintextCaptureSupported = support.Supported
	status.PlaintextCaptureSupportReason = support.Reason
	status.CaptureAllowed = stored.Enabled && stored.RiskAcknowledged && status.RiskAcknowledgementCurrent && support.Supported
	return status, nil
}

func (s *SettingService) UpdateRequestTraceOperatorSettings(ctx context.Context, input RequestTraceOperatorUpdateInput) (RequestTraceOperatorStatus, error) {
	if s == nil || s.settingRepo == nil {
		return RequestTraceOperatorStatus{}, ErrRequestTraceSettingsUnavailable
	}
	updates := map[string]string{}
	// 范围与开关同存一条记录：未提交范围字段时保留既有范围，避免关闭/开启动作顺手清空范围。
	previous, _ := s.readRequestTraceSettings(ctx)
	stored := RequestTraceSettings{
		Enabled:       input.Enabled,
		AllGroups:     previous.AllGroups,
		GroupIDs:      previous.GroupIDs,
		ModelScope:    previous.ModelScope,
		Models:        previous.Models,
		PlatformScope: previous.PlatformScope,
		Platforms:     previous.Platforms,
	}
	if input.ScopeProvided {
		stored.AllGroups = input.AllGroups
		stored.GroupIDs = input.GroupIDs
		stored.ModelScope = input.ModelScope
		stored.Models = input.Models
		stored.PlatformScope = input.PlatformScope
		stored.Platforms = input.Platforms
	}
	stored = NormalizeRequestTraceSettings(stored)
	if input.Enabled {
		if input.AdminUserID <= 0 {
			return RequestTraceOperatorStatus{}, ErrRequestTraceOperatorIdentityRequired
		}
		phrase := RequestTraceRiskAcknowledgementPhraseEN
		if normalizeAdminComplianceLanguage(input.Language) == "zh" {
			phrase = RequestTraceRiskAcknowledgementPhraseZH
		}
		if strings.TrimSpace(input.Phrase) == "" {
			return RequestTraceOperatorStatus{}, ErrRequestTraceRiskAcknowledgementRequired
		}
		if input.Phrase != phrase {
			return RequestTraceOperatorStatus{}, ErrRequestTraceRiskAcknowledgementInvalid
		}
		// Enablement bypasses the cached support verdict. Disablement never probes.
		support := s.requestTraceSupportFresh(ctx)
		s.requestTraceSupportCache.Store(&cachedPlaintextCaptureSupport{support: support, expiresAt: time.Now().Add(plaintextCaptureSupportCacheTTL)})
		if !support.Supported {
			return RequestTraceOperatorStatus{}, withPlaintextCaptureSupportReason(ErrRequestTraceDeploymentUnsupported, support.Reason)
		}
		stored.RiskAcknowledged = true
		ack := RequestTraceRiskAcknowledgement{
			Version: RequestTraceRiskAcknowledgementVersion, Phrase: input.Phrase, AdminUserID: input.AdminUserID,
			IPAddress: strings.TrimSpace(input.IPAddress), UserAgent: strings.TrimSpace(input.UserAgent), AcceptedAt: time.Now().UTC(),
		}
		payload, err := json.Marshal(ack)
		if err != nil {
			return RequestTraceOperatorStatus{}, fmt.Errorf("marshal request trace acknowledgement: %w", err)
		}
		updates[SettingKeyRequestTraceRiskAcknowledgement] = string(payload)
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return RequestTraceOperatorStatus{}, fmt.Errorf("marshal request trace settings: %w", err)
	}
	updates[SettingKeyRequestTrace] = string(payload)
	if err := s.settingRepo.SetMultiple(ctx, updates); err != nil {
		return RequestTraceOperatorStatus{}, fmt.Errorf("save request trace settings: %w", err)
	}
	// 开启与紧急关闭都必须立即生效，不能等缓存 TTL。
	s.invalidateRequestTraceGate()
	slog.Info("request_trace.operator_settings_updated", "audit", true, "enabled", stored.Enabled,
		"risk_version", RequestTraceRiskAcknowledgementVersion, "admin_user_id", input.AdminUserID)
	if !stored.Enabled {
		// A corrupt acknowledgement or a failing deployment probe must not turn a
		// successful emergency disable into an apparent failure. The stored switch
		// has already been written off; the capture gate cannot read it as enabled.
		status := RequestTraceOperatorStatus{
			RiskVersion:  RequestTraceRiskAcknowledgementVersion,
			RiskPhraseEN: RequestTraceRiskAcknowledgementPhraseEN,
			RiskPhraseZH: RequestTraceRiskAcknowledgementPhraseZH,
		}
		// 紧急关闭路径也要回显当前范围，避免管理端误以为范围被清空。
		status.AllGroups = stored.AllGroups
		status.GroupIDs = stored.GroupIDs
		status.ModelScope = stored.ModelScope
		status.Models = stored.Models
		status.PlatformScope = stored.PlatformScope
		status.Platforms = stored.Platforms
		status.PlaintextCaptureSupportReason = PlaintextCaptureSupportReasonProbeUnavailable
		if cached, ok := s.requestTraceSupportCache.Load().(*cachedPlaintextCaptureSupport); ok && cached != nil {
			status.PlaintextCaptureSupported = cached.support.Supported
			status.PlaintextCaptureSupportReason = cached.support.Reason
		}
		return status, nil
	}
	return s.GetRequestTraceOperatorStatus(ctx)
}
