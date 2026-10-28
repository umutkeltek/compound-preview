package authority

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/home"
)

// ---------- pure-function unit tests (no subprocess, no fixture HOME) ----------

func TestExtractTSFunctionBodyFindsBalancedBraces(t *testing.T) {
	source := `const x = 1;
function readCompoundThing(): Thing {
  if (a) { return { b: 1 }; }
  return { c: 2 };
}
function readCompoundOther(): Other {
  return {};
}
`
	body, err := extractTSFunctionBody(source, "function readCompoundThing(): Thing {")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body, "{") || !strings.HasSuffix(strings.TrimRight(body, "\n"), "}") {
		t.Fatalf("unexpected body: %q", body)
	}
	if !strings.Contains(body, "return { c: 2 };") {
		t.Fatalf("body missing expected content: %q", body)
	}
	if strings.Contains(body, "readCompoundOther") {
		t.Fatalf("body over-captured the next function: %q", body)
	}
}

func TestExtractTSFunctionBodyRejectsMissingSignature(t *testing.T) {
	if _, err := extractTSFunctionBody("const x = 1;", "function missing() {"); err == nil {
		t.Fatal("expected error for missing signature")
	}
}

func TestRequireGenuineCompoundCallBeforeStatusAcceptsRealShape(t *testing.T) {
	body := `function readCompoundCapabilityAuthority(): CompoundCapabilityAuthority {
  const base = { authority_ref: COMPOUND_CAPABILITY_AUTHORITY };
  const command = compoundCommand(["capability", "authority", "--json"]);
  if (!command) return { ...base, status: "unavailable" };
  try {
    const text = execFileSync(command.cmd, command.args, {});
    return { ...base, status: records.length ? "ok" : "missing" };
  } catch (err) {
    return { ...base, status: "unavailable" };
  }
}`
	if err := requireGenuineCompoundCallBeforeStatus("readCompoundCapabilityAuthority", body, "compoundCommand(", []string{`status: "ok"`}); err != nil {
		t.Fatalf("expected genuine-call shape to pass: %v", err)
	}
}

func TestRequireGenuineCompoundCallBeforeStatusRejectsFabricatedOK(t *testing.T) {
	body := `function readCompoundCapabilityAuthority(): CompoundCapabilityAuthority {
  const base = { authority_ref: COMPOUND_CAPABILITY_AUTHORITY, status: "ok" };
  const command = compoundCommand(["capability", "authority", "--json"]);
  if (!command) return { ...base, status: "unavailable" };
  const text = execFileSync(command.cmd, command.args, {});
  return { ...base };
}`
	err := requireGenuineCompoundCallBeforeStatus("readCompoundCapabilityAuthority", body, "compoundCommand(", []string{`status: "ok"`})
	if err == nil || !strings.Contains(err.Error(), "fabricated/legacy-resolver marker reintroduced") {
		t.Fatalf("expected fabrication rejection, got %v", err)
	}
}

func TestRequireGenuineCompoundCallBeforeStatusRejectsMissingConstructor(t *testing.T) {
	body := `function readCompoundCapabilityAuthority(): CompoundCapabilityAuthority {
  return { authority_ref: COMPOUND_CAPABILITY_AUTHORITY, status: "ok" };
}`
	err := requireGenuineCompoundCallBeforeStatus("readCompoundCapabilityAuthority", body, "compoundCommand(", []string{`status: "ok"`})
	if err == nil || !strings.Contains(err.Error(), "legacy-resolver marker reintroduced") {
		t.Fatalf("expected missing-constructor rejection, got %v", err)
	}
}

func TestRequireGenuineCompoundCallBeforeStatusRejectsMissingSubprocessCall(t *testing.T) {
	body := `function readCompoundCapabilityAuthority(): CompoundCapabilityAuthority {
  const command = compoundCommand(["capability", "authority", "--json"]);
  return { authority_ref: COMPOUND_CAPABILITY_AUTHORITY, status: "unavailable" };
}`
	err := requireGenuineCompoundCallBeforeStatus("readCompoundCapabilityAuthority", body, "compoundCommand(", []string{`status: "ok"`})
	if err == nil || !strings.Contains(err.Error(), "no genuine compound subprocess call") {
		t.Fatalf("expected missing-subprocess-call rejection, got %v", err)
	}
}

