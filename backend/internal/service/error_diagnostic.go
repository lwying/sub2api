package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 错误诊断记录（error diagnostic record）：独立于 usage-owned 请求审计的短期诊断事实。
//
// 每次「真正发往上游」的尝试收到 4xx/5xx 时各写一条，即使该逻辑请求随后重试成功；
// 没有使用记录时也成立（usage_log_id 为空），但绝不因此伪造用量，也绝不把正文塞进既有审计。
//
// 对外契约刻意收窄：每条尝试只有一个不可猜的 opaque 字符串 ID，
// 管理端输出只含协议枚举、尝试序号、上游状态、可选 usage 关联与正文状态／原因／到期；
// 不含账号、账号 ID、原始 URL、请求头、错误消息、指纹或其他自由字段。
//
// 保留期按 created_at 计算，两个层面必须分开理解：
//   - 第 7 天起 API 立即拒绝读取正文，第 30 天起 API 立即拒绝列表／详情；
//   - 在线主库上的物理清除（正文清空密文列、元数据删除整行）是周期任务，每轮有批量上限，
//     停机、积压或单轮未取完都会推迟它，因此**不承诺**到期即已物理删除，也不承诺确切时刻。
//     清理服务上报积压量与最老超期时长，由运维据此判断是否落后。
//
// 副本、备份与 PITR 的留存窗口由部署方决定；本实现不声称这些存储层到期不可恢复，
// 也不构成任何合规删除声明。
const (
	// SettingKeyErrorDiagnostic 保存错误诊断的采集开关与操作员风险确认。
	//
	// 生产默认全关：采集默认 OFF，且必须由操作员显式确认风险后才视为可用。
	// body_retention_enabled 是票 02 的分阶段 opt-in，元数据采集打开后仍默认关闭正文。
	SettingKeyErrorDiagnostic = "error_diagnostic_settings"

	// 协议枚举，与既有请求审计的 Protocol 用语一致。
	ErrorDiagnosticProtocolMessages        = "messages"
	ErrorDiagnosticProtocolChatCompletions = "chat_completions"
	ErrorDiagnosticProtocolResponses       = "responses"

	// ErrorDiagnosticStageWire 是唯一允许的采集阶段：真实发往上游的接缝。
	ErrorDiagnosticStageWire = "wire"

	// 正文状态（body_state）：管理端只看到状态，不看到内容。
	ErrorDiagnosticBodyStateNotObserved = "not_observed"
	ErrorDiagnosticBodyStateStored      = "stored"
	ErrorDiagnosticBodyStateSkipped     = "skipped"
	ErrorDiagnosticBodyStateExpired     = "expired"
	ErrorDiagnosticBodyStatePurged      = "purged"

	// 正文原因码（body_reason）：稳定枚举，可逐次展示，不泄漏内容。
	ErrorDiagnosticBodyNotObserved                  = "not_observed"
	ErrorDiagnosticBodyRetained                     = "retained"
	ErrorDiagnosticBodySkippedNotTextJSON           = "skipped_not_text_json"
	ErrorDiagnosticBodySkippedTooLarge              = "skipped_too_large"
	ErrorDiagnosticBodySkippedAttachment            = "skipped_attachment"
	ErrorDiagnosticBodySkippedKnownCredential       = "skipped_known_credential"
	ErrorDiagnosticBodySkippedIncompleteRead        = "skipped_incomplete_read"
	ErrorDiagnosticBodySkippedEncryptionUnavailable = "skipped_encryption_unavailable"
	ErrorDiagnosticBodySkippedRetentionDisabled     = "skipped_body_retention_disabled"

	// ErrorDiagnosticMetadataRetention 是元数据在在线主库的物理保留期。
	ErrorDiagnosticMetadataRetention = 30 * 24 * time.Hour
	// ErrorDiagnosticBodyRetention 是加密正文在在线主库的物理保留期。
	ErrorDiagnosticBodyRetention = 7 * 24 * time.Hour

	// ErrorDiagnosticMaxBodyBytes 是允许留存的出站正文上限（1 MiB）。
	ErrorDiagnosticMaxBodyBytes = 1 << 20
	// ErrorDiagnosticMaxListLimit 是单次列表查询的硬上限。
	ErrorDiagnosticMaxListLimit = 200
	// ErrorDiagnosticDefaultListLimit 是未指定 limit 时的默认条数。
	ErrorDiagnosticDefaultListLimit = 50
	// ErrorDiagnosticMaxListOffset 是翻页偏移的上限，仅供管理端遍历短期结果集。
	//
	// 这是防滥用的健全性边界，不是「可见范围」：它足够大（上限 × 单页条数 = 1000 页），
	// 因此管理端可以正常翻到任意真实存在的一页。没有上限会让 OFFSET 变成廉价的扫描放大器。
	ErrorDiagnosticMaxListOffset = 100_000
	// ErrorDiagnosticMaxAttemptIndex 是尝试序号的硬上限。
	ErrorDiagnosticMaxAttemptIndex = 1000
	// ErrorDiagnosticIDBytes 是不可猜 ID 的随机字节数（128 位）。
	ErrorDiagnosticIDBytes = 16
	// ErrorDiagnosticIDLength 是 hex 编码后的 ID 长度。
	ErrorDiagnosticIDLength = ErrorDiagnosticIDBytes * 2
	// ErrorDiagnosticMaxBodyReadBytes 是解密后允许返回的正文上限，超出即视为异常。
	ErrorDiagnosticMaxBodyReadBytes = ErrorDiagnosticMaxBodyBytes
)

var (
	// ErrErrorDiagnosticDisabled 表示采集未启用或操作员未做显式风险确认。
	ErrErrorDiagnosticDisabled = errors.New("error diagnostics are disabled")
	// ErrErrorDiagnosticInvalidAttempt 表示输入不满足白名单／边界，未写入任何行。
	ErrErrorDiagnosticInvalidAttempt = errors.New("error diagnostic attempt is not eligible")
	// ErrErrorDiagnosticNotFound 表示记录不存在、ID 不合法或已按 30 天期限删除。
	ErrErrorDiagnosticNotFound = errors.New("error diagnostic record not found")
	// ErrErrorDiagnosticBodyGone 表示正文未留存、已物理清除或已按 7 天期限到期。
	ErrErrorDiagnosticBodyGone = errors.New("error diagnostic body is not available")
	// ErrErrorDiagnosticHeaderValuesGone 表示头值未留存、已物理清除或已按 7 天期限到期。
	//
	// 与正文分开成不同的哨兵：「这条尝试有没有留过头值」与「有没有留过正文」是两个事实，
	// 共用一个错误会让接口无法区分，也会让读取结果变成互相推断的探针。
	ErrErrorDiagnosticHeaderValuesGone = errors.New("error diagnostic header values are not available")
	// ErrErrorDiagnosticUnavailable 表示存储或依赖不可用。
	ErrErrorDiagnosticUnavailable = errors.New("error diagnostics are temporarily unavailable")
	// ErrErrorDiagnosticBacklogUnsupported 表示存储层未提供清理积压观测能力。
	// 它与「积压为 0」是不同的事实，调用方不得把两者混为一谈。
	ErrErrorDiagnosticBacklogUnsupported = errors.New("error diagnostic cleanup backlog is not observable")
)

// ErrorDiagnosticSettings 是错误诊断的门控配置。
//
// 生产默认全关：Enabled 与 RiskAcknowledged 必须同时为真才允许写入元数据。
// BodyRetentionEnabled 是票 02 的分阶段 opt-in；未配置时为 false，正文一律不留存。
// 任何读取失败或字段缺失都按关闭处理（fail closed），不做隐式开启。
//
// 有效结论与存量意图是两件事，读取器必须把后者一并带出（见 BodyRetentionRequested）：
// 只凭收窄后的有效值无法区分「没有开启留存」与「要求了留存但部署做不到」。
type ErrorDiagnosticSettings struct {
	Enabled              bool `json:"enabled"`
	RiskAcknowledged     bool `json:"risk_acknowledged"`
	BodyRetentionEnabled bool `json:"body_retention_enabled"`
	// HeaderValueRetentionEnabled 是 429 头值留存（Claude Messages 专属）的独立 opt-in。
	//
	// 它与 BodyRetentionEnabled 是**两个开关**：正文关闭时头值照常采集，头值关闭时正文
	// 行为一字不改。风险确认（Enabled + RiskAcknowledged）仍是两者共同的前置门槛，
	// 头值本身不额外需要一份书面确认语句。
	HeaderValueRetentionEnabled bool `json:"header_values_enabled"`
	// BodyRetentionRequested 报告存量门控**是否要求过**留存正文，即并入「密钥是否可用」
	// 之前的那份意图。
	//
	// 它只在进程内传递，不落库（json:"-"）：持久化形状与含义都不变，既没有新键也没有新枚举。
	// 用途只有一个——让采集接缝能把「要求过、但没有稳定密钥」这一状态显式表达为封闭抑制，
	// 而不是让它退化成 not_observed（见 BodyRetentionSuppressedByMissingKey）。
	BodyRetentionRequested bool `json:"-"`
}

// CaptureAllowed 报告是否允许写入诊断元数据（采集开关 + 显式风险确认）。
func (s ErrorDiagnosticSettings) CaptureAllowed() bool {
	return s.Enabled && s.RiskAcknowledged
}

