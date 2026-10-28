package authority

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/home"
	"compound/internal/routeevidence"
)

type osCapabilityProofFixture struct {
	disabledExitZero          bool
	secondaryDisabledExitZero bool
	disabledWritesPacket      bool
	secondaryWritesPacket     bool
	disabledAuthorityRefs     []string
	projectionPoison          string
	projectionShadow          bool
	entrypointPoison          string
	shimPoison                string
	wrapperOfferID            string
	nonceWrapperOfferID       string
	legacyDispatch            bool
	mutateEntrypoint          bool
	mutateRestoreEntrypoint   bool
	mutateProjectionShadow    bool
	mutateCompoundBinary      bool
	mutateRestoreCompound     bool
	resolutionMutator         func(map[string]any)
	skipAbsorbedOSSeed        bool
}

type installedOSCapabilityProofFixture struct {
	compoundHome string
}

func installOSCapabilityProofFixture(t *testing.T, opts osCapabilityProofFixture) installedOSCapabilityProofFixture {
	t.Helper()
	root := t.TempDir()
	userHome := filepath.Join(root, "home")
	compoundHome := filepath.Join(root, "compound-home")
	binDir := filepath.Join(root, "bin")
	osRoot := filepath.Join(userHome, "absorbed-os")
	execLane := filepath.Join(osRoot, "exec-lane")
	for _, dir := range []string{filepath.Join(userHome, ".local", "bin"), filepath.Join(userHome, ".bun", "bin"), binDir, execLane} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := home.Init(compoundHome, "test", time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if !opts.skipAbsorbedOSSeed {
		seedAbsorbedOSRegistry(t, compoundHome, osRoot)
	}

	dispatch := `else if (cmd === "capability") exit = await runCapabilityProxy(rest);`
	if opts.legacyDispatch {
		dispatch = `else if (cmd === "capability") exit = await runCapability(rest);`
	}
	entrypoint := `#!/usr/bin/env bun
import { runCapabilityProxy } from "./os-capability-proxy";
const [cmd, ...rest] = process.argv.slice(2);
let exit = 0;
` + dispatch + `
process.exit(exit);
` + opts.entrypointPoison + "\n"
	writeExecutableFixture(t, filepath.Join(execLane, "os.ts"), entrypoint)
	projection := `import { spawnSync } from "node:child_process";
const compoundArgs = ["capability", "resolve", "--json"];
function validateResolution() { return true; }
export async function runCapabilityProxy() {
  const result = spawnSync("compound", compoundArgs);
  if (result.status !== 0) return 2;
  return validateResolution() ? 0 : 2;
}
	` + opts.projectionPoison + "\n"
	writeExecutableFixture(t, filepath.Join(execLane, "os-capability-proxy.ts"), projection)
	if opts.projectionShadow {
		writeExecutableFixture(t, filepath.Join(execLane, "os-capability-proxy.tsx"), `export async function runCapabilityProxy() { return 0; }`)
	}

	shim := `#!/bin/sh
` + opts.shimPoison + `
exec "${HOME}/.bun/bin/bun" "${HOME}/absorbed-os/exec-lane/os.ts" "$@"
`
	writeExecutableFixture(t, filepath.Join(userHome, ".local", "bin", "os"), shim)

	direct := osCapabilityResolutionFixture("tool:compound.proof-check")
	wrapperOfferID := opts.wrapperOfferID
	if wrapperOfferID == "" {
		wrapperOfferID = "tool:compound.proof-check"
	}
	wrapper := osCapabilityResolutionFixture(wrapperOfferID)
	secondaryDirect := osCapabilityResolutionFixtureForTask(osCapabilitySecondaryProbeTask, "judge-critique", "tool:compound.proof-check")
	secondaryWrapperOfferID := "tool:compound.proof-check"
	if opts.nonceWrapperOfferID != "" {
		secondaryWrapperOfferID = opts.nonceWrapperOfferID
	}
	secondaryWrapper := osCapabilityResolutionFixtureForTask(osCapabilitySecondaryProbeTask, "judge-critique", secondaryWrapperOfferID)
	if opts.resolutionMutator != nil {
		for _, report := range []map[string]any{direct, wrapper, secondaryDirect, secondaryWrapper} {
			opts.resolutionMutator(report)
		}
	}
	directBody, err := json.Marshal(direct)
	if err != nil {
		t.Fatal(err)
	}
	wrapperBody, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	secondaryDirectBody, err := json.Marshal(secondaryDirect)
	if err != nil {
		t.Fatal(err)
	}
	secondaryWrapperBody, err := json.Marshal(secondaryWrapper)
	if err != nil {
		t.Fatal(err)
	}

	const taskPlaceholder = "__OS_CAPABILITY_PROOF_TASK__"
	directTemplate := strings.ReplaceAll(string(directBody), osCapabilityProbeTask, taskPlaceholder)
	wrapperTemplate := strings.ReplaceAll(string(wrapperBody), osCapabilityProbeTask, taskPlaceholder)
	secondaryDirectTemplate := strings.ReplaceAll(string(secondaryDirectBody), osCapabilitySecondaryProbeTask, taskPlaceholder)
	secondaryWrapperTemplate := strings.ReplaceAll(string(secondaryWrapperBody), osCapabilitySecondaryProbeTask, taskPlaceholder)
	compoundMutation := ""
	if opts.mutateEntrypoint {
		compoundMutation += fmt.Sprintf("printf '\\n// mutated during proof\\n' >> %q\n", filepath.Join(execLane, "os.ts"))
	}
	if opts.mutateRestoreEntrypoint {
		entrypointPath := filepath.Join(execLane, "os.ts")
		backupPath := filepath.Join(root, "reviewed-os.ts")
		if err := os.WriteFile(backupPath, []byte(entrypoint), 0o600); err != nil {
			t.Fatal(err)
		}
		entrypointInfo, err := os.Stat(entrypointPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(backupPath, entrypointInfo.ModTime(), entrypointInfo.ModTime()); err != nil {
			t.Fatal(err)
		}
		compoundMutation += fmt.Sprintf("printf '\\n// transient alternate resolver\\n' >> %q\ncp %q %q\ntouch -r %q %q\n", entrypointPath, backupPath, entrypointPath, backupPath, entrypointPath)
	}
	if opts.mutateProjectionShadow {
		shadowPath := filepath.Join(execLane, "os-capability-proxy.tsx")
		compoundMutation += fmt.Sprintf("printf 'export async function runCapabilityProxy() { return 0; }\\n' > %q\nrm %q\n", shadowPath, shadowPath)
	}
	if opts.mutateCompoundBinary {
		compoundMutation += "printf '\\n# mutated during proof\\n' >> \"$0\"\n"
	}
	compoundBackupPath := filepath.Join(root, "reviewed-compound")
	if opts.mutateRestoreCompound {
		compoundMutation += fmt.Sprintf("printf '\\n# transient binary replacement\\n' >> \"$0\"\ncp %q \"$0\"\ntouch -r %q \"$0\"\n", compoundBackupPath, compoundBackupPath)
	}
	compoundScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "version" ]; then
  echo "compound 0.1.0 (build abc1234)"
  exit 0
fi
if [ "$1" = "capability" ] && [ "$2" = "resolve" ]; then
%s
  case "$4" in
  %q*)
    cat <<'JSON' | sed "s|%s|$4|g"
%s
JSON
    exit 0
    ;;
  %q*)
    cat <<'JSON' | sed "s|%s|$4|g"
