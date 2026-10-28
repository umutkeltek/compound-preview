package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/authority"
	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/mem"
)

func TestMaterializeOrientContextPackUsesOneSharedRuntimeSnapshot(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	in := contract.Input{
		Harness: "codex", Cwd: t.TempDir(), Task: "shared orient materialization",
		RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute",
	}

	prior := materializeRuntimeForOrient
	calls := 0
	materializeRuntimeForOrient = func(in contract.Input, pol contract.OrientationPolicy, homePath string, now time.Time) (contract.RuntimeMaterialization, error) {
		calls++
		return contract.RuntimeMaterialization{
			EffectiveContract: contract.Compile(in, pol, now),
			ContextPack:       contract.Pack(in, pol, now),
		}, nil
	}
	t.Cleanup(func() { materializeRuntimeForOrient = prior })

	pack, err := materializeOrientContextPack(homePath, in, 0, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("runtime materializations = %d, want one shared snapshot", calls)
	}
	if pack.ID == "" || pack.IDs.ContractID == "" {
		t.Fatalf("recorded pack lost attributable identity: %+v", pack.IDs)
	}
	if got := orientSourceRevision(pack.SourceRevisions, authority.InstalledHarnessEntrypointRevisionPrefix+"codex"); got != "" {
		t.Fatalf("public clone without installed-harness marker must omit entrypoint binding, got %q", got)
	}
}

func TestOrientPrintedAndPersistedRetrievalPostureMatch(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	out := captureStdout(t, func() {
		cmdOrient([]string{
			"--harness", "codex", "--cwd", cwd,
			"--repo-kind", "control-plane", "--task", "offline retrieval posture parity",
		})
	})
	var printed envelope.ContextPack
	if err := json.Unmarshal([]byte(out), &printed); err != nil {
		t.Fatalf("parse printed orient ContextPack: %v\n%s", err, out)
	}
	if printed.Retrieval == nil || printed.Retrieval.Level != envelope.RetrievalLevelNotAttempted {
		t.Fatalf("printed retrieval = %+v, want not_attempted", printed.Retrieval)
	}
	body, err := os.ReadFile(filepath.Join(homePath, "context", "packs", printed.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted envelope.ContextPack
	if err := json.Unmarshal(body, &persisted); err != nil {
		t.Fatalf("parse persisted orient ContextPack: %v", err)
	}
	if !reflect.DeepEqual(printed.Retrieval, persisted.Retrieval) {
		t.Fatalf("printed/persisted retrieval posture drift:\nprinted  %+v\npersisted %+v", printed.Retrieval, persisted.Retrieval)
	}
	if printed.ID != persisted.ID || printed.IDs.ContractID != persisted.IDs.ContractID {
		t.Fatalf("printed/persisted pair identity drift: printed=%s/%s persisted=%s/%s",
			printed.ID, printed.IDs.ContractID, persisted.ID, persisted.IDs.ContractID)
	}
}

func TestMaterializeOrientContextPackReturnsImmutableMaterializationID(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	in := contract.Input{
		Harness: "codex", Cwd: t.TempDir(), Task: "immutable orient materialization",
		RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute",
	}
	prior := materializeRuntimeForOrient
	materializeRuntimeForOrient = func(in contract.Input, pol contract.OrientationPolicy, _ string, now time.Time) (contract.RuntimeMaterialization, error) {
		return contract.RuntimeMaterialization{
			EffectiveContract: contract.Compile(in, pol, now),
			ContextPack:       contract.Pack(in, pol, now),
		}, nil
	}
	t.Cleanup(func() { materializeRuntimeForOrient = prior })

	first, err := materializeOrientContextPack(homePath, in, 0, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(homePath, "context", "packs", first.ID+".json")
	firstBytes, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("read first materialization %s: %v", first.ID, err)
	}
	second, err := materializeOrientContextPack(homePath, in, 0, cliTestNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.IDs.ContractID == second.IDs.ContractID {
		t.Fatalf("distinct runtime snapshots reused proof ids: first=%s/%s second=%s/%s",
			first.ID, first.IDs.ContractID, second.ID, second.IDs.ContractID)
	}
	if got, err := os.ReadFile(firstPath); err != nil || !bytes.Equal(got, firstBytes) {
		t.Fatalf("first context proof changed after second materialization: err=%v", err)
	}
}

func TestMaterializeOrientContextPackDoesNotPersistPartialPairOnPrecommitErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bodyBudget int
		mutate     func(*contract.RuntimeMaterialization)
		bodyError  bool
	}{
		{name: "id mismatch", mutate: func(runtime *contract.RuntimeMaterialization) {
			runtime.ContextPack.IDs.ContractID = "contract_mismatch"
		}},
		{name: "body error", bodyBudget: 1, bodyError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatal(err)
			}
			in := contract.Input{Harness: "codex", Cwd: t.TempDir(), Task: tc.name, RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
			priorRuntime, priorBodies := materializeRuntimeForOrient, materializeBodiesForOrient
			var produced contract.RuntimeMaterialization
			materializeRuntimeForOrient = func(in contract.Input, pol contract.OrientationPolicy, homePath string, now time.Time) (contract.RuntimeMaterialization, error) {
				produced = contract.RuntimeMaterialization{EffectiveContract: contract.Compile(in, pol, now), ContextPack: contract.Pack(in, pol, now)}
				if tc.mutate != nil {
					tc.mutate(&produced)
				}
				return produced, nil
			}
			if tc.bodyError {
				materializeBodiesForOrient = func(string, envelope.ContextPack, string, string, int) (envelope.ContextPack, error) {
					return envelope.ContextPack{}, fmt.Errorf("injected body error")
				}
			}
			t.Cleanup(func() { materializeRuntimeForOrient, materializeBodiesForOrient = priorRuntime, priorBodies })
			if _, err := materializeOrientContextPack(homePath, in, tc.bodyBudget, cliTestNow); err == nil {
				t.Fatal("expected precommit error")
			}
			assertOrientPairAbsent(t, homePath, produced)
		})
	}
}

