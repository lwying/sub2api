package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
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
	// 未分配、已软删除、用户失去资格三种情况共用该结果，避免通过猜测 ID 探知
	// 未授权账号是否存在。手动停用不再属于此列：已授权即保持可见。
	accountViewReasonNotFound = "ACCOUNT_NOT_FOUND"
)

// ErrAccountViewDisabled 表示当前用户没有开启账号查看能力。
var ErrAccountViewDisabled = infraerrors.New(403, accountViewReasonDisabled,
	"Account view is not enabled for this user")

// ErrVisibleAccountNotFound 表示账号对当前用户不可见。
var ErrVisibleAccountNotFound = infraerrors.New(404, accountViewReasonNotFound,
	"Account not found")

// ErrUnknownVisibleAccount 表示管理员分配时引用了不存在的账号。
// 失败响应里还会带上被拒的 id（见 UnknownVisibleAccountError），
// 使管理员界面能指出具体失败项。
var ErrUnknownVisibleAccount = infraerrors.New(400, "UNKNOWN_ACCOUNT",
	"One or more account ids do not exist")

// ErrAccountViewUserNotFound 表示管理员操作的目标用户不存在（或已删除）。
var ErrAccountViewUserNotFound = infraerrors.New(404, "USER_NOT_FOUND",
	"User not found")

// ErrVisibleAccountBatchTooLarge 表示运行时批量读取请求的账号 id 数量超过上限（HTTP 400）。
// 上限与列表分页上限一致：批量只为「当前一页」取运行时数据，超出一页的请求是调用方错误。
var ErrVisibleAccountBatchTooLarge = infraerrors.New(400, "TOO_MANY_ACCOUNT_IDS",
	"Too many account ids")

// maxVisibleAccountBatchIDs 是运行时批量读取允许的账号 id 上限（与列表分页上限一致）。
const maxVisibleAccountBatchIDs = maxVisibleAccountPageSize

// 分配失败的「明确失败项」契约：失败本身仍是 400 UNKNOWN_ACCOUNT，
// 具体的不可用账号 id 附加在错误元数据里。
const (
	// visibleAccountInvalidIDsMetadataKey 是失败响应里被拒账号 id 的元数据键。
	// 只承载有界的十进制 id，不含账号名称、凭据或任何账号内部配置。
	visibleAccountInvalidIDsMetadataKey = "invalid_account_ids"

	// visibleAccountInvalidCountMetadataKey 是本次被拒账号的总数。id 列表因上限被
	// 截断时，界面仍能说明实际有多少项失败，而不是把截断后的数量当成全部。
	visibleAccountInvalidCountMetadataKey = "invalid_account_count"

	// maxReportedInvalidVisibleAccountIDs 限制错误响应里列出的 id 数量：
	// 「一键添加」可能一次提交上百个账号，错误载荷必须保持有界。
	maxReportedInvalidVisibleAccountIDs = 50
)

// UnknownVisibleAccountError 构造「分配引用了不可用账号」的失败错误。
//
// 对外仍是 400 UNKNOWN_ACCOUNT，错误链上仍匹配 ErrUnknownVisibleAccount
// （ApplicationError.Is 只比较 Code 与 Reason），因此既有调用方与服务端/客户端
// 契约不变；新增的是 metadata 里被拒账号的 id：管理员在一次批量保存被整批
// 回滚后，据此就能知道该移除或替换哪一项，并在保留草稿的前提下重试。
//
// 只暴露数字 id：不含账号名称、状态、凭据与内部配置。id 去重升序，超过
// maxReportedInvalidVisibleAccountIDs 时只列前若干个，总数另由
// invalid_account_count 给出。返回值为哨兵的副本，不改动包级哨兵本身。
func UnknownVisibleAccountError(ids []int64) *infraerrors.ApplicationError {
	unique := uniqueSortedAccountIDs(ids)
	metadata := map[string]string{
		visibleAccountInvalidCountMetadataKey: strconv.Itoa(len(unique)),
	}
	if len(unique) > 0 {
		reported := unique
		if len(reported) > maxReportedInvalidVisibleAccountIDs {
			reported = reported[:maxReportedInvalidVisibleAccountIDs]
		}
		metadata[visibleAccountInvalidIDsMetadataKey] = formatAccountIDList(reported)
	}
	return ErrUnknownVisibleAccount.WithMetadata(metadata)
}

