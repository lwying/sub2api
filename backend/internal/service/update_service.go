package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrNoUpdateAvailable         = infraerrors.Conflict("ALREADY_UP_TO_DATE", "no update available; current version is latest")
	ErrRollbackVersionNotAllowed = infraerrors.BadRequest("ROLLBACK_VERSION_NOT_ALLOWED", "version is not in the allowed rollback list")

	// ErrBinaryUpdateUnsupported is returned by every path that would replace the
	// running binary in a deployment whose executable belongs to a container
	// image. The image, not the file inside the container, is the unit of update:
	// swapping /app/sub2api neither updates the image nor survives the next
	// container start, so the attempt is refused before anything is downloaded.
	ErrBinaryUpdateUnsupported = infraerrors.Conflict(
		"BINARY_UPDATE_UNSUPPORTED",
		"this instance runs from a container image, so in-app binary update and rollback are not available; update the container image and recreate the container instead",
	)
)

// Deployment markers. The marker is always an explicit build-time or
// image-level declaration, never inferred from the filesystem, so a native
// (GoReleaser/archive/systemd) binary that merely happens to run inside an
// unrelated container keeps its in-app updater.
const (
	// DeploymentTypeNative is the default: the deployment owns its executable and
	// the updater may replace it in place.
	DeploymentTypeNative = "native"
	// DeploymentTypeDocker marks a binary owned by a container image.
	DeploymentTypeDocker = "docker"
)

// normalizeDeploymentType maps a deployment marker onto the two kinds this
// updater distinguishes. Only an explicit "docker" counts as a container
// deployment; an empty or unrecognized marker is a native install, which keeps
// the in-app updater available.
func normalizeDeploymentType(marker string) string {
	if strings.EqualFold(strings.TrimSpace(marker), DeploymentTypeDocker) {
		return DeploymentTypeDocker
	}
	return DeploymentTypeNative
}

const (
	updateCacheTTL = 1200 // 20 minutes

	// Security: max download size (500MB)
	maxDownloadSize = 500 * 1024 * 1024

	// Security: checksums.txt is a small text file (~100 bytes per asset).
	// Bounding it keeps a misbehaving asset from exhausting memory while the
	// updater is only checking a version.
	maxChecksumSize = 1 << 20 // 1 MiB

	// Rollback: expose at most the 3 most recent versions older than current
	maxRollbackVersions = 3
	// Fetch a few extra releases so filtering (current/newer/prerelease) still leaves enough candidates
	rollbackFetchPageSize = 15
)

// UpdateCache defines cache operations for update service
type UpdateCache interface {
	GetUpdateInfo(ctx context.Context) (string, error)
	SetUpdateInfo(ctx context.Context, data string, ttl time.Duration) error
}

// GitHubReleaseClient 获取 GitHub release 信息的接口
type GitHubReleaseClient interface {
	FetchLatestRelease(ctx context.Context, repo string) (*GitHubRelease, error)
	FetchRecentReleases(ctx context.Context, repo string, perPage int) ([]*GitHubRelease, error)
	DownloadFile(ctx context.Context, url, dest string, maxSize int64) error
	FetchChecksumFile(ctx context.Context, url string, maxSize int64) ([]byte, error)
}

// UpdateService handles software updates
type UpdateService struct {
	cache          UpdateCache
	githubClient   GitHubReleaseClient
	currentVersion string
	buildType      string // "source" for manual builds, "release" for CI builds
	// deploymentType is "native" when the deployment owns its executable and
	// "docker" when a container image does. It gates every in-place binary swap.
	deploymentType string
}

// NewUpdateService creates a new UpdateService
func NewUpdateService(cache UpdateCache, githubClient GitHubReleaseClient, version, buildType, deploymentType string) *UpdateService {
	return &UpdateService{
		cache:          cache,
		githubClient:   githubClient,
		currentVersion: version,
		buildType:      buildType,
		deploymentType: normalizeDeploymentType(deploymentType),
	}
}

