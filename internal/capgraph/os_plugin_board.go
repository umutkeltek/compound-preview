package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// LoadOSPluginBoard adapts the generated OS skills-selection board plugin
// matrix into non-surfacing adapter nodes. Plugin enable state is inventory
// evidence for harness owners; importing it must not make plugins executable.
func LoadOSPluginBoard(path string, now time.Time) (SkillManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillManifest{}, err
	}
	var src osPluginBoardDoc
	if err := json.Unmarshal(data, &src); err != nil {
		return SkillManifest{}, fmt.Errorf("parse OS plugin board %s: %w", path, err)
	}
	rows := append([]osPluginRow(nil), src.PluginMatrix.Rows...)
	if len(rows) == 0 {
		return SkillManifest{}, fmt.Errorf("OS plugin board %s has no plugin_matrix.rows", path)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Plugin < rows[j].Plugin
	})
	if now.IsZero() {
		now = time.Now()
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	evidenceRef := "plugin_serving_board_json:" + revision[:19]
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest_os_plugins_" + revision[7:19],
		AdapterID:     "os.plugin-serving-board-json",
		SourceSystem:  "plugin-serving",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: SkillSourceRef{
			Path:        path,
			Kind:        "generated_view",
			Owner:       "capability-graph",
			SourceClass: "os-plugin-serving-board-json",
			Revision:    revision,
		},
		EvidenceRefs: []string{evidenceRef},
	}
	idCounts := map[string]int{}
	for _, row := range rows {
		idCounts[pluginAdapterID(row)]++
	}
	for _, row := range rows {
		entry, err := osPluginRowToManifestEntry(row, idCounts, evidenceRef)
		if err != nil {
			return SkillManifest{}, err
		}
		manifest.Skills = append(manifest.Skills, entry)
	}
	return manifest, nil
}

type osPluginBoardDoc struct {
	PluginMatrix struct {
		Rows []osPluginRow `json:"rows"`
	} `json:"plugin_matrix"`
}

type osPluginRow struct {
	Plugin      string `json:"plugin"`
	Claude      string `json:"claude"`
	Codex       string `json:"codex"`
	Opencode    string `json:"opencode"`
	Scope       string `json:"scope"`
	Marketplace string `json:"marketplace"`
}

func osPluginRowToManifestEntry(row osPluginRow, idCounts map[string]int, evidenceRef string) (SkillManifestEntry, error) {
	name := strings.TrimSpace(row.Plugin)
	if name == "" {
		return SkillManifestEntry{}, fmt.Errorf("OS plugin board row missing plugin")
	}
	id := pluginAdapterID(row)
	if idCounts[id] > 1 {
		id = id + ":" + slugSkill(row.Marketplace)
	}
	return SkillManifestEntry{
		ID:                  id,
		Kind:                "adapter",
		Name:                name,
		Status:              "needs_review",
		Triggers:            osPluginTriggers(row),
		Capabilities:        osPluginCapabilities(row),
		CompatibleHarnesses: osPluginHarnesses(row),
		EvidenceRefs:        []string{evidenceRef},
		Weight:              0.2,
		Retired:             true,
	}, nil
}

func pluginAdapterID(row osPluginRow) string {
	return "adapter:plugin:" + slugSkill(row.Plugin)
}

func osPluginTriggers(row osPluginRow) []string {
	return skillTokenList(row.Plugin, row.Marketplace, row.Scope, "plugin", "plugin serving")
}

func osPluginCapabilities(row osPluginRow) []string {
	values := []string{"plugin", "plugin serving", row.Marketplace, row.Scope}
	for _, harness := range osPluginHarnesses(row) {
		values = append(values, "enabled "+harness)
	}
	return skillTokenList(values...)
}

func osPluginHarnesses(row osPluginRow) []string {
	var out []string
	if pluginEnabled(row.Claude) {
		out = append(out, "claude_code")
	}
	if pluginEnabled(row.Codex) {
		out = append(out, "codex")
	}
	if pluginEnabled(row.Opencode) {
		out = append(out, "opencode")
	}
	return out
}

func pluginEnabled(value string) bool {
	return strings.Contains(strings.ToLower(value), "on")
}
