package routeevidence

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/reviewartifact"

	"gopkg.in/yaml.v3"
)

const (
	PromotionCriteriaSchemaVersion          = "compound.promotion_criteria.v0.1"
	PromotionCriteriaSourceAuthoritySchema  = "compound.promotion_criteria_source_authority.v0.1"
	PromotionReviewSchemaVersion            = "compound.route_promotion_review.v0.1"
	PromotionCriteriaAuthorityRef           = "compound://registries/promotion-criteria"
	promotionCriteriaFile                   = "promotion-criteria.yaml"
	promotionCriteriaSourceAuthorityFile    = "promotion-criteria-source-authority.jsonl"
	promotionCriteriaImportCommandName      = "route import-promotion-criteria"
	routePromotionReviewArea                = "route-promotions"
	promotionRecommendationPromote          = "promote"
	promotionRecommendationDemote           = "demote"
	promotionRecommendationHoldInsufficient = "hold_insufficient_evidence"
	promotionRecommendationHoldFailing      = "hold_failing"
	promotionMalformedActionShapeRepair     = "shape_repair"
	promotionReviewStatusReviewRequired     = "review_required"
	// promotionReviewStatusFiled marks a route-promotion review as recorded
	// provenance only, not operator review-debt. It MUST stay one of the terminal
	// statuses in internal/director reviewStatusTerminal so demote and hold_failing
	// authority outcomes — which have no operator-resolvable recording path
	// (RecordPromotionHoldReview records only hold_insufficient_evidence) — do not
	// strand the director's review-debt queue forever.
	promotionReviewStatusFiled = "recorded"
)

type PromotionCriteriaRegistry struct {
	SchemaVersion string               `json:"schema_version" yaml:"schema_version"`
	Entries       []PromotionCriterion `json:"entries" yaml:"entries"`
}

type PromotionCriterion struct {
	LaneID           string  `json:"lane_id" yaml:"lane_id"`
	CapabilityClass  string  `json:"capability_class" yaml:"capability_class"`
	MinSamples       int     `json:"min_samples" yaml:"min_samples"`
	MaxErrorRate     float64 `json:"max_error_rate" yaml:"max_error_rate"`
	MaxMalformedRate float64 `json:"max_malformed_rate" yaml:"max_malformed_rate"`
	MalformedAction  string  `json:"malformed_action,omitempty" yaml:"malformed_action"`
	MinWindowDays    int     `json:"min_window_days" yaml:"min_window_days"`
	Notes            string  `json:"notes,omitempty" yaml:"notes,omitempty"`
}

type PromotionCriteriaImportReport struct {
	AuthorityRef      string                           `json:"authority_ref"`
	AuthorityPath     string                           `json:"authority_path"`
	SourceAuthorityID string                           `json:"source_authority_id"`
	SourceAuthority   PromotionCriteriaSourceAuthority `json:"source_authority"`
	EntryCount        int                              `json:"entry_count"`
	CapabilityClasses []string                         `json:"capability_classes"`
	LaneIDs           []string                         `json:"lane_ids"`
}

