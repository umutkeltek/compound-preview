package product

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/mem"
	"compound/internal/reviewartifact"

	"gopkg.in/yaml.v3"
)

const (
	OutboundDryRunEvidencePlanSchemaVersion         = "compound.product_outbound_dry_run_evidence_plan.v0.1"
	OutboundDryRunEvidenceReviewSchemaVersion       = "compound.product_outbound_dry_run_evidence_review.v0.1"
	OutboundDryRunEvidenceReviewReportSchemaVersion = "compound.product_outbound_dry_run_evidence_review_report.v0.1"
	OutboundDryRunEvidenceReviewArea                = "product-outbound-dry-runs"
)

type OutboundDryRunEvidencePlan struct {
	SchemaVersion     string                                    `json:"schema_version"`
	ID                string                                    `json:"id"`
	CreatedAt         string                                    `json:"created_at"`
	Status            string                                    `json:"status"`
	ProductID         string                                    `json:"product_id"`
	ReceiptID         string                                    `json:"receipt_id"`
	PlanID            string                                    `json:"plan_id"`
	ReceiptPath       string                                    `json:"receipt_path"`
	ReviewSourcePath  string                                    `json:"review_source_path"`
	AdapterID         string                                    `json:"adapter_id,omitempty"`
	ChangeEvent       string                                    `json:"change_event"`
	Hypothesis        string                                    `json:"hypothesis"`
	ObservationWindow string                                    `json:"observation_window"`
	NextBoundedMove   string                                    `json:"next_bounded_move"`
	Artifacts         []capgraph.OutboundDistributionDryRunItem `json:"artifacts,omitempty"`
	EvidenceRefs      []string                                  `json:"evidence_refs"`
	TargetPath        string                                    `json:"target_path,omitempty"`
	TargetExists      bool                                      `json:"target_exists"`
	TargetHash        string                                    `json:"target_hash,omitempty"`
	ReviewDraft       OutboundDryRunEvidencePlanDraft           `json:"review_draft"`
	Warnings          []string                                  `json:"warnings,omitempty"`
}

