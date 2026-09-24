package releasecontract

import (
	"fmt"
	"sort"
	"strings"
)

// ChecksumsAssetName is the checksum file every installable fork binary
// release must publish. A binary release without it is treated as
// non-installable (fail closed) rather than as "probably fine": the fork's own
// download host cannot prove asset provenance, so an unverifiable archive must
// not become a one-click update.
const ChecksumsAssetName = "checksums.txt"

// ReleaseKind distinguishes a full binary release from a container-image-only
// release. An image-only release may exist, but it must never look installable
// to the in-app binary update path.
type ReleaseKind string

const (
	// KindBinary is a release carrying platform archives plus checksums.txt.
	KindBinary ReleaseKind = "binary"
	// KindImageOnly is a release carrying container images only (the
	// .goreleaser.simple.yaml path).
	KindImageOnly ReleaseKind = "image_only"
)

// ParseReleaseKind validates a workflow-provided kind.
func ParseReleaseKind(raw string) (ReleaseKind, error) {
	switch ReleaseKind(strings.TrimSpace(raw)) {
	case KindBinary:
		return KindBinary, nil
	case KindImageOnly:
		return KindImageOnly, nil
	default:
		return "", fmt.Errorf("unknown release kind %q (want %q or %q)", raw, KindBinary, KindImageOnly)
	}
}

// Platform is one GOOS/GOARCH pair.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// RequiredPlatforms is the platform set a fork binary release is expected to
// cover, matching the goos/goarch matrix in .goreleaser.yaml (windows/arm64 is
// intentionally absent there).
var RequiredPlatforms = []Platform{
	{OS: "linux", Arch: "amd64"},
	{OS: "linux", Arch: "arm64"},
	{OS: "darwin", Arch: "amd64"},
	{OS: "darwin", Arch: "arm64"},
	{OS: "windows", Arch: "amd64"},
}

// ArchiveName mirrors .goreleaser.yaml: archives are named
// sub2api_<version>_<os>_<arch> and use .zip on windows, .tar.gz elsewhere.
func ArchiveName(v Version, p Platform) string {
	ext := ".tar.gz"
	if p.OS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("sub2api_%s_%s_%s%s", v, p.OS, p.Arch, ext)
}

// ExpectedArchiveNames is the archive set a full binary release must publish.
func ExpectedArchiveNames(v Version) []string {
	out := make([]string, 0, len(RequiredPlatforms))
	for _, p := range RequiredPlatforms {
		out = append(out, ArchiveName(v, p))
	}
	return out
}

// ExpectedAssetNames is the complete asset contract for an installable binary
// release, checksums included.
func ExpectedAssetNames(v Version) []string {
	return append(ExpectedArchiveNames(v), ChecksumsAssetName)
}

// IsChecksums reports whether an asset name is the checksum file.
func IsChecksums(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), ChecksumsAssetName)
}

// AssetReport is the platform-by-platform installability of one release.
type AssetReport struct {
	ReleaseKind ReleaseKind `json:"release_kind"`
	// ChecksumsPresent is whether checksums.txt was published.
	ChecksumsPresent bool `json:"checksums_present"`
	// Installable is the strict summary: every required platform plus checksums.
	// Consumers that ask "can this platform install it?" must use
	// InstallablePlatforms instead.
	Installable bool `json:"installable"`
	// InstallablePlatforms are the platforms with a verifiable archive present.
	InstallablePlatforms []string `json:"installable_platforms"`
	// MissingPlatforms are the required platforms with no archive, or every
	// platform when the release cannot be trusted at all (no checksums).
	MissingPlatforms []string `json:"missing_platforms"`
	// Reason is the machine-readable outcome code.
	Reason string `json:"reason"`
	// Message is a human-readable explanation for logs.
	Message string `json:"message"`
}

