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
}

type RequestTraceOperatorUpdateInput struct {
	Enabled     bool
	Language    string
	Phrase      string
	AdminUserID int64
	IPAddress   string
	UserAgent   string
}

type RequestTraceGate struct {
	CaptureAllowed bool
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
	return stored, nil
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

func (s *SettingService) RequestTraceGate(ctx context.Context) RequestTraceGate {
	stored, err := s.readRequestTraceSettings(ctx)
	if err != nil || !stored.Enabled || !stored.RiskAcknowledged {
		return RequestTraceGate{}
	}
	ack, err := s.readRequestTraceAcknowledgement(ctx)
	if err != nil || ack == nil || !ack.CoversCurrentStatement() {
		return RequestTraceGate{}
	}
	return RequestTraceGate{CaptureAllowed: s.requestTraceSupport(ctx).Supported}
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
	stored := RequestTraceSettings{Enabled: input.Enabled}
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
		status.PlaintextCaptureSupportReason = PlaintextCaptureSupportReasonProbeUnavailable
		if cached, ok := s.requestTraceSupportCache.Load().(*cachedPlaintextCaptureSupport); ok && cached != nil {
			status.PlaintextCaptureSupported = cached.support.Supported
			status.PlaintextCaptureSupportReason = cached.support.Reason
		}
		return status, nil
	}
	return s.GetRequestTraceOperatorStatus(ctx)
}
