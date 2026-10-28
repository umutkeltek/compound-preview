package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/finding"
	"compound/internal/home"
	"compound/internal/mem"
)

const (
	cliFlagSeamChildEnv = "COMPOUND_TEST_CLI_FLAG_SEAM_CHILD"
	cliFlagSeamArgsEnv  = "COMPOUND_TEST_CLI_FLAG_SEAM_ARGS"
)

func TestCLIFlagSeamChild(t *testing.T) {
	if os.Getenv(cliFlagSeamChildEnv) != "1" {
		return
	}
	raw, err := base64.RawStdEncoding.DecodeString(os.Getenv(cliFlagSeamArgsEnv))
	if err != nil {
		t.Fatalf("decode CLI args: %v", err)
	}
	var args []string
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("parse CLI args: %v", err)
	}
	os.Args = append([]string{"compound"}, args...)
	main()
}

func TestFindingShowGlobalHomeAndRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	targetHome, fallbackHome := initializedCLIFlagHomes(t)
	recorded, err := finding.Record(targetHome, finding.Finding{
		Topic: "cli-flag-seam",
		Body:  "finding show reads the selected home",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("record finding: %v", err)
	}

	out, err := runCLIFlagSeam(t, fallbackHome, "finding", "show", "--home", targetHome, recorded.ID, "--json")
	if err != nil {
		t.Fatalf("finding show --home failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), recorded.ID) {
		t.Fatalf("finding show did not read target home:\n%s", out)
	}
	assertCLIUnknownFlag(t, targetHome, "finding", "show", "--bogus", recorded.ID, "--json")
}

func TestCaptureShowGlobalHomeAndRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	targetHome, fallbackHome := initializedCLIFlagHomes(t)
	store, err := mem.Open(targetHome)
	if err != nil {
		t.Fatalf("open target memory: %v", err)
	}
	const body = "capture show reads the selected home"
	cid, _, err := store.CaptureExhaust("test", "", body, cliTestNow)
	if err != nil {
		t.Fatalf("seed capture: %v", err)
	}

	out, err := runCLIFlagSeam(t, fallbackHome, "capture", "show", "--home", targetHome, cid)
	if err != nil {
		t.Fatalf("capture show --home failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), body) {
		t.Fatalf("capture show did not read target home:\n%s", out)
	}
	assertCLIUnknownFlag(t, targetHome, "capture", "show", "--bogus", cid)
}

func TestPlacementResolveGlobalHomeAndRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	targetHome, fallbackHome := initializedCLIFlagHomes(t)
	path := filepath.Join(targetHome, "private", "stray.md")

	out, err := runCLIFlagSeam(t, fallbackHome, "placement", "resolve", "--home", targetHome, path, "--json")
	if err != nil {
		t.Fatalf("placement resolve --home failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"allowed": false`) || !strings.Contains(string(out), path) {
		t.Fatalf("placement resolve did not evaluate against target home:\n%s", out)
	}
	assertCLIUnknownFlag(t, targetHome, "placement", "resolve", "--bogus", path, "--json")
}

func TestKnowledgeImportGlobalHomeAndRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	targetHome, fallbackHome := initializedCLIFlagHomes(t)
	fixture, err := filepath.Abs(filepath.Join("..", "..", "fixtures", "knowledge", "arxiv-export.json"))
	if err != nil {
		t.Fatalf("resolve knowledge fixture: %v", err)
	}

	out, err := runCLIFlagSeam(t, fallbackHome, "knowledge", "import", "--home", targetHome, fixture, "--json")
	if err != nil {
		t.Fatalf("knowledge import --home failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(targetHome, "indexes", "knowledge-imports.jsonl")); err != nil {
		t.Fatalf("knowledge import did not write target home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fallbackHome, "indexes", "knowledge-imports.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("knowledge import wrote fallback home, stat err=%v", err)
	}
	assertCLIUnknownFlag(t, targetHome, "knowledge", "import", "--bogus", fixture, "--json")
}

func TestMemoryTimelineGlobalHomeAndRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	targetHome, fallbackHome := initializedCLIFlagHomes(t)
	store, err := mem.Open(targetHome)
	if err != nil {
		t.Fatalf("open target memory: %v", err)
	}
	captured, err := store.Capture([]mem.Candidate{{
		Entity:          "FlagSeamMemory",
		Predicate:       "home_status",
		Object:          "selected",
		ObservationDate: cliTestNow.Format("2006-01-02T15:04:05Z07:00"),
		Confidence:      0.9,
		Derivation:      "deterministic",
		ClaimType:       "status",
		PromotionBasis:  candidates.BasisRuntimeProof,
		SourceTraceRef:  "compound://runtime-proof/cli-flag-seam",
		SnippetHash:     "sha256:cli-flag-seam",
		Visibility:      "operational",
	}}, cliTestNow)
	if err != nil {
		t.Fatalf("capture memory candidate: %v", err)
	}
	if _, err := store.Adjudicate(captured[0].ID, "promote", "work_cli_flag_seam", cliTestNow); err != nil {
		t.Fatalf("promote memory candidate: %v", err)
	}

	out, err := runCLIFlagSeam(t, fallbackHome, "memory", "timeline", "--home", targetHome, "FlagSeamMemory")
	if err != nil {
		t.Fatalf("memory timeline --home failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "FlagSeamMemory.home_status = selected") {
		t.Fatalf("memory timeline did not read target home:\n%s", out)
	}
	assertCLIUnknownFlag(t, targetHome, "memory", "timeline", "--bogus", "FlagSeamMemory")
}

func TestRouteEvidenceAuthorityUsesGlobalHome(t *testing.T) {
	t.Parallel()
	targetHome, fallbackHome := initializedCLIFlagHomes(t)
	out, err := runCLIFlagSeam(t, fallbackHome, "route", "evidence-authority", "--home", targetHome, "--recent", "0", "--json")
	if err != nil {
		t.Fatalf("route evidence-authority --home failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), targetHome) || strings.Contains(string(out), fallbackHome) {
		t.Fatalf("route evidence-authority did not consume target home:\n%s", out)
	}
}

func TestGlobalHomeOverridesEnvironment(t *testing.T) {
	t.Setenv("COMPOUND_HOME", "fallback-home")
	target := filepath.Join(t.TempDir(), "selected-home")
	if _, err := home.PrepareGlobalCLIArgs([]string{"narrate", "--home", target, "--json"}); err != nil {
		t.Fatalf("prepare global args: %v", err)
	}
	want, err := filepath.Abs(target)
	if err != nil {
		t.Fatalf("resolve target home: %v", err)
	}
	if got := os.Getenv("COMPOUND_HOME"); got != want {
		t.Fatalf("COMPOUND_HOME = %q, want explicit flag %q", got, want)
	}
}

func TestValueFlagSurvivorPreservesBehavior(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "absent", args: []string{"value"}},
		{name: "trailing", args: []string{"--item"}},
		{name: "first occurrence", args: []string{"--item", "first", "--item", "second"}, want: "first"},
		{name: "value may begin with dash", args: []string{"--item", "--literal"}, want: "--literal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := valueFlag(tt.args, "--item"); got != tt.want {
				t.Fatalf("valueFlag(%#v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func initializedCLIFlagHomes(t *testing.T) (string, string) {
	t.Helper()
	targetHome := t.TempDir()
	fallbackHome := t.TempDir()
	for name, path := range map[string]string{"target": targetHome, "fallback": fallbackHome} {
		if err := home.Init(path, "test", cliTestNow); err != nil {
			t.Fatalf("init %s home: %v", name, err)
		}
	}
	return targetHome, fallbackHome
}

func runCLIFlagSeam(t *testing.T, fallbackHome string, args ...string) ([]byte, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal CLI args: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIFlagSeamChild$")
	cmd.Env = cliFlagSeamEnv(fallbackHome,
		cliFlagSeamChildEnv+"=1",
		cliFlagSeamArgsEnv+"="+base64.RawStdEncoding.EncodeToString(raw),
	)
	return cmd.CombinedOutput()
}

func assertCLIUnknownFlag(t *testing.T, homePath string, args ...string) {
	t.Helper()
	out, err := runCLIFlagSeam(t, homePath, args...)
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("%s must reject --bogus, got err=%v output:\n%s", strings.Join(args[:2], " "), err, out)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("%s --bogus exit = %d, want 2; output:\n%s", strings.Join(args[:2], " "), exitErr.ExitCode(), out)
	}
	if !strings.Contains(string(out), "unknown flag --bogus") {
		t.Fatalf("%s --bogus output missing hard error:\n%s", strings.Join(args[:2], " "), out)
	}
}

func cliFlagSeamEnv(homePath string, extra ...string) []string {
	filtered := make([]string, 0, len(os.Environ())+len(extra)+1)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "COMPOUND_HOME=") ||
			strings.HasPrefix(item, cliFlagSeamChildEnv+"=") ||
			strings.HasPrefix(item, cliFlagSeamArgsEnv+"=") {
			continue
		}
		filtered = append(filtered, item)
	}
	filtered = append(filtered, "COMPOUND_HOME="+homePath)
	return append(filtered, extra...)
}