// BodyCaptureAllowed 报告是否允许留存加密正文（票 02 的分阶段 opt-in）。
func (s ErrorDiagnosticSettings) BodyCaptureAllowed() bool {
	return s.CaptureAllowed() && s.BodyRetentionEnabled
}

// HeaderValuesCaptureAllowed 报告是否允许留存 429 头值（与正文留存互不影响）。
//
// 它只看头值自己的开关：正文开关关着不影响头值，头值开关关着也不影响正文。
func (s ErrorDiagnosticSettings) HeaderValuesCaptureAllowed() bool {
	return s.CaptureAllowed() && s.HeaderValueRetentionEnabled
}

// BodyRetentionSuppressedByMissingKey 报告本次是否处于「要求留存正文、但没有可用稳定密钥」
// 的封闭抑制状态：正文一个字节都不 tee，但元数据必须留下稳定原因码
// skipped_encryption_unavailable。
//
// 它与「没有开启正文留存」是不同的事实，不能合并：前者是配置故障（门控要求留存，部署此刻
// 拿不出密钥），后者是正常关闭。两者若都按 not_observed 落库，运维看到的会是「本次没有
// opt-in 正文采集」，与运维界面上 key-unavailable 的状态自相矛盾，也无法定位故障。
//
// 判据同时看有效值与存量意图：有效值可能已被收窄为 false（见
// (*SettingService).applyErrorDiagnosticBodyRetentionKeyAvailability），只看它就把
// 「要求过、做不到」误判成「没要求」；而读取器没有收窄时（例如只回存量值的替身），
// BodyRetentionEnabled 本身就是意图，因此这里取或。
func (s ErrorDiagnosticSettings) BodyRetentionSuppressedByMissingKey(keyAvailable bool) bool {
	if keyAvailable {
		return false
	}
	return s.BodyRetentionEnabled || s.BodyRetentionRequested
}

// ErrorDiagnosticSettingsReader 由设置服务实现。
type ErrorDiagnosticSettingsReader interface {
	GetErrorDiagnosticSettings(ctx context.Context) (ErrorDiagnosticSettings, error)
}

// ErrorDiagnosticAttempt 是 transport 在真实上游尝试接缝处能观察到的事实。
//
// 调用方只提供「观察到的字节与状态」：协议、尝试序号、上游状态，以及本次出站正文的
// 原始字节与是否读完整。是否文本 JSON、是否含附件、是否含已知结构化凭据，以及最终是否
// 留存，全部由本服务判定（见 ClassifyErrorDiagnosticBody 与 DecideErrorDiagnosticBody），
// 调用方不能自称 stored，也不能在缺密钥时退化为明文。
// UsageLogID 为 0 表示该次失败没有使用记录。
type ErrorDiagnosticAttempt struct {
	UsageLogID         int64
	Protocol           string
	AttemptIndex       int
	Stage              string
	UpstreamStatusCode int

	// Body 是本次实际发往上游的完整出站正文；票 01 一律留空。
	// 调用方必须在此调用期间提供有效字节，不得延迟读取或事后重建。
	Body []byte
	// BodyReadComplete 报告 transport 是否读到 EOF（未截断、未中途失败）。
	BodyReadComplete bool
	// BodyVerdict 是 transport 对本次出站正文的观察结论，取值见 ErrorDiagnosticBodyVerdict*。
	//
	// transport 在体量超限或读取不完整时会主动扣住字节，因此这两类情况必须由 verdict
	// 显式说明，否则服务只能当作「未观察到正文」。verdict 只描述观察事实，不决定是否留存。
	// 空值表示调用方只提供字节，由服务自行判定。
	//
	// verdict 也承载「按策略扣住字节」的封闭抑制结论
	// （ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable）：调用方请求了正文留存、
	// 但没有可用稳定密钥，因此从未读取也从未复制这份明文。它只会导致跳过，
	// 永远不会导致留存，映射到既有的稳定原因码，不新增存储枚举。
	BodyVerdict string

	// HeaderValues 是本次尝试在 Messages + HTTP 429 时的头值快照（净化后的值，见
	// ErrorDiagnosticHeaderValues）。非 429 或非 Messages 的尝试必须留空：
	// 服务侧会按范围二次判定，越界的值一律不落库。
	//
	// 它与 Body 完全独立：正文开关关闭时头值仍可成立，反之亦然。
	HeaderValues ErrorDiagnosticHeaderValues
	// HeaderVerdict 是调用方对本次头值观察的结论，取值见 ErrorDiagnosticHeaderVerdict*。
	//
	// 空值表示调用方只给值、由服务自行判定；未知取值一律按不合格处理（fail closed）。
	HeaderVerdict string
}

// transport 对出站正文的观察结论。
const (
	ErrorDiagnosticBodyVerdictNotRequested = "not_requested"
	ErrorDiagnosticBodyVerdictComplete     = "complete"
	ErrorDiagnosticBodyVerdictTooLarge     = "too_large"
	ErrorDiagnosticBodyVerdictIncomplete   = "incomplete"
	// ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable 是「封闭抑制」结论：
	// 本次确实请求了正文留存（门控要求它），但没有可用的稳定密钥，因此调用方**按策略
	// 主动扣住**了字节——传输层从未读取、也从未复制这份明文。
	//
	// 它与 NotRequested 是不同的事实，必须分开表达：NotRequested 的语义是「本次没有
	// opt-in 正文采集」（正常关闭），而这个结论是「要求过、但部署做不到」（配置故障）。
	// 两者如果共用 NotRequested，该行就会落成 not_observed，把配置故障显示成正常运行。
	//
	// 它是**封闭**结论：只会导致跳过，永远不会导致留存，因此即使被夹带字节也不会被保存；
	// 且它映射到既有的稳定原因码（见 DecideErrorDiagnosticBody），不新增任何存储枚举。
	ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable = "suppressed_encryption_unavailable"
)

// ErrorDiagnosticBodyVerdictAllowed 报告 verdict 是否在允许集合内。
func ErrorDiagnosticBodyVerdictAllowed(verdict string) bool {
	switch verdict {
	case "", ErrorDiagnosticBodyVerdictNotRequested, ErrorDiagnosticBodyVerdictComplete,
		ErrorDiagnosticBodyVerdictTooLarge, ErrorDiagnosticBodyVerdictIncomplete,
		ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable:
		return true
	default:
		return false
	}
}

// ErrorDiagnosticKnownCredentialKeys 是已知结构化凭据字段名（按小写比较）。
//
// 出现即整份正文不留存：不做脱敏后再自称「完整」。
var ErrorDiagnosticKnownCredentialKeys = []string{
	"fallback_credit_token",
	"authorization",
	"proxy-authorization",
	"x-api-key",
	"x-goog-api-key",
	"api_key",
	"apikey",
	"access_token",
	"refresh_token",
	"id_token",
	"client_secret",
	"client_assertion",
	"password",
	"passwd",
	"secret",
	"cookie",
	"set-cookie",
	"session_token",
	"private_key",
}

// ErrorDiagnosticAttachmentKeys 是出现即视为携带图片／文件／音频附件的字段名（按小写比较）。
//
// 只列**载体键**：这些键名在本能力覆盖的三类协议（Messages／Chat Completions／Responses）
// 的请求体里专门用于承载媒体，因此出现即可判为附件。
// 名字过于通用的键（data、source、image、url…）不在这里硬编码，改由
// ErrorDiagnosticMediaTypeKeys／ErrorDiagnosticMediaTypeMarkers 与
// ErrorDiagnosticEncodedPayloadKeys 组成的结构判定识别，
// 避免把工具 schema 里同名的普通属性误判为附件。
var ErrorDiagnosticAttachmentKeys = []string{
	"image_url",
	"input_image",
	"input_audio",
	"input_file",
	"file_id",
	"file_data",
	"file_url",
	"media_type",
	"document",
	"attachment",
	"inline_data",
	"image_base64",
}

// ErrorDiagnosticMediaTypeKeys 是媒体类型判别键（按小写比较）。
//
// 它们的**字符串值**给出该对象的媒体类型；只有值命中已知媒体类型或 MIME 前缀时才构成
// 「媒体部件候选」。值比较是闭集枚举，不做自由文本子串匹配。
var ErrorDiagnosticMediaTypeKeys = []string{
	"type",
	"media_type",
	"mime_type",
}

// ErrorDiagnosticMediaTypeMarkers 是已知媒体类型标记（比较前与键名同样折叠下划线与连字符）。
//
// 只列本能力覆盖的三类协议里**确实作为媒体部件类型**出现的取值：
// OpenAI 的 input_audio／input_image／input_file／image_url／file，
// Anthropic 的 image／document 内容块与 source 的 base64／url 编码类型。
// 键名本身能直接命中的媒体（inline_data、file_data 等）不在此重复，见
// ErrorDiagnosticAttachmentKeys。
var ErrorDiagnosticMediaTypeMarkers = []string{
	"input_audio",
	"input_image",
	"input_file",
	"image",
	"image_url",
	"file",
	"document",
	"base64",
	"url",
}

