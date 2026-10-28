package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/harness"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// stubClaudeAdapter is a hermetic stand-in for the real subprocess-spawning claude
// adapter, so a Run test can reach the post-dispatch consumption record without
// spawning `claude`. It is injected through the buildAdapterRegistry seam.
type stubClaudeAdapter struct {
	ran                bool
	store              *runattempt.Store
	status             string
	runErr             error
	contradictWork     bool
	returnedStatus     string
	contradictStoredID bool
}

func (a *stubClaudeAdapter) Name() string { return "claude" }

func (a *stubClaudeAdapter) Run(_ context.Context, in harness.RunInput) (harness.RunOutput, error) {
	a.ran = true
	ra, err := a.store.MintWithActionIntent(in.WorkID, "actuator-test", in.Harness, in.ActionIntentID, in.Now)
	if err != nil {
		return harness.RunOutput{}, err
	}
	status := a.status
	if status == "" {
		status = runattempt.StatusSucceeded
	}
	if runattempt.IsTerminal(status) {
		ra, err = a.store.Complete(ra.ID, status, in.Now)
		if err != nil {
			return harness.RunOutput{RunAttempt: ra}, err
		}
	}
	if a.contradictWork {
		ra.WorkID = "work_foreign"
	}
	if a.returnedStatus != "" {
		ra.Status = a.returnedStatus
	}
	if a.contradictStoredID {
		contradictory := ra
		contradictory.ID = "ratt_foreign"
		data, err := json.Marshal(contradictory)
		if err != nil {
			return harness.RunOutput{}, err
		}
		// Deliberately bypass the valid writer: the production reader must not
		// treat a filename/body identity contradiction as terminal evidence.
		if err := os.WriteFile(filepath.Join(a.store.Home(), "run_attempts", ra.ID+".json"), data, 0o600); err != nil {
			return harness.RunOutput{}, err
		}
	}
	return harness.RunOutput{
		RunAttempt: ra,
		ExitCode:   0,
	}, a.runErr
}

func createKnownActuatorWork(t *testing.T, homePath, workID string) {
	t.Helper()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID:           workID,
		Owner:            "actuator-test",
		SuccessPredicate: "actuator test work completes",
	}, testNow); err != nil {
		t.Fatal(err)
	}
}

func installStubActuatorRegistry(t *testing.T, stub *stubClaudeAdapter) {
	t.Helper()
	original := buildAdapterRegistry
	buildAdapterRegistry = func(homePath string) *harness.AdapterRegistry {
		stub.store = runattempt.Open(homePath)
		registry := harness.NewAdapterRegistry(runattempt.Open(homePath))
		if err := registry.RegisterAdapter(stub); err != nil {
			t.Fatalf("register stub adapter: %v", err)
		}
		return registry
	}
	t.Cleanup(func() { buildAdapterRegistry = original })
}

