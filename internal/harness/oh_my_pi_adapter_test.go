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

// fakeOhMyPiBinary installs an executable named `omp` in a fresh temp dir and
// puts that dir first on PATH for the duration of the test. Two reasons this
// is not optional:
//
//  1. exec.CommandContext resolves argv[0] against PATH at construction time,
//     so without a real file on PATH cmd.Path stays the bare name and the test
//     would assert nothing about resolution.
//  2. The real `omp` is a network-dialling coding agent. A test that shells the
//     installed binary would be a test of the operator's machine, and would
//     spend tokens to do it.
//
// Returns the fake binary's absolute path.
func fakeOhMyPiBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "omp")
	// Exit 0 and print nothing: the adapter contract under test is the argv and
	// the environment, not the child's output.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake omp: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// TestOhMyPiAdapterName pins the registry key. The AdapterRegistry looks up by
// this exact string, the harness registry row names the same string, and the
// RunAttempt.Harness field carries it. Drift between the three is a silent
// routing failure.
func TestOhMyPiAdapterName(t *testing.T) {
	a := NewOhMyPiAdapter(runattempt.Open(t.TempDir()))
	if got, want := a.Name(), "oh_my_pi"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
}

// TestOhMyPiAdapterRunMintsRunAttempt is the mint-before-side-effect contract
// applied to oh-my-pi: the RunAttempt is created BEFORE exec.CommandContext is
// invoked, and the terminal status lands regardless of the subprocess exit.
//
// The subprocess is the PATH-resolved fake, so the side effect is observable
// and the run succeeds end-to-end without touching the real coding agent.
func TestOhMyPiAdapterRunMintsRunAttempt(t *testing.T) {
	fakeOhMyPiBinary(t)
	store := runattempt.Open(t.TempDir())
	a := NewOhMyPiAdapter(store)
	out, err := a.Run(context.Background(), RunInput{
		WorkID:         "work_ohmypi_unit",
		Workspace:      t.TempDir(),
		PermissionMode: "oneshot",
		Prompt:         "hello",
		Now:            time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run returned err: %v", err)
	}
	if out.RunAttempt.ID == "" {
		t.Fatal("RunAttempt.ID is empty")
	}
	if out.RunAttempt.Harness != "oh_my_pi" {
		t.Fatalf("RunAttempt.Harness = %q, want %q", out.RunAttempt.Harness, "oh_my_pi")
	}
	if out.RunAttempt.Status != "succeeded" {
		t.Fatalf("RunAttempt.Status = %q, want %q", out.RunAttempt.Status, "succeeded")
	}
	if out.TranscriptPath == "" {
		t.Fatal("TranscriptPath is empty")
	}
	// Run() must default the command to the family spec's binary, not leave it
	// empty and not borrow prime-agent's.
	if !strings.HasSuffix(out.RunAttempt.Command, "omp") {
		t.Fatalf("RunAttempt.Command = %q, want it to end in omp", out.RunAttempt.Command)
	}
}

// TestOhMyPiAdapterBuildCmdRPC verifies the default modal is `--mode rpc` and
// that the binary actually resolves through PATH to the oh-my-pi entry point.
// `--mode` accepts text|json|rpc on this fork, so rpc is a real value here and
// not a prime-agent-ism carried over by the shared adapter.
func TestOhMyPiAdapterBuildCmdRPC(t *testing.T) {
	fake := fakeOhMyPiBinary(t)
	// Empty PermissionMode (default) maps to --approval-mode=always-ask via
	// the spec posture fn. Set the shim path so buildCmd also emits --hook=.
	t.Setenv("COMPOUND_OMP_HOOK_PATH", "/tmp/compound.ts")
	a := NewOhMyPiAdapter(runattempt.Open(t.TempDir()))
	ws := t.TempDir()
	cmd := a.buildCmd(context.Background(), RunInput{
		WorkID:    "work_ohmypi_rpc",
		Workspace: ws,
		Prompt:    "echo hello",
		Now:       time.Now(),
	})
	if cmd.Path != fake {
		t.Fatalf("cmd.Path = %q, want the PATH-resolved fake %q", cmd.Path, fake)
	}
	if got, want := cmd.Dir, ws; got != want {
		t.Fatalf("cmd.Dir = %q, want %q", got, want)
	}
	// argv shape: omp --mode rpc --cwd WS --approval-mode=always-ask --hook=... <prompt>
	wantHead := []string{"omp", "--mode", "rpc", "--cwd", ws, "--approval-mode=always-ask", "--hook=/tmp/compound.ts"}
	if !reflect.DeepEqual(cmd.Args[:len(wantHead)], wantHead) {
		t.Fatalf("cmd.Args[:%d] = %v, want %v", len(wantHead), cmd.Args[:len(wantHead)], wantHead)
	}
	if got := cmd.Args[len(cmd.Args)-1]; got != "echo hello" {
		t.Fatalf("prompt must be last positional, got %q (args=%v)", got, cmd.Args)
	}

	// PI_CODING_AGENT_DIR isolation: oh-my-pi reads this var for its agent home
	// (documented in the shipped help as the session storage directory). It must
	// point at a fresh per-run directory, never the shared ~/.omp/agent.
	var agentHome string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PI_CODING_AGENT_DIR=") {
			agentHome = strings.TrimPrefix(kv, "PI_CODING_AGENT_DIR=")
		}
	}
	if agentHome == "" {
		t.Fatal("PI_CODING_AGENT_DIR isolation env missing — the run would share the real agent home")
	}
	if strings.Contains(agentHome, filepath.Join(".omp", "agent")) {
		t.Fatalf("PI_CODING_AGENT_DIR = %q must not resolve to the shared ~/.omp/agent", agentHome)
	}
	if info, err := os.Stat(agentHome); err != nil || !info.IsDir() {
		t.Fatalf("PI_CODING_AGENT_DIR = %q must exist as a directory (err=%v)", agentHome, err)
	}

	// The adapter MUST NOT carry prime-agent's env into this fork. Setting a var
	// a binary does not read is a claim nothing enforces, and the next reader
	// would believe oh-my-pi's self-update is neutralised when it is not.
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PRIME_AGENT_CODING_AGENT_DIR=") {
			t.Fatalf("prime-agent home env leaked into the oh-my-pi argv: %q", kv)
		}
		if strings.HasPrefix(kv, "PI_SKIP_VERSION_CHECK=") {
			t.Fatalf("PI_SKIP_VERSION_CHECK present (%q); oh-my-pi has no such env and the registry limitation says so", kv)
		}
	}
}

