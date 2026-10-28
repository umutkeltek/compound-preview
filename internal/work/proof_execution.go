package work

import (
	"compound/internal/authority/gitrepository"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ProofCheckExecutionTimeoutFlag is the optional bounded active-operation budget
// of one canonical full proof run. It is distinct from --wait (queue waiting for
// the scarce proof resource) and from --ttl (the reservation lease lifetime).
const ProofCheckExecutionTimeoutFlag = "--execution-timeout"

const (
	// ProofBudgetPhasePreparation is the pre-admission phase: source snapshot
	// capture, canonical plan resolution, and remote-profile sealing. It runs
	// before the scarce proof resource is reserved, so a stalled preparation is
	// refused without ever touching the reservation.
	ProofBudgetPhasePreparation = "pre-admission preparation"
	// ProofBudgetPhaseProofRun is the admitted phase: the sealed proof run
	// itself, after the resource is reserved.
	ProofBudgetPhaseProofRun = "the admitted proof run"
)

// ProofExecutionBudgetError is the precise deadline reason of a bounded
// canonical proof run. It names the budget, the phase that exceeded it, and the
// active operation that was actually consumed — the queue wait for the scarce
// proof resource is explicitly not part of that consumption.
type ProofExecutionBudgetError struct {
	Limit    time.Duration
	Consumed time.Duration
	Phase    string
}

func (e *ProofExecutionBudgetError) Error() string {
	return fmt.Sprintf(
		"proof-check execution budget of %s expired during %s after %s of active operation (queue waiting for the proof resource is excluded)",
		e.Limit,
		e.Phase,
		e.Consumed,
	)
}

// ProofExecutionBudget is the conserved active-operation budget of one
// canonical full proof run.
//
// It covers two things and nothing else: the pre-admission source/remote-profile
// preparation, and the admitted sealed proof run. Waiting in the queue for the
// scarce proof resource is not active operation, so it never consumes the
// budget; the preparation that already happened does, and the admitted run
// continues with what is left rather than a fresh full budget.
//
// A nil budget is an unbounded run: every method accepts it.
type ProofExecutionBudget struct {
	limit time.Duration
	spent time.Duration
}

// NewProofExecutionBudget returns the budget for a positive limit. A
// non-positive limit means the option was omitted, and an unbounded run has no
// budget object at all.
func NewProofExecutionBudget(limit time.Duration) *ProofExecutionBudget {
	if limit <= 0 {
		return nil
	}
	return &ProofExecutionBudget{limit: limit}
}

// Charge records active operation that already happened.
func (b *ProofExecutionBudget) Charge(elapsed time.Duration) {
	if b == nil || elapsed <= 0 {
		return
	}
	b.spent += elapsed
}

// Remaining reports the active operation still available.
func (b *ProofExecutionBudget) Remaining() time.Duration {
	if b == nil {
		return 0
	}
	remaining := b.limit - b.spent
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ExpiredError renders the precise deadline reason for a phase that ran out of
// budget.
func (b *ProofExecutionBudget) ExpiredError(phase string) error {
	if b == nil {
		return nil
	}
	consumed := b.limit // The cause is rendered only once the active allowance expires.
	return &ProofExecutionBudgetError{
		Limit:    b.limit,
		Consumed: consumed,
		Phase:    phase,
	}
}

// PreparationContext bounds the pre-admission preparation with the whole
// budget. The remote-profile sealing used to run on an unbounded background
// context; it is preparation, so it now stops with the budget — before the
// reservation and before any remote launch.
func (b *ProofExecutionBudget) PreparationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if b == nil {
		return parent, func() {}
	}
	return context.WithTimeoutCause(parent, b.Remaining(), b.ExpiredError(ProofBudgetPhasePreparation))
}

// RunContext bounds the admitted proof run with what is LEFT of the budget.
// The deadline starts here, after the queue wait for the scarce proof resource:
// waiting is not active operation, so it must not consume the budget.
func (b *ProofExecutionBudget) RunContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		parent = context.Background()
	}
	if b == nil {
		return parent, func() {}, nil
	}
	if b.Remaining() <= 0 {
		return parent, func() {}, b.ExpiredError(ProofBudgetPhasePreparation)
	}
	ctx, cancel := context.WithTimeoutCause(parent, b.Remaining(), b.ExpiredError(ProofBudgetPhaseProofRun))
	return ctx, cancel, nil
}

