package authority

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/mem"
)

const (
	testServedOrientationProofMode  = "served_orientation_authority"
	testServedOrientationBlockBegin = "<!-- BEGIN COMPOUND SERVED ORIENTATION CONTRACT -->"
	testServedOrientationBlockEnd   = "<!-- END COMPOUND SERVED ORIENTATION CONTRACT -->"
	testInstalledHarnessHomeKey     = "installed_harness_home"
)

// TestOSAbsorptionServedOrientationRowsRequireProofMode is the removal
// falsifier: EffectiveContract-owned OS absorption rows are structurally
// served-orientation rows. Removing or changing proof_mode must fail matrix
// validation and must not turn the doctor proof into "manual".
func TestOSAbsorptionServedOrientationRowsRequireProofMode(t *testing.T) {
	relevant := row(
		"os_source_example_live_start",
		"/synthetic/example-os/LIVE_START.md",
		"absorbed",
		osAbsorptionEffectiveContractAuthorityRef,
		"compound orient and served client contract",
		"",
	)
	relevant.CurrentRole = "source_in_place_evidence"
	if !osAbsorptionNeedsServedOrientationProof(relevant) {
		t.Fatalf("EffectiveContract-owned row must need served-orientation proof even when proof_mode is removed: %+v", relevant)
	}
	if err := validateOSAbsorptionMatrix(OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{relevant},
	}); err == nil || !strings.Contains(err.Error(), osAbsorptionProofModeServedOrient) {
		t.Fatalf("missing proof_mode should fail the matrix with the required mode named, got %v", err)
	}

	relevant.ProofMode = osAbsorptionProofModeDecisions
	if err := validateOSAbsorptionMatrix(OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{relevant},
	}); err == nil || !strings.Contains(err.Error(), osAbsorptionProofModeServedOrient) {
		t.Fatalf("wrong proof_mode should fail the matrix with the required mode named, got %v", err)
	}

	relevant.ProofMode = osAbsorptionProofModeServedOrient
	if err := validateOSAbsorptionMatrix(OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{relevant},
	}); err != nil {
		t.Fatalf("required proof_mode should validate: %v", err)
	}
}

// TestValidateOSAbsorptionServedEntrypointRejectsAnyNonCompoundOrientHeading
// proves the retired-heading check is genericized: it rejects ANY
// "## Orient first — " heading that is not the required Compound heading,
// not just one hardcoded legacy string.
func TestValidateOSAbsorptionServedEntrypointRejectsAnyNonCompoundOrientHeading(t *testing.T) {
	sourceRef := "/synthetic/example-source/ENTRYPOINT.md"
	body := strings.Join(append(validServedOrientationLines("claude", sourceRef),
		"",
		"## Orient first — Arbitrary Other Control Plane",
		"some retired predecessor entrypoint heading, not the required one",
	), "\n")
	err := validateOSAbsorptionServedEntrypoint("claude", sourceRef, body)
	if err == nil {
		t.Fatalf("expected rejection of an arbitrary non-Compound orient heading, got nil error")
	}
	if !strings.Contains(err.Error(), "retired orient heading") {
		t.Fatalf("error %q does not name the retired-heading reason", err.Error())
	}
	if !strings.Contains(err.Error(), "Arbitrary Other Control Plane") {
		t.Fatalf("error %q should quote the offending heading", err.Error())
	}
}

