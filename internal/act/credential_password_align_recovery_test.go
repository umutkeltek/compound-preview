package act

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func (f *alignmentFixture) freshRecovery(t *testing.T, original CredentialPasswordAlignmentPlanResult, result CredentialPasswordAlignmentResult, operation string) {
	t.Helper()
	f.now = time.Now().UTC()
	f.installer.credential.now = f.now
	f.r.Operation, f.r.Origin = "inspect", nil
	f.r.Catalog = policy.CredentialPasswordCatalogPin{DockerSHA256: f.r.Catalog.DockerSHA256}
	f.r.AttemptID = time.Now().UTC().Format("20060102t150405z") + "-" + credentialPasswordAlignHash(time.Now())[:16]
	f.workID = alignmentTestWork(t, f.owner.home, f.now)
	f.inspect(t)
	f.r.Origin = &policy.CredentialPasswordAlignmentOrigin{PlanRef: original.PlanRef, WorkID: original.Plan.WorkID, ActionID: original.Plan.ActionIntentID, AttemptID: result.RunAttemptID, ReceiptID: result.ReceiptID, TransitionID: result.TransitionID, RequestSHA256: original.Plan.RequestSHA256}
	f.r.Operation = operation
	f.r.AttemptID = time.Now().UTC().Format("20060102t150405z") + "-" + credentialPasswordAlignHash(time.Now())[:16]
	f.workID = credentialPasswordAlignRecoveryWorkID(original.Plan.ActionIntentID, result.RunAttemptID)
	s, _ := work.Open(f.owner.home)
	if _, e := s.Get(f.workID); e != nil {
		nativeInstallerWork(t, f.owner.home, "separately approved closed password recovery", f.workID, f.now)
	}
	f.now = time.Now().UTC()
	f.installer.credential.now = f.now
	f.r.AttemptID = f.now.Format("20060102t150405z") + "-" + credentialPasswordAlignHash(time.Now())[:16]
}

func TestCredentialPasswordAlignmentFailedRecoveryRemainsFailedUntilSeparateReconcile(t *testing.T) {
	f := newAlignmentFixture(t)
	f.inspect(t)
	p, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, p)
	f.transport.fail = true
	original, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || original.State != "recovery_required" {
		t.Fatal("missing original uncertainty")
	}
	f.transport.fail = false
	f.freshRecovery(t, p, original, "reconcile")
	wrongWork := alignmentTestWork(t, f.owner.home, f.now)
	if _, e = f.owner.Plan(f.r, wrongWork, "fixture"); e == nil {
		t.Fatal("caller substituted recovery Work")
	}
	recovery, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, recovery)
	f.transport.fail = true
	failed, e := f.owner.Execute(context.Background(), recovery.PlanRef, "fixture")
	if e == nil || failed.State != "recovery_required" {
		t.Fatal("failed recovery lost uncertainty", e)
	}
	f.transport.fail = false
	f.freshRecovery(t, p, original, "restore")
	if _, e = f.owner.Plan(f.r, f.workID, "fixture"); e == nil {
		t.Fatal("restore blindly retried after failed recovery")
	}
	f.r.Operation = "reconcile"
	next, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, next)
	completed, e := f.owner.Execute(context.Background(), next.PlanRef, "fixture")
	if e != nil || completed.State != "completed" {
		t.Fatal("separate reconciliation failed", e)
	}
	accepted, e := credentialPasswordAlignmentInstallerFence(f.owner.home, f.r.File.Path, policy.CredentialVerificationDestination(f.r.CredentialVerificationRequest), time.Now().UTC())
	if e != nil || len(accepted) != 1 || accepted[0].ActionID != next.Plan.ActionIntentID {
		t.Fatal("separate native recovery did not resolve origin group", e)
	}
	for _, id := range []string{original.RunAttemptID, failed.RunAttemptID} {
		a, e := runattempt.Open(f.owner.home).Get(id)
		if e != nil || a.Status != runattempt.StatusFailed {
			t.Fatal("failed attempt was rewritten")
		}
	}
	s, _ := work.Open(f.owner.home)
	if _, _, present, e := s.ReadReceiptedActionReservationInput(p.Plan.WorkID, p.Plan.ActionIntentID); e != nil || present {
		t.Fatal("original blocker not cleared by exact recovery proof", e)
	}
}

