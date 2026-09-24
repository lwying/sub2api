//go:build unit

// Behaviour tests for the fork release contract. Every case is driven by a
// local fixture under testdata/, so the accepted/rejected decisions can be
// replayed offline with no git tag, no release and no GitHub access.
package releasecontract

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func decodeFixture[T any](t *testing.T, name string) T {
	t.Helper()
	var out T
	if err := DecodeInputFile(filepath.Join("testdata", name), &out); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return out
}

func TestParseTagAcceptsOnlyThreeNumericSegments(t *testing.T) {
	valid := map[string]string{
		"v0.0.1":     "0.0.1",
		"v1.2.3":     "1.2.3",
		"v0.2.8":     "0.2.8",
		"v10.20.30":  "10.20.30",
		"v0.2.6\r\n": "0.2.6",
	}
	for raw, want := range valid {
		v, err := ParseTag(raw)
		if err != nil {
			t.Errorf("ParseTag(%q) rejected a valid fork version: %v", raw, err)
			continue
		}
		if v.String() != want {
			t.Errorf("ParseTag(%q) = %q, want %q", raw, v.String(), want)
		}
		if v.Tag() != "v"+want {
			t.Errorf("ParseTag(%q).Tag() = %q, want %q", raw, v.Tag(), "v"+want)
		}
	}

	invalid := []string{
		"",
		"0.2.8",       // missing the v prefix
		"V0.2.8",      // wrong case
		"v0.2",        // two segments
		"v0.2.8.1",    // four segments
		"v0.2.8-rc.1", // prerelease suffix
		"v0.2.8+build",
		"v01.2.8", // leading zero
		"v0.02.8", // leading zero
		"v0.2.08", // leading zero
		"vx.y.z",  // not numeric
		"v0.2.x",  // not numeric
		"v-1.2.3", // negative
		"release0.2.8",
		"v",     // nothing
		"v0..8", // empty segment
	}
	for _, raw := range invalid {
		if v, err := ParseTag(raw); err == nil {
			t.Errorf("ParseTag(%q) accepted an invalid fork version as %q", raw, v.String())
		}
	}

	// A bare X.Y.Z is accepted where the version is not a tag (VERSION file,
	// release API), but never as a tag.
	if v, err := ParseVersion("0.2.6"); err != nil || v.String() != "0.2.6" {
		t.Errorf("ParseVersion(0.2.6) = %v, %v; want 0.2.6", v, err)
	}
	if v, err := ParseVersion("v0.2.6"); err != nil || v.String() != "0.2.6" {
		t.Errorf("ParseVersion(v0.2.6) = %v, %v; want 0.2.6", v, err)
	}
}

func TestVersionOrdering(t *testing.T) {
	ordered := []string{"v0.2.6", "v0.2.7", "v0.2.10", "v0.3.0", "v1.0.0", "v10.0.0"}
	for i := 1; i < len(ordered); i++ {
		lo, err := ParseTag(ordered[i-1])
		if err != nil {
			t.Fatalf("ParseTag(%q): %v", ordered[i-1], err)
		}
		hi, err := ParseTag(ordered[i])
		if err != nil {
			t.Fatalf("ParseTag(%q): %v", ordered[i], err)
		}
		if lo.Compare(hi) >= 0 {
			t.Errorf("%s must sort below %s", ordered[i-1], ordered[i])
		}
		if hi.Compare(lo) <= 0 {
			t.Errorf("%s must sort above %s", ordered[i], ordered[i-1])
		}
	}
}

// TestReleaseGateDecisions covers acceptance criterion 1 for both entry points.
func TestReleaseGateRejectsUnknownReleaseKind(t *testing.T) {
	in := decodeFixture[ReleaseInput](t, "gate_ok.json")
	in.ReleaseKind = ReleaseKind("surprise")

	verdict := Evaluate(in)

	if verdict.OK || verdict.Code != ReasonKindMismatch {
		t.Fatalf("unknown release kind must fail with kind_mismatch: %+v", verdict)
	}
}

