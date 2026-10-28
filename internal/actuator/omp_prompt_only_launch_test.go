package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

type ompPromptOnlyProducerFixture struct {
	homePath, workspace, runtimeRoot, projectionPath string
	profileID, requestedModel, route, prompt         string
	producerActor, recipeRef, proofRef               string
	proof                                            harness.OMPNativePublicProof
	store                                            *work.Store
	now                                              time.Time
}

func runOMPPromptOnlyConsumerIntegration(t *testing.T, f ompPromptOnlyProducerFixture) {
	t.Helper()
	t.Run("foreign_home_store_refused_before_advance", func(t *testing.T) {
		const actor = "omp-consumer-home-identity-test"
		foreignHome, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := home.Init(foreignHome, "test", f.now); err != nil {
			t.Fatal(err)
		}
		foreignStore, err := work.Open(foreignHome)
		if err != nil {
			t.Fatal(err)
		}
		claimed := claimOMPPromptOnlyWork(t, foreignStore, actor, f.now.Add(time.Second))
		beforeActions := append([]string(nil), claimed.ActionIntentIDs...)
		adapter := harness.NewOMPPromptOnlyAdapter(runattempt.Open(f.homePath))
		prepared := preflightOMPPromptOnlyConsumer(t, adapter, claimed.ID, f, f.requestedModel, []string{f.proofRef})
		_, err = ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
			HomePath: f.homePath, Store: foreignStore, ClaimedWork: claimed, Adapter: adapter,
			RunInput: prepared, Actor: actor, Target: "running", Now: f.now.Add(2 * time.Second),
		})
		assertOMPPromptOnlyPreAdvanceRefusal(t, foreignStore, claimed, beforeActions, err)
	})
	for _, tc := range []struct {
		name string
		refs []string
	}{
		{"absent_public_proof", nil},
		{"only_unrelated_refs", []string{"route:synthetic-unrelated", f.recipeRef}},
		{"duplicate_public_proof", []string{f.proofRef, f.proofRef}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actor := "omp-consumer-proof-selection-" + tc.name
			claimed := claimOMPPromptOnlyWork(t, f.store, actor, f.now.Add(time.Second))
			beforeActions := append([]string(nil), claimed.ActionIntentIDs...)
			adapter := harness.NewOMPPromptOnlyAdapter(runattempt.Open(f.homePath))
			prepared := preflightOMPPromptOnlyConsumer(t, adapter, claimed.ID, f, f.requestedModel, tc.refs)
			_, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
				HomePath: f.homePath, Store: f.store, ClaimedWork: claimed, Adapter: adapter,
				RunInput: prepared, Actor: actor, Target: "running", Now: f.now.Add(2 * time.Second),
			})
			assertOMPPromptOnlyPreAdvanceRefusal(t, f.store, claimed, beforeActions, err)
		})
	}
	t.Run("prepared_work_identity_mismatch_refused_before_advance", func(t *testing.T) {
		const actor = "omp-consumer-work-identity-test"
		claimed := claimOMPPromptOnlyWork(t, f.store, actor, f.now.Add(time.Second))
		beforeActions := append([]string(nil), claimed.ActionIntentIDs...)
		adapter := harness.NewOMPPromptOnlyAdapter(runattempt.Open(f.homePath))
		prepared := preflightOMPPromptOnlyConsumer(t, adapter, f.proof.WorkID, f, f.requestedModel, []string{f.proofRef})
		_, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
			HomePath: f.homePath, Store: f.store, ClaimedWork: claimed, Adapter: adapter,
			RunInput: prepared, Actor: actor, Target: "running", Now: f.now.Add(2 * time.Second),
		})
		assertOMPPromptOnlyPreAdvanceRefusal(t, f.store, claimed, beforeActions, err)
	})
	t.Run("genuine_cross_work_consumer", func(t *testing.T) {
		const actor = "omp-consumer-test"
		claimed := claimOMPPromptOnlyWork(t, f.store, actor, f.now.Add(time.Second))
		adapter := harness.NewOMPPromptOnlyAdapter(runattempt.Open(f.homePath))
		wantRefs := []string{"route:synthetic-unrelated", f.recipeRef, f.proofRef}
		prepared := preflightOMPPromptOnlyConsumer(t, adapter, claimed.ID, f, f.requestedModel, wantRefs)
		reserved, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
			HomePath: f.homePath, Store: f.store, ClaimedWork: claimed, Adapter: adapter,
			RunInput: prepared, Actor: actor, Target: "running", Note: "genuine prompt-only consumer",
			Now: f.now.Add(2 * time.Second),
		})
		if err != nil {
			t.Fatalf("ReserveGovernedHarnessLaunch: %v", err)
		}
		prepared.ProofRefs[0] = "route:mutated-after-reservation"
		if !reflect.DeepEqual(reserved.RunInput.ProofRefs, wantRefs) {
			t.Fatalf("reserved ProofRefs = %v, want immutable %v", reserved.RunInput.ProofRefs, wantRefs)
		}
		out, runErr := adapter.Run(context.Background(), reserved.RunInput)
		finalized := finalizeOMPPromptOnlyRun(t, f.store, claimed, actor, adapter.Name(), reserved, out, runErr, false)
		if runErr != nil || !finalized.Succeeded || finalized.FinalState != "verifying" {
			t.Fatalf("consumer run=%v finalization=%+v", runErr, finalized)
		}
		if claimed.ID == f.proof.WorkID || out.RunAttempt.WorkID != claimed.ID ||
			out.RunAttempt.ActionIntentID != reserved.ActionIntentID ||
			out.RunAttempt.Harness != "omp-prompt-only" ||
			out.RunAttempt.Status != runattempt.StatusSucceeded {
			t.Fatalf("consumer RunAttempt identity = %+v", out.RunAttempt)
		}
		assertOMPPromptOnlyPosture(t, out.RunAttempt, reserved.RunningWork.PolicyDecisionID)
		if out.RunAttempt.Launch == nil || out.RunAttempt.Launch.PreparedAt == nil ||
			out.RunAttempt.Launch.CommittedAt == nil || out.RunAttempt.Launch.StartErrorAt != nil {
			t.Fatalf("consumer launch = %+v", out.RunAttempt.Launch)
		}
		if !reflect.DeepEqual(out.RunAttempt.ProofRefs, wantRefs) {
			t.Fatalf("consumer ProofRefs = %v, want %v", out.RunAttempt.ProofRefs, wantRefs)
		}
	})

	t.Run("forged_public_proof_refused_before_advance", func(t *testing.T) {
		const actor = "omp-consumer-forgery-test"
		forged := f.proof
		forged.ActionIntentID = "act_forged_prompt_only"
		forgedRef := putOMPPromptOnlyProof(t, f.homePath, forged)
		claimed := claimOMPPromptOnlyWork(t, f.store, actor, f.now.Add(3*time.Second))
		beforeActions := append([]string(nil), claimed.ActionIntentIDs...)
		adapter := harness.NewOMPPromptOnlyAdapter(runattempt.Open(f.homePath))
		prepared := preflightOMPPromptOnlyConsumer(t, adapter, claimed.ID, f, f.requestedModel, []string{forgedRef})
		_, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
			HomePath: f.homePath, Store: f.store, ClaimedWork: claimed, Adapter: adapter,
			RunInput: prepared, Actor: actor, Target: "running", Note: "forged proof refusal",
			Now: f.now.Add(4 * time.Second),
		})
		assertOMPPromptOnlyPreAdvanceRefusal(t, f.store, claimed, beforeActions, err)
	})

	t.Run("valid_profile_a_cannot_consume_coexisting_profile_b_proof", func(t *testing.T) {
		const actor = "omp-consumer-profile-confusion-test"
		profileARoot := installCoexistingOMPPromptOnlyRuntime(t, f, "test/profile-a")
		t.Setenv("SWITCHBACK_RUNTIME_ROOT", profileARoot)
		claimed := claimOMPPromptOnlyWork(t, f.store, actor, f.now.Add(5*time.Second))
		beforeActions := append([]string(nil), claimed.ActionIntentIDs...)
		adapter := harness.NewOMPPromptOnlyAdapter(runattempt.Open(f.homePath))
		prepared := preflightOMPPromptOnlyConsumer(t, adapter, claimed.ID, f, "test/profile-a", []string{f.proofRef})
		// Keep the B proof valid at the current default while the consumer remains prepared for A.
		t.Setenv("SWITCHBACK_RUNTIME_ROOT", f.runtimeRoot)
		if err := harness.ValidateOMPNativeProofCustody(f.homePath, f.proof); err != nil {
			t.Fatalf("B proof is not independently valid: %v", err)
		}
		_, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
			HomePath: f.homePath, Store: f.store, ClaimedWork: claimed, Adapter: adapter,
			RunInput: prepared, Actor: actor, Target: "running", Note: "profile confusion refusal",
			Now: f.now.Add(6 * time.Second),
		})
		assertOMPPromptOnlyPreAdvanceRefusal(t, f.store, claimed, beforeActions, err)
		if _, err := os.Stat(f.projectionPath); err != nil {
			t.Fatalf("profile B projection no longer coexists: %v", err)
		}
	})

	t.Run("coexisting_owner_rotation_refused_inside_start_guard", func(t *testing.T) {
		const actor = "omp-consumer-stale-guard-test"
		claimed := claimOMPPromptOnlyWork(t, f.store, actor, f.now.Add(7*time.Second))
		adapter := harness.NewOMPPromptOnlyAdapter(runattempt.Open(f.homePath))
		prepared := preflightOMPPromptOnlyConsumer(t, adapter, claimed.ID, f, f.requestedModel, []string{f.proofRef})
		reserved, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
			HomePath: f.homePath, Store: f.store, ClaimedWork: claimed, Adapter: adapter,
			RunInput: prepared, Actor: actor, Target: "running", Note: "stale proof guard",
			Now: f.now.Add(8 * time.Second),
		})
		if err != nil {
			t.Fatalf("initial reservation: %v", err)
		}
		profileARoot := installCoexistingOMPPromptOnlyRuntime(t, f, "test/profile-after-reservation")
		original, err := os.ReadFile(f.projectionPath)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := os.ReadFile(filepath.Join(profileARoot, "state", "profile-conformance", f.profileID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(f.projectionPath, original, 0o600); err != nil {
				t.Error(err)
			}
		})
		if err := os.WriteFile(f.projectionPath, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		started := false
		err = reserved.RunInput.StartGuard(context.Background(), func() error {
			started = true
			return nil
		})
		if !errors.Is(err, errOMPPromptOnlyAdmission) || started {
			t.Fatalf("stale guard err=%v started=%v", err, started)
		}
	})

	t.Run("same_work_prior_action_allowed_current_action_self_proof_refused", func(t *testing.T) {
		producerWork, err := f.store.Get(f.proof.WorkID)
		if err != nil || producerWork.State != "verifying" {
			t.Fatalf("producer work before resume = %+v err=%v", producerWork, err)
		}
		adapter := harness.NewOMPPromptOnlyAdapter(runattempt.Open(f.homePath))
		prepared := preflightOMPPromptOnlyConsumer(t, adapter, producerWork.ID, f, f.requestedModel, []string{f.proofRef})
		reserved, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
			HomePath: f.homePath, Store: f.store, ClaimedWork: producerWork, Adapter: adapter,
			RunInput: prepared, Actor: f.producerActor, Target: "running",
			Note: "same-work prompt-only continuation", ResumeVerifying: true,
			Now: f.now.Add(9 * time.Second),
		})
		if err != nil {
			t.Fatalf("same-work continuation reservation: %v", err)
		}
		if reserved.ActionIntentID == f.proof.ActionIntentID {
			t.Fatal("resume reused producer action")
		}
		selfProof := f.proof
		selfProof.ActionIntentID = reserved.ActionIntentID
		selfInput := prepared
		selfInput.ProofRefs = []string{putOMPPromptOnlyProof(t, f.homePath, selfProof)}
		if err := validateOMPPromptOnlyAdmission(f.homePath, selfInput, reserved.ActionIntentID); !errors.Is(err, errOMPPromptOnlyAdmission) {
			t.Fatalf("current-action self-proof error = %v, want refusal", err)
		}
		out, runErr := adapter.Run(context.Background(), reserved.RunInput)
		finalized := finalizeOMPPromptOnlyRun(t, f.store, producerWork, f.producerActor, adapter.Name(), reserved, out, runErr, true)
		if runErr != nil || !finalized.Succeeded || finalized.FinalState != "verifying" {
			t.Fatalf("same-work run=%v finalization=%+v", runErr, finalized)
		}
	})

	t.Run("name_only_impostor_refused_before_advance", func(t *testing.T) {
		const actor = "omp-consumer-impostor-test"
		claimed := claimOMPPromptOnlyWork(t, f.store, actor, f.now.Add(10*time.Second))
		beforeActions := append([]string(nil), claimed.ActionIntentIDs...)
		_, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
			HomePath: f.homePath, Store: f.store, ClaimedWork: claimed, Adapter: ompPromptOnlyImpostorAdapter{},
			RunInput: harness.RunInput{WorkID: claimed.ID, Harness: "omp-prompt-only", Workspace: f.workspace, Prompt: f.prompt, ProofRefs: []string{f.proofRef}, Now: f.now},
			Actor:    actor, Target: "running", Note: "impostor refusal", Now: f.now.Add(11 * time.Second),
		})
		assertOMPPromptOnlyPreAdvanceRefusal(t, f.store, claimed, beforeActions, err)
	})
}

