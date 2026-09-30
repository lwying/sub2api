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

// 明文导出副本的独立风险确认。
//
// 它与「Trace 采集」的确认**分开**：开启采集不等于接受"可以把明文副本写到本机临时文件
// 并最长七天可下载"。升级采集确认的版本时，导出确认也随之失效——但反过来，缺少导出
// 确认不会停掉已经开启的采集（那会让升级变成一次静默的停采）。
const (
	SettingKeyRequestTraceExportRiskAcknowledgement = "request_trace_export_risk_acknowledgement"
	RequestTraceExportRiskAcknowledgementVersion    = "v2026.09.30"

	RequestTraceExportRiskAcknowledgementPhraseEN = "Request Trace export writes a plaintext copy of Trace detail, including prompts, tool data and unverified raw body fragments, into this machine's temporary directory without extra encryption or file ACLs. Only the admin login session that created the task can read its manifest and shards for up to seven days; the files can be lost earlier, outlive the source records, and are not erased from backups, replicas, PITR or copies already downloaded. Exports are refused on multi-instance deployments without a shared filesystem. This is not a compliance erasure mechanism."
	RequestTraceExportRiskAcknowledgementPhraseZH = "请求 Trace 导出会把 Trace 详情（包括提示词、工具数据与脱敏未验证的原始片段）以明文副本写入本机临时目录，不额外加密、不设置额外文件权限。最长七天内只有创建该任务的管理员登录会话可读取它的清单与分片；文件可能更早丢失，也能在源记录删除后继续存在，并且不会从备份、副本、PITR 或已下载的拷贝中被擦除。多实例且无共享文件系统的部署一律拒绝导出。本功能不是合规擦除手段。"
)

var (
	ErrRequestTraceExportRiskAcknowledgementRequired = infraerrors.BadRequest("REQUEST_TRACE_EXPORT_RISK_ACK_REQUIRED", "creating a plaintext export requires the written export risk acknowledgement")
	ErrRequestTraceExportRiskAcknowledgementInvalid  = infraerrors.BadRequest("REQUEST_TRACE_EXPORT_RISK_ACK_INVALID", "the export risk acknowledgement does not match the current statement")
)

