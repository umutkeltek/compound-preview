package authority

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/mem"
)

func TestImportPathAuthorityMergesMovedPathIntoSameEntity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(root, "new", "sample-mobile-app")
	stalePath := filepath.Join(root, "old", "sample-mobile-app")
	if err := os.MkdirAll(activePath, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "path-authority.yaml")
	body := strings.Join([]string{
		"schema_version: " + PathAuthoritySchemaVersion,
		"entity_ref: project:sample-mobile-app",
		"entity_kind: project",
		"aliases: [SampleMobileApp]",
		"authoritative_source: fixture-project-contract",
		"source_bindings:",
		"  - kind: git_remote",
		"    value: https://example.invalid/sample-mobile-app.git",
		"  - kind: bundle",
		"    value: com.example.samplemobile",
		"path_bindings:",
		"  - path: " + stalePath,
		"    status: stale",
		"  - path: " + activePath,
		"    status: active",
		"host_bindings:",
		"  - host: macbook",
		"    status: active",
		"evidence_refs: [fixture:path-authority]",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	existing := []mem.Entity{
		{
			Slug:        "project:sample-mobile-app",
			Kind:        "project",
			Aliases:     []string{"SampleMobileApp"},
			Identifiers: []string{"bundle:com.example.samplemobile", stalePath},
			Visibility:  "operational",
		},
	}
	_, entity, facts, report, err := ImportPathAuthority(path, existing)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.EntityRef != "project:sample-mobile-app" {
		t.Fatalf("report = %+v", report)
	}
	for _, want := range []string{"https://example.invalid/sample-mobile-app.git", "git_remote:https://example.invalid/sample-mobile-app.git", "com.example.samplemobile", stalePath, activePath, "host:macbook"} {
		if !hasString(entity.Identifiers, want) {
			t.Fatalf("identifiers missing %q: %v", want, entity.Identifiers)
		}
	}
	if len(facts) == 0 {
		t.Fatal("path authority should emit deterministic authority facts")
	}
}

func TestImportPathAuthorityRejectsBadStatus(t *testing.T) {
	pa := PathAuthority{
		SchemaVersion:       PathAuthoritySchemaVersion,
		EntityRef:           "project:x",
		EntityKind:          "project",
		AuthoritativeSource: "fixture",
		PathBindings:        []PathBinding{{Path: "/x", Status: "maybe"}},
		EvidenceRefs:        []string{"fixture"},
	}
	if err := validatePathAuthority(pa); err == nil {
		t.Fatal("bad path status should be rejected")
	}
}

func TestImportPathAuthorityRejectsRelativeActiveBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "path-authority.yaml")
	body := strings.Join([]string{
		"schema_version: " + PathAuthoritySchemaVersion,
		"entity_ref: source:example-os",
		"entity_kind: repo",
		"authoritative_source: direct-path-authority-test",
		"path_bindings:",
		"  - path: relative/example-os",
		"    status: active",
		"evidence_refs:",
		"  - test:relative-active",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := ImportPathAuthority(path, nil)
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("direct path-authority import must reject relative active current_path, got %v", err)
	}
}

