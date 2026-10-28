package harness

import (
	homepkg "compound/internal/home"
	"compound/internal/runattempt"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestOMPVerifierContainsAsynchronousProbeFailures(t *testing.T) {
	for _, stream := range []int{1, 2} {
		t.Run(fmt.Sprintf("overflow_fd_%d", stream), func(t *testing.T) {
			body := fmt.Sprintf("#!/usr/bin/python3\nimport os,time\nos.write(%d,b'OVERFLOW_PRIVATE_CANARY'+b'x'*(4*1024*1024))\ntime.sleep(1)\n", stream)
			f := newOMPVerifierFixture(t, body)
			f.input.Timeout = 8 * time.Second
			out, err := f.adapter.Run(context.Background(), f.input)
			if err != nil {
				t.Fatal(err)
			}
			if out.RunAttempt.Status != runattempt.StatusFailed {
				t.Fatalf("status=%s", out.RunAttempt.Status)
			}
			transcript := string(mustReadTestFile(t, out.TranscriptPath))
			if transcript != "stderr: OMP_NATIVE_PROBE_FAILED\n" {
				t.Fatalf("public failure transcript escaped stable code: %q", transcript)
			}
			if out.TimedOut {
				t.Fatal("failure left recipe alive until outer timeout")
			}
			for _, ref := range out.RunAttempt.ProofRefs {
				if strings.HasPrefix(ref, "artifact:") {
					t.Fatalf("failure certified: %s", ref)
				}
			}
		})
	}
}

func TestOMPFailedProbeRetainsPrivateCapture(t *testing.T) {
	cases := []struct {
		name, body string
		timeout    time.Duration
		timedOut   bool
	}{
		{"native_exit", "#!/usr/bin/python3\nimport sys\nsys.stderr.write('PRIVATE_FAILURE_CANARY')\nsys.exit(7)\n", 5 * time.Second, false},
		{"outer_timeout", "#!/usr/bin/python3\nimport time\ntime.sleep(5)\n", 50 * time.Millisecond, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOMPVerifierFixture(t, tc.body)
			f.input.Timeout = tc.timeout
			out, err := f.adapter.Run(context.Background(), f.input)
			if err != nil {
				t.Fatal(err)
			}
			if out.RunAttempt.Status != runattempt.StatusFailed || out.TimedOut != tc.timedOut {
				t.Fatalf("outcome status=%s timeout=%v", out.RunAttempt.Status, out.TimedOut)
			}
			const prefix = "failed_private_capture_sha256:"
			var digest string
			for _, ref := range out.RunAttempt.ProofRefs {
				if strings.HasPrefix(ref, "artifact:") {
					t.Fatalf("failure certified: %s", ref)
				}
				if strings.HasPrefix(ref, prefix) {
					if digest != "" {
						t.Fatal("duplicate failed capture")
					}
					digest = strings.TrimPrefix(ref, prefix)
				}
			}
			if len(digest) != 64 {
				t.Fatalf("missing failed private capture marker: %v", out.RunAttempt.ProofRefs)
			}
			artifacts, err := homepkg.NewArtifactStore(f.home)
			if err != nil {
				t.Fatal(err)
			}
			body, err := artifacts.GetPrivate(homepkg.ArtifactRefPrefix + digest)
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				Schema   string `json:"schema_version"`
				Work     string `json:"work_id"`
				Attempt  string `json:"run_attempt_id"`
				Action   string `json:"action_intent_id"`
				TimedOut bool   `json:"timed_out"`
				Length   int    `json:"captured_length"`
				Digest   string `json:"captured_sha256"`
				Captured string `json:"captured_base64"`
			}
			if err := json.Unmarshal(body, &report); err != nil {
				t.Fatal(err)
			}
			captured, err := base64.StdEncoding.DecodeString(report.Captured)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(captured)
			if report.Schema != "compound.omp_failed_capture.v0.1" || report.Work != f.input.WorkID || report.Attempt != out.RunAttempt.ID || report.Action != f.input.ActionIntentID || report.TimedOut != tc.timedOut || report.Length != len(captured) || report.Digest != hex.EncodeToString(sum[:]) {
				t.Fatalf("failure capture binding mismatch: %+v", report)
			}
			if !tc.timedOut {
				var snapshot struct {
					Partial struct {
						LastChild struct {
							Stderr string `json:"stderr_base64"`
						} `json:"last_child"`
					} `json:"partial"`
				}
				if err := json.Unmarshal(captured, &snapshot); err != nil {
					t.Fatal(err)
				}
				stderr, err := base64.StdEncoding.DecodeString(snapshot.Partial.LastChild.Stderr)
				if err != nil || string(stderr) != "PRIVATE_FAILURE_CANARY" {
					t.Fatal("exact native stderr bytes lost from private snapshot")
				}
			}
			transcript := string(mustReadTestFile(t, out.TranscriptPath))
			if strings.Contains(transcript, "PRIVATE_FAILURE_CANARY") || strings.Contains(transcript, f.workspace) {
				t.Fatal("private capture leaked publicly")
			}
		})
	}
}