type PromotionCriteriaSourceAuthority struct {
	SchemaVersion       string   `json:"schema_version"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	SourcePath          string   `json:"source_path"`
	SourceRevision      string   `json:"source_revision"`
	ImportedVia         string   `json:"imported_via"`
	AuthorityRef        string   `json:"authority_ref"`
	AuthorityStatus     string   `json:"authority_status"`
	SourceRole          string   `json:"source_role"`
	Retires             string   `json:"retires"`
	RetirementCondition string   `json:"retirement_condition"`
	Rollback            string   `json:"rollback"`
	EntryCount          int      `json:"entry_count"`
	LaneIDs             []string `json:"lane_ids"`
	CapabilityClasses   []string `json:"capability_classes"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

type PromotionReview struct {
	SchemaVersion                 string               `json:"schema_version"`
	ID                            string               `json:"id"`
	CreatedAt                     string               `json:"created_at"`
	Status                        string               `json:"status"`
	Recommendation                string               `json:"recommendation"`
	Reason                        string               `json:"reason"`
	LaneID                        string               `json:"lane_id"`
	Lane                          string               `json:"lane,omitempty"`
	CapabilityClass               string               `json:"capability_class"`
	CurrentClaimLevel             string               `json:"current_claim_level"`
	RecommendedClaimLevel         string               `json:"recommended_claim_level,omitempty"`
	ClaimEvidence                 string               `json:"claim_evidence,omitempty"`
	ProposedChange                string               `json:"proposed_change,omitempty"`
	Criteria                      PromotionCriterion   `json:"criteria"`
	CriteriaMatched               bool                 `json:"criteria_matched"`
	Tallies                       PromotionTallies     `json:"tallies"`
	EvidenceRefs                  []string             `json:"evidence_refs"`
	MatchingSemantics             []string             `json:"matching_semantics"`
	MatchingLimitations           []string             `json:"matching_limitations"`
	ModelCapabilitiesAuthorityRef string               `json:"model_capabilities_authority_ref"`
	ModelCapabilitiesPath         string               `json:"model_capabilities_path"`
	ModelCapabilitiesSourceID     string               `json:"model_capabilities_source_id,omitempty"`
	ModelCapabilitiesRevision     string               `json:"model_capabilities_revision,omitempty"`
	CriteriaAuthorityRef          string               `json:"criteria_authority_ref"`
	CriteriaPath                  string               `json:"criteria_path"`
	CriteriaSourceID              string               `json:"criteria_source_id,omitempty"`
	CriteriaRevision              string               `json:"criteria_revision,omitempty"`
	ArtifactPaths                 reviewartifact.Paths `json:"-"`
}

type PromotionTallies struct {
	Samples                 int      `json:"samples"`
	FailedStatuses          int      `json:"failed_statuses"`
	FailedStatusRate        float64  `json:"failed_status_rate"`
	FirstSeen               string   `json:"first_seen,omitempty"`
	LastSeen                string   `json:"last_seen,omitempty"`
	WindowDays              float64  `json:"window_days"`
	PanelJudgeRuns          int      `json:"panel_judge_runs"`
	PanelJudgeFindings      int      `json:"panel_judge_findings"`
	JudgeJSONMalformed      int      `json:"judge_json_malformed"`
	JudgeJSONMalformedRate  float64  `json:"judge_json_malformed_rate"`
	ComparisonWins          int      `json:"comparison_wins"`
	ComparisonLosses        int      `json:"comparison_losses"`
	MatchedRouteEvidenceIDs []string `json:"matched_route_evidence_ids,omitempty"`
	MatchedPanelRunIDs      []string `json:"matched_panel_run_ids,omitempty"`
}

func PromotionCriteriaPath(homePath string) string {
	return filepath.Join(homePath, "registries", promotionCriteriaFile)
}

func PromotionCriteriaSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", promotionCriteriaSourceAuthorityFile)
}

func ImportPromotionCriteria(homePath, sourcePath string, now time.Time) (PromotionCriteriaImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return PromotionCriteriaImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(sourcePath) == "" {
		return PromotionCriteriaImportReport{}, fmt.Errorf("source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return PromotionCriteriaImportReport{}, fmt.Errorf("read promotion criteria %s: %w", sourcePath, err)
	}
	registry, err := DecodePromotionCriteriaYAML(data)
	if err != nil {
		return PromotionCriteriaImportReport{}, err
	}
	if err := validatePromotionCriteria(registry); err != nil {
		return PromotionCriteriaImportReport{}, err
	}
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return PromotionCriteriaImportReport{}, err
	}
	authorityPath := PromotionCriteriaPath(homePath)
	if err := os.WriteFile(authorityPath, data, 0o644); err != nil {
		return PromotionCriteriaImportReport{}, err
	}
	revision := "sha256:" + digestHex(data)
	laneIDs := promotionCriteriaLaneIDs(registry)
	classes := promotionCriteriaClasses(registry)
	rec := PromotionCriteriaSourceAuthority{
		SchemaVersion:       PromotionCriteriaSourceAuthoritySchema,
		ID:                  promotionCriteriaSourceAuthorityID(sourcePath, revision),
		CreatedAt:           rfc3339(now),
		SourcePath:          sourcePath,
		SourceRevision:      revision,
		ImportedVia:         promotionCriteriaImportCommandName,
		AuthorityRef:        PromotionCriteriaAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "source_view_authority:" + sourcePath,
		RetirementCondition: "compound route promotion-review reads compound://registries/promotion-criteria from COMPOUND_HOME; source view remains rollback input only",
		Rollback:            "remove " + authorityPath + " and rerun " + promotionCriteriaImportCommandName + " with a reviewed criteria file",
		EntryCount:          len(registry.Entries),
		LaneIDs:             laneIDs,
		CapabilityClasses:   classes,
		EvidenceRefs:        []string{PromotionCriteriaAuthorityRef},
	}
	if err := appendPromotionCriteriaSourceAuthority(homePath, rec); err != nil {
		return PromotionCriteriaImportReport{}, err
	}
	return PromotionCriteriaImportReport{
		AuthorityRef:      PromotionCriteriaAuthorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: rec.ID,
		SourceAuthority:   rec,
		EntryCount:        len(registry.Entries),
		CapabilityClasses: classes,
		LaneIDs:           laneIDs,
	}, nil
}

