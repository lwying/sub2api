//go:build unit

// Dry-run harness for the release workflow's shell steps.
//
// These tests execute the real `run:` script text taken from
// .github/workflows/release.yml, with every external command (go, gh, jq)
// stubbed, and assert the exit code a runner would see. They exist because a
// gate that rejects is only useful if the step that ran it actually fails:
// without `set -e`, a rejected gate still exits 0 from the trailing `echo`, and
// the release would proceed.
//
// Scope: this verifies shell control flow and exit propagation. It does not
// verify what the real commands print or whether GitHub's hosted API answers.
package releasecontract

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var workflowExprRe = regexp.MustCompile(`\$\{\{.*?\}\}`)

type stepResult struct {
	exitCode int
	output   string
}

const (
	stubGo = `#!/usr/bin/env bash
# Stub for "go run ./cmd/releasegate ...", which writes a verdict to -out.
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-out" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then
  printf '%s\n' '{"ok":false,"code":"upstream_only","version":"0.2.8","tag":"v0.2.8","release_kind":"binary","baseline":"0.2.6","should_write":"false","reason":"ok"}' > "$out"
fi
exit "${STUB_GO_EXIT:-0}"
`
	stubGH = `#!/usr/bin/env bash
# Record the invocation so a test can assert how the step actually calls the API.
if [ -n "${GH_ARGV_LOG:-}" ]; then
  printf '%s\n' "$*" >> "$GH_ARGV_LOG"
fi
printf '%s\n' '[]'
exit "${STUB_GH_EXIT:-0}"
`
	stubJQ = `#!/usr/bin/env bash
for a in "$@"; do
  if [ "$a" = "-n" ]; then
    printf '%s\n' '{}'
    exit 0
  fi
done
printf '%s\n' "${STUB_JQ_OUT:-placeholder}"
exit "${STUB_JQ_EXIT:-0}"
`
)

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod stub %s: %v", name, err)
	}
}

// --- bash resolution ---------------------------------------------------------
//
// On Windows, exec.LookPath("bash") is not enough to pick a usable shell: PATH
// may resolve to the WSL launcher (C:\Windows\System32\bash.exe), which cannot
// open an absolute Windows path such as C:\Users\...\step.sh (it reports "No
// such file or directory" and exits 127) and cannot read the Windows-form paths
// this harness passes in the step environment. Four tests would then fail for a
// reason that has nothing to do with release.yml.
//
// A candidate is therefore only used after it proves it can run one probe step
// shaped exactly like the harness's steps: the script named by its bare file
// name, with the process cwd in the script's directory, reading an absolute path
// that was handed over through the environment. RELEASECONTRACT_BASH overrides
// the search, and an explicit choice is never silently replaced.
const bashEnvOverride = "RELEASECONTRACT_BASH"

var (
	bashOnce    sync.Once
	bashChosen  string
	bashFailure error
)

// shellPath renders a path for the shell. MSYS and Cygwin bash accept a Windows
// path with forward slashes in file operations, while the WSL launcher accepts
// neither form - which is exactly why probeBash rejects it.
func shellPath(p string) string { return filepath.ToSlash(p) }

func resolveBash() (string, error) {
	bashOnce.Do(func() {
		candidates := bashCandidates()
		if override := strings.TrimSpace(os.Getenv(bashEnvOverride)); override != "" {
			candidates = []string{override}
		}

		var rejected []string
		for _, cand := range candidates {
			if _, err := os.Stat(cand); err != nil {
				rejected = append(rejected, cand+" (not installed)")
				continue
			}
			if reason := probeBash(cand); reason != "" {
				rejected = append(rejected, cand+" ("+reason+")")
				continue
			}
			bashChosen = cand
			return
		}
		bashFailure = fmt.Errorf("no bash that can run the release workflow steps was found; install Git for Windows/MSYS2 bash or set %s; tried: %s",
			bashEnvOverride, strings.Join(rejected, "; "))
	})
	return bashChosen, bashFailure
}

