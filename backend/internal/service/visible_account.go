package service

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 普通用户「已分配账号只读查看」（票据 05）。
//
// 该能力叠加在既有 user 身份之上：管理员逐用户开启开关并逐条分配账号，
// 默认关闭且默认零个。它只授予只读查看，且只返回字段白名单里的基础资料与
// 服务端掩码后的上游身份；不复用任何管理员宽度 DTO、账号导出、代理接口或
// 会主动探测上游的读端点，也不参与调度与分组路由。

const (
	// accountViewReasonDisabled 表示该普通用户的账号查看能力未开启（HTTP 403）。
	accountViewReasonDisabled = "ACCOUNT_VIEW_DISABLED"
	// accountViewReasonNotFound 表示账号不在该用户的可见范围内（HTTP 404）。
	// 未分配、已软删除、已手动禁用三种情况共用该结果，避免通过猜测 ID 探知
	// 未授权账号是否存在。
	accountViewReasonNotFound = "ACCOUNT_NOT_FOUND"
)

// ErrAccountViewDisabled 表示当前用户没有开启账号查看能力。
var ErrAccountViewDisabled = infraerrors.New(403, accountViewReasonDisabled,
	"Account view is not enabled for this user")

// ErrVisibleAccountNotFound 表示账号对当前用户不可见。
var ErrVisibleAccountNotFound = infraerrors.New(404, accountViewReasonNotFound,
	"Account not found")

// ErrUnknownVisibleAccount 表示管理员分配时引用了不存在的账号。
var ErrUnknownVisibleAccount = infraerrors.New(400, "UNKNOWN_ACCOUNT",
	"One or more account ids do not exist")

// ErrAccountViewUserNotFound 表示管理员操作的目标用户不存在（或已删除）。
var ErrAccountViewUserNotFound = infraerrors.New(404, "USER_NOT_FOUND",
	"User not found")

// AssignedVisibleAccount 是管理员视角下的已分配账号摘要。
// 仅供管理员接口使用（含真实名称与状态），普通用户响应绝不包含这些字段。
// 它是管理员界面 account_ids 里「有详情」的那部分：已被软删除的分配只有 id。
type AssignedVisibleAccount struct {
	ID       int64
	Name     string
	Platform string
	Type     string
	Status   string
}

// VisibleAccountRepository 是账号查看能力的数据访问接口。
//
// 所有面向普通用户的读取都必须在 SQL 层同时约束：用户存在且未被禁用、
// 能力开关已开启、存在显式分配关系、账号未软删除且未被手动禁用。
// 授权撤销、用户禁用与账号禁用都必须在下一个请求即生效，不依赖缓存。
type VisibleAccountRepository interface {
	// GetAccountViewEnabled 读取用户的能力开关；用户不存在、已软删除或已禁用时
	// 返回 false（与列表／详情的门禁同口径）。
	GetAccountViewEnabled(ctx context.Context, userID int64) (bool, error)
	// GetStoredAccountViewEnabled 读取用户「存储」的能力开关（管理员视角）：
	// 已禁用但未软删除的用户也返回真实存储值，避免管理员界面把 true 显示成 false
	// 并在原样保存时误关闭能力。用户不存在或已软删除时返回 ErrAccountViewUserNotFound。
	GetStoredAccountViewEnabled(ctx context.Context, userID int64) (bool, error)
	// UpdateAccountView 在单个事务内更新能力开关与分配集合。
	// enabled / accountIDs 为 nil 表示该项保持不变，accountIDs 为空切片表示清空。
	// 「新分配」的 account id 不存在（含软删除）时返回 ErrUnknownVisibleAccount，
	// 且开关与分配都不落库（整体回滚）；已在分配关系里的 id 允许保留（即使账号
	// 已被软删除），保证管理员界面原样保存不会撤销既有分配。
	// 用户不存在或已软删除时返回 ErrAccountViewUserNotFound。
	// 同一用户的并发替换必须串行化，且保留的分配行不得改写 granted_by/created_at。
	UpdateAccountView(ctx context.Context, userID int64, enabled *bool, accountIDs *[]int64, grantedBy *int64) error
	// ListAssignedAccountIDs 返回该用户全部已存储的分配 id（升序，含对应账号已
	// 软删除的分配）：管理员界面的 account_ids 是权威集合，GET 与 PUT 都按整份
	// 集合替换，缺 id 等于在原样保存时静默撤销关系。
	ListAssignedAccountIDs(ctx context.Context, userID int64) ([]int64, error)
	// ListAssignedAccountSummaries 返回该用户当前全部分配的账号详情（含手动禁用），
	// 供管理员界面撤销用；不要求能力开关开启。已被软删除的账号没有详情，
	// 它只出现在 ListAssignedAccountIDs 里。
	ListAssignedAccountSummaries(ctx context.Context, userID int64) ([]AssignedVisibleAccount, error)
	// ListVisibleAccounts 返回当前可见账号（分页）。search/platform 过滤只作用于
	// 已分配且未禁用的集合内部，不会扩大或探测范围。
	ListVisibleAccounts(ctx context.Context, userID int64, filter VisibleAccountFilter) ([]*Account, int64, error)
	// GetVisibleAccount 返回单个可见账号；不可见时返回 (nil, nil)。
	GetVisibleAccount(ctx context.Context, userID, accountID int64) (*Account, error)
}

