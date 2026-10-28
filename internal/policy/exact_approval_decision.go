package policy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

// ValidateConsumedDecision verifies that a pre-evaluated decision was produced
// by the exact one-use approval store for this action. It accepts a consumed
// approval after expiry only for receipt finalization, never for a new effect.
func (s *ExactActionApprovalStore) ValidateConsumedDecision(action envelope.ActionIntent, decision envelope.PolicyDecision) error {
	if s == nil || s.homePath == "" {
		return fmt.Errorf("exact approval store requires home")
	}
	if decision.ActionIntentID != action.ID || decision.Outcome != "allow_with_obligations" ||
		decision.StandingGrantID != "" || decision.ExpiresAt == "" {
		return fmt.Errorf("decision is not an exact approved decision for action")
	}
	const prefix = "exact one-use operator approval "
	const suffix = " matches action binding"
	approvalID := ""
	for _, reason := range decision.Reasons {
		if !strings.HasPrefix(reason, prefix) || !strings.HasSuffix(reason, suffix) {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(reason, prefix), suffix)
		if approvalID != "" || !strings.HasPrefix(id, "approval_grant_") {
			return fmt.Errorf("ambiguous exact approval reason")
		}
		approvalID = id
	}
	if approvalID == "" {
		return fmt.Errorf("exact approval reason missing")
	}
	status, err := s.Check(approvalID, time.Now())
	if err != nil {
		return err
	}
	if status.Status != ExactApprovalStatusConsumed || status.ExpiresAt != decision.ExpiresAt || status.ConsumedAt == "" {
		return fmt.Errorf("exact approval is not consumed for this decision")
	}
	consumedAt, err := time.Parse(time.RFC3339Nano, status.ConsumedAt)
	if err != nil {
		return err
	}
	if decision.ID != idFor("pol_", action.ID, "exact-approval", approvalID, ts(consumedAt)) {
		return fmt.Errorf("exact approval decision identity differs from consumption")
	}
	store, err := eventstore.Open(s.homePath)
	if err != nil {
		return err
	}
	decisionRecord, err := store.Read(decision.ID)
	if err != nil {
		return fmt.Errorf("exact approval decision is not durable: %w", err)
	}
	var durableDecision envelope.PolicyDecision
	if err := json.Unmarshal(decisionRecord.Bytes, &durableDecision); err != nil || !reflect.DeepEqual(durableDecision, decision) {
		return fmt.Errorf("exact approval durable decision differs from supplied decision")
	}
	approvalRecord, err := store.Read(approvalID)
	if err != nil {
		return err
	}
	approval, err := validateExactActionApprovalRecord(approvalRecord.ID, approvalRecord.Bytes)
	if err != nil {
		return err
	}
	planRef, current, err := validateExactPlanGrant(store, approval)
	if err != nil {
		return err
	}
	if !current || planRef == "" || !containsExactString(decision.EvidenceRefs, planRef) {
		return fmt.Errorf("exact approval lacks current plan evidence")
	}
	planID := strings.TrimPrefix(planRef, "compound://events/")
	planRecord, err := store.Read(planID)
	if err != nil {
		return err
	}
	plan, err := readExactApprovalPlanRecord(store, planID, planRecord.Bytes)
	if err != nil {
		return err
	}
	if plan.BindingDigest != status.BindingDigest || plan.Binding.FullIntentDigest != CanonicalFullIntentDigest(action) {
		return fmt.Errorf("exact approval plan does not bind action")
	}
	return nil
}