// RequestTraceExportRiskAcknowledgement 是管理员对当前版本导出风险声明的接受记录。
type RequestTraceExportRiskAcknowledgement struct {
	Version     string    `json:"version"`
	Phrase      string    `json:"phrase"`
	AdminUserID int64     `json:"admin_user_id"`
	IPAddress   string    `json:"ip_address,omitempty"`
	UserAgent   string    `json:"user_agent,omitempty"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

// CoversCurrentStatement 报告这份确认是否覆盖当前版本的声明。
func (a RequestTraceExportRiskAcknowledgement) CoversCurrentStatement() bool {
	return a.Version == RequestTraceExportRiskAcknowledgementVersion && a.AdminUserID > 0 && !a.AcceptedAt.IsZero() &&
		(a.Phrase == RequestTraceExportRiskAcknowledgementPhraseEN || a.Phrase == RequestTraceExportRiskAcknowledgementPhraseZH)
}

// RequestTraceExportRiskStatus 是管理端可见的导出确认状态。
type RequestTraceExportRiskStatus struct {
	Acknowledged bool   `json:"acknowledged"`
	Version      string `json:"version"`
	PhraseEN     string `json:"phrase_en"`
	PhraseZH     string `json:"phrase_zh"`
	Phrase       string `json:"phrase,omitempty"`
	AdminUserID  int64  `json:"admin_user_id,omitempty"`
	AcceptedAt   string `json:"accepted_at,omitempty"`
}

// RequestTraceExportRiskUpdateInput 是提交一次导出确认所需的输入。
type RequestTraceExportRiskUpdateInput struct {
	Language    string
	Phrase      string
	AdminUserID int64
	IPAddress   string
	UserAgent   string
}

// readRequestTraceExportAcknowledgement 读取已存的导出确认。
func (s *SettingService) readRequestTraceExportAcknowledgement(ctx context.Context) (*RequestTraceExportRiskAcknowledgement, error) {
	if s == nil || s.settingRepo == nil {
		return nil, ErrRequestTraceSettingsUnavailable
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRequestTraceExportRiskAcknowledgement)
	if errors.Is(err, ErrSettingNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get request trace export acknowledgement: %w", err)
	}
	var ack RequestTraceExportRiskAcknowledgement
	if err := json.Unmarshal([]byte(raw), &ack); err != nil {
		return nil, ErrRequestTraceExportRiskAcknowledgementInvalid
	}
	return &ack, nil
}

// RequestTraceExportAcknowledged 报告当前是否已经接受当前版本的导出风险声明。
// 读取失败或记录损坏一律按"未确认"处理：不能凭一次失败的读取就开放明文导出。
func (s *SettingService) RequestTraceExportAcknowledged(ctx context.Context) bool {
	ack, err := s.readRequestTraceExportAcknowledgement(ctx)
	if err != nil || ack == nil {
		return false
	}
	return ack.CoversCurrentStatement()
}

// GetRequestTraceExportRiskStatus 返回导出确认状态，供管理端渲染声明原文与当前状态。
func (s *SettingService) GetRequestTraceExportRiskStatus(ctx context.Context) (RequestTraceExportRiskStatus, error) {
	if s == nil || s.settingRepo == nil {
		return RequestTraceExportRiskStatus{}, ErrRequestTraceSettingsUnavailable
	}
	status := RequestTraceExportRiskStatus{
		Version:  RequestTraceExportRiskAcknowledgementVersion,
		PhraseEN: RequestTraceExportRiskAcknowledgementPhraseEN,
		PhraseZH: RequestTraceExportRiskAcknowledgementPhraseZH,
	}
	ack, err := s.readRequestTraceExportAcknowledgement(ctx)
	if err != nil {
		return status, err
	}
	if ack != nil {
		status.Phrase = ack.Phrase
		status.AdminUserID = ack.AdminUserID
		if !ack.AcceptedAt.IsZero() {
			status.AcceptedAt = ack.AcceptedAt.UTC().Format(time.RFC3339)
		}
		status.Acknowledged = ack.CoversCurrentStatement()
	}
	return status, nil
}

// AcknowledgeRequestTraceExportRisk 记录一次导出风险确认。
//
// 逐字确认：提交的文本必须与当前版本的声明完全一致（中文或英文其一），
// 否则拒绝写入——这样"接受"始终对应管理员真实读过的内容。
func (s *SettingService) AcknowledgeRequestTraceExportRisk(ctx context.Context, input RequestTraceExportRiskUpdateInput) (RequestTraceExportRiskStatus, error) {
	if s == nil || s.settingRepo == nil {
		return RequestTraceExportRiskStatus{}, ErrRequestTraceSettingsUnavailable
	}
	if input.AdminUserID <= 0 {
		return RequestTraceExportRiskStatus{}, ErrRequestTraceOperatorIdentityRequired
	}
	phrase := RequestTraceExportRiskAcknowledgementPhraseEN
	if normalizeAdminComplianceLanguage(input.Language) == "zh" {
		phrase = RequestTraceExportRiskAcknowledgementPhraseZH
	}
	if strings.TrimSpace(input.Phrase) == "" {
		return RequestTraceExportRiskStatus{}, ErrRequestTraceExportRiskAcknowledgementRequired
	}
	if input.Phrase != phrase {
		return RequestTraceExportRiskStatus{}, ErrRequestTraceExportRiskAcknowledgementInvalid
	}
	ack := RequestTraceExportRiskAcknowledgement{
		Version: RequestTraceExportRiskAcknowledgementVersion, Phrase: input.Phrase,
		AdminUserID: input.AdminUserID, IPAddress: strings.TrimSpace(input.IPAddress),
		UserAgent: strings.TrimSpace(input.UserAgent), AcceptedAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		return RequestTraceExportRiskStatus{}, ErrRequestTraceExportRiskAcknowledgementInvalid
	}
	if err := s.settingRepo.Set(ctx, SettingKeyRequestTraceExportRiskAcknowledgement, string(payload)); err != nil {
		return RequestTraceExportRiskStatus{}, fmt.Errorf("save request trace export acknowledgement: %w", err)
	}
	slog.Info("request_trace.export_risk_acknowledged", "audit", true,
		"version", RequestTraceExportRiskAcknowledgementVersion, "admin_user_id", input.AdminUserID)
	return s.GetRequestTraceExportRiskStatus(ctx)
}