// TestOhMyPiAdapterBuildCmdOneshot stamps the -p --no-session path when the
// caller opts in via PermissionMode="oneshot" — the path the verifier uses for
// `--dry-run` proofs, and the one that leaves no session state behind.
func TestOhMyPiAdapterBuildCmdOneshot(t *testing.T) {
	fakeOhMyPiBinary(t)
	// oneshot maps to -p --no-session in buildCmd AND to always-ask in
	// OhMyPiApprovalArgs (a dry-run never carries interactive yolo).
	t.Setenv("COMPOUND_OMP_HOOK_PATH", "/tmp/compound.ts")
	a := NewOhMyPiAdapter(runattempt.Open(t.TempDir()))
	cmd := a.buildCmd(context.Background(), RunInput{
		WorkID:         "work_ohmypi_oneshot",
		Workspace:      t.TempDir(),
		PermissionMode: "oneshot",
		Prompt:         "echo hello",
		Now:            time.Now(),
	})
	wantHead := []string{"omp", "-p", "--no-session", "--cwd", cmd.Args[4], "--approval-mode=always-ask", "--hook=/tmp/compound.ts"}
	if !reflect.DeepEqual(cmd.Args[:len(wantHead)], wantHead) {
		t.Fatalf("cmd.Args[:%d] = %v, want %v", len(wantHead), cmd.Args[:len(wantHead)], wantHead)
	}
}

