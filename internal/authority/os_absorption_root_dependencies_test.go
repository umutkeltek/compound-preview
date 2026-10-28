package authority

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/mem"
	"gopkg.in/yaml.v3"
)

func TestInspectOSRootDependenciesFindsActiveBindingsAndIgnoresDisabledOnes(t *testing.T) {
	compoundHome := t.TempDir()
	installedHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacyRoot := filepath.Join(installedHome, "Projects", "operating-system")
	umutOS := filepath.Join(legacyRoot, "umut-os")
	infraOS := filepath.Join(legacyRoot, "infra-os")
	decisionRoot := filepath.Join(legacyRoot, "decisions")
	for _, root := range []string{umutOS, infraOS, decisionRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeOSRootFixture(t, filepath.Join(decisionRoot, "decisions", "agents", "DEC-0001-active-decision.md"), "# Active decision\n")
	seedLegacyOSRegistry(t, compoundHome, installedHome, umutOS, infraOS, decisionRoot)
	writeOSRootFixture(t, filepath.Join(legacyRoot, "AGENTS.md"), "# Legacy root instructions\n")
	writeOSRootFixture(t, filepath.Join(umutOS, "LIVE_START.md"), "# Legacy live start\n")

	writeOSRootFixture(t, filepath.Join(installedHome, ".claude", "CLAUDE.md"), "generated from "+filepath.Join(umutOS, "spine")+"\n")
	writeOSRootFixture(t, filepath.Join(installedHome, ".codex", "AGENTS.md"), "Compound only\n")
	writeOSRootFixture(t, filepath.Join(installedHome, ".claude", "settings.json"), `{"hook":"${HOME}/Projects/operating-system/umut-os/spine/hook.ts"}`)
	writeOSRootFixture(t, filepath.Join(installedHome, ".codex", "hooks.json"), `{"hook":"compound hook session-start"}`)
	writeOSRootFixture(
		t,
		filepath.Join(installedHome, ".claude", "hooks", "RecallContext.hook.ts"),
		`const root = join(HOME, "Projects", "operating-system", "umut-os")`,
	)
	writeOSRootFixture(
		t,
		filepath.Join(installedHome, ".codex", "hooks", "RecallContext.hook.ts"),
		`// GENERATED from operating-system/umut-os/spine/hooks/RecallContext.hook.ts`,
	)
	writeOSRootFixture(t, filepath.Join(installedHome, ".local", "bin", "os"), "exec bun "+filepath.Join(umutOS, "exec-lane", "os.ts")+"\n")
	writeOSRootFixture(t, filepath.Join(installedHome, ".hermes", "config.yaml"), "workdir: ~/Projects/operating-system/infra-os\n")

	launchRoot := filepath.Join(installedHome, "Library", "LaunchAgents")
	writeOSRootFixture(t, filepath.Join(launchRoot, "ai.legacy.plist"), "<string>"+filepath.Join(umutOS, "cron", "tick.sh")+"</string>\n")
	writeOSRootFixture(t, filepath.Join(launchRoot, "ai.disabled.plist.disabled"), "<string>"+filepath.Join(umutOS, "cron", "disabled.sh")+"</string>\n")
	indirectWriter := filepath.Join(installedHome, ".hermes", "scripts", "brain-circulation.py")
	writeOSRootFixture(t, indirectWriter, "legacy_root = '"+filepath.Join(umutOS, "state", "runs")+"'\n")
	writeOSRootFixture(t, filepath.Join(launchRoot, "ai.indirect.plist"), "<string>"+indirectWriter+"</string>\n<string>2>&1</string>\n")
	dataLog := filepath.Join(installedHome, ".headroom", "logs", "status.jsonl")
	writeOSRootFixture(t, dataLog, `{"historical_source":"`+filepath.Join(umutOS, "state")+`"}`)
	writeOSRootFixture(t, filepath.Join(launchRoot, "ai.data.plist"), "<string>"+dataLog+"</string>\n")
	writeOSRootFixture(t, filepath.Join(launchRoot, "ai.path-list.plist"), "<string>"+installedHome+"/bin:/usr/bin:/bin</string>\n")

	hermesScript := filepath.Join(installedHome, ".hermes", "scripts", "legacy-job.py")
	writeOSRootFixture(t, hermesScript, "exec_path = '"+filepath.Join(umutOS, "cron", "legacy_job.py")+"'\n")
	jobs := map[string]any{"jobs": []map[string]any{
		{"name": "legacy enabled", "enabled": true, "script": "legacy-job.py"},
		{"name": "legacy disabled", "enabled": false, "script": filepath.Join(umutOS, "cron", "disabled.py")},
		{"name": "compound only", "enabled": true, "script": "compound-job.py"},
	}}
	jobsBody, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	writeOSRootFixture(t, filepath.Join(installedHome, ".hermes", "cron", "jobs.json"), string(jobsBody))

	report, err := inspectOSRootDependencies(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "fail" || report.DependencyCount != 11 {
		t.Fatalf("dependency status=%s count=%d kinds=%v dependencies=%+v", report.Status, report.DependencyCount, report.CountsByKind, report.Dependencies)
	}
	wantKinds := map[string]int{
		"hermes_config":         1,
		"hermes_job":            1,
		"installed_instruction": 1,
		"installed_shim":        1,
		"launchd":               2,
		"source_bootstrap":      2,
		"session_hook":          3,
	}
	if fmt.Sprint(report.CountsByKind) != fmt.Sprint(wantKinds) {
		t.Fatalf("counts = %v, want %v", report.CountsByKind, wantKinds)
	}
	if report.DecisionCoverage.Eligible != 1 || report.DecisionCoverage.Covered != 0 || report.DecisionCoverage.Missing != 1 {
		t.Fatalf("decision coverage = %+v", report.DecisionCoverage)
	}
	if len(report.Custody) != 3 {
		t.Fatalf("custody rows = %+v", report.Custody)
	}
	for _, custody := range report.Custody {
		if custody.Status != "missing" || custody.CustodyComplete {
			t.Fatalf("missing custody must stay incomplete: %+v", custody)
		}
	}
}

func TestInspectLegacyDecisionCoverageExcludesHistoricalReviewEvidence(t *testing.T) {
	homePath := t.TempDir()
	decisionRoot := filepath.Join(t.TempDir(), "decisions")
	writeOSRootFixture(t, filepath.Join(decisionRoot, "decisions", "agents", "DEC-0001-active.md"), "# Active decision\n")
	writeOSRootFixture(t, filepath.Join(decisionRoot, "reviews", "2026-01-01-probe.md"), "# Historical review evidence\n")
	writeOSRootFixture(t, filepath.Join(decisionRoot, "AGENTS.md"), "# Bootstrap instructions\n")
	writeOSRootFixture(t, filepath.Join(decisionRoot, "INDEX.md"), "# Decision index\n")

	coverage := inspectLegacyDecisionCoverage(homePath, []string{decisionRoot})
	if coverage.Eligible != 1 || coverage.Covered != 0 || coverage.Missing != 1 {
		t.Fatalf("decision coverage treated custody-only Markdown as semantic decisions: %+v", coverage)
	}
}

func TestInspectLegacyDecisionCoverageUsesFrozenCustodyWhenRootIsUnavailable(t *testing.T) {
	homePath := t.TempDir()
	decisionRoot := filepath.Join(t.TempDir(), "decisions")
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("# Frozen decision source\n")
	bodyRef, err := store.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	manifest := map[string]any{
		"lifecycle": map[string]any{"state": "legacy_frozen"},
		"items": []map[string]any{{
			"body_policy": "owned_snapshot",
			"body_ref":    bodyRef,
			"body_bytes":  len(body),
			"file_hash":   digest,
		}},
	}
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeOSRootFixture(t, filepath.Join(homePath, "sources", "manifests", "decisions.json"), string(manifestBody))

	var authorityRows []string
	for i, name := range []string{"DEC-0001-one.md", "DEC-0002-two.md"} {
		sourcePath := filepath.Join(decisionRoot, "decisions", "agents", name)
		row, err := json.Marshal(map[string]any{
			"id":               fmt.Sprintf("decsrc_%d", i),
			"source_path":      sourcePath,
			"source_kind":      "policy",
			"authority_status": "compound_primary",
			"retires":          "decision_source_authority:" + sourcePath,
		})
		if err != nil {
			t.Fatal(err)
		}
		authorityRows = append(authorityRows, string(row))
	}
	writeOSRootFixture(
		t,
		filepath.Join(homePath, "registries", "decision-source-authority.jsonl"),
		strings.Join(authorityRows, "\n")+"\n",
	)

	coverage := inspectLegacyDecisionCoverage(homePath, []string{decisionRoot})
	if coverage.Status != "ok" || coverage.Eligible != 2 || coverage.Covered != 2 || coverage.Missing != 0 {
		t.Fatalf("unavailable root should use frozen custody plus Compound-primary records: %+v", coverage)
	}
}

func TestInspectOSRootDependenciesSkipsPublicCloneWithoutDeclaredRoots(t *testing.T) {
	report, err := inspectOSRootDependencies(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "not_configured" || report.DependencyCount != 0 {
		t.Fatalf("public clone report = %+v", report)
	}
}

func TestInspectLegacySourceFamilyCustodyRequiresExactOwnedBodies(t *testing.T) {
	homePath := t.TempDir()
	root := t.TempDir()
	manifestPath := filepath.Join(homePath, "sources", "manifests", "os.json")
	writeOSRootFixture(t, manifestPath, `{"items":[{"body_policy":"source_in_place","file_hash":"sha256:missing"}]}`)
	got := inspectLegacySourceFamilyCustody(homePath, "os", root)
	if got.CustodyComplete || got.Status != "pending" {
		t.Fatalf("source-in-place custody = %+v", got)
	}

	body := []byte("owned operating-system source\n")
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bodyRef, err := store.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	manifest := map[string]any{"items": []map[string]any{{
		"body_policy": "owned_snapshot",
		"body_ref":    bodyRef,
		"body_bytes":  len(body),
		"file_hash":   digest,
	}}}
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeOSRootFixture(t, manifestPath, string(manifestBody))
	got = inspectLegacySourceFamilyCustody(homePath, "os", root)
	if !got.CustodyComplete || got.LifecycleTerminal || got.Status != "pending" {
		t.Fatalf("owned but nonterminal custody = %+v", got)
	}

	manifest["lifecycle"] = map[string]any{"state": "retired"}
	manifestBody, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeOSRootFixture(t, manifestPath, string(manifestBody))
	got = inspectLegacySourceFamilyCustody(homePath, "os", root)
	if !got.CustodyComplete || !got.LifecycleTerminal || got.Status != "ok" {
		t.Fatalf("retired owned custody = %+v", got)
	}
}

func seedLegacyOSRegistry(t *testing.T, compoundHome, installedHome, umutOS, infraOS, decisionsRoot string) {
	t.Helper()
	registryPath := filepath.Join(t.TempDir(), "os-path-authority.yaml")
	body, err := yaml.Marshal(osPathRegistryFile{
		Version: 1,
		CanonicalRoots: map[string]osPathRoot{
			InstalledHarnessHomeKey: {Path: installedHome, Status: "active"},
			"umut_os":               {Path: umutOS, Status: "active"},
			"infra_os":              {Path: infraOS, Status: "active"},
			"decisions":             {Path: decisionsRoot, Status: "active"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeOSRootFixture(t, registryPath, string(body))
	store, err := mem.Open(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := store.Entities()
	if err != nil {
		t.Fatal(err)
	}
	_, entities, facts, importReport, err := ImportOSPathRegistry(registryPath, existing)
	if err != nil {
		t.Fatal(err)
	}
	if importReport.Status != "ok" {
		t.Fatalf("path import = %+v", importReport)
	}
	now := time.Now()
	for _, entity := range entities {
		if _, err := store.RegisterEntity(entity, now); err != nil {
			t.Fatal(err)
		}
	}
	captured, err := store.Capture(facts, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range captured {
		if _, err := store.Adjudicate(candidate.ID, "promote", "test legacy OS dependency registry", now); err != nil {
			t.Fatal(err)
		}
	}
}

func writeOSRootFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