// binaryUpdateUnsupported reports whether this deployment must refuse to replace
// its own executable. It is checked at the start of every path that would write
// to the executable, before any download or rename.
func (s *UpdateService) binaryUpdateUnsupported() error {
	if s.deploymentType == DeploymentTypeDocker {
		return ErrBinaryUpdateUnsupported
	}
	return nil
}

// binaryUpdateSupported reports whether this deployment may install a downloaded
// binary in place.
func (s *UpdateService) binaryUpdateSupported() bool {
	return s.binaryUpdateUnsupported() == nil
}

// UpdateInfo contains update information
type UpdateInfo struct {
	CurrentVersion string       `json:"current_version"`
	LatestVersion  string       `json:"latest_version"`
	HasUpdate      bool         `json:"has_update"`
	ReleaseInfo    *ReleaseInfo `json:"release_info,omitempty"`
	Cached         bool         `json:"cached"`
	Warning        string       `json:"warning,omitempty"`
	BuildType      string       `json:"build_type"` // "source" or "release"
	// DeploymentType is "native" or "docker" and describes who owns the running
	// executable, independently of how it was built.
	DeploymentType string `json:"deployment_type"`
	// BinaryUpdateSupported is false when this deployment cannot install a
	// downloaded binary in place: the image is updated, not the file. It is
	// present on every response, including the ones that carry no release or a
	// failed check, so the caller never has to infer it from a missing field.
	BinaryUpdateSupported bool `json:"binary_update_supported"`
}

// ReleaseInfo contains GitHub release details
type ReleaseInfo struct {
	// TagName is the release tag the assets below belong to. Asset URLs are
	// validated against it, so it is kept verbatim ("v1.2.3") rather than
	// trimmed like LatestVersion.
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	Body        string  `json:"body"`
	PublishedAt string  `json:"published_at"`
	HTMLURL     string  `json:"html_url"`
	Assets      []Asset `json:"assets,omitempty"`
}

// Asset represents a release asset
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"download_url"`
	Size        int64  `json:"size"`
}

// GitHubRelease represents GitHub API response
type GitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	PublishedAt string        `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Assets      []GitHubAsset `json:"assets"`
}

// RollbackVersion describes a release version the system can roll back to
type RollbackVersion struct {
	Version     string `json:"version"` // without "v" prefix, e.g. "0.1.146"
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}

type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// CheckUpdate checks for available updates
func (s *UpdateService) CheckUpdate(ctx context.Context, force bool) (*UpdateInfo, error) {
	// Try cache first
	if !force {
		if cached, err := s.getFromCache(ctx); err == nil && cached != nil {
			return cached, nil
		}
	}

	// Fetch from the fork
	info, state, err := s.fetchLatestRelease(ctx)
	if err != nil {
		// Fail closed: a failed fork check may only fall back to cache data this
		// updater wrote for the fork. getFromCache refuses every other payload —
		// in particular the upstream-era entries — so an upstream release can
		// never be served as a fork update, and the administrator is told the
		// check failed instead.
		if cached, cacheErr := s.getFromCache(ctx); cacheErr == nil && cached != nil {
			cached.Warning = "Using cached data: " + err.Error()
			return cached, nil
		}
		return s.noUpdateInfo(err.Error()), nil
	}

	// Cache result
	s.saveToCache(ctx, info, state)
	return info, nil
}

// noUpdateInfo is the result shown when there is nothing installable to offer.
// LatestVersion stays at the running version so an unknown or unusable release
// tag is never presented to the administrator as a version to move to.
func (s *UpdateService) noUpdateInfo(warning string) *UpdateInfo {
	return &UpdateInfo{
		CurrentVersion:        s.currentVersion,
		LatestVersion:         s.currentVersion,
		HasUpdate:             false,
		Warning:               warning,
		BuildType:             s.buildType,
		DeploymentType:        s.deploymentType,
		BinaryUpdateSupported: s.binaryUpdateSupported(),
	}
}

