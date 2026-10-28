package product

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
	"compound/internal/work"

	"gopkg.in/yaml.v3"
)

const (
	ProductReleasePlanSchemaVersion         = "compound.product_release_plan.v0.1"
	ProductReleaseReviewSchemaVersion       = "compound.product_release_review.v0.1"
	ProductReleaseReviewReportSchemaVersion = "compound.product_release_review_report.v0.1"
	ProductReleaseWorkDraftSchemaVersion    = "compound.product_release_work_draft.v0.1"
	ProductReleaseObservationSchemaVersion  = "compound.product_release_observation.v0.1"
	ProductReleaseReviewArea                = "product-releases"
)

type ReleasePlanInput struct {
	ProductID         string
	ActionCardID      string
	ChangeSummary     string
	CandidateRefs     []string
	Rollback          string
	PostReleaseMetric string
	EvidenceRefs      []string
}

type ProductReleasePlan struct {
	SchemaVersion           string                    `json:"schema_version"`
	ID                      string                    `json:"id"`
	CreatedAt               string                    `json:"created_at"`
	Status                  string                    `json:"status"`
	ProductID               string                    `json:"product_id"`
	ActionCardID            string                    `json:"action_card_id,omitempty"`
	ChangeSummary           string                    `json:"change_summary"`
	ReleaseIdentity         ReleaseIdentity           `json:"release_identity"`
	Mechanism               string                    `json:"mechanism"`
	CandidateRefs           []string                  `json:"candidate_refs"`
	Rollback                string                    `json:"rollback"`
	PostReleaseMetric       string                    `json:"post_release_metric"`
	RequiredChecks          []string                  `json:"required_checks"`
	EvidenceRefs            []string                  `json:"evidence_refs"`
	Boundary                string                    `json:"boundary"`
	ExternalMutationAllowed bool                      `json:"external_mutation_allowed"`
	ReviewDraft             ProductReleaseReviewDraft `json:"review_draft"`
	ReviewedAt              string                    `json:"reviewed_at,omitempty"`
	ReviewID                string                    `json:"review_id,omitempty"`
	ReviewDecision          string                    `json:"review_decision,omitempty"`
	ReviewedPath            string                    `json:"reviewed_path,omitempty"`
}

type ProductReleaseReviewDraft struct {
	ReviewID         string   `json:"review_id"`
	Title            string   `json:"title"`
	ReviewQuestion   string   `json:"review_question"`
	AllowedDecisions []string `json:"allowed_decisions"`
	RequiredEvidence []string `json:"required_evidence"`
	ReviewCommand    string   `json:"review_command"`
	ReviewNote       string   `json:"review_note"`
}

type ProductReleasePlanPaths struct {
	PlanPath       string
	ReviewPath     string
	ReviewYAMLPath string
}

type ProductReleaseReview struct {
	SchemaVersion   string   `json:"schema_version" yaml:"schema_version"`
	ID              string   `json:"id" yaml:"id"`
	ProductID       string   `json:"product_id" yaml:"product_id"`
	PlanID          string   `json:"plan_id" yaml:"plan_id"`
	Decision        string   `json:"decision" yaml:"decision"`
	Rationale       string   `json:"rationale" yaml:"rationale"`
	Confidence      float64  `json:"confidence" yaml:"confidence"`
	ReviewedBy      string   `json:"reviewed_by" yaml:"reviewed_by"`
	CompletedChecks []string `json:"completed_checks,omitempty" yaml:"completed_checks,omitempty"`
	EvidenceRefs    []string `json:"evidence_refs" yaml:"evidence_refs"`
	ReviewedAt      string   `json:"reviewed_at,omitempty" yaml:"reviewed_at,omitempty"`
}

type ProductReleaseReviewReport struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	CreatedAt     string   `json:"created_at"`
	Status        string   `json:"status"`
	ProductID     string   `json:"product_id"`
	PlanID        string   `json:"plan_id"`
	ReviewID      string   `json:"review_id"`
	Decision      string   `json:"decision"`
	ReviewedBy    string   `json:"reviewed_by"`
	Authorized    bool     `json:"authorized"`
	Executed      bool     `json:"executed"`
	NextAction    string   `json:"next_action"`
	TransitionID  string   `json:"transition_id"`
	EvidenceRefs  []string `json:"evidence_refs"`
	ReviewPath    string   `json:"review_path"`
	ReviewedPath  string   `json:"reviewed_path"`
}

// ProductReleaseWorkDraft is the exact, authorization-bound release action
// handed to WorkEnvelope planning. It carries no external authority itself.
type ProductReleaseWorkDraft struct {
	SchemaVersion     string          `json:"schema_version"`
	ID                string          `json:"id"`
	Status            string          `json:"status"`
	ProductID         string          `json:"product_id"`
	PlanID            string          `json:"plan_id"`
	AuthorizationID   string          `json:"authorization_id"`
	ReviewID          string          `json:"review_id"`
	ReleaseIdentity   ReleaseIdentity `json:"release_identity"`
	Mechanism         string          `json:"mechanism"`
	ChangeSummary     string          `json:"change_summary"`
	CandidateRefs     []string        `json:"candidate_refs"`
	Rollback          string          `json:"rollback"`
	PostReleaseMetric string          `json:"post_release_metric"`
	RequiredChecks    []string        `json:"required_checks"`
	RequiresOperator  bool            `json:"requires_operator"`
	Boundary          string          `json:"boundary"`
	Proof             string          `json:"proof"`
	WorkID            string          `json:"work_id"`
	WorkOwner         string          `json:"work_owner"`
	SuccessPredicate  string          `json:"success_predicate"`
	DesiredOutcome    string          `json:"desired_outcome"`
	NonGoals          []string        `json:"non_goals"`
	EvidenceRefs      []string        `json:"evidence_refs"`
}

