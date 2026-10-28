package hook

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"compound/internal/envelope"
)

type injectionCap struct {
	name string
	cap  int
}

// budgetedInjectionSectionCaps mirrors, in declaration order, every cap passed to
// writeBudgetedInjectionSection — per surface. A new budgeted section must be added
// here in the same change that introduces it; that coupling is the point, because
// the cap sum is only an enforceable budget if the list is complete.
var budgetedInjectionSectionCaps = map[injectionSurface][]injectionCap{
	injectionSurfaceTask: {
		{"header", maxInjectionHeaderChars},
		{"orientation note", maxInjectionOrientationNoteChars},
		{"active memory", maxInjectionActiveMemoryChars},
		{"negatives", maxInjectionNegativeChars},
		{"active decisions", maxInjectionActiveDecisionChars},
		{"required checks", maxInjectionRequiredChecksChars},
		{"forbidden recall scopes", maxInjectionAvoidChars},
		{"capability offers", maxInjectionCapabilityOfferChars},
	},
	injectionSurfaceOrientation: {
		{"header", maxInjectionHeaderChars},
		{"summary", maxOrientationSummaryChars},
		{"active instructions", maxOrientationActiveInstructionChars},
		{"active hooks", maxOrientationActiveHookChars},
		{"included", maxOrientationIncludedChars},
		{"excluded", maxOrientationExcludedChars},
		{"hook readiness + roster", maxOrientationHookReadinessChars},
	},
}

// TestInjectionSectionCapsFitBudget pins the invariant a section cap exists to
// provide: the caps must sum within MaxContextChars.
//
// This is a regression test for a real landed defect, not a theoretical one. When
// the negatives section took a 900-rune cap it was documented as "funded by" the
// 137 runes the Active Decisions demotion freed. The arithmetic was never checked,
// the sum reached 6320 against a 6000 budget, and the caps silently stopped
// bounding anything: capInjectionChars then decided what survived by truncating
// whatever rendered last. Caps that oversubscribe their budget do not degrade
// gracefully — they relocate the loss to the tail, out of sight of every test that
// only asserts a section is present.
func TestInjectionSectionCapsFitBudget(t *testing.T) {
	for _, surface := range []injectionSurface{injectionSurfaceTask, injectionSurfaceOrientation} {
		t.Run(string(surface), func(t *testing.T) {
			sections := budgetedInjectionSectionCaps[surface]
			if len(sections) == 0 {
				t.Fatalf("surface %q has no caps listed; the budget cannot be enforced for a surface this test does not know about", surface)
			}
			sum := 0
			for _, section := range sections {
				if section.cap <= 0 {
					t.Errorf("section %q has non-positive cap %d", section.name, section.cap)
				}
				sum += section.cap
			}

			if sum > MaxContextChars {
				t.Fatalf("%s surface caps oversubscribe the injection budget: sum=%d > MaxContextChars=%d (over by %d).\n"+
					"A cap sum above the budget does not truncate the section that grew — it amputates whatever renders last.\n"+
					"Fund a raise by lowering another cap on the SAME surface, do not raise the sum.",
					surface, sum, MaxContextChars, sum-MaxContextChars)
			}
			t.Logf("%s surface: caps sum=%d of MaxContextChars=%d, headroom=%d runes",
				surface, sum, MaxContextChars, MaxContextChars-sum)
		})
	}
}

// TestIncludedTrustTiersFitTheOrientationCap pins that the tier encoding is paid for
// out of the Included section's EXISTING cap.
//
// The tiers were funded by choosing the cheaper encoding, not by raising a cap: one
// legend line plus a marker on demoted refs only, with verify-first left implicit as
// the default. Marking every ref would have cost a verb per ref for the tier the reader
// already applies by default. This test measures the live shape — the 5-ref pack a real
// `orient` serves this repo — against the unchanged cap, so a future edit that grows the
// per-ref line has to notice it is spending someone else's budget.
func TestIncludedTrustTiersFitTheOrientationCap(t *testing.T) {
	// Exactly what `compound orient --harness claude_code` includes for this repo.
	live := []envelope.IncludedRef{
		{Ref: "AGENTS.md", SourceClass: "source", Reason: "ambient for product repos", Confidence: 1, Status: "current"},
		{Ref: "skills/README.md", SourceClass: "source", Reason: "ambient for product repos", Confidence: 1, Status: "current"},
		{Ref: "constitution://universal-principles", SourceClass: "source", Reason: "ambient for product repos", Confidence: 1, Status: "current"},
		{Ref: "repo://docs", SourceClass: "source", Reason: "ambient for product repos", Confidence: 1, Status: "current"},
		{Ref: "memory://relevance-gated", SourceClass: "generated", Reason: "ambient for product repos", Confidence: 1, Status: "current"},
	}

	var section strings.Builder
	writeIncludedList(&section, live, maxOrientationIncludedChars)
	got := utf8.RuneCountInString(section.String())
	if got > maxOrientationIncludedChars {
		t.Fatalf("Included section with trust tiers is %d runes, over its %d cap (by %d).\n"+
			"Fund it by making the encoding cheaper — never by raising the cap.\n%s",
			got, maxOrientationIncludedChars, got-maxOrientationIncludedChars, section.String())
	}
	t.Logf("live 5-ref Included section with tiers: %d runes of cap %d (headroom %d)",
		got, maxOrientationIncludedChars, maxOrientationIncludedChars-got)

	// And the section must actually survive its own cap in the rendered digest: a
	// truncation marker here would mean the tier grammar was defined and then cut.
	pack := envelope.ContextPack{SummaryMD: "summary", Included: live}
	pack.Header.ID = "ctx_tier_budget"
	rendered := renderInjection(injectionRender{HookID: "hookev_tier_budget", Pack: pack, Surface: injectionSurfaceOrientation})
	idx := strings.Index(rendered, "## Included")
	if idx < 0 {
		t.Fatalf("orientation digest lost the Included section:\n%s", rendered)
	}
	tail := rendered[idx:]
	if cut := strings.Index(tail, "\n## "); cut > 0 {
		tail = tail[:cut]
	}
	if strings.Contains(tail, "Section truncated") {
		t.Fatalf("Included section truncated at the live pack size:\n%s", tail)
	}
	if !strings.Contains(tail, "- trust: unmarked = "+string(trustVerifyFirst)) {
		t.Fatalf("Included section must define its tier vocabulary:\n%s", tail)
	}
}

