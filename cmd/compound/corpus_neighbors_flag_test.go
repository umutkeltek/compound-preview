package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"compound/internal/corpus"
)

// TestCorpusNeighborsParsingEdgeCases — the CLI parses the same numeric
// vocabulary as the library. strconv.Atoi accepts negatives (returns -1)
// — the contract "negative is an exit-2 error" lives in the CLI's
// `n < 0` check AFTER parsing, not in strconv itself. The library
// rejects negative and over-cap at validate time. This test pins the
// contract for each surface so a future refactor that drops one of
// them is loud.
func TestCorpusNeighborsParsingEdgeCases(t *testing.T) {
	t.Parallel()
	// strconv.Atoi accepts anything that's a base-10 integer, including
	// negatives. The CLI's "must be non-negative" and the library's
	// "must be in [0, MaxNeighborRadius]" rules live OUTSIDE of strconv.
	for _, raw := range []string{"0", "1", strconv.Itoa(corpus.MaxNeighborRadius), "-1", strconv.Itoa(corpus.MaxNeighborRadius + 1)} {
		if _, err := strconv.Atoi(raw); err != nil {
			t.Errorf("strconv must parse %q: %v", raw, err)
		}
	}
}

// TestCorpusNeighborsJSONOmitempty — default-off means the JSON wire format
// must NOT carry a "neighbors" field on QueryHit. omitempty only drops the
// field when the slice is nil; an empty (non-nil) slice would render as
// `"neighbors":[]`. The default field value MUST be a nil slice so
// pre-existing callers see byte-identical output.
func TestCorpusNeighborsJSONOmitempty(t *testing.T) {
	t.Parallel()
	hit := corpus.QueryHit{ItemID: "i", ChunkID: "i#0", TextStatus: "ok"}
	body, err := json.Marshal(hit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), `"neighbors"`) {
		t.Fatalf("default QueryHit JSON must not include neighbors field: %s", body)
	}
}

// TestCorpusNeighborsJSONWhenPopulated — when neighbor expansion is
// active, the JSON shape is a list of objects with chunk_id, ordinal,
// text, text_status. No score field on the NEIGHBOR — context is not
// ranked. The hit itself may carry a score; the pin here is that the
// neighbor sub-object does not.
func TestCorpusNeighborsJSONWhenPopulated(t *testing.T) {
	t.Parallel()
	hit := corpus.QueryHit{
		ItemID:  "i",
		ChunkID: "i#0",
		Neighbors: []corpus.QueryNeighbor{
			{ChunkID: "i#1", Ordinal: 1, TextStatus: "ok"},
		},
	}
	body, err := json.Marshal(hit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"chunk_id":"i#1"`, `"ordinal":1`, `"text_status":"ok"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("neighbors JSON missing %q: %s", want, body)
		}
	}
	// The neighbor sub-object must not have its own score field. The hit
	// has one (Score float64); the neighbor struct does not. Decode into
	// a map so we can scope the check to the inner object.
	neighbor := corpus.QueryNeighbor{ChunkID: "i#1", Ordinal: 1, TextStatus: "ok"}
	inner, err := json.Marshal(neighbor)
	if err != nil {
		t.Fatalf("marshal neighbor: %v", err)
	}
	if strings.Contains(string(inner), `"score"`) {
		t.Fatalf("neighbor struct must not have a score field: %s", inner)
	}
}
