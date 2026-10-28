package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/observation"
)

// writeInstructionPackDriftObserveFixture mirrors
// writeHarnessDriftObserveFixture but for the instruction-pack seam: it
// produces a clean baseline (full RenderInstructionPack → fresh
// latest.json on disk) and returns the harness-scoped home state the
// drift bridge can perturb.
func writeInstructionPackDriftObserveFixture(t *testing.T) (homePath, targetPath string, render harness.PermissionRender) {
	t.Helper()
	homePath, targetPath = t.TempDir(), filepath.Join(t.TempDir(), "settings.json")
	writeInstructionPackDriftObserveJSON(t, targetPath, map[string]any{"theme": "dark"})
	policy := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow:       []string{"Read", "mcp__gbrain__*"},
			Deny:        []string{"Bash(rm:*)"},
			Ask:         []string{"WebFetch"},
			DefaultMode: "default",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	}
	writeInstructionPackDriftObserveJSON(t, harness.PermissionAuthorityPath(homePath, "claude_code"), policy)
	render, err := harness.RenderPermissions(homePath, "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, render.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := harness.WriteHarnessArtifactManifest(homePath, []harness.HarnessArtifact{render.Artifact}); err != nil {
		t.Fatal(err)
	}
	// Initial render populates latest.json so a clean baseline exists for
	// the drift comparator to compare against.
	seedCLIInstructionPackContext(t, homePath, "claude_code", "")
	if _, err := harness.RenderInstructionPack(homePath, "claude_code", "", cliTestNow); err != nil {
		t.Fatal(err)
	}
	return homePath, targetPath, render
}

// writeInstructionPackDriftObserveJSON mirrors writeHarnessDriftObserveJSON
// for cmd-level fixtures (the package-private helper lives in
// internal/harness so cmd/compound cannot reach it).
func writeInstructionPackDriftObserveJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// driftInstructionPackFixture perturbs the baseline by bumping the
// permission authority yaml — fresh RenderInstructionPack would now
// produce a DIFFERENT pack, so on-disk latest.json is stale relative to
// what the harness would render today. Returns the drifted
// InstructionPackDriftReport.
func driftInstructionPackFixture(t *testing.T, homePath, targetPath string, _ harness.PermissionRender) harness.InstructionPackDriftReport {
	t.Helper()
	policyBumped := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow:       []string{"Read", "mcp__gbrain__*", "mcp__*"},
			Deny:        []string{"Bash(rm:*)"},
			Ask:         []string{"WebFetch"},
			DefaultMode: "default",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	}
	writeInstructionPackDriftObserveJSON(t, harness.PermissionAuthorityPath(homePath, "claude_code"), policyBumped)
	report, err := harness.InstructionPackDrift(homePath, "claude_code", "", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Drifted {
		t.Fatalf("fixture should be drifted after authority bump: %+v", report)
	}
	return report
}

