package repository

import (
	"context"
	"errors"
	"strconv"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	dbpredicate "github.com/Wei-Shaw/sub2api/ent/predicate"
	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	dbuvaccount "github.com/Wei-Shaw/sub2api/ent/uservisibleaccount"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"

	entsql "entgo.io/ent/dialect/sql"
)

// user_visible_account_repo.go 实现普通用户「已分配账号只读查看」的数据访问。
//
// 所有面向普通用户的读取都在 SQL 层强制：用户存在且未禁用、能力开关已开启、
// 存在显式分配关系、账号未软删除。手动停用账号保持可见（按状态展示）；
// 授权撤销、用户禁用与账号软删除在下一次请求即生效；
// 不复用任何调度过滤（限流／过载／临时不可调度账号仍可见）。

// userVisibleAccountRepository 是 service.VisibleAccountRepository 的 Ent 实现。
type userVisibleAccountRepository struct {
	client *dbent.Client
}

// NewUserVisibleAccountRepository 构造账号查看仓储。
func NewUserVisibleAccountRepository(client *dbent.Client) service.VisibleAccountRepository {
	return &userVisibleAccountRepository{client: client}
}

// GetAccountViewEnabled 读取用户的能力开关。
// 用户不存在、已软删除或已被禁用时返回 false：与列表／详情在 SQL 层的门禁
// 同口径，避免 /auth/me 或任何直接调用给出与账号可见范围不一致的能力声明。
func (r *userVisibleAccountRepository) GetAccountViewEnabled(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	user, err := r.client.User.Query().
		Where(
			dbuser.IDEQ(userID),
			dbuser.DeletedAtIsNil(),
			dbuser.StatusEQ(domain.StatusActive),
		).
		Select(dbuser.FieldCanViewAssignedAccounts).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return user.CanViewAssignedAccounts, nil
}

// GetStoredAccountViewEnabled 读取用户存储的能力开关（管理员视角）。
//
// 与 GetAccountViewEnabled 的唯一差别在用户状态：只要用户行仍在（未软删除），
// 已禁用用户也返回其真实存储值。否则管理员界面会把存储的 true 显示成 false，
// 「打开即保存」一次就把能力真正关掉——一次误操作即永久撤销。
//
// 用户不存在或已软删除时返回 ErrAccountViewUserNotFound，与 PUT 同口径：
// 猜 ID 无法区分「不存在」与「已删除」。
func (r *userVisibleAccountRepository) GetStoredAccountViewEnabled(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, service.ErrAccountViewUserNotFound
	}
	user, err := r.client.User.Query().
		Where(dbuser.IDEQ(userID), dbuser.DeletedAtIsNil()).
		Select(dbuser.FieldCanViewAssignedAccounts).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return false, service.ErrAccountViewUserNotFound
		}
		return false, err
	}
	return user.CanViewAssignedAccounts, nil
}

