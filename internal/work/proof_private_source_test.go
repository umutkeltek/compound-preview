package work

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/finding"
	"compound/internal/home"
	"compound/internal/policy"
)

type privateSourceFixture struct {
	store        *Store
	workID, repo string
	canonical    string
	source       gitrepository.Snapshot
	review       PrivateSourceReview
	finding      finding.Finding
	action       envelope.ActionIntent
}

func privateDestinationFixture(at time.Time) PrivateSourceDestination {
	return PrivateSourceDestination{Host: "github.com", RepositoryID: 11, FullName: "example-owner/proof-fixture", CanonicalURL: "https://github.com/example-owner/proof-fixture.git", PushURL: "https://github.com/example-owner/proof-fixture.git", Remote: "origin", PublicationRef: "refs/remotes/origin/main", TargetRef: "refs/heads/main", ViewerID: 7, ViewerLogin: "synthetic-reviewer", Visibility: "private", ObservedAt: at.UTC().Format(time.RFC3339Nano)}
}

func newPrivateSourceFixture(t *testing.T, class string, approve bool) privateSourceFixture {
	return newPrivateSourceFixtureAt(t, class, approve, time.Now().UTC().Add(-20*time.Second), nil)
}

func stubPrivateDestination(t *testing.T, value PrivateSourceDestination) {
	t.Helper()
	previous := privateSourceLiveDestination
	privateSourceLiveDestination = func(_ context.Context, _ string, _ envelope.GitDeliveryContract, now time.Time) (PrivateSourceDestination, error) {
		value.ObservedAt = now.UTC().Format(time.RFC3339Nano)
		return value, nil
	}
	t.Cleanup(func() { privateSourceLiveDestination = previous })
}

func TestPrivateSourceReviewRequiresDistinctExactReleaseAuthority(t *testing.T) {
	for _, tc := range []struct {
		class            string
		approve, allowed bool
	}{{"local_reversible", false, false}, {"public", false, false}, {"public", true, true}} {
		t.Run(fmt.Sprint(tc.class, tc.approve), func(t *testing.T) {
			f := newPrivateSourceFixture(t, tc.class, tc.approve)
			stubPrivateDestination(t, f.review.Destination)
			got, err := f.store.resolvePrivateSourceReview(f.workID, proofActorAlpha, f.action.ID, f.repo, f.source, time.Now(), true)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v want=%v: %v", err == nil, tc.allowed, err)
			}
			if tc.allowed && (got.Binding.CID != f.finding.CID || got.Binding.PolicyID == "" || got.Destination.RepositoryID != 11) {
				t.Fatal("exact private authority was not consumed")
			}
		})
	}
}

func TestPrivateSourceReviewRejectsHighUnknownAndUnmatchedClassifications(t *testing.T) {
	f := newPrivateSourceFixture(t, "public", false)
	for _, mutate := range []func(*PrivateSourceReview){
		func(r *PrivateSourceReview) { r.Files[0].Approvals = nil },
		func(r *PrivateSourceReview) { r.Files[0].Approvals[0].Classification = "private_anything" },
		func(r *PrivateSourceReview) {
			r.Files[0].Approvals = append(r.Files[0].Approvals, r.Files[0].Approvals[0])
		},
		func(r *PrivateSourceReview) { r.Files[0].Approvals[0].EvidenceRefs = nil },
		func(r *PrivateSourceReview) { r.Source.Clean = false },
		func(r *PrivateSourceReview) { r.Destination.Visibility = "public" },
		func(r *PrivateSourceReview) {
			var report map[string]any
			_ = json.Unmarshal(r.Files[0].Report, &report)
			findings := report["findings"].([]any)
			item := findings[0].(map[string]any)
			item["id"], item["tier"], item["severity"], item["category"] = "credential.token", "HIGH", "HIGH", "secret"
			report["counts"] = map[string]int{"HIGH": 1, "MEDIUM": 0, "LOW": 0, "WARN": 0}
			r.Files[0].Report, _ = json.Marshal(report)
			r.Files[0].ExitCode = 3
		},
	} {
		var review PrivateSourceReview
		_ = json.Unmarshal(publicReviewJSON(t, f.review), &review)
		mutate(&review)
		if _, err := ParsePrivateSourceReview(publicReviewJSON(t, review), time.Now()); err == nil {
			t.Fatal("invalid/secret private classification accepted")
		}
	}
}

