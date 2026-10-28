package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LoadSkillDirManifest derives a SkillManifest from a directory of skill
// entries (one subdir per skill, each with SKILL.md). Dangling entries are
// INCLUDED with status="needs_review" and SkillPath set, so the slice-2 import
// path stamps Node.Source.Reachable=false on them -- a missing or broken
// skill is no longer phantom-reachable just because its id is named in the
// registry. The manifest's SourceRef.Path is the directory root so a future
// import re-derives the manifest from disk.
//
// This is the door that brings the live `~/.agents/skills` tree (which lives
// outside any quarantine, unlike the spine/views/skill-manifest.json the
// addendum cites) into the registry under the existing ApplySkillManifest
// path. Slice 5 of the addendum keeps cmd/compound's role to one line:
// `LoadSkillManifestForPath(p[0], now)` instead of `LoadSkillManifest(p[0])`,
// so the IsDir branch earns its ≤3 LOC budget.
func LoadSkillDirManifest(root string, now time.Time) (SkillManifest, error) {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return SkillManifest{}, err
	}
	if !info.IsDir() {
		return SkillManifest{}, fmt.Errorf("skill dir root %s is not a directory", root)
	}
	if now.IsZero() {
		now = time.Now()
	}
	entries, err := skillSourceEntries(root)
	if err != nil {
		return SkillManifest{}, err
	}
	if len(entries) == 0 {
		return SkillManifest{}, fmt.Errorf("skill dir root %s has no entries", root)
	}
	// Revision is a stable content manifest, not a hash of the root path.
	// Provenance must identify what was observed: relative path, file type,
	// content hash, and symlink target. Moving identical contents preserves
	// identity; changing bytes beneath the same root moves it.
	revision, err := skillDirContentRevision(root)
	if err != nil {
		return SkillManifest{}, err
	}
	evidenceRef := "skill_dir:" + revision[:19]
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest_dir_" + revision[7:19],
		AdapterID:     "compound.skill-dir",
		SourceSystem:  "skill-dir",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: SkillSourceRef{
			Path:        root,
			Kind:        "skill_dir",
			Owner:       "capability-graph",
			SourceClass: "skill-dir",
			Revision:    revision,
		},
		EvidenceRefs: []string{evidenceRef},
	}
	seen := map[string]bool{}
	for _, absDir := range entries {
		report := inspectSkillSourceEntry(root, absDir)
		// Skill id: always "skill:<slug of dir basename>" regardless of
		// frontmatter so the run is idempotent (a re-import collapses to
		// one row per id thanks to slice 1's upsert gate). We use the
		// SLUG of the basename rather than the frontmatter name -- a
		// retitled SKILL.md doesn't migrate the id.
		id := "skill:" + slugSkill(filepath.Base(absDir))
		if seen[id] {
			// Skip duplicates (defensive -- skillSourceEntries dedupes by
			// absolute path already; this guards against slug collisions
			// across distinct dirs).
			continue
		}
		seen[id] = true
		// Display name: prefer frontmatter, fall back to dir basename.
		name := strings.TrimSpace(report.Name)
		if name == "" {
			name = filepath.Base(absDir)
		}
		// status: needs_review for unreachable entries so the
		// valSurface-muted branch (status=needs_review keeps the skill off
		// surfacing per existing TestApplySkillManifestDoesNotSurfaceNeedsReviewSkill)
		// is what a fresh dangling entry gets. Real entries default to active.
		status := "active"
		if !report.Reachable {
			status = "needs_review"
		}
		// Triggers: derive from name tokens (skillTokenList filters
		// stop-words + dedupes; same pipeline the other loaders use so
		// triggering behaviour is consistent).
		triggers := skillTokenList(name, filepath.Base(absDir))
		skill := SkillManifestEntry{
			ID:           id,
			Kind:         "skill",
			Name:         name,
			Status:       status,
			Triggers:     triggers,
			EvidenceRefs: []string{evidenceRef},
			SkillPath:    absDir,
		}
		manifest.Skills = append(manifest.Skills, skill)
	}
	sort.SliceStable(manifest.Skills, func(i, j int) bool {
		return manifest.Skills[i].ID < manifest.Skills[j].ID
	})
	return manifest, nil
}

func skillDirContentRevision(root string) (string, error) {
	var rows []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if entry.Type()&os.ModeSymlink != 0 {
				target, targetErr := os.Readlink(path)
				if targetErr != nil {
					return targetErr
				}
				rows = append(rows, "L\t"+rel+"\t"+target)
				return nil
			}
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			rows = append(rows, "L\t"+rel+"\t"+target)
		case info.IsDir():
			rows = append(rows, "D\t"+rel)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			rows = append(rows, "F\t"+rel+"\t"+hex.EncodeToString(sum[:]))
		default:
			rows = append(rows, "O\t"+rel+"\t"+info.Mode().String())
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("manifest skill directory %s: %w", root, err)
	}
	sort.Strings(rows)
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// LoadSkillManifestForPath routes by file type: a directory goes through
// LoadSkillDirManifest; anything else falls through to LoadSkillManifest
// (the YAML manifest convention). The CLI verb `compound capability
// import-skills <path>` uses this dispatch so the IsDir branch earns its
// ≤ 3 LOC budget in cmd/compound -- one call site, one swap.
func LoadSkillManifestForPath(path string, now time.Time) (SkillManifest, error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return LoadSkillDirManifest(path, now)
	}
	return LoadSkillManifest(path)
}