func TestSameOSSkillsOfferIDsMatchesExactOrderedSequence(t *testing.T) {
	direct := []any{map[string]any{"node": map[string]any{"id": "tool:a"}}, map[string]any{"node": map[string]any{"id": "tool:b"}}}
	wrapper := []any{map[string]any{"id": "tool:a"}, map[string]any{"id": "tool:b"}}
	if !sameOSSkillsOfferIDs(direct, wrapper) {
		t.Fatal("expected identical ordered offer id sequences to match")
	}
}

// TestSameOSSkillsOfferIDsAcceptsTruncatedPrefix matches production reality:
// readCompoundCapabilitySurface slices to the first 12 offers while direct
// `compound capability surface` returns the full ranked list, so a real
// wrapper output is legitimately an ordered PREFIX of direct's, not an
// exact-length match.
func TestSameOSSkillsOfferIDsAcceptsTruncatedPrefix(t *testing.T) {
	direct := []any{
		map[string]any{"node": map[string]any{"id": "tool:a"}},
		map[string]any{"node": map[string]any{"id": "tool:b"}},
		map[string]any{"node": map[string]any{"id": "tool:c"}},
	}
	wrapper := []any{map[string]any{"id": "tool:a"}, map[string]any{"id": "tool:b"}}
	if !sameOSSkillsOfferIDs(direct, wrapper) {
		t.Fatal("expected a truncated ordered prefix to match")
	}
}

func TestSameOSSkillsOfferIDsRejectsReordering(t *testing.T) {
	direct := []any{map[string]any{"node": map[string]any{"id": "tool:a"}}, map[string]any{"node": map[string]any{"id": "tool:b"}}}
	wrapper := []any{map[string]any{"id": "tool:b"}, map[string]any{"id": "tool:a"}}
	if sameOSSkillsOfferIDs(direct, wrapper) {
		t.Fatal("expected reordered offer id sequence to be rejected")
	}
}

func TestSameOSSkillsOfferIDsRejectsDivergence(t *testing.T) {
	direct := []any{map[string]any{"node": map[string]any{"id": "tool:a"}}}
	wrapper := []any{map[string]any{"id": "tool:renamed-independent-resolver"}}
	if sameOSSkillsOfferIDs(direct, wrapper) {
		t.Fatal("expected divergent offer id sets to be rejected")
	}
}

func TestSameOSSkillsOfferIDsRejectsEmptyWrapperAgainstNonEmptyDirect(t *testing.T) {
	direct := []any{map[string]any{"node": map[string]any{"id": "tool:a"}}}
	if sameOSSkillsOfferIDs(direct, nil) {
		t.Fatal("expected an empty wrapper offer list to be rejected when direct has offers")
	}
}

func TestOSSkillsDispatchPatternMatchesRealShape(t *testing.T) {
	if !osSkillsDispatchPattern.MatchString(`else if (cmd === "skills") exit = await runSkills(rest);`) {
		t.Fatal("expected dispatch pattern to match real shape")
	}
	if osSkillsDispatchPattern.MatchString(`else if (cmd === "skills-renamed") exit = await runSkills(rest);`) {
		t.Fatal("dispatch pattern should not match a renamed command")
	}
}

func TestOSStatusDispatchPatternMatchesRealShape(t *testing.T) {
	if !osStatusDispatchPattern.MatchString(`if (cmd === "status") exit = await runStatus(rest.includes("--json"));`) {
		t.Fatal("expected dispatch pattern to match real shape")
	}
}

// ---------- synthetic entrypoint fixture (controls $HOME, no subprocess) ----------

