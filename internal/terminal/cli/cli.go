//go:build darwin

// Package cli is the `compound terminal` operator surface. It is the
// cmd-budget-compliant home of the terminal CLI (the monolith stays a
// dispatch layer; the surface lives in the organ). It implements the
// registry's readCommand contract via Run(args) and carries the flag
// helpers it needs locally, so cmd/compound needs no terminal shims.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"compound/internal/terminal"
	// Blank imports trigger each subpackage's init(), which calls
	// terminal.Register(New()) so the default registry is populated before
	// any caller invokes terminal.NewRegistry(). Adding a new terminal =
	// one new subpackage + one new blank-import line here.
	_ "compound/internal/terminal/cmux"
	_ "compound/internal/terminal/iterm"
	_ "compound/internal/terminal/terminalapp"
	_ "compound/internal/terminal/warp"
)

// ─────────────────────────────────────────────────────────────────────────────
// commandRegistry entry point (wired in command_registry.go: "terminal" key)

// Run is the organ entry point behind `compound terminal`, matching the
// registry's readCommand(func([]string)) contract. It exits with the
// dispatcher's code on non-zero, mirroring the browser/schedule organs.
func Run(args []string) {
	if code := runTerminal(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// runTerminal is the testable dispatcher. Mirrors runActivity's shape at
// cmd/compound/activity_cli.go:72.
//
//	args layout:
//
//	  compound terminal                                  -> list names
//	  compound terminal <name> doctor [--json]
//	  compound terminal <name> wire    [--json]
//	  compound terminal <name> unwire  [--json]
//	  compound terminal <name> hooks   [--json]
//	  compound terminal <name> workspace <verb> [args…] [--json]
//	  compound terminal <name> feed     <verb> [args…] [--json]
//
// Exit codes: 0 success, 1 error, 2 usage / unknown verb / unknown flag.
func runTerminal(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || cliHasUsageFlag(args) {
		fmt.Fprintln(stderr, terminalUsage())
		return 2
	}

	name := args[0]
	if name == "" {
		fmt.Fprintln(stderr, terminalUsage())
		return 2
	}

	// `compound terminal` (single arg, name OR flag) → list names. Pass the
	// full args slice so `--json` reaches listTerminalNames as a flag.
	if len(args) == 1 {
		return listTerminalNames(args, stdout, stderr)
	}

	reg := terminal.NewRegistry()
	t, ok := reg.Get(name)
	if !ok {
		fmt.Fprintf(stderr, "unknown terminal %q. known: %s\n", name, strings.Join(reg.Names(), ", "))
		return 2
	}

	// args[1] is the verb (doctor, wire, …).
	switch args[1] {
	case "doctor":
		return runTerminalDoctor(t, args[2:], stdout, stderr)
	case "wire":
		return runTerminalWire(t, args[2:], stdout, stderr)
	case "unwire":
		return runTerminalUnwire(t, args[2:], stdout, stderr)
	case "hooks":
		return runTerminalHooks(t, args[2:], stdout, stderr)
	case "workspace":
		return runTerminalWorkspace(t, args[2:], stdout, stderr)
	case "feed":
		return runTerminalFeed(t, args[2:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown verb %q for terminal %q\n%s\n", args[1], name, terminalUsage())
		return 2
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// sub-dispatchers

// listTerminalNames prints the registered terminal names in alphabetical
// order. Read-only (Read admission).
func listTerminalNames(args []string, stdout, stderr io.Writer) int {
	if bad, err := cliEnforceKnownFlags(args, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, terminalUsage())
		_ = bad
		return 2
	}
	reg := terminal.NewRegistry()
	out := map[string]any{
		"terminals": reg.Names(),
		"count":     len(reg.Names()),
	}
	if cliHasFlag(args, "--json") {
		return cliWriteJSON(stdout, stderr, out)
	}
	for _, n := range reg.Names() {
		fmt.Fprintln(stdout, n)
	}
	return 0
}

// runTerminalDoctor prints the DoctorReport. Read-only.
func runTerminalDoctor(t terminal.Terminal, args []string, stdout, stderr io.Writer) int {
	if bad, err := cliEnforceKnownFlags(args, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, terminalUsage())
		_ = bad
		return 2
	}
	rep, err := t.Doctor(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cliHasFlag(args, "--json") {
		return cliWriteJSON(stdout, stderr, rep)
	}
	fmt.Fprintf(stdout, "%s installed=%v ipc=%v hooks_wired=%d bridge=%v\n",
		t.Name(), rep.Installed, rep.IPCReachable, rep.HooksWired, rep.BridgeOK)
	for _, issue := range rep.Issues {
		fmt.Fprintf(stdout, "  - %s\n", issue)
	}
	return 0
}

// runTerminalWire prints the install snippet. Read-only — the snippet is
// shown to the operator for review; actual execution would be a separate
// gated verb (deferred to a follow-up PR; today the operator copies it).
func runTerminalWire(t terminal.Terminal, args []string, stdout, stderr io.Writer) int {
	if bad, err := cliEnforceKnownFlags(args, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, terminalUsage())
		_ = bad
		return 2
	}
	hs := t.Hooks()
	out := map[string]any{
		"terminal": t.Name(),
		"install":  hs.Install,
		"paths":    hs.Paths,
	}
	if cliHasFlag(args, "--json") {
		return cliWriteJSON(stdout, stderr, out)
	}
	fmt.Fprintf(stdout, "# %s install snippet\n%s\n", t.Name(), hs.Install)
	return 0
}

// runTerminalUnwire prints the uninstall snippet.
func runTerminalUnwire(t terminal.Terminal, args []string, stdout, stderr io.Writer) int {
	if bad, err := cliEnforceKnownFlags(args, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, terminalUsage())
		_ = bad
		return 2
	}
	hs := t.Hooks()
	if cliHasFlag(args, "--json") {
		return cliWriteJSON(stdout, stderr, map[string]any{
			"terminal":  t.Name(),
			"uninstall": hs.Uninstall,
		})
	}
	fmt.Fprintf(stdout, "# %s uninstall snippet\n%s\n", t.Name(), hs.Uninstall)
	return 0
}

// runTerminalHooks prints the HookSet as JSON or human-readable.
func runTerminalHooks(t terminal.Terminal, args []string, stdout, stderr io.Writer) int {
	if bad, err := cliEnforceKnownFlags(args, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, terminalUsage())
		_ = bad
		return 2
	}
	hs := t.Hooks()
	if cliHasFlag(args, "--json") {
		return cliWriteJSON(stdout, stderr, hs)
	}
	fmt.Fprintf(stdout, "terminal: %s\n", t.Name())
	for _, p := range hs.Paths {
		fmt.Fprintf(stdout, "  hook: %s\n", p)
	}
	return 0
}

// runTerminalWorkspace dispatches the workspace verbs.
func runTerminalWorkspace(t terminal.Terminal, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || cliHasUsageFlag(args) {
		fmt.Fprintln(stderr, terminalWorkspaceUsage())
		return 2
	}
	known := map[string]bool{"--json": true}
	if bad, err := cliEnforceKnownFlags(args, known); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, terminalWorkspaceUsage())
		_ = bad
		return 2
	}
	api := t.Workspace()
	ctx := context.Background()
	switch args[0] {
	case "select":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "workspace select requires <ref>")
			return 2
		}
		if err := api.Select(ctx, terminal.WorkspaceRef{Title: args[1]}); err != nil {
			fmt.Fprintln(stderr, "workspace select:", err)
			return 1
		}
	case "color":
		if len(args) < 3 {
			fmt.Fprintln(stderr, "workspace color requires <ref> <hex>")
			return 2
		}
		if err := api.SetColor(ctx, terminal.WorkspaceRef{Title: args[1]}, args[2]); err != nil {
			fmt.Fprintln(stderr, "workspace color:", err)
			return 1
		}
	case "icon":
		if len(args) < 3 {
			fmt.Fprintln(stderr, "workspace icon requires <ref> <glyph>")
			return 2
		}
		if err := api.SetIcon(ctx, terminal.WorkspaceRef{Title: args[1]}, args[2]); err != nil {
			fmt.Fprintln(stderr, "workspace icon:", err)
			return 1
		}
	default:
		fmt.Fprintf(stderr, "unknown workspace verb %q\n%s\n", args[0], terminalWorkspaceUsage())
		return 2
	}
	if cliHasFlag(args, "--json") {
		return cliWriteJSON(stdout, stderr, map[string]any{"ok": true, "verb": args[0]})
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}

