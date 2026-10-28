package work

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
)

func privateSourceBoundPlan(t *testing.T, f privateSourceFixture) CanonicalProofPlan {
	t.Helper()
	plan := CanonicalProofPlan{ID: "synthetic", Repo: f.repo, Source: f.source, Steps: []ProofCheckStep{{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs>"}, ProofRef: "redact-public-clean"}}}
	bound, err := BindNativeSourceRedactionReview(plan, f.store.home, f.workID, proofActorAlpha, f.action.ID)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func privateSourceVerifiedReport(t *testing.T, f privateSourceFixture, mutate func(map[string]any)) (ProofResourceCompleteResult, error) {
	t.Helper()
	plan := privateSourceBoundPlan(t, f)
	profile, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-3 * time.Second)
	reserved, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: f.repo, Command: "synthetic private proof finalizer", Profile: profile}, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	authority, err := f.store.resolvePrivateSourceReview(f.workID, proofActorAlpha, f.action.ID, f.repo, f.source, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	step.Status = "ok"
	step.PrivateSourceEvidence = &PrivateSourceEvidence{Binding: authority.Binding, Review: authority.Body, Scans: []PublicRedactionScan{f.review.Files[0].PublicRedactionScan}, Destination: authority.Destination}
	report := map[string]any{"schema_version": ProofCheckSchemaVersion, "profile_kind": ProofResourceProfileCanonicalFull, "generated_at": time.Now().Format(time.RFC3339Nano), "repo": f.repo, "base_ref": f.source.BaseSHA, "source_snapshot": f.source, "status": "ok", "dry_run": false, "proof_refs": []string{step.ProofRef}, "steps": []ProofCheckStep{step}}
	if mutate != nil {
		mutate(report)
	}
	artifacts, err := home.NewArtifactStore(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(publicReviewJSON(t, report))
	if err != nil {
		t.Fatal(err)
	}
	return f.store.CompleteProofResource(f.workID, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: reserved.Reservation.ID, ReportArtifactRef: ref, ProducerExitKnown: true, ProducerExitCode: 0}, time.Now())
}

func TestPrivateSourceNativeFinalizerRejectsTamperingAndAcceptsExactChain(t *testing.T) {
	for _, name := range []string{"legacy-schema", "missing-evidence", "missing-scan", "wrong-action", "wrong-review", "wrong-destination", "wrong-source", "public-substitution", "exact"} {
		t.Run(name, func(t *testing.T) {
			f := newPrivateSourceFixture(t, "public", true)
			stubPrivateDestination(t, f.review.Destination)
			result, err := privateSourceVerifiedReport(t, f, func(report map[string]any) {
				step := &report["steps"].([]ProofCheckStep)[0]
				switch name {
				case "legacy-schema":
					report["schema_version"] = "compound.proof_check_report.v0.4"
				case "missing-evidence":
					step.PrivateSourceEvidence = nil
				case "missing-scan":
					step.PrivateSourceEvidence.Scans = nil
				case "wrong-action":
					step.PrivateSourceEvidence.Binding.ActionID = "act_other"
				case "wrong-review":
					step.PrivateSourceEvidence.Review = append(step.PrivateSourceEvidence.Review, ' ')
				case "wrong-destination":
					step.PrivateSourceEvidence.Destination.RepositoryID++
				case "wrong-source":
					source := f.source
					source.TreeSHA = strings.Repeat("e", 40)
					report["source_snapshot"] = source
				case "public-substitution":
					step.ProofRef = "redact-public-clean"
				}
			})
			if name != "exact" {
				if err == nil || result.VerificationRef != "" {
					t.Fatalf("private tamper finalized: %s %v", result.VerificationRef, err)
				}
				return
			}
			if err != nil || result.VerificationRef == "" {
				t.Fatalf("exact private proof failed: %+v %v", result, err)
			}
			w, err := f.store.Get(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			got, found, err := f.store.latestSourceBoundProofResult(w)
			if err != nil || !found || got.VerificationRef != result.VerificationRef {
				t.Fatalf("native private proof chain failed readback: %v", err)
			}
		})
	}
}

type privatePublisherFixture struct {
	f                                privateSourceFixture
	tip                              string
	pushes                           int
	args                             []string
	pushError, loseResponse, rewrite bool
}

func newPrivatePublisherFixture(t *testing.T, linked bool) *privatePublisherFixture {
	t.Helper()
	seeds := map[string]string{"package.json": "{}\n", "bun.lock": "{}\n", "compound-proof.json": `{"schema_version":"compound.proof_consumers.v1","profile":"bun_consumers_v1","consumers":[{"id":"product-flow","command":["bun","test"],"dir":".","timeout_seconds":60,"required_tools":["bun"],"report_kind":"bun_junit","required_tests":["[\"proof.test.ts\",\"\",\"passes\",[\"proof.test.ts\"]]"]}]}`}
	f := newPrivateSourceFixtureWithRoots(t, "public", true, time.Now().Add(-20*time.Second), seeds, linked)
	stubPrivateDestination(t, f.review.Destination)
	if result, err := privateSourceVerifiedReport(t, f, nil); err != nil || result.VerificationRef == "" {
		t.Fatalf("private publisher proof: %+v %v", result, err)
	}
	if _, err := f.store.Advance(f.workID, AdvanceInput{To: "running", Actor: proofActorAlpha}, time.Now()); err != nil {
		t.Fatal(err)
	}
	p := &privatePublisherFixture{f: f, tip: f.source.BaseSHA}
	previous := privateSourcePublishCommand
	privateSourcePublishCommand = func(ctx context.Context, repo, name string, args ...string) ([]byte, error) {
		if name == "gh" {
			want := []string{"api", "--hostname", "github.com", "repos/example-owner/proof-fixture/git/ref/heads/main"}
			if !reflect.DeepEqual(args, want) {
				return nil, fmt.Errorf("unexpected API invocation")
			}
			return json.Marshal(map[string]any{"ref": "refs/heads/main", "object": map[string]string{"type": "commit", "sha": p.tip}})
		}
		if name == "git" && len(args) >= 2 && args[0] == "ls-remote" {
			if p.rewrite {
				return []byte("https://example.invalid/unapproved.git\n"), nil
			}
			return []byte(f.review.Destination.CanonicalURL + "\n"), nil
		}
		if name == "git" && len(args) > 6 && args[6] == "push" {
			p.pushes++
			p.args = append([]string(nil), args...)
			if !p.pushError {
				p.tip = f.source.HeadSHA
			}
			if p.pushError || p.loseResponse {
				return nil, fmt.Errorf("synthetic transport response lost")
			}
			return nil, nil
		}
		if name == "git" && len(args) > 0 && (args[0] == "rev-parse" || args[0] == "update-ref" || args[0] == "merge-base") {
			return runPrivateSourceCommand(ctx, repo, name, args...)
		}
		return nil, fmt.Errorf("unexpected publication command; network forbidden in fixture")
	}
	t.Cleanup(func() { privateSourcePublishCommand = previous })
	return p
}

func (p *privatePublisherFixture) plan(t *testing.T) PrivateSourcePublishRequest {
	t.Helper()
	r, err := p.f.store.PrivateSourcePublishPlan(context.Background(), p.f.workID, proofActorAlpha, p.f.repo, p.f.action.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func approvePrivatePublication(t *testing.T, p *privatePublisherFixture, request PrivateSourcePublishRequest) {
	t.Helper()
	if _, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now()); err == nil {
		t.Fatal("publication admitted without separate exact approval")
	}
	if p.pushes != 0 {
		t.Fatal("publication preceded exact approval")
	}
	approvals := policy.NewExactActionApprovalStore(p.f.store.home)
	cfg, _, err := policy.LoadConfig(p.f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := approvals.Plan(request.Action.ID, cfg, 5*time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := approvals.Approve(plan.ID, "operator:synthetic-reviewer", cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateSourcePublisherBindsExactApprovalAndFixedNonForceCommit(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprint(linked), func(t *testing.T) {
			p := newPrivatePublisherFixture(t, linked)
			request := p.plan(t)
			if request.Action.MutationClass != "public" || request.Action.Capability != PrivateSourcePublishCapability {
				t.Fatal("publication authority downgraded")
			}
			approvePrivatePublication(t, p, request)
			result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
			if err != nil || result.Status != "published" || result.Action == nil || p.pushes != 1 {
				t.Fatalf("private publication failed: %+v %v pushes=%d", result, err, p.pushes)
			}
			want := []string{"-c", "http.followRedirects=false", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", "push", "--porcelain", "--no-follow-tags", "--recurse-submodules=no", "--", request.Destination.CanonicalURL, request.Source.HeadSHA + ":refs/heads/main"}
			if !reflect.DeepEqual(p.args, want) {
				t.Fatalf("unowned transport args: %q", p.args)
			}
			if tip := proofSourceGitOutput(t, p.f.repo, "rev-parse", "refs/remotes/origin/main"); tip != p.f.source.HeadSHA {
				t.Fatal("tracking ref not reconciled")
			}
			if linked {
				body, err := os.ReadFile(filepath.Join(p.f.canonical, "catalog.txt"))
				if err != nil || string(body) != "preserved unrelated operator work\n" {
					t.Fatal("canonical WIP touched")
				}
			}
			replayed, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
			if err != nil || replayed.Action == nil || !replayed.Action.Replayed || p.pushes != 1 {
				t.Fatalf("completed publication did not replay exactly: %+v %v", replayed, err)
			}
		})
	}
}

func TestPrivateSourcePublisherRefusesRequestSourceProofDestinationAndTransportDrift(t *testing.T) {
	for _, name := range []string{"request-ref", "request-proof", "remote-tip", "rewritten-transport", "dirty-source", "local-target", "live-visibility"} {
		t.Run(name, func(t *testing.T) {
			p := newPrivatePublisherFixture(t, false)
			request := p.plan(t)
			approvePrivatePublication(t, p, request)
			switch name {
			case "request-ref":
				request.Destination.TargetRef = "refs/heads/other"
			case "request-proof":
				request.ProofRef = "proof-check:pfrs_other"
			case "remote-tip":
				p.tip = strings.Repeat("a", 40)
			case "rewritten-transport":
				p.rewrite = true
			case "dirty-source":
				if err := os.WriteFile(filepath.Join(p.f.repo, "catalog.txt"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "local-target":
				runProofSourceGit(t, p.f.repo, "update-ref", "refs/heads/main", p.f.source.BaseSHA)
			case "live-visibility":
				previous := privateSourceLiveDestination
				privateSourceLiveDestination = func(context.Context, string, envelope.GitDeliveryContract, time.Time) (PrivateSourceDestination, error) {
					return PrivateSourceDestination{}, fmt.Errorf("visibility unavailable")
				}
				t.Cleanup(func() { privateSourceLiveDestination = previous })
			}
			if _, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now()); err == nil || p.pushes != 0 {
				t.Fatalf("drift published: err=%v pushes=%d", err, p.pushes)
			}
		})
	}
}

func TestPrivateSourcePublisherReconcilesLostResponseWithoutRepush(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			p := newPrivatePublisherFixture(t, false)
			request := p.plan(t)
			approvePrivatePublication(t, p, request)
			p.pushError, p.loseResponse = pending, !pending
			result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
			if !pending {
				if err != nil || result.Status != "published" || p.pushes != 1 {
					t.Fatalf("lost success not confirmed: %+v %v", result, err)
				}
				return
			}
			if err == nil || result.Status != "pending" || result.ObservationRef == "" {
				t.Fatalf("unknown outcome claimed success: %+v %v", result, err)
			}
			p.tip = request.Source.HeadSHA
			result, err = p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
			if err != nil || result.Status != "published" || p.pushes != 1 {
				t.Fatalf("pending publication repushed or failed recovery: %+v %v pushes=%d", result, err, p.pushes)
			}
		})
	}
}

func TestPrivateSourcePublisherConsumedApprovalDoesNotOutliveDecision(t *testing.T) {
	p := newPrivatePublisherFixture(t, false)
	request := p.plan(t)
	approvePrivatePublication(t, p, request)
	decision, err := p.f.store.authorizePrivatePublication(request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.f.store.recheckPrivatePublication(request, decision, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := p.f.store.recheckPrivatePublication(request, decision, time.Now().Add(6*time.Minute)); err == nil {
		t.Fatal("consumed approval outlived publication decision")
	}
	if p.pushes != 0 {
		t.Fatal("authority check published source")
	}
}

func TestPrivateSourcePublisherRefusesUncertifiedPreBasePublicationGap(t *testing.T) {
	p := newPrivatePublisherFixture(t, false)
	p.tip = proofSourceGitOutput(t, p.f.repo, "rev-parse", p.f.source.BaseSHA+"^")
	runProofSourceGit(t, p.f.repo, "update-ref", "refs/remotes/origin/main", p.tip)
	request, err := p.f.store.PrivateSourcePublishPlan(context.Background(), p.f.workID, proofActorAlpha, p.f.repo, p.f.action.ID, time.Now())
	if err == nil || p.pushes != 0 {
		t.Fatalf("uncertified Remote..Base publication gap accepted: %+v %v", request, err)
	}
}

func TestPrivateSourcePublisherFixedRefspecPreventsConfiguredTagFollowing(t *testing.T) {
	f := newPrivateSourceFixture(t, "public", true)
	bare := filepath.Join(t.TempDir(), "destination.git")
	runProofSourceGit(t, f.repo, "init", "--bare", bare)
	runProofSourceGit(t, f.repo, "push", "--", bare, f.source.BaseSHA+":refs/heads/main")
	runProofSourceGit(t, f.repo, "tag", "-a", "unapproved-extra-tag", "-m", "synthetic annotated tag", f.source.HeadSHA)
	runProofSourceGit(t, f.repo, "config", "push.followTags", "true")
	runProofSourceGit(t, f.repo, "config", "push.recurseSubmodules", "on-demand")
	request := PrivateSourcePublishRequest{Source: f.source, Destination: PrivateSourceDestination{CanonicalURL: bare, TargetRef: "refs/heads/main"}}
	// Exercise the exact native transport options against a disposable local
	// bare repository. The publisher itself still accepts only authenticated
	// GitHub destinations; no real network or credential lookup occurs here.
	runProofSourceGit(t, f.repo, privateSourcePushArguments(request)...)
	if tags := proofSourceGitOutput(t, f.repo, "--git-dir", bare, "for-each-ref", "--format=%(refname)", "refs/tags"); tags != "" {
		t.Fatalf("configured tag following broadened admitted refspec: %s", tags)
	}
	if tip := proofSourceGitOutput(t, f.repo, "--git-dir", bare, "rev-parse", "refs/heads/main"); tip != f.source.HeadSHA {
		t.Fatal("fixed commit did not reach disposable destination")
	}
}

func TestPrivateSourcePublisherKeepsConcurrentTrackingUpdateAndRecoversWithoutRepush(t *testing.T) {
	p := newPrivatePublisherFixture(t, false)
	request := p.plan(t)
	approvePrivatePublication(t, p, request)
	concurrent := proofSourceGitOutput(t, p.f.repo, "rev-parse", p.f.source.BaseSHA+"^")
	previous := privateSourcePublishCommand
	privateSourcePublishCommand = func(ctx context.Context, repo, name string, args ...string) ([]byte, error) {
		body, err := previous(ctx, repo, name, args...)
		if name == "git" && len(args) > 6 && args[6] == "push" {
			runProofSourceGit(t, p.f.repo, "update-ref", "refs/remotes/origin/main", concurrent)
		}
		return body, err
	}
	t.Cleanup(func() { privateSourcePublishCommand = previous })
	result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	if err == nil || result.Status != "pending" || p.tip != request.Source.HeadSHA || p.pushes != 1 {
		t.Fatalf("partial publication not retained: %+v %v", result, err)
	}
	if tip := proofSourceGitOutput(t, p.f.repo, "rev-parse", "refs/remotes/origin/main"); tip != concurrent {
		t.Fatal("concurrent tracking ref overwritten")
	}
	runProofSourceGit(t, p.f.repo, "update-ref", "refs/remotes/origin/main", request.Source.HeadSHA, concurrent)
	result, err = p.f.store.ApplyPrivateSourcePublication(context.Background(), request, time.Now())
	if err != nil || result.Status != "published" || p.pushes != 1 {
		t.Fatalf("partial bookkeeping recovery repeated effect: %+v %v", result, err)
	}
}
