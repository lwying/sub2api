package releasecontract

import (
	"fmt"
	"strings"
)

// ForkRepo is the repository whose Releases are the only source of installable
// fork versions. Local git tags are not a substitute: this working tree also
// carries tags fetched from the upstream remote, so a baseline read from tags
// would treat upstream versions as published fork releases.
const ForkRepo = "lwying/sub2api"

// UpstreamRepo is the repository the fork syncs from. Its versions must never
// become fork releases on their own.
const UpstreamRepo = "Wei-Shaw/sub2api"

// Release entry points, as reported by the workflow.
const (
	EventPush     = "push"
	EventDispatch = "workflow_dispatch"
)

// ReleaseInput is the release gate's input. Every field is supplied by the
// workflow (or by a test fixture); no field is discovered by this package.
type ReleaseInput struct {
	// Event is EventPush or EventDispatch.
	Event string `json:"event"`
	// CandidateTag is the tag the release was triggered with, e.g. "v0.2.8".
	CandidateTag string `json:"candidate_tag"`
	// ForkRepo must equal ForkRepo.
	ForkRepo string `json:"fork_repo"`
	// ForkPublishedVersions are versions the fork has actually released, read
	// from its own Releases. Tags fetched from the upstream remote must not be
	// listed here.
	ForkPublishedVersions []string `json:"fork_published_versions"`
	// UpstreamVersions are versions known to exist upstream. A candidate found
	// here but not in ForkPublishedVersions arrived with an upstream sync.
	UpstreamVersions []string `json:"upstream_versions"`
	// CurrentVersionFile is the fork-owned VERSION file contents in the
	// checked-out tree, e.g. "0.2.6".
	CurrentVersionFile string `json:"current_version_file"`
	// ReleaseKind is the kind this run intends to publish.
	ReleaseKind ReleaseKind `json:"release_kind"`
	// Assets is the published asset list, when known. Empty means the release
	// has not been built yet, so only the version gate is evaluated.
	Assets []string `json:"assets"`
	// AssetsUnverified is set when the release asset list could not be read.
	// It must not be modelled as an empty list: "could not be read" and "read
	// and found empty" are different facts, and only the second one may be
	// reported as an image-only classification.
	AssetsUnverified bool `json:"assets_unverified"`
	// ForkBaselineUnknown is set when the fork's own Releases could not be read.
	// The gate then refuses to release instead of assuming an empty baseline: a
	// failed lookup must never be read as "the fork has published nothing".
	ForkBaselineUnknown bool `json:"fork_baseline_unknown"`
	// ForkBaselineTruncated is set when the release listing may have been cut
	// off by the API page cap. A possibly incomplete list must never be read as
	// a complete baseline: the dropped entry could be the highest released
	// version, which is exactly the number the gate compares against.
	ForkBaselineTruncated bool `json:"fork_baseline_truncated"`
	// UpstreamVersionsUnknown is set when the upstream tag list could not be
	// read. That only degrades the upstream-carried-version check, so it is
	// reported as a verdict warning rather than a rejection.
	UpstreamVersionsUnknown bool `json:"upstream_versions_unknown"`
}

// Verdict is the release gate's decision.
type Verdict struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
	// Version is the normalised X.Y.Z form, empty when the input was unusable.
	Version string `json:"version"`
	// Tag is the normalised vX.Y.Z form.
	Tag         string      `json:"tag"`
	ReleaseKind ReleaseKind `json:"release_kind"`
	// Baseline is the highest version the fork is already known to have
	// published (or the fork-owned VERSION file, if higher). Empty when the fork
	// has never published.
	Baseline string `json:"baseline"`
	// ExpectedAssets is the asset contract the publish job must satisfy.
	ExpectedAssets []string `json:"expected_assets"`
	// Assets is the installability report; nil until the release has assets.
	Assets *AssetReport `json:"assets_report,omitempty"`
	// Warnings are non-fatal observations worth surfacing in the job log.
	Warnings []string `json:"warnings,omitempty"`
}

