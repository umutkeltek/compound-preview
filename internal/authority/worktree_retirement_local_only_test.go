package authority

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
)

func localOnlyRetirementRequest(t *testing.T, f retirementGitFixture) WorktreeRetirementPlanRequest {
	t.Helper()
	retirementRunGit(t, f.repo, "update-ref", "-d", "refs/remotes/origin/main")
	identity, err := ReadGitRepositoryIdentity(f.repo, f.mainTip)
	if err != nil {
		t.Fatal(err)
	}
	req := retirementPlanRequest(f)
	req.PublicationRef = ""
	req.SourceProof.Delivery = &gitrepository.WorktreeRetirementSourceDelivery{WorkDigest: "sha256:" + strings.Repeat("e", 64), TargetWorktreeRoot: f.repo, RepositoryFingerprint: identity.Fingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: f.candidateTip, PublicationMode: "local_only"}
	return req
}

func TestRetirementLocalOnlyRequiresExactBoundSourceAuthority(t *testing.T) {
	for _, name := range []string{"valid", "unbound", "empty-mode", "required", "wrong-fingerprint", "wrong-target", "fake-publication"} {
		t.Run(name, func(t *testing.T) {
			f := newRetirementGitFixture(t)
			req := localOnlyRetirementRequest(t, f)
			switch name {
			case "unbound":
				req.SourceProof.Delivery = nil
			case "empty-mode":
				req.SourceProof.Delivery.PublicationMode = ""
			case "required":
				req.SourceProof.Delivery.PublicationMode = "required"
				req.SourceProof.Delivery.PublicationRef = "refs/remotes/origin/main"
			case "wrong-fingerprint":
				req.SourceProof.Delivery.RepositoryFingerprint = strings.Repeat("0", 64)
			case "wrong-target":
				req.SourceProof.Delivery.TargetLocalRef = "refs/heads/other"
			case "fake-publication":
				req.PublicationRef = "refs/remotes/origin/main"
			}
			plan, err := BuildWorktreeRetirementPlan(context.Background(), req, WorktreeRetirementDependencies{Git: ExecWorktreeRetirementGit{}, Liveness: staticRetirementLiveness{state: WorktreeLivenessUnused}, RecoveryInventoryPreflight: testRetirementRecoveryPreflight}, time.Now())
			if name != "valid" {
				if err == nil && plan.Eligible {
					t.Fatal("caller-selected local-only bypass was admitted")
				}
				return
			}
			if err != nil || !plan.Eligible || plan.PublicationRef != "" || plan.PublicationTip != "" || plan.CandidateRelation != WorktreeCandidateContained {
				t.Fatalf("local-only plan=%+v err=%v", plan, err)
			}
			if err := ValidateWorktreeRetirementPlan(plan); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetirementLocalOnlyRetainsExactLinkedDeliveryCheckout(t *testing.T) {
	for _, name := range []string{"valid-detached", "unregistered", "missing", "replaced-repository", "retire-delivery"} {
		t.Run(name, func(t *testing.T) {
			f := newRetirementGitFixture(t)
			req := localOnlyRetirementRequest(t, f)
			retained := filepath.Join(filepath.Dir(f.repo), "retained-delivery")
			retirementRunGit(t, f.repo, "worktree", "add", "--detach", retained, f.mainTip)
			retained, err := filepath.EvalSymlinks(retained)
			if err != nil {
				t.Fatal(err)
			}
			req.SourceProof.Delivery.TargetWorktreeRoot = retained
			switch name {
			case "unregistered":
				other := newRetirementGitFixture(t)
				req.SourceProof.Delivery.TargetWorktreeRoot = other.repo
			case "missing", "replaced-repository":
				if err := os.Rename(retained, retained+"-preserved"); err != nil {
					t.Fatal(err)
				}
				if name == "replaced-repository" {
					other := newRetirementGitFixture(t)
					if err := os.Rename(other.repo, retained); err != nil {
						t.Fatal(err)
					}
				}
			case "retire-delivery":
				req.WorktreePath, req.CandidateRef = retained, ""
			}
			plan, err := BuildWorktreeRetirementPlan(context.Background(), req, WorktreeRetirementDependencies{Git: ExecWorktreeRetirementGit{}, Liveness: staticRetirementLiveness{state: WorktreeLivenessUnused}, RecoveryInventoryPreflight: testRetirementRecoveryPreflight}, time.Now())
			if name == "valid-detached" {
				if err != nil || !plan.Eligible || plan.RepoRoot == retained {
					t.Fatalf("linked delivery plan=%+v err=%v", plan, err)
				}
				if err := ValidateWorktreeRetirementPlan(plan); err != nil {
					t.Fatal(err)
				}
				plan.Worktree.Path = retained
				plan.ID, plan.PlanDigest = "", ""
				plan.ID = WorktreeRetirementPlanID(plan)
				plan.PlanDigest = WorktreeRetirementPlanDigest(plan)
				if err := ValidateWorktreeRetirementPlan(plan); err == nil || !strings.Contains(err.Error(), "retained source delivery checkout") {
					t.Fatalf("pure validation admitted retained checkout: %v", err)
				}
				return
			}
			if err == nil && plan.Eligible {
				t.Fatal("unproven or retiring delivery checkout was admitted")
			}
			switch name {
			case "replaced-repository":
				if err == nil || !strings.Contains(err.Error(), "another repository") {
					t.Fatalf("replacement did not reach common-dir fence: %v", err)
				}
			case "retire-delivery":
				if !strings.Contains(strings.Join(plan.BlockReasons, " "), "source_delivery_worktree_cannot_be_retired") {
					t.Fatalf("retained checkout protection absent: %+v err=%v", plan.BlockReasons, err)
				}
			default:
				if !strings.Contains(strings.Join(plan.BlockReasons, " "), "source_delivery_worktree_unavailable") {
					t.Fatalf("delivery registration fence absent: %+v err=%v", plan.BlockReasons, err)
				}
			}
		})
	}
}

type changingRetirementLiveness struct{ state WorktreeLivenessState }

func (p *changingRetirementLiveness) Observe(context.Context, string) WorktreeLivenessObservation {
	return WorktreeLivenessObservation{State: p.state, Evidence: "test-current-liveness"}
}

func TestRetirementLocalOnlyRechecksAuthorityAtEffectAndResumeBoundaries(t *testing.T) {
	for _, name := range []string{"source-before-effect", "source-between-effects", "source-on-resume", "liveness", "recovery", "target-ref"} {
		t.Run(name, func(t *testing.T) {
			f := newRetirementGitFixture(t)
			req := localOnlyRetirementRequest(t, f)
			home, paths, review := authorizedRetirementRequestFixture(t, req)
			runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
			records := 0
			deps := successfulRetirementApplyDependencies(runner, &records)
			current := req.SourceProof
			deps.SourceProofResolver = func(string) (WorktreeRetirementSourceProof, error) { return current, nil }
			live := &changingRetirementLiveness{state: WorktreeLivenessUnused}
			deps.Liveness = live
			deps.AfterPhase = func(phase string) error {
				trigger := WorktreeRetirementPhasePrepared
				if name == "source-between-effects" {
					trigger = WorktreeRetirementPhaseWorktreeRemoved
				}
				if phase != trigger {
					return nil
				}
				switch name {
				case "source-before-effect", "source-between-effects":
					d := *current.Delivery
					d.WorkDigest = "sha256:" + strings.Repeat("f", 64)
					current.Delivery = &d
				case "source-on-resume":
					return errors.New("simulated interruption before effect")
				case "liveness":
					live.state = WorktreeLivenessActive
				case "recovery":
					return os.WriteFile(strings.SplitN(f.recoveryRef, ":", 3)[2], []byte("changed recovery"), 0o600)
				case "target-ref":
					retirementRunGit(t, f.repo, "update-ref", "refs/heads/main", f.candidateTip)
				}
				return nil
			}
			input := WorktreeRetirementApplyRequest{HomePath: home, PlanPath: paths.PlanPath, ReviewReportPath: review.ReviewPath, Actor: "codex:test", OwnerProof: validRetirementOwnerProof(), SourceProof: req.SourceProof}
			_, err := ApplyWorktreeRetirement(context.Background(), input, deps, time.Now())
			if err == nil {
				t.Fatal("drift admitted")
			}
			if name == "source-on-resume" {
				d := *current.Delivery
				d.PublicationMode = "required"
				d.PublicationRef = "refs/remotes/origin/main"
				current.Delivery = &d
				deps.AfterPhase = nil
				_, err = ApplyWorktreeRetirement(context.Background(), input, deps, time.Now())
				if err == nil {
					t.Fatal("changed source admitted on resume")
				}
			}
			want := 0
			if name == "source-between-effects" {
				want = 1
			}
			if runner.mutationCount() != want || !retirementGitRefExists(f.repo, f.candidateRef) {
				t.Fatalf("mutation after drift: count=%d want=%d ref retained=%v err=%v", runner.mutationCount(), want, retirementGitRefExists(f.repo, f.candidateRef), err)
			}
		})
	}
}

func TestRetirementHistoricalPlanRequiresFreshReviewBeforeAnyEffect(t *testing.T) {
	data, err := os.ReadFile("gitrepository/testdata/retirement-plan-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var plan WorktreeRetirementPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorktreeRetirementPlan(plan); err != nil {
		t.Fatalf("historical identity invalid: %v", err)
	}
	home := t.TempDir()
	path := filepath.Join(home, "historical.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
	records := 0
	deps := successfulRetirementApplyDependencies(runner, &records)
	_, err = ApplyWorktreeRetirement(context.Background(), WorktreeRetirementApplyRequest{HomePath: home, PlanPath: path, ReviewReportPath: filepath.Join(home, "previous-authorization.json"), Actor: plan.OwnerProof.LeaseHolder, OwnerProof: plan.OwnerProof, SourceProof: plan.SourceProof}, deps, time.Now())
	if err == nil || !strings.Contains(err.Error(), "historical retirement plan requires fresh v0.2 planning and review") || runner.mutationCount() != 0 {
		t.Fatalf("legacy live authority not refused before effects: %v", err)
	}
}