// updateInfoForRelease derives the administrator-visible result for one fork
// release. Installability is always recomputed from the release's own asset
// list, so a cached payload cannot claim an update the fork no longer offers.
func (s *UpdateService) updateInfoForRelease(tag string, releaseInfo *ReleaseInfo, state forkReleaseState, cached bool) *UpdateInfo {
	assessment := assessForkRelease(tag, assetNames(releaseInfo.Assets), state)
	if !assessment.ValidTag || !state.Known {
		// Either not a fork version at all, or a release whose draft/prerelease
		// state cannot be vouched for: do not expose its metadata as an update.
		info := s.noUpdateInfo(assessment.Message)
		info.Cached = cached
		return info
	}

	info := &UpdateInfo{
		CurrentVersion: s.currentVersion,
		LatestVersion:  assessment.Version.String(),
		HasUpdate: assessment.Installable &&
			parseRunningVersion(s.currentVersion).compare(assessment.Version) < 0,
		ReleaseInfo:           releaseInfo,
		Cached:                cached,
		BuildType:             s.buildType,
		DeploymentType:        s.deploymentType,
		BinaryUpdateSupported: s.binaryUpdateSupported(),
	}
	if !assessment.Installable {
		info.Warning = assessment.Message
	}
	return info
}

// PerformUpdate downloads and applies the update
// Uses atomic file replacement pattern for safe in-place updates
func (s *UpdateService) PerformUpdate(ctx context.Context) error {
	// Checked before the release lookup: a container deployment is refused even
	// when the network is unavailable and nothing could be downloaded anyway.
	if err := s.binaryUpdateUnsupported(); err != nil {
		return err
	}

	info, err := s.CheckUpdate(ctx, true)
	if err != nil {
		return err
	}

	if !info.HasUpdate || info.ReleaseInfo == nil {
		return ErrNoUpdateAvailable
	}

	return s.applyReleaseAssets(ctx, info.ReleaseInfo.TagName, info.ReleaseInfo.Assets)
}

// applyReleaseAssets downloads the platform archive of the given fork release,
// verifies its checksum, and atomically swaps the running binary.
// Shared by PerformUpdate (latest) and RollbackToVersion (specific older version).
//
// Everything is re-checked here from the release metadata alone: the tag, the
// platform archive, the presence of checksums.txt and the origin of both URLs.
// PerformUpdate only calls this for an installable release, so these checks are
// the last line of defence rather than the only one — and they must not depend
// on the caller having done them.
func (s *UpdateService) applyReleaseAssets(ctx context.Context, releaseTag string, releaseAssets []Asset) error {
	// The install gate works from the asset list, which carries no draft or
	// prerelease flags, so it cannot verify them itself and does not pretend to:
	// both callers reach it only for a release whose flags were observed and
	// enforced beforehand (fetchLatestRelease from the API release object, and
	// fetchRollbackCandidates from the same flags). A new caller that cannot make
	// that guarantee must check the flags before calling this.
	assessment := assessForkRelease(releaseTag, assetNames(releaseAssets), stableForkReleaseState())
	if !assessment.Installable {
		return fmt.Errorf("refusing to install %s: %s", releaseTag, assessment.Message)
	}

	var archive *Asset
	var checksums *Asset
	for i := range releaseAssets {
		if releaseAssets[i].Name == assessment.ArchiveName {
			archive = &releaseAssets[i]
		}
		if releaseAssets[i].Name == checksumsAssetName {
			checksums = &releaseAssets[i]
		}
	}
	if archive == nil {
		return fmt.Errorf("no compatible release found for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if checksums == nil {
		// Unreachable while assessForkRelease requires it, and deliberately kept
		// as a hard failure: an unverified archive must never be installed.
		return fmt.Errorf("%s is required to verify %s", checksumsAssetName, assessment.ArchiveName)
	}

	// SECURITY: the URLs must name this fork's own release, not merely a GitHub
	// host. Anything else (upstream URLs, another repository, a non-GitHub
	// asset host, plain HTTP) is refused before a single byte is downloaded.
	if err := validateForkAssetURL(archive.DownloadURL, releaseTag, archive.Name); err != nil {
		return fmt.Errorf("invalid download URL: %w", err)
	}
	if err := validateForkAssetURL(checksums.DownloadURL, releaseTag, checksums.Name); err != nil {
		return fmt.Errorf("invalid checksum URL: %w", err)
	}

	downloadURL := archive.DownloadURL
	checksumURL := checksums.DownloadURL

	// Get current executable path
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("failed to resolve symlinks: %w", err)
	}

	exeDir := filepath.Dir(exePath)

	// Create temp directory in the SAME directory as executable
	// This ensures os.Rename is atomic (same filesystem)
	tempDir, err := os.MkdirTemp(exeDir, ".sub2api-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Download archive
	archivePath := filepath.Join(tempDir, filepath.Base(downloadURL))
	if err := s.downloadFile(ctx, downloadURL, archivePath); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	// Verify the SHA256 of the archive against the release's checksums.txt. The
	// file is mandatory (checked above), so the archive is always verified
	// before it can reach the running executable.
	if err := s.verifyChecksum(ctx, archivePath, checksumURL); err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	// Extract binary from archive
	newBinaryPath := filepath.Join(tempDir, "sub2api")
	if err := extractBinaryFromArchive(archivePath, newBinaryPath); err != nil {
		return fmt.Errorf("extraction failed: %w", err)
	}

	// Set executable permission before replacement
	if err := os.Chmod(newBinaryPath, 0755); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}

	return replaceUpdateBinary(exePath, newBinaryPath, os.Rename)
}

