package lanemanifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimaxManifest = `lane: minimax
harness: claude-code          # claude-code | codex | opencode | api
model: minimax/MiniMax-M3
effort: xhigh
capture: segmented_full_wire  # metadata_only | segmented_full_wire | none
permission: bypass            # plan | acceptEdits | bypass | default
api:
  enabled: true
  route: minimax/*
hooks:
  roster: minimal
`

func TestParseMinimalDefaultsToMinimalRoster(t *testing.T) {
	m, err := Parse([]byte(minimaxManifest))
	if err != nil {
		t.Fatalf("parse minimax manifest: %v", err)
	}
	if m.Lane != "minimax" {
		t.Fatalf("lane = %q, want minimax", m.Lane)
	}
	if m.Harness != "claude-code" {
		t.Fatalf("harness = %q, want claude-code", m.Harness)
	}
	if m.Hooks.Roster != RosterMinimal {
		t.Fatalf("roster = %q, want minimal default", m.Hooks.Roster)
	}
	if m.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %q, want %q", m.SchemaVersion, SchemaVersion)
	}
	if !m.API.Enabled || m.API.Route != "minimax/*" {
		t.Fatalf("api block = %+v, want enabled minimax/*", m.API)
	}
}

func TestParseJSONInput(t *testing.T) {
	// YAML is a JSON superset; a JSON manifest must parse identically.
	m, err := Parse([]byte(`{"lane":"qwen","harness":"claude-code","hooks":{"roster":"full"}}`))
	if err != nil {
		t.Fatalf("parse JSON manifest: %v", err)
	}
	if m.Lane != "qwen" || m.Hooks.Roster != RosterFull {
		t.Fatalf("parsed = %+v, want qwen/full", m)
	}
}

func TestParseExplicitSchemaVersionMismatchRejected(t *testing.T) {
	body := strings.Replace(minimaxManifest, "lane: minimax", "lane: minimax\nschema_version: compound.lane_manifest.v9.9", 1)
	if _, err := Parse([]byte(body)); err == nil {
		t.Fatal("expected schema_version mismatch to be rejected")
	}
}

func TestParseUnknownRosterRejected(t *testing.T) {
	body := strings.Replace(minimaxManifest, "roster: minimal", "roster: exhaustive", 1)
	if _, err := Parse([]byte(body)); err == nil {
		t.Fatal("expected unknown roster to be rejected")
	} else if !strings.Contains(err.Error(), "roster") {
		t.Fatalf("roster error is not actionable: %v", err)
	}
}

func TestParseUnknownHarnessRejected(t *testing.T) {
	body := strings.Replace(minimaxManifest, "harness: claude-code", "harness: gemini", 1)
	if _, err := Parse([]byte(body)); err == nil {
		t.Fatal("expected unknown harness family to be rejected")
	}
}

func TestParseMissingLaneRejected(t *testing.T) {
	body := strings.Replace(minimaxManifest, "lane: minimax\n", "", 1)
	if _, err := Parse([]byte(body)); err == nil {
		t.Fatal("expected missing lane to be rejected")
	}
}

