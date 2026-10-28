package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/landlease"
)

// This file closes the CONSUMPTION gap flagged by two independent audits: internal/landlease's own
// package tests (internal/landlease/landlease_test.go) already prove the primitive's semantics
// exhaustively, but nothing proved the CLI verb layer the compound-land ceremony actually invokes
// end-to-end, and nothing stopped the ceremony's SKILL.md from silently dropping the lease steps.
//
//   - T1 (TestLandLeaseCeremonyTwoLaneScenario) drives the real "land-lease" CLI dispatch
//     (cmdLandLease -> runLandLease, cmd/compound/main.go:189 / landlease_cli.go:18) through a
//     two-lane acquire/refuse/wait-timeout/steal/holder-scoped-release narrative — the same call
//     path `compound land-lease ...` uses, just invoked in-process the way this repo's tests already
//     call handlers directly (see runProofCheckStep in main_test.go).
//   - T2 (TestLandLeaseCeremonySkillConsumesLease) is a drift guard: it fails if
//     skills/compound-land/SKILL.md ever stops instructing the ceremony to acquire/wait/release/
//     steal the lease, so the ceremony can't quietly regress to "advisory only" prose.

// runLandLeaseCeremonyCLI invokes the real land-lease CLI dispatch in-process (no subprocess), the
// same entrypoint `compound land-lease ...` resolves to. It is local to this file (distinct name
// from any helper in landlease_cli_test.go) so this file stays self-contained.
func runLandLeaseCeremonyCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var outBuf, errBuf strings.Builder
	code = runLandLease(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

// TestLandLeaseCeremonyTwoLaneScenario exercises the land-lease CLI the way two concurrent landing
// lanes actually collide: lane A acquires, lane B's plain acquire is refused while A is live, lane
// B's --wait --timeout acquire blocks for real and then times out, status still shows A holding,
// lane B's release is refused (holder-scoped), lane B steals the lease with a reason (recording the
// prior holder), lane A's release is now refused because the steal transferred the lease away from
// it, and finally lane B's release succeeds and frees the lease. Holder strings are generic
// (harnessA/harnessB) — no real harness/session names, per repo privacy law.
func TestLandLeaseCeremonyTwoLaneScenario(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const (
		holderA = "harnessA:sessA:work_aaa"
		holderB = "harnessB:sessB:work_bbb"
		ttl     = "5" // short TTL: this scenario always releases/steals explicitly, never waits on expiry
	)

	// Lane A acquires cleanly on a free lease.
	_, errStr, code := runLandLeaseCeremonyCLI(t, "acquire", "--holder", holderA, "--ttl", ttl, "--home", home)
	if code != 0 {
		t.Fatalf("lane A acquire on a free lease must succeed, got exit=%d stderr=%s", code, errStr)
	}

	// Lane B's PLAIN acquire (no --wait) must be REFUSED while A's lease is live: a plain acquire
	// never silently transfers a live lease held by someone else.
	_, errStr, code = runLandLeaseCeremonyCLI(t, "acquire", "--holder", holderB, "--ttl", ttl, "--home", home)
	if code == 0 {
		t.Fatalf("lane B plain acquire over lane A's live lease must be refused (non-zero exit), got exit=0")
	}
	if !strings.Contains(errStr, holderA) {
		t.Fatalf("refusal must name the current holder %q, got stderr=%q", holderA, errStr)
	}

	// Lane B's WAITING acquire (--wait --timeout) must actually block for real wall-clock time and
	// then time out — it must neither return instantly (a stub that never really waited) nor hang
	// past its bound. This is the one leg with no coverage anywhere else in the repo (the existing
	// landlease_cli_test.go legs never exercise --wait), so it is the leg proven by scratch mutation
	// below (see the handoff report for the captured red output).
	waitStart := time.Now()
	_, errStr, code = runLandLeaseCeremonyCLI(t, "acquire", "--holder", holderB, "--ttl", ttl, "--home", home, "--wait", "--timeout", "1")
	waitElapsed := time.Since(waitStart)
	if code == 0 {
		t.Fatalf("lane B waiting acquire must still be refused once its timeout elapses (holder A never released), got exit=0")
	}
	if !strings.Contains(errStr, "timed out") {
		t.Fatalf("waiting acquire that exhausts --timeout must report a timeout, got stderr=%q", errStr)
	}
	if waitElapsed < 900*time.Millisecond {
		t.Fatalf("waiting acquire returned in %s — faster than its own --timeout 1s, so it did not really wait", waitElapsed)
	}
	if waitElapsed > 5*time.Second {
		t.Fatalf("waiting acquire took %s to honor a 1s --timeout — it overshot its bound", waitElapsed)
	}

	// status must still show lane A holding, live, after B's failed attempts.
	out, errStr, code := runLandLeaseCeremonyCLI(t, "status", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("status exit=%d stderr=%s", code, errStr)
	}
	var st landlease.StatusResult
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("decode status: %v (%s)", err, out)
	}
	if !st.Held || st.Holder != holderA {
		t.Fatalf("status must show lane A still holding after B's refused/timed-out attempts: %+v", st)
	}

	// Lane B's release must be REFUSED: release is holder-scoped, and B never held the lease.
	_, errStr, code = runLandLeaseCeremonyCLI(t, "release", "--holder", holderB, "--home", home)
	if code == 0 {
		t.Fatalf("lane B release of a lease it never held must be refused, got exit=0")
	}

	// Lane B steals the lease with a reason: this is the ONLY path that transfers a live lease.
	// It must succeed and the persisted record must show prior_holder + reason.
	stealReason := "lane A wedged past its TTL window"
	out, errStr, code = runLandLeaseCeremonyCLI(t, "steal", "--holder", holderB, "--reason", stealReason, "--home", home, "--json")
	if code != 0 {
		t.Fatalf("lane B steal with a reason must succeed, got exit=%d stderr=%s", code, errStr)
	}
	var stolen landlease.Lease
	if err := json.Unmarshal([]byte(out), &stolen); err != nil {
		t.Fatalf("decode steal result: %v (%s)", err, out)
	}
	if stolen.Holder != holderB || stolen.PriorHolder != holderA || stolen.StealReason != stealReason {
		t.Fatalf("steal record wrong: holder=%q prior=%q reason=%q, want holder=%q prior=%q reason=%q",
			stolen.Holder, stolen.PriorHolder, stolen.StealReason, holderB, holderA, stealReason)
	}
	if stolen.StolenAt == "" {
		t.Fatalf("steal must record stolen_at")
	}

	// Re-read status to confirm the steal was actually PERSISTED (not just echoed back in the steal
	// command's own JSON) — a fresh status call re-reads the on-disk lease file.
	out, errStr, code = runLandLeaseCeremonyCLI(t, "status", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("post-steal status exit=%d stderr=%s", code, errStr)
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("decode post-steal status: %v (%s)", err, out)
	}
	if !st.Held || st.Holder != holderB || st.PriorHolder != holderA || st.StealReason != stealReason {
		t.Fatalf("persisted lease after steal must show holder=B prior=A reason=%q: %+v", stealReason, st)
	}

	// Lane A's release must NOW be refused: the steal transferred the lease away from A, so A no
	// longer holds it and cannot free it.
	_, errStr, code = runLandLeaseCeremonyCLI(t, "release", "--holder", holderA, "--home", home)
	if code == 0 {
		t.Fatalf("lane A release after the steal transferred the lease away from it must be refused, got exit=0")
	}

	// Lane B's release succeeds: B is the current holder after the steal.
	_, errStr, code = runLandLeaseCeremonyCLI(t, "release", "--holder", holderB, "--home", home)
	if code != 0 {
		t.Fatalf("lane B release as the current holder must succeed, got exit=%d stderr=%s", code, errStr)
	}
	out, errStr, code = runLandLeaseCeremonyCLI(t, "status", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("final status exit=%d stderr=%s", code, errStr)
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("decode final status: %v (%s)", err, out)
	}
	if st.Held {
		t.Fatalf("lease must be free after lane B's release: %+v", st)
	}
}

