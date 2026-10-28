package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/finding"
	"compound/internal/home"
	"compound/internal/hookplan"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

// surfacingRowByHarness finds the matrix row whose aliased harness matches (claude|codex|…).
func surfacingRowByHarness(rows []surfacingRow, harness string) (surfacingRow, bool) {
	for _, r := range rows {
		if r.Harness == harness {
			return r, true
		}
	}
	return surfacingRow{}, false
}

func rowBelow(r surfacingRow, col string) bool {
	for _, c := range r.BelowExpectation {
		if c == col {
			return true
		}
	}
	return false
}

// writeSyntheticOrient drops a minimal recorded EffectiveContract for a harness into the home so
// lastOrientByHarness attributes an orient at ts. It carries exactly the fields the reader reads
// (schema_version/harness/created_at), which is the orient-produced evidence class the matrix uses.
func writeSyntheticOrient(t *testing.T, home, harness string, ts time.Time) {
	t.Helper()
	dir := filepath.Join(home, "contracts", "effective", ts.UTC().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir orient dir: %v", err)
	}
	body := fmt.Sprintf(`{"schema_version":%q,"harness":%q,"created_at":%q}`,
		envelope.SVEffectiveContract, harness, ts.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "orient-"+harness+".json"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("write synthetic orient: %v", err)
	}
}

func TestPrintRetentionDisclosesFixedPathCacheScope(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printRetention(&out, doctorReport{Retention: &policy.RetentionScanReport{
		Scanned: []string{"/synthetic/repo"},
	}})
	if !strings.Contains(out.String(), "fixed-path caches: skipped") {
		t.Fatalf("doctor must distinguish a repo-only scan from a fixed-cache scan:\n%s", out.String())
	}
}

// F1 — the matrix mirrors today's real machine: claude guard ABSENT, codex guard DRIFT, both
// flagged below expectation. Built synthetically: install both harness hooks with one binary,
// delete the claude config (=> absent), then check with a different binary (=> codex drifts).
func TestDoctorSurfacing_F1_GuardAbsentAndDrift(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := t.TempDir()
	binaryA := "/opt/compound-a/compound"
	binaryB := "/opt/compound-b/compound"

	if _, err := hookplan.InstallHooks(base, binaryA, "", nil, false); err != nil {
		t.Fatalf("install hooks: %v", err)
	}
	if err := os.Remove(filepath.Join(base, hookplan.ConfigRel("claude-code", ""))); err != nil {
		t.Fatalf("remove claude config: %v", err)
	}

	rep := runDoctorReport(t, home, base, binaryB)

	claude, ok := surfacingRowByHarness(rep.Surfacing, "claude")
	if !ok {
		t.Fatalf("no claude row in matrix: %+v", rep.Surfacing)
	}
	if claude.Guard != "absent" || !rowBelow(claude, "guard") {
		t.Fatalf("claude guard should be absent+below, got %q below=%v", claude.Guard, claude.BelowExpectation)
	}
	codex, ok := surfacingRowByHarness(rep.Surfacing, "codex")
	if !ok {
		t.Fatalf("no codex row in matrix: %+v", rep.Surfacing)
	}
	if codex.Guard != "drift" || !rowBelow(codex, "guard") {
		t.Fatalf("codex guard should be drift+below, got %q below=%v", codex.Guard, codex.BelowExpectation)
	}
	// The trailing "!" is a render of below-expectation; prove the text agrees.
	text := runDoctorText(t, home, base, binaryB)
	if !strings.Contains(text, "absent!") || !strings.Contains(text, "drift!") {
		t.Fatalf("text should mark absent!/drift!:\n%s", text)
	}
}

