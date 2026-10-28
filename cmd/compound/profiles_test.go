package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/profiles"
)

func TestProfilesGenerateMatchesSourceAndCheckIsGreen(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	catalog, err := profiles.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdProfiles([]string{"generate"})
	})
	if !strings.Contains(out, "Generated ") {
		t.Fatalf("generate output missing summary:\n%s", out)
	}

	targets := profiles.TargetsForHome(homeDir)
	for _, role := range catalog.Roles {
		for _, harness := range role.Emit {
			path := filepath.Join(targets[harness], role.ID+".md")
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read generated %s: %v", path, err)
			}
			want := profiles.Render(role, harness)
			if string(got) != want {
				t.Fatalf("generated %s differs from source\nwant:\n%s\ngot:\n%s", path, want, string(got))
			}
		}
	}

	checkOut := captureStdout(t, func() {
		cmdProfiles([]string{"check"})
	})
	if !strings.Contains(checkOut, "IN SYNC — no drift") {
		t.Fatalf("check output should be green:\n%s", checkOut)
	}
}

func TestProfilesCheckDetectsDriftWithoutMutation(t *testing.T) {
	homeDir := t.TempDir()
	catalog, err := profiles.LoadCatalogWithOptions(profiles.LoadOptions{OverlayPaths: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	targets := profiles.TargetsForHome(homeDir)
	if _, err := profiles.Sync(catalog.Roles, targets, profiles.SyncOptions{}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(targets[profiles.HarnessCodex], "executor.md")
	tampered := []byte("compound-profile: generated\nTAMPERED\n")
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := profiles.Sync(catalog.Roles, targets, profiles.SyncOptions{Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Drift == 0 {
		t.Fatalf("check should detect drift: %+v", report)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(tampered) {
		t.Fatalf("check mutated tampered file:\n%s", string(got))
	}
}

func TestProfilesInstalledOnlyCheckSkipsMissingButDetectsGeneratedDrift(t *testing.T) {
	homeDir := t.TempDir()
	catalog, err := profiles.LoadCatalogWithOptions(profiles.LoadOptions{OverlayPaths: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	targets := profiles.TargetsForHome(homeDir)
	missing, err := profiles.Sync(catalog.Roles, targets, profiles.SyncOptions{Check: true, InstalledOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Drift != 0 || !strings.Contains(profiles.FinalLine(missing), "IN SYNC") {
		t.Fatalf("installed-only check should ignore missing generated profiles: %+v", missing)
	}

	if _, err := profiles.Sync(catalog.Roles, targets, profiles.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(targets[profiles.HarnessCodex], "executor.md")
	if err := os.WriteFile(path, []byte("compound-profile: generated\nDRIFT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drift, err := profiles.Sync(catalog.Roles, targets, profiles.SyncOptions{Check: true, InstalledOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if drift.Drift == 0 {
		t.Fatalf("installed-only check should detect drift in installed generated profile: %+v", drift)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "compound-profile: generated\nDRIFT\n" {
		t.Fatalf("installed-only check mutated drifted profile:\n%s", string(got))
	}
}

func TestProfilesRolesOnlyFallbackIsPublicSafe(t *testing.T) {
	catalog, err := profiles.LoadCatalogWithOptions(profiles.LoadOptions{OverlayPaths: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Roles) != 24 {
		t.Fatalf("roles-only catalog has %d roles, want 24", len(catalog.Roles))
	}

	var rendered strings.Builder
	for _, role := range catalog.Roles {
		for _, harness := range role.Emit {
			rendered.WriteString(profiles.Render(role, harness))
		}
	}
	if !strings.Contains(rendered.String(), profiles.GenMarker) {
		t.Fatal("roles-only render missing generated marker")
	}
}