func assertOrientPairAbsent(t *testing.T, homePath string, runtime contract.RuntimeMaterialization) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(homePath, "contracts", "effective", cliTestNow.Format("2006-01-02"), runtime.EffectiveContract.ID+".json"),
		filepath.Join(homePath, "contracts", "effective", "latest.json"),
		filepath.Join(homePath, "context", "packs", runtime.ContextPack.ID+".json"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("precommit error left artifact %s: %v", path, err)
		}
	}
}

func TestMaterializeOrientContextPackBindsInstalledEntrypointIntoBothArtifacts(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	installedHome := t.TempDir()
	entrypointPath := filepath.Join(installedHome, ".codex", "AGENTS.md")
	entrypointBody := []byte("# exact installed codex entrypoint\n")
	if err := os.MkdirAll(filepath.Dir(entrypointPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypointPath, entrypointBody, 0o644); err != nil {
		t.Fatal(err)
	}
	seedOrientInstalledHarnessHome(t, homePath, installedHome)

	in := contract.Input{
		Harness: "codex", Cwd: t.TempDir(), Task: "bind installed entrypoint",
		RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute",
	}
	pack, err := materializeOrientContextPack(homePath, in, 0, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	wantRevision := fmt.Sprintf("sha256:%x", sha256.Sum256(entrypointBody))
	source := authority.InstalledHarnessEntrypointRevisionPrefix + "codex"
	if got := orientSourceRevision(pack.SourceRevisions, source); got != wantRevision {
		t.Fatalf("ContextPack entrypoint revision = %q, want %q", got, wantRevision)
	}

	contractPath := filepath.Join(
		homePath,
		"contracts",
		"effective",
		cliTestNow.UTC().Format("2006-01-02"),
		pack.IDs.ContractID+".json",
	)
	body, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var effective envelope.EffectiveContract
	if err := json.Unmarshal(body, &effective); err != nil {
		t.Fatal(err)
	}
	if got := orientSourceRevision(effective.SourceRevisions, source); got != wantRevision {
		t.Fatalf("EffectiveContract entrypoint revision = %q, want %q", got, wantRevision)
	}
}

func TestMaterializeOrientContextPackBindsClaudeFamilyAliasesCanonically(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"claude-code", "claude_lane_opus"} {
		t.Run(harness, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatal(err)
			}
			installedHome := t.TempDir()
			entrypoint := filepath.Join(installedHome, ".claude", "CLAUDE.md")
			if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entrypoint, []byte("claude"), 0o644); err != nil {
				t.Fatal(err)
			}
			seedOrientInstalledHarnessHome(t, homePath, installedHome)
			pack, err := materializeOrientContextPack(homePath, contract.Input{Harness: harness, Cwd: t.TempDir(), Task: "alias", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}, 0, cliTestNow)
			if err != nil {
				t.Fatal(err)
			}
			if got := orientSourceRevision(pack.SourceRevisions, authority.InstalledHarnessEntrypointRevisionPrefix+"claude"); got == "" {
				t.Fatalf("canonical claude source missing: %+v", pack.SourceRevisions)
			}
			body, err := os.ReadFile(filepath.Join(homePath, "contracts", "effective", cliTestNow.Format("2006-01-02"), pack.IDs.ContractID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var effective envelope.EffectiveContract
			if err := json.Unmarshal(body, &effective); err != nil {
				t.Fatal(err)
			}
			// Registered aliases share the canonical v0.2 context identity. An
			// unregistered observational label remains unbindable and unchanged.
			wantHarness := map[string]string{"claude-code": "claude_code", "claude_lane_opus": "claude_lane_opus"}[harness]
			if effective.Harness != wantHarness {
				t.Fatalf("artifact harness = %q, want canonical identity %q", effective.Harness, wantHarness)
			}
		})
	}
}