type OutboundDryRunEvidencePlanDraft struct {
	ReviewID         string   `json:"review_id"`
	Title            string   `json:"title"`
	ReviewQuestion   string   `json:"review_question"`
	AllowedDecisions []string `json:"allowed_decisions"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewCommand    string   `json:"review_command"`
	ReviewNote       string   `json:"review_note"`
}

type OutboundDryRunEvidencePlanPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

type OutboundDryRunEvidenceReview struct {
	SchemaVersion     string   `json:"schema_version" yaml:"schema_version"`
	ID                string   `json:"id" yaml:"id"`
	ProductID         string   `json:"product_id" yaml:"product_id"`
	ReceiptID         string   `json:"receipt_id" yaml:"receipt_id"`
	Decision          string   `json:"decision" yaml:"decision"`
	Rationale         string   `json:"rationale" yaml:"rationale"`
	Confidence        float64  `json:"confidence" yaml:"confidence"`
	ExpectedEffect    string   `json:"expected_effect,omitempty" yaml:"expected_effect,omitempty"`
	PrimaryMetrics    []string `json:"primary_metrics,omitempty" yaml:"primary_metrics,omitempty"`
	ObservationWindow string   `json:"observation_window,omitempty" yaml:"observation_window,omitempty"`
	NextAction        string   `json:"next_action,omitempty" yaml:"next_action,omitempty"`
	EvidenceRefs      []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt        string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
	AppliedAt         string   `json:"applied_at,omitempty" yaml:"applied_at,omitempty"`
}

type OutboundDryRunEvidenceReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	ProductID     string   `json:"product_id"`
	ReceiptID     string   `json:"receipt_id"`
	ReviewID      string   `json:"review_id"`
	Decision      string   `json:"decision"`
	EvidenceID    string   `json:"evidence_id"`
	EvidenceRefs  []string `json:"evidence_refs"`
	HypothesisID  string   `json:"hypothesis_id,omitempty"`
	TransitionID  string   `json:"transition_id"`
	ReviewPath    string   `json:"review_path"`
	Warnings      []string `json:"warnings,omitempty"`
}

func BuildOutboundDryRunEvidencePlan(homePath, reviewedReceiptPath, targetPath string, now time.Time) (OutboundDryRunEvidencePlan, error) {
	homePath = strings.TrimSpace(homePath)
	reviewedReceiptPath = strings.TrimSpace(reviewedReceiptPath)
	if homePath == "" {
		return OutboundDryRunEvidencePlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if reviewedReceiptPath == "" {
		return OutboundDryRunEvidencePlan{}, fmt.Errorf("reviewed outbound dry-run receipt path required")
	}
	reviewed, err := loadOutboundDryRunCapabilityReviewReport(reviewedReceiptPath)
	if err != nil {
		return OutboundDryRunEvidencePlan{}, err
	}
	if reviewed.Status != "recorded" || reviewed.Decision != "accept_as_product_loop_evidence" {
		return OutboundDryRunEvidencePlan{}, fmt.Errorf("outbound dry-run review must be recorded accept_as_product_loop_evidence, got status=%q decision=%q", reviewed.Status, reviewed.Decision)
	}
	receiptPath, receipt, err := findOutboundDryRunCapabilityReceipt(homePath, reviewedReceiptPath, reviewed)
	if err != nil {
		return OutboundDryRunEvidencePlan{}, err
	}
	if receipt.SchemaVersion != capgraph.OutboundDistributionDryRunReceiptReportSchemaVersion {
		return OutboundDryRunEvidencePlan{}, fmt.Errorf("unexpected outbound dry-run receipt schema %q", receipt.SchemaVersion)
	}
	if receipt.ID != reviewed.ReceiptID || receipt.ProductID != reviewed.ProductID || receipt.PlanID != reviewed.PlanID {
		return OutboundDryRunEvidencePlan{}, fmt.Errorf("outbound dry-run receipt/review mismatch")
	}
	if receipt.Validation.PublicSideEffects || receipt.Validation.AccountAuthUsed || receipt.Validation.ExternalRequestsMade || receipt.Validation.Posted || receipt.Validation.Replied || receipt.Validation.SpentMoney {
		return OutboundDryRunEvidencePlan{}, fmt.Errorf("outbound dry-run receipt has public/auth/external side effects")
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(targetPath)
	if err != nil {
		return OutboundDryRunEvidencePlan{}, err
	}
	evidenceRefs := reviewartifact.UniqueSorted(compact(append(append(append([]string{}, reviewed.EvidenceRefs...), receipt.EvidenceRefs...), "outbound_dry_run_receipt:"+receipt.ID, "outbound_dry_run_capability_review:"+reviewed.ID, receiptPath, reviewedReceiptPath)))
	id := stableID("podrp_", reviewed.ProductID, reviewed.ReceiptID, reviewed.ID)
	return OutboundDryRunEvidencePlan{
		SchemaVersion:     OutboundDryRunEvidencePlanSchemaVersion,
		ID:                id,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		Status:            "review_required",
		ProductID:         reviewed.ProductID,
		ReceiptID:         reviewed.ReceiptID,
		PlanID:            reviewed.PlanID,
		ReceiptPath:       receiptPath,
		ReviewSourcePath:  reviewedReceiptPath,
		AdapterID:         receipt.AdapterID,
		ChangeEvent:       receipt.ProductLoop.ChangeEvent,
		Hypothesis:        receipt.ProductLoop.Hypothesis,
		ObservationWindow: receipt.ProductLoop.ObservationWindow,
		NextBoundedMove:   receipt.ProductLoop.NextBoundedMove,
		Artifacts:         receipt.Artifacts,
		EvidenceRefs:      evidenceRefs,
		TargetPath:        strings.TrimSpace(targetPath),
		TargetExists:      targetExists,
		TargetHash:        targetHash,
		ReviewDraft: OutboundDryRunEvidencePlanDraft{
			ReviewID:         "outbound_dry_run_review_" + stableID("", reviewed.ProductID, reviewed.ReceiptID, reviewed.ID),
			Title:            "Review outbound dry-run evidence for " + reviewed.ProductID,
			ReviewQuestion:   "Should this reviewed dry-run become product-loop hypothesis material?",
			AllowedDecisions: []string{"create_hypothesis_candidate", "park", "reject"},
			RequiredEvidence: []string{
				"capability dry-run review is recorded and accepted as product-loop evidence",
				"receipt validation shows dry_run with no auth, public posting, external requests, replies, or spend",
				"reviewed artifacts are local/synthetic drafts, not public distribution",
				"expected effect and primary metric are product-owned interpretations, not capability truth",
			},
			ReviewCommand: "compound product outbound-dry-run-review <review.yaml>",
			ReviewNote:    "Create a hypothesis candidate only after product review. This bridge never mutates ProductSignal or performs public distribution.",
		},
		Warnings: []string{
			"does not mutate ProductSignal ledger",
			"does not publish or authenticate social accounts",
			"does not declare causality or promote memory truth",
		},
	}, nil
}

func RecordOutboundDryRunEvidencePlan(homePath string, plan OutboundDryRunEvidencePlan) (OutboundDryRunEvidencePlanPaths, error) {
	if strings.TrimSpace(homePath) == "" {
		return OutboundDryRunEvidencePlanPaths{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(plan.ID) == "" {
		return OutboundDryRunEvidencePlanPaths{}, fmt.Errorf("outbound dry-run evidence plan id required")
	}
	dir, err := reviewartifact.Dir(homePath, OutboundDryRunEvidenceReviewArea)
	if err != nil {
		return OutboundDryRunEvidencePlanPaths{}, err
	}
	paths := OutboundDryRunEvidencePlanPaths{
		PlanPath:       filepath.Join(dir, plan.ID+".json"),
		ReviewPath:     filepath.Join(dir, plan.ID+".md"),
		ReviewYAMLPath: filepath.Join(dir, plan.ID+".review.yaml"),
	}
	if _, err := reviewartifact.Write(homePath, OutboundDryRunEvidenceReviewArea, plan.ID, plan, " ", renderOutboundDryRunEvidencePlanReview(plan)); err != nil {
		return OutboundDryRunEvidencePlanPaths{}, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, OutboundDryRunEvidenceReviewArea, plan.ID+".review.yaml", []byte(RenderOutboundDryRunEvidenceReviewYAML(plan))); err != nil {
		return OutboundDryRunEvidencePlanPaths{}, err
	}
	return paths, nil
}

func RenderOutboundDryRunEvidenceReviewYAML(plan OutboundDryRunEvidencePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", OutboundDryRunEvidenceReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "product_id: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "receipt_id: %s\n", plan.ReceiptID)
	fmt.Fprintln(&b, "decision: TODO_decision_create_hypothesis_candidate_park_reject")
	fmt.Fprintln(&b, "rationale: TODO_rationale")
	fmt.Fprintln(&b, "confidence: TODO_confidence_0_to_1")
	fmt.Fprintln(&b, "expected_effect: TODO_expected_effect")
	fmt.Fprintln(&b, "primary_metrics:")
	fmt.Fprintln(&b, " - TODO_primary_metric")
	fmt.Fprintf(&b, "observation_window: %s\n", firstNonEmpty(plan.ObservationWindow, "review window required"))
	fmt.Fprintf(&b, "next_action: %s\n", firstNonEmpty(plan.NextBoundedMove, "review next bounded move"))
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, " - %q\n", ref)
	}
	return b.String()
}

func LoadOutboundDryRunEvidenceReview(path string) (OutboundDryRunEvidenceReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OutboundDryRunEvidenceReview{}, err
	}
	var review OutboundDryRunEvidenceReview
	if err := yaml.Unmarshal(data, &review); err != nil {
		return OutboundDryRunEvidenceReview{}, err
	}
	return review, nil
}

func ApplyOutboundDryRunEvidenceReview(homePath string, store *mem.Store, review OutboundDryRunEvidenceReview, sourcePath string, now time.Time) (OutboundDryRunEvidenceReviewReport, error) {
	if err := ValidateOutboundDryRunEvidenceReview(review); err != nil {
		return OutboundDryRunEvidenceReviewReport{Status: "fail", ProductID: review.ProductID, ReceiptID: review.ReceiptID, ReviewID: review.ID}, err
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = now.UTC().Format(time.RFC3339)
	}
	review.AppliedAt = now.UTC().Format(time.RFC3339)
	plan, err := loadOutboundDryRunEvidencePlanForReview(homePath, sourcePath, review)
	if err != nil {
		return OutboundDryRunEvidenceReviewReport{}, err
	}
	if plan.ProductID != review.ProductID || plan.ReceiptID != review.ReceiptID {
		return OutboundDryRunEvidenceReviewReport{}, fmt.Errorf("outbound dry-run product plan/review mismatch")
	}
	evidence, err := store.RecordEvidence(mem.EvidenceItem{
		SourceKind: "product_outbound_dry_run_review",
		SourceID:   filepath.Base(sourcePath) + ":" + review.ID,
		QuoteText:  fmt.Sprintf("%s reviewed outbound dry-run receipt %s as %s: %s", review.ProductID, review.ReceiptID, review.Decision, review.Rationale),
		Actor:      "tool",
	}, now)
	if err != nil {
		return OutboundDryRunEvidenceReviewReport{}, err
	}
	evidenceRefs := reviewartifact.UniqueSorted(compact(append(append(append([]string{}, plan.EvidenceRefs...), review.EvidenceRefs...), evidence.ID, "outbound_dry_run_review:"+review.ID, "outbound_dry_run_receipt:"+review.ReceiptID)))
	var hypothesisID string
	afterRefs := append([]string{evidence.ID, "outbound_dry_run_review:" + review.ID}, evidenceRefs...)
	if review.Decision == "create_hypothesis_candidate" {
		hypothesisID = stableID("hyp_", review.ProductID, review.ReceiptID, review.ID, plan.ChangeEvent, review.ExpectedEffect)
		candidate := hypothesisCandidate(Hypothesis{
			ID:                hypothesisID,
			ProductID:         review.ProductID,
			ChangeEvent:       plan.ChangeEvent,
			ExpectedEffect:    review.ExpectedEffect,
			Metrics:           compact(review.PrimaryMetrics),
			ObservationWindow: firstNonEmpty(review.ObservationWindow, plan.ObservationWindow),
			Result:            "unknown",
			NextAction:        firstNonEmpty(review.NextAction, plan.NextBoundedMove),
			EvidenceRefs:      evidenceRefs,
		}, now)
		candidate.Verdict = candidates.Park
		if err := writeHypothesisCandidate(homePath, candidate); err != nil {
			return OutboundDryRunEvidenceReviewReport{}, err
		}
		afterRefs = append(afterRefs, "product_hypothesis:"+hypothesisID)
	}
	tr, err := BuildLoopTransition(LoopTransitionInput{
		Kind:         "outbound_dry_run_review",
		ProductID:    review.ProductID,
		ObjectID:     review.ID,
		SourceTrace:  "product_outbound_dry_run_review:" + review.ID,
		BeforeRefs:   plan.EvidenceRefs,
		AfterRefs:    afterRefs,
		EvidenceRefs: evidenceRefs,
		Summary:      fmt.Sprintf("product outbound dry-run review %s recorded as %s; no ProductSignal or public surface changed", review.ID, review.Decision),
	}, now)
	if err != nil {
		return OutboundDryRunEvidenceReviewReport{}, err
	}
	if err := RecordTransition(homePath, tr); err != nil {
		return OutboundDryRunEvidenceReviewReport{}, err
	}
	report := OutboundDryRunEvidenceReviewReport{
		SchemaVersion: OutboundDryRunEvidenceReviewReportSchemaVersion,
		ID:            reviewartifact.ID("podrr_", review.ID, evidence.ID, tr.ID),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        "recorded",
		ProductID:     review.ProductID,
		ReceiptID:     review.ReceiptID,
		ReviewID:      review.ID,
		Decision:      review.Decision,
		EvidenceID:    evidence.ID,
		EvidenceRefs:  evidenceRefs,
		HypothesisID:  hypothesisID,
		TransitionID:  tr.ID,
		ReviewPath:    strings.TrimSpace(sourcePath),
		Warnings: []string{
			"does not mutate ProductSignal ledger",
			"does not publish or authenticate social accounts",
			"does not declare causality",
		},
	}
	if err := recordOutboundDryRunEvidenceReviewResult(homePath, sourcePath, plan, report); err != nil {
		return OutboundDryRunEvidenceReviewReport{}, err
	}
	return report, nil
}

func ValidateOutboundDryRunEvidenceReview(review OutboundDryRunEvidenceReview) error {
	if review.SchemaVersion != OutboundDryRunEvidenceReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", OutboundDryRunEvidenceReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.ProductID) == "" || strings.TrimSpace(review.ReceiptID) == "" {
		return fmt.Errorf("outbound dry-run evidence review id product_id receipt_id required")
	}
	if strings.ContainsAny(review.ReceiptID, `/\`) {
		return fmt.Errorf("outbound dry-run evidence review receipt_id %q must not contain path separators", review.ReceiptID)
	}
	if !oneOf(review.Decision, []string{"create_hypothesis_candidate", "park", "reject"}) {
		return fmt.Errorf("outbound dry-run evidence review decision must be create_hypothesis_candidate|park|reject, got %q", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" {
		return fmt.Errorf("outbound dry-run evidence review rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("outbound dry-run evidence review confidence must be between 0 and 1")
	}
	if len(compact(review.EvidenceRefs)) == 0 {
		return fmt.Errorf("outbound dry-run evidence review evidence_refs required")
	}
	if review.Decision == "create_hypothesis_candidate" {
		if strings.TrimSpace(review.ExpectedEffect) == "" {
			return fmt.Errorf("outbound dry-run evidence review expected_effect required for hypothesis candidate")
		}
		if len(compact(review.PrimaryMetrics)) == 0 {
			return fmt.Errorf("outbound dry-run evidence review primary_metrics required for hypothesis candidate")
		}
	}
	return nil
}

func AnnotateOutboundDryRunEvidenceQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != OutboundDryRunEvidenceReviewArea {
			continue
		}
		annotateOutboundDryRunEvidenceItem(item)
	}
}

func annotateOutboundDryRunEvidenceItem(item *reviewartifact.QueueItem) {
	if record, ok := outboundDryRunEvidenceReviewedFromExtras(item.ExtraPaths); ok {
		item.Status = record.Status
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "product outbound dry-run evidence review recorded"
		item.Actions = nil
		item.DecisionCard = nil
		return
	}
	plan, ok := outboundDryRunEvidencePlanFromQueueItem(*item)
	if !ok {
		item.Readiness = "blocked"
		item.BlockedReason = "product outbound dry-run evidence plan unreadable"
		item.NextAction = "regenerate product outbound dry-run evidence plan"
		item.Actions = nil
		return
	}
	reviewPath := outboundDryRunEvidenceReviewYAMLPath(*item)
	if reviewPath == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft missing"
		item.NextAction = "regenerate product outbound dry-run evidence plan"
		item.Actions = nil
		return
	}
	item.Readiness = "needs_review"
	item.BlockedReason = "product owner must decide whether accepted dry-run evidence creates a hypothesis candidate"
	item.NextAction = "edit review YAML then record product outbound dry-run evidence review"
	card := outboundDryRunEvidenceDecisionCard(plan, reviewPath)
	item.DecisionCard = card
	item.Actions = []reviewartifact.Action{{
		ID:             "record_product_outbound_dry_run_review",
		Label:          "Record product outbound dry-run review",
		Command:        shellJoin([]string{"compound", "product", "outbound-dry-run-review", reviewPath}),
		Argv:           []string{"compound", "product", "outbound-dry-run-review", reviewPath},
		RequiresReview: true,
	}}
}

func renderOutboundDryRunEvidencePlanReview(plan OutboundDryRunEvidencePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Product Outbound Dry-Run Evidence Plan: %s\n\n", plan.ReceiptID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- product: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "- receipt: %s\n", plan.ReceiptID)
	fmt.Fprintf(&b, "- capability_review: %s\n", plan.ReviewSourcePath)
	fmt.Fprintf(&b, "- review_command: `%s`\n", plan.ReviewDraft.ReviewCommand)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Product Loop Basis")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- change_event: %s\n", plan.ChangeEvent)
	fmt.Fprintf(&b, "- capability_hypothesis: %s\n", plan.Hypothesis)
	fmt.Fprintf(&b, "- observation_window: %s\n", plan.ObservationWindow)
	fmt.Fprintf(&b, "- next_bounded_move: %s\n", plan.NextBoundedMove)
	fmt.Fprintf(&b, "- artifacts: %d\n", len(plan.Artifacts))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Required Evidence Before Review")
	fmt.Fprintln(&b)
	for _, evidence := range plan.ReviewDraft.RequiredEvidence {
		fmt.Fprintf(&b, "- %s\n", evidence)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Boundaries")
	fmt.Fprintln(&b)
	for _, warning := range plan.Warnings {
		fmt.Fprintf(&b, "- %s\n", warning)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Review YAML")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```yaml")
	fmt.Fprint(&b, RenderOutboundDryRunEvidenceReviewYAML(plan))
	fmt.Fprintln(&b, "```")
	return b.String()
}

