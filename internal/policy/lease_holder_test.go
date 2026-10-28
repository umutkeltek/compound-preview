package policy

import (
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestLeaseHolderDenyDecisionIsRecordableDeny(t *testing.T) {
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_lease_test",
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "actor-b",
		Capability:    "work.close",
		Action:        "close_denied_not_lease_holder",
		Target:        envelope.ActionTarget{Kind: "work", ID: "work_x", Path: "work/work/work_x.json"},
		MutationClass: "local_reversible",
		Reversibility: "manual",
		BlastRadius:   "local",
		Preview:       "close_denied_not_lease_holder work work_x",
		RollbackPlan:  "no mutation recorded; work state unchanged",
	}
	pd := LeaseHolderDenyDecision(ai, "work work_x is held by \"actor-a\"; close requires --by actor-a or --steal --reason \"...\"", time.Now())
	if pd.Outcome != "deny" {
		t.Fatalf("lease holder decision must be deny, got %q", pd.Outcome)
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("lease holder decision must validate: %v", err)
	}
	if len(pd.Reasons) == 0 || pd.Reasons[0] == "" {
		t.Fatalf("lease holder decision must record a reason: %+v", pd.Reasons)
	}
	found := false
	for _, ref := range pd.EvidenceRefs {
		if ref == LeaseHolderAuthorityRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("lease holder decision must cite %s in evidence refs: %+v", LeaseHolderAuthorityRef, pd.EvidenceRefs)
	}
}
