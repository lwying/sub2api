//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release        *GitHubRelease
	latestErr      error
	recentReleases []*GitHubRelease
	recentErr      error

	// downloadBody is what a download call writes to its destination. When it is
	// nil the stub reports an error instead, so a test that reaches a download it
	// did not intend fails loudly rather than silently succeeding.
	downloadBody []byte
	checksumData []byte
	checksumErr  error

	mu             sync.Mutex
	downloadCalls  []string
	checksumCalls  []string
	latestRequests []string
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.mu.Lock()
	s.latestRequests = append(s.latestRequests, repo)
	s.mu.Unlock()
	if s.latestErr != nil {
		return nil, s.latestErr
	}
	return s.release, nil
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(_ context.Context, url, dest string, _ int64) error {
	s.mu.Lock()
	s.downloadCalls = append(s.downloadCalls, url)
	s.mu.Unlock()
	if s.downloadBody == nil {
		return errors.New("unexpected download: " + url)
	}
	return os.WriteFile(dest, s.downloadBody, 0o644)
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(_ context.Context, url string, _ int64) ([]byte, error) {
	s.mu.Lock()
	s.checksumCalls = append(s.checksumCalls, url)
	s.mu.Unlock()
	if s.checksumErr != nil {
		return nil, s.checksumErr
	}
	return s.checksumData, nil
}

func (s *updateServiceGitHubClientStub) downloadedURLs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.downloadCalls...)
}

func (s *updateServiceGitHubClientStub) fetchedRepos() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.latestRequests...)
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{
			release: &GitHubRelease{
				TagName: "v0.1.132",
				Name:    "v0.1.132",
			},
		},
		"0.1.132",
		"release",
		DeploymentTypeNative,
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		current,
		"release",
		DeploymentTypeNative,
	)
}

// versionStrings renders a rollback list as plain version strings.
func versionStrings(versions []RollbackVersion) []string {
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		out = append(out, v.Version)
	}
	return out
}

// rollbackTestRelease builds a release that is installable on the host running
// the suite, which is what a rollback candidate must now be.
func rollbackTestRelease(t *testing.T, tag string) *GitHubRelease {
	t.Helper()
	updateTestRequireSupportedPlatform(t)
	return updateTestForkRelease(tag, updateTestRunningArchive(strings.TrimPrefix(tag, "v")), checksumsAssetName)
}