// VisibleAccountFilter 是普通用户列表查询条件。
// Search 只匹配对用户已公开的字段（平台、类型与数字账号 ID），
// 不会匹配管理员专有的账号名称，避免形成额外的信息探知通道。
type VisibleAccountFilter struct {
	Platform string
	Search   string
	Page     int
	PageSize int
}

// VisibleAccountView 是普通用户看到的账号安全视图（纯白名单）。
//
// 不含名称、备注、凭据、extra、代理、余额、错误消息、运行状态或完整上游身份。
type VisibleAccountView struct {
	ID                      int64
	Platform                string
	AccountType             string
	EmailMasked             string
	UsernameMasked          string
	UpstreamAccountIDMasked string
}

// VisibleAccountPage 是普通用户列表结果。
type VisibleAccountPage struct {
	Items    []VisibleAccountView
	Total    int64
	Page     int
	PageSize int
}

// AdminAccountView 是管理员分配界面所需的完整视图。
//
// AccountIDs 是权威集合：包含该用户全部已存储的分配，含对应账号已被软删除、
// 因而没有出现在 Accounts 里的分配。界面对缺失详情的 id 保留占位条目，
// 保存时原样回传整份集合。
type AdminAccountView struct {
	UserID     int64
	Enabled    bool
	AccountIDs []int64
	Accounts   []AssignedVisibleAccount
}

// VisibleAccountService 实现账号查看的鉴权、范围与脱敏规则。
type VisibleAccountService struct {
	repo VisibleAccountRepository
}

// NewVisibleAccountService 构造账号查看服务。
func NewVisibleAccountService(repo VisibleAccountRepository) *VisibleAccountService {
	return &VisibleAccountService{repo: repo}
}

// List 返回当前用户的可见账号分页。
// 能力未开启时返回 ErrAccountViewDisabled。
func (s *VisibleAccountService) List(ctx context.Context, userID int64, filter VisibleAccountFilter) (VisibleAccountPage, error) {
	if s == nil || s.repo == nil {
		return VisibleAccountPage{}, ErrAccountViewDisabled
	}
	enabled, err := s.repo.GetAccountViewEnabled(ctx, userID)
	if err != nil {
		return VisibleAccountPage{}, err
	}
	if !enabled {
		return VisibleAccountPage{}, ErrAccountViewDisabled
	}

	filter = normalizeVisibleAccountFilter(filter)
	accounts, total, err := s.repo.ListVisibleAccounts(ctx, userID, filter)
	if err != nil {
		return VisibleAccountPage{}, err
	}

	items := make([]VisibleAccountView, 0, len(accounts))
	for _, account := range accounts {
		if account == nil || accountManuallyDisabled(account.Status) {
			continue
		}
		items = append(items, BuildVisibleAccountView(account))
	}
	return VisibleAccountPage{
		Items:    items,
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}, nil
}

// Get 返回单个可见账号。
// 能力未开启返回 ErrAccountViewDisabled；未分配、已删除或已手动禁用返回
// ErrVisibleAccountNotFound，三者对外不可区分。
func (s *VisibleAccountService) Get(ctx context.Context, userID, accountID int64) (VisibleAccountView, error) {
	if s == nil || s.repo == nil {
		return VisibleAccountView{}, ErrAccountViewDisabled
	}
	enabled, err := s.repo.GetAccountViewEnabled(ctx, userID)
	if err != nil {
		return VisibleAccountView{}, err
	}
	if !enabled {
		return VisibleAccountView{}, ErrAccountViewDisabled
	}
	account, err := s.repo.GetVisibleAccount(ctx, userID, accountID)
	if err != nil {
		return VisibleAccountView{}, err
	}
	if account == nil || accountManuallyDisabled(account.Status) {
		return VisibleAccountView{}, ErrVisibleAccountNotFound
	}
	return BuildVisibleAccountView(account), nil
}

