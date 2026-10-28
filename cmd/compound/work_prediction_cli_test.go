package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/mem"
)

const workPredictionCLIBoundarySubEnv = "COMPOUND_WORK_PREDICTION_CLI_BOUNDARY_SUB"

// firstJSONObject locates the first '{' in CLI combined stdout (create/close print a leading
// summary line before the full envelope JSON) and decodes the remainder as one JSON object.
func firstJSONObject(t *testing.T, out string) map[string]json.RawMessage {
	t.Helper()
	idx := strings.Index(out, "{")
	if idx < 0 {
		t.Fatalf("no JSON object found in output: %q", out)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out[idx:]), &m); err != nil {
		t.Fatalf("decode JSON object: %v\n%s", err, out)
	}
	return m
}

// TestWorkCreateCLIPredictionRequiresOutcome covers F3 (CLI layer): staking any prediction flag
// without --predicted-outcome is a usage error (exit 2), grouped-optional like parseGitDelivery.
func TestWorkCreateCLIPredictionRequiresOutcome(t *testing.T) {
	t.Parallel()
	if os.Getenv(workPredictionCLIBoundarySubEnv) == "missing-outcome" {
		cmdWork([]string{"create", "--owner", "compound-core", "--predicate", "p", "--predicted-move-type", "rule_delta"})
		return
	}
	homePath := initializedTestHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkCreateCLIPredictionRequiresOutcome$")
	cmd.Env = append(os.Environ(), workPredictionCLIBoundarySubEnv+"=missing-outcome", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("staking a prediction without --predicted-outcome should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("want usage exit 2, got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "--predicted-outcome") {
		t.Fatalf("usage error must name --predicted-outcome:\n%s", output)
	}
}

// TestWorkCreateCLIPredictionRejectsReceiptOutcome covers F3: a staked prediction outcome of
// "no_op" (a Receipt.Outcome value, never a close outcome) is rejected.
func TestWorkCreateCLIPredictionRejectsReceiptOutcome(t *testing.T) {
	t.Parallel()
	if os.Getenv(workPredictionCLIBoundarySubEnv) == "receipt-outcome" {
		cmdWork([]string{"create", "--owner", "compound-core", "--predicate", "p", "--predicted-outcome", "no_op"})
		return
	}
	homePath := initializedTestHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkCreateCLIPredictionRejectsReceiptOutcome$")
	cmd.Env = append(os.Environ(), workPredictionCLIBoundarySubEnv+"=receipt-outcome", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("predicted-outcome=no_op should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("want die() exit 1, got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "prediction.outcome") {
		t.Fatalf("error must name prediction.outcome:\n%s", output)
	}
}

// TestWorkCreateCLIPredictionRejectsBadMoveType covers F3: an unrecognized predicted move_type is
// rejected.
func TestWorkCreateCLIPredictionRejectsBadMoveType(t *testing.T) {
	t.Parallel()
	if os.Getenv(workPredictionCLIBoundarySubEnv) == "bad-move-type" {
		cmdWork([]string{"create", "--owner", "compound-core", "--predicate", "p", "--predicted-outcome", "completed", "--predicted-move-type", "banana"})
		return
	}
	homePath := initializedTestHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkCreateCLIPredictionRejectsBadMoveType$")
	cmd.Env = append(os.Environ(), workPredictionCLIBoundarySubEnv+"=bad-move-type", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("predicted-move-type=banana should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("want die() exit 1, got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "prediction.move_type") {
		t.Fatalf("error must name prediction.move_type:\n%s", output)
	}
}

// TestWorkCreateCLIPredictionRoundTrips covers F4: `work create` with the three prediction flags
// prints an envelope JSON carrying the prediction, and `work show` round-trips it.
func TestWorkCreateCLIPredictionRoundTrips(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	out := captureStdout(t, func() {
		cmdWork([]string{
			"create", "--owner", "compound-core", "--predicate", "prediction round-trips",
			"--predicted-outcome", "completed", "--predicted-move-type", "rule_delta", "--prediction-note", "expect a clean rule-delta land",
		})
	})
	created := firstJSONObject(t, out)
	var predictionRaw json.RawMessage
	if err := json.Unmarshal(created["prediction"], &predictionRaw); err != nil || predictionRaw == nil {
		t.Fatalf("create output missing prediction field:\n%s", out)
	}
	var prediction struct {
		Outcome  string `json:"outcome"`
		MoveType string `json:"move_type"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal(created["prediction"], &prediction); err != nil {
		t.Fatalf("decode prediction: %v\n%s", err, out)
	}
	if prediction.Outcome != "completed" || prediction.MoveType != "rule_delta" || prediction.Note != "expect a clean rule-delta land" {
		t.Fatalf("prediction = %+v, want {completed rule_delta expect a clean rule-delta land}", prediction)
	}
	var workID string
	if err := json.Unmarshal(created["id"], &workID); err != nil || workID == "" {
		t.Fatalf("create output missing id:\n%s", out)
	}

	showOut := captureStdout(t, func() {
		cmdWork([]string{"show", workID, "--json"})
	})
	shown := firstJSONObject(t, showOut)
	var shownPrediction struct {
		Outcome  string `json:"outcome"`
		MoveType string `json:"move_type"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal(shown["prediction"], &shownPrediction); err != nil {
		t.Fatalf("show output missing prediction field: %v\n%s", err, showOut)
	}
	if shownPrediction != prediction {
		t.Fatalf("work show prediction = %+v, want %+v (round-trip mismatch)", shownPrediction, prediction)
	}
}

// TestWorkCloseCLIMintsSurpriseCandidateOnDivergence covers F5: a full CLI lifecycle
// (create->claim->advance->advance->close) with a diverged prediction mints exactly ONE
// observed-class memory candidate naming both tuples, writes a blob under
// observed/work-surprise/, never touches the ledger, and a second work with an identical
// diverged close the SAME day mints a DIFFERENT candidate (never dedupes across works).
func TestWorkCloseCLIMintsSurpriseCandidateOnDivergence(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	memStore, err := mem.Open(homePath)
	if err != nil {
		t.Fatalf("open mem store: %v", err)
	}
	before, err := memStore.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest before: %v", err)
	}

	runDivergedLifecycle := func(predicate string) string {
		t.Helper()
		out := captureStdout(t, func() {
			cmdWork([]string{
				"create", "--owner", "compound-core", "--predicate", predicate,
				"--predicted-outcome", "completed", "--predicted-move-type", "rule_delta",
			})
		})
		created := firstJSONObject(t, out)
		var workID string
		if err := json.Unmarshal(created["id"], &workID); err != nil || workID == "" {
			t.Fatalf("create output missing id:\n%s", out)
		}
		captureStdout(t, func() { cmdWork([]string{"claim", workID, "--by", "claude_code"}) })
		captureStdout(t, func() { cmdWork([]string{"advance", workID, "--to", "running", "--by", "claude_code"}) })
		captureStdout(t, func() { cmdWork([]string{"advance", workID, "--to", "verifying", "--by", "claude_code"}) })
		captureStdout(t, func() {
			cmdWork([]string{
				"close", workID, "--outcome", "completed", "--move-type", "control_plane_repair",
				"--by", "claude_code", "--proof", "check:proof",
			})
		})
		return workID
	}

	work1 := runDivergedLifecycle("first diverged close")

	candidateFiles := func() []string {
		entries, err := os.ReadDir(filepath.Join(homePath, "inbox", "memory-candidates"))
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("read candidates dir: %v", err)
		}
		var names []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				names = append(names, e.Name())
			}
		}
		return names
	}
	after1 := candidateFiles()
	if len(after1) != 1 {
		t.Fatalf("want exactly 1 candidate after one diverged close, got %d: %v", len(after1), after1)
	}

	blobDir := filepath.Join(homePath, "observed", "work-surprise")
	blobEntries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatalf("read blob dir %s: %v", blobDir, err)
	}
	if len(blobEntries) != 1 {
		t.Fatalf("want exactly 1 blob under observed/work-surprise/, got %d", len(blobEntries))
	}

	var cand mem.Candidate
	if err := candidates.ReadCandidate(homePath, candidates.Memory, strings.TrimSuffix(after1[0], ".json"), &cand); err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	if cand.Derivation != mem.DerivationObserved {
		t.Errorf("derivation = %q, want %q", cand.Derivation, mem.DerivationObserved)
	}
	if cand.PromotionBasis != "" {
		t.Errorf("promotion_basis = %q, want empty", cand.PromotionBasis)
	}
	if !strings.Contains(cand.Object, work1) {
		t.Errorf("statement must name the work id %s: %q", work1, cand.Object)
	}
	if !strings.Contains(cand.Object, "completed/rule_delta") || !strings.Contains(cand.Object, "completed/control_plane_repair") {
		t.Errorf("statement must name BOTH tuples: %q", cand.Object)
	}

	// A SECOND work, same day, identical diverged predicted/actual tuple: mints a DIFFERENT
	// candidate -- never dedupes across works.
	runDivergedLifecycle("second diverged close, same day")
	after2 := candidateFiles()
	if len(after2) != 2 {
		t.Fatalf("want exactly 2 candidates after a second work's diverged close, got %d: %v", len(after2), after2)
	}

	after, err := memStore.LedgerDigest()
	if err != nil {
		t.Fatalf("ledger digest after: %v", err)
	}
	if before != after {
		t.Fatalf("ledger digest changed across diverged closes: before=%s after=%s", before, after)
	}
}