// TestEmitInstructionPackDivergence_F1_EmitOnDrift mirrors
// TestEmitPermissionPostureDivergence_F1_EmitOnDrift: a drifted report +
// bridge must mint exactly one observation batch and persist it under
// $COMPOUND_HOME/state/instruction-pack-drift-observations/.
func TestEmitInstructionPackDivergence_F1_EmitOnDrift(t *testing.T) {
	t.Parallel()
	homePath, targetPath, render := writeInstructionPackDriftObserveFixture(t)
	report := driftInstructionPackFixture(t, homePath, targetPath, render)

	emission, err := emitInstructionPackDivergence(homePath, report, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if !emission.Applied {
		t.Fatalf("expected an applied emission, got %+v", emission)
	}
	if emission.ObservationID == "" || emission.SourcePath == "" {
		t.Fatalf("emission missing observation id/source path: %+v", emission)
	}
	if _, err := os.Stat(emission.SourcePath); err != nil {
		t.Fatalf("emitted batch not persisted at %s: %v", emission.SourcePath, err)
	}
	projection, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if projection.RecordCount < 1 {
		t.Fatalf("at least one intake record expected, got %d", projection.RecordCount)
	}
}

// TestEmitInstructionPackDivergence_F2_CardEndToEnd mirrors the
// permission-posture F2: the divergence_declared card surfaces in the
// source-intake projection so the operator sees the drift.
func TestEmitInstructionPackDivergence_F2_CardEndToEnd(t *testing.T) {
	t.Parallel()
	homePath, targetPath, render := writeInstructionPackDriftObserveFixture(t)
	report := driftInstructionPackFixture(t, homePath, targetPath, render)
	if _, err := emitInstructionPackDivergence(homePath, report, cliTestNow); err != nil {
		t.Fatal(err)
	}
	projection, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	var card *observation.SourceIntakeActionCard
	for i := range projection.ActionCards {
		if projection.ActionCards[i].Kind == "divergence_declared" {
			card = &projection.ActionCards[i]
			break
		}
	}
	if card == nil {
		t.Fatalf("expected divergence_declared card, got: %+v", projection.ActionCards)
	}
}

// TestEmitInstructionPackDivergence_F3_SilenceOnMatch mirrors the
// permission-posture F3: a non-drifted report must apply ZERO
// observations and write NOTHING.
func TestEmitInstructionPackDivergence_F3_SilenceOnMatch(t *testing.T) {
	t.Parallel()
	homePath, _, _ := writeInstructionPackDriftObserveFixture(t)
	report, err := harness.InstructionPackDrift(homePath, "claude_code", "", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted {
		t.Fatalf("clean fixture should not drift before mutation: %+v", report)
	}
	filesBefore := countFiles(t, homePath)

	emission, err := emitInstructionPackDivergence(homePath, report, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if emission.Applied {
		t.Fatalf("clean report must not emit, got %+v", emission)
	}
	if got := countFiles(t, homePath); got != filesBefore {
		t.Fatalf("clean report must write nothing: files before=%d after=%d", filesBefore, got)
	}
	projection, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if projection.RecordCount != 0 || len(projection.ActionCards) != 0 {
		t.Fatalf("clean report must produce zero intake records/cards, got record_count=%d cards=%+v",
			projection.RecordCount, projection.ActionCards)
	}
}

// TestEmitInstructionPackDivergence_F4_StableIdentity mirrors the
// permission-posture F4: re-emitting on the SAME drift state yields the
// SAME observation id (intake dedupes to one card); a genuinely different
// drift state mints a DIFFERENT id (so a second card is added).
func TestEmitInstructionPackDivergence_F4_StableIdentity(t *testing.T) {
	t.Parallel()
	homePath, targetPath, render := writeInstructionPackDriftObserveFixture(t)
	report := driftInstructionPackFixture(t, homePath, targetPath, render)

	first, err := emitInstructionPackDivergence(homePath, report, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := emitInstructionPackDivergence(homePath, report, cliTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.ObservationID != second.ObservationID {
		t.Fatalf("same drift state must reproduce the same observation id: %q vs %q",
			first.ObservationID, second.ObservationID)
	}
	projection, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	count := countDivergenceCards(projection)
	if count != 1 {
		t.Fatalf("same drift state emitted twice must dedupe to one card, got %d: %+v",
			count, projection.ActionCards)
	}

	// Mutate to a genuinely different drift state and re-emit.
	policyFurtherBumped := harness.PermissionAuthority{
		SchemaVersion: harness.PermissionAuthoritySchemaVersion,
		Harness:       "claude_code",
		AuthorityRef:  harness.PermissionAuthorityRef("claude_code"),
		TargetPath:    targetPath,
		Permissions: harness.PermissionRules{
			Allow:       []string{"mcp__gbrain__*"},
			Deny:        []string{"Bash(rm:*)"},
			Ask:         []string{"WebFetch"},
			DefaultMode: "default",
		},
		AutoMode: harness.AutoModePolicy{Allow: []string{"$defaults", "Read"}},
	}
	writeInstructionPackDriftObserveJSON(t, harness.PermissionAuthorityPath(homePath, "claude_code"), policyFurtherBumped)
	reportChanged, err := harness.InstructionPackDrift(homePath, "claude_code", "", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reportChanged.Drifted {
		t.Fatalf("changed fixture still drifted: %+v", reportChanged)
	}
	third, err := emitInstructionPackDivergence(homePath, reportChanged, cliTestNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if third.ObservationID == first.ObservationID {
		t.Fatalf("a different drift state must mint a different observation id, got same: %q",
			third.ObservationID)
	}
	projectionAfter, err := observation.LoadSourceIntakeProjection(homePath, cliTestNow.Add(2*time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	count = countDivergenceCards(projectionAfter)
	if count != 2 {
		t.Fatalf("a distinct drift state must add a second distinct card (total 2), got %d: %+v",
			count, projectionAfter.ActionCards)
	}
}

// TestInstructionPackDrift_F5_GeneratedAtIgnored is the GREEN-AT-BIRTH
// teeth for the "hash excludes GeneratedAt" guarantee: writing a fresh
// pack with a different clock value while holding inputs identical must
// still report MATCH — the comparator must never trip on timestamp
// variance.
func TestInstructionPackDrift_F5_GeneratedAtIgnored(t *testing.T) {
	t.Parallel()
	homePath, _, _ := writeInstructionPackDriftObserveFixture(t)
	first, err := harness.InstructionPackDrift(homePath, "claude_code", "", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if first.Drifted {
		t.Fatalf("clean fixture should not drift: %+v", first)
	}
	// Re-render latest.json on disk with a NEW clock value (no input
	// changes — same authority, same artifact, same manifest) so the
	// only delta is GeneratedAt.
	pack, err := harness.RenderInstructionPack(homePath, "claude_code", "", cliTestNow.Add(7*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pack.GeneratedAt.Equal(cliTestNow) {
		t.Fatal("GeneratedAt must actually advance to exercise this falsifier")
	}
	second, err := harness.InstructionPackDrift(homePath, "claude_code", "", cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if second.Drifted {
		t.Fatalf("GeneratedAt-only delta must not drift; got %+v", second)
	}
}
