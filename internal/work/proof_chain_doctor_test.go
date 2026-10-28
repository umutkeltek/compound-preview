package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

// jsonMarshal is the test-only re-encoder used to fabricate a decodable-but-
// invalid RunAttempt file (embedded id differs from filename id).
func jsonMarshal(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func runAttemptStoreForTest(s *Store) *runattempt.Store {
	return runattempt.Open(s.home)
}

func TestProofChainDoctorReportsCleanTerminalWork(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "terminal proof chain"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "proof-chain guard covered",
		ProofRefs: []string{"repair:proof-chain-doctor"},
	}, fixedNow); err != nil {
		t.Fatalf("close: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "ok" || len(report.Findings) != 0 {
		t.Fatalf("report = %+v, want clean ok", report)
	}
	if report.Counts.TerminalChecked != 1 || report.Counts.Complete != 1 {
		t.Fatalf("counts = %+v, want one complete terminal chain", report.Counts)
	}
}

func TestProofChainDoctorFailsTerminalWorkMissingReceiptTransition(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "broken terminal proof chain"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w.State = "completed"
	if err := s.writeJSON(s.workPath(w.ID), &w); err != nil {
		t.Fatalf("write broken work: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail: %+v", report.Status, report)
	}
	if report.Counts.MissingReceipt != 1 || report.Counts.MissingTransition != 1 {
		t.Fatalf("counts = %+v, want missing receipt and transition", report.Counts)
	}
	if !proofChainFindingContains(report, "missing_receipt") || !proofChainFindingContains(report, "missing_transition") {
		t.Fatalf("findings missing expected codes: %+v", report.Findings)
	}
}

