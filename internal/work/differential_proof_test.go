package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
)

func TestRecordedLandCheckVerificationRefAcceptsOnlyCanonicalShapes(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{
		"proof-check:pfrs_candidate",
		"proof-diff:pfrs_candidate:pfrs_baseline",
	} {
		if !validRecordedLandCheckVerificationRef(ref) {
			t.Fatalf("canonical verification ref rejected: %q", ref)
		}
	}
	for _, ref := range []string{
		"",
		"proof-check:pfr_candidate",
		"proof-diff:pfrs_candidate",
		"proof-diff:pfr_candidate:pfrs_baseline",
		"proof-diff:pfrs_candidate:pfrs_baseline:extra",
		"evidence:forged",
	} {
		if validRecordedLandCheckVerificationRef(ref) {
			t.Fatalf("malformed verification ref accepted: %q", ref)
		}
	}
}

func TestDifferentialProofComparisonAcceptsOnlyBaselineKnownFailures(t *testing.T) {
	t.Parallel()
	candidate, candidateReport, baseline, baselineReport := differentialComparisonFixture(t)
	if err := validateDifferentialProofComparison(candidate, candidateReport, baseline, baselineReport); err != nil {
		t.Fatalf("valid differential proof comparison rejected: %v", err)
	}
}

func TestDifferentialProofComparisonFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*ProofResourceResult, *ownedProofReport, *ProofResourceResult, *ownedProofReport)
		want   string
	}{
		{
			name: "candidate has new failure fact",
			mutate: func(_ *ProofResourceResult, c *ownedProofReport, _ *ProofResourceResult, _ *ownedProofReport) {
				c.Steps[0].FailureFacts = append(c.Steps[0].FailureFacts, differentialFact("c"))
			},
			want: "not present on baseline",
		},
		{
			name: "candidate has malformed failure fact",
			mutate: func(_ *ProofResourceResult, c *ownedProofReport, _ *ProofResourceResult, _ *ownedProofReport) {
				c.Steps[0].FailureFacts = []string{"sha256:not-a-digest"}
			},
			want: "malformed failure_facts",
		},
		{
			name: "candidate failure facts empty",
			mutate: func(_ *ProofResourceResult, c *ownedProofReport, _ *ProofResourceResult, _ *ownedProofReport) {
				c.Steps[0].FailureFacts = nil
			},
			want: "failure_facts",
		},
		{
			name: "gate inventory differs beyond source range",
			mutate: func(_ *ProofResourceResult, c *ownedProofReport, _ *ProofResourceResult, _ *ownedProofReport) {
				c.Steps[0].Command = append(c.Steps[0].Command, "--stat")
			},
			want: "gate inventory",
		},
		{
			name: "baseline head is not candidate base",
			mutate: func(_ *ProofResourceResult, _ *ownedProofReport, b *ProofResourceResult, _ *ownedProofReport) {
				b.SourceSnapshot.HeadSHA = strings.Repeat("d", 40)
			},
			want: "baseline HEAD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate, candidateReport, baseline, baselineReport := differentialComparisonFixture(t)
			tt.mutate(&candidate, &candidateReport, &baseline, &baselineReport)
			err := validateDifferentialProofComparison(candidate, candidateReport, baseline, baselineReport)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLandCheckConsumesGovernedDifferentialProofAfterReloadingChains(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	store := newStore(t)
	repo := initProofSourceRepo(t, "https://example.invalid/compound/differential.git")
	baselineBase, err := runLandingGitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "baseline.txt"), []byte("baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landingGit(t, repo, "add", "baseline.txt")
	landingGit(t, repo, "commit", "-m", "baseline")
	baselineHead, err := runLandingGitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	baselineWorktree := filepath.Join(t.TempDir(), "baseline-worktree")
	landingGit(t, repo, "worktree", "add", "--detach", baselineWorktree, baselineHead)
	baselineSnapshot, err := gitrepository.CaptureCleanSnapshot(baselineWorktree, baselineBase)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landingGit(t, repo, "add", "candidate.txt")
	landingGit(t, repo, "commit", "-m", "candidate")
	candidateSnapshot, err := gitrepository.CaptureCleanSnapshot(repo, baselineHead)
	if err != nil {
		t.Fatal(err)
	}

	baselineWork := createDifferentialProofWork(t, store, baselineWorktree, baselineSnapshot, baselineBase, "baseline-owner", now)
	candidateWork := createDifferentialProofWork(t, store, repo, candidateSnapshot, baselineHead, "candidate-owner", now.Add(time.Second))
	baselineResult := completeDifferentialFailedProof(t, store, baselineWork, baselineWorktree, baselineSnapshot, []string{differentialFact("a"), differentialFact("b")}, now.Add(3*time.Second))
	candidateResult := completeDifferentialFailedProof(t, store, candidateWork, repo, candidateSnapshot, []string{differentialFact("a")}, now.Add(6*time.Second))

	current, err := store.Get(candidateWork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(candidateWork.ID, AdvanceInput{
		To:         current.State,
		Actor:      "candidate-owner",
		ArtifactID: "commit:" + candidateSnapshot.HeadSHA,
	}, now.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	badVerificationRef := differentialProofVerificationRef(candidateResult.ID, "pfrs_missing")
	if _, err := store.Advance(candidateWork.ID, AdvanceInput{
		To:             current.State,
		Actor:          "candidate-owner",
		VerificationID: badVerificationRef,
	}, now.Add(10*time.Second)); err == nil {
		t.Fatal("malformed differential proof request was attached")
	}
	current, err = store.Get(candidateWork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if contains(current.VerificationIDs, badVerificationRef) {
		t.Fatalf("invalid differential verification mutated work: %v", current.VerificationIDs)
	}

	verificationRef := differentialProofVerificationRef(candidateResult.ID, baselineResult.ID)
	current, err = store.Advance(candidateWork.ID, AdvanceInput{
		To:             current.State,
		Actor:          "candidate-owner",
		VerificationID: verificationRef,
		Note:           "governed exact-base differential proof request",
	}, now.Add(10*time.Second))
	if err != nil {
		t.Fatalf("attach differential proof request: %v", err)
	}
	if !contains(current.VerificationIDs, verificationRef) {
		t.Fatalf("differential verification not attached: %v", current.VerificationIDs)
	}

	checked, err := store.LandCheck(candidateWork.ID, LandCheckOptions{
		CandidateTipSHA: candidateSnapshot.HeadSHA,
		Actor:           "candidate-owner",
		Record:          true,
	}, now.Add(11*time.Second))
	if err != nil {
		t.Fatalf("land-check differential proof: %v", err)
	}
	if checked.Status != "landed" || checked.ProofVerificationRef != verificationRef || checked.ProofHeadSHA != candidateSnapshot.HeadSHA {
		t.Fatalf("land-check = %+v", checked)
	}
	if err := validateRecordedLandCheck(current, checked.EvidenceRef, checked.ID, checked.ProofPath, checked); err != nil {
		t.Fatalf("recorded differential land-check rejected by terminal close gate: %v", err)
	}
}

func differentialFact(hexDigit string) string {
	return "sha256:" + strings.Repeat(hexDigit, 64)
}

func differentialComparisonFixture(t *testing.T) (ProofResourceResult, ownedProofReport, ProofResourceResult, ownedProofReport) {
	t.Helper()
	fingerprint := strings.Repeat("f", 64)
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 40)
	c := strings.Repeat("c", 40)
	baselineSnapshot := gitrepository.Snapshot{RepositoryFingerprint: fingerprint, BaseSHA: a, HeadSHA: b, TreeSHA: strings.Repeat("1", 40), Clean: true}
	candidateSnapshot := gitrepository.Snapshot{RepositoryFingerprint: fingerprint, BaseSHA: b, HeadSHA: c, TreeSHA: strings.Repeat("2", 40), Clean: true}
	candidate := ProofResourceResult{ID: "pfrs_candidate", Status: ProofResourceFailed, ReportStatus: "failed", OwnedReport: true, SourceSnapshot: &candidateSnapshot}
	baseline := ProofResourceResult{ID: "pfrs_baseline", Status: ProofResourceFailed, ReportStatus: "failed", OwnedReport: true, SourceSnapshot: &baselineSnapshot}
	candidateReport := decodeDifferentialReportForTest(t, candidateSnapshot, []string{differentialFact("a")})
	baselineReport := decodeDifferentialReportForTest(t, baselineSnapshot, []string{differentialFact("a"), differentialFact("b")})
	return candidate, candidateReport, baseline, baselineReport
}

func decodeDifferentialReportForTest(t *testing.T, source gitrepository.Snapshot, facts []string) ownedProofReport {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schema_version": "compound.proof_check_report.v0.3", "profile_kind": ProofResourceProfileCanonicalFull,
		"status": "failed", "generated_at": "2026-08-01T12:00:00Z", "repo": "/synthetic/repo",
		"base_ref": source.BaseSHA, "source_snapshot": source, "dry_run": false, "proof_refs": []string{},
		"steps": []map[string]any{{"name": "gate", "command": []string{"git", "diff", source.BaseSHA, source.HeadSHA}, "proof_ref": "gate-proof", "status": "failed", "failure_facts": facts}},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := decodeOwnedProofReport(body)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func createDifferentialProofWork(t *testing.T, store *Store, repo string, source gitrepository.Snapshot, expectedBase, owner string, now time.Time) envelope.WorkEnvelope {
	t.Helper()
	item, err := store.Create(CreateInput{Owner: owner, Actor: owner, SuccessPredicate: "differential proof is attributable", GitDelivery: &envelope.GitDeliveryContract{
		TargetWorktreeRoot: repo, RepositoryFingerprint: source.RepositoryFingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: expectedBase, PublicationMode: "local_only",
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err = store.Claim(item.ID, owner, now.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func completeDifferentialFailedProof(t *testing.T, store *Store, item envelope.WorkEnvelope, repo string, source gitrepository.Snapshot, facts []string, now time.Time) ProofResourceResult {
	t.Helper()
	gate := ProofResourceGateProfile{Name: "gate", Command: []string{"git", "diff", source.BaseSHA, source.HeadSHA}, ProofRef: "gate-proof"}
	profile, err := NewProofResourceProfileWithSource(ProofResourceProfileCanonicalFull, repo, source, []ProofResourceGateProfile{gate})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ReserveProofResource(item.ID, ProofResourceReserveInput{Actor: item.Owner, TTL: time.Minute, Workspace: repo, Command: "synthetic differential proof", Profile: profile}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"schema_version": "compound.proof_check_report.v0.3", "profile_kind": ProofResourceProfileCanonicalFull,
		"status": "failed", "generated_at": now.Add(time.Second).Format(time.RFC3339), "repo": repo,
		"base_ref": source.BaseSHA, "source_snapshot": source, "dry_run": false, "proof_refs": []string{},
		"steps": []map[string]any{{"name": gate.Name, "command": gate.Command, "proof_ref": gate.ProofRef, "status": "failed", "failure_facts": facts}},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(store.home)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteProofResource(item.ID, ProofResourceCompleteInput{Actor: item.Owner, ReservationID: reserved.Reservation.ID, ReportArtifactRef: ref, ProducerExitKnown: true, ProducerExitCode: 1}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if completed.Result.Status != ProofResourceFailed || completed.VerificationRef != "" || !completed.ResourceReleased {
		t.Fatalf("failed proof completion = %+v", completed)
	}
	return completed.Result
}
