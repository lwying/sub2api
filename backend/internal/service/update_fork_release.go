package service

import (
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
)

// Fork release contract for the in-app updater.
//
// The updater installs binaries from exactly one place: releases published by
// lwying/sub2api. Everything that decides "is this an update, and may I install
// it" is derived here from the release payload alone:
//
//   - the tag must be a strict vX.Y.Z three-segment numeric version;
//   - the running platform's archive must be published;
//   - checksums.txt must be published, because the fork's own download host
//     cannot prove asset provenance on its own;
//   - every download URL must name this repository, the selected release tag and
//     the exact asset, so a release cannot hand the updater a URL pointing at
//     another repository's (or upstream's) assets.
//
// These rules deliberately do not depend on the publishing pipeline having
// enforced a gate: the updater must stay fail-closed even for a hand-made or
// mis-published release. The archive naming mirrors .goreleaser.yaml
// (sub2api_<version>_<os>_<arch>, .zip on windows, .tar.gz elsewhere), which is
// the same contract the release workflow publishes and internal/releasecontract
// validates at publish time.
const (
	// forkRepo is the only GitHub repository the updater checks and installs from.
	forkRepo = "lwying/sub2api"
	// checksumsAssetName must be published by every installable fork release.
	checksumsAssetName = "checksums.txt"
	// assetDownloadHost is the host GitHub serves release asset URLs from.
	assetDownloadHost = "github.com"
)

// forkReleasePlatforms mirrors the goos/goarch matrix in .goreleaser.yaml
// (windows/arm64 is intentionally absent). A build outside this set has no
// published archive, so it can never be offered a one-click update.
var forkReleasePlatforms = []struct {
	goos   string
	goarch string
}{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
}

func isForkReleasePlatform(goos, goarch string) bool {
	for _, p := range forkReleasePlatforms {
		if p.goos == goos && p.goarch == goarch {
			return true
		}
	}
	return false
}

// forkVersion is a fork release version in the required three-segment numeric
// form. Prerelease suffixes, build metadata, two-segment forms and leading zeros
// are rejected: the update check compares numeric segments only, so
// "v0.2.7-rc.1" would silently sort as older than "v0.2.7" and a "v1.02.0" tag
// would not round-trip.
type forkVersion struct {
	major int
	minor int
	patch int
}

