package service

// 独立错误诊断**新明文层**的运维门控（ADR 0007 决定 4；票据 08 与 09）。
//
// 新层与旧层是两套确认，不是同一份确认的两种读法：
//
//   - 升级前共享确认（SettingKeyErrorDiagnosticRiskAcknowledgement，v2026.09.24.1）覆盖的是
//     「密文留存 7 天、缺密钥即不留」这件事。升级后共享版本已提升；按旧语句做过的确认
//     不能继续启动新采集，更不覆盖「明文留在库里、随 usage 删除、未关联三十天」的新事实。
//   - 新正文层与新 429 头值层各自有一条覆盖自己的语句版本与逐字原文的确认记录，
//     两者互相独立：任何一层的确认都不能打开另一层，关闭任何一层也不影响另一层。
//
// 门槛形状与既有两层同形（存量开关 + 覆盖当前语句版本的有效确认），但**没有密钥这一环**：
// 新格式不加密，要求一个稳定密钥既说不通，也会把「明文本来就能落库」显示成配置故障。
// 采集本身（enabled + 旧共享确认）仍是它们的前置门槛：没有元数据行就没有留存的载体。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	// SettingKeyErrorDiagnosticPlainBodyRiskAck 保存新明文正文层的书面风险确认记录。
	SettingKeyErrorDiagnosticPlainBodyRiskAck = "error_diagnostic_plain_body_risk_acknowledgement"

	// SettingKeyErrorDiagnosticPlainHeaderValuesRiskAck 保存新明文 429 头值层的书面风险确认记录。
	SettingKeyErrorDiagnosticPlainHeaderValuesRiskAck = "error_diagnostic_plain_header_values_risk_acknowledgement"

	// ErrorDiagnosticPlainBodyRiskAcknowledgementVersion 是新明文正文层确认语句的版本。
	//
	// 语句内容变化时必须提升版本：按旧语句做过的确认不再覆盖当前语句，
	// 已开启的部署会被判为「确认过期」，必须重新逐字确认后才继续采集。
	ErrorDiagnosticPlainBodyRiskAcknowledgementVersion = "v2026.09.27.1"

	// ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementVersion 是新明文 429 头值层
	// 确认语句的版本（与正文层各自独立）。
	ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementVersion = "v2026.09.27.1"

	// ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseEN / ZH 是必须逐字输入的语句。
	//
	// 语句按 ADR 0007 声明新层的剩余风险：正文以**明文**留在数据库里（没有加密、没有密钥
	// 保护），数据库读者、只读副本、备份、PITR 与人工导出都能直接读到或恢复它；可靠关联到
	// 使用记录后随该使用记录删除，未能关联时第三十天整点立即拒绝 API 读取，而物理删除只在
	// 在线主库由周期清理执行，积压或停机会推迟它且**不保证**任何最大延迟；使用记录清理关闭时
	// 已关联的明文没有时间上限；留存正文是任意客户端文本，可能含过滤无法识别的凭据；
	// 本功能不是合规删除或数据主体擦除手段。
	ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseEN = "Error diagnostic request bodies may be retained as plaintext (unencrypted, with no key protecting them) inside the database; anything that can read the database, its read replicas, backups, PITR or manual exports can read or recover it; a row reliably linked to a usage record is deleted with that usage record, an unlinked row stops being readable through the API at exactly 30 days while its physical deletion is performed only in the online primary database by periodic cleanup that backlog or downtime can delay without a guaranteed maximum; when usage retention is disabled, linked plaintext has no fixed maximum lifetime; retained text is arbitrary client content and may contain credentials that filtering cannot identify; this feature is not a compliance deletion or data-subject erasure tool."
	ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseZH = "错误诊断请求正文可能以明文（未经加密、没有密钥保护）留在数据库中；能读取数据库及其只读副本、备份、PITR 或人工导出的一方都能读到或恢复它；已可靠关联到使用记录的行随该使用记录删除，未关联的行在第三十天整点起立即拒绝 API 读取，而物理删除只在在线主库由周期清理执行，积压或停机可能使其延迟且不保证上限；使用记录清理关闭时已关联的明文没有时间上限；留存正文是任意客户端文本，可能含过滤无法识别的凭据；本功能不是合规删除或数据主体擦除手段。"

	// ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementPhraseEN / ZH 覆盖 429 头值这一层：
	// 同样是明文、同样随 usage 或三十天，但只采自白名单，绝不含凭据头。
	ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementPhraseEN = "Error diagnostic 429 header values may be retained as plaintext (unencrypted, with no key protecting them) inside the database; anything that can read the database, its read replicas, backups, PITR or manual exports can read or recover it; values are captured only from an allowlist that excludes credential headers such as Authorization, Cookie, Set-Cookie and X-Api-Key, and header names outside that list are never captured, while secrets hidden inside an allowlisted value cannot be identified automatically; a row reliably linked to a usage record is deleted with that usage record, an unlinked row stops being readable through the API at exactly 30 days while its physical deletion is performed only in the online primary database by periodic cleanup that backlog or downtime can delay without a guaranteed maximum; when usage retention is disabled, linked plaintext has no fixed maximum lifetime; this feature is not a compliance deletion or data-subject erasure tool."
	ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementPhraseZH = "错误诊断的 429 头值可能以明文（未经加密、没有密钥保护）留在数据库中；能读取数据库及其只读副本、备份、PITR 或人工导出的一方都能读到或恢复它；取值只采自白名单，绝不含 Authorization、Cookie、Set-Cookie、X-Api-Key 等凭据头，名单之外的头名一律不采，而藏在合格取值里的秘密无法被自动识别；已可靠关联到使用记录的行随该使用记录删除，未关联的行在第三十天整点起立即拒绝 API 读取，而物理删除只在在线主库由周期清理执行，积压或停机可能使其延迟且不保证上限；使用记录清理关闭时已关联的明文没有时间上限；本功能不是合规删除或数据主体擦除手段。"
)

