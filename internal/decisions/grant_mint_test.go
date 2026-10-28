package decisions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

var grantMintTestNow = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

// seedGrantMintWork writes a WorkEnvelope directly under work/work/<id>.json,
// mirroring how internal/work.(*Store).list reads envelopes. The annotator
// reads work through work.Open(...).List(), so this fixture drives it end to
// end without importing a mutation seam.
func seedGrantMintWork(t *testing.T, home, id, state string, refs ...string) {
	t.Helper()
	dir := filepath.Join(home, "work", "work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := envelope.WorkEnvelope{State: state}
	env.ID = id
	env.EvidenceRefs = refs
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedValidJobHealthGrant(t *testing.T, home, id string, now time.Time) {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Record(store, home, RecordInput{
		Kind:               KindGrant,
		ID:                 id,
		Title:              "Permit evidence-threshold admission for job health",
		ReviewedBy:         "operator:test",
		Scope:              AutonomyGrantScope,
		AutoAdmitClass:     "job_health",
		MinTerminalSamples: 5,
		MinWindowDays:      7,
		MaxFailures:        0,
		Now:                now,
	}); err != nil {
		t.Fatal(err)
	}
}

func annotateGrantMint(t *testing.T, home, area string) reviewartifact.QueueReport {
	t.Helper()
	report := reviewartifact.QueueReport{Area: area}
	AnnotateAutonomyGrantMintQueue(&report, home)
	return report
}

func grantMintItems(report reviewartifact.QueueReport) []reviewartifact.QueueItem {
	var out []reviewartifact.QueueItem
	for _, item := range report.Items {
		if item.Area == autonomyGrantMintArea {
			out = append(out, item)
		}
	}
	return out
}

// Falsifier 1: an open director-authored job_health envelope + zero grants ⇒
// exactly one grant-mint card, carrying the plain-language decision card.
func TestGrantMintEmitsOneCardWhenWorkWaitsAndNoGrant(t *testing.T) {
	home := t.TempDir()
	seedGrantMintWork(t, home, "work_abc123", "proposed", "director_trigger:job_health:abc123", "proposed_by:director")

	items := grantMintItems(annotateGrantMint(t, home, ""))
	if len(items) != 1 {
		t.Fatalf("expected exactly one autonomy-grant-mint card, got %d: %+v", len(items), items)
	}
	item := items[0]
	if item.Status != "review_required" || item.Band != reviewartifact.BandNeedsYou {
		t.Fatalf("card must be a needs-you review item, got status=%q band=%q", item.Status, item.Band)
	}
	if item.DecisionCard == nil {
		t.Fatal("card must carry a DecisionCard (else it trips ReviewRequiredMissingDecisionCard)")
	}
	if len(item.Actions) == 0 {
		t.Fatal("card must carry an action (else it trips ReviewRequiredMissingAction)")
	}
	// Plain-language copy is the whole point: pin the verbatim strings so a
	// future edit that breaks operator legibility reds here.
	if item.OperatorSummary != "Let proven job-health work enter the queue on its own?" {
		t.Fatalf("operator summary drifted from the packet copy: %q", item.OperatorSummary)
	}
	if item.DecisionCard.Claim != "Let proven job-health work enter the queue on its own?" {
		t.Fatalf("decision claim drifted: %q", item.DecisionCard.Claim)
	}
	wantWhy := "1 job-health item is waiting; each currently needs your click."
	if got := grantMintReasonDetail(item.DecisionCard, "why now"); got != wantWhy {
		t.Fatalf("dynamic why line drifted: got %q want %q", got, wantWhy)
	}
	if len(item.DecisionCard.Boundary) == 0 ||
		item.DecisionCard.Boundary[0] != "Anything external, destructive, irreversible, financial, or security-related still always comes to you. No grant can override that." {
		t.Fatalf("boundary (what never changes) drifted: %+v", item.DecisionCard.Boundary)
	}
	if item.DecisionCard.Meta["waiting_count"] != "1" || item.DecisionCard.Meta["auto_admit_class"] != "job_health" {
		t.Fatalf("decision card meta missing count/class: %+v", item.DecisionCard.Meta)
	}
	// The action carries the exact governed record command (the exact-command
	// disclosure), argv[0]=="compound" so attention's argv gate stays clean.
	argv := item.Actions[0].Argv
	if len(argv) == 0 || argv[0] != "compound" {
		t.Fatalf("action argv must start with compound: %v", argv)
	}
	wantArgv := []string{
		"compound", "decisions", "record",
		"--kind", "grant",
		"--scope", "autonomy-evidence-threshold",
		"--auto-admit-class", "job_health",
		"--min-terminal-samples", "5",
		"--min-window-days", "7",
		"--max-failures", "0",
		"--reviewed-by", "operator-console",
		"--title", "Let proven job-health work enter the queue on its own",
		"--json",
	}
	if !equalStrings(argv, wantArgv) {
		t.Fatalf("action argv drifted:\n got %v\nwant %v", argv, wantArgv)
	}
}

// Falsifier 2: a valid active job_health grant ⇒ zero cards (the decision is
// already made; a resting card would be a defect under the two-class law).
func TestGrantMintSilentWhenValidGrantExists(t *testing.T) {
	home := t.TempDir()
	seedGrantMintWork(t, home, "work_abc123", "proposed", "director_trigger:job_health:abc123")
	seedValidJobHealthGrant(t, home, "job-health-autonomy-grant", grantMintTestNow)

	if items := grantMintItems(annotateGrantMint(t, home, "")); len(items) != 0 {
		t.Fatalf("expected zero cards while a valid grant exists, got %d: %+v", len(items), items)
	}
}

// Falsifier 3: no director-trigger job_health work ⇒ zero cards even with zero
// grants (terminal director work and non-director work never count).
func TestGrantMintSilentWithoutWaitingWork(t *testing.T) {
	home := t.TempDir()
	// terminal director job_health work — excluded because it is done.
	seedGrantMintWork(t, home, "work_done", "completed", "director_trigger:job_health:done01")
	// non-director, non-terminal work — excluded because it is not director-authored.
	seedGrantMintWork(t, home, "work_human", "proposed", "proposed_by:human")

	if items := grantMintItems(annotateGrantMint(t, home, "")); len(items) != 0 {
		t.Fatalf("expected zero cards without waiting director job_health work, got %d: %+v", len(items), items)
	}
}

// Falsifier 4: a superseded grant counts as NO grant ⇒ the card emits. Same for
// a grant recorded under a different scope (director-apply, not autonomy).
func TestGrantMintEmitsWhenGrantSupersededOrWrongScope(t *testing.T) {
	t.Run("superseded", func(t *testing.T) {
		home := t.TempDir()
		seedGrantMintWork(t, home, "work_abc123", "proposed", "director_trigger:job_health:abc123")
		seedValidJobHealthGrant(t, home, "job-health-autonomy-grant", grantMintTestNow)
		store, err := mem.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		// A plain supersede (no autonomy scope/thresholds) revokes the grant
		// without re-granting — this is a revoke, not a fresh grant.
		if _, err := Record(store, home, RecordInput{
			Kind:       KindGrant,
			ID:         "job-health-autonomy-grant-revoke",
			Title:      "Revoke job-health autonomy grant",
			ReviewedBy: "operator:test",
			UndoOf:     "job-health-autonomy-grant",
			Now:        grantMintTestNow.Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		if items := grantMintItems(annotateGrantMint(t, home, "")); len(items) != 1 {
			t.Fatalf("superseded grant must not suppress the card, got %d: %+v", len(items), items)
		}
	})

	t.Run("wrong scope", func(t *testing.T) {
		home := t.TempDir()
		seedGrantMintWork(t, home, "work_abc123", "proposed", "director_trigger:job_health:abc123")
		store, err := mem.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		// A broad director-apply grant is NOT an autonomy grant; it must not
		// suppress the class-scoped autonomy card.
		if _, err := Record(store, home, RecordInput{
			Kind:       KindGrant,
			ID:         "director-apply-grant",
			Title:      "Grant director --apply",
			ReviewedBy: "operator:test",
			Scope:      "director-apply",
			Now:        grantMintTestNow,
		}); err != nil {
			t.Fatal(err)
		}
		if items := grantMintItems(annotateGrantMint(t, home, "")); len(items) != 1 {
			t.Fatalf("director-apply grant must not suppress the autonomy card, got %d: %+v", len(items), items)
		}
	})
}

// Falsifier 5 (leak guard): the synthesized card must not leak into a different
// --area query. It appears only for the unscoped queue or its own area.
func TestGrantMintRespectsAreaScope(t *testing.T) {
	home := t.TempDir()
	seedGrantMintWork(t, home, "work_abc123", "proposed", "director_trigger:job_health:abc123")

	if items := grantMintItems(annotateGrantMint(t, home, "work-advance")); len(items) != 0 {
		t.Fatalf("card leaked into an unrelated --area query: %+v", items)
	}
	if items := grantMintItems(annotateGrantMint(t, home, autonomyGrantMintArea)); len(items) != 1 {
		t.Fatalf("card must appear for its own --area query, got %d", len(items))
	}
}

func grantMintReasonDetail(card *reviewartifact.DecisionCard, label string) string {
	for _, r := range card.Why {
		if r.Label == label {
			return r.Detail
		}
	}
	return ""
}

func equalStrings(a, b []string) bool {
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