func (v forkVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// compare returns -1, 0 or 1.
func (v forkVersion) compare(other forkVersion) int {
	switch {
	case v.major != other.major:
		return sign(v.major - other.major)
	case v.minor != other.minor:
		return sign(v.minor - other.minor)
	case v.patch != other.patch:
		return sign(v.patch - other.patch)
	default:
		return 0
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// parseForkReleaseTag accepts exactly "vX.Y.Z" as a fork release tag.
func parseForkReleaseTag(raw string) (forkVersion, error) {
	tag := strings.TrimSpace(raw)
	if !strings.HasPrefix(tag, "v") {
		return forkVersion{}, fmt.Errorf("fork release tag %q must be v-prefixed, like v1.2.3", raw)
	}
	parts := strings.Split(tag[1:], ".")
	if len(parts) != 3 {
		return forkVersion{}, fmt.Errorf("fork release tag %q must be exactly three numeric segments", raw)
	}

	var nums [3]int
	for i, part := range parts {
		if part == "" {
			return forkVersion{}, fmt.Errorf("fork release tag %q has an empty segment", raw)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return forkVersion{}, fmt.Errorf("fork release tag %q segment %q is not numeric", raw, part)
			}
		}
		if len(part) > 1 && part[0] == '0' {
			return forkVersion{}, fmt.Errorf("fork release tag %q segment %q has a leading zero", raw, part)
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return forkVersion{}, fmt.Errorf("fork release tag %q segment %q is not a number: %w", raw, part, err)
		}
		nums[i] = n
	}
	return forkVersion{major: nums[0], minor: nums[1], patch: nums[2]}, nil
}

// parseRunningVersion is intentionally lenient: the version of the running
// build comes from build flags (a VERSION file or git describe) rather than from
// a release tag, so an unparseable or empty value must not break the check. It
// degrades to 0.0.0, which offers any valid fork release.
func parseRunningVersion(raw string) forkVersion {
	v := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if idx := strings.IndexByte(v, '-'); idx != -1 {
		v = v[:idx]
	}
	if idx := strings.IndexByte(v, '+'); idx != -1 {
		v = v[:idx]
	}

	var nums [3]int
	for i, part := range strings.Split(v, ".") {
		if i >= 3 {
			break
		}
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		nums[i] = n
	}
	return forkVersion{major: nums[0], minor: nums[1], patch: nums[2]}
}

// forkArchiveName returns the archive asset name a fork release must publish for
// the given platform. ok is false for platforms outside the release matrix.
func forkArchiveName(v forkVersion, goos, goarch string) (string, bool) {
	if !isForkReleasePlatform(goos, goarch) {
		return "", false
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("sub2api_%s_%s_%s%s", v, goos, goarch, ext), true
}

// forkReleaseState is the draft/prerelease state of a release, and whether it was
// actually observed.
//
// GitHub's "latest" endpoint normally excludes drafts and prereleases, but the
// updater must not depend on that: the publishing side refuses draft and
// prerelease releases outright, so a release that is either must never be offered
// for installation here. Known is what makes a *cached* payload safe — a payload
// that does not positively record that the release was observed as stable (for
// instance one hand-written into the update cache) is treated as unverifiable
// rather than assumed stable.
type forkReleaseState struct {
	Known      bool
	Draft      bool
	Prerelease bool
}

// observedForkReleaseState reads the flags of a release returned by the API.
func observedForkReleaseState(release *GitHubRelease) forkReleaseState {
	if release == nil {
		return forkReleaseState{}
	}
	return forkReleaseState{Known: true, Draft: release.Draft, Prerelease: release.Prerelease}
}

// stableForkReleaseState is the state to use for release metadata that carries no
// draft/prerelease flags of its own, such as the asset list the install gate
// works from. It asserts nothing and verifies nothing: it means "the caller has
// already established that this release is stable", so every caller must have
// done so from an observed release object (the update check, or a rollback
// candidate that was filtered on its flags). A future caller able to reach the
// install gate with release metadata it did not observe must enforce the flags
// itself before calling it.
func stableForkReleaseState() forkReleaseState { return forkReleaseState{Known: true} }

// forkReleaseAssessment is the verdict for one release tag and its asset names,
// as seen from the platform the updater is running on.
type forkReleaseAssessment struct {
	Tag      string
	Version  forkVersion
	ValidTag bool
	// Installable is true only when the release was observed as neither draft nor
	// prerelease, the tag is a fork version, and the platform archive and
	// checksums.txt are both published.
	Installable bool
	// ArchiveName is the platform archive asset the release must carry.
	ArchiveName string
	// ChecksumsPublished is whether checksums.txt was published.
	ChecksumsPublished bool
	// Reason is a stable code for logs; Message explains it for an operator.
	Reason  string
	Message string
}

// Reason codes reported by assessForkRelease.
const (
	forkReasonOK                  = "ok"
	forkReasonBadTag              = "bad_tag"
	forkReasonStateUnknown        = "release_state_unknown"
	forkReasonDraft               = "draft"
	forkReasonPrerelease          = "prerelease"
	forkReasonUnsupportedPlatform = "unsupported_platform"
	forkReasonMissingArchive      = "missing_platform_archive"
	forkReasonMissingChecksums    = "checksums_missing"
)

// assessForkRelease decides whether a fork release can be installed on this
// host. A release that fails any rule is reported as non-installable with a
// reason, never as a partial success.
func assessForkRelease(tag string, assetNames []string, state forkReleaseState) forkReleaseAssessment {
	assessment := forkReleaseAssessment{Tag: tag}

	version, err := parseForkReleaseTag(tag)
	if err != nil {
		assessment.Reason = forkReasonBadTag
		assessment.Message = err.Error()
		return assessment
	}
	assessment.Version = version
	assessment.ValidTag = true

	// Fail closed on the release state before anything else is believed about it.
	if !state.Known {
		assessment.Reason = forkReasonStateUnknown
		assessment.Message = fmt.Sprintf(
			"release %s cannot be offered: whether it is a draft or a prerelease could not be established", tag)
		return assessment
	}
	if state.Draft {
		assessment.Reason = forkReasonDraft
		assessment.Message = fmt.Sprintf("release %s is a draft and must not be installed", tag)
		return assessment
	}
	if state.Prerelease {
		assessment.Reason = forkReasonPrerelease
		assessment.Message = fmt.Sprintf("release %s is a prerelease and must not be installed", tag)
		return assessment
	}

	archiveName, ok := forkArchiveName(version, runtime.GOOS, runtime.GOARCH)
	if !ok {
		assessment.Reason = forkReasonUnsupportedPlatform
		assessment.Message = fmt.Sprintf(
			"no installable asset for %s/%s: the fork release contract does not cover this platform",
			runtime.GOOS, runtime.GOARCH)
		return assessment
	}
	assessment.ArchiveName = archiveName

	present := make(map[string]bool, len(assetNames))
	for _, name := range assetNames {
		present[strings.TrimSpace(name)] = true
	}

	if !present[archiveName] {
		assessment.Reason = forkReasonMissingArchive
		assessment.Message = fmt.Sprintf(
			"no installable asset for %s/%s: release %s does not publish %s",
			runtime.GOOS, runtime.GOARCH, tag, archiveName)
		return assessment
	}

	if !present[checksumsAssetName] {
		assessment.Reason = forkReasonMissingChecksums
		assessment.Message = fmt.Sprintf(
			"%s is required for an installable fork release; release %s does not publish it",
			checksumsAssetName, tag)
		return assessment
	}

	assessment.ChecksumsPublished = true
	assessment.Installable = true
	assessment.Reason = forkReasonOK
	assessment.Message = fmt.Sprintf("release %s is installable on %s/%s", tag, runtime.GOOS, runtime.GOARCH)
	return assessment
}

// forkRepoOwnerAndName splits forkRepo into its two path segments.
func forkRepoOwnerAndName() (string, string) {
	owner, name, found := strings.Cut(forkRepo, "/")
	if !found {
		return forkRepo, ""
	}
	return owner, name
}

// validateForkAssetURL requires a download URL to be an HTTPS URL on github.com
// whose path names this fork, the selected release tag and the exact asset.
//
// The host allowlist used before this check (github.com plus
// objects.githubusercontent.com) only proved "GitHub", not "the fork's release":
// any GitHub repository's asset URL passed it. Binding the path to
// lwying/sub2api/releases/download/<tag>/<asset> is what makes the metadata
// itself prove which release the bytes belong to.
//
// The URL is refused when any part of it is ambiguous, because a proxy, cache or
// HTTP router may resolve the bytes differently from url.Parse: query strings and
// fragments (never present on a release asset), an opaque URL, a percent-encoded
// path (url.Parse decodes "%2F" into a path separator, so the validated path and
// the requested path would differ), an explicit port, credentials, and a
// non-canonical path such as a doubled leading slash. Nothing is trimmed or
// normalised: the path must be exactly the canonical asset path, compared
// segment by segment so owner and repository keep GitHub's case-insensitive
// matching while the tag and asset name do not.
func validateForkAssetURL(rawURL, tag, assetName string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid asset URL %q: %w", rawURL, err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("asset URL must use https: %q", rawURL)
	}
	if parsed.User != nil {
		return fmt.Errorf("asset URL must not carry credentials: %q", rawURL)
	}
	if parsed.Opaque != "" {
		return fmt.Errorf("asset URL must be hierarchical: %q", rawURL)
	}
	if parsed.Port() != "" {
		return fmt.Errorf("asset URL must not specify a port: %q", rawURL)
	}
	if !strings.EqualFold(parsed.Host, assetDownloadHost) {
		return fmt.Errorf("asset %q is not served from %s: %q", assetName, assetDownloadHost, rawURL)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("asset URL must not carry a query or fragment: %q", rawURL)
	}
	if parsed.RawPath != "" {
		// An escape that url.Parse had to decode: the wire path and the decoded
		// path disagree, which is exactly the ambiguity a routing bypass needs.
		return fmt.Errorf("asset URL must not use percent-encoding: %q", rawURL)
	}
	if !strings.HasPrefix(parsed.Path, "/") {
		return fmt.Errorf("asset URL must use an absolute path: %q", rawURL)
	}

	owner, repo := forkRepoOwnerAndName()
	want := []string{owner, repo, "releases", "download", tag, assetName}
	segments := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(segments) != len(want) {
		return fmt.Errorf("asset URL %q does not point at release %s of %s", rawURL, tag, forkRepo)
	}
	for i, expected := range want {
		// Owner and repository are case-insensitive on GitHub; the tag and the
		// asset name are not.
		match := segments[i] == expected
		if i < 2 {
			match = strings.EqualFold(segments[i], expected)
		}
		if !match {
			return fmt.Errorf("asset URL %q does not point at release %s of %s", rawURL, tag, forkRepo)
		}
	}
	return nil
}

// isAllowedAssetRedirectHost reports whether a redirect target still belongs to
// GitHub's own release-asset hosts.
func isAllowedAssetRedirectHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == assetDownloadHost {
		return true
	}
	// Release assets redirect to GitHub-controlled asset hosts
	// (objects.githubusercontent.com, release-assets.githubusercontent.com).
	return strings.HasSuffix(h, ".githubusercontent.com")
}

// ForkAssetCheckRedirect refuses download redirects that leave GitHub's asset
// hosts or downgrade to plain HTTP, so a redirect cannot move an update download
// onto a third-party origin. It is exported because the HTTP client that
// performs release downloads is built in the repository layer.
func ForkAssetCheckRedirect(previous func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if req.URL == nil {
			return fmt.Errorf("asset download redirect without a URL")
		}
		if !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("asset download redirect must use https: %s", req.URL)
		}
		if !isAllowedAssetRedirectHost(req.URL.Hostname()) {
			return fmt.Errorf("asset download redirected off GitHub's asset hosts: %s", req.URL.Host)
		}
		if previous != nil {
			return previous(req, via)
		}
		return nil
	}
}