func TestValidateOSAbsorptionServedEntrypointEnforcesMemoryAuthorityBoundary(t *testing.T) {
	sourceRef := "/synthetic/example-source/ENTRYPOINT.md"
	required := []string{
		"Compound owns durable semantic meaning and promotion.",
		"brain is source/projection/migration evidence.",
		"gbrain is a bridge/index only.",
	}
	validLines := validServedOrientationLines("codex", sourceRef)

	for _, missing := range required {
		t.Run("requires "+missing, func(t *testing.T) {
			lines := make([]string, 0, len(validLines)-1)
			for _, line := range validLines {
				if line != missing {
					lines = append(lines, line)
				}
			}
			err := validateOSAbsorptionServedEntrypoint("codex", sourceRef, strings.Join(lines, "\n"))
			if err == nil {
				t.Fatalf("expected rejection when required memory-authority guidance %q is missing", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Fatalf("error %q should name missing guidance %q", err.Error(), missing)
			}
		})
	}

	for _, contradictory := range []string{
		"**~/brain** is the curated markdown **source of truth**.",
		"brain is canon.",
		"If curated brain and gbrain disagree, curated brain wins.",
		"Durable outcomes land directly in `~/brain`.",
		"brain remains the durable memory authority.",
		"Save durable facts to ~/brain.",
		"Hermes cron writes to ~/brain/reviews; curate to canon.",
		"gbrain remains the canonical memory authority.",
		"Do not forget: brain remains the durable memory authority.",
		"brain is not merely an index; it remains the durable memory authority.",
		"brain remains the durable\nmemory authority.",
		"brain takes precedence over Compound.",
		"brain is not merely evidence; it is the authority.",
		"Keep durable decisions in ~/brain.",
		"The final authority for durable memory is brain.",
		"brain is the system of record.",
		"In brain, store durable facts.",
		`Retired quotation: "brain is canon." brain remains the durable memory authority.`,
	} {
		t.Run("rejects "+contradictory, func(t *testing.T) {
			body := strings.Join(append(append([]string{}, validLines...), contradictory), "\n")
			err := validateOSAbsorptionServedEntrypoint("codex", sourceRef, body)
			if err == nil {
				t.Fatalf("expected rejection of contradictory memory-authority guidance %q", contradictory)
			}
			if !strings.Contains(err.Error(), "contradictory memory authority guidance") {
				t.Fatalf("error %q does not identify the memory-authority contradiction", err.Error())
			}
		})
	}
}

func TestValidateOSAbsorptionServedEntrypointRejectsFirstActionRecallCaseInsensitively(t *testing.T) {
	sourceRef := "/synthetic/example-source/ENTRYPOINT.md"
	for _, directive := range []string{
		"first ACTION is RECALL from gbrain.",
		"Before any work, recall from gbrain.",
		"BEFORE ANY WORK, RECALL FROM GBRAIN.",
	} {
		t.Run(directive, func(t *testing.T) {
			body := strings.Join(append(validServedOrientationLines("codex", sourceRef), directive), "\n")
			err := validateOSAbsorptionServedEntrypoint("codex", sourceRef, body)
			if err == nil || !strings.Contains(err.Error(), "retired first-action directive") {
				t.Fatalf("case-insensitive first-action recall must be rejected, got %v", err)
			}
		})
	}
}

func TestValidateOSAbsorptionServedEntrypointUsesOnlyActiveInstructions(t *testing.T) {
	sourceRef := "/synthetic/example-source/ENTRYPOINT.md"
	requiredAuthority := strings.Join([]string{
		osAbsorptionRequiredMeaningAuthority,
		osAbsorptionRequiredBrainRole,
		osAbsorptionRequiredGBrainRole,
	}, "\n")
	activeWithoutAuthority := strings.Join([]string{
		testServedOrientationBlockBegin,
		"## Orient first — Compound (the served contract)",
		"Run `compound orient --harness codex --task \"<task>\"`.",
		"These files remain source-in-place evidence and rollback:",
		sourceRef,
		testServedOrientationBlockEnd,
	}, "\n")

	for _, hidden := range []string{
		"<!--\n" + requiredAuthority + "\n-->",
		"```text\n" + requiredAuthority + "\n```",
		"````text\nignored outer example\n```\n" + strings.Join(validServedOrientationLines("codex", sourceRef), "\n") + "\n````",
		"> " + strings.ReplaceAll(requiredAuthority, "\n", "\n> "),
		"`" + strings.ReplaceAll(requiredAuthority, "\n", " ") + "`",
		`Retired quotation: "` + strings.ReplaceAll(requiredAuthority, "\n", " ") + `"`,
		"   " + strings.ReplaceAll(strings.Join(validServedOrientationLines("codex", sourceRef), "\n"), "\n", "\n   "),
	} {
		body := activeWithoutAuthority + "\n" + hidden
		err := validateOSAbsorptionServedEntrypoint("codex", sourceRef, body)
		if err == nil {
			t.Fatalf("authority roles hidden outside active instructions must not satisfy the guard:\n%s", hidden)
		}
		if !strings.Contains(err.Error(), "missing") {
			t.Fatalf("hidden authority roles should report missing active guidance, got %q", err)
		}
	}

	body := strings.Join(append(validServedOrientationLines("codex", sourceRef),
		"<!-- brain remains the durable memory authority. -->",
		"```text",
		"Save durable facts to ~/brain.",
		"```",
		"> Retired guidance: gbrain is canon.",
		`Retired quotation: "brain is the source of truth."`,
		"Never save durable facts directly to ~/brain; route them through Compound.",
		"Never keep durable decisions in ~/brain; route them through Compound.",
		"Never recall first from gbrain; Compound orient remains the first action.",
		"brain must not take precedence over Compound.",
		"Compound is the durable semantic authority; brain remains evidence.",
		"Save the task output to a local file; brain is evidence, not authority.",
		"The canonical role for gbrain is bridge/index only.",
		`The statement "brain is the source of truth" is a retired quotation kept for migration evidence.`,
	), "\n")
	if err := validateOSAbsorptionServedEntrypoint("codex", sourceRef, body); err != nil {
		t.Fatalf("commented, fenced, quoted-retired, and explicitly forbidden contradictions are not active guidance: %v", err)
	}

	withRelativeRef := validServedOrientationLines("codex", sourceRef)
	withRelativeRef = append(withRelativeRef[:len(withRelativeRef)-1], "relative/rollback.md", withRelativeRef[len(withRelativeRef)-1])
	if err := validateOSAbsorptionServedEntrypoint("codex", sourceRef, strings.Join(withRelativeRef, "\n")); err == nil ||
		!strings.Contains(err.Error(), "invalid source ref") {
		t.Fatalf("managed rollback refs must be absolute, got %v", err)
	}
}

func TestValidateOSAbsorptionServedEntrypointRejectsExtraRollbackRef(t *testing.T) {
	sourceRefs := []string{
		"/synthetic/example-os/LIVE_START.md",
		"/synthetic/example-os/universe/WHOLE_SYSTEM_MAP.md",
	}
	lines := validServedOrientationLines("codex", sourceRefs...)
	lines = append(lines[:len(lines)-1], "/synthetic/example-os/EXTRA.md", lines[len(lines)-1])

	err := validateOSAbsorptionServedEntrypointSources("codex", sourceRefs, strings.Join(lines, "\n"))
	if err == nil || !strings.Contains(err.Error(), "exact rollback refs") {
		t.Fatalf("managed block must reject any extra active absolute rollback ref, got %v", err)
	}
}

func TestOSAbsorptionDoctorRequiresExactServedOrientationSourceRows(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*OSAbsorptionSourceMatrix)
	}{
		{
			name: "deleting both rows fails",
			mutate: func(matrix *OSAbsorptionSourceMatrix) {
				matrix.Rows = nil
			},
		},
		{
			name: "relabeling both rows and owner fields fails",
			mutate: func(matrix *OSAbsorptionSourceMatrix) {
				for i := range matrix.Rows {
					matrix.Rows[i].SourceRef = filepath.Join(filepath.Dir(matrix.Rows[i].SourceRef), "OTHER.md")
					matrix.Rows[i].CompoundOwner = "compound://registries/unrelated"
					matrix.Rows[i].ProofMode = ""
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := installServedOrientationFixture(t)
			matrix, err := LoadOSAbsorptionMatrix(fixture.compoundHome)
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(&matrix)
			writeJSONFixture(t, OSAbsorptionMatrixPath(fixture.compoundHome), matrix)

			doctor, err := OSAbsorptionDoctor(fixture.compoundHome)
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			check, ok := findOSAbsorptionDoctorCheck(doctor.Checks, "served_orientation_source_rows")
			if !ok {
				t.Fatalf("served-orientation coverage check missing: %+v", doctor.Checks)
			}
			if check.Status != "fail" || !strings.Contains(check.Detail, "LIVE_START.md") ||
				!strings.Contains(check.Detail, "WHOLE_SYSTEM_MAP.md") {
				t.Fatalf("expected both derived source rows to fail independently of row owner/mode: %+v", check)
			}
			if doctor.Status != "fail" {
				t.Fatalf("missing served source rows must fail doctor: %+v", doctor)
			}
		})
	}
}