%s
JSON
    exit 0
    ;;
  esac
fi
exit 2
	`, compoundMutation, osCapabilitySecondaryProbeTask, taskPlaceholder, secondaryDirectTemplate, osCapabilityProbeTask, taskPlaceholder, directTemplate)
	writeExecutableFixture(t, filepath.Join(binDir, "compound"), compoundScript)
	if opts.mutateRestoreCompound {
		writeExecutableFixture(t, compoundBackupPath, compoundScript)
		compoundInfo, err := os.Stat(filepath.Join(binDir, "compound"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(compoundBackupPath, compoundInfo.ModTime(), compoundInfo.ModTime()); err != nil {
			t.Fatal(err)
		}
	}

	disabledAuthorityRefs := opts.disabledAuthorityRefs
	if disabledAuthorityRefs == nil {
		disabledAuthorityRefs = []string{
			routeevidence.CapabilityTaskClassifierAuthorityRef,
			capgraph.CapabilityGraphAuthorityRef,
			routeevidence.ModelCapabilitiesAuthorityRef,
		}
	}
	disabledReport := map[string]any{
		"schema_version": "os.capability_proxy.v0.1",
		"status":         "unavailable",
		"task":           osCapabilityProbeTask,
		"authority_refs": disabledAuthorityRefs,
		"error":          "disabled",
	}
	disabledBytes, err := json.Marshal(disabledReport)
	if err != nil {
		t.Fatal(err)
	}
	disabledBody := strings.ReplaceAll(string(disabledBytes), osCapabilityProbeTask, taskPlaceholder)
	disabledBranch := fmt.Sprintf(`cat <<'JSON' | sed "s|%s|$3|g"
