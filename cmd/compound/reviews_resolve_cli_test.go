package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
)

const reviewsResolveCLISubprocessEnv = "COMPOUND_REVIEWS_RESOLVE_CLI_SUBPROCESS"
const reviewsResolveCLIArgsEnv = "COMPOUND_REVIEWS_RESOLVE_CLI_ARGS"

func TestReviewsResolveCLISubprocess(t *testing.T) {
	if os.Getenv(reviewsResolveCLISubprocessEnv) == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(reviewsResolveCLIArgsEnv)), &args); err != nil {
		t.Fatal(err)
	}
	cmdReviews(args)
}

func TestReviewsResolveRequiresReasonUsage(t *testing.T) {
	t.Parallel()
	homePath := initReviewsResolveCLIHome(t)
	revision := reviewResolveRevisionFor(t, homePath, "parity", "parity_unknown_fixture")
	code, output := runReviewsResolveCLI(t, homePath,
		"resolve", "--item", "parity_unknown_fixture", "--area", "parity",
		"--expected-revision", revision, "--decision", "parked",
	)
	if code == 0 {
		t.Fatalf("resolve without --reason exited 0: %s", output)
	}
	if !strings.Contains(output, reviewsResolveUsage) {
		t.Fatalf("resolve without reason missing usage: %s", output)
	}
}

func TestReviewsResolveRequiresOwnerAreaAndRevisionBeforeWriting(t *testing.T) {
	t.Parallel()
	homePath := initReviewsResolveCLIHome(t)
	revision := reviewResolveRevisionFor(t, homePath, "parity", "parity_unknown_fixture")
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "missing area",
			args: []string{
				"resolve", "--item", "parity_unknown_fixture",
				"--expected-revision", revision, "--decision", "parked", "--reason", "fixture reason",
			},
		},
		{
			name: "missing revision",
			args: []string{
				"resolve", "--item", "parity_unknown_fixture", "--area", "parity",
				"--decision", "parked", "--reason", "fixture reason",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := snapshotReviewsResolveHome(t, homePath)
			code, output := runReviewsResolveCLI(t, homePath, tt.args...)
			if code == 0 || !strings.Contains(output, reviewsResolveUsage) {
				t.Fatalf("missing owner scope exit=%d output=%s", code, output)
			}
			assertReviewsResolveHomeUnchanged(t, before, homePath)
		})
	}
}

