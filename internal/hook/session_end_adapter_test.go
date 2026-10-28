package hook

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
)

const sessionEndAdapterOutcomeEvidenceSchema = "compound.session_end_adapter_outcome.v0.1"

type sessionEndAdapterAttemptEvidence struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	Status        string   `json:"status"`
	SessionID     string   `json:"session_id"`
	InputRef      string   `json:"input_ref"`
	RetryArgv     []string `json:"retry_argv"`
}

type sessionEndAdapterOutcomeEvidence struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	AttemptID     string `json:"attempt_id"`
	InputRef      string `json:"input_ref"`
	Status        string `json:"status"`
}

func requireSessionEndAdapterDiagnosticValue(t *testing.T, output, marker string) string {
	t.Helper()
	index := strings.Index(output, marker)
	if index < 0 {
		t.Fatalf("SessionEnd adapter diagnostic omitted %q: %s", marker, output)
	}
	fields := strings.Fields(output[index+len(marker):])
	if len(fields) == 0 {
		t.Fatalf("SessionEnd adapter diagnostic has no value after %q: %s", marker, output)
	}
	return strings.Trim(fields[0], `"`)
}

func requireGenericSessionEndHookResponse(t *testing.T, raw []byte) {
	t.Helper()
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("SessionEnd response is not JSON: %v\n%s", err, raw)
	}
	if response == nil || len(response) != 0 {
		t.Fatalf("SessionEnd response must be the generic empty object, got keys=%v\n%s", response, raw)
	}
}

func TestSessionEndAdapterPostKillReapWaitIsBounded(t *testing.T) {
	waited := make(chan error)
	started := time.Now()
	_, reaped := waitSessionEndAdapterReap(waited, 20*time.Millisecond)
	elapsed := time.Since(started)
	if reaped {
		t.Fatal("a worker that never reports Wait completion was reported as reaped")
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("post-kill reap wait was not bounded: %s", elapsed)
	}
}

func TestSessionEndAdapterOutcomeIdentityIncludesExecution(t *testing.T) {
	attemptID := "hook_sessend_attempt_fixed"
	first := sessionEndAdapterOutcomeID(attemptID, "sessend_exec_first")
	second := sessionEndAdapterOutcomeID(attemptID, "sessend_exec_second")
	if first == second {
		t.Fatalf("distinct executions collapsed onto one terminal outcome id: %s", first)
	}
}