type ReleaseObservationInput struct {
	ReviewedPath string
	ExecutionRef string
	SignalID     string
}

type ProductReleaseObservationReport struct {
	SchemaVersion      string   `json:"schema_version"`
	ID                 string   `json:"id"`
	CreatedAt          string   `json:"created_at"`
	Status             string   `json:"status"`
	ProductID          string   `json:"product_id"`
	PlanID             string   `json:"plan_id"`
	AuthorizationID    string   `json:"authorization_id"`
	AuthorizationAt    string   `json:"authorization_at"`
	WorkID             string   `json:"work_id"`
	ExecutionReceiptID string   `json:"execution_receipt_id"`
	ExecutedAt         string   `json:"executed_at"`
	SignalID           string   `json:"signal_id"`
	MetricName         string   `json:"metric_name"`
	ObservedAt         string   `json:"observed_at"`
	TransitionID       string   `json:"transition_id"`
	EvidenceRefs       []string `json:"evidence_refs"`
	ReportPath         string   `json:"report_path,omitempty"`
}

func BuildReleasePlan(homePath string, store *mem.Store, input ReleasePlanInput, now time.Time) (ProductReleasePlan, error) {
	if strings.TrimSpace(homePath) == "" {
		return ProductReleasePlan{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if store == nil {
		return ProductReleasePlan{}, fmt.Errorf("Compound memory store required")
	}
	input.ProductID = strings.TrimSpace(input.ProductID)
	input.ChangeSummary = strings.TrimSpace(input.ChangeSummary)
	input.Rollback = strings.TrimSpace(input.Rollback)
	input.PostReleaseMetric = strings.TrimSpace(input.PostReleaseMetric)
	if input.ProductID == "" || input.ChangeSummary == "" || input.Rollback == "" || input.PostReleaseMetric == "" {
		return ProductReleasePlan{}, fmt.Errorf("product_id change_summary rollback post_release_metric required")
	}
	input.CandidateRefs = reviewartifact.UniqueSorted(compact(input.CandidateRefs))
	if len(input.CandidateRefs) == 0 {
		return ProductReleasePlan{}, fmt.Errorf("at least one release candidate ref required")
	}
	identity, mechanism, err := resolveReleaseIdentity(store, input.ProductID)
	if err != nil {
		return ProductReleasePlan{}, err
	}
	requiredChecks := []string{
		"candidate_bound_to_release_identity",
		"verification_passed",
		"store_state_preflight_passed",
		"rollback_named",
		"post_release_signal_named",
	}
	evidenceRefs := append([]string{}, input.EvidenceRefs...)
	evidenceRefs = append(evidenceRefs, input.CandidateRefs...)
	evidenceRefs = append(evidenceRefs,
		input.ProductID,
		"product_release_identity:"+input.ProductID,
		"post_release_metric:"+input.PostReleaseMetric,
	)
	if cardID := strings.TrimSpace(input.ActionCardID); cardID != "" {
		evidenceRefs = append(evidenceRefs, "product_action_card:"+cardID)
	}
	evidenceRefs = reviewartifact.UniqueSorted(compact(evidenceRefs))
	id := stableID("prlp_", input.ProductID, identity.BundleID, identity.AppStoreConnectAppID, identity.Version, identity.Build, input.ChangeSummary, strings.Join(input.CandidateRefs, ","))
	reviewID := stableID("product_release_review_", id)
	return ProductReleasePlan{
		SchemaVersion:           ProductReleasePlanSchemaVersion,
		ID:                      id,
		CreatedAt:               now.UTC().Format(time.RFC3339),
		Status:                  "review_required",
		ProductID:               input.ProductID,
		ActionCardID:            strings.TrimSpace(input.ActionCardID),
		ChangeSummary:           input.ChangeSummary,
		ReleaseIdentity:         identity,
		Mechanism:               mechanism,
		CandidateRefs:           input.CandidateRefs,
		Rollback:                input.Rollback,
		PostReleaseMetric:       input.PostReleaseMetric,
		RequiredChecks:          requiredChecks,
		EvidenceRefs:            evidenceRefs,
		Boundary:                "This plan does not execute, upload, submit, publish, price, or mutate an external store. A reviewed authorization is evidence for a later governed action, never the action itself.",
		ExternalMutationAllowed: false,
		ReviewDraft: ProductReleaseReviewDraft{
			ReviewID:         reviewID,
			Title:            "Authorize the named release for " + input.ProductID,
			ReviewQuestion:   "Should exactly this verified candidate become authorized for the named release identity?",
			AllowedDecisions: []string{"authorize_release", "hold", "reject"},
			RequiredEvidence: []string{
				"candidate ref resolves to the tested artifact",
				"version and build match the external store preflight",
				"rollback and post-release signal are named",
				"authorization is scoped to this plan and does not itself execute",
			},
			ReviewCommand: "compound product release-review <review.yaml>",
			ReviewNote:    "Authorization records a governed next move. External store mutation remains a separate PolicyDecision-gated execution.",
		},
	}, nil
}

func resolveReleaseIdentity(store *mem.Store, productID string) (ReleaseIdentity, string, error) {
	read := func(predicate string) (string, error) {
		claim, ok, err := store.CurrentClaim(productID, predicate)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", nil
		}
		return strings.TrimSpace(claim.Object), nil
	}
	bundleID, err := read("release_bundle_id")
	if err != nil {
		return ReleaseIdentity{}, "", err
	}
	appID, err := read("release_app_store_connect_app_id")
	if err != nil {
		return ReleaseIdentity{}, "", err
	}
	version, err := read("release_version")
	if err != nil {
		return ReleaseIdentity{}, "", err
	}
	build, err := read("release_build")
	if err != nil {
		return ReleaseIdentity{}, "", err
	}
	subscriptionIDs, err := read("release_subscription_ids")
	if err != nil {
		return ReleaseIdentity{}, "", err
	}
	mechanism, err := read("release_mechanism")
	if err != nil {
		return ReleaseIdentity{}, "", err
	}
	if bundleID == "" || appID == "" || version == "" || build == "" || mechanism == "" {
		return ReleaseIdentity{}, "", fmt.Errorf("Compound-owned release identity incomplete for %s; import the product project/release contract before planning", productID)
	}
	return ReleaseIdentity{
		BundleID:             bundleID,
		AppStoreConnectAppID: appID,
		SubscriptionIDs:      compact(strings.Split(subscriptionIDs, ",")),
		Version:              version,
		Build:                build,
	}, mechanism, nil
}

func RecordReleasePlan(homePath string, plan ProductReleasePlan) (ProductReleasePlanPaths, error) {
	if strings.TrimSpace(plan.ID) == "" {
		return ProductReleasePlanPaths{}, fmt.Errorf("product release plan id required")
	}
	dir, err := reviewartifact.Dir(homePath, ProductReleaseReviewArea)
	if err != nil {
		return ProductReleasePlanPaths{}, err
	}
	paths := ProductReleasePlanPaths{
		PlanPath:       filepath.Join(dir, plan.ID+".json"),
		ReviewPath:     filepath.Join(dir, plan.ID+".md"),
		ReviewYAMLPath: filepath.Join(dir, plan.ID+".review.yaml"),
	}
	if _, err := reviewartifact.Write(homePath, ProductReleaseReviewArea, plan.ID, plan, " ", RenderReleasePlan(plan)); err != nil {
		return ProductReleasePlanPaths{}, err
	}
	if _, err := reviewartifact.WriteRaw(homePath, ProductReleaseReviewArea, plan.ID+".review.yaml", []byte(RenderReleaseReviewYAML(plan))); err != nil {
		return ProductReleasePlanPaths{}, err
	}
	return paths, nil
}

func RenderReleasePlan(plan ProductReleasePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Product release authorization: %s\n\n", plan.ProductID)
	fmt.Fprintf(&b, "- status: %s\n", plan.Status)
	fmt.Fprintf(&b, "- change: %s\n", plan.ChangeSummary)
	fmt.Fprintf(&b, "- destination: App Store Connect app %s · bundle %s\n", plan.ReleaseIdentity.AppStoreConnectAppID, plan.ReleaseIdentity.BundleID)
	fmt.Fprintf(&b, "- candidate: version %s · build %s\n", plan.ReleaseIdentity.Version, plan.ReleaseIdentity.Build)
	fmt.Fprintf(&b, "- mechanism: %s\n", plan.Mechanism)
	fmt.Fprintf(&b, "- rollback: %s\n", plan.Rollback)
	fmt.Fprintf(&b, "- post-release signal: %s\n", plan.PostReleaseMetric)
	fmt.Fprintf(&b, "- boundary: %s\n", plan.Boundary)
	return b.String()
}

func RenderReleaseReviewYAML(plan ProductReleasePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", ProductReleaseReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", plan.ReviewDraft.ReviewID)
	fmt.Fprintf(&b, "product_id: %s\n", plan.ProductID)
	fmt.Fprintf(&b, "plan_id: %s\n", plan.ID)
	fmt.Fprintln(&b, "decision: TODO_decision_authorize_release_hold_reject")
	fmt.Fprintln(&b, "rationale: TODO_rationale")
	fmt.Fprintln(&b, "confidence: TODO_confidence_0_to_1")
	fmt.Fprintln(&b, "reviewed_by: TODO_operator_actor")
	fmt.Fprintln(&b, "completed_checks:")
	for _, check := range plan.RequiredChecks {
		fmt.Fprintf(&b, " - TODO_%s\n", check)
	}
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range plan.EvidenceRefs {
		fmt.Fprintf(&b, " - %q\n", ref)
	}
	return b.String()
}

