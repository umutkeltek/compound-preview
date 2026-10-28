package work

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// proofCheckInventoryPath locates cmd/compound/proof_check.go relative to
// the repo root that contains internal/work/. Tests run with cwd set to the
// package directory, so go up two segments to reach the repo root.
func proofCheckInventoryPath(t *testing.T) string {
	t.Helper()
	// internal/work is the test's package directory; the repo root is its
	// parent's parent. Resolving via filepath.Abs keeps the path stable when
	// `go test` is invoked from elsewhere (CI runner, an outer `go test ./...`).
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working dir: %v", err)
	}
	candidate := filepath.Clean(filepath.Join(pwd, "..", "..", "cmd", "compound", "proof_check.go"))
	return candidate
}

// TestCISubsetMembershipIsTotal scans the source-of-truth inventory in
// cmd/compound/proof_check.go and asserts every gate is in exactly one of:
// --ci (CIPortable=true), --ci-console (Dir="console"), or the
// hostBoundGates allowlist. A future gate that lands WITHOUT classification
// fails this test by construction, so the proof-check CI contract cannot
// drift silently.
//
// The test is intentionally a single sweep rather than per-gone case table:
// the failure message lists every unclassified name in one place, and the
// failure surface (5 gates at the time of writing) is read off the diff
// without counting test cases.
func TestCISubsetMembershipIsTotal(t *testing.T) {
	inventory, err := ScanProofCheckStepNames(proofCheckInventoryPath(t))
	if err != nil {
		t.Fatalf("scan proof-check step names: %v", err)
	}
	if len(inventory) == 0 {
		t.Fatalf("scanned empty inventory; check that proofCheckInventoryPath points at cmd/compound/proof_check.go")
	}
	ci, ciConsole, hostBound, unclassified := CISubsetMembership(inventory)
	if len(unclassified) != 0 {
		t.Fatalf(
			"%d proof-check gates run in NO CI subset and are NOT in the hostBound allowlist:\n  unclassified: %s\n  ci: %d\n  ci-console: %d\n  host-bound: %d\nAdd CIPortable=true to the gate, set Dir=\"console\", or extend HostBoundGatesAllowlist with a one-line reason. A silent gap is the bug we are guarding against.",
			len(unclassified),
			strings.Join(unclassified, ", "),
			len(ci),
			len(ciConsole),
			len(hostBound),
		)
	}
	// Sanity: the 52 existing gates plus the required native credential gate;
	// if the inventory rebalances
	// (a real change), the test will not silently zero-out — it will fail
	// loudly with the new count below.
	const expectedInventorySize = 53
	if len(inventory) != expectedInventorySize {
		t.Fatalf(
			"proof-check inventory size changed: got %d, want %d (update this constant deliberately, never silently)",
			len(inventory),
			expectedInventorySize,
		)
	}
	// Sanity: the host-bound allowlist is small and intentional. If it grows
	// past a handful, the rule is being gamed; the test surfaces the growth
	// so a code-reviewer notices.
	// The five historical host dependencies remain, and the new frozen Docker
	// fixture is explicitly host-bound. No existing gate changed classification.
	const expectedHostBoundSize = 6
	if len(hostBound) != expectedHostBoundSize {
		t.Fatalf(
			"host-bound allowlist changed: got %d, want %d (five historical dependencies plus the exact frozen native fixture; review every added or reclassified gate)",
			len(hostBound),
			expectedHostBoundSize,
		)
	}
}

// TestCIPortableLiteralsMatchAllowlist is the guard rail: the three lists
// maintained inside this package MUST agree by name. A duplicate entry
// across lists is treated as unclassified; a missing entry in CIConsoleGateNames
// makes a console gate un-runnable from CI. Drift in either direction is a
// silent gate loss, so the test pins the lists by name.
func TestCIPortableLiteralsMatchAllowlist(t *testing.T) {
	ci := CIPortableGateNames()
	console := CIConsoleGateNames()
	host := HostBoundGatesAllowlist()
	allowNames := make([]string, 0, len(host))
	for _, h := range host {
		if strings.TrimSpace(h.Name) == "" {
			t.Fatalf("host-bound allowlist has an empty Name entry: %+v", h)
		}
		if strings.TrimSpace(h.Reason) == "" {
			t.Fatalf("host-bound allowlist entry %q has an empty Reason", h.Name)
		}
		allowNames = append(allowNames, h.Name)
	}
	// No name in CI ⊕ CIConsole ⊕ HostBound. Doubling up means a gate that
	// claims to run twice under different subsets, which the membership test
	// above would silently allow (first-match wins on ciSet / consoleSet /
	// allowSet). The double-entry check is what makes the rule unambiguous.
	overlap := map[string]string{}
	for _, n := range ci {
		overlap[n] = "ci"
	}
	for _, n := range console {
		if where, dup := overlap[n]; dup {
			t.Fatalf("gate %q appears in BOTH %s and ci-console", n, where)
		}
		overlap[n] = "ci-console"
	}
	for _, n := range allowNames {
		if where, dup := overlap[n]; dup {
			t.Fatalf("gate %q appears in BOTH %s and host-bound", n, where)
		}
	}
	// Sorted contract: hand-maintained lists sorted alphabetically makes
	// the FAIL message about a missing/extra name a one-glance diff against
	// the source inventory.
	if !sort.StringsAreSorted(ci) {
		t.Fatalf("CIPortableGateNames() must be sorted: %v", ci)
	}
	if !sort.StringsAreSorted(console) {
		t.Fatalf("CIConsoleGateNames() must be sorted: %v", console)
	}
	if !sort.StringsAreSorted(allowNames) {
		t.Fatalf("HostBoundGatesAllowlist() names must be sorted: %v", allowNames)
	}
}

// TestCISubsetCoverageReadOnly is a read-only echo for REPORT.md: it lists
// what each subset contains today, in stable order. Reporting -v prints the
// partition; -run alone stays silent. Used at land time to render the gate
// table; not a gate.
func TestCISubsetCoverageReadOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("coverage echo is not a gate")
	}
	inventory, err := ScanProofCheckStepNames(proofCheckInventoryPath(t))
	if err != nil {
		t.Fatalf("scan proof-check step names: %v", err)
	}
	ci, ciConsole, hostBound, unclassified := CISubsetMembership(inventory)
	t.Logf("ci (%d): %s", len(ci), strings.Join(ci, ", "))
	t.Logf("ci-console (%d): %s", len(ciConsole), strings.Join(ciConsole, ", "))
	t.Logf("host-bound (%d): %s", len(hostBound), strings.Join(hostBound, ", "))
	if len(unclassified) != 0 {
		t.Logf("unclassified (%d): %s", len(unclassified), strings.Join(unclassified, ", "))
	}
}