func TestPrivateSourceReviewClassifiesExactSyntheticAndOperationalFindings(t *testing.T) {
	f := newPrivateSourceFixture(t, "public", false)
	for _, tc := range []struct {
		id, class string
		allowed   bool
	}{
		{"pii.phone.e164", "private_synthetic_fixture", true},
		{"pii.cc", "private_synthetic_fixture", true},
		{"pii.email", "private_synthetic_fixture", true},
		{"pii.phone.e164", "private_nonsecret_operational_setting", true},
		{"pii.cc", "private_nonsecret_operational_setting", true},
		{"pii.email", "private_nonsecret_operational_setting", true},
		{"env.kv", "private_nonsecret_operational_setting", true},
		// Version literals, geometry decimals and local database filenames may
		// match these shapes without being an address or service hostname.
		{"internal.hostname", "private_nonsecret_operational_setting", true},
		{"pii.ip_public", "private_nonsecret_operational_setting", true},
		{"internal.hostname", "private_service_hostname", true},
		{"pii.ip_public", "private_operational_ip", true},
		{"internal.hostname", "private_operational_ip", false},
		{"pii.ip_public", "private_service_hostname", false},
		{"pii.ip_public", "private_synthetic_fixture", false},
		{"pii.ip_public", "private_anything", false},
		{"internal.hostname", "private_synthetic_fixture", false},
		{"internal.url_private", "private_nonsecret_operational_setting", false},
		{"pii.unknown", "private_synthetic_fixture", false},
		{"env.kv", "private_synthetic_fixture", false},
	} {
		t.Run(tc.id+"/"+tc.class, func(t *testing.T) {
			for _, condition := range []string{"exact", "missing-evidence", "unclassified", "high", "degraded", "mismatched-finding", "duplicate-approval"} {
				var review PrivateSourceReview
				if err := json.Unmarshal(publicReviewJSON(t, f.review), &review); err != nil {
					t.Fatal(err)
				}
				file := &review.Files[0]
				flagged := file.Approvals[0].Finding
				flagged.ID = tc.id
				counts := map[string]int{"HIGH": 0, "MEDIUM": 1, "LOW": 0, "WARN": 0}
				if condition == "high" {
					flagged.Tier, flagged.Severity = "HIGH", "HIGH"
					counts["HIGH"], counts["MEDIUM"], file.ExitCode = 1, 0, 3
				}
				if condition == "degraded" {
					flagged.Severity, flagged.ToolFenceDegraded = "WARN", true
					counts["MEDIUM"], counts["WARN"], file.ExitCode = 0, 1, 0
				}
				file.Report = publicReviewJSON(t, map[string]any{"findings": []PublicRedactionFinding{flagged}, "counts": counts, "repoVisibility": "public", "oversize": false})
				file.Approvals[0].Finding, file.Approvals[0].Classification = flagged, tc.class
				if condition == "missing-evidence" {
					file.Approvals[0].EvidenceRefs = nil
				}
				if condition == "unclassified" {
					file.Approvals = nil
				}
				if condition == "mismatched-finding" {
					file.Approvals[0].Finding.Col++
				}
				if condition == "duplicate-approval" {
					file.Approvals = append(file.Approvals, file.Approvals[0])
				}
				parsed, err := ParsePrivateSourceReview(publicReviewJSON(t, review), time.Now())
				want := tc.allowed && condition == "exact"
				if (err == nil) != want {
					t.Fatalf("%s allowed=%v want=%v: %v", condition, err == nil, want, err)
				}
				if want && (!bytesEqualJSON(parsed.Files[0].Report, file.Report) || parsed.Files[0].ExitCode != 2) {
					t.Fatal("classification changed the raw blocking scanner evidence")
				}
			}
		})
	}
}

