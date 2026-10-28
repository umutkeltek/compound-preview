package main

import (
	"testing"

	"compound/internal/policy"
)

func TestFirstPartyCommandAdmissionComesFromCLIRuntimeRegistry(t *testing.T) {
	t.Parallel()

	catalog := firstPartyCommandCatalog()
	request := policy.ToolActionRequest{
		Tool:               "Bash",
		Arguments:          map[string]any{"command": "compound work delegate work_example --harness codex --dry-run"},
		FirstPartyCommands: catalog,
	}

	got := policy.ClassifyToolAction(request)
	if !got.Known || got.Operation != "governed_command" || got.MutationClass != "local_reversible" {
		t.Fatalf("registered work command must enter the governed first-party lane: %+v", got)
	}

	withoutWork := catalog.Without("work")
	request.FirstPartyCommands = withoutWork
	got = policy.ClassifyToolAction(request)
	if got.Known || got.MutationClass != "security" {
		t.Fatalf("removing the CLI registry row must remove admission automatically: %+v", got)
	}
}

func TestFirstPartyCommandAdmissionStillFailsClosedOutsideRegistry(t *testing.T) {
	t.Parallel()

	request := policy.ToolActionRequest{
		Tool:               "Bash",
		Arguments:          map[string]any{"command": "compound definitely-not-a-command --confirm"},
		FirstPartyCommands: firstPartyCommandCatalog(),
	}
	got := policy.ClassifyToolAction(request)
	if got.Known || got.MutationClass != "security" {
		t.Fatalf("unknown first-party command escaped the security floor: %+v", got)
	}
}

func TestFirstPartyCommandAdmissionPreservesNestedInstallSecurityFloor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		command       string
		wantOperation string
		wantSink      string
	}{
		{
			command:       "compound harness posture-plan --harness codex --json",
			wantOperation: "local_command",
			wantSink:      "shell",
		},
		{
			command:       "compound hook install --harness codex --scope global",
			wantOperation: "install_command",
			wantSink:      "install",
		},
		{
			command:       "compound hook install --harness codex --check=false",
			wantOperation: "install_command",
			wantSink:      "install",
		},
		{
			command:       "compound hook install --harness codex --scope global --check --json",
			wantOperation: "local_command",
			wantSink:      "shell",
		},
	} {
		got := policy.ClassifyToolAction(policy.ToolActionRequest{
			Tool:               "Bash",
			Arguments:          map[string]any{"command": tc.command},
			FirstPartyCommands: firstPartyCommandCatalog(),
		})
		if !got.Known || got.Operation != tc.wantOperation || got.RequiredSink != tc.wantSink {
			t.Fatalf("%q classification = %+v, want operation=%q sink=%q", tc.command, got, tc.wantOperation, tc.wantSink)
		}
	}
}

func TestFirstPartyCommandAdmissionDistinguishesMutatingSubcommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		command       string
		wantOperation string
	}{
		{
			command:       "compound orient --harness codex --task release-audit",
			wantOperation: "governed_command",
		},
		{
			command:       "compound route import route-evidence.json",
			wantOperation: "governed_command",
		},
		{
			command:       "compound route evidence-authority --json",
			wantOperation: "local_command",
		},
		{
			command:       "compound route experiment-plan --profiles-root profiles --json",
			wantOperation: "local_command",
		},
		{
			command:       "compound coordination --json",
			wantOperation: "local_command",
		},
		{
			command:       "compound coordination measure --report drep_example",
			wantOperation: "governed_command",
		},
		{
			command:       "compound profiles check --installed-only",
			wantOperation: "local_command",
		},
		{
			command:       "compound profiles generate",
			wantOperation: "governed_command",
		},
		{
			command:       "compound home doctor --home /synthetic/home",
			wantOperation: "local_command",
		},
		{command: "compound browser route --json", wantOperation: "local_command"},
		{command: "compound browser settings --enable --work work_example", wantOperation: "governed_command"},
		{command: "compound browser provider-pricing --work work_example --model jev-1.13.0 --max-usd-per-request 0.003", wantOperation: "governed_command"},
		{command: "compound browser credential-set --work work_example --confirm", wantOperation: "governed_command"},
		{command: "compound browser credential-rotate --work work_example --confirm", wantOperation: "governed_command"},
		{command: "compound browser goal --work work_example --url https://example.com/ --goal read --expect-text Example", wantOperation: "governed_command"},
		{
			command:       "compound home where --home /synthetic/home",
			wantOperation: "local_command",
		},
		{
			command:       "compound home init --home /synthetic/home --global",
			wantOperation: "governed_command",
		},
		{
			command:       "compound home private-custody repair --home /synthetic/home",
			wantOperation: "governed_command",
		},
		{
			command:       "compound runtime doctor fixtures/runtime/synthetic-service.yaml",
			wantOperation: "governed_command",
		},
		{
			command:       "compound architecture doctor --strict",
			wantOperation: "local_command",
		},
		{
			command:       "compound contract hooks-authority --observe-live",
			wantOperation: "local_command",
		},
		{
			command:       "compound contract compile --harness codex --task hook-health",
			wantOperation: "governed_command",
		},
		{
			command:       "compound contract import-hooks --codex-hooks-json hooks.json",
			wantOperation: "governed_command",
		},
		{
			command:       "compound lanebeliefs fold",
			wantOperation: "local_command",
		},
		{
			command:       "compound lanebeliefs fold --dry-run",
			wantOperation: "local_command",
		},
		{
			command:       "compound lanebeliefs fold --experimental",
			wantOperation: "governed_command",
		},
		{
			command:       "compound lanebeliefs promote",
			wantOperation: "local_command",
		},
		{
			command:       "compound lanebeliefs promote --experimental",
			wantOperation: "governed_command",
		},
		{
			command:       "compound lanebeliefs cool",
			wantOperation: "local_command",
		},
		{
			command:       "compound lanebeliefs cool --experimental",
			wantOperation: "governed_command",
		},
		{
			command:       "compound lanebeliefs list",
			wantOperation: "local_command",
		},
	}

	for _, tc := range tests {
		t.Run(tc.command, func(t *testing.T) {
			got := policy.ClassifyToolAction(policy.ToolActionRequest{
				Tool:               "Bash",
				Arguments:          map[string]any{"command": tc.command},
				FirstPartyCommands: firstPartyCommandCatalog(),
			})
			if !got.Known || got.Operation != tc.wantOperation {
				t.Fatalf("%q classification = %+v, want operation=%q", tc.command, got, tc.wantOperation)
			}
		})
	}
}

