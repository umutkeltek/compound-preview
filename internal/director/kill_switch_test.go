package director

import (
	"testing"
	"time"

	"compound/internal/policy"
	"compound/internal/work"
)

// TestKillSwitchHaltsDirectorAutoAdmit wires the halt to the real typed
// job_health path: even a threshold-met class grant stays proposed while the
// halt is engaged, then a fresh evaluation at the clearing instant reconsiders
// and admits the same proposed WorkEnvelope.
func TestKillSwitchHaltsDirectorAutoAdmit(t *testing.T) {
	home := seededDirectorHome(t)
	now := directorTestNow.Add(96 * time.Hour)
	seedDirectorApplyGrant(t, home, now.Add(-72*time.Hour))
	seedJobHealthAutonomyGrant(t, home, "job_health", 2, 1, 0, now.Add(-72*time.Hour))
	seedJobHealthAutonomySample(t, home, "halt-a", now.Add(-48*time.Hour), "completed", "auto/judge", true)
	seedJobHealthAutonomySample(t, home, "halt-b", now.Add(-24*time.Hour), "completed", "auto/judge", true)
	seedFailingJobHealth(t, home)
	if err := policy.WriteAutonomyHalt(home, policy.AutonomyHalt{Global: true, Reason: "incident review"}); err != nil {
		t.Fatalf("engage halt: %v", err)
	}

	report, err := Propose(home, Options{Apply: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	proposal := findProposalByKind(report, "job_health")
	if proposal == nil || proposal.AutoAdmit == nil || proposal.AutoAdmitResult == nil {
		t.Fatalf("expected one typed job_health auto-admit attempt, got %+v", report)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if !proposal.AutoAdmitResult.Halted || proposal.AutoAdmitResult.Admitted {
		t.Fatalf("global halt must deny director auto-admit: %+v", proposal)
	}
	if got, _ := store.Get(proposal.WorkID); got.State != "proposed" {
		t.Fatalf("halted director work must stay proposed, got %q", got.State)
	}

	// Halt cleared: Director recompiles eligibility at the new instant and
	// reconsiders the same proposed WorkEnvelope.
	if err := policy.WriteAutonomyHalt(home, policy.AutonomyHalt{}); err != nil {
		t.Fatalf("clear halt: %v", err)
	}
	reconsidered, err := Propose(home, Options{Apply: true, Now: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	admitted := findProposalByKind(reconsidered, "job_health")
	if admitted == nil || admitted.WorkID != proposal.WorkID || admitted.AutoAdmitResult == nil || !admitted.AutoAdmitResult.Admitted {
		t.Fatalf("cleared halt must let director auto-admit proceed on the same work: first=%+v second=%+v report=%+v", proposal, admitted, reconsidered)
	}
	if got, _ := store.Get(proposal.WorkID); got.State != "admitted" {
		t.Fatalf("director work should be admitted after clearing halt, got %q", got.State)
	}
}