// errorDiagnosticMediaTypeValuePrefixes 是被判为媒体的 MIME 类型前缀（已折叠并小写）。
var errorDiagnosticMediaTypeValuePrefixes = []string{
	"image/",
	"audio/",
	"video/",
	"application/pdf",
	"application/octet-stream",
}

// ErrorDiagnosticEncodedPayloadKeys 是编码载荷字段名（按小写比较）。
//
// 单独出现不构成附件证据（data、url 这类键名在普通请求里同样常见），
// 只有与媒体类型判别值落在同一个对象（或该对象的直接子容器）里才算媒体部件。
var ErrorDiagnosticEncodedPayloadKeys = []string{
	"data",
	"base64",
	"b64",
	"b64_json",
	"data_uri",
	"bytes",
	"blob",
	"url",
	"uri",
}

// ErrorDiagnosticClassification 是对出站正文的安全分类结果。
//
// 分类只用于「能不能留存」，不用于改写内容：任何一项不合格都整份不留。
type ErrorDiagnosticClassification struct {
	IsTextJSON         bool
	HasAttachment      bool
	HasKnownCredential bool
}

// errorDiagnosticJSONFrame 跟踪当前容器是对象还是数组、对象是否在等下一个键，
// 以及本次遍历在该容器上看到的媒体结构证据。
//
// 每帧是固定大小，不含随正文增长的缓冲区，因此深层嵌套也不会带来与值等量的分配。
type errorDiagnosticJSONFrame struct {
	isObject  bool
	expectKey bool
	// pendingMediaTypeKey 记录刚读完的键是媒体类型判别键：它的值要按已知媒体类型比较。
	pendingMediaTypeKey bool
	// mediaTyped 表示本对象带已知媒体类型判别值。
	mediaTyped bool
	// hasEncodedPayload 表示本对象（或其直接子容器）带编码载荷字段。
	hasEncodedPayload bool
}

// errorDiagnosticKnownCredentialKeySet 等是折叠后的字段名集合。
// 折叠只在比较时发生，导出的原始名单保持可读的写法。
var (
	errorDiagnosticKnownCredentialKeySet = buildErrorDiagnosticKeySet(ErrorDiagnosticKnownCredentialKeys)
	errorDiagnosticAttachmentKeySet      = buildErrorDiagnosticKeySet(ErrorDiagnosticAttachmentKeys)
	errorDiagnosticMediaTypeKeySet       = buildErrorDiagnosticKeySet(ErrorDiagnosticMediaTypeKeys)
	errorDiagnosticMediaTypeMarkerSet    = buildErrorDiagnosticKeySet(ErrorDiagnosticMediaTypeMarkers)
	errorDiagnosticEncodedPayloadKeySet  = buildErrorDiagnosticKeySet(ErrorDiagnosticEncodedPayloadKeys)
)

func buildErrorDiagnosticKeySet(keys []string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[NormalizeErrorDiagnosticJSONKey(key)] = struct{}{}
	}
	return set
}

// ClassifyErrorDiagnosticBody 判定一段出站正文是否可直接留存。
//
// 保守优先：
//   - 必须是合法 UTF-8 且整体是合法 JSON（非文本、二进制、multipart 一律不合格）；
//   - 出现已知附件载体字段名（或任意 data: 编码载荷）即视为携带附件；
//   - 出现「媒体类型判别值 + 编码载荷」的媒体部件结构即视为携带附件；
//   - 出现已知结构化凭据字段名即视为含凭据。
//
// 字段名比较会先把下划线与连字符去掉再比对，因此 `access_token`、`accessToken`
// 与 `access-token` 是同一条规则；只比对**对象的键**，不比对值，
// 避免把用户自由文本里的普通单词误判成凭据字段（也不会因此漏掉结构化字段）。
//
// 媒体部件识别同样只看结构：判别键（type／media_type／mime_type）的值必须命中已知媒体类型
// 枚举或 MIME 前缀，编码载荷字段还必须与判别值落在同一个对象或其直接子容器里。
// 因此 `{"note":"the input_audio part"}` 这类自由文本、以及工具 schema 里名为
// data／source／url 的普通属性都不会被判为附件。
//
// 结构无法逐键举证时一律 fail-closed（整份判为不合格），不做弱判定：
// 遍历失败（键数超过上限，或解码器拒绝这种防御性情形）时**不再**退回按字面量做子串匹配，
// 因为那种弱判定只认正文里 snake_case／kebab-case 的键名字面量，
// camelCase 的已知凭据（例如 fallbackCreditToken）与「通用载体键 + 判别值」的媒体部件都会漏掉。
//
// 已知字段名之外的自由文本仍可能含客户自行写入的未知秘密，那是本能力上线前必须评审的
// 剩余风险，不能靠本函数自动识别，也不能因此声称「绝无秘密」。
func ClassifyErrorDiagnosticBody(body []byte) ErrorDiagnosticClassification {
	if len(body) == 0 {
		return ErrorDiagnosticClassification{}
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		return ErrorDiagnosticClassification{}
	}
	classification := ErrorDiagnosticClassification{IsTextJSON: true}
	if bytes.Contains(bytes.ToLower(body), []byte("data:")) {
		classification.HasAttachment = true
	}

	walked, result := errorDiagnosticClassifyJSONStructure(body)
	switch result {
	case errorDiagnosticJSONWalkComplete:
		classification.HasAttachment = classification.HasAttachment || walked.HasAttachment
		classification.HasKnownCredential = walked.HasKnownCredential
		return classification
	case errorDiagnosticJSONWalkKeyCapExceeded:
		// 键数超过上限：整份判为不合格，且绝不退回弱字面量判定。
		//
		// 这里必须 fail-closed，因为「用海量键把遍历推过上限」本身就是绕过结构化判定的路径：
		// 遍历被中止时，凭据与媒体部件都还没被看到，此时任何「没找到证据」都不成立。
		// 结论记在附件排除上（ADR 0005 的不合格项），而不是「非文本 JSON」：
		// 字节层面它确实是合法文本 JSON（IsTextJSON 保持为真），被拒绝的是「为这份结构背书」，
		// 既不是「找到了某个具体附件」，也不是「它不含凭据」。
		classification.HasAttachment = true
		return classification
	}
	// 除「完成」以外的结局一律整份不留：解码器拒绝（errorDiagnosticJSONWalkDecoderRejected）
	// 与将来可能新增的结局都按非文本 JSON 处理，绝不因为「没见过的结局」而默认合格。
	return ErrorDiagnosticClassification{}
}

// errorDiagnosticMaxWalkedJSONKeys 是正常遍历允许处理的键数上限。
//
// 超过上限的输入由调用方按 fail-closed 处理（整份不留，见 ClassifyErrorDiagnosticBody），
// 因此这里只累加计数、不缓存键名：内存占用与正文中的载荷大小、键数量都无关。
const errorDiagnosticMaxWalkedJSONKeys = 100_000

// errorDiagnosticJSONWalkResult 报告结构遍历的结局。
//
// 之所以不把失败笼统合成一个布尔值：两种失败是不同的事实，调用方必须为它们分别给出
// 安全的原因码，而不是让它们共用一个含糊的「未知」。
type errorDiagnosticJSONWalkResult int

const (
	// errorDiagnosticJSONWalkComplete 遍历读完整个正文并给出了完整结论。
	errorDiagnosticJSONWalkComplete errorDiagnosticJSONWalkResult = iota
	// errorDiagnosticJSONWalkKeyCapExceeded 键数超过 errorDiagnosticMaxWalkedJSONKeys：
	// 正文本身合法，但结构复杂到无法逐键举证「无附件、无凭据」，因此整份不留。
	errorDiagnosticJSONWalkKeyCapExceeded
	// errorDiagnosticJSONWalkDecoderRejected 解码器拒绝：正文不是可遍历的 JSON token 流。
	errorDiagnosticJSONWalkDecoderRejected
)

