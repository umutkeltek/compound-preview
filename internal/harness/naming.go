package harness

import (
	"fmt"
	"regexp"
	"strings"
)

// Enforcement for docs/NAMING.md. A naming document without a test is a naming
// document that drifts, and every rule here exists because this estate already
// paid for its violation:
//
//   - a lane whose id carried model AND effort ended up with a record that
//     disagreed with itself (effort=xhigh in one field, "ultra" in another), so
//     nothing could say which was authoritative;
//   - two lanes differing by one suffix, one free and one api-metered, made cost
//     inferable only from a name -- a billing accident waiting to happen;
//   - a `_full` profile turned out to have FEWER capabilities than the
//     unsuffixed one, because the suffix described an intended subscription tier
//     while capability came from somewhere else entirely.
//
// These are cheap to check and expensive to discover, which is the whole reason
// to spend a test on them.

var snakeCaseName = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// comparativeSuffixes imply an ordering the system does not enforce. The reader
// infers "more than the unsuffixed one" and the code never checked.
var comparativeSuffixes = []string{"_full", "_max", "_pro", "_lite", "_mini", "_plus", "_basic"}

// effortTokens belong in an `effort` FIELD. In a name they duplicate a value
// that lives elsewhere, and the two copies then disagree.
var effortTokens = []string{"ultra", "xhigh", "highest", "lowest", "turbo", "fast", "slow"}

// costTokens belong in a `cost_class` FIELD. Cost must never be inferable only
// from a name: the reader who misreads it pays real money.
var costTokens = []string{"free", "paid", "metered", "cheap", "premium", "trial"}

// ValidateHarnessName reports every way a harness id violates docs/NAMING.md.
// It returns all violations rather than the first, because a name usually breaks
// more than one rule and fixing them one round trip at a time is the pattern
// this whole document exists to stop.
func ValidateHarnessName(name string) []string {
	var violations []string
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return []string{"harness id is empty"}
	}
	if !snakeCaseName.MatchString(trimmed) {
		violations = append(violations, fmt.Sprintf(
			"harness id %q is not snake_case: harness ids use underscores, lane routes use slashes, and mixing them is how one thing acquires three spellings",
			trimmed,
		))
	}
	for _, suffix := range comparativeSuffixes {
		if strings.HasSuffix(trimmed, suffix) {
			violations = append(violations, fmt.Sprintf(
				"harness id %q carries comparative suffix %q, which implies an ordering nothing enforces",
				trimmed, suffix,
			))
		}
	}
	// Match on segments, not substrings: a substring check would flag a vendor
	// whose name merely contains a token and teach everyone to ignore the rule.
	segments := strings.Split(trimmed, "_")
	for _, seg := range segments {
		for _, token := range effortTokens {
			if seg == token {
				violations = append(violations, fmt.Sprintf(
					"harness id %q encodes effort (%q); effort is a field, and a second copy in the name will disagree with it",
					trimmed, token,
				))
			}
		}
		for _, token := range costTokens {
			if seg == token {
				violations = append(violations, fmt.Sprintf(
					"harness id %q encodes cost (%q); cost_class is a field, and cost must never be inferable only from a name",
					trimmed, token,
				))
			}
		}
	}
	return violations
}
