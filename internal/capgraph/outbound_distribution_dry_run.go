package capgraph

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/reviewartifact"
	"gopkg.in/yaml.v3"
)

const (
	OutboundDistributionDryRunReceiptSchemaVersion       = "compound.capability.outbound_distribution_dry_run_receipt.v0.1"
	OutboundDistributionDryRunReceiptReportSchemaVersion = "compound.capability.outbound_distribution_dry_run_receipt_report.v0.1"
	OutboundDistributionDryRunReviewSchemaVersion        = "compound.capability.outbound_distribution_dry_run_review.v0.1"
	OutboundDistributionDryRunReviewReportSchemaVersion  = "compound.capability.outbound_distribution_dry_run_review_report.v0.1"
	OutboundDistributionDryRunReviewArea                 = "outbound-distribution-dry-runs"
	OutboundDistributionDryRunAuthorityRef               = "compound://capability/outbound-distribution-dry-run"
)

type OutboundDistributionDryRunReceiptOptions struct {
	VerdictReviewPath string
	AdapterOutputPath string
	TargetPath        string
}

type OutboundDistributionDryRunAdapterOutput struct {
	SchemaVersion        string                           `json:"schema_version" yaml:"schema_version"`
	ID                   string                           `json:"id" yaml:"id"`
	PlanID               string                           `json:"plan_id" yaml:"plan_id"`
	ProductID            string                           `json:"product_id" yaml:"product_id"`
	AdapterID            string                           `json:"adapter_id" yaml:"adapter_id"`
	ProducedAt           string                           `json:"produced_at,omitempty" yaml:"produced_at,omitempty"`
	Mode                 string                           `json:"mode" yaml:"mode"`
	RunKind              string                           `json:"run_kind" yaml:"run_kind"`
	PublicSideEffects    bool                             `json:"public_side_effects" yaml:"public_side_effects"`
	AccountAuthUsed      bool                             `json:"account_auth_used" yaml:"account_auth_used"`
	ExternalRequestsMade bool                             `json:"external_requests_made" yaml:"external_requests_made"`
	Posted               bool                             `json:"posted" yaml:"posted"`
	Replied              bool                             `json:"replied" yaml:"replied"`
	SpentMoney           bool                             `json:"spent_money" yaml:"spent_money"`
	ProductLoop          OutboundDistributionProductLoop  `json:"product_loop" yaml:"product_loop"`
	Artifacts            []OutboundDistributionDryRunItem `json:"artifacts" yaml:"artifacts"`
	EvidenceRefs         []string                         `json:"evidence_refs" yaml:"evidence_refs"`
}

type OutboundDistributionProductLoop struct {
	ChangeEvent       string `json:"change_event" yaml:"change_event"`
	Hypothesis        string `json:"hypothesis" yaml:"hypothesis"`
	ObservationWindow string `json:"observation_window" yaml:"observation_window"`
	NextBoundedMove   string `json:"next_bounded_move" yaml:"next_bounded_move"`
}

type OutboundDistributionDryRunItem struct {
	Kind        string `json:"kind" yaml:"kind"`
	Channel     string `json:"channel" yaml:"channel"`
	Ref         string `json:"ref" yaml:"ref"`
	ContentHash string `json:"content_hash" yaml:"content_hash"`
}

type OutboundDistributionDryRunValidation struct {
	Mode                 string `json:"mode"`
	RunKind              string `json:"run_kind"`
	PublicSideEffects    bool   `json:"public_side_effects"`
	AccountAuthUsed      bool   `json:"account_auth_used"`
	ExternalRequestsMade bool   `json:"external_requests_made"`
	Posted               bool   `json:"posted"`
	Replied              bool   `json:"replied"`
	SpentMoney           bool   `json:"spent_money"`
	ArtifactCount        int    `json:"artifact_count"`
}