// bashCandidates lists plausible bash installs, most likely first: whatever PATH
// resolves (which may be the unusable WSL launcher), then the bash that ships
// next to git.exe, then the usual Git for Windows / MSYS2 / Cygwin locations.
func bashCandidates() []string {
	var out []string
	add := func(p string) {
		if strings.TrimSpace(p) == "" {
			return
		}
		for _, seen := range out {
			if strings.EqualFold(seen, p) {
				return
			}
		}
		out = append(out, p)
	}

	if p, err := exec.LookPath("bash"); err == nil {
		add(p)
	}
	// A Git for Windows install keeps bash under its root, next to the git.exe
	// found either in <root>\cmd or in <root>\mingw64\bin.
	if p, err := exec.LookPath("git"); err == nil {
		dir := filepath.Dir(p)
		for _, rel := range []string{
			filepath.Join("..", "bin", "bash.exe"),
			filepath.Join("..", "usr", "bin", "bash.exe"),
			filepath.Join("..", "..", "bin", "bash.exe"),
			filepath.Join("..", "..", "usr", "bin", "bash.exe"),
		} {
			add(filepath.Clean(filepath.Join(dir, rel)))
		}
	}
	for _, root := range []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("ProgramW6432"),
		os.Getenv("SystemDrive") + string(os.PathSeparator),
	} {
		if strings.TrimSpace(root) == "" {
			continue
		}
		add(filepath.Join(root, "Git", "bin", "bash.exe"))
		add(filepath.Join(root, "Git", "usr", "bin", "bash.exe"))
		add(filepath.Join(root, "msys64", "usr", "bin", "bash.exe"))
		add(filepath.Join(root, "cygwin64", "bin", "bash.exe"))
		add(filepath.Join(root, "cygwin", "bin", "bash.exe"))
	}
	return out
}

// probeBash reports why cand cannot run the harness's steps, or "" when it can.
func probeBash(cand string) string {
	dir, err := os.MkdirTemp("", "releasecontract-bash-probe")
	if err != nil {
		return "cannot create a probe directory: " + err.Error()
	}
	defer os.RemoveAll(dir)

	input := filepath.Join(dir, "probe-input.txt")
	if err := os.WriteFile(input, []byte("probe\n"), 0o644); err != nil {
		return "cannot write the probe input: " + err.Error()
	}
	script := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(script, []byte("set -euo pipefail\ncat \"$PROBE_INPUT\" >/dev/null\n"), 0o644); err != nil {
		return "cannot write the probe script: " + err.Error()
	}

	cmd := exec.Command(cand, filepath.Base(script))
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + shellPath(dir),
		"PROBE_INPUT=" + shellPath(input),
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("cannot run a step whose script is named relatively and whose environment carries a Windows path (%v): %s", err, strings.TrimSpace(string(out)))
	}
	return ""
}

