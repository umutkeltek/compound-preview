package act

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/finding"
	"compound/internal/home"
	"compound/internal/policy"
)

// ArtifactStore is the content-addressed blob store the outbound wrapper writes
// pre/post images to, under COMPOUND_HOME/artifacts/sha256 (the home's declared
// content-addressed dir — never the repo). Same bytes -> same ref, so capture is
// idempotent and a revert can restore the exact pre-image bytes by ref. It also carries
// the home root so Execute can RECORD each governed write's receipt/transition to the
// canonical event store, keeping the returned ids resolvable.
type ArtifactStore = home.ArtifactStore

// NewArtifactStore roots the store at COMPOUND_HOME/artifacts/sha256, creating it.
func NewArtifactStore(homePath string) (*ArtifactStore, error) {
	return home.NewArtifactStore(homePath)
}

// recordEnvelope persists a validated receipt/transition through the canonical event-store seam.
// A successful return includes a post-write Store.Read proof, so a returned
// receipt_id/transition_id is resolvable through the canonical engine rather
// than inferred from a now-obsolete loose-file path.
func recordEnvelope(home, id string, v any) error {
	if strings.TrimSpace(home) == "" {
		return fmt.Errorf("cannot record %q without a home", id)
	}
	store, err := eventstore.Open(home)
	if err != nil {
		return err
	}
	return recordEnvelopeWithStore(store, id, v)
}

type envelopePersistenceStore interface {
	PutJSON(id string, value any, indent string) (eventstore.PutResult, error)
	Read(raw string) (eventstore.Record, error)
}

func recordEnvelopeWithStore(store envelopePersistenceStore, id string, value any) error {
	if _, err := store.PutJSON(id, value, " "); err != nil {
		return err
	}
	record, err := store.Read(id)
	if err != nil {
		return fmt.Errorf("prove event %s persistence through Store.Read: %w", id, err)
	}
	if record.ID != id {
		return fmt.Errorf("prove event %s persistence through Store.Read: resolved id %q", id, record.ID)
	}
	return nil
}

// recordEnvelopeFn indirects recordEnvelope so a test can force the post-write persistence failure
// (the F2 rollback / F3 recovery-finding paths) that the real filesystem will not produce once
// preflight has proven the event store writable. It is unexported and swapped ONLY in tests; the
// production path always calls the real recordEnvelope.
var recordEnvelopeFn = recordEnvelope

// preflightEventStore proves the home's canonical event store is writable BEFORE any target
// mutation by round-tripping a uniquely-named probe through the physical owner. It
// converts the common post-write persistence failures (a missing or unwritable events dir) into a
// pre-write abort, so a governed write cannot leave a mutated-but-unreceipted target. A leftover
// probe cleanup error is non-fatal: writability is already proven, and the probe name never ends in
// ".json" so the event readers ignore it.
func preflightEventStore(homePath string) error {
	if strings.TrimSpace(homePath) == "" {
		return fmt.Errorf("act: cannot preflight event store without a home")
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return fmt.Errorf("act: event store not writable: %w", err)
	}
	if err := store.Preflight(); err != nil {
		return fmt.Errorf("act: event store not writable: %w", err)
	}
	return nil
}

// afterWriteFailure handles any failure that occurs AFTER a successful adapter.write: it attempts a
// best-effort restore through the adapter's distinct pre-image path, and if that ALSO fails records a durable
// act-recovery finding so an unreceipted, un-rolled-back mutation is never silent.
//
// The rollback is a DIRECT restoreAdapterPreImage call inside THIS admitted action's failure
// path. The original PolicyDecision that authorized the forward write already covers restoring that
// write's own pre-image, so this is NOT a new governed act (it is deliberately not re-gated): it is
// the obligation-bound cleanup of the mutation we just performed under that decision.
func afterWriteFailure(store *ArtifactStore, adapter Adapter, ai envelope.ActionIntent, preRef, postRef string, pre []byte, cause error, now time.Time) error {
	if rbErr := restoreAdapterPreImage(adapter, ai.Target, pre); rbErr != nil {
		return recordRecoveryFinding(store.Home(), ai, preRef, postRef, cause, rbErr, now)
	}
	return fmt.Errorf("act: %w — target rolled back to pre-image %s", cause, preRef)
}

