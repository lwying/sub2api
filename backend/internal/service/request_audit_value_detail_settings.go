package service

// 值明细旁路的运维门控与书面风险确认（ADR 0006）。
//
// 与 429 错误诊断（ADR 0005）同一套路，且刻意保持同一形状：布尔字段无法证明「写过」，
// 因此把开启动作绑定到一条持久确认记录上——操作员必须逐字输入当前版本的确认语句，
// 服务端每次更新都重新校验，并把语句原文、版本、确认时间与管理员 ID 一起落库。
// 没有这条记录（或版本不符／原文不符）就没有可审计的书面确认。
//
// 判定点分两层：
//   - 采集侧：RequestAuditValueDetailGate 在存量布尔值之上并入书面确认结论与密钥可用性；
//     没有覆盖当前语句版本的有效确认就没有采集。
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
	RequestAuditValueDetailRiskAcknowledgementVersion = "v2026.09.24"

	// RequestAuditValueDetailRiskAcknowledgementPhraseEN / ZH 是必须逐字输入的确认语句。
	//
	// 语句按 ADR 0006 声明剩余风险：值加密保留 7 天；到期后 API 读取立即失效，
	// 而物理删除只在在线主库由周期清理执行（约每 10 分钟一轮、每轮至多 500 行），
	// 积压或停机会使其延迟且**不保证上限**；留存内容是白名单内的头值、
	// metadata.user_id 组件与模型名，仍可能含过滤无法识别的标识；
	// 旧副本／备份／PITR／人工导出中的值仍可能留存且可恢复；
	// 本功能不是合规删除或数据主体擦除手段。
	RequestAuditValueDetailRiskAcknowledgementPhraseEN = "Claude /v1/messages request-audit value details are retained encrypted for 7 days; at expiry API reads are rejected immediately, while physical deletion is performed only in the online primary database by periodic cleanup (about every 10 minutes, at most 500 rows per round) that backlog or downtime can delay without a guaranteed maximum; retained content is allowlisted client and upstream header values, parsed metadata.user_id components and model names, and may contain identifiers that filtering cannot recognise; copies in replicas, backups, PITR and manual exports may persist and can be recovered there, and this feature is not a compliance deletion or data-subject erasure tool."
	RequestAuditValueDetailRiskAcknowledgementPhraseZH = "Claude /v1/messages 请求审计值明细以加密形式保留 7 天；到期后立即拒绝 API 读取，但物理删除只在在线主库由周期清理执行（约每 10 分钟一轮、每轮至多 500 行），积压或停机可能使其延迟且不保证上限；留存内容为白名单内的客户端与上游头值、解析出的 metadata.user_id 组件与模型名，可能含过滤无法识别的标识；只读副本、备份、PITR 与人工导出中的旧数据可能仍留存且可恢复，本功能不是合规删除或数据主体擦除手段。"
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

// RequestAuditValueDetailEncryptionKeyAvailable 报告是否具备可用于值明细留存的密钥。
//
// 判定与加密器一致：主密钥必须是非空、合法 hex、32 字节（AES-256）密钥；
// 并且必须是运维**显式配置**的密钥——启动时随机生成的密钥每次重启都会变，
// 已留存密文将永久不可解，因此这种部署不允许打开值明细留存。这里只回答「能不能用」。
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

// RequestAuditValueDetailGate 读取**有效**门控结论：存量布尔值、书面确认与密钥可用性
// 合并后的结果。这是采集侧的唯一判定点。
//
// fail closed，且不在热路径上抛错：
//   - 存量布尔值为假时不读取确认键（关闭态不依赖确认记录，也不产生额外读）；
//   - 读取器或存储不可用、确认记录读不到或读出错，一律按全关返回；
//   - 密钥不可用时 EncryptionAvailable 为假，值一律不留存（但「允许采集」这一事实仍然保留，
//     使运维界面能区分「没开」与「开了但缺密钥」）。
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
	gate.CaptureAllowed = true
	gate.EncryptionAvailable = s.RequestAuditValueDetailEncryptionKeyAvailable()
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
	return status, nil
}

// UpdateRequestAuditValueDetailOperatorSettings 应用一次运维开关更新。
//
// 规则：
//   - 结果状态为开启时，**每次**更新都必须重新逐字确认当前语句，并记录管理员 ID；
//     已有布尔值不能代替本次确认（避免通过「已确认过」绕过门槛）。
//   - 开启还要求此刻有可用的稳定密钥：否则每一次采集都注定被判为
//     skipped_encryption_unavailable，那是把配置故障显示成正常运行。
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
		if !s.RequestAuditValueDetailEncryptionKeyAvailable() {
			return RequestAuditValueDetailOperatorStatus{}, ErrRequestAuditValueDetailKeyUnavailable
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
