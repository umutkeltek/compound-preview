package promotedriver

import (
	"compound/internal/candidates"
	"compound/internal/lanebeliefs"
)

// Report is one driver pass's full receipt — the audit trail entry that
// makes autonomous adjudication reversible-by-inspection. It is appended
// verbatim to state/promote-driver/receipts.jsonl.
type Report struct {
	SchemaVersion string `json:"schema_version"`
	PassID        string `json:"pass_id"`
	Home          string `json:"home"`
	DryRun        bool   `json:"dry_run"`
	// Locked is true when another pass held the single-flight lock; the
	// pass did nothing else. Not an error — the doctrine is keep waiting.
	Locked bool `json:"locked,omitempty"`
	// Stage is planned, then exactly one terminal state:
	// committed (all required work succeeded), partial (bounded candidate-local
	// failures are named), failed (a phase or integrity write failed), or
	// recovery_required (mutations may have occurred but the terminal receipt
	// could not be durably appended).
	// An abandoned recovery closes replay while retaining prior operation and finalization evidence.
	Stage string `json:"stage,omitempty"`
	// Error carries the terminal failure/partial/recovery explanation.
	Error       string `json:"error,omitempty"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
	ReceiptPath string `json:"receipt_path,omitempty"`
	// Intent is frozen on the planned receipt before mutation. It binds the
	// candidate window, proposed per-candidate operations, cursors, and pending
	// generation so a planned-only pass is reconstructible.
	Intent            *PassIntent             `json:"intent,omitempty"`
	OperationResults  []PassOperationResult   `json:"operation_results,omitempty"`
	Finalization      *PassFinalizationResult `json:"finalization,omitempty"`
	ReconciledPassIDs []string                `json:"reconciled_pass_ids,omitempty"`

	Promote     PromotePhaseReport     `json:"promote"`
	Dedupe      DedupePhaseReport      `json:"dedupe"`
	Imports     ImportsPhaseReport     `json:"imports"`
	LaneBeliefs LaneBeliefsPhaseReport `json:"lanebeliefs"`
	Sweep       SweepPhaseReport       `json:"sweep"`

	// PendingAfter is the reconciled pending count when the sweep walked the
	// whole inbox this pass — the honest backlog number. Nil when the pass
	// stopped early (cursor-bounded) or ran dry.
	PendingAfter *int `json:"pending_after,omitempty"`
}

type PassIntent struct {
	PlanDigest                string                   `json:"plan_digest,omitempty"`
	WorkID                    string                   `json:"work_id,omitempty"`
	ExecutionAt               string                   `json:"execution_at,omitempty"`
	CandidateCorpusGeneration string                   `json:"candidate_corpus_generation,omitempty"`
	PromoteCursorBefore       candidates.EntryCursor   `json:"promote_cursor_before,omitempty"`
	SweepCursorBefore         candidates.EntryCursor   `json:"sweep_cursor_before,omitempty"`
	PendingGeneration         string                   `json:"pending_generation,omitempty"`
	CandidateIDs              []string                 `json:"candidate_ids"`
	CorruptEntries            []candidates.EntryCursor `json:"corrupt_entries,omitempty"`
	Operations                []PassIntentOperation    `json:"operations"`
	PromotePlan               PromotePhaseReport       `json:"promote_plan"`
	DedupePlan                DedupePhaseReport        `json:"dedupe_plan"`
	SweepPlan                 SweepPhaseReport         `json:"sweep_plan"`
}

type PassIntentOperation struct {
	Key          string                 `json:"key"`
	Ordinal      int                    `json:"ordinal"`
	Phase        string                 `json:"phase"`
	Operation    string                 `json:"operation"`
	Target       CandidatePrecondition  `json:"target"`
	Keeper       *CandidatePrecondition `json:"keeper,omitempty"`
	CurrentClaim *ClaimPrecondition     `json:"current_claim,omitempty"`
	DedupeKey    string                 `json:"dedupe_key,omitempty"`
}

type ClaimPrecondition struct {
	ID     string `json:"id,omitempty"`
	Absent bool   `json:"absent,omitempty"`
}

type CandidatePrecondition struct {
	ID             string                 `json:"id"`
	EntryCursor    candidates.EntryCursor `json:"entry_cursor"`
	State          string                 `json:"state"`
	BodyDigest     string                 `json:"body_digest"`
	PromotionBasis string                 `json:"promotion_basis,omitempty"`
}

type PassOperationResult struct {
	Key                  string `json:"key"`
	Ordinal              int    `json:"ordinal"`
	Phase                string `json:"phase"`
	Operation            string `json:"operation"`
	CandidateID          string `json:"candidate_id"`
	Outcome              string `json:"outcome"`
	Verdict              string `json:"verdict,omitempty"`
	CandidateState       string `json:"candidate_state,omitempty"`
	BeforeDigest         string `json:"before_digest,omitempty"`
	AfterDigest          string `json:"after_digest,omitempty"`
	MismatchCandidateID  string `json:"mismatch_candidate_id,omitempty"`
	PreconditionMismatch string `json:"precondition_mismatch,omitempty"`
	ClaimID              string `json:"claim_id,omitempty"`
	SupersededClaimID    string `json:"superseded_claim_id,omitempty"`
	TransitionID         string `json:"transition_id,omitempty"`
	Note                 string `json:"note,omitempty"`
	Error                string `json:"error,omitempty"`
}

// PassFinalizationResult is the durable result of publishing the frozen
// cursors and, for a full sweep, reconciling the candidate backlog. It is
// journaled before the terminal receipt so recovery validates the terminal
// projection against a prior exact result frame rather than trusting a
// mutable summary.
type PassFinalizationResult struct {
	PromoteCursorAfter candidates.EntryCursor     `json:"promote_cursor_after,omitempty"`
	SweepCursorAfter   candidates.EntryCursor     `json:"sweep_cursor_after,omitempty"`
	Pending            *PendingFinalizationResult `json:"pending,omitempty"`
}

type PendingFinalizationResult struct {
	Pending    int    `json:"pending"`
	Files      int    `json:"files"`
	Corrupt    int    `json:"corrupt,omitempty"`
	Generation string `json:"generation"`
}

// PromotePhaseReport receipts the basis-gated promotion phase. The walk
// resumes from ResumedFrom across passes (NextCursor persists it) so a
// saturated inbox still gives every proposed candidate a promotion chance
// within one full cycle.
type PromotePhaseReport struct {
	Limit       int                      `json:"limit"`
	Walked      int                      `json:"walked"`
	Decoded     int                      `json:"decoded"`
	Corrupt     int                      `json:"corrupt"`
	Scanned     int                      `json:"scanned"`
	Considered  int                      `json:"considered"` // proposed candidates seen this walk
	Promoted    int                      `json:"promoted"`
	ResumedFrom candidates.EntryCursor   `json:"resumed_from,omitempty"`
	NextCursor  candidates.EntryCursor   `json:"next_cursor,omitempty"`
	Deferred    []string                 `json:"deferred,omitempty"` // basis-eligible, over the per-pass limit
	Promotions  []PromotionReceipt       `json:"promotions,omitempty"`
	Errors      []string                 `json:"errors,omitempty"`
	CorruptIDs  []candidates.EntryCursor `json:"corrupt_ids,omitempty"`
}

// PromotionReceipt is one per-id promotion the pass performed (or, in a
// dry run, would have performed). The basis is echoed so the receipt is
// self-evidencing: a promote without a recorded basis would be a defect
// visible in the log itself.
type PromotionReceipt struct {
	CandidateID string `json:"candidate_id"`
	ClaimID     string `json:"claim_id,omitempty"`
	Basis       string `json:"promotion_basis"`
	Action      string `json:"action"` // promoted | would_promote
}

// DedupePhaseReport receipts exact-duplicate collapse. Runs only on a full
// walk so no group splits across passes.
type DedupePhaseReport struct {
	Groups    int            `json:"groups"`
	Collapsed int            `json:"collapsed"`
	Actions   []DedupeAction `json:"actions,omitempty"`
	Skipped   string         `json:"skipped,omitempty"` // reason (partial walk)
	Errors    []string       `json:"errors,omitempty"`
}

// DedupeAction names one duplicate group's keeper and the ids no_promoted
// in its favor. Naming the keeper is what makes the collapse reversible by
// inspection: the operator can see exactly which copy survived and why.
type DedupeAction struct {
	Key        string   `json:"key"`
	Kept       string   `json:"kept"`
	NoPromoted []string `json:"no_promoted"`
	Action     string   `json:"action"` // collapsed | would_collapse
}

// ImportsPhaseReport receipts the deterministic product-registry refresh.
type ImportsPhaseReport struct {
	Sources []ImportSourceReport `json:"sources,omitempty"`
	Skipped string               `json:"skipped,omitempty"`
	Error   string               `json:"error,omitempty"`
}

// ImportSourceReport is one registry file's disposition, hash-proven so a
// re-import can be audited back to the exact content that caused it.
type ImportSourceReport struct {
	Path       string   `json:"path"`
	HashBefore string   `json:"hash_before,omitempty"`
	HashAfter  string   `json:"hash_after,omitempty"`
	Action     string   `json:"action"` // up_to_date | would_import | imported | (error field set)
	ClaimIDs   []string `json:"claim_ids,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// LaneBeliefsPhaseReport embeds the three lane-belief organ reports in
// order: fold (B2), promote (B3), cool (B4). The driver sequences them;
// the gates are theirs.
type LaneBeliefsPhaseReport struct {
	Fold    lanebeliefs.FoldReport    `json:"fold"`
	Promote lanebeliefs.PromoteReport `json:"promote"`
	Cool    lanebeliefs.CoolReport    `json:"cool"`
	Error   string                    `json:"error,omitempty"` // the phase that failed; earlier phases completed
	Skipped string                    `json:"skipped,omitempty"`
}

// SweepPhaseReport receipts the disposition pass. VerdictCounts keys are
// the verbatim verdict strings ("park", "no_promote", ...).
type SweepPhaseReport struct {
	ScanLimit     int            `json:"scan_limit"`
	Walked        int            `json:"walked"`
	Decoded       int            `json:"decoded"`
	Corrupt       int            `json:"corrupt"`
	Scanned       int            `json:"scanned"`
	Adjudicated   int            `json:"adjudicated"`
	Skipped       int            `json:"skipped"`
	Deferred      int            `json:"deferred"`
	Failed        int            `json:"failed"`
	VerdictCounts map[string]int `json:"verdict_counts,omitempty"`
	Actions       []SweepAction  `json:"actions,omitempty"`
	ChangedIDs    []string       `json:"changed_ids,omitempty"`
	// BasisDeferred counts candidates the sweep left proposed because they
	// carry a promotable basis — reserved for the promotion walk, never
	// foreclosed by disposition.
	BasisDeferred int `json:"basis_deferred,omitempty"`
	// WalkedAll is true when the sweep read the inbox to the end this pass;
	// only then may the pending count be reconciled.
	WalkedAll         bool                     `json:"walked_all,omitempty"`
	NextCursor        candidates.EntryCursor   `json:"next_cursor,omitempty"`
	ReconciledPending *int                     `json:"reconciled_pending,omitempty"`
	CorruptIDs        []candidates.EntryCursor `json:"corrupt_ids,omitempty"`
	Errors            []string                 `json:"errors,omitempty"`
}

type SweepAction struct {
	CandidateID string `json:"candidate_id"`
	Verdict     string `json:"verdict"`
}