// RecoveryRequiredError carries evidence of an unresolved external effect. It
// is never permission to retry the write. Callers reconcile the same action.
type RecoveryRequiredError struct {
	ActionIntentID string
	PreImageRef    string
	PostImageRef   string
	FindingID      string
	Cause          error
	detail         string
}

func (e *RecoveryRequiredError) Error() string               { return e.detail }
func (e *RecoveryRequiredError) Unwrap() error               { return e.Cause }
func (*RecoveryRequiredError) MutationMayHaveOccurred() bool { return true }

// recordRecoveryFinding lands a durable, operator-visible act-recovery marker when a governed write
// could neither be receipted NOR rolled back — the manual-recovery case. The finding is
// evidence-class (never a ledger byte) and is surfaced by compound doctor's finding counts for
// free. If even the finding cannot be persisted, the returned error itself carries the pre-image
// ref and both error chains, so the manual-recovery material is never swallowed.
func recordRecoveryFinding(homePath string, ai envelope.ActionIntent, preRef, postRef string, cause, rbErr error, now time.Time) error {
	body := fmt.Sprintf(
		"governed outbound write left UNRECEIPTED and rollback FAILED — manual recovery required.\n"+
			"action_intent: %s\ntarget_kind: %s\ntarget_id: %s\ntarget_path: %s\n"+
			"pre_image_ref: %s\npost_image_ref: %s\npersistence_error: %v\nrollback_error: %v\n",
		ai.ID, ai.Target.Kind, ai.Target.ID, ai.Target.Path, preRef, postRef, cause, rbErr)
	refs := []string{preRef}
	if postRef != "" && postRef != preRef {
		refs = append(refs, postRef)
	}
	recovery := &RecoveryRequiredError{ActionIntentID: ai.ID, PreImageRef: preRef, PostImageRef: postRef, Cause: cause}
	rec, ferr := finding.Record(homePath, finding.Finding{
		Topic:        "act-recovery/" + strings.TrimSpace(ai.Target.Kind),
		Body:         body,
		ProducedBy:   "internal/act",
		WorkID:       ai.IDs.WorkID,
		EvidenceRefs: refs,
	}, now)
	if ferr != nil {
		recovery.detail = fmt.Sprintf("act: write persistence FAILED (%v), rollback FAILED (%v), AND recovery finding could not be recorded (%v) — external effect unresolved; pre-image ref %s; post-image ref %s; MANUAL RECOVERY REQUIRED", cause, rbErr, ferr, preRef, postRef)
		return recovery
	}
	recovery.FindingID = rec.ID
	recovery.detail = fmt.Sprintf("act: write persistence FAILED (%v) and rollback FAILED (%v) — recorded recovery finding %s (pre-image ref %s; post-image ref %s); MANUAL RECOVERY REQUIRED", cause, rbErr, rec.ID, preRef, postRef)
	return recovery
}

// Result is the receipted outcome of one governed outbound write.
type Result struct {
	Receipt      envelope.Receipt
	Transition   envelope.Transition
	PreImageRef  string
	PostImageRef string
	PreImage     []byte
	PostImage    []byte
}

