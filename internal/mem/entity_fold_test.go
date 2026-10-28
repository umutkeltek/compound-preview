package mem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appendRawEntityRow(t *testing.T, s *Store, raw string) {
	t.Helper()
	f, err := os.OpenFile(s.entitiesLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(raw + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestFoldEntitiesKeepsLastRowPerSlugInFirstAppearanceOrder(t *testing.T) {
	s := newStore(t)
	for _, e := range []Entity{
		{Slug: "repo:a", Kind: "repo", Identifiers: []string{"one"}},
		{Slug: "repo:b", Kind: "repo", Identifiers: []string{"two"}},
		{Slug: "repo:a", Kind: "repo", Identifiers: []string{"one", "three"}},
		{Slug: "repo:a", Kind: "repo", Identifiers: []string{"one", "three", "four"}},
	} {
		if _, err := s.RegisterEntity(e, t0); err != nil {
			t.Fatal(err)
		}
	}

	before, err := s.Entities()
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.FoldEntities(t0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Applied || !report.Verified {
		t.Fatalf("expected an applied+verified fold, got %+v", report)
	}
	if report.RowsAfter != 2 {
		t.Fatalf("want 2 rows after fold (one per slug), got %d", report.RowsAfter)
	}

	after, err := s.Entities()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("entity count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Slug != after[i].Slug {
			t.Fatalf("order changed at %d: %q -> %q", i, before[i].Slug, after[i].Slug)
		}
		if strings.Join(before[i].Identifiers, ",") != strings.Join(after[i].Identifiers, ",") {
			t.Fatalf("identifiers changed for %s: %v -> %v", before[i].Slug, before[i].Identifiers, after[i].Identifiers)
		}
	}
	if got := after[0].Identifiers; len(got) != 3 {
		t.Fatalf("fold must keep the LAST row's identifier set, got %v", got)
	}
}

// A fold that re-marshaled through the Entity struct would silently drop any
// field the struct does not model. That is real data loss, so the fold must move
// raw bytes; this test fails if anyone changes it to re-encode.
func TestFoldEntitiesPreservesFieldsTheStructDoesNotModel(t *testing.T) {
	s := newStore(t)
	appendRawEntityRow(t, s, `{"slug":"repo:a","kind":"repo","created_at":"2026-06-28T12:00:00Z"}`)
	appendRawEntityRow(t, s, `{"slug":"repo:a","kind":"repo","created_at":"2026-06-28T12:00:00Z","future_field":{"nested":[1,2,3]}}`)

	if _, err := s.FoldEntities(t0, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.entitiesLog())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"future_field"`) {
		t.Fatalf("fold dropped an unmodeled field; registry is now:\n%s", raw)
	}
}

func TestFoldEntitiesSkipsRowsTheReadersAlreadyIgnore(t *testing.T) {
	s := newStore(t)
	appendRawEntityRow(t, s, `{"slug":"repo:a","kind":"repo"}`)
	appendRawEntityRow(t, s, `{"slug":"","kind":"repo"}`)
	appendRawEntityRow(t, s, `{"slug":"repo:a","kind":`) // torn write

	before, err := s.Entities()
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.FoldEntities(t0, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.SkippedRows != 2 {
		t.Fatalf("want 2 skipped rows, got %d (%+v)", report.SkippedRows, report)
	}
	after, err := s.Entities()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) || before[0].Slug != after[0].Slug {
		t.Fatalf("skipping unreadable rows changed the registry: %+v -> %+v", before, after)
	}
}

func TestFoldEntitiesDryRunLeavesRegistryUntouched(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.RegisterEntity(Entity{Slug: "repo:a", Kind: "repo", Identifiers: []string{"x"}}, t0); err != nil {
			t.Fatal(err)
		}
	}
	original, err := os.ReadFile(s.entitiesLog())
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.FoldEntities(t0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.Applied {
		t.Fatalf("dry run must verify without applying, got %+v", report)
	}
	now, err := os.ReadFile(s.entitiesLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != string(now) {
		t.Fatal("dry run modified the registry")
	}
	if entries, _ := filepath.Glob(s.entitiesLog() + ".fold-*"); len(entries) != 0 {
		t.Fatalf("dry run left temp files behind: %v", entries)
	}
}

func TestFoldEntitiesArchivesPreFoldBytes(t *testing.T) {
	s := newStore(t)
	// Each registration must genuinely change the payload; identical ones no
	// longer append at all, which is what the no-op guard is for.
	ids := []string{}
	for i := 0; i < 4; i++ {
		ids = append(ids, string(rune('a'+i)))
		if _, err := s.RegisterEntity(Entity{Slug: "repo:a", Kind: "repo", Identifiers: ids}, t0); err != nil {
			t.Fatal(err)
		}
	}
	originalBytes, err := os.ReadFile(s.entitiesLog())
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.FoldEntities(t0, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArchivePath == "" || report.ArchiveSHA256 == "" {
		t.Fatalf("fold must archive the pre-fold bytes, got %+v", report)
	}
	archived, err := os.ReadFile(report.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(archived) != string(originalBytes) {
		t.Fatal("archive is not a byte-for-byte copy of the pre-fold registry")
	}
	if report.BytesAfter >= report.BytesBefore {
		t.Fatalf("fold did not reduce the registry: %d -> %d", report.BytesBefore, report.BytesAfter)
	}
}

// The topic reader deletes a slug when its latest row is not a topic. Folding
// must carry that transition, not resurrect the earlier topic row.
func TestFoldEntitiesPreservesTopicTransitions(t *testing.T) {
	s := newStore(t)
	if _, err := s.RegisterEntity(Entity{Slug: "topic:x", Kind: "topic"}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterEntity(Entity{Slug: "topic:x", Kind: "repo"}, t0); err != nil {
		t.Fatal(err)
	}
	before, err := s.TopicEntities()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("precondition: topic reader should drop the demoted slug, got %+v", before)
	}
	if _, err := s.FoldEntities(t0, true); err != nil {
		t.Fatal(err)
	}
	after, err := s.TopicEntities()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("fold resurrected a demoted topic: %+v", after)
	}
}

// A verification gate that cannot fail is not a gate. These prove it rejects
// encodings that do not agree, so the passing cases above mean something.
func TestVerifyFoldEquivalentRejectsChangedMeaning(t *testing.T) {
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.jsonl")
	folded := filepath.Join(dir, "folded.jsonl")

	cases := []struct{ name, originRows, foldedRows string }{
		{"dropped slug",
			"{\"slug\":\"repo:a\",\"kind\":\"repo\"}\n{\"slug\":\"repo:b\",\"kind\":\"repo\"}\n",
			"{\"slug\":\"repo:a\",\"kind\":\"repo\"}\n"},
		{"kept the wrong row",
			"{\"slug\":\"repo:a\",\"kind\":\"repo\",\"identifiers\":[\"old\"]}\n{\"slug\":\"repo:a\",\"kind\":\"repo\",\"identifiers\":[\"new\"]}\n",
			"{\"slug\":\"repo:a\",\"kind\":\"repo\",\"identifiers\":[\"old\"]}\n"},
		{"reordered slugs",
			"{\"slug\":\"repo:a\",\"kind\":\"repo\"}\n{\"slug\":\"repo:b\",\"kind\":\"repo\"}\n",
			"{\"slug\":\"repo:b\",\"kind\":\"repo\"}\n{\"slug\":\"repo:a\",\"kind\":\"repo\"}\n"},
		{"dropped an unmodeled field",
			"{\"slug\":\"topic:a\",\"kind\":\"topic\",\"summary\":\"kept\"}\n",
			"{\"slug\":\"topic:a\",\"kind\":\"topic\"}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(origin, []byte(tc.originRows), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(folded, []byte(tc.foldedRows), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := verifyFoldEquivalent(origin, folded); err == nil {
				t.Fatal("gate accepted an encoding that changed meaning")
			}
		})
	}
}

func countRegistryRows(t *testing.T, s *Store) int {
	t.Helper()
	b, err := os.ReadFile(s.entitiesLog())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func TestRegisterEntitySkipsUnchangedAppends(t *testing.T) {
	s := newStore(t)
	e := Entity{Slug: "repo:a", Kind: "repo", Identifiers: []string{"one", "two"}}
	for i := 0; i < 25; i++ {
		if _, err := s.RegisterEntity(e, t0); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRegistryRows(t, s); got != 1 {
		t.Fatalf("25 identical registrations wrote %d rows, want 1", got)
	}

	// A real change must still append.
	e.Identifiers = append(e.Identifiers, "three")
	if _, err := s.RegisterEntity(e, t0); err != nil {
		t.Fatal(err)
	}
	if got := countRegistryRows(t, s); got != 2 {
		t.Fatalf("a changed payload wrote %d rows, want 2", got)
	}
	after, err := s.Entities()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || len(after[0].Identifiers) != 3 {
		t.Fatalf("registry lost the change: %+v", after)
	}
}

func TestRegisterEntityReturnsCurrentRowWhenSkipping(t *testing.T) {
	s := newStore(t)
	e := Entity{Slug: "repo:a", Kind: "repo", Identifiers: []string{"one"}}
	first, err := s.RegisterEntity(e, t0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RegisterEntity(e, t0.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.Slug != first.Slug || strings.Join(second.Identifiers, ",") != strings.Join(first.Identifiers, ",") {
		t.Fatalf("skipped registration returned a different entity: %+v vs %+v", second, first)
	}
	if second.CreatedAt != first.CreatedAt {
		t.Fatalf("skipped registration must report the row on disk, got created_at %q want %q", second.CreatedAt, first.CreatedAt)
	}
}

// The invariant the fold restores: registry size tracks the number of state
// CHANGES, not the number of writes. A registry that grows per write is storing
// a set across a time axis, which costs sum(n) rather than n -- the shape that
// turned 237 entities into a 614MB log.
func TestRegistryRowsTrackChangesNotWrites(t *testing.T) {
	s := newStore(t)
	const entities, writesEach, changesEach = 5, 40, 3

	for i := 0; i < entities; i++ {
		slug := "repo:" + string(rune('a'+i))
		ids := []string{"base"}
		for w := 0; w < writesEach; w++ {
			// Only `changesEach` of the writes actually change anything.
			if w > 0 && w <= changesEach {
				ids = append(ids, string(rune('0'+w)))
			}
			if _, err := s.RegisterEntity(Entity{Slug: slug, Kind: "repo", Identifiers: ids}, t0); err != nil {
				t.Fatal(err)
			}
		}
	}

	want := entities * (1 + changesEach)
	if got := countRegistryRows(t, s); got != want {
		t.Fatalf("%d writes over %d entities wrote %d rows, want %d (one per entity plus one per real change)",
			entities*writesEach, entities, got, want)
	}
	report, err := s.FoldEntities(t0, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.RowsAfter != entities {
		t.Fatalf("folded registry holds %d rows, want one per entity (%d)", report.RowsAfter, entities)
	}
}

func TestFoldEntitiesIsIdempotent(t *testing.T) {
	s := newStore(t)
	ids := []string{}
	for i := 0; i < 6; i++ {
		ids = append(ids, string(rune('a'+i)))
		if _, err := s.RegisterEntity(Entity{Slug: "repo:a", Kind: "repo", Identifiers: ids}, t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.FoldEntities(t0, true); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(s.entitiesLog())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.FoldEntities(t0.Add(time.Hour), true)
	if err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(s.entitiesLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(again) {
		t.Fatal("folding an already-folded registry changed it")
	}
	if second.RowsBefore != second.RowsAfter {
		t.Fatalf("second fold claims to have reduced rows %d -> %d", second.RowsBefore, second.RowsAfter)
	}
}
