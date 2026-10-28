package hook

import (
	"strings"
	"testing"

	"compound/internal/envelope"
)

// F7 — injection-blindness: a pack WITH a populated body_pack renders an injection
// digest with ZERO body text and unchanged section budgets (hook.go:1604 path).
//
// Bodies are NOT for ambient hook delivery — the per-turn digest is capped at
// MaxContextChars = 6000 and a hook session is the wrong channel for full content
// (the pack JSON under $COMPOUND_HOME/context/packs/ and dispatch prompts are the
// delivery channels). renderInjection must ignore body_pack entirely. This test
// pins that law.
//
// Green-at-birth: renderInjection is already blind to the additive section. Teeth
// proven in the same commit by scratch-mutation — adding an explicit
// pack.BodyPack-aware branch that emits body text reds the assertions, then a
// targeted revert restores green.
func TestRenderInjectionIgnoresBodyPackSection(t *testing.T) {
	body := "TOP-SECRET-BODY-MARKER-NEVER-IN-INJECTION top secret body text that must never reach a hook digest"
	pack := envelope.ContextPack{
		SummaryMD:    "summary",
		ActiveMemory: []string{"claim://claim_a"},
		BodyPack: &envelope.BodyPackSection{
			BudgetTokens:        2000,
			EstimatedUsedTokens: 500,
			Bodies: []envelope.BodyPackItem{{
				Ref:             "claim://body_one",
				Source:          "corpus:personal-corpus",
				Rank:            0.9,
				EstimatedTokens: 500,
				SHA256:          "deadbeef",
				Status:          "ok",
				Text:            body,
			}},
			Omitted: []envelope.BodyPackOmission{{
				Ref:             "claim://too_big",
				Rank:            0.4,
				EstimatedTokens: 9000,
				Reason:          "budget_exhausted",
			}},
		},
	}
	pack.Header.ID = "ctx_test"
	text := renderInjection(injectionRender{HookID: "hookev_test", Pack: pack, Surface: injectionSurfaceTask})

	if strings.Contains(text, body) {
		t.Fatalf("injection digest must NOT carry body_pack body text, but found the body marker:\n%s", text)
	}
	if strings.Contains(text, "claim://body_one") {
		t.Fatalf("injection digest must NOT cite body_pack item refs (no body_pack rendering at all):\n%s", text)
	}
	if strings.Contains(text, "Body Pack") || strings.Contains(text, "body_pack") || strings.Contains(text, "## Bodies") {
		t.Fatalf("injection digest must NOT render a body_pack section header:\n%s", text)
	}
	// Unchanged section budgets: the standard Active Memory section still renders and
	// the digest stays within the MaxContextChars cap (the existence of body_pack must
	// not steal budget from the existing sections).
	if !strings.Contains(text, "## Active Memory") {
		t.Fatalf("active memory section must still render independently of body_pack:\n%s", text)
	}
	if len(text) > MaxContextChars {
		t.Fatalf("injection digest exceeded MaxContextChars cap: %d > %d", len(text), MaxContextChars)
	}
}