type OutboundDistributionDryRunReceiptReport struct {
	SchemaVersion     string                                `json:"schema_version"`
	ID                string                                `json:"id"`
	CreatedAt         string                                `json:"created_at"`
	Status            string                                `json:"status"`
	AuthorityRef      string                                `json:"authority_ref"`
	PlanID            string                                `json:"plan_id"`
	ProductID         string                                `json:"product_id"`
	VerdictReviewPath string                                `json:"verdict_review_path"`
	AdapterOutputPath string                                `json:"adapter_output_path"`
	AdoptionLevel     string                                `json:"adoption_level"`
	AdapterID         string                                `json:"adapter_id"`
	ProductLoop       OutboundDistributionProductLoop       `json:"product_loop"`
	Artifacts         []OutboundDistributionDryRunItem      `json:"artifacts"`
	Validation        OutboundDistributionDryRunValidation  `json:"validation"`
	EvidenceRefs      []string                              `json:"evidence_refs"`
	Boundaries        []string                              `json:"boundaries"`
	TargetPath        string                                `json:"target_path,omitempty"`
	TargetExists      bool                                  `json:"target_exists"`
	TargetHash        string                                `json:"target_hash,omitempty"`
	ReviewDraft       OutboundDistributionDryRunReviewDraft `json:"review_draft"`
}

type OutboundDistributionDryRunReviewDraft struct {
	ReviewID         string   `json:"review_id"`
	ReviewQuestion   string   `json:"review_question"`
	AllowedDecisions []string `json:"allowed_decisions"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewCommand    string   `json:"review_command"`
}

type OutboundDistributionDryRunReceiptPaths struct {
	ReportPath     string `json:"report_path"`
	ReviewPath     string `json:"review_path"`
	ReviewYAMLPath string `json:"review_yaml_path"`
}

type OutboundDistributionDryRunReview struct {
	SchemaVersion string   `json:"schema_version" yaml:"schema_version"`
	ID            string   `json:"id" yaml:"id"`
	ReceiptID     string   `json:"receipt_id" yaml:"receipt_id"`
	PlanID        string   `json:"plan_id" yaml:"plan_id"`
	ProductID     string   `json:"product_id" yaml:"product_id"`
	Decision      string   `json:"decision" yaml:"decision"`
	Rationale     string   `json:"rationale" yaml:"rationale"`
	Confidence    float64  `json:"confidence" yaml:"confidence"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt    string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
}

type OutboundDistributionDryRunReviewReport struct {
	SchemaVersion    string   `json:"schema_version"`
	ID               string   `json:"id"`
	CreatedAt        string   `json:"created_at"`
	Status           string   `json:"status"`
	ReceiptID        string   `json:"receipt_id"`
	PlanID           string   `json:"plan_id"`
	ProductID        string   `json:"product_id"`
	Decision         string   `json:"decision"`
	ReviewedPath     string   `json:"reviewed_path"`
	ReviewSourcePath string   `json:"review_source_path"`
	EvidenceRefs     []string `json:"evidence_refs"`
}