func TestReleaseGateDecisions(t *testing.T) {
	cases := []struct {
		fixture      string
		wantOK       bool
		wantCode     string
		wantVersion  string
		wantBaseline string
	}{
		{fixture: "gate_ok.json", wantOK: true, wantCode: ReasonOK, wantVersion: "0.2.8", wantBaseline: "0.2.6"},
		{fixture: "gate_upstream_only.json", wantOK: false, wantCode: ReasonUpstreamOnly, wantVersion: "0.2.7", wantBaseline: "0.2.6"},
		{fixture: "gate_not_increasing.json", wantOK: false, wantCode: ReasonNotIncreasing, wantVersion: "0.2.5", wantBaseline: "0.2.6"},
		{fixture: "gate_bad_format.json", wantOK: false, wantCode: ReasonBadFormat},
		{fixture: "gate_foreign_repo.json", wantOK: false, wantCode: ReasonForkMismatch},
		{fixture: "gate_baseline_unknown.json", wantOK: false, wantCode: ReasonBaselineUnknown, wantVersion: "0.2.8"},
		{fixture: "gate_baseline_truncated.json", wantOK: false, wantCode: ReasonBaselineTruncated, wantVersion: "0.2.8"},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			verdict := Evaluate(decodeFixture[ReleaseInput](t, tc.fixture))
			if verdict.OK != tc.wantOK {
				t.Errorf("ok = %v, want %v (code %s: %s)", verdict.OK, tc.wantOK, verdict.Code, verdict.Message)
			}
			if verdict.Code != tc.wantCode {
				t.Errorf("code = %q, want %q (%s)", verdict.Code, tc.wantCode, verdict.Message)
			}
			if want := tc.wantVersion; want != "" && verdict.Version != want {
				t.Errorf("version = %q, want %q", verdict.Version, want)
			}
			if want := tc.wantBaseline; want != "" && verdict.Baseline != want {
				t.Errorf("baseline = %q, want %q", verdict.Baseline, want)
			}
		})
	}
}

// TestUpstreamVersionsMustNotBecomeForkReleases pins the trap in the real
// working tree: this checkout carries tags fetched from the upstream remote, so
// a baseline taken from local tags would treat upstream versions as published
// fork releases. The gate must reject a version that only upstream has, even
// when it is strictly higher than the fork's own baseline.
func TestUpstreamVersionsMustNotBecomeForkReleases(t *testing.T) {
	in := ReleaseInput{
		Event:                 EventPush,
		CandidateTag:          "v0.2.7",
		ForkRepo:              ForkRepo,
		ForkPublishedVersions: []string{"v0.2.6"},
		// The upstream tag set observed in this working tree.
		UpstreamVersions:   []string{"v0.1.172", "v0.1.185", "v0.2.0", "v0.2.5", "v0.2.7"},
		CurrentVersionFile: "0.2.6",
		ReleaseKind:        KindBinary,
	}

	verdict := Evaluate(in)
	if verdict.OK {
		t.Fatalf("v0.2.7 is only an upstream version and must not be released by the fork")
	}
	if verdict.Code != ReasonUpstreamOnly {
		t.Errorf("code = %q, want %q", verdict.Code, ReasonUpstreamOnly)
	}
	if !strings.Contains(verdict.Message, UpstreamRepo) {
		t.Errorf("message should name the upstream repository, got %q", verdict.Message)
	}

	// The same candidate must be rejected through the manual dispatch entry too.
	in.Event = EventDispatch
	if dispatched := Evaluate(in); dispatched.OK {
		t.Errorf("workflow_dispatch accepted the same upstream-carried version")
	}

	// Choosing a version above the upstream one is the way out.
	in.CandidateTag = "v0.3.0"
	if ok := Evaluate(in); !ok.OK {
		t.Errorf("v0.3.0 must be accepted as a fork release, got %s: %s", ok.Code, ok.Message)
	}
}