// errorDiagnosticClassifyJSONStructure 在一次 token 遍历里同时收集结构化凭据、附件载体键
// 与媒体部件三类证据。
//
// body 必须已经通过 json.Valid，因此这里的失败只可能是键数超限、解码器自身的嵌套上限
// 或实现问题。**任何失败都不构成「没找到证据」**：调用方一律整份不留，
// 不做弱判定（见 ClassifyErrorDiagnosticBody）。只读取键名与判别值，不复制任何值，
// 因此分配量与正文中的载荷大小无关。
func errorDiagnosticClassifyJSONStructure(body []byte) (ErrorDiagnosticClassification, errorDiagnosticJSONWalkResult) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	classification := ErrorDiagnosticClassification{IsTextJSON: true}
	stack := make([]errorDiagnosticJSONFrame, 0, 16)
	keys := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ErrorDiagnosticClassification{}, errorDiagnosticJSONWalkDecoderRejected
		}
		expectKey := len(stack) > 0 && stack[len(stack)-1].isObject && stack[len(stack)-1].expectKey

		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{', '[':
				if len(stack) > 0 {
					// 判别键的值不是字符串（例如对象或数组）时不算媒体类型证据。
					stack[len(stack)-1].pendingMediaTypeKey = false
				}
				stack = append(stack, errorDiagnosticJSONFrame{isObject: value == '{', expectKey: value == '{'})
			default: // '}' 或 ']'
				if len(stack) == 0 {
					return ErrorDiagnosticClassification{}, errorDiagnosticJSONWalkDecoderRejected
				}
				closed := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if closed.isObject && closed.mediaTyped && closed.hasEncodedPayload {
					classification.HasAttachment = true
				}
				markErrorDiagnosticJSONValueConsumed(stack, closed.hasEncodedPayload)
			}
		case string:
			if expectKey {
				keys++
				if keys > errorDiagnosticMaxWalkedJSONKeys {
					return ErrorDiagnosticClassification{}, errorDiagnosticJSONWalkKeyCapExceeded
				}
				normalized := NormalizeErrorDiagnosticJSONKey(value)
				if _, found := errorDiagnosticAttachmentKeySet[normalized]; found {
					classification.HasAttachment = true
				}
				if _, found := errorDiagnosticKnownCredentialKeySet[normalized]; found {
					classification.HasKnownCredential = true
				}
				if _, found := errorDiagnosticEncodedPayloadKeySet[normalized]; found {
					stack[len(stack)-1].hasEncodedPayload = true
				}
				_, isMediaTypeKey := errorDiagnosticMediaTypeKeySet[normalized]
				stack[len(stack)-1].expectKey = false
				stack[len(stack)-1].pendingMediaTypeKey = isMediaTypeKey
				continue
			}
			// 顶层标量（例如 `"text"`）是合法 JSON，此时栈为空：只消费值，不记录证据。
			if len(stack) > 0 && stack[len(stack)-1].pendingMediaTypeKey && errorDiagnosticMediaTypeValueMatches(value) {
				stack[len(stack)-1].mediaTyped = true
			}
			markErrorDiagnosticJSONValueConsumed(stack, false)
		default:
			if len(stack) > 0 {
				stack[len(stack)-1].pendingMediaTypeKey = false
			}
			markErrorDiagnosticJSONValueConsumed(stack, false)
		}
	}
	if len(stack) != 0 {
		return ErrorDiagnosticClassification{}, errorDiagnosticJSONWalkDecoderRejected
	}
	return classification, errorDiagnosticJSONWalkComplete
}

// errorDiagnosticMediaTypeValueMaxLen 是参与媒体类型比较的字符串值长度上限。
//
// 已知媒体类型与 MIME 前缀都很短；超过上限的值（例如 base64 载荷本身）直接跳过，
// 既不为巨型字符串做折叠拷贝，也不影响判别结果。
const errorDiagnosticMediaTypeValueMaxLen = 64

// errorDiagnosticMediaTypeValueMatches 报告判别键的字符串值是否是已知媒体类型标记或 MIME 前缀。
//
// 折叠规则与键名一致（去下划线／连字符、转小写），比较是闭集枚举而非子串搜索。
func errorDiagnosticMediaTypeValueMatches(value string) bool {
	if len(value) == 0 || len(value) > errorDiagnosticMediaTypeValueMaxLen {
		return false
	}
	folded := NormalizeErrorDiagnosticJSONKey(value)
	if _, found := errorDiagnosticMediaTypeMarkerSet[folded]; found {
		return true
	}
	for _, prefix := range errorDiagnosticMediaTypeValuePrefixes {
		if strings.HasPrefix(folded, prefix) {
			return true
		}
	}
	return false
}

// NormalizeErrorDiagnosticJSONKey 把 JSON 字段名折叠成比较用的规范形式。
//
// 去掉下划线与连字符并转小写，使 snake_case、camelCase 与 kebab-case 命中同一条规则。
func NormalizeErrorDiagnosticJSONKey(key string) string {
	var builder strings.Builder
	builder.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '_' || c == '-':
			continue
		case c >= 'A' && c <= 'Z':
			_ = builder.WriteByte(c + ('a' - 'A'))
		default:
			_ = builder.WriteByte(c)
		}
	}
	return builder.String()
}

// markErrorDiagnosticJSONValueConsumed 记下「当前容器的一个值已读完」，
// 于是所属对象重新进入「下一个 token 是键」的状态；
// 子容器带编码载荷时把这份证据上提一层：媒体载体键常常是对象（input_audio、source），
// 载荷位于它的下一层，而媒体类型判别值与载体键同层。
func markErrorDiagnosticJSONValueConsumed(stack []errorDiagnosticJSONFrame, childHasEncodedPayload bool) {
	if len(stack) == 0 {
		return
	}
	if stack[len(stack)-1].isObject {
		stack[len(stack)-1].expectKey = true
	}
	if childHasEncodedPayload {
		stack[len(stack)-1].hasEncodedPayload = true
	}
}

// ErrorDiagnosticRecord 是管理端可读的安全诊断事实，不含正文与任何身份字段。
type ErrorDiagnosticRecord struct {
	ID                 string
	UsageLogID         int64
	HasUsage           bool
	Protocol           string
	AttemptIndex       int
	Stage              string
	UpstreamStatusCode int
	BodyState          string
	BodyReason         string
	CreatedAt          time.Time
	MetadataExpiresAt  time.Time
	BodyExpiresAt      time.Time
	BodyBytes          int
	BodyKeyVersion     int
	// BodyStored 报告密文当前是否仍物理存在于在线主库；body_state 由它加上到期时刻推导。
	BodyStored bool

	// HeaderState／HeaderReason 与正文同构，但独立计算：头值可以留存而正文没有，反之亦然。
	HeaderState      string
	HeaderReason     string
	HeaderExpiresAt  time.Time
	HeaderBytes      int
	HeaderKeyVersion int
	HeaderEntryCount int
	// HeaderStored 报告头值密文当前是否仍物理存在于在线主库。
	HeaderStored bool
}

// ExpiredAt 报告元数据在 now 时刻是否已到期（应用层拒绝读取）。
func (r ErrorDiagnosticRecord) ExpiredAt(now time.Time) bool {
	return !r.MetadataExpiresAt.After(now)
}

// BodyReadableAt 报告正文在 now 时刻是否仍可解密读取。
func (r ErrorDiagnosticRecord) BodyReadableAt(now time.Time) bool {
	return r.BodyStored && r.BodyExpiresAt.After(now) && !r.ExpiredAt(now)
}

// HeaderValuesReadableAt 报告头值在 now 时刻是否仍可解密读取。
func (r ErrorDiagnosticRecord) HeaderValuesReadableAt(now time.Time) bool {
	return r.HeaderStored && r.HeaderExpiresAt.After(now) && !r.ExpiredAt(now)
}

// normalizeErrorDiagnosticRecord 用持久化事实重新推导 body_state，
// 使「已到期」与「已物理清除」在读取路径上不被误报为仍可读。
func normalizeErrorDiagnosticRecord(record ErrorDiagnosticRecord, now time.Time) ErrorDiagnosticRecord {
	record.BodyState = DescribeBodyState(record.BodyReason, record.BodyStored, record.BodyExpiresAt, now)
	record.HeaderState = DescribeHeaderState(record.HeaderReason, record.HeaderStored, record.HeaderExpiresAt, now)
	return record
}

// DescribeBodyState 把原因码与到期映射成对外的 body_state。
func DescribeBodyState(reason string, stored bool, bodyExpiresAt time.Time, now time.Time) string {
	if stored {
		if !bodyExpiresAt.After(now) {
			return ErrorDiagnosticBodyStateExpired
		}
		return ErrorDiagnosticBodyStateStored
	}
	switch reason {
	case ErrorDiagnosticBodyRetained:
		// 曾留存但密文已被清理：在线主库已无正文。
		return ErrorDiagnosticBodyStatePurged
	case ErrorDiagnosticBodyNotObserved, "":
		return ErrorDiagnosticBodyStateNotObserved
	default:
		return ErrorDiagnosticBodyStateSkipped
	}
}

// ErrorDiagnosticWrite 是服务判定后的写入决定。
//
// 到期时刻由存储层按 ErrorDiagnosticMetadataRetention / ErrorDiagnosticBodyRetention
// 各自计算，调用方不能自行指定保留窗口。
type ErrorDiagnosticWrite struct {
	ID             string
	Attempt        ErrorDiagnosticAttempt
	BodyState      string
	BodyReason     string
	BodyCiphertext []byte
	BodyKeyVersion int
	// HeaderCiphertext 是加密后的 429 头值 JSON；为空表示不留存头值。
	// 它与 BodyCiphertext 相互独立：一行的两列可以有任意组合。
	HeaderState      string
	HeaderReason     string
	HeaderCiphertext []byte
	HeaderKeyVersion int
	HeaderEntryCount int
	// HeaderPayloadBytes 是加密前 JSON 载荷的字节数（不是密文长度）。
	HeaderPayloadBytes int
}