// Evaluate decides whether the input may become a new fork release. It is
// deliberately fail-closed: anything it cannot positively verify is rejected.
func Evaluate(in ReleaseInput) Verdict {
	if strings.TrimSpace(in.ForkRepo) != ForkRepo {
		return Verdict{
			OK:      false,
			Code:    ReasonForkMismatch,
			Message: fmt.Sprintf("release source must be %s, got %q: fork releases may only be produced from the fork repository", ForkRepo, in.ForkRepo),
		}
	}
	if in.Event != EventPush && in.Event != EventDispatch {
		return Verdict{
			OK:      false,
			Code:    ReasonUnknownEvent,
			Message: fmt.Sprintf("unsupported release trigger %q", in.Event),
		}
	}
	if _, err := ParseReleaseKind(string(in.ReleaseKind)); err != nil {
		return Verdict{OK: false, Code: ReasonKindMismatch, Message: err.Error()}
	}

	v, err := ParseTag(in.CandidateTag)
	if err != nil {
		return Verdict{
			OK:      false,
			Code:    ReasonBadFormat,
			Message: err.Error() + ": fork releases use an independent three-segment numeric version",
		}
	}

	if in.ForkBaselineUnknown {
		return Verdict{
			OK:      false,
			Code:    ReasonBaselineUnknown,
			Message: "the fork's published versions could not be read, so the release baseline is unknown: refusing to release rather than assume the fork has published nothing",
			Version: v.String(),
			Tag:     v.Tag(),
		}
	}

	if in.ForkBaselineTruncated {
		return Verdict{
			OK:      false,
			Code:    ReasonBaselineTruncated,
			Message: "the fork's release listing reached the API page cap, so the baseline may be missing a higher released version: raise the page size or verify the baseline before releasing",
			Version: v.String(),
			Tag:     v.Tag(),
		}
	}

	var warnings []string
	if in.UpstreamVersionsUnknown {
		warnings = append(warnings, "the upstream tag list could not be read, so the upstream-carried-version check did not run for this release (the fork baseline check still applied)")
	}
	var invalid []string
	baseline, hasBaseline := highestVersion(in.ForkPublishedVersions, &invalid)
	if len(invalid) > 0 {
		warnings = append(warnings, "ignored unparseable fork published versions: "+strings.Join(invalid, ", "))
	}
	if fileV, err := ParseVersion(in.CurrentVersionFile); err == nil {
		if !hasBaseline || fileV.Compare(baseline) > 0 {
			baseline = fileV
			hasBaseline = true
		}
	} else if strings.TrimSpace(in.CurrentVersionFile) != "" {
		warnings = append(warnings, fmt.Sprintf("fork-owned VERSION file %q is not a valid version and was not used as a baseline", in.CurrentVersionFile))
	}

	verdict := Verdict{
		Version:        v.String(),
		Tag:            v.Tag(),
		ReleaseKind:    in.ReleaseKind,
		ExpectedAssets: ExpectedAssetNames(v),
		Warnings:       warnings,
	}
	if hasBaseline {
		verdict.Baseline = baseline.String()
	}

	if hasBaseline && v.Compare(baseline) <= 0 {
		verdict.OK = false
		verdict.Code = ReasonNotIncreasing
		verdict.Message = fmt.Sprintf("%s is not above the fork release baseline %s: only a strictly increasing fork version may be released", v.Tag(), baseline)
		return verdict
	}

	// A version that exists upstream but was never released by the fork arrived
	// through an upstream sync (merge or VERSION bump), not through a fork
	// release decision. Pick a version above it instead.
	if containsVersion(in.UpstreamVersions, v) && !containsVersion(in.ForkPublishedVersions, v) {
		verdict.OK = false
		verdict.Code = ReasonUpstreamOnly
		verdict.Message = fmt.Sprintf("%s exists on %s but is not a published fork release: an upstream-carried version must not be released as a fork version", v.Tag(), UpstreamRepo)
		return verdict
	}

	if in.AssetsUnverified {
		report := UnverifiedAssetsReport(in.ReleaseKind)
		verdict.Assets = &report
	} else if len(in.Assets) > 0 {
		report := AssessAssets(in.ReleaseKind, v, in.Assets)
		verdict.Assets = &report
	}

	verdict.OK = true
	verdict.Code = ReasonOK
	verdict.Message = fmt.Sprintf("%s is a valid new fork release on %s", v.Tag(), ForkRepo)
	return verdict
}