// TestReleaseGateRejectsUnusableEventAndEmptyBaseline documents the fail-closed
// edge cases.
func TestReleaseGateRejectsUnusableEventAndEmptyBaseline(t *testing.T) {
	base := ReleaseInput{
		Event:              EventPush,
		CandidateTag:       "v0.2.8",
		ForkRepo:           ForkRepo,
		CurrentVersionFile: "0.2.6",
		ReleaseKind:        KindBinary,
	}

	unknown := base
	unknown.Event = "schedule"
	if got := Evaluate(unknown); got.OK || got.Code != ReasonUnknownEvent {
		t.Errorf("unknown trigger: got ok=%v code=%q, want rejected %q", got.OK, got.Code, ReasonUnknownEvent)
	}

	// With no published fork release and no usable VERSION file there is no
	// baseline: the first fork release is allowed, but the expected asset
	// contract is still reported for the publish job.
	first := base
	first.CurrentVersionFile = ""
	verdict := Evaluate(first)
	if !verdict.OK {
		t.Fatalf("first fork release must be allowed, got %s: %s", verdict.Code, verdict.Message)
	}
	if verdict.Baseline != "" {
		t.Errorf("baseline = %q, want empty when the fork has never published", verdict.Baseline)
	}
	if len(verdict.ExpectedAssets) != len(RequiredPlatforms)+1 {
		t.Errorf("expected assets = %d, want %d archives plus checksums", len(verdict.ExpectedAssets), len(RequiredPlatforms))
	}
	if !strings.Contains(strings.Join(verdict.ExpectedAssets, ","), ChecksumsAssetName) {
		t.Errorf("expected assets must include %s, got %v", ChecksumsAssetName, verdict.ExpectedAssets)
	}
}

// TestUnverifiedUpstreamListIsReportedNotSilent keeps a degraded check visible.
// When the upstream tag list cannot be read the upstream-carried-version check
// does not run; the verdict must record that, instead of implying it passed.
// This does not block the release, because the fork baseline check still ran.
func TestUnverifiedUpstreamListIsReportedNotSilent(t *testing.T) {
	verdict := Evaluate(ReleaseInput{
		Event:                   EventPush,
		CandidateTag:            "v0.2.8",
		ForkRepo:                ForkRepo,
		ForkPublishedVersions:   []string{"v0.2.6"},
		CurrentVersionFile:      "0.2.6",
		ReleaseKind:             KindBinary,
		UpstreamVersionsUnknown: true,
	})
	if !verdict.OK {
		t.Fatalf("an unreadable upstream list must not block a release, got %s: %s", verdict.Code, verdict.Message)
	}
	if len(verdict.Warnings) == 0 {
		t.Fatalf("the verdict must record that the upstream-carried-version check did not run")
	}
	if !strings.Contains(strings.Join(verdict.Warnings, " "), "upstream") {
		t.Errorf("warning should name the skipped upstream check, got %v", verdict.Warnings)
	}
}

