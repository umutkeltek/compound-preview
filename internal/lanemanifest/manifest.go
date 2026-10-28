// Package lanemanifest owns the one declarative unit that names a harness lane:
// the lane manifest. It is the single place that expresses "this lane is the
// same thing as that endpoint" (harness + model + capture + route + hooks) that
// switchback and compound both consume. Compound's consumption today is the hook
// roster: `hook install --lane-file` derives the install harness and selects the
// hook roster from the manifest. The manifest owns no authority object -- hook
// and harness configs retain authority; it is a declaration, not a ledger.
package lanemanifest

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the current lane-manifest schema. A manifest that carries a
// different schema_version is rejected rather than guessed at.
const SchemaVersion = "compound.lane_manifest.v0.1"

// Roster is the hook-roster class a lane is entitled to. Minimal is the
// interactive default (authority attestation + landing/deny gates only); full is
// the governed selection (every lifecycle event compound can materialize).
// Hooks are refinements, not prerequisites -- a lane works with zero hook
// ceremony at the minimal floor.
type Roster string

const (
	RosterMinimal Roster = "minimal"
	RosterFull    Roster = "full"
)

// KnownRosters is the closed set of valid roster classes.
var KnownRosters = []Roster{RosterMinimal, RosterFull}

// APIConfig declares the lane-as-API consumption mode. Switchback maps this onto
// its gateway route targets; compound does not consume it yet.
type APIConfig struct {
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Route   string `json:"route,omitempty" yaml:"route,omitempty"`
}

// HooksConfig is the compound-owned portion of the manifest: which roster of
// lifecycle hooks the lane installs.
type HooksConfig struct {
	Roster Roster `json:"roster" yaml:"roster"`
}

// Manifest is the declarative lane unit. Every field is dual-tagged so the same
// file serves YAML-tracked authority and JSON-emitting consumers.
type Manifest struct {
	SchemaVersion string      `json:"schema_version" yaml:"schema_version"`
	Lane          string      `json:"lane" yaml:"lane"`
	Harness       string      `json:"harness" yaml:"harness"` // claude-code | codex | opencode
	Model         string      `json:"model,omitempty" yaml:"model,omitempty"`
	Effort        string      `json:"effort,omitempty" yaml:"effort,omitempty"`
	Capture       string      `json:"capture,omitempty" yaml:"capture,omitempty"`
	Permission    string      `json:"permission,omitempty" yaml:"permission,omitempty"`
	API           APIConfig   `json:"api" yaml:"api"`
	Hooks         HooksConfig `json:"hooks" yaml:"hooks"`
}

// Parse decodes a manifest from YAML (or JSON -- YAML is a JSON superset),
// applies the minimal-roster default, and validates it. A manifest with no
// explicit roster means minimal: the interactive default, not the governed one.
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse lane manifest: %w", err)
	}
	if m.SchemaVersion == "" {
		m.SchemaVersion = SchemaVersion
	}
	if m.Hooks.Roster == "" {
		m.Hooks.Roster = RosterMinimal
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate checks the manifest against its closed vocabularies. It never reaches
// for a runtime registry -- the manifest is self-describing; harness-family and
// roster are enumerated here.
func (m Manifest) Validate() error {
	if m.SchemaVersion != "" && m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("lane manifest schema version %q, want %q", m.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(m.Lane) == "" {
		return errors.New("lane manifest requires lane")
	}
	switch strings.ToLower(strings.TrimSpace(m.Harness)) {
	case "claude-code", "codex", "opencode":
	default:
		return fmt.Errorf("lane manifest harness %q is not supported (claude-code | codex | opencode)", m.Harness)
	}
	switch m.Hooks.Roster {
	case RosterMinimal, RosterFull:
	default:
		return fmt.Errorf("lane manifest hooks.roster %q is not valid (minimal | full)", m.Hooks.Roster)
	}
	return nil
}

// InstallHarness derives the install harness compound writes hooks into for this
// manifest. A claude-code-family lane resolves through the lane -> hook_harness
// mapping (the same convention lanes.json publishes); a bare claude-code
// manifest installs the generic profile; codex family installs the native codex
// config. opencode has no managed hook install path yet.
func (m Manifest) InstallHarness() (string, error) {
	harness := strings.ToLower(strings.TrimSpace(m.Harness))
	lane := strings.ToLower(strings.TrimSpace(m.Lane))
	switch harness {
	case "claude-code":
		if lane == "" {
			return "claude-code", nil
		}
		if hookHarness, ok := laneHookHarness[lane]; ok {
			return hookHarness, nil
		}
		return "", fmt.Errorf("claude-code lane %q has no managed hook harness", lane)
	case "codex":
		return "codex", nil
	case "opencode":
		return "", errors.New("opencode has no managed hook install path yet")
	default:
		return "", fmt.Errorf("unknown harness family %q", m.Harness)
	}
}

// laneHookHarness mirrors the hook_harness field in adapters/_lib/lanes.json for
// the claude_code-family lanes that publish a managed hook profile. It is the
// same convention compound's installer already hardcodes (claude-minimax,
// claude-qwen, claude-zai); keeping it here makes the lane -> install-harness
// seam explicit instead of implicit. Lanes without a hook profile (native,
// codex-native) are deliberately absent -- they resolve to an error, never to a
// default profile compound does not install into.
var laneHookHarness = map[string]string{
	"zai":        "claude-zai",
	"zai-direct": "claude-zai",
	"codex":      "claude-zai", // the "codex" lane is a claude_code-family lane, distinct from native codex
	"minimax":    "claude-minimax",
	"qwen":       "claude-qwen",
}
