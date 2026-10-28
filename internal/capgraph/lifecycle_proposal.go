package capgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/reviewartifact"
)

const (
	CapabilityLifecycleProposalSchemaVersion = "compound.capability.lifecycle_proposal.v0.1"
	CapabilityLifecycleProposalReviewArea    = "capability-lifecycle-proposals"
)

// CapabilityLifecycleProposal is an owner-review artifact for revising or
// retiring CapabilityGraph behavior. It never mutates graph nodes, installs
// skills, executes tools, or promotes memory truth.
type CapabilityLifecycleProposal struct {
	SchemaVersion string                           `json:"schema_version"`
	ID            string                           `json:"id"`
	CreatedAt     string                           `json:"created_at"`
	Status        string                           `json:"status"`
	ProposalKind  string                           `json:"proposal_kind"`
	Subject       string                           `json:"subject"`
	Statement     string                           `json:"statement"`
	Detail        string                           `json:"detail,omitempty"`
	Confidence    float64                          `json:"confidence,omitempty"`
	EvidenceRefs  []string                         `json:"evidence_refs"`
	Draft         CapabilityLifecycleProposalDraft `json:"draft"`
	Warnings      []string                         `json:"warnings,omitempty"`
}

type CapabilityLifecycleProposalDraft struct {
	LifecycleAction  string   `json:"lifecycle_action"`
	CapabilityRef    string   `json:"capability_ref,omitempty"`
	ProposedChange   string   `json:"proposed_change"`
	Guardrail        string   `json:"guardrail"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewNote       string   `json:"review_note"`
}

type CapabilityLifecycleProposalInput struct {
	ProposalKind  string
	Subject       string
	Statement     string
	Detail        string
	Confidence    float64
	EvidenceRefs  []string
	CapabilityRef string
}

type CapabilityLifecycleProposalPaths struct {
	PlanPath   string
	ReviewPath string
}

func BuildCapabilityLifecycleProposal(in CapabilityLifecycleProposalInput, now time.Time) (CapabilityLifecycleProposal, error) {
	kind := strings.TrimSpace(in.ProposalKind)
	if kind == "" {
		kind = "revise"
	}
	if kind != "revise" && kind != "retire" {
		return CapabilityLifecycleProposal{}, fmt.Errorf("proposal_kind must be revise or retire")
	}
	evidenceRefs := reviewartifact.Unique(in.EvidenceRefs)
	if len(evidenceRefs) == 0 {
		return CapabilityLifecycleProposal{}, fmt.Errorf("capability lifecycle proposal evidence_refs required")
	}
	statement := reviewartifact.FirstNonEmpty(in.Statement, in.Subject)
	if strings.TrimSpace(statement) == "" {
		return CapabilityLifecycleProposal{}, fmt.Errorf("capability lifecycle proposal statement required")
	}
	subject := reviewartifact.FirstNonEmpty(in.Subject, statement)
	key := reviewartifact.Slugify(reviewartifact.FirstNonEmpty(in.CapabilityRef, subject, statement), 48)
	if key == "" {
		key = reviewartifact.Digest12(statement)
	}
	id := reviewartifact.ID("clp_", kind, key, strings.Join(evidenceRefs, "\x00"))
	proposal := CapabilityLifecycleProposal{
		SchemaVersion: CapabilityLifecycleProposalSchemaVersion,
		ID:            id,
		CreatedAt:     reviewartifact.Timestamp(now),
		Status:        "review_required",
		ProposalKind:  kind,
		Subject:       subject,
		Statement:     statement,
		Detail:        strings.TrimSpace(in.Detail),
		Confidence:    in.Confidence,
		EvidenceRefs:  evidenceRefs,
		Draft: CapabilityLifecycleProposalDraft{
			LifecycleAction: kind,
			CapabilityRef:   strings.TrimSpace(in.CapabilityRef),
			ProposedChange:  statement,
			Guardrail:       "Do not mutate CapabilityGraph, skill files, adapters, or routing defaults from this proposal without an explicit reviewed owner action.",
			RequiredEvidence: []string{
				"session evidence that produced the lifecycle proposal",
				"current capability or skill behavior being revised or retired",
				"overlap/replacement scan against existing CapabilityGraph nodes",
				"validation receipt or eval proving the revised lifecycle behavior",
				"rollback or re-enable condition for any retired behavior",
			},
			ReviewNote: "This lifecycle proposal is review material only. A reviewer may revise, retire, merge, or reject it, but importing or mutating CapabilityGraph requires a separate reviewed owner path.",
		},
		Warnings: []string{
			"does not mutate CapabilityGraph",
			"does not install or edit skill files",
			"does not execute tools or MCPs",
			"does not promote memory truth",
		},
	}
	return proposal, nil
}

// AnnotateCapabilityLifecycleProposalQueue fills in the operator card for
// capability-lifecycle-proposals review artifacts. The proposal remains source
// evidence; a separate decision sidecar drives queue movement after review.
func AnnotateCapabilityLifecycleProposalQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	now, err := time.Parse(time.RFC3339, report.GeneratedAt)
	if err != nil {
		now = time.Now()
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != CapabilityLifecycleProposalReviewArea {
			continue
		}
		annotateCapabilityLifecycleProposalItem(item, now)
	}
}

func annotateCapabilityLifecycleProposalItem(item *reviewartifact.QueueItem, now time.Time) {
	if item.JSONPath == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "capability lifecycle proposal path missing"
		item.NextAction = "regenerate capability lifecycle proposal"
		return
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "capability lifecycle proposal unreadable"
		return
	}
	var proposal CapabilityLifecycleProposal
	if err := json.Unmarshal(data, &proposal); err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "capability lifecycle proposal unreadable"
		return
	}
	snapshot, err := LoadCapabilityLifecycleProposalSnapshot(filepath.Dir(filepath.Dir(filepath.Dir(item.JSONPath))), proposal.ID)
	if err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "capability lifecycle proposal snapshot unreadable"
		return
	}
	item.EvidenceRefs = reviewartifact.Unique(append(item.EvidenceRefs, proposal.EvidenceRefs...))
	item.Owner = "capability-graph"
	band := reviewartifact.BandNeedsYou
	var disposition *CapabilityLifecycleDisposition
	var reservation *CapabilityLifecycleDispositionReservation
	choicesAvailable := true
	recorded, recordedOK := capabilityLifecycleDispositionFromExtras(item.ExtraPaths)
	recordedOK = recordedOK && recorded.ProposalID == proposal.ID && recorded.ProposalHash == snapshot.Hash
	reserved, reservedOK := capabilityLifecycleReservationFromExtras(item.ExtraPaths)
	reservedOK = reservedOK && reserved.ProposalID == proposal.ID && reserved.ProposalHash == snapshot.Hash
	reservationResolved := false
	if reservedOK {
		if final, ok := capabilityLifecycleDispositionFromExtrasByID(item.ExtraPaths, reserved.DispositionID); ok && final.ProposalID == proposal.ID && final.ProposalHash == snapshot.Hash {
			recorded = final
			recordedOK = true
			reservationResolved = true
		}
	}
	if reservedOK && !reservationResolved {
		reservation = &reserved
		choicesAvailable = false
		item.Actions = []reviewartifact.Action{capabilityLifecycleResumeAction(reserved)}
		item.Status = "held"
		band = reviewartifact.BandNeedsYou
		item.Readiness = "ready_to_apply"
		item.BlockedReason = ""
		item.NextAction = "resume the exact reserved tracked-work disposition " + reserved.DispositionID
	} else if recordedOK {
		disposition = &recorded
		item.Actions = nil
		item.BlockedReason = ""
		switch recorded.Decision {
		case CapabilityLifecycleOpenWork:
			choicesAvailable = false
			item.Status = "recorded"
			band = recorded.AfterBand
			item.Readiness = "no_action"
			item.NextAction = "capability lifecycle proposal linked to proposed work " + recorded.WorkID
		case CapabilityLifecycleDefer:
			revisitAt, err := time.Parse(time.RFC3339, recorded.DeferUntil)
			if err == nil && !now.Before(revisitAt) {
				item.Actions = capabilityLifecycleDecisionActions()
				item.Status = "review_required"
				band = reviewartifact.BandNeedsYou
				item.Readiness = "needs_review"
				item.NextAction = "defer trigger reached; review capability lifecycle proposal now"
			} else {
				choicesAvailable = false
				item.Status = "held"
				band = recorded.AfterBand
				item.Readiness = "watching"
				item.NextAction = "revisit capability lifecycle proposal after " + recorded.DeferUntil
			}
		default:
			choicesAvailable = false
			item.Status = "recorded"
			band = recorded.AfterBand
			item.Readiness = "no_action"
			item.NextAction = "capability lifecycle proposal rejected and filed"
		}
	} else if reviewartifact.TerminalStatus(proposal.Status) {
		choicesAvailable = false
		item.Actions = nil
		band = reviewartifact.BandFiled
		item.Readiness = "no_action"
		item.NextAction = "capability lifecycle proposal recorded"
	} else {
		item.Actions = capabilityLifecycleDecisionActions()
		item.Readiness = "needs_review"
		item.NextAction = "review capability lifecycle proposal " + reviewartifact.FirstNonEmpty(proposal.Subject, proposal.ID)
	}
	seedCapabilityLifecycleActions(item, proposal, snapshot.Hash)
	card, err := OperatorCardForLifecycleProposal(proposal)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "capability lifecycle operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "capability lifecycle operator card invalid: "+err.Error())
	}
	item.DecisionCard = capabilityLifecycleDecisionCard(snapshot, disposition)
	if reservation != nil {
		item.DecisionCard = capabilityLifecycleReservationDecisionCard(snapshot, *reservation)
	} else if !choicesAvailable {
		item.DecisionCard.Choices = nil
		item.DecisionCard.CanonicalActionHint = ""
	}
}

func seedCapabilityLifecycleActions(item *reviewartifact.QueueItem, proposal CapabilityLifecycleProposal, proposalHash string) {
	if item == nil {
		return
	}
	workID := reviewartifact.AuthoritySeedWorkID(*item, "")
	if workID == "" {
		return
	}
	evidenceRefs := reviewartifact.UniqueSorted(append(append([]string{}, proposal.EvidenceRefs...),
		"capability-lifecycle-proposal:"+proposal.ID,
		"capability-lifecycle-proposal-hash:"+proposalHash,
	))
	for i := range item.Actions {
		action := &item.Actions[i]
		actionName := "record_capability_lifecycle_" + action.ID
		preview := "Record the reviewed " + action.ID + " disposition for capability lifecycle proposal " + proposal.ID + "."
		rollback := "Record a superseding lifecycle proposal or disposition before any downstream work executes."
		receipt := "Recorded " + action.ID + " for capability lifecycle proposal " + proposal.ID + "."
		if action.ID == CapabilityLifecycleOpenWork || action.ID == "resume_open_work" {
			actionName = "open_capability_lifecycle_work"
			preview = "Create or resume the exact proposed WorkEnvelope for capability lifecycle proposal " + proposal.ID + "; no capability graph mutation executes here."
			rollback = "Cancel the proposed WorkEnvelope before execution and retain the proposal/reservation as evidence."
			receipt = "Created or resumed tracked work for capability lifecycle proposal " + proposal.ID + "."
		}
		_ = reviewartifact.SeedActionAuthority(action, reviewartifact.ActionAuthoritySeedSpec{
			WorkID: workID, CreatedAt: proposal.CreatedAt, ProducerName: "compound/internal/capgraph",
			Capability: "capability.lifecycle.review", Action: actionName,
			Target:        envelope.ActionTarget{Kind: "capability_lifecycle_proposal", ID: proposal.ID, Path: item.JSONPath},
			MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local",
			Preview: preview, RollbackPlan: rollback, Privacy: "private", Risk: "tier1",
			EvidenceRefs: evidenceRefs, TargetScope: "private", ExpectedReceipt: receipt,
			ExpectedProof: append([]string{
				"capability-lifecycle-proposal:" + proposal.ID,
				"capability-lifecycle-proposal-hash:" + proposalHash,
			}, evidenceRefs...),
		})
	}
}

func RecordCapabilityLifecycleProposal(homePath string, proposal CapabilityLifecycleProposal) (CapabilityLifecycleProposalPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return CapabilityLifecycleProposalPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(proposal.ID) == "" {
		return CapabilityLifecycleProposalPaths{}, fmt.Errorf("capability lifecycle proposal id required")
	}
	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		return CapabilityLifecycleProposalPaths{}, err
	}
	out := CapabilityLifecycleProposalPaths{
		PlanPath:   filepath.Join(dir, proposal.ID+".json"),
		ReviewPath: filepath.Join(dir, proposal.ID+".md"),
	}
	err = withCapabilityLifecycleDispositionLock(homePath, proposal.ID, func() error {
		winner, collision, err := publishCapabilityLifecycleProposalJSON(out.PlanPath, proposal)
		if err != nil {
			return err
		}
		// Markdown is a projection of the immutable JSON winner. Publishing JSON
		// first makes a crash between the pair recoverable by any later replay,
		// including a losing concurrent semantic collision.
		if err := recoverCapabilityLifecycleProposalMarkdown(out.ReviewPath, winner); err != nil {
			return err
		}
		if collision {
			return fmt.Errorf("capability lifecycle proposal %s semantic collision; refusing to overwrite reviewed evidence", proposal.ID)
		}
		return nil
	})
	return out, err
}

func publishCapabilityLifecycleProposalJSON(path string, requested CapabilityLifecycleProposal) (CapabilityLifecycleProposal, bool, error) {
	if existing, err := loadCapabilityLifecycleProposalCanonical(path, requested.ID); err == nil {
		return existing, !sameCapabilityLifecycleProposalSemantics(existing, requested), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return CapabilityLifecycleProposal{}, false, err
	}

	data, err := marshalCapabilityLifecycleProposalCanonical(requested)
	if err != nil {
		return CapabilityLifecycleProposal{}, false, err
	}
	if err := publishCapabilityLifecycleCanonical(path, data, false); err != nil && !errors.Is(err, os.ErrExist) {
		return CapabilityLifecycleProposal{}, false, err
	}
	winner, err := loadCapabilityLifecycleProposalCanonical(path, requested.ID)
	if err != nil {
		return CapabilityLifecycleProposal{}, false, err
	}
	return winner, !sameCapabilityLifecycleProposalSemantics(winner, requested), nil
}

func loadCapabilityLifecycleProposalCanonical(path, proposalID string) (CapabilityLifecycleProposal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CapabilityLifecycleProposal{}, err
	}
	var proposal CapabilityLifecycleProposal
	if err := json.Unmarshal(data, &proposal); err != nil {
		return CapabilityLifecycleProposal{}, fmt.Errorf("existing capability lifecycle proposal %s unreadable: %w", proposalID, err)
	}
	if proposal.SchemaVersion != CapabilityLifecycleProposalSchemaVersion || proposal.ID != proposalID {
		return CapabilityLifecycleProposal{}, fmt.Errorf("existing capability lifecycle proposal %s identity/schema mismatch", proposalID)
	}
	if strings.TrimSpace(proposal.Statement) == "" || len(reviewartifact.Unique(proposal.EvidenceRefs)) == 0 {
		return CapabilityLifecycleProposal{}, fmt.Errorf("existing capability lifecycle proposal %s incomplete", proposalID)
	}
	return proposal, nil
}

func sameCapabilityLifecycleProposalSemantics(existing, requested CapabilityLifecycleProposal) bool {
	// CreatedAt is observation metadata, not semantic identity. The first
	// writer's bytes remain authoritative even when a replay observes later.
	existing.CreatedAt = requested.CreatedAt
	return reflect.DeepEqual(existing, requested)
}

func marshalCapabilityLifecycleProposalCanonical(proposal CapabilityLifecycleProposal) ([]byte, error) {
	data, err := json.Marshal(proposal)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	card, err := OperatorCardForLifecycleProposal(proposal)
	if err != nil {
		return nil, fmt.Errorf("capability lifecycle proposal %s operator card: %w", proposal.ID, err)
	}
	if err := reviewartifact.ValidateOperatorCard(card); err != nil {
		return nil, fmt.Errorf("capability lifecycle proposal %s invalid operator card: %w", proposal.ID, err)
	}
	reviewartifact.PutOperatorCard(payload, card)
	payload["summary_source"] = reviewartifact.SummarySourceWriter
	data, err = json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func recoverCapabilityLifecycleProposalMarkdown(path string, proposal CapabilityLifecycleProposal) error {
	want := []byte(RenderCapabilityLifecycleProposalReview(proposal))
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(current, want) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	repair := err == nil
	if err := publishCapabilityLifecycleCanonical(path, want, repair); err != nil {
		if errors.Is(err, os.ErrExist) {
			current, readErr := os.ReadFile(path)
			if readErr == nil && bytes.Equal(current, want) {
				return nil
			}
		}
		return err
	}
	return nil
}

func RenderCapabilityLifecycleProposalReview(proposal CapabilityLifecycleProposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Capability Lifecycle Review: %s\n\n", proposal.ID)
	fmt.Fprintf(&b, "- status: %s\n", proposal.Status)
	fmt.Fprintf(&b, "- proposal_kind: %s\n", proposal.ProposalKind)
	fmt.Fprintf(&b, "- subject: %s\n", proposal.Subject)
	if proposal.Draft.CapabilityRef != "" {
		fmt.Fprintf(&b, "- capability_ref: %s\n", proposal.Draft.CapabilityRef)
	}
	fmt.Fprintf(&b, "- evidence_refs: %s\n\n", strings.Join(proposal.EvidenceRefs, ", "))
	fmt.Fprintln(&b, "This owner-review artifact does not mutate CapabilityGraph, install or edit skill files, execute tools/MCPs, or promote memory truth.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Proposal")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- statement: %s\n", proposal.Statement)
	if proposal.Detail != "" {
		fmt.Fprintf(&b, "- detail: %s\n", proposal.Detail)
	}
	if proposal.Confidence > 0 {
		fmt.Fprintf(&b, "- confidence: %.2f\n", proposal.Confidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Draft Lifecycle Change")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- action: %s\n", proposal.Draft.LifecycleAction)
	fmt.Fprintf(&b, "- proposed_change: %s\n", proposal.Draft.ProposedChange)
	fmt.Fprintf(&b, "- guardrail: %s\n", proposal.Draft.Guardrail)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Mutation")
	fmt.Fprintln(&b)
	for _, evidence := range proposal.Draft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n", proposal.Draft.ReviewNote)
	return b.String()
}