// TestAssetInstallability covers acceptance criterion 2: a full binary release
// is installable, a release without checksums.txt is not, a partial release is
// installable only where it has an archive, and an image-only release is never
// installable on any platform.
func TestAssetInstallability(t *testing.T) {
	cases := []struct {
		fixture       string
		wantOK        bool
		wantCode      string
		wantPlatforms []string
		wantMissing   []string
		wantChecksums bool
	}{
		{
			fixture:       "assets_binary_ok.json",
			wantOK:        true,
			wantCode:      ReasonOK,
			wantPlatforms: []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"},
			wantMissing:   nil,
			wantChecksums: true,
		},
		{
			fixture:       "assets_binary_no_checksums.json",
			wantOK:        false,
			wantCode:      ReasonChecksumsMissing,
			wantPlatforms: []string{},
			wantMissing:   []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"},
			wantChecksums: false,
		},
		{
			fixture:       "assets_binary_missing_windows.json",
			wantOK:        false,
			wantCode:      ReasonMissingPlatforms,
			wantPlatforms: []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"},
			wantMissing:   []string{"windows/amd64"},
			wantChecksums: true,
		},
		{
			fixture:       "assets_image_only.json",
			wantOK:        false,
			wantCode:      ReasonImageOnly,
			wantPlatforms: []string{},
			wantMissing:   []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"},
			wantChecksums: false,
		},
		{
			fixture:       "assets_image_only_with_archives.json",
			wantOK:        false,
			wantCode:      ReasonKindMismatch,
			wantPlatforms: []string{},
			wantMissing:   []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"},
			wantChecksums: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			in := decodeFixture[ReleaseInput](t, tc.fixture)
			kind, err := ParseReleaseKind(string(in.ReleaseKind))
			if err != nil {
				t.Fatalf("decode kind: %v", err)
			}
			v, err := ParseVersion(in.CandidateTag)
			if err != nil {
				t.Fatalf("decode version: %v", err)
			}
			report := AssessAssets(kind, v, in.Assets)
			if report.Installable != tc.wantOK {
				t.Errorf("installable = %v, want %v (%s)", report.Installable, tc.wantOK, report.Message)
			}
			if report.Reason != tc.wantCode {
				t.Errorf("reason = %q, want %q (%s)", report.Reason, tc.wantCode, report.Message)
			}
			if got := strings.Join(report.InstallablePlatforms, ","); got != strings.Join(tc.wantPlatforms, ",") {
				t.Errorf("installable platforms = %q, want %q", got, strings.Join(tc.wantPlatforms, ","))
			}
			if got := strings.Join(report.MissingPlatforms, ","); got != strings.Join(tc.wantMissing, ",") {
				t.Errorf("missing platforms = %q, want %q", got, strings.Join(tc.wantMissing, ","))
			}
			if report.ChecksumsPresent != tc.wantChecksums {
				t.Errorf("checksums present = %v, want %v", report.ChecksumsPresent, tc.wantChecksums)
			}
		})
	}
}

// TestUnreadableAssetListIsNotAnImageOnlyClassification pins the difference
// between "the asset list could not be read" and "the asset list was read and
// is empty". Collapsing the first into the second would let a failed
// verification be reported as a verified image-only release.
func TestUnreadableAssetListIsNotAnImageOnlyClassification(t *testing.T) {
	in := decodeFixture[ReleaseInput](t, "assets_unverified.json")

	kind, err := ParseReleaseKind(string(in.ReleaseKind))
	if err != nil {
		t.Fatalf("decode kind: %v", err)
	}
	report := UnverifiedAssetsReport(kind)
	if report.Reason != ReasonAssetsUnverified {
		t.Errorf("reason = %q, want %q", report.Reason, ReasonAssetsUnverified)
	}
	if report.Reason == ReasonImageOnly {
		t.Fatalf("an unreadable list must never be classified as image-only")
	}
	if report.Installable || len(report.InstallablePlatforms) != 0 {
		t.Errorf("an unverified release must claim nothing installable, got %+v", report)
	}
	if len(report.MissingPlatforms) != len(RequiredPlatforms) {
		t.Errorf("missing platforms = %v, want every required platform", report.MissingPlatforms)
	}

	// The same situation through the gate must carry the unverified report, not
	// an image-only one.
	gateInput := in
	gateInput.Event = EventPush
	gateInput.ForkRepo = ForkRepo
	gateInput.ForkPublishedVersions = []string{"v0.2.6"}
	gateInput.CurrentVersionFile = "0.2.6"
	verdict := Evaluate(gateInput)
	if !verdict.OK {
		t.Fatalf("the version gate should still pass, got %s: %s", verdict.Code, verdict.Message)
	}
	if verdict.Assets == nil || verdict.Assets.Reason != ReasonAssetsUnverified {
		t.Errorf("gate assets report = %+v, want reason %q", verdict.Assets, ReasonAssetsUnverified)
	}

	// And the CLI must reject it rather than print an image-only success.
	var stdout bytes.Buffer
	if got := Run([]string{"assets", "-input", fixturePath("assets_unverified.json")}, &stdout); got != ExitRejected {
		t.Errorf("exit = %d, want %d; output %s", got, ExitRejected, stdout.String())
	}
	var payload struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.OK || payload.Reason != ReasonAssetsUnverified {
		t.Errorf("verdict = %+v, want ok=false reason=%q", payload, ReasonAssetsUnverified)
	}
}

