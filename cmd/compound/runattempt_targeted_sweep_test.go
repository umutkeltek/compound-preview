package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/runattempt"
	"compound/internal/work"
)

const runAttemptTargetedSweepChildEnv = "COMPOUND_TEST_RUN_ATTEMPT_TARGETED_SWEEP"
const runAttemptTargetedSweepArgsChildEnv = "COMPOUND_TEST_RUN_ATTEMPT_TARGETED_SWEEP_ARGS"

func TestRunAttemptSweepAttemptRecoversOnlyExactIDWithoutEnumeratingUnrelatedRecords(t *testing.T) {
	if encoded := os.Getenv(runAttemptTargetedSweepArgsChildEnv); encoded != "" {
		args, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var decoded []string
		if err := json.Unmarshal(args, &decoded); err != nil {
			t.Fatal(err)
		}
		cmdRunAttempt(decoded)
		return
	}
	if targetID := os.Getenv(runAttemptTargetedSweepChildEnv); targetID != "" {
		cmdRunAttempt([]string{"sweep", "--attempt", targetID, "--threshold", "1m", "--json"})
		return
	}

	homePath := initializedTestHome(t)
	store := runattempt.Open(homePath)
	target, err := store.Mint("work_targeted_sweep", "actor:test", "claude-code", cliTestNow.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("mint target: %v", err)
	}
	unrelated, err := store.Mint("work_unrelated_corrupt", "actor:test", "claude-code", cliTestNow.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("mint unrelated: %v", err)
	}
	unrelatedPath := filepath.Join(homePath, "run_attempts", unrelated.ID+".json")
	corrupt := []byte("{not-json\\n")
	if err := os.WriteFile(unrelatedPath, corrupt, 0o600); err != nil {
		t.Fatalf("corrupt unrelated record: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunAttemptSweepAttemptRecoversOnlyExactIDWithoutEnumeratingUnrelatedRecords$")
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath, runAttemptTargetedSweepChildEnv+"="+target.ID)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("targeted sweep: %v\\nstdout=%s\\nstderr=%s", err, stdout.String(), stderr.String())
	}
	var report runattempt.SweepReport
	if err := json.NewDecoder(bytes.NewReader(stdout.Bytes())).Decode(&report); err != nil {
		t.Fatalf("decode targeted report: %v\\nstdout=%q", err, stdout.String())
	}
	if report.Scanned != 1 || report.Swept != 1 || len(report.SweptIDs) != 1 || report.SweptIDs[0] != target.ID {
		t.Fatalf("targeted report = %+v, want exactly %s swept", report, target.ID)
	}
	if bytes.Contains(stdout.Bytes(), []byte(unrelated.ID)) || bytes.Contains(stderr.Bytes(), []byte(unrelated.ID)) {
		t.Fatalf("targeted sweep surfaced unrelated id %q\\nstdout=%s\\nstderr=%s", unrelated.ID, stdout.String(), stderr.String())
	}
	got, err := runattempt.Open(homePath).Get(target.ID)
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if got.Status != runattempt.StatusAbandoned {
		t.Fatalf("target status = %q, want %q", got.Status, runattempt.StatusAbandoned)
	}
	after, err := os.ReadFile(unrelatedPath)
	if err != nil {
		t.Fatalf("read unrelated after targeted sweep: %v", err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("targeted sweep rewrote unrelated corrupt record: got %q want %q", after, corrupt)
	}
}

func TestRunAttemptSweepAttemptRejectsInvalidSelectorsBeforeHomeResolution(t *testing.T) {
	validID := "ratt_20260910T120000Z_deadbeef"
	for _, args := range [][]string{
		{"sweep", "--attempt"},
		{"sweep", "--attempt", ""},
		{"sweep", "--attempt", "not-an-attempt"},
		{"sweep", "--attempt", "../" + validID},
		{"sweep", "--attempt", validID, "--attempt", validID},
		{"sweep", "--attempt", validID, "--no-probe"},
	} {
		t.Run(args[1], func(t *testing.T) {
			homePath := filepath.Join(t.TempDir(), "poison-home")
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunAttemptSweepAttemptRecoversOnlyExactIDWithoutEnumeratingUnrelatedRecords$")
			cmd.Env = append(
				os.Environ(),
				"COMPOUND_HOME="+homePath,
				runAttemptTargetedSweepArgsChildEnv+"="+base64.RawStdEncoding.EncodeToString(raw),
			)
			output, runErr := cmd.CombinedOutput()
			exitErr, ok := runErr.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 2 {
				t.Fatalf("args=%q exit=%v output=%s, want usage exit 2", args, runErr, output)
			}
			if _, err := os.Stat(homePath); !os.IsNotExist(err) {
				t.Fatalf("args=%q resolved or materialized poison home %q: %v", args, homePath, err)
			}
		})
	}
}

