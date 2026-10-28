package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"compound/internal/home"
	"compound/internal/work"
)

// The workbench-projection gate had the same two problems as memory-doctor, in a worse
// ratio: 39.3s of every battery for an assertion that did not exist.
//
// COST. `workbench --json` is 0.29s on a fresh home and 43.5s on the operator's live one,
// and that gap grows with every envelope the operator opens.
//
// TEETH. The gate checked the exit code and threw the JSON away. cmdWorkbench only exits
// non-zero on hard I/O, and on a home with no envelopes every count is 0 with no error — so
// the gate could not tell a working projection from one that reports nothing. (That was not
// hypothetical: `(*work.Store).list` silently swallowed per-file parse errors, so a home
// full of unreadable envelopes also projected as zeros, green.)
//
// So this gate seeds four envelopes through the real lifecycle doors and asserts the counts
// they must produce. The fixture mirrors internal/work's own workbench proof
// (TestWorkbenchReportsOpenLoopsAndTerminalProof) — same shape, one lifecycle each, so the
// numbers below are derived from the ledger, not chosen to match an observed run.
func runProofCheckWorkbenchGate(repo string, step proofCheckStep) ([]byte, error) {
	return runProofCheckWorkbenchGateContext(context.Background(), repo, step)
}

func runProofCheckWorkbenchGateContext(ctx context.Context, repo string, step proofCheckStep) ([]byte, error) {
	tmpHome, err := os.MkdirTemp("", "compound-proofcheck-workbench-home-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpHome)
	now := time.Now().UTC()
	if err := home.Init(tmpHome, proofCheckWorkbenchActor, now); err != nil {
		return nil, fmt.Errorf("initialize proof-check workbench home: %w", err)
	}
	if err := seedProofCheckWorkbenchHome(tmpHome, now); err != nil {
		return nil, fmt.Errorf("seed proof-check workbench fixture: %w", err)
	}
	return runProofCheckWorkbenchInHomeContext(ctx, repo, tmpHome, step)
}

// proofCheckWorkbenchActor is the fixture's single identity. Advance and Close are
// holder-scoped, so the seed must act as the claimant it declared rather than let the CLI's
// default actor stand in — which is what a missing Actor field silently does.
const proofCheckWorkbenchActor = "proof-check"

// proofCheckWorkbenchFixtureCounts is what the seeded ledger must project: one envelope in
// each open band plus one closed with full proof. Every number is a consequence of
// seedProofCheckWorkbenchHome below.
var proofCheckWorkbenchFixtureCounts = struct {
	Total, Open, Terminal, NeedsClaim, NeedsReview, InProgress int
}{Total: 4, Open: 3, Terminal: 1, NeedsClaim: 1, NeedsReview: 1, InProgress: 1}

func seedProofCheckWorkbenchHome(homePath string, now time.Time) error {
	store, err := work.Open(homePath)
	if err != nil {
		return err
	}
	// needs_claim: created, never claimed.
	if _, err := store.Create(work.CreateInput{Owner: proofCheckWorkbenchActor, SuccessPredicate: "ready work gets claimed"}, now); err != nil {
		return fmt.Errorf("create needs_claim envelope: %w", err)
	}
	// needs_review: created held, so it sits waiting on a human, not on an executor.
	if _, err := store.Create(work.CreateInput{Owner: proofCheckWorkbenchActor, SuccessPredicate: "held work gets reviewed", State: "held"}, now); err != nil {
		return fmt.Errorf("create needs_review envelope: %w", err)
	}
	// in_progress: claimed and advanced to running.
	running, err := store.Create(work.CreateInput{Owner: proofCheckWorkbenchActor, SuccessPredicate: "running work gets verified"}, now)
	if err != nil {
		return fmt.Errorf("create in_progress envelope: %w", err)
	}
	if _, err := store.Claim(running.ID, proofCheckWorkbenchActor, now); err != nil {
		return fmt.Errorf("claim in_progress envelope: %w", err)
	}
	if _, err := store.Advance(running.ID, work.AdvanceInput{To: "running", Actor: proofCheckWorkbenchActor}, now); err != nil {
		return fmt.Errorf("advance in_progress envelope: %w", err)
	}
	// terminal: the full loop, closed with a receipt and a transition, so the projection's
	// missing_receipt / missing_transition counters are exercised at zero for a real reason.
	done, err := store.Create(work.CreateInput{Owner: proofCheckWorkbenchActor, SuccessPredicate: "terminal work has proof"}, now)
	if err != nil {
		return fmt.Errorf("create terminal envelope: %w", err)
	}
	if _, err := store.Claim(done.ID, proofCheckWorkbenchActor, now); err != nil {
		return fmt.Errorf("claim terminal envelope: %w", err)
	}
	if _, err := store.Advance(done.ID, work.AdvanceInput{To: "running", Actor: proofCheckWorkbenchActor}, now); err != nil {
		return fmt.Errorf("advance terminal envelope to running: %w", err)
	}
	if _, err := store.Advance(done.ID, work.AdvanceInput{To: "verifying", Actor: proofCheckWorkbenchActor, ArtifactID: "artifact_proofcheck_workbench", VerificationID: "ver_proofcheck_workbench"}, now); err != nil {
		return fmt.Errorf("advance terminal envelope to verifying: %w", err)
	}
	if _, err := store.Close(done.ID, work.CloseInput{
		Actor:     proofCheckWorkbenchActor,
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "proof-check workbench fixture",
		Changed:   true,
		ProofRefs: []string{"proof:proof-check-workbench-gate"},
	}, now); err != nil {
		return fmt.Errorf("close terminal envelope: %w", err)
	}
	// Residue: a lease whose work is terminal. Since I1 the close above releases its
	// own lease, so this population is no longer a side effect of closing and has to
	// be planted, or the agents gate reconciles two doors that are both empty.
	if err := store.WriteResidualLeaseForTerminalWork(done.ID, proofCheckWorkbenchActor, now); err != nil {
		return fmt.Errorf("plant terminal-work lease residue: %w", err)
	}
	return nil
}

// runProofCheckWorkbenchInHome is split out for the same reason as its memory-doctor
// sibling: the negative controls need to hand this an intentionally damaged home.
func runProofCheckWorkbenchInHome(repo, homePath string, step proofCheckStep) ([]byte, error) {
	return runProofCheckWorkbenchInHomeContext(context.Background(), repo, homePath, step)
}

func runProofCheckWorkbenchInHomeContext(ctx context.Context, repo, homePath string, step proofCheckStep) ([]byte, error) {
	executable, err := work.ProofRuntimeExecutable(ctx, step.Command[0], proofCheckRuntimeExecutable)
	if err != nil {
		return nil, err
	}
	cmd := proofCheckCommandContext(ctx, executable, step.Command[1:]...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, err
	}
	if err := proofCheckAssertWorkbenchProjection(output); err != nil {
		return output, err
	}
	return output, nil
}

func proofCheckAssertWorkbenchProjection(output []byte) error {
	var report workbenchCLIOutput
	if err := json.Unmarshal(output, &report); err != nil {
		return fmt.Errorf("parse workbench projection: %w", err)
	}
	want := proofCheckWorkbenchFixtureCounts
	got := report.Counts
	if got.Total != want.Total || got.Open != want.Open || got.Terminal != want.Terminal {
		return fmt.Errorf("workbench counts total/open/terminal = %d/%d/%d, want %d/%d/%d: the projection no longer reflects the seeded ledger",
			got.Total, got.Open, got.Terminal, want.Total, want.Open, want.Terminal)
	}
	if got.NeedsClaim != want.NeedsClaim || got.NeedsReview != want.NeedsReview || got.InProgress != want.InProgress {
		return fmt.Errorf("workbench band counts needs_claim/needs_review/in_progress = %d/%d/%d, want %d/%d/%d",
			got.NeedsClaim, got.NeedsReview, got.InProgress, want.NeedsClaim, want.NeedsReview, want.InProgress)
	}
	if got.MissingReceipt != 0 || got.MissingTransition != 0 {
		return fmt.Errorf("closed fixture envelope lacks proof: missing_receipt=%d missing_transition=%d", got.MissingReceipt, got.MissingTransition)
	}
	if len(report.Open) != want.Open {
		return fmt.Errorf("workbench listed %d open items, want %d: counts and items disagree", len(report.Open), want.Open)
	}
	if report.ReviewQueue == nil || report.ReviewQueue.SchemaVersion == "" {
		return fmt.Errorf("workbench projection carries no review queue: the operator-load meter did not run")
	}
	return nil
}