// uniqueSortedAccountIDs 去重并升序排序，使同一批失败项在任何调用点都得到稳定输出。
func uniqueSortedAccountIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
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
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// formatAccountIDList 用逗号连接 id：错误元数据是 map[string]string，
// 逗号分隔是既有错误元数据表达列表的方式。
func formatAccountIDList(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

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
// 能力开关已开启、存在显式分配关系、账号未软删除。
// 手动停用账号保持可见（按状态展示）；授权撤销、用户禁用与账号软删除
// 必须在下一个请求即生效，不依赖缓存。
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
	// 该错误是 UnknownVisibleAccountError 构造的副本：除错误码外还带有本次被拒的
	// 具体 id（有界），使管理员界面能指出失败项并保留草稿修正。
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

// VisibleAccountScopeRepository 是账号查看仓储的可选扩展能力（窄接口）。
//
// 服务层按类型断言使用：未实现的仓储（例如只覆盖基础读路径的测试替身）会退化为
// 「分组目录为空、批量交集为空」，不会 panic，也不会放宽任何可见性约束。
// 真实 PostgreSQL 仓储必须实现它，以保证批量与分组目录都走一次范围查询而不是逐行读取。
type VisibleAccountScopeRepository interface {
	// ListVisibleAccountGroups 返回该用户可见账号实际所属的分组去重集合。
	// 只从已授权账号的 account_groups 派生，不读取全局分组目录或用户 API Key 可用分组。
	ListVisibleAccountGroups(ctx context.Context, userID int64) ([]VisibleAccountGroup, error)
	// ListVisibleAccountsByIDs 返回授权集合内命中给定 id 的可见账号（一次范围查询）。
	// 未授权、已删除或用户失去资格的 id 一律不出现在结果里，调用方据此只回传授权 id。
	ListVisibleAccountsByIDs(ctx context.Context, userID int64, accountIDs []int64) ([]*Account, error)
}

// VisibleAccountUsageReader 是账号查看可复用的只读用量读取器（AccountUsageService 的窄切片）。
//
// 全部方法只读数据库／被动快照，不触发上游探测、Token 刷新或任何写操作。
type VisibleAccountUsageReader interface {
	GetTodayStats(ctx context.Context, accountID int64) (*WindowStats, error)
	GetTodayStatsBatch(ctx context.Context, accountIDs []int64) (map[int64]*WindowStats, error)
	GetAccountUsageStats(ctx context.Context, accountID int64, startTime, endTime time.Time) (*usagestats.AccountUsageStatsResponse, error)
	GetPassiveUsage(ctx context.Context, accountID int64) (*UsageInfo, error)
}

// VisibleAccountConcurrencyReader 是并发读取的窄切片（ConcurrencyService）。
type VisibleAccountConcurrencyReader interface {
	GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error)
}

// VisibleAccountPassiveUsageBatchReader 是可选的批量被动用量读取器。
//
// 实现方（*AccountUsageService）复用调用方已加载的账号对象批量构建被动快照，
// 不逐账号回表、不外呼、不写入。未实现时 RuntimeBatch 退回逐账号被动读取，
// 行为一致但效率较低；真实 wire 注入的读取器实现了本接口。
type VisibleAccountPassiveUsageBatchReader interface {
	GetPassiveUsageBatch(ctx context.Context, accounts []*Account) (map[int64]*UsageInfo, error)
}

// VisibleAccountFilter 是普通用户列表查询条件。
// Search 匹配对用户已公开的字段（数字账号 ID、名称、平台、类型），
// 状态与分组过滤也只作用于已授权集合内部。
type VisibleAccountFilter struct {
	Platform    string
	AccountType string
	Status      string
	GroupID     int64
	Search      string
	Page        int
	PageSize    int
}

// VisibleAccountGroup 是账号所属分组的只读安全摘要。
//
// 只包含展示所需字段，不含倍率、上下限、状态或任何分组内部配置。
type VisibleAccountGroup struct {
	ID               int64
	Name             string
	Platform         string
	SubscriptionType string
}