%s
JSON
exit 2`, taskPlaceholder, disabledBody)
	if opts.disabledExitZero {
		disabledBranch = fmt.Sprintf(`case "$3" in
%q*)
  cat <<'JSON' | sed "s|%s|$3|g"
%s
JSON
  exit 0
  ;;
%q*)
  cat <<'JSON' | sed "s|%s|$3|g"
%s
JSON
  exit 0
  ;;
esac
exit 2`, osCapabilitySecondaryProbeTask, taskPlaceholder, secondaryWrapperTemplate, osCapabilityProbeTask, taskPlaceholder, wrapperTemplate)
	} else if opts.secondaryDisabledExitZero {
		disabledBranch = fmt.Sprintf(`case "$3" in
%q*)
  cat <<'JSON' | sed "s|%s|$3|g"
%s
JSON
  exit 0
  ;;
esac
cat <<'JSON' | sed "s|%s|$3|g"
%s
JSON
exit 2`, osCapabilitySecondaryProbeTask, taskPlaceholder, secondaryWrapperTemplate, taskPlaceholder, disabledBody)
	}
	if opts.disabledWritesPacket {
		disabledBranch = fmt.Sprintf(`mkdir -p "$OS_CAPABILITY_PACKET_DIR"
printf '{}\n' > "$OS_CAPABILITY_PACKET_DIR/forged.json"
%s`, disabledBranch)
	} else if opts.secondaryWritesPacket {
		disabledBranch = fmt.Sprintf(`case "$3" in
%q*)
  mkdir -p "$OS_CAPABILITY_PACKET_DIR"
  printf '{}\n' > "$OS_CAPABILITY_PACKET_DIR/forged.json"
  ;;
esac
%s`, osCapabilitySecondaryProbeTask, disabledBranch)
	}
	bunScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "1.2.3"
  exit 0
fi
if [ "${OS_COMPOUND_CLI:-}" = "off" ]; then
%s
fi
case "$3" in
%q*)
  cat <<'JSON' | sed "s|%s|$3|g"
%s
JSON
  exit 0
  ;;
%q*)
  cat <<'JSON' | sed "s|%s|$3|g"
%s
JSON
  exit 0
  ;;
esac
exit 2
`, disabledBranch, osCapabilitySecondaryProbeTask, taskPlaceholder, secondaryWrapperTemplate, osCapabilityProbeTask, taskPlaceholder, wrapperTemplate)
	writeExecutableFixture(t, filepath.Join(userHome, ".bun", "bin", "bun"), bunScript)

	t.Setenv("HOME", userHome)
	t.Setenv("PATH", filepath.Join(userHome, ".local", "bin")+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return installedOSCapabilityProofFixture{compoundHome: compoundHome}
}

func doctorOSCapabilityRow(t *testing.T, homePath string) OSAbsorptionDoctorRow {
	return doctorOSCapabilityRowWithEvidence(t, homePath, osCapabilityFixtureSourceDigestRefs(t))
}

