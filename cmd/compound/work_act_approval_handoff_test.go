package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/work"
)

func TestWorkActApprovalHandoffPublicCLI(t *testing.T) {
	f := newClaimedExecutionFixture(t, "fixture", "public approval handoff")
	now := time.Now().UTC()
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: "act_cli_handoff_fixture", CreatedAt: now.Format(time.RFC3339),
			Producer: envelope.Producer{Kind: "test", Name: "fixture"}, IDs: envelope.IDs{WorkID: f.workID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1",
		},
		Actor: "fixture", Capability: "fixture.send", Action: "send", Target: envelope.ActionTarget{Kind: "recipient", ID: "person@example.invalid"},
		MutationClass: "external_side_effect", Reversibility: "manual", BlastRadius: "local", Preview: "fixture message", RollbackPlan: "stop before send",
	}
	intentPath := filepath.Join(t.TempDir(), "intent.json")
	write := func(path string, value any) {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(intentPath, ai)
	out, err := runCLIFlagSeam(t, f.homePath, "work", "act", f.workID, "--intent", intentPath)
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 3 || !strings.Contains(string(out), "proposal "+ai.ID+" persisted") {
		t.Fatalf("held handoff err=%v output=%s", err, out)
	}
	out, err = runCLIFlagSeam(t, f.homePath, "policy", "action-plan", ai.ID, "--ttl", "5m", "--json")
	if err != nil {
		t.Fatalf("review source unavailable: %v %s", err, out)
	}
	if !strings.Contains(string(out), ai.ID) {
		t.Fatalf("review output omitted intent: %s", out)
	}
	cfg, _, err := policy.LoadConfig(f.homePath)
	if err != nil {
		t.Fatal(err)
	}
	as := policy.NewExactActionApprovalStore(f.homePath)
	p, _, err := as.Plan(ai.ID, cfg, 5*time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := as.Approve(p.ID, "operator:fixture", cfg, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	out, err = runCLIFlagSeam(t, f.homePath, "work", "act", f.workID, "--intent", intentPath)
	if err != nil || !strings.Contains(string(out), "action "+ai.ID+" recorded") {
		t.Fatalf("approved retry err=%v output=%s", err, out)
	}
	if out, err := runCLIFlagSeam(t, f.homePath, "work", "act", f.workID, "--intent", intentPath); err == nil || !strings.Contains(string(out), "already recorded") {
		t.Fatalf("replay err=%v output=%s", err, out)
	}

	// A caller-supplied saved allow decision is not authority for a new action.
	ai.ID = "act_cli_stale_plan_fixture"
	ai.Target.ID = "other@example.invalid"
	planPath := filepath.Join(t.TempDir(), "plan.json")
	write(planPath, work.ActionExecutionPlan{SchemaVersion: work.ActionExecutionPlanSchemaVersion, ID: "waep_fixture", WorkID: f.workID, ActionIntent: ai, Decision: envelope.PolicyDecision{Outcome: "allow"}})
	out, err = runCLIFlagSeam(t, f.homePath, "work", "act", f.workID, "--plan", planPath)
	exitErr, ok = err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 3 || !strings.Contains(string(out), "policy hold") {
		t.Fatalf("stale allow trusted err=%v output=%s", err, out)
	}
}
