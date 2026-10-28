// memory_cli_hints_test.go covers the operator-facing message constructors
// in memory_cli_hints.go. The end-to-end subprocess assertions live in
// cmd/compound/memory_cli_test.go; these tests exercise the pure helpers
// directly so a behavior regression in the helpers surfaces without the
// cost of a forked test binary.
package mem

import (
	"bytes"
	"strings"
	"testing"

	"compound/internal/candidates"
)

func TestDoorCommandTranslation(t *testing.T) {
	cases := []struct {
		name     string
		consumer string
		want     string
	}{
		{"known-symbol-translates-to-CLI", "capgraph.Consume", "compound capability consume"},
		{"unknown-symbol-passes-through", "compound something unknown", "compound something unknown"},
		{"empty-passes-through", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DoorCommand(tc.consumer); got != tc.want {
				t.Fatalf("DoorCommand(%q) = %q, want %q", tc.consumer, got, tc.want)
			}
		})
	}
}

func TestIdFromCandidatePrefixKnownAndUnknown(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want candidates.Kind
	}{
		{"src_cand-returns-capability_observation", "src_cand_abc123", candidates.CapabilityObs},
		{"other-prefix-returns-blank", "mem_cand_anything", ""},
		{"empty-id-returns-blank", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IdFromCandidatePrefix(tc.id); got != tc.want {
				t.Fatalf("IdFromCandidatePrefix(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestKindCrossNamespacePointerReturnsEmptyForMemoryAndUnknown(t *testing.T) {
	cases := []struct {
		name string
		kind string
		want string
	}{
		{"memory-kind-returns-empty", "memory", ""},
		{"unknown-kind-returns-empty", "definitely_not_a_kind", ""},
		{"blank-kind-returns-empty", "   ", ""},
		{"empty-kind-returns-empty", "", ""},
		{"capability_observation-returns-real-pointer", "capability_observation", "compound capability consume"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := KindCrossNamespacePointer(tc.kind)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("KindCrossNamespacePointer(%q) = %q, want empty", tc.kind, got)
				}
				return
			}
			// capability_observation lives in inbox/capability-observations,
			// judgement via capgraph.Consume which we translate to
			// `compound capability consume`. Asserting on a fragment instead
			// of the full string keeps the test stable if the candidates
			// table gains a verdict in the future.
			if !strings.Contains(got, tc.want) {
				t.Fatalf("KindCrossNamespacePointer(%q) = %q, want substring %q", tc.kind, got, tc.want)
			}
			if !strings.Contains(got, "--kind capability_observation is adjudicated via") {
				t.Fatalf("hint must include the operator-facing sentence frame; got %q", got)
			}
		})
	}
}

func TestMaybePrintCrossKindPointerRespectsEmptyAndUnknown(t *testing.T) {
	buf := &bytes.Buffer{}
	MaybePrintCrossKindPointer(buf, "definitely_not_a_kind")
	if buf.Len() != 0 {
		t.Fatalf("unknown kind must write nothing, got %q", buf.String())
	}
	buf.Reset()
	MaybePrintCrossKindPointer(buf, "")
	if buf.Len() != 0 {
		t.Fatalf("blank kind must write nothing, got %q", buf.String())
	}
	buf.Reset()
	MaybePrintCrossKindPointer(buf, "capability_observation")
	if !strings.Contains(buf.String(), "compound capability consume") {
		t.Fatalf("capability_observation must produce a door pointer; got %q", buf.String())
	}
}

func TestPrintAdjudicateUnknownIDProvidesSearchedLocationAndLookup(t *testing.T) {
	cases := []struct {
		name        string
		id          string
		storeErr    string
		mustContain []string
		mustAvoid   []string
	}{
		{
			name:     "plain-missing-id-no-door",
			id:       "cand_doesnotexist",
			storeErr: "not found: cand_doesnotexist.json",
			mustContain: []string{
				"error: not found: cand_doesnotexist.json",
				"searched: $COMPOUND_HOME/" + candidates.InboxDir(candidates.Memory) + "/cand_doesnotexist.json",
				"`compound memory candidates --state proposed`",
				// Generic note (no door pointer for an unknown prefix):
				"  note: ids minted by other producers",
			},
		},
		{
			name:     "src_cand-id-names-the-capability-door",
			id:       "src_cand_xyz",
			storeErr: "not found: src_cand_xyz.json",
			mustContain: []string{
				"error: not found: src_cand_xyz.json",
				"searched: $COMPOUND_HOME/" + candidates.InboxDir(candidates.Memory) + "/src_cand_xyz.json",
				"  note: # --kind capability_observation is adjudicated via compound capability consume",
			},
			mustAvoid: []string{
				// When the door pointer applies we should NOT print the
				// generic "ids minted by other producers" line — the door
				// pointer is sharper information for the operator.
				"  note: ids minted by other producers",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			PrintAdjudicateUnknownID(buf, tc.id, tc.storeErr)
			got := buf.String()
			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in output:\n%s", want, got)
				}
			}
			for _, forbid := range tc.mustAvoid {
				if strings.Contains(got, forbid) {
					t.Fatalf("forbidden substring %q appeared in output:\n%s", forbid, got)
				}
			}
		})
	}
}