func TestProofChainDoctorFailsMismatchedTerminalWorkID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "mismatched terminal proof chain"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	closed, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "proof-chain mismatch covered",
		ProofRefs: []string{"repair:proof-chain-doctor"},
	}, fixedNow)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	closed.Receipt.IDs.WorkID = "work_other"
	if err := s.replaceEvent(closed.Receipt.ID, &closed.Receipt); err != nil {
		t.Fatalf("write corrupted receipt: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" || report.Counts.MismatchedWorkID != 1 {
		t.Fatalf("report = %+v, want work-id mismatch failure", report)
	}
	if !proofChainFindingContains(report, "receipt_work_mismatch") {
		t.Fatalf("findings missing receipt mismatch: %+v", report.Findings)
	}
}

func TestProofChainDoctorFailsTerminalPolicyActionMismatch(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "policy action mismatch"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	closed, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "policy action mismatch covered",
		ProofRefs: []string{"repair:proof-chain-doctor"},
	}, fixedNow)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	var pd envelope.PolicyDecision
	if err := s.readJSON(s.envPath(closed.Work.PolicyDecisionID), &pd); err != nil {
		t.Fatalf("read policy decision: %v", err)
	}
	pd.ActionIntentID = "act_missing_from_work"
	if err := s.replaceEvent(pd.ID, &pd); err != nil {
		t.Fatalf("write corrupted policy decision: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" || report.Counts.MismatchedActionPolicy != 1 {
		t.Fatalf("report = %+v, want action/policy mismatch failure", report)
	}
	if !proofChainFindingContains(report, "policy_action_mismatch") {
		t.Fatalf("findings missing policy action mismatch: %+v", report.Findings)
	}
}

func TestProofChainDoctorFailsMissingTransitionActionRef(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "transition action ref mismatch"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	closed, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "transition action ref mismatch covered",
		ProofRefs: []string{"repair:proof-chain-doctor"},
	}, fixedNow)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	closed.Transition.ActionRefs = nil
	if err := s.replaceEvent(closed.Transition.ID, &closed.Transition); err != nil {
		t.Fatalf("write corrupted transition: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" || report.Counts.MissingTransitionActionRef == 0 {
		t.Fatalf("report = %+v, want transition action-ref failure", report)
	}
	if !proofChainFindingContains(report, "transition_missing_action_ref") {
		t.Fatalf("findings missing transition action-ref mismatch: %+v", report.Findings)
	}
}

func TestProofChainDoctorFindsOrphanRuntimeEnvelopes(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "orphan envelope scan"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	closed, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "orphan envelope scan covered",
		ProofRefs: []string{"repair:proof-chain-doctor"},
	}, fixedNow)
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	var ai envelope.ActionIntent
	if err := s.readJSON(s.envPath(closed.Work.ActionIntentIDs[len(closed.Work.ActionIntentIDs)-1]), &ai); err != nil {
		t.Fatalf("read action intent: %v", err)
	}
	ai.ID = "act_orphan_extra"
	if err := s.writeJSON(s.envPath(ai.ID), &ai); err != nil {
		t.Fatalf("write orphan action intent: %v", err)
	}
	var pd envelope.PolicyDecision
	if err := s.readJSON(s.envPath(closed.Work.PolicyDecisionID), &pd); err != nil {
		t.Fatalf("read policy decision: %v", err)
	}
	pd.ID = "pol_orphan_extra"
	if err := s.writeJSON(s.envPath(pd.ID), &pd); err != nil {
		t.Fatalf("write orphan policy decision: %v", err)
	}
	rec := closed.Receipt
	rec.ID = "rec_orphan_extra"
	if err := s.writeJSON(s.envPath(rec.ID), &rec); err != nil {
		t.Fatalf("write orphan receipt: %v", err)
	}
	tr := closed.Transition
	tr.ID = "tr_orphan_extra"
	if err := s.writeJSON(s.envPath(tr.ID), &tr); err != nil {
		t.Fatalf("write orphan transition: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" ||
		report.Counts.OrphanActionIntent != 1 ||
		report.Counts.OrphanPolicyDecision != 1 ||
		report.Counts.OrphanReceipt != 1 ||
		report.Counts.OrphanTransition != 1 {
		t.Fatalf("orphan counts = %+v", report.Counts)
	}
	for _, code := range []string{"orphan_action_intent", "orphan_policy_decision", "orphan_receipt", "orphan_transition"} {
		if !proofChainFindingContains(report, code) {
			t.Fatalf("findings missing %s: %+v", code, report.Findings)
		}
	}
}

// TestProofChainDoctorFailsReferencedRunAttemptMissing is the smallest
// falsifier for the RunAttempt proof dimension: a terminal work whose envelope
// references a ratt_ id whose file is absent must produce a missing_run_attempt
// finding and bump the run-attempt proof counter, so the doctor stops
// certifying "complete" on a chain that omits the executed attempt.
//
// Reproduces the live defect: a closed terminal work whose on-disk run_attempts
// record was deleted would otherwise still report status:"ok" and complete:1
// because the doctor never loaded RunAttempts.
func TestProofChainDoctorFailsReferencedRunAttemptMissing(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "missing referenced RunAttempt"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "missing referenced RunAttempt covered",
		ProofRefs: []string{"repair:proof-chain-runattempt"},
	}, fixedNow); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Inject a syntactically-valid ratt_ id that has no matching file on
	// disk. A malformed id would test invalid-id classification, not
	// missing-attempt; runattempt.Valid enforces the canonical shape so the
	// doctor classifies "valid id, absent file" as missing_run_attempt.
	final, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	final.RunAttemptIDs = append(final.RunAttemptIDs, "ratt_20260809T000000Z_deadbeef")
	if err := s.writeJSON(s.workPath(w.ID), &final); err != nil {
		t.Fatalf("corrupt work: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail: %+v", report.Status, report)
	}
	if report.Counts.MissingRunAttempt != 1 {
		t.Fatalf("MissingRunAttempt = %d, want exactly 1", report.Counts.MissingRunAttempt)
	}
	if !proofChainFindingContains(report, "missing_run_attempt") {
		t.Fatalf("findings missing missing_run_attempt: %+v", report.Findings)
	}
}