// osWrapperFixtureOpts controls specific mutations to the synthetic os.ts
// entrypoint and to the live parity shim scripts, letting each test flip
// exactly one falsifier at a time.
type osWrapperFixtureOpts struct {
	breakSkillsDispatch            bool
	breakStatusDispatch            bool
	skillsSurfaceNoCompoundCommand bool
	skillsSurfaceFabricateOK       bool
	omitStatusReaderSnapshotKey    string
	fabricateStatusReaderKey       string
	omitNativeProbeSig             string

	wrapperOfferID          string
	directOfferID           string
	anchorLiveNodeCount     int
	anchorDirectNodeCount   int
	statusSnapshotOverrides map[string]map[string]any
	skipAbsorbedOSSeed      bool
}

const osWrapperFixtureDefaultOfferID = "tool:compound.proof-check"

func buildReaderFunctionSource(sig, markerField, markerValue string, fabricateOKBeforeCall bool) string {
	var b strings.Builder
	b.WriteString(sig + "\n")
	fmt.Fprintf(&b, "  const base = { %s: %q };\n", markerField, markerValue)
	if fabricateOKBeforeCall {
		b.WriteString("  if (false) return { ...base, status: \"ok\" };\n")
	}
	b.WriteString("  const command = compoundCommand([\"probe\"]);\n")
	b.WriteString("  if (!command) return { ...base, status: \"unavailable\" };\n")
	b.WriteString("  const text = execFileSync(command.cmd, command.args, {});\n")
	b.WriteString("  return { ...base, status: \"ok\" };\n")
	b.WriteString("}\n\n")
	return b.String()
}

func buildOSWrapperEntrypointSource(compoundHome string, opts osWrapperFixtureOpts) string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bun\n")
	b.WriteString("const [cmd, ...rest] = process.argv.slice(2);\nlet exit = 0;\n")

	statusDispatch := `if (cmd === "status") exit = await runStatus(rest.includes("--json"));`
	if opts.breakStatusDispatch {
		statusDispatch = `if (cmd === "status-renamed") exit = await runStatus(rest.includes("--json"));`
	}
	skillsDispatch := `else if (cmd === "skills") exit = await runSkills(rest);`
	if opts.breakSkillsDispatch {
		skillsDispatch = `else if (cmd === "skills-renamed") exit = await runSkills(rest);`
	}
	b.WriteString(statusDispatch + "\n" + skillsDispatch + "\n")
	b.WriteString("process.exit(exit);\n\n")

	b.WriteString(`const compoundSurface: CompoundCapabilitySurface = query
  ? readCompoundCapabilitySurface(query, host)
  : { status: "unavailable" };

function compoundSurfaceCommand(): { cmd: string; args: string[]; cwd?: string } | null {
  return compoundCommand(["capability", "surface", "--json"]);
}

`)
	surfaceSig := `function readCompoundCapabilitySurface(task: string, host: string | null = null): CompoundCapabilitySurface {`
	if opts.skillsSurfaceNoCompoundCommand {
		fmt.Fprintf(&b, "%s\n  return { authority_ref: %q, status: \"ok\" };\n}\n\n", surfaceSig, capgraph.CapabilityGraphAuthorityRef)
	} else {
		var sb strings.Builder
		sb.WriteString(surfaceSig + "\n")
		fmt.Fprintf(&sb, "  const base = { authority_ref: %q };\n", capgraph.CapabilityGraphAuthorityRef)
		if opts.skillsSurfaceFabricateOK {
			sb.WriteString("  if (false) return { ...base, status: \"ok\" };\n")
		}
		sb.WriteString("  const command = compoundSurfaceCommand();\n")
		sb.WriteString("  if (!command) return { ...base, status: \"unavailable\" };\n")
		sb.WriteString("  const text = execFileSync(command.cmd, command.args, {});\n")
		sb.WriteString("  return { ...base, status: \"ok\" };\n")
		sb.WriteString("}\n\n")
		b.WriteString(sb.String())
	}

	for _, section := range osStatusCompoundSections(compoundHome) {
		if section.snapshotKey == opts.omitStatusReaderSnapshotKey {
			continue
		}
		fabricate := section.snapshotKey == opts.fabricateStatusReaderKey
		b.WriteString(buildReaderFunctionSource(section.readerSig, section.markerField, section.markerValue, fabricate))
	}
	for _, probe := range osStatusRetainedNativeProbes {
		if probe == opts.omitNativeProbeSig {
			continue
		}
		b.WriteString(probe + "\n  // native probe body\n}\n\n")
	}
	return b.String()
}

