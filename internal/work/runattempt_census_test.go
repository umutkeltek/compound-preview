package work

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

func censusRunningWork(t *testing.T) (*Store, envelope.WorkEnvelope, *runattempt.Store, runattempt.RunAttempt) {
	t.Helper()
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "codex", SuccessPredicate: "complete execution accounting"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "codex", fixedNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	w, err = s.Advance(w.ID, AdvanceInput{To: "running", Actor: "codex"}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	attempts := runattempt.Open(s.home)
	ra, err := attempts.MintWithActionIntent(w.ID, "codex", "os-harness-entrypoints", latestActionIntentID(w), fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetOrigin(ra.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	return s, w, attempts, ra
}

func TestCloseCensusIncludesReverseOnlyTerminalExecutionWithoutRewritingIt(t *testing.T) {
	s, w, attempts, ra := censusRunningWork(t)
	child := exec.Command("true")
	if _, err := attempts.CommitLaunch(ra.ID, fixedNow.Add(4*time.Minute), child.Start); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Complete(ra.ID, runattempt.StatusSucceeded, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	w, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex"}, fixedNow.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(w.RunAttemptIDs, ra.ID) {
		t.Fatal("setup forward-attached the historical execution")
	}
	path := filepath.Join(s.home, "run_attempts", ra.ID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex", Summary: "account for historical execution"}, fixedNow.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(closed.Work.RunAttemptIDs, ra.ID) {
		t.Fatal("terminal generation omitted its reverse-linked execution")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("census rewrote terminal execution history: %v", err)
	}
}

func TestCloseCensusAccountsForExactStartRefusalWithoutTaskFailure(t *testing.T) {
	s, w, attempts, ra := censusRunningWork(t)
	startErr := errors.New("synthetic refusal before process start")
	if _, err := attempts.CommitLaunch(ra.ID, fixedNow.Add(4*time.Minute), func() error { return startErr }); !errors.Is(err, startErr) {
		t.Fatalf("start refusal evidence: %v", err)
	}
	if _, err := attempts.Complete(ra.ID, runattempt.StatusFailed, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex"}, fixedNow.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex", Summary: "no task executed by refused start"}, fixedNow.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	retained, err := attempts.Get(ra.ID)
	if err != nil || retained.Status != runattempt.StatusFailed || !slices.Contains(closed.Work.RunAttemptIDs, ra.ID) {
		t.Fatalf("start refusal was dropped or rewritten: attempt=%+v err=%v", retained, err)
	}
	report, err := s.ProofChainDoctor(fixedNow.Add(8*time.Minute), ProofChainDoctorOptions{})
	if err != nil || report.Status != "ok" || report.Counts.Complete != 1 {
		t.Fatalf("safe refusal failed terminal proof: %+v, err=%v", report, err)
	}
}

func TestCloseCensusRefusesUncertainLaunchDespiteTerminalLabel(t *testing.T) {
	s, w, attempts, ra := censusRunningWork(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A real process death releases kernel locks but cannot publish a launch
	// disposition. Recovering a panic in-process does not simulate that boundary.
	crash := exec.Command(executable, "-test.run=^TestCloseCensusLaunchCrashHelper$", "-test.timeout=15s")
	crash.Env = append(os.Environ(), "COMPOUND_CENSUS_CRASH_HOME="+s.home, "COMPOUND_CENSUS_CRASH_ATTEMPT="+ra.ID)
	output, err := crash.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("launch crash helper: err=%v output=%s", err, output)
	}
	if _, err := attempts.Complete(ra.ID, runattempt.StatusFailed, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex"}, fixedNow.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	before := snapshotCloseAuthorityFiles(t, s.home)
	_, err = s.Close(w.ID, CloseInput{Outcome: "failed", MoveType: "audit_only", Actor: "codex", Summary: "terminal label is not launch disposition"}, fixedNow.Add(7*time.Minute))
	if err == nil || !strings.Contains(err.Error(), ra.ID) || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("uncertain launch passed terminal certification: %v", err)
	}
	after := snapshotCloseAuthorityFiles(t, s.home)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("uncertain census refusal changed close authority")
	}
}

func TestCloseCensusLaunchCrashHelper(t *testing.T) {
	home := os.Getenv("COMPOUND_CENSUS_CRASH_HOME")
	if home == "" {
		return
	}
	_, err := runattempt.Open(home).CommitLaunch(os.Getenv("COMPOUND_CENSUS_CRASH_ATTEMPT"), fixedNow.Add(4*time.Minute), func() error {
		os.Exit(77)
		return nil
	})
	t.Fatalf("crash helper did not reach launch: %v", err)
}

func TestProofChainDoctorRefusesPreparedOnlyLaunchDespiteTerminalLabel(t *testing.T) {
	s, w, attempts, ra := censusRunningWork(t)
	if _, err := attempts.Complete(ra.ID, runattempt.StatusFailed, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(w.ID, CloseInput{Outcome: "failed", MoveType: "audit_only", Actor: "codex"}, fixedNow.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The current close door cannot produce this historical/late-evidence
	// contradiction, so inject only the exact launch sidecar deliberately.
	dir := filepath.Join(s.home, "run_attempts", "launch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"schema_version": "compound.runattempt.launch_evidence.v0.1",
		"attempt_id":     ra.ID, "kind": "prepared", "at": fixedNow.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ra.ID+".prepared.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := s.ProofChainDoctor(fixedNow.Add(8*time.Minute), ProofChainDoctorOptions{})
	if err != nil || report.Status != "fail" || report.Counts.Complete != 0 {
		t.Fatalf("uncertain launch certified terminal work: %+v, err=%v", report, err)
	}
	for _, finding := range report.Findings {
		if finding.Code == "run_attempt_launch_uncertain" && strings.Contains(finding.Message, ra.ID) {
			return
		}
	}
	t.Fatalf("doctor omitted actionable uncertain-launch finding: %+v", report.Findings)
}

func TestCloseCensusRefusesUnfinalizedStartError(t *testing.T) {
	s, w, attempts, ra := censusRunningWork(t)
	if _, err := attempts.CommitLaunch(ra.ID, fixedNow.Add(4*time.Minute), func() error { return errors.New("refused start") }); err == nil {
		t.Fatal("expected start refusal")
	}
	attached, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex", RunAttemptIDs: []string{ra.ID}}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex"}, fixedNow.Add(6*time.Minute)); err == nil {
		t.Fatal("unfinished start refusal became task success")
	}
	after, err := s.Get(w.ID)
	if err != nil || !reflect.DeepEqual(after, attached) {
		t.Fatalf("refusal rotated work authority: err=%v", err)
	}
	current, err := runattempt.Open(s.home).Get(ra.ID)
	if err != nil || runattempt.IsTerminal(current.Status) {
		t.Fatalf("refusal terminalized execution: %+v, err=%v", current, err)
	}
}

func TestCloseCensusOrdersReverseOnlyPredecessorBeforeAttachedRetry(t *testing.T) {
	s, w, attempts, parent := censusRunningWork(t)
	failedChild := exec.Command("false")
	if _, err := attempts.CommitLaunch(parent.ID, fixedNow.Add(4*time.Minute), failedChild.Start); err != nil {
		t.Fatal(err)
	}
	if err := failedChild.Wait(); err == nil {
		t.Fatal("expected failed predecessor")
	}
	if _, err := attempts.Complete(parent.ID, runattempt.StatusFailed, fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	child, err := attempts.MintWithActionIntent(w.ID, "codex", "os-harness-entrypoints", parent.ActionIntentID, fixedNow.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetOrigin(child.ID, runattempt.OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.SetPredecessor(child.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	successfulChild := exec.Command("true")
	if _, err := attempts.CommitLaunch(child.ID, fixedNow.Add(7*time.Minute), successfulChild.Start); err != nil {
		t.Fatal(err)
	}
	if err := successfulChild.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Complete(child.ID, runattempt.StatusSucceeded, fixedNow.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "codex", RunAttemptIDs: []string{child.ID}}, fixedNow.Add(9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex"}, fixedNow.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Index(closed.Work.RunAttemptIDs, parent.ID) >= slices.Index(closed.Work.RunAttemptIDs, child.ID) {
		t.Fatalf("census inverted retry lineage: %v", closed.Work.RunAttemptIDs)
	}
}

func TestProofChainDoctorCannotHideUnreadableCensusBehindNonterminalWork(t *testing.T) {
	s, w, attempts, ra := censusRunningWork(t)
	if _, err := attempts.Complete(ra.ID, runattempt.StatusSucceeded, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex"}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	late, err := attempts.MintWithActionIntent(w.ID, "codex", "os-harness-entrypoints", latestActionIntentID(closed.Work), fixedNow.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Create(CreateInput{Owner: "codex", SuccessPredicate: "unrelated nonterminal work"}, fixedNow.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	other.RunAttemptIDs = []string{late.ID}
	if err := s.writeJSON(s.workPath(other.ID), &other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "run_attempts", late.ID+".json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := s.ProofChainDoctor(fixedNow.Add(8*time.Minute), ProofChainDoctorOptions{})
	if err != nil || report.Status != "fail" || report.Counts.Complete != 0 {
		t.Fatalf("unknown membership certified terminal work: %+v, err=%v", report, err)
	}
}

func TestProofChainDoctorDetectsReverseOnlyAttemptOnTerminalWork(t *testing.T) {
	s, w, attempts, ra := censusRunningWork(t)
	if _, err := attempts.Complete(ra.ID, runattempt.StatusSucceeded, fixedNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "codex"}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.ProofChainDoctor(fixedNow.Add(6*time.Minute), ProofChainDoctorOptions{})
	if err != nil || report.Status != "ok" || report.Counts.Complete != 1 {
		t.Fatalf("accounted terminal work = %+v, err=%v", report, err)
	}
	late, err := attempts.MintWithActionIntent(w.ID, "codex", "os-harness-entrypoints", latestActionIntentID(closed.Work), fixedNow.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Complete(late.ID, runattempt.StatusSucceeded, fixedNow.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	report, err = s.ProofChainDoctor(fixedNow.Add(9*time.Minute), ProofChainDoctorOptions{})
	if err != nil || report.Status != "fail" || report.Counts.Complete != 0 || report.Counts.InvalidRunAttempt != 1 {
		t.Fatalf("omitted terminal attempt certified: %+v, err=%v", report, err)
	}
	if !slices.Contains(report.Items[0].Problems, "unlisted_run_attempt:"+late.ID) {
		t.Fatalf("doctor did not identify omitted attempt: %+v", report)
	}
}
