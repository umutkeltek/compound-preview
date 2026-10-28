package authority

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

func TestOSAbsorptionMatrixImportAndDoctor(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	capRows := []string{
		"/synthetic/example-os/spine/views/skill-manifest.json",
		"/synthetic/example-os/registry/tool-topology.json",
		"/synthetic/example-os/spine/views/skills-selection-board.json",
	}
	for _, source := range capRows {
		if _, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
			SourcePath:     source,
			SourceRevision: "sha256:" + digest12(source),
			ImportedVia:    "test import",
			NodeIDs:        []string{"skill:" + filepath.Base(source)},
			EvidenceRefs:   []string{"fixture:" + filepath.Base(source)},
		}, now); err != nil {
			t.Fatalf("record cap source authority: %v", err)
		}
	}
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	matrix := OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows: []OSAbsorptionMatrixRow{
			row("skill_manifest", capRows[0], "absorbed", capgraph.CapabilityGraphAuthorityRef, "compound capability authority", "capability_source_authority"),
			row("tool_topology", capRows[1], "absorbed", capgraph.CapabilityGraphAuthorityRef, "compound capability authority", "capability_source_authority"),
			row("skills_selection_board", capRows[2], "absorbed", capgraph.CapabilityGraphAuthorityRef, "compound capability authority", "capability_source_authority"),
			row("brain", "/synthetic/brain/projects/compound.md", "pending", "compound://ledger/memory", "compound memory recall", ""),
			row("gbrain", "gbrain://synthetic-store", "pending", "compound://adapters/memory-engine/gbrain", "compound memory engine-doctor", ""),
			row("infra_os", "/synthetic/infra-os/project-registry.yaml", "pending", "compound://registries/projects", "compound runtime doctor", ""),
			row("os_status", "cli:os status", "pending", "compound://projections/system-status", "os status wrapper", ""),
		},
	}
	writeJSONFixture(t, matrixPath, matrix)
	report, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now)
	if err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	if report.RowCount != 7 || report.AbsorbedCount != 3 || report.PendingCount != 4 {
		t.Fatalf("unexpected import counts: %+v", report)
	}
	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if doctor.Status != "fail" {
		t.Fatalf("pending retirement rows must keep the doctor red: %+v", doctor)
	}
	if doctor.AutoProofCount != 3 || doctor.PendingWithGateCount != 4 {
		t.Fatalf("doctor proof counts wrong: %+v", doctor)
	}
	if check, ok := findOSAbsorptionDoctorCheck(doctor.Checks, "pending_retirement_gates"); !ok || check.Status != "fail" {
		t.Fatalf("pending retirement gate must fail until count reaches zero: %+v", doctor.Checks)
	}
}

func TestOSAbsorptionDoctorFlagsMissingRequiredFields(t *testing.T) {
	homePath := t.TempDir()
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	matrix := OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows: []OSAbsorptionMatrixRow{
			{
				ID:          "bad_row",
				SourceRef:   "/synthetic/source",
				OldOwner:    "legacy",
				OldRole:     "legacy role",
				Privacy:     "private",
				Status:      "pending",
				CurrentRole: "pending",
			},
		},
	}
	writeJSONFixture(t, matrixPath, matrix)
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if doctor.Status != "fail" {
		t.Fatalf("doctor should fail: %+v", doctor)
	}
	if doctor.MissingRequiredCount != 1 {
		t.Fatalf("expected missing row count, got %+v", doctor)
	}
}