// VisibleAccountView 是普通用户看到的账号安全视图（纯白名单）。
//
// 包含名称、平台、类型、状态、分组摘要、容量与安全额度字段；
// 不含备注、凭据、extra、代理、余额、原始错误消息或完整上游身份。
// 运行期字段（usage / today_stats / current_concurrency）不在列表视图里，
// 由 RuntimeBatch 按「当前一页」批量读取，避免逐行请求。
type VisibleAccountView struct {
	ID                      int64
	Name                    string
	Platform                string
	AccountType             string
	Status                  string
	Schedulable             bool
	Concurrency             int
	Groups                  []VisibleAccountGroup
	RateLimitResetAt        *time.Time
	OverloadUntil           *time.Time
	TempUnschedulableUntil  *time.Time
	WindowCostLimit         *float64
	WindowCostStickyReserve *float64
	MaxSessions             *int
	SessionIdleTimeoutMin   *int
	BaseRPM                 *int
	RPMStrategy             *string
	QuotaLimit              *float64
	QuotaUsed               *float64
	QuotaDailyLimit         *float64
	QuotaDailyUsed          *float64
	QuotaWeeklyLimit        *float64
	QuotaWeeklyUsed         *float64
	EmailMasked             string
	UsernameMasked          string
	UpstreamAccountIDMasked string
}

// VisibleAccountRuntime 是单个账号的运行期只读快照。
//
// Usage 只在 Anthropic OAuth/Setup-Token 账号上由被动采样（只读 Extra 与数据库）构建；
// 其它平台保持 nil（未知），不发起任何上游探测或刷新。
// TodayStats / CurrentConcurrency 为 nil 表示读取器未接线或读取失败，均不补零冒充已知。
type VisibleAccountRuntime struct {
	Usage              *UsageInfo
	TodayStats         *WindowStats
	CurrentConcurrency *int
	Concurrency        int
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
//
// usageReader / concurrencyReader 是可选依赖：未接线时列表仍可用，运行期字段
// （usage / today_stats / current_concurrency）按 nil（未知）返回，不补零冒充已知。
type VisibleAccountService struct {
	repo              VisibleAccountRepository
	usageReader       VisibleAccountUsageReader
	concurrencyReader VisibleAccountConcurrencyReader
}

// NewVisibleAccountService 构造账号查看服务（保持旧构造签名，运行期读取器随后注入）。
func NewVisibleAccountService(repo VisibleAccountRepository) *VisibleAccountService {
	return &VisibleAccountService{repo: repo}
}

// SetUsageReader 注入只读用量读取器（可选；在 wire 图中 usage 服务构造完成后调用）。
func (s *VisibleAccountService) SetUsageReader(reader VisibleAccountUsageReader) {
	if s == nil {
		return
	}
	s.usageReader = reader
}

// SetConcurrencyReader 注入并发读取器（可选；由 wire 的 ProvideVisibleAccountService 注入）。
func (s *VisibleAccountService) SetConcurrencyReader(reader VisibleAccountConcurrencyReader) {
	if s == nil {
		return
	}
	s.concurrencyReader = reader
}

// scopeRepo 返回实现了分组目录／批量范围查询的仓储；未实现时返回 nil。
func (s *VisibleAccountService) scopeRepo() VisibleAccountScopeRepository {
	if s == nil || s.repo == nil {
		return nil
	}
	scope, _ := s.repo.(VisibleAccountScopeRepository)
	return scope
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
		if account == nil {
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
// 能力未开启返回 ErrAccountViewDisabled；未分配、已软删除或用户失去资格返回
// ErrVisibleAccountNotFound，三者对外不可区分。手动停用账号仍然可见。
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
	if account == nil {
		return VisibleAccountView{}, ErrVisibleAccountNotFound
	}
	return BuildVisibleAccountView(account), nil
}

// Groups 返回该用户可见账号实际所属的分组去重集合。
//
// 只从已授权账号的 account_groups 派生，不读取全局分组目录，也不混用用户 API Key
// 的可用分组。能力未开启时返回 ErrAccountViewDisabled；仓储未实现扩展接口时返回空集合。
func (s *VisibleAccountService) Groups(ctx context.Context, userID int64) ([]VisibleAccountGroup, error) {
	if s == nil || s.repo == nil {
		return nil, ErrAccountViewDisabled
	}
	enabled, err := s.repo.GetAccountViewEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrAccountViewDisabled
	}
	scope := s.scopeRepo()
	if scope == nil {
		return []VisibleAccountGroup{}, nil
	}
	return scope.ListVisibleAccountGroups(ctx, userID)
}

// Usage 返回单个账号的被动用量快照（只读）。
//
// 仅 Anthropic OAuth / Setup-Token 账号支持被动采样；其它平台返回 (nil, nil)
// 表示「暂无可用数据」，不发起上游探测、不刷新 Token、不补零。
// 未授权、已删除或用户失去资格时返回 ErrVisibleAccountNotFound。
func (s *VisibleAccountService) Usage(ctx context.Context, userID, accountID int64) (*UsageInfo, error) {
	if s == nil || s.repo == nil {
		return nil, ErrAccountViewDisabled
	}
	enabled, err := s.repo.GetAccountViewEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrAccountViewDisabled
	}
	account, err := s.repo.GetVisibleAccount(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrVisibleAccountNotFound
	}
	return s.passiveUsage(ctx, account), nil
}

