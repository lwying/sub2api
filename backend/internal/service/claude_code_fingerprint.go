package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const claudeFingerprintIDsContextKey = "claude_fingerprint_ids"

func stageClaudeFingerprintIDs(c *gin.Context, ids *claudeFingerprintIDs) {
	if c != nil {
		c.Set(claudeFingerprintIDsContextKey, ids)
	}
}

// stagedClaudeFingerprintIDs 读取本 attempt 暂存的收敛 ID。
// 两层门控：这里只按平台判定（UsesClaudeProtocol 是平台谓词，Bedrock/Vertex 同样说
// Anthropic 协议），凭据类型与 opt-in 由 GetClaudeFingerprintMode 把关——未显式开启
// 的账号根本不会被暂存，因此本函数读不到值，出站头也不会被改写。
// 账号 ID 必须匹配：failover 换号后上一账号的 ID 不得被应用到新账号。
func stagedClaudeFingerprintIDs(c *gin.Context, account *Account) *claudeFingerprintIDs {
	if c == nil || account == nil || !account.UsesClaudeProtocol() {
		return nil
	}
	value, ok := c.Get(claudeFingerprintIDsContextKey)
	if !ok {
		return nil
	}
	ids, ok := value.(*claudeFingerprintIDs)
	if !ok || ids == nil || ids.accountID != account.ID {
		return nil
	}
	return ids
}

type claudeFingerprintMode string

const (
	claudeFingerprintOff     claudeFingerprintMode = "off"
	claudeFingerprintDevice  claudeFingerprintMode = "device"
	claudeFingerprintSession claudeFingerprintMode = "session"
	claudeFingerprintFull    claudeFingerprintMode = "full"
)

const (
	claudeFingerprintModeExtraKey = "claude_fingerprint_mode"
	claudeFingerprintSeedExtraKey = "claude_fingerprint_seed"
)

func canonicalClaudeFingerprintSeed(value any) (string, bool) {
	raw, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(raw)
	parsed, err := uuid.Parse(trimmed)
	if err != nil || parsed == uuid.Nil || trimmed != parsed.String() {
		return "", false
	}
	return trimmed, true
}

func newClaudeFingerprintSeed() string {
	return uuid.NewString()
}

func stripClaudeFingerprintSeed(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	stripped := maps.Clone(extra)
	delete(stripped, claudeFingerprintSeedExtraKey)
	return stripped
}

func claudeFingerprintModeFromExtra(extra map[string]any) claudeFingerprintMode {
	if extra == nil {
		return claudeFingerprintOff
	}
	raw, _ := extra[claudeFingerprintModeExtraKey].(string)
	switch claudeFingerprintMode(strings.TrimSpace(raw)) {
	case claudeFingerprintOff, claudeFingerprintDevice, claudeFingerprintSession, claudeFingerprintFull:
		return claudeFingerprintMode(strings.TrimSpace(raw))
	default:
		return claudeFingerprintOff
	}
}

func claudeFingerprintModeRequiresSeed(mode claudeFingerprintMode) bool {
	switch mode {
	case claudeFingerprintDevice, claudeFingerprintSession, claudeFingerprintFull:
		return true
	default:
		return false
	}
}

func claudeFingerprintSeed(extra map[string]any) (string, bool) {
	if extra == nil {
		return "", false
	}
	return canonicalClaudeFingerprintSeed(extra[claudeFingerprintSeedExtraKey])
}

func prepareClaudeFingerprintExtraForCreate(platform, accountType string, extra map[string]any) map[string]any {
	prepared := stripClaudeFingerprintSeed(extra)
	if platform != PlatformAnthropic ||
		(accountType != AccountTypeOAuth && accountType != AccountTypeSetupToken) ||
		!claudeFingerprintModeRequiresSeed(claudeFingerprintModeFromExtra(prepared)) {
		return prepared
	}
	if prepared == nil {
		prepared = make(map[string]any, 1)
	}
	prepared[claudeFingerprintSeedExtraKey] = newClaudeFingerprintSeed()
	return prepared
}

