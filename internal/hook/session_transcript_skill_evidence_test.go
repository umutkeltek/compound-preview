package hook

// SkillsFired evidence boundary. `SkillsFired` feeds Receipt.Skills.Used, which capgraph
// reads as the CONSUMED signal for a served capability ref (internal/capgraph
// probe_group_usage.go and injection_observation.go), and that consumption is what raises a
// capability's score in the next orient. So the question "did this session use skill X?"
// is not bookkeeping -- a false yes manufactures the evidence that decides what gets
// offered next, and the loop feeds itself.
//
// Before this file, detection scanned the RAW TEXT of every transcript line, so an
// assistant merely NAMING a skill path in prose counted as having used it. These tests
// pin the boundary: a tool actually touching the skill is use; talking about it is not.

import (
	"strings"
	"testing"
)

// skillTranscriptCase is one transcript shape and what it should prove about skill use.
type skillTranscriptCase struct {
	name  string
	entry string
	want  []string
	why   string
}

// TestSkillsFiredCountsToolUseNotProse is the headline falsifier. The prose case is the
// defect: it is currently REPORTED as a skill use, and the fixture in
// TestSessionEndWorklessTranscriptProducesContentFreeReceipt asserts that as correct.
//
// Falsifier: restore the raw-line scan (skillsFromLine over `trimmed` in
// scanSessionTranscript) and the prose case goes RED, because prose again counts as use.
func TestSkillsFiredCountsToolUseNotProse(t *testing.T) {
	for _, tc := range []skillTranscriptCase{
		{
			name: "prose mention is not use",
			entry: `{"type":"assistant","timestamp":"2026-07-10T01:01:00Z","message":{"role":"assistant","content":[` +
				`{"type":"text","text":"Loaded /repo/skills/golang-testing/SKILL.md for this fix."},` +
				`{"type":"tool_use","name":"Bash","input":{"command":"go test ./internal/hook"}}]}}`,
			want: nil,
			why:  "naming a skill path in prose is talk, not a capability invocation",
		},
		{
			name: "Skill tool call is use",
			entry: `{"type":"assistant","timestamp":"2026-07-10T01:01:00Z","message":{"role":"assistant","content":[` +
				`{"type":"tool_use","name":"Skill","input":{"skill":"golang-testing"}}]}}`,
			want: []string{"capability://skill:golang-testing"},
			why:  "the harness invoked the skill through its own door",
		},
		{
			name: "reading SKILL.md is use",
			entry: `{"type":"assistant","timestamp":"2026-07-10T01:01:00Z","message":{"role":"assistant","content":[` +
				`{"type":"tool_use","name":"Read","input":{"file_path":"/repo/skills/golang-testing/SKILL.md"}}]}}`,
			want: []string{"capability://skill:golang-testing"},
			why:  "a skill loaded by reading its SKILL.md is genuinely used, with no Skill block to show for it -- this is the true signal the raw-line scan was reaching for, and it must survive",
		},
		{
			name: "catting SKILL.md is use",
			entry: `{"type":"assistant","timestamp":"2026-07-10T01:01:00Z","message":{"role":"assistant","content":[` +
				`{"type":"tool_use","name":"Bash","input":{"command":"cat /repo/skills/golang-testing/SKILL.md"}}]}}`,
			want: []string{"capability://skill:golang-testing"},
			why:  "same as Read, through a different tool",
		},
		{
			name: "a USER pasting a skill path is not use",
			entry: `{"type":"user","timestamp":"2026-07-10T01:01:00Z","message":{"role":"user","content":[` +
				`{"type":"text","text":"see /repo/skills/golang-testing/SKILL.md"}]}}`,
			want: nil,
			why:  "the operator quoting a path says nothing about what the session did",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := writeTranscript(t, home, tc.entry+"\n")

			got, err := scanSessionTranscript(path)
			if err != nil {
				t.Fatalf("scan transcript: %v", err)
			}
			if !equalStringSets(got.SkillsFired, tc.want) {
				t.Errorf("SkillsFired = %v, want %v\n  %s", got.SkillsFired, tc.want, tc.why)
			}
		})
	}
}

// equalStringSets compares order-insensitively; SkillsFired is a sorted key set, but the
// assertion should not break if that ordering contract changes.
func equalStringSets(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[strings.TrimSpace(g)]++
	}
	for _, w := range want {
		seen[strings.TrimSpace(w)]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
