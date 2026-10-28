package actuator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/policy"
	"compound/internal/runattempt"
)

func reserveTestAction(t *testing.T, homePath, workID string) Consumption {
	t.Helper()
	plan, err := EvaluatePlan(homePath, runIntent(RunInput{WorkID: workID, Harness: "claude", PermissionMode: "plan"}, testNow), "execution", "", policy.DefaultConfig(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	plan, reservation, err := reservePlan(homePath, plan, testNow)
	if err != nil || plan.Decision.Mode != ModeAuto || reservation.ReservationID == "" {
		t.Fatalf("reservation failed: %+v, %v", plan, err)
	}
	return reservation
}

func TestBudgetReservationReopenSettlementAndExactRetry(t *testing.T) {
	homePath := t.TempDir()
	reservation := reserveTestAction(t, homePath, "work_budget_reopen")
	ledger := OpenLedger(homePath) // simulate a fresh process after admission
	if err := ledger.Record(reservation); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(settledConsumption(reservation, "ratt_budget_reopen")); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(settledConsumption(reservation, "ratt_budget_reopen")); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(reservation); err == nil {
		t.Fatal("settled reservation replayed as pending authority")
	}
	data, err := os.ReadFile(ledger.consumptionPath())
	if err != nil || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("retry appended extra charge: %q, %v", data, err)
	}
	used, err := ledger.Used("", "execution", testNow.Add(-time.Hour))
	if err != nil || used.Actions != 1 {
		t.Fatalf("settled usage = %+v, %v; want one", used, err)
	}
	used, err = ledger.Used("", "execution", testNow.Add(48*time.Hour))
	if err != nil || used.Actions != 0 {
		t.Fatalf("settled historical action must age normally: %+v, %v", used, err)
	}
}

func TestBudgetUnresolvedReservationDoesNotAgeOut(t *testing.T) {
	homePath := t.TempDir()
	budget := DefaultBudget(testNow)
	budget.Channels[0].MaxActions = 1
	if err := OpenLedger(homePath).SaveBudget(budget); err != nil {
		t.Fatal(err)
	}
	reserveTestAction(t, homePath, "work_budget_uncertain")
	plan, err := EvaluatePlan(homePath, runIntent(RunInput{WorkID: "work_budget_next", Harness: "claude", PermissionMode: "plan"}, testNow), "execution", "", policy.DefaultConfig(), testNow.Add(48*time.Hour))
	if err != nil || plan.Decision.Mode != ModeHold {
		t.Fatalf("unknown predecessor recovered allowance by aging: %+v, %v", plan, err)
	}
	if len(plan.Headroom.PendingReservationIDs) != 1 {
		t.Fatalf("unknown reservation is not actionable in budget projection: %+v", plan.Headroom)
	}
}

func TestRunUncertainOrContradictoryAttemptRetainsReservation(t *testing.T) {
	for _, scenario := range []string{"pending", "returned-terminal-only", "foreign-work", "file-id-mismatch", "terminal-failure"} {
		t.Run(scenario, func(t *testing.T) {
			homePath := t.TempDir()
			createKnownActuatorWork(t, homePath, "work_budget_attempt")
			stub := &stubClaudeAdapter{}
			switch scenario {
			case "pending":
				stub.status = runattempt.StatusPending
			case "returned-terminal-only":
				stub.status = runattempt.StatusPending
				stub.returnedStatus = runattempt.StatusSucceeded
			case "foreign-work":
				stub.contradictWork = true
			case "file-id-mismatch":
				stub.contradictStoredID = true
			case "terminal-failure":
				stub.status = runattempt.StatusFailed
				stub.runErr = errors.New("task failed")
			}
			installStubActuatorRegistry(t, stub)
			result, err := Run(homePath, RunInput{WorkID: "work_budget_attempt", Harness: "claude", PermissionMode: "plan"}, policy.DefaultConfig(), testNow)
			if err == nil || !result.Executed || result.RunAttemptID == "" {
				t.Fatalf("real partial execution must remain truthful and non-green: %+v, %v", result, err)
			}
			used, usageErr := OpenLedger(homePath).Used("", "execution", testNow.Add(48*time.Hour))
			if usageErr != nil {
				t.Fatal(usageErr)
			}
			if scenario == "terminal-failure" {
				if used.Actions != 0 || len(used.PendingReservationIDs) != 0 {
					t.Fatalf("known terminal failure must settle and age normally: %+v", used)
				}
			} else if used.Actions != 1 || len(used.PendingReservationIDs) != 1 {
				t.Fatalf("uncertain attempt recovered allowance: %+v", used)
			}
		})
	}
}

func TestBudgetFirstReservationSyncsAncestorsBeforeDispatch(t *testing.T) {
	homePath := t.TempDir()
	createKnownActuatorWork(t, homePath, "work_budget_durable")
	stub := &stubClaudeAdapter{}
	installStubActuatorRegistry(t, stub)
	original := syncBudgetDirectory
	var synced []string
	syncBudgetDirectory = func(path string) error {
		synced = append(synced, path)
		if path == homePath {
			return errors.New("home directory sync unavailable")
		}
		return original(path)
	}
	t.Cleanup(func() { syncBudgetDirectory = original })
	result, err := Run(homePath, RunInput{WorkID: "work_budget_durable", Harness: "claude", PermissionMode: "plan"}, policy.DefaultConfig(), testNow)
	if err == nil || result.Executed || stub.ran {
		t.Fatalf("undurable admission dispatched: %+v, %v", result, err)
	}
	want := []string{filepath.Join(homePath, "actuator", "budget"), filepath.Join(homePath, "actuator"), homePath}
	if strings.Join(synced, "\n") != strings.Join(want, "\n") {
		t.Fatalf("directory sync chain = %v; want %v", synced, want)
	}
	used, usageErr := OpenLedger(homePath).Used("", "execution", testNow.Add(48*time.Hour))
	if usageErr != nil || used.Actions != 1 || len(used.PendingReservationIDs) != 1 {
		t.Fatalf("uncertain append must remain accounted for: %+v, %v", used, usageErr)
	}
}

func TestBudgetLegacyConsumptionBytesAndUsagePreserved(t *testing.T) {
	homePath := t.TempDir()
	ledger := OpenLedger(homePath)
	if err := ledger.Record(Consumption{SchemaVersion: "compound.actuator_consumption.v0.1", At: testNow.Format(time.RFC3339), Channel: "execution", Actions: 1}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(ledger.consumptionPath())
	reservation := reserveTestAction(t, homePath, "work_budget_legacy")
	if err := ledger.Record(settledConsumption(reservation, "ratt_budget_legacy")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(ledger.consumptionPath())
	if !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("legacy rows rewritten during v0.2 admission")
	}
	used, err := ledger.Used("", "execution", testNow.Add(-time.Hour))
	if err != nil || used.Actions != 2 {
		t.Fatalf("legacy and settled charges counted incorrectly: %+v, %v", used, err)
	}
}

func TestBudgetSettlementRejectsChangedIdentityWithoutCorruptingLedger(t *testing.T) {
	for _, field := range []string{"work", "action", "channel", "timestamp", "actions", "receipt"} {
		t.Run(field, func(t *testing.T) {
			homePath := t.TempDir()
			reservation := reserveTestAction(t, homePath, "work_budget_integrity")
			ledger := OpenLedger(homePath)
			before, err := os.ReadFile(ledger.consumptionPath())
			if err != nil {
				t.Fatal(err)
			}
			settlement := settledConsumption(reservation, "ratt_budget_integrity")
			switch field {
			case "work":
				settlement.WorkID = "work_foreign"
			case "action":
				settlement.ActionIntentID = "act_foreign"
			case "channel":
				settlement.Channel = "foreign"
			case "timestamp":
				settlement.At = testNow.Add(time.Hour).Format(time.RFC3339)
			case "actions":
				settlement.Actions = 0
			case "receipt":
				settlement.ReceiptRef = ""
			}
			if err := ledger.Record(settlement); err == nil {
				t.Fatal("contradictory settlement accepted")
			}
			after, _ := os.ReadFile(ledger.consumptionPath())
			if string(before) != string(after) {
				t.Fatal("rejected settlement mutated ledger")
			}
		})
	}
}

func TestRunFiniteUnaccountedProviderLimitsHold(t *testing.T) {
	for _, dimension := range []string{"usd", "tokens"} {
		t.Run(dimension, func(t *testing.T) {
			homePath := t.TempDir()
			createKnownActuatorWork(t, homePath, "work_budget_cost")
			budget := DefaultBudget(testNow)
			if dimension == "usd" {
				budget.Channels[0].MaxSpendUSD = 1
			} else {
				budget.Channels[0].MaxTokens = 100
			}
			if err := OpenLedger(homePath).SaveBudget(budget); err != nil {
				t.Fatal(err)
			}
			stub := &stubClaudeAdapter{}
			installStubActuatorRegistry(t, stub)
			result, err := Run(homePath, RunInput{WorkID: "work_budget_cost", Harness: "claude", PermissionMode: "plan"}, policy.DefaultConfig(), testNow)
			if err != nil || !result.Held || result.Executed || stub.ran {
				t.Fatalf("unknown cost dispatched under finite %s ceiling: %+v, %v", dimension, result, err)
			}
		})
	}
}

// This helper is a separate OS process, not a goroutine sharing test locks.
func TestBudgetReservationProcessHelper(t *testing.T) {
	homePath := os.Getenv("COMPOUND_TEST_BUDGET_RESERVATION_HOME")
	if homePath == "" {
		return
	}
	plan, err := EvaluatePlan(homePath, runIntent(RunInput{WorkID: "work_budget_process", Harness: "claude", PermissionMode: "plan"}, testNow), "execution", "", policy.DefaultConfig(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "ready")
	if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	plan, _, err = reservePlan(homePath, plan, testNow)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, plan.Decision.Mode)
}

func TestBudgetReservationSerializesSeparateProcesses(t *testing.T) {
	homePath := t.TempDir()
	budget := DefaultBudget(testNow)
	budget.Channels[0].MaxActions = 1
	if err := OpenLedger(homePath).SaveBudget(budget); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type child struct {
		cmd    *exec.Cmd
		input  io.WriteCloser
		output *bufio.Reader
	}
	var children []child
	for i := 0; i < 2; i++ {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestBudgetReservationProcessHelper$")
		cmd.Env = append(os.Environ(), "COMPOUND_TEST_BUDGET_RESERVATION_HOME="+homePath)
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child{cmd, input, bufio.NewReader(output)})
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
	}
	// Both speculative plans must exist before either admission can reserve.
	for _, child := range children {
		line, err := child.output.ReadString('\n')
		if err != nil || line != "ready\n" {
			t.Fatalf("helper did not reach barrier: %q, %v", line, err)
		}
	}
	for _, child := range children {
		if _, err := child.input.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		child.input.Close()
	}
	auto, hold := 0, 0
	for _, child := range children {
		line, err := child.output.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "auto\n" {
			auto++
		} else if line == "hold\n" {
			hold++
		} else {
			t.Fatalf("unexpected helper verdict %q", line)
		}
		io.Copy(io.Discard, child.output)
		if err := child.cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if auto != 1 || hold != 1 {
		t.Fatalf("cross-process over-admission: auto=%d hold=%d", auto, hold)
	}
}