// T5 (F10): the switchback instruction write is GOVERNED, not a raw os.WriteFile.
// Under the default policy a workspace instruction file is local_reversible/easy, so
// it flows AUTO through act.Execute(FileArtifactAdapter) and carries a receipt +
// transition recorded in the canonical event store. (When policy holds it instead,
// the step is honestly Held with the target path in reasons — never a silent raw
// write.)
func TestSwitchbackInstructionStepIsGoverned(t *testing.T) {
	home := t.TempDir()
	createKnownActuatorWork(t, home, "work_switch")
	workspace := t.TempDir()
	cfg := policy.DefaultConfig()
	profile := ProjectProfile{
		SchemaVersion: ProfileSchemaVersion,
		ID:            "prof_instr",
		Slug:          "demo",
		Workspace:     workspace,
		Routing:       RoutingSpec{Harness: "claude"},
		Instructions: []InstructionSurface{
			{Name: "AGENTS.md", Path: "AGENTS.md", Body: "# demo agents contract\n"},
		},
		CreatedAt: testNow.Format(time.RFC3339),
	}

	res, err := Switchback(home, profile, "work_switch", cfg, testNow)
	if err != nil {
		t.Fatalf("switchback: %v", err)
	}

	var instr *StepResult
	for i := range res.Steps {
		if res.Steps[i].Kind == StepInstructions {
			instr = &res.Steps[i]
		}
	}
	if instr == nil {
		t.Fatal("no instruction step in switchback result")
	}
	if instr.Held {
		t.Fatalf("instruction step held unexpectedly: %v", instr.Reasons)
	}
	if instr.ReceiptRef == "" {
		t.Fatalf("instruction write must be governed with a receipt, got none (reasons=%v)", instr.Reasons)
	}

	target := filepath.Join(workspace, "AGENTS.md")
	got, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("instruction file not written: %v", rerr)
	}
	if string(got) != "# demo agents contract\n" {
		t.Fatalf("instruction body wrong: %q", got)
	}

	events, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Read(instr.ReceiptRef); err != nil {
		t.Fatalf("instruction receipt must resolve through the event store: %v", err)
	}
	if _, err := events.Read(instr.TransitionRef); err != nil {
		t.Fatalf("instruction transition must resolve through the event store: %v", err)
	}
}

// T1 (F8): an arbitrary harness command (os-harness-entrypoints) must be classified
// so the default policy outcome is HOLD and Route never auto-executes it — derived
// from what is run, not a constant local_reversible label.
func TestRunIntentHoldsArbitraryHarnessCommand(t *testing.T) {
	in := RunInput{WorkID: "work_1", Harness: "os-harness-entrypoints", Command: "true", Args: []string{"--flag"}}
	intent := runIntent(in, testNow)
	if intent.MutationClass != "external_side_effect" {
		t.Fatalf("arbitrary harness run must be classified external_side_effect, got %q", intent.MutationClass)
	}
	pd := policy.Evaluate(intent, policy.DefaultConfig(), testNow)
	if pd.Outcome != "hold" {
		t.Fatalf("default policy for an arbitrary harness run must be hold, got %q", pd.Outcome)
	}
	d := Route(intent, pd.Outcome, openHeadroom())
	if d.Mode != ModeHold {
		t.Fatalf("Route must HOLD an arbitrary harness run, got %s (reasons=%v)", d.Mode, d.Reasons)
	}
}

// T1 (F8, end-to-end): Run of an arbitrary harness command must HOLD without
// dispatching — no subprocess, no RunAttempt minted — and persist a held-move card.
func TestRunHoldsArbitraryCommandWithoutDispatch(t *testing.T) {
	home := t.TempDir()
	createKnownActuatorWork(t, home, "work_run")
	res, err := Run(home, RunInput{
		WorkID:  "work_run",
		Harness: "os-harness-entrypoints",
		Command: "true",
	}, policy.DefaultConfig(), testNow)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Held || res.Executed {
		t.Fatalf("arbitrary harness run must be HELD, not executed: %+v", res)
	}
	if res.RunAttemptID != "" {
		t.Fatalf("held run must not mint a RunAttempt, got %q", res.RunAttemptID)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "run_attempts")); len(entries) != 0 {
		t.Fatalf("held run must not spawn a subprocess / mint a RunAttempt; found %d run_attempt entries", len(entries))
	}
	if res.HeldMoveID == "" {
		t.Fatal("held run must persist a human-gated held-move card")
	}
}

