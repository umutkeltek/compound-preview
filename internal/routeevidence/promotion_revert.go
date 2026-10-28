package routeevidence

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/finding"
	"compound/internal/reviewartifact"
)

// The governed one-line undo for `route promotion-apply`.
//
// It does NOT re-derive "what the claim used to be" from the review. It loads
// the EXACT pre-image artifact the forward act captured and restores those bytes
// through act.ExecuteRevert under a freshly gated revert intent. Drift on the
// target path or on the post-image revision is a refusal: if the authority moved
// since the apply, this undo would silently discard whatever came after it.

// PromotionRevertPlan is the validated, pre-mutation description of one undo.
type PromotionRevertPlan struct {
	ReviewID        string `json:"review_id"`
	AppliedRecordID string `json:"applied_record_id"`
	AppliedPath     string `json:"applied_path"`
	LaneID          string `json:"lane_id"`
	CapabilityClass string `json:"capability_class"`

	CurrentClaimLevel  string `json:"current_claim_level"`
	RestoredClaimLevel string `json:"restored_claim_level"`

	AuthorityRef      string `json:"authority_ref"`
	AuthorityPath     string `json:"authority_path"`
	AuthorityRevision string `json:"authority_revision"`
	// RestoredRevision is MEASURED from the pre-image artifact body, not taken
	// from the applied record's self-asserted field. It is what the
	// source-authority ledger is stamped with after a successful restore.
	RestoredRevision string `json:"restored_revision"`

	PreImageRef  string `json:"pre_image_ref"`
	PostImageRef string `json:"post_image_ref"`

	// PreImage is the exact captured pre-image blob (the act file pre-image
	// encoding), fetched from the content-addressed store by ref.
	PreImage []byte `json:"-"`
}

// PromotionRevertResult is what the CLI renders after a confirmed undo.
type PromotionRevertResult struct {
	Plan         PromotionRevertPlan `json:"plan"`
	ReceiptID    string              `json:"receipt_id"`
	TransitionID string              `json:"transition_id"`
	IntentID     string              `json:"action_intent_id"`
	PolicyID     string              `json:"policy_decision_id"`
	PreImageRef  string              `json:"pre_image_ref"`
	PostImageRef string              `json:"post_image_ref"`
}

// PlanPromotionRevert validates an undo and resolves the exact pre-image bytes.
// It writes nothing. Target drift, revision drift, a missing artifact, or a
// malformed record all fail here.
func PlanPromotionRevert(homePath, appliedPath string) (PromotionRevertPlan, error) {
	if strings.TrimSpace(homePath) == "" {
		return PromotionRevertPlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	record, err := LoadPromotionAppliedRecord(appliedPath)
	if err != nil {
		return PromotionRevertPlan{}, err
	}
	if strings.TrimSpace(record.Status) != "applied" {
		return PromotionRevertPlan{}, fmt.Errorf("route promotion applied record %s status %q is not revertible", record.ID, record.Status)
	}
	if strings.TrimSpace(record.PreImageRef) == "" || strings.TrimSpace(record.PostImageRef) == "" {
		return PromotionRevertPlan{}, fmt.Errorf("route promotion applied record %s carries no pre/post image refs", record.ID)
	}
	if strings.TrimSpace(record.ActionIntent.ID) == "" || record.ActionIntent.ID != record.ActionIntentID {
		return PromotionRevertPlan{}, fmt.Errorf("route promotion applied record %s has no coherent original action intent", record.ID)
	}

	manifest, authorityPath, authorityRevision, err := loadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return PromotionRevertPlan{}, fmt.Errorf("load model capabilities authority: %w", err)
	}
	// Target drift: the record's intent must still name the live authority file.
	if strings.TrimSpace(record.ActionIntent.Target.Path) != authorityPath {
		return PromotionRevertPlan{}, fmt.Errorf("route promotion applied record %s targets %s, but the live %s authority is %s — refusing to revert across targets",
			record.ID, record.ActionIntent.Target.Path, ModelCapabilitiesAuthorityRef, authorityPath)
	}

	store, err := act.NewArtifactStore(homePath)
	if err != nil {
		return PromotionRevertPlan{}, err
	}
	// Bind the record to CANONICAL evidence before trusting ANY of its claims —
	// including its own revision strings. verifyAppliedRecordEvidence recomputes
	// both authority revisions from the actual pre/post artifact BODIES, so the
	// drift guard below compares against a derived fact rather than a field the
	// record asserts about itself.
	derived, err := verifyAppliedRecordEvidence(homePath, store, record)
	if err != nil {
		return PromotionRevertPlan{}, err
	}

	// Revision drift: the authority must still be exactly the post-image this
	// apply left behind, measured from that artifact's bytes. Comparing against a
	// self-asserted field here would let a tampered record claim the CURRENT
	// revision and overwrite a newer change with a stale pre-image.
	if authorityRevision != derived.afterRevision {
		return PromotionRevertPlan{}, fmt.Errorf("route promotion applied record %s post-image is revision %s, live authority revision is %s — refusing to revert over a newer change",
			record.ID, derived.afterRevision, authorityRevision)
	}

	current := ""
	if model, ok := manifest.Models[record.LaneID]; ok {
		current = strings.TrimSpace(model.Capabilities[record.CapabilityClass].Level)
	}

	preImage, err := store.Get(record.PreImageRef)
	if err != nil {
		return PromotionRevertPlan{}, fmt.Errorf("resolve pre-image %s for route promotion applied record %s: %w", record.PreImageRef, record.ID, err)
	}

	return PromotionRevertPlan{
		ReviewID:           record.ReviewID,
		AppliedRecordID:    record.ID,
		AppliedPath:        appliedPath,
		LaneID:             record.LaneID,
		CapabilityClass:    record.CapabilityClass,
		CurrentClaimLevel:  current,
		RestoredClaimLevel: record.PriorClaimLevel,
		AuthorityRef:       ModelCapabilitiesAuthorityRef,
		AuthorityPath:      authorityPath,
		AuthorityRevision:  authorityRevision,
		RestoredRevision:   derived.beforeRevision,
		PreImageRef:        record.PreImageRef,
		PostImageRef:       record.PostImageRef,
		PreImage:           preImage,
	}, nil
}