// UpdateAccountView 在单个事务内更新能力开关与分配集合。
//
// 事务一开始就锁住目标用户行（SELECT ... FOR UPDATE），再做任何校验与写入。
// READ COMMITTED 下，两个并发的「全量替换 account_ids」请求若各自先删后插，
// 互相看不到对方未提交的行，提交后会把两次分配并集成并集——一个请求里被撤销的
// 账号会被另一个请求悄悄留下，等于越权授予。用户行锁把同一用户的替换串行化，
// 后到者能看到先提交者的行并按自己的意图覆盖。仅切换开关（account_ids 省略）
// 也走同一把锁，避免与并发替换交错。
//
// 分配集合按增量替换：只删除被撤销的行、只插入新增的行，保留的行原样不动，
// 因此幂等重复提交、追加分配与开关切换都不会改写既有行的 granted_by/created_at。
//
// 任一校验失败（用户不存在或已软删除、账号 id 不存在）都整体回滚，
// 不会留下「开关已打开但分配未落库」的中间状态。
// 账号 id 不可用时的错误带本次被拒的具体 id（service.UnknownVisibleAccountError），
// 供管理员界面指出失败项；错误只描述 id，不含账号名称或凭据。
func (r *userVisibleAccountRepository) UpdateAccountView(
	ctx context.Context,
	userID int64,
	enabled *bool,
	accountIDs *[]int64,
	grantedBy *int64,
) error {
	if userID <= 0 {
		return service.ErrAccountViewUserNotFound
	}

	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return err
	}
	var txClient *dbent.Client
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		txClient = tx.Client()
	} else {
		txClient = r.client
	}

	// 锁住目标用户行：不存在或已软删除 → 404，且不写入任何数据。
	if _, err := txClient.User.Query().
		Where(dbuser.IDEQ(userID), dbuser.DeletedAtIsNil()).
		Select(dbuser.FieldID).
		ForUpdate().
		Only(ctx); err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrAccountViewUserNotFound
		}
		return err
	}

	// 先校验全部输入，再做任何写入；当前分配集合既用于校验也用于增量替换。
	existing, err := txClient.UserVisibleAccount.Query().
		Where(dbuvaccount.UserIDEQ(userID)).
		Select(dbuvaccount.FieldAccountID).
		All(ctx)
	if err != nil {
		return err
	}
	current := make(map[int64]struct{}, len(existing))
	for _, row := range existing {
		current[row.AccountID] = struct{}{}
	}

	var ids []int64
	if accountIDs != nil {
		// 非法（<=0）id 明确报错，而不是被静默丢弃成「清空分配」；
		// 报错时同样指出具体是哪几项。
		if invalid := nonPositiveAccountIDs(*accountIDs); len(invalid) > 0 {
			return service.UnknownVisibleAccountError(invalid)
		}
		ids = uniquePositiveIDs(*accountIDs)
		if err := validateNewVisibleAccountIDs(ctx, txClient, ids, current); err != nil {
			return err
		}
	}

	if enabled != nil {
		if _, err := txClient.User.Update().
			Where(dbuser.IDEQ(userID), dbuser.DeletedAtIsNil()).
			SetCanViewAssignedAccounts(*enabled).
			Save(ctx); err != nil {
			return err
		}
	}

	if accountIDs != nil {
		if err := replaceVisibleAccountAssignments(ctx, txClient, userID, ids, current, grantedBy); err != nil {
			return err
		}
	}

	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// validateNewVisibleAccountIDs 校验本次请求里「新增」的分配 id：
// 必须存在且未软删除。已经在分配关系里的 id 一律放行，即使对应账号已被软删除。
//
// 放行保留是必要的：管理员界面的 account_ids 是权威集合，会原样回传（详情缺失的
// id 也照传）。若拒绝，原样保存会变成 400；而如果把这类 id 当成未知账号隐式丢弃，
// 原样保存就会静默撤销这条分配关系。
//
// 失败时返回带具体 id 的 ErrUnknownVisibleAccount 副本（service.UnknownVisibleAccountError）：
// 整批授权都不落库，但管理员能据此知道是哪个账号在此期间被删除或失效，从而移除或
// 替换该项后重试，而不是面对一句无从下手的泛化失败。校验发生在任何写入之前，
// 因此失败仍不产生部分授权。
func validateNewVisibleAccountIDs(
	ctx context.Context,
	txClient *dbent.Client,
	ids []int64,
	current map[int64]struct{},
) error {
	newIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := current[id]; ok {
			continue
		}
		newIDs = append(newIDs, id)
	}
	if len(newIDs) == 0 {
		return nil
	}
	live, err := txClient.Account.Query().
		Where(dbaccount.IDIn(newIDs...), dbaccount.DeletedAtIsNil()).
		Select(dbaccount.FieldID).
		All(ctx)
	if err != nil {
		return err
	}
	liveIDs := make(map[int64]struct{}, len(live))
	for _, account := range live {
		liveIDs[account.ID] = struct{}{}
	}
	if missing := missingVisibleAccountIDs(newIDs, liveIDs); len(missing) > 0 {
		return service.UnknownVisibleAccountError(missing)
	}
	return nil
}

