package hook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
)

const (
	sessionEndAdapterAttemptSchema       = "compound.session_end_adapter_attempt.v0.1"
	sessionEndAdapterOutcomeSchema       = "compound.session_end_adapter_outcome.v0.1"
	sessionEndAdapterStatusAccepted      = "accepted"
	sessionEndAdapterStatusSucceeded     = "succeeded"
	sessionEndAdapterStatusTimedOut      = "timed_out"
	sessionEndAdapterStatusFailed        = "failed"
	sessionEndAdapterAttemptIDPrefix     = "hook_sessend_attempt_"
	sessionEndAdapterOutcomeIDPrefix     = "hook_sessend_outcome_"
	sessionEndAdapterDefaultTimeout      = 2 * time.Second
	sessionEndAdapterMaxLiveTimeout      = 5 * time.Second
	sessionEndAdapterMaxReplayTimeout    = 30 * time.Second
	sessionEndAdapterAcceptanceTimeout   = time.Second
	sessionEndAdapterPostKillReapTimeout = 250 * time.Millisecond
	sessionEndAdapterMaxInputBytes       = 1 << 20
	sessionEndAdapterMaxChildOutputBytes = 256 << 10
	sessionEndAdapterWorkerOverrideEnv   = "COMPOUND_SESSION_END_ADAPTER_ALLOW_WORKER_OVERRIDE"
	sessionEndAdapterUsage               = "usage: compound hook session-end-adapter --live --harness claude_code|claude_zai|claude_minimax|claude_qwen [--timeout 2s] OR compound hook session-end-adapter --replay hook_sessend_attempt_<id> [--timeout 30s]"
)

type sessionEndAdapterArgs struct {
	Harness    string
	Timeout    time.Duration
	TimeoutSet bool
	Worker     string
	ReplayID   string
}

type sessionEndAdapterPersistenceState string

const (
	sessionEndAdapterPersistenceCommitted    sessionEndAdapterPersistenceState = "committed"
	sessionEndAdapterPersistenceNotCommitted sessionEndAdapterPersistenceState = "not_committed"
	sessionEndAdapterPersistenceUnknown      sessionEndAdapterPersistenceState = "unknown"
)

type sessionEndAdapterInput struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	WorkID         string `json:"work_id"`
	CompoundWorkID string `json:"compound_work_id"`
}

type sessionEndAdapterAttempt struct {
	envelope.Header
	Harness       string   `json:"harness"`
	SessionID     string   `json:"session_id,omitempty"`
	WorkID        string   `json:"work_id,omitempty"`
	InputHash     string   `json:"input_hash"`
	InputRef      string   `json:"input_ref"`
	InputCustody  string   `json:"input_custody,omitempty"`
	PredecessorID string   `json:"predecessor_id,omitempty"`
	RetryArgv     []string `json:"retry_argv"`
	TimeoutMS     int64    `json:"timeout_ms"`
	Status        string   `json:"status"`
}

type sessionEndAdapterAcceptanceResult struct {
	Attempt          sessionEndAdapterAttempt
	PersistenceState sessionEndAdapterPersistenceState
	DeadlineExceeded bool
	Err              error
}