func replaceUpdateBinary(exePath, newBinaryPath string, move func(string, string) error) error {
	backupPath := exePath + ".backup"
	oldBackupPath := backupPath + ".previous"
	preservedBackup := false
	if _, err := os.Stat(oldBackupPath); err == nil {
		return fmt.Errorf("cannot update while previous backup path exists: %s", oldBackupPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect prior backup: %w", err)
	}
	if _, err := os.Stat(backupPath); err == nil {
		if err := move(backupPath, oldBackupPath); err != nil {
			return fmt.Errorf("preserve previous backup: %w", err)
		}
		preservedBackup = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect backup: %w", err)
	}
	if err := move(exePath, backupPath); err != nil {
		if preservedBackup {
			if restoreErr := move(oldBackupPath, backupPath); restoreErr != nil {
				return fmt.Errorf("backup failed: %w (prior backup at %s; restore failed: %v)", err, oldBackupPath, restoreErr)
			}
		}
		return fmt.Errorf("backup failed: %w", err)
	}
	if err := move(newBinaryPath, exePath); err != nil {
		if restoreErr := move(backupPath, exePath); restoreErr != nil {
			return fmt.Errorf("replace failed and restore failed: %w (restore error: %v)", err, restoreErr)
		}
		if preservedBackup {
			if restoreErr := move(oldBackupPath, backupPath); restoreErr != nil {
				return fmt.Errorf("replace failed (current restored), prior backup remains at %s: %w", oldBackupPath, restoreErr)
			}
		}
		return fmt.Errorf("replace failed (restored backup): %w", err)
	}
	if preservedBackup {
		_ = os.Remove(oldBackupPath)
	}
	return nil
}

// Rollback restores the local .backup binary left by the last in-place update.
//
// Like PerformUpdate and RollbackToVersion it writes to the running executable,
// so a deployment that does not own its binary must refuse it: restoring the
// backup inside a container would be undone by the next container start.
func (s *UpdateService) Rollback() error {
	if err := s.binaryUpdateUnsupported(); err != nil {
		return err
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("failed to resolve symlinks: %w", err)
	}

	return restoreBackupBinary(exePath, exePath+".backup", os.Rename)
}

// rollbackAsidePath is where the running executable is parked while the backup
// takes its place. The path is owned by this updater: an existing file there
// blocks the rollback instead of being overwritten, mirroring the update path's
// refusal to clobber an unowned ".previous" file.
func rollbackAsidePath(exePath string) string {
	return exePath + ".rollback-aside"
}

// restoreBackupBinary installs backupPath as the running executable.
//
// The swap cannot be a single rename onto the executable path: on Windows
// renaming onto a mapped executable image fails with ACCESS_DENIED, which is the
// same wall the update path avoids by moving the current binary aside first. So
// the current binary is parked, the backup is installed at the now-vacant
// executable path, and the parked binary is moved into the backup slot — the
// same "the replaced binary becomes the new .backup" invariant the update path
// keeps, so the local recovery path stays usable afterwards.
func restoreBackupBinary(exePath, backupPath string, move func(string, string) error) error {
	if _, err := os.Stat(backupPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no backup found")
		}
		return fmt.Errorf("inspect backup: %w", err)
	}

	asidePath := rollbackAsidePath(exePath)
	if _, err := os.Stat(asidePath); err == nil {
		return fmt.Errorf("cannot roll back while %s exists", asidePath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect rollback path: %w", err)
	}

	if err := move(exePath, asidePath); err != nil {
		return fmt.Errorf("rollback failed: %w", err)
	}
	if err := move(backupPath, exePath); err != nil {
		if restoreErr := move(asidePath, exePath); restoreErr != nil {
			return fmt.Errorf("rollback failed: %w (current binary parked at %s; restore failed: %v)", err, asidePath, restoreErr)
		}
		return fmt.Errorf("rollback failed: %w", err)
	}
	// The rollback itself has taken effect at this point; a failure to refill the
	// backup slot is reported as partial state rather than as a failed rollback,
	// and the parked binary is named so it can be recovered by hand.
	if err := move(asidePath, backupPath); err != nil {
		return fmt.Errorf("rollback applied, but the .backup slot could not be refilled: %w (rolled-back-from binary parked at %s)", err, asidePath)
	}
	return nil
}

// ListRollbackVersions returns up to maxRollbackVersions release versions that are
// strictly older than the current version (the current version itself is excluded),
// newest first. Draft and prerelease entries are skipped.
func (s *UpdateService) ListRollbackVersions(ctx context.Context) ([]RollbackVersion, error) {
	releases, err := s.fetchRollbackCandidates(ctx)
	if err != nil {
		return nil, err
	}

	versions := make([]RollbackVersion, 0, len(releases))
	for _, r := range releases {
		versions = append(versions, RollbackVersion{
			Version:     strings.TrimPrefix(r.TagName, "v"),
			PublishedAt: r.PublishedAt,
			HTMLURL:     r.HTMLURL,
		})
	}
	return versions, nil
}

// RollbackToVersion downloads and installs a specific older version.
// The target must be one of the versions returned by ListRollbackVersions;
// anything else (including the current version) is rejected.
func (s *UpdateService) RollbackToVersion(ctx context.Context, version string) error {
	if err := s.binaryUpdateUnsupported(); err != nil {
		return err
	}

	target := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if target == "" {
		return ErrRollbackVersionNotAllowed
	}

	releases, err := s.fetchRollbackCandidates(ctx)
	if err != nil {
		return err
	}

	var match *GitHubRelease
	for _, r := range releases {
		if strings.TrimPrefix(r.TagName, "v") == target {
			match = r
			break
		}
	}
	if match == nil {
		return ErrRollbackVersionNotAllowed
	}

	assets := make([]Asset, len(match.Assets))
	for i, a := range match.Assets {
		assets[i] = Asset{
			Name:        a.Name,
			DownloadURL: a.BrowserDownloadURL,
			Size:        a.Size,
		}
	}

	return s.applyReleaseAssets(ctx, match.TagName, assets)
}

// fetchRollbackCandidates fetches recent releases and keeps the newest
// maxRollbackVersions entries strictly older than the current version.
func (s *UpdateService) fetchRollbackCandidates(ctx context.Context) ([]*GitHubRelease, error) {
	releases, err := s.githubClient.FetchRecentReleases(ctx, forkRepo, rollbackFetchPageSize)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(releases))
	candidates := make([]*GitHubRelease, 0, maxRollbackVersions)
	for _, r := range releases {
		if r == nil || r.Draft || r.Prerelease {
			continue
		}
		// Only fork releases: a tag outside vX.Y.Z is not a version this
		// updater may offer, let alone install.
		version, err := parseForkReleaseTag(r.TagName)
		if err != nil {
			continue
		}
		key := version.String()
		if seen[key] {
			continue
		}
		// Only versions strictly older than current (also excludes current itself)
		if version.compare(parseRunningVersion(s.currentVersion)) >= 0 {
			continue
		}
		// A candidate must be installable on this host, on exactly the terms the
		// install step enforces: the platform archive plus checksums.txt. An
		// image-only or missing-asset release must not be offered as something an
		// administrator can roll back to and then watch fail.
		if !assessForkRelease(r.TagName, gitHubAssetNames(r.Assets), observedForkReleaseState(r)).Installable {
			continue
		}
		seen[key] = true
		candidates = append(candidates, r)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		left, leftErr := parseForkReleaseTag(candidates[i].TagName)
		right, rightErr := parseForkReleaseTag(candidates[j].TagName)
		if leftErr != nil || rightErr != nil {
			return false
		}
		return left.compare(right) > 0
	})

	if len(candidates) > maxRollbackVersions {
		candidates = candidates[:maxRollbackVersions]
	}
	return candidates, nil
}