func TestRunFailsClosedWhenHeldMoveCardCannotBePersisted(t *testing.T) {
	home := t.TempDir()
	createKnownActuatorWork(t, home, "work_run")

	actuatorDir := filepath.Join(home, "actuator")
	if err := os.MkdirAll(actuatorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actuatorDir, "held-moves"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Run(home, RunInput{
		WorkID:  "work_run",
		Harness: "os-harness-entrypoints",
		Command: "true",
	}, policy.DefaultConfig(), testNow)

	if err == nil || !strings.Contains(err.Error(), "persist held-move card") {
		t.Fatalf("held-card persistence failure must surface, result=%+v err=%v", result, err)
	}
	if result.Held || result.HeldMoveID != "" || result.HeldMovePath != "" {
		t.Fatalf("an unpersisted card must not be reported as a successful hold: %+v", result)
	}
}

// T2 (F8, regression guard): a claude run in plan mode (and empty→plan default) stays
// local_reversible/easy and routes AUTO — plan mode is read-only by contract.
func TestRunIntentClaudePlanStaysAuto(t *testing.T) {
	for _, mode := range []string{"", "plan"} {
		in := RunInput{WorkID: "work_2", Harness: "claude", PermissionMode: mode, Prompt: "summarize"}
		intent := runIntent(in, testNow)
		if intent.MutationClass != "local_reversible" || intent.Reversibility != "easy" {
			t.Fatalf("claude+%q must stay local_reversible/easy, got %q/%q", mode, intent.MutationClass, intent.Reversibility)
		}
		pd := policy.Evaluate(intent, policy.DefaultConfig(), testNow)
		d := Route(intent, pd.Outcome, openHeadroom())
		if d.Mode != ModeAuto {
			t.Fatalf("claude+%q (plan) must route AUTO, got %s (reasons=%v)", mode, d.Mode, d.Reasons)
		}
	}
}

// F8: a claude run in a WRITE posture (acceptEdits/bypass/default) is not read-only,
// so it must be held like any other arbitrary run — plan is the ONLY auto-safe mode.
func TestRunIntentClaudeNonPlanHolds(t *testing.T) {
	for _, mode := range []string{"acceptEdits", "bypass", "default"} {
		in := RunInput{WorkID: "work_3", Harness: "claude", PermissionMode: mode, Prompt: "edit files"}
		intent := runIntent(in, testNow)
		if intent.MutationClass != "external_side_effect" {
			t.Fatalf("claude+%q must be held (external_side_effect), got %q", mode, intent.MutationClass)
		}
		pd := policy.Evaluate(intent, policy.DefaultConfig(), testNow)
		d := Route(intent, pd.Outcome, openHeadroom())
		if d.Mode != ModeHold {
			t.Fatalf("claude+%q must HOLD, got %s", mode, d.Mode)
		}
	}
}

// T4 (F9.2): a corrupt row in the consumption ledger must make Used ERROR naming the
// line, and the budget gate must treat that as BLOCKING (hold) — never silently skip
// the row and under-count, and never hard-error out of the plan.
func TestCorruptLedgerErrorsAndHoldsBudget(t *testing.T) {
	home := t.TempDir()
	l := OpenLedger(home)
	// Row 1: a valid consumption row (so the file + dir exist).
	if err := l.Record(Consumption{Channel: "*", At: testNow.Format(time.RFC3339), Actions: 1}); err != nil {
		t.Fatalf("record valid row: %v", err)
	}
	// Row 2: a corrupt, non-JSON row appended directly.
	f, err := os.OpenFile(filepath.Join(home, "actuator", "budget", "consumption.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{ this is not valid json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Used errors, naming the corrupt line.
	if _, uerr := l.Used("", "*", testNow.Add(-time.Hour)); uerr == nil {
		t.Fatal("Used must error on a corrupt ledger row, not silently skip it")
	} else if !strings.Contains(uerr.Error(), "line 2") {
		t.Fatalf("Used error must name the corrupt line number, got: %v", uerr)
	}

	// The budget gate converts the unreadable ledger into a HOLD, not zero usage,
	// and not a hard error that aborts the plan.
	h, cerr := l.Check(DefaultBudget(testNow), "", "*", testNow)
	if cerr != nil {
		t.Fatalf("Check must convert the ledger error into a hold, not propagate a hard error: %v", cerr)
	}
	if h.Allowed {
		t.Fatal("a corrupt budget ledger must HOLD (not allowed), never be treated as zero usage")
	}

	// End-to-end: an otherwise-auto move is HELD on the corrupt ledger.
	intent := runIntent(RunInput{WorkID: "w", Harness: "claude", PermissionMode: "plan"}, testNow)
	plan, perr := EvaluatePlan(home, intent, "", "", policy.DefaultConfig(), testNow)
	if perr != nil {
		t.Fatalf("EvaluatePlan must not hard-error on a corrupt ledger: %v", perr)
	}
	if plan.Decision.Mode != ModeHold {
		t.Fatalf("an otherwise-auto move on a corrupt ledger must be HELD, got %s (reasons=%v)", plan.Decision.Mode, plan.Decision.Reasons)
	}
}

// A failed settlement after dispatch must preserve both the truthful execution
// result and its durable budget reservation. Injection is after dispatch, not
// a pre-admission write failure.
func TestRunSurfacesConsumptionRecordFailure(t *testing.T) {
	home := t.TempDir()
	createKnownActuatorWork(t, home, "work_rec")
	original := recordRunConsumption
	recordRunConsumption = func(*Ledger, Consumption) error { return errors.New("consumption sink unavailable") }
	t.Cleanup(func() { recordRunConsumption = original })

	stub := &stubClaudeAdapter{}
	orig := buildAdapterRegistry
	buildAdapterRegistry = func(h string) *harness.AdapterRegistry {
		stub.store = runattempt.Open(h)
		reg := harness.NewAdapterRegistry(runattempt.Open(h))
		if err := reg.RegisterAdapter(stub); err != nil {
			t.Fatalf("register stub adapter: %v", err)
		}
		return reg
	}
	defer func() { buildAdapterRegistry = orig }()

	res, err := Run(home, RunInput{
		WorkID:         "work_rec",
		Harness:        "claude",
		PermissionMode: "plan",
		Prompt:         "hello",
	}, policy.DefaultConfig(), testNow)

	if !stub.ran {
		t.Fatal("precondition: the auto run must have dispatched the stub adapter")
	}
	if !res.Executed {
		t.Fatalf("the run DID execute; result must report Executed=true: %+v", res)
	}
	if err == nil {
		t.Fatal("a failed consumption record must surface as an error, not be silently discarded")
	}
	if !strings.Contains(err.Error(), "consumption") {
		t.Fatalf("error must name the consumption-record failure, got: %v", err)
	}
	if res.RunAttemptID == "" || !strings.Contains(err.Error(), res.RunAttemptID) {
		t.Fatalf("error must report the run happened (run_attempt id %q), got: %v", res.RunAttemptID, err)
	}
	used, usageErr := OpenLedger(home).Used("", "execution", testNow.Add(48*time.Hour))
	if usageErr != nil || used.Actions != 1 {
		t.Fatalf("failed settlement must retain reservation even after its window: %+v, %v", used, usageErr)
	}
}

func TestRunRejectsMissingWorkBeforeDispatch(t *testing.T) {
	homePath := t.TempDir()
	stub := &stubClaudeAdapter{}
	installStubActuatorRegistry(t, stub)

	result, err := Run(homePath, RunInput{
		WorkID:         "work_missing",
		Harness:        "claude",
		PermissionMode: "plan",
		Prompt:         "inspect the work",
	}, policy.DefaultConfig(), testNow)

	if err == nil || !strings.Contains(err.Error(), "work_missing") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing work must fail with a named not-found error, result=%+v err=%v", result, err)
	}
	if stub.ran {
		t.Fatal("missing work must fail before harness dispatch")
	}
	if entries, _ := os.ReadDir(filepath.Join(homePath, "run_attempts")); len(entries) != 0 {
		t.Fatalf("missing work must not mint a RunAttempt; found %d entries", len(entries))
	}
}

func TestSwitchbackRejectsMissingWorkBeforeMutation(t *testing.T) {
	homePath := t.TempDir()
	workspace := t.TempDir()
	target := filepath.Join(workspace, "AGENTS.md")
	routingPath := filepath.Join(t.TempDir(), "routing.json")
	routingBefore := "{\"lane\":\"baseline\"}\n"
	if err := os.WriteFile(routingPath, []byte(routingBefore), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Switchback(homePath, ProjectProfile{
		SchemaVersion: ProfileSchemaVersion,
		ID:            "prof_missing_work",
		Slug:          "missing-work-demo",
		Workspace:     workspace,
		Routing: RoutingSpec{
			Harness:     "claude",
			ConfigPath:  routingPath,
			ConfigKey:   "lane",
			ConfigValue: "next",
		},
		Instructions: []InstructionSurface{{
			Name: "AGENTS.md",
			Path: "AGENTS.md",
			Body: "# must never be written\n",
		}},
		CreatedAt: testNow.Format(time.RFC3339),
	}, "work_missing", policy.DefaultConfig(), testNow)

	if err == nil || !strings.Contains(err.Error(), "work_missing") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing work must fail with a named not-found error, result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("missing work must fail before the instruction write, stat err=%v", statErr)
	}
	routingAfter, readErr := os.ReadFile(routingPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(routingAfter) != routingBefore {
		t.Fatalf("missing work must fail before the routing write: before=%q after=%q", routingBefore, routingAfter)
	}
	for _, path := range []string{
		filepath.Join(homePath, "actuator"),
		filepath.Join(homePath, "artifacts"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("missing work must not create actuator artifacts at %s, stat err=%v", path, statErr)
		}
	}
}

func TestActionIntentIdentityIncludesInvocationTime(t *testing.T) {
	firstAt := testNow
	secondAt := testNow.Add(time.Second)

	tests := []struct {
		name  string
		first envelope.ActionIntent
		next  envelope.ActionIntent
	}{
		{
			name: "harness run",
			first: runIntent(RunInput{
				WorkID: "work_identity", Harness: "claude", PermissionMode: "plan",
				Workspace: "/workspace", Prompt: "inspect",
			}, firstAt),
			next: runIntent(RunInput{
				WorkID: "work_identity", Harness: "claude", PermissionMode: "plan",
				Workspace: "/workspace", Prompt: "inspect",
			}, secondAt),
		},
		{
			name:  "instruction write",
			first: instructionIntent(ProjectProfile{Slug: "demo"}, "work_identity", "/workspace/AGENTS.md", firstAt),
			next:  instructionIntent(ProjectProfile{Slug: "demo"}, "work_identity", "/workspace/AGENTS.md", secondAt),
		},
		{
			name: "routing repoint",
			first: *routingIntent(ProjectProfile{
				Slug: "demo", Routing: RoutingSpec{
					Harness: "claude", ConfigPath: "/config.json", ConfigKey: "route", ConfigValue: "next",
				},
			}, "work_identity", firstAt),
			next: *routingIntent(ProjectProfile{
				Slug: "demo", Routing: RoutingSpec{
					Harness: "claude", ConfigPath: "/config.json", ConfigKey: "route", ConfigValue: "next",
				},
			}, "work_identity", secondAt),
		},
		{
			name: "distribution post",
			first: postIntent(
				PostInput{WorkID: "work_identity", ProductID: "product:demo", Content: "same"},
				"distribution", "generic", "/outbox/post.json", firstAt,
			),
			next: postIntent(
				PostInput{WorkID: "work_identity", ProductID: "product:demo", Content: "same"},
				"distribution", "generic", "/outbox/post.json", secondAt,
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.first.ID == test.next.ID {
				t.Fatalf("repeated action attempts reused %s despite different invocation times", test.first.ID)
			}
		})
	}
}

func TestRunPersistsAuthorizationBeforeDispatch(t *testing.T) {
	homePath := t.TempDir()
	createKnownActuatorWork(t, homePath, "work_authorized")
	stub := &stubClaudeAdapter{}
	installStubActuatorRegistry(t, stub)

	result, err := Run(homePath, RunInput{
		WorkID:         "work_authorized",
		Harness:        "claude",
		PermissionMode: "plan",
		Prompt:         "inspect the work",
	}, policy.DefaultConfig(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !stub.ran || !result.Executed {
		t.Fatalf("known auto-safe work must dispatch after authorization persists: %+v", result)
	}
	if result.Plan.PolicyDecision.ActionIntentID != result.Plan.Intent.ID {
		t.Fatalf("policy decision %s authorizes %q, want %q", result.Plan.PolicyDecision.ID, result.Plan.PolicyDecision.ActionIntentID, result.Plan.Intent.ID)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Read(result.Plan.Intent.ID); err != nil {
		t.Fatalf("ActionIntent %s is not resolvable after dispatch: %v", result.Plan.Intent.ID, err)
	}
	if _, err := events.Read(result.Plan.PolicyDecision.ID); err != nil {
		t.Fatalf("PolicyDecision %s is not resolvable after dispatch: %v", result.Plan.PolicyDecision.ID, err)
	}
}

func TestRunDoesNotDispatchWhenAuthorizationPersistenceFails(t *testing.T) {
	homePath := t.TempDir()
	createKnownActuatorWork(t, homePath, "work_persist_fail")
	input := RunInput{
		WorkID:         "work_persist_fail",
		Harness:        "claude",
		PermissionMode: "plan",
		Prompt:         "inspect the work",
	}
	intent := runIntent(input, testNow)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(intent.ID, map[string]any{
		"id":       intent.ID,
		"conflict": "pre-existing different authorization",
	}, "  "); err != nil {
		t.Fatal(err)
	}
	stub := &stubClaudeAdapter{}
	installStubActuatorRegistry(t, stub)

	result, err := Run(homePath, input, policy.DefaultConfig(), testNow)

	if err == nil || !strings.Contains(err.Error(), "persist") {
		t.Fatalf("authorization persistence failure must abort with a named error, result=%+v err=%v", result, err)
	}
	if stub.ran || result.Executed {
		t.Fatalf("authorization persistence failure must happen before dispatch: result=%+v ran=%v", result, stub.ran)
	}
	if entries, _ := os.ReadDir(filepath.Join(homePath, "run_attempts")); len(entries) != 0 {
		t.Fatalf("authorization persistence failure must not mint a RunAttempt; found %d entries", len(entries))
	}
}

func TestSwitchbackAccountingFailureIsStructuredAndNonGreen(t *testing.T) {
	homePath := t.TempDir()
	createKnownActuatorWork(t, homePath, "work_switch_accounting")
	workspace := t.TempDir()
	original := recordSwitchbackConsumption
	recordSwitchbackConsumption = func(*Ledger, Consumption) error {
		return errors.New("accounting sink unavailable")
	}
	t.Cleanup(func() { recordSwitchbackConsumption = original })

	result, err := Switchback(homePath, ProjectProfile{
		SchemaVersion: ProfileSchemaVersion,
		ID:            "prof_accounting_failure",
		Slug:          "accounting-demo",
		Workspace:     workspace,
		Routing:       RoutingSpec{Harness: "claude"},
		Instructions: []InstructionSurface{{
			Name: "AGENTS.md",
			Path: "AGENTS.md",
			Body: "# accounting failure remains visible\n",
		}},
		CreatedAt: testNow.Format(time.RFC3339),
	}, "work_switch_accounting", policy.DefaultConfig(), testNow)

	if err == nil || !strings.Contains(err.Error(), "accounting") {
		t.Fatalf("receipted write with failed accounting must return a named error, result=%+v err=%v", result, err)
	}
	var appliedWithReceipt bool
	for _, step := range result.Steps {
		if step.Applied && step.ReceiptRef != "" {
			appliedWithReceipt = true
			break
		}
	}
	if !appliedWithReceipt {
		t.Fatalf("partial result must preserve the applied receipted step: %+v", result.Steps)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"accounting_failed":true`) ||
		!strings.Contains(string(encoded), `"accounting_error":"accounting sink unavailable"`) {
		t.Fatalf("partial result must expose structured accounting failure fields: %s", encoded)
	}
}