func TestCredentialPasswordAlignmentFinalizationResumesWithoutAnotherLaunch(t *testing.T) {
	f := newAlignmentFixture(t)
	p, e := f.owner.Plan(f.r, f.workID, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	f.approve(t, p)
	real := credentialPasswordAlignCompleteAttempt
	credentialPasswordAlignCompleteAttempt = func(string, string, string, string, string, string, time.Time) error {
		return errors.New("synthetic local proof outage")
	}
	t.Cleanup(func() { credentialPasswordAlignCompleteAttempt = real })
	failed, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e == nil || failed.ReceiptID == "" || f.transport.starts != 1 {
		t.Fatal("missing staged native outcome", e)
	}
	credentialPasswordAlignCompleteAttempt = real
	recovered, e := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
	if e != nil || recovered.State != "completed" || f.transport.starts != 1 || recovered.RunAttemptID != failed.RunAttemptID {
		t.Fatal("local finalization replayed remote operation", e)
	}
}

func TestCredentialPasswordAlignmentAdmissionAndStartRaces(t *testing.T) {
	for _, which := range []string{"missing", "expired", "wrong_actor", "cancelled_context", "work", "policy", "halt", "start_halt", "start_fence"} {
		t.Run(which, func(t *testing.T) {
			f := newAlignmentFixture(t)
			if strings.HasPrefix(which, "start_") {
				f.inspect(t)
				f.transport.starts = 0
			}
			p, e := f.owner.Plan(f.r, f.workID, "fixture")
			if e != nil {
				t.Fatal(e)
			}
			if which != "missing" {
				f.approve(t, p)
			}
			actor := "fixture"
			mutateWork := func() {
				s, _ := work.Open(f.owner.home)
				if _, e := s.Advance(f.workID, work.AdvanceInput{To: "running", Actor: "fixture"}, f.now.Add(time.Second)); e != nil {
					t.Fatal(e)
				}
			}
			switch which {
			case "expired":
				f.installer.credential.now = f.now.Add(16 * time.Minute)
			case "wrong_actor":
				actor = "other"
			case "work":
				mutateWork()
			case "halt":
				if e = policy.WriteAutonomyHalt(f.owner.home, policy.AutonomyHalt{Global: true, Reason: "synthetic halt"}); e != nil {
					t.Fatal(e)
				}
			case "policy":
				if e = os.MkdirAll(filepath.Join(f.owner.home, "config"), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(f.owner.home, "config", "policy.yaml"), []byte("forbidden_paths:\n  - '**/.env'\n  - '**/fixture/**'\n"), 0600); e != nil {
					t.Fatal(e)
				}
			case "start_halt":
				f.transport.stage = func() {
					if e := policy.WriteAutonomyHalt(f.owner.home, policy.AutonomyHalt{Global: true, Reason: "synthetic start halt"}); e != nil {
						t.Fatal(e)
					}
				}
			case "start_fence":
				f.transport.stage = func() {
					// A malformed own fence must be rejected before OS start.
					r, _ := runattempt.Open(f.owner.home).List()
					for _, a := range r {
						if a.ActionIntentID == p.Plan.ActionIntentID {
							if e := os.WriteFile(filepath.Join(f.owner.home, "run_attempts", a.ID+".json"), []byte("{}"), 0600); e != nil {
								t.Fatal(e)
							}
						}
					}
				}
			}
			ctx := context.Background()
			if which == "cancelled_context" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			result, e := f.owner.Execute(ctx, p.PlanRef, actor)
			if e == nil || f.transport.starts != 0 || (!strings.HasPrefix(which, "start_") && result.ApprovalConsumed) {
				t.Fatal("changed authority reached OS start", which, e)
			}
		})
	}
}
