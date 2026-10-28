package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/sourceobs"
	"compound/internal/work"
)

func TestSourceManifestGbrainCapturesMatchingDumpIntoArtifactCustody(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}

	dump := []byte(strings.Repeat("postgres-custom-dump-block\n", 128*1024))
	dumpPath := filepath.Join(t.TempDir(), "gbrain.dump")
	if err := os.WriteFile(dumpPath, dump, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(dump)

	fixturePath := filepath.Join("..", "..", "fixtures", "sourceobs", "gbrain-db-export-synthetic.json")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var export sourceobs.GbrainDBExport
	if err := json.Unmarshal(fixture, &export); err != nil {
		t.Fatal(err)
	}
	export.DumpRef = "sha256:" + hex.EncodeToString(sum[:])
	exportPath := filepath.Join(t.TempDir(), "gbrain-export.json")
	exportBody, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exportPath, exportBody, 0o600); err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		cmdSource([]string{"manifest-create", "--family", "gbrain_db_export_manifest", "--from-export", exportPath, "--dump", dumpPath, "--json"})
	})
	manifestBody, err := os.ReadFile(sourceobs.SourceManifestPath(homePath, sourceobs.GbrainDBExportManifestFamily))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(manifestBody, &raw); err != nil {
		t.Fatal(err)
	}
	meta, _ := raw["family_meta"].(map[string]any)
	if got, _ := meta["dump_body_ref"].(string); got != "artifact:"+export.DumpRef {
		t.Fatalf("dump_body_ref = %q, want artifact:%s", got, export.DumpRef)
	}
	if got, _ := meta["dump_bytes"].(float64); int64(got) != int64(len(dump)) {
		t.Fatalf("dump_bytes = %.0f, want %d", got, len(dump))
	}

	doctor, err := sourceobs.DoctorSourceManifest(homePath, sourceobs.GbrainDBExportManifestFamily)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" || !doctor.CustodyComplete {
		t.Fatalf("owned dump doctor = %+v", doctor)
	}
	dumpCheck := sourceManifestDoctorCheckByName(doctor, "gbrain_dump_custody")
	if dumpCheck.Status != "ok" || !strings.Contains(dumpCheck.Detail, "restore/read cutover not proven") {
		t.Fatalf("dump custody check = %+v", dumpCheck)
	}

	if err := os.Remove(dumpPath); err != nil {
		t.Fatal(err)
	}
	recoveredPath := filepath.Join(t.TempDir(), "recovered.dump")
	materializeOut := captureStdout(t, func() {
		cmdSource([]string{"manifest-materialize-dump", sourceobs.GbrainDBExportManifestFamily, "--target", recoveredPath, "--json"})
	})
	var recovery sourceobs.SourceManifestMaterializeReport
	if err := json.Unmarshal([]byte(materializeOut), &recovery); err != nil {
		t.Fatalf("parse materialize report: %v\n%s", err, materializeOut)
	}
	if recovery.Bytes != int64(len(dump)) {
		t.Fatalf("recovered bytes = %d, want %d", recovery.Bytes, len(dump))
	}
	recovered, err := os.ReadFile(recoveredPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(recovered) != string(dump) {
		t.Fatal("materialized dump differs from deleted source")
	}

	artifactPath := filepath.Join(homePath, "artifacts", "sha256", strings.TrimPrefix("artifact:"+export.DumpRef, home.ArtifactRefPrefix))
	if err := os.WriteFile(artifactPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	doctor, err = sourceobs.DoctorSourceManifest(homePath, sourceobs.GbrainDBExportManifestFamily)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "fail" || doctor.CustodyComplete {
		t.Fatalf("corrupt dump doctor = %+v", doctor)
	}
	if check := sourceManifestDoctorCheckByName(doctor, "gbrain_dump_custody"); check.Status != "fail" {
		t.Fatalf("corrupt dump custody check = %+v", check)
	}
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	doctor, err = sourceobs.DoctorSourceManifest(homePath, sourceobs.GbrainDBExportManifestFamily)
	if err != nil {
		t.Fatal(err)
	}
	missingCheck := sourceManifestDoctorCheckByName(doctor, "gbrain_dump_custody")
	if doctor.Status != "fail" || missingCheck.Status != "fail" || !strings.Contains(missingCheck.Detail, filepath.Join(homePath, "artifacts", "sha256")) {
		t.Fatalf("missing dump doctor = %+v check=%+v", doctor, missingCheck)
	}
}

func sourceManifestDoctorCheckByName(report sourceobs.SourceManifestDoctorReport, name string) sourceobs.SourceManifestDoctorCheck {
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	return sourceobs.SourceManifestDoctorCheck{Name: name, Status: "missing"}
}

func TestSourceManifestGbrainFromExportCLI(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join("..", "..", "fixtures", "sourceobs", "gbrain-db-export-synthetic.json")
	createOut := captureStdout(t, func() {
		cmdSource([]string{"manifest-create", "--family", "gbrain_db_export_manifest", "--from-export", exportPath, "--json"})
	})
	var createReport sourceobs.SourceManifestReport
	if err := json.Unmarshal([]byte(createOut), &createReport); err != nil {
		t.Fatalf("parse create report: %v\n%s", err, createOut)
	}
	if createReport.ItemCount != 3 {
		t.Fatalf("create report = %+v", createReport)
	}

	showOut := captureStdout(t, func() {
		cmdSource([]string{"manifest-show", "gbrain_db_export_manifest", "--json"})
	})
	var manifest sourceobs.SourceManifest
	if err := json.Unmarshal([]byte(showOut), &manifest); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, showOut)
	}
	if manifest.FamilyMeta == nil || manifest.FamilyMeta.GenerationLabel != "current-2026-07-09" {
		t.Fatalf("expected generation_label in show, got %+v", manifest.FamilyMeta)
	}
	if len(manifest.Items) != 3 {
		t.Fatalf("items = %d", len(manifest.Items))
	}

	textOut := captureStdout(t, func() {
		cmdSource([]string{"manifest-show", "gbrain_db_export_manifest"})
	})
	if !strings.Contains(textOut, "generation_label: current-2026-07-09") {
		t.Fatalf("text show missing generation_label:\n%s", textOut)
	}

	doctorOut := captureStdout(t, func() {
		cmdSource([]string{"manifest-doctor", "gbrain_db_export_manifest", "--json"})
	})
	var doctor sourceobs.SourceManifestDoctorReport
	if err := json.Unmarshal([]byte(doctorOut), &doctor); err != nil {
		t.Fatalf("parse doctor: %v\n%s", err, doctorOut)
	}
	if doctor.Status != "ok" {
		t.Fatalf("doctor = %+v", doctor)
	}
}

