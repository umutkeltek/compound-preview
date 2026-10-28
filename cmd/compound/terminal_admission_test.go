package main

import (
	"testing"

	"compound/internal/policy"
)

// `compound terminal` is registered read (doctor/hooks/wire/unwire were the
// first four subcommands to ship). wire/unwire are mutating IPC operations
// against the host terminal, so they were declared governed from day one.
//
// But terminal also grew `workspace select | color | icon` and
// `feed push | status` verbs — both call into the terminal's IPC bridge to
// mutate workspace state and push events. Without explicit subcommand
// admission, those verbs inherit the base `read` row and would slip past
// the classifier the same way `presence announce` did (see
// presence_announce_admission_test.go). This test pins the admission rows
// on the runtime registry so a future refactor cannot silently re-categorise
// a mutating verb as read.
//
// Note: this is a structural check against `compoundCommandRegistry()` and
// the catalog built from it. `policy.ClassifyToolAction` only does ONE level
// of subcommand lookup (command → subcommand), but terminal's user-facing
// shape is `compound terminal <name> <verb>` where `<name>` is a positional
// terminal-name token that occupies the classifier's `subcommand` slot. So
// the catalog classifier can never route the terminal mutating verbs on its
// own — the admission row IS the contract, and pinning it on the registry is
// the only check that survives the shape mismatch.
func TestTerminalSubcommandAdmissionMatchesMutationSurface(t *testing.T) {
	t.Parallel()

	terminal, ok := compoundCommandRegistry()["terminal"]
	if !ok {
		t.Fatal("terminal command missing from runtime registry")
	}
	if terminal.Admission != policy.FirstPartyCommandRead {
		t.Fatalf("terminal base admission = %q, want %q",
			terminal.Admission, policy.FirstPartyCommandRead)
	}

	wantGoverned := map[string]bool{
		"wire":      true,
		"unwire":    true,
		"workspace": true,
		"feed":      true,
	}
	wantRead := map[string]bool{
		"doctor": true,
		"hooks":  true,
	}

	for verb := range wantGoverned {
		got, ok := terminal.SubcommandAdmission[verb]
		if !ok {
			t.Errorf("terminal %q admission row missing; mutating verb would inherit base read", verb)
			continue
		}
		if got != policy.FirstPartyCommandGoverned {
			t.Errorf("terminal %q admission = %q, want %q (mutating verb)",
				verb, got, policy.FirstPartyCommandGoverned)
		}
	}
	for verb := range wantRead {
		got, ok := terminal.SubcommandAdmission[verb]
		if !ok {
			t.Errorf("terminal %q admission row missing", verb)
			continue
		}
		if got != policy.FirstPartyCommandRead {
			t.Errorf("terminal %q admission = %q, want %q (read verb)",
				verb, got, policy.FirstPartyCommandRead)
		}
	}

	// Round-trip through the catalog so we catch drift between the
	// runtime registry and the policy catalog that ClassifyToolAction
	// consults. (Mirrors the assertions in
	// command_registry_admission_test.go:TestFirstPartyCommandCatalogMatchesEveryRuntimeCommand
	// but scoped to terminal, so this test stays legible in isolation.)
	catalog := firstPartyCommandCatalog()
	for verb, want := range map[string]policy.FirstPartyCommandAdmission{
		"workspace": policy.FirstPartyCommandGoverned,
		"feed":      policy.FirstPartyCommandGoverned,
		"doctor":    policy.FirstPartyCommandRead,
		"hooks":     policy.FirstPartyCommandRead,
	} {
		got, ok := catalog.LookupSubcommand("terminal", verb)
		if !ok {
			t.Errorf("catalog missing terminal %q admission row", verb)
			continue
		}
		if got != want {
			t.Errorf("catalog terminal %q admission = %q, want %q", verb, got, want)
		}
	}
}
