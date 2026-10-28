package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"compound/internal/arch"
	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/contract"
	"compound/internal/decisions"
	"compound/internal/distiller"
	"compound/internal/envelope"
	"compound/internal/eventsource"
	"compound/internal/finding"
	"compound/internal/home"
	"compound/internal/hook"
	"compound/internal/knowledge"
	"compound/internal/mem"
	"compound/internal/observation"
	"compound/internal/panel"
	"compound/internal/policy"
	"compound/internal/product"
	"compound/internal/projection"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
	"compound/internal/runattempt"
	"compound/internal/runtimebinding"
	"compound/internal/sourceobs"
	"compound/internal/tooltopology"
	"compound/internal/work"
	"gopkg.in/yaml.v3"
)

var cliTestNow = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

func init() {
	workbenchNow = func() time.Time { return cliTestNow }
}

const workCLIBoundarySubEnv = "COMPOUND_WORK_CLI_BOUNDARY_SUB"

func writeCLITrace(t *testing.T, homePath, id, workID string) envelope.TraceEnvelope {
	t.Helper()
	trace := envelope.TraceEnvelope{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTraceEnvelope,
			ID:            id,
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Source:          envelope.TraceSource{System: "test", SourceKind: "cli", EventClass: "observe_run"},
		Actor:           "codex",
		MutationClass:   "local_reversible",
		Summary:         "observed CLI run",
		ToolsModelsUsed: []string{},
		FilesTouched:    []string{},
		Outputs:         []string{"trace-output:cli"},
	}
	if err := trace.Validate(); err != nil {
		t.Fatal(err)
	}
	traceBytes, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(homePath, "events", trace.ID+".json")
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, append(traceBytes, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return trace
}

func containsString(set []string, v string) bool {
	for _, item := range set {
		if item == v {
			return true
		}
	}
	return false
}

func TestPrepareGlobalArgs(t *testing.T) {
	t.Run("strips home and sets absolute environment", func(t *testing.T) {
		t.Setenv("COMPOUND_HOME", "fallback-home")
		got, err := home.PrepareGlobalCLIArgs([]string{"work", "show", "--home", "relative-home", "work_123", "--json"})
		if err != nil {
			t.Fatalf("prepare global args: %v", err)
		}
		want := []string{"work", "show", "work_123", "--json"}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("prepared args = %#v, want %#v", got, want)
		}
		wantHome, err := filepath.Abs("relative-home")
		if err != nil {
			t.Fatalf("absolute expected home: %v", err)
		}
		if gotHome := os.Getenv("COMPOUND_HOME"); gotHome != wantHome {
			t.Fatalf("COMPOUND_HOME = %q, want %q", gotHome, wantHome)
		}
	})

	t.Run("rejects missing home value", func(t *testing.T) {
		t.Setenv("COMPOUND_HOME", "fallback-home")
		if _, err := home.PrepareGlobalCLIArgs([]string{"work", "show", "--home"}); err == nil {
			t.Fatal("trailing --home must fail")
		}
		if got := os.Getenv("COMPOUND_HOME"); got != "fallback-home" {
			t.Fatalf("COMPOUND_HOME changed after missing value: %q", got)
		}
	})

	t.Run("rejects blank home value", func(t *testing.T) {
		t.Setenv("COMPOUND_HOME", "fallback-home")
		if _, err := home.PrepareGlobalCLIArgs([]string{"work", "show", "--home", "  "}); err == nil {
			t.Fatal("blank --home value must fail")
		}
		if got := os.Getenv("COMPOUND_HOME"); got != "fallback-home" {
			t.Fatalf("COMPOUND_HOME changed after blank value: %q", got)
		}
	})

	t.Run("passes non-home args through untouched", func(t *testing.T) {
		t.Setenv("COMPOUND_HOME", "fallback-home")
		input := []string{"work", "show", "--home=elsewhere", "work_123", "--json"}
		got, err := home.PrepareGlobalCLIArgs(input)
		if err != nil {
			t.Fatalf("prepare global args: %v", err)
		}
		if strings.Join(got, "\x00") != strings.Join(input, "\x00") {
			t.Fatalf("prepared args = %#v, want exact pass-through %#v", got, input)
		}
		if gotHome := os.Getenv("COMPOUND_HOME"); gotHome != "fallback-home" {
			t.Fatalf("COMPOUND_HOME = %q, want untouched fallback", gotHome)
		}
	})
}

func TestPrepareCompoundEntrypointArgsRewritesOnlyOSAlias(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		argv0   string
		args    []string
		want    []string
		wantErr bool
	}{
		{name: "compound unchanged", argv0: "/opt/bin/compound", args: []string{"system", "status", "--json"}, want: []string{"system", "status", "--json"}},
		{name: "default status", argv0: "/opt/bin/os", want: []string{"system", "status"}},
		{name: "status", argv0: "os", args: []string{"status", "--json"}, want: []string{"system", "status", "--json"}},
		{name: "doctor", argv0: "os", args: []string{"doctor", "--json"}, want: []string{"doctor", "--json"}},
		{name: "capability", argv0: "os", args: []string{"capability", "ship", "ios", "--privacy", "--json"}, want: []string{"capability", "resolve", "ship", "ios", "--privacy-sensitive", "--json"}},
		{name: "capability missing", argv0: "os", args: []string{"capability"}, wantErr: true},
		{name: "capability packet retired", argv0: "os", args: []string{"capability", "task", "--write-packet"}, wantErr: true},
		{name: "skills query", argv0: "os", args: []string{"skills", "app", "store", "--host", "claude", "--json"}, want: []string{"capability", "surface", "app", "store", "--harness", "claude_code", "--json"}},
		{name: "skills inventory", argv0: "os", args: []string{"skills", "--host", "codex", "--json"}, want: []string{"capability", "list", "--harness", "codex", "--json"}},
		{name: "route alias", argv0: "os", args: []string{"route", "architecture", "--privacy", "--json"}, want: []string{"route", "plan", "--capability-class", "plan-decompose", "--privacy-sensitive", "--json"}},
		{name: "route extraction alias", argv0: "os", args: []string{"route", "extract"}, want: []string{"route", "plan", "--capability-class", "classify-extract"}},
		{name: "route list retired", argv0: "os", args: []string{"route", "--list"}, wantErr: true},
		{name: "unknown retired", argv0: "os", args: []string{"proof"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := prepareCompoundEntrypointArgs(tc.argv0, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("prepare args = %#v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("prepared args = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestVersionStringEmbedsBuildSHA proves `compound version` carries the build SHA that
// install.sh's ldflags embed — buildSHA is a package var (set via -X at build time), so
// the test sets it directly rather than needing a real ldflags-built binary (T3).
func TestVersionStringEmbedsBuildSHA(t *testing.T) {
	original := buildSHA
	defer func() { buildSHA = original }()

	buildSHA = ""
	if got := versionString(); !strings.Contains(got, "(build dev)") {
		t.Fatalf("unset buildSHA: versionString() = %q, want it to report dev", got)
	}

	buildSHA = "a1b2c3d"
	got := versionString()
	if !strings.Contains(got, "a1b2c3d") {
		t.Fatalf("versionString() = %q, want embedded sha a1b2c3d", got)
	}
	if strings.Contains(got, "(build dev)") {
		t.Fatalf("versionString() = %q, still reports dev after buildSHA was set", got)
	}

	out := captureStdout(t, func() { fmt.Println(versionString()) })
	if !strings.Contains(out, "a1b2c3d") {
		t.Fatalf("printed version output = %q, want embedded sha", out)
	}
}

// TestVersionJSONAnswersTheInstallChallenge proves the surface install verification depends on:
// `version --json` must emit a PARSEABLE report bound to a nonce the caller mints, not just the
// version string a stub can echo. Without the challenge fields, the installer is back to a string
// compare — the probe a four-line fixture answers correctly.
func TestVersionJSONAnswersTheInstallChallenge(t *testing.T) {
	original := buildSHA
	defer func() { buildSHA = original }()
	buildSHA = "a1b2c3d"

	out := captureStdout(t, func() { cmdVersion([]string{"--json", "--challenge", "nonce-xyz"}) })
	var report arch.VersionReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("version --json is not a parseable report: %v\n%s", err, out)
	}
	if report.SchemaVersion != arch.VersionReportSchema {
		t.Fatalf("schema_version = %q, want %q", report.SchemaVersion, arch.VersionReportSchema)
	}
	if report.BuildSHA != "a1b2c3d" || strings.TrimSpace(report.Version) == "" {
		t.Fatalf("version report = %+v", report)
	}
	if report.Challenge != "nonce-xyz" {
		t.Fatalf("challenge = %q, want the nonce echoed verbatim", report.Challenge)
	}
	want := arch.VersionChallengeDigest("nonce-xyz", report.Version, report.BuildSHA)
	if report.ChallengeDigest != want {
		t.Fatalf("challenge_digest = %q, want %q", report.ChallengeDigest, want)
	}

	// A different nonce must produce a different answer, or a recorded response replays forever.
	other := captureStdout(t, func() { cmdVersion([]string{"--json", "--challenge", "nonce-abc"}) })
	if strings.Contains(other, report.ChallengeDigest) {
		t.Fatalf("challenge_digest is nonce-independent:\n%s", other)
	}

	// Plain `version` keeps its human string, so the existing operator surface is unchanged.
	plain := captureStdout(t, func() { cmdVersion(nil) })
	if strings.TrimSpace(plain) != versionString() {
		t.Fatalf("plain version output = %q, want %q", plain, versionString())
	}
}

func TestSchemaCLIExportAndValidate(t *testing.T) {
	listOut := captureStdout(t, func() {
		cmdSchema([]string{"list"})
	})
	if !strings.Contains(listOut, "context_pack") || !strings.Contains(listOut, "route_evidence") {
		t.Fatalf("schema list output =\n%s", listOut)
	}

	exportOut := captureStdout(t, func() {
		cmdSchema([]string{"export", "context-pack"})
	})
	if !strings.Contains(exportOut, `"compound://schemas/context_pack"`) || !strings.Contains(exportOut, `"capability_offers"`) {
		t.Fatalf("schema export output =\n%s", exportOut)
	}

	dir := t.TempDir()
	allOut := captureStdout(t, func() {
		cmdSchema([]string{"export", "--all", "--dir", dir})
	})
	if !strings.Contains(allOut, "context_pack.schema.json") {
		t.Fatalf("schema export --all output =\n%s", allOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "route_evidence.schema.json")); err != nil {
		t.Fatalf("missing exported route schema: %v", err)
	}

	path := filepath.Join(t.TempDir(), "context-pack.json")
	data, err := json.Marshal(validCLIContextPack())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	validateOut := captureStdout(t, func() {
		cmdSchema([]string{"validate", "context-pack", path})
	})
	if !strings.Contains(validateOut, "ok (context-pack)") {
		t.Fatalf("schema validate output =\n%s", validateOut)
	}
}

func validCLIContextPack() envelope.ContextPack {
	return envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            "ctx_cli_schema",
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound", Version: "test"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD: "schema cli test",
		Included: []envelope.IncludedRef{{
			Ref:         "capability://skill:test",
			SourceClass: "generated",
			Reason:      "schema test",
			Confidence:  1,
			Status:      "current",
		}},
		Excluded: []envelope.ExcludedRef{},
		CapabilityOffers: []envelope.CapabilityOffer{{
			Ref:          "capability://skill:test",
			Kind:         "skill",
			Name:         "Test Skill",
			Score:        1,
			Why:          "trigger",
			CostRiskNote: "offer only; execution still requires WorkEnvelope, ActionIntent, and PolicyDecision",
		}},
		ActiveInstructions: []string{},
		ActiveDecisions:    []string{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
}

func writeCLIContextPackArtifact(t *testing.T, homePath, id string) {
	t.Helper()
	pack := validCLIContextPack()
	pack.ID = id
	path := filepath.Join(homePath, "context", "packs", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir context packs: %v", err)
	}
	data, err := json.MarshalIndent(pack, "", " ")
	if err != nil {
		t.Fatalf("marshal context pack: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write context pack: %v", err)
	}
}

func TestHomeArchiveCommandsRoundTrip(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "ledger", "durable.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "tmp", "cache.txt"), []byte("drop"), 0o644); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "home.tar.gz")
	out := captureStdout(t, func() {
		cmdHome([]string{"backup", "--target", archivePath})
	})
	if !strings.Contains(out, "# home backup") || !strings.Contains(out, "- archive: "+archivePath) {
		t.Fatalf("backup output:\n%s", out)
	}

	out = captureStdout(t, func() {
		cmdHome([]string{"verify", archivePath})
	})
	if !strings.Contains(out, "- verdict: ok") || !strings.Contains(out, "- kind: backup") {
		t.Fatalf("verify output:\n%s", out)
	}

	restorePath := filepath.Join(t.TempDir(), ".compound-restored")
	out = captureStdout(t, func() {
		cmdHome([]string{"restore", archivePath, "--target", restorePath})
	})
	if !strings.Contains(out, "# home restore") || !strings.Contains(out, "- verdict: ok") {
		t.Fatalf("restore output:\n%s", out)
	}
	if got := readFile(t, filepath.Join(restorePath, "ledger", "durable.json")); got != `{"ok":true}` {
		t.Fatalf("restored durable file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(restorePath, "tmp", "cache.txt")); !os.IsNotExist(err) {
		t.Fatalf("backup should not restore tmp cache, stat err=%v", err)
	}

	out = captureStdout(t, func() {
		cmdHome([]string{"compact"})
	})
	if !strings.Contains(out, "# home compact") || !strings.Contains(out, "- removed_files: 1") {
		t.Fatalf("compact output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(homePath, "tmp", "cache.txt")); !os.IsNotExist(err) {
		t.Fatalf("compact should remove tmp cache, stat err=%v", err)
	}
}

func TestHomeInitHelpIsReadOnly(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	t.Setenv("COMPOUND_HOME", homePath)

	out := captureStdout(t, func() {
		cmdHome([]string{"init", "--help"})
	})
	if !strings.Contains(out, "usage: compound home") {
		t.Fatalf("home init --help should print usage, got:\n%s", out)
	}
	if _, err := os.Stat(homePath); !os.IsNotExist(err) {
		t.Fatalf("home init --help mutated COMPOUND_HOME, stat err=%v", err)
	}
}

func TestProofCheckDryRunListsDeterministicGates(t *testing.T) {
	out := captureStdout(t, func() {
		cmdProofCheck([]string{"--dry-run", "--json", "--repo", "."})
	})
	var report proofCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse proof-check report: %v\n%s", err, out)
	}
	if report.SchemaVersion != proofCheckSchemaVersion ||
		report.ProfileKind != work.ProofResourceProfileCanonicalFull ||
		report.Status != "ok" ||
		!report.DryRun {
		t.Fatalf("bad proof-check report: %+v", report)
	}
	wantProofs := []string{
		"go-test-count-1-general-release",
		"go-test-count-1-home",
		"credential-password-alignment-native-pg17-no-skips",
		"go-vet-all",
		"go-build-all",
		"gofmt-check",
		"diff-check",
		"console-tsc-noemit",
		"console-smoke",
		"console-projection-timeout-smoke",
		"console-mutation-gate-smoke",
		"console-instance-pin-smoke",
		"console-continue-button-smoke",
		"console-work-action-row-smoke",
		"console-mutation-readiness-consumers-smoke",
		"console-private-fallback-smoke",
		"console-private-runtime-guard-smoke",
		"console-move-type-drift-smoke",
		"console-ai-boundary-smoke",
		"console-client-bundle-guard-smoke",
		"console-work-trace-smoke",
		"console-lineage-smoke",
		"console-generic-actions-smoke",
		"console-view-registry-smoke",
		"console-decide-brief-smoke",
		"console-decision-execute-smoke",
		"console-standing-law-smoke",
		"console-memory-data-smoke",
		"console-memory-read-honesty-smoke",
		"console-evidence-card-smoke",
		"console-attention-consistency-smoke",
		"console-work-language-smoke",
		"console-work-legibility-smoke",
		"console-ask-triage-smoke",
		"console-representation-falsifiers-smoke",
		"console-representation-cluster-guard-smoke",
		"console-agents-consistency-smoke",
		"console-projection-doors-smoke",
		"console-stream-tail-smoke",
		"console-decide-decision-feedback-smoke",
		"console-projection-degradation-smoke",
		"console-stream-door-smoke",
		"console-traffic-board-smoke",
		"console-ads-smoke",
		"console-delegated-sessions-smoke",
		"console-coordination-smoke",
		"bun-tools-test",
		"architecture-doctor-strict",
		"profiles-check-installed-only",
		"workbench-projection-refresh",
		"codex-pretooluse-native-gate-proof",
		"memory-doctor",
		"redact-public-clean",
	}
	if len(report.Steps) != len(wantProofs) {
		t.Fatalf("steps = %+v, want %d", report.Steps, len(wantProofs))
	}
	for i, want := range wantProofs {
		if report.Steps[i].ProofRef != want || report.Steps[i].Status != "planned" {
			t.Fatalf("step %d = %+v, want proof %s planned", i, report.Steps[i], want)
		}
		if want == "codex-pretooluse-native-gate-proof" &&
			!stringSliceContains(report.Steps[i].Command, "--enforce") {
			t.Fatalf("native gate proof command is not explicitly enforced: %+v", report.Steps[i].Command)
		}
		isBunConsoleSmoke := report.Steps[i].Name == "console-projection-timeout-smoke" || report.Steps[i].Name == "console-continue-button-smoke" || report.Steps[i].Name == "console-work-action-row-smoke" || report.Steps[i].Name == "console-work-trace-smoke" || report.Steps[i].Name == "console-lineage-smoke" || report.Steps[i].Name == "console-generic-actions-smoke" || report.Steps[i].Name == "console-view-registry-smoke" || report.Steps[i].Name == "console-decide-brief-smoke" || report.Steps[i].Name == "console-decision-execute-smoke" || report.Steps[i].Name == "console-standing-law-smoke" || report.Steps[i].Name == "console-memory-data-smoke" || report.Steps[i].Name == "console-evidence-card-smoke" || report.Steps[i].Name == "console-work-language-smoke" || report.Steps[i].Name == "console-work-legibility-smoke" || report.Steps[i].Name == "console-ask-triage-smoke" || report.Steps[i].Name == "console-representation-falsifiers-smoke" || report.Steps[i].Name == "console-stream-tail-smoke" || report.Steps[i].Name == "console-decide-decision-feedback-smoke" || report.Steps[i].Name == "console-attention-consistency-smoke" || report.Steps[i].Name == "console-projection-degradation-smoke" || report.Steps[i].Name == "console-stream-door-smoke" || report.Steps[i].Name == "console-traffic-board-smoke" || report.Steps[i].Name == "console-ads-smoke" || report.Steps[i].Name == "console-delegated-sessions-smoke" || report.Steps[i].Name == "console-coordination-smoke"
		isBunConsoleSmoke = isBunConsoleSmoke || report.Steps[i].Name == "console-memory-read-honesty-smoke"
		if strings.HasPrefix(report.Steps[i].Name, "console-") && report.Steps[i].Name != "console-tsc" && !isBunConsoleSmoke && !sameStringSlicePrefix(report.Steps[i].Command, []string{"node", "--experimental-strip-types"}) {
			t.Fatalf("console smoke step %s command = %+v, want node --experimental-strip-types", report.Steps[i].Name, report.Steps[i].Command)
		}
		if !stringSliceContains(report.ProofRefs, want) {
			t.Fatalf("proof refs missing %q: %+v", want, report.ProofRefs)
		}
	}
}

func TestProofCheckCIFlagRunsOnlyPortableGates(t *testing.T) {
	out := captureStdout(t, func() {
		cmdProofCheck([]string{"--ci", "--dry-run", "--json", "--repo", "."})
	})
	var report proofCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse proof-check --ci report: %v\n%s", err, out)
	}
	if !report.CIOnly || report.Status != "ok" || !report.DryRun {
		t.Fatalf("bad proof-check --ci report: ci_only=%v status=%s dry_run=%v", report.CIOnly, report.Status, report.DryRun)
	}
	// The CI gate set is single-sourced from proofCheckSteps via CIPortable; --ci must run
	// exactly the fresh-clone-portable gates, in order, and nothing that needs the external
	// redact binary, the installed binary, or a bun/node console runner.
	wantCIProofs := []string{
		"go-test-count-1-general-release",
		"go-test-count-1-home",
		"go-vet-all",
		"go-build-all",
		"gofmt-check",
		"diff-check",
		"architecture-doctor-strict",
		"memory-doctor",
	}
	if len(report.Steps) != len(wantCIProofs) {
		names := make([]string, 0, len(report.Steps))
		for _, s := range report.Steps {
			names = append(names, s.ProofRef)
		}
		t.Fatalf("proof-check --ci ran %d gates %v, want %d %v", len(report.Steps), names, len(wantCIProofs), wantCIProofs)
	}
	for i, want := range wantCIProofs {
		if report.Steps[i].ProofRef != want || report.Steps[i].Status != "planned" {
			t.Fatalf("ci step %d = %+v, want proof %s planned", i, report.Steps[i], want)
		}
	}
	// The non-portable gates must NOT appear under --ci (they hard-fail or skip in a fresh clone).
	excluded := []string{
		"redact-public-clean", "profiles-check-installed-only", "workbench-projection-refresh",
		"codex-pretooluse-native-gate-proof", "console-smoke", "console-tsc-noemit", "bun-tools-test",
	}
	for _, s := range report.Steps {
		for _, bad := range excluded {
			if s.ProofRef == bad {
				t.Fatalf("proof-check --ci included non-portable gate %q", bad)
			}
		}
	}
	// Guard the single-source invariant: every CIPortable gate in the canonical list is exactly
	// the wantCIProofs set — a newly-added gate mis-tagged CIPortable fails here, not silently in CI.
	steps, err := proofCheckSteps("")
	if err != nil {
		t.Fatalf("proofCheckSteps: %v", err)
	}
	gotPortable := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.CIPortable {
			gotPortable = append(gotPortable, s.ProofRef)
		}
	}
	if len(gotPortable) != len(wantCIProofs) {
		t.Fatalf("CIPortable gates = %v, want %v", gotPortable, wantCIProofs)
	}
	for i, want := range wantCIProofs {
		if gotPortable[i] != want {
			t.Fatalf("CIPortable gate %d = %s, want %s", i, gotPortable[i], want)
		}
	}
}

// TestProofCheckCIConsoleFlagRunsOnlyConsoleGates mirrors TestProofCheckCIFlagRunsOnlyPortableGates
// for the console/TypeScript lane: --ci-console must run exactly the gates that need bun +
// console/node_modules (every Dir=="console" step), in dry-run order, and never the bare-Go or
// binary-dependent gates, nor the env-coupled bun-tools-test. The expected set is single-sourced
// from proofCheckSteps (not hardcoded), so a newly-added console smoke joins this subset
// automatically and a mis-routed gate fails the test before it reds the CI console job.
func TestProofCheckCIConsoleFlagRunsOnlyConsoleGates(t *testing.T) {
	out := captureStdout(t, func() {
		cmdProofCheck([]string{"--ci-console", "--dry-run", "--json", "--repo", "."})
	})
	var report proofCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse proof-check --ci-console report: %v\n%s", err, out)
	}
	if !report.CIConsole || report.CIOnly || report.Status != "ok" || !report.DryRun {
		t.Fatalf("bad proof-check --ci-console report: ci_console=%v ci_only=%v status=%s dry_run=%v", report.CIConsole, report.CIOnly, report.Status, report.DryRun)
	}
	steps, err := proofCheckSteps("")
	if err != nil {
		t.Fatalf("proofCheckSteps: %v", err)
	}
	wantProofSet := map[string]bool{}
	for _, s := range steps {
		if s.Dir == "console" {
			wantProofSet[s.ProofRef] = true
		}
	}
	if len(wantProofSet) == 0 {
		t.Fatal("no console gates in proofCheckSteps; the CI console job would run nothing")
	}
	if len(report.Steps) != len(wantProofSet) {
		names := make([]string, 0, len(report.Steps))
		for _, s := range report.Steps {
			names = append(names, s.ProofRef)
		}
		t.Fatalf("proof-check --ci-console ran %d gates %v, want %d", len(report.Steps), names, len(wantProofSet))
	}
	for _, s := range report.Steps {
		if !wantProofSet[s.ProofRef] {
			t.Fatalf("proof-check --ci-console included non-console gate %q", s.ProofRef)
		}
		if s.Status != "planned" {
			t.Fatalf("ci-console step %s status=%s, want planned", s.ProofRef, s.Status)
		}
	}
	// The bare-Go / binary-dependent gates must NOT appear under --ci-console (no Go-test intent,
	// and they belong to the --ci job). bun-tools-test is excluded too: tools/*.test.ts are
	// env-coupled (asc_preflight validates a macOS launchd plist) and red a Linux CI runner — they
	// stay local-battery-only. Re-adding it here reds the CI console job, not this test, so it is
	// pinned out explicitly.
	for _, banned := range []string{
		"go-test-count-1-general-release", "go-test-count-1-home", "go-vet-all", "go-build-all", "gofmt-check", "diff-check",
		"architecture-doctor-strict", "memory-doctor", "redact-public-clean",
		"profiles-check-installed-only", "codex-pretooluse-native-gate-proof", "workbench-projection-refresh",
		"bun-tools-test",
	} {
		for _, s := range report.Steps {
			if s.ProofRef == banned {
				t.Fatalf("proof-check --ci-console included non-console gate %q", banned)
			}
		}
	}
}

func TestProofCheckMissingConsoleSmokeDependencyEmitsSkipReason(t *testing.T) {
	t.Parallel()
	step := proofCheckStep{Name: "console-work-trace-smoke", Command: []string{"bun", "scripts/work-trace-smoke.mjs"}, SkipIfMissing: "definitely-missing-proof-tool"}
	got := runProofCheckStep(t.TempDir(), step, "")
	if got.Status != "skipped" || got.SkipReason != "required executable definitely-missing-proof-tool not found" {
		t.Fatalf("silent proof-check skip: %+v", got)
	}
}

func TestProofCheckHumanOutputIncludesOnlyFailedStepTail(t *testing.T) {
	report := proofCheckReport{
		Status: "failed",
		Repo:   "/repo",
		Steps: []proofCheckStep{
			{
				Name:       "go-test",
				Command:    []string{"go", "test", "-count=1", "./..."},
				ProofRef:   "go-test-count-1-all",
				Status:     "failed",
				OutputTail: "--- FAIL: TestLinuxOnly\n    linux_only_test.go:42: exact failure evidence\nFAIL",
			},
			{
				Name:       "go-vet",
				Command:    []string{"go", "vet", "./..."},
				ProofRef:   "go-vet-all",
				Status:     "ok",
				OutputTail: "successful step chatter must stay hidden",
			},
		},
	}
	out := captureStdout(t, func() { printProofCheck(report) })
	for _, want := range []string{"output_tail:", "--- FAIL: TestLinuxOnly", "linux_only_test.go:42: exact failure evidence", "FAIL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("proof-check human output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "successful step chatter must stay hidden") {
		t.Fatalf("proof-check human output printed a successful step tail:\n%s", out)
	}
}

// A long Linux run can push an early package's FAIL line out of the bounded tail
// (pipeline 580 hid internal/act). The human CI report must still name it, and must
// print the tail once.
func TestProofCheckHumanOutputNamesFailuresMissingFromTail(t *testing.T) {
	report := proofCheckReport{
		Status: "failed",
		Repo:   "/repo",
		Steps: []proofCheckStep{{
			Name:         "go-test",
			Command:      []string{"go", "run", "./scripts/testbattery", "general-release"},
			ProofRef:     "go-test-count-1-general-release",
			Status:       "failed",
			OutputTail:   "ok  \tcompound/internal/zeta\t1.0s\nFAIL",
			FailureLines: []string{"--- FAIL: TestEarlyPackage (0.30s)", "FAIL\tcompound/internal/early\t37.2s"},
		}},
	}
	out := captureStdout(t, func() { printProofCheck(report) })
	for _, want := range []string{"failure_lines:", "--- FAIL: TestEarlyPackage (0.30s)", "FAIL\tcompound/internal/early\t37.2s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("proof-check human output missing %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "compound/internal/zeta"); got != 1 {
		t.Fatalf("proof-check human output printed the tail %d times, want 1:\n%s", got, out)
	}
}

func TestProofCheckChangedFilesIncludesCleanCommittedRange(t *testing.T) {
	t.Parallel()
	repo := initCompoundCommandGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "console.ts"), []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", "console.ts")
	runCommandGit(t, repo, "commit", "-m", "add console")

	files, err := proofCheckChangedFiles(repo, "HEAD~1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "console.ts" {
		t.Fatalf("changed files = %+v, want committed range file", files)
	}

	defaultFiles, err := proofCheckChangedFiles(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultFiles) != 0 {
		t.Fatalf("default changed files = %+v, want clean worktree empty", defaultFiles)
	}
}

func TestProofCheckArtifactConsumesVerificationAdjacentActions(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	report := proofCheckReport{
		SchemaVersion: proofCheckSchemaVersion,
		Status:        "ok",
		GeneratedAt:   cliTestNow.Format(time.RFC3339),
		Repo:          "/tmp/compound-test",
		ProofRefs:     []string{"architecture-doctor-strict", "workbench-projection-refresh", "codex-pretooluse-native-gate-proof", "go-test-count-1-all"},
		Steps: []proofCheckStep{
			{Name: "architecture-doctor", ProofRef: "architecture-doctor-strict", Status: "ok"},
			{Name: "workbench-projection", ProofRef: "workbench-projection-refresh", Status: "ok"},
			{Name: "codex-pretooluse-native-gate", ProofRef: "codex-pretooluse-native-gate-proof", Status: "ok"},
			{Name: "go-test", ProofRef: "go-test-count-1-all", Status: "ok"},
		},
	}
	report, err := recordProofCheckRuntimeArtifact(homePath, report)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArtifactRef != "artifact:artifacts/refs/proof-check-installed-runtime-20260629.md" {
		t.Fatalf("artifact ref = %q", report.ArtifactRef)
	}
	body := readFile(t, filepath.Join(homePath, "artifacts", "refs", "proof-check-installed-runtime-20260629.md"))
	for _, phrase := range []string{
		"proof-check-installed-runtime",
		"architecture-doctor-strict",
		"workbench-projection-refresh",
		"codex-pretooluse-native-gate-proof",
		"adjacent Workbench action",
		"verified",
		"Do not rerun",
	} {
		if !strings.Contains(body, phrase) {
			t.Fatalf("proof artifact missing %q:\n%s", phrase, body)
		}
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "app-store-connect ASC private config scheduler valve remains held", State: "held"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	workbench, err := store.Workbench(cliTestNow, work.WorkbenchOptions{RecentLimit: 10, Actor: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	adjacent := workbench.DispatchNext.AdjacentProgress
	if adjacent == nil {
		t.Fatalf("missing adjacent progress: %+v", workbench.DispatchNext)
	}
	for _, id := range []string{"proof-check-installed-runtime", "architecture-doctor-strict", "workbench-projection-refresh", "codex-pretooluse-native-gate-proof"} {
		action, ok := workbenchAdjacentActionByID(adjacent.ConsumedNonPacketActions, id)
		if !ok {
			t.Fatalf("%s not consumed by proof-check artifact: %+v", id, adjacent)
		}
		if len(action.Command) != 0 || !stringSliceContains(action.ConsumedEvidenceRefs, report.ArtifactRef) {
			t.Fatalf("%s consumed action should strip command and keep proof ref: %+v", id, action)
		}
	}
	if _, ok := workbenchAdjacentActionByID(adjacent.NonPacketActions, "proof-check-installed-runtime"); ok {
		t.Fatalf("proof-check action still active after proof artifact: %+v", adjacent.NonPacketActions)
	}
	if _, ok := workbenchAdjacentActionByID(adjacent.NonPacketActions, "workbench-projection-refresh"); ok {
		t.Fatalf("workbench projection action still active after proof artifact: %+v", adjacent.NonPacketActions)
	}
}

func TestProofCheckGoStepsUseLocalToolchain(t *testing.T) {
	dir := t.TempDir()
	goPath := filepath.Join(dir, "go")
	if err := os.WriteFile(goPath, []byte("#!/bin/sh\nprintf 'GOTOOLCHAIN=%s\\n' \"$GOTOOLCHAIN\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	result := runProofCheckStep(t.TempDir(), proofCheckStep{
		Name:     "go-test",
		Command:  []string{"go", "version"},
		ProofRef: "go-version-local-toolchain",
	}, "")
	if result.Status != "ok" || !strings.Contains(result.OutputTail, "GOTOOLCHAIN=local") {
		t.Fatalf("proof-check go env = %+v", result)
	}
}

func TestProofCheckGofmtFailsOnUnformattedTrackedGo(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runCommandGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main(){\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", "main.go")

	out, err := runProofCheckGofmt(repo)
	if err == nil {
		t.Fatalf("expected gofmt check to fail for unformatted tracked Go file, output=%s", out)
	}
	if !strings.Contains(string(out), "main.go") {
		t.Fatalf("gofmt output should identify unformatted file, got %q", out)
	}

	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runProofCheckGofmt(repo)
	if err != nil {
		t.Fatalf("expected gofmt check to pass after formatting: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "gofmt-clean") {
		t.Fatalf("clean gofmt output should be explicit, got %q", out)
	}
}

// TestProofCheckNativeGateDeniesUnownedWrite drives the real PreToolUse hook end-to-end and
// proves the open-world fix at the native seam: an agent inventing a file under COMPOUND_HOME
// in no typed habitat dir is denied (not silently allowed) and told where to go. Builds the
// binary, so it is skipped under -short.
func TestProofCheckNativeGateDeniesUnownedWrite(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the compound binary")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Clean(filepath.Join(wd, "..", ".."))
	exe := compoundTestBinary(t)
	step := proofCheckStep{
		Name:     "codex-pretooluse-native-gate",
		Command:  []string{exe, "hook", "pre-tool-use", "--live", "--harness", "codex", "--enforce"},
		ProofRef: "codex-pretooluse-native-gate-proof",
	}
	// A nil error means all three cases held: valid write allowed, misplaced canon denied, and
	// (the new assertion) the unowned write under the governed home denied with the redirect.
	if out, err := runProofCheckCodexPreToolUseNativeGate(repo, step); err != nil {
		t.Fatalf("native gate proof failed: %v\n%s", err, out)
	}
}

func TestSourceObserveBatchCLIRoutesFixturesWithoutApplying(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		wantRoute string
	}{
		{"gsc metric", "../../fixtures/observation/gsc-product-metric.json", observation.RouteProductSignal},
		{"asc metric", "../../fixtures/observation/asc-product-metric.json", observation.RouteProductSignal},
		{"content knowledge", "../../fixtures/observation/content-knowledge-export.json", observation.RouteKnowledgeExport},
		{"event trace", "../../fixtures/observation/event-trace-envelope.json", observation.RouteTraceEnvelope},
		{"route evidence", "../../fixtures/observation/route-evidence.json", observation.RouteRouteEvidence},
		{"social feedback", "../../fixtures/observation/social-distribution-feedback.json", observation.RouteCandidateOnly},
		{"support message", "../../fixtures/observation/support-comment.json", observation.RouteCandidateOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				cmdSource([]string{"observe-batch", tt.fixture, "--dry-run", "--json"})
			})
			var report observation.Report
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("parse observe-batch report: %v\n%s", err, out)
			}
			if report.Status != "ok" || !report.DryRun || report.AppliedProductSignals {
				t.Fatalf("report = %+v", report)
			}
			if len(report.Decisions) != 1 || report.Decisions[0].Route != tt.wantRoute {
				t.Fatalf("decisions = %+v, want route %s", report.Decisions, tt.wantRoute)
			}
			if len(report.ProductApplyResults) != 0 {
				t.Fatalf("dry-run applied product signals: %+v", report.ProductApplyResults)
			}
		})
	}
}

func TestSourceObserveBatchCLIApplyProductSignalsChangesProductLoopProjection(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(homePath, store, bundle, "fixtures/product/market-loop.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}
	before, err := product.BuildLoopModel(homePath, product.LoopOptions{ProductID: "product:focus-notes", Now: cliTestNow})
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/gsc-product-metric.json", "--apply-product-signals", "--json"})
	})
	var report observation.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse apply report: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.DryRun || !report.AppliedProductSignals {
		t.Fatalf("report = %+v", report)
	}
	if len(report.ProductApplyResults) != 1 || len(report.ProductApplyResults[0].SignalIDs) != 1 || len(report.ProductApplyResults[0].TransitionIDs) != 1 {
		t.Fatalf("apply results = %+v", report.ProductApplyResults)
	}
	after, err := product.BuildLoopModel(homePath, product.LoopOptions{ProductID: "product:focus-notes", Now: cliTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if sourceObserveBatchSignalCount(after, "gsc_clicks") != sourceObserveBatchSignalCount(before, "gsc_clicks")+1 {
		t.Fatalf("product loops did not gain gsc_clicks signal before=%+v after=%+v", before.Products, after.Products)
	}
}

func TestSourceObserveBatchCLIAppliesGeneratedSourceObservation(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/generated-system-source.json", "--apply-source-observations", "--json"})
	})
	var report observation.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse source-observation apply report: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.DryRun || !report.AppliedSourceObservations || report.AppliedProductSignals {
		t.Fatalf("source observation apply report = %+v", report)
	}
	if len(report.SourceObservationApplyResults) != 1 {
		t.Fatalf("source observation results = %+v", report.SourceObservationApplyResults)
	}
	result := report.SourceObservationApplyResults[0]
	if result.ObservationID != "obs_fixture_generated_workbench" || result.SourceObservationID == "" || len(result.CandidateIDs) != 1 || len(result.NoPromoteIDs) != 1 {
		t.Fatalf("source observation result = %+v", result)
	}
	candidatePath := filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade), result.CandidateIDs[0]+".json")
	if _, err := os.Stat(candidatePath); err != nil {
		t.Fatalf("expected sourceobs candidate %s: %v", candidatePath, err)
	}
	noPromoteRows := readNoPromoteDecisionRows(t, homePath)
	if len(noPromoteRows) != 1 || noPromoteRows[0].CandidateRef != result.CandidateIDs[0] || noPromoteRows[0].OwnerTarget.Ref != result.SourceObservationID {
		t.Fatalf("no-promote rows = %+v result=%+v", noPromoteRows, result)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("source observation apply must not create product signals ledger: %v", err)
	}
}

func TestSourceIntakeCLIAndOrientConsumeRecordedObservationBatches(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(homePath, store, bundle, "fixtures/product/market-loop.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		cmdPanel([]string{"import-lenses", "../../fixtures/panel/lens-manifest.yaml"})
	})

	for _, args := range [][]string{
		{"observe-batch", "../../fixtures/observation/gsc-product-metric.json", "--apply-product-signals", "--json"},
		{"observe-batch", "../../fixtures/observation/content-knowledge-export.json", "--apply-knowledge", "--json"},
		{"observe-batch", "../../fixtures/observation/generated-system-source.json", "--apply-source-observations", "--json"},
		{"observe-batch", "../../fixtures/observation/route-evidence.json", "--apply-route-evidence", "--json"},
		{"observe-batch", "../../fixtures/observation/event-trace-envelope.json", "--apply-events", "--json"},
		{"observe-batch", "../../fixtures/observation/social-distribution-feedback.json", "--review-candidates", "--json"},
	} {
		captureStdout(t, func() {
			cmdSource(args)
		})
	}

	out := captureStdout(t, func() {
		cmdSource([]string{"intake", "--json"})
	})
	var projection observation.SourceIntakeProjection
	if err := json.Unmarshal([]byte(out), &projection); err != nil {
		t.Fatalf("parse source intake projection: %v\n%s", err, out)
	}
	if projection.Status != "ok" || projection.RecordCount != 6 || projection.RowCount < 6 {
		t.Fatalf("projection = %+v", projection)
	}
	for _, route := range []string{observation.RouteProductSignal, observation.RouteKnowledgeExport, observation.RouteSourceObservation, observation.RouteRouteEvidence, observation.RouteTraceEnvelope, observation.RouteCandidateOnly} {
		if projection.Counts.ByRoute[route] == 0 {
			t.Fatalf("route %s missing from source intake projection: %+v", route, projection.Counts.ByRoute)
		}
	}
	productRow := findCLISourceIntakeRow(projection.Rows, observation.RouteProductSignal)
	if productRow == nil || productRow.Owner != "internal/product" || productRow.SubjectID != "product:focus-notes" || productRow.SourceCapturedAt == "" || len(productRow.AppliedIDs) == 0 {
		t.Fatalf("product intake row = %+v", productRow)
	}
	if !stringSliceContains(productRow.NextCommand, "loops") {
		t.Fatalf("product intake next command = %+v", productRow.NextCommand)
	}
	knowledgeRow := findCLISourceIntakeRow(projection.Rows, observation.RouteKnowledgeExport)
	if knowledgeRow == nil || knowledgeRow.Owner != "internal/knowledge" || !stringSliceContainsPrefix(knowledgeRow.AppliedIDs, "extraction:") {
		t.Fatalf("knowledge intake row = %+v", knowledgeRow)
	}
	extractionCard := findCLISourceIntakeActionCard(projection.ActionCards, "knowledge_extraction_pending")
	if extractionCard == nil ||
		extractionCard.RowID != knowledgeRow.ID ||
		!stringSliceContains(extractionCard.Command, "knowledge") ||
		!stringSliceContains(extractionCard.Command, "extract") ||
		!stringSliceContains(extractionCard.Command, "--item") ||
		stringSliceContains(extractionCard.Command, "--pending") ||
		!strings.Contains(extractionCard.Boundary, "no automatic memory promotion") {
		t.Fatalf("knowledge extraction source-intake card = %+v", extractionCard)
	}
	eventRow := findCLISourceIntakeRow(projection.Rows, observation.RouteTraceEnvelope)
	if eventRow == nil || eventRow.Owner != "internal/eventsource" || len(eventRow.AppliedIDs) == 0 {
		t.Fatalf("event intake row = %+v", eventRow)
	}

	orientOut := captureStdout(t, func() {
		cmdOrient([]string{"--operator", "--json", "--recent", "3"})
	})
	var orient orientOperatorReport
	if err := json.Unmarshal([]byte(orientOut), &orient); err != nil {
		t.Fatalf("parse orient operator json: %v\n%s", err, orientOut)
	}
	if orient.NeedsMe.SourceIntake == nil || orient.NeedsMe.SourceIntake.RowCount < 6 || orient.NeedsMe.SourceIntake.AppliedCount == 0 || orient.NeedsMe.SourceIntake.ReviewRecordedCount == 0 {
		t.Fatalf("orient source intake = %+v", orient.NeedsMe.SourceIntake)
	}
	if len(orient.NeedsMe.SourceIntake.Commands) == 0 || !stringSliceContains(orient.NeedsMe.SourceIntake.Commands[0].Command, "intake") {
		t.Fatalf("orient source intake commands = %+v", orient.NeedsMe.SourceIntake.Commands)
	}
	orientExtractionCard := findCLISourceIntakeActionCard(orient.NeedsMe.SourceIntake.ActionCards, "knowledge_extraction_pending")
	if orientExtractionCard == nil || orientExtractionCard.RowID != knowledgeRow.ID || !stringSliceContains(orientExtractionCard.Command, "--item") {
		t.Fatalf("orient source intake missing knowledge extraction card: %+v", orient.NeedsMe.SourceIntake.ActionCards)
	}
}

// freshGSCBatchFixture rewrites the batch fixture's absolute timestamps to
// now-1h. The connections projection derives staleness from wall-clock (72h),
// so a fixture with fixed dates time-bombs this test the day it ages past the
// threshold — fresh applied evidence must deterministically read "observed".
func freshGSCBatchFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../fixtures/observation/gsc-product-metric.json")
	if err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	body := strings.ReplaceAll(string(raw), "2026-07-04T12:00:00Z", fresh)
	body = strings.ReplaceAll(body, "2026-07-04T00:00:00Z", fresh)
	path := filepath.Join(t.TempDir(), "gsc-product-metric-fresh.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSourceConnectionsCLIShowsKnownAndObservedSources(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(homePath, store, bundle, "fixtures/product/market-loop.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		cmdSource([]string{"observe-batch", freshGSCBatchFixture(t), "--apply-product-signals", "--json"})
	})

	out := captureStdout(t, func() {
		cmdSource([]string{"connections", "--json"})
	})
	var projection observation.SourceConnectionsProjection
	if err := json.Unmarshal([]byte(out), &projection); err != nil {
		t.Fatalf("parse source connections projection: %v\n%s", err, out)
	}
	if projection.Status != "ok" || projection.Counts.Total < 8 {
		t.Fatalf("projection = %+v", projection)
	}
	if projection.Counts.Observed == 0 || projection.Counts.Connected != 0 {
		t.Fatalf("source connection counts = %+v, want historical rows observed and no live connected proof", projection.Counts)
	}
	if projection.Counts.EvidenceObserved == 0 || projection.Counts.AvailabilityHealthy != 0 {
		t.Fatalf("split source state counts = %+v, want observed evidence and no healthy live connection", projection.Counts)
	}
	textOut := captureStdout(t, func() {
		cmdSource([]string{"connections"})
	})
	wantAvailabilityLine := "- availability: unknown=" + strconv.Itoa(projection.Counts.AvailabilityUnknown) +
		" not_configured=" + strconv.Itoa(projection.Counts.AvailabilityNotConfigured) +
		" needs_private_config=" + strconv.Itoa(projection.Counts.AvailabilityNeedsPrivateConfig) +
		" configured=" + strconv.Itoa(projection.Counts.AvailabilityConfigured) +
		" healthy=" + strconv.Itoa(projection.Counts.AvailabilityHealthy) +
		" failing=" + strconv.Itoa(projection.Counts.AvailabilityFailing)
	wantEvidenceLine := "- evidence_state: never_seen=" + strconv.Itoa(projection.Counts.EvidenceNeverSeen) +
		" observed=" + strconv.Itoa(projection.Counts.EvidenceObserved) +
		" imported=" + strconv.Itoa(projection.Counts.EvidenceImported) +
		" stale=" + strconv.Itoa(projection.Counts.EvidenceStale) +
		" has_cards=" + strconv.Itoa(projection.Counts.EvidenceHasCards)
	if !strings.Contains(textOut, wantAvailabilityLine) || !strings.Contains(textOut, wantEvidenceLine) {
		t.Fatalf("source connections text output missing split state counts:\nwant %q\nwant %q\ngot:\n%s", wantAvailabilityLine, wantEvidenceLine, textOut)
	}
	if strings.Contains(textOut, " status=") {
		t.Fatalf("source connections text output still prints legacy connection status:\n%s", textOut)
	}
	gsc := findCLISourceConnection(projection.Connections, "google_search_console")
	if gsc == nil ||
		gsc.Status != "observed" ||
		gsc.Availability != "unknown" ||
		gsc.EvidenceState != "observed" ||
		gsc.RowCount == 0 ||
		!stringSliceContains(gsc.Produces, "product signal") {
		t.Fatalf("gsc connection = %+v", gsc)
	}
	asc := findCLISourceConnection(projection.Connections, "asc_daily_signals")
	if asc == nil ||
		asc.Status != "blocked_private_config" ||
		asc.Availability != "needs_private_config" ||
		asc.EvidenceState != "never_seen" ||
		!asc.PrivateSetupNeeded ||
		asc.NextSafeAction == "" {
		t.Fatalf("asc connection = %+v", asc)
	}
	githubStars := findCLISourceConnection(projection.Connections, "github_stars")
	if githubStars == nil ||
		githubStars.Status != "not_connected" ||
		githubStars.Availability != "not_configured" ||
		githubStars.EvidenceState != "never_seen" {
		t.Fatalf("github stars connection = %+v", githubStars)
	}
}

func TestSessionsMineDryRunCLIReportsProofRequirements(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdSessions([]string{"mine", "--since", "24h", "--dry-run", "--json"})
	})
	var report sessionMiningReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse sessions mine report: %v\n%s", err, out)
	}
	if report.Status != "ok" || !report.DryRun || report.Since != "24h" {
		t.Fatalf("report = %+v", report)
	}
	if len(report.ProofRequired) == 0 || !stringSliceContains(report.ProofRequired, "dry-run only; no session import, no memory promotion, no skill promotion") {
		t.Fatalf("proof requirements = %+v", report.ProofRequired)
	}
	if len(report.NextCommand) == 0 || !stringSliceContains(report.NextCommand, "sessions") {
		t.Fatalf("next command = %+v", report.NextCommand)
	}
}

func TestSourceIntakeCLIAndOrientExposeActionCards(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(homePath, store, bundle, "fixtures/product/market-loop.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}

	value := 1.0
	stale := observation.Batch{
		SchemaVersion: observation.BatchSchemaVersion,
		Source: observation.Source{
			Kind:       "google_search_console",
			Ref:        "site:https://example.invalid",
			CapturedAt: cliTestNow.Add(-96 * time.Hour).Format(time.RFC3339),
			Collector:  "fixture-gsc-edge",
			Privacy:    "synthetic",
		},
		Subject: observation.Subject{Kind: "product", ID: "product:focus-notes", Surface: "search", Channel: "organic_search"},
		Observations: []observation.Observation{{
			ID:         "obs_cli_stale_clicks",
			Kind:       "metric",
			Metric:     "clicks",
			Value:      &value,
			ObservedAt: cliTestNow.Add(-96 * time.Hour).Format(time.RFC3339),
			Confidence: 1,
		}},
	}
	stalePath := filepath.Join(homePath, "stale-observation.json")
	writeCLIJSONFixture(t, stalePath, stale)

	unsupported := observation.Batch{
		SchemaVersion: observation.BatchSchemaVersion,
		Source: observation.Source{
			Kind:       "fixture_payload",
			Ref:        "fixture:unsupported",
			CapturedAt: cliTestNow.Format(time.RFC3339),
			Collector:  "fixture-edge",
			Privacy:    "synthetic",
		},
		Subject: observation.Subject{Kind: "product", ID: "product:focus-notes"},
		Observations: []observation.Observation{{
			ID:         "obs_cli_unknown_payload",
			Kind:       "mystery",
			ObservedAt: cliTestNow.Format(time.RFC3339),
			Confidence: 0.5,
		}},
	}
	unsupportedPath := filepath.Join(homePath, "unsupported-observation.json")
	writeCLIJSONFixture(t, unsupportedPath, unsupported)

	captureStdout(t, func() {
		cmdSource([]string{"observe-batch", stalePath, "--apply-product-signals", "--json"})
	})
	captureStdout(t, func() {
		cmdSource([]string{"observe-batch", unsupportedPath, "--apply-product-signals", "--json"})
	})
	captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/social-distribution-feedback.json", "--apply-product-signals", "--json"})
	})

	out := captureStdout(t, func() {
		cmdSource([]string{"intake", "--json"})
	})
	var projection observation.SourceIntakeProjection
	if err := json.Unmarshal([]byte(out), &projection); err != nil {
		t.Fatalf("parse source intake projection: %v\n%s", err, out)
	}
	if projection.ActionCardCount < 3 || projection.Counts.ActionRequired < 3 {
		t.Fatalf("source intake action cards missing: counts=%+v cards=%+v", projection.Counts, projection.ActionCards)
	}
	for _, kind := range []string{"stale_source", "skipped_route", "unsupported_kind"} {
		card := findCLISourceIntakeActionCard(projection.ActionCards, kind)
		if card == nil || card.Title == "" || len(card.Command) == 0 || len(card.RequiredInputs) == 0 || len(card.ExpectedValidation) == 0 || len(card.StopConditions) == 0 || card.Boundary == "" || card.Proof == "" {
			t.Fatalf("source intake action card %s incomplete: %+v", kind, card)
		}
	}

	orientOut := captureStdout(t, func() {
		cmdOrient([]string{"--operator", "--json", "--recent", "3"})
	})
	var orient orientOperatorReport
	if err := json.Unmarshal([]byte(orientOut), &orient); err != nil {
		t.Fatalf("parse orient operator json: %v\n%s", err, orientOut)
	}
	if orient.NeedsMe.SourceIntake == nil || orient.NeedsMe.SourceIntake.ActionCardCount < 3 || len(orient.NeedsMe.SourceIntake.ActionCards) == 0 {
		t.Fatalf("orient source-intake action cards missing: %+v", orient.NeedsMe.SourceIntake)
	}
}

func TestSourceIntakeWorkPlanCLIAdmitsActionCardThroughWorkbenchPreview(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(homePath, store, bundle, "fixtures/product/market-loop.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/social-distribution-feedback.json", "--apply-product-signals", "--json"})
	})
	intakeOut := captureStdout(t, func() {
		cmdSource([]string{"intake", "--json"})
	})
	var projection observation.SourceIntakeProjection
	if err := json.Unmarshal([]byte(intakeOut), &projection); err != nil {
		t.Fatalf("parse source intake projection: %v\n%s", err, intakeOut)
	}
	card := findCLISourceIntakeActionCard(projection.ActionCards, "skipped_route")
	if card == nil {
		t.Fatalf("missing skipped source-intake card: %+v", projection.ActionCards)
	}

	out := captureStdout(t, func() {
		cmdSource([]string{"intake-work-plan", "--card", card.ID, "--json"})
	})
	var report observation.SourceIntakeWorkPlanReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse source intake work-plan: %v\n%s", err, out)
	}
	if report.Status != "recorded" || report.SourceIntake.CardID != card.ID || report.WorkPlan.WorkID == "" || report.WorkPlan.PlanPath == "" || report.WorkPlan.WorkState != "proposed" {
		t.Fatalf("source intake work-plan report = %+v", report)
	}
	if report.WorkPlan.ActionContext == nil ||
		report.WorkPlan.ActionContext.Kind != "source_intake_action_card" ||
		report.WorkPlan.ActionContext.SourceID != card.ID ||
		!sameStringSlice(report.WorkPlan.ActionContext.Command, card.Command) ||
		report.WorkPlan.ActionContext.Proof != card.Proof ||
		report.WorkPlan.ActionContext.Boundary != card.Boundary {
		t.Fatalf("source intake work-plan action context mismatch: ctx=%+v card=%+v", report.WorkPlan.ActionContext, card)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Get(report.WorkPlan.WorkID); err == nil {
		t.Fatalf("intake-work-plan should not admit work %s", report.WorkPlan.WorkID)
	}
	workbenchJSON := captureStdout(t, func() {
		cmdWorkbench([]string{"--json"})
	})
	var wb workbenchCLIOutput
	if err := json.Unmarshal([]byte(workbenchJSON), &wb); err != nil {
		t.Fatalf("parse workbench: %v\n%s", err, workbenchJSON)
	}
	if wb.Counts.AdmissionPreviews != 1 || len(wb.AdmissionPreviews) != 1 || wb.AdmissionPreviews[0].WorkID != report.WorkPlan.WorkID || wb.AdmissionPreviews[0].PlanPath == "" {
		t.Fatalf("workbench should expose source-intake admission preview: counts=%+v previews=%+v", wb.Counts, wb.AdmissionPreviews)
	}
	workbenchText := captureStdout(t, func() {
		cmdWorkbench(nil)
	})
	if !strings.Contains(workbenchText, "admission_preview: "+report.WorkPlan.WorkID) ||
		!strings.Contains(workbenchText, "staleness=fresh") ||
		!strings.Contains(workbenchText, "admit: compound work admit-plan --plan "+report.WorkPlan.PlanPath) ||
		!strings.Contains(workbenchText, "action_context: "+strings.Join(card.Command, " ")) {
		t.Fatalf("workbench text missing source-intake action context:\n%s", workbenchText)
	}
	admitOut := captureStdout(t, func() {
		cmdWork([]string{"admit-plan", "--plan", report.WorkPlan.PlanPath, "--by", "operator-test", "--json"})
	})
	var admitReport work.AdmitCreatePlanReport
	if err := json.Unmarshal([]byte(admitOut), &admitReport); err != nil {
		t.Fatalf("parse admit-plan: %v\n%s", err, admitOut)
	}
	if admitReport.Status != "admitted" || admitReport.ActionContext == nil || admitReport.ActionContext.SourceID != card.ID {
		t.Fatalf("admit-plan did not preserve source-intake action context: %+v", admitReport)
	}
}

func TestWorkReviewCardPlanCLIAdmitsCardThroughWorkbenchPreview(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	held, err := workStore.Create(work.CreateInput{
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "held",
		SuccessPredicate: "held app-store-connect ASC daily signals need private config before scheduler",
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	cardID := "operator-gate-" + held.ID + "-app-store-connect-private-config"

	out := captureStdout(t, func() {
		cmdWork([]string{"review-card-plan", "--card", cardID, "--actor", "codex", "--json"})
	})
	var report work.ReviewCardWorkPlanReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse review-card work-plan: %v\n%s", err, out)
	}
	if report.Status != "recorded" ||
		report.ReviewCard.CardID != cardID ||
		report.ReviewCard.CardKind != "source_private_config_gate" ||
		report.WorkPlan.WorkID == "" ||
		report.WorkPlan.PlanPath == "" ||
		report.WorkPlan.ActionContext == nil ||
		report.WorkPlan.ActionContext.Kind != "workbench_review_card" ||
		report.WorkPlan.ActionContext.SourceID != cardID ||
		!sameStringSlice(report.WorkPlan.ActionContext.Command, report.ReviewCard.ActionCommand) ||
		!stringSliceContains(report.WorkPlan.ActionContext.StopConditions, "requested action would fetch live ASC data, apply ProductSignal, load scheduler, or clear held work") {
		t.Fatalf("review-card work-plan did not preserve gate context: %+v", report)
	}
	if _, err := workStore.Get(report.WorkPlan.WorkID); err == nil {
		t.Fatalf("review-card-plan should not admit work %s", report.WorkPlan.WorkID)
	}
	sourceWork, err := workStore.Get(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceWork.State != "held" {
		t.Fatalf("review-card-plan should not clear source held work: %s", sourceWork.State)
	}

	workbenchJSON := captureStdout(t, func() {
		cmdWorkbench([]string{"--json"})
	})
	var wb workbenchCLIOutput
	if err := json.Unmarshal([]byte(workbenchJSON), &wb); err != nil {
		t.Fatalf("parse workbench: %v\n%s", err, workbenchJSON)
	}
	if wb.Counts.AdmissionPreviews != 1 ||
		len(wb.AdmissionPreviews) != 1 ||
		wb.AdmissionPreviews[0].WorkID != report.WorkPlan.WorkID ||
		wb.AdmissionPreviews[0].ActionContext == nil ||
		wb.AdmissionPreviews[0].ActionContext.Kind != "workbench_review_card" ||
		!wb.AdmissionPreviews[0].ActionContext.RequiresOperator ||
		wb.AdmissionPreviews[0].ActionSummary != report.ReviewCard.CardTitle ||
		!wb.AdmissionPreviews[0].RequiresOperator ||
		wb.AdmissionPreviews[0].AdmissionBlockedReason == "" ||
		len(wb.AdmissionPreviews[0].AdmitCommand) != 0 {
		t.Fatalf("workbench should expose review-card admission preview: counts=%+v previews=%+v", wb.Counts, wb.AdmissionPreviews)
	}
	workbenchText := captureStdout(t, func() {
		cmdWorkbench(nil)
	})
	if !strings.Contains(workbenchText, "admission_preview: "+report.WorkPlan.WorkID) ||
		!strings.Contains(workbenchText, "operator_required: true") ||
		!strings.Contains(workbenchText, "blocked_reason: operator required inputs") ||
		!strings.Contains(workbenchText, "action_context: "+strings.Join(report.ReviewCard.ActionCommand, " ")) ||
		strings.Contains(workbenchText, "admit: compound work admit-plan --plan "+report.WorkPlan.PlanPath) {
		t.Fatalf("workbench text missing review-card action context:\n%s", workbenchText)
	}

	admitOut := captureStdout(t, func() {
		cmdWork([]string{"admit-plan", "--plan", report.WorkPlan.PlanPath, "--by", "operator-test", "--json"})
	})
	var admitReport work.AdmitCreatePlanReport
	if err := json.Unmarshal([]byte(admitOut), &admitReport); err != nil {
		t.Fatalf("parse admit-plan: %v\n%s", err, admitOut)
	}
	if admitReport.Status != "admitted" ||
		admitReport.ActionContext == nil ||
		admitReport.ActionContext.SourceID != cardID ||
		!sameStringSlice(admitReport.ActionContext.Command, report.ReviewCard.ActionCommand) {
		t.Fatalf("admit-plan did not preserve review-card action context: %+v", admitReport)
	}
	sourceAfterAdmit, err := workStore.Get(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfterAdmit.State != "held" {
		t.Fatalf("admitting review-card proposal should not clear source held work: %s", sourceAfterAdmit.State)
	}
}

func TestSourceIntakeWorkPlanCLICarriesActionCardThroughActionAndClosePlan(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := product.Load("../../fixtures/product/market-loop.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Apply(homePath, store, bundle, "fixtures/product/market-loop.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/social-distribution-feedback.json", "--apply-product-signals", "--json"})
	})
	intakeOut := captureStdout(t, func() {
		cmdSource([]string{"intake", "--json"})
	})
	var projection observation.SourceIntakeProjection
	if err := json.Unmarshal([]byte(intakeOut), &projection); err != nil {
		t.Fatalf("parse source intake projection: %v\n%s", err, intakeOut)
	}
	card := findCLISourceIntakeActionCard(projection.ActionCards, "skipped_route")
	if card == nil {
		t.Fatalf("missing skipped source-intake card: %+v", projection.ActionCards)
	}

	workPlanOut := captureStdout(t, func() {
		cmdSource([]string{"intake-work-plan", "--card", card.ID, "--json"})
	})
	var report observation.SourceIntakeWorkPlanReport
	if err := json.Unmarshal([]byte(workPlanOut), &report); err != nil {
		t.Fatalf("parse source intake work-plan: %v\n%s", err, workPlanOut)
	}
	if report.WorkPlan.ActionContext == nil ||
		report.WorkPlan.ActionContext.Kind != "source_intake_action_card" ||
		report.WorkPlan.ActionContext.SourceID != card.ID ||
		!sameStringSlice(report.WorkPlan.ActionContext.Command, card.Command) ||
		report.WorkPlan.ActionContext.Proof != card.Proof {
		t.Fatalf("source-intake work plan did not preserve card action context: ctx=%+v card=%+v", report.WorkPlan.ActionContext, card)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	admitOut := captureStdout(t, func() {
		cmdWork([]string{"admit-plan", "--plan", report.WorkPlan.PlanPath, "--by", "operator-test", "--json"})
	})
	var admitReport work.AdmitCreatePlanReport
	if err := json.Unmarshal([]byte(admitOut), &admitReport); err != nil {
		t.Fatalf("parse admit-plan: %v\n%s", err, admitOut)
	}
	if admitReport.Status != "admitted" || admitReport.ActionContext == nil || admitReport.ActionContext.SourceID != card.ID {
		t.Fatalf("admit-plan did not preserve source-intake action context: %+v", admitReport)
	}

	advancePlanJSON := captureStdout(t, func() {
		cmdWork([]string{"advance-plan", report.WorkPlan.WorkID, "--to", "ready", "--actor", "codex", "--dry-run", "--json"})
	})
	var advancePlan work.AdvancePlan
	if err := json.Unmarshal([]byte(advancePlanJSON), &advancePlan); err != nil {
		t.Fatalf("parse advance-plan: %v\n%s", err, advancePlanJSON)
	}
	if advancePlan.ActionContext == nil ||
		!sameStringSlice(advancePlan.ActionContext.Command, card.Command) ||
		advancePlan.ActionContext.Boundary != card.Boundary ||
		advancePlan.ActionContext.Proof != card.Proof {
		t.Fatalf("advance-plan JSON missing source-intake action context: %+v", advancePlan.ActionContext)
	}
	captureStdout(t, func() {
		cmdWork([]string{"advance", report.WorkPlan.WorkID, "--to", "ready", "--by", "codex"})
	})
	claimPlanJSON := captureStdout(t, func() {
		cmdWork([]string{"claim-plan", report.WorkPlan.WorkID, "--actor", "codex", "--dry-run", "--json"})
	})
	var claimPlan work.ClaimPlan
	if err := json.Unmarshal([]byte(claimPlanJSON), &claimPlan); err != nil {
		t.Fatalf("parse claim-plan: %v\n%s", err, claimPlanJSON)
	}
	if claimPlan.ActionContext == nil ||
		!sameStringSlice(claimPlan.ActionContext.Command, card.Command) ||
		claimPlan.ActionContext.Boundary != card.Boundary ||
		claimPlan.ActionContext.Proof != card.Proof {
		t.Fatalf("claim-plan JSON missing source-intake action context: %+v", claimPlan.ActionContext)
	}
	captureStdout(t, func() {
		cmdWork([]string{"claim", report.WorkPlan.WorkID, "--by", "codex"})
	})
	captureStdout(t, func() {
		cmdWork([]string{"advance", report.WorkPlan.WorkID, "--to", "running", "--by", "codex"})
	})
	actionPlanJSON := captureStdout(t, func() {
		cmdWork([]string{"action-plan", report.WorkPlan.WorkID, "--actor", "codex", "--json"})
	})
	var actionPlan work.ActionExecutionPlan
	if err := json.Unmarshal([]byte(actionPlanJSON), &actionPlan); err != nil {
		t.Fatalf("parse action-plan: %v\n%s", err, actionPlanJSON)
	}
	if actionPlan.Status != "action_execution_policy_allowed" ||
		actionPlan.PlanPath == "" ||
		actionPlan.ActionContext == nil ||
		!sameStringSlice(actionPlan.ActionCommand, card.Command) ||
		!sameStringSlice(actionPlan.RequiredInputs, card.RequiredInputs) ||
		!sameStringSlice(actionPlan.ExpectedValidation, card.ExpectedValidation) ||
		actionPlan.Proof != card.Proof ||
		actionPlan.ReceiptDraft.ID == "" ||
		actionPlan.ActionIntent.Target.ID != card.ID ||
		actionPlan.ActionIntent.Target.Path != report.WorkPlan.ActionContext.SourceRef {
		t.Fatalf("action-plan did not preserve receipt-ready source-intake context: %+v", actionPlan)
	}
	closePlanBeforeAct := captureStdout(t, func() {
		cmdWork([]string{"close-plan", report.WorkPlan.WorkID, "--action-plan", actionPlan.PlanPath, "--actor", "codex"})
	})
	if !strings.Contains(closePlanBeforeAct, "no_action: action execution plan") ||
		!strings.Contains(closePlanBeforeAct, "not recorded") {
		t.Fatalf("close-plan should refuse action plan before act --plan records it:\n%s", closePlanBeforeAct)
	}

	if len(card.Command) < 3 || card.Command[0] != "compound" || card.Command[1] != "source" {
		t.Fatalf("source-intake card command is not a compound source command: %+v", card.Command)
	}
	captureStdout(t, func() {
		cmdSource(card.Command[2:])
	})
	afterCardCommandOut := captureStdout(t, func() {
		cmdSource([]string{"intake", "--json"})
	})
	var afterCardCommand observation.SourceIntakeProjection
	if err := json.Unmarshal([]byte(afterCardCommandOut), &afterCardCommand); err != nil {
		t.Fatalf("parse source intake after card command: %v\n%s", err, afterCardCommandOut)
	}
	reviewedRow := findCLISourceIntakeRowByObservationStatus(afterCardCommand.Rows, card.ObservationID, "review_recorded")
	if reviewedRow == nil || reviewedRow.Owner != "internal/observation" || !stringSliceContainsPrefix(reviewedRow.AppliedIDs, "review:") {
		t.Fatalf("source-intake projection did not expose review-recorded result after card command: %+v", afterCardCommand.Rows)
	}
	oldRow := findCLISourceIntakeRowByID(afterCardCommand.Rows, card.RowID)
	if oldRow == nil || oldRow.Status != "skipped" || !oldRow.ActionRequired {
		t.Fatalf("source-intake projection should keep original skipped row explicit: %+v", oldRow)
	}

	actOut := captureStdout(t, func() {
		cmdWork([]string{"act", report.WorkPlan.WorkID, "--plan", actionPlan.PlanPath})
	})
	if !strings.Contains(actOut, "action "+actionPlan.ActionIntent.ID+" recorded") {
		t.Fatalf("act --plan did not record planned action:\n%s", actOut)
	}
	afterAct, err := workStore.Get(report.WorkPlan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	// Since the exact-approval handoff landing (2026-09-18), act --plan
	// re-evaluates policy at act time (store.EvaluateAndRecordAction) instead
	// of trusting the plan file's pre-made decision: the plan intent still
	// threads verbatim, but the recorded decision is the fresh evaluation —
	// the same decision the CLI prints on the summary line.
	if afterAct.State != "running" ||
		!stringSliceContains(afterAct.ActionIntentIDs, actionPlan.ActionIntent.ID) ||
		!strings.HasPrefix(afterAct.PolicyDecisionID, "pol_") ||
		!strings.Contains(actOut, "decision "+afterAct.PolicyDecisionID) {
		t.Fatalf("act --plan did not thread source-intake action intent with a fresh act-time decision: %+v", afterAct)
	}
	closePlanJSON := captureStdout(t, func() {
		cmdWork([]string{"close-plan", report.WorkPlan.WorkID, "--action-plan", actionPlan.PlanPath, "--actor", "codex", "--json"})
	})
	var closePlan work.ClosePlan
	if err := json.Unmarshal([]byte(closePlanJSON), &closePlan); err != nil {
		t.Fatalf("parse close-plan: %v\n%s", err, closePlanJSON)
	}
	if closePlan.Status != "close_policy_allowed" ||
		closePlan.ActionExecutionPlanID != actionPlan.ID ||
		closePlan.ReceiptDraft.ID != actionPlan.ReceiptDraft.ID ||
		!stringSliceContains(closePlan.ProofRefs, report.WorkPlan.ActionContext.SourceRef) ||
		!stringSliceContains(closePlan.ExpectedValidation, card.ExpectedValidation[0]) {
		t.Fatalf("close-plan JSON missing source-intake receipt/proof context: %+v", closePlan)
	}
}

func TestSourceObserveBatchCLIAppliesContentToKnowledge(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/content-knowledge-export.json", "--apply-knowledge", "--json"})
	})
	var report observation.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse knowledge apply report: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.DryRun || !report.AppliedKnowledge || report.AppliedProductSignals || report.AppliedSourceObservations {
		t.Fatalf("knowledge apply report = %+v", report)
	}
	if len(report.KnowledgeApplyResults) != 1 {
		t.Fatalf("knowledge apply results = %+v", report.KnowledgeApplyResults)
	}
	result := report.KnowledgeApplyResults[0]
	if result.ObservationID != "obs_fixture_content_knowledge" || len(result.EvidenceIDs) != 1 || len(result.CandidateIDs) == 0 || len(result.ExtractionIDs) != 1 || result.Owner != "internal/knowledge" {
		t.Fatalf("knowledge result = %+v", result)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].SourceKind != "web_capture" || !strings.Contains(evidence[0].QuoteText, "citable evidence") {
		t.Fatalf("knowledge evidence = %+v", evidence)
	}
	var extraction knowledge.ExtractionRequest
	extractionPath := filepath.Join(homePath, "inbox", "knowledge-extractions", result.ExtractionIDs[0]+".json")
	if err := json.Unmarshal([]byte(readFile(t, extractionPath)), &extraction); err != nil {
		t.Fatalf("parse extraction %s: %v", extractionPath, err)
	}
	if extraction.State != knowledge.ExtractionStatePending || extraction.EvidenceRef != result.EvidenceIDs[0] || extraction.SourceKind != "web_capture" {
		t.Fatalf("knowledge extraction = %+v", extraction)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("knowledge apply must not create product signals ledger: %v", err)
	}
}

func TestSourceObserveBatchCLIAppliesRouteEvidence(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/route-evidence.json", "--apply-route-evidence", "--json"})
	})
	var report observation.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse observe-batch report: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.DryRun || !report.AppliedRouteEvidence || report.AppliedProductSignals || report.AppliedKnowledge {
		t.Fatalf("route evidence apply report = %+v", report)
	}
	if len(report.RouteEvidenceApplyResults) != 1 {
		t.Fatalf("route evidence apply results = %+v", report.RouteEvidenceApplyResults)
	}
	result := report.RouteEvidenceApplyResults[0]
	if result.ObservationID != "obs_fixture_route_evidence" || result.RouteEvidenceID == "" || result.TransitionID == "" || result.Owner != "internal/routeevidence" {
		t.Fatalf("route evidence result = %+v", result)
	}
	ev, ok, err := routeevidence.LookupRequestID(homePath, "req_fixture_observation_route")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || ev.ID != result.RouteEvidenceID || ev.TransitionID != result.TransitionID {
		t.Fatalf("route evidence lookup = %+v ok=%t result=%+v", ev, ok, result)
	}
	if ev.SelectedTarget.Provider != "fixture-provider" || ev.SelectedTarget.Model != "fixture-model" || ev.CostMicros != 2400 || ev.Usage.TotalTokens != 150 {
		t.Fatalf("route evidence = %+v", ev)
	}
	if !testEventExists(t, homePath, result.TransitionID) {
		t.Fatal("route transition missing")
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("route evidence apply must not create product signals ledger: %v", err)
	}
}

func TestSourceObserveBatchCLIAppliesEventsToTraceEnvelope(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/event-trace-envelope.json", "--apply-events", "--json"})
	})
	var report observation.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse observe-batch report: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.DryRun || !report.AppliedEvents || report.AppliedRouteEvidence || report.AppliedProductSignals || report.AppliedKnowledge || report.AppliedSourceObservations {
		t.Fatalf("event apply report = %+v", report)
	}
	if len(report.EventApplyResults) != 1 {
		t.Fatalf("event apply results = %+v", report.EventApplyResults)
	}
	result := report.EventApplyResults[0]
	if len(result.ObservationIDs) != 1 || result.ObservationIDs[0] != "obs_fixture_event_trace" || len(result.TraceIDs) != 1 || result.SourceAuthorityID == "" || result.Owner != "internal/eventsource" {
		t.Fatalf("event result = %+v", result)
	}
	authority, err := eventsource.LoadAuthority(homePath, 5)
	if err != nil {
		t.Fatal(err)
	}
	if authority.Status != "ok" || authority.TraceCount != 1 || len(authority.SourceAuthority) != 1 || authority.SourceAuthority[0].ID != result.SourceAuthorityID {
		t.Fatalf("event authority = %+v result=%+v", authority, result)
	}
	if !testEventExists(t, homePath, result.TraceIDs[0]) {
		t.Fatal("trace envelope missing")
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("event apply must not create product signals ledger: %v", err)
	}
}

func TestSourceObserveBatchCLIRecordsCandidateReviewMaterial(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	captureStdout(t, func() {
		cmdRoute([]string{"import-model-capabilities", filepath.Join("..", "..", "fixtures", "panel", "model-capabilities.yaml")})
	})
	captureStdout(t, func() {
		cmdPanel([]string{"import-lenses", filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")})
	})

	out := captureStdout(t, func() {
		cmdSource([]string{"observe-batch", "../../fixtures/observation/social-distribution-feedback.json", "--review-candidates", "--json"})
	})
	var report observation.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse review-candidates report: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.DryRun || !report.ReviewCandidates || len(report.CandidateReviewResults) != 1 {
		t.Fatalf("review-candidates report = %+v", report)
	}
	result := report.CandidateReviewResults[0]
	if result.CandidateID == "" || result.ReviewPath == "" || result.ReviewYAMLPath == "" {
		t.Fatalf("candidate review result missing paths: %+v", result)
	}

	var candidate struct {
		ID           string   `json:"id"`
		Kind         string   `json:"kind"`
		Subject      string   `json:"subject"`
		State        string   `json:"state"`
		EvidenceRefs []string `json:"evidence_refs"`
	}
	if err := candidates.ReadCandidate(homePath, candidates.CapabilityObs, result.CandidateID, &candidate); err != nil {
		t.Fatalf("read observation candidate: %v", err)
	}
	if candidate.Kind != string(candidates.CapabilityObs) || candidate.State != "needs_review" || !strings.Contains(candidate.Subject, "product:") {
		t.Fatalf("candidate = %+v", candidate)
	}
	if !stringSliceContains(candidate.EvidenceRefs, "observation:"+result.ObservationID) {
		t.Fatalf("candidate evidence refs = %+v, want observation ref", candidate.EvidenceRefs)
	}

	reviewsOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", observation.ObservationReviewArea, "--json"})
	})
	var queue reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(reviewsOut), &queue); err != nil {
		t.Fatalf("parse observation review queue: %v\n%s", err, reviewsOut)
	}
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 || len(queue.Items) != 1 {
		t.Fatalf("observation review queue should expose actionable decision card: %+v", queue)
	}
	if queue.Counts.ReviewGroupCount != 1 || len(queue.Groups) != 1 || queue.Groups[0].Count != 1 {
		t.Fatalf("observation review queue should expose one decision group: %+v", queue.Groups)
	}
	card := queue.Items[0].DecisionCard
	if card == nil || card.Meta["target_kind"] != "capability_observation" || card.Meta["candidate_id"] != result.CandidateID {
		t.Fatalf("decision card = %+v", card)
	}
	if len(queue.Items[0].Actions) != 1 || !strings.Contains(queue.Items[0].Actions[0].Command, "compound panel plan --target-kind capability_observation") {
		t.Fatalf("queue actions = %+v", queue.Items[0].Actions)
	}

	lensesOut := captureStdout(t, func() {
		cmdPanel([]string{"lenses", "--target-kind", "capability_observation", "--domain", "social-distribution", "--task", "distribution feedback", "--json"})
	})
	var selection panel.LensSelection
	if err := json.Unmarshal([]byte(lensesOut), &selection); err != nil {
		t.Fatalf("parse lens selection: %v\n%s", err, lensesOut)
	}
	if !panelSelectionIncludes(selection, "lens:social-distribution-mechanics") {
		t.Fatalf("social observation did not select social distribution lens: %+v", selection)
	}

	planOut := captureStdout(t, func() {
		cmdPanel([]string{"plan", "--target-kind", "capability_observation", "--target", result.CandidateID, "--domain", "social-distribution", "--task", "distribution feedback", "--not-privacy-sensitive", "synthetic observation fixture", "--max-items", "1", "--json"})
	})
	var planned panel.Run
	if err := json.Unmarshal([]byte(planOut), &planned); err != nil {
		t.Fatalf("parse observation panel plan: %v\n%s", err, planOut)
	}
	if planned.Status != "planned" || len(planned.Targets) != 1 || planned.Targets[0].Ref != result.CandidateID {
		t.Fatalf("observation panel plan = %+v", planned)
	}
	if !panelRunIncludesLens(planned, "lens:social-distribution-mechanics") {
		t.Fatalf("observation panel plan missing social-distribution lens: %+v", planned.LensSet)
	}

	fixtureDir := writeObservationPanelFixtureResponses(t, planned, result.CandidateID)
	runOut := captureStdout(t, func() {
		cmdPanel([]string{"run", "--plan", planned.ID, "--fixtures", fixtureDir, "--json"})
	})
	var completed panel.Run
	if err := json.Unmarshal([]byte(runOut), &completed); err != nil {
		t.Fatalf("parse observation panel run: %v\n%s", err, runOut)
	}
	if completed.Status != "verdicts_recorded" || len(completed.Findings) != 1 {
		t.Fatalf("observation panel run = %+v", completed)
	}
	finding := completed.Findings[0]
	if finding.TargetRef != result.CandidateID || finding.Synthesis.Decision != "park" || finding.VerdictArtifact == "" {
		t.Fatalf("observation panel finding = %+v", finding)
	}
	verdictRaw, err := os.ReadFile(finding.VerdictArtifact)
	if err != nil {
		t.Fatalf("read observation panel verdict artifact: %v", err)
	}
	if !strings.Contains(string(verdictRaw), "compound.panel_verdict.v0.1") || !strings.Contains(string(verdictRaw), result.CandidateID) {
		t.Fatalf("observation panel verdict artifact did not bind target/schema:\n%s", verdictRaw)
	}
	var verdictArtifact struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(verdictRaw, &verdictArtifact); err != nil {
		t.Fatalf("parse observation panel verdict artifact: %v", err)
	}

	verdictQueueOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", "panel-verdicts", "--json"})
	})
	var verdictQueue reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(verdictQueueOut), &verdictQueue); err != nil {
		t.Fatalf("parse panel verdict queue: %v\n%s", err, verdictQueueOut)
	}
	if verdictQueue.Counts.ReviewRequired != 1 || verdictQueue.Counts.DecisionCardCount != 1 || len(verdictQueue.Items) != 1 {
		t.Fatalf("panel verdict queue should expose reviewable observation verdict: %+v", verdictQueue)
	}
	if verdictQueue.Counts.ReviewGroupCount != 1 || len(verdictQueue.Groups) != 1 || verdictQueue.Groups[0].Count != 1 {
		t.Fatalf("panel verdict queue should expose grouped review decision: %+v", verdictQueue.Groups)
	}
	if verdictQueue.Items[0].DecisionCard == nil || !strings.Contains(verdictQueue.Items[0].NextAction, "owner review required") {
		t.Fatalf("panel verdict queue item should be reviewable: %+v", verdictQueue.Items[0])
	}
	productTargetPath := filepath.Join(t.TempDir(), "product-loop.md")
	if err := os.WriteFile(productTargetPath, []byte("# Product loop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	productPlanOut := captureStdout(t, func() {
		cmdProduct([]string{"observation-verdict-plan", "--verdict", finding.VerdictArtifact, "--target", productTargetPath})
	})
	if !strings.Contains(productPlanOut, "# product observation-verdict-plan") ||
		!strings.Contains(productPlanOut, "- product: product:focus-notes") ||
		!strings.Contains(productPlanOut, "- verdict: "+verdictArtifact.ID) ||
		!strings.Contains(productPlanOut, "- review_yaml:") {
		t.Fatalf("product observation-verdict-plan output missing proof lines:\n%s", productPlanOut)
	}
	productReviewYAML := outputValue(productPlanOut, "- review_yaml:")
	if productReviewYAML == "" {
		t.Fatalf("product observation-verdict-plan did not print review YAML:\n%s", productPlanOut)
	}
	productQueue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: product.ObservationVerdictReviewArea}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	product.AnnotateReviewQueue(&productQueue)
	reviewartifact.Recount(&productQueue)
	if productQueue.Counts.ReviewRequired != 1 || productQueue.Counts.DecisionCardCount != 1 || productQueue.Counts.ActionableCount != 1 {
		t.Fatalf("product observation verdict queue should expose decision card: %+v", productQueue.Counts)
	}
	if productQueue.Items[0].DecisionCard == nil || productQueue.Items[0].Actions[0].ID != "record_product_observation_verdict_review" {
		t.Fatalf("product observation verdict queue item should be actionable: %+v", productQueue.Items[0])
	}
	reviewYAML := readFile(t, productReviewYAML)
	reviewYAML = strings.ReplaceAll(reviewYAML, "decision: TODO_decision_create_hypothesis_candidate_park_reject", "decision: create_hypothesis_candidate")
	reviewYAML = strings.ReplaceAll(reviewYAML, "rationale: TODO_rationale", "rationale: reviewed observation feedback is product evidence, not ProductSignal")
	reviewYAML = strings.ReplaceAll(reviewYAML, "confidence: TODO_confidence_0_to_1", "confidence: 0.74")
	reviewYAML = strings.ReplaceAll(reviewYAML, "expected_effect: TODO_expected_effect", "expected_effect: export-support feedback should improve distribution reply quality")
	if err := os.WriteFile(productReviewYAML, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	productReviewOut := captureStdout(t, func() {
		cmdProduct([]string{"observation-verdict-review", productReviewYAML})
	})
	if !strings.Contains(productReviewOut, "# product observation-verdict-review") ||
		!strings.Contains(productReviewOut, "- decision: create_hypothesis_candidate") ||
		!strings.Contains(productReviewOut, "- hypothesis:") ||
		!strings.Contains(productReviewOut, "- transition:") {
		t.Fatalf("product observation-verdict-review output missing proof lines:\n%s", productReviewOut)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("product observation verdict review must not mutate ProductSignal ledger: %v", err)
	}
}

func TestSourceObservationVerdictCLIRecordsNoPromoteEvidence(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	obs := sourceobs.SourceObservation{
		SchemaVersion: sourceobs.SchemaVersion,
		ID:            "srcobs_cli_generated_view",
		AdapterID:     "fixture.generated-view",
		SourceSystem:  "fixture-system",
		ObservedAt:    cliTestNow.Format(time.RFC3339),
		SourceRef: sourceobs.SourceRef{
			Path:        "fixtures/generated/system-view.json",
			Kind:        "generated_view",
			Owner:       "fixture-system",
			SourceClass: "generated",
			Revision:    "sha256:cli-source-view",
		},
		GeneratedView: sourceobs.GeneratedView{
			ViewKind:  "system_view",
			Consumer:  "compound source observation verdict test",
			Generator: "fixture",
			Marker:    "generated",
			Freshness: "fresh",
		},
		MapsTo: []sourceobs.MapTarget{{
			ObjectFamily:   "capability",
			CandidateKind:  string(candidates.CapabilityObs),
			EntityHint:     "system-view",
			PromoteAllowed: false,
		}},
		EvidenceRefs: []string{"fixture:cli-source-observation"},
	}
	sourceReport, err := sourceobs.Apply(homePath, obs, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceReport.CandidateIDs) != 1 {
		t.Fatalf("source observation candidate ids = %+v", sourceReport.CandidateIDs)
	}
	var candidate sourceobs.CandidateRecord
	if err := candidates.ReadCandidate(homePath, candidates.CapabilityObs, sourceReport.CandidateIDs[0], &candidate); err != nil {
		t.Fatal(err)
	}
	verdictPath := writeCLISourceObservationVerdictFixture(t, homePath, "panel_verdict_cli_source", candidate)
	planOut := captureStdout(t, func() {
		cmdSource([]string{"observation-verdict-plan", "--verdict", verdictPath})
	})
	if !strings.Contains(planOut, "# source observation-verdict-plan") || !strings.Contains(planOut, "- owner_status: sourceobs") || !strings.Contains(planOut, "- candidate: "+candidate.ID) {
		t.Fatalf("source observation-verdict-plan output missing proof lines:\n%s", planOut)
	}
	reviewYAMLPath := outputValue(planOut, "- review_yaml:")
	if reviewYAMLPath == "" {
		t.Fatalf("source observation-verdict-plan did not print review YAML:\n%s", planOut)
	}
	queueOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", sourceobs.SourceObservationVerdictReviewArea, "--json"})
	})
	var queue reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(queueOut), &queue); err != nil {
		t.Fatalf("parse source observation verdict queue: %v\n%s", err, queueOut)
	}
	if sourceobs.SourceObservationVerdictReviewArea != "source-observation-verdicts" || queue.Area != sourceobs.SourceObservationVerdictReviewArea {
		t.Fatalf("source observation verdict review area mismatch: area=%q queue=%q", sourceobs.SourceObservationVerdictReviewArea, queue.Area)
	}
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 {
		t.Fatalf("source observation verdict queue should expose decision card: %+v", queue.Counts)
	}
	if queue.Items[0].DecisionCard == nil || queue.Items[0].Actions[0].ID != "record_source_observation_verdict_review" {
		t.Fatalf("source observation verdict queue item should be actionable: %+v", queue.Items[0])
	}
	reviewYAML := readFile(t, reviewYAMLPath)
	reviewYAML = strings.ReplaceAll(reviewYAML, "decision: TODO_decision_record_no_promote_park_reject", "decision: record_no_promote")
	reviewYAML = strings.ReplaceAll(reviewYAML, "rationale: TODO_rationale", "rationale: reviewed generated source verdict remains no-promote evidence")
	reviewYAML = strings.ReplaceAll(reviewYAML, "confidence: TODO_confidence_0_to_1", "confidence: 0.81")
	if err := os.WriteFile(reviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewOut := captureStdout(t, func() {
		cmdSource([]string{"observation-verdict-review", reviewYAMLPath})
	})
	if !strings.Contains(reviewOut, "# source observation-verdict-review") || !strings.Contains(reviewOut, "- decision: record_no_promote") || !strings.Contains(reviewOut, "- no_promote:") {
		t.Fatalf("source observation-verdict-review output missing proof lines:\n%s", reviewOut)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("source observation verdict review must not promote claims: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("source observation verdict review must not mutate product signals: %v", err)
	}
}

func writeObservationPanelFixtureResponses(t *testing.T, run panel.Run, targetRef string) string {
	t.Helper()
	fixtureDir := t.TempDir()
	digest := panelFixtureDigest(targetRef)
	judgeDir := filepath.Join(fixtureDir, "judge-responses")
	if err := os.MkdirAll(judgeDir, 0o755); err != nil {
		t.Fatalf("create judge fixture dir: %v", err)
	}
	judge := `{"decision":"park","confidence":0.74,"rationale":"Observation feedback is useful review material but should stay parked evidence until a product or source owner consumes it.","dissent":[]}`
	if err := os.WriteFile(filepath.Join(judgeDir, digest+".json"), []byte(judge), 0o644); err != nil {
		t.Fatalf("write judge fixture: %v", err)
	}
	for _, lens := range run.LensSet.Included {
		dir := filepath.Join(fixtureDir, "lens-responses", lens.Ref)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create lens fixture dir %s: %v", lens.Ref, err)
		}
		body := `{"confidence":0.81,"findings":[{"claim":"Candidate has bounded observation evidence and should feed review material without truth promotion.","severity":"low","evidence_refs":["observation_batch:fixture"]}]}`
		if err := os.WriteFile(filepath.Join(dir, digest+".json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write lens fixture %s: %v", lens.Ref, err)
		}
	}
	return fixtureDir
}

func writeCLISourceObservationVerdictFixture(t *testing.T, homePath, verdictID string, candidate sourceobs.CandidateRecord) string {
	t.Helper()
	dir := filepath.Join(homePath, "reviews", "panel-verdicts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             verdictID,
		"created_at":     cliTestNow.Format(time.RFC3339),
		"run_id":         "run_cli_source_observation",
		"target": map[string]any{
			"kind":       string(candidates.CapabilityObs),
			"ref":        candidate.ID,
			"visibility": "internal",
			"summary":    candidate.Statement,
			"payload":    json.RawMessage(raw),
		},
		"lens_findings": []any{},
		"synthesis": map[string]any{
			"schema_version":     "compound.panel_synthesis.v0.1",
			"decision":           "park",
			"confidence":         0.73,
			"rationale":          "Synthetic panel verdict keeps generated source observation as evidence.",
			"dissent":            []any{},
			"guardrails_applied": []string{"evidence_not_truth"},
		},
		"evidence_refs": []string{"route_evidence:cli-source-observation-verdict"},
	}
	data, err := json.MarshalIndent(body, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, verdictID+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func panelFixtureDigest(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])[:12]
}

func panelRunIncludesLens(run panel.Run, ref string) bool {
	for _, included := range run.LensSet.Included {
		if included.Ref == ref {
			return true
		}
	}
	return false
}

func sourceObserveBatchSignalCount(model product.LoopModel, metric string) int {
	if len(model.Products) == 0 {
		return 0
	}
	return len(model.Products[0].SignalsByMetric[metric])
}

func findCLISourceIntakeRow(rows []observation.SourceIntakeRow, route string) *observation.SourceIntakeRow {
	for i := range rows {
		if rows[i].Route == route {
			return &rows[i]
		}
	}
	return nil
}

func findCLISourceIntakeRowByID(rows []observation.SourceIntakeRow, id string) *observation.SourceIntakeRow {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}

func findCLISourceIntakeRowByObservationStatus(rows []observation.SourceIntakeRow, observationID, status string) *observation.SourceIntakeRow {
	for i := range rows {
		if rows[i].ObservationID == observationID && rows[i].Status == status {
			return &rows[i]
		}
	}
	return nil
}

func findCLISourceIntakeActionCard(cards []observation.SourceIntakeActionCard, kind string) *observation.SourceIntakeActionCard {
	for i := range cards {
		if cards[i].Kind == kind {
			return &cards[i]
		}
	}
	return nil
}

func findCLISourceConnection(connections []observation.SourceConnection, id string) *observation.SourceConnection {
	for i := range connections {
		if connections[i].ID == id {
			return &connections[i]
		}
	}
	return nil
}

func stringSliceContainsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func writeCLIJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeExtractCLIUsesFixtureAndWritesCandidate(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	var importReport knowledge.ImportReport
	importOut := captureStdout(t, func() {
		cmdKnowledge([]string{"import", filepath.Join("..", "..", "fixtures", "knowledge", "arxiv-export.json"), "--json"})
	})
	if err := json.Unmarshal([]byte(importOut), &importReport); err != nil {
		t.Fatalf("parse import report: %v\n%s", err, importOut)
	}
	if len(importReport.ExtractionIDs) == 0 || len(importReport.EvidenceIDs) == 0 {
		t.Fatalf("import report missing ids: %+v", importReport)
	}
	fixtureDir := t.TempDir()
	writeKnowledgeExtractionFixture(t, fixtureDir, importReport.ExtractionIDs[0], importReport.EvidenceIDs[0])

	var extractReport knowledge.ExtractReport
	extractOut := captureStdout(t, func() {
		cmdKnowledge([]string{"extract", "--item", importReport.ExtractionIDs[0], "--fixtures", fixtureDir, "--json"})
	})
	if err := json.Unmarshal([]byte(extractOut), &extractReport); err != nil {
		t.Fatalf("parse extract report: %v\n%s", err, extractOut)
	}
	if extractReport.Processed != 1 || extractReport.Created != 1 || len(extractReport.CandidateIDs) != 1 {
		t.Fatalf("extract report = %+v", extractReport)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range candidates {
		if c.ID == extractReport.CandidateIDs[0] {
			found = c.Qualifiers["source"] == "knowledge_extraction" && c.State == "parked" && strings.Contains(c.Reason, "knowledge extraction")
		}
	}
	if !found {
		t.Fatalf("extracted candidate not found or wrong shape: %s in %+v", extractReport.CandidateIDs[0], candidates)
	}
}

func writeKnowledgeExtractionFixture(t *testing.T, dir, extractionID, evidenceID string) {
	t.Helper()
	body := `{
 "schema_version": "compound.distiller.llm_extraction.v0.1",
 "trace_ref": "knowledge_extraction:` + extractionID + `",
 "extractor": "fixture",
 "model": "fixture-rules",
 "source_refs": [` + strconv.Quote(evidenceID) + `],
 "candidates": [
  {
   "kind": "memory",
   "entity": "paper:retrieval-agents",
   "predicate": "claim",
   "object": "Retrieval agents need grounded evidence to avoid echo amplification.",
   "confidence": 0.78,
   "derivation": "extracted",
   "claim_type": "architecture",
   "qualifiers": {"domain": "knowledge-intake"},
   "evidence_refs": [` + strconv.Quote(evidenceID) + `]
  }
 ]
}`
	if err := os.WriteFile(filepath.Join(dir, extractionID+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const knowledgeExtractPendingChildEnv = "COMPOUND_TEST_KNOWLEDGE_EXTRACT_PENDING_CHILD"

// TestKnowledgeExtractPendingPrintsReportEvenWhenItemsFail is the bulk-drain
// falsifier (envelope work_f10d5bffed0d): for a --pending batch on which at
// least one item fails, the CLI must still emit the JSON report (so a bulk
// drainer can see what did and did not get processed) before any non-zero exit.
// The current code path calls die(err) BEFORE printJSON(report), which
// suppresses the entire batch report — the normal partial-failure case for a
// dispatcher burning a queue. Uses the same child-re-exec pattern as
// TestHomeDoctorJSONFailureExitsNonZero because the in-process die/os.Exit
// would kill the test binary.
func TestKnowledgeExtractPendingPrintsReportEvenWhenItemsFail(t *testing.T) {
	if childHome := os.Getenv(knowledgeExtractPendingChildEnv); childHome != "" {
		t.Setenv("COMPOUND_HOME", childHome)
		fixturesDir := os.Getenv("COMPOUND_TEST_KNOWLEDGE_EXTRACT_PENDING_FIXTURES")
		cmdKnowledge([]string{"extract", "--pending", "--fixtures", fixturesDir, "--json"})
		os.Exit(0)
		return
	}

	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	var importReport knowledge.ImportReport
	importOut := captureStdout(t, func() {
		cmdKnowledge([]string{"import", filepath.Join("..", "..", "fixtures", "knowledge", "arxiv-export.json"), "--json"})
	})
	if err := json.Unmarshal([]byte(importOut), &importReport); err != nil {
		t.Fatalf("parse import report: %v\n%s", err, importOut)
	}
	if len(importReport.ExtractionIDs) < 2 {
		t.Fatalf("need at least 2 pending extractions to stage a bulk-drain partial failure, got %+v", importReport)
	}

	// Fixture for one item, MISSING fixtures for the rest → every other item
	// fails inside Extract (no fixture on disk), Extract returns both a
	// partially-populated report and a non-nil error. The CLI must still
	// print the report before exiting.
	fixturesDir := t.TempDir()
	writeKnowledgeExtractionFixture(t, fixturesDir, importReport.ExtractionIDs[0], importReport.EvidenceIDs[0])

	cmd := exec.Command(os.Args[0], "-test.run=^TestKnowledgeExtractPendingPrintsReportEvenWhenItemsFail$")
	cmd.Env = append(os.Environ(),
		knowledgeExtractPendingChildEnv+"="+homePath,
		"COMPOUND_TEST_KNOWLEDGE_EXTRACT_PENDING_FIXTURES="+fixturesDir,
	)
	var stdout, stderrBuf bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("partial-failure --pending batch must exit non-zero, got err=%v stdout:\n%s\nstderr:\n%s", err, stdout.String(), stderrBuf.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("partial-failure --pending batch must exit 1 (failed items present), got %d stdout:\n%s\nstderr:\n%s", exitErr.ExitCode(), stdout.String(), stderrBuf.String())
	}

	// The bulk-drain contract: the JSON report is the only authoritative
	// record of what was processed, on stdout. Even on a partial failure,
	// the report must be parseable from the JSON the CLI emitted BEFORE any
	// non-zero exit. The error narrative goes to stderr (the existing
	// drainer-friendly contract).
	if !strings.Contains(stderrBuf.String(), "knowledge extraction completed with") {
		t.Fatalf("stderr should carry the partial-failure error narrative, got:\n%s", stderrBuf.String())
	}
	var got knowledge.ExtractReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("bulk-drain JSON on stdout must be parseable on partial failure: %v\nstdout:\n%s", err, stdout.String())
	}
	if got.Processed < 1 {
		t.Fatalf("at least one item should have processed (the one with a fixture), got %+v", got)
	}
	if got.Failed < 1 {
		t.Fatalf("partial failure must be reflected as Failed>0 in the report, got %+v", got)
	}
	if len(got.ExtractionIDs) < 1 {
		t.Fatalf("report must record the processed extraction id(s), got %+v", got)
	}
}

func TestHomeOperationalHistoryArchiveCommandsHideAndRestoreHotHistory(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "ready work remains hot"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	done, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "terminal work archives"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(done.ID, "codex", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(done.ID, work.AdvanceInput{To: "running"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(done.ID, work.AdvanceInput{To: "verifying", VerificationID: "ver_cli_archive"}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Close(done.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "archive history CLI proof",
		Changed:   true,
		ProofRefs: []string{"proof:cli-archive"},
	}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	writeResidualLeaseForTerminalWork(t, homePath, done.ID, "codex", cliTestNow)
	if _, err := reviewartifact.Write(homePath, "panel-verdicts", "pv_cli_archive", map[string]any{
		"id":     "pv_cli_archive",
		"status": "recorded",
	}, " ", "# Verdict\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewartifact.WriteJSON(homePath, "panel-verdicts", "pv_cli_archive.reviewed.json", map[string]any{
		"id":     "pvr_cli_archive",
		"status": "recorded",
	}, " "); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdHome([]string{"doctor"}) })
	for _, want := range []string{
		"operational_hygiene: DIRTY",
		"hygiene:no-terminal-work-hot: DIRTY",
		"hygiene:no-stale-leases: DIRTY",
		"hygiene_action: archive-terminal-work",
		"hygiene_action: archive-stale-leases",
		"dry_run: compound home archive-history --kind work --dry-run",
		"dry_run: compound home archive-history --kind lease --dry-run",
		"verdict: ok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dirty home doctor missing %q:\n%s", want, out)
		}
	}

	out = captureStdout(t, func() { cmdHome([]string{"archive-history", "--help"}) })
	if !strings.Contains(out, "usage: compound home archive-history") || strings.Contains(out, "archived:") {
		t.Fatalf("archive-history --help should print usage only:\n%s", out)
	}
	activeArchive, err := home.LoadOperationalArchiveIndex(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(activeArchive) != 0 {
		t.Fatalf("archive-history --help mutated archive index: %+v", activeArchive)
	}
	if _, err := os.Stat(filepath.Join(homePath, "work", "work", done.ID+".json")); err != nil {
		t.Fatalf("archive-history --help moved terminal work: %v", err)
	}

	doctorJSON := captureStdout(t, func() { cmdHome([]string{"doctor", "--json"}) })
	var doctor struct {
		SchemaVersion      string `json:"schema_version"`
		OperationalHygiene struct {
			OK                   bool     `json:"ok"`
			TerminalHotWorkCount int      `json:"terminal_hot_work_count"`
			StaleLeaseCount      int      `json:"stale_lease_count"`
			TerminalHotWork      []string `json:"terminal_hot_work"`
			StaleLeases          []string `json:"stale_leases"`
		} `json:"operational_hygiene"`
		OperationalHygieneActions []struct {
			ID                 string   `json:"id"`
			Command            []string `json:"command"`
			DryRunCommand      []string `json:"dry_run_command"`
			ExpectedValidation []string `json:"expected_validation"`
			StopConditions     []string `json:"stop_conditions"`
			Boundary           string   `json:"boundary"`
			Proof              string   `json:"proof"`
		} `json:"operational_hygiene_actions"`
	}
	if err := json.Unmarshal([]byte(doctorJSON), &doctor); err != nil {
		t.Fatalf("home doctor --json should parse: %v\n%s", err, doctorJSON)
	}
	if doctor.SchemaVersion != "compound.home_doctor_report.v0.1" ||
		doctor.OperationalHygiene.OK ||
		doctor.OperationalHygiene.TerminalHotWorkCount != 1 ||
		doctor.OperationalHygiene.StaleLeaseCount != 1 ||
		len(doctor.OperationalHygieneActions) != 2 {
		t.Fatalf("home doctor --json missing hygiene debt/action cards: %+v", doctor)
	}
	actionByID := map[string][]string{}
	for _, action := range doctor.OperationalHygieneActions {
		actionByID[action.ID] = action.Command
		if len(action.DryRunCommand) == 0 || len(action.ExpectedValidation) == 0 || len(action.StopConditions) == 0 || action.Boundary == "" || action.Proof == "" {
			t.Fatalf("home doctor action missing gates: %+v", action)
		}
	}
	if !sameStringSlice(actionByID["archive-terminal-work"], []string{"compound", "home", "archive-history", "--kind", "work"}) ||
		!sameStringSlice(actionByID["archive-stale-leases"], []string{"compound", "home", "archive-history", "--kind", "lease"}) {
		t.Fatalf("home doctor --json actions route wrong commands: %+v", actionByID)
	}

	out = captureStdout(t, func() {
		cmdHome([]string{"archive-history", "--kind", "work", "--kind", "lease", "--kind", "review"})
	})
	if !strings.Contains(out, "# home archive-history") || !strings.Contains(out, "- work: 1") || !strings.Contains(out, "- lease: 1") || !strings.Contains(out, "- review: 3") {
		t.Fatalf("archive-history output:\n%s", out)
	}
	out = captureStdout(t, func() { cmdHome([]string{"doctor"}) })
	for _, want := range []string{
		"operational_hygiene: ok",
		"hygiene:no-terminal-work-hot: ok",
		"hygiene:no-stale-leases: ok",
		"verdict: ok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("swept home doctor missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(homePath, "work", "work", done.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("terminal work should move out of hot store, stat err=%v", err)
	}

	var wb workbenchCLIOutput
	out = captureStdout(t, func() { cmdWorkbench([]string{"--json", "--recent", "10", "--review-area", "panel-verdicts"}) })
	if err := json.Unmarshal([]byte(out), &wb); err != nil {
		t.Fatalf("parse default workbench: %v\n%s", err, out)
	}
	if wb.Counts.Terminal != 0 || wb.ReviewQueue == nil || wb.ReviewQueue.Counts.Total != 0 {
		t.Fatalf("default workbench should hide archived history: counts=%+v review=%+v", wb.Counts, wb.ReviewQueue)
	}

	var reviews reviewartifact.QueueReport
	out = captureStdout(t, func() { cmdReviews([]string{"--json", "--area", "panel-verdicts"}) })
	if err := json.Unmarshal([]byte(out), &reviews); err != nil {
		t.Fatalf("parse default reviews: %v\n%s", err, out)
	}
	if reviews.Counts.Total != 0 {
		t.Fatalf("default reviews should hide archived history: %+v", reviews.Counts)
	}

	out = captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--recent", "10", "--review-area", "panel-verdicts", "--include-archived"})
	})
	if err := json.Unmarshal([]byte(out), &wb); err != nil {
		t.Fatalf("parse archived workbench: %v\n%s", err, out)
	}
	if wb.Counts.Terminal != 1 || wb.ReviewQueue == nil || wb.ReviewQueue.Counts.Total != 1 {
		t.Fatalf("include-archived workbench should show history: counts=%+v review=%+v", wb.Counts, wb.ReviewQueue)
	}

	out = captureStdout(t, func() { cmdReviews([]string{"--json", "--area", "panel-verdicts", "--include-archived"}) })
	if err := json.Unmarshal([]byte(out), &reviews); err != nil {
		t.Fatalf("parse archived reviews: %v\n%s", err, out)
	}
	if reviews.Counts.Total != 1 || reviews.Items[0].ID != "pv_cli_archive" {
		t.Fatalf("include-archived reviews should show history: %+v", reviews)
	}

	out = captureStdout(t, func() { cmdHome([]string{"restore-history", "--all"}) })
	if !strings.Contains(out, "# home restore-history") || !strings.Contains(out, "- restored: 5") {
		t.Fatalf("restore-history output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(homePath, "work", "work", done.ID+".json")); err != nil {
		t.Fatalf("terminal work should restore to hot store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "reviews", "panel-verdicts", "pv_cli_archive.reviewed.json")); err != nil {
		t.Fatalf("review sidecar should restore to hot store: %v", err)
	}
}

// writeResidualLeaseForTerminalWork plants the stale-lease residue this test's dirty-home
// fixture needs. Close now releases the claim lease in the same publish that terminalizes
// the work, so closing no longer leaves residue behind and can no longer manufacture it.
// The two sources that still produce it are reproduced here directly: a home written before
// that change, and the crash window between the terminal publish and the release. Writing
// the residue explicitly keeps this test pointed at its actual subject — archive-history
// hiding and restoring stale-lease residue — instead of silently depending on close's
// lease behavior, which is a different test's job (internal/work).
func writeResidualLeaseForTerminalWork(t *testing.T, homePath, workID, by string, at time.Time) {
	t.Helper()
	blob, err := json.Marshal(&work.Lease{
		WorkID:  workID,
		By:      by,
		ClaimAt: strconv.FormatInt(at.UTC().UnixNano(), 10),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(homePath, "work", "leases", workID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOrientUsesRepoKindRegistry(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := os.MkdirAll(filepath.Join(homePath, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	controlRoot := t.TempDir()
	controlCwd := filepath.Join(controlRoot, "example-os")
	if err := os.MkdirAll(controlCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	productRoot := t.TempDir()
	productCwd := filepath.Join(productRoot, "ios-app")
	if err := os.MkdirAll(productCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := "schema_version: compound.repo_kind_registry.v0.1\nrepos:\n  - kind: control-plane\n    roots:\n      - " + controlRoot + "\n  - kind: product\n    roots:\n      - " + productRoot + "\n"
	if err := os.WriteFile(filepath.Join(homePath, "config", "repo-kinds.yaml"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}

	var control envelope.ContextPack
	out := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", controlCwd, "--task", "audit hooks"})
	})
	if err := json.Unmarshal([]byte(out), &control); err != nil {
		t.Fatalf("parse control orient: %v\n%s", err, out)
	}
	if !contextPackIncludes(control, "control-plane://workflow-contract") {
		t.Fatalf("control-plane pack should include workflow contract: %+v", control.Included)
	}
	if control.Uncertainty.Level != "known" {
		t.Fatalf("registry-resolved pack should be known, got %q", control.Uncertainty.Level)
	}

	var product envelope.ContextPack
	out = captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", productCwd, "--task", "prepare release"})
	})
	if err := json.Unmarshal([]byte(out), &product); err != nil {
		t.Fatalf("parse product orient: %v\n%s", err, out)
	}
	if contextPackIncludes(product, "control-plane://workflow-contract") {
		t.Fatalf("product pack should not include workflow contract: %+v", product.Included)
	}
	if !contextPackExcludes(product, "control-plane://workflow-contract") {
		t.Fatalf("product pack should record workflow contract exclusion: %+v", product.Excluded)
	}
}

func TestContractImportHooksFeedsCompile(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("HOME", homePath)
	source := filepath.Join("..", "..", "fixtures", "harness", "codex-hooks-json.json")
	if err := contract.StageSyntheticHookHandlers(homePath, source); err != nil {
		t.Fatalf("stage synthetic hook handlers: %v", err)
	}
	importOut := captureStdout(t, func() {
		cmdContract([]string{"import-hooks", "--codex-hooks-json", source})
	})
	for _, want := range []string{
		`"authority_ref": "compound://config/installed-hooks"`,
		`"hook_count": 3`,
		`"authority_status": "compound_primary"`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import-hooks output missing %q:\n%s", want, importOut)
		}
	}

	authorityOut := captureStdout(t, func() {
		cmdContract([]string{"hooks-authority"})
	})
	for _, want := range []string{
		`"installed": true`,
		`"hook_count": 3`,
		`"pre_tool_use_readiness": [`,
		`"harness": "codex"`,
		`"status": "non_blocking"`,
		`"source_role": "evidence_only_input_after_import"`,
	} {
		if !strings.Contains(authorityOut, want) {
			t.Fatalf("hooks-authority output missing %q:\n%s", want, authorityOut)
		}
	}

	cwd := t.TempDir()
	out := captureStdout(t, func() {
		cmdContract([]string{"compile", "--harness", "codex", "--cwd", cwd, "--task", "use hooks", "--repo-kind", "control-plane"})
	})
	var ec envelope.EffectiveContract
	if err := json.Unmarshal([]byte(out), &ec); err != nil {
		t.Fatalf("parse contract compile: %v\n%s", err, out)
	}
	for _, want := range []string{"hook://codex/SessionStart", "hook://codex/UserPromptSubmit"} {
		if !stringSliceContains(ec.ActiveHooks, want) {
			t.Fatalf("compiled active hooks = %v, want %s", ec.ActiveHooks, want)
		}
	}
	if !sourceRevisionExists(ec.SourceRevisions, "compound://config/installed-hooks") {
		t.Fatalf("compiled source revisions missing installed hooks authority: %+v", ec.SourceRevisions)
	}
	var latest envelope.EffectiveContract
	latestPath := filepath.Join(homePath, "contracts", "effective", "latest.json")
	latestData, err := os.ReadFile(latestPath)
	if err != nil {
		t.Fatalf("read latest effective contract: %v", err)
	}
	if err := json.Unmarshal(latestData, &latest); err != nil {
		t.Fatalf("parse latest effective contract: %v", err)
	}
	if latest.ID != ec.ID {
		t.Fatalf("latest effective contract id = %q, want %q", latest.ID, ec.ID)
	}
	createdAt, err := time.Parse(time.RFC3339, ec.CreatedAt)
	if err != nil {
		t.Fatalf("parse contract created_at %q: %v", ec.CreatedAt, err)
	}
	datedPath := filepath.Join(homePath, "contracts", "effective", createdAt.UTC().Format("2006-01-02"), ec.ID+".json")
	if _, err := os.Stat(datedPath); err != nil {
		t.Fatalf("dated effective contract artifact missing: %v", err)
	}

	orientOut := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "use hooks", "--repo-kind", "control-plane"})
	})
	var pack envelope.ContextPack
	if err := json.Unmarshal([]byte(orientOut), &pack); err != nil {
		t.Fatalf("parse orient pack: %v\n%s", err, orientOut)
	}
	if pack.IDs.ContractID == "" {
		t.Fatal("orient pack missing sealed contract_id")
	}
	orientUseHooksCreatedAt, err := time.Parse(time.RFC3339, pack.CreatedAt)
	if err != nil {
		t.Fatalf("parse orient created_at %q: %v", pack.CreatedAt, err)
	}
	orientUseHooksContractPath := filepath.Join(
		homePath,
		"contracts",
		"effective",
		orientUseHooksCreatedAt.UTC().Format("2006-01-02"),
		pack.IDs.ContractID+".json",
	)
	orientUseHooksContractData, err := os.ReadFile(orientUseHooksContractPath)
	if err != nil {
		t.Fatalf("read orient effective contract artifact: %v", err)
	}
	var orientUseHooksContract envelope.EffectiveContract
	if err := json.Unmarshal(orientUseHooksContractData, &orientUseHooksContract); err != nil {
		t.Fatalf("parse orient effective contract artifact: %v", err)
	}
	for _, want := range []string{"hook://codex/SessionStart", "hook://codex/UserPromptSubmit"} {
		if !stringSliceContains(orientUseHooksContract.ActiveHooks, want) {
			t.Fatalf("orient contract active hooks = %v, want %s", orientUseHooksContract.ActiveHooks, want)
		}
	}
	packPath := filepath.Join(homePath, "context", "packs", pack.ID+".json")
	packData, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatalf("read orient context pack artifact: %v", err)
	}
	var storedPack envelope.ContextPack
	if err := json.Unmarshal(packData, &storedPack); err != nil {
		t.Fatalf("parse orient context pack artifact: %v", err)
	}
	if storedPack.ID != pack.ID || storedPack.IDs.ContractID != pack.IDs.ContractID {
		t.Fatalf("stored orient pack id/contract = %q/%q, want %q/%q",
			storedPack.ID, storedPack.IDs.ContractID, pack.ID, pack.IDs.ContractID)
	}
	orientOnlyOut := captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--cwd", cwd, "--task", "orient persists contract without precompile", "--repo-kind", "control-plane"})
	})
	var orientOnlyPack envelope.ContextPack
	if err := json.Unmarshal([]byte(orientOnlyOut), &orientOnlyPack); err != nil {
		t.Fatalf("parse orient-only pack: %v\n%s", err, orientOnlyOut)
	}
	if orientOnlyPack.IDs.ContractID == "" {
		t.Fatal("orient-only pack missing contract_id")
	}
	orientCreatedAt, err := time.Parse(time.RFC3339, orientOnlyPack.CreatedAt)
	if err != nil {
		t.Fatalf("parse orient-only created_at %q: %v", orientOnlyPack.CreatedAt, err)
	}
	orientContractPath := filepath.Join(homePath, "contracts", "effective", orientCreatedAt.UTC().Format("2006-01-02"), orientOnlyPack.IDs.ContractID+".json")
	orientContractData, err := os.ReadFile(orientContractPath)
	if err != nil {
		t.Fatalf("read orient-only effective contract artifact: %v", err)
	}
	var orientContract envelope.EffectiveContract
	if err := json.Unmarshal(orientContractData, &orientContract); err != nil {
		t.Fatalf("parse orient-only effective contract artifact: %v", err)
	}
	if orientContract.ID != orientOnlyPack.IDs.ContractID {
		t.Fatalf("orient-only effective contract id = %q, want %q", orientContract.ID, orientOnlyPack.IDs.ContractID)
	}
}

func TestContractImportHookRegistrationFeedsCompile(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("HOME", homePath)
	source := filepath.Join("..", "..", "fixtures", "harness", "hook-registration.ts")
	if err := contract.StageSyntheticHookHandlers(homePath, source); err != nil {
		t.Fatalf("stage synthetic hook handlers: %v", err)
	}
	importOut := captureStdout(t, func() {
		cmdContract([]string{"import-hooks", "--hook-registration-ts", source})
	})
	captureStdout(t, func() {
		cmdContract([]string{"import-hooks", "--hook-registration-ts", source})
	})
	for _, want := range []string{
		`"authority_ref": "compound://config/installed-hooks"`,
		`"hook_count": 7`,
		`"imported_via": "contract import-hooks --hook-registration-ts"`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import-hooks output missing %q:\n%s", want, importOut)
		}
	}
	authorityOut := captureStdout(t, func() {
		cmdContract([]string{"hooks-authority"})
	})
	for _, want := range []string{
		`"harnesses": [`,
		`"claude_code"`,
		`"codex"`,
		`"hook_count": 7`,
		`"hook_readiness": [`,
		`"fixture_present": true`,
		`"live_adapter": true`,
		`"live_command": "compound hook session-start --live --harness claude_code"`,
	} {
		if !strings.Contains(authorityOut, want) {
			t.Fatalf("hooks-authority output missing %q:\n%s", want, authorityOut)
		}
	}
	var authorityReport struct {
		SourceRoleCounts            map[string]int                           `json:"source_role_counts"`
		RetiredSourceAuthorityCount int                                      `json:"retired_source_authority_count"`
		RetiredSourceAuthorityRefs  []string                                 `json:"retired_source_authority_refs"`
		SourceAuthority             []contract.InstalledHooksSourceAuthority `json:"source_authority"`
		PreToolUseReadiness         []contract.HookGateReadiness             `json:"pre_tool_use_readiness"`
		HookReadiness               []contract.InstalledHookRuntimeReadiness `json:"hook_readiness"`
	}
	if err := json.Unmarshal([]byte(authorityOut), &authorityReport); err != nil {
		t.Fatalf("parse hooks-authority: %v\n%s", err, authorityOut)
	}
	if len(authorityReport.SourceAuthority) != 1 ||
		authorityReport.SourceRoleCounts["evidence_only_input_after_import"] != 1 ||
		authorityReport.RetiredSourceAuthorityCount != 1 ||
		!stringSliceContains(authorityReport.RetiredSourceAuthorityRefs, "source_view_authority:"+source) {
		t.Fatalf("unexpected hooks authority source summary: %+v", authorityReport)
	}
	if got := findHookReadiness(authorityReport.HookReadiness, "claude_code", "SessionStart"); got == nil ||
		got.Status != "ready" || !got.Installed || !got.FixturePresent || !got.LiveAdapter {
		t.Fatalf("claude session-start readiness not ready: %+v", got)
	}
	if got := findHookReadiness(authorityReport.HookReadiness, "claude_code", "PreToolUse"); got == nil ||
		got.Status != "ready" || !strings.Contains(got.FixtureRef, "pre-tool-use.claude.json") {
		t.Fatalf("claude pre-tool-use readiness not ready: %+v", got)
	}
	if got := findHookReadiness(authorityReport.HookReadiness, "codex", "SessionStart"); got == nil ||
		got.Status != "ready" || !got.FixturePresent || !got.LiveAdapter || len(got.Actions) != 0 {
		t.Fatalf("codex session-start readiness not ready: %+v", got)
	}
	if got := findHookReadiness(authorityReport.HookReadiness, "codex", "UserPromptSubmit"); got == nil ||
		got.Status != "ready" || !got.FixturePresent || !got.LiveAdapter || len(got.Actions) != 0 {
		t.Fatalf("codex user-prompt-submit readiness not ready: %+v", got)
	}
	if got := findHookReadiness(authorityReport.HookReadiness, "codex", "PreToolUse"); got == nil ||
		got.Status != "ready" || !got.FixturePresent || !got.LiveAdapter || len(got.Actions) != 0 {
		t.Fatalf("codex pre-tool-use runtime readiness not ready: %+v", got)
	}
	if got := findHookReadiness(authorityReport.HookReadiness, "codex", "Stop"); got == nil ||
		got.Status != "ready" || !got.FixturePresent || !got.LiveAdapter || len(got.Actions) != 0 {
		t.Fatalf("codex Stop runtime readiness not ready: %+v", got)
	}
	if got := findHookGateReadiness(authorityReport.PreToolUseReadiness, "codex"); got == nil ||
		got.Status != "non_blocking" || !got.NativeBlock || got.Installed || got.AdapterRequired || len(got.Actions) != 0 {
		t.Fatalf("codex pre-tool-use observer should not report installed native enforcement: %+v", got)
	}
	out := captureStdout(t, func() {
		cmdContract([]string{"compile", "--harness", "claude_code", "--cwd", t.TempDir(), "--task", "use hooks", "--repo-kind", "control-plane"})
	})
	var ec envelope.EffectiveContract
	if err := json.Unmarshal([]byte(out), &ec); err != nil {
		t.Fatalf("parse contract compile: %v\n%s", err, out)
	}
	for _, want := range []string{"hook://claude_code/SessionStart", "hook://claude_code/UserPromptSubmit", "hook://claude_code/PreToolUse"} {
		if !stringSliceContains(ec.ActiveHooks, want) {
			t.Fatalf("compiled active hooks = %v, want %s", ec.ActiveHooks, want)
		}
	}
}

func TestUnavailableConcretePreToolUseFailsLoudAcrossAuthorityAndOrientation(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, handlerPath string)
	}{
		{name: "missing"},
		{
			name: "symlink",
			setup: func(t *testing.T, handlerPath string) {
				if err := os.MkdirAll(filepath.Dir(handlerPath), 0o755); err != nil {
					t.Fatal(err)
				}
				realPath := filepath.Join(filepath.Dir(handlerPath), "real-placement-guard.ts")
				if err := os.WriteFile(realPath, []byte("process.stdout.write('{}')\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(realPath, handlerPath); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			homePath := t.TempDir()
			harnessHome := t.TempDir()
			t.Setenv("COMPOUND_HOME", homePath)
			t.Setenv("HOME", harnessHome)
			handlerPath := filepath.Join(harnessHome, ".claude", "hooks", "PlacementGuard.hook.ts")
			if test.setup != nil {
				test.setup(t, handlerPath)
			}
			sourcePath := filepath.Join(t.TempDir(), "hook-registration.ts")
			source := `export const HOOK_REGISTRATIONS = [
  { harness: "claude", event: "PreToolUse", matcher: "Write|Edit", file: "PlacementGuard.hook.ts", command: ` + "`\"$HOME/.claude/hooks/PlacementGuard.hook.ts\"`" + ` },
];`
			if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}
			captureStdout(t, func() {
				cmdContract([]string{"import-hooks", "--hook-registration-ts", sourcePath})
			})

			authorityOut := captureStdout(t, func() {
				cmdContract([]string{"hooks-authority"})
			})
			var authority contract.HooksAuthorityReport
			if err := json.Unmarshal([]byte(authorityOut), &authority); err != nil {
				t.Fatalf("parse hooks-authority: %v\n%s", err, authorityOut)
			}
			gate := findHookGateReadiness(authority.PreToolUseReadiness, "claude_code")
			if gate == nil || gate.Status != "handler_unavailable" || gate.Status == "native_blocking" || len(gate.Actions) == 0 {
				t.Fatalf("unavailable gate readiness = %+v", gate)
			}
			runtime := findHookReadiness(authority.HookReadiness, "claude_code", "PreToolUse")
			if runtime == nil || runtime.Status != "handler_unavailable" || runtime.Status == "ready" || len(runtime.Actions) == 0 {
				t.Fatalf("unavailable runtime readiness = %+v", runtime)
			}

			in := contract.Input{
				Harness:     "claude_code",
				Cwd:         t.TempDir(),
				Task:        "edit source with guarded mutation",
				RepoKind:    "control-plane",
				RepoKindVia: "flag",
				Mode:        "execute",
			}
			ec, err := contract.CompileWithRuntime(in, contract.DefaultPolicy(), homePath, cliTestNow)
			if err != nil {
				t.Fatal(err)
			}
			assertUnavailableHookOrientation(t, ec.ActiveHooks, ec.Excluded, ec.HookRoster, ec.HookReadiness, nil)
			pack, err := contract.PackWithCapabilities(in, contract.DefaultPolicy(), homePath, cliTestNow)
			if err != nil {
				t.Fatal(err)
			}
			assertUnavailableHookOrientation(t, pack.ActiveHooks, pack.Excluded, pack.HookRoster, pack.HookReadiness, pack.RequiredChecks)
		})
	}
}

func assertUnavailableHookOrientation(t *testing.T, active []string, excluded []envelope.ExcludedRef, roster []envelope.HookRosterEntry, readiness []envelope.HookReadiness, requiredChecks []string) {
	t.Helper()
	ref := "hook://claude_code/PreToolUse"
	if stringSliceContains(active, ref) {
		t.Fatalf("unavailable hook surfaced active: %v", active)
	}
	foundExcluded := false
	for _, row := range excluded {
		if row.Ref == ref && strings.Contains(row.Reason, "unavailable") {
			foundExcluded = true
		}
	}
	if !foundExcluded {
		t.Fatalf("unavailable hook missing fail-loud exclusion: %+v", excluded)
	}
	foundRoster := false
	for _, row := range roster {
		if row.Ref == ref && row.Status == "handler_unavailable" && strings.Contains(row.Reason, "unavailable") {
			foundRoster = true
		}
	}
	if !foundRoster {
		t.Fatalf("unavailable hook missing roster gap: %+v", roster)
	}
	foundReadiness := false
	for _, row := range readiness {
		if row.Harness == "claude_code" && row.Hook == "PreToolUse" && row.Status == "handler_unavailable" && len(row.Actions) > 0 {
			foundReadiness = true
		}
	}
	if !foundReadiness {
		t.Fatalf("unavailable hook missing readiness remediation: %+v", readiness)
	}
	if requiredChecks != nil && !sliceContainsSubstring(requiredChecks, "status=handler_unavailable") {
		t.Fatalf("unavailable hook missing required check: %+v", requiredChecks)
	}
}

func sliceContainsSubstring(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func TestWorkImportPacketCreatesWorkEnvelopeWithoutClaims(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	out := captureStdout(t, func() {
		cmdWork([]string{"import-packet", packetPath})
	})
	if !strings.Contains(out, "created=true") || !strings.Contains(out, "Compound Goal Packet: Capability Intake") {
		t.Fatalf("import-packet output missing proof:\n%s", out)
	}
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("import-packet output missing JSON:\n%s", out)
	}
	var report work.PacketImportReport
	if err := json.Unmarshal([]byte(out[start:]), &report); err != nil {
		t.Fatalf("parse import-packet report: %v\n%s", err, out)
	}
	if report.Work.State != "proposed" || report.Work.Owner != "compound-core" {
		t.Fatalf("work state/owner = %s/%s", report.Work.State, report.Work.Owner)
	}
	if report.Work.Authority.SourceClass != "source" || report.Work.Uncertainty.Level != "inferred" {
		t.Fatalf("work authority/uncertainty = %+v / %+v", report.Work.Authority, report.Work.Uncertainty)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("packet import must not create claims ledger, stat err=%v", err)
	}
	out = captureStdout(t, func() {
		cmdWork([]string{"import-packet", packetPath})
	})
	if !strings.Contains(out, "created=false") {
		t.Fatalf("reimport should be idempotent:\n%s", out)
	}
}

func TestWorkCreateCLIWiresEnvelopeFields(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	writeCLIContextPackArtifact(t, homePath, "ctx_cli_create")

	out := captureStdout(t, func() {
		cmdWork([]string{
			"create",
			"--work-id", "work_cli_planned",
			"--owner", "compound-core",
			"--predicate", "continuation envelope preserves fields",
			"--outcome", "continue adaptive capability OS work",
			"--non-goal", "bulk clone,install",
			"--non-goal", "serve marketplace skills",
			"--evidence-ref", "compound://reviews/external-repo-intake",
			"--evidence", "brain://session-state",
			"--context-pack", "ctx_cli_create",
			"--state", "ready",
			"--risk", "tier2",
			"--privacy", "sensitive",
		})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("create output missing JSON:\n%s", out)
	}
	var w envelope.WorkEnvelope
	if err := json.Unmarshal([]byte(out[start:]), &w); err != nil {
		t.Fatalf("parse create work: %v\n%s", err, out)
	}
	if w.Owner != "compound-core" || w.State != "ready" || w.DesiredOutcome != "continue adaptive capability OS work" {
		t.Fatalf("work core fields not wired: %+v", w)
	}
	if w.ID != "work_cli_planned" {
		t.Fatalf("work id = %q, want explicit planned id", w.ID)
	}
	if w.Privacy != "sensitive" || w.Risk != "tier2" || w.IDs.ContextPackID != "ctx_cli_create" {
		t.Fatalf("header fields not wired: privacy=%q risk=%q context=%q", w.Privacy, w.Risk, w.IDs.ContextPackID)
	}
	wantNonGoals := []string{"bulk clone", "install", "serve marketplace skills"}
	if !sameStringSlice(w.NonGoals, wantNonGoals) {
		t.Fatalf("non_goals = %v, want %v", w.NonGoals, wantNonGoals)
	}
	wantEvidence := []string{"compound://reviews/external-repo-intake", "brain://session-state"}
	if !sameStringSlice(w.EvidenceRefs, wantEvidence) {
		t.Fatalf("evidence_refs = %v, want %v", w.EvidenceRefs, wantEvidence)
	}
}

func TestWorkCreatePlanCLIRecordsPolicyWithoutCreating(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	writeCLIContextPackArtifact(t, homePath, "ctx_cli_create_plan")

	out := captureStdout(t, func() {
		cmdWork([]string{
			"create-plan",
			"--owner", "compound-core",
			"--by", "codex",
			"--predicate", "cli create plan stays proposal-only",
			"--outcome", "show create gate",
			"--context-pack", "ctx_cli_create_plan",
			"--json",
		})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("create-plan output missing JSON:\n%s", out)
	}
	var plan work.CreatePlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse create plan: %v\n%s", err, out)
	}
	if plan.Status != "create_policy_allowed" || !plan.Policy.Allowed || plan.PlanPath == "" {
		t.Fatalf("create plan not allowed/recorded: %+v", plan)
	}
	if !stringSliceContains(plan.CreateCommand, "--work-id") || !stringSliceContains(plan.CreateCommand, plan.WorkID) {
		t.Fatalf("create plan command must preserve planned work id %s: %v", plan.WorkID, plan.CreateCommand)
	}
	if _, err := os.Stat(plan.PlanPath); err != nil {
		t.Fatalf("create plan artifact missing: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	if _, err := store.Get(plan.WorkID); err == nil {
		t.Fatalf("create-plan must not write work %s", plan.WorkID)
	}
}

func TestWorkCreatePlanCLIPreservesPlanClassAndReworkLineage(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	writeCLIContextPackArtifact(t, homePath, "ctx_cli_create_plan_lineage")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	earlier, err := store.Create(work.CreateInput{
		WorkID:           "work_cli_create_plan_earlier",
		Owner:            "compound-core",
		Actor:            "codex",
		SuccessPredicate: "the earlier work whose breakage requires rework",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create earlier work: %v", err)
	}

	out, runErr := runCLIFlagSeam(t, homePath,
		"work", "create-plan",
		"--owner", "compound-core",
		"--by", "codex",
		"--predicate", "planned rework preserves its admission contract",
		"--outcome", "repair the earlier work",
		"--context-pack", "ctx_cli_create_plan_lineage",
		"--plan-class", "phased",
		"--rework-of", earlier.ID,
		"--json",
	)
	if runErr != nil {
		t.Fatalf("create-plan must accept plan-class and rework lineage flags: %v\n%s", runErr, out)
	}
	start := strings.Index(string(out), "{")
	if start < 0 {
		t.Fatalf("create-plan output missing JSON:\n%s", out)
	}
	var plan work.CreatePlan
	if err := json.NewDecoder(strings.NewReader(string(out[start:]))).Decode(&plan); err != nil {
		t.Fatalf("parse create plan: %v\n%s", err, out)
	}
	if plan.Work.PlanClass != "phased" || plan.Work.ReworkOfWorkID != earlier.ID {
		t.Fatalf("planned work dropped admission contract: plan_class=%q rework_of=%q", plan.Work.PlanClass, plan.Work.ReworkOfWorkID)
	}
	if !landingCommandHasPair(plan.CreateCommand, "--plan-class", "phased") ||
		!landingCommandHasPair(plan.CreateCommand, "--rework-of", earlier.ID) {
		t.Fatalf("create command dropped admission contract: %v", plan.CreateCommand)
	}
}

func TestWorkImportPacketReadyClosesThroughReceiptTransition(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	writeCLIContextPackArtifact(t, homePath, "ctx_packet_cli")
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	out := captureStdout(t, func() {
		cmdWork([]string{"import-packet", packetPath, "--state", "ready", "--context-pack", "ctx_packet_cli"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("import-packet output missing JSON:\n%s", out)
	}
	var report work.PacketImportReport
	if err := json.Unmarshal([]byte(out[start:]), &report); err != nil {
		t.Fatalf("parse import-packet report: %v\n%s", err, out)
	}
	workID := report.Work.ID
	if report.Work.State != "ready" {
		t.Fatalf("packet work state = %q, want ready", report.Work.State)
	}
	captureStdout(t, func() {
		cmdWork([]string{"claim", workID, "--by", "codex"})
	})
	packetRunAttemptID := "ratt_20260720T000000Z_aabbccdd"
	if _, err := runattempt.Open(homePath).MintWithID(packetRunAttemptID, workID, "codex", "codex", cliTestNow); err != nil {
		t.Fatalf("mint packet CLI RunAttempt: %v", err)
	}
	captureStdout(t, func() {
		cmdWork([]string{"advance", workID, "--to", "running", "--run-attempt", packetRunAttemptID})
	})
	captureStdout(t, func() {
		cmdWork([]string{"advance", workID, "--to", "verifying", "--artifact", "artifact_packet_cli", "--verification", "ver_packet_cli"})
	})
	out = captureStdout(t, func() {
		cmdWork([]string{"close", workID, "--outcome", "completed", "--move-type", "control_plane_repair", "--summary", "packet CLI work closed through receipt and transition", "--changed"})
	})
	start = strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("close output missing JSON:\n%s", out)
	}
	var closed work.CloseResult
	if err := json.Unmarshal([]byte(out[start:]), &closed); err != nil {
		t.Fatalf("parse close result: %v\n%s", err, out)
	}
	if closed.Work.ID != workID || closed.Receipt.IDs.WorkID != workID || closed.Transition.IDs.WorkID != workID {
		t.Fatalf("work id chain broken: work=%s receipt=%s transition=%s", closed.Work.ID, closed.Receipt.IDs.WorkID, closed.Transition.IDs.WorkID)
	}
	if closed.Work.IDs.TransitionID != closed.Transition.ID {
		t.Fatalf("work transition id = %q, want %q", closed.Work.IDs.TransitionID, closed.Transition.ID)
	}
	if stringSliceContains(closed.Transition.ActionRefs, "run_packet_cli") {
		t.Fatalf("transition action_refs must not contain legacy run id after S1; got %v", closed.Transition.ActionRefs)
	}
	if !stringSliceContains(closed.Work.RunAttemptIDs, packetRunAttemptID) {
		t.Fatalf("work RunAttemptIDs missing the ratt_ id attached via --run-attempt; got %v", closed.Work.RunAttemptIDs)
	}
	if !stringSliceContains(closed.Transition.AfterRefs, "artifact_packet_cli") ||
		!stringSliceContains(closed.Transition.AfterRefs, closed.Receipt.ID) {
		t.Fatalf("transition refs incomplete: after=%v receipt=%s", closed.Transition.AfterRefs, closed.Receipt.ID)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("packet CLI loop must not create claims ledger, stat err=%v", err)
	}
}

func TestWorkbenchCommandShowsOpenAndTerminalProof(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "ready workbench proof"}, cliTestNow)
	if err != nil {
		t.Fatalf("create ready: %v", err)
	}
	done, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "closed workbench proof"}, cliTestNow)
	if err != nil {
		t.Fatalf("create done: %v", err)
	}
	if _, err := store.Claim(done.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim done: %v", err)
	}
	if _, err := store.Advance(done.ID, work.AdvanceInput{To: "running"}, cliTestNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.Advance(done.ID, work.AdvanceInput{To: "verifying", ArtifactID: "artifact_workbench", VerificationID: "ver_workbench"}, cliTestNow); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	closed, err := store.Close(done.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "workbench CLI terminal proof",
		Changed:   true,
		ProofRefs: []string{"proof:workbench-cli"},
	}, cliTestNow)
	if err != nil {
		t.Fatalf("close done: %v", err)
	}

	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--recent", "10"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("workbench output missing JSON:\n%s", out)
	}
	var report work.WorkbenchReport
	if err := json.Unmarshal([]byte(out[start:]), &report); err != nil {
		t.Fatalf("parse workbench report: %v\n%s", err, out)
	}
	if report.Counts.Open != 1 || report.Counts.Terminal != 1 || report.Counts.NeedsClaim != 1 {
		t.Fatalf("counts = %+v", report.Counts)
	}
	if len(report.Open) != 1 || report.Open[0].WorkID != ready.ID || report.Open[0].NextAction != "claim before touching" {
		t.Fatalf("open item = %+v", report.Open)
	}
	if !report.DispatchNext.CanRun || report.DispatchNext.Action != "claim" || report.DispatchNext.WorkID != ready.ID {
		t.Fatalf("dispatch next = %+v", report.DispatchNext)
	}
	if len(report.Recent) != 1 || report.Recent[0].ReceiptID != closed.Receipt.ID || report.Recent[0].TransitionID != closed.Transition.ID {
		t.Fatalf("recent item = %+v", report.Recent)
	}
}

func TestWorkbenchCommandCanAttachCapabilitySurface(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	captureStdout(t, func() {
		cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "harness-split.yaml")})
	})

	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--capability-task", "pricing"})
	})
	var report workbenchCLIOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("workbench JSON did not parse: %v\n%s", err, out)
	}
	if report.CapabilitySurface == nil {
		t.Fatalf("capability surface missing: %+v", report)
	}
	if report.Counts.Total != 0 || report.DispatchNext.WaitReason != "no_open_work" {
		t.Fatalf("workbench state changed unexpectedly: counts=%+v dispatch=%+v", report.Counts, report.DispatchNext)
	}
	surface := report.CapabilitySurface
	if surface.SchemaVersion != capgraph.SurfaceSchemaVersion ||
		surface.AuthorityRef != capgraph.CapabilityGraphAuthorityRef ||
		surface.Status != "ok" ||
		surface.Task != "pricing" ||
		surface.Harness != "codex" ||
		surface.OfferCount != len(surface.Offers) ||
		len(surface.OfferSummaries) != len(surface.Offers) {
		t.Fatalf("unexpected capability surface: %+v", surface)
	}
	var ids []string
	for _, offer := range surface.OfferSummaries {
		ids = append(ids, offer.ID)
	}
	if !stringSliceContains(ids, "skill:codex-pricing") || !stringSliceContains(ids, "skill:generic-pricing") {
		t.Fatalf("codex/generic offers missing: %+v", ids)
	}
	if stringSliceContains(ids, "skill:claude-pricing") {
		t.Fatalf("claude-only offer leaked into codex workbench surface: %+v", ids)
	}

	human := captureStdout(t, func() {
		cmdWorkbench([]string{"--capability-task", "pricing"})
	})
	for _, want := range []string{
		`capability_surface status=ok offers=2 showing=2 omitted=0 task="pricing" harness="codex" authority=compound://registries/capability-graph`,
		"capability_offer: skill skill:codex-pricing",
		"capability_offer: skill skill:generic-pricing",
		"open: none",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("human workbench output missing %q:\n%s", want, human)
		}
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("workbench capability surface must not promote claims, stat err=%v", err)
	}
}

func TestCapabilitySurfaceReportCarriesExternalRepoEvidenceWithoutOfferingIt(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(capgraph.Node{
		ID:       "lens:social-distribution-mechanics",
		Kind:     "lens",
		Name:     "Social Distribution Mechanics",
		Triggers: []string{"social", "distribution"},
		Weight:   1.1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(capgraph.Node{
		ID:       "procedure:external-repo-gitroomhq-postiz-app-workflow",
		Kind:     "procedure",
		Name:     "External Repo Gitroomhq Postiz App Workflow",
		Triggers: []string{"external", "repo", "gitroomhq", "postiz", "app", "workflow", "evaluate", "further"},
		Weight:   1,
		Retired:  true,
	}); err != nil {
		t.Fatal(err)
	}
	writeExternalRepoIntakeContextFixture(t, homePath)

	report, err := buildCapabilitySurfaceReport(store, homePath, "evaluate social media distribution outbound posting adapter with Postiz for product feedback loops", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !surfaceReportHasOffer(report, "lens:social-distribution-mechanics") {
		t.Fatalf("expected social distribution lens offer: %+v", report.Offers)
	}
	if surfaceReportHasOffer(report, "procedure:external-repo-gitroomhq-postiz-app-workflow") {
		t.Fatalf("needs-review external repo evidence must not become an offer: %+v", report.Offers)
	}
	if len(report.MatchedEvidence) != 1 {
		t.Fatalf("expected one matched external repo evidence row, got %+v", report.MatchedEvidence)
	}
	evidence := report.MatchedEvidence[0]
	if evidence.Kind != "external_repo_review_evidence" ||
		!strings.Contains(evidence.Ref, "gitroomhq/postiz-app") ||
		!strings.Contains(evidence.Detail, "product outbound distribute pipe") ||
		!strings.Contains(evidence.Detail, "not adoption") {
		t.Fatalf("matched evidence should preserve owner and boundary: %+v", evidence)
	}
}

func TestCapabilityOutboundDistributionPlanJSON(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	writeExternalRepoIntakeContextFixture(t, homePath)

	out := captureStdout(t, func() {
		cmdCapability([]string{"outbound-distribution-plan", "--task", "evaluate social media distribution outbound posting adapter with Postiz for product feedback loops", "--json"})
	})
	var report capgraph.OutboundDistributionPlan
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	if report.Status != "review_ready" || len(report.Candidates) != 1 {
		t.Fatalf("outbound distribution plan should expose one candidate: %+v", report)
	}
	candidate := report.Candidates[0]
	boundary := strings.ToLower(candidate.Boundary)
	if candidate.Repo != "gitroomhq/postiz-app" ||
		candidate.Role != "posting_adapter_candidate" ||
		!strings.Contains(boundary, "auth social") ||
		!strings.Contains(boundary, "auto-post") {
		t.Fatalf("candidate should preserve Postiz adapter boundary: %+v", candidate)
	}
	if !outboundPlanHasGate(report.RequiredGates, "tier3_public_distribution_policy") ||
		!outboundPlanHasGate(report.RequiredGates, "product_loop_target") ||
		report.AdoptionLevel != "evidence_only" {
		t.Fatalf("plan should stay evidence-only with Tier-3/product-loop gates: %+v", report)
	}
}

func TestCapabilityOutboundDistributionVerdictPlanAndReviewCLI(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	writeExternalRepoIntakeContextFixture(t, homePath)
	targetPath := filepath.Join(t.TempDir(), "product-loop.md")
	original := "# Product Loop\n\nExisting notes.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{
			"outbound-distribution-verdict-plan",
			"--task", "evaluate social media distribution outbound posting adapter with Postiz for product feedback loops",
			"--product", "product:focus-notes",
			"--change-event", "prepare outbound distribution dry-run for screenshot change",
			"--hypothesis", "rented posting edge can produce reviewable drafts without public posting",
			"--observation-window", "14d",
			"--dry-run-receipt", "receipt:outbound-dry-run-cli",
			"--target", targetPath,
			"--json",
		})
	})
	var created struct {
		SchemaVersion  string                                   `json:"schema_version"`
		Status         string                                   `json:"status"`
		Plan           capgraph.OutboundDistributionVerdictPlan `json:"plan"`
		PlanPath       string                                   `json:"plan_path"`
		ReviewPath     string                                   `json:"review_path"`
		ReviewYAMLPath string                                   `json:"review_yaml_path"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("parse verdict plan report: %v\n%s", err, out)
	}
	if created.Status != "review_required" ||
		created.Plan.ProductID != "product:focus-notes" ||
		created.Plan.AdoptionLevel != "verdict_only" ||
		len(created.Plan.Candidates) != 1 ||
		created.PlanPath == "" ||
		created.ReviewYAMLPath == "" {
		t.Fatalf("bad verdict plan CLI report: %+v", created)
	}
	for _, path := range []string{created.PlanPath, created.ReviewPath, created.ReviewYAMLPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing CLI artifact %s: %v", path, err)
		}
	}
	reviewsOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", capgraph.OutboundDistributionVerdictReviewArea, "--json"})
	})
	var reviews reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(reviewsOut), &reviews); err != nil {
		t.Fatalf("parse reviews: %v\n%s", err, reviewsOut)
	}
	if reviews.Counts.ReviewRequired != 1 ||
		reviews.Counts.DecisionCardCount != 1 ||
		reviews.Counts.ActionableCount != 1 ||
		reviews.Counts.ReviewRequiredMissingDecisionCard != 0 ||
		reviews.Counts.ReviewRequiredMissingAction != 0 {
		t.Fatalf("outbound verdict review should be actionable: %+v", reviews.Counts)
	}

	draft, err := os.ReadFile(created.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewYAML := strings.ReplaceAll(string(draft), "TODO_decision_adopt_for_dry_run_park_reject", "park")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_rationale", "park until product-specific distribution target is selected")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_confidence_0_to_1", "0.63")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_dry_run_receipt_ref", "receipt:outbound-dry-run-cli")
	if err := os.WriteFile(created.ReviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewOut := captureStdout(t, func() {
		cmdCapability([]string{"outbound-distribution-verdict-review", created.ReviewYAMLPath})
	})
	if !strings.Contains(reviewOut, "# capability outbound-distribution-verdict-review") ||
		!strings.Contains(reviewOut, "- status: recorded") ||
		!strings.Contains(reviewOut, "- decision: park") {
		t.Fatalf("review command output missing proof:\n%s", reviewOut)
	}
	reviewsOut = captureStdout(t, func() {
		cmdReviews([]string{"--area", capgraph.OutboundDistributionVerdictReviewArea, "--json"})
	})
	if err := json.Unmarshal([]byte(reviewsOut), &reviews); err != nil {
		t.Fatalf("parse terminal reviews: %v\n%s", err, reviewsOut)
	}
	if reviews.Counts.ReviewRequired != 0 || reviews.Counts.OK != 1 || reviews.Items[0].Readiness != "no_action" {
		t.Fatalf("recorded verdict should terminalize review item: counts=%+v item=%+v", reviews.Counts, reviews.Items[0])
	}
	if got, err := os.ReadFile(targetPath); err != nil || string(got) != original {
		t.Fatalf("target mutated err=%v:\n%s", err, got)
	}
}

func TestCapabilityOutboundDistributionDryRunReceiptCLI(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	writeExternalRepoIntakeContextFixture(t, homePath)
	targetPath := filepath.Join(t.TempDir(), "product-loop.md")
	if err := os.WriteFile(targetPath, []byte("# Product Loop\n\nExisting notes.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	planOut := captureStdout(t, func() {
		cmdCapability([]string{
			"outbound-distribution-verdict-plan",
			"--task", "evaluate social media distribution outbound posting adapter with Postiz product feedback loops",
			"--product", "product:focus-notes",
			"--change-event", "prepare outbound distribution dry-run screenshot change",
			"--hypothesis", "rented posting edge can produce reviewable drafts without public posting",
			"--observation-window", "14d",
			"--dry-run-receipt", "receipt:outbound-dry-run-cli",
			"--target", targetPath,
			"--json",
		})
	})
	var created struct {
		ReviewYAMLPath string `json:"review_yaml_path"`
	}
	if err := json.Unmarshal([]byte(planOut), &created); err != nil {
		t.Fatalf("parse verdict plan: %v\n%s", err, planOut)
	}
	draft, err := os.ReadFile(created.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewYAML := strings.ReplaceAll(string(draft), "TODO_decision_adopt_for_dry_run_park_reject", "adopt_for_dry_run")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_rationale", "allow synthetic dry-run receipt validation only")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_confidence_0_to_1", "0.71")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_dry_run_receipt_ref", "receipt:outbound-dry-run-cli")
	if err := os.WriteFile(created.ReviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	verdictOut := captureStdout(t, func() {
		cmdCapability([]string{"outbound-distribution-verdict-review", created.ReviewYAMLPath, "--json"})
	})
	var verdict capgraph.OutboundDistributionVerdictReviewReport
	if err := json.Unmarshal([]byte(verdictOut), &verdict); err != nil {
		t.Fatalf("parse verdict review: %v\n%s", err, verdictOut)
	}
	adapterOutputPath := filepath.Join(t.TempDir(), "adapter-output.json")
	adapterOutput := map[string]any{
		"schema_version":         capgraph.OutboundDistributionDryRunReceiptSchemaVersion,
		"id":                     "oddr_cli_postiz_synthetic",
		"plan_id":                verdict.PlanID,
		"product_id":             verdict.ProductID,
		"adapter_id":             "adapter:postiz-synthetic",
		"mode":                   "dry_run",
		"run_kind":               "synthetic",
		"public_side_effects":    false,
		"account_auth_used":      false,
		"external_requests_made": false,
		"posted":                 false,
		"replied":                false,
		"spent_money":            false,
		"product_loop": map[string]any{
			"change_event":       "prepare outbound distribution dry-run screenshot change",
			"hypothesis":         "drafts can be reviewed before public posting",
			"observation_window": "14d",
			"next_bounded_move":  "review drafts and request Tier-3 policy only if publishing is warranted",
		},
		"artifacts": []map[string]any{{
			"kind":         "draft_post",
			"channel":      "synthetic-social",
			"ref":          "fixture://postiz/draft-1",
			"content_hash": "sha256:fixture-cli-draft",
		}},
		"evidence_refs": []string{"fixture:cli-dry-run"},
	}
	data, err := json.MarshalIndent(adapterOutput, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adapterOutputPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	receiptOut := captureStdout(t, func() {
		cmdCapability([]string{
			"outbound-distribution-dry-run-receipt",
			"--verdict-review", verdict.ReviewedPath,
			"--adapter-output", adapterOutputPath,
			"--json",
		})
	})
	var receipt struct {
		SchemaVersion  string                                           `json:"schema_version"`
		Status         string                                           `json:"status"`
		Report         capgraph.OutboundDistributionDryRunReceiptReport `json:"report"`
		ReportPath     string                                           `json:"report_path"`
		ReviewPath     string                                           `json:"review_path"`
		ReviewYAMLPath string                                           `json:"review_yaml_path"`
	}
	if err := json.Unmarshal([]byte(receiptOut), &receipt); err != nil {
		t.Fatalf("parse dry-run receipt: %v\n%s", err, receiptOut)
	}
	if receipt.Status != "review_required" || receipt.Report.AdoptionLevel != "dry_run_only" || receipt.ReportPath == "" || receipt.ReviewYAMLPath == "" {
		t.Fatalf("bad dry-run receipt CLI report: %+v", receipt)
	}
	reviewsOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", capgraph.OutboundDistributionDryRunReviewArea, "--json"})
	})
	var reviews reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(reviewsOut), &reviews); err != nil {
		t.Fatalf("parse dry-run reviews: %v\n%s", err, reviewsOut)
	}
	if reviews.Counts.ReviewRequired != 1 || reviews.Counts.DecisionCardCount != 1 || reviews.Counts.ActionableCount != 1 {
		t.Fatalf("dry-run receipt should be reviewable: counts=%+v items=%+v", reviews.Counts, reviews.Items)
	}
	dryRunReview, err := os.ReadFile(receipt.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	dryRunReviewYAML := strings.ReplaceAll(string(dryRunReview), "TODO_decision_accept_as_product_loop_evidence_park_reject", "accept_as_product_loop_evidence")
	dryRunReviewYAML = strings.ReplaceAll(dryRunReviewYAML, "TODO_rationale", "synthetic receipt is bounded and reviewable")
	dryRunReviewYAML = strings.ReplaceAll(dryRunReviewYAML, "TODO_confidence_0_to_1", "0.7")
	if err := os.WriteFile(receipt.ReviewYAMLPath, []byte(dryRunReviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewOut := captureStdout(t, func() {
		cmdCapability([]string{"outbound-distribution-dry-run-review", receipt.ReviewYAMLPath})
	})
	if !strings.Contains(reviewOut, "# capability outbound-distribution-dry-run-review") ||
		!strings.Contains(reviewOut, "- status: recorded") ||
		!strings.Contains(reviewOut, "- decision: accept_as_product_loop_evidence") {
		t.Fatalf("dry-run review command output missing proof:\n%s", reviewOut)
	}
	reviewedPaths, err := filepath.Glob(filepath.Join(homePath, "reviews", capgraph.OutboundDistributionDryRunReviewArea, "*.reviewed.json"))
	if err != nil || len(reviewedPaths) != 1 {
		t.Fatalf("expected one reviewed capability dry-run receipt, paths=%v err=%v", reviewedPaths, err)
	}
	productPlanOut := captureStdout(t, func() {
		cmdProduct([]string{"outbound-dry-run-plan", "--reviewed-receipt", reviewedPaths[0], "--target", targetPath})
	})
	if !strings.Contains(productPlanOut, "# product outbound-dry-run-plan") ||
		!strings.Contains(productPlanOut, "- status: review_required") ||
		!strings.Contains(productPlanOut, "- receipt: "+receipt.Report.ID) {
		t.Fatalf("product outbound dry-run plan output missing proof:\n%s", productPlanOut)
	}
	productReviewsOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", product.OutboundDryRunEvidenceReviewArea, "--json"})
	})
	var productReviews reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(productReviewsOut), &productReviews); err != nil {
		t.Fatalf("parse product dry-run reviews: %v\n%s", err, productReviewsOut)
	}
	if productReviews.Counts.ReviewRequired != 1 || productReviews.Counts.DecisionCardCount != 1 || productReviews.Counts.ActionableCount != 1 {
		t.Fatalf("product dry-run bridge should be reviewable: counts=%+v items=%+v", productReviews.Counts, productReviews.Items)
	}
	productReviewPaths, err := filepath.Glob(filepath.Join(homePath, "reviews", product.OutboundDryRunEvidenceReviewArea, "*.review.yaml"))
	if err != nil || len(productReviewPaths) != 1 {
		t.Fatalf("expected one product dry-run review YAML, paths=%v err=%v", productReviewPaths, err)
	}
	productReviewDraft, err := os.ReadFile(productReviewPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	productReviewYAML := strings.ReplaceAll(string(productReviewDraft), "TODO_decision_create_hypothesis_candidate_park_reject", "create_hypothesis_candidate")
	productReviewYAML = strings.ReplaceAll(productReviewYAML, "TODO_rationale", "reviewed dry-run evidence can become product-loop candidate")
	productReviewYAML = strings.ReplaceAll(productReviewYAML, "TODO_confidence_0_to_1", "0.66")
	productReviewYAML = strings.ReplaceAll(productReviewYAML, "TODO_expected_effect", "reviewed outbound drafts improve product-message clarity before public distribution")
	productReviewYAML = strings.ReplaceAll(productReviewYAML, "TODO_primary_metric", "distribution_draft_quality")
	if err := os.WriteFile(productReviewPaths[0], []byte(productReviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	productReviewOut := captureStdout(t, func() {
		cmdProduct([]string{"outbound-dry-run-review", productReviewPaths[0]})
	})
	if !strings.Contains(productReviewOut, "# product outbound-dry-run-review") ||
		!strings.Contains(productReviewOut, "- status: recorded") ||
		!strings.Contains(productReviewOut, "- decision: create_hypothesis_candidate") ||
		!strings.Contains(productReviewOut, "- hypothesis: hyp_") {
		t.Fatalf("product outbound dry-run review output missing proof:\n%s", productReviewOut)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("product outbound dry-run bridge must not create product signals: %v", err)
	}
}

func TestWorkbenchCommandCanAttachRoutePlan(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	captureStdout(t, func() {
		cmdRoute([]string{"import-model-capabilities", filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")})
	})

	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--route-class", "classify-extract", "--route-privacy-sensitive"})
	})
	var report workbenchCLIOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("workbench JSON did not parse: %v\n%s", err, out)
	}
	if report.RoutePlan == nil {
		t.Fatalf("route plan missing: %+v", report)
	}
	plan := report.RoutePlan
	if plan.SchemaVersion != routeevidence.CapabilityRoutePlanSchemaVersion ||
		plan.AuthorityRef != routeevidence.ModelCapabilitiesAuthorityRef ||
		plan.CapabilityClass != "classify-extract" ||
		!plan.PrivacySensitive ||
		plan.Status != "planned" {
		t.Fatalf("unexpected route plan: %+v", plan)
	}
	if len(plan.Chain) != 1 || plan.Chain[0].Tier != "local-batch" || plan.Chain[0].ModelID != "mac/qwen-fixture" {
		t.Fatalf("privacy-sensitive route should surface only local-batch lane: %+v", plan.Chain)
	}
	if len(plan.Omitted) != 2 {
		t.Fatalf("privacy-sensitive route should preserve omitted non-local lanes: %+v", plan.Omitted)
	}

	human := captureStdout(t, func() {
		cmdWorkbench([]string{"--route-class", "classify-extract", "--route-privacy-sensitive"})
	})
	for _, want := range []string{
		`route_plan status=planned class="classify-extract" privacy_sensitive=true lanes=1 omitted=2 authority=compound://registries/model-router`,
		"route_lane: 2 mac/qwen-fixture",
		"open: none",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("human workbench route output missing %q:\n%s", want, human)
		}
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("workbench route plan must not promote claims, stat err=%v", err)
	}
}

func TestWorkbenchCapabilitySurfaceIsBounded(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatalf("open capgraph: %v", err)
	}
	for i := 0; i < workbenchCapabilityOfferLimit+3; i++ {
		id := "skill:pricing-" + strconv.Itoa(i)
		err := store.Add(capgraph.Node{
			ID:       id,
			Kind:     "skill",
			Name:     "Pricing " + strconv.Itoa(i),
			Triggers: []string{"pricing"},
			Weight:   float64(workbenchCapabilityOfferLimit+3-i) / 10,
		})
		if err != nil {
			t.Fatalf("add node %s: %v", id, err)
		}
	}

	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--capability-task", "pricing"})
	})
	var report workbenchCLIOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("workbench JSON did not parse: %v\n%s", err, out)
	}
	surface := report.CapabilitySurface
	if surface == nil {
		t.Fatalf("capability surface missing: %+v", report)
	}
	if surface.OfferCount != workbenchCapabilityOfferLimit+3 || len(surface.Offers) != workbenchCapabilityOfferLimit || len(surface.OfferSummaries) != workbenchCapabilityOfferLimit {
		t.Fatalf("surface should preserve full count but bound embedded offers: %+v", surface)
	}
	if surface.OfferLimit != workbenchCapabilityOfferLimit || surface.OmittedOfferCount != 3 {
		t.Fatalf("surface limit metadata = limit %d omitted %d", surface.OfferLimit, surface.OmittedOfferCount)
	}
	if surface.OfferSummaries[0].ID != "skill:pricing-0" {
		t.Fatalf("bounded surface lost ranking order: %+v", surface.OfferSummaries[:3])
	}

	out = captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--capability-task", "pricing", "--capability-limit", "3"})
	})
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("workbench JSON with explicit limit did not parse: %v\n%s", err, out)
	}
	surface = report.CapabilitySurface
	if surface == nil || surface.OfferLimit != 3 || len(surface.Offers) != 3 || surface.OmittedOfferCount != workbenchCapabilityOfferLimit {
		t.Fatalf("explicit limit surface = %+v", surface)
	}
}

func TestCapabilityHelpDoesNotConsumeReceipts(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatalf("open capgraph: %v", err)
	}
	if err := store.Add(capgraph.Node{ID: "skill:pricing", Kind: "skill", Name: "Pricing", Triggers: []string{"pricing"}}); err != nil {
		t.Fatal(err)
	}
	rec := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            "rec_help_guard",
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "compound"},
			IDs:           envelope.IDs{WorkID: "work_help_guard"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:  "completed",
		MoveType: "outward_delta",
		StateDelta: envelope.StateDelta{
			Changed:      true,
			Summary:      "help guard receipt",
			FilesChanged: []string{},
			Artifacts:    []string{},
			Verification: []string{},
		},
		Cost:   envelope.ReceiptCost{ModelCalls: []string{}},
		Skills: envelope.ReceiptSkills{Used: []string{"capability://skill:pricing"}, Missed: []string{}, Candidate: []string{}},
		Memory: envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:   envelope.ReceiptNext{SuggestedWork: []string{}},
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(homePath, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "events", rec.ID+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdCapability([]string{"consume-receipts", "--help"}) })
	if !strings.Contains(out, "usage: compound capability") || strings.Contains(out, "receipts_scanned") {
		t.Fatalf("help output should be usage only, got:\n%s", out)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.EdgeCount != 0 {
		t.Fatalf("--help consumed receipts and mutated graph: %+v", stats)
	}
}

func TestCommandHelpShortCircuitsBeforeRuntimeValidation(t *testing.T) {
	t.Setenv("COMPOUND_HOME", filepath.Join(t.TempDir(), "missing-home"))
	tests := []struct {
		name string
		run  func()
		want string
	}{
		{
			name: "work create",
			run:  func() { cmdWork([]string{"create", "--help"}) },
			want: "usage: compound work create",
		},
		{
			name: "work import-packet",
			run:  func() { cmdWork([]string{"import-packet", "--help"}) },
			want: "usage: compound work import-packet",
		},
		{
			name: "memory candidate",
			run:  func() { cmdMemory([]string{"candidate", "--help"}) },
			want: "usage: compound memory candidate",
		},
		{
			name: "memory adjudicate",
			run:  func() { cmdMemory([]string{"adjudicate", "--help"}) },
			want: "usage: compound memory adjudicate",
		},
		{
			name: "route cost-doctor",
			run:  func() { cmdRoute([]string{"cost-doctor", "--help"}) },
			want: "usage: compound route cost-doctor",
		},
		{
			name: "route overview",
			run:  func() { cmdRoute([]string{"--help"}) },
			want: "promotion-record --review",
		},
		{
			name: "route promotion-record",
			run:  func() { cmdRoute([]string{"promotion-record", "--help"}) },
			want: "usage: compound route promotion-record",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureStdout(t, tt.run)
			if !strings.Contains(out, tt.want) {
				t.Fatalf("help output missing %q:\n%s", tt.want, out)
			}
		})
	}
}

func TestWorkbenchCommandIncludesReviewQueueSummary(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	if _, err := reviewartifact.Write(homePath, "product-reviews", "prp_workbench", map[string]any{
		"id":            "prp_workbench",
		"status":        "review_required",
		"product_id":    "product:focus-notes",
		"hypothesis_id": "hyp_focus",
		"signal_id":     "sig_focus",
	}, " ", "# Product review\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewartifact.WriteRaw(homePath, "product-reviews", "prp_workbench.review.yaml", []byte("schema_version: compound.product_review.v0.1\n")); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--review-area", "product-reviews"})
	})
	var report workbenchCLIOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse workbench review queue JSON: %v\n%s", err, out)
	}
	if report.ReviewQueue == nil ||
		report.ReviewQueue.SchemaVersion != reviewartifact.QueueSchemaVersion ||
		report.ReviewQueue.AuthorityRef != "compound://reviews" ||
		report.ReviewQueue.Area != "product-reviews" ||
		report.ReviewQueue.Counts.Total != 1 ||
		report.ReviewQueue.Counts.ReviewRequired != 1 ||
		len(report.ReviewQueue.Actionable) != 1 ||
		report.ReviewQueue.Actionable[0].ID != "prp_workbench" ||
		report.ReviewQueue.Actionable[0].Readiness != "needs_review" ||
		len(report.ReviewQueue.Actionable[0].Actions) != 1 {
		t.Fatalf("bad workbench review queue: %+v", report.ReviewQueue)
	}

	human := captureStdout(t, func() {
		cmdWorkbench([]string{"--review-area", "product-reviews"})
	})
	for _, want := range []string{
		"review_queue total=1 review_required=1 ready_to_apply=0 ok=0 authority=compound://reviews",
		"review_action: product-reviews prp_workbench",
		"open: none",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("human workbench review queue output missing %q:\n%s", want, human)
		}
	}
}

func TestWorkbenchCommandShowsDispatchStepRuns(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	if _, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "workbench command sees dispatch run"}, cliTestNow); err != nil {
		t.Fatalf("create work: %v", err)
	}
	run, err := store.DispatchStepRun(cliTestNow, work.DispatchStepRunOptions{
		DispatchStepOptions: work.DispatchStepOptions{Actor: "codex"},
		MaxSteps:            5,
	})
	if err != nil {
		t.Fatalf("dispatch step run: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--recent", "10"})
	})
	if !strings.Contains(out, "dispatch_step_runs=1") ||
		!strings.Contains(out, "wait="+run.WaitReason) ||
		!strings.Contains(out, run.ID) {
		t.Fatalf("workbench output missing dispatch run:\n%s", out)
	}
}

func TestWorkbenchCommandShowsContinueRuns(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "workbench command sees continue run"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	run, err := store.ContinueRun(cliTestNow, work.ContinueRunOptions{
		ContinueOptions: work.ContinueOptions{Actor: "codex", ExpectWorkID: ready.ID},
		MaxSteps:        5,
	})
	if err != nil {
		t.Fatalf("continue run: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--recent", "10"})
	})
	if !strings.Contains(out, "continue_runs=1") ||
		!strings.Contains(out, "wait="+run.WaitReason) ||
		!strings.Contains(out, run.ID) {
		t.Fatalf("workbench output missing continue run:\n%s", out)
	}
}

func TestWorkbenchCommandDispatchNextUsesActor(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	created, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "actor claim workbench"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	claimed, err := store.Claim(created.ID, "claude", cliTestNow)
	if err != nil {
		t.Fatalf("claim work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--recent", "10", "--actor", "claude"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("workbench output missing JSON:\n%s", out)
	}
	var report work.WorkbenchReport
	if err := json.Unmarshal([]byte(out[start:]), &report); err != nil {
		t.Fatalf("parse workbench report: %v\n%s", err, out)
	}
	if !report.DispatchNext.CanRun || report.DispatchNext.Action != "advance" || report.DispatchNext.WorkID != claimed.ID {
		t.Fatalf("dispatch next = %+v", report.DispatchNext)
	}
}

func TestWorkActivePacketProofCommand(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	writeCLIContextPackArtifact(t, homePath, "ctx_packet")
	imported, err := store.ImportPacket(filepath.Join("..", "..", "fixtures", "work", "goal-packet.md"), work.PacketImportOptions{
		Owner:         "compound-core",
		State:         "ready",
		ContextPackID: "ctx_packet",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("import packet: %v", err)
	}
	if _, err := store.Claim(imported.Work.ID, "codex", cliTestNow.Add(time.Second)); err != nil {
		t.Fatalf("claim packet: %v", err)
	}
	mainPacketRunAttempt, err := runattempt.Open(homePath).Mint(imported.Work.ID, "codex", "codex", cliTestNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(imported.Work.ID, work.AdvanceInput{To: "running", RunAttemptIDs: []string{mainPacketRunAttempt.ID}}, cliTestNow.Add(2*time.Second)); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.Advance(imported.Work.ID, work.AdvanceInput{To: "verifying", VerificationID: "packet-work-contract:active-packet-lifecycle"}, cliTestNow.Add(3*time.Second)); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	closed, err := store.Close(imported.Work.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "active packet proof command",
		Changed:   true,
		ProofRefs: []string{"packet-work-contract:active-packet-lifecycle"},
	}, cliTestNow.Add(4*time.Second))
	if err != nil {
		t.Fatalf("close packet: %v", err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"active-packet-proof", "--surface", imported.Work.CanonicalSurface, "--json"})
	})
	var report work.ActivePacketProofReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse active packet proof: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.WorkID != imported.Work.ID || report.ReceiptID != closed.Receipt.ID || report.TransitionID != closed.Transition.ID {
		t.Fatalf("active packet proof report = %+v", report)
	}
	if !report.Checks["work_completed"] || !report.Checks["receipt_work_matches"] || !report.Checks["transition_work_matches"] {
		t.Fatalf("active packet proof checks = %+v", report.Checks)
	}

	workbenchOut := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--recent", "10", "--active-packet-surface", imported.Work.CanonicalSurface})
	})
	var workbenchReport work.WorkbenchReport
	if err := json.Unmarshal([]byte(workbenchOut), &workbenchReport); err != nil {
		t.Fatalf("parse workbench report: %v\n%s", err, workbenchOut)
	}
	if workbenchReport.ActivePacketProof == nil ||
		workbenchReport.ActivePacketProof.Status != "ok" ||
		workbenchReport.ActivePacketProof.WorkID != imported.Work.ID ||
		workbenchReport.ActivePacketProof.ReceiptID != closed.Receipt.ID ||
		workbenchReport.ActivePacketProof.TransitionID != closed.Transition.ID {
		t.Fatalf("workbench active packet proof = %+v", workbenchReport.ActivePacketProof)
	}
	if workbenchReport.DispatchNext.CanRun ||
		workbenchReport.DispatchNext.WaitReason != "source_packet_terminal" ||
		workbenchReport.DispatchNext.WorkID != imported.Work.ID {
		t.Fatalf("workbench dispatch next = %+v", workbenchReport.DispatchNext)
	}

	humanOut := captureStdout(t, func() {
		cmdWorkbench([]string{"--recent", "10", "--active-packet-surface", imported.Work.CanonicalSurface})
	})
	if !strings.Contains(humanOut, "active_packet_proof status=ok") ||
		!strings.Contains(humanOut, "dispatch_wait: source_packet_terminal work="+imported.Work.ID) {
		t.Fatalf("workbench human output missing active packet terminal proof:\n%s", humanOut)
	}
}

func TestWorkActivePacketProofDetectsChangedSourcePacket(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	writeCLIContextPackArtifact(t, homePath, "ctx_packet")
	imported, err := store.ImportPacket(packetPath, work.PacketImportOptions{
		Owner:         "compound-core",
		State:         "ready",
		ContextPackID: "ctx_packet",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("import packet: %v", err)
	}
	if _, err := store.Claim(imported.Work.ID, "codex", cliTestNow.Add(time.Second)); err != nil {
		t.Fatalf("claim packet: %v", err)
	}
	mainPacketRunAttempt, err := runattempt.Open(homePath).Mint(imported.Work.ID, "codex", "codex", cliTestNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(imported.Work.ID, work.AdvanceInput{To: "running", RunAttemptIDs: []string{mainPacketRunAttempt.ID}}, cliTestNow.Add(2*time.Second)); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.Advance(imported.Work.ID, work.AdvanceInput{To: "verifying", VerificationID: "packet-work-contract:active-packet-lifecycle"}, cliTestNow.Add(3*time.Second)); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	if _, err := store.Close(imported.Work.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "active packet proof command",
		Changed:   true,
		ProofRefs: []string{"packet-work-contract:active-packet-lifecycle"},
	}, cliTestNow.Add(4*time.Second)); err != nil {
		t.Fatalf("close packet: %v", err)
	}

	data, err := os.ReadFile(packetPath)
	if err != nil {
		t.Fatalf("read packet fixture: %v", err)
	}
	changedPacket := filepath.Join(t.TempDir(), "goal-packet.md")
	if err := os.WriteFile(changedPacket, append(data, []byte("\nFreshness proof line.\n")...), 0o644); err != nil {
		t.Fatalf("write changed packet: %v", err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"active-packet-proof", "--source-packet", changedPacket, "--json"})
	})
	var report work.ActivePacketProofReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse active packet proof: %v\n%s", err, out)
	}
	if report.Status != "risk" || report.WorkID != imported.Work.ID || report.Checks["source_packet_matches"] {
		t.Fatalf("active packet proof should flag changed source packet, got %+v", report)
	}
	if report.ExpectedPacketEvidenceRef == "" || containsString(report.PacketEvidenceRefs, report.ExpectedPacketEvidenceRef) {
		t.Fatalf("expected packet evidence not recorded as mismatch: %+v", report)
	}

	workbenchOut := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--recent", "10", "--source-packet", changedPacket})
	})
	var workbenchReport work.WorkbenchReport
	if err := json.Unmarshal([]byte(workbenchOut), &workbenchReport); err != nil {
		t.Fatalf("parse workbench report: %v\n%s", err, workbenchOut)
	}
	if workbenchReport.ActivePacketProof == nil ||
		workbenchReport.ActivePacketProof.Status != "risk" ||
		workbenchReport.ActivePacketProof.Checks["source_packet_matches"] ||
		workbenchReport.DispatchNext.WaitReason != "active_packet_proof_risk" {
		t.Fatalf("workbench should surface changed source packet risk: proof=%+v dispatch=%+v", workbenchReport.ActivePacketProof, workbenchReport.DispatchNext)
	}
}

func TestWorkDispatchPlanCommandRecordsProposalWithoutClaiming(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch plan selects this work"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-plan", "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-plan output missing JSON:\n%s", out)
	}
	var plan work.DispatchPlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse dispatch plan: %v\n%s", err, out)
	}
	if plan.Status != "ready_to_claim" || plan.Selected == nil || plan.Selected.WorkID != ready.ID {
		t.Fatalf("dispatch plan = %+v, ready=%s", plan, ready.ID)
	}
	if plan.Selected.ClaimPolicy == nil || !plan.Selected.ClaimPolicy.Allowed {
		t.Fatalf("dispatch plan selected item missing allowed claim policy: %+v", plan.Selected)
	}
	if plan.PlanPath == "" {
		t.Fatalf("dispatch plan did not record runtime artifact: %+v", plan)
	}
	if _, err := os.Stat(plan.PlanPath); err != nil {
		t.Fatalf("dispatch plan artifact missing: %v", err)
	}
	after, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get ready work: %v", err)
	}
	if after.State != "ready" {
		t.Fatalf("dispatch plan must not claim work, state=%s", after.State)
	}
	if _, err := os.Stat(filepath.Join(homePath, "work", "leases", ready.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("dispatch plan must not create lease, stat err=%v", err)
	}
}

func TestWorkDispatchClaimCommandClaimsSelectedWork(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch claim selects work"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-claim", "--next", "--confirm", "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-claim output missing JSON:\n%s", out)
	}
	var result work.DispatchClaimResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse dispatch claim: %v\n%s", err, out)
	}
	if result.Status != "claimed" || result.ClaimedWorkID != ready.ID || result.PolicyDecisionID == "" || result.ActionIntentID == "" {
		t.Fatalf("dispatch claim result = %+v, ready=%s", result, ready.ID)
	}
	if result.DispatchPlanPath == "" || result.ResultPath == "" {
		t.Fatalf("dispatch claim did not record plan/result: %+v", result)
	}
	after, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get claimed work: %v", err)
	}
	if after.State != "claimed" || after.Authority.Owner != "codex" || after.PolicyDecisionID != result.PolicyDecisionID {
		t.Fatalf("dispatch claim did not claim through policy path: work=%+v result=%+v", after, result)
	}
	if _, err := os.Stat(filepath.Join(homePath, "work", "leases", ready.ID+".json")); err != nil {
		t.Fatalf("dispatch claim must create lease, stat err=%v", err)
	}
	if after.IDs.TransitionID != "" {
		t.Fatalf("dispatch claim must not close work: %+v", after.IDs)
	}
}

func TestWorkDispatchClaimCommandClaimsExplicitWork(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	first, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch claim skips non-target"}, cliTestNow)
	if err != nil {
		t.Fatalf("create first work: %v", err)
	}
	second, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch claim exact target"}, cliTestNow.Add(time.Second))
	if err != nil {
		t.Fatalf("create second work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-claim", second.ID, "--confirm", "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-claim output missing JSON:\n%s", out)
	}
	var result work.DispatchClaimResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse dispatch claim: %v\n%s", err, out)
	}
	if result.Status != "claimed" || result.ClaimedWorkID != second.ID {
		t.Fatalf("dispatch claim result = %+v, target=%s", result, second.ID)
	}
	firstAfter, err := store.Get(first.ID)
	if err != nil {
		t.Fatalf("get first work: %v", err)
	}
	if firstAfter.State != "ready" {
		t.Fatalf("dispatch claim should not select non-target work: %+v", firstAfter)
	}
}

func TestWorkDispatchClaimCommandArglessPreviewsWithoutClaiming(t *testing.T) {
	t.Parallel()
	if os.Getenv(workCLIBoundarySubEnv) == "dispatch-claim-preview" {
		work.LaneCapacityGuard = nil
		cmdWork([]string{"dispatch-claim", "--actor", "codex", "--json"})
		return
	}
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "argless preview does not claim"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkDispatchClaimCommandArglessPreviewsWithoutClaiming$")
	cmd.Env = append(os.Environ(), workCLIBoundarySubEnv+"=dispatch-claim-preview", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("unconfirmed dispatch-claim should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("unconfirmed dispatch-claim should exit 1, got %d output=%s", exitErr.ExitCode(), output)
	}
	var preview work.DispatchPlan
	if err := json.Unmarshal(output, &preview); err != nil {
		t.Fatalf("decode dispatch preview: %v\n%s", err, output)
	}
	if preview.Selected == nil || preview.Selected.WorkID != ready.ID {
		t.Fatalf("argless preview selected %+v, want %s", preview.Selected, ready.ID)
	}
	after, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get work after preview: %v", err)
	}
	if after.State != "ready" {
		t.Fatalf("argless preview changed work state to %s", after.State)
	}
}

func TestWorkCreateRejectsRunningStateAsUsage(t *testing.T) {
	t.Parallel()
	if os.Getenv(workCLIBoundarySubEnv) == "create-running" {
		cmdWork([]string{"create", "--owner", "compound-core", "--predicate", "direct running create", "--state", "running"})
		return
	}
	homePath := t.TempDir()
	if err := home.Init(homePath, "global", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkCreateRejectsRunningStateAsUsage$")
	cmd.Env = append(os.Environ(), workCLIBoundarySubEnv+"=create-running", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("work create should exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("work create should exit 2, got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "create at ready -> claim -> advance") {
		t.Fatalf("work create usage did not explain lease-preserving path:\n%s", output)
	}
}

func TestWorkCreateAllowsRunningStateInTestHomeThroughLifecycle(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	cmdWork([]string{"create", "--owner", "compound-core", "--predicate", "test fixture starts running", "--state", "running"})
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 || items[0].State != "running" {
		t.Fatalf("running test fixture = %+v, err=%v", items, err)
	}
	_, events, err := store.Show(items[0].ID)
	if err != nil {
		t.Fatalf("show running test fixture: %v", err)
	}
	wantTransitions := [][2]string{{"", "ready"}, {"ready", "claimed"}, {"claimed", "running"}}
	if len(events) != len(wantTransitions) {
		t.Fatalf("running test fixture lifecycle = %+v, want %d events", events, len(wantTransitions))
	}
	for i, want := range wantTransitions {
		if events[i].From != want[0] || events[i].To != want[1] {
			t.Fatalf("running test fixture event %d = %+v, want %q -> %q", i, events[i], want[0], want[1])
		}
	}
}

func TestWorkCreateCommandsRejectUndocumentedInitialStates(t *testing.T) {
	t.Parallel()
	switch os.Getenv(workCLIBoundarySubEnv) {
	case "create-admitted":
		cmdWork([]string{"create", "--owner", "compound-core", "--predicate", "direct admitted create", "--state", "admitted"})
		return
	case "create-plan-running":
		cmdWork([]string{"create-plan", "--owner", "compound-core", "--predicate", "direct running create plan", "--state", "running", "--dry-run"})
		return
	case "create-plan-admitted":
		cmdWork([]string{"create-plan", "--owner", "compound-core", "--predicate", "direct admitted create plan", "--state", "admitted", "--dry-run"})
		return
	case "import-packet-running":
		cmdWork([]string{"import-packet", filepath.Join("..", "..", "fixtures", "work", "goal-packet.md"), "--state", "running"})
		return
	case "import-packet-admitted":
		cmdWork([]string{"import-packet", filepath.Join("..", "..", "fixtures", "work", "goal-packet.md"), "--state", "admitted"})
		return
	}

	for _, child := range []string{"create-admitted", "create-plan-running", "create-plan-admitted", "import-packet-running", "import-packet-admitted"} {
		t.Run(child, func(t *testing.T) {
			homePath := initializedTestHome(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestWorkCreateCommandsRejectUndocumentedInitialStates$")
			cmd.Env = append(os.Environ(), workCLIBoundarySubEnv+"="+child, "COMPOUND_HOME="+homePath)
			output, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("%s should exit non-zero, got err=%v output=%s", child, err, output)
			}
			if exitErr.ExitCode() != 2 {
				t.Fatalf("%s should exit 2, got %d output=%s", child, exitErr.ExitCode(), output)
			}
			if !strings.Contains(string(output), "initial work state must be proposed or ready") {
				t.Fatalf("%s did not explain initial-state boundary:\n%s", child, output)
			}
		})
	}
}

func TestWorkDispatchAdvanceCommandStartsClaimedWork(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch advance starts work"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(ready.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-advance", "--next", "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-advance output missing JSON:\n%s", out)
	}
	var result work.DispatchAdvanceResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse dispatch advance: %v\n%s", err, out)
	}
	if result.Status != "running" || result.AdvancedWorkID != ready.ID || result.PolicyDecisionID == "" || result.ActionIntentID == "" {
		t.Fatalf("dispatch advance result = %+v, ready=%s", result, ready.ID)
	}
	if result.ResultPath == "" {
		t.Fatalf("dispatch advance did not record result: %+v", result)
	}
	after, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get advanced work: %v", err)
	}
	if after.State != "running" || after.PolicyDecisionID != result.PolicyDecisionID {
		t.Fatalf("dispatch advance did not use policy path: work=%+v result=%+v", after, result)
	}
	if after.IDs.TransitionID != "" {
		t.Fatalf("dispatch advance must not close work: %+v", after.IDs)
	}
}

func TestWorkDispatchClosePlanCommandRecordsClosePlanWithoutClosing(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch close-plan command"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(ready.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	running, err := store.Advance(ready.ID, work.AdvanceInput{To: "running", Actor: "codex"}, cliTestNow)
	if err != nil {
		t.Fatalf("advance running: %v", err)
	}
	trace := writeCLITrace(t, homePath, "trace_cli_dispatch_close_plan", running.ID)
	if _, err := store.ObserveRun(running.ID, work.ObserveRunInput{Actor: "codex", TraceID: trace.ID}, cliTestNow); err != nil {
		t.Fatalf("observe run: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-close-plan", "--next", "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-close-plan output missing JSON:\n%s", out)
	}
	var result work.DispatchClosePlanResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse dispatch close-plan result: %v\n%s", err, out)
	}
	if result.Status != "close_policy_allowed" || result.Selected == nil || result.Selected.WorkID != ready.ID {
		t.Fatalf("dispatch close-plan result = %+v, ready=%s", result, ready.ID)
	}
	if result.ClosePlan == nil || result.ClosePlanPath == "" || result.ResultPath == "" || !containsString(result.ClosePlan.ProofRefs, trace.ID) {
		t.Fatalf("dispatch close-plan missing recorded proof plan: %+v", result)
	}
	after, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get close-planned work: %v", err)
	}
	if after.State != "verifying" || after.IDs.TransitionID != "" {
		t.Fatalf("dispatch close-plan must not close work: %+v", after)
	}
}

func TestWorkDispatchCloseCommandClosesProofReadyWork(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch close command"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(ready.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	running, err := store.Advance(ready.ID, work.AdvanceInput{To: "running", Actor: "codex"}, cliTestNow)
	if err != nil {
		t.Fatalf("advance running: %v", err)
	}
	trace := writeCLITrace(t, homePath, "trace_cli_dispatch_close", running.ID)
	if _, err := store.ObserveRun(running.ID, work.ObserveRunInput{Actor: "codex", TraceID: trace.ID}, cliTestNow); err != nil {
		t.Fatalf("observe run: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-close", "--next", "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-close output missing JSON:\n%s", out)
	}
	var result work.DispatchCloseResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse dispatch close result: %v\n%s", err, out)
	}
	if result.Status != "closed" || result.ClosedWorkID != ready.ID || result.ReceiptID == "" || result.TransitionID == "" {
		t.Fatalf("dispatch close result = %+v, ready=%s", result, ready.ID)
	}
	after, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get closed work: %v", err)
	}
	if after.State != "completed" || after.IDs.TransitionID != result.TransitionID {
		t.Fatalf("dispatch close did not terminalize work: %+v", after)
	}
}

func TestWorkDispatchStepCommandClaimsReadyWork(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch step command"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-step", "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-step output missing JSON:\n%s", out)
	}
	var result work.DispatchStepResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse dispatch step result: %v\n%s", err, out)
	}
	if result.Status != "claimed" || result.Action != "claim" || result.WorkID != ready.ID {
		t.Fatalf("dispatch step result = %+v, ready=%s", result, ready.ID)
	}
	if result.NextAction != "advance running or release to ready" || result.PostCounts == nil || result.PostCounts.InProgress != 1 {
		t.Fatalf("dispatch step JSON missing post-state: %+v", result)
	}
	if !result.CanContinue || len(result.ContinueCommand) != 5 || result.ContinueCommand[2] != "dispatch-step" || result.ContinueCommand[4] != "codex" {
		t.Fatalf("dispatch step JSON missing continuation: %+v", result)
	}
	after, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get claimed work: %v", err)
	}
	if after.State != "claimed" || after.Authority.Owner != "codex" {
		t.Fatalf("dispatch step did not claim ready work: %+v", after)
	}
}

func TestWorkDispatchStepCommandImportsSourcePacket(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-step", "--actor", "codex", "--source-packet", packetPath, "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-step output missing JSON:\n%s", out)
	}
	var result work.DispatchStepResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse dispatch step result: %v\n%s", err, out)
	}
	if result.Status != "claimed" || result.Action != "claim" || result.DispatchClaim == nil {
		t.Fatalf("dispatch step result = %+v", result)
	}
	source := result.DispatchClaim.Plan.Source
	if source == nil || source.Kind != "packet" || !source.Created || result.WorkID != source.WorkID {
		t.Fatalf("dispatch step source = %+v result=%+v", source, result)
	}
	claimed, err := store.Get(source.WorkID)
	if err != nil {
		t.Fatalf("get packet work: %v", err)
	}
	if claimed.State != "claimed" || claimed.Authority.Owner != "codex" {
		t.Fatalf("dispatch step did not claim packet work: %+v", claimed)
	}
}

func TestWorkDispatchStepCommandReportsTerminalSourcePacket(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	imported, err := store.ImportPacket(packetPath, work.PacketImportOptions{
		Owner: "compound-core",
		State: "ready",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("import packet: %v", err)
	}
	if _, err := store.Claim(imported.Work.ID, "codex", cliTestNow.Add(time.Second)); err != nil {
		t.Fatalf("claim packet: %v", err)
	}
	mainTerminalRunAttempt, err := runattempt.Open(homePath).Mint(imported.Work.ID, "codex", "codex", cliTestNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(imported.Work.ID, work.AdvanceInput{To: "running", RunAttemptIDs: []string{mainTerminalRunAttempt.ID}}, cliTestNow.Add(2*time.Second)); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.Advance(imported.Work.ID, work.AdvanceInput{To: "verifying", VerificationID: "packet-work-contract:terminal-source"}, cliTestNow.Add(3*time.Second)); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}
	if _, err := store.Close(imported.Work.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "terminal source packet proof",
		Changed:   true,
		ProofRefs: []string{"packet-work-contract:terminal-source"},
	}, cliTestNow.Add(4*time.Second)); err != nil {
		t.Fatalf("close packet: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-step", "--actor", "codex", "--source-packet", packetPath, "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-step output missing JSON:\n%s", out)
	}
	var result work.DispatchStepResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse dispatch step result: %v\n%s", err, out)
	}
	if result.Status != "source_packet_terminal" || result.Action != "none" || result.WorkID != imported.Work.ID {
		t.Fatalf("dispatch step result = %+v", result)
	}
	if result.Source == nil || result.Source.Created || result.Source.State != "completed" {
		t.Fatalf("dispatch step source = %+v", result.Source)
	}
	if result.WaitReason != "source_packet_terminal" || !containsString(result.BlockedOn, "work.terminal:"+imported.Work.ID) {
		t.Fatalf("dispatch step wait = %q blocked=%v", result.WaitReason, result.BlockedOn)
	}
}

func TestWorkDispatchStepCommandRunsUntilWait(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "dispatch step run command"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-step", "--actor", "codex", "--until-wait", "--max-steps", "5", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-step run output missing JSON:\n%s", out)
	}
	var result work.DispatchStepRunResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse dispatch step run result: %v\n%s", err, out)
	}
	if result.Status != "waiting" || result.StepCount != 2 || result.WaitReason != "run_evidence" {
		t.Fatalf("dispatch step run result = %+v", result)
	}
	if len(result.Steps) != 2 || result.Steps[0].Action != "claim" || result.Steps[1].Action != "advance" {
		t.Fatalf("dispatch step run steps = %+v", result.Steps)
	}
	if _, err := os.Stat(result.ResultPath); err != nil {
		t.Fatalf("dispatch step run result not recorded: %v", err)
	}
	after, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if after.State != "running" {
		t.Fatalf("dispatch step run should stop at running: %+v", after)
	}
}

func TestWorkContinueCommandRunsSuggestedWorkUntilWait(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	now := time.Now().UTC()
	plan, err := store.CreatePlan(work.CreateInput{
		WorkID:           "work_cli_continue_preview",
		Owner:            "compound-core",
		Actor:            "codex",
		State:            "proposed",
		SuccessPredicate: "CLI continue starts suggested work",
	}, now, work.CreatePlanOptions{
		Record:        true,
		Capability:    "product.action_review",
		Action:        "plan_work_from_reviewed_product_action",
		TargetKind:    "product_action_review",
		TargetID:      "cli_continue_demo",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"continue", "--actor", "codex", "--expect-work", plan.WorkID, "--until-wait", "--max-steps", "6", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("continue run output missing JSON:\n%s", out)
	}
	var result work.ContinueRunResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse continue run result: %v\n%s", err, out)
	}
	if result.Status != "waiting" || result.StepCount != 4 || result.WaitReason != "run_evidence" {
		t.Fatalf("continue run result = %+v", result)
	}
	if len(result.Steps) != 4 || result.Steps[0].Action != "admit" || result.Steps[1].Action != "prepare" || result.Steps[2].Action != "claim" || result.Steps[3].Action != "advance" {
		t.Fatalf("continue run steps = %+v", result.Steps)
	}
	after, err := store.Get(plan.WorkID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if after.State != "running" {
		t.Fatalf("continue should stop at running: %+v", after)
	}
}

func TestWorkClaimPlanCommandRecordsPolicyWithoutClaiming(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "claim plan command"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"claim-plan", ready.ID, "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("claim-plan output missing JSON:\n%s", out)
	}
	var plan work.ClaimPlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse claim plan: %v\n%s", err, out)
	}
	if plan.Status != "claim_policy_allowed" || !plan.Policy.Allowed {
		t.Fatalf("claim plan policy not allowed: %+v", plan)
	}
	if plan.PlanPath == "" {
		t.Fatalf("claim plan was not recorded: %+v", plan)
	}
	if _, err := os.Stat(filepath.Join(homePath, "work", "leases", ready.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("claim plan must not create lease, stat err=%v", err)
	}
}

func TestWorkAdvancePlanAndAdvanceCommandRecordPolicy(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "advance plan command"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(ready.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"advance-plan", ready.ID, "--to", "running", "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("advance-plan output missing JSON:\n%s", out)
	}
	var plan work.AdvancePlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse advance plan: %v\n%s", err, out)
	}
	if plan.Status != "advance_policy_allowed" || !plan.Policy.Allowed || plan.PlanPath == "" {
		t.Fatalf("advance plan policy not allowed/recorded: %+v", plan)
	}
	afterPlan, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get after plan: %v", err)
	}
	if afterPlan.State != "claimed" {
		t.Fatalf("advance plan must not mutate work: %+v", afterPlan)
	}
	out = captureStdout(t, func() {
		cmdWork([]string{"advance", ready.ID, "--to", "running", "--by", "codex", "--run-attempt", "ratt_20260720T000000Z_eeff0011"})
	})
	if !strings.Contains(out, "policy=") {
		t.Fatalf("advance output missing policy id:\n%s", out)
	}
	afterAdvance, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get after advance: %v", err)
	}
	if afterAdvance.State != "running" || afterAdvance.PolicyDecisionID == "" {
		t.Fatalf("advance command did not record policy: %+v", afterAdvance)
	}
	if stringSliceContains(afterAdvance.ActionIntentIDs, "run_cli") {
		t.Fatalf("ActionIntentIDs must not contain legacy run id after S1; got %v", afterAdvance.ActionIntentIDs)
	}
	if !stringSliceContains(afterAdvance.RunAttemptIDs, "ratt_20260720T000000Z_eeff0011") {
		t.Fatalf("RunAttemptIDs missing the ratt_ id attached via --run-attempt; got %v", afterAdvance.RunAttemptIDs)
	}
}

func TestWorkObserveRunCommandAttachesTrace(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	w, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "observe run command"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex"}, cliTestNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	trace := envelope.TraceEnvelope{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTraceEnvelope,
			ID:            "trace_cli_observe",
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "compound"},
			IDs:           envelope.IDs{WorkID: w.ID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Source:          envelope.TraceSource{System: "test", SourceKind: "cli", EventClass: "observe_run"},
		Actor:           "codex",
		MutationClass:   "local_reversible",
		Summary:         "observed CLI run",
		ToolsModelsUsed: []string{},
		FilesTouched:    []string{},
		Outputs:         []string{"trace-output:cli"},
	}
	if err := trace.Validate(); err != nil {
		t.Fatal(err)
	}
	traceBytes, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(homePath, "events", trace.ID+".json")
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, append(traceBytes, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"observe-run", w.ID, "--trace", trace.ID, "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("observe-run output missing JSON:\n%s", out)
	}
	var result work.RunObservationResult
	if err := json.Unmarshal([]byte(out[start:]), &result); err != nil {
		t.Fatalf("parse observe-run result: %v\n%s", err, out)
	}
	if result.Status != "observed" || result.WorkID != w.ID || result.TraceID != trace.ID || result.PolicyDecisionID == "" {
		t.Fatalf("observe-run result = %+v", result)
	}
	after, err := store.Get(w.ID)
	if err != nil {
		t.Fatalf("get observed work: %v", err)
	}
	if after.State != "verifying" || !stringSliceContains(after.VerificationIDs, trace.ID) || after.IDs.TransitionID != "" {
		t.Fatalf("observe-run did not attach trace without closing: %+v", after)
	}
}

func TestWorkClosePlanAndCloseCommandRecordPolicy(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	ready, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "close plan command"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(ready.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := store.Advance(ready.ID, work.AdvanceInput{To: "running", Actor: "codex"}, cliTestNow); err != nil {
		t.Fatalf("advance work: %v", err)
	}
	out := captureStdout(t, func() {
		cmdWork([]string{"close-plan", ready.ID, "--outcome", "completed", "--move-type", "control_plane_repair", "--actor", "codex", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("close-plan output missing JSON:\n%s", out)
	}
	var plan work.ClosePlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse close plan: %v\n%s", err, out)
	}
	if plan.Status != "close_policy_allowed" || !plan.Policy.Allowed || plan.PlanPath == "" {
		t.Fatalf("close plan policy not allowed/recorded: %+v", plan)
	}
	afterPlan, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get after plan: %v", err)
	}
	if afterPlan.State != "running" || afterPlan.IDs.TransitionID != "" {
		t.Fatalf("close plan must not mutate work: %+v", afterPlan)
	}
	out = captureStdout(t, func() {
		cmdWork([]string{"close", ready.ID, "--outcome", "completed", "--move-type", "control_plane_repair", "--summary", "close proof", "--proof", "proof:cli-close", "--proof-ref", "proof:cli-close-ref", "--by", "codex"})
	})
	if !strings.Contains(out, "policy=") {
		t.Fatalf("close output missing policy id:\n%s", out)
	}
	start = strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("close output missing JSON:\n%s", out)
	}
	var closeResult work.CloseResult
	if err := json.Unmarshal([]byte(out[start:]), &closeResult); err != nil {
		t.Fatalf("parse close output: %v\n%s", err, out)
	}
	if !stringSliceContains(closeResult.Receipt.ProofRefs, "proof:cli-close") || !stringSliceContains(closeResult.Receipt.ProofRefs, "proof:cli-close-ref") {
		t.Fatalf("close receipt proof refs = %+v", closeResult.Receipt.ProofRefs)
	}
	closed, err := store.Get(ready.ID)
	if err != nil {
		t.Fatalf("get closed work: %v", err)
	}
	if closed.State != "completed" || closed.PolicyDecisionID == "" || closed.IDs.TransitionID == "" {
		t.Fatalf("close command did not record policy/transition: %+v", closed)
	}
}

// Falsifier 8 (anonymous-close-falsifier), read-only half: `work close-plan` never calls
// die() on a lease-block (it just reports NoAction), so the CLI's --by-less-on-held-work
// denial is directly observable here without a subprocess. The mutating `work close`
// refusal itself is proven at the Store level (TestCloseRefusesNonLeaseHolder,
// internal/work/lease_close_test.go) — die() would os.Exit the test process, so per the
// packet CLI tests cover success + steal-success + flag validation only.
func TestWorkClosePlanCommandReportsLeaseHolderDenial(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	held, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "lease holder cli"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(held.ID, "actor-a", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := store.Advance(held.ID, work.AdvanceInput{To: "running", Actor: "actor-a"}, cliTestNow); err != nil {
		t.Fatalf("advance work: %v", err)
	}

	// No --actor at all (anonymous close-plan of held work): reports the holder-named
	// denial, offers no close command, and does not call die() (ClosePlan never errors on
	// a lease block, only sets NoAction).
	out := captureStdout(t, func() {
		cmdWork([]string{"close-plan", held.ID, "--outcome", "completed", "--move-type", "control_plane_repair", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("close-plan output missing JSON:\n%s", out)
	}
	var plan work.ClosePlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse close plan: %v\n%s", err, out)
	}
	if plan.NoAction == "" || !strings.Contains(plan.NoAction, "actor-a") {
		t.Fatalf("close-plan without --actor on held work must name the holder actor-a: %+v", plan)
	}
	if len(plan.CloseCommand) != 0 {
		t.Fatalf("close-plan without --actor on held work must not offer a close command: %v", plan.CloseCommand)
	}
	after, err := store.Get(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" {
		t.Fatalf("close-plan must not mutate work: %+v", after)
	}
}

// Steal-success path: `work close --steal --reason` on a held work succeeds via the CLI
// and the emitted close command from close-plan carries --steal --reason.
func TestWorkCloseCommandStealOverridesLeaseHolder(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	held, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "steal cli"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(held.ID, "actor-a", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := store.Advance(held.ID, work.AdvanceInput{To: "running", Actor: "actor-a"}, cliTestNow); err != nil {
		t.Fatalf("advance work: %v", err)
	}

	planOut := captureStdout(t, func() {
		cmdWork([]string{
			"close-plan", held.ID, "--outcome", "completed", "--move-type", "control_plane_repair",
			"--actor", "actor-b", "--steal", "--reason", "actor-a executor died mid-build", "--json",
		})
	})
	start := strings.Index(planOut, "{")
	if start < 0 {
		t.Fatalf("close-plan output missing JSON:\n%s", planOut)
	}
	var plan work.ClosePlan
	if err := json.Unmarshal([]byte(planOut[start:]), &plan); err != nil {
		t.Fatalf("parse close plan: %v\n%s", err, planOut)
	}
	if !stringSliceContains(plan.CloseCommand, "--reason") || !stringSliceContains(plan.CloseCommand, "actor-a executor died mid-build") {
		t.Fatalf("steal-ready close command must carry --reason <reason>: %v", plan.CloseCommand)
	}

	closeOut := captureStdout(t, func() {
		cmdWork([]string{
			"close", held.ID, "--outcome", "completed", "--move-type", "control_plane_repair",
			"--summary", "stolen close", "--proof", "proof:steal-close", "--actor", "actor-b",
			"--steal", "--reason", "actor-a executor died mid-build",
		})
	})
	if !strings.Contains(closeOut, "completed") {
		t.Fatalf("stolen close output missing completed state:\n%s", closeOut)
	}
	closed, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get closed work: %v", err)
	}
	if closed.State != "completed" {
		t.Fatalf("stolen close command did not close the work: %+v", closed)
	}
	_, evs, err := store.Show(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if strings.Contains(e.Note, "stolen") && strings.Contains(e.Note, "actor-a") {
			found = true
		}
	}
	if !found {
		t.Fatalf("trail missing the steal record: %+v", evs)
	}
}

// Steal-success path for the advance-gate lease enforcement: `work advance --steal --reason`
// on a held work succeeds via the CLI, and the trail carries the prior holder, the reason,
// and the stealing actor. The non-holder-refusal falsifier itself is proven at the Store
// level (TestAdvanceRefusesNonLeaseHolder, internal/work/lease_advance_test.go) — the
// mutating `work advance` calls die()/os.Exit on refusal, same as `work close`, so per the
// close-gate lane's own precedent CLI tests here cover only the success + steal-success path.
func TestWorkAdvanceCommandStealOverridesLeaseHolder(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	held, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "advance steal cli"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(held.ID, "actor-a", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{
			"advance", held.ID, "--to", "running",
			"--actor", "actor-b", "--steal", "--reason", "actor-a executor died mid-build",
		})
	})
	if !strings.Contains(out, "running") {
		t.Fatalf("stolen advance output missing running state:\n%s", out)
	}
	advanced, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get advanced work: %v", err)
	}
	if advanced.State != "running" {
		t.Fatalf("stolen advance command did not advance the work: %+v", advanced)
	}
	_, evs, err := store.Show(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if strings.Contains(e.Note, "stolen") && strings.Contains(e.Note, "actor-a") {
			found = true
		}
	}
	if !found {
		t.Fatalf("trail missing the steal record: %+v", evs)
	}
}

func TestValidateKnownFlagsRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	err := validateKnownFlags(
		[]string{"work_123", "--outcome", "completed", "--proof-ref", "proof:ok", "--proof-rfe", "proof:typo"},
		map[string]bool{"--outcome": true, "--proof-ref": true},
		map[string]bool{"--changed": true},
	)
	if err == nil || !strings.Contains(err.Error(), "unknown flag --proof-rfe") {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if err := validateKnownFlags([]string{"work_123", "--outcome", "completed", "--changed"}, map[string]bool{"--outcome": true}, map[string]bool{"--changed": true}); err != nil {
		t.Fatalf("known flags rejected: %v", err)
	}
}

func TestWorkDispatchPlanCommandPreviewsSourcePacketReadOnly(t *testing.T) {
	isolateDispatchCapacityGuard(t)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	packetPath := filepath.Join("..", "..", "fixtures", "work", "goal-packet.md")
	out := captureStdout(t, func() {
		cmdWork([]string{"dispatch-plan", "--actor", "codex", "--source-packet", packetPath, "--dry-run", "--json"})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("dispatch-plan output missing JSON:\n%s", out)
	}
	var plan work.DispatchPlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse dispatch plan: %v\n%s", err, out)
	}
	if plan.Source == nil || plan.Source.Kind != "packet" || plan.Source.Created || plan.Source.WorkID != "" {
		t.Fatalf("dispatch plan source preview mismatch: %+v", plan.Source)
	}
	if plan.Status != "no_open_work" || plan.Selected != nil {
		t.Fatalf("dispatch plan must not import/select packet work: %+v", plan)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("list work: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("dispatch plan must not write work state: %+v", list)
	}
	if _, err := os.Stat(filepath.Join(homePath, "dispatch-plans")); !os.IsNotExist(err) {
		t.Fatalf("dry-run dispatch plan must not record plan, stat err=%v", err)
	}
}

func TestInstallScriptBuildsInitializesAndDoctorsTempHome(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(t.TempDir(), "bin")
	homePath := filepath.Join(t.TempDir(), ".compound")
	tempHome := t.TempDir()
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	modCacheOut, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatalf("go env GOMODCACHE: %v", err)
	}
	modCache := strings.TrimSpace(string(modCacheOut))

	cmd := exec.Command("sh", filepath.Join(repoRoot, "install.sh"))
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"HOME="+tempHome,
		"GOBIN="+installDir,
		"COMPOUND_HOME="+homePath,
		"GOMODCACHE="+modCache,
		"GOCACHE="+filepath.Join(t.TempDir(), "gocache"),
		"PATH="+installDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	binaryPath := filepath.Join(installDir, "compound")
	if _, err := os.Stat(binaryPath); err != nil {
		t.Fatalf("installed binary missing: %v\n%s", err, out)
	}
	aliasPath := filepath.Join(installDir, "os")
	if _, err := os.Lstat(aliasPath); !os.IsNotExist(err) {
		t.Fatalf("default public install created an os alias: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "compound-autonomy-heartbeat.sh")); !os.IsNotExist(err) {
		t.Fatalf("default public install created the private autonomy heartbeat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "scripts", "attention-letter.py")); !os.IsNotExist(err) {
		t.Fatalf("default public install created the private decision renderer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homePath, home.ManifestName)); err != nil {
		t.Fatalf("home manifest missing: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{"install verification ok", "verdict: ok", "status: ok", "compound installed", "compound workbench"} {
		if !strings.Contains(text, want) {
			t.Fatalf("install output missing %q:\n%s", want, text)
		}
	}
	// The artifact is built to a staging path and only moved in once verified, so a completed
	// install leaves no staged file behind — and a failed one replaces nothing.
	staged, err := filepath.Glob(filepath.Join(installDir, ".*.incoming.*"))
	if err != nil {
		t.Fatalf("glob staged artifacts: %v", err)
	}
	if len(staged) != 0 {
		t.Fatalf("install left staged artifacts behind: %v", staged)
	}
	headOut, headErr := exec.Command("git", "-C", repoRoot, "rev-parse", "--short", "HEAD").Output()
	headSHA := ""
	if headErr == nil {
		headSHA = strings.TrimSpace(string(headOut))
	}
	if _, err := arch.VerifyInstalledArtifact(binaryPath, headSHA); err != nil {
		t.Fatalf("the artifact install.sh actually placed does not verify: %v", err)
	}
}

func TestInstallScriptRefusesForeignOSAliasBeforeBuild(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(installDir, "os")
	foreign := []byte("#!/bin/sh\necho foreign-owner\n")
	if err := os.WriteFile(aliasPath, foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(repoRoot, "install.sh"))
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"GOBIN="+installDir,
		"COMPOUND_HOME="+filepath.Join(t.TempDir(), ".compound"),
		"COMPOUND_INSTALL_OS_ALIAS=1",
	)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "refusing to replace foreign os file") {
		t.Fatalf("foreign os preflight err=%v output=%s", err, out)
	}
	got, readErr := os.ReadFile(aliasPath)
	if readErr != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("foreign os file changed: err=%v body=%q", readErr, got)
	}
	if _, statErr := os.Stat(filepath.Join(installDir, "compound")); !os.IsNotExist(statErr) {
		t.Fatalf("foreign alias refusal still installed compound: %v", statErr)
	}
}

// TestInstallScriptAutonomyLabShipsHeartbeatWithItsHelper runs the opt-in
// autonomy-lab install, which no other test exercises. The heartbeat sources
// $(dirname "$0")/_lib/heartbeat-promote-stage.sh, so the helper must land beside
// it or the installed heartbeat dies on its first tick.
func TestInstallScriptAutonomyLabShipsHeartbeatWithItsHelper(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// This test is about which files ship, not a clean build, so it reuses the
	// caller's warm caches; the cold build belongs to the default-install test.
	cacheOut, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	caches := strings.Split(strings.TrimSpace(string(cacheOut)), "\n")
	if len(caches) != 2 {
		t.Fatalf("go env GOMODCACHE GOCACHE = %q", cacheOut)
	}
	cmd := exec.Command("sh", filepath.Join(repoRoot, "install.sh"))
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"GOBIN="+installDir,
		"COMPOUND_HOME="+filepath.Join(t.TempDir(), ".compound"),
		"GOMODCACHE="+caches[0],
		"GOCACHE="+caches[1],
		"PATH="+installDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"COMPOUND_INSTALL_AUTONOMY_LAB=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("autonomy-lab install.sh failed: %v\n%s", err, out)
	}
	for _, rel := range []string{"compound-autonomy-heartbeat.sh", filepath.Join("_lib", "heartbeat-promote-stage.sh")} {
		if _, err := os.Stat(filepath.Join(installDir, rel)); err != nil {
			t.Fatalf("autonomy-lab install did not ship %s: %v\n%s", rel, err, out)
		}
	}
}

// TestHeartbeatShellSuitesRunUnderBash puts the heartbeat shell suites in the Go
// test inventory so every proof battery runs them. They use $RANDOM under set -u,
// which POSIX sh (dash) aborts on before the first assertion, so each suite must
// declare bash and runs under it here.
func TestHeartbeatShellSuitesRunUnderBash(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, suite := range []struct{ rel, banner string }{
		{"scripts/_lib/heartbeat-promote-stage.test.sh", "ALL TESTS PASSED"},
		{"scripts/_lib/heartbeat-end-to-end.test.sh", "ALL E2E TESTS PASSED"},
	} {
		path := filepath.Join(repoRoot, filepath.FromSlash(suite.rel))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if first, _, _ := strings.Cut(string(body), "\n"); first != "#!/usr/bin/env bash" {
			t.Fatalf("%s declares %q but relies on bash-only $RANDOM", suite.rel, first)
		}
		cmd := exec.Command("bash", path)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), suite.banner) {
			t.Fatalf("%s under bash: err=%v, want %q in output:\n%s", suite.rel, err, suite.banner, out)
		}
	}
}

func TestReviewsCommandShowsReviewQueue(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	if _, err := reviewartifact.Write(homePath, "decision-lifecycle-repairs", "dlrp_cli", map[string]any{
		"id":           "dlrp_cli",
		"status":       "review_required",
		"repair_count": 3,
	}, " ", "# Review\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewartifact.Write(homePath, "route-policy", "rpp_cli", map[string]any{
		"id":     "rpp_cli",
		"status": "ok",
	}, " ", "# OK\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewartifact.Write(homePath, "product-reviews", "prp_cli", map[string]any{
		"id":            "prp_cli",
		"status":        "review_required",
		"product_id":    "product:focus-notes",
		"hypothesis_id": "hyp_focus",
		"signal_id":     "sig_focus",
	}, " ", "# Product review\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewartifact.WriteRaw(homePath, "product-reviews", "prp_cli.review.yaml", []byte("schema_version: compound.product_review.v0.1\n")); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { cmdReviews([]string{"--json"}) })
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("reviews output missing JSON:\n%s", out)
	}
	var report reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(out[start:]), &report); err != nil {
		t.Fatalf("parse reviews report: %v\n%s", err, out)
	}
	if report.SchemaVersion != reviewartifact.QueueSchemaVersion || report.Counts.Total != 3 || report.Counts.ReviewRequired != 2 || report.Counts.OK != 1 {
		t.Fatalf("reviews report = %+v", report)
	}
	if report.Counts.ActionableCount != 2 || report.Counts.ReviewRequiredMissingDecisionCard != 2 || report.Counts.ReviewRequiredMissingAction != 0 {
		t.Fatalf("reviews surface coverage = %+v", report.Counts)
	}
	if report.Items[0].ID != "dlrp_cli" || report.Items[0].RepairCount != 3 {
		t.Fatalf("first review item = %+v", report.Items[0])
	}
	var productItem reviewartifact.QueueItem
	for _, item := range report.Items {
		if item.ID == "prp_cli" {
			productItem = item
			break
		}
	}
	if productItem.ID == "" || productItem.Readiness != "needs_review" || len(productItem.Actions) != 1 || !productItem.Actions[0].RequiresReview {
		t.Fatalf("product review queue item should be review-gated actionable: %+v", productItem)
	}
	if !strings.Contains(productItem.BlockedReason, "review id product_id hypothesis_id required") {
		t.Fatalf("product review queue item should expose validation blocker: %+v", productItem)
	}
	textOut := captureStdout(t, func() { cmdReviews([]string{"--area", "decision-lifecycle-repairs"}) })
	if !strings.Contains(textOut, "reviews total=1") || !strings.Contains(textOut, "review_surface decision_cards=0 actionable=1 missing_cards=1 missing_actions=0 fallback_summaries=") || !strings.Contains(textOut, "decision-lifecycle-repairs/dlrp_cli") || strings.Contains(textOut, "rpp_cli") {
		t.Fatalf("filtered reviews output =\n%s", textOut)
	}
}

func TestReviewsBriefPlanCommandEmitsBoundedInstructionPacket(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	const area = "decision-brief-fixtures"
	if _, err := reviewartifact.Write(homePath, area, "brief_member_a", map[string]any{
		"id":               "brief_member_a",
		"status":           "review_required",
		"created_at":       "2026-07-10T03:00:00Z",
		"evidence_refs":    []string{"compound://evidence/a"},
		"operator_summary": "Review brief_member_a.",
		"context_label":    "Decision brief plan fixture",
		"group_key":        area + "|shared",
	}, "  ", "# Review\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewartifact.Write(homePath, area, "brief_member_b", map[string]any{
		"id":               "brief_member_b",
		"status":           "review_required",
		"created_at":       "2026-07-10T03:01:00Z",
		"evidence_refs":    []string{"compound://evidence/b"},
		"operator_summary": "Review brief_member_b.",
		"context_label":    "Decision brief plan fixture",
		"group_key":        area + "|shared",
	}, "  ", "# Review\n"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { cmdReviews([]string{"brief-plan", "--item", "brief_member_a", "--json"}) })
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("brief-plan output missing JSON:\n%s", out)
	}
	var plan reviewsBriefPlan
	if err := json.Unmarshal([]byte(out[start:]), &plan); err != nil {
		t.Fatalf("parse brief-plan: %v\n%s", err, out)
	}
	if plan.ItemID != "brief_member_a" || plan.Area != area {
		t.Fatalf("plan item/area = %+v", plan)
	}
	wantArtifact := filepath.Join(homePath, "reviews", area, "brief_member_a.json")
	if plan.ArtifactPath != wantArtifact {
		t.Fatalf("artifact_path = %q, want %q", plan.ArtifactPath, wantArtifact)
	}
	wantWrite := filepath.Join(homePath, "reviews", area, "brief_member_a.brief.json")
	if plan.WritePath != wantWrite {
		t.Fatalf("write_path = %q, want %q", plan.WritePath, wantWrite)
	}
	if !sameStringSlice(plan.MemberIDs, []string{"brief_member_a", "brief_member_b"}) {
		t.Fatalf("member_ids = %v, want both group members", plan.MemberIDs)
	}
	if !sameStringSlice(plan.EvidenceRefs, []string{"compound://evidence/a", "compound://evidence/b"}) {
		t.Fatalf("evidence_refs = %v, want both members' refs", plan.EvidenceRefs)
	}
	if plan.BriefSchema["$id"] != reviewartifact.DecisionBriefSchemaVersion {
		t.Fatalf("brief_schema $id = %v, want %v", plan.BriefSchema["$id"], reviewartifact.DecisionBriefSchemaVersion)
	}
	if len(plan.ProposeOnlyWarnings) == 0 {
		t.Fatal("propose_only_warnings must not be empty")
	}

	// A solo item (no shared group_key) plans with only itself as a member.
	if _, err := reviewartifact.Write(homePath, area, "brief_solo", map[string]any{
		"id":               "brief_solo",
		"status":           "review_required",
		"created_at":       "2026-07-10T03:02:00Z",
		"evidence_refs":    []string{"compound://evidence/solo"},
		"operator_summary": "Review brief_solo.",
		"context_label":    "Decision brief plan fixture",
		"group_key":        area + "|solo",
	}, "  ", "# Review\n"); err != nil {
		t.Fatal(err)
	}
	soloOut := captureStdout(t, func() { cmdReviews([]string{"brief-plan", "--item", "brief_solo", "--json"}) })
	soloStart := strings.Index(soloOut, "{")
	var soloPlan reviewsBriefPlan
	if err := json.Unmarshal([]byte(soloOut[soloStart:]), &soloPlan); err != nil {
		t.Fatalf("parse solo brief-plan: %v\n%s", err, soloOut)
	}
	if !sameStringSlice(soloPlan.MemberIDs, []string{"brief_solo"}) {
		t.Fatalf("solo member_ids = %v, want just itself", soloPlan.MemberIDs)
	}

	wantSoloWrite := filepath.Join(homePath, "reviews", area, "brief_solo.brief.json")
	textOut := captureStdout(t, func() { cmdReviews([]string{"brief-plan", "--item", "brief_solo"}) })
	if !strings.Contains(textOut, "brief-plan item=brief_solo") || !strings.Contains(textOut, wantSoloWrite) {
		t.Fatalf("brief-plan text output =\n%s", textOut)
	}
}

func TestReviewsCommandAnnotatesRoutePromotionHold(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	if _, err := reviewartifact.Write(homePath, "route-promotions", "route_promotion_cli", map[string]any{
		"schema_version":   "compound.route_promotion_review.v0.1",
		"id":               "route_promotion_cli",
		"status":           "review_required",
		"recommendation":   "hold_insufficient_evidence",
		"reason":           "samples 2 below min_samples 10",
		"lane_id":          "auto/judge",
		"capability_class": "classify-extract",
		"evidence_refs":    []string{"route_a", "route_b"},
	}, " ", "# Route promotion\n"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { cmdReviews([]string{"--area", "route-promotions", "--json"}) })
	var report reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse reviews report: %v\n%s", err, out)
	}
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 1 || report.Counts.OK != 0 {
		t.Fatalf("route promotion review should remain review debt: %+v", report.Counts)
	}
	if len(report.Items) != 1 || report.Items[0].Readiness != "needs_review" {
		t.Fatalf("route promotion item should be annotated needs_review: %+v", report.Items)
	}
	item := report.Items[0]
	if len(item.Actions) != 1 || !strings.Contains(item.NextAction, "hold lane promotion") {
		t.Fatalf("route promotion item should expose one clean record action and the hold/no-mutation next action: %+v", item)
	}
	record := item.Actions[0]
	if record.ID != "route_promotion_record" || !record.RequiresReview ||
		len(record.Argv) == 0 || record.Argv[0] != "compound" {
		t.Fatalf("route promotion action should be the clean review-required record act: %+v", record)
	}
	for _, token := range record.Argv {
		if token == "--rationale" {
			t.Fatalf("clean record argv must omit --rationale (renderer appends operator text): %v", record.Argv)
		}
	}
	if len(item.EvidenceRefs) != 2 {
		t.Fatalf("route promotion item should preserve evidence refs: %+v", item)
	}
}

func TestReviewsCommandAnnotatesRoutePromotionRecordedHold(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	paths, err := reviewartifact.Write(homePath, "route-promotions", "route_promotion_recorded_cli", map[string]any{
		"schema_version":                   "compound.route_promotion_review.v0.1",
		"id":                               "route_promotion_recorded_cli",
		"status":                           "review_required",
		"recommendation":                   "hold_insufficient_evidence",
		"reason":                           "samples 2 below min_samples 10",
		"lane_id":                          "auto/judge",
		"capability_class":                 "classify-extract",
		"current_claim_level":              "assumed",
		"model_capabilities_authority_ref": routeevidence.ModelCapabilitiesAuthorityRef,
		"criteria_authority_ref":           routeevidence.PromotionCriteriaAuthorityRef,
		"evidence_refs":                    []string{"route_a", "route_b"},
	}, " ", "# Route promotion\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.RecordPromotionHoldReview(homePath, paths.PlanPath, "owner reviewed current samples; keep held until criteria are met", cliTestNow); err != nil {
		t.Fatalf("record promotion hold review: %v", err)
	}

	out := captureStdout(t, func() { cmdReviews([]string{"--area", "route-promotions", "--json"}) })
	var report reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse reviews report: %v\n%s", err, out)
	}
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 0 || report.Counts.OK != 1 {
		t.Fatalf("recorded route promotion hold should be ok: %+v", report.Counts)
	}
	if report.Counts.ByStatus["recorded"] != 1 {
		t.Fatalf("recorded route promotion status missing: %+v", report.Counts.ByStatus)
	}
	if len(report.Items) != 1 || report.Items[0].Status != "recorded" || report.Items[0].Readiness != "review_recorded" {
		t.Fatalf("route promotion recorded hold should be review_recorded: %+v", report.Items)
	}
	item := report.Items[0]
	if len(item.Actions) != 0 || !strings.Contains(item.NextAction, "keep lane promotion held") {
		t.Fatalf("recorded route promotion should expose no-action next step: %+v", item)
	}
}

func TestReviewsCommandAnnotatesPanelVerdictReview(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	if _, err := reviewartifact.Write(homePath, "panel-verdicts", "panel_verdict_cli", map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             "panel_verdict_cli",
		"run_id":         "prun_cli",
		"target": map[string]any{
			"kind":    "memory_candidate",
			"ref":     "cand_panel_cli",
			"summary": "candidate needs human adjudication",
		},
		"lens_findings": []map[string]any{
			{
				"lens_ref":           "lens:evidence-strength",
				"route_evidence_ref": "route_a",
				"status":             "ok",
			},
			{
				"lens_ref":           "lens:privacy",
				"route_evidence_ref": "route_b",
				"status":             "failed",
			},
		},
		"synthesis": map[string]any{
			"schema_version": "compound.panel_synthesis.v0.1",
			"decision":       "needs_human",
			"confidence":     0.62,
			"rationale":      "privacy lens failed",
		},
		"evidence_refs": []string{"route_b", "route_a", "route_a"},
	}, " ", "# Panel verdict\n"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { cmdReviews([]string{"--area", "panel-verdicts", "--json"}) })
	var report reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse reviews report: %v\n%s", err, out)
	}
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 1 || report.Counts.ReadyToApply != 0 || report.Counts.OK != 0 {
		t.Fatalf("panel verdict should remain review debt: %+v", report.Counts)
	}
	if report.Counts.ByStatus["review_required"] != 1 || report.Counts.ByStatus["unknown"] != 0 {
		t.Fatalf("panel verdict projected status should be review_required: %+v", report.Counts.ByStatus)
	}
	if len(report.Items) != 1 || report.Items[0].Status != "review_required" || report.Items[0].Readiness != "needs_review" {
		t.Fatalf("panel verdict item should be annotated needs_review: %+v", report.Items)
	}
	item := report.Items[0]
	if item.DecisionCard == nil {
		t.Fatalf("panel verdict queue should expose a read-only decision card: %+v", item)
	}
	if len(item.Actions) != 5 {
		t.Fatalf("panel verdict queue should expose four shell command hints plus one clean member action: %+v", item)
	}
	cleanActions := 0
	for _, action := range item.Actions {
		if !action.RequiresReview || !strings.Contains(action.Command, "compound panel verdict-record") {
			t.Fatalf("panel verdict action should stay canonical and review-required: %+v", action)
		}
		if len(action.Argv) > 0 && action.Argv[0] == "compound" {
			cleanActions++
			continue
		}
		if !strings.Contains(action.Command, "compound memory adjudicate") {
			t.Fatalf("panel shell action should chain the canonical adjudicate command: %+v", action)
		}
	}
	if cleanActions != 1 {
		t.Fatalf("panel verdict queue should expose exactly one clean-argv member action, got %d: %+v", cleanActions, item.Actions)
	}
	if !strings.Contains(item.NextAction, "panel requested human decision") || !strings.Contains(item.NextAction, "memory_candidate/cand_panel_cli") || !strings.Contains(item.NextAction, "failed_lenses=1") {
		t.Fatalf("panel verdict item should expose owner-review context: %+v", item)
	}
	if len(item.EvidenceRefs) != 2 {
		t.Fatalf("panel verdict item should preserve de-duplicated evidence refs: %+v", item)
	}
}

func TestReviewsCommandAnnotatesAppliedPanelVerdictNoAction(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	paths, err := reviewartifact.Write(homePath, "panel-verdicts", "panel_verdict_applied_cli", map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             "panel_verdict_applied_cli",
		"run_id":         "prun_applied_cli",
		"target": map[string]any{
			"kind":    "memory_candidate",
			"ref":     "cand_panel_applied_cli",
			"summary": "candidate already applied by panel run",
		},
		"lens_findings": []map[string]any{{
			"lens_ref":           "lens:evidence-strength",
			"route_evidence_ref": "route_applied",
			"status":             "failed",
		}},
		"synthesis": map[string]any{
			"schema_version": "compound.panel_synthesis.v0.1",
			"decision":       "park",
			"confidence":     0.74,
			"rationale":      "application already recorded no-promote",
		},
		"evidence_refs": []string{"route_applied"},
	}, " ", "# Panel verdict\n")
	if err != nil {
		t.Fatal(err)
	}
	store, err := panel.Open(homePath)
	if err != nil {
		t.Fatalf("open panel store: %v", err)
	}
	run := panel.Run{
		SchemaVersion: panel.RunSchemaVersion,
		ID:            "prun_applied_cli",
		CreatedAt:     cliTestNow.Format(time.RFC3339),
		Status:        "applied",
		TargetKind:    "memory_candidate",
		Findings: []panel.TargetFinding{{
			TargetRef:       "cand_panel_applied_cli",
			VerdictArtifact: paths.PlanPath,
			LensFindings: []panel.LensFinding{{
				LensRef: "lens:evidence-strength",
				Status:  "failed",
			}},
			Synthesis: panel.Synthesis{
				SchemaVersion: panel.SynthesisSchemaVersion,
				Decision:      "park",
				Confidence:    0.74,
				Rationale:     "application already recorded no-promote",
			},
			Application: panel.Application{
				Applied:           true,
				MappedVerdict:     "park",
				ActionIntentRef:   "act_applied_cli",
				PolicyDecisionRef: "pol_applied_cli",
				CandidateRef:      "cand_panel_applied_cli",
				NoPromoteRef:      "nop_applied_cli",
			},
		}},
		CountsBefore: map[string]int{"observed": 1},
		CountsAfter:  map[string]int{"parked": 1},
	}
	if err := store.SaveRun(run); err != nil {
		t.Fatalf("save panel run: %v", err)
	}

	out := captureStdout(t, func() {
		cmdReviews([]string{"--area", "panel-verdicts", "--json"})
	})
	var report reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse reviews report: %v\n%s", err, out)
	}
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 0 || report.Counts.ReadyToApply != 0 || report.Counts.OK != 1 {
		t.Fatalf("applied panel verdict should be terminal: %+v", report.Counts)
	}
	if report.Counts.ByStatus["applied"] != 1 || report.Counts.ByStatus["review_required"] != 0 {
		t.Fatalf("applied panel verdict projected status should be applied: %+v", report.Counts.ByStatus)
	}
	if len(report.Items) != 1 || report.Items[0].Status != "applied" || report.Items[0].Readiness != "no_action" {
		t.Fatalf("applied panel verdict item should be annotated no_action: %+v", report.Items)
	}
	item := report.Items[0]
	if len(item.Actions) != 0 {
		t.Fatalf("applied panel verdict must not expose mutating apply actions: %+v", item)
	}
	if !strings.Contains(item.NextAction, "already applied by prun_applied_cli") || !strings.Contains(item.NextAction, "memory_candidate/cand_panel_applied_cli") {
		t.Fatalf("applied panel verdict item should expose terminal application context: %+v", item)
	}
}

func TestReviewsCommandResolvesJudgedComparisonPanelVerdict(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	if _, err := reviewartifact.Write(homePath, "panel-verdicts", "panel_verdict_stale_comparison_cli", map[string]any{
		"schema_version": "compound.panel_verdict.v0.1",
		"id":             "panel_verdict_stale_comparison_cli",
		"run_id":         "prun_stale_comparison_cli",
		"target": map[string]any{
			"kind":    "comparison",
			"ref":     "cmp_stale_comparison_cli",
			"summary": "duplicate comparison verdict after target was judged",
		},
		"lens_findings": []map[string]any{},
		"synthesis": map[string]any{
			"schema_version": "compound.panel_synthesis.v0.1",
			"decision":       "park",
			"confidence":     0.1,
			"rationale":      "insufficient evidence",
		},
		"evidence_refs": []string{"route_stale"},
	}, " ", "# Panel verdict\n"); err != nil {
		t.Fatal(err)
	}
	comparisonDir := filepath.Join(homePath, "inbox", "comparisons")
	if err := os.MkdirAll(comparisonDir, 0o755); err != nil {
		t.Fatalf("create comparison inbox: %v", err)
	}
	candidate := map[string]any{
		"schema_version":   panel.ComparisonCandidateSchemaVersion,
		"id":               "cmp_stale_comparison_cli",
		"capability_class": "judge-critique",
		"input_summary":    "same input",
		"input_digest":     "sha256:cmp-stale-comparison-cli",
		"baseline":         map[string]any{"lane_id": "baseline", "output": "baseline answer", "total_tokens": 100, "latency_ms": 1000},
		"candidate":        map[string]any{"lane_id": "candidate", "output": "candidate answer", "total_tokens": 90, "latency_ms": 900},
		"state":            "judged",
		"verdict":          "inconclusive",
		"verdict_artifact": filepath.Join(homePath, "reviews", "panel-verdicts", "panel_verdict_sibling_comparison_cli.json"),
		"created_at":       cliTestNow.Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(candidate, "", " ")
	if err != nil {
		t.Fatalf("marshal comparison candidate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(comparisonDir, "cmp_stale_comparison_cli.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write comparison candidate: %v", err)
	}
	out := captureStdout(t, func() {
		cmdReviews([]string{"--area", "panel-verdicts", "--json"})
	})
	var report reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse reviews report: %v\n%s", err, out)
	}
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 0 || report.Counts.OK != 1 {
		t.Fatalf("resolved comparison panel verdict should be terminal: %+v", report.Counts)
	}
	if report.Counts.ByStatus["resolved"] != 1 {
		t.Fatalf("resolved comparison status missing: %+v", report.Counts.ByStatus)
	}
	item := report.Items[0]
	if item.Status != "resolved" || item.Readiness != "no_action" || len(item.Actions) != 0 {
		t.Fatalf("resolved comparison panel verdict should expose no action: %+v", item)
	}
	if !strings.Contains(item.NextAction, "comparison target already judged") || !strings.Contains(item.NextAction, "inconclusive") {
		t.Fatalf("resolved comparison panel verdict should explain terminal target: %+v", item)
	}
}

func TestReviewsCommandAnnotatesCapabilityEvaluationRecorded(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	if _, err := reviewartifact.Write(homePath, "capability-evaluation-intake", "cep_cli", map[string]any{
		"schema_version": "compound.capability.evaluation_plan.v0.1",
		"id":             "cep_cli",
		"status":         "recorded",
		"evaluation_id":  "capability-evaluation.external.example",
		"capability":     "example-capability",
		"target_path":    "/tmp/capability-manifest.yaml",
		"evidence_refs":  []string{"eval_a", "eval_b", "eval_a"},
	}, " ", "# Capability evaluation\n"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { cmdReviews([]string{"--area", "capability-evaluation-intake", "--json"}) })
	var report reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse reviews report: %v\n%s", err, out)
	}
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 0 || report.Counts.ReadyToApply != 0 || report.Counts.OK != 1 {
		t.Fatalf("capability evaluation recorded item should remain terminal: %+v", report.Counts)
	}
	item := report.Items[0]
	if item.Readiness != "no_action" || len(item.Actions) != 0 {
		t.Fatalf("capability evaluation recorded item should be terminal/non-mutating: %+v", item)
	}
	if !strings.Contains(item.NextAction, "separate reviewed manifest import") {
		t.Fatalf("capability evaluation item should expose graph mutation boundary: %+v", item)
	}
	if len(item.EvidenceRefs) != 2 {
		t.Fatalf("capability evaluation item should preserve de-duplicated evidence refs: %+v", item)
	}
}

func TestReviewsRecordFindingConsumesStandingGrantAndRecordsWorkAction(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	grantSource := filepath.Join(t.TempDir(), "standing-grants.yaml")
	if err := os.WriteFile(grantSource, []byte(`generated_at: "2026-07-01T12:00:00Z"
authority:
  decision_ref: "grant:test-review-finding"
  reviewed_by: "operator:test"
grants:
  - grant_id: grant_review_finding
    capabilities:
      - review.finding
      - review.summary
    target_scope: state/runs/*
    reversibility_ceiling: reversible
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(homePath, grantSource, cliTestNow); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "review finding action is governed by StandingGrant",
		DesiredOutcome:   "review finding recorded under state/runs and linked to WorkEnvelope",
		EvidenceRefs:     []string{"test://standing-grant-review-finding"},
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	runRoot := t.TempDir()
	target := "state/runs/standing-grant-review/finding.json"
	sourceArtifact := "state/runs/agent-session/session-summary.json"
	sourcePath := filepath.Join(runRoot, filepath.FromSlash(sourceArtifact))
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte(`{"finding":"agent session produced review evidence"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceTrace := envelope.TraceEnvelope{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTraceEnvelope,
			ID:            "trace_review_finding_source",
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "compound-test"},
			IDs:           envelope.IDs{WorkID: w.ID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{"test://trace-source"},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Source:        envelope.TraceSource{System: "test-runner", SourceKind: "session.summary"},
		MutationClass: "read",
		Summary:       "test trace source for review finding",
	}
	traceData, err := json.MarshalIndent(sourceTrace, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(homePath, "events", sourceTrace.ID+".json")
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, append(traceData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdReviews([]string{
			"record-finding",
			"--work-id", w.ID,
			"--target", target,
			"--root", runRoot,
			"--summary", "review finding recorded through standing grant",
			"--source-artifact", sourceArtifact,
			"--source-trace", sourceTrace.ID,
			"--evidence-ref", "test://review-input",
		})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("record finding output missing JSON:\n%s", out)
	}
	var report reviewFindingRecord
	if err := json.Unmarshal([]byte(out[start:]), &report); err != nil {
		t.Fatalf("parse record finding report: %v\n%s", err, out)
	}
	if report.Status != "recorded" || report.WorkID != w.ID || report.TargetPath != target {
		t.Fatalf("record finding report = %+v", report)
	}
	if report.PolicyOutcome != "allow_with_obligations" || report.StandingGrantID != "grant_review_finding:review-finding" {
		t.Fatalf("policy did not consume standing grant: %+v", report)
	}
	if len(report.SourceArtifacts) != 1 || report.SourceArtifacts[0].Path != sourceArtifact {
		t.Fatalf("source artifact not recorded: %+v", report.SourceArtifacts)
	}
	if !strings.HasPrefix(report.SourceArtifacts[0].Hash, "sha256:") || !stringSliceContains(report.EvidenceRefs, sourceArtifact) || !stringSliceContains(report.EvidenceRefs, report.SourceArtifacts[0].Hash) {
		t.Fatalf("source artifact evidence not recorded: source=%+v refs=%v", report.SourceArtifacts[0], report.EvidenceRefs)
	}
	if len(report.SourceTraces) != 1 || report.SourceTraces[0].ID != sourceTrace.ID {
		t.Fatalf("source trace not recorded: %+v", report.SourceTraces)
	}
	if !stringSliceContains(report.EvidenceRefs, "trace_envelope:"+sourceTrace.ID) || !stringSliceContains(report.EvidenceRefs, "test://trace-source") {
		t.Fatalf("source trace evidence not recorded: traces=%+v refs=%v", report.SourceTraces, report.EvidenceRefs)
	}
	if _, err := os.Stat(filepath.Join(runRoot, target)); err != nil {
		t.Fatalf("finding json not written: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(filepath.Join(runRoot, target), ".json") + ".md"); err != nil {
		t.Fatalf("finding markdown not written: %v", err)
	}
	got, _, err := store.Show(w.ID)
	if err != nil {
		t.Fatalf("show work: %v", err)
	}
	if !stringSliceContains(got.ActionIntentIDs, report.ActionIntentID) || got.PolicyDecisionID != report.PolicyDecisionID {
		t.Fatalf("work action chain not recorded: runs=%v policy=%q report=%+v", got.ActionIntentIDs, got.PolicyDecisionID, report)
	}
	if !testEventExists(t, homePath, report.ActionIntentID) {
		t.Fatal("action intent envelope not persisted")
	}
	if !testEventExists(t, homePath, report.PolicyDecisionID) {
		t.Fatal("policy decision envelope not persisted")
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("review finding action must not promote claims, stat err=%v", err)
	}
}

func TestReviewsRecordFindingAutoWorkClosesWorkEnvelope(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	grantSource := filepath.Join(t.TempDir(), "standing-grants.yaml")
	if err := os.WriteFile(grantSource, []byte(`
generated_at: "2026-07-01T12:00:00Z"
authority:
  decision_ref: "grant:test-review-finding"
  reviewed_by: "operator:test"
grants:
  - grant_id: grant_review_finding
    capabilities:
      - review.finding
    target_scope: state/runs/*
    reversibility_ceiling: reversible
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(homePath, grantSource, cliTestNow); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
	sourceTrace := envelope.TraceEnvelope{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTraceEnvelope,
			ID:            "trace_auto_review_finding_source",
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "compound-test"},
			IDs:           envelope.IDs{},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{"test://auto-trace-source"},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Source:        envelope.TraceSource{System: "test-runner", SourceKind: "session.summary"},
		MutationClass: "read",
		Summary:       "auto-work trace source for review finding",
	}
	traceData, err := json.MarshalIndent(sourceTrace, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(homePath, "events", sourceTrace.ID+".json")
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, append(traceData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	runRoot := t.TempDir()
	target := "state/runs/auto-review/finding.json"
	out := captureStdout(t, func() {
		cmdReviews([]string{
			"record-finding",
			"--auto-work",
			"--target", target,
			"--root", runRoot,
			"--summary", "auto-work review finding recorded through standing grant",
			"--source-trace", sourceTrace.ID,
		})
	})
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("record finding output missing JSON:\n%s", out)
	}
	var report reviewFindingRecord
	if err := json.Unmarshal([]byte(out[start:]), &report); err != nil {
		t.Fatalf("parse record finding report: %v\n%s", err, out)
	}
	if report.WorkID == "" || report.PolicyOutcome != "allow_with_obligations" || report.StandingGrantID == "" {
		t.Fatalf("auto-work report did not consume grant: %+v", report)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	got, _, err := store.Show(report.WorkID)
	if err != nil {
		t.Fatalf("show auto work: %v", err)
	}
	if got.State != "completed" || got.IDs.TransitionID == "" || !stringSliceContains(got.ActionIntentIDs, report.ActionIntentID) {
		t.Fatalf("auto work not closed through action chain: work=%+v report=%+v", got, report)
	}
	if !testEventExists(t, homePath, got.IDs.TransitionID) {
		t.Fatal("transition envelope not persisted")
	}
	if _, err := os.Stat(filepath.Join(runRoot, target)); err != nil {
		t.Fatalf("finding json not written: %v", err)
	}
}

// TestRecordReviewFindingRecordsActionBeforeArtifactWrite proves the policy-
// enforcement seam runs before any irreversible artifact write. We make the
// artifact directory read-only so the MkdirAll after RecordAction fails. In the
// old ordering (artifact write before RecordAction), the ActionIntent and
// PolicyDecision would never have been recorded; in the fixed ordering they are
// durable even though the artifact cannot be written.
func TestRecordReviewFindingRecordsActionBeforeArtifactWrite(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	grantSource := filepath.Join(t.TempDir(), "standing-grants.yaml")
	if err := os.WriteFile(grantSource, []byte(`generated_at: "2026-07-01T12:00:00Z"
authority:
  decision_ref: "grant:test-review-finding"
  reviewed_by: "operator:test"
grants:
  - grant_id: grant_review_finding
    capabilities:
      - review.finding
    target_scope: state/runs/*
    reversibility_ceiling: reversible
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(homePath, grantSource, cliTestNow); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "review finding action recorded before artifact write",
		DesiredOutcome:   "demonstrate policy seam ordering",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	runRoot := t.TempDir()
	target := "state/runs/ordering-test/finding.json"
	if err := os.Chmod(runRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runRoot, 0o755) })

	_, err = recordReviewFinding(recordReviewFindingInput{
		WorkID:     w.ID,
		TargetPath: target,
		Root:       runRoot,
		Summary:    "ordering regression test",
	}, cliTestNow)
	if err == nil {
		t.Fatal("expected artifact write failure, got nil")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permission denied error, got %v", err)
	}

	if _, serr := os.Stat(filepath.Join(runRoot, target)); !os.IsNotExist(serr) {
		t.Fatalf("artifact file must not exist after failed write: %v", serr)
	}
	markdownPath := strings.TrimSuffix(filepath.Join(runRoot, target), ".json") + ".md"
	if _, serr := os.Stat(markdownPath); !os.IsNotExist(serr) {
		t.Fatalf("markdown file must not exist after failed write: %v", serr)
	}

	got, _, err := store.Show(w.ID)
	if err != nil {
		t.Fatalf("show work: %v", err)
	}
	if got.State != w.State {
		t.Fatalf("work state changed from %q to %q on artifact write failure; must stay unchanged", w.State, got.State)
	}
	if got.IDs.TransitionID != "" {
		t.Fatalf("work transition id must be empty; got %q", got.IDs.TransitionID)
	}
	if len(got.ActionIntentIDs) == 0 || got.PolicyDecisionID == "" {
		t.Fatalf("ActionIntent + PolicyDecision must be recorded before artifact write: work=%+v", got)
	}
	if !testEventExists(t, homePath, got.ActionIntentIDs[0]) {
		t.Fatalf("ActionIntent envelope %q not persisted", got.ActionIntentIDs[0])
	}
	if !testEventExists(t, homePath, got.PolicyDecisionID) {
		t.Fatalf("PolicyDecision envelope %q not persisted", got.PolicyDecisionID)
	}
	// The ActionIntent was recorded before the artifact existed, so its evidence
	// refs must reference inputs/source artifacts/work only, not the target path.
	if len(got.ActionIntentIDs) == 0 {
		t.Fatalf("expected at least one ActionIntentID, got %v", got.ActionIntentIDs)
	}
	var action envelope.ActionIntent
	readTestEventJSON(t, homePath, got.ActionIntentIDs[len(got.ActionIntentIDs)-1], &action)
	if action.Action != "record_review_finding" {
		t.Fatalf("last action intent is %q, want record_review_finding", action.Action)
	}
	if stringSliceContains(action.EvidenceRefs, target) {
		t.Fatalf("ActionIntent evidence_refs must not reference not-yet-written target %q", target)
	}
	if !stringSliceContains(action.EvidenceRefs, "work://"+w.ID) {
		t.Fatalf("ActionIntent evidence_refs must still reference work %q; got %v", w.ID, action.EvidenceRefs)
	}
}

// failingCloseWorkStore wraps a *work.Store and fails Close to simulate a
// mid-lifecycle failure after the review-finding artifact has already been
// written. It satisfies the reviewFindingWorkStore seam used by recordReviewFinding.
type failingCloseWorkStore struct {
	*work.Store
}

func (f *failingCloseWorkStore) Close(workID string, in work.CloseInput, now time.Time) (work.CloseResult, error) {
	return work.CloseResult{}, errors.New("injected close failure after artifact write")
}

// TestRecordReviewFindingLifecycleFailureRollsBackArtifacts proves that if the
// auto-work lifecycle transitions fail after the artifact files have been
// written, the files are rolled back and no orphaned artifact remains.
func TestRecordReviewFindingLifecycleFailureRollsBackArtifacts(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	grantSource := filepath.Join(t.TempDir(), "standing-grants.yaml")
	if err := os.WriteFile(grantSource, []byte(`generated_at: "2026-07-01T12:00:00Z"
authority:
  decision_ref: "grant:test-review-finding"
  reviewed_by: "operator:test"
grants:
  - grant_id: grant_review_finding
    capabilities:
      - review.finding
    target_scope: state/runs/*
    reversibility_ceiling: reversible
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(homePath, grantSource, cliTestNow); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
	runRoot := t.TempDir()
	target := "state/runs/rollback-test/finding.json"

	orig := reviewFindingStoreOpener
	reviewFindingStoreOpener = func(homePath string) (reviewFindingWorkStore, error) {
		store, err := work.Open(homePath)
		if err != nil {
			return nil, err
		}
		return &failingCloseWorkStore{Store: store}, nil
	}
	defer func() { reviewFindingStoreOpener = orig }()

	_, err := recordReviewFinding(recordReviewFindingInput{
		AutoWork:   true,
		TargetPath: target,
		Root:       runRoot,
		Summary:    "rollback regression test",
	}, cliTestNow)
	if err == nil {
		t.Fatal("expected lifecycle failure, got nil")
	}
	if !strings.Contains(err.Error(), "injected close failure") {
		t.Fatalf("expected injected close failure, got %v", err)
	}

	artifactPath := filepath.Join(runRoot, target)
	markdownPath := strings.TrimSuffix(artifactPath, ".json") + ".md"
	if _, serr := os.Stat(artifactPath); !os.IsNotExist(serr) {
		t.Fatalf("artifact file must be rolled back after lifecycle failure: %v", serr)
	}
	if _, serr := os.Stat(markdownPath); !os.IsNotExist(serr) {
		t.Fatalf("markdown file must be rolled back after lifecycle failure: %v", serr)
	}
}

func TestHookSessionEndAutoConsumesCapabilityFeedback(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	graph, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatalf("open capability graph: %v", err)
	}
	if err := graph.Add(capgraph.Node{
		ID:       "skill:session-end-proof",
		Kind:     "skill",
		Name:     "Session End Proof",
		Triggers: []string{"session end proof"},
	}); err != nil {
		t.Fatalf("add capability node: %v", err)
	}

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "session-end validates used capability",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running"}, cliTestNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.RecordAction(
		w.ID,
		testActionIntent(w.ID, "act_session_end_used", "capability://skill:session-end-proof"),
		testPolicyDecision(w.ID, "pol_session_end_used", "act_session_end_used"),
		cliTestNow,
	); err != nil {
		t.Fatalf("record action: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying"}, cliTestNow); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}

	fixturePath := filepath.Join(t.TempDir(), "session-end.json")
	writeJSONFixture(t, fixturePath, hook.Fixture{
		Harness:  "codex",
		WorkID:   w.ID,
		MoveType: "outward_delta",
		Outcome:  "completed",
		Summary:  "session-end closure used capability",
	})

	cmdHook([]string{"session-end", fixturePath})

	edges := readCapabilityEdges(t, homePath)
	if !hasCapabilityEdge(edges, capgraph.EdDeclaredUsedBy, "skill:session-end-proof") {
		t.Fatalf("session-end did not consume declared use; edges=%+v", edges)
	}
	if hasCapabilityEdge(edges, capgraph.EdValidatedBy, "skill:session-end-proof") {
		t.Fatalf("session-end declaration fabricated independent validation; edges=%+v", edges)
	}
}

func TestAuthorityPathRegistryImportsOSRegistryAndResolvesMovedPaths(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	// Skips where the running uid has no /etc/passwd entry (the CI runner executes as
	// 1000:1000): this test builds its fixture FROM the real account home, so without an
	// account there is no fixture to build. Absent precondition, not a weakened gate — the
	// resolver-vs-$HOME guarantee itself is covered environment-independently by
	// internal/authority's TestImportOSPathRegistryPrefersResolverOverProcessHOME.
	account, err := user.Current()
	if err != nil {
		t.Skipf("no OS account for this uid (%v); path-authority import needs a real account home", err)
	}
	accountHome, err := filepath.EvalSymlinks(account.HomeDir)
	if err != nil {
		t.Fatalf("canonical account home: %v", err)
	}
	fakeHome := filepath.Join(t.TempDir(), "spoofed-home")
	t.Setenv("HOME", fakeHome)
	activePath := accountHome
	retiredPath := filepath.Join(accountHome, "retired-path-authority-fixture")
	fixture := filepath.Join(t.TempDir(), "os-path-authority.yaml")
	fixtureBody := strings.Join([]string{
		"version: 1",
		"canonical_roots:",
		"  account_home_probe:",
		`    path: "~"`,
		"    status: active",
		"stale_or_compat_roots:",
		"  retired_probe:",
		`    path: "~/retired-path-authority-fixture"`,
		"    status: retired",
		"",
	}, "\n")
	if err := os.WriteFile(fixture, []byte(fixtureBody), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdAuthority([]string{"path-registry", fixture})
	})
	if !strings.Contains(out, "entities: 2") || !strings.Contains(out, "status: ok") {
		t.Fatalf("path-registry output =\n%s", out)
	}

	active := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", activePath})
	})
	if !strings.Contains(active, "status: resolved") || !strings.Contains(active, "entity: source:account-home-probe") {
		t.Fatalf("active path resolve output =\n%s", active)
	}

	retired := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", retiredPath})
	})
	if !strings.Contains(retired, "status: resolved") || !strings.Contains(retired, "entity: source:retired-probe") {
		t.Fatalf("retired path resolve output =\n%s", retired)
	}

	oldAuthority := captureStdout(t, func() {
		cmdAuthority([]string{"source:retired-probe"})
	})
	if strings.Contains(oldAuthority, "**current_path**") {
		t.Fatalf("retired root must not be current authority:\n%s", oldAuthority)
	}
	if !strings.Contains(oldAuthority, "**known_path_retired**: "+retiredPath) {
		t.Fatalf("retired authority output missing known retired path:\n%s", oldAuthority)
	}
}

func TestAuthorityJobRosterImportsAndReportsAuthority(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	source := filepath.Join(t.TempDir(), "cron-jobs.json")
	writeCLIJobRosterFixture(t, source)

	importOut := captureStdout(t, func() {
		cmdAuthority([]string{"import-job-roster", source})
	})
	for _, want := range []string{
		`"authority_ref": "compound://registries/job-roster"`,
		`"job_count": 2`,
		`"error_count": 1`,
		`"source_role": "evidence_only_input_after_import"`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import job roster output missing %q:\n%s", want, importOut)
		}
	}

	usage := filepath.Join(t.TempDir(), "cron-token-usage.json")
	writeCLIJobTokenUsageFixture(t, usage)
	tokenOut := captureStdout(t, func() {
		cmdAuthority([]string{"import-job-token-governance", usage})
	})
	for _, want := range []string{
		`"token_sessions_count": 8`,
		`"enabled_agent_count": 1`,
		`"high_token_enabled_count": 1`,
	} {
		if !strings.Contains(tokenOut, want) {
			t.Fatalf("import job token governance output missing %q:\n%s", want, tokenOut)
		}
	}

	authorityOut := captureStdout(t, func() {
		cmdAuthority([]string{"job-roster-authority", "--json"})
	})
	for _, want := range []string{
		`"installed": true`,
		`"job_count": 2`,
		`"enabled_count": 2`,
		`"error_count": 1`,
		`"name": "ibkr-exit-check-daily"`,
	} {
		if !strings.Contains(authorityOut, want) {
			t.Fatalf("job roster authority output missing %q:\n%s", want, authorityOut)
		}
	}
	healthOut := captureStdout(t, func() {
		cmdAuthority([]string{"job-health-plan"})
	})
	for _, want := range []string{
		`"schema_version": "compound.job_health_plan_report.v0.1"`,
		`"plan_count": 1`,
		`"job_name": "ibkr-exit-check-daily"`,
		`"PlanPath":`,
	} {
		if !strings.Contains(healthOut, want) {
			t.Fatalf("job health plan output missing %q:\n%s", want, healthOut)
		}
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("job roster command must not promote claims, stat err=%v", err)
	}
}

func writeCLIInfraServicesFixture(t *testing.T, path string) {
	t.Helper()
	body := strings.Join([]string{
		"services:",
		"  example-svc.internal:",
		"    role: example_role_a",
		"    authority: example_authority_a",
		"    status: active",
		"  other-svc.example.invalid:",
		"    role: example_role_b",
		"    authority: example_authority_b",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readNonEmptyLinesCLI(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestAuthorityImportInfraServicesRegistersEntityAndResolvesWithProvenance
// covers F1/F2/F3 of work_c8178a5ab583: import-infra-services registers a
// resolvable service:* entity per row and `authority resolve` prints its
// import provenance additively, while an unrelated token stays unresolved
// (no wildcard leakage).
func TestAuthorityImportInfraServicesRegistersEntityAndResolvesWithProvenance(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	source := filepath.Join(t.TempDir(), "services.yaml")
	writeCLIInfraServicesFixture(t, source)

	before := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "example-svc.internal"})
	})
	if !strings.Contains(before, "status: unresolved") {
		t.Fatalf("expected unresolved before import, got:\n%s", before)
	}

	importOut := captureStdout(t, func() {
		cmdAuthority([]string{"import-infra-services", source})
	})
	for _, want := range []string{
		`"authority_ref": "compound://registries/services"`,
		`"registered_entity_slugs"`,
		`"capability_node_ids"`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import-infra-services output missing %q:\n%s", want, importOut)
		}
	}

	byIdentifier := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "example-svc.internal"})
	})
	for _, want := range []string{
		"status: resolved",
		"entity: service:example-svc",
		"via: identifier",
		"role: example_role_a",
		"authority: example_authority_a",
		"status: active",
		"source_revision: sha256:",
	} {
		if !strings.Contains(byIdentifier, want) {
			t.Fatalf("resolve by identifier missing %q:\n%s", want, byIdentifier)
		}
	}

	byAlias := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "example-svc"})
	})
	if !strings.Contains(byAlias, "entity: service:example-svc") || !strings.Contains(byAlias, "via: alias") {
		t.Fatalf("resolve by alias =\n%s", byAlias)
	}

	unknown := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "nosuch.example.invalid"})
	})
	if !strings.Contains(unknown, "status: unresolved") {
		t.Fatalf("unrelated token should stay unresolved after import, got:\n%s", unknown)
	}
}

// TestAuthorityImportInfraServicesReimportIsIdempotent is F4: re-running
// import-infra-services must not duplicate the entity row or change resolve
// output.
func TestAuthorityImportInfraServicesReimportIsIdempotent(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	source := filepath.Join(t.TempDir(), "services.yaml")
	writeCLIInfraServicesFixture(t, source)

	captureStdout(t, func() { cmdAuthority([]string{"import-infra-services", source}) })
	resolveOnce := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "example-svc.internal"})
	})

	captureStdout(t, func() { cmdAuthority([]string{"import-infra-services", source}) })
	resolveTwice := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "example-svc.internal"})
	})

	if resolveOnce != resolveTwice {
		t.Fatalf("resolve output changed after re-import:\nonce=%s\ntwice=%s", resolveOnce, resolveTwice)
	}

	entityLines := readNonEmptyLinesCLI(t, filepath.Join(homePath, "ledger", "entities.jsonl"))
	count := 0
	for _, line := range entityLines {
		if strings.Contains(line, `"slug":"service:example-svc"`) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("re-import duplicated the entity row: %d matches in %v", count, entityLines)
	}

	nodeLines := readNonEmptyLinesCLI(t, filepath.Join(homePath, "registries", "capability-nodes.jsonl"))
	nodeCount := 0
	for _, line := range nodeLines {
		if strings.Contains(line, `"id":"service:example-svc"`) {
			nodeCount++
		}
	}
	if nodeCount != 1 {
		t.Fatalf("re-import duplicated the capability node row: %d matches in %v", nodeCount, nodeLines)
	}
}

func writeCLIModelCatalogFixture(t *testing.T, path string) {
	t.Helper()
	body := `{
  "schema": "examplecorp/provider-registry@3",
  "models": [
    {
      "provider_id": "examplecorp",
      "model_id": "alpha-flash",
      "display_name": "Alpha Flash",
      "upstream_model_ids": ["alpha-flash-001"],
      "input_micros_per_mtok": 140000,
      "output_micros_per_mtok": 560000,
      "context_window": 128000,
      "tier": "standard",
      "source_url": "https://catalog.invalid/alpha-flash",
      "workload_kind": "chat"
    },
    {
      "provider_id": "examplecorp",
      "model_id": "shared-model",
      "display_name": "Shared Model",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 100000,
      "output_micros_per_mtok": 300000,
      "context_window": 32000,
      "tier": "value",
      "source_url": "",
      "workload_kind": "chat"
    },
    {
      "provider_id": "othercorp",
      "model_id": "shared-model",
      "display_name": "Shared Model Prime",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 90000,
      "output_micros_per_mtok": 270000,
      "context_window": 64000,
      "tier": "value",
      "source_url": "https://catalog.invalid/shared-model-prime",
      "workload_kind": "chat"
    },
    {
      "provider_id": "examplecorp",
      "model_id": "legacy-model",
      "display_name": "Legacy Model",
      "upstream_model_ids": [],
      "input_micros_per_mtok": 50000,
      "output_micros_per_mtok": 150000,
      "context_window": 8000,
      "tier": "legacy",
      "source_url": "",
      "workload_kind": "chat"
    }
  ],
  "deprecated": [
    {
      "model": "legacy-model",
      "providers": ["examplecorp"],
      "status": "deprecated",
      "note": "superseded by alpha-flash"
    }
  ]
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAuthorityImportModelCatalogRegistersEntitiesAndResolvesWithProvenance
// covers F1/F2/F3/F7 of work_07ea9df822a7 end to end through the CLI:
// import-model-catalog registers a resolvable model:* entity per enrollable
// row, `authority resolve` prints its import provenance (incl. an exact
// micros->USD price string) additively, a shared alias resolves multi, and
// the deprecated row stays unresolved.
func TestAuthorityImportModelCatalogRegistersEntitiesAndResolvesWithProvenance(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeCLIModelCatalogFixture(t, source)

	before := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "model:examplecorp/alpha-flash"})
	})
	if !strings.Contains(before, "status: unresolved") {
		t.Fatalf("expected unresolved before import, got:\n%s", before)
	}

	importOut := captureStdout(t, func() {
		cmdAuthority([]string{"import-model-catalog", source})
	})
	for _, want := range []string{
		`"authority_ref": "compound://registries/model-router"`,
		`"registered_entity_slugs"`,
		`"capability_node_ids"`,
		`"deprecated_count": 1`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import-model-catalog output missing %q:\n%s", want, importOut)
		}
	}

	bySlug := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "model:examplecorp/alpha-flash"})
	})
	for _, want := range []string{
		"status: resolved",
		"entity: model:examplecorp/alpha-flash",
		"via: slug",
		"provider: examplecorp",
		"price_in_usd_per_mtok: 0.14",
		"price_out_usd_per_mtok: 0.56",
		"context_window: 128000",
		"tier: standard",
		"source_revision: sha256:",
	} {
		if !strings.Contains(bySlug, want) {
			t.Fatalf("resolve by slug missing %q:\n%s", want, bySlug)
		}
	}

	byAlias := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "alpha-flash"})
	})
	if !strings.Contains(byAlias, "entity: model:examplecorp/alpha-flash") || !strings.Contains(byAlias, "via: alias") {
		t.Fatalf("resolve by alias =\n%s", byAlias)
	}

	bySharedAlias := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "shared-model"})
	})
	if !strings.Contains(bySharedAlias, "status: multi") {
		t.Fatalf("shared alias resolve should be multi, got:\n%s", bySharedAlias)
	}

	deprecated := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "model:examplecorp/legacy-model"})
	})
	if !strings.Contains(deprecated, "status: unresolved") {
		t.Fatalf("deprecated row should stay unresolved, got:\n%s", deprecated)
	}
}

// TestAuthorityImportModelCatalogReimportIsIdempotent is F4 at the CLI level:
// re-running import-model-catalog must not duplicate the entity row or the
// capability node row, and resolve output must stay identical.
func TestAuthorityImportModelCatalogReimportIsIdempotent(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	source := filepath.Join(t.TempDir(), "provider-registry.json")
	writeCLIModelCatalogFixture(t, source)

	captureStdout(t, func() { cmdAuthority([]string{"import-model-catalog", source}) })
	resolveOnce := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "model:examplecorp/alpha-flash"})
	})

	captureStdout(t, func() { cmdAuthority([]string{"import-model-catalog", source}) })
	resolveTwice := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "model:examplecorp/alpha-flash"})
	})

	if resolveOnce != resolveTwice {
		t.Fatalf("resolve output changed after re-import:\nonce=%s\ntwice=%s", resolveOnce, resolveTwice)
	}

	entityLines := readNonEmptyLinesCLI(t, filepath.Join(homePath, "ledger", "entities.jsonl"))
	count := 0
	for _, line := range entityLines {
		if strings.Contains(line, `"slug":"model:examplecorp/alpha-flash"`) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("re-import duplicated the entity row: %d matches in %v", count, entityLines)
	}

	nodeLines := readNonEmptyLinesCLI(t, filepath.Join(homePath, "registries", "capability-nodes.jsonl"))
	nodeCount := 0
	for _, line := range nodeLines {
		if strings.Contains(line, `"id":"model:examplecorp/alpha-flash"`) {
			nodeCount++
		}
	}
	if nodeCount != 1 {
		t.Fatalf("re-import duplicated the capability node row: %d matches in %v", nodeCount, nodeLines)
	}
}

func TestAuthorityGitImportsSourceControlState(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	repo := initCompoundCommandGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdAuthority([]string{"git", repo})
	})
	for _, want := range []string{"# source control", "- status: dirty", "- branch: main", "- unstaged: 1", "- untracked: 1", "- changed_paths: 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("authority git output missing %q:\n%s", want, out)
		}
	}
	jsonOut := captureStdout(t, func() {
		cmdAuthority([]string{"git", repo, "--json"})
	})
	var report struct {
		SchemaVersion  string `json:"schema_version"`
		Status         string `json:"status"`
		AuthorityRef   string `json:"authority_ref"`
		EntityRef      string `json:"entity_ref"`
		GitRoot        string `json:"git_root"`
		Branch         string `json:"branch"`
		Ahead          int    `json:"ahead"`
		Behind         int    `json:"behind"`
		Unstaged       int    `json:"unstaged"`
		Untracked      int    `json:"untracked"`
		ChangedPaths   int    `json:"changed_paths"`
		CandidateCount int    `json:"candidate_count"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("parse authority git JSON: %v\n%s", err, jsonOut)
	}
	wantRepoInfo, err := os.Stat(repo)
	if err != nil {
		t.Fatal(err)
	}
	gotRepoInfo, err := os.Stat(report.GitRoot)
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != "compound.source_control_snapshot.v0.1" ||
		report.Status != "dirty" ||
		report.AuthorityRef != "compound://ledger/source-control" ||
		report.Branch != "main" ||
		report.Ahead != 0 ||
		report.Behind != 0 ||
		report.Unstaged != 1 ||
		report.Untracked != 1 ||
		report.ChangedPaths != 2 ||
		report.CandidateCount == 0 ||
		!strings.HasPrefix(report.EntityRef, "repo:") ||
		!os.SameFile(wantRepoInfo, gotRepoInfo) {
		t.Fatalf("bad authority git JSON: %+v\n%s", report, jsonOut)
	}
	resolved := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", repo})
	})
	if !strings.Contains(resolved, "status: resolved") || !strings.Contains(resolved, "entity: repo:") {
		t.Fatalf("git root did not resolve source-control entity:\n%s", resolved)
	}
}

func TestAuthorityDirtyPlanWritesReviewWithoutMutatingRepo(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	repo := initCompoundCommandGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := compoundCommandGitStatus(t, repo)
	out := captureStdout(t, func() {
		cmdAuthority([]string{"dirty-plan", repo, "--work-id", "work_dirty_gate"})
	})
	for _, want := range []string{
		"# source-control dirty-plan scr_",
		"- status: review_required",
		"- work: work_dirty_gate",
		"- source_status: dirty",
		"- changed_paths: 2",
		"- required_evidence: 4",
		"- evidence_refs: 1",
		"- plan:",
		"- review:",
		"- review_yaml:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dirty-plan output missing %q:\n%s", want, out)
		}
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	reviewYAMLPath := outputValue(out, "- review_yaml:")
	var plan struct {
		SchemaVersion string `json:"schema_version"`
		Status        string `json:"status"`
		WorkID        string `json:"work_id"`
		ChangedPaths  int    `json:"changed_paths"`
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != "compound.source_control_review_plan.v0.1" || plan.Status != "review_required" || plan.WorkID != "work_dirty_gate" || plan.ChangedPaths != 2 {
		t.Fatalf("bad dirty plan: %+v", plan)
	}
	if _, err := os.Stat(reviewYAMLPath); err != nil {
		t.Fatalf("dirty-plan review yaml missing: %v", err)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not stage") || !strings.Contains(review, "Required Evidence") || !strings.Contains(review, "Review YAML Template") {
		t.Fatalf("dirty-plan review missing no-mutation/procedure text:\n%s", review)
	}
	jsonOut := captureStdout(t, func() {
		cmdAuthority([]string{"dirty-plan", repo, "--work-id", "work_dirty_gate", "--json"})
	})
	var dirtyPlanJSON struct {
		SchemaVersion string `json:"schema_version"`
		Status        string `json:"status"`
		AuthorityRef  string `json:"authority_ref"`
		ID            string `json:"id"`
		ChangedPaths  int    `json:"changed_paths"`
		Paths         struct {
			PlanPath       string `json:"plan_path"`
			ReviewPath     string `json:"review_path"`
			ReviewYAMLPath string `json:"review_yaml_path"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &dirtyPlanJSON); err != nil {
		t.Fatalf("parse dirty-plan JSON: %v\n%s", err, jsonOut)
	}
	if dirtyPlanJSON.SchemaVersion != "compound.source_control_review_plan.v0.1" ||
		dirtyPlanJSON.Status != "review_required" ||
		dirtyPlanJSON.AuthorityRef != "compound://reviews/source-control-cleanup" ||
		dirtyPlanJSON.ID == "" ||
		dirtyPlanJSON.ChangedPaths != 2 ||
		dirtyPlanJSON.Paths.PlanPath == "" ||
		dirtyPlanJSON.Paths.ReviewPath == "" ||
		dirtyPlanJSON.Paths.ReviewYAMLPath == "" {
		t.Fatalf("bad dirty-plan JSON: %+v\n%s", dirtyPlanJSON, jsonOut)
	}
	reviewYAML := readFile(t, reviewYAMLPath)
	reviewYAML = strings.ReplaceAll(reviewYAML, "decision: TODO_decision_owner_action_required_owner_action_done_parked_no_action", "decision: parked")
	reviewYAML = strings.ReplaceAll(reviewYAML, `rationale: ""`, `rationale: "untracked scratch is parked for owner classification"`)
	reviewYAML = strings.ReplaceAll(reviewYAML, "confidence: -1", "confidence: 0.9")
	reviewYAML = strings.ReplaceAll(reviewYAML, `next_action: "TODO: reviewed source-control cleanup next action"`, `next_action: "leave scratch parked until owner classifies it"`)
	if err := os.WriteFile(reviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewOut := captureStdout(t, func() {
		cmdAuthority([]string{"dirty-review", reviewYAMLPath, "--json"})
	})
	var reviewReport struct {
		SchemaVersion string `json:"schema_version"`
		Status        string `json:"status"`
		Decision      string `json:"decision"`
		ReviewPath    string `json:"review_path"`
	}
	if err := json.Unmarshal([]byte(reviewOut), &reviewReport); err != nil {
		t.Fatalf("parse dirty-review JSON: %v\n%s", err, reviewOut)
	}
	if reviewReport.SchemaVersion != "compound.source_control_review_report.v0.1" ||
		reviewReport.Status != "recorded" ||
		reviewReport.Decision != "parked" ||
		reviewReport.ReviewPath == "" {
		t.Fatalf("bad dirty-review JSON: %+v\n%s", reviewReport, reviewOut)
	}
	queueOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", "source-control-cleanup", "--json"})
	})
	var queue reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(queueOut), &queue); err != nil {
		t.Fatalf("parse source-control cleanup queue: %v\n%s", err, queueOut)
	}
	if queue.Counts.Total != 1 || queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 || queue.Items[0].Readiness != "review_recorded" {
		t.Fatalf("source-control cleanup queue not recorded: %+v", queue)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("dirty-plan must not promote claims, stat err=%v", err)
	}
	after := compoundCommandGitStatus(t, repo)
	if before != after {
		t.Fatalf("dirty-plan mutated repo\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestHookPreToolUseHoldsDirtySourceControlMutation(t *testing.T) {
	// Home is workspace-local to the target repo (COMPOUND_HOME under the repo it governs),
	// so the dirty mutation stays IN-SCOPE (compound's own governed repo) and the held
	// gate-review mint still fires — see internal/hook's scope+dedup guard.
	repo := initCompoundCommandGitRepo(t)
	homePath := filepath.Join(repo, ".compound")
	if err := home.Init(homePath, home.HomeKindLocal, cliTestNow); err != nil {
		t.Fatalf("init compatible hook home: %v", err)
	}
	t.Setenv("COMPOUND_HOME", homePath)
	target := filepath.Join(repo, "README.md")
	if err := os.WriteFile(target, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture := writeHookFixture(t, hook.Fixture{
		Harness: "claude_code",
		WorkID:  "work_dirty_cli",
		Action: &envelope.ActionIntent{
			Header: envelope.Header{
				SchemaVersion: envelope.SVActionIntent,
				ID:            "act_dirty_cli",
				CreatedAt:     cliTestNow.Format(time.RFC3339),
				Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
				IDs:           envelope.IDs{WorkID: "work_dirty_cli"},
				Authority:     envelope.Authority{SourceClass: "runtime"},
				Privacy:       "private",
				Risk:          "tier1",
				EvidenceRefs:  []string{},
				Uncertainty:   envelope.Uncertainty{Level: "known"},
			},
			Actor:         "claude_code",
			Capability:    "filesystem.write",
			Action:        "write_file",
			Target:        envelope.ActionTarget{Kind: "file", Path: target},
			MutationClass: "local_reversible",
			Reversibility: "easy",
			BlastRadius:   "repo",
		},
	})
	out := captureStdout(t, func() {
		cmdHook([]string{"pre-tool-use", fixture})
	})
	for _, want := range []string{`"status": "blocked"`, `"outcome": "hold"`, "source_control_status dirty", "source-control:repo:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("hook pre-tool-use output missing %q:\n%s", want, out)
		}
	}
	if !testEventExists(t, homePath, "act_dirty_cli") {
		t.Fatal("missing runtime action act_dirty_cli")
	}
	for _, ledgerName := range []string{"entities.jsonl", "claims.jsonl"} {
		if _, err := os.Stat(filepath.Join(homePath, "ledger", ledgerName)); !os.IsNotExist(err) {
			t.Fatalf("pre-tool-use must not create memory ledger %s, stat err=%v", ledgerName, err)
		}
	}
	if len(testEventEntries(t, homePath, "pol_")) == 0 {
		t.Fatalf("policy decision artifact missing in events dir")
	}
	listOut := captureStdout(t, func() {
		cmdWork([]string{"list"})
	})
	for _, want := range []string{"held", "review held policy gate"} {
		if !strings.Contains(listOut, want) {
			t.Fatalf("work list output missing held review %q:\n%s", want, listOut)
		}
	}
}

func TestHookPreToolUseEnforceReturnsGateExit(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := writeHookFixture(t, hook.Fixture{
		Harness: "claude_code",
		WorkID:  "work_enforce",
		Action: &envelope.ActionIntent{
			Header: envelope.Header{
				SchemaVersion: envelope.SVActionIntent,
				ID:            "act_enforce",
				CreatedAt:     cliTestNow.Format(time.RFC3339),
				Producer:      envelope.Producer{Kind: "hook", Name: "compound-test"},
				IDs:           envelope.IDs{WorkID: "work_enforce"},
				Authority:     envelope.Authority{SourceClass: "runtime"},
				Privacy:       "private",
				Risk:          "tier2",
				EvidenceRefs:  []string{},
				Uncertainty:   envelope.Uncertainty{Level: "known"},
			},
			Actor:         "claude_code",
			Capability:    "filesystem.delete",
			Action:        "delete_tree",
			Target:        envelope.ActionTarget{Kind: "path", Path: "build/cache"},
			MutationClass: "destructive",
			Reversibility: "hard",
		},
	})

	var code int
	out := captureStdout(t, func() {
		code = runHookCommand([]string{"pre-tool-use", "--enforce", fixture})
	})
	if code != 3 {
		t.Fatalf("enforced held hook exit = %d, want 3\n%s", code, out)
	}
	for _, want := range []string{`"status": "blocked"`, `"outcome": "hold"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("enforced hook output missing %q:\n%s", want, out)
		}
	}
}

func TestHookPreToolUseEnforceBlocksNativeHarness(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := writeHookFixture(t, hook.Fixture{
		Harness: "codex",
		WorkID:  "work_native_block",
		Action: &envelope.ActionIntent{
			Header: envelope.Header{
				SchemaVersion: envelope.SVActionIntent,
				ID:            "act_native_block",
				CreatedAt:     cliTestNow.Format(time.RFC3339),
				Producer:      envelope.Producer{Kind: "hook", Name: "compound-test"},
				IDs:           envelope.IDs{WorkID: "work_native_block"},
				Authority:     envelope.Authority{SourceClass: "runtime"},
				Privacy:       "private",
				Risk:          "tier2",
				EvidenceRefs:  []string{},
				Uncertainty:   envelope.Uncertainty{Level: "known"},
			},
			Actor:         "codex",
			Capability:    "filesystem.delete",
			Action:        "delete_tree",
			Target:        envelope.ActionTarget{Kind: "path", Path: "build/cache"},
			MutationClass: "destructive",
			Reversibility: "hard",
		},
	})

	var enforced int
	enforcedOut := captureStdout(t, func() {
		enforced = runHookCommand([]string{"pre-tool-use", "--enforce", fixture})
	})
	if enforced != 3 {
		t.Fatalf("enforced native hook exit = %d, want 3\n%s", enforced, enforcedOut)
	}
	if !strings.Contains(enforcedOut, `"status": "blocked"`) {
		t.Fatalf("enforced native hook should preserve blocked evidence:\n%s", enforcedOut)
	}

	var observed int
	observedOut := captureStdout(t, func() {
		observed = runHookCommand([]string{"pre-tool-use", fixture})
	})
	if observed != 0 {
		t.Fatalf("non-enforced fixture hook exit = %d, want 0\n%s", observed, observedOut)
	}
}

func TestHookPreToolUseEnforceBlocksPlacementGuard(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := writeHookFixture(t, hook.Fixture{
		Harness: "codex",
		WorkID:  "work_placement",
		Placement: &policy.PlacementGuardContext{
			Mode:         "block",
			Confidence:   "block",
			OwnerRef:     "owner:decisions",
			TargetRef:    "docs/wrong-home.md",
			Reason:       "requested target does not match owner placement",
			EvidenceRefs: []string{"placement:rule:abc123"},
		},
		Action: &envelope.ActionIntent{
			Header: envelope.Header{
				SchemaVersion: envelope.SVActionIntent,
				ID:            "act_placement",
				CreatedAt:     cliTestNow.Format(time.RFC3339),
				Producer:      envelope.Producer{Kind: "hook", Name: "compound-test"},
				IDs:           envelope.IDs{WorkID: "work_placement"},
				Authority:     envelope.Authority{SourceClass: "runtime"},
				Privacy:       "private",
				Risk:          "tier2",
				EvidenceRefs:  []string{},
				Uncertainty:   envelope.Uncertainty{Level: "known"},
			},
			Actor:         "codex",
			Capability:    "filesystem.write",
			Action:        "write_file",
			Target:        envelope.ActionTarget{Kind: "file", Path: "docs/wrong-home.md"},
			MutationClass: "local_reversible",
			Reversibility: "easy",
			BlastRadius:   "repo",
		},
	})
	var code int
	out := captureStdout(t, func() {
		code = runHookCommand([]string{"pre-tool-use", "--enforce", fixture})
	})
	if code != 3 {
		t.Fatalf("enforced placement hook exit = %d, want 3\n%s", code, out)
	}
	for _, want := range []string{
		`"status": "blocked"`,
		`"outcome": "hold"`,
		"placement_guard mode=block confidence=block",
		"placement:rule:abc123",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("enforced placement output missing %q:\n%s", want, out)
		}
	}
}

func TestPolicyPlacementRulesAuthorityFeedsPreToolUse(t *testing.T) {
	homePath := t.TempDir()
	userHome := t.TempDir()
	if err := home.Init(homePath, home.HomeKindGlobal, cliTestNow); err != nil {
		t.Fatalf("init compatible placement-policy home: %v", err)
	}
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("HOME", userHome)
	t.Setenv("PLACEMENT_GUARD_MODE", "block")

	source := filepath.Join("..", "..", "fixtures", "policy", "placement-rules.json")
	importOut := captureStdout(t, func() {
		cmdPolicy([]string{"import-placement-rules", source})
	})
	for _, want := range []string{
		`"authority_ref": "compound://registries/placement-rules"`,
		`"owner_count": 1`,
		`"open_write_count": 1`,
		`"authority_status": "compound_primary"`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import placement rules output missing %q:\n%s", want, importOut)
		}
	}

	authorityOut := captureStdout(t, func() {
		cmdPolicy([]string{"placement-authority"})
	})
	authorityJSONOut := captureStdout(t, func() {
		cmdPolicy([]string{"placement-authority", "--json"})
	})
	for _, want := range []string{
		`"installed": true`,
		`"placement_guard_mode": "block"`,
		`"policy_source": "built-in"`,
		`"owner_count": 1`,
		`"block_owner_count": 1`,
		`"block_owner_ids": [
    "docs"
  ]`,
		`"source_role": "evidence_only_input_after_import"`,
	} {
		if !strings.Contains(authorityOut, want) {
			t.Fatalf("placement authority output missing %q:\n%s", want, authorityOut)
		}
		if !strings.Contains(authorityJSONOut, want) {
			t.Fatalf("placement authority --json output missing %q:\n%s", want, authorityJSONOut)
		}
	}

	canonicalTarget := filepath.Join(userHome, "compound-owners", "docs", "DOC-0001.md")
	canonicalFixture := writeHookFixture(t, hook.Fixture{
		Harness: "codex",
		WorkID:  "work_placement_authority_canonical",
		Action: &envelope.ActionIntent{
			Header: envelope.Header{
				SchemaVersion: envelope.SVActionIntent,
				ID:            "act_placement_authority_canonical",
				CreatedAt:     cliTestNow.Format(time.RFC3339),
				Producer:      envelope.Producer{Kind: "hook", Name: "compound-test"},
				IDs:           envelope.IDs{WorkID: "work_placement_authority_canonical"},
				Authority:     envelope.Authority{SourceClass: "runtime"},
				Privacy:       "private",
				Risk:          "tier2",
				EvidenceRefs:  []string{},
				Uncertainty:   envelope.Uncertainty{Level: "known"},
			},
			Actor:         "codex",
			Capability:    "filesystem.write",
			Action:        "write_file",
			Target:        envelope.ActionTarget{Kind: "file", Path: canonicalTarget},
			MutationClass: "local_reversible",
			Reversibility: "easy",
			BlastRadius:   "repo",
		},
	})
	var canonicalCode int
	canonicalOut := captureStdout(t, func() {
		canonicalCode = runHookCommand([]string{"pre-tool-use", "--enforce", canonicalFixture})
	})
	if canonicalCode != 0 {
		t.Fatalf("canonical placement hook exit = %d, want 0\n%s", canonicalCode, canonicalOut)
	}
	if strings.Contains(canonicalOut, "placement-owner:docs") {
		t.Fatalf("canonical owner home should not emit placement context:\n%s", canonicalOut)
	}

	target := filepath.Join(userHome, "outside", "DOC-0001.md")
	fixture := writeHookFixture(t, hook.Fixture{
		Harness: "codex",
		WorkID:  "work_placement_authority",
		Action: &envelope.ActionIntent{
			Header: envelope.Header{
				SchemaVersion: envelope.SVActionIntent,
				ID:            "act_placement_authority",
				CreatedAt:     cliTestNow.Format(time.RFC3339),
				Producer:      envelope.Producer{Kind: "hook", Name: "compound-test"},
				IDs:           envelope.IDs{WorkID: "work_placement_authority"},
				Authority:     envelope.Authority{SourceClass: "runtime"},
				Privacy:       "private",
				Risk:          "tier2",
				EvidenceRefs:  []string{},
				Uncertainty:   envelope.Uncertainty{Level: "known"},
			},
			Actor:         "codex",
			Capability:    "filesystem.write",
			Action:        "write_file",
			Target:        envelope.ActionTarget{Kind: "file", Path: target},
			MutationClass: "local_reversible",
			Reversibility: "easy",
			BlastRadius:   "repo",
		},
	})

	var code int
	out := captureStdout(t, func() {
		code = runHookCommand([]string{"pre-tool-use", "--enforce", fixture})
	})
	if code != 3 {
		t.Fatalf("enforced hook exit = %d, want 3\n%s", code, out)
	}
	for _, want := range []string{
		`"status": "blocked"`,
		`"outcome": "hold"`,
		"compound://registries/placement-rules",
		"placement-owner:docs",
		"canon-filename-outside-owner-home",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("hook output missing %q:\n%s", want, out)
		}
	}
}

func TestPolicyOperatorStateAuthorityFeedsPolicyConfig(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	source := filepath.Join("..", "..", "fixtures", "policy", "operator-state-l1.json")
	importOut := captureStdout(t, func() {
		cmdPolicy([]string{"import-operator-state", source})
	})
	for _, want := range []string{
		`"authority_ref": "compound://registries/operator-state"`,
		`"autonomy_level": "L1"`,
		`"authority_status": "compound_primary"`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import operator-state output missing %q:\n%s", want, importOut)
		}
	}

	authorityOut := captureStdout(t, func() {
		cmdPolicy([]string{"operator-authority"})
	})
	authorityJSONOut := captureStdout(t, func() {
		cmdPolicy([]string{"operator-authority", "--json"})
	})
	for _, want := range []string{
		`"installed": true`,
		`"autonomy_level": "L1"`,
		`"source_role": "evidence_only_input_after_import"`,
	} {
		if !strings.Contains(authorityOut, want) {
			t.Fatalf("operator authority output missing %q:\n%s", want, authorityOut)
		}
		if !strings.Contains(authorityJSONOut, want) {
			t.Fatalf("operator authority --json output missing %q:\n%s", want, authorityJSONOut)
		}
	}

	cfg := policyConfig()
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_operator_state",
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound-test"},
			IDs:           envelope.IDs{WorkID: "work_operator_state"},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "codex",
		Capability:    "filesystem.write",
		Action:        "write_file",
		Target:        envelope.ActionTarget{Kind: "file", Path: "docs/x.md"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "repo",
	}
	pd := policy.Evaluate(ai, cfg, cliTestNow)
	if pd.Outcome != "hold" {
		t.Fatalf("L1 operator state should hold local reversible action, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !stringSliceContains(pd.EvidenceRefs, policy.OperatorStateAuthorityRef) {
		t.Fatalf("policy decision missing operator-state authority evidence: %v", pd.EvidenceRefs)
	}
	if !strings.Contains(strings.Join(pd.Reasons, " "), "operator autonomy ceiling L1") {
		t.Fatalf("policy decision missing autonomy reason: %v", pd.Reasons)
	}
}

func TestPolicyOperatorPromptsAuthorityReportsPromptCards(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	source := filepath.Join("..", "..", "fixtures", "policy", "operator-decision-prompts.json")
	importOut := captureStdout(t, func() {
		cmdPolicy([]string{"import-operator-prompts", source})
	})
	for _, want := range []string{
		`"authority_ref": "compound://registries/operator-decision-prompts"`,
		`"prompt_count": 1`,
		`"high_severity_count": 1`,
		`"authority_status": "compound_primary"`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import operator prompts output missing %q:\n%s", want, importOut)
		}
	}

	authorityOut := captureStdout(t, func() {
		cmdPolicy([]string{"operator-prompts-authority"})
	})
	authorityJSONOut := captureStdout(t, func() {
		cmdPolicy([]string{"operator-prompts-authority", "--json"})
	})
	for _, want := range []string{
		`"installed": true`,
		`"prompt_count": 1`,
		`"source_role": "evidence_only_input_after_import"`,
		`"id": "revenue-primary:fixture"`,
	} {
		if !strings.Contains(authorityOut, want) {
			t.Fatalf("operator prompts authority output missing %q:\n%s", want, authorityOut)
		}
		if !strings.Contains(authorityJSONOut, want) {
			t.Fatalf("operator prompts authority --json output missing %q:\n%s", want, authorityJSONOut)
		}
	}
}

func TestPolicyOperatorApprovalFrontierAuthorityReportsDeferredFamilies(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	source := filepath.Join("..", "..", "fixtures", "policy", "operator-approval-frontier-summary.json")
	importOut := captureStdout(t, func() {
		cmdPolicy([]string{"import-operator-approval-frontier", source})
	})
	for _, want := range []string{
		`"authority_ref": "compound://registries/operator-approval-frontier"`,
		`"family_count": 2`,
		`"deferred_count": 2`,
		`"authority_status": "compound_primary"`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import operator approval frontier output missing %q:\n%s", want, importOut)
		}
	}

	authorityOut := captureStdout(t, func() {
		cmdPolicy([]string{"operator-approval-frontier-authority"})
	})
	authorityJSONOut := captureStdout(t, func() {
		cmdPolicy([]string{"operator-approval-frontier-authority", "--json"})
	})
	for _, want := range []string{
		`"installed": true`,
		`"family_count": 2`,
		`"deferred_count": 2`,
		`"source_role": "evidence_only_input_after_import"`,
		`"id": "operator-decision-prompt-cards"`,
	} {
		if !strings.Contains(authorityOut, want) {
			t.Fatalf("operator approval frontier authority output missing %q:\n%s", want, authorityOut)
		}
		if !strings.Contains(authorityJSONOut, want) {
			t.Fatalf("operator approval frontier authority --json output missing %q:\n%s", want, authorityJSONOut)
		}
	}
}

func TestPolicyReleaseGoalHoldsAuthorityReportsHoldBacklog(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	dbPath := filepath.Join(t.TempDir(), "release-orchestrator-state.sqlite")
	createReleaseGoalHoldsDB(t, dbPath)

	importOut := captureStdout(t, func() {
		cmdPolicy([]string{"import-release-goal-holds", dbPath})
	})
	for _, want := range []string{
		`"authority_ref": "compound://registries/release-goal-holds"`,
		`"hold_count": 1`,
		`"authority_status": "compound_primary"`,
		`"source_role": "evidence_only_input_after_import"`,
	} {
		if !strings.Contains(importOut, want) {
			t.Fatalf("import release-goal holds output missing %q:\n%s", want, importOut)
		}
	}
	authorityOut := captureStdout(t, func() {
		cmdPolicy([]string{"release-goal-holds-authority", "--json"})
	})
	for _, want := range []string{
		`"installed": true`,
		`"hold_count": 1`,
		`"goal_id": "release-hold-fixture"`,
		`"app_id": "phone-cleaner"`,
	} {
		if !strings.Contains(authorityOut, want) {
			t.Fatalf("release-goal holds authority output missing %q:\n%s", want, authorityOut)
		}
	}
}

func TestImportedSkillOfferCanRecordDeclaredSessionEndFeedback(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "skill-manifest.yaml")})
	var validationBefore capgraph.Edge
	for _, edge := range readCapabilityEdges(t, homePath) {
		if edge.Kind == capgraph.EdValidatedBy && edge.From == "skill:pricing" {
			validationBefore = edge
		}
	}
	cmdHook([]string{"user-prompt-submit", writeHookFixture(t, hook.Fixture{
		Harness:  "codex",
		Cwd:      "/repo/product",
		RepoKind: "product",
		Task:     "pricing paywall",
		Prompt:   "prepare pricing paywall review",
	})})

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w := onlyWork(t, store)
	if w.IDs.ContextPackID == "" {
		t.Fatal("user-prompt work did not preserve context_pack_id")
	}
	pack := readContextPack(t, homePath, w.IDs.ContextPackID)
	if !contextPackIncludes(pack, "capability://skill:pricing") {
		t.Fatalf("context pack did not include imported pricing capability; included=%+v", pack.Included)
	}

	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "ready"}, cliTestNow); err != nil {
		t.Fatalf("advance ready: %v", err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running"}, cliTestNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.RecordAction(
		w.ID,
		testActionIntent(w.ID, "act_imported_skill_used", "capability://skill:pricing"),
		testPolicyDecision(w.ID, "pol_imported_skill_used", "act_imported_skill_used"),
		cliTestNow,
	); err != nil {
		t.Fatalf("record action: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying"}, cliTestNow); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}

	cmdHook([]string{"session-end", writeHookFixture(t, hook.Fixture{
		Harness:  "codex",
		WorkID:   w.ID,
		MoveType: "outward_delta",
		Outcome:  "completed",
		Summary:  "pricing paywall review used imported skill",
	})})

	edges := readCapabilityEdges(t, homePath)
	if !hasCapabilityEdge(edges, capgraph.EdDeclaredUsedBy, "skill:pricing") {
		t.Fatalf("session-end did not consume imported use declaration; edges=%+v", edges)
	}
	validations := 0
	for _, edge := range edges {
		if edge.Kind == capgraph.EdValidatedBy && edge.From == "skill:pricing" {
			validations++
			if edge.To != "eval:pricing-copy" || edge.SourceReceipt != "" || edge != validationBefore {
				t.Fatalf("session-end changed typed evaluation evidence; edge=%+v", edge)
			}
		}
	}
	if validations != 1 {
		t.Fatalf("session-end created or lost typed evaluation evidence; edges=%+v", edges)
	}
}

func TestImportedSkillOfferCanRecordDeclaredUseInSecondHarnessProductLoop(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "harness-split.yaml")})
	cmdHook([]string{"user-prompt-submit", writeHookFixture(t, hook.Fixture{
		Harness:  "claude_code",
		Cwd:      "/repo/product",
		RepoKind: "product",
		Task:     "pricing paywall",
		Prompt:   "prepare pricing paywall review",
	})})

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w := onlyWork(t, store)
	pack := readContextPack(t, homePath, w.IDs.ContextPackID)
	if !contextPackIncludes(pack, "capability://skill:claude-pricing") {
		t.Fatalf("claude context pack did not include claude-compatible pricing skill; included=%+v", pack.Included)
	}
	if contextPackIncludes(pack, "capability://skill:codex-pricing") {
		t.Fatalf("claude context pack included codex-only skill; included=%+v", pack.Included)
	}

	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "ready"}, cliTestNow); err != nil {
		t.Fatalf("advance ready: %v", err)
	}
	if _, err := store.Claim(w.ID, "claude_code", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "claude_code"}, cliTestNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.RecordAction(
		w.ID,
		testActionIntent(w.ID, "act_imported_claude_skill_used", "capability://skill:claude-pricing"),
		testPolicyDecision(w.ID, "pol_imported_claude_skill_used", "act_imported_claude_skill_used"),
		cliTestNow,
	); err != nil {
		t.Fatalf("record action: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying", Actor: "claude_code"}, cliTestNow); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}

	cmdHook([]string{"session-end", writeHookFixture(t, hook.Fixture{
		Harness:  "claude_code",
		WorkID:   w.ID,
		MoveType: "outward_delta",
		Outcome:  "completed",
		Summary:  "pricing paywall review used imported claude skill",
	})})

	edges := readCapabilityEdges(t, homePath)
	if !hasCapabilityEdge(edges, capgraph.EdDeclaredUsedBy, "skill:claude-pricing") {
		t.Fatalf("session-end did not consume imported claude skill declaration; edges=%+v", edges)
	}
	if hasCapabilityEdge(edges, capgraph.EdValidatedBy, "skill:claude-pricing") {
		t.Fatalf("session-end declaration fabricated independent validation; edges=%+v", edges)
	}
	if hasCapabilityEdge(edges, capgraph.EdDeclaredUsedBy, "skill:codex-pricing") ||
		hasCapabilityEdge(edges, capgraph.EdValidatedBy, "skill:codex-pricing") {
		t.Fatalf("session-end incorrectly credited codex-only skill from claude work; edges=%+v", edges)
	}
}

func TestWorkCloseExplicitFeedbackAutoConsumes(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	graph, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatalf("open capability graph: %v", err)
	}
	for _, node := range []capgraph.Node{
		{ID: "skill:missed-cli", Kind: "skill", Name: "Missed CLI", Triggers: []string{"pricing"}},
		{ID: "skill:false-cli", Kind: "skill", Name: "False CLI", Triggers: []string{"pricing"}},
	} {
		if err := graph.Add(node); err != nil {
			t.Fatalf("add node %s: %v", node.ID, err)
		}
	}

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w, err := store.Create(work.CreateInput{Owner: "codex", SuccessPredicate: "record explicit feedback"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running"}, cliTestNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying"}, cliTestNow); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}

	cmdWork([]string{
		"close", w.ID,
		"--outcome", "failed",
		"--move-type", "outward_delta",
		"--summary", "pricing review",
		"--changed",
		"--missed", "capability://skill:missed-cli",
		"--false-triggered", "capability://skill:false-cli",
	})

	edges := readCapabilityEdges(t, homePath)
	if !hasCapabilityEdgeTo(edges, capgraph.EdMissedWhen, "skill:missed-cli") {
		t.Fatalf("work close did not auto-consume missed feedback; edges=%+v", edges)
	}
	if !hasCapabilityEdgeTo(edges, capgraph.EdFalseTriggeredBy, "skill:false-cli") {
		t.Fatalf("work close did not auto-consume false-trigger feedback; edges=%+v", edges)
	}
}

func TestDistillerRunAutoConsumesCapabilityObservation(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	graph, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatalf("open capability graph: %v", err)
	}
	if err := graph.Add(capgraph.Node{ID: "skill:distill-pricing", Kind: "skill", Name: "Distill Pricing", Triggers: []string{"pricing"}}); err != nil {
		t.Fatalf("add node: %v", err)
	}

	tracePath := writeJSONFile(t, `{
		"trace_ref":"trace_distiller_feedback",
		"candidates":[{
			"kind":"capability_observation",
			"subject":"skill:distill-pricing",
			"statement":"pricing review",
			"detail":"missed_when",
			"confidence":0.9
		}]
	}`)
	cmdDistiller([]string{"run", tracePath})

	edges := readCapabilityEdges(t, homePath)
	if !hasCapabilityEdgeTo(edges, capgraph.EdMissedWhen, "skill:distill-pricing") {
		t.Fatalf("distiller run did not auto-consume capability observation; edges=%+v", edges)
	}
}

func TestDistillerRunDoesNotCreateCapabilityGraph(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	tracePath := writeJSONFile(t, `{
		"trace_ref":"trace_no_graph",
		"candidates":[{
			"kind":"capability_observation",
			"subject":"skill:no-graph",
			"statement":"pricing review",
			"detail":"missed_when",
			"confidence":0.9
		}]
	}`)
	cmdDistiller([]string{"run", tracePath})

	if _, err := os.Stat(filepath.Join(homePath, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("distiller run created capability graph unexpectedly: %v", err)
	}
}

func TestDistillerEvalPlanUsesOperationalizedEvalCandidateWithoutMutatingCorpus(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	tracePath := writeJSONFile(t, `{
 "trace_ref": "trace_eval_plan_candidate",
 "candidates": [{
  "kind": "eval_case",
  "subject": "distill.capability_feedback",
  "statement": "distiller should preserve missed capability feedback as regression coverage",
  "confidence": 0.93,
  "evidence_refs": ["trace:trace_eval_plan_candidate", "receipt:rec_eval_plan"]
 }]
}`)
	captureStdout(t, func() {
		cmdDistiller([]string{"run", tracePath})
	})
	evalRows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.EvalCase)))
	if len(evalRows) != 1 || evalRows[0].Verdict != distiller.Operationalize {
		t.Fatalf("eval candidates = %+v", evalRows)
	}
	corpusDir := filepath.Join(t.TempDir(), "distiller-evals")
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	corpusFile := filepath.Join(corpusDir, "existing-eval.json")
	original := `{"existing":true}` + "\n"
	if err := os.WriteFile(corpusFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdDistiller([]string{"eval-plan", "--candidate", evalRows[0].ID, "--corpus", corpusDir})
	})
	if !strings.Contains(out, "# distiller eval-plan evp_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- target_exists: true") ||
		!strings.Contains(out, "- required_artifacts: 3") ||
		!strings.Contains(out, "- evidence_refs: 2") ||
		!strings.Contains(out, "- plan:") ||
		!strings.Contains(out, "- review:") {
		t.Fatalf("eval-plan output missing proof lines:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	var plan distiller.EvalPlan
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != distiller.EvalPlanSchemaVersion ||
		plan.CandidateID != evalRows[0].ID ||
		plan.CandidateVerdict != distiller.Operationalize ||
		plan.FixtureDraft.SchemaVersion != distiller.EvalCaseSchemaVersion ||
		plan.FixtureDraft.ProposedCaseID == "" ||
		plan.FixtureDraft.RequestTraceRef != "trace:trace_eval_plan_candidate" {
		t.Fatalf("bad eval plan: %+v", plan)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not mutate the eval corpus") ||
		!strings.Contains(review, "Required Before Corpus Apply") {
		t.Fatalf("review missing no-mutation/procedure text:\n%s", review)
	}
	if got := readFile(t, corpusFile); got != original {
		t.Fatalf("corpus mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("eval-plan must not promote claims, stat err=%v", err)
	}
}

func TestDistillerImprovementPlanUsesMethodAndPolicyCandidatesWithoutMutatingTarget(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	tracePath := writeJSONFile(t, `{
 "trace_ref": "trace_improvement_plan_candidates",
 "candidates": [
  {
   "kind": "method_upgrade",
   "subject": "distill.memory.dates",
   "statement": "ground relative dates against trace timestamp before emitting memory candidates",
   "detail": "date_grounding",
   "confidence": 0.91,
   "evidence_refs": ["trace:dates", "eval:before"]
  },
  {
   "kind": "policy_patch",
   "subject": "distill.promotion.policy",
   "statement": "reject model-supplied promotion_basis unless source adapter already proved basis",
   "detail": "promotion_basis_guard",
   "confidence": 0.93,
   "evidence_refs": ["trace:policy", "eval:adversarial"]
  }
 ]
}`)
	captureStdout(t, func() {
		cmdDistiller([]string{"run", tracePath})
	})
	methodRows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade)))
	policyRows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.PolicyPatch)))
	if len(methodRows) != 1 || methodRows[0].Verdict != distiller.Park {
		t.Fatalf("method candidates = %+v", methodRows)
	}
	if len(policyRows) != 1 || policyRows[0].Verdict != distiller.Park {
		t.Fatalf("policy candidates = %+v", policyRows)
	}
	targetPath := filepath.Join(t.TempDir(), "distiller-policy.md")
	original := "# Distiller Policy\n\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		row      distiller.Candidate
		wantKind string
		wantArea string
		wantKey  string
		wantText string
	}{
		{"method", methodRows[0], distiller.KindMethodUpgrade, "method", "distill-memory-dates", "trace timestamp"},
		{"policy", policyRows[0], distiller.KindPolicyPatch, "policy", "distill-promotion-policy", "promotion_basis"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				cmdDistiller([]string{"improvement-plan", "--candidate", tc.row.ID, "--target", targetPath})
			})
			if !strings.Contains(out, "# distiller improvement-plan imp_") ||
				!strings.Contains(out, "- status: review_required") ||
				!strings.Contains(out, "- candidate_kind: "+tc.wantKind) ||
				!strings.Contains(out, "- area: "+tc.wantArea) ||
				!strings.Contains(out, "- improvement_key: "+tc.wantKey) ||
				!strings.Contains(out, "- target_exists: true") ||
				!strings.Contains(out, "- required_evidence: 4") ||
				!strings.Contains(out, "- evidence_refs: 2") ||
				!strings.Contains(out, "- plan:") ||
				!strings.Contains(out, "- review:") {
				t.Fatalf("improvement-plan output missing proof lines:\n%s", out)
			}
			planPath := outputValue(out, "- plan:")
			reviewPath := outputValue(out, "- review:")
			var plan distiller.ImprovementPlan
			planData, err := os.ReadFile(planPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(planData, &plan); err != nil {
				t.Fatal(err)
			}
			if plan.SchemaVersion != distiller.ImprovementPlanSchemaVersion ||
				plan.CandidateID != tc.row.ID ||
				plan.CandidateKind != tc.wantKind ||
				plan.Draft.Area != tc.wantArea ||
				!strings.Contains(plan.Draft.ProposedChange, tc.wantText) {
				t.Fatalf("bad improvement plan: %+v", plan)
			}
			review := readFile(t, reviewPath)
			if !strings.Contains(review, "does not mutate distiller policy") ||
				!strings.Contains(review, "Required Evidence Before Apply") {
				t.Fatalf("review missing no-mutation/procedure text:\n%s", review)
			}
		})
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("distiller target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("improvement-plan must not promote claims, stat err=%v", err)
	}
}

func TestDistillerRulePlanUsesFailureCandidateWithoutMutatingRuleTarget(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	tracePath := writeJSONFile(t, `{
 "trace_ref": "trace_failure_rule_candidate",
 "candidates": [{
  "kind": "failure_pattern",
  "subject": "dirty-source-control-hold",
  "statement": "agent repeatedly tries mutating a dirty repo without cleanup review",
  "detail": "repeated_policy_hold",
  "confidence": 0.91,
  "evidence_refs": ["trace:dirty-hold", "policy:hold"]
 }]
}`)
	captureStdout(t, func() {
		cmdDistiller([]string{"run", tracePath})
	})
	failureRows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.FailurePattern)))
	if len(failureRows) != 1 || failureRows[0].Verdict != distiller.Park {
		t.Fatalf("failure candidates = %+v", failureRows)
	}
	targetPath := filepath.Join(t.TempDir(), "rules.md")
	original := "# Rules\n\nExisting rule source.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdDistiller([]string{"rule-plan", "--candidate", failureRows[0].ID, "--target", targetPath})
	})
	if !strings.Contains(out, "# distiller rule-plan frp_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- failure_key: dirty-source-control-hold") ||
		!strings.Contains(out, "- target_exists: true") ||
		!strings.Contains(out, "- required_evidence: 4") ||
		!strings.Contains(out, "- evidence_refs: 2") ||
		!strings.Contains(out, "- plan:") ||
		!strings.Contains(out, "- review:") {
		t.Fatalf("rule-plan output missing proof lines:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	var plan distiller.FailureRulePlan
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != distiller.FailureRulePlanSchemaVersion ||
		plan.CandidateID != failureRows[0].ID ||
		plan.CandidateVerdict != distiller.Park ||
		plan.RuleDraft.RuleID == "" ||
		!strings.Contains(plan.RuleDraft.Trigger, "agent repeatedly tries") {
		t.Fatalf("bad failure rule plan: %+v", plan)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not mutate doctrine or policy") ||
		!strings.Contains(review, "Required Evidence Before Apply") {
		t.Fatalf("review missing no-mutation/procedure text:\n%s", review)
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("rule target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("rule-plan must not promote claims, stat err=%v", err)
	}
}

func TestDistillerTastePlanUsesTasteCandidateWithoutMutatingTarget(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	tracePath := writeJSONFile(t, `{
 "trace_ref": "trace_taste_plan_candidate",
 "candidates": [{
  "kind": "taste",
  "subject": "product:sample-mobile-app:visuals",
  "statement": "screenshots should feel native, specific, and inspection-ready rather than generic marketing cards",
  "detail": "app_store_visuals",
  "confidence": 0.88,
  "evidence_refs": ["trace:taste", "asset:before-after"]
 }]
}`)
	captureStdout(t, func() {
		cmdDistiller([]string{"run", tracePath})
	})
	tasteRows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.Taste)))
	if len(tasteRows) != 1 || tasteRows[0].Verdict != distiller.Park {
		t.Fatalf("taste candidates = %+v", tasteRows)
	}
	targetPath := filepath.Join(t.TempDir(), "taste.md")
	original := "# Taste\n\nExisting standard source.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdDistiller([]string{"taste-plan", "--candidate", tasteRows[0].ID, "--target", targetPath})
	})
	if !strings.Contains(out, "# distiller taste-plan tsp_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- taste_key: product-sample-mobile-app-visuals") ||
		!strings.Contains(out, "- target_exists: true") ||
		!strings.Contains(out, "- required_evidence: 4") ||
		!strings.Contains(out, "- evidence_refs: 2") ||
		!strings.Contains(out, "- plan:") ||
		!strings.Contains(out, "- review:") {
		t.Fatalf("taste-plan output missing proof lines:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	var plan distiller.TastePlan
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != distiller.TastePlanSchemaVersion ||
		plan.CandidateID != tasteRows[0].ID ||
		plan.CandidateVerdict != distiller.Park ||
		plan.StandardDraft.StandardID == "" ||
		!strings.Contains(plan.StandardDraft.PositiveStandard, "inspection-ready") {
		t.Fatalf("bad taste plan: %+v", plan)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not mutate doctrine or product files") ||
		!strings.Contains(review, "Required Evidence Before Apply") {
		t.Fatalf("review missing no-mutation/procedure text:\n%s", review)
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("taste target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("taste-plan must not promote claims, stat err=%v", err)
	}
}

func TestRoutePlanUsesRouteObservationWithoutMutatingTarget(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	tracePath := writeJSONFile(t, `{
 "trace_ref": "trace_route_policy_candidate",
 "candidates": [{
  "kind": "route_observation",
  "subject": "task:cheap-extract",
  "statement": "use cheap extraction lane for bounded candidate-only summaries when eval gate passes",
  "detail": "model_router_cost_guard",
  "confidence": 0.87,
  "evidence_refs": ["route:req_123", "transition:tr_route"]
 }]
}`)
	captureStdout(t, func() {
		cmdDistiller([]string{"run", tracePath})
	})
	routeRows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.RouteObservation)))
	if len(routeRows) != 1 || routeRows[0].Verdict != distiller.Park {
		t.Fatalf("route candidates = %+v", routeRows)
	}
	targetPath := filepath.Join(t.TempDir(), "route-policy.json")
	original := `{"routes":[]}` + "\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdRoute([]string{"plan", "--candidate", routeRows[0].ID, "--target", targetPath})
	})
	if !strings.Contains(out, "# route plan rpp_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- route_key: task-cheap-extract") ||
		!strings.Contains(out, "- target_exists: true") ||
		!strings.Contains(out, "- required_evidence: 4") ||
		!strings.Contains(out, "- evidence_refs: 2") ||
		!strings.Contains(out, "- plan:") ||
		!strings.Contains(out, "- review:") {
		t.Fatalf("route plan output missing proof lines:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	var plan routeevidence.RoutePolicyPlan
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != routeevidence.RoutePolicyPlanSchemaVersion ||
		plan.CandidateID != routeRows[0].ID ||
		plan.CandidateVerdict != distiller.Park ||
		plan.PolicyDraft.PolicyID == "" ||
		!strings.Contains(plan.PolicyDraft.RoutePreference, "cheap extraction lane") {
		t.Fatalf("bad route policy plan: %+v", plan)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not mutate gateway/provider config") ||
		!strings.Contains(review, "Required Evidence Before Apply") {
		t.Fatalf("review missing no-mutation/procedure text:\n%s", review)
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("route target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("route plan must not promote claims, stat err=%v", err)
	}
}

func TestRouteOutcomeReviewUsesLedgerEvidenceAndReceiptWithoutMutatingTarget(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := filepath.Join("..", "..", "fixtures", "route", "provider-calls.ndjson")
	importOut := captureStdout(t, func() {
		cmdRoute([]string{"import-provider-ledger", fixture, "--tail", "1"})
	})
	routeID := outputValue(importOut, "- last_route_evidence:")
	if routeID == "" {
		t.Fatalf("import output missing route evidence id:\n%s", importOut)
	}
	receiptPath := writeJSONFile(t, `{
 "schema_version": "compound.receipt.v0.1",
 "id": "rec_route_outcome_cli",
 "created_at": "2026-06-29T12:00:00Z",
 "producer": {"kind": "fixture", "name": "compound"},
 "ids": {"work_id": "run_fixture_provider_2"},
 "scope": {"entity_id": "system:compound", "surface": "route"},
 "authority": {"source_class": "runtime", "status": "current"},
 "privacy": "private",
 "risk": "tier1",
 "evidence_refs": ["receipt:evidence"],
 "uncertainty": {"level": "known"},
 "outcome": "failed",
 "move_type": "outward_delta",
 "state_delta": {
  "changed": false,
  "summary": "cheap route failed quality gate",
  "verification": ["fixture quality review failed"]
 },
 "cost": {"model_calls": ["route_fixture"]},
 "skills": {"used": [], "missed": [], "candidate": []},
 "memory": {"candidates": [], "promoted": [], "parked": []},
 "next": {"suggested_work": [], "review_trigger": "review route quality before cheap-lane reuse"}
}`)
	targetPath := filepath.Join(t.TempDir(), "route-policy.json")
	original := `{"routes":[]}` + "\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdRoute([]string{"outcome-review", "--route-id", routeID, "--receipt", receiptPath, "--target", targetPath, "--record"})
	})
	for _, want := range []string{
		"# route outcome-review ror_",
		"- receipt_outcome: failed",
		"- assessment_class: route_remorse",
		"- should_review_policy: true",
		"- candidate: route_outcome_",
		"- no_promote: nop_",
		"- verdict: route_outcome_review_not_policy",
		"- plan:",
		"- review:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("outcome-review output missing %q:\n%s", want, out)
		}
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	var review routeevidence.RouteOutcomeReview
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &review); err != nil {
		t.Fatal(err)
	}
	if review.SchemaVersion != routeevidence.RouteOutcomeReviewSchemaVersion ||
		review.RouteEvidenceID != routeID ||
		review.ReceiptID != "rec_route_outcome_cli" ||
		review.CandidateID == "" ||
		review.NoPromoteID == "" {
		t.Fatalf("bad route outcome review: %+v", review)
	}
	text := readFile(t, reviewPath)
	if !strings.Contains(text, "does not mutate gateway/provider config") ||
		!strings.Contains(text, "Required Evidence Before Policy Change") ||
		!strings.Contains(text, "not billing truth") {
		t.Fatalf("review missing no-mutation/non-claim text:\n%s", text)
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("route target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("route outcome-review must not promote claims, stat err=%v", err)
	}
}

func TestRouteImportProviderLedgerWritesEvidenceWithoutClaims(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	fixture := filepath.Join("..", "..", "fixtures", "route", "provider-calls.ndjson")
	out := captureStdout(t, func() {
		cmdRoute([]string{"import-provider-ledger", fixture, "--tail", "1"})
	})
	if !strings.Contains(out, "- imported: 1") || !strings.Contains(out, "- verdict: metadata_only_not_task_success") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	trID := outputValue(out, "- last_transition:")
	if trID == "" {
		t.Fatalf("missing transition in output:\n%s", out)
	}
	rowData := readFile(t, filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl"))
	if strings.Count(string(rowData), "\n") != 1 ||
		!strings.Contains(string(rowData), "req_fixture_provider_2") ||
		strings.Contains(string(rowData), "req_fixture_provider_1") ||
		strings.Contains(string(rowData), "ignored") {
		t.Fatalf("bad imported route evidence:\n%s", rowData)
	}
	if !testEventExists(t, homePath, trID) {
		t.Fatalf("transition %s not recorded", trID)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("provider-ledger import must not promote claims, stat err=%v", err)
	}
}

func TestRouteCostDoctorRecordsReviewableObservation(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	fixture := filepath.Join("..", "..", "fixtures", "route", "provider-calls-cost-gaps.ndjson")
	out := captureStdout(t, func() {
		cmdRoute([]string{"cost-doctor", fixture, "--record"})
	})
	if !strings.Contains(out, "- status: review") ||
		!strings.Contains(out, "- token_usage_missing_cost: 1") ||
		!strings.Contains(out, "- verdict: route_cost_observation_not_policy") ||
		!strings.Contains(out, "- candidate:") ||
		!strings.Contains(out, "- no_promote:") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	candidateID := outputValue(out, "- candidate:")
	candidatePath := filepath.Join(homePath, candidates.InboxDir(candidates.RouteObservation), candidateID+".json")
	candidateData := readFile(t, candidatePath)
	if !strings.Contains(string(candidateData), "provider ledger has 1 token-usage row") ||
		strings.Contains(string(candidateData), "ignored") {
		t.Fatalf("bad route-cost candidate:\n%s", candidateData)
	}
	noPromoteRows := readNoPromoteDecisionRows(t, homePath)
	if len(noPromoteRows) != 1 ||
		noPromoteRows[0].CandidateRef != candidateID ||
		noPromoteRows[0].OwnerTarget.Kind != "route_cost_coverage" {
		t.Fatalf("no-promote rows = %+v", noPromoteRows)
	}
	targetPath := filepath.Join(t.TempDir(), "route-policy.md")
	original := []byte("# route policy\n")
	if err := os.WriteFile(targetPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	planOut := captureStdout(t, func() {
		cmdRoute([]string{"plan", "--candidate", candidateID, "--target", targetPath})
	})
	if !strings.Contains(planOut, "# route plan") || !strings.Contains(planOut, "- status: review_required") {
		t.Fatalf("route plan did not consume cost observation:\n%s", planOut)
	}
	if got := readFile(t, targetPath); string(got) != string(original) {
		t.Fatalf("route policy target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("cost doctor must not promote claims, stat err=%v", err)
	}
}

func TestCapabilitySkillPlanUsesSkillCandidateWithoutMutatingGraphOrTarget(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	tracePath := writeJSONFile(t, `{
 "trace_ref": "trace_skill_candidate",
 "candidates": [{
  "kind": "skill",
  "subject": "frontend performance lens",
  "statement": "review React component rendering paths before adding memoization",
  "detail": "react_performance_review",
  "confidence": 0.88,
  "evidence_refs": ["trace:skill", "receipt:frontend"]
 }]
}`)
	captureStdout(t, func() {
		cmdDistiller([]string{"run", tracePath})
	})
	skillRows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.Skill)))
	if len(skillRows) != 1 || skillRows[0].Verdict != distiller.Park {
		t.Fatalf("skill candidates = %+v", skillRows)
	}
	targetPath := filepath.Join(t.TempDir(), "SKILL.md")
	original := "# Existing Skill\n\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdCapability([]string{"skill-plan", "--candidate", skillRows[0].ID, "--target", targetPath})
	})
	if !strings.Contains(out, "# capability skill-plan skp_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- skill_key: frontend-performance-lens") ||
		!strings.Contains(out, "- node_id: skill:frontend-performance-lens") ||
		!strings.Contains(out, "- target_exists: true") ||
		!strings.Contains(out, "- required_evidence: 4") ||
		!strings.Contains(out, "- evidence_refs: 2") ||
		!strings.Contains(out, "- plan:") ||
		!strings.Contains(out, "- review:") {
		t.Fatalf("skill-plan output missing proof lines:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	var plan capgraph.SkillPlan
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != capgraph.SkillPlanSchemaVersion ||
		plan.CandidateID != skillRows[0].ID ||
		plan.CandidateVerdict != distiller.Park ||
		plan.NodeDraft.NodeID != "skill:frontend-performance-lens" ||
		!strings.Contains(plan.NodeDraft.Implementation, "React component") {
		t.Fatalf("bad skill plan: %+v", plan)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not mutate CapabilityGraph") ||
		!strings.Contains(review, "Required Evidence Before Import") {
		t.Fatalf("review missing no-mutation/procedure text:\n%s", review)
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("skill target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("skill-plan must not mutate capability graph, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("skill-plan must not promote claims, stat err=%v", err)
	}
}

func TestCapabilitySkillCandidateReviewWritesReviewOnly(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "skill-manifest.yaml")})
	nodesPath := filepath.Join(homePath, "registries", "capability-nodes.jsonl")
	beforeNodes, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	candidatePath := writeJSONFile(t, `{
 "id": "example/skills/pricing",
 "name": "Pricing",
 "slug": "pricing",
 "description": "Review pricing and paywall packaging decisions.",
 "source": "example/skills",
 "source_type": "github",
 "install_url": "https://github.com/example/skills",
 "url": "https://skills.sh/example/skills/pricing",
 "installs": 428400,
 "official": true,
 "files": [{"path": "SKILL.md", "contents": "---\nname: pricing\n---\n"}],
 "audits": [{"provider": "Socket", "status": "pass", "summary": "No alerts", "risk_level": "LOW"}]
}`)
	targetDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("# staged only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdCapability([]string{"skill-candidate-review", candidatePath, "--target", targetDir})
	})
	for _, want := range []string{
		"# capability skill-candidate-review scr_",
		"- candidate_kind: skill",
		"- classification: shared_skill",
		"- lifecycle_stage: staged_candidate",
		"- disposition: merge",
		"- overlaps: ",
		"- plan: ",
		"- review: ",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill-candidate-review output missing %q:\n%s", want, out)
		}
	}
	jsonOut := captureStdout(t, func() {
		cmdCapability([]string{"skill-candidate-review", candidatePath, "--target", targetDir, "--json"})
	})
	var parsed struct {
		Review capgraph.SkillCandidateReview `json:"review"`
		Paths  struct {
			PlanPath       string `json:"plan_path"`
			ReviewPath     string `json:"review_path"`
			ReviewYAMLPath string `json:"review_yaml_path"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		t.Fatalf("skill-candidate-review --json should parse: %v\n%s", err, jsonOut)
	}
	if parsed.Review.ID == "" || parsed.Review.GraphCoherence.DecisionHint != "merge_existing" {
		t.Fatalf("json review missing graph coherence: %+v", parsed.Review)
	}
	if parsed.Paths.PlanPath == "" || parsed.Paths.ReviewPath == "" || parsed.Paths.ReviewYAMLPath == "" {
		t.Fatalf("json paths missing: %+v", parsed.Paths)
	}
	if entries, err := os.ReadDir(filepath.Join(homePath, "reviews", "skill-candidates")); err != nil || len(entries) < 2 {
		t.Fatalf("expected skill candidate review artifacts, entries=%v err=%v", entries, err)
	}
	afterNodes, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterNodes) != string(beforeNodes) {
		t.Fatalf("skill-candidate-review must not mutate graph nodes")
	}
	if entries, err := os.ReadDir(filepath.Join(homePath, candidates.InboxDir(candidates.Skill))); err == nil && len(entries) != 0 {
		t.Fatalf("skill-candidate-review must not write skill inbox candidate, entries=%v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read skill inbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("skill-candidate-review must not promote claims, stat err=%v", err)
	}
}

func TestCapabilitySkillCandidateDecisionJSON(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	candidatePath := writeJSONFile(t, `{
		"id": "example/skills/web-design",
		"name": "Web Design",
		"description": "Review web UI layout and accessibility.",
		"source": "example/skills",
		"url": "https://skills.sh/example/skills/web-design",
		"files": [{"path": "SKILL.md", "contents": "---\nname: web-design\n---\n"}]
	}`)
	captureStdout(t, func() {
		cmdCapability([]string{"skill-candidate-review", candidatePath})
	})
	reviewDir := filepath.Join(homePath, "reviews", "skill-candidates")
	var reviewYAML string
	entries, err := os.ReadDir(reviewDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".review.yaml") {
			reviewYAML = filepath.Join(reviewDir, entry.Name())
		}
	}
	if reviewYAML == "" {
		t.Fatal("review yaml not written")
	}
	body := readFile(t, reviewYAML)
	body = strings.ReplaceAll(body, "TODO_decision_create_candidate_merge_existing_reject_reference_only_keep_needs_review", "merge_existing")
	body = strings.ReplaceAll(body, "rationale: \"\"", "rationale: \"existing skill owner should absorb useful parts\"")
	body = strings.ReplaceAll(body, "confidence: -1", "confidence: 0.75")
	if err := os.WriteFile(reviewYAML, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonOut := captureStdout(t, func() {
		cmdCapability([]string{"skill-candidate-decision", reviewYAML, "--json"})
	})
	var report capgraph.SkillCandidateDecisionReport
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("skill-candidate-decision --json should parse: %v\n%s", err, jsonOut)
	}
	if report.Status != "recorded" || report.Decision != "merge_existing" || report.ReviewID == "" {
		t.Fatalf("unexpected decision report: %+v", report)
	}
	if report.CandidateID != "" || report.CandidatePath != "" {
		t.Fatalf("merge_existing should not create a skill candidate: %+v", report)
	}
	reviewedYAML := readFile(t, reviewYAML)
	refreshJSONOut := captureStdout(t, func() {
		cmdCapability([]string{"skill-candidate-review", candidatePath, "--json"})
	})
	var refreshParsed struct {
		Review capgraph.SkillCandidateReview `json:"review"`
		Paths  struct {
			ReviewYAMLPath string `json:"review_yaml_path"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(refreshJSONOut), &refreshParsed); err != nil {
		t.Fatalf("refreshed skill-candidate-review --json should parse: %v\n%s", err, refreshJSONOut)
	}
	if refreshParsed.Review.Status != "recorded" || refreshParsed.Review.NextAction != "skill candidate intake decision recorded" {
		t.Fatalf("refreshed review should keep terminal decision status: %+v", refreshParsed.Review)
	}
	if got := readFile(t, reviewYAML); got != reviewedYAML || strings.Contains(got, "TODO_decision") {
		t.Fatalf("review refresh should preserve reviewed YAML:\nbefore=%s\nafter=%s", reviewedYAML, got)
	}
	if refreshParsed.Paths.ReviewYAMLPath != reviewYAML {
		t.Fatalf("refreshed review yaml path = %q, want %q", refreshParsed.Paths.ReviewYAMLPath, reviewYAML)
	}
}

func TestCapabilitySkillCandidateDecisionFeedsSkillPlan(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "skill-manifest.yaml")})
	nodesPath := filepath.Join(homePath, "registries", "capability-nodes.jsonl")
	beforeNodes, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	candidatePath := writeJSONFile(t, `{
 "id": "example/skills/pricing",
 "name": "Pricing",
 "slug": "pricing",
 "description": "Review pricing and paywall packaging decisions.",
 "source": "example/skills",
 "source_type": "github",
 "url": "https://skills.sh/example/skills/pricing",
 "files": [{"path": "SKILL.md", "contents": "---\nname: pricing\n---\n"}]
}`)
	captureStdout(t, func() {
		cmdCapability([]string{"skill-candidate-review", candidatePath})
	})
	reviewDir := filepath.Join(homePath, "reviews", "skill-candidates")
	var reviewYAML string
	entries, err := os.ReadDir(reviewDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".review.yaml") {
			reviewYAML = filepath.Join(reviewDir, entry.Name())
		}
	}
	if reviewYAML == "" {
		t.Fatal("review yaml not written")
	}
	body := readFile(t, reviewYAML)
	body = strings.ReplaceAll(body, "TODO_decision_create_candidate_merge_existing_reject_reference_only_keep_needs_review", "create_candidate")
	body = strings.ReplaceAll(body, "rationale: \"\"", "rationale: \"reviewed source should enter skill-plan intake\"")
	body = strings.ReplaceAll(body, "confidence: -1", "confidence: 0.8")
	if err := os.WriteFile(reviewYAML, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdCapability([]string{"skill-candidate-decision", reviewYAML})
	})
	for _, want := range []string{
		"# capability skill-candidate-decision scdr_",
		"- status: candidate_recorded",
		"- decision: create_candidate",
		"- candidate: example-skills-pricing",
		"- candidate_path: ",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill-candidate-decision output missing %q:\n%s", want, out)
		}
	}
	planOut := captureStdout(t, func() {
		cmdCapability([]string{"skill-plan", "--candidate", "example-skills-pricing"})
	})
	if !strings.Contains(planOut, "# capability skill-plan skp_") || !strings.Contains(planOut, "- candidate: example-skills-pricing") {
		t.Fatalf("skill-plan did not consume reviewed candidate:\n%s", planOut)
	}
	afterNodes, err := os.ReadFile(nodesPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterNodes) != string(beforeNodes) {
		t.Fatalf("skill-candidate decision/plan must not mutate graph nodes")
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("skill-candidate decision/plan must not promote claims, stat err=%v", err)
	}
}

func TestRuntimeFeedbackReachesDistillerRequestAndGraph(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "skill-manifest.yaml")})
	cmdHook([]string{"user-prompt-submit", writeHookFixture(t, hook.Fixture{
		Harness:  "codex",
		Cwd:      "/repo/product",
		RepoKind: "product",
		Task:     "pricing paywall",
		Prompt:   "prepare pricing paywall review",
	})})

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w := onlyWork(t, store)
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "ready"}, cliTestNow); err != nil {
		t.Fatalf("advance ready: %v", err)
	}
	if _, err := store.Claim(w.ID, "codex", cliTestNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running"}, cliTestNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "verifying"}, cliTestNow); err != nil {
		t.Fatalf("advance verifying: %v", err)
	}

	cmdHook([]string{"session-end", writeHookFixture(t, hook.Fixture{
		Harness:        "codex",
		WorkID:         w.ID,
		MoveType:       "outward_delta",
		Outcome:        "failed",
		Summary:        "pricing paywall feedback",
		Missed:         []string{"capability://skill:pricing"},
		FalseTriggered: []string{"capability://skill:pricing"},
	})})

	reqJSON := captureStdout(t, func() {
		cmdDistiller([]string{"request", "--trace-ref", "trace_runtime_feedback", "--work-id", w.ID})
	})
	var req distiller.LLMExtractionRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("parse distiller request: %v\n%s", err, reqJSON)
	}
	if len(req.CapabilityFeedback) != 2 {
		t.Fatalf("capability_feedback = %+v, want missed + false-trigger", req.CapabilityFeedback)
	}
	if req.CapabilityFeedback[0].Subject != "skill:pricing" || req.CapabilityFeedback[0].Detail != "missed_when" {
		t.Fatalf("missed feedback not preserved: %+v", req.CapabilityFeedback[0])
	}
	if req.CapabilityFeedback[1].Subject != "skill:pricing" || req.CapabilityFeedback[1].Detail != "false_triggered_by" {
		t.Fatalf("false-trigger feedback not preserved: %+v", req.CapabilityFeedback[1])
	}
	if !strings.Contains(strings.Join(req.Instructions, "\n"), "Do not infer missed_when or false_triggered_by from skills.candidate offers") {
		t.Fatalf("request missing no-inference instruction: %v", req.Instructions)
	}

	before := len(readCapabilityEdges(t, homePath))
	responsePath := writeJSONFile(t, `{
		"schema_version":"compound.distiller.llm_extraction.v0.1",
		"trace_ref":"trace_runtime_feedback_response",
		"source_refs":["receipt:runtime_feedback"],
		"candidates":[
			{"kind":"capability_observation","subject":"skill:pricing","statement":"pricing paywall feedback","detail":"missed_when","confidence":0.9,"evidence_refs":["receipt:runtime_feedback"]},
			{"kind":"capability_observation","subject":"skill:pricing","statement":"pricing paywall feedback","detail":"false_triggered_by","confidence":0.9,"evidence_refs":["receipt:runtime_feedback"]}
		]
	}`)
	cmdDistiller([]string{"run", "--llm-response", responsePath})
	after := readCapabilityEdges(t, homePath)
	if len(after) <= before {
		t.Fatalf("distiller feedback did not add graph edges; before=%d after=%d edges=%+v", before, len(after), after)
	}
	if countCapabilityEdgeTo(after, capgraph.EdMissedWhen, "skill:pricing") < 2 {
		t.Fatalf("missed feedback did not flow through receipt and distiller paths; edges=%+v", after)
	}
	if countCapabilityEdgeTo(after, capgraph.EdFalseTriggeredBy, "skill:pricing") < 2 {
		t.Fatalf("false-trigger feedback did not flow through receipt and distiller paths; edges=%+v", after)
	}
}

func TestProductImportProjectContractPromotesSourceInPlaceIdentity(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{
			"import",
			"--project-contract", filepath.Join("..", "..", "fixtures", "product", "project-contract.yaml"),
			"--release", filepath.Join("..", "..", "fixtures", "product", "release-contract.yaml"),
		})
	})
	if !strings.Contains(out, "# product import product:source_contract_demo") ||
		!strings.Contains(out, "- claims: 13") ||
		!strings.Contains(out, "- signals: 0") ||
		!strings.Contains(out, "- hypotheses: 0") {
		t.Fatalf("product import output missing source-contract proof:\n%s", out)
	}
	resolved := captureStdout(t, func() {
		cmdAuthority([]string{"resolve", "appstore:app-demo-sourcecontract"})
	})
	if !strings.Contains(resolved, "status: resolved") || !strings.Contains(resolved, "entity: product:source_contract_demo") {
		t.Fatalf("app store identifier did not resolve to product entity:\n%s", resolved)
	}
	authJSON := captureStdout(t, func() {
		cmdProduct([]string{"authority", "--json"})
	})
	var auth product.AuthorityReport
	if err := json.Unmarshal([]byte(authJSON), &auth); err != nil {
		t.Fatalf("parse product authority JSON: %v\n%s", err, authJSON)
	}
	if auth.AuthorityRef != product.AuthorityRef || auth.ProductCount != 1 || auth.Products[0].ProductID != "product:source_contract_demo" {
		t.Fatalf("product authority = %+v", auth)
	}
}

func TestProductSignalImportsASCDatapointsSourceInPlace(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{
			"signal",
			"--asc-datapoints", filepath.Join("..", "..", "fixtures", "product", "asc-datapoints.json"),
			"--product", "product:focus-notes",
			"--app-id", "123456789",
			"--metric", "review_count",
		})
	})
	if !strings.Contains(out, "# product signal sig_") || !strings.Contains(out, "- product: product:focus-notes") {
		t.Fatalf("product signal output missing ASC import proof:\n%s", out)
	}
	tl, err := product.LoadTimeline(homePath, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Signals) != 1 || tl.Signals[0].MetricName != "app_store_connect_review_count" || tl.Signals[0].Before != tl.Signals[0].After {
		t.Fatalf("ASC signal timeline = %+v", tl.Signals)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("ASC signal import should not create promoted claims ledger: %v", err)
	}
}

func TestProductSignalImportsASCSalesSeriesWindows(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{
			"signal",
			"--asc-sales-series", filepath.Join("..", "..", "fixtures", "product", "asc-sales-series.jsonl"),
			"--product", "product:focus-notes",
			"--app-id", "123456789",
			"--metric", "units",
			"--before", "2026-07-01/2026-07-03",
			"--after", "2026-07-08/2026-07-10",
			"--change-event", "Changed App Store screenshot order.",
		})
	})
	if !strings.Contains(out, "# product signal sig_") || !strings.Contains(out, "- product: product:focus-notes") {
		t.Fatalf("product signal output missing ASC sales import proof:\n%s", out)
	}
	tl, err := product.LoadTimeline(homePath, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Signals) != 1 || tl.Signals[0].MetricName != "app_store_connect_units" || tl.Signals[0].Before != 9 || tl.Signals[0].After != 18 {
		t.Fatalf("ASC sales signal timeline = %+v", tl.Signals)
	}
	if len(tl.Hypotheses) != 0 {
		t.Fatalf("ASC sales signal should not create hypotheses: %+v", tl.Hypotheses)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("ASC sales signal import should not create promoted claims ledger: %v", err)
	}
}

func TestProductSignalImportsASCFunnelSeriesCoverageGatedWindows(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{
			"signal",
			"--asc-funnel-series", filepath.Join("..", "..", "fixtures", "product", "asc-funnel-series.jsonl"),
			"--product", "product:focus-notes",
			"--app-id", "123456789",
			"--metric", "impressions",
			"--before", "2026-07-01/2026-07-03",
			"--after", "2026-07-08/2026-07-10",
			"--change-event", "Reworked App Store product page screenshots.",
		})
	})
	if !strings.Contains(out, "# product signal sig_") || !strings.Contains(out, "- product: product:focus-notes") {
		t.Fatalf("product signal output missing ASC funnel-series import proof:\n%s", out)
	}
	tl, err := product.LoadTimeline(homePath, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	// Every calendar day has observed v2 impressions evidence, including explicit zeros.
	// The three-day sums remain 300 and 400; no unknown day is silently dropped.
	if len(tl.Signals) != 1 || tl.Signals[0].MetricName != "app_store_connect_impressions" || tl.Signals[0].Before != 300 || tl.Signals[0].After != 400 {
		t.Fatalf("ASC funnel signal timeline = %+v", tl.Signals)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("ASC funnel signal import should not create promoted claims ledger: %v", err)
	}
}

func TestProductChangeStreamImportsSourceIssues(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	fixture := filepath.Join("..", "..", "fixtures", "product", "change-stream-candidates.md")
	out := captureStdout(t, func() {
		cmdProduct([]string{"change-stream", fixture})
	})
	if !strings.Contains(out, "# product change-stream change_stream_candidates:change-stream-candidates.md") ||
		!strings.Contains(out, "- candidates_read: 2") ||
		!strings.Contains(out, "- issues_created: 2") ||
		!strings.Contains(out, "- product: product:focus_notes") {
		t.Fatalf("change-stream output missing proof:\n%s", out)
	}
	replay := captureStdout(t, func() {
		cmdProduct([]string{"change-stream", fixture})
	})
	if !strings.Contains(replay, "- issues_created: 0") || !strings.Contains(replay, "- issues_existing: 2") {
		t.Fatalf("change-stream replay should be idempotent:\n%s", replay)
	}
	doctor, err := product.Doctor(homePath, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.SourceIssueCount != 2 {
		t.Fatalf("product doctor should surface change-stream source issues: %+v", doctor)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("change-stream import should not create promoted claims ledger: %v", err)
	}
}

func TestProductSourceIssuePlanUsesOpenSourceIssueWithoutClosingIt(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := filepath.Join("..", "..", "fixtures", "product", "change-stream-candidates.md")
	importOut := captureStdout(t, func() {
		cmdProduct([]string{"change-stream", fixture})
	})
	issueID := outputValue(importOut, "- issue:")
	if issueID == "" {
		t.Fatalf("change-stream output missing issue:\n%s", importOut)
	}
	targetPath := filepath.Join(t.TempDir(), "product-review.md")
	original := "# Product Review\n\nExisting product review notes.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{"source-issue-plan", "--product", "product:focus_notes", "--issue", issueID, "--target", targetPath})
	})
	if !strings.Contains(out, "# product source-issue-plan psip_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- product: product:focus_notes") ||
		!strings.Contains(out, "- issue: "+issueID) ||
		!strings.Contains(out, "- target_exists: true") ||
		!strings.Contains(out, "- required_evidence: 4") ||
		!strings.Contains(out, "- evidence_refs:") ||
		!strings.Contains(out, "- plan:") ||
		!strings.Contains(out, "- review:") ||
		!strings.Contains(out, "- review_yaml:") {
		t.Fatalf("source-issue-plan output missing proof:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	reviewYAMLPath := outputValue(out, "- review_yaml:")
	var plan product.SourceIssuePlan
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != product.SourceIssuePlanSchemaVersion || plan.IssueID != issueID || plan.ReviewDraft.ClosureCommand == "" {
		t.Fatalf("bad source issue plan = %+v", plan)
	}
	reviewData, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviewData), "result: resolved|waived|false_positive") {
		t.Fatalf("review artifact missing closure template:\n%s", string(reviewData))
	}
	reviewYAMLData, err := os.ReadFile(reviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reviewYAMLData), "TODO_resolved_waived_or_false_positive") {
		t.Fatalf("review YAML should require explicit review result:\n%s", string(reviewYAMLData))
	}
	draft, err := product.LoadSourceIssueReview(reviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := product.ValidateSourceIssueReview(draft); err == nil {
		t.Fatal("generated review YAML draft should not validate before reviewer selects a result")
	}
	afterTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterTarget) != original {
		t.Fatal("source-issue-plan mutated target")
	}
	doctor, err := product.Doctor(homePath, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.SourceIssueCount != 2 {
		t.Fatalf("source issue plan should not close issue, doctor = %+v", doctor)
	}
	reviewedYAML := strings.ReplaceAll(string(reviewYAMLData), "TODO_resolved_waived_or_false_positive", "false_positive")
	reviewedYAML = strings.ReplaceAll(reviewedYAML, "TODO: reviewed source owner evidence", "reviewed source issue was not durable product change")
	reviewedPath := filepath.Join(t.TempDir(), "source-issue-review.yaml")
	if err := os.WriteFile(reviewedPath, []byte(reviewedYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewOut := captureStdout(t, func() {
		cmdProduct([]string{"source-issue-review", reviewedPath})
	})
	if !strings.Contains(reviewOut, "# product source issue review "+plan.ReviewDraft.ReviewID) ||
		!strings.Contains(reviewOut, "- status: closed") ||
		!strings.Contains(reviewOut, "- issue: "+issueID) ||
		!strings.Contains(reviewOut, "- transition:") {
		t.Fatalf("source-issue-review output missing closure proof:\n%s", reviewOut)
	}
	afterClosure, err := product.Doctor(homePath, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if afterClosure.Status != "needs_review" || afterClosure.SourceIssueCount != 1 {
		t.Fatalf("closing one generated source issue should leave one open issue, doctor = %+v", afterClosure)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("source issue plan should not create promoted claims ledger: %v", err)
	}
}

func TestProductSourceIssueReviewResolvedFollowUpCreatesHypothesisCandidate(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	fixture := filepath.Join("..", "..", "fixtures", "product", "change-stream-candidates.md")
	importOut := captureStdout(t, func() {
		cmdProduct([]string{"change-stream", fixture})
	})
	issueID := outputValue(importOut, "- issue:")
	if issueID == "" {
		t.Fatalf("change-stream output missing issue:\n%s", importOut)
	}
	reviewPath := filepath.Join(t.TempDir(), "source-issue-review.yaml")
	reviewYAML := `schema_version: compound.product_source_issue_review.v0.1
id: source_issue_review_follow_up_cli
product_id: product:focus_notes
issue_id: ` + issueID + `
result: resolved
rationale: "reviewed source issue represents a real change worth measuring, not a metric claim yet"
evidence_refs:
  - fixture:source-issue-review
follow_up:
  change_event: "Reviewed change-stream candidate as real App Store screenshot change"
  expected_effect: "clearer first screenshot may improve product page conversion"
  metrics:
    - app_store_product_page_conversion
  observation_window: "14d after reviewed change"
  next_action: "import an explicit ProductSignal after the analytics window"
  evidence_refs:
    - fixture:source-issue-follow-up
`
	if err := os.WriteFile(reviewPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{"source-issue-review", reviewPath})
	})
	hypothesisID := outputValue(out, "- hypothesis:")
	if !strings.Contains(out, "- status: closed") ||
		!strings.Contains(out, "- transition:") ||
		hypothesisID == "" {
		t.Fatalf("source-issue-review follow-up output missing proof:\n%s", out)
	}
	tl, err := product.LoadTimeline(homePath, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Hypotheses) != 1 || tl.Hypotheses[0].ID != hypothesisID {
		t.Fatalf("timeline hypotheses = %+v, want %s", tl.Hypotheses, hypothesisID)
	}
	doctor, err := product.Doctor(homePath, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.PendingReviewCount != 1 || doctor.SourceIssueCount != 1 {
		t.Fatalf("doctor should keep follow-up review debt visible: %+v", doctor)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("follow-up should not create ProductSignal ledger: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("follow-up should not create promoted claims ledger: %v", err)
	}
}

func TestProductCanonicalMetricsCLI(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	fixture := filepath.Join("..", "..", "fixtures", "product", "canonical-latest.json")
	out := captureStdout(t, func() {
		cmdProduct([]string{"canonical-metrics", fixture, "--product", "product:focus-notes"})
	})
	if !strings.Contains(out, "# product canonical-metrics product_metrics:canonical_latest:focus_notes:2026_07_13") ||
		!strings.Contains(out, "- signals_created: 2") ||
		!strings.Contains(out, "- hypotheses_created: 1") ||
		!strings.Contains(out, "- hypothesis: focus-notes.appstore-low-conversion") {
		t.Fatalf("canonical-metrics output missing proof:\n%s", out)
	}
	doctor, err := product.Doctor(homePath, "product:focus-notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "needs_review" || doctor.ReviewReadyCount != 2 {
		t.Fatalf("doctor = %+v", doctor)
	}
}

func TestProductHypothesisPlanUsesDistillerCandidateWithoutMutatingTarget(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	tracePath := filepath.Join(t.TempDir(), "trace.json")
	if err := os.WriteFile(tracePath, []byte(`{
  "trace_ref": "trace_product_hypothesis",
  "candidates": [{
    "kind": "product_hypothesis",
    "subject": "product:focus-notes:app-store",
    "statement": "screenshot clarity may improve product page conversion",
    "detail": "Changed first screenshot to show fast capture moment",
    "confidence": 0.82,
    "evidence_refs": ["trace:product-hypothesis", "product_signal:sig_focus"]
  }]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		cmdDistiller([]string{"run", tracePath})
	})
	rows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.ProductHypothesis)))
	if len(rows) != 1 || rows[0].Verdict != distiller.Park {
		t.Fatalf("product hypothesis candidates = %+v", rows)
	}
	targetPath := filepath.Join(t.TempDir(), "product-strategy.md")
	original := "# Product Strategy\n\nExisting strategy.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{"hypothesis-plan", "--candidate", rows[0].ID, "--target", targetPath})
	})
	if !strings.Contains(out, "# product hypothesis-plan php_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- product: product:focus-notes") ||
		!strings.Contains(out, "- target_exists: true") ||
		!strings.Contains(out, "- required_evidence: 4") ||
		!strings.Contains(out, "- evidence_refs: 2") ||
		!strings.Contains(out, "- plan:") ||
		!strings.Contains(out, "- review:") ||
		!strings.Contains(out, "- signal_draft:") {
		t.Fatalf("hypothesis-plan output missing proof lines:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	signalDraftPath := outputValue(out, "- signal_draft:")
	var plan product.HypothesisPlan
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != product.HypothesisPlanSchemaVersion ||
		plan.CandidateID != rows[0].ID ||
		plan.CandidateVerdict != distiller.Park ||
		plan.ProductID != "product:focus-notes" ||
		plan.ReviewDraft.DecisionGate == "" ||
		!strings.Contains(plan.ExpectedEffect, "conversion") {
		t.Fatalf("bad hypothesis plan: %+v", plan)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not mutate product strategy") ||
		!strings.Contains(review, "Required Evidence Before Action") {
		t.Fatalf("review missing no-mutation/procedure text:\n%s", review)
	}
	signalDraft := readFile(t, signalDraftPath)
	if !strings.Contains(signalDraft, "TODO_before_value") ||
		!strings.Contains(signalDraft, "product_hypothesis:"+rows[0].ID) {
		t.Fatalf("signal draft missing guarded placeholders/evidence:\n%s", signalDraft)
	}
	if _, err := product.LoadSignal(signalDraftPath); err == nil {
		t.Fatal("generated signal draft should not load before reviewer supplies metric values")
	}
	signalYAML := strings.ReplaceAll(signalDraft, "TODO_provider", "app_store_connect")
	signalYAML = strings.ReplaceAll(signalYAML, "TODO_surface", "app-store-page")
	signalYAML = strings.ReplaceAll(signalYAML, "TODO_before_value", "0.041")
	signalYAML = strings.ReplaceAll(signalYAML, "TODO_after_value", "0.049")
	signalPath := filepath.Join(t.TempDir(), "product-signal.yaml")
	if err := os.WriteFile(signalPath, []byte(signalYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	signalOut := captureStdout(t, func() {
		cmdProduct([]string{"signal", signalPath})
	})
	if !strings.Contains(signalOut, "# product signal") ||
		!strings.Contains(signalOut, "- status: ok") ||
		!strings.Contains(signalOut, "- transition:") {
		t.Fatalf("edited signal draft did not import as ProductSignal:\n%s", signalOut)
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("hypothesis plan mutated target:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("hypothesis plan must not promote claims, stat err=%v", err)
	}
}

func TestProductReviewPlanUsesSignalEvidenceWithoutResolvingHypothesis(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	candidateID := "hyp_cli_review_plan"
	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.ProductHypothesis))
	if err := os.MkdirAll(candidateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := product.HypothesisCandidate{
		SchemaVersion:     product.ProductHypothesisCandidate,
		ID:                candidateID,
		Kind:              string(candidates.ProductHypothesis),
		ProductID:         "product:focus_notes",
		ChangeEvent:       "Changed first screenshot to show faster capture.",
		ExpectedEffect:    "product page conversion improves",
		Metrics:           []string{"app_store_product_page_conversion"},
		ObservationWindow: "14d",
		Result:            "unknown",
		NextAction:        "review explicit signal evidence",
		Verdict:           candidates.Park,
		EvidenceRefs:      []string{"product_source_issue_review:cli"},
	}
	candidateData, err := json.MarshalIndent(candidate, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateDir, candidateID+".json"), append(candidateData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	signalID := "sig_cli_review_plan"
	signalPath := filepath.Join(t.TempDir(), "signal.yaml")
	if err := os.WriteFile(signalPath, []byte(`schema_version: compound.product_signal.v0.1
source:
 kind: app_store_connect
 ref: fixture:cli-review-plan
signal:
 id: sig_cli_review_plan
 product_id: product:focus_notes
 surface: app-store-page
 change_event: "Changed first screenshot to show faster capture."
 metric_name: app_store_product_page_conversion
 before: 0.041
 after: 0.049
 window: "14d"
 confidence: 0.66
 confounders:
  - "paid acquisition changed during window"
 evidence_refs:
  - asc:focus:conversion-window
`), 0o644); err != nil {
		t.Fatal(err)
	}
	signalOut := captureStdout(t, func() {
		cmdProduct([]string{"signal", signalPath})
	})
	if !strings.Contains(signalOut, "# product signal "+signalID) ||
		!strings.Contains(signalOut, "- status: ok") ||
		!strings.Contains(signalOut, "- transition:") {
		t.Fatalf("signal output missing proof:\n%s", signalOut)
	}
	doctorOut := captureStdout(t, func() {
		cmdProduct([]string{"doctor", "product:focus_notes"})
	})
	if !strings.Contains(doctorOut, "- review_ready: 1") ||
		!strings.Contains(doctorOut, "- review_plan: compound product review-plan --hypothesis "+candidateID+" --signal "+signalID) ||
		!strings.Contains(doctorOut, "match: metric_match app_store_product_page_conversion 14d") {
		t.Fatalf("doctor should surface review-ready signal pair:\n%s", doctorOut)
	}
	targetPath := filepath.Join(t.TempDir(), "product-review.md")
	original := "# Product Review\n\nExisting review notes.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdProduct([]string{"review-plan", "--hypothesis", candidateID, "--signal", signalID, "--target", targetPath})
	})
	if !strings.Contains(out, "# product review-plan prp_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- hypothesis: "+candidateID) ||
		!strings.Contains(out, "- signal: "+signalID) ||
		!strings.Contains(out, "- review_yaml:") {
		t.Fatalf("review-plan output missing proof lines:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewYAMLPath := outputValue(out, "- review_yaml:")
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var plan product.ReviewPlan
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != product.ReviewPlanSchemaVersion ||
		plan.HypothesisID != candidateID ||
		plan.SignalID != signalID ||
		plan.ReviewDraft.ReviewID == "" {
		t.Fatalf("bad review plan: %+v", plan)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	unedited, err := product.LoadReview(reviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.ApplyReview(homePath, store, unedited, reviewYAMLPath, cliTestNow.Add(time.Hour)); err == nil {
		t.Fatal("unedited review YAML should be rejected")
	}
	reviewYAML := readFile(t, reviewYAMLPath)
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_result_supported_refuted_or_inconclusive", "supported")
	reviewYAML = strings.ReplaceAll(reviewYAML, "rationale: \"\"", "rationale: \"signal moved in the expected direction after confounder review\"")
	reviewYAML = strings.ReplaceAll(reviewYAML, "confidence: -1", "confidence: 0.61")
	reviewYAML = strings.ReplaceAll(reviewYAML, "next_action: \"TODO: next action after reviewed evidence\"", "next_action: \"prepare next screenshot experiment\"")
	if err := os.WriteFile(reviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewOut := captureStdout(t, func() {
		cmdProduct([]string{"review", reviewYAMLPath, "--reviewed-by", "operator"})
	})
	if !strings.Contains(reviewOut, "# product review "+plan.ReviewDraft.ReviewID) ||
		!strings.Contains(reviewOut, "- status: updated") ||
		!strings.Contains(reviewOut, "- candidate_outcome: committed") ||
		!strings.Contains(reviewOut, "- transition:") {
		t.Fatalf("product review output missing proof:\n%s", reviewOut)
	}
	doctor, err := product.Doctor(homePath, "product:focus_notes")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" || doctor.PendingReviewCount != 0 {
		t.Fatalf("review should clear pending hypothesis debt: %+v", doctor)
	}
	reviewsOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", "product-reviews", "--json"})
	})
	var queue reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(reviewsOut), &queue); err != nil {
		t.Fatalf("parse review queue: %v\n%s", err, reviewsOut)
	}
	if queue.Counts.Total != 1 || queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 {
		t.Fatalf("review queue should treat recorded product review terminal: %+v", queue.Counts)
	}
	if len(queue.Items) != 1 || queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "no_action" || len(queue.Items[0].Actions) != 0 {
		t.Fatalf("product review queue item should be terminal: %+v", queue.Items)
	}
	loopsOut := captureStdout(t, func() {
		cmdProduct([]string{"loops", "--json", "--product", "product:focus_notes"})
	})
	var loops product.LoopModel
	if err := json.Unmarshal([]byte(loopsOut), &loops); err != nil {
		t.Fatalf("parse product loops: %v\n%s", err, loopsOut)
	}
	if err := loops.Validate(); err != nil {
		t.Fatalf("validate product loops: %v\n%s", err, loopsOut)
	}
	if len(loops.Products) != 1 {
		t.Fatalf("product loops should contain one product: %+v", loops.Products)
	}
	for _, hypothesis := range loops.Products[0].OpenHypotheses {
		if hypothesis.ID == candidateID {
			t.Fatalf("reviewed hypothesis should leave open_hypotheses: %+v", loops.Products[0].OpenHypotheses)
		}
	}
	var sawReview bool
	for _, loopReview := range loops.Products[0].Reviews {
		if loopReview.ID == plan.ReviewDraft.ReviewID &&
			loopReview.HypothesisID == candidateID &&
			loopReview.SignalID == signalID &&
			loopReview.Verdict == "supported" {
			sawReview = true
		}
	}
	if !sawReview {
		t.Fatalf("product loops should retain review history with signal ref: %+v", loops.Products[0].Reviews)
	}
	var sawNextActionGroup bool
	for _, group := range loops.Products[0].ClosedReviewsByNextAction {
		if group.NextAction == "prepare next screenshot experiment" &&
			stringSliceContains(group.ReviewIDs, plan.ReviewDraft.ReviewID) &&
			stringSliceContains(group.HypothesisIDs, candidateID) &&
			stringSliceContains(group.SignalIDs, signalID) {
			sawNextActionGroup = true
		}
	}
	if !sawNextActionGroup {
		t.Fatalf("product loops should group closed review by next action: %+v", loops.Products[0].ClosedReviewsByNextAction)
	}
	afterDoctorOut := captureStdout(t, func() {
		cmdProduct([]string{"doctor", "product:focus_notes"})
	})
	if !strings.Contains(afterDoctorOut, "- review_ready: 0") ||
		strings.Contains(afterDoctorOut, "- review_plan:") {
		t.Fatalf("doctor should clear review-ready pair after review:\n%s", afterDoctorOut)
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("review-plan mutated target:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("review-plan must not promote claims, stat err=%v", err)
	}
}

func TestSourceObserveWritesCandidateAndNoPromoteWithoutPromotion(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdSource([]string{"observe", filepath.Join("..", "..", "fixtures", "sourceobs", "generated-skill-manifest.yaml")})
	})
	if !strings.Contains(out, "# source observation srcobs_fixture_skill_manifest") ||
		!strings.Contains(out, "- candidate:") ||
		!strings.Contains(out, "- no_promote:") {
		t.Fatalf("source observe output missing proof lines:\n%s", out)
	}

	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read candidate dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("candidate entries = %d, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(candidateDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	var candidate sourceobs.CandidateRecord
	if err := json.Unmarshal(data, &candidate); err != nil {
		t.Fatalf("parse candidate: %v", err)
	}
	if candidate.Kind != string(candidates.CapabilityObs) ||
		candidate.Verdict != candidates.NoPromote ||
		candidate.PromoteAllowed {
		t.Fatalf("candidate promotion state = kind %q verdict %q allowed %v", candidate.Kind, candidate.Verdict, candidate.PromoteAllowed)
	}
	if candidate.SourceObservationID != "srcobs_fixture_skill_manifest" {
		t.Fatalf("candidate source observation = %q", candidate.SourceObservationID)
	}

	noPromoteRows := readNoPromoteDecisionRows(t, homePath)
	if len(noPromoteRows) != 1 {
		t.Fatalf("no-promote rows = %d, want 1", len(noPromoteRows))
	}
	if noPromoteRows[0].CandidateRef != candidate.ID ||
		noPromoteRows[0].OwnerTarget.Kind != "source_observation" ||
		noPromoteRows[0].OwnerTarget.Ref != "srcobs_fixture_skill_manifest" {
		t.Fatalf("no-promote row not tied to source observation candidate: %+v candidate=%s", noPromoteRows[0], candidate.ID)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("source observe should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestSourceObserveImportsSecondOpinionAsMethodUpgradeNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdSource([]string{"observe", filepath.Join("..", "..", "fixtures", "sourceobs", "oracle-second-opinion.yaml")})
	})
	if !strings.Contains(out, "# source observation srcobs_fixture_oracle_second_opinion") ||
		!strings.Contains(out, "- candidate:") ||
		!strings.Contains(out, "- no_promote:") {
		t.Fatalf("source observe output missing second-opinion proof lines:\n%s", out)
	}
	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read candidate dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("candidate entries = %d, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(candidateDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read candidate: %v", err)
	}
	var candidate sourceobs.CandidateRecord
	if err := json.Unmarshal(data, &candidate); err != nil {
		t.Fatalf("parse candidate: %v", err)
	}
	if candidate.Kind != string(candidates.MethodUpgrade) ||
		candidate.Subject != "oracle-second-opinion" ||
		candidate.Verdict != candidates.NoPromote ||
		candidate.PromoteAllowed {
		t.Fatalf("second-opinion candidate should be method_upgrade/no_promote: %+v", candidate)
	}
	if !stringSliceContains(candidate.EvidenceRefs, "oracle_session:fixture") ||
		!stringSliceContains(candidate.EvidenceRefs, "source:fixtures/oracle/second-opinion-answer.md#sha256:oraclefixture") {
		t.Fatalf("second-opinion candidate evidence refs = %v", candidate.EvidenceRefs)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("second-opinion source observation must not promote claims, stat err=%v", err)
	}
}

func TestCapabilityImportSkillsRecordsSourceAuthority(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "skill-manifest.yaml")})
	})
	if !strings.Contains(out, "- source_authority:") {
		t.Fatalf("import-skills output missing source authority marker:\n%s", out)
	}
	captureStdout(t, func() {
		cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "skill-manifest.yaml")})
	})

	records, err := capgraph.LoadSourceAuthority(homePath)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("source authority records = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.AuthorityStatus != "compound_authoritative" || rec.SourceRole != "evidence_only_input" {
		t.Fatalf("wrong authority status/role: %+v", rec)
	}
	if rec.Retires != "source_view_authority:fixtures/skills/skill-manifest.yaml" {
		t.Fatalf("retired source authority = %q", rec.Retires)
	}
	if !stringSliceContains(rec.NodeIDs, "skill:pricing") || rec.SourceObservationID == "" || len(rec.NoPromoteIDs) != 1 {
		t.Fatalf("authority marker missing proof fields: %+v", rec)
	}

	authorityOut := captureStdout(t, func() { cmdCapability([]string{"authority"}) })
	if !strings.Contains(authorityOut, rec.ID) || !strings.Contains(authorityOut, "compound_authoritative") {
		t.Fatalf("capability authority output missing marker:\n%s", authorityOut)
	}
	if strings.Count(authorityOut, rec.ID) != 1 {
		t.Fatalf("capability authority output should collapse replayed imports:\n%s", authorityOut)
	}
	authorityJSON := captureStdout(t, func() { cmdCapability([]string{"authority", "--json"}) })
	var authorityReport struct {
		SchemaVersion               string                           `json:"schema_version"`
		AuthorityRef                string                           `json:"authority_ref"`
		SourceRoleCounts            map[string]int                   `json:"source_role_counts"`
		RetiredSourceAuthorityCount int                              `json:"retired_source_authority_count"`
		RetiredSourceAuthorityRefs  []string                         `json:"retired_source_authority_refs"`
		Records                     []capgraph.SourceAuthorityRecord `json:"records"`
	}
	if err := json.Unmarshal([]byte(authorityJSON), &authorityReport); err != nil {
		t.Fatalf("parse authority json: %v\n%s", err, authorityJSON)
	}
	if authorityReport.SchemaVersion != capgraph.SourceAuthoritySchemaVersion ||
		authorityReport.AuthorityRef != "compound://registries/capability-graph" ||
		authorityReport.SourceRoleCounts["evidence_only_input"] != 1 ||
		authorityReport.RetiredSourceAuthorityCount != 1 ||
		!stringSliceContains(authorityReport.RetiredSourceAuthorityRefs, "source_view_authority:fixtures/skills/skill-manifest.yaml") ||
		len(authorityReport.Records) != 1 ||
		authorityReport.Records[0].ID != rec.ID ||
		authorityReport.Records[0].SourceRole != "evidence_only_input" {
		t.Fatalf("unexpected authority json report: %+v", authorityReport)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("capability source authority must not promote claims, stat err=%v", err)
	}
}

func TestCapabilityImportPluginBoardRecordsSourceAuthority(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{"import-skills", "--os-plugin-board-json", filepath.Join("..", "..", "fixtures", "skills", "os-plugin-board.json")})
	})
	if !strings.Contains(out, "- source_authority:") || !strings.Contains(out, "- node: adapter:plugin:browser-openai-bundled") {
		t.Fatalf("plugin-board import output missing authority/node marker:\n%s", out)
	}
	records, err := capgraph.LoadSourceAuthority(homePath)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("source authority records = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.SourceClass != "os-plugin-serving-board-json" || rec.SourceObservationID == "" || len(rec.NoPromoteIDs) != 1 {
		t.Fatalf("plugin authority marker missing proof fields: %+v", rec)
	}
	if !stringSliceContains(rec.NodeIDs, "adapter:plugin:browser-openai-bundled") {
		t.Fatalf("plugin authority missing adapter node: %+v", rec.NodeIDs)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("plugin-board import must not promote claims, stat err=%v", err)
	}
}

func TestCapabilityImportTopologyRecordsSourceObservationNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{"import-topology", filepath.Join("..", "..", "fixtures", "tooltopology", "browser-cli-vs-mcp.yaml")})
	})
	for _, want := range []string{"# tool topology topo_fixture_browser", "- node: tool:browser-cli", "- edge: fallback_for:mcp:browser->tool:browser-cli", "- source_observation:", "- source_candidate:", "- source_no_promote:", "- source_authority:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-topology output missing %q:\n%s", want, out)
		}
	}
	edges := readCapabilityEdges(t, homePath)
	if !hasCapabilityEdge(edges, capgraph.EdFallbackFor, "mcp:browser") {
		t.Fatalf("fallback edge not imported: %+v", edges)
	}
	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read capability observations: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("capability observation entries = %d, want 1", len(entries))
	}
	noPromoteRows := readNoPromoteDecisionRows(t, homePath)
	if len(noPromoteRows) != 1 || noPromoteRows[0].OwnerTarget.Kind != "source_observation" {
		t.Fatalf("no-promote rows = %+v", noPromoteRows)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("topology import should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestCapabilityImportMCPorterCatalogRecordsSourceObservationNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdCapability([]string{"import-topology", "--mcporter-json", filepath.Join("..", "..", "fixtures", "tooltopology", "mcporter-list.json")})
	})
	for _, want := range []string{
		"# tool topology topo_mcporter_",
		"- node: mcp:context7",
		"- node: tool:context7.get-library-docs",
		"- node: mcp:cloudflare-api",
		"- source_observation:",
		"- source_candidate:",
		"- source_no_promote:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcporter import output missing %q:\n%s", want, out)
		}
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Surface("current library docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Node.ID != "tool:context7.get-library-docs" {
		t.Fatalf("mcporter docs tool did not surface: %+v", hits)
	}
	authHits, err := store.Surface("cloudflare api")
	if err != nil {
		t.Fatal(err)
	}
	if len(authHits) != 0 {
		t.Fatalf("auth-blocked mcporter server should not surface: %+v", authHits)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("mcporter source import should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestCapabilityImportSkillsRecordsSourceObservationNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "skill-manifest.yaml")})
	})
	for _, want := range []string{"# capability import-skills skill_manifest_fixture", "- node: skill:pricing", "- edge: requires:skill:pricing->tool:analytics", "- source_observation:", "- source_candidate:", "- source_no_promote:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-skills output missing %q:\n%s", want, out)
		}
	}
	edges := readCapabilityEdges(t, homePath)
	if !hasCapabilityEdge(edges, capgraph.EdRequires, "skill:pricing") {
		t.Fatalf("requires edge not imported: %+v", edges)
	}
	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read capability observations: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("capability observation entries = %d, want 1", len(entries))
	}
	noPromoteRows := readNoPromoteDecisionRows(t, homePath)
	if len(noPromoteRows) != 1 || noPromoteRows[0].OwnerTarget.Kind != "source_observation" {
		t.Fatalf("no-promote rows = %+v", noPromoteRows)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("skill import should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestCapabilitySurfaceJSONFiltersByHarness(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	captureStdout(t, func() {
		cmdCapability([]string{"import-skills", filepath.Join("..", "..", "fixtures", "skills", "harness-split.yaml")})
	})

	out := captureStdout(t, func() {
		cmdCapability([]string{"surface", "--harness", "codex", "--json", "pricing"})
	})
	var report capgraph.SurfaceReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("surface JSON did not parse: %v\n%s", err, out)
	}
	if report.SchemaVersion != capgraph.SurfaceSchemaVersion ||
		report.AuthorityRef != capgraph.CapabilityGraphAuthorityRef ||
		report.Status != "ok" ||
		report.Task != "pricing" ||
		report.Harness != "codex" ||
		report.NodeCount == 0 ||
		report.OfferCount != len(report.Offers) ||
		len(report.OfferSummaries) != len(report.Offers) {
		t.Fatalf("unexpected surface metadata: %+v", report)
	}
	var ids []string
	for _, offer := range report.Offers {
		ids = append(ids, offer.Node.ID)
	}
	if !stringSliceContains(ids, "skill:codex-pricing") || !stringSliceContains(ids, "skill:generic-pricing") {
		t.Fatalf("codex/generic offers missing: %+v", ids)
	}
	if stringSliceContains(ids, "skill:claude-pricing") {
		t.Fatalf("claude-only offer leaked into codex surface: %+v", ids)
	}
	var summaryIDs []string
	for _, offer := range report.OfferSummaries {
		summaryIDs = append(summaryIDs, offer.ID)
		if offer.ID == "skill:codex-pricing" && (offer.Kind != "skill" || offer.Name == "" || offer.CostRiskNote == "") {
			t.Fatalf("codex summary missing flat fields: %+v", offer)
		}
	}
	if !stringSliceContains(summaryIDs, "skill:codex-pricing") || stringSliceContains(summaryIDs, "skill:claude-pricing") {
		t.Fatalf("summary ids = %+v", summaryIDs)
	}
	if !hasMatchedTokenCount(report.MatchedTokenCounts, "pricing", 2) {
		t.Fatalf("matched token counts = %+v, want pricing=2", report.MatchedTokenCounts)
	}
}

func TestCapabilityImportEvaluationsRecordsGraphEvidenceAndSourceNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{"add", "skill:web-design-guidelines", "skill", "Web Design Guidelines", "--triggers", "web,design"})
		cmdCapability([]string{"import-evaluations", filepath.Join("..", "..", "fixtures", "capability-evaluations", "web-design-guidelines.json")})
	})
	for _, want := range []string{"# capability import-evaluations", "- eval_node: eval:capability-evaluation-external-vercel-web-design-guidelines", "- edge: evaluated_by:skill:web-design-guidelines->eval:capability-evaluation-external-vercel-web-design-guidelines", "- edge: validated_by:skill:web-design-guidelines->eval:capability-evaluation-external-vercel-web-design-guidelines", "- source_observation:", "- source_candidate:", "- source_no_promote:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-evaluations output missing %q:\n%s", want, out)
		}
	}
	edges := readCapabilityEdges(t, homePath)
	if !hasCapabilityEdge(edges, capgraph.EdEvaluatedBy, "skill:web-design-guidelines") || !hasCapabilityEdge(edges, capgraph.EdValidatedBy, "skill:web-design-guidelines") {
		t.Fatalf("expected evaluation edges, got %+v", edges)
	}
	surfaceOut := captureStdout(t, func() {
		cmdCapability([]string{"surface", "web design guidelines"})
	})
	if !strings.Contains(surfaceOut, "skill:web-design-guidelines") {
		t.Fatalf("surface output missing skill offer:\n%s", surfaceOut)
	}
	if strings.Contains(surfaceOut, "\n  eval:capability-evaluation") {
		t.Fatalf("evaluation evidence node must not surface as a capability offer row:\n%s", surfaceOut)
	}
	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read capability observations: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("capability observation entries = %d, want 1", len(entries))
	}
	noPromoteRows := readNoPromoteDecisionRows(t, homePath)
	if len(noPromoteRows) != 1 || noPromoteRows[0].OwnerTarget.Kind != "source_observation" {
		t.Fatalf("no-promote rows = %+v", noPromoteRows)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("evaluation import should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestCapabilityImportEvaluationsSurfacesUnknownReviewDebt(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{"import-evaluations", filepath.Join("..", "..", "fixtures", "capability-evaluations", "web-design-guidelines.json")})
	})
	for _, want := range []string{
		"- status: review",
		"- evaluations_read: 1",
		"- applied_evaluations: 0",
		"- skipped_unknown_count: 1",
		"- skipped_unknown: web-design-guidelines",
		"- source_no_promote:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-evaluations output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("unknown evaluation import must not promote claims, stat err=%v", err)
	}
}

func TestCapabilityImportSurfacingMissesRecordsFeedbackAndSourceNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	cmdCapability([]string{"add", "skill:web-design-guidelines", "skill", "Web Design Guidelines", "--triggers", "css"})
	beforeOut := captureStdout(t, func() {
		cmdCapability([]string{"surface", "app store design review"})
	})
	if strings.Contains(beforeOut, "skill:web-design-guidelines") {
		t.Fatalf("precondition: skill surfaced before report feedback:\n%s", beforeOut)
	}
	out := captureStdout(t, func() {
		cmdCapability([]string{"import-surfacing-misses", filepath.Join("..", "..", "fixtures", "capability-surfacing", "skill-surfacing-proposals.json")})
	})
	for _, want := range []string{
		"# capability import-surfacing-misses",
		"- status: review",
		"- misses_read: 2",
		"- false_triggers_read: 0",
		"- applied_misses: 1",
		"- applied_false_triggers: 0",
		"- edge: missed_when:context:app->skill:web-design-guidelines",
		"- skipped_unknown_count: 1",
		"- skipped_unknown: production-audit",
		"- source_observation:",
		"- source_candidate:",
		"- source_no_promote:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-surfacing-misses output missing %q:\n%s", want, out)
		}
	}
	afterOut := captureStdout(t, func() {
		cmdCapability([]string{"surface", "app store design review"})
	})
	if !strings.Contains(afterOut, "skill:web-design-guidelines") {
		t.Fatalf("surfacing miss feedback should surface skill:\n%s", afterOut)
	}
	authorityJSON := captureStdout(t, func() {
		cmdCapability([]string{"authority", "--json"})
	})
	var authority struct {
		NodeCount         int            `json:"node_count"`
		EdgeCount         int            `json:"edge_count"`
		EdgeKindCounts    map[string]int `json:"edge_kind_counts"`
		SourceRecordCount int            `json:"source_record_count"`
		SourceEdgeCount   int            `json:"source_edge_count"`
	}
	if err := json.Unmarshal([]byte(authorityJSON), &authority); err != nil {
		t.Fatalf("capability authority --json should parse: %v\n%s", err, authorityJSON)
	}
	if authority.EdgeKindCounts[capgraph.EdMissedWhen] == 0 {
		t.Fatalf("authority should expose feedback edge kind counts: %+v", authority.EdgeKindCounts)
	}
	if authority.NodeCount != 1 || authority.EdgeCount == 0 || authority.SourceRecordCount != 0 || authority.SourceEdgeCount != 0 {
		t.Fatalf("authority should expose current graph feedback separately from source handoff totals: %+v", authority)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("surfacing miss import must not promote claims, stat err=%v", err)
	}
}

// A2 (packet 2026-07-15-slice2A-skill-surfacing-verb) at the CLI layer: the
// open skill-surfacing review card carries the exact import action (queue
// projection), running the verb on that card's own path applies edges AND
// clears the card, and a second run is a clean idempotent no-op.
func TestCapabilityImportSurfacingMissesResolvesReviewQueueItemAndStaysIdempotent(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	cmdCapability([]string{"add", "skill:web-design-guidelines", "skill", "Web Design Guidelines", "--triggers", "css"})

	reviewDir := filepath.Join(homePath, "reviews", "skill-surfacing")
	if err := os.MkdirAll(reviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reviewPath := filepath.Join(reviewDir, "session-mining-backfill.json")
	if err := os.WriteFile(reviewPath, []byte(`{
  "id": "ssfb_test_cli01",
  "generated": "2026-07-09T18:39:12Z",
  "all_misses": [
    {"skill": "web-design-guidelines", "count": 1, "receipts": ["packet-test-cli"], "reasons": ["app store design review"], "maxConf": 0.9}
  ],
  "false_triggers": [],
  "perReceipt": [
    {"dir": "packet-test-cli", "kind": "human", "lanes": "deterministic-backfill",
      "real": [{"skill": "web-design-guidelines", "confidence": 0.9, "reason": "app store design review"}],
      "false_triggered": []}
  ]
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	queueBefore, err := buildReviewQueueReport(homePath, "skill-surfacing", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(queueBefore.Items) != 1 || len(queueBefore.Items[0].Actions) != 1 || queueBefore.Items[0].Actions[0].ID != "skill-surfacing.import" {
		t.Fatalf("queue before import = %+v", queueBefore.Items)
	}
	wantArgv := []string{"compound", "capability", "import-surfacing-misses", reviewPath}
	if strings.Join(queueBefore.Items[0].Actions[0].Argv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("action argv = %+v, want %+v", queueBefore.Items[0].Actions[0].Argv, wantArgv)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{"import-surfacing-misses", reviewPath})
	})
	for _, want := range []string{
		"- applied_misses: 1",
		"- applied_edges: 4",
		"- review_resolved: true",
		"- review_receipt: rec_",
		"- review_undo: compound reviews resolve",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-surfacing-misses output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(strings.TrimSuffix(reviewPath, ".json") + ".reviewed.json"); err != nil {
		t.Fatalf("resolution sidecar missing: %v", err)
	}

	queueAfter, err := buildReviewQueueReport(homePath, "skill-surfacing", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(queueAfter.Items) != 1 || queueAfter.Items[0].Band != reviewartifact.BandFiled || len(queueAfter.Items[0].Actions) != 0 {
		t.Fatalf("queue after import should be filed with no action: %+v", queueAfter.Items[0])
	}

	replayOut := captureStdout(t, func() {
		cmdCapability([]string{"import-surfacing-misses", reviewPath})
	})
	if !strings.Contains(replayOut, "- applied_edges: 0") || !strings.Contains(replayOut, "- review_resolved: false") {
		t.Fatalf("replay import should be a clean idempotent no-op, got:\n%s", replayOut)
	}
}

func TestCapabilitySkillSourceDoctorReportsRawSkillSource(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	root := t.TempDir()
	skillDir := filepath.Join(root, "skills", "pricing")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: pricing
description: "Pricing workflow"
---
# pricing
`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{"skill-source-doctor", root, "--json"})
	})
	for _, want := range []string{`"schema_version": "compound.skill_source_doctor.v0.1"`, `"status": "ok"`, `"skill_file_count": 1`, `"path": "skills/pricing/SKILL.md"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill-source-doctor output missing %q:\n%s", want, out)
		}
	}
}

func TestCapabilityEvaluationPlanUsesUnknownEvaluationWithoutMutatingGraph(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	targetPath := filepath.Join(t.TempDir(), "skill-manifest.yaml")
	if err := os.WriteFile(targetPath, []byte("existing: skill manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdCapability([]string{
			"evaluation-plan",
			filepath.Join("..", "..", "fixtures", "capability-evaluations", "web-design-guidelines.json"),
			"--capability", "web-design-guidelines",
			"--target", targetPath,
		})
	})
	for _, want := range []string{
		"# capability evaluation-plan",
		"- status: review_required",
		"- evaluation_set:",
		"- evaluation: capability-evaluation.external.vercel.web-design-guidelines",
		"- capability: web-design-guidelines",
		"- verdict: adapt",
		"- node_draft: skill:web-design-guidelines",
		"- node_kind: skill",
		"- required_evidence: 5",
		"- evidence_refs:",
		"- plan:",
		"- review:",
		"- review_yaml:",
		"- manifest_draft:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("evaluation-plan output missing %q:\n%s", want, out)
		}
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	reviewYAMLPath := outputValue(out, "- review_yaml:")
	manifestDraftPath := outputValue(out, "- manifest_draft:")
	var plan capgraph.CapabilityEvaluationPlan
	if err := json.Unmarshal([]byte(readFile(t, planPath)), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.NodeDraft.NodeID != "skill:web-design-guidelines" || plan.TargetHash == "" {
		t.Fatalf("plan = %+v", plan)
	}
	manifest, err := capgraph.LoadSkillManifest(manifestDraftPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Skills) != 1 || manifest.Skills[0].Status != "needs_review" || manifest.Skills[0].ID != "skill:web-design-guidelines" {
		t.Fatalf("manifest draft = %+v", manifest)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not mutate CapabilityGraph") || !strings.Contains(review, "Required Review") {
		t.Fatalf("review missing guardrails:\n%s", review)
	}
	if got := readFile(t, targetPath); got != "existing: skill manifest\n" {
		t.Fatalf("target mutated: %q", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("evaluation-plan must not mutate capability graph, stat err=%v", err)
	}
	if _, err := capgraph.LoadCapabilityEvaluationReview(reviewYAMLPath); err == nil {
		t.Fatal("unedited evaluation review YAML should be rejected")
	}
	reviewYAML := readFile(t, reviewYAMLPath)
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_activate_reject_or_keep_needs_review", "activate")
	reviewYAML = strings.ReplaceAll(reviewYAML, "rationale: \"\"", "rationale: \"reviewed trigger scope and validation evidence\"")
	reviewYAML = strings.ReplaceAll(reviewYAML, "confidence: -1", "confidence: 0.84")
	editedReviewYAMLPath := filepath.Join(t.TempDir(), "capability-evaluation-review.yaml")
	if err := os.WriteFile(editedReviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewOut := captureStdout(t, func() {
		cmdCapability([]string{"evaluation-review", editedReviewYAMLPath})
	})
	for _, want := range []string{
		"# capability evaluation-review",
		"- status: manifest_ready",
		"- decision: activate",
		"- node: skill:web-design-guidelines",
		"- node_status: active",
		"- manifest:",
	} {
		if !strings.Contains(reviewOut, want) {
			t.Fatalf("evaluation-review output missing %q:\n%s", want, reviewOut)
		}
	}
	activeManifestPath := outputValue(reviewOut, "- manifest:")
	if _, err := os.Stat(filepath.Join(homePath, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("evaluation-review must not mutate capability graph, stat err=%v", err)
	}
	importOut := captureStdout(t, func() {
		cmdCapability([]string{"import-skills", manifestDraftPath})
	})
	if !strings.Contains(importOut, "node: skill:web-design-guidelines") {
		t.Fatalf("manifest draft import output =\n%s", importOut)
	}
	surfaceOut := captureStdout(t, func() {
		cmdCapability([]string{"surface", "web design guidelines"})
	})
	if strings.Contains(surfaceOut, "skill:web-design-guidelines") {
		t.Fatalf("needs_review manifest draft must not surface:\n%s", surfaceOut)
	}
	activeManifest, err := capgraph.LoadSkillManifest(activeManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.ApplySkillManifest(homePath, activeManifest, cliTestNow); err == nil || !strings.Contains(err.Error(), "resurrect") {
		t.Fatalf("active manifest import must be refused by retired-state guard: %v", err)
	}
	activeSurfaceOut := captureStdout(t, func() {
		cmdCapability([]string{"surface", "web design guidelines"})
	})
	if strings.Contains(activeSurfaceOut, "skill:web-design-guidelines") {
		t.Fatalf("retired node must not surface after blocked activation attempt:\n%s", activeSurfaceOut)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("evaluation-plan must not promote claims, stat err=%v", err)
	}
}

func TestEventImportOSWritesTraceEnvelopeWithoutPromotion(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")

	out := captureStdout(t, func() {
		cmdEvent([]string{"import-os", fixture, "--stream", "control-events"})
	})
	if !strings.Contains(out, "- authority: compound://events/trace-envelopes") ||
		!strings.Contains(out, "- source_authority: evsrc_") ||
		!strings.Contains(out, "imported: 2") ||
		!strings.Contains(out, "skipped: 0") {
		t.Fatalf("event import output =\n%s", out)
	}
	authority, err := eventsource.LoadSourceAuthority(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(authority) != 1 ||
		authority[0].AuthorityRef != eventsource.TraceEventsAuthorityRef ||
		authority[0].AuthorityStatus != "compound_primary" {
		t.Fatalf("event source authority = %+v", authority)
	}
	authorityOut := captureStdout(t, func() {
		cmdEvent([]string{"authority", "--json"})
	})
	var authorityReport eventsource.AuthorityReport
	if err := json.Unmarshal([]byte(authorityOut), &authorityReport); err != nil {
		t.Fatalf("parse event authority: %v\n%s", err, authorityOut)
	}
	if authorityReport.AuthorityRef != eventsource.TraceEventsAuthorityRef || !authorityReport.Installed || authorityReport.TraceCount != 2 {
		t.Fatalf("event authority report = %+v", authorityReport)
	}
	if len(authorityReport.SourceAuthority) != 1 || authorityReport.SourceAuthority[0].ID == "" {
		t.Fatalf("event authority source rows = %+v", authorityReport.SourceAuthority)
	}
	if len(authorityReport.RecentTraces) != 2 || authorityReport.RecentTraces[0].ID == "" {
		t.Fatalf("event authority recent traces = %+v", authorityReport.RecentTraces)
	}
	tracePath := filepath.Join(homePath, "events", "traces.ndjson")
	lines := splitJSONLines(readFile(t, tracePath))
	if len(lines) != 2 {
		t.Fatalf("trace lines = %d, want 2", len(lines))
	}
	var tr envelope.TraceEnvelope
	if err := json.Unmarshal([]byte(lines[0]), &tr); err != nil {
		t.Fatalf("parse trace: %v", err)
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("trace invalid: %v", err)
	}
	if tr.Kind() != "trace_envelope" || tr.Source.System != "os-event" || tr.MutationClass != "read" {
		t.Fatalf("trace = %+v", tr)
	}

	replay := captureStdout(t, func() {
		cmdEvent([]string{"import-os", fixture, "--stream", "control-events"})
	})
	if !strings.Contains(replay, "- source_authority: evsrc_") ||
		!strings.Contains(replay, "imported: 0") ||
		!strings.Contains(replay, "skipped: 2") {
		t.Fatalf("event replay output =\n%s", replay)
	}
	if lines := splitJSONLines(readFile(t, tracePath)); len(lines) != 2 {
		t.Fatalf("replay should not append duplicate traces, lines=%d", len(lines))
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("event import must not write claims ledger, stat err=%v", err)
	}
}

func TestEventShowAndListServeFromCompoundHome(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")

	captureStdout(t, func() {
		cmdEvent([]string{"import-os", fixture, "--stream", "os-control-events"})
	})
	authorityOut := captureStdout(t, func() {
		cmdEvent([]string{"authority", "--json"})
	})
	var authorityReport eventsource.AuthorityReport
	if err := json.Unmarshal([]byte(authorityOut), &authorityReport); err != nil {
		t.Fatalf("parse authority: %v\n%s", err, authorityOut)
	}
	if len(authorityReport.RecentTraces) == 0 {
		t.Fatalf("no traces to show")
	}
	traceID := authorityReport.RecentTraces[0].ID

	// event show <id> --json returns the envelope with its source ref.
	showOut := captureStdout(t, func() {
		cmdEvent([]string{"show", traceID, "--json"})
	})
	var detail eventsource.TraceDetail
	if err := json.Unmarshal([]byte(showOut), &detail); err != nil {
		t.Fatalf("parse show: %v\n%s", err, showOut)
	}
	if detail.TraceID != traceID || detail.Status != "ok" {
		t.Fatalf("show detail = %+v", detail)
	}
	if detail.Source.Stream != "os-control-events" || detail.Source.Line < 1 {
		t.Fatalf("show must carry source ref, got %+v", detail.Source)
	}
	if !strings.Contains(detail.SourceRef, fixture) {
		t.Fatalf("source_ref must drill back to source stream, got %q", detail.SourceRef)
	}
	if err := detail.Trace.Validate(); err != nil {
		t.Fatalf("served trace invalid: %v", err)
	}

	// event list --json serves summaries scoped to the stream.
	listOut := captureStdout(t, func() {
		cmdEvent([]string{"list", "--stream", "os-control-events", "--json"})
	})
	var listReport eventsource.TraceListReport
	if err := json.Unmarshal([]byte(listOut), &listReport); err != nil {
		t.Fatalf("parse list: %v\n%s", err, listOut)
	}
	if listReport.Count != 2 || len(listReport.Traces) != 2 {
		t.Fatalf("list report = %+v", listReport)
	}
	for _, tr := range listReport.Traces {
		if tr.Stream != "os-control-events" {
			t.Fatalf("list stream filter leaked %q", tr.Stream)
		}
	}
}

func TestEventImportOSCanProduceTraceBackedReviewFinding(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	grantSource := filepath.Join(t.TempDir(), "standing-grants.yaml")
	if err := os.WriteFile(grantSource, []byte(`generated_at: "2026-07-01T12:00:00Z"
authority:
  decision_ref: "grant:test-review-finding"
  reviewed_by: "operator:test"
grants:
  - grant_id: grant_review_finding
    capabilities:
      - review.finding
      - review.summary
    target_scope: state/runs/*
    reversibility_ceiling: reversible
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(homePath, grantSource, cliTestNow); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
	runRoot := t.TempDir()
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	out := captureStdout(t, func() {
		cmdEvent([]string{
			"import-os", fixture,
			"--stream", "control-events",
			"--limit", "1",
			"--review-root", runRoot,
			"--review-summary", "event import producer review finding",
		})
	})
	if !strings.Contains(out, "imported: 1") || !strings.Contains(out, "- review_findings: 1") {
		t.Fatalf("event review import output =\n%s", out)
	}
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("missing review finding JSON:\n%s", out)
	}
	var report reviewFindingRecord
	if err := json.Unmarshal([]byte(out[start:]), &report); err != nil {
		t.Fatalf("parse review finding report: %v\n%s", err, out)
	}
	if report.PolicyOutcome != "allow_with_obligations" || report.StandingGrantID == "" || len(report.SourceTraces) != 1 {
		t.Fatalf("review finding report = %+v", report)
	}
	if _, err := os.Stat(filepath.Join(runRoot, filepath.FromSlash(report.TargetPath))); err != nil {
		t.Fatalf("review finding artifact missing: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	got, events, err := store.Show(report.WorkID)
	if err != nil {
		t.Fatalf("show work: %v", err)
	}
	receiptSeen := false
	for _, ev := range events {
		for _, ref := range ev.Attached {
			if strings.HasPrefix(ref, "rec_") {
				receiptSeen = true
			}
		}
	}
	if got.State != "completed" || got.IDs.TransitionID == "" || !receiptSeen || !stringSliceContains(got.ActionIntentIDs, report.ActionIntentID) {
		t.Fatalf("auto review work not closed: work=%+v report=%+v", got, report)
	}
}

func TestDistillerRequestLoadsImportedEventTraces(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	captureStdout(t, func() {
		cmdEvent([]string{"import-os", fixture, "--stream", "control-events"})
	})

	var traces []envelope.TraceEnvelope
	for _, line := range splitJSONLines(readFile(t, filepath.Join(homePath, "events", "traces.ndjson"))) {
		var tr envelope.TraceEnvelope
		if err := json.Unmarshal([]byte(line), &tr); err != nil {
			t.Fatalf("parse trace line: %v", err)
		}
		traces = append(traces, tr)
	}
	if len(traces) != 2 {
		t.Fatalf("traces = %d, want 2", len(traces))
	}

	out := captureStdout(t, func() {
		cmdDistiller([]string{
			"request",
			"--trace-ref", "event-batch:control-events",
			"--event-trace", traces[0].ID,
			"--event-trace", traces[1].ID,
		})
	})
	var req distiller.LLMExtractionRequest
	if err := json.Unmarshal([]byte(out), &req); err != nil {
		t.Fatalf("parse distiller request: %v\n%s", err, out)
	}
	if req.TraceRef != "event-batch:control-events" || req.RawTrace != "" {
		t.Fatalf("request trace/raw = %q/%q", req.TraceRef, req.RawTrace)
	}
	traceEnvelopeCount := 0
	for _, env := range req.Envelopes {
		if env.Kind == "trace_envelope" {
			traceEnvelopeCount++
		}
	}
	if traceEnvelopeCount != 2 {
		t.Fatalf("trace envelope count = %d, envelopes=%+v", traceEnvelopeCount, req.Envelopes)
	}
	for _, tr := range traces {
		if !stringSliceContains(req.SourceRefs, "trace_envelope:"+tr.ID) {
			t.Fatalf("request source refs missing trace %s: %v", tr.ID, req.SourceRefs)
		}
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("distiller request over event traces must not write claims ledger, stat err=%v", err)
	}
}

func TestDistillerImportConversationMiningRecordsSourceNoPromote(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "agent-conversation-mining-report-latest.json")
	tagsPath := filepath.Join(dir, "agent-conversation-mined-tags-latest.jsonl")
	lossinessPath := filepath.Join(dir, "agent-conversation-lossiness-latest.json")
	if err := os.WriteFile(tagsPath, []byte(`{"accept_for_dream":true,"importance":10,"knowledge_type":"operational","quality_score":10,"tags":["architecture","distillation"],"decision_reasons":["accepted"],"summary":"high-signal system discussion","model_provider":"deterministic","model_name":"rules-v1","candidate":{"source":"codex","raw_path":"raw/session.jsonl","clean_path":"clean/session.md","clean_status":"mapped","stable":true,"importance":19,"quality_score":10,"knowledge_type":"operational","dream_candidate":true,"reasons":["architecture"],"retained_ratio":0.02,"excerpt":"<<ref>>"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lossinessPath, []byte(`{"known_lossy_rules":{"drops":"tool results"},"claude_clean_vs_raw_size_ratio":0.044,"codex_clean_vs_raw_size_ratio":0.012}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, []byte(`{"ok":true,"updated_at":"2026-06-20T17:19:19Z","queue":"queue.jsonl","processed":1,"accepted":1,"rejected":0,"providers":{"deterministic":1},"out":"`+tagsPath+`","report":"`+reportPath+`","dream_pack":"dream-pack","dream_pack_files":1,"note":"Prepared high-fidelity Dream pack only. Dream execution remains separate gated."}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdDistiller([]string{"import-conversation-mining", reportPath, "--lossiness", lossinessPath})
	})
	for _, want := range []string{
		"# distiller import-conversation-mining",
		"- status: ok",
		"- mined_rows: 1",
		"- accepted_for_review: 1",
		"- dream_execution: gated",
		"- lossiness_recorded: true",
		"- top_tag: architecture",
		"- source_observation:",
		"- source_candidate:",
		"- review_command: compound distiller improvement-plan --candidate",
		"- source_no_promote:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-conversation-mining output missing %q:\n%s", want, out)
		}
	}
	sourceCandidateID := outputValue(out, "- source_candidate:")
	if sourceCandidateID == "" {
		t.Fatalf("source candidate missing:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("conversation mining import must not write claims ledger, stat err=%v", err)
	}
	methodDir := filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade))
	files, err := os.ReadDir(methodDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("method upgrade candidates = %d, want 1", len(files))
	}
	planOut := captureStdout(t, func() {
		cmdDistiller([]string{"improvement-plan", "--candidate", sourceCandidateID})
	})
	if !strings.Contains(planOut, "# distiller improvement-plan") || !strings.Contains(planOut, "- candidate_kind: method_upgrade") {
		t.Fatalf("conversation mining source candidate did not enter improvement-plan path:\n%s", planOut)
	}
	planPath := outputValue(planOut, "- plan:")
	var plan distiller.ImprovementPlan
	if err := json.Unmarshal([]byte(readFile(t, planPath)), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.CandidateID != sourceCandidateID || plan.CandidateVerdict != candidates.NoPromote || plan.Draft.Area != "method" {
		t.Fatalf("improvement plan = %+v", plan)
	}
	reviewYAMLPath := outputValue(planOut, "- review_yaml:")
	if reviewYAMLPath == "" {
		t.Fatalf("improvement plan did not print review_yaml:\n%s", planOut)
	}
	var review distiller.ImprovementReview
	if err := yaml.Unmarshal([]byte(readFile(t, reviewYAMLPath)), &review); err != nil {
		t.Fatal(err)
	}
	review.Decision = distiller.ImprovementReviewDecisionAccept
	review.Rationale = "reviewed conversation-mining import replay; keep source evidence candidate-only while improving future extraction review."
	review.Confidence = 0.77
	evalOut := captureStdout(t, func() {
		cmdDistiller([]string{"eval", filepath.Join("..", "..", "fixtures", "distiller", "llm-offline-eval.json"), "--record"})
	})
	proofRef := outputValue(evalOut, "- proof_ref:")
	if proofRef == "" {
		t.Fatalf("distiller eval --record did not print proof_ref:\n%s", evalOut)
	}
	review.EvalProofRefs = []string{proofRef}
	reviewData, err := yaml.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	editedReviewPath := filepath.Join(homePath, "reviews", "distiller-improvements", "edited-conversation-mining.review.yaml")
	if err := os.WriteFile(editedReviewPath, reviewData, 0o644); err != nil {
		t.Fatal(err)
	}
	reviewOut := captureStdout(t, func() {
		cmdDistiller([]string{"improvement-review", editedReviewPath})
	})
	if !strings.Contains(reviewOut, "# distiller improvement-review") || !strings.Contains(reviewOut, "- decision: accept") {
		t.Fatalf("conversation mining source candidate did not enter improvement-review path:\n%s", reviewOut)
	}
	reviewedPath := outputValue(reviewOut, "- reviewed:")
	if reviewedPath == "" {
		t.Fatalf("improvement-review did not print reviewed artifact:\n%s", reviewOut)
	}
	var reviewed distiller.ReviewedImprovement
	if err := json.Unmarshal([]byte(readFile(t, reviewedPath)), &reviewed); err != nil {
		t.Fatal(err)
	}
	if reviewed.Status != "accepted" || reviewed.PlanID != plan.ID || reviewed.CandidateID != sourceCandidateID || len(reviewed.EvalProofRefs) != 1 {
		t.Fatalf("reviewed improvement = %+v", reviewed)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("conversation mining improvement-plan/review must not write claims ledger, stat err=%v", err)
	}
}

func TestDistillerImportSessionLensesRecordsSourceNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	dir := t.TempDir()
	lensesPath := filepath.Join(dir, "multi-lens.json")
	if err := os.WriteFile(lensesPath, []byte(`{
  "decisions": [{"summary": "Keep extraction source-only."}],
  "outcomes": [{"summary": "Batch completed."}],
  "patterns": [{"summary": "Recall misses repeat."}, {"summary": "Operator valves hold."}],
  "skills": [{"name": "source bridge"}],
  "taste": [{"summary": "Plain language."}],
  "contradictions": [{"summary": "Extracted evidence is not promoted truth."}]
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdDistiller([]string{"import-session-lenses", lensesPath})
	})
	for _, want := range []string{
		"# distiller import-session-lenses",
		"- decisions: 1",
		"- outcomes: 1",
		"- patterns: 2",
		"- skills: 1",
		"- taste: 1",
		"- contradictions: 1",
		"- total_items: 7",
		"- source_observation:",
		"- source_candidate:",
		"- review_command: compound distiller improvement-plan --candidate",
		"- source_no_promote:",
		"- row_import: no",
		"- memory_promotion: no",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-session-lenses output missing %q:\n%s", want, out)
		}
	}
	sourceCandidateID := outputValue(out, "- source_candidate:")
	if sourceCandidateID == "" {
		t.Fatalf("source candidate missing:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("session lenses import must not write claims ledger, stat err=%v", err)
	}
	nopRows := readNoPromoteDecisionRows(t, homePath)
	if len(nopRows) != 1 || nopRows[0].CandidateRef != sourceCandidateID || nopRows[0].OwnerTarget.Kind != "source_observation" {
		t.Fatalf("no-promote rows = %+v candidate=%s", nopRows, sourceCandidateID)
	}
}

func TestDistillerImportConversationCoverageRecordsSourceNoPromote(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "agent-conversation-coverage-latest.json")
	if err := os.WriteFile(reportPath, []byte(`{
		"ok": true,
		"updated_at": "2026-06-20T15:13:24Z",
		"sources": {
			"claude": {
				"raw_files": 23665,
				"mapped_clean": 21783,
				"unmapped_raw": 1882,
				"stable": 23659,
				"raw_mb": 2514.53,
				"clean_mb": 107.18,
				"avg_retained_ratio": 0.08021,
				"dream_candidates": 69,
				"missing_embeddings": 0
			},
			"codex": {
				"raw_files": 1829,
				"mapped_clean": 1357,
				"unmapped_raw": 472,
				"stable": 1822,
				"raw_mb": 8845.39,
				"clean_mb": 106.17,
				"avg_retained_ratio": 0.09318,
				"dream_candidates": 4,
				"missing_embeddings": 0
			}
		},
		"total_rows": 25494,
		"top_tags": [
			{"source":"claude","raw_basename":"session.jsonl","clean_status":"mapped","importance":19,"quality_score":10,"knowledge_type":"operational","dream_candidate":true,"reasons":["architecture"]}
		]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdDistiller([]string{"import-conversation-coverage", reportPath})
	})
	for _, want := range []string{
		"# distiller import-conversation-coverage conversation_coverage:",
		"- unmapped_raw: 2354",
		"- dream_candidates: 73",
		"- review_debt: 6",
		"- source_observation:",
		"- source_candidate:",
		"- review_command: compound distiller improvement-plan --candidate",
		"- source_no_promote:",
		"- row_import: no",
		"- dream_execution: no",
		"- memory_promotion: no",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("import-conversation-coverage output missing %q:\n%s", want, out)
		}
	}
	sourceCandidateID := outputValue(out, "- source_candidate:")
	if sourceCandidateID == "" {
		t.Fatalf("source candidate missing:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("conversation coverage import should not write claims ledger, stat err=%v", err)
	}
	methodDir := filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade))
	files, err := os.ReadDir(methodDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("method upgrade candidates = %d, want 1", len(files))
	}
	planOut := captureStdout(t, func() {
		cmdDistiller([]string{"improvement-plan", "--candidate", sourceCandidateID})
	})
	if !strings.Contains(planOut, "# distiller improvement-plan") || !strings.Contains(planOut, "- candidate_kind: method_upgrade") {
		t.Fatalf("conversation coverage source candidate did not enter improvement-plan path:\n%s", planOut)
	}
}

func TestDistillerRunLLMEventResponseParksMemoryWithoutPromotion(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	captureStdout(t, func() {
		cmdEvent([]string{"import-os", fixture, "--stream", "control-events"})
	})

	lines := splitJSONLines(readFile(t, filepath.Join(homePath, "events", "traces.ndjson")))
	if len(lines) == 0 {
		t.Fatal("expected imported trace envelopes")
	}
	var trace envelope.TraceEnvelope
	if err := json.Unmarshal([]byte(lines[0]), &trace); err != nil {
		t.Fatalf("parse trace line: %v", err)
	}
	traceRef := "trace_envelope:" + trace.ID
	confidence := 0.88
	response := distiller.LLMExtractionResponse{
		SchemaVersion: distiller.LLMExtractionSchemaVersion,
		TraceRef:      "trace_event_runtime_candidate",
		SourceRefs:    []string{traceRef},
		Candidates: []distiller.LLMCandidate{{
			Kind:         distiller.KindMemory,
			Entity:       "project:" + trace.Scope.ProjectID,
			Predicate:    "event_observed",
			Object:       trace.Source.SourceID,
			Confidence:   &confidence,
			Derivation:   "llm_extracted_from_imported_trace",
			EvidenceRefs: []string{traceRef},
		}},
	}
	responseData, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	responsePath := filepath.Join(t.TempDir(), "event-response.json")
	if err := os.WriteFile(responsePath, append(responseData, '\n'), 0o644); err != nil {
		t.Fatalf("write response: %v", err)
	}

	out := captureStdout(t, func() {
		cmdDistiller([]string{"run", "--llm-response", responsePath})
	})
	if !strings.Contains(out, "# distilled trace_event_runtime_candidate") ||
		!strings.Contains(out, "memory") ||
		!strings.Contains(out, "park") ||
		!strings.Contains(out, "no promotion basis") {
		t.Fatalf("distiller run output missing parked memory proof:\n%s", out)
	}

	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.Memory))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read memory candidate dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("memory candidate entries = %d, want 1", len(entries))
	}
	candidateData, err := os.ReadFile(filepath.Join(candidateDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read memory candidate: %v", err)
	}
	var candidate distiller.Candidate
	if err := json.Unmarshal(candidateData, &candidate); err != nil {
		t.Fatalf("parse memory candidate: %v", err)
	}
	if candidate.Verdict != distiller.Park || !stringSliceContains(candidate.EvidenceRefs, traceRef) {
		t.Fatalf("candidate did not preserve parked trace evidence: %+v", candidate)
	}

	decisionLines := splitJSONLines(readFile(t, filepath.Join(homePath, "ledger", "adjudications.jsonl")))
	if len(decisionLines) != 1 {
		t.Fatalf("adjudication rows = %d, want 1", len(decisionLines))
	}
	var decision distiller.AdjudicationDecision
	if err := json.Unmarshal([]byte(decisionLines[0]), &decision); err != nil {
		t.Fatalf("parse adjudication: %v", err)
	}
	if decision.Kind != distiller.KindMemory || decision.Verdict != distiller.Park || !stringSliceContains(decision.EvidenceRefs, traceRef) {
		t.Fatalf("adjudication did not preserve parked trace evidence: %+v", decision)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("event-derived llm response must not promote claims, stat err=%v", err)
	}
}

func TestDistillerEventRequestSummaryCanDriveParkedMemoryCandidate(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	captureStdout(t, func() {
		cmdEvent([]string{"import-os", fixture, "--stream", "control-events"})
	})
	lines := splitJSONLines(readFile(t, filepath.Join(homePath, "events", "traces.ndjson")))
	if len(lines) == 0 {
		t.Fatal("expected imported trace envelopes")
	}
	var imported envelope.TraceEnvelope
	if err := json.Unmarshal([]byte(lines[0]), &imported); err != nil {
		t.Fatalf("parse imported trace: %v", err)
	}
	requestOut := captureStdout(t, func() {
		cmdDistiller([]string{
			"request",
			"--trace-ref", "event-batch:control-events",
			"--event-trace", imported.ID,
		})
	})
	var req distiller.LLMExtractionRequest
	if err := json.Unmarshal([]byte(requestOut), &req); err != nil {
		t.Fatalf("parse request: %v\n%s", err, requestOut)
	}
	if len(req.Envelopes) != 1 || req.Envelopes[0].Trace == nil {
		t.Fatalf("request missing structured trace envelope: %+v", req.Envelopes)
	}
	eventEnv := req.Envelopes[0]
	if eventEnv.Trace.Source.SourceID == "" ||
		eventEnv.Trace.Scope.ProjectID == "" ||
		!stringSliceContains(eventEnv.Trace.FilesTouched, "/workspace/snapshots/daily.json") {
		t.Fatalf("request trace summary is not useful source material: %+v", eventEnv.Trace)
	}
	traceRef := "trace_envelope:" + eventEnv.ID
	confidence := 0.91
	response := distiller.LLMExtractionResponse{
		SchemaVersion: distiller.LLMExtractionSchemaVersion,
		TraceRef:      "trace_event_request_summary_candidate",
		SourceRefs:    []string{traceRef},
		Candidates: []distiller.LLMCandidate{{
			Kind:         distiller.KindMemory,
			Entity:       "project:" + eventEnv.Trace.Scope.ProjectID,
			Predicate:    "event_observed",
			Object:       eventEnv.Trace.Source.SourceID,
			Confidence:   &confidence,
			Derivation:   "llm_extracted_from_trace_envelope_summary",
			EvidenceRefs: []string{traceRef},
		}},
	}
	responseData, err := json.MarshalIndent(response, "", " ")
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	responsePath := filepath.Join(t.TempDir(), "event-request-summary-response.json")
	if err := os.WriteFile(responsePath, append(responseData, '\n'), 0o644); err != nil {
		t.Fatalf("write response: %v", err)
	}
	out := captureStdout(t, func() {
		cmdDistiller([]string{"run", "--llm-response", responsePath})
	})
	if !strings.Contains(out, "# distilled trace_event_request_summary_candidate") ||
		!strings.Contains(out, "park") ||
		!strings.Contains(out, "no promotion basis") {
		t.Fatalf("distiller run output missing parked proof:\n%s", out)
	}
	candidateRows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.Memory)))
	if len(candidateRows) != 1 ||
		candidateRows[0].Entity != "project:control" ||
		candidateRows[0].Object != eventEnv.Trace.Source.SourceID ||
		candidateRows[0].Verdict != distiller.Park ||
		!stringSliceContains(candidateRows[0].EvidenceRefs, traceRef) {
		t.Fatalf("event request summary candidate not parked with trace evidence: %+v", candidateRows)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("event request summary candidate must not promote claims, stat err=%v", err)
	}
}

func TestDecisionCandidatePlanUsesDistillerCandidateWithoutMutatingTarget(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	confidence := 0.88
	response := distiller.LLMExtractionResponse{
		SchemaVersion: distiller.LLMExtractionSchemaVersion,
		TraceRef:      "trace_decision_candidate",
		SourceRefs:    []string{"trace_envelope:trace_decision_candidate"},
		Candidates: []distiller.LLMCandidate{{
			Kind:       distiller.KindDecision,
			Subject:    "decision:route-free-model-judging",
			Statement:  "Use paid frontier lanes for final judgment only after cheap lanes produce evidence.",
			Detail:     "Keeps routing cost controlled while preserving quality gates.",
			Confidence: &confidence,
			Derivation: "llm_extracted_from_session",
			Qualifiers: map[string]string{
				"kind":           decisions.KindOperational,
				"owner":          "compound",
				"scope":          "routing",
				"falsifier":      "cheap lanes repeatedly miss material defects",
				"review_trigger": "after three route-evidence regressions",
			},
			EvidenceRefs: []string{"trace_envelope:trace_decision_candidate", "receipt:routing"},
		}},
	}
	responseData, err := json.MarshalIndent(response, "", " ")
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	responsePath := filepath.Join(t.TempDir(), "decision-response.json")
	if err := os.WriteFile(responsePath, append(responseData, '\n'), 0o644); err != nil {
		t.Fatalf("write response: %v", err)
	}
	captureStdout(t, func() {
		cmdDistiller([]string{"run", "--llm-response", responsePath})
	})
	rows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.Decision)))
	if len(rows) != 1 || rows[0].Verdict != distiller.Park || rows[0].Kind != distiller.KindDecision {
		t.Fatalf("decision candidates = %+v", rows)
	}
	targetPath := filepath.Join(t.TempDir(), "DEC-routing.md")
	original := "# Existing Decision\n\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdDecisions([]string{"candidate-plan", "--candidate", rows[0].ID, "--target", targetPath})
	})
	if !strings.Contains(out, "# decisions candidate-plan dcp_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- decision_key: route-free-model-judging") ||
		!strings.Contains(out, "- plan:") ||
		!strings.Contains(out, "- review:") {
		t.Fatalf("candidate-plan output missing proof lines:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	if planPath == "" || reviewPath == "" {
		t.Fatalf("missing artifact paths:\n%s", out)
	}
	var plan decisions.DecisionCandidatePlan
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != decisions.DecisionCandidatePlanSchemaVersion ||
		plan.CandidateID != rows[0].ID ||
		plan.LifecycleDraft.EntityID != "decision:route-free-model-judging" ||
		plan.TargetHash == "" {
		t.Fatalf("bad decision candidate plan: %+v", plan)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not mutate decision source files") ||
		!strings.Contains(review, "## Draft DecisionLifecycle") {
		t.Fatalf("review missing no-mutation warning:\n%s", review)
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("decision target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("candidate-plan must not promote claims, stat err=%v", err)
	}
}

func TestMemoryWritePlanUsesParkedDistillerCandidateWithoutMutatingBrain(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	confidence := 0.86
	response := distiller.LLMExtractionResponse{
		SchemaVersion: distiller.LLMExtractionSchemaVersion,
		TraceRef:      "trace_brain_write_candidate",
		SourceRefs:    []string{"trace_envelope:trace_brain_write_candidate"},
		Candidates: []distiller.LLMCandidate{{
			Kind:         distiller.KindMemory,
			Entity:       "project:sample-mobile-app",
			Predicate:    "release_gate",
			Object:       "review App Store screenshots before release",
			Confidence:   &confidence,
			Derivation:   "llm_extracted_from_session",
			ClaimType:    "constraint",
			EvidenceRefs: []string{"trace_envelope:trace_brain_write_candidate"},
		}},
	}
	responseData, err := json.MarshalIndent(response, "", " ")
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	responsePath := filepath.Join(t.TempDir(), "brain-write-response.json")
	if err := os.WriteFile(responsePath, append(responseData, '\n'), 0o644); err != nil {
		t.Fatalf("write response: %v", err)
	}
	captureStdout(t, func() {
		cmdDistiller([]string{"run", "--llm-response", responsePath})
	})
	candidateRows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.Memory)))
	if len(candidateRows) != 1 || candidateRows[0].Verdict != distiller.Park {
		t.Fatalf("parked candidates = %+v", candidateRows)
	}
	targetPath := filepath.Join(t.TempDir(), "brain", "products", "sample-mobile-app.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "# SampleMobileApp\n\nExisting curated product memory.\n"
	if err := os.WriteFile(targetPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdMemory([]string{"write-plan", "--candidate", candidateRows[0].ID, "--target", targetPath})
	})
	if !strings.Contains(out, "# memory write-plan mwp_") ||
		!strings.Contains(out, "- status: review_required") ||
		!strings.Contains(out, "- target_exists: true") ||
		!strings.Contains(out, "- evidence_refs: 1") ||
		!strings.Contains(out, "- plan:") ||
		!strings.Contains(out, "- review:") {
		t.Fatalf("write-plan output missing proof lines:\n%s", out)
	}
	planPath := outputValue(out, "- plan:")
	reviewPath := outputValue(out, "- review:")
	if planPath == "" || reviewPath == "" {
		t.Fatalf("missing artifact paths:\n%s", out)
	}
	var plan mem.MeaningWritePlan
	planData, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != mem.MeaningWritePlanSchemaVersion ||
		plan.CandidateID != candidateRows[0].ID ||
		plan.Entity != "project:sample-mobile-app" ||
		plan.TargetHash == "" ||
		!strings.Contains(plan.ProposedMarkdown, "review App Store screenshots before release") {
		t.Fatalf("bad meaning write plan: %+v", plan)
	}
	review := readFile(t, reviewPath)
	if !strings.Contains(review, "does not mutate curated memory source files") {
		t.Fatalf("review missing no-mutation warning:\n%s", review)
	}
	if got := readFile(t, targetPath); got != original {
		t.Fatalf("brain target mutated:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("write-plan must not promote claims, stat err=%v", err)
	}
}

func TestDistillerRunGroundsLLMResponseWithRequest(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	request := distiller.LLMExtractionRequest{
		SchemaVersion: distiller.LLMExtractionSchemaVersion,
		TraceRef:      "runtime:work_receipt_proof",
		SourceRefs: []string{
			"receipt:rec_receipt_proof",
			"context_injection:hook_receipt_proof.md",
			"proof:hook-readiness",
		},
		Policy: distiller.DefaultPolicy(),
	}
	confidence := 0.91
	response := distiller.LLMExtractionResponse{
		SchemaVersion: distiller.LLMExtractionSchemaVersion,
		TraceRef:      "runtime:work_receipt_proof",
		Extractor:     "fixture-offline",
		Model:         "offline-fixture",
		Candidates: []distiller.LLMCandidate{{
			Kind:         distiller.KindMethodUpgrade,
			Subject:      "distiller.receipt-proof-learning",
			Statement:    "Offline distiller runs should bind response evidence to saved request source refs.",
			Confidence:   &confidence,
			EvidenceRefs: []string{"proof:hook-readiness", "context_injection:hook_receipt_proof.md"},
		}},
	}

	requestPath := filepath.Join(t.TempDir(), "request.json")
	requestData, err := json.MarshalIndent(request, "", " ")
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if err := os.WriteFile(requestPath, append(requestData, '\n'), 0o644); err != nil {
		t.Fatalf("write request: %v", err)
	}
	responsePath := filepath.Join(t.TempDir(), "response.json")
	responseData, err := json.MarshalIndent(response, "", " ")
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if err := os.WriteFile(responsePath, append(responseData, '\n'), 0o644); err != nil {
		t.Fatalf("write response: %v", err)
	}

	out := captureStdout(t, func() {
		cmdDistiller([]string{"run", "--llm-response", responsePath, "--request", requestPath})
	})
	if !strings.Contains(out, "method_upgrade") || !strings.Contains(out, "park") {
		t.Fatalf("distiller output missing parked method upgrade:\n%s", out)
	}

	rows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade)))
	if len(rows) != 1 {
		t.Fatalf("method upgrade candidates = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Verdict != distiller.Park {
		t.Fatalf("candidate verdict = %q, want park", got.Verdict)
	}
	for _, want := range []string{"proof:hook-readiness", "context_injection:hook_receipt_proof.md"} {
		found := false
		for _, ref := range got.EvidenceRefs {
			if ref == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("candidate evidence refs missing %q: %+v", want, got.EvidenceRefs)
		}
	}
}

func TestDistillerRequestRecordPersistsReplayablePacket(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	rawPath := filepath.Join(t.TempDir(), "trace.txt")
	if err := os.WriteFile(rawPath, []byte("hook output refs must remain replayable evidence\n"), 0o644); err != nil {
		t.Fatalf("write raw trace: %v", err)
	}

	out := captureStdout(t, func() {
		cmdDistiller([]string{"request", "--trace-ref", "trace_recorded_request", "--raw", rawPath, "--record"})
	})
	var stdoutReq distiller.LLMExtractionRequest
	if err := json.Unmarshal([]byte(out), &stdoutReq); err != nil {
		t.Fatalf("parse stdout request: %v\n%s", err, out)
	}
	if stdoutReq.TraceRef != "trace_recorded_request" || !stringSliceContains(stdoutReq.SourceRefs, "raw_trace:trace_recorded_request") {
		t.Fatalf("stdout request not grounded in raw trace: %+v", stdoutReq)
	}

	requestDir := filepath.Join(homePath, "reviews", "distiller-requests")
	entries, err := os.ReadDir(requestDir)
	if err != nil {
		t.Fatalf("read request records: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("request record count = %d, want 1", len(entries))
	}
	recordedPath := filepath.Join(requestDir, entries[0].Name())
	recordedRef := distiller.LLMRequestRecordRefPrefix + strings.TrimSuffix(entries[0].Name(), ".json")
	var recordedReq distiller.LLMExtractionRequest
	if err := json.Unmarshal([]byte(readFile(t, recordedPath)), &recordedReq); err != nil {
		t.Fatalf("parse recorded request: %v", err)
	}
	if recordedReq.TraceRef != stdoutReq.TraceRef || !stringSliceContains(recordedReq.SourceRefs, "raw_trace:trace_recorded_request") {
		t.Fatalf("recorded request changed replay material: %+v", recordedReq)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("distiller request --record must not promote claims, stat err=%v", err)
	}

	confidence := 0.9
	response := distiller.LLMExtractionResponse{
		SchemaVersion: distiller.LLMExtractionSchemaVersion,
		TraceRef:      "trace_recorded_request",
		Extractor:     "fixture-offline",
		Model:         "offline-fixture",
		Candidates: []distiller.LLMCandidate{{
			Kind:         distiller.KindMethodUpgrade,
			Subject:      "distiller.recorded-request-replay",
			Statement:    "Recorded distiller request packets should remain reusable for grounded offline replay.",
			Confidence:   &confidence,
			EvidenceRefs: []string{"raw_trace:trace_recorded_request"},
		}},
	}
	responsePath := filepath.Join(t.TempDir(), "response.json")
	responseData, err := json.MarshalIndent(response, "", " ")
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if err := os.WriteFile(responsePath, append(responseData, '\n'), 0o644); err != nil {
		t.Fatalf("write response: %v", err)
	}

	runOut := captureStdout(t, func() {
		cmdDistiller([]string{"run", "--llm-response", responsePath, "--request", recordedRef})
	})
	if !strings.Contains(runOut, "method_upgrade") || !strings.Contains(runOut, "park") {
		t.Fatalf("recorded request did not replay through grounded run:\n%s", runOut)
	}
	rows := readCandidateRows(t, filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade)))
	if len(rows) != 1 || !stringSliceContains(rows[0].EvidenceRefs, "raw_trace:trace_recorded_request") {
		t.Fatalf("replayed candidate not grounded in recorded request: %+v", rows)
	}
}

func TestMemoryImportPageCreatesEvidenceAndCandidatesWithoutClaims(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	pagePath := filepath.Join("..", "..", "fixtures", "brain", "knowledge-page.md")

	out := captureStdout(t, func() {
		cmdMemory([]string{"import-page", pagePath})
	})
	if !strings.Contains(out, "# memory import-page kg-synthetic-visual-standard") ||
		!strings.Contains(out, "- candidates: 2") ||
		!strings.Contains(out, "- created: 2") {
		t.Fatalf("import-page output =\n%s", out)
	}
	cands, err := os.ReadDir(filepath.Join(homePath, candidates.InboxDir(candidates.Memory)))
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidate files = %d, want 2", len(cands))
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("import-page must not promote claims, stat err=%v", err)
	}

	out = captureStdout(t, func() {
		cmdMemory([]string{"import-page", pagePath})
	})
	if !strings.Contains(out, "- created: 0") || !strings.Contains(out, "- reused: 2") {
		t.Fatalf("second import should be idempotent:\n%s", out)
	}
}

func TestMemoryTimelinePrintsEvidenceAndSupersessionRefs(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Capture([]mem.Candidate{{
		Entity:          "MemoryEngine",
		Predicate:       "authority_status",
		Object:          "rented index",
		ObservationDate: cliTestNow.Format(time.RFC3339),
		Confidence:      0.9,
		Derivation:      "deterministic",
		ClaimType:       "architecture",
		PromotionBasis:  candidates.BasisRuntimeProof,
		SourceTraceRef:  "compound://runtime-proof/first",
		SnippetHash:     "sha256:first",
		Visibility:      "operational",
	}}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	firstResult, err := store.Adjudicate(first[0].ID, "promote", "work_timeline_proof", cliTestNow)
	if err != nil {
		t.Fatalf("promote first: %v", err)
	}
	second, err := store.Capture([]mem.Candidate{{
		Entity:          "MemoryEngine",
		Predicate:       "authority_status",
		Object:          "rented index with reference coverage",
		ObservationDate: cliTestNow.Format(time.RFC3339),
		Confidence:      0.96,
		Derivation:      "deterministic",
		ClaimType:       "architecture",
		PromotionBasis:  candidates.BasisRuntimeProof,
		SourceTraceRef:  "compound://runtime-proof/second",
		SnippetHash:     "sha256:second",
		Visibility:      "operational",
	}}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := store.Adjudicate(second[0].ID, "promote", "work_timeline_proof", cliTestNow)
	if err != nil {
		t.Fatalf("promote second: %v", err)
	}
	out := captureStdout(t, func() {
		cmdMemory([]string{"timeline", "MemoryEngine"})
	})
	for _, want := range []string{
		firstResult.Claim.ID,
		secondResult.Claim.ID,
		"evidence=" + first[0].ID + ",compound://runtime-proof/first",
		"evidence=" + second[0].ID + ",compound://runtime-proof/second",
		"supersedes=" + firstResult.Claim.ID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("timeline output missing %q:\n%s", want, out)
		}
	}
}

func TestMemoryCandidateRelationsRemainStructuralEdges(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() {
		cmdMemory([]string{"candidate", filepath.Join("..", "..", "fixtures", "traces", "microservice-graph.json")})
	})
	if !strings.Contains(out, "service:gateway -depends_on-> service:auth") ||
		!strings.Contains(out, "service:auth -depends_on-> lib:jwt") {
		t.Fatalf("memory candidate did not persist structural relations:\n%s", out)
	}
	edges := readFile(t, filepath.Join(homePath, "ledger", "edges.jsonl"))
	if !strings.Contains(edges, `"kind":"depends_on"`) ||
		!strings.Contains(edges, `"from":"service:gateway"`) ||
		!strings.Contains(edges, `"to":"lib:jwt"`) {
		t.Fatalf("relation edges not recorded structurally:\n%s", edges)
	}
	if _, err := os.Stat(filepath.Join(homePath, "inbox", "relation-candidates")); !os.IsNotExist(err) {
		t.Fatalf("relation must not create candidate inbox, stat err=%v", err)
	}
}

func TestMemoryGraphCommandSupportsDirectionAndKind(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(entity string) {
		got, err := store.Capture([]mem.Candidate{{
			Entity:          entity,
			Predicate:       "kind",
			Object:          "industry",
			ObservationDate: cliTestNow.Format(time.RFC3339),
			Confidence:      0.9,
			Derivation:      "deterministic",
			PromotionBasis:  candidates.BasisRuntimeProof,
			SourceTraceRef:  "trace_cli_graph_" + entity,
			SnippetHash:     "sha256:" + entity,
			Visibility:      "operational",
		}}, cliTestNow)
		if err != nil {
			t.Fatalf("capture %s: %v", entity, err)
		}
		if _, err := store.Adjudicate(got[0].ID, "promote", "work_cli_graph", cliTestNow); err != nil {
			t.Fatalf("promote %s: %v", entity, err)
		}
	}
	for _, entity := range []string{"industry:us-eu-saas", "industry:saas", "industry:tech-2026", "industry:fintech", "noise:benchmark"} {
		mk(entity)
	}
	if _, err := store.Relate("industry:us-eu-saas", "part_of", "industry:saas", 0.9, cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Relate("industry:saas", "part_of", "industry:tech-2026", 0.9, cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Relate("industry:fintech", "part_of", "industry:tech-2026", 0.9, cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Relate("industry:us-eu-saas", "relates", "noise:benchmark", 0.9, cliTestNow); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdMemory([]string{"graph", "industry:us-eu-saas", "--direction", "up", "--kind", "part_of", "--depth", "2"})
	})
	for _, want := range []string{"industry:us-eu-saas.kind = industry", "industry:saas.kind = industry", "industry:tech-2026.kind = industry"} {
		if !strings.Contains(out, want) {
			t.Fatalf("graph output missing %q:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"industry:fintech.kind = industry", "noise:benchmark.kind = industry", "-relates->"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("graph output included forbidden %q:\n%s", forbidden, out)
		}
	}
}

func TestMemoryDoctorUsesConfiguredSidecarEngine(t *testing.T) {
	// Freshness gates (memDoctorThresholds) compare claim ages against
	// real time.Now(); cliTestNow is a fixed 2026-06-29 clock, so a claim
	// minted at cliTestNow reads ~2 months stale and the doctor's !ok path
	// os.Exit(1)s — which kills the test BINARY silently mid-run. This
	// fixture therefore uses a live clock: the claim is seconds old, the
	// promotion hop is fresh, and the doctor proves the engine hop the
	// test exists to prove.
	now := time.Now().UTC()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{{
		Entity:         "project:api",
		Predicate:      "language",
		Object:         "go",
		Confidence:     0.9,
		Derivation:     "extracted",
		PromotionBasis: candidates.BasisTrustedDeterministic,
		SourceTraceRef: "trace_cli_memory_doctor",
		SnippetHash:    "sha256:test",
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(got[0].ID, "promote", "work_cli_memory_doctor", now); err != nil {
		t.Fatal(err)
	}

	bin := compoundMockEngineTestBinary(t)
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", bin)
	t.Setenv("COMPOUND_MEM_ENGINE_EMBEDDER", "reference:lexical:0:0")
	// Clear ambient breadth-embedder env so a developer shell's live
	// embedder (e.g. ollama:nomic-embed-text) cannot leak into the fixture
	// and flip semantic_readiness to embedder_mismatch against the mock's
	// reference:lexical:0:0 stamp. Regression pin for the 2026-08-23/24 flake.
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", "")

	out := captureStdout(t, func() { cmdMemory([]string{"doctor"}) })
	if !strings.Contains(out, "sync/embedding") || !strings.Contains(out, "ok       reference") || !strings.Contains(out, "ledger_unchanged=true") {
		t.Fatalf("memory doctor should prove configured engine hop:\n%s", out)
	}
	jsonOut := captureStdout(t, func() { cmdMemory([]string{"doctor", "--json"}) })
	var report mem.Report
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("memory doctor --json should parse: %v\n%s", err, jsonOut)
	}
	if !report.OK {
		t.Fatalf("memory doctor --json should be ok: %+v", report)
	}
	if report.SemanticReadiness == nil || report.SemanticReadiness.Status != "lexical_only" || report.SemanticReadiness.Sidecar != "configured" {
		t.Fatalf("memory doctor --json should report sidecar lexical readiness: %+v", report.SemanticReadiness)
	}
	var foundEngineHop bool
	for _, hop := range report.Hops {
		if hop.Name == "sync/embedding" && hop.Status == "ok" && strings.Contains(hop.Detail, "ledger_unchanged=true") {
			foundEngineHop = true
		}
	}
	if !foundEngineHop {
		t.Fatalf("memory doctor --json missing configured engine hop: %+v", report.Hops)
	}
}

func TestMemoryDoctorReportsLexicalSemanticReadiness(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", "")
	t.Setenv("COMPOUND_MEM_ENGINE_EMBEDDER", "")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	jsonOut := captureStdout(t, func() { cmdMemory([]string{"doctor", "--json"}) })
	var report mem.Report
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("memory doctor --json parse: %v\n%s", err, jsonOut)
	}
	if report.SemanticReadiness == nil {
		t.Fatalf("semantic_readiness missing: %+v", report)
	}
	got := report.SemanticReadiness
	if got.Status != "lexical_only" || got.Sidecar != "none" || got.VectorCacheStatus != "not_configured" || got.EmbedderConfigured {
		t.Fatalf("semantic readiness = %+v, want lexical_only/no sidecar/no embedder", got)
	}
}

func TestMemoryDoctorReportsSemanticConfiguredWithoutSidecar(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_MEM_ENGINE_EMBEDDER", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "openai")
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "text-embedding-3-small")
	t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", "https://api.openai.com/v1")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	jsonOut := captureStdout(t, func() { cmdMemory([]string{"doctor", "--json"}) })
	var report mem.Report
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("memory doctor --json parse: %v\n%s", err, jsonOut)
	}
	got := report.SemanticReadiness
	if got == nil || got.Status != "semantic_inactive" || !got.EmbedderConfigured || got.EmbedderIdentity != "openai:text-embedding-3-small:1536:f32" || got.Sidecar != "none" {
		t.Fatalf("semantic readiness = %+v, want configured embedder without sidecar", got)
	}
}

func TestMemoryVectorCacheReadinessReportsManifestState(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	identity := "openai:text-embedding-3-small:1536:f32"
	manifestPath := filepath.Join(homePath, "indexes", "breadth", memoryStampSlug(identity), "manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifest := func(t *testing.T, embedded int, model string) {
		t.Helper()
		raw := fmt.Sprintf(`{
  "schema_version": "compound.breadth.index.v1",
  "embedder": {"provider": "openai", "model": %q, "dim": 1536, "bits": "f32"},
  "corpus_gen": 7,
  "claim_count": 2,
  "embedded_count": %d
}
`, model, embedded)
		if err := os.WriteFile(manifestPath, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeManifest(t, 2, "text-embedding-3-small")
	status, _ := memoryVectorCacheReadiness(homePath, identity, 0)
	if status.Status != "healthy" || status.CorpusGen != 7 || status.EmbeddedCount != 2 {
		t.Fatalf("healthy status = %+v", status)
	}
	writeManifest(t, 1, "text-embedding-3-small")
	status, _ = memoryVectorCacheReadiness(homePath, identity, 0)
	if status.Status != "partial" {
		t.Fatalf("partial status = %+v", status)
	}
	writeManifest(t, 2, "text-embedding-3-large")
	status, _ = memoryVectorCacheReadiness(homePath, identity, 0)
	if status.Status != "embedder_mismatch" {
		t.Fatalf("mismatch status = %+v", status)
	}
}

func TestMemoryDoctorEngineProfileWiresReadOnlyCLISidecar(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	// Live clock for the same freshness-gate reason as
	// TestMemoryDoctorUsesConfiguredSidecarEngine: a cliTestNow claim reads
	// months stale and the doctor's !ok path os.Exit(1)s the test binary.
	now := time.Now().UTC()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{{
		Entity:         "project:api",
		Predicate:      "language",
		Object:         "go",
		Confidence:     0.9,
		Derivation:     "extracted",
		PromotionBasis: candidates.BasisTrustedDeterministic,
		SourceTraceRef: "trace_cli_memory_profile",
		SnippetHash:    "sha256:test",
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(got[0].ID, "promote", "work_cli_memory_profile", now); err != nil {
		t.Fatal(err)
	}
	fakeCLI := filepath.Join(t.TempDir(), "memory-cli")
	if err := os.WriteFile(fakeCLI, []byte("#!/bin/sh\necho \"project api language go\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_MEMORY_CLI_BIN", fakeCLI)
	t.Setenv("COMPOUND_MEMORY_CLI_LIMIT", "1")
	// Same ambient-shell isolation as TestMemoryDoctorUsesConfiguredSidecarEngine:
	// a live COMPOUND_BREADTH_EMBED_* from the developer shell must not flip the
	// sync/embedding hop to embedder_mismatch under the memory-cli profile.
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", "")

	jsonOut := captureStdout(t, func() {
		cmdMemory([]string{"doctor", "--engine-profile", "memory-cli", "--json"})
	})
	var report mem.Report
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("memory doctor --engine-profile --json parse: %v\n%s", err, jsonOut)
	}
	var foundEngineHop bool
	for _, hop := range report.Hops {
		if hop.Name == "sync/embedding" && hop.Status == "ok" && strings.Contains(hop.Detail, "memory-cli") && strings.Contains(hop.Detail, "ledger_unchanged=true") {
			foundEngineHop = true
		}
	}
	if !foundEngineHop {
		t.Fatalf("memory doctor --engine-profile missing configured engine hop: %+v", report.Hops)
	}
}

func TestMemoryEngineConfigGbrainProfileIsRetired(t *testing.T) {
	// gbrain is retired as a memory engine (2026-08-08): compound owns memory
	// via the ledger + corpus lanes. A config or flag naming gbrain must fail
	// loudly and direct to breadth, never silently shell to a gbrain binary.
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	if _, err := writeMemoryEngineConfig(homePath, "gbrain"); err == nil {
		t.Fatal("gbrain profile must fail closed, got no error")
	} else if !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "breadth") {
		t.Fatalf("gbrain retirement error is not actionable: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(homePath, "config", "memory-engine.yaml")); err == nil {
		t.Fatal("gbrain profile must not persist a config")
	}
}

func TestMemoryEngineConfigBreadthProfileResolvesOwnedSidecar(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	binDir := t.TempDir()
	sidecarPath := filepath.Join(binDir, "compound-breadth-engine")
	if err := os.WriteFile(sidecarPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake breadth engine: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out := captureStdout(t, func() {
		cmdMemory([]string{"engine-config", "--profile", "breadth"})
	})
	if !strings.Contains(out, "- configured: true") || !strings.Contains(out, "- profile: breadth") {
		t.Fatalf("engine-config breadth output =\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(homePath, "config", "memory-engine.yaml"))
	if err != nil {
		t.Fatalf("read memory engine config: %v", err)
	}
	if !strings.Contains(string(data), "profile: breadth") {
		t.Fatalf("memory engine config did not persist breadth profile:\n%s", data)
	}

	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "gbrain")
	restore, err := configureMemoryEngineFromHome(homePath, "")
	if err != nil {
		t.Fatalf("configure persisted breadth profile: %v", err)
	}
	defer restore()
	if got := os.Getenv("COMPOUND_MEM_ENGINE_BIN"); got != sidecarPath {
		t.Fatalf("breadth profile sidecar = %q, want %q", got, sidecarPath)
	}
	if got, ok := os.LookupEnv("COMPOUND_MEMORY_CLI_PROFILE"); ok {
		t.Fatalf("breadth profile should not keep memory CLI profile, got %q", got)
	}
}

func TestMemoryEngineProfileFlagOverridesPersistedConfig(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	// Persist a breadth config, then override it with an explicit profile flag.
	// (gbrain is retired as a memory engine, so it can no longer be persisted.)
	if _, err := writeMemoryEngineConfig(homePath, "breadth"); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "")

	restore, err := configureMemoryEngineFromHome(homePath, "memory-cli")
	if err != nil {
		t.Fatalf("configure explicit profile: %v", err)
	}
	defer restore()
	if got := os.Getenv("COMPOUND_MEMORY_CLI_PROFILE"); got != "memory-cli" {
		t.Fatalf("explicit profile = %q, want memory-cli", got)
	}
}

func TestProjectionDoctorSourceObserveWritesMethodUpgradeNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdProjection([]string{"doctor", filepath.Join("..", "..", "fixtures", "projections", "stale-freshness.yaml"), "--source-observe"})
	})
	if !strings.Contains(out, "- status: stale") ||
		!strings.Contains(out, "- source_observation: srcobs_") ||
		!strings.Contains(out, "- source_candidate:") ||
		!strings.Contains(out, "- source_no_promote:") {
		t.Fatalf("projection source-observe output missing proof lines:\n%s", out)
	}

	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.MethodUpgrade))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read method-upgrade dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("method-upgrade entries = %d, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(candidateDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read method-upgrade candidate: %v", err)
	}
	var candidate sourceobs.CandidateRecord
	if err := json.Unmarshal(data, &candidate); err != nil {
		t.Fatalf("parse method-upgrade candidate: %v", err)
	}
	if candidate.Kind != string(candidates.MethodUpgrade) || candidate.Verdict != candidates.NoPromote || candidate.PromoteAllowed {
		t.Fatalf("candidate = %+v", candidate)
	}
	if candidate.Subject != "projection:synthetic-runtime-dashboard" ||
		!strings.HasPrefix(candidate.SourceObservationID, "srcobs_") ||
		candidate.SourceRef != "generated-dashboard.md" {
		t.Fatalf("candidate did not keep projection source identity: %+v", candidate)
	}

	rows := readNoPromoteDecisionRows(t, homePath)
	if len(rows) != 1 {
		t.Fatalf("no-promote rows = %d, want 1", len(rows))
	}
	if rows[0].CandidateRef != candidate.ID || rows[0].OwnerTarget.Kind != "source_observation" || !strings.HasPrefix(rows[0].OwnerTarget.Ref, "srcobs_") {
		t.Fatalf("no-promote row not tied to projection source observation: %+v", rows[0])
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("projection source-observe should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestProjectionRepairReviewCLIRecordsReviewedSidecar(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	planOut := captureStdout(t, func() {
		cmdProjection([]string{"repair-plan", filepath.Join("..", "..", "fixtures", "projections", "stale-freshness.yaml")})
	})
	if !strings.Contains(planOut, "# projection repair-plan") || !strings.Contains(planOut, "- review_yaml:") {
		t.Fatalf("projection repair-plan output missing review YAML:\n%s", planOut)
	}
	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: projection.ProjectionRegenerationReviewArea}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	projection.AnnotateProjectionRegenerationQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.Total != 1 || queue.Counts.ReviewRequired != 1 || queue.Items[0].Readiness != "needs_review" {
		t.Fatalf("initial projection regeneration queue = %+v", queue)
	}
	reviewYAML := ""
	for _, path := range queue.Items[0].ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			reviewYAML = path
			break
		}
	}
	if reviewYAML == "" {
		t.Fatalf("review YAML missing from queue item: %+v", queue.Items[0])
	}
	review, err := projection.LoadRegenerationReview(reviewYAML)
	if err != nil {
		t.Fatalf("load review YAML: %v", err)
	}
	review.Decision = "owner_action_required"
	review.Rationale = "owner reviewed stale projection; regenerate through owner generator before trusting dashboard"
	review.Confidence = 0.86
	review.NextAction = "run owner generator and rerun projection doctor"
	edited, err := yaml.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewYAML, edited, 0o644); err != nil {
		t.Fatalf("write edited review YAML: %v", err)
	}

	reviewOut := captureStdout(t, func() {
		cmdProjection([]string{"repair-review", reviewYAML})
	})
	if !strings.Contains(reviewOut, "# projection repair-review") ||
		!strings.Contains(reviewOut, "- status: recorded") ||
		!strings.Contains(reviewOut, "- decision: owner_action_required") ||
		!strings.Contains(reviewOut, "- reviewed:") {
		t.Fatalf("projection repair-review output missing proof lines:\n%s", reviewOut)
	}
	queue, err = reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: projection.ProjectionRegenerationReviewArea}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	projection.AnnotateProjectionRegenerationQueue(&queue)
	reviewartifact.Recount(&queue)
	if queue.Counts.OK != 1 || queue.Counts.ReviewRequired != 0 || queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "review_recorded" {
		t.Fatalf("recorded projection regeneration queue = %+v", queue)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("projection repair-review should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestHarnessObserveWritesCapabilityNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	out := captureStdout(t, func() { cmdHarness([]string{"observe"}) })
	for _, want := range []string{"# harness observe", "- source_observation: srcobs_", "- source_candidate:", "- source_no_promote:", "- verdict: capability_evidence_only_not_authority"} {
		if !strings.Contains(out, want) {
			t.Fatalf("harness observe output missing %q:\n%s", want, out)
		}
	}
	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatalf("read capability observation dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("capability observation entries = %d, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(candidateDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read capability observation: %v", err)
	}
	var candidate struct {
		ID                  string `json:"id"`
		Kind                string `json:"kind"`
		Subject             string `json:"subject"`
		Verdict             string `json:"verdict"`
		SourceObservationID string `json:"source_observation_id"`
		PromoteAllowed      bool   `json:"promote_allowed"`
	}
	if err := json.Unmarshal(data, &candidate); err != nil {
		t.Fatalf("parse capability observation: %v", err)
	}
	if candidate.Kind != string(candidates.CapabilityObs) || candidate.Subject != "harness_registry:default" || candidate.Verdict != candidates.NoPromote || candidate.PromoteAllowed || candidate.SourceObservationID == "" {
		t.Fatalf("candidate = %+v", candidate)
	}
	rows := readNoPromoteDecisionRows(t, homePath)
	if len(rows) != 1 || rows[0].CandidateRef != candidate.ID || rows[0].OwnerTarget.Kind != "source_observation" || rows[0].OwnerTarget.Ref != candidate.SourceObservationID {
		t.Fatalf("no-promote rows = %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("harness observation should not promote claims, stat err=%v", err)
	}
}

func TestHarnessObserveManifestWritesCapabilityNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := filepath.Join("..", "..", "fixtures", "harness", "harness-registry.yaml")
	out := captureStdout(t, func() {
		cmdHarness([]string{"observe", fixture})
	})
	for _, want := range []string{"# harness observe", "- source_observation: srcobs_", "- source_candidate:", "- source_no_promote:", "- verdict: capability_evidence_only_not_authority"} {
		if !strings.Contains(out, want) {
			t.Fatalf("harness manifest observe output missing %q:\n%s", want, out)
		}
	}
	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("capability candidate entries = %d, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(candidateDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var candidate sourceobs.CandidateRecord
	if err := json.Unmarshal(data, &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.Subject != "harness_registry:fixture-harness-registry" || candidate.Verdict != candidates.NoPromote || candidate.PromoteAllowed {
		t.Fatalf("candidate = %+v", candidate)
	}
	rows := readNoPromoteDecisionRows(t, homePath)
	if len(rows) != 1 || rows[0].CandidateRef != candidate.ID {
		t.Fatalf("no-promote rows = %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("harness manifest observation should not promote claims, stat err=%v", err)
	}
}

func TestHarnessObserveOSEntrypointsWritesCapabilityNoPromote(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := filepath.Join("..", "..", "fixtures", "harness", "os-harness-entrypoints.yaml")
	out := captureStdout(t, func() {
		cmdHarness([]string{"observe", "--os-entrypoints", fixture})
	})
	for _, want := range []string{"# harness observe", "- source_observation: srcobs_", "- source_candidate:", "- source_no_promote:", "- verdict: capability_evidence_only_not_authority"} {
		if !strings.Contains(out, want) {
			t.Fatalf("OS entrypoints observe output missing %q:\n%s", want, out)
		}
	}
	candidateDir := filepath.Join(homePath, candidates.InboxDir(candidates.CapabilityObs))
	entries, err := os.ReadDir(candidateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("capability candidate entries = %d, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(candidateDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var candidate sourceobs.CandidateRecord
	if err := json.Unmarshal(data, &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.Subject != "harness_registry:os-harness-entrypoints" || candidate.Verdict != candidates.NoPromote || candidate.PromoteAllowed {
		t.Fatalf("candidate = %+v", candidate)
	}
	rows := readNoPromoteDecisionRows(t, homePath)
	if len(rows) != 1 || rows[0].CandidateRef != candidate.ID {
		t.Fatalf("no-promote rows = %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("OS entrypoint observation should not promote claims, stat err=%v", err)
	}
}

func TestProjectionRepairPlanCreatesGateSourceObservationAndReview(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	out := captureStdout(t, func() {
		cmdProjection([]string{"repair-plan", filepath.Join("..", "..", "fixtures", "projections", "stale-freshness.yaml")})
	})
	if !strings.Contains(out, "# projection repair-plan") ||
		!strings.Contains(out, "- writeback_gate: owg_") ||
		!strings.Contains(out, "- source_observation: srcobs_") ||
		!strings.Contains(out, "- source_candidate:") ||
		!strings.Contains(out, "- source_no_promote:") ||
		!strings.Contains(out, "- review:") ||
		!strings.Contains(out, "- artifact:") {
		t.Fatalf("repair-plan output missing proof lines:\n%s", out)
	}

	planDir := filepath.Join(homePath, "reviews", "projection-regeneration")
	planFiles, err := filepath.Glob(filepath.Join(planDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	reviewFiles, err := filepath.Glob(filepath.Join(planDir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(planFiles) != 1 || len(reviewFiles) != 1 {
		t.Fatalf("plan/review files = %d/%d", len(planFiles), len(reviewFiles))
	}
	var plan struct {
		SchemaVersion       string   `json:"schema_version"`
		Status              string   `json:"status"`
		WritebackGateID     string   `json:"writeback_gate_id"`
		SourceObservationID string   `json:"source_observation_id"`
		CandidateIDs        []string `json:"candidate_ids"`
		NoPromoteIDs        []string `json:"no_promote_ids"`
	}
	data, err := os.ReadFile(planFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != projection.RegenerationPlanSchemaVersion || plan.Status != "review_required" ||
		!strings.HasPrefix(plan.WritebackGateID, "owg_") || !strings.HasPrefix(plan.SourceObservationID, "srcobs_") ||
		len(plan.CandidateIDs) != 1 || len(plan.NoPromoteIDs) != 1 {
		t.Fatalf("bad regeneration plan: %+v", plan)
	}
	review, err := os.ReadFile(reviewFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(review), "does not regenerate or edit the generated view") {
		t.Fatalf("review did not preserve no-direct-write warning:\n%s", review)
	}
	rows := readNoPromoteDecisionRows(t, homePath)
	if len(rows) != 1 || rows[0].CandidateRef != plan.CandidateIDs[0] {
		t.Fatalf("no-promote rows = %+v plan=%+v", rows, plan)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("projection repair-plan should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestRuntimeRepairPlanCreatesGateAndReview(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	out := captureStdout(t, func() {
		cmdRuntime([]string{"repair-plan", filepath.Join("..", "..", "fixtures", "runtime", "broken-service.yaml")})
	})
	for _, want := range []string{
		"# runtime repair-plan rrp_",
		"- failed_checks: 1",
		"- writeback_gate: owg_",
		"- work: work_",
		"- receipt: rec_",
		"- transition: tr_",
		"- plan:",
		"- review:",
		"- review_yaml:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("runtime repair-plan output missing %q:\n%s", want, out)
		}
	}
	planDir := filepath.Join(homePath, "reviews", "runtime-repair")
	planFiles, err := filepath.Glob(filepath.Join(planDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	reviewFiles, err := filepath.Glob(filepath.Join(planDir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(planFiles) != 1 || len(reviewFiles) != 1 {
		t.Fatalf("plan/review files = %d/%d", len(planFiles), len(reviewFiles))
	}
	var plan struct {
		SchemaVersion   string `json:"schema_version"`
		Status          string `json:"status"`
		FailedCount     int    `json:"failed_count"`
		WritebackGateID string `json:"writeback_gate_id"`
		ReceiptID       string `json:"receipt_id"`
		TransitionID    string `json:"transition_id"`
	}
	data, err := os.ReadFile(planFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != runtimebinding.RepairPlanSchemaVersion ||
		plan.Status != "review_required" ||
		plan.FailedCount != 1 ||
		!strings.HasPrefix(plan.WritebackGateID, "owg_") ||
		!strings.HasPrefix(plan.ReceiptID, "rec_") ||
		!strings.HasPrefix(plan.TransitionID, "tr_") {
		t.Fatalf("bad runtime repair plan: %+v", plan)
	}
	review, err := os.ReadFile(reviewFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(review), "does not mutate runtime owner files") {
		t.Fatalf("review did not preserve no-direct-write warning:\n%s", review)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("runtime repair-plan should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestRuntimeRepairReviewCLIRecordsReviewedSidecar(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	planOut := captureStdout(t, func() {
		cmdRuntime([]string{"repair-plan", filepath.Join("..", "..", "fixtures", "runtime", "broken-service.yaml")})
	})
	reviewYAML := outputValue(planOut, "- review_yaml:")
	if reviewYAML == "" {
		t.Fatalf("runtime repair-plan did not print review_yaml:\n%s", planOut)
	}
	body, err := os.ReadFile(reviewYAML)
	if err != nil {
		t.Fatal(err)
	}
	reviewedYAML := strings.ReplaceAll(string(body), "TODO_decision_owner_action_required_owner_action_done_defer_no_action", "owner_action_required")
	reviewedYAML = strings.ReplaceAll(reviewedYAML, "rationale: \"\"", "rationale: \"owner reviewed runtime repair path\"")
	reviewedYAML = strings.ReplaceAll(reviewedYAML, "confidence: -1", "confidence: 0.91")
	reviewedYAML = strings.ReplaceAll(reviewedYAML, "TODO: reviewed runtime repair next action", "rerun runtime doctor after owner contract edit")
	if err := os.WriteFile(reviewYAML, []byte(reviewedYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewOut := captureStdout(t, func() {
		cmdRuntime([]string{"repair-review", reviewYAML})
	})
	if !strings.Contains(reviewOut, "# runtime repair-review") ||
		!strings.Contains(reviewOut, "- status: recorded") ||
		!strings.Contains(reviewOut, "- decision: owner_action_required") ||
		!strings.Contains(reviewOut, "- reviewed:") {
		t.Fatalf("runtime repair-review output missing proof:\n%s", reviewOut)
	}
	queueOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", runtimebinding.RuntimeRepairReviewArea, "--json"})
	})
	var report reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(queueOut), &report); err != nil {
		t.Fatalf("parse review queue: %v\n%s", err, queueOut)
	}
	if report.Counts.Total != 1 || report.Counts.OK != 1 || report.Counts.ReviewRequired != 0 {
		t.Fatalf("runtime repair review should close queue debt: %+v", report.Counts)
	}
	if len(report.Items) != 1 || report.Items[0].Status != "recorded" || report.Items[0].Readiness != "review_recorded" {
		t.Fatalf("runtime repair item not recorded: %+v", report.Items)
	}
	if !strings.Contains(report.Items[0].NextAction, "rerun runtime doctor") {
		t.Fatalf("reviewed next action missing: %+v", report.Items[0])
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("runtime repair-review should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestRuntimeDoctorLiveProbeIsOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "runtime-live.yaml")
	body := `runtime_binding:
  service_id: service:live-api
  owner_entity: product:live
  host_binding: host:local-test
  deployment_binding: process:live-api
  data_bindings: [sqlite:live.db]
  health_check_refs: [` + srv.URL + `/health]
  risk_tier: tier1
  evidence_refs: [fixture:runtime-binding]
  status: active
service_contract:
  service_id: service:live-api
  interface_refs: [http:get:/health]
  owner_entity: product:live
  risk_tier: tier1
  evidence_refs: [fixture:service-contract]
  status: active
runtime_probes:
  - id: health
    kind: http
    target: ` + srv.URL + `/health
    expected_status: 204
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdRuntime([]string{"doctor", path})
	})
	if strings.Contains(out, "live:health") {
		t.Fatalf("structural doctor should not run live probe without --live:\n%s", out)
	}

	out = captureStdout(t, func() {
		cmdRuntime([]string{"doctor", "--live", path})
	})
	if !strings.Contains(out, "- live:health: ok") || !strings.Contains(out, "- status: ok") {
		t.Fatalf("live doctor output:\n%s", out)
	}
}

func TestProjectionDashboardReadModelSourceObserve(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "source.json"), []byte(`{"ok":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(dir, "read-model.json")
	body := `{
  "version": 1,
  "generated_at": "2000-01-01T00:00:00Z",
  "source_paths": ["source.json"],
  "cards": [
    {
      "id": "runtime-health",
      "title": "Runtime health",
      "lane": "control",
      "status": "risk",
      "owner": "system",
      "source_paths": ["source.json"],
      "next_action": "review runtime evidence",
      "visibility": "operator"
    }
  ]
}
`
	if err := os.WriteFile(modelPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		cmdProjection([]string{"dashboard-read-model", modelPath, "--max-age-hours", "1", "--source-observe"})
	})
	for _, want := range []string{"# projection doctor", "- projection: projection:dashboard-read-model", "- status: stale", "- source_observation: srcobs_", "- source_candidate:", "- source_no_promote:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard read-model output missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("dashboard read-model source observe must not promote claims, stat err=%v", err)
	}
}

func TestProjectionContextRecordFlowsIntoOrient(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	out := captureStdout(t, func() {
		cmdProjection([]string{"doctor", filepath.Join("..", "..", "fixtures", "projections", "stale-freshness.yaml"), "--record-context", "--repo-kind", "control-plane"})
	})
	if !strings.Contains(out, "- context_record: pctx_") || !strings.Contains(out, "- context_repo_kinds: control-plane") {
		t.Fatalf("projection doctor should record orientation context:\n%s", out)
	}
	out = captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--repo-kind", "control-plane", "--task", "runtime dashboard"})
	})
	var control envelope.ContextPack
	if err := json.Unmarshal([]byte(out), &control); err != nil {
		t.Fatalf("parse control context pack: %v\n%s", err, out)
	}
	if !contextPackIncludes(control, "projection://synthetic-runtime-dashboard") {
		t.Fatalf("control-plane orient should include projection context: %+v", control.Included)
	}
	var projectionRef envelope.IncludedRef
	for _, included := range control.Included {
		if included.Ref == "projection://synthetic-runtime-dashboard" {
			projectionRef = included
			break
		}
	}
	if projectionRef.SourceClass != "generated" || projectionRef.Status != "stale" ||
		!strings.Contains(projectionRef.Reason, "generated-view context evidence") {
		t.Fatalf("projection context ref should be generated/stale evidence, got %+v", projectionRef)
	}
	out = captureStdout(t, func() {
		cmdOrient([]string{"--harness", "codex", "--repo-kind", "product", "--task", "runtime dashboard"})
	})
	var product envelope.ContextPack
	if err := json.Unmarshal([]byte(out), &product); err != nil {
		t.Fatalf("parse product context pack: %v\n%s", err, out)
	}
	if contextPackIncludes(product, "projection://synthetic-runtime-dashboard") {
		t.Fatalf("product orient should not inherit control-plane projection context: %+v", product.Included)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("projection context orientation must not promote claims, stat err=%v", err)
	}
}

func testActionIntent(workID, id, capability string) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            id,
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound-test"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "codex",
		Capability:    capability,
		Action:        "write_receipt",
		Target:        envelope.ActionTarget{Kind: "path", Path: "events"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
	}
}

func testPolicyDecision(workID, id, actionID string) envelope.PolicyDecision {
	return envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision,
			ID:            id,
			CreatedAt:     cliTestNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "policy", Name: "compound-test"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: actionID,
		Outcome:        "allow_with_obligations",
		Reasons:        []string{"test action is local and reversible"},
		Obligations:    []string{"emit receipt"},
	}
}

func TestDecisionsRepairPlanBrainWritesReviewArtifactWithoutPromotion(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	brainPath := filepath.Join(t.TempDir(), "brain", "wiki", "reflections", "decision.md")
	if err := os.MkdirAll(filepath.Dir(brainPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brainPath, []byte(`---
type: reflection
title: 2026 06 11 Memory Engine Ownership Decision 537a02
date: '2026-06-11T00:00:00.000Z'
---

# memory engine ownership decision - own contract, rent engine

## decision

The verdict:

> **"Keep the rented memory engine continuously updated. Own the contract seam."**

## How apply

When ReleaseX ships or is externally blocked, execute the parked spec.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdDecisions([]string{"repair-plan", "--brain", brainPath})
	})
	if !strings.Contains(out, "# decisions repair-plan") ||
		!strings.Contains(out, "source_kind: brain") ||
		!strings.Contains(out, "repairs: 1") ||
		!strings.Contains(out, "missing_falsifier: 1") ||
		!strings.Contains(out, "missing_review_trigger: 0") {
		t.Fatalf("repair-plan output =\n%s", out)
	}
	planPath := outputValue(out, "plan:")
	if planPath == "" {
		t.Fatalf("missing plan path in output:\n%s", out)
	}
	plan, err := decisions.LoadLifecycleRepairPlan(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceKind != "brain" || plan.RepairCount != 1 || len(plan.Repairs) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.HasPrefix(plan.Repairs[0].EvidenceRef, "brain_source:") {
		t.Fatalf("evidence ref = %q", plan.Repairs[0].EvidenceRef)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("repair-plan should not promote claims, claims ledger stat err=%v", err)
	}
}

func TestDecisionsRepairApplyBrainReviewedCopyClosesDebt(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	brainPath := filepath.Join(t.TempDir(), "brain", "wiki", "reflections", "decision.md")
	if err := os.MkdirAll(filepath.Dir(brainPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brainPath, []byte(`---
type: reflection
title: 2026 06 11 Memory Engine Ownership Decision 537a02
date: '2026-06-11T00:00:00.000Z'
---

# memory engine ownership decision - own contract, rent engine

## decision

The verdict:

> **"Keep the rented memory engine continuously updated. Own the contract seam."**

## How apply

When ReleaseX ships or is externally blocked, execute the parked spec.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	planOut := captureStdout(t, func() {
		cmdDecisions([]string{"repair-plan", "--brain", brainPath})
	})
	planPath := outputValue(planOut, "plan:")
	if planPath == "" {
		t.Fatalf("missing plan path:\n%s", planOut)
	}
	plan, err := decisions.LoadLifecycleRepairPlan(planPath)
	if err != nil {
		t.Fatal(err)
	}
	plan.Status = "reviewed"
	planData, err := json.MarshalIndent(plan, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, append(planData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	applyOut := captureStdout(t, func() {
		cmdDecisions([]string{"repair-apply", "--plan", planPath, "--write-reviewed"})
	})
	if !strings.Contains(applyOut, "dry_run: false") ||
		!strings.Contains(applyOut, "applied_fields: 1") ||
		!strings.Contains(applyOut, "changed_files: 1") {
		t.Fatalf("repair-apply output =\n%s", applyOut)
	}
	body := readFile(t, brainPath)
	if !strings.Contains(body, "falsifier:") {
		t.Fatalf("reviewed brain copy missing applied falsifier:\n%s", body)
	}

	afterOut := captureStdout(t, func() {
		cmdDecisions([]string{"repair-plan", "--brain", brainPath})
	})
	afterPlanPath := outputValue(afterOut, "plan:")
	if afterPlanPath == "" {
		t.Fatalf("missing after plan path:\n%s", afterOut)
	}
	afterPlan, err := decisions.LoadLifecycleRepairPlan(afterPlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterPlan.RepairCount != 0 || afterPlan.Status != "ok" {
		t.Fatalf("after plan = %+v", afterPlan)
	}

	captureStdout(t, func() {
		cmdDecisions([]string{"import", "--brain", brainPath})
	})
	claims := splitJSONLines(readFile(t, filepath.Join(homePath, "ledger", "claims.jsonl")))
	if len(claims) != 1 {
		t.Fatalf("claims = %v", claims)
	}
}

func TestDecisionsGenericSourceDirImportRepairAndAuthorityCoverage(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	sourceDir := t.TempDir()
	writeSourceFile := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(sourceDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSourceFile("README.md", "# Decision index")
	writeSourceFile("decision-template.md", "# Template")
	writeSourceFile("2026-06-30-complete.md", `---
title: Complete note decision
status: accepted
falsifier: Complete decision no longer protects the behavior.
review_trigger: Re-check when behavior changes.
---
# Complete note decision

## Decision
Use Compound as the decision authority.
`)
	writeSourceFile("2026-06-30-incomplete.md", `---
title: Incomplete note decision
status: accepted
---
# Incomplete note decision

## Decision
Park until lifecycle proof is reviewed.
`)
	importOut := captureStdout(t, func() {
		cmdDecisions([]string{"import", "--source-dir", sourceDir, "--source-kind", "note"})
	})
	if !strings.Contains(importOut, "processed 2 decision sources") ||
		!strings.Contains(importOut, "compound_primary") ||
		!strings.Contains(importOut, "compound_parked") ||
		!strings.Contains(importOut, "- source note-") {
		t.Fatalf("import output =\n%s", importOut)
	}
	repairOut := captureStdout(t, func() {
		cmdDecisions([]string{"repair-plan", "--source-dir", sourceDir, "--source-kind", "note"})
	})
	if !strings.Contains(repairOut, "source_kind: note") || !strings.Contains(repairOut, "repairs: 1") {
		t.Fatalf("repair output =\n%s", repairOut)
	}
	authorityOut := captureStdout(t, func() {
		cmdDecisions([]string{"authority", "--source-dir", sourceDir, "--source-kind", "note"})
	})
	for _, want := range []string{
		"# decisions authority",
		"- status: ok",
		"- eligible: 2",
		"- covered: 2",
		"- missing: 0",
		"source_kind.note: 2",
		"authority_status.compound_primary: 1",
		"authority_status.compound_parked: 1",
	} {
		if !strings.Contains(authorityOut, want) {
			t.Fatalf("authority output missing %q:\n%s", want, authorityOut)
		}
	}
	authorityJSON := captureStdout(t, func() {
		cmdDecisions([]string{"authority", "--source-dir", sourceDir, "--source-kind", "note", "--json"})
	})
	var authorityReport struct {
		SchemaVersion  string         `json:"schema_version"`
		Status         string         `json:"status"`
		AuthorityRef   string         `json:"authority_ref"`
		RecordCount    int            `json:"record_count"`
		Eligible       int            `json:"eligible"`
		Covered        int            `json:"covered"`
		Missing        int            `json:"missing"`
		CountsByKind   map[string]int `json:"counts_by_kind"`
		CountsByStatus map[string]int `json:"counts_by_status"`
	}
	if err := json.Unmarshal([]byte(authorityJSON), &authorityReport); err != nil {
		t.Fatalf("parse authority JSON: %v\n%s", err, authorityJSON)
	}
	if authorityReport.SchemaVersion != "compound.decision_source_authority_coverage.v0.1" ||
		authorityReport.Status != "ok" ||
		authorityReport.AuthorityRef != "compound://registries/decision-source-authority" ||
		authorityReport.RecordCount != 2 ||
		authorityReport.Eligible != 2 ||
		authorityReport.Covered != 2 ||
		authorityReport.Missing != 0 ||
		authorityReport.CountsByKind["note"] != 2 ||
		authorityReport.CountsByStatus["compound_primary"] != 1 ||
		authorityReport.CountsByStatus["compound_parked"] != 1 {
		t.Fatalf("bad authority JSON: %+v\n%s", authorityReport, authorityJSON)
	}
}

func writeHookFixture(t *testing.T, fixture hook.Fixture) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.json")
	writeJSONFixture(t, path, fixture)
	return path
}

func onlyWork(t *testing.T, store *work.Store) envelope.WorkEnvelope {
	t.Helper()
	works, err := store.List()
	if err != nil {
		t.Fatalf("list work: %v", err)
	}
	if len(works) != 1 {
		t.Fatalf("work count = %d, want 1: %+v", len(works), works)
	}
	return works[0]
}

func readContextPack(t *testing.T, homePath, id string) envelope.ContextPack {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(homePath, "context", "packs", id+".json"))
	if err != nil {
		t.Fatalf("read context pack %s: %v", id, err)
	}
	var pack envelope.ContextPack
	if err := json.Unmarshal(b, &pack); err != nil {
		t.Fatalf("parse context pack %s: %v", id, err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("context pack %s invalid: %v", id, err)
	}
	return pack
}

func contextPackIncludes(pack envelope.ContextPack, ref string) bool {
	for _, included := range pack.Included {
		if included.Ref == ref {
			return true
		}
	}
	return false
}

func contextPackExcludes(pack envelope.ContextPack, ref string) bool {
	for _, excluded := range pack.Excluded {
		if excluded.Ref == ref && excluded.Reason != "" {
			return true
		}
	}
	return false
}

func writeJSONFixture(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExternalRepoIntakeContextFixture(t *testing.T, homePath string) {
	t.Helper()
	batchDir := filepath.Join(homePath, "inbox", "external-repo-intake", "2026-07-04-context")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	writeJSONFixture(t, mapPath, map[string]any{
		"boundaries": []string{"review-only", "no install"},
		"items": []map[string]any{
			{
				"repo":             "gitroomhq/postiz-app",
				"owner":            "product outbound distribute pipe",
				"next_review_step": "evaluate rented posting edge behind PolicyDecision; posting receipts feed product loops",
				"not_to_do":        "do not auth social accounts or auto-post",
			},
		},
	})
	writeJSONFixture(t, filepath.Join(batchDir, "run-summary.json"), map[string]any{
		"schema_version":          "compound.external_repo_intake_run.v0.1",
		"id":                      "2026-07-04-context",
		"created_at":              "2026-07-04T07:30:00Z",
		"map_path":                mapPath,
		"source_observation_path": filepath.Join(batchDir, "source-observation.json"),
		"candidate_count":         1,
		"counts_by_decision": map[string]any{
			"keep_needs_review": 1,
		},
		"counts_by_lane": map[string]any{
			"distribution_adapter_candidate": 1,
		},
		"decisions": []map[string]any{
			{
				"repo":              "gitroomhq/postiz-app",
				"decision":          "keep_needs_review",
				"lane":              "distribution_adapter_candidate",
				"status":            "recorded",
				"evidence_strength": 0.86,
			},
		},
	})
}

func surfaceReportHasOffer(report capgraph.SurfaceReport, id string) bool {
	for _, offer := range report.Offers {
		if offer.Node.ID == id {
			return true
		}
	}
	return false
}

func outboundPlanHasGate(gates []capgraph.OutboundDistributionGate, kind string) bool {
	for _, gate := range gates {
		if gate.Kind == kind {
			return true
		}
	}
	return false
}

func writeCLIJobRosterFixture(t *testing.T, path string) {
	t.Helper()
	data := `{
  "version": 1,
  "jobs": [
    {
      "job_id": "d422776dde1c",
      "name": "ibkr-exit-check-daily",
      "enabled": true,
      "state": "scheduled",
      "schedule": "33 19 * * 1-5",
      "script": "ibkr_exit_check_daily.sh",
      "lane": "no_agent_script",
      "last_status": "error",
      "last_error": "missing python venv",
      "last_run_at": "2026-07-01T10:00:00Z"
    },
    {
      "job_id": "digest",
      "name": "brain digest",
      "enabled": true,
      "state": "scheduled",
      "schedule": "every 6h",
      "script": "recall_digest.ts",
      "last_status": "ok",
      "last_run_at": "2026-07-01T09:00:00Z"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCLIJobTokenUsageFixture(t *testing.T, path string) {
	t.Helper()
	data := `{
  "version": 1,
  "generated_at": "2026-07-01T12:00:00Z",
  "metric": "all_tokens",
  "sessions_count": 8,
  "jobs": [
    {
      "job_id": "digest",
      "name": "brain digest",
      "enabled": true,
      "no_agent": false,
      "runs": 4,
      "total_tokens": 1200000,
      "avg_tokens_per_run": 300000,
      "risk_class": "high_token_agent_job"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRouteImportModelCapabilitiesFeedsCapabilityPlan(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	source := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")

	importOut := captureStdout(t, func() {
		cmdRoute([]string{"import-model-capabilities", source})
	})
	if !strings.Contains(importOut, "# route import-model-capabilities") ||
		!strings.Contains(importOut, "- authority: compound://registries/model-router") ||
		!strings.Contains(importOut, "- models: 5") ||
		!strings.Contains(importOut, "- verdict: compound_primary") {
		t.Fatalf("bad import output:\n%s", importOut)
	}

	modelsJSONOut := captureStdout(t, func() {
		cmdRoute([]string{"model-capabilities", "--json"})
	})
	var modelProjection routeevidence.ModelCapabilitiesProjection
	if err := json.Unmarshal([]byte(modelsJSONOut), &modelProjection); err != nil {
		t.Fatalf("parse model capabilities JSON: %v\n%s", err, modelsJSONOut)
	}
	if modelProjection.SchemaVersion != routeevidence.ModelCapabilitiesProjectionSchema ||
		modelProjection.AuthorityRef != routeevidence.ModelCapabilitiesAuthorityRef ||
		modelProjection.ModelCount != 5 ||
		len(modelProjection.Lanes) != 5 ||
		modelProjection.Lanes[0].ModelID != "auto/extract" ||
		len(modelProjection.Lanes[0].Capabilities) == 0 {
		t.Fatalf("bad model capabilities projection: %+v", modelProjection)
	}

	planOut := captureStdout(t, func() {
		cmdRoute([]string{"plan", "--capability-class", "judge-critique"})
	})
	if !strings.Contains(planOut, "# route capability-plan crp_") ||
		!strings.Contains(planOut, "- routing_rule: routing_defaults[judge-critique]") ||
		!strings.Contains(planOut, "- chain_count: 3") ||
		!strings.Contains(planOut, "lane_justification: routing_defaults[judge-critique][0] -> auto/judge") ||
		!strings.Contains(planOut, "lane_justification: routing_defaults[judge-critique][1] -> deepseek/deepseek-v4-pro") ||
		!strings.Contains(planOut, "lane_justification: routing_defaults[judge-critique][2] -> scout/chat") ||
		!strings.Contains(planOut, "cost_class=api-marginal") ||
		!strings.Contains(planOut, "harness=switchback provider_route=auto/judge effort=high") ||
		!strings.Contains(planOut, "role=tripwire_only") ||
		!strings.Contains(planOut, "- verdict: metadata_only_not_task_success") {
		t.Fatalf("bad route plan output:\n%s", planOut)
	}
	planJSONOut := captureStdout(t, func() {
		cmdRoute([]string{"plan", "--capability-class", "judge-critique", "--json"})
	})
	var plan routeevidence.CapabilityRoutePlan
	if err := json.Unmarshal([]byte(planJSONOut), &plan); err != nil {
		t.Fatalf("parse route plan JSON: %v\n%s", err, planJSONOut)
	}
	if plan.SchemaVersion != routeevidence.CapabilityRoutePlanSchemaVersion ||
		plan.AuthorityRef != routeevidence.ModelCapabilitiesAuthorityRef ||
		plan.CapabilityClass != "judge-critique" ||
		plan.RoutingRule != "routing_defaults[judge-critique]" ||
		len(plan.Chain) != 3 {
		t.Fatalf("bad route plan JSON: %+v", plan)
	}
	costFixture := filepath.Join("..", "..", "fixtures", "route", "provider-calls-cost-gaps.ndjson")
	costOut := captureStdout(t, func() {
		cmdRoute([]string{"cost-doctor", costFixture, "--record"})
	})
	if !strings.Contains(costOut, "- candidate: route_cost_") ||
		!strings.Contains(costOut, "- no_promote: nop_") {
		t.Fatalf("bad route cost-doctor output:\n%s", costOut)
	}
	planWithDebtOut := captureStdout(t, func() {
		cmdRoute([]string{"plan", "--capability-class", "judge-critique"})
	})
	if !strings.Contains(planWithDebtOut, "- pending_route_observations: 1") ||
		!strings.Contains(planWithDebtOut, "match=global:provider_cost_coverage") ||
		!strings.Contains(planWithDebtOut, "review=\"compound route plan --candidate route_cost_") ||
		!strings.Contains(planWithDebtOut, "- warning: pending route observations require owner review before routing policy changes") {
		t.Fatalf("route plan missing pending observation debt:\n%s", planWithDebtOut)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("route model capability import/plan must not promote claims, stat err=%v", err)
	}
}

func TestRouteEvidenceAuthorityCommandShowsProviderLedgerRows(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	fixture := filepath.Join("..", "..", "fixtures", "route", "provider-calls.ndjson")
	captureStdout(t, func() {
		cmdRoute([]string{"import-provider-ledger", fixture, "--tail", "2"})
	})
	out := captureStdout(t, func() {
		cmdRoute([]string{"evidence-authority", "--json", "--recent", "1"})
	})
	var report routeevidence.AuthorityReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse route evidence authority: %v\n%s", err, out)
	}
	if report.SchemaVersion != routeevidence.RouteEvidenceAuthorityReportSchema ||
		report.AuthorityRef != routeevidence.RouteEvidenceAuthorityRef ||
		report.Status != "ok" ||
		report.RouteEvidenceCount != 2 ||
		report.ProviderLedgerEvidenceCount != 2 ||
		len(report.Recent) != 1 {
		t.Fatalf("bad route evidence authority report: %+v", report)
	}
	if report.Recent[0].ID == "" || report.Recent[0].Provider == "" {
		t.Fatalf("recent route summary missing id/provider: %+v", report.Recent[0])
	}
}

func TestCapabilityToolBoundaryDoctorJSON(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	captureStdout(t, func() {
		cmdCapability([]string{"import-topology", "--mcporter-json", filepath.Join("..", "..", "fixtures", "tooltopology", "mcporter-list.json")})
	})
	captureStdout(t, func() {
		cmdCapability([]string{"import-skills", "--os-plugin-board-json", filepath.Join("..", "..", "fixtures", "skills", "os-plugin-board.json")})
	})
	out := captureStdout(t, func() {
		cmdCapability([]string{"tool-boundary-doctor", "--json"})
	})
	var report tooltopology.BoundaryDoctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("tool-boundary-doctor --json should parse: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.MCPCount == 0 || report.ToolCount == 0 || report.PluginAdapterCount == 0 || report.PluginSurfaceCount != 0 {
		t.Fatalf("bad boundary doctor report: %+v", report)
	}
}

func TestCapabilityExternalIntakeReportJSON(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	batchDir := filepath.Join(homePath, "inbox", "external-repo-intake", "2026-07-01-test")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	if err := os.WriteFile(mapPath, []byte(`{
  "boundaries": ["review-only", "no install"],
  "items": [
    {
      "repo": "example/research",
      "owner": "deep-research",
      "next_review_step": "run freshness eval",
      "not_to_do": "do not auto-run broad research"
    }
  ]
}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summaryPath := filepath.Join(batchDir, "run-summary.json")
	sourceObservationPath := filepath.Join(batchDir, "source-observation.json")
	if err := os.WriteFile(summaryPath, []byte(`{
  "schema_version": "compound.external_repo_intake_run.v0.1",
  "id": "2026-07-01-test",
  "created_at": "2026-07-01T12:00:00Z",
  "map_path": "`+filepath.ToSlash(mapPath)+`",
  "source_observation_path": "`+filepath.ToSlash(sourceObservationPath)+`",
  "candidate_count": 1,
  "source_observe_output": [
    "# source observation srcobs_test",
    "- source_candidate: src_cand_test",
    "- source_no_promote: nop_test"
  ],
  "counts_by_decision": {"keep_needs_review": 1},
  "counts_by_lane": {"freshness_research_skill_candidate": 1},
  "decisions": [
    {
      "repo": "example/research",
      "decision": "keep_needs_review",
      "lane": "freshness_research_skill_candidate",
      "status": "recorded",
      "evidence_strength": 0.91,
      "duplicate_risk": 0.1,
      "review_id": "scr_test"
    }
  ]
}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	jsonOut := captureStdout(t, func() {
		cmdCapability([]string{"external-intake", "--json"})
	})
	var report capgraph.ExternalRepoIntakeReport
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("external-intake --json should parse: %v\n%s", err, jsonOut)
	}
	if report.Status != "ok" || report.CandidateCount != 1 || report.OpenReviewCount != 1 {
		t.Fatalf("bad report: %+v", report)
	}
	if report.SourceObservationID != "srcobs_test" || len(report.NoPromoteIDs) != 1 || report.NoPromoteIDs[0] != "nop_test" {
		t.Fatalf("source evidence not surfaced: %+v", report)
	}
	if len(report.HighValue) != 1 || report.HighValue[0].Owner != "deep-research" || report.HighValue[0].NextReviewStep != "run freshness eval" {
		t.Fatalf("high-value metadata missing: %+v", report.HighValue)
	}
	defaultHuman := captureStdout(t, func() {
		cmdCapability([]string{"external-intake"})
	})
	for _, want := range []string{
		"- keep_needs_review_decision: 1",
		"- open_owner_reviews: 1",
		"- high_value: example/research decision=keep_needs_review lane=freshness_research_skill_candidate owner_review=pending capability_evaluation=blocked_on_owner_review graph=blocked_on_owner_review owner=deep-research",
	} {
		if !strings.Contains(defaultHuman, want) {
			t.Fatalf("default external-intake text missing %q:\n%s", want, defaultHuman)
		}
	}
	filteredJSON := captureStdout(t, func() {
		cmdCapability([]string{"external-intake", "--repo", "github.com/example/research", "--json"})
	})
	var filtered capgraph.ExternalRepoIntakeReport
	if err := json.Unmarshal([]byte(filteredJSON), &filtered); err != nil {
		t.Fatalf("external-intake --repo --json should parse: %v\n%s", err, filteredJSON)
	}
	if filtered.RepoQuery != "github.com/example/research" || len(filtered.Matches) != 1 || filtered.Matches[0].Repo != "example/research" {
		t.Fatalf("filtered repo report missing match: %+v", filtered)
	}
	humanOut := captureStdout(t, func() {
		cmdCapability([]string{"external-intake", "--repo", "research"})
	})
	if !strings.Contains(humanOut, "- repo_query: research matches=1") || !strings.Contains(humanOut, "- repo_match: example/research decision=keep_needs_review") {
		t.Fatalf("human repo lookup missing match:\n%s", humanOut)
	}
	sourceListPath := filepath.Join(homePath, "repos.md")
	if err := os.WriteFile(sourceListPath, []byte("https://github.com/example/research\nhttps://github.com/example/unknown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceListJSON := captureStdout(t, func() {
		cmdCapability([]string{"external-intake", "--source-list", sourceListPath, "--json"})
	})
	var sourceListReport capgraph.ExternalRepoIntakeReport
	if err := json.Unmarshal([]byte(sourceListJSON), &sourceListReport); err != nil {
		t.Fatalf("external-intake --source-list --json parse: %v\n%s", err, sourceListJSON)
	}
	if sourceListReport.SourceListTotal != 2 || sourceListReport.SourceListKnown != 1 || sourceListReport.SourceListUnknown != 1 {
		t.Fatalf("source-list coverage missing counts: %+v", sourceListReport)
	}
	if sourceListReport.SourceListByDecision["keep_needs_review"] != 1 || sourceListReport.SourceListByLane["freshness_research_skill_candidate"] != 1 || sourceListReport.SourceListByOwner["deep-research"] != 1 {
		t.Fatalf("source-list aggregate counts missing: decision=%+v lane=%+v owner=%+v", sourceListReport.SourceListByDecision, sourceListReport.SourceListByLane, sourceListReport.SourceListByOwner)
	}
	if sourceListReport.SourceListByOwnerReviewStatus["pending"] != 1 || sourceListReport.SourceListByCapabilityEvaluationStatus["blocked_on_owner_review"] != 1 {
		t.Fatalf("source-list status counts missing: owner_review=%+v capability_eval=%+v", sourceListReport.SourceListByOwnerReviewStatus, sourceListReport.SourceListByCapabilityEvaluationStatus)
	}
	if len(sourceListReport.SourceListReviewNext) != 1 || sourceListReport.SourceListReviewNext[0].Matches[0].Repo != "example/research" {
		t.Fatalf("source-list review_next missing match: %+v", sourceListReport.SourceListReviewNext)
	}
	if sourceListReport.SourceListReviewNextCount == nil || *sourceListReport.SourceListReviewNextCount != 1 || sourceListReport.SourceListEvalNextCount == nil || *sourceListReport.SourceListEvalNextCount != 0 {
		t.Fatalf("source-list follow-up counts missing: review=%v eval=%v", sourceListReport.SourceListReviewNextCount, sourceListReport.SourceListEvalNextCount)
	}
	if sourceListReport.SourceListReviewNextMeaning != "owner_review_suggestions_not_open_review_debt" || sourceListReport.SourceListEvalNextMeaning != "focused_capability_evaluation_follow_up_queue" {
		t.Fatalf("source-list follow-up meanings missing: review=%q eval=%q", sourceListReport.SourceListReviewNextMeaning, sourceListReport.SourceListEvalNextMeaning)
	}
	if sourceListReport.SourceListItems[0].Status != "known" || sourceListReport.SourceListItems[0].Matches[0].Repo != "example/research" {
		t.Fatalf("source-list known item missing match: %+v", sourceListReport.SourceListItems)
	}
	sourceListText := captureStdout(t, func() {
		cmdCapability([]string{"external-intake", "--source-list", sourceListPath})
	})
	for _, want := range []string{
		"- source_list_owner_review.pending: 1",
		"- source_list_capability_evaluation.blocked_on_owner_review: 1",
		"- source_list_review_next: example/research",
		"- source_list_known_omitted: 1 details=--json",
	} {
		if !strings.Contains(sourceListText, want) {
			t.Fatalf("source-list text missing %q:\n%s", want, sourceListText)
		}
	}
	if strings.Contains(sourceListText, "- source_list_known:") {
		t.Fatalf("source-list text should not dump known rows by default:\n%s", sourceListText)
	}
	reviewPlanJSON := captureStdout(t, func() {
		cmdCapability([]string{"external-intake-review-plan", "--source-list", sourceListPath, "--json"})
	})
	var reviewPlan capgraph.ExternalRepoIntakeReviewPlanReport
	if err := json.Unmarshal([]byte(reviewPlanJSON), &reviewPlan); err != nil {
		t.Fatalf("external-intake-review-plan --json parse: %v\n%s", err, reviewPlanJSON)
	}
	if reviewPlan.Status != "ok" || reviewPlan.WroteCount != 1 || len(reviewPlan.Items) != 1 || reviewPlan.Items[0].Repo != "example/research" {
		t.Fatalf("external-intake-review-plan report missing artifact: %+v", reviewPlan)
	}
	if _, err := os.Stat(reviewPlan.Items[0].JSONPath); err != nil {
		t.Fatalf("review artifact JSON not written: %v", err)
	}
	if reviewPlan.Items[0].ReviewYAMLPath == "" {
		t.Fatalf("review YAML not written: %+v", reviewPlan.Items[0])
	}
	reviewDraft, err := os.ReadFile(reviewPlan.Items[0].ReviewYAMLPath)
	if err != nil {
		t.Fatalf("read review YAML: %v", err)
	}
	editedReview := strings.ReplaceAll(string(reviewDraft), `rationale: ""`, `rationale: "reviewed source-list repo against current Compound owners"`)
	if err := os.WriteFile(reviewPlan.Items[0].ReviewYAMLPath, []byte(editedReview), 0o644); err != nil {
		t.Fatalf("write edited review YAML: %v", err)
	}
	reviewJSON := captureStdout(t, func() {
		cmdCapability([]string{"external-intake-review", reviewPlan.Items[0].ReviewYAMLPath, "--json"})
	})
	var reviewReport capgraph.ExternalRepoIntakeReviewReport
	if err := json.Unmarshal([]byte(reviewJSON), &reviewReport); err != nil {
		t.Fatalf("external-intake-review --json parse: %v\n%s", err, reviewJSON)
	}
	if reviewReport.Status != "recorded" || reviewReport.Repo != "example/research" || reviewReport.Decision != "keep_needs_review" {
		t.Fatalf("external-intake-review report missing recorded review: %+v", reviewReport)
	}
	if reviewReport.Rationale != "reviewed source-list repo against current Compound owners" || reviewReport.SourceHash == "" || reviewReport.SourceRef == "" || reviewReport.ReviewedAt == "" || len(reviewReport.Boundaries) == 0 {
		t.Fatalf("external-intake-review report should preserve reviewed evidence: %+v", reviewReport)
	}
	reviewsJSON := captureStdout(t, func() {
		cmdReviews([]string{"--area", "external-repo-intake", "--json"})
	})
	var reviews reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(reviewsJSON), &reviews); err != nil {
		t.Fatalf("reviews external-repo-intake parse: %v\n%s", err, reviewsJSON)
	}
	if reviews.Counts.Total != 1 || reviews.Counts.ReviewRequired != 0 || reviews.Counts.OK != 1 || reviews.Items[0].Status != "recorded" || reviews.Items[0].Readiness != "review_recorded" {
		t.Fatalf("external repo review should close queue item: counts=%+v item=%+v", reviews.Counts, reviews.Items[0])
	}
	humanDefaultAfterReview := captureStdout(t, func() {
		cmdCapability([]string{"external-intake"})
	})
	for _, want := range []string{
		"- keep_needs_review_decision: 1",
		"- open_owner_reviews: 0",
		"- high_value: example/research decision=keep_needs_review lane=freshness_research_skill_candidate owner_review=recorded capability_evaluation=pending graph=blocked_on_capability_evaluation owner=deep-research",
	} {
		if !strings.Contains(humanDefaultAfterReview, want) {
			t.Fatalf("default external-intake text should distinguish raw decision count from open owner-review debt; missing %q:\n%s", want, humanDefaultAfterReview)
		}
	}
	afterReviewJSON := captureStdout(t, func() {
		cmdCapability([]string{"external-intake", "--source-list", sourceListPath, "--json"})
	})
	var afterReview capgraph.ExternalRepoIntakeReport
	if err := json.Unmarshal([]byte(afterReviewJSON), &afterReview); err != nil {
		t.Fatalf("external-intake after review --json parse: %v\n%s", err, afterReviewJSON)
	}
	if len(afterReview.SourceListReviewNext) != 0 || len(afterReview.SourceListEvalNext) != 1 {
		t.Fatalf("recorded owner review should move source-list item from review-next to evaluation-next: %+v", afterReview)
	}
	if afterReview.SourceListReviewNextCount == nil || *afterReview.SourceListReviewNextCount != 0 || afterReview.SourceListEvalNextCount == nil || *afterReview.SourceListEvalNextCount != 1 {
		t.Fatalf("recorded owner review follow-up counts missing: review=%v eval=%v", afterReview.SourceListReviewNextCount, afterReview.SourceListEvalNextCount)
	}
	evalMatch := afterReview.SourceListEvalNext[0].Matches[0]
	if evalMatch.OwnerReviewStatus != "recorded" || evalMatch.OwnerReviewPath == "" || evalMatch.OwnerReviewNextAction == "" {
		t.Fatalf("evaluation follow-up missing recorded review context: %+v", evalMatch)
	}
	humanAfterReview := captureStdout(t, func() {
		cmdCapability([]string{"external-intake", "--source-list", sourceListPath})
	})
	if !strings.Contains(humanAfterReview, "- source_list_follow_up: review_next=0 meaning=owner_review_suggestions_not_open_review_debt evaluation_next=1 meaning=focused_capability_evaluation_follow_up_queue") {
		t.Fatalf("human source-list output should label follow-up meanings:\n%s", humanAfterReview)
	}
	if !strings.Contains(humanAfterReview, "- source_list_evaluation_next: example/research") || strings.Contains(humanAfterReview, "- source_list_review_next: example/research") {
		t.Fatalf("human source-list output should show evaluation-next only after review:\n%s", humanAfterReview)
	}
	evalPlanJSON := captureStdout(t, func() {
		cmdCapability([]string{"external-intake-evaluation-plan", "--source-list", sourceListPath, "--repo", "github.com/example/research", "--json"})
	})
	var evalPlan capgraph.ExternalRepoCapabilityEvaluationPlanReport
	if err := json.Unmarshal([]byte(evalPlanJSON), &evalPlan); err != nil {
		t.Fatalf("external-intake-evaluation-plan --json parse: %v\n%s", err, evalPlanJSON)
	}
	if evalPlan.Status != "review_required" || evalPlan.Repo != "example/research" || evalPlan.OwnerReviewStatus != "recorded" {
		t.Fatalf("external-intake-evaluation-plan report = %+v", evalPlan)
	}
	if evalPlan.EvaluationPath == "" || evalPlan.PlanPath == "" || evalPlan.ReviewYAMLPath == "" || evalPlan.ManifestDraftPath == "" {
		t.Fatalf("external-intake-evaluation-plan paths missing: %+v", evalPlan)
	}
	if _, err := os.Stat(filepath.Join(homePath, "registries", "capability-nodes.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("external-intake-evaluation-plan must not mutate capability graph, stat err=%v", err)
	}
	evalReviewDraft, err := os.ReadFile(evalPlan.ReviewYAMLPath)
	if err != nil {
		t.Fatalf("read evaluation review YAML: %v", err)
	}
	editedEvalReview := strings.ReplaceAll(string(evalReviewDraft), "decision: TODO_activate_reject_or_keep_needs_review", "decision: keep_needs_review")
	editedEvalReview = strings.ReplaceAll(editedEvalReview, `rationale: ""`, `rationale: "reviewed external repo evaluation; keep manifest parked for explicit graph import"`)
	editedEvalReview = strings.ReplaceAll(editedEvalReview, "confidence: -1", "confidence: 0.82")
	if err := os.WriteFile(evalPlan.ReviewYAMLPath, []byte(editedEvalReview), 0o644); err != nil {
		t.Fatalf("write evaluation review YAML: %v", err)
	}
	evalReviewText := captureStdout(t, func() {
		cmdCapability([]string{"evaluation-review", evalPlan.ReviewYAMLPath})
	})
	if !strings.Contains(evalReviewText, "- status: manifest_ready") || !strings.Contains(evalReviewText, "- node_status: needs_review") {
		t.Fatalf("evaluation-review text missing reviewed manifest status:\n%s", evalReviewText)
	}
	humanAfterEvalReview := captureStdout(t, func() {
		cmdCapability([]string{"external-intake", "--source-list", sourceListPath})
	})
	for _, want := range []string{
		"- source_list_capability_graph.reviewed_manifest_pending_import: 1",
		"- source_list_graph_import_follow_up: graph_import_next=1 omitted=0 meaning=reviewed_manifest_import_queue_not_graph_mutation",
		"- source_list_graph_import_next: example/research",
	} {
		if !strings.Contains(humanAfterEvalReview, want) {
			t.Fatalf("human source-list output missing graph-import follow-up %q:\n%s", want, humanAfterEvalReview)
		}
	}
	importJSON := captureStdout(t, func() {
		cmdCapability([]string{"external-intake-import-reviewed", "--source-list", sourceListPath, "--json"})
	})
	var importReport capgraph.ExternalRepoReviewedManifestImportReport
	if err := json.Unmarshal([]byte(importJSON), &importReport); err != nil {
		t.Fatalf("external-intake-import-reviewed --json parse: %v\n%s", err, importJSON)
	}
	if importReport.Status != "ok" || importReport.PendingImportCount != 1 || importReport.ImportedCount != 1 || len(importReport.Items) != 1 {
		t.Fatalf("external-intake-import-reviewed report = %+v", importReport)
	}
	afterImportJSON := captureStdout(t, func() {
		cmdCapability([]string{"external-intake", "--source-list", sourceListPath, "--json"})
	})
	var afterImport capgraph.ExternalRepoIntakeReport
	if err := json.Unmarshal([]byte(afterImportJSON), &afterImport); err != nil {
		t.Fatalf("external-intake after graph import --json parse: %v\n%s", err, afterImportJSON)
	}
	if afterImport.SourceListByCapabilityGraphStatus["imported"] != 1 || len(afterImport.SourceListGraphImportNext) != 0 {
		t.Fatalf("graph import should clear follow-up queue: status=%+v next=%+v", afterImport.SourceListByCapabilityGraphStatus, afterImport.SourceListGraphImportNext)
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatalf("open graph after import: %v", err)
	}
	hits, err := store.Surface("external repo example review workflow")
	if err != nil {
		t.Fatalf("surface after import: %v", err)
	}
	for _, hit := range hits {
		if hit.Node.ID == importReport.Items[0].NodeID {
			t.Fatalf("needs_review imported node should not surface: %+v", hits)
		}
	}
}

func writeJSONFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMemoryCandidatesJSONCommandListsWithoutEvidenceByDefault(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := store.RecordEvidence(mem.EvidenceItem{SourceKind: "chatgpt", SourceID: "conversation:cli", QuoteText: "candidate came from a bounded conversation quote"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := store.Capture([]mem.Candidate{{
		Entity:         "entity:cli",
		Predicate:      "status",
		Object:         "current promoted belief",
		Confidence:     0.91,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		PromotionBasis: candidates.BasisTrustedDeterministic,
		SourceTraceRef: ev.ID,
		SnippetHash:    ev.ContentHash,
		Visibility:     "operational",
	}}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := store.Adjudicate(prior[0].ID, "promote", "", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	// ClaimType decision keeps this on the operator_required (proposed) path.
	captured, err := store.Capture([]mem.Candidate{{
		Entity:         "entity:cli",
		Predicate:      "status",
		Object:         "listed",
		Confidence:     0.9,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		PromotionBasis: candidates.BasisTrustedDeterministic,
		SourceTraceRef: ev.ID,
		SnippetHash:    ev.ContentHash,
		Visibility:     "operational",
	}}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdMemory([]string{"candidates", "--json", "--state", "proposed", "--limit", "1"})
	})
	var report mem.CandidateListReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse candidates json: %v\n%s", err, out)
	}
	if report.SchemaVersion != mem.CandidateListSchemaVersion || report.Counts.Matched != 1 || report.Items[0].ID != captured[0].ID {
		t.Fatalf("candidate list report = %+v", report)
	}
	if report.Items[0].SourceTraceRef != "" || len(report.Items[0].EvidenceRefs) != 0 || report.Items[0].Evidence != nil || report.Items[0].BelievedBefore != nil {
		t.Fatalf("evidence leaked by default: %+v", report.Items[0])
	}

	out = captureStdout(t, func() {
		cmdMemory([]string{"candidates", "--json", "--state", "proposed", "--include-evidence"})
	})
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse candidates include-evidence json: %v\n%s", err, out)
	}
	if report.Items[0].SourceTraceRef != ev.ID || !stringSliceContains(report.Items[0].EvidenceRefs, ev.ID) {
		t.Fatalf("include evidence report = %+v", report.Items[0])
	}
	if report.Items[0].Evidence == nil || report.Items[0].Evidence.Quote != "candidate came from a bounded conversation quote" || report.Items[0].Evidence.SourceID != "conversation:cli" {
		t.Fatalf("include evidence card context = %+v", report.Items[0].Evidence)
	}
	if report.Items[0].BelievedBefore == nil || report.Items[0].BelievedBefore.ClaimID != promoted.Claim.ID || report.Items[0].BelievedBefore.Object != "current promoted belief" {
		t.Fatalf("include believed_before context = %+v", report.Items[0].BelievedBefore)
	}
}

func TestWorkbenchJSONIncludesOfferedPacketsFromPacketDir(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	packetDir := filepath.Join(t.TempDir(), "packets")
	if err := os.MkdirAll(packetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	packet := `# CLI packet
Date: 2026-07-03
Status: ready
Owner: codex
Success: packet is listed.
`
	if err := os.WriteFile(filepath.Join(packetDir, "cli-packet.md"), []byte(packet), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--packet-dir", packetDir})
	})
	var report workbenchCLIOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse workbench json: %v\n%s", err, out)
	}
	if report.OfferedPackets == nil || report.OfferedPackets.Counts.Listed != 1 || report.OfferedPackets.Items[0].Title != "CLI packet" {
		t.Fatalf("offered packets missing from workbench: %+v", report.OfferedPackets)
	}
}

func TestWorkbenchJSONSurfacesAdjacentProgressWhenHeldValvesBlockDispatch(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	held, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "held operator valve", State: "held"}, cliTestNow)
	if err != nil {
		t.Fatalf("create held work: %v", err)
	}
	packetDir := filepath.Join(t.TempDir(), "packets")
	if err := os.MkdirAll(packetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	packet := `# Adjacent CLI packet
Date: 2026-07-04
Status: ready
Owner: codex
Success: adjacent work remains available while held valve stays visible.
`
	if err := os.WriteFile(filepath.Join(packetDir, "adjacent-cli.md"), []byte(packet), 0o644); err != nil {
		t.Fatal(err)
	}
	consolePacket := `# Console CLI packet
Date: 2026-07-04
Status: ready - CONSOLE LANE'S CLAIM
Owner: claude-console-lane
Success: console lane work should not be a Codex adjacent packet.
`
	if err := os.WriteFile(filepath.Join(packetDir, "console-cli.md"), []byte(consolePacket), 0o644); err != nil {
		t.Fatal(err)
	}
	operatorPacket := `# Operator CLI packet
Date: 2026-07-04
Status: ready (operator registration flip remaining)
Owner: codex + operator
Success: operator-only work should not be a Codex adjacent packet.
`
	if err := os.WriteFile(filepath.Join(packetDir, "operator-cli.md"), []byte(operatorPacket), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--packet-dir", packetDir})
	})
	var report workbenchCLIOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse workbench json: %v\n%s", err, out)
	}
	next := report.DispatchNext
	if next.CanRun || next.WaitReason != "needs_review" || !stringSliceContains(next.BlockedOn, "work.review:"+held.ID) {
		t.Fatalf("dispatch next should keep held valve blocking direct dispatch: %+v", next)
	}
	if next.AdjacentProgress == nil || !next.AdjacentProgress.Available || next.AdjacentProgress.PacketCandidateCount != 1 {
		t.Fatalf("dispatch next missing adjacent progress: %+v", next)
	}
	if !stringSliceContains(next.AdjacentProgress.HeldWorkIDs, held.ID) || !stringSliceContains(next.AdjacentProgress.PacketRefs, "adjacent-cli.md") {
		t.Fatalf("adjacent progress context = %+v", next.AdjacentProgress)
	}
	for _, leaked := range []string{"console-cli.md", "operator-cli.md"} {
		if stringSliceContains(next.AdjacentProgress.PacketRefs, leaked) {
			t.Fatalf("adjacent progress leaked non-actionable packet %s: %+v", leaked, next.AdjacentProgress)
		}
	}

	textOut := captureStdout(t, func() {
		cmdWorkbench([]string{"--packet-dir", packetDir})
	})
	if !strings.Contains(textOut, "dispatch_wait: needs_review") || !strings.Contains(textOut, "adjacent_progress: available=true") || !strings.Contains(textOut, "adjacent_next:") {
		t.Fatalf("workbench text output missing adjacent progress:\n%s", textOut)
	}
}

func TestWorkbenchJSONSurfacesAdjacentNonPacketActionsWhenPacketsFilteredOut(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	asc, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held app-store-connect ASC private config",
		State:            "held",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create asc held work: %v", err)
	}
	conversation, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "held chatgpt conversation import promotion",
		State:            "held",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create conversation held work: %v", err)
	}
	runDir := filepath.Join(homePath, "runs", conversation.ID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidateDir := filepath.Join(homePath, "inbox", "memory-candidates")
	if err := os.MkdirAll(candidateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIJSONFixture(t, filepath.Join(candidateDir, "cand_cli_conversation_review.json"), map[string]any{
		"id":     "cand_cli_conversation_review",
		"state":  "parked",
		"entity": "knowledge:chatgpt:cli-fixture",
	})
	writeCLIJSONFixture(t, filepath.Join(runDir, "panel-run-single-prun_cli_fixture.json"), map[string]any{
		"schema_version": "compound.panel_run.v0.1",
		"id":             "prun_cli_fixture",
		"status":         "applied",
		"target_kind":    "memory_candidate",
		"targets": []map[string]any{{
			"ref": "cand_cli_conversation_review",
		}},
	})
	writeCLIJSONFixture(t, filepath.Join(runDir, "promoted-chatgpt-claims.json"), map[string]any{
		"count": 0,
		"refs":  []string{},
	})
	packetDir := filepath.Join(t.TempDir(), "packets")
	if err := os.MkdirAll(packetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	consolePacket := `# Console CLI packet
Date: 2026-07-04
Status: ready - CONSOLE LANE'S CLAIM
Owner: claude-console-lane
Success: console lane work should not be Codex adjacent packet.
`
	if err := os.WriteFile(filepath.Join(packetDir, "console-cli.md"), []byte(consolePacket), 0o644); err != nil {
		t.Fatal(err)
	}
	operatorPacket := `# Operator CLI packet
Date: 2026-07-04
Status: ready (operator registration flip remaining)
Owner: codex + operator
Success: operator-only work should not be Codex adjacent packet.
`
	if err := os.WriteFile(filepath.Join(packetDir, "operator-cli.md"), []byte(operatorPacket), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdWorkbench([]string{"--json", "--packet-dir", packetDir})
	})
	var report workbenchCLIOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse workbench json: %v\n%s", err, out)
	}
	if report.Counts.RunArtifacts != 2 {
		t.Fatalf("workbench JSON should count run artifacts: counts=%+v", report.Counts)
	}
	var conversationItem *work.WorkbenchItem
	for i := range report.Open {
		if report.Open[i].WorkID == conversation.ID {
			conversationItem = &report.Open[i]
			break
		}
	}
	if conversationItem == nil ||
		len(conversationItem.RunArtifacts) != 2 ||
		conversationItem.RunArtifacts[0].Ref != filepath.ToSlash(filepath.Join("runs", conversation.ID, "panel-run-single-prun_cli_fixture.json")) ||
		conversationItem.RunArtifacts[0].Kind != "panel_run" ||
		conversationItem.RunArtifacts[0].Status != "applied" ||
		!stringSliceContains(conversationItem.RunArtifacts[0].TargetRefs, "cand_cli_conversation_review") ||
		!stringSliceContains(conversationItem.MemoryCandidateIDs, "cand_cli_conversation_review") {
		t.Fatalf("workbench JSON should expose conversation run artifact without private path: %+v", conversationItem)
	}
	if len(conversationItem.ReviewCards) != 1 ||
		conversationItem.ReviewCards[0].CandidateID != "cand_cli_conversation_review" ||
		conversationItem.ReviewCards[0].CandidateState != "parked" ||
		conversationItem.ReviewCards[0].PanelRunID != "prun_cli_fixture" ||
		conversationItem.ReviewCards[0].PromotionCount == nil ||
		*conversationItem.ReviewCards[0].PromotionCount != 0 {
		t.Fatalf("workbench JSON should expose exact conversation review card: %+v", conversationItem.ReviewCards)
	}
	adjacent := report.DispatchNext.AdjacentProgress
	if adjacent == nil || !adjacent.Available || adjacent.PacketCandidateCount != 0 {
		t.Fatalf("workbench should expose non-packet adjacent actions: %+v", report.DispatchNext)
	}
	for _, id := range []string{conversationItem.ReviewCards[0].ID, "operator-gate-" + asc.ID + "-app-store-connect-private-config", "asc-sales-series-bridge-proof", "conversation-preflight-private-export", "second-opinion-evidence-recovery", "codex-pretooluse-native-gate-proof", "architecture-doctor-strict"} {
		if _, ok := workbenchAdjacentActionByID(adjacent.NonPacketActions, id); !ok {
			t.Fatalf("missing adjacent non-packet action %s: %+v", id, adjacent.NonPacketActions)
		}
	}
	ascBridge, _ := workbenchAdjacentActionByID(adjacent.NonPacketActions, "asc-sales-series-bridge-proof")
	ascBridgeCommandText := "action_command: " + strings.Join(ascBridge.Command, " ")
	if ascBridgeCommandText != "action_command: bun test tools/asc_sales_series_bridge.test.ts" ||
		!stringSliceContains(ascBridge.ExpectedValidation, "apply_results shows signal_count and transition_count") {
		t.Fatalf("ASC bridge proof action should remain available as JSON action even when text preview is capped: %+v", ascBridge)
	}
	reviewCardAction, _ := workbenchAdjacentActionByID(adjacent.NonPacketActions, conversationItem.ReviewCards[0].ID)
	if !reviewCardAction.RequiresOperator ||
		reviewCardAction.ReviewCard == nil ||
		reviewCardAction.ReviewCard.CandidateID != "cand_cli_conversation_review" ||
		!stringSliceContains(reviewCardAction.StopConditions, "requested action would run import, extraction, panel run, mass import, or automatic promotion") {
		t.Fatalf("adjacent review-card action should carry exact candidate boundary: %+v", reviewCardAction)
	}
	if _, ok := workbenchAdjacentActionByID(adjacent.NonPacketActions, "conversation-memory-review-gate"); ok {
		t.Fatalf("generic conversation review gate should be suppressed by exact review card: %+v", adjacent.NonPacketActions)
	}
	if _, ok := workbenchAdjacentActionByID(adjacent.NonPacketActions, "asc-preflight-private-config"); ok {
		t.Fatalf("generic ASC preflight should be suppressed by exact review card: %+v", adjacent.NonPacketActions)
	}
	oracleAction, _ := workbenchAdjacentActionByID(adjacent.NonPacketActions, "second-opinion-evidence-recovery")
	if !oracleAction.RequiresOperator ||
		!strings.Contains(strings.Join(oracleAction.Command, " "), "tools/second_opinion_preflight.ts") ||
		!strings.Contains(strings.Join(oracleAction.Command, " "), "--source-observation") ||
		!stringSliceContains(oracleAction.RequiredInputs, "private source-observation YAML path") ||
		!stringSliceContains(oracleAction.StopConditions, "source observation would promote truth") ||
		!stringSliceContains(oracleAction.ExpectedValidation, "no source observe import during preflight") {
		t.Fatalf("second-opinion action should expose preflight before source-observe path: %+v", oracleAction)
	}
	for _, ref := range adjacent.PacketRefs {
		if strings.Contains(ref, "console-cli.md") || strings.Contains(ref, "operator-cli.md") {
			t.Fatalf("filtered packet leaked into adjacent refs: %+v", adjacent.PacketRefs)
		}
	}
	textOut := captureStdout(t, func() {
		cmdWorkbench([]string{"--packet-dir", packetDir})
	})
	if !strings.Contains(textOut, "adjacent_progress: available=true packet_candidates=0") ||
		!strings.Contains(textOut, "run_artifacts=2") ||
		!strings.Contains(textOut, "run_artifact: "+filepath.ToSlash(filepath.Join("runs", conversation.ID, "panel-run-single-prun_cli_fixture.json"))+" kind=panel_run status=applied target=cand_cli_conversation_review") ||
		!strings.Contains(textOut, "adjacent_non_packet_action: "+conversationItem.ReviewCards[0].ID+" kind=review_card") ||
		!strings.Contains(textOut, "action_command: compound panel show prun_cli_fixture --json") ||
		!strings.Contains(textOut, "promotion summary remains zero before explicit promotion") ||
		!strings.Contains(textOut, "adjacent_non_packet_action: operator-gate-") ||
		!strings.Contains(textOut, "kind=review_card") ||
		!strings.Contains(textOut, "operator private ASC config needed") ||
		!strings.Contains(textOut, "action_command: bun tools/asc_preflight.ts --config <private-asc-config.json> --json") ||
		!strings.Contains(textOut, "action_required_inputs: private ASC product mapping config") ||
		!strings.Contains(textOut, "action_expected_validation: asc preflight returns status ok or warn") ||
		!strings.Contains(textOut, "action_stop_conditions: private config missing or invalid") {
		t.Fatalf("workbench text output missing adjacent non-packet actions:\n%s", textOut)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	return string(readTestPath(t, path))
}

func createReleaseGoalHoldsDB(t *testing.T, dbPath string) {
	t.Helper()
	stmt := `
create table goals (
  goal_id text primary key,
  app_id text not null,
  objective text not null,
  state text not null,
  artifact_dir text not null,
  created_at text not null,
  updated_at text not null
);
create table packets (
  packet_id text primary key,
  goal_id text not null,
  lane text not null,
  state text not null,
  packet_json text not null,
  updated_at text not null
);
create table events (
  id integer primary key autoincrement,
  goal_id text not null,
  event_type text not null,
  payload_json text not null,
  created_at text not null
);
insert into goals(goal_id, app_id, objective, state, artifact_dir, created_at, updated_at) values
  ('release-hold-fixture', 'phone-cleaner', 'Keep phone-cleaner release current', 'HOLD', '/tmp/release-hold-fixture', '2026-07-01T00:00:00', '2026-07-01T00:00:00'),
  ('release-hold-before-gate', 'old-app', 'Old hold', 'HOLD', '/tmp/old-hold', '2026-06-10T11:00:00', '2026-06-10T11:00:00'),
  ('release-approved-fixture', 'approved-app', 'Approved release', 'APPROVED', '/tmp/approved', '2026-07-01T00:00:00', '2026-07-01T00:00:00');
`
	cmd := exec.Command("sqlite3", dbPath)
	cmd.Stdin = strings.NewReader(stmt)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create release-goal sqlite fixture: %v\n%s", err, out)
	}
}

func TestCaptureStdoutDrainsLargeOutput(t *testing.T) {
	payload := strings.Repeat("x", 1<<20)
	got := captureStdout(t, func() {
		if _, err := io.WriteString(os.Stdout, payload); err != nil {
			t.Fatal(err)
		}
	})
	if got != payload {
		t.Fatalf("captured %d bytes, want %d", len(got), len(payload))
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	type readResult struct {
		out []byte
		err error
	}
	readDone := make(chan readResult, 1)
	go func() {
		out, err := io.ReadAll(r)
		readDone <- readResult{out: out, err: err}
	}()

	os.Stdout = w
	defer func() {
		os.Stdout = old
		_ = w.Close()
		_ = r.Close()
	}()
	fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-readDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	return string(result.out)
}

func sameStringSlice(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sameStringSlicePrefix(got, wantPrefix []string) bool {
	if len(got) < len(wantPrefix) {
		return false
	}
	for i := range wantPrefix {
		if got[i] != wantPrefix[i] {
			return false
		}
	}
	return true
}

func outputValue(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key) {
			return strings.TrimSpace(strings.TrimPrefix(line, key))
		}
	}
	return ""
}

func readCapabilityEdges(t *testing.T, homePath string) []capgraph.Edge {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(homePath, "registries", "capability-edges.jsonl"))
	if err != nil {
		t.Fatalf("read capability edges: %v", err)
	}
	var edges []capgraph.Edge
	for _, line := range splitJSONLines(string(b)) {
		var edge capgraph.Edge
		if err := json.Unmarshal([]byte(line), &edge); err != nil {
			t.Fatalf("parse edge %q: %v", line, err)
		}
		edges = append(edges, edge)
	}
	return edges
}

func readCandidateRows(t *testing.T, dir string) []distiller.Candidate {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read candidate dir: %v", err)
	}
	var out []distiller.Candidate
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read candidate %s: %v", entry.Name(), err)
		}
		var c distiller.Candidate
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatalf("parse candidate %s: %v", entry.Name(), err)
		}
		out = append(out, c)
	}
	return out
}

type noPromoteDecisionRow struct {
	CandidateRef string `json:"candidate_ref"`
	OwnerTarget  struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	} `json:"owner_target"`
}

func readNoPromoteDecisionRows(t *testing.T, homePath string) []noPromoteDecisionRow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(homePath, "ledger", "no-promote-decisions.jsonl"))
	if err != nil {
		t.Fatalf("read no-promote rows: %v", err)
	}
	var rows []noPromoteDecisionRow
	for _, line := range splitJSONLines(string(data)) {
		var row noPromoteDecisionRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse no-promote row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func splitJSONLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func workbenchAdjacentActionByID(actions []work.WorkbenchAdjacentAction, id string) (work.WorkbenchAdjacentAction, bool) {
	for _, action := range actions {
		if action.ID == id {
			return action, true
		}
	}
	return work.WorkbenchAdjacentAction{}, false
}

func findHookReadiness(rows []contract.InstalledHookRuntimeReadiness, harnessName, hookName string) *contract.InstalledHookRuntimeReadiness {
	for i := range rows {
		if rows[i].Harness == harnessName && rows[i].Hook == hookName {
			return &rows[i]
		}
	}
	return nil
}

func findHookAction(actions []contract.HookGateAction, kind string) *contract.HookGateAction {
	for i := range actions {
		if actions[i].Kind == kind {
			return &actions[i]
		}
	}
	return nil
}

func findHookGateReadiness(rows []contract.HookGateReadiness, harnessName string) *contract.HookGateReadiness {
	for i := range rows {
		if rows[i].Harness == harnessName && rows[i].Hook == "PreToolUse" {
			return &rows[i]
		}
	}
	return nil
}

func hasMatchedTokenCount(values []capgraph.MatchedTokenCount, token string, count int) bool {
	for _, value := range values {
		if value.Token == token && value.Count == count {
			return true
		}
	}
	return false
}

func sourceRevisionExists(revs []envelope.SourceRev, source string) bool {
	for _, rev := range revs {
		if rev.Source == source {
			return true
		}
	}
	return false
}

func hasCapabilityEdge(edges []capgraph.Edge, kind, from string) bool {
	for _, edge := range edges {
		if edge.Kind == kind && edge.From == from {
			return true
		}
	}
	return false
}

func hasCapabilityEdgeTo(edges []capgraph.Edge, kind, to string) bool {
	for _, edge := range edges {
		if edge.Kind == kind && edge.To == to {
			return true
		}
	}
	return false
}

func countCapabilityEdgeTo(edges []capgraph.Edge, kind, to string) int {
	count := 0
	for _, edge := range edges {
		if edge.Kind == kind && edge.To == to {
			count++
		}
	}
	return count
}

func initCompoundCommandGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runCommandGit(t, repo, "init", "-b", "main")
	runCommandGit(t, repo, "config", "user.email", "test@example.invalid")
	runCommandGit(t, repo, "config", "user.name", "Compound Test")
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module compound\n\ngo 1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCommandGit(t, repo, "add", "go.mod", "README.md")
	runCommandGit(t, repo, "commit", "-m", "initial")
	return repo
}

func runCommandGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func compoundCommandGitStatus(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain=v1", "-b")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status failed: %v\n%s", err, string(out))
	}
	return string(out)
}

func TestProductActionPlanAndReviewCLIExposeActionCardsThroughReviewQueue(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	seedCLIProductActionLoop(t, homePath, store)

	model, err := product.BuildLoopModel(homePath, product.LoopOptions{ProductID: "product:action-cli", Now: cliTestNow})
	if err != nil {
		t.Fatal(err)
	}
	card := findCLIActionCard(model.Products, "product_next_bounded_move")
	if card.ID == "" {
		t.Fatalf("missing action card: %+v", model.Products)
	}
	planOut := captureStdout(t, func() {
		cmdProduct([]string{"action-plan", "--product", "product:action-cli", "--card", card.ID})
	})
	if !strings.Contains(planOut, "# product action-plan") || !strings.Contains(planOut, "- card_kind: product_next_bounded_move") || !strings.Contains(planOut, "- review_yaml:") {
		t.Fatalf("action-plan output missing proof:\n%s", planOut)
	}

	reviewsOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", product.ProductActionReviewArea, "--json"})
	})
	var queue reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(reviewsOut), &queue); err != nil {
		t.Fatalf("parse action review queue: %v\n%s", err, reviewsOut)
	}
	if queue.Counts.ReviewRequired != 1 || queue.Counts.DecisionCardCount != 1 || queue.Counts.ActionableCount != 1 || len(queue.Items) != 1 {
		t.Fatalf("action review queue should expose one actionable decision card: %+v", queue)
	}
	reviewYAMLPath := filepath.Join(homePath, "reviews", product.ProductActionReviewArea, queue.Items[0].ID+".review.yaml")
	draft, err := os.ReadFile(reviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewYAML := strings.ReplaceAll(string(draft), "TODO_decision_propose_work_park_reject", "propose_work")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_rationale", "CLI reviewed action card before work admission")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_confidence_0_to_1", "0.74")
	reviewYAML = strings.ReplaceAll(reviewYAML, "TODO_reviewed_by_actor", "operator")
	if err := os.WriteFile(reviewYAMLPath, []byte(reviewYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	reviewOut := captureStdout(t, func() {
		cmdProduct([]string{"action-review", reviewYAMLPath})
	})
	if !strings.Contains(reviewOut, "# product action-review") || !strings.Contains(reviewOut, "- status: recorded") || !strings.Contains(reviewOut, "- decision: propose_work") {
		t.Fatalf("action-review output missing proof:\n%s", reviewOut)
	}
	reviewsOut = captureStdout(t, func() {
		cmdReviews([]string{"--area", product.ProductActionReviewArea, "--json"})
	})
	if err := json.Unmarshal([]byte(reviewsOut), &queue); err != nil {
		t.Fatalf("parse recorded action review queue: %v\n%s", err, reviewsOut)
	}
	if queue.Counts.ReviewRequired != 0 || queue.Counts.OK != 1 || queue.Items[0].Readiness != "no_action" {
		t.Fatalf("recorded action review should be terminal: %+v", queue)
	}
	reviewedPath := outputValue(reviewOut, "- reviewed:")
	if reviewedPath == "" {
		t.Fatalf("action-review output missing reviewed path:\n%s", reviewOut)
	}
	beforeTimeline, err := product.LoadTimeline(homePath, "product:action-cli")
	if err != nil {
		t.Fatal(err)
	}
	workPlanOut := captureStdout(t, func() {
		cmdProduct([]string{"action-work-plan", "--reviewed", reviewedPath, "--actor", "operator", "--json"})
	})
	var workPlanReport struct {
		Status        string                  `json:"status"`
		ProductAction product.ActionWorkDraft `json:"product_action"`
		WorkPlan      work.CreatePlan         `json:"work_plan"`
	}
	if err := json.Unmarshal([]byte(workPlanOut), &workPlanReport); err != nil {
		t.Fatalf("parse action-work-plan: %v\n%s", err, workPlanOut)
	}
	if workPlanReport.Status != "recorded" ||
		workPlanReport.WorkPlan.Status != "create_policy_allowed" ||
		workPlanReport.WorkPlan.WorkState != "proposed" ||
		workPlanReport.WorkPlan.WorkID == "" ||
		workPlanReport.WorkPlan.PlanPath == "" ||
		workPlanReport.ProductAction.WorkID != workPlanReport.WorkPlan.WorkID ||
		workPlanReport.ProductAction.ActionSummary == "" ||
		len(workPlanReport.ProductAction.ActionCommand) == 0 {
		t.Fatalf("action-work-plan did not preserve reviewed action as proposed work plan: %+v", workPlanReport)
	}
	if workPlanReport.WorkPlan.ActionContext == nil ||
		workPlanReport.WorkPlan.ActionContext.ActionSummary != workPlanReport.ProductAction.ActionSummary ||
		!sameStringSlice(workPlanReport.WorkPlan.ActionContext.Command, workPlanReport.ProductAction.ActionCommand) ||
		workPlanReport.WorkPlan.ActionContext.Boundary != workPlanReport.ProductAction.Boundary ||
		workPlanReport.WorkPlan.ActionContext.Proof != workPlanReport.ProductAction.Proof {
		t.Fatalf("action-work-plan did not preserve runnable action context: %+v", workPlanReport.WorkPlan.ActionContext)
	}
	if _, err := os.Stat(workPlanReport.WorkPlan.PlanPath); err != nil {
		t.Fatalf("recorded create-plan missing: %v", err)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workStore.Get(workPlanReport.WorkPlan.WorkID); err == nil {
		t.Fatalf("action-work-plan should not admit work %s", workPlanReport.WorkPlan.WorkID)
	}
	afterTimeline, err := product.LoadTimeline(homePath, "product:action-cli")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTimeline.Signals) != len(beforeTimeline.Signals) ||
		len(afterTimeline.Hypotheses) != len(beforeTimeline.Hypotheses) ||
		len(afterTimeline.Transitions) != len(beforeTimeline.Transitions) {
		t.Fatalf("action-work-plan should not mutate product timeline: before=%+v after=%+v", beforeTimeline, afterTimeline)
	}
	workbenchJSON := captureStdout(t, func() {
		cmdWorkbench([]string{"--json"})
	})
	var wb workbenchCLIOutput
	if err := json.Unmarshal([]byte(workbenchJSON), &wb); err != nil {
		t.Fatalf("parse workbench: %v\n%s", err, workbenchJSON)
	}
	if wb.Counts.AdmissionPreviews != 1 ||
		len(wb.AdmissionPreviews) != 1 ||
		wb.AdmissionPreviews[0].WorkID != workPlanReport.WorkPlan.WorkID ||
		wb.AdmissionPreviews[0].PlanPath == "" {
		t.Fatalf("workbench should expose admission preview: counts=%+v previews=%+v", wb.Counts, wb.AdmissionPreviews)
	}
	workbenchText := captureStdout(t, func() {
		cmdWorkbench(nil)
	})
	if !strings.Contains(workbenchText, "admission_previews=1") ||
		!strings.Contains(workbenchText, "admission_preview: "+workPlanReport.WorkPlan.WorkID) ||
		!strings.Contains(workbenchText, "staleness=fresh") ||
		!strings.Contains(workbenchText, "admit: compound work admit-plan --plan "+workPlanReport.WorkPlan.PlanPath) ||
		!strings.Contains(workbenchText, "action_context: "+strings.Join(workPlanReport.ProductAction.ActionCommand, " ")) {
		t.Fatalf("workbench text missing admission preview:\n%s", workbenchText)
	}
	admitOut := captureStdout(t, func() {
		cmdWork([]string{"admit-plan", "--plan", workPlanReport.WorkPlan.PlanPath, "--by", "operator-test", "--json"})
	})
	var admitReport work.AdmitCreatePlanReport
	if err := json.Unmarshal([]byte(admitOut), &admitReport); err != nil {
		t.Fatalf("parse admit-plan: %v\n%s", err, admitOut)
	}
	if admitReport.Status != "admitted" ||
		admitReport.PlanID != workPlanReport.WorkPlan.ID ||
		admitReport.WorkID != workPlanReport.WorkPlan.WorkID ||
		admitReport.WorkState != "proposed" ||
		admitReport.ActionIntentID != workPlanReport.WorkPlan.ActionIntent.ID ||
		admitReport.PolicyDecisionID != workPlanReport.WorkPlan.Decision.ID ||
		!admitReport.PolicyAllowed {
		t.Fatalf("admit-plan did not preserve create-plan proof chain: %+v", admitReport)
	}
	admitted, events, err := workStore.Show(workPlanReport.WorkPlan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.State != "proposed" ||
		admitted.PolicyDecisionID != workPlanReport.WorkPlan.Decision.ID ||
		len(admitted.ActionIntentIDs) != 1 ||
		admitted.ActionIntentIDs[0] != workPlanReport.WorkPlan.ActionIntent.ID ||
		len(events) != 1 ||
		events[0].Note != "admitted_from_create_plan" {
		t.Fatalf("admitted work not threaded through create-plan: work=%+v events=%+v", admitted, events)
	}
	workbenchAfterAdmit := captureStdout(t, func() {
		cmdWorkbench([]string{"--json"})
	})
	var wbAfter workbenchCLIOutput
	if err := json.Unmarshal([]byte(workbenchAfterAdmit), &wbAfter); err != nil {
		t.Fatalf("parse workbench after admit: %v\n%s", err, workbenchAfterAdmit)
	}
	if wbAfter.Counts.AdmissionPreviews != 0 || len(wbAfter.AdmissionPreviews) != 0 {
		t.Fatalf("admitted work should retire admission preview: counts=%+v previews=%+v", wbAfter.Counts, wbAfter.AdmissionPreviews)
	}
	var admittedItem *work.WorkbenchItem
	for i := range wbAfter.Open {
		if wbAfter.Open[i].WorkID == workPlanReport.WorkPlan.WorkID {
			admittedItem = &wbAfter.Open[i]
			break
		}
	}
	if admittedItem == nil ||
		admittedItem.ActionContext == nil ||
		admittedItem.ActionContext.ActionSummary != workPlanReport.ProductAction.ActionSummary ||
		!sameStringSlice(admittedItem.ActionContext.Command, workPlanReport.ProductAction.ActionCommand) ||
		admittedItem.ActionContext.Boundary != workPlanReport.ProductAction.Boundary ||
		admittedItem.ActionContext.Proof != workPlanReport.ProductAction.Proof ||
		!strings.Contains(admittedItem.NextAction, "prepare ready with reviewed action context") {
		t.Fatalf("workbench admitted item missing runnable action context: item=%+v", admittedItem)
	}

	advancePlanText := captureStdout(t, func() {
		cmdWork([]string{"advance-plan", workPlanReport.WorkPlan.WorkID, "--to", "ready", "--actor", "codex", "--dry-run"})
	})
	if !strings.Contains(advancePlanText, "action_context: "+strings.Join(workPlanReport.ProductAction.ActionCommand, " ")) ||
		!strings.Contains(advancePlanText, "proof="+workPlanReport.ProductAction.Proof) {
		t.Fatalf("advance-plan text missing action context:\n%s", advancePlanText)
	}
	advancePlanJSON := captureStdout(t, func() {
		cmdWork([]string{"advance-plan", workPlanReport.WorkPlan.WorkID, "--to", "ready", "--actor", "codex", "--dry-run", "--json"})
	})
	var advancePlan work.AdvancePlan
	if err := json.Unmarshal([]byte(advancePlanJSON), &advancePlan); err != nil {
		t.Fatalf("parse advance-plan: %v\n%s", err, advancePlanJSON)
	}
	if advancePlan.ActionContext == nil ||
		!sameStringSlice(advancePlan.ActionContext.Command, workPlanReport.ProductAction.ActionCommand) ||
		advancePlan.ActionContext.Proof != workPlanReport.ProductAction.Proof {
		t.Fatalf("advance-plan JSON missing action context: %+v", advancePlan.ActionContext)
	}

	captureStdout(t, func() {
		cmdWork([]string{"advance", workPlanReport.WorkPlan.WorkID, "--to", "ready", "--by", "codex"})
	})
	claimPlanText := captureStdout(t, func() {
		cmdWork([]string{"claim-plan", workPlanReport.WorkPlan.WorkID, "--actor", "codex", "--dry-run"})
	})
	if !strings.Contains(claimPlanText, "action_context: "+strings.Join(workPlanReport.ProductAction.ActionCommand, " ")) ||
		!strings.Contains(claimPlanText, "proof="+workPlanReport.ProductAction.Proof) {
		t.Fatalf("claim-plan text missing action context:\n%s", claimPlanText)
	}
	claimPlanJSON := captureStdout(t, func() {
		cmdWork([]string{"claim-plan", workPlanReport.WorkPlan.WorkID, "--actor", "codex", "--dry-run", "--json"})
	})
	var claimPlan work.ClaimPlan
	if err := json.Unmarshal([]byte(claimPlanJSON), &claimPlan); err != nil {
		t.Fatalf("parse claim-plan: %v\n%s", err, claimPlanJSON)
	}
	if claimPlan.ActionContext == nil ||
		claimPlan.ActionContext.ActionSummary != workPlanReport.ProductAction.ActionSummary ||
		!sameStringSlice(claimPlan.ActionContext.Command, workPlanReport.ProductAction.ActionCommand) ||
		claimPlan.ActionContext.Proof != workPlanReport.ProductAction.Proof {
		t.Fatalf("claim-plan JSON missing action context: %+v", claimPlan.ActionContext)
	}

	captureStdout(t, func() {
		cmdWork([]string{"claim", workPlanReport.WorkPlan.WorkID, "--by", "codex"})
	})
	captureStdout(t, func() {
		cmdWork([]string{"advance", workPlanReport.WorkPlan.WorkID, "--to", "running", "--by", "codex"})
	})
	actionPlanText := captureStdout(t, func() {
		cmdWork([]string{"action-plan", workPlanReport.WorkPlan.WorkID, "--actor", "codex"})
	})
	if !strings.Contains(actionPlanText, "action_command: "+strings.Join(workPlanReport.ProductAction.ActionCommand, " ")) ||
		!strings.Contains(actionPlanText, "action_summary: "+workPlanReport.ProductAction.ActionSummary) ||
		!strings.Contains(actionPlanText, "required_inputs: "+strings.Join(workPlanReport.ProductAction.RequiredInputs, ", ")) ||
		!strings.Contains(actionPlanText, "expected_validation: "+strings.Join(workPlanReport.ProductAction.ExpectedValidation, ", ")) ||
		!strings.Contains(actionPlanText, "stop_conditions: "+strings.Join(workPlanReport.ProductAction.StopConditions, ", ")) ||
		!strings.Contains(actionPlanText, "record_after_execution: compound work act "+workPlanReport.WorkPlan.WorkID+" --plan ") ||
		!strings.Contains(actionPlanText, "close_after_validation: compound work close-plan "+workPlanReport.WorkPlan.WorkID+" --action-plan ") {
		t.Fatalf("action-plan text missing receipt-ready boundary:\n%s", actionPlanText)
	}
	actionPlanPath := outputValue(actionPlanText, "plan:")
	if actionPlanPath == "" {
		t.Fatalf("action-plan text missing plan path:\n%s", actionPlanText)
	}
	actionPlan, err := work.LoadActionExecutionPlan(actionPlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if actionPlan.ActionContext == nil ||
		actionPlan.ActionSummary != workPlanReport.ProductAction.ActionSummary ||
		!sameStringSlice(actionPlan.ActionContext.Command, workPlanReport.ProductAction.ActionCommand) ||
		actionPlan.ReceiptDraft.MoveType != "outward_delta" ||
		actionPlan.ReceiptDraft.Outcome != "completed" ||
		!stringSliceContains(actionPlan.ReceiptDraft.ProofRefs, workPlanReport.WorkPlan.ActionContext.SourceRef) {
		t.Fatalf("action-plan artifact missing action context receipt shape: %+v", actionPlan)
	}
	closePlanBeforeActText := captureStdout(t, func() {
		cmdWork([]string{"close-plan", workPlanReport.WorkPlan.WorkID, "--action-plan", actionPlan.PlanPath, "--actor", "codex"})
	})
	if !strings.Contains(closePlanBeforeActText, "no_action: action execution plan") ||
		!strings.Contains(closePlanBeforeActText, "not recorded") {
		t.Fatalf("close-plan should refuse action plan before act --plan records it:\n%s", closePlanBeforeActText)
	}
	actionPlanJSON := captureStdout(t, func() {
		cmdWork([]string{"action-plan", workPlanReport.WorkPlan.WorkID, "--actor", "codex", "--dry-run", "--json"})
	})
	var dryRunActionPlan work.ActionExecutionPlan
	if err := json.Unmarshal([]byte(actionPlanJSON), &dryRunActionPlan); err != nil {
		t.Fatalf("parse action-plan: %v\n%s", err, actionPlanJSON)
	}
	if dryRunActionPlan.PlanPath != "" ||
		dryRunActionPlan.ActionIntent.Target.ID != workPlanReport.WorkPlan.ActionContext.SourceID ||
		dryRunActionPlan.ActionIntent.Target.Path != workPlanReport.WorkPlan.ActionContext.SourceRef {
		t.Fatalf("dry-run action-plan should derive target without recording: %+v", dryRunActionPlan)
	}
	actOut := captureStdout(t, func() {
		cmdWork([]string{"act", workPlanReport.WorkPlan.WorkID, "--plan", actionPlan.PlanPath})
	})
	if !strings.Contains(actOut, "action "+actionPlan.ActionIntent.ID+" recorded") {
		t.Fatalf("act --plan did not record planned action:\n%s", actOut)
	}
	afterAct, err := workStore.Get(workPlanReport.WorkPlan.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	// Same contract as the source-intake plan test: since the exact-approval
	// handoff landing (2026-09-18), the plan's pre-made decision is NOT
	// threaded verbatim — act --plan re-evaluates at act time and records the
	// fresh decision it printed; the plan intent ID threads verbatim.
	if afterAct.State != "running" ||
		!stringSliceContains(afterAct.ActionIntentIDs, actionPlan.ActionIntent.ID) ||
		!strings.HasPrefix(afterAct.PolicyDecisionID, "pol_") ||
		!strings.Contains(actOut, "decision "+afterAct.PolicyDecisionID) {
		t.Fatalf("act --plan did not thread action intent with a fresh act-time decision: %+v", afterAct)
	}
	closePlanText := captureStdout(t, func() {
		cmdWork([]string{"close-plan", workPlanReport.WorkPlan.WorkID, "--action-plan", actionPlan.PlanPath, "--actor", "codex"})
	})
	if !strings.Contains(closePlanText, "action_execution_plan: "+actionPlan.ID) ||
		!strings.Contains(closePlanText, "expected_validation: "+strings.Join(workPlanReport.ProductAction.ExpectedValidation, ", ")) ||
		!strings.Contains(closePlanText, "--proof "+workPlanReport.WorkPlan.ActionContext.SourceRef) {
		t.Fatalf("close-plan text did not consume action execution receipt draft:\n%s", closePlanText)
	}
	closePlanJSON := captureStdout(t, func() {
		cmdWork([]string{"close-plan", workPlanReport.WorkPlan.WorkID, "--action-plan", actionPlan.PlanPath, "--actor", "codex", "--json"})
	})
	var closePlan work.ClosePlan
	if err := json.Unmarshal([]byte(closePlanJSON), &closePlan); err != nil {
		t.Fatalf("parse close-plan: %v\n%s", err, closePlanJSON)
	}
	if closePlan.ActionExecutionPlanID != actionPlan.ID ||
		!stringSliceContains(closePlan.ProofRefs, workPlanReport.WorkPlan.ActionContext.SourceRef) ||
		!stringSliceContains(closePlan.ExpectedValidation, workPlanReport.ProductAction.ExpectedValidation[0]) ||
		closePlan.ReceiptDraft.ID != actionPlan.ReceiptDraft.ID {
		t.Fatalf("close-plan JSON missing action execution receipt draft: %+v", closePlan)
	}
	trace := writeCLITrace(t, homePath, "trace_product_action_context_cli", workPlanReport.WorkPlan.WorkID)
	observeRunText := captureStdout(t, func() {
		cmdWork([]string{"observe-run", workPlanReport.WorkPlan.WorkID, "--trace", trace.ID, "--actor", "codex"})
	})
	if !strings.Contains(observeRunText, "action_context: "+strings.Join(workPlanReport.ProductAction.ActionCommand, " ")) ||
		!strings.Contains(observeRunText, "proof="+workPlanReport.ProductAction.Proof) {
		t.Fatalf("observe-run text missing action context:\n%s", observeRunText)
	}
	observeRunJSON := captureStdout(t, func() {
		cmdWork([]string{"observe-run", workPlanReport.WorkPlan.WorkID, "--trace", trace.ID, "--actor", "codex", "--json"})
	})
	var observeRun work.RunObservationResult
	if err := json.Unmarshal([]byte(observeRunJSON), &observeRun); err != nil {
		t.Fatalf("parse observe-run: %v\n%s", err, observeRunJSON)
	}
	if observeRun.ActionContext == nil ||
		observeRun.ActionContext.ActionSummary != workPlanReport.ProductAction.ActionSummary ||
		!sameStringSlice(observeRun.ActionContext.Command, workPlanReport.ProductAction.ActionCommand) ||
		observeRun.ActionContext.Proof != workPlanReport.ProductAction.Proof {
		t.Fatalf("observe-run JSON missing action context: %+v", observeRun.ActionContext)
	}

	afterAdmitTimeline, err := product.LoadTimeline(homePath, "product:action-cli")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterAdmitTimeline.Signals) != len(beforeTimeline.Signals) ||
		len(afterAdmitTimeline.Hypotheses) != len(beforeTimeline.Hypotheses) ||
		len(afterAdmitTimeline.Transitions) != len(beforeTimeline.Transitions) {
		t.Fatalf("admit-plan should not mutate product timeline: before=%+v after=%+v", beforeTimeline, afterAdmitTimeline)
	}
}

func seedCLIProductActionLoop(t *testing.T, homePath string, store *mem.Store) {
	t.Helper()
	bundle := product.Bundle{
		SchemaVersion: product.SchemaVersion,
		Product: product.Product{
			ID:             "product:action-cli",
			Name:           "Action CLI",
			OwnerEntity:    "operator:test",
			Surfaces:       []product.Surface{{ID: "surface:action-cli", Kind: "web", Ref: "fixture://action-cli"}},
			Channels:       []string{"web"},
			Positioning:    "Synthetic action-card review CLI fixture.",
			PrivacyPosture: "test_fixture",
		},
		Hypotheses: []product.Hypothesis{{
			ID:                "hyp_action_cli",
			ProductID:         "product:action-cli",
			ChangeEvent:       "Changed onboarding CTA copy.",
			ExpectedEffect:    "Activation rate increases.",
			Metrics:           []string{"activation_rate"},
			ObservationWindow: "7d after change",
			Result:            "unknown",
			NextAction:        "review activation after window",
			EvidenceRefs:      []string{"fixture:action-cli-hypothesis"},
		}},
		EvidenceRefs: []string{"fixture:action-cli-product"},
	}
	if _, err := product.Apply(homePath, store, bundle, "fixture/action-cli-product.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}
	signalDoc := product.SignalDocument{
		SchemaVersion: product.SignalSchemaVersion,
		Source:        product.SignalSource{Kind: "posthog", Ref: "fixture:action-cli-activation"},
		Signal: product.Signal{
			ID:           "sig_action_cli_activation",
			ProductID:    "product:action-cli",
			Surface:      "onboarding",
			ChangeEvent:  "Changed onboarding CTA copy.",
			MetricName:   "activation_rate",
			Before:       0.22,
			After:        0.29,
			Window:       "2026-06-29/2026-07-06",
			Confidence:   0.71,
			Confounders:  []string{"synthetic fixture"},
			EvidenceRefs: []string{"fixture:action-cli-activation"},
			ObservedAt:   cliTestNow.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		},
		EvidenceRefs: []string{"fixture:action-cli-export"},
	}
	if _, err := product.ApplySignal(homePath, store, signalDoc, "fixture/action-cli-signal.yaml", cliTestNow.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	review := product.Review{
		SchemaVersion: product.ReviewSchemaVersion,
		ID:            "review_action_cli_activation",
		ProductID:     "product:action-cli",
		HypothesisID:  "hyp_action_cli",
		Result:        "supported",
		Rationale:     "Synthetic activation movement supports a bounded next action.",
		Confidence:    0.76,
		NextAction:    "prepare one bounded onboarding experiment proposal",
		EvidenceRefs:  []string{"product_signal:sig_action_cli_activation", "fixture:action-cli-review"},
		ReviewedBy:    "operator",
	}
	if _, err := product.ApplyReview(homePath, store, review, "fixture/action-cli-review.yaml", cliTestNow.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func findCLIActionCard(products []product.ProductLoop, kind string) product.LoopActionCard {
	for _, loop := range products {
		for _, card := range loop.ActionCards {
			if card.Kind == kind {
				return card
			}
		}
	}
	return product.LoopActionCard{}
}

func TestMemoryEngineBreadthProfileAppliesOptInEmbedderFromConfig(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	// A fake breadth sidecar so profile=breadth resolves a bin.
	binDir := t.TempDir()
	sidecarPath := filepath.Join(binDir, "compound-breadth-engine")
	if err := os.WriteFile(sidecarPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake breadth engine: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfgPath := memoryEngineConfigPath(homePath)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	yamlBody := "schema_version: compound.memory_engine_config.v0.1\n" +
		"profile: breadth\n" +
		"embed:\n" +
		"  provider: ollama\n" +
		"  model: nomic-embed-text\n" +
		"  base_url: http://embedder.invalid:11434/v1\n"
	if err := os.WriteFile(cfgPath, []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Ensure a stripped env before configuring.
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", "")

	restore, err := configureMemoryEngineFromHome(homePath, "")
	if err != nil {
		t.Fatalf("configure from breadth+embed config: %v", err)
	}
	if got := os.Getenv("COMPOUND_BREADTH_EMBED_MODEL"); got != "nomic-embed-text" {
		t.Fatalf("embed model from config = %q, want nomic-embed-text (semantic lane would stay OFF)", got)
	}
	if got := os.Getenv("COMPOUND_BREADTH_EMBED_PROVIDER"); got != "ollama" {
		t.Fatalf("embed provider = %q, want ollama", got)
	}
	if got := os.Getenv("COMPOUND_BREADTH_EMBED_BASE_URL"); got != "http://embedder.invalid:11434/v1" {
		t.Fatalf("embed base_url = %q", got)
	}
	restore()
	if got := os.Getenv("COMPOUND_BREADTH_EMBED_MODEL"); got != "" {
		t.Fatalf("restore should clear embed model, got %q", got)
	}
}

func TestMemoryEngineOptInEmbedderIsFillInOnly_ShellExportWins(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	binDir := t.TempDir()
	sidecarPath := filepath.Join(binDir, "compound-breadth-engine")
	if err := os.WriteFile(sidecarPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake breadth engine: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfgPath := memoryEngineConfigPath(homePath)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	yamlBody := "schema_version: compound.memory_engine_config.v0.1\nprofile: breadth\nembed:\n  provider: ollama\n  model: config-model\n"
	if err := os.WriteFile(cfgPath, []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "shell-model") // explicit export must win

	restore, err := configureMemoryEngineFromHome(homePath, "")
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	defer restore()
	if got := os.Getenv("COMPOUND_BREADTH_EMBED_MODEL"); got != "shell-model" {
		t.Fatalf("shell export should win over config, got %q", got)
	}
}

func TestMemoryEngineNoEmbedBlockLeavesLaneOff(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	binDir := t.TempDir()
	sidecarPath := filepath.Join(binDir, "compound-breadth-engine")
	if err := os.WriteFile(sidecarPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake breadth engine: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := writeMemoryEngineConfig(homePath, "breadth"); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "")
	restore, err := configureMemoryEngineFromHome(homePath, "")
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	defer restore()
	if got := os.Getenv("COMPOUND_BREADTH_EMBED_MODEL"); got != "" {
		t.Fatalf("no embed block must leave lane OFF (no silent default), got model=%q", got)
	}
}

// TestDistillerRequestFromFindingCLIRecordsPrivateBodyReferenceOnly is the public CLI seam red
// test for the finding→distiller bridge. `compound distiller request --finding <id>` must
// accept the new flag, set source_privacy to private, embed finding:<id> and work:<id> in
// source_refs, produce a ref-only artifact under reviews/distiller-requests/ that omits the
// finding body, and not write any ledger byte.
func TestDistillerRequestFromFindingCLIRecordsPrivateBodyReferenceOnly(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	var recorded struct {
		ID     string `json:"id"`
		CID    string `json:"cid"`
		WorkID string `json:"work_id"`
	}
	out := captureStdout(t, func() {
		cmdFinding([]string{"record", "--topic", "bridge-cli", "--body", "body-must-not-appear-in-cli-request", "--work-id", "work_bridge_cli", "--produced-by", "agent:codex", "--json"})
	})
	if err := json.Unmarshal([]byte(out), &recorded); err != nil {
		t.Fatalf("parse finding record: %v\n%s", err, out)
	}
	if !strings.HasPrefix(recorded.ID, "find_") {
		t.Fatalf("finding record = %+v", recorded)
	}
	// `finding record --json` returns the body-free record-ack (id/path/status/privacy/
	// visibility only); cid and work_id are provenance metadata that live behind the explicit
	// show surface. Read them from there rather than widening the ack.
	showOut := captureStdout(t, func() {
		cmdFinding([]string{"show", recorded.ID, "--json"})
	})
	if err := json.Unmarshal([]byte(showOut), &recorded); err != nil {
		t.Fatalf("parse finding show: %v\n%s", err, showOut)
	}
	if !strings.HasPrefix(recorded.CID, "sha256:") || recorded.WorkID != "work_bridge_cli" {
		t.Fatalf("finding show = %+v", recorded)
	}

	reqJSON := captureStdout(t, func() {
		cmdDistiller([]string{"request", "--trace-ref", "trace_bridge_cli", "--finding", recorded.ID, "--record"})
	})
	var req distiller.LLMExtractionRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("parse distiller request: %v\n%s", err, reqJSON)
	}
	if req.SourcePrivacy != "private" {
		t.Fatalf("source_privacy = %q, want private", req.SourcePrivacy)
	}
	if !containsString(req.SourceRefs, "finding:"+recorded.ID) {
		t.Fatalf("source_refs missing finding ref: %+v", req.SourceRefs)
	}
	if !containsString(req.SourceRefs, "work:"+recorded.WorkID) {
		t.Fatalf("source_refs missing work ref: %+v", req.SourceRefs)
	}
	if len(req.FindingRefs) != 1 || req.FindingRefs[0].ID != recorded.ID || req.FindingRefs[0].CID != recorded.CID {
		t.Fatalf("finding_refs = %+v, want one entry id=%s cid=%s", req.FindingRefs, recorded.ID, recorded.CID)
	}
	if req.RawTrace != "" {
		t.Fatalf("stored request RawTrace must be empty, got %q", req.RawTrace)
	}

	recordedPath := filepath.Join(homePath, "reviews", "distiller-requests")
	entries, err := os.ReadDir(recordedPath)
	if err != nil {
		t.Fatalf("read distiller-requests dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("distiller-requests entries = %d, want 1", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(recordedPath, entries[0].Name()))
	if err != nil {
		t.Fatalf("read recorded request: %v", err)
	}
	if strings.Contains(string(body), "body-must-not-appear-in-cli-request") {
		t.Fatalf("stored request leaked finding body: %s", body)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("distiller request must never touch ledger/claims.jsonl, stat err=%v", err)
	}
}

// TestDistillerRequestFromFindingCLIDerivesTraceRef proves that with --finding and no explicit
// --trace-ref, the trace_ref defaults to `finding:<id>`; --trace-ref still wins when provided.
func TestDistillerRequestFromFindingCLIDerivesTraceRef(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	recJSON := captureStdout(t, func() {
		cmdFinding([]string{"record", "--topic", "trace-derived", "--body", "trace-derived-body", "--work-id", "work_trace_derived", "--produced-by", "agent:codex", "--json"})
	})
	var rec finding.Finding
	if err := json.Unmarshal([]byte(recJSON), &rec); err != nil {
		t.Fatalf("parse finding record: %v\n%s", err, recJSON)
	}

	// no --trace-ref → derive `finding:<id>`
	derivedJSON := captureStdout(t, func() {
		cmdDistiller([]string{"request", "--finding", rec.ID})
	})
	var derived distiller.LLMExtractionRequest
	if err := json.Unmarshal([]byte(derivedJSON), &derived); err != nil {
		t.Fatalf("parse derived request: %v\n%s", err, derivedJSON)
	}
	if derived.TraceRef != "finding:"+rec.ID {
		t.Fatalf("derived trace_ref = %q, want finding:%s", derived.TraceRef, rec.ID)
	}

	// explicit --trace-ref still wins.
	explicitJSON := captureStdout(t, func() {
		cmdDistiller([]string{"request", "--trace-ref", "trace_explicit", "--finding", rec.ID})
	})
	var explicit distiller.LLMExtractionRequest
	if err := json.Unmarshal([]byte(explicitJSON), &explicit); err != nil {
		t.Fatalf("parse explicit request: %v\n%s", err, explicitJSON)
	}
	if explicit.TraceRef != "trace_explicit" {
		t.Fatalf("explicit trace_ref = %q, want trace_explicit", explicit.TraceRef)
	}
}

// TestDistillerRequestFromFindingCLIIdempotentReplay proves same finding + unchanged prior
// state records the same request artifact id (packet's idempotence invariant).
func TestDistillerRequestFromFindingCLIIdempotentReplay(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	var rec finding.Finding
	out := captureStdout(t, func() {
		cmdFinding([]string{"record", "--topic", "bridge-cli-idem", "--body", "idem-body", "--work-id", "work_bridge_cli_idem", "--produced-by", "agent:codex", "--json"})
	})
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("parse finding record: %v\n%s", err, out)
	}

	first := captureStdout(t, func() {
		cmdDistiller([]string{"request", "--trace-ref", "trace_bridge_cli_idem", "--finding", rec.ID, "--record"})
	})
	second := captureStdout(t, func() {
		cmdDistiller([]string{"request", "--trace-ref", "trace_bridge_cli_idem", "--finding", rec.ID, "--record"})
	})
	if first != second {
		t.Fatalf("idempotent replay must produce identical request JSON\nfirst=%s\nsecond=%s", first, second)
	}
}