func BuildOutboundDistributionDryRunReceipt(homePath string, opts OutboundDistributionDryRunReceiptOptions, now time.Time) (OutboundDistributionDryRunReceiptReport, error) {
	opts.VerdictReviewPath = strings.TrimSpace(opts.VerdictReviewPath)
	opts.AdapterOutputPath = strings.TrimSpace(opts.AdapterOutputPath)
	opts.TargetPath = strings.TrimSpace(opts.TargetPath)
	if strings.TrimSpace(homePath) == "" {
		return OutboundDistributionDryRunReceiptReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if opts.VerdictReviewPath == "" {
		return OutboundDistributionDryRunReceiptReport{}, fmt.Errorf("verdict review path required")
	}
	if opts.AdapterOutputPath == "" {
		return OutboundDistributionDryRunReceiptReport{}, fmt.Errorf("adapter output path required")
	}
	verdict, err := LoadOutboundDistributionVerdictReviewReport(opts.VerdictReviewPath)
	if err != nil {
		return OutboundDistributionDryRunReceiptReport{}, err
	}
	if verdict.Decision != "adopt_for_dry_run" {
		return OutboundDistributionDryRunReceiptReport{}, fmt.Errorf("outbound verdict decision %q is not adopt_for_dry_run", verdict.Decision)
	}
	output, err := LoadOutboundDistributionDryRunAdapterOutput(opts.AdapterOutputPath)
	if err != nil {
		return OutboundDistributionDryRunReceiptReport{}, err
	}
	if err := validateOutboundDistributionDryRunOutput(verdict, output); err != nil {
		return OutboundDistributionDryRunReceiptReport{}, err
	}
	targetExists, targetHash, err := reviewartifact.HashFileTarget(opts.TargetPath)
	if err != nil {
		return OutboundDistributionDryRunReceiptReport{}, err
	}
	id := "oddr_" + outboundDistributionStableID("", verdict.PlanID, output.ID, output.AdapterID, output.ProductLoop.NextBoundedMove)
	reviewID := "outbound_distribution_dry_run_review_" + outboundDistributionSlug(output.ProductID) + "_" + outboundDistributionStableID("", id)
	evidenceRefs := reviewartifact.UniqueSorted(append(append([]string{}, verdict.EvidenceRefs...), append(output.EvidenceRefs, verdict.ReviewedPath, opts.AdapterOutputPath)...))
	return OutboundDistributionDryRunReceiptReport{
		SchemaVersion:     OutboundDistributionDryRunReceiptReportSchemaVersion,
		ID:                id,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		Status:            "review_required",
		AuthorityRef:      OutboundDistributionDryRunAuthorityRef,
		PlanID:            verdict.PlanID,
		ProductID:         verdict.ProductID,
		VerdictReviewPath: verdict.ReviewedPath,
		AdapterOutputPath: opts.AdapterOutputPath,
		AdoptionLevel:     "dry_run_only",
		AdapterID:         output.AdapterID,
		ProductLoop:       output.ProductLoop,
		Artifacts:         output.Artifacts,
		Validation: OutboundDistributionDryRunValidation{
			Mode:                 output.Mode,
			RunKind:              output.RunKind,
			PublicSideEffects:    output.PublicSideEffects,
			AccountAuthUsed:      output.AccountAuthUsed,
			ExternalRequestsMade: output.ExternalRequestsMade,
			Posted:               output.Posted,
			Replied:              output.Replied,
			SpentMoney:           output.SpentMoney,
			ArtifactCount:        len(output.Artifacts),
		},
		EvidenceRefs: evidenceRefs,
		Boundaries: []string{
			"does not mutate product signals, product hypotheses, memory claims, or capability graph offers",
			"does not authenticate social accounts, post, reply, spend, clone, install, or contact external services",
			"public distribution remains behind Tier-3 PolicyDecision",
		},
		TargetPath:   opts.TargetPath,
		TargetExists: targetExists,
		TargetHash:   targetHash,
		ReviewDraft: OutboundDistributionDryRunReviewDraft{
			ReviewID:         reviewID,
			ReviewQuestion:   "Does this dry-run receipt provide reviewable product-loop evidence without public side effects?",
			AllowedDecisions: []string{"accept_as_product_loop_evidence", "park", "reject"},
			RequiredEvidence: []string{
				"adopt_for_dry_run verdict review report",
				"adapter output proves mode=dry_run and public_side_effects=false",
				"product_loop carries change_event, hypothesis, observation_window, next_bounded_move",
				"artifacts are draft/local refs with content_hash, not public posts",
			},
			ReviewCommand: "compound capability outbound-distribution-dry-run-review <review.yaml>",
		},
	}, nil
}

func LoadOutboundDistributionVerdictReviewReport(path string) (OutboundDistributionVerdictReviewReport, error) {
	var report OutboundDistributionVerdictReviewReport
	data, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return report, fmt.Errorf("parse outbound verdict review report %s: %w", path, err)
	}
	if report.SchemaVersion != OutboundDistributionVerdictReportSchemaVersion {
		return report, fmt.Errorf("unexpected outbound verdict review schema %q", report.SchemaVersion)
	}
	return report, nil
}

func LoadOutboundDistributionDryRunAdapterOutput(path string) (OutboundDistributionDryRunAdapterOutput, error) {
	var output OutboundDistributionDryRunAdapterOutput
	data, err := os.ReadFile(path)
	if err != nil {
		return output, err
	}
	if err := json.Unmarshal(data, &output); err != nil {
		return output, fmt.Errorf("parse outbound dry-run adapter output %s: %w", path, err)
	}
	return output, nil
}

