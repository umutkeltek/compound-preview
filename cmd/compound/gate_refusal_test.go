package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"compound/internal/envelope"
)

// TestGateRefusalRendersAllFourParts pins the shape: a refusal that drops a part
// degrades back into the one-line "error: ...; pass --flag" it replaced.
func TestGateRefusalRendersAllFourParts(t *testing.T) {
	t.Parallel()
	text := gateRefusal{
		Command:  "work advance --execute",
		Gate:     "workspace_resolution",
		Refused:  "no execution workspace could be resolved",
		Tried:    []string{"--workspace flag: not passed", "current working directory: unavailable (getwd: gone)"},
		DoThis:   []string{"compound work advance work_x --execute --harness claude --workspace <project-path>"},
		Prevents: "running a code-editing harness against COMPOUND_HOME",
	}.String()

	for _, want := range []string{
		"error: work advance --execute refused at gate workspace_resolution",
		"refused: no execution workspace could be resolved",
		"tried in order:",
		"1. --workspace flag: not passed",
		"2. current working directory: unavailable (getwd: gone)",
		"do this:",
		"compound work advance work_x --execute --harness claude --workspace <project-path>",
		"prevents: running a code-editing harness against COMPOUND_HOME",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal text missing %q:\n%s", want, text)
		}
	}

	// The tried-list is ordered and numbered: the reader must be able to see WHICH
	// rung failed, which is the whole reason the trace is threaded out of the resolver.
	if strings.Index(text, "1. --workspace") > strings.Index(text, "2. current working") {
		t.Errorf("tried steps rendered out of attempt order:\n%s", text)
	}
}

// TestGateRefusalKeepsSuggestedCommandsRunnable pins that a missing interpolated
// value becomes a visible placeholder rather than a gap. "compound work show  #
// confirm..." reads as a complete command and silently is not one.
func TestGateRefusalKeepsSuggestedCommandsRunnable(t *testing.T) {
	t.Parallel()
	if got := firstNonEmptyRefusalValue("", "<work-id>"); got != "<work-id>" {
		t.Errorf("empty value must render a placeholder, got %q", got)
	}
	if got := firstNonEmptyRefusalValue("   ", "<work-id>"); got != "<work-id>" {
		t.Errorf("blank value must render a placeholder, got %q", got)
	}
	if got := firstNonEmptyRefusalValue("work_abc", "<work-id>"); got != "work_abc" {
		t.Errorf("present value must survive, got %q", got)
	}
}

// TestGateRefusalSuggestedCommandsAreInvocable pins that every command a refusal
// tells the reader to run is one they can paste.
//
// A refusal's whole value is the exact command, so a suggestion that omits the
// binary name ("harness run --harness x" when the command is "compound harness run")
// is worse than no suggestion: it looks authoritative and fails. Caught in review on
// this file's first draft, which is why the check reads the real sources below
// instead of a list of examples restated here — a hand-maintained list would pass
// while the actual refusal sites drifted.
func TestGateRefusalSuggestedCommandsAreInvocable(t *testing.T) {
	t.Parallel()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	// Escaped quotes are part of a literal, not the end of one: a refusal that suggests
	// `--reason "<why>"` writes them, and an extractor that stopped at the backslash split
	// that one command into fragments and reported the tail as a command missing its binary
	// name. Matching whole Go string literals (then unquoting them) keeps the check on the
	// command the operator actually reads — and stops the check from pressuring authors into
	// unquoted, unrunnable suggestions to keep it green.
	literal := regexp.MustCompile(`"((?:[^"\\]|\\.){4,})"`)
	checked := 0

	for _, path := range sources {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range doThisBlocks(string(body)) {
			for _, m := range literal.FindAllStringSubmatch(block, -1) {
				cmd, unquoteErr := strconv.Unquote(m[0])
				if unquoteErr != nil {
					cmd = m[1]
				}
				// Only the command-shaped literals; a bare flag name or a fragment
				// interpolated by Sprintf is not a suggestion on its own.
				if !strings.Contains(cmd, " ") || strings.HasPrefix(cmd, "#") {
					continue
				}
				checked++
				if !strings.HasPrefix(cmd, "compound ") {
					t.Errorf("%s: suggested command is not invocable as written (missing the binary name): %q", path, cmd)
				}
				if strings.Contains(cmd, "one-of-the-above") || strings.Contains(cmd, "see above") {
					t.Errorf("%s: suggested command defers to prose instead of naming a value: %q", path, cmd)
				}
			}
		}
	}

	// Teeth: if the extraction silently matched nothing, the loop above would pass
	// vacuously and the convention would go unenforced.
	if checked == 0 {
		t.Fatal("found no DoThis command literals to check — the extraction is broken, so this test proves nothing")
	}
	t.Logf("checked %d suggested commands across refusal sites", checked)
}

