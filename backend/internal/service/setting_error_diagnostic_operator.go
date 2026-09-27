package service

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

// 错误诊断的运维开关（管理员 API）。
//
// ADR 0005 要求生产采集、正文留存与 429 头值留存默认关闭，并由运维在完成**书面**风险
// 确认后显式开启。三者的门槛由宽到窄：采集需要门控布尔值 + 有效确认；正文留存与头值留存
// 各自在此之上再加自己的开关与可用稳定密钥，且两者**正交互不影响**。
// 布尔字段无法证明「写过」：本文件把开启动作绑定到一条持久确认记录上——操作员必须逐字
// 输入当前版本的确认语句，服务端每次更新都重新校验，并把语句原文、版本、确认时间与
// 管理员 ID 一起落库。没有这条记录（或版本不符）就没有可审计的书面确认。
//
// 门控键的形状与语义不变（enabled／risk_acknowledged 布尔值，由既有 reader 解析），
// 本服务是唯一会写它的运维入口。但**只解析布尔值的门控本身不足以证明书面确认**：存量被
// 越权改写（直接写库、其它管理入口、历史脏数据）时，布尔值可能为真而没有对应的书面确认。
//
// 判定点因此分两层，本文件只负责其中属于运维开关的部分：
//   - 采集侧：GetErrorDiagnosticSettings（由 storage 侧实现）在存量布尔值之上并入
//     书面确认结论，即调用本文件的 ErrorDiagnosticRiskAcknowledgementCurrent；
//     没有覆盖当前语句版本的有效确认就没有采集。本文件不重复实现该判定。
//   - 运维界面：GetErrorDiagnosticOperatorStatus 同时展示存量值（
//     ReadStoredErrorDiagnosticSettings）与校验结论，使越权改写呈现为
//     「存量开着、但不允许采集」；确认键读不出来时显式报不可用，而不是显示成「未确认」。

const (
	// SettingKeyErrorDiagnosticRiskAcknowledgement 保存最后一次书面风险确认的记录。
	//
	// 它与门控键分开存放：门控可以被改回关闭，而书面确认是已经发生过的审计事实。
	SettingKeyErrorDiagnosticRiskAcknowledgement = "error_diagnostic_risk_acknowledgement"

	// ErrorDiagnosticRiskAcknowledgementVersion 是确认语句的版本。
	//
	// 语句内容变化时必须提升版本：已开启的部署不会自动继承新语句，操作员必须在下一次
	// 更新时按新语句重新逐字确认（服务端每次更新都校验，见 UpdateErrorDiagnosticOperatorSettings）。
	//
	// ADR 0007 要求升级切换采集路径后旧共享确认不能凭原有开关继续采集任何新记录
	// （包括旧密文）；历史密文仍按原七天截止可读。新明文正文与头值还各自要求
	// 独立的当前逐字确认，不能只凭这一份共享确认开启。
	ErrorDiagnosticRiskAcknowledgementVersion = "v2026.09.27.2"

	// ErrorDiagnosticRiskAcknowledgementPhraseEN / ZH 是必须逐字输入的确认语句。
	//
	// 语句按 ADR 0005 声明剩余风险：正文 7 天、元数据 30 天；到期后 API 读取立即失效
	// （按创建时间计算，与清理进度无关），而物理删除只在在线主库由周期清理执行，积压或
	// 停机会使其延迟且**不保证上限**；留存正文是任意客户端文本，可能含无法识别的凭据；
	// 旧副本／备份／PITR／人工导出中的正文仍可能留存且可恢复；本功能不是合规删除或
	// 数据主体擦除手段。
	//
	// 头值（ADR 0005 的另一条窄路径）同样写在语句里：它与正文共用 7 天到期但到期时刻独立，
	// 只采自白名单，且**绝不含** Authorization／Cookie／Set-Cookie／X-Api-Key 这类凭据头——
	// 名单之外的头名不采，值内的未知秘密仍无法被自动识别，因此白名单是信任边界，
	// 不是「绝无秘密」的保证。
	ErrorDiagnosticRiskAcknowledgementPhraseEN = "Error diagnostic bodies and upstream 429 header values are retained for 7 days and sanitized metadata for 30 days; at expiry API reads are rejected immediately, while physical deletion is performed only in the online primary database by periodic cleanup that backlog or downtime can delay without a guaranteed maximum; retained text is arbitrary client content and may contain credentials that filtering cannot identify; header values are captured only from an allowlist that excludes credential headers such as Authorization, Cookie, Set-Cookie and X-Api-Key, and header names outside that list are never captured; copies in replicas, backups, PITR and manual exports may persist and can be recovered there, and this feature is not a compliance deletion or data-subject erasure tool."
	ErrorDiagnosticRiskAcknowledgementPhraseZH = "错误诊断正文与上游 429 头值保留 7 天、净化元数据保留 30 天；到期后立即拒绝 API 读取，但物理删除只在在线主库由周期清理执行，积压或停机可能使其延迟且不保证上限；留存正文是任意客户端文本，可能含无法识别的凭据；头值只采自白名单，绝不含 Authorization、Cookie、Set-Cookie、X-Api-Key 等凭据头，名单之外的头名一律不采；只读副本、备份、PITR 与人工导出中的旧数据可能仍留存且可恢复，本功能不是合规删除或数据主体擦除手段。"
)

