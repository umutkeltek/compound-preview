package harness

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// TestOhMyPiBuildCmdCarriesPostureAndHook is the argv-shape falsifier for
// the R7B "no inheritance" rule: every governed oh_my_pi launch MUST carry
// the explicit --approval-mode and --hook= fragments the spec owns,
// regardless of any host config that might otherwise be inherited. The OMP
// CLI's default approval-mode is yolo (the headless mutation the wrapper
// failed closed for), so the only safe dispatch is the one that stamps
// both flags explicitly.
func TestOhMyPiBuildCmdCarriesPostureAndHook(t *testing.T) {
	t.Run("bypass_stamps_yolo_and_hook", func(t *testing.T) {
		fakeOhMyPiBinary(t)
		t.Setenv("COMPOUND_OMP_HOOK_PATH", "/tmp/compound.ts")
		a := NewOhMyPiAdapter(runattempt.Open(t.TempDir()))
		cmd := a.buildCmd(context.Background(), RunInput{
			WorkID:         "work_ohmypi_bypass",
			Workspace:      t.TempDir(),
			PermissionMode: "bypass",
			Prompt:         "do the thing",
			Now:            time.Now(),
		})
		hasYolo, hasHook := false, false
		for _, arg := range cmd.Args {
			if arg == "--approval-mode=yolo" {
				hasYolo = true
			}
			if arg == "--hook=/tmp/compound.ts" {
				hasHook = true
			}
		}
		if !hasYolo {
			t.Fatalf("bypass argv missing --approval-mode=yolo; got %v", cmd.Args)
		}
		if !hasHook {
			t.Fatalf("bypass argv missing --hook=<path>; got %v", cmd.Args)
		}
		if cmd.Path == "/bin/sh" {
			t.Fatalf("bypass launch was refused (sh -c) with a configured shim: %v", cmd.Args)
		}
	})

	t.Run("default_stamps_always_ask_and_hook", func(t *testing.T) {
		fakeOhMyPiBinary(t)
		t.Setenv("COMPOUND_OMP_HOOK_PATH", "/tmp/compound.ts")
		a := NewOhMyPiAdapter(runattempt.Open(t.TempDir()))
		cmd := a.buildCmd(context.Background(), RunInput{
			WorkID:    "work_ohmypi_default",
			Workspace: t.TempDir(),
			Prompt:    "do the thing",
			Now:       time.Now(),
		})
		hasAlwaysAsk, hasHook := false, false
		for _, arg := range cmd.Args {
			if arg == "--approval-mode=always-ask" {
				hasAlwaysAsk = true
			}
			if arg == "--hook=/tmp/compound.ts" {
				hasHook = true
			}
		}
		if !hasAlwaysAsk {
			t.Fatalf("default argv missing --approval-mode=always-ask; got %v", cmd.Args)
		}
		if !hasHook {
			t.Fatalf("default argv missing --hook=<path>; got %v", cmd.Args)
		}
	})

	t.Run("empty_shim_skips_hook_for_non_bypass", func(t *testing.T) {
		fakeOhMyPiBinary(t)
		t.Setenv("COMPOUND_OMP_HOOK_PATH", "")
		a := NewOhMyPiAdapter(runattempt.Open(t.TempDir()))
		cmd := a.buildCmd(context.Background(), RunInput{
			WorkID:         "work_ohmypi_no_shim",
			Workspace:      t.TempDir(),
			PermissionMode: "acceptEdits",
			Prompt:         "do the thing",
			Now:            time.Now(),
		})
		for _, arg := range cmd.Args {
			if strings.HasPrefix(arg, "--hook=") {
				t.Fatalf("empty shim path MUST skip --hook= for non-bypass; got %v", cmd.Args)
			}
		}
	})

	t.Run("unknown_posture_refused", func(t *testing.T) {
		fakeOhMyPiBinary(t)
		t.Setenv("COMPOUND_OMP_HOOK_PATH", "/tmp/compound.ts")
		a := NewOhMyPiAdapter(runattempt.Open(t.TempDir()))
		cmd := a.buildCmd(context.Background(), RunInput{
			WorkID:         "work_ohmypi_bad_posture",
			Workspace:      t.TempDir(),
			PermissionMode: "guess-mode",
			Prompt:         "do the thing",
			Now:            time.Now(),
		})
		if cmd.Path != "/bin/sh" {
			t.Fatalf("unknown posture MUST refuse via sh -c, got cmd.Path=%q", cmd.Path)
		}
		joined := strings.Join(cmd.Args, " ")
		if !strings.Contains(joined, "unknown permission mode") || !strings.Contains(joined, "guess-mode") {
			t.Fatalf("refusal reason MUST name the bad mode, got %q", joined)
		}
	})
}

// TestOhMyPiBypassRefusesWithoutShim is the R7B launch-readiness refusal:
// PermissionMode=bypass without a loadable shim file MUST refuse at the
// adapter Run boundary (BEFORE runWithSubprocess mints a RunAttempt).
func TestOhMyPiBypassRefusesWithoutShim(t *testing.T) {
	cases := []struct {
		name        string
		envValue    string
		setEnv      bool
		wantErrFrag string
	}{
		{"empty_env_refused", "", true, "is empty"},
		{"whitespace_env_refused", "   ", true, "is empty"},
		{"missing_file_refused", "/tmp/definitely-not-a-shim-2026-09-11.ts", true, "shim not found at"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeOhMyPiBinary(t)
			if tc.setEnv {
				t.Setenv("COMPOUND_OMP_HOOK_PATH", tc.envValue)
			} else {
				_ = os.Unsetenv("COMPOUND_OMP_HOOK_PATH")
			}
			store := runattempt.Open(t.TempDir())
			a := NewOhMyPiAdapter(store)
			out, err := a.Run(context.Background(), RunInput{
				WorkID:         "work_ohmypi_bypass_refuse",
				Workspace:      t.TempDir(),
				PermissionMode: "bypass",
				Prompt:         "hello",
				Now:            time.Now(),
			})
			if err == nil {
				t.Fatalf("expected refusal error, got RunAttempt=%+v", out.RunAttempt)
			}
			if !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrFrag)
			}
			if out.RunAttempt.ID != "" {
				t.Fatalf("refused launch must NOT mint a RunAttempt; got ID=%q", out.RunAttempt.ID)
			}
		})
	}
}

// TestOhMyPiBypassAcceptsLoadableShim is the GREEN counterpart: a bypass
// launch with a loadable shim file on disk passes precheckLaunch and the
// subprocess runs.
func TestOhMyPiBypassAcceptsLoadableShim(t *testing.T) {
	fakeOhMyPiBinary(t)
	shimPath := t.TempDir() + "/compound.ts"
	if err := os.WriteFile(shimPath, []byte("// compound-owned: oh-my-pi-hook-shim@2\n"), 0o644); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("COMPOUND_OMP_HOOK_PATH", shimPath)
	store := runattempt.Open(t.TempDir())
	a := NewOhMyPiAdapter(store)
	out, err := a.Run(context.Background(), RunInput{
		WorkID:         "work_ohmypi_bypass_ok",
		Workspace:      t.TempDir(),
		PermissionMode: "bypass",
		Prompt:         "hello",
		Now:            time.Now(),
	})
	if err != nil {
		t.Fatalf("bypass launch with loadable shim MUST succeed, got %v", err)
	}
	if out.RunAttempt.Status != "succeeded" {
		t.Fatalf("RunAttempt.Status = %q, want succeeded", out.RunAttempt.Status)
	}
}
