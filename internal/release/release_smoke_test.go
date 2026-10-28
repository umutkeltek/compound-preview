package release

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyReleaseSmokeRefusesWithoutEffects(t *testing.T) {
	scratch := t.TempDir()
	// A deliberately absent identity makes the historical script refuse before
	// its unsafe build/install body. Never exercise that body against a real home.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "../../scripts/release-smoke.sh")
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		"COMPOUND_RELEASE_IDENTITY_FILE="+filepath.Join(scratch, "absent"),
		"COMPOUND_HOME="+filepath.Join(scratch, "home"),
		"COMPOUND="+filepath.Join(scratch, "compound"),
		"LOG_DIR="+filepath.Join(scratch, "logs"))
	out, err := cmd.CombinedOutput()
	if err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 2 {
		t.Fatalf("legacy entrypoint must refuse with exit 2: %v\n%s", err, out)
	}
	for _, want := range []string{"retired", "proof-check", "docs/operations/recovery.md"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing safe replacement guidance %q: %s", want, out)
		}
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("retired smoke created state: entries=%v err=%v", entries, err)
	}
}

func TestLegacyReleaseSmokeValidIdentityCannotPublish(t *testing.T) {
	scratch := t.TempDir()
	identity := filepath.Join(scratch, "identity")
	binary := filepath.Join(scratch, "compound")
	home := filepath.Join(scratch, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(home, "sentinel")
	for name, body := range map[string]string{identity: "operator@example.invalid\n", binary: "previous-binary", sentinel: "preserved-state"} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "../../scripts/release-smoke.sh")
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "COMPOUND_RELEASE_IDENTITY_FILE="+identity,
		"COMPOUND="+binary, "COMPOUND_HOME="+home, "LOG_DIR="+filepath.Join(scratch, "logs"))
	out, err := cmd.CombinedOutput()
	if err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "retired") {
		t.Fatalf("valid identity must not reactivate retired entrypoint: %v\n%s", err, out)
	}
	for name, want := range map[string]string{binary: "previous-binary", sentinel: "preserved-state"} {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != want {
			t.Fatalf("retired smoke changed %s: %q err=%v", filepath.Base(name), got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(scratch, "logs")); !os.IsNotExist(err) {
		t.Fatalf("retired smoke created logs: %v", err)
	}
}