var (
	// ErrErrorDiagnosticRiskAcknowledgementRequired 表示请求未携带书面风险确认语句。
	ErrErrorDiagnosticRiskAcknowledgementRequired = infraerrors.BadRequest(
		"ERROR_DIAGNOSTIC_RISK_ACK_REQUIRED",
		"enabling error diagnostics requires the written risk acknowledgement phrase",
	)
	// ErrErrorDiagnosticRiskAcknowledgementInvalid 表示确认语句与当前版本不符。
	ErrErrorDiagnosticRiskAcknowledgementInvalid = infraerrors.BadRequest(
		"ERROR_DIAGNOSTIC_RISK_ACK_INVALID",
		"the risk acknowledgement phrase does not match the required statement",
	)
	// ErrErrorDiagnosticBodyRetentionKeyUnavailable 表示缺少可用于正文留存的稳定密钥。
	//
	// 它与诊断记录的逐行结果码 ErrorDiagnosticBodySkippedEncryptionUnavailable
	// ("skipped_encryption_unavailable") 不是同一件事，不得合并成一条文案：这里说明
	// 「留存根本不允许开启」，那个是「采集运行后这一行的结果」，两者可以同时为真，
	// 且那个枚举刻意不区分「没配密钥」与「密钥不可用」。
	ErrErrorDiagnosticBodyRetentionKeyUnavailable = infraerrors.BadRequest(
		"ERROR_DIAGNOSTIC_BODY_KEY_UNAVAILABLE",
		"body retention requires a configured, restart-stable encryption key",
	)
	// ErrErrorDiagnosticHeaderValueRetentionKeyUnavailable 表示缺少可用于头值留存的稳定密钥。
	//
	// 与 ErrErrorDiagnosticBodyRetentionKeyUnavailable 分开成两个哨兵，尽管两者共用同一把
	// 稳定密钥（判定同源，见 ErrorDiagnosticBodyEncryptionKeyAvailable）：被拒绝的是两个
	// 独立的开关，界面必须能说清是哪一层开不了，不能让「开不了头值留存」显示成
	// 「正文留存不能用」。
	ErrErrorDiagnosticHeaderValueRetentionKeyUnavailable = infraerrors.BadRequest(
		"ERROR_DIAGNOSTIC_HEADER_KEY_UNAVAILABLE",
		"429 header value retention requires a configured, restart-stable encryption key",
	)
	// ErrErrorDiagnosticOperatorIdentityRequired 表示没有可记录的管理员身份。
	ErrErrorDiagnosticOperatorIdentityRequired = infraerrors.Forbidden(
		"ERROR_DIAGNOSTIC_OPERATOR_SESSION_REQUIRED",
		"enabling error diagnostics requires an authenticated admin identity to record the acknowledgement",
	)
	// ErrErrorDiagnosticSettingServiceUnavailable 表示设置服务不可用。
	ErrErrorDiagnosticSettingServiceUnavailable = infraerrors.InternalServer(
		"SETTING_SERVICE_UNAVAILABLE",
		"setting service is unavailable",
	)
)