func TestInstallHarnessDerivation(t *testing.T) {
	cases := []struct {
		name        string
		manifest    string
		want        string
		wantErrText string
	}{
		{"minimax lane", `lane: minimax` + "\n" + `harness: claude-code`, "claude-minimax", ""},
		{"qwen lane", `lane: qwen` + "\n" + `harness: claude-code`, "claude-qwen", ""},
		{"zai lane", `lane: zai` + "\n" + `harness: claude-code`, "claude-zai", ""},
		{"zai-direct lane", `lane: zai-direct` + "\n" + `harness: claude-code`, "claude-zai", ""},
		// the "codex" lane is a claude_code-family lane; its hook harness is claude-zai
		{"codex claude lane", `lane: codex` + "\n" + `harness: claude-code`, "claude-zai", ""},
		// native codex family installs the codex config, regardless of lane name
		{"native codex family", `lane: codex` + "\n" + `harness: codex`, "codex", ""},
		// the native claude lane publishes no hook profile; it must refuse rather
		// than fall through to a default profile compound does not install into
		{"native lane no hooks", `lane: native` + "\n" + `harness: claude-code`, "", "no managed hook harness"},
		{"opencode no path", `lane: opencode-oss` + "\n" + `harness: opencode`, "", "no managed hook install path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse([]byte(tc.manifest))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := m.InstallHarness()
			if tc.wantErrText != "" {
				if err == nil {
					t.Fatalf("InstallHarness = %q, want error containing %q", got, tc.wantErrText)
				}
				if !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("InstallHarness error = %q, want containing %q", err.Error(), tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("InstallHarness: %v", err)
			}
			if got != tc.want {
				t.Fatalf("InstallHarness = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTrackedManifestFixtureStaysValid pins the tracked reference manifest so a
// future schema change cannot silently invalidate the documented example.
func TestTrackedManifestFixtureStaysValid(t *testing.T) {
	fixture := filepath.Join("..", "..", "fixtures", "lanemanifest", "minimax.lane.yaml")
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read tracked fixture: %v", err)
	}
	m, err := Parse(body)
	if err != nil {
		t.Fatalf("tracked fixture no longer parses: %v", err)
	}
	if m.Hooks.Roster != RosterFull {
		t.Fatalf("tracked fixture roster = %q, want full (governed lane)", m.Hooks.Roster)
	}
	installHarness, err := m.InstallHarness()
	if err != nil {
		t.Fatalf("tracked fixture install harness: %v", err)
	}
	if installHarness != "claude-minimax" {
		t.Fatalf("tracked fixture install harness = %q, want claude-minimax", installHarness)
	}
}

// TestLaneHookHarnessMirrorsLanesJSON enforces the parity contract the
// laneHookHarness map documents: it "mirrors the hook_harness field in
// adapters/_lib/lanes.json". Without this test, the two could drift — lanes.json
// gains or changes a managed hook profile, or laneHookHarness gains an orphan —
// and hook install would derive a different harness than adapter install. Every
// non-empty hook_harness lane in lanes.json must be present with the same value;
// every empty-hook_harness lane must be absent (native/codex-native resolve to
// an error, never a default profile); and laneHookHarness must hold no lane
// lanes.json does not publish.
func TestLaneHookHarnessMirrorsLanesJSON(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "adapters", "_lib", "lanes.json"))
	if err != nil {
		t.Fatalf("read lanes.json: %v", err)
	}
	var doc struct {
		Lanes []struct {
			ID          string `json:"id"`
			HookHarness string `json:"hook_harness"`
		} `json:"lanes"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse lanes.json: %v", err)
	}
	if len(doc.Lanes) == 0 {
		t.Fatal("lanes.json publishes no lanes")
	}

	published := map[string]string{}
	for _, lane := range doc.Lanes {
		if lane.ID == "" {
			t.Fatal("lanes.json has a lane with no id")
		}
		published[lane.ID] = lane.HookHarness
		if lane.HookHarness == "" {
			if _, ok := laneHookHarness[lane.ID]; ok {
				t.Fatalf("lane %q has empty hook_harness but IS present in laneHookHarness; it must resolve to an error", lane.ID)
			}
			continue
		}
		got, ok := laneHookHarness[lane.ID]
		if !ok {
			t.Fatalf("lane %q publishes hook_harness %q but laneHookHarness has no entry", lane.ID, lane.HookHarness)
		}
		if got != lane.HookHarness {
			t.Fatalf("lane %q: laneHookHarness = %q, lanes.json = %q — the two must agree", lane.ID, got, lane.HookHarness)
		}
	}
	for lane := range laneHookHarness {
		if _, ok := published[lane]; !ok {
			t.Fatalf("laneHookHarness has orphan lane %q not published in lanes.json", lane)
		}
	}
}
