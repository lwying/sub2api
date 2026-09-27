//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

// accountOptionsRepoStub 实现可选的 ListAccountOptions 能力（并集口径），
// 用于验证 service 优先走仓储自己的候选查询。
type accountOptionsRepoStub struct {
	accountRepoStub

	optionsCalls int
	gotStatus    string
	gotParams    pagination.PaginationParams
	gotGroupID   int64
	accounts     []Account
	total        int64
	err          error

	listWithFiltersCalls int
}

func (s *accountOptionsRepoStub) ListAccountOptions(
	_ context.Context,
	params pagination.PaginationParams,
	_, _, status, _ string,
	groupID int64,
) ([]Account, *pagination.PaginationResult, error) {
	s.optionsCalls++
	s.gotParams = params
	s.gotStatus = status
	s.gotGroupID = groupID
	if s.err != nil {
		return nil, nil, s.err
	}
	return s.accounts, &pagination.PaginationResult{Total: s.total}, nil
}

func (s *accountOptionsRepoStub) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string, string, int64, string) ([]Account, *pagination.PaginationResult, error) {
	s.listWithFiltersCalls++
	return nil, nil, nil
}

// TestAdminServiceImpl_ListAccountOptions_RequiresUnionForInactiveAndDisabled 固定
// 「停用」候选口径：inactive 与 disabled 是同一个选项，必须由仓储的并集查询回答。
//
// 当 accountRepo 没有实现可选 ListAccountOptions 时，service 不能退回精确过滤——
// 精确匹配只返回其中一个取值，会静默少给候选（例如只给 disabled 而漏掉历史 inactive）。
// 该守卫必须与 repository.ListAccountOptions 用同一套「去空白 + 忽略大小写」归一化，
// 否则 status=disabled、DISABLED、InActive 这类写法会绕过守卫、静默收窄结果。
func TestAdminServiceImpl_ListAccountOptions_RequiresUnionForInactiveAndDisabled(t *testing.T) {
	for _, status := range []string{
		StatusInactive,
		StatusDisabled,
		"DISABLED",
		" Disabled ",
		"InActive",
		" inactive ",
	} {
		t.Run(status, func(t *testing.T) {
			repo := &accountRepoStubForAdminList{
				listWithFiltersAccounts: []Account{{ID: 1, Name: "legacy"}},
				listWithFiltersResult:   &pagination.PaginationResult{Total: 1},
			}
			svc := &adminServiceImpl{accountRepo: repo}

			accounts, total, err := svc.ListAccountOptions(context.Background(), 1, 20, "", "", status, "", 0)
			require.Error(t, err, "status=%q must not silently narrow to one exact value", status)
			require.Contains(t, err.Error(), "account candidate status union is unavailable")
			require.Nil(t, accounts)
			require.Zero(t, total)
			require.Zero(t, repo.listWithFiltersCalls,
				"status=%q must not fall back to the exact ListWithFilters filter", status)
		})
	}
}

// TestAdminServiceImpl_ListAccountOptions_FallsBackForNonUnionStatuses 固定回退仍然
// 可用：不是「停用」口径的状态照旧交给 ListWithFilters，且原样透传。
func TestAdminServiceImpl_ListAccountOptions_FallsBackForNonUnionStatuses(t *testing.T) {
	repo := &accountRepoStubForAdminList{
		listWithFiltersAccounts: []Account{{ID: 2, Name: "active"}},
		listWithFiltersResult:   &pagination.PaginationResult{Total: 1},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	accounts, total, err := svc.ListAccountOptions(context.Background(), 1, 20, "", "", StatusActive, "", 0)
	require.NoError(t, err)
	require.Equal(t, []Account{{ID: 2, Name: "active"}}, accounts)
	require.Equal(t, int64(1), total)
	require.Equal(t, 1, repo.listWithFiltersCalls)
	require.Equal(t, StatusActive, repo.listWithFiltersStatus)
}

// TestAdminServiceImpl_ListAccountOptions_PrefersRepositoryUnionQuery 固定首选路径：
// 仓储实现了候选查询时，service 直接把原始筛选（含原样 status）交给它，不再二次过滤。
func TestAdminServiceImpl_ListAccountOptions_PrefersRepositoryUnionQuery(t *testing.T) {
	repo := &accountOptionsRepoStub{
		accounts: []Account{{ID: 3, Name: "opt", Status: StatusInactive}},
		total:    1,
	}
	svc := &adminServiceImpl{accountRepo: repo}

	accounts, total, err := svc.ListAccountOptions(context.Background(), 2, 10, PlatformOpenAI, AccountTypeOAuth, StatusDisabled, "opt", 7)
	require.NoError(t, err)
	require.Equal(t, []Account{{ID: 3, Name: "opt", Status: StatusInactive}}, accounts)
	require.Equal(t, int64(1), total)
	require.Equal(t, 1, repo.optionsCalls)
	require.Zero(t, repo.listWithFiltersCalls)
	require.Equal(t, StatusDisabled, repo.gotStatus, "status 必须原样透传，由仓储做并集归一化")
	require.Equal(t, int64(7), repo.gotGroupID)
	require.Equal(t, pagination.PaginationParams{Page: 2, PageSize: 10, SortBy: "name", SortOrder: "asc"}, repo.gotParams)
}