// verifyAppliedRecordEvidence proves an applied record is backed by canonical
// envelopes rather than by its own assertions. It resolves the named receipt and
// transition from the home's event store and requires them to agree with the
// record on the action intent and on both image refs; it then requires both
// artifacts to exist in the content-addressed store.
//
// Without this, `promotion-revert --applied <file>` would be a write primitive
// pointed at arbitrary attacker-supplied bytes: anyone who could author a JSON
// file could name any artifact ref as a "pre-image" and have it restored over
// the model-router authority under a real PolicyDecision.
func verifyAppliedRecordEvidence(homePath string, store *act.ArtifactStore, record PromotionAppliedRecord) (derivedRevisions, error) {
	var derived derivedRevisions
	events, err := eventstore.Open(homePath)
	if err != nil {
		return derived, fmt.Errorf("open event store to verify route promotion applied record %s: %w", record.ID, err)
	}

	receipt, err := loadReceiptRefs(events, record.ReceiptID)
	if err != nil {
		return derived, fmt.Errorf("route promotion applied record %s names receipt %s which does not resolve: %w", record.ID, record.ReceiptID, err)
	}
	transition, err := loadTransitionRefs(events, record.TransitionID)
	if err != nil {
		return derived, fmt.Errorf("route promotion applied record %s names transition %s which does not resolve: %w", record.ID, record.TransitionID, err)
	}

	// The receipt must prove exactly this write: both image refs, and the file it changed.
	if !containsAll(receipt.ProofRefs, record.PreImageRef, record.PostImageRef) {
		return derived, fmt.Errorf("route promotion applied record %s disagrees with receipt %s: proof_refs %v do not carry pre %s and post %s",
			record.ID, record.ReceiptID, receipt.ProofRefs, record.PreImageRef, record.PostImageRef)
	}
	if !containsAll(receipt.StateDelta.FilesChanged, record.AuthorityPath) {
		return derived, fmt.Errorf("route promotion applied record %s targets %s but receipt %s changed %v",
			record.ID, record.AuthorityPath, record.ReceiptID, receipt.StateDelta.FilesChanged)
	}
	// The transition must name the same governed action and the same before/after images.
	if !containsAll(transition.ActionRefs, record.ActionIntentID) {
		return derived, fmt.Errorf("route promotion applied record %s names intent %s but transition %s references %v",
			record.ID, record.ActionIntentID, record.TransitionID, transition.ActionRefs)
	}
	if !containsAll(transition.BeforeRefs, record.PreImageRef) || !containsAll(transition.AfterRefs, record.PostImageRef) {
		return derived, fmt.Errorf("route promotion applied record %s disagrees with transition %s on before/after images (%v -> %v)",
			record.ID, record.TransitionID, transition.BeforeRefs, transition.AfterRefs)
	}

	// The embedded ActionIntent body is UNTRUSTED input. Rebuild the intent
	// deterministically from the record's structured fields and require the
	// rebuilt id to equal the id the canonical transition corroborates: because
	// the id is a hash over work/capability/authority-path/target/review/level,
	// that equality authenticates every one of those inputs. Then require the
	// embedded body to match the rebuilt body field for field.
	//
	// Without this, an attacker could keep the real receipt/transition/intent IDs
	// and swap the embedded Target.Path, and the revert would faithfully restore
	// a captured blob over a DIFFERENT file under a real PolicyDecision.
	expected := expectedApplyIntent(record)
	if expected.ID != strings.TrimSpace(record.ActionIntentID) {
		return derived, fmt.Errorf("route promotion applied record %s does not reproduce its own action intent id: record fields derive %s but the record claims %s",
			record.ID, expected.ID, record.ActionIntentID)
	}
	if err := sameGovernedIntentBody(expected, record.ActionIntent); err != nil {
		return derived, fmt.Errorf("route promotion applied record %s embedded action intent was tampered: %w", record.ID, err)
	}

	// Both images must physically exist in the content-addressed store.
	if _, err := store.Verify(record.PreImageRef); err != nil {
		return derived, fmt.Errorf("route promotion applied record %s pre-image %s is not resolvable in the artifact store: %w", record.ID, record.PreImageRef, err)
	}
	if _, err := store.Verify(record.PostImageRef); err != nil {
		return derived, fmt.Errorf("route promotion applied record %s post-image %s is not resolvable in the artifact store: %w", record.ID, record.PostImageRef, err)
	}

	// Finally, DERIVE both authority revisions from the artifact BODIES and require
	// the record's own revision strings to match exactly.
	//
	// These two fields decide (a) whether the drift guard lets a restore proceed
	// and (b) which revision the source-authority ledger is stamped with after a
	// successful restore. Left self-asserted, a tampered authority_revision_after
	// could name the CURRENT revision and wave a stale pre-image over a newer
	// change, and a tampered authority_revision_before could stamp the ledger with
	// a revision the file does not carry — an authority/ledger split by forgery.
	before, err := authorityRevisionFromImage(store, record.PreImageRef)
	if err != nil {
		return derived, fmt.Errorf("route promotion applied record %s pre-image %s could not be measured: %w", record.ID, record.PreImageRef, err)
	}
	after, err := authorityRevisionFromImage(store, record.PostImageRef)
	if err != nil {
		return derived, fmt.Errorf("route promotion applied record %s post-image %s could not be measured: %w", record.ID, record.PostImageRef, err)
	}
	if strings.TrimSpace(record.AuthorityRevisionBefore) != before {
		return derived, fmt.Errorf("route promotion applied record %s claims authority_revision_before %s but its pre-image artifact %s measures %s — refusing a forged revision",
			record.ID, record.AuthorityRevisionBefore, record.PreImageRef, before)
	}
	if strings.TrimSpace(record.AuthorityRevisionAfter) != after {
		return derived, fmt.Errorf("route promotion applied record %s claims authority_revision_after %s but its post-image artifact %s measures %s — refusing a forged revision",
			record.ID, record.AuthorityRevisionAfter, record.PostImageRef, after)
	}
	derived.beforeRevision = before
	derived.afterRevision = after
	return derived, nil
}

