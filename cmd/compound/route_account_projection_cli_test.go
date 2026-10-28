package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
	"compound/internal/routeevidence"
	"gopkg.in/yaml.v3"
)

// TestRouteImportAccountProjectionCLI exercises the `route import-account-projection` subcommand
// end to end: it writes the projection and reports the provider count + verdict, and the
// projection round-trips through the loader.
func TestRouteImportAccountProjectionCLI(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	src := filepath.Join("..", "..", "fixtures", "route", "account-projection-source.json")

	out := captureStdout(t, func() { cmdRoute([]string{"import-account-projection", src}) })
	if !strings.Contains(out, "# route import-account-projection") || !strings.Contains(out, "- providers: 3") {
		t.Fatalf("bad import-account-projection output:\n%s", out)
	}
	if !strings.Contains(out, "projection_not_governed_registry") {
		t.Fatalf("import-account-projection must declare projection verdict:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(homePath, "projections", "account-projection.json")); err != nil {
		t.Fatalf("projection not written: %v", err)
	}
	projection, ok, err := routeevidence.LoadAccountProjection(homePath)
	if err != nil || !ok {
		t.Fatalf("load projection: ok=%v err=%v", ok, err)
	}
	if len(projection.Providers) != 3 {
		t.Fatalf("bad round-tripped projection: %+v", projection)
	}
}

// TestRouteUsageMentionsImportAccountProjection pins both usage surfaces the packet requires
// updated: the short routeUsage("") default and (indirectly, via the same string) the CLI's own
// contract for `compound route` with no/unknown subcommand.
func TestRouteUsageMentionsImportAccountProjection(t *testing.T) {
	if !strings.Contains(routeUsage(""), "import-account-projection") {
		t.Fatalf("routeUsage(\"\") must mention import-account-projection:\n%s", routeUsage(""))
	}
}

// TestRouteAccountProjectionGatesPlanCLI drives `route plan --capability-class build-implement`
// end to end through the CLI after importing a projection where codex has zero healthy accounts,
// and asserts the human output omits the codex lane for an account-admission reason without
// touching the (untouched-by-this-packet) plan print block.
func TestRouteAccountProjectionGatesPlanCLI(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	manifest := routeevidence.ModelCapabilitiesManifest{
		Version:           1,
		Purpose:           "account admission gate CLI fixture",
		CapabilityClasses: []string{"build-implement"},
		Models: map[string]routeevidence.ModelCapabilityEntry{
			"codex/gpt": {
				Lane: "codex (subscription)", Harness: "codex", ProviderRoute: "codex",
				EffortDefault: "high", CostClass: "subscription-fixed", Tier: "premium", PacketCeiling: "multi-file",
				Capabilities: map[string]routeevidence.CapabilityClaim{"build-implement": {Level: "primary", Evidence: "fixture codex"}},
			},
			"switchback/fallback": {
				Lane: "switchback fallback", Harness: "switchback", ProviderRoute: "auto/build",
				EffortDefault: "medium", CostClass: "api-marginal", Tier: "cheap", PacketCeiling: "multi-file",
				Capabilities: map[string]routeevidence.CapabilityClaim{"build-implement": {Level: "capable", Evidence: "fixture fallback"}},
			},
		},
		RoutingDefaults: map[string][]string{"build-implement": {"codex/gpt", "switchback/fallback"}},
	}
	manifestData, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestSrc := filepath.Join(t.TempDir(), "model-capabilities.yaml")
	if err := os.WriteFile(manifestSrc, manifestData, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	captureStdout(t, func() { cmdRoute([]string{"import-model-capabilities", manifestSrc}) })

	projectionSrc := filepath.Join("..", "..", "fixtures", "route", "account-projection-source.json")
	captureStdout(t, func() { cmdRoute([]string{"import-account-projection", projectionSrc}) })

	out := captureStdout(t, func() { cmdRoute([]string{"plan", "--capability-class", "build-implement"}) })
	if !strings.Contains(out, "# route capability-plan") {
		t.Fatalf("bad route plan output:\n%s", out)
	}
	if !strings.Contains(out, "- omitted:") || !strings.Contains(out, "codex/gpt") || !strings.Contains(out, "state=dead") || !strings.Contains(out, "healthy=0") || !strings.Contains(out, "provider=codex") {
		t.Fatalf("plan output must omit codex/gpt for zero healthy accounts:\n%s", out)
	}
	firstLane := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "- lane:") {
			firstLane = line
			break
		}
	}
	if !strings.Contains(firstLane, "switchback/fallback") {
		t.Fatalf("chain must lead with fallback, first lane line was %q\nfull:\n%s", firstLane, out)
	}
	if strings.Contains(out, "pa_test") || strings.Contains(out, "account_id") {
		t.Fatalf("route plan CLI output must never leak account identity:\n%s", out)
	}
}
