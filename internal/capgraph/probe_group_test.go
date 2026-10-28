package capgraph

import "testing"

// TestParseProbeGroupLabelWellFormed pins the exact shape internal/contract's
// probe-group selector stamps (probe_contracts.go: `"probe:" + class + "/" + group +
// "; "` prepended to the ref's original reason).
func TestParseProbeGroupLabelWellFormed(t *testing.T) {
	cases := []struct {
		name      string
		reason    string
		wantLabel string
	}{
		{
			name:      "memory claim reason",
			reason:    "probe:system-work/standing-law; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional-widget.state = shipped",
			wantLabel: "system-work/standing-law",
		},
		{
			name:      "position reason",
			reason:    "probe:product-work/trap; Compound memory folded position; source stores and indexes are evidence, not authority: pos_fictional [predicate=state, claim_type=status] -> shipped",
			wantLabel: "product-work/trap",
		},
		{
			name:      "general class",
			reason:    "probe:general/opportunity; Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional.thing = value",
			wantLabel: "general/opportunity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, ok := parseProbeGroupLabel(tc.reason)
			if !ok {
				t.Fatalf("parseProbeGroupLabel(%q) ok=false, want true", tc.reason)
			}
			if label != tc.wantLabel {
				t.Fatalf("parseProbeGroupLabel(%q) = %q, want %q", tc.reason, label, tc.wantLabel)
			}
		})
	}
}

// TestParseProbeGroupLabelMalformedIsUnlabeled proves every malformed or absent shape
// fails safe to ok=false — counted as unlabeled, never a panic or an error return.
func TestParseProbeGroupLabelMalformedIsUnlabeled(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{"empty reason", ""},
		{"no probe prefix at all", "recency"},
		{"legacy ledger-claim reason", "Compound memory ledger claim; source stores and indexes are evidence, not authority: fictional-widget.state = shipped"},
		{"missing class/group separator", "probe:system-work; Compound memory ledger claim: x"},
		{"missing terminator (no semicolon)", "probe:system-work/standing-law"},
		{"empty class", "probe:/standing-law; Compound memory ledger claim: x"},
		{"empty group", "probe:system-work/; Compound memory ledger claim: x"},
		{"empty class and group", "probe:/; Compound memory ledger claim: x"},
		{"whitespace-only class", "probe: /standing-law; Compound memory ledger claim: x"},
		{"whitespace-only group", "probe:system-work/ ; Compound memory ledger claim: x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, ok := parseProbeGroupLabel(tc.reason)
			if ok {
				t.Fatalf("parseProbeGroupLabel(%q) ok=true label=%q, want ok=false", tc.reason, label)
			}
			if label != "" {
				t.Fatalf("parseProbeGroupLabel(%q) label=%q, want empty on ok=false", tc.reason, label)
			}
		})
	}
}
