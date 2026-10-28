package sourceobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var lifecycleTestNow = time.Date(2026, 7, 14, 9, 30, 0, 0, time.UTC)

func TestSourceManifestLifecycleRejectsSkipsAndProofSubstitutionWithoutMutation(t *testing.T) {
	homePath, family, _ := lifecycleOwnedManifest(t)
	manifestPath := SourceManifestPath(homePath, family)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = AdvanceSourceManifestLifecycle(homePath, family, SourceManifestLifecycleAdvanceRequest{
		ToState:          SourceLifecycleOwnedSnapshot,
		ExpectedRevision: 0,
		WorkID:           "work_skip",
		Actor:            "codex:test",
		ActionIntentID:   "act_skip",
		PolicyDecisionID: "pol_skip",
		Proofs:           lifecycleProofs(SourceLifecycleGateExactCustody),
	}, lifecycleTestNow)
	if err == nil || !strings.Contains(err.Error(), "illegal source lifecycle transition registered -> owned_snapshot") {
		t.Fatalf("skip transition error = %v", err)
	}
	assertFileBytes(t, manifestPath, before)

	_, err = AdvanceSourceManifestLifecycle(homePath, family, SourceManifestLifecycleAdvanceRequest{
		ToState:          SourceLifecycleCapturing,
		ExpectedRevision: 0,
		WorkID:           "work_substitution",
		Actor:            "codex:test",
		ActionIntentID:   "act_substitution",
		PolicyDecisionID: "pol_substitution",
		Proofs:           lifecycleProofs(SourceLifecycleGateExactCustody),
	}, lifecycleTestNow)
	if err == nil || !strings.Contains(err.Error(), "requires exactly [source_inventory]") {
		t.Fatalf("proof substitution error = %v", err)
	}
	assertFileBytes(t, manifestPath, before)

	loaded, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Lifecycle == nil || loaded.Lifecycle.State != SourceLifecycleRegistered || loaded.Lifecycle.Revision != 0 {
		t.Fatalf("failed advances mutated lifecycle: %+v", loaded.Lifecycle)
	}
}

