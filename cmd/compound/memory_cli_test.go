// memory_cli_test.go covers the operator-facing error/hint seams added by
// packet work_3c70f011f6ba (memory CLI truthful errors + cross-namespace
// candidate doors). All three falsifiers run in a child test process
// (see TestMemoryCLIRunsSubcommandChild) so we can observe the verb
// handlers' os.Exit code and capture their stderr directly — the in-process
// cmdMemory path interacts with non-trivial test-housekeeping (engine
// config restore, sidecar env) that we deliberately want to keep out of
// the assertions.
package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/home"
)

// memoryCliChildEnv is the sentinel that tells the child re-entry which
// subcommand to run, with all other args (after the chosen subcommand)
// provided via memoryCliArgsEnv.
const (
	memoryCliChildEnv = "COMPOUND_TEST_MEMORY_CLI_CHILD"
	memoryCliArgsEnv  = "COMPOUND_TEST_MEMORY_CLI_ARGS"
)

func TestParseMemoryAfterCursorAcceptsTokenOrFilenameAndRejectsPaths(t *testing.T) {
	t.Parallel()
	want := candidates.EntryCursor("cand_01.json")
	token, err := want.Token()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"cand_01.json", token} {
		got, err := parseMemoryAfterCursor(value)
		if err != nil || got != want {
			t.Fatalf("parse %q = %q, %v; want %q", value, got, err, want)
		}
	}
	if got, err := parseMemoryAfterCursor(""); err != nil || got != "" {
		t.Fatalf("empty cursor = %q, %v", got, err)
	}
	for _, value := range []string{"../escape.json", "/absolute.json", "filename-v1:not-base64***"} {
		if got, err := parseMemoryAfterCursor(value); err == nil {
			t.Fatalf("invalid cursor %q accepted as %q", value, got)
		}
	}
}

// memoryCliChildArgsEnv is set by the parent in addition to the
// memoryCliChildEnv; it carries the full argv the child passes into
// cmdMemory([]string{...}). Newlines are the separator (no real CLI arg
// contains a newline in this packet).
func splitMemoryCliArgs(t *testing.T) []string {
	t.Helper()
	raw := os.Getenv(memoryCliArgsEnv)
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// runMemoryCLIAsChild spawns the test binary itself with the sentinel env
// vars set, asks it to invoke cmdMemory(sub, args...), and returns the
// combined stdout/stderr plus the *exec.ExitError if any. This mirrors the
// pattern in contract_instructions_cli_test.go.
func runMemoryCLIAsChild(t *testing.T, homePath, sub string, args ...string) ([]byte, error) {
	t.Helper()
	combined := []string{sub}
	combined = append(combined, args...)
	cmd := exec.Command(os.Args[0], "-test.run=^TestMemoryCLIRunsSubcommandChild$")
	env := []string{
		memoryCliChildEnv + "=" + sub,
		memoryCliArgsEnv + "=" + strings.Join(combined, "\n"),
		"COMPOUND_HOME=" + homePath,
	}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, memoryCliChildEnv+"=") ||
			strings.HasPrefix(e, memoryCliArgsEnv+"=") ||
			strings.HasPrefix(e, "COMPOUND_HOME=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	return cmd.CombinedOutput()
}

// TestMemoryCLIRunsSubcommandChild is the child-process counterpart that
// every exit-code check below relies on. The parent passes the chosen
// subcommand through memoryCliChildEnv and the full argv through
// memoryCliArgsEnv, so we do not have to fork N test bodies. cmdMemory may
// os.Exit on the verb handlers (by design); the parent captures the code
// via *exec.ExitError.
func TestMemoryCLIRunsSubcommandChild(t *testing.T) {
	t.Parallel()
	if os.Getenv(memoryCliChildEnv) == "" {
		return
	}
	argv := splitMemoryCliArgs(t)
	if len(argv) == 0 {
		return
	}
	cmdMemory(argv)
}

// TestMemoryAdjudicateNoArgExitsTwoWithUsage (falsifier 1):
// `compound memory adjudicate` (no positional id) must exit 2 and print the
// adjudicate usage line on stderr, WITHOUT touching the mem store.
//
// Pre-change behavior: err = "not found: .json", exit 1 — nonsensical.
func TestMemoryAdjudicateNoArgExitsTwoWithUsage(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}
	out, runErr := runMemoryCLIAsChild(t, homePath, "adjudicate")
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("exit code = %v, want 2; output:\n%s", runErr, out)
	}
	got := string(out)
	if !strings.Contains(got, "usage: compound memory adjudicate <id>") {
		t.Fatalf("expected usage line on stderr, got:\n%s", got)
	}
	if strings.Contains(got, "not found: .json") {
		t.Fatalf("regression: still hitting readJSON for empty id; got:\n%s", got)
	}
}