func DecodePromotionCriteriaYAML(data []byte) (PromotionCriteriaRegistry, error) {
	var registry PromotionCriteriaRegistry
	if err := yaml.Unmarshal(data, &registry); err != nil {
		return PromotionCriteriaRegistry{}, fmt.Errorf("parse promotion criteria: %w", err)
	}
	return registry, nil
}

func LoadPromotionCriteria(homePath string) (PromotionCriteriaRegistry, string, error) {
	if strings.TrimSpace(homePath) == "" {
		return PromotionCriteriaRegistry{}, "", fmt.Errorf("COMPOUND_HOME required")
	}
	path := PromotionCriteriaPath(homePath)
	data, err := os.ReadFile(path)
	if err != nil {
		return PromotionCriteriaRegistry{}, path, err
	}
	registry, err := DecodePromotionCriteriaYAML(data)
	if err != nil {
		return PromotionCriteriaRegistry{}, path, err
	}
	if err := validatePromotionCriteria(registry); err != nil {
		return PromotionCriteriaRegistry{}, path, err
	}
	return registry, path, nil
}

func LoadPromotionCriteriaSourceAuthority(homePath string) ([]PromotionCriteriaSourceAuthority, error) {
	data, err := os.ReadFile(PromotionCriteriaSourceAuthorityPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []PromotionCriteriaSourceAuthority
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec PromotionCriteriaSourceAuthority
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse promotion criteria source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	return records, nil
}

func EvaluateLanePromotions(homePath string, now time.Time) ([]PromotionReview, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, fmt.Errorf("COMPOUND_HOME required")
	}
	models, modelPath, modelAuthorityRevision, err := loadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return nil, fmt.Errorf("load model capabilities authority: %w", err)
	}
	criteria, criteriaPath, err := LoadPromotionCriteria(homePath)
	if err != nil {
		return nil, fmt.Errorf("load promotion criteria authority: %w", err)
	}
	modelSourceID, modelRevision, err := latestModelCapabilitiesSource(homePath, modelAuthorityRevision)
	if err != nil {
		return nil, err
	}
	criteriaSourceID, criteriaRevision, err := latestPromotionCriteriaSource(homePath)
	if err != nil {
		return nil, err
	}
	rows, err := loadPromotionRouteEvidence(homePath)
	if err != nil {
		return nil, err
	}
	panelSignals, err := loadPromotionPanelSignals(homePath)
	if err != nil {
		return nil, err
	}
	comparisonSignals, err := loadPromotionComparisonSignals(homePath)
	if err != nil {
		return nil, err
	}

	var reviews []PromotionReview
	for _, laneID := range modelCapabilityIDs(models) {
		model := models.Models[laneID]
		for _, class := range promotionClaimClasses(model) {
			claim := model.Capabilities[class]
			level := strings.TrimSpace(claim.Level)
			if level != "assumed" && level != "capable" {
				continue
			}
			criterion, matched := promotionCriterionFor(criteria, laneID, class)
			tallies := tallyPromotionEvidence(laneID, rows, panelSignals[laneID], comparisonSignals[promotionComparisonKey{laneID: laneID, capabilityClass: class}])
			review := buildPromotionReview(promotionReviewInput{
				Now:                             now,
				LaneID:                          laneID,
				Model:                           model,
				CapabilityClass:                 class,
				Claim:                           claim,
				Criterion:                       criterion,
				CriteriaMatched:                 matched,
				Tallies:                         tallies,
				ModelCapabilitiesPath:           modelPath,
				ModelCapabilitiesSourceID:       modelSourceID,
				ModelCapabilitiesSourceRevision: modelRevision,
				CriteriaPath:                    criteriaPath,
				CriteriaSourceID:                criteriaSourceID,
				CriteriaSourceRevision:          criteriaRevision,
			})
			paths, err := reviewartifact.Write(homePath, routePromotionReviewArea, review.ID, review, " ", promotionReviewMarkdown(review))
			if err != nil {
				return nil, err
			}
			review.ArtifactPaths = paths
			reviews = append(reviews, review)
		}
	}
	sort.Slice(reviews, func(i, j int) bool {
		if reviews[i].LaneID != reviews[j].LaneID {
			return reviews[i].LaneID < reviews[j].LaneID
		}
		return reviews[i].CapabilityClass < reviews[j].CapabilityClass
	})
	return reviews, nil
}