func TestPrivateSourceDestinationDriftAndOfflineFailClosed(t *testing.T) {
	f := newPrivateSourceFixture(t, "public", true)
	previous := privateSourceLiveDestination
	t.Cleanup(func() { privateSourceLiveDestination = previous })
	for _, change := range []string{"repository", "viewer", "url", "ref", "visibility", "offline"} {
		t.Run(change, func(t *testing.T) {
			privateSourceLiveDestination = func(_ context.Context, _ string, _ envelope.GitDeliveryContract, now time.Time) (PrivateSourceDestination, error) {
				value := f.review.Destination
				value.ObservedAt = now.UTC().Format(time.RFC3339Nano)
				switch change {
				case "repository":
					value.RepositoryID++
				case "viewer":
					value.ViewerID++
				case "url":
					value.PushURL = "https://github.com/example-owner/other-fixture.git"
				case "ref":
					value.TargetRef = "refs/heads/other"
				case "visibility":
					value.Visibility = "public"
				case "offline":
					return value, fmt.Errorf("offline")
				}
				return value, nil
			}
			if _, err := f.store.resolvePrivateSourceReview(f.workID, proofActorAlpha, f.action.ID, f.repo, f.source, time.Now(), true); err == nil {
				t.Fatal("destination drift accepted")
			}
		})
	}
}