func (s *UpdateService) fetchLatestRelease(ctx context.Context) (*UpdateInfo, forkReleaseState, error) {
	release, err := s.githubClient.FetchLatestRelease(ctx, forkRepo)
	if err != nil {
		return nil, forkReleaseState{}, err
	}
	if release == nil {
		return nil, forkReleaseState{}, fmt.Errorf("the fork release check for %s returned no release", forkRepo)
	}
	state := observedForkReleaseState(release)

	assets := make([]Asset, len(release.Assets))
	for i, a := range release.Assets {
		assets[i] = Asset{
			Name:        a.Name,
			DownloadURL: a.BrowserDownloadURL,
			Size:        a.Size,
		}
	}

	return s.updateInfoForRelease(release.TagName, &ReleaseInfo{
		TagName:     release.TagName,
		Name:        release.Name,
		Body:        release.Body,
		PublishedAt: release.PublishedAt,
		HTMLURL:     release.HTMLURL,
		Assets:      assets,
	}, state, false), state, nil
}

func (s *UpdateService) downloadFile(ctx context.Context, downloadURL, dest string) error {
	return s.githubClient.DownloadFile(ctx, downloadURL, dest, maxDownloadSize)
}

// assetNames lists the asset names of a release for contract assessment.
func assetNames(assets []Asset) []string {
	names := make([]string, 0, len(assets))
	for _, asset := range assets {
		names = append(names, asset.Name)
	}
	return names
}

