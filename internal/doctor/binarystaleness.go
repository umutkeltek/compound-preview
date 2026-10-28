package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Relation classifies how the installed binary's build SHA relates to the repo's
// target tip (HEAD of the branch the operator cares about — main, by default). The
// user's evidence on 2026-08-17 was the trigger: an installed build five commits
// behind main reported "all-clear" because doctor's existing projections check only
// asked "does the projection match the RUNNING binary", never "is the running binary
// as new as main". This type is the named signal that closes the gap.
type Relation string

const (
	// RelationEqual means installed SHA equals target tip exactly.
	RelationEqual Relation = "equal"
	// RelationDescendant means installed SHA is a descendant of target tip — the
	// installed build is at least as new as main, possibly ahead on a forward branch.
	RelationDescendant Relation = "descendant"
	// RelationAncestor means installed SHA is a strict ancestor of target tip — main
	// has commits the installed binary cannot see. This is the FAILURE the user hit:
	// doctor said all-clear while five commits of fix were waiting behind the install.
	RelationAncestor Relation = "ancestor"
	// RelationUnknown means we could not establish the relation (empty SHAs, git
	// errors, no embedded build SHA in the binary). Not a pass — not a verdict.
	RelationUnknown Relation = "unknown"
)

// BinaryStalenessReport is the named-signal payload the doctor surfaces. It carries the
// installed SHA, the target tip, and the relation between them — never a bare boolean —
// so the operator can read the verdict directly instead of guessing from a green light.
type BinaryStalenessReport struct {
	InstalledSHA string   `json:"installed_sha"`
	TargetTip    string   `json:"target_tip"`
	Relation     Relation `json:"relation"`
	// Stale is the boolean derived from Relation for convenience, but the relation
	// itself is the primary signal. false when relation is Equal/Descendant/Unknown;
	// true when relation is Ancestor.
	Stale     bool   `json:"stale"`
	Note      string `json:"note,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// RelateSHAs is the PURE decision at the center of the check: given an installed SHA
// and a target tip, plus an isAncestor hook, classify the relation. The hook is
// injected so the function is testable without a real git repo. Behavior:
//
//	installed == tip               → RelationEqual
//	installed is ancestor of tip   → RelationAncestor
//	installed is descendant of tip → RelationDescendant
//	either SHA empty / unset       → RelationUnknown
//	ancestor lookup errors         → RelationUnknown (note carries the error)
func RelateSHAs(installed, target string, isAncestor func(parent, child string) (bool, error)) Relation {
	installed = strings.TrimSpace(installed)
	target = strings.TrimSpace(target)
	if installed == "" || target == "" {
		return RelationUnknown
	}
	if strings.EqualFold(installed, target) {
		return RelationEqual
	}
	// Short-circuit: prefix matches where the shorter is the prefix of the longer
	// are the same commit (short SHA vs long SHA). Without this, `a1b2c3d` vs
	// `a1b2c3d4e5f...` would be misclassified as RelationUnknown.
	if strings.HasPrefix(installed, target) || strings.HasPrefix(target, installed) {
		return RelationEqual
	}
	// isAncestor(parent=A, child=B) answers: is A a strict ancestor of B? We use it
	// in BOTH directions so a forward-branch installed binary (descendant of main)
	// does not get flagged stale just because the SHAs don't match exactly.
	anc, err := isAncestor(installed, target)
	if err != nil {
		return RelationUnknown
	}
	if anc {
		return RelationAncestor
	}
	anc, err = isAncestor(target, installed)
	if err != nil {
		return RelationUnknown
	}
	if anc {
		return RelationDescendant
	}
	// SHAs differ and are not related by ancestry — an unrelated fork or a fully
	// divergent local branch. Treat as unknown rather than ancestor: we cannot prove
	// the installed binary is older than main when we cannot see how they connect.
	return RelationUnknown
}

// ComputeBinaryStaleness wires the impure seams: reads the installed binary's `version`
// output to recover the embedded build SHA, asks git for the target tip (HEAD of the
// repoRoot), and asks git rev-list for the ancestry check. Any single failure degrades
// to RelationUnknown — never a false-ancestor verdict — because the cost of a wrong
// "stale" reading is operator churn, while the cost of a wrong "unknown" reading is the
// operator running `make install` once to clear the gap.
func ComputeBinaryStaleness(
	repoRoot string,
	installedBinaryPath string,
	versionFn func(string) (string, error),
	parseSHA func(string) string,
	headFn func(string) (string, error),
	isAncestorFn func(parent, child string) (bool, error),
) BinaryStalenessReport {
	report := BinaryStalenessReport{CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if versionFn == nil || parseSHA == nil || headFn == nil || isAncestorFn == nil {
		report.Relation = RelationUnknown
		report.Note = "internal: nil function dependency passed to ComputeBinaryStaleness"
		return report
	}
	verOut, err := versionFn(installedBinaryPath)
	if err != nil {
		report.Relation = RelationUnknown
		report.Note = fmt.Sprintf("installed binary %s did not respond to `version`: %v", installedBinaryPath, err)
		return report
	}
	report.InstalledSHA = parseSHA(verOut)
	if report.InstalledSHA == "" || strings.EqualFold(report.InstalledSHA, "dev") {
		report.Relation = RelationUnknown
		report.Note = fmt.Sprintf("installed binary %s has no embedded build SHA (predates this feature or built without ldflags)", installedBinaryPath)
		return report
	}
	tip, err := headFn(repoRoot)
	if err != nil || strings.TrimSpace(tip) == "" {
		report.Relation = RelationUnknown
		report.Note = fmt.Sprintf("could not resolve HEAD sha in %s: %v", repoRoot, err)
		return report
	}
	report.TargetTip = tip
	report.Relation = RelateSHAs(report.InstalledSHA, report.TargetTip, isAncestorFn)
	report.Stale = report.Relation == RelationAncestor
	return report
}

// GitIsAncestor is the impure wiring of the isAncestor hook: `git rev-list --ancestor-path`
// is the canonical "is A a strict ancestor of B" check. It is a method-shaped variable
// so tests can substitute fakes without exposing exec.Command to the falsifier.
var GitIsAncestor = defaultGitIsAncestor

// defaultGitIsAncestor shells out to git. It is the seam ComputeBinaryStaleness uses
// when no override is supplied. Kept short — git's own merge-base machinery is the
// ground truth here, no point reimplementing reachability in Go.
func defaultGitIsAncestor(parent, child string) (bool, error) {
	if strings.TrimSpace(parent) == "" || strings.TrimSpace(child) == "" {
		return false, fmt.Errorf("empty parent or child sha")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	// `git merge-base --is-ancestor <A> <B>` exits 0 when A is an ancestor of B,
	// exits 1 when not, and exits 128 on error. We map that to (bool, error) here.
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", parent, child)
	if err := cmd.Run(); err != nil {
		// exit code 1 from merge-base is "not an ancestor" — not an error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ParseInstalledBuildSHA extracts the embedded build SHA from `compound version` output.
// Lives here (not in internal/arch) because the doctor contract owns the parsing for
// the binary-staleness signal — internal/arch's freshness check uses its own regex for
// the build-freshness gate, and the two can drift independently.
func ParseInstalledBuildSHA(versionOutput string) string {
	const open = "(build "
	const close = ")"
	idx := strings.Index(versionOutput, open)
	if idx < 0 {
		return ""
	}
	rest := versionOutput[idx+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		return ""
	}
	sha := strings.TrimSpace(rest[:end])
	if sha == "" || strings.EqualFold(sha, "dev") {
		return ""
	}
	return sha
}

// InstalledBinaryPath mirrors install.sh: $GOBIN/compound if GOBIN is set, else
// $HOME/.local/bin/compound. Exported so cmd/compound/doctor_cli.go can resolve the
// same path the installer wrote to without re-deriving the convention.
func InstalledBinaryPath() string {
	if gobin := strings.TrimSpace(os.Getenv("GOBIN")); gobin != "" {
		return filepath.Join(gobin, "compound")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".local", "bin", "compound")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "bin", "compound")
}

// RunInstalledVersion shells out to the installed binary's `version` subcommand and
// returns its stdout. This is the seam doctor uses to recover the embedded build SHA.
var RunInstalledVersion = defaultRunInstalledVersion

func defaultRunInstalledVersion(binaryPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, binaryPath, "version").Output()
	return string(out), err
}

// GitHeadSHA returns the short HEAD SHA of repoRoot. Exported so doctor_cli can wire it
// without duplicating the git invocation; tests can substitute a fake.
var GitHeadSHA = defaultGitHeadSHA

func defaultGitHeadSHA(repoRoot string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// RenderBinaryStaleness writes the binary-staleness verdict to stdout in the doctor's
// plain-text format. The named signal — installed SHA + target tip + relation — appears
// on a single line so the operator can read the verdict directly instead of guessing
// from a bare green light. Closes the 2026-08-17 gap where doctor said all-clear while
// installed was five commits behind main.
func RenderBinaryStaleness(stdout io.Writer, r BinaryStalenessReport) {
	fmt.Fprintln(stdout, "\n## binary staleness (is installed binary as new as main?)")
	if r.InstalledSHA == "" && r.TargetTip == "" {
		if r.Note != "" {
			fmt.Fprintf(stdout, "- %s\n", r.Note)
		} else {
			fmt.Fprintln(stdout, "- (could not determine installed SHA and target tip)")
		}
		return
	}
	switch r.Relation {
	case RelationAncestor:
		fmt.Fprintf(stdout, "- STALE: installed=%s, target=%s — run `make install`\n", r.InstalledSHA, r.TargetTip)
	case RelationEqual:
		fmt.Fprintf(stdout, "- fresh: installed=%s matches target=%s\n", r.InstalledSHA, r.TargetTip)
	case RelationDescendant:
		fmt.Fprintf(stdout, "- fresh: installed=%s is ahead of target=%s\n", r.InstalledSHA, r.TargetTip)
	default:
		if r.Note != "" {
			fmt.Fprintf(stdout, "- unknown: installed=%s, target=%s — %s\n", r.InstalledSHA, r.TargetTip, r.Note)
		} else {
			fmt.Fprintf(stdout, "- unknown: installed=%s, target=%s\n", r.InstalledSHA, r.TargetTip)
		}
	}
}
