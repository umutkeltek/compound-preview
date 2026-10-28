package work

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/runattempt"
)

func privateFailedCompletionFixture(t *testing.T) (privateSourceFixture, ProofResourceReserveResult, ProofResourceCompleteInput, time.Time) {
	t.Helper()
	f, profile := privateNativeConsumerProfile(t)
	at := time.Now().UTC()
	acquired, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{
		Actor: proofActorAlpha, TTL: 20 * time.Minute, Workspace: f.repo,
		Command: "compound proof-check --out /synthetic/failed-proof.json", Profile: profile,
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	steps := make([]map[string]any, 0, len(profile.Gates))
	for _, gate := range profile.Gates {
		steps = append(steps, map[string]any{"name": gate.Name, "command": gate.Command, "dir": gate.Dir, "skip_if_missing": gate.SkipIfMissing, "proof_ref": gate.ProofRef, "status": "failed"})
	}
	report := map[string]any{
		"schema_version": "compound.proof_check_report.v0.5", "profile_kind": ProofResourceProfileCanonicalFull,
		"status": "failed", "generated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"repo": profile.Repo, "base_ref": profile.BaseRef, "source_snapshot": profile.SourceSnapshot,
		"dry_run": false, "proof_refs": []string{}, "steps": steps,
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(acquired.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	return f, acquired, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, ReportArtifactRef: ref, ProducerExitKnown: true, ProducerExitCode: 1}, at
}

func assertFailedPrivateCompletion(t *testing.T, f privateSourceFixture, result ProofResourceCompleteResult) {
	t.Helper()
	if result.Result.Status != ProofResourceFailed || result.Result.RunAttemptStatus != runattempt.StatusFailed ||
		result.Result.ReportStatus != "failed" || result.VerificationRef != "" || !result.ResourceReleased {
		t.Fatalf("completion must preserve terminal failure without verification: %+v", result)
	}
	w, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State == "completed" {
		t.Fatal("failed report completed source Work")
	}
}

func TestFailedPrivateCompletionRetainsFailureAfterReviewExpiry(t *testing.T) {
	f, acquired, input, at := privateFailedCompletionFixture(t)
	after := at.Add(6 * time.Minute)
	if err := f.store.validatePrivateSourceProfileAuthority(f.workID, proofActorAlpha, acquired.Reservation.Profile, after); err == nil {
		t.Fatal("fixture must have genuinely expired current source authority")
	}
	completed, err := f.store.CompleteProofResource(f.workID, input, after)
	if err != nil {
		t.Fatalf("authentic failed result remained held by expired publication review: %v", err)
	}
	assertFailedPrivateCompletion(t, f, completed)
	if err := f.store.validatePrivateSourceProfileAuthority(f.workID, proofActorAlpha, acquired.Reservation.Profile, after); err == nil {
		t.Fatal("recording failure renewed the expired publication review")
	}
	if completed.Result.ReportArtifactRef != input.ReportArtifactRef {
		t.Fatal("original owned report was replaced")
	}
}

func TestFailedPrivateCompletionExactRetrySurvivesReviewExpiry(t *testing.T) {
	f, acquired, input, at := privateFailedCompletionFixture(t)
	original := proofResourceWriteEventRecord
	proofResourceWriteEventRecord = func(s *Store, stage, id string, value any) error {
		if stage == "result" {
			return errors.New("injected failed-result persistence fault")
		}
		return original(s, stage, id, value)
	}
	t.Cleanup(func() { proofResourceWriteEventRecord = original })
	_, firstErr := f.store.CompleteProofResource(f.workID, input, at.Add(time.Second))
	proofResourceWriteEventRecord = original
	if firstErr == nil {
		t.Fatal("fixture must retain a genuine failed completion before commit")
	}
	calls := 0
	resumed, found, err := f.store.ResumeProofResourceCompletion(f.workID, ProofResourceResumeInput{
		Actor: proofActorAlpha, Workspace: acquired.Reservation.Workspace, Command: acquired.Reservation.Command,
		Profile: acquired.Reservation.Profile, PrepareOwnedReport: func(result ProofResourceResult) error {
			calls++
			if result.ReportArtifactRef != input.ReportArtifactRef || result.ReportStatus != "failed" {
				t.Fatal("recovery changed original report")
			}
			return nil
		},
	}, at.Add(6*time.Minute))
	if err != nil || !found {
		t.Fatalf("exact failed recovery remained blocked after expiry: found=%v err=%v", found, err)
	}
	if calls != 1 {
		t.Fatalf("owned report publication barrier calls=%d", calls)
	}
	assertFailedPrivateCompletion(t, f, resumed)
}

func TestPrivateCompletionHistoricalAuthorityCannotCertifyOrHideUnknownOutcome(t *testing.T) {
	f, profile := privateNativeConsumerProfile(t)
	after := time.Now().UTC().Add(6 * time.Minute)
	failure := ProofResourceResult{
		WorkID: f.workID, Actor: proofActorAlpha, Status: ProofResourceFailed,
		ReportStatus: "failed", OwnedReport: true, ReportArtifactRef: "artifact:sha256:synthetic",
		ProducerExitKnown: true, ProducerExitCode: 1, RunAttemptStatus: runattempt.StatusFailed,
	}
	if err := f.store.validatePrivateSourceCompletionAuthority(f.workID, proofActorAlpha, profile, failure, after); err != nil {
		t.Fatalf("historical failure authority fixture: %v", err)
	}
	cases := map[string]func(*ProofResourceResult){
		"verified":           func(v *ProofResourceResult) { v.Status = ProofResourceVerified },
		"report_ok":          func(v *ProofResourceResult) { v.ReportStatus = "ok" },
		"unknown_exit":       func(v *ProofResourceResult) { v.ProducerExitKnown = false },
		"zero_exit":          func(v *ProofResourceResult) { v.ProducerExitCode = 0 },
		"not_owned":          func(v *ProofResourceResult) { v.OwnedReport = false },
		"no_artifact":        func(v *ProofResourceResult) { v.ReportArtifactRef = "" },
		"verification":       func(v *ProofResourceResult) { v.VerificationRef = "proof-check:synthetic" },
		"not_failed_attempt": func(v *ProofResourceResult) { v.RunAttemptStatus = runattempt.StatusRunning },
		"foreign_work":       func(v *ProofResourceResult) { v.WorkID = "work_other" },
		"foreign_actor":      func(v *ProofResourceResult) { v.Actor = "another-actor" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			value := failure
			change(&value)
			if err := f.store.validatePrivateSourceCompletionAuthority(f.workID, proofActorAlpha, profile, value, after); err == nil {
				t.Fatal("expired current authority accepted a certifying, unknown or foreign result")
			}
		})
	}
	drift := copyPrivateConsumerProfile(profile)
	drift.SourceSnapshot.HeadSHA = "ffffffffffffffffffffffffffffffffffffffff"
	if err := f.store.validatePrivateSourceCompletionAuthority(f.workID, proofActorAlpha, drift, failure, after); err == nil {
		t.Fatal("historical failure authority accepted source snapshot drift")
	}
}

func TestExpiredPrivateReviewStillRefusesNewReservation(t *testing.T) {
	f, profile := privateNativeConsumerProfile(t)
	_, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{
		Actor: proofActorAlpha, TTL: time.Minute, Workspace: f.repo,
		Command: "compound proof-check --out /synthetic/new-proof.json", Profile: profile,
	}, time.Now().UTC().Add(6*time.Minute))
	if err == nil {
		t.Fatal("expired original review authorized a new proof reservation")
	}
}