// runWorkflowStep executes one step's run script and reports its exit code.
func runWorkflowStep(t *testing.T, jobName, stepName string, extraEnv map[string]string) stepResult {
	t.Helper()

	bashPath, err := resolveBash()
	if err != nil {
		// An explicitly requested shell that does not work is a configuration
		// error, not a reason to silently skip the contract checks.
		if strings.TrimSpace(os.Getenv(bashEnvOverride)) != "" {
			t.Fatalf("cannot execute the workflow shell steps on this host: %v", err)
		}
		t.Skipf("cannot execute the workflow shell steps on this host: %v", err)
	}
	t.Logf("running %s/%s with %s", jobName, stepName, bashPath)

	doc := parseWorkflow(t)
	job, ok := doc.Jobs[jobName]
	if !ok {
		t.Fatalf("no job %q", jobName)
	}
	step := findStep(t, job.Steps, stepName)

	script := workflowExprRe.ReplaceAllString(step.Run, "EXPR")

	tmp := t.TempDir()
	stubBin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(stubBin, 0o755); err != nil {
		t.Fatalf("mkdir stubs: %v", err)
	}
	writeStub(t, stubBin, "go", stubGo)
	writeStub(t, stubBin, "gh", stubGH)
	writeStub(t, stubBin, "jq", stubJQ)

	// Simulate the checkout layout the steps assume: the fork-owned VERSION file
	// (read by the writeback decision) and the backend/ directory the asset step
	// cds into.
	for _, rel := range []string{filepath.Join("cmd", "server"), filepath.Join("backend", "cmd", "server")} {
		dir := filepath.Join(tmp, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("0.2.6\n"), 0o644); err != nil {
			t.Fatalf("write %s/VERSION: %v", rel, err)
		}
	}
	workDir := tmp
	if step.WorkingDirectory != "" {
		workDir = filepath.Join(tmp, filepath.FromSlash(step.WorkingDirectory))
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			t.Fatalf("mkdir working-directory %s: %v", step.WorkingDirectory, err)
		}
	}

	// The script is invoked by its bare file name with the cwd set to its own
	// directory, so bash never has to open an absolute Windows path (the WSL
	// launcher cannot open one at all; see resolveBash).
	scriptPath := filepath.Join(workDir, "step.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("write step script: %v", err)
	}

	// The step's own env: block is part of the contract (and `set -u` makes a
	// missing variable fatal), so provide it the way the runner would. Explicit
	// test values win over the workflow placeholders. Paths the script reads or
	// writes are handed over in the shell's own flavour (see shellPath).
	envMap := map[string]string{
		"PATH":          shellPath(stubBin) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RUNNER_TEMP":   shellPath(tmp),
		"GITHUB_OUTPUT": shellPath(filepath.Join(tmp, "github_output")),
		"HOME":          shellPath(tmp),
	}
	for k, v := range step.Env {
		envMap[k] = workflowExprRe.ReplaceAllString(v, "EXPR")
	}
	for k, v := range extraEnv {
		envMap[k] = v
	}
	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}

	// Invoked without -e on purpose: the step's own `set -e` must be what makes
	// a rejected gate fail, otherwise the behaviour depends on how the workflow
	// happens to be invoked.
	cmd := exec.Command(bashPath, filepath.Base(scriptPath))
	cmd.Dir = workDir
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()

	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !asExitError(runErr, &exitErr) {
			t.Fatalf("run step: %v\n%s", runErr, out)
		}
		code = exitErr.ExitCode()
	}
	return stepResult{exitCode: code, output: string(out)}
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// TestReleaseGateStepFailsWhenTheGateRejects is the regression that matters: a
// rejected release input must abort the step. If the step's exit status comes
// from the trailing `echo`, the gate would look green and the release would go
// ahead.
func TestReleaseGateStepFailsWhenTheGateRejects(t *testing.T) {
	rejected := runWorkflowStep(t, gateJob, "Run fork release gate", map[string]string{"STUB_GO_EXIT": "1"})
	if rejected.exitCode == 0 {
		t.Errorf("a rejected gate must fail the step, but it exited 0\n%s", rejected.output)
	}

	accepted := runWorkflowStep(t, gateJob, "Run fork release gate", map[string]string{"STUB_GO_EXIT": "0"})
	if accepted.exitCode != 0 {
		t.Errorf("an accepted gate must pass the step, got exit %d\n%s", accepted.exitCode, accepted.output)
	}
}

// TestVersionWritebackStepFailsWhenTheDecisionRejects covers the same control
// flow for the VERSION writeback decision.
func TestVersionWritebackStepFailsWhenTheDecisionRejects(t *testing.T) {
	rejected := runWorkflowStep(t, "sync-version-file", "Decide the VERSION writeback", map[string]string{"STUB_GO_EXIT": "1"})
	if rejected.exitCode == 0 {
		t.Errorf("a refused writeback must fail the step, but it exited 0\n%s", rejected.output)
	}

	accepted := runWorkflowStep(t, "sync-version-file", "Decide the VERSION writeback", map[string]string{"STUB_GO_EXIT": "0"})
	if accepted.exitCode != 0 {
		t.Errorf("an accepted writeback must pass the step, got exit %d\n%s", accepted.exitCode, accepted.output)
	}
}

