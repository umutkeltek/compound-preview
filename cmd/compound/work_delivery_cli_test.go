package main

import (
	"crypto/sha256"
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
	"compound/internal/work"
)

func TestWorkDeliveryCLIRequestIsExactAndExclusive(t *testing.T) {
	base := []string{"--delivery-action", "act_original", "--delivery-kind", envelope.DeliveryPrivateRuntime, "--delivery-evidence", "artifact:sha256:proof"}
	request, err := parseWorkDeliveryRequest(base)
	if err != nil || request == nil || request.ActionIntentID != "act_original" || request.Kind != envelope.DeliveryPrivateRuntime || request.EvidenceRef != "artifact:sha256:proof" {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	for _, args := range [][]string{nil, {"--to", "running"}, {"--execute", "--", "synthetic", "--delivery-action", "not-a-work-binding"}} {
		if request, err := parseWorkDeliveryRequest(args); err != nil || request != nil {
			t.Fatalf("ordinary advance: %+v %v", request, err)
		}
	}
	for _, args := range [][]string{base[:2], base[:4], {"--delivery-action", "", "--delivery-kind", envelope.DeliveryPrivateRuntime, "--delivery-evidence", "ref"}} {
		if _, err := parseWorkDeliveryRequest(args); err == nil {
			t.Fatalf("accepted partial request: %v", args)
		}
	}
	for _, flag := range []string{"--execute", "--lane", "--launch-profile", "--effort", "--expected-selection-revision", "--target-worktree-root", "--local-only", "--run-attempt", "--artifact", "--verification", "--context-pack", "--harness", "--prompt", "--workspace", "--permission-mode", "--timeout", "--"} {
		if _, err := parseWorkDeliveryRequest(append(append([]string(nil), base...), flag, "value")); err == nil {
			t.Errorf("accepted mixed operation %s", flag)
		}
	}
}

func TestWorkDeliveryCaptureCLIJourney(t *testing.T) {
	runWorkDeliveryCaptureCLIJourney(t, "operator:test")
}

func TestWorkDeliveryCaptureCLIExecutorHeldJourney(t *testing.T) {
	runWorkDeliveryCaptureCLIJourney(t, "executor:test")
}

func runWorkDeliveryCaptureCLIJourney(t *testing.T, holder string) {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	now := time.Now().UTC()
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	s, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(work.CreateInput{Owner: "operator:test", Actor: "operator:test", SuccessPredicate: "private readback is bound before close", State: "ready"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, holder, now); err != nil {
		t.Fatal(err)
	}
	w, err = s.Advance(w.ID, work.AdvanceInput{To: "running", Actor: holder}, now)
	if err != nil {
		t.Fatal(err)
	}
	repo, base, _ := initLandingCLIRepo(t)
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("docs_private/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "docs_private"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := "docs_private/runtime.json"
	action := envelope.ActionIntent{
		Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_private_cli", CreatedAt: now.Format(time.RFC3339Nano), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: w.ID}, Privacy: "private", Risk: "tier1", Uncertainty: envelope.Uncertainty{Level: "known"}},
		Actor:  holder, Capability: "build-implement", Action: "update_private_runtime",
		Target:        envelope.ActionTarget{Kind: "path", ID: w.ID, Path: filepath.Join(repo, path)},
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local", RollbackPlan: "restore exact prior absence",
	}
	action.Authority = envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.RecordAction(w.ID, action, policy.Evaluate(action, cfg, now), now)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("observed private fixture")
	if err := os.WriteFile(filepath.Join(repo, path), body, 0o600); err != nil {
		t.Fatal(err)
	}
	request := work.PrivateRuntimeDeliveryCaptureRequest{
		SchemaVersion: work.PrivateRuntimeDeliveryCaptureSchema, CaptureID: "cli_readback", ActionIntentID: action.ID,
		RepositoryRoot: repo, RepositoryRef: base, ExpectedWorkState: w.State,
		ExpectedPolicyDecisionID: w.PolicyDecisionID, ExpectedActionIntentID: w.ActionIntentIDs[len(w.ActionIntentIDs)-1],
		Files: []work.PrivateRuntimeDeliveryCaptureFile{{Path: path, AfterSHA256: fmt.Sprintf("%x", sha256.Sum256(body)), BeforeAbsent: true}},
	}
	requestBody, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(requestPath, requestBody, 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"delivery-capture", w.ID, "--request", requestPath, "--by", holder, "--json"})
	})
	var result work.PrivateRuntimeDeliveryCaptureResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("capture output: %v\n%s", err, out)
	}
	if result.Status != "captured_not_bound" || result.ReceiptID == "" {
		t.Fatalf("capture=%+v", result)
	}
	current, err := s.Get(w.ID)
	if err != nil || len(current.NonSourceDelivery) != 0 || current.State != "running" {
		t.Fatalf("capture changed binding or state: %+v %v", current, err)
	}
	planArgs := []string{"advance-plan", w.ID, "--to", "running", "--actor", "operator:test", "--delivery-action", action.ID, "--delivery-kind", result.Delivery.Kind, "--delivery-evidence", result.Delivery.EvidenceRef, "--dry-run", "--json"}
	beforePreview := continueLifecycleSnapshot(t, homePath)
	if holder != "operator:test" {
		out = captureStdout(t, func() { cmdWork(planArgs) })
		var held work.AdvancePlan
		if err := json.Unmarshal([]byte(out), &held); err != nil || held.NoAction == "" || len(held.AdvanceCommand) != 0 {
			t.Fatalf("unapproved lease override offered: %+v %v", held, err)
		}
		planArgs = append(planArgs, "--steal", "--reason", "operator-approved delivery classification only")
	}
	out = captureStdout(t, func() { cmdWork(planArgs) })
	var plan work.AdvancePlan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan: %v\n%s", err, out)
	}
	if len(plan.AdvanceCommand) < 3 {
		t.Fatalf("no guarded advance: %s", out)
	}
	if holder != "operator:test" {
		if got, ok := exactFlagValue(plan.AdvanceCommand, "--reason"); !ok || got != "operator-approved delivery classification only" {
			t.Fatalf("reason not preserved: %v", plan.AdvanceCommand)
		}
	}
	if !reflect.DeepEqual(beforePreview, continueLifecycleSnapshot(t, homePath)) {
		t.Fatal("delivery preview changed lifecycle files")
	}
	for flag, want := range map[string]string{
		"--expected-work-state":      current.State,
		"--expected-policy-decision": current.PolicyDecisionID,
		"--expected-action-intent":   current.ActionIntentIDs[len(current.ActionIntentIDs)-1],
	} {
		if got, ok := exactFlagValue(plan.AdvanceCommand, flag); !ok || got != want {
			t.Fatalf("%s = %q, want %q", flag, got, want)
		}
	}
	leases, err := s.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() { cmdWork(plan.AdvanceCommand[2:]) })
	afterLeases, err := s.ListLeases()
	if err != nil || !reflect.DeepEqual(leases, afterLeases) {
		t.Fatalf("binding transferred executor lease: %+v -> %+v (%v)", leases, afterLeases, err)
	}
	current, err = s.Get(w.ID)
	if err != nil || len(current.NonSourceDelivery) != 1 {
		t.Fatalf("binding: %+v %v", current, err)
	}
	captureStdout(t, func() {
		cmdWork([]string{"close", w.ID, "--outcome", "completed", "--move-type", "control_plane_repair", "--by", holder, "--summary", "observed and bound private readback", "--proof-ref", result.Delivery.EvidenceRef})
	})
	current, err = s.Get(w.ID)
	if err != nil || current.State != "completed" {
		t.Fatalf("close: %+v %v", current, err)
	}
}