// ErrorDiagnosticRepository 是诊断存储的持久化契约。
//
// 读取路径必须自行处理到期：即使清理任务延迟，也不能把已到期内容交还给调用方。
type ErrorDiagnosticRepository interface {
	// CreateErrorDiagnostic 写入一条独立诊断；BodyCiphertext 为空表示不留存正文。
	CreateErrorDiagnostic(ctx context.Context, write ErrorDiagnosticWrite, now time.Time) (ErrorDiagnosticRecord, error)
	// GetErrorDiagnostic 读取元数据；不存在或已删除返回 ErrErrorDiagnosticNotFound。
	GetErrorDiagnostic(ctx context.Context, id string) (ErrorDiagnosticRecord, error)
	// ListErrorDiagnosticsByUsageLog 读取某条使用记录关联的诊断（含重试前的失败尝试）。
	ListErrorDiagnosticsByUsageLog(ctx context.Context, usageLogID int64, limit int) ([]ErrorDiagnosticRecord, error)
	// ListRecentErrorDiagnostics 是无 usage 诊断的独立入口，按创建时间倒序。
	ListRecentErrorDiagnostics(ctx context.Context, protocol string, limit int) ([]ErrorDiagnosticRecord, error)
	// ListRecentErrorDiagnosticPage 是无 usage 诊断的分页入口；只返回未过第 30 天的记录。
	ListRecentErrorDiagnosticPage(ctx context.Context, protocol string, now time.Time, offset, limit int) ([]ErrorDiagnosticRecord, error)
	// CountRecentErrorDiagnostics 返回当前仍可读的诊断条数，供管理端展示真实总数。
	CountRecentErrorDiagnostics(ctx context.Context, protocol string, now time.Time) (int64, error)
	// ReadErrorDiagnosticBody 返回解密后的出站正文；未留存或已到期返回 ErrErrorDiagnosticBodyGone。
	ReadErrorDiagnosticBody(ctx context.Context, id string, now time.Time) ([]byte, error)
	// ReadErrorDiagnosticHeaderValues 返回解密并重新校验后的 429 头值；
	// 未留存、已到期、已清除或解密结果不合格一律返回 ErrErrorDiagnosticHeaderValuesGone。
	ReadErrorDiagnosticHeaderValues(ctx context.Context, id string, now time.Time) (ErrorDiagnosticHeaderValues, error)
	// ClearExpiredErrorDiagnosticBodies 在在线主库物理清除第 7 天到期的正文密文列。
	ClearExpiredErrorDiagnosticBodies(ctx context.Context, now time.Time, batch int) (int64, error)
	// ClearExpiredErrorDiagnosticHeaderValues 在在线主库物理清除第 7 天到期的头值密文列。
	ClearExpiredErrorDiagnosticHeaderValues(ctx context.Context, now time.Time, batch int) (int64, error)
	// DeleteExpiredErrorDiagnostics 在在线主库物理删除第 30 天到期的整行。
	DeleteExpiredErrorDiagnostics(ctx context.Context, now time.Time, batch int) (int64, error)
}

// ErrorDiagnosticCleanupBacklogReader 是存储层的**可选**能力：读取清理积压。
//
// 单独成接口而不是并入 ErrorDiagnosticRepository：监控探针不该强迫每个存储实现
// （以及所有测试替身）都长出这个方法。真实仓储实现它；不支持时清理照常运行，
// 只是没有积压观测，且读取方会得到明确的不支持错误，而不是被伪装成「积压为 0」。
type ErrorDiagnosticCleanupBacklogReader interface {
	ReadErrorDiagnosticCleanupBacklog(ctx context.Context, now time.Time) (ErrorDiagnosticCleanupBacklog, error)
}

// ErrorDiagnosticCleanupBacklog 是清理积压的只读监控视图。
//
// 它是「物理残留」而不是「仍可读取」：应用层在到期时刻就已拒绝读取。
// 积压只说明清理还没跑完（停机、积压或本轮批量未取完），因此这里也不承诺确切的物理删除时刻。
type ErrorDiagnosticCleanupBacklog struct {
	BodiesOverdue         int64
	RecordsOverdue        int64
	HeaderValuesOverdue   int64
	OldestBodyOverdueAt   time.Time
	OldestRecordOverdueAt time.Time
	OldestHeaderOverdueAt time.Time
}

// OldestOverdueSeconds 返回最老的超期时长（秒），供监控直接上报。
//
// 积压为空或无有效时间时返回 0。上报秒数而不上报时刻：时长足以判断清理是否卡住，
// 又不把内部时间线细节带进监控系统。
func (b ErrorDiagnosticCleanupBacklog) OldestOverdueSeconds(now time.Time) int64 {
	oldest := time.Time{}
	for _, candidate := range []time.Time{
		b.OldestBodyOverdueAt,
		b.OldestRecordOverdueAt,
		b.OldestHeaderOverdueAt,
	} {
		if !candidate.IsZero() && (oldest.IsZero() || candidate.Before(oldest)) {
			oldest = candidate
		}
	}
	if oldest.IsZero() {
		return 0
	}
	if seconds := now.Sub(oldest).Seconds(); seconds > 0 {
		return int64(seconds)
	}
	return 0
}

// ErrorDiagnosticBodyCipher 加密／解密留存的出站正文。
//
// 实现必须使用认证加密且不得提供明文回退；KeyVersion 用于标记密文所属密钥代。
type ErrorDiagnosticBodyCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
	KeyVersion() int
}

// ErrorDiagnosticMetricsSnapshot 是不含正文与凭据的计数快照。
type ErrorDiagnosticMetricsSnapshot struct {
	Attempts           int64
	StoredRecords      int64
	RejectedAttempts   int64
	DisabledSuppressed int64
	WriteFailures      int64
	BodyStored         int64
	BodySkipped        int64
	BodyReads          int64
	BodyReadDenied     int64
	BodiesCleared      int64
	RecordsDeleted     int64
	// HeaderValues* 是 429 头值留存的独立计数：与正文计数分开，便于分辨「哪一层没在采」。
	HeaderValuesStored     int64
	HeaderValuesSkipped    int64
	HeaderValuesReads      int64
	HeaderValuesReadDenied int64
	HeaderValuesCleared    int64
	CleanupFailures        int64
	// Dropped 统计因有界队列溢出而被丢弃的诊断（不含正文，也不代表已持久化）。
	Dropped int64
	// OverdueBodies／OverdueRecords／OverdueHeaderValues 是最近一次观测到的清理积压量
	// （物理残留，不是可读范围）。
	OverdueBodies       int64
	OverdueRecords      int64
	OverdueHeaderValues int64
	// OldestOverdueSeconds 是最近一次观测到的最老超期时长。
	OldestOverdueSeconds int64
}

// ErrorDiagnosticMetrics 是进程级计数，只累加离散事件，不记录正文、凭据或标识。
type ErrorDiagnosticMetrics struct {
	attempts               atomic.Int64
	storedRecords          atomic.Int64
	rejectedAttempts       atomic.Int64
	disabledSuppressed     atomic.Int64
	writeFailures          atomic.Int64
	bodyStored             atomic.Int64
	bodySkipped            atomic.Int64
	bodyReads              atomic.Int64
	bodyReadDenied         atomic.Int64
	bodiesCleared          atomic.Int64
	recordsDeleted         atomic.Int64
	headerValuesStored     atomic.Int64
	headerValuesSkipped    atomic.Int64
	headerValuesReads      atomic.Int64
	headerValuesReadDenied atomic.Int64
	headerValuesCleared    atomic.Int64
	cleanupFailures        atomic.Int64
	dropped                atomic.Int64
	overdueBodies          atomic.Int64
	overdueRecords         atomic.Int64
	overdueHeaders         atomic.Int64
	oldestOverdue          atomic.Int64
}

// NewErrorDiagnosticMetrics 创建一个进程级计数集。
func NewErrorDiagnosticMetrics() *ErrorDiagnosticMetrics { return &ErrorDiagnosticMetrics{} }

// Snapshot 返回可直接序列化的计数快照。
func (m *ErrorDiagnosticMetrics) Snapshot() ErrorDiagnosticMetricsSnapshot {
	if m == nil {
		return ErrorDiagnosticMetricsSnapshot{}
	}
	return ErrorDiagnosticMetricsSnapshot{
		Attempts:               m.attempts.Load(),
		StoredRecords:          m.storedRecords.Load(),
		RejectedAttempts:       m.rejectedAttempts.Load(),
		DisabledSuppressed:     m.disabledSuppressed.Load(),
		WriteFailures:          m.writeFailures.Load(),
		BodyStored:             m.bodyStored.Load(),
		BodySkipped:            m.bodySkipped.Load(),
		BodyReads:              m.bodyReads.Load(),
		BodyReadDenied:         m.bodyReadDenied.Load(),
		BodiesCleared:          m.bodiesCleared.Load(),
		RecordsDeleted:         m.recordsDeleted.Load(),
		HeaderValuesStored:     m.headerValuesStored.Load(),
		HeaderValuesSkipped:    m.headerValuesSkipped.Load(),
		HeaderValuesReads:      m.headerValuesReads.Load(),
		HeaderValuesReadDenied: m.headerValuesReadDenied.Load(),
		HeaderValuesCleared:    m.headerValuesCleared.Load(),
		CleanupFailures:        m.cleanupFailures.Load(),
		Dropped:                m.dropped.Load(),
		OverdueBodies:          m.overdueBodies.Load(),
		OverdueRecords:         m.overdueRecords.Load(),
		OverdueHeaderValues:    m.overdueHeaders.Load(),
		OldestOverdueSeconds:   m.oldestOverdue.Load(),
	}
}

