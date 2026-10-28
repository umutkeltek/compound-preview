package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/hookplan"
	"compound/internal/work"
)

func captureCoordination(t *testing.T, args []string) string {
	t.Helper()
	var buf strings.Builder
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdCoordination(args)
	w.Close()
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err != nil {
			break
		}
	}
	os.Stdout = old
	return buf.String()
}

func coordinationCLIHome(t *testing.T) string {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	return seedCoordinationCLIHome(t, homePath, "lane-x")
}

func seedCoordinationCLIHome(t *testing.T, homePath, actor string) string {
	t.Helper()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w, err := store.Create(work.CreateInput{Owner: actor, DesiredOutcome: "ship the parser", SuccessPredicate: "parser lands"}, cliTestNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Claim(w.ID, actor, cliTestNow.Add(time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: actor, Note: "start"}, cliTestNow.Add(2*time.Minute)); err != nil {
		t.Fatalf("advance: %v", err)
	}
	return w.ID
}

// The console contract: `coordination --json` emits compound.coordination.v0.1
// with me / others / waits, generic actors, no hardcoded identities.
func TestCoordinationCLIJSONContract(t *testing.T) {
	workID := coordinationCLIHome(t)

	out := captureCoordination(t, []string{"--json"})
	var report work.CoordinationReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, out)
	}
	if report.SchemaVersion != work.CoordinationSchemaVersion {
		t.Fatalf("schema_version = %q", report.SchemaVersion)
	}
	if report.Me.Actor != "lane-x" { // most-present actor, never a hardcoded name
		t.Fatalf("me.actor = %q, want lane-x (doing the running work)", report.Me.Actor)
	}
	if report.Me.Doing == nil || report.Me.Doing.WorkID != workID {
		t.Fatalf("me.doing = %+v, want %s", report.Me.Doing, workID)
	}
	if report.Me.CanContinue {
		t.Fatalf("me.can_continue = true; running work needs run evidence")
	}
	if report.Me.WaitReason != "run_evidence" {
		t.Fatalf("me.wait_reason = %q", report.Me.WaitReason)
	}
	if len(report.Waits) == 0 {
		t.Fatalf("waits empty; lane-x is blocked on %s", workID)
	}
}

func TestCoordinationCLIJSONEmbedsPureOperatorProjection(t *testing.T) {
	workID := coordinationCLIHome(t)
	homePath := os.Getenv("COMPOUND_HOME")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("body sentinel must remain behind artifact custody"))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := store.RecordCollaboration(workID, work.CollaborationInput{
		Actor: "lane-x", IdempotencyKey: "coordination-cli-operator", BodyRef: bodyRef,
		DeliveryClass: work.CollaborationDeliverySilentState,
		Delta: work.CollaborationStateDelta{
			Kind: "scope", SubjectRef: "scope:coordination-cli", Before: "open", After: "reserved",
		},
		Affects: work.CollaborationAffects{WorkIDs: []string{workID}},
	}, cliTestNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	before := snapshotTree(t, homePath)
	first := captureCoordination(t, []string{"--json"})
	second := captureCoordination(t, []string{"--json"})
	after := snapshotTree(t, homePath)
	if len(before) != len(after) {
		t.Fatalf("coordination polls changed home file count: before=%d after=%d", len(before), len(after))
	}
	for path, body := range before {
		if after[path] != body {
			t.Fatalf("coordination polls mutated %s", path)
		}
	}

	var payload struct {
		work.CoordinationReport
		OperatorCoordination hookplan.OperatorCoordinationProjection `json:"operator_coordination"`
	}
	if err := json.Unmarshal([]byte(first), &payload); err != nil {
		t.Fatalf("first JSON: %v\n%s", err, first)
	}
	if payload.Me.Doing == nil || payload.Me.Doing.WorkID != workID {
		t.Fatalf("existing coordination report changed: %+v", payload.CoordinationReport)
	}
	if payload.OperatorCoordination.Total != 1 || len(payload.OperatorCoordination.Items) != 1 ||
		payload.OperatorCoordination.Items[0].EventRef != recorded.Event.ID {
		t.Fatalf("operator coordination missing from JSON: %+v", payload.OperatorCoordination)
	}
	var repeated struct {
		OperatorCoordination hookplan.OperatorCoordinationProjection `json:"operator_coordination"`
	}
	if err := json.Unmarshal([]byte(second), &repeated); err != nil {
		t.Fatalf("second JSON: %v\n%s", err, second)
	}
	if payload.OperatorCoordination.Revision != repeated.OperatorCoordination.Revision {
		t.Fatalf("level-triggered projection changed across polls: first=%+v second=%+v",
			payload.OperatorCoordination, repeated.OperatorCoordination)
	}
	for _, forbidden := range []string{"body sentinel", "cursor", "report_ref", "acknowledgement"} {
		if strings.Contains(strings.ToLower(first), forbidden) {
			t.Fatalf("coordination JSON exposed forbidden %q: %s", forbidden, first)
		}
	}
}

