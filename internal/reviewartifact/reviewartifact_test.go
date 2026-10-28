package reviewartifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

// These tests pin the exact behaviour every migrated call site relied on. They
// are written to FAIL if the shared mechanism ever drifts from the per-package
// helpers it replaced — that drift is the only way collapsing 17 writers into
// one module could change a persisted artifact.

func TestDigest12AndID(t *testing.T) {
	want := hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("abc")); return s[:] }())[:12]
	if got := Digest12("abc"); got != want {
		t.Fatalf("Digest12 = %q, want %q", got, want)
	}
	if got := Digest12("abc"); len(got) != 12 {
		t.Fatalf("Digest12 length = %d, want 12", len(got))
	}
	// ID joins parts with "|" then prefixes — matches the old idFor/idSkill/idImprovement.
	if got, want := ID("evp_", "cand-1", "/corpus"), "evp_"+Digest12("cand-1|/corpus"); got != want {
		t.Fatalf("ID = %q, want %q", got, want)
	}
	if got, want := ID("p_", "x"), "p_"+Digest12("x"); got != want {
		t.Fatalf("ID single part = %q, want %q", got, want)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct {
		in     string
		maxLen int
		want   string
	}{
		{"Hello, World!", 0, "hello-world"},
		{"  spaced  out  ", 0, "spaced-out"},
		{"___leading_and_trailing___", 0, "leading-and-trailing"},
		{"UPPER_Snake.Case", 0, "upper-snake-case"},
		{"", 0, ""},
		{"!!!", 0, ""},
		{"a1b2c3", 0, "a1b2c3"},
	}
	for _, c := range cases {
		if got := Slugify(c.in, c.maxLen); got != c.want {
			t.Errorf("Slugify(%q, %d) = %q, want %q", c.in, c.maxLen, got, c.want)
		}
	}
	// maxLen truncates and re-trims so the result never ends in a dash.
	long := "this-is-a-very-long-subject-line-that-exceeds-the-limit-by-a-lot"
	got := Slugify(long, 48)
	if len(got) > 48 {
		t.Fatalf("Slugify maxLen not applied: len=%d", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Fatalf("Slugify truncation left a trailing dash: %q", got)
	}
	// A value whose 48th char lands on a dash boundary must be trimmed.
	if g := Slugify("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbb", 48); strings.HasSuffix(g, "-") {
		t.Fatalf("trailing dash after truncation: %q", g)
	}
}

func TestUniqueSortedAndUnique(t *testing.T) {
	in := []string{" b ", "a", "b", "", "c", "a"}
	if got := UniqueSorted(in); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("UniqueSorted = %v, want [a b c]", got)
	}
	// Unique preserves first-seen order; UniqueSorted does not.
	if got := Unique(in); strings.Join(got, ",") != "b,a,c" {
		t.Fatalf("Unique = %v, want [b a c]", got)
	}
	if got := Unique(nil); got != nil {
		t.Fatalf("Unique(nil) = %v, want nil", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "  ", "x ", "y"); got != "x" {
		t.Fatalf("FirstNonEmpty = %q, want %q", got, "x")
	}
	if got := FirstNonEmpty("", "   "); got != "" {
		t.Fatalf("FirstNonEmpty all-empty = %q, want \"\"", got)
	}
}

func TestHashFileTarget(t *testing.T) {
	if exists, hash, err := HashFileTarget(""); err != nil || exists || hash != "" {
		t.Fatalf("empty path: (%v,%q,%v), want (false,\"\",nil)", exists, hash, err)
	}
	if exists, hash, err := HashFileTarget(filepath.Join(t.TempDir(), "nope")); err != nil || exists || hash != "" {
		t.Fatalf("missing path: (%v,%q,%v), want (false,\"\",nil)", exists, hash, err)
	}
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(fp, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	exists, hash, err := HashFileTarget(fp)
	wantSum := sha256.Sum256([]byte("payload"))
	if err != nil || !exists || hash != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("file hash = (%v,%q,%v), want (true,%q,nil)", exists, hash, err, hex.EncodeToString(wantSum[:]))
	}
	// A directory target is an error, not a recursive walk (finding 2).
	if _, _, err := HashFileTarget(dir); err == nil {
		t.Fatal("HashFileTarget(dir) must error, not walk")
	}
}

func TestHashTreeTarget(t *testing.T) {
	// File branch matches HashFileTarget.
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.txt")
	_ = os.WriteFile(fp, []byte("payload"), 0o644)
	_, fh, _ := HashTreeTarget(fp)
	wantSum := sha256.Sum256([]byte("payload"))
	if fh != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("tree file hash mismatch: %q", fh)
	}
	// Directory branch: stable regardless of file creation order.
	dirA := t.TempDir()
	_ = os.WriteFile(filepath.Join(dirA, "a"), []byte("1"), 0o644)
	_ = os.WriteFile(filepath.Join(dirA, "b"), []byte("2"), 0o644)
	dirB := t.TempDir()
	_ = os.WriteFile(filepath.Join(dirB, "b"), []byte("2"), 0o644)
	_ = os.WriteFile(filepath.Join(dirB, "a"), []byte("1"), 0o644)
	_, hashA, _ := HashTreeTarget(dirA)
	_, hashB, _ := HashTreeTarget(dirB)
	if hashA == "" || hashA != hashB {
		t.Fatalf("dir hash not order-independent: %q vs %q", hashA, hashB)
	}
}