func LoadReleaseReview(path string) (ProductReleaseReview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ProductReleaseReview{}, err
	}
	var review ProductReleaseReview
	if err := yaml.Unmarshal(data, &review); err != nil {
		return ProductReleaseReview{}, err
	}
	return review, nil
}

func ApplyReleaseReview(homePath string, review ProductReleaseReview, sourcePath string, now time.Time) (ProductReleaseReviewReport, error) {
	if err := ValidateReleaseReview(review); err != nil {
		return ProductReleaseReviewReport{Status: "fail", ProductID: review.ProductID, PlanID: review.PlanID, ReviewID: review.ID}, err
	}
	plan, err := loadReleasePlan(homePath, review.PlanID)
	if err != nil {
		return ProductReleaseReviewReport{}, err
	}
	if plan.ProductID != review.ProductID || plan.ReviewDraft.ReviewID != review.ID {
		return ProductReleaseReviewReport{}, fmt.Errorf("product release plan/review mismatch")
	}
	if review.Decision == "authorize_release" {
		for _, required := range plan.RequiredChecks {
			if !containsString(review.CompletedChecks, required) {
				return ProductReleaseReviewReport{}, fmt.Errorf("release authorization missing completed check %q", required)
			}
		}
	}
	if review.ReviewedAt == "" {
		review.ReviewedAt = now.UTC().Format(time.RFC3339)
	}
	authorized := review.Decision == "authorize_release"
	nextAction := "keep this release held; no external action is authorized"
	status := "recorded"
	if authorized {
		status = "authorization_recorded"
		nextAction = "dispatch exactly this candidate through a governed release WorkEnvelope; external mutation still requires its allowing PolicyDecision and execution receipt"
	}
	evidenceRefs := reviewartifact.UniqueSorted(compact(append(append([]string{}, plan.EvidenceRefs...), review.EvidenceRefs...)))
	report := ProductReleaseReviewReport{
		SchemaVersion: ProductReleaseReviewReportSchemaVersion,
		ID:            reviewartifact.ID("prlr_", plan.ID, review.ID, review.Decision),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Status:        status,
		ProductID:     review.ProductID,
		PlanID:        plan.ID,
		ReviewID:      review.ID,
		Decision:      review.Decision,
		ReviewedBy:    strings.TrimSpace(review.ReviewedBy),
		Authorized:    authorized,
		Executed:      false,
		NextAction:    nextAction,
		EvidenceRefs:  evidenceRefs,
		ReviewPath:    strings.TrimSpace(sourcePath),
	}
	afterRefs := append([]string{}, evidenceRefs...)
	afterRefs = append(afterRefs, "product_release_review:"+review.ID)
	if authorized {
		afterRefs = append(afterRefs, "product_release_authorization:"+report.ID)
	}
	tr, err := BuildLoopTransition(LoopTransitionInput{
		Kind:         "product_release_review",
		ProductID:    plan.ProductID,
		ObjectID:     report.ID,
		SourceTrace:  "product_release_review:" + review.ID,
		BeforeRefs:   plan.EvidenceRefs,
		AfterRefs:    afterRefs,
		EvidenceRefs: evidenceRefs,
		Summary:      fmt.Sprintf("product release plan %s reviewed as %s; executed=false", plan.ID, review.Decision),
	}, now)
	if err != nil {
		return ProductReleaseReviewReport{}, err
	}
	if err := RecordTransition(homePath, tr); err != nil {
		return ProductReleaseReviewReport{}, err
	}
	report.TransitionID = tr.ID
	dir, err := reviewartifact.Dir(homePath, ProductReleaseReviewArea)
	if err != nil {
		return ProductReleaseReviewReport{}, err
	}
	name := plan.ID + "." + report.ID + ".reviewed.json"
	report.ReviewedPath = filepath.Join(dir, name)
	if _, err := reviewartifact.WriteJSON(homePath, ProductReleaseReviewArea, name, report, " "); err != nil {
		return ProductReleaseReviewReport{}, err
	}
	plan.Status = status
	plan.ReviewedAt = report.CreatedAt
	plan.ReviewID = review.ID
	plan.ReviewDecision = review.Decision
	plan.ReviewedPath = report.ReviewedPath
	if _, err := reviewartifact.Write(homePath, ProductReleaseReviewArea, plan.ID, plan, " ", RenderReleasePlan(plan)); err != nil {
		return ProductReleaseReviewReport{}, err
	}
	return report, nil
}

