package lanematrix

import (
	"os"
	"path/filepath"
	"testing"
)

// writeProjection writes a synthetic conformance projection. Fixture names are
// deliberately synthetic (vendorx/...) -- a realistic-looking fixture is exactly
// where a real product name gets typed into a public repo.
func writeProjection(t *testing.T, dir, file, schema, id, harness, lane, route string) {
	t.Helper()
	body := `{
  "schema": "` + schema + `",
  "conformance_revision": "sha256:fixture",
  "profile": {
    "id": "` + id + `",
    "harness": "` + harness + `",
    "provider_lane": "` + lane + `",
    "route": "` + route + `",
    "capture_policy": {"mode": "observed_fixture"}
  }
}`
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

// A projection announcing an unknown schema must be SKIPPED and REPORTED, never
// read as a lane and never silently dropped. Reading an unreadable input as an
// absence is how a lane with no hooks was recorded as having all of them.
func TestUnknownSchemaIsSkippedAndReported(t *testing.T) {
	dir := t.TempDir()
	writeProjection(t, dir, "good.json", ProjectionSchema, "hx.vendorx-a", "harness-x", "vendorx-a", "vendorx/model-a")
	writeProjection(t, dir, "future.json", "switchback/profile-conformance@99", "hx.vendorx-b", "harness-x", "vendorx-b", "vendorx/model-b")

	lanes, skipped, err := LoadLanes(dir)
	if err != nil {
		t.Fatalf("LoadLanes: %v", err)
	}
	if len(lanes) != 1 {
		t.Fatalf("lanes = %d, want 1 (the future-schema projection must not become a lane): %+v", len(lanes), lanes)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %d, want 1 — a skipped projection must be reported, not swallowed", len(skipped))
	}
}

// The directory listing must not become the enumeration. Containers, non-JSON
// strays, and backup files sharing the namespace are not lanes -- the exact
// over-report that made 52 launcher files look like 52 realized combinations.
func TestDirectoryStraysAreNotLanes(t *testing.T) {
	dir := t.TempDir()
	writeProjection(t, dir, "real.json", ProjectionSchema, "hx.vendorx-a", "harness-x", "vendorx-a", "vendorx/model-a")
	if err := os.Mkdir(filepath.Join(dir, "archive.json"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a projection"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write broken: %v", err)
	}

	lanes, skipped, err := LoadLanes(dir)
	if err != nil {
		t.Fatalf("LoadLanes: %v", err)
	}
	if len(lanes) != 1 {
		t.Fatalf("lanes = %d, want 1; a directory or stray file must never count as a lane: %+v", len(lanes), lanes)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %d, want 1 (the malformed json); got %v", len(skipped), skipped)
	}
}

// A projection with no route has no canonical lane identity, so it cannot be a
// matrix row. Admitting it would let the composite profile id silently stand in
// for the lane name -- the exact conflation docs/NAMING.md forbids.
func TestProjectionWithoutRouteIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeProjection(t, dir, "noroute.json", ProjectionSchema, "hx.vendorx-a", "harness-x", "vendorx-a", "")

	lanes, skipped, err := LoadLanes(dir)
	if err != nil {
		t.Fatalf("LoadLanes: %v", err)
	}
	if len(lanes) != 0 {
		t.Fatalf("lanes = %d, want 0 — a projection with no route is not a lane", len(lanes))
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %d, want 1", len(skipped))
	}
}

// Compound spells harnesses snake_case; the launch owner spells the same family
// with dashes. This test fails if Build compares them raw, which would report a
// genuinely realized combination as unrealized.
func TestRealizedAcrossSpellingBoundary(t *testing.T) {
	lanes := []Lane{{ProfileID: "harness-x.vendorx-a", Route: "vendorx/model-a", Harness: "harness-x"}}
	m := Build([]string{"harness_x.vendorx_a"}, lanes, nil)

	if len(m.Cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(m.Cells))
	}
	if m.Cells[0].State != StateRealized {
		t.Fatalf("state = %q, want %q — snake_case/dash spellings of one harness must compare equal",
			m.Cells[0].State, StateRealized)
	}
	if m.Cells[0].ProfileID != "harness-x.vendorx-a" {
		t.Fatalf("realized cell must carry its binding profile id, got %q", m.Cells[0].ProfileID)
	}
}

// The over-claim guard. An unbound combination must NOT be reported as
// possible: the published projection carries no wire protocol or tap binding,
// so compatibility is undecided. Reporting `possible` would route work to an
// executor that may not be able to speak to the lane at all -- the unsafe
// direction in docs/NAMING.md §Capability.
func TestUnboundCombinationIsUnknownNotPossible(t *testing.T) {
	lanes := []Lane{{ProfileID: "harness-x.vendorx-a", Route: "vendorx/model-a", Harness: "harness-x"}}
	m := Build([]string{"harness_x.vendorx_a", "harness_y"}, lanes, nil)

	var other Cell
	for _, c := range m.Cells {
		if c.Harness == "harness_y" {
			other = c
		}
	}
	if other.State == StatePossible {
		t.Fatal("an unbound combination was reported `possible`; compatibility is not published, so it must be `unknown`")
	}
	if other.State != StateUnknown {
		t.Fatalf("state = %q, want %q", other.State, StateUnknown)
	}
	if other.Reason == "" {
		t.Fatal("an unknown cell must name the missing input, or the caller cannot act on it")
	}
	if len(m.Limits) == 0 {
		t.Fatal("a matrix that could not decide compatibility must say so in Limits")
	}
}

// harnessProfile lets a harness bind to a profile id that is not its own name.
// Without the override, a harness whose identifier differs from its binding
// profile reads as unrealized even though the launch owner published the bind.
func TestHarnessProfileOverrideResolvesRealized(t *testing.T) {
	lanes := []Lane{{ProfileID: "family-a.vendorx-a", Route: "vendorx/model-a", Harness: "family-a"}}
	m := Build([]string{"harness_specific"}, lanes, map[string]string{"harness_specific": "family-a.vendorx-a"})

	if m.Cells[0].State != StateRealized {
		t.Fatalf("state = %q, want %q — a mapped family must resolve to its published binding",
			m.Cells[0].State, StateRealized)
	}
}

func TestCountsSummarizesStates(t *testing.T) {
	lanes := []Lane{
		{ProfileID: "harness-x.a", Route: "vendorx/model-a", Harness: "harness-x"},
		{ProfileID: "harness-x.b", Route: "vendorx/model-b", Harness: "harness-x"},
	}
	m := Build([]string{"harness_x.a", "harness_x.b"}, lanes, nil)
	counts := m.Counts()
	if counts[StateRealized] != 2 {
		t.Fatalf("realized = %d, want 2", counts[StateRealized])
	}
	if counts[StateUnknown] != 2 {
		t.Fatalf("unknown = %d, want 2", counts[StateUnknown])
	}
}
