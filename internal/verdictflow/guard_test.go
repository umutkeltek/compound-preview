package verdictflow

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// This is the permanent one-implementation guard for the verdict-bridge seam.
//
// R1 (landed 980d26fd) extracted the shared plan-record / load / annotate-loop /
// extras-scan / shell-quoting helpers out of the three panel-verdict bridges
// (product, sourceobs, capgraph) into this package. Its acceptance teeth were
// PROCEDURAL (a one-time scratch mutation that reddened the domain goldens);
// nothing permanent stopped a future edit from silently copying a helper back
// into a bridge and re-forking the behavior.
//
// This guard reads the three bridge SOURCES as text (go test runs with cwd =
// this package dir, so the "../<pkg>/..." relative paths resolve to the sibling
// packages) and asserts BOTH directions of the law:
//
//	(a) each bridge still contains the exact delegation call sites it must, and
//	(b) no bridge re-defines a local copy of an extracted helper (by function
//	    name OR by a distinctive body tell that survives a rename).
//
// Plain substring / regex checks are deliberate: the retired copies lived in
// OTHER files in the product/capgraph packages (product.go, review_queue.go,
// evaluation_review_queue.go) and are legitimate there, so a go/parser walk of
// the whole package would false-positive. Reading only the three named bridge
// files, and keying the forbidden check on `func <name>(` DEFINITIONS (never
// call sites), is what keeps this precise.
const oneImplementationLaw = "one implementation of the extracted verdict helpers lives in internal/verdictflow — " +
	"delegate to it, or consciously update internal/verdictflow/guard_test.go in the SAME diff"

type verdictBridge struct {
	// path is relative to this package directory (internal/verdictflow).
	path string
	name string
	// required substrings PROVE the bridge still delegates to verdictflow (or,
	// for reviewartifact.UniqueSorted, to the shared reviewartifact seam).
	required []string
}

var verdictBridges = []verdictBridge{
	{
		path: "../product/observation_verdict.go",
		name: "internal/product/observation_verdict.go",
		required: []string{
			"verdictflow.RecordPlan(",
			"verdictflow.LoadReview[ObservationVerdictReview](",
			"verdictflow.LatestReviewedFromExtras(",
		},
	},
	{
		path: "../sourceobs/observation_verdict.go",
		name: "internal/sourceobs/observation_verdict.go",
		required: []string{
			"verdictflow.RecordPlan(",
			"verdictflow.AnnotateQueue(",
			"verdictflow.LatestReviewedFromExtras(",
			"verdictflow.Compact(",
			"verdictflow.OneOf(",
			"verdictflow.ShellJoin(",
			"reviewartifact.UniqueSorted(",
		},
	},
	{
		path: "../capgraph/outbound_distribution_verdict.go",
		name: "internal/capgraph/outbound_distribution_verdict.go",
		required: []string{
			"verdictflow.RecordPlan(",
			"verdictflow.AnnotateQueue(",
			"verdictflow.LatestReviewedFromExtras(",
		},
	},
}

// forbiddenHelperDef matches a LOCAL definition of any retired helper: a
// package-level `func <name>(` whose name CONTAINS one of the extracted tokens
// (case-insensitive). The retired privates were named e.g.
// sourceObservationVerdictCompact / ...JoinShell / ...ShellArg, so keying on the
// contained token catches a copy-back under any wrapper name. It keys on `func`
// definitions only, so a delegation CALL (verdictflow.Compact(, verdictflow.ShellJoin(,
// or capgraph's own shellQuote( call site) never trips it.
var forbiddenHelperDef = regexp.MustCompile(`(?i)func\s+[A-Za-z0-9_]*(compact|oneof|shelljoin|joinshell|shellarg|shellquote)[A-Za-z0-9_]*\s*\(`)

// forbiddenBodyTells are two verbatim lines from the retired helper bodies. They
// survive renaming the copied function, catching a copy-back even when the
// author invents a fresh name.
var forbiddenBodyTells = []struct {
	tell   string
	helper string
}{
	{`!strings.HasPrefix(value, "TODO")`, "Compact"},
	{`strings.ReplaceAll(arg, "'", "'\"'\"'")`, "shellArg"},
}

func readBridge(t *testing.T, b verdictBridge) string {
	t.Helper()
	src, err := os.ReadFile(b.path)
	if err != nil {
		t.Fatalf("read bridge source %s (%s): %v", b.name, b.path, err)
	}
	return string(src)
}

// TestVerdictBridgesStillDelegate reddens if any bridge drops a delegation call
// it is required to make into internal/verdictflow.
func TestVerdictBridgesStillDelegate(t *testing.T) {
	for _, b := range verdictBridges {
		t.Run(b.name, func(t *testing.T) {
			text := readBridge(t, b)
			for _, want := range b.required {
				if !strings.Contains(text, want) {
					t.Errorf("%s no longer contains the required delegation call %q.\n%s",
						b.name, want, oneImplementationLaw)
				}
			}
		})
	}
}

// TestVerdictBridgesDoNotReimplementHelpers reddens if any bridge reintroduces a
// local copy of an extracted helper — by function definition OR body tell.
func TestVerdictBridgesDoNotReimplementHelpers(t *testing.T) {
	for _, b := range verdictBridges {
		t.Run(b.name, func(t *testing.T) {
			text := readBridge(t, b)
			if m := forbiddenHelperDef.FindString(text); m != "" {
				t.Errorf("%s defines a local copy of an extracted helper: %q.\n%s",
					b.name, strings.TrimSpace(m), oneImplementationLaw)
			}
			for _, bt := range forbiddenBodyTells {
				if strings.Contains(text, bt.tell) {
					t.Errorf("%s contains the retired %s body tell %q — the extracted logic was copied back.\n%s",
						b.name, bt.helper, bt.tell, oneImplementationLaw)
				}
			}
		})
	}
}