func TestServedOrientationSourceRowsUsesInstalledHarnessExpectation(t *testing.T) {
	t.Run("public clone without either marker is not applicable", func(t *testing.T) {
		check, err := osAbsorptionServedOrientationSourceRowsCheck(t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if check.Status != "ok" || !strings.Contains(check.Detail, "not applicable") {
			t.Fatalf("public clone should remain not applicable: %+v", check)
		}
	})

	t.Run("installed harness marker without owned source manifest fails", func(t *testing.T) {
		homePath := filepath.Join(t.TempDir(), "compound-home")
		installedHome := t.TempDir()
		seedInstalledHarnessHomeRegistry(t, homePath, installedHome)

		check, err := osAbsorptionServedOrientationSourceRowsCheck(homePath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if check.Status != "fail" ||
			!strings.Contains(check.Detail, InstalledHarnessHomeKey) ||
			!strings.Contains(check.Detail, "compound://sources/manifests/os") {
			t.Fatalf("installed expectation must fail without owned SourceManifest custody: %+v", check)
		}
	})
}

func TestServedOrientationRowsUseOwnedSourceManifestWithoutCopiedRoot(t *testing.T) {
	manifest := osAbsorptionServedManifest{
		SourceFamily: "os",
		Lifecycle: osAbsorptionServedManifestLifecycle{
			State: "legacy_frozen",
		},
		Items: []osAbsorptionServedManifestItem{
			{
				ID:         "srcitem_live_start",
				SourceRef:  "/synthetic/retired-os/LIVE_START.md",
				BodyPolicy: "owned_snapshot",
				BodyRef:    "artifact:sha256:live",
			},
			{
				ID:         "srcitem_system_map",
				SourceRef:  "/synthetic/retired-os/universe/WHOLE_SYSTEM_MAP.md",
				BodyPolicy: "owned_snapshot",
				BodyRef:    "artifact:sha256:map",
			},
		},
	}
	rows := []OSAbsorptionMatrixRow{
		{
			ID:            "live-start",
			SourceRef:     osAbsorptionServedManifestItemRef("srcitem_live_start"),
			CompoundOwner: osAbsorptionEffectiveContractAuthorityRef,
			ProofMode:     osAbsorptionProofModeServedOrient,
		},
		{
			ID:            "system-map",
			SourceRef:     osAbsorptionServedManifestItemRef("srcitem_system_map"),
			CompoundOwner: osAbsorptionEffectiveContractAuthorityRef,
			ProofMode:     osAbsorptionProofModeServedOrient,
		},
	}

	check := osAbsorptionServedManifestSourceRowsCheck(manifest, rows)
	if check.Status != "ok" {
		t.Fatalf("owned SourceManifest items should satisfy served source coverage: %+v", check)
	}

	rows[1].SourceRef = osAbsorptionServedManifestItemRef("srcitem_live_start")
	check = osAbsorptionServedManifestSourceRowsCheck(manifest, rows)
	if check.Status != "fail" || !strings.Contains(check.Detail, "WHOLE_SYSTEM_MAP.md") {
		t.Fatalf("duplicate manifest item must not satisfy both source roles: %+v", check)
	}
}

func TestGeneratedCompoundInstructionEntrypointReplacesLegacyRollbackBlock(t *testing.T) {
	body := strings.Join([]string{
		"<!-- BEGIN COMPOUND GENERATED INSTRUCTIONS harness=codex -->",
		"## Compound-generated contract",
		"Generated by `compound contract compile` for **codex**.",
		"- Meaning and memory: Compound is the authority for durable meaning.",
		"- Runtime boundary: Task-specific intent stays in `compound orient`.",
		"<!-- END COMPOUND GENERATED INSTRUCTIONS -->",
	}, "\n")
	if err := validateOSAbsorptionGeneratedInstructionEntrypoint("codex", body); err != nil {
		t.Fatalf("Compound-generated instruction block should be the served authority: %v", err)
	}
	if err := validateOSAbsorptionGeneratedInstructionEntrypoint("codex", strings.Replace(body, "compound orient", "legacy recall", 1)); err == nil {
		t.Fatal("generated instruction proof accepted a block without Compound orientation")
	}
}

func TestOSAbsorptionServedOrientationAuthorityProof(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testing.T, servedOrientationFixture)
		wantStatus string
		wantDetail string
	}{
		{
			name:       "both generated clients and cited ContextPack are live",
			wantStatus: "ok",
		},
		{
			name: "documented Claude harness alias remains bound to Claude family",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				mutateServedOrientationContract(t, fixture.contractPaths["claude"], func(contract *envelope.EffectiveContract) {
					contract.Harness = "claude-code"
				})
			},
			wantStatus: "ok",
		},
		{
			name: "historical ContextPack is stale evidence",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				mutateServedOrientationPack(t, fixture.contextPackPaths["codex"], func(pack *envelope.ContextPack) {
					pack.CreatedAt = time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
				})
			},
			wantStatus: "failed",
			wantDetail: "stale",
		},
		{
			name: "ContextPack must bind to its harness contract",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				mutateServedOrientationContract(t, fixture.contractPaths["codex"], func(contract *envelope.EffectiveContract) {
					contract.Harness = "claude"
				})
			},
			wantStatus: "failed",
			wantDetail: "harness",
		},
		{
			name: "both harness orientations must share one live scope",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				otherScope := t.TempDir()
				mutateServedOrientationPack(t, fixture.contextPackPaths["claude"], func(pack *envelope.ContextPack) {
					pack.Scope.Surface = otherScope
				})
				mutateServedOrientationContract(t, fixture.contractPaths["claude"], func(contract *envelope.EffectiveContract) {
					contract.Scope.Surface = otherScope
				})
			},
			wantStatus: "failed",
			wantDetail: "scope",
		},
		{
			name: "ContextPack source revisions must still be current",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				if err := os.WriteFile(fixture.currentSourcePath, []byte("# architecture changed after orient\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "failed",
			wantDetail: "current source revision",
		},
		{
			name: "ContextPack evidence must bind the installed entrypoint digest",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				body, err := os.ReadFile(fixture.codexPath)
				if err != nil {
					t.Fatal(err)
				}
				body = append(body, []byte("\n<!-- harmless post-orient regeneration -->\n")...)
				if err := os.WriteFile(fixture.codexPath, body, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "failed",
			wantDetail: "digest",
		},
		{
			name: "installed entrypoint symlink repoint after materialization fails",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				external := filepath.Join(t.TempDir(), "AGENTS.md")
				copyServedOrientationFixtureFile(t, fixture.codexPath, external)
				if err := os.Remove(fixture.codexPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, fixture.codexPath); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "failed",
			wantDetail: "symlink",
		},
		{
			name: "matrix-only digest evidence is insufficient",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				mutateServedOrientationPack(t, fixture.contextPackPaths["codex"], func(pack *envelope.ContextPack) {
					pack.SourceRevisions = removeServedEntrypointRevision(pack.SourceRevisions, "codex")
				})
				mutateServedOrientationContract(t, fixture.contractPaths["codex"], func(contract *envelope.EffectiveContract) {
					contract.SourceRevisions = removeServedEntrypointRevision(contract.SourceRevisions, "codex")
				})
			},
			wantStatus: "failed",
			wantDetail: "artifact binding",
		},
		{
			name: "ContextPack missing installed entrypoint binding fails",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				mutateServedOrientationPack(t, fixture.contextPackPaths["codex"], func(pack *envelope.ContextPack) {
					pack.SourceRevisions = removeServedEntrypointRevision(pack.SourceRevisions, "codex")
				})
			},
			wantStatus: "failed",
			wantDetail: "ContextPack",
		},
		{
			name: "EffectiveContract missing installed entrypoint binding fails",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				mutateServedOrientationContract(t, fixture.contractPaths["codex"], func(contract *envelope.EffectiveContract) {
					contract.SourceRevisions = removeServedEntrypointRevision(contract.SourceRevisions, "codex")
				})
			},
			wantStatus: "failed",
			wantDetail: "EffectiveContract",
		},
		{
			name: "artifact entrypoint digest mismatch fails",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				mutateServedOrientationPack(t, fixture.contextPackPaths["codex"], func(pack *envelope.ContextPack) {
					setServedEntrypointRevision(pack.SourceRevisions, "codex", strings.Repeat("0", 64))
				})
			},
			wantStatus: "failed",
			wantDetail: "artifact binding",
		},
		{
			name: "ContextPack duplicate installed entrypoint revision fails",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				mutateServedOrientationPack(t, fixture.contextPackPaths["codex"], func(pack *envelope.ContextPack) {
					pack.SourceRevisions = append(pack.SourceRevisions, envelope.SourceRev{Source: servedEntrypointRevisionSource("codex"), Rev: "sha256:" + strings.Repeat("f", 64)})
				})
			},
			wantStatus: "failed", wantDetail: "duplicate source revision",
		},
		{
			name: "EffectiveContract duplicate installed entrypoint revision fails",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				mutateServedOrientationContract(t, fixture.contractPaths["codex"], func(contract *envelope.EffectiveContract) {
					contract.SourceRevisions = append(contract.SourceRevisions, envelope.SourceRev{Source: servedEntrypointRevisionSource("codex"), Rev: "sha256:" + strings.Repeat("f", 64)})
				})
			},
			wantStatus: "failed", wantDetail: "duplicate source revision",
		},
		{
			name: "ContextPack duplicate control plane revision fails",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				mutateServedOrientationPack(t, fixture.contextPackPaths["codex"], func(pack *envelope.ContextPack) {
					pack.SourceRevisions = append(pack.SourceRevisions, envelope.SourceRev{Source: "control-plane://orientation-map", Rev: "sha256:conflict"})
				})
			},
			wantStatus: "failed", wantDetail: "duplicate source revision",
		},
		{
			name: "EffectiveContract duplicate control plane revision fails",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				mutateServedOrientationContract(t, fixture.contractPaths["codex"], func(contract *envelope.EffectiveContract) {
					contract.SourceRevisions = append(contract.SourceRevisions, envelope.SourceRev{Source: "control-plane://orientation-map", Rev: "sha256:conflict"})
				})
			},
			wantStatus: "failed", wantDetail: "duplicate source revision",
		},
		{
			name: "missing served client fails closed",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				if err := os.Remove(fixture.codexPath); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "failed",
			wantDetail: "codex entrypoint",
		},
		{
			name: "missing cited ContextPack fails closed",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				if err := os.Remove(fixture.contextPackPaths["codex"]); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "failed",
			wantDetail: "ContextPack",
		},
		{
			name: "each harness requires its own ContextPack citation",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				for _, id := range []string{"os_source_served_orientation_primary", "os_source_served_orientation_secondary"} {
					mutateOSAbsorptionMatrixRow(t, fixture.compoundHome, id, func(row *OSAbsorptionMatrixRow) {
						kept := row.EvidenceRefs[:0]
						for _, ref := range row.EvidenceRefs {
							if !strings.HasPrefix(ref, "context_pack:claude:") {
								kept = append(kept, ref)
							}
						}
						row.EvidenceRefs = kept
					})
				}
			},
			wantStatus: "failed",
			wantDetail: "missing claude ContextPack",
		},
		{
			name: "removed proof mode fails closed instead of becoming manual",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				for _, id := range []string{"os_source_served_orientation_primary", "os_source_served_orientation_secondary"} {
					mutateOSAbsorptionMatrixRow(t, fixture.compoundHome, id, func(row *OSAbsorptionMatrixRow) {
						row.ProofMode = ""
					})
				}
			},
			wantStatus: "failed",
			wantDetail: osAbsorptionProofModeServedOrient,
		},
		{
			name: "wrong non-empty proof mode cannot dispatch another proof",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				if _, err := decisions.RecordSourceAuthority(fixture.compoundHome, decisions.DecisionImportReport{
					SourcePath:  "/synthetic/example-os/decisions/DEC-0001.md",
					SourceKind:  "decision_source",
					EvidenceID:  "ev_served_wrong_mode",
					CandidateID: "cand_served_wrong_mode",
					ClaimID:     "claim_served_wrong_mode",
					Verdict:     candidates.Promote,
					Lifecycle:   decisions.Lifecycle{ID: "dec_served_wrong_mode"},
				}, "test import", time.Date(2026, 7, 12, 17, 1, 0, 0, time.UTC)); err != nil {
					t.Fatalf("seed alternate proof: %v", err)
				}
				for _, id := range []string{"os_source_served_orientation_primary", "os_source_served_orientation_secondary"} {
					mutateOSAbsorptionMatrixRow(t, fixture.compoundHome, id, func(row *OSAbsorptionMatrixRow) {
						row.ProofMode = osAbsorptionProofModeDecisions
					})
				}
			},
			wantStatus: "failed",
			wantDetail: osAbsorptionProofModeServedOrient,
		},
		{
			name: "arbitrary copied harness entrypoints fail binding",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				copiedCodex := filepath.Join(t.TempDir(), "AGENTS.md")
				copiedClaude := filepath.Join(t.TempDir(), "CLAUDE.md")
				copyServedOrientationFixtureFile(t, fixture.codexPath, copiedCodex)
				copyServedOrientationFixtureFile(t, fixture.claudePath, copiedClaude)
				for _, id := range []string{"os_source_served_orientation_primary", "os_source_served_orientation_secondary"} {
					mutateOSAbsorptionMatrixRow(t, fixture.compoundHome, id, func(row *OSAbsorptionMatrixRow) {
						row.EvidenceRefs = replaceServedEntrypointEvidence(row.EvidenceRefs, "codex", copiedCodex)
						row.EvidenceRefs = replaceServedEntrypointEvidence(row.EvidenceRefs, "claude", copiedClaude)
					})
				}
			},
			wantStatus: "failed",
			wantDetail: "live installed",
		},
		{
			name: "arbitrary copied source entrypoint fails binding",
			mutate: func(t *testing.T, fixture servedOrientationFixture) {
				t.Helper()
				copiedSource := filepath.Join(t.TempDir(), "LIVE_START.md")
				copyServedOrientationFixtureFile(t, fixture.liveStartPath, copiedSource)
				for _, id := range []string{"os_source_served_orientation_primary", "os_source_served_orientation_secondary"} {
					mutateOSAbsorptionMatrixRow(t, fixture.compoundHome, id, func(row *OSAbsorptionMatrixRow) {
						oldSourceRef := row.SourceRef
						row.SourceRef = copiedSource
						for _, entrypointPath := range []string{fixture.codexPath, fixture.claudePath} {
							body, err := os.ReadFile(entrypointPath)
							if err != nil {
								t.Fatal(err)
							}
							body = []byte(strings.ReplaceAll(string(body), oldSourceRef, copiedSource))
							if err := os.WriteFile(entrypointPath, body, 0o644); err != nil {
								t.Fatal(err)
							}
						}
					})
				}
			},
			wantStatus: "failed",
			wantDetail: "declared absorbed-OS source entrypoint",
		},
	}

	// Build the fixture tree once and snapshot it, rather than paying the
	// full install (file writes + registry seeding) 23 times. Every subtest
	// gets an isolated tree by wiping root and restoring the snapshot in
	// place before it mutates anything — see captureServedOrientationTreeSnapshot.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	fixture := installServedOrientationFixtureAt(t, root)
	golden := captureServedOrientationTreeSnapshot(t, root)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restoreServedOrientationTreeSnapshot(t, root, golden)
			if tt.mutate != nil {
				tt.mutate(t, fixture)
			}

			doctor, err := OSAbsorptionDoctor(fixture.compoundHome)
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			for _, id := range []string{"os_source_served_orientation_primary", "os_source_served_orientation_secondary"} {
				got, ok := findOSAbsorptionDoctorRow(doctor.Rows, id)
				if !ok {
					t.Fatalf("doctor row %s missing: %+v", id, doctor.Rows)
				}
				if got.ProofStatus != tt.wantStatus {
					t.Fatalf("%s proof_status=%q, want %q: %+v", id, got.ProofStatus, tt.wantStatus, got)
				}
				if tt.wantStatus == "ok" {
					for _, want := range []string{
						testServedOrientationProofMode,
						"codex_context_pack=" + fixture.contextPackIDs["codex"],
						"claude_context_pack=" + fixture.contextPackIDs["claude"],
						"codex_sha256=",
						"claude_sha256=",
					} {
						if !strings.Contains(got.ProofRef, want) {
							t.Fatalf("%s proof_ref %q missing %q", id, got.ProofRef, want)
						}
					}
					continue
				}
				if !strings.Contains(got.ProofDetail, tt.wantDetail) {
					t.Fatalf("%s proof_detail %q missing %q", id, got.ProofDetail, tt.wantDetail)
				}
			}
		})
	}
}

