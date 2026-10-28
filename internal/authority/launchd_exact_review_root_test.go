package authority

// Independently authored acceptance case for the real L3 admission failure.
// The existing effect simulator ensures this test never invokes a user service.
import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

func TestLaunchdRootL3ReviewRequiresConsumedApprovalBeforeRecording(t *testing.T) {
	f := newAdversarialLiveFixture(t)
	operatorSource := filepath.Join(t.TempDir(), "operator.json")
	adversarialWriteJSON(t, operatorSource, map[string]any{
		"autonomy_lever": map[string]any{"level": "L3", "set_by": "operator:fixture", "set_at": f.now.Format(time.RFC3339)},
	})
	if _, err := policy.ImportOperatorState(f.home, operatorSource, f.now); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	runtimeRun := f.f.env.Run
	f.f.env.Run = func(ctx context.Context, exe string, args ...string) (LaunchdCommandResult, error) {
		if exe == "/usr/bin/plutil" {
			body, err := os.ReadFile(args[len(args)-1])
			return LaunchdCommandResult{Stdout: string(body)}, err
		}
		return runtimeRun(ctx, exe, args...)
	}
	var err error
	f.plan, err = BuildLaunchdRepairPlan(context.Background(), f.f.env, f.plan.WorkID, f.plan.Actor, f.f.plist, f.f.candidate, f.f.proofs)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.StandingGrants[len(f.cfg.StandingGrants)-1].Destination = launchdPlanDestination(f.plan)
	f.writeConfig(t)
	if err = RecordLaunchdRepairPlan(f.home, f.plan, f.now); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := policy.LoadConfig(f.home)
	if err != nil || cfg.OperatorAutonomyLevel != "L3" || cfg.OperatorAutonomyAuthorityRef != policy.OperatorStateAuthorityRef {
		t.Fatalf("fixture did not load actual L3 authority: level=%s ref=%s err=%v", cfg.OperatorAutonomyLevel, cfg.OperatorAutonomyAuthorityRef, err)
	}

	seal := func(rollback bool) LaunchdRepairReview {
		t.Helper()
		fn := ReviewLaunchdRepairPlan
		if rollback {
			fn = ReviewLaunchdRepairRollbackPlan
		}
		before, err := f.store.Get(f.plan.WorkID)
		if err != nil {
			t.Fatal(err)
		}
		effectsBefore := append([]string(nil), f.effects...)
		expires := f.now.Add(10 * time.Minute)
		pending, err := fn(f.home, f.plan.ID, "grant_example_launchd", "operator:fixture", "fixture:root-l3-review", expires, f.now)
		if err != nil || !pending.Pending || pending.Review != nil {
			t.Fatalf("expected pending exact review: result=%+v err=%v", pending, err)
		}
		after, err := f.store.Get(f.plan.WorkID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("L3 recorded a review before exact approval: before=%s after=%s err=%v", before.PolicyDecisionID, after.PolicyDecisionID, err)
		}
		retry, err := fn(f.home, f.plan.ID, "grant_example_launchd", "operator:fixture", "fixture:root-l3-review", expires, f.now)
		if err != nil || !retry.Pending || retry.ActionIntentID != pending.ActionIntentID || !reflect.DeepEqual(effectsBefore, f.effects) {
			t.Fatalf("pending retry changed identity or effects: %+v err=%v effects=%v", retry, err, f.effects)
		}
		body, err := os.ReadFile(pending.IntentPath)
		if err != nil {
			t.Fatal(err)
		}
		var intent envelope.ActionIntent
		if err = json.Unmarshal(body, &intent); err != nil {
			t.Fatal(err)
		}
		ap := policy.NewExactActionApprovalStore(f.home)
		approvalPlan, _, _, err := ap.EnsurePlanForIntent(intent, cfg, 5*time.Minute, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = ap.Approve(approvalPlan.ID, "operator:fixture", cfg, f.now); err != nil {
			t.Fatal(err)
		}
		recorded, err := f.store.EvaluateAndRecordAction(f.plan.WorkID, intent, f.now)
		if err != nil || !recorded.Recorded || !policy.Allowed(recorded.Decision) {
			t.Fatalf("authentic exact approval failed: result=%+v err=%v", recorded, err)
		}
		if err = ap.ValidateConsumedDecision(intent, recorded.Decision); err != nil {
			t.Fatalf("review did not consume authentic exact approval: %v", err)
		}
		if _, err = f.store.EvaluateAndRecordAction(f.plan.WorkID, intent, f.now); err == nil || !strings.Contains(err.Error(), "already recorded") {
			t.Fatalf("duplicate review recording must refuse: %v", err)
		}
		sealed, err := fn(f.home, f.plan.ID, "grant_example_launchd", "operator:fixture", "fixture:root-l3-review", expires, f.now)
		if err != nil || sealed.Pending || sealed.Review == nil {
			t.Fatalf("authentic review did not seal: result=%+v err=%v", sealed, err)
		}
		return *sealed.Review
	}
	f.review = seal(false)
	if len(f.effects) != 0 || f.loaded || !f.disabled {
		t.Fatal("review performed an OS effect")
	}
	result, err := f.apply()
	if err != nil || len(result.Phases) != 3 || !f.loaded || f.disabled {
		t.Fatalf("approved exact apply failed: result=%+v err=%v", result, err)
	}
	if _, err = f.apply(); err != nil || !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap"}) {
		t.Fatalf("apply replay repeated effects: %v err=%v", f.effects, err)
	}
	rollbackReview := seal(true)
	if _, err = RollbackLaunchdRepair(context.Background(), f.home, f.plan.ID, rollbackReview.ID, f.plan.Actor, f.f.env); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(f.plan.PlistPath)
	if err != nil || f.loaded || !f.disabled || !bytesEqual(actual, f.plan.Original) || !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap", "bootout", "disable"}) {
		t.Fatalf("L3 rollback was not exact: effects=%v err=%v", f.effects, err)
	}
}

