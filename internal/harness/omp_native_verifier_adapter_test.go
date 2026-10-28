package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestOMPNativeVerifierRejectsCallerExecutionSurface(t *testing.T) {
	a := NewOMPNativeVerifierAdapter(runattempt.Open(t.TempDir()))
	base := RunInput{WorkID: "work_omp_native", Workspace: t.TempDir(), Prompt: "prove prompt only", Now: time.Now().UTC()}
	for name, alter := range map[string]func(*RunInput){
		"command":     func(in *RunInput) { in.Command = "sh" },
		"args":        func(in *RunInput) { in.Args = []string{"-c", "unsafe"} },
		"env":         func(in *RunInput) { in.Env = []string{"UNSAFE=1"} },
		"mode":        func(in *RunInput) { in.PermissionMode = "bypass" },
		"dash prompt": func(in *RunInput) { in.Prompt = "--unsafe" },
		"at prompt":   func(in *RunInput) { in.Prompt = "@unsafe" },
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			alter(&in)
			if _, err := a.Preflight(context.Background(), in); err == nil {
				t.Fatal("Preflight accepted caller-controlled execution surface")
			}
		})
	}
}

func TestOMPNativeVerifierRejectsUngovernedForgedSuccessBeforeChild(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	logPath := filepath.Join(home, "capture")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMP_NATIVE_CAPTURE\"\nprintf '%s\\n' \"$PI_CONFIG_DIR\" >> \"$OMP_NATIVE_CAPTURE\"\nprintf '%s\\n' \"$PI_CODING_AGENT_DIR\" >> \"$OMP_NATIVE_CAPTURE\"\nprintf '%s\\n' '{\"schema_version\":\"compound.omp_native_verifier_report.v0.1\",\"result\":\"no_tools_and_tool_response_refused\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "omp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OMP_NATIVE_CAPTURE", logPath)
	a := NewOMPNativeVerifierAdapter(runattempt.Open(home))
	workspace := t.TempDir()
	if _, err := a.Run(context.Background(), RunInput{WorkID: "work_omp_native", Workspace: workspace, Prompt: "prove prompt only", Now: time.Now().UTC()}); err == nil {
		t.Fatal("ungoverned fake omp success was allowed to launch")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("forged omp child was launched: %v", err)
	}
}