// ErrorDiagnosticRiskAcknowledgement 是一次书面风险确认的持久记录。
//
// Phrase 保存的是**确认当时的语句原文**：语句常量以后改动也不会改写这条证据，
// 审计时能看出操作员究竟确认了哪段文字。
type ErrorDiagnosticRiskAcknowledgement struct {
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
// 当前这段文字：直接写库（或其它管理入口）完全可以落一条版本正确而原文任意的记录。
// 因此语句原文必须与当前版本的两条语句之一**逐字**相同。
//
// 两种语言各自按自己的原文确认：确认过程本来就允许选语言（见
// UpdateErrorDiagnosticOperatorSettings 的 language 参数），所以语言不是证据的一部分，
// 原文才是。多一个空格、少一个标点都算不匹配——「逐字确认」是这道门槛的全部意义。
func (a ErrorDiagnosticRiskAcknowledgement) CoversCurrentStatement() bool {
	if a.Version != ErrorDiagnosticRiskAcknowledgementVersion {
		return false
	}
	return a.Phrase == ErrorDiagnosticRiskAcknowledgementPhraseEN ||
		a.Phrase == ErrorDiagnosticRiskAcknowledgementPhraseZH
}

// ErrorDiagnosticRiskAcknowledgementView 是回显给管理员界面的确认记录视图。
//
// 只含审计必要的四项：版本、语句原文、管理员 ID、确认时间。来源 IP 与 User-Agent
// 只留在持久记录里，不回显到诊断相关的响应面：确认记录不进入诊断门控，
// 操作员的来源信息也不应随诊断能力扩散。
type ErrorDiagnosticRiskAcknowledgementView struct {
	Version     string    `json:"version"`
	Phrase      string    `json:"phrase"`
	AdminUserID int64     `json:"admin_user_id"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

// ErrorDiagnosticOperatorStatus 是运维开关的当前状态（管理员可见）。
//
// 只描述门控与确认记录本身：不含加密密钥、正文、账号或用户身份。四个布尔字段
// （Enabled／RiskAcknowledged／BodyRetentionEnabled／HeaderValueRetentionEnabled）
// 如实回显**存量门控值**，而 CaptureAllowed／BodyRetentionAllowed／
// HeaderValueRetentionAllowed 是**校验后**的结论：它们同时要求存量布尔值与一条覆盖当前
// 语句版本的有效书面确认，两个留存结论还各自要求可用的稳定密钥（同一把密钥，判定同源）。
//
// 因此存量被越权改写时（enabled=true 但没有当前确认）界面会明确显示不可采集，
// 而不是「开着但不解释为什么」。
type ErrorDiagnosticOperatorStatus struct {
	Enabled              bool `json:"enabled"`
	RiskAcknowledged     bool `json:"risk_acknowledged"`
	BodyRetentionEnabled bool `json:"body_retention_enabled"`
	// HeaderValueRetentionEnabled 是**存量**的 429 头值留存开关。
	//
	// 它与正文开关正交：正文关闭时头值照常采集，头值关闭时正文行为一字不改。
	// 与 BodyRetentionEnabled 同样如实回显，不做校验后的收窄（见 HeaderValueRetentionAllowed）。
	HeaderValueRetentionEnabled bool                                    `json:"header_values_enabled"`
	CaptureAllowed              bool                                    `json:"capture_allowed"`
	BodyRetentionAllowed        bool                                    `json:"body_retention_allowed"`
	HeaderValueRetentionAllowed bool                                    `json:"header_values_allowed"`
	BodyEncryptionKeyAvailable  bool                                    `json:"body_encryption_key_available"`
	RiskVersion                 string                                  `json:"risk_version"`
	RiskPhraseEN                string                                  `json:"risk_phrase_en"`
	RiskPhraseZH                string                                  `json:"risk_phrase_zh"`
	RiskAcknowledgement         *ErrorDiagnosticRiskAcknowledgementView `json:"risk_acknowledgement,omitempty"`
	// RiskAcknowledgementCurrent 报告在库的记录是否覆盖当前语句：版本匹配且原文逐字相同
	// （见 CoversCurrentStatement）。为 false 时，即使门控布尔值为真，也没有覆盖当前语句的
	// 书面确认——包括「版本正确但原文任意」这种伪造记录。
	RiskAcknowledgementCurrent bool `json:"risk_acknowledgement_current"`

	// 以下是两个**新明文层**（票据 08／09）的状态，形状与旧层一致但各自独立：
	// 存量的布尔值如实回显，Allowed 是并入本层自己的逐字确认后的结论。
	// 新层不参与密钥判定：新格式不加密，因此这里没有对应的 key 字段。
	PlainBodyRetentionEnabled bool `json:"plain_body_enabled"`
	PlainBodyRetentionAllowed bool `json:"plain_body_allowed"`
	// PlainHeaderValueRetentionEnabled／Allowed 是新明文 429 头值层。
	//
	// 与正文层互相独立：任何一层的确认或开关都不打开另一层。
	PlainHeaderValueRetentionEnabled bool `json:"plain_header_values_enabled"`
	PlainHeaderValueRetentionAllowed bool `json:"plain_header_values_allowed"`
	// 每一层各自的语句版本与原文，以及库里的确认记录：界面据此说清「哪一层的确认过期了」，
	// 而不是只给一个无法解释的关闭状态。
	PlainBodyRiskVersion           string                                  `json:"plain_body_risk_version"`
	PlainBodyRiskPhraseEN          string                                  `json:"plain_body_risk_phrase_en"`
	PlainBodyRiskPhraseZH          string                                  `json:"plain_body_risk_phrase_zh"`
	PlainBodyRiskAcknowledgement   *ErrorDiagnosticRiskAcknowledgementView `json:"plain_body_risk_acknowledgement,omitempty"`
	PlainBodyRiskAckCurrent        bool                                    `json:"plain_body_risk_acknowledgement_current"`
	PlainHeaderRiskVersion         string                                  `json:"plain_header_risk_version"`
	PlainHeaderRiskPhraseEN        string                                  `json:"plain_header_risk_phrase_en"`
	PlainHeaderRiskPhraseZH        string                                  `json:"plain_header_risk_phrase_zh"`
	PlainHeaderRiskAcknowledgement *ErrorDiagnosticRiskAcknowledgementView `json:"plain_header_risk_acknowledgement,omitempty"`
	PlainHeaderRiskAckCurrent      bool                                    `json:"plain_header_risk_acknowledgement_current"`

	// PlaintextCaptureSupported／PlaintextCaptureSupportReason 是两个**新明文层**共同的部署
	// 前提结论（ADR 0007；票据 10）：本部署的数据库能不能保证「明文随 usage 消失」。
	//
	// 两层共用一条结论（同一张 error_diagnostic_records、同一个 usage 关联），但它是**新明文
	// 专属**的门槛：存量开关、旧共享确认、旧密文正文/头值留存都不受它影响，界面上也不会
	// 因为它把旧层说成不可用。Reason 是闭集原因码，不含数据库错误原文。
	PlaintextCaptureSupported     bool   `json:"plaintext_capture_supported"`
	PlaintextCaptureSupportReason string `json:"plaintext_capture_support_reason"`
}

// ErrorDiagnosticOperatorUpdateInput 是一次运维开关更新请求。
type ErrorDiagnosticOperatorUpdateInput struct {
	Enabled              bool
	BodyRetentionEnabled bool
	// HeaderValueRetentionEnabled 是 429 头值留存的独立开关，与正文留存正交。
	//
	// 门槛与正文留存相同：本次书面确认 + 可用稳定密钥。两者互不代替，也不互相影响
	// （正文关闭时头值照常采集，反之亦然）。
	HeaderValueRetentionEnabled bool
	// PlainBodyEnabled 是**新明文正文**层的独立开关（票据 08）。
	//
	// 门槛与旧层同形但**没有密钥这一环**：本次逐字确认本层自己的语句即可，且**必须**逐字确认
	// 当前明文语句——旧共享语句一个字都打不开它（见 PlainBodyPhrase）。
	PlainBodyEnabled bool
	// PlainHeaderValuesEnabled 是**新明文 429 头值**层的独立开关（票据 09），
	// 与正文层互相独立：各自的确认与开关都不打开对方。
	PlainHeaderValuesEnabled bool
	Language                 string
	// Phrase 是采集门控（旧共享层）的书面确认语句。
	Phrase string
	// PlainBodyPhrase／PlainHeaderValuesPhrase 是两个新层各自的书面确认语句。
	//
	// 只有在本次请求要求打开对应层时才必填；语句原文必须与该层当前版本的语句逐字相同。
	// 不复用 Phrase：三层的语句讲的是三件不同的风险，能互相代替就等于三份确认都没意义。
	PlainBodyPhrase         string
	PlainHeaderValuesPhrase string
	AdminUserID             int64
	IPAddress               string
	UserAgent               string
}

// expectedErrorDiagnosticRiskPhrase 返回该语言下必须逐字输入的确认语句。
//
// 语言归一复用管理员合规确认的同一规则（zh* → zh，其余 → en），
// 保证同一批双语确认入口对 language 的解释一致。
func expectedErrorDiagnosticRiskPhrase(language string) string {
	if normalizeAdminComplianceLanguage(language) == "zh" {
		return ErrorDiagnosticRiskAcknowledgementPhraseZH
	}
	return ErrorDiagnosticRiskAcknowledgementPhraseEN
}

// ErrorDiagnosticBodyRetentionKeyAvailability 是门控读取器的**可选**能力：报告正文留存的
// 稳定密钥此刻是否可用。
//
// 与 ErrorDiagnosticRiskAcknowledgementReader 分开：那份能力回答「允许不允许」（运维写了什么），
// 这份回答「做不做得到」（部署有没有稳定密钥）。采集接缝必须同时拿到两者才 tee 出站正文：
// 门控布尔值开着而密钥缺席时（配置被移除、随机密钥重启后失效），正文注定被判为
// skipped_encryption_unavailable，传输层不该为它读走并复制最多 1 MiB 明文。
//
// 不声明本能力的读取器按「没有稳定密钥」处理（fail closed）。这与
// ErrorDiagnosticService 的写入判定同向：那边用 cipher 是否存在决定能否留存，两边都不会
// 只凭布尔值就把明文带出传输层。
type ErrorDiagnosticBodyRetentionKeyAvailability interface {
	ErrorDiagnosticBodyEncryptionKeyAvailable() bool
}

var _ ErrorDiagnosticBodyRetentionKeyAvailability = (*SettingService)(nil)

// applyErrorDiagnosticBodyRetentionKeyAvailability 收窄**正文留存**这一层结论：
// 没有可用稳定密钥时，有效的正文留存一律不可用，同时把存量意图原样带出。
//
// 只收窄 BodyRetentionEnabled，不动采集（Enabled／RiskAcknowledged）：
// 元数据只要有一条有效书面确认就该继续采集，正文不该因为密钥缺席而停在
// 「布尔值开着、但每一行都注定被跳过」的状态——那是把配置故障显示成正常运行。
//
// 收窄必须与意图分开存放：接缝要靠 BodyRetentionRequested 区分「没有开启留存」（正常关闭，
// 只能报告 not_observed）与「要求过留存但部署拿不出密钥」（配置故障，必须留下稳定原因码
// skipped_encryption_unavailable）。只看收窄后的布尔值会把后者误判成前者。
//
// 判定与 ErrorDiagnosticBodyEncryptionKeyAvailable 同源，因此运维界面的 BodyRetentionAllowed
// 与采集侧的结论不会各自漂移；采集接缝读到的是同一条收窄后的结论，不会 tee 出注定被丢弃的明文。
func (s *SettingService) applyErrorDiagnosticBodyRetentionKeyAvailability(settings ErrorDiagnosticSettings) ErrorDiagnosticSettings {
	// 取或而不是直接赋值：重复收窄（拿已收窄的结论再判一次）也不会把意图丢掉。
	settings.BodyRetentionRequested = settings.BodyRetentionRequested || settings.BodyRetentionEnabled
	if !s.ErrorDiagnosticBodyEncryptionKeyAvailable() {
		settings.BodyRetentionEnabled = false
	}
	return settings
}

// ErrorDiagnosticBodyEncryptionKeyAvailable 报告是否具备可用于正文留存的密钥。
//
// 判定与正文加密器一致：主密钥必须是非空、合法 hex、32 字节（AES-256）密钥；
// 并且必须是运维显式配置的密钥——启动时随机生成的密钥每次重启都会变，已留存密文
// 将永久不可解，因此这种部署不允许打开正文留存。这里只回答「能不能用」，不回显密钥。
func (s *SettingService) ErrorDiagnosticBodyEncryptionKeyAvailable() bool {
	if s == nil || s.cfg == nil || !s.cfg.Totp.EncryptionKeyConfigured {
		return false
	}
	raw, err := hex.DecodeString(strings.TrimSpace(s.cfg.Totp.EncryptionKey))
	if err != nil {
		return false
	}
	return len(raw) == 32
}

// getErrorDiagnosticRiskAcknowledgement 读取在库的书面确认记录。
//
// 没有记录、记录损坏或缺少版本／管理员 ID／确认时间时都返回「没有有效确认」：
// 残缺的记录不是证据，不能被当成已确认，也不能被算作当前版本的确认。
//
// 语句原文不在这里判定：原文不匹配的记录仍然被读出来，供运维状态如实展示「库里存了什么」，
// 但由 CoversCurrentStatement 判定为不覆盖当前语句。校验结论只有一个来源，
// 读取与判定分开，界面就不会把「有一条记录」误显示成「已确认当前语句」。
func (s *SettingService) getErrorDiagnosticRiskAcknowledgement(ctx context.Context) (*ErrorDiagnosticRiskAcknowledgement, error) {
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyErrorDiagnosticRiskAcknowledgement)
	if errors.Is(err, ErrSettingNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get error diagnostic risk acknowledgement: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var ack ErrorDiagnosticRiskAcknowledgement
	if err := json.Unmarshal([]byte(raw), &ack); err != nil {
		return nil, fmt.Errorf("decode error diagnostic risk acknowledgement: %w", err)
	}
	if strings.TrimSpace(ack.Version) == "" || ack.AdminUserID <= 0 || ack.AcceptedAt.IsZero() {
		return nil, nil
	}
	return &ack, nil
}

// ErrorDiagnosticRiskAcknowledgementCurrent 报告是否存在覆盖当前语句版本的有效书面确认。
//
// 「有效」由 CoversCurrentStatement 定义：版本匹配**并且**语句原文与当前版本的
// 两条语句之一逐字相同。只校验版本不足以采信——那样一条任意原文的记录就能打开采集。
//
// 只读确认键：不读门控键，因此不会被门控读取路径递归调用。返回的是布尔结论，
// 操作员 ID／来源 IP 等 PII 不进入采集侧。
func (s *SettingService) ErrorDiagnosticRiskAcknowledgementCurrent(ctx context.Context) (bool, error) {
	if s == nil || s.settingRepo == nil {
		return false, ErrErrorDiagnosticSettingServiceUnavailable
	}
	ack, err := s.getErrorDiagnosticRiskAcknowledgement(ctx)
	if err != nil {
		// 读不到／解不开确认记录都按「没有有效确认」处理，由调用方 fail closed。
		return false, err
	}
	return ack != nil && ack.CoversCurrentStatement(), nil
}

// GetErrorDiagnosticOperatorStatus 读取运维开关状态。
//
// 同时给出存量值与校验结论：存量读取失败返回错误（由调用方按不可用处理），
// 绝不用「全关」掩盖存储故障；确认键读取失败同样返回错误，让界面显示「确认记录读不出来」，
// 而不是把故障显示成「未确认」——两者对运维的含义完全不同。
// errorDiagnosticPlaintextAcknowledgement 校验并编码某一新明文层本次的逐字确认。
//
// 三个事实必须同时成立：本层被请求打开、语句非空、语句与**本层当前版本**的语句逐字相同。
// 请求里没有语句（或只有空白）与语句不对是两种不同的拒绝，因此返回不同的哨兵，
// 但两者都不写入任何键：被拒的更新不能留下部分状态。
func errorDiagnosticPlaintextAcknowledgement(spec plaintextRiskAcknowledgementSpec, requested bool, phrase string, input ErrorDiagnosticOperatorUpdateInput, invalid error) (string, error) {
	if !requested {
		return "", nil
	}
	trimmed := strings.TrimSpace(phrase)
	if trimmed == "" {
		return "", ErrErrorDiagnosticRiskAcknowledgementRequired
	}
	if trimmed != spec.expectedPhrase(input.Language) {
		return "", invalid
	}
	payload, err := json.Marshal(ErrorDiagnosticRiskAcknowledgement{
		Version:     spec.Version,
		Phrase:      trimmed,
		AdminUserID: input.AdminUserID,
		IPAddress:   strings.TrimSpace(input.IPAddress),
		UserAgent:   strings.TrimSpace(input.UserAgent),
		AcceptedAt:  time.Now().UTC(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal plaintext risk acknowledgement: %w", err)
	}
	return string(payload), nil
}

func (s *SettingService) GetErrorDiagnosticOperatorStatus(ctx context.Context) (ErrorDiagnosticOperatorStatus, error) {
	status := ErrorDiagnosticOperatorStatus{
		RiskVersion:  ErrorDiagnosticRiskAcknowledgementVersion,
		RiskPhraseEN: ErrorDiagnosticRiskAcknowledgementPhraseEN,
		RiskPhraseZH: ErrorDiagnosticRiskAcknowledgementPhraseZH,

		PlainBodyRiskVersion:    plainBodyRiskSpec.Version,
		PlainBodyRiskPhraseEN:   plainBodyRiskSpec.PhraseEN,
		PlainBodyRiskPhraseZH:   plainBodyRiskSpec.PhraseZH,
		PlainHeaderRiskVersion:  plainHeaderRiskSpec.Version,
		PlainHeaderRiskPhraseEN: plainHeaderRiskSpec.PhraseEN,
		PlainHeaderRiskPhraseZH: plainHeaderRiskSpec.PhraseZH,
	}
	if s == nil || s.settingRepo == nil {
		return ErrorDiagnosticOperatorStatus{}, ErrErrorDiagnosticSettingServiceUnavailable
	}

	stored, err := s.ReadStoredErrorDiagnosticSettings(ctx)
	if err != nil {
		return ErrorDiagnosticOperatorStatus{}, err
	}
	ack, err := s.getErrorDiagnosticRiskAcknowledgement(ctx)
	if err != nil {
		return ErrorDiagnosticOperatorStatus{}, err
	}
	plainBodyAck, err := s.getErrorDiagnosticRiskAcknowledgementByKey(ctx, SettingKeyErrorDiagnosticPlainBodyRiskAck)
	if err != nil {
		return ErrorDiagnosticOperatorStatus{}, err
	}
	plainHeaderAck, err := s.getErrorDiagnosticRiskAcknowledgementByKey(ctx, SettingKeyErrorDiagnosticPlainHeaderValuesRiskAck)
	if err != nil {
		return ErrorDiagnosticOperatorStatus{}, err
	}

	status.Enabled = stored.Enabled
	status.RiskAcknowledged = stored.RiskAcknowledged
	status.BodyRetentionEnabled = stored.BodyRetentionEnabled
	status.HeaderValueRetentionEnabled = stored.HeaderValueRetentionEnabled
	status.PlainBodyRetentionEnabled = stored.PlainBodyRetentionEnabled
	status.PlainHeaderValueRetentionEnabled = stored.PlainHeaderValueRetentionEnabled
	if ack != nil {
		status.RiskAcknowledgement = &ErrorDiagnosticRiskAcknowledgementView{
			Version:     ack.Version,
			Phrase:      ack.Phrase,
			AdminUserID: ack.AdminUserID,
			AcceptedAt:  ack.AcceptedAt,
		}
		status.RiskAcknowledgementCurrent = ack.CoversCurrentStatement()
	}
	// 两个新层的确认记录各自回显并各自判定：界面必须能说清「哪一层的确认过期了」，
	// 而不是把「有一条旧共享确认」显示成新层已确认。
	status.PlainBodyRiskAcknowledgement = plaintextAcknowledgementView(plainBodyAck)
	status.PlainBodyRiskAckCurrent = plainBodyRiskSpec.covers(plainBodyAck)
	status.PlainHeaderRiskAcknowledgement = plaintextAcknowledgementView(plainHeaderAck)
	status.PlainHeaderRiskAckCurrent = plainHeaderRiskSpec.covers(plainHeaderAck)

	// 校验结论复用采集侧同一判定（ApplyErrorDiagnosticRiskAcknowledgement），
	// 不在这里另写一份规则：存量布尔值 + 当前版本的有效书面确认，缺一不可。
	// 正文留存再并入「稳定密钥是否可用」，用的是与 GetErrorDiagnosticSettings 相同的收窄函数，
	// 因此界面结论与采集侧结论出自同一条规则。
	// enabled=true 但没有当前确认（或没有稳定密钥）时，这里是明确的 false，
	// 而不是「开着但不解释」。
	verified := s.applyErrorDiagnosticBodyRetentionKeyAvailability(
		ApplyErrorDiagnosticRiskAcknowledgement(ctx, stored, s))
	// 部署前提与采集侧用同一个收窄函数（同一个带缓存的探针），因此界面上两个新明文层的
	// 结论与接缝实际会不会采完全一致：存量开着、确认也在、但部署不支持时，
	// plain_*_allowed 是明确的 false，而不是「开着但不解释」。
	verified = s.ApplyErrorDiagnosticPlaintextCaptureSupport(ctx, verified)
	status.BodyEncryptionKeyAvailable = s.ErrorDiagnosticBodyEncryptionKeyAvailable()
	status.CaptureAllowed = verified.CaptureAllowed()
	status.BodyRetentionAllowed = verified.BodyCaptureAllowed()
	// 头值这一层的结论用的是与采集侧相同的两个来源：有效确认（verified 已并入）与
	// 自己的存量开关；密钥与正文同源，因此不会再引入第二把密钥规则。
	// 注意这里**不**把结论写回读取器：收窄只用于展示（理由见
	// errorDiagnosticHeaderValueRetentionAllowed）。
	status.HeaderValueRetentionAllowed = errorDiagnosticHeaderValueRetentionAllowed(verified, status.BodyEncryptionKeyAvailable)
	// 新明文层的结论用的是各自独立的确认与各自的存量开关，且**不并入密钥**：
	// 明文本来就不加密，要求一个稳定密钥既说不通，也会把「明文本来就能落库」报成配置故障。
	status.PlainBodyRetentionAllowed = plainBodyRiskSpec.covers(plainBodyAck) && verified.PlainBodyCaptureAllowed()
	status.PlainHeaderValueRetentionAllowed = plainHeaderRiskSpec.covers(plainHeaderAck) && verified.PlainHeaderValuesCaptureAllowed()
	// 部署前提单独回显：它不是某一层的开关，而是两层共同的数据库前提。原因码让界面能说清
	// 「为什么开着却不允许采集」——分区、所有权外键缺失、探针查不出来，三者含义不同；
	// 数据库错误原文不在这里出现。
	support := s.PlaintextCaptureSupport(ctx)
	status.PlaintextCaptureSupported = support.Supported
	status.PlaintextCaptureSupportReason = support.Reason
	return status, nil
}

// errorDiagnosticHeaderValueRetentionAllowed 给出**头值留存**这一层的校验结论：
// 有效确认 + 头值自己的存量开关 + 可用稳定密钥，三者缺一不可。
//
// 它刻意与正文的收窄函数（applyErrorDiagnosticBodyRetentionKeyAvailability）分开：
// 正文的收窄会被写回读取器（GetErrorDiagnosticSettings），而头值这一层**只用于展示**。
// 传输接缝必须继续从存量开关推导「本次要求过头值留存」，把读取器里的
// HeaderValueRetentionEnabled 收窄成 false 会让接缝把配置故障（要求过、但没有密钥）
// 报成 not_requested，也就丢掉了稳定原因码
// skipped_encryption_unavailable（见 ErrorDiagnosticHeaderVerdictSuppressedEncryptionUnavailable）。
// 因此本函数不改动任何读取路径，只在运维结论上如实呈现「存量的头值开关此刻没有生效」。
func errorDiagnosticHeaderValueRetentionAllowed(verified ErrorDiagnosticSettings, keyAvailable bool) bool {
	return verified.HeaderValuesCaptureAllowed() && keyAvailable
}

// UpdateErrorDiagnosticOperatorSettings 应用一次运维开关更新。
//
// 规则：
//   - 结果状态为开启时，**每次**更新都必须重新逐字确认当前语句，并记录管理员 ID；
//     已有布尔值不能代替本次确认（避免通过「已确认过」绕过门槛），库里已有的旧确认
//     也不能代替本次确认（语句版本变化后旧确认不再覆盖当前语句）。
//   - 正文留存与 429 头值留存是两个更窄的层：各自需要开启 + 本次确认 + 可用密钥，
//     三者缺一不可；两者互不代替，也不互相牵连。
//   - 关闭永远允许，不需要确认或身份，并强制把正文留存与头值留存一并关掉
//     （兜底方向必须总能关）。
//
// 校验全部通过后才写入：确认记录与门控在同一个多次 upsert 中落库，失败不会留下
// 「门控已开但没有确认记录」的状态。
func (s *SettingService) UpdateErrorDiagnosticOperatorSettings(ctx context.Context, input ErrorDiagnosticOperatorUpdateInput) (ErrorDiagnosticOperatorStatus, error) {
	if s == nil || s.settingRepo == nil {
		return ErrorDiagnosticOperatorStatus{}, ErrErrorDiagnosticSettingServiceUnavailable
	}

	// 关闭时这里是全 false：正文留存、头值留存与风险确认位一并落回关闭。
	// 关闭不需要任何确认或身份，也不接受「只关一半」——两个留存开关都随门控一起关掉。
	settings := ErrorDiagnosticSettings{Enabled: input.Enabled}
	updates := map[string]string{}

	if input.Enabled {
		// 书面确认必须记名：没有管理员身份就没有操作员 ID，不能退化成匿名开启。
		if input.AdminUserID <= 0 {
			return ErrorDiagnosticOperatorStatus{}, ErrErrorDiagnosticOperatorIdentityRequired
		}
		phrase := strings.TrimSpace(input.Phrase)
		if phrase == "" {
			return ErrorDiagnosticOperatorStatus{}, ErrErrorDiagnosticRiskAcknowledgementRequired
		}
		if phrase != expectedErrorDiagnosticRiskPhrase(input.Language) {
			return ErrorDiagnosticOperatorStatus{}, ErrErrorDiagnosticRiskAcknowledgementInvalid
		}
		settings.RiskAcknowledged = true
		if input.BodyRetentionEnabled {
			if !s.ErrorDiagnosticBodyEncryptionKeyAvailable() {
				return ErrorDiagnosticOperatorStatus{}, ErrErrorDiagnosticBodyRetentionKeyUnavailable
			}
			settings.BodyRetentionEnabled = true
		}
		// 头值是第二层留存，门槛与正文同形但**互相独立**：各自需要自己被请求、共用同一把
		// 稳定密钥。这里既不放行「只开正文就顺带开头值」，也不因为正文没开而拒绝头值。
		if input.HeaderValueRetentionEnabled {
			if !s.ErrorDiagnosticBodyEncryptionKeyAvailable() {
				return ErrorDiagnosticOperatorStatus{}, ErrErrorDiagnosticHeaderValueRetentionKeyUnavailable
			}
			settings.HeaderValueRetentionEnabled = true
		}
		// 两个新明文层：各自需要自己被请求 + **本层自己的**逐字确认 + 部署前提。旧共享语句
		// （phrase）不能代替它们，它们也不能互相代替；两层都不看密钥。
		//
		// 部署前提只在**至少一层被请求打开**时才查：数据库形态既不挡住旧密文层的开启，
		// 也不挡住任何一层的关闭——降级方向永远放行（见 UpdateErrorDiagnosticOperatorSettings
		// 顶部「关闭永远允许」的约定）。探针绕过缓存，见 rejectPlaintextCaptureEnablement。
		if input.PlainBodyEnabled || input.PlainHeaderValuesEnabled {
			if err := s.rejectPlaintextCaptureEnablement(ctx, ErrErrorDiagnosticPlaintextDeploymentUnsupported); err != nil {
				return ErrorDiagnosticOperatorStatus{}, err
			}
		}
		plainBodyAck, err := errorDiagnosticPlaintextAcknowledgement(plainBodyRiskSpec, input.PlainBodyEnabled, input.PlainBodyPhrase, input, ErrErrorDiagnosticPlainBodyRiskAcknowledgementInvalid)
		if err != nil {
			return ErrorDiagnosticOperatorStatus{}, err
		}
		plainHeaderAck, err := errorDiagnosticPlaintextAcknowledgement(plainHeaderRiskSpec, input.PlainHeaderValuesEnabled, input.PlainHeaderValuesPhrase, input, ErrErrorDiagnosticPlainHeaderValuesRiskAcknowledgementInvalid)
		if err != nil {
			return ErrorDiagnosticOperatorStatus{}, err
		}
		sharedAck := ErrorDiagnosticRiskAcknowledgement{
			Version:     ErrorDiagnosticRiskAcknowledgementVersion,
			Phrase:      phrase,
			AdminUserID: input.AdminUserID,
			IPAddress:   strings.TrimSpace(input.IPAddress),
			UserAgent:   strings.TrimSpace(input.UserAgent),
			AcceptedAt:  time.Now().UTC(),
		}
		payload, err := json.Marshal(sharedAck)
		if err != nil {
			return ErrorDiagnosticOperatorStatus{}, fmt.Errorf("marshal error diagnostic risk acknowledgement: %w", err)
		}
		updates[SettingKeyErrorDiagnosticRiskAcknowledgement] = string(payload)
		if input.PlainBodyEnabled {
			settings.PlainBodyRetentionEnabled = true
			updates[SettingKeyErrorDiagnosticPlainBodyRiskAck] = plainBodyAck
		}
		if input.PlainHeaderValuesEnabled {
			settings.PlainHeaderValueRetentionEnabled = true
			updates[SettingKeyErrorDiagnosticPlainHeaderValuesRiskAck] = plainHeaderAck
		}
	}

	settingsPayload, err := json.Marshal(settings)
	if err != nil {
		return ErrorDiagnosticOperatorStatus{}, fmt.Errorf("marshal error diagnostic settings: %w", err)
	}
	updates[SettingKeyErrorDiagnostic] = string(settingsPayload)

	if err := s.settingRepo.SetMultiple(ctx, updates); err != nil {
		return ErrorDiagnosticOperatorStatus{}, fmt.Errorf("save error diagnostic operator settings: %w", err)
	}

	// 开关事件必须可审计（谁、把什么打开／关掉、确认了哪个版本），
	// 但确认语句原文只落库、不进日志。
	slog.Info("error_diagnostic.operator_settings_updated",
		"audit", true,
		"enabled", settings.Enabled,
		"body_retention_enabled", settings.BodyRetentionEnabled,
		"header_values_enabled", settings.HeaderValueRetentionEnabled,
		// 新明文层的开关同样进审计：审计必须能回答「谁在什么时候打开了明文留存」，
		// 而语句原文只落库、不进日志。
		"plain_body_enabled", settings.PlainBodyRetentionEnabled,
		"plain_header_values_enabled", settings.PlainHeaderValueRetentionEnabled,
		"risk_version", ErrorDiagnosticRiskAcknowledgementVersion,
		"plain_body_risk_version", plainBodyRiskSpec.Version,
		"plain_header_risk_version", plainHeaderRiskSpec.Version,
		"admin_user_id", input.AdminUserID,
	)

	return s.GetErrorDiagnosticOperatorStatus(ctx)
}
