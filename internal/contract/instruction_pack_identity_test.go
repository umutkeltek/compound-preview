package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/harness"
)

func TestInstructionPackAutomaticClassificationChangeDriftsOwnContext(t *testing.T) {
	for _, markerKind := range []string{"product", "control-plane"} {
		t.Run(markerKind, func(t *testing.T) {
			homePath, cwd := t.TempDir(), t.TempDir()
			now := time.Now().UTC()
			in := Input{Harness: "codex", Cwd: cwd, Task: "inspect synthetic repository"}
			resolve := func() {
				t.Helper()
				var err error
				in.RepoKind, in.RepoKindVia, err = ResolveRepoKindWithHome(homePath, "", cwd)
				if err != nil {
					t.Fatal(err)
				}
			}
			record := func(at time.Time) RuntimeMaterialization {
				t.Helper()
				runtime, err := MaterializeRuntime(in, DefaultPolicy(), homePath, at)
				if err != nil {
					t.Fatal(err)
				}
				sealed, err := SealRuntimeMaterialization(runtime)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := RecordRuntimeMaterialization(homePath, sealed); err != nil {
					t.Fatal(err)
				}
				return sealed
			}
			resolve()
			if in.RepoKindVia != "default" {
				t.Fatalf("expected isolated unclassified fixture, got %+v", in)
			}
			before := record(now)
			if _, err := RenderInstructionPackForContext(homePath, "codex", "", before.ContextPack.ID, now); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(cwd, ".compound"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, ".compound", "repo.yaml"), []byte("kind: "+markerKind+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			resolve()
			after := record(now.Add(time.Second))
			if after.EffectiveContract.ContextKey != before.EffectiveContract.ContextKey {
				t.Fatal("automatic classification forked the installed request identity")
			}
			drift, err := harness.InstructionPackDrift(homePath, "codex", "", now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if !drift.Drifted || !strings.Contains(strings.Join(drift.Details, " "), "effective_contract layer drifted") {
				t.Fatalf("own classification-source change became invisible: %+v", drift)
			}
		})
	}
}

func TestInstructionPackRequestSelectionsRemainDistinct(t *testing.T) {
	base := Input{Harness: "codex", Cwd: t.TempDir(), Task: "review source", RepoKind: "product", RepoKindVia: "flag"}
	for _, name := range []string{"explicit-repo-kind", "topic", "task-contract"} {
		t.Run(name, func(t *testing.T) {
			other := base
			switch name {
			case "explicit-repo-kind":
				other.RepoKind = "control-plane"
			case "topic":
				other.Topic = "synthetic-topic"
			case "task-contract":
				other.TaskContract.SuccessPredicate = "synthetic success predicate"
			}
			if Compile(base, DefaultPolicy(), time.Now()).ContextKey == Compile(other, DefaultPolicy(), time.Now()).ContextKey {
				t.Fatalf("explicit request selection collided: %s", name)
			}
		})
	}
}

func TestInstructionPackRepeatedMaterializationIsNotAuthorityDrift(t *testing.T) {
	homePath, in, prior, _ := instructionContextFixture(t)
	at := time.Now().UTC().Add(time.Minute)
	runtime, err := MaterializeRuntime(in, DefaultPolicy(), homePath, at)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealRuntimeMaterialization(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.EffectiveContract.ID == prior.EffectiveContract.ID {
		t.Fatal("control did not advance the immutable generation")
	}
	if _, err := RecordRuntimeMaterialization(homePath, sealed); err != nil {
		t.Fatal(err)
	}
	report, err := harness.InstructionPackDrift(homePath, "codex", "", at)
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted {
		t.Fatalf("timestamp-only rematerialization drifted: %+v", report)
	}
}

func TestInstructionPackCanonicalWorkspaceAliasesShareContext(t *testing.T) {
	for _, variant := range []string{"symlink", "relative", "harness-alias"} {
		t.Run(variant, func(t *testing.T) {
			homePath, cwd := t.TempDir(), t.TempDir()
			canonical, err := filepath.EvalSymlinks(cwd)
			if err != nil {
				t.Fatal(err)
			}
			in := Input{Harness: "claude_code", Cwd: canonical, Task: "review synthetic source", RepoKind: "product", RepoKindVia: "flag"}
			alias := in
			switch variant {
			case "symlink":
				alias.Cwd = filepath.Join(t.TempDir(), "workspace-alias")
				if err := os.Symlink(canonical, alias.Cwd); err != nil {
					t.Fatal(err)
				}
			case "relative":
				processCwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				alias.Cwd, err = filepath.Rel(processCwd, canonical)
				if err != nil {
					t.Fatal(err)
				}
			case "harness-alias":
				alias.Harness = "claude-code"
			}
			now := time.Now().UTC()
			record := func(input Input, at time.Time, changed bool) RuntimeMaterialization {
				t.Helper()
				runtime, err := MaterializeRuntime(input, DefaultPolicy(), homePath, at)
				if err != nil {
					t.Fatal(err)
				}
				if changed {
					runtime.EffectiveContract.ActiveDecisions = []string{"decision://alias-source-change"}
				}
				sealed, err := SealRuntimeMaterialization(runtime)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := RecordRuntimeMaterialization(homePath, sealed); err != nil {
					t.Fatal(err)
				}
				return sealed
			}
			before := record(in, now, false)
			if _, err := RenderInstructionPackForContext(homePath, in.Harness, "", before.ContextPack.ID, now); err != nil {
				t.Fatal(err)
			}
			after := record(alias, now.Add(time.Second), true)
			if before.EffectiveContract.ContextKey != after.EffectiveContract.ContextKey {
				t.Error("workspace or harness alias forked one canonical context")
			}
			report, err := harness.InstructionPackDrift(homePath, in.Harness, "", now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if !report.Drifted {
				t.Error("own authority change via alias was hidden from installed context")
			}
		})
	}
}