func TestOSAbsorptionServedOrientationAuthorityProofRejectsHOMESpoof(t *testing.T) {
	fixture := installServedOrientationFixture(t)
	spoofedHome := t.TempDir()
	spoofedCodex := filepath.Join(spoofedHome, ".codex", "AGENTS.md")
	spoofedClaude := filepath.Join(spoofedHome, ".claude", "CLAUDE.md")
	copyServedOrientationFixtureFile(t, fixture.codexPath, spoofedCodex)
	copyServedOrientationFixtureFile(t, fixture.claudePath, spoofedClaude)
	for _, id := range []string{"os_source_served_orientation_primary", "os_source_served_orientation_secondary"} {
		mutateOSAbsorptionMatrixRow(t, fixture.compoundHome, id, func(row *OSAbsorptionMatrixRow) {
			row.EvidenceRefs = replaceServedEntrypointEvidence(row.EvidenceRefs, "codex", spoofedCodex)
			row.EvidenceRefs = replaceServedEntrypointEvidence(row.EvidenceRefs, "claude", spoofedClaude)
		})
	}
	t.Setenv("HOME", spoofedHome)

	doctor, err := OSAbsorptionDoctor(fixture.compoundHome)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	for _, id := range []string{"os_source_served_orientation_primary", "os_source_served_orientation_secondary"} {
		got, ok := findOSAbsorptionDoctorRow(doctor.Rows, id)
		if !ok {
			t.Fatalf("doctor row %s missing", id)
		}
		if got.ProofStatus != "failed" || !strings.Contains(got.ProofDetail, "live installed") {
			t.Fatalf("HOME-spoofed entrypoint must fail the trusted installed-home binding: %+v", got)
		}
	}
}

