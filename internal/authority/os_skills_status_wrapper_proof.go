package authority

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/policy"
)

// This file proves the os_source_os_skills_cli and os_source_os_status_cli
// matrix rows honestly. Both `os skills` and `os status` are dashboards
// (unlike `os capability`, a pure zero-native-logic proxy) that blend
// genuine live compound authority reads with retained-native operational
// probes: skill-corpus registries for `os skills`; daemon/launchd,
// switchback, token-ledger, and CodexBar-capacity telemetry for `os status`.
// compound does not and should not own those native reads (they are local
// operational/telemetry state), so this proof does not require their
// absence. It proves only the compound-sourced slice is real:
//
//  1. digest-anchored wrapper file: the live exec-lane/os.ts entrypoint
//     hashes to the reviewed evidence digest (a mismatch is wrapper DRIFT →
//     proof_status=pending, never a silent ok and never a hard failure —
//     mirrors infraRegistryProof's drift handling).
//  2. forbidden legacy-resolver marker absent: each compound-reading
//     function must construct its subprocess via compoundCommand(...) and
//     invoke it (execFileSync/spawnSync on command.cmd) BEFORE it can ever
//     claim a literal `status: "ok"`. A hardcoded/fabricated "ok" appearing
//     before the genuine subprocess call is treated as a reintroduced
//     legacy-resolver marker → proof_status=failed.
//  3. parity probe: the wrapper is actually executed (not just statically
//     analyzed) and its compound-sourced output is cross-checked against a
//     direct `compound` binary invocation for the same input.

const (
	osAbsorptionSystemStatusAuthorityRef = "compound://projections/system-status"

	osSkillsEntrypointDigestRef = "os_skills_entrypoint_sha256:"
	osStatusEntrypointDigestRef = "os_status_entrypoint_sha256:"
)

var (
	osSkillsDispatchPattern = regexp.MustCompile(`\bcmd\s*===\s*["']skills["']`)
	osStatusDispatchPattern = regexp.MustCompile(`\bcmd\s*===\s*["']status["']`)
)

// readOSWrapperEntrypoint reads the live, canonical os.ts entrypoint shared
// by every `os` subcommand (capability, skills, status, ...). It does not
// re-derive it from the installed shim (os_capability_wrapper_authority
// already proves the shim -> entrypoint wiring on the sibling
// os_source_os_capability_cli row); it resolves the compound-owned
// exec-lane/os.ts sub-path beneath the operator-declared absorbed-OS root
// (ResolveAbsorbedOSEntrypoint), bounded and snapshot-verified like every
// other proof read in this package. found=false means no absorbed-OS root
// is configured (or the declared root is absent on disk) — the caller must
// skip, not fail.
func readOSWrapperEntrypoint(compoundHome string) (path string, body []byte, found bool, err error) {
	path, found, err = ResolveAbsorbedOSEntrypoint(compoundHome, AbsorbedOSExecLaneEntrypoint)
	if err != nil {
		return "", nil, false, err
	}
	if !found {
		return "", nil, false, nil
	}
	body, err = readRegularBounded(path, maxOSCapabilityEntrypointBytes)
	if err != nil {
		return "", nil, false, err
	}
	return path, body, true, nil
}