// doThisBlocks returns the source text of each `DoThis: []string{...}` literal,
// matched by brace depth so a nested Sprintf call does not end the block early.
func doThisBlocks(src string) []string {
	const marker = "DoThis: []string{"
	var out []string
	for idx := strings.Index(src, marker); idx >= 0; idx = strings.Index(src, marker) {
		rest := src[idx+len(marker):]
		depth := 1
		end := -1
		for i, r := range rest {
			switch r {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break
		}
		out = append(out, rest[:end])
		src = rest[end:]
	}
	return out
}

// refusalSuggestedCommands extracts the commands a RENDERED refusal tells the reader to
// run: the indented lines under "do this:", ending at the next section.
//
// It is the rendering-side twin of doThisBlocks above, which reads the SOURCE. Both are
// needed: the source sweep proves every literal is invocable, and this one proves the text
// an operator actually sees still carries a pasteable command after interpolation — a
// Sprintf whose value came back empty produces a source literal that passes and a rendered
// line that does not.
func refusalSuggestedCommands(text string) []string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "  do this:"):
			inBlock = true
		case inBlock && strings.HasPrefix(line, "    "):
			out = append(out, strings.TrimSpace(line))
		case inBlock:
			inBlock = false
		}
	}
	return out
}

// assertTeachingRefusal is the shared per-site falsifier: it asserts that text is a refusal
// rendered through gateRefusal at gate, that all four sections survived rendering, and that
// every command it suggests is invocable as written.
//
// The vacuity guard matters more than the section checks: an assertion loop over zero
// extracted commands passes while proving nothing, which is exactly how a site could be
// reverted to a bare error without any test going red.
func assertTeachingRefusal(t *testing.T, text, gate string, wantFragments ...string) {
	t.Helper()

	if !strings.Contains(text, "refused at gate "+gate) {
		t.Fatalf("refusal does not name gate %q:\n%s", gate, text)
	}
	for _, section := range []string{"  refused: ", "  tried in order:", "  do this:", "  prevents: "} {
		if !strings.Contains(text, section) {
			t.Errorf("gate %s: refusal is missing the %q section:\n%s", gate, strings.TrimSpace(section), text)
		}
	}

	commands := refusalSuggestedCommands(text)
	if len(commands) == 0 {
		t.Fatalf("gate %s: refusal suggests no command — the whole point of the form is the exact next command:\n%s", gate, text)
	}
	for _, cmd := range commands {
		if !strings.HasPrefix(cmd, "compound ") {
			t.Errorf("gate %s: suggested command is not invocable as written (missing the binary name): %q", gate, cmd)
		}
		if strings.Contains(cmd, "  #") {
			// A trailing comment is fine; an EMPTY interpolation before it is not.
			if head := strings.TrimSpace(strings.SplitN(cmd, "  #", 2)[0]); strings.Contains(head, "  ") {
				t.Errorf("gate %s: suggested command has a gap where a value should be: %q", gate, cmd)
			}
		}
	}

	for _, want := range wantFragments {
		if !strings.Contains(text, want) {
			t.Errorf("gate %s: refusal missing %q:\n%s", gate, want, text)
		}
	}
}

