package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"compound/internal/envelope"
)

const DefaultBudget = 45 * time.Second

type BudgetEnvState string

const (
	BudgetEnvAbsent  BudgetEnvState = "absent"
	BudgetEnvValid   BudgetEnvState = "valid"
	BudgetEnvInvalid BudgetEnvState = "invalid"
)

type BudgetResolution struct {
	State     BudgetEnvState
	Budget    time.Duration
	Requested time.Duration
	Clamped   bool
}

type BudgetCommand struct {
	HookName     string
	FixturePath  string
	Enforce      bool
	Live         bool
	Harness      string
	ParityLogDir string
}

type BudgetAnswer struct {
	Result     Result
	StderrLine string
	Exit       int
}

func BudgetFromEnv(getenv func(string) string) time.Duration {
	raw := ""
	if getenv != nil {
		raw = getenv("COMPOUND_HOOK_BUDGET")
	}
	return ResolveBudget(raw, 0).Budget
}

// ResolveBudget treats the generic budget and any positive harness deadline as
// safety ceilings. An environment value may shorten that ceiling, never extend
// it; malformed and non-positive values retain the safe ceiling while exposing
// an invalid state to callers and tests.
func ResolveBudget(raw string, harnessBudget time.Duration) BudgetResolution {
	ceiling := DefaultBudget
	if harnessBudget > 0 && harnessBudget < ceiling {
		ceiling = harnessBudget
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return BudgetResolution{State: BudgetEnvAbsent, Budget: ceiling}
	}
	requested, err := time.ParseDuration(trimmed)
	if err != nil || requested <= 0 {
		return BudgetResolution{State: BudgetEnvInvalid, Budget: ceiling}
	}
	if requested > ceiling {
		return BudgetResolution{
			State:     BudgetEnvValid,
			Budget:    ceiling,
			Requested: requested,
			Clamped:   true,
		}
	}
	return BudgetResolution{
		State:     BudgetEnvValid,
		Budget:    requested,
		Requested: requested,
	}
}

func EnumName(name string) string {
	if value, ok := map[string]string{
		"session-start":      "SessionStart",
		"user-prompt-submit": "UserPromptSubmit",
		"pre-tool-use":       "PreToolUse",
		"post-tool-use":      "PostToolUse",
		"session-end":        "SessionEnd",
		"stop":               "Stop",
	}[name]; ok {
		return value
	}
	return name
}

func NormalizeHarness(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "-", "_")
}

func BudgetExpiryAnswer(command BudgetCommand, budget time.Duration, now time.Time) BudgetAnswer {
	blocked := command.Enforce && command.HookName == "pre-tool-use"
	status := "fail_open"
	failOpenReason := envelope.FailOpenBudgetExceeded
	policyPosture := "fail_open"
	effectiveOutcome := "allow"
	exitBehavior := "exit_0"
	exit := 0
	if blocked {
		status = "blocked"
		failOpenReason = ""
		policyPosture = "enforced_block"
		effectiveOutcome = "block"
		exitBehavior = "exit_4"
		exit = 4
	}
	event := envelope.HookEvent{
		Harness:             command.Harness,
		HookName:            EnumName(command.HookName),
		Status:              status,
		FailOpenReason:      failOpenReason,
		PolicyPosture:       policyPosture,
		EffectiveOutcome:    effectiveOutcome,
		HarnessExitBehavior: exitBehavior,
		EvidencePersistence: "not_persisted_watchdog_response",
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("budget:%s:%s:%d", command.Harness, command.HookName, now.UnixNano())))
	event.Header = envelope.Header{
		SchemaVersion: envelope.SVHookEvent,
		ID:            "hook_" + hex.EncodeToString(sum[:6]),
		CreatedAt:     now.UTC().Format(time.RFC3339Nano),
		Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
	}
	summary := fmt.Sprintf("hook exceeded its %s wall-clock budget; answered %s before the outer harness kill", budget, status)
	result := Result{Event: event, Summary: summary}
	return BudgetAnswer{
		Result:     result,
		Exit:       exit,
		StderrLine: fmt.Sprintf("[%s/%s] %s (budget_exceeded) — %s", command.Harness, status, event.HookName, summary),
	}
}

type AnswerGate struct {
	once        sync.Once
	watchdogWon atomic.Bool
}

func NewAnswerGate() *AnswerGate { return &AnswerGate{} }

func (g *AnswerGate) MainAnswer(publish func()) { g.once.Do(publish) }

func (g *AnswerGate) WatchdogAnswer(publish func()) bool {
	won := false
	g.once.Do(func() {
		won = true
		g.watchdogWon.Store(true)
		publish()
	})
	return won
}

func StartBudgetWatchdog(command BudgetCommand, gate *AnswerGate, now func() time.Time, publish func(BudgetAnswer)) func() {
	budget := BudgetFromEnv(os.Getenv)
	return StartBudgetWatchdogWithBudget(command, gate, now, budget, publish)
}

// StartBudgetWatchdogWithBudget arms the watchdog with a caller-resolved
// budget. The cmd layer takes the smaller of the generic and harness-specific
// ceilings, then permits a valid environment value to shorten it. Outer kill
// timeouts differ per harness (the OMP shim kills wrapped hooks at ~8s, so its
// internal deadline must answer at ~6s, not 45s), and the harness registry
// lives outside this package's import graph.
func StartBudgetWatchdogWithBudget(command BudgetCommand, gate *AnswerGate, now func() time.Time, budget time.Duration, publish func(BudgetAnswer)) func() {
	if !command.Live || budget <= 0 {
		return func() {}
	}
	timer := time.AfterFunc(budget, func() {
		clock := now
		if clock == nil {
			clock = time.Now
		}
		gate.WatchdogAnswer(func() { publish(BudgetExpiryAnswer(command, budget, clock())) })
	})
	return func() { timer.Stop() }
}

func ProducedKinds(produced []envelope.Envelope) []string {
	out := make([]string, 0, len(produced))
	for _, item := range produced {
		if item != nil {
			out = append(out, item.Kind())
		}
	}
	return out
}