// missingVisibleAccountIDs 返回 wanted 里不存在于 live 的 id，保持 wanted 的顺序。
// 纯函数：只描述「哪些新增项不可用」，不触碰任何行。
func missingVisibleAccountIDs(wanted []int64, live map[int64]struct{}) []int64 {
	var missing []int64
	for _, id := range wanted {
		if _, ok := live[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// replaceVisibleAccountAssignments 在当前事务里把分配集合替换为 wanted，但只做增量：
// 撤销的删除、新增的插入、保留的原样不动。
//
// 「保留」是有意的：granted_by/created_at 记录首次授予事实（谁在何时把它授给该用户），
// 重复提交同一集合、追加新分配或仅切换开关都不该改写已存在行的这两列。整表删除重建
// 会把 created_at 刷新成当下、把 granted_by 改成最后一个操作者，丢掉授权来源。
func replaceVisibleAccountAssignments(
	ctx context.Context,
	txClient *dbent.Client,
	userID int64,
	wanted []int64,
	current map[int64]struct{},
	grantedBy *int64,
) error {
	want := make(map[int64]struct{}, len(wanted))
	for _, id := range wanted {
		want[id] = struct{}{}
	}

	revoke := make([]int64, 0, len(current))
	for id := range current {
		if _, ok := want[id]; !ok {
			revoke = append(revoke, id)
		}
	}
	if len(revoke) > 0 {
		if _, err := txClient.UserVisibleAccount.Delete().
			Where(dbuvaccount.UserIDEQ(userID), dbuvaccount.AccountIDIn(revoke...)).
			Exec(ctx); err != nil {
			return err
		}
	}

	builders := make([]*dbent.UserVisibleAccountCreate, 0, len(wanted))
	for _, id := range wanted {
		if _, ok := current[id]; ok {
			continue
		}
		builder := txClient.UserVisibleAccount.Create().
			SetUserID(userID).
			SetAccountID(id)
		if grantedBy != nil && *grantedBy > 0 {
			builder = builder.SetGrantedBy(*grantedBy)
		}
		builders = append(builders, builder)
	}
	if len(builders) > 0 {
		if err := txClient.UserVisibleAccount.CreateBulk(builders...).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ListAssignedAccountIDs 返回该用户全部已存储的分配 id（升序），包括对应账号已被
// 软删除因而没有详情可展示的分配。
//
// 管理员界面的 account_ids 是权威集合，GET 与 PUT 都按整份集合替换：这里过滤掉
// 软删除账号，管理员在界面上「打开即保存」就会把那条关系静默撤销。用户侧可见范围
// 不受影响（见 visibleAccountsQuery）。
func (r *userVisibleAccountRepository) ListAssignedAccountIDs(ctx context.Context, userID int64) ([]int64, error) {
	if userID <= 0 {
		return []int64{}, nil
	}
	rows, err := r.client.UserVisibleAccount.Query().
		Where(dbuvaccount.UserIDEQ(userID)).
		Select(dbuvaccount.FieldAccountID).
		Order(dbuvaccount.ByAccountID()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.AccountID)
	}
	return ids, nil
}

// ListAssignedAccountSummaries 返回该用户的全部分配账号详情（含手动禁用），
// 供管理员撤销；不要求能力开关开启。
//
// 这是「详情」而不是权威集合：已被软删除的账号不在这里（它已不可读），但它的
// 分配 id 仍在 ListAssignedAccountIDs 中，管理员界面按 id 保留占位条目。
func (r *userVisibleAccountRepository) ListAssignedAccountSummaries(
	ctx context.Context,
	userID int64,
) ([]service.AssignedVisibleAccount, error) {
	if userID <= 0 {
		return []service.AssignedVisibleAccount{}, nil
	}
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.DeletedAtIsNil(),
			dbaccount.HasVisibleUsersWith(dbuser.IDEQ(userID)),
		).
		Select(
			dbaccount.FieldID,
			dbaccount.FieldName,
			dbaccount.FieldPlatform,
			dbaccount.FieldType,
			dbaccount.FieldStatus,
		).
		Order(dbaccount.ByID()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.AssignedVisibleAccount, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, service.AssignedVisibleAccount{
			ID:       account.ID,
			Name:     account.Name,
			Platform: account.Platform,
			Type:     account.Type,
			Status:   account.Status,
		})
	}
	return out, nil
}

// ListVisibleAccounts 返回普通用户当前可见的账号分页。
func (r *userVisibleAccountRepository) ListVisibleAccounts(
	ctx context.Context,
	userID int64,
	filter service.VisibleAccountFilter,
) ([]*service.Account, int64, error) {
	if userID <= 0 {
		return []*service.Account{}, 0, nil
	}
	total, err := r.visibleAccountsQuery(userID, filter).Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []*service.Account{}, 0, nil
	}
	accounts, err := r.visibleAccountsQuery(userID, filter).
		Order(dbaccount.ByID()).
		Offset((filter.Page - 1) * filter.PageSize).
		Limit(filter.PageSize).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := r.accountsToService(ctx, accounts)
	return out, int64(total), nil
}

// ListVisibleAccountsByIDs 返回授权集合内命中给定 id 的可见账号（一次范围查询）。
//
// 未授权、对应账号已软删除、用户已禁用或资格已关闭的 id 一律不出现在结果里；
// 调用方据此只回传授权 id，不会通过错误详情暴露未授权 id 是否存在。
func (r *userVisibleAccountRepository) ListVisibleAccountsByIDs(
	ctx context.Context,
	userID int64,
	accountIDs []int64,
) ([]*service.Account, error) {
	ids := uniquePositiveIDs(accountIDs)
	if userID <= 0 || len(ids) == 0 {
		return []*service.Account{}, nil
	}
	accounts, err := r.visibleAccountsQuery(userID, service.VisibleAccountFilter{}).
		Where(dbaccount.IDIn(ids...)).
		Order(dbaccount.ByID()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts), nil
}

// ListVisibleAccountGroups 返回该用户可见账号实际所属的分组去重集合。
//
// 只从已授权账号的 account_groups 派生，不读取全局分组目录，也不混用用户 API Key
// 的可用分组。分组顺序按 id 升序，便于前端稳定渲染。
func (r *userVisibleAccountRepository) ListVisibleAccountGroups(
	ctx context.Context,
	userID int64,
) ([]service.VisibleAccountGroup, error) {
	if userID <= 0 {
		return []service.VisibleAccountGroup{}, nil
	}
	visible, err := r.visibleAccountsQuery(userID, service.VisibleAccountFilter{}).
		Select(dbaccount.FieldID).
		All(ctx)
	if err != nil {
		return nil, err
	}
	accountIDs := make([]int64, 0, len(visible))
	for _, account := range visible {
		accountIDs = append(accountIDs, account.ID)
	}
	if len(accountIDs) == 0 {
		return []service.VisibleAccountGroup{}, nil
	}
	entries, err := r.client.AccountGroup.Query().
		Where(dbaccountgroup.AccountIDIn(accountIDs...)).
		Select(dbaccountgroup.FieldGroupID).
		All(ctx)
	if err != nil {
		return nil, err
	}
	groupIDs := make([]int64, 0, len(entries))
	for _, entry := range entries {
		groupIDs = append(groupIDs, entry.GroupID)
	}
	groupIDs = uniquePositiveIDs(groupIDs)
	if len(groupIDs) == 0 {
		return []service.VisibleAccountGroup{}, nil
	}
	groups, err := r.client.Group.Query().
		Where(dbgroup.IDIn(groupIDs...), dbgroup.DeletedAtIsNil()).
		Order(dbgroup.ByID()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.VisibleAccountGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, service.VisibleAccountGroup{
			ID:               group.ID,
			Name:             group.Name,
			Platform:         group.Platform,
			SubscriptionType: group.SubscriptionType,
		})
	}
	return out, nil
}