// TestOhMyPiAdapterPassesCallerFlagsThrough covers the flag the registry
// limitation calls load-bearing: the machine default for tools.approvalMode can
// be yolo, so a governed dispatch has to state its posture explicitly. The
// adapter does not invent one — it must forward what the caller passed, in
// order, ahead of the prompt.
func TestOhMyPiAdapterPassesCallerFlagsThrough(t *testing.T) {
	fakeOhMyPiBinary(t)
	// The oh_my_pi spec reads COMPOUND_OMP_HOOK_PATH at buildCmd time and
	// stamps --hook=<path> on every launch. Set it to a sentinel so the
	// adapter does not refuse with the empty-path error. The sentinel
	// does not need to exist on disk: --hook= is just an argv fragment
	// for this assertion (the refusal-on-missing-file lives in
	// precheckLaunch, not buildCmd, because the bypass launch is the only
	// mode that mandates a loadable shim).
	t.Setenv("COMPOUND_OMP_HOOK_PATH", "/tmp/compound.ts")
	a := NewOhMyPiAdapter(runattempt.Open(t.TempDir()))
	ws := t.TempDir()
	cmd := a.buildCmd(context.Background(), RunInput{
		WorkID:    "work_ohmypi_flags",
		Workspace: ws,
		Args:      []string{"", "--approval-mode", "write", "--max-time", "30m"},
		Prompt:    "do the thing",
		Now:       time.Now(),
	})
	want := []string{"omp", "--mode", "rpc", "--cwd", ws, "--approval-mode=always-ask", "--hook=/tmp/compound.ts", "--approval-mode", "write", "--max-time", "30m", "do the thing"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

// TestOhMyPiAdapterWorkspaceNeverCompoundHome is the named falsifier: the
// adapter MUST NOT stamp COMPOUND_HOME, ~/.compound, or any workspace root the
// run delegate did not authorize.
func TestOhMyPiAdapterWorkspaceNeverCompoundHome(t *testing.T) {
	fakeOhMyPiBinary(t)
	a := NewOhMyPiAdapter(runattempt.Open(t.TempDir()))
	cmd := a.buildCmd(context.Background(), RunInput{
		WorkID:    "work_ohmypi_noworkspace",
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

// TestOhMyPiAdapterDryRunProbe is the readiness proof: format the argv without
// executing it, and confirm the four invariants the verifier asserts:
//
//  1. The binary is omp — no fall-through to prime-agent, codex or claude.
//  2. The mode is `--mode rpc` (default) or `-p --no-session` (oneshot).
//  3. The workspace is the run-delegate-resolved value, and an unset one is
//     visible rather than silently defaulted.
//  4. The prompt is present (no empty-prompt dispatch).
func TestOhMyPiAdapterDryRunProbe(t *testing.T) {
	ws := t.TempDir()
	rpc := FormatOhMyPiDryRunPrompt(RunInput{
		WorkID:    "work_ohmypi_dryrun_rpc",
		Workspace: ws,
		Prompt:    "verify a proof",
		Now:       time.Now(),
	})
	if !strings.HasPrefix(rpc, "omp ") {
		t.Fatalf("rpc summary must name the omp binary: %q", rpc)
	}
	if strings.Contains(rpc, "prime-agent") {
		t.Fatalf("rpc summary leaked the sibling fork's binary: %q", rpc)
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

	oneshot := FormatOhMyPiDryRunPrompt(RunInput{
		WorkID:         "work_ohmypi_dryrun_oneshot",
		Workspace:      ws,
		PermissionMode: "oneshot",
		Prompt:         "verify a proof",
		Now:            time.Now(),
	})
	if !strings.Contains(oneshot, "mode=oneshot") {
		t.Fatalf("oneshot summary missing mode=oneshot: %q", oneshot)
	}

	unresolved := FormatOhMyPiDryRunPrompt(RunInput{
		WorkID: "work_ohmypi_dryrun_unresolved",
		Prompt: "verify a proof",
		Now:    time.Now(),
	})
	if !strings.Contains(unresolved, "(unset)") {
		t.Fatalf("unset workspace must be flagged: %q", unresolved)
	}
}