// Execute performs ONE governed outbound write and returns its receipt + transition.
//
// The gate is BY CONSTRUCTION: Execute refuses to call adapter.write unless pd is a
// valid PolicyDecision that authorizes THIS exact intent (pd.action_intent_id ==
// ai.ID) and allows it. A missing, mismatched, or non-allowing decision returns an
// error before the adapter is ever touched, so the target stays byte-identical — a
// mutation without a matching allowing decision cannot happen. Because write is
// unexported (package act only), this gate is not merely the intended path but the
// ONLY reachable one: no external package can call an adapter's write to bypass it.
//
// On allow it (iii) captures the pre-image via ReadCurrent into a content-addressed
// artifact, (iv) writes, (v) reads the post-image back into an artifact, (vi) emits
// the outward_delta Receipt (proof_refs=[pre,post], state_delta.changed=true), and
// (vii) records the Transition (before_refs=[pre], after_refs=[post]). The policy's
// obligations for the granted path — emit a Receipt, record a rollback_plan, capture
// the pre-image — are therefore satisfied by construction, not by prompt obedience.
func Execute(store *ArtifactStore, adapter Adapter, ai envelope.ActionIntent, pd envelope.PolicyDecision, newValue []byte, now time.Time) (Result, error) {
	if strings.HasPrefix(strings.TrimSpace(ai.Action), "revert:") {
		return Result{}, fmt.Errorf("act: revert intents require ExecuteRevert")
	}
	return execute(store, adapter, ai, pd, newValue, false, now)
}

// ExecuteRevert performs the governed restore of an adapter pre-image. Keeping
// restore control bytes on a distinct entrypoint means a forward file write is
// always opaque data, even when its bytes happen to match an adapter's encoded
// pre-image schema.
func ExecuteRevert(store *ArtifactStore, adapter Adapter, ai envelope.ActionIntent, pd envelope.PolicyDecision, preImage []byte, now time.Time) (Result, error) {
	if !strings.HasPrefix(strings.TrimSpace(ai.Action), "revert:") {
		return Result{}, fmt.Errorf("act: ExecuteRevert requires a RevertIntent")
	}
	return execute(store, adapter, ai, pd, preImage, true, now)
}

