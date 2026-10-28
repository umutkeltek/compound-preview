package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"compound/internal/home"
	"compound/internal/product"
	"compound/internal/reviewartifact"
)

// recordQueueDraftCLI records one draft through the CLI door and returns its id.
func recordQueueDraftCLI(t *testing.T, body string) string {
	t.Helper()
	out := captureStdout(t, func() {
		cmdProduct([]string{
			"content-draft", "record",
			"--product", "product:prod_synth_a",
			"--platform", "x",
			"--text", body,
			"--voice-pack", "voice:prod_synth_a_v1",
			"--evidence", "finding:synth_a",
			"--ttl", "72h",
		})
	})
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "- id:"))
		}
	}
	t.Fatalf("could not parse draft id from record output:\n%s", out)
	return ""
}

// End-to-end through the CLI: plan -> hand-edit per-draft verdicts in the YAML ->
// apply. Proves the batch surfaces through the canonical review-queue builder, that
// approve mints a governed post pair and operationalizes, reject retains, and hold
// is inert. RED if any of the verbs are unwired, if the reviewqueue annotation is
// missing, or if approve fails to mint / operationalize.
func TestProductContentQueuePlanEditApplyCLI(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}

	approveID := recordQueueDraftCLI(t, "approve-me: launch thread for prod_synth_a")
	rejectID := recordQueueDraftCLI(t, "reject-me: off-voice draft")
	holdID := recordQueueDraftCLI(t, "hold-me: undecided draft")

	// plan renders the batch + writes the review skeleton.
	planOut := captureStdout(t, func() {
		cmdProduct([]string{"content-queue", "plan"})
	})
	if !strings.Contains(planOut, "# product content-queue plan") ||
		!strings.Contains(planOut, "- status: review_required") ||
		!strings.Contains(planOut, "- reviewable: 3") {
		t.Fatalf("content-queue plan output missing proof:\n%s", planOut)
	}
	reviewPaths, err := filepath.Glob(filepath.Join(homePath, "reviews", product.ContentQueueReviewArea, "*.review.yaml"))
	if err != nil || len(reviewPaths) != 1 {
		t.Fatalf("expected exactly one content-queue review YAML, got %v (err %v)", reviewPaths, err)
	}
	reviewPath := reviewPaths[0]

	// The batch surfaces through the CANONICAL review-queue builder (cmdReviews ->
	// buildReviewQueueReport -> reviewqueue.Build -> AnnotateContentQueue).
	reviewsOut := captureStdout(t, func() {
		cmdReviews([]string{"--area", product.ContentQueueReviewArea, "--json"})
	})
	var reviews reviewartifact.QueueReport
	if err := json.Unmarshal([]byte(reviewsOut), &reviews); err != nil {
		t.Fatalf("parse content-queue reviews: %v\n%s", err, reviewsOut)
	}
	if reviews.Counts.ReviewRequired != 1 || reviews.Counts.DecisionCardCount != 1 || reviews.Counts.ActionableCount != 1 {
		t.Fatalf("content-queue batch should be reviewable via the canonical builder: counts=%+v", reviews.Counts)
	}

	// Hand-edit the per-draft verdicts in the YAML.
	raw, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	var review product.ContentQueueReview
	if err := yaml.Unmarshal(raw, &review); err != nil {
		t.Fatalf("parse review YAML: %v\n%s", err, raw)
	}
	for i := range review.Drafts {
		switch review.Drafts[i].DraftID {
		case approveID:
			review.Drafts[i].Verdict = "approve"
		case rejectID:
			review.Drafts[i].Verdict = "reject"
		case holdID:
			review.Drafts[i].Verdict = "hold"
		}
	}
	edited, err := yaml.Marshal(&review)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewPath, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	// apply executes the verdicts.
	applyOut := captureStdout(t, func() {
		cmdProduct([]string{"content-queue", "apply", "--review", reviewPath, "--json"})
	})
	var report product.ContentQueueReviewReport
	if err := json.Unmarshal([]byte(applyOut), &report); err != nil {
		t.Fatalf("parse apply report: %v\n%s", err, applyOut)
	}
	if report.Counts.Approved != 1 || report.Counts.Rejected != 1 || report.Counts.Held != 1 || report.Counts.Errors != 0 {
		t.Fatalf("unexpected apply counts: %+v", report.Counts)
	}

	// The approved row minted the governed pair, findable by draft id.
	var approvedRow *product.ContentQueueRowResult
	for i := range report.Rows {
		if report.Rows[i].DraftID == approveID {
			approvedRow = &report.Rows[i]
		}
	}
	if approvedRow == nil || approvedRow.Outcome != "approved" {
		t.Fatalf("approved row missing/wrong: %+v", report.Rows)
	}
	if !strings.HasPrefix(approvedRow.ActionIntentID, "act_") || !strings.HasPrefix(approvedRow.PolicyDecisionID, "pol_") {
		t.Fatalf("approved row did not mint a governed pair: %+v", approvedRow)
	}
	auth, err := product.LoadContentPostAuthorization(homePath, approveID)
	if err != nil {
		t.Fatalf("S4 load authorization for approved draft: %v", err)
	}
	if auth.ActionIntentID != approvedRow.ActionIntentID || auth.PolicyDecisionID != approvedRow.PolicyDecisionID {
		t.Fatalf("authorization ids %s/%s disagree with report row %s/%s", auth.ActionIntentID, auth.PolicyDecisionID, approvedRow.ActionIntentID, approvedRow.PolicyDecisionID)
	}
	authFiles, _ := filepath.Glob(filepath.Join(homePath, "content-post-authorizations", "*.json"))
	if len(authFiles) != 1 {
		t.Fatalf("want exactly one content-post authorization after one approve, got %v", authFiles)
	}

	// Final draft states: approved->operationalized, rejected->rejected (retained),
	// held->proposed (unchanged).
	listOut := captureStdout(t, func() {
		cmdProduct([]string{"content-draft", "list", "--json"})
	})
	var drafts []product.ContentDraft
	if err := json.Unmarshal([]byte(listOut), &drafts); err != nil {
		t.Fatalf("parse draft list: %v\n%s", err, listOut)
	}
	states := map[string]string{}
	for _, d := range drafts {
		states[d.ID] = d.State
	}
	if states[approveID] != product.ContentDraftStateOperationalized {
		t.Fatalf("approved draft state = %q, want operationalized", states[approveID])
	}
	if states[rejectID] != product.ContentDraftStateRejected {
		t.Fatalf("rejected draft state = %q, want rejected (retained)", states[rejectID])
	}
	if states[holdID] != product.ContentDraftStateProposed {
		t.Fatalf("held draft state = %q, want proposed (unchanged)", states[holdID])
	}
}
