package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/hookplan"
)

func TestImportHookInstallReportRejectsConfigRevisionMismatchBeforeAuthorityMutation(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	report, err := hookplan.InstallHooks(
		harnessHome,
		filepath.Join(harnessHome, ".local", "bin", "compound"),
		"global",
		[]string{"codex"},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	configPath := report.Entries[0].ConfigPath
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(configData, ' '), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ImportHookInstallReport(compoundHome, harnessHome, report, time.Now()); err == nil ||
		!strings.Contains(err.Error(), "config revision mismatch") {
		t.Fatalf("ImportHookInstallReport() error = %v, want config revision mismatch", err)
	}
	for _, path := range []string{
		InstalledHooksPath(compoundHome),
		InstalledHooksSourceAuthorityPath(compoundHome),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("revision mismatch mutated authority at %s: %v", path, err)
		}
	}
}

func TestImportHookInstallReportPublishesAllHarnessesAsOneAuthorityUpdate(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	binaryPath := filepath.Join(harnessHome, ".local", "bin", "compound")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("synthetic compound binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := hookplan.InstallHooks(harnessHome, binaryPath, "global", nil, false)
	if err != nil {
		t.Fatal(err)
	}

	results, err := ImportHookInstallReport(
		compoundHome,
		harnessHome,
		report,
		time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("combined install published %d authority updates, want 1", len(results))
	}
	result := results[0]
	if result.SourceAuthority.SourcePath != managedHookInstallationRef {
		t.Fatalf("combined source ref = %q, want %q", result.SourceAuthority.SourcePath, managedHookInstallationRef)
	}
	wantHarnesses := []string{"claude_code", "claude_minimax", "claude_qwen", "claude_zai", "codex"}
	if strings.Join(result.Harnesses, ",") != strings.Join(wantHarnesses, ",") {
		t.Fatalf("combined harnesses = %v, want %v", result.Harnesses, wantHarnesses)
	}
	// 24 = three isolated Claude lanes at five bindings each, plus codex at five (native SessionEnd added 2026-08-26;
	// SessionEnd receipt) and claude_code at four (no per-turn UserPromptSubmit).
	if result.IncomingHookCount != 24 || len(result.Manifest.Hooks) != 24 {
		t.Fatalf(
			"combined manifest incoming=%d hooks=%d, want all 24 lifecycle bindings",
			result.IncomingHookCount,
			len(result.Manifest.Hooks),
		)
	}
	records, err := LoadInstalledHooksSourceAuthority(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("combined install wrote %d source-authority records, want 1", len(records))
	}
}

func TestImportHookInstallReportRollsBackAuthorityWhenProvenanceAppendFails(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	report, err := hookplan.InstallHooks(
		harnessHome,
		filepath.Join(harnessHome, ".local", "bin", "compound"),
		"global",
		[]string{"codex"},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	provenancePath := InstalledHooksSourceAuthorityPath(compoundHome)
	if err := os.MkdirAll(filepath.Dir(provenancePath), 0o755); err != nil {
		t.Fatal(err)
	}
	provenanceBefore := []byte("{malformed provenance\n")
	if err := os.WriteFile(provenancePath, provenanceBefore, 0o640); err != nil {
		t.Fatal(err)
	}
	authorityPath := InstalledHooksPath(compoundHome)
	if err := os.MkdirAll(filepath.Dir(authorityPath), 0o755); err != nil {
		t.Fatal(err)
	}
	authorityBefore := []byte(`schema_version: compound.installed_hooks.v0.1
hooks:
  - harness: claude_code
    name: SessionStart
    event: SessionStart
    ref: handler:prior
    revision: ""
    status: current
`)
	if err := os.WriteFile(authorityPath, authorityBefore, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ImportHookInstallReport(compoundHome, harnessHome, report, time.Now()); err == nil ||
		!strings.Contains(err.Error(), "parse installed hooks source authority") {
		t.Fatalf("ImportHookInstallReport() error = %v, want provenance append failure", err)
	}
	authorityAfter, err := os.ReadFile(authorityPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(authorityAfter) != string(authorityBefore) {
		t.Fatalf("failed publication changed authority:\nbefore=%s\nafter=%s", authorityBefore, authorityAfter)
	}
	authorityInfo, err := os.Stat(authorityPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := authorityInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("failed publication changed authority mode to %o, want 600", got)
	}
	provenanceAfter, err := os.ReadFile(provenancePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(provenanceAfter) != string(provenanceBefore) {
		t.Fatalf("failed publication changed provenance:\nbefore=%s\nafter=%s", provenanceBefore, provenanceAfter)
	}
	info, err := os.Stat(provenancePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("failed publication changed provenance mode to %o, want 640", got)
	}
}

func TestImportHookInstallReportRollsBackNewAuthorityUnderRestrictiveUmask(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	report, err := hookplan.InstallHooks(
		harnessHome,
		filepath.Join(harnessHome, ".local", "bin", "compound"),
		"global",
		[]string{"codex"},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	provenancePath := InstalledHooksSourceAuthorityPath(compoundHome)
	if err := os.MkdirAll(filepath.Dir(provenancePath), 0o755); err != nil {
		t.Fatal(err)
	}
	provenanceBefore := []byte("{malformed provenance\n")
	if err := os.WriteFile(provenancePath, provenanceBefore, 0o640); err != nil {
		t.Fatal(err)
	}

	previousUmask := syscall.Umask(0o077)
	defer syscall.Umask(previousUmask)
	if _, err := ImportHookInstallReport(compoundHome, harnessHome, report, time.Now()); err == nil ||
		!strings.Contains(err.Error(), "parse installed hooks source authority") {
		t.Fatalf("ImportHookInstallReport() error = %v, want provenance append failure", err)
	}

	authorityPath := InstalledHooksPath(compoundHome)
	if _, err := os.Stat(authorityPath); !os.IsNotExist(err) {
		t.Fatalf("failed publication left partial authority at %s: %v", authorityPath, err)
	}
	provenanceAfter, err := os.ReadFile(provenancePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(provenanceAfter) != string(provenanceBefore) {
		t.Fatalf("failed publication changed provenance:\nbefore=%s\nafter=%s", provenanceBefore, provenanceAfter)
	}
}

func TestRollbackHookInstallAuthorityPreservesConcurrentChanges(t *testing.T) {
	for _, initiallyExists := range []bool{false, true} {
		name := "absent preimage"
		if initiallyExists {
			name = "existing preimage"
		}
		t.Run(name, func(t *testing.T) {
			compoundHome := t.TempDir()
			authorityPath := InstalledHooksPath(compoundHome)
			if err := os.MkdirAll(filepath.Dir(authorityPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if initiallyExists {
				if err := os.WriteFile(authorityPath, []byte("prior authority\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			snapshots, err := snapshotHookInstallAuthority(compoundHome)
			if err != nil {
				t.Fatal(err)
			}
			for i := range snapshots {
				if snapshots[i].path != authorityPath {
					continue
				}
				snapshots[i].postKnown = true
				snapshots[i].postExists = true
				snapshots[i].postData = []byte("transaction-owned post-image\n")
				snapshots[i].postMode = 0o644
				if snapshots[i].existed {
					snapshots[i].postMode = snapshots[i].mode
				}
			}
			concurrent := []byte("concurrent authority edit\n")
			if err := os.WriteFile(authorityPath, concurrent, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(authorityPath, 0o640); err != nil {
				t.Fatal(err)
			}

			err = rollbackHookInstallAuthority(snapshots)
			if err == nil || !strings.Contains(err.Error(), "rollback conflict") {
				t.Fatalf("rollback error = %v, want concurrent-change conflict", err)
			}
			after, err := os.ReadFile(authorityPath)
			if err != nil {
				t.Fatalf("rollback removed concurrent authority: %v", err)
			}
			if string(after) != string(concurrent) {
				t.Fatalf("rollback clobbered concurrent authority:\nwant=%s\ngot=%s", concurrent, after)
			}
			info, err := os.Stat(authorityPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o640 {
				t.Fatalf("rollback clobbered concurrent authority mode: got %o want 640", got)
			}
		})
	}
}

func TestRollbackHookInstallAuthorityRequiresExactPostimageMode(t *testing.T) {
	compoundHome := t.TempDir()
	authorityPath := InstalledHooksPath(compoundHome)
	if err := os.MkdirAll(filepath.Dir(authorityPath), 0o755); err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshotHookInstallAuthority(compoundHome)
	if err != nil {
		t.Fatal(err)
	}
	ownPostimage := []byte("transaction-owned post-image\n")
	for i := range snapshots {
		if snapshots[i].path == authorityPath {
			snapshots[i].postKnown = true
			snapshots[i].postExists = true
			snapshots[i].postData = ownPostimage
			snapshots[i].postMode = 0o644
		}
	}
	if err := os.WriteFile(authorityPath, ownPostimage, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(authorityPath, 0o600); err != nil {
		t.Fatal(err)
	}

	err = rollbackHookInstallAuthority(snapshots)
	if err == nil || !strings.Contains(err.Error(), "rollback conflict") {
		t.Fatalf("rollback error = %v, want post-image mode conflict", err)
	}
	after, err := os.ReadFile(authorityPath)
	if err != nil {
		t.Fatalf("rollback removed mode-drifted authority: %v", err)
	}
	if string(after) != string(ownPostimage) {
		t.Fatalf("rollback changed mode-drifted authority:\nwant=%s\ngot=%s", ownPostimage, after)
	}
	info, err := os.Stat(authorityPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("rollback clobbered concurrent mode: got %o want 600", got)
	}
}

func TestReconcileInstalledHooksReportsLiveClaudeSessionEndAsExtraUnowned(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	t.Setenv("HOME", harnessHome)
	registrationSource := filepath.Join("..", "..", "fixtures", "harness", "hook-registration.ts")
	importReport, err := ImportHookRegistrationTS(compoundHome, registrationSource, time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ImportHookRegistrationTS() error = %v", err)
	}
	writeHarnessHookConfig(t, filepath.Join(harnessHome, ".claude", "settings.json"), `{
  "hooks": {
    "SessionStart": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/RecallContext.hook.ts\""}]}],
    "UserPromptSubmit": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/IntentRecall.hook.ts\""}]}],
    "PreToolUse": [{"matcher":"*","hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/PlacementGuard.hook.ts\""}]}],
    "SessionEnd": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/SessionReceipt.hook.ts\""}]}]
  }
}`)
	writeHarnessHookConfig(t, filepath.Join(harnessHome, ".codex", "hooks.json"), `{
  "hooks": {
    "SessionStart": [{"hooks":[{"type":"command","command":"$HOME/.bun/bin/bun $HOME/.codex/hooks/RecallContext.hook.ts"}]}],
    "UserPromptSubmit": [{"hooks":[{"type":"command","command":"$HOME/.bun/bin/bun $HOME/.codex/hooks/IntentRecall.hook.ts"}]}]
  }
}`)

	got, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() error = %v", err)
	}
	if len(got.Plan.Bindings) != importReport.HookCount {
		t.Fatalf("desired HookPlan bindings = %+v, want %d imported authority rows", got.Plan.Bindings, importReport.HookCount)
	}
	if hookPlanHasEvent(got.Plan, "claude_code", "SessionEnd") {
		t.Fatalf("observed SessionEnd was promoted into desired plan: %+v", got.Plan.Bindings)
	}
	if len(got.Observation.Bindings) != 6 {
		t.Fatalf("observed installation bindings = %+v, want six live registrations", got.Observation.Bindings)
	}
	for _, binding := range got.Observation.Bindings {
		if binding.HandlerRevision != "" {
			t.Fatalf("config command digest masqueraded as handler revision: %+v", binding)
		}
		if binding.InvocationRevision == "" {
			t.Fatalf("observed hook lost privacy-safe invocation identity: %+v", binding)
		}
	}
	delta := hookReconciliationDelta(got.Reconciliation, "claude_code", "SessionEnd")
	if delta == nil || delta.State != hookplan.StateExtraUnowned {
		t.Fatalf("Claude SessionEnd delta = %+v, want extra_unowned", delta)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal reconciliation report: %v", err)
	}
	if strings.Contains(string(encoded), ".bun/bin/bun ") || strings.Contains(string(encoded), `\"$HOME`) {
		t.Fatalf("reconciliation leaked a full harness command: %s", encoded)
	}
}

func TestImportAndObserveInstalledHookContentAreIndependentAttestations(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	t.Setenv("HOME", harnessHome)
	handlerPath := filepath.Join(harnessHome, ".claude", "hooks", "RecallContext.hook.ts")
	writeHarnessHookConfig(t, handlerPath, "#!/usr/bin/env bun\nprocess.stdout.write('{}')")
	sourcePath := filepath.Join(t.TempDir(), "hook-registration.ts")
	source := `export const HOOK_REGISTRATIONS = [
  { harness: "claude", event: "SessionStart", file: "RecallContext.hook.ts", command: ` + "`\"$HOME/.claude/hooks/RecallContext.hook.ts\"`" + ` },
];`
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	imported, err := ImportHookRegistrationTS(compoundHome, sourcePath, time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ImportHookRegistrationTS() error = %v", err)
	}
	if len(imported.Manifest.Hooks) != 1 || len(imported.Manifest.Hooks[0].Revision) != len("sha256:")+64 || !strings.HasPrefix(imported.Manifest.Hooks[0].Revision, "sha256:") {
		t.Fatalf("imported handler content was not attested: %+v", imported.Manifest.Hooks)
	}
	desiredRevision := imported.Manifest.Hooks[0].Revision

	writeHarnessHookConfig(t, filepath.Join(harnessHome, ".claude", "settings.json"), `{
  "hooks": {"SessionStart": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/RecallContext.hook.ts\""}]}]}
}`)
	ready, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() ready error = %v", err)
	}
	delta := hookReconciliationDelta(ready.Reconciliation, "claude_code", "SessionStart")
	if delta == nil || delta.State != hookplan.StateReady {
		t.Fatalf("matching independent content attestations = %+v, want ready", delta)
	}
	if len(ready.Observation.Bindings) != 1 || ready.Observation.Bindings[0].HandlerRevision != desiredRevision {
		t.Fatalf("observed handler attestation = %+v, want %s", ready.Observation.Bindings, desiredRevision)
	}

	writeHarnessHookConfig(t, handlerPath, "#!/usr/bin/env bun\nprocess.stdout.write('{\"drifted\":true}')")
	drifted, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() drift error = %v", err)
	}
	delta = hookReconciliationDelta(drifted.Reconciliation, "claude_code", "SessionStart")
	if delta == nil || delta.State != hookplan.StateDrifted {
		t.Fatalf("changed handler content = %+v, want drifted", delta)
	}

	if err := os.Remove(handlerPath); err != nil {
		t.Fatal(err)
	}
	missing, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() missing-handler error = %v", err)
	}
	delta = hookReconciliationDelta(missing.Reconciliation, "claude_code", "SessionStart")
	if delta == nil || delta.State != hookplan.StateDrifted {
		t.Fatalf("missing known handler = %+v, want drifted", delta)
	}

	realHandlerPath := filepath.Join(harnessHome, ".claude", "hooks", "real-recall.ts")
	writeHarnessHookConfig(t, realHandlerPath, "#!/usr/bin/env bun\nprocess.stdout.write('{}')")
	if err := os.Symlink(realHandlerPath, handlerPath); err != nil {
		t.Fatal(err)
	}
	symlinked, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() symlink error = %v", err)
	}
	delta = hookReconciliationDelta(symlinked.Reconciliation, "claude_code", "SessionStart")
	if delta == nil || delta.State != hookplan.StateDrifted {
		t.Fatalf("symlinked known handler = %+v, want drifted", delta)
	}
}

func TestImportUnavailableConcreteHandlerReconcilesDrifted(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, handlerPath string)
	}{
		{name: "missing"},
		{
			name: "symlink",
			setup: func(t *testing.T, handlerPath string) {
				realPath := filepath.Join(filepath.Dir(handlerPath), "real-recall.ts")
				writeHarnessHookConfig(t, realPath, "#!/usr/bin/env bun\nprocess.stdout.write('{}')")
				if err := os.Symlink(realPath, handlerPath); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compoundHome := t.TempDir()
			harnessHome := t.TempDir()
			t.Setenv("HOME", harnessHome)
			handlerPath := filepath.Join(harnessHome, ".claude", "hooks", "RecallContext.hook.ts")
			if test.setup != nil {
				test.setup(t, handlerPath)
			}
			sourcePath := filepath.Join(t.TempDir(), "hook-registration.ts")
			source := `export const HOOK_REGISTRATIONS = [
  { harness: "claude", event: "SessionStart", file: "RecallContext.hook.ts", command: ` + "`\"$HOME/.claude/hooks/RecallContext.hook.ts\"`" + ` },
];`
			if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}

			imported, err := ImportHookRegistrationTS(compoundHome, sourcePath, time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("ImportHookRegistrationTS() error = %v", err)
			}
			if len(imported.Manifest.Hooks) != 1 || imported.Manifest.Hooks[0].Revision != installedHookHandlerUnavailableRevision {
				t.Fatalf("imported unavailable handler attestation = %+v, want fail-loud sentinel", imported.Manifest.Hooks)
			}
			writeHarnessHookConfig(t, filepath.Join(harnessHome, ".claude", "settings.json"), `{
  "hooks": {"SessionStart": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/RecallContext.hook.ts\""}]}]}
}`)
			reconciled, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
			if err != nil {
				t.Fatalf("ReconcileInstalledHooks() error = %v", err)
			}
			delta := hookReconciliationDelta(reconciled.Reconciliation, "claude_code", "SessionStart")
			if delta == nil || delta.State != hookplan.StateDrifted || delta.DesiredHandlerRevision != installedHookHandlerUnavailableRevision {
				t.Fatalf("unavailable imported handler reconciliation = %+v, want drifted sentinel", delta)
			}
		})
	}
}

func TestOpaqueImportedHandlerIdentityRemainsRegisteredWithoutContentAttestation(t *testing.T) {
	manifest := InstalledHooksManifest{
		SchemaVersion: installedHooksSchemaVersion,
		Hooks: []InstalledHook{
			{Harness: "claude_code", Name: "SessionStart", Event: "SessionStart", Ref: "handler:opaque-runtime", Status: "current"},
		},
	}
	attestInstalledHookHandlers(&manifest)
	if manifest.Hooks[0].Revision != "" {
		t.Fatalf("opaque handler identity received concrete-unavailable sentinel: %+v", manifest.Hooks[0])
	}
	plan, err := hookPlanFromInstalledManifest(manifest, t.TempDir())
	if err != nil {
		t.Fatalf("hookPlanFromInstalledManifest() error = %v", err)
	}
	reconciled, err := hookplan.Reconcile(plan, hookplan.InstallationSnapshot{Bindings: []hookplan.ObservedBinding{
		{Harness: "claude_code", Event: "SessionStart", HandlerRef: "handler:opaque-runtime", Active: true},
	}}, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(reconciled.Deltas) != 1 || reconciled.Deltas[0].State != hookplan.StateRegistered {
		t.Fatalf("opaque handler reconciliation = %+v, want registered", reconciled.Deltas)
	}
}

func TestCanonicalHookInvocationRevisionIncludesArgumentsForEveryHandler(t *testing.T) {
	harnessHome := t.TempDir()
	tests := []struct {
		name       string
		desired    string
		observed   string
		driftedArg string
	}{
		{
			name:       "compound binary",
			desired:    `"$HOME/.local/bin/compound" hook pre-tool-use --live --harness codex`,
			observed:   filepath.Join(harnessHome, ".local", "bin", "compound") + ` hook pre-tool-use --live --harness codex`,
			driftedArg: filepath.Join(harnessHome, ".local", "bin", "compound") + ` hook session-start --live --harness codex`,
		},
		{
			name:       "typescript hook",
			desired:    `"$HOME/.claude/hooks/RecallContext.hook.ts" --mode compact`,
			observed:   `"` + filepath.Join(harnessHome, ".claude", "hooks", "RecallContext.hook.ts") + `" --mode compact`,
			driftedArg: `"` + filepath.Join(harnessHome, ".claude", "hooks", "RecallContext.hook.ts") + `" --mode full`,
		},
		{
			// hookplan.HookCommand shell-quotes the binary path (install.go) so a spaced
			// install directory (e.g. an app-bundle path) shows up quoted in both the
			// portable $HOME-form authority ref and the literal absolute-path observation.
			// The two must canonicalize identically (quoting must not read as drift), and the
			// quoted path must resolve as ONE field so the trailing subcommand still
			// participates in drift detection (driftedArg still differs).
			name:       "compound binary with spaced install directory",
			desired:    `'$HOME/Compound Apps/compound' hook pre-tool-use --live --harness codex`,
			observed:   `'` + filepath.Join(harnessHome, "Compound Apps", "compound") + `' hook pre-tool-use --live --harness codex`,
			driftedArg: `'` + filepath.Join(harnessHome, "Compound Apps", "compound") + `' hook session-start --live --harness codex`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			desired := canonicalHookInvocationRevision(test.desired, harnessHome)
			observed := canonicalHookInvocationRevision(test.observed, harnessHome)
			drifted := canonicalHookInvocationRevision(test.driftedArg, harnessHome)
			if !strings.HasPrefix(desired, "sha256:") {
				t.Fatalf("canonical invocation revision = %q, want privacy-safe digest", desired)
			}
			if desired != observed {
				t.Fatalf("equivalent commands differ: desired=%q observed=%q", desired, observed)
			}
			if desired == drifted {
				t.Fatalf("different command arguments collapsed to %q", desired)
			}
		})
	}
}

func TestHookPlanSkipsRetiredInstalledRows(t *testing.T) {
	manifest := InstalledHooksManifest{
		SchemaVersion: installedHooksSchemaVersion,
		Hooks: []InstalledHook{
			{Harness: "claude_code", Name: "SessionStart", Event: "SessionStart", Ref: "handler://active", Status: "current"},
			{Harness: "claude_code", Name: "SessionEnd", Event: "SessionEnd", Ref: "handler://retired", Status: "retired"},
		},
	}
	plan, err := hookPlanFromInstalledManifest(manifest, t.TempDir())
	if err != nil {
		t.Fatalf("hookPlanFromInstalledManifest() error = %v", err)
	}
	if len(plan.Bindings) != 1 || plan.Bindings[0].Event != "SessionStart" {
		t.Fatalf("HookPlan bindings = %+v, want only current row", plan.Bindings)
	}
}

func TestImportHookRegistrationPreservesExternalRowsAcrossNarrowReimport(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	t.Setenv("HOME", harnessHome)
	for path, body := range map[string]string{
		filepath.Join(harnessHome, ".claude", "hooks", "RecallContext.hook.ts"):   "#!/usr/bin/env bun\nprocess.stdout.write('{}')",
		filepath.Join(harnessHome, ".claude", "hooks", "IntentRecall.hook.ts"):    "#!/usr/bin/env bun\nprocess.stdout.write('{}')",
		filepath.Join(harnessHome, ".claude", "hooks", "KittyEnvPersist.hook.ts"): "#!/usr/bin/env bun\nprocess.stdout.write('{}')",
	} {
		writeHarnessHookConfig(t, path, body)
	}
	broadSourcePath := filepath.Join(t.TempDir(), "hook-registration.ts")
	broadSource := `export const HOOK_REGISTRATIONS = [
  { harness: "claude", event: "SessionStart", file: "RecallContext.hook.ts", command: ` + "`\"$HOME/.claude/hooks/RecallContext.hook.ts\"`" + ` },
  { harness: "claude", event: "UserPromptSubmit", file: "IntentRecall.hook.ts", command: ` + "`\"$HOME/.claude/hooks/IntentRecall.hook.ts\"`" + ` },
  { harness: "claude", event: "SessionStart", file: "KittyEnvPersist.hook.ts", command: ` + "`\"$HOME/.claude/hooks/KittyEnvPersist.hook.ts\"`" + `, status: "preserved_external" },
];`
	if err := os.WriteFile(broadSourcePath, []byte(broadSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportHookRegistrationTS(compoundHome, broadSourcePath, time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("broad ImportHookRegistrationTS() error = %v", err)
	}

	narrowSourcePath := filepath.Join(t.TempDir(), "hook-registration.ts")
	narrowSource := `export const HOOK_REGISTRATIONS = [
  { harness: "claude", event: "SessionStart", file: "RecallContext.hook.ts", command: ` + "`\"$HOME/.claude/hooks/RecallContext.hook.ts\"`" + ` },
];`
	if err := os.WriteFile(narrowSourcePath, []byte(narrowSource), 0o644); err != nil {
		t.Fatal(err)
	}
	reimported, err := ImportHookRegistrationTS(compoundHome, narrowSourcePath, time.Date(2026, 7, 14, 20, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("narrow ImportHookRegistrationTS() error = %v", err)
	}
	if reimported.HookCount != 2 {
		t.Fatalf("narrow re-import hook count = %d, want current source row plus preserved declaration", reimported.HookCount)
	}
	manifest, err := LoadInstalledHooksManifest(compoundHome)
	if err != nil {
		t.Fatalf("LoadInstalledHooksManifest() error = %v", err)
	}
	if len(manifest.Hooks) != 2 {
		t.Fatalf("merged installed hooks = %+v, want narrow managed authority plus preserved declaration", manifest.Hooks)
	}
	preserved := installedHookByRefSuffix(manifest, "/.claude/hooks/KittyEnvPersist.hook.ts")
	if preserved == nil || preserved.Status != "preserved_external" {
		t.Fatalf("preserved external row after re-import = %+v", preserved)
	}
	if intent := installedHookByRefSuffix(manifest, "/.claude/hooks/IntentRecall.hook.ts"); intent != nil {
		t.Fatalf("managed row omitted by authoritative re-import was retained: %+v", intent)
	}

	writeHarnessHookConfig(t, filepath.Join(harnessHome, ".claude", "settings.json"), `{
  "hooks": {
    "SessionStart": [{"hooks":[
      {"type":"command","command":"\"$HOME/.claude/hooks/RecallContext.hook.ts\""},
      {"type":"command","command":"\"$HOME/.claude/hooks/KittyEnvPersist.hook.ts\""}
    ]}],
    "UserPromptSubmit": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/IntentRecall.hook.ts\""}]}]
  }
}`)
	reconciled, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() error = %v", err)
	}
	if len(reconciled.Plan.Bindings) != 1 || len(reconciled.Plan.PreservedBindings) != 1 {
		t.Fatalf("desired/preserved plan split = desired:%+v preserved:%+v", reconciled.Plan.Bindings, reconciled.Plan.PreservedBindings)
	}
	if delta := hookReconciliationDeltaByRef(reconciled.Reconciliation, "$HOME/.claude/hooks/KittyEnvPersist.hook.ts"); delta == nil || delta.State != hookplan.StateExternalPreserved {
		t.Fatalf("KittyEnvPersist reconciliation = %+v, want external_preserved", delta)
	}

	in := Input{Harness: "claude_code", Cwd: t.TempDir(), Task: "use session hooks", RepoKind: "product", Mode: "execute"}
	contract, err := CompileWithRuntime(in, DefaultPolicy(), compoundHome, time.Date(2026, 7, 14, 20, 10, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("CompileWithRuntime() error = %v", err)
	}
	if len(contract.ActiveHooks) != 1 || countString(contract.ActiveHooks, "hook://claude_code/SessionStart") != 1 {
		t.Fatalf("preserved external row leaked into active_hooks: %+v", contract.ActiveHooks)
	}
}

func TestImportCodexHooksPreservesExternalPeerWhenCompoundOwnsLifecycleSlot(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	t.Setenv("HOME", harnessHome)

	compoundPath := filepath.Join(harnessHome, ".local", "bin", "compound")
	recallPath := filepath.Join(harnessHome, ".codex", "hooks", "RecallContext.hook.ts")
	writeHarnessHookConfig(t, compoundPath, "compound fixture")
	writeHarnessHookConfig(t, recallPath, "recall fixture")

	sourcePath := filepath.Join(harnessHome, ".codex", "hooks.json")
	writeHarnessHookConfig(t, sourcePath, `{
  "hooks": {
    "SessionStart": [
      {"hooks":[{"type":"command","command":"RECALL_HARNESS=codex $HOME/.bun/bin/bun $HOME/.codex/hooks/RecallContext.hook.ts"}]},
      {"hooks":[{"type":"command","command":"$HOME/.local/bin/compound hook session-start --live --harness codex"}]}
    ]
  }
}`)

	report, err := ImportCodexHooksJSON(compoundHome, sourcePath, time.Date(2026, 7, 29, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ImportCodexHooksJSON() error = %v", err)
	}
	managed := installedHookByRefSuffix(report.Manifest, "/.local/bin/compound")
	if managed == nil || managed.Status != "current" {
		t.Fatalf("Compound SessionStart row = %+v, want current", managed)
	}
	preserved := installedHookByRefSuffix(report.Manifest, "/.codex/hooks/RecallContext.hook.ts")
	if preserved == nil || preserved.Status != "preserved_external" {
		t.Fatalf("RecallContext SessionStart row = %+v, want preserved_external", preserved)
	}

	reconciled, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() error = %v", err)
	}
	if len(reconciled.Plan.Bindings) != 1 || len(reconciled.Plan.PreservedBindings) != 1 {
		t.Fatalf("desired/preserved plan split = desired:%+v preserved:%+v", reconciled.Plan.Bindings, reconciled.Plan.PreservedBindings)
	}
	if delta := hookReconciliationDeltaByRef(reconciled.Reconciliation, "$HOME/.codex/hooks/RecallContext.hook.ts"); delta == nil || delta.State != hookplan.StateExternalPreserved {
		t.Fatalf("RecallContext reconciliation = %+v, want external_preserved", delta)
	}
}

func TestReconcileInstalledHooksWithoutPlanStillReportsObservedExtras(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	writeHarnessHookConfig(t, filepath.Join(harnessHome, ".claude", "settings.json"), `{
  "hooks": {
    "SessionEnd": [{"hooks":[{"type":"command","command":"\"$HOME/.claude/hooks/SessionReceipt.hook.ts\""}]}]
  }
}`)

	got, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() error = %v", err)
	}
	if len(got.Plan.Bindings) != 0 || len(got.Observation.Bindings) != 1 {
		t.Fatalf("desired/observed split = plan:%+v observed:%+v", got.Plan.Bindings, got.Observation.Bindings)
	}
	if len(got.Reconciliation.Deltas) != 1 || got.Reconciliation.Deltas[0].State != hookplan.StateExtraUnowned {
		t.Fatalf("reconciliation = %+v, want observed hook extra_unowned", got.Reconciliation.Deltas)
	}
}

func TestCanonicalHookHandlerRefNormalizesCurrentHomeWithoutExplicitHarnessHome(t *testing.T) {
	harnessHome := t.TempDir()
	t.Setenv("HOME", harnessHome)
	absolute := canonicalHookHandlerRef(filepath.Join(harnessHome, ".local", "bin", "compound")+` hook pre-tool-use --live --harness codex`, "")
	portable := canonicalHookHandlerRef(`$HOME/.local/bin/compound hook pre-tool-use --live --harness codex`, harnessHome)
	if absolute != portable {
		t.Fatalf("implicit current-home normalization drifted: absolute=%q portable=%q", absolute, portable)
	}
}

// TestCanonicalHookCommandFieldsQuoteTransparentForSpacedPath is a direct falsifier: a quoted
// binary path containing a space (as hookplan.HookCommand now emits for a spaced install
// directory) must tokenize as ONE field, not split at the embedded space. Against the historical
// strings.Fields-based implementation this produced 7 bogus fields ("/tmp/Compound",
// "Apps/compound", "hook", ...) instead of the correct 6.
func TestCanonicalHookCommandFieldsQuoteTransparentForSpacedPath(t *testing.T) {
	cmd := `'/tmp/Compound Apps/compound' hook pre-tool-use --live --harness codex`
	got := canonicalHookCommandFields(cmd)
	want := []string{"/tmp/Compound Apps/compound", "hook", "pre-tool-use", "--live", "--harness", "codex"}
	if len(got) != len(want) {
		t.Fatalf("canonicalHookCommandFields(%q) = %#v (%d fields), want %#v (%d fields)", cmd, got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("canonicalHookCommandFields(%q)[%d] = %q, want %q (full: %#v)", cmd, i, got[i], want[i], got)
		}
	}
}

// TestCanonicalHookCommandFieldsUnquotedUnchanged is the backward-compat invariant: a command
// with no quotes and no spaces-inside-quotes must tokenize identically to the pre-fix
// strings.Fields + per-field quote-trim implementation, so existing installed handler refs and
// invocation revisions never drift because of this change.
func TestCanonicalHookCommandFieldsUnquotedUnchanged(t *testing.T) {
	commands := []string{
		"/usr/local/bin/compound hook pre-tool-use --live --harness codex",
		"$HOME/.bun/bin/bun $HOME/.codex/hooks/RecallContext.hook.ts",
		"compound hook pre-tool-use --live --harness claude-code",
		"",
		"   ",
	}
	for _, cmd := range commands {
		got := canonicalHookCommandFields(cmd)
		want := legacyHookCommandFields(cmd)
		if len(got) != len(want) {
			t.Fatalf("canonicalHookCommandFields(%q) = %#v, want %#v (unquoted backward-compat break)", cmd, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("canonicalHookCommandFields(%q) = %#v, want %#v (unquoted backward-compat break)", cmd, got, want)
			}
		}
	}
}

// legacyHookCommandFields is the pre-fix tokenizer (plain strings.Fields + per-field quote-trim),
// kept here only to pin down the backward-compat invariant above.
func legacyHookCommandFields(command string) []string {
	fields := strings.Fields(command)
	for i := range fields {
		fields[i] = strings.Trim(fields[i], "\"'`")
	}
	return fields
}

// TestCanonicalHookHandlerRefQuoteTransparentForSpacedInstallPath proves the quote-transparent
// tokenizer composes with $HOME normalization: a quoted spaced install path canonicalizes to the
// SAME handler ref whether it's given as a portable "$HOME/..." reference or an absolute path, so
// a correctly-quoted install (hookplan.HookCommand) is never read as drift merely because of how
// its path happens to be represented.
func TestCanonicalHookHandlerRefQuoteTransparentForSpacedInstallPath(t *testing.T) {
	harnessHome := t.TempDir()
	spacedPath := filepath.Join(harnessHome, "Compound Apps", "compound")
	absolute := `'` + spacedPath + `' hook pre-tool-use --live --harness codex`
	portable := `'$HOME/Compound Apps/compound' hook pre-tool-use --live --harness codex`

	got := canonicalHookHandlerRef(absolute, harnessHome)
	want := canonicalHookHandlerRef(portable, harnessHome)
	if got != want {
		t.Fatalf("quoted spaced install path canonicalized differently: absolute=%q portable=%q (quoting introduced false drift)", got, want)
	}
	if got == "" {
		t.Fatal("handler ref empty for spaced install path")
	}
}

func writeHarnessHookConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir harness config: %v", err)
	}
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatalf("write harness config: %v", err)
	}
}

func hookPlanHasEvent(plan hookplan.HookPlan, harnessName, event string) bool {
	for _, binding := range plan.Bindings {
		if binding.Harness == harnessName && binding.Event == event {
			return true
		}
	}
	return false
}

func hookReconciliationDelta(reconciliation hookplan.Reconciliation, harnessName, event string) *hookplan.Delta {
	for i := range reconciliation.Deltas {
		if reconciliation.Deltas[i].Harness == harnessName && reconciliation.Deltas[i].Event == event {
			return &reconciliation.Deltas[i]
		}
	}
	return nil
}

func hookReconciliationDeltaByRef(reconciliation hookplan.Reconciliation, ref string) *hookplan.Delta {
	for i := range reconciliation.Deltas {
		if reconciliation.Deltas[i].ObservedHandlerRef == ref {
			return &reconciliation.Deltas[i]
		}
	}
	return nil
}

func installedHookByRefSuffix(manifest InstalledHooksManifest, suffix string) *InstalledHook {
	for i := range manifest.Hooks {
		if strings.HasSuffix(manifest.Hooks[i].Ref, suffix) {
			return &manifest.Hooks[i]
		}
	}
	return nil
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

// The delegate-readiness blocking_hook_live gate consumes this loop end-to-end:
// a harness whose hooks are genuinely installed must reconcile its
// mutation-covering PreToolUse binding to ready. claude_minimax stayed
// invisible here even after a real install, because the observation source
// table lacked its profile settings path -- so the readiness gate reported the
// binding missing forever while the authority and the settings both had it.
func TestReconcileInstalledHooksObservesClaudeMinimaxProfile(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	t.Setenv("HOME", harnessHome)
	binaryPath := filepath.Join(harnessHome, ".local", "bin", "compound")
	writeHarnessHookConfig(t, binaryPath, "#!/bin/sh\nexit 0\n")

	report, err := hookplan.InstallHooks(harnessHome, binaryPath, "global", []string{"claude-minimax"}, false)
	if err != nil {
		t.Fatalf("InstallHooks(claude-minimax) error = %v", err)
	}
	if _, err := ImportHookInstallReport(
		compoundHome,
		harnessHome,
		report,
		time.Date(2026, 7, 31, 15, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("ImportHookInstallReport() error = %v", err)
	}

	got, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() error = %v", err)
	}
	observed := 0
	for _, binding := range got.Observation.Bindings {
		if binding.Harness == "claude_minimax" {
			observed++
		}
	}
	if observed != 5 {
		t.Fatalf("observed %d claude_minimax bindings, want 5 (observation source missing)", observed)
	}
	delta := hookReconciliationDelta(got.Reconciliation, "claude_minimax", "PreToolUse")
	if delta == nil || delta.State != hookplan.StateReady {
		t.Fatalf("claude_minimax PreToolUse reconciliation = %+v, want ready", delta)
	}
}

// TestReconcileInstalledHooksObservesClaudeQwenProfile mirrors the minimax
// falsifier for the fifth managed harness: a genuinely installed claude-qwen
// binding must reconcile its mutation-covering PreToolUse to ready. Without the
// qwen profile settings path in the observation source table, the readiness
// gate reports the binding missing forever while the authority and the
// settings both have it.
func TestReconcileInstalledHooksObservesClaudeQwenProfile(t *testing.T) {
	compoundHome := t.TempDir()
	harnessHome := t.TempDir()
	t.Setenv("HOME", harnessHome)
	binaryPath := filepath.Join(harnessHome, ".local", "bin", "compound")
	writeHarnessHookConfig(t, binaryPath, "#!/bin/sh\nexit 0\n")

	report, err := hookplan.InstallHooks(harnessHome, binaryPath, "global", []string{"claude-qwen"}, false)
	if err != nil {
		t.Fatalf("InstallHooks(claude-qwen) error = %v", err)
	}
	if _, err := ImportHookInstallReport(
		compoundHome,
		harnessHome,
		report,
		time.Date(2026, 7, 31, 19, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("ImportHookInstallReport() error = %v", err)
	}

	got, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() error = %v", err)
	}
	observed := 0
	for _, binding := range got.Observation.Bindings {
		if binding.Harness == "claude_qwen" {
			observed++
		}
	}
	if observed != 5 {
		t.Fatalf("observed %d claude_qwen bindings, want 5 (observation source missing)", observed)
	}
	delta := hookReconciliationDelta(got.Reconciliation, "claude_qwen", "PreToolUse")
	if delta == nil || delta.State != hookplan.StateReady {
		t.Fatalf("claude_qwen PreToolUse reconciliation = %+v, want ready", delta)
	}
}

// TestObserveHarnessHookInstallationResolvesSwitchbackPublishedPaths pins the
// observation source table to the on-disk layout switchback actually publishes.
// Each claude harness's settings.json must live at the path the installer writes
// AND the path observeHarnessHookInstallation reads -- the two are contractually
// the same path, so a fixture dropped at the published path must be observed as
// a binding under that harness.
//
// The fifth harness claude_qwen used to point at "qwen-tap", a directory
// InstallHooks never creates -- the switchback published layout for qwen is
// ".switchback/config/claude/_providers/qwen/settings.json", matching the
// claude_minimax line directly above it. With the stale "qwen-tap" entry, the
// source list read a path that no installer ever writes, so the bindings were
// never observed and the blocking_hook_live readiness gate could never see them.
// The fix routes claude_qwen through "_providers/qwen". Against the old path,
// the claude_qwen fixture at _providers/qwen/settings.json is silently skipped
// (the source list looks for qwen-tap/settings.json) -- so the per-case
// subtest below catches the regression directly. The table-driven form makes
// the omission visible as a single missing case rather than a fuzzier
// end-to-end readiness gap.
func TestObserveHarnessHookInstallationResolvesSwitchbackPublishedPaths(t *testing.T) {
	harnessHome := t.TempDir()
	t.Setenv("HOME", harnessHome)

	cases := []struct {
		harness string
		relPath string // path relative to harnessHome where switchback publishes this harness's settings.json
	}{
		{harness: "claude_code", relPath: filepath.Join(".claude", "settings.json")},
		{harness: "claude_zai", relPath: filepath.Join(".config", "switchback", "claude", "zai-direct", "settings.json")},
		{harness: "claude_minimax", relPath: filepath.Join(".config", "switchback", "claude", "_providers", "minimax-lane", "settings.json")},
		{harness: "claude_qwen", relPath: filepath.Join(".config", "switchback", "claude", "_providers", "qwen", "settings.json")},
	}

	// Minimal settings.json body: parseObservedHarnessHooks produces one binding
	// per command. The exact handler command does not matter -- the harness
	// identity plus SourceRef on the resulting binding is what tells us which
	// observation source fired.
	fixture := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo fixture"}]}]}}`
	for _, c := range cases {
		writeHarnessHookConfig(t, filepath.Join(harnessHome, c.relPath), fixture)
	}

	// observeHarnessHookInstallation is package-private, but ReconcileInstalledHooks
	// (also in package contract) is the smallest existing seam that composes it
	// and exposes the snapshot via Observation.Bindings. No new API surface.
	compoundHome := t.TempDir()
	got, err := ReconcileInstalledHooks(compoundHome, harnessHome, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("ReconcileInstalledHooks() error = %v", err)
	}

	for _, c := range cases {
		c := c
		t.Run(c.harness, func(t *testing.T) {
			wantSourceRef := "harness://" + c.harness + "/settings"
			for _, binding := range got.Observation.Bindings {
				if binding.Harness == c.harness && binding.SourceRef == wantSourceRef {
					return
				}
			}
			t.Fatalf(
				"no observed binding with SourceRef=%q for harness %q (settings expected at %q under harnessHome; observation source table is missing or mis-routed)",
				wantSourceRef, c.harness, c.relPath,
			)
		})
	}
}