const rollbackTestCurrentVersion = "0.1.147"

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		rollbackTestRelease(t, "v0.1.148"),                                               // newer than current: excluded
		rollbackTestRelease(t, "v0.1.147"),                                               // current: excluded
		{TagName: "v0.1.146-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		rollbackTestRelease(t, "v0.1.146"),
		{TagName: "v0.1.145", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		rollbackTestRelease(t, "v0.1.144"),
		rollbackTestRelease(t, "v0.1.144"), // duplicate: excluded
		rollbackTestRelease(t, "v0.1.143"),
		rollbackTestRelease(t, "v0.1.142"), // beyond cap of 3: excluded
	}
	svc := newRollbackTestService(rollbackTestCurrentVersion, releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Equal(t, []string{"0.1.146", "0.1.144", "0.1.143"}, versionStrings(versions))
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		rollbackTestRelease(t, "v0.1.144"),
		rollbackTestRelease(t, "v0.1.146"),
		rollbackTestRelease(t, "v0.1.145"),
	}
	svc := newRollbackTestService(rollbackTestCurrentVersion, releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Equal(t, []string{"0.1.146", "0.1.145", "0.1.144"}, versionStrings(versions))
}

func TestUpdateServiceListRollbackVersionsSkipsNonInstallableReleases(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	releases := []*GitHubRelease{
		updateTestForkRelease("v0.1.146", updateTestRunningArchive("0.1.146"), checksumsAssetName),
		updateTestForkRelease("v0.1.145", updateTestOtherPlatformArchive("0.1.145"), checksumsAssetName), // image-only for this host
		updateTestForkRelease("v0.1.144", updateTestRunningArchive("0.1.144")),                           // no checksums
		{TagName: "v0.1.143"},     // no assets at all
		{TagName: "v0.1.142-ish"}, // not a fork version
	}
	svc := newRollbackTestService(rollbackTestCurrentVersion, releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Equal(t, []string{"0.1.146"}, versionStrings(versions),
		"only a release this host can actually install may be offered as a rollback candidate")

	// The install path refuses the very same releases, so listing and installing
	// agree on what is a candidate.
	for _, target := range []string{"0.1.145", "0.1.144", "0.1.143"} {
		require.ErrorIs(t, svc.RollbackToVersion(context.Background(), target), ErrRollbackVersionNotAllowed,
			"target %q is not an installable candidate", target)
	}
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.148"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		"0.1.147",
		"release",
		DeploymentTypeNative,
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148"},
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
		{TagName: "v0.1.144"},
		{TagName: "v0.1.143"},
		{TagName: "v0.1.142"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	for _, target := range []string{
		"",         // empty
		"0.1.147",  // current version
		"v0.1.147", // current version with prefix
		"0.1.148",  // newer than current
		"0.1.142",  // older than the 3 most recent
		"9.9.9",    // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	releases := []*GitHubRelease{
		rollbackTestRelease(t, "v0.1.147"),
		rollbackTestRelease(t, "v0.1.146"),
	}
	svc := newRollbackTestService(rollbackTestCurrentVersion, releases)

	err := svc.RollbackToVersion(context.Background(), "v0.1.146")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	// The target cleared the version and asset gates and only failed at the stub
	// download, which proves the v-prefixed version itself was accepted.
	require.Contains(t, err.Error(), "download failed")
}

func TestUpdateServiceCheckUpdateRefusesDraftOrPrereleaseRelease(t *testing.T) {
	updateTestRequireSupportedPlatform(t)

	tests := []struct {
		name       string
		draft      bool
		prerelease bool
		wantTip    string
	}{
		{name: "draft", draft: true, wantTip: "draft"},
		{name: "prerelease", prerelease: true, wantTip: "prerelease"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := updateTestForkRelease("v9.9.9", updateTestRunningArchive("9.9.9"), checksumsAssetName)
			release.Draft = tt.draft
			release.Prerelease = tt.prerelease

			client := &updateServiceGitHubClientStub{release: release}
			svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

			info, err := svc.CheckUpdate(context.Background(), false)

			require.NoError(t, err)
			require.False(t, info.HasUpdate, "a %s release with a valid tag and assets must not be offered", tt.name)
			require.Contains(t, info.Warning, tt.wantTip, "the warning should name the reason")

			require.ErrorIs(t, svc.PerformUpdate(context.Background()), ErrNoUpdateAvailable)
			require.Empty(t, client.downloadedURLs(), "a %s release must not be downloaded", tt.name)
		})
	}
}

// updateTestForkCacheEntry is a cache payload as this updater writes it, with the
// release flags it observed. Recorded flags are how a cached payload proves the
// release was actually seen as stable.
func updateTestForkCacheEntry(t *testing.T, tag string, version string, recorded bool, draft bool, prerelease bool) string {
	t.Helper()
	entry := map[string]any{
		"repo":                   forkRepo,
		"release_tag":            tag,
		"release_flags_recorded": recorded,
		"release_draft":          draft,
		"release_prerelease":     prerelease,
		"release_info": map[string]any{
			"name":     tag,
			"tag_name": tag,
			"html_url": "https://github.com/lwying/sub2api/releases/tag/" + tag,
			"assets":   forkReleaseAssetsJSON(t, version),
		},
		"timestamp": time.Now().Unix(),
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	return string(data)
}

func TestUpdateServiceCheckUpdateRefusesCacheWithoutReleaseFlags(t *testing.T) {
	updateTestRequireSupportedPlatform(t)

	tests := []struct {
		name    string
		payload string
		wantTip string
		// wantVersionWithheld is true when the payload is so untrustworthy that its
		// version must not be surfaced as the latest fork version either.
		wantVersionWithheld bool
	}{
		{
			name: "flags never recorded",
			payload: func() string {
				entry := map[string]any{
					"repo":        forkRepo,
					"release_tag": "v9.9.9",
					"release_info": map[string]any{
						"name":     "v9.9.9",
						"tag_name": "v9.9.9",
						"assets":   nil,
					},
					"timestamp": time.Now().Unix(),
				}
				data, err := json.Marshal(entry)
				require.NoError(t, err)
				return string(data)
			}(),
			// The surfaced warning is the failed fork query, not the cache refusal:
			// the refusal is proven by the withheld version and release info below.
			// The refusal reason itself is pinned in the assessment unit test.
			wantVersionWithheld: true,
		},
		{
			name:    "recorded draft",
			payload: updateTestForkCacheEntry(t, "v9.9.9", "9.9.9", true, true, false),
			wantTip: "draft",
		},
		{
			name:    "recorded prerelease",
			payload: updateTestForkCacheEntry(t, "v9.9.9", "9.9.9", true, false, true),
			wantTip: "prerelease",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The fork query fails, so the only thing that could answer the check is
			// the cached payload under scrutiny.
			client := &updateServiceGitHubClientStub{latestErr: errors.New("fork API unavailable")}
			svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{data: tt.payload}, client)

			info, err := svc.CheckUpdate(context.Background(), false)

			require.NoError(t, err)
			require.False(t, info.HasUpdate, "an unverifiable or unstable cache payload must not become an update")
			require.NotEmpty(t, info.Warning)
			if tt.wantTip != "" {
				require.Contains(t, info.Warning, tt.wantTip)
			}
			if tt.wantVersionWithheld {
				require.NotEqual(t, "9.9.9", info.LatestVersion,
					"a payload whose release state cannot be vouched for must not surface a version either")
				require.Nil(t, info.ReleaseInfo)
			}
			require.Empty(t, client.downloadedURLs())
		})
	}
}

func TestUpdateServiceCheckUpdateUsesCacheWithRecordedStableFlags(t *testing.T) {
	updateTestRequireSupportedPlatform(t)

	// The control case for the rule above: when the payload records that the
	// release was observed as stable, a failed fork query may fall back to it.
	client := &updateServiceGitHubClientStub{latestErr: errors.New("fork API unavailable")}
	svc := newForkUpdateService(t, "0.2.6",
		&updateServiceCacheStub{data: updateTestForkCacheEntry(t, "v9.9.9", "9.9.9", true, false, false)}, client)

	info, err := svc.CheckUpdate(context.Background(), true)

	require.NoError(t, err)
	require.True(t, info.Cached)
	require.True(t, info.HasUpdate)
	require.Equal(t, "9.9.9", info.LatestVersion)
}

// --- Checksum parsing -------------------------------------------------------
//
// The checksums file decides whether an archive is installed, so its parsing is
// pinned here: an ambiguous or malformed file must fail closed instead of
// resolving to whichever line happened to come first.

func TestFindChecksumForFile(t *testing.T) {
	const fileName = "sub2api_0.3.0_linux_amd64.tar.gz"
	digest := strings.Repeat("a1", 32)

	tests := []struct {
		name    string
		content string
		want    string
		wantErr string
	}{
		{name: "single entry", content: digest + "  " + fileName + "\n", want: digest},
		{name: "uppercase digest is normalised", content: strings.ToUpper(digest) + "  " + fileName + "\n", want: digest},
		{name: "binary mode marker", content: digest + " *" + fileName + "\n", want: digest},
		{name: "blank lines before the entry", content: "\n\n" + digest + "  " + fileName + "\n", want: digest},
		{name: "other assets listed too", content: strings.Repeat("b2", 32) + "  other.zip\n" + digest + "  " + fileName + "\n", want: digest},
		{name: "missing entry", content: strings.Repeat("b2", 32) + "  other.zip\n", wantErr: "checksum not found"},
		{name: "empty file", content: "", wantErr: "checksum not found"},
		{name: "short digest", content: "abc123  " + fileName + "\n", wantErr: "does not hold a SHA256 digest"},
		{name: "non hex digest", content: strings.Repeat("zz", 32) + "  " + fileName + "\n", wantErr: "does not hold a SHA256 digest"},
		{name: "duplicate entry", content: digest + "  " + fileName + "\n" + strings.Repeat("c3", 32) + "  " + fileName + "\n", wantErr: "more than once"},
		{name: "line with one field", content: digest + "\n", wantErr: "malformed checksums line"},
		{name: "line with three fields", content: digest + "  " + fileName + "  extra\n", wantErr: "malformed checksums line"},
		{name: "malformed line elsewhere in the file", content: digest + "  " + fileName + "\nnot-a-checksum-line\n", wantErr: "malformed checksums line"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findChecksumForFile([]byte(tt.content), fileName)

			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestFindChecksumForFileRejectsDuplicateOfTheTargetFile(t *testing.T) {
	// The first entry matches the real archive; a second, different digest for
	// the same name must make the whole file untrustworthy rather than let a
	// bogus archive through on the strength of the first line.
	const fileName = "sub2api_0.3.0_linux_amd64.tar.gz"
	content := strings.Repeat("a1", 32) + "  " + fileName + "\n" + strings.Repeat("00", 32) + "  " + fileName + "\n"

	_, err := findChecksumForFile([]byte(content), fileName)

	require.Error(t, err)
	require.Contains(t, err.Error(), "more than once")
}

// --- Fork update source -----------------------------------------------------
//
// The updater checks and installs only releases published by lwying/sub2api.
// These tests drive the two outward entry points (CheckUpdate / PerformUpdate)
// through a fake GitHub release client, so the assertions are about what an
// administrator sees and what the updater is willing to download.

// updateTestArchiveName mirrors .goreleaser.yaml: sub2api_<version>_<os>_<arch>,
// .zip on windows and .tar.gz elsewhere.
func updateTestArchiveName(version, osName, arch string) string {
	ext := ".tar.gz"
	if osName == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("sub2api_%s_%s_%s%s", version, osName, arch, ext)
}

// updateTestPlatforms is the release matrix in .goreleaser.yaml.
var updateTestPlatforms = []struct{ os, arch string }{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
}

func updateTestRunningArchive(version string) string {
	return updateTestArchiveName(version, runtime.GOOS, runtime.GOARCH)
}

func updateTestOtherPlatformArchive(version string) string {
	for _, p := range updateTestPlatforms {
		if p.os != runtime.GOOS || p.arch != runtime.GOARCH {
			return updateTestArchiveName(version, p.os, p.arch)
		}
	}
	return ""
}

// updateTestRequireSupportedPlatform skips a test when the host is outside the
// fork's release platform matrix, where no platform archive is expected to
// exist and nothing can be installable.
func updateTestRequireSupportedPlatform(t *testing.T) {
	t.Helper()
	for _, p := range updateTestPlatforms {
		if p.os == runtime.GOOS && p.arch == runtime.GOARCH {
			return
		}
	}
	t.Skipf("%s/%s is outside the fork release platform matrix", runtime.GOOS, runtime.GOARCH)
}

func updateTestForkAssetURL(tag, name string) string {
	return "https://github.com/lwying/sub2api/releases/download/" + tag + "/" + name
}

func updateTestForkRelease(tag string, assetNames ...string) *GitHubRelease {
	release := &GitHubRelease{
		TagName: tag,
		Name:    tag,
		HTMLURL: "https://github.com/lwying/sub2api/releases/tag/" + tag,
	}
	for _, name := range assetNames {
		release.Assets = append(release.Assets, GitHubAsset{
			Name:               name,
			BrowserDownloadURL: updateTestForkAssetURL(tag, name),
			Size:               1024,
		})
	}
	return release
}

// updateTestUpstreamCacheEntry is a legacy cache payload written while the
// updater still read Wei-Shaw/sub2api: no repo binding, upstream asset URLs.
func updateTestUpstreamCacheEntry(t *testing.T, latest string) string {
	t.Helper()
	remote := "github.com/Wei-Shaw/sub2api/releases/download/v" + latest
	entry := map[string]any{
		"latest": latest,
		"release_info": map[string]any{
			"name":         "v" + latest,
			"html_url":     "https://github.com/Wei-Shaw/sub2api/releases/tag/v" + latest,
			"published_at": "2026-09-01T00:00:00Z",
			"assets": []map[string]any{
				{
					"name":         updateTestRunningArchive(latest),
					"download_url": "https://" + remote + "/" + updateTestRunningArchive(latest),
					"size":         1024,
				},
				{
					"name":         "checksums.txt",
					"download_url": "https://" + remote + "/checksums.txt",
					"size":         100,
				},
			},
		},
		"timestamp": time.Now().Unix(),
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	return string(data)
}

func newForkUpdateService(t *testing.T, current string, cache *updateServiceCacheStub, client *updateServiceGitHubClientStub) *UpdateService {
	t.Helper()
	return NewUpdateService(cache, client, current, "release", DeploymentTypeNative)
}

func TestUpdateServiceCheckUpdateIgnoresLegacyUpstreamCache(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	cache := &updateServiceCacheStub{data: updateTestUpstreamCacheEntry(t, "9.9.9")}
	client := &updateServiceGitHubClientStub{
		release: updateTestForkRelease("v0.2.7", updateTestRunningArchive("0.2.7"), checksumsAssetName),
	}
	svc := newForkUpdateService(t, "0.2.6", cache, client)

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.Equal(t, []string{forkRepo}, client.fetchedRepos(), "the fork must be queried instead of trusting the legacy cache")
	require.NotEqual(t, "9.9.9", info.LatestVersion, "an upstream cache entry must never surface as the latest version")
	require.Equal(t, "0.2.7", info.LatestVersion)
	require.True(t, info.HasUpdate)
	require.False(t, info.Cached)
}

func TestUpdateServiceCheckUpdateRejectsCacheBoundToAnotherRepo(t *testing.T) {
	entry := map[string]any{
		"repo":         "Wei-Shaw/sub2api",
		"release_tag":  "v9.9.9",
		"release_info": map[string]any{"name": "v9.9.9"},
		"timestamp":    time.Now().Unix(),
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)

	client := &updateServiceGitHubClientStub{latestErr: errors.New("fork API unavailable")}
	svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{data: string(data)}, client)

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.False(t, info.HasUpdate)
	require.NotEqual(t, "9.9.9", info.LatestVersion)
	require.Empty(t, client.downloadedURLs())
}

func TestUpdateServiceCheckUpdateFailsSafeWhenForkQueryFails(t *testing.T) {
	cache := &updateServiceCacheStub{data: updateTestUpstreamCacheEntry(t, "9.9.9")}
	client := &updateServiceGitHubClientStub{latestErr: errors.New("fork API unavailable")}
	svc := newForkUpdateService(t, "0.2.6", cache, client)

	for _, force := range []bool{false, true} {
		info, err := svc.CheckUpdate(context.Background(), force)

		require.NoError(t, err, "force=%v", force)
		require.False(t, info.HasUpdate, "force=%v: upstream assets must not become the fork update", force)
		require.NotEqual(t, "9.9.9", info.LatestVersion, "force=%v", force)
		require.NotEmpty(t, info.Warning, "force=%v: the failed fork check must be visible", force)
		require.Empty(t, client.downloadedURLs(), "force=%v", force)
	}
}

func TestUpdateServiceCheckUpdateFallsBackToForkCacheOnQueryFailure(t *testing.T) {
	updateTestRequireSupportedPlatform(t)

	// A cache entry written by the fork updater itself, release flags included, is
	// legitimate fallback material; the upstream-era entry is not.
	client := &updateServiceGitHubClientStub{latestErr: errors.New("fork API unavailable")}
	svc := newForkUpdateService(t, "0.2.6",
		&updateServiceCacheStub{data: updateTestForkCacheEntry(t, "v0.2.7", "0.2.7", true, false, false)}, client)

	info, checkErr := svc.CheckUpdate(context.Background(), true)

	require.NoError(t, checkErr)
	require.True(t, info.Cached)
	require.True(t, info.HasUpdate)
	require.Equal(t, "0.2.7", info.LatestVersion)
}

func forkReleaseAssetsJSON(t *testing.T, version string) []map[string]any {
	t.Helper()
	archive := updateTestRunningArchive(version)
	return []map[string]any{
		{
			"name":         archive,
			"download_url": updateTestForkAssetURL("v"+version, archive),
			"size":         1024,
		},
		{
			"name":         checksumsAssetName,
			"download_url": updateTestForkAssetURL("v"+version, checksumsAssetName),
			"size":         100,
		},
	}
}

func TestUpdateServiceCheckUpdateRequiresPlatformAssetAndChecksums(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	tests := []struct {
		name    string
		assets  []string
		wantTip string
	}{
		{
			name:    "no archive for the running platform",
			assets:  []string{updateTestOtherPlatformArchive("9.9.9"), checksumsAssetName},
			wantTip: "no installable asset",
		},
		{
			name:    "no checksums file",
			assets:  []string{updateTestRunningArchive("9.9.9")},
			wantTip: "checksums.txt",
		},
		{
			name:    "release carries no binary assets at all",
			assets:  nil,
			wantTip: "no installable asset",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &updateServiceGitHubClientStub{release: updateTestForkRelease("v9.9.9", tt.assets...)}
			svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

			info, err := svc.CheckUpdate(context.Background(), false)

			require.NoError(t, err)
			require.False(t, info.HasUpdate, "an uninstallable fork release must not be offered as a one-click update")
			require.Contains(t, info.Warning, tt.wantTip, "the warning should explain why the release is not installable")

			updateErr := svc.PerformUpdate(context.Background())
			require.ErrorIs(t, updateErr, ErrNoUpdateAvailable)
			require.Empty(t, client.downloadedURLs(), "an uninstallable release must not be downloaded")
		})
	}
}

func TestUpdateServiceCheckUpdateOffersInstallableForkRelease(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	client := &updateServiceGitHubClientStub{
		release: updateTestForkRelease("v9.9.9", updateTestRunningArchive("9.9.9"), checksumsAssetName),
	}
	cache := &updateServiceCacheStub{}
	svc := newForkUpdateService(t, "0.2.6", cache, client)

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.True(t, info.HasUpdate)
	require.Equal(t, "9.9.9", info.LatestVersion)
	require.Equal(t, "0.2.6", info.CurrentVersion)
	require.Equal(t, forkRepo, client.fetchedRepos()[0])
	require.NotEmpty(t, cache.data, "the check should be cached")

	// Same release served from the fork cache must agree.
	cached, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.True(t, cached.Cached)
	require.True(t, cached.HasUpdate)
	require.Equal(t, "9.9.9", cached.LatestVersion)
}

func TestUpdateServiceCheckUpdateRejectsMalformedForkTags(t *testing.T) {
	for _, tag := range []string{
		"0.3.0",          // missing v prefix
		"v0.3",           // two segments
		"v0.3.0.1",       // four segments
		"v0.3.0-rc.1",    // prerelease suffix
		"v0.3.0+build",   // build metadata
		"v03.0.0",        // leading zero
		"release-v0.3.0", // not a tag at all
	} {
		t.Run(tag, func(t *testing.T) {
			client := &updateServiceGitHubClientStub{
				release: updateTestForkRelease(tag, updateTestRunningArchive("0.3.0"), checksumsAssetName),
			}
			svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

			info, err := svc.CheckUpdate(context.Background(), false)

			require.NoError(t, err)
			require.False(t, info.HasUpdate, "a tag outside vX.Y.Z must not become an update")
			require.NotEqual(t, "0.3.0", info.LatestVersion)
			require.NotEqual(t, tag, info.LatestVersion)
			require.NotEmpty(t, info.Warning)
		})
	}
}

func TestUpdateServiceCheckUpdateIgnoresUpstreamNewerRelease(t *testing.T) {
	// Upstream released a newer version, the fork has not: the fork client
	// returns the fork's own (older) release, so nothing is offered.
	client := &updateServiceGitHubClientStub{release: updateTestForkRelease("v0.2.6", updateTestRunningArchive("0.2.6"), checksumsAssetName)}
	svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.False(t, info.HasUpdate)
	require.Equal(t, "0.2.6", info.LatestVersion)
}

func TestUpdateServicePerformUpdateRefusesForeignAssetURLs(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	archive := updateTestRunningArchive("9.9.9")
	release := updateTestForkRelease("v9.9.9", archive, checksumsAssetName)
	upstream := "https://github.com/Wei-Shaw/sub2api/releases/download/v9.9.9/"
	release.Assets[0].BrowserDownloadURL = upstream + archive
	release.Assets[1].BrowserDownloadURL = upstream + checksumsAssetName

	client := &updateServiceGitHubClientStub{release: release}
	svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.Empty(t, client.downloadedURLs(), "an asset URL that is not bound to the fork release must not be downloaded")
	require.NotErrorIs(t, err, ErrNoUpdateAvailable)
}

func TestUpdateServicePerformUpdateRejectsAssetHostOutsideGitHub(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	archive := updateTestRunningArchive("9.9.9")
	release := updateTestForkRelease("v9.9.9", archive, checksumsAssetName)
	release.Assets[0].BrowserDownloadURL = "https://objects.githubusercontent.com/lwying/sub2api/releases/download/v9.9.9/" + archive
	release.Assets[1].BrowserDownloadURL = "http://github.com/lwying/sub2api/releases/download/v9.9.9/" + checksumsAssetName

	client := &updateServiceGitHubClientStub{release: release}
	svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.Empty(t, client.downloadedURLs(), "only HTTPS URLs on the fork's own release path may be downloaded")
	require.NotErrorIs(t, err, ErrNoUpdateAvailable)
}

func TestReplaceUpdateBinaryPreservesPriorBackupWhenCurrentMoveFails(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	priorBackup := current + ".backup"
	incoming := filepath.Join(dir, "next")
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(priorBackup, []byte("previous-working"), 0o755))
	require.NoError(t, os.WriteFile(incoming, []byte("verified-next"), 0o755))
	moveFailed := errors.New("current executable is locked")
	move := func(src, dst string) error {
		if src == current && dst == priorBackup {
			return moveFailed
		}
		return os.Rename(src, dst)
	}

	err := replaceUpdateBinary(current, incoming, move)

	require.ErrorIs(t, err, moveFailed)
	body, readErr := os.ReadFile(priorBackup)
	require.NoError(t, readErr)
	require.Equal(t, "previous-working", string(body))
	body, readErr = os.ReadFile(current)
	require.NoError(t, readErr)
	require.Equal(t, "current", string(body))
}

func TestReplaceUpdateBinaryRestoresPriorBackupWhenNewMoveFails(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	priorBackup := current + ".backup"
	incoming := filepath.Join(dir, "next")
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(priorBackup, []byte("previous-working"), 0o755))
	require.NoError(t, os.WriteFile(incoming, []byte("verified-next"), 0o755))
	moveFailed := errors.New("cannot install next")
	move := func(src, dst string) error {
		if src == incoming && dst == current {
			return moveFailed
		}
		return os.Rename(src, dst)
	}

	err := replaceUpdateBinary(current, incoming, move)

	require.ErrorIs(t, err, moveFailed)
	body, readErr := os.ReadFile(current)
	require.NoError(t, readErr)
	require.Equal(t, "current", string(body))
	body, readErr = os.ReadFile(priorBackup)
	require.NoError(t, readErr)
	require.Equal(t, "previous-working", string(body))
}

func TestReplaceUpdateBinaryKeepsCurrentAsRollbackOnSuccess(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	priorBackup := current + ".backup"
	incoming := filepath.Join(dir, "next")
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(priorBackup, []byte("previous-working"), 0o755))
	require.NoError(t, os.WriteFile(incoming, []byte("verified-next"), 0o755))

	require.NoError(t, replaceUpdateBinary(current, incoming, os.Rename))

	body, readErr := os.ReadFile(current)
	require.NoError(t, readErr)
	require.Equal(t, "verified-next", string(body))
	body, readErr = os.ReadFile(priorBackup)
	require.NoError(t, readErr)
	require.Equal(t, "current", string(body))
	_, err := os.Stat(priorBackup + ".previous")
	require.True(t, os.IsNotExist(err))
}