// NewErrorDiagnosticID 生成一个不可猜的尝试标识（128 位随机，hex）。
func NewErrorDiagnosticID() (string, error) {
	buf := make([]byte, ErrorDiagnosticIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate error diagnostic id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ValidErrorDiagnosticID 报告 id 是否是本系统生成的 opaque 标识。
//
// 严格形状校验：长度与字符集都必须匹配，避免把任意字符串当成查询条件。
func ValidErrorDiagnosticID(id string) bool {
	if len(id) != ErrorDiagnosticIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// 诊断故障的稳定告警标识：运维按事件名与原因码聚合，不依赖日志正文。
const (
	ErrorDiagnosticAlertWriteFailed   = "error_diagnostic.write_failed"
	ErrorDiagnosticAlertDropped       = "error_diagnostic.dropped"
	ErrorDiagnosticAlertCleanupFailed = "error_diagnostic.cleanup_failed"

	ErrorDiagnosticAlertCodeDBError         = "db_error"
	ErrorDiagnosticAlertCodeUnavailable     = "unavailable"
	ErrorDiagnosticAlertCodeQueueOverflow   = "queue_overflow"
	ErrorDiagnosticAlertCodeBodyClearFailed = "body_clear_failed"
	// ErrorDiagnosticAlertCodeHeaderValueClearFailed 与正文清除失败分开成不同的原因码：
	// 两段卡住的处置不同（一个是模型正文，一个是 429 头值），合并会让运维看不出是哪一段。
	ErrorDiagnosticAlertCodeHeaderValueClearFailed = "header_value_clear_failed"
	ErrorDiagnosticAlertCodeRecordDeleteFailed     = "record_delete_failed"
	ErrorDiagnosticAlertCodeBacklog                = "cleanup_backlog"
	ErrorDiagnosticAlertCodeBacklogProbeFailed     = "backlog_probe_failed"
)

// ErrorDiagnosticAlertCleanupBacklog 是清理积压的稳定事件名。
const ErrorDiagnosticAlertCleanupBacklog = "error_diagnostic.cleanup_backlog"

// ErrorDiagnosticAlertInterval 是同一告警事件的最小输出间隔。
//
// 期间只累加被抑制的次数，不发新行；下一次到期时把累计次数一次性带出。
// 这样上游错误风暴下日志量有上界，而计数不会丢。
const ErrorDiagnosticAlertInterval = 30 * time.Second

// errorDiagnosticAlerter 以有界速率输出不含敏感内容的诊断故障告警。
//
// 只输出事件名、原因码与计数三类信息：绝不输出数据库错误值（约束冲突可能回显涉及的行值）、
// 正文、凭据、诊断标识或协议以外的请求字段。
type errorDiagnosticAlerter struct {
	mu         sync.Mutex
	interval   time.Duration
	now        func() time.Time
	lastAt     map[string]time.Time
	suppressed map[string]int64
}

func newErrorDiagnosticAlerter(interval time.Duration, now func() time.Time) *errorDiagnosticAlerter {
	if interval <= 0 {
		interval = ErrorDiagnosticAlertInterval
	}
	if now == nil {
		now = time.Now
	}
	return &errorDiagnosticAlerter{
		interval:   interval,
		now:        now,
		lastAt:     map[string]time.Time{},
		suppressed: map[string]int64{},
	}
}

// warn 在允许的时刻输出一条告警；被抑制的次数会累积到下一次输出。
//
// 这是唯一的日志出口，且只接受稳定原因码，因此任何调用方都无法把正文或原始错误带进日志。
func (a *errorDiagnosticAlerter) warn(event, code string, total int64, extra ...zap.Field) {
	if a == nil {
		return
	}
	// 限速按「事件 + 原因码」分桶，而不是只按事件分桶：
	// 否则同一事件下两种不同的故障会互相压制，运维只会看到先出现的那一种，
	// 可能因此错过「两段清理都在失败」这类组合故障。
	key := event + "|" + code
	a.mu.Lock()
	now := a.now()
	last, seen := a.lastAt[key]
	if seen && now.Sub(last) < a.interval {
		a.suppressed[key]++
		a.mu.Unlock()
		return
	}
	sinceLast := a.suppressed[key] + 1
	a.suppressed[key] = 0
	a.lastAt[key] = now
	a.mu.Unlock()

	logger.L().Warn(event, append([]zap.Field{
		zap.String("code", code),
		zap.Int64("count_since_last_alert", sinceLast),
		zap.Int64("total", total),
	}, extra...)...)
}

// warnBacklog 输出积压告警，附带积压明细（数量与最老超期时长，均非敏感内容）。
func (a *errorDiagnosticAlerter) warnBacklog(event, code string, total int64, extra ...zap.Field) {
	a.warn(event, code, total, extra...)
}

// ErrorDiagnosticService 是错误诊断的领域入口。
//
// 写入路径 fail-open：调用方（网关接缝）在拿到错误时忽略它并继续原本的上游／客户端流程；
// 服务本身不做重试、不阻塞、不改变任何响应。读取路径 fail-closed：任何异常都返回错误，
// 绝不返回未经加密或已到期的内容。
type ErrorDiagnosticService struct {
	repo     ErrorDiagnosticRepository
	settings ErrorDiagnosticSettingsReader
	cipher   ErrorDiagnosticBodyCipher
	metrics  *ErrorDiagnosticMetrics
	alerts   *errorDiagnosticAlerter
	now      func() time.Time
	newID    func() (string, error)
}

// NewErrorDiagnosticService 构造诊断服务。cipher 可为 nil，此时正文一律不留存。
func NewErrorDiagnosticService(repo ErrorDiagnosticRepository, settings ErrorDiagnosticSettingsReader, cipher ErrorDiagnosticBodyCipher) *ErrorDiagnosticService {
	return &ErrorDiagnosticService{
		repo:     repo,
		settings: settings,
		cipher:   cipher,
		metrics:  NewErrorDiagnosticMetrics(),
		alerts:   newErrorDiagnosticAlerter(ErrorDiagnosticAlertInterval, time.Now),
		now:      time.Now,
		newID:    NewErrorDiagnosticID,
	}
}

// Metrics 暴露进程级计数供运维读取；返回值为 nil 安全。
func (s *ErrorDiagnosticService) Metrics() *ErrorDiagnosticMetrics {
	if s == nil {
		return nil
	}
	return s.metrics
}

// Settings 读取当前门控配置。读取失败或未配置时返回全关，不做隐式开启。
func (s *ErrorDiagnosticService) Settings(ctx context.Context) ErrorDiagnosticSettings {
	if s == nil || s.settings == nil {
		return ErrorDiagnosticSettings{}
	}
	settings, err := s.settings.GetErrorDiagnosticSettings(ctx)
	if err != nil {
		return ErrorDiagnosticSettings{}
	}
	return settings
}

// CaptureEnabled 报告当前是否允许写入诊断元数据（采集开关 + 显式风险确认）。
func (s *ErrorDiagnosticService) CaptureEnabled(ctx context.Context) bool {
	return s.Settings(ctx).CaptureAllowed()
}

// ValidateErrorDiagnosticAttempt 执行严格白名单与边界校验。
//
// 不满足条件的输入不产生任何行：非覆盖分支、本地拒绝、网络故障都不能伪装成 4xx/5xx 诊断。
func ValidateErrorDiagnosticAttempt(attempt ErrorDiagnosticAttempt) error {
	switch attempt.Protocol {
	case ErrorDiagnosticProtocolMessages, ErrorDiagnosticProtocolChatCompletions, ErrorDiagnosticProtocolResponses:
	default:
		return fmt.Errorf("%w: unsupported protocol", ErrErrorDiagnosticInvalidAttempt)
	}
	if attempt.Stage != ErrorDiagnosticStageWire {
		return fmt.Errorf("%w: unsupported stage", ErrErrorDiagnosticInvalidAttempt)
	}
	if attempt.UpstreamStatusCode < 400 || attempt.UpstreamStatusCode > 599 {
		return fmt.Errorf("%w: upstream status out of range", ErrErrorDiagnosticInvalidAttempt)
	}
	if attempt.AttemptIndex < 0 || attempt.AttemptIndex > ErrorDiagnosticMaxAttemptIndex {
		return fmt.Errorf("%w: attempt index out of range", ErrErrorDiagnosticInvalidAttempt)
	}
	if attempt.UsageLogID < 0 {
		return fmt.Errorf("%w: negative usage log id", ErrErrorDiagnosticInvalidAttempt)
	}
	if !ErrorDiagnosticBodyVerdictAllowed(attempt.BodyVerdict) {
		// 未知 verdict 只有在同时夹带了字节时才算不合格：服务无法为这些字节背书，
		// 既不能留存也不能谎称已完整读取。没有字节时它等同于「未观察到正文」。
		if len(attempt.Body) > 0 {
			return fmt.Errorf("%w: unsupported body verdict", ErrErrorDiagnosticInvalidAttempt)
		}
	}
	if !ErrorDiagnosticHeaderVerdictAllowed(attempt.HeaderVerdict) {
		// 头值同理：未知结论只有在同时夹带了头值时才算不合格，否则等同于「未观察到头值」。
		if !attempt.HeaderValues.Empty() {
			return fmt.Errorf("%w: unsupported header verdict", ErrErrorDiagnosticInvalidAttempt)
		}
	}
	return nil
}

// DecideErrorDiagnosticBody 判定该次尝试的正文状态与原因，并给出可否留存的结论。
//
// 判定只在服务侧进行：调用方不能自称 stored，也不能在缺密钥时退化为明文。
// 越界或不合格的正文只影响正文，不影响该次元数据落库。
// 判定顺序固定：transport 的显式 verdict 优先（含「按策略扣住字节」的封闭抑制结论），
// 其次体量（超限优先于截断），再读取完整性，再内容分类，最后才是策略与密钥。
func DecideErrorDiagnosticBody(attempt ErrorDiagnosticAttempt, bodyCaptureAllowed bool, cipherAvailable bool) (state string, reason string, retain bool) {
	// transport 扣住字节的情况必须显式表达，否则无法与「没有 opt-in」区分。
	switch attempt.BodyVerdict {
	case ErrorDiagnosticBodyVerdictNotRequested:
		return ErrorDiagnosticBodyStateNotObserved, ErrorDiagnosticBodyNotObserved, false
	case ErrorDiagnosticBodyVerdictTooLarge:
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedTooLarge, false
	case ErrorDiagnosticBodyVerdictIncomplete:
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedIncompleteRead, false
	case ErrorDiagnosticBodyVerdictSuppressedEncryptionUnavailable:
		// 封闭抑制：要求过留存、部署拿不出稳定密钥，调用方因此一个字节都没读。
		// 映射到既有的稳定原因码 skipped_encryption_unavailable，不新增存储枚举，
		// 也绝不因为「要求过」而把正文当作可留存。
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedEncryptionUnavailable, false
	}
	if len(attempt.Body) == 0 {
		return ErrorDiagnosticBodyStateNotObserved, ErrorDiagnosticBodyNotObserved, false
	}
	if len(attempt.Body) > ErrorDiagnosticMaxBodyBytes {
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedTooLarge, false
	}
	if !attempt.BodyReadComplete {
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedIncompleteRead, false
	}
	classification := ClassifyErrorDiagnosticBody(attempt.Body)
	if !classification.IsTextJSON {
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedNotTextJSON, false
	}
	if classification.HasAttachment {
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedAttachment, false
	}
	if classification.HasKnownCredential {
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedKnownCredential, false
	}
	if !bodyCaptureAllowed {
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedRetentionDisabled, false
	}
	if !cipherAvailable {
		return ErrorDiagnosticBodyStateSkipped, ErrorDiagnosticBodySkippedEncryptionUnavailable, false
	}
	return ErrorDiagnosticBodyStateStored, ErrorDiagnosticBodyRetained, true
}

// NormalizeErrorDiagnosticListLimit 把列表条数限制在 [1, ErrorDiagnosticMaxListLimit]。
func NormalizeErrorDiagnosticListLimit(limit int) int {
	if limit <= 0 {
		return ErrorDiagnosticDefaultListLimit
	}
	if limit > ErrorDiagnosticMaxListLimit {
		return ErrorDiagnosticMaxListLimit
	}
	return limit
}

// NormalizeErrorDiagnosticOffset 把翻页偏移收敛到 [0, ErrorDiagnosticMaxListOffset]。
//
// 负数一律归零。上限只是防滥用的健全性边界，远大于任何真实的翻页需求，
// 因此调用方可以按真实 total 翻到任意存在的一页，不会因为被夹到某个较小值而
// 把上一页的行当成下一页返回。
func NormalizeErrorDiagnosticOffset(offset int) int {
	if offset <= 0 {
		return 0
	}
	if offset > ErrorDiagnosticMaxListOffset {
		return ErrorDiagnosticMaxListOffset
	}
	return offset
}

// RecordErrorDiagnostic 记录一次真实上游失败尝试。
//
// 返回的错误由调用方决定是否忽略：诊断写入失败不得改变原本的上游／客户端结果。
func (s *ErrorDiagnosticService) RecordErrorDiagnostic(ctx context.Context, attempt ErrorDiagnosticAttempt) (ErrorDiagnosticRecord, error) {
	if s == nil || s.repo == nil {
		return ErrorDiagnosticRecord{}, ErrErrorDiagnosticUnavailable
	}
	if s.metrics != nil {
		s.metrics.attempts.Add(1)
	}
	if err := ValidateErrorDiagnosticAttempt(attempt); err != nil {
		if s.metrics != nil {
			s.metrics.rejectedAttempts.Add(1)
		}
		return ErrorDiagnosticRecord{}, err
	}
	settings := s.Settings(ctx)
	if !settings.CaptureAllowed() {
		if s.metrics != nil {
			s.metrics.disabledSuppressed.Add(1)
		}
		return ErrorDiagnosticRecord{}, ErrErrorDiagnosticDisabled
	}
	state, reason, retain := DecideErrorDiagnosticBody(attempt, settings.BodyCaptureAllowed(), s.cipher != nil)
	write := ErrorDiagnosticWrite{
		Attempt:    attempt,
		BodyState:  state,
		BodyReason: reason,
	}
	if retain {
		encrypted, err := s.cipher.Encrypt(attempt.Body)
		if err != nil || len(encrypted) == 0 {
			// 加密失败绝不回退为明文，也不自称已留存。
			write.BodyState = ErrorDiagnosticBodyStateSkipped
			write.BodyReason = ErrorDiagnosticBodySkippedEncryptionUnavailable
		} else {
			write.BodyCiphertext = encrypted
			write.BodyKeyVersion = s.cipher.KeyVersion()
		}
	}
	// 头值是与正文正交的第二条留存路径：它有自己的开关与到期时刻，判定顺序
	// （范围 → verdict → 白名单与有界校验 → 开关 → 密钥）见 DecideErrorDiagnosticHeaderValues。
	// 正文结论不参与这里的任何一步，反之亦然。
	headerDecision := DecideErrorDiagnosticHeaderValues(attempt, settings.HeaderValuesCaptureAllowed(), s.cipher != nil)
	write.HeaderState = headerDecision.State
	write.HeaderReason = headerDecision.Reason
	write.HeaderEntryCount = headerDecision.EntryCount
	if headerDecision.Retained() {
		encrypted, err := s.cipher.Encrypt(headerDecision.Payload)
		if err != nil || len(encrypted) == 0 {
			// 与正文同一约定：加密失败绝不回退为明文，也不自称已留存。
			write.HeaderState = ErrorDiagnosticHeaderStateSkipped
			write.HeaderReason = ErrorDiagnosticHeaderSkippedEncryptionUnavailable
			write.HeaderEntryCount = 0
		} else {
			write.HeaderCiphertext = encrypted
			write.HeaderKeyVersion = s.cipher.KeyVersion()
			write.HeaderPayloadBytes = len(headerDecision.Payload)
		}
	}
	id, err := s.newID()
	if err != nil || !ValidErrorDiagnosticID(id) {
		if s.metrics != nil {
			s.metrics.writeFailures.Add(1)
		}
		return ErrorDiagnosticRecord{}, ErrErrorDiagnosticUnavailable
	}
	write.ID = id
	record, err := s.repo.CreateErrorDiagnostic(ctx, write, s.now())
	if err != nil {
		failures := int64(1)
		if s.metrics != nil {
			failures = s.metrics.writeFailures.Add(1)
		}
		// 只报原因码与计数：底层错误值可能回显约束冲突涉及的行值，绝不进入日志。
		// 写出失败本身就是这里要报的事实，而不是调用方等待的结果。
		s.alerts.warn(ErrorDiagnosticAlertWriteFailed, classifyErrorDiagnosticWriteFailure(err), failures)
		return ErrorDiagnosticRecord{}, err
	}
	record = normalizeErrorDiagnosticRecord(record, s.now())
	if s.metrics != nil {
		s.metrics.storedRecords.Add(1)
		if record.BodyState == ErrorDiagnosticBodyStateStored {
			s.metrics.bodyStored.Add(1)
		} else {
			s.metrics.bodySkipped.Add(1)
		}
		// 头值只统计「在范围内、但没留下」的那部分为跳过：不在范围的行（其它协议与其它状态码）
		// 记 skipped 会把未采集伪装成采集失败，指标就再也说明不了头值这一层是否在跑。
		switch {
		case record.HeaderState == ErrorDiagnosticHeaderStateStored:
			s.metrics.headerValuesStored.Add(1)
		case strings.HasPrefix(record.HeaderReason, "skipped_"):
			s.metrics.headerValuesSkipped.Add(1)
		}
	}
	return record, nil
}

// GetErrorDiagnostic 读取一条诊断元数据；已到期（第 30 天）或 ID 不合法时按不存在处理。
func (s *ErrorDiagnosticService) GetErrorDiagnostic(ctx context.Context, id string) (ErrorDiagnosticRecord, error) {
	if s == nil || s.repo == nil || !ValidErrorDiagnosticID(id) {
		return ErrorDiagnosticRecord{}, ErrErrorDiagnosticNotFound
	}
	record, err := s.repo.GetErrorDiagnostic(ctx, id)
	if err != nil {
		return ErrorDiagnosticRecord{}, err
	}
	now := s.now()
	if record.ExpiredAt(now) {
		return ErrorDiagnosticRecord{}, ErrErrorDiagnosticNotFound
	}
	return normalizeErrorDiagnosticRecord(record, now), nil
}

// ListErrorDiagnosticsByUsageLog 读取某条使用记录关联的失败尝试；usageLogID 无效时返回空列表。
//
// 顺序是按尝试时间正序，便于按逻辑请求还原「先失败、后重试成功」的时间线。
func (s *ErrorDiagnosticService) ListErrorDiagnosticsByUsageLog(ctx context.Context, usageLogID int64, limit int) ([]ErrorDiagnosticRecord, error) {
	if s == nil || s.repo == nil {
		return nil, ErrErrorDiagnosticUnavailable
	}
	if usageLogID <= 0 {
		return nil, nil
	}
	records, err := s.repo.ListErrorDiagnosticsByUsageLog(ctx, usageLogID, NormalizeErrorDiagnosticListLimit(limit))
	if err != nil {
		return nil, err
	}
	return viewErrorDiagnosticRecords(records, s.now()), nil
}

// ListRecentErrorDiagnostics 是无 usage 诊断的独立入口；protocol 为空表示不限协议。
//
// 顺序是按创建时间倒序（由存储层保证），并过滤掉已过第 30 天的记录，
// 使清理任务延迟时列表也不会把已到期内容交还给调用方。
func (s *ErrorDiagnosticService) ListRecentErrorDiagnostics(ctx context.Context, protocol string, limit int) ([]ErrorDiagnosticRecord, error) {
	if s == nil || s.repo == nil {
		return nil, ErrErrorDiagnosticUnavailable
	}
	if protocol != "" {
		switch protocol {
		case ErrorDiagnosticProtocolMessages, ErrorDiagnosticProtocolChatCompletions, ErrorDiagnosticProtocolResponses:
		default:
			return nil, fmt.Errorf("%w: unsupported protocol", ErrErrorDiagnosticInvalidAttempt)
		}
	}
	records, err := s.repo.ListRecentErrorDiagnostics(ctx, protocol, NormalizeErrorDiagnosticListLimit(limit))
	if err != nil {
		return nil, err
	}
	return viewErrorDiagnosticRecords(records, s.now()), nil
}

// ListRecentErrorDiagnosticPage 是无 usage 诊断的分页入口。
//
// offset 与 limit 都被收敛到有界范围，且存储层只返回未过第 30 天的记录，
// 使 page/page_size 的翻页不会把已到期的元数据交还给调用方。
func (s *ErrorDiagnosticService) ListRecentErrorDiagnosticPage(ctx context.Context, protocol string, offset, limit int) ([]ErrorDiagnosticRecord, error) {
	if s == nil || s.repo == nil {
		return nil, ErrErrorDiagnosticUnavailable
	}
	if protocol != "" {
		switch protocol {
		case ErrorDiagnosticProtocolMessages, ErrorDiagnosticProtocolChatCompletions, ErrorDiagnosticProtocolResponses:
		default:
			return nil, fmt.Errorf("%w: unsupported protocol", ErrErrorDiagnosticInvalidAttempt)
		}
	}
	records, err := s.repo.ListRecentErrorDiagnosticPage(ctx, protocol, s.now(), NormalizeErrorDiagnosticOffset(offset), NormalizeErrorDiagnosticListLimit(limit))
	if err != nil {
		return nil, err
	}
	return viewErrorDiagnosticRecords(records, s.now()), nil
}

// CountRecentErrorDiagnostics 返回当前仍可读的诊断条数，供管理端展示真实总数。
func (s *ErrorDiagnosticService) CountRecentErrorDiagnostics(ctx context.Context, protocol string) (int64, error) {
	if s == nil || s.repo == nil {
		return 0, ErrErrorDiagnosticUnavailable
	}
	if protocol != "" {
		switch protocol {
		case ErrorDiagnosticProtocolMessages, ErrorDiagnosticProtocolChatCompletions, ErrorDiagnosticProtocolResponses:
		default:
			return 0, fmt.Errorf("%w: unsupported protocol", ErrErrorDiagnosticInvalidAttempt)
		}
	}
	return s.repo.CountRecentErrorDiagnostics(ctx, protocol, s.now())
}

// viewErrorDiagnosticRecords 在读取路径上过滤已过第 30 天的记录并推导 body_state，
// 使清理任务延迟时列表也不会把已到期内容交还给调用方。
func viewErrorDiagnosticRecords(records []ErrorDiagnosticRecord, now time.Time) []ErrorDiagnosticRecord {
	out := make([]ErrorDiagnosticRecord, 0, len(records))
	for _, record := range records {
		if record.ExpiredAt(now) {
			continue
		}
		out = append(out, normalizeErrorDiagnosticRecord(record, now))
	}
	return out
}

// RecordDroppedErrorDiagnostic 记录一次因有界队列溢出而被丢弃的诊断。
//
// 这是给 transport 有界转发器用的非阻塞接缝：不读设置、不加密、不访问数据库，
// 只累加一个不含正文、凭据或标识的计数。调用方在队列满时丢弃诊断并调用它，
// 绝不允许为此阻塞或回压正在进行的上游请求。
func (s *ErrorDiagnosticService) RecordDroppedErrorDiagnostic() {
	if s == nil || s.metrics == nil {
		return
	}
	dropped := s.metrics.dropped.Add(1)
	// 计数之外还要留下有界速率的告警：运维必须能区分「没有错误」与「诊断一直在被丢弃」。
	// warn 内部按 ErrorDiagnosticAlertInterval 限速并把被抑制的次数聚合成下一次的计数，
	// 因此这条路径最多每 30 秒输出一行，不构成日志放大。
	s.alerts.warn(ErrorDiagnosticAlertDropped, ErrorDiagnosticAlertCodeQueueOverflow, dropped)
}

// classifyErrorDiagnosticWriteFailure 把写入失败收敛成稳定原因码。
//
// 不回传 err 本身：原因码是运维聚合用的，错误值只用于判定分类。
func classifyErrorDiagnosticWriteFailure(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrErrorDiagnosticUnavailable):
		return ErrorDiagnosticAlertCodeUnavailable
	default:
		return ErrorDiagnosticAlertCodeDBError
	}
}

// ReadErrorDiagnosticBody 解密并返回一次失败尝试的出站正文。
//
// 这是显式动作：默认列表与普通用户永远拿不到正文，只有管理端显式请求且未到期才返回内容。
// 未留存、已物理清除、已到期或 ID 不合法都返回 ErrErrorDiagnosticBodyGone。
func (s *ErrorDiagnosticService) ReadErrorDiagnosticBody(ctx context.Context, id string) ([]byte, error) {
	if s == nil || s.repo == nil || s.cipher == nil || !ValidErrorDiagnosticID(id) {
		if s != nil && s.metrics != nil {
			s.metrics.bodyReadDenied.Add(1)
		}
		return nil, ErrErrorDiagnosticBodyGone
	}
	body, err := s.repo.ReadErrorDiagnosticBody(ctx, id, s.now())
	if err != nil {
		if s.metrics != nil && errors.Is(err, ErrErrorDiagnosticBodyGone) {
			s.metrics.bodyReadDenied.Add(1)
		}
		return nil, err
	}
	if len(body) > ErrorDiagnosticMaxBodyReadBytes {
		// 解密结果越界说明存储被篡改或密钥错配，按不可用处理，不返回内容。
		if s.metrics != nil {
			s.metrics.bodyReadDenied.Add(1)
		}
		return nil, ErrErrorDiagnosticBodyGone
	}
	if s.metrics != nil {
		s.metrics.bodyReads.Add(1)
	}
	return body, nil
}

// Counters 返回当前计数快照。
func (s *ErrorDiagnosticService) Counters() ErrorDiagnosticMetricsSnapshot {
	if s == nil {
		return ErrorDiagnosticMetricsSnapshot{}
	}
	return s.metrics.Snapshot()
}

// ReadErrorDiagnosticHeaderValues 解密并返回一次失败尝试的 429 头值。
//
// 与正文同一契约：这是显式动作，只有管理端显式请求且未到期才返回内容；未留存、
// 已物理清除、已按 7 天到期、ID 不合法、密钥缺失或解密结果不合格，一律返回
// ErrErrorDiagnosticHeaderValuesGone——使读取结果不能作为「这条尝试留过什么」的探针。
func (s *ErrorDiagnosticService) ReadErrorDiagnosticHeaderValues(ctx context.Context, id string) (ErrorDiagnosticHeaderValues, error) {
	if s == nil || s.repo == nil || s.cipher == nil || !ValidErrorDiagnosticID(id) {
		if s != nil && s.metrics != nil {
			s.metrics.headerValuesReadDenied.Add(1)
		}
		return ErrorDiagnosticHeaderValues{}, ErrErrorDiagnosticHeaderValuesGone
	}
	values, err := s.repo.ReadErrorDiagnosticHeaderValues(ctx, id, s.now())
	if err != nil {
		if s.metrics != nil && errors.Is(err, ErrErrorDiagnosticHeaderValuesGone) {
			s.metrics.headerValuesReadDenied.Add(1)
		}
		return ErrorDiagnosticHeaderValues{}, err
	}
	if s.metrics != nil {
		s.metrics.headerValuesReads.Add(1)
	}
	return values, nil
}