func execute(store *ArtifactStore, adapter Adapter, ai envelope.ActionIntent, pd envelope.PolicyDecision, newValue []byte, restore bool, now time.Time) (Result, error) {
	if store == nil {
		return Result{}, fmt.Errorf("act: artifact store required")
	}
	if adapter == nil {
		return Result{}, fmt.Errorf("act: adapter required")
	}
	if err := gate(ai, pd); err != nil {
		return Result{}, err
	}
	if bound, ok := adapter.(interface {
		validateIntent(string, envelope.ActionIntent, envelope.PolicyDecision, []byte) error
	}); ok {
		if err := bound.validateIntent(store.Home(), ai, pd, newValue); err != nil {
			return Result{}, fmt.Errorf("act: adapter intent binding: %w", err)
		}
	}

	// PREFLIGHT before any mutation: prove the canonical event store is writable, so a persistence
	// failure that would otherwise strand a mutated-but-unreceipted target becomes a pre-write abort.
	// On failure the target is byte-identical — the adapter was never touched.
	if err := preflightEventStore(store.Home()); err != nil {
		return Result{}, err
	}

	pre, err := adapter.ReadCurrent(ai.Target)
	if err != nil {
		return Result{}, fmt.Errorf("act: capture pre-image: %w", err)
	}
	preRef, err := store.Put(pre)
	if err != nil {
		return Result{}, fmt.Errorf("act: store pre-image: %w", err)
	}

	var writeErr error
	if restore {
		writeErr = restoreAdapterPreImage(adapter, ai.Target, newValue)
	} else {
		writeErr = adapter.write(ai.Target, newValue)
	}
	if writeErr != nil {
		// A transport can lose its response after the target accepted the write.
		// Only explicitly uncertain errors enter recovery; ordinary pre-write
		// refusals retain their existing zero-rollback semantics.
		var uncertain interface{ MutationMayHaveOccurred() bool }
		if errors.As(writeErr, &uncertain) && uncertain.MutationMayHaveOccurred() {
			// Capture any observation before compensation. For irreversible
			// transports it is the only retained response to this exact attempt.
			post, readErr := adapter.ReadCurrent(ai.Target)
			postRef := ""
			var putErr error
			if readErr == nil {
				postRef, putErr = store.Put(post)
			}
			return Result{}, afterWriteFailure(store, adapter, ai, preRef, postRef, pre, errors.Join(writeErr, readErr, putErr), now)
		}
		return Result{}, fmt.Errorf("act: write: %w", writeErr)
	}

	// From here the target is MUTATED. Every failure below is routed through afterWriteFailure, which
	// rolls the target back to the pre-image (or, if rollback also fails, lands a durable recovery
	// finding) — so no path returns an error with the target left silently mutated and unreceipted.
	post, err := adapter.ReadCurrent(ai.Target)
	if err != nil {
		return Result{}, afterWriteFailure(store, adapter, ai, preRef, "", pre, fmt.Errorf("act: read-back post-image: %w", err), now)
	}
	postRef, err := store.Put(post)
	if err != nil {
		return Result{}, afterWriteFailure(store, adapter, ai, preRef, "", pre, fmt.Errorf("act: store post-image: %w", err), now)
	}

	receipt, transition := receiptAndTransition(ai, preRef, postRef, pre, post, now)
	if err := receipt.Validate(); err != nil {
		return Result{}, afterWriteFailure(store, adapter, ai, preRef, postRef, pre, fmt.Errorf("act: emitted receipt invalid: %w", err), now)
	}
	if err := transition.Validate(); err != nil {
		return Result{}, afterWriteFailure(store, adapter, ai, preRef, postRef, pre, fmt.Errorf("act: emitted transition invalid: %w", err), now)
	}
	// RECORD the receipt and transition to the home's canonical event store, so the returned
	// ids are resolvable envelope files the event readers (executorlens, eventsource, capgraph,
	// distiller) can see — not a dangling in-memory reference. A governed write that cannot be
	// recorded is a failed governed write; preflight makes this near-impossible, but if it still
	// fails the target is rolled back rather than left mutated-but-unreceipted.
	if err := recordEnvelopeFn(store.Home(), receipt.ID, &receipt); err != nil {
		return Result{}, afterWriteFailure(store, adapter, ai, preRef, postRef, pre, fmt.Errorf("act: record receipt %s: %w", receipt.ID, err), now)
	}
	if err := recordEnvelopeFn(store.Home(), transition.ID, &transition); err != nil {
		return Result{}, afterWriteFailure(store, adapter, ai, preRef, postRef, pre, fmt.Errorf("act: record transition %s: %w", transition.ID, err), now)
	}
	return Result{
		Receipt:      receipt,
		Transition:   transition,
		PreImageRef:  preRef,
		PostImageRef: postRef,
		PreImage:     pre,
		PostImage:    post,
	}, nil
}

// Gate reports whether Execute WOULD admit this (ai, pd) pair — the same
// by-construction refusal Execute applies — WITHOUT performing any write or touching
// the adapter. A caller that must materialize setup state (e.g. create the target key a
// repoint-only adapter needs) uses this to confirm the write is admissible BEFORE any
// mutation, so a refused decision leaves the target byte-identical. It returns nil iff
// Execute(store, adapter, ai, pd, ...) would proceed to the write. Execute re-checks the
// same gate internally, so this is an advisory pre-flight, never a substitute for it.
func Gate(ai envelope.ActionIntent, pd envelope.PolicyDecision) error { return gate(ai, pd) }

// gate is the by-construction refusal: an outbound write proceeds only if pd is a
// valid PolicyDecision that authorizes THIS exact intent and allows it. Anything
// else errors before the adapter is called.
func gate(ai envelope.ActionIntent, pd envelope.PolicyDecision) error {
	if err := ai.Validate(); err != nil {
		return fmt.Errorf("act: action intent invalid: %w", err)
	}
	if ai.MutationClass == "read" {
		return fmt.Errorf("act: %q is a read, not an outbound write", ai.ID)
	}
	if strings.TrimSpace(ai.RollbackPlan) == "" {
		return fmt.Errorf("act: action %q requires a rollback_plan (outbound writes must be revertible)", ai.ID)
	}
	if err := pd.Validate(); err != nil {
		return fmt.Errorf("act: no valid PolicyDecision for %q: %w", ai.ID, err)
	}
	if pd.ActionIntentID != ai.ID {
		return fmt.Errorf("act: PolicyDecision authorizes %q, not this intent %q — refusing to write", pd.ActionIntentID, ai.ID)
	}
	if err := policy.Gate(pd); err != nil {
		return fmt.Errorf("act: %w — refusing to write %q", err, ai.ID)
	}
	return nil
}