func TestReplaceUpdateBinaryDoesNotDeleteUnownedPreviousFile(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	incoming := filepath.Join(dir, "next")
	priorPrevious := current + ".backup.previous"
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(incoming, []byte("verified-next"), 0o755))
	require.NoError(t, os.WriteFile(priorPrevious, []byte("unowned-previous"), 0o755))

	err := replaceUpdateBinary(current, incoming, os.Rename)
	require.Error(t, err, "an unexpected preexisting file must block before any replacement")

	body, err := os.ReadFile(priorPrevious)
	require.NoError(t, err)
	require.Equal(t, "unowned-previous", string(body))
	body, err = os.ReadFile(current)
	require.NoError(t, err)
	require.Equal(t, "current", string(body))
}

func TestUpdateServicePerformUpdateVerifiesChecksumBeforeReplacingBinary(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	archive := updateTestRunningArchive("9.9.9")
	release := updateTestForkRelease("v9.9.9", archive, checksumsAssetName)
	client := &updateServiceGitHubClientStub{
		release:      release,
		downloadBody: []byte("not the archive the checksums file describes"),
		checksumData: []byte("0000000000000000000000000000000000000000000000000000000000000000  " + archive + "\n"),
	}
	svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	exePath, err := os.Executable()
	require.NoError(t, err)
	backupPath := exePath + ".backup"
	backupBefore, backupErr := os.Stat(backupPath)
	require.True(t, backupErr == nil || os.IsNotExist(backupErr))

	updateErr := svc.PerformUpdate(context.Background())

	require.Error(t, updateErr)
	require.Contains(t, updateErr.Error(), "checksum")
	require.Len(t, client.downloadedURLs(), 1)
	backupAfter, statErr := os.Stat(backupPath)
	if os.IsNotExist(backupErr) {
		require.True(t, os.IsNotExist(statErr), "a failed verification must not create a backup")
	} else {
		require.NoError(t, statErr, "a failed verification must preserve an existing backup")
		require.True(t, os.SameFile(backupBefore, backupAfter))
	}
	_, exeErr := os.Stat(exePath)
	require.NoError(t, exeErr, "the running executable must be left in place")
}

