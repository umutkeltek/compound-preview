package work

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

type privateReleaseTagFixture struct {
	publication                 *privatePublisherFixture
	request                     PrivateSourcePublishRequest
	completed                   ReceiptedActionResult
	tagSHA                      string
	pushes, observations        int
	args                        []string
	observationError, pushError bool
	loseResponse, malformed     bool
	beforePush, afterPush       func()
	afterObservation            func()
}

func newPrivateReleaseTagFixture(t *testing.T, linked bool) *privateReleaseTagFixture {
	t.Helper()
	p := newPrivatePublisherFixture(t, linked)
	publication := p.plan(t)
	approvePrivatePublication(t, p, publication)
	result, err := p.f.store.ApplyPrivateSourcePublication(context.Background(), publication, time.Now())
	if err != nil || result.Action == nil {
		t.Fatalf("source publication prerequisite: %+v %v", result, err)
	}
	f := &privateReleaseTagFixture{publication: p, request: publication, completed: *result.Action}
	previous := privateSourcePublishCommand
	privateSourcePublishCommand = func(ctx context.Context, repo, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 6 && args[6] == "ls-remote" {
			f.observations++
			if f.afterObservation != nil {
				f.afterObservation()
			}
			if f.observationError {
				return nil, fmt.Errorf("synthetic offline tag observation")
			}
			if f.malformed {
				return []byte(f.publication.f.source.HeadSHA + "\trefs/tags/v1.2.3\n" + f.publication.f.source.BaseSHA + "\trefs/tags/extra\n"), nil
			}
			want := append(privateReleaseGitArguments("ls-remote"), "--refs", "--", publication.Destination.CanonicalURL, "refs/tags/v1.2.3")
			if !reflect.DeepEqual(args, want) {
				return nil, fmt.Errorf("tag observation broadened exact transport/ref: %q", args)
			}
			if f.tagSHA == "" {
				return nil, nil
			}
			return []byte(f.tagSHA + "\trefs/tags/v1.2.3\n"), nil
		}
		if name == "git" && len(args) > 6 && args[6] == "push" && strings.Contains(args[len(args)-1], ":refs/tags/") {
			f.pushes++
			f.args = append([]string(nil), args...)
			if f.beforePush != nil {
				f.beforePush()
			}
			if f.pushError || (f.tagSHA != "" && f.tagSHA != publication.Source.HeadSHA) {
				return nil, fmt.Errorf("synthetic non-force push refused")
			}
			f.tagSHA = publication.Source.HeadSHA
			if f.afterPush != nil {
				f.afterPush()
			}
			if f.loseResponse {
				return nil, fmt.Errorf("synthetic response lost after effect")
			}
			return nil, nil
		}
		return previous(ctx, repo, name, args...)
	}
	t.Cleanup(func() { privateSourcePublishCommand = previous })
	return f
}