func TestLaunchdRootLegacyConsumedReviewKeepsPhaseProofValid(t *testing.T) {
	f := newAdversarialLiveFixture(t)
	cfg, _, err := policy.LoadConfig(f.home)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.store.Get(f.plan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := launchdExactGrant(f.plan, cfg, "grant_example_launchd", f.now)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the pre-v2 request shape in a synthetic home, then obtain
	// real native approval and action records. No consumed decision is forged.
	req := LaunchdReviewRequest{Purpose: "apply", PlanID: f.plan.ID, PlanDigest: f.plan.Digest,
		GrantID: grant.ID, Grant: grant, ConfigRevision: launchdDigest(cfg), ReviewedBy: "operator:fixture",
		EvidenceRef: "fixture:historical-native-review", ExpiresAt: f.now.Add(10 * time.Minute), BeforeWork: before}
	req.Action = launchdReviewRequestAction(f.plan, req)
	proposal, err := f.store.EvaluateAndRecordAction(f.plan.WorkID, req.Action, f.now)
	if err != nil || proposal.Recorded || proposal.Decision.Outcome != "hold" {
		t.Fatalf("legacy review must begin held: result=%+v err=%v", proposal, err)
	}
	ap := policy.NewExactActionApprovalStore(f.home)
	plan, _, _, err := ap.EnsurePlanForIntent(req.Action, cfg, 5*time.Minute, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ap.Approve(plan.ID, req.ReviewedBy, cfg, f.now); err != nil {
		t.Fatal(err)
	}
	recorded, err := f.store.EvaluateAndRecordAction(f.plan.WorkID, req.Action, f.now)
	if err != nil || !recorded.Recorded {
		t.Fatalf("native legacy approval did not record: %+v err=%v", recorded, err)
	}
	if err = ap.ValidateConsumedDecision(req.Action, recorded.Decision); err != nil {
		t.Fatal(err)
	}
	legacy := LaunchdRepairReview{Request: req, Decision: recorded.Decision, WorkSHA256: launchdWorkDigest(recorded.Work)}
	legacy.ID = "ldrr_" + launchdDigest(legacy)[:24]
	if err = writeLaunchdArtifact(f.home, legacy.ID+".review.json", legacy); err != nil {
		t.Fatal(err)
	}
	if err = validateLaunchdReviewEvidence(f.home, f.plan, legacy); err != nil {
		t.Fatalf("actual legacy approval evidence was invalidated: %v", err)
	}
	f.review = legacy
	first, err := f.apply()
	if err != nil || len(first.Phases) != 3 {
		t.Fatalf("legacy consumed review could not produce phases: %+v err=%v", first, err)
	}
	replayed, err := f.apply()
	if err != nil || !reflect.DeepEqual(first.Phases, replayed.Phases) || !reflect.DeepEqual(f.effects, []string{"enable", "bootstrap"}) {
		t.Fatalf("legacy phase proofs did not validate on replay: %+v err=%v effects=%v", replayed, err, f.effects)
	}
}