func TestSourceManifestCLICommands(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(t.TempDir(), "brain", "projects")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceDir, "compound.md")
	if err := os.WriteFile(sourcePath, []byte("---\ntitle: Compound\n---\n# Compound\n\n## Facts\n- fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	createOut := captureStdout(t, func() {
		cmdSource([]string{"manifest-create", "--family", "brain_source_manifest", sourcePath, "--json"})
	})
	var createReport sourceobs.SourceManifestReport
	if err := json.Unmarshal([]byte(createOut), &createReport); err != nil {
		t.Fatalf("parse create report: %v\n%s", err, createOut)
	}
	if createReport.ItemCount != 1 || createReport.AuthorityRef == "" || createReport.ManifestPath == "" {
		t.Fatalf("create report = %+v", createReport)
	}

	showOut := captureStdout(t, func() {
		cmdSource([]string{"manifest-show", "brain_source_manifest", "--json"})
	})
	var manifest sourceobs.SourceManifest
	if err := json.Unmarshal([]byte(showOut), &manifest); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, showOut)
	}
	if len(manifest.Items) != 1 || manifest.Items[0].SourceRef != sourcePath {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Items[0].BodyPolicy != "owned_snapshot" || manifest.Items[0].BodyRef == "" {
		t.Fatalf("generic CLI capture must default to Compound-owned custody: %+v", manifest.Items[0])
	}

	doctorOut := captureStdout(t, func() {
		cmdSource([]string{"manifest-doctor", "brain_source_manifest", "--json"})
	})
	var doctor sourceobs.SourceManifestDoctorReport
	if err := json.Unmarshal([]byte(doctorOut), &doctor); err != nil {
		t.Fatalf("parse doctor: %v\n%s", err, doctorOut)
	}
	if doctor.Status != "ok" || doctor.HashMismatchCount != 0 || !doctor.CustodyComplete {
		t.Fatalf("doctor = %+v", doctor)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "restored", "compound.md")
	materializeOut := captureStdout(t, func() {
		cmdSourceManifestMaterialize([]string{"brain_source_manifest", "--item", manifest.Items[0].ID, "--target", targetPath, "--json"})
	})
	var materialize sourceobs.SourceManifestMaterializeReport
	if err := json.Unmarshal([]byte(materializeOut), &materialize); err != nil {
		t.Fatalf("parse materialize report: %v\n%s", err, materializeOut)
	}
	if materialize.BodyRef != manifest.Items[0].BodyRef || materialize.TargetPath != targetPath || materialize.Bytes == 0 {
		t.Fatalf("materialize report = %+v", materialize)
	}
	restored, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "# Compound") {
		t.Fatalf("restored body = %q", restored)
	}

	drilldownOut := captureStdout(t, func() {
		cmdSource([]string{"manifest-drilldown", "brain_source_manifest", "--item", manifest.Items[0].ID, "--json"})
	})
	var drilldown sourceobs.SourceManifestDrilldown
	if err := json.Unmarshal([]byte(drilldownOut), &drilldown); err != nil {
		t.Fatalf("parse drilldown: %v\n%s", err, drilldownOut)
	}
	if !drilldown.SourceAvailable || !drilldown.HashMatches || len(drilldown.DrilldownPath) != 3 {
		if !drilldown.BodyAvailable || !drilldown.BodyMatches {
			t.Fatalf("drilldown lost both source and owned body = %+v", drilldown)
		}
	}
}

