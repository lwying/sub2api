//go:build unit

// Structured (parsed, not text-scanned) validation of the release entry points.
// A release.yml that does not parse as YAML, or a goreleaser config whose asset
// contract drifted, fails here instead of in CI at release time.
package releasecontract

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflowStepWith struct {
	Ref string `yaml:"ref"`
}

type workflowStep struct {
	Name             string            `yaml:"name"`
	If               string            `yaml:"if"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	Env              map[string]string `yaml:"env"`
	WorkingDirectory string            `yaml:"working-directory"`
	With             workflowStepWith  `yaml:"with"`
}

type workflowJob struct {
	Needs []string       `yaml:"needs"`
	If    string         `yaml:"if"`
	Steps []workflowStep `yaml:"steps"`
}

type workflowConcurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

type workflowDoc struct {
	Jobs        map[string]workflowJob `yaml:"jobs"`
	Env         map[string]string      `yaml:"env"`
	Concurrency workflowConcurrency    `yaml:"concurrency"`
}

func parseWorkflow(t *testing.T) workflowDoc {
	t.Helper()
	// Only jobs are decoded: the "on" key is a YAML boolean and is never needed
	// structurally, while its triggers are asserted in workflow_contract_test.go.
	var doc workflowDoc
	if err := yaml.Unmarshal([]byte(readRepoFile(t, releaseWorkflowRelPath)), &doc); err != nil {
		t.Fatalf("release.yml is not valid YAML: %v", err)
	}
	if len(doc.Jobs) == 0 {
		t.Fatalf("release.yml parsed but has no jobs")
	}
	return doc
}

func needsGate(job workflowJob) bool {
	for _, n := range job.Needs {
		if n == gateJob {
			return true
		}
	}
	return false
}

func TestReleaseWorkflowParsesAndGatesBothEntryPoints(t *testing.T) {
	doc := parseWorkflow(t)

	gate, ok := doc.Jobs[gateJob]
	if !ok {
		t.Fatalf("release.yml has no %q job", gateJob)
	}
	if gate.If != "" {
		t.Errorf("%s must run for both entry points, but declares a job-level if: %q", gateJob, gate.If)
	}
	gateRunsCLI := false
	for _, step := range gate.Steps {
		if strings.Contains(step.Run, gateCLI) {
			gateRunsCLI = true
		}
	}
	if !gateRunsCLI {
		t.Errorf("%s must run %s so tag push and workflow_dispatch share one version decision", gateJob, gateCLI)
	}

	for _, name := range []string{"update-version", "build-frontend", "release"} {
		job, ok := doc.Jobs[name]
		if !ok {
			t.Errorf("release.yml has no %q job", name)
			continue
		}
		if !needsGate(job) {
			t.Errorf("job %q does not depend on %q (needs: %v)", name, gateJob, job.Needs)
		}
	}

	// The checkout ref must be the gated, normalised tag, not the raw event input.
	release := doc.Jobs["release"]
	foundRef := false
	for _, step := range release.Steps {
		if step.Uses == "" || !strings.Contains(step.Uses, "actions/checkout") {
			continue
		}
		foundRef = true
	}
	if !foundRef {
		t.Errorf("release job must still check out a ref")
	}
}

func TestVersionWritebackPushesOnlyWithOptIn(t *testing.T) {
	doc := parseWorkflow(t)

	sync, ok := doc.Jobs["sync-version-file"]
	if !ok {
		t.Fatalf("release.yml has no sync-version-file job")
	}
	if !strings.Contains(sync.If, "needs.release.result == 'success'") {
		t.Errorf("sync-version-file must only run after a successful release, got if: %q", sync.If)
	}

	pushSteps := 0
	for _, step := range sync.Steps {
		if !strings.Contains(step.Run, "git push") {
			continue
		}
		pushSteps++
		if !strings.Contains(step.If, defaultBranchPushOptIn) {
			t.Errorf("push step %q is not guarded by %s (if: %q)", step.Name, defaultBranchPushOptIn, step.If)
		}
	}
	if pushSteps != 1 {
		t.Errorf("expected exactly one push step in sync-version-file, found %d", pushSteps)
	}

	// A push onto a possibly protected default branch must never be reachable
	// without an explicit condition.
	for _, step := range sync.Steps {
		if strings.Contains(step.Run, "git push") && strings.TrimSpace(step.If) == "" {
			t.Errorf("push step %q has no if: condition", step.Name)
		}
	}
}

type goreleaserBuild struct {
	Goos   []string `yaml:"goos"`
	Goarch []string `yaml:"goarch"`
}

type goreleaserDoc struct {
	Builds   []goreleaserBuild `yaml:"builds"`
	Archives []any             `yaml:"archives"`
	Checksum struct {
		NameTemplate string `yaml:"name_template"`
		Disable      bool   `yaml:"disable"`
	} `yaml:"checksum"`
	Release struct {
		SkipUpload *bool `yaml:"skip_upload"`
	} `yaml:"release"`
}

func TestGoreleaserConfigsEncodeTheAssetContract(t *testing.T) {
	var full goreleaserDoc
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".goreleaser.yaml")), &full); err != nil {
		t.Fatalf(".goreleaser.yaml is not valid YAML: %v", err)
	}
	if full.Checksum.NameTemplate != ChecksumsAssetName {
		t.Errorf(".goreleaser.yaml checksum name_template = %q, want %q", full.Checksum.NameTemplate, ChecksumsAssetName)
	}
	if full.Checksum.Disable {
		t.Errorf(".goreleaser.yaml must not disable checksums: a binary release depends on %s", ChecksumsAssetName)
	}
	if len(full.Archives) == 0 {
		t.Errorf(".goreleaser.yaml must declare archives, otherwise no platform binary is published")
	}
	if len(full.Builds) == 0 {
		t.Fatalf(".goreleaser.yaml declares no build")
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if !containsString(full.Builds[0].Goos, goos) {
			t.Errorf(".goreleaser.yaml goos %v is missing %q", full.Builds[0].Goos, goos)
		}
	}
	for _, goarch := range []string{"amd64", "arm64"} {
		if !containsString(full.Builds[0].Goarch, goarch) {
			t.Errorf(".goreleaser.yaml goarch %v is missing %q", full.Builds[0].Goarch, goarch)
		}
	}

	var simple goreleaserDoc
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".goreleaser.simple.yaml")), &simple); err != nil {
		t.Fatalf(".goreleaser.simple.yaml is not valid YAML: %v", err)
	}
	if len(simple.Archives) != 0 {
		t.Errorf(".goreleaser.simple.yaml must publish no archives, got %d", len(simple.Archives))
	}
	if !simple.Checksum.Disable {
		t.Errorf(".goreleaser.simple.yaml must disable checksums: an image-only release has nothing verifiable to checksum")
	}
	if simple.Release.SkipUpload == nil || !*simple.Release.SkipUpload {
		t.Errorf(".goreleaser.simple.yaml must set release.skip_upload to keep an image-only release free of binary assets")
	}
	if len(simple.Builds) == 0 {
		t.Fatalf(".goreleaser.simple.yaml declares no build")
	}
	if containsString(simple.Builds[0].Goos, "windows") || containsString(simple.Builds[0].Goos, "darwin") {
		t.Errorf(".goreleaser.simple.yaml goos %v must stay linux-only", simple.Builds[0].Goos)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func findStep(t *testing.T, steps []workflowStep, name string) workflowStep {
	t.Helper()
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step named %q", name)
	return workflowStep{}
}

// TestImageOnlyConfigIsSelectedByTheSameFlagThatClassifiesIt covers the
// consistency the asset contract depends on: whichever config GoReleaser is
// pointed at must be the config the classification was derived from. If the
// simple (image-only) config could be selected while the run is classified as a
// binary release, the asset gate would demand archives that this config can
// never produce.
func TestImageOnlyConfigIsSelectedByTheSameFlagThatClassifiesIt(t *testing.T) {
	workflow := readRepoFile(t, releaseWorkflowRelPath)

	selected := 0
	for _, line := range strings.Split(workflow, "\n") {
		if !strings.Contains(line, "--config=.goreleaser.simple.yaml") {
			continue
		}
		selected++
		if !strings.Contains(line, "SIMPLE_RELEASE") {
			t.Errorf("the simple goreleaser config must be selected by the same SIMPLE_RELEASE flag that sets the release kind, got %q", strings.TrimSpace(line))
		}
	}
	if selected != 1 {
		t.Errorf("expected exactly one simple-config selection, found %d", selected)
	}

	doc := parseWorkflow(t)
	collect := findStep(t, doc.Jobs[gateJob].Steps, "Collect release input")
	for _, want := range []string{"SIMPLE_RELEASE", "image_only", "binary"} {
		if !strings.Contains(collect.Run, want) {
			t.Errorf("the gate input step must derive the release kind from SIMPLE_RELEASE (missing %q)", want)
		}
	}

	// The post-publish asset check must consume the gate's classification
	// instead of re-deriving one of its own.
	verify := findStep(t, doc.Jobs["release"].Steps, "Verify release asset contract")
	if got := verify.Env["RELEASE_KIND"]; got != "${{ needs.release-gate.outputs.release_kind }}" {
		t.Errorf("asset check RELEASE_KIND = %q, want the gate's release_kind output", got)
	}
}

// TestBaselineCollectionIsFailClosedAndObservable covers the pagination and
// fetch-successity boundary: the gate may only compare against a baseline it
// actually has, and whatever it concluded must be readable in the run log.
func TestBaselineCollectionIsFailClosedAndObservable(t *testing.T) {
	doc := parseWorkflow(t)
	collect := findStep(t, doc.Jobs[gateJob].Steps, "Collect release input")

	// A page cap must be reported as truncation, never as a short baseline.
	for _, want := range []string{"PUBLISHED_LIMIT", "fork_baseline_truncated", "fork_baseline_unknown", "upstream_versions_unknown"} {
		if !strings.Contains(collect.Run, want) {
			t.Errorf("the gate input step must record %q so the gate can fail closed or warn", want)
		}
	}

	// The verdict (decision, baseline, warnings) must reach the job log.
	gateStep := findStep(t, doc.Jobs[gateJob].Steps, "Run fork release gate")
	if !strings.Contains(gateStep.Run, "release-gate-result.json") || !strings.Contains(gateStep.Run, "cat ") {
		t.Errorf("the gate step must print the verdict JSON so the decision and any warning are observable in the run log")
	}
}

// TestAssetVerificationDistinguishesReadFailure covers the post-publish read.
// "Could not read the assets" and "read the assets, there are none" are
// different facts: only the second may be reported as an image-only
// classification, and the first must fail loudly instead of quietly looking
// like a successful verification.
func TestAssetVerificationDistinguishesReadFailure(t *testing.T) {
	doc := parseWorkflow(t)
	verify := findStep(t, doc.Jobs["release"].Steps, "Verify release asset contract")

	for _, want := range []string{"ASSETS_UNVERIFIED", "assets_unverified"} {
		if !strings.Contains(verify.Run, want) {
			t.Errorf("the asset check must record a failed read as %q instead of collapsing it into an empty list", want)
		}
	}
	if !strings.Contains(strings.ToLower(verify.Run), "not completed") {
		t.Errorf("the asset check must fail with an explicit \"verification was not completed\" error when the read failed")
	}
	if !strings.Contains(verify.Run, "exit 1") {
		t.Errorf("the failed-read path must exit non-zero rather than reporting success")
	}
	// The classification claim may only be made from a verified read, so it must
	// sit after the unverified guard in the same script.
	unverifiedGuard := strings.Index(verify.Run, `ASSETS_UNVERIFIED" = "true"`)
	imageOnlyClaim := strings.Index(verify.Run, "image-only release:")
	if unverifiedGuard < 0 || imageOnlyClaim < 0 {
		t.Fatalf("asset check must contain both the unverified guard and the image-only claim")
	}
	if unverifiedGuard > imageOnlyClaim {
		t.Errorf("the unverified guard must be evaluated before any image-only claim is printed")
	}
}

