package hookplan

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// omp has no message channel at session_start, so the door map and session
// summary reach the model only if the shim carries them into the first
// before_agent_start message — once, ahead of the prompt's own ContextPack.
func TestOmpShimRuntimeCarriesSessionStartContextToFirstMessage(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is required to execute the shipped OMP shim")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// argv: hook <verb> --live --harness oh_my_pi; answer per verb.
	body := "#!/bin/sh\nf=\"$HOME/$2.json\"\nif [ -f \"$f\" ]; then cat \"$f\"; else echo '{}'; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "compound"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	driver, err := filepath.Abs(filepath.Join("testdata", "omp_shim_session_start_test.ts"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bun, driver)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("OMP shim dropped session-start context: %v\n%s", err, out)
	}
}

func TestOmpShimRuntimeUsesNativeSessionContextAtEveryBoundary(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is required to execute the shipped OMP shim")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ncat >> \"$HOME/fixture-calls.jsonl\"\nprintf '\\n' >> \"$HOME/fixture-calls.jsonl\"\nprintf '%s\\n' '{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"additionalContext\":\"boundary-pack\",\"permissionDecision\":\"allow\"}}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "compound"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	driver, err := filepath.Abs(filepath.Join("testdata", "omp_shim_session_identity_test.ts"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bun, driver)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("OMP shim used the wrong native session: %v\n%s", err, out)
	}
}