// Finding 1: review ids/names and areas are joined into paths, and some come
// from edited/generated YAML — a path-shaped value must be rejected, not allowed
// to escape reviews/<area>/.
func TestPathValidationRejectsTraversal(t *testing.T) {
	home := t.TempDir()
	type rec struct {
		ID string `json:"id"`
	}
	for _, n := range []string{"../escape", "a/b", "..", ".", "x/../y", "/abs", "a\\b", "  "} {
		if _, err := Write(home, "area", n, rec{}, " ", "m"); err == nil {
			t.Errorf("Write id=%q: expected validation error", n)
		}
		if _, err := WriteJSON(home, "area", n, rec{}, " "); err == nil {
			t.Errorf("WriteJSON name=%q: expected validation error", n)
		}
		if _, err := WriteRaw(home, "area", n, []byte("x")); err == nil {
			t.Errorf("WriteRaw name=%q: expected validation error", n)
		}
	}
	for _, a := range []string{"../up", "/abs", "a/../b", "..", ""} {
		if _, err := Dir(home, a); err == nil {
			t.Errorf("Dir area=%q: expected validation error", a)
		}
	}
	// A valid nested area + bare name still works.
	if _, err := WriteJSON(home, "distiller-eval/proofs", "p.json", rec{ID: "ok"}, "  "); err != nil {
		t.Fatalf("valid nested area/name rejected: %v", err)
	}
}