func TestSourceManifestCLISourceInPlaceIsExplicitAndCustodyIncomplete(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "legacy.md")
	if err := os.WriteFile(sourcePath, []byte("# Legacy bridge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		cmdSource([]string{"manifest-create", "--family", "legacy_bridge", sourcePath, "--source-in-place", "--json"})
	})
	manifest, err := sourceobs.LoadSourceManifest(homePath, "legacy_bridge")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Items[0].BodyPolicy != "source_in_place" || manifest.Items[0].BodyRef != "" {
		t.Fatalf("legacy item = %+v", manifest.Items[0])
	}
	doctor, err := sourceobs.DoctorSourceManifest(homePath, "legacy_bridge")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "ok" || doctor.CustodyComplete || doctor.InventoryOnlyCount != 1 {
		t.Fatalf("legacy doctor = %+v", doctor)
	}
}

func TestSourceManifestLifecycleCLIIsDryRunByDefaultAndRecordsGovernedTransition(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(sourcePath, []byte("# Governed source lifecycle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := sourceobs.BuildOwnedSourceManifest(homePath, "governed_family", []string{sourcePath}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "sourceobs", Actor: "codex:lifecycle", SuccessPredicate: "advance governed source lifecycle", State: "ready"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex:lifecycle", cliTestNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex:lifecycle"}, cliTestNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	manifestPath := sourceobs.SourceManifestPath(homePath, "governed_family")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runSourceManifestLifecycleAdvance([]string{
		"governed_family", "--to", sourceobs.SourceLifecycleCapturing,
		"--work", w.ID, "--by", "codex:lifecycle",
		"--proof", sourceobs.SourceLifecycleGateSourceInventory + "=check://complete-roster",
		"--json",
	}, &stdout, &stderr, cliTestNow.Add(3*time.Second))
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("dry-run code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var dry map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &dry); err != nil {
		t.Fatalf("parse dry-run: %v\n%s", err, stdout.String())
	}
	if dry["mode"] != "dry-run" || dry["policy_allowed"] != true || dry["from_state"] != sourceobs.SourceLifecycleRegistered || dry["to_state"] != sourceobs.SourceLifecycleCapturing {
		t.Fatalf("dry-run report = %+v", dry)
	}
	afterDry, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, afterDry) {
		t.Fatal("dry-run mutated source manifest")
	}

	stdout.Reset()
	stderr.Reset()
	code = runSourceManifestLifecycleAdvance([]string{
		"governed_family", "--to", sourceobs.SourceLifecycleCapturing,
		"--work", w.ID, "--by", "codex:lifecycle",
		"--proof", sourceobs.SourceLifecycleGateSourceInventory + "=check://complete-roster",
		"--confirm", "--json",
	}, &stdout, &stderr, cliTestNow.Add(4*time.Second))
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("confirm code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var applied map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &applied); err != nil {
		t.Fatalf("parse apply: %v\n%s", err, stdout.String())
	}
	if applied["mode"] != "confirmed" || applied["applied"] != true || applied["action_intent_id"] == "" || applied["policy_decision_id"] == "" {
		t.Fatalf("applied report = %+v", applied)
	}
	loaded, err := sourceobs.LoadSourceManifest(homePath, "governed_family")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Lifecycle.State != sourceobs.SourceLifecycleCapturing || loaded.Lifecycle.Revision != 1 || len(loaded.Lifecycle.History) != 1 {
		t.Fatalf("lifecycle after apply = %+v", loaded.Lifecycle)
	}
	transition := loaded.Lifecycle.History[0]
	if transition.WorkID != w.ID || transition.ActionIntentID != applied["action_intent_id"] || transition.PolicyDecisionID != applied["policy_decision_id"] {
		t.Fatalf("transition governance links = %+v report=%+v", transition, applied)
	}
	afterWork, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterWork.PolicyDecisionID != transition.PolicyDecisionID || !stringSliceContains(afterWork.ActionIntentIDs, transition.ActionIntentID) {
		t.Fatalf("work did not retain governed action: %+v", afterWork)
	}

	doctor, err := sourceobs.DoctorSourceManifest(homePath, "governed_family")
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Lifecycle.State != sourceobs.SourceLifecycleCapturing || doctor.Lifecycle.NextState != sourceobs.SourceLifecycleOwnedSnapshot || doctor.Lifecycle.ReadPosture != "legacy_primary_compound_inventory" {
		t.Fatalf("doctor lifecycle = %+v", doctor.Lifecycle)
	}
}