// newDockerUpdateService builds an updater whose executable starts in an image.
// In-app updates affect this container's writable layer, not that image.
func newDockerUpdateService(
	t *testing.T,
	current string,
	cache *updateServiceCacheStub,
	client *updateServiceGitHubClientStub,
) *UpdateService {
	t.Helper()
	return NewUpdateService(cache, client, current, "release", DeploymentTypeDocker)
}

func installableDockerCheckupRelease(t *testing.T) *GitHubRelease {
	t.Helper()
	return updateTestForkRelease("v0.2.7", updateTestRunningArchive("0.2.7"), checksumsAssetName)
}

// moveRecorder records the order of file moves (and can inject failures) so a
// test can prove a swap never renames onto the running executable's path first.
type moveRecorder struct {
	moves [][2]string
	fail  func(src, dst string) error
}

func (r *moveRecorder) move(src, dst string) error {
	r.moves = append(r.moves, [2]string{src, dst})
	if r.fail != nil {
		if err := r.fail(src, dst); err != nil {
			return err
		}
	}
	return os.Rename(src, dst)
}

func (r *moveRecorder) indexOf(src, dst string) int {
	for i, m := range r.moves {
		if m[0] == src && m[1] == dst {
			return i
		}
	}
	return -1
}

func TestUpdateServiceDockerDeploymentRefusesRollbackToVersion(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	client := &updateServiceGitHubClientStub{
		recentReleases: []*GitHubRelease{rollbackTestRelease(t, "v0.1.146")},
		downloadBody:   []byte("archive"),
	}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, rollbackTestCurrentVersion, "release", DeploymentTypeDocker)

	err := svc.RollbackToVersion(context.Background(), "0.1.146")

	require.ErrorIs(t, err, ErrBinaryUpdateUnsupported)
	require.Empty(t, client.downloadedURLs(),
		"rolling back a container deployment is an image change, not a download")
}