// GetVisibleAccount 返回单个可见账号；不可见（未分配／已删除／资格关闭）时返回 (nil, nil)。
//
// 注意：手动停用（disabled/inactive）不再使账号不可见——管理员已授权即应看到停用状态。
func (r *userVisibleAccountRepository) GetVisibleAccount(
	ctx context.Context,
	userID, accountID int64,
) (*service.Account, error) {
	if userID <= 0 || accountID <= 0 {
		return nil, nil
	}
	account, err := r.visibleAccountsQuery(userID, service.VisibleAccountFilter{}).
		Where(dbaccount.IDEQ(accountID)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := r.accountsToService(ctx, []*dbent.Account{account})
	if len(out) == 0 {
		return nil, nil
	}
	return out[0], nil
}

// accountsToService 把 ent 账号投影为 service 账号并附加所属分组摘要。
//
// 分组通过 account_groups 一次批量加载（不是逐账号 N+1）；加载失败时保持
// 返回账号但分组为空——列表的主体字段不应因为分组联查失败而整页报错。
func (r *userVisibleAccountRepository) accountsToService(
	ctx context.Context,
	accounts []*dbent.Account,
) []*service.Account {
	out := make([]*service.Account, 0, len(accounts))
	if len(accounts) == 0 {
		return out
	}
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		out = append(out, accountEntityToService(account))
		ids = append(ids, account.ID)
	}
	groupsByAccount, err := r.loadVisibleGroupsByAccount(ctx, ids)
	if err != nil {
		return out
	}
	for _, account := range out {
		if groups, ok := groupsByAccount[account.ID]; ok {
			account.Groups = groups
		}
	}
	return out
}

