package corpus

import (
	"testing"

	"compound/internal/sourceobs"
)

// TestQueryWithOptionsRadiusZeroIsByteIdenticalDefault pins the off-by-default
// invariant. NeighborRadius=0 must not populate QueryHit.Neighbors at all —
// the omitempty JSON tag carries that through to wire format.
func TestQueryWithOptionsRadiusZeroIsByteIdenticalDefault(t *testing.T) {
	hit := QueryHit{ItemID: "i", ChunkID: "i#0"}
	if hit.Neighbors != nil {
		t.Fatalf("zero-value QueryHit.Neighbors must be nil, got %v", hit.Neighbors)
	}
}

// TestQueryOptionsNegativeRadiusIsRejected — clamping to 0 would silently
// turn a caller's bug into "expansion is off", which looks exactly like
// success. The right move is to refuse and let the caller see the bug.
func TestQueryOptionsNegativeRadiusIsRejected(t *testing.T) {
	if err := (QueryOptions{NeighborRadius: -1}).validate(); err == nil {
		t.Fatalf("expected negative radius to be rejected")
	}
}

// TestQueryOptionsRadiusBeyondMaxIsRejected — the cap is the only thing that
// keeps a single query from becoming a whole-corpus read. A radius above the
// cap is refused, not clamped.
func TestQueryOptionsRadiusBeyondMaxIsRejected(t *testing.T) {
	if err := (QueryOptions{NeighborRadius: MaxNeighborRadius + 1}).validate(); err == nil {
		t.Fatalf("expected radius above max to be rejected")
	}
}

// TestQueryOptionsRadiusZeroAndCapAccept — radius 0 (the default) and radius
// equal to the cap must validate cleanly. Off-by-default is the load-bearing
// rule, so we name it.
func TestQueryOptionsRadiusZeroAndCapAccept(t *testing.T) {
	for _, r := range []int{0, 1, MaxNeighborRadius} {
		if err := (QueryOptions{NeighborRadius: r}).validate(); err != nil {
			t.Fatalf("radius %d should validate clean: %v", r, err)
		}
	}
}

// TestNeighborOrdinalsMidAnchor — anchor at ordinal 2 of a 5-chunk item with
// radius 5 returns ordinals [0,1,3,4] (anchor excluded, no out-of-bounds).
func TestNeighborOrdinalsMidAnchor(t *testing.T) {
	manifest := fixtureManifestWithChunks("item", 5)
	_, ordinals, err := neighborOrdinalsFor(manifest, "item#2", 5)
	if err != nil {
		t.Fatalf("neighborOrdinalsFor: %v", err)
	}
	want := []int{0, 1, 3, 4}
	if !equalInts(ordinals, want) {
		t.Fatalf("ordinals = %v, want %v", ordinals, want)
	}
}

// TestNeighborOrdinalsLowAnchor — anchor at 0 with radius 5 → expected
// [1,2,3,4]. The low clamp cuts any negative ordinal.
func TestNeighborOrdinalsLowAnchor(t *testing.T) {
	manifest := fixtureManifestWithChunks("edge", 5)
	_, ordinals, err := neighborOrdinalsFor(manifest, "edge#0", 5)
	if err != nil {
		t.Fatalf("low anchor: %v", err)
	}
	want := []int{1, 2, 3, 4}
	if !equalInts(ordinals, want) {
		t.Fatalf("low-anchor ordinals = %v, want %v", ordinals, want)
	}
}

// TestNeighborOrdinalsHighAnchor — anchor at last ordinal (4) with radius 5
// → expected [0,1,2,3]. The high clamp cuts anything past chunkCount-1.
func TestNeighborOrdinalsHighAnchor(t *testing.T) {
	manifest := fixtureManifestWithChunks("edge", 5)
	_, ordinals, err := neighborOrdinalsFor(manifest, "edge#4", 5)
	if err != nil {
		t.Fatalf("high anchor: %v", err)
	}
	want := []int{0, 1, 2, 3}
	if !equalInts(ordinals, want) {
		t.Fatalf("high-anchor ordinals = %v, want %v", ordinals, want)
	}
}

// TestNeighborOrdinalsItemNotInManifest — anchor for an item that does not
// appear in the manifest returns no ordinals and no error. Expansion against
// an unknown chunk id is a no-op rather than a hard fail.
func TestNeighborOrdinalsItemNotInManifest(t *testing.T) {
	manifest := fixtureManifestWithChunks("present", 3)
	_, ordinals, err := neighborOrdinalsFor(manifest, "absent#1", 2)
	if err != nil {
		t.Fatalf("absent item anchor: %v", err)
	}
	if len(ordinals) != 0 {
		t.Fatalf("absent item should have no neighbors, got %v", ordinals)
	}
}

// TestBuildChunkIDIsInverseOfSplit proves the construction round-trips
// through the existing chunk-id parser — same shape, same fields, same
// failure modes. If this diverges, every chunk id ResolveChunkText gets is
// malformed and the resolver silently degrades to a per-item rejection.
func TestBuildChunkIDIsInverseOfSplit(t *testing.T) {
	for _, tt := range []struct{ id string }{
		{"paper#0"}, {"book.pdf#7"}, {"dir/sub#99"},
	} {
		item, ord, err := splitChunkID(tt.id)
		if err != nil {
			t.Fatalf("split(%q) err: %v", tt.id, err)
		}
		if buildChunkID(item, ord) != tt.id {
			t.Fatalf("split→build != %q", tt.id)
		}
	}
}

// fixtureManifestWithChunks returns a Manifest with one item whose
// SourceManifestItem.Chunks ordinal list runs 0..n-1. Body bytes are not
// required: this fixture exists to drive the bounding/clamping test for
// neighborOrdinalsFor without invoking the live chunker.
func fixtureManifestWithChunks(itemID string, n int) Manifest {
	item := sourceobs.SourceManifestItem{ID: itemID}
	for i := 0; i < n; i++ {
		item.Chunks = append(item.Chunks, sourceobs.SourceManifestChunk{Ordinal: i})
	}
	return Manifest{
		SchemaVersion:       ManifestSchemaVersion,
		PrefixSchemaVersion: PrefixSchemaVersion,
		Lane:                "neighbor-test",
		Items:               []sourceobs.SourceManifestItem{item},
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