func TestUpdateServiceDockerDeploymentRefusesBackupRollback(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		"0.2.6",
		"release",
		DeploymentTypeDocker,
	)

	err := svc.Rollback()

	require.ErrorIs(t, err, ErrBinaryUpdateUnsupported,
		"a container-owned binary must not be swapped for a local .backup")
}

func TestUpdateServiceDockerDeploymentReportsWritableLayerUpdateCapability(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	client := &updateServiceGitHubClientStub{release: installableDockerCheckupRelease(t)}
	svc := newDockerUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.True(t, info.HasUpdate, "a newer fork release is still news worth showing the administrator")
	require.Equal(t, "0.2.7", info.LatestVersion)
	require.Equal(t, DeploymentTypeDocker, info.DeploymentType)
	require.True(t, info.BinaryUpdateSupported, "the current container permits a temporary writable-layer update")
	require.Equal(t, "release", info.BuildType, "build_type keeps describing how the binary was built")
}

func TestUpdateServiceDockerDeploymentReportsCapabilityWithoutARelease(t *testing.T) {
	client := &updateServiceGitHubClientStub{latestErr: errors.New("fork API unavailable")}
	svc := newDockerUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.False(t, info.HasUpdate)
	require.Equal(t, DeploymentTypeDocker, info.DeploymentType)
	require.True(t, info.BinaryUpdateSupported, "capability is distinct from availability of a release")
}