var (
	// ErrErrorDiagnosticPlainBodyRiskAcknowledgementInvalid 表示新明文正文层的确认语句与
	// 当前版本不符（缺语句、用错层的语句、或旧共享语句）。
	ErrErrorDiagnosticPlainBodyRiskAcknowledgementInvalid = infraerrors.BadRequest(
		"ERROR_DIAGNOSTIC_PLAIN_BODY_RISK_ACK_INVALID",
		"enabling plaintext error diagnostic bodies requires the current plaintext risk acknowledgement phrase",
	)
	// ErrErrorDiagnosticPlainHeaderValuesRiskAcknowledgementInvalid 同形，但属于头值层：
	// 两层各有各的语句，不能互相代替。
	ErrErrorDiagnosticPlainHeaderValuesRiskAcknowledgementInvalid = infraerrors.BadRequest(
		"ERROR_DIAGNOSTIC_PLAIN_HEADER_RISK_ACK_INVALID",
		"enabling plaintext 429 header values requires the current plaintext risk acknowledgement phrase",
	)
)

// ErrorDiagnosticPlaintextRiskAcknowledgementReader 报告两个新明文层的书面确认是否覆盖
// 当前语句版本。只返回布尔结论：操作员身份等 PII 不进入采集侧。
type ErrorDiagnosticPlaintextRiskAcknowledgementReader interface {
	ErrorDiagnosticPlainBodyRiskAcknowledgementCurrent(ctx context.Context) (bool, error)
	ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementCurrent(ctx context.Context) (bool, error)
}

var _ ErrorDiagnosticPlaintextRiskAcknowledgementReader = (*SettingService)(nil)

