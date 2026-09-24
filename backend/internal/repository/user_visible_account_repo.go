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

// UpdateAccountView 在单个事务内更新能力开关与分配集合。
//
// 任一校验失败（用户不存在、账号 id 不存在）都整体回滚，
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

	exists, err := txClient.User.Query().
		Where(dbuser.IDEQ(userID), dbuser.DeletedAtIsNil()).
		Exist(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return service.ErrAccountViewUserNotFound
	}

	// 先校验全部输入，再做任何写入。
	var ids []int64
	if accountIDs != nil {
		// 非法（<=0）id 明确报错，而不是被静默丢弃成「清空分配」。
		if err := validateAccountIDs(*accountIDs); err != nil {
			return err
		}
		ids = uniquePositiveIDs(*accountIDs)
		if len(ids) > 0 {
			found, err := txClient.Account.Query().
				Where(dbaccount.IDIn(ids...), dbaccount.DeletedAtIsNil()).
				Count(ctx)
			if err != nil {
				return err
			}
			if found != len(ids) {
				return service.ErrUnknownVisibleAccount
			}
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
		if _, err := txClient.UserVisibleAccount.Delete().
			Where(dbuvaccount.UserIDEQ(userID)).
			Exec(ctx); err != nil {
			return err
		}
		if len(ids) > 0 {
			builders := make([]*dbent.UserVisibleAccountCreate, 0, len(ids))
			for _, accountID := range ids {
				builder := txClient.UserVisibleAccount.Create().
					SetUserID(userID).
					SetAccountID(accountID)
				if grantedBy != nil && *grantedBy > 0 {
					builder = builder.SetGrantedBy(*grantedBy)
				}
				builders = append(builders, builder)
			}
			if err := txClient.UserVisibleAccount.CreateBulk(builders...).Exec(ctx); err != nil {
				return err
			}
		}
	}

	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ListAssignedAccountSummaries 返回该用户的全部分配账号（含手动禁用），
// 供管理员撤销；不要求能力开关开启。
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
// LOWER(BTRIM(status)) 不属于 {disabled, inactive}。
//
// 与 service.accountManuallyDisabled 保持同一口径（大小写与首尾空白不敏感），
// 保证分页计数与返回内容一致；error／expired／限流等状态不在此列，保持可见。
func dbaccountNotManuallyDisabled() dbpredicate.Account {
	return dbpredicate.Account(func(s *entsql.Selector) {
		col := s.C(dbaccount.FieldStatus)
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("LOWER(BTRIM(COALESCE(").
				WriteString(col).
				WriteString(", ''))) NOT IN (").
				Arg(domain.StatusDisabled).
				WriteString(", ").
				Arg(domain.StatusInactive).
				WriteString(")")
		}))
	})
}

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