// extractTSFunctionBody returns the full `function ... { ... }` text for the
// given exact signature (including its trailing `{`), by brace-counting from
// the first `{` after the signature to its matching close. It is a
// pragmatic scanner (no string/template-literal awareness) sufficient for
// the well-formed, reviewed TypeScript functions this proof inspects — the
// same "whole source, literal substrings" style already used throughout
// os_capability_wrapper_proof.go.
func extractTSFunctionBody(source, signature string) (string, error) {
	idx := strings.Index(source, signature)
	if idx < 0 {
		return "", fmt.Errorf("function signature not found: %s", signature)
	}
	braceRel := strings.Index(source[idx:], "{")
	if braceRel < 0 {
		return "", fmt.Errorf("function signature has no body: %s", signature)
	}
	start := idx + braceRel
	depth := 0
	for i := start; i < len(source); i++ {
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced function body for signature: %s", signature)
}

// requireGenuineCompoundCallBeforeStatus is the falsifier for a reintroduced
// legacy resolver: it requires the function body to construct its command
// via the given constructorMarker (e.g. "compoundCommand(" for the fifteen
// os status readers that call it directly, or "compoundSurfaceCommand("
// for readCompoundCapabilitySurface, which goes through that one-line
// wrapper instead) and to actually invoke it (execFileSync or spawnSync on
// command.cmd) before any of the given status-literal markers (e.g.
// `status: "ok"`) can appear. A marker appearing earlier in the body than
// the genuine subprocess call means the function can claim success without
// ever having asked compound — exactly the fabrication shape this proof
// exists to catch.
func requireGenuineCompoundCallBeforeStatus(label, body, constructorMarker string, statusMarkers []string) error {
	if !strings.Contains(body, constructorMarker) {
		return fmt.Errorf("%s does not construct its command via %s...) (legacy-resolver marker reintroduced)", label, constructorMarker)
	}
	callIdx := -1
	for _, marker := range []string{"execFileSync(command.cmd", "spawnSync(command.cmd"} {
		if i := strings.Index(body, marker); i >= 0 && (callIdx == -1 || i < callIdx) {
			callIdx = i
		}
	}
	if callIdx < 0 {
		return fmt.Errorf("%s has no genuine compound subprocess call (execFileSync/spawnSync on command.cmd) (legacy-resolver marker reintroduced)", label)
	}
	for _, marker := range statusMarkers {
		if i := strings.Index(body, marker); i >= 0 && i < callIdx {
			return fmt.Errorf("%s claims %q before invoking the compound subprocess (fabricated/legacy-resolver marker reintroduced)", label, marker)
		}
	}
	return nil
}

// ---------- os_source_os_skills_cli ----------

func osAbsorptionNeedsOSSkillsProof(row OSAbsorptionMatrixRow) bool {
	tupleMatches := row.ID == "os_source_os_skills_cli" &&
		row.SourceRef == "cli:os skills" &&
		row.SourceKind == "compatibility_cli" &&
		row.CompoundOwner == capgraph.CapabilityGraphAuthorityRef &&
		row.Status == "wrapper"
	if row.ProofMode != "" {
		return row.ProofMode == osAbsorptionProofModeOSSkills && tupleMatches
	}
	return tupleMatches
}

func osAbsorptionOSSkillsProof(homePath string, row OSAbsorptionMatrixRow) (osAbsorptionProofResult, bool) {
	path, body, found, err := readOSWrapperEntrypoint(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "os skills entrypoint: " + err.Error()}, false
	}
	if !found {
		return osAbsorptionProofResult{skipped: true, reason: osAbsorptionAbsorbedOSSkipReason}, false
	}
	if !hasExactString(row.EvidenceRefs, capgraph.CapabilityGraphAuthorityRef) {
		return osAbsorptionProofResult{reason: "os skills wrapper requires capability graph authority evidence ref"}, false
	}
	expectedDigest, err := exactOSCapabilityDigestEvidence(row.EvidenceRefs, osSkillsEntrypointDigestRef)
	if err != nil {
		return osAbsorptionProofResult{reason: "os skills expected entrypoint binding: " + err.Error()}, false
	}
	liveDigest := digestBytes(body)
	if liveDigest != expectedDigest {
		return osAbsorptionProofResult{reason: fmt.Sprintf(
			"os skills entrypoint %s sha256 %s does not match reviewed %s (wrapper drift; re-review and re-import)",
			path, liveDigest, expectedDigest,
		)}, true
	}
	text := string(body)
	if !osSkillsDispatchPattern.MatchString(text) || !strings.Contains(text, `else if (cmd === "skills") exit = await runSkills(rest);`) {
		return osAbsorptionProofResult{reason: "os skills dispatch no longer reaches runSkills"}, false
	}
	const surfaceReaderSig = `function readCompoundCapabilitySurface(task: string, host: string | null = null): CompoundCapabilitySurface {`
	for _, want := range []string{
		`const compoundSurface: CompoundCapabilitySurface = query`,
		`? readCompoundCapabilitySurface(query, host)`,
		surfaceReaderSig,
		`function compoundSurfaceCommand(): { cmd: string; args: string[]; cwd?: string } | null {`,
		`return compoundCommand(["capability", "surface", "--json"]);`,
	} {
		if !strings.Contains(text, want) {
			return osAbsorptionProofResult{reason: fmt.Sprintf("os skills missing required compound-surface wiring %q", want)}, false
		}
	}
	surfaceBody, err := extractTSFunctionBody(text, surfaceReaderSig)
	if err != nil {
		return osAbsorptionProofResult{reason: "os skills capability-surface reader: " + err.Error()}, false
	}
	if err := requireGenuineCompoundCallBeforeStatus("readCompoundCapabilitySurface", surfaceBody, "compoundSurfaceCommand(", []string{`status: "ok"`}); err != nil {
		return osAbsorptionProofResult{reason: err.Error()}, false
	}

	// Parity probe: actually run the wrapper and cross-check against a
	// direct compound invocation for the same nonce-suffixed task.
	osPath, err := exec.LookPath("os")
	if err != nil {
		return osAbsorptionProofResult{reason: "resolve installed os binary: " + err.Error()}, false
	}
	compoundBinary, err := resolveOSCapabilityBinary()
	if err != nil {
		return osAbsorptionProofResult{reason: "resolve installed compound binary: " + err.Error()}, false
	}
	probeTask, err := newOSCapabilityProbeTask(osCapabilityProbeTask)
	if err != nil {
		return osAbsorptionProofResult{reason: "generate os skills probe nonce: " + err.Error()}, false
	}

	wrapper, err := runOSCapabilityCommand(osPath, []string{"skills", probeTask, "--json"}, map[string]string{"COMPOUND_HOME": homePath})
	if err != nil || wrapper.exitCode != 0 {
		return osAbsorptionProofResult{reason: commandFailure("os skills", wrapper, err)}, false
	}
	var wrapperOut map[string]any
	if err := json.Unmarshal(wrapper.stdout, &wrapperOut); err != nil {
		return osAbsorptionProofResult{reason: "os skills output: " + err.Error()}, false
	}
	surface, ok := wrapperOut["compound_capability_surface"].(map[string]any)
	if !ok {
		return osAbsorptionProofResult{reason: "os skills output missing compound_capability_surface"}, false
	}
	if stringField(surface, "status") != "ok" {
		return osAbsorptionProofResult{reason: fmt.Sprintf("os skills compound_capability_surface status=%q, want ok", stringField(surface, "status"))}, false
	}
	if stringField(surface, "authority_ref") != capgraph.CapabilityGraphAuthorityRef {
		return osAbsorptionProofResult{reason: "os skills compound_capability_surface authority_ref does not match compound://registries/capability-graph"}, false
	}
	if stringField(surface, "task") != probeTask {
		return osAbsorptionProofResult{reason: "os skills compound_capability_surface task does not echo the probe nonce"}, false
	}
	wrapperOffers, _ := surface["offers"].([]any)

	direct, err := runOSCapabilityCommand(compoundBinary.path, []string{"capability", "surface", "--json", probeTask}, map[string]string{"COMPOUND_HOME": homePath})
	if err != nil || direct.exitCode != 0 {
		return osAbsorptionProofResult{reason: commandFailure("compound capability surface", direct, err)}, false
	}
	var directOut map[string]any
	if err := json.Unmarshal(direct.stdout, &directOut); err != nil {
		return osAbsorptionProofResult{reason: "compound capability surface output: " + err.Error()}, false
	}
	if stringField(directOut, "status") != "ok" || stringField(directOut, "task") != probeTask {
		return osAbsorptionProofResult{reason: "compound capability surface direct probe did not resolve the nonce task"}, false
	}
	directOffers, _ := directOut["offers"].([]any)
	if len(directOffers) == 0 {
		return osAbsorptionProofResult{reason: "os skills parity probe task produced no direct compound offers to compare"}, false
	}
	if !sameOSSkillsOfferIDs(directOffers, wrapperOffers) {
		return osAbsorptionProofResult{reason: "os skills capability surface offers diverge from direct compound capability surface"}, false
	}

	proofRef := fmt.Sprintf(
		"%s:entrypoint_sha256=%s;compound_authority=%s;offers=%d;compound_sha256=%s",
		osAbsorptionProofModeOSSkills, liveDigest, capgraph.CapabilityGraphAuthorityRef, len(wrapperOffers), compoundBinary.digest,
	)
	return osAbsorptionProofResult{ok: true, proofRef: proofRef}, false
}