func (f *privateReleaseTagFixture) plan(t *testing.T) PrivateReleaseTagRequest {
	t.Helper()
	r, err := f.publication.f.store.PrivateReleaseTagPlan(context.Background(), f.request, "v1.2.3", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *privateReleaseTagFixture) approve(t *testing.T, request PrivateReleaseTagRequest) {
	t.Helper()
	s := f.publication.f.store
	if _, err := s.ApplyPrivateReleaseTag(context.Background(), request, time.Now()); err == nil || f.pushes != 0 {
		t.Fatal("source publication approval authorized a distinct release tag")
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		t.Fatal(err)
	}
	approvals := policy.NewExactActionApprovalStore(s.home)
	plan, _, err := approvals.Plan(request.Action.ID, cfg, 5*time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := approvals.Approve(plan.ID, "operator:synthetic-reviewer", cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateReleaseTagRequiresSeparateExactApprovalAndPublishesOnlyOneImmutableRef(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprint(linked), func(t *testing.T) {
			f := newPrivateReleaseTagFixture(t, linked)
			s := f.publication.f.store
			before := snapshotHome(t, s.home)
			request := f.plan(t)
			if snapshotHome(t, s.home) != before || f.pushes != 0 || f.publication.pushes != 1 {
				t.Fatal("release tag planning changed native state or published anything")
			}
			if request.Action.Capability != PrivateReleaseTagCapability || request.Action.Action != PrivateReleaseTagAction || request.Action.MutationClass != "public" || request.Action.ID == f.request.Action.ID || request.PublicationReceiptID != f.completed.Receipt.ID {
				t.Fatal("release tag did not bind its own action and original native receipt")
			}
			f.approve(t, request)
			result, err := s.ApplyPrivateReleaseTag(context.Background(), request, time.Now())
			if err != nil || result.Status != "published" || result.Action == nil || !result.Action.Receipt.StateDelta.Changed || f.pushes != 1 || f.publication.pushes != 1 {
				t.Fatalf("exact tag failed: %+v %v pushes=%d", result, err, f.pushes)
			}
			want := []string{"-c", "http.followRedirects=false", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", "push", "--porcelain", "--no-follow-tags", "--recurse-submodules=no", "--", request.Destination.CanonicalURL, request.Publication.Source.HeadSHA + ":refs/tags/v1.2.3"}
			if !reflect.DeepEqual(f.args, want) {
				t.Fatalf("release tag transport broadened: %q", f.args)
			}
			if linked {
				body, err := os.ReadFile(filepath.Join(f.publication.f.canonical, "catalog.txt"))
				if err != nil || string(body) != "preserved unrelated operator work\n" {
					t.Fatal("release metadata touched canonical source WIP")
				}
			}
			replay, err := s.ApplyPrivateReleaseTag(context.Background(), request, time.Now())
			if err != nil || replay.Action == nil || !replay.Action.Replayed || f.pushes != 1 {
				t.Fatalf("tag completed replay repeated effect: %+v %v", replay, err)
			}
		})
	}
}

func TestPrivateReleaseTagIdenticalExistingTagIsObservedWithoutPush(t *testing.T) {
	f := newPrivateReleaseTagFixture(t, false)
	f.tagSHA = f.request.Source.HeadSHA
	request := f.plan(t)
	f.approve(t, request)
	result, err := f.publication.f.store.ApplyPrivateReleaseTag(context.Background(), request, time.Now())
	if err != nil || result.Action == nil || result.Action.Receipt.StateDelta.Changed || f.pushes != 0 {
		t.Fatalf("identical tag was rewritten: %+v %v", result, err)
	}
}

func TestPrivateReleaseTagReadOnlyPublicationVerificationPreservesCrashedClaim(t *testing.T) {
	for _, boundary := range []string{"work", "lease"} {
		t.Run(boundary, func(t *testing.T) {
			f := newPrivateReleaseTagFixture(t, false)
			s := f.publication.f.store
			if _, err := s.Advance(f.request.WorkID, AdvanceInput{To: "held", Actor: proofActorAlpha}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Advance(f.request.WorkID, AdvanceInput{To: "ready", Actor: "operator:synthetic-reviewer", Steal: true, StealReason: "synthetic crash-recovery fixture"}, time.Now()); err != nil {
				t.Fatal(err)
			}
			crashClaimAtBoundary(t, s, f.request.WorkID, proofActorAlpha, time.Now(), boundary)
			marker := s.claimPendingPath(f.request.WorkID)
			pending, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			before := snapshotHome(t, s.home)
			reader, err := OpenReadOnly(s.home)
			if err != nil {
				t.Fatal(err)
			}
			verified, verifyErr := reader.completedPrivateSourcePublication(f.request)
			after, readErr := os.ReadFile(marker)
			if readErr != nil || string(after) != string(pending) || snapshotHome(t, s.home) != before {
				t.Fatalf("read-only publication verification recovered or changed a crashed claim: read=%v verify=%v", readErr, verifyErr)
			}
			if verifyErr != nil || verified.Receipt.ID != f.completed.Receipt.ID {
				t.Fatalf("read-only historical receipt verification failed: %v", verifyErr)
			}
			completion, found, _, err := s.preflightReceiptedActionCompletion(f.request.WorkID, f.request.Action.ID)
			if err != nil || !found {
				t.Fatal("completed source publication disappeared", err)
			}
			if _, err := s.replayPrivateSourcePublication(f.request, completion); err != nil {
				t.Fatalf("writable replay no longer performs owned claim recovery: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("writable replay left the pending claim: %v", err)
			}
		})
	}
}

func TestPrivateReleaseTagRejectsMissingPublicationAndHonorsHistoricalCertificate(t *testing.T) {
	p := newPrivatePublisherFixture(t, false)
	if _, err := p.f.store.PrivateReleaseTagPlan(context.Background(), p.plan(t), "v1.2.3", time.Now()); err == nil || p.pushes != 0 {
		t.Fatal("unpublished source authorized release tag")
	}
	f := newPrivateReleaseTagFixture(t, false)
	future := time.Now().Add(10 * time.Minute)
	if _, err := f.publication.f.store.PrivateReleaseTagPlan(context.Background(), f.request, "v1.2.3", future); err != nil {
		t.Fatalf("historical completed publication was treated as a renewed source approval: %v", err)
	}
	if err := f.publication.f.store.validatePrivatePublication(context.Background(), f.request, future, true); err == nil {
		t.Fatal("expired historical review admitted fresh source publication")
	}
}

func TestPrivateReleaseTagRejectsFreshDestinationSourceAndRemoteDrift(t *testing.T) {
	for _, change := range []string{"public", "repository", "viewer", "push-url", "main", "offline", "extra-refs", "different-tag", "dirty-source", "receipt"} {
		t.Run(change, func(t *testing.T) {
			f := newPrivateReleaseTagFixture(t, false)
			request := f.plan(t)
			s := f.publication.f.store
			switch change {
			case "public", "repository", "viewer", "push-url":
				destination := f.request.Destination
				switch change {
				case "public":
					destination.Visibility = "public"
				case "repository":
					destination.RepositoryID++
				case "viewer":
					destination.ViewerID++
				case "push-url":
					destination.PushURL = "https://github.com/example-owner/different-fixture.git"
				}
				stubPrivateDestination(t, destination)
			case "main":
				f.publication.tip = f.request.Source.BaseSHA
			case "offline":
				f.observationError = true
			case "extra-refs":
				f.malformed = true
			case "different-tag":
				f.tagSHA = f.request.Source.BaseSHA
			case "dirty-source":
				if err := os.WriteFile(filepath.Join(f.request.Repo, "catalog.txt"), []byte("changed source\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				tamperGovernedReceipt(t, s, f.completed, func(receipt *envelope.Receipt) { receipt.StateDelta.Summary = "changed native publication receipt" })
			}
			if _, err := s.ApplyPrivateReleaseTag(context.Background(), request, time.Now()); err == nil || f.pushes != 0 {
				t.Fatalf("drift published release tag: %v pushes=%d", err, f.pushes)
			}
		})
	}
}

func TestPrivateReleaseTagRejectsChangedActionAndUnsafeVersions(t *testing.T) {
	f := newPrivateReleaseTagFixture(t, false)
	request := f.plan(t)
	for _, tag := range []string{"", "--all", "v1.2.3/extra", "refs/heads/main", "v1.2.3^{commit}"} {
		if _, err := f.publication.f.store.PrivateReleaseTagPlan(context.Background(), f.request, tag, time.Now()); err == nil {
			t.Fatalf("unsafe version accepted: %q", tag)
		}
	}
	for _, mutate := range []func(*PrivateReleaseTagRequest){
		func(r *PrivateReleaseTagRequest) { r.Publication.Source.HeadSHA = r.Publication.Source.BaseSHA },
		func(r *PrivateReleaseTagRequest) { r.PublicationReceiptID = "rec_other" },
		func(r *PrivateReleaseTagRequest) { r.ReleaseTag = "v1.2.4" },
		func(r *PrivateReleaseTagRequest) { r.Action.Capability = PrivateSourcePublishCapability },
		func(r *PrivateReleaseTagRequest) { r.Destination.RepositoryID++ },
	} {
		copy := request
		mutate(&copy)
		if _, err := f.publication.f.store.ApplyPrivateReleaseTag(context.Background(), copy, time.Now()); err == nil || f.pushes != 0 {
			t.Fatal("changed concrete release action was accepted")
		}
	}
}

func TestPrivateReleaseTagLostResponsesAndPendingRetriesNeverRepush(t *testing.T) {
	for _, condition := range []string{"lost-response", "failed-push", "concurrent-conflict", "visibility-drift"} {
		t.Run(condition, func(t *testing.T) {
			f := newPrivateReleaseTagFixture(t, false)
			request := f.plan(t)
			f.approve(t, request)
			switch condition {
			case "lost-response":
				f.loseResponse = true
			case "failed-push":
				f.pushError = true
			case "concurrent-conflict":
				f.beforePush = func() { f.tagSHA = f.request.Source.BaseSHA }
			case "visibility-drift":
				f.afterPush = func() {
					destination := f.request.Destination
					destination.Visibility = "public"
					stubPrivateDestination(t, destination)
				}
			}
			result, err := f.publication.f.store.ApplyPrivateReleaseTag(context.Background(), request, time.Now())
			if f.pushes != 1 || (err == nil) != (condition == "lost-response") {
				t.Fatalf("unexpected transport/postflight: %+v %v pushes=%d", result, err, f.pushes)
			}
			if condition != "lost-response" {
				if _, err := f.publication.f.store.ApplyPrivateReleaseTag(context.Background(), request, time.Now()); err == nil || f.pushes != 1 {
					t.Fatal("ambiguous pending action repushed or invented success")
				}
				stubPrivateDestination(t, f.request.Destination)
				f.tagSHA = f.request.Source.HeadSHA
				result, err = f.publication.f.store.ApplyPrivateReleaseTag(context.Background(), request, time.Now())
				if err != nil || result.Action == nil || f.pushes != 1 {
					t.Fatalf("observe-only recovery failed: %+v %v", result, err)
				}
			}
		})
	}
}

func TestPrivateReleaseTagRechecksAuthorityAfterFinalValidation(t *testing.T) {
	f := newPrivateReleaseTagFixture(t, false)
	request := f.plan(t)
	f.approve(t, request)
	f.observations = 0
	var admittedDecision envelope.PolicyDecision
	f.afterObservation = func() {
		if f.observations != 3 {
			return
		}
		s := f.publication.f.store
		pending, _, present, err := s.ReadReceiptedActionReservationInput(f.request.WorkID, request.Action.ID)
		if err != nil || !present || pending.PreEvaluatedDecision == nil {
			t.Fatalf("final validation did not have native reservation: %v", err)
		}
		admittedDecision = *pending.PreEvaluatedDecision
		if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{DisabledClasses: map[string]string{PrivateReleaseTagCapability: "synthetic halt after final destination validation"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.publication.f.store.ApplyPrivateReleaseTag(context.Background(), request, time.Now()); err == nil || f.pushes != 0 || f.observations != 3 {
		t.Fatalf("new halt reached transport: %v pushes=%d scans=%d", err, f.pushes, f.observations)
	}
	if err := policy.WriteAutonomyHalt(f.publication.f.store.home, policy.AutonomyHalt{}); err != nil {
		t.Fatal(err)
	}
	expiry, err := time.Parse(time.RFC3339Nano, admittedDecision.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.publication.f.store.recheckPrivateReleaseTag(request, admittedDecision, expiry.Add(time.Second)); err == nil {
		t.Fatal("expired exact decision was accepted")
	}
}

func TestPrivateReleaseTagRealGitPushSuppressesExtraTagsAndRefusesRewrite(t *testing.T) {
	p := newPrivatePublisherFixture(t, false)
	f := p.f
	bare := filepath.Join(t.TempDir(), "remote.git")
	runProofSourceGit(t, f.repo, "init", "--bare", bare)
	runProofSourceGit(t, f.repo, "config", "push.followTags", "true")
	runProofSourceGit(t, f.repo, "tag", "-a", "extra-annotated", "-m", "synthetic extra tag", f.source.HeadSHA)
	request := PrivateReleaseTagRequest{ReleaseTag: "v1.2.3", Publication: PrivateSourcePublishRequest{Source: f.source}, Destination: f.review.Destination}
	args := privateReleaseTagPushArguments(request)
	args[len(args)-2] = bare
	runProofSourceGit(t, f.repo, args...)
	refs := proofSourceGitOutput(t, f.repo, "--git-dir", bare, "for-each-ref", "--format=%(refname) %(objectname) %(objecttype)")
	if refs != "refs/tags/v1.2.3 "+f.source.HeadSHA+" commit" {
		t.Fatalf("tag push broadened refs or created an annotated tag: %s", refs)
	}
	conflict := filepath.Join(t.TempDir(), "conflict.git")
	runProofSourceGit(t, f.repo, "init", "--bare", conflict)
	runProofSourceGit(t, f.repo, "push", "--no-follow-tags", conflict, f.source.BaseSHA+":refs/tags/v1.2.3")
	args[len(args)-2] = conflict
	if _, err := runPrivateSourceCommand(context.Background(), f.repo, "git", args...); err == nil {
		t.Fatal("non-force release tag rewrote an existing tag")
	}
	if got := proofSourceGitOutput(t, f.repo, "--git-dir", conflict, "rev-parse", "refs/tags/v1.2.3"); got != f.source.BaseSHA {
		t.Fatal("conflicting immutable tag changed")
	}
}