func TestSourceManifestLifecycleCLIHoldsWriterCutoverWithoutExplicitPolicyAuthority(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(sourcePath, []byte("# Writer cutover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := sourceobs.BuildOwnedSourceManifest(homePath, "cutover_family", []string{sourcePath}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceobs.RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatal(err)
	}
	evidenceByState := map[string]string{}
	advance := func(to string, gates ...string) {
		loaded, loadErr := sourceobs.LoadSourceManifest(homePath, "cutover_family")
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		proofs := make([]sourceobs.SourceLifecycleProof, 0, len(gates))
		for _, gate := range gates {
			evidenceRef := "check://" + gate
			if bound := evidenceByState[to]; bound != "" {
				evidenceRef = bound
			}
			proofs = append(proofs, sourceobs.SourceLifecycleProof{Gate: gate, EvidenceRef: evidenceRef})
		}
		if _, advanceErr := sourceobs.AdvanceSourceManifestLifecycle(homePath, "cutover_family", sourceobs.SourceManifestLifecycleAdvanceRequest{
			ToState: to, ExpectedRevision: loaded.Lifecycle.Revision, WorkID: "work_setup", Actor: "codex:test",
			ActionIntentID: "act_setup_" + to, PolicyDecisionID: "pol_setup_" + to, Proofs: proofs,
		}, cliTestNow); advanceErr != nil {
			t.Fatalf("setup advance to %s: %v", to, advanceErr)
		}
	}
	advance(sourceobs.SourceLifecycleCapturing, sourceobs.SourceLifecycleGateSourceInventory)
	advance(sourceobs.SourceLifecycleOwnedSnapshot, sourceobs.SourceLifecycleGateExactCustody)
	verifiedGates := []string{
		sourceobs.SourceLifecycleGateIsolatedRestore,
		sourceobs.SourceLifecycleGateSemanticParity,
		sourceobs.SourceLifecycleGateRealConsumerReplay,
		sourceobs.SourceLifecycleGateLegacyUnavailableNegativeControl,
	}
	evidenceByState[sourceobs.SourceLifecycleVerified] = recordCLISourceLifecycleEvidence(t, homePath, "cutover_family", verifiedGates)
	advance(sourceobs.SourceLifecycleVerified,
		verifiedGates...,
	)

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: "sourceobs", Actor: "codex:lifecycle", SuccessPredicate: "writer cutover stays held", State: "ready"}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "codex:lifecycle", cliTestNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "codex:lifecycle"}, cliTestNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(sourceobs.SourceManifestPath(homePath, "cutover_family"))
	if err != nil {
		t.Fatal(err)
	}
	writerGates := []string{
		sourceobs.SourceLifecycleGateWriterRoster,
		sourceobs.SourceLifecycleGateWriterDestination,
		sourceobs.SourceLifecycleGateRollbackRehearsal,
		sourceobs.SourceLifecycleGatePrimaryWriterCutover,
	}
	writerEvidenceRef := recordCLISourceLifecycleEvidence(t, homePath, "cutover_family", writerGates)
	args := []string{
		"cutover_family", "--to", sourceobs.SourceLifecycleCompoundPrimaryWriter,
		"--work", w.ID, "--by", "codex:lifecycle",
		"--proof", sourceobs.SourceLifecycleGateWriterRoster + "=" + writerEvidenceRef,
		"--proof", sourceobs.SourceLifecycleGateWriterDestination + "=" + writerEvidenceRef,
		"--proof", sourceobs.SourceLifecycleGateRollbackRehearsal + "=" + writerEvidenceRef,
		"--proof", sourceobs.SourceLifecycleGatePrimaryWriterCutover + "=" + writerEvidenceRef,
		"--legacy-writer", "writer://legacy", "--compound-writer", "compound://writer",
		"--rollback", "runbook://rollback", "--writer-fence", "lease://epoch-1",
		"--writer-epoch", "1", "--quiet-window", "1h", "--confirm", "--json",
	}
	var stdout, stderr bytes.Buffer
	code := runSourceManifestLifecycleAdvance(args, &stdout, &stderr, cliTestNow.Add(3*time.Second))
	if code == 0 || !strings.Contains(stderr.String(), "policy hold") {
		t.Fatalf("cutover should be policy-held: code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	after, err := os.ReadFile(sourceobs.SourceManifestPath(homePath, "cutover_family"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("policy-held cutover mutated source manifest")
	}
}

