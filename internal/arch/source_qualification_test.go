package arch

// Source qualification vs full doctor — the separation controls.
//
// Doctor (the canonical full API) runs the source checks AND three runtime audits:
// the live proveable-pair law, the installed build freshness and the installed sidecar
// freshness. DoctorSource runs the source checks only. These controls pin BOTH halves:
//
//	A. source invariance — DoctorSource's findings and OK are identical under an empty
//	   runtime world and under a populated/corrupt one, and the installed binary is
//	   never executed;
//	B. full-doctor negative control — the unchanged Doctor still consumes the fake
//	   installed binary, still reports installed drift, and still FAILS the pair law on a
//	   terminal orphan under --strict;
//	C. source guard teeth — DoctorSource still reports real source failures, so a canned
//	   green source API cannot pass this package.
//
// Every runtime path here is a disposable fixture the test owns. Nothing in this file
// reads the operator's real installation, home or git state beyond the requested source
// repo (git may inspect that repo; it never reads live runtime state).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"compound/internal/eventstore"
)

// runtimeCheckNames are the three checks Doctor runs against RUNTIME state. A source
// report must contain none of them.
var runtimeCheckNames = map[string]bool{
	"proveable-pair-law": true,
	"build-freshness":    true,
	"sidecar-freshness":  true,
}

// runtimeFixture is a disposable runtime world: a COMPOUND_HOME, a GOBIN holding a fake
// installed compound binary that RECORDS its own invocation, and (when populated) drifted
// installed sidecars plus pair-law evidence.
type runtimeFixture struct {
	root   string // disposable root; HOME points here
	world  string // the fixture tree that must stay byte-identical (COMPOUND_HOME + GOBIN)
	home   string // COMPOUND_HOME
	gobin  string // GOBIN — the installed binary lives here
	marker string // written by the fake installed binary when (and only when) it runs
}