// BuildReleaseWorkDraft turns an exact recorded authorization into bounded
// work semantics. It refuses held/rejected reviews and never creates work or
// performs the release.
func BuildReleaseWorkDraft(homePath, reviewedPath string) (ProductReleaseWorkDraft, error) {
	homePath = strings.TrimSpace(homePath)
	reviewedPath = strings.TrimSpace(reviewedPath)
	if homePath == "" {
		return ProductReleaseWorkDraft{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if reviewedPath == "" {
		return ProductReleaseWorkDraft{}, fmt.Errorf("reviewed release report path required")
	}
	report, err := LoadProductReleaseReviewReport(reviewedPath)
	if err != nil {
		return ProductReleaseWorkDraft{}, err
	}
	if report.Status != "authorization_recorded" || !report.Authorized || report.Executed || report.Decision != "authorize_release" {
		return ProductReleaseWorkDraft{}, fmt.Errorf("release review report %q is not an unexecuted authorization", report.ID)
	}
	plan, err := loadReleasePlan(homePath, report.PlanID)
	if err != nil {
		return ProductReleaseWorkDraft{}, err
	}
	if plan.ProductID != report.ProductID || plan.ReviewID != report.ReviewID || plan.ReviewedPath != report.ReviewedPath {
		return ProductReleaseWorkDraft{}, fmt.Errorf("release authorization does not match current release plan")
	}
	workID := stableID("work_", "product_release_authorization", report.ID)
	evidenceRefs := append([]string{}, report.EvidenceRefs...)
	evidenceRefs = append(evidenceRefs,
		"product_release_plan:"+plan.ID,
		"product_release_authorization:"+report.ID,
		"product_release_review_transition:"+report.TransitionID,
		"compound://reviews/"+ProductReleaseReviewArea+"/"+filepath.Base(reviewedPath),
	)
	proof := fmt.Sprintf("A governed completed outward_delta Receipt for %s is linked to a later ProductSignal whose metric_name is exactly %s", workID, plan.PostReleaseMetric)
	boundary := "An external store mutation requires the exact recorded authorization plus a separately allowing PolicyDecision. Stop before execution when identity, candidate, checks, rollback, or provider preflight differs."
	return ProductReleaseWorkDraft{
		SchemaVersion:     ProductReleaseWorkDraftSchemaVersion,
		ID:                stableID("prwd_", plan.ID, report.ID),
		Status:            "work_plan_ready",
		ProductID:         plan.ProductID,
		PlanID:            plan.ID,
		AuthorizationID:   report.ID,
		ReviewID:          report.ReviewID,
		ReleaseIdentity:   plan.ReleaseIdentity,
		Mechanism:         plan.Mechanism,
		ChangeSummary:     plan.ChangeSummary,
		CandidateRefs:     append([]string{}, plan.CandidateRefs...),
		Rollback:          plan.Rollback,
		PostReleaseMetric: plan.PostReleaseMetric,
		RequiredChecks:    append([]string{}, plan.RequiredChecks...),
		RequiresOperator:  true,
		Boundary:          boundary,
		Proof:             proof,
		WorkID:            workID,
		WorkOwner:         plan.ProductID,
		SuccessPredicate:  proof,
		DesiredOutcome:    fmt.Sprintf("Release exactly %s version %s build %s through %s, then return metric %s as a ProductSignal", plan.ProductID, plan.ReleaseIdentity.Version, plan.ReleaseIdentity.Build, plan.Mechanism, plan.PostReleaseMetric),
		NonGoals: []string{
			"do not treat release authorization as external execution",
			"do not substitute another product, candidate, version, build, or destination",
			"do not execute without a separately allowing PolicyDecision",
			"do not claim release success without a governed completed outward_delta Receipt",
			"do not close the product loop with a different or pre-authorization metric signal",
		},
		EvidenceRefs: reviewartifact.UniqueSorted(compact(evidenceRefs)),
	}, nil
}

func LoadProductReleaseReviewReport(path string) (ProductReleaseReviewReport, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return ProductReleaseReviewReport{}, err
	}
	var report ProductReleaseReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return ProductReleaseReviewReport{}, err
	}
	if report.SchemaVersion != ProductReleaseReviewReportSchemaVersion {
		return ProductReleaseReviewReport{}, fmt.Errorf("schema_version must be %s, got %q", ProductReleaseReviewReportSchemaVersion, report.SchemaVersion)
	}
	if strings.TrimSpace(report.ID) == "" || strings.TrimSpace(report.PlanID) == "" || strings.TrimSpace(report.ProductID) == "" {
		return ProductReleaseReviewReport{}, fmt.Errorf("product release review report id plan_id product_id required")
	}
	return report, nil
}

