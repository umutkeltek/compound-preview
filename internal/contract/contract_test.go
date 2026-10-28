package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/projection"
)

func sealForRecord(t *testing.T, runtime RuntimeMaterialization) RuntimeMaterialization {
	t.Helper()
	sealed, err := SealRuntimeMaterialization(runtime)
	if err != nil {
		t.Fatalf("seal runtime materialization: %v", err)
	}
	return sealed
}

func TestProductionHasNoSingleArtifactWriterDoor(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	forbidden := []string{"RecordEffectiveContract(", "RecordContextPack("}
	var findings []string
	for _, productionRoot := range []string{"cmd", "internal"} {
		err := filepath.Walk(filepath.Join(repoRoot, productionRoot), func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, symbol := range forbidden {
				if strings.Contains(string(body), symbol) {
					rel, relErr := filepath.Rel(repoRoot, path)
					if relErr != nil {
						return relErr
					}
					findings = append(findings, rel+":"+symbol)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(findings) != 0 {
		t.Fatalf("single-artifact writer door remains reachable from production: %v", findings)
	}
}

func TestRecordRuntimeMaterializationSerializesConcurrentIdenticalCommits(t *testing.T) {
	homePath := t.TempDir()
	in := mkInput("control-plane")
	oldRuntime := RuntimeMaterialization{EffectiveContract: Compile(in, DefaultPolicy(), fixedNow), ContextPack: Pack(in, DefaultPolicy(), fixedNow)}
	oldRuntime = sealForRecord(t, oldRuntime)
	paths, err := RecordRuntimeMaterialization(homePath, oldRuntime)
	if err != nil {
		t.Fatal(err)
	}
	next := oldRuntime
	next.EffectiveContract.Unknowns = append(next.EffectiveContract.Unknowns, "new")
	next.ContextPack.SummaryMD += " new"
	sealedNext := sealForRecord(t, next)
	nextPaths := RuntimeMaterializationArtifactPaths{
		ContractPath: filepath.Join(homePath, "contracts", "effective", fixedNow.Format("2006-01-02"), sealedNext.EffectiveContract.ID+".json"),
		LatestPath:   paths.LatestPath,
		PackPath:     filepath.Join(homePath, "context", "packs", sealedNext.ContextPack.ID+".json"),
	}

	aAtPack := make(chan struct{})
	releaseA := make(chan struct{})
	bAtLockAttempt := make(chan struct{})
	bLockContended := make(chan struct{})
	bEnteredCommit := make(chan struct{})
	var lockAttempts atomic.Int32
	var contractRenames atomic.Int32
	var packRenames atomic.Int32
	fsOps := runtimeMaterializationFS{
		createTemp: os.CreateTemp,
		beforeLock: func() {
			if lockAttempts.Add(1) == 2 {
				close(bAtLockAttempt)
			}
		},
		lockContended: func() { close(bLockContended) },
		rename: func(oldPath, newPath string) error {
			if newPath == nextPaths.ContractPath && contractRenames.Add(1) == 2 {
				close(bEnteredCommit)
			}
			if newPath == nextPaths.PackPath && packRenames.Add(1) == 1 {
				close(aAtPack)
				<-releaseA
				return fmt.Errorf("injected A pack failure")
			}
			return os.Rename(oldPath, newPath)
		},
	}

	aDone := make(chan error, 1)
	bDone := make(chan error, 1)
	go func() {
		_, err := recordRuntimeMaterializationWithFS(homePath, sealedNext, fsOps)
		aDone <- err
	}()
	<-aAtPack
	go func() {
		_, err := recordRuntimeMaterializationWithFS(homePath, sealedNext, fsOps)
		bDone <- err
	}()
	awaitContractSignal(t, bAtLockAttempt, "B pre-lock attempt")
	awaitContractSignal(t, bLockContended, "B lock contention")
	select {
	case <-bEnteredCommit:
		t.Fatal("B entered the filesystem commit section before A rolled back")
	default:
	}
	close(releaseA)
	if err := <-aDone; err == nil {
		t.Fatal("A must fail at its injected pack rename")
	}
	if err := <-bDone; err != nil {
		t.Fatalf("B must complete after A rolls back: %v", err)
	}
	for path, want := range map[string][]byte{
		nextPaths.ContractPath: mustRuntimeArtifactJSON(t, sealedNext.EffectiveContract),
		nextPaths.PackPath:     mustRuntimeArtifactJSON(t, sealedNext.ContextPack),
		nextPaths.LatestPath:   mustRuntimeArtifactJSON(t, sealedNext.EffectiveContract),
	} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("final artifact mismatch at %s: err=%v", path, err)
		}
	}
}

func awaitContractSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func mustRuntimeArtifactJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(body, '\n')
}

var fixedNow = time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)

func mkInput(kind string) Input {
	return Input{Harness: "claude_code", Cwd: "/repo/x", Task: "fix paywall copy", RepoKind: kind, RepoKindVia: "flag", Mode: "plan"}
}

// The produced contract and pack must be valid envelopes and round-trip losslessly —
// orientation output flows through the same typed spine as everything else.
func TestCompileAndPackValidate(t *testing.T) {
	pol := DefaultPolicy()
	for _, kind := range []string{"product", "control-plane", "library"} {
		in := mkInput(kind)
		ec := Compile(in, pol, fixedNow)
		if err := ec.Validate(); err != nil {
			t.Fatalf("%s contract invalid: %v", kind, err)
		}
		if err := envelope.RoundTrip(&ec); err != nil {
			t.Fatalf("%s contract round-trip: %v", kind, err)
		}
		cp := Pack(in, pol, fixedNow)
		if err := cp.Validate(); err != nil {
			t.Fatalf("%s pack invalid: %v", kind, err)
		}
		if cp.IDs.ContractID != ec.ID {
			t.Fatalf("%s pack contract_id = %q, want %q", kind, cp.IDs.ContractID, ec.ID)
		}
		if err := envelope.RoundTrip(&cp); err != nil {
			t.Fatalf("%s pack round-trip: %v", kind, err)
		}
	}
}

func TestInstalledHookRuntimeReadinessRowsExposeFixtureAndLiveGaps(t *testing.T) {
	manifest := InstalledHooksManifest{
		SchemaVersion: installedHooksSchemaVersion,
		Hooks: []InstalledHook{
			{Harness: "claude_code", Name: "SessionStart", Ref: "hook://claude_code/SessionStart", Status: "current"},
			{Harness: "claude_code", Name: "SessionEnd", Ref: "hook://claude_code/SessionEnd", Status: "current"},
			{Harness: "codex", Name: "SessionStart", Ref: "hook://codex/SessionStart", Status: "current"},
			{Harness: "claude_code", Name: "OldHook", Ref: "hook://claude_code/OldHook", Status: "retired"},
		},
	}
	rows := InstalledHookRuntimeReadinessRows(manifest, []HookRuntimeSupport{
		{Harness: "claude_code", Hook: "SessionStart", FixturePresent: true, FixtureRef: "fixtures/hooks/session-start.json", LiveAdapter: true, LiveCommand: "compound hook session-start --live --harness claude-code"},
		{Harness: "claude_code", Hook: "SessionEnd", LiveAdapter: true, LiveCommand: "compound hook session-end --live --harness claude-code"},
		{Harness: "codex", Hook: "SessionStart", FixturePresent: true, FixtureRef: "fixtures/hooks/session-start.json"},
	})
	if got := contractHookReadiness(rows, "claude_code", "SessionStart"); got == nil || got.Status != "ready" || !got.FixturePresent || !got.LiveAdapter {
		t.Fatalf("claude session-start readiness = %+v", got)
	}
	if got := contractHookReadiness(rows, "claude_code", "SessionEnd"); got == nil || got.Status != "missing_fixture" || got.FixturePresent || !got.LiveAdapter {
		t.Fatalf("claude session-end readiness should expose missing fixture: %+v", got)
	}
	if got := contractHookReadiness(rows, "codex", "SessionStart"); got == nil || got.Status != "missing_live_adapter" || !got.FixturePresent || got.LiveAdapter {
		t.Fatalf("codex session-start readiness should expose missing live adapter: %+v", got)
	}
	if got := contractHookReadiness(rows, "claude_code", "OldHook"); got != nil {
		t.Fatalf("retired hook should not produce readiness row: %+v", got)
	}
}

func TestPreservedExternalInstalledHooksDoNotSurfaceReadiness(t *testing.T) {
	manifest := InstalledHooksManifest{
		SchemaVersion: installedHooksSchemaVersion,
		Hooks: []InstalledHook{
			{
				Harness: "claude_code",
				Name:    "SessionStart",
				Event:   "SessionStart",
				Ref:     "$HOME/.claude/hooks/KittyEnvPersist.hook.ts",
				Status:  "preserved_external",
			},
		},
	}
	if rows := InstalledHookRuntimeReadinessRows(manifest, nil); len(rows) != 0 {
		t.Fatalf("runtime readiness surfaced preserved external row: %+v", rows)
	}
	if rows := InstalledHookGateReadiness(manifest, map[string]bool{}); len(rows) != 0 {
		t.Fatalf("gate readiness surfaced harness known only through preserved external row: %+v", rows)
	}
}

func TestOpaqueInstalledWriteGateRetainsRegisteredReadinessWithoutContentAttestation(t *testing.T) {
	manifest := InstalledHooksManifest{
		SchemaVersion: installedHooksSchemaVersion,
		Hooks: []InstalledHook{
			{
				Harness:  "claude_code",
				Name:     "PreToolUse",
				Event:    "PreToolUse",
				Matcher:  "Write|Edit",
				Ref:      "handler:opaque-runtime",
				Revision: "",
				Status:   "current",
			},
		},
	}
	gate := findHookGateReadiness(InstalledHookGateReadiness(manifest, map[string]bool{"claude_code": true}), "claude_code")
	if gate == nil || gate.Status != "native_blocking" || !gate.Installed {
		t.Fatalf("opaque write-gate readiness = %+v, want installed native_blocking", gate)
	}
	runtime := contractHookReadiness(InstalledHookRuntimeReadinessRows(manifest, []HookRuntimeSupport{
		{Harness: "claude_code", Hook: "PreToolUse", FixturePresent: true, LiveAdapter: true},
	}), "claude_code", "PreToolUse")
	if runtime == nil || runtime.Status != "ready" || !runtime.Installed {
		t.Fatalf("opaque runtime readiness = %+v, want installed ready", runtime)
	}
}

func TestInstalledHookGateReadinessRequiresAvailableNonBlockingPreToolUseObserver(t *testing.T) {
	t.Run("non-blocking status on another event is not a PreToolUse posture", func(t *testing.T) {
		manifest := InstalledHooksManifest{
			SchemaVersion: installedHooksSchemaVersion,
			Hooks: []InstalledHook{{
				Harness:  "codex",
				Name:     "SessionStart",
				Event:    "SessionStart",
				Ref:      "handler:session-start",
				Revision: "sha256:session-start",
				Status:   "non_blocking",
			}},
		}
		gate := findHookGateReadiness(
			InstalledHookGateReadiness(manifest, map[string]bool{"codex": true}),
			"codex",
		)
		if gate == nil || gate.Status != "not_installed" || gate.Installed {
			t.Fatalf("SessionStart-only non-blocking readiness = %+v, want not_installed", gate)
		}
	})

	t.Run("unavailable non-blocking observer remains visible", func(t *testing.T) {
		manifest := InstalledHooksManifest{
			SchemaVersion: installedHooksSchemaVersion,
			Hooks: []InstalledHook{
				{
					Harness:  "codex",
					Name:     "PreToolUse",
					Event:    "PreToolUse",
					Matcher:  "*",
					Ref:      "handler:available-observer",
					Revision: "sha256:available-observer",
					Status:   "non_blocking",
				},
				{
					Harness:  "codex",
					Name:     "PreToolUse",
					Event:    "PreToolUse",
					Matcher:  "*",
					Ref:      "/synthetic/missing-observer",
					Revision: installedHookHandlerUnavailableRevision,
					Status:   "non_blocking",
				},
			},
		}
		gate := findHookGateReadiness(
			InstalledHookGateReadiness(manifest, map[string]bool{"codex": true}),
			"codex",
		)
		if gate == nil || gate.Status != "handler_unavailable" || gate.Installed || len(gate.Actions) == 0 {
			t.Fatalf("mixed available/unavailable observer readiness = %+v, want handler_unavailable remediation", gate)
		}
	})
}

func TestMixedAvailableAndUnavailableWriteGatesFailLoudInEveryOrder(t *testing.T) {
	ready := InstalledHook{
		Harness:  "claude_code",
		Name:     "PreToolUse",
		Event:    "PreToolUse",
		Matcher:  "Write",
		Ref:      "handler:attested-write-gate",
		Revision: "sha256:attested-write-gate",
		Status:   "current",
	}
	unavailable := InstalledHook{
		Harness:  "claude_code",
		Name:     "PreToolUse",
		Event:    "PreToolUse",
		Matcher:  "Edit",
		Ref:      "/synthetic/missing-edit-gate.hook.ts",
		Revision: installedHookHandlerUnavailableRevision,
		Status:   "current",
	}
	orders := []struct {
		name  string
		hooks []InstalledHook
	}{
		{name: "attested_then_unavailable", hooks: []InstalledHook{ready, unavailable}},
		{name: "unavailable_then_attested", hooks: []InstalledHook{unavailable, ready}},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			manifest := InstalledHooksManifest{SchemaVersion: installedHooksSchemaVersion, Hooks: order.hooks}
			gate := findHookGateReadiness(InstalledHookGateReadiness(manifest, map[string]bool{"claude_code": true}), "claude_code")
			assertUnavailableMixedGate(t, gate)

			home := t.TempDir()
			if _, err := importInstalledHooks(home, "synthetic-mixed-gates", []byte(order.name), manifest, installedHooksRegistryImportedVia, fixedNow); err != nil {
				t.Fatal(err)
			}
			in := Input{
				Harness:     "claude_code",
				Cwd:         t.TempDir(),
				Task:        "edit source through mixed write gates",
				RepoKind:    "control-plane",
				RepoKindVia: "flag",
				Mode:        "execute",
			}
			ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			ecGate := findEnvelopeHookReadiness(ec.HookReadiness, "claude_code", "PreToolUse")
			if ecGate == nil || ecGate.Status != "handler_unavailable" || len(ecGate.Actions) == 0 {
				t.Fatalf("effective contract mixed-gate readiness = %+v", ecGate)
			}
			pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			packGate := findEnvelopeHookReadiness(pack.HookReadiness, "claude_code", "PreToolUse")
			if packGate == nil || packGate.Status != "handler_unavailable" || len(packGate.Actions) == 0 {
				t.Fatalf("context pack mixed-gate readiness = %+v", packGate)
			}
			if !containsStringWith(pack.RequiredChecks, "status=handler_unavailable") {
				t.Fatalf("context pack mixed-gate required checks = %+v", pack.RequiredChecks)
			}
		})
	}
}

func assertUnavailableMixedGate(t *testing.T, gate *HookGateReadiness) {
	t.Helper()
	if gate == nil || gate.Status != "handler_unavailable" || gate.Installed || gate.Status == "native_blocking" || len(gate.Actions) == 0 {
		t.Fatalf("mixed write-gate readiness = %+v, want fail-loud handler_unavailable with remediation", gate)
	}
}

func TestModeAffectsContractAndContextPackIDs(t *testing.T) {
	in := mkInput("control-plane")
	in.Task = "same task"
	observe := in
	observe.Mode = "observe"
	execute := in
	execute.Mode = "execute"

	observeContract := Compile(observe, DefaultPolicy(), fixedNow)
	executeContract := Compile(execute, DefaultPolicy(), fixedNow)
	if observeContract.ID == executeContract.ID {
		t.Fatalf("contract IDs should differ by mode: observe=%s execute=%s", observeContract.ID, executeContract.ID)
	}
	observePack := Pack(observe, DefaultPolicy(), fixedNow)
	executePack := Pack(execute, DefaultPolicy(), fixedNow)
	if observePack.ID == executePack.ID {
		t.Fatalf("context pack IDs should differ by mode: observe=%s execute=%s", observePack.ID, executePack.ID)
	}
	if observePack.IDs.ContractID != observeContract.ID || executePack.IDs.ContractID != executeContract.ID {
		t.Fatalf("pack contract IDs do not match mode-specific contracts: observe=%q/%q execute=%q/%q", observePack.IDs.ContractID, observeContract.ID, executePack.IDs.ContractID, executeContract.ID)
	}
}

func TestRecordRuntimeMaterializationWritesDatedLatestAndPackArtifacts(t *testing.T) {
	homePath := t.TempDir()
	in := mkInput("control-plane")
	ec := Compile(in, DefaultPolicy(), fixedNow)
	pack := Pack(in, DefaultPolicy(), fixedNow)
	sealed, err := SealRuntimeMaterialization(RuntimeMaterialization{
		EffectiveContract: ec,
		ContextPack:       pack,
	})
	if err != nil {
		t.Fatal(err)
	}

	paths, err := RecordRuntimeMaterialization(homePath, sealed)
	if err != nil {
		t.Fatal(err)
	}

	wantContractPath := filepath.Join(homePath, "contracts", "effective", "2026-06-28", sealed.EffectiveContract.ID+".json")
	wantLatestPath := filepath.Join(homePath, "contracts", "effective", "latest.json")
	wantPackPath := filepath.Join(homePath, "context", "packs", sealed.ContextPack.ID+".json")
	if paths.ContractPath != wantContractPath {
		t.Fatalf("contract path = %q, want %q", paths.ContractPath, wantContractPath)
	}
	if paths.LatestPath != wantLatestPath {
		t.Fatalf("latest path = %q, want %q", paths.LatestPath, wantLatestPath)
	}
	if paths.PackPath != wantPackPath {
		t.Fatalf("pack path = %q, want %q", paths.PackPath, wantPackPath)
	}
	invalid := ec
	invalid.CreatedAt = "not-a-time"
	invalidRuntime := sealForRecord(t, RuntimeMaterialization{
		EffectiveContract: invalid,
		ContextPack:       pack,
	})
	if _, err := RecordRuntimeMaterialization(homePath, invalidRuntime); err == nil {
		t.Fatal("expected invalid created_at to fail")
	}

	for _, path := range []string{paths.ContractPath, paths.LatestPath} {
		var got envelope.EffectiveContract
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if got.ID != sealed.EffectiveContract.ID || got.Harness != ec.Harness {
			t.Fatalf("artifact %s = id %q harness %q, want id %q harness %q", path, got.ID, got.Harness, sealed.EffectiveContract.ID, ec.Harness)
		}
	}
	var got envelope.ContextPack
	data, err := os.ReadFile(paths.PackPath)
	if err != nil {
		t.Fatalf("read context pack artifact: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse context pack artifact: %v", err)
	}
	if got.ID != sealed.ContextPack.ID || got.IDs.ContractID != sealed.ContextPack.IDs.ContractID {
		t.Fatalf("artifact id/contract = %q/%q, want %q/%q", got.ID, got.IDs.ContractID, sealed.ContextPack.ID, sealed.ContextPack.IDs.ContractID)
	}
}

func TestRecordRuntimeMaterializationRejectsUnsealedCallerState(t *testing.T) {
	in := mkInput("control-plane")
	runtime := RuntimeMaterialization{
		EffectiveContract: Compile(in, DefaultPolicy(), fixedNow),
		ContextPack:       Pack(in, DefaultPolicy(), fixedNow),
	}
	if _, err := RecordRuntimeMaterialization(t.TempDir(), runtime); err == nil {
		t.Fatal("record accepted request-shaped ids that remain stale in caller state")
	} else if !strings.Contains(err.Error(), "must be sealed before recording") {
		t.Fatalf("error = %q, want explicit caller-visible seal requirement", err)
	}
}

func TestRecordRuntimeMaterializationKeepsPriorProofRefsImmutable(t *testing.T) {
	homePath := t.TempDir()
	in := mkInput("control-plane")
	first := RuntimeMaterialization{
		EffectiveContract: Compile(in, DefaultPolicy(), fixedNow),
		ContextPack:       Pack(in, DefaultPolicy(), fixedNow),
	}
	first = sealForRecord(t, first)
	firstPaths, err := RecordRuntimeMaterialization(homePath, first)
	if err != nil {
		t.Fatal(err)
	}
	firstContractBytes, err := os.ReadFile(firstPaths.ContractPath)
	if err != nil {
		t.Fatal(err)
	}
	firstPackBytes, err := os.ReadFile(firstPaths.PackPath)
	if err != nil {
		t.Fatal(err)
	}

	second := first
	second.EffectiveContract.CreatedAt = fixedNow.Add(time.Second).Format(time.RFC3339)
	second.ContextPack.CreatedAt = fixedNow.Add(time.Second).Format(time.RFC3339)
	second.EffectiveContract.SourceRevisions = append(
		[]envelope.SourceRev(nil),
		first.EffectiveContract.SourceRevisions...,
	)
	second.ContextPack.SourceRevisions = append(
		[]envelope.SourceRev(nil),
		first.ContextPack.SourceRevisions...,
	)
	second.EffectiveContract.SourceRevisions[0].Rev = "sha256:second"
	second.ContextPack.SourceRevisions[0].Rev = "sha256:second"
	second = sealForRecord(t, second)
	secondPaths, err := RecordRuntimeMaterialization(homePath, second)
	if err != nil {
		t.Fatal(err)
	}
	if firstPaths.ContractPath == secondPaths.ContractPath || firstPaths.PackPath == secondPaths.PackPath {
		t.Fatalf("distinct materializations reused proof paths:\nfirst=%+v\nsecond=%+v", firstPaths, secondPaths)
	}
	if got, err := os.ReadFile(firstPaths.ContractPath); err != nil || !bytes.Equal(got, firstContractBytes) {
		t.Fatalf("prior effective-contract proof changed: err=%v", err)
	}
	if got, err := os.ReadFile(firstPaths.PackPath); err != nil || !bytes.Equal(got, firstPackBytes) {
		t.Fatalf("prior context-pack proof changed: err=%v", err)
	}
}

