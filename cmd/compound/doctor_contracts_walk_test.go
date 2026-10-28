package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
)

// S5 (packet 2026-07-17-p2p3-prefix-scan-doctor-walk): lastOrientByHarness
// (doctor_cli.go:515) and lastPeersByHarness (doctor_cli.go:552) each ran an
// independent filepath.WalkDir over contracts/effective. This falsifier
// captures the BASE golden output of buildSurfacingCoverage's
// contracts/effective-derived columns (orient + peers; guard/injection do
// not depend on that directory) on a fixture home covering the join logic,
// the schema filter, and both KINDS of degraded read (a torn/foreign JSON
// body, and an entry filepath.WalkDir cannot open at all), plus a
// missing-both-directories home. The merge into one WalkDir (P3) must
// reproduce these bytes exactly.

// writeJSONFile writes body (already valid JSON text, no trailing newline
// required) to path, creating parent directories as needed.
func writeJSONFileAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// buildContractsWalkFixture seeds a home whose contracts/effective and
// context/packs trees exercise: two EffectiveContracts for the same harness
// (latest-wins), a second harness with a single EffectiveContract, a
// foreign-schema file in contracts/effective (must be ignored), a
// structurally-malformed JSON file in contracts/effective (must be skipped,
// not fatal), a ContextPack with non-empty peers joined to the first
// harness's contract, a ContextPack with EMPTY peers (must not count as
// wired), and a foreign-schema file in context/packs (must be ignored). now
// is fixed so age strings never depend on wall-clock jitter between runs.
func buildContractsWalkFixture(t *testing.T) (home string, now time.Time) {
	t.Helper()
	home = t.TempDir()
	now = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	eff := filepath.Join(home, "contracts", "effective")
	packs := filepath.Join(home, "context", "packs")

	// claude: two contracts, the newer (now-3h) must win over the older (now-10h).
	writeJSONFileAt(t, filepath.Join(eff, "2026-07-17", "ctr_claude_a.json"), fmt.Sprintf(
		`{"schema_version":%q,"id":"ctr_claude_a","harness":"claude","created_at":%q}`,
		envelope.SVEffectiveContract, now.Add(-3*time.Hour).Format(time.RFC3339)))
	writeJSONFileAt(t, filepath.Join(eff, "2026-07-16", "ctr_claude_b.json"), fmt.Sprintf(
		`{"schema_version":%q,"id":"ctr_claude_b","harness":"claude","created_at":%q}`,
		envelope.SVEffectiveContract, now.Add(-10*time.Hour).Format(time.RFC3339)))

	// codex: one contract, no ContextPack ever references it => peers stays absent.
	writeJSONFileAt(t, filepath.Join(eff, "2026-07-16", "ctr_codex_a.json"), fmt.Sprintf(
		`{"schema_version":%q,"id":"ctr_codex_a","harness":"codex","created_at":%q}`,
		envelope.SVEffectiveContract, now.Add(-30*time.Hour).Format(time.RFC3339)))

	// Depth probes: a foreign-schema file and a structurally malformed file, both
	// in the SAME directory as real contracts, must not crash or count.
	writeJSONFileAt(t, filepath.Join(eff, "2026-07-17", "foreign.json"),
		`{"schema_version":"other.schema.v0.1","harness":"claude","created_at":"2026-07-17T00:00:00Z"}`)
	if err := os.WriteFile(filepath.Join(eff, "2026-07-17", "malformed.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write malformed: %v", err)
	}

	// A dangling symlink: filepath.WalkDir's own stat of it errors, exercising
	// the `err != nil` branch (distinct from a readable-but-malformed body).
	if err := os.Symlink(filepath.Join(home, "nowhere.json"), filepath.Join(eff, "2026-07-17", "dangling.json")); err != nil {
		t.Fatalf("symlink dangling: %v", err)
	}

	// claude's newer contract gets a ContextPack with non-empty peers.
	writeJSONFileAt(t, filepath.Join(packs, "pack_claude_a.json"), fmt.Sprintf(
		`{"schema_version":%q,"ids":{"contract_id":"ctr_claude_a"},"created_at":%q,"peers":{"active_leases":[{"a":1}],"lanes":[]}}`,
		envelope.SVContextPack, now.Add(-2*time.Hour).Format(time.RFC3339)))
	// claude's OLDER contract also has a pack, but with EMPTY peers -- must not count.
	writeJSONFileAt(t, filepath.Join(packs, "pack_claude_b.json"), fmt.Sprintf(
		`{"schema_version":%q,"ids":{"contract_id":"ctr_claude_b"},"created_at":%q,"peers":{"active_leases":[],"lanes":[]}}`,
		envelope.SVContextPack, now.Add(-9*time.Hour).Format(time.RFC3339)))
	// A foreign-schema pack must be ignored.
	writeJSONFileAt(t, filepath.Join(packs, "foreign.json"),
		`{"schema_version":"other.pack.v0.1","ids":{"contract_id":"ctr_claude_a"},"peers":{"active_leases":[{"a":1}]}}`)

	return home, now
}

