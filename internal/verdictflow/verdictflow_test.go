package verdictflow

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"compound/internal/reviewartifact"
)

// These tests PIN the current byte-compatible behavior of the extracted verdict
// helpers (R1, landed 980d26fd). They are documentation of the contract the
// three domain bridges depend on, NOT an invitation to "improve" a helper: a
// change here that is not a conscious contract change is a regression. Every
// case below is written so that a silent behavior drift reddens.

func TestCompact(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil input returns non-nil empty", nil, []string{}},
		{"empty input returns non-nil empty", []string{}, []string{}},
		{"all-dropped returns non-nil empty", []string{"", "   ", "TODO", "TODO_x"}, []string{}},
		{"trims each value and drops empties", []string{" a ", "", "\tb\n"}, []string{"a", "b"}},
		{"drops TODO PREFIX (not word): TODOx drops, aTODO/TOD kept", []string{"TODO", "TODOx", "aTODO", "TOD"}, []string{"aTODO", "TOD"}},
		{"preserves order, does not sort", []string{"z", "a", "m"}, []string{"z", "a", "m"}},
		{"does not dedup", []string{"a", "a", "b"}, []string{"a", "a", "b"}},
		{"whitespace-only values dropped", []string{"\t", "\n", " "}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compact(tt.in)
			// Compact never returns nil (make with cap len) — pin that explicitly,
			// because callers pass the result straight into UniqueSorted.
			if got == nil {
				t.Fatalf("Compact(%q) returned nil, want non-nil slice", tt.in)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Compact(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestOneOf(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		allowed []string
		want    bool
	}{
		{"trims the value before matching", "  park  ", []string{"park", "reject"}, true},
		{"does NOT trim the allowed list", "park", []string{" park "}, false},
		{"exact match is case-sensitive", "PARK", []string{"park"}, false},
		{"nil allowed is false", "x", nil, false},
		{"empty allowed slice is false", "x", []string{}, false},
		{"trimmed-empty value matches empty allowed entry", "   ", []string{""}, true},
		{"matches one of several", "reject", []string{"a", "reject", "b"}, true},
		{"no match is false", "z", []string{"a", "b"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OneOf(tt.value, tt.allowed); got != tt.want {
				t.Errorf("OneOf(%q, %q) = %v, want %v", tt.value, tt.allowed, got, tt.want)
			}
		})
	}
}

func TestShellJoin(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"nil is empty string", nil, ""},
		{"empty slice is empty string", []string{}, ""},
		{"plain arg passes through bare", []string{"compound"}, "compound"},
		{"empty arg becomes ''", []string{""}, "''"},
		{"space triggers single-quoting", []string{"a b"}, "'a b'"},
		{"tab triggers single-quoting", []string{"a\tb"}, "'a\tb'"},
		{"newline triggers single-quoting", []string{"a\nb"}, "'a\nb'"},
		{"double-quote triggers single-quoting (no inner escape)", []string{"a\"b"}, "'a\"b'"},
		{"single-quote is escaped as '\"'\"'", []string{"a'b"}, "'a'\"'\"'b'"},
		{"backslash triggers single-quoting", []string{"a\\b"}, "'a\\b'"},
		{"dollar triggers single-quoting", []string{"x$y"}, "'x$y'"},
		{"backtick triggers single-quoting", []string{"a`b"}, "'a`b'"},
		{"multiple args join with a single space", []string{"compound", "product", "a b"}, "compound product 'a b'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShellJoin(tt.in); got != tt.want {
				t.Errorf("ShellJoin(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestShellArgQuotingTriggers pins, directly against the private shellArg
// (white-box), that EVERY character in the quoting trigger set forces
// single-quoting. This is the exact set F3's scratch mutation perturbs.
func TestShellArgQuotingTriggers(t *testing.T) {
	triggers := []struct {
		name string
		arg  string
		want string
	}{
		{"space", "a b", "'a b'"},
		{"tab", "a\tb", "'a\tb'"},
		{"newline", "a\nb", "'a\nb'"},
		{"double-quote", "a\"b", "'a\"b'"},
		{"single-quote", "a'b", "'a'\"'\"'b'"},
		{"backslash", "a\\b", "'a\\b'"},
		{"dollar", "a$b", "'a$b'"},
		{"backtick", "a`b", "'a`b'"},
	}
	for _, tt := range triggers {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellArg(tt.arg); got != tt.want {
				t.Errorf("shellArg(%q) = %q, want %q (trigger char must force quoting)", tt.arg, got, tt.want)
			}
		})
	}
	if got := shellArg(""); got != "''" {
		t.Errorf("shellArg(\"\") = %q, want %q", got, "''")
	}
	if got := shellArg("plain"); got != "plain" {
		t.Errorf("shellArg(%q) = %q, want it passed through bare", "plain", got)
	}
}

// reviewedRec is a minimal generic payload for exercising
// LatestReviewedFromExtras with concrete valid/id/createdAt accessors.
type reviewedRec struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	OK        bool   `json:"ok"`
}

func recValid(r reviewedRec) bool     { return r.OK }
func recID(r reviewedRec) string      { return r.ID }
func recCreated(r reviewedRec) string { return r.CreatedAt }

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func latest(paths []string) (reviewedRec, bool) {
	return LatestReviewedFromExtras(paths, recValid, recID, recCreated)
}

func TestLatestReviewedFromExtras(t *testing.T) {
	t.Run("newest createdAt wins regardless of path order", func(t *testing.T) {
		dir := t.TempDir()
		a := writeFile(t, dir, "a.reviewed.json", `{"id":"a","created_at":"2026-01-01","ok":true}`)
		b := writeFile(t, dir, "b.reviewed.json", `{"id":"b","created_at":"2026-02-01","ok":true}`)
		for _, order := range [][]string{{a, b}, {b, a}} {
			got, ok := latest(order)
			if !ok || got.ID != "b" {
				t.Errorf("order %v: got (%+v, %v), want id=b ok=true", order, got, ok)
			}
		}
	})

	t.Run("ties keep the first-seen record (strict greater-than)", func(t *testing.T) {
		dir := t.TempDir()
		x := writeFile(t, dir, "x.reviewed.json", `{"id":"x","created_at":"2026-05-05","ok":true}`)
		y := writeFile(t, dir, "y.reviewed.json", `{"id":"y","created_at":"2026-05-05","ok":true}`)
		got, ok := latest([]string{x, y})
		if !ok || got.ID != "x" {
			t.Errorf("got (%+v, %v), want id=x ok=true (first-seen on tie)", got, ok)
		}
	})

	t.Run("only .reviewed.json suffixed paths are considered", func(t *testing.T) {
		dir := t.TempDir()
		// A later record with the wrong suffix must be ignored.
		wrong := writeFile(t, dir, "z.json", `{"id":"z","created_at":"2999-01-01","ok":true}`)
		bak := writeFile(t, dir, "w.reviewed.json.bak", `{"id":"w","created_at":"2999-01-01","ok":true}`)
		good := writeFile(t, dir, "g.reviewed.json", `{"id":"g","created_at":"2000-01-01","ok":true}`)
		got, ok := latest([]string{wrong, bak, good})
		if !ok || got.ID != "g" {
			t.Errorf("got (%+v, %v), want id=g ok=true (non-suffixed skipped)", got, ok)
		}
	})

	t.Run("unreadable/missing files are skipped", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "missing.reviewed.json")
		good := writeFile(t, dir, "g.reviewed.json", `{"id":"g","created_at":"2026-01-01","ok":true}`)
		got, ok := latest([]string{missing, good})
		if !ok || got.ID != "g" {
			t.Errorf("got (%+v, %v), want id=g ok=true (missing skipped)", got, ok)
		}
	})

	t.Run("json-invalid files are skipped", func(t *testing.T) {
		dir := t.TempDir()
		bad := writeFile(t, dir, "bad.reviewed.json", `{not valid json`)
		good := writeFile(t, dir, "g.reviewed.json", `{"id":"g","created_at":"2026-01-01","ok":true}`)
		got, ok := latest([]string{bad, good})
		if !ok || got.ID != "g" {
			t.Errorf("got (%+v, %v), want id=g ok=true (invalid JSON skipped)", got, ok)
		}
	})

	t.Run("valid()-failing records are skipped even when newer", func(t *testing.T) {
		dir := t.TempDir()
		invalid := writeFile(t, dir, "iv.reviewed.json", `{"id":"iv","created_at":"2999-01-01","ok":false}`)
		good := writeFile(t, dir, "g.reviewed.json", `{"id":"g","created_at":"2000-01-01","ok":true}`)
		got, ok := latest([]string{invalid, good})
		if !ok || got.ID != "g" {
			t.Errorf("got (%+v, %v), want id=g ok=true (valid()==false skipped)", got, ok)
		}
	})

	t.Run("empty paths returns not-found", func(t *testing.T) {
		if _, ok := latest(nil); ok {
			t.Error("latest(nil) reported found=true, want false")
		}
		if _, ok := latest([]string{}); ok {
			t.Error("latest([]) reported found=true, want false")
		}
	})

	t.Run("an empty-id current winner is replaced by ANY later valid candidate", func(t *testing.T) {
		dir := t.TempDir()
		// rec1 is valid (ok) but has an empty id and the NEWER date; rec2 is
		// valid with an older date. Because the current winner's id is empty,
		// rec2 replaces it regardless of createdAt ordering.
		rec1 := writeFile(t, dir, "1.reviewed.json", `{"id":"","created_at":"2026-12-31","ok":true}`)
		rec2 := writeFile(t, dir, "2.reviewed.json", `{"id":"b","created_at":"2026-01-01","ok":true}`)
		got, ok := latest([]string{rec1, rec2})
		if !ok || got.ID != "b" {
			t.Errorf("got (%+v, %v), want id=b ok=true (empty-id winner replaced regardless of date)", got, ok)
		}
	})

	t.Run("a sole empty-id valid record reports not-found", func(t *testing.T) {
		dir := t.TempDir()
		only := writeFile(t, dir, "e.reviewed.json", `{"id":"","created_at":"2026-05-05","ok":true}`)
		// NOTE (report-only, byte-compat with pre-extraction): the first return
		// value here is the empty-id record itself, not a zero value; the bool
		// is the authority every caller uses. We pin the bool.
		if _, ok := latest([]string{only}); ok {
			t.Error("sole empty-id record reported found=true, want false")
		}
	})
}

