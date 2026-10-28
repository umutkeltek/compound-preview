package work

import "testing"

// TestCommandShapedPredicateClassifies proves the standing-proof gate is CONSERVATIVE: it
// qualifies a predicate only when the WHOLE trimmed value reads as a single re-runnable command
// (recognized executable + arg, a real `go` subcommand, or a path-form invocation), and rejects
// ordinary prose predicates -- which are the overwhelming majority in the wild ("candidate reaches
// main", "a state delta exists"). A false-positive here would mint a spurious graduation offer, so
// the prose cases are the ones that must fail if the depth were wrong.
func TestCommandShapedPredicateClassifies(t *testing.T) {
	t.Parallel()
	qualifies := []struct{ pred, want string }{
		{"go test ./internal/work/...", "go test ./internal/work/..."},
		{"  go vet ./...  ", "go vet ./..."}, // trimmed
		{"compound proof-check --ci", "compound proof-check --ci"},
		{"proof-check --ci-console", "proof-check --ci-console"},
		{"bun test", "bun test"},
		{"bunx tsc --noEmit", "bunx tsc --noEmit"},
		{"pytest tests/unit", "pytest tests/unit"},
		{"./scripts/verify.sh", "./scripts/verify.sh"},
		{"/usr/bin/true", "/usr/bin/true"},
	}
	for _, tc := range qualifies {
		got, ok := CommandShapedPredicate(tc.pred)
		if !ok {
			t.Errorf("CommandShapedPredicate(%q) ok=false, want true", tc.pred)
			continue
		}
		if got != tc.want {
			t.Errorf("CommandShapedPredicate(%q) command=%q, want %q", tc.pred, got, tc.want)
		}
	}

	rejects := []string{
		"",                                     // empty
		"   ",                                  // whitespace only
		"a state delta exists",                 // prose
		"candidate reaches main",               // prose
		"admitted create-plan becomes work",    // prose
		"go to the dashboard and confirm",      // English "go" that is NOT a go subcommand
		"go",                                   // bare go, no subcommand
		"compound",                             // bare executable, no arg
		"bun",                                  // bare executable, no arg
		"make the tests pass",                  // "make" deliberately excluded (English-ambiguous)
		"the app builds and screenshots\nlook", // multi-line spec block
		"ensure go test passes before landing", // command mention buried in prose (head is "ensure")
	}
	for _, pred := range rejects {
		if got, ok := CommandShapedPredicate(pred); ok {
			t.Errorf("CommandShapedPredicate(%q) ok=true (command=%q), want false", pred, got)
		}
	}
}
