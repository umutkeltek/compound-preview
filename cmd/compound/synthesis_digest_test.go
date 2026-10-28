package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/mem"
)

// TestSynthesisDigestCLIRendersReadOnlyBlastTieredPage is the table-driven CLI coverage for
// `compound synthesis digest [--since <ts>] [--json]` (packet
// packet-reconcile-slice3-split-card-digest.md gap 1): it must consume the existing
// internal/mem digest rendering, thread --since through to the output, and support --json —
// wired the same way `sweep` is wired in cmd/compound/synthesis.go, but strictly read-only
// (unlike sweep it never opens a WorkEnvelope).
func TestSynthesisDigestCLIRendersReadOnlyBlastTieredPage(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, homePath string)
		args  []string
		check func(t *testing.T, out string)
	}{
		{
			name: "empty home text mode is zero-action",
			args: []string{"digest"},
			check: func(t *testing.T, out string) {
				if strings.TrimSpace(out) != "reconcile digest: zero actions" {
					t.Fatalf("text output = %q, want the zero-action page", out)
				}
			},
		},
		{
			name: "empty home json mode reports zero_action and schema",
			args: []string{"digest", "--json"},
			check: func(t *testing.T, out string) {
				var digest mem.SynthesisReconcileDigest
				if err := json.Unmarshal([]byte(out), &digest); err != nil {
					t.Fatalf("parse digest json: %v\n%s", err, out)
				}
				if !digest.ZeroAction {
					t.Fatalf("digest = %+v, want zero_action", digest)
				}
				if digest.SchemaVersion != mem.SynthesisReconcileDigestSchemaVersion {
					t.Fatalf("schema_version = %q, want %q", digest.SchemaVersion, mem.SynthesisReconcileDigestSchemaVersion)
				}
			},
		},
		{
			name: "--since threads through to json output",
			args: []string{"digest", "--since", "2026-07-01T00:00:00Z", "--json"},
			check: func(t *testing.T, out string) {
				var digest mem.SynthesisReconcileDigest
				if err := json.Unmarshal([]byte(out), &digest); err != nil {
					t.Fatalf("parse digest json: %v\n%s", err, out)
				}
				if digest.Since != "2026-07-01T00:00:00Z" {
					t.Fatalf("since = %q, want 2026-07-01T00:00:00Z", digest.Since)
				}
			},
		},
		{
			name: "reconciled usage receipt renders a tier-2 undo command",
			setup: func(t *testing.T, homePath string) {
				now := time.Now().UTC()
				claimAt := now.Add(-2 * time.Hour).Format(time.RFC3339)
				receiptAt := now.Add(-time.Hour).Format(time.RFC3339)
				appendClaimLine(t, filepath.Join(homePath, "ledger", "claims.jsonl"), mem.Claim{
					ID:             "claim_digest_cli_used",
					Seq:            1,
					Entity:         "project:digest-cli",
					Predicate:      "runtime",
					Object:         "Go",
					ValidFrom:      claimAt,
					Confidence:     0.9,
					Derivation:     "extracted",
					PromotionBasis: "explicit_confirmation",
					EvidenceRefs:   []string{"trace_digest_cli_used"},
					Visibility:     "ambient",
					CreatedAt:      claimAt,
				})
				writeSynthesisReconcileCLIReceipt(t, homePath, "rec_digest_cli_usage", receiptAt, []string{"claim://claim_digest_cli_used"})
				captureStdout(t, func() {
					cmdSynthesis([]string{"sweep", "--stage", "reconcile", "--json"})
				})
			},
			args: []string{"digest"},
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, "decay-routed") {
					t.Fatalf("expected a decay-routed tier-2 line, got %q", out)
				}
				if !strings.Contains(out, "undo: compound synthesis undo-checkpoint --kind decay_routed") {
					t.Fatalf("expected the decay-routed undo command, got %q", out)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			t.Setenv("COMPOUND_HOME", homePath)
			if err := home.Init(homePath, "test", cliTestNow); err != nil {
				t.Fatalf("init home: %v", err)
			}
			if tt.setup != nil {
				tt.setup(t, homePath)
			}
			out := captureStdout(t, func() {
				cmdSynthesis(tt.args)
			})
			tt.check(t, out)
		})
	}
}