func validateOutboundDistributionDryRunOutput(verdict OutboundDistributionVerdictReviewReport, output OutboundDistributionDryRunAdapterOutput) error {
	if output.SchemaVersion != OutboundDistributionDryRunReceiptSchemaVersion {
		return fmt.Errorf("unexpected outbound dry-run receipt schema %q", output.SchemaVersion)
	}
	if strings.TrimSpace(output.ID) == "" {
		return fmt.Errorf("outbound dry-run receipt id required")
	}
	if output.PlanID != verdict.PlanID {
		return fmt.Errorf("adapter output plan_id %q does not match verdict plan_id %q", output.PlanID, verdict.PlanID)
	}
	if output.ProductID != verdict.ProductID {
		return fmt.Errorf("adapter output product_id %q does not match verdict product_id %q", output.ProductID, verdict.ProductID)
	}
	if strings.TrimSpace(output.AdapterID) == "" {
		return fmt.Errorf("adapter_id required")
	}
	if strings.TrimSpace(output.Mode) != "dry_run" {
		return fmt.Errorf("mode must be dry_run, got %q", output.Mode)
	}
	if strings.TrimSpace(output.RunKind) != "synthetic" && strings.TrimSpace(output.RunKind) != "local_dry_run" {
		return fmt.Errorf("run_kind must be synthetic or local_dry_run, got %q", output.RunKind)
	}
	if output.PublicSideEffects || output.AccountAuthUsed || output.ExternalRequestsMade || output.Posted || output.Replied || output.SpentMoney {
		return fmt.Errorf("dry-run receipt shows public side effects/auth/external requests/posting/spend")
	}
	if strings.TrimSpace(output.ProductLoop.ChangeEvent) == "" || strings.TrimSpace(output.ProductLoop.Hypothesis) == "" || strings.TrimSpace(output.ProductLoop.ObservationWindow) == "" || strings.TrimSpace(output.ProductLoop.NextBoundedMove) == "" {
		return fmt.Errorf("product_loop requires change_event, hypothesis, observation_window, next_bounded_move")
	}
	if len(output.Artifacts) == 0 {
		return fmt.Errorf("at least one dry-run artifact required")
	}
	for i, artifact := range output.Artifacts {
		if strings.TrimSpace(artifact.Kind) == "" || strings.TrimSpace(artifact.Ref) == "" || strings.TrimSpace(artifact.ContentHash) == "" {
			return fmt.Errorf("artifact %d requires kind, ref, content_hash", i)
		}
		if strings.HasPrefix(strings.ToLower(artifact.Ref), "http://") || strings.HasPrefix(strings.ToLower(artifact.Ref), "https://") {
			return fmt.Errorf("artifact %d ref looks public; dry-run artifacts must be local or fixture refs", i)
		}
	}
	if len(output.EvidenceRefs) == 0 {
		return fmt.Errorf("evidence_refs required")
	}
	return nil
}

func RecordOutboundDistributionDryRunReceipt(homePath string, report OutboundDistributionDryRunReceiptReport) (OutboundDistributionDryRunReceiptPaths, error) {
	if strings.TrimSpace(report.ID) == "" {
		return OutboundDistributionDryRunReceiptPaths{}, fmt.Errorf("dry-run receipt report id required")
	}
	paths, err := reviewartifact.Write(homePath, OutboundDistributionDryRunReviewArea, report.ID, report, " ", RenderOutboundDistributionDryRunReceipt(report))
	if err != nil {
		return OutboundDistributionDryRunReceiptPaths{}, err
	}
	reviewPath, err := reviewartifact.WriteRaw(homePath, OutboundDistributionDryRunReviewArea, report.ID+".review.yaml", []byte(RenderOutboundDistributionDryRunReviewYAML(report)))
	if err != nil {
		return OutboundDistributionDryRunReceiptPaths{}, err
	}
	return OutboundDistributionDryRunReceiptPaths{ReportPath: paths.PlanPath, ReviewPath: paths.ReviewPath, ReviewYAMLPath: reviewPath}, nil
}

