package arch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// buildFreshnessTimeout bounds EACH external call (git + the installed binary). Both are
// sequential in checkBuildFreshnessAt, so worst case is 2x this — kept well under the
// freshness check's <2s repo-side budget (see W2 H0: hooks/consumers run the installed
// binary with no way to tell it is stale; this check is INFO/WARN, never a failing gate).
const buildFreshnessTimeout = 700 * time.Millisecond

// buildFreshnessVersionRE extracts the embedded build SHA from `compound version` output
// (format: "compound <version> (build <sha|dev>)" — see cmd/compound/main.go's buildSHA
// ldflags var). Liberal on purpose so a version-string reformat doesn't silently break
// parsing into a false "missing SHA" verdict.
var buildFreshnessVersionRE = regexp.MustCompile(`\(build ([0-9a-fA-F]+|dev)\)`)

// BuildFreshnessStatus classifies an installed binary against the repo's current HEAD.
type BuildFreshnessStatus string

const (
	BuildFresh   BuildFreshnessStatus = "fresh"   // installed SHA matches (a prefix of) HEAD
	BuildStale   BuildFreshnessStatus = "stale"   // installed SHA differs from HEAD
	BuildMissing BuildFreshnessStatus = "missing" // no binary at the installed path
	BuildUnknown BuildFreshnessStatus = "unknown" // binary present but no parseable embedded SHA, or HEAD unresolvable
)

// ParseBuildSHA extracts the embedded build SHA from `compound version` output. Returns
// "" if the output doesn't carry a parseable "(build <sha>)" segment.
func ParseBuildSHA(versionOutput string) string {
	m := buildFreshnessVersionRE.FindStringSubmatch(versionOutput)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// classifyBuildFreshness is the PURE decision at the center of the check: given an
// already-extracted installed SHA ("" if unparseable) and the repo HEAD SHA, decide
// fresh/stale/unknown. No filesystem or process access — BuildMissing is decided by the
// caller (the binary path didn't exist at all), never here.
func classifyBuildFreshness(installedSHA, headSHA string) BuildFreshnessStatus {
	installedSHA = strings.TrimSpace(installedSHA)
	headSHA = strings.TrimSpace(headSHA)
	if installedSHA == "" || strings.EqualFold(installedSHA, "dev") || headSHA == "" {
		return BuildUnknown
	}
	if strings.HasPrefix(headSHA, installedSHA) || strings.HasPrefix(installedSHA, headSHA) {
		return BuildFresh
	}
	return BuildStale
}

// checkBuildFreshnessAt is the testable seam: binaryPath is stat'd directly (a fixture
// path, never resolved here), and headFn/versionFn are injected so tests never need a
// real git repo or a real installed compound binary — fakes/stubs only. Always returns
// exactly one Finding, and NEVER Fail: a stale/missing installed binary is expected
// mid-session (landing always outdates it until `make install` reruns) and must not
// flip the proof-check battery red.
func checkBuildFreshnessAt(binaryPath string, headFn func() (string, error), versionFn func(string) (string, error)) []Finding {
	if _, err := os.Stat(binaryPath); err != nil {
		return []Finding{{
			Check:    "build-freshness",
			Severity: Info,
			Message:  fmt.Sprintf("no installed binary at %s (run `make install`) — status=%s", binaryPath, BuildMissing),
		}}
	}
	headSHA, err := headFn()
	if err != nil || strings.TrimSpace(headSHA) == "" {
		return []Finding{{
			Check:    "build-freshness",
			Severity: Info,
			Message:  fmt.Sprintf("could not resolve HEAD sha — status=%s", BuildUnknown),
		}}
	}
	verOut, err := versionFn(binaryPath)
	if err != nil {
		return []Finding{{
			Check:    "build-freshness",
			Severity: Warn,
			Message:  fmt.Sprintf("installed binary %s did not respond to `version`: %v — status=%s", binaryPath, err, BuildUnknown),
		}}
	}
	installedSHA := ParseBuildSHA(verOut)
	status := classifyBuildFreshness(installedSHA, headSHA)
	switch status {
	case BuildFresh:
		return []Finding{{
			Check:    "build-freshness",
			Severity: Info,
			Message:  fmt.Sprintf("installed binary %s matches HEAD %s — status=%s", binaryPath, headSHA, status),
		}}
	case BuildStale:
		return []Finding{{
			Check:    "build-freshness",
			Severity: Warn,
			Message:  fmt.Sprintf("installed binary %s is build %s, HEAD is %s — run `make install` — status=%s", binaryPath, installedSHA, headSHA, status),
		}}
	default:
		return []Finding{{
			Check:    "build-freshness",
			Severity: Info,
			Message:  fmt.Sprintf("installed binary %s has no embedded build SHA (predates this feature or built without ldflags) — status=%s", binaryPath, status),
		}}
	}
}

// checkBuildFreshnessContract is the real (impure) wiring Doctor() calls: resolves the
// installed binary path the same way install.sh does, git HEAD in repoRoot, and the
// installed binary's own `version` output.
func checkBuildFreshnessContract(repoRoot string) []Finding {
	return checkBuildFreshnessAt(installedBinaryPath(), func() (string, error) { return gitHeadSHA(repoRoot) }, runInstalledVersion)
}

// installedBinaryPath mirrors install.sh: $GOBIN/compound if GOBIN is set, else
// $HOME/.local/bin/compound.
func installedBinaryPath() string {
	if gobin := strings.TrimSpace(os.Getenv("GOBIN")); gobin != "" {
		return filepath.Join(gobin, "compound")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".local", "bin", "compound")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "bin", "compound")
}

