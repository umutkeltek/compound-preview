package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	homepkg "compound/internal/home"
	"compound/internal/runattempt"
)

func TestOMPPrivateEnvelopeRejectsMalformedTrailingAndUnknown(t *testing.T) {
	valid := `{"schema_version":"compound.omp_native_verifier_private.v0.1","config":{},"report":{},"sanitized":{}}`
	for name, body := range map[string]string{
		"malformed":         `{`,
		"trailing":          valid + ` {}`,
		"unknown raw field": `{"schema_version":"compound.omp_native_verifier_private.v0.1","config":{},"report":{},"sanitized":{},"raw_canary":"secret"}`,
		"missing config":    `{"schema_version":"compound.omp_native_verifier_private.v0.1","report":{},"sanitized":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeOMPPrivateEnvelope([]byte(body)); err == nil || err.Error() != "OMP_PRIVATE_CAPTURE_SCHEMA" {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := decodeOMPPrivateEnvelope([]byte(valid)); err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
}

func TestOMPPublicReportRejectsUnknownRawAndTrailingFields(t *testing.T) {
	for name, body := range map[string]string{
		"raw owner": `{"owner":{"model":"private-canary"}}`,
		"platform":  `{"platform":{"release":"private-canary"}}`,
		"unknown":   `{"raw_canary":"private-canary"}`,
		"trailing":  `{} {"raw_canary":"private-canary"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeOMPPublicReport([]byte(body)); err == nil || err.Error() != "OMP_PUBLIC_REPORT_SCHEMA" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOMPRecipeMalformedFDConfigEmitsOnlyStableCode(t *testing.T) {
	interpreter, err := resolveOMPVerifierInterpreter("")
	if err != nil {
		t.Skip(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "bad-config")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	canary := "private-config-canary"
	if _, err := f.WriteString(`{"broken":"` + canary); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(interpreter.Path, "-e", ompNativeVerifierRecipe)
	cmd.ExtraFiles = []*os.File{f}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("malformed config succeeded")
	}
	if stdout.Len() != 0 || stderr.String() != "OMP_PRIVATE_CONFIG_INVALID\n" || strings.Contains(stderr.String(), canary) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunnerStableStartErrorHidesUnderlyingPathOptIn(t *testing.T) {
	home := t.TempDir()
	canary := "/private/start/canary"
	out, err := runWithSubprocessOptions(context.Background(), runattempt.Open(home), "omp-native-verifier", RunInput{WorkID: "work_omp_start", Harness: "omp-native-verifier", Workspace: home, Command: "node", Now: time.Now().UTC()}, func(context.Context, RunInput) *exec.Cmd { return exec.Command(canary) }, defaultHeartbeatInterval, subprocessRunOptions{stableStartError: "OMP_PROCESS_START"})
	if err == nil || strings.Contains(err.Error(), canary) || !strings.Contains(err.Error(), "OMP_PROCESS_START") || out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	transcript, readErr := os.ReadFile(out.TranscriptPath)
	if readErr != nil || strings.Contains(string(transcript), canary) {
		t.Fatalf("transcript=%q err=%v", transcript, readErr)
	}
}

func TestOMPPrivateBytesRejectLengthDigestAndMalformed(t *testing.T) {
	raw := []byte("private-byte-canary")
	sum := sha256.Sum256(raw)
	encoded, digest := base64.StdEncoding.EncodeToString(raw), hex.EncodeToString(sum[:])
	for name, got := range map[string]error{"malformed": validateOMPPrivateBytes("request", "%%%", digest, len(raw)), "length": validateOMPPrivateBytes("request", encoded, digest, len(raw)+1), "digest": validateOMPPrivateBytes("request", encoded, strings.Repeat("0", 64), len(raw))} {
		if got == nil || strings.Contains(got.Error(), string(raw)) {
			t.Fatalf("%s error=%v", name, got)
		}
	}
	if err := validateOMPPrivateBytes("request", encoded, digest, len(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestOMPTranscriptRejectsExtraOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript")
	for name, body := range map[string]string{"extra stdout": "stdout: OMP_NATIVE_CAPTURE_READY\nstdout: private-canary\n", "stderr": "stdout: OMP_NATIVE_CAPTURE_READY\nstderr: private-canary\n", "missing": ""} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := validateOMPTranscript(path); err == nil || err.Error() != "OMP_PUBLIC_TRANSCRIPT_DRIFT" {
				t.Fatalf("error=%v", err)
			}
		})
	}
	if err := os.WriteFile(path, []byte("stdout: OMP_NATIVE_CAPTURE_READY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateOMPTranscript(path); err != nil {
		t.Fatal(err)
	}
}

func TestOMPWrapperArgvDriftRejected(t *testing.T) {
	if reflectStringSliceEqual([]string{"--cwd=private-canary"}, []string{"--cwd=expected"}) {
		t.Fatal("argv drift accepted")
	}
}

func TestOMPPrivateEnvelopeOversizeRejectedBeforeDecode(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	artifacts, err := homepkg.NewArtifactStore(home)
	if err != nil {
		t.Fatal(err)
	}
	spool, err := artifacts.CreatePrivateSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Remove()
	fd, err := spool.OpenForChild()
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), (12<<20)+1)
	if _, err := fd.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Consume(12 << 20); err == nil || strings.Contains(err.Error(), string(chunk[:64])) {
		t.Fatalf("oversize error=%v", err)
	}
}