// Stats 返回单个可见账号的整体使用统计（账号口径汇总）。
//
// 先做对象级授权（GetVisibleAccount + 能力开关），再调用只读统计读取器；
// 未授权账号与不存在账号返回同一 404，不通过统计接口暴露存在性。
// 读取器未接线时返回 (nil, nil)。返回的原始响应仍会由 handler 投影为安全 DTO
// （去掉上游端点明细），本方法只保证聚合口径与只读。
func (s *VisibleAccountService) Stats(
	ctx context.Context,
	userID, accountID int64,
	startTime, endTime time.Time,
) (*usagestats.AccountUsageStatsResponse, error) {
	if s == nil || s.repo == nil {
		return nil, ErrAccountViewDisabled
	}
	enabled, err := s.repo.GetAccountViewEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrAccountViewDisabled
	}
	account, err := s.repo.GetVisibleAccount(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrVisibleAccountNotFound
	}
	if s.usageReader == nil {
		return nil, nil
	}
	return s.usageReader.GetAccountUsageStats(ctx, accountID, startTime, endTime)
}

// RuntimeBatch 返回一页账号的运行期只读快照。
//
// 先做一次能力校验与范围查询（GetVisibleAccountsByIDs），只有落在授权集合内的 id
// 会出现在结果里；未授权或已删除的 id 被静默忽略，响应不暴露其存在性。
// 上限 maxVisibleAccountBatchIDs：超出一页的请求返回 ErrVisibleAccountBatchTooLarge。
// 今日统计与当前并发各走一次批量读取；被动用量只对 Anthropic OAuth / Setup-Token
// 账号读取，其余平台保持 nil，全程不发起上游探测。
func (s *VisibleAccountService) RuntimeBatch(
	ctx context.Context,
	userID int64,
	accountIDs []int64,
) (map[int64]VisibleAccountRuntime, error) {
	if s == nil || s.repo == nil {
		return nil, ErrAccountViewDisabled
	}
	enabled, err := s.repo.GetAccountViewEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrAccountViewDisabled
	}

	ids := uniquePositiveAccountIDs(accountIDs)
	if len(ids) > maxVisibleAccountBatchIDs {
		return nil, ErrVisibleAccountBatchTooLarge
	}
	result := make(map[int64]VisibleAccountRuntime, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	scope := s.scopeRepo()
	if scope == nil {
		// 仓储未实现批量范围查询：不逐行回退（那会变成 N+1 且可能越权），
		// 直接返回空结果表示「无授权账号命中」。
		return result, nil
	}
	accounts, err := scope.ListVisibleAccountsByIDs(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return result, nil
	}

	authorizedIDs := make([]int64, 0, len(accounts))
	byID := make(map[int64]*Account, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		authorizedIDs = append(authorizedIDs, account.ID)
		byID[account.ID] = account
	}
	if len(authorizedIDs) == 0 {
		return result, nil
	}

	var todayStats map[int64]*WindowStats
	if s.usageReader != nil {
		if stats, statsErr := s.usageReader.GetTodayStatsBatch(ctx, authorizedIDs); statsErr == nil {
			todayStats = stats
		}
	}
	var concurrency map[int64]int
	if s.concurrencyReader != nil {
		if counts, concErr := s.concurrencyReader.GetAccountConcurrencyBatch(ctx, authorizedIDs); concErr == nil {
			concurrency = counts
		}
	}
	usageByID := s.passiveUsageBatch(ctx, accounts)

	for _, id := range authorizedIDs {
		account := byID[id]
		runtime := VisibleAccountRuntime{
			Concurrency: account.Concurrency,
			Usage:       usageByID[id],
		}
		if todayStats != nil {
			if stats := todayStats[id]; stats != nil {
				runtime.TodayStats = stats
			}
		}
		if concurrency != nil {
			if count, ok := concurrency[id]; ok {
				current := count
				runtime.CurrentConcurrency = &current
			}
		}
		result[id] = runtime
	}
	return result, nil
}