// liveControlPlanePack is the pack a real `compound orient --harness claude_code` serves
// this repo, verbatim, in served order — captured from the rendered SessionStart digest at
// COMPOUND_HOME/context/injections/, not hand-composed.
//
// Ref 8 is the one that matters and the one two previous fixtures got wrong. It is
// `decision://DEC-0001`, whose 84-rune reason is nearly double the cheap refs above it. An
// earlier fixture put `hook://claude_code/PostToolUse` (44-rune reason) in that slot and
// measured the section at 1075 of 1100 — "fits, 25 to spare" — while the served pack needed
// 1105 and truncated by 5 on every session. Thirty runes of fixture optimism inverted the
// verdict. Keep the EXPENSIVE tail ref here: a cap fixture is only as true as its worst line.
func liveControlPlanePack() []envelope.IncludedRef {
	const ambient = "ambient for control-plane repos"
	const decisionReason = "accepted decision from Compound memory ledger; old source is source-in-place evidence"
	return []envelope.IncludedRef{
		{Ref: "AGENTS.md", SourceClass: "source", Reason: "ambient for control-plane repos", Confidence: 1, Status: "current"},
		{Ref: "skills/README.md", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "constitution://universal-principles", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "control-plane://orientation-map", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "control-plane://workflow-contract", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "control-plane://sop-packets", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "memory://relevance-gated", SourceClass: "generated", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "decision://DEC-0001", SourceClass: "runtime", Reason: decisionReason, Confidence: 1, Status: "current"},
	}
}

func TestIncludedTrustTiersFitTheCapAtTheRefBound(t *testing.T) {
	atBound := liveControlPlanePack()
	if len(atBound) != maxInjectionIncludedRefs {
		t.Fatalf("fixture must sit exactly at the ref bound: have %d refs, maxInjectionIncludedRefs=%d.\n"+
			"If the bound moved, re-measure the section at the NEW bound — a cap proven at the old one proves nothing.",
			len(atBound), maxInjectionIncludedRefs)
	}

	var section strings.Builder
	writeIncludedList(&section, atBound, maxOrientationIncludedChars)
	got := utf8.RuneCountInString(section.String())
	if got > maxOrientationIncludedChars {
		t.Fatalf("Included section at the %d-ref bound is %d runes, over its %d cap (by %d).\n"+
			"The renderer is supposed to drop WHOLE refs to fit; if it is over, the drop loop is not working.",
			maxInjectionIncludedRefs, got, maxOrientationIncludedChars, got-maxOrientationIncludedChars)
	}
	t.Logf("Included at the %d-ref bound with tiers: %d runes of cap %d (headroom %d)",
		maxInjectionIncludedRefs, got, maxOrientationIncludedChars, maxOrientationIncludedChars-got)

	pack := envelope.ContextPack{SummaryMD: "summary", Included: atBound}
	pack.Header.ID = "ctx_tier_bound"
	rendered := renderInjection(injectionRender{HookID: "hookev_tier_bound", Pack: pack, Surface: injectionSurfaceOrientation})
	idx := strings.Index(rendered, "## Included")
	if idx < 0 {
		t.Fatalf("orientation digest lost the Included section at the ref bound:\n%s", rendered)
	}
	tail := rendered[idx:]
	if cut := strings.Index(tail, "\n## "); cut > 0 {
		tail = tail[:cut]
	}
	// The whole point of the drop loop: the section must never hand the reader a severed
	// ref. A truncation marker here means the rune-cut path ran after all.
	if strings.Contains(tail, "Section truncated") {
		t.Fatalf("Included section truncated at the served pack — refs must be DROPPED whole, not cut:\n%s", tail)
	}
	if !strings.Contains(tail, "`memory://relevance-gated` "+includedRefTrustMarker) {
		t.Fatalf("the one demoted ref at the bound lost its marker, so it now reads as verify-first:\n%s", tail)
	}
	// Every ref that rendered must be a complete line. A reason may be shortened
	// only when the renderer marks the loss explicitly.
	for _, ref := range atBound {
		prefix := "- `" + ref.Ref + "`"
		start := strings.Index(tail, prefix)
		if start < 0 {
			continue // dropped for budget, and the omission line below accounts for it
		}
		rest := tail[start:]
		end := strings.Index(rest, "\n")
		if end < 0 {
			t.Fatalf("ref %q rendered without a complete line:\n%s", ref.Ref, tail)
		}
		line := rest[:end]
		if !strings.HasSuffix(line, "reason="+ref.Reason) &&
			!strings.HasSuffix(line, "…[elided]") {
			t.Fatalf("ref %q reason was silently severed instead of explicitly elided:\n%s", ref.Ref, tail)
		}
	}
}