// ObserveProductRelease closes signal -> authorized action -> governed release
// -> returned signal only when every identity and time edge is proven.
func ObserveProductRelease(homePath string, input ReleaseObservationInput, now time.Time) (ProductReleaseObservationReport, error) {
	homePath = strings.TrimSpace(homePath)
	input.ReviewedPath = strings.TrimSpace(input.ReviewedPath)
	input.ExecutionRef = strings.TrimSpace(input.ExecutionRef)
	input.SignalID = strings.TrimSpace(input.SignalID)
	if homePath == "" || input.ReviewedPath == "" || input.ExecutionRef == "" || input.SignalID == "" {
		return ProductReleaseObservationReport{}, fmt.Errorf("COMPOUND_HOME reviewed_path execution_ref signal_id required")
	}
	report, err := LoadProductReleaseReviewReport(input.ReviewedPath)
	if err != nil {
		return ProductReleaseObservationReport{}, err
	}
	draft, err := BuildReleaseWorkDraft(homePath, input.ReviewedPath)
	if err != nil {
		return ProductReleaseObservationReport{}, err
	}
	plan, err := loadReleasePlan(homePath, report.PlanID)
	if err != nil {
		return ProductReleaseObservationReport{}, err
	}
	receipt, err := loadGovernedReleaseReceipt(homePath, input.ExecutionRef, draft)
	if err != nil {
		return ProductReleaseObservationReport{}, err
	}
	signal, err := loadSignalByID(homePath, input.SignalID)
	if err != nil {
		return ProductReleaseObservationReport{}, err
	}
	if signal.ProductID != plan.ProductID {
		return ProductReleaseObservationReport{}, fmt.Errorf("post-release signal belongs to %s, want %s", signal.ProductID, plan.ProductID)
	}
	if signal.MetricName != plan.PostReleaseMetric {
		return ProductReleaseObservationReport{}, fmt.Errorf("post-release signal metric %q does not match planned post-release metric %q", signal.MetricName, plan.PostReleaseMetric)
	}
	authorizedAt, err := time.Parse(time.RFC3339Nano, report.CreatedAt)
	if err != nil {
		return ProductReleaseObservationReport{}, fmt.Errorf("release authorization created_at invalid: %w", err)
	}
	executedAt, err := time.Parse(time.RFC3339Nano, receipt.CreatedAt)
	if err != nil {
		return ProductReleaseObservationReport{}, fmt.Errorf("release receipt created_at invalid: %w", err)
	}
	observedAt, err := time.Parse(time.RFC3339Nano, signal.ObservedAt)
	if err != nil {
		return ProductReleaseObservationReport{}, fmt.Errorf("post-release signal observed_at required and must be RFC3339: %w", err)
	}
	if executedAt.Before(authorizedAt) {
		return ProductReleaseObservationReport{}, fmt.Errorf("release receipt predates authorization")
	}
	if observedAt.Before(executedAt) {
		return ProductReleaseObservationReport{}, fmt.Errorf("post-release signal predates governed release execution")
	}
	if !containsString(signal.EvidenceRefs, receipt.ID) && !containsString(signal.EvidenceRefs, "receipt:"+receipt.ID) {
		return ProductReleaseObservationReport{}, fmt.Errorf("post-release signal does not cite governed receipt %s", receipt.ID)
	}
	id := stableID("prlo_", report.ID, receipt.ID, signal.ID)
	evidenceRefs := reviewartifact.UniqueSorted(compact(append(append(append([]string{}, report.EvidenceRefs...), receipt.ProofRefs...), signal.EvidenceRefs...)))
	evidenceRefs = reviewartifact.UniqueSorted(append(evidenceRefs,
		"product_release_authorization:"+report.ID,
		"receipt:"+receipt.ID,
		"product_signal:"+signal.ID,
	))
	transition, err := BuildLoopTransition(LoopTransitionInput{
		Kind:         "product_release_observation",
		ProductID:    plan.ProductID,
		ObjectID:     id,
		SourceTrace:  receipt.ID,
		BeforeRefs:   []string{"product_release_authorization:" + report.ID, report.TransitionID},
		AfterRefs:    []string{"product_signal:" + signal.ID},
		EvidenceRefs: evidenceRefs,
		Summary:      fmt.Sprintf("authorized release %s executed by governed receipt %s and returned planned signal %s (%s)", report.ID, receipt.ID, signal.ID, signal.MetricName),
	}, now)
	if err != nil {
		return ProductReleaseObservationReport{}, err
	}
	if err := RecordTransition(homePath, transition); err != nil {
		return ProductReleaseObservationReport{}, err
	}
	observed := ProductReleaseObservationReport{
		SchemaVersion:      ProductReleaseObservationSchemaVersion,
		ID:                 id,
		CreatedAt:          now.UTC().Format(time.RFC3339),
		Status:             "loop_closed",
		ProductID:          plan.ProductID,
		PlanID:             plan.ID,
		AuthorizationID:    report.ID,
		AuthorizationAt:    report.CreatedAt,
		WorkID:             receipt.IDs.WorkID,
		ExecutionReceiptID: receipt.ID,
		ExecutedAt:         receipt.CreatedAt,
		SignalID:           signal.ID,
		MetricName:         signal.MetricName,
		ObservedAt:         signal.ObservedAt,
		TransitionID:       transition.ID,
		EvidenceRefs:       evidenceRefs,
	}
	name := observed.ID + ".observed.json"
	path, err := reviewartifact.WriteJSON(homePath, ProductReleaseReviewArea, name, observed, " ")
	if err != nil {
		return ProductReleaseObservationReport{}, err
	}
	observed.ReportPath = path
	return observed, nil
}