type promotionReviewInput struct {
	Now                             time.Time
	LaneID                          string
	Model                           ModelCapabilityEntry
	CapabilityClass                 string
	Claim                           CapabilityClaim
	Criterion                       PromotionCriterion
	CriteriaMatched                 bool
	Tallies                         PromotionTallies
	ModelCapabilitiesPath           string
	ModelCapabilitiesSourceID       string
	ModelCapabilitiesSourceRevision string
	CriteriaPath                    string
	CriteriaSourceID                string
	CriteriaSourceRevision          string
}

func buildPromotionReview(in promotionReviewInput) PromotionReview {
	recommendation, targetLevel, reason := promotionRecommendation(strings.TrimSpace(in.Claim.Level), in.CriteriaMatched, in.Criterion, in.Tallies)
	proposed := ""
	if recommendation == promotionRecommendationPromote || recommendation == promotionRecommendationDemote {
		proposed = fmt.Sprintf("In the model-capabilities source registry, change models[%q].capabilities[%q].level from %q to %q after owner review. Compound does not apply this edit.", in.LaneID, in.CapabilityClass, strings.TrimSpace(in.Claim.Level), targetLevel)
	}
	evidenceRefs := append([]string{}, in.Tallies.MatchedRouteEvidenceIDs...)
	evidenceRefs = append(evidenceRefs, in.Tallies.MatchedPanelRunIDs...)
	review := PromotionReview{
		SchemaVersion:                 PromotionReviewSchemaVersion,
		ID:                            promotionReviewID(in.LaneID, in.CapabilityClass),
		CreatedAt:                     rfc3339(in.Now),
		Status:                        promotionReviewStatusFor(recommendation),
		Recommendation:                recommendation,
		Reason:                        reason,
		LaneID:                        in.LaneID,
		Lane:                          strings.TrimSpace(in.Model.Lane),
		CapabilityClass:               in.CapabilityClass,
		CurrentClaimLevel:             strings.TrimSpace(in.Claim.Level),
		RecommendedClaimLevel:         targetLevel,
		ClaimEvidence:                 strings.TrimSpace(in.Claim.Evidence),
		ProposedChange:                proposed,
		Criteria:                      in.Criterion,
		CriteriaMatched:               in.CriteriaMatched,
		Tallies:                       in.Tallies,
		EvidenceRefs:                  reviewartifact.UniqueSorted(evidenceRefs),
		MatchingSemantics:             promotionMatchingSemantics(),
		MatchingLimitations:           promotionMatchingLimitations(),
		ModelCapabilitiesAuthorityRef: ModelCapabilitiesAuthorityRef,
		ModelCapabilitiesPath:         in.ModelCapabilitiesPath,
		ModelCapabilitiesSourceID:     in.ModelCapabilitiesSourceID,
		ModelCapabilitiesRevision:     in.ModelCapabilitiesSourceRevision,
		CriteriaAuthorityRef:          PromotionCriteriaAuthorityRef,
		CriteriaPath:                  in.CriteriaPath,
		CriteriaSourceID:              in.CriteriaSourceID,
		CriteriaRevision:              in.CriteriaSourceRevision,
	}
	if targetLevel == "" {
		review.RecommendedClaimLevel = ""
	}
	return review
}