func gitHeadSHA(repoRoot string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildFreshnessTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func runInstalledVersion(binaryPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildFreshnessTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, binaryPath, "version").Output()
	return string(out), err
}

// SidecarFreshnessStatus classifies an INSTALLED sidecar script against the repo copy
// install.sh would place. The installed binary carries an embedded build SHA, so
// build-freshness can compare it to HEAD; a shell script carries no such stamp, and
// nothing else compares the two. That gap is not theoretical: on 2026-08-30 a commit
// removed `--registry` from the heartbeat's promote-driver call AND added a CLI refusal
// for that exact flag combination, but the installed copy kept the flag, so every hourly
// autonomy pass exited rc=2 for 28h while build-freshness still reported "fresh".
type SidecarFreshnessStatus string

const (
	SidecarFresh   SidecarFreshnessStatus = "fresh"   // installed bytes match the repo copy
	SidecarStale   SidecarFreshnessStatus = "stale"   // installed bytes differ from the repo copy
	SidecarMissing SidecarFreshnessStatus = "missing" // not installed (autonomy lab is opt-in)
	SidecarUnknown SidecarFreshnessStatus = "unknown" // repo copy unreadable — nothing to compare against
)

// sidecarScript is one file install.sh COPIES out of the repo (and can therefore drift).
// Scripts install.sh merely executes from the repo — sync-ceremony-overlays.sh — cannot
// drift and are deliberately absent here. The two destinations differ: the heartbeat lands
// next to the binary, the attention renderer lands under COMPOUND_HOME.
type sidecarScript struct {
	RepoRel string // repo-relative source install.sh copies from
	DestDir func() string
}

// installedSidecarScripts mirrors the copies install.sh makes when
// COMPOUND_INSTALL_AUTONOMY_LAB=1. Keyed by installed file name.
var installedSidecarScripts = map[string]sidecarScript{
	"compound-autonomy-heartbeat.sh": {
		RepoRel: "scripts/compound-autonomy-heartbeat.sh",
		DestDir: installedBinDir,
	},
	"attention-letter.py": {
		RepoRel: "scripts/attention-letter.py",
		DestDir: installedHomeScriptsDir,
	},
}

// classifySidecarFreshness is the PURE decision: given the installed bytes (nil if the file
// is absent) and the repo bytes (nil if unreadable), decide fresh/stale/missing/unknown.
// Compared byte-for-byte because install.sh copies verbatim — any difference at all means
// the scheduler is running something the repo does not describe.
func classifySidecarFreshness(installed, repo []byte) SidecarFreshnessStatus {
	if installed == nil {
		return SidecarMissing
	}
	if repo == nil {
		return SidecarUnknown
	}
	if string(installed) == string(repo) {
		return SidecarFresh
	}
	return SidecarStale
}

// checkSidecarFreshnessAt is the testable seam: both sides are injected so tests need
// neither a real install nor a real repo. Returns exactly one Finding per script and NEVER
// Fail — a CI clone has no installed sidecar at all, and this gate is CI-portable.
func checkSidecarFreshnessAt(names []string, installedFn, repoFn func(string) ([]byte, error)) []Finding {
	out := make([]Finding, 0, len(names))
	for _, name := range names {
		installed, err := installedFn(name)
		if err != nil {
			installed = nil
		}
		repo, err := repoFn(name)
		if err != nil {
			repo = nil
		}
		status := classifySidecarFreshness(installed, repo)
		finding := Finding{Check: "sidecar-freshness", Severity: Info}
		switch status {
		case SidecarFresh:
			finding.Message = fmt.Sprintf("installed %s matches the repo copy — status=%s", name, status)
		case SidecarStale:
			finding.Severity = Warn
			finding.Message = fmt.Sprintf("installed %s differs from the repo copy — the scheduler is running unversioned bytes; reinstall with COMPOUND_INSTALL_AUTONOMY_LAB=1 ./install.sh — status=%s", name, status)
		case SidecarMissing:
			finding.Message = fmt.Sprintf("%s is not installed (autonomy lab is opt-in) — status=%s", name, status)
		default:
			finding.Message = fmt.Sprintf("repo copy for %s is unreadable — status=%s", name, status)
		}
		out = append(out, finding)
	}
	return out
}

// checkSidecarFreshnessContract is the real wiring Doctor() calls: installed path resolved
// the same way install.sh resolves it, repo path relative to repoRoot.
func checkSidecarFreshnessContract(repoRoot string) []Finding {
	names := make([]string, 0, len(installedSidecarScripts))
	for name := range installedSidecarScripts {
		names = append(names, name)
	}
	sort.Strings(names)
	return checkSidecarFreshnessAt(names,
		func(name string) ([]byte, error) {
			return os.ReadFile(filepath.Join(installedSidecarScripts[name].DestDir(), name))
		},
		func(name string) ([]byte, error) {
			return os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(installedSidecarScripts[name].RepoRel)))
		},
	)
}

// installedBinDir mirrors install.sh's install_dir: $GOBIN if set, else $HOME/.local/bin.
func installedBinDir() string {
	return filepath.Dir(installedBinaryPath())
}

// installedHomeScriptsDir mirrors install.sh's $compound_home/scripts: $COMPOUND_HOME if
// set, else $HOME/.compound.
func installedHomeScriptsDir() string {
	if h := strings.TrimSpace(os.Getenv("COMPOUND_HOME")); h != "" {
		return filepath.Join(h, "scripts")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".compound", "scripts")
	}
	return filepath.Join(os.Getenv("HOME"), ".compound", "scripts")
}
