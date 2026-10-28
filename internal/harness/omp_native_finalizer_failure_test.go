package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	homepkg "compound/internal/home"
	"compound/internal/runattempt"
)

func TestOMPFinalizerSchemaFailureRetainsExactMalformedPrivateCapture(t *testing.T) {
	f := newOMPVerifierFixture(t, "")
	const canary = "MALFORMED_PRIVATE_CANARY"
	proxy := filepath.Join(f.root, "node-proxy")
	script := fmt.Sprintf("#!/usr/bin/python3\nimport os,subprocess,sys\ntry:\n os.fstat(3)\n inherited=True\nexcept OSError:\n inherited=False\ncode=subprocess.call([%q]+sys.argv[1:],pass_fds=(3,) if inherited else ())\nif inherited and code==0:\n os.ftruncate(3,0)\n os.lseek(3,0,os.SEEK_SET)\n os.write(3,b'%s')\n os.fsync(3)\nsys.exit(code)\n", f.interpreter.Path, canary)
	if err := os.WriteFile(proxy, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(proxy)
	if err != nil {
		t.Fatal(err)
	}
	f.adapter.interpreterPath = canonical
	out, err := f.adapter.Run(context.Background(), f.input)
	if err == nil || !strings.Contains(err.Error(), "OMP_PRIVATE_CAPTURE_SCHEMA") ||
		out.RunAttempt.Status != runattempt.StatusFailed || out.ExitCode != 0 {
		t.Fatalf("post-exit schema refusal status=%s exit=%d err=%v", out.RunAttempt.Status, out.ExitCode, err)
	}
	captured := assertOMPFailedPrivateCapture(t, f.home, f.input, out)
	if !bytes.Equal(captured, []byte(canary)) {
		t.Fatal("exact malformed private bytes were lost")
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(string(mustReadTestFile(t, out.TranscriptPath)), canary) {
		t.Fatal("malformed private capture leaked publicly")
	}
}

func assertOMPFailedPrivateCapture(t *testing.T, home string, in RunInput, out RunOutput) []byte {
	t.Helper()
	var digest string
	count := 0
	for _, ref := range out.RunAttempt.ProofRefs {
		if strings.HasPrefix(ref, "artifact:") {
			t.Fatal("failed finalization attached certifying artifact")
		}
		if strings.HasPrefix(ref, "failed_private_capture_sha256:") {
			count++
			digest = strings.TrimPrefix(ref, "failed_private_capture_sha256:")
		}
	}
	if count != 1 || len(digest) != 64 {
		t.Fatalf("failure markers=%d, want exactly one", count)
	}
	artifacts, err := homepkg.NewArtifactStore(home)
	if err != nil {
		t.Fatal(err)
	}
	body, err := artifacts.GetPrivate(homepkg.ArtifactRefPrefix + digest)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Schema   string `json:"schema_version"`
		WorkID   string `json:"work_id"`
		RunID    string `json:"run_attempt_id"`
		ActionID string `json:"action_intent_id"`
		ExitCode int    `json:"exit_code"`
		TimedOut bool   `json:"timed_out"`
		Length   int    `json:"captured_length"`
		Digest   string `json:"captured_sha256"`
		Captured string `json:"captured_base64"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(report.Captured)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if report.Schema != "compound.omp_failed_capture.v0.1" ||
		report.WorkID != in.WorkID || report.RunID != out.RunAttempt.ID ||
		report.ActionID != in.ActionIntentID || report.ExitCode != out.ExitCode ||
		report.TimedOut != out.TimedOut || report.Length != len(raw) ||
		report.Digest != hex.EncodeToString(sum[:]) {
		t.Fatal("failed private capture identity or raw digest mismatch")
	}
	return raw
}

func TestRunnerFinalizerErrorRetainsBeforeTerminalization(t *testing.T) {
	for _, retentionFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("retention_fails_%t", retentionFails), func(t *testing.T) {
			root := t.TempDir()
			store := runattempt.Open(root)
			finalized, retained := false, 0
			out, err := runWithSubprocessOptions(context.Background(), store,
				"os-harness-entrypoints", durabilityRunInput(root), durabilityBuild,
				defaultHeartbeatInterval, subprocessRunOptions{
					FinalizeEvidence: func(subprocessEvidenceContext) ([]string, error) {
						finalized = true
						return []string{"artifact:must-not-attach"}, errors.New("proof rejected")
					},
					RetainFailedEvidence: func(e subprocessEvidenceContext) (string, error) {
						retained++
						attempt, err := store.Get(e.RunAttemptID)
						if err != nil || runattempt.IsTerminal(attempt.Status) || !finalized || e.ExitCode != 0 {
							t.Fatal("failure retention ran out of order")
						}
						if retentionFails {
							return "", errors.New("snapshot unavailable")
						}
						return strings.Repeat("a", 64), nil
					},
				})
			if err == nil || out.RunAttempt.Status != runattempt.StatusFailed || out.ExitCode != 0 || retained != 1 {
				t.Fatalf("finalizer failure status=%s exit=%d retained=%d err=%v", out.RunAttempt.Status, out.ExitCode, retained, err)
			}
			persisted, loadErr := store.Get(out.RunAttempt.ID)
			if loadErr != nil || persisted.Status != runattempt.StatusFailed {
				t.Fatal("failed attempt not durable")
			}
			for _, ref := range persisted.ProofRefs {
				if strings.HasPrefix(ref, "artifact:") {
					t.Fatal("failed finalizer proof attached")
				}
			}
			if !retentionFails && !strings.Contains(strings.Join(persisted.ProofRefs, "\n"), "failed_private_capture_sha256:") {
				t.Fatal("retained failure ref was not durable")
			}
		})
	}
}
