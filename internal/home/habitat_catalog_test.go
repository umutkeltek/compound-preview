package home

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDoctorAndBackupRejectUnknownTopLevelRoot(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "local", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "invented-feature-root/state.json", `{"unowned":true}`)

	report := Doctor(h)
	if report.OK {
		t.Fatalf("doctor accepted an unowned top-level root: %+v", report)
	}

	_, err := Backup(h, filepath.Join(t.TempDir(), "backup.tar.gz"), archiveTestNow)
	if err == nil || !strings.Contains(err.Error(), "invented-feature-root") {
		t.Fatalf("backup must reject the same unowned root, got %v", err)
	}
}

func TestCompactPreservesBehaviorBearingProjectionExceptions(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "local", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	protected := []string{
		"projections/account-projection.json",
		"projections/narrate-thread-seen.json",
		"projections/orient-operator-last-seq.json",
		"projections/director/runs/run.json",
		"projections/source-intake/reports/report.json",
	}
	for _, rel := range protected {
		writeHomeFile(t, h, rel, "keep")
	}
	writeHomeFile(t, h, "projections/rebuildable-view.json", "drop")

	report, err := Compact(h, CompactOptions{Rebuildable: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.RemovedFiles != 1 {
		t.Fatalf("compact removed behavior-bearing state: %+v", report)
	}
	for _, rel := range protected {
		if _, err := os.Stat(filepath.Join(h, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("protected habitat %s was compacted: %v", rel, err)
		}
	}
	assertMissing(t, filepath.Join(h, "projections", "rebuildable-view.json"))
}

func TestBackupRestoresBehaviorBearingProjectionExceptionsOnly(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "local", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, h, "projections/account-projection.json", "keep")
	writeHomeFile(t, h, "projections/rebuildable-view.json", "drop")
	writeHomeFile(t, h, "close-plans/legacy-plan.json", "bridge")

	archivePath := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, err := Backup(h, archivePath, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), ".compound-restored")
	if _, err := RestoreArchive(archivePath, restored, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	if got := mustReadHomeFile(t, restored, "projections/account-projection.json"); got != "keep" {
		t.Fatalf("restored behavior-bearing projection = %q", got)
	}
	assertMissing(t, filepath.Join(restored, "projections", "rebuildable-view.json"))
	if got := mustReadHomeFile(t, restored, "close-plans/legacy-plan.json"); got != "bridge" {
		t.Fatalf("restored bridge body = %q", got)
	}
}

func TestVerifyArchiveRejectsUnknownHabitatBeforeRestore(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "unknown-root.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	entry, err := writeBytesEntry(tw, "invented-feature-root/state.json", []byte("unowned"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeArchiveManifest(tw, ArchiveManifest{
		SchemaVersion: ArchiveSchemaVersion,
		Kind:          ArchiveKindBackup,
		CreatedAt:     archiveTestNow.UTC().Format("2006-01-02T15:04:05Z"),
		Entries:       []ArchiveEntry{entry},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := VerifyArchive(archivePath); err == nil || !strings.Contains(err.Error(), "invented-feature-root") {
		t.Fatalf("archive verification accepted unknown habitat: %v", err)
	}
}

func TestHabitatCatalogClassifiesEveryKnownRootAndMakesBridgeDebtExplicit(t *testing.T) {
	entries := HabitatEntries()
	if len(entries) == 0 {
		t.Fatal("habitat catalog is empty")
	}
	knownRoots := []string{
		"action-execution-plans", "advance-plans", "archives", "artifacts", "bench-suites",
		"claim-plans", "close-plans", "config", "context", "continue-runs", "continues",
		"contracts", "create-plans", "credit-harvest", "decisions", "delegate-workspaces",
		"dispatch-advances", "dispatch-claims", "dispatch-plans", "dispatch-step-runs", "dispatch-steps", "distill",
		"events", "evidence", "exports", "imports", "inbox", "indexes", "lanebeliefs",
		"ledger", "lock", "locks", "logs", "observed", "packs", "panel-runs", "private",
		"projections", "registries", "reports", "reviewed-fixtures", "reviews",
		"run-observations", "runs", "runtime", "scripts", "snapshots", "social-profiles", "sources", "state", "tmp", "transactions", "work",
	}
	for _, root := range knownRoots {
		entry, ok := HabitatForPath(root)
		if !ok || entry.Prefix != root {
			t.Fatalf("known root %q is not owned by its catalog entry: %+v ok=%t", root, entry, ok)
		}
	}
	for _, entry := range entries {
		if entry.Status == HabitatStatusBridge && (entry.CanonicalPrefix == "" || entry.RetirementCondition == "") {
			t.Fatalf("bridge %s hides migration debt: %+v", entry.ID, entry)
		}
		if entry.Compact && entry.Class != HabitatClassRebuildable && entry.Class != HabitatClassTransient {
			t.Fatalf("behavior-bearing habitat is compactable: %+v", entry)
		}
		if strings.HasPrefix(entry.Prefix, "projections/") && entry.Backup && entry.Compact {
			t.Fatalf("protected projection cannot also be compactable: %+v", entry)
		}
	}
}

func TestHabitatCatalogOwnsPermissionActuatorCanonicalAndLegacyPaths(t *testing.T) {
	canonicalTransactions, ok := HabitatForPath("work/transactions/harness-permissions/claude_code.json")
	if !ok || canonicalTransactions.Prefix != "work/transactions" || canonicalTransactions.Status != HabitatStatusActive || canonicalTransactions.Class != HabitatClassOperational || !canonicalTransactions.Backup || canonicalTransactions.Compact || !canonicalTransactions.PlacementAllowed {
		t.Fatalf("canonical permission transaction habitat is not durable operational state: %+v ok=%t", canonicalTransactions, ok)
	}
	canonicalLock, ok := HabitatForPath("lock/harness-permissions.lock")
	if !ok || canonicalLock.Prefix != "lock" || canonicalLock.Status != HabitatStatusActive || canonicalLock.Class != HabitatClassTransient || !canonicalLock.PlacementAllowed {
		t.Fatalf("canonical permission lock habitat is not owned transient state: %+v ok=%t", canonicalLock, ok)
	}
	for _, tc := range []struct {
		path, prefix, canonical string
	}{
		{path: "locks/harness-permissions.lock", prefix: "locks", canonical: "lock"},
		{path: "transactions/harness-permissions/claude_code.json", prefix: "transactions", canonical: "work/transactions"},
	} {
		entry, ok := HabitatForPath(tc.path)
		if !ok || entry.Prefix != tc.prefix || entry.Status != HabitatStatusBridge || entry.CanonicalPrefix != tc.canonical || !entry.Backup || entry.Compact || entry.PlacementAllowed || entry.RetirementCondition == "" {
			t.Fatalf("legacy permission habitat %q hides compatibility debt: %+v ok=%t", tc.path, entry, ok)
		}
	}

	homePath := t.TempDir()
	writeHomeFile(t, homePath, "locks/harness-permissions.lock", "")
	writeHomeFile(t, homePath, "transactions/harness-permissions/claude_code.json", `{}`)
	inventory, err := InspectHabitat(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if !inventory.OK || !reflect.DeepEqual(inventory.BridgeRoots, []string{"locks", "transactions"}) {
		t.Fatalf("legacy permission roots are not explicit bridges: %+v", inventory)
	}
}

// The content-drafts inbox is auto-registered from the candidates leaf (one home
// per kind). Pin it as a durable operational inbox: RED if the kind is renamed or
// the auto-loop breaks (HabitatForPath misses), or if it is ever misclassified as
// compactable / non-backed-up — either would silently lose durable marketing drafts.
func TestHabitatCatalogOwnsContentDraftInbox(t *testing.T) {
	entry, ok := HabitatForPath("inbox/content-drafts/cdraft_synth.json")
	if !ok {
		t.Fatal("inbox/content-drafts is not owned by the habitat catalog")
	}
	if entry.Prefix != "inbox/content-drafts" {
		t.Fatalf("content-drafts habitat prefix = %q, want inbox/content-drafts", entry.Prefix)
	}
	if entry.Status != HabitatStatusActive || entry.Class != HabitatClassOperational {
		t.Fatalf("content-drafts inbox is not active operational: %+v", entry)
	}
	if !entry.Scaffold || !entry.Backup || !entry.PlacementAllowed || entry.Compact {
		t.Fatalf("content-drafts inbox must scaffold + back up + allow placement like sibling inbox children and never compact: %+v", entry)
	}
}

func TestHabitatCatalogOwnsCoordinationDeliveryUnderContext(t *testing.T) {
	for _, relative := range []string{
		"context/coordination-delivery",
		"context/coordination-delivery/reports/drep_fixture.json",
		"context/coordination-delivery/measurements/dmeasure_fixture.json",
		"context/coordination-delivery/cursors/cursor_fixture.json",
	} {
		entry, ok := HabitatForPath(relative)
		if !ok {
			t.Fatalf("coordination delivery path is unknown: %s", relative)
		}
		if entry.ID != "context.coordination-delivery" ||
			entry.Class != HabitatClassOperational || !entry.Backup || entry.Compact {
			t.Fatalf("coordination delivery habitat for %s = %+v", relative, entry)
		}
	}
	if _, ok := HabitatForPath("hook-delivery/report.json"); ok {
		t.Fatal("legacy hook-delivery top-level root must not be blessed")
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "local", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	writeHomeFile(t, homePath, "context/coordination-delivery/cursors/cursor.json", `{"semantic_revision":"sha256:fixture"}`)
	writeHomeFile(t, homePath, "context/coordination-delivery/reports/report.json", `{"delivered_bytes":42}`)
	archivePath := filepath.Join(t.TempDir(), "coordination-delivery.tar.gz")
	if _, err := Backup(homePath, archivePath, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), ".compound-restored")
	if _, err := RestoreArchive(archivePath, restored, archiveTestNow); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"context/coordination-delivery/cursors/cursor.json",
		"context/coordination-delivery/reports/report.json",
	} {
		if _, err := os.Stat(filepath.Join(restored, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("coordination delivery backup/restore lost %s: %v", relative, err)
		}
	}
}

func TestHabitatCatalogOwnsCanonicalSourceManifestsAndNamesLegacyRetirement(t *testing.T) {
	canonical, ok := HabitatForPath("sources/manifests/family.json")
	if !ok || canonical.Prefix != "sources/manifests" || canonical.Status != HabitatStatusActive || canonical.Class != HabitatClassEvidence || !canonical.Backup || canonical.Compact || !canonical.PlacementAllowed {
		t.Fatalf("canonical SourceManifest habitat is not durable evidence: %+v ok=%t", canonical, ok)
	}

	legacy, ok := HabitatForPath("registries/source-manifests/family.json")
	if !ok || legacy.Prefix != "registries/source-manifests" || legacy.Status != HabitatStatusBridge || legacy.CanonicalPrefix != "sources/manifests" || legacy.PlacementAllowed || legacy.RetirementCondition == "" {
		t.Fatalf("legacy SourceManifest path hides retirement debt: %+v ok=%t", legacy, ok)
	}
}

func TestHabitatCatalogOwnsPrivateArtifactCustodyCanonically(t *testing.T) {
	for _, tc := range []struct {
		path   string
		prefix string
		id     string
	}{
		{path: "private", prefix: "private", id: "private"},
		{path: "private/artifacts", prefix: "private/artifacts", id: "private.artifacts"},
		{path: "private/artifacts/sha256/example", prefix: "private/artifacts/sha256", id: "private.artifacts.sha256"},
	} {
		entry, ok := HabitatForPath(tc.path)
		if !ok || entry.ID != tc.id || entry.Prefix != tc.prefix || entry.Status != HabitatStatusActive || entry.Class != HabitatClassEvidence || !entry.Backup || entry.Compact {
			t.Fatalf("private custody habitat %q is not canonical durable evidence: %+v ok=%t", tc.path, entry, ok)
		}
		if entry.Scaffold {
			t.Fatalf("private custody habitat %q must not use generic 0755 scaffolding: %+v", tc.path, entry)
		}
		if entry.PlacementAllowed {
			t.Fatalf("private custody habitat %q must remain writer-restricted: %+v", tc.path, entry)
		}
	}
}

func TestHabitatCatalogProjectionsCannotMutateAuthority(t *testing.T) {
	entries := HabitatEntries()
	dirs := HabitatDirs()
	placementDirs := HabitatPlacementDirs()
	if len(entries) == 0 || len(dirs) == 0 || len(placementDirs) == 0 {
		t.Fatal("habitat catalog projections are empty")
	}

	entries[0].Prefix = "mutated-entry"
	dirs[0] = "mutated-dir"
	placementDirs[0] = "mutated-placement-dir"

	if _, ok := HabitatForPath("mutated-entry"); ok {
		t.Fatal("HabitatEntries returned mutable catalog authority")
	}
	for _, dir := range HabitatDirs() {
		if dir == "mutated-dir" {
			t.Fatal("HabitatDirs returned mutable catalog authority")
		}
	}
	for _, dir := range HabitatPlacementDirs() {
		if dir == "mutated-placement-dir" {
			t.Fatal("HabitatPlacementDirs returned mutable catalog authority")
		}
	}
}

func TestHabitatPathMakesLegacyWriterDebtExplicitAndRejectsTraversal(t *testing.T) {
	h := filepath.Join(string(os.PathSeparator), "owned", ".compound")
	got, err := HabitatPath(h, HabitatClosePlans, "plan.json")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(h, "close-plans", "plan.json")
	if got != want {
		t.Fatalf("close-plan habitat path = %q, want %q", got, want)
	}
	entry, ok := HabitatForPath("close-plans/plan.json")
	if !ok || entry.Status != HabitatStatusBridge || entry.CanonicalPrefix != "work/plans/close" {
		t.Fatalf("close-plan writer lost bridge debt: %+v ok=%t", entry, ok)
	}
	if _, err := HabitatPath(h, HabitatClosePlans, "../escape.json"); err == nil {
		t.Fatal("habitat path accepted traversal")
	}
}

// TestHabitatCatalogOwnsActuatorRoot: the governed outbound actuator writes
// durable operational state under actuator/ (budget ledger, outbox,
// signal-checks). The habitat catalog must own the root, or every home
// doctor run reports it as an unknown root and the habitat verdict goes
// red. Falsifier: removing the actuator entries from habitatEntries turns
// the InspectHabitat check RED with unknown_roots=["actuator"] — the live
// drift this pins was created when the actuator landed without its
// same-change catalog registration (2026-07-21).
// TestHabitatCatalogOwnsContentPostAuthorizations: S3 content-queue approve
// writes ContentPostAuthorization records under content-post-authorizations/;
// an unowned root reds home doctor and blocks archive-history repairs.
// Falsifier: removing the catalog entry makes InspectHabitat report the root
// as unknown (the same-change law the actuator violated on 2026-07-21).
func TestHabitatCatalogOwnsContentPostAuthorizations(t *testing.T) {
	entry, ok := HabitatForPath("content-post-authorizations/cdraft_0000.json")
	if !ok || entry.Prefix != "content-post-authorizations" || entry.Status != HabitatStatusActive || entry.Class != HabitatClassOperational || !entry.Backup || entry.Compact {
		t.Fatalf("content-post-authorizations habitat is not owned operational state: %+v ok=%t", entry, ok)
	}
	homePath := t.TempDir()
	writeHomeFile(t, homePath, "content-post-authorizations/cdraft_0000.json", `{}`)
	inventory, err := InspectHabitat(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.UnknownRoots) != 0 {
		t.Fatalf("content-post-authorizations must be a known root; unknown_roots=%v", inventory.UnknownRoots)
	}
}

func TestHabitatCatalogOwnsActuatorRoot(t *testing.T) {
	entry, ok := HabitatForPath("actuator/budget/2026-07-21.json")
	if !ok || entry.Prefix != "actuator/budget" || entry.Status != HabitatStatusActive || entry.Class != HabitatClassOperational || !entry.Backup || entry.Compact || !entry.PlacementAllowed {
		t.Fatalf("actuator budget habitat is not owned operational state: %+v ok=%t", entry, ok)
	}

	homePath := t.TempDir()
	writeHomeFile(t, homePath, "actuator/budget/2026-07-21.json", `{}`)
	writeHomeFile(t, homePath, "actuator/outbox/post_0001.json", `{}`)
	writeHomeFile(t, homePath, "actuator/signal-checks/check_0001.json", `{}`)
	writeHomeFile(t, homePath, "profiles/overlay.json", `{}`)
	writeHomeFile(t, homePath, "recovery/land_work_0000_deadbeef.tar", "")
	inventory, err := InspectHabitat(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.UnknownRoots) != 0 {
		t.Fatalf("actuator/, profiles/, recovery/ must be known roots; unknown_roots=%v", inventory.UnknownRoots)
	}
}

// TestDoctorRecognizesOwnedFeatureRoots: profiles (operator overlay), recovery
// (land/claim recovery archives), and the actuator outbox family are owned
// habitat roots, not foreign dirs. Falsifier: dropping any of their
// habitatCatalog entries turns this RED — InspectHabitat reports the root in
// UnknownRoots and the doctor fails, which is exactly the regression that broke
// `home doctor` (and every agent's hygiene gate) on a real home carrying a
// profiles overlay dir and recovery archives.
func TestDoctorRecognizesOwnedFeatureRoots(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".compound")
	if err := Init(h, "local", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	// Real content in each owned root, as the live features write it.
	writeHomeFile(t, h, "profiles/overlay.json", `{"agents":{}}`)
	writeHomeFile(t, h, "recovery/land_work_deadbeef.tar", "recovery-archive")
	writeHomeFile(t, h, "actuator/outbox/pending/move.json", `{"id":"mv_1"}`)

	report := Doctor(h)
	for _, root := range []string{"profiles", "recovery", "actuator"} {
		for _, unknown := range report.Habitat.UnknownRoots {
			if unknown == root {
				t.Fatalf("owned feature root %q reported unknown: %+v", root, report.Habitat)
			}
		}
	}
	if !report.HabitatOK {
		t.Fatalf("habitat not ok with owned feature roots present: %+v", report.Habitat)
	}
	if !report.OK {
		t.Fatalf("doctor not ok with owned feature roots present: %+v", report)
	}
}

// TestHabitatCatalogOwnsSocialProfilesRoot: one Chrome profile per online
// identity lives under social-profiles/<account_id> (account-identity skill,
// social-accounts registry). A profile carries live session cookies, so it is
// credential state: Backup MUST stay false and it must never scaffold or
// compact. Lived 2026-10-07: the first profile (cardintel-paddle) made home
// doctor report an unknown habitat, which refused compact, archive-history
// and make install. Falsifier: removing the catalog entry turns this RED.
func TestHabitatCatalogOwnsSocialProfilesRoot(t *testing.T) {
	entry, ok := HabitatForPath("social-profiles/cardintel-paddle/Default/Cookies")
	if !ok {
		t.Fatal("social-profiles/ is not owned by the habitat catalog")
	}
	if entry.ID != "social-profiles" || entry.Prefix != "social-profiles" || entry.Status != HabitatStatusActive || entry.Class != HabitatClassConfig {
		t.Fatalf("social-profiles habitat is not active configuration state: %+v", entry)
	}
	if entry.Backup {
		t.Fatalf("social-profiles habitat must never flow into backup bundles: %+v", entry)
	}
	if entry.Scaffold || entry.Compact {
		t.Fatalf("social-profiles habitat must not scaffold or compact: %+v", entry)
	}

	homePath := t.TempDir()
	writeHomeFile(t, homePath, "social-profiles/cardintel-paddle/Default/Cookies", `{}`)
	inventory, err := InspectHabitat(homePath)
	if err != nil {
		t.Fatalf("InspectHabitat: %v", err)
	}
	if len(inventory.UnknownRoots) != 0 || len(inventory.UnknownEntries) != 0 {
		t.Fatalf("social-profiles reported as unknown: roots=%v entries=%v", inventory.UnknownRoots, inventory.UnknownEntries)
	}
}

// TestHabitatCatalogOwnsSecretsRoot: live transport credentials are custodied
// under secrets/ (e.g. secrets/buzz/agent-compound-lane.json). An unregistered
// root reds home doctor — and with it every lane's battery — as unknown_root
// "secrets" (lived 2026-07-22: the console-projection-doors-smoke gate went red
// machine-wide on a live home carrying the buzz lane's credentials). Falsifier:
// removing the secrets catalog entry turns this RED — HabitatForPath misses and
// InspectHabitat reports the root as unknown. Backup MUST stay false:
// credentials never flow into backup bundles.
func TestHabitatCatalogOwnsSecretsRoot(t *testing.T) {
	entry, ok := HabitatForPath("secrets/buzz/agent-compound-lane.json")
	if !ok {
		t.Fatal("secrets/ is not owned by the habitat catalog")
	}
	if entry.ID != "secrets" || entry.Prefix != "secrets" || entry.Status != HabitatStatusActive || entry.Class != HabitatClassConfig {
		t.Fatalf("secrets habitat is not active configuration state: %+v", entry)
	}
	if entry.Backup {
		t.Fatalf("secrets habitat must never flow into backup bundles: %+v", entry)
	}
	if entry.Scaffold || entry.Compact {
		t.Fatalf("secrets habitat must not scaffold or compact: %+v", entry)
	}

	homePath := t.TempDir()
	writeHomeFile(t, homePath, "secrets/buzz/agent-compound-lane.json", `{}`)
	inventory, err := InspectHabitat(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.UnknownRoots) != 0 {
		t.Fatalf("secrets must be a known root; unknown_roots=%v", inventory.UnknownRoots)
	}
}

// TestHabitatCatalogOwnsLaneAutomationSurfaces: lane automation writes
// three surfaces under COMPOUND_HOME that no compound organ creates:
// delegate-workspaces/ (per-delegation lane workspaces), scripts/
// (babysit/triage tooling), and the nightly-continue receipt logs
// (top-level files appended by the launchd job). An unregistered root
// reds home doctor — lived 2026-08-05: `home doctor` reported all three
// as unknown on a live home, which blocked the terminal-work
// archive-history repair and made `make install` fail its hygiene gate.
// Falsifier: removing any catalog entry turns this RED — InspectHabitat
// reports the surface as unknown.
func TestHabitatCatalogOwnsLaneAutomationSurfaces(t *testing.T) {
	dirSurfaces := []struct {
		path  string
		id    string
		class HabitatClass
	}{
		{path: "delegate-workspaces/digest-lane-20260804/DISPATCH-PROMPT.md", id: "delegate-workspaces", class: HabitatClassOperational},
		{path: "scripts/babysit-lane.sh", id: "scripts", class: HabitatClassConfig},
	}
	for _, tc := range dirSurfaces {
		entry, ok := HabitatForPath(tc.path)
		if !ok {
			t.Fatalf("%s is not owned by the habitat catalog", tc.path)
		}
		if entry.Prefix != tc.id || entry.Status != HabitatStatusActive || entry.Class != tc.class {
			t.Fatalf("%s habitat is not active %s state: %+v", tc.id, tc.class, entry)
		}
		if entry.Scaffold || entry.Compact {
			t.Fatalf("%s must not scaffold or compact: %+v", tc.id, entry)
		}
		if !entry.Backup || !entry.PlacementAllowed {
			t.Fatalf("%s must back up and allow placement: %+v", tc.id, entry)
		}
	}
	for _, logFile := range []string{
		"nightly-continue.log",
		"nightly-continue.stderr.log",
		"nightly-continue.stdout.log",
	} {
		entry, ok := HabitatForPath(logFile)
		if !ok {
			t.Fatalf("%s is not owned by the habitat catalog", logFile)
		}
		if entry.Prefix != logFile || entry.Status != HabitatStatusActive || entry.Class != HabitatClassOperational {
			t.Fatalf("%s habitat is not active operational: %+v", logFile, entry)
		}
		if !entry.TopLevelFile || !entry.Backup {
			t.Fatalf("%s must be a backed-up top-level file: %+v", logFile, entry)
		}
	}

	homePath := t.TempDir()
	for _, rel := range []string{
		"delegate-workspaces/digest-lane/DISPATCH-PROMPT.md",
		"scripts/babysit-lane.sh",
		"nightly-continue.log",
		"nightly-continue.stderr.log",
		"nightly-continue.stdout.log",
	} {
		writeHomeFile(t, homePath, rel, "")
	}
	inventory, err := InspectHabitat(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.UnknownRoots) != 0 || len(inventory.UnknownEntries) != 0 {
		t.Fatalf("lane automation surfaces must be known; unknown_roots=%v unknown_entries=%v", inventory.UnknownRoots, inventory.UnknownEntries)
	}
	report := Doctor(homePath)
	if !report.HabitatOK {
		t.Fatalf("doctor habitat not ok with lane automation surfaces present: %+v", report.Habitat)
	}
}

// TestHabitatCatalogOwnsOperatorReturnQueue: the away-mode return queue lives
// at operator/return-queue.jsonl. An unregistered root reds home doctor, which
// blocks archive-history and `make install` -- so the first away flush on a
// live home would have broken both. Falsifier: removing the catalog entry turns
// this RED (operator reported as an unknown root).
func TestHabitatCatalogOwnsOperatorReturnQueue(t *testing.T) {
	entry, ok := HabitatForPath("operator/return-queue.jsonl")
	if !ok || entry.Prefix != "operator" || entry.Status != HabitatStatusActive || entry.Class != HabitatClassOperational {
		t.Fatalf("operator/ is not active operational state: %+v ok=%t", entry, ok)
	}
	if !entry.Backup || entry.Compact || entry.Scaffold {
		t.Fatalf("operator/ must back up, never compact, and not scaffold: %+v", entry)
	}
	homePath := t.TempDir()
	writeHomeFile(t, homePath, "operator/return-queue.jsonl", "")
	writeHomeFile(t, homePath, "operator/return-queue.lock", "")
	inventory, err := InspectHabitat(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.UnknownRoots) != 0 || len(inventory.UnknownEntries) != 0 {
		t.Fatalf("operator/ must be known; unknown_roots=%v unknown_entries=%v", inventory.UnknownRoots, inventory.UnknownEntries)
	}
	if report := Doctor(homePath); !report.HabitatOK {
		t.Fatalf("doctor habitat not ok with the return queue present: %+v", report.Habitat)
	}
}
