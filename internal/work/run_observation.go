package work

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

const RunObservationSchemaVersion = "compound.work_run_observation.v0.1"

// completeObservedRunAttempt is the terminal job-observation seam. Production
// delegates to RunAttempt.Complete; tests can force the persistence failure that
// ObserveRun must treat as enforcement failure rather than optional metadata.
var completeObservedRunAttempt = func(store *runattempt.Store, id, status string, now time.Time) (runattempt.RunAttempt, error) {
	return store.Complete(id, status, now)
}

var markObservedRunAttemptRunning = func(store *runattempt.Store, id string, now time.Time) (runattempt.RunAttempt, error) {
	return store.MarkRunning(id, now)
}

type ObserveRunInput struct {
	Actor   string
	TraceID string
	Note    string
	// JobFilePath is the optional path to a codex plugin job file
	// (executor_jobs.go codexJobFile shape). When set, ObserveRun reads the
	// job, mints (or retrieves) a deterministic RunAttempt from it, and
	// attaches the ratt_ id to the advance. C2: this is the producer that
	// gives observe-run a first-class execution record.
	JobFilePath string
	// S3 attribution (optional; observer-supplied). Present-flag stamps: a
	// non-empty value stamps the matching RunAttempt field; an absent flag
	// leaves the field at its zero value, which the omitempty tags drop from
	// the JSON. Stamped onto the job-file-minted RunAttempt while it is still
	// pending (before the status-map Complete), so the terminal-refusing
	// setters accept it.
	LaneID        string
	ModelID       string
	ProviderRoute string
	TokensIn      int64
	TokensOut     int64
}

type RunObservationResult struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	GeneratedAt   string `json:"generated_at"`
	Status        string `json:"status"`
	Actor         string `json:"actor"`
	Note          string `json:"note,omitempty"`
	ResultPath    string `json:"result_path,omitempty"`
	WorkID        string `json:"work_id"`
	FromState     string `json:"from_state"`
	State         string `json:"state"`
	TraceID       string `json:"trace_id"`
	// RunAttemptID is the ratt_* id minted from a --job-file (C2). Empty
	// when no job file was supplied.
	RunAttemptID     string                 `json:"run_attempt_id,omitempty"`
	ActionIntentID   string                 `json:"action_intent_id,omitempty"`
	PolicyDecisionID string                 `json:"policy_decision_id,omitempty"`
	Work             envelope.WorkEnvelope  `json:"work"`
	Trace            envelope.TraceEnvelope `json:"trace"`
	ActionContext    *WorkActionContext     `json:"action_context,omitempty"`
	NoAction         string                 `json:"no_action,omitempty"`
	Warnings         []string               `json:"warnings,omitempty"`
}