// passiveUsageBatch 批量构建授权账号的被动快照。
//
// 优先走 VisibleAccountPassiveUsageBatchReader（复用已加载账号 + 批量窗口统计，
// 不逐账号回表）；读取器未实现该扩展时退回逐账号被动读取，保证功能等价。
func (s *VisibleAccountService) passiveUsageBatch(ctx context.Context, accounts []*Account) map[int64]*UsageInfo {
	out := make(map[int64]*UsageInfo)
	if s == nil || s.usageReader == nil || len(accounts) == 0 {
		return out
	}
	if batchReader, ok := s.usageReader.(VisibleAccountPassiveUsageBatchReader); ok {
		batch, err := batchReader.GetPassiveUsageBatch(ctx, accounts)
		if err == nil {
			for _, account := range accounts {
				if usage := batch[account.ID]; usage != nil {
					// 防御性收敛：即使读取器未做去合成 5h，也保证只读口径一致。
					if sanitized := sanitizePassiveUsageForRead(account, usage); sanitized != nil {
						out[account.ID] = sanitized
					}
				}
			}
			return out
		}
	}
	for _, account := range accounts {
		if usage := s.passiveUsage(ctx, account); usage != nil {
			out[account.ID] = usage
		}
	}
	return out
}

// passiveUsage 只在 Anthropic OAuth / Setup-Token 账号上读取被动采样。
//
// 其它平台返回 nil：GetPassiveUsage 只支持该形态，且这里绝不回退到主动 GetUsage。
// 读取失败（例如账号在此期间被删除）同样返回 nil，不把原始错误文本带给用户。
// 返回前按只读口径收敛：无真实 session 窗口时丢弃 estimate 合成的空 5h，
// 无任何被动事实时返回 nil（未知），与批量路径一致。
func (s *VisibleAccountService) passiveUsage(ctx context.Context, account *Account) *UsageInfo {
	if s == nil || s.usageReader == nil || account == nil {
		return nil
	}
	if !account.IsAnthropicOAuthOrSetupToken() {
		return nil
	}
	usage, err := s.usageReader.GetPassiveUsage(ctx, account.ID)
	if err != nil || usage == nil {
		return nil
	}
	return sanitizePassiveUsageForRead(account, usage)
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
	filter.AccountType = strings.ToLower(strings.TrimSpace(filter.AccountType))
	filter.Status = strings.ToLower(strings.TrimSpace(filter.Status))
	if filter.GroupID < 0 {
		filter.GroupID = 0
	}
	filter.Search = strings.TrimSpace(filter.Search)
	return filter
}