func TestRunAttemptSweepAttemptPreservesWorkEnvelopeBytesAndState(t *testing.T) {
	homePath := initializedTestHome(t)
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workStore.Create(work.CreateInput{
		Owner:            "targeted-sweep-audit",
		SuccessPredicate: "exact recovery does not mutate the work envelope",
	}, cliTestNow.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := workStore.Claim(w.ID, "targeted-sweep-audit", cliTestNow.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed.RunAttemptIDs) != 1 {
		t.Fatalf("claimed run_attempt_ids = %v, want one", claimed.RunAttemptIDs)
	}
	workPath := filepath.Join(homePath, "work", "work", w.ID+".json")
	beforeBytes, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}

	output, runErr := runAttemptTargetedSweepChild(t, homePath, []string{
		"sweep", "--attempt", claimed.RunAttemptIDs[0], "--threshold", "1m", "--json",
	})
	if runErr != nil {
		t.Fatalf("targeted sweep: %v\noutput=%s", runErr, output)
	}
	afterBytes, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("targeted sweep rewrote WorkEnvelope bytes\nbefore=%s\nafter=%s", beforeBytes, afterBytes)
	}
	afterWork, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := afterWork.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, claimed) {
		t.Fatalf("targeted sweep changed WorkEnvelope state\ngot=%+v\nwant=%+v", got, claimed)
	}
}

func TestRunAttemptSweepAttemptReportsMissingAndUnreadableExactTargetWithoutEnumeration(t *testing.T) {
	for _, tc := range []struct {
		name            string
		corruptSelected bool
	}{
		{name: "missing exact target"},
		{name: "unreadable exact target", corruptSelected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := initializedTestHome(t)
			store := runattempt.Open(homePath)
			now := cliTestNow.Add(-2 * time.Hour)
			target, err := store.Mint("work_targeted_error", "actor:test", "claude-code", now)
			if err != nil {
				t.Fatal(err)
			}
			unrelated, err := store.Mint("work_unrelated_error", "actor:test", "claude-code", now)
			if err != nil {
				t.Fatal(err)
			}
			unrelatedPath := filepath.Join(homePath, "run_attempts", unrelated.ID+".json")
			corrupt := []byte("{not-json\n")
			if err := os.WriteFile(unrelatedPath, corrupt, 0o600); err != nil {
				t.Fatal(err)
			}
			attemptID := target.ID
			if tc.corruptSelected {
				if err := os.WriteFile(filepath.Join(homePath, "run_attempts", target.ID+".json"), corrupt, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				attemptID = "ratt_20260910T120000Z_deadbeef"
			}

			output, runErr := runAttemptTargetedSweepChild(t, homePath, []string{
				"sweep", "--attempt", attemptID, "--threshold", "1m", "--json",
			})
			if runErr == nil {
				t.Fatalf("exact target %q unexpectedly recovered", attemptID)
			}
			if bytes.Contains(output, []byte(homePath)) {
				t.Fatalf("private home path leaked in exact-target error: %s", output)
			}
			if bytes.Contains(output, []byte(unrelated.ID)) {
				t.Fatalf("exact-target error enumerated unrelated ID %q: %s", unrelated.ID, output)
			}
			after, err := os.ReadFile(unrelatedPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, corrupt) {
				t.Fatalf("exact-target failure rewrote unrelated record: got %q want %q", after, corrupt)
			}
		})
	}
}

func runAttemptTargetedSweepChild(t *testing.T, homePath string, args []string) ([]byte, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunAttemptSweepAttemptRecoversOnlyExactIDWithoutEnumeratingUnrelatedRecords$")
	cmd.Env = append(
		os.Environ(),
		"COMPOUND_HOME="+homePath,
		runAttemptTargetedSweepArgsChildEnv+"="+base64.RawStdEncoding.EncodeToString(raw),
	)
	return cmd.CombinedOutput()
}