type servedOrientationFixture struct {
	compoundHome      string
	codexPath         string
	claudePath        string
	liveStartPath     string
	contextPackIDs    map[string]string
	contextPackPaths  map[string]string
	contractPaths     map[string]string
	scopePath         string
	currentSourcePath string
}

func installServedOrientationFixture(t *testing.T) servedOrientationFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	return installServedOrientationFixtureAt(t, root)
}

// installServedOrientationFixtureAt builds the fixture tree under an
// already-established root (HOME already pointed at it by the caller). It is
// the reusable core behind installServedOrientationFixture, split out so a
// caller that wants to build the scaffolding once and reuse the same root
// across many subtests (see servedOrientationTreeSnapshot below) can call it
// exactly once instead of paying its file-write and registry-seeding cost per
// subtest.
func installServedOrientationFixtureAt(t *testing.T, root string) servedOrientationFixture {
	t.Helper()
	compoundHome := filepath.Join(root, "compound-home")
	codexPath := filepath.Join(root, ".codex", "AGENTS.md")
	claudePath := filepath.Join(root, ".claude", "CLAUDE.md")
	scopePath := filepath.Join(root, "served-scope")
	currentSourcePath := filepath.Join(scopePath, "docs", "ARCHITECTURE.md")
	for rel, body := range map[string]string{
		"docs/ARCHITECTURE.md":                  "# architecture\n",
		"docs/LAYERS.md":                        "# layers\n",
		"docs/architecture/contracts/README.md": "# contracts\n",
		"docs/architecture/runtime.md":          "# runtime\n",
		"docs/roadmap/phases.md":                "# phases\n",
	} {
		path := filepath.Join(scopePath, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	contextPackIDs := map[string]string{
		"codex":  "ctx_served_orientation_codex",
		"claude": "ctx_served_orientation_claude",
	}
	contextPackPaths := make(map[string]string, len(contextPackIDs))
	contractPaths := make(map[string]string, len(contextPackIDs))
	createdAt := time.Now().UTC().Truncate(time.Second)
	absorbedOSRoot := filepath.Join(root, "example-os")
	liveStart := filepath.Join(absorbedOSRoot, "LIVE_START.md")
	wholeMap := filepath.Join(absorbedOSRoot, "universe", "WHOLE_SYSTEM_MAP.md")
	if err := os.MkdirAll(filepath.Dir(wholeMap), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{liveStart, wholeMap} {
		if err := os.WriteFile(path, []byte("# synthetic absorbed-OS source entrypoint\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seedAbsorbedOSRegistry(t, compoundHome, absorbedOSRoot)
	seedInstalledHarnessHomeRegistry(t, compoundHome, root)

	for _, entrypoint := range []struct {
		harness string
		path    string
	}{
		{harness: "codex", path: codexPath},
		{harness: "claude", path: claudePath},
	} {
		if err := os.MkdirAll(filepath.Dir(entrypoint.path), 0o755); err != nil {
			t.Fatal(err)
		}
		body := strings.Join(append([]string{
			"<!-- GENERATED from an absorbed-OS steering config; generator stamp test -->",
		}, append(validServedOrientationLines(entrypoint.harness, liveStart, wholeMap),
			"If `compound orient` is unavailable or fails, use the rollback path.",
			"",
		)...), "\n")
		if err := os.WriteFile(entrypoint.path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(entrypoint.path, createdAt.Add(-time.Minute), createdAt.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	entrypointDigests := map[string]string{}
	for harness, path := range map[string]string{"codex": codexPath, "claude": claudePath} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		entrypointDigests[harness] = digestBytes(body)
	}
	for _, harness := range []string{"codex", "claude"} {
		contextPackPaths[harness], contractPaths[harness] = writeServedOrientationContextPack(
			t,
			compoundHome,
			scopePath,
			harness,
			contextPackIDs[harness],
			"contract_served_orientation_"+harness,
			entrypointDigests[harness],
			createdAt,
		)
	}
	matrixPath := filepath.Join(root, "matrix.json")
	rows := make([]OSAbsorptionMatrixRow, 0, 2)
	for _, source := range []struct {
		id  string
		ref string
	}{
		{id: "os_source_served_orientation_primary", ref: liveStart},
		{id: "os_source_served_orientation_secondary", ref: wholeMap},
	} {
		r := row(source.id, source.ref, "absorbed", "compound://contracts/effective-contract", "compound orient and served client contract", testServedOrientationProofMode)
		r.CurrentRole = "source_in_place_evidence"
		r.EvidenceRefs = []string{
			"served_entrypoint:codex:" + codexPath,
			"served_entrypoint:claude:" + claudePath,
			"context_pack:codex:" + contextPackIDs["codex"] + ":" + entrypointDigests["codex"],
			"context_pack:claude:" + contextPackIDs["claude"] + ":" + entrypointDigests["claude"],
		}
		rows = append(rows, r)
	}
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          rows,
	})
	if _, err := ImportOSAbsorptionMatrix(compoundHome, matrixPath, time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import matrix: %v", err)
	}
	return servedOrientationFixture{
		compoundHome:      compoundHome,
		codexPath:         codexPath,
		claudePath:        claudePath,
		liveStartPath:     liveStart,
		contextPackIDs:    contextPackIDs,
		contextPackPaths:  contextPackPaths,
		contractPaths:     contractPaths,
		scopePath:         scopePath,
		currentSourcePath: currentSourcePath,
	}
}

// servedOrientationTreeEntry is one captured filesystem node (file, dir, or
// symlink) under a servedOrientationFixture root, byte- and mtime-exact.
type servedOrientationTreeEntry struct {
	relPath    string
	isDir      bool
	isSymlink  bool
	linkTarget string
	mode       os.FileMode
	modTime    time.Time
	content    []byte
}

// captureServedOrientationTreeSnapshot walks root and records every node so
// it can be restored byte-for-byte later. Every OSAbsorptionMatrixRow,
// ContextPack, and EffectiveContract this fixture writes embeds absolute
// paths derived from root (codexPath, compoundHome, ...), so a rebuild-heavy
// per-subtest fixture cannot simply be relocated to a fresh t.TempDir(): the
// embedded references would point at a directory that no longer exists. This
// snapshot instead lets many subtests share ONE root path and reset its
// on-disk content in place, which keeps every embedded absolute path valid.
func captureServedOrientationTreeSnapshot(t *testing.T, root string) []servedOrientationTreeEntry {
	t.Helper()
	var entries []servedOrientationTreeEntry
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entries = append(entries, servedOrientationTreeEntry{relPath: rel, isSymlink: true, linkTarget: target})
			return nil
		}
		if info.IsDir() {
			entries = append(entries, servedOrientationTreeEntry{relPath: rel, isDir: true, mode: info.Mode()})
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, servedOrientationTreeEntry{
			relPath: rel, mode: info.Mode(), modTime: info.ModTime(), content: body,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("capture served-orientation tree snapshot: %v", err)
	}
	return entries
}

// restoreServedOrientationTreeSnapshot wipes root and rewrites it from a
// captured snapshot, including file mtimes. This gives each subtest an
// isolated, freshly-built-equivalent tree without re-running the fixture's
// file writes and registry seeding 23 times: the wipe is unconditional (a
// prior subtest may have deleted or symlinked a fixture path), so a subtest
// can never observe a mutation left behind by an earlier one.
func restoreServedOrientationTreeSnapshot(t *testing.T, root string, entries []servedOrientationTreeEntry) {
	t.Helper()
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("reset served-orientation tree: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("recreate served-orientation root: %v", err)
	}
	for _, entry := range entries {
		full := filepath.Join(root, entry.relPath)
		switch {
		case entry.isDir:
			if err := os.MkdirAll(full, entry.mode); err != nil {
				t.Fatalf("restore dir %s: %v", entry.relPath, err)
			}
		case entry.isSymlink:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("restore symlink parent %s: %v", entry.relPath, err)
			}
			if err := os.Symlink(entry.linkTarget, full); err != nil {
				t.Fatalf("restore symlink %s: %v", entry.relPath, err)
			}
		default:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("restore file parent %s: %v", entry.relPath, err)
			}
			if err := os.WriteFile(full, entry.content, entry.mode); err != nil {
				t.Fatalf("restore file %s: %v", entry.relPath, err)
			}
			if err := os.Chtimes(full, entry.modTime, entry.modTime); err != nil {
				t.Fatalf("restore mtime %s: %v", entry.relPath, err)
			}
		}
	}
}

func validServedOrientationLines(harness string, sourceRefs ...string) []string {
	lines := []string{
		testServedOrientationBlockBegin,
		"## Orient first — Compound (the served contract)",
		"Run `compound orient --harness " + harness + " --task \"<task>\"`.",
		osAbsorptionRequiredMeaningAuthority,
		osAbsorptionRequiredBrainRole,
		osAbsorptionRequiredGBrainRole,
		"These files remain source-in-place evidence and rollback:",
	}
	lines = append(lines, sourceRefs...)
	return append(lines, testServedOrientationBlockEnd)
}

func seedInstalledHarnessHomeRegistry(t *testing.T, compoundHome, installedHome string) {
	t.Helper()
	registryPath := filepath.Join(t.TempDir(), "installed-harness-home.yaml")
	body := []byte("version: 1\ncanonical_roots:\n  " + testInstalledHarnessHomeKey + ":\n    path: " + installedHome + "\n    status: active\n")
	if err := os.WriteFile(registryPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := store.Entities()
	if err != nil {
		t.Fatal(err)
	}
	_, entities, facts, report, err := ImportOSPathRegistry(registryPath, existing)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" {
		t.Fatalf("installed harness home registry status=%s", report.Status)
	}
	now := time.Date(2026, 7, 12, 16, 59, 0, 0, time.UTC)
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
		if _, err := store.Adjudicate(candidate.ID, "promote", "test-seed-installed-harness-home", now); err != nil {
			t.Fatal(err)
		}
	}
}

func copyServedOrientationFixtureFile(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func replaceServedEntrypointEvidence(refs []string, harness, path string) []string {
	got := append([]string(nil), refs...)
	prefix := osAbsorptionServedEntrypointEvidenceRef + harness + ":"
	for i, ref := range got {
		if strings.HasPrefix(ref, prefix) {
			got[i] = prefix + path
		}
	}
	return got
}

func writeServedOrientationContextPack(
	t *testing.T,
	compoundHome string,
	scopePath string,
	harness string,
	id string,
	contractID string,
	entrypointDigest string,
	createdAt time.Time,
) (string, string) {
	t.Helper()
	refs := []struct {
		ref         string
		sourceClass string
	}{
		{ref: "control-plane://orientation-map", sourceClass: "source"},
		{ref: "control-plane://workflow-contract", sourceClass: "source"},
		{ref: "control-plane://sop-packets", sourceClass: "source"},
		{ref: "memory://relevance-gated", sourceClass: "generated"},
	}
	included := make([]envelope.IncludedRef, 0, len(refs))
	revisions := make([]envelope.SourceRev, 0, len(refs))
	for _, ref := range refs {
		included = append(included, envelope.IncludedRef{
			Ref:         ref.ref,
			SourceClass: ref.sourceClass,
			Reason:      "served orientation fixture",
			Confidence:  1,
			Status:      "current",
		})
		revision := "sha256:" + digestBytes([]byte("current memory revision"))
		if ref.sourceClass == "source" {
			revision = testServedOrientationSourceRevision(t, scopePath, ref.ref)
		}
		revisions = append(revisions, envelope.SourceRev{Source: ref.ref, Rev: revision})
	}
	revisions = append(revisions, envelope.SourceRev{
		Source: servedEntrypointRevisionSource(harness),
		Rev:    "sha256:" + entrypointDigest,
	})
	taskContract := envelope.TaskContract{Intent: "prove served orientation"}
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            id,
			CreatedAt:     createdAt.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "served-orientation"},
			IDs:           envelope.IDs{ContractID: contractID},
			Scope:         envelope.Scope{Surface: scopePath},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD:    "served orientation fixture",
		TaskContract: taskContract,
		Included:     included,
		Excluded:     []envelope.ExcludedRef{{Ref: "repo://unrelated", Reason: "out of scope"}},
		ActiveInstructions: []string{
			"control-plane://orientation-map",
			"control-plane://workflow-contract",
			"control-plane://sop-packets",
		},
		ActiveHooks:     []string{},
		ActiveDecisions: []string{},
		SourceRevisions: revisions,
		RequiredChecks:  []string{},
		Avoid:           []string{},
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("fixture ContextPack invalid: %v", err)
	}
	packPath := filepath.Join(compoundHome, "context", "packs", id+".json")
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFixture(t, packPath, pack)

	contract := envelope.EffectiveContract{
		Header: envelope.Header{
			SchemaVersion: envelope.SVEffectiveContract,
			ID:            contractID,
			CreatedAt:     pack.CreatedAt,
			Producer:      envelope.Producer{Kind: "test", Name: "served-orientation"},
			Scope:         pack.Scope,
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Harness:            harness,
		TaskContract:       taskContract,
		ActiveInstructions: append([]string(nil), pack.ActiveInstructions...),
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		Excluded:           []envelope.ExcludedRef{},
		SourceRevisions:    append([]envelope.SourceRev(nil), revisions...),
		Unknowns:           []string{},
		Conflicts:          []string{},
	}
	if err := contract.Validate(); err != nil {
		t.Fatalf("fixture EffectiveContract invalid: %v", err)
	}
	contractPath := filepath.Join(
		compoundHome,
		"contracts",
		"effective",
		createdAt.UTC().Format("2006-01-02"),
		contractID+".json",
	)
	if err := os.MkdirAll(filepath.Dir(contractPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFixture(t, contractPath, contract)
	return packPath, contractPath
}

func servedEntrypointRevisionSource(harness string) string {
	return "installed-entrypoint://" + harness
}

func removeServedEntrypointRevision(revisions []envelope.SourceRev, harness string) []envelope.SourceRev {
	got := make([]envelope.SourceRev, 0, len(revisions))
	for _, revision := range revisions {
		if revision.Source != servedEntrypointRevisionSource(harness) {
			got = append(got, revision)
		}
	}
	return got
}

func setServedEntrypointRevision(revisions []envelope.SourceRev, harness, digest string) {
	for i := range revisions {
		if revisions[i].Source == servedEntrypointRevisionSource(harness) {
			revisions[i].Rev = "sha256:" + digest
			return
		}
	}
}

func testServedOrientationSourceRevision(t *testing.T, scopePath, ref string) string {
	t.Helper()
	var rels []string
	switch ref {
	case "control-plane://orientation-map":
		rels = []string{"docs/ARCHITECTURE.md", "docs/LAYERS.md"}
	case "control-plane://workflow-contract":
		rels = []string{"docs/architecture/contracts/README.md"}
	case "control-plane://sop-packets":
		rels = []string{"docs/architecture/runtime.md", "docs/roadmap/phases.md"}
	default:
		t.Fatalf("unknown served source ref %q", ref)
	}
	rows := make([]string, 0, len(rels))
	for _, rel := range rels {
		body, err := os.ReadFile(filepath.Join(scopePath, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, rel+":sha256:"+digest12(string(body)))
	}
	sort.Strings(rows)
	return "sha256:" + digest12(strings.Join(rows, "\n"))
}

func mutateServedOrientationPack(t *testing.T, path string, mutate func(*envelope.ContextPack)) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pack envelope.ContextPack
	if err := json.Unmarshal(body, &pack); err != nil {
		t.Fatal(err)
	}
	mutate(&pack)
	writeJSONFixture(t, path, pack)
}

func mutateServedOrientationContract(t *testing.T, path string, mutate func(*envelope.EffectiveContract)) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var contract envelope.EffectiveContract
	if err := json.Unmarshal(body, &contract); err != nil {
		t.Fatal(err)
	}
	mutate(&contract)
	writeJSONFixture(t, path, contract)
}

func findOSAbsorptionDoctorRow(rows []OSAbsorptionDoctorRow, id string) (OSAbsorptionDoctorRow, bool) {
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return OSAbsorptionDoctorRow{}, false
}

func findOSAbsorptionDoctorCheck(checks []OSAbsorptionDoctorCheck, name string) (OSAbsorptionDoctorCheck, bool) {
	for _, check := range checks {
		if check.Name == name {
			return check, true
		}
	}
	return OSAbsorptionDoctorCheck{}, false
}
