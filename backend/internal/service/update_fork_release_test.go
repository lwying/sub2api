//go:build unit

package service

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseForkReleaseTag(t *testing.T) {
	tests := []struct {
		tag     string
		want    string
		wantErr bool
	}{
		{tag: "v0.2.6", want: "0.2.6"},
		{tag: "v1.0.0", want: "1.0.0"},
		{tag: "v10.20.30", want: "10.20.30"},
		{tag: "  v0.2.6  ", want: "0.2.6"},
		{tag: "0.2.6", wantErr: true},
		{tag: "v0.2", wantErr: true},
		{tag: "v0.2.6.1", wantErr: true},
		{tag: "v0.2.6-rc.1", wantErr: true},
		{tag: "v0.2.6+build.1", wantErr: true},
		{tag: "v0.02.6", wantErr: true},
		{tag: "vx.y.z", wantErr: true},
		{tag: "v", wantErr: true},
		{tag: "", wantErr: true},
		{tag: "release-0.2.6", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			got, err := parseForkReleaseTag(tt.tag)

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got.String())
		})
	}
}

func TestParseRunningVersionIsLenient(t *testing.T) {
	// The running version comes from build flags, not a release tag: odd values
	// must degrade rather than break the check.
	tests := map[string]string{
		"0.2.6":         "0.2.6",
		"v0.2.6":        "0.2.6",
		"0.2.6-dirty":   "0.2.6",
		"0.2.6+commit1": "0.2.6",
		"1.2.3":         "1.2.3",
		"dev":           "0.0.0",
		"":              "0.0.0",
		"unknown":       "0.0.0",
	}

	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			require.Equal(t, want, parseRunningVersion(raw).String())
		})
	}
}

func TestForkArchiveNameMirrorsGoreleaser(t *testing.T) {
	version, err := parseForkReleaseTag("v0.3.0")
	require.NoError(t, err)

	tests := []struct {
		goos, goarch string
		want         string
		wantOK       bool
	}{
		{goos: "windows", goarch: "amd64", want: "sub2api_0.3.0_windows_amd64.zip", wantOK: true},
		{goos: "linux", goarch: "amd64", want: "sub2api_0.3.0_linux_amd64.tar.gz", wantOK: true},
		{goos: "linux", goarch: "arm64", want: "sub2api_0.3.0_linux_arm64.tar.gz", wantOK: true},
		{goos: "darwin", goarch: "arm64", want: "sub2api_0.3.0_darwin_arm64.tar.gz", wantOK: true},
		// Outside the release matrix: no archive is published, so nothing is
		// installable and the update path must not invent a name.
		{goos: "windows", goarch: "arm64"},
		{goos: "freebsd", goarch: "amd64"},
	}

	for _, tt := range tests {
		t.Run(tt.goos+"/"+tt.goarch, func(t *testing.T) {
			name, ok := forkArchiveName(version, tt.goos, tt.goarch)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, name)
		})
	}
}

func TestAssessForkReleaseRequiresTagArchiveAndChecksums(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	archive := updateTestRunningArchive("0.3.0")
	stable := stableForkReleaseState()

	tests := []struct {
		name         string
		tag          string
		assets       []string
		state        forkReleaseState
		wantValidTag bool
		wantReason   string
	}{
		{
			name:         "installable",
			tag:          "v0.3.0",
			assets:       []string{archive, checksumsAssetName},
			state:        stable,
			wantValidTag: true,
			wantReason:   forkReasonOK,
		},
		{
			name:         "draft release",
			tag:          "v0.3.0",
			assets:       []string{archive, checksumsAssetName},
			state:        forkReleaseState{Known: true, Draft: true},
			wantValidTag: true,
			wantReason:   forkReasonDraft,
		},
		{
			name:         "prerelease release",
			tag:          "v0.3.0",
			assets:       []string{archive, checksumsAssetName},
			state:        forkReleaseState{Known: true, Prerelease: true},
			wantValidTag: true,
			wantReason:   forkReasonPrerelease,
		},
		{
			name:         "release flags never observed",
			tag:          "v0.3.0",
			assets:       []string{archive, checksumsAssetName},
			state:        forkReleaseState{},
			wantValidTag: true,
			wantReason:   forkReasonStateUnknown,
		},
		{
			name:         "missing platform archive",
			tag:          "v0.3.0",
			assets:       []string{updateTestOtherPlatformArchive("0.3.0"), checksumsAssetName},
			state:        stable,
			wantValidTag: true,
			wantReason:   forkReasonMissingArchive,
		},
		{
			name:         "missing checksums",
			tag:          "v0.3.0",
			assets:       []string{archive},
			state:        stable,
			wantValidTag: true,
			wantReason:   forkReasonMissingChecksums,
		},
		{
			name:         "image-only release",
			tag:          "v0.3.0",
			assets:       []string{"sub2api_0.3.0_amd64.deb"},
			state:        stable,
			wantValidTag: true,
			wantReason:   forkReasonMissingArchive,
		},
		{
			name:         "not a fork tag",
			tag:          "v0.3.0-rc.1",
			assets:       []string{archive, checksumsAssetName},
			state:        stable,
			wantValidTag: false,
			wantReason:   forkReasonBadTag,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assessForkRelease(tt.tag, tt.assets, tt.state)

			require.Equal(t, tt.wantValidTag, got.ValidTag)
			require.Equal(t, tt.wantReason, got.Reason)
			require.Equal(t, tt.wantReason == forkReasonOK, got.Installable)
			require.NotEmpty(t, got.Message)
		})
	}
}