func promotionRecommendation(level string, criteriaMatched bool, criterion PromotionCriterion, tallies PromotionTallies) (recommendation, targetLevel, reason string) {
	if !criteriaMatched {
		return promotionRecommendationHoldInsufficient, "", "no promotion criteria matched lane/capability or wildcard default"
	}
	if tallies.Samples < criterion.MinSamples {
		return promotionRecommendationHoldInsufficient, "", fmt.Sprintf("samples %d below min_samples %d", tallies.Samples, criterion.MinSamples)
	}
	if tallies.WindowDays < float64(criterion.MinWindowDays) {
		return promotionRecommendationHoldInsufficient, "", fmt.Sprintf("evidence window %.2f days below min_window_days %d", tallies.WindowDays, criterion.MinWindowDays)
	}
	if tallies.FailedStatusRate > criterion.MaxErrorRate {
		if level == "capable" {
			return promotionRecommendationDemote, "assumed", fmt.Sprintf("failing rates exceed criteria: failed_status_rate %.3f <= %.3f, judge_json_malformed_rate %.3f <= %.3f", tallies.FailedStatusRate, criterion.MaxErrorRate, tallies.JudgeJSONMalformedRate, criterion.MaxMalformedRate)
		}
		return promotionRecommendationHoldFailing, "", fmt.Sprintf("failing rates exceed criteria: failed_status_rate %.3f <= %.3f, judge_json_malformed_rate %.3f <= %.3f", tallies.FailedStatusRate, criterion.MaxErrorRate, tallies.JudgeJSONMalformedRate, criterion.MaxMalformedRate)
	}
	malformedBreach := tallies.JudgeJSONMalformedRate > criterion.MaxMalformedRate
	if malformedBreach && strings.TrimSpace(criterion.MalformedAction) != promotionMalformedActionShapeRepair {
		if level == "capable" {
			return promotionRecommendationDemote, "assumed", fmt.Sprintf("failing rates exceed criteria: failed_status_rate %.3f <= %.3f, judge_json_malformed_rate %.3f <= %.3f", tallies.FailedStatusRate, criterion.MaxErrorRate, tallies.JudgeJSONMalformedRate, criterion.MaxMalformedRate)
		}
		return promotionRecommendationHoldFailing, "", fmt.Sprintf("failing rates exceed criteria: failed_status_rate %.3f <= %.3f, judge_json_malformed_rate %.3f <= %.3f", tallies.FailedStatusRate, criterion.MaxErrorRate, tallies.JudgeJSONMalformedRate, criterion.MaxMalformedRate)
	}
	malformedReason := ""
	if malformedBreach {
		malformedReason = fmt.Sprintf("; malformed_rate %.3f exceeds %.3f treated as repairable shape defect (meaning>shape) - route outputs via shape-repair pass", tallies.JudgeJSONMalformedRate, criterion.MaxMalformedRate)
	}
	switch level {
	case "assumed":
		return promotionRecommendationPromote, "capable", "criteria satisfied for assumed -> capable" + malformedReason
	case "capable":
		return promotionRecommendationPromote, "primary", "criteria satisfied for capable -> primary" + malformedReason
	default:
		return promotionRecommendationHoldInsufficient, "", "claim level is not promotion-eligible" + malformedReason
	}
}

// promotionReviewStatusFor picks the emitted status for a route-promotion review.
// promote and hold_insufficient_evidence surface as operator review-debt: promote
// awaits an authority decision brief and hold_insufficient is cleared with
// `compound route promotion-record`. demote and hold_failing are automated
// authority outcomes with no operator-resolvable recording path, so they are
// emitted as recorded provenance — the full artifact is still written to disk —
// instead of stranding the director's review-debt queue forever.
func promotionReviewStatusFor(recommendation string) string {
	if promotionRecommendationIsAuthorityOutcome(recommendation) {
		return promotionReviewStatusFiled
	}
	return promotionReviewStatusReviewRequired
}

// promotionRecommendationIsAuthorityOutcome reports whether a recommendation is an
// automated/authority outcome that carries no operator-resolvable review action.
func promotionRecommendationIsAuthorityOutcome(recommendation string) bool {
	switch strings.TrimSpace(recommendation) {
	case promotionRecommendationDemote, promotionRecommendationHoldFailing:
		return true
	default:
		return false
	}
}