func TestOSAbsorptionPermissionPolicyFixtureStaysPendingUntilConsumerProof(t *testing.T) {
	homePath := t.TempDir()
	fixture := filepath.Join("..", "..", "fixtures", "authority", "os-absorption-permission-policy.json")
	report, err := ImportOSAbsorptionMatrix(homePath, fixture, time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.RowCount != 1 || report.PendingCount != 1 || report.AbsorbedCount != 0 {
		t.Fatalf("import counts = rows:%d pending:%d absorbed:%d", report.RowCount, report.PendingCount, report.AbsorbedCount)
	}
	matrix, err := LoadOSAbsorptionMatrix(homePath)
	if err != nil {
		t.Fatal(err)
	}
	row := matrix.Rows[0]
	if row.ID != "os_source_claude_code_permission_policy" ||
		row.CompoundOwner != "compound://config/harness-permissions/claude_code" ||
		row.CurrentRole != "compatibility and reviewed import evidence" ||
		!strings.Contains(row.RetirementGate, "no independent legacy policy writer") {
		t.Fatalf("permission-policy absorption row lost its ownership boundary: %+v", row)
	}
	if !strings.HasPrefix(row.SourceRevision, "sha256:") || len(row.EvidenceRefs) != 1 {
		t.Fatalf("permission-policy source provenance missing: %+v", row)
	}
}

// TestOSAbsorptionAutoProofDecisionsAndOperatorState is J1's positive proof:
// os_source_decisions_repo and os_source_operator_state report proof_status=ok
// with a proof_ref once their REAL consumers (the decisions source-authority
// ledger and the policy operator-state reader) have live evidence, without any
// row status-string change (decisions stays evidence_only, operator-state
// stays absorbed).
func TestOSAbsorptionAutoProofDecisionsAndOperatorState(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	if _, err := decisions.RecordSourceAuthority(homePath, decisions.DecisionImportReport{
		SourcePath:  "/synthetic/operating-system/decisions/DEC-0001.md",
		SourceKind:  "decision_source",
		EvidenceID:  "ev_dec_1",
		CandidateID: "cand_dec_1",
		ClaimID:     "claim_dec_1",
		Verdict:     candidates.Promote,
		Lifecycle:   decisions.Lifecycle{ID: "dec_0001"},
	}, "test import", now); err != nil {
		t.Fatalf("record decisions source authority: %v", err)
	}

	opSourcePath := filepath.Join(t.TempDir(), "operator-state.json")
	if err := os.WriteFile(opSourcePath, []byte(`{"mode":"test","autonomy_lever":{"level":"L2"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportOperatorState(homePath, opSourcePath, now); err != nil {
		t.Fatalf("import operator state: %v", err)
	}

	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	matrix := OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows: []OSAbsorptionMatrixRow{
			row("os_source_decisions_repo", "/synthetic/operating-system/decisions/", "evidence_only", osAbsorptionDecisionsLedgerRef, "compound decisions authority", ""),
			row("os_source_operator_state", "/synthetic/example-os/registry/operator-state.json", "absorbed", policy.OperatorStateAuthorityRef, "compound policy evaluate", ""),
		},
	}
	writeJSONFixture(t, matrixPath, matrix)
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
		t.Fatalf("import matrix: %v", err)
	}

	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	byID := map[string]OSAbsorptionDoctorRow{}
	for _, r := range doctor.Rows {
		byID[r.ID] = r
	}
	decRow, ok := byID["os_source_decisions_repo"]
	if !ok || decRow.ProofStatus != "ok" || decRow.ProofRef == "" || decRow.Status != "evidence_only" {
		t.Fatalf("expected decisions row proof_status=ok with proof_ref and unchanged status=evidence_only, got %+v", decRow)
	}
	opRow, ok := byID["os_source_operator_state"]
	if !ok || opRow.ProofStatus != "ok" || opRow.ProofRef == "" || opRow.Status != "absorbed" {
		t.Fatalf("expected operator-state row proof_status=ok with proof_ref and unchanged status=absorbed, got %+v", opRow)
	}
}

// TestOSAbsorptionDecisionsProofFailsWhenLedgerAbsent is J1's falsifier: with
// no decisions source-authority record ever written to COMPOUND_HOME, the row
// must flip to proof_status=failed — never ok, never silently manual.
func TestOSAbsorptionDecisionsProofFailsWhenLedgerAbsent(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	matrix := OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows: []OSAbsorptionMatrixRow{
			row("os_source_decisions_repo", "/synthetic/operating-system/decisions/", "evidence_only", osAbsorptionDecisionsLedgerRef, "compound decisions authority", ""),
		},
	}
	writeJSONFixture(t, matrixPath, matrix)
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(doctor.Rows) != 1 {
		t.Fatalf("expected one row, got %+v", doctor.Rows)
	}
	got := doctor.Rows[0]
	if got.ProofStatus != "failed" {
		t.Fatalf("expected proof_status=failed when the decisions ledger is absent, got %+v", got)
	}
	if got.ProofDetail == "" {
		t.Fatalf("expected a failure reason on the row, got %+v", got)
	}
	if got.Status != "evidence_only" {
		t.Fatalf("row status string must not change: %+v", got)
	}
}

// TestOSAbsorptionOperatorStateProofFailsWhenRegistryAbsent mirrors the
// decisions falsifier for the operator-state row: no import ever ran, so the
// policy reader finds nothing and the row must fail proof, not pass or hide
// behind "manual".
func TestOSAbsorptionOperatorStateProofFailsWhenRegistryAbsent(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	matrix := OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows: []OSAbsorptionMatrixRow{
			row("os_source_operator_state", "/synthetic/example-os/registry/operator-state.json", "absorbed", policy.OperatorStateAuthorityRef, "compound policy evaluate", ""),
		},
	}
	writeJSONFixture(t, matrixPath, matrix)
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(doctor.Rows) != 1 {
		t.Fatalf("expected one row, got %+v", doctor.Rows)
	}
	got := doctor.Rows[0]
	if got.ProofStatus != "failed" {
		t.Fatalf("expected proof_status=failed when the operator-state registry is absent, got %+v", got)
	}
	if got.Status != "absorbed" {
		t.Fatalf("row status string must not change: %+v", got)
	}
}

// TestOSAbsorptionCountsSplitProvenVsAsserted is J2's falsifier: on a fixture
// with one machine-proven absorbed row, one manual (unproven) absorbed-bucket
// row, and one pending row, machine_proven_count must diverge from the
// legacy absorbed_count bucket — proving the split reflects real evidence,
// not the same status-string tally under a new name.
func TestOSAbsorptionCountsSplitProvenVsAsserted(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	capSource := "/synthetic/example-os/spine/views/skill-manifest.json"
	if _, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
		SourcePath:     capSource,
		SourceRevision: "sha256:" + digest12(capSource),
		ImportedVia:    "test import",
		NodeIDs:        []string{"skill:manifest"},
		EvidenceRefs:   []string{"fixture:manifest"},
	}, now); err != nil {
		t.Fatalf("record cap source authority: %v", err)
	}

	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	matrix := OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows: []OSAbsorptionMatrixRow{
			row("skill_manifest", capSource, "absorbed", capgraph.CapabilityGraphAuthorityRef, "compound capability authority", "capability_source_authority"),
			row("os_capability_cli", "/synthetic/cli/os-capability", "wrapper", capgraph.CapabilityGraphAuthorityRef, "os capability wrapper", ""),
			row("brain", "/synthetic/brain/projects/compound.md", "pending", "compound://ledger/memory", "compound memory recall", ""),
		},
	}
	writeJSONFixture(t, matrixPath, matrix)
	importReport, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now)
	if err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	if importReport.AbsorbedCount != 2 || importReport.PendingCount != 1 {
		t.Fatalf("unexpected import counts: %+v", importReport)
	}

	doctor, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if doctor.AbsorbedCount != 2 {
		t.Fatalf("expected legacy absorbed_count=2 (status bucket), got %+v", doctor)
	}
	if doctor.MachineProvenCount != 1 {
		t.Fatalf("expected machine_proven_count=1 (only the capability-proven row), got %+v", doctor)
	}
	if doctor.AssertedCount != 1 {
		t.Fatalf("expected asserted_count=1 (the manual wrapper row), got %+v", doctor)
	}
	if doctor.PendingCount != 1 {
		t.Fatalf("expected pending_count=1 (existing), got %+v", doctor)
	}
	if doctor.MachineProvenCount == doctor.AbsorbedCount {
		t.Fatalf("machine_proven_count must diverge from absorbed_count to prove the split is real: %+v", doctor)
	}
}

func TestOSAbsorptionOSCapabilityProofBindsCompoundOnlyProjection(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "ok" || got.Status != "wrapper" {
		t.Fatalf("Compound-only os capability projection should be machine-proven: %+v", got)
	}
	for _, want := range []string{
		"contract://capability-task-classifier",
		"sha256:" + repeatHex("b", 64),
		"compound://registries/capability-graph",
		"sha256:" + repeatHex("c", 12),
		"compound://registries/model-router",
		"shim_sha256:",
		"entrypoint_sha256:",
		"projection_sha256:",
		"bun_sha256:",
		"bun_version:",
		"compound_build:",
		"mrcsrc_aaaaaaaaaaaa",
	} {
		if !strings.Contains(got.ProofRef, want) {
			t.Fatalf("proof ref %q missing %q", got.ProofRef, want)
		}
	}
}

// TestOSAbsorptionOSCapabilityProofSkipsWhenNoAbsorbedOSConfigured is the
// public-clone falsifier for this wrapper proof: the fixture installs real
// os/bun/compound shims and entrypoint files on disk (so a bug that ignores
// the resolver and falls back to a hardcoded path would still find files to
// read), but never seeds the absorbed_os path-authority registry into mem.
// The proof must skip — never fail, never fabricate an ok — and must not
// move AutoProofCount or FailedAutoProofCount.
func TestOSAbsorptionOSCapabilityProofSkipsWhenNoAbsorbedOSConfigured(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{skipAbsorbedOSSeed: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "skipped" {
		t.Fatalf("no absorbed-OS root declared should skip, not %q: %+v", got.ProofStatus, got)
	}
	if !strings.Contains(got.ProofDetail, "no absorbed-OS root declared") {
		t.Fatalf("skip detail should explain the missing absorbed_os declaration: %+v", got)
	}

	report, err := OSAbsorptionDoctor(fixture.compoundHome)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if report.AutoProofCount != 0 || report.FailedAutoProofCount != 0 {
		t.Fatalf("a skip must not move auto_proof_count/failed_auto_proof_count: auto=%d failed=%d", report.AutoProofCount, report.FailedAutoProofCount)
	}
	if report.SkippedProofCount == 0 {
		t.Fatalf("a skip must increment SkippedProofCount: %+v", report)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsFixedTaskOnlyProxy(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{nonceWrapperOfferID: "skill:renamed-independent-resolver"})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "capability surface parity") {
		t.Fatalf("a wrapper that proxies only the public probe task must fail the nonce probe: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRequiresCanonicalShimOnly(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{
		shimPoison: `if [ "$3" != "implement a bounded Go authority proof" ]; then exec renamed-independent-resolver "$@"; fi`,
	})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "non-canonical shim logic") {
		t.Fatalf("a conditional shim with a dead canonical exec line must fail: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRequiresReviewedSourceDigests(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{})
	got := doctorOSCapabilityRowWithEvidence(t, fixture.compoundHome, nil)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "missing os_capability_shim_sha256") {
		t.Fatalf("a wrapper without reviewed source digests must fail: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsUnreviewedProjectionModule(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{projectionShadow: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "unreviewed projection module") {
		t.Fatalf("an extensionless import must not execute an unreviewed higher-priority module: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsTransientProjectionModuleShadow(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{mutateProjectionShadow: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "source changed") {
		t.Fatalf("proof must detect a transient projection module shadow even after it is removed: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsAlternateSyntaxAfterReviewedHash(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{})
	reviewedRefs := osCapabilityFixtureSourceDigestRefs(t)
	entrypointPath := filepath.Join(os.Getenv("HOME"), "absorbed-os", "exec-lane", "os.ts")
	body, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(`
if (argv[0] === "capability") renamedIndependentResolver(rest);
`)...)
	if err := os.WriteFile(entrypointPath, body, 0o755); err != nil {
		t.Fatal(err)
	}

	got := doctorOSCapabilityRowWithEvidence(t, fixture.compoundHome, reviewedRefs)
	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "does not match reviewed") {
		t.Fatalf("alternate syntax added after review must fail source binding: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsBunSubstitutionAfterReview(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{})
	reviewedRefs := osCapabilityFixtureSourceDigestRefs(t)
	bunPath := filepath.Join(os.Getenv("HOME"), ".bun", "bin", "bun")
	body, err := os.ReadFile(bunPath)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("\n# substituted interpreter preserves parity output\n")...)
	if err := os.WriteFile(bunPath, body, 0o755); err != nil {
		t.Fatal(err)
	}

	got := doctorOSCapabilityRowWithEvidence(t, fixture.compoundHome, reviewedRefs)
	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "Bun digest") {
		t.Fatalf("a parity-preserving Bun substitution must fail reviewed dependency binding: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRequiresClassifierEvidence(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{
		disabledAuthorityRefs: []string{capgraph.CapabilityGraphAuthorityRef, routeevidence.ModelCapabilitiesAuthorityRef},
	})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "authority refs") {
		t.Fatalf("disabled output without classifier authority must fail: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsIndependentFallback(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{disabledExitZero: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "disabled Compound") {
		t.Fatalf("os capability must fail closed when Compound is disabled: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsClassSpecificFallback(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{secondaryDisabledExitZero: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "disabled Compound") {
		t.Fatalf("judge-critique path must fail closed independently of the build path: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsDisabledPacketWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, tc := range []struct {
		name string
		opts osCapabilityProofFixture
	}{
		{name: "build probe", opts: osCapabilityProofFixture{disabledWritesPacket: true}},
		{name: "judge probe", opts: osCapabilityProofFixture{secondaryWritesPacket: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := installOSCapabilityProofFixture(t, tc.opts)
			got := doctorOSCapabilityRow(t, fixture.compoundHome)

			if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "wrote an os capability packet") {
				t.Fatalf("disabled Compound must not leave a capability packet before failing closed: %+v", got)
			}
		})
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsRetiredSourceRead(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{projectionPoison: `const legacy = "skill-manifest.json"`})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "retired source") {
		t.Fatalf("projection source must not read retired OS authorities: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRequiresExactOutputParity(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{wrapperOfferID: "skill:forged-offer"})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "capability surface parity") {
		t.Fatalf("self-reported authority refs without exact output parity must fail: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRequiresProductionDispatch(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{legacyDispatch: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "production dispatch") {
		t.Fatalf("dead projection module must not prove the production CLI: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRequiresUniqueProductionDispatch(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{
		entrypointPoison: `else if (cmd === "capability") exit = await renamedIndependentResolver(rest);`,
	})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "production dispatch") {
		t.Fatalf("a second capability dispatch must fail even when the canonical line remains: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsSourceMutationDuringProbe(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{mutateEntrypoint: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "source changed") {
		t.Fatalf("proof must not cite pre-run source bytes after the entrypoint changes: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsSourceSwapAndRestoreDuringProbe(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{mutateRestoreEntrypoint: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "source changed") {
		t.Fatalf("proof must detect transient source mutation even when bytes are restored: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsBinaryMutationDuringProbe(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{mutateCompoundBinary: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "binary changed") {
		t.Fatalf("proof must not cite a pre-run Compound binary after it changes: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsBinarySwapAndRestoreDuringProbe(t *testing.T) {
	fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{mutateRestoreCompound: true})
	got := doctorOSCapabilityRow(t, fixture.compoundHome)

	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "binary changed") {
		t.Fatalf("proof must detect transient Compound mutation even when bytes are restored: %+v", got)
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsDormantIndependentResolverSource(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, source := range []string{
		`import { classifyCapability } from "./classify-capability";`,
		`import { resolve as resolveCapabilityIntent } from "../scripts/resolve_capability";`,
		`async function runCapability() {}`,
		`function writeCapabilityContextPacket() {}`,
		`function collectLoopTriggerFires() {}`,
		`function appendLoopTriggerFireReceipts() {}`,
		`const TOOL_HINTS = [];`,
		`const OS_CAPABILITY_FIRES_PATH = "forged";`,
		`const CAPABILITY_OWNERSHIP = {};`,
	} {
		t.Run(source, func(t *testing.T) {
			fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{entrypointPoison: source})
			got := doctorOSCapabilityRow(t, fixture.compoundHome)

			if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "dormant independent resolver") {
				t.Fatalf("dormant resolver source must fail: %+v", got)
			}
		})
	}
}

func TestOSAbsorptionOSCapabilityProofRejectsDormantIndependentProjectionSource(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	for _, source := range []string{
		`const routeInfo = {};`,
		`function classifyCapability() {}`,
		`async function readCompoundCapabilitySurface() {}`,
	} {
		t.Run(source, func(t *testing.T) {
			fixture := installOSCapabilityProofFixture(t, osCapabilityProofFixture{projectionPoison: source})
			got := doctorOSCapabilityRow(t, fixture.compoundHome)

			if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "retired source") {
				t.Fatalf("dormant projection resolver source must fail: %+v", got)
			}
		})
	}
}

func TestOSAbsorptionOSCapabilityProofModeRequiresExactTuple(t *testing.T) {
	valid := OSAbsorptionMatrixRow{
		ID:            "os_source_os_capability_cli",
		SourceRef:     "cli:os capability",
		SourceKind:    "compatibility_cli",
		CompoundOwner: capgraph.CapabilityGraphAuthorityRef,
		Status:        "wrapper",
		ProofMode:     osAbsorptionProofModeOSCapability,
	}
	if !osAbsorptionNeedsOSCapabilityProof(valid) {
		t.Fatal("exact os capability tuple should opt into wrapper proof")
	}
	invalid := valid
	invalid.SourceRef = "cli:not-os-capability"
	if osAbsorptionNeedsOSCapabilityProof(invalid) {
		t.Fatal("explicit proof mode must not certify a different CLI tuple")
	}
	valid.ProofMode = osAbsorptionProofModeCapability
	if osAbsorptionNeedsOSCapabilityProof(valid) {
		t.Fatal("an explicit different proof mode must suppress wrapper proof")
	}
}

func TestOSAbsorptionWorkPacketAuthorityProofBindsArchivedSourcePacket(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}

	sourceDir := filepath.Join(t.TempDir(), "example-os", "packets")
	sourcePacket := writePacketAuthorityFixture(t, sourceDir, "source-packet.md", "Source packet")
	sourceClosed := closePacketAuthorityWork(t, homePath, sourcePacket, true, now)
	listedPacket := writePacketAuthorityFixture(t, t.TempDir(), "listed-packet.md", "Listed absorption packet")
	listedClosed := closePacketAuthorityWork(t, homePath, listedPacket, false, now.Add(time.Minute))

	if _, err := home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"work"}}, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("archive terminal work: %v", err)
	}

	got := doctorPacketAuthorityRow(t, homePath, packetAuthorityRow(sourceDir, listedClosed.Work.ID), now.Add(3*time.Minute))
	if got.ProofStatus != "ok" || got.Status != "wrapper" {
		t.Fatalf("packet row should be machine-proven without changing status: %+v", got)
	}
	for _, workID := range []string{listedClosed.Work.ID, sourceClosed.Work.ID} {
		if !strings.Contains(got.ProofRef, workID) {
			t.Fatalf("proof ref %q missing %s", got.ProofRef, workID)
		}
	}
	if !strings.Contains(got.ProofRef, "prerequisite="+listedClosed.Work.ID) || !strings.Contains(got.ProofRef, "source_consumer="+sourceClosed.Work.ID) {
		t.Fatalf("proof ref must distinguish prerequisite and source-consumer roles: %q", got.ProofRef)
	}
}

func TestOSAbsorptionWorkPacketAuthorityProofRequiresEveryListedWork(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	sourceDir := filepath.Join(t.TempDir(), "packets")
	sourcePacket := writePacketAuthorityFixture(t, sourceDir, "source-packet.md", "Source packet")
	closePacketAuthorityWork(t, homePath, sourcePacket, true, now)
	listedPacket := writePacketAuthorityFixture(t, t.TempDir(), "listed-packet.md", "Listed packet")
	listedClosed := closePacketAuthorityWork(t, homePath, listedPacket, false, now.Add(time.Minute))

	got := doctorPacketAuthorityRow(t, homePath, packetAuthorityRow(sourceDir, listedClosed.Work.ID, "work_missing"), now.Add(2*time.Minute))
	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "work_missing") {
		t.Fatalf("one missing listed work must fail the whole row: %+v", got)
	}
}

func TestOSAbsorptionWorkPacketAuthorityProofRejectsNonPacketWork(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, homePath string, closed work.CloseResult)
		want   string
	}{
		{
			name: "runtime source class",
			mutate: func(t *testing.T, homePath string, closed work.CloseResult) {
				t.Helper()
				mutatePacketAuthorityWork(t, homePath, closed.Work.ID, func(w *envelope.WorkEnvelope) {
					w.Authority.SourceClass = "runtime"
				})
			},
			want: "source_class",
		},
		{
			name: "non-packet canonical surface",
			mutate: func(t *testing.T, homePath string, closed work.CloseResult) {
				t.Helper()
				mutatePacketAuthorityWork(t, homePath, closed.Work.ID, func(w *envelope.WorkEnvelope) {
					w.CanonicalSurface = "command:not-a-packet"
				})
			},
			want: "canonical_surface",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
			sourceDir := filepath.Join(t.TempDir(), "packets")
			sourcePacket := writePacketAuthorityFixture(t, sourceDir, "source-packet.md", "Source packet")
			closePacketAuthorityWork(t, homePath, sourcePacket, true, now)
			listedPacket := writePacketAuthorityFixture(t, t.TempDir(), "listed-packet.md", "Listed packet")
			listedClosed := closePacketAuthorityWork(t, homePath, listedPacket, false, now.Add(time.Minute))
			tt.mutate(t, homePath, listedClosed)

			got := doctorPacketAuthorityRow(t, homePath, packetAuthorityRow(sourceDir, listedClosed.Work.ID), now.Add(2*time.Minute))
			if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, tt.want) {
				t.Fatalf("non-packet work should fail with %q: %+v", tt.want, got)
			}
		})
	}
}

func TestOSAbsorptionWorkPacketAuthorityProofRequiresReceiptTransitionChain(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	sourceDir := filepath.Join(t.TempDir(), "packets")
	sourcePacket := writePacketAuthorityFixture(t, sourceDir, "source-packet.md", "Source packet")
	closePacketAuthorityWork(t, homePath, sourcePacket, true, now)
	listedPacket := writePacketAuthorityFixture(t, t.TempDir(), "listed-packet.md", "Listed packet")
	listedClosed := closePacketAuthorityWork(t, homePath, listedPacket, false, now.Add(time.Minute))
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Remove(listedClosed.Transition.ID); err != nil {
		t.Fatalf("remove transition falsifier: %v", err)
	}

	got := doctorPacketAuthorityRow(t, homePath, packetAuthorityRow(sourceDir, listedClosed.Work.ID), now.Add(2*time.Minute))
	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "transition") {
		t.Fatalf("completed state without transition proof must fail: %+v", got)
	}
}

func TestOSAbsorptionWorkPacketAuthorityProofRequiresCanonicalGovernedChain(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	sourceDir := filepath.Join(t.TempDir(), "packets")
	sourcePacket := writePacketAuthorityFixture(t, sourceDir, "source-packet.md", "Source packet")
	closePacketAuthorityWork(t, homePath, sourcePacket, true, now)
	listedPacket := writePacketAuthorityFixture(t, t.TempDir(), "listed-packet.md", "Listed packet")
	listedClosed := closePacketAuthorityWork(t, homePath, listedPacket, false, now.Add(time.Minute))
	mutatePacketAuthorityWork(t, homePath, listedClosed.Work.ID, func(w *envelope.WorkEnvelope) {
		w.ActionIntentIDs = nil
		w.PolicyDecisionID = ""
	})

	got := doctorPacketAuthorityRow(t, homePath, packetAuthorityRow(sourceDir, listedClosed.Work.ID), now.Add(2*time.Minute))
	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "canonical proof chain incomplete") {
		t.Fatalf("receipt/transition files without governed action/policy proof must fail: %+v", got)
	}
}

func TestOSAbsorptionWorkPacketAuthorityProofRejectsUnboundSourceDirectory(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	sourceDir := filepath.Join(t.TempDir(), "packets")
	writePacketAuthorityFixture(t, sourceDir, "never-imported.md", "Never imported source packet")
	listedPacket := writePacketAuthorityFixture(t, t.TempDir(), "listed-packet.md", "Valid but unrelated packet")
	listedClosed := closePacketAuthorityWork(t, homePath, listedPacket, true, now)

	got := doctorPacketAuthorityRow(t, homePath, packetAuthorityRow(sourceDir, listedClosed.Work.ID), now.Add(time.Minute))
	if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "source directory") {
		t.Fatalf("unrelated packet work must not prove the declared source directory: %+v", got)
	}
}

func TestOSAbsorptionWorkPacketAuthorityProofModeSelection(t *testing.T) {
	if !osAbsorptionNeedsWorkPacketProof(OSAbsorptionMatrixRow{ProofMode: osAbsorptionProofModeWorkPacket}) {
		t.Fatal("explicit work_packet_authority mode should opt in regardless of row shape")
	}
	auto := OSAbsorptionMatrixRow{CompoundOwner: "compound://work", SourceKind: "packet_dir", Status: "wrapper"}
	if !osAbsorptionNeedsWorkPacketProof(auto) {
		t.Fatal("live work packet tuple should auto-detect")
	}
	auto.ProofMode = osAbsorptionProofModeCapability
	if osAbsorptionNeedsWorkPacketProof(auto) {
		t.Fatal("an explicit different proof mode must suppress tuple auto-detection")
	}
}

func TestOSAbsorptionPacketEvidenceRequiresCanonicalDigestShape(t *testing.T) {
	validRevision := "sha256:" + strings.Repeat("a", 64)
	validEvidence := "packet_sha256:" + strings.Repeat("a", 16)
	valid := envelope.WorkEnvelope{Header: envelope.Header{
		Authority:    envelope.Authority{SourceRevision: validRevision},
		EvidenceRefs: []string{validEvidence},
	}}
	if got, err := osAbsorptionPacketEvidence(valid); err != nil || got != validEvidence {
		t.Fatalf("canonical packet evidence should pass: got=%q err=%v", got, err)
	}

	tests := []struct {
		name     string
		revision string
		evidence string
	}{
		{name: "bare digest", revision: strings.Repeat("a", 64), evidence: validEvidence},
		{name: "short revision", revision: "sha256:" + strings.Repeat("a", 16), evidence: validEvidence},
		{name: "short evidence", revision: validRevision, evidence: "packet_sha256:" + strings.Repeat("a", 12)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := valid
			w.Authority.SourceRevision = tt.revision
			w.EvidenceRefs = []string{tt.evidence}
			if _, err := osAbsorptionPacketEvidence(w); err == nil {
				t.Fatalf("non-canonical digest shape should fail: revision=%q evidence=%q", tt.revision, tt.evidence)
			}
		})
	}
}

func TestOSAbsorptionWorkPacketAuthorityRejectsRelativeSourceDirectory(t *testing.T) {
	_, _, _, err := osAbsorptionSourcePacketCandidates(OSAbsorptionMatrixRow{SourceKind: "packet_dir", SourceRef: "relative/packets"})
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative packet directory must fail cwd-independent proof, got %v", err)
	}
}

func packetAuthorityRow(sourceDir string, workIDs ...string) OSAbsorptionMatrixRow {
	r := row("os_source_example_packets", sourceDir, "wrapper", "compound://work", "compound work import-packet / workbench / dispatch-step", "")
	r.SourceKind = "packet_dir"
	r.EvidenceRefs = append([]string(nil), workIDs...)
	return r
}

func doctorPacketAuthorityRow(t *testing.T, homePath string, matrixRow OSAbsorptionMatrixRow, now time.Time) OSAbsorptionDoctorRow {
	t.Helper()
	matrixPath := filepath.Join(t.TempDir(), "matrix.json")
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{SchemaVersion: OSAbsorptionMatrixSchema, Rows: []OSAbsorptionMatrixRow{matrixRow}})
	if _, err := ImportOSAbsorptionMatrix(homePath, matrixPath, now); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	report, err := OSAbsorptionDoctor(homePath)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(report.Rows) != 1 {
		t.Fatalf("doctor rows = %+v, want one", report.Rows)
	}
	return report.Rows[0]
}

func writePacketAuthorityFixture(t *testing.T, dir, name, title string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir packet fixture: %v", err)
	}
	path := filepath.Join(dir, name)
	body := "# " + title + "\nDate: 2026-07-05\nStatus: ready\nOwner: authority-test\nSuccess: packet proof reaches a state delta.\n\n## Objective\n\nProve packet authority.\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write packet fixture: %v", err)
	}
	return path
}

func closePacketAuthorityWork(t *testing.T, homePath, packetPath string, withContext bool, now time.Time) work.CloseResult {
	t.Helper()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	contextID := ""
	if withContext {
		contextID = "ctx_" + digest12(packetPath)
		writePacketAuthorityContext(t, homePath, contextID, now)
	}
	imported, err := store.ImportPacket(packetPath, work.PacketImportOptions{State: "ready", Actor: "authority-test", ContextPackID: contextID}, now)
	if err != nil {
		t.Fatalf("import packet: %v", err)
	}
	if _, err := store.Claim(imported.Work.ID, "authority-test", now.Add(time.Second)); err != nil {
		t.Fatalf("claim packet: %v", err)
	}
	if _, err := store.Advance(imported.Work.ID, work.AdvanceInput{To: "running", Actor: "authority-test", Note: "run packet proof"}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("advance packet: %v", err)
	}
	if _, err := store.Advance(imported.Work.ID, work.AdvanceInput{To: "verifying", Actor: "authority-test", VerificationID: "check:packet-authority"}, now.Add(3*time.Second)); err != nil {
		t.Fatalf("verify packet: %v", err)
	}
	closed, err := store.Close(imported.Work.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Summary:   "packet authority fixture complete",
		Changed:   true,
		Actor:     "authority-test",
		ProofRefs: []string{"repair:packet-authority-test"},
	}, now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("close packet: %v", err)
	}
	return closed
}

func writePacketAuthorityContext(t *testing.T, homePath, id string, now time.Time) {
	t.Helper()
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            id,
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "authority-test"},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD:          "packet authority context",
		Included:           []envelope.IncludedRef{},
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	path := filepath.Join(homePath, "context", "packs", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir context fixture: %v", err)
	}
	writeJSONFixture(t, path, pack)
}

func mutatePacketAuthorityWork(t *testing.T, homePath, workID string, mutate func(*envelope.WorkEnvelope)) {
	t.Helper()
	path := filepath.Join(homePath, "work", "work", workID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read work fixture: %v", err)
	}
	var w envelope.WorkEnvelope
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatalf("decode work fixture: %v", err)
	}
	mutate(&w)
	writeJSONFixture(t, path, w)
}

func row(id, source, status, owner, consumer, proofMode string) OSAbsorptionMatrixRow {
	return OSAbsorptionMatrixRow{
		ID:             id,
		SourceRef:      source,
		OldOwner:       "legacy",
		OldRole:        "legacy source",
		CurrentRole:    "evidence_only_input",
		Privacy:        "private",
		CompoundOwner:  owner,
		Consumer:       consumer,
		Proof:          consumer + " proof",
		ProofMode:      proofMode,
		RetirementGate: "consumer reads Compound authority",
		Status:         status,
	}
}

func writeJSONFixture(t *testing.T, path string, v any) {
	t.Helper()
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