func RenderOutboundDistributionDryRunReceipt(report OutboundDistributionDryRunReceiptReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Outbound Distribution Dry-Run Receipt: %s\n\n", report.ProductID)
	fmt.Fprintf(&b, "- status: %s\n", report.Status)
	fmt.Fprintf(&b, "- product: %s\n", report.ProductID)
	fmt.Fprintf(&b, "- plan: %s\n", report.PlanID)
	fmt.Fprintf(&b, "- adapter: %s\n", report.AdapterID)
	fmt.Fprintf(&b, "- adoption_level: %s\n", report.AdoptionLevel)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## No Public Side Effects")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- mode: %s\n", report.Validation.Mode)
	fmt.Fprintf(&b, "- run_kind: %s\n", report.Validation.RunKind)
	fmt.Fprintf(&b, "- public_side_effects: %t\n", report.Validation.PublicSideEffects)
	fmt.Fprintf(&b, "- account_auth_used: %t\n", report.Validation.AccountAuthUsed)
	fmt.Fprintf(&b, "- external_requests_made: %t\n", report.Validation.ExternalRequestsMade)
	fmt.Fprintf(&b, "- posted: %t\n", report.Validation.Posted)
	fmt.Fprintf(&b, "- replied: %t\n", report.Validation.Replied)
	fmt.Fprintf(&b, "- spent_money: %t\n", report.Validation.SpentMoney)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Product Loop Evidence")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- change_event: %s\n", report.ProductLoop.ChangeEvent)
	fmt.Fprintf(&b, "- hypothesis: %s\n", report.ProductLoop.Hypothesis)
	fmt.Fprintf(&b, "- observation_window: %s\n", report.ProductLoop.ObservationWindow)
	fmt.Fprintf(&b, "- next_bounded_move: %s\n", report.ProductLoop.NextBoundedMove)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Artifacts")
	fmt.Fprintln(&b)
	for _, artifact := range report.Artifacts {
		fmt.Fprintf(&b, "- %s channel=%s ref=%s hash=%s\n", artifact.Kind, artifact.Channel, artifact.Ref, artifact.ContentHash)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Boundaries")
	fmt.Fprintln(&b)
	for _, boundary := range report.Boundaries {
		fmt.Fprintf(&b, "- %s\n", boundary)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Review command: `%s`\n", report.ReviewDraft.ReviewCommand)
	return b.String()
}

func RenderOutboundDistributionDryRunReviewYAML(report OutboundDistributionDryRunReceiptReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", OutboundDistributionDryRunReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", report.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "receipt_id: %s\n", report.ID)
	fmt.Fprintf(&b, "plan_id: %s\n", report.PlanID)
	fmt.Fprintf(&b, "product_id: %s\n", report.ProductID)
	fmt.Fprintln(&b, "decision: TODO_decision_accept_as_product_loop_evidence_park_reject")
	fmt.Fprintln(&b, "rationale: TODO_rationale")
	fmt.Fprintln(&b, "confidence: TODO_confidence_0_to_1")
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range report.EvidenceRefs {
		fmt.Fprintf(&b, " - %s\n", ref)
	}
	return b.String()
}

func LoadOutboundDistributionDryRunReview(path string) (OutboundDistributionDryRunReview, error) {
	var review OutboundDistributionDryRunReview
	data, err := os.ReadFile(path)
	if err != nil {
		return review, err
	}
	if err := yaml.Unmarshal(data, &review); err != nil {
		return review, err
	}
	return review, nil
}