func outboundDryRunEvidenceDecisionCard(plan OutboundDryRunEvidencePlan, reviewPath string) *reviewartifact.DecisionCard {
	argv := []string{"compound", "product", "outbound-dry-run-review", reviewPath}
	return &reviewartifact.DecisionCard{
		Claim:               "Review accepted dry-run evidence for " + plan.ProductID,
		OptionalRationale:   false,
		PanelLean:           "create hypothesis only if product-owned expected effect and metric are clear",
		CanonicalActionHint: "Edit review YAML decision/rationale/confidence, then run `compound product outbound-dry-run-review <review.yaml>`. This may create a parked product hypothesis candidate; it never mutates ProductSignal or posts.",
		Boundary:            plan.Warnings,
		Meta:                map[string]string{"review_yaml": reviewPath, "receipt_id": plan.ReceiptID},
		Why: []reviewartifact.DecisionCardReason{
			{Label: "capability evidence", Question: "What was accepted?", Detail: plan.ReceiptID},
			{Label: "product loop", Question: "What product loop would consume it?", Detail: plan.ChangeEvent + " / " + plan.NextBoundedMove},
			{Label: "artifacts", Question: "What reviewable local artifacts exist?", Detail: fmt.Sprintf("%d artifact(s)", len(plan.Artifacts))},
			{Label: "boundary", Severity: "warning", Question: "What must stay outside this bridge?", Detail: strings.Join(plan.Warnings, "; ")},
		},
		Choices: []reviewartifact.DecisionCardChoice{
			{ID: "create_hypothesis_candidate", Label: "Create Candidate", Effect: "Creates a parked product hypothesis candidate from reviewed dry-run evidence.", Command: shellJoin(argv), Argv: argv, RequiresReview: true},
			{ID: "park", Label: "Park", Effect: "Records review evidence without creating a hypothesis candidate.", Command: shellJoin(argv), Argv: argv, RequiresReview: true},
			{ID: "reject", Label: "Reject", Effect: "Records that the dry-run evidence should not feed product-loop work.", Command: shellJoin(argv), Argv: argv, RequiresReview: true},
		},
	}
}