// ApplyErrorDiagnosticPlaintextRiskAcknowledgement 把两个新层的书面确认并入采集门控结论。
//
// 与旧层的合并函数同形但**互相独立**：旧层缺失不会关闭新层，新层缺失也不会影响旧层；
// 只有「存量布尔值为真」并且「存在覆盖当前语句版本的有效确认」时才报告该层可采集。
// 因此直接改库把布尔值写成真不足以打开新明文，旧共享确认真实存在也不够。
//
// fail closed：布尔值为假时不读确认键（关闭态不产生额外读）；读取器缺失或读取失败时把
// 该层的有效值收窄为 false，且不把错误交给采集热路径。
func ApplyErrorDiagnosticPlaintextRiskAcknowledgement(ctx context.Context, settings ErrorDiagnosticSettings, reader ErrorDiagnosticPlaintextRiskAcknowledgementReader) ErrorDiagnosticSettings {
	if !settings.PlainBodyRetentionEnabled && !settings.PlainHeaderValueRetentionEnabled {
		return settings
	}
	if reader == nil {
		settings.PlainBodyRetentionEnabled = false
		settings.PlainHeaderValueRetentionEnabled = false
		return settings
	}
	if settings.PlainBodyRetentionEnabled {
		current, err := reader.ErrorDiagnosticPlainBodyRiskAcknowledgementCurrent(ctx)
		if err != nil || !current {
			settings.PlainBodyRetentionEnabled = false
		}
	}
	if settings.PlainHeaderValueRetentionEnabled {
		current, err := reader.ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementCurrent(ctx)
		if err != nil || !current {
			settings.PlainHeaderValueRetentionEnabled = false
		}
	}
	return settings
}

// ApplyErrorDiagnosticPlaintextCaptureSupport 把**部署前提**并入两个新明文层的结论。
//
// 前提不存在时（usage_logs 分区、所有权外键缺失、探针查不出来或没接上），两个新明文层的
// 有效值一律收窄为 false：明文一旦写下去，数据库就不能保证它随 usage 消失，而 ADR 0007
// 明确要求这种部署组合在开启前就被阻止，不能用异步孤儿扫描兜底。
//
// 只收窄这两个新层的布尔值：Enabled／RiskAcknowledged（诊断元数据采集）、
// BodyRetentionEnabled／HeaderValueRetentionEnabled（旧密文正文与头值）与所有旧记录的
// 读取都不受部署形态影响——旧格式的七天/三十天期限与形态无关。
//
// fail closed 且不在热路径上抛错：关闭态（两层都未开启）不读探针，因此默认部署零额外成本；
// 探针结论带短 TTL 缓存（见 PlaintextCaptureSupport），失败也只会落到「不支持」。
// 存量的**意图**不在本函数里表达：它要继续回答「运维要求过明文吗」，由存量布尔值本身承担，
// 收窄后的结论与它分开存放，界面因此能说清「开着、但不允许」。
func (s *SettingService) ApplyErrorDiagnosticPlaintextCaptureSupport(ctx context.Context, settings ErrorDiagnosticSettings) ErrorDiagnosticSettings {
	if !settings.PlainBodyRetentionEnabled && !settings.PlainHeaderValueRetentionEnabled {
		return settings
	}
	if s.PlaintextCaptureSupport(ctx).Supported {
		return settings
	}
	settings.PlainBodyRetentionEnabled = false
	settings.PlainHeaderValueRetentionEnabled = false
	return settings
}

// plaintextRiskAcknowledgementSpec 是一层新明文确认的（版本，语句）二元组。
type plaintextRiskAcknowledgementSpec struct {
	Version  string
	PhraseEN string
	PhraseZH string
}

var (
	plainBodyRiskSpec = plaintextRiskAcknowledgementSpec{
		Version:  ErrorDiagnosticPlainBodyRiskAcknowledgementVersion,
		PhraseEN: ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseEN,
		PhraseZH: ErrorDiagnosticPlainBodyRiskAcknowledgementPhraseZH,
	}
	plainHeaderRiskSpec = plaintextRiskAcknowledgementSpec{
		Version:  ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementVersion,
		PhraseEN: ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementPhraseEN,
		PhraseZH: ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementPhraseZH,
	}
)