func tallyPromotionEvidence(laneID string, rows []Evidence, panel promotionPanelSignal, comparison promotionComparisonSignal) PromotionTallies {
	var first time.Time
	var last time.Time
	tallies := PromotionTallies{
		PanelJudgeRuns:          panel.JudgeRuns,
		PanelJudgeFindings:      panel.JudgeFindings,
		JudgeJSONMalformed:      panel.JudgeJSONMalformed,
		ComparisonWins:          comparison.Wins,
		ComparisonLosses:        comparison.Losses,
		MatchedPanelRunIDs:      reviewartifact.UniqueSorted(panel.PanelRunRefs),
		MatchedRouteEvidenceIDs: []string{},
	}
	for _, row := range rows {
		if strings.TrimSpace(row.SelectedTarget.Model) != laneID {
			continue
		}
		tallies.Samples++
		if row.ID != "" {
			tallies.MatchedRouteEvidenceIDs = append(tallies.MatchedRouteEvidenceIDs, row.ID)
		}
		if !promotionRouteStatusOK(row.Status) {
			tallies.FailedStatuses++
		}
		seen, err := time.Parse(time.RFC3339, row.CreatedAt)
		if err == nil {
			if first.IsZero() || seen.Before(first) {
				first = seen
			}
			if last.IsZero() || seen.After(last) {
				last = seen
			}
		}
	}
	tallies.MatchedRouteEvidenceIDs = reviewartifact.UniqueSorted(tallies.MatchedRouteEvidenceIDs)
	if tallies.Samples > 0 {
		tallies.FailedStatusRate = float64(tallies.FailedStatuses) / float64(tallies.Samples)
	}
	if !first.IsZero() {
		tallies.FirstSeen = rfc3339(first)
		tallies.LastSeen = rfc3339(last)
		tallies.WindowDays = last.Sub(first).Hours() / 24
	}
	if tallies.PanelJudgeFindings > 0 {
		tallies.JudgeJSONMalformedRate = float64(tallies.JudgeJSONMalformed) / float64(tallies.PanelJudgeFindings)
	}
	return tallies
}

func loadPromotionRouteEvidence(homePath string) ([]Evidence, error) {
	path := filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read route evidence ledger: %w", err)
	}
	defer f.Close()
	var rows []Evidence
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		ev, err := Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode route evidence line %d: %w", line, err)
		}
		rows = append(rows, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan route evidence ledger: %w", err)
	}
	return rows, nil
}

type promotionPanelSignal struct {
	JudgeRuns          int
	JudgeFindings      int
	JudgeJSONMalformed int
	PanelRunRefs       []string
}

type promotionComparisonKey struct {
	laneID          string
	capabilityClass string
}

type promotionComparisonSignal struct {
	Wins   int
	Losses int
}

func canonicalPromotionLaneNodeID(laneID string) (string, bool) {
	return capgraph.AgentLaneNodeIDFromShortLaneID(laneID)
}

type promotionComparisonEvidenceRow struct {
	ComparisonID    string  `json:"comparison_id"`
	CapabilityClass string  `json:"capability_class"`
	BaselineLane    string  `json:"baseline_lane"`
	CandidateLane   string  `json:"candidate_lane"`
	Decision        string  `json:"decision"`
	Confidence      float64 `json:"confidence"`
	RunID           string  `json:"run_id"`
	VerdictArtifact string  `json:"verdict_artifact"`
	CreatedAt       string  `json:"created_at"`
}

type promotionPanelRunFile struct {
	ID    string `json:"id"`
	Route struct {
		JudgeLane struct {
			ModelID string `json:"model_id"`
		} `json:"judge_lane"`
	} `json:"route"`
	Findings []struct {
		Synthesis struct {
			SchemaVersion         string   `json:"schema_version"`
			Decision              string   `json:"decision"`
			GuardrailsApplied     []string `json:"guardrails_applied"`
			JudgeRouteEvidenceRef string   `json:"judge_route_evidence_ref"`
		} `json:"synthesis"`
	} `json:"findings"`
}