// TestProofChainDoctorFailsRunAttemptWorkIDMismatch: a terminal work whose
// envelope references a RunAttempt bound to a different work_id must surface
// run_attempt_work_mismatch. The audit chain breaks if a run attempt is
// forward-linked from a work it was never authorized against.
func TestProofChainDoctorFailsRunAttemptWorkIDMismatch(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "RunAttempt bound to other work"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "work-id mismatch covered",
		ProofRefs: []string{"repair:proof-chain-runattempt"},
	}, fixedNow); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Mint a RunAttempt for an unrelated work_id, then attach it to this
	// work's envelope. The doctor must surface the cross-work forward-link.
	// Terminalize the foreign attempt so orthogonal counters (nonterminal,
	// action-mismatch) stay silent -- the test isolates one defect.
	raStore := runAttemptStoreForTest(s)
	foreign, err := raStore.Mint("work_foreign", "codex", "os-harness-entrypoints", fixedNow)
	if err != nil {
		t.Fatalf("mint foreign: %v", err)
	}
	if _, err := raStore.Complete(foreign.ID, runattempt.StatusSucceeded, fixedNow); err != nil {
		t.Fatalf("complete foreign: %v", err)
	}
	final, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	final.RunAttemptIDs = append(final.RunAttemptIDs, foreign.ID)
	if err := s.writeJSON(s.workPath(w.ID), &final); err != nil {
		t.Fatalf("corrupt work: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail: %+v", report.Status, report)
	}
	if report.Counts.RunAttemptWorkMismatch != 1 {
		t.Fatalf("RunAttemptWorkMismatch = %d, want exactly 1", report.Counts.RunAttemptWorkMismatch)
	}
	if report.Counts.MismatchedWorkID != 1 {
		t.Fatalf("MismatchedWorkID = %d, want exactly 1 (aggregate bump on work-id mismatch)", report.Counts.MismatchedWorkID)
	}
	if report.Counts.RunAttemptActionMismatch != 0 || report.Counts.RunAttemptNonterminal != 0 || report.Counts.InvalidRunAttempt != 0 || report.Counts.MissingRunAttempt != 0 || report.Counts.UnreadableRunAttempt != 0 {
		t.Fatalf("orthogonal counters must stay silent for an isolated work-id mismatch: %+v", report.Counts)
	}
	if !proofChainFindingContains(report, "run_attempt_work_mismatch") {
		t.Fatalf("findings missing run_attempt_work_mismatch: %+v", report.Findings)
	}
}

// TestProofChainDoctorFailsRunAttemptActionMismatch: a terminal work whose
// envelope references a RunAttempt bound to an ActionIntent id absent from
// this work's action_intent_ids must surface run_attempt_action_mismatch.
// The attempt-to-intent audit chain is broken when an act_ is forward-linked
// from a work it never authorized.
func TestProofChainDoctorFailsRunAttemptActionMismatch(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "RunAttempt bound to other action"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "action mismatch covered",
		ProofRefs: []string{"repair:proof-chain-runattempt"},
	}, fixedNow); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Mint a RunAttempt bound to this work_id BUT with an action_intent_id
	// that is NOT in the work's action_intent_ids. Doctor must surface the
	// mismatch. Terminalize so orthogonal counters (nonterminal, work-id
	// mismatch, invalid) stay silent -- the test isolates one defect.
	raStore := runAttemptStoreForTest(s)
	stray, err := raStore.MintWithActionIntent(w.ID, "codex", "os-harness-entrypoints", "act_stray_not_in_envelope", fixedNow)
	if err != nil {
		t.Fatalf("mint stray: %v", err)
	}
	if _, err := raStore.Complete(stray.ID, runattempt.StatusSucceeded, fixedNow); err != nil {
		t.Fatalf("complete stray: %v", err)
	}
	final, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	final.RunAttemptIDs = append(final.RunAttemptIDs, stray.ID)
	if err := s.writeJSON(s.workPath(w.ID), &final); err != nil {
		t.Fatalf("corrupt work: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail: %+v", report.Status, report)
	}
	if report.Counts.RunAttemptActionMismatch != 1 {
		t.Fatalf("RunAttemptActionMismatch = %d, want exactly 1", report.Counts.RunAttemptActionMismatch)
	}
	if report.Counts.RunAttemptWorkMismatch != 0 || report.Counts.RunAttemptNonterminal != 0 || report.Counts.InvalidRunAttempt != 0 || report.Counts.MissingRunAttempt != 0 || report.Counts.UnreadableRunAttempt != 0 {
		t.Fatalf("orthogonal counters must stay silent for an isolated action-intent mismatch: %+v", report.Counts)
	}
	if !proofChainFindingContains(report, "run_attempt_action_mismatch") {
		t.Fatalf("findings missing run_attempt_action_mismatch: %+v", report.Findings)
	}
}