func loadOutboundDryRunCapabilityReviewReport(path string) (capgraph.OutboundDistributionDryRunReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return capgraph.OutboundDistributionDryRunReviewReport{}, err
	}
	var report capgraph.OutboundDistributionDryRunReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return capgraph.OutboundDistributionDryRunReviewReport{}, err
	}
	if report.SchemaVersion != capgraph.OutboundDistributionDryRunReviewReportSchemaVersion {
		return report, fmt.Errorf("unexpected outbound dry-run review report schema %q", report.SchemaVersion)
	}
	if strings.TrimSpace(report.ID) == "" || strings.TrimSpace(report.ReceiptID) == "" || strings.TrimSpace(report.ProductID) == "" {
		return report, fmt.Errorf("outbound dry-run review report id receipt_id product_id required")
	}
	return report, nil
}

func findOutboundDryRunCapabilityReceipt(homePath, reviewedPath string, reviewed capgraph.OutboundDistributionDryRunReviewReport) (string, capgraph.OutboundDistributionDryRunReceiptReport, error) {
	candidates := []string{
		filepath.Join(filepath.Dir(reviewedPath), reviewed.ReceiptID+".json"),
		filepath.Join(homePath, "reviews", capgraph.OutboundDistributionDryRunReviewArea, reviewed.ReceiptID+".json"),
	}
	if reviewed.ReviewedPath != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(reviewed.ReviewedPath), reviewed.ReceiptID+".json"))
	}
	for _, path := range reviewartifact.Unique(candidates) {
		receipt, err := loadOutboundDryRunCapabilityReceipt(path)
		if err == nil {
			return path, receipt, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", capgraph.OutboundDistributionDryRunReceiptReport{}, err
		}
	}
	return "", capgraph.OutboundDistributionDryRunReceiptReport{}, fmt.Errorf("outbound dry-run receipt %q not found", reviewed.ReceiptID)
}