func TestImportPathAuthorityCanonicalizesActiveSymlinkBinding(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real-root")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(root, "linked-root")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "path-authority.yaml")
	body := strings.Join([]string{
		"schema_version: " + PathAuthoritySchemaVersion,
		"entity_ref: source:linked-root",
		"entity_kind: repo",
		"authoritative_source: direct-path-authority-test",
		"path_bindings:",
		"  - path: " + linkRoot,
		"    status: active",
		"evidence_refs:",
		"  - test:linked-active",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	pa, _, facts, _, err := ImportPathAuthority(path, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := pa.PathBindings[0].Path; got != want {
		t.Fatalf("stored active path = %q, want canonical %q", got, want)
	}
	if !hasCandidate(facts, "source:linked-root", "current_path", want) {
		t.Fatalf("current_path did not use canonical path %q: %+v", want, facts)
	}
}

func TestImportPathAuthorityRejectsMissingActiveBinding(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing-root")
	path := filepath.Join(root, "path-authority.yaml")
	body := strings.Join([]string{
		"schema_version: " + PathAuthoritySchemaVersion,
		"entity_ref: source:missing-root",
		"entity_kind: repo",
		"authoritative_source: direct-path-authority-test",
		"path_bindings:",
		"  - path: " + missing,
		"    status: active",
		"  - path: " + filepath.Join(root, "missing-history"),
		"    status: retired",
		"evidence_refs:",
		"  - test:missing-active",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := ImportPathAuthority(path, nil)
	if err == nil || !strings.Contains(err.Error(), "active") || !strings.Contains(err.Error(), "missing-root") {
		t.Fatalf("missing active binding must fail while missing history remains allowed, got %v", err)
	}
}

func TestImportPathAuthorityRejectsMultipleActiveBindingsRegardlessOfOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			root := t.TempDir()
			first := filepath.Join(root, "first")
			second := filepath.Join(root, "second")
			for _, path := range []string{first, second} {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if reverse {
				first, second = second, first
			}
			path := filepath.Join(root, "path-authority.yaml")
			body := "schema_version: " + PathAuthoritySchemaVersion + "\nentity_ref: source:two-active\nentity_kind: repo\nauthoritative_source: test\npath_bindings:\n  - path: " + first + "\n    status: active\n  - path: " + second + "\n    status: active\nevidence_refs: [test]\n"
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, _, _, err := ImportPathAuthority(path, nil); err == nil || !strings.Contains(err.Error(), "at most one") {
				t.Fatalf("multiple active bindings must fail independent of order, got %v", err)
			}
		})
	}
}

func TestImportOSPathRegistryNormalizesActiveAndStaleRoots(t *testing.T) {
	accountHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(accountHome, "Projects", "control-spine", "root"),
		filepath.Join(accountHome, "brain"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registryPath := filepath.Join(t.TempDir(), "os-path-authority.yaml")
	registryBody := strings.Join([]string{
		"version: 1",
		"canonical_roots:",
		"  control_spine:",
		"    path: ~/Projects/control-spine/root",
		"    status: active",
		"  meaning_store:",
		"    path: ~/brain",
		"    status: active_index",
		"stale_or_compat_roots:",
		"  old_control_spine:",
		"    path: ~/Projects/old-control-spine",
		"    status: retired_archived",
		"  old_decisions:",
		"    path: ~/Desktop/decisions",
		"    status: missing_stale",
		"  folded_agent_context:",
		"    path: ~/Projects/control-spine/agent-context",
		"    status: retired_merged",
		"",
	}, "\n")
	if err := os.WriteFile(registryPath, []byte(registryBody), 0o644); err != nil {
		t.Fatal(err)
	}
	_, entities, facts, report, err := importOSPathRegistryWithAccountHome(
		registryPath,
		nil,
		func() (string, error) { return accountHome, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.EntityCount != 5 {
		t.Fatalf("report = %+v", report)
	}
	if len(entities) != 5 {
		t.Fatalf("entities = %d, want 5", len(entities))
	}
	control := findEntity(entities, "source:control-spine")
	if control.Slug == "" {
		t.Fatal("missing control spine entity")
	}
	controlPath := filepath.Join(accountHome, "Projects", "control-spine", "root")
	if !hasString(control.Identifiers, controlPath) {
		t.Fatalf("control identifiers = %v", control.Identifiers)
	}
	if !hasCandidate(facts, "source:control-spine", "current_path", controlPath) {
		t.Fatalf("active root did not create current_path claim: %+v", facts)
	}
	oldControlPath := filepath.Join(accountHome, "Projects", "old-control-spine")
	if hasCandidate(facts, "source:old-control-spine", "current_path", oldControlPath) {
		t.Fatal("retired compatibility root must not create current_path")
	}
	if !hasCandidate(facts, "source:old-control-spine", "known_path_retired", oldControlPath) {
		t.Fatalf("retired root did not create known_path_retired claim: %+v", facts)
	}
	if !hasCandidate(facts, "source:old-decisions", "known_path_stale", filepath.Join(accountHome, "Desktop", "decisions")) {
		t.Fatalf("missing_stale root did not create known_path_stale claim: %+v", facts)
	}
}

// TestImportOSPathRegistryPrefersResolverOverProcessHOME is the environment-INDEPENDENT
// half of the HOME-spoofing guarantee: `~` must expand through the account-home resolver,
// never through $HOME. It injects the resolver through the existing seam, so it needs no
// OS account and runs everywhere — including the CI container, where the sibling test
// below cannot run at all.
//
// This split exists because the two properties were fused in one test that required an
// account, which made the WHOLE guarantee unobservable in CI. The valuable half — that
// import consults the resolver rather than the environment — never needed an account.
//
// Falsifier: make importOSPathRegistryWithAccountHome expand `~` from os.Getenv("HOME")
// and this goes RED, because the spoofed HOME would win over the resolver.
func TestImportOSPathRegistryPrefersResolverOverProcessHOME(t *testing.T) {
	// Compare against the CANONICAL forms: import resolves symlinks, so on macOS a
	// /var/folders temp dir is recorded as /private/var/folders. Asserting the raw
	// t.TempDir() string fails on a path the import handled correctly.
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spoofed, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", spoofed)

	path := filepath.Join(t.TempDir(), "path-authority.yaml")
	body := "version: 1\ncanonical_roots:\n  account_home_probe:\n    path: \"~\"\n    status: active\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, facts, _, importErr := importOSPathRegistryWithAccountHome(path, nil, func() (string, error) {
		return resolved, nil
	})
	if importErr != nil {
		t.Fatalf("import: %v", importErr)
	}
	if !hasCandidate(facts, "source:account-home-probe", "current_path", resolved) {
		t.Fatalf("`~` must expand through the resolver (%q), not $HOME (%q); facts=%+v", resolved, spoofed, facts)
	}
	if hasCandidate(facts, "source:account-home-probe", "current_path", spoofed) {
		t.Fatalf("spoofed $HOME %q leaked into the imported path; facts=%+v", spoofed, facts)
	}
}

// TestImportOSPathRegistryUsesAccountHomeWhenProcessHOMEIsSpoofed covers the remaining,
// environment-DEPENDENT half: that the REAL resolver reads the OS account rather than the
// environment. It needs an actual account, so it skips where there is none.
//
// The skip is not a weakened gate — it is an absent precondition. `user.Current()` fails
// in a container whose running uid has no /etc/passwd entry (the CI runner executes this
// step as 1000:1000), and "the account home differs from $HOME" is not a claim that can be
// made when there is no account. Note what is deliberately NOT done here: giving
// currentAccountHome a $HOME fallback would make this pass everywhere and destroy the
// guarantee, since the function exists precisely because $HOME is spoofable.
func TestImportOSPathRegistryUsesAccountHomeWhenProcessHOMEIsSpoofed(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Skipf("no OS account for this uid (%v); the resolver-vs-$HOME half is covered by TestImportOSPathRegistryPrefersResolverOverProcessHOME", err)
	}
	want, err := filepath.EvalSymlinks(account.HomeDir)
	if err != nil {
		t.Fatalf("canonical account home: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "path-authority.yaml")
	body := "version: 1\ncanonical_roots:\n  account_home_probe:\n    path: \"~\"\n    status: active\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, facts, _, err := ImportOSPathRegistry(path, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !hasCandidate(facts, "source:account-home-probe", "current_path", want) {
		t.Fatalf("HOME spoof changed imported account path; want canonical account home %q, facts=%+v", want, facts)
	}
}

func TestImportOSPathRegistryRejectsRelativeActiveRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "path-authority.yaml")
	body := "version: 1\ncanonical_roots:\n  relative_root:\n    path: relative/root\n    status: active\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := importOSPathRegistryWithAccountHome(path, nil, func() (string, error) {
		return t.TempDir(), nil
	})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative active root must fail closed, got %v", err)
	}
}