func doctorOSCapabilityRowWithEvidence(t *testing.T, homePath string, sourceEvidenceRefs []string) OSAbsorptionDoctorRow {
	t.Helper()
	matrixRow := row("os_source_os_capability_cli", "cli:os capability", "wrapper", capgraph.CapabilityGraphAuthorityRef, "os capability wrapper and compound capability resolve", "")
	matrixRow.SourceKind = "compatibility_cli"
	matrixRow.CurrentRole = "compatibility_wrapper_over_compound_capability_and_router"
	matrixRow.ProofMode = osAbsorptionProofModeOSCapability
	matrixRow.EvidenceRefs = []string{
		routeevidence.CapabilityTaskClassifierAuthorityRef,
		capgraph.CapabilityGraphAuthorityRef,
		routeevidence.ModelCapabilitiesAuthorityRef,
	}
	matrixRow.EvidenceRefs = append(matrixRow.EvidenceRefs, sourceEvidenceRefs...)
	matrixRow.RetirementGate = "os capability performs no independent authority read"
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{SchemaVersion: OSAbsorptionMatrixSchema, Rows: []OSAbsorptionMatrixRow{matrixRow}})
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	report, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 1 {
		t.Fatalf("doctor rows = %+v", report.Rows)
	}
	return report.Rows[0]
}

func osCapabilityFixtureSourceDigestRefs(t *testing.T) []string {
	t.Helper()
	homePath := os.Getenv("HOME")
	paths := []struct {
		prefix string
		path   string
	}{
		{prefix: osCapabilityShimDigestRef, path: filepath.Join(homePath, ".local", "bin", "os")},
		{prefix: osCapabilityEntrypointDigestRef, path: filepath.Join(homePath, "absorbed-os", "exec-lane", "os.ts")},
		{prefix: osCapabilityProjectionDigestRef, path: filepath.Join(homePath, "absorbed-os", "exec-lane", "os-capability-proxy.ts")},
		{prefix: osCapabilityBunDigestRef, path: filepath.Join(homePath, ".bun", "bin", "bun")},
	}
	refs := make([]string, 0, len(paths))
	for _, item := range paths {
		body, err := os.ReadFile(item.path)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, item.prefix+digestBytes(body))
	}
	return refs
}

func osCapabilityResolutionFixture(offerID string) map[string]any {
	return osCapabilityResolutionFixtureForTask(osCapabilityProbeTask, "build-implement", offerID)
}

func osCapabilityResolutionFixtureForTask(task, capabilityClass, offerID string) map[string]any {
	return map[string]any{
		"schema_version":    "compound.capability_resolution.v0.1",
		"status":            "ok",
		"task":              task,
		"privacy_sensitive": false,
		"classification": map[string]any{
			"schema_version":      routeevidence.CapabilityTaskClassifierSchemaVersion,
			"authority_ref":       routeevidence.CapabilityTaskClassifierAuthorityRef,
			"authority_revision":  "sha256:" + repeatHex("b", 64),
			"rule_id":             capabilityClass,
			"capability_class":    capabilityClass,
			"deterministic_first": "reproduce/run tests before LLM code work",
			"evaluation_method":   "tests + diff + runtime verification",
		},
		"capability_surface": map[string]any{
			"schema_version":      capgraph.SurfaceSchemaVersion,
			"authority_ref":       capgraph.CapabilityGraphAuthorityRef,
			"authority_revision":  "sha256:" + repeatHex("c", 12),
			"status":              "ok",
			"task":                task,
			"node_count":          1,
			"edge_count":          0,
			"source_record_count": 1,
			"offer_count":         1,
			"offer_limit":         12,
			"offers": []any{map[string]any{
				"node":  map[string]any{"id": offerID, "kind": "tool", "name": "compound proof-check"},
				"score": 4.2,
				"why":   "fixture",
			}},
			"offer_summaries": []any{},
		},
		"route_plan": map[string]any{
			"schema_version":      routeevidence.CapabilityRoutePlanSchemaVersion,
			"id":                  "crp_fixture",
			"created_at":          "2026-07-11T00:00:00Z",
			"status":              "planned",
			"authority_ref":       routeevidence.ModelCapabilitiesAuthorityRef,
			"authority_path":      "/synthetic/model-capabilities.yaml",
			"source_authority_id": "mrcsrc_aaaaaaaaaaaa",
			"source_revision":     "sha256:" + repeatHex("a", 64),
			"capability_class":    capabilityClass,
			"privacy_sensitive":   false,
			"routing_rule":        "routing_defaults[" + capabilityClass + "]",
			"lane_justification":  []string{"fixture"},
			"chain": []any{map[string]any{
				"position": 0, "model_id": "codex", "lane": "codex", "tier": "premium", "claim_level": "capable", "gate_role": "eligible", "justification": "fixture",
			}},
			"route_lane_limit":          16,
			"policy_omitted_lane_limit": 16,
			"warning_limit":             16,
		},
		"authority_refs": []string{
			routeevidence.CapabilityTaskClassifierAuthorityRef,
			capgraph.CapabilityGraphAuthorityRef,
			routeevidence.ModelCapabilitiesAuthorityRef,
		},
	}
}

