package work

import (
	"strings"
	"testing"
	"time"
)

// TestBoundEnvelopeClosesAuditOnlyWithoutLandedProof is the falsifier for the
// gate's asymmetry.
//
// gitDeliveryProofForClose computed `signal` -- which gitDeliveryRequirementSignal
// correctly returns empty for an audit_only move with no commit artifact -- and
// then discarded it on the `w.GitDelivery != nil` branch, demanding a landed
// proof unconditionally. The audit exemption was therefore reachable only for
// envelopes with NO delivery contract.
//
// That made every bound envelope permanently unclosable as an audit. It bites in
// practice because git_delivery doubles as the execution-workspace pointer: work
// binds it at t0 just to have somewhere to run, long before the close's move type
// exists. ~97 read-only audit envelopes were stranded this way -- a green
// fan-out terminalized nothing -- while the CLI's own refusal text kept teaching
// `--move-type audit_only` as the remedy.
func TestBoundEnvelopeClosesAuditOnlyWithoutLandedProof(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
	repo, base, _ := sharedLandingReadyRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "a read-only audit on a bound envelope closes without landing",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)

	closed, err := store.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "audit_only",
		Actor:     "codex",
		Summary:   "read-only audit; nothing delivered",
		ProofRefs: []string{"evidence:audit-report"},
	}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("audit_only close on a git_delivery-bound envelope must not require landed proof, got: %v", err)
	}
	if closed.Work.State != "completed" {
		t.Fatalf("state = %q, want completed", closed.Work.State)
	}
}

// TestBoundEnvelopeAuditOnlyWithCommitArtifactStillRequiresLandedProof is the
// paired falsifier that must STAY refusing. Without it, the fix above is
// indistinguishable from deleting the gate: any envelope could clear the landing
// requirement by declaring itself an audit.
//
// gitDeliveryRequirementSignal checks latestCommitArtifact BEFORE the audit_only
// short-circuit, so an "audit" that actually produced a commit still yields a
// non-empty signal and must fall through to the landed-proof gate.
func TestBoundEnvelopeAuditOnlyWithCommitArtifactStillRequiresLandedProof(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "an audit that committed is not an audit",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)

	_, err = store.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "audit_only",
		Actor:     "codex",
		Summary:   "claims audit_only but carries a commit",
		ProofRefs: []string{"commit:" + candidate},
	}, now.Add(3*time.Minute))
	if err == nil {
		t.Fatal("audit_only must NOT waive the landed-proof gate when a proof ref declares a commit; the close succeeded")
	}
}

// TestWorkbenchItemNeedsGitDeliveryProofHonorsAuditOnly pins the READ MODEL to
// the same verdict, and in the same order, as the enforcer.
//
// The predicate took no move type at all while its only caller
// (dispatchClosePlanCandidate) had one in scope and never passed it. So a bound
// audit envelope was advertised "await_proof" forever even though the close
// itself would have succeeded: the dispatch loop stalled on a verdict the gate
// did not share. A loop that cannot be told an item is closeable never closes it.
func TestWorkbenchItemNeedsGitDeliveryProofHonorsAuditOnly(t *testing.T) {
	t.Parallel()
	bound := WorkbenchItem{GitDeliveryBound: true}

	if workbenchItemNeedsGitDeliveryProof(bound, "audit_only") {
		t.Fatal("a bound envelope closing audit_only must not be reported as needing delivery proof")
	}
	if !workbenchItemNeedsGitDeliveryProof(bound, "outward_delta") {
		t.Fatal("a bound envelope closing outward_delta must still need delivery proof")
	}

	// Commit-artifact precedence: order must match gitDeliveryRequirementSignal,
	// where latestCommitArtifact is consulted before the audit_only exemption.
	committed := WorkbenchItem{
		GitDeliveryBound: true,
		ArtifactIDs:      []string{"commit:0123456789012345678901234567890123456789"},
	}
	if !workbenchItemNeedsGitDeliveryProof(committed, "audit_only") {
		t.Fatal("a recorded commit artifact must demand proof even under audit_only")
	}

	// An unbound envelope with no commit needs nothing, regardless of move type.
	if workbenchItemNeedsGitDeliveryProof(WorkbenchItem{}, "outward_delta") {
		t.Fatal("an unbound envelope with no commit artifact must not need delivery proof")
	}
}

// TestAuditCloseGateRefusalStaysGreppable guards the CLI seam: work_lease_refusal
// keys its teaching output off these marker substrings, so rewording the gate
// here silently blinds the refusal renderer.
func TestAuditCloseGateRefusalStaysGreppable(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	now := time.Date(2026, 7, 31, 20, 0, 0, 0, time.UTC)
	repo, base, _ := sharedLandingReadyRepo(t)

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "non-audit work still refuses with the documented marker",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, w.ID, now)

	_, err = store.Close(w.ID, CloseInput{
		Outcome:  "completed",
		MoveType: "outward_delta",
		Actor:    "codex",
		Summary:  "no landed proof",
	}, now.Add(3*time.Minute))
	if err == nil {
		t.Fatal("outward_delta without landed proof must refuse")
	}
	if !strings.Contains(err.Error(), "land-check") && !strings.Contains(err.Error(), "landed proof") {
		t.Fatalf("refusal lost its greppable marker, CLI teaching breaks: %v", err)
	}
}