// TestIncludedWholeLineRepairKeepsTheExistingCap pins that rendering changes
// fit the existing section budget instead of taking budget from unrelated
// orientation sections.
func TestIncludedWholeLineRepairKeepsTheExistingCap(t *testing.T) {
	const unchangedCap = 1100
	if maxOrientationIncludedChars != unchangedCap {
		t.Fatalf(
			"whole-line repair raised Included cap: got=%d want=%d; fit whole refs inside the existing budget",
			maxOrientationIncludedChars,
			unchangedCap,
		)
	}

	refs := liveControlPlanePack()
	for i := len(refs); i < 109; i++ {
		refs = append(refs, envelope.IncludedRef{
			Ref:         fmt.Sprintf("decision://DEC-%04d", i),
			SourceClass: "runtime",
			Reason:      "accepted decision",
			Confidence:  1,
			Status:      "current",
		})
	}
	pack := envelope.ContextPack{SummaryMD: "summary", Included: refs}
	pack.Header.ID = "ctx_tier_existing_cap"
	rendered := renderInjection(injectionRender{
		HookID:  "hookev_tier_existing_cap",
		Pack:    pack,
		Surface: injectionSurfaceOrientation,
	})
	idx := strings.Index(rendered, "## Included")
	if idx < 0 {
		t.Fatalf("orientation digest lost Included section:\n%s", rendered)
	}
	section := rendered[idx:]
	if cut := strings.Index(section, "\n## "); cut > 0 {
		section = section[:cut]
	}
	if strings.Contains(section, "Section truncated") {
		t.Fatalf("Included section used generic rune truncation:\n%s", section)
	}
	if got := utf8.RuneCountInString(section); got > unchangedCap {
		t.Fatalf("Included section exceeded unchanged cap: got=%d want<=%d\n%s", got, unchangedCap, section)
	}
	if !strings.Contains(section, "`decision://DEC-0001`") {
		t.Fatalf("budgeting dropped the session-specific tail ref before redundant source provenance:\n%s", section)
	}
	displayed := 0
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "- `") {
			displayed++
		}
	}
	wantOmission := fmt.Sprintf(
		"- %d more included ref(s) omitted digest budget; see ContextPack JSON for full provenance",
		len(refs)-displayed,
	)
	if !strings.Contains(section, wantOmission) {
		t.Fatalf("Included omission row is inaccurate; want %q:\n%s", wantOmission, section)
	}
}

func TestIncludedListExactlyAtCapIsLossless(t *testing.T) {
	refs := liveControlPlanePack()

	var full strings.Builder
	writeIncludedList(&full, refs, 10_000)
	exactCap := utf8.RuneCountInString(full.String())

	var exact strings.Builder
	writeIncludedList(&exact, refs, exactCap)

	if exact.String() != full.String() {
		t.Fatalf(
			"Included section that fits exactly at its cap must stay lossless (cap=%d):\nfull:\n%s\nexact:\n%s",
			exactCap,
			full.String(),
			exact.String(),
		)
	}
}