// The candidate list stays readable in a container deployment: the frontend uses
// it to name the fork release an operator should pin as an image tag.
func TestUpdateServiceDockerDeploymentStillListsRollbackVersions(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	client := &updateServiceGitHubClientStub{recentReleases: []*GitHubRelease{
		rollbackTestRelease(t, "v0.1.146"),
		rollbackTestRelease(t, "v0.1.145"),
	}}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, rollbackTestCurrentVersion, "release", DeploymentTypeDocker)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Equal(t, []string{"0.1.146", "0.1.145"}, versionStrings(versions))
}

func TestNewUpdateServiceTreatsOnlyDockerMarkerAsContainerDeployment(t *testing.T) {
	updateTestRequireSupportedPlatform(t)

	for _, marker := range []string{DeploymentTypeDocker, "DOCKER", " docker "} {
		svc := NewUpdateService(&updateServiceCacheStub{},
			&updateServiceGitHubClientStub{release: installableDockerCheckupRelease(t)},
			"0.2.6", "release", marker)
		info, err := svc.CheckUpdate(context.Background(), false)
		require.NoError(t, err)
		require.Equal(t, DeploymentTypeDocker, info.DeploymentType, "marker %q", marker)
		require.True(t, info.BinaryUpdateSupported, "marker %q", marker)
	}

	// Anything else — including the empty marker every native and GoReleaser
	// build carries — must keep the in-app updater available.
	for _, marker := range []string{"", " ", "native", "linux", "unknown"} {
		svc := NewUpdateService(&updateServiceCacheStub{},
			&updateServiceGitHubClientStub{release: installableDockerCheckupRelease(t)},
			"0.2.6", "release", marker)
		info, err := svc.CheckUpdate(context.Background(), false)
		require.NoError(t, err)
		require.Equal(t, DeploymentTypeNative, info.DeploymentType, "marker %q", marker)
		require.True(t, info.BinaryUpdateSupported, "marker %q", marker)
	}
}

