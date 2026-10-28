package capgraph

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDoctorSkillSourceValidatesRawSkillFiles(t *testing.T) {
	root := t.TempDir()
	writeSkillSource(t, root, "pricing", `---
name: pricing
description: "Pricing workflow"
---
# pricing
`)
	writeSkillSource(t, root, "signup", `---
name: signup
description: "Signup workflow"
---
# signup
`)

	report, err := DoctorSkillSource(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.SkillFileCount != 2 || report.ErrorCount != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestDoctorSkillSourceRejectsMissingFrontmatterAndDuplicateNames(t *testing.T) {
	root := t.TempDir()
	writeSkillSource(t, root, "first", `---
name: duplicate
description: "First"
---
# first
`)
	writeSkillSource(t, root, "second", `---
name: duplicate
description: "Second"
---
# second
`)
	writeSkillSource(t, root, "broken", `# missing frontmatter
`)

	report, err := DoctorSkillSource(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "fail" {
		t.Fatalf("status = %s, want fail: %+v", report.Status, report)
	}
	if len(report.DuplicateNames) != 1 || report.DuplicateNames[0].Name != "duplicate" {
		t.Fatalf("duplicate names = %+v", report.DuplicateNames)
	}
	if report.ErrorCount < 3 {
		t.Fatalf("error count = %d, want at least 3: %+v", report.ErrorCount, report)
	}
}

// F11 — TestSkillSourceDoctorSupportsSkillsContainerLayout pins the
// regression-fixing slice-8 behaviour: layout-B (`root/skills/<name>/SKILL.md`)
// walks correctly, the `skills/` CONTAINER is NOT itself reported as a skill,
// Path is the SKILL.md-relative path ending in "SKILL.md" (matching the
// pre-slice-2 doctor shape that downstream consumers depend on), and a
// good layout-B entry reports Status=ok.
//
// On base (slice-2 still in place): the os.ReadDir-based enumeration saw
// `root/skills` itself as an entry and surfaced it as a "missing" skill
// (it has no SKILL.md), which made Status=fail. The failing cmd-side test
// `TestCapabilitySkillSourceDoctorReportsRawSkillSource` triggered this.
//
// Slice-2 of `TestSkillSourceDoctorFlagsDanglingSymlink` (a separate test
// in source_reachability_test.go) already pinned the F3 dangling behaviour
// for layout-A; F11 pins the F3-equivalent for layout-B plus the
// regression fix.
func TestSkillSourceDoctorSupportsSkillsContainerLayout(t *testing.T) {
	root := t.TempDir()
	// Single layout-B skill under root/skills/pricing/SKILL.md.
	writeSkillSource(t, root, "skills/pricing", `---
name: pricing
description: "Pricing workflow"
---
# pricing
`)

	report, err := DoctorSkillSource(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %s, want ok: %+v", report.Status, report)
	}
	if report.ErrorCount != 0 {
		t.Fatalf("error count = %d, want 0: %+v", report.ErrorCount, report)
	}
	if report.SkillFileCount != 1 {
		t.Fatalf("skill file count = %d, want 1: %+v", report.SkillFileCount, report)
	}
	// Files must contain exactly one entry whose Path is the SKILL.md file
	// (not the dir), matching the pre-slice-2 doctor output shape.
	var pricing *SkillSourceFileReport
	for i := range report.Files {
		if report.Files[i].Path == "skills/pricing/SKILL.md" {
			pricing = &report.Files[i]
		}
	}
	if pricing == nil {
		t.Fatalf("pricing entry missing; got %+v", report.Files)
	}
	if !pricing.Reachable || pricing.Status != "ok" || pricing.Reason != "ok" {
		t.Fatalf("pricing entry = %+v, want Reachable=true Status=ok Reason=ok", pricing)
	}
	if pricing.Name != "pricing" || pricing.Description != "Pricing workflow" {
		t.Fatalf("pricing entry name/desc wrong: %+v", pricing)
	}
	// The container itself must NOT appear as a skill.
	for i := range report.Files {
		if report.Files[i].Path == "skills/SKILL.md" || report.Files[i].Path == "skills" {
			t.Fatalf("container leaked into Files: %+v", report.Files[i])
		}
	}
}

// F12 — TestSkillSourceDoctorIgnoresNonSkillRealDirs pins the "not a skill"
// silent-skip rule: a real directory with no SKILL.md (a notes folder
// next to a real skill) does NOT fail the doctor, does NOT count as an
// error, does NOT show up in Files.
//
// On base the entry was reported as missing/fail because the old inspector
// had no "skip non-skill" branch.
func TestSkillSourceDoctorIgnoresNonSkillRealDirs(t *testing.T) {
	root := t.TempDir()
	writeSkillSource(t, root, "good", `---
name: good
description: "Good skill"
---
# good
`)
	// A real dir WITHOUT SKILL.md: should be silently skipped, not flagged.
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := DoctorSkillSource(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %s, want ok (notes is not a skill, not dangling): %+v", report.Status, report)
	}
	if report.ErrorCount != 0 {
		t.Fatalf("error count = %d, want 0: %+v", report.ErrorCount, report)
	}
	if report.SkillFileCount != 1 {
		t.Fatalf("skill file count = %d, want 1 (good only): %+v", report.SkillFileCount, report)
	}
	if len(report.Files) != 1 {
		t.Fatalf("files count = %d, want 1: %+v", len(report.Files), report.Files)
	}
	// None of the entries should mention "notes".
	for _, fr := range report.Files {
		if fr.Path == "notes/SKILL.md" || fr.Path == "notes" {
			t.Fatalf("notes leaked into Files: %+v", fr)
		}
	}
	// A real regular file (README.md) is also silently skipped.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".vscode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".vscode", "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = DoctorSkillSource(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.ErrorCount != 0 {
		t.Fatalf("README.md + .vscode/settings.json must not break the doctor: %+v", report)
	}
	if report.SkillFileCount != 1 {
		t.Fatalf("SkillFileCount = %d, want 1 (still just good)", report.SkillFileCount)
	}
}

func writeSkillSource(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