// TestAssetVerificationStepExitStatus covers the post-publish check: an
// unreadable asset list must fail, while a legitimately empty image-only release
// must still pass (the subcommand's non-zero "not installable" result is
// expected there and must not be mistaken for a step failure).
func TestAssetVerificationStepExitStatus(t *testing.T) {
	unreadable := runWorkflowStep(t, "release", "Verify release asset contract", map[string]string{
		"STUB_GH_EXIT": "1",
		"STUB_GO_EXIT": "1",
		"STUB_JQ_OUT":  "assets_unverified",
		"TAG":          "v0.2.8",
		"REPOSITORY":   "lwying/sub2api",
		"RELEASE_KIND": "image_only",
	})
	if unreadable.exitCode == 0 {
		t.Errorf("an unreadable asset list must fail the step even for an image-only release, but it exited 0\n%s", unreadable.output)
	}
	if !strings.Contains(strings.ToLower(unreadable.output), "not completed") {
		t.Errorf("the failed read must be reported as an incomplete verification, got:\n%s", unreadable.output)
	}
	if strings.Contains(unreadable.output, "image-only release: no platform binary asset") {
		t.Errorf("a failed read must not be reported as a verified image-only classification:\n%s", unreadable.output)
	}

	imageOnly := runWorkflowStep(t, "release", "Verify release asset contract", map[string]string{
		"STUB_GH_EXIT": "0",
		"STUB_GO_EXIT": "1",
		"STUB_JQ_OUT":  "image_only",
		"TAG":          "v0.2.8",
		"REPOSITORY":   "lwying/sub2api",
		"RELEASE_KIND": "image_only",
	})
	if imageOnly.exitCode != 0 {
		t.Errorf("a verified empty image-only release must pass, got exit %d\n%s", imageOnly.exitCode, imageOnly.output)
	}

	brokenBinary := runWorkflowStep(t, "release", "Verify release asset contract", map[string]string{
		"STUB_GH_EXIT": "0",
		"STUB_GO_EXIT": "1",
		"STUB_JQ_OUT":  "checksums_missing",
		"TAG":          "v0.2.8",
		"REPOSITORY":   "lwying/sub2api",
		"RELEASE_KIND": "binary",
	})
	if brokenBinary.exitCode == 0 {
		t.Errorf("a binary release failing the asset contract must fail the step, got exit 0\n%s", brokenBinary.output)
	}
}

// TestUpstreamTagListIsFetchedWithEveryPage covers how the auxiliary half of the
// baseline check is collected. The upstream-carried-version rule may only be
// applied to a list it actually has: one `per_page=100` page returns the API
// default page and silently drops the tags beyond it, so a version that does
// exist upstream can look absent from upstream and be released as a fork
// version — the same "a truncated list is not a complete list" failure the fork
// baseline already fails closed on.
func TestUpstreamTagListIsFetchedWithEveryPage(t *testing.T) {
	argLog := filepath.Join(t.TempDir(), "gh-argv.log")
	env := map[string]string{
		"GH_ARGV_LOG":    shellPath(argLog),
		"EVENT_NAME":     "push",
		"REF":            "refs/tags/v0.2.8",
		"REPOSITORY":     "lwying/sub2api",
		"UPSTREAM_REPO":  "Wei-Shaw/sub2api",
		"SIMPLE_RELEASE": "false",
	}

	result := runWorkflowStep(t, gateJob, "Collect release input", env)
	if result.exitCode != 0 {
		t.Fatalf("collecting the gate input must succeed, got exit %d\n%s", result.exitCode, result.output)
	}

	raw, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("the step made no gh call at all: %v", err)
	}
	tagsCalls := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.Contains(line, "/tags") {
			continue
		}
		tagsCalls++
		if !strings.Contains(line, "--paginate") {
			t.Errorf("the upstream tag list is fetched with one page only, so older upstream tags are invisible to the carried-version check: %q", line)
		}
		if !strings.Contains(line, "--slurp") {
			t.Errorf("a paginated fetch must be slurped into a single array before it is parsed as JSON: %q", line)
		}
	}
	if tagsCalls == 0 {
		t.Fatalf("the step never requested the upstream tag list:\n%s", raw)
	}

	// A failed upstream lookup stays a degradation, not a step failure: the fork
	// baseline check still applies and the verdict records the skipped check.
	degraded := runWorkflowStep(t, gateJob, "Collect release input", map[string]string{
		"STUB_GH_EXIT":  "1",
		"EVENT_NAME":    "push",
		"REF":           "refs/tags/v0.2.8",
		"REPOSITORY":    "lwying/sub2api",
		"UPSTREAM_REPO": "Wei-Shaw/sub2api",
	})
	if degraded.exitCode != 0 {
		t.Errorf("an unreadable upstream tag list only degrades the auxiliary check; it must not fail the gate step, got exit %d\n%s", degraded.exitCode, degraded.output)
	}
	if !strings.Contains(degraded.output, "skipped") {
		t.Errorf("the degraded upstream check must be reported in the run log, got:\n%s", degraded.output)
	}
}