// TestProofChainDoctorFailsRunAttemptNonterminal: a terminal work that
// forward-links a still-pending/running RunAttempt must fail closed. A
// terminal envelope whose executed attempt never reached a terminal status
// is a contradiction -- the work says done, the attempt says still running.
func TestProofChainDoctorFailsRunAttemptNonterminal(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "RunAttempt still nonterminal at terminal work"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "nonterminal referenced covered",
		ProofRefs: []string{"repair:proof-chain-runattempt"},
	}, fixedNow); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Mint a fresh pending RunAttempt bound to this work_id; never terminalize
	// it. The doctor must flag it.
	raStore := runAttemptStoreForTest(s)
	pending, err := raStore.Mint(w.ID, "codex", "os-harness-entrypoints", fixedNow)
	if err != nil {
		t.Fatalf("mint pending: %v", err)
	}
	final, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	final.RunAttemptIDs = append(final.RunAttemptIDs, pending.ID)
	if err := s.writeJSON(s.workPath(w.ID), &final); err != nil {
		t.Fatalf("corrupt work: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail: %+v", report.Status, report)
	}
	if report.Counts.RunAttemptNonterminal != 1 {
		t.Fatalf("RunAttemptNonterminal = %d, want exactly 1", report.Counts.RunAttemptNonterminal)
	}
	if report.Counts.RunAttemptWorkMismatch != 0 || report.Counts.RunAttemptActionMismatch != 0 || report.Counts.InvalidRunAttempt != 0 || report.Counts.MissingRunAttempt != 0 || report.Counts.UnreadableRunAttempt != 0 {
		t.Fatalf("orthogonal counters must stay silent for an isolated nonterminal: %+v", report.Counts)
	}
	if !proofChainFindingContains(report, "run_attempt_nonterminal") {
		t.Fatalf("findings missing run_attempt_nonterminal: %+v", report.Findings)
	}
}

// TestProofChainDoctorFailsRunAttemptUnreadable: a terminal work whose
// referenced RunAttempt file exists but is undecodable must surface
// run_attempt_unreadable. The audit chain is broken when the file is
// present but its bytes do not parse.
func TestProofChainDoctorFailsRunAttemptUnreadable(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "RunAttempt file unreadable"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "unreadable RunAttempt covered",
		ProofRefs: []string{"repair:proof-chain-runattempt"},
	}, fixedNow); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Mint a RunAttempt bound to this work_id, then overwrite its file with
	// non-JSON bytes so Get returns a decode error (not ErrNotFound).
	raStore := runAttemptStoreForTest(s)
	corrupt, err := raStore.Mint(w.ID, "codex", "os-harness-entrypoints", fixedNow)
	if err != nil {
		t.Fatalf("mint corrupt: %v", err)
	}
	if _, err := raStore.Complete(corrupt.ID, runattempt.StatusSucceeded, fixedNow); err != nil {
		t.Fatalf("complete corrupt: %v", err)
	}
	rattPath := raStore.Home() + "/run_attempts/" + corrupt.ID + ".json"
	if err := os.WriteFile(rattPath, []byte("not-json-bytes"), 0o644); err != nil {
		t.Fatalf("overwrite ratt: %v", err)
	}
	final, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	final.RunAttemptIDs = append(final.RunAttemptIDs, corrupt.ID)
	if err := s.writeJSON(s.workPath(w.ID), &final); err != nil {
		t.Fatalf("corrupt work: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail: %+v", report.Status, report)
	}
	if report.Counts.UnreadableRunAttempt != 1 {
		t.Fatalf("UnreadableRunAttempt = %d, want exactly 1", report.Counts.UnreadableRunAttempt)
	}
	if report.Counts.RunAttemptWorkMismatch != 0 || report.Counts.RunAttemptActionMismatch != 0 || report.Counts.RunAttemptNonterminal != 0 || report.Counts.InvalidRunAttempt != 0 || report.Counts.MissingRunAttempt != 0 {
		t.Fatalf("orthogonal counters must stay silent for an isolated unreadable: %+v", report.Counts)
	}
	if !proofChainFindingContains(report, "run_attempt_unreadable") {
		t.Fatalf("findings missing run_attempt_unreadable: %+v", report.Findings)
	}
}

// TestProofChainDoctorAcceptsLegacyTerminalWorkWithZeroRunAttemptRefs:
// compatibility -- a terminal work whose envelope has NO RunAttempt refs
// (legacy, pre-S1 evidence) must remain accepted. The new dimension is
// additive; not every terminal work has a RunAttempt.
func TestProofChainDoctorAcceptsLegacyTerminalWorkWithZeroRunAttemptRefs(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "legacy zero refs"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "legacy zero refs covered",
		ProofRefs: []string{"repair:proof-chain-runattempt"},
	}, fixedNow); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Force the envelope's RunAttemptIDs to be empty (legacy shape). Claim
	// and Advance each mint one in this test path, so we rewrite the JSON
	// with the slice cleared.
	final, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// A genuine pre-RunAttempt home has neither forward nor reverse
	// records. Clearing references alone would hide existing history.
	for _, id := range final.RunAttemptIDs {
		if err := os.Remove(filepath.Join(s.home, "run_attempts", id+".json")); err != nil {
			t.Fatal(err)
		}
	}
	final.RunAttemptIDs = nil
	if err := s.writeJSON(s.workPath(w.ID), &final); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q, want ok for legacy zero refs: %+v", report.Status, report)
	}
	if report.Counts.Complete != 1 {
		t.Fatalf("counts = %+v, want Complete=1", report.Counts)
	}
}