func receiptAndTransition(ai envelope.ActionIntent, preRef, postRef string, pre, post []byte, now time.Time) (envelope.Receipt, envelope.Transition) {
	proofRefs := []string{preRef, postRef}
	summary := fmt.Sprintf("outbound write %s %s: %q -> %q", ai.Target.Kind, ai.Target.ID, string(pre), string(post))

	receipt := envelope.Receipt{
		Header:   header(envelope.SVReceipt, idFor("rec_", ai.ID, preRef, postRef, ts(now)), ai, now),
		Outcome:  "completed",
		MoveType: "outward_delta",
		StateDelta: envelope.StateDelta{
			Changed:      true,
			Summary:      summary,
			FilesChanged: filesChanged(ai.Target.Path),
			Artifacts:    append([]string{}, proofRefs...),
			Verification: []string{},
		},
		Cost:      envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:    envelope.ReceiptSkills{Used: []string{ai.Capability}, Missed: []string{}, Candidate: []string{}, UsedDeclared: true},
		Memory:    envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:      envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs: append([]string{}, proofRefs...),
	}
	receipt.EvidenceRefs = append([]string{}, proofRefs...)

	transition := envelope.Transition{
		Header:     header(envelope.SVTransition, idFor("tr_", ai.ID, preRef, postRef, ts(now)), ai, now),
		BeforeRefs: []string{preRef},
		ActionRefs: []string{ai.ID},
		AfterRefs:  []string{postRef},
		StateDelta: summary,
		Projections: envelope.Projections{
			Memory: []string{}, Skill: []string{ai.Capability}, Product: []string{}, Cost: []string{},
			Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{},
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
	}
	transition.EvidenceRefs = append([]string{}, proofRefs...)

	if lane := strings.TrimSpace(ai.Producer.LaneID); lane != "" {
		receipt.Producer.LaneID = lane
		transition.Producer.LaneID = lane
	}
	return receipt, transition
}

// RevertIntent builds the governed revert of a completed outbound write: the same
// target, restoring the captured pre-image through ExecuteRevert. The caller loads
// the pre-image bytes from store.Get(res.PreImageRef); policy re-gates the restore
// exactly like the forward write, so a rollback is itself a governed outbound write,
// never a privileged bypass.
func RevertIntent(orig envelope.ActionIntent, res Result, now time.Time) envelope.ActionIntent {
	ai := orig
	ai.ID = idFor("act_", "revert", orig.ID, res.PreImageRef, res.PostImageRef, ts(now))
	ai.CreatedAt = now.UTC().Format(time.RFC3339)
	ai.Action = "revert:" + strings.TrimSpace(orig.Action)
	ai.Preview = "restore pre-image " + res.PreImageRef + " over " + res.PostImageRef
	ai.RollbackPlan = "restore post-image artifact " + res.PostImageRef
	return ai
}

func header(sv, id string, ai envelope.ActionIntent, now time.Time) envelope.Header {
	return envelope.Header{
		SchemaVersion: sv,
		ID:            id,
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
		IDs:           envelope.IDs{WorkID: ai.IDs.WorkID, ContextPackID: ai.IDs.ContextPackID},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       orDefault(ai.Privacy, "private"),
		Risk:          orDefault(ai.Risk, "tier1"),
		EvidenceRefs:  []string{},
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}

func filesChanged(path string) []string {
	if strings.TrimSpace(path) == "" {
		return []string{}
	}
	return []string{path}
}

func idFor(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(h[:])[:12]
}

func ts(now time.Time) string { return now.UTC().Format(time.RFC3339Nano) }

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