// uniquePositiveAccountIDs 去重并丢弃非正 id，供批量读取使用。
// 与管理员写入路径不同：批量读取里的非法 id 只可能来自错误客户端，
// 静默丢弃不会改写任何授权意图，只会少返回一项。
func uniquePositiveAccountIDs(ids []int64) []int64 {
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
// 只读取：基础资料（id/名称/平台/类型/状态/调度位/并发上限）、分组摘要、
// 调度态时间点、来自 Extra 的安全额度与窗口配置。
// 备注、凭据、代理、原始错误消息与完整 extra 一律不进入视图；上游身份只以
// 服务端掩码结果出现。所有可缺省字段用指针，缺省即 nil（未知），不补零冒充已知。
func BuildVisibleAccountView(account *Account) VisibleAccountView {
	if account == nil {
		return VisibleAccountView{}
	}
	email, username, upstreamAccountID := visibleAccountIdentity(account)
	view := VisibleAccountView{
		ID:                      account.ID,
		Name:                    account.Name,
		Platform:                account.Platform,
		AccountType:             account.Type,
		Status:                  account.Status,
		Schedulable:             account.Schedulable,
		Concurrency:             account.Concurrency,
		Groups:                  visibleAccountGroups(account),
		RateLimitResetAt:        account.RateLimitResetAt,
		OverloadUntil:           account.OverloadUntil,
		TempUnschedulableUntil:  account.TempUnschedulableUntil,
		EmailMasked:             maskVisibleAccountEmail(email),
		UsernameMasked:          maskVisibleAccountIdentity(username),
		UpstreamAccountIDMasked: maskVisibleAccountIdentity(upstreamAccountID),
	}
	// 这些窗口 / RPM 配置只在 Extra 里显式存在时返回：相关 getter 带默认值
	// （例如粘性预留默认 10、RPM 策略默认 tiered），用 getter 会把「未配置」
	// 显示成默认值，与「缺 null 不补默认」的口径冲突。
	view.WindowCostLimit = visibleAccountExtraFloatPtr(account.Extra, "window_cost_limit")
	view.WindowCostStickyReserve = visibleAccountExtraFloatPtr(account.Extra, "window_cost_sticky_reserve")
	view.MaxSessions = visibleAccountExtraIntPtr(account.Extra, "max_sessions")
	view.SessionIdleTimeoutMin = visibleAccountExtraIntPtr(account.Extra, "session_idle_timeout_minutes")
	view.BaseRPM = visibleAccountExtraIntPtr(account.Extra, "base_rpm")
	view.RPMStrategy = visibleAccountExtraStringPtr(account.Extra, "rpm_strategy")
	// 配额只在确实配置了上限时返回（与管理员账号列表同口径）；
	// 未配置即 nil，不把「无限制」显示成 0。
	if account.IsAPIKeyOrBedrock() {
		if limit := account.GetQuotaLimit(); limit > 0 {
			view.QuotaLimit = visibleAccountFloatPtr(limit)
			used := account.GetQuotaUsed()
			view.QuotaUsed = &used
		}
		if limit := account.GetQuotaDailyLimit(); limit > 0 {
			view.QuotaDailyLimit = visibleAccountFloatPtr(limit)
			used := account.GetQuotaDailyUsed()
			if account.IsDailyQuotaPeriodExpired() {
				used = 0
			}
			view.QuotaDailyUsed = &used
		}
		if limit := account.GetQuotaWeeklyLimit(); limit > 0 {
			view.QuotaWeeklyLimit = visibleAccountFloatPtr(limit)
			used := account.GetQuotaWeeklyUsed()
			if account.IsWeeklyQuotaPeriodExpired() {
				used = 0
			}
			view.QuotaWeeklyUsed = &used
		}
	}
	return view
}

// visibleAccountGroups 把账号所属分组投影为安全摘要（缺省为空切片，不是 nil，便于前端遍历）。
func visibleAccountGroups(account *Account) []VisibleAccountGroup {
	if account == nil || len(account.Groups) == 0 {
		return []VisibleAccountGroup{}
	}
	out := make([]VisibleAccountGroup, 0, len(account.Groups))
	for _, group := range account.Groups {
		if group == nil {
			continue
		}
		out = append(out, VisibleAccountGroup{
			ID:               group.ID,
			Name:             group.Name,
			Platform:         group.Platform,
			SubscriptionType: group.SubscriptionType,
		})
	}
	return out
}

func visibleAccountFloatPtr(v float64) *float64 { return &v }

// visibleAccountExtraFloatPtr 只在 Extra 显式存在该键时返回值，否则 nil（未知）。
func visibleAccountExtraFloatPtr(extra map[string]any, key string) *float64 {
	if extra == nil {
		return nil
	}
	raw, ok := extra[key]
	if !ok {
		return nil
	}
	v := parseExtraFloat64(raw)
	return &v
}

// visibleAccountExtraIntPtr 只在 Extra 显式存在该键时返回值，否则 nil（未知）。
func visibleAccountExtraIntPtr(extra map[string]any, key string) *int {
	if extra == nil {
		return nil
	}
	raw, ok := extra[key]
	if !ok {
		return nil
	}
	v := parseExtraInt(raw)
	return &v
}

// visibleAccountExtraStringPtr 只在 Extra 显式存在非空字符串时返回值，否则 nil（未知）。
func visibleAccountExtraStringPtr(extra map[string]any, key string) *string {
	if extra == nil {
		return nil
	}
	raw, ok := extra[key]
	if !ok || raw == nil {
		return nil
	}
	v := strings.TrimSpace(fmt.Sprint(raw))
	if v == "" {
		return nil
	}
	return &v
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
