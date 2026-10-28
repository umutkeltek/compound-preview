package reviewartifact

import (
	"regexp"
	"strings"
	"testing"
)

// grammarBanlist mirrors the exact regex ValidateOperatorCard enforces (S1
// falsifier, spec console-legibility-design-spec.md section 2 G3): no raw ids,
// key=value pairs, paths, or file extensions in operator-facing text.
var grammarBanlist = regexp.MustCompile(`(=|--|_[a-f0-9]{8,}|/Users/|\.json|\.md)`)

func TestSanitizeFragmentStripsBracketTags(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"classify-extract [session PRIVACY-SENSITIVE: verdict LOCAL judge only] on vast-batch", "classify-extract on vast-batch"},
		{"[synthetic] classify-extract fixture", "classify-extract fixture"},
		{"no tags here", "no tags here"},
		{"  extra   spaces   [tag]  more  ", "extra spaces more"},
	}
	for _, c := range cases {
		if got := SanitizeFragment(c.in); got != c.want {
			t.Errorf("SanitizeFragment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShortLabelTruncatesAtWordBoundaryNeverMidToken(t *testing.T) {
	// This is the exact input shape that produced the "clas…" mid-word bug:
	// a long free-text summary carrying a bracket tag, truncated to 34 runes
	// (the surface field's cap in the panel comparison template).
	in := "classify-extract [session PRIVACY-SENSITIVE: verdict LOCAL judge only] classify-extract on vast-batch/claude"
	got := ShortLabel(in, 34)
	if strings.Contains(got, "[") || strings.Contains(got, "]") {
		t.Fatalf("ShortLabel leaked a bracket tag: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("ShortLabel(%q, 34) = %q, want an ellipsis suffix", in, got)
	}
	body := strings.TrimSuffix(got, "…")
	// Every truncated word in the source must appear whole in the output;
	// a mid-token cut ("clas…") would leave a body word that is a strict,
	// non-space-bounded prefix of a source word rather than a whole one.
	sourceWords := map[string]bool{}
	for _, w := range strings.Fields(SanitizeFragment(in)) {
		sourceWords[w] = true
	}
	for _, w := range strings.Fields(body) {
		if !sourceWords[w] {
			t.Fatalf("ShortLabel(%q, 34) = %q, produced a mid-token fragment %q", in, got, w)
		}
	}
	if got := ShortLabel("a-single-run-on-token-with-no-spaces-anywhere-in-it-at-all-x", 10); utf8RuneCount(got) > 10 {
		t.Fatalf("ShortLabel must still respect max when no word boundary exists: %q", got)
	}
}

func utf8RuneCount(s string) int {
	return len([]rune(s))
}

// TestOperatorCardGrammarFalsifier is the shared falsifier every fixed/new
// template must pass: no template output may match the banlist regex, and
// none may exceed the 160-char sentence cap (spec section 2 G1/G3).
func TestOperatorCardGrammarFalsifier(t *testing.T) {
	samples := []string{
		"The judge panel says ornith beat hauhaucs at classify-extract on a private transcript. Record the win?",
		"The judge panel says deepseek lost to autoextract on classify-extract. Record the loss?",
		"Repo intake kept mksglu/context-mode as reference evidence for Agent Workbench — nothing installed or run. File it?",
		"A recall bake-off ran 66 golden queries against gbrain. Read the scores.",
		"Memory synthesis finished a sweep — proposals recorded, nothing applied. Pick at most one repair to open.",
		"Memory holds two claims about decisionlifecycle decision lifecycle status that disagree. Pick which is true?",
		"The route watcher recorded outward delta route evidence. Read the outcome.",
		"Capability screening proposes a lifecycle change touching 5 evidence hooks. Review the proposal?",
	}
	for _, s := range samples {
		if grammarBanlist.MatchString(s) {
			t.Errorf("banlist violation in sample: %q", s)
		}
		if utf8RuneCount(s) > 160 {
			t.Errorf("sample exceeds 160 chars (%d): %q", utf8RuneCount(s), s)
		}
	}
}