// loadVisibleGroupsByAccount 一次查询 account_groups 与 groups，返回按账号归并的分组。
func (r *userVisibleAccountRepository) loadVisibleGroupsByAccount(
	ctx context.Context,
	accountIDs []int64,
) (map[int64][]*service.Group, error) {
	out := make(map[int64][]*service.Group)
	ids := uniquePositiveIDs(accountIDs)
	if len(ids) == 0 {
		return out, nil
	}
	entries, err := r.client.AccountGroup.Query().
		Where(dbaccountgroup.AccountIDIn(ids...)).
		Order(dbaccountgroup.ByAccountID(), dbaccountgroup.ByPriority()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return out, nil
	}
	groupIDs := make([]int64, 0, len(entries))
	for _, entry := range entries {
		groupIDs = append(groupIDs, entry.GroupID)
	}
	groups, err := r.client.Group.Query().
		Where(dbgroup.IDIn(uniquePositiveIDs(groupIDs)...), dbgroup.DeletedAtIsNil()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	groupMap := make(map[int64]*service.Group, len(groups))
	for _, group := range groups {
		groupMap[group.ID] = groupEntityToService(group)
	}
	for _, entry := range entries {
		if group := groupMap[entry.GroupID]; group != nil {
			out[entry.AccountID] = append(out[entry.AccountID], group)
		}
	}
	return out, nil
}

// visibleAccountsQuery 构造「当前用户可见账号」的查询：
// 账号未软删除 + 该用户能力已开启、未禁用且持有显式分配。
//
// 手动停用（disabled/inactive）不再排除：管理员已授权的账号即使被停用也保持可见，
// 只是以停用状态展示。撤权、用户禁用、资格关闭与账号软删除仍然即时生效。
//
// search 匹配对用户已公开的字段（数字账号 ID、名称、平台、类型），全部在已授权
// 集合内部执行，不会扩大或探测范围。
func (r *userVisibleAccountRepository) visibleAccountsQuery(
	userID int64,
	filter service.VisibleAccountFilter,
) *dbent.AccountQuery {
	query := r.client.Account.Query().Where(
		dbaccount.DeletedAtIsNil(),
		dbaccount.HasVisibleUsersWith(
			dbuser.IDEQ(userID),
			dbuser.DeletedAtIsNil(),
			dbuser.StatusEQ(domain.StatusActive),
			dbuser.CanViewAssignedAccountsEQ(true),
		),
	)
	if filter.Platform != "" {
		query = query.Where(dbaccount.PlatformEQ(filter.Platform))
	}
	if filter.AccountType != "" {
		query = query.Where(dbaccount.TypeEQ(filter.AccountType))
	}
	if filter.Status != "" {
		query = query.Where(dbaccountStatusEquals(filter.Status))
	}
	if filter.GroupID > 0 {
		query = query.Where(dbaccount.HasAccountGroupsWith(dbaccountgroup.GroupIDEQ(filter.GroupID)))
	}
	if filter.Search != "" {
		if numeric, err := strconv.ParseInt(filter.Search, 10, 64); err == nil {
			query = query.Where(dbaccount.IDEQ(numeric))
		} else {
			query = query.Where(dbaccount.Or(
				dbaccount.NameContainsFold(filter.Search),
				dbaccount.PlatformContainsFold(filter.Search),
				dbaccount.TypeContainsFold(filter.Search),
			))
		}
	}
	return query
}

// dbaccountStatusEquals 在 SQL 层做状态过滤：
// LOWER(BTRIM(status, <与 Go strings.TrimSpace 同集合的空白>)) 与过滤值比较。
//
// 与展示脱敏使用的空白口径一致：大小写与首尾空白（含 NBSP、全角空格等）都不改变结论，
// 否则同一状态的账号会因空白差异漏出列表。过滤值已由服务层归一为小写去空白。
//
// inactive 是归一化后的「停用」语义：历史库同时存在 disabled 与 inactive 两种写法，
// 面向用户的筛选只暴露 inactive，因此 status=inactive 必须同时命中两者；
// 其余状态保持精确等值匹配。
func dbaccountStatusEquals(status string) dbpredicate.Account {
	values := []string{status}
	if status == domain.StatusInactive {
		values = []string{domain.StatusInactive, domain.StatusDisabled}
	}
	return dbpredicate.Account(func(s *entsql.Selector) {
		col := s.C(dbaccount.FieldStatus)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("LOWER(BTRIM(COALESCE(").
				WriteString(col).
				WriteString(", ''), ").
				WriteString(postgresTrimSpaceChars).
				WriteString("))")
			if len(values) == 1 {
				b.WriteString(" = ").Arg(values[0])
				return
			}
			b.WriteString(" IN (")
			b.Arg(values[0])
			b.WriteString(", ")
			b.Arg(values[1])
			b.WriteString(")")
		}))
	})
}

