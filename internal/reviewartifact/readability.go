package reviewartifact

import (
	"math"
	"strings"
	"unicode"
)

// ReadabilityGradeCeiling is the highest Flesch–Kincaid grade operator-facing
// daily surfaces (narrate scripts, digest letters, and the derived/fallback
// card prose they echo) may score. Eighth grade is the operator-legibility
// bar: anything denser reads as agent-speak, which is exactly what the
// plain-language card work exists to remove.
const ReadabilityGradeCeiling = 8.0

// GradeLevel scores text on the deterministic Flesch–Kincaid grade scale every
// operator-legibility check shares:
//
//	0.39 × (words / sentences) + 11.8 × (syllables / words) − 15.59
//
// The determinism contract is fixed (no locale, no dictionary, no network):
// sentences are segments split on . ! ? … ; : and newlines; words are runs of
// letters or digits; syllables are vowel groups (a e i o u y) with a
// silent-trailing-e adjustment — the e is kept for -le endings ("table",
// "cycle"), dropped otherwise ("make", "route"), and silent -ed endings drop
// their e-group except after t/d ("recorded", "waited" keep it, "joined" and
// "failed" lose it). A word always counts as at least one syllable, so digit
// tokens like "27" or "4KB" score 1. Empty or wordless text scores 0.
func GradeLevel(text string) float64 {
	words, syllables, sentences := countProse(text)
	if words == 0 {
		return 0
	}
	if sentences == 0 {
		sentences = 1
	}
	grade := 0.39*(float64(words)/float64(sentences)) + 11.8*(float64(syllables)/float64(words)) - 15.59
	return math.Round(grade*100) / 100
}

// Readable reports whether text sits at or below the shared grade ceiling.
func Readable(text string) bool {
	return GradeLevel(text) <= ReadabilityGradeCeiling
}

func isProseSentenceEnd(r rune) bool {
	switch r {
	case '.', '!', '?', '…', ';', ':', '\n', '\r':
		return true
	}
	return false
}

func isProseWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// countProse walks text once, counting words, syllables, and sentences that
// contain at least one word (a trailing terminator without words is not a
// sentence, so "…!" cannot deflate the average).
func countProse(text string) (words, syllables, sentences int) {
	var token strings.Builder
	sentenceHasWord := false
	flush := func() {
		if token.Len() == 0 {
			return
		}
		words++
		syllables += countSyllables(token.String())
		token.Reset()
		sentenceHasWord = true
	}
	endSentence := func() {
		if sentenceHasWord {
			sentences++
			sentenceHasWord = false
		}
	}
	for _, r := range text {
		switch {
		case isProseWordRune(r):
			token.WriteRune(unicode.ToLower(r))
		case isProseSentenceEnd(r):
			flush()
			endSentence()
		default:
			flush()
		}
	}
	flush()
	endSentence()
	return words, syllables, sentences
}

const proseVowels = "aeiouy"

func proseIsVowel(b byte) bool {
	return strings.IndexByte(proseVowels, b) >= 0
}

// countSyllables applies the documented vowel-group heuristic to one
// already-lowercased word token (letters and digits only).
func countSyllables(word string) int {
	groups := 0
	prevVowel := false
	for i := 0; i < len(word); i++ {
		v := proseIsVowel(word[i])
		if v && !prevVowel {
			groups++
		}
		prevVowel = v
	}
	if groups == 0 {
		return 1
	}
	if groups > 1 {
		switch {
		case strings.HasSuffix(word, "le") && len(word) > 2 && !proseIsVowel(word[len(word)-3]):
			// "table", "cycle": the trailing e sounds — keep it.
		case strings.HasSuffix(word, "e"):
			groups-- // silent e: "make", "route"
		case strings.HasSuffix(word, "ed") && !strings.HasSuffix(word, "ted") && !strings.HasSuffix(word, "ded"):
			groups-- // silent ed: "joined", "failed"; "recorded"/"waited" keep it
		}
	}
	if groups < 1 {
		groups = 1
	}
	return groups
}
