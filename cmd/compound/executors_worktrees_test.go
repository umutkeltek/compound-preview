package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/envelope"
	"compound/internal/work"
)

// Keep every test in this package hermetic: never read the operator's real
// COMPOUND_HOME for keeps/identity. The default (empty, readable) leaves ground
// truth untouched; tests that exercise ledger joins override this per-case.
func init() {
	worktreeInventoryCLILedger = func() worktreeInventoryLedger {
		return worktreeInventoryLedger{KeepsReadable: true}
	}
}

// The retirement CLI's unused fake also serves inventory tests, but inventory
// asks for one root-set snapshot instead of one Observe call per row.
func (cliUnusedWorktreeLiveness) Snapshot(_ context.Context, roots []string) map[string]authority.WorktreeLivenessObservation {
	observations := make(map[string]authority.WorktreeLivenessObservation, len(roots))
	for _, root := range roots {
		observations[root] = authority.WorktreeLivenessObservation{State: authority.WorktreeLivenessUnused, Evidence: "synthetic CLI fixture"}
	}
	return observations
}

// W6: the CLI renders a versioned JSON envelope whose rows all carry
// class/reason/next_action, and text mode surfaces the plain-language
// next-action strings. Runs the real Git seam over a fixture with one
// landed_clean and one stalled_unlanded worktree so both action strings appear.
func TestExecutorsWorktreesCLIRendersJSONAndText(t *testing.T) {
	repo := newExecutorsWorktreesCLIFixture(t)

	previous := worktreeInventoryCLILiveness
	worktreeInventoryCLILiveness = cliUnusedWorktreeLiveness{}
	t.Cleanup(func() { worktreeInventoryCLILiveness = previous })

	// Runtime-relative future now so freshly created fixture files are far older
	// than the activity window (no live-via-activity, no fixed-date time bomb).
	now := time.Now().Add(48 * time.Hour)

	var stdout, stderr bytes.Buffer
	code := runExecutorsWorktrees(context.Background(), []string{"--json", "--repo", repo}, &stdout, &stderr, now)
	if code != 0 {
		t.Fatalf("json code=%d stderr=%s", code, stderr.String())
	}
	var inv authority.WorktreeInventory
	if err := json.Unmarshal(stdout.Bytes(), &inv); err != nil {
		t.Fatalf("decode json: %v\n%s", err, stdout.String())
	}
	if inv.SchemaVersion != authority.WorktreeInventorySchemaVersion {
		t.Fatalf("schema_version = %q", inv.SchemaVersion)
	}
	if inv.Repo == "" || inv.GeneratedAt == "" || inv.AuthorityRef == "" {
		t.Fatalf("envelope header incomplete: %+v", inv)
	}
	if len(inv.Rows) == 0 {
		t.Fatal("no rows rendered")
	}
	seen := map[string]bool{}
	for _, row := range inv.Rows {
		if row.Class == "" || row.Reason == "" || row.NextAction == "" {
			t.Fatalf("row missing class/reason/next_action: %+v", row)
		}
		seen[row.Class] = true
	}
	for _, want := range []string{authority.WorktreeClassMain, authority.WorktreeClassLandedClean, authority.WorktreeClassStalledUnlanded} {
		if !seen[want] {
			t.Fatalf("expected class %q among rows; got classes %v", want, seen)
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = runExecutorsWorktrees(context.Background(), []string{"--repo", repo}, &stdout, &stderr, now)
	if code != 0 {
		t.Fatalf("text code=%d stderr=%s", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"unlanded work — check lane liveness before salvage or keep",
		"retire via compound authority worktree-retire",
		"leave alone — primary checkout",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text mode missing next-action string %q\n%s", want, text)
		}
	}
}

// F1 dirty-main-live: dirty activity in the primary checkout must be visible
// on its main-class row and in the projection's header counts.
func TestExecutorsWorktreesF1DirtyMainIsLiveAndCounted(t *testing.T) {
	repo := newExecutorsWorktreesCLIFixture(t)
	setExecutorsWorktreesLedger(t, worktreeInventoryLedger{KeepsReadable: true})
	now := time.Now()
	dirty := filepath.Join(repo, "main-lane-wip.txt")
	if err := os.WriteFile(dirty, []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dirty, now, now); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runExecutorsWorktrees(context.Background(), []string{"--json", "--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("json code=%d stderr=%s", code, stderr.String())
	}
	var inv authority.WorktreeInventory
	if err := json.Unmarshal(stdout.Bytes(), &inv); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	main := executorsWorktreesCLIMainRow(t, inv)
	if main.DirtyFiles != 1 || !main.ActivityRecent || !main.Live {
		t.Fatalf("dirty main must surface recent live activity: %+v", main)
	}
	if !strings.Contains(main.Reason, "active lane") || !strings.Contains(main.Reason, "primary checkout") {
		t.Fatalf("dirty main reason must name the live primary-checkout lane: %q", main.Reason)
	}
	liveRows := 0
	for _, row := range inv.Rows {
		if row.Live {
			liveRows++
		}
	}
	if inv.Counts.Live != liveRows || inv.Counts.Live == 0 {
		t.Fatalf("counts.live = %d, live rows = %d; dirty main must be included", inv.Counts.Live, liveRows)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runExecutorsWorktrees(context.Background(), []string{"--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("text code=%d stderr=%s", code, stderr.String())
	}
	if text := stdout.String(); strings.Contains(text, "live=0") || !strings.Contains(text, "main_dirty=1") {
		t.Fatalf("text counts must expose dirty main activity:\n%s", text)
	}
}

func TestExecutorsWorktreesCLIRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runExecutorsWorktrees(context.Background(), []string{"--bogus"}, &stdout, &stderr, time.Now())
	if code != 2 || !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func newExecutorsWorktreesCLIFixture(t *testing.T) string {
	t.Helper()
	repo, _, _ := initLandingCLIRepo(t)
	parent := filepath.Dir(repo)

	// landed_clean: a branch at main, clean.
	runLandingCLIGit(t, repo, "branch", "lane-landed", "main")
	landedWT := filepath.Join(parent, "wt-landed")
	runLandingCLIGit(t, repo, "worktree", "add", landedWT, "lane-landed")

	// stalled_unlanded: the candidate branch (one commit ahead of main) plus a
	// dirty file.
	stalledWT := filepath.Join(parent, "wt-stalled")
	runLandingCLIGit(t, repo, "worktree", "add", stalledWT, "candidate")
	if err := os.WriteFile(filepath.Join(stalledWT, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// setExecutorsWorktreesLedger overrides the ledger loader for one test and
// restores the hermetic default afterwards.
func setExecutorsWorktreesLedger(t *testing.T, led worktreeInventoryLedger) {
	t.Helper()
	prevLive := worktreeInventoryCLILiveness
	prevLedger := worktreeInventoryCLILedger
	worktreeInventoryCLILiveness = cliUnusedWorktreeLiveness{}
	worktreeInventoryCLILedger = func() worktreeInventoryLedger { return led }
	t.Cleanup(func() {
		worktreeInventoryCLILiveness = prevLive
		worktreeInventoryCLILedger = prevLedger
	})
}

func executorsWorktreesCLIRow(t *testing.T, inv authority.WorktreeInventory, branch string) authority.WorktreeInventoryRow {
	t.Helper()
	for _, row := range inv.Rows {
		if row.Branch == branch {
			return row
		}
	}
	t.Fatalf("no row for branch %q", branch)
	return authority.WorktreeInventoryRow{}
}

func executorsWorktreesCLIMainRow(t *testing.T, inv authority.WorktreeInventory) authority.WorktreeInventoryRow {
	t.Helper()
	for _, row := range inv.Rows {
		if row.IsMain {
			return row
		}
	}
	t.Fatal("no main row")
	return authority.WorktreeInventoryRow{}
}

// F4 envelope-join: a running envelope bound to the canonical repository root
// joins work identity and its live lease holder onto the main row even without
// a worktree-branch artifact.
func TestExecutorsWorktreesF4RunningEnvelopeJoinsMainRoot(t *testing.T) {
	repo, base, _ := initLandingCLIRepo(t)
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	w, err := store.Create(work.CreateInput{
		Owner: "executors-observability", Actor: "operator:test", SuccessPredicate: "main lane is visible",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: repo, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: base, PublicationMode: "local_only",
		},
	}, now.Add(-3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	const holder = "codex:main-lane"
	if _, err := store.Claim(w.ID, holder, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: holder}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	prevLive := worktreeInventoryCLILiveness
	prevLedger := worktreeInventoryCLILedger
	worktreeInventoryCLILiveness = cliUnusedWorktreeLiveness{}
	worktreeInventoryCLILedger = defaultWorktreeInventoryLedger
	t.Cleanup(func() {
		worktreeInventoryCLILiveness = prevLive
		worktreeInventoryCLILedger = prevLedger
	})

	var stdout, stderr bytes.Buffer
	if code := runExecutorsWorktrees(context.Background(), []string{"--json", "--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("json code=%d stderr=%s", code, stderr.String())
	}
	var inv authority.WorktreeInventory
	if err := json.Unmarshal(stdout.Bytes(), &inv); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	main := executorsWorktreesCLIMainRow(t, inv)
	if main.WorkID != w.ID || main.WorkState != "running" {
		t.Fatalf("main row must join running root-bound envelope: %+v", main)
	}
	if !strings.Contains(stdout.String(), `"work_holder": "`+holder+`"`) {
		t.Fatalf("main row JSON must expose holder %q:\n%s", holder, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runExecutorsWorktrees(context.Background(), []string{"--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("text code=%d stderr=%s", code, stderr.String())
	}
	if text := stdout.String(); !strings.Contains(text, w.ID) || !strings.Contains(text, holder) {
		t.Fatalf("text row must expose work id and holder:\n%s", text)
	}
}

// F5 ambiguity honesty: several running envelopes can share MAIN's canonical
// delivery root. The projection must expose deterministic candidates without
// guessing that the newest envelope is the active editor, in JSON or text.
func TestExecutorsWorktreesF5AmbiguousMainRootDoesNotInferEditor(t *testing.T) {
	repo := newExecutorsWorktreesCLIFixture(t)
	canonicalRepo := landingCLIOutput(t, repo, "rev-parse", "--show-toplevel")
	setExecutorsWorktreesLedger(t, worktreeInventoryLedger{
		KeepsReadable: true,
		Envelopes: []authority.WorktreeEnvelopeArtifact{
			{WorkID: "work_zeta", State: "running", Holder: "codex:zeta", TargetWorktreeRoot: canonicalRepo, CreatedAt: "2026-07-17T11:00:00Z"},
			{WorkID: "work_alpha", State: "running", Holder: "codex:alpha", TargetWorktreeRoot: canonicalRepo, CreatedAt: "2026-07-17T09:00:00Z"},
		},
	})
	now := time.Now().Add(48 * time.Hour)

	var stdout, stderr bytes.Buffer
	if code := runExecutorsWorktrees(context.Background(), []string{"--json", "--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("json code=%d stderr=%s", code, stderr.String())
	}
	var payload struct {
		Rows []struct {
			IsMain         bool   `json:"is_main"`
			WorkID         string `json:"work_id"`
			WorkState      string `json:"work_state"`
			WorkHolder     string `json:"work_holder"`
			Reason         string `json:"reason"`
			WorkCandidates []struct {
				WorkID     string `json:"work_id"`
				WorkHolder string `json:"work_holder"`
			} `json:"work_candidates"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	mainIndex := -1
	for i := range payload.Rows {
		if payload.Rows[i].IsMain {
			mainIndex = i
			break
		}
	}
	if mainIndex < 0 {
		t.Fatal("no main row")
	}
	main := payload.Rows[mainIndex]
	if main.WorkID != "" || main.WorkState != "" || main.WorkHolder != "" {
		t.Fatalf("ambiguous main-root evidence must not expose singular identity: %+v", main)
	}
	if len(main.WorkCandidates) != 2 ||
		main.WorkCandidates[0].WorkID != "work_alpha" || main.WorkCandidates[0].WorkHolder != "codex:alpha" ||
		main.WorkCandidates[1].WorkID != "work_zeta" || main.WorkCandidates[1].WorkHolder != "codex:zeta" {
		t.Fatalf("JSON candidates must be deterministic and retain holders: %+v", main.WorkCandidates)
	}
	if !strings.Contains(main.Reason, "active editor was not inferred") {
		t.Fatalf("reason must state that the active editor was not inferred: %q", main.Reason)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runExecutorsWorktrees(context.Background(), []string{"--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("text code=%d stderr=%s", code, stderr.String())
	}
	if text := stdout.String(); !strings.Contains(text, "WORK") || !strings.Contains(text, "ambiguous (2 candidates)") ||
		!strings.Contains(text, "active editor was not inferred") {
		t.Fatalf("text mode must label main-root ambiguity honestly:\n%s", text)
	}
}

// The CLI renders joined envelope identity: JSON carries work_id/work_state and
// text mode surfaces a WORK column only when a row joined an exact envelope. The
// known dangling refs/heads/(pending-dispatch) artifact is ignored.
func TestExecutorsWorktreesCLIRendersEnvelopeIdentity(t *testing.T) {
	repo := newExecutorsWorktreesCLIFixture(t)
	setExecutorsWorktreesLedger(t, worktreeInventoryLedger{
		KeepsReadable: true,
		Envelopes: []authority.WorktreeEnvelopeArtifact{
			{WorkID: "work_abc123", State: "running", BranchRef: "refs/heads/candidate", CreatedAt: "2026-07-17T10:00:00Z"},
			{WorkID: "work_dangling", State: "running", BranchRef: "refs/heads/(pending-dispatch)", CreatedAt: "2026-07-17T10:00:00Z"},
		},
	})
	now := time.Now().Add(48 * time.Hour)

	var stdout, stderr bytes.Buffer
	if code := runExecutorsWorktrees(context.Background(), []string{"--json", "--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("json code=%d stderr=%s", code, stderr.String())
	}
	var inv authority.WorktreeInventory
	if err := json.Unmarshal(stdout.Bytes(), &inv); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	candidate := executorsWorktreesCLIRow(t, inv, "candidate")
	if candidate.WorkID != "work_abc123" || candidate.WorkState != "running" {
		t.Fatalf("candidate row should join exact envelope identity: %+v", candidate)
	}
	if landed := executorsWorktreesCLIRow(t, inv, "lane-landed"); landed.WorkID != "" {
		t.Fatalf("lane-landed has no artifact and must stay bare: %+v", landed)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runExecutorsWorktrees(context.Background(), []string{"--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("text code=%d stderr=%s", code, stderr.String())
	}
	if text := stdout.String(); !strings.Contains(text, "WORK") || !strings.Contains(text, "work_abc123 (running)") {
		t.Fatalf("text mode should show a WORK column with the joined work id:\n%s", text)
	}
}

// The CLI renders a keep annotation: the kept row keeps its ground-truth class
// but its next_action defers to the operator decision, both in JSON and text.
func TestExecutorsWorktreesCLIRendersKeepAnnotation(t *testing.T) {
	repo := newExecutorsWorktreesCLIFixture(t)
	setExecutorsWorktreesLedger(t, worktreeInventoryLedger{
		KeepsReadable: true,
		Keeps: map[string]authority.WorktreeKeepAnnotation{
			"refs/heads/candidate": {DecisionID: "keep-candidate", Revisit: "candidate lands or is abandoned"},
		},
	})
	now := time.Now().Add(48 * time.Hour)

	var stdout, stderr bytes.Buffer
	if code := runExecutorsWorktrees(context.Background(), []string{"--json", "--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("json code=%d stderr=%s", code, stderr.String())
	}
	var inv authority.WorktreeInventory
	if err := json.Unmarshal(stdout.Bytes(), &inv); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	candidate := executorsWorktreesCLIRow(t, inv, "candidate")
	if !candidate.Kept || candidate.KeepDecisionID != "keep-candidate" {
		t.Fatalf("candidate row should be annotated kept: %+v", candidate)
	}
	if candidate.Class != authority.WorktreeClassStalledUnlanded {
		t.Fatalf("kept annotation must not change class: %+v", candidate)
	}
	if inv.Counts.Kept != 1 {
		t.Fatalf("counts.kept = %d, want 1", inv.Counts.Kept)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runExecutorsWorktrees(context.Background(), []string{"--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("text code=%d stderr=%s", code, stderr.String())
	}
	if text := stdout.String(); !strings.Contains(text, "kept by operator decision keep-candidate") {
		t.Fatalf("text mode should defer next_action to the keep:\n%s", text)
	}
}

// When the ledger reports keeps unreadable, the CLI surfaces keeps_readable:false
// (JSON) and an honest note (text) instead of implying no keeps exist.
func TestExecutorsWorktreesCLIReportsKeepsUnreadable(t *testing.T) {
	repo := newExecutorsWorktreesCLIFixture(t)
	setExecutorsWorktreesLedger(t, worktreeInventoryLedger{KeepsReadable: false})
	now := time.Now().Add(48 * time.Hour)

	var stdout, stderr bytes.Buffer
	if code := runExecutorsWorktrees(context.Background(), []string{"--json", "--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("json code=%d stderr=%s", code, stderr.String())
	}
	var inv authority.WorktreeInventory
	if err := json.Unmarshal(stdout.Bytes(), &inv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if inv.KeepsReadable == nil || *inv.KeepsReadable {
		t.Fatalf("unreadable keeps must surface keeps_readable:false: %v", inv.KeepsReadable)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runExecutorsWorktrees(context.Background(), []string{"--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("text code=%d stderr=%s", code, stderr.String())
	}
	if text := stdout.String(); !strings.Contains(text, "keep records unreadable") {
		t.Fatalf("text mode should note keeps unreadable:\n%s", text)
	}
}

// F-DEBT-CLI (work_984e63b68945): `compound executors worktrees` must surface the
// scratch-worktree-debt count and the exact sweep suggestion in JSON and text output.
// Reported (advisory, non-fatal) — the inventory is read-only. The fixture's
// lane-landed (contained+clean) branch is the debt candidate at 72h; the candidate
// branch (one commit ahead, stalled_unlanded) is the negative.
func TestExecutorsWorktreesCLIScratchWorktreeDebt(t *testing.T) {
	repo := newExecutorsWorktreesCLIFixture(t)
	setExecutorsWorktreesLedger(t, worktreeInventoryLedger{KeepsReadable: true})
	// 72h future: fixture rows are comfortably past the 48h debt threshold and well
	// outside the 60m activity window. Runtime-relative (time.Now()), not a fixed
	// fixture date — no staleness time bomb.
	now := time.Now().Add(72 * time.Hour)

	var stdout, stderr bytes.Buffer
	if code := runExecutorsWorktrees(context.Background(), []string{"--json", "--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("json code=%d stderr=%s", code, stderr.String())
	}
	var inv authority.WorktreeInventory
	if err := json.Unmarshal(stdout.Bytes(), &inv); err != nil {
		t.Fatalf("decode json: %v\n%s", err, stdout.String())
	}
	if inv.Counts.ScratchWorktreeDebt != 1 {
		t.Fatalf("counts.scratch_worktree_debt = %d, want 1: %+v", inv.Counts.ScratchWorktreeDebt, inv.Counts)
	}
	if len(inv.ScratchWorktreeDebtSweep) != 1 {
		t.Fatalf("sweep suggestions = %d, want 1: %+v", len(inv.ScratchWorktreeDebtSweep), inv.ScratchWorktreeDebtSweep)
	}
	if !strings.Contains(inv.ScratchWorktreeDebtSweep[0], "git worktree remove") {
		t.Fatalf("sweep suggestion must name `git worktree remove`: %q", inv.ScratchWorktreeDebtSweep[0])
	}
	landed := executorsWorktreesCLIRow(t, inv, "lane-landed")
	if !landed.DebtEligible {
		t.Fatalf("lane-landed must be debt-eligible at 72h: %+v", landed)
	}
	// Negative: the candidate branch (one commit ahead of main, dirty) is
	// stalled_unlanded and must NOT be debt-eligible.
	candidate := executorsWorktreesCLIRow(t, inv, "candidate")
	if candidate.DebtEligible {
		t.Fatalf("candidate (unmerged+dirty) must NOT be debt-eligible: %+v", candidate)
	}

	// Text mode must surface both the count on the counts line and the sweep block.
	stdout.Reset()
	stderr.Reset()
	if code := runExecutorsWorktrees(context.Background(), []string{"--repo", repo}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("text code=%d stderr=%s", code, stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, "scratch_worktree_debt=1") {
		t.Fatalf("text counts line must surface scratch_worktree_debt=1:\n%s", text)
	}
	if !strings.Contains(text, "git worktree remove") || !strings.Contains(text, "scratch-worktree-debt:") {
		t.Fatalf("text must surface the sweep suggestion block:\n%s", text)
	}
	if !strings.Contains(text, "advisory") {
		t.Fatalf("text must label the sweep advisory/non-fatal:\n%s", text)
	}

	// Threshold falsifier: the same fixture at 24h reports zero debt (lane-landed
	// is still landed_clean but under the 48h threshold — cooling down, not debt).
	nowFresh := time.Now().Add(24 * time.Hour)
	stdout.Reset()
	stderr.Reset()
	if code := runExecutorsWorktrees(context.Background(), []string{"--json", "--repo", repo}, &stdout, &stderr, nowFresh); code != 0 {
		t.Fatalf("fresh json code=%d stderr=%s", code, stderr.String())
	}
	var invFresh authority.WorktreeInventory
	if err := json.Unmarshal(stdout.Bytes(), &invFresh); err != nil {
		t.Fatalf("decode fresh json: %v\n%s", err, stdout.String())
	}
	if invFresh.Counts.ScratchWorktreeDebt != 0 || len(invFresh.ScratchWorktreeDebtSweep) != 0 {
		t.Fatalf("under-48h inventory must report zero debt and no sweep: counts=%+v sweep=%+v", invFresh.Counts, invFresh.ScratchWorktreeDebtSweep)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runExecutorsWorktrees(context.Background(), []string{"--repo", repo}, &stdout, &stderr, nowFresh); code != 0 {
		t.Fatalf("fresh text code=%d stderr=%s", code, stderr.String())
	}
	if freshText := stdout.String(); strings.Contains(freshText, "scratch-worktree-debt:") {
		t.Fatalf("under-48h text must NOT surface the sweep block:\n%s", freshText)
	}
}