func TestUpdateServiceNativeDeploymentStillOffersBinaryUpdate(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	client := &updateServiceGitHubClientStub{release: installableDockerCheckupRelease(t)}
	svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	info, err := svc.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.True(t, info.HasUpdate)
	require.Equal(t, DeploymentTypeNative, info.DeploymentType)
	require.True(t, info.BinaryUpdateSupported)
}

// A native archive build must still reach the install path: the container guard
// must not become a blanket refusal of the existing updater.
func TestUpdateServiceNativeDeploymentStillReachesTheInstallPath(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	archive := updateTestRunningArchive("0.2.7")
	client := &updateServiceGitHubClientStub{
		release:      updateTestForkRelease("v0.2.7", archive, checksumsAssetName),
		downloadBody: []byte("not the archive the checksums file describes"),
		checksumData: []byte("0000000000000000000000000000000000000000000000000000000000000000  " + archive + "\n"),
	}
	svc := newForkUpdateService(t, "0.2.6", &updateServiceCacheStub{}, client)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrBinaryUpdateUnsupported,
		"a native deployment may never be rejected by the container guard")
	require.Contains(t, err.Error(), "checksum")
	require.Len(t, client.downloadedURLs(), 1)
}

func TestRestoreBackupBinaryMovesRunningBinaryAsideBeforeReplacingIt(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	backup := current + ".backup"
	aside := rollbackAsidePath(current)
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(backup, []byte("previous-working"), 0o755))
	recorder := &moveRecorder{}

	require.NoError(t, restoreBackupBinary(current, backup, recorder.move))

	asideMove := recorder.indexOf(current, aside)
	installMove := recorder.indexOf(backup, current)
	require.NotEqual(t, -1, asideMove, "the running binary must be moved aside first")
	require.NotEqual(t, -1, installMove, "the backup must be installed at the executable path")
	require.Less(t, asideMove, installMove,
		"renaming onto a running executable fails on Windows, so it must be vacated first")

	body, err := os.ReadFile(current)
	require.NoError(t, err)
	require.Equal(t, "previous-working", string(body))
	body, err = os.ReadFile(backup)
	require.NoError(t, err)
	require.Equal(t, "current", string(body),
		"the binary rolled back from stays available as the new .backup")
	_, err = os.Stat(aside)
	require.True(t, os.IsNotExist(err), "no parked file may be left behind")
}

