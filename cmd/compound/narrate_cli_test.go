package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
	"compound/internal/narrate"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

// seedNarrateHome builds a home with one held (needs_you) and one ready work
// item so attention produces a real board for narrate to compose over.
func seedNarrateHome(t *testing.T) string {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID: "work_hold_narrate", Owner: "compound-core", State: "held",
		SuccessPredicate: "held work waits for an operator decision",
	}, cliTestNow); err != nil {
		t.Fatalf("create held work: %v", err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID: "work_ready_narrate", Owner: "compound-core", State: "ready",
		SuccessPredicate: "safe local work can be claimed",
	}, cliTestNow); err != nil {
		t.Fatalf("create ready work: %v", err)
	}
	return homePath
}

// The CLI emits a valid script and leaves both the orient cursor and narration
// sidecar absent: narrate is a pure read projection.
func TestNarrateCLIEmitsScriptAndLeavesOrientCursorUntouched(t *testing.T) {
	homePath := seedNarrateHome(t)
	cursorPath := filepath.Join(homePath, "projections", "orient-operator-last-seq.json")
	sidecarPath := filepath.Join(homePath, "projections", "narrate-thread-seen.json")

	if _, err := os.Stat(cursorPath); !os.IsNotExist(err) {
		t.Fatalf("orient cursor should not exist on a fresh home before narrate: %v", err)
	}

	output := captureStdout(t, func() { cmdNarrate([]string{"--json", "--home", homePath}) })
	var script narrate.Script
	if err := json.Unmarshal([]byte(output), &script); err != nil {
		t.Fatalf("parse narrate JSON: %v\n%s", err, output)
	}
	if script.SchemaVersion != narrate.SchemaVersion {
		t.Fatalf("schema version = %q, want %q", script.SchemaVersion, narrate.SchemaVersion)
	}
	if script.Counts.Narrated+script.Counts.Folded != script.Counts.TotalMembers {
		t.Fatalf("conservation broke through the CLI: %+v", script.Counts)
	}
	if script.ScriptHash == "" {
		t.Fatalf("script_hash missing from CLI output")
	}

	// narrate must NOT advance (or even create) the orient operator cursor.
	if _, err := os.Stat(cursorPath); !os.IsNotExist(err) {
		t.Fatalf("narrate advanced/created the orient cursor (spec §7 forbids it): %v", err)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Fatalf("narrate created a thread-seen sidecar despite its read-only contract: %v", err)
	}
}

// --text renders a felt read: voiced beat lines and a trailing conservation line.
func TestNarrateCLITextRendersConservationLine(t *testing.T) {
	homePath := seedNarrateHome(t)
	out := captureStdout(t, func() { cmdNarrate([]string{"--text", "--home", homePath}) })
	if !strings.Contains(out, "narrated ") || !strings.Contains(out, "· folded ") {
		t.Fatalf("text render missing the conservation line:\n%s", out)
	}
	if !strings.Contains(out, "[") {
		t.Fatalf("text render produced no voiced beat line:\n%s", out)
	}
	t.Logf("narrate --text:\n%s", out)
}

