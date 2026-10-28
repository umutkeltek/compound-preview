package capgraph

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

func TestSkillSurfacingMissesAppliesKnownMissesAndSkipsUnknown(t *testing.T) {
	home, store := newStore(t)
	if err := store.Add(Node{
		ID:       "skill:web-design-guidelines",
		Kind:     "skill",
		Name:     "Web Design Guidelines",
		Triggers: []string{"css"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := surfaced(store, "app store design review")["skill:web-design-guidelines"]; ok {
		t.Fatal("precondition: skill should not surface before miss feedback")
	}
	misses, err := LoadSkillSurfacingMisses(filepath.Join("..", "..", "fixtures", "capability-surfacing", "skill-surfacing-proposals.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplySkillSurfacingMisses(home, misses, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "review" || report.MissesRead != 2 || report.AppliedMisses != 1 || report.AppliedEdges == 0 {
		t.Fatalf("report = %+v", report)
	}
	if report.FalseTriggersRead != 0 || report.AppliedFalseTriggers != 0 {
		t.Fatalf("unexpected false-trigger counters = %+v", report)
	}
	if len(report.SkippedUnknown) != 1 || report.SkippedUnknown[0] != "production-audit" {
		t.Fatalf("skipped unknown = %+v", report.SkippedUnknown)
	}
	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdMissedWhen, "context:app", "skill:web-design-guidelines") ||
		!hasEdge(edges, EdMissedWhen, "context:store", "skill:web-design-guidelines") {
		t.Fatalf("edges = %+v, want missed_when context report reason", edges)
	}
	if _, ok := surfaced(store, "app store design review")["skill:web-design-guidelines"]; !ok {
		t.Fatal("surfacing miss feedback should raise future surfacing for matching context")
	}
	replay, err := ApplySkillSurfacingMisses(home, misses, t0)
	if err != nil {
		t.Fatal(err)
	}
	if replay.AppliedEdges != 0 || replay.AppliedMisses != 0 {
		t.Fatalf("replay should be idempotent, got %+v", replay)
	}
}

func TestSkillSurfacingMissesAppliesFalseTriggers(t *testing.T) {
	home, store := newStore(t)
	nodeID := "tool:stitch.create-design-system-from-md"
	if err := store.Add(Node{
		ID:       nodeID,
		Kind:     "tool",
		Name:     "stitch.create_design_system_from_design_md",
		Triggers: []string{"design"},
	}); err != nil {
		t.Fatal(err)
	}
	task := "instruction design skill graph hook memory coherence"
	if _, ok := surfaced(store, task)[nodeID]; !ok {
		t.Fatal("precondition: generic design trigger should surface before false-trigger feedback")
	}
	path := filepath.Join(t.TempDir(), "skill-surfacing-feedback.json")
	if err := os.WriteFile(path, []byte(`{
  "generated": "2026-07-01T00:00:00Z",
  "false_triggers": [
    {
      "skill": "tool:stitch.create-design-system-from-md",
      "count": 1,
      "receipts": ["2026-07-01-instruction-design"],
      "reasons": ["instruction design skill graph hook memory coherence"],
      "maxConf": 1.0
    }
  ],
  "perReceipt": [
    {
      "dir": "2026-07-01-instruction-design",
      "kind": "human",
      "lanes": "codex",
      "false_triggered": [
        {
          "skill": "tool:stitch.create-design-system-from-md",
          "confidence": 1.0,
          "reason": "instruction design skill graph hook memory coherence"
        }
      ]
    }
  ]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	misses, err := LoadSkillSurfacingMisses(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplySkillSurfacingMisses(home, misses, t0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.MissesRead != 0 || report.FalseTriggersRead != 1 || report.AppliedFalseTriggers != 1 || report.AppliedEdges == 0 {
		t.Fatalf("report = %+v", report)
	}
	edges := readCapEdges(t, home)
	if !hasEdge(edges, EdFalseTriggeredBy, "context:design", nodeID) ||
		!hasEdge(edges, EdFalseTriggeredBy, "context:memory", nodeID) {
		t.Fatalf("edges = %+v, want false_triggered_by context feedback", edges)
	}
	if _, ok := surfaced(store, task)[nodeID]; ok {
		t.Fatal("false-trigger feedback should lower matching future surfacing below threshold")
	}
}

// A1 (packet 2026-07-15-slice2A-skill-surfacing-verb): an OPEN skill-surfacing
// review item gets exactly the "fold these misses into the capability graph"
// action with exact argv; a sibling item that already carries a resolution
// sidecar gets none.
func TestAnnotateSkillSurfacingQueueAttachesImportActionAndSkipsResolved(t *testing.T) {
	home := t.TempDir()
	openPath := writeSkillSurfacingReviewFixture(t, home, "ssfb_test_open01", "open-item")
	resolvedPath := writeSkillSurfacingReviewFixture(t, home, "ssfb_test_resolved01", "resolved-item")
	writeSkillSurfacingResolutionSidecar(t, resolvedPath)

	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: skillSurfacingReviewArea}, t0)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSkillSurfacingQueue(&queue)
	if len(queue.Items) != 2 {
		t.Fatalf("queue items = %+v", queue.Items)
	}
	var openItem, resolvedItem *reviewartifact.QueueItem
	for i := range queue.Items {
		switch queue.Items[i].ID {
		case "ssfb_test_open01":
			openItem = &queue.Items[i]
		case "ssfb_test_resolved01":
			resolvedItem = &queue.Items[i]
		}
	}
	if openItem == nil || resolvedItem == nil {
		t.Fatalf("expected both fixture items in queue, got %+v", queue.Items)
	}
	if len(openItem.Actions) != 1 {
		t.Fatalf("open item actions = %+v", openItem.Actions)
	}
	got := openItem.Actions[0]
	wantArgv := []string{"compound", "capability", "import-surfacing-misses", openPath}
	if got.ID != "skill-surfacing.import" ||
		got.Label != "Fold these misses into the capability graph" ||
		!got.RequiresReview ||
		strings.Join(got.Argv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("open item action = %+v, want argv %+v", got, wantArgv)
	}
	if len(resolvedItem.Actions) != 0 {
		t.Fatalf("resolved item should carry no action, got %+v", resolvedItem.Actions)
	}
}

// A2: running the extended import-surfacing-misses machinery on a fixture
// item applies capgraph edges AND records the review resolution sidecar; the
// queue re-bands the item out of needs-you; a second run is a clean no-op
// (no duplicate edges, no duplicate resolution, no error).
func TestImportSurfacingMissesResolvesQueueItemAndStaysIdempotent(t *testing.T) {
	home, store := newStore(t)
	if err := store.Add(Node{
		ID:       "skill:web-design-guidelines",
		Kind:     "skill",
		Name:     "Web Design Guidelines",
		Triggers: []string{"design"},
	}); err != nil {
		t.Fatal(err)
	}
	path := writeSkillSurfacingReviewFixture(t, home, "ssfb_test_import01", "session-backfill")

	misses, err := LoadSkillSurfacingMisses(path)
	if err != nil {
		t.Fatal(err)
	}
	applyReport, err := ApplySkillSurfacingMisses(home, misses, t0)
	if err != nil {
		t.Fatal(err)
	}
	if applyReport.AppliedEdges == 0 {
		t.Fatalf("expected apply to add capgraph edges, got %+v", applyReport)
	}

	queue, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: skillSurfacingReviewArea}, t0)
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSkillSurfacingQueue(&queue)
	if len(queue.Items) != 1 || len(queue.Items[0].Actions) != 1 || queue.Items[0].Band == reviewartifact.BandFiled {
		t.Fatalf("pre-resolve queue item = %+v", queue.Items)
	}

	resolved, result, err := ResolveSkillSurfacingMissesReviewItem(home, path, "operator", t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !resolved || result.ReceiptID == "" || result.SidecarPath == "" {
		t.Fatalf("expected resolution to apply, resolved=%t result=%+v", resolved, result)
	}
	sidecarPath := strings.TrimSuffix(path, ".json") + ".reviewed.json"
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("resolution sidecar missing: %v", err)
	}

	queue, err = reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: skillSurfacingReviewArea}, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateSkillSurfacingQueue(&queue)
	if len(queue.Items) != 1 || queue.Items[0].Band != reviewartifact.BandFiled || len(queue.Items[0].Actions) != 0 {
		t.Fatalf("post-resolve queue item should be filed with no action: %+v", queue.Items[0])
	}

	replayApply, err := ApplySkillSurfacingMisses(home, misses, t0.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replayApply.AppliedEdges != 0 {
		t.Fatalf("replay apply should add no duplicate edges, got %+v", replayApply)
	}
	replayResolved, _, err := ResolveSkillSurfacingMissesReviewItem(home, path, "operator", t0.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("replay resolve should exit clean, got err=%v", err)
	}
	if replayResolved {
		t.Fatal("replay resolve should be a no-op against an already-resolved item, not resolve again")
	}
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("resolution sidecar should still be the single one from the first run: %v", err)
	}
}

func writeSkillSurfacingReviewFixture(t *testing.T, home, id, tag string) string {
	t.Helper()
	dir := filepath.Join(home, "reviews", skillSurfacingReviewArea)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, tag+".json")
	payload := fmt.Sprintf(`{
  "id": %q,
  "generated": "2026-07-09T18:39:12Z",
  "scanned": 1,
  "skipped": 0,
  "laneFails": 0,
  "remaining": 0,
  "proposals": [],
  "all_misses": [
    {
      "skill": "web-design-guidelines",
      "count": 1,
      "receipts": [%q],
      "reasons": ["test fixture reason for %s"],
      "maxConf": 0.9
    }
  ],
  "false_triggers": [],
  "perReceipt": [
    {
      "dir": %q,
      "kind": "human",
      "lanes": "deterministic-backfill",
      "real": [
        {"skill": "web-design-guidelines", "confidence": 0.9, "reason": "test fixture reason for %s"}
      ],
      "false_triggered": []
    }
  ]
}`, id, "packet-"+tag, tag, "packet-"+tag, tag)
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSkillSurfacingResolutionSidecar(t *testing.T, corePath string) {
	t.Helper()
	sidecarPath := strings.TrimSuffix(corePath, ".json") + ".reviewed.json"
	stem := filepath.Base(strings.TrimSuffix(corePath, ".json"))
	sidecar := reviewartifact.ReviewResolution{
		SchemaVersion:     reviewartifact.ReviewResolutionSchemaVersion,
		ID:                "rres_test_" + stem,
		Status:            "recorded",
		Decision:          "reviewed",
		Reason:            "test fixture pre-resolved",
		Actor:             "operator",
		ReviewedBy:        "operator",
		ResolvedAt:        reviewartifact.Timestamp(t0),
		SourceArtifactRef: "reviews/" + skillSurfacingReviewArea + "/" + filepath.Base(corePath),
		PreviousStatus:    "unknown",
		ReceiptID:         "rec_test_" + stem,
		UndoHint:          "compound reviews resolve --item test --undo --reason 'test'",
	}
	data, err := json.MarshalIndent(sidecar, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