func loadGovernedReleaseReceipt(homePath, ref string, draft ProductReleaseWorkDraft) (envelope.Receipt, error) {
	if !strings.HasPrefix(ref, "rec_") || strings.ContainsAny(ref, `/\\`) {
		return envelope.Receipt{}, fmt.Errorf("governed release receipt ref must be a rec_ id")
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return envelope.Receipt{}, err
	}
	record, err := events.ReadRegular(ref, "receipt")
	if err != nil {
		return envelope.Receipt{}, fmt.Errorf("governed release receipt unreadable: %w", err)
	}
	var receipt envelope.Receipt
	if err := json.Unmarshal(record.Bytes, &receipt); err != nil {
		return envelope.Receipt{}, fmt.Errorf("governed release receipt invalid: %w", err)
	}
	if err := receipt.Validate(); err != nil || receipt.ID != ref {
		if err == nil {
			err = fmt.Errorf("identity mismatch")
		}
		return envelope.Receipt{}, fmt.Errorf("governed release receipt invalid: %w", err)
	}
	if receipt.Outcome != "completed" || receipt.MoveType != "outward_delta" || !receipt.StateDelta.Changed {
		return envelope.Receipt{}, fmt.Errorf("governed release receipt must be completed outward_delta with changed=true")
	}
	if receipt.IDs.WorkID != draft.WorkID {
		return envelope.Receipt{}, fmt.Errorf("governed release receipt belongs to work %s, want %s", receipt.IDs.WorkID, draft.WorkID)
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		return envelope.Receipt{}, err
	}
	w, lifecycle, err := workStore.Show(receipt.IDs.WorkID)
	if err != nil {
		return envelope.Receipt{}, fmt.Errorf("governed release work unreadable: %w", err)
	}
	if !containsString(w.EvidenceRefs, "product_release_authorization:"+draft.AuthorizationID) {
		return envelope.Receipt{}, fmt.Errorf("governed release work does not carry exact authorization")
	}
	var actionID, policyID, transitionID string
	for _, event := range lifecycle {
		if !containsString(event.Attached, receipt.ID) {
			continue
		}
		for _, attached := range event.Attached {
			switch {
			case strings.HasPrefix(attached, "act_"):
				actionID = attached
			case strings.HasPrefix(attached, "pol_"):
				policyID = attached
			case strings.HasPrefix(attached, "tr_"):
				transitionID = attached
			}
		}
		break
	}
	if actionID == "" || policyID == "" || transitionID == "" {
		return envelope.Receipt{}, fmt.Errorf("receipt is not linked by one governed action lifecycle event")
	}
	var action envelope.ActionIntent
	if err := readReleaseEnvelope(events, actionID, &action); err != nil {
		return envelope.Receipt{}, err
	}
	var decision envelope.PolicyDecision
	if err := readReleaseEnvelope(events, policyID, &decision); err != nil {
		return envelope.Receipt{}, err
	}
	var transition envelope.Transition
	if err := readReleaseEnvelope(events, transitionID, &transition); err != nil {
		return envelope.Receipt{}, err
	}
	if action.IDs.WorkID != draft.WorkID ||
		action.Scope.ProductID != draft.ProductID ||
		action.Capability != "product.release.execute" ||
		action.Target.Kind != "app_store_release" ||
		action.Target.ID != draft.PlanID ||
		(action.MutationClass != "public" && action.MutationClass != "external_side_effect") {
		return envelope.Receipt{}, fmt.Errorf("governed release action does not describe exact external product release")
	}
	if decision.IDs.WorkID != draft.WorkID || decision.ActionIntentID != action.ID || (decision.Outcome != "allow" && decision.Outcome != "allow_with_obligations") {
		return envelope.Receipt{}, fmt.Errorf("governed release action lacks allowing PolicyDecision")
	}
	if transition.IDs.WorkID != draft.WorkID || !containsString(transition.ActionRefs, action.ID) || !containsString(transition.AfterRefs, receipt.ID) {
		return envelope.Receipt{}, fmt.Errorf("governed release transition does not link action and receipt")
	}
	if !containsString(receipt.ProofRefs, "product_release_authorization:"+draft.AuthorizationID) {
		return envelope.Receipt{}, fmt.Errorf("governed release receipt does not cite exact authorization")
	}
	return receipt, nil
}