func newRuntimeFixture(t *testing.T, populated bool) *runtimeFixture {
	t.Helper()
	root := t.TempDir()
	fx := &runtimeFixture{
		root:   root,
		world:  filepath.Join(root, "world"),
		home:   filepath.Join(root, "world", "home"),
		gobin:  filepath.Join(root, "world", "gobin"),
		marker: filepath.Join(root, "installed-binary-invoked"),
	}
	for _, dir := range []string{fx.home, fx.gobin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !populated {
		return fx
	}
	// A fake installed binary that records every `version` invocation and reports a
	// build SHA that is guaranteed to differ from HEAD (so the full doctor sees
	// installed drift). The recording is gated on the `version` argument, so the
	// warm-up execution below can never forge the marker — only a real doctor
	// invocation can.
	binPath := filepath.Join(fx.gobin, "compound")
	writeFixtureFile(t, binPath,
		"#!/bin/sh\n"+
			"if [ \"$1\" = 'version' ]; then\n"+
			"  printf '%s\\n' \"$*\" >> '"+fx.marker+"'\n"+
			"fi\n"+
			"printf '%s\\n' 'compound 0.0.0 (build "+staleBuildSHA(t)+")'\n", 0o755)
	// Warm the executable. The FIRST exec of a freshly created file pays a per-path
	// admission cost that can exceed the doctor's 700ms exec budget on a loaded
	// machine; paying it here keeps the doctor's own exec fast. The warm-up uses a
	// non-`version` argument, so it records nothing.
	if out, err := exec.Command(binPath, "warmup").CombinedOutput(); err != nil {
		t.Fatalf("warm the synthetic installed binary: %v\n%s", err, out)
	}
	// Drifted installed sidecars: bytes that differ from the repo copies install.sh
	// copies out, so the sidecar freshness check reports drift.
	writeFixtureFile(t, filepath.Join(fx.gobin, "compound-autonomy-heartbeat.sh"),
		"#!/bin/sh\n# drifted installed heartbeat\n", 0o755)
	writeFixtureFile(t, filepath.Join(fx.home, "scripts", "attention-letter.py"),
		"#!/usr/bin/env python3\n# drifted installed attention renderer\n", 0o644)
	// Pair-law evidence: a TERMINAL work whose receipt exists but whose transition does
	// not. The full doctor must fail this under --strict.
	seedPairLawHome(t, fx.home,
		map[string][]byte{"rec_orphan": receiptBody("rec_orphan", "work_orphan", "")},
		map[string]string{"work_orphan": "completed"})
	return fx
}

// withEnv confines HOME, GOBIN and COMPOUND_HOME to the fixture for the duration of fn.
// Nothing outside the fixture is reachable.
func (fx *runtimeFixture) withEnv(t *testing.T, fn func() Report) Report {
	t.Helper()
	t.Setenv("HOME", fx.root)
	t.Setenv("GOBIN", fx.gobin)
	t.Setenv("COMPOUND_HOME", fx.home)
	return fn()
}

// staleBuildSHA returns an installed build SHA that is guaranteed to differ from the
// repo's HEAD, so the full doctor classifies the fake installation as stale.
func staleBuildSHA(t *testing.T) string {
	t.Helper()
	head, err := gitHeadSHA(repoRoot)
	if err != nil || strings.TrimSpace(head) == "" {
		return "deadbeefdeadbeef"
	}
	head = strings.TrimSpace(head)
	if head[0] == '0' {
		return "1" + head[1:]
	}
	return "0" + head[1:]
}

func writeFixtureFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// seedPairLawHome writes post-genesis receipts/transitions into a synthetic COMPOUND_HOME
// event store and the work envelopes that decide whether an orphan counts as a broken
// CLOSE (terminal envelope) or is exempt (no envelope = projection, non-terminal = live).
func seedPairLawHome(t *testing.T, home string, records map[string][]byte, envelopes map[string]string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	if err := store.Ensure(); err != nil {
		t.Fatalf("ensure event store: %v", err)
	}
	for id, body := range records {
		if _, err := store.PutJSON(id, json.RawMessage(body), ""); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	for workID, state := range envelopes {
		dir := filepath.Join(home, "work", "work")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"state":"` + state + `"}`
		if err := os.WriteFile(filepath.Join(dir, workID+".json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write work envelope %s: %v", workID, err)
		}
	}
}

// snapshotTree maps every file under root to the SHA-256 of its bytes, so a control can
// prove the fixture was not mutated.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			out[rel+"/"] = "dir"
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

func snapshotDiff(before, after map[string]string) string {
	var lines []string
	for rel, hash := range before {
		got, ok := after[rel]
		switch {
		case !ok:
			lines = append(lines, rel+": removed")
		case got != hash:
			lines = append(lines, rel+": changed")
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			lines = append(lines, rel+": added")
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "; ")
}

func findingsWithCheck(report Report, check string) []Finding {
	var out []Finding
	for _, f := range report.Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

// requireFinding asserts a finding with the given check, severity and message substring.
func requireFinding(t *testing.T, report Report, check string, severity Severity, substring string) Finding {
	t.Helper()
	matches := findingsWithCheck(report, check)
	for _, f := range matches {
		if f.Severity == severity && strings.Contains(f.Message, substring) {
			return f
		}
	}
	t.Fatalf("no %s finding with severity %s containing %q in %+v", check, severity, substring, matches)
	return Finding{}
}

// --- A. source invariance and the installed-command sentinel ---

// TestDoctorSourceIsInvariantToRuntimeState is the core separation control: the source
// report must not move when the runtime world underneath it changes. The populated world
// carries a fake installed binary that records its own invocation, drifted installed
// sidecars and a broken pair-law close; the empty world carries none of it. The source
// findings and OK must be byte-identical, no installed-command marker may appear, no
// runtime finding may leak into the source report, and the fixture must be unchanged.
func TestDoctorSourceIsInvariantToRuntimeState(t *testing.T) {
	empty := newRuntimeFixture(t, false)
	populated := newRuntimeFixture(t, true)

	emptyBefore := snapshotTree(t, empty.world)
	populatedBefore := snapshotTree(t, populated.world)

	emptyReport := empty.withEnv(t, func() Report { return DoctorSource(repoRoot, true) })
	populatedReport := populated.withEnv(t, func() Report { return DoctorSource(repoRoot, true) })

	if len(emptyReport.Findings) != len(populatedReport.Findings) {
		t.Fatalf("source finding count moved with runtime state: empty=%d populated=%d\nempty=%+v\npopulated=%+v",
			len(emptyReport.Findings), len(populatedReport.Findings), emptyReport.Findings, populatedReport.Findings)
	}
	for i := range emptyReport.Findings {
		if emptyReport.Findings[i] != populatedReport.Findings[i] {
			t.Fatalf("source finding %d moved with runtime state:\nempty=%+v\npopulated=%+v",
				i, emptyReport.Findings[i], populatedReport.Findings[i])
		}
	}
	if emptyReport.OK != populatedReport.OK {
		t.Fatalf("source OK moved with runtime state: empty=%v populated=%v", emptyReport.OK, populatedReport.OK)
	}
	if !emptyReport.OK {
		t.Fatalf("the source report of this repo must be green, got %+v", emptyReport.Findings)
	}

	for _, f := range populatedReport.Findings {
		if runtimeCheckNames[f.Check] {
			t.Fatalf("source report leaked the runtime finding %+v", f)
		}
	}

	if _, err := os.Stat(populated.marker); !os.IsNotExist(err) {
		t.Fatalf("DoctorSource executed the installed binary (marker exists): %v", err)
	}

	if diff := snapshotDiff(emptyBefore, snapshotTree(t, empty.world)); diff != "" {
		t.Fatalf("DoctorSource mutated the empty runtime fixture: %s", diff)
	}
	if diff := snapshotDiff(populatedBefore, snapshotTree(t, populated.world)); diff != "" {
		t.Fatalf("DoctorSource mutated the populated runtime fixture: %s", diff)
	}
}

// TestDoctorSourceFindingsAreASubsequenceOfTheFullDoctor proves no source check was
// dropped or reordered by the split: every source finding (same check, severity and
// message) appears in the full report in the same relative order. A source API that
// filtered the full report, or that lost a check, fails here.
func TestDoctorSourceFindingsAreASubsequenceOfTheFullDoctor(t *testing.T) {
	fx := newRuntimeFixture(t, true)

	source := fx.withEnv(t, func() Report { return DoctorSource(repoRoot, true) })
	full := fx.withEnv(t, func() Report { return Doctor(repoRoot, true) })

	j := 0
	for _, want := range source.Findings {
		found := false
		for ; j < len(full.Findings); j++ {
			if full.Findings[j] == want {
				found = true
				j++
				break
			}
		}
		if !found {
			t.Fatalf("source finding %+v is missing from (or out of order in) the full doctor report: %+v", want, full.Findings)
		}
	}
	if len(source.Findings) == 0 {
		t.Fatal("the source report is empty — the source inventory was not exercised")
	}
}

// --- B. full-doctor negative control ---

// TestFullDoctorStillAuditsRuntimeState is the negative control for the split: the
// unchanged full Doctor must still consume the fake installed binary, still report
// installed drift (as Warn — never a new Fail), and still fail the pair law on a
// terminal orphan under --strict.
func TestFullDoctorStillAuditsRuntimeState(t *testing.T) {
	fx := newRuntimeFixture(t, true)

	report := fx.withEnv(t, func() Report { return Doctor(repoRoot, true) })

	if _, err := os.Stat(fx.marker); err != nil {
		t.Fatalf("the full doctor did not execute the installed binary: %v", err)
	}
	requireFinding(t, report, "build-freshness", Warn, "status=stale")
	requireFinding(t, report, "sidecar-freshness", Warn, "status=stale")
	requireFinding(t, report, "proveable-pair-law", Fail, "work_orphan")
	if report.OK {
		t.Fatalf("the full doctor must not be OK while a terminal work breaks the pair law: %+v", report.Findings)
	}
}

// TestFullDoctorPairLawSeverityAndExemptionsRetainSemantics pins the parts of the live
// audit the split must not disturb: the strict/non-strict severity ladder, the
// projection-only exemption, the live-work exemption and the valid session receipt.
func TestFullDoctorPairLawSeverityAndExemptionsRetainSemantics(t *testing.T) {
	cases := []struct {
		name       string
		records    map[string][]byte
		envelopes  map[string]string
		wantStrict Severity
		wantLoose  Severity
	}{
		{
			name: "terminal close with one side of the pair missing fails under strict",
			records: map[string][]byte{
				"rec_orphan": receiptBody("rec_orphan", "work_orphan", ""),
			},
			envelopes:  map[string]string{"work_orphan": "completed"},
			wantStrict: Fail,
			wantLoose:  Warn,
		},
		{
			name: "projection-only transition with no work envelope is exempt",
			records: map[string][]byte{
				"tr_projection": transitionBody("tr_projection", "work_projection"),
			},
			envelopes:  map[string]string{},
			wantStrict: Info,
			wantLoose:  Info,
		},
		{
			name: "live work with a non-terminal envelope is exempt",
			records: map[string][]byte{
				"rec_live": receiptBody("rec_live", "work_live", ""),
			},
			envelopes:  map[string]string{"work_live": "held"},
			wantStrict: Info,
			wantLoose:  Info,
		},
		{
			name: "a valid session receipt is not a scan error",
			records: map[string][]byte{
				"rec_session": receiptBody("rec_session", "", "sess_alpha"),
				"rec_paired":  receiptBody("rec_paired", "work_paired", ""),
				"tr_paired":   transitionBody("tr_paired", "work_paired"),
			},
			envelopes:  map[string]string{},
			wantStrict: Info,
			wantLoose:  Info,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, strict := range []bool{true, false} {
				fx := newRuntimeFixture(t, false)
				seedPairLawHome(t, fx.home, tc.records, tc.envelopes)
				report := fx.withEnv(t, func() Report { return Doctor(repoRoot, strict) })
				want := tc.wantLoose
				if strict {
					want = tc.wantStrict
				}
				requireFinding(t, report, "proveable-pair-law", want, "")
				for _, f := range findingsWithCheck(report, "proveable-pair-law") {
					if strings.Contains(f.Message, "parse errors") {
						t.Fatalf("strict=%v: pair law reported a scan error: %s", strict, f.Message)
					}
				}
			}
		})
	}
}

// TestFullDoctorFindingOrderIsUnchanged pins the ORDER of the canonical full report. The
// split moved no check, so the three runtime audits must still sit exactly where they did:
// the pair law directly after the candidate map, and the two freshness audits directly
// before the mint ratchet, with the cmd/compound budget right after it and the
// convergence ratchet last.
//
// The comparison is over the COLLAPSED check sequence: a check may emit more than one
// finding (sidecar-freshness reports one per sidecar script, cmd-compound-budget one per
// metric), and what must not change is the order of the checks themselves.
func TestFullDoctorFindingOrderIsUnchanged(t *testing.T) {
	fx := newRuntimeFixture(t, true)
	report := fx.withEnv(t, func() Report { return Doctor(repoRoot, true) })

	var collapsed []string
	for _, f := range report.Findings {
		if len(collapsed) == 0 || collapsed[len(collapsed)-1] != f.Check {
			collapsed = append(collapsed, f.Check)
		}
	}
	index := map[string]int{}
	for i, check := range collapsed {
		if _, seen := index[check]; !seen {
			index[check] = i
		}
	}
	for _, check := range []string{"candidate-map", "proveable-pair-law", "build-freshness", "sidecar-freshness", "mint-ratchet", "cmd-compound-budget", "convergence"} {
		if _, ok := index[check]; !ok {
			t.Fatalf("the full doctor report carries no %q finding: %+v", check, collapsed)
		}
	}
	if collapsed[0] != "candidate-map" {
		t.Fatalf("candidate-map is no longer the first check: %v", collapsed)
	}
	if collapsed[1] != "proveable-pair-law" {
		t.Fatalf("proveable-pair-law is no longer the second check: %v", collapsed)
	}
	for _, pair := range [][2]string{
		{"build-freshness", "sidecar-freshness"},
		{"sidecar-freshness", "mint-ratchet"},
		{"mint-ratchet", "cmd-compound-budget"},
		{"cmd-compound-budget", "convergence"},
	} {
		if index[pair[0]]+1 != index[pair[1]] {
			t.Fatalf("%s and %s are no longer adjacent (%d, %d): %v", pair[0], pair[1], index[pair[0]], index[pair[1]], collapsed)
		}
	}
	if collapsed[len(collapsed)-1] != "convergence" {
		t.Fatalf("the convergence ratchet is no longer the last check: %v", collapsed)
	}
}

// --- C. source guard teeth ---

// TestDoctorSourceKeepsSourceGuardTeeth proves the source API is not a canned green
// report: a planted doc leak must surface as a Fail through DoctorSource, and the source
// report of this repo must actually exercise the source inventory.
func TestDoctorSourceKeepsSourceGuardTeeth(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	realEmail := "synthetic" + "person@gm" + "ail.com"
	mustWriteArchFixture(t, repo, "README.md", "contact: "+realEmail+"\n")
	runGit("add", "--", "README.md")

	report := DoctorSource(repo, true)
	if !hasDocLeakFailContaining(docLeakFindingsOf(report), "README.md:", realEmail) {
		t.Fatalf("DoctorSource did not report the planted doc leak: %+v", report.Findings)
	}
	if report.OK {
		t.Fatalf("DoctorSource reported OK on a repo with a planted doc leak: %+v", report.Findings)
	}

	// The source inventory must actually run: a canned green report would carry none of
	// these source checks.
	live := DoctorSource(repoRoot, false)
	for _, check := range []string{"candidate-map", "classification", "doc-leak", "mint-ratchet", "cmd-compound-budget", "convergence"} {
		if len(findingsWithCheck(live, check)) == 0 {
			t.Fatalf("the source report carries no %q finding — the source inventory was not exercised", check)
		}
	}
}

func docLeakFindingsOf(report Report) []Finding {
	return findingsWithCheck(report, "doc-leak")
}