// derivedRevisions carries authority revisions MEASURED from artifact bodies, as
// opposed to the strings an applied record asserts about itself.
type derivedRevisions struct {
	beforeRevision string
	afterRevision  string
}

// actFilePreImageSchema mirrors internal/act's FileArtifactAdapter pre-image
// encoding. Reading it here (rather than importing an unexported decoder) lets
// this package measure what a captured image actually contains. The fallback
// matches act's own: an untagged blob is treated as raw content.
const actFilePreImageSchema = "compound.file_preimage.v0.1"

type actFilePreImage struct {
	Schema  string `json:"schema"`
	Exists  bool   `json:"exists"`
	Content []byte `json:"content,omitempty"`
}

func decodeActFileImage(blob []byte) []byte {
	var p actFilePreImage
	if err := json.Unmarshal(blob, &p); err == nil && p.Schema == actFilePreImageSchema {
		return p.Content
	}
	return blob
}

// authorityRevisionFromImage measures the model-router revision a captured image
// represents, using the same digest scheme the importer and the apply path use.
func authorityRevisionFromImage(store *act.ArtifactStore, ref string) (string, error) {
	blob, err := store.Get(ref)
	if err != nil {
		return "", err
	}
	return "sha256:" + digestHex(decodeActFileImage(blob)), nil
}