func RecordOutboundDistributionDryRunReview(homePath string, review OutboundDistributionDryRunReview, reviewPath string, now time.Time) (OutboundDistributionDryRunReviewReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return OutboundDistributionDryRunReviewReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if review.SchemaVersion != OutboundDistributionDryRunReviewSchemaVersion {
		return OutboundDistributionDryRunReviewReport{}, fmt.Errorf("unexpected outbound dry-run review schema %q", review.SchemaVersion)
	}
	if strings.TrimSpace(review.ReceiptID) == "" {
		return OutboundDistributionDryRunReviewReport{}, fmt.Errorf("receipt_id required")
	}
	if !outboundDistributionDryRunAllowedReviewDecision(review.Decision) {
		return OutboundDistributionDryRunReviewReport{}, fmt.Errorf("decision %q must be accept_as_product_loop_evidence, park, or reject", review.Decision)
	}
	if strings.TrimSpace(review.Rationale) == "" || strings.HasPrefix(strings.TrimSpace(review.Rationale), "TODO") {
		return OutboundDistributionDryRunReviewReport{}, fmt.Errorf("rationale required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return OutboundDistributionDryRunReviewReport{}, fmt.Errorf("confidence must be between 0 and 1")
	}
	review.ReviewedAt = firstNonEmpty(review.ReviewedAt, now.UTC().Format(time.RFC3339))
	report := OutboundDistributionDryRunReviewReport{
		SchemaVersion:    OutboundDistributionDryRunReviewReportSchemaVersion,
		ID:               "oddr_review_" + outboundDistributionStableID("", review.ReceiptID, review.Decision, review.Rationale),
		CreatedAt:        now.UTC().Format(time.RFC3339),
		Status:           "recorded",
		ReceiptID:        review.ReceiptID,
		PlanID:           review.PlanID,
		ProductID:        review.ProductID,
		Decision:         review.Decision,
		ReviewSourcePath: strings.TrimSpace(reviewPath),
		EvidenceRefs:     reviewartifact.UniqueSorted(append([]string{}, review.EvidenceRefs...)),
	}
	path, err := reviewartifact.WriteJSON(homePath, OutboundDistributionDryRunReviewArea, review.ReceiptID+".reviewed.json", report, " ")
	if err != nil {
		return OutboundDistributionDryRunReviewReport{}, err
	}
	report.ReviewedPath = path
	if _, err := reviewartifact.WriteJSON(homePath, OutboundDistributionDryRunReviewArea, review.ReceiptID+".reviewed.json", report, " "); err != nil {
		return OutboundDistributionDryRunReviewReport{}, err
	}
	return report, nil
}

func AnnotateOutboundDistributionDryRunQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != OutboundDistributionDryRunReviewArea {
			continue
		}
		annotateOutboundDistributionDryRunItem(item)
	}
}

func annotateOutboundDistributionDryRunItem(item *reviewartifact.QueueItem) {
	if reviewed, ok := outboundDistributionDryRunReviewedFromExtras(item.ExtraPaths); ok {
		item.Status = "ok"
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = "outbound distribution dry-run review recorded: " + reviewed.Decision
		item.Actions = nil
		item.DecisionCard = nil
		return
	}
	receipt, ok := outboundDistributionDryRunReceiptFromQueueItem(*item)
	if !ok {
		item.Status = "blocked"
		item.Readiness = "blocked"
		item.BlockedReason = "outbound distribution dry-run receipt artifact unreadable"
		item.NextAction = "regenerate outbound dry-run receipt"
		item.Actions = nil
		return
	}
	if strings.TrimSpace(item.Status) == "" || strings.TrimSpace(item.Status) == "unknown" {
		item.Status = "review_required"
	}
	reviewPath := outboundDistributionDryRunReviewYAMLPath(*item)
	if reviewPath == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "review YAML draft missing"
		item.NextAction = "regenerate outbound dry-run receipt"
		item.Actions = nil
		return
	}
	item.Readiness = "needs_review"
	item.BlockedReason = "dry-run receipt must be accepted, parked, or rejected before product-loop use"
	item.NextAction = "edit review YAML then record outbound dry-run evidence decision"
	card := outboundDistributionDryRunDecisionCard(receipt, reviewPath)
	item.DecisionCard = card
	item.Actions = outboundDistributionDryRunDecisionCardActions(card)
}