// sameOSSkillsOfferIDs compares the direct `compound capability surface`
// offer set (offers[].node.id) against the `os skills` wrapper's already
// projected offer set (offers[].id) for the same probe task. Equal sets
// (including both empty) prove the wrapper reproduces exactly what compound
// returned rather than fabricating or silently dropping/adding entries.
// sameOSSkillsOfferIDs proves the wrapper's offer sequence is an exact,
// order-preserving PREFIX of the direct compound offer sequence — not an
// exact-set match. The wrapper deliberately bounds what it echoes
// (`offers.slice(0, 12)` in readCompoundCapabilitySurface, mirroring
// maxOSCapabilityOffers in the sibling os_capability proof) while direct
// `compound capability surface` returns the full ranked list; an exact-set
// comparison would spuriously fail every real (non-trivial) task where more
// than 12 nodes match. A prefix mismatch, a reordering, or an empty wrapper
// against a non-empty direct list still fails the probe.
func sameOSSkillsOfferIDs(direct, wrapper []any) bool {
	directIDs := osSkillsOfferIDSequence(direct)
	wrapperIDs := osSkillsOfferIDSequence(wrapper)
	if len(wrapperIDs) == 0 || len(wrapperIDs) > len(directIDs) {
		return false
	}
	for i, id := range wrapperIDs {
		if directIDs[i] != id {
			return false
		}
	}
	return true
}