// TestIncludedSelectionPreservesRuntimeRefsAheadOfSourceRefs pins the live
// preselection contract: when the served pack carries more refs than the ref
// bound, the renderer must shed redundant SourceClass=source provenance so
// session-specific runtime/generated refs survive — DEC-0001 sits AFTER the
// sixth source ref and the memory ref, so a prefix slice never reaches it.
//
// A live SessionStart pack observed maxInjectionIncludedRefs=8 select six
// source refs + memory + first hook, omitting decision://DEC-0001 because the
// prefix slice never reached the runtime tail. The preselection must not sever
// DEC-0001 just because it sits after six source refs and the memory ref in the
// served order.
func TestIncludedSelectionPreservesRuntimeRefsAheadOfSourceRefs(t *testing.T) {
	const decisionReason = "accepted decision from Compound memory ledger; old source is source-in-place evidence"
	const hookReason = "installed hook selected for harness and task"
	const ambient = "ambient for control-plane repos"
	// Captured live served shape: six source refs, memory, four hook runtime
	// refs, then decision://DEC-0001. The remaining refs land after the
	// decision as additional runtime decisions.
	refs := []envelope.IncludedRef{
		{Ref: "AGENTS.md", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "skills/README.md", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "constitution://universal-principles", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "control-plane://orientation-map", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "control-plane://workflow-contract", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "control-plane://sop-packets", SourceClass: "source", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "memory://relevance-gated", SourceClass: "generated", Reason: ambient, Confidence: 1, Status: "current"},
		{Ref: "hook://codex/PreToolUse", SourceClass: "runtime", Reason: hookReason, Confidence: 1, Status: "current"},
		{Ref: "hook://codex/PostToolUse", SourceClass: "runtime", Reason: hookReason, Confidence: 1, Status: "current"},
		{Ref: "hook://codex/SessionStart", SourceClass: "runtime", Reason: hookReason, Confidence: 1, Status: "current"},
		{Ref: "hook://codex/UserPromptSubmit", SourceClass: "runtime", Reason: hookReason, Confidence: 1, Status: "current"},
		{Ref: "decision://DEC-0001", SourceClass: "runtime", Reason: decisionReason, Confidence: 1, Status: "current"},
	}
	for i := 2; len(refs) < 108; i++ {
		refs = append(refs, envelope.IncludedRef{
			Ref:         fmt.Sprintf("decision://DEC-%04d", i),
			SourceClass: "runtime",
			Reason:      decisionReason,
			Confidence:  1,
			Status:      "current",
		})
	}
	if len(refs) != 108 {
		t.Fatalf("fixture must serve 108 refs, got %d", len(refs))
	}

	pack := envelope.ContextPack{SummaryMD: "summary", Included: refs}
	pack.Header.ID = "ctx_live_ref_selection"
	rendered := renderInjection(injectionRender{
		HookID:  "hookev_live_ref_selection",
		Pack:    pack,
		Surface: injectionSurfaceOrientation,
	})
	idx := strings.Index(rendered, "## Included")
	if idx < 0 {
		t.Fatalf("orientation digest lost Included section:\n%s", rendered)
	}
	section := rendered[idx:]
	if cut := strings.Index(section, "\n## "); cut > 0 {
		section = section[:cut]
	}
	if strings.Contains(section, "Section truncated") {
		t.Fatalf("Included section used generic rune truncation:\n%s", section)
	}
	if got := utf8.RuneCountInString(section); got > maxOrientationIncludedChars {
		t.Fatalf("Included section exceeded unchanged cap: got=%d want=%d\n%s", got, maxOrientationIncludedChars, section)
	}
	// Every session-specific runtime/generated ref named on the live hook must survive.
	for _, ref := range []string{
		"decision://DEC-0001",
		"memory://relevance-gated",
		"hook://codex/PreToolUse",
		"hook://codex/PostToolUse",
		"hook://codex/SessionStart",
		"hook://codex/UserPromptSubmit",
	} {
		if !strings.Contains(section, "`"+ref+"`") {
			t.Fatalf("session-specific ref %q was shed before redundant source refs:\n%s", ref, section)
		}
	}
	// The trust marker on the demoted memory ref survives budgeting.
	if !strings.Contains(section, "`memory://relevance-gated` "+includedRefTrustMarker) {
		t.Fatalf("the demoted memory ref lost its marker under budgeting:\n%s", section)
	}
	// Original relative order is preserved among refs that were displayed: no
	// ref whose original index is HIGHER may appear before one whose original
	// index is lower.
	origIdx := map[string]int{}
	for i, ref := range refs {
		origIdx[ref.Ref] = i
	}
	var displayed []string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "- `") {
			continue
		}
		start := strings.Index(line, "`")
		end := strings.Index(line[start+1:], "`")
		if start < 0 || end < 0 {
			continue
		}
		ref := line[start+1 : start+1+end]
		if _, ok := origIdx[ref]; ok {
			displayed = append(displayed, ref)
		}
	}
	for i := 0; i < len(displayed); i++ {
		for j := i + 1; j < len(displayed); j++ {
			if origIdx[displayed[i]] > origIdx[displayed[j]] {
				t.Fatalf("displayed ref order violated: %q (orig=%d) appears before %q (orig=%d)\n%s",
					displayed[i], origIdx[displayed[i]], displayed[j], origIdx[displayed[j]], section)
			}
		}
	}
	// Omission count is total minus displayed — never a fixed number, never
	// capped by the bound.
	wantOmission := fmt.Sprintf(
		"- %d more included ref(s) omitted digest budget; see ContextPack JSON for full provenance",
		len(refs)-len(displayed),
	)
	if !strings.Contains(section, wantOmission) {
		t.Fatalf("omission row is inaccurate; want %q:\n%s", wantOmission, section)
	}
	// Every displayed ref line ends with a complete reason: either the full
	// reason text or an elision that carries the FULL explicit marker
	// `…[elided]`. A bare `reason=…` reads as a complete-but-terser reason and
	// is the exact live defect this regression pins — the renderer must never
	// substitute the marker with just the ellipsis when the room cannot fit the
	// full marker plus any prefix.
	origReason := map[string]string{}
	for _, ref := range refs {
		origReason[ref.Ref] = ref.Reason
	}
	for _, refName := range displayed {
		want := origReason[refName]
		fullLine := "- `" + refName + "`"
		start := strings.Index(section, fullLine)
		if start < 0 {
			t.Fatalf("displayed ref %q not found in section:\n%s", refName, section)
		}
		rest := section[start:]
		end := strings.Index(rest, "\n")
		if end < 0 {
			t.Fatalf("displayed ref %q rendered without a complete line (no newline):\n%s", refName, section)
		}
		line := rest[:end]
		reasonIdx := strings.Index(line, "reason=")
		if reasonIdx < 0 {
			t.Fatalf("displayed ref %q rendered without a reason field:\n%s", refName, line)
		}
		reasonPart := line[reasonIdx+len("reason="):]
		endsFull := strings.HasSuffix(line, "reason="+want)
		endsElided := strings.HasSuffix(line, "…[elided]")
		switch {
		case endsFull:
			// reason fit in full; no marker is the honest shape.
		case endsElided:
			// reason was shortened but carries the FULL explicit marker.
		default:
			t.Fatalf("displayed ref %q reason did not complete: want full or `…[elided]` suffix, got reason=%q (line=%q)\n"+
				"a bare `reason=…` reads as a complete reason with no loss marker and is the live defect.",
				refName, reasonPart, line)
		}
	}
}

func TestIncludedListPreservesDynamicTailWhenNoStaticRefCanBeShed(t *testing.T) {
	refs := make([]envelope.IncludedRef, 0, maxInjectionIncludedRefs)
	for i := 0; i < maxInjectionIncludedRefs; i++ {
		refs = append(refs, envelope.IncludedRef{
			Ref:         fmt.Sprintf("decision://runtime-%02d", i),
			SourceClass: "runtime",
			Reason:      strings.Repeat("session-specific evidence ", 8),
			Confidence:  1,
			Status:      "current",
		})
	}
	tailRef := refs[len(refs)-1].Ref

	var section strings.Builder
	writeIncludedList(&section, refs, maxOrientationIncludedChars)

	if !strings.Contains(section.String(), "`"+tailRef+"`") {
		t.Fatalf("dynamic tail ref %q was shed before an older dynamic ref:\n%s", tailRef, section.String())
	}
}

