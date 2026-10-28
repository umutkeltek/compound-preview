package reviewartifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDerivedOperatorCardStatesWhatWhyAction is the core plain-language
// falsifier: a cardless record's derived card must state WHAT the record is
// (its own title clause), WHY it sits in front of the operator (its own
// status), and ONE concrete action (its own next_action) — and it must
// validate and read at or below eighth grade.
func TestDerivedOperatorCardStatesWhatWhyAction(t *testing.T) {
	payload := map[string]any{
		"title":          "Nightly build is red on the smoke suite",
		"status":         "review_required",
		"readiness":      "needs_review",
		"next_action":    "Approve or decline the nightly build",
		"blocked_reason": "",
	}
	card := DerivedOperatorCard(payload, "product-hypotheses")
	if err := ValidateOperatorCard(card); err != nil {
		t.Fatalf("derived card invalid: %v", err)
	}
	if !strings.Contains(card.Summary, "Nightly build is red on the smoke suite") {
		t.Fatalf("summary lost the record's own what: %q", card.Summary)
	}
	if !strings.Contains(card.Summary, "decision") {
		t.Fatalf("summary lost the why (waiting on a decision): %q", card.Summary)
	}
	if card.Unlock != "Approve or decline the nightly build" {
		t.Fatalf("unlock = %q, want the record's own next_action", card.Unlock)
	}
	if !Readable(card.Summary) || !Readable(card.Unlock) {
		t.Fatalf("derived card above grade (summary %.2f, unlock %.2f)", GradeLevel(card.Summary), GradeLevel(card.Unlock))
	}
}

// TestDerivedOperatorCardBlockedAndReadyVariants pins the why clause for the
// other statuses a cardless record can carry.
func TestDerivedOperatorCardBlockedAndReadyVariants(t *testing.T) {
	blocked := DerivedOperatorCard(map[string]any{
		"status":         "review_required",
		"blocked_reason": "the upstream lease expired",
	}, "route-policy")
	if !strings.Contains(blocked.Summary, "blocked: the upstream lease expired.") {
		t.Fatalf("blocked summary = %q, want the record's blocked reason", blocked.Summary)
	}
	ready := DerivedOperatorCard(map[string]any{
		"status":    "review_required",
		"readiness": "ready_to_apply",
	}, "route-policy")
	if !strings.Contains(ready.Summary, "ready to apply.") {
		t.Fatalf("ready summary = %q", ready.Summary)
	}
	done := DerivedOperatorCard(map[string]any{"status": "completed"}, "worktree-retirement")
	if !strings.Contains(done.Summary, "already finished.") {
		t.Fatalf("terminal summary = %q", done.Summary)
	}
}

// TestDerivedOperatorCardDegradesPastMachineSyntax feeds free-text fields
// full of banned machine syntax and unreadable jargon: the ladder must still
// return a validating, eighth-grade card that never echoes the syntax.
func TestDerivedOperatorCardDegradesPastMachineSyntax(t *testing.T) {
	hostile := map[string]any{
		"title":          "fix --force=1 by editing /Users/tester/secret.json and _a1b2c3d4e5f6 now",
		"status":         "review_required",
		"blocked_reason": "path /Users/tester/x.md blocked id _deadbeefcafe",
		"next_action":    "run compound --config=/Users/tester/x.json --yes?",
	}
	card := DerivedOperatorCard(hostile, "capability-evaluation-intake")
	if err := ValidateOperatorCard(card); err != nil {
		t.Fatalf("hostile payload produced an invalid card: %v (%+v)", err, card)
	}
	if !Readable(card.Summary) || !Readable(card.Unlock) {
		t.Fatalf("hostile payload card above grade: %q / %q", card.Summary, card.Unlock)
	}
	if strings.Contains(card.Summary, "/Users/") || strings.Contains(card.Summary, "--force") {
		t.Fatalf("summary leaked machine syntax: %q", card.Summary)
	}
	if strings.Contains(card.Unlock, "?") {
		t.Fatalf("unlock asked a question: %q", card.Unlock)
	}
	if !strings.Contains(card.Summary, "blocked") {
		t.Fatalf("degraded summary lost the why: %q", card.Summary)
	}
}

// TestDerivedOperatorCardAlwaysValidates sweeps hostile payload shapes: the
// composer may never hand back a card that fails ValidateOperatorCard, or
// Write would refuse to persist records it previously accepted.
func TestDerivedOperatorCardAlwaysValidates(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		area    string
	}{
		{"empty payload", map[string]any{}, "panel-verdicts"},
		{"nil payload", nil, "panel-verdicts"},
		{"jargon area", map[string]any{"status": "review_required"}, "capability-evaluation-intake"},
		{"nested area", map[string]any{"status": "review_required"}, "distiller-eval/proofs"},
		{"empty area", map[string]any{"status": "review_required"}, ""},
		{"long title", map[string]any{"title": strings.Repeat("word ", 60), "status": "review_required"}, "panel-verdicts"},
		{"unknown status", map[string]any{"status": "weird-new-status"}, "panel-verdicts"},
	}
	for _, c := range cases {
		card := DerivedOperatorCard(c.payload, c.area)
		if err := ValidateOperatorCard(card); err != nil {
			t.Errorf("%s: derived card invalid: %v (%+v)", c.name, err, card)
			continue
		}
		if !Readable(card.Summary) || !Readable(card.Unlock) {
			t.Errorf("%s: derived card above grade: %q / %q", c.name, card.Summary, card.Unlock)
		}
		if card.Unlock == "" {
			t.Errorf("%s: derived card carries no concrete action", c.name)
		}
		if strings.Contains(card.Summary, "needs your read") {
			t.Errorf("%s: derived card is the retired generic fallback: %q", c.name, card.Summary)
		}
	}
}