func TestArchiveNamingMatchesGoreleaser(t *testing.T) {
	v, err := ParseTag("v0.2.8")
	if err != nil {
		t.Fatalf("ParseTag: %v", err)
	}
	want := map[Platform]string{
		{OS: "linux", Arch: "amd64"}:   "sub2api_0.2.8_linux_amd64.tar.gz",
		{OS: "linux", Arch: "arm64"}:   "sub2api_0.2.8_linux_arm64.tar.gz",
		{OS: "darwin", Arch: "amd64"}:  "sub2api_0.2.8_darwin_amd64.tar.gz",
		{OS: "darwin", Arch: "arm64"}:  "sub2api_0.2.8_darwin_arm64.tar.gz",
		{OS: "windows", Arch: "amd64"}: "sub2api_0.2.8_windows_amd64.zip",
	}
	for platform, name := range want {
		if got := ArchiveName(v, platform); got != name {
			t.Errorf("ArchiveName(%s) = %q, want %q", platform, got, name)
		}
	}
	if len(ExpectedAssetNames(v)) != len(want)+1 {
		t.Errorf("expected %d archives plus checksums, got %v", len(want), ExpectedAssetNames(v))
	}
	if !IsChecksums(ChecksumsAssetName) || !IsChecksums(" CHECKSUMS.TXT ") {
		t.Errorf("IsChecksums must match %s case-insensitively", ChecksumsAssetName)
	}
	if IsChecksums("sub2api_0.2.8_linux_amd64.tar.gz") {
		t.Errorf("IsChecksums must not match an archive")
	}
}

// TestVersionWritebackDecisions covers acceptance criterion 3: the writeback
// may only move the fork-owned VERSION file forward, and it stays off a
// possibly protected default branch unless the maintainer opts in.
func TestVersionWritebackDecisions(t *testing.T) {
	cases := []struct {
		fixture   string
		wantOK    bool
		wantCode  string
		wantWrite bool
		wantPush  bool
	}{
		{fixture: "sync_ok.json", wantOK: true, wantCode: ReasonOK, wantWrite: true, wantPush: true},
		{fixture: "sync_push_not_permitted.json", wantOK: true, wantCode: ReasonPushNotPermitted, wantWrite: false, wantPush: false},
		{fixture: "sync_backwards.json", wantOK: false, wantCode: ReasonWouldMoveBack, wantWrite: false, wantPush: false},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			verdict := DecideSyncVersion(decodeFixture[SyncInput](t, tc.fixture))
			if verdict.OK != tc.wantOK {
				t.Errorf("ok = %v, want %v (%s)", verdict.OK, tc.wantOK, verdict.Message)
			}
			if verdict.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", verdict.Code, tc.wantCode)
			}
			if verdict.ShouldWrite != tc.wantWrite {
				t.Errorf("should_write = %v, want %v", verdict.ShouldWrite, tc.wantWrite)
			}
			if verdict.PushPermitted != tc.wantPush {
				t.Errorf("push_permitted = %v, want %v", verdict.PushPermitted, tc.wantPush)
			}
		})
	}

	// Already in sync is a success with nothing to do, not a rewrite.
	synced := DecideSyncVersion(SyncInput{CandidateTag: "v0.2.6", CurrentVersionFile: "0.2.6", AllowDefaultBranchPush: true})
	if !synced.OK || synced.Code != ReasonAlreadyCurrent || synced.ShouldWrite {
		t.Errorf("already-current file: got ok=%v code=%q should_write=%v", synced.OK, synced.Code, synced.ShouldWrite)
	}

	// A malformed release tag can never drive the writeback.
	bad := DecideSyncVersion(SyncInput{CandidateTag: "0.2.6", CurrentVersionFile: "0.2.5", AllowDefaultBranchPush: true})
	if bad.OK || bad.Code != ReasonBadFormat {
		t.Errorf("malformed tag: got ok=%v code=%q, want rejected %q", bad.OK, bad.Code, ReasonBadFormat)
	}
}