func TestSourceManifestLifecycleEnforcesEveryForwardGateAndMeasuredQuietWindow(t *testing.T) {
	homePath, family, _ := lifecycleOwnedManifest(t)
	now := lifecycleTestNow

	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCapturing, lifecycleProofs(SourceLifecycleGateSourceInventory), nil, now)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleOwnedSnapshot, lifecycleProofs(SourceLifecycleGateExactCustody), nil, now.Add(time.Minute))

	verifiedProofs := lifecycleProofs(
		SourceLifecycleGateLegacyUnavailableNegativeControl,
		SourceLifecycleGateRealConsumerReplay,
		SourceLifecycleGateSemanticParity,
		SourceLifecycleGateIsolatedRestore,
	)
	missingNegative := verifiedProofs[1:]
	assertLifecycleAdvanceFailsUnchanged(t, homePath, family, SourceLifecycleVerified, missingNegative, nil, "legacy_unavailable_negative_control")
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleVerified, verifiedProofs, nil, now.Add(2*time.Minute))

	handoff := &SourceWriterHandoff{
		LegacyWriterRefs:           []string{"writer://legacy/editor", "job://legacy/sync"},
		CompoundWriterRef:          "compound://writers/source-family",
		RollbackRef:                "runbook://restore-legacy-writer",
		FenceRef:                   "lease://source-family/epoch-1",
		WriterEpoch:                1,
		RequiredQuietWindowSeconds: 3600,
	}
	writerProofs := lifecycleProofs(
		SourceLifecycleGateWriterRoster,
		SourceLifecycleGateWriterDestination,
		SourceLifecycleGateRollbackRehearsal,
		SourceLifecycleGatePrimaryWriterCutover,
	)
	assertLifecycleAdvanceFailsUnchanged(t, homePath, family, SourceLifecycleCompoundPrimaryWriter, writerProofs[:3], handoff, "primary_writer_cutover")
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCompoundPrimaryWriter, writerProofs, handoff, now.Add(3*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleLegacyFrozen, lifecycleProofs(SourceLifecycleGateLegacyFreeze), nil, now.Add(4*time.Minute))

	shortWindow := lifecycleProofs(SourceLifecycleGateQuietWindowMeasurement)
	shortWindow[0].QuietWindow = &SourceQuietWindowMeasurement{
		StartedAt:         now.Add(5 * time.Minute).Format(time.RFC3339),
		EndedAt:           now.Add(35 * time.Minute).Format(time.RFC3339),
		UnexplainedReads:  0,
		UnexplainedWrites: 0,
	}
	assertLifecycleAdvanceFailsUnchanged(t, homePath, family, SourceLifecycleQuietWindowProven, shortWindow, nil, "shorter than required")

	dirtyWindow := lifecycleProofs(SourceLifecycleGateQuietWindowMeasurement)
	dirtyWindow[0].QuietWindow = &SourceQuietWindowMeasurement{
		StartedAt:         now.Add(5 * time.Minute).Format(time.RFC3339),
		EndedAt:           now.Add(70 * time.Minute).Format(time.RFC3339),
		UnexplainedReads:  0,
		UnexplainedWrites: 1,
	}
	assertLifecycleAdvanceFailsUnchanged(t, homePath, family, SourceLifecycleQuietWindowProven, dirtyWindow, nil, "unexplained legacy traffic")

	quietProof := lifecycleProofs(SourceLifecycleGateQuietWindowMeasurement)
	quietProof[0].QuietWindow = &SourceQuietWindowMeasurement{
		StartedAt:         now.Add(5 * time.Minute).Format(time.RFC3339),
		EndedAt:           now.Add(70 * time.Minute).Format(time.RFC3339),
		ObservedReads:     17,
		ObservedWrites:    4,
		UnexplainedReads:  0,
		UnexplainedWrites: 0,
	}
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleQuietWindowProven, quietProof, nil, now.Add(71*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleArchived, lifecycleProofs(SourceLifecycleGateArchiveVerified, SourceLifecycleGateRollbackRehearsal), nil, now.Add(72*time.Minute))

	badRetirement := lifecycleProofs(SourceLifecycleGateOperatorRetirementDecision)
	badRetirement[0].EvidenceRef = "review://not-an-operator-decision"
	assertLifecycleAdvanceFailsUnchangedWithActor(t, homePath, family, SourceLifecycleRetired, badRetirement, nil, "operator:test", "decision://")
	validDecisionWrongActor := lifecycleProofs(SourceLifecycleGateOperatorRetirementDecision)
	validDecisionWrongActor[0].EvidenceRef = "decision://retire-source-family-2026-07-14"
	assertLifecycleAdvanceFailsUnchangedWithActor(t, homePath, family, SourceLifecycleRetired, validDecisionWrongActor, nil, "codex:test", "operator actor")

	retirement := lifecycleProofs(SourceLifecycleGateOperatorRetirementDecision)
	retirement[0].EvidenceRef = "decision://retire-source-family-2026-07-14"
	mustAdvanceLifecycleWithActor(t, homePath, family, SourceLifecycleRetired, retirement, nil, "operator:test", now.Add(73*time.Minute))

	manifest, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	status := SourceManifestLifecycleStatus(manifest)
	if status.State != SourceLifecycleRetired || !status.Terminal || status.WritePosture != "compound_authoritative_legacy_retired" {
		t.Fatalf("retired status = %+v", status)
	}
	if len(manifest.Lifecycle.History) != int(manifest.Lifecycle.Revision) || manifest.Lifecycle.Revision != 8 {
		t.Fatalf("lifecycle history/revision = %d/%d", len(manifest.Lifecycle.History), manifest.Lifecycle.Revision)
	}
}

