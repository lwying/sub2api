package releasecontract

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

// Exit codes of the releasegate command.
const (
	ExitOK       = 0
	ExitRejected = 1
	ExitUsage    = 2
)

// Usage is the command help text.
const Usage = `releasegate - fork release contract checks for ` + ForkRepo + `

usage:
  releasegate gate         -input <file> [-out <file>]   validate a release version input
  releasegate sync-version -input <file> [-out <file>]   decide the VERSION writeback
  releasegate assets       -input <file> [-out <file>]   report per-platform installability

Each subcommand reads a JSON input file and writes a JSON verdict to stdout.
Exit code 0 means accepted, 1 means rejected, 2 means the input was unusable.
The command never reads the network or git; the workflow and its fixtures
supply every fact it decides on.`

type assetVerdict struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
	AssetReport
}

// Run executes a releasegate subcommand and returns the process exit code.
func Run(args []string, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, Usage)
		return ExitUsage
	}

	switch args[0] {
	case "gate":
		return runGate(args[1:], stdout)
	case "sync-version":
		return runSyncVersion(args[1:], stdout)
	case "assets":
		return runAssets(args[1:], stdout)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, Usage)
		return ExitOK
	default:
		fmt.Fprintf(stdout, "unknown subcommand %q\n\n%s\n", args[0], Usage)
		return ExitUsage
	}
}

// parseFlags parses -input/-out and returns the decoded input file.
func parseFlags(sub string, args []string, stdout io.Writer, into any) (inPath, outPath string, code int, err error) {
	fs := flag.NewFlagSet(sub, flag.ContinueOnError)
	fs.SetOutput(stdout)
	inFlag := fs.String("input", "", "path to the JSON input file")
	outFlag := fs.String("out", "", "optional path to also write the JSON verdict to")
	if perr := fs.Parse(args); perr != nil {
		return "", "", ExitUsage, perr
	}
	inPath, outPath = *inFlag, *outFlag
	if inPath == "" {
		return "", "", ExitUsage, fmt.Errorf("-%s requires -input <file>", sub)
	}
	raw, rerr := os.ReadFile(inPath)
	if rerr != nil {
		return "", "", ExitUsage, rerr
	}
	if derr := json.Unmarshal(raw, into); derr != nil {
		return "", "", ExitUsage, fmt.Errorf("parse %s: %w", inPath, derr)
	}
	return inPath, outPath, ExitOK, nil
}

// emit writes the verdict as indented JSON to stdout, optionally to -out too,
// and mirrors the message to stderr so a failing CI step is readable.
func emit(stdout io.Writer, outPath string, verdict any, message string, exitCode int) int {
	encoded, err := json.MarshalIndent(verdict, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "releasegate: encode verdict: %v\n", err)
		return ExitUsage
	}
	fmt.Fprintf(stdout, "%s\n", encoded)
	if outPath != "" {
		if werr := os.WriteFile(outPath, append(encoded, '\n'), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "releasegate: write %s: %v\n", outPath, werr)
			return ExitUsage
		}
	}
	if message != "" {
		fmt.Fprintf(os.Stderr, "releasegate: %s\n", message)
	}
	return exitCode
}

func runGate(args []string, stdout io.Writer) int {
	var in ReleaseInput
	_, outPath, code, err := parseFlags("gate", args, stdout, &in)
	if err != nil {
		fmt.Fprintf(stdout, "releasegate gate: %v\n", err)
		return code
	}
	verdict := Evaluate(in)
	exit := ExitOK
	if !verdict.OK {
		exit = ExitRejected
	}
	return emit(stdout, outPath, verdict, verdict.Message, exit)
}

func runSyncVersion(args []string, stdout io.Writer) int {
	var in SyncInput
	_, outPath, code, err := parseFlags("sync-version", args, stdout, &in)
	if err != nil {
		fmt.Fprintf(stdout, "releasegate sync-version: %v\n", err)
		return code
	}
	verdict := DecideSyncVersion(in)
	exit := ExitOK
	if !verdict.OK {
		exit = ExitRejected
	}
	return emit(stdout, outPath, verdict, verdict.Message, exit)
}

func runAssets(args []string, stdout io.Writer) int {
	var in ReleaseInput
	_, outPath, code, err := parseFlags("assets", args, stdout, &in)
	if err != nil {
		fmt.Fprintf(stdout, "releasegate assets: %v\n", err)
		return code
	}
	v, verr := ParseVersion(in.CandidateTag)
	if verr != nil {
		fmt.Fprintf(stdout, "releasegate assets: %v\n", verr)
		return ExitUsage
	}
	kind, kerr := ParseReleaseKind(string(in.ReleaseKind))
	if kerr != nil {
		fmt.Fprintf(stdout, "releasegate assets: %v\n", kerr)
		return ExitUsage
	}
	report := AssessAssets(kind, v, in.Assets)
	if in.AssetsUnverified {
		// An unreadable asset list is a failed verification, never an
		// image-only classification.
		report = UnverifiedAssetsReport(kind)
	}
	verdict := assetVerdict{
		OK:          report.Installable,
		Code:        report.Reason,
		Message:     report.Message,
		AssetReport: report,
	}
	exit := ExitOK
	if !report.Installable {
		exit = ExitRejected
	}
	return emit(stdout, outPath, verdict, report.Message, exit)
}

// DecodeInputFile reads and decodes a JSON fixture or workflow input file. It
// exists so tests (and later tickets 07/08) share one decoding path with the CLI.
func DecodeInputFile(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
