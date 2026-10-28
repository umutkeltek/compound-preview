package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
)

func TestPolicyActionApprovalPlanApproveCheckCommands(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	monitored := policy.MonitorToolAction(policy.ToolActionRequest{
		WorkID:             "work_cli_exact_approval",
		Actor:              "codex",
		Harness:            "codex",
		Tool:               "browser.submit",
		Arguments:          map[string]any{"target": "publish", "form": "prepared-draft"},
		ToolSchemaRevision: "builtin:browser.submit",
	}, policy.DefaultConfig(), cliTestNow)
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(monitored.Intent.ID, monitored.Intent, "  "); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runPolicyActionApprovalCommand(
		[]string{"action-plan", monitored.Intent.ID, "--ttl", "5m", "--json"},
		&stdout,
		&stderr,
		cliTestNow,
	); code != 0 {
		t.Fatalf("action-plan exit=%d stderr=%s", code, stderr.String())
	}
	var plan policy.ExactActionApprovalPlan
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
		t.Fatalf("parse action plan: %v\n%s", err, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runPolicyActionApprovalCommand(
		[]string{"action-approve", plan.ID, "--granted-by", "operator:fixture", "--json"},
		&stdout,
		&stderr,
		cliTestNow.Add(time.Second),
	); code != 0 {
		t.Fatalf("action-approve exit=%d stderr=%s", code, stderr.String())
	}
	var approval policy.ExactActionApproval
	if err := json.Unmarshal(stdout.Bytes(), &approval); err != nil {
		t.Fatalf("parse action approval: %v\n%s", err, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runPolicyActionApprovalCommand(
		[]string{"action-check", approval.ID, "--json"},
		&stdout,
		&stderr,
		cliTestNow.Add(2*time.Second),
	); code != 0 {
		t.Fatalf("action-check exit=%d stderr=%s", code, stderr.String())
	}
	var status policy.ExactActionApprovalStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("parse action status: %v\n%s", err, stdout.String())
	}
	if status.Status != policy.ExactApprovalStatusReady {
		t.Fatalf("status = %+v, want ready", status)
	}
}