func TestAnnotateQueue(t *testing.T) {
	t.Run("nil report is a no-op and never calls the annotator", func(t *testing.T) {
		called := false
		// Must not panic on a nil report.
		AnnotateQueue(nil, "area", func(*reviewartifact.QueueItem) { called = true })
		if called {
			t.Error("annotator was called for a nil report")
		}
	})

	t.Run("empty items never calls the annotator", func(t *testing.T) {
		called := false
		AnnotateQueue(&reviewartifact.QueueReport{}, "area", func(*reviewartifact.QueueItem) { called = true })
		if called {
			t.Error("annotator was called for a report with no items")
		}
	})

	t.Run("only matching-area items are annotated, via a pointer into report.Items", func(t *testing.T) {
		report := &reviewartifact.QueueReport{Items: []reviewartifact.QueueItem{
			{Area: "match", ID: "1"},
			{Area: "other", ID: "2"},
			{Area: "match", ID: "3"},
		}}
		var seen []string
		AnnotateQueue(report, "match", func(it *reviewartifact.QueueItem) {
			seen = append(seen, it.ID)
			it.Status = "touched"
		})
		if !reflect.DeepEqual(seen, []string{"1", "3"}) {
			t.Errorf("annotated ids = %q, want [1 3]", seen)
		}
		// Mutations must be visible in the caller's report: the annotator gets
		// &report.Items[i], not a copy.
		if report.Items[0].Status != "touched" {
			t.Errorf("Items[0].Status = %q, want touched (mutation not visible → not a pointer into the slice)", report.Items[0].Status)
		}
		if report.Items[2].Status != "touched" {
			t.Errorf("Items[2].Status = %q, want touched", report.Items[2].Status)
		}
		if report.Items[1].Status != "" {
			t.Errorf("Items[1].Status = %q, want \"\" (foreign area must stay untouched)", report.Items[1].Status)
		}
	})
}
