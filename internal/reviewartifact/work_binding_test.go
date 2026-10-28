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
)

var workBindingTestNow = time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)

func TestRecordWorkBindingPreservesCoreAndLoadsOneCurrentHead(t *testing.T) {
	homePath, reviewPath, original := workBindingFixture(t, "panel-verdicts", "pver_test")
	in := RecordWorkBindingInput{
		Area:           "panel-verdicts",
		ReviewID:       "pver_test",
		CoreReviewPath: reviewPath,
		SourceWorkID:   "work_source_owner",
		SourceRun:      &ReviewSourceRun{Kind: "panel_run", ID: "prun_test", WorkID: "work_source_owner"},
		FallbackWorkID: "work_panel_area_owner",
		EvidenceRefs:   []string{"run:prun_test", "work:work_source_owner"},
	}
	first, firstPath, err := RecordWorkBinding(homePath, in, workBindingTestNow)
	if err != nil {
		t.Fatal(err)
	}
	again, againPath, err := RecordWorkBinding(homePath, in, workBindingTestNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.CreatedAt != first.CreatedAt || againPath != firstPath {
		t.Fatalf("idempotent record changed binding: first=%+v path=%s again=%+v path=%s", first, firstPath, again, againPath)
	}
	if got, err := os.ReadFile(reviewPath); err != nil || string(got) != string(original) {
		t.Fatalf("recording rewrote sealed core review: err=%v before=%s after=%s", err, original, got)
	}

	current, err := LoadCurrentWorkBinding(homePath, WorkBindingTarget{
		Area: "panel-verdicts", ReviewID: "pver_test", CoreReviewPath: reviewPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.ID != first.ID || current.SourceWorkID != "work_source_owner" ||
		current.FallbackWorkID != "work_panel_area_owner" || current.SourceRun == nil ||
		current.SourceRun.ID != "prun_test" {
		t.Fatalf("current binding = %+v", current)
	}
	sum := sha256.Sum256(original)
	if current.ReviewSHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("review digest = %q", current.ReviewSHA256)
	}

	report, err := Queue(homePath, QueueOptions{Area: "panel-verdicts"}, workBindingTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("binding sidecar became a core review item: %+v", report.Items)
	}
	if report.Items[0].WorkBinding == nil || report.Items[0].WorkBinding.ID != first.ID {
		t.Fatalf("queue did not attach current binding: %+v", report.Items[0])
	}
	if !containsString(report.Items[0].ExtraPaths, firstPath) {
		t.Fatalf("binding sidecar missing from companion paths: %v", report.Items[0].ExtraPaths)
	}
}

func TestRecordWorkBindingRequiresExactSupersessionAndRejectsForks(t *testing.T) {
	homePath, reviewPath, _ := workBindingFixture(t, "decision-candidates", "dcp_test")
	base := RecordWorkBindingInput{
		Area:           "decision-candidates",
		ReviewID:       "dcp_test",
		CoreReviewPath: reviewPath,
		SourceWorkID:   "work_source",
		FallbackWorkID: "work_area",
	}
	first, _, err := RecordWorkBinding(homePath, base, workBindingTestNow)
	if err != nil {
		t.Fatal(err)
	}

	changed := base
	changed.FallbackWorkID = "work_area_next"
	if _, _, err := RecordWorkBinding(homePath, changed, workBindingTestNow.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "supersedes") {
		t.Fatalf("conflicting write without exact supersession = %v", err)
	}
	changed.SupersedesBindingID = first.ID
	second, _, err := RecordWorkBinding(homePath, changed, workBindingTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	current, err := LoadCurrentWorkBinding(homePath, WorkBindingTarget{
		Area: "decision-candidates", ReviewID: "dcp_test", CoreReviewPath: reviewPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.ID != second.ID || current.SupersedesBindingID != first.ID {
		t.Fatalf("current superseding binding = %+v", current)
	}

	fork := second
	fork.FallbackWorkID = "work_fork"
	fork.SupersedesBindingID = first.ID
	fork.CreatedAt = workBindingTestNow.Add(2 * time.Minute).Format(time.RFC3339)
	fork.ID = workBindingID(fork)
	forkPath := workBindingPath(reviewPath, fork.ID)
	data, err := json.MarshalIndent(fork, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(forkPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCurrentWorkBinding(homePath, WorkBindingTarget{
		Area: "decision-candidates", ReviewID: "dcp_test", CoreReviewPath: reviewPath,
	}); err == nil || !strings.Contains(err.Error(), "fork") {
		t.Fatalf("forked binding chain loaded: %v", err)
	}
}

func TestRecordWorkBindingRepairsCoreDigestDriftByExactHeadSupersession(t *testing.T) {
	homePath, reviewPath, _ := workBindingFixture(t, "route-promotions", "route_promotion_test")
	input := RecordWorkBindingInput{
		Area:           "route-promotions",
		ReviewID:       "route_promotion_test",
		CoreReviewPath: reviewPath,
		FallbackWorkID: "work_review_area_route_promotions",
	}
	first, firstPath, err := RecordWorkBinding(homePath, input, workBindingTestNow)
	if err != nil {
		t.Fatal(err)
	}
	historicalBytes, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}

	currentReview := []byte(`{"id":"route_promotion_test","status":"review_required","revision":2}` + "\n")
	if err := os.WriteFile(reviewPath, currentReview, 0o644); err != nil {
		t.Fatal(err)
	}
	target := WorkBindingTarget{
		Area: "route-promotions", ReviewID: "route_promotion_test", CoreReviewPath: reviewPath,
	}
	if _, err := LoadCurrentWorkBinding(homePath, target); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("digest-drifted head loaded as current: %v", err)
	}
	driftedQueue, err := Queue(homePath, QueueOptions{Area: "route-promotions"}, workBindingTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(driftedQueue.Items) != 1 || driftedQueue.Items[0].WorkBinding != nil ||
		!containsSubstring(driftedQueue.Items[0].Warnings, "owner-work binding invalid") {
		t.Fatalf("digest-drifted queue did not fail closed: %+v", driftedQueue.Items)
	}

	head, err := LoadWorkBindingHead(homePath, target)
	if err != nil {
		t.Fatal(err)
	}
	if head == nil || head.ID != first.ID || head.ReviewSHA256 != first.ReviewSHA256 {
		t.Fatalf("structural head = %+v, want historical head %s", head, first.ID)
	}
	if _, _, err := RecordWorkBinding(homePath, input, workBindingTestNow.Add(time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "supersedes_binding_id must equal current head "+first.ID) {
		t.Fatalf("digest-drift repair without exact supersession = %v", err)
	}

	input.SupersedesBindingID = first.ID
	second, _, err := RecordWorkBinding(homePath, input, workBindingTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.SupersedesBindingID != first.ID || second.ReviewSHA256 == first.ReviewSHA256 {
		t.Fatalf("superseding binding = %+v", second)
	}
	sum := sha256.Sum256(currentReview)
	if want := "sha256:" + hex.EncodeToString(sum[:]); second.ReviewSHA256 != want {
		t.Fatalf("new head digest = %q want %q", second.ReviewSHA256, want)
	}
	if after, err := os.ReadFile(firstPath); err != nil || string(after) != string(historicalBytes) {
		t.Fatalf("historical binding changed: err=%v\nbefore=%s\nafter=%s", err, historicalBytes, after)
	}
	current, err := LoadCurrentWorkBinding(homePath, target)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.ID != second.ID {
		t.Fatalf("current repaired binding = %+v", current)
	}
}

func TestLoadCurrentWorkBindingRejectsDigestPathAndSourceRunDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, homePath, reviewPath, bindingPath string)
		want   string
	}{
		{
			name: "core digest drift",
			mutate: func(t *testing.T, _, reviewPath, _ string) {
				t.Helper()
				if err := os.WriteFile(reviewPath, []byte(`{"id":"pver_test","status":"changed"}`+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "digest",
		},
		{
			name: "bound path drift",
			mutate: func(t *testing.T, _, _, bindingPath string) {
				t.Helper()
				rewriteBindingFixture(t, bindingPath, func(binding *ReviewWorkBinding) {
					binding.ReviewPath = "reviews/other-area/other.json"
				})
			},
			want: "path",
		},
		{
			name: "malformed historical digest",
			mutate: func(t *testing.T, _, _, bindingPath string) {
				t.Helper()
				rewriteBindingFixture(t, bindingPath, func(binding *ReviewWorkBinding) {
					binding.ReviewSHA256 = "sha256:not-a-digest"
				})
			},
			want: "review_sha256",
		},
		{
			name: "source run work mismatch",
			mutate: func(t *testing.T, _, _, bindingPath string) {
				t.Helper()
				rewriteBindingFixture(t, bindingPath, func(binding *ReviewWorkBinding) {
					binding.SourceRun.WorkID = "work_different"
				})
			},
			want: "source_run",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath, reviewPath, _ := workBindingFixture(t, "panel-verdicts", "pver_test")
			_, bindingPath, err := RecordWorkBinding(homePath, RecordWorkBindingInput{
				Area:           "panel-verdicts",
				ReviewID:       "pver_test",
				CoreReviewPath: reviewPath,
				SourceWorkID:   "work_source",
				SourceRun:      &ReviewSourceRun{Kind: "panel_run", ID: "prun_test", WorkID: "work_source"},
				FallbackWorkID: "work_area",
			}, workBindingTestNow)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, homePath, reviewPath, bindingPath)
			if _, err := LoadCurrentWorkBinding(homePath, WorkBindingTarget{
				Area: "panel-verdicts", ReviewID: "pver_test", CoreReviewPath: reviewPath,
			}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("drift loaded: %v", err)
			}
		})
	}
}

func TestLoadWorkBindingHeadRejectsMissingParentAndCorruptMember(t *testing.T) {
	t.Run("missing parent", func(t *testing.T) {
		homePath, reviewPath, _ := workBindingFixture(t, "panel-verdicts", "pver_missing_parent")
		_, bindingPath, err := RecordWorkBinding(homePath, RecordWorkBindingInput{
			Area:           "panel-verdicts",
			ReviewID:       "pver_missing_parent",
			CoreReviewPath: reviewPath,
			FallbackWorkID: "work_area",
		}, workBindingTestNow)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(bindingPath)
		if err != nil {
			t.Fatal(err)
		}
		var binding ReviewWorkBinding
		if err := json.Unmarshal(data, &binding); err != nil {
			t.Fatal(err)
		}
		binding.SupersedesBindingID = "rwb_deadbeefdead"
		binding.ID = workBindingID(binding)
		data, err = json.MarshalIndent(binding, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		newPath := workBindingPath(reviewPath, binding.ID)
		if err := os.WriteFile(newPath, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(bindingPath); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadWorkBindingHead(homePath, WorkBindingTarget{
			Area: "panel-verdicts", ReviewID: "pver_missing_parent", CoreReviewPath: reviewPath,
		}); err == nil || !strings.Contains(err.Error(), "supersedes missing binding") {
			t.Fatalf("missing-parent chain loaded: %v", err)
		}
	})

	t.Run("corrupt member", func(t *testing.T) {
		homePath, reviewPath, _ := workBindingFixture(t, "panel-verdicts", "pver_corrupt")
		if _, _, err := RecordWorkBinding(homePath, RecordWorkBindingInput{
			Area:           "panel-verdicts",
			ReviewID:       "pver_corrupt",
			CoreReviewPath: reviewPath,
			FallbackWorkID: "work_area",
		}, workBindingTestNow); err != nil {
			t.Fatal(err)
		}
		corruptPath := workBindingPath(reviewPath, "rwb_corrupt00000")
		if err := os.WriteFile(corruptPath, []byte("{not-json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadWorkBindingHead(homePath, WorkBindingTarget{
			Area: "panel-verdicts", ReviewID: "pver_corrupt", CoreReviewPath: reviewPath,
		}); err == nil || !strings.Contains(err.Error(), "parse review work binding") {
			t.Fatalf("corrupt chain member loaded: %v", err)
		}
	})
}

func workBindingFixture(t *testing.T, area, reviewID string) (string, string, []byte) {
	t.Helper()
	homePath := t.TempDir()
	dir := filepath.Join(homePath, "reviews", area)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"id":"` + reviewID + `","status":"review_required"}` + "\n")
	path := filepath.Join(dir, reviewID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return homePath, path, data
}

func rewriteBindingFixture(t *testing.T, path string, mutate func(*ReviewWorkBinding)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var binding ReviewWorkBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		t.Fatal(err)
	}
	mutate(&binding)
	data, err = json.MarshalIndent(binding, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