func loadOutboundDryRunCapabilityReceipt(path string) (capgraph.OutboundDistributionDryRunReceiptReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return capgraph.OutboundDistributionDryRunReceiptReport{}, err
	}
	var receipt capgraph.OutboundDistributionDryRunReceiptReport
	if err := json.Unmarshal(data, &receipt); err != nil {
		return capgraph.OutboundDistributionDryRunReceiptReport{}, err
	}
	return receipt, nil
}

func loadOutboundDryRunEvidencePlanForReview(homePath, sourcePath string, review OutboundDryRunEvidenceReview) (OutboundDryRunEvidencePlan, error) {
	stem := reviewArtifactStem(sourcePath)
	if stem != "" {
		plan, err := loadOutboundDryRunEvidencePlan(homePath, stem)
		if err == nil {
			return plan, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return OutboundDryRunEvidencePlan{}, err
		}
	}
	return loadOutboundDryRunEvidencePlanByReview(homePath, review)
}

func loadOutboundDryRunEvidencePlan(homePath, stem string) (OutboundDryRunEvidencePlan, error) {
	path := filepath.Join(homePath, "reviews", OutboundDryRunEvidenceReviewArea, stem+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return OutboundDryRunEvidencePlan{}, err
	}
	var plan OutboundDryRunEvidencePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return OutboundDryRunEvidencePlan{}, err
	}
	if strings.TrimSpace(plan.ID) == "" {
		return OutboundDryRunEvidencePlan{}, fmt.Errorf("product outbound dry-run evidence plan %s missing id", path)
	}
	return plan, nil
}

