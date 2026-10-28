package doctor

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"compound/internal/harness"
)

// HarnessVersionDrift is one row of the harness_version_drift named signal.
// It carries the harness id, the expected (operator-pinned) version, the
// observed (binary --version) version, and the status — the same shape the
// binary-staleness signal uses: never a bare boolean, always a triple the
// operator can read directly. A signal that hides behind a green/red dot is
// useless; this struct is what the operator opens to find out WHICH harness
// drifted and HOW it drifted.
//
// Status vocabulary:
//   - "match"      observed == expected (after normalization)
//   - "drift"      observed != expected, both present (the FAILURE case)
//   - "unpinned"   expected is empty — operator has not blessed a version
//   - "unobserved" expected is set, observed is empty (binary missing / --version failed / unparseable)
type HarnessVersionDrift struct {
	Harness  string `json:"harness"`
	Expected string `json:"expected,omitempty"`
	Observed string `json:"observed,omitempty"`
	Status   string `json:"status"`
	Note     string `json:"note,omitempty"`
}

// HarnessVersionDriftReport is the doctor's named-signal payload. It is the
// analog of BinaryStalenessReport: a single struct, one render function, one
// set of operator-readable facts. The harness list is sorted by harness name
// so the text render is stable across runs.
type HarnessVersionDriftReport struct {
	// Rows: one per registered harness, sorted by harness name.
	Rows []HarnessVersionDrift `json:"rows"`
	// DriftCount is the count of rows with Status == "drift". The operator's
	// at-a-glance answer; surfaced in the text render alongside the rows.
	DriftCount int `json:"drift_count"`
	// UnobservedCount is the count of rows with Status == "unobserved" — a
	// pinned harness whose binary the doctor could not probe.
	UnobservedCount int `json:"unobserved_count"`
	// Note carries a single-line explanation when the report is empty or
	// when a global failure prevented per-harness probing.
	Note string `json:"note,omitempty"`
}

// ComputeHarnessVersionDrift walks the registry, recovers the installed
// version for each pinned harness via harness.ObservedVersion, and classifies
// the relation between observed and expected. Pure: the runner hooks
// (observedFn) are injected so tests can substitute a fake without invoking
// real binaries. Production callers should use ComputeHarnessVersionDrift
// (no args) which wires harness.ObservedVersion as the runner.
func ComputeHarnessVersionDrift(reg harness.Registry) HarnessVersionDriftReport {
	return ComputeHarnessVersionDriftWithRunner(reg, harness.ObservedVersion)
}

// ComputeHarnessVersionDriftWithRunner is the test seam. observedFn returns
// the observed version string for a harness name (or "" when unobservable).
// Tests pass a fake to drive specific drift scenarios without touching a real
// prime-agent binary.
func ComputeHarnessVersionDriftWithRunner(
	reg harness.Registry,
	observedFn func(name string) string,
) HarnessVersionDriftReport {
	report := HarnessVersionDriftReport{}
	names := reg.Names()
	// Sort for stable render — registry.Names() already sorts, but assert
	// the contract here so a future change to Names() can't silently reorder
	// the operator-visible signal.
	sort.Strings(names)
	for _, name := range names {
		h, _ := reg.Get(name)
		expected := strings.TrimSpace(h.ExpectedVersion)
		var observed string
		if observedFn != nil {
			observed = strings.TrimSpace(observedFn(name))
		}
		row := HarnessVersionDrift{
			Harness:  name,
			Expected: expected,
			Observed: observed,
			Status:   classifyDrift(expected, observed),
		}
		switch row.Status {
		case "drift":
			report.DriftCount++
		case "unobserved":
			report.UnobservedCount++
		}
		report.Rows = append(report.Rows, row)
	}
	return report
}

// classifyDrift maps (expected, observed) → status. The cases are small
// enough to live as a table-style if-chain rather than a map; readability wins
// over micro-optimization here. Keep the vocabulary narrow: match, drift,
// unpinned, unobserved.
func classifyDrift(expected, observed string) string {
	if expected == "" {
		return "unpinned"
	}
	if observed == "" {
		return "unobserved"
	}
	if strings.EqualFold(expected, observed) {
		return "match"
	}
	return "drift"
}

// RenderHarnessVersionDrift writes the named signal to stdout. Same shape as
// RenderBinaryStaleness: a single section header, one bullet per row, a drift
// count summary at the bottom. Never a bare green/red dot.
func RenderHarnessVersionDrift(stdout io.Writer, rep HarnessVersionDriftReport) {
	fmt.Fprintln(stdout, "\n## harness version drift (installed vs expected)")
	if len(rep.Rows) == 0 {
		if rep.Note != "" {
			fmt.Fprintf(stdout, "- %s\n", rep.Note)
		} else {
			fmt.Fprintln(stdout, "- (no harnesses registered)")
		}
		return
	}
	for _, row := range rep.Rows {
		switch row.Status {
		case "match":
			fmt.Fprintf(stdout, "- match: %s=%s\n", row.Harness, row.Expected)
		case "drift":
			fmt.Fprintf(stdout, "- DRIFT: %s installed=%s expected=%s\n", row.Harness, row.Observed, row.Expected)
		case "unpinned":
			fmt.Fprintf(stdout, "- unpinned: %s (no expected version)\n", row.Harness)
		case "unobserved":
			fmt.Fprintf(stdout, "- unobserved: %s expected=%s — binary missing or --version unparseable\n", row.Harness, row.Expected)
		default:
			fmt.Fprintf(stdout, "- unknown: %s\n", row.Harness)
		}
	}
	if rep.DriftCount > 0 {
		fmt.Fprintf(stdout, "- %d harness(es) drifted from expected\n", rep.DriftCount)
	} else if rep.UnobservedCount > 0 {
		fmt.Fprintf(stdout, "- %d harness(es) pinned but unobserved\n", rep.UnobservedCount)
	}
}