func prepareClaudeFingerprintExtraForUpdate(account *Account, extra map[string]any) map[string]any {
	prepared := stripClaudeFingerprintSeed(extra)
	if account == nil || !account.IsClaudeOAuthLike() {
		return prepared
	}
	if seed, ok := claudeFingerprintSeed(account.Extra); ok {
		if prepared == nil {
			prepared = make(map[string]any, 1)
		}
		prepared[claudeFingerprintSeedExtraKey] = seed
		return prepared
	}
	if claudeFingerprintModeRequiresSeed(claudeFingerprintModeFromExtra(prepared)) {
		if prepared == nil {
			prepared = make(map[string]any, 1)
		}
		prepared[claudeFingerprintSeedExtraKey] = newClaudeFingerprintSeed()
	}
	return prepared
}

// sanitizedClaudeFingerprintExtraUpdates 用于 key 级 JSONB 更新载荷：剥离调用方
// 提供的 seed（真值由仓库层原子保活）。语义与 stripClaudeFingerprintSeed 完全一致，
// 保留本名是因为它标注的是调用点意图（净化更新载荷），与 Codex 侧同名函数对称。
func sanitizedClaudeFingerprintExtraUpdates(updates map[string]any) map[string]any {
	return stripClaudeFingerprintSeed(updates)
}

func ShouldEnsureClaudeFingerprintSeedForExtraUpdates(updates map[string]any) bool {
	if updates == nil {
		return false
	}
	return claudeFingerprintModeRequiresSeed(claudeFingerprintModeFromExtra(updates))
}

func (a *Account) GetClaudeFingerprintMode() claudeFingerprintMode {
	if a == nil || !a.IsClaudeOAuthLike() {
		return claudeFingerprintOff
	}
	return claudeFingerprintModeFromExtra(a.Extra)
}

// --- 确定性派生工具 ---

// deriveStableClaudeDeviceID 从种子确定性派生 64 位十六进制 device_id。
// 与 Claude Code 真实 device_id 格式一致（32 字节随机数的 hex 编码）。
func deriveStableClaudeDeviceID(seed string) string {
	h := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(h[:])
}

// deriveStableClaudeSessionID 从种子确定性派生 session_id。
// 客户端会话为空时派生账号级恒定值（full 模式用），否则按客户端会话派生
// （session 模式用：每个真实客户端会话一个独立 session_id）。
// 复用 deriveStableUUIDv4，与 Codex 侧同源，避免两套 UUIDv4 变形实现漂移。
func deriveStableClaudeSessionID(seed, clientSessionID string) string {
	if seed == "" {
		return ""
	}
	if clientSessionID == "" {
		return deriveStableUUIDv4("sub2api:claude-session-id:v1:" + seed)
	}
	return deriveStableUUIDv4("sub2api:claude-session-id:v1:" + seed + ":" + clientSessionID)
}

func resolveConvergedClaudeDeviceID(account *Account, seed string) string {
	if account == nil {
		return ""
	}
	if deviceID := account.GetClaudeDeviceID(); deviceID != "" {
		return deviceID
	}
	if seed == "" {
		return ""
	}
	return deriveStableClaudeDeviceID("sub2api:claude-device-id:v1:" + seed)
}

// resolveConvergedClaudeAccountUUID 返回账号真实的 Anthropic account_uuid。
// 收敛只能把客户端身份对齐到已知的账号身份，因此账户元数据缺失时返回空，
// 由调用方跳过 account_uuid 改写，而不是凭空派生一个上游从未见过的值
// （这一点与 device_id 不同：device_id 无真实值时可以从种子派生，
// account_uuid 必须始终是账号在 Anthropic 侧的真实身份）。
func resolveConvergedClaudeAccountUUID(account *Account) string {
	if account == nil {
		return ""
	}
	return account.GetClaudeAccountUUID()
}

// --- 核心 ID 集合 ---

type claudeFingerprintIDs struct {
	accountID   int64
	mode        claudeFingerprintMode
	deviceID    string
	accountUUID string
	sessionID   string
}

func resolveClaudeFingerprintIDs(account *Account, clientSessionID string, mode claudeFingerprintMode) *claudeFingerprintIDs {
	if account == nil || mode == claudeFingerprintOff {
		return nil
	}
	seed, ok := claudeFingerprintSeed(account.Extra)
	if !ok {
		return nil
	}

	ids := &claudeFingerprintIDs{
		accountID: account.ID,
		mode:      mode,
	}

	ids.deviceID = resolveConvergedClaudeDeviceID(account, seed)
	if ids.deviceID == "" {
		return nil
	}

	switch mode {
	case claudeFingerprintDevice:
		return ids
	case claudeFingerprintSession:
		ids.accountUUID = resolveConvergedClaudeAccountUUID(account)
		ids.sessionID = deriveStableClaudeSessionID(seed, clientSessionID)
		return ids
	case claudeFingerprintFull:
		ids.accountUUID = resolveConvergedClaudeAccountUUID(account)
		ids.sessionID = deriveStableClaudeSessionID(seed, "")
		return ids
	}
	return nil
}