func TestWorkAdvancePlanLeaseOverrideRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		exit  int
	}{
		{"no_override", nil, 0},
		{"steal_without_reason", []string{"--steal"}, 0},
		{"reason_without_steal", []string{"--reason", "reviewed binding"}, 0},
		{"blank_reason", []string{"--steal", "--reason", " "}, 0},
		{"missing_reason_value", []string{"--steal", "--reason"}, 2},
		{"reason_before_option", []string{"--steal", "--reason", "--json"}, 2},
		{"reason_value_is_steal", []string{"--reason", "--steal"}, 2},
		{"action_value_is_not_steal", []string{"--action", "--steal", "--reason", "reviewed binding"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath, workID := workRefusalHome(t, "executor:test", "preview never takes the lease")
			before := continueLifecycleSnapshot(t, homePath)
			args := []string{"work", "advance-plan", workID, "--to", "running", "--actor", "operator:test", "--dry-run", "--json"}
			stdout, stderr, exit, err := runCompoundSplit(t, homePath, append(args, tc.flags...)...)
			if exit != tc.exit {
				t.Fatalf("exit=%d want=%d err=%v stdout=%s stderr=%s", exit, tc.exit, err, stdout, stderr)
			}
			if exit == 0 {
				var plan work.AdvancePlan
				if err := json.Unmarshal(stdout, &plan); err != nil || plan.NoAction == "" || len(plan.AdvanceCommand) != 0 {
					t.Fatalf("refused preview offered execution: %+v %v", plan, err)
				}
			}
			if !reflect.DeepEqual(before, continueLifecycleSnapshot(t, homePath)) {
				t.Fatal("refused preview changed lifecycle files")
			}
		})
	}
}

func TestWorkAdvancePlanSeparatorDoesNotParsePositionalReason(t *testing.T) {
	homePath, _ := workRefusalHome(t, "executor:test", "positional token is not an override option")
	before := continueLifecycleSnapshot(t, homePath)
	_, stderr, exit, err := runCompoundSplit(t, homePath, "work", "advance-plan", "--to", "running", "--actor", "operator:test", "--dry-run", "--json", "--", "--reason")
	if exit != 2 || !strings.Contains(string(stderr), "usage: compound work advance-plan") || strings.Contains(string(stderr), "panic:") {
		t.Fatalf("invalid work identity must report usage, not parse positional token: exit=%d err=%v stderr=%s", exit, err, stderr)
	}
	if !reflect.DeepEqual(before, continueLifecycleSnapshot(t, homePath)) {
		t.Fatal("invalid work identity changed lifecycle files")
	}
}