func fixtureHooks() []doctorHook {
	return []doctorHook{
		{Harness: "claude-code", Status: "ok"},
		{Harness: "codex", Status: "drift"},
	}
}

// marshalSurfacing renders the parts of buildSurfacingCoverage's output that
// depend on contracts/effective (orient, orient_age, peers, below_expectation)
// as deterministic, sorted JSON for byte comparison. Guard/injection columns
// are excluded from the pin: they do not depend on the directory this
// falsifier targets, and injection additionally depends on
// capgraph.BuildInjectionObservationReport's own unrelated event scan.
type pinnedRow struct {
	Lane             string   `json:"lane"`
	Harness          string   `json:"harness"`
	Orient           string   `json:"orient"`
	OrientAge        string   `json:"orient_age,omitempty"`
	Peers            string   `json:"peers"`
	BelowExpectation []string `json:"below_expectation,omitempty"`
}

func pinRows(rows []surfacingRow) string {
	pinned := make([]pinnedRow, 0, len(rows))
	for _, r := range rows {
		pinned = append(pinned, pinnedRow{
			Lane: r.Lane, Harness: r.Harness, Orient: r.Orient, OrientAge: r.OrientAge,
			Peers: r.Peers, BelowExpectation: r.BelowExpectation,
		})
	}
	out, err := json.MarshalIndent(pinned, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(out)
}

const wantContractsWalkGolden = `[
  {
    "lane": "claude-code",
    "harness": "claude",
    "orient": "wired",
    "orient_age": "3h",
    "peers": "wired (last 2h)"
  },
  {
    "lane": "codex",
    "harness": "codex",
    "orient": "wired",
    "orient_age": "1d",
    "peers": "absent",
    "below_expectation": [
      "guard"
    ]
  }
]`

// S5: the rich fixture's orient/peers columns, byte-pinned against the base
// (pre-merge, two-WalkDir) implementation. The P3 merge into one WalkDir must
// reproduce this exact JSON.
func TestDoctorContractsEffectiveWalkGoldenRichFixture(t *testing.T) {
	t.Parallel()
	home, now := buildContractsWalkFixture(t)
	rows, _, _ := buildSurfacingCoverage(home, fixtureHooks(), now)
	if got := pinRows(rows); got != wantContractsWalkGolden {
		t.Fatalf("contracts/effective-derived rows changed:\ngot:\n%s\nwant:\n%s", got, wantContractsWalkGolden)
	}
}

const wantContractsWalkGoldenMissingDirs = `[
  {
    "lane": "claude-code",
    "harness": "claude",
    "orient": "never",
    "peers": "absent",
    "below_expectation": [
      "orient"
    ]
  },
  {
    "lane": "codex",
    "harness": "codex",
    "orient": "never",
    "peers": "absent",
    "below_expectation": [
      "guard",
      "orient"
    ]
  }
]`

// S5 (degraded/missing-dir path): neither contracts/effective nor
// context/packs exists at all. filepath.WalkDir's own root-stat error must be
// swallowed exactly as it is today -- no crash, everything reads as
// never/absent -- and the merge must not change that.
func TestDoctorEffectiveContractWalkRecognizesBothSchemaVersions(t *testing.T) {
	homePath := t.TempDir()
	now := time.Now().UTC()
	for i, version := range []string{"compound.effective_contract.v0.1", "compound.effective_contract.v0.2"} {
		id, name := fmt.Sprintf("contract_historical_%d", i), fmt.Sprintf("synthetic-harness-%d", i)
		writeJSONFileAt(t, filepath.Join(homePath, "contracts", "effective", "2026-06-28", id+".json"),
			fmt.Sprintf(`{"schema_version":%q,"id":%q,"harness":%q,"created_at":%q}`, version, id, name, now.Format(time.RFC3339)))
	}
	orient, byContract := scanEffectiveContracts(homePath)
	if len(orient) != 2 || len(byContract) != 2 {
		t.Fatalf("historical/current versions lost: orient=%v contracts=%v", orient, byContract)
	}
}

func TestDoctorContractsEffectiveWalkGoldenMissingDirs(t *testing.T) {
	t.Parallel()
	home := t.TempDir() // no contracts/, no context/ at all
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	rows, _, _ := buildSurfacingCoverage(home, fixtureHooks(), now)
	if got := pinRows(rows); got != wantContractsWalkGoldenMissingDirs {
		t.Fatalf("missing-dir rows changed:\ngot:\n%s\nwant:\n%s", got, wantContractsWalkGoldenMissingDirs)
	}
}