type installedOSWrapperProofFixture struct {
	compoundHome string
	entrypoint   []byte
}

// installOSWrapperProofFixture writes a synthetic os.ts to a temp $HOME
// (structural checks only need this) and, unless the test is purely
// structural, also installs `os`/`compound` shim scripts on $PATH so the
// live parity probe has something real to execute. Tests that expect to
// fail before reaching the subprocess stage may leave the shim fields at
// their zero value; installOSWrapperProofFixture still writes minimal
// (always-exit-2) shims so an accidental fall-through is loud, not a
// silent pass.
func installOSWrapperProofFixture(t *testing.T, opts osWrapperFixtureOpts) installedOSWrapperProofFixture {
	t.Helper()
	root := t.TempDir()
	userHome := filepath.Join(root, "home")
	compoundHome := filepath.Join(root, "compound-home")
	binDir := filepath.Join(root, "bin")
	osRoot := filepath.Join(userHome, "absorbed-os")
	execLane := filepath.Join(osRoot, "exec-lane")
	for _, dir := range []string{filepath.Join(userHome, ".local", "bin"), binDir, execLane} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := home.Init(compoundHome, "test", time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if !opts.skipAbsorbedOSSeed {
		seedAbsorbedOSRegistry(t, compoundHome, osRoot)
	}

	source := buildOSWrapperEntrypointSource(compoundHome, opts)
	writeExecutableFixture(t, filepath.Join(execLane, "os.ts"), source)

	wrapperOfferID := opts.wrapperOfferID
	if wrapperOfferID == "" {
		wrapperOfferID = osWrapperFixtureDefaultOfferID
	}
	directOfferID := opts.directOfferID
	if directOfferID == "" {
		directOfferID = osWrapperFixtureDefaultOfferID
	}
	anchorLiveNodeCount := opts.anchorLiveNodeCount
	if anchorLiveNodeCount == 0 {
		anchorLiveNodeCount = 3
	}
	anchorDirectNodeCount := opts.anchorDirectNodeCount
	if anchorDirectNodeCount == 0 {
		anchorDirectNodeCount = anchorLiveNodeCount
	}

	snapshot := map[string]any{}
	for _, section := range osStatusCompoundSections(compoundHome) {
		entry := map[string]any{section.markerField: section.markerValue, "status": "ok"}
		if section.snapshotKey == "capability_authority" {
			entry["node_count"] = float64(anchorLiveNodeCount)
			entry["edge_count"] = float64(2)
		}
		if override, ok := opts.statusSnapshotOverrides[section.snapshotKey]; ok {
			for k, v := range override {
				entry[k] = v
			}
		}
		snapshot[section.snapshotKey] = entry
	}
	snapshotBody, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(root, "status-snapshot.json")
	if err := os.WriteFile(snapshotPath, snapshotBody, 0o600); err != nil {
		t.Fatal(err)
	}

	osShim := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "skills" ]; then
  TASK="$2"
  cat <<JSON
{"compound_capability_surface":{"status":"ok","authority_ref":"%s","task":"$TASK","offers":[{"id":"%s"}]}}
JSON
  exit 0
fi
if [ "$1" = "status" ]; then
  cat %q
  exit 0
fi
exit 2
`, capgraph.CapabilityGraphAuthorityRef, wrapperOfferID, snapshotPath)
	writeExecutableFixture(t, filepath.Join(userHome, ".local", "bin", "os"), osShim)

	compoundShim := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "capability" ] && [ "$2" = "surface" ]; then
  TASK="$4"
  cat <<JSON
{"status":"ok","task":"$TASK","offers":[{"node":{"id":"%s","kind":"tool","name":"fixture"},"score":1,"why":"fixture"}]}
JSON
  exit 0
fi
if [ "$1" = "capability" ] && [ "$2" = "authority" ]; then
  cat <<JSON
{"authority_ref":"%s","node_count":%d,"edge_count":2,"records":[]}
JSON
  exit 0
fi
exit 2
`, directOfferID, capgraph.CapabilityGraphAuthorityRef, anchorDirectNodeCount)
	writeExecutableFixture(t, filepath.Join(binDir, "compound"), compoundShim)

	t.Setenv("HOME", userHome)
	t.Setenv("PATH", filepath.Join(userHome, ".local", "bin")+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return installedOSWrapperProofFixture{compoundHome: compoundHome, entrypoint: []byte(source)}
}