// AdminView 返回管理员视角的分配状态（含手动禁用账号，便于撤销）。
//
// 与面向普通用户的读取有两处刻意的差别：
//   - 开关读「存储值」而不是门禁值：已禁用用户也如实显示，界面原样保存时不会把
//     存储的 true 写成 false；
//   - account_ids 是完整权威集合，包含对应账号已软删除、因而没有详情的分配，
//     否则界面原样保存会静默撤销那条关系。
//
// 用户不存在或已软删除时返回 ErrAccountViewUserNotFound（与 AdminUpdate 同口径）。
func (s *VisibleAccountService) AdminView(ctx context.Context, userID int64) (AdminAccountView, error) {
	if s == nil || s.repo == nil {
		return AdminAccountView{}, ErrVisibleAccountNotFound
	}
	enabled, err := s.repo.GetStoredAccountViewEnabled(ctx, userID)
	if err != nil {
		return AdminAccountView{}, err
	}
	accountIDs, err := s.repo.ListAssignedAccountIDs(ctx, userID)
	if err != nil {
		return AdminAccountView{}, err
	}
	assigned, err := s.repo.ListAssignedAccountSummaries(ctx, userID)
	if err != nil {
		return AdminAccountView{}, err
	}
	return adminAccountView(userID, enabled, accountIDs, assigned), nil
}

// AdminUpdate 原子地更新能力开关与分配集合。
// enabled / accountIDs 为 nil 表示该项保持不变；accountIDs 为空切片表示清空。
func (s *VisibleAccountService) AdminUpdate(
	ctx context.Context,
	userID int64,
	enabled *bool,
	accountIDs *[]int64,
	grantedBy *int64,
) (AdminAccountView, error) {
	if s == nil || s.repo == nil {
		return AdminAccountView{}, ErrVisibleAccountNotFound
	}
	normalizedIDs := accountIDs
	if accountIDs != nil {
		deduped := dedupeAccountIDs(*accountIDs)
		normalizedIDs = &deduped
	}
	// 开关与分配必须在同一事务内生效：未知账号 id 被拒绝时不能留下
	// 「开关已打开但分配未落库」的中间状态。
	if err := s.repo.UpdateAccountView(ctx, userID, enabled, normalizedIDs, grantedBy); err != nil {
		return AdminAccountView{}, err
	}
	return s.AdminView(ctx, userID)
}

// adminAccountView 组装管理员视图。
//
// accountIDs 是权威集合（含没有详情的分配），accounts 是其中可读账号的详情，
// 两者长度可以不同：界面按 id 保留占位条目，保存时原样回传整份集合。
func adminAccountView(
	userID int64,
	enabled bool,
	accountIDs []int64,
	assigned []AssignedVisibleAccount,
) AdminAccountView {
	ids := make([]int64, 0, len(accountIDs))
	ids = append(ids, accountIDs...)
	accounts := make([]AssignedVisibleAccount, 0, len(assigned))
	accounts = append(accounts, assigned...)
	return AdminAccountView{
		UserID:     userID,
		Enabled:    enabled,
		AccountIDs: ids,
		Accounts:   accounts,
	}
}

// accountManuallyDisabled 判断账号是否被管理员手动停用。
//
// 只有手动停用（历史 disabled 与编辑器 inactive）才算「禁用」；
// error / expired 属故障或过期，临时不可调度、限流、过载等调度态也一律保持可见，
// 不能复用会过滤限流账号的调度查询。
func accountManuallyDisabled(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusDisabled, StatusInactive:
		return true
	default:
		return false
	}
}

func normalizeVisibleAccountFilter(filter VisibleAccountFilter) VisibleAccountFilter {
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize < 1 {
		filter.PageSize = defaultVisibleAccountPageSize
	}
	if filter.PageSize > maxVisibleAccountPageSize {
		filter.PageSize = maxVisibleAccountPageSize
	}
	filter.Platform = strings.ToLower(strings.TrimSpace(filter.Platform))
	filter.Search = strings.TrimSpace(filter.Search)
	return filter
}

