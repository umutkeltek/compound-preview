package work

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// CIPortableGateNames names every ProofCheckStep whose definition carries
// CIPortable: true at the source-of-truth inventory in
// cmd/compound/proof_check.go (`proofCheckSteps`). These are the gates that
// `compound proof-check --ci` runs: a fresh-clone-portable subset that needs no
// installed binary, no COMPOUND_HOME body, and no console toolchain beyond Go.
// Maintained as a literal here so a future gate added without CIPortable=true
// fails the membership test until the author classifies it.
func CIPortableGateNames() []string {
	return []string{
		"architecture-doctor",
		"diff-check",
		"go-build",
		"go-test",
		"go-test-home",
		"go-vet",
		"gofmt",
		"memory-doctor",
	}
}

// CIConsoleGateNames names every ProofCheckStep whose definition carries
// Dir: "console" at the source-of-truth inventory. These are the gates that
// `compound proof-check --ci-console` runs: smoke tests under console/scripts
// plus console/tsc; they need bun + console/node_modules from a clean checkout.
// Maintained as a literal so a future console gate added without Dir=console
// fails the membership test until the author classifies it.
func CIConsoleGateNames() []string {
	return []string{
		"console-ads-smoke",
		"console-agents-consistency-smoke",
		"console-ai-boundary-smoke",
		"console-ask-triage-smoke",
		"console-attention-consistency-smoke",
		"console-client-bundle-guard-smoke",
		"console-continue-button-smoke",
		"console-coordination-smoke",
		"console-decide-brief-smoke",
		"console-decide-decision-feedback-smoke",
		"console-decision-execute-smoke",
		"console-delegated-sessions-smoke",
		"console-evidence-card-smoke",
		"console-generic-actions-smoke",
		"console-instance-pin-smoke",
		"console-lineage-smoke",
		"console-memory-data-smoke",
		"console-memory-read-honesty-smoke",
		"console-move-type-drift-smoke",
		"console-mutation-gate-smoke",
		"console-mutation-readiness-consumers-smoke",
		"console-private-fallback-smoke",
		"console-private-runtime-guard-smoke",
		"console-projection-degradation-smoke",
		"console-projection-doors-smoke",
		"console-projection-timeout-smoke",
		"console-representation-cluster-guard-smoke",
		"console-representation-falsifiers-smoke",
		"console-smoke",
		"console-standing-law-smoke",
		"console-stream-door-smoke",
		"console-stream-tail-smoke",
		"console-traffic-board-smoke",
		"console-tsc",
		"console-view-registry-smoke",
		"console-work-action-row-smoke",
		"console-work-language-smoke",
		"console-work-legibility-smoke",
		"console-work-trace-smoke",
	}
}

// HostBoundGate describes a ProofCheckStep that does not run in any CI subset
// and is justified to NEVER be classified into one. The reason is a single
// line a triage reader can act on; the gate name is the authority identifier.
//
// Host-bound is a deliberate, documented choice, not a failure: a CI job MUST
// print an explicit `skipped: host-bound: <gate>: <reason>` line for every
// entry here, never silently drop it. The CI subset membership test asserts
// the rule by construction: a gate that runs in --ci or --ci-console does NOT
// need to appear here.
type HostBoundGate struct {
	Name   string
	Reason string
}