func loadPromotionPanelSignals(homePath string) (map[string]promotionPanelSignal, error) {
	dir := filepath.Join(homePath, "panel-runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]promotionPanelSignal{}, nil
		}
		return nil, err
	}
	out := map[string]promotionPanelSignal{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var run promotionPanelRunFile
		if err := json.Unmarshal(data, &run); err != nil {
			return nil, fmt.Errorf("parse panel run %s: %w", path, err)
		}
		laneID := strings.TrimSpace(run.Route.JudgeLane.ModelID)
		if laneID == "" {
			continue
		}
		signal := out[laneID]
		signal.JudgeRuns++
		runRef := "panel-run:" + firstNonEmpty(strings.TrimSpace(run.ID), strings.TrimSuffix(entry.Name(), ".json"))
		signal.PanelRunRefs = append(signal.PanelRunRefs, runRef)
		for _, finding := range run.Findings {
			if !promotionSynthesisPresent(finding.Synthesis.SchemaVersion, finding.Synthesis.Decision, finding.Synthesis.JudgeRouteEvidenceRef, finding.Synthesis.GuardrailsApplied) {
				continue
			}
			signal.JudgeFindings++
			for _, guardrail := range finding.Synthesis.GuardrailsApplied {
				if guardrail == "judge_json_malformed" {
					signal.JudgeJSONMalformed++
				}
			}
		}
		out[laneID] = signal
	}
	for laneID, signal := range out {
		signal.PanelRunRefs = reviewartifact.UniqueSorted(signal.PanelRunRefs)
		out[laneID] = signal
	}
	return out, nil
}

func loadPromotionComparisonSignals(homePath string) (map[promotionComparisonKey]promotionComparisonSignal, error) {
	path := filepath.Join(homePath, "ledger", "comparison-evidence.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[promotionComparisonKey]promotionComparisonSignal{}, nil
		}
		return nil, err
	}
	defer f.Close()
	out := map[promotionComparisonKey]promotionComparisonSignal{}
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row promotionComparisonEvidenceRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse comparison evidence line %d: %w", lineNo, err)
		}
		key := promotionComparisonKey{laneID: strings.TrimSpace(row.CandidateLane), capabilityClass: strings.TrimSpace(row.CapabilityClass)}
		if key.laneID == "" || key.capabilityClass == "" {
			continue
		}
		signal := out[key]
		switch strings.TrimSpace(row.Decision) {
		case "prefer_candidate":
			signal.Wins++
		case "prefer_baseline":
			signal.Losses++
		}
		out[key] = signal
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func promotionSynthesisPresent(schemaVersion, decision, judgeRouteRef string, guardrails []string) bool {
	return strings.TrimSpace(schemaVersion) != "" || strings.TrimSpace(decision) != "" || strings.TrimSpace(judgeRouteRef) != "" || len(guardrails) > 0
}

func promotionRouteStatusOK(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok", "completed", "complete", "success", "succeeded":
		return true
	default:
		return false
	}
}

func promotionCriterionFor(registry PromotionCriteriaRegistry, laneID, class string) (PromotionCriterion, bool) {
	var fallback PromotionCriterion
	hasFallback := false
	for _, criterion := range registry.Entries {
		if strings.TrimSpace(criterion.CapabilityClass) != class {
			continue
		}
		if strings.TrimSpace(criterion.LaneID) == laneID {
			return criterion, true
		}
		if strings.TrimSpace(criterion.LaneID) == "*" {
			fallback = criterion
			hasFallback = true
		}
	}
	return fallback, hasFallback
}