// expectedPlainRiskPhrase 返回该语言下必须逐字输入的语句。
//
// 语言归一复用管理员合规确认的同一规则（zh* → zh，其余 → en），与旧层一致。
func (s plaintextRiskAcknowledgementSpec) expectedPhrase(language string) string {
	if normalizeAdminComplianceLanguage(language) == "zh" {
		return s.PhraseZH
	}
	return s.PhraseEN
}

// covers 报告一条已持久化的确认是否覆盖本层的当前语句。
//
// 版本与逐字原文缺一不可：只校验版本会让一条任意原文的记录打开明文留存。
func (s plaintextRiskAcknowledgementSpec) covers(ack *ErrorDiagnosticRiskAcknowledgement) bool {
	if ack == nil || ack.Version != s.Version {
		return false
	}
	return ack.Phrase == s.PhraseEN || ack.Phrase == s.PhraseZH
}

// getErrorDiagnosticRiskAcknowledgementByKey 读取某个确认键下的书面确认记录。
//
// 与旧层的读取函数同一约定：没有记录、记录损坏或缺少版本／管理员 ID／确认时间时都按
// 「没有有效确认」返回 nil，残缺的记录不是证据。语句原文不在这里判定，由 covers 判定，
// 使运维状态能如实展示「库里存了什么」，而校验结论只有一个来源。
func (s *SettingService) getErrorDiagnosticRiskAcknowledgementByKey(ctx context.Context, key string) (*ErrorDiagnosticRiskAcknowledgement, error) {
	raw, err := s.settingRepo.GetValue(ctx, key)
	if errors.Is(err, ErrSettingNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get error diagnostic risk acknowledgement %s: %w", key, err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var ack ErrorDiagnosticRiskAcknowledgement
	if err := json.Unmarshal([]byte(raw), &ack); err != nil {
		return nil, fmt.Errorf("decode error diagnostic risk acknowledgement %s: %w", key, err)
	}
	if strings.TrimSpace(ack.Version) == "" || ack.AdminUserID <= 0 || ack.AcceptedAt.IsZero() {
		return nil, nil
	}
	return &ack, nil
}

// ErrorDiagnosticPlainBodyRiskAcknowledgementCurrent 报告新明文正文层是否有覆盖当前语句的确认。
func (s *SettingService) ErrorDiagnosticPlainBodyRiskAcknowledgementCurrent(ctx context.Context) (bool, error) {
	if s == nil || s.settingRepo == nil {
		return false, ErrErrorDiagnosticSettingServiceUnavailable
	}
	ack, err := s.getErrorDiagnosticRiskAcknowledgementByKey(ctx, SettingKeyErrorDiagnosticPlainBodyRiskAck)
	if err != nil {
		return false, err
	}
	return plainBodyRiskSpec.covers(ack), nil
}

// ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementCurrent 报告新明文 429 头值层的确认。
func (s *SettingService) ErrorDiagnosticPlainHeaderValuesRiskAcknowledgementCurrent(ctx context.Context) (bool, error) {
	if s == nil || s.settingRepo == nil {
		return false, ErrErrorDiagnosticSettingServiceUnavailable
	}
	ack, err := s.getErrorDiagnosticRiskAcknowledgementByKey(ctx, SettingKeyErrorDiagnosticPlainHeaderValuesRiskAck)
	if err != nil {
		return false, err
	}
	return plainHeaderRiskSpec.covers(ack), nil
}

// plaintextAcknowledgementView 把一条确认记录收敛成可回显的四项。
func plaintextAcknowledgementView(ack *ErrorDiagnosticRiskAcknowledgement) *ErrorDiagnosticRiskAcknowledgementView {
	if ack == nil {
		return nil
	}
	return &ErrorDiagnosticRiskAcknowledgementView{
		Version:     ack.Version,
		Phrase:      ack.Phrase,
		AdminUserID: ack.AdminUserID,
		AcceptedAt:  ack.AcceptedAt,
	}
}
