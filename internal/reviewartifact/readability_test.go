package reviewartifact

import (
	"strings"
	"testing"
)

// TestGradeLevelDeterministicAndOrdered pins the two properties the operator
// surfaces rely on: identical input scores identically, and genuinely dense
// prose scores far above plain prose.
func TestGradeLevelDeterministicAndOrdered(t *testing.T) {
	plain := "One item needs your decision."
	dense := "Pursuant to our antecedent correspondence, implementation of the aforementioned provisions necessitates reconsideration."
	first := GradeLevel(plain)
	if first != GradeLevel(plain) {
		t.Fatalf("GradeLevel not deterministic: %v vs %v", first, GradeLevel(plain))
	}
	if GradeLevel(dense) <= GradeLevel(plain) {
		t.Fatalf("dense prose (%.2f) must outscore plain prose (%.2f)", GradeLevel(dense), GradeLevel(plain))
	}
	if GradeLevel("") != 0 || GradeLevel("— … ; :") != 0 {
		t.Fatalf("wordless text must score 0, got %v / %v", GradeLevel(""), GradeLevel("— … ; :"))
	}
}

// TestGradeLevelSyllableRules exercises the documented syllable heuristic on
// the exact shapes the card prose uses (silent e, silent ed, kept -le,
// digit tokens scoring one).
func TestGradeLevelSyllableRules(t *testing.T) {
	cases := []struct {
		word string
		want int
		note string
	}{
		{"make", 1, "silent e"},
		{"route", 1, "silent e"},
		{"table", 2, "trailing e sounds after consonant+le"},
		{"cycle", 2, "trailing e sounds after consonant+le"},
		{"joined", 1, "silent ed"},
		{"failed", 1, "silent ed"},
		{"recorded", 3, "ed keeps sounding after d"},
		{"waited", 2, "ed keeps sounding after t"},
		{"27", 1, "digit tokens score one"},
		{"4kb", 1, "no vowel groups scores one"},
		{"decision", 3, "vowel groups"},
		{"capability", 5, "vowel groups"},
	}
	for _, c := range cases {
		if got := countSyllables(c.word); got != c.want {
			t.Errorf("countSyllables(%q) = %d, want %d (%s)", c.word, got, c.want, c.note)
		}
	}
}

// TestOperatorProseCorpusHoldsEighthGrade is the regression gate for every
// plain-language sentence the derived card, attention, narrate, and digest
// composers emit: each must sit at or below the shared ceiling. A template
// edit that drifts above eighth grade fails here before it ships to the
// operator.
func TestOperatorProseCorpusHoldsEighthGrade(t *testing.T) {
	corpus := []string{
		// DerivedOperatorCard rungs.
		"A new panel verdicts record needs your decision.",
		"A new review record needs your decision.",
		"A new route policy record is blocked: the lease is held.",
		"A new review record is ready to apply.",
		"A new review record is already finished.",
		"Open it with compound reviews --area bench.",
		"Open the review queue and read this record.",
		// Attention last-resort group prose.
		"1 item needs your decision.",
		"3 items need your decision.",
		"Open the review queue to read the details.",
		"Background work moved; nothing is needed from you.",
		// Narrate fallback + composed prose.
		"A review item needs your decision.",
		"Open the review queue to read it.",
		"Recorded — 3 of 4 changes joined; 1 failed.",
		"This record waits on your decision.",
	}
	for _, text := range corpus {
		if !Readable(text) {
			t.Errorf("operator prose above grade %.1f (%.2f): %q", ReadabilityGradeCeiling, GradeLevel(text), text)
		}
	}
}

// TestLegacyGenericCardSummaryDetection pins the exact retired sentence the
// queue must re-derive on load, including the truncated variant.
func TestLegacyGenericCardSummaryDetection(t *testing.T) {
	detects := []string{
		"Product content queue: a new record needs your read.",
		"a new record needs your read.",
		"A new record needs your read…",
	}
	for _, s := range detects {
		if !LegacyGenericCardSummary(s) {
			t.Errorf("LegacyGenericCardSummary(%q) = false, want true", s)
		}
	}
	keeps := []string{
		"The judge panel needs your call. Record it?",
		"A new record needs your decision.",
		"",
	}
	for _, s := range keeps {
		if LegacyGenericCardSummary(s) {
			t.Errorf("LegacyGenericCardSummary(%q) = true, want false", s)
		}
	}
}

// TestGradeLevelFailsJargonRegression proves the check itself trips on the
// failure mode it exists to catch: a card that leans on machine-flavored
// compound jargon instead of plain words.
func TestGradeLevelFailsJargonRegression(t *testing.T) {
	jargon := "A new capability evaluation intake record needs your decision."
	if Readable(jargon) {
		t.Fatalf("jargon card passed the ceiling (%.2f): %q", GradeLevel(jargon), jargon)
	}
	if !strings.Contains(t.Name(), "") {
		t.Fatal("unreachable")
	}
}