func recordCLISourceLifecycleEvidence(t *testing.T, homePath, family string, gates []string) string {
	t.Helper()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	actor := "codex:source-evidence"
	now := cliTestNow.Add(time.Duration(len(strings.Join(gates, "|"))) * time.Second)
	w, err := store.Create(work.CreateInput{
		Owner:            "source-evidence",
		Actor:            actor,
		CanonicalSurface: family,
		SuccessPredicate: "prove " + strings.Join(gates, ","),
		State:            "ready",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, actor, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: actor}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	proofRefs := make([]string, 0, len(gates))
	for _, gate := range gates {
		proofRefs = append(proofRefs, sourceobs.SourceLifecycleGateEvidenceRef(family, gate))
	}
	closed, err := store.Close(w.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "outward_delta",
		Actor:     actor,
		Summary:   "synthetic lifecycle gate evidence",
		Changed:   true,
		ProofRefs: proofRefs,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return "transition:" + closed.Transition.ID
}

func TestSourceManifestCreateUsageNamesDirectoryModeExactly(t *testing.T) {
	t.Parallel()
	const want = "usage: compound source manifest-create --family <id> ((<source>... | --source-dir <dir>) [--source-in-place] | --from-export <export.json> [--dump <dump>]) [--json]"
	if sourceManifestCreateUsage != want {
		t.Fatalf("source manifest-create usage = %q, want %q", sourceManifestCreateUsage, want)
	}
	if fragment := strings.TrimPrefix(want, "usage: compound source "); !strings.Contains(sourceUsage(), fragment) {
		t.Fatalf("source usage does not include exact manifest-create usage fragment %q", fragment)
	}
}

func TestSourceManifestDirectoryCLIReplayMatchesExplicitFiles(t *testing.T) {
	sourceRoot := t.TempDir()
	firstPath := filepath.Join(sourceRoot, "00-first.md")
	nestedPath := filepath.Join(sourceRoot, "nested", "10-second.md")
	lastPath := filepath.Join(sourceRoot, "z-last.md")
	if err := os.MkdirAll(filepath.Dir(nestedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		firstPath:                                "# First\n",
		nestedPath:                               "# Second\n",
		lastPath:                                 "# Last\n",
		filepath.Join(sourceRoot, "ignored.txt"): "not markdown\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	const family = "directory_cli_equivalence"
	directoryHome := t.TempDir()
	if err := home.Init(directoryHome, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", directoryHome)
	captureStdout(t, func() {
		cmdSource([]string{"manifest-create", "--family", family, "--source-dir", sourceRoot, "--json"})
	})
	directoryManifest, err := sourceobs.LoadSourceManifest(directoryHome, family)
	if err != nil {
		t.Fatal(err)
	}
	if len(directoryManifest.Items) != 3 {
		t.Fatalf("directory manifest items = %d, want 3: %+v", len(directoryManifest.Items), directoryManifest.Items)
	}
	for _, item := range directoryManifest.Items {
		if item.SourceRef == sourceRoot || item.Privacy != "private" || item.BodyPolicy != "owned_snapshot" || item.BodyRef == "" {
			t.Fatalf("directory intake changed the SourceManifest item contract: %+v", item)
		}
	}

	manifestPath := sourceobs.SourceManifestPath(directoryHome, family)
	beforeReplay, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		cmdSource([]string{"manifest-create", "--family", family, "--source-dir", sourceRoot, "--json"})
	})
	afterReplay, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeReplay, afterReplay) {
		t.Fatal("directory replay changed the recorded SourceManifest")
	}

	explicitHome := t.TempDir()
	if err := home.Init(explicitHome, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", explicitHome)
	captureStdout(t, func() {
		cmdSource([]string{"manifest-create", "--family", family, firstPath, nestedPath, lastPath, "--json"})
	})
	explicitManifest, err := sourceobs.LoadSourceManifest(explicitHome, family)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(directoryManifest, explicitManifest) {
		t.Fatalf("directory manifest differs from explicit files:\ndirectory=%+v\nexplicit=%+v", directoryManifest, explicitManifest)
	}
}

const sourceManifestDirectoryConflictChild = "COMPOUND_SOURCE_MANIFEST_DIRECTORY_CONFLICT_CHILD"

func TestSourceManifestDirectoryRejectsConflictingInputModesBeforeWrite(t *testing.T) {
	t.Parallel()
	if mode := os.Getenv(sourceManifestDirectoryConflictChild); mode != "" {
		family := "directory_conflict_" + mode
		sourceRoot := os.Getenv("COMPOUND_SOURCE_MANIFEST_DIRECTORY_ROOT")
		sourcePath := os.Getenv("COMPOUND_SOURCE_MANIFEST_DIRECTORY_FILE")
		switch mode {
		case "positional":
			cmdSource([]string{"manifest-create", "--family", family, "--source-dir", sourceRoot, sourcePath})
		case "export":
			cmdSource([]string{"manifest-create", "--family", family, "--source-dir", sourceRoot, "--from-export", sourcePath})
		case "blank-export":
			cmdSource([]string{"manifest-create", "--family", family, "--source-dir", sourceRoot, "--from-export", ""})
		case "repeated-export":
			cmdSource([]string{"manifest-create", "--family", family, "--from-export", sourcePath, "--from-export", sourcePath})
		default:
			os.Exit(3)
		}
		return
	}

	sourceRoot := t.TempDir()
	sourcePath := filepath.Join(sourceRoot, "source.md")
	if err := os.WriteFile(sourcePath, []byte("# Conflict\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"positional", "export", "blank-export", "repeated-export"} {
		t.Run(mode, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSourceManifestDirectoryRejectsConflictingInputModesBeforeWrite$")
			cmd.Env = append(os.Environ(),
				sourceManifestDirectoryConflictChild+"="+mode,
				"COMPOUND_HOME="+homePath,
				"COMPOUND_SOURCE_MANIFEST_DIRECTORY_ROOT="+sourceRoot,
				"COMPOUND_SOURCE_MANIFEST_DIRECTORY_FILE="+sourcePath,
			)
			output, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 2 {
				t.Fatalf("conflicting mode exit = %v, output:\n%s", err, output)
			}
			if !strings.Contains(string(output), "--source-dir") {
				t.Fatalf("conflicting mode error does not name --source-dir:\n%s", output)
			}
			family := "directory_conflict_" + mode
			if _, err := os.Stat(sourceobs.SourceManifestPath(homePath, family)); !os.IsNotExist(err) {
				t.Fatalf("conflicting mode wrote manifest authority: %v", err)
			}
		})
	}
}

const sourceManifestDirectoryAtomicChild = "COMPOUND_SOURCE_MANIFEST_DIRECTORY_ATOMIC_CHILD"

func TestSourceManifestDirectoryErrorIsAtomicBeforeCustodyOrAuthority(t *testing.T) {
	t.Parallel()
	if os.Getenv(sourceManifestDirectoryAtomicChild) != "" {
		cmdSource([]string{
			"manifest-create",
			"--family", "directory_atomic_error",
			"--source-dir", os.Getenv("COMPOUND_SOURCE_MANIFEST_DIRECTORY_ROOT"),
		})
		return
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "00-before.md"), []byte("# Before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("# Outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sourceRoot, "z-escape.md")); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSourceManifestDirectoryErrorIsAtomicBeforeCustodyOrAuthority$")
	cmd.Env = append(os.Environ(),
		sourceManifestDirectoryAtomicChild+"=1",
		"COMPOUND_HOME="+homePath,
		"COMPOUND_SOURCE_MANIFEST_DIRECTORY_ROOT="+sourceRoot,
	)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("directory error exit = %v, output:\n%s", err, output)
	}
	if !strings.Contains(string(output), "symlink") {
		t.Fatalf("directory error does not explain symlink refusal:\n%s", output)
	}
	if _, err := os.Stat(sourceobs.SourceManifestPath(homePath, "directory_atomic_error")); !os.IsNotExist(err) {
		t.Fatalf("directory error wrote manifest authority: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(homePath, "artifacts", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory error published partial artifact custody: %v", entries)
	}
}