func (s *Store) ObserveRun(workID string, in ObserveRunInput, now time.Time) (RunObservationResult, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return RunObservationResult{}, fmt.Errorf("observe-run requires <work_id>")
	}
	traceID := strings.TrimSpace(in.TraceID)
	if traceID == "" {
		return RunObservationResult{}, fmt.Errorf("observe-run requires --trace <trace_id>")
	}
	if !strings.HasPrefix(traceID, "trace_") {
		return RunObservationResult{}, fmt.Errorf("observe-run trace id must start with trace_: %s", traceID)
	}
	w, err := s.Get(workID)
	if err != nil {
		return RunObservationResult{}, err
	}
	trace, err := s.GetTrace(traceID)
	if err != nil {
		return RunObservationResult{}, err
	}
	if trace.IDs.WorkID != "" && trace.IDs.WorkID != workID {
		return RunObservationResult{}, fmt.Errorf("trace %s belongs to work %s, not %s", trace.ID, trace.IDs.WorkID, workID)
	}
	actor := strings.TrimSpace(in.Actor)
	if actor == "" {
		actor = "codex"
	}
	note := strings.TrimSpace(in.Note)
	if note == "" {
		note = "observed run trace " + trace.ID
	}
	from := w.State
	// C2: when a job file is supplied, mint (or retrieve) a deterministic
	// RunAttempt from it and attach the ratt_ id to the advance. The
	// RunAttempt is execution evidence; the trace is verification evidence.
	var runAttemptID string
	var runAttemptIDs []string
	if strings.TrimSpace(in.JobFilePath) != "" {
		ra, raErr := s.observeRunFromJobFile(in, workID, actor, now)
		if raErr != nil {
			return RunObservationResult{}, fmt.Errorf("observe-run job file: %w", raErr)
		}
		runAttemptID = ra.ID
		runAttemptIDs = []string{ra.ID}
	}
	advanced, err := s.Advance(workID, AdvanceInput{
		To:             "verifying",
		Actor:          actor,
		VerificationID: trace.ID,
		RunAttemptIDs:  runAttemptIDs,
		Note:           note,
	}, now)
	if err != nil {
		return RunObservationResult{}, err
	}
	result := RunObservationResult{
		SchemaVersion:    RunObservationSchemaVersion,
		ID:               idFor("wro_", workID, trace.ID, actor, ts(now)),
		GeneratedAt:      now.UTC().Format(time.RFC3339),
		Status:           "observed",
		Actor:            actor,
		Note:             strings.TrimSpace(in.Note),
		WorkID:           workID,
		FromState:        from,
		State:            advanced.State,
		TraceID:          trace.ID,
		RunAttemptID:     runAttemptID,
		PolicyDecisionID: advanced.PolicyDecisionID,
		Work:             advanced,
		Trace:            trace,
		Warnings: []string{
			"attaches observed trace as verification evidence",
			"does not execute task commands",
			"does not close work",
			"does not emit receipt or transition proof",
		},
	}
	if actionContext, warnings := s.actionContextForWork(advanced); actionContext != nil || len(warnings) > 0 {
		result.ActionContext = actionContext
		result.Warnings = append(result.Warnings, warnings...)
	}
	if len(advanced.ActionIntentIDs) > 0 {
		result.ActionIntentID = advanced.ActionIntentIDs[len(advanced.ActionIntentIDs)-1]
	}
	path, err := s.RecordRunObservation(result)
	if err != nil {
		return RunObservationResult{}, err
	}
	result.ResultPath = path
	return result, nil
}

