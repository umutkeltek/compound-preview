package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func TestWorkLandCheckDefaultTextReportsForeignDirtPreservation(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	repo, base, tip := initLandingCLIRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	runLandingCLIGit(t, filepath.Dir(remote), "init", "--bare", remote)
	runLandingCLIGit(t, repo, "remote", "add", "origin", remote)
	runLandingCLIGit(t, repo, "push", "-u", "origin", "main")
	runLandingCLIGit(t, repo, "merge", "--ff-only", tip)
	runLandingCLIGit(t, repo, "push", "origin", "main")

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "codex",
		Actor:            "codex",
		SuccessPredicate: "default land-check text preserves foreign WIP visibility",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: repo,
			TargetLocalRef:     "refs/heads/main",
			ExpectedBaseSHA:    base,
			PublicationMode:    "required",
			PublicationRef:     "refs/remotes/origin/main",
		},
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: w.State, Actor: "codex", ArtifactID: "commit:" + tip}, cliTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "foreign-lane-wip.txt"), []byte("neighbor lane WIP\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"land-check", w.ID, "--tip", tip, "--dry-run"})
	})
	if !strings.Contains(out, "status=missing_source_bound_proof") ||
		!strings.Contains(out, "fresh canonical certifying proof") ||
		!strings.Contains(out, "next_action:") ||
		!strings.Contains(out, "preserve") ||
		!strings.Contains(out, "foreign-lane-wip.txt") {
		t.Fatalf("default CLI text must expose the foreign-WIP preservation warning:\n%s", out)
	}
}

// writeObserveRunTrace writes a valid trace envelope bound to workID under
// <home>/events and returns its id. Mirrors the inline trace in
// TestWorkObserveRunCommandAttachesTrace so the observe-run handler accepts it.
func writeObserveRunTrace(t *testing.T, homePath, workID, traceID string) string {
	t.Helper()
	trace := envelope.TraceEnvelope{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTraceEnvelope,
			ID:            traceID,
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Source:          envelope.TraceSource{System: "test", SourceKind: "cli", EventClass: "observe_run"},
		Actor:           "codex",
		MutationClass:   "local_reversible",
		Summary:         "observed CLI run",
		ToolsModelsUsed: []string{},
		FilesTouched:    []string{},
		Outputs:         []string{"trace-output:cli"},
	}
	if err := trace.Validate(); err != nil {
		t.Fatalf("validate trace: %v", err)
	}
	traceBytes, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(homePath, "events", trace.ID+".json")
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, append(traceBytes, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return trace.ID
}

// writeObserveRunJobFile writes a completed codex plugin job file (the
// camelCase keys internal/work.codexJobFile decodes) and returns its path.
// observe-run --job-file mints the deterministic RunAttempt the S3
// attribution flags land on.
func writeObserveRunJobFile(t *testing.T, jobID string) string {
	t.Helper()
	job := map[string]interface{}{
		"id": jobID, "status": "completed", "phase": "execute", "pid": 12345,
		"startedAt": "2026-07-20T12:00:00Z", "logFile": "/tmp/" + jobID + ".log",
		"summary": "test job " + jobID, "title": "test job " + jobID,
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), jobID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// observeRunSetup is the shared scaffolding for the observe-run attribution
// CLI tests: a home, a claimed+running work, a trace, and a job file. Returns
// the home path, work id, trace id, and job-file path.
func observeRunSetup(t *testing.T, predicate string) (homePath, workID, traceID, jobPath string) {
	t.Helper()
	homePath = initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "codex", SuccessPredicate: predicate}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	traceID = writeObserveRunTrace(t, homePath, w.ID, "trace_attr_"+predicate[:6])
	jobPath = writeObserveRunJobFile(t, "job-"+predicate[:6])
	return homePath, w.ID, traceID, jobPath
}

// runObserveRunJSON invokes `work observe-run ... --json` and returns the
// parsed result. Exits the test process on a usage error (cmdWork os.Exit).
func runObserveRunJSON(t *testing.T, args []string) work.RunObservationResult {
	t.Helper()
	out := captureStdout(t, func() { cmdWork(args) })
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("observe-run output missing JSON:\n%s", out)
	}
	var result work.RunObservationResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse observe-run result: %v\n%s", err, out)
	}
	return result
}

// TestWorkObserveRunAttributionFlagsStampRecord: observe-run with the five S3
// attribution flags lands all five on the job-file-minted RunAttempt.
// Falsifier (compile-fail-as-RED at base): the flags, the ObserveRunInput
// fields, and the RunAttempt fields it asserts on did not exist before this
// slice. Teeth proven by a scratch mutation that drops the
// observeRunFromJobFile stamping (reds this; reverted by targeted edit).
func TestWorkObserveRunAttributionFlagsStampRecord(t *testing.T) {
	homePath, workID, traceID, jobPath := observeRunSetup(t, "attrflags")
	result := runObserveRunJSON(t, []string{
		"observe-run", workID,
		"--trace", traceID, "--job-file", jobPath, "--actor", "codex",
		"--lane", "lane-a:worker", "--model", "model-x", "--provider-route", "provider-y",
		"--tokens-in", "100", "--tokens-out", "250",
		"--json",
	})
	if result.RunAttemptID == "" {
		t.Fatalf("observe-run with --job-file must mint a RunAttempt, got: %+v", result)
	}
	ra, err := runattempt.Open(homePath).Get(result.RunAttemptID)
	if err != nil {
		t.Fatalf("get minted RunAttempt %s: %v", result.RunAttemptID, err)
	}
	if ra.LaneID != "lane-a:worker" || ra.ModelID != "model-x" || ra.ProviderRoute != "provider-y" {
		t.Fatalf("attribution fields not stamped: lane=%q model=%q provider=%q", ra.LaneID, ra.ModelID, ra.ProviderRoute)
	}
	if ra.TokensIn != 100 || ra.TokensOut != 250 {
		t.Fatalf("token usage not stamped: in=%d out=%d", ra.TokensIn, ra.TokensOut)
	}
}

// TestWorkObserveRunWithoutAttributionFlagsEmitsNoKeys: observe-run WITHOUT
// the S3 attribution flags (but WITH --job-file, so a record exists) mints a
// RunAttempt whose JSON carries none of the five new keys. This is the
// omitempty proof at the CLI layer -- a record that has S2 fields but no
// attribution fields must not sprout empty attribution keys.
func TestWorkObserveRunWithoutAttributionFlagsEmitsNoKeys(t *testing.T) {
	homePath, workID, traceID, jobPath := observeRunSetup(t, "nokeys")
	result := runObserveRunJSON(t, []string{
		"observe-run", workID,
		"--trace", traceID, "--job-file", jobPath, "--actor", "codex",
		"--json",
	})
	if result.RunAttemptID == "" {
		t.Fatalf("observe-run with --job-file must mint a RunAttempt, got: %+v", result)
	}
	raw, err := os.ReadFile(filepath.Join(homePath, "run_attempts", result.RunAttemptID+".json"))
	if err != nil {
		t.Fatalf("read minted RunAttempt json: %v", err)
	}
	for _, key := range []string{`"lane_id"`, `"model_id"`, `"provider_route"`, `"tokens_in"`, `"tokens_out"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("RunAttempt JSON emitted attribution key %s when no attribution flag was passed:\n%s", key, raw)
		}
	}
	// Sanity: the record DOES carry an S2 field, so the absence above is the
	// attribution omitempty, not an empty record.
	if !strings.Contains(string(raw), `"transcript_ref"`) {
		t.Fatalf("expected an S2 transcript_ref on the record, got:\n%s", raw)
	}
}
