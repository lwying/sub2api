//go:build unit

// Static contract checks for the fork release entry points.
//
// These assertions are deliberately text-based: they read the real
// .github/workflows/release.yml and .goreleaser*.yaml from the working tree, so
// the publishing entry points cannot silently drift away from the locally
// tested release contract. No network access, no git tags, no GitHub calls.
package releasecontract

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// All paths below are relative to the repository root.
const (
	releaseWorkflowRelPath = ".github/workflows/release.yml"

	// gateJob is the job that must reject non vX.Y.Z / non-increasing /
	// upstream-carried versions for both tag push and workflow_dispatch.
	gateJob = "release-gate"
	// gateCLI is the only place where release input is normalised and judged.
	gateCLI = "cmd/releasegate"

	// defaultBranchPushOptIn is the maintainer-controlled switch that allows the
	// VERSION writeback to touch a protected default branch.
	defaultBranchPushOptIn = "FORK_ALLOW_DEFAULT_BRANCH_VERSION_PUSH"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

var jobHeaderRe = regexp.MustCompile(`(?m)^  ([A-Za-z0-9_-]+):[ \t]*$`)

// jobBlock returns the YAML body of one job, up to the next indent-2 key.
func jobBlock(t *testing.T, workflow, name string) string {
	t.Helper()
	all := jobHeaderRe.FindAllStringSubmatchIndex(workflow, -1)
	for i, m := range all {
		if workflow[m[2]:m[3]] != name {
			continue
		}
		end := len(workflow)
		if i+1 < len(all) {
			end = all[i+1][0]
		}
		return workflow[m[0]:end]
	}
	t.Fatalf("release.yml has no job %q", name)
	return ""
}

func hasJob(t *testing.T, workflow, name string) bool {
	t.Helper()
	for _, m := range jobHeaderRe.FindAllStringSubmatch(workflow, -1) {
		if m[1] == name {
			return true
		}
	}
	return false
}

var stepHeaderRe = regexp.MustCompile(`(?m)^      - `)

// steps splits a job block into its step blocks (indent 6 list items).
func steps(job string) []string {
	idx := stepHeaderRe.FindAllStringIndex(job, -1)
	out := make([]string, 0, len(idx))
	for i, m := range idx {
		end := len(job)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out = append(out, job[m[0]:end])
	}
	return out
}

func mustContain(t *testing.T, what, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s must contain %q", what, needle)
	}
}

// TestReleaseWorkflowGatesEveryEntryPoint covers acceptance criterion 1: a
// release input that is not vX.Y.Z, is not above the fork's published baseline,
// or was merely carried in by an upstream sync must not become a fork release
// through either entry point (tag push or manual dispatch).
func TestReleaseWorkflowGatesEveryEntryPoint(t *testing.T) {
	workflow := readRepoFile(t, releaseWorkflowRelPath)

	if !hasJob(t, workflow, gateJob) {
		t.Fatalf("release.yml has no %q job: tag push and workflow_dispatch would publish without validating the fork version", gateJob)
	}

	gate := jobBlock(t, workflow, gateJob)
	if strings.Contains(gate, "\n    if:") {
		t.Errorf("%s must run for every entry point, but it declares a job-level if:", gateJob)
	}
	mustContain(t, gateJob, gate, gateCLI)

	// version normalisation and the release decision live in the tested CLI, so
	// the publishing jobs must depend on the gate rather than re-derive a tag.
	for _, name := range []string{"update-version", "build-frontend", "release"} {
		block := jobBlock(t, workflow, name)
		needs := needsLine(block)
		if needs == "" {
			t.Errorf("job %q must declare needs including %q", name, gateJob)
			continue
		}
		if !strings.Contains(needs, gateJob) {
			t.Errorf("job %q needs %q but has %q: a rejected version could still be built and published", name, gateJob, strings.TrimSpace(needs))
		}
	}
	mustContain(t, "update-version", jobBlock(t, workflow, "update-version"), "needs."+gateJob+".outputs.version")
}

func needsLine(job string) string {
	for _, ln := range strings.Split(job, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "needs:") {
			return ln
		}
	}
	return ""
}

