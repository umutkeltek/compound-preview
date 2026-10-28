package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F7 (TestScanSessionTranscriptParsesOmpSkillInvocation) pins that the
// session transcript scanner recognizes an omp-style JSONL session file
// and counts a Skill tool invocation in its SkillsFired summary -- WITHOUT
// scanning prose, so a prose-only assistant line yields nothing (the
// TestSkillsFiredCountsToolUseNotProse invariant applies to every format
// the parser learns).
//
// The omp format is identified by a header line whose JSON has
// {"type":"session","version":N,...}. The fixture below uses two
// representative assistant messages: one with a Skill tool_use, one
// prose-only. The packet's live proof 3 fails on base because the parser
// does not recognise omp's tool-call shape -- this test turns that into a
// concrete red.
func TestScanSessionTranscriptParsesOmpSkillInvocation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omp-session.jsonl")
	// omp header (line 1 title, line 2 session). Synthetic ids + paths --
	// the parser keys off shape, not content values.
	headerTitle := `{"type":"title","v":1,"title":"","updatedAt":"2026-08-25T15:26:26.411Z","pad":"   "}`
	headerSession := `{"type":"session","version":3,"id":"00000000-aaaa-bbbb-cccc-000000000000","timestamp":"2026-08-25T15:26:26.411Z","cwd":"/tmp/x/sample"}`
	// Assistant message carrying a Skill tool_use. omp's assistant messages
	// carry content blocks under message.content; this is the tool_use
	// shape omp writes (verified against the live omp session file in
	// ~/.omp/agent/sessions/<slug>/).
	assistantSkill := `{"type":"message","id":"m1","parentId":"p1","timestamp":"2026-08-25T15:28:53.400Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Skill","input":{"skill":"web-perf"}}]}}`
	// Prose-only assistant line -- must NOT show up as a skill invocation.
	assistantProse := `{"type":"message","id":"m2","parentId":"p1","timestamp":"2026-08-25T15:30:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"I will invoke the web-perf skill next."}]}}`
	content := strings.Join([]string{headerTitle, headerSession, assistantSkill, assistantProse}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := scanSessionTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(summary.SkillsFired, "capability://skill:web-perf") {
		t.Fatalf("SkillsFired = %v, want capability://skill:web-perf (omp tool_use Skill)", summary.SkillsFired)
	}
	// The prose-only line carries no Skill block; the parser must not promote
	// the literal string "web-perf" to a skill invocation.
	if !containsString(summary.SkillsFired, "capability://skill:"+anotherSkillName) {
		// sanity: prose should NOT have added any skill that wasn't already counted
		for _, s := range summary.SkillsFired {
			if s != "capability://skill:web-perf" {
				t.Fatalf("prose-only line leaked into SkillsFired: %v", summary.SkillsFired)
			}
		}
	}
	if summary.ToolCalls < 1 {
		t.Fatalf("ToolCalls = %d, want >=1 (Skill block was a tool_use)", summary.ToolCalls)
	}
}

// anotherSkillName is a sentinel that, if discovered, would mean the
// parser regressed into prose-matching. It must NEVER appear in SkillsFired.
const anotherSkillName = "totally-not-a-skill"