func osWrapperFixtureEntrypointDigestRefs(t *testing.T, fixture installedOSWrapperProofFixture) (skills, status string) {
	t.Helper()
	digest := digestBytes(fixture.entrypoint)
	return osSkillsEntrypointDigestRef + digest, osStatusEntrypointDigestRef + digest
}

func doctorOSSkillsRow(t *testing.T, fixture installedOSWrapperProofFixture, evidenceRefs []string) OSAbsorptionDoctorRow {
	t.Helper()
	matrixRow := row("os_source_os_skills_cli", "cli:os skills", "wrapper", capgraph.CapabilityGraphAuthorityRef, "os skills wrapper and compound capability surface", "")
	matrixRow.SourceKind = "compatibility_cli"
	matrixRow.CurrentRole = "compatibility_wrapper_with_proven_capability_surface_parity_and_retained_native_skill_corpus_scope"
	matrixRow.ProofMode = osAbsorptionProofModeOSSkills
	matrixRow.EvidenceRefs = evidenceRefs
	matrixRow.RetirementGate = "os skills capability_surface section resolves only through compound; skill corpus fields remain retained-native scope"
	return runOSAbsorptionDoctorSingleRow(t, fixture.compoundHome, matrixRow)
}

func doctorOSStatusRow(t *testing.T, fixture installedOSWrapperProofFixture, evidenceRefs []string) OSAbsorptionDoctorRow {
	t.Helper()
	matrixRow := row("os_source_os_status_cli", "cli:os status", "wrapper", osAbsorptionSystemStatusAuthorityRef, "os status wrapper and compound projections", "")
	matrixRow.SourceKind = "compatibility_cli"
	matrixRow.CurrentRole = "compatibility_wrapper_proxying_compound_authority_projections_with_retained_native_operational_probes"
	matrixRow.ProofMode = osAbsorptionProofModeOSStatus
	matrixRow.EvidenceRefs = evidenceRefs
	matrixRow.RetirementGate = "os status compound-authority sections resolve only through compound; daemon/switchback/ledger/capacity telemetry remain retained-native scope"
	return runOSAbsorptionDoctorSingleRow(t, fixture.compoundHome, matrixRow)
}

