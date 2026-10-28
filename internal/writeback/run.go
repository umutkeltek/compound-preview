package writeback

import (
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/work"
)

// GatedWritebackResult is the outcome of one governed owner-writeback run.
type GatedWritebackResult struct {
	WorkID           string
	ActionIntentID   string
	PolicyDecisionID string
	GateID           string
	GateStatus       string
	ReceiptID        string
	TransitionID     string
}

// GatedWriteback parameterizes one governed owner-writeback run. A caller (a
// doctor surface) supplies the per-domain values plus three closures for the
// parts it owns — building the ActionIntent (its own id/header scheme),
// evaluating policy, and building the OwnerWritebackGate. RunGatedWriteback owns
// the spine choreography behind one seam.
type GatedWriteback struct {
	Home            string
	Now             time.Time
	Work            work.CreateInput
	Actor           string
	RunningNote     string
	CheckedNote     string
	Verification    string // post-check VerificationID; "" => the checked gate's CandidateRef
	MoveType        string // success-close move_type
	ActionIntentFor func(workID string) envelope.ActionIntent
	Evaluate        func(envelope.ActionIntent) envelope.PolicyDecision
	GateFor         func(ai envelope.ActionIntent, pd envelope.PolicyDecision) OwnerWritebackGate
	BlockedSummary  func(status, gateID string) string
	AllowedSummary  func(status, gateID string) string
}

// RunGatedWriteback runs the one governed owner-writeback choreography: open the
// work + writeback stores, create/claim/advance a work envelope, propose the
// ActionIntent, evaluate policy and the OwnerWritebackGate, then close the work
// (the caller's MoveType, or audit_only when the gate blocks) while recording the
// gate and the receipt/transition refs. This is the internal seam the per-domain
// doctor CLIs delegate to; the spine no longer lives in the CLI.
func RunGatedWriteback(in GatedWriteback) (GatedWritebackResult, error) {
	var result GatedWritebackResult
	// Lifecycle-generated ids include UnixNano. Give each distinct governed
	// move its own instant even when the caller supplies one batch timestamp;
	// otherwise two same-state advances mint one id with different content.
	at := func(step time.Duration) time.Time { return in.Now.Add(step) }
	workStore, err := work.Open(in.Home)
	if err != nil {
		return result, err
	}
	wbStore, err := Open(in.Home)
	if err != nil {
		return result, err
	}
	w, err := workStore.Create(in.Work, in.Now)
	if err != nil {
		return result, err
	}
	if _, err := workStore.Claim(w.ID, in.Actor, at(1)); err != nil {
		return result, err
	}
	if _, err := workStore.Advance(w.ID, work.AdvanceInput{To: "running", Actor: in.Actor, Note: in.RunningNote}, at(2)); err != nil {
		return result, err
	}
	ai := in.ActionIntentFor(w.ID)
	pd := in.Evaluate(ai)
	gate := in.GateFor(ai, pd)
	checked, err := EvaluateGate(gate, &pd, at(3))
	if err != nil {
		return result, err
	}
	result = GatedWritebackResult{WorkID: w.ID, ActionIntentID: ai.ID, PolicyDecisionID: pd.ID, GateID: checked.ID, GateStatus: checked.Status}
	if !policy.Allowed(pd) || checked.Status != GateAllowed {
		closeRes, closeErr := workStore.Close(w.ID, work.CloseInput{
			Outcome:  "failed",
			MoveType: "audit_only",
			Actor:    in.Actor,
			Summary:  in.BlockedSummary(checked.Status, checked.ID),
			Changed:  true,
		}, at(4))
		if closeErr == nil {
			checked.ReceiptRef = closeRes.Receipt.ID
			result.ReceiptID = closeRes.Receipt.ID
			result.TransitionID = closeRes.Transition.ID
		}
		if err := wbStore.RecordGate(checked); err != nil {
			return result, err
		}
		return result, closeErr
	}
	if _, err := workStore.RecordAction(w.ID, ai, pd, at(4)); err != nil {
		return result, err
	}
	verification := in.Verification
	if verification == "" {
		verification = checked.CandidateRef
	}
	if _, err := workStore.Advance(w.ID, work.AdvanceInput{
		To:             "running",
		Actor:          in.Actor,
		ArtifactID:     checked.ID,
		VerificationID: verification,
		Note:           in.CheckedNote,
	}, at(5)); err != nil {
		return result, err
	}
	closeRes, err := workStore.Close(w.ID, work.CloseInput{
		Outcome:  "completed",
		MoveType: in.MoveType,
		Actor:    in.Actor,
		Summary:  in.AllowedSummary(checked.Status, checked.ID),
		Changed:  true,
	}, at(6))
	if err != nil {
		return result, err
	}
	checked.ReceiptRef = closeRes.Receipt.ID
	if err := wbStore.RecordGate(checked); err != nil {
		return result, err
	}
	result.ReceiptID = closeRes.Receipt.ID
	result.TransitionID = closeRes.Transition.ID
	return result, nil
}