func TestImportOSPathRegistryCanonicalizesExistingSymlinkRoot(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real-root")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(root, "linked-root")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "path-authority.yaml")
	body := "version: 1\ncanonical_roots:\n  linked_root:\n    path: " + linkRoot + "\n    status: active\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, facts, _, err := importOSPathRegistryWithAccountHome(path, nil, func() (string, error) {
		return root, nil
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !hasCandidate(facts, "source:linked-root", "current_path", wantRoot) {
		t.Fatalf("existing symlink root was not canonicalized to %q: %+v", wantRoot, facts)
	}
}

func TestImportOSPathRegistryRejectsMissingActiveRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "path-authority.yaml")
	missing := filepath.Join(root, "missing-active-root")
	body := "version: 1\ncanonical_roots:\n  ordinary_active_root:\n    path: " + missing + "\n    status: active\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, _, _, err := importOSPathRegistryWithAccountHome(path, nil, func() (string, error) {
		return root, nil
	})
	if err == nil || !strings.Contains(err.Error(), "ordinary_active_root") || !strings.Contains(err.Error(), "existing") {
		t.Fatalf("every missing active root must fail closed, got %v", err)
	}
}

func TestImportOSPathRegistryRejectsActiveRegularFileRoot(t *testing.T) {
	root := t.TempDir()
	fileRoot := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(fileRoot, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(root, "registry.yaml")
	if err := os.WriteFile(registry, []byte("version: 1\ncanonical_roots:\n  file_root:\n    path: "+fileRoot+"\n    status: active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := ImportOSPathRegistry(registry, nil); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("active OS root backed by a regular file must fail, got %v", err)
	}
}

func TestImportOSPathRegistryRejectsUnknownStatus(t *testing.T) {
	path := t.TempDir() + "/path-authority.yaml"
	data := []byte("version: 1\ncanonical_roots:\n  x:\n    path: ~/x\n    status: mystery\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := ImportOSPathRegistry(path, nil); err == nil {
		t.Fatal("unknown OS path status should be rejected")
	}
}

func hasString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func findEntity(values []mem.Entity, slug string) mem.Entity {
	for _, v := range values {
		if v.Slug == slug {
			return v
		}
	}
	return mem.Entity{}
}

func hasCandidate(values []mem.Candidate, entity, predicate, object string) bool {
	for _, v := range values {
		if v.Entity == entity && v.Predicate == predicate && v.Object == object {
			return true
		}
	}
	return false
}
