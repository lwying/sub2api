package repository

import (
	"context"
	"errors"
	"strconv"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
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
// 存在显式分配关系、账号未软删除且未被手动禁用。授权撤销、用户禁用与账号禁用
// 在下一次请求即生效；不复用任何调度过滤（限流／过载／临时不可调度账号仍可见）。

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
		// 非法（<=0）id 明确报错，而不是被静默丢弃成「清空分配」。
		if err := validateAccountIDs(*accountIDs); err != nil {
			return err
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
	found, err := txClient.Account.Query().
		Where(dbaccount.IDIn(newIDs...), dbaccount.DeletedAtIsNil()).
		Count(ctx)
	if err != nil {
		return err
	}
	if found != len(newIDs) {
		return service.ErrUnknownVisibleAccount
	}
	return nil
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
	out := make([]*service.Account, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, accountEntityToService(account))
	}
	return out, int64(total), nil
}

// GetVisibleAccount 返回单个可见账号；不可见（未分配／已删除／已禁用）时返回 (nil, nil)。
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
	return accountEntityToService(account), nil
}

// visibleAccountsQuery 构造「当前用户可见账号」的查询：
// 账号未软删除且未被手动禁用 + 该用户能力已开启、未禁用且持有显式分配。
//
// search 只匹配对用户已公开的字段（数字账号 ID、平台、类型），
// 不匹配管理员专有的账号名称，避免额外的信息探知通道。
func (r *userVisibleAccountRepository) visibleAccountsQuery(
	userID int64,
	filter service.VisibleAccountFilter,
) *dbent.AccountQuery {
	query := r.client.Account.Query().Where(
		dbaccount.DeletedAtIsNil(),
		// 手动禁用判定必须与服务层同口径：大小写与首尾空白都不改变结论，
		// 否则列表计数会与过滤后的内容不一致。
		dbaccountNotManuallyDisabled(),
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
	if filter.Search != "" {
		if numeric, err := strconv.ParseInt(filter.Search, 10, 64); err == nil {
			query = query.Where(dbaccount.IDEQ(numeric))
		} else {
			query = query.Where(dbaccount.Or(
				dbaccount.PlatformContainsFold(filter.Search),
				dbaccount.TypeContainsFold(filter.Search),
			))
		}
	}
	return query
}

// dbaccountNotManuallyDisabled 在 SQL 层排除手动禁用账号：
// LOWER(BTRIM(status, <与 Go strings.TrimSpace 同集合的空白>)) 不属于
// {disabled, inactive}。
//
// 与 service.accountManuallyDisabled 必须严格同口径：计数的 SQL 与返回内容的 DTO
// 过滤是两处实现，空白定义若不一致（例如 SQL 只裁空格、Go 还裁制表符），
// 页面 total 就会把 DTO 过滤掉的账号算进去，出现「计数 1、列表为空」。
// 反方向（SQL 裁得比 Go 少）不会造成计数不一致，但会把一个判为手动的账号继续
// 展示给用户；因此这里取 Go 的完整空白集合。
//
// error／expired／限流等状态不在此列，保持可见。
func dbaccountNotManuallyDisabled() dbpredicate.Account {
	return dbpredicate.Account(func(s *entsql.Selector) {
		col := s.C(dbaccount.FieldStatus)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("LOWER(BTRIM(COALESCE(").
				WriteString(col).
				WriteString(", ''), ").
				WriteString(postgresTrimSpaceChars).
				WriteString(")) NOT IN (").
				Arg(domain.StatusDisabled).
				WriteString(", ").
				Arg(domain.StatusInactive).
				WriteString(")")
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

// validateAccountIDs 拒绝对非正整数 id 的分配请求。这类输入只可能来自错误的
// 客户端或篡改的载荷，不能当成「该项未提供」或「清空分配」处理。
func validateAccountIDs(ids []int64) error {
	for _, id := range ids {
		if id <= 0 {
			return service.ErrUnknownVisibleAccount
		}
	}
	return nil
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