func TestReviewsResolveGenericReviewIsSidecarOnlyAndUndoRestoresArchiveVisibility(t *testing.T) {
	t.Parallel()
	homePath := initReviewsResolveCLIHome(t)
	revision := reviewResolveRevisionFor(t, homePath, "parity", "parity_unknown_fixture")
	before := snapshotReviewsResolveHome(t, homePath)

	code, output := runReviewsResolveCLI(t, homePath,
		"resolve", "--item", "parity_unknown_fixture", "--area", "parity",
		"--expected-revision", revision, "--decision", "parked",
		"--reason", "hold this parity evidence until the next evidence window", "--json",
	)
	if code != 0 {
		t.Fatalf("resolve exit=%d output=%s", code, output)
	}
	var resolved map[string]any
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&resolved); err != nil {
		t.Fatalf("parse resolve JSON: %v\n%s", err, output)
	}
	afterResolve := snapshotReviewsResolveHome(t, homePath)
	added, changed, removed := snapshotDelta(before, afterResolve)
	if len(changed) != 0 || len(removed) != 0 || len(added) != 2 {
		t.Fatalf("resolve delta added=%v changed=%v removed=%v, want sidecar + receipt only", added, changed, removed)
	}
	var sidecarRel, receiptRel string
	for _, rel := range added {
		switch {
		case strings.HasSuffix(rel, ".reviewed.json"):
			sidecarRel = rel
		case strings.HasPrefix(rel, "events/rec_") && strings.HasSuffix(rel, ".json"):
			receiptRel = rel
		}
	}
	if sidecarRel != "reviews/parity/parity_unknown_fixture.reviewed.json" || receiptRel == "" {
		t.Fatalf("resolve added unexpected files: %v", added)
	}
	if got := resolved["sidecar_path"]; got == "" || got == nil {
		t.Fatalf("resolve JSON missing sidecar_path: %v", resolved)
	}

	queue, err := buildReviewQueueReport(homePath, "parity", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Items) != 1 || queue.Items[0].Status != "recorded" || queue.Items[0].Readiness != "no_action" || queue.Counts.ReviewRequired != 0 {
		t.Fatalf("resolved parity review is not terminal: %+v", queue)
	}
	dryRun, err := home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"review"}, DryRun: true}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if !archiveReportHasPath(dryRun, "reviews/parity/parity_unknown_fixture.json") {
		t.Fatalf("resolved item not selected by archive dry-run: %+v", dryRun)
	}

	code, output = runReviewsResolveCLI(t, homePath,
		"resolve", "--item", "parity_unknown_fixture", "--undo",
		"--reason", "new evidence requires returning this card to review", "--json",
	)
	if code != 0 {
		t.Fatalf("undo exit=%d output=%s", code, output)
	}
	if _, err := os.Stat(filepath.Join(homePath, filepath.FromSlash(sidecarRel))); !os.IsNotExist(err) {
		t.Fatalf("undo must remove active sidecar, stat err=%v", err)
	}
	queue, err = buildReviewQueueReport(homePath, "parity", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Items) != 1 || queue.Items[0].Status != "unknown" || queue.Counts.ReviewRequired != 1 {
		t.Fatalf("undo did not restore prior visibility: %+v", queue)
	}
	dryRun, err = home.ArchiveOperationalHistory(homePath, home.OperationalArchiveOptions{Kinds: []string{"review"}, DryRun: true}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if archiveReportHasPath(dryRun, "reviews/parity/parity_unknown_fixture.json") {
		t.Fatalf("undone item still selected by archive dry-run: %+v", dryRun)
	}

	afterUndo := snapshotReviewsResolveHome(t, homePath)
	if got := countMatchingFiles(afterUndo, "events/rec_", ".json"); got != 2 {
		t.Fatalf("receipt files after resolve+undo = %d, want 2", got)
	}
	for rel := range afterUndo {
		if !strings.HasPrefix(rel, "events/rec_") || !strings.HasSuffix(rel, ".json") {
			continue
		}
		data := readTestPath(t, filepath.Join(homePath, filepath.FromSlash(rel)))
		if kind, err := envelope.ValidateBytes(data); err != nil || kind != "receipt" {
			t.Fatalf("receipt %s validation kind=%q err=%v", rel, kind, err)
		}
	}
	for rel, hash := range before {
		if afterUndo[rel] != hash {
			t.Fatalf("pre-existing file %s mutated across resolve+undo", rel)
		}
	}
}

