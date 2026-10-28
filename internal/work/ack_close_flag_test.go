package work

import (
	"strings"
	"testing"

	"compound/internal/runattempt"
)

// Falsifier: --acknowledge-abandoned RATT --reason R pairs are extracted
// linearly from args; missing id, missing reason, or --reason starting with
// "--" refused at the door. The resulting RunAttemptTerminalizationRequest
// carries the closed-vocabulary owner + target_status that the close-time
// gate consumes.
func TestParseAcknowledgeAbandonedFlagPairsHonorsDoorContract(t *testing.T) {
	t.Run("happy path single pair", func(t *testing.T) {
		got, err := ParseAcknowledgeAbandonedFlagPairs(
			[]string{"--acknowledge-abandoned", "ratt_x", "--reason", "stale sweep"},
			"",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 pair, got %d", len(got))
		}
		r := got[0]
		if r.RunAttemptID != "ratt_x" || r.Reason != "stale sweep" {
			t.Fatalf("r = %+v", r)
		}
		if r.Owner != runattempt.TerminalizationOwnerWorkAcknowledgeAbandoned {
			t.Fatalf("owner = %q, want %q", r.Owner, runattempt.TerminalizationOwnerWorkAcknowledgeAbandoned)
		}
		if r.TargetStatus != runattempt.StatusAbandoned {
			t.Fatalf("target_status = %q, want %q", r.TargetStatus, runattempt.StatusAbandoned)
		}
	})
	t.Run("happy path multiple pairs", func(t *testing.T) {
		got, err := ParseAcknowledgeAbandonedFlagPairs([]string{
			"--acknowledge-abandoned", "ratt_a", "--reason", "first",
			"--proof", "check:x",
			"--acknowledge-abandoned", "ratt_b", "--reason", "second",
		}, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 pairs, got %d", len(got))
		}
		if got[0].RunAttemptID != "ratt_a" || got[1].RunAttemptID != "ratt_b" {
			t.Fatalf("got = %+v", got)
		}
	})
	t.Run("nil args yields nil", func(t *testing.T) {
		got, err := ParseAcknowledgeAbandonedFlagPairs(nil, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
	t.Run("no --acknowledge-abandoned flags yields nil", func(t *testing.T) {
		got, err := ParseAcknowledgeAbandonedFlagPairs([]string{"--outcome", "completed", "--reason", "steal"}, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
	t.Run("missing following ratt_ id refused", func(t *testing.T) {
		_, err := ParseAcknowledgeAbandonedFlagPairs([]string{"--acknowledge-abandoned"}, "")
		if err == nil || !strings.Contains(err.Error(), "requires a following ratt_ id") {
			t.Fatalf("want missing-id error; got: %v", err)
		}
	})
	t.Run("empty following ratt_ id refused", func(t *testing.T) {
		_, err := ParseAcknowledgeAbandonedFlagPairs([]string{"--acknowledge-abandoned", ""}, "")
		if err == nil || !strings.Contains(err.Error(), "must be non-empty") {
			t.Fatalf("want empty-id error; got: %v", err)
		}
	})
	t.Run("missing --reason refused", func(t *testing.T) {
		_, err := ParseAcknowledgeAbandonedFlagPairs([]string{"--acknowledge-abandoned", "ratt_x"}, "")
		if err == nil || !strings.Contains(err.Error(), "requires a non-empty --reason") {
			t.Fatalf("want missing-reason error; got: %v", err)
		}
	})
	t.Run("--reason value starting with -- refused", func(t *testing.T) {
		_, err := ParseAcknowledgeAbandonedFlagPairs([]string{
			"--acknowledge-abandoned", "ratt_x", "--reason", "--steal",
		}, "")
		if err == nil || !strings.Contains(err.Error(), "must not start with --") {
			t.Fatalf("want --reason shape error; got: %v", err)
		}
	})
	t.Run("--reason value with colons preserved verbatim", func(t *testing.T) {
		got, err := ParseAcknowledgeAbandonedFlagPairs([]string{
			"--acknowledge-abandoned", "ratt_x", "--reason", "abandon_ack:ratt_x:nested-colon",
		}, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got[0].Reason != "abandon_ack:ratt_x:nested-colon" {
			t.Fatalf("reason = %q, want colons preserved", got[0].Reason)
		}
	})
}