// TestWorkCloseCLIMatchingPredictionMintsNothing covers F6: a close that CONFIRMS its staked
// prediction mints ZERO candidates and writes no blob.
func TestWorkCloseCLIMatchingPredictionMintsNothing(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	out := captureStdout(t, func() {
		cmdWork([]string{
			"create", "--owner", "compound-core", "--predicate", "matching prediction",
			"--predicted-outcome", "completed", "--predicted-move-type", "outward_delta",
		})
	})
	created := firstJSONObject(t, out)
	var workID string
	if err := json.Unmarshal(created["id"], &workID); err != nil || workID == "" {
		t.Fatalf("create output missing id:\n%s", out)
	}
	captureStdout(t, func() { cmdWork([]string{"claim", workID, "--by", "claude_code"}) })
	captureStdout(t, func() { cmdWork([]string{"advance", workID, "--to", "running", "--by", "claude_code"}) })
	captureStdout(t, func() { cmdWork([]string{"advance", workID, "--to", "verifying", "--by", "claude_code"}) })
	captureStdout(t, func() {
		cmdWork([]string{
			"close", workID, "--outcome", "completed", "--move-type", "outward_delta",
			"--by", "claude_code", "--changed",
		})
	})

	entries, err := os.ReadDir(filepath.Join(homePath, "inbox", "memory-candidates"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read candidates dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	if n != 0 {
		t.Fatalf("want 0 candidates on a matching (non-diverged) close, got %d", n)
	}
	if _, err := os.Stat(filepath.Join(homePath, "observed", "work-surprise")); !os.IsNotExist(err) {
		t.Fatalf("want no observed/work-surprise/ dir on a matching close, stat err=%v", err)
	}
}