// gitHubAssetNames lists the asset names of a raw GitHub release.
func gitHubAssetNames(assets []GitHubAsset) []string {
	names := make([]string, 0, len(assets))
	for _, asset := range assets {
		names = append(names, asset.Name)
	}
	return names
}

func (s *UpdateService) verifyChecksum(ctx context.Context, filePath, checksumURL string) error {
	// Download checksums file
	checksumData, err := s.githubClient.FetchChecksumFile(ctx, checksumURL, maxChecksumSize)
	if err != nil {
		return fmt.Errorf("failed to download checksums: %w", err)
	}

	// Calculate file hash
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	expectedHash, err := findChecksumForFile(checksumData, filepath.Base(filePath))
	if err != nil {
		return err
	}
	if !strings.EqualFold(expectedHash, actualHash) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

// findChecksumForFile returns the one published SHA256 digest for fileName.
//
// The checksums file decides whether an archive is installed, so it is parsed
// strictly instead of "first line that happens to match": every line must be a
// well-formed "<sha256>  <name>" pair, the digest must really be 64 hex
// characters, and a file listed twice is refused as ambiguous rather than
// resolved by picking one. A malformed or duplicated entry is an error, never a
// silent pass.
func findChecksumForFile(checksumData []byte, fileName string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(checksumData))
	scanner.Buffer(make([]byte, 0, 64*1024), maxChecksumSize)

	var found string
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) != 2 {
			return "", fmt.Errorf("malformed checksums line %d", lineNumber)
		}
		digest := parts[0]
		if !isSHA256Digest(digest) {
			return "", fmt.Errorf("checksums line %d does not hold a SHA256 digest", lineNumber)
		}
		// "sha256sum -b" marks binary mode with a leading "*".
		name := strings.TrimPrefix(parts[1], "*")
		if name != fileName {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("checksums file lists %s more than once", fileName)
		}
		found = strings.ToLower(digest)
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("failed to read checksums: %w", err)
	}
	if found == "" {
		return "", fmt.Errorf("checksum not found for %s", fileName)
	}
	return found, nil
}

