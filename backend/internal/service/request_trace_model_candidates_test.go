//go:build unit

package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type traceCandidateAccountRepoStub struct {
	accountRepoStub
	accounts []Account
	err      error
}

func (s *traceCandidateAccountRepoStub) ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	return s.accounts, s.err
}

type traceCandidateCompositeRouteRepoStub struct {
	compositeRouteRepoStub
	publicModels []string
	err          error
	calls        int
}

func (s *traceCandidateCompositeRouteRepoStub) ListEnabledPublicModels(context.Context) ([]string, error) {
	s.calls++
	return s.publicModels, s.err
}

// traceCandidateAccountSourceKeyRepoStub exposes the narrow source-only reader
// and fails the test if the full account list is loaded instead.
type traceCandidateAccountSourceKeyRepoStub struct {
	accountRepoStub
	keys           []string
	err            error
	sourceKeyCalls int
	listAllCalls   int
}

func (s *traceCandidateAccountSourceKeyRepoStub) ListModelMappingSourceKeys(context.Context) ([]string, error) {
	s.sourceKeyCalls++
	return s.keys, s.err
}

func (s *traceCandidateAccountSourceKeyRepoStub) ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	s.listAllCalls++
	return nil, errors.New("full account list must not be loaded when the source-only reader exists")
}

func newTraceCandidateAdminService(groupRepo GroupRepository, accountRepo AccountRepository, routeRepo CompositeModelRouteRepository) *adminServiceImpl {
	return &adminServiceImpl{groupRepo: groupRepo, accountRepo: accountRepo, compositeRouteRepo: routeRepo}
}

func countFold(models []string, target string) int {
	n := 0
	for _, model := range models {
		if strings.EqualFold(model, target) {
			n++
		}
	}
	return n
}

func TestAdminServiceImplGetRequestTraceModelCandidatesAggregatesGlobalSources(t *testing.T) {
	groupRepo := &groupRepoStubForAdmin{
		listWithFiltersGroups: []Group{
			{
				ID:             1,
				ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"  Zed-Source-Alpha  ", "zed-glob-*", ""}},
				ModelRouting:   map[string][]int64{"zed-route-exact": {1}, "zed-route-glob-*": {2}},
			},
		},
	}
	accountRepo := &traceCandidateAccountRepoStub{
		accounts: []Account{
			{
				ID:       1,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"acct-source-key": "upstream-target-xyz"},
				},
			},
		},
	}
	routeRepo := &traceCandidateCompositeRouteRepoStub{publicModels: []string{"route-public-model", "route-glob-*"}}
	svc := newTraceCandidateAdminService(groupRepo, accountRepo, routeRepo)

	defaults := compositeDefaultModelsListCandidateIDs()
	tooLong := strings.Repeat("z", requestTraceModelCandidateMaxLength+1)
	channelKeys := []string{"channel-source-key", "ZED-SOURCE-ALPHA", "channel-glob-*", tooLong, "  channel-spaced-key  "}

	models, err := svc.GetRequestTraceModelCandidates(context.Background(), channelKeys)
	require.NoError(t, err)

	require.NotEmpty(t, defaults)
	require.Contains(t, models, defaults[0], "platform defaults stay in the directory")
	require.Contains(t, models, "Zed-Source-Alpha", "allowlist entry is trimmed and keeps the first casing")
	require.Contains(t, models, "zed-route-exact")
	require.Contains(t, models, "acct-source-key")
	require.Contains(t, models, "channel-source-key")
	require.Contains(t, models, "channel-spaced-key")
	require.Contains(t, models, "route-public-model")

	require.NotContains(t, models, "upstream-target-xyz", "mapping targets are never candidates")
	require.NotContains(t, models, "zed-glob-*", "glob allowlist entries never become an exact model")
	require.NotContains(t, models, "zed-route-glob-*", "glob routing keys never become an exact model")
	require.NotContains(t, models, "channel-glob-*")
	require.NotContains(t, models, "route-glob-*")
	require.NotContains(t, models, tooLong, "an over-long model is skipped")
	require.NotContains(t, models, tooLong[:requestTraceModelCandidateMaxLength], "an over-long model is never truncated into a wrong name")

	require.Equal(t, 1, countFold(models, "zed-source-alpha"), "case-insensitive dedup keeps exactly one entry")

	sorted := append([]string(nil), models...)
	sort.Strings(sorted)
	require.Equal(t, sorted, models, "candidates are sorted")
	require.Positive(t, routeRepo.calls, "the enabled composite route directory is read")
}

func TestAdminServiceImplGetRequestTraceModelCandidatesPropagatesReadErrors(t *testing.T) {
	groupErr := errors.New("groups down")
	svc := newTraceCandidateAdminService(&groupRepoStubForAdmin{listWithFiltersErr: groupErr}, nil, nil)
	_, err := svc.GetRequestTraceModelCandidates(context.Background(), nil)
	require.ErrorIs(t, err, groupErr)

	accountErr := errors.New("accounts down")
	svc = newTraceCandidateAdminService(&groupRepoStubForAdmin{}, &traceCandidateAccountRepoStub{err: accountErr}, nil)
	_, err = svc.GetRequestTraceModelCandidates(context.Background(), nil)
	require.ErrorIs(t, err, accountErr)

	routeErr := errors.New("routes down")
	svc = newTraceCandidateAdminService(&groupRepoStubForAdmin{}, nil, &traceCandidateCompositeRouteRepoStub{err: routeErr})
	_, err = svc.GetRequestTraceModelCandidates(context.Background(), nil)
	require.ErrorIs(t, err, routeErr)
}

func TestAdminServiceImplGetRequestTraceModelCandidatesPrefersSourceOnlyAccountReader(t *testing.T) {
	accountRepo := &traceCandidateAccountSourceKeyRepoStub{keys: []string{"acct-narrow-key", "acct-dropped-*"}}
	svc := newTraceCandidateAdminService(&groupRepoStubForAdmin{}, accountRepo, nil)

	models, err := svc.GetRequestTraceModelCandidates(context.Background(), nil)
	require.NoError(t, err)
	require.Contains(t, models, "acct-narrow-key")
	require.NotContains(t, models, "acct-dropped-*", "the shared filters still drop globs from the narrow reader")
	require.Equal(t, 1, accountRepo.sourceKeyCalls)
	require.Zero(t, accountRepo.listAllCalls, "the credential-bearing full account list is never loaded")
}

func TestAdminServiceImplGetRequestTraceModelCandidatesToleratesMissingOptionalRepos(t *testing.T) {
	svc := &adminServiceImpl{}
	models, err := svc.GetRequestTraceModelCandidates(context.Background(), nil)
	require.NoError(t, err)
	require.Contains(t, models, compositeDefaultModelsListCandidateIDs()[0])

	// A repository that only implements ListByGroup must not fail the aggregation;
	// the global read is an optional, asserted capability.
	svc = &adminServiceImpl{compositeRouteRepo: compositeRouteRepoStub{}}
	models, err = svc.GetRequestTraceModelCandidates(context.Background(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, models)
}
