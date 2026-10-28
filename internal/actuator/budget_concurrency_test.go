package actuator

import (
	"context"
	"sync"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

type budgetBarrierAdapter struct {
	entered chan string
	release chan struct{}
	store   *runattempt.Store
}

func (a *budgetBarrierAdapter) Name() string { return "claude" }

func (a *budgetBarrierAdapter) Run(_ context.Context, in harness.RunInput) (harness.RunOutput, error) {
	ra, err := a.store.MintWithActionIntent(in.WorkID, "actuator-test", in.Harness, in.ActionIntentID, in.Now)
	if err != nil {
		return harness.RunOutput{}, err
	}
	a.entered <- in.WorkID
	<-a.release
	ra, err = a.store.Complete(ra.ID, runattempt.StatusSucceeded, in.Now)
	return harness.RunOutput{RunAttempt: ra}, err
}

// Hold the first actual consumer inside execution. A second admission must not
// dispatch while the one remaining action is already in flight.
func TestRunConcurrentAdmissionRespectsActionLimit(t *testing.T) {
	homePath := t.TempDir()
	for _, id := range []string{"work_budget_first", "work_budget_second"} {
		createKnownActuatorWork(t, homePath, id)
	}
	ledger := OpenLedger(homePath)
	budget := DefaultBudget(testNow)
	budget.Channels[0].MaxActions = 1
	if err := ledger.SaveBudget(budget); err != nil {
		t.Fatal(err)
	}
	adapter := &budgetBarrierAdapter{entered: make(chan string, 2), release: make(chan struct{}), store: runattempt.Open(homePath)}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(adapter.release) }) }
	original := buildAdapterRegistry
	buildAdapterRegistry = func(homePath string) *harness.AdapterRegistry {
		registry := harness.NewAdapterRegistry(runattempt.Open(homePath))
		if err := registry.RegisterAdapter(adapter); err != nil {
			panic(err)
		}
		return registry
	}
	t.Cleanup(func() { buildAdapterRegistry = original })
	type outcome struct {
		result RunResult
		err    error
	}
	first := make(chan outcome, 1)
	second := make(chan outcome, 1)
	var workers sync.WaitGroup
	t.Cleanup(func() {
		release()
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("budget admission workers did not stop before fixture cleanup")
		}
	})
	run := func(id string, done chan<- outcome) {
		defer workers.Done()
		result, err := Run(homePath, RunInput{WorkID: id, Harness: "claude", Prompt: "inspect", PermissionMode: "plan"}, policy.DefaultConfig(), testNow)
		done <- outcome{result, err}
	}
	workers.Add(1)
	go run("work_budget_first", first)
	select {
	case <-adapter.entered:
	case got := <-first:
		t.Fatalf("first run did not enter adapter: %+v", got)
	case <-time.After(5 * time.Second):
		t.Fatal("first run did not reach execution barrier")
	}
	workers.Add(1)
	go run("work_budget_second", second)
	var secondResult outcome
	secondDispatched := false
	select {
	case <-adapter.entered:
		secondDispatched = true
	case secondResult = <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second admission did not return or reach execution barrier")
	}
	release()
	firstResult := <-first
	if secondDispatched {
		secondResult = <-second
	}
	if firstResult.err != nil || !firstResult.result.Executed {
		t.Fatalf("first execution failed: %+v", firstResult)
	}
	if secondDispatched || secondResult.err != nil || !secondResult.result.Held || secondResult.result.Executed {
		t.Fatalf("one-action budget over-admitted: second dispatched=%t result=%+v error=%v", secondDispatched, secondResult.result, secondResult.err)
	}
	used, err := ledger.Used("", "execution", testNow.Add(-24*time.Hour))
	if err != nil || used.Actions != 1 {
		t.Fatalf("consumption = %+v, %v; want exactly one action", used, err)
	}
}