// ceremonyLeaseMarkers are the literal strings the compound-land ceremony's SKILL.md body must
// contain for the ceremony to actually CONSUME the land-lease (acquire before the guarded merge,
// release after) rather than merely documenting that the primitive exists. Markers mirror
// skills/compound-land/SKILL.md:22 (scope-claim status check), :130 (acquire with --wait --timeout),
// :140 (steal --reason as the stale-holder escape hatch), and :182 (release with the same holder
// string used to acquire).
var ceremonyLeaseMarkers = []string{
	"land-lease status --json",
	"land-lease acquire --holder",
	"--wait --timeout",
	"land-lease release --holder",
	"land-lease steal",
	"--reason",
}

// missingCeremonyLeaseMarkers returns every marker in ceremonyLeaseMarkers absent from content, so a
// failing assertion can name exactly what regressed instead of a bare "SKILL.md changed". It is a
// pure string check with no file I/O, so both the real SKILL.md and a doctored in-memory copy (used
// by the falsifier test below) run through the identical code path.
func missingCeremonyLeaseMarkers(content string) []string {
	var missing []string
	for _, marker := range ceremonyLeaseMarkers {
		if !strings.Contains(content, marker) {
			missing = append(missing, marker)
		}
	}
	return missing
}

// TestLandLeaseCeremonySkillConsumesLease is the drift guard: it reads the REAL
// skills/compound-land/SKILL.md from the repo root and fails if the ceremony ever stops instructing
// its consumer to acquire/wait/release/steal the land-lease. Repo root is resolved the same way
// other cmd/compound tests resolve repo-relative files (e.g. the install.sh path in main_test.go):
// cmd/compound is two directories below the repo root, so "../.." from the test's working directory
// (cmd/compound, where `go test` runs) lands on the repo root.
func TestLandLeaseCeremonySkillConsumesLease(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	skillPath := filepath.Join(repoRoot, "skills", "compound-land", "SKILL.md")
	raw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillPath, err)
	}
	if missing := missingCeremonyLeaseMarkers(string(raw)); len(missing) > 0 {
		t.Fatalf("%s must instruct the landing ceremony to CONSUME the land-lease (acquire/wait/release/steal), not just document it exists — missing marker(s): %v", skillPath, missing)
	}
}

