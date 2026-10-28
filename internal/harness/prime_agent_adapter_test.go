package harness

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// TestPrimeAgentAdapterName pins the registry key. The AdapterRegistry
// looks up by this exact string; the harness registry row names the
// same string "prime_agent" and the RunAttempt.Harness field carries
// the same string. A drift between these three is a silent routing
// failure.
func TestPrimeAgentAdapterName(t *testing.T) {
	a := NewPrimeAgentAdapter(runattempt.Open(t.TempDir()))
	if got, want := a.Name(), "prime_agent"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
}

// TestPrimeAgentAdapterRunMintsRunAttempt is the mint-before-side-effect
// contract applied to prime-agent: the RunAttempt is created BEFORE
// exec.CommandContext is invoked, and the terminal status lands
// regardless of the subprocess exit.
//
// The subprocess we drive is `true` (PATH-resolved on every Unix) so
// the side effect is observable and the run succeeds end-to-end. The
// RunAttempt.Harness field must be "prime_agent" so the registry
// lookup and the audit chain remain consistent.
func TestPrimeAgentAdapterRunMintsRunAttempt(t *testing.T) {
	store := runattempt.Open(t.TempDir())
	a := NewPrimeAgentAdapter(store)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	out, err := a.Run(context.Background(), RunInput{
		WorkID:         "work_primeagent_unit",
		Workspace:      t.TempDir(),
		Command:        "true",
		PermissionMode: "oneshot",
		Prompt:         "hello",
		Now:            now,
	})
	if err != nil {
		t.Fatalf("Run returned err: %v", err)
	}
	if out.RunAttempt.ID == "" {
		t.Fatal("RunAttempt.ID is empty")
	}
	if out.RunAttempt.Harness != "prime_agent" {
		t.Fatalf("RunAttempt.Harness = %q, want %q", out.RunAttempt.Harness, "prime_agent")
	}
	if out.RunAttempt.Status != "succeeded" {
		t.Fatalf("RunAttempt.Status = %q, want %q (subprocess should succeed)", out.RunAttempt.Status, "succeeded")
	}
	if out.TranscriptPath == "" {
		t.Fatal("TranscriptPath is empty")
	}
}

// TestPrimeAgentAdapterBuildCmdRPC verifies the default modal is
// `--mode rpc` so the persistent-kernel JSON-RPC supervisor continues
// to receive structured output. This is the headless mode the run
// delegate requires for `--dry-run` readiness proofs.
func TestPrimeAgentAdapterBuildCmdRPC(t *testing.T) {
	a := NewPrimeAgentAdapter(runattempt.Open(t.TempDir()))
	ws := t.TempDir()
	cmd := a.buildCmd(context.Background(), RunInput{
		WorkID:    "work_primeagent_rpc",
		Workspace: ws,
		Prompt:    "echo hello",
		Now:       time.Now(),
	})
	if got := cmd.Path; !strings.HasSuffix(got, "prime-agent") {
		t.Fatalf("cmd.Path = %q, want suffix prime-agent", got)
	}
	if got, want := cmd.Dir, ws; got != want {
		t.Fatalf("cmd.Dir = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(cmd.Args[:3], []string{"prime-agent", "--mode", "rpc"}) {
		t.Fatalf("cmd.Args = %v, want first three to be [prime-agent --mode rpc]", cmd.Args)
	}
	// PRIME_AGENT_CODING_AGENT_DIR isolation line must be present (per-run, never ~/.prime/agent).
	var sawPrimeHome bool
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PRIME_AGENT_CODING_AGENT_DIR=") && !strings.Contains(kv, "/.prime/agent") {
			sawPrimeHome = true
		}
	}
	if !sawPrimeHome {
		t.Fatal("PRIME_AGENT_CODING_AGENT_DIR isolation env missing or pointing at ~/.prime/agent")
	}
	// PI_SKIP_VERSION_CHECK=1 must be set so prime does not self-update mid-run.
	var sawSkipUpdate bool
	for _, kv := range cmd.Env {
		if kv == "PI_SKIP_VERSION_CHECK=1" {
			sawSkipUpdate = true
		}
	}
	if !sawSkipUpdate {
		t.Fatal("PI_SKIP_VERSION_CHECK=1 missing — prime would self-update mid-run and invalidate the RunAttempt chrome")
	}
	// The adapter MUST NOT set the legacy/wrong PRIME_AGENT_HOME: upstream
	// prime-agent reads PRIME_AGENT_CODING_AGENT_DIR (packages/coding-agent/src/config.ts);
	// PRIME_AGENT_HOME would silently share ~/.prime/agent across runs.
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PRIME_AGENT_HOME=") {
			t.Fatalf("legacy PRIME_AGENT_HOME present (%q); upstream prime-agent does not read this env", kv)
		}
	}
}

