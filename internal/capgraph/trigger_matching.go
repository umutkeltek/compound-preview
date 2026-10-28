package capgraph

// taskTokenMatches decides whether a task mentioned a trigger token, tolerating
// the singular/plural split.
//
// This exists because of a measured failure, not a hypothetical one.
// skill:paywalls declares the trigger "paywalls" and not "paywall", so the task
// "the paywall is not converting" matched it on nothing and ranked it 29th on
// its own topic. The defect was invisible while reputation could surface a
// capability with no topical match: the skill still appeared, just for a reason
// that had nothing to do with the task, and it appeared equally for a Go
// concurrency bug. Masking a data defect with a popularity score is worse than
// the defect, because nobody goes looking for it.
//
// Deliberately only trailing "s". Real stemming would also collapse pairs that
// mean different things in this vocabulary, and a router that quietly conflates
// two capabilities is harder to debug than one that misses a plural.
func taskTokenMatches(taskTokens map[string]bool, tok string) bool {
	if taskTokens[tok] {
		return true
	}
	if len(tok) > 3 {
		if singular, ok := trimTrailing(tok, "s"); ok && taskTokens[singular] {
			return true
		}
	}
	if len(tok) >= 3 && taskTokens[tok+"s"] {
		return true
	}
	return false
}

func trimTrailing(s, suffix string) (string, bool) {
	if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)], true
	}
	return "", false
}