func TestSessionEndAdapterWorkerOutputUsesGenericSessionEndSchema(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		raw   string
		valid bool
	}{
		{name: "empty object", raw: `{}`, valid: true},
		{name: "empty object with whitespace", raw: "  {}\n", valid: true},
		{name: "null", raw: `null`},
		{name: "array", raw: `[]`},
		{name: "malformed", raw: `{`},
		{name: "legacy event-specific output", raw: `{"hookSpecificOutput":{"hookEventName":"SessionEnd"}}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := validSessionEndAdapterWorkerOutput([]byte(testCase.raw)); got != testCase.valid {
				t.Fatalf("validSessionEndAdapterWorkerOutput(%q) = %t, want %t", testCase.raw, got, testCase.valid)
			}
		})
	}

	worker := sessionEndAdapterWorkerResult{Stdout: []byte(`{"hookSpecificOutput":{"hookEventName":"SessionEnd"}}`)}
	status, errorClass := sessionEndAdapterWorkerOutcome(worker)
	if status != sessionEndAdapterStatusFailed || errorClass != "invalid_worker_output" {
		t.Fatalf("event-specific SessionEnd worker response = status %q class %q", status, errorClass)
	}
}

func TestSessionEndAdapterPersistenceStatesDistinguishCommittedFromNotCommitted(t *testing.T) {
	input := []byte("{\"hook_event_name\":\"SessionEnd\",\"session_id\":\"sess_persistence_states\"}\n")
	parsed := sessionEndAdapterArgs{Harness: "claude-code", Timeout: time.Second, Worker: "/unused"}
	native := sessionEndAdapterInput{HookEventName: "SessionEnd", SessionID: "sess_persistence_states"}

	committedHome := t.TempDir()
	committedAttempt := prepareSessionEndAdapterAcceptance(parsed, native, input, time.Unix(1_800_000_000, 1).UTC())
	// Assert on the synchronous persistence path rather than the deadline
	// wrapper: this test's subject is committed-vs-not_committed
	// classification, and routing it through a 1s wall-clock timer races real
	// fsync latency under parallel load (a slow-but-successful write can be
	// misclassified as unknown). Deadline behavior of the bounded wrapper is
	// covered separately by channel-synchronized deadline tests.
	committedState, committedErr := persistSessionEndAdapterAcceptanceDetailed(committedHome, committedAttempt, input)
	if committedState != sessionEndAdapterPersistenceCommitted || committedErr != nil {
		t.Fatalf("committed persistence state=%s err=%v", committedState, committedErr)
	}

	notCommittedHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(notCommittedHome, "artifacts"), []byte("blocks artifact directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	notCommittedAttempt := prepareSessionEndAdapterAcceptance(parsed, native, input, time.Unix(1_800_000_000, 2).UTC())
	notCommittedState, notCommittedErr := persistSessionEndAdapterAcceptanceDetailed(notCommittedHome, notCommittedAttempt, input)
	if notCommittedState != sessionEndAdapterPersistenceNotCommitted || notCommittedErr == nil {
		t.Fatalf("not-committed persistence state=%s err=%v", notCommittedState, notCommittedErr)
	}
}

func TestSessionEndAdapterPersistenceStateRequiresDurablePublicationSignal(t *testing.T) {
	compoundHome := t.TempDir()
	input := []byte("{\"hook_event_name\":\"SessionEnd\",\"session_id\":\"sess_reconcile_writer_error\"}\n")
	parsed := sessionEndAdapterArgs{Harness: "claude-code", Timeout: time.Second, Worker: "/unused"}
	native := sessionEndAdapterInput{HookEventName: "SessionEnd", SessionID: "sess_reconcile_writer_error"}
	attempt := prepareSessionEndAdapterAcceptance(parsed, native, input, time.Unix(1_800_000_000, 3).UTC())
	events, err := eventstore.Open(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	lateError := fmt.Errorf("injected error after durable publication")
	put, err := events.PutJSONAtomic(attempt.ID, attempt, "  ", eventstore.AtomicOptions{
		AfterPublish: func() error { return lateError },
	})
	if err == nil || !put.Created {
		t.Fatalf("post-publication fixture result=%+v err=%v", put, err)
	}
	if state := classifySessionEndAdapterEventPersistence(events, attempt.ID, put, err); state != sessionEndAdapterPersistenceCommitted {
		t.Fatalf("durably published record plus late error state=%s, want committed", state)
	}
	if state := classifySessionEndAdapterEventPersistence(events, attempt.ID, eventstore.PutResult{}, lateError); state != sessionEndAdapterPersistenceUnknown {
		t.Fatalf("visible record without durable publication signal state=%s, want unknown", state)
	}
	if state := classifySessionEndAdapterEventPersistence(events, sessionEndAdapterAttemptIDPrefix+"absent", eventstore.PutResult{}, lateError); state != sessionEndAdapterPersistenceNotCommitted {
		t.Fatalf("absent record after writer error state=%s, want not_committed", state)
	}
}

func TestSessionEndAdapterCommitAtDeadlineReportsUnknownPersistenceState(t *testing.T) {
	// Advance the virtual deadline only after real persistence has reached the
	// committed boundary and its goroutine is blocked on release. Disk latency
	// must not change which side of that boundary this test exercises.
	synctest.Test(t, func(t *testing.T) {
		compoundHome := t.TempDir()
		input := []byte("{\"hook_event_name\":\"SessionEnd\",\"session_id\":\"sess_commit_at_deadline\"}\n")
		parsed := sessionEndAdapterArgs{Harness: "claude-code", Timeout: time.Second, Worker: "/unused"}
		native := sessionEndAdapterInput{HookEventName: "SessionEnd", SessionID: "sess_commit_at_deadline"}
		prepared := prepareSessionEndAdapterAcceptance(parsed, native, input, time.Unix(1_800_000_000, 123).UTC())

		committed := make(chan struct{})
		release := make(chan struct{})
		result := persistSessionEndAdapterAcceptanceBounded(
			compoundHome,
			prepared,
			input,
			500*time.Millisecond,
			func() {
				close(committed)
				<-release
			},
		)
		defer close(release)
		select {
		case <-committed:
		default:
			t.Fatal("deadline fired before the acceptance commit boundary was reached")
		}
		if result.Err == nil || result.PersistenceState != sessionEndAdapterPersistenceUnknown {
			t.Fatalf("commit-at-deadline result=%+v, want truthful timeout/unknown state", result)
		}
		if result.Attempt.ID != prepared.ID || result.Attempt.InputRef != prepared.InputRef {
			t.Fatalf("pending identity was lost: result=%+v prepared=%+v", result.Attempt, prepared)
		}
		wantRetry := []string{"compound", "hook", "session-end-adapter", "--replay", prepared.ID}
		if strings.Join(result.Attempt.RetryArgv, "\x00") != strings.Join(wantRetry, "\x00") {
			t.Fatalf("pending retry argv=%q want=%q", result.Attempt.RetryArgv, wantRetry)
		}

		events, err := eventstore.Open(compoundHome)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := events.Read(prepared.ID); err != nil {
			t.Fatalf("committed acceptance is not exact-ID readable after deadline: %v", err)
		}
		artifacts, err := home.NewArtifactStore(compoundHome)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := artifacts.GetPrivate(prepared.InputRef)
		if err != nil {
			t.Fatalf("committed exact input is not privately replayable after deadline: %v", err)
		}
		if _, err := artifacts.Get(prepared.InputRef); err == nil {
			t.Fatalf("new SessionEnd input must not be present in public artifact custody: %s", prepared.InputRef)
		}
		if !bytes.Equal(stored, input) {
			t.Fatalf("pending input changed: want=%q got=%q", input, stored)
		}
	})
}

func TestSessionEndAdapterAcceptanceDiagnosticDistinguishesDeadlineFromAmbiguousError(t *testing.T) {
	attempt := sessionEndAdapterAttempt{InputRef: home.ArtifactRefPrefix + "known_input"}
	attempt.ID = sessionEndAdapterAttemptIDPrefix + "must_not_be_advertised"

	var ambiguous bytes.Buffer
	writeSessionEndAdapterAcceptanceError(&ambiguous, sessionEndAdapterAcceptanceResult{
		Attempt:          attempt,
		PersistenceState: sessionEndAdapterPersistenceUnknown,
		Err:              fmt.Errorf("sync result unavailable"),
	})
	if strings.Contains(ambiguous.String(), "timed out") || !strings.Contains(ambiguous.String(), "could not be confirmed") {
		t.Fatalf("synchronous ambiguity was misreported as a deadline: %s", ambiguous.String())
	}
	if strings.Contains(ambiguous.String(), attempt.ID) {
		t.Fatalf("ambiguous acceptance advertised an unconfirmed id: %s", ambiguous.String())
	}

	var deadline bytes.Buffer
	writeSessionEndAdapterAcceptanceError(&deadline, sessionEndAdapterAcceptanceResult{
		Attempt:          attempt,
		PersistenceState: sessionEndAdapterPersistenceUnknown,
		DeadlineExceeded: true,
		Err:              fmt.Errorf("timed out after 1s"),
	})
	if !strings.Contains(deadline.String(), "persistence timed out") || !strings.Contains(deadline.String(), "state=unknown") {
		t.Fatalf("deadline ambiguity was not explicit: %s", deadline.String())
	}
}

func TestSessionEndAdapterRejectsWorkerOverrideOutsideTestTemp(t *testing.T) {
	t.Setenv(sessionEndAdapterWorkerOverrideEnv, "1")
	_, err := parseSessionEndAdapterArgs([]string{
		"--live", "--harness", "claude-code", "--worker", "/usr/bin/true",
	})
	if err == nil || !strings.Contains(err.Error(), "test temporary directory") {
		t.Fatalf("non-temporary worker override error = %v", err)
	}
}

func TestSessionEndAdapterNormalizesClaudeProfileAliasesBeforeAcceptance(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"claude_code":    "claude_code",
		"claude-code":    "claude_code",
		"claude_zai":     "claude_zai",
		"claude-zai":     "claude_zai",
		"claude_minimax": "claude_minimax",
		"claude-minimax": "claude_minimax",
		"claude_qwen":    "claude_qwen",
		"claude-qwen":    "claude_qwen",
	}
	for input, want := range cases {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			parsed, err := parseSessionEndAdapterArgs([]string{"--live", "--harness", input})
			if err != nil {
				t.Fatalf("parse canonical/alias harness %q: %v", input, err)
			}
			if parsed.Harness != want {
				t.Fatalf("parsed harness = %q, want canonical %q", parsed.Harness, want)
			}
		})
	}
}

func TestSessionEndAdapterReplayAcceptsDashedEvidenceButExecutesCanonically(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell worker fixture requires a POSIX host")
	}
	t.Setenv(sessionEndAdapterWorkerOverrideEnv, "1")
	compoundHome := t.TempDir()
	t.Setenv("COMPOUND_HOME", compoundHome)

	input := []byte(`{"hook_event_name":"SessionEnd","session_id":"sess_legacy_alias"}`)
	parent := prepareSessionEndAdapterAcceptance(
		sessionEndAdapterArgs{Harness: "claude-zai", Timeout: time.Second},
		sessionEndAdapterInput{HookEventName: "SessionEnd", SessionID: "sess_legacy_alias"},
		input,
		time.Unix(1_800_000_000, 0).UTC(),
	)
	// Preserve the pre-canonicalization evidence shape explicitly: replay must
	// accept old dashed records but every new attempt and worker run is canonical.
	parent.Harness = "claude-zai"
	if err := persistSessionEndAdapterAcceptance(compoundHome, parent, input); err != nil {
		t.Fatal(err)
	}

	argvPath := filepath.Join(t.TempDir(), "worker-argv.txt")
	workerPath := filepath.Join(t.TempDir(), "successful-session-end-worker.sh")
	worker := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nprintf '%%s\\n' '{}'\n", argvPath)
	if err := os.WriteFile(workerPath, []byte(worker), 0o755); err != nil {
		t.Fatal(err)
	}
	// This control checks replay identity and argv, not host scheduling latency.
	// Use the existing manual replay bound; dedicated tests retain tight deadlines.
	var stdout, stderr bytes.Buffer
	if code := RunSessionEndAdapterCommand(
		[]string{"--replay", parent.ID, "--timeout", "30s", "--worker", workerPath},
		strings.NewReader("must-not-be-consumed"),
		&stdout,
		&stderr,
	); code != 0 {
		t.Fatalf("replay exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var outcome sessionEndAdapterOutcomeEvidence
	if err := json.Unmarshal(stdout.Bytes(), &outcome); err != nil {
		t.Fatalf("decode replay outcome: %v stdout=%s", err, stdout.String())
	}
	events, err := eventstore.Open(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	record, err := events.Read(outcome.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	var replay sessionEndAdapterAttempt
	if err := json.Unmarshal(record.Bytes, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Harness != "claude_zai" {
		t.Fatalf("replayed attempt harness = %q, want canonical claude_zai", replay.Harness)
	}
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(string(argv)), []string{"hook", "session-end", "--live", "--harness", "claude_zai"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("worker argv = %q, want %q", got, want)
	}
}

func TestSessionEndAdapterRetryArgvReplaysAcceptedExactInputByID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell worker fixture requires a POSIX host")
	}
	t.Setenv(sessionEndAdapterWorkerOverrideEnv, "1")
	compoundHome := t.TempDir()
	t.Setenv("COMPOUND_HOME", compoundHome)

	slowWorker := filepath.Join(t.TempDir(), "slow-session-end-worker.sh")
	if err := os.WriteFile(slowWorker, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	input := "{\n  \"hook_event_name\": \"SessionEnd\",\n  \"session_id\": \"sess_exact_replay\",\n  \"unknown_future_field\": [1, 2, 3]\n}\n"
	var liveStdout, liveStderr bytes.Buffer
	if code := RunSessionEndAdapterCommand([]string{
		"--live", "--harness", "claude-code", "--timeout", "1s", "--worker", slowWorker,
	}, strings.NewReader(input), &liveStdout, &liveStderr); code != 0 {
		t.Fatalf("live adapter exit=%d stdout=%s stderr=%s", code, liveStdout.String(), liveStderr.String())
	}

	const evidenceMarker = "retry evidence="
	after, ok := strings.CutPrefix(strings.TrimSpace(liveStderr.String()), "session-end adapter: worker timed out")
	if !ok || !strings.Contains(after, evidenceMarker) {
		t.Fatalf("timeout omitted retry identity: %s", liveStderr.String())
	}
	attemptID := strings.Fields(strings.SplitN(after, evidenceMarker, 2)[1])[0]
	if !strings.HasPrefix(attemptID, "hook_sessend_attempt_") {
		t.Fatalf("attempt id %q is not routed through the hook family", attemptID)
	}

	events, err := eventstore.Open(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	record, err := events.Read(attemptID)
	if err != nil {
		t.Fatalf("read accepted attempt by exact id: %v", err)
	}
	var attempt sessionEndAdapterAttemptEvidence
	if err := json.Unmarshal(record.Bytes, &attempt); err != nil {
		t.Fatal(err)
	}
	wantRetry := []string{"compound", "hook", "session-end-adapter", "--replay", attemptID}
	if strings.Join(attempt.RetryArgv, "\x00") != strings.Join(wantRetry, "\x00") {
		t.Fatalf("retry argv=%q want=%q", attempt.RetryArgv, wantRetry)
	}

	workerInput := filepath.Join(t.TempDir(), "replayed-input.json")
	successWorker := filepath.Join(t.TempDir(), "successful-session-end-worker.sh")
	worker := fmt.Sprintf("#!/bin/sh\nhead -c %d > %q\nprintf '%%s\\n' '{}'\n", len(input), workerInput)
	if err := os.WriteFile(successWorker, []byte(worker), 0o755); err != nil {
		t.Fatal(err)
	}
	var replayStdout, replayStderr bytes.Buffer
	// Keep the live one-second timeout above, but give this successful exact-input
	// replay the existing manual replay bound under parallel filesystem load.
	replayArgs := append(append([]string(nil), attempt.RetryArgv[3:]...), "--timeout", "30s", "--worker", successWorker)
	if code := RunSessionEndAdapterCommand(replayArgs, strings.NewReader("must-not-be-consumed"), &replayStdout, &replayStderr); code != 0 {
		partial, partialErr := os.ReadFile(workerInput)
		t.Fatalf("replay exit=%d stdout=%s stderr=%s worker_input_bytes=%d worker_input_err=%v", code, replayStdout.String(), replayStderr.String(), len(partial), partialErr)
	}
	gotInput, err := os.ReadFile(workerInput)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotInput) != input {
		t.Fatalf("replay changed accepted stdin:\nwant=%q\ngot=%q", input, gotInput)
	}
	var outcome sessionEndAdapterOutcomeEvidence
	if err := json.Unmarshal(replayStdout.Bytes(), &outcome); err != nil {
		t.Fatalf("decode replay outcome: %v stdout=%s", err, replayStdout.String())
	}
	if outcome.Status != sessionEndAdapterStatusSucceeded || !strings.HasPrefix(outcome.ID, "hook_sessend_outcome_") {
		t.Fatalf("replay outcome=%+v", outcome)
	}
	if _, err := events.Read(outcome.ID); err != nil {
		t.Fatalf("read replay outcome by exact id: %v", err)
	}

	// A replay without an override must inherit the stored replay timeout, not
	// fall back to the original live deadline or the parser's default.
	var inheritedStdout, inheritedStderr bytes.Buffer
	if code := RunSessionEndAdapterCommand(
		[]string{"--replay", outcome.AttemptID, "--worker", successWorker},
		strings.NewReader("must-not-be-consumed"), &inheritedStdout, &inheritedStderr,
	); code != 0 {
		t.Fatalf("inherited replay exit=%d stdout=%s stderr=%s", code, inheritedStdout.String(), inheritedStderr.String())
	}
	var inheritedOutcome sessionEndAdapterOutcomeEvidence
	if err := json.Unmarshal(inheritedStdout.Bytes(), &inheritedOutcome); err != nil {
		t.Fatal(err)
	}
	if inheritedOutcome.Status != sessionEndAdapterStatusSucceeded {
		t.Fatalf("inherited replay outcome=%+v", inheritedOutcome)
	}
	inheritedRecord, err := events.Read(inheritedOutcome.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	var inherited sessionEndAdapterAttempt
	if err := json.Unmarshal(inheritedRecord.Bytes, &inherited); err != nil {
		t.Fatal(err)
	}
	if inherited.TimeoutMS != 30000 || inherited.PredecessorID != outcome.AttemptID || inherited.Harness != "claude_code" {
		t.Fatalf("inherited replay timeout=%d predecessor=%q harness=%q", inherited.TimeoutMS, inherited.PredecessorID, inherited.Harness)
	}
	if inherited.InputRef != attempt.InputRef || inherited.InputHash != sessionEndAdapterInputHash([]byte(input)) {
		t.Fatalf("inherited replay changed accepted input identity: %+v", inherited)
	}
	inheritedInput, err := os.ReadFile(workerInput)
	if err != nil {
		t.Fatal(err)
	}
	if string(inheritedInput) != input {
		t.Fatalf("inherited replay changed accepted stdin: want=%q got=%q", input, inheritedInput)
	}

}

func TestSessionEndAdapterManualReplayCanUseLongerBoundedTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell worker fixture requires a POSIX host")
	}
	t.Setenv(sessionEndAdapterWorkerOverrideEnv, "1")
	compoundHome := t.TempDir()
	t.Setenv("COMPOUND_HOME", compoundHome)

	// The worker must exceed the 100ms live deadline yet complete inside the 1s
	// replay deadline. A fixed sleep races load: under contention the child can
	// be starved past the replay deadline (observed duration_ms≈1001-1003 red).
	// Synchronize instead of guessing: the live run is expected to time out
	// while the worker is still running, so the first worker invocation marks
	// readiness and then blocks on a gate file that only the replayed
	// invocation (a fresh process) removes. This keeps "exceeds the live
	// deadline" true by construction rather than by a sleep length.
	workerDir := t.TempDir()
	gatePath := filepath.Join(workerDir, "gate")
	workerPath := filepath.Join(workerDir, "slow-valid-session-end-worker.sh")
	worker := fmt.Sprintf("#!/bin/sh\nwhile [ -e %q ]; do sleep 0.05; done\nprintf '%%s\\n' '{}'\n", gatePath)
	if err := os.WriteFile(workerPath, []byte(worker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gatePath, []byte("gated"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := `{"hook_event_name":"SessionEnd","session_id":"sess_longer_manual_replay","unknown_future_field":{"kept":true}}`

	var liveStdout, liveStderr bytes.Buffer
	if code := RunSessionEndAdapterCommand([]string{
		"--live", "--harness", "claude-code", "--timeout", "100ms", "--worker", workerPath,
	}, strings.NewReader(input), &liveStdout, &liveStderr); code != 0 {
		t.Fatalf("live adapter exit=%d stdout=%s stderr=%s", code, liveStdout.String(), liveStderr.String())
	}
	if !strings.Contains(liveStderr.String(), "worker timed out after 100ms") {
		t.Fatalf("live attempt did not reproduce the short-deadline failure: %s", liveStderr.String())
	}
	// Release the gate only after the live deadline was observed to fire: the
	// replay's worker invocation now exits promptly, so the 1s replay deadline
	// only needs to cover process spawn plus a 50ms poll, not a fixed sleep
	// racing scheduler starvation.
	if err := os.Remove(gatePath); err != nil {
		t.Fatal(err)
	}
	parentID := requireSessionEndAdapterDiagnosticValue(t, liveStderr.String(), "retry evidence=")
	events, err := eventstore.Open(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	parentRecord, err := events.Read(parentID)
	if err != nil {
		t.Fatalf("read parent acceptance: %v", err)
	}
	var parent sessionEndAdapterAttempt
	if err := json.Unmarshal(parentRecord.Bytes, &parent); err != nil {
		t.Fatal(err)
	}
	if parent.TimeoutMS != 100 {
		t.Fatalf("parent timeout_ms=%d, want 100", parent.TimeoutMS)
	}

	var replayStdout, replayStderr bytes.Buffer
	if code := RunSessionEndAdapterCommand([]string{
		"--replay", parent.ID, "--timeout", "1s", "--worker", workerPath,
	}, strings.NewReader("must-not-be-consumed"), &replayStdout, &replayStderr); code != 0 {
		t.Fatalf("manual replay exit=%d stdout=%s stderr=%s", code, replayStdout.String(), replayStderr.String())
	}
	var outcome sessionEndAdapterOutcomeEvidence
	if err := json.Unmarshal(replayStdout.Bytes(), &outcome); err != nil {
		t.Fatalf("decode replay outcome: %v stdout=%s", err, replayStdout.String())
	}
	if outcome.Status != sessionEndAdapterStatusSucceeded {
		t.Fatalf("manual replay outcome=%+v stderr=%s", outcome, replayStderr.String())
	}
	childRecord, err := events.Read(outcome.AttemptID)
	if err != nil {
		t.Fatalf("read child replay attempt: %v", err)
	}
	var child sessionEndAdapterAttempt
	if err := json.Unmarshal(childRecord.Bytes, &child); err != nil {
		t.Fatal(err)
	}
	if child.PredecessorID != parent.ID {
		t.Fatalf("child predecessor_id=%q, want %q", child.PredecessorID, parent.ID)
	}
	if child.InputHash != parent.InputHash || child.InputRef != parent.InputRef {
		t.Fatalf("manual replay changed source identity: parent hash/ref=%s %s child=%s %s", parent.InputHash, parent.InputRef, child.InputHash, child.InputRef)
	}
	if child.TimeoutMS != 1000 {
		t.Fatalf("child timeout_ms=%d, want replay override 1000", child.TimeoutMS)
	}
}

func TestSessionEndAdapterRejectsManualReplayTimeoutAboveThirtySeconds(t *testing.T) {
	accepted, err := parseSessionEndAdapterArgs([]string{
		"--replay", sessionEndAdapterAttemptIDPrefix + "bounded", "--timeout", "30s",
	})
	if err != nil {
		t.Fatalf("30s manual replay timeout should be accepted: %v", err)
	}
	if accepted.Timeout != 30*time.Second {
		t.Fatalf("manual replay timeout=%s, want 30s", accepted.Timeout)
	}
	_, err = parseSessionEndAdapterArgs([]string{
		"--replay", sessionEndAdapterAttemptIDPrefix + "too_long", "--timeout", "30.001s",
	})
	if err == nil || !strings.Contains(err.Error(), "between 10ms and 30s") {
		t.Fatalf("over-bound manual replay timeout error=%v", err)
	}
}

func TestSessionEndAdapterBoundsAcceptancePersistenceBeforeStartingWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO persistence fixture requires a POSIX host")
	}

	compoundHome := t.TempDir()
	workerMarker := filepath.Join(t.TempDir(), "worker-started")
	workerPath := filepath.Join(t.TempDir(), "observable-session-end-worker.sh")
	worker := fmt.Sprintf("#!/bin/sh\ntouch %q\n", workerMarker)
	if err := os.WriteFile(workerPath, []byte(worker), 0o755); err != nil {
		t.Fatal(err)
	}
	input := `{"hook_event_name":"SessionEnd","session_id":"sess_slow_acceptance","cwd":"/repo/example"}`
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(input)))
	artifactDir := filepath.Join(compoundHome, "private", "artifacts", "sha256")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("mkfifo", filepath.Join(artifactDir, digest)).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, output)
	}

	t.Setenv("COMPOUND_HOME", compoundHome)
	t.Setenv(sessionEndAdapterWorkerOverrideEnv, "1")
	args := []string{
		"--live", "--harness", "claude-code",
		"--timeout", "100ms", "--worker", workerPath,
	}
	var stdout, stderr bytes.Buffer

	started := time.Now()
	code := RunSessionEndAdapterCommand(args, strings.NewReader(input), &stdout, &stderr)
	elapsed := time.Since(started)
	if code != 0 {
		t.Fatalf("SessionEnd adapter exit = %d after %s\nstdout=%s\nstderr=%s", code, elapsed, stdout.String(), stderr.String())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("SessionEnd acceptance exceeded its bounded handoff window: %s", elapsed)
	}
	requireGenericSessionEndHookResponse(t, stdout.Bytes())
	if !strings.Contains(stderr.String(), "acceptance persistence failed") || !strings.Contains(stderr.String(), "sha256:"+digest) {
		t.Fatalf("SessionEnd acceptance private-custody failure was not explicit: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "state=not_committed") {
		t.Fatalf("SessionEnd acceptance failure did not preserve non-committed state: %s", stderr.String())
	}
	for _, forbidden := range []string{"pending=", "retry=", sessionEndAdapterAttemptIDPrefix} {
		if strings.Contains(stderr.String(), forbidden) {
			t.Fatalf("unknown SessionEnd acceptance advertised uncommitted replay identity %q: %s", forbidden, stderr.String())
		}
	}
	if _, err := os.Stat(workerMarker); !os.IsNotExist(err) {
		t.Fatalf("SessionEnd worker started before durable acceptance: stat err=%v", err)
	}
}

func TestSessionEndAdapterReportsAcceptanceNotCommittedWithoutReplayIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell worker fixture requires a POSIX host")
	}
	t.Setenv(sessionEndAdapterWorkerOverrideEnv, "1")
	compoundHome := t.TempDir()
	t.Setenv("COMPOUND_HOME", compoundHome)
	if err := os.WriteFile(filepath.Join(compoundHome, "artifacts"), []byte("blocks artifact directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	workerMarker := filepath.Join(t.TempDir(), "worker-started")
	workerPath := filepath.Join(t.TempDir(), "observable-session-end-worker.sh")
	worker := fmt.Sprintf("#!/bin/sh\ntouch %q\n", workerMarker)
	if err := os.WriteFile(workerPath, []byte(worker), 0o755); err != nil {
		t.Fatal(err)
	}
	input := `{"hook_event_name":"SessionEnd","session_id":"sess_acceptance_not_committed"}`
	var stdout, stderr bytes.Buffer
	if code := RunSessionEndAdapterCommand([]string{
		"--live", "--harness", "claude-code", "--timeout", "100ms", "--worker", workerPath,
	}, strings.NewReader(input), &stdout, &stderr); code != 0 {
		t.Fatalf("SessionEnd adapter exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	requireGenericSessionEndHookResponse(t, stdout.Bytes())
	if !strings.Contains(stderr.String(), "acceptance persistence failed") || !strings.Contains(stderr.String(), "state=not_committed") {
		t.Fatalf("SessionEnd acceptance failure did not report a definite non-commit: %s", stderr.String())
	}
	for _, forbidden := range []string{"pending=", "retry=", sessionEndAdapterAttemptIDPrefix} {
		if strings.Contains(stderr.String(), forbidden) {
			t.Fatalf("failed SessionEnd acceptance advertised uncommitted replay identity %q: %s", forbidden, stderr.String())
		}
	}
	if _, err := os.Stat(workerMarker); !os.IsNotExist(err) {
		t.Fatalf("SessionEnd worker started after failed acceptance persistence: stat err=%v", err)
	}
}

func TestSessionEndAdapterReportsUnknownOutcomeWithoutTerminalIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell worker fixture requires a POSIX host")
	}
	t.Setenv(sessionEndAdapterWorkerOverrideEnv, "1")
	compoundHome := t.TempDir()
	t.Setenv("COMPOUND_HOME", compoundHome)

	workerPath := filepath.Join(t.TempDir(), "break-outcome-store-worker.sh")
	worker := "#!/bin/sh\nmv \"$COMPOUND_HOME/events\" \"$COMPOUND_HOME/events.accepted\"\ntouch \"$COMPOUND_HOME/events\"\nexit 9\n"
	if err := os.WriteFile(workerPath, []byte(worker), 0o755); err != nil {
		t.Fatal(err)
	}
	input := `{"hook_event_name":"SessionEnd","session_id":"sess_outcome_not_committed"}`
	var stdout, stderr bytes.Buffer
	if code := RunSessionEndAdapterCommand([]string{
		"--live", "--harness", "claude-code", "--timeout", "5s", "--worker", workerPath,
	}, strings.NewReader(input), &stdout, &stderr); code != 0 {
		t.Fatalf("SessionEnd adapter exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	requireGenericSessionEndHookResponse(t, stdout.Bytes())
	if !strings.Contains(stderr.String(), "terminal outcome persistence") || !strings.Contains(stderr.String(), "state=unknown") {
		t.Fatalf("SessionEnd outcome failure did not preserve unreadable-store ambiguity: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "terminal=") {
		t.Fatalf("failed SessionEnd outcome advertised an uncommitted terminal identity: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "retry evidence="+sessionEndAdapterAttemptIDPrefix) {
		t.Fatalf("failed SessionEnd outcome lost its known-committed acceptance identity: %s", stderr.String())
	}
}

func TestSessionEndAdapterRendersUnknownOutcomeWithoutTerminalIdentity(t *testing.T) {
	attempt := sessionEndAdapterAttempt{
		InputRef: home.ArtifactRefPrefix + "known_input",
		RetryArgv: []string{
			"compound", "hook", "session-end-adapter", "--replay", sessionEndAdapterAttemptIDPrefix + "known_committed",
		},
	}
	attempt.ID = sessionEndAdapterAttemptIDPrefix + "known_committed"
	outcome := sessionEndAdapterOutcomeResult{
		PersistenceState: sessionEndAdapterPersistenceUnknown,
		Err:              fmt.Errorf("timed out after 1s"),
	}
	var stderr bytes.Buffer
	writeSessionEndAdapterWorkerFailure(&stderr, "worker failed", outcome, attempt)
	writeSessionEndAdapterOutcomeError(&stderr, outcome, attempt)

	if strings.Contains(stderr.String(), "terminal=") {
		t.Fatalf("unknown SessionEnd outcome advertised a terminal identity: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "state=unknown") {
		t.Fatalf("unknown SessionEnd outcome hid deadline ambiguity: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "retry evidence="+attempt.ID) {
		t.Fatalf("unknown SessionEnd outcome lost its known-committed acceptance identity: %s", stderr.String())
	}
}

func TestSessionEndAdapterCommittedOutcomeRemainsUsableAfterPostPublicationError(t *testing.T) {
	attempt := sessionEndAdapterAttempt{InputRef: home.ArtifactRefPrefix + "committed_input"}
	attempt.ID = sessionEndAdapterAttemptIDPrefix + "committed"
	acceptance := sessionEndAdapterAcceptanceResult{Attempt: attempt, PersistenceState: sessionEndAdapterPersistenceCommitted}
	outcome := sessionEndAdapterOutcomeResult{
		Outcome: sessionEndAdapterOutcome{
			AttemptID: attempt.ID,
			InputRef:  attempt.InputRef,
			Status:    sessionEndAdapterStatusSucceeded,
		},
		PersistenceState: sessionEndAdapterPersistenceCommitted,
		Err:              fmt.Errorf("late lock release error"),
	}
	outcome.Outcome.ID = sessionEndAdapterOutcomeIDPrefix + "committed"
	worker := sessionEndAdapterWorkerResult{Stdout: []byte("{}\n")}
	var stdout, stderr bytes.Buffer
	completeSessionEndAdapterWorkerSuccess(&stdout, &stderr, worker, acceptance, outcome)

	if !bytes.Equal(stdout.Bytes(), worker.Stdout) {
		t.Fatalf("committed outcome lost worker response: got=%q want=%q", stdout.String(), worker.Stdout)
	}
	for _, want := range []string{"worker succeeded", "state=committed", "terminal=" + outcome.Outcome.ID} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("committed late-error outcome omitted %q: %s", want, stderr.String())
		}
	}
}

func TestSessionEndAdapterTimesOutSlowWorkerWithoutCancellingHookAndKeepsRetryEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell worker fixture requires a POSIX host")
	}

	compoundHome := t.TempDir()
	workerPath := filepath.Join(t.TempDir(), "slow-session-end-worker.sh")
	if err := os.WriteFile(workerPath, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	input := `{"hook_event_name":"SessionEnd","session_id":"sess_slow_worker","cwd":"/repo/example","transcript_path":"/repo/example/transcript.jsonl"}`

	t.Setenv("COMPOUND_HOME", compoundHome)
	t.Setenv(sessionEndAdapterWorkerOverrideEnv, "1")
	args := []string{
		"--live", "--harness", "claude-code",
		"--timeout", "100ms", "--worker", workerPath,
	}
	var stdout, stderr bytes.Buffer

	started := time.Now()
	code := RunSessionEndAdapterCommand(args, strings.NewReader(input), &stdout, &stderr)
	elapsed := time.Since(started)
	if code != 0 {
		t.Fatalf("SessionEnd adapter exit = %d after %s\nstdout=%s\nstderr=%s", code, elapsed, stdout.String(), stderr.String())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("SessionEnd adapter exceeded its bounded handoff window: %s", elapsed)
	}

	requireGenericSessionEndHookResponse(t, stdout.Bytes())
	if !strings.Contains(stderr.String(), "timed out") || !strings.Contains(stderr.String(), home.ArtifactRefPrefix) {
		t.Fatalf("SessionEnd adapter hid its timeout/retry evidence: %s", stderr.String())
	}

	store, err := eventstore.Open(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := requireSessionEndAdapterDiagnosticValue(t, stderr.String(), "retry evidence=")
	outcomeID := requireSessionEndAdapterDiagnosticValue(t, stderr.String(), "terminal=")
	attemptRecord, err := store.Read(attemptID)
	if err != nil {
		t.Fatalf("read timeout acceptance by exact id: %v", err)
	}
	outcomeRecord, err := store.Read(outcomeID)
	if err != nil {
		t.Fatalf("read timeout outcome by exact id: %v", err)
	}
	var attempt sessionEndAdapterAttemptEvidence
	var outcome sessionEndAdapterOutcomeEvidence
	if err := json.Unmarshal(attemptRecord.Bytes, &attempt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(outcomeRecord.Bytes, &outcome); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "accepted" || attempt.SessionID != "sess_slow_worker" || attempt.InputRef == "" {
		t.Fatalf("durable SessionEnd handoff = %+v", attempt)
	}
	if outcome.Status != "timed_out" || outcome.AttemptID != attempt.ID || outcome.InputRef != attempt.InputRef {
		t.Fatalf("durable SessionEnd terminal outcome = %+v, acceptance=%+v", outcome, attempt)
	}
	if strings.Join(attempt.RetryArgv, "\x00") != strings.Join([]string{"compound", "hook", "session-end-adapter", "--replay", attempt.ID}, "\x00") {
		t.Fatalf("durable SessionEnd handoff lost retry argv: %+v", attempt.RetryArgv)
	}
	artifacts, err := home.NewArtifactStore(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	storedInput, err := artifacts.GetPrivate(attempt.InputRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.Get(attempt.InputRef); err == nil {
		t.Fatalf("new SessionEnd input must not be present in public artifact custody: %s", attempt.InputRef)
	}
	if string(storedInput) != input {
		t.Fatalf("durable SessionEnd input changed:\nwant=%s\ngot=%s", input, storedInput)
	}
}

func TestSessionEndAdapterMarksSuccessfulWorkerProcessing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell worker fixture requires a POSIX host")
	}

	compoundHome := t.TempDir()
	workerPath := filepath.Join(t.TempDir(), "successful-session-end-worker.sh")
	worker := "#!/bin/sh\nprintf '%s\\n' '{}'\n"
	if err := os.WriteFile(workerPath, []byte(worker), 0o755); err != nil {
		t.Fatal(err)
	}
	input := `{"hook_event_name":"SessionEnd","session_id":"sess_successful_worker","cwd":"/repo/example"}`

	t.Setenv("COMPOUND_HOME", compoundHome)
	t.Setenv(sessionEndAdapterWorkerOverrideEnv, "1")
	args := []string{
		"--live", "--harness", "claude-code",
		"--timeout", "5s", "--worker", workerPath,
	}
	var stdout, stderr bytes.Buffer
	if code := RunSessionEndAdapterCommand(args, strings.NewReader(input), &stdout, &stderr); code != 0 {
		t.Fatalf("SessionEnd adapter exit = %d\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	requireGenericSessionEndHookResponse(t, stdout.Bytes())

	store, err := eventstore.Open(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := requireSessionEndAdapterDiagnosticValue(t, stderr.String(), "acceptance=")
	outcomeID := requireSessionEndAdapterDiagnosticValue(t, stderr.String(), "terminal=")
	attemptRecord, err := store.Read(attemptID)
	if err != nil {
		t.Fatalf("read successful acceptance by exact id: %v", err)
	}
	outcomeRecord, err := store.Read(outcomeID)
	if err != nil {
		t.Fatalf("read successful outcome by exact id: %v", err)
	}
	var attempt sessionEndAdapterAttemptEvidence
	var outcome sessionEndAdapterOutcomeEvidence
	if err := json.Unmarshal(attemptRecord.Bytes, &attempt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(outcomeRecord.Bytes, &outcome); err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "accepted" || attempt.InputRef == "" {
		t.Fatalf("successful SessionEnd acceptance missing: %+v", attempt)
	}
	if outcome.Status != "succeeded" || outcome.AttemptID != attempt.ID || outcome.InputRef != attempt.InputRef {
		t.Fatalf("successful SessionEnd outcome = %+v, acceptance=%+v", outcome, attempt)
	}
}
