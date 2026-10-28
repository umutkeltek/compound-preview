package main

import (
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/policy"
)

func classifyViaPolicyCLI(t *testing.T, command string) policy.ShellClassifyReport {
	t.Helper()
	out := captureStdout(t, func() { cmdPolicy([]string{"classify", "--command", command, "--json"}) })
	var report policy.ShellClassifyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("policy classify --json: %v\n%s", err, out)
	}
	return report
}

// TestPolicyClassifyDispatchesToShellVerbCatalog proves `compound policy
// classify` reaches the catalog with the executable's real first-party
// registry: a catalog row is named, and a registry read falls back unchanged.
func TestPolicyClassifyDispatchesToShellVerbCatalog(t *testing.T) {
	t.Setenv("COMPOUND_HOME", t.TempDir())

	ads := classifyViaPolicyCLI(t, "compound product ads import --platform apple_search_ads --product demo-app --report r.csv")
	if ads.Capability != "ads.report.write" || ads.MutationClass != "local_reversible" ||
		ads.Destination != "ads-product:demo-app" || ads.CatalogRow != "compound-product-ads-write" {
		t.Fatalf("ads import report = %+v", ads)
	}
	version := classifyViaPolicyCLI(t, "compound version")
	if version.CatalogRow != policy.ShellVerbCatalogFallback || version.Capability != "shell.local_command" {
		t.Fatalf("a first-party registry read must fall back to the routine-read lane: %+v", version)
	}
}

// TestLiveActionIntentsNameShellVerbCatalogCapability drives the live hook's
// intent builder: the intent a standing grant is matched against carries the
// catalog capability and semantic destination, with the command hash kept as
// evidence.
func TestLiveActionIntentsNameShellVerbCatalogCapability(t *testing.T) {
	t.Setenv("COMPOUND_HOME", t.TempDir())
	for _, tc := range []struct {
		harness, tool, command         string
		capability, class, destination string
	}{
		{"codex", "exec_command", "gog gmail send --to x@example.com -a ops@example.com",
			"gmail.message.send", "public", "gmail-account:ops@example.com"},
		{"claude-code", "Bash", "asc reviews list --app app-fixture-1",
			"app_store.customer_reviews.read", "read", "asc-app:app-fixture-1"},
		{"claude-code", "Bash", `curl -s -H "Authorization: Bearer $PADDLE_API_KEY" https://api.paddle.com/transactions`,
			"paddle.api.read", "read", "paddle-api:api.paddle.com"},
	} {
		actions, err := liveActionIntents(tc.harness, claudeLiveHookInput{
			SessionID:          "sess_shell_verb_catalog",
			WorkID:             "work_shell_verb_catalog",
			Cwd:                t.TempDir(),
			ToolName:           tc.tool,
			ToolInput:          map[string]any{"command": tc.command},
			ToolSchemaRevision: "builtin:exec-command-v1",
		}, cliTestNow)
		if err != nil {
			t.Fatalf("liveActionIntents(%q): %v", tc.command, err)
		}
		if len(actions) != 1 {
			t.Fatalf("%q: actions = %d, want 1", tc.command, len(actions))
		}
		action := actions[0]
		if action.Capability != tc.capability || action.MutationClass != tc.class || action.Destination != tc.destination {
			t.Fatalf("%q intent = capability %q class %q destination %q; want %q %q %q",
				tc.command, action.Capability, action.MutationClass, action.Destination, tc.capability, tc.class, tc.destination)
		}
		hash := classifyViaPolicyCLI(t, tc.command).CommandHash
		found := false
		for _, ref := range action.EvidenceRefs {
			found = found || ref == hash
		}
		if !strings.HasPrefix(hash, "command:sha256:") || !found {
			t.Fatalf("%q: command hash %q missing from intent evidence %v", tc.command, hash, action.EvidenceRefs)
		}
	}
}
