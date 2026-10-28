package harness

// skill_adapter_door_test.go — F11: every rendered adapter names only
// commands that exist on the one browser door.
//
// The door is the agent-browser CLI. Its flags and verbs below are the ones
// printed by `agent-browser --help` (0.34.0, first on PATH; 0.38.2 is a
// superset). A header that names an MCP tool the harness does not load, a
// shell CLI that is not installed, or a bare-JSON positional payload sends a
// lane down a path that fails at the first call. These tests fail on any of
// those shapes.

import (
	"regexp"
	"strings"
	"testing"
)

// agentBrowserFlags are the global flags a header may use, mapped to whether
// they take a value. Verified against `agent-browser --help`.
var agentBrowserFlags = map[string]bool{
	"--profile":         true,
	"--session":         true,
	"--namespace":       true,
	"--allowed-domains": true,
	"--cdp":             true,
	"--headed":          false,
	"--json":            false,
}

// agentBrowserVerbs are the commands a header may use. Verified against
// `agent-browser --help` "Core Commands" / "Get Info" / "Sessions".
var agentBrowserVerbs = map[string]bool{
	"open": true, "snapshot": true, "click": true, "fill": true, "type": true,
	"press": true, "screenshot": true, "get": true, "eval": true, "wait": true,
	"close": true, "scroll": true, "back": true, "reload": true, "session": true,
}

var bareJSONPositional = regexp.MustCompile(`\s'?\{"`)

// headerOf returns the rendered text that precedes the body: the part the
// adapter owns. The fixture body is excluded so its wording cannot mask or
// trigger a header finding.
func headerOf(t *testing.T, h HarnessAdapter) string {
	t.Helper()
	body := fixtureBody(t)
	out, err := RenderSkillAdapter(body, h, fixedNow())
	if err != nil {
		t.Fatalf("render %s: %v", h.ID, err)
	}
	i := strings.Index(string(out), string(body))
	if i < 0 {
		t.Fatalf("render %s does not contain the body", h.ID)
	}
	return string(out[:i])
}

func TestAdapterHeadersNameOnlyRealBrowserDoors(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no private overlay: tracked template only
	for _, h := range DefaultSkillAdapters() {
		t.Run(h.ID, func(t *testing.T) {
			header := headerOf(t, h)
			for _, banned := range []string{
				"browser_screenshot", // no such tool on any configured backend
				"mcporter call",      // mcporter is not installed; not a door
				"browser_evaluate",   // Playwright-MCP name; no harness here loads it
			} {
				if strings.Contains(header, banned) {
					t.Errorf("F11 FAIL: %s header names %q", h.ID, banned)
				}
			}
			if m := bareJSONPositional.FindString(header); m != "" {
				t.Errorf("F11 FAIL: %s header carries a bare-JSON positional payload (%q)", h.ID, m)
			}
			checkAgentBrowserLines(t, h.ID, header)
		})
	}
}

func TestCodexAdapterUsesTheAgentBrowserDoor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var codex *HarnessAdapter
	for _, h := range DefaultSkillAdapters() {
		if h.ID == "codex" {
			h := h
			codex = &h
		}
	}
	if codex == nil {
		t.Fatal("codex adapter missing from DefaultSkillAdapters")
	}
	header := headerOf(t, *codex)
	for _, want := range []string{
		"agent-browser --session",
		"--profile",
		" snapshot -i",
		" screenshot ",
		" close",
	} {
		if !strings.Contains(header, want) {
			t.Errorf("F11 FAIL: codex header missing %q", want)
		}
	}
	// The bundled plugin's enabled state lives in the operator's codex
	// config; the tracked template must not assert it either way.
	for _, claim := range []string{"intentionally disabled", "plugin is disabled", "Bundled plugin is OFF"} {
		if strings.Contains(header, claim) {
			t.Errorf("F11 FAIL: codex header asserts plugin state %q", claim)
		}
	}
}

// checkAgentBrowserLines parses every line that starts with `agent-browser`
// and fails on any flag or verb the CLI does not print in --help.
func checkAgentBrowserLines(t *testing.T, id, header string) {
	t.Helper()
	for _, raw := range strings.Split(header, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "agent-browser ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		verb := ""
		for i := 0; i < len(fields); i++ {
			f := fields[i]
			if strings.HasPrefix(f, "--") {
				takesValue, ok := agentBrowserFlags[f]
				if !ok {
					t.Errorf("F11 FAIL: %s header uses unverified agent-browser flag %q in %q", id, f, line)
					continue
				}
				if takesValue {
					i++
				}
				continue
			}
			verb = f
			break
		}
		if verb == "" || !agentBrowserVerbs[verb] {
			t.Errorf("F11 FAIL: %s header uses unverified agent-browser verb %q in %q", id, verb, line)
		}
	}
}
