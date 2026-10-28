package harness

// F10 supplement: the public redact gate on the tracked skill_adapter.go.
// private references that the operator's gate would catch (URL_PRIVATE,
// TOOL_PRIVATE) must live ONLY in the private overlay, never in tracked Go.

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestRedactGateOnSkillAdapterFile(t *testing.T) {
	body, err := os.ReadFile("skill_adapter.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(body)
	// Mirror the public redact gate's "private tool reference" rules:
	// HIGH = private URL or private file path; MEDIUM = private tool name.
	banned := map[string]string{
		"localhost:31337": "HIGH (internal.url_private — voice-notify URL)",
		"PAI/":            "MEDIUM (internal.tool_private — PAI USER/SKILLCUSTOMIZATIONS path)",
		"~/.claude/PAI":   "MEDIUM (internal.tool_private — PAI MEMORY/SKILLS path)",
		"mcp__":           "MEDIUM (internal.tool_private — Claude-specific MCP tool namespace)",
	}
	var hits []string
	for needle, severity := range banned {
		if strings.Contains(text, needle) {
			hits = append(hits, fmt.Sprintf("%s: %q", severity, needle))
		}
	}
	if len(hits) > 0 {
		t.Fatalf("redact gate FAIL — private references remain in tracked template:\n  %s",
			strings.Join(hits, "\n  "))
	}
	t.Logf("HIGH=0 MEDIUM=0 — private references live in the overlay file only")
}