func containsVersion(raws []string, want Version) bool {
	for _, raw := range raws {
		v, err := ParseVersion(raw)
		if err != nil {
			continue
		}
		if v.Compare(want) == 0 {
			return true
		}
	}
	return false
}

// SyncInput is the input of the VERSION writeback decision.
type SyncInput struct {
	// CandidateTag is the released tag.
	CandidateTag string `json:"candidate_tag"`
	// CurrentVersionFile is the fork-owned VERSION file in the target branch.
	CurrentVersionFile string `json:"current_version_file"`
	// AllowDefaultBranchPush mirrors the maintainer-controlled opt-in. When it
	// is false the writeback is skipped instead of forcing a push onto a branch
	// that may be protected.
	AllowDefaultBranchPush bool `json:"allow_default_branch_push"`
}

// SyncVerdict decides whether the workflow may write VERSION for a released tag.
//
// Two rules matter for a protected default branch: the fork-owned VERSION file
// may only ever move forward, and the push itself needs an explicit opt-in.
// A skipped writeback is a success with an explanation, never a silent no-op and
// never a forced push.
type SyncVerdict struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Version string `json:"version"`
	// ShouldWrite is whether the job should modify VERSION at all.
	ShouldWrite bool `json:"should_write"`
	// PushPermitted is whether the job may push the change to the default branch.
	PushPermitted bool `json:"push_permitted"`
}

// DecideSyncVersion evaluates SyncInput.
func DecideSyncVersion(in SyncInput) SyncVerdict {
	v, err := ParseTag(in.CandidateTag)
	if err != nil {
		return SyncVerdict{
			OK:      false,
			Code:    ReasonBadFormat,
			Message: err.Error(),
		}
	}

	current, curErr := ParseVersion(in.CurrentVersionFile)
	switch {
	case curErr != nil:
		// No usable baseline in the file: writing the released version forward is
		// still the only safe direction, so allow the write but say why.
		verdict := SyncVerdict{
			OK:            true,
			Version:       v.String(),
			ShouldWrite:   true,
			PushPermitted: in.AllowDefaultBranchPush,
			Message:       fmt.Sprintf("fork-owned VERSION file %q is not a valid version; writing %s forward", in.CurrentVersionFile, v.String()),
		}
		if !in.AllowDefaultBranchPush {
			verdict.Code = ReasonPushNotPermitted
			verdict.Message = notPermittedMessage(v)
			verdict.ShouldWrite = false
		} else {
			verdict.Code = ReasonOK
		}
		return verdict
	case current.Compare(v) == 0:
		return SyncVerdict{
			OK:            true,
			Code:          ReasonAlreadyCurrent,
			Message:       fmt.Sprintf("fork-owned VERSION file already records %s", v.String()),
			Version:       v.String(),
			PushPermitted: in.AllowDefaultBranchPush,
		}
	case current.Compare(v) > 0:
		return SyncVerdict{
			OK:      false,
			Code:    ReasonWouldMoveBack,
			Message: fmt.Sprintf("refusing to write %s over the fork-owned VERSION file %s: the version file must only move forward", v.String(), current.String()),
			Version: v.String(),
		}
	}

	if !in.AllowDefaultBranchPush {
		return SyncVerdict{
			OK:      true,
			Code:    ReasonPushNotPermitted,
			Message: notPermittedMessage(v),
			Version: v.String(),
		}
	}
	return SyncVerdict{
		OK:            true,
		Code:          ReasonOK,
		Message:       fmt.Sprintf("writing fork-owned VERSION file forward to %s", v.String()),
		Version:       v.String(),
		ShouldWrite:   true,
		PushPermitted: true,
	}
}

func notPermittedMessage(v Version) string {
	return fmt.Sprintf("skipped writing %s to the default branch: %s is not enabled, so the version writeback stays off a possibly protected branch and must be applied through the maintainer-controlled PR path", v.String(), DefaultBranchPushVar)
}

// DefaultBranchPushVar is the repository variable that opts the VERSION
// writeback into pushing the default branch directly.
const DefaultBranchPushVar = "FORK_ALLOW_DEFAULT_BRANCH_VERSION_PUSH"
