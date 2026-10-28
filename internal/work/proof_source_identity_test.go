package work

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/runattempt"
)

func TestReserveProofResourceRejectsWorkRepositoryFingerprintMismatchBeforeMutation(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	repoA := initProofSourceRepo(t, "https://example.invalid/compound/repo-a.git")
	repoB := initProofSourceRepo(t, "https://example.invalid/compound/repo-b.git")
	sourceA, err := gitrepository.CaptureCleanSnapshot(repoA, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	sourceB, err := gitrepository.CaptureCleanSnapshot(repoB, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 4, 30, 0, 0, time.UTC)
	item, err := store.Create(CreateInput{
		Owner:            "proof-source-test",
		Actor:            proofActorAlpha,
		SuccessPredicate: "proof belongs to the WorkEnvelope repository",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot:    repoA,
			RepositoryFingerprint: sourceA.RepositoryFingerprint,
			TargetLocalRef:        "refs/heads/main",
			ExpectedBaseSHA:       sourceA.BaseSHA,
			PublicationMode:       "local_only",
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err = store.Claim(item.ID, proofActorAlpha, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewProofResourceProfileWithSource(
		ProofResourceProfileCanonicalFull,
		repoB,
		sourceB,
		[]ProofResourceGateProfile{{
			Name:     "synthetic-gate",
			Command:  []string{"go", "test", "./internal/synthetic"},
			ProofRef: "synthetic-proof",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.ReserveProofResource(item.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: repoB,
		Command:   "compound proof-check --base refs/heads/main --out /synthetic/proof.json",
		Profile:   profile,
	}, now.Add(2*time.Second))
	if err == nil || !strings.Contains(err.Error(), "repository fingerprint") {
		t.Fatalf("mismatched repository reserve error = %v", err)
	}
	current, getErr := store.Get(item.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if len(current.RunAttemptIDs) != len(item.RunAttemptIDs) ||
		len(current.ActionIntentIDs) != len(item.ActionIntentIDs) {
		t.Fatalf("fingerprint mismatch mutated work: before=%+v after=%+v", item, current)
	}
	status, statusErr := landlease.Status(store.home, now.Add(2*time.Second))
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if status.Held {
		t.Fatalf("fingerprint mismatch acquired proof lease: %+v", status)
	}
}

func TestCompleteProofResourceStoresSealedSourceSnapshot(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	repo := initProofSourceRepo(t, "https://example.invalid/compound/repo-proof.git")
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 5, 0, 0, 0, time.UTC)
	item, err := store.Create(CreateInput{
		Owner:            "proof-source-test",
		Actor:            proofActorAlpha,
		SuccessPredicate: "sealed proof source remains reachable",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot:    repo,
			RepositoryFingerprint: source.RepositoryFingerprint,
			TargetLocalRef:        "refs/heads/main",
			ExpectedBaseSHA:       source.BaseSHA,
			PublicationMode:       "local_only",
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err = store.Claim(item.ID, proofActorAlpha, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	gates := []ProofResourceGateProfile{{
		Name:     "synthetic-gate",
		Command:  []string{"go", "test", "./internal/synthetic"},
		ProofRef: "synthetic-proof",
	}}
	profile, err := NewProofResourceProfileWithSource(
		ProofResourceProfileCanonicalFull,
		repo,
		source,
		gates,
	)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ReserveProofResource(item.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: repo,
		Command:   "compound proof-check --base refs/heads/main --out /synthetic/proof.json",
		Profile:   profile,
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	reportBody, err := json.Marshal(map[string]any{
		"schema_version":  "compound.proof_check_report.v0.3",
		"profile_kind":    ProofResourceProfileCanonicalFull,
		"status":          "ok",
		"generated_at":    now.Add(3 * time.Second).Format(time.RFC3339),
		"repo":            repo,
		"base_ref":        source.BaseSHA,
		"source_snapshot": source,
		"dry_run":         false,
		"proof_refs":      []string{"synthetic-proof"},
		"steps": []map[string]any{{
			"name":      "synthetic-gate",
			"command":   []string{"go", "test", "./internal/synthetic"},
			"proof_ref": "synthetic-proof",
			"status":    "ok",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(store.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(reportBody)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteProofResource(item.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     reserved.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if completed.Result.SourceSnapshot == nil ||
		*completed.Result.SourceSnapshot != source {
		t.Fatalf("completed proof source = %+v, want %+v", completed.Result.SourceSnapshot, source)
	}
	item, err = store.Advance(item.ID, AdvanceInput{
		To:         item.State,
		Actor:      proofActorAlpha,
		ArtifactID: "commit:" + source.HeadSHA,
	}, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	matching, err := store.LandCheck(item.ID, LandCheckOptions{
		CandidateTipSHA: source.HeadSHA,
		Actor:           proofActorAlpha,
		Record:          true,
	}, now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if matching.Status != "landed" ||
		matching.ProofVerificationRef != completed.VerificationRef ||
		matching.ProofHeadSHA != source.HeadSHA {
		t.Fatalf("matching proof land-check = %+v", matching)
	}
	current, err := store.Get(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.recordedLandedProofForClose(current, []string{matching.EvidenceRef}); err != nil {
		t.Fatalf("committed proof finalizer chain did not satisfy close: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repo, "replayed.txt"), []byte("replayed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runProofSourceGit(t, repo, "add", "replayed.txt")
	runProofSourceGit(t, repo, "commit", "-m", "replayed candidate")
	replayedTip := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
	item, err = store.Advance(item.ID, AdvanceInput{
		To:         item.State,
		Actor:      proofActorAlpha,
		ArtifactID: "commit:" + replayedTip,
	}, now.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	mismatched, err := store.LandCheck(item.ID, LandCheckOptions{
		CandidateTipSHA: replayedTip,
		Actor:           proofActorAlpha,
	}, now.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if mismatched.Status != "proof_candidate_tip_mismatch" ||
		mismatched.ProofHeadSHA != source.HeadSHA {
		t.Fatalf("replayed candidate reused stale proof: %+v", mismatched)
	}
}

func TestLandCheckRefusesSourceControlledCandidateWithoutCommittedProof(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 28, 7, 30, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingLandedRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "source-controlled candidate requires certifying proof",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, item.ID, now)
	attachLandingCommit(t, store, item.ID, candidate, now.Add(3*time.Minute))

	report, err := store.LandCheck(item.ID, LandCheckOptions{
		CandidateTipSHA: candidate,
		Actor:           "codex",
		Record:          true,
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "missing_source_bound_proof" {
		t.Fatalf("land-check without committed source proof status = %q, want missing_source_bound_proof: %+v", report.Status, report)
	}
	if !strings.Contains(report.NextAction, "fresh canonical certifying proof") {
		t.Fatalf("land-check without committed source proof next action = %q", report.NextAction)
	}
	current, err := store.Get(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if contains(current.EvidenceRefs, report.EvidenceRef) {
		t.Fatalf("non-landed proof check attached as delivery evidence: %+v", current.EvidenceRefs)
	}
}

func TestLandCheckRequiresSourceBoundProofBeforeFastForwardAdvice(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingReadyRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "proof admission precedes landing advice",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, item.ID, now)
	attachLandingCommit(t, store, item.ID, candidate, now.Add(3*time.Minute))

	report, err := store.LandCheck(item.ID, LandCheckOptions{
		CandidateTipSHA: candidate,
		Actor:           "codex",
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "missing_source_bound_proof" {
		t.Fatalf("unproved ready candidate status = %q, want missing_source_bound_proof: %+v", report.Status, report)
	}
	if !strings.Contains(report.NextAction, "fresh canonical certifying proof") ||
		strings.Contains(report.NextAction, "fast-forward") {
		t.Fatalf("unproved ready candidate received landing advice: %q", report.NextAction)
	}
}

func TestRecordedLandedReportWithoutSourceProofCannotClose(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 28, 7, 40, 0, 0, time.UTC)
	repo, base, candidate := sharedLandingLandedRepo(t)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Create(CreateInput{
		Owner:            "codex",
		SuccessPredicate: "legacy landed report cannot bypass source proof",
		GitDelivery:      localGitDelivery(repo, base),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	activateLandingWork(t, store, item.ID, now)
	attachLandingCommit(t, store, item.ID, candidate, now.Add(3*time.Minute))
	current, err := store.Get(item.ID)
	if err != nil {
		t.Fatal(err)
	}

	legacy, err := store.evaluateLandCheck(current, LandCheckOptions{
		CandidateTipSHA: candidate,
		Actor:           "codex",
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	legacy.Status = "landed"
	legacy.NextAction = ""
	legacy.CandidateExists = true
	legacy.ExpectedBaseExists = true
	legacy.ExpectedBaseAncestorOfCandidate = true
	legacy.CandidateContainedInTarget = true
	legacy.TargetAncestorOfCandidate = true
	legacy.CandidateContainedInPublication = true
	legacy.ProofVerificationRef = ""
	legacy.ProofHeadSHA = ""
	if err := sealLandCheckReport(&legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.recordLandCheck(&legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(item.ID, AdvanceInput{
		To:           current.State,
		Actor:        "codex",
		EvidenceRefs: []string{legacy.EvidenceRef},
	}, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	plan, err := store.ClosePlan(item.ID, now.Add(5*time.Minute+30*time.Second), ClosePlanOptions{
		Actor:    "codex",
		Outcome:  "completed",
		MoveType: "outward_delta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "not_closeable" || !strings.Contains(plan.NoAction, "fresh canonical certifying proof") {
		t.Fatalf("close plan with landed report lacking source proof = %+v", plan)
	}

	_, err = store.Close(item.ID, CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     "codex",
		Summary:   "legacy delivery report",
		ProofRefs: []string{legacy.EvidenceRef},
	}, now.Add(6*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "fresh canonical certifying proof") {
		t.Fatalf("close with landed report lacking source proof err = %v", err)
	}
}

func TestSHA256SourceBoundProofAndLandBinding(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	repo := initSHA256ProofSourceRepo(t)
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(source.BaseSHA) != 64 || len(source.HeadSHA) != 64 || len(source.TreeSHA) != 64 {
		t.Fatalf("sha256 source snapshot truncated object ids: %+v", source)
	}
	now := time.Date(2026, 7, 28, 5, 20, 0, 0, time.UTC)
	item, err := store.Create(CreateInput{
		Owner:            "proof-source-sha256-test",
		Actor:            proofActorAlpha,
		SuccessPredicate: "sha256 proof source remains bound through land check",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot:    repo,
			RepositoryFingerprint: source.RepositoryFingerprint,
			TargetLocalRef:        "refs/heads/main",
			ExpectedBaseSHA:       source.BaseSHA,
			PublicationMode:       "local_only",
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err = store.Claim(item.ID, proofActorAlpha, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	gates := []ProofResourceGateProfile{{
		Name:     "synthetic-gate",
		Command:  []string{"go", "test", "./internal/synthetic"},
		ProofRef: "synthetic-proof",
	}}
	profile, err := NewProofResourceProfileWithSource(
		ProofResourceProfileCanonicalFull,
		repo,
		source,
		gates,
	)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ReserveProofResource(item.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: repo,
		Command:   "compound proof-check --base refs/heads/main --out /synthetic/proof.json",
		Profile:   profile,
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reserved.ExecutionBarrier == nil {
		t.Fatal("sha256 proof reservation omitted execution barrier")
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	reportBody, err := json.Marshal(map[string]any{
		"schema_version":  "compound.proof_check_report.v0.3",
		"profile_kind":    ProofResourceProfileCanonicalFull,
		"status":          "ok",
		"generated_at":    now.Add(3 * time.Second).Format(time.RFC3339),
		"repo":            repo,
		"base_ref":        source.BaseSHA,
		"source_snapshot": source,
		"dry_run":         false,
		"proof_refs":      []string{"synthetic-proof"},
		"steps": []map[string]any{{
			"name":      "synthetic-gate",
			"command":   []string{"go", "test", "./internal/synthetic"},
			"proof_ref": "synthetic-proof",
			"status":    "ok",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(store.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(reportBody)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteProofResource(item.ID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     reserved.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if completed.Result.SourceSnapshot == nil ||
		*completed.Result.SourceSnapshot != source ||
		len(completed.Result.SourceSnapshot.HeadSHA) != 64 {
		t.Fatalf("sha256 proof completion source mismatch: %+v", completed.Result.SourceSnapshot)
	}
	item, err = store.Advance(item.ID, AdvanceInput{
		To:         item.State,
		Actor:      proofActorAlpha,
		ArtifactID: "commit:" + source.HeadSHA,
	}, now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	landed, err := store.LandCheck(item.ID, LandCheckOptions{
		CandidateTipSHA: source.HeadSHA,
		Actor:           proofActorAlpha,
		Record:          true,
	}, now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if landed.Status != "landed" ||
		landed.CandidateTipSHA != source.HeadSHA ||
		landed.ProofHeadSHA != source.HeadSHA ||
		landed.ProofVerificationRef != completed.VerificationRef {
		t.Fatalf("sha256 source-bound land check = %+v", landed)
	}
}

func TestLandCheckRejectsPreparedProofBeforeFinalizerCommit(t *testing.T) {
	fixture := newSourceBoundProofCompletionFixture(t, time.Date(2026, 7, 28, 5, 20, 0, 0, time.UTC))
	originalTerminalize := proofResourceTerminalizeRunAttempt
	proofResourceTerminalizeRunAttempt = func(
		_ *Store,
		_, _, _ string,
		_ time.Time,
	) (runattempt.RunAttempt, error) {
		return runattempt.RunAttempt{}, errors.New("synthetic crash before terminal RunAttempt")
	}
	t.Cleanup(func() {
		proofResourceTerminalizeRunAttempt = originalTerminalize
	})

	_, err := fixture.store.CompleteProofResource(
		fixture.item.ID,
		ProofResourceCompleteInput{
			Actor:             proofActorAlpha,
			ReservationID:     fixture.reserved.Reservation.ID,
			ReportArtifactRef: fixture.reportRef,
			ProducerExitKnown: true,
			ProducerExitCode:  0,
		},
		fixture.now.Add(4*time.Second),
	)
	if err == nil || !strings.Contains(err.Error(), "synthetic crash") {
		t.Fatalf("prepared completion err = %v, want crash before finalizer commitment", err)
	}
	pending, present, _, err := fixture.store.readProofResourceCompletionPending(fixture.reserved.Reservation.ID)
	if err != nil || !present {
		t.Fatalf("prepared completion journal: present=%t err=%v", present, err)
	}
	var preparedResult ProofResourceResult
	if err := fixture.store.readEventJSON(pending.Result.ID, &preparedResult); err != nil {
		t.Fatalf("prepared result not persisted: %v", err)
	}
	attempt, err := fixture.store.runAttempts().Get(pending.Result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status == runattempt.StatusSucceeded {
		t.Fatalf("crash-window RunAttempt unexpectedly succeeded: %+v", attempt)
	}

	current, err := fixture.store.Get(fixture.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err = fixture.store.Advance(fixture.item.ID, AdvanceInput{
		To:             current.State,
		Actor:          proofActorAlpha,
		ArtifactID:     "commit:" + fixture.source.HeadSHA,
		VerificationID: pending.VerificationRef,
	}, fixture.now.Add(5*time.Second))
	if err != nil {
		t.Fatalf("attach prepared proof ref: %v", err)
	}
	if !oneOf(pending.VerificationRef, current.VerificationIDs) {
		t.Fatal("prepared proof ref was not attached for crash-window falsifier")
	}

	_, err = fixture.store.LandCheck(fixture.item.ID, LandCheckOptions{
		CandidateTipSHA: fixture.source.HeadSHA,
		Actor:           proofActorAlpha,
	}, fixture.now.Add(6*time.Second))
	if err == nil || !strings.Contains(err.Error(), "proof finalizer chain") {
		t.Fatalf("land-check accepted prepared-only proof: %v", err)
	}
}

func TestLandCheckAcceptsFinalizerCommittedRetryChain(t *testing.T) {
	fixture := newSourceBoundProofCompletionFixture(t, time.Date(2026, 7, 28, 5, 40, 0, 0, time.UTC))
	originalTerminalize := proofResourceTerminalizeRunAttempt
	proofResourceTerminalizeRunAttempt = func(
		_ *Store,
		_, _, _ string,
		_ time.Time,
	) (runattempt.RunAttempt, error) {
		return runattempt.RunAttempt{}, errors.New("synthetic first-attempt finalizer fault")
	}
	t.Cleanup(func() {
		proofResourceTerminalizeRunAttempt = originalTerminalize
	})
	input := ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     fixture.reserved.Reservation.ID,
		ReportArtifactRef: fixture.reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}
	if _, err := fixture.store.CompleteProofResource(
		fixture.item.ID,
		input,
		fixture.now.Add(4*time.Second),
	); err == nil || !strings.Contains(err.Error(), "synthetic first-attempt") {
		t.Fatalf("first completion err = %v, want injected finalizer fault", err)
	}
	proofResourceTerminalizeRunAttempt = originalTerminalize

	completed, err := fixture.store.CompleteProofResource(
		fixture.item.ID,
		input,
		fixture.now.Add(5*time.Second),
	)
	if err != nil {
		t.Fatalf("retry completion: %v", err)
	}
	if completed.Result.RunAttemptID == fixture.reserved.Reservation.RunAttemptID {
		t.Fatalf("completion did not mint retry RunAttempt: %+v", completed.Result)
	}
	if !completed.ResourceReleased || completed.VerificationRef == "" {
		t.Fatalf("retry completion did not commit and release: %+v", completed)
	}
	current, err := fixture.store.Get(fixture.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Advance(fixture.item.ID, AdvanceInput{
		To:         current.State,
		Actor:      proofActorAlpha,
		ArtifactID: "commit:" + fixture.source.HeadSHA,
	}, fixture.now.Add(5*time.Second+time.Millisecond)); err != nil {
		t.Fatalf("attach retry candidate tip: %v", err)
	}

	checked, err := fixture.store.LandCheck(fixture.item.ID, LandCheckOptions{
		CandidateTipSHA: fixture.source.HeadSHA,
		Actor:           proofActorAlpha,
	}, fixture.now.Add(6*time.Second))
	if err != nil {
		t.Fatalf("finalizer-committed retry proof rejected: %v", err)
	}
	if checked.ProofVerificationRef != completed.VerificationRef ||
		checked.ProofHeadSHA != fixture.source.HeadSHA {
		t.Fatalf("land-check retry chain = %+v, want verification=%s head=%s", checked, completed.VerificationRef, fixture.source.HeadSHA)
	}
}

func TestLandCheckAcceptsExactProofReservationGovernanceVariants(t *testing.T) {
	t.Parallel()
	completeAndAttachCandidate := func(
		t *testing.T,
		fixture sourceBoundProofCompletionFixture,
		variant string,
	) ProofResourceCompleteResult {
		t.Helper()
		completed, err := fixture.store.CompleteProofResource(
			fixture.item.ID,
			ProofResourceCompleteInput{
				Actor:             proofActorBeta,
				ReservationID:     fixture.reserved.Reservation.ID,
				ReportArtifactRef: fixture.reportRef,
				ProducerExitKnown: true,
				ProducerExitCode:  0,
			},
			fixture.now.Add(8*time.Second),
		)
		if err != nil {
			t.Fatalf("complete %s proof: %v", variant, err)
		}
		current, err := fixture.store.Get(fixture.item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.Advance(fixture.item.ID, AdvanceInput{
			To:         current.State,
			Actor:      proofActorBeta,
			ArtifactID: "commit:" + fixture.source.HeadSHA,
		}, fixture.now.Add(9*time.Second)); err != nil {
			t.Fatalf("attach %s candidate: %v", variant, err)
		}
		return completed
	}

	for _, variant := range []string{
		"reserve",
		"ordinary steal",
		"termination-evidenced steal",
	} {
		t.Run(variant, func(t *testing.T) {
			fixture := newSourceBoundTakeoverProofFixture(
				t,
				time.Date(2026, 8, 4, 3, 0, 0, 0, time.UTC),
				variant,
			)
			completed := completeAndAttachCandidate(t, fixture, variant)

			checked, err := fixture.store.LandCheck(fixture.item.ID, LandCheckOptions{
				CandidateTipSHA: fixture.source.HeadSHA,
				Actor:           proofActorBeta,
			}, fixture.now.Add(10*time.Second))
			if err != nil {
				t.Fatalf("LandCheck rejected %s reservation governance: %v", variant, err)
			}
			if checked.Status != "landed" ||
				checked.ProofVerificationRef != completed.VerificationRef ||
				checked.ProofHeadSHA != fixture.source.HeadSHA {
				t.Fatalf("%s LandCheck = %+v, want committed proof %s", variant, checked, completed.VerificationRef)
			}
		})
	}

	t.Run("checksum-valid wrong reservation action is rejected", func(t *testing.T) {
		fixture := newSourceBoundTakeoverProofFixture(
			t,
			time.Date(2026, 8, 4, 3, 30, 0, 0, time.UTC),
			"termination-evidenced steal",
		)
		var action envelope.ActionIntent
		if err := fixture.store.readEventJSON(fixture.reserved.Reservation.ActionIntentID, &action); err != nil {
			t.Fatal(err)
		}
		action.Action = proofResourceActionReserve
		if err := fixture.store.replaceEvent(action.ID, &action); err != nil {
			t.Fatal(err)
		}
		completeAndAttachCandidate(t, fixture, "termination-evidenced steal")

		_, err := fixture.store.LandCheck(fixture.item.ID, LandCheckOptions{
			CandidateTipSHA: fixture.source.HeadSHA,
			Actor:           proofActorBeta,
		}, fixture.now.Add(10*time.Second))
		if err == nil ||
			!strings.Contains(err.Error(), "reservation governance") ||
			!strings.Contains(err.Error(), "identity does not bind proof work") {
			t.Fatalf("LandCheck wrong reservation action error = %v", err)
		}
	})

	t.Run("partial termination evidence is rejected", func(t *testing.T) {
		priorRun := "ratt_20260804T030000Z_12345678"
		reservation := ProofResourceReservation{
			ID:                        "pfr_tampered_takeover",
			PriorHolder:               proofResourceHolder("pfr_prior", priorRun, "work_prior", proofActorAlpha),
			PriorRunAttemptID:         priorRun,
			StealReason:               "tampered takeover",
			StolenAt:                  "2026-08-04T03:00:00Z",
			TerminatedRunAttemptID:    priorRun,
			TerminationConfirmedAt:    "2026-08-04T03:00:00Z",
			TerminationConfirmationID: "pfrt_tampered",
		}
		if _, err := proofReservationExpectedAction(reservation); err == nil ||
			!strings.Contains(err.Error(), "termination-evidenced") {
			t.Fatalf("partial termination-evidenced reservation action error = %v", err)
		}
	})
}

func newSourceBoundTakeoverProofFixture(
	t *testing.T,
	now time.Time,
	variant string,
) sourceBoundProofCompletionFixture {
	t.Helper()
	store := newStore(t)
	repo := initProofSourceRepo(t, "https://example.invalid/compound/proof-takeover-finalizer.git")
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	var predecessor envelope.WorkEnvelope
	var predecessorReservation ProofResourceReserveResult
	if variant != "reserve" {
		writeProofResourceStealGrant(t, store.home)
		predecessor = claimedProofWork(t, store, "takeover predecessor remains auditable", proofActorAlpha, now)
		predecessorProfile, err := NewProofResourceProfile(
			ProofResourceProfileCanonicalFull,
			"/synthetic/proof-takeover-predecessor",
			"",
			[]ProofResourceGateProfile{{
				Name:     "synthetic-predecessor-gate",
				Command:  []string{"go", "test", "./internal/synthetic-predecessor"},
				ProofRef: "synthetic-predecessor-proof",
			}},
		)
		if err != nil {
			t.Fatal(err)
		}
		predecessorReservation, err = store.ReserveProofResource(predecessor.ID, ProofResourceReserveInput{
			Actor:     proofActorAlpha,
			TTL:       time.Minute,
			Workspace: "/synthetic/proof-takeover-predecessor",
			Command:   "compound proof-check --out /synthetic/predecessor.json",
			Profile:   predecessorProfile,
		}, now.Add(2*time.Second))
		if err != nil {
			t.Fatalf("reserve %s predecessor: %v", variant, err)
		}
		if err := closeProofFixtureExecutionBarrier(predecessorReservation.ExecutionBarrier); err != nil {
			t.Fatal(err)
		}
	}

	item, err := store.Create(CreateInput{
		Owner:            "proof-takeover-finalizer-test",
		Actor:            proofActorBeta,
		SuccessPredicate: "LandCheck accepts the exact persisted reservation governance variant",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot:    repo,
			RepositoryFingerprint: source.RepositoryFingerprint,
			TargetLocalRef:        "refs/heads/main",
			ExpectedBaseSHA:       source.BaseSHA,
			PublicationMode:       "local_only",
		},
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	item, err = store.Claim(item.ID, proofActorBeta, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewProofResourceProfileWithSource(
		ProofResourceProfileCanonicalFull,
		repo,
		source,
		[]ProofResourceGateProfile{{
			Name:     "synthetic-gate",
			Command:  []string{"go", "test", "./internal/synthetic"},
			ProofRef: "synthetic-proof",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	reserveInput := ProofResourceReserveInput{
		Actor:     proofActorBeta,
		TTL:       time.Minute,
		Workspace: repo,
		Command:   "compound proof-check --base " + source.BaseSHA + " --out /synthetic/takeover-finalizer.json",
		Profile:   profile,
	}
	if variant != "reserve" {
		reserveInput.Steal = true
		reserveInput.StealReason = "governed takeover finalizer regression"
	}
	if variant == "termination-evidenced steal" {
		reserveInput.TerminatedRunAttemptID = predecessorReservation.Reservation.RunAttemptID
	}
	reserve := func() (ProofResourceReserveResult, error) {
		return store.ReserveProofResource(item.ID, reserveInput, now.Add(5*time.Second))
	}
	if variant == "ordinary steal" {
		reserveWithoutObserver := reserve
		reserve = func() (ProofResourceReserveResult, error) {
			return reserveProofWithTakeoverObserver(reserveWithoutObserver, func() (bool, error) {
				status, statusErr := landlease.Status(store.home, now.Add(5*time.Second))
				if statusErr != nil {
					return false, statusErr
				}
				if status.Holder == predecessorReservation.Reservation.Holder {
					return false, nil
				}
				_, predecessorErr := store.CompleteProofResource(predecessor.ID, ProofResourceCompleteInput{
					Actor:         proofActorAlpha,
					ReservationID: predecessorReservation.Reservation.ID,
					FailureReason: "predecessor observed governed lease transfer",
				}, now.Add(6*time.Second))
				if predecessorErr == nil {
					return false, errors.New("stolen predecessor completion unexpectedly succeeded")
				}
				attempt, attemptErr := store.runAttempts().Get(predecessorReservation.Reservation.RunAttemptID)
				if attemptErr != nil {
					return false, attemptErr
				}
				if !runattempt.IsTerminal(attempt.Status) {
					return false, errors.New("stolen predecessor remained nonterminal")
				}
				return true, nil
			})
		}
	}
	reserved, err := reserve()
	if err != nil {
		t.Fatalf("reserve %s proof: %v", variant, err)
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}

	reportBody, err := json.Marshal(map[string]any{
		"schema_version":  "compound.proof_check_report.v0.3",
		"profile_kind":    ProofResourceProfileCanonicalFull,
		"status":          "ok",
		"generated_at":    now.Add(7 * time.Second).Format(time.RFC3339),
		"repo":            repo,
		"base_ref":        source.BaseSHA,
		"source_snapshot": source,
		"dry_run":         false,
		"proof_refs":      []string{"synthetic-proof"},
		"steps": []map[string]any{{
			"name":      "synthetic-gate",
			"command":   []string{"go", "test", "./internal/synthetic"},
			"proof_ref": "synthetic-proof",
			"status":    "ok",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(store.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(reportBody)
	if err != nil {
		t.Fatal(err)
	}
	return sourceBoundProofCompletionFixture{
		store:     store,
		item:      item,
		repo:      repo,
		source:    source,
		reserved:  reserved,
		reportRef: reportRef,
		now:       now,
	}
}

type sourceBoundProofCompletionFixture struct {
	store     *Store
	item      envelope.WorkEnvelope
	repo      string
	source    gitrepository.Snapshot
	reserved  ProofResourceReserveResult
	reportRef string
	now       time.Time
}

func newSourceBoundProofCompletionFixture(t *testing.T, now time.Time) sourceBoundProofCompletionFixture {
	t.Helper()
	store := newStore(t)
	repo := initProofSourceRepo(t, "https://example.invalid/compound/repo-finalizer.git")
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Create(CreateInput{
		Owner:            "proof-finalizer-test",
		Actor:            proofActorAlpha,
		SuccessPredicate: "only finalizer-committed proof may land",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot:    repo,
			RepositoryFingerprint: source.RepositoryFingerprint,
			TargetLocalRef:        "refs/heads/main",
			ExpectedBaseSHA:       source.BaseSHA,
			PublicationMode:       "local_only",
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err = store.Claim(item.ID, proofActorAlpha, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewProofResourceProfileWithSource(
		ProofResourceProfileCanonicalFull,
		repo,
		source,
		[]ProofResourceGateProfile{{
			Name:     "synthetic-gate",
			Command:  []string{"go", "test", "./internal/synthetic"},
			ProofRef: "synthetic-proof",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ReserveProofResource(item.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       time.Minute,
		Workspace: repo,
		Command:   "compound proof-check --base refs/heads/main --out /synthetic/finalizer.json",
		Profile:   profile,
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	reportBody, err := json.Marshal(map[string]any{
		"schema_version":  "compound.proof_check_report.v0.3",
		"profile_kind":    ProofResourceProfileCanonicalFull,
		"status":          "ok",
		"generated_at":    now.Add(3 * time.Second).Format(time.RFC3339),
		"repo":            repo,
		"base_ref":        source.BaseSHA,
		"source_snapshot": source,
		"dry_run":         false,
		"proof_refs":      []string{"synthetic-proof"},
		"steps": []map[string]any{{
			"name":      "synthetic-gate",
			"command":   []string{"go", "test", "./internal/synthetic"},
			"proof_ref": "synthetic-proof",
			"status":    "ok",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(store.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(reportBody)
	if err != nil {
		t.Fatal(err)
	}
	return sourceBoundProofCompletionFixture{
		store:     store,
		item:      item,
		repo:      repo,
		source:    source,
		reserved:  reserved,
		reportRef: reportRef,
		now:       now,
	}
}

func completeSourceBoundProofForDetachedCandidate(
	t *testing.T,
	store *Store,
	workID string,
	actor string,
	repo string,
	candidate string,
	now time.Time,
) {
	t.Helper()
	proofWorktree := filepath.Join(t.TempDir(), "proof-worktree")
	landingGit(t, repo, "worktree", "add", "--detach", proofWorktree, candidate)
	completeSourceBoundProofForWork(t, store, workID, actor, proofWorktree, now)
	landingGit(t, repo, "worktree", "remove", proofWorktree)
}

func completeSourceBoundProofForWork(
	t *testing.T,
	store *Store,
	workID string,
	actor string,
	repo string,
	now time.Time,
) ProofResourceCompleteResult {
	t.Helper()
	item, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if item.GitDelivery == nil {
		t.Fatalf("work %s has no git delivery contract", workID)
	}
	if item.State == "ready" {
		item, err = store.Claim(workID, actor, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	runProofSourceGit(t, repo, "update-index", "--refresh")
	source, err := gitrepository.CaptureCleanSnapshot(repo, item.GitDelivery.ExpectedBaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewProofResourceProfileWithSource(
		ProofResourceProfileCanonicalFull,
		repo,
		source,
		[]ProofResourceGateProfile{{
			Name:     "synthetic-gate",
			Command:  []string{"go", "test", "./internal/synthetic"},
			ProofRef: "synthetic-proof",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ReserveProofResource(workID, ProofResourceReserveInput{
		Actor:     actor,
		TTL:       time.Minute,
		Workspace: repo,
		Command:   "compound proof-check --base " + source.BaseSHA + " --out /synthetic/source-bound.json",
		Profile:   profile,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	reportBody, err := json.Marshal(map[string]any{
		"schema_version":  "compound.proof_check_report.v0.3",
		"profile_kind":    ProofResourceProfileCanonicalFull,
		"status":          "ok",
		"generated_at":    now.Add(time.Second).Format(time.RFC3339),
		"repo":            repo,
		"base_ref":        source.BaseSHA,
		"source_snapshot": source,
		"dry_run":         false,
		"proof_refs":      []string{"synthetic-proof"},
		"steps": []map[string]any{{
			"name":      "synthetic-gate",
			"command":   []string{"go", "test", "./internal/synthetic"},
			"proof_ref": "synthetic-proof",
			"status":    "ok",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(store.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(reportBody)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteProofResource(workID, ProofResourceCompleteInput{
		Actor:             actor,
		ReservationID:     reserved.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return completed
}

func TestRecordedLandCheckRejectsCallerShapedProofResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		proofHead     func(f landingProofFixture) string
		wantCloseFail bool
	}{
		{
			name:          "matching",
			proofHead:     func(f landingProofFixture) string { return f.candidate },
			wantCloseFail: true,
		},
		{
			name: "mismatch",
			proofHead: func(f landingProofFixture) string {
				head := strings.Repeat("f", len(f.candidate))
				if head == f.candidate {
					head = strings.Repeat("e", len(f.candidate))
				}
				return head
			},
			wantCloseFail: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 28, 5, 30, 0, 0, time.UTC)
			fixture := newLandingProofFixture(t, now)
			item, err := fixture.store.Get(fixture.workID)
			if err != nil {
				t.Fatal(err)
			}
			head := tc.proofHead(fixture)
			resultID := idFor("pfrs_", fixture.workID, "stored-proof", head)
			verificationRef := "proof-check:" + resultID
			sourceIdentity := gitrepository.Snapshot{
				RepositoryFingerprint: item.GitDelivery.RepositoryFingerprint,
				BaseSHA:               fixture.base,
				HeadSHA:               head,
				TreeSHA:               head,
				Clean:                 true,
			}
			reportBody, err := json.Marshal(map[string]any{
				"schema_version":  "compound.proof_check_report.v0.3",
				"profile_kind":    ProofResourceProfileCanonicalFull,
				"status":          "ok",
				"generated_at":    now.Add(5 * time.Minute).Format(time.RFC3339),
				"repo":            fixture.repo,
				"base_ref":        fixture.base,
				"source_snapshot": sourceIdentity,
				"dry_run":         false,
				"proof_refs":      []string{"synthetic-proof"},
				"steps": []map[string]any{{
					"name":      "synthetic-gate",
					"command":   []string{"go", "test", "./internal/synthetic"},
					"proof_ref": "synthetic-proof",
					"status":    "ok",
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			artifacts, err := home.NewArtifactStore(fixture.store.home)
			if err != nil {
				t.Fatal(err)
			}
			reportRef, err := artifacts.Put(reportBody)
			if err != nil {
				t.Fatal(err)
			}
			result := ProofResourceResult{
				SchemaVersion:     ProofResourceResultSchemaVersion,
				ID:                resultID,
				Kind:              "work.proof_resource.result",
				Status:            ProofResourceVerified,
				WorkID:            fixture.workID,
				OwnedReport:       true,
				ReportArtifactRef: reportRef,
				VerificationRef:   verificationRef,
				SourceSnapshot:    &sourceIdentity,
			}
			if err := fixture.store.writeEventJSON(resultID, &result); err != nil {
				t.Fatal(err)
			}
			item, err = fixture.store.Advance(fixture.workID, AdvanceInput{
				To:             item.State,
				Actor:          "codex",
				VerificationID: verificationRef,
			}, now.Add(5*time.Minute))
			if err != nil {
				t.Fatal(err)
			}

			proof := fixture.proof
			if !tc.wantCloseFail {
				proof, err = fixture.store.LandCheck(fixture.workID, LandCheckOptions{
					RepoPath:        fixture.repo,
					CandidateTipSHA: fixture.candidate,
					TargetRef:       "main",
					Record:          true,
				}, now.Add(6*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if proof.Status != "landed" {
					t.Fatalf("matching stored proof did not produce landed proof: %+v", proof)
				}
			}
			_, err = fixture.store.recordedLandedProofForClose(item, []string{proof.EvidenceRef})
			if tc.wantCloseFail {
				if err == nil || !strings.Contains(err.Error(), "proof finalizer chain") {
					t.Fatalf("caller-shaped stored proof close error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("matching stored proof rejected at close: %v", err)
			}
		})
	}
}

func initProofSourceRepo(t *testing.T, origin string) string {
	t.Helper()
	repo := t.TempDir()
	runProofSourceGit(t, repo, "init", "-b", "main")
	runProofSourceGit(t, repo, "config", "user.email", "test@example.invalid")
	runProofSourceGit(t, repo, "config", "user.name", "Compound Test")
	runProofSourceGit(t, repo, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte(origin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runProofSourceGit(t, repo, "add", "seed.txt")
	runProofSourceGit(t, repo, "commit", "-m", "seed")
	return repo
}

func initSHA256ProofSourceRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.Command("git", "-C", repo, "init", "--object-format=sha256", "-b", "main")
	out, err := cmd.CombinedOutput()
	if err != nil {
		lower := strings.ToLower(string(out))
		if strings.Contains(lower, "sha256") &&
			(strings.Contains(lower, "not supported") ||
				strings.Contains(lower, "unsupported") ||
				strings.Contains(lower, "unknown object format")) {
			t.Skipf("git explicitly reports SHA-256 object format unsupported: %s", out)
		}
		t.Fatalf("git init --object-format=sha256: %v\n%s", err, out)
	}
	runProofSourceGit(t, repo, "config", "user.email", "test@example.invalid")
	runProofSourceGit(t, repo, "config", "user.name", "Compound Test")
	runProofSourceGit(t, repo, "remote", "add", "origin", "https://example.invalid/compound/sha256-proof.git")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("sha256 seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runProofSourceGit(t, repo, "add", "seed.txt")
	runProofSourceGit(t, repo, "commit", "-m", "seed")
	return repo
}

func runProofSourceGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func proofSourceGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