// The same served script is embedded in orient --operator (one builder).
func TestOrientOperatorEmbedsNarrateScript(t *testing.T) {
	homePath := seedNarrateHome(t)
	output := captureStdout(t, func() { cmdOrientOperator(homePath, []string{"--operator", "--json"}) })
	var report struct {
		SchemaVersion string          `json:"schema_version"`
		Narrate       *narrate.Script `json:"narrate"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("parse orient --operator JSON: %v\n%s", err, output)
	}
	if report.Narrate == nil {
		t.Fatalf("orient --operator did not embed a narrate script:\n%s", output)
	}
	if report.Narrate.SchemaVersion != narrate.SchemaVersion {
		t.Fatalf("embedded narrate schema = %q, want %q", report.Narrate.SchemaVersion, narrate.SchemaVersion)
	}
	if report.Narrate.ScriptHash == "" {
		t.Fatalf("embedded narrate script missing script_hash")
	}
}

func trafficBeatFromScript(s narrate.Script) *narrate.Beat {
	for i := range s.Beats {
		if s.Beats[i].ThreadID == "route|cheap-share" {
			return &s.Beats[i]
		}
	}
	return nil
}

// End to end: with no imported provider ledger the cheap-share beat is absent;
// after `route import-provider-ledger` records a mixed ledger, narrate surfaces
// ONE deterministic beat with the real split. This proves the whole chain
// (import -> route-evidence ledger -> narrate) without touching live state.
func TestNarrateCLISurfacesCheapShareFromImportedLedger(t *testing.T) {
	homePath := seedNarrateHome(t)

	// no ledger yet -> beat absent (never a fabricated ratio).
	before := captureStdout(t, func() { cmdNarrate([]string{"--json", "--home", homePath}) })
	var beforeScript narrate.Script
	if err := json.Unmarshal([]byte(before), &beforeScript); err != nil {
		t.Fatalf("parse narrate JSON (before): %v\n%s", err, before)
	}
	if b := trafficBeatFromScript(beforeScript); b != nil {
		t.Fatalf("cheap-share beat present before any provider ledger import: %q", b.Body)
	}

	// import a mixed provider ledger: one zero-cost (cheap) row + one paid row.
	ledger := filepath.Join(t.TempDir(), "provider-calls.ndjson")
	rows := strings.Join([]string{
		`{"request_id":"req_cheap","ts_start":"2026-06-29T09:00:00Z","provider":"fixture-provider","model":"fixture-small","transport":"chat_completions","input_tokens":80,"output_tokens":20,"status":"ok"}`,
		`{"request_id":"req_paid","ts_start":"2026-06-29T10:00:00Z","provider":"fixture-provider","model":"fixture-large","transport":"responses","input_tokens":60,"output_tokens":40,"estimated_cost_usd":0.0025,"status":"ok"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(ledger, []byte(rows), 0o644); err != nil {
		t.Fatalf("write provider ledger fixture: %v", err)
	}
	if _, err := routeevidence.ImportProviderLedgerFile(homePath, ledger, routeevidence.ProviderLedgerImportOptions{}); err != nil {
		t.Fatalf("import provider ledger: %v", err)
	}

	after := captureStdout(t, func() { cmdNarrate([]string{"--json", "--home", homePath}) })
	var afterScript narrate.Script
	if err := json.Unmarshal([]byte(after), &afterScript); err != nil {
		t.Fatalf("parse narrate JSON (after): %v\n%s", err, after)
	}
	beat := trafficBeatFromScript(afterScript)
	if beat == nil {
		t.Fatalf("cheap-share beat absent after importing a provider ledger:\n%s", after)
	}
	// 1 of 2 routed calls ran on a zero-cost lane (50%).
	for _, want := range []string{"1 of 2", "50%", "By spend"} {
		if !strings.Contains(beat.Body, want) {
			t.Fatalf("cheap-share body missing %q: %q", want, beat.Body)
		}
	}
	// the extra stream must not perturb attention conservation.
	if afterScript.Counts.Narrated+afterScript.Counts.Folded != afterScript.Counts.TotalMembers {
		t.Fatalf("cheap-share beat broke conservation through the CLI: %+v", afterScript.Counts)
	}
}

// --mood re-weights but never changes membership or warn presence, end to end.
func TestNarrateCLIMoodReweightsFixedMembership(t *testing.T) {
	homePath := seedNarrateHome(t)
	run := func(mood string) narrate.Script {
		out := captureStdout(t, func() { cmdNarrate([]string{"--json", "--mood", mood, "--home", homePath}) })
		var s narrate.Script
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatalf("parse narrate JSON (%s): %v\n%s", mood, err, out)
		}
		return s
	}
	members := func(s narrate.Script) map[string]bool {
		m := map[string]bool{}
		for _, b := range s.Beats {
			m[b.ThreadID] = true
		}
		for _, g := range s.Standing.Groups {
			m[g.ThreadID] = true
		}
		return m
	}
	d := run("decide")
	y := run("worry")
	md, my := members(d), members(y)
	if len(md) != len(my) {
		t.Fatalf("mood changed membership size: decide=%d worry=%d", len(md), len(my))
	}
	for k := range md {
		if !my[k] {
			t.Fatalf("mood dropped member %q", k)
		}
	}
}