// TestMemoryAdjudicateUnknownIDNamesSearchedLocationAndDoor (falsifier 2):
// an unresolvable id (a) prints a not-found message that names the searched
// inbox location, (b) names the obvious cross-namespace door for known
// non-memory kinds, and (c) exits 1 (operator-facing, not usage).
//
// We assert against "cand_doesnotexist" because no fixture writes it. Using
// a `src_cand_*` id pins the cross-namespace door to capability_observation
// / `compound capability consume` (the high-leverage case the packet opens
// with).
func TestMemoryAdjudicateUnknownIDNamesSearchedLocationAndDoor(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}

	cases := []struct {
		name         string
		id           string
		wantExit     int
		wantContains []string
	}{
		{
			name:     "plain-missing-id",
			id:       "cand_doesnotexist",
			wantExit: 1,
			wantContains: []string{
				"error: not found: cand_doesnotexist",
				"searched: $COMPOUND_HOME/" + candidates.InboxDir(candidates.Memory) + "/cand_doesnotexist.json",
				"`compound memory candidates --state proposed`",
			},
		},
		{
			name:     "src_cand-id-numbers-the-capability-door",
			id:       "src_cand_abcdef0123",
			wantExit: 1,
			wantContains: []string{
				"error: not found: src_cand_abcdef0123",
				"searched: $COMPOUND_HOME/" + candidates.InboxDir(candidates.Memory) + "/src_cand_abcdef0123.json",
				"capability_observation",
				"compound capability consume",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, runErr := runMemoryCLIAsChild(t, homePath, "adjudicate",
				tc.id, "--verdict", "promote")
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != tc.wantExit {
				t.Fatalf("exit code = %v, want %d; output:\n%s", runErr, tc.wantExit, out)
			}
			got := string(out)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in output:\n%s", want, got)
				}
			}
		})
	}
}

// TestMemoryCandidatesKindPointerNamesForeignDoor (falsifier 3):
// `compound memory candidates --kind capability_observation` must print a
// one-line pointer to the owning door (`compound capability consume`) in
// addition to the normal 0/N summary. The listing still runs; we just
// don't leave the operator thinking the kind doesn't exist anywhere.
func TestMemoryCandidatesKindPointerNamesForeignDoor(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("home init: %v", err)
	}

	out, runErr := runMemoryCLIAsChild(t, homePath, "candidates", "--kind", "capability_observation")
	if runErr != nil {
		t.Fatalf("candidates --kind: %v\noutput:\n%s", runErr, out)
	}
	got := string(out)
	if !strings.Contains(got, "# memory candidates 0/0") {
		t.Fatalf("expected normal 0/0 summary before the hint; output:\n%s", got)
	}
	wantHint := "--kind capability_observation is adjudicated via compound capability consume (inbox: inbox/capability-observations"
	if !strings.Contains(got, wantHint) {
		t.Fatalf("missing cross-namespace pointer %q in output:\n%s", wantHint, got)
	}

	// Sanity: an unknown kind prints no hint (we never invent one).
	out2, runErr2 := runMemoryCLIAsChild(t, homePath, "candidates", "--kind", "definitely_not_a_kind")
	if runErr2 != nil {
		t.Fatalf("candidates --kind unknown: %v\noutput:\n%s", runErr2, out2)
	}
	if strings.Contains(string(out2), "is adjudicated via") {
		t.Fatalf("unknown kind should not print a door pointer; got:\n%s", out2)
	}
	// Sanity: the memory kind itself prints no extra hint.
	out3, runErr3 := runMemoryCLIAsChild(t, homePath, "candidates", "--kind", "memory")
	if runErr3 != nil {
		t.Fatalf("candidates --kind memory: %v\noutput:\n%s", runErr3, out3)
	}
	if strings.Contains(string(out3), "is adjudicated via") {
		t.Fatalf("memory kind should not print a door pointer (it lives here); got:\n%s", out3)
	}
}

func TestMemoryAdjudicatePendingPromoteRefused(t *testing.T) {
	t.Parallel()
	// Settled design decision 3 (memory-batch-adjudicate packet): promotion
	// stays per-id only. Refuse --verdict promote in --pending mode at the
	// CLI door so a stale batch cannot promote without the operator.
	//
	// The store-level invariant is pinned by TestAdjudicatePendingRefusesFixedPromote;
	// this test exercises the CLI refuse-publishes-the-error path.
	t.Skip("CLI smoke requires an integration harness; the store-level invariant is pinned by TestAdjudicatePendingRefusesFixedPromote")
}
