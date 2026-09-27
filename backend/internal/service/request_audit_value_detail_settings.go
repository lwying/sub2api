package service

// 值明细旁路的运维门控与书面风险确认（ADR 0006）。
//
// 与 429 错误诊断（ADR 0005）同一套路，且刻意保持同一形状：布尔字段无法证明「写过」，
// 因此把开启动作绑定到一条持久确认记录上——操作员必须逐字输入当前版本的确认语句，
// 服务端每次更新都重新校验，并把语句原文、版本、确认时间与管理员 ID 一起落库。
// 没有这条记录（或版本不符／原文不符）就没有可审计的书面确认。
//
// 判定点分两层：
//   - 采集侧：RequestAuditValueDetailGate 将存量开关与当前版本逐字确认结合；
//     旧密钥可用性仅决定旧密文是否可读，不控制新明文采集。
//   - 运维界面：GetRequestAuditValueDetailOperatorStatus 同时展示存量值与校验结论，
//     使越权改写（直接写库、其它管理入口、历史脏数据）呈现为
//     「存量开着、但不允许采集」，而不是一个无法解释的开启。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	// SettingKeyRequestAuditValueDetail 保存值明细的采集开关与操作员风险确认布尔位。
	SettingKeyRequestAuditValueDetail = "request_audit_value_detail_settings"

	// SettingKeyRequestAuditValueDetailRiskAcknowledgement 保存最后一次书面风险确认的记录。
	//
	// 它与门控键分开存放：门控可以被改回关闭，而书面确认是已经发生过的审计事实。
	SettingKeyRequestAuditValueDetailRiskAcknowledgement = "request_audit_value_detail_risk_acknowledgement"

	// RequestAuditValueDetailRiskAcknowledgementVersion 是确认语句的版本。
	//
	// 语句内容变化时必须提升版本：已开启的部署不会自动继承新语句，操作员必须在下一次
	// 更新时按新语句重新逐字确认（服务端每次更新都校验）。
	RequestAuditValueDetailRiskAcknowledgementVersion = "v2026.09.27"

	// RequestAuditValueDetailRiskAcknowledgementPhraseEN / ZH 是新版明文
	// 采集所需的逐字确认。旧加密记录保留原七天到期时刻，新的明文值
	// 随所属 usage 生存，若 usage 清理关闭则可能无限期可读；备份与副本
	// 仍可能保留旧值，本功能不是合规删除或数据主体擦除。
	RequestAuditValueDetailRiskAcknowledgementPhraseEN = "New request-audit value details from actually audited Messages, Chat Completions and Responses requests are stored in plaintext for as long as the owning usage record exists, with no independent expiry; if usage cleanup is disabled they may remain readable indefinitely. Retained data includes allowlisted client and upstream header values, bounded device and session identifiers and caller-controlled model aliases. Even allowlisted values and model aliases can contain secrets or short prompt-like text that filtering cannot recognise. One upstream 429 can also be retained separately in error diagnostics. Legacy encrypted details keep their original seven-day expiry. Copies in replicas, backups, PITR and manual exports may persist after usage deletion. This feature is not a compliance deletion or data-subject erasure tool."
	RequestAuditValueDetailRiskAcknowledgementPhraseZH = "新请求审计值明细对实际逐次审计的 Messages、Chat Completions、Responses 请求以明文存储，并完全随所属使用记录保留，无独立到期时间；若停用使用记录清理，明文可能无限期可读。留存内容包括白名单内的客户端与上游头值、有界设备及会话标识、调用方可控的模型别名；允许的值与模型别名仍可能含过滤无法识别的秘密或短提示词片段。同一次上游 429 还可能在独立错误诊断中另行留存。旧加密值明细仍在原七天到期。只读副本、备份、PITR 与人工导出中的副本可能在使用记录删除后仍保留；本功能不是合规删除或数据主体擦除手段。"
)