func TestMaterializeOrientContextPackFailsWhenInstalledEntrypointMissing(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	installedHome := t.TempDir()
	seedOrientInstalledHarnessHome(t, homePath, installedHome)

	_, err := materializeOrientContextPack(homePath, contract.Input{
		Harness: "codex", Cwd: t.TempDir(), Task: "missing installed entrypoint",
		RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute",
	}, 0, cliTestNow)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("promoted installed-harness marker must fail closed on missing exact entrypoint, got %v", err)
	}
}

func TestContractCompilePersistsBoundInstalledEntrypointPair(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	source, wantRevision := seedCodexInstalledEntrypointRevision(t, homePath)
	cwd := t.TempDir()
	out := captureStdout(t, func() {
		cmdContract([]string{
			"compile",
			"--harness", "codex",
			"--cwd", cwd,
			"--task", "compile one bound runtime pair",
			"--repo-kind", "control-plane",
		})
	})
	var rendered envelope.EffectiveContract
	if err := json.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatalf("parse contract compile output: %v\n%s", err, out)
	}
	effective, pack := readPersistedRuntimePair(t, homePath, rendered.ID)
	if !reflect.DeepEqual(effective, rendered) {
		t.Fatalf("rendered EffectiveContract differs from persisted artifact:\nrendered=%+v\npersisted=%+v", rendered, effective)
	}
	if got := orientSourceRevision(effective.SourceRevisions, source); got != wantRevision {
		t.Fatalf("persisted EffectiveContract entrypoint revision = %q, want %q", got, wantRevision)
	}
	if got := orientSourceRevision(pack.SourceRevisions, source); got != wantRevision {
		t.Fatalf("persisted ContextPack entrypoint revision = %q, want %q", got, wantRevision)
	}
}