// TestProofChainDoctorAcceptsLegacyRunAttemptWithEmptyActionIntentID:
// compatibility -- a RunAttempt whose record has empty action_intent_id
// (legacy/delegate evidence) referenced by a terminal work must remain
// accepted. The action-mismatch check only fires when action_intent_id is
// non-empty and absent from the work envelope.
func TestProofChainDoctorAcceptsLegacyRunAttemptWithEmptyActionIntentID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "legacy empty action"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "legacy empty action covered",
		ProofRefs: []string{"repair:proof-chain-runattempt"},
	}, fixedNow); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Rewrite the already-bound claim attempt into its real v0.1 shape:
	// action_intent_id and all v0.2 terminalization fields are absent. Keeping
	// the WorkEnvelope byte-identical matters: appending a new attempt after
	// close would invalidate the closing event's exact work digest and test a
	// corrupt chain instead of legacy compatibility.
	final, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(final.RunAttemptIDs) != 1 {
		t.Fatalf("closed work RunAttemptIDs = %v, want one claim attempt", final.RunAttemptIDs)
	}
	legacyPath := filepath.Join(s.home, "run_attempts", final.RunAttemptIDs[0]+".json")
	body, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("read modern RunAttempt: %v", err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(body, &legacy); err != nil {
		t.Fatalf("decode modern RunAttempt: %v", err)
	}
	legacy["schema_version"] = runattempt.LegacySchemaVersionV01
	for _, field := range []string{
		"action_intent_id", "terminalization_intent", "terminalization_owner",
		"terminalization_receipt_id", "terminalization_transition_id", "terminalization_reason",
		"terminalization_corrected_from_status", "terminalization_corrected_from_completed_at",
		"terminalization_corrected_from_sweep_reason", "terminalization_corrected_from_sweep_classification",
	} {
		delete(legacy, field)
	}
	body, err = json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("encode legacy RunAttempt: %v", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(legacyPath, body, 0o600); err != nil {
		t.Fatalf("write legacy RunAttempt: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q, want ok for legacy empty action: %+v", report.Status, report)
	}
	if report.Counts.Complete != 1 {
		t.Fatalf("counts = %+v, want Complete=1", report.Counts)
	}
}

// TestProofChainDoctorFailsRunAttemptInvalidRecord: a decodable RunAttempt
// file whose embedded identity is invalid must surface run_attempt_invalid.
// The fixture below flips the embedded id field on disk while keeping the
// schema_version at the canonical v0.1, so this test isolates the embedded-id
// axis of the identity check; schema_version is verified by the production
// code through the same `ra.ID != rattID || ra.SchemaVersion != SchemaVersion`
// branch and would also fire if a separate test flipped the schema.
//
// Identity must be checked BEFORE WorkID linkage so a corrupt record
// (embedded id absent or different from the filename) classifies as
// run_attempt_invalid instead of run_attempt_work_mismatch; that ordering
// is the corrected behavior described in the v0.2 review.
func TestProofChainDoctorFailsRunAttemptInvalidRecord(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "RunAttempt embedded id mismatches filename"}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := s.Close(w.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "invalid RunAttempt covered",
		ProofRefs: []string{"repair:proof-chain-runattempt"},
	}, fixedNow); err != nil {
		t.Fatalf("close: %v", err)
	}

	raStore := runAttemptStoreForTest(s)
	ra, err := raStore.Mint(w.ID, "codex", "os-harness-entrypoints", fixedNow)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := raStore.Complete(ra.ID, runattempt.StatusSucceeded, fixedNow); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Rewrite the file with an embedded id that does NOT match the filename
	// id while keeping schema_version at the canonical v0.1; status is set
	// to succeeded so the orthogonal terminal check stays silent.
	swapped := struct {
		SchemaVersion string `json:"schema_version"`
		ID            string `json:"id"`
		WorkID        string `json:"work_id"`
		Status        string `json:"status"`
	}{
		SchemaVersion: "compound.runattempt.v0.1",
		ID:            "ratt_20260809T000000Z_deadbeef",
		WorkID:        w.ID,
		Status:        "succeeded",
	}
	rattPath := raStore.Home() + "/run_attempts/" + ra.ID + ".json"
	body, err := jsonMarshal(swapped)
	if err != nil {
		t.Fatalf("marshal swap: %v", err)
	}
	if err := os.WriteFile(rattPath, body, 0o644); err != nil {
		t.Fatalf("overwrite ratt: %v", err)
	}
	final, err := s.Get(w.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	final.RunAttemptIDs = append(final.RunAttemptIDs, ra.ID)
	if err := s.writeJSON(s.workPath(w.ID), &final); err != nil {
		t.Fatalf("rewrite work: %v", err)
	}

	report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %q, want fail: %+v", report.Status, report)
	}
	if report.Counts.InvalidRunAttempt != 1 {
		t.Fatalf("InvalidRunAttempt = %d, want exactly 1", report.Counts.InvalidRunAttempt)
	}
	if report.Counts.RunAttemptWorkMismatch != 0 || report.Counts.RunAttemptActionMismatch != 0 || report.Counts.RunAttemptNonterminal != 0 || report.Counts.MissingRunAttempt != 0 || report.Counts.UnreadableRunAttempt != 0 {
		t.Fatalf("orthogonal counters must stay silent for an isolated invalid-record: %+v", report.Counts)
	}
	if !proofChainFindingContains(report, "run_attempt_invalid") {
		t.Fatalf("findings missing run_attempt_invalid: %+v", report.Findings)
	}
}