// TestIncludedListElidesReasonsRatherThanSeveringOrDroppingTheTail pins the repair, using an
// explicit tight limit so it tests the renderer's property rather than today's cap value.
//
// The trust legend pushed the live control-plane pack from 977 to 1105 runes
// against a 1100 cap, so the generic cap severed `decision://DEC-0001`
// mid-reason. Ref identity must survive; reason loss must be explicit.
func TestIncludedListElidesReasonsRatherThanSeveringOrDroppingTheTail(t *testing.T) {
	pack := liveControlPlanePack()
	pack[len(pack)-1].Reason += strings.Repeat(" durable evidence", 16)
	// Tight enough to force elision, but not below the identity floor. The extended fixture
	// exceeds 1200 runes in full, so the elision path runs while still leaving each of the
	// 8 refs more than minIncludedRefChars, which is what identity costs. Below that floor
	// refs MUST drop, and that is the backstop behaving correctly, not an identity guarantee.
	const tight = 1200

	var section strings.Builder
	writeIncludedList(&section, pack, tight)
	out := section.String()

	if n := utf8.RuneCountInString(out); n > tight {
		t.Fatalf("section is %d runes against a %d limit — the budget loop failed to bound it:\n%s", n, tight, out)
	}
	// No severed reason: the live defect rendered DEC-0001's reason cut mid-phrase with
	// nothing marking the cut.
	if strings.Contains(out, "reason=accepted decision\n") {
		t.Fatalf("`decision://DEC-0001` rendered with a silently severed reason — the exact live defect:\n%s", out)
	}
	// Every ref keeps its identity, including the tail ref that a drop-the-tail renderer
	// would have discarded.
	for _, ref := range pack {
		if !strings.Contains(out, "`"+ref.Ref+"`") {
			t.Fatalf("ref %q lost its identity under budget; refs may lose PROSE, never presence:\n%s", ref.Ref, out)
		}
	}
	// The demotion survives budgeting — losing a marker silently elevates a ref.
	if !strings.Contains(out, "`memory://relevance-gated` "+includedRefTrustMarker) {
		t.Fatalf("the demoted ref lost its marker under budget, so it now reads as verify-first:\n%s", out)
	}
	// Anything trimmed is ANNOUNCED. An unmarked trim reads as a complete-but-terser reason.
	if !strings.Contains(out, "…[elided]") {
		t.Fatalf("the pack cannot fit %d runes in full, so something was trimmed without saying so:\n%s", tight, out)
	}
}

// TestInjectionSurfacesPartitionTheSectionsWithoutOverlap pins the property that
// makes the split worth doing: each section is served by exactly ONE surface.
//
// A section on both surfaces is the duplication the split removed, paid for twice
// per turn. A section on neither is worse — it silently stops reaching agents
// while every cap and every per-surface test stays green, which is precisely how
// the orientation tail went undelivered for 2714 digests.
func TestInjectionSurfacesPartitionTheSectionsWithoutOverlap(t *testing.T) {
	full := envelope.ContextPack{
		SummaryMD:          "summary body",
		ActiveInstructions: []string{"AGENTS.md"},
		ActiveHooks:        []string{"hook://claude_code/PreToolUse"},
		ActiveDecisions:    []string{"decision://DEC-0001"},
		RequiredChecks:     []string{"pre_write_gate claude_code/PreToolUse status=missing_gate: run X before mutation"},
		Avoid:              []string{"self-model"},
		ActiveMemory:       []string{"memory://one"},
		Negatives: []envelope.NegativeCard{
			{Source: "failure_pattern", ApproachKey: "approach-one", Ref: "candidate://failure_pattern/cand_one"},
		},
		CapabilityOffers: []envelope.CapabilityOffer{
			{Ref: "capability://skill:example", Kind: "skill", Name: "example", Score: 1},
		},
		Included: []envelope.IncludedRef{
			{Ref: "AGENTS.md", SourceClass: "source", Status: "current", Confidence: 1, Reason: "ambient"},
		},
		Excluded: []envelope.ExcludedRef{{Ref: "control-plane://orientation-map", Reason: "out of scope"}},
		HookReadiness: []envelope.HookReadiness{
			{Harness: "claude_code", Hook: "PreToolUse", Status: "missing_gate", Installed: false},
		},
		HookRoster: []envelope.HookRosterEntry{
			{Ref: "hook://claude_code/PreToolUse", Harness: "claude_code", Hook: "PreToolUse", Event: "PreToolUse", Status: "installed"},
		},
	}
	full.Header.ID = "ctx_partition"

	task := renderInjection(injectionRender{HookID: "hookev_task", Pack: full, Surface: injectionSurfaceTask, OrientationNote: "- Orientation served this session at `hookev_x`"})
	orientation := renderInjection(injectionRender{HookID: "hookev_orientation", Pack: full, Surface: injectionSurfaceOrientation})

	// heading -> the surface that must carry it.
	for heading, want := range map[string]injectionSurface{
		"## Active Memory":            injectionSurfaceTask,
		"## Already Tried And Failed": injectionSurfaceTask,
		"## Active Decisions":         injectionSurfaceTask,
		"## Required Checks":          injectionSurfaceTask,
		"## Forbidden Recall Scopes":  injectionSurfaceTask,
		"## Capability Offers":        injectionSurfaceTask,
		"## Summary":                  injectionSurfaceOrientation,
		"## Active Instructions":      injectionSurfaceOrientation,
		"## Active Hooks":             injectionSurfaceOrientation,
		"## Included":                 injectionSurfaceOrientation,
		"## Excluded":                 injectionSurfaceOrientation,
		"## Hook Gate Readiness":      injectionSurfaceOrientation,
		"## Hook Roster":              injectionSurfaceOrientation,
	} {
		inTask := strings.Contains(task, heading)
		inOrientation := strings.Contains(orientation, heading)
		switch {
		case inTask && inOrientation:
			t.Errorf("%q renders on BOTH surfaces; the split exists to stop paying for it twice per turn", heading)
		case !inTask && !inOrientation:
			t.Errorf("%q renders on NEITHER surface; it has silently stopped reaching agents", heading)
		case want == injectionSurfaceTask && !inTask:
			t.Errorf("%q must render on the task surface (it is computed per prompt), got orientation", heading)
		case want == injectionSurfaceOrientation && !inOrientation:
			t.Errorf("%q must render on the orientation surface (it does not vary with the prompt), got task", heading)
		}
	}

	// Neither surface may amputate a realistic pack — the whole point of splitting.
	for name, text := range map[string]string{"task": task, "orientation": orientation} {
		if strings.Contains(text, "Compound context truncated") {
			t.Errorf("%s surface truncated a realistic pack:\n%s", name, text)
		}
		if got := utf8.RuneCountInString(text); got > MaxContextChars {
			t.Errorf("%s surface exceeded MaxContextChars: %d > %d", name, got, MaxContextChars)
		}
	}
}

