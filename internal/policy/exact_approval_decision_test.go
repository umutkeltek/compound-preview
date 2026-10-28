package policy

import (
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
)

func TestValidateConsumedDecisionRequiresExactDurableIssuedDecision(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	req := monitorRequest("browser.submit", map[string]any{"target": "publish", "form": "prepared-draft"})
	first := MonitorToolAction(req, cfg, fixedNow)
	persistActionIntentForTest(t, homePath, first.Intent)
	approvals := NewExactActionApprovalStore(homePath)
	plan, _, _, err := approvals.EnsurePlanForIntent(first.Intent, cfg, 5*time.Minute, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := approvals.Approve(plan.ID, "operator:fixture", cfg, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	retry := MonitorToolAction(req, cfg, fixedNow.Add(2*time.Second))
	decision, matched, err := approvals.ApplyMatching(retry.Intent, cfg, retry.Decision, fixedNow.Add(2*time.Second))
	if err != nil || !matched {
		t.Fatalf("consume exact approval: matched=%t err=%v", matched, err)
	}
	if err := approvals.ValidateConsumedDecision(retry.Intent, decision); err == nil {
		t.Fatal("consumed grant without durable issued decision accepted")
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(decision.ID, decision, "  "); err != nil {
		t.Fatal(err)
	}
	if err := approvals.ValidateConsumedDecision(retry.Intent, decision); err != nil {
		t.Fatalf("valid issued decision rejected: %v", err)
	}
	forged := decision
	forged.Obligations = append(append([]string{}, decision.Obligations...), "forged obligation")
	if err := approvals.ValidateConsumedDecision(retry.Intent, forged); err == nil {
		t.Fatal("forged obligations accepted with consumed grant")
	}
	foreign := retry.Intent
	foreign.ArgumentsHash = "sha256:foreign"
	if err := approvals.ValidateConsumedDecision(foreign, decision); err == nil {
		t.Fatal("foreign action accepted with consumed grant")
	}
}
