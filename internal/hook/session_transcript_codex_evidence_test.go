package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F5 (TestScanSessionTranscriptParsesCodexRollout) pins that the session transcript
// scanner recognises a codex-format rollout JSONL file and counts a Skill tool
// invocation in its SkillsFired summary -- WITHOUT scanning prose, so a prose-only
// assistant message yields nothing (the TestSkillsFiredCountsToolUseNotProse invariant
// applies to every format the parser learns).
//
// Codex writes `{"type":"response_item","payload":{"type":"function_call","name":...,
// "arguments":"<json string>"}}` for tool calls; the skill-detection rules live in
// applyCodexRollout and reuse skillFromInput / skillsFromLine exactly the way the
// claude branch does. Pinned by F3 + F4 already -- this test pins the prose/no-prose
// boundary in isolation so a future parser change cannot silently widen it.
func TestScanSessionTranscriptParsesCodexRollout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-session.jsonl")
	const sessionID = "00000000-aaaa-bbbb-cccc-000000000000"
	headerSession := `{"type":"session_meta","timestamp":"2026-08-26T15:00:00Z","payload":{"id":"` + sessionID + `","cwd":"/tmp/x/sample"}}`
	// Codex function_call whose arguments point at the skill via a Skill tool_use.
	assistantSkill := `{"type":"response_item","timestamp":"2026-08-26T15:28:53.400Z","payload":{"type":"function_call","name":"Skill","arguments":"{\"skill\":\"web-perf\"}"}}`
	// Prose-only assistant line -- must NOT show up as a skill invocation. Codex does
	// not emit a "message" with text the way claude does, so the prose case is modelled
	// here as an `assistant_message` line that carries no function_call. That line should
	// produce zero skills, mirroring TestSkillsFiredCountsToolUseNotProse for omp.
	proseOnly := `{"type":"event_msg","timestamp":"2026-08-26T15:30:00.000Z","payload":{"type":"user_message","message":"Loaded /repo/skills/web-perf/SKILL.md next."}}`
	content := strings.Join([]string{headerSession, assistantSkill, proseOnly}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := scanSessionTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(summary.SkillsFired, "capability://skill:web-perf") {
		t.Fatalf("SkillsFired = %v, want capability://skill:web-perf (codex function_call Skill)", summary.SkillsFired)
	}
	for _, s := range summary.SkillsFired {
		if s != "capability://skill:web-perf" {
			t.Fatalf("prose-only line leaked into SkillsFired: %v", summary.SkillsFired)
		}
	}
	if summary.ToolCalls < 1 {
		t.Fatalf("ToolCalls = %d, want >=1 (Skill block was a function_call)", summary.ToolCalls)
	}
	if summary.Format != "codex" {
		t.Fatalf("Format = %q, want codex", summary.Format)
	}
}

// TestScanSessionTranscriptCountsCodexReadOfSkillMd pins the genuine-use-without-Skill-block
// rule for codex: a function_call whose arguments cat a SKILL.md counts the skill, just like
// the claude branch does for tool_use Read/Bash inputs. This is the codex-shaped twin of the
// `reading SKILL.md is use` case in TestSkillsFiredCountsToolUseNotProse.
func TestScanSessionTranscriptCountsCodexReadOfSkillMd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-skill-read.jsonl")
	const sessionID = "00000000-aaaa-bbbb-cccc-000000000000"
	lines := []string{
		`{"type":"session_meta","timestamp":"2026-08-26T15:00:00Z","payload":{"id":"` + sessionID + `","cwd":"/repo/sample"}}`,
		`{"type":"response_item","timestamp":"2026-08-26T15:01:00Z","payload":{"type":"function_call","name":"Bash","arguments":"{\"command\":\"cat /repo/skills/example-skill/SKILL.md\"}"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := scanSessionTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(summary.SkillsFired, "capability://skill:example-skill") {
		t.Fatalf("SkillsFired = %v, want capability://skill:example-skill (codex Bash cat)", summary.SkillsFired)
	}
}

// TestSniffTranscriptFormatDetectsCodex pins the format sniffer: a codex rollout's
// first line carries `type:"session_meta"` and the sniffer must return "codex" so
// future per-format routing (none yet -- the parser is branch-on-shape today) can
// key off Format. omp already had a similar pin in TestScanSessionTranscriptParsesOmp.
func TestSniffTranscriptFormatDetectsCodex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-head.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","timestamp":"2026-08-26T15:00:00Z","payload":{"id":"abc"}}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := sniffTranscriptFormat(f); got != "codex" {
		t.Fatalf("sniffTranscriptFormat = %q, want codex", got)
	}
}
