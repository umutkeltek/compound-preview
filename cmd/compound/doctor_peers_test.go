package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
)

// writeRecordedEffectiveWithID records a minimal EffectiveContract carrying id + harness — the join
// key the peers cell uses to attribute a recorded ContextPack (which carries no harness) back to a
// harness. Mirrors the paired runtime materializer's dated contract convention.
func writeRecordedEffectiveWithID(t *testing.T, home, harness, id string, ts time.Time) {
	t.Helper()
	dir := filepath.Join(home, "contracts", "effective", ts.UTC().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir effective: %v", err)
	}
	body := fmt.Sprintf(`{"schema_version":%q,"id":%q,"harness":%q,"created_at":%q}`,
		envelope.SVEffectiveContract, id, harness, ts.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("write effective: %v", err)
	}
}

// writeRecordedPack records a minimal ContextPack under the same context/packs dir orient records
// to, linked to an effective contract by ids.contract_id, with an optional peers section.
func writeRecordedPack(t *testing.T, home, packID, contractID string, ts time.Time, peersJSON string) {
	t.Helper()
	dir := filepath.Join(home, "context", "packs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir packs: %v", err)
	}
	body := fmt.Sprintf(`{"schema_version":%q,"id":%q,"created_at":%q,"ids":{"contract_id":%q}%s}`,
		envelope.SVContextPack, packID, ts.UTC().Format(time.RFC3339), contractID, peersJSON)
	if err := os.WriteFile(filepath.Join(dir, packID+".json"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("write pack: %v", err)
	}
}

// F4 — the doctor peers cell reads orient-recorded ContextPack evidence: a harness whose latest
// recorded pack carries a non-empty peers section renders `wired (...)`, a harness whose recorded
// pack has no peers renders `absent`, and the static `absent (S2)` placeholder is GONE from the
// code path (both the cell and the note line).
func TestPeersDoctor_F4_CellReadsRecordedPeersEvidence(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := t.TempDir()
	binary := "/opt/compound/compound"
	now := time.Now()

	// codex: recorded pack WITH a non-empty peers section → wired.
	writeRecordedEffectiveWithID(t, home, "codex", "ec_codex", now.Add(-2*time.Hour))
	writeRecordedPack(t, home, "ctx_codex", "ec_codex", now.Add(-2*time.Hour),
		`,"peers":{"active_leases":[{"work_id":"w1","holder":"lane-a","age":"1h","stale":false}],"lanes":[]}`)

	// claude: recorded pack WITHOUT a peers section → absent.
	writeRecordedEffectiveWithID(t, home, "claude-code", "ec_claude", now.Add(-3*time.Hour))
	writeRecordedPack(t, home, "ctx_claude", "ec_claude", now.Add(-3*time.Hour), "")

	rep := runDoctorReport(t, home, base, binary)

	codex, ok := surfacingRowByHarness(rep.Surfacing, "codex")
	if !ok {
		t.Fatalf("no codex row: %+v", rep.Surfacing)
	}
	if !strings.HasPrefix(codex.Peers, "wired") {
		t.Fatalf("codex peers cell should be wired (recorded peers-bearing pack), got %q", codex.Peers)
	}
	claude, ok := surfacingRowByHarness(rep.Surfacing, "claude")
	if !ok {
		t.Fatalf("no claude row: %+v", rep.Surfacing)
	}
	if claude.Peers != "absent" {
		t.Fatalf("claude peers cell should be absent (recorded pack has no peers), got %q", claude.Peers)
	}

	// The static S2 placeholder must be gone from the whole surface (cells + note line).
	text := runDoctorText(t, home, base, binary)
	if strings.Contains(text, "(S2)") {
		t.Fatalf("static (S2) peers placeholder must be gone from doctor output:\n%s", text)
	}
	if strings.Contains(text, "absent (S2)") {
		t.Fatalf("static 'absent (S2)' cell must be gone:\n%s", text)
	}
	if !strings.Contains(text, "wired") {
		t.Fatalf("text should render the codex peers cell as wired:\n%s", text)
	}
}

// surfacingDataRows returns the rendered matrix data rows (between the column header and the first
// note line) from doctor text, trimmed of trailing whitespace.
func surfacingDataRows(text string) []string {
	lines := strings.Split(text, "\n")
	var rows []string
	inTable := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "lane ") { // the column header row
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if strings.HasPrefix(ln, "->") || strings.TrimSpace(ln) == "" {
			break
		}
		rows = append(rows, strings.TrimRight(ln, " "))
	}
	return rows
}

// F5 — matrix row rendering: two lane tuples identical up to the truncation point render as two
// DISTINCT rows (the beyond-truncation difference survives via a disambiguating suffix), while two
// byte-identical rows collapse to a single rendered row. Table stays within its column budget.
func TestPeersMatrix_F5_TruncationDistinctAndByteIdenticalCollapse(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	binary := "/opt/compound/compound"

	t.Run("identical-up-to-truncation-render-distinct", func(t *testing.T) {
		home := t.TempDir()
		store, err := capgraph.Open(home)
		if err != nil {
			t.Fatalf("open capgraph: %v", err)
		}
		// Two lanes sharing a long prefix, differing only near the end (well past a 30-col cut).
		for _, n := range []capgraph.Node{
			{ID: "agent-lane:codex--auto-build--super-long-shared-model-name-alpha--balanced", Kind: "agent-lane",
				Name: "codex / auto/build / super-long-shared-model-name-alpha / balanced", Weight: 0.25, EvidenceOnly: true},
			{ID: "agent-lane:codex--auto-build--super-long-shared-model-name-bravo--balanced", Kind: "agent-lane",
				Name: "codex / auto/build / super-long-shared-model-name-bravo / balanced", Weight: 0.25, EvidenceOnly: true},
		} {
			if err := store.Add(n); err != nil {
				t.Fatalf("add lane: %v", err)
			}
		}
		text := runDoctorText(t, home, base, binary)
		rows := surfacingDataRows(text)
		if len(rows) != 2 {
			t.Fatalf("want 2 distinct rendered rows, got %d: %q", len(rows), rows)
		}
		if rows[0] == rows[1] {
			t.Fatalf("lanes differing only beyond the truncation point must render distinct rows, both = %q", rows[0])
		}
		for _, r := range rows {
			if len(r) > 110 {
				t.Fatalf("rendered row exceeds the ~110 col budget (%d): %q", len(r), r)
			}
		}
	})

	t.Run("byte-identical-rows-collapse", func(t *testing.T) {
		home := t.TempDir()
		store, err := capgraph.Open(home)
		if err != nil {
			t.Fatalf("open capgraph: %v", err)
		}
		// Two DISTINCT node ids that render byte-identically (same name, same harness alias).
		for _, id := range []string{"agent-lane:dup--a", "agent-lane:dup--b"} {
			if err := store.Add(capgraph.Node{ID: id, Kind: "agent-lane", Name: "dup / lane / x / y", Weight: 0.25, EvidenceOnly: true}); err != nil {
				t.Fatalf("add dup lane %s: %v", id, err)
			}
		}
		text := runDoctorText(t, home, base, binary)
		rows := surfacingDataRows(text)
		if len(rows) != 1 {
			t.Fatalf("byte-identical rendered rows must collapse to one, got %d: %q", len(rows), rows)
		}
	})
}