var (
	// ErrRequestAuditValueDetailRiskAcknowledgementRequired 表示请求未携带书面风险确认语句。
	ErrRequestAuditValueDetailRiskAcknowledgementRequired = infraerrors.BadRequest(
		"REQUEST_AUDIT_VALUE_DETAIL_RISK_ACK_REQUIRED",
		"enabling request audit value details requires the written risk acknowledgement phrase",
	)
	// ErrRequestAuditValueDetailRiskAcknowledgementInvalid 表示确认语句与当前版本不符。
	ErrRequestAuditValueDetailRiskAcknowledgementInvalid = infraerrors.BadRequest(
		"REQUEST_AUDIT_VALUE_DETAIL_RISK_ACK_INVALID",
		"the risk acknowledgement phrase does not match the required statement",
	)
	// ErrRequestAuditValueDetailKeyUnavailable 表示缺少可用于留存值明细的稳定密钥。
	ErrRequestAuditValueDetailKeyUnavailable = infraerrors.BadRequest(
		"REQUEST_AUDIT_VALUE_DETAIL_KEY_UNAVAILABLE",
		"request audit value details require a configured, restart-stable encryption key",
	)
	// ErrRequestAuditValueDetailOperatorIdentityRequired 表示没有可记录的管理员身份。
	ErrRequestAuditValueDetailOperatorIdentityRequired = infraerrors.Forbidden(
		"REQUEST_AUDIT_VALUE_DETAIL_OPERATOR_SESSION_REQUIRED",
		"enabling request audit value details requires an authenticated admin identity to record the acknowledgement",
	)
	// ErrRequestAuditValueDetailSettingsUnavailable 表示设置服务不可用。
	ErrRequestAuditValueDetailSettingsUnavailable = infraerrors.InternalServer(
		"REQUEST_AUDIT_VALUE_DETAIL_SETTINGS_UNAVAILABLE",
		"request audit value detail settings are unavailable",
	)
)

// RequestAuditValueDetailSettings 是**存量**门控配置。
type RequestAuditValueDetailSettings struct {
	Enabled          bool `json:"enabled"`
	RiskAcknowledged bool `json:"risk_acknowledged"`
}

