package lanes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

// An externally-owned adapter is reported, not obeyed as a veto. Before
// 2026-09-22 any enrolled oh_my_pi/opencode entry made refreshPreflightReady
// permanently false, so `./install.sh` could publish no binary at all on a home
// that enrolled one. The native lanes must still refresh, and the external
// adapter's own files must still be left to their installer.
func TestRefreshAllRefreshesNativeLanesAndReportsExternalAdapters(t *testing.T) {
	for _, external := range []string{"oh_my_pi", "opencode"} {
		t.Run(external, func(t *testing.T) {
			homePath, baseDir := t.TempDir(), t.TempDir()
			binaryPath := filepath.Join(t.TempDir(), "compound-synthetic")
			if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
				t.Fatal(err)
			}
			writeInstalledHooksManifest(t, homePath, map[string]string{
				"codex": "binary:" + binaryPath, external: "binary:" + binaryPath,
			})
			verdicts, err := RefreshAllInstalledHarnesses(RefreshLaneOptions{
				HomePath: homePath, BaseDir: baseDir, BinaryPath: binaryPath, Now: time.Now().UTC(),
			})
			if err != nil || len(verdicts) != 2 {
				t.Fatalf("refresh verdicts: %+v; error: %v", verdicts, err)
			}
			owner, repair := "adapters/opencode/install.ts", "bun adapters/opencode/install.ts"
			if external == "oh_my_pi" {
				owner = "compound hook install --harness oh-my-pi"
				repair = owner
			}
			if verdicts[1].Harness != external || verdicts[1].Status != statusRefreshExternal ||
				!strings.Contains(verdicts[1].Detail, owner) || verdicts[1].RepairCommand != repair {
				t.Fatalf("external adapter was not reported to its owner: %+v", verdicts[1])
			}
			// The whole point of the repair. With the veto in place the ladder
			// short-circuited and handed back the untouched preflight verdict, so
			// the native lane still read `ready` — the status it can only hold
			// while nothing has been refreshed. Any processed outcome (clean,
			// drifted, or a genuine per-lane error) proves the lane was reached;
			// a bare synthetic home has no lane default, so demanding success
			// here would assert an unrelated contract-compile precondition.
			if verdicts[0].Harness != "codex" || verdicts[0].Status == statusRefreshReady {
				t.Fatalf("external entry short-circuited the native lane: %+v", verdicts[0])
			}
			// The native writer still must not author the external adapter's surfaces.
			for _, untouched := range []string{
				filepath.Join(baseDir, ".omp", "agent", "hooks", "pre", "compound.ts"),
				filepath.Join(baseDir, ".config", "opencode", "plugins", "compound.ts"),
			} {
				if _, statErr := os.Stat(untouched); !os.IsNotExist(statErr) {
					t.Errorf("native refresh authored externally-owned %s: %v", untouched, statErr)
				}
			}
		})
	}
}

func TestRefreshPreflightCLIIsReadOnlyAndDoesNotClaimActivation(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), "absent-home")
	baseDir := filepath.Join(t.TempDir(), "absent-profile")
	binaryPath := filepath.Join(t.TempDir(), "compound-synthetic")
	args := []string{"--preflight", "--home", homePath, "--base", baseDir, "--binary", binaryPath}
	if err := RunCLIFromArgs(args, time.Now().UTC()); err != nil {
		t.Fatalf("cold-start preflight: %v", err)
	}
	if err := RunCLIFromArgs(append(args, "--harness", "codex"), time.Now().UTC()); err != nil {
		t.Fatalf("missing native config is eligible for later creation: %v", err)
	}
	for _, path := range []string{homePath, baseDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only preflight created %s: %v", path, err)
		}
	}
	if err := home.Init(homePath, home.HomeKindGlobal, time.Now()); err != nil {
		t.Fatal(err)
	}
	writeInstalledHooksManifest(t, homePath, map[string]string{"codex": "binary:synthetic", "opencode": "binary:synthetic"})
	// This is the install gate. An enrolled external adapter must not refuse it,
	// or the installer can never publish a binary on this home.
	if err := RunCLIFromArgs(args, time.Now().UTC()); err != nil {
		t.Fatalf("all-lane preflight refused publication over an externally-owned adapter: %v", err)
	}
	// The full refresh may still report a per-lane problem in this bare home
	// (no lane default means no contract compile). What it must never do is fail
	// over the external adapter — not at the preflight gate, and not at the
	// completeness check that decides the CLI's exit code. Naming the adapter is
	// the point: an assertion that only excluded the preflight message let
	// `refresh incomplete for oh_my_pi: external` reach a real install.
	if err := RunCLIFromArgs(args[1:], time.Now().UTC()); err != nil &&
		(strings.Contains(err.Error(), "refresh preflight refused") || strings.Contains(err.Error(), "opencode")) {
		t.Fatalf("full refresh failed over an externally-owned adapter: %v", err)
	}
	if err := RunCLIFromArgs(append(args, "--harness", "codex"), time.Now().UTC()); err != nil {
		t.Fatalf("explicit native lane was blocked by unrelated external entry: %v", err)
	}
	// A lane with no owner at all is still a hard refusal; only a lane whose
	// installer is known and whose hook carries no pinned identity is waived.
	if err := RunCLIFromArgs(append(args, "--harness", "unrecognized"), time.Now().UTC()); err == nil {
		t.Fatal("preflight accepted a harness with no refresh owner")
	}
	if v := preflightLaneRefresh(RefreshLaneOptions{HomePath: homePath, BaseDir: baseDir, BinaryPath: binaryPath}, "unrecognized"); v.Status != StatusError {
		t.Fatalf("unknown adapter accepted: %+v", v)
	}
}

// The refresh CLI's exit code is decided by refreshIncompleteVerdict. Reaching
// it through RunCLIFromArgs cannot prove this rule: in a bare synthetic home the
// native lane is itself non-clean and is selected first, masking the external
// lane entirely. That masking is exactly why `refresh incomplete for oh_my_pi:
// external` survived the first repair and only surfaced on a real install, once
// the operator's claude_code lane went clean. Assert the rule directly.
func TestRefreshIncompleteVerdictNeverBlamesAnExternalAdapter(t *testing.T) {
	external := LaneVerdict{Harness: "oh_my_pi", Status: statusRefreshExternal, Detail: "separate adapter owns this lane"}
	for _, tc := range []struct {
		name     string
		verdicts []LaneVerdict
		want     string // "" means no lane should fail the CLI
	}{
		{name: "every native lane clean, one external", want: "", verdicts: []LaneVerdict{
			{Harness: "codex", Status: StatusClean}, external,
		}},
		{name: "external alone", want: "", verdicts: []LaneVerdict{external}},
		{name: "a real native failure still fails", want: "codex", verdicts: []LaneVerdict{
			{Harness: "codex", Status: StatusError, Detail: "hook install failed"}, external,
		}},
		{name: "external never outranks a native failure", want: "codex", verdicts: []LaneVerdict{
			external, {Harness: "codex", Status: StatusDrifted},
		}},
		{name: "a genuine failure replaces a bare ready", want: "codex", verdicts: []LaneVerdict{
			{Harness: "claude_code", Status: statusRefreshReady}, external, {Harness: "codex", Status: StatusError},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := refreshIncompleteVerdict(tc.verdicts)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("externally-owned lane failed the refresh CLI: %+v", *got)
				}
				return
			}
			if got == nil || got.Harness != tc.want {
				t.Fatalf("incomplete verdict = %v, want harness %q", got, tc.want)
			}
		})
	}
}
