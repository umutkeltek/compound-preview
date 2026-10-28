package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"compound/internal/policy"
)

// The route authority verbs must be reachable through the real route
// dispatcher, not only through internal/routeevidence functions. Each child
// deliberately supplies invalid arguments: reaching the verb-specific usage
// and exit 2 proves the dispatcher selected that handler.
func TestRouteAuthorityVerbsAreWiredAndDiscoverable(t *testing.T) {
	t.Parallel()
	if os.Getenv("COMPOUND_ROUTE_AUTHORITY_CLI_CHILD") == "1" {
		verb := os.Getenv("COMPOUND_ROUTE_AUTHORITY_CLI_VERB")
		args := []string{verb}
		if verb == "model-capabilities" {
			args = append(args, "--definitely-invalid")
		}
		if !tryRoutePromotionCommand(args, t.TempDir()) {
			os.Exit(44)
		}
		os.Exit(0)
	}

	cases := []struct {
		verb      string
		usagePart string
	}{
		{verb: "model-capabilities", usagePart: "route model-capabilities"},
		{verb: "promotion-apply", usagePart: "route promotion-apply"},
		{verb: "promotion-revert", usagePart: "route promotion-revert"},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRouteAuthorityVerbsAreWiredAndDiscoverable$")
			cmd.Env = append(os.Environ(),
				"COMPOUND_ROUTE_AUTHORITY_CLI_CHILD=1",
				"COMPOUND_ROUTE_AUTHORITY_CLI_VERB="+tc.verb,
			)
			out, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 2 {
				t.Fatalf("%s dispatcher exit = %v, want usage exit 2; output:\n%s", tc.verb, err, out)
			}
			if !strings.Contains(string(out), tc.usagePart) {
				t.Fatalf("%s did not reach its handler usage; output:\n%s", tc.verb, out)
			}
		})
	}

	allHelp := routeUsage("")
	for _, verb := range []string{"model-capabilities", "promotion-apply", "promotion-revert"} {
		if !strings.Contains(allHelp, verb) {
			t.Fatalf("route help omits %s: %s", verb, allHelp)
		}
		if got := routeUsage(verb); !strings.Contains(got, "route "+verb) {
			t.Fatalf("route %s --help is not specific: %s", verb, got)
		}
	}
}

// Both mutation verbs are previews unless --confirm is present. First-party
// command admission must preserve that distinction so inspecting a plan is a
// read while the actual authority write stays governed.
func TestRouteAuthorityPreviewIsReadAndConfirmIsGoverned(t *testing.T) {
	t.Parallel()
	catalog := firstPartyCommandCatalog()
	for _, verb := range []string{"promotion-apply", "promotion-revert"} {
		preview, ok := catalog.LookupSubcommandWithFlags("route", verb, []string{verb})
		if !ok || preview != policy.FirstPartyCommandRead {
			t.Fatalf("route %s preview admission = %q found=%t, want read", verb, preview, ok)
		}
		confirmed, ok := catalog.LookupSubcommandWithFlags("route", verb, []string{verb, "--confirm"})
		if !ok || confirmed != policy.FirstPartyCommandGoverned {
			t.Fatalf("route %s --confirm admission = %q found=%t, want governed", verb, confirmed, ok)
		}
	}
}