func TestParseOSCapabilityResolutionAcceptsAdditiveReorderedAuthorityEvidence(t *testing.T) {
	report := osCapabilityResolutionFixture("tool:compound.proof-check")
	report["authority_refs"] = []string{
		"future://additive-proof",
		routeevidence.ModelCapabilitiesAuthorityRef,
		capgraph.CapabilityGraphAuthorityRef,
		routeevidence.CapabilityTaskClassifierAuthorityRef,
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseOSCapabilityResolution(body, osCapabilityProbeTask, "build-implement"); err != nil {
		t.Fatalf("additive reordered authority evidence should parse: %v", err)
	}
}

func TestParseOSCapabilityResolutionRejectsUnboundNestedAuthorities(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing classifier top ref", mutate: func(report map[string]any) {
			report["authority_refs"] = []string{capgraph.CapabilityGraphAuthorityRef, routeevidence.ModelCapabilitiesAuthorityRef}
		}},
		{name: "missing graph top ref", mutate: func(report map[string]any) {
			report["authority_refs"] = []string{routeevidence.CapabilityTaskClassifierAuthorityRef, routeevidence.ModelCapabilitiesAuthorityRef}
		}},
		{name: "missing router top ref", mutate: func(report map[string]any) {
			report["authority_refs"] = []string{routeevidence.CapabilityTaskClassifierAuthorityRef, capgraph.CapabilityGraphAuthorityRef}
		}},
		{name: "classifier schema", mutate: func(report map[string]any) { report["classification"].(map[string]any)["schema_version"] = "forged" }},
		{name: "classifier ref", mutate: func(report map[string]any) { report["classification"].(map[string]any)["authority_ref"] = "forged" }},
		{name: "classifier revision", mutate: func(report map[string]any) {
			report["classification"].(map[string]any)["authority_revision"] = "sha256:short"
		}},
		{name: "classifier class", mutate: func(report map[string]any) { report["classification"].(map[string]any)["capability_class"] = "" }},
		{name: "classifier rule", mutate: func(report map[string]any) { report["classification"].(map[string]any)["rule_id"] = "" }},
		{name: "graph schema", mutate: func(report map[string]any) {
			report["capability_surface"].(map[string]any)["schema_version"] = "forged"
		}},
		{name: "graph ref", mutate: func(report map[string]any) { report["capability_surface"].(map[string]any)["authority_ref"] = "forged" }},
		{name: "graph revision", mutate: func(report map[string]any) {
			report["capability_surface"].(map[string]any)["authority_revision"] = "sha256:short"
		}},
		{name: "graph status", mutate: func(report map[string]any) { report["capability_surface"].(map[string]any)["status"] = "draft" }},
		{name: "router schema", mutate: func(report map[string]any) { report["route_plan"].(map[string]any)["schema_version"] = "forged" }},
		{name: "router ref", mutate: func(report map[string]any) { report["route_plan"].(map[string]any)["authority_ref"] = "forged" }},
		{name: "router source id", mutate: func(report map[string]any) { report["route_plan"].(map[string]any)["source_authority_id"] = "" }},
		{name: "router source id delimiter", mutate: func(report map[string]any) {
			report["route_plan"].(map[string]any)["source_authority_id"] = "mrcsrc_aaaaaaaaaaaa;projection_sha256:forged"
		}},
		{name: "router source id slash", mutate: func(report map[string]any) {
			report["route_plan"].(map[string]any)["source_authority_id"] = "mrcsrc_aaaaaaaaaaaa/forged"
		}},
		{name: "router source id newline", mutate: func(report map[string]any) {
			report["route_plan"].(map[string]any)["source_authority_id"] = "mrcsrc_aaaaaaaaaaaa\nforged"
		}},
		{name: "router source id wrong prefix", mutate: func(report map[string]any) {
			report["route_plan"].(map[string]any)["source_authority_id"] = "source_aaaaaaaaaaaa"
		}},
		{name: "router source id oversized", mutate: func(report map[string]any) {
			report["route_plan"].(map[string]any)["source_authority_id"] = "mrcsrc_" + strings.Repeat("a", 256)
		}},
		{name: "router revision", mutate: func(report map[string]any) { report["route_plan"].(map[string]any)["source_revision"] = "sha256:short" }},
		{name: "router status", mutate: func(report map[string]any) { report["route_plan"].(map[string]any)["status"] = "draft" }},
		{name: "router class mismatch", mutate: func(report map[string]any) {
			report["route_plan"].(map[string]any)["capability_class"] = "judge-critique"
		}},
		{name: "empty route", mutate: func(report map[string]any) { report["route_plan"].(map[string]any)["chain"] = []any{} }},
		{name: "oversized route", mutate: func(report map[string]any) {
			lane := report["route_plan"].(map[string]any)["chain"].([]any)[0]
			report["route_plan"].(map[string]any)["chain"] = make([]any, 17)
			for index := range report["route_plan"].(map[string]any)["chain"].([]any) {
				report["route_plan"].(map[string]any)["chain"].([]any)[index] = lane
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := osCapabilityResolutionFixture("tool:compound.proof-check")
			tc.mutate(report)
			body, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseOSCapabilityResolution(body, osCapabilityProbeTask, "build-implement"); err == nil {
				t.Fatal("malformed authority binding should fail")
			}
		})
	}
}