// TestVersionWritebackRequiresOptIn covers acceptance criterion 3: the
// workflow must not unconditionally push a version file onto a protected
// default branch, and a failed publish must not forge a new version.
func TestVersionWritebackRequiresOptIn(t *testing.T) {
	workflow := readRepoFile(t, releaseWorkflowRelPath)
	sync := jobBlock(t, workflow, "sync-version-file")

	// Only a successful release may touch VERSION.
	mustContain(t, "sync-version-file", sync, "needs.release.result == 'success'")

	// The monotonic + format decision must come from the tested CLI.
	mustContain(t, "sync-version-file", sync, gateCLI)

	pushed := false
	for _, step := range steps(sync) {
		if !strings.Contains(step, "git push origin HEAD:") {
			continue
		}
		pushed = true
		if !strings.Contains(step, "if:") {
			t.Errorf("the default-branch push step is unconditional: guarded steps must declare an explicit opt-in")
		}
		if !strings.Contains(step, defaultBranchPushOptIn) {
			t.Errorf("the default-branch push step must be guarded by %s, got:\n%s", defaultBranchPushOptIn, step)
		}
	}
	if !pushed {
		t.Errorf("sync-version-file no longer performs a default-branch push; update this contract test to match the new maintainer-controlled path")
	}
}

// TestNoUnescapedDispatchInputInShellScripts asserts that the dispatch input is
// passed through env instead of being interpolated into a shell script, so a
// crafted tag input cannot execute arbitrary commands in the release job.
func TestNoUnescapedDispatchInputInShellScripts(t *testing.T) {
	workflow := readRepoFile(t, releaseWorkflowRelPath)
	for _, line := range shellScriptLines(workflow) {
		if strings.Contains(line, "github.event.inputs.") {
			t.Errorf("dispatch input is interpolated directly into a run script (command injection risk): %q", line)
		}
	}
}

// TestSimpleReleaseInputIsAuthoritativeForDispatch covers the precedence of the
// single switch that selects both the image-only goreleaser config and the
// release kind. On workflow_dispatch the input is the value the maintainer
// picked, so an explicit simple_release=false must be able to turn an image-only
// release off even when the repository variable SIMPLE_RELEASE is true;
// OR-ing the two makes the input unable to say "no" and silently downgrades a
// requested full binary release to an image-only one. The repository variable is
// the default for the tag-push entry point, where no input exists.
func TestSimpleReleaseInputIsAuthoritativeForDispatch(t *testing.T) {
	workflow := readRepoFile(t, releaseWorkflowRelPath)

	const unsafeOr = "github.event.inputs.simple_release == 'true' || vars.SIMPLE_RELEASE"

	assignments := 0
	for _, ln := range strings.Split(workflow, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "SIMPLE_RELEASE:") {
			continue
		}
		assignments++
		if strings.Contains(trimmed, unsafeOr) {
			t.Errorf("SIMPLE_RELEASE ORs the dispatch input with the repository variable, so an explicit simple_release=false cannot turn the image-only release off: %q", trimmed)
		}
		if !strings.Contains(trimmed, "github.event_name == 'workflow_dispatch'") ||
			!strings.Contains(trimmed, "github.event_name != 'workflow_dispatch'") {
			t.Errorf("SIMPLE_RELEASE must split on github.event_name: the dispatch input decides for dispatches, the repository variable only for tag pushes. got %q", trimmed)
		}
		for _, want := range []string{"github.event.inputs.simple_release", "vars.SIMPLE_RELEASE"} {
			if !strings.Contains(trimmed, want) {
				t.Errorf("SIMPLE_RELEASE must still read %q, got %q", want, trimmed)
			}
		}
	}
	if assignments == 0 {
		t.Errorf("release.yml no longer derives SIMPLE_RELEASE; update this contract test if the release kind moved somewhere else")
	}
}

// Docker Hub repository descriptions are optional metadata: a token with image
// push permission can still get HTTP 403 for the description PATCH. The binary
// release and its asset check must not be marked failed after publishing.
func TestDockerHubDescriptionCannotFailPublishedRelease(t *testing.T) {
	workflow := readRepoFile(t, releaseWorkflowRelPath)
	release := jobBlock(t, workflow, "release")
	for _, step := range steps(release) {
		if !strings.Contains(step, "name: Update DockerHub description") {
			continue
		}
		mustContain(t, "Docker Hub description step", step, "continue-on-error: true")
		mustContain(t, "Docker Hub description step", step, "peter-evans/dockerhub-description@v5")
		return
	}
	t.Fatal("release job has no Docker Hub description step")
}

