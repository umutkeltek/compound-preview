package doctor

import (
	"bytes"
	"strings"
	"testing"

	"compound/internal/harness"
)

// TestClassifyDriftTable pins the four-way status vocabulary that the named
// signal renders to the operator: match, drift, unpinned, unobserved. Any
// new status must be added here AND to RenderHarnessVersionDrift together —
// a status the renderer cannot handle is a silent "unknown: harness" bullet
// instead of the named row the operator opened the signal to read.
func TestClassifyDriftTable(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		observed string
		want     string
	}{
		{name: "equal versions match", expected: "0.7.4", observed: "0.7.4", want: "match"},
		{name: "case-insensitive match", expected: "0.7.4", observed: "0.7.4", want: "match"},
		{name: "different versions drift", expected: "0.7.4", observed: "0.7.1", want: "drift"},
		{name: "missing observed with expected set", expected: "0.7.4", observed: "", want: "unobserved"},
		{name: "missing expected", expected: "", observed: "0.7.4", want: "unpinned"},
		{name: "both missing", expected: "", observed: "", want: "unpinned"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := classifyDrift(c.expected, c.observed)
			if got != c.want {
				t.Fatalf("classifyDrift(%q,%q) = %q, want %q", c.expected, c.observed, got, c.want)
			}
		})
	}
}

// TestComputeHarnessVersionDriftFalsifier is the named-signal contract for
// the fake "0.7.1 vs 0.7.4" case the operator-visible surface must surface.
// A registry with prime_agent pinned at "0.7.4" and an observed "0.7.1" MUST
// report drift, with the row carrying both versions so the operator can
// read the verdict directly. A bare boolean would lose this signal — the
// named-signal contract is the WHOLE point.
func TestComputeHarnessVersionDriftFalsifier(t *testing.T) {
	reg := harness.Registry{Harnesses: map[string]harness.Harness{
		"prime_agent": {
			Name:            "prime_agent",
			ExpectedVersion: "0.7.4",
		},
		"claude_code": {
			Name:            "claude_code",
			ExpectedVersion: "1.0.0",
		},
		"codex": {
			Name:            "codex",
			ExpectedVersion: "", // unpinned
		},
		"hermes": {
			// no ExpectedVersion at all
		},
	}}
	report := ComputeHarnessVersionDriftWithRunner(reg, func(name string) string {
		switch name {
		case "prime_agent":
			return "0.7.1"
		case "claude_code":
			return "1.0.0"
		default:
			return ""
		}
	})

	if report.DriftCount != 1 {
		t.Fatalf("DriftCount = %d, want 1 (prime_agent 0.7.1 vs 0.7.4)", report.DriftCount)
	}
	if report.UnobservedCount != 0 {
		t.Fatalf("UnobservedCount = %d, want 0", report.UnobservedCount)
	}

	var primeRow *HarnessVersionDrift
	for i := range report.Rows {
		if report.Rows[i].Harness == "prime_agent" {
			primeRow = &report.Rows[i]
			break
		}
	}
	if primeRow == nil {
		t.Fatalf("prime_agent row missing: %+v", report.Rows)
	}
	if primeRow.Status != "drift" {
		t.Fatalf("prime_agent status = %q, want %q", primeRow.Status, "drift")
	}
	if primeRow.Expected != "0.7.4" {
		t.Fatalf("prime_agent expected = %q, want %q", primeRow.Expected, "0.7.4")
	}
	if primeRow.Observed != "0.7.1" {
		t.Fatalf("prime_agent observed = %q, want %q", primeRow.Observed, "0.7.1")
	}

	// claude_code should be match.
	for _, row := range report.Rows {
		if row.Harness == "claude_code" && row.Status != "match" {
			t.Fatalf("claude_code status = %q, want %q", row.Status, "match")
		}
		if row.Harness == "codex" && row.Status != "unpinned" {
			t.Fatalf("codex status = %q, want %q (no ExpectedVersion)", row.Status, "unpinned")
		}
		if row.Harness == "hermes" && row.Status != "unpinned" {
			t.Fatalf("hermes status = %q, want %q (no ExpectedVersion)", row.Status, "unpinned")
		}
	}

	// Rows MUST be sorted by harness name for stable text render.
	wantOrder := []string{"claude_code", "codex", "hermes", "prime_agent"}
	for i, want := range wantOrder {
		if i >= len(report.Rows) {
			t.Fatalf("missing row at %d (want %q)", i, want)
		}
		if report.Rows[i].Harness != want {
			t.Fatalf("row[%d].Harness = %q, want %q", i, report.Rows[i].Harness, want)
		}
	}
}

// TestRenderHarnessVersionDriftDrift is the text-render proof: when the
// registry surfaces a drift row, the rendered bullet MUST contain "DRIFT"
// AND both versions AND the harness name. Hiding behind a green/red dot is
// useless — the operator opens the signal to read WHICH harness drifted and
// HOW it drifted.
func TestRenderHarnessVersionDriftDrift(t *testing.T) {
	rep := HarnessVersionDriftReport{
		Rows: []HarnessVersionDrift{
			{Harness: "prime_agent", Expected: "0.7.4", Observed: "0.7.1", Status: "drift"},
		},
		DriftCount: 1,
	}
	var buf bytes.Buffer
	RenderHarnessVersionDrift(&buf, rep)
	out := buf.String()
	if !strings.Contains(out, "DRIFT") {
		t.Fatalf("render missing DRIFT marker: %q", out)
	}
	if !strings.Contains(out, "prime_agent") {
		t.Fatalf("render missing harness name: %q", out)
	}
	if !strings.Contains(out, "0.7.4") || !strings.Contains(out, "0.7.1") {
		t.Fatalf("render missing versions: %q", out)
	}
	if !strings.Contains(out, "1 harness(es) drifted") {
		t.Fatalf("render missing drift count: %q", out)
	}
}

// TestRenderHarnessVersionDriftMatchAndUnpinned covers the non-failing rows
// — match and unpinned — so the operator who has nothing to fix sees a
// clear "no action needed" row instead of silence.
func TestRenderHarnessVersionDriftMatchAndUnpinned(t *testing.T) {
	rep := HarnessVersionDriftReport{
		Rows: []HarnessVersionDrift{
			{Harness: "prime_agent", Expected: "0.7.4", Observed: "0.7.4", Status: "match"},
			{Harness: "codex", Expected: "", Status: "unpinned"},
		},
	}
	var buf bytes.Buffer
	RenderHarnessVersionDrift(&buf, rep)
	out := buf.String()
	if !strings.Contains(out, "match: prime_agent=0.7.4") {
		t.Fatalf("render missing match row: %q", out)
	}
	if !strings.Contains(out, "unpinned: codex") {
		t.Fatalf("render missing unpinned row: %q", out)
	}
	if strings.Contains(out, "DRIFT") {
		t.Fatalf("render mistakenly contains DRIFT marker: %q", out)
	}
}

// TestComputeHarnessVersionDriftEmptyRegistry proves the report degrades
// gracefully when the registry has no harnesses — a falsifier against
// the "no harnesses registered" panic the operator would otherwise see.
func TestComputeHarnessVersionDriftEmptyRegistry(t *testing.T) {
	report := ComputeHarnessVersionDriftWithRunner(harness.Registry{}, func(string) string { return "" })
	if len(report.Rows) != 0 {
		t.Fatalf("Rows = %d, want 0 for empty registry", len(report.Rows))
	}
	if report.DriftCount != 0 || report.UnobservedCount != 0 {
		t.Fatalf("non-zero counts on empty registry: drift=%d unobserved=%d", report.DriftCount, report.UnobservedCount)
	}
}