// ProofExecutionBudgetCause reports the bounded-deadline reason that ended ctx,
// or nil when ctx ended for another reason (operator signal, gate failure) or
// has not ended at all.
func ProofExecutionBudgetCause(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if cause == nil {
		return nil
	}
	var budgetErr *ProofExecutionBudgetError
	if errors.As(cause, &budgetErr) {
		return budgetErr
	}
	return nil
}

// proofCheckOptionValue reads one option's value in both accepted spellings and
// reports how many times the option appeared. A present option with no value is
// a refusal, never a silently ignored budget.
func proofCheckOptionValue(args []string, name string) (value string, present bool, count int) {
	for i, arg := range args {
		switch {
		case arg == name:
			count++
			if count == 1 && i+1 < len(args) {
				value = args[i+1]
			}
		case strings.HasPrefix(arg, name+"="):
			count++
			if count == 1 {
				value = strings.TrimPrefix(arg, name+"=")
			}
		}
	}
	return value, count > 0, count
}

// ParseProofCheckExecutionTimeout parses the optional bounded execution budget.
//
// The budget is a CONSERVED active-operation allowance: it covers pre-admission
// source/remote-profile preparation plus the admitted sealed proof run. Queue
// waiting for the scarce proof resource is not active work and never consumes
// it. Zero means the option was omitted, which preserves the unbounded
// interactive behavior exactly.
//
// The option is refused in lightweight and recovery modes because those modes
// never run the canonical battery the budget would bound.
func ParseProofCheckExecutionTimeout(args []string) (time.Duration, error) {
	raw, present, count := proofCheckOptionValue(args, ProofCheckExecutionTimeoutFlag)
	if !present {
		return 0, nil
	}
	if count > 1 {
		return 0, fmt.Errorf("%s may be provided only once", ProofCheckExecutionTimeoutFlag)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "--") {
		return 0, fmt.Errorf("%s requires a positive duration such as 30m", ProofCheckExecutionTimeoutFlag)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive duration such as 30m: %w", ProofCheckExecutionTimeoutFlag, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as 30m", ProofCheckExecutionTimeoutFlag)
	}
	if ProofCheckLightweightMode(args) {
		return 0, fmt.Errorf("%s requires the canonical full proof lane", ProofCheckExecutionTimeoutFlag)
	}
	return parsed, nil
}