func TestRunOSCapabilityCommandActivelyBoundsOutputAndKillsProcessGroup(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "flood")
	childPIDPath := filepath.Join(root, "child.pid")
	writeExecutableFixture(t, script, `#!/bin/sh
sleep 30 &
printf '%s' "$!" > "$PID_FILE"
while :; do
  printf '0123456789abcdef0123456789abcdef'
done
`)

	result, err := runOSCapabilityCommandWithLimits(script, nil, map[string]string{"PID_FILE": childPIDPath}, 3*time.Second, 1024)
	if !errors.Is(err, errOSCapabilityOutputLimit) {
		t.Fatalf("flood command error = %v, want %v", err, errOSCapabilityOutputLimit)
	}
	if got := len(result.stdout) + len(result.stderr); got > 1024 {
		t.Fatalf("retained output = %d bytes, want <= 1024", got)
	}
	pidBody, err := os.ReadFile(childPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBody)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("flood command child process %d survived process-group cancellation: %v", pid, err)
	}
}

func TestRunOSCapabilityCommandLimitsAreActiveAndTimeoutIsDistinct(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		limit     int64
		wantError error
		wantBytes int
	}{
		{name: "exact stdout limit", body: `printf '12345678'`, limit: 8, wantBytes: 8},
		{name: "stdout overflow", body: `printf '123456789'`, limit: 8, wantError: errOSCapabilityOutputLimit, wantBytes: 8},
		{name: "stderr overflow", body: `printf '123456789' >&2`, limit: 8, wantError: errOSCapabilityOutputLimit, wantBytes: 8},
		{name: "combined overflow", body: `printf '12345'; printf '6789' >&2`, limit: 8, wantError: errOSCapabilityOutputLimit, wantBytes: 8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runOSCapabilityCommandWithLimits("/bin/sh", []string{"-c", tc.body}, nil, 3*time.Second, tc.limit)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("error = %v, want %v", err, tc.wantError)
			}
			if got := len(result.stdout) + len(result.stderr); got != tc.wantBytes {
				t.Fatalf("retained output = %d, want %d", got, tc.wantBytes)
			}
		})
	}

	_, err := runOSCapabilityCommandWithLimits("/bin/sh", []string{"-c", "sleep 30"}, nil, 20*time.Millisecond, 1024)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errOSCapabilityOutputLimit) {
		t.Fatalf("timeout error = %v, want deadline exceeded distinct from output limit", err)
	}
}