func preflightOMPPromptOnlyConsumer(t *testing.T, adapter *harness.OMPPromptOnlyAdapter, workID string, f ompPromptOnlyProducerFixture, model string, refs []string) harness.RunInput {
	t.Helper()
	prepared, err := adapter.Preflight(context.Background(), harness.RunInput{
		WorkID: workID, Harness: adapter.Name(), Workspace: f.workspace, Prompt: f.prompt,
		RequestedModelID: model, RequestedProviderRoute: f.route,
		ProofRefs: append([]string(nil), refs...), Timeout: time.Minute, Now: f.now,
	})
	if err != nil {
		t.Fatalf("OMPPromptOnlyAdapter.Preflight: %v", err)
	}
	return prepared
}

func claimOMPPromptOnlyWork(t *testing.T, store *work.Store, actor string, now time.Time) envelope.WorkEnvelope {
	t.Helper()
	created, err := store.Create(work.CreateInput{Owner: actor, SuccessPredicate: "synthetic governed prompt-only consumer"}, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(created.ID, actor, now)
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func finalizeOMPPromptOnlyRun(t *testing.T, store *work.Store, prior envelope.WorkEnvelope, actor, harnessName string, reserved GovernedHarnessReservation, out harness.RunOutput, runErr error, resume bool) GovernedHarnessFinalizationResult {
	t.Helper()
	finalized, err := FinalizeGovernedHarnessRun(GovernedHarnessFinalizationInput{
		Store: store, WorkID: prior.ID, Actor: actor, HarnessName: harnessName,
		RunningPolicyID: reserved.RunningWork.PolicyDecisionID, RunningActionIntentID: reserved.ActionIntentID,
		ExistingRunAttemptIDs: prior.RunAttemptIDs, ResumeVerifying: resume,
		Output: out, RunError: runErr, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("FinalizeGovernedHarnessRun: %v", err)
	}
	return finalized
}

func assertOMPPromptOnlyPosture(t *testing.T, attempt runattempt.RunAttempt, policyID string) {
	t.Helper()
	p := attempt.ExecutionPosture
	if p == nil || p.PermissionMode != "prompt_only" || p.PolicyDecisionID != policyID || len(p.AuthorityEvidence) != 1 {
		t.Fatalf("consumer posture = %+v", p)
	}
	m := p.AuthorityEvidence[0]
	if m.Kind != "omp_prompt_only" || m.Ref != "harness:omp-prompt-only" || m.Revision != "v0.1" {
		t.Fatalf("consumer marker = %+v", m)
	}
}

func assertOMPPromptOnlyPreAdvanceRefusal(t *testing.T, store *work.Store, claimed envelope.WorkEnvelope, beforeActions []string, gotErr error) {
	t.Helper()
	if !errors.Is(gotErr, errOMPPromptOnlyAdmission) {
		t.Fatalf("error = %v, want content-free refusal", gotErr)
	}
	after, err := store.Get(claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "claimed" || !reflect.DeepEqual(after.ActionIntentIDs, beforeActions) {
		t.Fatalf("refusal changed work: %+v", after)
	}
}

func putOMPPromptOnlyProof(t *testing.T, homePath string, proof harness.OMPNativePublicProof) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schema_version": proof.SchemaVersion, "work_id": proof.WorkID,
		"run_attempt_id": proof.RunAttemptID, "action_intent_id": proof.ActionIntentID,
		"report_sha256": proof.ReportSHA256, "private_custody_sha256": proof.PrivateCustodySHA256,
		"evidence_refs": proof.EvidenceRefs,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func installCoexistingOMPPromptOnlyRuntime(t *testing.T, f ompPromptOnlyProducerFixture, model string) string {
	t.Helper()
	body, err := os.ReadFile(f.projectionPath)
	if err != nil {
		t.Fatal(err)
	}
	var p harness.OMPOwnerProjection
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	p.Profile.RequestedModel, p.Profile.RequestModel = model, model
	if len(p.PromptOnly.FixedArgv) < 3 {
		t.Fatal("OMP owner projection fixed argv is incomplete")
	}
	p.PromptOnly.FixedArgv[2] = "--model=switchback/" + model
	if err := p.Validate(f.profileID); err != nil {
		t.Fatalf("coexisting OMP projection: %v", err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	projectionRoot := filepath.Join(runtimeRoot, "state", "profile-conformance")
	if err := os.MkdirAll(projectionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectionRoot, f.profileID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return runtimeRoot
}

type ompPromptOnlyImpostorAdapter struct{}

func (ompPromptOnlyImpostorAdapter) Name() string { return "omp-prompt-only" }
func (ompPromptOnlyImpostorAdapter) Run(context.Context, harness.RunInput) (harness.RunOutput, error) {
	panic("name-only OMP prompt-only impostor must never run")
}