// receiptRefs and transitionRefs are the narrow slices of each envelope this
// verification needs. They are decoded separately because a Receipt's state_delta
// is an OBJECT while a Transition's is a summary STRING — one shared shape cannot
// parse both, and pretending otherwise silently fails every verification.
type receiptRefs struct {
	ProofRefs  []string `json:"proof_refs"`
	StateDelta struct {
		FilesChanged []string `json:"files_changed"`
	} `json:"state_delta"`
}

type transitionRefs struct {
	ActionRefs []string `json:"action_refs"`
	BeforeRefs []string `json:"before_refs"`
	AfterRefs  []string `json:"after_refs"`
}

func loadReceiptRefs(events *eventstore.Store, id string) (receiptRefs, error) {
	var out receiptRefs
	body, err := readEnvelopeBytes(events, id)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("parse receipt %s: %w", id, err)
	}
	return out, nil
}

func loadTransitionRefs(events *eventstore.Store, id string) (transitionRefs, error) {
	var out transitionRefs
	body, err := readEnvelopeBytes(events, id)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("parse transition %s: %w", id, err)
	}
	return out, nil
}

func readEnvelopeBytes(events *eventstore.Store, id string) ([]byte, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("empty envelope id")
	}
	rec, err := events.Read(id)
	if err != nil {
		return nil, err
	}
	return rec.Bytes, nil
}