func TestRecordRuntimeMaterializationRejectsInstalledEntrypointRevisionSkew(t *testing.T) {
	const (
		source    = "installed-entrypoint://claude"
		revisionA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		revisionB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	base := RuntimeMaterialization{
		EffectiveContract: Compile(mkInput("control-plane"), DefaultPolicy(), fixedNow),
		ContextPack:       Pack(mkInput("control-plane"), DefaultPolicy(), fixedNow),
	}
	tests := []struct {
		name   string
		mutate func(*RuntimeMaterialization)
	}{
		{
			name: "contract binding missing",
			mutate: func(runtime *RuntimeMaterialization) {
				runtime.ContextPack.SourceRevisions = append(runtime.ContextPack.SourceRevisions, envelope.SourceRev{Source: source, Rev: revisionA})
			},
		},
		{
			name: "context pack binding missing",
			mutate: func(runtime *RuntimeMaterialization) {
				runtime.EffectiveContract.SourceRevisions = append(runtime.EffectiveContract.SourceRevisions, envelope.SourceRev{Source: source, Rev: revisionA})
			},
		},
		{
			name: "binding revisions differ",
			mutate: func(runtime *RuntimeMaterialization) {
				runtime.EffectiveContract.SourceRevisions = append(runtime.EffectiveContract.SourceRevisions, envelope.SourceRev{Source: source, Rev: revisionA})
				runtime.ContextPack.SourceRevisions = append(runtime.ContextPack.SourceRevisions, envelope.SourceRev{Source: source, Rev: revisionB})
			},
		},
		{
			name: "binding revision empty",
			mutate: func(runtime *RuntimeMaterialization) {
				runtime.EffectiveContract.SourceRevisions = append(runtime.EffectiveContract.SourceRevisions, envelope.SourceRev{Source: source})
				runtime.ContextPack.SourceRevisions = append(runtime.ContextPack.SourceRevisions, envelope.SourceRev{Source: source})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := base
			runtime.EffectiveContract.SourceRevisions = append([]envelope.SourceRev(nil), base.EffectiveContract.SourceRevisions...)
			runtime.ContextPack.SourceRevisions = append([]envelope.SourceRev(nil), base.ContextPack.SourceRevisions...)
			test.mutate(&runtime)
			runtime = sealForRecord(t, runtime)

			if _, err := RecordRuntimeMaterialization(t.TempDir(), runtime); err == nil {
				t.Fatal("installed entrypoint revision skew must fail pair persistence")
			} else if !strings.Contains(err.Error(), "installed entrypoint revision parity") {
				t.Fatalf("error = %q, want installed entrypoint revision parity failure", err)
			}
		})
	}
}

func TestRecordRuntimeMaterializationAcceptsMatchingInstalledEntrypointRevision(t *testing.T) {
	const (
		source   = "installed-entrypoint://claude"
		revision = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	runtime := RuntimeMaterialization{
		EffectiveContract: Compile(mkInput("control-plane"), DefaultPolicy(), fixedNow),
		ContextPack:       Pack(mkInput("control-plane"), DefaultPolicy(), fixedNow),
	}
	binding := envelope.SourceRev{Source: source, Rev: revision}
	runtime.EffectiveContract.SourceRevisions = append(runtime.EffectiveContract.SourceRevisions, binding)
	runtime.ContextPack.SourceRevisions = append(runtime.ContextPack.SourceRevisions, binding)
	runtime = sealForRecord(t, runtime)

	paths, err := RecordRuntimeMaterialization(t.TempDir(), runtime)
	if err != nil {
		t.Fatalf("matching installed entrypoint revisions must persist: %v", err)
	}
	for _, path := range []string{paths.ContractPath, paths.PackPath, paths.LatestPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("matching pair artifact %s missing: %v", path, err)
		}
	}
}

func TestRecordRuntimeMaterializationLeavesNoOrphanOnPackFailure(t *testing.T) {
	homePath := t.TempDir()
	in := mkInput("control-plane")
	runtime := RuntimeMaterialization{
		EffectiveContract: Compile(in, DefaultPolicy(), fixedNow),
		ContextPack:       Pack(in, DefaultPolicy(), fixedNow),
	}
	sealed, err := SealRuntimeMaterialization(runtime)
	if err != nil {
		t.Fatal(err)
	}
	blockedPack := filepath.Join(homePath, "context", "packs", sealed.ContextPack.ID+".json")
	if err := os.MkdirAll(blockedPack, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordRuntimeMaterialization(homePath, sealed); err == nil {
		t.Fatal("pack staging failure must fail the pair")
	}
	contractPath := filepath.Join(homePath, "contracts", "effective", fixedNow.Format("2006-01-02"), sealed.EffectiveContract.ID+".json")
	latestPath := filepath.Join(homePath, "contracts", "effective", "latest.json")
	for _, path := range []string{contractPath, latestPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("pair failure left orphan %s: %v", path, err)
		}
	}
}

func TestRecordRuntimeMaterializationRestoresAllPreimagesWhenLatestCommitFails(t *testing.T) {
	homePath := t.TempDir()
	in := mkInput("control-plane")
	runtime := RuntimeMaterialization{EffectiveContract: Compile(in, DefaultPolicy(), fixedNow), ContextPack: Pack(in, DefaultPolicy(), fixedNow)}
	runtime = sealForRecord(t, runtime)
	paths, err := RecordRuntimeMaterialization(homePath, runtime)
	if err != nil {
		t.Fatal(err)
	}
	preimages := map[string][]byte{}
	for _, path := range []string{paths.ContractPath, paths.PackPath, paths.LatestPath} {
		preimages[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	runtime.ContextPack.SummaryMD += " changed"
	runtime.EffectiveContract.Unknowns = append(runtime.EffectiveContract.Unknowns, "changed")
	changed := sealForRecord(t, runtime)
	changedPaths := RuntimeMaterializationArtifactPaths{
		ContractPath: filepath.Join(homePath, "contracts", "effective", fixedNow.Format("2006-01-02"), changed.EffectiveContract.ID+".json"),
		LatestPath:   paths.LatestPath,
		PackPath:     filepath.Join(homePath, "context", "packs", changed.ContextPack.ID+".json"),
	}
	fsOps := runtimeMaterializationFS{createTemp: os.CreateTemp, rename: func(oldPath, newPath string) error {
		if newPath == paths.LatestPath {
			return fmt.Errorf("injected latest commit failure")
		}
		return os.Rename(oldPath, newPath)
	}}
	if _, err := recordRuntimeMaterializationWithFS(homePath, changed, fsOps); err == nil {
		t.Fatal("latest commit injection must fail")
	}
	for path, want := range preimages {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("preimage changed at %s: err=%v", path, err)
		}
	}
	for _, path := range []string{changedPaths.ContractPath, changedPaths.PackPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("failed materialization left new immutable artifact %s: %v", path, err)
		}
	}
	for _, dir := range []string{filepath.Dir(paths.ContractPath), filepath.Dir(paths.PackPath), filepath.Dir(paths.LatestPath)} {
		matches, err := filepath.Glob(filepath.Join(dir, ".runtime-materialization-*"))
		if err != nil || len(matches) != 0 {
			t.Fatalf("latest failure left temp files in %s: %v err=%v", dir, matches, err)
		}
	}
}

func TestRecordRuntimeMaterializationReportsRollbackFailures(t *testing.T) {
	homePath := t.TempDir()
	in := mkInput("control-plane")
	runtime := RuntimeMaterialization{
		EffectiveContract: Compile(in, DefaultPolicy(), fixedNow),
		ContextPack:       Pack(in, DefaultPolicy(), fixedNow),
	}
	runtime = sealForRecord(t, runtime)
	paths, err := RecordRuntimeMaterialization(homePath, runtime)
	if err != nil {
		t.Fatal(err)
	}
	runtime.EffectiveContract.Unknowns = append(runtime.EffectiveContract.Unknowns, "changed")
	runtime.ContextPack.SummaryMD += " changed"
	changed := sealForRecord(t, runtime)
	changedContractPath := filepath.Join(homePath, "contracts", "effective", fixedNow.Format("2006-01-02"), changed.EffectiveContract.ID+".json")
	changedPackPath := filepath.Join(homePath, "context", "packs", changed.ContextPack.ID+".json")

	fsOps := runtimeMaterializationFS{
		createTemp: os.CreateTemp,
		rename: func(oldPath, newPath string) error {
			if newPath != paths.LatestPath {
				return os.Rename(oldPath, newPath)
			}
			if err := os.Remove(changedPackPath); err != nil {
				t.Fatalf("remove committed pack before rollback injection: %v", err)
			}
			if err := os.Mkdir(changedPackPath, 0o755); err != nil {
				t.Fatalf("replace committed pack with rollback blocker: %v", err)
			}
			if err := os.WriteFile(filepath.Join(changedPackPath, "blocker"), []byte("block"), 0o644); err != nil {
				t.Fatalf("make rollback removal fail: %v", err)
			}
			return fmt.Errorf("injected latest commit failure")
		},
	}
	_, err = recordRuntimeMaterializationWithFS(homePath, changed, fsOps)
	if err == nil {
		t.Fatal("latest commit plus rollback failure must be reported")
	}
	for _, want := range []string{"injected latest commit failure", "rollback remove"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to include %q", err, want)
		}
	}
	if _, readErr := os.Stat(changedContractPath); !os.IsNotExist(readErr) {
		t.Fatalf("contract rollback did not continue after pack rollback failure: %v", readErr)
	}
}

func TestRecordRuntimeMaterializationTreatsAlreadyAbsentRollbackTargetAsRestored(t *testing.T) {
	homePath := t.TempDir()
	in := mkInput("control-plane")
	runtime := RuntimeMaterialization{
		EffectiveContract: Compile(in, DefaultPolicy(), fixedNow),
		ContextPack:       Pack(in, DefaultPolicy(), fixedNow),
	}
	runtime = sealForRecord(t, runtime)
	paths, err := RecordRuntimeMaterialization(homePath, runtime)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.ContractPath, paths.PackPath, paths.LatestPath} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	runtime.EffectiveContract.Unknowns = append(runtime.EffectiveContract.Unknowns, "changed")
	runtime.ContextPack.SummaryMD += " changed"
	changed := sealForRecord(t, runtime)
	changedContractPath := filepath.Join(homePath, "contracts", "effective", fixedNow.Format("2006-01-02"), changed.EffectiveContract.ID+".json")
	fsOps := runtimeMaterializationFS{
		createTemp: os.CreateTemp,
		rename: func(oldPath, newPath string) error {
			if newPath != paths.LatestPath {
				return os.Rename(oldPath, newPath)
			}
			if err := os.Remove(changedContractPath); err != nil {
				t.Fatalf("make contract rollback target already absent: %v", err)
			}
			return fmt.Errorf("injected latest commit failure")
		},
	}
	_, err = recordRuntimeMaterializationWithFS(homePath, changed, fsOps)
	if err == nil || !strings.Contains(err.Error(), "injected latest commit failure") {
		t.Fatalf("error = %v, want injected commit failure", err)
	}
	if strings.Contains(err.Error(), "rollback remove") {
		t.Fatalf("already absent rollback target must count as restored, got %q", err)
	}
}

