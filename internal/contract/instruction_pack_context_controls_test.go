package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/harness"
)

func TestInstructionPackExplicitRebindingReplacesOnlyProfileSelection(t *testing.T) {
	homePath, in, a, _ := instructionContextFixture(t)
	in.Task = "review another synthetic task"
	now := time.Now().UTC().Add(time.Minute)
	runtime, err := MaterializeRuntime(in, DefaultPolicy(), homePath, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SealRuntimeMaterialization(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordRuntimeMaterialization(homePath, b); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderInstructionPackForContext(homePath, "codex", "", b.ContextPack.ID, now); err != nil {
		t.Fatal(err)
	}
	change := func(prior RuntimeMaterialization) {
		t.Helper()
		prior.EffectiveContract.ActiveDecisions = []string{"decision://synthetic-rebound-source"}
		sealed, err := SealRuntimeMaterialization(prior)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RecordRuntimeMaterialization(homePath, sealed); err != nil {
			t.Fatal(err)
		}
	}
	change(a)
	report, err := harness.InstructionPackDrift(homePath, "codex", "", now)
	if err != nil || report.Drifted {
		t.Fatalf("old context still controls rebound profile: %+v, %v", report, err)
	}
	if pair, err := ReadContextPackContract(homePath, a.ContextPack.ID); err != nil || pair.EffectiveContract.ID != a.EffectiveContract.ID {
		t.Fatalf("profile rebinding rewrote immutable work evidence: %+v, %v", pair, err)
	}
	change(b)
	report, err = harness.InstructionPackDrift(homePath, "codex", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Drifted {
		t.Fatalf("rebound context lost its own authority drift: %+v", report)
	}
}

func TestInstructionPackRefusesAlteredImmutableSelector(t *testing.T) {
	for _, variant := range []string{"context-pack-link", "contract-context"} {
		t.Run(variant, func(t *testing.T) {
			homePath, in, prior, paths := instructionContextFixture(t)
			in.Task = "review another synthetic task"
			runtime, err := MaterializeRuntime(in, DefaultPolicy(), homePath, time.Now().UTC().Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			alternate, err := SealRuntimeMaterialization(runtime)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RecordRuntimeMaterialization(homePath, alternate); err != nil {
				t.Fatal(err)
			}
			packPath := harness.InstructionPackPath(homePath, "codex", "")
			before, err := os.ReadFile(packPath)
			if err != nil {
				t.Fatal(err)
			}
			// Bypass the owned writer to model storage corruption, retaining the sealed
			// path/ID while redirecting its schema-valid linkage to another valid scope.
			var changed []byte
			switch variant {
			case "context-pack-link":
				original := prior.ContextPack
				original.IDs.ContractID = alternate.EffectiveContract.ID
				changed, err = json.Marshal(original)
				if err == nil {
					err = os.WriteFile(paths.PackPath, changed, 0600)
				}
			case "contract-context":
				original := prior.EffectiveContract
				original.ContextKey = alternate.EffectiveContract.ContextKey
				changed, err = json.Marshal(original)
				if err == nil {
					err = os.WriteFile(paths.ContractPath, changed, 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadContextPackContract(homePath, prior.ContextPack.ID); err == nil {
				t.Error("altered immutable selector was accepted as execution proof")
			}
			if _, err := RenderInstructionPackForContext(homePath, "codex", "", prior.ContextPack.ID, time.Now()); err == nil {
				t.Error("altered immutable selector redirected profile binding")
			}
			after, err := os.ReadFile(packPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Error("refused selector changed installed pack")
			}
		})
	}
}

func TestInstructionPackRefusesDamagedScopedProjection(t *testing.T) {
	for _, variant := range []string{"unsealed-change", "sealed-without-immutable", "missing-immutable", "immutable-bytes-mismatch"} {
		t.Run(variant, func(t *testing.T) {
			homePath, _, prior, paths := instructionContextFixture(t)
			packPath := harness.InstructionPackPath(homePath, "codex", "")
			before, err := os.ReadFile(packPath)
			if err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "unsealed-change", "sealed-without-immutable":
				changed := prior
				changed.EffectiveContract.ActiveDecisions = []string{"decision://damaged-projection"}
				if variant == "sealed-without-immutable" {
					changed, err = SealRuntimeMaterialization(changed)
					if err != nil {
						t.Fatal(err)
					}
				}
				body, err := json.Marshal(changed.EffectiveContract)
				if err != nil {
					t.Fatal(err)
				}
				// Deliberately bypass the writer: the corruption cannot enter through the sealed transaction.
				if err := os.WriteFile(paths.ScopedLatestPath, body, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-immutable":
				if err := os.Remove(paths.ContractPath); err != nil {
					t.Fatal(err)
				}
			case "immutable-bytes-mismatch":
				body, err := os.ReadFile(paths.ContractPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.ContractPath, append(body, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := harness.RenderInstructionPack(homePath, "codex", "", time.Now()); err == nil {
				t.Error("damaged scoped projection was rendered into a clean instruction pack")
			}
			if _, err := harness.InstructionPackDrift(homePath, "codex", "", time.Now()); err == nil {
				t.Error("damaged scoped projection was accepted by drift verification")
			}
			after, err := os.ReadFile(packPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Error("refused render rewrote the installed pack")
			}
		})
	}
}

func instructionContextFixture(t *testing.T) (string, Input, RuntimeMaterialization, RuntimeMaterializationArtifactPaths) {
	t.Helper()
	homePath := t.TempDir()
	in := Input{Harness: "codex", Cwd: t.TempDir(), Task: "review synthetic project", RepoKind: "product", RepoKindVia: "flag", Mode: "review"}
	runtime, err := MaterializeRuntime(in, DefaultPolicy(), homePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealRuntimeMaterialization(runtime)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordRuntimeMaterialization(homePath, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RenderInstructionPackForContext(homePath, "codex", "", sealed.ContextPack.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	return homePath, in, sealed, paths
}

func TestInstructionPackDetectsOwnContractChange(t *testing.T) {
	homePath, _, prior, _ := instructionContextFixture(t)
	changed := prior
	changed.EffectiveContract.ActiveDecisions = []string{"decision://synthetic-authority-change"}
	sealed, err := SealRuntimeMaterialization(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordRuntimeMaterialization(homePath, sealed); err != nil {
		t.Fatal(err)
	}
	report, err := harness.InstructionPackDrift(homePath, "codex", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Drifted || !strings.Contains(strings.Join(report.Details, " "), "effective_contract layer drifted") {
		t.Fatalf("own authority change must drift: %+v", report)
	}
	// The old immutable pair remains a valid receipt even after its projection advances.
	pair, err := ReadContextPackContract(homePath, prior.ContextPack.ID)
	if err != nil || pair.EffectiveContract.ID != prior.EffectiveContract.ID {
		t.Fatalf("immutable join changed: %+v, %v", pair, err)
	}
}

func TestInstructionPackRefusesUnresolvedContract(t *testing.T) {
	for _, variant := range []string{"missing", "malformed", "identity-mismatch", "legacy-global"} {
		t.Run(variant, func(t *testing.T) {
			homePath, _, _, paths := instructionContextFixture(t)
			packPath := harness.InstructionPackPath(homePath, "codex", "")
			if variant == "legacy-global" {
				body, err := os.ReadFile(packPath)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := filepath.Rel(homePath, paths.ScopedLatestPath)
				if err != nil {
					t.Fatal(err)
				}
				body = []byte(strings.ReplaceAll(string(body), filepath.ToSlash(ref), "contracts/effective/latest.json"))
				if err := os.WriteFile(packPath, body, 0600); err != nil {
					t.Fatal(err)
				}
			} else if variant == "missing" {
				if err := os.Remove(paths.ScopedLatestPath); err != nil {
					t.Fatal(err)
				}
			} else {
				body := []byte("{")
				if variant == "identity-mismatch" {
					var err error
					body, err = os.ReadFile(paths.ScopedLatestPath)
					if err != nil {
						t.Fatal(err)
					}
					body = []byte(strings.Replace(string(body), `"harness": "codex"`, `"harness": "hermes"`, 1))
				}
				// Deliberately bypass the validated write door to model damaged custody.
				if err := os.WriteFile(paths.ScopedLatestPath, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(packPath)
			if err != nil {
				t.Fatal(err)
			}
			if report, err := harness.InstructionPackDrift(homePath, "codex", "", time.Now()); err == nil {
				t.Fatalf("unresolved authority must refuse, got %+v", report)
			}
			after, err := os.ReadFile(packPath)
			if err != nil || string(after) != string(before) {
				t.Fatal("read-only drift changed installed pack")
			}
		})
	}
}

func TestInstructionPackUnboundIsNotClean(t *testing.T) {
	homePath := t.TempDir()
	if _, err := harness.RenderInstructionPack(homePath, "codex", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if report, err := harness.InstructionPackDrift(homePath, "codex", "", time.Now()); err == nil {
		t.Fatalf("unbound pack cannot be certified clean: %+v", report)
	}
}

func TestInstructionPackContextSelectionRejectsWrongHarnessAndUnsafeRef(t *testing.T) {
	homePath, _, sealed, _ := instructionContextFixture(t)
	if _, err := RenderInstructionPackForContext(homePath, "hermes", "", sealed.ContextPack.ID, time.Now()); err == nil {
		t.Fatal("cross-harness context accepted")
	}
	for _, ref := range []string{"../outside.json", "/outside.json", "contracts/effective/latest.json", "contracts/effective/scopes/../latest.json"} {
		if _, err := harness.RenderInstructionPackWithContractRef(homePath, "codex", "", ref, time.Now()); err == nil {
			t.Fatalf("unsafe or unscoped ref accepted: %q", ref)
		}
	}
}
func TestScopedMaterializationRollbackPreservesInstalledConsumer(t *testing.T) {
	for _, failGlobal := range []bool{false, true} {
		t.Run(map[bool]string{false: "scope", true: "global"}[failGlobal], func(t *testing.T) {
			homePath, _, prior, paths := instructionContextFixture(t)
			preimages := make(map[string][]byte)
			for _, path := range []string{paths.ScopedLatestPath, paths.LatestPath} {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				preimages[path] = body
			}
			changed := prior
			changed.EffectiveContract.ActiveDecisions = []string{"decision://rollback-control"}
			sealed, err := SealRuntimeMaterialization(changed)
			if err != nil {
				t.Fatal(err)
			}
			failPath := paths.ScopedLatestPath
			if failGlobal {
				failPath = paths.LatestPath
			}
			_, err = recordRuntimeMaterializationWithFS(homePath, sealed, runtimeMaterializationFS{
				createTemp: os.CreateTemp,
				rename: func(from, to string) error {
					if to == failPath {
						return errors.New("synthetic publication failure")
					}
					return os.Rename(from, to)
				},
			})
			if err == nil {
				t.Fatal("publication failure was ignored")
			}
			for path, want := range preimages {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(want) {
					t.Fatalf("projection not restored: %s, %v", path, err)
				}
			}
			if report, err := harness.InstructionPackDrift(homePath, "codex", "", time.Now()); err != nil || report.Drifted {
				t.Fatalf("rollback invalidated installed consumer: %+v, %v", report, err)
			}
			matches, err := filepath.Glob(filepath.Join(homePath, "contracts", "effective", "*", sealed.EffectiveContract.ID+".json"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("failed publication left immutable candidate: %v, %v", matches, err)
			}
		})
	}
}