// `--actor` repoints "me" without changing the projection's honesty: an
// explicit actor with no presence gets an empty face, never a re-target.
func TestCoordinationCLIExplicitActor(t *testing.T) {
	coordinationCLIHome(t)
	out := captureCoordination(t, []string{"--json", "--actor", "lane-y"})
	var report work.CoordinationReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, out)
	}
	if report.Actor != "lane-y" || !report.ActorExplicit {
		t.Fatalf("actor = %q explicit=%v", report.Actor, report.ActorExplicit)
	}
	if report.Me.Doing != nil {
		t.Fatalf("lane-y has no live work; me.doing = %+v", report.Me.Doing)
	}
	var sawLaneX bool
	for _, other := range report.Others {
		if other.Actor == "lane-x" {
			sawLaneX = true
		}
	}
	if !sawLaneX {
		t.Fatalf("lane-x dropped from others: %+v", report.Others)
	}
}

// Human output keeps the coordination vocabulary: Me / Others / Waiting-on.
func TestCoordinationCLIHumanSections(t *testing.T) {
	coordinationCLIHome(t)
	out := captureCoordination(t, []string{})
	for _, want := range []string{"ME", "OTHERS", "WAITING-ON", "lane-x", "run_evidence"} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing %q:\n%s", want, out)
		}
	}
}

func TestCoordinationCLIUsage(t *testing.T) {
	out := captureCoordination(t, []string{"--help"})
	if !strings.Contains(out, "usage: compound coordination") {
		t.Fatalf("usage output = %q", out)
	}
}

func TestCoordinationCLIUsesExplicitHome(t *testing.T) {
	t.Parallel()

	if os.Getenv("COMPOUND_TEST_COORDINATION_EXPLICIT_HOME") == "1" {
		cmdCoordination([]string{"--home", os.Getenv("COMPOUND_TEST_HOME"), "--json"})
		return
	}
	homePath := t.TempDir()
	workID := seedCoordinationCLIHome(t, homePath, "lane-explicit")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoordinationCLIUsesExplicitHome$")
	cmd.Env = append(os.Environ(),
		"COMPOUND_TEST_COORDINATION_EXPLICIT_HOME=1",
		"COMPOUND_TEST_HOME="+homePath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("coordination --home failed: %v\n%s", err, out)
	}
	var report work.CoordinationReport
	if err := json.Unmarshal([]byte(strings.SplitN(string(out), "\n", 2)[0]), &report); err != nil {
		t.Fatalf("parse explicit-home report: %v\n%s", err, out)
	}
	if report.Me.Actor != "lane-explicit" || report.Me.Doing == nil || report.Me.Doing.WorkID != workID {
		t.Fatalf("explicit home was not consumed: %+v", report.Me)
	}
}