// TestReleaseContentIsCheckedOutFromTheGatedTag keeps every job that produces
// content for the release on the tag the gate decided to release. On a manual
// dispatch of an older tag the trigger ref is whatever branch was selected (the
// default branch), so build-frontend would otherwise compile and embed a
// frontend from a different commit than the Go code the release job checks out:
// the published release would not correspond to its own tag.
func TestReleaseContentIsCheckedOutFromTheGatedTag(t *testing.T) {
	doc := parseWorkflow(t)

	gatedRef := "${{ needs." + gateJob + ".outputs.tag }}"
	for _, name := range []string{"build-frontend", "release"} {
		job, ok := doc.Jobs[name]
		if !ok {
			t.Errorf("release.yml has no %q job", name)
			continue
		}
		checkouts := 0
		for _, step := range job.Steps {
			if !strings.Contains(step.Uses, "actions/checkout") {
				continue
			}
			checkouts++
			if got := step.With.Ref; got != gatedRef {
				t.Errorf("job %q checks out %q, want the gated tag %q: the release would ship another commit's content than the version it publishes", name, got, gatedRef)
			}
		}
		if checkouts == 0 {
			t.Errorf("job %q must check out the source tree from the gated tag", name)
		}
	}
}

// TestReleaseRunsAreSerialised covers the two mutable publish targets of a
// release run. Both goreleaser configs push a `:latest` image tag (see
// .goreleaser.yaml / .goreleaser.simple.yaml) and sync-version-file moves a
// branch pointer, so two release runs in flight at once — a tag push plus a
// dispatch, or a dispatch of an older tag — can leave `:latest` pointing at the
// older version, or make the VERSION writeback depend on which run pushes first.
// The guard must serialise the whole workflow, must not cancel a publish that is
// already uploading, and must not be keyed on the tag: a per-version group would
// let exactly those two runs proceed in parallel.
func TestReleaseRunsAreSerialised(t *testing.T) {
	doc := parseWorkflow(t)
	if strings.TrimSpace(doc.Concurrency.Group) == "" {
		t.Fatalf("release.yml declares no concurrency group: concurrent release runs can race the mutable :latest image tag and the VERSION writeback")
	}
	if doc.Concurrency.CancelInProgress {
		t.Errorf("concurrency.cancel-in-progress is true: a cancelled run can leave a half-published release (some platforms uploaded, :latest not yet moved)")
	}
	for _, banned := range []string{"inputs", "github.ref", "refs/"} {
		if strings.Contains(doc.Concurrency.Group, banned) {
			t.Errorf("the concurrency group must not be keyed on the released version (%q in %q): two different versions could then publish at the same time", banned, doc.Concurrency.Group)
		}
	}
}