// HostBoundGatesAllowlist is the single source of truth for "this gate is
// off-limits for the fresh-clone-portable subset and the console/TS subset,
// and here is why." Adding a gate WITHOUT one of (CIPortable=true, Dir="console")
// at the source inventory OR an explicit entry here breaks the membership test.
//
// Reason-writing rules: name the dependency that makes the gate fresh-clone
// unfriendly (installed binary, live home, host service, vendor-only CLI). One
// line. Do not write "for now", "TODO", or "until X" — that is debt, not a
// reason. If the dependency will eventually go away, write the dep itself
// (host-bound: macOS launchd plist) and let the date follow.
func HostBoundGatesAllowlist() []HostBoundGate {
	return []HostBoundGate{
		{
			Name:   "bun-tools-test",
			Reason: "macOS launchd plist + scheduler state (asc_preflight) — proven gate-skip convention, kept local-battery-only per ciConsoleSteps comment",
		},
		{
			Name:   "codex-pretooluse-native-gate",
			Reason: "runs the codex harness via work.ProofRuntimeExecutable; the inventory gate is not yet flagged CIPortable and a fence-restricted equal-length edit defers the reclassification",
		},
		{
			Name:   "credential-password-alignment-native",
			Reason: "requires the local Docker Unix socket and exact frozen synthetic Linux/PostgreSQL17 prerequisite image; full canonical proof fails when either is absent",
		},
		{
			Name:   "profiles-check",
			Reason: "reads the operator's installed profiles under ~/.claude/agents/ and ~/.codex/agents/; an empty install is not a failing check",
		},
		{
			Name:   "redact-public",
			Reason: "calls the operator-installed redact bun CLI at ~/.bun/bin/redact, which proxies to ~/brain/operating-system/solo-builder-os/tools/redact/cli.ts (outside this repo; not vendored to keep the worktree fence)",
		},
		{
			Name:   "workbench-projection",
			Reason: "reads the populated COMPOUND_HOME work/ ledger; empty home projects emptiness, not failure",
		},
	}
}

// proofCheckStepsNamePattern matches the gate-name field at the source
// inventory in cmd/compound/proof_check.go: every gate is a struct literal
// of the form `Name: "<id>",` inside the []ProofCheckStep slice returned by
// proofCheckSteps. We only match lines whose indentation and trailing comma
// shape match the proven convention; a hand-formatted one-off would slip past
// the membership test, which is correct: the test pins the inventory, not the
// file's whitespace habits.
var proofCheckStepsNamePattern = regexp.MustCompile(`^\s*\{\s*Name:\s*"([^"]+)"`)

// ScanProofCheckStepNames reads the proofCheckSteps inventory from the source
// file at proofCheckPath and returns the ordered, deduplicated list of gate
// names. Used by the CI membership test; not a runtime path. The function
// fails closed on any read or parse error so a missing source file is
// surfaced as a membership test failure, never as a silent "no gates".
func ScanProofCheckStepNames(proofCheckPath string) ([]string, error) {
	file, err := os.Open(proofCheckPath)
	if err != nil {
		return nil, fmt.Errorf("open proof_check inventory: %w", err)
	}
	defer file.Close()

	seen := map[string]bool{}
	var ordered []string
	scanner := bufio.NewScanner(file)
	// Long lines (the inline []string{...} commands run >4 KiB through testify
	// parameterised cases) are not gate-name lines, but readers default to 64K
	// so they survive anyway. Pin it to 1 MiB to keep memory bounded against
	// future giant literals; the regex prunes the noise.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		match := proofCheckStepsNamePattern.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}
		name := strings.TrimSpace(match[1])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		ordered = append(ordered, name)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan proof_check inventory: %w", err)
	}
	return ordered, nil
}

// CISubsetMembership classifies an inventory of gate names into "ci" (runs in
// the fresh-clone-portable subset), "ci-console" (runs in the console subset),
// "host-bound" (allowlist entry with reason), or "unclassified" (none of the
// above — the membership test fails on these by construction).
//
// Unclassified names are the exact failure surface: the audit's "5 unlisted
// gates" are unclassified today; after the allowlist is authored, the set is
// empty. Sorted output so test failures are deterministic across runs.
func CISubsetMembership(names []string) (ci, ciConsole, hostBound, unclassified []string) {
	ciSet := map[string]bool{}
	for _, n := range CIPortableGateNames() {
		ciSet[n] = true
	}
	consoleSet := map[string]bool{}
	for _, n := range CIConsoleGateNames() {
		consoleSet[n] = true
	}
	allowSet := map[string]string{}
	for _, h := range HostBoundGatesAllowlist() {
		allowSet[h.Name] = h.Reason
	}
	for _, name := range names {
		switch {
		case ciSet[name]:
			ci = append(ci, name)
		case consoleSet[name]:
			ciConsole = append(ciConsole, name)
		case allowSet[name] != "":
			hostBound = append(hostBound, name)
		default:
			unclassified = append(unclassified, name)
		}
	}
	sort.Strings(ci)
	sort.Strings(ciConsole)
	sort.Strings(hostBound)
	sort.Strings(unclassified)
	return ci, ciConsole, hostBound, unclassified
}