func osSkillsOfferIDSequence(offers []any) []string {
	ids := make([]string, 0, len(offers))
	for _, item := range offers {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := ""
		if node, ok := m["node"].(map[string]any); ok {
			id = stringField(node, "id")
		}
		if id == "" {
			id = stringField(m, "id")
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// ---------- os_source_os_status_cli ----------

// osStatusCompoundSection pins one runStatus() snapshot key to the reader
// function that populates it, and to the provenance marker the doctor
// checks on every live run. Most sections carry an authority_ref field;
// review_queue/workbench instead carry a COMPOUND_HOME-scoped root path
// (review_root/work_root) because their TS types never declare
// authority_ref at all.
type osStatusCompoundSection struct {
	label       string
	snapshotKey string
	readerSig   string
	markerField string
	markerValue string
	requireOK   bool
}

func osStatusCompoundSections(homePath string) []osStatusCompoundSection {
	return []osStatusCompoundSection{
		{
			label:       "readCompoundCapabilityAuthority",
			snapshotKey: "capability_authority",
			readerSig:   `function readCompoundCapabilityAuthority(): CompoundCapabilityAuthority {`,
			markerField: "authority_ref",
			markerValue: capgraph.CapabilityGraphAuthorityRef,
			requireOK:   true,
		},
		{
			label:       "readCompoundRouteEvidenceAuthority",
			snapshotKey: "route_evidence",
			readerSig:   `function readCompoundRouteEvidenceAuthority(): CompoundRouteEvidenceAuthority {`,
			markerField: "authority_ref",
			markerValue: "compound://ledger/execution-route-evidence",
		},
		{
			label:       "readCompoundEventTraceAuthority",
			snapshotKey: "event_trace_authority",
			readerSig:   `function readCompoundEventTraceAuthority(): CompoundEventTraceAuthority {`,
			markerField: "authority_ref",
			markerValue: "compound://events/trace-envelopes",
		},
		{
			label:       "readCompoundToolBoundaryDoctor",
			snapshotKey: "tool_boundary_doctor",
			readerSig:   `function readCompoundToolBoundaryDoctor(): CompoundToolBoundaryDoctor {`,
			markerField: "authority_ref",
			markerValue: capgraph.CapabilityGraphAuthorityRef,
		},
		{
			label:       "readCompoundExternalRepoIntake",
			snapshotKey: "external_repo_intake",
			readerSig:   `function readCompoundExternalRepoIntake(repoQuery = ""): CompoundExternalRepoIntake {`,
			markerField: "authority_ref",
			markerValue: "compound://reviews/external-repo-intake",
		},
		{
			label:       "readCompoundSkillSourceDoctor",
			snapshotKey: "skill_source_doctor",
			readerSig:   `function readCompoundSkillSourceDoctor(): CompoundSkillSourceDoctor {`,
			markerField: "authority_ref",
			markerValue: "compound://registries/capability-graph#tool:compound.skill-source-doctor",
		},
		{
			label:       "readCompoundReviewQueue",
			snapshotKey: "review_queue",
			readerSig:   `function readCompoundReviewQueue(): CompoundReviewQueue {`,
			markerField: "review_root",
			markerValue: filepath.Join(homePath, "reviews"),
		},
		{
			label:       "readCompoundProductAuthority",
			snapshotKey: "product_authority",
			readerSig:   `function readCompoundProductAuthority(): CompoundProductAuthority {`,
			markerField: "authority_ref",
			markerValue: "compound://ledger/product-feedback",
		},
		{
			label:       "readCompoundWorkbench",
			snapshotKey: "workbench",
			readerSig:   `function readCompoundWorkbench(): CompoundWorkbench {`,
			markerField: "work_root",
			markerValue: filepath.Join(homePath, "work"),
		},
		{
			label:       "readCompoundReleaseGoalHoldsAuthority",
			snapshotKey: "release_goal_holds",
			readerSig:   `function readCompoundReleaseGoalHoldsAuthority(): CompoundReleaseGoalHoldsAuthority {`,
			markerField: "authority_ref",
			markerValue: "compound://registries/release-goal-holds",
		},
		{
			label:       "readCompoundOperatorAuthority",
			snapshotKey: "operator_state",
			readerSig:   `function readCompoundOperatorAuthority(): CompoundOperatorAuthority {`,
			markerField: "authority_ref",
			markerValue: policy.OperatorStateAuthorityRef,
		},
		{
			label:       "readCompoundPlacementAuthority",
			snapshotKey: "placement_rules",
			readerSig:   `function readCompoundPlacementAuthority(): CompoundPlacementAuthority {`,
			markerField: "authority_ref",
			markerValue: "compound://registries/placement-rules",
		},
		{
			label:       "readCompoundStandingGrantsAuthority",
			snapshotKey: "standing_grants",
			readerSig:   `function readCompoundStandingGrantsAuthority(): CompoundStandingGrantsAuthority {`,
			markerField: "authority_ref",
			markerValue: "compound://registries/standing-grants",
		},
		{
			label:       "readCompoundJobRosterAuthority",
			snapshotKey: "job_roster",
			readerSig:   `function readCompoundJobRosterAuthority(): CompoundJobRosterAuthority {`,
			markerField: "authority_ref",
			markerValue: JobRosterAuthorityRef,
		},
		{
			label:       "readCompoundMemoryDoctor",
			snapshotKey: "memory_engine_doctor",
			readerSig:   `function readCompoundMemoryDoctor(): CompoundMemoryDoctor {`,
			markerField: "authority_ref",
			markerValue: "compound://memory/doctor",
		},
	}
}

// osStatusRetainedNativeProbes are the operational/telemetry probe
// functions the os_source_os_status_cli row names as retained-native scope
// (daemon/launchd, switchback, local token ledger, CodexBar capacity).
// compound does not own this operational state; this proof only asserts
// they are still present (not silently swapped for a fabricated
// compound-sourced stand-in) so the row's retained-native claim stays true.
var osStatusRetainedNativeProbes = []string{
	`function probeDaemons(lines: Line[], releaseGoalHolds: CompoundReleaseGoalHoldsAuthority) {`,
	`async function probeSwitchback(lines: Line[]) {`,
	`function probeLedger(lines: Line[], snapshot: Record<string, unknown>) {`,
	`function probeCapacity(lines: Line[], snapshot: Record<string, unknown>) {`,
}

func osAbsorptionNeedsOSStatusProof(row OSAbsorptionMatrixRow) bool {
	tupleMatches := row.ID == "os_source_os_status_cli" &&
		row.SourceRef == "cli:os status" &&
		row.SourceKind == "compatibility_cli" &&
		row.CompoundOwner == osAbsorptionSystemStatusAuthorityRef &&
		row.Status == "wrapper"
	if row.ProofMode != "" {
		return row.ProofMode == osAbsorptionProofModeOSStatus && tupleMatches
	}
	return tupleMatches
}

func osAbsorptionOSStatusProof(homePath string, row OSAbsorptionMatrixRow) (osAbsorptionProofResult, bool) {
	path, body, found, err := readOSWrapperEntrypoint(homePath)
	if err != nil {
		return osAbsorptionProofResult{reason: "os status entrypoint: " + err.Error()}, false
	}
	if !found {
		return osAbsorptionProofResult{skipped: true, reason: osAbsorptionAbsorbedOSSkipReason}, false
	}
	expectedDigest, err := exactOSCapabilityDigestEvidence(row.EvidenceRefs, osStatusEntrypointDigestRef)
	if err != nil {
		return osAbsorptionProofResult{reason: "os status expected entrypoint binding: " + err.Error()}, false
	}
	liveDigest := digestBytes(body)
	if liveDigest != expectedDigest {
		return osAbsorptionProofResult{reason: fmt.Sprintf(
			"os status entrypoint %s sha256 %s does not match reviewed %s (wrapper drift; re-review and re-import)",
			path, liveDigest, expectedDigest,
		)}, true
	}
	text := string(body)
	if !osStatusDispatchPattern.MatchString(text) || !strings.Contains(text, `if (cmd === "status") exit = await runStatus(rest.includes("--json"));`) {
		return osAbsorptionProofResult{reason: "os status dispatch no longer reaches runStatus"}, false
	}

	sections := osStatusCompoundSections(homePath)
	for _, section := range sections {
		if !strings.Contains(text, section.readerSig) {
			return osAbsorptionProofResult{reason: fmt.Sprintf("os status missing required compound reader %q", section.readerSig)}, false
		}
		fnBody, err := extractTSFunctionBody(text, section.readerSig)
		if err != nil {
			return osAbsorptionProofResult{reason: fmt.Sprintf("os status %s: %v", section.label, err)}, false
		}
		if err := requireGenuineCompoundCallBeforeStatus(section.label, fnBody, "compoundCommand(", []string{`status: "ok"`}); err != nil {
			return osAbsorptionProofResult{reason: err.Error()}, false
		}
	}
	for _, want := range osStatusRetainedNativeProbes {
		if !strings.Contains(text, want) {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"os status retained-native probe missing: %q (the matrix row's retained-native-scope claim is now false; update the row or restore the probe)",
				want,
			)}, false
		}
	}

	osPath, err := exec.LookPath("os")
	if err != nil {
		return osAbsorptionProofResult{reason: "resolve installed os binary: " + err.Error()}, false
	}
	compoundBinary, err := resolveOSCapabilityBinary()
	if err != nil {
		return osAbsorptionProofResult{reason: "resolve installed compound binary: " + err.Error()}, false
	}

	// `os status --json` uses its exit code to carry the dashboard verdict
	// itself (0=ok, 1=warn, 2=fail — see runStatus()'s `return fails ? 2 :
	// exitWarns ? 1 : 0`); it prints the full JSON snapshot to stdout in
	// every one of those cases. Treating exit!=0 as a wrapper malfunction
	// would make this proof flap red every time some unrelated native probe
	// (daemon down, switchback unreachable) legitimately warns/fails. Only
	// a real exec error, or an exit code outside that known 0/1/2 contract,
	// counts as the wrapper itself being broken.
	wrapper, err := runOSCapabilityCommandWithLimits(osPath, []string{"status", "--json"}, map[string]string{"COMPOUND_HOME": homePath}, 90*time.Second, maxOSCapabilityCommandBytes)
	if err != nil || wrapper.exitCode < 0 || wrapper.exitCode > 2 {
		return osAbsorptionProofResult{reason: commandFailure("os status", wrapper, err)}, false
	}
	var snapshot map[string]any
	if err := json.Unmarshal(wrapper.stdout, &snapshot); err != nil {
		return osAbsorptionProofResult{reason: "os status output: " + err.Error()}, false
	}

	var anchor map[string]any
	for _, section := range sections {
		raw, ok := snapshot[section.snapshotKey]
		if !ok {
			return osAbsorptionProofResult{reason: fmt.Sprintf("os status snapshot missing key %q", section.snapshotKey)}, false
		}
		entry, ok := raw.(map[string]any)
		if !ok {
			return osAbsorptionProofResult{reason: fmt.Sprintf("os status snapshot key %q is not an object", section.snapshotKey)}, false
		}
		marker := stringField(entry, section.markerField)
		if marker != section.markerValue {
			return osAbsorptionProofResult{reason: fmt.Sprintf(
				"os status snapshot key %q %s=%q, want %q (possible fabrication or drift)",
				section.snapshotKey, section.markerField, marker, section.markerValue,
			)}, false
		}
		status := stringField(entry, "status")
		switch status {
		case "ok", "unavailable", "missing", "fail", "empty", "needs_review":
		default:
			return osAbsorptionProofResult{reason: fmt.Sprintf("os status snapshot key %q has unrecognized status %q", section.snapshotKey, status)}, false
		}
		if section.requireOK {
			if status != "ok" {
				return osAbsorptionProofResult{reason: fmt.Sprintf(
					"os status anchor section %q status=%q, want ok (compound authority unreachable in this run)",
					section.snapshotKey, status,
				)}, false
			}
			anchor = entry
		}
	}
	if anchor == nil {
		return osAbsorptionProofResult{reason: "os status did not report the required anchor section"}, false
	}

	direct, err := runOSCapabilityCommand(compoundBinary.path, []string{"capability", "authority", "--json"}, map[string]string{"COMPOUND_HOME": homePath})
	if err != nil || direct.exitCode != 0 {
		return osAbsorptionProofResult{reason: commandFailure("compound capability authority", direct, err)}, false
	}
	var directOut map[string]any
	if err := json.Unmarshal(direct.stdout, &directOut); err != nil {
		return osAbsorptionProofResult{reason: "compound capability authority output: " + err.Error()}, false
	}
	directNodeCount, _ := directOut["node_count"].(float64)
	directEdgeCount, _ := directOut["edge_count"].(float64)
	liveNodeCount, _ := anchor["node_count"].(float64)
	liveEdgeCount, _ := anchor["edge_count"].(float64)
	if directNodeCount != liveNodeCount || directEdgeCount != liveEdgeCount {
		return osAbsorptionProofResult{reason: fmt.Sprintf(
			"os status capability_authority node/edge counts (%v/%v) diverge from direct compound capability authority (%v/%v)",
			liveNodeCount, liveEdgeCount, directNodeCount, directEdgeCount,
		)}, false
	}

	proofRef := fmt.Sprintf(
		"%s:entrypoint_sha256=%s;compound_sections=%d;anchor=capability_authority;anchor_nodes=%v;anchor_edges=%v;compound_sha256=%s",
		osAbsorptionProofModeOSStatus, liveDigest, len(sections), liveNodeCount, liveEdgeCount, compoundBinary.digest,
	)
	return osAbsorptionProofResult{ok: true, proofRef: proofRef}, false
}