// TestRenderInjectionOmitsEmptyForbiddenRecallScopes pins that an unset recall-scope
// restriction renders nothing at all.
//
// The channel carries mem.Forbidden, which is empty for control-plane repos, so
// "## Forbidden Recall Scopes / - none" was the rendered result on essentially every
// prompt. That is a false all-clear in a section whose heading promises a checked
// constraint: a reader cannot distinguish "no scope is forbidden" from "nothing
// produces this channel". Silence is the honest rendering of an absent constraint.
func TestRenderInjectionOmitsEmptyForbiddenRecallScopes(t *testing.T) {
	pack := envelope.ContextPack{SummaryMD: "summary"}
	pack.Header.ID = "ctx_test"

	text := renderInjection(injectionRender{HookID: "hookev_test", Pack: pack, Surface: injectionSurfaceTask})

	if strings.Contains(text, "Forbidden Recall Scopes") {
		t.Fatalf("empty recall-scope restriction still rendered a section:\n%s", text)
	}
	if strings.Contains(text, "## Avoid") {
		t.Fatalf("empty recall-scope restriction rendered the superseded label:\n%s", text)
	}

	// Non-empty must still render: suppression is about absence, not about the
	// channel being unimportant.
	pack.Avoid = []string{"self-model"}
	text = renderInjection(injectionRender{HookID: "hookev_test", Pack: pack, Surface: injectionSurfaceTask})
	if !strings.Contains(text, "## Forbidden Recall Scopes") || !strings.Contains(text, "self-model") {
		t.Fatalf("configured recall-scope restriction did not render:\n%s", text)
	}
}

// TestRenderInjectionSeparatesSectionHeadings pins the blank line before every
// rendered section heading.
//
// Three writers (capability offers, summary, included) emitted their heading with
// no leading newline and relied on a trailing newline from whichever section
// happened to precede them. Once an upstream section was suppressed or truncated
// mid-line, the heading fused to the previous line — observed live as
// "- none## Capability Offers" — and a fused ATX heading stops being a heading in
// every markdown reader that consumes the digest.
func TestRenderInjectionSeparatesSectionHeadings(t *testing.T) {
	pack := envelope.ContextPack{
		SummaryMD: "summary",
		Avoid:     []string{"self-model"},
		CapabilityOffers: []envelope.CapabilityOffer{
			{Ref: "capability://skill:example", Kind: "skill", Name: "example", Score: 1},
		},
		ActiveInstructions: []string{"AGENTS.md"},
		ActiveHooks:        []string{"hook://claude_code/PreToolUse"},
		Included: []envelope.IncludedRef{
			{Ref: "AGENTS.md", SourceClass: "source", Status: "current", Confidence: 1, Reason: "ambient"},
		},
	}
	pack.Header.ID = "ctx_test"

	text := renderInjection(injectionRender{HookID: "hookev_test", Pack: pack, Surface: injectionSurfaceTask})

	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			continue
		}
		if idx := strings.Index(line, "## "); idx > 0 {
			t.Errorf("section heading fused to preceding content, so it no longer parses as a heading: %q", line)
		}
	}
}

// TestOrientationNoteHasThreeStatesNotTwo pins the pointer's honesty.
//
// The pointer exists so a session that never received its orientation surface can
// tell. The failure mode a two-state pointer walks into is the mirror of the one it
// fixes: with no home or no session id there is no marker to look for, so a
// served/not-served pointer would report "NOT served" from missing plumbing and
// manufacture an alarm. A false "orientation missing" trains the reader to ignore
// the line, which costs exactly what the silent gap cost.
func TestOrientationNoteHasThreeStatesNotTwo(t *testing.T) {
	home := t.TempDir()

	t.Run("served names the hook that served it", func(t *testing.T) {
		if err := recordSessionOrientationServed(home, "session-served", "hookev_orient_1", fixedNow); err != nil {
			t.Fatal(err)
		}
		note := orientationNoteForSession(home, "session-served", "claude_code")
		if !strings.Contains(note, "Orientation served this session") || !strings.Contains(note, "hookev_orient_1") {
			t.Fatalf("served note must cite the serving hook: %q", note)
		}
		if !strings.Contains(note, "--harness claude_code") {
			t.Fatalf("note must carry a runnable re-serve command for THIS harness: %q", note)
		}
	})

	t.Run("provably not served says so", func(t *testing.T) {
		note := orientationNoteForSession(home, "session-never-started", "claude_code")
		if !strings.Contains(note, "Orientation NOT served this session") {
			t.Fatalf("a session with no marker must report the gap: %q", note)
		}
	})

	t.Run("unprovable claims neither", func(t *testing.T) {
		for name, note := range map[string]string{
			"no home":       orientationNoteForSession("", "session-x", "claude_code"),
			"no session id": orientationNoteForSession(home, "", "claude_code"),
		} {
			if strings.Contains(note, "NOT served") {
				t.Errorf("%s: cannot observe the session, so must not assert a missing orientation: %q", name, note)
			}
			if strings.Contains(note, "served this session at") {
				t.Errorf("%s: cannot observe the session, so must not assert it was served: %q", name, note)
			}
			if !strings.Contains(note, "compound orient") {
				t.Errorf("%s: note must still say how to get orientation: %q", name, note)
			}
		}
	})

	t.Run("session id never appears in clear on disk", func(t *testing.T) {
		const secret = "session-secret-abc123"
		if err := recordSessionOrientationServed(home, secret, "hookev_orient_2", fixedNow); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(sessionOrientationPath(home, secret))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("session id must be hashed, not stored in clear: %s", body)
		}
		if strings.Contains(sessionOrientationPath(home, secret), secret) {
			t.Fatalf("session id must be hashed in the path too: %s", sessionOrientationPath(home, secret))
		}
	})
}