// shellScriptLines returns non-empty lines that belong to a `run:` script body.
func shellScriptLines(workflow string) []string {
	var out []string
	runIndent := -1
	for _, ln := range strings.Split(workflow, "\n") {
		trimmed := strings.TrimSpace(ln)
		indent := len(ln) - len(strings.TrimLeft(ln, " "))
		if runIndent >= 0 {
			if trimmed == "" {
				continue
			}
			if indent > runIndent {
				out = append(out, trimmed)
				continue
			}
			runIndent = -1
		}
		if strings.HasSuffix(trimmed, "run: |") || strings.HasSuffix(trimmed, "run: >-") {
			runIndent = indent
		}
	}
	return out
}

// TestGoreleaserAssetContractIsDeclared covers acceptance criterion 2: the full
// config must produce the platform archives plus checksums.txt that make a
// binary release installable, and the simple config must stay image-only with
// no archives and no checksums so it cannot be mistaken for an installable
// binary release.
func TestGoreleaserAssetContractIsDeclared(t *testing.T) {
	full := readRepoFile(t, ".goreleaser.yaml")
	mustContain(t, ".goreleaser.yaml", full, "checksums.txt")
	for _, goos := range []string{"linux", "windows", "darwin"} {
		mustContain(t, ".goreleaser.yaml goos", full, goos)
	}
	mustContain(t, ".goreleaser.yaml archives", full, "archives:")

	simple := readRepoFile(t, ".goreleaser.simple.yaml")
	mustContain(t, ".goreleaser.simple.yaml archives", simple, "archives: []")
	if !strings.Contains(simple, "disable: true") {
		t.Errorf(".goreleaser.simple.yaml must disable checksums: an image-only release has no verifiable binary assets")
	}
	// The build matrix itself (linux-only, no windows/darwin) is asserted
	// structurally in yaml_contract_test.go; a text search here would also match
	// comments and documentation.
}

// TestInstallerAssetNamingMatchesThisContract pins the cross-ticket contract
// with the installer (ticket 08). The installer refuses any release that does
// not publish checksums.txt plus this platform's archive *by exactly these
// names*, so a drift between this package and deploy/install.sh would make every
// published fork release uninstallable even though both sides look correct in
// isolation.
func TestInstallerAssetNamingMatchesThisContract(t *testing.T) {
	installer := readRepoFile(t, "deploy/install.sh")

	mustContain(t, "deploy/install.sh", installer, `CHECKSUMS_ASSET_NAME="`+ChecksumsAssetName+`"`)
	mustContain(t, "deploy/install.sh", installer, `GITHUB_REPO="`+ForkRepo+`"`)
	// Both archive shapes this contract produces must be the ones the installer
	// composes, with the version normalised without its leading "v".
	mustContain(t, "deploy/install.sh", installer, "sub2api_${version_num}_${OS}_${ARCH}.tar.gz")
	mustContain(t, "deploy/install.sh", installer, "sub2api_${version_num}_${OS}_${ARCH}.zip")

	// The naming scheme must agree with ArchiveName for every required platform.
	v, err := ParseTag("v0.2.8")
	if err != nil {
		t.Fatalf("ParseTag: %v", err)
	}
	for _, p := range RequiredPlatforms {
		name := ArchiveName(v, p)
		if want := "sub2api_" + v.String() + "_" + p.OS + "_" + p.Arch; !strings.HasPrefix(name, want) {
			t.Errorf("ArchiveName(%s) = %q, want prefix %q", p, name, want)
		}
		ext := ".tar.gz"
		if p.OS == "windows" {
			ext = ".zip"
		}
		if !strings.HasSuffix(name, ext) {
			t.Errorf("ArchiveName(%s) = %q, want suffix %q", p, name, ext)
		}
	}
}

// TestForkSlugIsPinnedAcrossWorkflowAndPackage keeps the repository identity
// used by the gate and by the workflow in one place. This is asserted against
// release.yml rather than docs/: docs/* is gitignored in this fork, so a test
// depending on a local-only file would fail in CI.
func TestForkSlugIsPinnedAcrossWorkflowAndPackage(t *testing.T) {
	doc := parseWorkflow(t)
	if got := doc.Env["FORK_REPO"]; got != ForkRepo {
		t.Errorf("release.yml env FORK_REPO = %q, want %q", got, ForkRepo)
	}
	if got := doc.Env["UPSTREAM_REPO"]; got != UpstreamRepo {
		t.Errorf("release.yml env UPSTREAM_REPO = %q, want %q", got, UpstreamRepo)
	}
}