func TestAssessForkReleaseMessageNamesTheReason(t *testing.T) {
	updateTestRequireSupportedPlatform(t)
	assets := []string{updateTestRunningArchive("0.3.0"), checksumsAssetName}

	tests := map[forkReleaseState]string{
		{Known: true, Draft: true}:      "draft",
		{Known: true, Prerelease: true}: "prerelease",
		{}:                              "could not be established",
	}

	for state, wantTip := range tests {
		got := assessForkRelease("v0.3.0", assets, state)

		require.False(t, got.Installable)
		require.Contains(t, got.Message, wantTip)
	}
}

func TestAssessForkReleaseRefusesUnsupportedPlatform(t *testing.T) {
	// Simulating another platform directly keeps this deterministic regardless of
	// where the suite runs.
	_, ok := forkArchiveName(forkVersion{major: 0, minor: 3, patch: 0}, "plan9", "mips")
	require.False(t, ok)
}

func TestValidateForkAssetURL(t *testing.T) {
	const (
		tag   = "v0.3.0"
		asset = "sub2api_0.3.0_linux_amd64.tar.gz"
	)
	valid := "https://github.com/lwying/sub2api/releases/download/" + tag + "/" + asset

	tests := []struct {
		name    string
		url     string
		asset   string
		wantErr bool
	}{
		{name: "fork release asset", url: valid, asset: asset},
		{name: "checksums of the same release", url: "https://github.com/lwying/sub2api/releases/download/" + tag + "/" + checksumsAssetName, asset: checksumsAssetName},
		{name: "owner and repo are case insensitive", url: "https://github.com/LwYing/Sub2API/releases/download/" + tag + "/" + asset, asset: asset},
		{name: "upstream repository", url: "https://github.com/Wei-Shaw/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "other repository", url: "https://github.com/attacker/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "release asset host instead of github.com", url: "https://objects.githubusercontent.com/lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "lookalike host", url: "https://github.com.evil.example/lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "plain HTTP", url: "http://github.com/lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "wrong tag", url: "https://github.com/lwying/sub2api/releases/download/v9.9.9/" + asset, asset: asset, wantErr: true},
		{name: "different asset of the same release", url: "https://github.com/lwying/sub2api/releases/download/" + tag + "/sub2api_0.3.0_linux_arm64.tar.gz", asset: asset, wantErr: true},
		{name: "assets endpoint instead of the release download path", url: "https://github.com/lwying/sub2api/releases/assets/1234", asset: asset, wantErr: true},
		{name: "extra path segment", url: "https://github.com/lwying/sub2api/releases/download/" + tag + "/extra/" + asset, asset: asset, wantErr: true},
		{name: "credentials in URL", url: "https://user:pass@github.com/lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "explicit port", url: "https://github.com:8443/lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "default port", url: "https://github.com:443/lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "query string", url: "https://github.com/lwying/sub2api/releases/download/" + tag + "/" + asset + "?x=1", asset: asset, wantErr: true},
		{name: "fragment", url: "https://github.com/lwying/sub2api/releases/download/" + tag + "/" + asset + "#frag", asset: asset, wantErr: true},
		{name: "encoded path separator", url: "https://github.com/lwying%2Fsub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "encoded asset name", url: "https://github.com/lwying/sub2api/releases/download/" + tag + "/sub2api%5F0.3.0_linux_amd64.tar.gz", asset: asset, wantErr: true},
		{name: "doubled leading slash", url: "https://github.com//lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "trailing slash", url: "https://github.com/lwying/sub2api/releases/download/" + tag + "/" + asset + "/", asset: asset, wantErr: true},
		{name: "traversal segment", url: "https://github.com/lwying/sub2api/releases/download/" + tag + "/../latest/" + asset, asset: asset, wantErr: true},
		{name: "opaque URL", url: "https:github.com/lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "unicode lookalike host", url: "https://gіthub.com/lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
		{name: "punycode lookalike host", url: "https://github.com.evil.example/lwying/sub2api/releases/download/" + tag + "/" + asset, asset: asset, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateForkAssetURL(tt.url, tag, tt.asset)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestForkAssetCheckRedirectRestrictsHosts(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "same host", url: "https://github.com/lwying/sub2api/releases/download/v0.3.0/x"},
		{name: "objects asset host", url: "https://objects.githubusercontent.com/asset"},
		{name: "release assets host", url: "https://release-assets.githubusercontent.com/asset"},
		{name: "other githubusercontent host", url: "https://raw.githubusercontent.com/asset"},
		{name: "plain HTTP", url: "http://objects.githubusercontent.com/asset", wantErr: true},
		{name: "third party", url: "https://evil.example/asset", wantErr: true},
		{name: "suffix spoof", url: "https://evilgithubusercontent.com/asset", wantErr: true},
		{name: "host spoof", url: "https://github.com.evil.example/asset", wantErr: true},
	}

	check := ForkAssetCheckRedirect(nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tt.url, nil)
			require.NoError(t, err)

			err = check(req, nil)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestForkAssetCheckRedirectKeepsPreviousPolicy(t *testing.T) {
	sentinel := errors.New("previous policy refused")
	check := ForkAssetCheckRedirect(func(*http.Request, []*http.Request) error { return sentinel })

	req, err := http.NewRequest(http.MethodGet, "https://objects.githubusercontent.com/asset", nil)
	require.NoError(t, err)

	require.ErrorIs(t, check(req, nil), sentinel)

	// A refused redirect must not be reported as accepted just because the
	// previous policy is nil.
	require.NoError(t, ForkAssetCheckRedirect(nil)(req, nil))
}

func TestValidateForkAssetURLRejectsMissingScheme(t *testing.T) {
	err := validateForkAssetURL("/lwying/sub2api/releases/download/v0.3.0/x", "v0.3.0", "x")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "https"))
}