const (
	defaultVisibleAccountPageSize = 20
	maxVisibleAccountPageSize     = 100
)

// dedupeAccountIDs 去掉重复 id，但保留非法（<=0）取值交由仓储拒绝：
// 静默丢弃会让 {"account_ids":[0]} 变成「清空分配」，等于擅自改写管理员意图。
func dedupeAccountIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return []int64{}
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// BuildVisibleAccountView 把账号投影为纯白名单视图。
//
// 名称、备注、凭据、extra、代理与运行状态一律不进入视图；上游身份只以
// 服务端掩码结果出现。账号名称不参与任何回退：本视图不包含名称字段。
func BuildVisibleAccountView(account *Account) VisibleAccountView {
	if account == nil {
		return VisibleAccountView{}
	}
	email, username, upstreamAccountID := visibleAccountIdentity(account)
	return VisibleAccountView{
		ID:                      account.ID,
		Platform:                account.Platform,
		AccountType:             account.Type,
		EmailMasked:             maskVisibleAccountEmail(email),
		UsernameMasked:          maskVisibleAccountIdentity(username),
		UpstreamAccountIDMasked: maskVisibleAccountIdentity(upstreamAccountID),
	}
}

// visibleAccountIdentity 从账号凭据与扩展数据中只读取被允许展示的上游身份字段。
// 不读取 URL、错误消息、备注或任何凭据键。
func visibleAccountIdentity(account *Account) (email, username, upstreamAccountID string) {
	if account == nil {
		return "", "", ""
	}
	email = firstStringValue(account.Credentials, "email")
	if email == "" {
		email = firstStringValue(account.Extra, "email", "email_address")
	}
	username = firstStringValue(account.Credentials, "username")
	if username == "" {
		username = firstStringValue(account.Extra, "username")
	}
	upstreamAccountID = firstStringValue(account.Credentials,
		"account_id", "chatgpt_account_id", "chatgpt_user_id")
	if upstreamAccountID == "" {
		upstreamAccountID = firstStringValue(account.Extra,
			"account_id", "chatgpt_account_id", "chatgpt_user_id")
	}
	return email, username, upstreamAccountID
}

// visibleAccountMaskPlaceholder 是掩码后的固定占位符。
const visibleAccountMaskPlaceholder = "***"

// visibleAccountMaskShortLen 以下的标识整体替换为占位符。
//
// 阈值取 6：值太短时任何「保留首尾」的掩码都会把原文几乎全部暴露
// （例如 4 位值保留首尾 2+2 等于原样输出），因此直接不显示。
const visibleAccountMaskShortLen = 6

// maskVisibleAccountIdentity 掩码用户名／上游账号 ID 等标识。
// 短值整体替换为占位符，不原样输出；较长值只保留首尾各两个字符。
func maskVisibleAccountIdentity(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	runes := []rune(trimmed)
	if len(runes) <= visibleAccountMaskShortLen {
		return visibleAccountMaskPlaceholder
	}
	return string(runes[:2]) + visibleAccountMaskPlaceholder + string(runes[len(runes)-2:])
}

// maskVisibleAccountEmail 掩码邮箱。首字符以外的本地部分与域名主体一律隐藏；
// 任一组成部分过短时该部分整体替换为占位符，短邮箱不会原样暴露。
func maskVisibleAccountEmail(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	at := strings.LastIndex(trimmed, "@")
	if at <= 0 || at == len(trimmed)-1 {
		// 不是可解析邮箱：退化为同级别的标识掩码。
		return maskVisibleAccountIdentity(trimmed)
	}
	local := []rune(trimmed[:at])
	domain := trimmed[at+1:]
	domainLabel := domain
	if dot := strings.Index(domain, "."); dot > 0 {
		domainLabel = domain[:dot]
	}

	maskedLocal := visibleAccountMaskPlaceholder
	if len(local) > 3 {
		maskedLocal = string(local[:1]) + visibleAccountMaskPlaceholder
	}
	maskedDomain := visibleAccountMaskPlaceholder
	if labelRunes := []rune(domainLabel); len(labelRunes) > 3 {
		maskedDomain = string(labelRunes[:1]) + visibleAccountMaskPlaceholder
	}
	return maskedLocal + "@" + maskedDomain
}
