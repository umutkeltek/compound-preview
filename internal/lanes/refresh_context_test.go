package lanes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/harness"
	"gopkg.in/yaml.v3"
)

func TestRefreshAllWithoutDefaultsGivesExplicitBindingRepair(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "legacy-global"}[legacy], func(t *testing.T) {
			homePath, baseDir := t.TempDir(), t.TempDir()
			binaryPath := filepath.Join(t.TempDir(), "compound-synthetic")
			if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
				t.Fatal(err)
			}
			writeInstalledHooksManifest(t, homePath, map[string]string{"codex": "binary:" + binaryPath})
			if legacy {
				path := harness.InstructionPackPath(homePath, "codex", "")
				pack, err := harness.RenderInstructionPack(homePath, "codex", "", time.Now())
				if err != nil {
					t.Fatal(err)
				}
				pack.Roster.EffectiveContract = harness.InstructionPackRosterRef{Ref: "contracts/effective/latest.json", Revision: "sha256:legacy"}
				body, err := json.Marshal(pack)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			verdicts, err := RefreshAllInstalledHarnesses(RefreshLaneOptions{HomePath: homePath, BaseDir: baseDir, BinaryPath: binaryPath, Now: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			if len(verdicts) != 1 || verdicts[0].Status == StatusClean {
				t.Fatalf("missing binding must refuse: %+v", verdicts)
			}
			if !strings.Contains(verdicts[0].RepairCommand, "--context-pack") {
				t.Fatalf("repair cannot establish an explicit context binding: %+v", verdicts[0])
			}
			in := contract.Input{Harness: "codex", Cwd: t.TempDir(), Task: "review synthetic recovery", RepoKind: "product", RepoKindVia: "flag"}
			runtime, err := contract.MaterializeRuntime(in, contract.DefaultPolicy(), homePath, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			sealed, err := contract.SealRuntimeMaterialization(runtime)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := contract.RecordRuntimeMaterialization(homePath, sealed); err != nil {
				t.Fatal(err)
			}
			// Same owned seam as the advertised CLI --context-pack repair.
			if _, err := contract.RenderInstructionPackForContext(homePath, "codex", "", sealed.ContextPack.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			verdicts, err = RefreshAllInstalledHarnesses(RefreshLaneOptions{HomePath: homePath, BaseDir: baseDir, BinaryPath: binaryPath, Now: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			if len(verdicts) != 1 || verdicts[0].Status != StatusClean {
				t.Fatalf("explicit recovery did not converge: %+v", verdicts)
			}
		})
	}
}

func TestRefreshAllBindsEachCanonicalContext(t *testing.T) {
	homePath, baseDir := t.TempDir(), t.TempDir()
	binaryPath := filepath.Join(t.TempDir(), "compound-synthetic")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	writeInstalledHooksManifest(t, homePath, map[string]string{
		"codex": "binary:" + binaryPath, "claude_minimax": "binary:" + binaryPath,
	})
	defaults := laneDefaultsFile{Lanes: []LaneDefault{
		{Harness: "codex", Cwd: t.TempDir(), Task: "review synthetic source"},
		{Harness: "claude_minimax", Cwd: t.TempDir(), Task: "inspect synthetic fixtures"},
	}}
	body, err := yaml.Marshal(defaults)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LaneDefaultsPath(homePath), body, 0600); err != nil {
		t.Fatal(err)
	}
	verdicts, err := RefreshAllInstalledHarnesses(RefreshLaneOptions{
		HomePath: homePath, BaseDir: baseDir, BinaryPath: binaryPath, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("want both lanes, got %+v", verdicts)
	}
	refs := make(map[string]bool)
	for _, verdict := range verdicts {
		if verdict.Status != StatusClean {
			t.Fatalf("lane not clean: %+v", verdict)
		}
		report, err := harness.InstructionPackDrift(homePath, verdict.Harness, "", time.Now())
		if err != nil || report.Drifted {
			t.Fatalf("independent post-loop drift: %+v, %v", report, err)
		}
		body, err := os.ReadFile(report.Path)
		if err != nil {
			t.Fatal(err)
		}
		var pack harness.InstructionPack
		if err := json.Unmarshal(body, &pack); err != nil {
			t.Fatal(err)
		}
		ref := pack.Roster.EffectiveContract.Ref
		if refs[ref] {
			t.Fatalf("different lane contexts share contract %q", ref)
		}
		refs[ref] = true
		body, err = os.ReadFile(filepath.Join(homePath, filepath.FromSlash(ref)))
		if err != nil {
			t.Fatal(err)
		}
		var effective envelope.EffectiveContract
		if err := json.Unmarshal(body, &effective); err != nil {
			t.Fatal(err)
		}
		if effective.Harness != verdict.Harness {
			t.Fatalf("borrowed another lane's contract: %+v", effective)
		}
	}
}
