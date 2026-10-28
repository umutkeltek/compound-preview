package contract

import (
	"strings"
	"testing"
)

// AppendDelegateVariantToPrompt is the internal/contract home of the
// --instruction-block wire. The cmd/compound side holds a thin flag, this
// function does the work. It must:
//   - return an error for empty harness or empty preset (fail-closed)
//   - render the executor variant for a valid (harness, preset) pair
//   - preserve the original prompt as a prefix
//   - append after a blank line
//   - never write the repo file (this is enforced by the render path;
//     tested indirectly by the fact the test has no AGENTS.md/CLAUDE.md
//     in the cwd and the function still returns without error)
func TestAppendDelegateVariantToPrompt_ValidatesInputs(t *testing.T) {
	if _, err := AppendDelegateVariantToPrompt("", "/tmp", "executor", "do the work"); err == nil {
		t.Fatalf("empty harness must error")
	}
	if _, err := AppendDelegateVariantToPrompt("claude_code", "/tmp", "", "do the work"); err == nil {
		t.Fatalf("empty preset must error")
	}
	if _, err := AppendDelegateVariantToPrompt("unknown", "/tmp", "executor", "do the work"); err == nil {
		t.Fatalf("unsupported harness must error")
	}
}

func TestAppendDelegateVariantToPrompt_RendersAndPreservesPrompt(t *testing.T) {
	const speech = "do the executor audit"
	got, err := AppendDelegateVariantToPrompt("claude_code", "/tmp", "executor", speech)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !strings.HasPrefix(got, speech) {
		t.Fatalf("prompt not preserved as prefix; head=%q", got[:min(80, len(got))])
	}
	for _, want := range []string{
		"Work only inside the dispatched envelope/worktree",
		"Commit incrementally on the dispatched branch",
		"do not push, do not check out main",
		"Report back in the handoff format",
		"Escalate instead of improvising when blocked",
		"preset=" + "`executor`",
		"<!-- BEGIN COMPOUND GENERATED INSTRUCTIONS harness=claude_code -->",
		"<!-- END COMPOUND GENERATED INSTRUCTIONS -->",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("appended prompt missing %q", want)
		}
	}
}

func TestAppendDelegateVariantToPrompt_EmptyPromptBecomesVariant(t *testing.T) {
	got, err := AppendDelegateVariantToPrompt("claude_code", "/tmp", "executor", "")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !strings.Contains(got, "preset="+"`executor`") {
		t.Fatalf("empty prompt must yield the variant alone; got head=%q", got[:min(80, len(got))])
	}
}

// AppendDelegateVariantToPrompt must not leak task text into the appended
// variant. Mirrors the B7 guard for the render path.
func TestAppendDelegateVariantToPrompt_DoesNotLeakTaskText(t *testing.T) {
	got, err := AppendDelegateVariantToPrompt("claude_code", "/tmp", "executor", "private alpha task text")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// The prompt prefix is preserved, so the caller's text will appear at
	// the very head of the output. The variant body (after the blank line)
	// must NOT carry it.
	idx := strings.Index(got, "<!-- BEGIN")
	if idx < 0 {
		t.Fatalf("variant body not found")
	}
	if strings.Contains(got[idx:], "private alpha task text") {
		t.Fatalf("task text leaked into variant body:\n%s", got[idx:])
	}
}