func TestRestoreBackupBinaryLeavesEverythingWhenAsideMoveFails(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	backup := current + ".backup"
	moveFailed := errors.New("current executable is locked")
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(backup, []byte("previous-working"), 0o755))
	recorder := &moveRecorder{fail: func(src, _ string) error {
		if src == current {
			return moveFailed
		}
		return nil
	}}

	err := restoreBackupBinary(current, backup, recorder.move)

	require.ErrorIs(t, err, moveFailed)
	requireFileBody(t, current, "current")
	requireFileBody(t, backup, "previous-working")
	_, statErr := os.Stat(rollbackAsidePath(current))
	require.True(t, os.IsNotExist(statErr))
}

func TestRestoreBackupBinaryRestoresCurrentWhenBackupCannotBeInstalled(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	backup := current + ".backup"
	moveFailed := errors.New("cannot install backup")
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(backup, []byte("previous-working"), 0o755))
	recorder := &moveRecorder{fail: func(src, dst string) error {
		if src == backup && dst == current {
			return moveFailed
		}
		return nil
	}}

	err := restoreBackupBinary(current, backup, recorder.move)

	require.ErrorIs(t, err, moveFailed)
	requireFileBody(t, current, "current")
	requireFileBody(t, backup, "previous-working")
	_, statErr := os.Stat(rollbackAsidePath(current))
	require.True(t, os.IsNotExist(statErr), "a failed restore must not park a file")
}

// If even the restore fails the executable path is left vacant, so the error has
// to name where the only copy of the binary went.
func TestRestoreBackupBinaryNamesParkedBinaryWhenRestoreAlsoFails(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	backup := current + ".backup"
	aside := rollbackAsidePath(current)
	moveFailed := errors.New("cannot move")
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(backup, []byte("previous-working"), 0o755))
	recorder := &moveRecorder{fail: func(_, dst string) error {
		if dst == current {
			return moveFailed
		}
		return nil
	}}

	err := restoreBackupBinary(current, backup, recorder.move)

	require.ErrorIs(t, err, moveFailed)
	require.Contains(t, err.Error(), aside)
	require.Contains(t, err.Error(), "restore failed")
	requireFileBody(t, aside, "current")
	requireFileBody(t, backup, "previous-working")
}

func TestRestoreBackupBinaryReportsPartialStateWhenBackupSlotCannotBeRefilled(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	backup := current + ".backup"
	aside := rollbackAsidePath(current)
	moveFailed := errors.New("cannot refill backup")
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(backup, []byte("previous-working"), 0o755))
	recorder := &moveRecorder{fail: func(_, dst string) error {
		if dst == backup {
			return moveFailed
		}
		return nil
	}}

	err := restoreBackupBinary(current, backup, recorder.move)

	require.ErrorIs(t, err, moveFailed)
	require.Contains(t, err.Error(), aside, "the parked binary must be named so it can be recovered")
	requireFileBody(t, current, "previous-working")
	requireFileBody(t, aside, "current")
}

func TestRestoreBackupBinaryRefusesWhenNoBackupExists(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))

	err := restoreBackupBinary(current, current+".backup", os.Rename)

	require.Error(t, err)
	require.Contains(t, err.Error(), "no backup found")
	requireFileBody(t, current, "current")
}

func TestRestoreBackupBinaryDoesNotClobberUnownedAsideFile(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "sub2api")
	backup := current + ".backup"
	require.NoError(t, os.WriteFile(current, []byte("current"), 0o755))
	require.NoError(t, os.WriteFile(backup, []byte("previous-working"), 0o755))
	require.NoError(t, os.WriteFile(rollbackAsidePath(current), []byte("unowned"), 0o755))

	err := restoreBackupBinary(current, backup, os.Rename)

	require.Error(t, err, "an unexpected preexisting file must block before anything moves")
	requireFileBody(t, current, "current")
	requireFileBody(t, backup, "previous-working")
	requireFileBody(t, rollbackAsidePath(current), "unowned")
}

func requireFileBody(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(body))
}