func TestHookLiveSessionStartPersistsBoundInstalledEntrypointPair(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	source, wantRevision := seedCodexInstalledEntrypointRevision(t, homePath)
	cwd := t.TempDir()
	inputBody, err := json.Marshal(map[string]string{
		"hook_event_name": "SessionStart",
		"session_id":      "sess_bound_pair",
		"cwd":             cwd,
		"source":          "startup",
	})
	if err != nil {
		t.Fatal(err)
	}
	var code int
	captureStdout(t, func() {
		withStdin(t, string(inputBody), func() {
			code = runHookCommand([]string{"session-start", "--live", "--harness", "codex"})
		})
	})
	if code != 0 {
		t.Fatalf("live session-start exit = %d", code)
	}
	effective, pack := readPersistedRuntimePair(t, homePath, "")
	if got := orientSourceRevision(effective.SourceRevisions, source); got != wantRevision {
		t.Fatalf("live-hook EffectiveContract entrypoint revision = %q, want %q", got, wantRevision)
	}
	if got := orientSourceRevision(pack.SourceRevisions, source); got != wantRevision {
		t.Fatalf("live-hook ContextPack entrypoint revision = %q, want %q", got, wantRevision)
	}
}

func seedCodexInstalledEntrypointRevision(t *testing.T, homePath string) (string, string) {
	t.Helper()
	installedHome := t.TempDir()
	entrypointBody := []byte("# installed codex entrypoint\n")
	entrypointPath := filepath.Join(installedHome, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(entrypointPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypointPath, entrypointBody, 0o644); err != nil {
		t.Fatal(err)
	}
	seedOrientInstalledHarnessHome(t, homePath, installedHome)
	return authority.InstalledHarnessEntrypointRevisionPrefix + "codex",
		fmt.Sprintf("sha256:%x", sha256.Sum256(entrypointBody))
}

func readPersistedRuntimePair(
	t *testing.T,
	homePath, wantContractID string,
) (envelope.EffectiveContract, envelope.ContextPack) {
	t.Helper()
	latestBody, err := os.ReadFile(filepath.Join(homePath, "contracts", "effective", "latest.json"))
	if err != nil {
		t.Fatalf("read latest EffectiveContract: %v", err)
	}
	var effective envelope.EffectiveContract
	if err := json.Unmarshal(latestBody, &effective); err != nil {
		t.Fatalf("parse latest EffectiveContract: %v", err)
	}
	if wantContractID != "" && effective.ID != wantContractID {
		t.Fatalf("latest EffectiveContract id = %q, want %q", effective.ID, wantContractID)
	}
	entries, err := os.ReadDir(filepath.Join(homePath, "context", "packs"))
	if err != nil {
		t.Fatalf("read persisted ContextPacks: %v", err)
	}
	var matching []envelope.ContextPack
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(homePath, "context", "packs", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var pack envelope.ContextPack
		if err := json.Unmarshal(body, &pack); err != nil {
			t.Fatalf("parse ContextPack %s: %v", entry.Name(), err)
		}
		if pack.IDs.ContractID == effective.ID {
			matching = append(matching, pack)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("persisted ContextPacks joined to contract %s = %d, want 1", effective.ID, len(matching))
	}
	return effective, matching[0]
}

func seedOrientInstalledHarnessHome(t *testing.T, homePath, installedHome string) {
	t.Helper()
	registryPath := filepath.Join(t.TempDir(), "os-path-authority.yaml")
	body := []byte("version: 1\ncanonical_roots:\n  installed_harness_home:\n    path: " + installedHome + "\n    status: active\n")
	if err := os.WriteFile(registryPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := store.Entities()
	if err != nil {
		t.Fatal(err)
	}
	_, entities, facts, _, err := authority.ImportOSPathRegistry(registryPath, existing)
	if err != nil {
		t.Fatal(err)
	}
	for _, entity := range entities {
		if _, err := store.RegisterEntity(entity, cliTestNow); err != nil {
			t.Fatal(err)
		}
	}
	captured, err := store.Capture(facts, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range captured {
		if _, err := store.Adjudicate(candidate.ID, "promote", "orient installed entrypoint fixture", cliTestNow); err != nil {
			t.Fatal(err)
		}
	}
}

func orientSourceRevision(revisions []envelope.SourceRev, source string) string {
	for _, revision := range revisions {
		if revision.Source == source {
			return revision.Rev
		}
	}
	return ""
}