func readReleaseEnvelope(store *eventstore.Store, ref string, target envelope.Envelope) error {
	record, err := store.ReadRegular(ref, target.Kind())
	if err != nil {
		return fmt.Errorf("governed release %s unreadable: %w", target.Kind(), err)
	}
	if err := json.Unmarshal(record.Bytes, target); err != nil {
		return fmt.Errorf("governed release %s invalid: %w", target.Kind(), err)
	}
	if err := target.Validate(); err != nil {
		return fmt.Errorf("governed release %s invalid: %w", target.Kind(), err)
	}
	return nil
}

func ValidateReleaseReview(review ProductReleaseReview) error {
	if review.SchemaVersion != ProductReleaseReviewSchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", ProductReleaseReviewSchemaVersion, review.SchemaVersion)
	}
	if strings.TrimSpace(review.ID) == "" || strings.TrimSpace(review.ProductID) == "" || strings.TrimSpace(review.PlanID) == "" {
		return fmt.Errorf("product release review id product_id plan_id required")
	}
	if strings.ContainsAny(review.ID+review.PlanID, `/\`) {
		return fmt.Errorf("product release review ids must not contain path separators")
	}
	if !oneOf(review.Decision, []string{"authorize_release", "hold", "reject"}) || strings.Contains(review.Decision, "TODO") {
		return fmt.Errorf("product release review decision must be authorize_release|hold|reject")
	}
	if strings.TrimSpace(review.Rationale) == "" || strings.Contains(review.Rationale, "TODO") {
		return fmt.Errorf("product release review rationale required")
	}
	if strings.TrimSpace(review.ReviewedBy) == "" || strings.Contains(review.ReviewedBy, "TODO") {
		return fmt.Errorf("product release review reviewed_by required")
	}
	if review.Confidence < 0 || review.Confidence > 1 {
		return fmt.Errorf("product release review confidence must be between 0 and 1")
	}
	if len(compact(review.EvidenceRefs)) == 0 {
		return fmt.Errorf("product release review evidence_refs required")
	}
	for _, ref := range append(append([]string{}, review.CompletedChecks...), review.EvidenceRefs...) {
		if strings.Contains(ref, "TODO") {
			return fmt.Errorf("product release review checks/evidence must be completed")
		}
	}
	return nil
}

func loadReleasePlan(homePath, planID string) (ProductReleasePlan, error) {
	if strings.TrimSpace(planID) == "" || strings.ContainsAny(planID, `/\`) {
		return ProductReleasePlan{}, fmt.Errorf("product release plan id invalid")
	}
	path := filepath.Join(homePath, "reviews", ProductReleaseReviewArea, planID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ProductReleasePlan{}, err
	}
	var plan ProductReleasePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ProductReleasePlan{}, err
	}
	if plan.ID != planID || plan.SchemaVersion != ProductReleasePlanSchemaVersion {
		return ProductReleasePlan{}, fmt.Errorf("product release plan identity/schema mismatch")
	}
	return plan, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func annotateProductReleaseItem(item *reviewartifact.QueueItem) {
	if report, ok := productReleaseReviewedFromExtras(item.ExtraPaths); ok {
		item.Status = report.Status
		item.Readiness = "no_action"
		item.BlockedReason = ""
		item.NextAction = report.NextAction
		item.Actions = nil
		item.DecisionCard = nil
		return
	}
	plan, err := productReleasePlanFromQueueItem(*item)
	if err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "product release plan unreadable"
		item.NextAction = "regenerate product release plan"
		item.Actions = nil
		return
	}
	reviewPath := strings.TrimSuffix(item.JSONPath, ".json") + ".review.yaml"
	if _, err := os.Stat(reviewPath); err != nil {
		item.Readiness = "blocked"
		item.BlockedReason = "product release review YAML missing"
		item.NextAction = "regenerate product release plan"
		item.Actions = nil
		return
	}
	argv := []string{"compound", "product", "release-review", reviewPath}
	item.Readiness = "needs_review"
	item.BlockedReason = "external release authority requires an explicit product-owner decision"
	item.NextAction = "verify the named checks, edit review YAML, then record release authorization"
	item.Actions = []reviewartifact.Action{{
		ID:             "record_product_release_review",
		Label:          "Record product release decision",
		Command:        shellJoin(argv),
		Argv:           argv,
		RequiresReview: true,
	}}
	item.DecisionCard = &reviewartifact.DecisionCard{
		Claim: fmt.Sprintf("Release %s version %s build %s through %s", plan.ProductID, plan.ReleaseIdentity.Version, plan.ReleaseIdentity.Build, plan.Mechanism),
		Why: []reviewartifact.DecisionCardReason{
			{Label: "Change", Detail: plan.ChangeSummary},
			{Label: "Rollback", Detail: plan.Rollback},
			{Label: "Post-release signal", Detail: plan.PostReleaseMetric},
		},
		Choices: []reviewartifact.DecisionCardChoice{
			{ID: "authorize_release", Label: "Authorize release", Effect: "authorize only this identity and candidate; execution remains separately governed"},
			{ID: "hold", Label: "Hold", Effect: "keep the candidate visible without release authority"},
			{ID: "reject", Label: "Reject", Effect: "record that this candidate must not release"},
		},
		Boundary:            []string{plan.Boundary},
		CanonicalActionHint: shellJoin(argv),
	}
}

func productReleasePlanFromQueueItem(item reviewartifact.QueueItem) (ProductReleasePlan, error) {
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return ProductReleasePlan{}, err
	}
	var plan ProductReleasePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return ProductReleasePlan{}, err
	}
	if plan.SchemaVersion != ProductReleasePlanSchemaVersion || plan.ID == "" {
		return ProductReleasePlan{}, fmt.Errorf("not a product release plan")
	}
	return plan, nil
}

func productReleaseReviewedFromExtras(paths []string) (ProductReleaseReviewReport, bool) {
	var found ProductReleaseReviewReport
	for _, path := range paths {
		if !strings.HasSuffix(path, ".reviewed.json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var report ProductReleaseReviewReport
		if err := json.Unmarshal(data, &report); err != nil || report.SchemaVersion != ProductReleaseReviewReportSchemaVersion {
			continue
		}
		if found.ID == "" || report.CreatedAt > found.CreatedAt {
			found = report
		}
	}
	return found, found.ID != ""
}