// The coordination CLI composes ONE request-scoped capacity observation, bound
// to the home and the request instant the store will use. The seeded fixture
// advances its work to "running", which is NOT a dispatch point, so no CLI test
// ever reached the lane-capacity guard: had the CLI's binding drifted from the
// store's, every dispatch-point actor would have failed the binding and the
// command would exit non-zero with no other test failing. This one seeds
// CLAIMED work — a dispatch point — so the guard is actually called.
func TestCoordinationCLICapacityObservationBindingMatchesTheStore(t *testing.T) {
	t.Parallel()

	if os.Getenv("COMPOUND_TEST_COORDINATION_BINDING") == "1" {
		cmdCoordination([]string{"--home", os.Getenv("COMPOUND_TEST_HOME"), "--json"})
		return
	}
	now := time.Now().UTC()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("home init: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w, err := store.Create(work.CreateInput{Owner: "lane-x", DesiredOutcome: "ship the parser", SuccessPredicate: "parser lands"}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Claim(w.ID, "lane-x", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCoordinationCLICapacityObservationBindingMatchesTheStore$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(),
		"COMPOUND_TEST_COORDINATION_BINDING=1",
		"COMPOUND_TEST_HOME="+homePath,
		// HOME points at the temp dir so the CodexBar usage feed and the multi-auth
		// quota cache resolve inside it: the guard's answer must not depend on the
		// machine's real capacity files.
		"HOME="+homePath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("coordination with a dispatch-point actor failed: %v\n%s", err, out)
	}
	var report work.CoordinationReport
	if err := json.Unmarshal([]byte(strings.SplitN(string(out), "\n", 2)[0]), &report); err != nil {
		t.Fatalf("parse coordination report: %v\n%s", err, out)
	}
	if report.Me.Lane == nil {
		t.Fatalf("the lane-capacity guard was never reached: %+v", report.Me)
	}
}

func TestCoordinationMeasureCLIReportsObservedOutcome(t *testing.T) {
	t.Parallel()

	if os.Getenv("COMPOUND_TEST_COORDINATION_MEASURE") == "1" {
		cmdCoordination([]string{
			"measure",
			"--report", os.Getenv("COMPOUND_TEST_DELIVERY_REPORT"),
			"--home", os.Getenv("COMPOUND_TEST_HOME"),
			"--json",
		})
		return
	}
	homePath := t.TempDir()
	workID := seedCoordinationCLIHome(t, homePath, "lane-measure")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := artifacts.Put([]byte("bounded coordination body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCollaboration(workID, work.CollaborationInput{
		Actor:          "lane-measure",
		IdempotencyKey: "measure-cli",
		BodyRef:        bodyRef,
		DeliveryClass:  work.CollaborationDeliveryNextBoundary,
		Delta: work.CollaborationStateDelta{
			Kind: "work", SubjectRef: workID, Before: "running", After: "reviewed",
		},
		Affects: work.CollaborationAffects{WorkIDs: []string{workID}},
	}, cliTestNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	delivery, err := hookplan.DeliverCoordination(hookplan.DeliveryRequest{
		HomePath: homePath, Harness: "codex", CursorKey: "measure-cli",
		Recipient: hookplan.DeliveryRecipient{WorkID: workID},
		Trigger:   hookplan.DeliveryTriggerBoundary,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoordinationMeasureCLIReportsObservedOutcome$")
	cmd.Env = append(os.Environ(),
		"COMPOUND_TEST_COORDINATION_MEASURE=1",
		"COMPOUND_TEST_DELIVERY_REPORT="+delivery.ID,
		"COMPOUND_TEST_HOME="+homePath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("coordination measure failed: %v\n%s", err, out)
	}
	var receipt hookplan.DeliveryMeasurementReceipt
	if err := json.Unmarshal([]byte(strings.SplitN(string(out), "\n", 2)[0]), &receipt); err != nil {
		t.Fatalf("parse measurement receipt: %v\n%s", err, out)
	}
	if receipt.DeliveryReportID != delivery.ID || receipt.ID == "" {
		t.Fatalf("measurement does not identify delivery: %+v", receipt)
	}
	if len(receipt.CausedActionRefs) != 0 || len(receipt.ObservedActionRefs) != 0 ||
		len(receipt.ObservedRunAttemptRefs) != 0 || len(receipt.ObservedWorkRefs) != 0 {
		t.Fatalf("unchanged work measured a false outcome: %+v", receipt)
	}
}