func loadOutboundDryRunEvidencePlanByReview(homePath string, review OutboundDryRunEvidenceReview) (OutboundDryRunEvidencePlan, error) {
	root := filepath.Join(homePath, "reviews", OutboundDryRunEvidenceReviewArea)
	var found OutboundDryRunEvidencePlan
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" || strings.Contains(filepath.Base(path), ".reviewed.") {
			return nil
		}
		plan, err := loadOutboundDryRunEvidencePlanByPath(path)
		if err != nil {
			return nil
		}
		if plan.ReviewDraft.ReviewID == review.ID && plan.ProductID == review.ProductID && plan.ReceiptID == review.ReceiptID {
			found = plan
			return filepath.SkipAll
		}
		return nil
	})
	if os.IsNotExist(err) {
		return OutboundDryRunEvidencePlan{}, fmt.Errorf("product outbound dry-run evidence plan for review %q not found", review.ID)
	}
	if err != nil {
		return OutboundDryRunEvidencePlan{}, err
	}
	if found.ID == "" {
		return OutboundDryRunEvidencePlan{}, fmt.Errorf("product outbound dry-run evidence plan for review %q not found", review.ID)
	}
	return found, nil
}

func loadOutboundDryRunEvidencePlanByPath(path string) (OutboundDryRunEvidencePlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OutboundDryRunEvidencePlan{}, err
	}
	var plan OutboundDryRunEvidencePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return OutboundDryRunEvidencePlan{}, err
	}
	return plan, nil
}

