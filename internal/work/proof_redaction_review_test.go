package work

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/finding"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/policy"
)

// An unbound review is not harmless metadata. Ignoring it would make a report
// look certified even though its claimed privacy classification was not checked.
func TestOwnedProofCannotSilentlyDiscardRedactionReviewEvidence(t *testing.T) {
	t.Parallel()
	input := canonicalOwnedProofReport()
	input["base_ref"] = ""
	input["steps"].([]map[string]any)[0]["redaction_evidence"] = map[string]any{
		"review_action_id":  "act_unbound_review",
		"review_finding_id": "find_unbound_review",
		"review_cid":        "sha256:unbound",
		"scans":             []any{},
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	report, err := decodeOwnedProofReport(body)
	if err != nil {
		return // Strict decoding is also a valid refusal boundary.
	}
	profile, err := NewProofResourceProfile(ProofResourceProfileCanonicalFull, "/synthetic/repo-alpha", "", []ProofResourceGateProfile{
		{Name: "synthetic-gate-a", Command: []string{"go", "test", "./internal/synthetic-a"}, ProofRef: "synthetic-proof-a"},
		{Name: "synthetic-gate-b", Command: []string{"go", "test", "./internal/synthetic-b"}, ProofRef: "synthetic-proof-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation := ProofResourceReservation{
		Profile: profile, Workspace: profile.Repo,
		AcquiredAt: fixedNow.Add(2 * time.Second).Format(time.RFC3339),
		ExpiresAt:  fixedNow.Add(5 * time.Second).Format(time.RFC3339),
	}
	if err := validateOwnedProofReportAgainstReservation(report, reservation); err == nil {
		t.Fatal("canonical proof silently discarded unbound privacy-review evidence")
	}
}

func publicReviewJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func publicReviewScanFixture(t *testing.T, body []byte) (PublicRedactionScan, PublicRedactionFinding) {
	t.Helper()
	f := PublicRedactionFinding{ID: "pii.phone.e164", Tier: "MEDIUM", Severity: "MEDIUM", Category: "pii", Description: "Phone number", Line: 1, Col: 11, Preview: "1202*******", AutoRedactable: true, RepoVisibility: "public"}
	report := map[string]any{"findings": []PublicRedactionFinding{f}, "counts": map[string]int{"HIGH": 0, "MEDIUM": 1, "LOW": 0, "WARN": 0}, "repoVisibility": "public", "oversize": false}
	hash := sha256.Sum256(body)
	return PublicRedactionScan{Path: "catalog.txt", SHA256: hex.EncodeToString(hash[:]), Report: publicReviewJSON(t, report), ExitCode: 2}, f
}

type publicReviewFixture struct {
	store        *Store
	workID, repo string
	source       gitrepository.Snapshot
	review       PublicRedactionReview
	finding      finding.Finding
	action       envelope.ActionIntent
}

func newPublicReviewFixture(t *testing.T, mutationClass string, approve bool) publicReviewFixture {
	return newPublicReviewFixtureAt(t, mutationClass, approve, fixedNow, nil)
}

func newPublicReviewFixtureAt(t *testing.T, mutationClass string, approve bool, at time.Time, seeds map[string]string) publicReviewFixture {
	t.Helper()
	homePath := t.TempDir()
	if err := home.Init(homePath, home.HomeKindGlobal, at); err != nil {
		t.Fatal(err)
	}
	s, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	repo := initProofSourceRepo(t, "https://example.invalid/proof/public-catalog.git")
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
	w, err := s.Create(CreateInput{
		Owner: proofActorAlpha, Actor: proofActorAlpha, State: "ready", SuccessPredicate: "exact public content review is consumed",
		GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: repo, RepositoryFingerprint: source.RepositoryFingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: base, PublicationMode: "local_only"},
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, proofActorAlpha, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	scan, flagged := publicReviewScanFixture(t, body)
	review := PublicRedactionReview{SchemaVersion: PublicRedactionReviewSchema, WorkID: w.ID, Source: source, Files: []PublicRedactionFile{{PublicRedactionScan: scan, Approvals: []PublicRedactionApproval{{Finding: flagged, Classification: "public_provider_identifier", EvidenceRefs: []string{"provider://synthetic/public-catalog"}}}}}}
	f, err := finding.Record(s.home, finding.Finding{Topic: "synthetic-public-review", Body: string(publicReviewJSON(t, review)), ProducedBy: proofActorAlpha, WorkID: w.ID}, at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ai := envelope.ActionIntent{
		Header: newHeader(envelope.SVActionIntent, "act_public_review_fixture", at.Add(3*time.Second), w.ID, "", "private", "tier2"),
		Actor:  proofActorAlpha, Capability: PublicRedactionReviewCapability, Action: PublicRedactionReviewAction,
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
	return publicReviewFixture{s, w.ID, repo, source, review, f, ai}
}

func TestPublicReviewConsumesExactNativeAuthorityNotLocalPermission(t *testing.T) {
	for _, tc := range []struct {
		name, class    string
		approved, want bool
	}{
		{"local-allow-is-not-release", "local_reversible", false, false},
		{"held-public-action-is-not-performed", "public", false, false},
		{"exact-native-public-approval", "public", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPublicReviewFixture(t, tc.class, tc.approved)
			a, err := fixture.store.ResolvePublicRedactionReview(fixture.workID, proofActorAlpha, fixture.action.ID, fixture.repo, fixture.source, fixedNow.Add(7*time.Second))
			if (err == nil) != tc.want {
				t.Fatalf("native review allowed=%t want=%t: %v", err == nil, tc.want, err)
			}
			if tc.want && (a.Binding.CID != fixture.finding.CID || a.Binding.FindingID != fixture.finding.ID || a.Binding.PolicyID == "") {
				t.Fatalf("not exact native authority: %+v", a.Binding)
			}
		})
	}
}

func TestPublicReviewAuthorityDriftFailsClosed(t *testing.T) {
	for _, name := range []string{"wrong-actor", "wrong-work", "source-drift", "expired", "missing-finding", "corrupt-cid", "linked-finding", "current-deny", "false-public-policy"} {
		t.Run(name, func(t *testing.T) {
			fixture := newPublicReviewFixture(t, "public", true)
			actor, workID, source, at := proofActorAlpha, fixture.workID, fixture.source, fixedNow.Add(7*time.Second)
			findingPath := filepath.Join(fixture.store.home, finding.Subdir, fixture.finding.ID+".json")
			switch name {
			case "wrong-actor":
				actor = proofActorBeta
			case "wrong-work":
				workID = "work_foreign"
			case "source-drift":
				source.TreeSHA = strings.Repeat("b", 40)
			case "expired":
				at = fixedNow.Add(7 * time.Minute)
			case "missing-finding":
				if err := os.Remove(findingPath); err != nil {
					t.Fatal(err)
				}
			case "corrupt-cid":
				f := fixture.finding
				f.CID = "sha256:" + strings.Repeat("a", 64)
				if err := os.WriteFile(findingPath, publicReviewJSON(t, f), 0600); err != nil {
					t.Fatal(err)
				}
			case "linked-finding":
				target := filepath.Join(t.TempDir(), "foreign.json")
				if err := os.Rename(findingPath, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, findingPath); err != nil {
					t.Fatal(err)
				}
			case "current-deny":
				if err := os.MkdirAll(filepath.Join(fixture.store.home, "config"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixture.store.home, "config", "policy.yaml"), []byte("forbidden_paths:\n  - "+fixture.repo+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "false-public-policy":
				// An ordinary native record API can receive caller-shaped decisions.
				// A consumer must still demand actual current release authority.
				other := newPublicReviewFixture(t, "public", false)
				pd := policy.Evaluate(other.action, policy.DefaultConfig(), fixedNow.Add(6*time.Second))
				pd.Outcome = "allow_with_obligations"
				pd.Obligations = []string{"emit a Receipt"}
				if _, err := other.store.RecordAction(other.workID, other.action, pd, fixedNow.Add(6*time.Second)); err != nil {
					t.Fatal(err)
				}
				fixture = other
				workID = fixture.workID
				source = fixture.source
			}
			if _, err := fixture.store.ResolvePublicRedactionReview(workID, actor, fixture.action.ID, fixture.repo, source, at); err == nil {
				t.Fatal("unsafe native review authority accepted")
			}
		})
	}
}

func TestPublicReviewRejectsMalformedAndUnreviewableReports(t *testing.T) {
	fixture := newPublicReviewFixture(t, "public", true)
	valid := publicReviewJSON(t, fixture.review)
	if _, err := ParsePublicRedactionReview(valid); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"high", "non-privacy", "other-pii", "wrong-classification", "unreviewed", "duplicate-approval", "count-mismatch", "exit-mismatch", "oversize", "missing-counts", "duplicate-finding", "duplicate-json-key", "case-duplicate-key", "source-not-clean", "duplicate-path", "path-traversal", "trailing-json"} {
		t.Run(name, func(t *testing.T) {
			var review PublicRedactionReview
			if err := json.Unmarshal(valid, &review); err != nil {
				t.Fatal(err)
			}
			file := &review.Files[0]
			var report map[string]any
			if err := json.Unmarshal(file.Report, &report); err != nil {
				t.Fatal(err)
			}
			findings := report["findings"].([]any)
			flag := findings[0].(map[string]any)
			switch name {
			case "high":
				flag["severity"] = "HIGH"
				flag["tier"] = "HIGH"
				report["counts"] = map[string]int{"HIGH": 1, "MEDIUM": 0, "LOW": 0, "WARN": 0}
				file.ExitCode = 3
			case "non-privacy":
				flag["category"] = "credential"
			case "other-pii":
				flag["id"] = "pii.other"
			case "wrong-classification":
				file.Approvals[0].Classification = "public_support_contact"
			case "unreviewed":
				file.Approvals = nil
			case "duplicate-approval":
				file.Approvals = append(file.Approvals, file.Approvals[0])
			case "count-mismatch":
				report["counts"] = map[string]int{"HIGH": 0, "MEDIUM": 0, "LOW": 0, "WARN": 0}
			case "exit-mismatch":
				file.ExitCode = 0
			case "oversize":
				report["oversize"] = true
			case "missing-counts":
				delete(report, "counts")
			case "duplicate-finding":
				report["findings"] = append(findings, findings[0])
				report["counts"] = map[string]int{"HIGH": 0, "MEDIUM": 2, "LOW": 0, "WARN": 0}
			case "source-not-clean":
				review.Source.Clean = false
			case "duplicate-path":
				review.Files = append(review.Files, review.Files[0])
			case "path-traversal":
				file.Path = "../catalog.txt"
			}
			file.Report = publicReviewJSON(t, report)
			body := publicReviewJSON(t, review)
			switch name {
			case "duplicate-json-key":
				body = append([]byte(`{"work_id":"work_foreign",`), body[1:]...)
			case "case-duplicate-key":
				body = append([]byte(`{"WORK_ID":"work_foreign",`), body[1:]...)
			case "trailing-json":
				body = append(body, []byte(` {}`)...)
			}
			if _, err := ParsePublicRedactionReview(body); err == nil {
				t.Fatal("unsafe review/report accepted")
			}
		})
	}
}

func TestPublicReviewScannerCountsMustBeExplicit(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"HIGH", "MEDIUM", "LOW", "WARN"} {
		for _, missing := range []bool{true, false} {
			t.Run(key+map[bool]string{true: "/missing", false: "/null"}[missing], func(t *testing.T) {
				counts := map[string]any{"HIGH": 0, "MEDIUM": 0, "LOW": 0, "WARN": 0}
				if missing {
					delete(counts, key)
				} else {
					counts[key] = nil
				}
				report := map[string]any{"findings": []PublicRedactionFinding{}, "counts": counts, "repoVisibility": "public", "oversize": false}
				if _, err := ParsePublicRedactionReport(publicReviewJSON(t, report), 0); err == nil {
					t.Fatal("incomplete scanner counts accepted as a clean scan")
				}
			})
		}
	}
}

func TestPublicReviewRejectsUnicodeDuplicateDecidingKeys(t *testing.T) {
	t.Parallel()
	scan, _ := publicReviewScanFixture(t, []byte("synthetic body"))
	ambiguous := strings.Replace(string(scan.Report), `"severity":"MEDIUM"`, `"severity":"HIGH","\u017feverity":"MEDIUM"`, 1)
	if _, err := ParsePublicRedactionReport([]byte(ambiguous), 2); err == nil {
		t.Error("scanner severity was silently replaced through Unicode case folding")
	}
	report := publicReviewJSON(t, canonicalOwnedProofReport())
	ambiguousReport := append([]byte(`{"\u017fchema_version":"compound.proof_check_report.v0.1",`), report[1:]...)
	if _, err := decodeOwnedProofReport(ambiguousReport); err == nil {
		t.Error("owned proof accepted two decoder-equivalent schema keys")
	}
	if err := consumerUniqueJSON([]byte(`{"key":1,"\u212aey":2}`)); err == nil {
		t.Error("deciding JSON accepted Kelvin-sign field ambiguity")
	}
}

func TestPublicReviewFreshScannerMustCoverEveryExactFile(t *testing.T) {
	fixture := newPublicReviewFixture(t, "public", true)
	review := fixture.review
	cleanBody := []byte(`{"findings":[],"counts":{"HIGH":0,"MEDIUM":0,"LOW":0,"WARN":0},"repoVisibility":"public","oversize":false}`)
	review.Files = append(review.Files, PublicRedactionFile{PublicRedactionScan: PublicRedactionScan{Path: "z-clean.txt", SHA256: strings.Repeat("c", 64), Report: cleanBody, ExitCode: 0}})
	for _, name := range []string{"exact", "missing-clean-file", "changed-body", "new-high", "new-medium", "different-position", "different-preview", "unexpected-file", "duplicate-file"} {
		t.Run(name, func(t *testing.T) {
			scans := []PublicRedactionScan{review.Files[0].PublicRedactionScan, review.Files[1].PublicRedactionScan}
			switch name {
			case "missing-clean-file":
				scans = scans[:1]
			case "changed-body":
				scans[0].SHA256 = strings.Repeat("d", 64)
			case "new-high", "new-medium", "different-position", "different-preview":
				var report map[string]any
				if err := json.Unmarshal(scans[0].Report, &report); err != nil {
					t.Fatal(err)
				}
				fs := report["findings"].([]any)
				f := fs[0].(map[string]any)
				switch name {
				case "new-high":
					fs = append(fs, map[string]any{"id": "credential.token", "tier": "HIGH", "severity": "HIGH", "category": "credential", "description": "Secret", "line": 2, "col": 1, "preview": "mask****", "autoRedactable": false, "repoVisibility": "public"})
					report["counts"] = map[string]int{"HIGH": 1, "MEDIUM": 1, "LOW": 0, "WARN": 0}
					scans[0].ExitCode = 3
				case "new-medium":
					extra := map[string]any{}
					for k, v := range f {
						extra[k] = v
					}
					extra["line"] = 2
					fs = append(fs, extra)
					report["counts"] = map[string]int{"HIGH": 0, "MEDIUM": 2, "LOW": 0, "WARN": 0}
				case "different-position":
					f["col"] = 12
				case "different-preview":
					f["preview"] = "9999*******"
				}
				report["findings"] = fs
				scans[0].Report = publicReviewJSON(t, report)
			case "unexpected-file":
				scans = append(scans, scans[1])
				scans[2].Path = "other.txt"
			case "duplicate-file":
				scans[1] = scans[0]
			}
			err := ValidatePublicRedactionScans(review, scans)
			if (err == nil) != (name == "exact") {
				t.Fatalf("scanner consumption %s: %v", name, err)
			}
		})
	}
}

func TestPublicReviewProfileAndReportBindNativeTuple(t *testing.T) {
	fixture := newPublicReviewFixture(t, "public", true)
	a, err := fixture.store.ResolvePublicRedactionReview(fixture.workID, proofActorAlpha, fixture.action.ID, fixture.repo, fixture.source, fixedNow.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	plan := CanonicalProofPlan{ID: "synthetic", Repo: fixture.repo, Source: fixture.source, Steps: []ProofCheckStep{{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs>"}, ProofRef: "redact-public-clean"}}}
	plain, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindPublicRedactionReview(plan, a.Binding)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ProofResourceProfileForCanonicalPlan(bound)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Digest == profile.Digest || plan.Steps[0].ProofRef != "redact-public-clean" {
		t.Fatal("review authority was not sealed independently from clean proof")
	}
	step := bound.Steps[0]
	step.Status = "ok"
	step.RedactionEvidence = &PublicRedactionEvidence{Binding: a.Binding, Review: a.Body, Scans: []PublicRedactionScan{fixture.review.Files[0].PublicRedactionScan}}
	if err := ValidatePublicRedactionEvidence(step, &fixture.source, fixture.workID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "wrong-work", "wrong-source", "wrong-policy", "review-body-drift", "scan-body-drift", "unbound"} {
		t.Run(name, func(t *testing.T) {
			copy := cloneCanonicalProofPlan(CanonicalProofPlan{Steps: []ProofCheckStep{step}}).Steps[0]
			source, workID := fixture.source, fixture.workID
			switch name {
			case "missing":
				copy.RedactionEvidence = nil
			case "wrong-work":
				workID = "work_foreign"
			case "wrong-source":
				source.TreeSHA = strings.Repeat("e", 40)
			case "wrong-policy":
				copy.RedactionEvidence.Binding.PolicyID = "pol_other"
			case "review-body-drift":
				copy.RedactionEvidence.Review = append(copy.RedactionEvidence.Review, ' ')
			case "scan-body-drift":
				copy.RedactionEvidence.Scans[0].SHA256 = strings.Repeat("f", 64)
			case "unbound":
				copy.Command = plan.Steps[0].Command
				copy.ProofRef = "redact-public-clean"
			}
			if err := ValidatePublicRedactionEvidence(copy, &source, workID); err == nil {
				t.Fatal("review evidence forged or removed without refusal")
			}
		})
	}
}

func publicReviewBoundPlan(t *testing.T, f publicReviewFixture, binding PublicRedactionReviewBinding) CanonicalProofPlan {
	t.Helper()
	plan := CanonicalProofPlan{ID: "synthetic", Repo: f.repo, Source: f.source, Steps: []ProofCheckStep{{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs>"}, ProofRef: "redact-public-clean"}}}
	bound, err := BindPublicRedactionReview(plan, binding)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func TestPublicReviewAdmissionRefusesUnperformedAuthorityBeforeMint(t *testing.T) {
	f := newPublicReviewFixture(t, "public", false)
	bound := publicReviewBoundPlan(t, f, PublicRedactionReviewBinding{ActionID: f.action.ID, PolicyID: "pol_unperformed", FindingID: f.finding.ID, CID: f.finding.CID})
	profile, err := ProofResourceProfileForCanonicalPlan(bound)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: f.repo, Command: "synthetic reviewed proof", Profile: profile}, fixedNow.Add(7*time.Second))
	if err == nil {
		_ = closeProofFixtureExecutionBarrier(result.ExecutionBarrier)
		t.Fatal("admission accepted an unperformed privacy review")
	}
	after, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.RunAttemptIDs) != len(after.RunAttemptIDs) || len(before.ActionIntentIDs) != len(after.ActionIntentIDs) {
		t.Fatal("privacy-review refusal minted proof authority or a RunAttempt")
	}
}

func TestPublicReviewFinalizationRechecksAuthorityExpiry(t *testing.T) {
	f := newPublicReviewFixture(t, "public", true)
	a, err := f.store.ResolvePublicRedactionReview(f.workID, proofActorAlpha, f.action.ID, f.repo, f.source, fixedNow.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	bound := publicReviewBoundPlan(t, f, a.Binding)
	profile, err := ProofResourceProfileForCanonicalPlan(bound)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: 10 * time.Minute, Workspace: f.repo, Command: "synthetic reviewed proof", Profile: profile}, fixedNow.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	step := bound.Steps[0]
	step.Status = "ok"
	step.RedactionEvidence = &PublicRedactionEvidence{Binding: a.Binding, Review: a.Body, Scans: []PublicRedactionScan{f.review.Files[0].PublicRedactionScan}}
	late := fixedNow.Add(7 * time.Minute)
	report := map[string]any{"schema_version": ProofCheckSchemaVersion, "profile_kind": ProofResourceProfileCanonicalFull, "status": "ok", "generated_at": late.Format(time.RFC3339), "repo": f.repo, "base_ref": f.source.BaseSHA, "source_snapshot": f.source, "dry_run": false, "proof_refs": []string{step.ProofRef}, "steps": []ProofCheckStep{step}}
	artifacts, err := home.NewArtifactStore(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(publicReviewJSON(t, report))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := f.store.CompleteProofResource(f.workID, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: reserved.Reservation.ID, ReportArtifactRef: ref, ProducerExitKnown: true, ProducerExitCode: 0}, late)
	if err == nil || completed.VerificationRef != "" {
		t.Fatal("expired release authority certified a green proof")
	}
}

func TestPublicReviewExpiredApprovalCLIRecoversOnlySealedCompletion(t *testing.T) {
	// The report is a synthetic historical fixture. The public CLI must recover
	// its native sealed finalizer, never execute the fixture's Cargo gates.
	at := time.Now().UTC().Add(-12 * time.Minute)
	f := newPublicReviewFixtureAt(t, "public", true, at, map[string]string{
		"Cargo.toml": "[package]\nname = \"synthetic-proof\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"Cargo.lock": "version = 3\n",
		"src/lib.rs": "pub fn value() -> u8 { 1 }\n",
	})
	a, err := f.store.ResolvePublicRedactionReview(f.workID, proofActorAlpha, f.action.ID, f.repo, f.source, at.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ResolveCanonicalProofPlan(f.repo, f.source, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = BindPublicRedactionReview(plan, a.Binding)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "proof.json")
	args := []string{"proof-check", "--repo", f.repo, "--base", f.source.BaseSHA, "--work", f.workID, "--actor", proofActorAlpha, "--redact-review", f.action.ID, "--out", out}
	command := "argv-json:" + string(publicReviewJSON(t, append([]string{"compound"}, args...)))
	reserved, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: 10 * time.Minute, Workspace: f.repo, Command: command, Profile: profile}, at.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	refs := []string{}
	for i := range plan.Steps {
		step := &plan.Steps[i]
		step.Status = "ok"
		refs = append(refs, step.ProofRef)
		if step.Name == "redact-public" {
			step.RedactionEvidence = &PublicRedactionEvidence{Binding: a.Binding, Review: a.Body, Scans: []PublicRedactionScan{f.review.Files[0].PublicRedactionScan}}
		}
	}
	report := map[string]any{"schema_version": ProofCheckSchemaVersion, "profile_kind": ProofResourceProfileCanonicalFull, "generated_at": at.Add(8 * time.Second).Format(time.RFC3339), "repo": f.repo, "base_ref": f.source.BaseSHA, "source_snapshot": f.source, "status": "ok", "dry_run": false, "proof_refs": refs, "steps": plan.Steps}
	artifacts, err := home.NewArtifactStore(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	body := publicReviewJSON(t, report)
	ref, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(f.store.home, "state")
	beforeWrite := proofResourceWriteWorkRecord
	t.Cleanup(func() {
		proofResourceWriteWorkRecord = beforeWrite
		_ = os.Chmod(state, 0755)
	})
	proofResourceWriteWorkRecord = func(s *Store, stage string, w *envelope.WorkEnvelope) error {
		if err := beforeWrite(s, stage, w); err != nil {
			return err
		}
		if s.home == f.store.home && stage == "work_final" {
			return os.Chmod(state, 0555)
		}
		return nil
	}
	completed, err := f.store.CompleteProofResource(f.workID, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: reserved.Reservation.ID, ReportArtifactRef: ref, ProducerExitKnown: true, ProducerExitCode: 0}, at.Add(8*time.Second))
	proofResourceWriteWorkRecord = beforeWrite
	if err := os.Chmod(state, 0755); err != nil {
		t.Fatal(err)
	}
	if err == nil || !completed.RecoveryJournalRetained || completed.ResourceReleased || completed.VerificationRef == "" {
		t.Fatalf("fixture did not commit a release-pending finalizer: %+v %v", completed, err)
	}
	if _, err := f.store.ResolvePublicRedactionReview(f.workID, proofActorAlpha, f.action.ID, f.repo, f.source, time.Now()); err == nil {
		t.Fatal("fixture public approval is not expired")
	}
	before, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, "go", append([]string{"run", "./cmd/compound"}, args...)...)
	child.Dir = filepath.Join("..", "..")
	child.Env = append(os.Environ(), "COMPOUND_HOME="+f.store.home)
	output, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "battery: ok") {
		t.Fatalf("expired review prevented exact CLI recovery: %v\n%s", err, output)
	}
	readback, err := os.ReadFile(out)
	if err != nil || string(readback) != string(body) {
		t.Fatalf("CLI did not restore the exact historical report: %v", err)
	}
	after, err := f.store.Get(f.workID)
	if err != nil || len(after.RunAttemptIDs) != len(before.RunAttemptIDs) || len(after.VerificationIDs) != len(before.VerificationIDs) {
		t.Fatalf("recovery executed new gates or minted another proof: %+v %v", after, err)
	}
	lease, err := landlease.Status(f.store.home, time.Now())
	if err != nil || lease.Held {
		t.Fatalf("exact CLI recovery retained proof resource: %+v %v", lease, err)
	}
	args[len(args)-1] = filepath.Join(t.TempDir(), "new-proof.json")
	child = exec.CommandContext(ctx, "go", append([]string{"run", "./cmd/compound"}, args...)...)
	child.Dir = filepath.Join("..", "..")
	child.Env = append(os.Environ(), "COMPOUND_HOME="+f.store.home)
	output, err = child.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "review decision expired") {
		t.Fatalf("expired historical binding admitted fresh proof: %v\n%s", err, output)
	}
	latest, err := f.store.Get(f.workID)
	if err != nil || len(latest.RunAttemptIDs) != len(after.RunAttemptIDs) {
		t.Fatal("fresh proof refusal minted another RunAttempt")
	}
}

func TestPublicReviewLegacyCleanReportCompatibility(t *testing.T) {
	t.Parallel()
	input := canonicalOwnedProofReport()
	profile, err := NewProofResourceProfile(ProofResourceProfileCanonicalFull, "/synthetic/repo-alpha", "refs/heads/main", []ProofResourceGateProfile{
		{Name: "synthetic-gate-a", Command: []string{"go", "test", "./internal/synthetic-a"}, ProofRef: "synthetic-proof-a"},
		{Name: "synthetic-gate-b", Command: []string{"go", "test", "./internal/synthetic-b"}, ProofRef: "synthetic-proof-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation := ProofResourceReservation{Profile: profile, Workspace: profile.Repo, AcquiredAt: fixedNow.Add(2 * time.Second).Format(time.RFC3339), ExpiresAt: fixedNow.Add(5 * time.Second).Format(time.RFC3339)}
	for _, version := range []string{"compound.proof_check_report.v0.4", ProofCheckSchemaVersion} {
		input["schema_version"] = version
		report, err := decodeOwnedProofReport(publicReviewJSON(t, input))
		if err != nil || !IsAcceptedProofCheckSchemaVersion(version, ProofCheckSchemaVersion) {
			t.Fatalf("clean historical report became unreadable: %s %v", version, err)
		}
		if err := validateOwnedProofReportAgainstReservation(report, reservation); err != nil {
			t.Fatalf("clean historical report lost certification compatibility: %s %v", version, err)
		}
	}
}

func TestPublicReviewFinalizerRejectsTamperingAndReadsCommittedChain(t *testing.T) {
	for _, name := range []string{"legacy-schema", "wrong-action", "wrong-policy", "missing-evidence", "missing-scan", "review-body-drift", "wrong-source", "exact"} {
		t.Run(name, func(t *testing.T) {
			f := newPublicReviewFixture(t, "public", true)
			a, err := f.store.ResolvePublicRedactionReview(f.workID, proofActorAlpha, f.action.ID, f.repo, f.source, fixedNow.Add(7*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			plan := publicReviewBoundPlan(t, f, a.Binding)
			profile, err := ProofResourceProfileForCanonicalPlan(plan)
			if err != nil {
				t.Fatal(err)
			}
			reserved, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: f.repo, Command: "synthetic reviewed finalizer", Profile: profile}, fixedNow.Add(7*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
				t.Fatal(err)
			}
			step := plan.Steps[0]
			step.Status = "ok"
			step.RedactionEvidence = &PublicRedactionEvidence{Binding: a.Binding, Review: a.Body, Scans: []PublicRedactionScan{f.review.Files[0].PublicRedactionScan}}
			makeReport := func() map[string]any {
				copy := cloneCanonicalProofPlan(CanonicalProofPlan{Steps: []ProofCheckStep{step}}).Steps
				return map[string]any{"schema_version": ProofCheckSchemaVersion, "profile_kind": ProofResourceProfileCanonicalFull, "generated_at": fixedNow.Add(8 * time.Second).Format(time.RFC3339), "repo": f.repo, "base_ref": f.source.BaseSHA, "source_snapshot": f.source, "status": "ok", "dry_run": false, "proof_refs": []string{step.ProofRef}, "steps": copy}
			}
			artifacts, err := home.NewArtifactStore(f.store.home)
			if err != nil {
				t.Fatal(err)
			}
			complete := func(report map[string]any) (ProofResourceCompleteResult, error) {
				ref, err := artifacts.Put(publicReviewJSON(t, report))
				if err != nil {
					t.Fatal(err)
				}
				return f.store.CompleteProofResource(f.workID, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: reserved.Reservation.ID, ReportArtifactRef: ref, ProducerExitKnown: true, ProducerExitCode: 0}, fixedNow.Add(8*time.Second))
			}
			if name != "exact" {
				report := makeReport()
				copy := &report["steps"].([]ProofCheckStep)[0]
				switch name {
				case "legacy-schema":
					report["schema_version"] = "compound.proof_check_report.v0.4"
				case "wrong-action":
					copy.Command[5] = "act_foreign_review"
				case "wrong-policy":
					copy.RedactionEvidence.Binding.PolicyID = "pol_foreign_review"
				case "missing-evidence":
					copy.RedactionEvidence = nil
				case "missing-scan":
					copy.RedactionEvidence.Scans = nil
				case "review-body-drift":
					copy.RedactionEvidence.Review = append(copy.RedactionEvidence.Review, ' ')
				case "wrong-source":
					source := f.source
					source.TreeSHA = strings.Repeat("e", 40)
					report["source_snapshot"] = source
				}
				result, err := complete(report)
				if err == nil || result.VerificationRef != "" {
					t.Fatalf("tampered report crossed native finalization: %+v %v", result, err)
				}
				return
			}
			result, err := complete(makeReport())
			if err != nil || !result.ResourceReleased || result.VerificationRef == "" {
				t.Fatalf("exact reviewed evidence did not finalize: %+v %v", result, err)
			}
			w, err := f.store.Get(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := f.store.runAttempts().Get(reserved.Reservation.RunAttemptID)
			if err != nil {
				t.Fatal(err)
			}
			valid, err := f.store.committedVerifiedProofRunAttempt(w, attempt)
			if err != nil || !valid {
				t.Fatalf("reviewed native certificate did not survive owned chain readback: %v", err)
			}
		})
	}
}

func TestPublicReviewScannerOverflowRetainsBoundedPartialEvidence(t *testing.T) {
	f := newPublicReviewFixtureAt(t, "public", true, time.Now().UTC().Add(-10*time.Second), nil)
	a, err := f.store.ResolvePublicRedactionReview(f.workID, proofActorAlpha, f.action.ID, f.repo, f.source, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plan := publicReviewBoundPlan(t, f, a.Binding)
	body, err := canonicalProofGitBlob(f.repo, f.source.HeadSHA, f.review.Files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ContextWithPublicRedactionReview(ContextWithCanonicalProofPlan(ctx, plan), f.store.home, f.workID, proofActorAlpha)
	// A real child supplies an external-tool overflow, not a mocked success.
	command := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "dd", "if=/dev/zero", "bs=1024", "count=1025")
	}
	inputs := func(context.Context, string, gitrepository.Snapshot) ([]PublicRedactionInput, error) {
		return []PublicRedactionInput{{Path: f.review.Files[0].Path, Body: body}}, nil
	}
	_, evidence, err := RunReviewedPublicRedaction(ctx, f.repo, plan.Steps[0], f.source.BaseSHA, []string{f.source.HeadSHA}, inputs, command)
	if err == nil || !strings.Contains(err.Error(), "scanner output exceeded") || evidence == nil || len(evidence.Scans) != 1 || len(evidence.Scans[0].Report) == 0 || len(evidence.Scans[0].Report) > finding.MaxBodyBytes {
		t.Fatalf("scanner overflow lost bounded failure evidence: evidence=%v error=%v", evidence != nil, err)
	}
	step := plan.Steps[0]
	step.Status, step.RedactionEvidence = "failed", evidence
	if err := ValidatePublicRedactionEvidence(step, &f.source, f.workID); err != nil {
		t.Fatalf("bounded partial evidence cannot be retained in red proof: %v", err)
	}
	step.Status = "ok"
	if err := ValidatePublicRedactionEvidence(step, &f.source, f.workID); err == nil {
		t.Fatal("partial overflow evidence certified as green")
	}
}