// TestPrimeAgentAdapterBuildCmdOneshot stamps the -p --no-session path
// when the caller explicitly opts in via PermissionMode="oneshot".
// This is the path the verifier uses for `--dry-run` proofs.
func TestPrimeAgentAdapterBuildCmdOneshot(t *testing.T) {
	a := NewPrimeAgentAdapter(runattempt.Open(t.TempDir()))
	cmd := a.buildCmd(context.Background(), RunInput{
		WorkID:         "work_primeagent_oneshot",
		Workspace:      t.TempDir(),
		PermissionMode: "oneshot",
		Prompt:         "echo hello",
		Now:            time.Now(),
	})
	if !reflect.DeepEqual(cmd.Args[:3], []string{"prime-agent", "-p", "--no-session"}) {
		t.Fatalf("cmd.Args = %v, want first three to be [prime-agent -p --no-session]", cmd.Args)
	}
}

// TestPrimeAgentAdapterWorkspaceNeverCompoundHome is the named
// falsifier: the adapter MUST NOT stamp COMPOUND_HOME, ~/.compound, or
// any other workspace root that the run delegate did not authorize.
// A regression here is the same kind of failure the work delegate
// catches at the workspace-resolution layer.
func TestPrimeAgentAdapterWorkspaceNeverCompoundHome(t *testing.T) {
	a := NewPrimeAgentAdapter(runattempt.Open(t.TempDir()))
	// Even if the caller forgot to set Workspace, the adapter must
	// surface a hard error rather than silently fall back to
	// COMPOUND_HOME. We assert by stamping "." and confirming the
	// adapter never slammed in ~/.compound.
	cmd := a.buildCmd(context.Background(), RunInput{
		WorkID:    "work_primeagent_noworkspace",
		Workspace: "",
		Prompt:    "echo hello",
		Now:       time.Now(),
	})
	// Derive the operator home at runtime: hardcoding an absolute home path
	// would both leak it into a public-destined tree and only assert for one user.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if strings.HasPrefix(cmd.Dir, filepath.Join(home, ".compound")) {
			t.Fatalf("cmd.Dir = %q must not resolve to ~/.compound", cmd.Dir)
		}
	}
}

// TestPrimeAgentAdapterDryRunProbe is the readiness proof: format the
// argv without executing it, and confirm the four invariants the
// verifier asserts:
//
//  1. The binary is prime-agent (no fall-through to codex / claude).
//  2. The mode is `--mode rpc` (default) or `-p --no-session` (oneshot).
//  3. The workspace is the run-delegate-resolved value, never a
//     fallback to cwd / COMPOUND_HOME / ~/.compound.
//  4. The prompt is present (no empty-prompt dispatch).
func TestPrimeAgentAdapterDryRunProbe(t *testing.T) {
	ws := t.TempDir()
	rpc := FormatPrimeAgentDryRunPrompt(RunInput{
		WorkID:    "work_primeagent_dryrun_rpc",
		Workspace: ws,
		Prompt:    "verify a proof",
		Now:       time.Now(),
	})
	if !strings.Contains(rpc, "prime-agent") {
		t.Fatalf("rpc summary missing prime-agent: %q", rpc)
	}
	if !strings.Contains(rpc, "mode=rpc") {
		t.Fatalf("rpc summary missing mode=rpc: %q", rpc)
	}
	if !strings.Contains(rpc, ws) {
		t.Fatalf("rpc summary missing workspace %q: %q", ws, rpc)
	}
	if !strings.Contains(rpc, "verify a proof") {
		t.Fatalf("rpc summary missing prompt: %q", rpc)
	}

	oneshot := FormatPrimeAgentDryRunPrompt(RunInput{
		WorkID:         "work_primeagent_dryrun_oneshot",
		Workspace:      ws,
		PermissionMode: "oneshot",
		Prompt:         "verify a proof",
		Now:            time.Now(),
	})
	if !strings.Contains(oneshot, "mode=oneshot") {
		t.Fatalf("oneshot summary missing mode=oneshot: %q", oneshot)
	}

	// unset workspace is visible in the summary so the verifier can
	// detect a workspace-resolution failure.
	unresolved := FormatPrimeAgentDryRunPrompt(RunInput{
		WorkID: "work_primeagent_dryrun_unresolved",
		Prompt: "verify a proof",
		Now:    time.Now(),
	})
	if !strings.Contains(unresolved, "(unset)") {
		t.Fatalf("unset workspace must be flagged: %q", unresolved)
	}
}