// --- user_id 解析与改写 ---

// claudeFingerprintUserIDComponents 读出可收敛的客户端身份组件。
//
// 判据用仓库既有的 ParseMetadataUserID，它同时认两种形态：
//   - 新（JSON）格式：要求 device_id 与 session_id 都非空；
//   - 旧拼接格式 user_<dev>_account_<uuid>_session_<uuid>：真实 Claude Code 仍在用
//     （见测试夹具 fixtures/real-cc-body.json，其 metadata.user_id 就是旧格式）。
//
// 只有「读不出的身份」才不收敛（缺失、空串、JSON 但缺字段、任意非规范字符串）：
// 此时既没有可收敛掉的客户端身份，凭空合成一份又会让所有此类客户端塌成同一个
// 账号级会话，与 session/full 模式的语义相反。旧格式**不再**被归入此类——它可读，
// 因此按原格式收敛并原格式回写（见 applyClaudeFingerprintToUserIDJSON）。
func claudeFingerprintUserIDComponents(raw string) (*ParsedUserID, bool) {
	parsed := ParseMetadataUserID(raw)
	if parsed == nil {
		return nil, false
	}
	return parsed, true
}

// resolveClaudeFingerprintIDsFromRawBody 在原始 JSON 字节上解析收敛 ID。
// Claude 的转发热路径全程只持有原始 body（body 可能达数十 MB，禁止全量
// Unmarshal），因此解析与改写都只走 gjson/sjson（与
// applyClaudeFingerprintClientMetadataRaw 成对）。
func resolveClaudeFingerprintIDsFromRawBody(account *Account, body []byte) *claudeFingerprintIDs {
	if len(body) == 0 {
		return nil
	}
	userID := gjson.GetBytes(body, "metadata.user_id")
	if !userID.Exists() || userID.Type != gjson.String {
		return nil
	}
	return resolveClaudeFingerprintIDsFromUserID(account, userID.String())
}

// resolveClaudeFingerprintIDsFromUserID 是解析收敛 ID 的核心。
//
// 客户端身份不可读（缺失、旧拼接格式、或新格式但缺字段）时返回 nil：此时既没有
// 可收敛掉的客户端身份，凭空合成一份账号级身份又会让所有此类客户端退化成同一个
// 会话，与 session/full 模式的语义相反（见 claudeFingerprintUserIDComponents）。
func resolveClaudeFingerprintIDsFromUserID(account *Account, userIDRaw string) *claudeFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetClaudeFingerprintMode()
	if mode == claudeFingerprintOff {
		return nil
	}
	parsed, ok := claudeFingerprintUserIDComponents(userIDRaw)
	if !ok {
		return nil
	}
	return resolveClaudeFingerprintIDs(account, parsed.SessionID, mode)
}

// stageClaudeFingerprintForBody 是请求体侧的唯一收敛入口：解析本 attempt 的
// 收敛 ID、用同一份 IDs 改写请求体，并把 IDs 暂存到 gin context 供出站头改写
// （applyStagedClaudeFingerprintHeaders）读取——头与体必须共享同一份 IDs，
// 否则上游可由头值探测出真实会话。
//
// 必须无条件覆写暂存值（含 nil）：failover 从收敛账号切到 off 账号时，
// 上一账号的 IDs 不得残留并被误应用到新账号（见 stageClaudeFingerprintIDs）。
// 体改写失败时一并作废暂存值，宁可本次不收敛，也不让头与体不一致。
//
// c 为 nil 时整体跳过：没有暂存位置就改不出配对的出站头，只改体反而制造
// "体收敛、头残留客户端原值"的分裂身份。调用方都是网关 handler，c 恒非 nil。
//
// **每个 attempt 只对客户端原始 body 调用一次**：session 模式的 session_id 由
// 客户端会话派生，而收敛会把 body 里的 session_id 一并改写，因此对已收敛的 body
// 再调用一次得到的是第二级派生值 derive(seed, derive(seed, clientSession))，
// 不是不动点。生产路径满足该前提——handler 的 failover 循环每次 attempt 都用
// parsedReq.CloneForBody 从客户端原始 body 重新克隆（gateway_handler.go），
// 收敛只改到该 attempt 自己的副本。重试循环内部不要再插一次本调用。
func stageClaudeFingerprintForBody(c *gin.Context, account *Account, body []byte) []byte {
	if c == nil {
		return body
	}
	stageClaudeFingerprintIDs(c, nil)
	ids := resolveClaudeFingerprintIDsFromRawBody(account, body)
	if ids == nil {
		return body
	}
	next, changed, err := applyClaudeFingerprintClientMetadataRaw(body, ids)
	if err != nil {
		logger.LegacyPrintf("service.gateway", "Warning: failed to apply Claude fingerprint convergence for account %d: %v", account.ID, err)
		return body
	}
	stageClaudeFingerprintIDs(c, ids)
	if !changed {
		return body
	}
	return next
}