func TestSourceManifestLifecycleRollbackEdgesAreExplicitAndRetirementIsTerminal(t *testing.T) {
	homePath, family, _ := lifecycleOwnedManifest(t)
	now := lifecycleTestNow
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCapturing, lifecycleProofs(SourceLifecycleGateSourceInventory), nil, now)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleOwnedSnapshot, lifecycleProofs(SourceLifecycleGateExactCustody), nil, now.Add(time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleVerified, lifecycleProofs(SourceLifecycleGateIsolatedRestore, SourceLifecycleGateSemanticParity, SourceLifecycleGateRealConsumerReplay, SourceLifecycleGateLegacyUnavailableNegativeControl), nil, now.Add(2*time.Minute))

	handoff := func(epoch int64) *SourceWriterHandoff {
		return &SourceWriterHandoff{
			LegacyWriterRefs: []string{"writer://legacy"}, CompoundWriterRef: "compound://writer",
			RollbackRef: "runbook://rollback", FenceRef: "lease://epoch", WriterEpoch: epoch,
			RequiredQuietWindowSeconds: 60,
		}
	}
	writerProofs := lifecycleProofs(SourceLifecycleGateWriterRoster, SourceLifecycleGateWriterDestination, SourceLifecycleGateRollbackRehearsal, SourceLifecycleGatePrimaryWriterCutover)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCompoundPrimaryWriter, writerProofs, handoff(1), now.Add(3*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleVerified, lifecycleProofs(SourceLifecycleGateWriterRollback), nil, now.Add(4*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCompoundPrimaryWriter, writerProofs, handoff(2), now.Add(5*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleLegacyFrozen, lifecycleProofs(SourceLifecycleGateLegacyFreeze), nil, now.Add(6*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCompoundPrimaryWriter, lifecycleProofs(SourceLifecycleGateLegacyUnfreeze), nil, now.Add(7*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleLegacyFrozen, lifecycleProofs(SourceLifecycleGateLegacyFreeze), nil, now.Add(8*time.Minute))

	quiet := lifecycleProofs(SourceLifecycleGateQuietWindowMeasurement)
	quiet[0].QuietWindow = &SourceQuietWindowMeasurement{StartedAt: now.Add(8 * time.Minute).Format(time.RFC3339), EndedAt: now.Add(10 * time.Minute).Format(time.RFC3339)}
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleQuietWindowProven, quiet, nil, now.Add(11*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleLegacyFrozen, lifecycleProofs(SourceLifecycleGateQuietWindowInvalidated), nil, now.Add(12*time.Minute))
	quiet[0].QuietWindow = &SourceQuietWindowMeasurement{StartedAt: now.Add(12 * time.Minute).Format(time.RFC3339), EndedAt: now.Add(14 * time.Minute).Format(time.RFC3339)}
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleQuietWindowProven, quiet, nil, now.Add(15*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleArchived, lifecycleProofs(SourceLifecycleGateArchiveVerified, SourceLifecycleGateRollbackRehearsal), nil, now.Add(16*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleQuietWindowProven, lifecycleProofs(SourceLifecycleGateArchiveRollback), nil, now.Add(17*time.Minute))
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleArchived, lifecycleProofs(SourceLifecycleGateArchiveVerified, SourceLifecycleGateRollbackRehearsal), nil, now.Add(18*time.Minute))
	retirement := lifecycleProofs(SourceLifecycleGateOperatorRetirementDecision)
	retirement[0].EvidenceRef = "decision://retired"
	mustAdvanceLifecycleWithActor(t, homePath, family, SourceLifecycleRetired, retirement, nil, "operator:test", now.Add(19*time.Minute))
	assertLifecycleAdvanceFailsUnchangedWithActor(t, homePath, family, SourceLifecycleArchived, lifecycleProofs(SourceLifecycleGateArchiveRollback), nil, "operator:test", "terminal")
}

func TestSourceManifestLifecycleLegacySchemasLoadRegisteredAndRecoveryStaysAvailable(t *testing.T) {
	for _, schema := range []string{legacySourceManifestSchemaVersion, custodySourceManifestSchemaVersion} {
		t.Run(schema, func(t *testing.T) {
			homePath, family, sourcePath := lifecycleOwnedManifest(t)
			path := SourceManifestPath(homePath, family)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body = []byte(strings.Replace(string(body), SourceManifestSchemaVersion, schema, 1))
			body = []byte(strings.Replace(string(body), "\n  \"lifecycle\": {\n    \"state\": \"registered\",\n    \"revision\": 0,\n    \"history\": []\n  },", "", 1))
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			manifest, err := LoadSourceManifest(homePath, family)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.SchemaVersion != schema || manifest.Lifecycle == nil || manifest.Lifecycle.State != SourceLifecycleRegistered {
				t.Fatalf("legacy load = schema %q lifecycle %+v", manifest.SchemaVersion, manifest.Lifecycle)
			}
			if err := os.Remove(sourcePath); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "recovered.md")
			if _, err := MaterializeSourceManifestItem(homePath, family, manifest.Items[0].ID, target); err != nil {
				t.Fatalf("pre-retirement recovery must remain available: %v", err)
			}
		})
	}
}

func TestSourceManifestCreateAndLifecycleAdvanceShareAuthorityLock(t *testing.T) {
	homePath, family, _ := lifecycleOwnedManifest(t)
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "new.md"), []byte("# New source\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- withSourceManifestAuthorityLock(homePath, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	advanceDone := make(chan error, 1)
	go func() {
		_, err := AdvanceSourceManifestLifecycle(homePath, family, SourceManifestLifecycleAdvanceRequest{
			ToState:          SourceLifecycleCapturing,
			ExpectedRevision: 0,
			WorkID:           "work_shared_authority_lock",
			Actor:            "codex:test",
			ActionIntentID:   "act_shared_authority_lock",
			PolicyDecisionID: "pol_shared_authority_lock",
			Proofs:           lifecycleProofs(SourceLifecycleGateSourceInventory),
		}, lifecycleTestNow.Add(time.Minute))
		advanceDone <- err
	}()
	createDone := make(chan error, 1)
	go func() {
		_, _, err := CreateSourceManifestDirectory(homePath, family, sourceRoot, false, lifecycleTestNow.Add(2*time.Minute))
		createDone <- err
	}()

	select {
	case err := <-advanceDone:
		t.Fatalf("lifecycle advance bypassed shared authority lock: %v", err)
	case err := <-createDone:
		t.Fatalf("directory create bypassed shared authority lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-advanceDone; err != nil {
		t.Fatal(err)
	}
	if err := <-createDone; err != nil {
		t.Fatal(err)
	}

	manifest, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Items) != 2 {
		t.Fatalf("concurrent create/lifecycle lost items: %+v", manifest.Items)
	}
	if manifest.Lifecycle == nil || manifest.Lifecycle.State != SourceLifecycleCapturing || manifest.Lifecycle.Revision != 1 {
		t.Fatalf("concurrent create/lifecycle lost lifecycle: %+v", manifest.Lifecycle)
	}
}

func lifecycleOwnedManifest(t *testing.T) (string, string, string) {
	t.Helper()
	homePath := t.TempDir()
	family := "lifecycle_test_family"
	sourcePath := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(sourcePath, []byte("# Source family\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildOwnedSourceManifest(homePath, family, []string{sourcePath}, lifecycleTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordSourceManifest(homePath, manifest); err != nil {
		t.Fatal(err)
	}
	return homePath, family, sourcePath
}

func lifecycleProofs(gates ...string) []SourceLifecycleProof {
	proofs := make([]SourceLifecycleProof, 0, len(gates))
	for _, gate := range gates {
		proofs = append(proofs, SourceLifecycleProof{Gate: gate, EvidenceRef: "check://" + gate})
	}
	return proofs
}

func mustAdvanceLifecycle(t *testing.T, homePath, family, to string, proofs []SourceLifecycleProof, handoff *SourceWriterHandoff, now time.Time) SourceManifestLifecycleAdvanceReport {
	t.Helper()
	return mustAdvanceLifecycleWithActor(t, homePath, family, to, proofs, handoff, "codex:test", now)
}

func mustAdvanceLifecycleWithActor(t *testing.T, homePath, family, to string, proofs []SourceLifecycleProof, handoff *SourceWriterHandoff, actor string, now time.Time) SourceManifestLifecycleAdvanceReport {
	t.Helper()
	proofs = bindLifecycleTestEnvelopeEvidence(t, homePath, family, proofs)
	manifest, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	revision := manifest.Lifecycle.Revision
	report, err := AdvanceSourceManifestLifecycle(homePath, family, SourceManifestLifecycleAdvanceRequest{
		ToState:          to,
		ExpectedRevision: revision,
		WorkID:           "work_lifecycle_test",
		Actor:            actor,
		ActionIntentID:   "act_lifecycle_" + to,
		PolicyDecisionID: "pol_lifecycle_" + to,
		Proofs:           proofs,
		WriterHandoff:    handoff,
	}, now)
	if err != nil {
		t.Fatalf("advance to %s: %v", to, err)
	}
	if !report.Applied || report.ToState != to || report.Revision != revision+1 {
		t.Fatalf("advance report = %+v", report)
	}
	return report
}

func assertLifecycleAdvanceFailsUnchanged(t *testing.T, homePath, family, to string, proofs []SourceLifecycleProof, handoff *SourceWriterHandoff, want string) {
	t.Helper()
	assertLifecycleAdvanceFailsUnchangedWithActor(t, homePath, family, to, proofs, handoff, "codex:test", want)
}

func assertLifecycleAdvanceFailsUnchangedWithActor(t *testing.T, homePath, family, to string, proofs []SourceLifecycleProof, handoff *SourceWriterHandoff, actor, want string) {
	t.Helper()
	proofs = bindLifecycleTestEnvelopeEvidence(t, homePath, family, proofs)
	path := SourceManifestPath(homePath, family)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AdvanceSourceManifestLifecycle(homePath, family, SourceManifestLifecycleAdvanceRequest{
		ToState:          to,
		ExpectedRevision: manifest.Lifecycle.Revision,
		WorkID:           "work_lifecycle_rejected",
		Actor:            actor,
		ActionIntentID:   "act_lifecycle_rejected",
		PolicyDecisionID: "pol_lifecycle_rejected",
		Proofs:           proofs,
		WriterHandoff:    handoff,
	}, lifecycleTestNow.Add(24*time.Hour))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("advance to %s error = %v, want substring %q", to, err, want)
	}
	assertFileBytes(t, path, before)
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s changed after rejected transition", path)
	}
}