// TestCapabilityOfferDropsWholeFieldsWorstFirst pins the field priority.
//
// The failure being prevented is specific and was observed live: the section cap
// truncated by rune count and cut INSIDE a `conflicts=` boundary list, so the offer
// rendered carrying one partial conflict when it actually had several. A severed
// constraint list does not read as a truncated offer — it reads as an offer with
// fewer conditions than it has, which is a policy surface quietly weakening itself.
// So truncation must drop WHOLE fields, worst-first, and say which.
func TestCapabilityOfferDropsWholeFieldsWorstFirst(t *testing.T) {
	long := func(prefix string, n int) []envelope.CapabilityOfferConstraint {
		out := make([]envelope.CapabilityOfferConstraint, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, envelope.CapabilityOfferConstraint{
				Kind: "boundary",
				Ref:  fmt.Sprintf("boundary://%s-%02d", prefix, i),
			})
		}
		return out
	}
	offer := envelope.CapabilityOffer{
		Ref:       "capability://procedure:example",
		Kind:      "procedure",
		Name:      "Example",
		Score:     7.8,
		Why:       "trigger×weight=2.0",
		Conflicts: long("must-not", 3),
		Required:  long("needs", 3),
		Evidence:  long("validated-by", 8),
		Harnesses: long("surfaces-with", 3),
	}

	// A budget that cannot hold everything: something must go.
	line, dropped := renderCapabilityOffer(offer, 300)

	if len(dropped) == 0 {
		t.Fatal("a budget too small for every field must report what it dropped")
	}
	// The head is identity — an offer without its ref is unactionable, so it is never
	// what gets sacrificed.
	if !strings.Contains(line, "capability://procedure:example") {
		t.Errorf("offer head must always survive: %q", line)
	}
	// why and conflicts are what a reader acts on: why it was offered, and what it
	// must not be used for.
	for _, want := range []string{"why=", "conflicts="} {
		if !strings.Contains(line, want) {
			t.Errorf("%s must outrank droppable fields, got %q (dropped %v)", want, line, dropped)
		}
	}
	// evidence and note repeat or merely justify; they go first.
	if !containsString(dropped, "evidence") {
		t.Errorf("evidence must be dropped before conflicts, dropped=%v", dropped)
	}
	if containsString(dropped, "conflicts") {
		t.Errorf("conflicts must never be dropped while lower-priority fields remain, dropped=%v", dropped)
	}

	// No field may be emitted partially: every `label=` present must be whole.
	// A half-rendered constraint list is the exact defect this replaces.
	for _, label := range []string{"conflicts=", "required=", "evidence=", "harnesses="} {
		if !strings.Contains(line, label) {
			continue
		}
		seg := line[strings.Index(line, label)+len(label):]
		if cut := strings.Index(seg, " "); cut >= 0 {
			seg = seg[:cut]
		}
		if strings.HasSuffix(seg, ",") || strings.HasSuffix(seg, "boundary:") {
			t.Errorf("field %s rendered partially (%q) — truncation must drop whole fields", label, seg)
		}
	}

	// Budget is respected including the omission marker the caller appends.
	if got := utf8.RuneCountInString(line); got > 300 {
		t.Errorf("rendered offer exceeded its per-offer budget: %d > 300", got)
	}
}

// TestCapabilityOfferSectionReportsOmittedOffers pins that offers dropped whole are
// counted. Silently rendering 3 of 9 offers reads as "there are 3 offers".
func TestCapabilityOfferSectionReportsOmittedOffers(t *testing.T) {
	offers := make([]envelope.CapabilityOffer, 0, 40)
	for i := 0; i < 40; i++ {
		offers = append(offers, envelope.CapabilityOffer{
			Ref:   fmt.Sprintf("capability://skill:example-%02d", i),
			Kind:  "skill",
			Name:  fmt.Sprintf("Example %02d", i),
			Score: 1,
			Why:   strings.Repeat("reason ", 20),
		})
	}
	var b strings.Builder
	writeCapabilityOfferList(&b, offers)
	out := b.String()

	shown := strings.Count(out, "- `capability://skill:example-")
	if shown == len(offers) {
		t.Skip("budget held every offer; nothing to report")
	}
	if !strings.Contains(out, "more offer(s) omitted from digest budget") {
		t.Fatalf("dropped offers must be counted, not silently absent (showed %d of %d):\n%s", shown, len(offers), out)
	}
}

