package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// The parent must own cleanup: a killed recipe cannot execute a JS finally.
func TestOMPVerifierRemovesOwnedProbeDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, native string
		timeout      time.Duration
		refuse       bool
		wantStatus   string
	}{
		{"success", "", 10 * time.Second, false, runattempt.StatusSucceeded},
		{"native_failure", "#!/usr/bin/python3\nimport sys\nsys.exit(7)\n", 10 * time.Second, false, runattempt.StatusFailed},
		{"outer_timeout", "#!/usr/bin/python3\nimport time\ntime.sleep(5)\n", 700 * time.Millisecond, false, runattempt.StatusFailed},
		{"start_refused", "", 10 * time.Second, true, runattempt.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOMPVerifierFixture(t, tc.native)
			probeParent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", probeParent)
			f.input.Timeout = tc.timeout
			if tc.refuse {
				f.input.StartGuard = func(context.Context, func() error) error { return errors.New("synthetic start refusal") }
			}
			out, _ := f.adapter.Run(context.Background(), f.input)
			if out.RunAttempt.Status != tc.wantStatus {
				t.Fatalf("fixture outcome=%s want=%s", out.RunAttempt.Status, tc.wantStatus)
			}
			if tc.name == "outer_timeout" && !out.TimedOut {
				t.Fatal("timeout fixture did not engage")
			}
			entries, err := os.ReadDir(probeParent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("owned private probe directories retained after %s: count=%d", tc.name, len(entries))
			}
		})
	}
}