func recordOutboundDryRunEvidenceReviewResult(homePath, sourcePath string, plan OutboundDryRunEvidencePlan, report OutboundDryRunEvidenceReviewReport) error {
	stem := reviewArtifactStem(sourcePath)
	if stem == "" {
		stem = plan.ID
	}
	if _, err := reviewartifact.WriteJSON(homePath, OutboundDryRunEvidenceReviewArea, stem+"."+report.ID+".reviewed.json", report, " "); err != nil {
		return err
	}
	plan.Status = "recorded"
	path := filepath.Join(homePath, "reviews", OutboundDryRunEvidenceReviewArea, stem+".json")
	data, err := json.MarshalIndent(plan, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func outboundDryRunEvidencePlanFromQueueItem(item reviewartifact.QueueItem) (OutboundDryRunEvidencePlan, bool) {
	plan, err := loadOutboundDryRunEvidencePlanByPath(item.JSONPath)
	if err != nil {
		return OutboundDryRunEvidencePlan{}, false
	}
	return plan, plan.ID != ""
}

func outboundDryRunEvidenceReviewedFromExtras(paths []string) (OutboundDryRunEvidenceReviewReport, bool) {
	var found OutboundDryRunEvidenceReviewReport
	for _, path := range paths {
		if !strings.HasSuffix(path, ".reviewed.json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record OutboundDryRunEvidenceReviewReport
		if err := json.Unmarshal(data, &record); err != nil {
			continue
		}
		if strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.Status) == "" {
			continue
		}
		if found.ID == "" || record.CreatedAt > found.CreatedAt {
			found = record
		}
	}
	return found, found.ID != ""
}

func outboundDryRunEvidenceReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			return path
		}
	}
	return ""
}