// applyClaudeFingerprintToUserIDJSON 是唯一改写核心：按组件改写并回写
// metadata["user_id"]。它作用在一个已解码的小对象上，调用方负责取用与回写：
// 转发热路径（applyClaudeFingerprintClientMetadataRaw）用 gjson 只取出这一小块、
// 改完再 sjson 拼回，全程不碰 body 其余字节。收敛语义因此只有这一份实现。
//
// 回写格式跟随客户端原始形态（ParsedUserID.IsNewFormat）：旧拼接格式仍原样是旧拼接
// 格式，只有 device_id 等组件被替换。这一点很重要——真实 Claude Code 至今仍在发旧
// 拼接格式，若一律转成 JSON，body 形态就会与该客户端的 UA 版本自相矛盾；而若为图省事
// 跳过旧格式，功能对真实流量就等于不生效（设备与会话原样泄露给上游）。
func applyClaudeFingerprintToUserIDJSON(metadata map[string]any, ids *claudeFingerprintIDs) bool {
	if metadata == nil || ids == nil {
		return false
	}

	userIDRaw, _ := metadata["user_id"].(string)
	parsed, ok := claudeFingerprintUserIDComponents(userIDRaw)
	if !ok {
		// 读不出的身份一律原样透传，不重建：重建会丢组件或把客户端塌成账号级会话
		// （见 claudeFingerprintUserIDComponents 的说明）。
		return false
	}

	deviceID, accountUUID, sessionID := parsed.DeviceID, parsed.AccountUUID, parsed.SessionID

	modified := false

	if ids.deviceID != "" && deviceID != ids.deviceID {
		deviceID = ids.deviceID
		modified = true
	}

	switch ids.mode {
	case claudeFingerprintDevice:
		// 仅收敛 device_id，account_uuid / session_id 保留客户端原值
	case claudeFingerprintSession, claudeFingerprintFull:
		if ids.accountUUID != "" && accountUUID != ids.accountUUID {
			accountUUID = ids.accountUUID
			modified = true
		}
		if ids.sessionID != "" && sessionID != ids.sessionID {
			sessionID = ids.sessionID
			modified = true
		}
	}

	if !modified {
		return false
	}

	candidate := FormatMetadataUserIDInFormat(deviceID, accountUUID, sessionID, parsed.IsNewFormat)

	// 自校验：旧拼接格式对组件有字面形态要求（device 必须 64 位十六进制、account 只能
	// 十六进制与连字符、session 必须 36 位）。account_uuid 来自账号 extra，管理员可改，
	// device_id 亦可由 extra 覆写；写进去若让结果无法解析，下游读 metadata.user_id 的
	// 头同步与审计会静默失效。此时宁可本次不收敛，也不要把身份写成自己都读不出的样子。
	if ParseMetadataUserID(candidate) == nil {
		return false
	}

	metadata["user_id"] = candidate
	return true
}