// TestProofChainDoctorFailsRunAttemptInvalidReference is the small falsifier
// for the silent-corruption trap in the prior RunAttemptIDs population:
// compactPrefix(w.RunAttemptIDs, "ratt_") would drop non-ratt_ entries from
// the slot and the doctor would then treat the work as a legacy zero-ref
// chain -- complete:1, status:"ok". The doctor must surface a populated
// malformed slot as run_attempt_invalid, not silently absorb it.
//
// Reproduces the live defect path: a terminal work whose envelope carries
// non-ratt_ or empty references must not pass the doctor. The table also
// covers whitespace-only and padded canonical refs -- both fail
// runattempt.Valid because the regex requires the exact `ratt_<UTC>_<8 hex>`
// shape (no trimming is applied to the raw entry).
func TestProofChainDoctorFailsRunAttemptInvalidReference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		reference string
	}{
		{name: "non_ratt_legacy_evidence", reference: "run_bad_legacy_evidence"},
		{name: "empty_string", reference: ""},
		{name: "whitespace_only", reference: "   "},
		{name: "padded_canonical_ref", reference: " ratt_20260809T000000Z_deadbeef "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			w, err := s.Create(CreateInput{Owner: "compound-core", SuccessPredicate: "invalid RunAttempt reference " + tc.name}, fixedNow)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if _, err := s.Claim(w.ID, "codex", fixedNow); err != nil {
				t.Fatalf("claim: %v", err)
			}
			if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Note: "run"}, fixedNow); err != nil {
				t.Fatalf("advance: %v", err)
			}
			if _, err := s.Close(w.ID, CloseInput{
				Outcome:   "completed",
				MoveType:  "control_plane_repair",
				Summary:   "invalid reference covered",
				ProofRefs: []string{"repair:proof-chain-runattempt"},
			}, fixedNow); err != nil {
				t.Fatalf("close: %v", err)
			}

			// Inject the malformed reference; the doctor must surface it.
			final, err := s.Get(w.ID)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			final.RunAttemptIDs = append(final.RunAttemptIDs, tc.reference)
			if err := s.writeJSON(s.workPath(w.ID), &final); err != nil {
				t.Fatalf("corrupt work: %v", err)
			}

			report, err := s.ProofChainDoctor(fixedNow, ProofChainDoctorOptions{})
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			if report.Status != "fail" {
				t.Fatalf("status = %q, want fail: %+v", report.Status, report)
			}
			if report.Counts.InvalidRunAttempt != 1 {
				t.Fatalf("counts = %+v, want InvalidRunAttempt == 1 (the populated bad slot must not be silently absorbed)", report.Counts)
			}
			if !proofChainFindingContains(report, "run_attempt_invalid") {
				t.Fatalf("findings missing run_attempt_invalid: %+v", report.Findings)
			}
		})
	}
}