// TestGateCLIExitCodesAndJSON pins the external seam the release workflow
// consumes: argv in, JSON verdict plus exit code out.
func TestGateCLIExitCodesAndJSON(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantExit int
		wantCode string
	}{
		{"accepted", []string{"gate", "-input", fixturePath("gate_ok.json")}, ExitOK, ReasonOK},
		{"upstream carried", []string{"gate", "-input", fixturePath("gate_upstream_only.json")}, ExitRejected, ReasonUpstreamOnly},
		{"not increasing", []string{"gate", "-input", fixturePath("gate_not_increasing.json")}, ExitRejected, ReasonNotIncreasing},
		{"bad format", []string{"gate", "-input", fixturePath("gate_bad_format.json")}, ExitRejected, ReasonBadFormat},
		{"binary assets ok", []string{"assets", "-input", fixturePath("assets_binary_ok.json")}, ExitOK, ReasonOK},
		{"image only", []string{"assets", "-input", fixturePath("assets_image_only.json")}, ExitRejected, ReasonImageOnly},
		{"writeback skipped", []string{"sync-version", "-input", fixturePath("sync_push_not_permitted.json")}, ExitOK, ReasonPushNotPermitted},
		{"writeback backwards", []string{"sync-version", "-input", fixturePath("sync_backwards.json")}, ExitRejected, ReasonWouldMoveBack},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if got := Run(tc.args, &stdout); got != tc.wantExit {
				t.Errorf("exit = %d, want %d (output %s)", got, tc.wantExit, stdout.String())
			}
			var payload struct {
				OK   bool   `json:"ok"`
				Code string `json:"code"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
				t.Fatalf("stdout is not a JSON verdict: %v\n%s", err, stdout.String())
			}
			if payload.Code != tc.wantCode {
				t.Errorf("verdict code = %q, want %q", payload.Code, tc.wantCode)
			}
			if payload.OK != (tc.wantExit == ExitOK) {
				t.Errorf("verdict ok = %v but exit = %d", payload.OK, tc.wantExit)
			}
		})
	}
}

func TestGateCLIUsageErrors(t *testing.T) {
	var stdout bytes.Buffer
	if got := Run(nil, &stdout); got != ExitUsage {
		t.Errorf("no args: exit = %d, want %d", got, ExitUsage)
	}
	stdout.Reset()
	if got := Run([]string{"nonsense"}, &stdout); got != ExitUsage {
		t.Errorf("unknown subcommand: exit = %d, want %d", got, ExitUsage)
	}
	stdout.Reset()
	if got := Run([]string{"gate"}, &stdout); got != ExitUsage {
		t.Errorf("missing -input: exit = %d, want %d", got, ExitUsage)
	}
	stdout.Reset()
	if got := Run([]string{"gate", "-input", filepath.Join("testdata", "does-not-exist.json")}, &stdout); got != ExitUsage {
		t.Errorf("missing file: exit = %d, want %d", got, ExitUsage)
	}
}

// TestGateWritesVerdictFile checks the -out path the workflow relies on to pass
// the normalised version and release kind to later jobs.
func TestGateWritesVerdictFile(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "result.json")
	var stdout bytes.Buffer
	if got := Run([]string{"gate", "-input", fixturePath("gate_ok.json"), "-out", outPath}, &stdout); got != ExitOK {
		t.Fatalf("exit = %d, want %d", got, ExitOK)
	}
	var verdict Verdict
	if err := DecodeInputFile(outPath, &verdict); err != nil {
		t.Fatalf("decode -out file: %v", err)
	}
	if verdict.Version != "0.2.8" || verdict.Tag != "v0.2.8" || !verdict.OK {
		t.Errorf("written verdict = %+v", verdict)
	}
	if len(verdict.ExpectedAssets) == 0 {
		t.Errorf("written verdict must carry the expected asset contract")
	}
}

func fixturePath(name string) string {
	return filepath.Join("testdata", name)
}