// postgresTrimSpaceChars 是与 Go strings.TrimSpace（unicode.IsSpace）同一集合的
// 空白字符字面量，供 BTRIM 的字符集参数使用。
//
// 集合 = \t \n \v \f \r、空格、NEL(U+0085)、NBSP(U+00A0)、Ogham 空格(U+1680)、
// U+2000–U+200A、U+2028、U+2029、U+202F、U+205F、表意空格(U+3000)。
// 零宽空格(U+200B) 不属于空白，两侧都不得当成手动禁用变体。
//
// 用 \uXXXX 转义需要数据库编码为 UTF8（本项目的迁移与部署都以 UTF8 建库）。
const postgresTrimSpaceChars = `E'\x09\x0a\x0b\x0c\x0d\x20\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000'`

// nonPositiveAccountIDs 返回请求里的非正整数 id。这类输入只可能来自错误的
// 客户端或篡改的载荷，不能当成「该项未提供」或「清空分配」处理。返回原值而不是
// 布尔，是为了让失败响应能指出具体是哪几项（含 id <= 0 的项）。
func nonPositiveAccountIDs(ids []int64) []int64 {
	var invalid []int64
	for _, id := range ids {
		if id <= 0 {
			invalid = append(invalid, id)
		}
	}
	return invalid
}

func uniquePositiveIDs(ids []int64) []int64 {
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