// TestCapabilityOfferSectionNeverTruncatesMidField is the end-to-end guarantee.
//
// Built from the shape observed live: several verbose procedure offers whose
// `conflicts=` lists are long enough that the old renderer cut one in half. The
// section must stay inside its cap WITHOUT the generic section truncator ever having
// to fire, because that truncator cuts by rune count and cannot respect a field
// boundary.
func TestCapabilityOfferSectionNeverTruncatesMidField(t *testing.T) {
	boundaries := func(n int) []envelope.CapabilityOfferConstraint {
		out := make([]envelope.CapabilityOfferConstraint, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, envelope.CapabilityOfferConstraint{
				Kind:   "boundary",
				Ref:    fmt.Sprintf("boundary://landed-not-working-exercise-real-flow-observe-behavior-%02d", i),
				Detail: "landed is not working — exercise the real flow and observe behavior, not just tests",
			})
		}
		return out
	}
	offers := make([]envelope.CapabilityOffer, 0, 6)
	for i := 0; i < 6; i++ {
		offers = append(offers, envelope.CapabilityOffer{
			Ref:          fmt.Sprintf("capability://procedure:ceremony-%02d", i),
			Kind:         "procedure",
			Name:         "Compound Verify — prove a change is ALIVE (wired, fed, cut over) before calling it done",
			Score:        7.8,
			Why:          "trigger×weight=2.0 +semantic=0.64 +validated_by=5.0",
			Conflicts:    boundaries(3),
			Required:     boundaries(2),
			Evidence:     boundaries(4),
			CostRiskNote: "offer only; execution still requires WorkEnvelope, ActionIntent, and PolicyDecision",
		})
	}

	var raw strings.Builder
	writeCapabilityOfferList(&raw, offers)
	section := raw.String()

	// The renderer must fit its own cap unaided.
	if got := utf8.RuneCountInString(section); got > maxInjectionCapabilityOfferChars {
		t.Fatalf("offer section exceeded its cap unaided: %d > %d\n%s", got, maxInjectionCapabilityOfferChars, section)
	}
	// And therefore the generic truncator must have nothing to do.
	if capped := capInjectionSection(section, maxInjectionCapabilityOfferChars); strings.Contains(capped, "Section truncated") {
		t.Fatalf("offer section still needed rune-count truncation, which cuts inside fields:\n%s", capped)
	}

	// Every rendered field is whole: no line may end mid-constraint. A trailing comma
	// or a bare "boundary:" is the signature of the severed list observed live.
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "- `capability://") {
			continue
		}
		if strings.HasSuffix(line, ",") || strings.HasSuffix(line, "boundary:") || strings.HasSuffix(line, "=") {
			t.Errorf("offer line ends mid-field: %q", line)
		}
	}
	// Whatever was left out is stated, so the reader is not silently under-informed.
	shown := strings.Count(section, "- `capability://procedure:ceremony-")
	if shown < len(offers) && !strings.Contains(section, "more offer(s) omitted") {
		t.Errorf("showed %d of %d offers without saying so:\n%s", shown, len(offers), section)
	}
	t.Logf("rendered %d of %d offers in %d runes (cap %d)", shown, len(offers), utf8.RuneCountInString(section), maxInjectionCapabilityOfferChars)
}

// TestCapabilityOfferKeepsAConstraintCountWhenTheContentCannotFit pins the safety
// signal.
//
// With many offers sharing the section, no single offer can carry a long conflicts
// list whole. The first version of this renderer then dropped conflicts entirely
// while lower-priority fields survived (observed live: every offer rendered
// `harnesses=` and reported `conflicts` omitted), because the packer skipped an
// oversized field and kept filling with smaller ones. An offer whose conditions are
// invisible reads as unconditional, which is the failure mode the whole change
// exists to remove — so when the content cannot fit, the COUNT must survive.
func TestCapabilityOfferKeepsAConstraintCountWhenTheContentCannotFit(t *testing.T) {
	verbose := func(n int) []envelope.CapabilityOfferConstraint {
		out := make([]envelope.CapabilityOfferConstraint, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, envelope.CapabilityOfferConstraint{
				Kind:   "boundary",
				Ref:    fmt.Sprintf("boundary://a-very-long-boundary-reference-that-will-not-fit-%02d", i),
				Detail: "a long human-readable explanation of the boundary that pushes this field well past any per-offer budget",
			})
		}
		return out
	}
	offer := envelope.CapabilityOffer{
		Ref:       "capability://procedure:example",
		Kind:      "procedure",
		Name:      "Example",
		Score:     7.8,
		Why:       "trigger×weight=2.0",
		Conflicts: verbose(3),
		Required:  verbose(2),
		Harnesses: []envelope.CapabilityOfferConstraint{{Kind: "surfaces_with", Ref: "harness:claude_code"}},
	}

	line, dropped := renderCapabilityOffer(offer, 260)

	// The count form survives even though the content cannot.
	if !strings.Contains(line, "conflicts=3(omitted)") {
		t.Errorf("a conflicts list too long to render must still report its count, got %q (dropped %v)", line, dropped)
	}
	if !strings.Contains(line, "required=2(omitted)") {
		t.Errorf("a required list too long to render must still report its count, got %q", line)
	}
	// And "conflicts" must NOT be reported as fully dropped, because it wasn't.
	if containsString(dropped, "conflicts") {
		t.Errorf("conflicts reported as dropped while its count rendered: %q dropped=%v", line, dropped)
	}
	if got := utf8.RuneCountInString(line); got > 260 {
		t.Errorf("offer exceeded budget: %d > 260", got)
	}

	// Non-safety fields do not get a count form — they are genuinely droppable, and
	// spending budget announcing their absence would crowd out the counts that matter.
	if strings.Contains(line, "evidence=") || strings.Contains(line, "harnesses=1(omitted)") {
		t.Errorf("non-safety fields must not consume budget on count forms: %q", line)
	}
}