func outboundDistributionDryRunDecisionCard(receipt OutboundDistributionDryRunReceiptReport, reviewPath string) *reviewartifact.DecisionCard {
	command := outboundDistributionDryRunReviewCommand(reviewPath)
	card := &reviewartifact.DecisionCard{
		Claim:               "Review outbound dry-run receipt for " + receipt.ProductID,
		OptionalRationale:   false,
		PanelLean:           "accept only if synthetic/local dry-run artifacts are reviewable and no public side effects occurred",
		CanonicalActionHint: "Edit review YAML decision/rationale/confidence, then run `compound capability outbound-distribution-dry-run-review <review.yaml>`. records evidence review only; does not mutate product signals or post.",
		Boundary: []string{
			"does not mutate product signals, hypotheses, memory, capability graph, or external accounts",
			"public distribution remains Tier-3 gated",
		},
		Meta: map[string]string{
			"review_yaml": reviewPath,
			"receipt_id":  receipt.ID,
			"product_id":  receipt.ProductID,
		},
	}
	card.Why = []reviewartifact.DecisionCardReason{
		{Label: "side effects", Severity: "warning", Question: "Did the adapter avoid all public side effects?", Detail: fmt.Sprintf("posted=%t auth=%t external_requests=%t", receipt.Validation.Posted, receipt.Validation.AccountAuthUsed, receipt.Validation.ExternalRequestsMade)},
		{Label: "product loop", Question: "What loop would consume this?", Detail: receipt.ProductLoop.ChangeEvent + " / " + receipt.ProductLoop.NextBoundedMove},
		{Label: "artifacts", Question: "What reviewable draft artifacts exist?", Detail: fmt.Sprintf("%d artifact(s)", len(receipt.Artifacts))},
		{Label: "boundary", Severity: "warning", Question: "What must not happen from this receipt?", Detail: strings.Join(receipt.Boundaries, "; ")},
	}
	card.Choices = []reviewartifact.DecisionCardChoice{
		{ID: "accept_as_product_loop_evidence", Label: "Accept Evidence", Effect: "Records dry-run output as reviewed product-loop evidence only; no product signal or public action.", Command: command, Argv: []string{"compound", "capability", "outbound-distribution-dry-run-review", reviewPath}, RequiresReview: true},
		{ID: "park", Label: "Park", Effect: "Keeps receipt available without product-loop consumption.", Command: command, Argv: []string{"compound", "capability", "outbound-distribution-dry-run-review", reviewPath}, RequiresReview: true},
		{ID: "reject", Label: "Reject", Effect: "Records dry-run output should not feed product-loop evidence.", Command: command, Argv: []string{"compound", "capability", "outbound-distribution-dry-run-review", reviewPath}, RequiresReview: true},
	}
	return card
}

func outboundDistributionDryRunDecisionCardActions(card *reviewartifact.DecisionCard) []reviewartifact.Action {
	if card == nil {
		return nil
	}
	return []reviewartifact.Action{{
		ID:             "record_outbound_distribution_dry_run_review",
		Label:          "Record outbound dry-run evidence review",
		Command:        outboundDistributionDryRunReviewCommand(card.Meta["review_yaml"]),
		Argv:           []string{"compound", "capability", "outbound-distribution-dry-run-review", card.Meta["review_yaml"]},
		RequiresReview: true,
	}}
}

func outboundDistributionDryRunReceiptFromQueueItem(item reviewartifact.QueueItem) (OutboundDistributionDryRunReceiptReport, bool) {
	var receipt OutboundDistributionDryRunReceiptReport
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return receipt, false
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		return receipt, false
	}
	return receipt, receipt.ID != ""
}

func outboundDistributionDryRunReviewedFromExtras(paths []string) (OutboundDistributionDryRunReviewReport, bool) {
	var found OutboundDistributionDryRunReviewReport
	for _, path := range paths {
		if !strings.HasSuffix(path, ".reviewed.json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var report OutboundDistributionDryRunReviewReport
		if err := json.Unmarshal(data, &report); err != nil {
			continue
		}
		if report.Status != "recorded" {
			continue
		}
		if found.ID == "" || report.CreatedAt > found.CreatedAt {
			found = report
		}
	}
	return found, found.ID != ""
}

func outboundDistributionDryRunReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			return path
		}
	}
	return ""
}

func outboundDistributionDryRunReviewCommand(reviewPath string) string {
	if strings.TrimSpace(reviewPath) == "" {
		return "compound capability outbound-distribution-dry-run-review <review.yaml>"
	}
	quoted := shellQuote(reviewPath)
	return "$EDITOR " + quoted + " && compound capability outbound-distribution-dry-run-review " + quoted
}

func outboundDistributionDryRunAllowedReviewDecision(decision string) bool {
	switch strings.TrimSpace(decision) {
	case "accept_as_product_loop_evidence", "park", "reject":
		return true
	default:
		return false
	}
}