// TestDerivedOperatorCardActionLadder pins the one-concrete-action ladder:
// next_action wins, then the first exposed action label, then the review verb.
func TestDerivedOperatorCardActionLadder(t *testing.T) {
	withAction := DerivedOperatorCard(map[string]any{
		"status":  "review_required",
		"actions": []any{map[string]any{"label": "Allow edits to this surface"}},
	}, "harness-edit-proposals")
	if withAction.Unlock != "Allow edits to this surface" {
		t.Fatalf("unlock = %q, want the exposed action label", withAction.Unlock)
	}
	bare := DerivedOperatorCard(map[string]any{"status": "review_required"}, "bench")
	if bare.Unlock != "Open it with compound reviews --area bench." {
		t.Fatalf("bare unlock = %q, want the area-named review verb", bare.Unlock)
	}
	longArea := DerivedOperatorCard(map[string]any{"status": "review_required"}, "route-policy")
	if longArea.Unlock != "Open the review queue and read this record." {
		t.Fatalf("long-area unlock = %q, want the readable terminal rung", longArea.Unlock)
	}
	jargon := DerivedOperatorCard(map[string]any{"status": "review_required"}, "capability-evaluation-intake")
	if jargon.Unlock != "Open the review queue and read this record." {
		t.Fatalf("jargon unlock = %q, want the readable terminal rung", jargon.Unlock)
	}
}

// TestWriteDerivesPlainCardForCardlessPayload observes the writer seam: a
// payload with no authored card gets a derived what/why/action card stamped
// (summary_source stays "fallback" — the writer did not author it), and the
// retired generic sentence never lands on disk.
func TestWriteDerivesPlainCardForCardlessPayload(t *testing.T) {
	home := t.TempDir()
	paths, err := Write(home, "product-content-queue", "pcq_1", map[string]any{
		"id":          "pcq_1",
		"title":       "Draft post needs a sponsor before publish",
		"status":      "review_required",
		"next_action": "Approve or decline the draft post",
	}, " ", "# Draft\n")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	card, ok := OperatorCardFromMap(payload)
	if !ok {
		t.Fatalf("no operator card injected: %s", raw)
	}
	if err := ValidateOperatorCard(card); err != nil {
		t.Fatalf("injected card invalid: %v", err)
	}
	if !strings.Contains(card.Summary, "Draft post needs a sponsor before publish") || !strings.Contains(card.Summary, "decision") {
		t.Fatalf("injected summary lacks what/why: %q", card.Summary)
	}
	if card.Unlock != "Approve or decline the draft post" {
		t.Fatalf("injected unlock = %q, want the record's next_action", card.Unlock)
	}
	if payload["summary_source"] != SummarySourceFallback {
		t.Fatalf("summary_source = %v, want %q", payload["summary_source"], SummarySourceFallback)
	}
	if strings.Contains(card.Summary, "needs your read") {
		t.Fatalf("retired generic fallback reached disk: %q", card.Summary)
	}
}

// TestQueueReplacesLegacyGenericSummaryOnLoad is the operator-visible
// regression test for artifacts already on disk: a record whose stored card
// is the retired generic sentence is re-derived at load, so the queue, the
// attention report, and the daily digest all show a specific card.
func TestQueueReplacesLegacyGenericSummaryOnLoad(t *testing.T) {
	home := t.TempDir()
	areaDir := filepath.Join(home, "reviews", "product-content-queue")
	if err := os.MkdirAll(areaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{
 "id": "pcq_legacy",
 "status": "review_required",
 "title": "Weekly changelog draft waits for a sponsor",
 "operator_summary": "product content queue: a new record needs your read.",
 "context_label": "product content queue",
 "group_key": "product-content-queue|fallback",
 "summary_source": "fallback",
 "next_action": "Approve or decline the changelog draft"
}`
	if err := os.WriteFile(filepath.Join(areaDir, "pcq_legacy.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Queue(home, QueueOptions{}, time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("queue items = %d, want 1: %+v", len(report.Items), report.Items)
	}
	item := report.Items[0]
	if strings.Contains(item.OperatorSummary, "needs your read") {
		t.Fatalf("legacy generic summary survived queue load: %q", item.OperatorSummary)
	}
	if !strings.Contains(item.OperatorSummary, "changelog draft waits for a sponsor") {
		t.Fatalf("re-derived summary lost the record's own title: %q", item.OperatorSummary)
	}
	if item.OperatorUnlock != "Approve or decline the changelog draft" {
		t.Fatalf("re-derived unlock = %q, want the record's next_action", item.OperatorUnlock)
	}
	if report.Counts.NeedsYouFallbackGroupCount != 1 || report.Counts.FallbackSummaryCount != 1 {
		t.Fatalf("debt counters must still track the unauthored card: %+v", report.Counts)
	}
	for _, group := range report.Groups {
		if strings.Contains(group.OperatorSummary, "needs your read") {
			t.Fatalf("generic fallback reached a queue group: %q", group.OperatorSummary)
		}
	}
}