// RequestAuditValueDetailRiskAcknowledgement 是一次书面风险确认的持久记录。
//
// Phrase 保存的是**确认当时的语句原文**：语句常量以后改动也不会改写这条证据，
// 审计时能看出操作员究竟确认了哪段文字。
type RequestAuditValueDetailRiskAcknowledgement struct {
	Version     string    `json:"version"`
	Phrase      string    `json:"phrase"`
	AdminUserID int64     `json:"admin_user_id"`
	IPAddress   string    `json:"ip_address,omitempty"`
	UserAgent   string    `json:"user_agent,omitempty"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

// CoversCurrentStatement 报告这条记录是否就是**当前版本、且逐字记录了当前语句**的确认。
//
// 版本、管理员 ID 与确认时间只能证明「库里有一条形状完整的记录」，不能证明操作员确认过
// 当前这段文字：直接写库完全可以落一条版本正确而原文任意的记录。因此语句原文必须与
// 当前版本的两条语句之一**逐字**相同。两种语言各自按自己的原文确认，语言不是证据的一部分。
func (a RequestAuditValueDetailRiskAcknowledgement) CoversCurrentStatement() bool {
	if a.Version != RequestAuditValueDetailRiskAcknowledgementVersion {
		return false
	}
	return a.Phrase == RequestAuditValueDetailRiskAcknowledgementPhraseEN ||
		a.Phrase == RequestAuditValueDetailRiskAcknowledgementPhraseZH
}

// RequestAuditValueDetailRiskAcknowledgementView 是回显给管理员界面的确认记录视图。
//
// 只含审计必要的四项：来源 IP 与 User-Agent 只留在持久记录里，不回显到响应面。
type RequestAuditValueDetailRiskAcknowledgementView struct {
	Version     string    `json:"version"`
	Phrase      string    `json:"phrase"`
	AdminUserID int64     `json:"admin_user_id"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

// RequestAuditValueDetailOperatorStatus 是运维开关的当前状态（管理员可见）。
//
// 三个布尔字段（Enabled／RiskAcknowledged／EncryptionKeyAvailable）如实回显事实，
// 而 CaptureAllowed 是**校验后**的结论：它同时要求存量布尔值与一条覆盖当前语句版本的
// 有效书面确认。因此存量被越权改写时界面会明确显示不可采集。
type RequestAuditValueDetailOperatorStatus struct {
	Enabled                bool                                            `json:"enabled"`
	RiskAcknowledged       bool                                            `json:"risk_acknowledged"`
	CaptureAllowed         bool                                            `json:"capture_allowed"`
	EncryptionKeyAvailable bool                                            `json:"encryption_key_available"`
	RiskVersion            string                                          `json:"risk_version"`
	RiskPhraseEN           string                                          `json:"risk_phrase_en"`
	RiskPhraseZH           string                                          `json:"risk_phrase_zh"`
	RiskAcknowledgement    *RequestAuditValueDetailRiskAcknowledgementView `json:"risk_acknowledgement,omitempty"`
	// RiskAcknowledgementCurrent 报告在库的记录是否覆盖当前语句：版本匹配且原文逐字相同。
	RiskAcknowledgementCurrent bool `json:"risk_acknowledgement_current"`

	// PlaintextCaptureSupported／PlaintextCaptureSupportReason 是**部署前提**的结论（ADR 0007；
	// 票据 10）：本部署的数据库能不能保证「明文随 usage 消失」。
	//
	// 两者与存量开关、书面确认互相独立：它们解释的是「存量开着、确认也在，为什么仍然
	// 不允许采集」。Reason 是闭集原因码（分区与否、所有权外键是否还在、探针是否查得出来），
	// 绝不回显数据库错误原文。关闭不需要这个前提，所以关闭态永远是可行的。
	PlaintextCaptureSupported     bool   `json:"plaintext_capture_supported"`
	PlaintextCaptureSupportReason string `json:"plaintext_capture_support_reason"`
}

// RequestAuditValueDetailOperatorUpdateInput 是一次运维开关更新请求。
type RequestAuditValueDetailOperatorUpdateInput struct {
	Enabled     bool
	Language    string
	Phrase      string
	AdminUserID int64
	IPAddress   string
	UserAgent   string
}

// expectedRequestAuditValueDetailRiskPhrase 返回该语言下必须逐字输入的确认语句。
//
// 语言归一复用管理员合规确认的同一规则（zh* → zh，其余 → en），
// 保证同一批双语确认入口对 language 的解释一致。
func expectedRequestAuditValueDetailRiskPhrase(language string) string {
	if normalizeAdminComplianceLanguage(language) == "zh" {
		return RequestAuditValueDetailRiskAcknowledgementPhraseZH
	}
	return RequestAuditValueDetailRiskAcknowledgementPhraseEN
}

// ReadStoredRequestAuditValueDetailSettings 读取**存量**门控配置，不做书面确认校验。
//
// 仅供运维状态展示「存了什么」。采集侧一律使用 RequestAuditValueDetailGate。
func (s *SettingService) ReadStoredRequestAuditValueDetailSettings(ctx context.Context) (RequestAuditValueDetailSettings, error) {
	if s == nil || s.settingRepo == nil {
		return RequestAuditValueDetailSettings{}, ErrRequestAuditValueDetailSettingsUnavailable
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRequestAuditValueDetail)
	if errors.Is(err, ErrSettingNotFound) {
		return RequestAuditValueDetailSettings{}, nil
	}
	if err != nil {
		return RequestAuditValueDetailSettings{}, fmt.Errorf("get request audit value detail settings: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return RequestAuditValueDetailSettings{}, nil
	}
	var settings RequestAuditValueDetailSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		// 解不开的设置按「全关」处理，绝不按字段默认值当成开启。
		return RequestAuditValueDetailSettings{}, nil
	}
	return settings, nil
}

// getRequestAuditValueDetailRiskAcknowledgement 读取在库的书面确认记录。
//
// 没有记录、记录损坏或缺少版本／管理员 ID／确认时间时都返回「没有有效确认」：
// 残缺的记录不是证据。语句原文不在这里判定（由 CoversCurrentStatement 负责），
// 使界面能如实展示「库里存了什么」，而校验结论只有一个来源。
func (s *SettingService) getRequestAuditValueDetailRiskAcknowledgement(ctx context.Context) (*RequestAuditValueDetailRiskAcknowledgement, error) {
	if s == nil || s.settingRepo == nil {
		return nil, ErrRequestAuditValueDetailSettingsUnavailable
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRequestAuditValueDetailRiskAcknowledgement)
	if errors.Is(err, ErrSettingNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get request audit value detail risk acknowledgement: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var ack RequestAuditValueDetailRiskAcknowledgement
	if err := json.Unmarshal([]byte(raw), &ack); err != nil {
		return nil, fmt.Errorf("decode request audit value detail risk acknowledgement: %w", err)
	}
	if strings.TrimSpace(ack.Version) == "" || ack.AdminUserID <= 0 || ack.AcceptedAt.IsZero() {
		return nil, nil
	}
	return &ack, nil
}

// RequestAuditValueDetailRiskAcknowledgementCurrent 报告是否存在覆盖当前语句版本的有效书面确认。
func (s *SettingService) RequestAuditValueDetailRiskAcknowledgementCurrent(ctx context.Context) (bool, error) {
	ack, err := s.getRequestAuditValueDetailRiskAcknowledgement(ctx)
	if err != nil {
		return false, err
	}
	return ack != nil && ack.CoversCurrentStatement(), nil
}

// RequestAuditValueDetailEncryptionKeyAvailable reports whether legacy encrypted rows can be decrypted.
//
// 判定与加密器一致：主密钥必须是非空、合法 hex、32 字节（AES-256）密钥；
// 并且必须是运维**显式配置**的密钥——启动时随机生成的密钥每次重启都会变，
// 已留存密文将永久不可解；此状态只影响旧行读取。
func (s *SettingService) RequestAuditValueDetailEncryptionKeyAvailable() bool {
	if s == nil || s.cfg == nil || !s.cfg.Totp.EncryptionKeyConfigured {
		return false
	}
	raw, err := hex.DecodeString(strings.TrimSpace(s.cfg.Totp.EncryptionKey))
	if err != nil {
		return false
	}
	return len(raw) == 32
}

// RequestAuditValueDetailGate combines the stored switch and written acknowledgement;
// key availability is reported independently for legacy reads.
//
// fail closed，且不在热路径上抛错：
//   - 存量布尔值为假时不读取确认键（关闭态不依赖确认记录，也不产生额外读）；
//   - 读取器或存储不可用、确认记录读不到或读出错，一律按全关返回；
//   - 缺旧密钥仅影响旧密文读取，不阻断已确认的新明文采集。
func (s *SettingService) RequestAuditValueDetailGate(ctx context.Context) RequestAuditValueDetailGate {
	gate := RequestAuditValueDetailGate{}
	if s == nil || s.settingRepo == nil {
		return gate
	}
	stored, err := s.ReadStoredRequestAuditValueDetailSettings(ctx)
	if err != nil || !stored.Enabled || !stored.RiskAcknowledged {
		return gate
	}
	current, err := s.RequestAuditValueDetailRiskAcknowledgementCurrent(ctx)
	if err != nil || !current {
		return gate
	}
	// 旧密文可读性与本门控无关：它只回答「旧行还能不能解密」，新明文不需要密钥。
	gate.EncryptionAvailable = s.RequestAuditValueDetailEncryptionKeyAvailable()

	// ADR 0007 / 票据 10：新值明细一律以**明文**写入，并随所属 usage 删除。这条门控因此
	// 同时是「部署前提」的门控——数据库不能保证明文随 usage 消失时（分区 usage_logs、
	// 所有权外键缺失、探针查不出来），一个字节的新明文都不写。旧密文读取不经过这里。
	if !s.PlaintextCaptureSupport(ctx).Supported {
		return gate
	}

	gate.CaptureAllowed = true
	return gate
}

var _ RequestAuditValueDetailGateReader = (*SettingService)(nil)

// GetRequestAuditValueDetailOperatorStatus 读取运维开关状态。
//
// 同时给出存量值与校验结论：读取失败返回错误，绝不用「全关」掩盖存储故障；
// 确认键读不到时同样报错，让界面显示「确认记录读不出来」，而不是把故障显示成「未确认」。
func (s *SettingService) GetRequestAuditValueDetailOperatorStatus(ctx context.Context) (RequestAuditValueDetailOperatorStatus, error) {
	status := RequestAuditValueDetailOperatorStatus{
		RiskVersion:  RequestAuditValueDetailRiskAcknowledgementVersion,
		RiskPhraseEN: RequestAuditValueDetailRiskAcknowledgementPhraseEN,
		RiskPhraseZH: RequestAuditValueDetailRiskAcknowledgementPhraseZH,
	}
	if s == nil || s.settingRepo == nil {
		return RequestAuditValueDetailOperatorStatus{}, ErrRequestAuditValueDetailSettingsUnavailable
	}
	stored, err := s.ReadStoredRequestAuditValueDetailSettings(ctx)
	if err != nil {
		return RequestAuditValueDetailOperatorStatus{}, err
	}
	ack, err := s.getRequestAuditValueDetailRiskAcknowledgement(ctx)
	if err != nil {
		return RequestAuditValueDetailOperatorStatus{}, err
	}
	status.Enabled = stored.Enabled
	status.RiskAcknowledged = stored.RiskAcknowledged
	if ack != nil {
		status.RiskAcknowledgement = &RequestAuditValueDetailRiskAcknowledgementView{
			Version:     ack.Version,
			Phrase:      ack.Phrase,
			AdminUserID: ack.AdminUserID,
			AcceptedAt:  ack.AcceptedAt,
		}
		status.RiskAcknowledgementCurrent = ack.CoversCurrentStatement()
	}
	status.EncryptionKeyAvailable = s.RequestAuditValueDetailEncryptionKeyAvailable()
	// 校验结论复用采集侧同一判定，不在这里另写一份规则。
	gate := s.RequestAuditValueDetailGate(ctx)
	status.CaptureAllowed = gate.CaptureAllowed
	// 部署前提单独回显且**与门控同源**（同一个带缓存的结论），因此界面上不会出现
	// 「结论说不能采集、理由说受支持」这种自相矛盾的状态。
	support := s.PlaintextCaptureSupport(ctx)
	status.PlaintextCaptureSupported = support.Supported
	status.PlaintextCaptureSupportReason = support.Reason
	return status, nil
}

// UpdateRequestAuditValueDetailOperatorSettings 应用一次运维开关更新。
//
// 规则：
//   - 结果状态为开启时，**每次**更新都必须重新逐字确认当前语句，并记录管理员 ID；
//     已有布尔值不能代替本次确认（避免通过「已确认过」绕过门槛）。
//   - 新明文值不依赖加密密钥；已有加密记录的解密仍要求原稳定密钥。
//   - 关闭永远允许，不需要确认或身份，也不能被任何前置校验挡住（兜底方向必须总能关）。
//
// 校验全部通过后才写入：确认记录与门控在同一个多次 upsert 中落库，失败不会留下
// 「门控已开但没有确认记录」的状态。
func (s *SettingService) UpdateRequestAuditValueDetailOperatorSettings(ctx context.Context, input RequestAuditValueDetailOperatorUpdateInput) (RequestAuditValueDetailOperatorStatus, error) {
	if s == nil || s.settingRepo == nil {
		return RequestAuditValueDetailOperatorStatus{}, ErrRequestAuditValueDetailSettingsUnavailable
	}
	settings := RequestAuditValueDetailSettings{Enabled: input.Enabled}
	updates := map[string]string{}

	if input.Enabled {
		if input.AdminUserID <= 0 {
			return RequestAuditValueDetailOperatorStatus{}, ErrRequestAuditValueDetailOperatorIdentityRequired
		}
		phrase := strings.TrimSpace(input.Phrase)
		if phrase == "" {
			return RequestAuditValueDetailOperatorStatus{}, ErrRequestAuditValueDetailRiskAcknowledgementRequired
		}
		if phrase != expectedRequestAuditValueDetailRiskPhrase(input.Language) {
			return RequestAuditValueDetailOperatorStatus{}, ErrRequestAuditValueDetailRiskAcknowledgementInvalid
		}
		// 部署前提：数据库保证不了「明文随 usage 消失」时不允许开启。这一步用**绕过缓存**的
		// 探针（见 rejectPlaintextCaptureEnablement）——缓存里的旧结论不能替数据库放行一次
		// 不可逆的开启。关闭路径根本走不到这里，因此紧急关闭永远不被它挡住。
		if err := s.rejectPlaintextCaptureEnablement(ctx, ErrRequestAuditValueDetailDeploymentUnsupported); err != nil {
			return RequestAuditValueDetailOperatorStatus{}, err
		}
		settings.RiskAcknowledged = true

		ack := RequestAuditValueDetailRiskAcknowledgement{
			Version:     RequestAuditValueDetailRiskAcknowledgementVersion,
			Phrase:      phrase,
			AdminUserID: input.AdminUserID,
			IPAddress:   strings.TrimSpace(input.IPAddress),
			UserAgent:   strings.TrimSpace(input.UserAgent),
			AcceptedAt:  time.Now().UTC(),
		}
		payload, err := json.Marshal(ack)
		if err != nil {
			return RequestAuditValueDetailOperatorStatus{}, fmt.Errorf("marshal request audit value detail risk acknowledgement: %w", err)
		}
		updates[SettingKeyRequestAuditValueDetailRiskAcknowledgement] = string(payload)
	}

	settingsPayload, err := json.Marshal(settings)
	if err != nil {
		return RequestAuditValueDetailOperatorStatus{}, fmt.Errorf("marshal request audit value detail settings: %w", err)
	}
	updates[SettingKeyRequestAuditValueDetail] = string(settingsPayload)

	if err := s.settingRepo.SetMultiple(ctx, updates); err != nil {
		return RequestAuditValueDetailOperatorStatus{}, fmt.Errorf("save request audit value detail operator settings: %w", err)
	}

	// 开关事件必须可审计（谁、把什么打开／关掉、确认了哪个版本），
	// 但确认语句原文只落库、不进日志。
	slog.Info("request_audit_value_detail.operator_settings_updated",
		"audit", true,
		"enabled", settings.Enabled,
		"risk_version", RequestAuditValueDetailRiskAcknowledgementVersion,
		"admin_user_id", input.AdminUserID,
	)

	return s.GetRequestAuditValueDetailOperatorStatus(ctx)
}