func TestPrivateSourceEvidenceRetainsFindingsAndRejectsSourceOrInventoryDrift(t *testing.T) {
	f := newPrivateSourceFixture(t, "public", true)
	stubPrivateDestination(t, f.review.Destination)
	a, err := f.store.resolvePrivateSourceReview(f.workID, proofActorAlpha, f.action.ID, f.repo, f.source, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	step := ProofCheckStep{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs>", privateSourceReviewMarker, a.Binding.ActionID, a.Binding.PolicyID, a.Binding.FindingID, a.Binding.CID}, ProofRef: "redact-private-source-reviewed", Status: "ok"}
	step.PrivateSourceEvidence = &PrivateSourceEvidence{Binding: a.Binding, Review: a.Body, Scans: []PublicRedactionScan{f.review.Files[0].PublicRedactionScan}, Destination: a.Destination}
	if err := ValidatePrivateSourceEvidence(step, &f.source, f.workID); err != nil {
		t.Fatal(err)
	}
	if !bytesEqualJSON(step.PrivateSourceEvidence.Scans[0].Report, f.review.Files[0].Report) {
		t.Fatal("raw scanner findings changed")
	}
	step.PrivateSourceEvidence.Scans = nil
	if ValidatePrivateSourceEvidence(step, &f.source, f.workID) == nil {
		t.Fatal("missing scanner inventory accepted")
	}
}

func bytesEqualJSON(a, b []byte) bool { return string(a) == string(b) }

func TestPrivateSourceNativeBinderPinsOriginalProfileAndLeavesPublicReviewSeparate(t *testing.T) {
	f := newPrivateSourceFixture(t, "public", true)
	plan := CanonicalProofPlan{ID: "synthetic", Repo: f.repo, Source: f.source, Steps: []ProofCheckStep{{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs>"}, ProofRef: "redact-public-clean"}}}
	bound, err := BindNativeSourceRedactionReview(plan, f.store.home, f.workID, proofActorAlpha, f.action.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, private, err := PrivateSourceBindingForStep(bound.Steps[0]); err != nil || !private {
		t.Fatal("native private binding missing", err)
	}
	if _, public, err := PublicRedactionBindingForStep(bound.Steps[0]); err != nil || public {
		t.Fatal("private review masqueraded as public approval", err)
	}
	plan.Steps[0].Command[3] = "changed profile"
	if _, err := BindNativeSourceRedactionReview(plan, f.store.home, f.workID, proofActorAlpha, f.action.ID); err == nil {
		t.Fatal("profile drift accepted")
	}
}

func newPrivateSourceFixtureAt(t *testing.T, mutationClass string, approve bool, at time.Time, seeds map[string]string) privateSourceFixture {
	return newPrivateSourceFixtureWithRoots(t, mutationClass, approve, at, seeds, false)
}

func newPrivateSourceFixtureWithRoots(t *testing.T, mutationClass string, approve bool, at time.Time, seeds map[string]string, linked bool) privateSourceFixture {
	t.Helper()
	homePath := t.TempDir()
	if err := home.Init(homePath, home.HomeKindGlobal, at); err != nil {
		t.Fatal(err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	repo := initProofSourceRepo(t, "https://github.com/example-owner/proof-fixture.git")
	for name, body := range seeds {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		runProofSourceGit(t, repo, "add", name)
	}
	if len(seeds) != 0 {
		runProofSourceGit(t, repo, "commit", "-m", "synthetic proof foundation")
	}
	base := proofSourceGitOutput(t, repo, "rev-parse", "HEAD")
	// Generate a reserved synthetic phone at runtime, never a real contact.
	body := []byte(strings.Join([]string{"provider: +1", "202", "555", "0197\n"}, ""))
	if err := os.WriteFile(filepath.Join(repo, "catalog.txt"), body, 0600); err != nil {
		t.Fatal(err)
	}
	runProofSourceGit(t, repo, "add", "catalog.txt")
	runProofSourceGit(t, repo, "commit", "-m", "public candidate")
	source, err := gitrepository.CaptureCleanSnapshot(repo, base)
	if err != nil {
		t.Fatal(err)
	}
	runProofSourceGit(t, repo, "update-ref", "refs/remotes/origin/main", base)
	canonical := repo
	if linked {
		workspace := filepath.Join(t.TempDir(), "certified")
		runProofSourceGit(t, repo, "worktree", "add", "-b", "certified-source", workspace, "HEAD")
		repo = workspace
		if err := os.WriteFile(filepath.Join(canonical, "catalog.txt"), []byte("preserved unrelated operator work\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := s.Create(CreateInput{
		Owner: proofActorAlpha, Actor: proofActorAlpha, State: "ready", SuccessPredicate: "exact public content review is consumed",
		GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: canonical, RepositoryFingerprint: source.RepositoryFingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: base, PublicationMode: "required", PublicationRef: "refs/remotes/origin/main"},
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, proofActorAlpha, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	scan, flagged := publicReviewScanFixture(t, body)
	plan := CanonicalProofPlan{ID: "synthetic", Repo: repo, Source: source, Steps: []ProofCheckStep{{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs>"}, ProofRef: "redact-public-clean"}}}
	digest, err := privateSourceProfileDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	review := PrivateSourceReview{SchemaVersion: PrivateSourceReviewSchema, WorkID: w.ID, Source: source, SourceProfileDigest: digest, Destination: privateDestinationFixture(at), Files: []PublicRedactionFile{{PublicRedactionScan: scan, Approvals: []PublicRedactionApproval{{Finding: flagged, Classification: "private_provider_identifier", EvidenceRefs: []string{"provider://synthetic/public-catalog"}}}}}}
	f, err := finding.Record(s.home, finding.Finding{Topic: "synthetic-public-review", Body: string(publicReviewJSON(t, review)), ProducedBy: proofActorAlpha, WorkID: w.ID}, at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ai := envelope.ActionIntent{
		Header: newHeader(envelope.SVActionIntent, "act_private_source_fixture", at.Add(3*time.Second), w.ID, "", "private", "tier2"),
		Actor:  proofActorAlpha, Capability: PrivateSourceReviewCapability, Action: PrivateSourceReviewAction,
		Target:        envelope.ActionTarget{Kind: "repository", ID: source.RepositoryFingerprint, Path: repo},
		MutationClass: mutationClass, Reversibility: "manual", BlastRadius: "repo", ArgumentsHash: f.CID,
		Preview: "classify exactly one source-bound public provider identifier", RollbackPlan: "retain candidate; require new proof on source drift",
	}
	ai.EvidenceRefs = []string{"finding:" + f.ID}
	r, err := s.EvaluateAndRecordAction(w.ID, ai, at.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if mutationClass == "public" && r.Recorded {
		t.Fatal("fixture public review unexpectedly allowed without authority")
	}
	if mutationClass == "local_reversible" && !r.Recorded {
		t.Fatal("local-permission control must be actually allowed and performed")
	}
	if approve {
		if r.Recorded || r.Decision.Outcome != "hold" {
			t.Fatal("fixture must begin at native policy hold")
		}
		cfg, _, err := policy.LoadConfig(s.home)
		if err != nil {
			t.Fatal(err)
		}
		approvals := policy.NewExactActionApprovalStore(s.home)
		p, _, err := approvals.Plan(ai.ID, cfg, 5*time.Minute, at.Add(4*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := approvals.Approve(p.ID, "operator:synthetic-reviewer", cfg, at.Add(5*time.Second)); err != nil {
			t.Fatal(err)
		}
		r, err = s.EvaluateAndRecordAction(w.ID, ai, at.Add(6*time.Second))
		if err != nil || !r.Recorded {
			t.Fatalf("exact native review approval: %+v %v", r, err)
		}
	}
	return privateSourceFixture{store: s, workID: w.ID, repo: repo, canonical: canonical, source: source, review: review, finding: f, action: ai}
}
