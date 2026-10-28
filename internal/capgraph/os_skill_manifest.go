package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LoadOSSkillManifest adapts the generated OS skill manifest into Compound's
// strict skill manifest contract. The source remains evidence only; imported
// nodes are offers, never execution.
func LoadOSSkillManifest(path string, now time.Time) (SkillManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillManifest{}, err
	}
	var src osSkillManifestDoc
	if err := json.Unmarshal(data, &src); err != nil {
		return SkillManifest{}, fmt.Errorf("parse OS skill manifest %s: %w", path, err)
	}
	if len(src.Entries) == 0 {
		return SkillManifest{}, fmt.Errorf("OS skill manifest %s has no entries", path)
	}
	if now.IsZero() {
		now = time.Now()
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	evidenceRef := "skill_manifest_json:" + revision[:19]
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest_os_" + revision[7:19],
		AdapterID:     "os.skill-manifest-json",
		SourceSystem:  "skill-manifest",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: SkillSourceRef{
			Path:        path,
			Kind:        "generated_view",
			Owner:       "capability-graph",
			SourceClass: "os-skill-manifest-json",
			Revision:    revision,
		},
		EvidenceRefs: []string{evidenceRef},
	}
	// nameCounts keys use the faithful identity slug (not the legacy mangled form).
	nameCounts := map[string]int{}
	for _, entry := range src.Entries {
		key := skillEntryKind(entry) + ":" + slugSkill(entry.Name)
		nameCounts[key]++
	}
	for _, entry := range src.Entries {
		skill, stubs, err := osSkillToManifestEntry(entry, nameCounts, evidenceRef)
		if err != nil {
			return SkillManifest{}, err
		}
		manifest.Skills = append(manifest.Skills, skill)
		manifest.Skills = append(manifest.Skills, stubs...)
	}
	sort.SliceStable(manifest.Skills, func(i, j int) bool {
		return manifest.Skills[i].ID < manifest.Skills[j].ID
	})
	return manifest, nil
}

type osSkillManifestDoc struct {
	Generated      string                 `json:"_generated"`
	Counts         map[string]interface{} `json:"counts"`
	DuplicateNames []interface{}          `json:"duplicate_names"`
	Entries        []osSkillEntry         `json:"entries"`
}

type osSkillEntry struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Trigger string `json:"trigger"`
	Path    string `json:"path"`
	Home    string `json:"home"`
}

func osSkillToManifestEntry(entry osSkillEntry, nameCounts map[string]int, evidenceRef string) (SkillManifestEntry, []SkillManifestEntry, error) {
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		return SkillManifestEntry{}, nil, fmt.Errorf("OS skill manifest entry missing name")
	}
	kind := skillEntryKind(entry)
	if kind == "" {
		return SkillManifestEntry{}, nil, fmt.Errorf("OS skill manifest entry %q unknown kind %q", name, entry.Kind)
	}
	faithfulSlug := slugSkill(name)
	baseID := kind + ":" + faithfulSlug
	id := baseID
	homeQualified := nameCounts[baseID] > 1
	if homeQualified {
		id = kind + ":" + slugSkill(entry.Home) + ":" + faithfulSlug
	}
	skill := SkillManifestEntry{
		ID:                  id,
		Kind:                kind,
		Name:                name,
		Status:              "active",
		Triggers:            osSkillTriggers(entry),
		Capabilities:        osSkillCapabilities(entry),
		CompatibleHarnesses: osSkillHarnesses(entry),
		EvidenceRefs:        []string{evidenceRef},
		Weight:              osSkillWeight(entry),
	}
	// Slice-2 reachability stamp: entry.Path is the absolute dir containing
	// the skill's SKILL.md (verified against the existing os-skill-manifest
	// fixture at os_skill_manifest_test.go:79). ApplySkillManifest will look
	// it up via DoctorSkillSourceEntry and stamp Node.Source on the node;
	// a stale or broken dir becomes Source.Reachable=false WITHIN ONE IMPORT.
	if clean := strings.TrimSpace(entry.Path); clean != "" && filepath.IsAbs(clean) {
		skill.SkillPath = clean
	}
	// Migrate mangled pre-fix ids: retired stub + retired_by edge (via ReplacementFor).
	legacyID := kind + ":" + legacySlugSkill(name)
	if homeQualified {
		legacyID = kind + ":" + legacySlugSkill(entry.Home) + ":" + legacySlugSkill(name)
	}
	var stubs []SkillManifestEntry
	if legacyID != id {
		skill.ReplacementFor = []string{legacyID}
		// Minimal trigger so skillOK validates; retired status keeps the stub off Surface.
		stubs = append(stubs, SkillManifestEntry{
			ID:           legacyID,
			Kind:         kind,
			Name:         name + " (legacy id)",
			Status:       "retired",
			Triggers:     []string{name},
			EvidenceRefs: []string{evidenceRef},
		})
	}
	return skill, stubs, nil
}

func skillEntryKind(entry osSkillEntry) string {
	switch strings.TrimSpace(entry.Kind) {
	case "skill":
		return "skill"
	case "recipe":
		return "procedure"
	default:
		return ""
	}
}

func osSkillTriggers(entry osSkillEntry) []string {
	values := []string{entry.Name, entry.Kind, entry.Home, entry.Trigger}
	return skillTokenList(values...)
}

func osSkillCapabilities(entry osSkillEntry) []string {
	return skillTokenList(entry.Kind, entry.Home)
}

func osSkillHarnesses(entry osSkillEntry) []string {
	switch strings.TrimSpace(entry.Home) {
	case "codex", "codex-superpowers":
		return []string{"codex"}
	case "claude":
		return []string{"claude_code"}
	default:
		return nil
	}
}

func osSkillWeight(entry osSkillEntry) float64 {
	switch strings.TrimSpace(entry.Kind) {
	case "recipe":
		return 1.25
	default:
		return 1
	}
}

func skillTokenList(values ...string) []string {
	// Trigger/capability tokens keep stop-word + short filtering (unchanged).
	return collectSkillTokens(true, values...)
}

// slugSkill is the faithful identity slug: lowercase, non-alphanumeric → separator,
// ALL tokens kept in order (no stop-word strip, no dedup, no short-token filter).
// Empty input → "unknown".
func slugSkill(s string) string {
	parts := collectFaithfulTokens(s)
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "-")
}

// legacySlugSkill is the frozen pre-migration identity algorithm (stop-words +
// dedup). Used only to compute retired stub ids when a skill name's faithful
// slug differs from the historical mangled id. Do not use for new identity.
func legacySlugSkill(s string) string {
	parts := collectSkillTokens(false, s)
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "-")
}

func collectFaithfulTokens(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func collectSkillTokens(filterShort bool, values ...string) []string {
	stop := map[string]bool{
		"a": true, "an": true, "and": true, "are": true, "as": true, "be": true,
		"for": true, "from": true, "in": true, "into": true, "is": true, "it": true,
		"of": true, "on": true, "or": true, "the": true, "to": true, "use": true,
		"user": true, "wants": true, "when": true, "with": true,
	}
	allowedShort := map[string]bool{"ai": true, "ci": true, "db": true, "go": true, "os": true, "ui": true, "ux": true}
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		for _, part := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
		}) {
			part = strings.TrimSpace(part)
			if stop[part] || seen[part] {
				continue
			}
			if filterShort && len(part) < 3 && !allowedShort[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}