// ProofCheckArgsWithoutLegacyRecovery drops the legacy-recovery switch from the
// exact invocation argv. The retained legacy journal is matched on the argv it
// was actually invoked with, never on the recovery command that reads it.
func ProofCheckArgsWithoutLegacyRecovery(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--recover-legacy-retained" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// ProofCheckArgsWithResolvedOut rewrites the --out value in the exact
// invocation argv so the recorded identity names the resolved absolute path
// rather than whatever the caller typed.
func ProofCheckArgsWithResolvedOut(args []string, resolvedOut string) []string {
	out := append([]string(nil), args...)
	for i := range out {
		switch {
		case out[i] == "--out" && i+1 < len(out):
			out[i+1] = resolvedOut
			return out
		case strings.HasPrefix(out[i], "--out="):
			out[i] = "--out=" + resolvedOut
			return out
		}
	}
	return out
}

type ProofCheckReport struct {
	SchemaVersion  string                  `json:"schema_version"`
	ProfileKind    string                  `json:"profile_kind,omitempty"`
	Status         string                  `json:"status"`
	GeneratedAt    string                  `json:"generated_at"`
	Repo           string                  `json:"repo"`
	BaseRef        string                  `json:"base_ref,omitempty"`
	SourceSnapshot *gitrepository.Snapshot `json:"source_snapshot,omitempty"`
	DryRun         bool                    `json:"dry_run"`
	CIOnly         bool                    `json:"ci_only,omitempty"`
	CIConsole      bool                    `json:"ci_console,omitempty"`
	ArtifactRef    string                  `json:"artifact_ref,omitempty"`
	Warnings       []string                `json:"warnings,omitempty"`
	ProofRefs      []string                `json:"proof_refs"`
	Steps          []ProofCheckStep        `json:"steps"`
	// Presubmit is set only on a non-certifying presubmit report.
	Presubmit *ProofCheckPresubmit `json:"presubmit,omitempty"`
}

func ProofCheckVerdictLine(report ProofCheckReport, path string) string {
	if report.ProfileKind == ProofCheckProfilePresubmit {
		return ProofCheckPresubmitVerdictLine(report, path)
	}
	total := len(report.Steps)
	passed := 0
	skipped := 0
	failed := 0
	planned := 0
	firstFailed := ""
	var elapsedMS int64
	for _, step := range report.Steps {
		elapsedMS += step.DurationMS
		switch step.Status {
		case "ok":
			passed++
		case "skipped":
			skipped++
		case "failed":
			failed++
			if firstFailed == "" {
				firstFailed = step.Name
			}
		case "planned":
			planned++
		}
	}
	if report.ProfileKind == ProofCheckProfileDevelopment {
		out := ""
		if strings.TrimSpace(path) != "" {
			out = " out=" + path
		}
		if report.DryRun {
			return fmt.Sprintf("dev-check: planned non_certifying=true planned=%d total=%d elapsed_ms=%d%s", planned, total, elapsedMS, out)
		}
		if report.Status == "ok" {
			return fmt.Sprintf("dev-check: ok non_certifying=true passed=%d skipped=%d failed=%d total=%d elapsed_ms=%d%s", passed, skipped, failed, total, elapsedMS, out)
		}
		return fmt.Sprintf("dev-check: FAILED non_certifying=true passed=%d skipped=%d failed=%d total=%d elapsed_ms=%d first_failed=%s%s", passed, skipped, failed, total, elapsedMS, firstFailed, out)
	}
	if report.Status == "ok" {
		return fmt.Sprintf("battery: ok passed=%d skipped=%d failed=%d total=%d out=%s", passed, skipped, failed, total, path)
	}
	return fmt.Sprintf("battery: FAILED passed=%d skipped=%d failed=%d total=%d first_failed=%s out=%s", passed, skipped, failed, total, firstFailed, path)
}

func ProofCheckReleasePendingVerdictLine(report ProofCheckReport, path string) string {
	total := len(report.Steps)
	passed := 0
	skipped := 0
	failed := 0
	for _, step := range report.Steps {
		switch step.Status {
		case "ok":
			passed++
		case "skipped":
			skipped++
		case "failed":
			failed++
		}
	}
	return fmt.Sprintf(
		"battery: FAILED release_pending=true passed=%d skipped=%d failed=%d total=%d out=%s",
		passed,
		skipped,
		failed,
		total,
		path,
	)
}

type proofContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r proofContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, context.Cause(r.ctx)
	}
	return r.source.Read(p)
}

type proofContextWriter struct {
	ctx    context.Context
	target io.Writer
}

func (w proofContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, context.Cause(w.ctx)
	}
	return w.target.Write(p)
}

// copyProofContext checks cancellation on both sides of each bounded chunk. A syscall
// already blocked in the kernel still has to return before synchronous cleanup can finish.
func copyProofContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	n, err := io.CopyBuffer(proofContextWriter{ctx, dst}, proofContextReader{ctx, src}, make([]byte, 64<<10))
	if err == nil && ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	return n, err
}

const ProofCheckProfileDevelopment = "development"

func ProofCheckPostReportFailureVerdict(report ProofCheckReport, path string) string {
	total := len(report.Steps)
	passed := 0
	skipped := 0
	failed := 0
	for _, step := range report.Steps {
		switch step.Status {
		case "ok":
			passed++
		case "skipped":
			skipped++
		case "failed":
			failed++
		}
	}
	return fmt.Sprintf(
		"battery: FAILED post_report_error=true passed=%d skipped=%d failed=%d total=%d out=%s",
		passed,
		skipped,
		failed,
		total,
		path,
	)
}