// runTerminalFeed dispatches the feed verbs.
func runTerminalFeed(t terminal.Terminal, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || cliHasUsageFlag(args) {
		fmt.Fprintln(stderr, terminalFeedUsage())
		return 2
	}
	known := map[string]bool{
		"--body":   true,
		"--source": true,
		"--kind":   true,
		"--json":   true,
	}
	if bad, err := cliEnforceKnownFlags(args, known); err != nil {
		fmt.Fprintf(stderr, "%s\n%s\n", err, terminalFeedUsage())
		_ = bad
		return 2
	}
	api := t.Feed()
	ctx := context.Background()
	switch args[0] {
	case "push":
		body := strings.TrimSpace(cliValueFlag(args, "--body"))
		if body == "" {
			fmt.Fprintln(stderr, "feed push requires --body <text>")
			return 2
		}
		opts := terminal.FeedPushOpts{
			Source: strings.TrimSpace(cliValueFlag(args, "--source")),
			Kind:   strings.TrimSpace(cliValueFlag(args, "--kind")),
		}
		if err := api.Push(ctx, body, opts); err != nil {
			fmt.Fprintln(stderr, "feed push:", err)
			return 1
		}
	case "status":
		if len(args) < 3 {
			fmt.Fprintln(stderr, "feed status requires <key> <value>")
			return 2
		}
		if err := api.SetStatus(ctx, args[1], args[2]); err != nil {
			fmt.Fprintln(stderr, "feed status:", err)
			return 1
		}
	default:
		fmt.Fprintf(stderr, "unknown feed verb %q\n%s\n", args[0], terminalFeedUsage())
		return 2
	}
	if cliHasFlag(args, "--json") {
		return cliWriteJSON(stdout, stderr, map[string]any{"ok": true, "verb": args[0]})
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// usage strings

func terminalUsage() string {
	return "usage:\n" +
		"  compound terminal                                  list registered terminals (--json)\n" +
		"  compound terminal <name> doctor [--json]           health report for <name>\n" +
		"  compound terminal <name> wire    [--json]          print the install snippet for <name>'s bridge hooks\n" +
		"  compound terminal <name> unwire  [--json]          print the uninstall snippet\n" +
		"  compound terminal <name> hooks   [--json]          print the hook paths\n" +
		"  compound terminal <name> workspace select <ref> [--json]\n" +
		"  compound terminal <name> workspace color  <ref> <hex> [--json]\n" +
		"  compound terminal <name> workspace icon   <ref> <glyph> [--json]\n" +
		"  compound terminal <name> feed     push   --body <text> [--source S] [--kind K] [--json]\n" +
		"  compound terminal <name> feed     status <key> <value> [--json]\n" +
		"\n" +
		"known terminals: cmux iterm2 warp terminal-app\n"
}

func terminalWorkspaceUsage() string {
	return "usage:\n" +
		"  compound terminal <name> workspace select <ref>         [--json]\n" +
		"  compound terminal <name> workspace color  <ref> <hex>    [--json]\n" +
		"  compound terminal <name> workspace icon   <ref> <glyph>  [--json]\n"
}

func terminalFeedUsage() string {
	return "usage:\n" +
		"  compound terminal <name> feed push   --body <text> [--source S] [--kind K] [--json]\n" +
		"  compound terminal <name> feed status <key> <value> [--json]\n"
}

// ─────────────────────────────────────────────────────────────────────────────
// local flag/json helpers
//
// These are small, self-contained copies of the cmd/compound package-scope
// helpers (cliEnforceKnownFlags, cliWriteJSON, cliHasFlag, cliHasUsageFlag). The organ
// cannot import cmd/compound (that would cycle the dispatch layer back into
// itself), so it carries its own — the same pattern schedule/browser use.

func cliHasUsageFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func cliHasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

// cliEnforceKnownFlags returns the first unknown flag (or "" if all args are
// known). It tolerates values: a "--name=value" form and a standalone
// "--name" followed by a non-flag token are both accepted for a known name.
func cliEnforceKnownFlags(args []string, known map[string]bool) (string, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name := arg
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			name = name[:idx]
		}
		if !known[name] {
			return name, fmt.Errorf("unknown flag %q", name)
		}
	}
	return "", nil
}

func cliWriteJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func cliValueFlag(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