func TestOSCapabilityExpectedSourceDigestsRequireReviewedExactBytes(t *testing.T) {
	snapshot := osCapabilitySourceSnapshot{
		shim: []byte("shim"), entrypoint: []byte("entrypoint"), projection: []byte("projection"),
	}
	refs := []string{
		osCapabilityShimDigestRef + digestBytes(snapshot.shim),
		osCapabilityEntrypointDigestRef + digestBytes(snapshot.entrypoint),
		osCapabilityProjectionDigestRef + digestBytes(snapshot.projection),
		osCapabilityBunDigestRef + digestBytes([]byte("bun")),
	}
	expected, err := expectedOSCapabilitySourceDigests(refs)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.matchesExpected(expected); err != nil {
		t.Fatalf("reviewed bytes should match: %v", err)
	}

	if _, err := expectedOSCapabilitySourceDigests(refs[:2]); err == nil {
		t.Fatal("missing projection digest must fail")
	}
	if _, err := expectedOSCapabilitySourceDigests(refs[:3]); err == nil {
		t.Fatal("missing Bun digest must fail")
	}
	malformed := append([]string(nil), refs...)
	malformed[0] = osCapabilityShimDigestRef + "short"
	if _, err := expectedOSCapabilitySourceDigests(malformed); err == nil {
		t.Fatal("malformed reviewed digest must fail")
	}
	conflicting := append(append([]string(nil), refs...), osCapabilityShimDigestRef+strings.Repeat("a", 64))
	if _, err := expectedOSCapabilitySourceDigests(conflicting); err == nil {
		t.Fatal("conflicting reviewed digests must fail")
	}
	expected.entrypoint = strings.Repeat("0", 64)
	if err := snapshot.matchesExpected(expected); err == nil {
		t.Fatal("installed bytes that diverge from reviewed digest must fail")
	}
}

func TestOSCapabilityProbeTasksCarryUnpredictableNonceSuffixes(t *testing.T) {
	for _, base := range []string{osCapabilityProbeTask, osCapabilitySecondaryProbeTask} {
		first, err := newOSCapabilityProbeTask(base)
		if err != nil {
			t.Fatal(err)
		}
		second, err := newOSCapabilityProbeTask(base)
		if err != nil {
			t.Fatal(err)
		}
		prefix := base + " nonce="
		if !strings.HasPrefix(first, prefix) || !strings.HasPrefix(second, prefix) {
			t.Fatalf("nonce probes must preserve class-bearing template: %q %q", first, second)
		}
		firstNonce := strings.TrimPrefix(first, prefix)
		secondNonce := strings.TrimPrefix(second, prefix)
		if len(firstNonce) != 32 || !isOSAbsorptionHex(firstNonce) || len(secondNonce) != 32 || !isOSAbsorptionHex(secondNonce) || firstNonce == secondNonce {
			t.Fatalf("nonce suffixes are not independent 128-bit hex values: %q %q", firstNonce, secondNonce)
		}
	}
}

func writeExecutableFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func repeatHex(value string, count int) string {
	out := ""
	for len(out) < count {
		out += value
	}
	return out[:count]
}