func TestFirstPartyCommandAdmissionKeepsDoctorMutationNeighborsHeld(t *testing.T) {
	t.Parallel()
	commands := []string{
		"compound home backup --home /synthetic/home --target /synthetic/archive",
		"compound home restore --archive /synthetic/archive --target /synthetic/home",
		"compound home compact --home /synthetic/home",
		"compound runtime repair-plan fixtures/runtime/synthetic-service.yaml",
		"compound runtime repair-review review.yaml",
		"compound architecture publish --public",
		"compound contract definitely-not-a-command",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			got := policy.ClassifyToolAction(policy.ToolActionRequest{
				Tool:               "Bash",
				Arguments:          map[string]any{"command": command},
				FirstPartyCommands: firstPartyCommandCatalog(),
			})
			if got.Known || got.MutationClass != "security" {
				t.Fatalf("%q escaped security-floor hold: %+v", command, got)
			}
		})
	}
}

func TestRunAttemptAdmissionSeparatesReadOnlyListFromMutation(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		"compound run-attempt list",
		"compound run-attempt list --non-terminal --json",
		"compound run-attempt list --home /tmp/compound-read-fixture",
		"compound run-attempt list >&2",
		"compound run-attempt list >&-",
		"compound run-attempt list 2>&1",
	} {
		got := policy.ClassifyToolAction(policy.ToolActionRequest{
			Tool:               "Bash",
			Arguments:          map[string]any{"command": command},
			FirstPartyCommands: firstPartyCommandCatalog(),
		})
		if !got.Known || got.Operation != "local_command" || got.MutationClass != "local_reversible" {
			t.Fatalf("%q classification = %+v, want known automatic local command", command, got)
		}
	}

	for _, command := range []string{
		"compound run-attempt",
		"compound run-attempt sweep --threshold 30m",
		"compound run-attempt create",
		"compound run-attempt start",
		"compound run-attempt finish",
		"compound run-attempt heartbeat",
		"compound run-attempt link-predecessor",
		"compound run-attempt show ratt_20260729T000000Z_00000000 --json",
		"compound run-attempt definitely-not-a-subcommand",
		"/tmp/attacker/compound run-attempt list",
		"compound run-attempt list >&/tmp/compound-classifier-output",
		"compound run-attempt list >>&/tmp/compound-classifier-output",
		"compound run-attempt list >>&2",
		"compound run-attempt list >>&-",
		"compound run-attempt list 2>>&1",
		"compound run-attempt list $(touch /tmp/compound-classifier-must-not-run)",
		`compound run-attempt "$RUN_ATTEMPT_SUBCOMMAND"`,
		"compound run-attempt $(printf list)",
		"compound run-attempt `printf list`",
		`compound run-attempt li${RUN_ATTEMPT_SUFFIX}`,
		"compound run-attempt l*",
		"compound run-attempt {list,sweep}",
	} {
		got := policy.ClassifyToolAction(policy.ToolActionRequest{
			Tool:               "Bash",
			Arguments:          map[string]any{"command": command},
			FirstPartyCommands: firstPartyCommandCatalog(),
		})
		if got.Known || got.MutationClass != "security" {
			t.Fatalf("%q classification = %+v, want fail-closed security hold", command, got)
		}
	}
}

func TestFirstPartyCommandCatalogMatchesEveryRuntimeCommand(t *testing.T) {
	t.Parallel()

	catalog := firstPartyCommandCatalog()
	for name, command := range compoundCommandRegistry() {
		got, ok := catalog.Lookup(name)
		if !ok {
			t.Fatalf("runtime command %q has no admission metadata", name)
		}
		if got != command.Admission {
			t.Fatalf("runtime command %q admission = %q, catalog = %q", name, command.Admission, got)
		}
		for subcommand, want := range command.SubcommandAdmission {
			got, ok := catalog.LookupSubcommand(name, subcommand)
			if !ok || got != want {
				t.Fatalf("runtime command %q %q admission = %q found=%t, want %q", name, subcommand, got, ok, want)
			}
		}
		for subcommand, flagAdmissions := range command.SubcommandFlagAdmission {
			for flag, want := range flagAdmissions {
				got, ok := catalog.LookupSubcommandWithFlags(name, subcommand, []string{subcommand, flag})
				if !ok || got != want {
					t.Fatalf("runtime command %q %q flag %q admission = %q found=%t, want %q", name, subcommand, flag, got, ok, want)
				}
			}
		}
	}
}