func initReviewsResolveCLIHome(t *testing.T) string {
	t.Helper()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewartifact.Write(homePath, "parity", "parity_unknown_fixture", map[string]any{
		"id":     "parity_unknown_fixture",
		"status": "unknown",
	}, " ", "# Parity review fixture\n"); err != nil {
		t.Fatal(err)
	}
	review := routeevidence.PromotionReview{
		SchemaVersion:                 routeevidence.PromotionReviewSchemaVersion,
		ID:                            "route_promotion_resolve_fixture",
		CreatedAt:                     "2026-06-29T12:00:00Z",
		Status:                        "review_required",
		Recommendation:                "demote",
		Reason:                        "fixture route evidence crossed the failing threshold",
		LaneID:                        "fixture/route-lane",
		CapabilityClass:               "classify",
		CurrentClaimLevel:             "strong",
		RecommendedClaimLevel:         "weak",
		ProposedChange:                "fixture proposal only",
		EvidenceRefs:                  []string{"route_evidence_fixture"},
		MatchingSemantics:             []string{"fixture"},
		MatchingLimitations:           []string{"fixture only"},
		ModelCapabilitiesAuthorityRef: "compound://registries/model-capabilities",
		ModelCapabilitiesPath:         "registries/model-capabilities.json",
		CriteriaAuthorityRef:          routeevidence.PromotionCriteriaAuthorityRef,
		CriteriaPath:                  "registries/promotion-criteria.yaml",
	}
	if _, err := reviewartifact.Write(homePath, "route-promotions", review.ID, review, " ", "# Route promotion fixture\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "registries", "route-authority-sentinel.json"), []byte("{\"authority\":\"unchanged\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return homePath
}

func runReviewsResolveCLI(t *testing.T, homePath string, args ...string) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReviewsResolveCLISubprocess$")
	cmd.Env = append(os.Environ(),
		"COMPOUND_HOME="+homePath,
		reviewsResolveCLISubprocessEnv+"=1",
		reviewsResolveCLIArgsEnv+"="+string(encoded),
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run reviews resolve subprocess: %v", err)
	}
	return exitErr.ExitCode(), string(output)
}

func snapshotReviewsResolveHome(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == "events/segments" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range testEventEntries(t, root) {
		record, err := entry.Read()
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(record.Bytes)
		out["events/"+entry.ID+".json"] = hex.EncodeToString(sum[:])
	}
	return out
}

func snapshotDelta(before, after map[string]string) (added, changed, removed []string) {
	for rel, hash := range after {
		if old, ok := before[rel]; !ok {
			added = append(added, rel)
		} else if old != hash {
			changed = append(changed, rel)
		}
	}
	for rel := range before {
		if _, ok := after[rel]; !ok {
			removed = append(removed, rel)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return
}

func archiveReportHasPath(report home.OperationalArchiveReport, path string) bool {
	for _, entry := range report.Entries {
		if entry.OriginalPath == path {
			return true
		}
	}
	return false
}

func countMatchingFiles(files map[string]string, prefix, suffix string) int {
	count := 0
	for rel := range files {
		if strings.HasPrefix(rel, prefix) && strings.HasSuffix(rel, suffix) {
			count++
		}
	}
	return count
}

func TestReviewsResolveUnknownItem(t *testing.T) {
	t.Parallel()
	homePath := initReviewsResolveCLIHome(t)
	code, output := runReviewsResolveCLI(t, homePath,
		"resolve", "--item", "missing_review_item", "--area", "parity",
		"--expected-revision", "review-resolution-v2:sha256:"+strings.Repeat("a", 64), "--decision", "dismissed",
		"--reason", "the fixture should not exist",
	)
	if code == 0 || !strings.Contains(output, `unknown review item "missing_review_item"`) {
		t.Fatalf("unknown item exit=%d output=%s", code, output)
	}
}

func TestReviewsResolveStaleRevisionWritesNeitherSidecarNorReceipt(t *testing.T) {
	t.Parallel()
	homePath := initReviewsResolveCLIHome(t)
	staleRevision := reviewResolveRevisionFor(t, homePath, "parity", "parity_unknown_fixture")
	path := filepath.Join(homePath, "reviews", "parity", "parity_unknown_fixture.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	payload["status"] = "review_required"
	body, err = json.MarshalIndent(payload, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotReviewsResolveHome(t, homePath)
	code, output := runReviewsResolveCLI(t, homePath,
		"resolve", "--item", "parity_unknown_fixture", "--area", "parity",
		"--expected-revision", staleRevision, "--decision", "parked",
		"--reason", "this request rendered before owner state changed", "--json",
	)
	if code == 0 || !strings.Contains(output, "changed since it was rendered") {
		t.Fatalf("stale revision exit=%d output=%s", code, output)
	}
	var staleConflict reviewsResolveErrorResult
	if err := json.Unmarshal([]byte(output), &staleConflict); err != nil || staleConflict.ErrorCode != "stale_action_revision" {
		t.Fatalf("stale revision missing structured conflict code: err=%v result=%+v output=%s", err, staleConflict, output)
	}
	assertReviewsResolveHomeUnchanged(t, before, homePath)
}

func TestReviewsResolveWrongAreaWritesNeitherSidecarNorReceipt(t *testing.T) {
	t.Parallel()
	homePath := initReviewsResolveCLIHome(t)
	revision := reviewResolveRevisionFor(t, homePath, "parity", "parity_unknown_fixture")
	before := snapshotReviewsResolveHome(t, homePath)
	code, output := runReviewsResolveCLI(t, homePath,
		"resolve", "--item", "parity_unknown_fixture", "--area", "route-promotions",
		"--expected-revision", revision, "--decision", "dismissed",
		"--reason", "the supplied area is deliberately wrong", "--json",
	)
	if code == 0 || !strings.Contains(output, `unknown review item "parity_unknown_fixture" in area "route-promotions"`) {
		t.Fatalf("wrong area exit=%d output=%s", code, output)
	}
	assertReviewsResolveHomeUnchanged(t, before, homePath)
}

func TestReviewsResolveFreshOwnerActionWritesNeitherSidecarNorReceipt(t *testing.T) {
	t.Parallel()
	homePath := initReviewsResolveCLIHome(t)
	revision := reviewResolveRevisionFor(t, homePath, "route-promotions", "route_promotion_resolve_fixture")
	before := snapshotReviewsResolveHome(t, homePath)
	code, output := runReviewsResolveCLI(t, homePath,
		"resolve", "--item", "route_promotion_resolve_fixture", "--area", "route-promotions",
		"--expected-revision", revision, "--decision", "dismissed",
		"--reason", "a synthetic disposition must not hide the owner action", "--json",
	)
	if code == 0 || !strings.Contains(output, "now has an owner action") {
		t.Fatalf("fresh owner action exit=%d output=%s", code, output)
	}
	var ownerConflict reviewsResolveErrorResult
	if err := json.Unmarshal([]byte(output), &ownerConflict); err != nil || ownerConflict.ErrorCode != "owner_action_present" {
		t.Fatalf("owner action missing structured conflict code: err=%v result=%+v output=%s", err, ownerConflict, output)
	}
	assertReviewsResolveHomeUnchanged(t, before, homePath)
}

func TestReviewResolutionGroupHasActionRejectsMatchingGroupOnlyAction(t *testing.T) {
	t.Parallel()
	item := reviewartifact.QueueItem{Area: "parity", ID: "parity_group_member"}
	groups := []reviewartifact.QueueGroup{{
		Area:            "parity",
		ItemIDs:         []string{"parity_group_member"},
		SafeBatchAction: &reviewartifact.Action{ID: "group_owner_action"},
	}}
	if !reviewResolutionGroupHasAction(groups, item) {
		t.Fatal("matching group-only safe_batch_action was not detected")
	}
	if reviewResolutionGroupHasAction(groups, reviewartifact.QueueItem{Area: "parity", ID: "other"}) {
		t.Fatal("group action matched an unrelated item")
	}
	if reviewResolutionGroupHasAction(groups, reviewartifact.QueueItem{Area: "source", ID: item.ID}) {
		t.Fatal("group action matched an unrelated area")
	}
}

func reviewResolveRevisionFor(t *testing.T, homePath, area, itemID string) string {
	t.Helper()
	report, err := buildReviewQueueReport(homePath, area, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range report.Items {
		if item.ID == itemID {
			if item.ReviewResolutionRevision == "" {
				t.Fatalf("review item %s/%s has no owner revision", area, itemID)
			}
			return item.ReviewResolutionRevision
		}
	}
	t.Fatalf("review item %s/%s not found", area, itemID)
	return ""
}

func assertReviewsResolveHomeUnchanged(t *testing.T, before map[string]string, homePath string) {
	t.Helper()
	after := snapshotReviewsResolveHome(t, homePath)
	added, changed, removed := snapshotDelta(before, after)
	if len(added) != 0 || len(changed) != 0 || len(removed) != 0 {
		t.Fatalf("rejected resolve mutated home: added=%v changed=%v removed=%v", added, changed, removed)
	}
}

func TestHomeArchiveReportPrintsKeptReason(t *testing.T) {
	out := captureStdout(t, func() {
		printHomeOperationalArchiveReport("archive-history", home.OperationalArchiveReport{
			SchemaVersion: home.OperationalArchiveSchemaVersion,
			Home:          "fixture-home",
			DryRun:        true,
			Counts:        map[string]int{},
			Kept: []home.OperationalArchiveKeptEntry{{
				Kind:         "review",
				Status:       "operator_pending",
				OriginalPath: "reviews/panel-verdicts/panel_verdict_fixture.json",
				KeptReason:   "operator_pending_without_reviewed_by",
			}},
		})
	})
	if !strings.Contains(out, "kept_reason=operator_pending_without_reviewed_by") {
		t.Fatalf("archive report output missing kept_reason: %s", out)
	}
}