// F2 — a harness with no recorded orient shows orient:never; after a synthetic orient artifact is
// written for it, the same harness flips to orient:wired and drops "orient" from below-expectation.
// A foreign-schema file in the same tree must NOT count (depth probe on the schema filter).
func TestDoctorSurfacing_F2_OrientNeverThenWired(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := t.TempDir()
	binary := "/opt/compound/compound"

	before := runDoctorReport(t, home, base, binary)
	codex, ok := surfacingRowByHarness(before.Surfacing, "codex")
	if !ok {
		t.Fatalf("no codex row: %+v", before.Surfacing)
	}
	if codex.Orient != "never" || !rowBelow(codex, "orient") {
		t.Fatalf("codex orient should be never+below, got %q below=%v", codex.Orient, codex.BelowExpectation)
	}

	// A non-EffectiveContract file must be ignored by the orient scan.
	foreignDir := filepath.Join(home, "contracts", "effective", "2026-07-14")
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(foreignDir, "foreign.json"), []byte(`{"schema_version":"other","harness":"codex","created_at":"2026-07-14T10:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("write foreign: %v", err)
	}
	stillNever := runDoctorReport(t, home, base, binary)
	if r, _ := surfacingRowByHarness(stillNever.Surfacing, "codex"); r.Orient != "never" {
		t.Fatalf("foreign-schema file must not count as orient, got %q", r.Orient)
	}

	// Now a real orient artifact for codex, three hours ago.
	writeSyntheticOrient(t, home, "codex", time.Now().Add(-3*time.Hour))
	after := runDoctorReport(t, home, base, binary)
	codex2, _ := surfacingRowByHarness(after.Surfacing, "codex")
	if codex2.Orient != "wired" {
		t.Fatalf("codex orient should be wired after synthetic orient, got %q", codex2.Orient)
	}
	if rowBelow(codex2, "orient") {
		t.Fatalf("wired orient must not be below expectation: %v", codex2.BelowExpectation)
	}
	if codex2.OrientAge == "" {
		t.Fatalf("wired orient should carry a coarse age")
	}
}

// F3 — a fresh, empty home (no capgraph, no hooks) still renders a non-empty matrix: fallback
// harness rows plus the explicit no-lanes note, exit 0, no crash.
func TestDoctorSurfacing_F3_FreshHomeFallbackRows(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := t.TempDir() // empty base => both guards absent

	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", base, "--binary", "/opt/compound/compound", "--json"}
	code := runDoctor(args, &out, &errOut)
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse json: %v (%q)", err, out.String())
	}
	// Doctor exit-code contract: see runDoctorReport's header. Accept
	// either exit code; the structural assertions below are the test.
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("fresh-home doctor returned exit %d with no problems, stderr=%q", code, errOut.String())
	}
	if len(rep.Surfacing) < 2 {
		t.Fatalf("fresh home must show fallback rows, got %d", len(rep.Surfacing))
	}
	if rep.SurfacingNote != "no agent lanes imported — showing hook harnesses only" {
		t.Fatalf("fresh home should carry the no-lanes note, got %q", rep.SurfacingNote)
	}
	if _, ok := surfacingRowByHarness(rep.Surfacing, "claude"); !ok {
		t.Fatalf("fallback rows must include claude: %+v", rep.Surfacing)
	}
	if _, ok := surfacingRowByHarness(rep.Surfacing, "codex"); !ok {
		t.Fatalf("fallback rows must include codex: %+v", rep.Surfacing)
	}
	if rep.SurfacingInjection.Scope != "global" || rep.SurfacingInjection.LastConsumed != "never" {
		t.Fatalf("fresh-home injection should be global/never, got %+v", rep.SurfacingInjection)
	}
}

// F4 — --json exposes the surfacing array with stable field names, and the text render agrees with
// the JSON (same guard tokens, same global injection numbers, same S2 peers caveat).
func TestDoctorSurfacing_F4_JSONAndTextAgree(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := t.TempDir()
	binary := "/opt/compound/compound"

	var jsonOut, errOut bytes.Buffer
	jsonArgs := []string{"--home", home, "--base", base, "--binary", binary, "--json"}
	code := runDoctor(jsonArgs, &jsonOut, &errOut)
	var rep doctorReport
	if err := json.Unmarshal(jsonOut.Bytes(), &rep); err != nil {
		t.Fatalf("parse json: %v", err)
	}
	// Doctor exit-code contract: see runDoctorReport's header.
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("json doctor returned exit %d with no problems: %s", code, errOut.String())
	}
	raw := jsonOut.String()
	for _, field := range []string{`"surfacing"`, `"surfacing_injection"`, `"guard"`, `"orient"`, `"peers"`} {
		if !strings.Contains(raw, field) {
			t.Fatalf("json missing stable field %s:\n%s", field, raw)
		}
	}

	text := runDoctorText(t, home, base, binary)
	if !strings.Contains(text, "## surfacing coverage") {
		t.Fatalf("text missing surfacing section:\n%s", text)
	}
	// The JSON↔text agreement invariant has never changed; the numbers it agrees ON have changed
	// twice. It first pinned `served <total>, consumed <n>` (a bare pair that reads as a rate),
	// then a measured/unmeasured decomposition (a rate over a denominator the receipt cannot
	// support — see TestDoctorInjectionReportsNoRateBecauseConsumptionProvenanceIsUnrecoverable).
	// It now pins counts with the session counts they came from, and no ratio at all.
	injLine := fmt.Sprintf("served %d event(s) across %d session(s), consumed %d event(s) across %d session(s)",
		rep.SurfacingInjection.ServedEvents,
		rep.SurfacingInjection.SessionsServed,
		rep.SurfacingInjection.ConsumedEvents,
		rep.SurfacingInjection.SessionsWithConsumption)
	if !strings.Contains(text, injLine) {
		t.Fatalf("text injection numbers must match json (%s):\n%s", injLine, text)
	}
	if !strings.Contains(text, "global") {
		t.Fatalf("text must state the injection scope is global:\n%s", text)
	}
	// Slice C flips the peers cell from the static "absent (S2)" placeholder to an evidence read:
	// the S2 caveat is gone and the peers column is now a real state (absent on a fresh home).
	if strings.Contains(text, "(S2)") {
		t.Fatalf("static (S2) peers caveat must be gone:\n%s", text)
	}
	if !strings.Contains(text, "peers") {
		t.Fatalf("text must carry the peers column/note:\n%s", text)
	}
	// Each row's guard and peers tokens from JSON appear in the text.
	for _, r := range rep.Surfacing {
		if !strings.Contains(text, r.Guard) {
			t.Fatalf("guard token %q from json row missing in text:\n%s", r.Guard, text)
		}
		if !strings.Contains(text, r.Peers) {
			t.Fatalf("peers token %q from json row missing in text:\n%s", r.Peers, text)
		}
	}
}

// When agent lanes are imported into the capgraph, the matrix rows are the lane nodes (not the
// fallback bare harnesses) and the no-lanes note is absent — while the guard/orient columns still
// join by the lane's aliased harness. Covers the imported-lanes branch of buildSurfacingCoverage.
func TestDoctorSurfacing_LaneRowsFromCapgraph(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := t.TempDir()
	binary := "/opt/compound/compound"

	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatalf("open capgraph: %v", err)
	}
	for _, n := range []capgraph.Node{
		{ID: "agent-lane:codex--auto-build--gpt--balanced", Kind: "agent-lane", Name: "codex / auto/build / gpt / balanced", Weight: 0.25, EvidenceOnly: true},
		{ID: "agent-lane:claude-code--auto-build--claude--balanced", Kind: "agent-lane", Name: "claude-code / auto/build / claude / balanced", Weight: 0.25, EvidenceOnly: true},
	} {
		if err := store.Add(n); err != nil {
			t.Fatalf("seed lane %s: %v", n.ID, err)
		}
	}

	rep := runDoctorReport(t, home, base, binary)
	if rep.SurfacingNote != "" {
		t.Fatalf("imported lanes => no fallback note, got %q", rep.SurfacingNote)
	}
	if len(rep.Surfacing) != 2 {
		t.Fatalf("want 2 lane rows, got %d: %+v", len(rep.Surfacing), rep.Surfacing)
	}
	for _, r := range rep.Surfacing {
		if !strings.Contains(r.Lane, " / ") {
			t.Fatalf("row lane should be a lane name, got %q", r.Lane)
		}
	}
	if _, ok := surfacingRowByHarness(rep.Surfacing, "codex"); !ok {
		t.Fatalf("codex lane row missing: %+v", rep.Surfacing)
	}
	if _, ok := surfacingRowByHarness(rep.Surfacing, "claude"); !ok {
		t.Fatalf("claude lane row missing: %+v", rep.Surfacing)
	}
}

func TestDoctorSurfacingClaudeZaiLaneJoinsItsOwnGuardAndOrient(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := t.TempDir()
	binary := "/opt/compound/compound"
	profileDir := hookplan.ClaudeZaiProfileDir(base)
	if _, err := hookplan.InstallHooksWithConfigDir(base, binary, "global", []string{"claude-zai"}, profileDir, false); err != nil {
		t.Fatalf("install claude-zai hooks: %v", err)
	}
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(capgraph.Node{
		ID: "agent-lane:claude-zai--zai-direct--inherit--balanced", Kind: "agent-lane",
		Name: "claude-zai / zai-direct / inherit / balanced", Weight: 0.25, EvidenceOnly: true,
	}); err != nil {
		t.Fatal(err)
	}
	writeSyntheticOrient(t, home, "claude_zai", time.Now().Add(-time.Hour))
	rep := runDoctorReport(t, home, base, binary)
	zai, ok := surfacingRowByHarness(rep.Surfacing, "claude-zai")
	if !ok {
		t.Fatalf("claude-zai row missing from doctor: %+v", rep.Surfacing)
	}
	if zai.Guard != "wired" || zai.Orient != "wired" || len(zai.BelowExpectation) != 0 {
		t.Fatalf("claude-zai surfacing = %+v, want independently wired guard+orient", zai)
	}
	if _, native := surfacingRowByHarness(rep.Surfacing, "claude"); native {
		t.Fatalf("claude-zai lane collapsed into native Claude row: %+v", rep.Surfacing)
	}
}

// runDoctorReport runs doctor with --json and returns the parsed report.
// Doctor exit-code contract (packet work_3da5d3c76707): exit 0 means no
// problems; exit 1 means at least one. Existing in-process tests
// exercise doctor on fixtures that may legitimately contain problems
// the doctor correctly surfaces (e.g. harness version drift on the
// developer's machine). This helper accepts either exit code so those
// tests' structural assertions remain the thing under test; the
// dedicated exit-code contract tests live in doctor_exit_truth_test.go.
func runDoctorReport(t *testing.T, home, base, binary string) doctorReport {
	t.Helper()
	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", base, "--binary", binary, "--json"}
	code := runDoctor(args, &out, &errOut)
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse doctor json: %v (%q)", err, out.String())
	}
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("doctor --json returned exit %d with no problems reported, stderr=%q", code, errOut.String())
	}
	return rep
}

// runDoctorText runs doctor in text mode and returns stdout. Text mode
// has no JSON to inspect, so we cannot check Problems; the dedicated
// exit-code contract tests live in doctor_exit_truth_test.go and run
// the doctor as a subprocess where the real exit code is observable.
func runDoctorText(t *testing.T, home, base, binary string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", base, "--binary", binary}
	_ = runDoctor(args, &out, &errOut)
	return out.String()
}

// compound doctor composes existing organ states into one read-only surface. This proves the
// composition end-to-end: an empty home reports hooks-absent + an install hint, lease-free, zero
// findings, and the fail-open placement default; recording a finding plus a superseding revision
// moves the live/superseded counts. It writes nothing.
func TestDoctorComposesOrganState(t *testing.T) {
	// Determinism: placement mode is overridable by env (policy.applyPlacementGuardEnv reads
	// COMPOUND_PLACEMENT_GUARD_MODE / PLACEMENT_GUARD_MODE). Clear both so this test always exercises
	// the built-in default (observe) regardless of the operator's ambient shell — the env-dependence
	// is what made this assertion flip between "block" and "observe" across environments.
	t.Setenv("COMPOUND_PLACEMENT_GUARD_MODE", "")
	t.Setenv("PLACEMENT_GUARD_MODE", "")

	home := t.TempDir()
	base := t.TempDir() // empty base => no harness hooks installed

	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", base, "--binary", "/usr/local/bin/compound", "--json"}
	code := runDoctor(args, &out, &errOut)
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse doctor json: %v (%q)", err, out.String())
	}
	// Doctor exit-code contract: see runDoctorReport's header.
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("doctor returned exit %d with no problems, stderr=%q", code, errOut.String())
	}
	if len(rep.Hooks) == 0 {
		t.Fatal("doctor should report per-harness hook status")
	}
	for _, h := range rep.Hooks {
		if h.Status != "absent" {
			t.Fatalf("empty base => hook %s should be absent, got %q", h.Harness, h.Status)
		}
	}
	if rep.HookHint == "" {
		t.Fatal("absent hooks should yield an install hint")
	}
	if rep.LeaseHeld {
		t.Fatal("no lease should be held")
	}
	// policy.DefaultConfig and an empty-home policy.LoadConfig use observe. The block assertion in
	// TestLoadConfigOverlaysDefaultPolicy follows an explicit policy.yaml fixture; it is not the
	// built-in default. Doctor must report the loaded mode and make fail-open staging explicit.
	if rep.PlacementMode != "observe" {
		t.Fatalf("empty-home placement mode should be observe, got %q", rep.PlacementMode)
	}
	if rep.PlacementNote == "" {
		t.Fatal("observe mode should carry an explicit fail-open warning")
	}
	if rep.FindingsTotal != 0 {
		t.Fatalf("no findings expected, got %d", rep.FindingsTotal)
	}

	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	f1, err := finding.Record(home, finding.Finding{Topic: "deploy-runbook", Body: "v1 steps"}, now)
	if err != nil {
		t.Fatalf("record f1: %v", err)
	}
	if _, err := finding.Record(home, finding.Finding{Topic: "deploy-runbook", Body: "v2 steps", Supersedes: f1.ID}, now.Add(time.Minute)); err != nil {
		t.Fatalf("record f2: %v", err)
	}

	out.Reset()
	errOut.Reset()
	code = runDoctor(args, &out, &errOut)
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse doctor json (rerun): %v", err)
	}
	// Doctor exit-code contract: see runDoctorReport's header.
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("doctor rerun returned exit %d with no problems, stderr=%q", code, errOut.String())
	}
	if rep.FindingsTotal != 2 || rep.FindingsLive != 1 || rep.FindingsSuperseded != 1 {
		t.Fatalf("findings counts wrong: total=%d live=%d superseded=%d", rep.FindingsTotal, rep.FindingsLive, rep.FindingsSuperseded)
	}
}

// TestFindingRecordPersistsEveryEvidenceFlag replays the multi --evidence CLI
// invocation that silently dropped all but the first ref (live repro
// find_ddcc04dc0641: 3 of 4 refs lost). It falsifies the single-valueFlag parse
// and proves the recorded finding carries -- and finding show reads back -- every
// ref that was passed.
func TestFindingRecordPersistsEveryEvidenceFlag(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	now := time.Date(2026, 7, 16, 20, 0, 0, 0, time.UTC)

	args := []string{
		"--topic", "multi-ev",
		"--body", "four evidence refs passed at record time",
		"--evidence", "commit:aaa111",
		"--evidence", "check:bbb222",
		"--evidence", "land-check:ccc333",
		"--evidence", "live:ddd444",
	}

	want := []string{"commit:aaa111", "check:bbb222", "land-check:ccc333", "live:ddd444"}
	refs := findingEvidenceRefs(args)
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("findingEvidenceRefs parsed %v, want all four %v", refs, want)
	}

	rec, err := finding.Record(home, finding.Finding{
		Topic:        valueFlag(args, "--topic"),
		Body:         valueFlag(args, "--body"),
		EvidenceRefs: refs,
	}, now)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	shown, err := finding.Show(home, rec.ID)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !reflect.DeepEqual(shown.EvidenceRefs, want) {
		t.Fatalf("finding show evidence = %v, want %v (only the first ref surviving is the original bug)", shown.EvidenceRefs, want)
	}
}

// TestFindingEvidenceRefsCommaAndRepeatBackCompat proves the comma-joined form
// still works, mixes with repeated flags, and drops empty/whitespace refs.
func TestFindingEvidenceRefsCommaAndRepeatBackCompat(t *testing.T) {
	t.Parallel()
	got := findingEvidenceRefs([]string{
		"--evidence", "a,b",
		"--evidence", " c ",
		"--evidence", "",
		"--evidence", "d,,e",
	})
	want := []string{"a", "b", "c", "d", "e"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("comma/repeat/empty handling = %v, want %v", got, want)
	}
}

// -----------------------------------------------------------------------
// Sweep-teeth doctor surface (DEC-0027 build step 1, work_195771b63a58):
// doctor must report (a) how many non-terminal attempts are stuck past
// DefaultSweepThreshold and (b) how many were swept as ambiguous (so the
// operator knows what to reconcile). These are read off the on-disk
// run_attempts JSON in the same walk that counts RunAttemptsStuck; the
// test pins the shape so a future refactor doesn't silently drop the
// fields.
// -----------------------------------------------------------------------

// doctorReport is duplicated from doctor_cli.go so this test can decode
// without pulling the unexported alias; both shapes are the same struct
// (doctorReport) by construction. If the test fails to compile because
// the doctor field shape drifted, mirror the change here.

func TestDoctorReportsSweepTeethCounters(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := t.TempDir()

	// Stage four attempts in the doctor's home:
	//   1. STUCK + fresh heartbeat      → counts in Stuck, NOT in StuckPastTTL
	//   2. STUCK + stale                → counts in Stuck AND StuckPastTTL
	//   3. ABANDONED + classification=ambiguous → counts in SweptAmbiguous
	//   4. ABANDONED + classification=side_effect_free → does NOT count
	//
	// Doctor uses time.Now() as its "now", so we anchor the stale attempt
	// to a long-ago CreatedAt and the fresh attempt to a recent Heartbeat
	// (rather than to a fixed t0) — that way the "past TTL" boundary
	// stays correct regardless of when the test runs.
	now := time.Now().UTC()
	store := runattempt.Open(home)

	// 1. Stuck, fresh heartbeat → excluded from StuckPastTTL.
	fresh, err := store.Mint("work_fresh", "actor", "fake", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Heartbeat(fresh.ID, now.Add(-1*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// 2. Stuck, no heartbeat, well past TTL (2h old > 30m threshold).
	stale, err := store.Mint("work_stale", "actor", "fake", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(stale.ID, runattempt.OriginClaimReservation); err != nil {
		t.Fatal(err)
	}

	// 3. Already swept as ambiguous.
	amb, err := store.Mint("work_amb", "actor", "fake", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetTranscript(amb.ID, "transcripts/"+amb.ID+".json"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteWithReasonAndClassification(
		amb.ID,
		runattempt.StatusAbandoned,
		"stale: ambiguous — no activity for 2h0m0s (threshold 30m0s)",
		runattempt.SweepClassificationAmbiguous,
		now.Add(-1*time.Hour),
	); err != nil {
		t.Fatal(err)
	}

	// 4. Already swept as side_effect_free (NOT ambiguous).
	clean, err := store.Mint("work_clean", "actor", "fake", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(clean.ID, runattempt.OriginClaimReservation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteWithReasonAndClassification(
		clean.ID,
		runattempt.StatusAbandoned,
		"stale: side_effect_free — no activity for 2h0m0s (threshold 30m0s)",
		runattempt.SweepClassificationSideEffectFree,
		now.Add(-1*time.Hour),
	); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", base, "--binary", "/opt/compound/compound", "--json"}
	code := runDoctor(args, &out, &errOut)
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse json: %v (%q)", err, out.String())
	}
	// Doctor exit-code contract: see runDoctorReport's header.
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("doctor exit=%d with no problems reported, stderr=%q", code, errOut.String())
	}

	// Stuck = 2 (fresh + stale; clean and amb are terminal).
	if rep.RunAttemptsStuck != 2 {
		t.Fatalf("RunAttemptsStuck = %d, want 2 (fresh + stale)", rep.RunAttemptsStuck)
	}
	// StuckPastTTL = 1 (only stale; fresh was heartbeated 1m ago).
	if rep.RunAttemptsStuckPastTTL != 1 {
		t.Fatalf("RunAttemptsStuckPastTTL = %d, want 1 (only stale)", rep.RunAttemptsStuckPastTTL)
	}
	if rep.RunAttemptsStuckPastTTLOldest == "" {
		t.Fatalf("RunAttemptsStuckPastTTLOldest must be populated when StuckPastTTL > 0")
	}
	// SweptAmbiguous = 1 (only amb; clean is side_effect_free).
	if rep.RunAttemptsSweptAmbiguous != 1 {
		t.Fatalf("RunAttemptsSweptAmbiguous = %d, want 1 (amb only; clean is side_effect_free)", rep.RunAttemptsSweptAmbiguous)
	}
	if len(rep.RunAttemptsSweptAmbiguousIDs) != 1 || rep.RunAttemptsSweptAmbiguousIDs[0] != amb.ID {
		t.Fatalf("SweptAmbiguousIDs = %v, want [%s]", rep.RunAttemptsSweptAmbiguousIDs, amb.ID)
	}
}

// Falsifier: a fresh home (no run_attempts dir) must report zero across
// the new counters, not panic or surface a stale default. The doctor
// continues to render the existing stuck-count zero, and the new fields
// must join it cleanly.
func TestDoctorReportsSweepTeethCountersOnFreshHome(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := t.TempDir()

	var out, errOut bytes.Buffer
	args := []string{"--home", home, "--base", base, "--binary", "/opt/compound/compound", "--json"}
	code := runDoctor(args, &out, &errOut)
	var rep doctorReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("parse json: %v", err)
	}
	if code != 0 && len(rep.Problems) == 0 {
		t.Fatalf("fresh-home doctor exit=%d with no problems reported, stderr=%q", code, errOut.String())
	}
	if rep.RunAttemptsStuck != 0 || rep.RunAttemptsStuckPastTTL != 0 || rep.RunAttemptsSweptAmbiguous != 0 {
		t.Fatalf("fresh home: stuck=%d past_ttl=%d swept_ambiguous=%d, want all 0",
			rep.RunAttemptsStuck, rep.RunAttemptsStuckPastTTL, rep.RunAttemptsSweptAmbiguous)
	}
	if len(rep.RunAttemptsSweptAmbiguousIDs) != 0 {
		t.Fatalf("fresh home: SweptAmbiguousIDs should be empty, got %v", rep.RunAttemptsSweptAmbiguousIDs)
	}
}

func TestDoctorReportsUnreadableRunAttemptLaunchEvidence(t *testing.T) {
	t.Parallel()
	for _, corruption := range []string{"malformed-json", "unsafe-mode"} {
		t.Run(corruption, func(t *testing.T) {
			homePath := t.TempDir()
			base := t.TempDir()
			created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
			store := runattempt.Open(homePath)
			attempt, err := store.Mint("work_doctor_unreadable_"+corruption, "actor:test", "harness:test", created)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			preparedPath := filepath.Join(homePath, "run_attempts", "launch", attempt.ID+".prepared.json")
			switch corruption {
			case "malformed-json":
				if err := os.WriteFile(preparedPath, []byte("{\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(preparedPath, 0o600); err != nil {
					t.Fatal(err)
				}
			case "unsafe-mode":
				if err := os.Chmod(preparedPath, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var out, errOut bytes.Buffer
			args := []string{"--home", homePath, "--base", base, "--binary", "/opt/compound/compound", "--json"}
			// The unreadable launch evidence we seeded IS a real problem;
			// the doctor's exit-1 is correct on this fixture. We still
			// assert the structural shape (count + ids) below.
			code := runDoctor(args, &out, &errOut)
			var rep struct {
				RunAttemptsUnreadable    int      `json:"run_attempts_unreadable"`
				RunAttemptsUnreadableIDs []string `json:"run_attempts_unreadable_ids"`
			}
			if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
				t.Fatalf("parse json: %v (%q)", err, out.String())
			}
			_ = code
			if rep.RunAttemptsUnreadable != 1 || len(rep.RunAttemptsUnreadableIDs) != 1 || rep.RunAttemptsUnreadableIDs[0] != attempt.ID {
				t.Fatalf("doctor unreadable evidence = %+v, want count=1 id=%s", rep, attempt.ID)
			}
		})
	}
}

// seedInjectionSession writes one ContextPack carrying servedClaims in active_memory plus the
// receipt that closed it: ids.context_pack_id joins the two. ApplicationMethod distinguishes
// an explicit application declaration from historical Used/UsedDeclared ambiguity; a readable
// transcript or typed declaration alone does not establish use of other served claims.
func seedInjectionSession(t *testing.T, homePath, recID, packID string, createdAt time.Time, servedClaims, consumedClaims []string, usedDeclared bool, applicationMethod string) {
	t.Helper()
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            packID,
			CreatedAt:     createdAt.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "compound"},
			Authority:     envelope.Authority{SourceClass: "generated", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD:          "injection honesty fixture",
		Included:           []envelope.IncludedRef{},
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       servedClaims,
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("fixture pack %s invalid: %v", packID, err)
	}
	b, err := json.MarshalIndent(pack, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	packPath := filepath.Join(homePath, "context", "packs", packID+".json")
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packPath, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if consumedClaims == nil {
		consumedClaims = []string{}
	}
	rec := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            recID,
			CreatedAt:     createdAt.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: "work_" + recID, ContextPackID: packID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    "completed",
		MoveType:   "outward_delta",
		StateDelta: envelope.StateDelta{Changed: true, Summary: "fixture", FilesChanged: []string{}, Artifacts: []string{}, Verification: []string{}},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Used: consumedClaims, Missed: []string{}, Candidate: []string{}, UsedDeclared: usedDeclared, ApplicationMethod: applicationMethod},
		Memory:     envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs:  []string{"check:fixture"},
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("fixture receipt %s invalid: %v", recID, err)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(rec.ID, &rec, " "); err != nil {
		t.Fatal(err)
	}
}

// SUPERSEDED 2026-08-11 — TestDoctorInjectionRollupSeparatesMeasuredFromUnmeasuredSessions was
// deleted here, not weakened, because it asserted a contract that turned out to be false.
//
// It required served claim-events to split into "measured" and "unmeasured" by
// Receipt.Skills.UsedDeclared, on my reading that used_declared=true meant "the session-end
// consumption detector ran". It does not: cmd/compound/work_cli.go sets it from a typed --used
// flag and internal/hook/hook.go sets it from the detector, and the receipt records neither. So
// the test pinned a denominator the data cannot support, and on the real home it would have
// certified 425 "measured" events of which 420 were CLI closes that measured nothing.
//
// A falsifier that encodes a wrong inference does not fail loudly — it defends the inference
// against its own fix. Its replacement is the test below, which keeps the part that was always
// right (unknown is never rendered as zero) and drops the part that was invented (that receipts
// can tell you which sessions were measured).

// Historical receipts cannot identify which writer set UsedDeclared; it is not a measurement.
//
// This falsifier exists because the previous fix (1b9628f0) got the axis wrong. It split served
// claim-events by Receipt.Skills.UsedDeclared and called one side "measured". But UsedDeclared has
// Before explicit application provenance, TWO writers had incompatible meanings:
//
//	cmd/compound/work_cli.go  UsedDeclared: has("--used")   — an operator/agent typed a flag
//	internal/hook/hook.go     UsedDeclared: usedDeclared    — the session-end detector actually ran
//
// Nothing on the Receipt records which one wrote it; a detector-produced receipt and a CLI-close
// receipt are structurally identical (verified 2026-08-11 by field-by-field comparison of
// rec_396a4e3ccee2 against two declared CLI closes — same keys, same producer shape). So "measured"
// was a claim the data could not support: on the real home, 96 of the 97 "declared" sessions were
// CLI closes contributing 420 served events and ZERO claim consumption, while the single session
// where the detector actually ran consumed 5 of the 5 it was served.
//
// The honest surface therefore reports no rate at all. The fixture below is the exact shape that
// broke the old split: one real consumption, and TWO declared sessions that consumed nothing. If a
// future change reintroduces a "measured" denominator, those two will inflate it again and this
// test fails.
func TestDoctorInjectionReportsNoRateBecauseConsumptionProvenanceIsUnrecoverable(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	base := t.TempDir()
	binary := "/opt/compound/compound"
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	// One explicitly declared application: served 2, applied 1.
	seedInjectionSession(t, homePath, "rec_real_consumption", "ctx_real", cliTestNow,
		[]string{"claim://claim_r1", "claim://claim_r2"}, []string{"claim://claim_r1"}, true, "explicit_declaration")
	// Two CLI closes that passed --used (so used_declared=true) but declared capability refs, not
	// claim refs. These consumed nothing and are NOT measurements of claim consumption.
	seedInjectionSession(t, homePath, "rec_cli_a", "ctx_cli_a", cliTestNow.Add(time.Minute),
		[]string{"claim://claim_a1", "claim://claim_a2", "claim://claim_a3"},
		[]string{"capability://skill:compound-land"}, true, "")
	seedInjectionSession(t, homePath, "rec_cli_b", "ctx_cli_b", cliTestNow.Add(2*time.Minute),
		[]string{"claim://claim_b1", "claim://claim_b2"},
		[]string{"capability://skill:compound-verify"}, true, "")
	// One session that declared nothing at all.
	seedInjectionSession(t, homePath, "rec_silent", "ctx_silent", cliTestNow.Add(3*time.Minute),
		[]string{"claim://claim_s1", "claim://claim_s2"}, nil, false, "")

	var jsonOut, errOut bytes.Buffer
	// Doctor exit-code contract: see runDoctorReport's header.
	code := runDoctor([]string{"--home", homePath, "--base", base, "--binary", binary, "--json"}, &jsonOut, &errOut)
	var repAny doctorReport
	_ = json.Unmarshal(jsonOut.Bytes(), &repAny)
	if code != 0 && len(repAny.Problems) == 0 {
		t.Fatalf("json doctor returned exit %d with no problems: %s", code, errOut.String())
	}
	raw := jsonOut.String()

	// The wrong axis must be GONE. A field named "measured" invites exactly the reading the data
	// cannot support, so its absence is part of the contract.
	for _, banned := range []string{"measured_served_events", "unmeasured_served_events", "measured_sessions", "unmeasured_sessions"} {
		if strings.Contains(raw, banned) {
			t.Errorf("json still exposes %q — served events cannot be split by used_declared, which conflates a CLI flag with a detector run", banned)
		}
	}

	var got struct {
		Inj struct {
			ServedEvents               int  `json:"served_events"`
			ConsumedEvents             int  `json:"consumed_events"`
			SessionsServed             int  `json:"sessions_served"`
			SessionsWithConsumption    int  `json:"sessions_with_consumption"`
			SessionsWithoutConsumption int  `json:"sessions_without_consumption"`
			ProvenanceRecoverable      bool `json:"consumption_provenance_recoverable"`
		} `json:"surfacing_injection"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &got); err != nil {
		t.Fatalf("parse json: %v", err)
	}

	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"served_events", got.Inj.ServedEvents, 9},
		{"consumed_events", got.Inj.ConsumedEvents, 1},
		{"sessions_served", got.Inj.SessionsServed, 4},
		// Only the receipt with explicit application provenance counts. The two
		// historical CLI declarations remain ambiguous for claim application.
		{"sessions_with_consumption", got.Inj.SessionsWithConsumption, 1},
		{"sessions_without_consumption", got.Inj.SessionsWithoutConsumption, 3},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if got.Inj.ProvenanceRecoverable {
		t.Errorf("consumption_provenance_recoverable must be false: the Receipt records no writer for used_declared")
	}

	text := runDoctorText(t, homePath, base, binary)
	// No rate may be rendered, in either direction.
	for _, banned := range []string{"measured", "of 9 measured", "consumption rate"} {
		if strings.Contains(text, banned) && !strings.Contains(text, "no consumption rate") {
			t.Errorf("text renders %q, implying a rate the data cannot support:\n%s", banned, text)
		}
	}
	// The reason must be stated where the operator reads it, naming both writers.
	for _, want := range []string{"no consumption rate", "used_declared", "UNKNOWN"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q — an unexplained count is the defect this replaces:\n%s", want, text)
		}
	}
}
