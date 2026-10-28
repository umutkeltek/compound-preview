package main

import (
	"fmt"
	"os"
	"time"

	"compound/internal/harness"
	"compound/internal/hook"
)

type hookAnswerGate struct{ *hook.AnswerGate }

func newHookAnswerGate() *hookAnswerGate { return &hookAnswerGate{AnswerGate: hook.NewAnswerGate()} }

func (g *hookAnswerGate) mainAnswer(publish func()) { g.MainAnswer(publish) }

func (g *hookAnswerGate) watchdogAnswer(publish func()) bool {
	return g.WatchdogAnswer(publish)
}

func resolveHookBudget(parsed hookCommandArgs, raw string) time.Duration {
	var harnessBudget time.Duration
	if ms := harness.DefaultRegistry().HookBudgetMS(parsed.Harness); ms > 0 {
		harnessBudget = time.Duration(ms) * time.Millisecond
	}
	return hook.ResolveBudget(raw, harnessBudget).Budget
}

func startHookBudgetWatchdog(parsed hookCommandArgs, gate *hookAnswerGate, now func() time.Time) func() {
	// The safe ceiling is the smaller of the generic default and any
	// harness-specific deadline. An operator override may shorten that
	// ceiling, never extend it. For oh_my_pi this keeps the internal answer
	// at or below 6s so it fires before the shim's ~8s outer kill.
	raw := os.Getenv("COMPOUND_HOOK_BUDGET")
	budget := resolveHookBudget(parsed, raw)
	return hook.StartBudgetWatchdogWithBudget(parsed, gate.AnswerGate, now, budget, func(answer hook.BudgetAnswer) {
		body := liveHookResponseWithEnforcement(
			harness.DefaultRegistry(), parsed.Harness, parsed.HookName, answer.Result, parsed.Enforce, nil,
		)
		printJSON(body)
		fmt.Fprintln(os.Stderr, answer.StderrLine)
		os.Exit(answer.Exit)
	})
}