func TestWrite(t *testing.T) {
	home := t.TempDir()
	type payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	p := payload{ID: "evp_x", Name: "demo"}
	paths, err := Write(home, "distiller-eval", "evp_x", p, " ", "# Title\n")
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(home, "reviews", "distiller-eval")
	if paths.PlanPath != filepath.Join(wantDir, "evp_x.json") {
		t.Fatalf("PlanPath = %q", paths.PlanPath)
	}
	if paths.ReviewPath != filepath.Join(wantDir, "evp_x.md") {
		t.Fatalf("ReviewPath = %q", paths.ReviewPath)
	}

	jsonBytes, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	// JSON is indented with the caller's indent and ends in exactly one newline.
	if !strings.HasSuffix(string(jsonBytes), "}\n") || strings.HasSuffix(string(jsonBytes), "}\n\n") {
		t.Fatalf("json trailing newline wrong: %q", string(jsonBytes))
	}
	if !strings.Contains(string(jsonBytes), "\n \"id\":") {
		t.Fatalf("json indent not preserved (want single space): %q", string(jsonBytes))
	}
	var got payload
	if err := json.Unmarshal(jsonBytes, &got); err != nil || got != p {
		t.Fatalf("round-trip = %+v err=%v", got, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(jsonBytes, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["operator_summary"] == "" || raw["summary_source"] != SummarySourceFallback {
		t.Fatalf("fallback operator card not injected: %+v", raw)
	}

	md, _ := os.ReadFile(paths.ReviewPath)
	if string(md) != "# Title\n" {
		t.Fatalf("markdown written non-verbatim: %q", string(md))
	}

	// Two-space indent is preserved too (the other half of the call sites).
	paths2, err := Write(home, "capability-skills", "skp_y", p, "  ", "x")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(paths2.PlanPath)
	if !strings.Contains(string(b2), "\n  \"id\":") {
		t.Fatalf("two-space indent not preserved: %q", string(b2))
	}

	// Empty id is rejected; empty home is rejected.
	if _, err := Write(home, "x", "  ", p, " ", "m"); err == nil {
		t.Fatal("expected error for empty id")
	}
	if _, err := Write("  ", "x", "id", p, " ", "m"); err == nil {
		t.Fatal("expected error for empty home")
	}
}

func TestRecountBuildsDecisionGroups(t *testing.T) {
	report := QueueReport{Items: []QueueItem{
		{
			Area:            "panel-verdicts",
			ID:              "pv_1",
			Status:          "review_required",
			Readiness:       "needs_review",
			Band:            BandNeedsYou,
			OperatorSummary: "The judge panel needs your call. Record it?",
			ContextLabel:    "bakeoff round",
			GroupKey:        "panel-verdicts|round|reject",
			EvidenceRefs:    []string{"claim:a"},
			Actions: []Action{{
				ID:             "record_review",
				Command:        "compound reviews record one",
				RequiresReview: true,
			}},
		},
		{
			Area:            "panel-verdicts",
			ID:              "pv_2",
			Status:          "review_required",
			Readiness:       "needs_review",
			Band:            BandNeedsYou,
			OperatorSummary: "The judge panel needs your call. Record it?",
			ContextLabel:    "bakeoff round",
			GroupKey:        "panel-verdicts|round|reject",
			EvidenceRefs:    []string{"claim:b", "claim:a"},
			Actions: []Action{{
				ID:             "record_review",
				Command:        "compound reviews record two",
				RequiresReview: true,
			}},
		},
		{
			Area:            "panel-verdicts",
			ID:              "pv_done",
			Status:          "recorded",
			Readiness:       "review_recorded",
			Band:            BandFiled,
			OperatorSummary: "Already recorded.",
			ContextLabel:    "bakeoff round",
			GroupKey:        "panel-verdicts|round|reject",
		},
	}}
	Recount(&report)

	if report.Counts.ReviewGroupCount != 1 || len(report.Groups) != 1 {
		t.Fatalf("groups = %+v counts=%+v, want one open group", report.Groups, report.Counts)
	}
	group := report.Groups[0]
	if group.Key != "panel-verdicts|round|reject" || group.Count != 2 {
		t.Fatalf("group identity/count = %+v", group)
	}
	if strings.Join(group.ItemIDs, ",") != "pv_1,pv_2" {
		t.Fatalf("group item ids = %+v", group.ItemIDs)
	}
	if strings.Join(group.EvidenceRefs, ",") != "claim:a,claim:b" {
		t.Fatalf("group evidence refs = %+v", group.EvidenceRefs)
	}
	if len(group.EvidenceSummaries) != 2 {
		t.Fatalf("group evidence summaries = %+v, want 2", group.EvidenceSummaries)
	}
	if got := group.EvidenceSummaries[0]; got.Ref != "claim:a" || got.Label != "a" || got.SourceKind != "claim" || got.Privacy != "unknown" || got.OpenCommand != "compound system lineage claim:a --json" {
		t.Fatalf("first evidence summary = %+v", got)
	}
	if got := group.EvidenceSummaries[1]; got.Ref != "claim:b" || got.Label != "b" || got.SourceKind != "claim" || got.OpenCommand != "compound system lineage claim:b --json" {
		t.Fatalf("second evidence summary = %+v", got)
	}
	if group.SafeBatchAction != nil || !strings.Contains(group.TerminalReason, "item-specific") {
		t.Fatalf("unsafe item-specific actions should not batch: %+v", group)
	}
	if group.Sample == nil || group.Sample.ID != "pv_1" {
		t.Fatalf("sample = %+v, want pv_1", group.Sample)
	}
}

func TestRecountEvidenceSummariesUsePanelDecisionContext(t *testing.T) {
	report := QueueReport{Items: []QueueItem{{
		Area:            "panel-verdicts",
		ID:              "pv_context",
		Status:          "review_required",
		Readiness:       "needs_review",
		Band:            BandNeedsYou,
		OperatorSummary: "The judge panel needs your call. Record it?",
		ContextLabel:    "private transcript bakeoff",
		GroupKey:        "panel-verdicts|prun_context|needs_human",
		EvidenceRefs:    []string{"artifact:panel-runs/prun_context.json"},
		DecisionCard: &DecisionCard{
			Claim:     "local model beat the baseline on classify-extract",
			PanelLean: "needs_human",
			Meta: map[string]string{
				"run_id":      "prun_context",
				"target_kind": "comparison",
				"target_ref":  "cmp_context",
			},
		},
	}}}
	Recount(&report)

	if len(report.Groups) != 1 || len(report.Groups[0].EvidenceSummaries) != 1 {
		t.Fatalf("groups = %+v, want one evidence summary", report.Groups)
	}
	summary := report.Groups[0].EvidenceSummaries[0]
	for _, want := range []string{"Panel verdict prun_context", "needs human", "comparison/cmp_context", "local model beat the baseline"} {
		if !strings.Contains(summary.Summary, want) {
			t.Fatalf("summary = %q, want %q", summary.Summary, want)
		}
	}
}

func TestTimestamp(t *testing.T) {
	at := time.Date(2026, 6, 30, 12, 0, 0, 0, time.FixedZone("x", 3*3600))
	if got := Timestamp(at); got != "2026-06-30T09:00:00Z" {
		t.Fatalf("Timestamp = %q, want UTC RFC3339", got)
	}
}

func TestWriteJSON(t *testing.T) {
	home := t.TempDir()
	type rec struct {
		ID string `json:"id"`
	}
	// Nested area + custom suffix + no markdown sibling.
	path, err := WriteJSON(home, "distiller-eval/proofs", "p1.json", rec{ID: "x"}, "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "reviews", "distiller-eval", "proofs", "p1.json") {
		t.Fatalf("path = %q", path)
	}
	if _, err := os.Stat(filepath.Join(home, "reviews", "distiller-eval", "proofs", "p1.md")); !os.IsNotExist(err) {
		t.Fatal("WriteJSON must NOT create a .md sibling")
	}
	b, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(b), "}\n") || !strings.Contains(string(b), "\n  \"id\":") {
		t.Fatalf("indent/newline not preserved: %q", string(b))
	}
	var got rec
	if err := json.Unmarshal(b, &got); err != nil || got.ID != "x" {
		t.Fatalf("round-trip failed: %+v %v", got, err)
	}
	if _, err := WriteJSON("  ", "a", "n.json", rec{}, " "); err == nil {
		t.Fatal("expected error for empty home")
	}
}

func TestWriteRaw(t *testing.T) {
	home := t.TempDir()
	body := []byte("schema_version: x\nid: y\n") // YAML manifest, no extra newline added
	path, err := WriteRaw(home, "capability-evaluation-intake", "r1.reviewed-skill-manifest.yaml", body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(body) {
		t.Fatalf("WriteRaw altered bytes: %q != %q", string(b), string(body))
	}
}

func TestQueueSummarizesReviewArtifacts(t *testing.T) {
	home := t.TempDir()
	_, err := Write(home, "decision-lifecycle-repairs", "dlrp_a", map[string]any{
		"id":            "dlrp_a",
		"status":        "review_required",
		"created_at":    "2026-07-01T00:00:00Z",
		"source_kind":   "note",
		"source_root":   "owner-root",
		"owner":         "capability-intake",
		"not_to_do":     "do not import external repo wholesale",
		"boundaries":    []string{"review-only", "no graph mutation", "review-only"},
		"repair_count":  2,
		"evidence_refs": []string{"e2", "e1", "e1"},
		"next_action":   "review owner repair",
	}, " ", "# Review\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteRaw(home, "decision-lifecycle-repairs", "dlrp_a.review.yaml", []byte("status: pending\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteJSON(home, "decision-lifecycle-repairs", "dlrp_a.dlra_1.apply.json", map[string]any{
		"id":      "dlra_1",
		"plan_id": "dlrp_a",
		"status":  "ok",
	}, " "); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteJSON(home, "decision-lifecycle-repairs", "dlrp_a.decision_1.decision.json", map[string]any{
		"id":        "decision_1",
		"review_id": "dlrp_a",
		"status":    "recorded",
	}, " "); err != nil {
		t.Fatal(err)
	}
	_, err = Write(home, "decision-lifecycle-repairs", "dlrp_ok", map[string]any{
		"id":     "dlrp_ok",
		"status": "ok",
	}, " ", "# OK\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Write(home, "route-policy", "rpp_b", map[string]any{
		"id":          "rpp_b",
		"status":      "held",
		"repairs":     []string{"one", "two", "three"},
		"target_path": "target.md",
	}, " ", "# Route\n")
	if err != nil {
		t.Fatal(err)
	}
	report, err := Queue(home, QueueOptions{}, time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != QueueSchemaVersion || report.GeneratedAt != "2026-07-01T01:00:00Z" {
		t.Fatalf("bad report identity: %+v", report)
	}
	if report.Counts.Total != 3 || report.Counts.ReviewRequired != 2 || report.Counts.OK != 1 {
		t.Fatalf("bad counts: %+v", report.Counts)
	}
	if report.Counts.ByArea["decision-lifecycle-repairs"] != 2 || report.Counts.ByArea["route-policy"] != 1 {
		t.Fatalf("bad area counts: %+v", report.Counts.ByArea)
	}
	if report.Items[0].ID != "dlrp_a" || report.Items[0].Status != "review_required" {
		t.Fatalf("review-required item should sort first: %+v", report.Items)
	}
	if report.Items[0].NextAction != "review owner repair" {
		t.Fatalf("next_action not surfaced: %+v", report.Items[0])
	}
	if report.Items[0].Owner != "capability-intake" || report.Items[0].NotToDo != "do not import external repo wholesale" {
		t.Fatalf("owner placement metadata not surfaced: %+v", report.Items[0])
	}
	if strings.Join(report.Items[0].Boundaries, ",") != "no graph mutation,review-only" {
		t.Fatalf("boundaries not unique/sorted: %+v", report.Items[0].Boundaries)
	}
	if report.Items[0].MarkdownPath == "" || len(report.Items[0].ExtraPaths) != 3 {
		t.Fatalf("expected markdown and draft sibling: %+v", report.Items[0])
	}
	if strings.Join(report.Items[0].EvidenceRefs, ",") != "e1,e2" {
		t.Fatalf("evidence refs not unique/sorted: %+v", report.Items[0].EvidenceRefs)
	}
	if report.Items[1].RepairCount != 3 {
		t.Fatalf("repair count should fall back to repairs length: %+v", report.Items[1])
	}
	filtered, err := Queue(home, QueueOptions{Area: "route-policy"}, time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Counts.Total != 1 || filtered.Items[0].Area != "route-policy" {
		t.Fatalf("area filter failed: %+v", filtered)
	}
}

func TestQueueEmptyReviews(t *testing.T) {
	home := t.TempDir()
	report, err := Queue(home, QueueOptions{}, time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Total != 0 || len(report.Items) != 0 {
		t.Fatalf("empty queue = %+v", report)
	}
}

func TestQueueAreaFilterDoesNotTraverseUnrelatedAreas(t *testing.T) {
	homePath := t.TempDir()
	if _, err := Write(homePath, "route-policy", "rpp_scoped", map[string]any{
		"id":     "rpp_scoped",
		"status": "recorded",
	}, " ", "# Scoped\n"); err != nil {
		t.Fatal(err)
	}
	unrelatedDir := filepath.Join(homePath, "reviews", "unrelated-area")
	if err := os.MkdirAll(unrelatedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelatedDir, "broken.json"), []byte("{not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Queue(homePath, QueueOptions{Area: "route-policy"}, time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("area-scoped queue traversed unrelated review area: %v", err)
	}
	if report.Counts.Total != 1 || len(report.Items) != 1 || report.Items[0].ID != "rpp_scoped" {
		t.Fatalf("area-scoped queue = %+v, want only rpp_scoped", report)
	}
}

func TestRecountTracksReviewCardCoverage(t *testing.T) {
	report := QueueReport{Items: []QueueItem{
		{
			Area:          "capability-evaluation-intake",
			ID:            "needs_card",
			Status:        "review_required",
			Readiness:     "needs_review",
			Band:          BandNeedsYou,
			SummarySource: SummarySourceFallback,
		},
		{
			Area:      "panel-verdicts",
			ID:        "has_card",
			Status:    "review_required",
			Readiness: "needs_review",
			DecisionCard: &DecisionCard{
				Claim: "Review this item.",
			},
			Actions: []Action{{
				ID:             "record",
				Label:          "Record",
				Command:        "compound example",
				RequiresReview: true,
			}},
		},
		{
			Area:   "external-repo-intake",
			ID:     "recorded",
			Status: "recorded",
		},
	}}
	Recount(&report)
	if report.Counts.Total != 3 || report.Counts.ReviewRequired != 2 || report.Counts.OK != 1 {
		t.Fatalf("basic counts = %+v", report.Counts)
	}
	if report.Counts.DecisionCardCount != 1 || report.Counts.ActionableCount != 1 {
		t.Fatalf("coverage counts = %+v", report.Counts)
	}
	if report.Counts.ReviewRequiredMissingDecisionCard != 1 || report.Counts.ReviewRequiredMissingAction != 1 {
		t.Fatalf("missing coverage counts = %+v", report.Counts)
	}
	if report.Counts.FallbackSummaryCount != 1 ||
		report.Counts.ReviewRequiredFallbackSummary != 1 ||
		report.Counts.NeedsYouFallbackGroupCount != 1 ||
		report.Counts.BySummarySource[SummarySourceFallback] != 1 {
		t.Fatalf("fallback summary debt counts = %+v", report.Counts)
	}
}

func TestQueueExcludesArchivedReviewsByDefaultAndIncludesWhenAsked(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	_, err := Write(homePath, "panel-verdicts", "pv_archive", map[string]any{
		"id":     "pv_archive",
		"status": "recorded",
	}, " ", "# Verdict\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteJSON(homePath, "panel-verdicts", "pv_archive.reviewed.json", map[string]any{
		"id":     "pvr_archive",
		"status": "recorded",
	}, " "); err != nil {
		t.Fatal(err)
	}
	if _, err := home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"review"}}, time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	report, err := Queue(homePath, QueueOptions{Area: "panel-verdicts"}, time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Total != 0 || len(report.Items) != 0 {
		t.Fatalf("default queue should exclude archived reviews: %+v", report)
	}

	withArchived, err := Queue(homePath, QueueOptions{Area: "panel-verdicts", IncludeArchived: true}, time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if withArchived.Counts.Total != 1 || withArchived.Counts.OK != 1 || len(withArchived.Items) != 1 {
		t.Fatalf("include archived queue should show recorded review: %+v", withArchived)
	}
	item := withArchived.Items[0]
	if item.ID != "pv_archive" || item.Status != "recorded" {
		t.Fatalf("archived queue item = %+v", item)
	}
	if len(item.ExtraPaths) != 2 || !strings.Contains(strings.Join(item.ExtraPaths, "\n"), "archives/operational/files/reviews/panel-verdicts/pv_archive.reviewed.json") {
		t.Fatalf("archived review sidecars missing: %+v", item.ExtraPaths)
	}
}