// isSHA256Digest reports whether s is exactly 64 hexadecimal characters.
func isSHA256Digest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// updateCachePayload is the cached outcome of one fork update check.
//
// The payload is bound to the fork via Repo, and installability is never stored
// as a verdict: it is recomputed from ReleaseTag and the release assets on every
// read, so a stale or hand-written payload cannot conjure an update.
type updateCachePayload struct {
	Repo       string `json:"repo"`
	ReleaseTag string `json:"release_tag,omitempty"`
	// ReleaseFlagsRecorded is written only when the release's draft and
	// prerelease flags were actually observed, and the observed values are kept
	// alongside it. A payload without the marker cannot prove the release was
	// stable, so it is refused instead of being assumed stable.
	ReleaseFlagsRecorded bool         `json:"release_flags_recorded,omitempty"`
	ReleaseDraft         bool         `json:"release_draft,omitempty"`
	ReleasePrerelease    bool         `json:"release_prerelease,omitempty"`
	ReleaseInfo          *ReleaseInfo `json:"release_info,omitempty"`
	Warning              string       `json:"warning,omitempty"`
	Timestamp            int64        `json:"timestamp"`
}

func (s *UpdateService) getFromCache(ctx context.Context) (*UpdateInfo, error) {
	data, err := s.cache.GetUpdateInfo(ctx)
	if err != nil {
		return nil, err
	}

	var cached updateCachePayload
	if err := json.Unmarshal([]byte(data), &cached); err != nil {
		return nil, err
	}

	// Only a payload this updater wrote for the fork is usable. Entries written
	// while the updater still read the upstream repository carry no repo binding
	// at all, and an entry naming another repository is refused outright: a
	// failed fork check must never be answered with upstream release data.
	if cached.Repo != forkRepo {
		return nil, fmt.Errorf("cached update info is not from %s", forkRepo)
	}
	if time.Now().Unix()-cached.Timestamp > updateCacheTTL {
		return nil, fmt.Errorf("cache expired")
	}

	// A cached "nothing installable" outcome is returned as such; the reason is
	// preserved so the administrator still sees why.
	if cached.ReleaseTag == "" || cached.ReleaseInfo == nil {
		info := s.noUpdateInfo(cached.Warning)
		info.Cached = true
		return info, nil
	}

	state := forkReleaseState{
		Known:      cached.ReleaseFlagsRecorded,
		Draft:      cached.ReleaseDraft,
		Prerelease: cached.ReleasePrerelease,
	}
	info := s.updateInfoForRelease(cached.ReleaseTag, cached.ReleaseInfo, state, true)
	if info.ReleaseInfo == nil {
		// The stored tag is not a fork version: treat the whole payload as a miss
		// rather than serving a malformed release from cache.
		return nil, fmt.Errorf("cached update info holds a non-fork release tag %q", cached.ReleaseTag)
	}
	return info, nil
}

func (s *UpdateService) saveToCache(ctx context.Context, info *UpdateInfo, state forkReleaseState) {
	cacheData := updateCachePayload{
		Repo:                 forkRepo,
		ReleaseFlagsRecorded: state.Known,
		ReleaseDraft:         state.Draft,
		ReleasePrerelease:    state.Prerelease,
		Warning:              info.Warning,
		Timestamp:            time.Now().Unix(),
	}
	if info.ReleaseInfo != nil {
		cacheData.ReleaseTag = info.ReleaseInfo.TagName
		cacheData.ReleaseInfo = info.ReleaseInfo
	}

	data, err := json.Marshal(cacheData)
	if err != nil {
		return
	}
	_ = s.cache.SetUpdateInfo(ctx, string(data), time.Duration(updateCacheTTL)*time.Second)
}