func containsAll(haystack []string, wanted ...string) bool {
	for _, want := range wanted {
		found := false
		for _, have := range haystack {
			if strings.TrimSpace(have) == strings.TrimSpace(want) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// expectedApplyIntent rebuilds the forward ActionIntent a given applied record
// MUST have executed, from the record's own structured fields. It is the trusted
// reconstruction: the revert is built from this, never from the record's embedded
// intent body.
func expectedApplyIntent(record PromotionAppliedRecord) envelope.ActionIntent {
	plan := PromotionApplyPlan{
		ReviewID:           strings.TrimSpace(record.ReviewID),
		Recommendation:     strings.TrimSpace(record.Recommendation),
		LaneID:             strings.TrimSpace(record.LaneID),
		CapabilityClass:    strings.TrimSpace(record.CapabilityClass),
		CurrentClaimLevel:  strings.TrimSpace(record.PriorClaimLevel),
		ProposedClaimLevel: strings.TrimSpace(record.AppliedClaimLevel),
		AuthorityPath:      strings.TrimSpace(record.AuthorityPath),
		AuthorityRevision:  strings.TrimSpace(record.AuthorityRevisionBefore),
		ProposedRevision:   strings.TrimSpace(record.AuthorityRevisionAfter),
	}
	// CreatedAt does not participate in the intent id and is not safety-relevant;
	// reuse the recorded value so the reconstruction is otherwise identical.
	created, err := time.Parse(time.RFC3339, strings.TrimSpace(record.ActionIntent.CreatedAt))
	if err != nil {
		created = time.Unix(0, 0).UTC()
	}
	return PromotionApplyIntent(strings.TrimSpace(record.ActionIntent.IDs.WorkID), plan, created)
}

// sameGovernedIntentBody compares the fields that decide WHAT a revert touches
// and under what authority. Anything unequal means the embedded body cannot be
// trusted to describe the recorded action.
func sameGovernedIntentBody(want, got envelope.ActionIntent) error {
	checks := []struct {
		field     string
		want, got string
	}{
		{"id", want.ID, got.ID},
		{"capability", want.Capability, got.Capability},
		{"action", want.Action, got.Action},
		{"target.kind", want.Target.Kind, got.Target.Kind},
		{"target.id", want.Target.ID, got.Target.ID},
		{"target.path", want.Target.Path, got.Target.Path},
		{"mutation_class", want.MutationClass, got.MutationClass},
		{"reversibility", want.Reversibility, got.Reversibility},
		{"blast_radius", want.BlastRadius, got.BlastRadius},
		{"actor", want.Actor, got.Actor},
	}
	for _, c := range checks {
		if strings.TrimSpace(c.want) != strings.TrimSpace(c.got) {
			return fmt.Errorf("%s is %q, expected %q", c.field, c.got, c.want)
		}
	}
	return nil
}

// compensateToLedgerRevision puts the authority file back to the image the
// source-authority ledger head already names, so authority and ledger AGREE even
// though the revert failed. Restoring agreement is the point: a home where the
// ledger head names a revision the YAML no longer carries is one where route
// planning and promotion review both refuse.
//
// The compensating write goes through the SAME act seam under the SAME decision
// that authorized this restore. If it fails, a durable recovery finding is
// recorded so an unreconciled authority/ledger split is never silent.
func compensateToLedgerRevision(homePath string, store *act.ArtifactStore, adapter act.Adapter, record PromotionAppliedRecord, pd envelope.PolicyDecision, targetImageRef string, cause error, now time.Time) error {
	image, gerr := store.Get(targetImageRef)
	if gerr != nil {
		return recordAuthorityLedgerSplit(homePath, record, targetImageRef, cause,
			fmt.Errorf("compensating image %s could not be read: %w", targetImageRef, gerr), now)
	}
	base := expectedApplyIntent(record)
	ai := act.RevertIntent(base, act.Result{PreImageRef: targetImageRef, PostImageRef: targetImageRef}, now)
	ai.Preview = "restore authority to the revision named by the source-authority ledger head"
	cpd := pd
	cpd.ActionIntentID = ai.ID
	if _, cerr := act.ExecuteRevert(store, adapter, ai, cpd, image, now); cerr != nil {
		return recordAuthorityLedgerSplit(homePath, record, targetImageRef, cause, cerr, now)
	}
	return fmt.Errorf("route promotion revert FAILED: the source-authority ledger head could not be updated (%w); the authority was put back to the revision the ledger names (%s), so authority and ledger agree and NO claim change is in effect",
		cause, record.AuthorityRevisionAfter)
}

// recordAuthorityLedgerSplit lands durable, operator-visible recovery evidence
// when authority and ledger could NOT be brought back into agreement. It mirrors
// internal/act's own recovery-finding contract: evidence class, never a ledger
// byte, surfaced by the doctor's finding counts.
func recordAuthorityLedgerSplit(homePath string, record PromotionAppliedRecord, targetImageRef string, cause, compensationErr error, now time.Time) error {
	body := fmt.Sprintf(
		"model-router authority and source-authority ledger DISAGREE — manual recovery required.\n"+
			"applied_record: %s\nreview_id: %s\nauthority_path: %s\n"+
			"ledger_head_revision: %s\nauthority_expected_image: %s\npre_image_ref: %s\npost_image_ref: %s\n"+
			"original_error: %v\ncompensation_error: %v\n"+
			"effect: latestModelCapabilitiesSource will refuse until the ledger head and the authority file name the same revision.\n",
		record.ID, record.ReviewID, record.AuthorityPath,
		record.AuthorityRevisionAfter, targetImageRef, record.PreImageRef, record.PostImageRef,
		cause, compensationErr)
	rec, ferr := finding.Record(homePath, finding.Finding{
		Topic:        "act-recovery/model_router_authority_ledger_split",
		Body:         body,
		ProducedBy:   "internal/routeevidence",
		WorkID:       strings.TrimSpace(record.ActionIntent.IDs.WorkID),
		EvidenceRefs: []string{record.PreImageRef, record.PostImageRef},
	}, now)
	if ferr != nil {
		return fmt.Errorf("route promotion revert FAILED (%v), compensation FAILED (%v), AND the recovery finding could not be recorded (%v) — %s and its source-authority ledger DISAGREE; ledger names %s; MANUAL RECOVERY REQUIRED",
			cause, compensationErr, ferr, record.AuthorityPath, record.AuthorityRevisionAfter)
	}
	return fmt.Errorf("route promotion revert FAILED (%v) and compensation FAILED (%v) — %s and its source-authority ledger DISAGREE; recovery finding %s records the exact refs; MANUAL RECOVERY REQUIRED",
		cause, compensationErr, record.AuthorityPath, rec.ID)
}

// PromotionRevertIntent builds the revert ActionIntent from the RECONSTRUCTED
// forward intent (see expectedApplyIntent), never from the record's embedded
// body, so an undo is bound to the governed action the canonical envelopes
// attest rather than to operator-supplied JSON.
func PromotionRevertIntent(workID string, record PromotionAppliedRecord, now time.Time) envelope.ActionIntent {
	ai := act.RevertIntent(expectedApplyIntent(record), act.Result{PreImageRef: record.PreImageRef, PostImageRef: record.PostImageRef}, now)
	ai.IDs.WorkID = workID
	return ai
}

// RevertPromotionApply performs the governed restore. It requires an allowing
// PolicyDecision for the revert intent; act.ExecuteRevert re-gates by
// construction. On success the applied record is marked reverted so the review
// is appliable again and the queue never shows a stale "applied" resolution.
func RevertPromotionApply(homePath, workID string, plan PromotionRevertPlan, pd envelope.PolicyDecision, now time.Time) (PromotionRevertResult, error) {
	if strings.TrimSpace(workID) == "" {
		return PromotionRevertResult{}, fmt.Errorf("work id required for a governed promotion revert")
	}
	// PromotionRevertPlan is an exported preview, not authority. Rebuild it from
	// the applied record immediately before mutation and use only the newly
	// verified path, revisions, refs, and content-addressed pre-image below.
	freshPlan, err := PlanPromotionRevert(homePath, plan.AppliedPath)
	if err != nil {
		return PromotionRevertResult{}, err
	}
	plan = freshPlan
	record, err := LoadPromotionAppliedRecord(plan.AppliedPath)
	if err != nil {
		return PromotionRevertResult{}, err
	}
	store, err := act.NewArtifactStore(homePath)
	if err != nil {
		return PromotionRevertResult{}, err
	}
	// TOCTOU guard, mirroring the apply path: the authority must still be exactly
	// the revision this revert was planned against, immediately before the act.
	if err := assertAuthorityRevision(homePath, plan.AuthorityRevision, "revert"); err != nil {
		return PromotionRevertResult{}, err
	}
	// Re-verify the record against canonical evidence too: planning and executing
	// are separate calls, and the record file is operator-supplied input.
	if _, err := verifyAppliedRecordEvidence(homePath, store, record); err != nil {
		return PromotionRevertResult{}, err
	}
	ai := PromotionRevertIntent(workID, record, now)
	adapter := act.NewFileArtifactAdapter()
	res, err := act.ExecuteRevert(store, adapter, ai, pd, plan.PreImage, now)
	if err != nil {
		return PromotionRevertResult{}, err
	}

	// The restore moved the authority back to its pre-apply revision, so the
	// source-authority ledger head must follow. Without this the ledger would
	// still name the reverted-away revision and every later route plan or
	// promotion review would error out against a "restored" authority.
	// Stamp the ledger with the revision MEASURED from the pre-image artifact, so
	// a forged authority_revision_before cannot point the ledger head at a
	// revision the restored file does not carry.
	restoredRevision := plan.RestoredRevision
	if lerr := appendPromotionAuthorityRecordFn(homePath, plan.AuthorityPath, restoredRevision, promotionRevertCommandName, record.ReviewID, res); lerr != nil {
		// The YAML is now at the PRE revision while the ledger head still names the
		// POST revision. That disagreement makes latestModelCapabilitiesSource
		// refuse, breaking every later route plan and promotion review — so it is
		// not acceptable to simply return. Compensate by putting the authority back
		// to the POST image the ledger already describes, restoring agreement, and
		// report the revert as failed with nothing changed.
		return PromotionRevertResult{}, compensateToLedgerRevision(homePath, store, adapter, record, pd, plan.PostImageRef, lerr, now)
	}
	record.Status = "reverted"
	record.UndoCommand = ""
	record.EvidenceRefs = reviewartifact.UniqueSorted(append(append([]string{}, record.EvidenceRefs...),
		res.Receipt.ID, res.Transition.ID, res.PreImageRef, res.PostImageRef))
	if _, werr := writePromotionAppliedRecordFn(homePath, record.ReviewID, record); werr != nil {
		// The authority AND the ledger head are both back at the pre-apply
		// revision, so the home is coherent: the claim change is genuinely undone.
		// Only the record's own status line is stale, and because it still names
		// the rolled-back post revision, a later revert of the same record fails
		// closed on the revision guard rather than re-applying anything. Report it
		// as the bookkeeping failure it is.
		return PromotionRevertResult{}, fmt.Errorf("authority and source-authority ledger were restored to %s (receipt %s, transition %s) but the applied record at %s could not be marked reverted (%w) — the undo IS in effect; the stale record is refused by the revision guard, delete or repair it",
			plan.RestoredClaimLevel, res.Receipt.ID, res.Transition.ID, plan.AppliedPath, werr)
	}

	return PromotionRevertResult{
		Plan:         plan,
		ReceiptID:    res.Receipt.ID,
		TransitionID: res.Transition.ID,
		IntentID:     ai.ID,
		PolicyID:     pd.ID,
		PreImageRef:  res.PreImageRef,
		PostImageRef: res.PostImageRef,
	}, nil
}
