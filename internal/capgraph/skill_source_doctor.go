package capgraph

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const SkillSourceDoctorSchemaVersion = "compound.skill_source_doctor.v0.1"

type SkillSourceDoctorReport struct {
	SchemaVersion  string                     `json:"schema_version"`
	Root           string                     `json:"root"`
	Status         string                     `json:"status"`
	SkillFileCount int                        `json:"skill_file_count"`
	ErrorCount     int                        `json:"error_count"`
	Checks         []SkillSourceDoctorCheck   `json:"checks,omitempty"`
	Files          []SkillSourceFileReport    `json:"files,omitempty"`
	DuplicateNames []SkillSourceDuplicateName `json:"duplicate_names,omitempty"`
	// Dangling lists the entries that were reported as dangling_symlink.
	// Distinct from the Files list because dangling entries have no SKILL.md
	// and therefore no frontmatter name to dedupe on; the doctor surfaces
	// them here so the operator can see what is unreachable on disk WITHOUT
	// having to scan Files for reason="dangling_symlink" themselves.
	Dangling []string `json:"dangling,omitempty"`
}

type SkillSourceDoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type SkillSourceFileReport struct {
	// Path is the SKILL.md path relative to the root, ending in "SKILL.md"
	// for layout-A (root/<name>/SKILL.md) and layout-B
	// (root/skills/<name>/SKILL.md). Matches the shape the pre-slice-2 /
	// pre-slice-8 doctor produced, so downstream consumers (CLI renderer,
	// ApplySkillManifest slice-2 stamping) see the same path spelling.
	Path        string `json:"path"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	// Reachable is true iff the skill's dir AND its SKILL.md both resolve on
	// disk (Lstat+Stat dance: a real dir, a non-dangling symlink, or a
	// symlink whose target resolves and is the expected kind). False means
	// the skill CANNOT fire from disk as of this report, regardless of any
	// ConnectionSurface facts the graph carries. ComputeReachability
	// counts reachability as `Source == nil || Source.Reachable`.
	Reachable bool `json:"reachable"`
	// Reason names WHICH step failed:
	//
	//	ok                entry resolved, SKILL.md present and well-formed
	//	dangling_symlink  symlink whose target is gone (Lstat-success, Stat-fail)
	//	missing           symlink-to-dir-without-SKILL.md (intended skill, broken wiring)
	//	not_a_skill       entry is structurally ineligible (real file or real
	//	                  dir with no SKILL.md) — silent skip; not an error,
	//	                  not dangling, just not part of this manifest
	//	unreadable        SKILL.md present but os.ReadFile failed
	//	no_frontmatter    SKILL.md present but frontmatter malformed/missing
	Reason string   `json:"reason,omitempty"`
	Errors []string `json:"errors,omitempty"`
}

type SkillSourceDuplicateName struct {
	Name  string   `json:"name"`
	Paths []string `json:"paths"`
}

// DoctorSkillSource walks both supported skill-source layouts simultaneously
// (`root/<name>/SKILL.md` AND `root/skills/<name>/SKILL.md`) and reports
// the reachability of each. Real directories with no SKILL.md are silently
// ignored — they are not skills, not dangling, just not part of the
// manifest. Symlinks whose target resolves to a dir without SKILL.md are
// reported as "missing" (the author intended a skill there, the wiring is
// broken). Symlinks whose target is gone are "dangling_symlink". The
// layout-B container `root/skills/` is itself excluded from the report
// (it's a directory OF skills, not one).
func DoctorSkillSource(root string) (SkillSourceDoctorReport, error) {
	root = filepath.Clean(root)
	entries, err := skillSourceEntries(root)
	if err != nil {
		return SkillSourceDoctorReport{}, err
	}
	report := SkillSourceDoctorReport{
		SchemaVersion: SkillSourceDoctorSchemaVersion,
		Root:          root,
		Status:        "ok",
	}
	addCheck := func(name string, ok bool, detail string) {
		status := "ok"
		if !ok {
			status = "fail"
			report.Status = "fail"
		}
		report.Checks = append(report.Checks, SkillSourceDoctorCheck{Name: name, Status: status, Detail: detail})
	}
	addCheck("skill_files", len(entries) > 0, fmt.Sprintf("%d", len(entries)))
	// SkillFileCount is the count of valid skill entries in the report's
	// Files list (NOT the count of candidates inspected). A real-dir with
	// no SKILL.md (sibling notes folder, layout container, etc.) is
	// silently skipped — it never claimed to be a skill — so it does
	// not count. This matches the pre-slice-2 doctor behaviour where the
	// glob `root/*/SKILL.md` only matched entries that had a SKILL.md,
	// making SkillFileCount==len(Files) by construction.
	report.SkillFileCount = 0 // assigned below, after the loop filter

	namePaths := map[string][]int{}
	for _, absDir := range entries {
		fileReport := inspectSkillSourceEntry(root, absDir)
		// "not_a_skill" means the entry is structurally ineligible (real
		// file in a skill layout, or a real dir with no SKILL.md inside).
		// These are not skills and not dangling: drop them silently so
		// they don't fail the doctor or appear in Files. The author may
		// have created the dir mid-edit; we don't penalise that.
		if fileReport.Reason == "not_a_skill" {
			continue
		}
		if fileReport.Status != "ok" {
			report.Status = "fail"
			report.ErrorCount += len(fileReport.Errors)
		}
		if !fileReport.Reachable && fileReport.Reason == "dangling_symlink" {
			report.Dangling = append(report.Dangling, fileReport.Path)
		}
		if strings.TrimSpace(fileReport.Name) != "" {
			namePaths[fileReport.Name] = append(namePaths[fileReport.Name], len(report.Files))
		}
		report.Files = append(report.Files, fileReport)
	}
	sort.Strings(report.Dangling)

	for name, indexes := range namePaths {
		if len(indexes) < 2 {
			continue
		}
		dup := SkillSourceDuplicateName{Name: name}
		for _, idx := range indexes {
			path := report.Files[idx].Path
			report.Files[idx].Status = "fail"
			report.Files[idx].Errors = append(report.Files[idx].Errors, "duplicate skill name")
			dup.Paths = append(dup.Paths, path)
			report.ErrorCount++
		}
		sort.Strings(dup.Paths)
		report.DuplicateNames = append(report.DuplicateNames, dup)
		report.Status = "fail"
	}
	sort.Slice(report.DuplicateNames, func(i, j int) bool {
		return report.DuplicateNames[i].Name < report.DuplicateNames[j].Name
	})
	addCheck("duplicate_names", len(report.DuplicateNames) == 0, fmt.Sprintf("%d", len(report.DuplicateNames)))
	addCheck("dangling_entries", len(report.Dangling) == 0, fmt.Sprintf("%d", len(report.Dangling)))
	report.SkillFileCount = len(report.Files)
	return report, nil
}

// DoctorSkillSourceEntry is the per-entry surface the import path uses
// (ApplySkillManifest stamps Node.Source from its result). Behaviour
// matches DoctorSkillSource for one entry.
func DoctorSkillSourceEntry(root, relDir string) SkillSourceFileReport {
	absDir := filepath.Join(root, relDir)
	return inspectSkillSourceEntry(root, absDir)
}

// skillSourceEntries lists every candidate skill entry under root. Both
// supported layouts are walked:
//   - root/<name>           (layout A: direct)
//   - root/skills/<name>    (layout B: nested, container is `skills/`)
//
// The layout-B container itself is NEVER added as a candidate: it is a
// directory OF skill dirs, not a skill. Regular files under either
// root or root/skills/ are not candidates either — a README.md or
// stray .DS_Store isn't a skill.
//
// Whatever remains is either:
//   - a real directory (may or may not contain SKILL.md — the inspector
//     decides "ok" vs "not_a_skill" per entry), or
//   - a symlink (whose lstat+stat dance classifies dangling / missing
//     / ok per the rules in inspectSkillSourceEntry).
func skillSourceEntries(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if filepath.Base(root) != "SKILL.md" {
			return nil, fmt.Errorf("skill source file must be named SKILL.md: %s", root)
		}
		return []string{filepath.Dir(root)}, nil
	}
	seen := map[string]bool{}
	var dirs []string
	skillContainer := filepath.Join(root, "skills")

	// Scan both roots; a single helper keeps the skip-container rule and
	// the regular-file filter in one place.
	addNonFile := func(parent string, skipContainer bool) error {
		dh, err := os.ReadDir(parent)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, de := range dh {
			abs := filepath.Join(parent, de.Name())
			if skipContainer && abs == skillContainer {
				// Layout-B container: do not surface as a skill.
				continue
			}
			di, err := de.Info()
			if err != nil {
				continue
			}
			// Regular files (READMEs, lock files, .DS_Store, etc.) are
			// never skill dirs. Keep dirs and symlinks; the inspector
			// decides between real-dir-no-SKILL.md (not_a_skill) and
			// the rest.
			if di.Mode().IsRegular() {
				continue
			}
			if seen[abs] {
				continue
			}
			seen[abs] = true
			dirs = append(dirs, abs)
		}
		return nil
	}

	if err := addNonFile(root, true); err != nil {
		return nil, err
	}
	// Layout B: only meaningful if the container is itself a directory.
	// Skip silently when absent (NOT an error — that's the layout-A case).
	if ci, err := os.Stat(skillContainer); err == nil && ci.IsDir() {
		if err := addNonFile(skillContainer, false); err != nil {
			return nil, err
		}
	}

	sort.Strings(dirs)
	return dirs, nil
}

// inspectSkillSourceEntry is the canonical reachability inspector. It
// walks the Lstat+Stat dance on the entry itself AND its SKILL.md, and
// returns a SkillSourceFileReport whose Reason names exactly what
// happened (if anything). It is the only place reachability is derived
// for skill nodes; DoctorSkillSource, DoctorSkillSourceEntry, and
// ApplySkillManifest all share it. See Reason vocabulary on
// SkillSourceFileReport.
func inspectSkillSourceEntry(root, absDir string) SkillSourceFileReport {
	rel, err := filepath.Rel(root, absDir)
	if err != nil {
		rel = absDir
	}
	report := SkillSourceFileReport{Path: filepath.Join(rel, "SKILL.md"), Status: "fail", Reachable: false}
	absFile := filepath.Join(absDir, "SKILL.md")

	// 1) Lstat the entry itself.
	linfo, lerr := os.Lstat(absDir)
	if lerr != nil {
		report.Reason = "missing"
		return report
	}
	mode := linfo.Mode()

	// 1a) Regular file (README.md, .DS_Store, lockfile, etc.) → silent
	// "not a skill". These are siblings of skill dirs, not candidates.
	if mode.IsRegular() {
		report.Reason = "not_a_skill"
		return report
	}

	// 2) Symlink? Stat the target to tell dangling from resolved.
	if mode&os.ModeSymlink != 0 {
		if sinfo, serr := os.Stat(absDir); serr != nil || !sinfo.IsDir() {
			report.Reason = "dangling_symlink"
			return report
		}
		// symlink → real dir: fall through to the SKILL.md check below.
	} else if !mode.IsDir() {
		// neither regular file nor symlink nor directory → ineligible.
		report.Reason = "not_a_skill"
		return report
	}

	// 3) The entry IS (or resolves to) a directory. Check for SKILL.md.
	flinfo, ferr := os.Lstat(absFile)
	if ferr != nil {
		// Real dir without SKILL.md — the operator may simply be mid-edit
		// or the dir is a non-skill neighbour. Per the spec, this is
		// NOT dangling — it never claimed to be a skill. Silent skip.
		report.Reason = "not_a_skill"
		return report
	}
	if flinfo.Mode()&os.ModeSymlink != 0 {
		if _, fserr := os.Stat(absFile); fserr != nil {
			report.Reason = "dangling_symlink"
			return report
		}
	}
	data, rerr := os.ReadFile(absFile)
	if rerr != nil {
		report.Reason = "unreadable"
		report.Errors = append(report.Errors, rerr.Error())
		return report
	}
	frontmatter, ferr := skillSourceFrontmatter(data)
	if ferr != nil {
		report.Reason = "no_frontmatter"
		report.Errors = append(report.Errors, ferr.Error())
		return report
	}
	var fields map[string]any
	if uerr := yaml.Unmarshal([]byte(frontmatter), &fields); uerr != nil || fields == nil {
		report.Reason = "no_frontmatter"
		if uerr != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("YAML: %v", uerr))
		} else {
			report.Errors = append(report.Errors, "front matter YAML mapping")
		}
		return report
	}
	report.Name = skillSourceStringField(fields, "name")
	report.Description = skillSourceStringField(fields, "description")
	if report.Name == "" {
		report.Reason = "no_frontmatter"
		report.Errors = append(report.Errors, "missing non-empty string field: name")
		return report
	}
	if report.Description == "" {
		report.Reason = "no_frontmatter"
		report.Errors = append(report.Errors, "missing non-empty string field: description")
		return report
	}
	report.Status = "ok"
	report.Reachable = true
	report.Reason = "ok"
	return report
}

func skillSourceFrontmatter(data []byte) (string, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return "", fmt.Errorf("front matter must start on first line with ---")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), nil
		}
	}
	return "", fmt.Errorf("front matter must close with ---")
}

func skillSourceStringField(fields map[string]any, name string) string {
	value, ok := fields[name]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}