// TestConvertedGateSitesStayConverted is the sweep the packet's falsifier asks for: each row
// is a gate that was deliberately converted to the teaching form, and the row goes red the
// moment an edit drops that gate's rendering back to a bare `error: ...` line.
//
// It reads the sources rather than driving each verb because the point is coverage of the
// CONVERSION, not of the behaviour — the behaviour is pinned by the per-site tests named in
// each row's reason. A row here with no matching per-site test would be ceremony.
func TestConvertedGateSitesStayConverted(t *testing.T) {
	t.Parallel()
	converted := []struct {
		file string
		gate string
		why  string
	}{
		{"harness_exec_cli.go", "adapter_resolution", "unknown --harness, ungoverned run record"},
		{"harness_exec_cli.go", "workspace_resolution", "no workspace, never fall back to the home"},
		{"work_advance_execute.go", "adapter_resolution", "unknown --harness before any state change"},
		{"work_advance_execute.go", "workspace_resolution", "no workspace before any state change"},
		{"act_cli.go", "service_entity_resolution", "governed infra write at an unknown service"},
		{"act_cli.go", "policy_decision", "no standing grant allows the outbound write"},
		{"../../internal/work/landing_resource_cli.go", "land_lease_contention", "a live lease is held by another lane"},
		{"../../internal/work/landing_resource_cli.go", "land_lease_holder", "release/steal by a non-holder"},
		{"main.go", "finding_body_bound", "a body larger than the direct-write bound"},
		{"work_lease_refusal.go", "work_lease_holder", "claim/close against another actor's lease"},
	}

	for _, row := range converted {
		body, err := os.ReadFile(row.file)
		if err != nil {
			t.Fatalf("%s: %v", row.file, err)
		}
		if !strings.Contains(string(body), `Gate:    "`+row.gate+`"`) && !strings.Contains(string(body), `Gate: "`+row.gate+`"`) {
			t.Errorf("%s no longer renders gate %q through gateRefusal (%s) — a converted gate was reverted to a bare error", row.file, row.gate, row.why)
		}
	}
}

// TestResolveExecuteWorkspaceReportsWhichRungResolved pins the trace the refusal
// quotes. A ladder that reports only its final verdict cannot be distinguished from
// one that never ran, so each rung must name itself AND its observed result.
func TestResolveExecuteWorkspaceReportsWhichRungResolved(t *testing.T) {
	t.Parallel()
	t.Run("explicit flag wins and is traced", func(t *testing.T) {
		ws, tried := resolveExecuteWorkspace([]string{"--workspace", "/repo/project"}, nil)
		if ws != "/repo/project" {
			t.Fatalf("workspace = %q, want /repo/project", ws)
		}
		if len(tried) != 1 || !strings.Contains(tried[0], "/repo/project") {
			t.Fatalf("expected the flag rung traced with its value, got %v", tried)
		}
	})

	t.Run("git delivery rung records why the flag rung was skipped", func(t *testing.T) {
		gd := &envelope.GitDeliveryContract{TargetWorktreeRoot: "/repo/worktree"}
		ws, tried := resolveExecuteWorkspace(nil, gd)
		if ws != "/repo/worktree" {
			t.Fatalf("workspace = %q, want /repo/worktree", ws)
		}
		if len(tried) != 2 {
			t.Fatalf("expected both rungs traced, got %v", tried)
		}
		if !strings.Contains(tried[0], "not passed") {
			t.Fatalf("first rung must record that --workspace was absent, got %q", tried[0])
		}
	})

	t.Run("empty target root is distinguished from an absent contract", func(t *testing.T) {
		_, absent := resolveExecuteWorkspace(nil, nil)
		_, empty := resolveExecuteWorkspace(nil, &envelope.GitDeliveryContract{})
		if !strings.Contains(absent[1], "no git delivery contract") {
			t.Errorf("absent contract must say so, got %q", absent[1])
		}
		if !strings.Contains(empty[1], "target_worktree_root is empty") {
			t.Errorf("present-but-empty contract must be distinguished from an absent one, got %q", empty[1])
		}
		// The two cases need different remedies (attach a contract vs fix its field),
		// so collapsing them into one message sends the reader to the wrong fix.
		if absent[1] == empty[1] {
			t.Errorf("absent and empty contracts rendered the same trace line: %q", absent[1])
		}
	})
}
