package hook

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveBudgetTriStateAndSafetyCeiling(t *testing.T) {
	const overflow = "999999999999999999999999h"
	tests := []struct {
		name          string
		raw           string
		harnessBudget time.Duration
		wantState     BudgetEnvState
		wantBudget    time.Duration
		wantRequested time.Duration
		wantClamped   bool
	}{
		{name: "absent generic", wantState: BudgetEnvAbsent, wantBudget: 45 * time.Second},
		{name: "blank harness", raw: " \t", harnessBudget: 6 * time.Second, wantState: BudgetEnvAbsent, wantBudget: 6 * time.Second},
		{name: "valid shorter", raw: "2s", harnessBudget: 6 * time.Second, wantState: BudgetEnvValid, wantBudget: 2 * time.Second, wantRequested: 2 * time.Second},
		{name: "valid harness clamp", raw: "10s", harnessBudget: 6 * time.Second, wantState: BudgetEnvValid, wantBudget: 6 * time.Second, wantRequested: 10 * time.Second, wantClamped: true},
		{name: "valid generic clamp", raw: "1h", wantState: BudgetEnvValid, wantBudget: 45 * time.Second, wantRequested: time.Hour, wantClamped: true},
		{name: "invalid text", raw: "soon", harnessBudget: 6 * time.Second, wantState: BudgetEnvInvalid, wantBudget: 6 * time.Second},
		{name: "zero", raw: "0s", harnessBudget: 6 * time.Second, wantState: BudgetEnvInvalid, wantBudget: 6 * time.Second},
		{name: "negative", raw: "-5s", harnessBudget: 6 * time.Second, wantState: BudgetEnvInvalid, wantBudget: 6 * time.Second},
		{name: "overflow", raw: overflow, harnessBudget: 6 * time.Second, wantState: BudgetEnvInvalid, wantBudget: 6 * time.Second},
		{name: "unsafe harness budget cannot extend generic", harnessBudget: 90 * time.Second, wantState: BudgetEnvAbsent, wantBudget: 45 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveBudget(tt.raw, tt.harnessBudget)
			if got.State != tt.wantState || got.Budget != tt.wantBudget ||
				got.Requested != tt.wantRequested || got.Clamped != tt.wantClamped {
				t.Fatalf("ResolveBudget(%q, %v) = %+v, want state=%s budget=%v requested=%v clamped=%v",
					tt.raw, tt.harnessBudget, got, tt.wantState, tt.wantBudget, tt.wantRequested, tt.wantClamped)
			}
		})
	}
}

func TestAnswerGateConcurrentExactlyOnce(t *testing.T) {
	for attempt := 0; attempt < 200; attempt++ {
		gate := NewAnswerGate()
		start := make(chan struct{})
		var wg sync.WaitGroup
		var published atomic.Int32
		var watchdogWins atomic.Int32
		for worker := 0; worker < 32; worker++ {
			worker := worker
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if worker%2 == 0 {
					gate.MainAnswer(func() { published.Add(1) })
					return
				}
				if gate.WatchdogAnswer(func() { published.Add(1) }) {
					watchdogWins.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := published.Load(); got != 1 {
			t.Fatalf("attempt %d published %d answers, want exactly 1", attempt, got)
		}
		if got := watchdogWins.Load(); got > 1 {
			t.Fatalf("attempt %d reported %d watchdog winners, want at most 1", attempt, got)
		}
	}
}