func proofChainFindingContains(report ProofChainDoctorReport, code string) bool {
	for _, finding := range report.Findings {
		if finding.Code == code || strings.Contains(finding.Message, code) {
			return true
		}
	}
	return false
}

func TestProofChainDoctorFindsTerminalizationReasonMismatch(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "doctor-terminalization-reason-mismatch", fixedNow)
	closed, err := s.Close(workID, CloseInput{
		Outcome:                       "completed",
		MoveType:                      "control_plane_repair",
		Summary:                       "doctor binds complete caller intent",
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations: []RunAttemptTerminalizationRequest{{
			RunAttemptID: runAttemptIDs[0],
			Owner:        runattempt.TerminalizationOwnerPanelApply,
			TargetStatus: runattempt.StatusFailed,
			Reason:       "target failed before close",
		}},
	}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ra, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil || ra.TerminalizationIntent == nil {
		t.Fatalf("get prepared attempt: ra=%+v err=%v", ra, err)
	}
	if _, err := runattempt.Open(s.home).CompletePreparedTerminalization(ra.ID, *ra.TerminalizationIntent, fixedNow.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ra, err = runattempt.Open(s.home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	tampered := *ra.TerminalizationIntent
	tampered.TerminalizationReason = "tampered after close"
	ra.TerminalizationIntent = &tampered
	writeRunAttemptForReconcileTest(t, s, ra)

	report, err := s.ProofChainDoctor(fixedNow.Add(7*time.Minute), ProofChainDoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.RunAttemptTerminalizationMismatch != 1 {
		t.Fatalf("terminalization mismatch count=%d, want 1; work=%s receipt=%s findings=%+v", report.Counts.RunAttemptTerminalizationMismatch, closed.Work.ID, closed.Receipt.ID, report.Findings)
	}
	if !proofChainFindingContains(report, "run_attempt_terminalization_mismatch") {
		t.Fatalf("doctor omitted terminalization mismatch finding: %+v", report.Findings)
	}
}

func TestProofChainDoctorFindsRunAttemptOutcomeMismatch(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, s, "doctor-outcome-mismatch", fixedNow)
	if _, err := s.Close(workID, CloseInput{
		Outcome: "completed", MoveType: "control_plane_repair", Summary: "seed completed work",
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ra, err := runattempt.Open(s.home).Get(runAttemptIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	ra.Status = runattempt.StatusFailed
	writeRunAttemptForReconcileTest(t, s, ra)
	report, err := s.ProofChainDoctor(fixedNow.Add(6*time.Minute), ProofChainDoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.RunAttemptOutcomeMismatch != 1 || !proofChainFindingContains(report, "run_attempt_outcome_mismatch") {
		t.Fatalf("outcome mismatch missing: counts=%+v findings=%+v", report.Counts, report.Findings)
	}
}

func TestProofChainDoctorRejectsMalformedRetryChildBeforeHistoryExemption(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, claimAttemptIDs := closeTerminalizeWork(t, s, "doctor-malformed-retry-child", fixedNow)
	attempts := runattempt.Open(s.home)
	if _, err := attempts.CompleteWithReason(
		claimAttemptIDs[0],
		runattempt.StatusAbandoned,
		"historical claim lane ended before retry execution",
		fixedNow.Add(4*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	failed := attachTerminalCloseHistoryAttempt(
		t, s, workID, "", runattempt.StatusFailed, true, fixedNow.Add(5*time.Minute),
	)
	succeeded := attachTerminalCloseHistoryAttempt(
		t, s, workID, failed.ID, runattempt.StatusSucceeded, true, fixedNow.Add(6*time.Minute),
	)
	if _, err := s.Close(workID, CloseInput{
		Outcome:  "completed",
		MoveType: "control_plane_repair",
		Summary:  "seed valid retry history",
	}, fixedNow.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	succeeded.TerminalizationOwner = runattempt.TerminalizationOwnerPanelApply
	succeeded.TerminalizationReceiptID = "rec_partial_retry_child"
	writeRunAttemptForReconcileTest(t, s, succeeded)

	report, err := s.ProofChainDoctor(fixedNow.Add(8*time.Minute), ProofChainDoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.InvalidRunAttempt == 0 || !proofChainFindingContains(report, "run_attempt_invalid") {
		t.Fatalf("doctor accepted malformed retry child: counts=%+v findings=%+v", report.Counts, report.Findings)
	}
}

func TestProofChainDoctorRejectsEveryExactTerminalizationBindingTamper(t *testing.T) {
	t.Parallel()
	baselineStore := newStore(t)
	workID, runAttemptIDs := closeTerminalizeWork(t, baselineStore, "doctor-intent-tamper", fixedNow)
	if _, err := baselineStore.Close(workID, CloseInput{
		Outcome:                       "completed",
		MoveType:                      "control_plane_repair",
		Summary:                       "doctor exact intent tamper",
		SkipRunAttemptTerminalization: true,
		RunAttemptTerminalizations: []RunAttemptTerminalizationRequest{{
			RunAttemptID: runAttemptIDs[0], Owner: runattempt.TerminalizationOwnerPanelApply,
			TargetStatus: runattempt.StatusFailed, Reason: "original reason",
		}},
	}, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	baselineAttempt, err := runattempt.Open(baselineStore.home).Get(runAttemptIDs[0])
	if err != nil || baselineAttempt.TerminalizationIntent == nil {
		t.Fatalf("get prepared attempt: ra=%+v err=%v", baselineAttempt, err)
	}
	if _, err := runattempt.Open(baselineStore.home).CompletePreparedTerminalization(
		baselineAttempt.ID,
		*baselineAttempt.TerminalizationIntent,
		fixedNow.Add(6*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	baselineAttempt, err = runattempt.Open(baselineStore.home).Get(baselineAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertCleanBaseline := func(t *testing.T) {
		t.Helper()
		report, err := baselineStore.ProofChainDoctor(fixedNow.Add(7*time.Minute), ProofChainDoctorOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if report.Counts.RunAttemptTerminalizationMismatch != 0 {
			t.Fatalf("shared baseline is not clean: counts=%+v findings=%+v", report.Counts, report.Findings)
		}
	}
	assertCleanBaseline(t)

	tests := []struct {
		name   string
		mutate func(*runattempt.TerminalizationIntent)
	}{
		{"expected_work_hash", func(in *runattempt.TerminalizationIntent) {
			in.ExpectedWorkSHA256 = "sha256:" + strings.Repeat("b", 64)
		}},
		{"expected_work_state", func(in *runattempt.TerminalizationIntent) { in.ExpectedWorkState = "running" }},
		{"expected_action", func(in *runattempt.TerminalizationIntent) { in.ExpectedActionIntentID = "act_validlooking_stale" }},
		{"expected_policy", func(in *runattempt.TerminalizationIntent) { in.ExpectedPolicyDecisionID = "pol_validlooking_stale" }},
		{"authority_action", func(in *runattempt.TerminalizationIntent) { in.AuthorityActionIntentID = "act_validlooking_foreign" }},
		{"authority_policy", func(in *runattempt.TerminalizationIntent) { in.AuthorityPolicyDecisionID = "pol_validlooking_foreign" }},
		{"close_action", func(in *runattempt.TerminalizationIntent) { in.CloseActionIntentID = "act_validlooking_close" }},
		{"close_policy", func(in *runattempt.TerminalizationIntent) { in.ClosePolicyDecisionID = "pol_validlooking_close" }},
		{"close_input_digest", func(in *runattempt.TerminalizationIntent) {
			in.CloseInputSHA256 = "sha256:" + strings.Repeat("c", 64)
		}},
		{"prepared_at", func(in *runattempt.TerminalizationIntent) { in.PreparedAt = in.PreparedAt.Add(time.Second) }},
		{"reason", func(in *runattempt.TerminalizationIntent) { in.TerminalizationReason = "tampered reason" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), "home")
			if err := os.CopyFS(homePath, os.DirFS(baselineStore.home)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			ra := baselineAttempt
			tampered := *ra.TerminalizationIntent
			tt.mutate(&tampered)
			if err := tampered.Validate(); err != nil {
				t.Fatalf("test tamper must remain structurally valid: %v", err)
			}
			ra.TerminalizationIntent = &tampered
			writeRunAttemptForReconcileTest(t, s, ra)
			report, err := s.ProofChainDoctor(fixedNow.Add(7*time.Minute), ProofChainDoctorOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if report.Counts.RunAttemptTerminalizationMismatch != 1 || !proofChainFindingContains(report, "run_attempt_terminalization_mismatch") {
				t.Fatalf("doctor accepted %s tamper: counts=%+v findings=%+v", tt.name, report.Counts, report.Findings)
			}
		})
	}
	assertCleanBaseline(t)
}