func validatePromotionCriteria(registry PromotionCriteriaRegistry) error {
	var errs []string
	if registry.SchemaVersion != PromotionCriteriaSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version must be %q", PromotionCriteriaSchemaVersion))
	}
	if len(registry.Entries) == 0 {
		errs = append(errs, "entries required")
	}
	seen := map[string]bool{}
	for i, entry := range registry.Entries {
		prefix := fmt.Sprintf("entries[%d]", i)
		laneID := strings.TrimSpace(entry.LaneID)
		class := strings.TrimSpace(entry.CapabilityClass)
		if laneID == "" {
			errs = append(errs, prefix+".lane_id required")
		}
		if class == "" {
			errs = append(errs, prefix+".capability_class required")
		}
		if entry.MinSamples <= 0 {
			errs = append(errs, prefix+".min_samples must be positive")
		}
		if entry.MaxErrorRate < 0 || entry.MaxErrorRate > 1 {
			errs = append(errs, prefix+".max_error_rate must be 0..1")
		}
		if entry.MaxMalformedRate < 0 || entry.MaxMalformedRate > 1 {
			errs = append(errs, prefix+".max_malformed_rate must be 0..1")
		}
		malformedAction := strings.TrimSpace(entry.MalformedAction)
		if malformedAction != "" && malformedAction != promotionMalformedActionShapeRepair {
			errs = append(errs, fmt.Sprintf("%s.malformed_action must be empty or %q", prefix, promotionMalformedActionShapeRepair))
		}
		if entry.MinWindowDays < 0 {
			errs = append(errs, prefix+".min_window_days must be non-negative")
		}
		key := laneID + "|" + class
		if laneID != "" && class != "" {
			if seen[key] {
				errs = append(errs, prefix+" duplicates lane_id/capability_class "+key)
			}
			seen[key] = true
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("promotion criteria invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

func appendPromotionCriteriaSourceAuthority(homePath string, rec PromotionCriteriaSourceAuthority) error {
	path := PromotionCriteriaSourceAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func latestPromotionCriteriaSource(homePath string) (id, revision string, err error) {
	records, err := LoadPromotionCriteriaSourceAuthority(homePath)
	if err != nil {
		return "", "", err
	}
	if len(records) == 0 {
		return "", "", nil
	}
	latest := records[len(records)-1]
	return latest.ID, latest.SourceRevision, nil
}

func promotionReviewMarkdown(review PromotionReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Route Promotion Review\n\n")
	fmt.Fprintf(&b, "- lane: %s\n", review.LaneID)
	fmt.Fprintf(&b, "- capability_class: %s\n", review.CapabilityClass)
	fmt.Fprintf(&b, "- current_claim_level: %s\n", review.CurrentClaimLevel)
	fmt.Fprintf(&b, "- recommendation: %s\n", review.Recommendation)
	fmt.Fprintf(&b, "- reason: %s\n", review.Reason)
	fmt.Fprintf(&b, "- samples: %d\n", review.Tallies.Samples)
	fmt.Fprintf(&b, "- failed_status_rate: %.3f\n", review.Tallies.FailedStatusRate)
	fmt.Fprintf(&b, "- judge_json_malformed_rate: %.3f\n", review.Tallies.JudgeJSONMalformedRate)
	fmt.Fprintf(&b, "- window_days: %.2f\n", review.Tallies.WindowDays)
	if review.ProposedChange != "" {
		fmt.Fprintf(&b, "\n## Proposed Source Edit\n\n%s\n", review.ProposedChange)
	}
	fmt.Fprintf(&b, "\n## Matching Semantics\n\n")
	for _, item := range review.MatchingSemantics {
		fmt.Fprintf(&b, "- %s\n", item)
	}
	if len(review.MatchingLimitations) > 0 {
		fmt.Fprintf(&b, "\n## Limitations\n\n")
		for _, item := range review.MatchingLimitations {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	return b.String()
}

func promotionClaimClasses(model ModelCapabilityEntry) []string {
	classes := make([]string, 0, len(model.Capabilities))
	for class := range model.Capabilities {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	return classes
}

func promotionCriteriaLaneIDs(registry PromotionCriteriaRegistry) []string {
	ids := make([]string, 0, len(registry.Entries))
	for _, entry := range registry.Entries {
		ids = append(ids, entry.LaneID)
	}
	return sortedStrings(ids)
}

func promotionCriteriaClasses(registry PromotionCriteriaRegistry) []string {
	classes := make([]string, 0, len(registry.Entries))
	for _, entry := range registry.Entries {
		classes = append(classes, entry.CapabilityClass)
	}
	return sortedStrings(classes)
}

func promotionCriteriaSourceAuthorityID(path, revision string) string {
	return idFor("pcsrc_", path, revision, promotionCriteriaImportCommandName)
}

func promotionReviewID(laneID, capabilityClass string) string {
	return idFor("route_promotion_", laneID, capabilityClass)
}

func promotionMatchingSemantics() []string {
	return []string{
		"route ledger rows are attributed when execution_route_evidence.selected_target.model equals the model-capabilities lane id",
		"panel malformed judge JSON is attributed from panel-runs/*.json when route.judge_lane.model_id equals the lane id and a finding synthesis guardrail is judge_json_malformed",
		"comparison evidence wins/losses are attributed when ledger/comparison-evidence.jsonl candidate_lane equals lane id and capability_class equals the claim class",
	}
}

func promotionMatchingLimitations() []string {
	return []string{
		"route evidence rows are lane-attributed, not capability-class-specific; the same lane samples are tallied for each assumed/capable claim on that lane",
		"model override evidence refs are not dereferenced because panel route evidence records the planned lane id directly in selected_target.model",
	}
}
