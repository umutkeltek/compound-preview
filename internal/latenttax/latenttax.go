// Package latenttax is the one-week latent-tax rig: a COMPOUND-NATIVE, read-only
// evaluator organ that measures the cost/effect of standing context obligations
// across a factorial of placement arms. It is the substrate half of the rig — the
// arm/task schema, the run-index it owns, the metric extraction that READS existing
// ledger objects, and the pre-registered falsifier registry the report evaluates.
// The week of bulk arm runs happens LATER on cheap/free lanes via the dispatcher;
// this package never calls a provider or a network, never executes an arm, and has
// NO code path that promotes anything (the deliverable is a report + an
// adjudication REQUEST, never auto-promotion).
//
// It follows the internal/bench doctrine exactly: deterministic, zero-LLM,
// zero-authority-mutation; its output is EVIDENCE, not a decision. Two invariants
// keep the numbers trustworthy:
//
//   - It DERIVES every metric by reading the SAME ledger objects the runtime
//     already wrote for a run's work_id — thrash from PolicyDecision denials,
//     tokens/latency from execution-route-evidence rows, task success from the
//     receipt-graded verdict recorded on the run. It recomputes nothing and owns
//     no second metric store beyond a thin run-index JSONL under COMPOUND_HOME.
//   - Every degradation is tested against a LENGTH-MATCHED neutral-filler arm at
//     each instruction dose. A filler arm whose token length diverges from its
//     instruction arm beyond tolerance is a deterministic config error — so
//     "context length wearing an instruction costume" cannot masquerade as a tax.
//
// Design authority (private, cited by path only, never quoted into tracked files):
// the Opus consult sections 3-4 + the synthesis amendments, a $COMPOUND_HOME-adjacent
// evidence bundle. The three pre-registered kill-criteria this rig settles:
//   - filler >= instructions  -> kills the latent-tax framing (leaving keep-contexts-short)
//   - gate-only thrash >> both -> kills binary obligation placement (already amended to BOTH)
//   - observer ~ in-band on downstream quality -> kills terminating-trajectory as a practical lever
package latenttax

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Schema versions — this package owns its report/config shapes the way internal/bench
// owns its bench_report schema (a leaf report schema; no central registration).
const (
	TaskSetSchemaVersion   = "compound.latenttax_taskset.v0.1"
	ArmConfigSchemaVersion = "compound.latenttax_arms.v0.1"
	RunRecordSchemaVersion = "compound.latenttax_run.v0.1"
	FalsifierSchemaVersion = "compound.latenttax_falsifiers.v0.1"
	ReportSchemaVersion    = "compound.latenttax_report.v0.1"
)

// ObligationRelevance splits tasks by whether the obligation is ever exercised. A
// DORMANT task's default action never touches the obligation (isolates PURE latent
// tax — the obligation only sits in context); an ACTIVE task's default action would
// violate it (exercises the gate/thrash). Empty defaults to dormant so a minimal
// task set still isolates the tax.
const (
	Dormant = "dormant"
	Active  = "active"
)

// Task is one benchmark task definition. It is provider- and harness-neutral: a
// prompt body, the graded success criteria a judge lane checks, and the capability
// class the run requires. Generic content only — the tracked starter set ships no
// personal or product names.
type Task struct {
	ID                  string `json:"id"`
	Prompt              string `json:"prompt"`
	SuccessCriteria     string `json:"success_criteria"`
	CapabilityClass     string `json:"capability_class"`
	ObligationRelevance string `json:"obligation_relevance,omitempty"` // dormant|active
}

// Relevance returns the effective obligation relevance (default dormant).
func (t Task) Relevance() string {
	if strings.TrimSpace(t.ObligationRelevance) == "" {
		return Dormant
	}
	return t.ObligationRelevance
}

// TaskSet is the versioned benchmark task-set fixture. The dispatcher runs each
// task through every arm on cheap lanes; the rig scores the resulting ledger.
type TaskSet struct {
	SchemaVersion string `json:"schema_version"`
	TaskSetID     string `json:"task_set_id"`
	Version       string `json:"version"`
	Description   string `json:"description,omitempty"`
	Tasks         []Task `json:"tasks"`
}

// LoadTaskSet reads and validates a task-set JSON file.
func LoadTaskSet(path string) (TaskSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TaskSet{}, err
	}
	var ts TaskSet
	if err := json.Unmarshal(data, &ts); err != nil {
		return TaskSet{}, fmt.Errorf("parse task set %s: %w", path, err)
	}
	if err := ts.Validate(); err != nil {
		return TaskSet{}, err
	}
	return ts, nil
}

// Validate checks the task-set is well-formed: right schema, non-empty unique task
// ids, each task carries a prompt + graded success criteria + capability class, and
// obligation_relevance (when set) is a known value.
func (ts TaskSet) Validate() error {
	var errs []string
	if ts.SchemaVersion != TaskSetSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version=%q, want %q", ts.SchemaVersion, TaskSetSchemaVersion))
	}
	if strings.TrimSpace(ts.TaskSetID) == "" {
		errs = append(errs, "task_set_id is required")
	}
	if len(ts.Tasks) == 0 {
		errs = append(errs, "task set has no tasks")
	}
	seen := map[string]bool{}
	for i, t := range ts.Tasks {
		where := fmt.Sprintf("task[%d]", i)
		if strings.TrimSpace(t.ID) == "" {
			errs = append(errs, where+": id is required")
			continue
		}
		where = "task " + t.ID
		if seen[t.ID] {
			errs = append(errs, where+": duplicate id")
		}
		seen[t.ID] = true
		if strings.TrimSpace(t.Prompt) == "" {
			errs = append(errs, where+": prompt is required")
		}
		if strings.TrimSpace(t.SuccessCriteria) == "" {
			errs = append(errs, where+": success_criteria is required (a graded rubric the judge lane checks)")
		}
		if strings.TrimSpace(t.CapabilityClass) == "" {
			errs = append(errs, where+": capability_class is required")
		}
		if r := strings.TrimSpace(t.ObligationRelevance); r != "" && r != Dormant && r != Active {
			errs = append(errs, fmt.Sprintf("%s: obligation_relevance=%q, want %q or %q", where, r, Dormant, Active))
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("task set invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

// countTokens is the deterministic token proxy used for dose-matching: whitespace
// words. It has no LLM/tokenizer dependency, so the length-matched-filler invariant
// is a pure, reproducible check — the SAME input bytes always yield the same count.
func countTokens(s string) int {
	return len(strings.Fields(s))
}