func runOSAbsorptionDoctorSingleRow(t *testing.T, homePath string, matrixRow OSAbsorptionMatrixRow) OSAbsorptionDoctorRow {
	t.Helper()
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{SchemaVersion: OSAbsorptionMatrixSchema, Rows: []OSAbsorptionMatrixRow{matrixRow}})
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)); err != nil {
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

// ---------- os_source_os_skills_cli doctor-level tests ----------

func TestOSAbsorptionOSSkillsProofBindsCompoundOnlySurface(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{})
	skillsRef, _ := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSSkillsRow(t, fixture, []string{capgraph.CapabilityGraphAuthorityRef, skillsRef})
	if got.ProofStatus != "ok" {
		t.Fatalf("proof_status = %q, detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofRef, osAbsorptionProofModeOSSkills) {
		t.Fatalf("proof_ref = %q, want prefix %q", got.ProofRef, osAbsorptionProofModeOSSkills)
	}
}

// TestOSAbsorptionOSSkillsProofSkipsWhenNoAbsorbedOSConfigured is the
// public-clone falsifier: real entrypoint/shim files exist on disk, but the
// absorbed_os path-authority registry was never seeded into mem, so the
// resolver reports found=false and the proof must skip, not fail or
// fabricate ok.
func TestOSAbsorptionOSSkillsProofSkipsWhenNoAbsorbedOSConfigured(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{skipAbsorbedOSSeed: true})
	got := doctorOSSkillsRow(t, fixture, []string{capgraph.CapabilityGraphAuthorityRef})
	if got.ProofStatus != "skipped" {
		t.Fatalf("proof_status = %q, want skipped; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "no absorbed-OS root declared") {
		t.Fatalf("proof_detail = %q, want missing absorbed-OS root explanation", got.ProofDetail)
	}
}

func TestOSAbsorptionOSSkillsProofDetectsEntrypointDrift(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{})
	skillsRef, _ := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	// Mutate the live entrypoint AFTER computing the reviewed digest — this
	// is drift: the row still names a real (once-true) digest, but the live
	// file has moved. Must be "pending", never a silent "ok" and never a
	// hard "failed".
	homePath, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	entrypointPath := filepath.Join(homePath, "absorbed-os", "exec-lane", "os.ts")
	f, err := os.OpenFile(entrypointPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n// mutated after review\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got := doctorOSSkillsRow(t, fixture, []string{capgraph.CapabilityGraphAuthorityRef, skillsRef})
	if got.ProofStatus != "pending" {
		t.Fatalf("proof_status = %q, want pending; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "wrapper drift") {
		t.Fatalf("proof_detail = %q, want wrapper drift explanation", got.ProofDetail)
	}
}

func TestOSAbsorptionOSSkillsProofRejectsFabricatedSurfaceReader(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{skillsSurfaceFabricateOK: true})
	skillsRef, _ := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSSkillsRow(t, fixture, []string{capgraph.CapabilityGraphAuthorityRef, skillsRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "fabricated/legacy-resolver marker reintroduced") {
		t.Fatalf("proof_detail = %q, want fabrication rejection", got.ProofDetail)
	}
}

func TestOSAbsorptionOSSkillsProofRejectsMissingCompoundCommandConstructor(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{skillsSurfaceNoCompoundCommand: true})
	skillsRef, _ := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSSkillsRow(t, fixture, []string{capgraph.CapabilityGraphAuthorityRef, skillsRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
}

func TestOSAbsorptionOSSkillsProofRejectsBrokenDispatch(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{breakSkillsDispatch: true})
	skillsRef, _ := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSSkillsRow(t, fixture, []string{capgraph.CapabilityGraphAuthorityRef, skillsRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "dispatch") {
		t.Fatalf("proof_detail = %q, want dispatch rejection", got.ProofDetail)
	}
}

func TestOSAbsorptionOSSkillsProofRejectsDivergentOffers(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{
		wrapperOfferID: "tool:compound.proof-check",
		directOfferID:  "tool:renamed-independent-resolver",
	})
	skillsRef, _ := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSSkillsRow(t, fixture, []string{capgraph.CapabilityGraphAuthorityRef, skillsRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "diverge") {
		t.Fatalf("proof_detail = %q, want offer divergence rejection", got.ProofDetail)
	}
}

// ---------- os_source_os_status_cli doctor-level tests ----------

func TestOSAbsorptionOSStatusProofBindsFifteenCompoundSections(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{})
	_, statusRef := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSStatusRow(t, fixture, []string{statusRef})
	if got.ProofStatus != "ok" {
		t.Fatalf("proof_status = %q, detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofRef, fmt.Sprintf("compound_sections=%d", len(osStatusCompoundSections(fixture.compoundHome)))) {
		t.Fatalf("proof_ref = %q, want compound_sections count", got.ProofRef)
	}
}

// TestOSAbsorptionOSStatusProofSkipsWhenNoAbsorbedOSConfigured is the
// public-clone falsifier for the status wrapper proof, mirroring the skills
// one above.
func TestOSAbsorptionOSStatusProofSkipsWhenNoAbsorbedOSConfigured(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{skipAbsorbedOSSeed: true})
	got := doctorOSStatusRow(t, fixture, nil)
	if got.ProofStatus != "skipped" {
		t.Fatalf("proof_status = %q, want skipped; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "no absorbed-OS root declared") {
		t.Fatalf("proof_detail = %q, want missing absorbed-OS root explanation", got.ProofDetail)
	}
}

func TestOSAbsorptionOSStatusProofDetectsEntrypointDrift(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{})
	_, statusRef := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	homePath, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	entrypointPath := filepath.Join(homePath, "absorbed-os", "exec-lane", "os.ts")
	f, err := os.OpenFile(entrypointPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n// mutated after review\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got := doctorOSStatusRow(t, fixture, []string{statusRef})
	if got.ProofStatus != "pending" {
		t.Fatalf("proof_status = %q, want pending; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "wrapper drift") {
		t.Fatalf("proof_detail = %q, want wrapper drift explanation", got.ProofDetail)
	}
}

func TestOSAbsorptionOSStatusProofRejectsFabricatedReader(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{fabricateStatusReaderKey: "job_roster"})
	_, statusRef := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSStatusRow(t, fixture, []string{statusRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "readCompoundJobRosterAuthority") {
		t.Fatalf("proof_detail = %q, want the fabricated reader named", got.ProofDetail)
	}
}

func TestOSAbsorptionOSStatusProofRejectsMissingReader(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{omitStatusReaderSnapshotKey: "standing_grants"})
	_, statusRef := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSStatusRow(t, fixture, []string{statusRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "readCompoundStandingGrantsAuthority") {
		t.Fatalf("proof_detail = %q, want the missing reader named", got.ProofDetail)
	}
}

func TestOSAbsorptionOSStatusProofRejectsMissingRetainedNativeProbe(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{omitNativeProbeSig: osStatusRetainedNativeProbes[0]})
	_, statusRef := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSStatusRow(t, fixture, []string{statusRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "retained-native probe missing") {
		t.Fatalf("proof_detail = %q, want retained-native probe rejection", got.ProofDetail)
	}
}

func TestOSAbsorptionOSStatusProofRejectsBrokenDispatch(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{breakStatusDispatch: true})
	_, statusRef := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSStatusRow(t, fixture, []string{statusRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
}

func TestOSAbsorptionOSStatusProofRejectsAnchorMismatch(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{anchorLiveNodeCount: 3, anchorDirectNodeCount: 9})
	_, statusRef := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSStatusRow(t, fixture, []string{statusRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "diverge") {
		t.Fatalf("proof_detail = %q, want anchor divergence rejection", got.ProofDetail)
	}
}

func TestOSAbsorptionOSStatusProofRejectsFabricatedProvenanceMarker(t *testing.T) {
	fixture := installOSWrapperProofFixture(t, osWrapperFixtureOpts{
		statusSnapshotOverrides: map[string]map[string]any{
			"job_roster": {"authority_ref": "compound://registries/forged-job-roster"},
		},
	})
	_, statusRef := osWrapperFixtureEntrypointDigestRefs(t, fixture)
	got := doctorOSStatusRow(t, fixture, []string{statusRef})
	if got.ProofStatus != "failed" {
		t.Fatalf("proof_status = %q, want failed; detail = %q", got.ProofStatus, got.ProofDetail)
	}
	if !strings.Contains(got.ProofDetail, "possible fabrication or drift") {
		t.Fatalf("proof_detail = %q, want fabricated provenance marker rejection", got.ProofDetail)
	}
}

// ---------- real-environment integration check (skips if unavailable) ----------

// TestOSAbsorptionOSWrapperProofsAgainstRealEnvironment runs both proofs
// against THIS machine's real installed `os`/`compound` binaries and
// whatever absorbed-OS root (if any) this COMPOUND_HOME has declared via
// `compound authority path-registry`, using freshly computed digests (never
// asserting a baked-in expectation about live counts, which legitimately
// drift). It skips cleanly when the real tools are not on PATH (CI/other
// machines) or when this machine has not declared an absorbed-OS root — the
// same public-clone skip the production proofs themselves report — and is
// the one place this suite actually exercises the live parity probe against
// production data rather than a fixture.
//
// Gated on COMPOUND_TEST_REAL_ENV=1, not testing.Short(): this is not a
// "slow test" trimmed off a quick inner loop, it is a check that proves the
// OPERATOR'S environment is healthy (shells out to the real `os skills` /
// `os status` CLI and the real `compound` binary against the real, populated
// COMPOUND_HOME, 4 subprocess invocations at a 20s timeout each) living
// inside a suite that is supposed to prove a DIFF is correct — the same
// defect this project fixes elsewhere. The canonical gate
// (`go test -count=1 -timeout=40m ./...`) never passes -short, so a -short
// guard here would be decorative, exactly like the neighbouring
// TestOSAbsorption* real-environment checks in this package that already
// carry one and still run on every default-path invocation.
func TestOSAbsorptionOSWrapperProofsAgainstRealEnvironment(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_REAL_ENV") != "1" {
		t.Skip("probes this machine's real installed `os`/`compound` binaries and real COMPOUND_HOME; set COMPOUND_TEST_REAL_ENV=1 to opt in")
	}
	if _, err := exec.LookPath("os"); err != nil {
		t.Skip("real `os` binary not on PATH; skipping real-environment check")
	}
	if _, err := exec.LookPath("compound"); err != nil {
		t.Skip("real `compound` binary not on PATH; skipping real-environment check")
	}
	homePath, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	// Read-only against the REAL, already-populated COMPOUND_HOME (never a
	// fresh temp home): the parity probe needs a real capability graph to
	// surface real offers against, and this proof never writes to homePath,
	// so pointing at production state here is safe and is the only way this
	// check is meaningful.
	compoundHome := os.Getenv("COMPOUND_HOME")
	if compoundHome == "" {
		compoundHome = filepath.Join(homePath, ".compound")
	}
	if _, err := os.Stat(compoundHome); err != nil {
		t.Skipf("real COMPOUND_HOME not present at %s; skipping", compoundHome)
	}

	entrypointPath, found, err := ResolveAbsorbedOSEntrypoint(compoundHome, AbsorbedOSExecLaneEntrypoint)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Skip("no absorbed-OS root declared in this COMPOUND_HOME; skipping real-environment check")
	}
	body, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := digestBytes(body)

	skillsRow := row("os_source_os_skills_cli", "cli:os skills", "wrapper", capgraph.CapabilityGraphAuthorityRef, "os skills wrapper and compound capability surface", osAbsorptionProofModeOSSkills)
	skillsRow.SourceKind = "compatibility_cli"
	skillsRow.EvidenceRefs = []string{capgraph.CapabilityGraphAuthorityRef, osSkillsEntrypointDigestRef + digest}
	skillsRow.RetirementGate = "os skills capability_surface section resolves only through compound"
	skillsResult, skillsDrift := osAbsorptionOSSkillsProof(compoundHome, skillsRow)
	t.Logf("real os_source_os_skills_cli proof: ok=%v drift=%v reason=%q proof_ref=%q", skillsResult.ok, skillsDrift, skillsResult.reason, skillsResult.proofRef)

	statusRow := row("os_source_os_status_cli", "cli:os status", "wrapper", osAbsorptionSystemStatusAuthorityRef, "os status wrapper and compound projections", osAbsorptionProofModeOSStatus)
	statusRow.SourceKind = "compatibility_cli"
	statusRow.EvidenceRefs = []string{osStatusEntrypointDigestRef + digest}
	statusRow.RetirementGate = "os status compound-authority sections resolve only through compound"
	statusResult, statusDrift := osAbsorptionOSStatusProof(compoundHome, statusRow)
	t.Logf("real os_source_os_status_cli proof: ok=%v drift=%v reason=%q proof_ref=%q", statusResult.ok, statusDrift, statusResult.reason, statusResult.proofRef)
}
