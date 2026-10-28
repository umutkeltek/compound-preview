package latenttax

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Verdict is the receipt-graded task-success outcome. It is filled by the DISPATCHER
// after a cheap judge lane grades the run's receipt; the rig substrate only defines
// and reads it (blinding happens AT THE ADAPTER — the rig records which judge lane
// graded and whether it was blinded, it does not implement blinding).
const (
	VerdictUngraded = "ungraded" // planned but not yet run/graded
	VerdictPass     = "pass"
	VerdictFail     = "fail"
	VerdictPartial  = "partial"
)

var verdicts = []string{VerdictUngraded, VerdictPass, VerdictFail, VerdictPartial}

// CeremonyMode tags a run for the terminating-trajectory (Claim 3) axis. Most arm
// runs carry none; the dispatcher tags ceremony-bearing runs in_band vs observer so
// the SAME rig can evaluate the observer-vs-in-band kill-criterion when that data
// exists. Absent (empty) = none; the criterion then renders insufficient_data.
const (
	CeremonyNone     = "none"
	CeremonyInBand   = "in_band"
	CeremonyObserver = "observer"
)

var ceremonyModes = []string{"", CeremonyNone, CeremonyInBand, CeremonyObserver}

// RunRecord is the thin run-index line: the experimental FRAME for one arm-run. It
// maps a compound work_id to its arm/dose/task and records HOW the run was graded
// (judge lane id + blinded flag) plus the receipt the verdict cites. The rig OWNS
// this record (a JSONL under COMPOUND_HOME); EVERY other metric is DERIVED by reading
// ledger objects for work_id — thrash from PolicyDecision denials, tokens/latency
// from route evidence. Nothing here is authority; it is the join key + the grading
// provenance the ledger objects themselves do not carry (there is no task-success
// verdict object in the spine today — see package doc / handoff).
type RunRecord struct {
	SchemaVersion string `json:"schema_version"`
	RunID         string `json:"run_id"`
	TaskSetID     string `json:"task_set_id"`
	TaskID        string `json:"task_id"`
	ArmID         string `json:"arm_id"`
	ArmKind       string `json:"arm_kind"`
	Dose          int    `json:"dose"`
	WorkID        string `json:"work_id,omitempty"`       // dispatcher fills when it runs the arm
	JudgeLaneID   string `json:"judge_lane_id,omitempty"` // recorded, not implemented
	Blinded       bool   `json:"blinded,omitempty"`       // blinding at the adapter; recorded flag only
	ReceiptRef    string `json:"receipt_ref,omitempty"`   // rec_ id the graded verdict cites
	Verdict       string `json:"verdict"`                 // ungraded|pass|fail|partial
	CeremonyMode  string `json:"ceremony_mode,omitempty"` // none|in_band|observer (Claim 3 axis)
	CreatedAt     string `json:"created_at"`
}

// Validate checks one run record is well-formed.
func (r RunRecord) Validate() error {
	var errs []string
	if r.SchemaVersion != RunRecordSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version=%q, want %q", r.SchemaVersion, RunRecordSchemaVersion))
	}
	for _, f := range []struct{ name, val string }{
		{"run_id", r.RunID}, {"task_set_id", r.TaskSetID}, {"task_id", r.TaskID},
		{"arm_id", r.ArmID}, {"arm_kind", r.ArmKind}, {"created_at", r.CreatedAt},
	} {
		if strings.TrimSpace(f.val) == "" {
			errs = append(errs, f.name+" is required")
		}
	}
	if !oneOfStr(r.ArmKind, armKinds) {
		errs = append(errs, fmt.Sprintf("arm_kind=%q not one of %v", r.ArmKind, armKinds))
	}
	if !oneOfStr(r.Verdict, verdicts) {
		errs = append(errs, fmt.Sprintf("verdict=%q not one of %v", r.Verdict, verdicts))
	}
	if !oneOfStr(r.CeremonyMode, ceremonyModes) {
		errs = append(errs, fmt.Sprintf("ceremony_mode=%q not one of %v", r.CeremonyMode, ceremonyModes[1:]))
	}
	if r.WorkID != "" && !strings.HasPrefix(r.WorkID, "work_") {
		errs = append(errs, fmt.Sprintf("work_id %q must start with work_", r.WorkID))
	}
	if r.ReceiptRef != "" && !strings.HasPrefix(r.ReceiptRef, "rec_") {
		errs = append(errs, fmt.Sprintf("receipt_ref %q must start with rec_", r.ReceiptRef))
	}
	// A graded (non-ungraded) verdict must cite the work it graded — the whole point
	// is that metrics are DERIVED from that work's ledger objects.
	if r.Verdict != VerdictUngraded && strings.TrimSpace(r.WorkID) == "" {
		errs = append(errs, fmt.Sprintf("verdict=%q requires a work_id (the run must be bound to a compound work to derive metrics)", r.Verdict))
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("run record %s invalid: %s", r.RunID, strings.Join(errs, "; "))
	}
	return nil
}

// runID is deterministic: the same (task_set, task, arm, dose) always yields the same
// run id, so re-planning a matrix is idempotent and the dispatcher can dedupe.
func runID(taskSetID, taskID, armID string, dose int) string {
	h := sha256.Sum256([]byte(strings.Join([]string{taskSetID, taskID, armID, fmt.Sprintf("%d", dose)}, "|")))
	return "ltxrun_" + hex.EncodeToString(h[:])[:12]
}

// PlanRuns mints the full arm-run matrix as PROPOSED, ungraded run records: one per
// (task × dose × arm). It executes nothing, calls no provider, and binds no work_id —
// the dispatcher claims each record, runs the arm on a CHEAP lane, and fills
// work_id/receipt_ref/judge_lane_id/blinded/verdict later. createdAt is injected so
// the plan is deterministic in tests.
func PlanRuns(ts TaskSet, ac ArmConfig, createdAt string) ([]RunRecord, error) {
	if err := ts.Validate(); err != nil {
		return nil, err
	}
	if err := ac.Validate(); err != nil {
		return nil, err
	}
	var out []RunRecord
	for _, t := range ts.Tasks {
		for _, d := range ac.Doses {
			for _, a := range d.Arms {
				out = append(out, RunRecord{
					SchemaVersion: RunRecordSchemaVersion,
					RunID:         runID(ts.TaskSetID, t.ID, a.ID, d.Dose),
					TaskSetID:     ts.TaskSetID,
					TaskID:        t.ID,
					ArmID:         a.ID,
					ArmKind:       a.Kind,
					Dose:          d.Dose,
					Verdict:       VerdictUngraded,
					CreatedAt:     createdAt,
				})
			}
		}
	}
	return out, nil
}

// LoadRunIndex reads a run-index JSONL file, validating each record. A missing file
// is an empty index (no error) so a report over a not-yet-populated plan is legible.
func LoadRunIndex(path string) ([]RunRecord, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []RunRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	seen := map[string]bool{}
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var r RunRecord
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if seen[r.RunID] {
			return nil, fmt.Errorf("%s line %d: duplicate run_id %s", path, line, r.RunID)
		}
		seen[r.RunID] = true
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarshalRunIndex renders records as newline-delimited JSON (byte-stable order = the
// slice order). Used by the CLI to write the run index under COMPOUND_HOME.
func MarshalRunIndex(records []RunRecord) ([]byte, error) {
	var b strings.Builder
	for _, r := range records {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		line, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}