// applyClaudeFingerprintClientMetadataRaw 透传热路径：gjson 提取 + sjson 拼回，
// 不对整个 body 做全量 Unmarshal。
func applyClaudeFingerprintClientMetadataRaw(body []byte, ids *claudeFingerprintIDs) ([]byte, bool, error) {
	if len(body) == 0 || ids == nil {
		return body, false, nil
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body, false, nil
	}

	// 客户端完全没带 metadata.user_id 时不作为：收敛是「改写」而不是「铸造」身份。
	// 此时既没有客户端身份需要收敛，凭空合成的那份又未必符合真实形态
	// （没有 account_uuid 时只有 device_id + session_id），比不改更糟。
	// 身份铸造由 ensureClaudeOAuthMetadataUserID 负责，那里保证形态正确。
	// 与 resolveClaudeFingerprintIDsFromUserID 的同一门控保持对称。
	userIDResult := gjson.GetBytes(body, "metadata.user_id")
	if !userIDResult.Exists() {
		return body, false, nil
	}

	metadataExisting := map[string]any{}
	if userIDResult.Type == gjson.String {
		metadataExisting["user_id"] = userIDResult.String()
	}

	if !applyClaudeFingerprintToUserIDJSON(metadataExisting, ids) {
		return body, false, nil
	}

	userIDRaw, _ := metadataExisting["user_id"].(string)
	next, setErr := sjson.SetBytes(body, "metadata.user_id", userIDRaw)
	if setErr != nil {
		return body, false, fmt.Errorf("splice converged user_id: %w", setErr)
	}
	return next, true, nil
}

// --- 请求头处理 ---
// Claude Code 的 session_id 同时出现在 metadata.user_id 和
// x-claude-code-session-id 头中，且两者始终一致。
// 收敛时两者都需要改写，否则上游可通过头值探测真实会话。

func applyClaudeFingerprintHeaders(h http.Header, ids *claudeFingerprintIDs) {
	if h == nil || ids == nil {
		return
	}
	if ids.sessionID != "" {
		h.Set("x-claude-code-session-id", ids.sessionID)
		h.Set("x-session-id", ids.sessionID)
	}
}

func applyStagedClaudeFingerprintHeaders(c *gin.Context, account *Account, h http.Header) {
	applyClaudeFingerprintHeaders(h, stagedClaudeFingerprintIDs(c, account))
}

// applyStagedClaudeFingerprintClientMetadataRaw 用 context 中已暂存的收敛 ID 重新收敛
// 原始字节 body，是头改写的配对操作。
//
// 需要它是因为 metadata.user_id 在下游还有别的写者：buildUpstreamRequest /
// buildCountTokensRequest 里的 RewriteUserIDWithMasking 会把 device_id 换成
// fp.ClientID、session_id 重新哈希（账号有 account_uuid 时默认触发），正好盖掉收敛结果，
// 于是出站头指向收敛会话、wire body 指向另一个会话——收敛模块自己禁止的分裂身份。
// Codex 侧的结构是「身份 scope 在前、指纹收敛最后」，这里用同一份暂存 IDs 再写一次，
// 把顺序补齐。
//
// 只应用已解析的 IDs、不重新解析，因此不受 stageClaudeFingerprintForBody
// 「每 attempt 只解析一次」的约束：重复应用同一份 IDs 是对合的不动点。
func applyStagedClaudeFingerprintClientMetadataRaw(c *gin.Context, account *Account, body []byte) []byte {
	ids := stagedClaudeFingerprintIDs(c, account)
	if ids == nil {
		return body
	}
	next, changed, err := applyClaudeFingerprintClientMetadataRaw(body, ids)
	if err != nil {
		logger.LegacyPrintf("service.gateway", "Warning: failed to re-apply Claude fingerprint convergence for account %d: %v", account.ID, err)
		return body
	}
	if !changed {
		return body
	}
	return next
}
func (a *Account) UsesClaudeProtocol() bool {
	if a == nil {
		return false
	}
	return a.Platform == PlatformAnthropic
}

func (a *Account) IsClaudeOAuthLike() bool {
	if a == nil || a.Platform != PlatformAnthropic {
		return false
	}
	return a.Type == AccountTypeOAuth || a.Type == AccountTypeSetupToken
}

func (a *Account) GetClaudeDeviceID() string {
	if a == nil || a.Extra == nil {
		return ""
	}
	v, _ := a.Extra["claude_device_id"].(string)
	return strings.TrimSpace(v)
}

// GetClaudeAccountUUID 返回账号真实的 Anthropic account_uuid（OAuth 元数据，
// 与 buildOAuthMetadataUserID / count_tokens 走同一个 extra 键）。
// 缺失时返回空，此时收敛不会改写 account_uuid。
func (a *Account) GetClaudeAccountUUID() string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.GetExtraString("account_uuid"))
}