type sessionEndAdapterOutcome struct {
	envelope.Header
	AttemptID    string `json:"attempt_id"`
	ExecutionID  string `json:"execution_id"`
	InputRef     string `json:"input_ref"`
	InputCustody string `json:"input_custody,omitempty"`
	Status       string `json:"status"`
	ErrorClass   string `json:"error_class,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
}

type sessionEndAdapterOutcomeResult struct {
	Outcome          sessionEndAdapterOutcome
	PersistenceState sessionEndAdapterPersistenceState
	DeadlineExceeded bool
	Err              error
}

type sessionEndAdapterWorkerResult struct {
	Stdout   []byte
	Stderr   []byte
	Err      error
	TimedOut bool
	Duration time.Duration
}

// SessionEnd accepts only Claude's generic hook response fields. It does not
// accept hookSpecificOutput, whose event-specific union excludes SessionEnd.
type sessionEndAdapterResponse struct{}

// WriteSessionEndAdapterHomeUnavailable preserves Claude's native SessionEnd
// fail-open response when the CLI composition root refuses to hand the adapter
// a missing or incompatible writable home.
func WriteSessionEndAdapterHomeUnavailable(stdout, stderr io.Writer, err error) {
	writeSessionEndAdapterResponse(stdout)
	fmt.Fprintln(stderr, "session-end adapter: Compound home unavailable:", err)
}

// RunSessionEndAdapterCommand executes the bounded Claude-family SessionEnd
// adapter behind the thin cmd/compound dispatch seam.
func RunSessionEndAdapterCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	parsed, err := parseSessionEndAdapterArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if parsed.ReplayID != "" {
		homePath, err := home.Resolve("")
		if err != nil {
			fmt.Fprintln(stderr, "session-end adapter replay: resolve Compound home:", err)
			return 1
		}
		return runSessionEndAdapterReplay(parsed, homePath, stdout, stderr)
	}
	input, err := readSessionEndAdapterInput(stdin)
	if err != nil {
		writeSessionEndAdapterResponse(stdout)
		fmt.Fprintln(stderr, "session-end adapter: invalid live input:", err)
		return 0
	}
	var native sessionEndAdapterInput
	if err := json.Unmarshal(input, &native); err != nil {
		writeSessionEndAdapterResponse(stdout)
		fmt.Fprintln(stderr, "session-end adapter: parse live input:", err)
		return 0
	}
	if strings.TrimSpace(native.HookEventName) != "SessionEnd" {
		writeSessionEndAdapterResponse(stdout)
		fmt.Fprintf(stderr, "session-end adapter: hook_event_name=%q, want SessionEnd\n", native.HookEventName)
		return 0
	}
	if strings.TrimSpace(native.SessionID) == "" && strings.TrimSpace(native.WorkID) == "" && strings.TrimSpace(native.CompoundWorkID) == "" {
		writeSessionEndAdapterResponse(stdout)
		fmt.Fprintln(stderr, "session-end adapter: live input needs session_id, work_id, or compound_work_id")
		return 0
	}

	homePath, err := home.Resolve("")
	if err != nil {
		writeSessionEndAdapterResponse(stdout)
		fmt.Fprintln(stderr, "session-end adapter: resolve Compound home:", err)
		return 0
	}
	prepared := prepareSessionEndAdapterAcceptance(parsed, native, input, time.Now())
	acceptance := persistSessionEndAdapterAcceptanceBounded(
		homePath,
		prepared,
		input,
		sessionEndAdapterAcceptanceTimeout,
		nil,
	)
	if acceptance.PersistenceState != sessionEndAdapterPersistenceCommitted {
		writeSessionEndAdapterResponse(stdout)
		writeSessionEndAdapterAcceptanceError(stderr, acceptance)
		return 0
	}
	if acceptance.Err != nil {
		fmt.Fprintf(
			stderr,
			"session-end adapter: acceptance committed despite post-publication error: %v; state=committed; acceptance=%s\n",
			acceptance.Err,
			acceptance.Attempt.ID,
		)
	}

	executionID := sessionEndAdapterExecutionID(acceptance.Attempt.ID, time.Now())
	worker := runSessionEndAdapterWorker(parsed.Worker, parsed.Harness, input, parsed.Timeout)
	if len(worker.Stderr) > 0 {
		_, _ = stderr.Write(worker.Stderr)
		if worker.Stderr[len(worker.Stderr)-1] != '\n' {
			_, _ = io.WriteString(stderr, "\n")
		}
	}
	if worker.TimedOut {
		outcome := persistSessionEndAdapterOutcomeBounded(
			homePath,
			acceptance.Attempt,
			executionID,
			sessionEndAdapterStatusTimedOut,
			"deadline_exceeded",
			worker.Duration,
			time.Now(),
			sessionEndAdapterAcceptanceTimeout,
		)
		writeSessionEndAdapterResponse(stdout)
		writeSessionEndAdapterWorkerFailure(
			stderr,
			fmt.Sprintf("worker timed out after %s", parsed.Timeout),
			outcome,
			acceptance.Attempt,
		)
		writeSessionEndAdapterOutcomeError(stderr, outcome, acceptance.Attempt)
		return 0
	}
	if worker.Err != nil {
		outcome := persistSessionEndAdapterOutcomeBounded(
			homePath,
			acceptance.Attempt,
			executionID,
			sessionEndAdapterStatusFailed,
			"worker_exit",
			worker.Duration,
			time.Now(),
			sessionEndAdapterAcceptanceTimeout,
		)
		writeSessionEndAdapterResponse(stdout)
		writeSessionEndAdapterWorkerFailure(
			stderr,
			fmt.Sprintf("worker failed: %v", worker.Err),
			outcome,
			acceptance.Attempt,
		)
		writeSessionEndAdapterOutcomeError(stderr, outcome, acceptance.Attempt)
		return 0
	}
	if !validSessionEndAdapterWorkerOutput(worker.Stdout) {
		outcome := persistSessionEndAdapterOutcomeBounded(
			homePath,
			acceptance.Attempt,
			executionID,
			sessionEndAdapterStatusFailed,
			"invalid_worker_output",
			worker.Duration,
			time.Now(),
			sessionEndAdapterAcceptanceTimeout,
		)
		writeSessionEndAdapterResponse(stdout)
		writeSessionEndAdapterWorkerFailure(
			stderr,
			"worker emitted invalid hook JSON",
			outcome,
			acceptance.Attempt,
		)
		writeSessionEndAdapterOutcomeError(stderr, outcome, acceptance.Attempt)
		return 0
	}
	outcome := persistSessionEndAdapterOutcomeBounded(
		homePath,
		acceptance.Attempt,
		executionID,
		sessionEndAdapterStatusSucceeded,
		"",
		worker.Duration,
		time.Now(),
		sessionEndAdapterAcceptanceTimeout,
	)
	completeSessionEndAdapterWorkerSuccess(stdout, stderr, worker, acceptance, outcome)
	return 0
}

func persistSessionEndAdapterOutcomeBounded(
	homePath string,
	attempt sessionEndAdapterAttempt,
	executionID string,
	status string,
	errorClass string,
	duration time.Duration,
	now time.Time,
	timeout time.Duration,
) sessionEndAdapterOutcomeResult {
	result := make(chan sessionEndAdapterOutcomeResult, 1)
	go func() {
		outcome, state, err := persistSessionEndAdapterOutcomeDetailed(homePath, attempt, executionID, status, errorClass, duration, now)
		result <- sessionEndAdapterOutcomeResult{Outcome: outcome, PersistenceState: state, Err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case terminal := <-result:
		return terminal
	case <-timer.C:
		return sessionEndAdapterOutcomeResult{
			PersistenceState: sessionEndAdapterPersistenceUnknown,
			DeadlineExceeded: true,
			Err:              fmt.Errorf("timed out after %s", timeout),
		}
	}
}

func persistSessionEndAdapterOutcome(
	homePath string,
	attempt sessionEndAdapterAttempt,
	executionID string,
	status string,
	errorClass string,
	duration time.Duration,
	now time.Time,
) (sessionEndAdapterOutcome, error) {
	outcome, _, err := persistSessionEndAdapterOutcomeDetailed(homePath, attempt, executionID, status, errorClass, duration, now)
	return outcome, err
}

func persistSessionEndAdapterOutcomeDetailed(
	homePath string,
	attempt sessionEndAdapterAttempt,
	executionID string,
	status string,
	errorClass string,
	duration time.Duration,
	now time.Time,
) (sessionEndAdapterOutcome, sessionEndAdapterPersistenceState, error) {
	switch status {
	case sessionEndAdapterStatusSucceeded, sessionEndAdapterStatusTimedOut, sessionEndAdapterStatusFailed:
	default:
		return sessionEndAdapterOutcome{}, sessionEndAdapterPersistenceNotCommitted, fmt.Errorf("unknown SessionEnd adapter outcome %q", status)
	}
	if strings.TrimSpace(executionID) == "" {
		return sessionEndAdapterOutcome{}, sessionEndAdapterPersistenceNotCommitted, fmt.Errorf("SessionEnd adapter outcome requires execution_id")
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return sessionEndAdapterOutcome{}, sessionEndAdapterPersistenceNotCommitted, err
	}
	outcome := sessionEndAdapterOutcome{
		Header: envelope.Header{
			SchemaVersion: sessionEndAdapterOutcomeSchema,
			ID:            sessionEndAdapterOutcomeID(attempt.ID, executionID),
			CreatedAt:     now.UTC().Format(time.RFC3339Nano),
			Producer:      envelope.Producer{Kind: "hook_adapter", Name: "compound"},
			IDs:           attempt.IDs,
			Authority:     envelope.Authority{SourceClass: "evidence", Owner: "compound", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{"compound://events/" + attempt.ID, attempt.InputRef},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		AttemptID:    attempt.ID,
		ExecutionID:  executionID,
		InputRef:     attempt.InputRef,
		InputCustody: attempt.InputCustody,
		Status:       status,
		ErrorClass:   errorClass,
		DurationMS:   duration.Milliseconds(),
	}
	put, err := events.PutJSON(outcome.ID, outcome, "  ")
	state := classifySessionEndAdapterEventPersistence(events, outcome.ID, put, err)
	return outcome, state, err
}

func classifySessionEndAdapterEventPersistence(
	events *eventstore.Store,
	id string,
	put eventstore.PutResult,
	persistErr error,
) sessionEndAdapterPersistenceState {
	if persistErr == nil {
		return sessionEndAdapterPersistenceCommitted
	}
	if put.Created {
		return sessionEndAdapterPersistenceCommitted
	}
	if events == nil || strings.TrimSpace(id) == "" {
		return sessionEndAdapterPersistenceUnknown
	}
	_, err := events.Read(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sessionEndAdapterPersistenceNotCommitted
		}
		return sessionEndAdapterPersistenceUnknown
	}
	// Visibility without PutResult.Created can follow a failed Sync. It is not
	// durable enough to advertise a replayable identity.
	return sessionEndAdapterPersistenceUnknown
}

func writeSessionEndAdapterOutcomeError(stderr io.Writer, outcome sessionEndAdapterOutcomeResult, attempt sessionEndAdapterAttempt) {
	if outcome.PersistenceState == sessionEndAdapterPersistenceCommitted {
		if outcome.Err != nil {
			fmt.Fprintf(
				stderr,
				"session-end adapter: terminal outcome committed despite post-publication error: %v; state=committed; terminal=%s\n",
				outcome.Err,
				outcome.Outcome.ID,
			)
		}
		return
	}
	verb := "failed"
	if outcome.PersistenceState == sessionEndAdapterPersistenceUnknown {
		verb = "could not be confirmed"
		if outcome.DeadlineExceeded {
			verb = "timed out"
		}
	}
	fmt.Fprintf(
		stderr,
		"session-end adapter: terminal outcome persistence %s: %v; state=%s; acceptance %s remains pending with input_ref=%s\n",
		verb,
		outcome.Err,
		outcome.PersistenceState,
		attempt.ID,
		attempt.InputRef,
	)
}

func writeSessionEndAdapterAcceptanceError(stderr io.Writer, acceptance sessionEndAdapterAcceptanceResult) {
	if acceptance.PersistenceState == sessionEndAdapterPersistenceCommitted {
		return
	}
	if acceptance.PersistenceState == sessionEndAdapterPersistenceUnknown {
		verb := "could not be confirmed"
		if acceptance.DeadlineExceeded {
			verb = "timed out"
		}
		fmt.Fprintf(
			stderr,
			"session-end adapter: acceptance persistence %s: %v; state=unknown; worker not started; input_ref=%s\n",
			verb,
			acceptance.Err,
			acceptance.Attempt.InputRef,
		)
		return
	}
	fmt.Fprintf(
		stderr,
		"session-end adapter: acceptance persistence failed: %v; state=not_committed; worker not started; input_ref=%s\n",
		acceptance.Err,
		acceptance.Attempt.InputRef,
	)
}

func completeSessionEndAdapterWorkerSuccess(
	stdout io.Writer,
	stderr io.Writer,
	worker sessionEndAdapterWorkerResult,
	acceptance sessionEndAdapterAcceptanceResult,
	outcome sessionEndAdapterOutcomeResult,
) {
	if outcome.PersistenceState != sessionEndAdapterPersistenceCommitted {
		writeSessionEndAdapterResponse(stdout)
		writeSessionEndAdapterOutcomeError(stderr, outcome, acceptance.Attempt)
		return
	}
	writeSessionEndAdapterOutcomeError(stderr, outcome, acceptance.Attempt)
	fmt.Fprintf(
		stderr,
		"session-end adapter: worker succeeded; acceptance=%s terminal=%s input_ref=%s\n",
		acceptance.Attempt.ID,
		outcome.Outcome.ID,
		acceptance.Attempt.InputRef,
	)
	_, _ = stdout.Write(worker.Stdout)
	if len(worker.Stdout) == 0 || worker.Stdout[len(worker.Stdout)-1] != '\n' {
		_, _ = io.WriteString(stdout, "\n")
	}
}

func writeSessionEndAdapterWorkerFailure(
	stderr io.Writer,
	summary string,
	outcome sessionEndAdapterOutcomeResult,
	attempt sessionEndAdapterAttempt,
) {
	fmt.Fprintf(stderr, "session-end adapter: %s; retry evidence=%s", summary, attempt.ID)
	if outcome.PersistenceState == sessionEndAdapterPersistenceCommitted {
		fmt.Fprintf(stderr, " terminal=%s", outcome.Outcome.ID)
	}
	fmt.Fprintf(
		stderr,
		" input_ref=%s retry=%q\n",
		attempt.InputRef,
		strings.Join(attempt.RetryArgv, " "),
	)
}

func parseSessionEndAdapterArgs(args []string) (sessionEndAdapterArgs, error) {
	parsed := sessionEndAdapterArgs{Timeout: sessionEndAdapterDefaultTimeout}
	live := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--live":
			live = true
		case "--harness":
			if i+1 >= len(args) {
				return sessionEndAdapterArgs{}, fmt.Errorf(sessionEndAdapterUsage)
			}
			i++
			parsed.Harness = strings.TrimSpace(args[i])
		case "--timeout":
			if i+1 >= len(args) {
				return sessionEndAdapterArgs{}, fmt.Errorf(sessionEndAdapterUsage)
			}
			i++
			timeout, err := time.ParseDuration(strings.TrimSpace(args[i]))
			if err != nil {
				return sessionEndAdapterArgs{}, fmt.Errorf("session-end adapter: --timeout must be a duration")
			}
			parsed.Timeout = timeout
			parsed.TimeoutSet = true
		case "--replay":
			if i+1 >= len(args) {
				return sessionEndAdapterArgs{}, fmt.Errorf(sessionEndAdapterUsage)
			}
			i++
			parsed.ReplayID = strings.TrimSpace(args[i])
		case "--worker":
			if i+1 >= len(args) {
				return sessionEndAdapterArgs{}, fmt.Errorf(sessionEndAdapterUsage)
			}
			i++
			parsed.Worker = strings.TrimSpace(args[i])
		default:
			return sessionEndAdapterArgs{}, fmt.Errorf(sessionEndAdapterUsage)
		}
	}
	if live == (parsed.ReplayID != "") {
		return sessionEndAdapterArgs{}, fmt.Errorf(sessionEndAdapterUsage)
	}
	if live {
		canonical, ok := canonicalSessionEndHarness(parsed.Harness)
		if !ok {
			return sessionEndAdapterArgs{}, fmt.Errorf(sessionEndAdapterUsage)
		}
		if parsed.Timeout < 10*time.Millisecond || parsed.Timeout > sessionEndAdapterMaxLiveTimeout {
			return sessionEndAdapterArgs{}, fmt.Errorf("session-end adapter: live --timeout must be between 10ms and %s", sessionEndAdapterMaxLiveTimeout)
		}
		parsed.Harness = canonical
	} else {
		if parsed.Harness != "" || !strings.HasPrefix(parsed.ReplayID, sessionEndAdapterAttemptIDPrefix) {
			return sessionEndAdapterArgs{}, fmt.Errorf(sessionEndAdapterUsage)
		}
		if parsed.TimeoutSet && (parsed.Timeout < 10*time.Millisecond || parsed.Timeout > sessionEndAdapterMaxReplayTimeout) {
			return sessionEndAdapterArgs{}, fmt.Errorf("session-end adapter: replay --timeout must be between 10ms and %s", sessionEndAdapterMaxReplayTimeout)
		}
	}
	if parsed.Worker == "" {
		worker, err := os.Executable()
		if err != nil {
			return sessionEndAdapterArgs{}, fmt.Errorf("session-end adapter: resolve worker executable: %w", err)
		}
		parsed.Worker = worker
	} else {
		if os.Getenv(sessionEndAdapterWorkerOverrideEnv) != "1" {
			return sessionEndAdapterArgs{}, fmt.Errorf("session-end adapter: --worker is disabled outside an explicit test override")
		}
		if !sessionEndAdapterRunningUnderTest() {
			return sessionEndAdapterArgs{}, fmt.Errorf("session-end adapter: --worker is available only to a test binary")
		}
		if err := validateSessionEndAdapterWorker(parsed.Worker); err != nil {
			return sessionEndAdapterArgs{}, err
		}
	}
	return parsed, nil
}

func canonicalSessionEndHarness(value string) (string, bool) {
	switch strings.TrimSpace(value) {
	case "claude_code", "claude-code":
		return "claude_code", true
	case "claude_zai", "claude-zai":
		return "claude_zai", true
	case "claude_minimax", "claude-minimax":
		return "claude_minimax", true
	case "claude_qwen", "claude-qwen":
		return "claude_qwen", true
	default:
		return "", false
	}
}

func runSessionEndAdapterReplay(parsed sessionEndAdapterArgs, homePath string, stdout, stderr io.Writer) int {
	events, err := eventstore.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "session-end adapter replay: open event store:", err)
		return 1
	}
	record, err := events.Read(parsed.ReplayID)
	if err != nil {
		fmt.Fprintf(stderr, "session-end adapter replay: accepted attempt %s is unavailable: %v\n", parsed.ReplayID, err)
		return 1
	}
	var parent sessionEndAdapterAttempt
	if err := json.Unmarshal(record.Bytes, &parent); err != nil {
		fmt.Fprintln(stderr, "session-end adapter replay: decode accepted attempt:", err)
		return 1
	}
	if err := validateSessionEndAdapterAttempt(parent, parsed.ReplayID); err != nil {
		fmt.Fprintln(stderr, "session-end adapter replay:", err)
		return 1
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "session-end adapter replay: open artifact store:", err)
		return 1
	}
	input, err := sessionEndAdapterLoadAcceptedInput(artifacts, parent)
	if err != nil {
		fmt.Fprintf(stderr, "session-end adapter replay: load exact input %s: %v\n", parent.InputRef, err)
		return 1
	}
	if got := sessionEndAdapterInputHash(input); got != parent.InputHash {
		fmt.Fprintf(stderr, "session-end adapter replay: input hash %s does not match accepted %s\n", got, parent.InputHash)
		return 1
	}

	now := time.Now()
	attempt := parent
	canonicalHarness, _ := canonicalSessionEndHarness(parent.Harness)
	attempt.Harness = canonicalHarness
	attempt.ID = sessionEndAdapterAttemptID(attempt.Harness, parent.InputHash, now)
	attempt.CreatedAt = now.UTC().Format(time.RFC3339Nano)
	attempt.PredecessorID = parent.ID
	attempt.RetryArgv = sessionEndAdapterRetryArgv(attempt.ID)
	attempt.EvidenceRefs = []string{"compound://events/" + parent.ID, parent.InputRef}
	if parsed.TimeoutSet {
		attempt.TimeoutMS = parsed.Timeout.Milliseconds()
	}
	if _, err := events.PutJSON(attempt.ID, attempt, "  "); err != nil {
		fmt.Fprintln(stderr, "session-end adapter replay: persist replay attempt:", err)
		return 1
	}

	executionID := sessionEndAdapterExecutionID(attempt.ID, time.Now())
	worker := runSessionEndAdapterWorker(parsed.Worker, attempt.Harness, input, time.Duration(attempt.TimeoutMS)*time.Millisecond)
	if len(worker.Stderr) > 0 {
		_, _ = stderr.Write(worker.Stderr)
		if worker.Stderr[len(worker.Stderr)-1] != '\n' {
			_, _ = io.WriteString(stderr, "\n")
		}
	}
	status, errorClass := sessionEndAdapterWorkerOutcome(worker)
	outcome, err := persistSessionEndAdapterOutcome(homePath, attempt, executionID, status, errorClass, worker.Duration, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "session-end adapter replay: persist terminal outcome:", err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(outcome); err != nil {
		fmt.Fprintln(stderr, "session-end adapter replay: encode outcome:", err)
		return 1
	}
	if status != sessionEndAdapterStatusSucceeded {
		fmt.Fprintf(stderr, "session-end adapter replay: worker %s; retry=%q\n", status, strings.Join(attempt.RetryArgv, " "))
		return 1
	}
	return 0
}

func sessionEndAdapterLoadAcceptedInput(artifacts *home.ArtifactStore, attempt sessionEndAdapterAttempt) ([]byte, error) {
	switch strings.TrimSpace(attempt.InputCustody) {
	case "private_artifact":
		return artifacts.GetPrivate(attempt.InputRef)
	case "", "public_artifact":
		return artifacts.Get(attempt.InputRef)
	default:
		return nil, fmt.Errorf("SessionEnd adapter acceptance %s has unknown input_custody=%q", attempt.ID, attempt.InputCustody)
	}
}

func validateSessionEndAdapterAttempt(attempt sessionEndAdapterAttempt, requestedID string) error {
	if attempt.SchemaVersion != sessionEndAdapterAttemptSchema || attempt.ID != requestedID || !strings.HasPrefix(attempt.ID, sessionEndAdapterAttemptIDPrefix) {
		return fmt.Errorf("record %s is not a SessionEnd adapter acceptance", requestedID)
	}
	if _, ok := canonicalSessionEndHarness(attempt.Harness); !ok || attempt.Status != sessionEndAdapterStatusAccepted || attempt.InputRef == "" || attempt.InputHash == "" {
		return fmt.Errorf("SessionEnd adapter acceptance %s is incomplete", requestedID)
	}
	maxTimeout := sessionEndAdapterMaxLiveTimeout
	if attempt.PredecessorID != "" {
		if attempt.PredecessorID == attempt.ID || !strings.HasPrefix(attempt.PredecessorID, sessionEndAdapterAttemptIDPrefix) {
			return fmt.Errorf("SessionEnd adapter acceptance %s has invalid predecessor_id=%q", requestedID, attempt.PredecessorID)
		}
		maxTimeout = sessionEndAdapterMaxReplayTimeout
	}
	timeout := time.Duration(attempt.TimeoutMS) * time.Millisecond
	if timeout < 10*time.Millisecond || timeout > maxTimeout {
		return fmt.Errorf("SessionEnd adapter acceptance %s has invalid timeout_ms=%d", requestedID, attempt.TimeoutMS)
	}
	return nil
}

func sessionEndAdapterWorkerOutcome(worker sessionEndAdapterWorkerResult) (string, string) {
	if worker.TimedOut {
		return sessionEndAdapterStatusTimedOut, "deadline_exceeded"
	}
	if worker.Err != nil {
		return sessionEndAdapterStatusFailed, "worker_exit"
	}
	if !validSessionEndAdapterWorkerOutput(worker.Stdout) {
		return sessionEndAdapterStatusFailed, "invalid_worker_output"
	}
	return sessionEndAdapterStatusSucceeded, ""
}

func validSessionEndAdapterWorkerOutput(raw []byte) bool {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil || response == nil {
		return false
	}
	return len(response) == 0
}

func validateSessionEndAdapterWorker(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("session-end adapter: --worker must be an absolute path")
	}
	rel, err := filepath.Rel(os.TempDir(), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("session-end adapter: --worker must live under the test temporary directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("session-end adapter: inspect --worker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("session-end adapter: --worker must be a regular, non-symlink executable")
	}
	return nil
}

func sessionEndAdapterRunningUnderTest() bool {
	executable, err := os.Executable()
	if err != nil {
		return false
	}
	normalized := filepath.ToSlash(executable)
	return strings.HasSuffix(executable, ".test") || strings.Contains(normalized, "/go-build")
}

func readSessionEndAdapterInput(stdin io.Reader) ([]byte, error) {
	input, err := io.ReadAll(io.LimitReader(stdin, sessionEndAdapterMaxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, fmt.Errorf("empty stdin")
	}
	if len(input) > sessionEndAdapterMaxInputBytes {
		return nil, fmt.Errorf("stdin exceeds %d bytes", sessionEndAdapterMaxInputBytes)
	}
	return input, nil
}

func persistSessionEndAdapterAcceptanceBounded(
	homePath string,
	attempt sessionEndAdapterAttempt,
	input []byte,
	timeout time.Duration,
	afterPersist func(),
) sessionEndAdapterAcceptanceResult {
	result := make(chan sessionEndAdapterAcceptanceResult, 1)
	go func() {
		state, err := persistSessionEndAdapterAcceptanceDetailed(homePath, attempt, input)
		if state == sessionEndAdapterPersistenceCommitted && afterPersist != nil {
			afterPersist()
		}
		result <- sessionEndAdapterAcceptanceResult{Attempt: attempt, PersistenceState: state, Err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case accepted := <-result:
		return accepted
	case <-timer.C:
		return sessionEndAdapterAcceptanceResult{
			Attempt:          attempt,
			PersistenceState: sessionEndAdapterPersistenceUnknown,
			DeadlineExceeded: true,
			Err:              fmt.Errorf("timed out after %s", timeout),
		}
	}
}

func prepareSessionEndAdapterAcceptance(
	parsed sessionEndAdapterArgs,
	native sessionEndAdapterInput,
	input []byte,
	now time.Time,
) sessionEndAdapterAttempt {
	canonicalHarness, _ := canonicalSessionEndHarness(parsed.Harness)
	inputHash := sessionEndAdapterInputHash(input)
	inputRef := home.ArtifactRefPrefix + strings.TrimPrefix(inputHash, "sha256:")
	workID := strings.TrimSpace(native.WorkID)
	if workID == "" {
		workID = strings.TrimSpace(native.CompoundWorkID)
	}
	attemptID := sessionEndAdapterAttemptID(canonicalHarness, inputHash, now)
	return sessionEndAdapterAttempt{
		Header: envelope.Header{
			SchemaVersion: sessionEndAdapterAttemptSchema,
			ID:            attemptID,
			CreatedAt:     now.UTC().Format(time.RFC3339Nano),
			Producer:      envelope.Producer{Kind: "hook_adapter", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID, SessionID: strings.TrimSpace(native.SessionID)},
			Authority:     envelope.Authority{SourceClass: "evidence", Owner: "compound", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{inputRef},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Harness:      canonicalHarness,
		SessionID:    strings.TrimSpace(native.SessionID),
		WorkID:       workID,
		InputHash:    inputHash,
		InputRef:     inputRef,
		InputCustody: "private_artifact",
		RetryArgv:    sessionEndAdapterRetryArgv(attemptID),
		TimeoutMS:    parsed.Timeout.Milliseconds(),
		Status:       sessionEndAdapterStatusAccepted,
	}
}

func persistSessionEndAdapterAcceptance(
	homePath string,
	attempt sessionEndAdapterAttempt,
	input []byte,
) error {
	_, err := persistSessionEndAdapterAcceptanceDetailed(homePath, attempt, input)
	return err
}

func persistSessionEndAdapterAcceptanceDetailed(
	homePath string,
	attempt sessionEndAdapterAttempt,
	input []byte,
) (sessionEndAdapterPersistenceState, error) {
	events, err := eventstore.Open(homePath)
	if err != nil {
		return sessionEndAdapterPersistenceNotCommitted, err
	}
	if err := events.Preflight(); err != nil {
		return sessionEndAdapterPersistenceNotCommitted, err
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		return sessionEndAdapterPersistenceNotCommitted, err
	}
	inputRef, err := artifacts.PutPrivate(input)
	if err != nil {
		return sessionEndAdapterPersistenceNotCommitted, err
	}
	if inputRef != attempt.InputRef {
		return sessionEndAdapterPersistenceNotCommitted, fmt.Errorf("accepted input ref %s does not match prepared %s", inputRef, attempt.InputRef)
	}
	put, err := events.PutJSON(attempt.ID, attempt, "  ")
	state := classifySessionEndAdapterEventPersistence(events, attempt.ID, put, err)
	return state, err
}

func sessionEndAdapterAttemptID(harnessName, inputHash string, now time.Time) string {
	return sessionEndAdapterAttemptIDPrefix + sessionEndAdapterShortHash(harnessName, inputHash, now.UTC().Format(time.RFC3339Nano))
}

func sessionEndAdapterExecutionID(attemptID string, now time.Time) string {
	return "sessend_exec_" + sessionEndAdapterShortHash(attemptID, now.UTC().Format(time.RFC3339Nano))
}

func sessionEndAdapterOutcomeID(attemptID, executionID string) string {
	return sessionEndAdapterOutcomeIDPrefix + sessionEndAdapterShortHash(attemptID, executionID)
}

// sessionEndAdapterShortHash preserves the command-layer adapter's original
// NUL-delimited identity bytes so accepted evidence remains replayable after
// moving the implementation into the hook spine.
func sessionEndAdapterShortHash(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))[:12]
}

func sessionEndAdapterRetryArgv(attemptID string) []string {
	return []string{"compound", "hook", "session-end-adapter", "--replay", attemptID}
}

func runSessionEndAdapterWorker(workerPath, harnessName string, input []byte, timeout time.Duration) sessionEndAdapterWorkerResult {
	started := time.Now()
	command := exec.Command(workerPath, "hook", "session-end", "--live", "--harness", harnessName)
	setSessionEndAdapterProcessGroup(command)
	command.Stdin = bytes.NewReader(input)
	stdout := newSessionEndAdapterCapture(sessionEndAdapterMaxChildOutputBytes)
	stderr := newSessionEndAdapterCapture(sessionEndAdapterMaxChildOutputBytes)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return sessionEndAdapterWorkerResult{Err: err, Duration: time.Since(started)}
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waited:
		return sessionEndAdapterWorkerResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Err: err, Duration: time.Since(started)}
	case <-timer.C:
		killErr := killSessionEndAdapterProcessGroup(command)
		err, reaped := waitSessionEndAdapterReap(waited, sessionEndAdapterPostKillReapTimeout)
		if !reaped {
			err = fmt.Errorf("worker did not reap within %s after kill", sessionEndAdapterPostKillReapTimeout)
			if killErr != nil {
				err = fmt.Errorf("%w: %v", err, killErr)
			}
			return sessionEndAdapterWorkerResult{Err: err, TimedOut: true, Duration: time.Since(started)}
		}
		return sessionEndAdapterWorkerResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Err: err, TimedOut: true, Duration: time.Since(started)}
	}
}

func waitSessionEndAdapterReap(waited <-chan error, timeout time.Duration) (error, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waited:
		return err, true
	case <-timer.C:
		return nil, false
	}
}

type sessionEndAdapterCapture struct {
	buf       bytes.Buffer
	remaining int
}

func newSessionEndAdapterCapture(limit int) *sessionEndAdapterCapture {
	return &sessionEndAdapterCapture{remaining: limit}
}

func (w *sessionEndAdapterCapture) Write(p []byte) (int, error) {
	written := len(p)
	if w.remaining <= 0 {
		return written, nil
	}
	keep := len(p)
	if keep > w.remaining {
		keep = w.remaining
	}
	_, _ = w.buf.Write(p[:keep])
	w.remaining -= keep
	return written, nil
}

func (w *sessionEndAdapterCapture) Bytes() []byte {
	return append([]byte(nil), w.buf.Bytes()...)
}

func writeSessionEndAdapterResponse(stdout io.Writer) {
	_ = json.NewEncoder(stdout).Encode(sessionEndAdapterResponse{})
}

func sessionEndAdapterInputHash(input []byte) string {
	sum := sha256.Sum256(input)
	return "sha256:" + hex.EncodeToString(sum[:])
}