// observeRunFromJobFile reads a codex plugin job file (codexJobFile shape
// from executor_jobs.go), mints a deterministic RunAttempt from it (same
// job id → same ratt_ id, idempotent), and populates the S2 fields from
// the job's data. The RunAttempt's harness is "codex" (not
// HarnessPlaceholder) — this is the C2 producer that replaces the
// placeholder at the observe-run site.
//
// Status mapping: job "running" → StatusRunning (never terminal); job
// "completed"/"succeeded" → StatusSucceeded; job "failed" → StatusFailed.
// A running job's RunAttempt stays running until C3 sweeps it or a later
// observe-run with a completed job file terminalizes it.
func (s *Store) observeRunFromJobFile(in ObserveRunInput, workID, actor string, now time.Time) (runattempt.RunAttempt, error) {
	data, err := os.ReadFile(in.JobFilePath)
	if err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("read job file: %w", err)
	}
	var job codexJobFile
	if err := json.Unmarshal(data, &job); err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("parse job file: %w", err)
	}
	jobID := strings.TrimSpace(job.ID)
	if jobID == "" {
		jobID = strings.TrimSuffix(filepath.Base(in.JobFilePath), ".json")
	}
	// Deterministic id: same job → same ratt_ id → idempotent. The
	// timestamp component comes from the job's StartedAt (not the
	// observation time) so the same job always produces the same id
	// regardless of when it is observed.
	jobTime := now
	if strings.TrimSpace(job.StartedAt) != "" {
		if parsed, err := time.Parse(time.RFC3339, job.StartedAt); err == nil {
			jobTime = parsed
		}
	}
	rattID := runattempt.MintDeterministic("codex-job:"+jobID, jobTime)
	store := s.runAttempts()
	ra, err := store.MintWithID(rattID, workID, actor, "codex", now)
	if err != nil {
		return runattempt.RunAttempt{}, fmt.Errorf("mint from job %s: %w", jobID, err)
	}
	// F2: stamp provenance once, at genuine creation. Any record reaching
	// this path with an empty Origin was, by construction, minted through
	// this exact observe-run codepath (MintWithID with this deterministic
	// scheme has no other caller) -- so backfilling a pre-F2 legacy record's
	// Origin here is correct, not a guess. Skipped once Origin is set, and
	// never attempted on a terminal record (SetOrigin refuses on terminal,
	// same as every other setter).
	if ra.Origin == "" && !runattempt.IsTerminal(ra.Status) {
		if updated, oerr := store.SetOrigin(ra.ID, runattempt.OriginObservedExternal); oerr == nil {
			ra = updated
		}
	}
	// If the RunAttempt already existed as a TERMINAL record (idempotent
	// retrieve), return it without re-populating fields — the first
	// observer's data stands. A non-terminal existing record (pending or,
	// since F5, running) still needs to flow through the populate logic
	// below so a LATER terminal job file can still terminalize it (F5/T6) —
	// checking only "!= pending" would wrongly treat "running" as an
	// already-finished retrieve and never terminalize it.
	if runattempt.IsTerminal(ra.Status) {
		return ra, nil
	}
	// Populate S2 fields from the job data. Best-effort: a failure here
	// does not abort the observe-run (the RunAttempt exists, fields are
	// evidence, not enforcement).
	if strings.TrimSpace(job.LogFile) != "" {
		if updated, err := store.SetTranscript(ra.ID, job.LogFile); err == nil {
			ra = updated
		}
	}
	if strings.TrimSpace(job.Summary) != "" {
		if updated, err := store.SetCommand(ra.ID, "codex-job:"+job.Summary); err == nil {
			ra = updated
		}
	} else {
		if updated, err := store.SetCommand(ra.ID, "codex-job:"+jobID); err == nil {
			ra = updated
		}
	}
	// S3 attribution from the observer's flags (present-flag stamps). Empty
	// values are omitempty-dropped, so absent flags change nothing. Stamped
	// while the attempt is still pending (before the status-map Complete) so
	// the terminal-refusing setters accept it. Best-effort: attribution is
	// evidence, not enforcement -- the first observer's data stands (the
	// idempotent early-return above guards a re-observation).
	if strings.TrimSpace(in.LaneID) != "" || strings.TrimSpace(in.ModelID) != "" || strings.TrimSpace(in.ProviderRoute) != "" {
		if updated, err := store.SetAttribution(ra.ID, in.LaneID, in.ModelID, in.ProviderRoute); err == nil {
			ra = updated
		}
	}
	if in.TokensIn != 0 || in.TokensOut != 0 {
		if updated, err := store.SetTokenUsage(ra.ID, in.TokensIn, in.TokensOut); err == nil {
			ra = updated
		}
	}
	// Map job status to RunAttempt status. A running job stays running
	// (never terminal); a completed/failed job terminalizes.
	jobStatus := strings.ToLower(strings.TrimSpace(job.Status))
	switch jobStatus {
	case "completed", "succeeded", "done":
		updated, err := completeObservedRunAttempt(store, ra.ID, runattempt.StatusSucceeded, now)
		if err != nil {
			return runattempt.RunAttempt{}, fmt.Errorf("terminalize observed job %s as succeeded: %w", jobID, err)
		}
		ra = updated
	case "failed", "error":
		updated, err := completeObservedRunAttempt(store, ra.ID, runattempt.StatusFailed, now)
		if err != nil {
			return runattempt.RunAttempt{}, fmt.Errorf("terminalize observed job %s as failed: %w", jobID, err)
		}
		ra = updated
	case "running", "in_progress":
		// F5: mark genuinely running (was left as pending, contradicting
		// this function's own doc comment). C3 sweeps stuck attempts; a
		// later observe-run with a terminal job file still terminalizes it
		// (see the early-return guard above, which only skips a truly
		// terminal existing record).
		updated, err := markObservedRunAttemptRunning(store, ra.ID, now)
		if err != nil {
			return runattempt.RunAttempt{}, fmt.Errorf("mark observed job %s running: %w", jobID, err)
		}
		ra = updated
	default:
		// Unknown status: leave as pending. Doctor surfaces it.
	}
	return ra, nil
}

func (s *Store) GetTrace(traceID string) (envelope.TraceEnvelope, error) {
	var trace envelope.TraceEnvelope
	if err := s.readEventJSON(traceID, &trace); err != nil {
		return trace, err
	}
	if err := trace.Validate(); err != nil {
		return trace, err
	}
	return trace, nil
}

func (s *Store) RecordRunObservation(result RunObservationResult) (string, error) {
	if strings.TrimSpace(result.ID) == "" {
		return "", os.ErrInvalid
	}
	dir := filepath.Join(s.home, "run-observations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, result.ID+".json")
	if err := s.writeJSON(path, &result); err != nil {
		return "", err
	}
	return path, nil
}