// Reason codes shared by AssetReport and Verdict.
const (
	ReasonOK                = "ok"
	ReasonImageOnly         = "image_only"
	ReasonKindMismatch      = "kind_mismatch"
	ReasonChecksumsMissing  = "checksums_missing"
	ReasonAssetsUnverified  = "assets_unverified"
	ReasonMissingPlatforms  = "missing_platforms"
	ReasonForkMismatch      = "fork_repo_mismatch"
	ReasonBadFormat         = "bad_format"
	ReasonNotIncreasing     = "not_increasing"
	ReasonUpstreamOnly      = "upstream_only"
	ReasonUnknownEvent      = "unknown_event"
	ReasonBaselineUnknown   = "fork_baseline_unknown"
	ReasonBaselineTruncated = "fork_baseline_truncated"
	ReasonAlreadyCurrent    = "already_current"
	ReasonWouldMoveBack     = "would_move_backwards"
	ReasonPushNotPermitted  = "push_not_permitted"
)

// AssessAssets decides what a release can install, from its asset list alone.
//
// The declared release kind is authoritative, so a release declared image-only
// reports zero installable platforms even if archives were attached by mistake
// (that case is flagged as a kind mismatch instead of being quietly upgraded).
// A binary release without checksums.txt reports zero installable platforms.
func AssessAssets(kind ReleaseKind, v Version, assets []string) AssetReport {
	present := make(map[string]bool, len(assets))
	for _, a := range assets {
		present[strings.TrimSpace(a)] = true
	}

	report := AssetReport{
		ReleaseKind:          kind,
		ChecksumsPresent:     present[ChecksumsAssetName],
		InstallablePlatforms: []string{},
		MissingPlatforms:     []string{},
	}

	switch kind {
	case KindImageOnly:
		report.MissingPlatforms = platformStrings(RequiredPlatforms)
		if report.ChecksumsPresent || len(archiveAssets(present, v)) > 0 {
			report.Reason = ReasonKindMismatch
			report.Message = "release is declared image-only but carries binary assets; it must not be used for an in-app binary update until it is classified correctly"
			return report
		}
		report.Reason = ReasonImageOnly
		report.Message = "container-image-only release: no platform binary asset is published, so it is not installable on any platform"
		return report

	case KindBinary:
		if !report.ChecksumsPresent {
			report.MissingPlatforms = platformStrings(RequiredPlatforms)
			report.Reason = ReasonChecksumsMissing
			report.Message = ChecksumsAssetName + " is required for an installable binary release; a release without it is treated as non-installable"
			return report
		}
		for _, p := range RequiredPlatforms {
			if present[ArchiveName(v, p)] {
				report.InstallablePlatforms = append(report.InstallablePlatforms, p.String())
				continue
			}
			report.MissingPlatforms = append(report.MissingPlatforms, p.String())
		}
		if len(report.MissingPlatforms) == 0 {
			report.Installable = true
			report.Reason = ReasonOK
			report.Message = "binary release publishes every required platform archive and " + ChecksumsAssetName
			return report
		}
		report.Reason = ReasonMissingPlatforms
		report.Message = "binary release is missing platform archives: " + strings.Join(report.MissingPlatforms, ", ")
		return report

	default:
		report.MissingPlatforms = platformStrings(RequiredPlatforms)
		report.Reason = ReasonKindMismatch
		report.Message = fmt.Sprintf("unknown release kind %q", kind)
		return report
	}
}

// UnverifiedAssetsReport is the verdict when the published asset list could not
// be read at all.
//
// It exists so an unreadable release can never be mistaken for one that was
// read and found to carry no binary assets: an empty list is a fact only when
// the read succeeded. Nothing installable is claimed here, and the caller is
// expected to treat this as a failed verification rather than as a
// classification.
func UnverifiedAssetsReport(kind ReleaseKind) AssetReport {
	return AssetReport{
		ReleaseKind:          kind,
		InstallablePlatforms: []string{},
		MissingPlatforms:     platformStrings(RequiredPlatforms),
		Reason:               ReasonAssetsUnverified,
		Message:              "the published asset list could not be read, so asset verification was not completed; this run makes no claim about installability, and the release must not be treated as classified",
	}
}

func archiveAssets(present map[string]bool, v Version) []string {
	var out []string
	for _, p := range RequiredPlatforms {
		if name := ArchiveName(v, p); present[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func platformStrings(ps []Platform) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}
