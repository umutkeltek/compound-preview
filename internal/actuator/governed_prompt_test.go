package actuator

import (
	"context"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

type goalCaptureAdapter struct {
	attempts *runattempt.Store
	prompts  []string
}

func (a *goalCaptureAdapter) Name() string { return "claude" }

func (a *goalCaptureAdapter) GovernedPermissionPosture() {}

func (a *goalCaptureAdapter) RevalidateGovernedRun(_ context.Context, in harness.RunInput) (harness.RunInput, error) {
	return in, nil
}

func TestRegistryClaudeLanesSupportGovernedGoalPrompt(t *testing.T) {
	registry := harness.NewAdapterRegistry(runattempt.Open(t.TempDir()))
	seen := map[string]bool{}
	for _, name := range registry.Names() {
		adapter, ok := registry.AdapterByName(name)
		if !ok {
			t.Fatalf("registered adapter %q missing", name)
		}
		_, permissionAdapter := adapter.(harness.GovernedPermissionAdapter)
		if strings.HasPrefix(name, "claude") {
			if !permissionAdapter {
				t.Fatalf("Claude lane %q will miss goal handoff", name)
			}
			seen[name] = true
		} else if permissionAdapter {
			t.Fatalf("non-Claude lane %q unexpectedly entered goal handoff", name)
		}
	}
	for _, name := range []string{"claude", "claude_minimax", "claude_zai_full"} {
		if !seen[name] {
			t.Fatalf("production Claude lane %q absent from registry", name)
		}
	}
}

func (a *goalCaptureAdapter) Run(_ context.Context, in harness.RunInput) (harness.RunOutput, error) {
	a.prompts = append(a.prompts, in.Prompt)
	now := time.Now().UTC()
	attempt, err := a.attempts.MintWithActionIntent(in.WorkID, "goal-capture-test", a.Name(), in.ActionIntentID, now)
	if err != nil {
		return harness.RunOutput{}, err
	}
	if _, err := a.attempts.SetOrigin(attempt.ID, runattempt.OriginHarnessExecution); err != nil {
		return harness.RunOutput{}, err
	}
	attempt, err = a.attempts.Complete(attempt.ID, runattempt.StatusSucceeded, now.Add(time.Second))
	if err != nil {
		return harness.RunOutput{}, err
	}
	return harness.RunOutput{RunAttempt: attempt, ExitCode: 0}, nil
}

func TestGovernedAdapterReceivesCurrentGoalOnRunAndResume(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(work.CreateInput{
		Owner: "goal-capture-test", DesiredOutcome: "produce a bounded audit",
		SuccessPredicate: "accepted audit names the current goal",
		NonGoals:         []string{"do not browse global profile files"},
		ExecutionSelection: &envelope.ExecutionSelection{
			LaunchProfileID: "fixture-goal-profile", Harness: "claude",
			Source: "explicit", Revision: 1, SelectedBy: "goal-capture-test",
			SelectedAt: now.Format(time.RFC3339),
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(created.ID, "goal-capture-test", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	adapter := &goalCaptureAdapter{attempts: runattempt.Open(homePath)}
	first, err := ExecuteGovernedHarnessAdvance(context.Background(), homePath, store, claimed, adapter,
		harness.RunInput{WorkID: claimed.ID, Harness: adapter.Name(), Workspace: t.TempDir(), Prompt: "audit the selected work", PermissionMode: "plan", Now: now},
		"goal-capture-test", "running", "first audit", claimed.RunAttemptIDs, false, now.Add(2*time.Second))
	if err != nil || first.RunError != nil {
		t.Fatalf("first execution: %v / %v", err, first.RunError)
	}
	if first.Finalized.FinalState != "verifying" {
		t.Fatalf("first state = %q", first.Finalized.FinalState)
	}
	verifying, err := store.Get(claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
		HomePath: homePath, Store: store, ClaimedWork: claimed, Adapter: adapter,
		RunInput: harness.RunInput{WorkID: claimed.ID, Harness: adapter.Name(), Prompt: "stale goal"},
		Actor:    "goal-capture-test", Target: "running", ResumeVerifying: true, Now: now.Add(3 * time.Second),
	}); err == nil {
		t.Fatal("stale claimed envelope was accepted for resume")
	}
	second, err := ExecuteGovernedHarnessAdvance(context.Background(), homePath, store, verifying, adapter,
		harness.RunInput{WorkID: verifying.ID, Harness: adapter.Name(), Workspace: t.TempDir(), Prompt: "continue with the new audit focus", PermissionMode: "plan", Now: now.Add(3 * time.Second)},
		"goal-capture-test", "running", "resume audit", verifying.RunAttemptIDs, true, now.Add(3*time.Second))
	if err != nil || second.RunError != nil {
		t.Fatalf("resume execution: %v / %v", err, second.RunError)
	}
	if len(adapter.prompts) != 2 {
		t.Fatalf("adapter captured %d prompts, want 2", len(adapter.prompts))
	}
	for i, request := range []string{"audit the selected work", "continue with the new audit focus"} {
		for _, want := range []string{request, claimed.ID, "accepted audit names the current goal", "produce a bounded audit", "do not browse global profile files", "fixture-goal-profile"} {
			if !strings.Contains(adapter.prompts[i], want) {
				t.Fatalf("adapter prompt %d missing %q: %s", i, want, adapter.prompts[i])
			}
		}
	}
	if strings.Contains(adapter.prompts[1], "audit the selected work") {
		t.Fatalf("resume prompt retained stale request: %s", adapter.prompts[1])
	}
}

func TestGovernedGoalPromptNamesContextPack(t *testing.T) {
	w := envelope.WorkEnvelope{SuccessPredicate: "current proof", DesiredOutcome: "current outcome"}
	w.ID = "work_context_fixture"
	w.IDs.ContextPackID = "ctx_current_fixture"
	got := governedGoalPrompt(w, "latest instruction")
	for _, want := range []string{"work_context_fixture", "ctx_current_fixture", "current proof", "latest instruction"} {
		if !strings.Contains(got, want) {
			t.Fatalf("goal prompt missing %q: %s", want, got)
		}
	}
}

func TestGovernedCodexReservationReceivesCurrentGoal(t *testing.T) {
	now := time.Now().UTC()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(work.CreateInput{
		Owner: "goal-capture-test", DesiredOutcome: "audit the held-out metadata routes",
		SuccessPredicate: "six route verdicts with source evidence",
		NonGoals:         []string{"do not modify the product"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.Claim(created.ID, "goal-capture-test", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	adapter, ok := harness.NewAdapterRegistry(runattempt.Open(homePath)).AdapterByName("codex")
	if !ok {
		t.Fatal("production Codex adapter missing")
	}
	reserved, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
		HomePath: homePath, Store: store, ClaimedWork: claimed, Adapter: adapter,
		RunInput: harness.RunInput{WorkID: claimed.ID, Harness: "codex", Workspace: t.TempDir(), Prompt: "resume the product audit", PermissionMode: "plan"},
		Actor:    "goal-capture-test", Target: "running", Now: now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{claimed.ID, created.DesiredOutcome, created.SuccessPredicate, created.NonGoals[0], "resume the product audit"} {
		if !strings.Contains(reserved.RunInput.Prompt, want) {
			t.Fatalf("Codex prompt missing %q: %s", want, reserved.RunInput.Prompt)
		}
	}
}

func TestGovernedRawCommandKeepsCallerInput(t *testing.T) {
	for _, args := range [][]string{nil, {"raw-arg"}} {
		t.Run(strings.Join(args, ","), func(t *testing.T) {
			now := time.Now().UTC()
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", now); err != nil {
				t.Fatal(err)
			}
			store, err := work.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			created, err := store.Create(work.CreateInput{Owner: "goal-capture-test", SuccessPredicate: "raw command remains exact"}, now)
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.Claim(created.ID, "goal-capture-test", now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			adapter := &goalCaptureAdapter{attempts: runattempt.Open(homePath)}
			reserved, err := ReserveGovernedHarnessLaunch(GovernedHarnessReservationInput{
				HomePath: homePath, Store: store, ClaimedWork: claimed, Adapter: adapter,
				RunInput: harness.RunInput{WorkID: claimed.ID, Harness: adapter.Name(), Workspace: t.TempDir(), Command: "/bin/true", Args: args},
				Actor:    "goal-capture-test", Target: "running", Now: now.Add(2 * time.Second),
			})
			if err != nil {
				t.Fatal(err)
			}
			if reserved.RunInput.Prompt != "" || len(reserved.RunInput.Args) != len(args) {
				t.Fatalf("raw command changed: prompt=%q args=%v", reserved.RunInput.Prompt, reserved.RunInput.Args)
			}
		})
	}
}