// TestMissingCeremonyLeaseMarkersCatchesDrift is the falsifier PROOF for the drift guard above: it
// feeds a doctored COPY of ceremony-shaped content (never the real SKILL.md, never touched on disk)
// with one marker stripped through the exact same missingCeremonyLeaseMarkers helper the real test
// uses, and asserts the checker actually reports it missing. Without this test, a bug that made
// missingCeremonyLeaseMarkers always return an empty slice (e.g. an accidentally-empty
// ceremonyLeaseMarkers, or a helper that never actually checks) would let
// TestLandLeaseCeremonySkillConsumesLease pass green forever regardless of what SKILL.md says.
func TestMissingCeremonyLeaseMarkersCatchesDrift(t *testing.T) {
	t.Parallel()
	doctored := `# doctored copy — not the real SKILL.md
Scope claim: compound land-lease status --json
Then: compound land-lease acquire --holder "<h>" --wait --timeout 120
Release: compound land-lease release --holder "<h>"
`
	// Every marker except "land-lease steal" and "--reason" is present; the steal-related markers
	// were deliberately dropped from this doctored copy.
	missing := missingCeremonyLeaseMarkers(doctored)
	wantMissing := []string{"land-lease steal", "--reason"}
	if len(missing) != len(wantMissing) {
		t.Fatalf("doctored copy missing markers = %v, want %v", missing, wantMissing)
	}
	for i, w := range wantMissing {
		if missing[i] != w {
			t.Fatalf("doctored copy missing markers = %v, want %v", missing, wantMissing)
		}
	}

	// A complete copy (every marker present) must report nothing missing.
	complete := doctored + `Steal: compound land-lease steal --holder "<h>" --reason "<why>"` + "\n"
	if got := missingCeremonyLeaseMarkers(complete); len(got) != 0 {
		t.Fatalf("complete doctored copy should report no missing markers, got %v", got)
	}
}