func TestRecordRuntimeMaterializationPackStageFailurePreservesAllPreimages(t *testing.T) {
	homePath := t.TempDir()
	in := mkInput("control-plane")
	runtime := RuntimeMaterialization{EffectiveContract: Compile(in, DefaultPolicy(), fixedNow), ContextPack: Pack(in, DefaultPolicy(), fixedNow)}
	runtime = sealForRecord(t, runtime)
	paths, err := RecordRuntimeMaterialization(homePath, runtime)
	if err != nil {
		t.Fatal(err)
	}
	preimages := map[string][]byte{}
	for _, path := range []string{paths.ContractPath, paths.PackPath, paths.LatestPath} {
		preimages[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	runtime.EffectiveContract.Unknowns = append(runtime.EffectiveContract.Unknowns, "changed")
	runtime.ContextPack.SummaryMD += " changed"
	changed := sealForRecord(t, runtime)
	calls := 0
	fsOps := runtimeMaterializationFS{rename: os.Rename, createTemp: func(dir, pattern string) (*os.File, error) {
		calls++
		if calls == 2 {
			return nil, fmt.Errorf("injected pack stage failure")
		}
		return os.CreateTemp(dir, pattern)
	}}
	if _, err := recordRuntimeMaterializationWithFS(homePath, changed, fsOps); err == nil {
		t.Fatal("pack stage injection must fail")
	}
	for path, want := range preimages {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("preimage changed at %s: err=%v", path, err)
		}
	}
	for _, dir := range []string{filepath.Dir(paths.ContractPath), filepath.Dir(paths.PackPath), filepath.Dir(paths.LatestPath)} {
		matches, err := filepath.Glob(filepath.Join(dir, ".runtime-materialization-*"))
		if err != nil || len(matches) != 0 {
			t.Fatalf("stage failure left temp files in %s: %v err=%v", dir, matches, err)
		}
	}
}

// The acceptance test for P2: the SAME task yields DIFFERENT packs in a product repo
// vs the control-plane repo. Governance context is excluded in product, included in
// control-plane — the orientation-scope fix.
func TestProductVsControlPlaneDifferentPacks(t *testing.T) {
	pol := DefaultPolicy()
	prod := Pack(mkInput("product"), pol, fixedNow)
	ctl := Pack(mkInput("control-plane"), pol, fixedNow)

	inclRefs := func(cp envelope.ContextPack) map[string]bool {
		m := map[string]bool{}
		for _, r := range cp.Included {
			m[r.Ref] = true
		}
		return m
	}
	exclRefs := func(cp envelope.ContextPack) map[string]bool {
		m := map[string]bool{}
		for _, r := range cp.Excluded {
			m[r.Ref] = true
		}
		return m
	}

	gov := "control-plane://orientation-map"
	if !exclRefs(prod)[gov] {
		t.Errorf("product pack should EXCLUDE governance map %q; excluded=%v", gov, prod.Excluded)
	}
	if !inclRefs(ctl)[gov] {
		t.Errorf("control-plane pack should INCLUDE governance map %q; included=%v", gov, ctl.Included)
	}
	// product-docs is the mirror case: included in product, excluded in control-plane.
	docs := "repo://docs"
	if !inclRefs(prod)[docs] {
		t.Errorf("product pack should INCLUDE product docs %q", docs)
	}
	if !exclRefs(ctl)[docs] {
		t.Errorf("control-plane pack should EXCLUDE product docs %q", docs)
	}
	// And the packs must genuinely differ in their included set.
	if len(prod.Included) == len(ctl.Included) && len(prod.Excluded) == len(ctl.Excluded) {
		t.Errorf("packs should differ: prod incl=%d excl=%d, ctl incl=%d excl=%d",
			len(prod.Included), len(prod.Excluded), len(ctl.Included), len(ctl.Excluded))
	}
	// Every excluded ref must carry a reason (the whole point of recording exclusion).
	for _, r := range prod.Excluded {
		if r.Reason == "" {
			t.Errorf("excluded ref %q has no reason", r.Ref)
		}
	}
}

// TestPackServesOperatingCeremoniesForFreshClone is the F3 falsifier: a fresh public
// clone's `orient --harness claude` (no repo-kind marker → defaults to product) must
// still SERVE the tracked build-ceremony surface, so the four ceremonies are reachable
// without the gitignored .claude/skills overlay. The ceremony index is ambient-for-all,
// exactly like the operating contract (AGENTS.md), so no repo-kind guess can drop it.
func TestPackServesOperatingCeremoniesForFreshClone(t *testing.T) {
	const ceremonyRef = "skills/README.md"
	for _, kind := range []string{"product", "control-plane", "library"} {
		pack := Pack(mkInput(kind), DefaultPolicy(), fixedNow)
		found := false
		for _, r := range pack.Included {
			if r.Ref == ceremonyRef {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s pack must serve operating-ceremonies ref %q (fresh-clone reachable); included=%+v", kind, ceremonyRef, pack.Included)
		}
	}
}

func TestPackIncludesUniversalPrinciplesTier0Constitution(t *testing.T) {
	repo := t.TempDir()
	docPath := filepath.Join(repo, "docs", "architecture", "universal-principles.md")
	if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Universal Principles\n\n" +
		"## The Laws · *irreducible core*\n" +
		"1. Discover reality before acting.\n" +
		"2. Own the spine; rent the executors.\n" +
		"16. Govern in proportion to risk.\n\n" +
		"## Open Frontier\n- unresolved\n"
	if err := os.WriteFile(docPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Input{Harness: "codex", Cwd: repo, Task: "ship a small change", RepoKind: "product", RepoKindVia: "fixture", Mode: "execute"}
	pack := Pack(in, DefaultPolicy(), fixedNow)
	if !hasIncluded(pack, universalPrinciplesRef) {
		t.Fatalf("context pack missing Tier-0 constitution ref: %+v", pack.Included)
	}
	if !contains(pack.ActiveInstructions, universalPrinciplesRef) {
		t.Fatalf("active instructions missing Tier-0 constitution ref: %+v", pack.ActiveInstructions)
	}
	if !strings.Contains(pack.SummaryMD, "Tier-0 constitution: Laws 1. Discover reality before acting.") || !strings.Contains(pack.SummaryMD, "16. Govern in proportion to risk.") {
		t.Fatalf("summary should surface compact Laws, got %q", pack.SummaryMD)
	}
	if rev := sourceRev(pack.SourceRevisions, universalPrinciplesRef); !strings.HasPrefix(rev, "sha256:") {
		t.Fatalf("constitution source revision = %q, want sha256", rev)
	}
}
func TestPackWithCapabilitiesIncludesProjectionContextByRepoKind(t *testing.T) {
	home := t.TempDir()
	report, err := projection.DoctorFile(filepath.Join("..", "..", "fixtures", "projections", "stale-freshness.yaml"), time.Date(2026, 6, 29, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := projection.RecordContext(home, report, []string{"control-plane"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	control, err := PackWithCapabilities(mkInput("control-plane"), DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	projectionRef, ok := findIncludedRef(control, "projection://synthetic-runtime-dashboard")
	if !ok {
		t.Fatalf("control-plane pack should include projection freshness ref: %+v", control.Included)
	}
	if projectionRef.SourceClass != "generated" || projectionRef.Status != "stale" ||
		!strings.Contains(projectionRef.Reason, "generated-view context evidence") {
		t.Fatalf("projection context ref should stay generated/stale evidence, got %+v", projectionRef)
	}
	product, err := PackWithCapabilities(mkInput("product"), DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findIncludedRef(product, "projection://synthetic-runtime-dashboard"); ok {
		t.Fatalf("product pack should not inherit control-plane projection context: %+v", product.Included)
	}
}

func TestPackWithCapabilitiesOffersOSAdaptationSteeringForControlPlaneTask(t *testing.T) {
	home := t.TempDir()
	repo := writeOSAdaptationRuleFixture(t)
	in := Input{
		Harness:     "codex",
		Cwd:         repo,
		Task:        "adapt one operating-system capability into Compound context-aware authority",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatal(err)
	}
	offer := findCapabilityOffer(pack, osAdaptationCapabilityRef)
	if offer == nil {
		t.Fatalf("control-plane OS adaptation task should get adaptation offer: %+v", pack.CapabilityOffers)
	}
	if offer.Kind != "doctrine" || !strings.Contains(offer.Why, "trigger=old-system+adaptation") {
		t.Fatalf("offer should explain trigger metadata: %+v", offer)
	}
	if !hasOfferConstraint(offer.Required, "context_rule", osAdaptationRuleRef) {
		t.Fatalf("offer missing required private rule ref: %+v", offer.Required)
	}
	if !hasOfferConstraint(offer.Required, "trigger", "context://old-system-adaptation-task") {
		t.Fatalf("offer missing explicit trigger constraint: %+v", offer.Required)
	}
	if !hasOfferConstraint(offer.Required, "action", "compound://runtime/context-pack-adaptation-contract") {
		t.Fatalf("offer missing explicit action constraint: %+v", offer.Required)
	}
	if !hasOfferConstraint(offer.Required, "boundary", "compound://boundary/public-private") {
		t.Fatalf("offer missing explicit boundary constraint: %+v", offer.Required)
	}
	if !hasOfferConstraint(offer.Required, "silence", "context://ordinary-product-library-work") {
		t.Fatalf("offer missing explicit silence constraint: %+v", offer.Required)
	}
	if !hasOfferConstraint(offer.Required, "proof_gate", "compound://architecture-doctor/adaptation-steering") {
		t.Fatalf("offer missing architecture doctor proof gate: %+v", offer.Required)
	}
	if !hasOfferConstraint(offer.Required, "retirement", "compound://adaptation/old-owner-narrowing") {
		t.Fatalf("offer missing retirement/narrowing constraint: %+v", offer.Required)
	}
	if !hasOfferConstraint(offer.Conflicts, "conflicts_with", "pattern://copy-old-instructions-verbatim") {
		t.Fatalf("offer missing copy-paste conflict: %+v", offer.Conflicts)
	}
	if !hasOfferConstraint(offer.Evidence, "source_revision", osAdaptationRuleRef) {
		t.Fatalf("offer missing source revision evidence: %+v", offer.Evidence)
	}
	if !strings.Contains(offer.CostRiskNote, "stays silent") {
		t.Fatalf("offer should carry silence condition: %+v", offer)
	}
	if !hasIncluded(pack, osAdaptationCapabilityRef) {
		t.Fatalf("ContextPack should include adaptation capability ref: %+v", pack.Included)
	}
	included, _ := findIncludedRef(pack, osAdaptationCapabilityRef)
	if included.SourceClass != "private_overlay" || !strings.Contains(included.Reason, "no private body injection") {
		t.Fatalf("included ref should stay private-overlay refs-only: %+v", included)
	}
	if rev := sourceRev(pack.SourceRevisions, osAdaptationRuleRef); !strings.HasPrefix(rev, "sha256:") {
		t.Fatalf("ContextPack missing adaptation rule source revision: %+v", pack.SourceRevisions)
	}
	if !strings.Contains(pack.SummaryMD, "context-aware OS adaptation offer included") {
		t.Fatalf("summary should mention adaptation offer: %s", pack.SummaryMD)
	}
}

func TestPackWithCapabilitiesOSAdaptationSteeringStaysSilentForProductContext(t *testing.T) {
	home := t.TempDir()
	repo := writeOSAdaptationRuleFixture(t)
	in := Input{
		Harness:     "codex",
		Cwd:         repo,
		Task:        "adapt one operating-system capability into Compound context-aware authority",
		RepoKind:    "product",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if offer := findCapabilityOffer(pack, osAdaptationCapabilityRef); offer != nil {
		t.Fatalf("product context must not receive control-plane adaptation offer: %+v", offer)
	}
	if hasIncluded(pack, osAdaptationCapabilityRef) {
		t.Fatalf("product context must not include adaptation steering ref: %+v", pack.Included)
	}
	if rev := sourceRev(pack.SourceRevisions, osAdaptationRuleRef); rev != "" {
		t.Fatalf("product context must not consume adaptation rule source revision: %+v", pack.SourceRevisions)
	}
}

func TestPackWithCapabilitiesOSAdaptationSteeringStaysSilentForOrdinaryControlPlaneWork(t *testing.T) {
	home := t.TempDir()
	repo := writeOSAdaptationRuleFixture(t)
	in := Input{
		Harness:     "codex",
		Cwd:         repo,
		Task:        "repair product signal projection counters",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if offer := findCapabilityOffer(pack, osAdaptationCapabilityRef); offer != nil {
		t.Fatalf("ordinary control-plane work must not carry OS adaptation steering: %+v", offer)
	}
	if hasIncluded(pack, osAdaptationCapabilityRef) {
		t.Fatalf("ordinary control-plane work must not include adaptation steering ref: %+v", pack.Included)
	}
	if rev := sourceRev(pack.SourceRevisions, osAdaptationRuleRef); rev != "" {
		t.Fatalf("ordinary control-plane work must not consume adaptation rule source revision: %+v", pack.SourceRevisions)
	}
}

func TestCompileWithRuntimeIncludesOSAdaptationSteeringOffer(t *testing.T) {
	home := t.TempDir()
	repo := writeOSAdaptationRuleFixture(t)
	in := Input{
		Harness:     "codex",
		Cwd:         repo,
		Task:        "adapt one operating-system hook into Compound ContextPack authority",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := ec.Validate(); err != nil {
		t.Fatal(err)
	}
	offer := findEffectiveContractCapabilityOffer(ec, osAdaptationCapabilityRef)
	if offer == nil {
		t.Fatalf("effective contract should carry adaptation steering offer: %+v", ec.CapabilityOffers)
	}
	if !hasOfferConstraint(offer.Required, "action", "compound://runtime/context-pack-adaptation-contract") ||
		!hasOfferConstraint(offer.Required, "silence", "context://ordinary-product-library-work") {
		t.Fatalf("effective contract adaptation offer should carry action and silence constraints: %+v", offer.Required)
	}
	if rev := sourceRev(ec.SourceRevisions, osAdaptationRuleRef); !strings.HasPrefix(rev, "sha256:") {
		t.Fatalf("effective contract missing adaptation rule source revision: %+v", ec.SourceRevisions)
	}
}

func TestPackWithCapabilitiesOffersOracleSecondOpinionForHighLeverageDoctrineTask(t *testing.T) {
	home := t.TempDir()
	repo := writeOracleSecondOpinionRuleFixture(t)
	in := Input{
		Harness:     "codex",
		Cwd:         repo,
		Task:        "change memory governance and public private policy; ask Oracle for architecture second opinion",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatal(err)
	}
	offer := findCapabilityOffer(pack, oracleSecondOpinionCapabilityRef)
	if offer == nil {
		t.Fatalf("high-leverage governance task should get Oracle second-opinion offer: %+v", pack.CapabilityOffers)
	}
	if offer.Kind != "procedure" || !strings.Contains(offer.Why, "trigger=high-leverage-governance") {
		t.Fatalf("offer should explain trigger metadata: %+v", offer)
	}
	for _, tc := range []struct {
		kind string
		ref  string
	}{
		{"context_rule", oracleSecondOpinionRuleRef},
		{"trigger", "context://high-leverage-governance-change"},
		{"action", "compound://runtime/oracle-second-opinion-evidence"},
		{"writeback", "compound://source-observe/second-opinion-evidence"},
		{"boundary", "compound://boundary/oracle-evidence-not-authority"},
		{"silence", "context://ordinary-deterministic-work"},
		{"proof_gate", "compound://proof/local-state-delta-after-oracle"},
		{"retirement", "compound://adaptation/oracle-backend-narrowing"},
	} {
		if !hasOfferConstraint(offer.Required, tc.kind, tc.ref) {
			t.Fatalf("offer missing %s/%s constraint: %+v", tc.kind, tc.ref, offer.Required)
		}
	}
	if !hasOfferConstraint(offer.Conflicts, "conflicts_with", "pattern://oracle-as-authority") {
		t.Fatalf("offer missing oracle-as-authority conflict: %+v", offer.Conflicts)
	}
	if !hasOfferConstraint(offer.Evidence, "source_revision", oracleSecondOpinionRuleRef) {
		t.Fatalf("offer missing source revision evidence: %+v", offer.Evidence)
	}
	if !hasIncluded(pack, oracleSecondOpinionCapabilityRef) {
		t.Fatalf("ContextPack should include Oracle second-opinion ref: %+v", pack.Included)
	}
	if rev := sourceRev(pack.SourceRevisions, oracleSecondOpinionRuleRef); !strings.HasPrefix(rev, "sha256:") {
		t.Fatalf("ContextPack missing Oracle rule source revision: %+v", pack.SourceRevisions)
	}
}

func TestPackWithCapabilitiesOracleSecondOpinionStaysSilentForOrdinaryWork(t *testing.T) {
	home := t.TempDir()
	repo := writeOracleSecondOpinionRuleFixture(t)
	in := Input{
		Harness:     "codex",
		Cwd:         repo,
		Task:        "repair product signal projection counters",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if offer := findCapabilityOffer(pack, oracleSecondOpinionCapabilityRef); offer != nil {
		t.Fatalf("ordinary work must not carry Oracle second-opinion offer: %+v", offer)
	}
	if hasIncluded(pack, oracleSecondOpinionCapabilityRef) {
		t.Fatalf("ordinary work must not include Oracle second-opinion ref: %+v", pack.Included)
	}
	if rev := sourceRev(pack.SourceRevisions, oracleSecondOpinionRuleRef); rev != "" {
		t.Fatalf("ordinary work must not consume Oracle rule source revision: %+v", pack.SourceRevisions)
	}
}

func TestCompileWithRuntimeIncludesOracleSecondOpinionOffer(t *testing.T) {
	home := t.TempDir()
	repo := writeOracleSecondOpinionRuleFixture(t)
	in := Input{
		Harness:     "codex",
		Cwd:         repo,
		Task:        "redesign hook spine ownership boundary; ask oracle for second opinion",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := ec.Validate(); err != nil {
		t.Fatal(err)
	}
	offer := findEffectiveContractCapabilityOffer(ec, oracleSecondOpinionCapabilityRef)
	if offer == nil {
		t.Fatalf("effective contract should carry Oracle second-opinion offer: %+v", ec.CapabilityOffers)
	}
	if !hasOfferConstraint(offer.Required, "action", "compound://runtime/oracle-second-opinion-evidence") ||
		!hasOfferConstraint(offer.Required, "writeback", "compound://source-observe/second-opinion-evidence") ||
		!hasOfferConstraint(offer.Required, "proof_gate", "compound://proof/local-state-delta-after-oracle") {
		t.Fatalf("effective contract Oracle offer should carry action, writeback, and proof constraints: %+v", offer.Required)
	}
	if rev := sourceRev(ec.SourceRevisions, oracleSecondOpinionRuleRef); !strings.HasPrefix(rev, "sha256:") {
		t.Fatalf("effective contract missing Oracle rule source revision: %+v", ec.SourceRevisions)
	}
}

func TestPackWithCapabilitiesIncludesExternalRepoIntakeMatches(t *testing.T) {
	home := t.TempDir()
	batchDir := filepath.Join(home, "inbox", "external-repo-intake", "2026-07-01-test")
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(batchDir, "external-repo-intake-map.json")
	if err := os.WriteFile(mapPath, []byte(`{
  "boundaries": ["review-only", "no install"],
  "items": [
    {
      "repo": "affaan-m/ECC",
      "owner": "Compound architecture / hooks / memory / skill graph",
      "next_review_step": "review one hook or MCP pattern against Compound adapters",
      "not_to_do": "do not import ECC wholesale or create a parallel operating system"
    }
  ]
}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summaryPath := filepath.Join(batchDir, "run-summary.json")
	sourceObservationPath := filepath.Join(batchDir, "source-observation.json")
	if err := os.WriteFile(summaryPath, []byte(`{
  "schema_version": "compound.external_repo_intake_run.v0.1",
  "id": "2026-07-01-test",
  "created_at": "2026-07-01T12:00:00Z",
  "map_path": "`+filepath.ToSlash(mapPath)+`",
  "source_observation_path": "`+filepath.ToSlash(sourceObservationPath)+`",
  "candidate_count": 1,
  "counts_by_decision": {"keep_needs_review": 1},
  "counts_by_lane": {"harness_operating_system_reference": 1},
  "decisions": [
    {
      "repo": "affaan-m/ECC",
      "decision": "keep_needs_review",
      "lane": "harness_operating_system_reference",
      "status": "open",
      "evidence_strength": 0.83
    }
  ]
}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	in := mkInput("control-plane")
	in.Task = "Should we adopt https://github.com/affaan-m/ECC hooks and MCP plugins?"
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("pack invalid: %v", err)
	}
	ref, ok := findIncludedRef(pack, externalRepoIntakeRef("affaan-m/ECC"))
	if !ok {
		t.Fatalf("pack should include external repo intake match: %+v", pack.Included)
	}
	if ref.SourceClass != "generated" || ref.Status != "current" || ref.Confidence != 0.83 {
		t.Fatalf("external intake ref should stay generated/current evidence, got %+v", ref)
	}
	for _, want := range []string{
		"external_repo_intake repo=affaan-m/ECC decision=keep_needs_review",
		"owner=Compound architecture / hooks / memory / skill graph",
		"next_review_step=review one hook or MCP pattern against Compound adapters",
		"boundary=do not import ECC wholesale or create a parallel operating system",
	} {
		if !strings.Contains(ref.Reason, want) {
			t.Fatalf("external intake reason missing %q: %s", want, ref.Reason)
		}
	}
	if !strings.Contains(pack.SummaryMD, "1 external repo intake match(es) included") {
		t.Fatalf("summary missing external intake count: %s", pack.SummaryMD)
	}

	in.Task = "evaluate hooks memory skill graph architecture pattern for Compound from external operating-system reference"
	contextual, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	contextualRef, ok := findIncludedRef(contextual, externalRepoIntakeRef("affaan-m/ECC"))
	if !ok {
		t.Fatalf("contextual task should include external repo intake evidence: %+v", contextual.Included)
	}
	if !strings.Contains(contextualRef.Reason, "owner=Compound architecture / hooks / memory / skill graph") {
		t.Fatalf("contextual external repo reason should preserve owner metadata: %s", contextualRef.Reason)
	}

	in.Task = "Should we adopt a repository that has not been reviewed?"
	unmatched, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findIncludedRef(unmatched, externalRepoIntakeRef("affaan-m/ECC")); ok {
		t.Fatalf("unmatched task should not include ECC intake ref: %+v", unmatched.Included)
	}
}

func findIncludedRef(pack envelope.ContextPack, ref string) (envelope.IncludedRef, bool) {
	for _, included := range pack.Included {
		if included.Ref == ref {
			return included, true
		}
	}
	return envelope.IncludedRef{}, false
}

// Encodes the personal-corpus-not-ambient gate: a product repo forbids self-model /
// personal-corpus memory from being ambient; the control-plane allows self-model only
// on explicit request.
func TestPersonalCorpusGate(t *testing.T) {
	pol := DefaultPolicy()
	prod := Compile(mkInput("product"), pol, fixedNow)
	if !contains(prod.MemoryPolicy.Forbidden, "personal-corpus") || !contains(prod.MemoryPolicy.Forbidden, "self-model") {
		t.Errorf("product contract must forbid personal-corpus and self-model ambient; got %v", prod.MemoryPolicy.Forbidden)
	}
	ctl := Compile(mkInput("control-plane"), pol, fixedNow)
	if contains(ctl.MemoryPolicy.Ambient, "self-model") {
		t.Errorf("control-plane must not make self-model ambient; got ambient=%v", ctl.MemoryPolicy.Ambient)
	}
	if !contains(ctl.MemoryPolicy.ExplicitOnly, "self-model") {
		t.Errorf("control-plane should allow self-model only on explicit request; got explicit_only=%v", ctl.MemoryPolicy.ExplicitOnly)
	}
	if !contains(ctl.MemoryPolicy.ExplicitOnly, "personal-corpus") || contains(ctl.MemoryPolicy.Forbidden, "personal-corpus") {
		t.Errorf("control-plane must make personal-corpus explicit-only, not forbidden; explicit_only=%v forbidden=%v", ctl.MemoryPolicy.ExplicitOnly, ctl.MemoryPolicy.Forbidden)
	}
}

// Repo kind is discovered from a committed marker (path-independent), walking up from a
// nested cwd; an explicit flag overrides; absent marker defaults to product.
func TestResolveRepoKind(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".compound", "repo.yaml"), []byte("kind: control-plane\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	if k, via := ResolveRepoKind("", nested); k != "control-plane" {
		t.Errorf("nested cwd should detect control-plane from marker, got %q (via %s)", k, via)
	}
	if k, via := ResolveRepoKind("product", nested); k != "product" || via != "flag" {
		t.Errorf("explicit flag should win, got %q via %s", k, via)
	}
	if k, via := ResolveRepoKind("", t.TempDir()); k != "product" || via != "default" {
		t.Errorf("no marker should default to product/default, got %q via %s", k, via)
	}
}

func TestResolveRepoKindUsesRuntimeRegistry(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	controlRoot := t.TempDir()
	controlNested := filepath.Join(controlRoot, "example-os", "ops")
	if err := os.MkdirAll(controlNested, 0o755); err != nil {
		t.Fatal(err)
	}
	productRoot := t.TempDir()
	productNested := filepath.Join(productRoot, "app")
	if err := os.MkdirAll(productNested, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := []byte("schema_version: " + repoKindRegistrySchema + "\nrepos:\n  - kind: control-plane\n    roots:\n      - " + controlRoot + "\n  - kind: product\n    roots:\n      - " + productRoot + "\n")
	if err := os.WriteFile(filepath.Join(home, "config", "repo-kinds.yaml"), registry, 0o644); err != nil {
		t.Fatal(err)
	}

	kind, via, err := ResolveRepoKindWithHome(home, "", controlNested)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "control-plane" || !strings.HasPrefix(via, "repo-kind-registry:") {
		t.Fatalf("control kind = %q via %q", kind, via)
	}
	kind, _, err = ResolveRepoKindWithHome(home, "", productNested)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "product" {
		t.Fatalf("product kind = %q", kind)
	}
	kind, via, err = ResolveRepoKindWithHome(home, "library", controlNested)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "library" || via != "flag" {
		t.Fatalf("flag should win, kind=%q via=%q", kind, via)
	}
}

func TestResolveRepoKindMarkerWinsOverRuntimeRegistry(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".compound", "repo.yaml"), []byte("kind: library\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "repo-kinds.yaml"), []byte("schema_version: "+repoKindRegistrySchema+"\nrepos:\n  - kind: control-plane\n    roots:\n      - "+root+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	kind, via, err := ResolveRepoKindWithHome(home, "", root)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "library" || !strings.HasSuffix(via, filepath.Join(".compound", "repo.yaml")) {
		t.Fatalf("marker should win, kind=%q via=%q", kind, via)
	}
}

// Defaulting the repo kind is recorded as an unknown and softens uncertainty to inferred —
// the contract never silently pretends it knew.
func TestDefaultedRepoKindRecorded(t *testing.T) {
	in := Input{Harness: "codex", Cwd: "/somewhere", Task: "x", RepoKind: "product", RepoKindVia: "default"}
	ec := Compile(in, DefaultPolicy(), fixedNow)
	if len(ec.Unknowns) == 0 {
		t.Error("defaulted repo kind should be recorded in unknowns")
	}
	if ec.Uncertainty.Level != "inferred" {
		t.Errorf("defaulted repo kind should set uncertainty=inferred, got %q", ec.Uncertainty.Level)
	}
}

func TestFixtureReposProveOrientationScope(t *testing.T) {
	root := repoRoot(t)
	productCwd := filepath.Join(root, "fixtures", "orientation", "product-repo", "app")
	controlCwd := filepath.Join(root, "fixtures", "orientation", "control-plane-repo", "ops")

	productKind, productVia := ResolveRepoKind("", productCwd)
	controlKind, controlVia := ResolveRepoKind("", controlCwd)
	if productKind != "product" || controlKind != "control-plane" {
		t.Fatalf("fixture kinds product=%q control=%q", productKind, controlKind)
	}
	if productVia == "default" || controlVia == "default" {
		t.Fatalf("fixture markers should be discovered, product via=%q control via=%q", productVia, controlVia)
	}

	pol := DefaultPolicy()
	product := Pack(Input{Harness: "codex", Cwd: productCwd, Task: "ship onboarding copy", RepoKind: productKind, RepoKindVia: productVia, Mode: "execute"}, pol, fixedNow)
	control := Pack(Input{Harness: "codex", Cwd: controlCwd, Task: "audit hooks", RepoKind: controlKind, RepoKindVia: controlVia, Mode: "execute"}, pol, fixedNow)

	if hasIncluded(product, "control-plane://workflow-contract") {
		t.Fatal("product repo must not include control-plane workflow contract ambiently")
	}
	if !hasExcluded(product, "control-plane://workflow-contract") {
		t.Fatal("product repo must record control-plane workflow contract as excluded")
	}
	if !hasIncluded(control, "control-plane://workflow-contract") {
		t.Fatal("control-plane repo must include workflow contract")
	}
	if hasExcluded(control, "control-plane://workflow-contract") {
		t.Fatal("control-plane repo should not exclude its workflow contract")
	}
}

func TestPackWithCapabilitiesDoesNotCreateStateWhenGraphMissing(t *testing.T) {
	home := t.TempDir()
	pack, err := PackWithCapabilities(mkInput("product"), DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatal(err)
	}
	if hasIncluded(pack, "capability://skill:pricing") {
		t.Fatal("missing graph should not surface capability refs")
	}
	if _, err := os.Stat(filepath.Join(home, "registries")); !os.IsNotExist(err) {
		t.Fatalf("read-only orientation should not create registries dir, err=%v", err)
	}
}

func TestPackWithCapabilitiesSurfacesGraphOffers(t *testing.T) {
	home := t.TempDir()
	manifest := capgraph.SkillManifest{
		SchemaVersion: capgraph.SkillManifestSchemaVersion,
		ID:            "skill_manifest_fixture",
		AdapterID:     "fixture.skill-manifest",
		SourceSystem:  "fixture-os",
		ObservedAt:    "2026-06-29T00:00:00Z",
		SourceRef: capgraph.SkillSourceRef{
			Path:        "fixtures/skills/skill-manifest.yaml",
			Kind:        "generated_view",
			Owner:       "capability-fixture",
			SourceClass: "generated",
			Revision:    "rev_fixture",
		},
		EvidenceRefs: []string{"artifact_sha256:skill-fixture"},
		Skills: []capgraph.SkillManifestEntry{
			{
				ID:                  "skill:pricing",
				Name:                "Pricing",
				Status:              "active",
				Triggers:            []string{"pricing", "paywall"},
				Capabilities:        []string{"monetization"},
				CompatibleHarnesses: []string{"claude_code"},
				Requires:            []string{"tool:revenuecat"},
				ConflictsWith:       []string{"skill:old-paywall"},
				ValidatedBy:         []string{"eval:pricing-copy"},
				Weight:              2,
			},
			{
				ID:       "skill:old-paywall",
				Name:     "Old Paywall",
				Status:   "retired",
				Triggers: []string{"paywall"},
			},
		},
	}
	if _, err := capgraph.ApplySkillManifest(home, manifest, fixedNow); err != nil {
		t.Fatal(err)
	}
	pack, err := PackWithCapabilities(mkInput("product"), DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatal(err)
	}
	if sourceRev(pack.SourceRevisions, capgraph.CapabilityGraphAuthorityRef) == "" {
		t.Fatalf("context pack missing capability graph source revision: %+v", pack.SourceRevisions)
	}
	included, ok := findIncludedRef(pack, "capability://skill:pricing")
	if !ok {
		t.Fatalf("context pack should include surfaced skill offer; included=%v", pack.Included)
	}
	if !containsText(included.Reason, "matched=paywall") {
		t.Fatalf("included reason should explain matched trigger tokens: %q", included.Reason)
	}
	offer := findCapabilityOffer(pack, "capability://skill:pricing")
	if offer == nil {
		t.Fatalf("context pack should include structured capability offer; offers=%v", pack.CapabilityOffers)
	}
	if offer.Kind != "skill" || offer.Name != "Pricing" || offer.Score <= 0 {
		t.Fatalf("unexpected capability offer: %+v", offer)
	}
	if !hasOfferConstraint(offer.Required, capgraph.EdRequires, "tool:revenuecat") {
		t.Fatalf("required constraints = %+v", offer.Required)
	}
	if !hasOfferConstraint(offer.Conflicts, capgraph.EdConflictsWith, "skill:old-paywall") {
		t.Fatalf("conflict constraints = %+v", offer.Conflicts)
	}
	if !hasOfferConstraint(offer.Harnesses, capgraph.EdSurfacesWith, "harness:claude_code") {
		t.Fatalf("harness constraints = %+v", offer.Harnesses)
	}
	if !hasOfferConstraint(offer.Evidence, capgraph.EdValidatedBy, "eval:pricing-copy") {
		t.Fatalf("evidence constraints = %+v", offer.Evidence)
	}
	if offer.CostRiskNote == "" || !containsText(offer.CostRiskNote, "offer only") {
		t.Fatalf("offer should carry execution boundary note: %+v", offer)
	}
	if hasIncluded(pack, "capability://skill:old-paywall") {
		t.Fatalf("retired capability must not be included; included=%v", pack.Included)
	}
	if pack.SummaryMD == "" || !containsText(pack.SummaryMD, "capability offer(s) surfaced") {
		t.Fatalf("summary should report surfaced offers, got %q", pack.SummaryMD)
	}
	ec, err := CompileWithRuntime(mkInput("product"), DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := ec.Validate(); err != nil {
		t.Fatal(err)
	}
	contractOffer := findEffectiveContractCapabilityOffer(ec, "capability://skill:pricing")
	if contractOffer == nil {
		t.Fatalf("effective contract capability offers = %+v, want skill:pricing", ec.CapabilityOffers)
	}
	if contractOffer.CostRiskNote == "" || !hasOfferConstraint(contractOffer.Required, capgraph.EdRequires, "tool:revenuecat") {
		t.Fatalf("effective contract offer lost constraints: %+v", contractOffer)
	}
	if sourceRev(ec.SourceRevisions, capgraph.CapabilityGraphAuthorityRef) == "" {
		t.Fatalf("effective contract missing capability graph source revision: %+v", ec.SourceRevisions)
	}
}

func TestPackWithCapabilitiesBoundsCapabilityOffers(t *testing.T) {
	home := t.TempDir()
	total := maxContextCapabilityOffers + 3
	skills := make([]capgraph.SkillManifestEntry, 0, total)
	for i := 0; i < total; i++ {
		skills = append(skills, capgraph.SkillManifestEntry{
			ID:       fmt.Sprintf("skill:budget-%02d", i),
			Name:     fmt.Sprintf("Budget %02d", i),
			Status:   "active",
			Triggers: []string{"budgeted"},
			Weight:   float64(total - i),
		})
	}
	manifest := capgraph.SkillManifest{
		SchemaVersion: capgraph.SkillManifestSchemaVersion,
		ID:            "skill_manifest_budget_fixture",
		AdapterID:     "fixture.skill-manifest",
		SourceSystem:  "fixture-os",
		ObservedAt:    "2026-06-29T00:00:00Z",
		SourceRef: capgraph.SkillSourceRef{
			Path:        "fixtures/skills/budgeted-skill-manifest.yaml",
			Kind:        "generated_view",
			Owner:       "capability-fixture",
			SourceClass: "generated",
			Revision:    "rev_budget_fixture",
		},
		EvidenceRefs: []string{"artifact_sha256:skill-budget-fixture"},
		Skills:       skills,
	}
	if _, err := capgraph.ApplySkillManifest(home, manifest, fixedNow); err != nil {
		t.Fatal(err)
	}
	in := mkInput("product")
	in.Harness = "codex"
	in.Task = "budgeted capability review"
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := len(pack.CapabilityOffers); got != maxContextCapabilityOffers {
		t.Fatalf("capability offers = %d, want %d", got, maxContextCapabilityOffers)
	}
	if got := countCapabilityIncluded(pack); got != maxContextCapabilityOffers {
		t.Fatalf("capability included refs = %d, want %d", got, maxContextCapabilityOffers)
	}
	if findCapabilityOffer(pack, "capability://skill:budget-00") == nil || !hasIncluded(pack, "capability://skill:budget-00") {
		t.Fatalf("highest-ranked offer should be retained; offers=%v included=%v", pack.CapabilityOffers, pack.Included)
	}
	if findCapabilityOffer(pack, "capability://skill:budget-14") != nil || hasIncluded(pack, "capability://skill:budget-14") {
		t.Fatalf("lowest-ranked offer should be omitted from context budget; offers=%v included=%v", pack.CapabilityOffers, pack.Included)
	}
	if !containsText(pack.SummaryMD, "12 of 15 capability offer(s) surfaced") || !containsText(pack.SummaryMD, "3 omitted from context budget") {
		t.Fatalf("summary should report context budget omission, got %q", pack.SummaryMD)
	}
}

func TestPackWithCapabilitiesFiltersByHarnessCompatibility(t *testing.T) {
	home := t.TempDir()
	manifest := capgraph.SkillManifest{
		SchemaVersion: capgraph.SkillManifestSchemaVersion,
		ID:            "skill_manifest_fixture",
		AdapterID:     "fixture.skill-manifest",
		SourceSystem:  "fixture-os",
		ObservedAt:    "2026-06-29T00:00:00Z",
		SourceRef: capgraph.SkillSourceRef{
			Path:        "fixtures/skills/skill-manifest.yaml",
			Kind:        "generated_view",
			Owner:       "capability-fixture",
			SourceClass: "generated",
			Revision:    "rev_fixture",
		},
		EvidenceRefs: []string{"artifact_sha256:skill-fixture"},
		Skills: []capgraph.SkillManifestEntry{
			{
				ID:                  "skill:codex-pricing",
				Name:                "Codex Pricing",
				Status:              "active",
				Triggers:            []string{"pricing"},
				CompatibleHarnesses: []string{"codex"},
			},
			{
				ID:                  "skill:claude-pricing",
				Name:                "Claude Pricing",
				Status:              "active",
				Triggers:            []string{"pricing"},
				CompatibleHarnesses: []string{"claude_code"},
			},
			{
				ID:       "skill:generic-pricing",
				Name:     "Generic Pricing",
				Status:   "active",
				Triggers: []string{"pricing"},
			},
		},
	}
	if _, err := capgraph.ApplySkillManifest(home, manifest, fixedNow); err != nil {
		t.Fatal(err)
	}
	codexIn := mkInput("product")
	codexIn.Harness = "codex"
	codexIn.Task = "improve pricing"
	codexPack, err := PackWithCapabilities(codexIn, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !hasIncluded(codexPack, "capability://skill:codex-pricing") || !hasIncluded(codexPack, "capability://skill:generic-pricing") {
		t.Fatalf("codex pack missing codex/generic offers: %+v", codexPack.Included)
	}
	if hasIncluded(codexPack, "capability://skill:claude-pricing") {
		t.Fatalf("codex pack must not include claude-only offer: %+v", codexPack.Included)
	}
	claudeIn := mkInput("product")
	claudeIn.Harness = "claude_code"
	claudeIn.Task = "improve pricing"
	claudePack, err := PackWithCapabilities(claudeIn, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !hasIncluded(claudePack, "capability://skill:claude-pricing") || !hasIncluded(claudePack, "capability://skill:generic-pricing") {
		t.Fatalf("claude pack missing claude/generic offers: %+v", claudePack.Included)
	}
	if hasIncluded(claudePack, "capability://skill:codex-pricing") {
		t.Fatalf("claude pack must not include codex-only offer: %+v", claudePack.Included)
	}
}

func TestPackWithCapabilitiesSurfacesMatchedInstructionRulesOnly(t *testing.T) {
	home := t.TempDir()
	manifest, err := capgraph.LoadSkillManifest(filepath.Join("..", "..", "fixtures", "skills", "instruction-rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.ApplySkillManifest(home, manifest, fixedNow); err != nil {
		t.Fatal(err)
	}

	matched := Input{Harness: "codex", Cwd: t.TempDir(), Task: "fix memory recall adjudication", RepoKind: "control-plane", RepoKindVia: "fixture", Mode: "execute"}
	matchedPack, err := PackWithCapabilities(matched, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := matchedPack.Validate(); err != nil {
		t.Fatal(err)
	}
	offer := findCapabilityOffer(matchedPack, "capability://instruction_rule:memory-recall-adjudication")
	if offer == nil {
		t.Fatalf("matched ContextPack missing InstructionRule offer; offers=%+v", matchedPack.CapabilityOffers)
	}
	if offer.Kind != "instruction_rule" || !hasIncluded(matchedPack, "capability://instruction_rule:memory-recall-adjudication") {
		t.Fatalf("instruction rule should surface like capability offer, offer=%+v included=%+v", offer, matchedPack.Included)
	}
	if !hasOfferConstraint(offer.Required, "read", "docs/architecture/contracts/README.md") {
		t.Fatalf("instruction rule missing read ref constraint: %+v", offer.Required)
	}
	if !hasOfferConstraint(offer.Required, "route", "learning") {
		t.Fatalf("instruction rule missing route constraint: %+v", offer.Required)
	}
	if !offerConstraintsContainDetail(offer.Conflicts, "no promoted claim mutation without adjudication") {
		t.Fatalf("instruction rule missing boundary constraint: %+v", offer.Conflicts)
	}
	if !containsText(offer.CostRiskNote, "never auto-executes") {
		t.Fatalf("instruction rule must carry offer-only boundary note: %+v", offer)
	}

	unmatched := Input{Harness: "codex", Cwd: t.TempDir(), Task: "update pricing copy", RepoKind: "product", RepoKindVia: "fixture", Mode: "execute"}
	unmatchedPack, err := PackWithCapabilities(unmatched, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if findCapabilityOffer(unmatchedPack, "capability://instruction_rule:memory-recall-adjudication") != nil || hasIncluded(unmatchedPack, "capability://instruction_rule:memory-recall-adjudication") {
		t.Fatalf("unmatched ContextPack must not get memory InstructionRule; offers=%+v included=%+v", unmatchedPack.CapabilityOffers, unmatchedPack.Included)
	}
}
func TestRuntimeContractIncludesAcceptedDecisionAndRealSourceRevisions(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("# Repo Instructions\nUse Compound context.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	brainDecision := filepath.Join(t.TempDir(), "brain-decision.md")
	if err := os.WriteFile(brainDecision, []byte(`---
id: compound-context-pack-decision
title: Compound ContextPack owns decision context
kind: doctrine
status: accepted
falsifier: Agents read the old brain file directly instead of the ContextPack.
review_trigger: Revisit when ContextPack stops including the decision ref.
owner: compound
scope: public-core
---
# Compound ContextPack owns decision context
Future agents consume this decision through Compound runtime context.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := decisions.ImportBrainDecision(store, brainDecision, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(t.TempDir(), "RecallContext.hook.ts")
	if err := os.WriteFile(hookPath, []byte("console.log('hook')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema_version: compound.installed_hooks.v0.1\nhooks:\n - harness: codex\n   name: SessionStart\n   ref: " + hookPath + "\n   status: current\n"
	if err := os.WriteFile(filepath.Join(home, "config", "installed-hooks.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Input{Harness: "codex", Cwd: repo, Task: "use imported decision", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	decisionRef := "decision://" + report.Lifecycle.ID
	if !contains(ec.ActiveDecisions, decisionRef) {
		t.Fatalf("effective contract active decisions = %v, want %s", ec.ActiveDecisions, decisionRef)
	}
	if !contains(ec.ActiveHooks, "hook://codex/SessionStart") {
		t.Fatalf("effective contract active hooks = %v", ec.ActiveHooks)
	}
	if sourceRev(ec.SourceRevisions, "AGENTS.md") == "unknown" {
		t.Fatalf("AGENTS.md source revision was not resolved: %+v", ec.SourceRevisions)
	}
	if sourceRev(ec.SourceRevisions, "hook://codex/SessionStart") == "unknown" {
		t.Fatalf("hook source revision was not resolved: %+v", ec.SourceRevisions)
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(pack.ActiveDecisions, decisionRef) {
		t.Fatalf("context pack active decisions = %v, want %s", pack.ActiveDecisions, decisionRef)
	}
	if !hasIncluded(pack, decisionRef) {
		t.Fatalf("context pack should include decision ref %s; included=%v", decisionRef, pack.Included)
	}
}

func TestRuntimeContractResolvesControlPlaneSourceRevisions(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	files := map[string]string{
		"AGENTS.md":                             "# Repo Instructions\nUse Compound context.\n",
		"docs/ARCHITECTURE.md":                  "# Architecture\nRead LAYERS first.\n",
		"docs/LAYERS.md":                        "# Layers\nEnvelope spine.\n",
		"docs/architecture/contracts/README.md": "# Contracts\nContext and work contracts.\n",
		"docs/architecture/runtime.md":          "# Runtime\nCOMPOUND_HOME and envelope spine.\n",
		"docs/roadmap/phases.md":                "# Phases\nEnvelope-first roadmap.\n",
	}
	for rel, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	in := Input{Harness: "codex", Cwd: repo, Task: "compile control-plane source refs", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{
		"control-plane://orientation-map",
		"control-plane://workflow-contract",
		"control-plane://sop-packets",
	} {
		if !contains(ec.ActiveInstructions, source) {
			t.Fatalf("effective contract active instructions missing %s: %v", source, ec.ActiveInstructions)
		}
		rev := sourceRev(ec.SourceRevisions, source)
		if rev == "" || rev == "unknown" || !strings.HasPrefix(rev, "sha256:") {
			t.Fatalf("source revision for %s = %q, want sha256 digest in %+v", source, rev, ec.SourceRevisions)
		}
		packRev := sourceRev(pack.SourceRevisions, source)
		if packRev == "" || packRev == "unknown" || !strings.HasPrefix(packRev, "sha256:") {
			t.Fatalf("context pack source revision for %s = %q, want sha256 digest in %+v", source, packRev, pack.SourceRevisions)
		}
	}
}

func TestImportCodexHooksJSONFeedsRuntimeContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join("..", "..", "fixtures", "harness", "codex-hooks-json.json")
	if err := StageSyntheticHookHandlers(home, source); err != nil {
		t.Fatalf("stage synthetic hook handlers: %v", err)
	}
	report, err := ImportCodexHooksJSON(home, source, fixedNow)
	if err != nil {
		t.Fatalf("import codex hooks json: %v", err)
	}
	if report.AuthorityRef != InstalledHooksAuthorityRef || report.HookCount != 3 {
		t.Fatalf("unexpected import report: %+v", report)
	}
	if len(report.Harnesses) != 1 || report.Harnesses[0] != "codex" {
		t.Fatalf("import harnesses = %+v", report.Harnesses)
	}
	manifest, err := LoadInstalledHooksManifest(home)
	if err != nil {
		t.Fatalf("load installed hooks manifest: %v", err)
	}
	if len(manifest.Hooks) != 3 {
		t.Fatalf("installed hooks = %+v", manifest.Hooks)
	}
	if !installedHookHasMatcher(manifest, "codex", "PreToolUse", "*") {
		t.Fatalf("Codex hook import lost the native PreToolUse matcher: %+v", manifest.Hooks)
	}
	gate := findInstalledHook(manifest, "codex", "PreToolUse")
	if gate == nil || !strings.HasSuffix(gate.Ref, "/.local/bin/compound") || !strings.HasPrefix(gate.InvocationRevision, "sha256:") {
		t.Fatalf("Codex hook import lost the handler display ref or privacy-safe invocation identity: %+v", gate)
	}
	if gate.Status != "non_blocking" {
		t.Fatalf("observation-only Codex PreToolUse status = %q, want non_blocking", gate.Status)
	}
	readiness := findHookGateReadiness(InstalledHookGateReadiness(manifest, map[string]bool{"codex": true}), "codex")
	if readiness == nil || readiness.Status != "non_blocking" || readiness.Installed || readiness.AdapterRequired {
		t.Fatalf("observation-only Codex readiness = %+v, want non_blocking without an installed gate", readiness)
	}
	plan, err := hookPlanFromInstalledManifest(manifest, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range plan.Bindings {
		if binding.Harness == "codex" && binding.Event == "PreToolUse" && binding.Blocking {
			t.Fatalf("observation-only Codex PreToolUse projected as blocking: %+v", binding)
		}
	}
	records, err := LoadInstalledHooksSourceAuthority(home)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("source authority records = %d, want 1", len(records))
	}
	if records[0].AuthorityStatus != "compound_primary" || records[0].SourceRole != "evidence_only_input_after_import" {
		t.Fatalf("bad source authority: %+v", records[0])
	}
	in := Input{Harness: "codex", Cwd: t.TempDir(), Task: "use hooks", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hook://codex/PreToolUse", "hook://codex/SessionStart", "hook://codex/UserPromptSubmit"} {
		if !contains(ec.ActiveHooks, want) {
			t.Fatalf("effective contract active hooks = %v, want %s", ec.ActiveHooks, want)
		}
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hook://codex/PreToolUse", "hook://codex/SessionStart", "hook://codex/UserPromptSubmit"} {
		if !contains(pack.ActiveHooks, want) {
			t.Fatalf("context pack active hooks = %v, want %s", pack.ActiveHooks, want)
		}
	}
	if sourceRev(ec.SourceRevisions, InstalledHooksAuthorityRef) == "" {
		t.Fatalf("effective contract missing installed-hooks source revision: %+v", ec.SourceRevisions)
	}
}

func TestImportCodexHooksJSONPreservesExplicitPreToolUseEnforcement(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binaryPath := filepath.Join(home, ".local", "bin", "compound")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("synthetic compound binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "hooks.json")
	data := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"$HOME/.local/bin/compound hook pre-tool-use --live --harness codex --enforce"}]}]}}`
	if err := os.WriteFile(source, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportCodexHooksJSON(home, source, fixedNow); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadInstalledHooksManifest(home)
	if err != nil {
		t.Fatal(err)
	}
	gate := findInstalledHook(manifest, "codex", "PreToolUse")
	if gate == nil || gate.Status != "current" {
		t.Fatalf("explicitly enforced Codex PreToolUse = %+v, want current blocking authority", gate)
	}
	readiness := findHookGateReadiness(InstalledHookGateReadiness(manifest, map[string]bool{"codex": true}), "codex")
	if readiness == nil || readiness.Status != "native_blocking" || !readiness.Installed {
		t.Fatalf("explicitly enforced Codex readiness = %+v, want native_blocking", readiness)
	}
	plan, err := hookPlanFromInstalledManifest(manifest, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range plan.Bindings {
		if binding.Harness == "codex" && binding.Event == "PreToolUse" {
			if !binding.Blocking {
				t.Fatalf("explicitly enforced Codex PreToolUse projected non-blocking: %+v", binding)
			}
			return
		}
	}
	t.Fatalf("explicitly enforced Codex plan omitted PreToolUse: %+v", plan.Bindings)
}

func TestInstalledHookStatusInferenceOnlyDowngradesExactCompoundObserver(t *testing.T) {
	for _, tc := range []struct {
		name     string
		command  string
		explicit string
		want     string
	}{
		{
			name:    "managed observer",
			command: "$HOME/.local/bin/compound hook pre-tool-use --live --harness codex",
			want:    "non_blocking",
		},
		{
			name:    "managed enforcement",
			command: "$HOME/.local/bin/compound hook pre-tool-use --live --harness codex --enforce",
			want:    "current",
		},
		{
			name:    "external hook",
			command: "$HOME/.codex/hooks/PlacementGuard.hook.ts --live",
			want:    "current",
		},
		{
			name:    "unrelated compound arguments",
			command: "wrapper compound --label pre-tool-use --live",
			want:    "current",
		},
		{
			name:    "wrapper receives compound observer argv",
			command: "wrapper compound hook pre-tool-use --live --harness codex",
			want:    "current",
		},
		{
			name:     "explicit source status wins",
			command:  "$HOME/.local/bin/compound hook pre-tool-use --live --harness codex",
			explicit: "current",
			want:     "current",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := installedHookStatusForCommand("PreToolUse", tc.command, tc.explicit); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstalledHooksImportProvenanceTracksResultingAuthorityAcrossPreservedReplay(t *testing.T) {
	home := t.TempDir()
	importedVia := installedHooksCodexImportedVia
	sourceA := filepath.Join(t.TempDir(), "hooks-a.json")
	sourceB := filepath.Join(t.TempDir(), "hooks-b.json")
	dataA := []byte(`{"source":"A"}`)
	dataB := []byte(`{"source":"preserved-B"}`)
	manifestA := InstalledHooksManifest{Hooks: []InstalledHook{
		{Harness: "codex", Name: "SessionStart", Event: "SessionStart", Ref: "handler:a", Status: "current"},
	}}
	manifestB := InstalledHooksManifest{Hooks: []InstalledHook{
		{Harness: "claude_code", Name: "SessionStart", Event: "SessionStart", Ref: "handler:preserved-b", Status: "preserved_external"},
	}}

	first, err := importInstalledHooks(home, sourceA, dataA, manifestA, importedVia, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	firstAuthorityBytes, err := os.ReadFile(InstalledHooksPath(home))
	if err != nil {
		t.Fatal(err)
	}
	second, err := importInstalledHooks(home, sourceB, dataB, manifestB, importedVia, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	secondAuthorityBytes, err := os.ReadFile(InstalledHooksPath(home))
	if err != nil {
		t.Fatal(err)
	}
	third, err := importInstalledHooks(home, sourceA, dataA, manifestA, importedVia, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	thirdAuthorityBytes, err := os.ReadFile(InstalledHooksPath(home))
	if err != nil {
		t.Fatal(err)
	}

	firstJSON := jsonObject(t, first)
	secondJSON := jsonObject(t, second)
	thirdJSON := jsonObject(t, third)
	firstAuthorityRevision := hashBytes(firstAuthorityBytes)
	secondAuthorityRevision := hashBytes(secondAuthorityBytes)
	thirdAuthorityRevision := hashBytes(thirdAuthorityBytes)
	if first.SourceAuthority.SourceRevision != third.SourceAuthority.SourceRevision {
		t.Fatalf("A replay source revisions differ: first=%q third=%q", first.SourceAuthority.SourceRevision, third.SourceAuthority.SourceRevision)
	}
	if firstJSON["authority_revision"] != firstAuthorityRevision || secondJSON["authority_revision"] != secondAuthorityRevision || thirdJSON["authority_revision"] != thirdAuthorityRevision {
		t.Fatalf("import reports do not identify exact resulting authorities: first=%v second=%v third=%v", firstJSON, secondJSON, thirdJSON)
	}
	if firstAuthorityRevision == thirdAuthorityRevision || first.SourceAuthority.ID == third.SourceAuthority.ID {
		t.Fatalf("A replay collapsed distinct resulting authorities: first=%+v third=%+v", first.SourceAuthority, third.SourceAuthority)
	}
	if thirdJSON["prior_authority_revision"] != secondAuthorityRevision {
		t.Fatalf("third import prior authority = %v, want %q", thirdJSON["prior_authority_revision"], secondAuthorityRevision)
	}
	if thirdJSON["incoming_hook_count"] != float64(1) || third.HookCount != 2 {
		t.Fatalf("third import incoming/resulting counts = %v/%d, want 1/2", thirdJSON["incoming_hook_count"], third.HookCount)
	}
	records, err := LoadInstalledHooksSourceAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("source-authority replay records = %d, want 3: %+v", len(records), records)
	}
	thirdRecordJSON := jsonObject(t, records[2])
	if thirdRecordJSON["authority_revision"] != thirdAuthorityRevision || thirdRecordJSON["prior_authority_revision"] != secondAuthorityRevision || thirdRecordJSON["incoming_hook_count"] != float64(1) {
		t.Fatalf("third source-authority provenance = %v", thirdRecordJSON)
	}
}

func TestInstalledHooksSequentialClaudeCodexThenOpenCodeImportPreservesOtherManagedHarnesses(t *testing.T) {
	home := t.TempDir()
	claudeCodex := InstalledHooksManifest{Hooks: []InstalledHook{
		{Harness: "claude_code", Name: "PreToolUse", Event: "PreToolUse", Matcher: "*", Ref: "handler:claude-managed", Status: "current"},
		{Harness: "codex", Name: "SessionStart", Event: "SessionStart", Ref: "handler:codex-managed", Status: "current"},
		{Harness: "claude_code", Name: "SessionStart", Event: "SessionStart", Ref: "handler:external-peer", Status: "preserved_external"},
	}}
	if _, err := importInstalledHooks(
		home,
		"claude-codex-hook-registration.ts",
		[]byte("claude-codex"),
		claudeCodex,
		installedHooksRegistryImportedVia,
		fixedNow,
	); err != nil {
		t.Fatalf("import Claude/Codex hooks: %v", err)
	}

	opencode := InstalledHooksManifest{Hooks: []InstalledHook{
		{Harness: "opencode", Name: "tool.execute.before", Event: "tool.execute.before", Matcher: "*", Ref: "handler:opencode-managed", Status: "current"},
	}}
	report, err := importInstalledHooks(
		home,
		"opencode-hook-registration.ts",
		[]byte("opencode"),
		opencode,
		installedHooksRegistryImportedVia,
		fixedNow.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("import OpenCode hooks: %v", err)
	}

	if report.HookCount != 4 {
		t.Fatalf("OpenCode import resulting hook count = %d, want 4: %+v", report.HookCount, report.Manifest.Hooks)
	}
	for _, want := range []struct {
		harness string
		name    string
	}{
		{harness: "claude_code", name: "PreToolUse"},
		{harness: "codex", name: "SessionStart"},
		{harness: "opencode", name: "tool.execute.before"},
	} {
		if hook := findInstalledHook(report.Manifest, want.harness, want.name); hook == nil || hook.Status != "current" {
			t.Fatalf("resulting managed hook %s/%s = %+v, want current", want.harness, want.name, hook)
		}
	}
	external := findInstalledHook(report.Manifest, "claude_code", "SessionStart")
	if external == nil || external.Status != "preserved_external" {
		t.Fatalf("resulting preserved external hook = %+v, want preserved_external", external)
	}
}

func TestImportHookRegistrationTSFeedsRuntimeContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join("..", "..", "fixtures", "harness", "hook-registration.ts")
	if err := StageSyntheticHookHandlers(home, source); err != nil {
		t.Fatalf("stage synthetic hook handlers: %v", err)
	}
	report, err := ImportHookRegistrationTS(home, source, fixedNow)
	if err != nil {
		t.Fatalf("import hook registration ts: %v", err)
	}
	if report.AuthorityRef != InstalledHooksAuthorityRef || report.HookCount != 7 {
		t.Fatalf("unexpected import report: %+v", report)
	}
	if len(report.Harnesses) != 2 || report.Harnesses[0] != "claude_code" || report.Harnesses[1] != "codex" {
		t.Fatalf("import harnesses = %+v", report.Harnesses)
	}
	records, err := LoadInstalledHooksSourceAuthority(home)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("source authority records = %d, want 1", len(records))
	}
	if records[0].ImportedVia != installedHooksRegistryImportedVia {
		t.Fatalf("imported_via = %q, want %q", records[0].ImportedVia, installedHooksRegistryImportedVia)
	}
	manifest, err := LoadInstalledHooksManifest(home)
	if err != nil {
		t.Fatalf("load installed hooks manifest: %v", err)
	}
	if !installedHookHasMatcher(manifest, "claude_code", "PreToolUse", "*") {
		t.Fatalf("imported hook registration should preserve PreToolUse matcher: %+v", manifest.Hooks)
	}
	if !installedHookHasMatcher(manifest, "codex", "PreToolUse", "*") {
		t.Fatalf("imported hook registration should preserve Codex PreToolUse matcher: %+v", manifest.Hooks)
	}
	if gate := findInstalledHook(manifest, "codex", "PreToolUse"); gate == nil || !strings.HasSuffix(gate.Ref, "/.local/bin/compound") || !strings.HasPrefix(gate.InvocationRevision, "sha256:") {
		t.Fatalf("imported hook registration should preserve the handler display ref and privacy-safe invocation identity: %+v", gate)
	}

	claudeInput := Input{Harness: "claude_code", Cwd: t.TempDir(), Task: "use hooks", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	ec, err := CompileWithRuntime(claudeInput, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hook://claude_code/PreToolUse", "hook://claude_code/SessionStart", "hook://claude_code/UserPromptSubmit"} {
		if !contains(ec.ActiveHooks, want) {
			t.Fatalf("claude effective contract active hooks = %v, want %s", ec.ActiveHooks, want)
		}
	}
	pack, err := PackWithCapabilities(claudeInput, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(pack.ActiveHooks, "hook://claude_code/SessionStart") {
		t.Fatalf("claude context pack active hooks = %v", pack.ActiveHooks)
	}
	readOnlyInput := Input{Harness: "claude_code", Cwd: t.TempDir(), Task: "audit current hook status", RepoKind: "control-plane", RepoKindVia: "flag"}
	readOnlyEC, err := CompileWithRuntime(readOnlyInput, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(readOnlyEC.ActiveHooks, "hook://claude_code/PreToolUse") {
		t.Fatalf("read-only effective contract must keep universal PreToolUse hook active: %v", readOnlyEC.ActiveHooks)
	}
	readOnlyPack, err := PackWithCapabilities(readOnlyInput, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(readOnlyPack.ActiveHooks, "hook://claude_code/PreToolUse") || hasExcluded(readOnlyPack, "hook://claude_code/PreToolUse") {
		t.Fatalf("read-only context pack must retain universal gate active=%v excluded=%v", readOnlyPack.ActiveHooks, readOnlyPack.Excluded)
	}

	codexInput := Input{Harness: "codex", Cwd: t.TempDir(), Task: "edit source file with hooks", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	codex, err := CompileWithRuntime(codexInput, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(codex.ActiveHooks, "hook://claude_code/PreToolUse") {
		t.Fatalf("codex effective contract leaked claude hook: %v", codex.ActiveHooks)
	}
	if !contains(codex.ActiveHooks, "hook://codex/PreToolUse") {
		t.Fatalf("codex effective contract active hooks = %v, want native PreToolUse", codex.ActiveHooks)
	}
	for _, want := range []string{"hook://codex/SessionStart", "hook://codex/UserPromptSubmit", "hook://codex/Stop"} {
		if !contains(codex.ActiveHooks, want) {
			t.Fatalf("codex effective contract active hooks = %v, want %s", codex.ActiveHooks, want)
		}
	}
}

func TestInstalledHookGateReadinessRequiresWrapperForFailOpenCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join("..", "..", "fixtures", "harness", "hook-registration.ts")
	if err := StageSyntheticHookHandlers(home, source); err != nil {
		t.Fatalf("stage synthetic hook handlers: %v", err)
	}
	if _, err := ImportHookRegistrationTS(home, source, fixedNow); err != nil {
		t.Fatalf("import hook registration ts: %v", err)
	}
	binaryPath := filepath.Join(home, ".local", "bin", "compound")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("synthetic compound binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	enforcedCodexSource := filepath.Join(t.TempDir(), "codex-hooks.json")
	enforcedCodex := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"$HOME/.local/bin/compound hook pre-tool-use --live --harness codex --enforce"}]}]}}`
	if err := os.WriteFile(enforcedCodexSource, []byte(enforcedCodex), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportCodexHooksJSON(home, enforcedCodexSource, fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("import explicit Codex enforcement: %v", err)
	}
	manifest, err := LoadInstalledHooksManifest(home)
	if err != nil {
		t.Fatalf("load installed hooks manifest: %v", err)
	}
	readiness := InstalledHookGateReadiness(manifest, map[string]bool{
		"claude_code": true,
		"codex":       false,
	})
	claude := findHookGateReadiness(readiness, "claude_code")
	if claude == nil || claude.Status != "native_blocking" || !claude.Installed || claude.AdapterRequired {
		t.Fatalf("claude readiness = %+v", claude)
	}
	codex := findHookGateReadiness(readiness, "codex")
	if codex == nil || codex.Status != "wrapper_required" || !codex.AdapterRequired {
		t.Fatalf("codex readiness = %+v", codex)
	}
	if !strings.Contains(codex.AdapterCommand, "hook pre-tool-use --enforce") {
		t.Fatalf("codex adapter command = %q", codex.AdapterCommand)
	}
	proof := findHookGateAction(codex.Actions, "prove_fixture_gate")
	if proof == nil ||
		proof.ExpectedExitCode != 3 ||
		proof.ExpectedStatus != "fail_open_or_blocked" ||
		!strings.Contains(proof.Command, "COMPOUND_HOME=$(mktemp -d)") ||
		!strings.Contains(proof.Command, "fixtures/hooks/pre-tool-use.codex.json") ||
		proof.RequiresOperator {
		t.Fatalf("codex fixture proof action = %+v", proof)
	}
	live := findHookGateAction(codex.Actions, "prove_live_wrapper_gate")
	if live == nil ||
		live.ExpectedStatus != "ok" ||
		!strings.Contains(live.Command, "compound hook pre-tool-use --live --harness codex") ||
		!strings.Contains(live.Proof, "compound.codex_live_hook_response.v0.1") ||
		live.RequiresOperator {
		t.Fatalf("codex live wrapper proof action = %+v", live)
	}
	wrapper := findHookGateAction(codex.Actions, "operator_wrapper_required")
	if wrapper == nil || !wrapper.RequiresOperator || !strings.Contains(wrapper.Proof, "hooks-authority") {
		t.Fatalf("codex wrapper action = %+v", wrapper)
	}
	if len(claude.Actions) != 0 {
		t.Fatalf("native blocking claude gate should not emit action cards: %+v", claude.Actions)
	}
}

func TestInstalledHookRuntimeReadinessAddsMissingAdapterActions(t *testing.T) {
	manifest := InstalledHooksManifest{
		SchemaVersion: installedHooksSchemaVersion,
		Hooks: []InstalledHook{
			{Harness: "codex", Name: "SessionStart", Event: "SessionStart", Ref: "hook://codex/SessionStart", Status: "current"},
			{Harness: "claude_code", Name: "SessionStart", Event: "SessionStart", Ref: "hook://claude_code/SessionStart", Status: "current"},
		},
	}
	rows := InstalledHookRuntimeReadinessRows(manifest, []HookRuntimeSupport{
		{Harness: "codex", Hook: "SessionStart", FixturePresent: true, FixtureRef: "fixtures/hooks/session-start.json"},
		{Harness: "claude_code", Hook: "SessionStart", FixturePresent: true, FixtureRef: "fixtures/hooks/session-start.json", LiveAdapter: true, LiveCommand: "compound hook session-start --live --harness claude-code"},
	})
	codex := contractHookReadiness(rows, "codex", "SessionStart")
	if codex == nil || codex.Status != "missing_live_adapter" {
		t.Fatalf("codex readiness = %+v", codex)
	}
	fixture := findHookGateAction(codex.Actions, "prove_fixture_hook")
	if fixture == nil ||
		fixture.RequiresOperator ||
		!strings.Contains(fixture.Command, "COMPOUND_HOME=$(mktemp -d)") ||
		!strings.Contains(fixture.Command, "compound hook session-start fixtures/hooks/session-start.json") {
		t.Fatalf("codex fixture hook action = %+v", fixture)
	}
	shim := findHookGateAction(codex.Actions, "operator_live_adapter_required")
	if shim == nil ||
		!shim.RequiresOperator ||
		!strings.Contains(shim.Command, "compound hook session-start --live --harness codex") ||
		!strings.Contains(shim.Proof, "compound contract hooks-authority") ||
		!strings.Contains(shim.Rollback, "compound contract hooks-authority") {
		t.Fatalf("codex live adapter action = %+v", shim)
	}
	claude := contractHookReadiness(rows, "claude_code", "SessionStart")
	if claude == nil || claude.Status != "ready" || len(claude.Actions) != 0 {
		t.Fatalf("claude ready row should not emit action cards: %+v", claude)
	}
}

func TestPackWithCapabilitiesRecordsUninstalledNativeWriteGateForMutationTask(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `schema_version: compound.installed_hooks.v0.1
hooks:
  - harness: codex
    name: SessionStart
    event: SessionStart
    ref: /tmp/codex-session-start.hook.ts
    status: current
  - harness: codex
    name: UserPromptSubmit
    event: UserPromptSubmit
    ref: /tmp/codex-user-prompt-submit.hook.ts
    status: current
`
	if err := os.WriteFile(filepath.Join(home, "config", "installed-hooks.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Input{
		Harness:     "codex",
		Cwd:         t.TempDir(),
		Task:        "edit source file",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hook://codex/SessionStart", "hook://codex/UserPromptSubmit"} {
		if !contains(pack.ActiveHooks, want) {
			t.Fatalf("context pack active hooks = %v, want %s", pack.ActiveHooks, want)
		}
	}
	reason := excludedReason(pack, "hook://codex/PreToolUse")
	if !strings.Contains(reason, "no installed PreToolUse write gate") {
		t.Fatalf("context pack excluded=%v, want missing write gate reason", pack.Excluded)
	}
	readiness := findEnvelopeHookReadiness(pack.HookReadiness, "codex", "PreToolUse")
	if readiness == nil || readiness.Status != "not_installed" || !readiness.NativeBlock || readiness.Installed || readiness.AdapterRequired {
		t.Fatalf("context pack hook readiness = %+v, want native gate awaiting installation", pack.HookReadiness)
	}
	if action := findEnvelopeHookAction(readiness.Actions, "operator_install_required"); action == nil || !action.RequiresOperator {
		t.Fatalf("uninstalled native gate missing operator install action: %+v", readiness.Actions)
	}
	if len(pack.RequiredChecks) != 0 {
		t.Fatalf("uninstalled native gate should not misrepresent fixture proof as a sufficient pre-write check: %v", pack.RequiredChecks)
	}

	readOnly := in
	readOnly.Task = "audit current hook status"
	readOnly.Mode = ""
	readOnlyPack, err := PackWithCapabilities(readOnly, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if hasExcluded(readOnlyPack, "hook://codex/PreToolUse") {
		t.Fatalf("read-only context pack should not report missing write gate: %v", readOnlyPack.Excluded)
	}
	if len(readOnlyPack.RequiredChecks) != 0 {
		t.Fatalf("read-only context pack required_checks = %v, want none", readOnlyPack.RequiredChecks)
	}
}

func TestCompileWithRuntimeRecordsExcludedContextAndHookGaps(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `schema_version: compound.installed_hooks.v0.1
hooks:
  - harness: codex
    name: SessionStart
    event: SessionStart
    ref: /tmp/codex-session-start.hook.ts
    status: current
  - harness: codex
    name: UserPromptSubmit
    event: UserPromptSubmit
    ref: /tmp/codex-user-prompt-submit.hook.ts
    status: current
`
	if err := os.WriteFile(filepath.Join(home, "config", "installed-hooks.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Input{
		Harness:     "codex",
		Cwd:         t.TempDir(),
		Task:        "edit source file",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hook://codex/SessionStart", "hook://codex/UserPromptSubmit"} {
		if !contains(ec.ActiveHooks, want) {
			t.Fatalf("effective contract active hooks = %v, want %s", ec.ActiveHooks, want)
		}
	}
	if reason := excludedRefReason(ec.Excluded, "repo://docs"); reason == "" {
		t.Fatalf("effective contract should record out-of-scope repo docs exclusion: %+v", ec.Excluded)
	}
	reason := excludedRefReason(ec.Excluded, "hook://codex/PreToolUse")
	if !strings.Contains(reason, "no installed PreToolUse write gate") {
		t.Fatalf("effective contract excluded=%v, want missing write gate reason", ec.Excluded)
	}
	codexReadiness := findEnvelopeHookReadiness(ec.HookReadiness, "codex", "PreToolUse")
	if codexReadiness == nil || codexReadiness.Status != "not_installed" || !codexReadiness.NativeBlock || codexReadiness.Installed || codexReadiness.AdapterRequired {
		t.Fatalf("effective contract hook readiness = %+v, want native gate awaiting installation", ec.HookReadiness)
	}
	if codexReadiness.AdapterCommand != "" {
		t.Fatalf("native gate awaiting installation should not prescribe a wrapper adapter: %+v", codexReadiness)
	}
	if action := findEnvelopeHookAction(codexReadiness.Actions, "prove_fixture_gate"); action == nil ||
		action.ExpectedExitCode != 3 ||
		!strings.Contains(action.Command, "fixtures/hooks/pre-tool-use.codex.json") {
		t.Fatalf("effective contract hook readiness missing fixture action: %+v", codexReadiness.Actions)
	}
	if action := findEnvelopeHookAction(codexReadiness.Actions, "operator_install_required"); action == nil || !action.RequiresOperator {
		t.Fatalf("effective contract hook readiness missing native install action: %+v", codexReadiness.Actions)
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	packReadiness := findEnvelopeHookReadiness(pack.HookReadiness, "codex", "PreToolUse")
	if packReadiness == nil || packReadiness.Status != "not_installed" || !packReadiness.NativeBlock || packReadiness.AdapterRequired {
		t.Fatalf("context pack hook readiness = %+v, want native gate awaiting installation", pack.HookReadiness)
	}
	if action := findEnvelopeHookAction(packReadiness.Actions, "prove_fixture_gate"); action == nil ||
		action.ExpectedExitCode != 3 ||
		!strings.Contains(action.Command, "fixtures/hooks/pre-tool-use.codex.json") {
		t.Fatalf("context pack hook readiness missing fixture action: %+v", packReadiness.Actions)
	}
	if action := findEnvelopeHookAction(packReadiness.Actions, "operator_install_required"); action == nil || !action.RequiresOperator {
		t.Fatalf("context pack hook readiness missing native install action: %+v", packReadiness.Actions)
	}
	if len(pack.RequiredChecks) != 0 {
		t.Fatalf("native gate awaiting installation should not expose wrapper checks: %v", pack.RequiredChecks)
	}
	readOnly := in
	readOnly.Task = "audit README headings"
	readOnly.Mode = ""
	readOnlyContract, err := CompileWithRuntime(readOnly, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(readOnlyContract.HookReadiness) != 0 {
		t.Fatalf("read-only contract should not carry write-gate readiness: %+v", readOnlyContract.HookReadiness)
	}
	hookAudit := in
	hookAudit.Task = "audit current hook status"
	hookAudit.Mode = ""
	hookAuditContract, err := CompileWithRuntime(hookAudit, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if findEnvelopeHookReadiness(hookAuditContract.HookReadiness, "codex", "PreToolUse") == nil {
		t.Fatalf("hook audit contract should carry hook readiness: %+v", hookAuditContract.HookReadiness)
	}
}

func TestCompileWithRuntimeRecordsTaskConditionedHookRoster(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `schema_version: compound.installed_hooks.v0.1
hooks:
  - harness: claude_code
    name: SessionStart
    event: SessionStart
    ref: /tmp/claude-session-start.hook.ts
    status: current
  - harness: claude_code
    name: UserPromptSubmit
    event: UserPromptSubmit
    ref: /tmp/claude-user-prompt-submit.hook.ts
    status: current
  - harness: claude_code
    name: PreToolUse
    event: PreToolUse
    matcher: "*"
    ref: /tmp/claude-placement-guard.hook.ts
    status: current
`
	if err := os.WriteFile(filepath.Join(home, "config", "installed-hooks.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Input{
		Harness:     "claude_code",
		Cwd:         t.TempDir(),
		Task:        "edit source file",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	preToolUse := findEnvelopeHookRoster(ec.HookRoster, "hook://claude_code/PreToolUse")
	if preToolUse == nil || preToolUse.Status != "active" || preToolUse.Matcher != "*" {
		t.Fatalf("effective contract hook roster = %+v, want active scoped PreToolUse row", ec.HookRoster)
	}
	if !strings.Contains(preToolUse.Reason, "mutation task") {
		t.Fatalf("effective contract PreToolUse reason = %q, want mutation-task reason", preToolUse.Reason)
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	packPreToolUse := findEnvelopeHookRoster(pack.HookRoster, "hook://claude_code/PreToolUse")
	if packPreToolUse == nil || packPreToolUse.Status != "active" || packPreToolUse.Event != "PreToolUse" {
		t.Fatalf("context pack hook roster = %+v, want active PreToolUse row", pack.HookRoster)
	}
	includedPreToolUse, ok := findIncludedRef(pack, "hook://claude_code/PreToolUse")
	if !ok {
		t.Fatalf("context pack included refs missing active PreToolUse hook: %+v", pack.Included)
	}
	if includedPreToolUse.SourceClass != "runtime" || includedPreToolUse.Status != "current" || !strings.Contains(includedPreToolUse.Reason, "mutation task") {
		t.Fatalf("context pack included PreToolUse = %+v, want runtime current mutation-task reason", includedPreToolUse)
	}

	readOnly := in
	readOnly.Task = "audit README headings"
	readOnly.Mode = ""
	readOnlyContract, err := CompileWithRuntime(readOnly, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(readOnlyContract.ActiveHooks, "hook://claude_code/PreToolUse") {
		t.Fatalf("read-only contract must keep universal PreToolUse active: %+v", readOnlyContract.ActiveHooks)
	}
	activePreToolUse := findEnvelopeHookRoster(readOnlyContract.HookRoster, "hook://claude_code/PreToolUse")
	if activePreToolUse == nil || activePreToolUse.Status != "active" {
		t.Fatalf("read-only contract hook roster = %+v, want active universal PreToolUse row", readOnlyContract.HookRoster)
	}
	if strings.Contains(activePreToolUse.Reason, "read-only task") {
		t.Fatalf("read-only universal PreToolUse reason = %q, must not claim exclusion", activePreToolUse.Reason)
	}
}

func TestPackWithCapabilitiesIncludesRelevantCompoundMemory(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{
		{
			Entity:          "compound",
			Predicate:       "capability_authority_surface",
			Object:          "Compound owns the capability graph runtime surface",
			Confidence:      0.91,
			Derivation:      "deterministic",
			ClaimType:       "architecture",
			Qualifiers:      map[string]string{"domain": "capability_authority"},
			PromotionBasis:  candidates.BasisCuratedPage,
			SourceTraceRef:  "ev_compound",
			SnippetHash:     "sha256:compound",
			Visibility:      "contextual",
			ObservationDate: "2026-06-30T00:00:00Z",
		},
		{
			Entity:          "operator",
			Predicate:       "preference",
			Object:          "private self-model fact",
			Confidence:      1,
			Derivation:      "deterministic",
			ClaimType:       "preference",
			PromotionBasis:  candidates.BasisTrustedDeterministic,
			SourceTraceRef:  "ev_private",
			SnippetHash:     "sha256:private",
			Visibility:      "private",
			ObservationDate: "2026-06-30T00:00:00Z",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	relevant, err := store.Adjudicate(got[0].ID, "promote", "test", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(got[1].ID, "promote", "test", fixedNow); err != nil {
		t.Fatal(err)
	}

	in := Input{Harness: "codex", Cwd: "/repo/compound", Task: "resume compound absorption", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	ref := "claim://" + relevant.Claim.ID
	if !contains(pack.ActiveMemory, ref) {
		t.Fatalf("context pack active_memory = %v, want %s", pack.ActiveMemory, ref)
	}
	if !hasIncluded(pack, ref) {
		t.Fatalf("context pack should include memory ref %s; included=%v", ref, pack.Included)
	}
	if !strings.Contains(pack.SummaryMD, "Compound memory (owned ledger): compound.capability_authority_surface") {
		t.Fatalf("summary should carry compact Compound memory line, got %q", pack.SummaryMD)
	}
	if strings.Contains(pack.SummaryMD, "private self-model fact") {
		t.Fatalf("summary leaked private/self-model memory: %q", pack.SummaryMD)
	}
}

// TestPackWithCapabilitiesNarrowsAmbientMemoryByTopic proves the topic-scoped recall
// capability reaches the ContextPack builder (a live work-path consumer), not just the
// `memory recall --topic` CLI. With Input.Topic set, ambient recall keeps only claims in
// the topic subtree and records the excluded out-of-topic claim; with Topic empty, both
// claims surface (regression guard: empty topic = behavior unchanged).
func TestPackWithCapabilitiesNarrowsAmbientMemoryByTopic(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// Two-topic taxonomy: alpha (with a child) and beta, imported the same way memory
	// import-topics does. alpha-workflows parts into alpha, so alpha's subtree covers it.
	topics := []byte(`{
	  "schema_version": "compound.memory_topics.v0.1",
	  "topics": [
	    {"slug": "topic:alpha", "narrows_to": ["topic:alpha-workflows"]},
	    {"slug": "topic:alpha-workflows", "part_of": ["topic:alpha"]},
	    {"slug": "topic:beta"}
	  ]
	}`)
	if _, err := store.ImportTopics(topics, fixedNow); err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{
		{
			Entity:          "compound",
			Predicate:       "alpha_stance",
			Object:          "workflow automation ledger stance",
			Confidence:      0.9,
			Derivation:      "deterministic",
			ClaimType:       "architecture",
			TopicRefs:       []string{"topic:alpha-workflows"}, // child of alpha → in alpha's subtree
			PromotionBasis:  candidates.BasisCuratedPage,
			SourceTraceRef:  "ev_alpha",
			SnippetHash:     "sha256:alpha",
			Visibility:      "contextual",
			ObservationDate: "2026-06-30T00:00:00Z",
		},
		{
			Entity:          "compound",
			Predicate:       "beta_stance",
			Object:          "workflow automation gateway stance",
			Confidence:      0.9,
			Derivation:      "deterministic",
			ClaimType:       "architecture",
			TopicRefs:       []string{"topic:beta"},
			PromotionBasis:  candidates.BasisCuratedPage,
			SourceTraceRef:  "ev_beta",
			SnippetHash:     "sha256:beta",
			Visibility:      "contextual",
			ObservationDate: "2026-06-30T00:00:00Z",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := store.Adjudicate(got[0].ID, "promote", "test", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	beta, err := store.Adjudicate(got[1].ID, "promote", "test", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	alphaRef := "claim://" + alpha.Claim.ID
	betaRef := "claim://" + beta.Claim.ID

	base := Input{Harness: "codex", Cwd: "/repo/compound", Task: "workflow automation stance", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}

	// Topic-narrowed: only the alpha-subtree claim survives; the beta claim is excluded.
	scoped := base
	scoped.Topic = "topic:alpha"
	narrowed, err := PackWithCapabilities(scoped, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(narrowed.ActiveMemory, alphaRef) {
		t.Fatalf("topic-narrowed active_memory = %v, want alpha ref %s", narrowed.ActiveMemory, alphaRef)
	}
	if contains(narrowed.ActiveMemory, betaRef) {
		t.Fatalf("topic-narrowed active_memory = %v, must NOT include out-of-topic beta ref %s", narrowed.ActiveMemory, betaRef)
	}
	if !hasExcluded(narrowed, betaRef) {
		t.Fatalf("topic-narrowed pack should record beta ref %s as excluded with reason; excluded=%v", betaRef, narrowed.Excluded)
	}
	if !excludedReasonContains(narrowed, betaRef, "narrowed to topic") {
		t.Fatalf("beta exclusion reason should name the topic narrowing; excluded=%v", narrowed.Excluded)
	}
	if !strings.Contains(narrowed.SummaryMD, "Ambient recall narrowed to topic") {
		t.Fatalf("topic-narrowed summary should stamp the topic scope, got %q", narrowed.SummaryMD)
	}

	// Regression guard: empty topic keeps the pre-topic behavior — BOTH claims surface,
	// neither is topic-excluded, and no topic stamp appears.
	both, err := PackWithCapabilities(base, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(both.ActiveMemory, alphaRef) || !contains(both.ActiveMemory, betaRef) {
		t.Fatalf("empty-topic active_memory = %v, want both %s and %s", both.ActiveMemory, alphaRef, betaRef)
	}
	if hasExcluded(both, betaRef) {
		t.Fatalf("empty-topic pack must not topic-exclude beta ref %s; excluded=%v", betaRef, both.Excluded)
	}
	if strings.Contains(both.SummaryMD, "Ambient recall narrowed to topic") {
		t.Fatalf("empty-topic summary must not carry a topic stamp, got %q", both.SummaryMD)
	}
}

// TestExcludedMemoryLedgerRecordsOverflowInsteadOfSilentlyDropping is the E11 audit
// falsifier: loadMemoryRefsFromView hard-caps its `excluded` slice at
// maxContextMemoryRefs (5) with a bare `excluded = excluded[:maxContextMemoryRefs]`,
// exactly like the legitimate `out` (ambient inclusion) cap just above it -- but
// Excluded is documented as a "never silently dropped" accounting ledger, not a
// budget-bounded offer list. Reusing more than five out-of-topic claims proves the
// 6th+ exclusion vanishes from pack.Excluded with no trace it was ever evaluated.
func TestExcludedMemoryLedgerRecordsOverflowInsteadOfSilentlyDropping(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	topics := []byte(`{
	  "schema_version": "compound.memory_topics.v0.1",
	  "topics": [
	    {"slug": "topic:alpha", "narrows_to": ["topic:alpha-workflows"]},
	    {"slug": "topic:alpha-workflows", "part_of": ["topic:alpha"]},
	    {"slug": "topic:beta"}
	  ]
	}`)
	if _, err := store.ImportTopics(topics, fixedNow); err != nil {
		t.Fatal(err)
	}

	cands := []mem.Candidate{{
		Entity:          "compound",
		Predicate:       "alpha_stance",
		Object:          "workflow automation ledger stance",
		Confidence:      0.9,
		Derivation:      "deterministic",
		ClaimType:       "architecture",
		TopicRefs:       []string{"topic:alpha-workflows"},
		PromotionBasis:  candidates.BasisCuratedPage,
		SourceTraceRef:  "ev_alpha",
		SnippetHash:     "sha256:alpha",
		Visibility:      "contextual",
		ObservationDate: "2026-06-30T00:00:00Z",
	}}
	// Comfortably more than maxContextMemoryRefs (5) out-of-topic claims -- past the
	// cap, exclusions must still be visible, not silently vanish.
	const outOfTopicCount = maxContextMemoryRefs + 2
	for i := 0; i < outOfTopicCount; i++ {
		cands = append(cands, mem.Candidate{
			Entity:          "compound",
			Predicate:       fmt.Sprintf("beta_stance_%d", i),
			Object:          "workflow automation gateway stance",
			Confidence:      0.9,
			Derivation:      "deterministic",
			ClaimType:       "architecture",
			TopicRefs:       []string{"topic:beta"},
			PromotionBasis:  candidates.BasisCuratedPage,
			SourceTraceRef:  fmt.Sprintf("ev_beta_%d", i),
			SnippetHash:     fmt.Sprintf("sha256:beta-%d", i),
			Visibility:      "contextual",
			ObservationDate: "2026-06-30T00:00:00Z",
		})
	}
	got, err := store.Capture(cands, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	var betaRefs []string
	for i := range got {
		res, err := store.Adjudicate(got[i].ID, "promote", "test", fixedNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if res.Claim != nil && strings.HasPrefix(res.Claim.Predicate, "beta_stance") {
			betaRefs = append(betaRefs, "claim://"+res.Claim.ID)
		}
	}
	if len(betaRefs) != outOfTopicCount {
		t.Fatalf("setup: want %d beta claims promoted, got %d", outOfTopicCount, len(betaRefs))
	}

	in := Input{Harness: "codex", Cwd: "/repo/compound", Task: "workflow automation stance", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute", Topic: "topic:alpha"}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	itemizedBeta := 0
	for _, ref := range betaRefs {
		if hasExcluded(pack, ref) {
			itemizedBeta++
		}
	}
	if itemizedBeta >= outOfTopicCount {
		t.Fatalf("test fixture did not exercise the cap: all %d out-of-topic exclusions were itemized (want capped at %d)", itemizedBeta, maxContextMemoryRefs)
	}

	overflowRef := "exclusion-overflow://memory"
	reason := excludedRefReason(pack.Excluded, overflowRef)
	if reason == "" {
		t.Fatalf("only %d of %d out-of-topic exclusions were itemized and no overflow summary entry (%s) recorded the rest -- %d exclusions vanished silently, violating the never-silently-dropped Excluded ledger contract (audit E11): excluded=%+v",
			itemizedBeta, outOfTopicCount, overflowRef, outOfTopicCount-itemizedBeta, pack.Excluded)
	}
	wantOverflow := outOfTopicCount - itemizedBeta
	if !strings.Contains(reason, fmt.Sprintf("%d", wantOverflow)) {
		t.Fatalf("overflow reason should record the omitted count %d, got %q", wantOverflow, reason)
	}
}

func excludedReasonContains(pack envelope.ContextPack, ref, want string) bool {
	for _, exc := range pack.Excluded {
		if exc.Ref == ref && strings.Contains(exc.Reason, want) {
			return true
		}
	}
	return false
}

func TestPackWithCapabilitiesInjectsFoldedPositionDigest(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{
		{
			Entity:          "project:compound",
			Predicate:       "runtime_stance",
			Object:          "claim refs only",
			Confidence:      0.9,
			Derivation:      "deterministic",
			ClaimType:       "architecture",
			Qualifiers:      map[string]string{"domain": "memory_default_organ"},
			PromotionBasis:  candidates.BasisCuratedPage,
			SourceTraceRef:  "ev_position_old",
			SnippetHash:     "sha256:position-old",
			Visibility:      "contextual",
			ObservationDate: "2026-06-30T00:00:00Z",
		},
		{
			Entity:          "project:compound",
			Predicate:       "runtime_stance",
			Object:          "ContextPack uses folded positions digest",
			Confidence:      0.94,
			Derivation:      "deterministic",
			ClaimType:       "architecture",
			Qualifiers:      map[string]string{"domain": "memory_default_organ"},
			PromotionBasis:  candidates.BasisCuratedPage,
			SourceTraceRef:  "ev_position_new",
			SnippetHash:     "sha256:position-new",
			Visibility:      "contextual",
			ObservationDate: "2026-07-01T00:00:00Z",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(got[0].ID, "promote", "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	latest, err := store.Adjudicate(got[1].ID, "promote", "test", fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	fold, err := store.FoldPositions(mem.PositionFoldOptions{}, fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(fold.Positions) != 1 {
		t.Fatalf("folded positions = %+v, want one runtime_stance slot", fold.Positions)
	}

	in := Input{Harness: "codex", Cwd: "/repo/compound", Task: "wire memory default organ positions ContextPack", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	positionRef := "position://" + fold.Positions[0].ID
	if !contains(pack.ActiveMemory, positionRef) {
		t.Fatalf("context pack active_memory = %v, want positions-derived ref %s", pack.ActiveMemory, positionRef)
	}
	if !hasIncluded(pack, positionRef) {
		t.Fatalf("context pack should include positions-derived ref %s; included=%v", positionRef, pack.Included)
	}
	if len(pack.ActivePositions) != 1 {
		t.Fatalf("context pack active_positions = %+v, want one folded position", pack.ActivePositions)
	}
	pos := pack.ActivePositions[0]
	wantLine := fmt.Sprintf("%s [predicate=runtime_stance, claim_type=architecture] → ContextPack uses folded positions digest (since-seq %d, chain-length 2)", fold.Positions[0].ID, latest.Claim.Seq)
	if pos.Ref != positionRef || pos.Entity != "project:compound" || pos.Topic != "memory_default_organ" ||
		pos.Stance != "ContextPack uses folded positions digest" || pos.SinceSeq != latest.Claim.Seq ||
		pos.ChainLength != 2 || pos.Line != wantLine {
		t.Fatalf("active position = %+v, want line %q", pos, wantLine)
	}
	encodedPosition, err := json.Marshal(pos)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedPosition), `"predicate":`) || strings.Contains(string(encodedPosition), `"claim_type":`) {
		t.Fatalf("context-pack v0.1 ActivePosition gained unversioned fields: %s", encodedPosition)
	}
	if !strings.Contains(pack.SummaryMD, wantLine) {
		t.Fatalf("summary should carry folded positions line %q, got %q", wantLine, pack.SummaryMD)
	}
}

func TestPackWithCapabilitiesKeepsDistinctFactSlotsUnderOneDomain(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	candidatesIn := []mem.Candidate{
		{
			Entity: "compound", Predicate: "os_absorption_end_state", Object: "compound owns the meaning layer",
			Confidence: 0.92, Derivation: "deterministic", ClaimType: "architecture",
			Qualifiers: map[string]string{"domain": "os-absorption"}, PromotionBasis: candidates.BasisCuratedPage,
			SourceTraceRef: "ev_end_state", SnippetHash: "sha256:end-state", Visibility: "contextual",
		},
		{
			Entity: "compound", Predicate: "os_absorption_stance", Object: "adapt sources instead of copying them",
			Confidence: 0.91, Derivation: "deterministic", ClaimType: "architecture",
			Qualifiers: map[string]string{"domain": "os-absorption"}, PromotionBasis: candidates.BasisCuratedPage,
			SourceTraceRef: "ev_stance", SnippetHash: "sha256:stance", Visibility: "contextual",
		},
	}
	captured, err := store.Capture(candidatesIn, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range captured {
		if _, err := store.Adjudicate(candidate.ID, "promote", "test", fixedNow); err != nil {
			t.Fatal(err)
		}
	}
	fold, err := store.FoldPositions(mem.PositionFoldOptions{}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(fold.Positions) != 2 {
		t.Fatalf("folded positions = %+v, want two fact slots", fold.Positions)
	}

	pack, err := PackWithCapabilities(Input{
		Harness: "codex", Cwd: "/repo/compound", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute",
		Task: "verify os absorption end state and stance",
	}, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.ActivePositions) != 2 {
		t.Fatalf("active positions = %+v, want two fact slots", pack.ActivePositions)
	}
	refs := map[string]bool{}
	wantPredicates := map[string]string{
		"compound owns the meaning layer":       "os_absorption_end_state",
		"adapt sources instead of copying them": "os_absorption_stance",
	}
	for _, position := range pack.ActivePositions {
		refs[position.Ref] = true
		wantPredicate, ok := wantPredicates[position.Stance]
		if !ok {
			t.Fatalf("unexpected active position: %+v", position)
		}
		if !strings.Contains(position.Line, "[predicate="+wantPredicate+", claim_type=architecture]") {
			t.Fatalf("active position lost slot meaning: %+v", position)
		}
		if !contains(pack.ActiveMemory, position.Ref) || !strings.Contains(pack.SummaryMD, position.Line) {
			t.Fatalf("position %s not consumed by ContextPack: active=%v summary=%q", position.Ref, pack.ActiveMemory, pack.SummaryMD)
		}
	}
	if len(refs) != 2 {
		t.Fatalf("distinct fact slots collided on refs: %+v", pack.ActivePositions)
	}
}

func TestPackWithCapabilitiesLegacyPositionCheckpointKeepsDecisionRef(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := store.Capture([]mem.Candidate{
		{
			Entity: "compound", Predicate: "os_absorption_end_state", Object: "compound owns the meaning layer",
			Confidence: 0.92, Derivation: "deterministic", ClaimType: "decision",
			Qualifiers: map[string]string{"domain": "os-absorption"}, PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_end_state", SnippetHash: "sha256:end-state-decision", Visibility: "contextual",
		},
		{
			Entity: "compound", Predicate: "os_absorption_stance", Object: "sources remain evidence",
			Confidence: 0.92, Derivation: "deterministic", ClaimType: "decision",
			Qualifiers: map[string]string{"domain": "os-absorption"}, PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_stance", SnippetHash: "sha256:stance-decision", Visibility: "contextual",
		},
		{
			Entity: "compound", Predicate: "os_absorption_method", Object: "adapt sources through governed intake",
			Confidence: 0.92, Derivation: "deterministic", ClaimType: "decision",
			Qualifiers: map[string]string{"domain": "os-absorption"}, PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_method", SnippetHash: "sha256:method-decision", Visibility: "contextual",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	promoted := make([]mem.AdjudicateResult, 0, len(captured))
	for _, candidate := range captured {
		result, err := store.Adjudicate(candidate.ID, "promote", "test", fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		promoted = append(promoted, result)
	}
	legacy, err := json.Marshal(mem.Position{ID: "compound::os-absorption", Entity: "compound", Topic: "os-absorption", ActiveClaimID: promoted[2].Claim.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "ledger", "positions.jsonl"), append(legacy, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	pack, err := PackWithCapabilities(Input{
		Harness: "codex", Cwd: "/repo/compound", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute",
		Task: "verify os absorption method",
	}, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range promoted {
		decisionRef := "decision://" + result.Claim.ID
		if !contains(pack.ActiveDecisions, decisionRef) || !hasIncluded(pack, decisionRef) {
			t.Fatalf("legacy position checkpoint hid accepted decision ref %s: active=%v included=%v", decisionRef, pack.ActiveDecisions, pack.Included)
		}
	}
	if len(pack.ActivePositions) != 0 {
		t.Fatalf("decision claims belong to active_decisions, got active_positions=%+v", pack.ActivePositions)
	}
}

func TestPackWithCapabilitiesEmptyPositionsCheckpointKeepsClaimRefPath(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{{
		Entity:          "project:compound",
		Predicate:       "capability_authority_surface",
		Object:          "Compound owns the capability graph runtime surface",
		Confidence:      0.91,
		Derivation:      "deterministic",
		ClaimType:       "architecture",
		Qualifiers:      map[string]string{"domain": "capability_authority"},
		PromotionBasis:  candidates.BasisCuratedPage,
		SourceTraceRef:  "ev_compound",
		SnippetHash:     "sha256:compound",
		Visibility:      "contextual",
		ObservationDate: "2026-06-30T00:00:00Z",
	}}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := store.Adjudicate(got[0].ID, "promote", "test", fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	in := Input{Harness: "codex", Cwd: "/repo/compound", Task: "resume compound absorption", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	before, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "ledger", "positions.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimRef := "claim://" + promoted.Claim.ID
	if !contains(after.ActiveMemory, claimRef) {
		t.Fatalf("empty positions checkpoint removed old claim-ref path: active_memory=%v want %s", after.ActiveMemory, claimRef)
	}
	beforePositions, err := json.Marshal(before.ActivePositions)
	if err != nil {
		t.Fatal(err)
	}
	afterPositions, err := json.Marshal(after.ActivePositions)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.ActivePositions) == 0 || string(afterPositions) != string(beforePositions) {
		t.Fatalf("empty positions projection changed the owner-folded current positions: before=%s after=%s", beforePositions, afterPositions)
	}
	if !sameStrings(before.ActiveMemory, after.ActiveMemory) || !sameIncludedRefs(before.Included, after.Included) || before.SummaryMD != after.SummaryMD {
		t.Fatalf("empty positions checkpoint changed claim-ref behavior\nbefore active=%v included=%v summary=%q\nafter active=%v included=%v summary=%q",
			before.ActiveMemory, before.Included, before.SummaryMD, after.ActiveMemory, after.Included, after.SummaryMD)
	}
}

func TestPackWithCapabilitiesIncludesTaskRelevantMemoryBeyondEntityKey(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	var cands []mem.Candidate
	for i := 0; i < 5; i++ {
		cands = append(cands, mem.Candidate{
			Entity:          "repo:compound",
			Predicate:       fmt.Sprintf("source_control_fact_%d", i),
			Object:          "repo state is current",
			Confidence:      0.9,
			Derivation:      "deterministic",
			ClaimType:       "status",
			PromotionBasis:  candidates.BasisRuntimeProof,
			SourceTraceRef:  fmt.Sprintf("ev_repo_%d", i),
			SnippetHash:     fmt.Sprintf("sha256:repo-%d", i),
			Visibility:      "operational",
			ObservationDate: "2026-07-02T00:00:00Z",
		})
	}
	cands = append(cands,
		mem.Candidate{
			Entity:          "CapabilityGraph",
			Predicate:       "surfacing_feedback_status",
			Object:          "external skill repo adoption should surface procedure:skill-ops ahead of production-audit for capability surfacing",
			Confidence:      0.91,
			Derivation:      "deterministic",
			ClaimType:       "status",
			Qualifiers:      map[string]string{"domain": "capability-surfacing"},
			PromotionBasis:  candidates.BasisRuntimeProof,
			SourceTraceRef:  "ev_capability_graph",
			SnippetHash:     "sha256:capability-graph",
			Visibility:      "operational",
			ObservationDate: "2026-07-02T00:00:00Z",
		},
		mem.Candidate{
			Entity:          "operator",
			Predicate:       "preference",
			Object:          "private external skill repo preference",
			Confidence:      1,
			Derivation:      "deterministic",
			ClaimType:       "preference",
			PromotionBasis:  candidates.BasisTrustedDeterministic,
			SourceTraceRef:  "ev_private",
			SnippetHash:     "sha256:private",
			Visibility:      "private",
			ObservationDate: "2026-07-02T00:00:00Z",
		},
	)
	got, err := store.Capture(cands, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	var capabilityRef string
	for i := range got {
		res, err := store.Adjudicate(got[i].ID, "promote", "test", fixedNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if res.Claim != nil && res.Claim.Entity == "CapabilityGraph" {
			capabilityRef = "claim://" + res.Claim.ID
		}
	}
	in := Input{
		Harness:     "codex",
		Cwd:         "/repo/compound",
		Task:        "Evaluate external skill repos for relevant capability surfacing without blind adoption",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(pack.ActiveMemory, capabilityRef) {
		t.Fatalf("context pack active_memory = %v, want task-relevant %s", pack.ActiveMemory, capabilityRef)
	}
	if rev := sourceRev(pack.SourceRevisions, memoryRelevanceSourceRef); rev == "" || rev == "unknown" {
		t.Fatalf("context pack memory source revision = %q, want ledger digest in %+v", rev, pack.SourceRevisions)
	}
	if strings.Contains(pack.SummaryMD, "private external skill repo preference") {
		t.Fatalf("summary leaked private/self-model memory: %q", pack.SummaryMD)
	}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ec.ActiveMemory, capabilityRef) {
		t.Fatalf("effective contract active_memory = %v, want task-relevant %s", ec.ActiveMemory, capabilityRef)
	}
	if rev := sourceRev(ec.SourceRevisions, memoryRelevanceSourceRef); rev == "" || rev == "unknown" {
		t.Fatalf("effective contract memory source revision = %q, want ledger digest in %+v", rev, ec.SourceRevisions)
	}
}

func TestPackWithCapabilitiesExcludesVolatileRepoStateMemory(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cands := []mem.Candidate{
		{
			Entity:          "repo:compound",
			Predicate:       "ahead_count",
			Object:          "253",
			Confidence:      1,
			Derivation:      "deterministic",
			ClaimType:       "status",
			Qualifiers:      map[string]string{"domain": "ahead_count"},
			PromotionBasis:  candidates.BasisRuntimeProof,
			SourceTraceRef:  "ev_repo_ahead",
			SnippetHash:     "sha256:repo-ahead",
			Visibility:      "operational",
			ObservationDate: "2026-07-02T00:00:00Z",
		},
		{
			Entity:          "repo:compound",
			Predicate:       "changed_path_count",
			Object:          "4",
			Confidence:      1,
			Derivation:      "deterministic",
			ClaimType:       "status",
			Qualifiers:      map[string]string{"domain": "changed_path_count"},
			PromotionBasis:  candidates.BasisRuntimeProof,
			SourceTraceRef:  "ev_repo_changed",
			SnippetHash:     "sha256:repo-changed",
			Visibility:      "operational",
			ObservationDate: "2026-07-02T00:00:00Z",
		},
		{
			Entity:          "repo:compound",
			Predicate:       "source_control_status",
			Object:          "dirty",
			Confidence:      1,
			Derivation:      "deterministic",
			ClaimType:       "status",
			Qualifiers:      map[string]string{"domain": "source_control_status"},
			PromotionBasis:  candidates.BasisRuntimeProof,
			SourceTraceRef:  "ev_repo_status",
			SnippetHash:     "sha256:repo-status",
			Visibility:      "operational",
			ObservationDate: "2026-07-02T00:00:00Z",
		},
		{
			Entity:          "CapabilityGraph",
			Predicate:       "surfacing_feedback_status",
			Object:          "capability surfacing still reaches context from operational memory",
			Confidence:      0.91,
			Derivation:      "deterministic",
			ClaimType:       "status",
			Qualifiers:      map[string]string{"domain": "capability-surfacing"},
			PromotionBasis:  candidates.BasisRuntimeProof,
			SourceTraceRef:  "ev_capability_graph",
			SnippetHash:     "sha256:capability-graph",
			Visibility:      "operational",
			ObservationDate: "2026-07-02T00:00:00Z",
		},
	}
	got, err := store.Capture(cands, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	var blockedRefs []string
	var capabilityRef string
	for i := range got {
		res, err := store.Adjudicate(got[i].ID, "promote", "test", fixedNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if res.Claim == nil {
			continue
		}
		ref := "claim://" + res.Claim.ID
		if res.Claim.Entity == "repo:compound" {
			blockedRefs = append(blockedRefs, ref)
		}
		if res.Claim.Entity == "CapabilityGraph" {
			capabilityRef = ref
		}
	}
	in := Input{
		Harness:     "codex",
		Cwd:         "/repo/compound",
		Task:        "inspect repo compound changed path count and capability surfacing context",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "review",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(pack.ActiveMemory, capabilityRef) {
		t.Fatalf("context pack active_memory = %v, want useful operational memory %s", pack.ActiveMemory, capabilityRef)
	}
	for _, ref := range blockedRefs {
		if contains(pack.ActiveMemory, ref) || hasIncluded(pack, ref) {
			t.Fatalf("volatile repo-state memory %s leaked into context pack: active=%v included=%v", ref, pack.ActiveMemory, pack.Included)
		}
	}
	if strings.Contains(pack.SummaryMD, "repo:compound.ahead_count") ||
		strings.Contains(pack.SummaryMD, "repo:compound.changed_path_count") ||
		strings.Contains(pack.SummaryMD, "repo:compound.source_control_status") {
		t.Fatalf("summary leaked volatile repo-state memory: %q", pack.SummaryMD)
	}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ec.ActiveMemory, capabilityRef) {
		t.Fatalf("effective contract active_memory = %v, want useful operational memory %s", ec.ActiveMemory, capabilityRef)
	}
	for _, ref := range blockedRefs {
		if contains(ec.ActiveMemory, ref) {
			t.Fatalf("volatile repo-state memory %s leaked into effective contract: active=%v", ref, ec.ActiveMemory)
		}
	}
}

func TestPackWithCapabilitiesIncludesTaskRelevantKnowledgeImportClaim(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cands := []mem.Candidate{
		{
			Entity:         "knowledge:arxiv:2607.01213v1",
			Predicate:      "reference_title",
			Object:         "RepoRescue: An Empirical Study of LLM Agents on Whole-Repository Compatibility Rescue",
			Confidence:     0.8,
			Derivation:     "deterministic",
			PromotionBasis: candidates.BasisTrustedDeterministic,
			Qualifiers: map[string]string{
				"source":      "knowledge_import",
				"source_kind": "arxiv",
				"source_ref":  "arxiv:cs.SE",
			},
			SourceTraceRef:  "ev_arxiv",
			SnippetHash:     "sha256:arxiv",
			Visibility:      "contextual",
			ObservationDate: "2026-07-01",
		},
	}
	for i := 0; i < maxContextMemoryRefs; i++ {
		cands = append(cands, mem.Candidate{
			Entity:          "repo:compound",
			Predicate:       fmt.Sprintf("source_control_fact_%d", i),
			Object:          "repo state is current",
			Confidence:      0.9,
			Derivation:      "deterministic",
			ClaimType:       "status",
			PromotionBasis:  candidates.BasisRuntimeProof,
			SourceTraceRef:  fmt.Sprintf("ev_repo_%d", i),
			SnippetHash:     fmt.Sprintf("sha256:repo-%d", i),
			Visibility:      "operational",
			ObservationDate: "2026-07-02T00:00:00Z",
		})
	}
	got, err := store.Capture(cands, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	var knowledgeRef string
	for i := range got {
		res, err := store.Adjudicate(got[i].ID, "promote", "test", fixedNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if res.Claim != nil && strings.HasPrefix(res.Claim.Entity, "knowledge:") {
			knowledgeRef = "claim://" + res.Claim.ID
		}
	}
	if knowledgeRef == "" {
		t.Fatal("knowledge claim was not promoted")
	}
	matching := Input{
		Harness:     "codex",
		Cwd:         "/repo/compound",
		Task:        "RepoRescue whole-repository compatibility rescue for LLM agents",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(matching, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(pack.ActiveMemory, knowledgeRef) {
		t.Fatalf("context pack active_memory = %v, want task-relevant knowledge claim %s", pack.ActiveMemory, knowledgeRef)
	}
	unrelated := matching
	unrelated.Task = "billing dashboard polish"
	unrelatedPack, err := PackWithCapabilities(unrelated, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(unrelatedPack.ActiveMemory, knowledgeRef) {
		t.Fatalf("unrelated task active_memory = %v, should not include %s", unrelatedPack.ActiveMemory, knowledgeRef)
	}
}

func TestPackWithCapabilitiesIncludesExplicitPrivateChatGPTConversationClaim(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{
		{
			Entity:         "knowledge:chatgpt:conversation-001",
			Predicate:      "proposes_strategy",
			Object:         "An owned personal cognition runtime should answer next actions from goals, thinking patterns, and evidence.",
			Confidence:     0.82,
			Derivation:     "extracted",
			PromotionBasis: "",
			Qualifiers: map[string]string{
				"source":        "knowledge_extraction",
				"source_kind":   "chatgpt",
				"extraction_id": "kx_private_conversation",
			},
			SourceTraceRef:  "ev_private_chatgpt",
			SnippetHash:     "sha256:private-chatgpt",
			Visibility:      "private",
			ObservationDate: "2026-07-03",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(got[0].ID, "park", "operator-reviewed", fixedNow); err != nil {
		t.Fatal(err)
	}
	confirmation, err := store.RecordOperatorConfirmation(got[0].ID, "Keep this private conversation claim.", "operator:test", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	res, err := store.AdjudicateExplicitConfirmation(got[0].ID, confirmation.ID, "operator-reviewed", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claim == nil || res.Claim.PromotionBasis != candidates.BasisExplicitConfirmation {
		t.Fatalf("explicit private conversation claim = %+v", res.Claim)
	}
	ref := "claim://" + res.Claim.ID
	matching := Input{
		Harness:     "claude-code",
		Cwd:         "/repo/compound",
		Task:        "personal cognition runtime next actions from goals patterns evidence",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(matching, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(pack.ActiveMemory, ref) {
		t.Fatalf("context pack active_memory = %v, want explicit private conversation claim %s", pack.ActiveMemory, ref)
	}
	unrelated := matching
	unrelated.Task = "billing dashboard polish"
	unrelatedPack, err := PackWithCapabilities(unrelated, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(unrelatedPack.ActiveMemory, ref) {
		t.Fatalf("unrelated task active_memory = %v, should not include private conversation claim %s", unrelatedPack.ActiveMemory, ref)
	}
	product := matching
	product.RepoKind = "product"
	productPack, err := PackWithCapabilities(product, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(productPack.ActiveMemory, ref) {
		t.Fatalf("product repo active_memory = %v, must not include private conversation claim %s", productPack.ActiveMemory, ref)
	}
	if _, ok := findIncludedRef(productPack, ref); ok {
		t.Fatalf("product repo included refs must not smuggle private conversation claim %s", ref)
	}
	if reason := excludedRefReason(productPack.Excluded, ref); !strings.Contains(reason, "forbids personal-corpus") {
		t.Fatalf("product repo excluded reason for %s = %q, want personal-corpus policy block", ref, reason)
	}
	productContract, err := CompileWithRuntime(product, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(productContract.ActiveMemory, ref) {
		t.Fatalf("product effective contract active_memory = %v, must not include private conversation claim %s", productContract.ActiveMemory, ref)
	}
	if reason := excludedRefReason(productContract.Excluded, ref); !strings.Contains(reason, "forbids personal-corpus") {
		t.Fatalf("product effective contract excluded reason for %s = %q, want personal-corpus policy block", ref, reason)
	}
	library := matching
	library.RepoKind = "library"
	libraryPack, err := PackWithCapabilities(library, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(libraryPack.ActiveMemory, ref) {
		t.Fatalf("library repo active_memory = %v, must not include private conversation claim %s", libraryPack.ActiveMemory, ref)
	}
	if reason := excludedRefReason(libraryPack.Excluded, ref); !strings.Contains(reason, "forbids personal-corpus") {
		t.Fatalf("library repo excluded reason for %s = %q, want personal-corpus policy block", ref, reason)
	}
}

// TestPackWithCapabilitiesExcludesRelevantPersonalClaimWithoutExplicitConfirmation
// proves the control-plane ExplicitOnly personal-corpus gate: a task-relevant
// private/preference/corpus-qualified claim whose durable PromotionBasis is NOT
// candidates.BasisExplicitConfirmation must be excluded from active_memory and
// Included, with the canonical reason "requires explicit personal-corpus
// handling". The existing explicitly-confirmed private-conversation test stays
// green; this test isolates the gap — the prior policy leaked relevant
// personal claims through the `rank <= 0` branch instead of consulting the
// promotion basis.
func TestPackWithCapabilitiesExcludesRelevantPersonalClaimWithoutExplicitConfirmation(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{
		{
			Entity:         "knowledge:corpus:journal",
			Predicate:      "personal_cognition_runtime",
			Object:         "An owned personal cognition runtime should answer next actions from goals, thinking patterns, and evidence.",
			Confidence:     0.82,
			Derivation:     "extracted",
			ClaimType:      "preference",
			PromotionBasis: candidates.BasisCuratedPage,
			Qualifiers: map[string]string{
				"source":        "knowledge_corpus",
				"source_kind":   "notebook",
				"extraction_id": "kx_private_corpus",
			},
			SourceTraceRef:  "ev_private_corpus",
			SnippetHash:     "sha256:private-corpus",
			Visibility:      "private",
			ObservationDate: "2026-07-04",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	res, err := store.Adjudicate(got[0].ID, "promote", "test", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claim == nil {
		t.Fatalf("promote returned nil claim: %+v", res)
	}
	if res.Claim.PromotionBasis == candidates.BasisExplicitConfirmation {
		t.Fatalf("red test setup mistake: promoted basis = %q; need a non-explicit-confirmation basis", res.Claim.PromotionBasis)
	}
	ref := "claim://" + res.Claim.ID
	matching := Input{
		Harness:     "codex",
		Cwd:         "/repo/compound",
		Task:        "personal cognition runtime next actions from goals patterns evidence",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(matching, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(pack.ActiveMemory, ref) {
		t.Fatalf("context pack active_memory = %v, want excluded non-explicit personal-corpus claim %s", pack.ActiveMemory, ref)
	}
	if _, ok := findIncludedRef(pack, ref); ok {
		t.Fatalf("context pack Included refs must not smuggle non-explicit personal-corpus claim %s", ref)
	}
	reason := excludedRefReason(pack.Excluded, ref)
	if !strings.Contains(reason, "requires explicit personal-corpus handling") {
		t.Fatalf("context pack excluded reason for %s = %q, want reason containing %q", ref, reason, "requires explicit personal-corpus handling")
	}
	contract, err := CompileWithRuntime(matching, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(contract.ActiveMemory, ref) {
		t.Fatalf("effective contract active_memory = %v, want excluded non-explicit personal-corpus claim %s", contract.ActiveMemory, ref)
	}
	if reason := excludedRefReason(contract.Excluded, ref); !strings.Contains(reason, "requires explicit personal-corpus handling") {
		t.Fatalf("effective contract excluded reason for %s = %q, want reason containing %q", ref, reason, "requires explicit personal-corpus handling")
	}
}

func TestPackWithCapabilitiesIgnoresStopwordOnlyTaskOverlap(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{
		{
			Entity:         "knowledge:arxiv:stopword.0001v1",
			Predicate:      "reference_title",
			Object:         "Findings about the impact for agents",
			Confidence:     0.8,
			Derivation:     "deterministic",
			PromotionBasis: candidates.BasisTrustedDeterministic,
			Qualifiers: map[string]string{
				"source":      "knowledge_import",
				"source_kind": "arxiv",
			},
			SourceTraceRef:  "ev_stopword",
			SnippetHash:     "sha256:stopword",
			Visibility:      "contextual",
			ObservationDate: "2026-07-01",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	res, err := store.Adjudicate(got[0].ID, "promote", "test", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	ref := "claim://" + res.Claim.ID
	in := Input{
		Harness:     "codex",
		Cwd:         "/repo/compound",
		Task:        "polish the billing dashboard for launch",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if contains(pack.ActiveMemory, ref) {
		t.Fatalf("stopword-only overlap surfaced unrelated claim in active_memory: %v", pack.ActiveMemory)
	}
}

func TestPackWithCapabilitiesRejectsLegacyKnowledgeCandidateVisibility(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Capture([]mem.Candidate{
		{
			Entity:         "knowledge:arxiv:2607.01213v1",
			Predicate:      "reports_result",
			Object:         "RepoRescue evaluates LLM agents on whole-repository compatibility rescue",
			Confidence:     0.8,
			Derivation:     "extracted",
			PromotionBasis: candidates.BasisTrustedDeterministic,
			Qualifiers: map[string]string{
				"source":        "knowledge_extraction",
				"source_kind":   "arxiv",
				"extraction_id": "kx_reporescue",
			},
			SourceTraceRef:  "knowledge_extraction:kx_reporescue",
			SnippetHash:     "sha256:arxiv-meaning",
			Visibility:      "contextual",
			ObservationDate: "2026-07-01",
		},
		{
			Entity:         "knowledge:arxiv:legacy.01213v1",
			Predicate:      "reference_title",
			Object:         "LegacyRepoRescue should not surface through candidate visibility",
			Confidence:     0.8,
			Derivation:     "deterministic",
			PromotionBasis: candidates.BasisTrustedDeterministic,
			Qualifiers: map[string]string{
				"source":      "knowledge_import",
				"source_kind": "arxiv",
			},
			SourceTraceRef:  "ev_legacy_arxiv",
			SnippetHash:     "sha256:legacy-arxiv",
			Visibility:      "candidate",
			ObservationDate: "2026-07-01",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	var meaningRef, legacyRef string
	for i := range got {
		res, err := store.Adjudicate(got[i].ID, "promote", "test", fixedNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		switch res.Claim.Object {
		case "RepoRescue evaluates LLM agents on whole-repository compatibility rescue":
			meaningRef = "claim://" + res.Claim.ID
		case "LegacyRepoRescue should not surface through candidate visibility":
			legacyRef = "claim://" + res.Claim.ID
		}
	}
	matching := Input{
		Harness:     "codex",
		Cwd:         "/repo/compound",
		Task:        "RepoRescue whole-repository compatibility rescue LLM agents LegacyRepoRescue",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(matching, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(pack.ActiveMemory, meaningRef) {
		t.Fatalf("context pack active_memory = %v, want task-relevant meaning claim %s", pack.ActiveMemory, meaningRef)
	}
	if contains(pack.ActiveMemory, legacyRef) {
		t.Fatalf("legacy candidate-visibility knowledge claim surfaced via special case: %v", pack.ActiveMemory)
	}
}

func TestRuntimeContractDedupesDecisionRefsByDecisionID(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := store.Capture([]mem.Candidate{
		{
			Entity:         "decision:legacy-DEC-0001",
			Predicate:      "decided",
			Object:         "Legacy localization baseline",
			Confidence:     1,
			Derivation:     "deterministic",
			ClaimType:      "decision",
			Qualifiers:     map[string]string{"decision_id": "DEC-0001", "lifecycle_status": "accepted"},
			PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_old",
			SnippetHash:    "old",
			Visibility:     "ambient",
		},
		{
			Entity:         "decision:DEC-0001",
			Predicate:      "decided",
			Object:         "Current localization baseline",
			Confidence:     1,
			Derivation:     "deterministic",
			ClaimType:      "decision",
			Qualifiers:     map[string]string{"decision_id": "DEC-0001", "lifecycle_status": "accepted"},
			PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_new",
			SnippetHash:    "new",
			Visibility:     "ambient",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Adjudicate(cands[0].ID, "promote", "test", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Adjudicate(cands[1].ID, "promote", "test", fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	in := mkInput("control-plane")
	in.Task = "update localization baseline"
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := countStrings(ec.ActiveDecisions, "decision://DEC-0001"); got != 1 {
		t.Fatalf("contract should include one decision ref, got %d in %v", got, ec.ActiveDecisions)
	}
	if rev := sourceRev(ec.SourceRevisions, "decision://DEC-0001"); rev != "claim:"+second.Claim.ID {
		t.Fatalf("contract should use newest claim revision, got %q want %q (old %s)", rev, "claim:"+second.Claim.ID, first.Claim.ID)
	}

	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := countStrings(pack.ActiveDecisions, "decision://DEC-0001"); got != 1 {
		t.Fatalf("pack should include one active decision ref, got %d in %v", got, pack.ActiveDecisions)
	}
	if got := countIncluded(pack, "decision://DEC-0001"); got != 1 {
		t.Fatalf("pack should include one included decision ref, got %d in %+v", got, pack.Included)
	}
}

func TestControlPlaneContextRanksBoundsAndExplainsAcceptedDecisions(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}

	candidatesToCapture := []mem.Candidate{
		{
			Entity:         "decision:DEC-Z-TOP",
			Predicate:      "decided",
			Object:         "Claude capture session receipt reliability repair",
			Confidence:     1,
			Derivation:     "deterministic",
			ClaimType:      "decision",
			Qualifiers:     map[string]string{"decision_id": "DEC-Z-TOP", "lifecycle_status": "accepted"},
			PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_top",
			SnippetHash:    "top",
			Visibility:     "ambient",
		},
	}
	for i := 0; i < 13; i++ {
		id := fmt.Sprintf("DEC-TIE-%02d", i)
		candidatesToCapture = append(candidatesToCapture, mem.Candidate{
			Entity:         "decision:" + id,
			Predicate:      "decided",
			Object:         "Claude capture event",
			Confidence:     1,
			Derivation:     "deterministic",
			ClaimType:      "decision",
			Qualifiers:     map[string]string{"decision_id": id, "lifecycle_status": "accepted"},
			PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_" + id,
			SnippetHash:    strings.ToLower(id),
			Visibility:     "ambient",
		})
	}
	candidatesToCapture = append(candidatesToCapture, mem.Candidate{
		Entity:         "decision:DEC-IRRELEVANT",
		Predicate:      "decided",
		Object:         "Paywall checkout conversion baseline",
		Confidence:     1,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     map[string]string{"decision_id": "DEC-IRRELEVANT", "lifecycle_status": "accepted"},
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "ev_irrelevant",
		SnippetHash:    "irrelevant",
		Visibility:     "ambient",
	})
	candidatesToCapture = append(candidatesToCapture, mem.Candidate{
		Entity:         "decision:DEC-TARGET-4242",
		Predicate:      "decided",
		Object:         "Identifier-only sentinel",
		Confidence:     1,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     map[string]string{"decision_id": "DEC-TARGET-4242", "lifecycle_status": "accepted"},
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "ev_identifier",
		SnippetHash:    "identifier",
		Visibility:     "ambient",
	})
	for _, id := range []string{"brain-D1", "brain-D2"} {
		candidatesToCapture = append(candidatesToCapture, mem.Candidate{
			Entity:         "decision:" + id,
			Predicate:      "decided",
			Object:         "Short identifier sentinel",
			Confidence:     1,
			Derivation:     "deterministic",
			ClaimType:      "decision",
			Qualifiers:     map[string]string{"decision_id": id, "lifecycle_status": "accepted"},
			PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_" + id,
			SnippetHash:    strings.ToLower(id),
			Visibility:     "ambient",
		})
	}
	candidatesToCapture = append(candidatesToCapture, mem.Candidate{
		Entity:         "legacy:fallback",
		Predicate:      "decided",
		Object:         "Canonical fallback sentinel",
		Confidence:     1,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     map[string]string{"lifecycle_status": "accepted"},
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "ev_fallback",
		SnippetHash:    "fallback",
		Visibility:     "ambient",
	})

	captured, err := store.Capture(candidatesToCapture, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimIDs := map[string]string{}
	for i, candidate := range captured {
		result, err := store.Adjudicate(candidate.ID, "promote", "test", fixedNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimIDs[decisions.CanonicalRef(*result.Claim)] = result.Claim.ID
	}

	in := Input{
		Harness:     "codex",
		Cwd:         "/repo/control-plane",
		Task:        "repair claude capture session receipt reliability",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	ec, err := CompileWithRuntime(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	wantActive := []string{"decision://DEC-Z-TOP"}
	for i := 0; i < 11; i++ {
		wantActive = append(wantActive, fmt.Sprintf("decision://DEC-TIE-%02d", i))
	}
	if got, want := strings.Join(ec.ActiveDecisions, "\n"), strings.Join(wantActive, "\n"); got != want {
		t.Fatalf("effective contract decisions = %v, want ranked bounded set %v", ec.ActiveDecisions, wantActive)
	}
	if got, want := strings.Join(pack.ActiveDecisions, "\n"), strings.Join(wantActive, "\n"); got != want {
		t.Fatalf("context pack decisions = %v, want parity with effective contract %v", pack.ActiveDecisions, wantActive)
	}
	for _, ref := range wantActive {
		if !hasIncluded(pack, ref) {
			t.Fatalf("selected decision %s missing from included refs: %+v", ref, pack.Included)
		}
		wantRev := "claim:" + claimIDs[ref]
		if got := sourceRev(ec.SourceRevisions, ref); got != wantRev {
			t.Fatalf("effective contract source revision for %s = %q, want %q", ref, got, wantRev)
		}
		if got := sourceRev(pack.SourceRevisions, ref); got != wantRev {
			t.Fatalf("context pack source revision for %s = %q, want %q", ref, got, wantRev)
		}
	}

	for _, ref := range []string{"decision://DEC-TIE-11", "decision://DEC-TIE-12"} {
		wantReason := "accepted decision from Compound ledger omitted: relevant but outside 12-ref task context budget"
		if got := exclusionReason(ec.Excluded, ref); got != wantReason {
			t.Fatalf("effective contract budget exclusion for %s = %q, want %q", ref, got, wantReason)
		}
		if got := exclusionReason(pack.Excluded, ref); got != wantReason {
			t.Fatalf("context pack budget exclusion for %s = %q, want %q", ref, got, wantReason)
		}
		if hasIncluded(pack, ref) || sourceRev(ec.SourceRevisions, ref) != "" || sourceRev(pack.SourceRevisions, ref) != "" {
			t.Fatalf("budget-omitted decision %s leaked into included refs or source revisions", ref)
		}
	}

	irrelevantRef := "decision://DEC-IRRELEVANT"
	wantIrrelevantReason := "accepted decision from Compound ledger omitted: no task relevance"
	if got := exclusionReason(ec.Excluded, irrelevantRef); got != wantIrrelevantReason {
		t.Fatalf("effective contract irrelevant exclusion = %q, want %q", got, wantIrrelevantReason)
	}
	if got := exclusionReason(pack.Excluded, irrelevantRef); got != wantIrrelevantReason {
		t.Fatalf("context pack irrelevant exclusion = %q, want %q", got, wantIrrelevantReason)
	}
	if hasIncluded(pack, irrelevantRef) || sourceRev(ec.SourceRevisions, irrelevantRef) != "" || sourceRev(pack.SourceRevisions, irrelevantRef) != "" {
		t.Fatalf("irrelevant decision leaked into included refs or source revisions")
	}

	structuralOnly := in
	structuralOnly.Task = "inspect accepted decision lifecycle status"
	structuralEC, err := CompileWithRuntime(structuralOnly, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(structuralEC.ActiveDecisions) != 0 {
		t.Fatalf("structural ledger vocabulary manufactured task relevance: %v", structuralEC.ActiveDecisions)
	}

	exactID := in
	exactID.Task = "apply DEC-TARGET-4242"
	exactIDEC, err := CompileWithRuntime(exactID, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(exactIDEC.ActiveDecisions, "\n"), "decision://DEC-TARGET-4242"; got != want {
		t.Fatalf("exact decision id lookup = %v, want only %s", exactIDEC.ActiveDecisions, want)
	}

	shortID := in
	shortID.Task = "apply brain-D1"
	shortIDEC, err := CompileWithRuntime(shortID, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(shortIDEC.ActiveDecisions, "\n"), "decision://brain-D1"; got != want {
		t.Fatalf("short decision id lookup = %v, want only %s", shortIDEC.ActiveDecisions, want)
	}

	fallbackRef := ""
	for ref, claimID := range claimIDs {
		if strings.HasSuffix(ref, claimID) {
			fallbackRef = ref
			break
		}
	}
	if fallbackRef == "" {
		t.Fatal("missing canonical fallback decision ref")
	}
	fallbackID := in
	fallbackID.Task = "apply " + fallbackRef
	fallbackIDEC, err := CompileWithRuntime(fallbackID, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fallbackIDEC.ActiveDecisions, "\n"); got != fallbackRef {
		t.Fatalf("claim-id fallback lookup = %v, want only %s", fallbackIDEC.ActiveDecisions, fallbackRef)
	}

	emptyTask := in
	emptyTask.Task = ""
	emptyEC, err := CompileWithRuntime(emptyTask, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyEC.ActiveDecisions) != 0 {
		t.Fatalf("empty task selected decisions: %v", emptyEC.ActiveDecisions)
	}
	for ref := range claimIDs {
		if got, want := exclusionReason(emptyEC.Excluded, ref), "accepted decision from Compound ledger omitted: no task relevance"; got != want {
			t.Fatalf("empty-task exclusion for %s = %q, want %q", ref, got, want)
		}
		if got := sourceRev(emptyEC.SourceRevisions, ref); got != "" {
			t.Fatalf("empty task leaked source revision for %s: %q", ref, got)
		}
	}
}

func TestTaskMentionsDecisionIdentityRequiresWholeIdentity(t *testing.T) {
	for _, tc := range []struct {
		task string
		want bool
	}{
		{task: "apply brain-D1", want: true},
		{task: "apply `brain-D1`", want: true},
		{task: "apply decision://brain-D1", want: true},
		{task: "apply xbrain-D1", want: false},
		{task: "apply brain-D10", want: false},
		{task: "apply brain-D1-extra", want: false},
	} {
		if got := taskMentionsDecisionIdentity(tc.task, "brain-D1"); got != tc.want {
			t.Errorf("taskMentionsDecisionIdentity(%q) = %v, want %v", tc.task, got, tc.want)
		}
	}
}

func TestProductContextScopesAcceptedDecisionsByTask(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := store.Capture([]mem.Candidate{
		{
			Entity:         "decision:DEC-PAYWALL",
			Predicate:      "decided",
			Object:         "Paywall onboarding checkout context must carry conversion proof before changing purchase flow",
			Confidence:     1,
			Derivation:     "deterministic",
			ClaimType:      "decision",
			Qualifiers:     map[string]string{"decision_id": "DEC-PAYWALL", "lifecycle_status": "accepted"},
			PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_paywall",
			SnippetHash:    "paywall",
			Visibility:     "ambient",
		},
		{
			Entity:         "decision:DEC-CONTROL",
			Predicate:      "decided",
			Object:         "Control-plane packet dispatch should use WorkEnvelope receipts before route promotion",
			Confidence:     1,
			Derivation:     "deterministic",
			ClaimType:      "decision",
			Qualifiers:     map[string]string{"decision_id": "DEC-CONTROL", "lifecycle_status": "accepted"},
			PromotionBasis: candidates.BasisDecisionRecord,
			SourceTraceRef: "ev_control",
			SnippetHash:    "control",
			Visibility:     "ambient",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, cand := range cands {
		if _, err := store.Adjudicate(cand.ID, "promote", "test", fixedNow); err != nil {
			t.Fatal(err)
		}
	}

	in := Input{
		Harness:     "codex",
		Cwd:         "/repo/card-intel",
		Task:        "fix onboarding paywall conversion bug",
		RepoKind:    "product",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := countStrings(pack.ActiveDecisions, "decision://DEC-PAYWALL"); got != 1 {
		t.Fatalf("product pack should include task-relevant decision, got %d in %v", got, pack.ActiveDecisions)
	}
	if contains(pack.ActiveDecisions, "decision://DEC-CONTROL") || hasIncluded(pack, "decision://DEC-CONTROL") {
		t.Fatalf("product pack should not include unrelated control-plane decision: active=%v included=%+v", pack.ActiveDecisions, pack.Included)
	}
	if !hasExcluded(pack, "decision://DEC-CONTROL") {
		t.Fatalf("product pack should record unrelated accepted decision as excluded: %+v", pack.Excluded)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root")
		}
		dir = parent
	}
}

func hasIncluded(pack envelope.ContextPack, ref string) bool {
	for _, inc := range pack.Included {
		if inc.Ref == ref {
			return true
		}
	}
	return false
}

func countIncluded(pack envelope.ContextPack, ref string) int {
	n := 0
	for _, inc := range pack.Included {
		if inc.Ref == ref {
			n++
		}
	}
	return n
}

func countCapabilityIncluded(pack envelope.ContextPack) int {
	n := 0
	for _, inc := range pack.Included {
		if strings.HasPrefix(inc.Ref, "capability://") {
			n++
		}
	}
	return n
}

func countStrings(values []string, want string) int {
	n := 0
	for _, value := range values {
		if value == want {
			n++
		}
	}
	return n
}

func hasExcluded(pack envelope.ContextPack, ref string) bool {
	return exclusionReason(pack.Excluded, ref) != ""
}

func exclusionReason(excluded []envelope.ExcludedRef, ref string) string {
	for _, exc := range excluded {
		if exc.Ref == ref {
			return exc.Reason
		}
	}
	return ""
}

func writeOSAdaptationRuleFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	planDir := filepath.Join(repo, "docs_private", "plan")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rule := strings.Join([]string{
		"# Operating-System Adaptation Rule",
		"## Instruction Tuning Standard",
		"- Trigger: old operating-system material is being adapted into Compound.",
		"- Action: translate it into a consumed command, hook, projection, doctor, ContextPack, or product loop.",
		"- Boundary: do not copy private paths, accounts, skill bodies, or prompt blocks.",
		"- Silence condition: ordinary product/library tasks do not load this.",
		"- Proof: a command/test/projection consumes the translated behavior.",
		"- Retirement: the old owner becomes narrower, retired, or evidence-only.",
		"Only merge and proof count.",
	}, "\n")
	if err := os.WriteFile(filepath.Join(planDir, "OPERATING-SYSTEM-ADAPTATION-RULE.md"), []byte(rule), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func writeOracleSecondOpinionRuleFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	planDir := filepath.Join(repo, "docs_private", "plan")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rule := strings.Join([]string{
		"# Oracle Second-Opinion Rule",
		"## Trigger",
		"- high-leverage doctrine, governance, public/private, hook, spine, routing, or memory changes.",
		"## Action",
		"- ask Oracle with files attached; save answer privately; decide locally.",
		"## Boundary",
		"- Oracle evidence is not authority; do not edit config or expose private bodies.",
		"## Silence",
		"- ordinary deterministic implementation and tests.",
		"## Proof",
		"- saved private answer plus local tests/doctor/installed command.",
		"## Retirement",
		"- retire when a generic second-opinion adapter owns this path.",
	}, "\n")
	if err := os.WriteFile(filepath.Join(planDir, "ORACLE-SECOND-OPINION-RULE.md"), []byte(rule), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func excludedReason(pack envelope.ContextPack, ref string) string {
	return excludedRefReason(pack.Excluded, ref)
}

func excludedRefReason(excluded []envelope.ExcludedRef, ref string) string {
	for _, exc := range excluded {
		if exc.Ref == ref {
			return exc.Reason
		}
	}
	return ""
}

func installedHookHasMatcher(manifest InstalledHooksManifest, harness, name, matcher string) bool {
	for _, hook := range manifest.Hooks {
		if hook.Harness == harness && hook.Name == name && hook.Matcher == matcher {
			return true
		}
	}
	return false
}

func findInstalledHook(manifest InstalledHooksManifest, harness, name string) *InstalledHook {
	for i := range manifest.Hooks {
		if manifest.Hooks[i].Harness == harness && manifest.Hooks[i].Name == name {
			return &manifest.Hooks[i]
		}
	}
	return nil
}

func findHookGateReadiness(readiness []HookGateReadiness, harness string) *HookGateReadiness {
	for i := range readiness {
		if readiness[i].Harness == harness {
			return &readiness[i]
		}
	}
	return nil
}

func findEnvelopeHookReadiness(readiness []envelope.HookReadiness, harness, hook string) *envelope.HookReadiness {
	for i := range readiness {
		if readiness[i].Harness == harness && readiness[i].Hook == hook {
			return &readiness[i]
		}
	}
	return nil
}

func findEnvelopeHookAction(actions []envelope.HookReadinessAction, kind string) *envelope.HookReadinessAction {
	for i := range actions {
		if actions[i].Kind == kind {
			return &actions[i]
		}
	}
	return nil
}

func findEnvelopeHookRoster(roster []envelope.HookRosterEntry, ref string) *envelope.HookRosterEntry {
	for i := range roster {
		if roster[i].Ref == ref {
			return &roster[i]
		}
	}
	return nil
}

func findCapabilityOffer(pack envelope.ContextPack, ref string) *envelope.CapabilityOffer {
	for i := range pack.CapabilityOffers {
		if pack.CapabilityOffers[i].Ref == ref {
			return &pack.CapabilityOffers[i]
		}
	}
	return nil
}

func findEffectiveContractCapabilityOffer(ec envelope.EffectiveContract, ref string) *envelope.CapabilityOffer {
	for i := range ec.CapabilityOffers {
		if ec.CapabilityOffers[i].Ref == ref {
			return &ec.CapabilityOffers[i]
		}
	}
	return nil
}

func contractHookReadiness(rows []InstalledHookRuntimeReadiness, harnessName, hookName string) *InstalledHookRuntimeReadiness {
	for i := range rows {
		if rows[i].Harness == harnessName && rows[i].Hook == hookName {
			return &rows[i]
		}
	}
	return nil
}

func findHookGateAction(actions []HookGateAction, kind string) *HookGateAction {
	for i := range actions {
		if actions[i].Kind == kind {
			return &actions[i]
		}
	}
	return nil
}

func hasOfferConstraint(values []envelope.CapabilityOfferConstraint, kind, ref string) bool {
	for _, value := range values {
		if value.Kind == kind && value.Ref == ref {
			return true
		}
	}
	return false
}

func offerConstraintsContainDetail(values []envelope.CapabilityOfferConstraint, needle string) bool {
	for _, value := range values {
		if strings.Contains(value.Detail, needle) {
			return true
		}
	}
	return false
}

func containsText(s, needle string) bool {
	return strings.Contains(s, needle)
}

func containsStringWith(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func sourceRev(revs []envelope.SourceRev, source string) string {
	for _, rev := range revs {
		if rev.Source == source {
			return rev.Rev
		}
	}
	return ""
}

func jsonObject(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameIncludedRefs(a, b []envelope.IncludedRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeBreadthEngine is a deterministic mem.Engine test double: it returns a fixed, caller-set hit
// list (still honoring the ledger-computed allowlist), so a test can assert the ContextPack's
// ambient selection uses the engine's ranking WITHOUT a live sidecar or embedder.
type fakeBreadthEngine struct{ hits []mem.Hit }

func (f *fakeBreadthEngine) Capabilities() mem.EngineCapabilities {
	return mem.EngineCapabilities{Name: "fake", Breadth: true}
}
func (f *fakeBreadthEngine) Index(_ []mem.Claim, _ int64) error { return nil }
func (f *fakeBreadthEngine) Query(_, _ string, allowlist []string, _ int) ([]mem.Hit, error) {
	allowed := map[string]bool{}
	for _, id := range allowlist {
		allowed[id] = true
	}
	out := make([]mem.Hit, 0, len(f.hits))
	for _, h := range f.hits {
		if allowed[h.ClaimID] { // respect the ledger gate — the engine never surfaces a non-visible claim
			out = append(out, h)
		}
	}
	return out, nil
}

// TestPackWithCapabilitiesUsesBreadthEngineForAmbientSelection proves the keystone wiring: the
// ContextPack's ambient memory is ranked by the owned breadth engine, not just token overlap — a
// claim that shares NO tokens with the task surfaces only because the engine ranked it (fails
// before the wiring, passes after). The second assertion guards the boundary: a claim the ledger
// gates (volatile repo-state), even ranked #1 by the engine, must NOT surface — the engine RANKS,
// the ledger GATES.
func TestPackWithCapabilitiesUsesBreadthEngineForAmbientSelection(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cands := []mem.Candidate{
		{ // A — token-overlapping control: naive ranker already surfaces it
			Entity: "lookup-notes", Predicate: "state", Object: "the lookup path latency was reduced",
			Confidence: 0.9, Derivation: "deterministic", ClaimType: "status",
			Qualifiers:     map[string]string{"domain": "lookup-notes"},
			PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "ev_a", SnippetHash: "sha256:a",
			Visibility: "operational", ObservationDate: "2026-07-02T00:00:00Z",
		},
		{ // B — semantically related but TOKEN-DISJOINT from the task: only the engine can surface it
			Entity: "vector-index", Predicate: "state", Object: "embeddings enable semantic retrieval over the corpus",
			Confidence: 0.9, Derivation: "deterministic", ClaimType: "status",
			Qualifiers:     map[string]string{"domain": "vector-index"},
			PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "ev_b", SnippetHash: "sha256:b",
			Visibility: "operational", ObservationDate: "2026-07-02T00:00:00Z",
		},
		{ // C — volatile repo-state: LEDGER-GATED; must never enter ambient even ranked #1
			Entity: "repo:compound", Predicate: "changed_path_count", Object: "9",
			Confidence: 1, Derivation: "deterministic", ClaimType: "status",
			Qualifiers:     map[string]string{"domain": "changed_path_count"},
			PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "ev_c", SnippetHash: "sha256:c",
			Visibility: "operational", ObservationDate: "2026-07-02T00:00:00Z",
		},
	}
	got, err := store.Capture(cands, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for i := range got {
		res, err := store.Adjudicate(got[i].ID, "promote", "test", fixedNow.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if res.Claim != nil {
			ids[res.Claim.Entity] = res.Claim.ID
		}
	}
	idB, idC := ids["vector-index"], ids["repo:compound"]
	if idB == "" || idC == "" {
		t.Fatalf("seed claims not promoted: ids=%v", ids)
	}

	// The engine ranks C highest and B next; A is left to the naive ranker (it has token overlap).
	fake := &fakeBreadthEngine{hits: []mem.Hit{{ClaimID: idC, Score: 0.99}, {ClaimID: idB, Score: 0.80}}}
	orig := breadthEngineFactory
	breadthEngineFactory = func(string) (mem.Engine, func()) { return fake, func() {} }
	t.Cleanup(func() { breadthEngineFactory = orig })
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "fake-sidecar") // engages semanticTaskRanks; factory is faked

	in := Input{Harness: "claude", Cwd: "/repo/compound", Task: "speed up the lookup path", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	refB := "claim://" + idB
	if !contains(pack.ActiveMemory, refB) {
		t.Fatalf("token-disjoint claim %s not surfaced by the breadth engine: active_memory=%v", refB, pack.ActiveMemory)
	}
	refC := "claim://" + idC
	if contains(pack.ActiveMemory, refC) || hasIncluded(pack, refC) {
		t.Fatalf("ledger-gated volatile claim %s leaked into ambient despite engine rank #1: active=%v", refC, pack.ActiveMemory)
	}
}
