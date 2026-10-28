package capgraph

import (
	"compound/internal/reviewartifact"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	ExternalRepoIntakeReportSchemaVersion     = "compound.capability.external_repo_intake_report.v0.1"
	ExternalRepoIntakeReviewPlanSchemaVersion = "compound.capability.external_repo_intake_review_plan.v0.1"
	ExternalRepoReviewedManifestImportVersion = "compound.capability.external_repo_reviewed_manifest_import.v0.1"
	ExternalRepoIntakeReviewArea              = "external-repo-intake"
	ExternalRepoIntakeAuthorityRef            = "compound://reviews/external-repo-intake"
)

type ExternalRepoIntakeReport struct {
	SchemaVersion                          string                       `json:"schema_version"`
	AuthorityRef                           string                       `json:"authority_ref"`
	Status                                 string                       `json:"status"`
	BatchCount                             int                          `json:"batch_count"`
	LatestBatchID                          string                       `json:"latest_batch_id,omitempty"`
	LatestSummaryPath                      string                       `json:"latest_summary_path,omitempty"`
	MapPath                                string                       `json:"map_path,omitempty"`
	SourceObservationPath                  string                       `json:"source_observation_path,omitempty"`
	CandidateCount                         int                          `json:"candidate_count,omitempty"`
	CountsByDecision                       map[string]int               `json:"counts_by_decision,omitempty"`
	CountsByLane                           map[string]int               `json:"counts_by_lane,omitempty"`
	OpenReviewCount                        int                          `json:"open_review_count"`
	MergeExistingCount                     int                          `json:"merge_existing_count,omitempty"`
	ReferenceOnlyCount                     int                          `json:"reference_only_count,omitempty"`
	SourceObservationID                    string                       `json:"source_observation_id,omitempty"`
	SourceCandidateIDs                     []string                     `json:"source_candidate_ids,omitempty"`
	NoPromoteIDs                           []string                     `json:"no_promote_ids,omitempty"`
	RepoQuery                              string                       `json:"repo_query,omitempty"`
	Matches                                []ExternalRepoIntakeDecision `json:"matches,omitempty"`
	SourceListPath                         string                       `json:"source_list_path,omitempty"`
	SourceListTotal                        int                          `json:"source_list_total,omitempty"`
	SourceListKnown                        int                          `json:"source_list_known,omitempty"`
	SourceListUnknown                      int                          `json:"source_list_unknown,omitempty"`
	SourceListByDecision                   map[string]int               `json:"source_list_counts_by_decision,omitempty"`
	SourceListByLane                       map[string]int               `json:"source_list_counts_by_lane,omitempty"`
	SourceListByOwner                      map[string]int               `json:"source_list_counts_by_owner,omitempty"`
	SourceListByOwnerReviewStatus          map[string]int               `json:"source_list_counts_by_owner_review_status,omitempty"`
	SourceListByCapabilityEvaluationStatus map[string]int               `json:"source_list_counts_by_capability_evaluation_status,omitempty"`
	SourceListByCapabilityGraphStatus      map[string]int               `json:"source_list_counts_by_capability_graph_status,omitempty"`
	SourceListReviewNextMeaning            string                       `json:"source_list_review_next_meaning,omitempty"`
	SourceListEvalNextMeaning              string                       `json:"source_list_evaluation_next_meaning,omitempty"`
	SourceListGraphImportNextMeaning       string                       `json:"source_list_graph_import_next_meaning,omitempty"`
	SourceListReviewNextCount              *int                         `json:"source_list_review_next_count,omitempty"`
	SourceListEvalNextCount                *int                         `json:"source_list_evaluation_next_count,omitempty"`
	SourceListGraphImportNextCount         *int                         `json:"source_list_graph_import_next_count,omitempty"`
	SourceListGraphImportNextOmitted       int                          `json:"source_list_graph_import_next_omitted,omitempty"`
	SourceListReviewNext                   []ExternalRepoSourceListItem `json:"source_list_review_next,omitempty"`
	SourceListEvalNext                     []ExternalRepoSourceListItem `json:"source_list_evaluation_next,omitempty"`
	SourceListGraphImportNext              []ExternalRepoSourceListItem `json:"source_list_graph_import_next,omitempty"`
	SourceListItems                        []ExternalRepoSourceListItem `json:"source_list_items,omitempty"`
	HighValue                              []ExternalRepoIntakeDecision `json:"high_value,omitempty"`
	Recent                                 []ExternalRepoIntakeDecision `json:"recent,omitempty"`
	Boundaries                             []string                     `json:"boundaries"`
}

type ExternalRepoIntakeDecision struct {
	Repo                             string  `json:"repo"`
	Decision                         string  `json:"decision"`
	Lane                             string  `json:"lane,omitempty"`
	Status                           string  `json:"status,omitempty"`
	Owner                            string  `json:"owner,omitempty"`
	NextReviewStep                   string  `json:"next_review_step,omitempty"`
	NotToDo                          string  `json:"not_to_do,omitempty"`
	GraphHint                        string  `json:"graph_hint,omitempty"`
	DuplicateRisk                    float64 `json:"duplicate_risk,omitempty"`
	EvidenceStrength                 float64 `json:"evidence_strength,omitempty"`
	ReviewID                         string  `json:"review_id,omitempty"`
	ReviewPath                       string  `json:"review_path,omitempty"`
	OwnerReviewStatus                string  `json:"owner_review_status,omitempty"`
	OwnerReviewPath                  string  `json:"owner_review_path,omitempty"`
	OwnerReviewDecision              string  `json:"owner_review_decision,omitempty"`
	OwnerReviewNextAction            string  `json:"owner_review_next_action,omitempty"`
	CapabilityEvaluationStatus       string  `json:"capability_evaluation_status,omitempty"`
	CapabilityEvaluationPath         string  `json:"capability_evaluation_path,omitempty"`
	CapabilityEvaluationDecision     string  `json:"capability_evaluation_decision,omitempty"`
	CapabilityEvaluationNodeID       string  `json:"capability_evaluation_node_id,omitempty"`
	CapabilityEvaluationNodeStatus   string  `json:"capability_evaluation_node_status,omitempty"`
	CapabilityEvaluationManifestPath string  `json:"capability_evaluation_manifest_path,omitempty"`
	CapabilityEvaluationNextAction   string  `json:"capability_evaluation_next_action,omitempty"`
	CapabilityGraphStatus            string  `json:"capability_graph_status,omitempty"`
}

type ExternalRepoSourceListItem struct {
	Query      string                       `json:"query"`
	Normalized string                       `json:"normalized,omitempty"`
	Status     string                       `json:"status"`
	MatchCount int                          `json:"match_count"`
	Matches    []ExternalRepoIntakeDecision `json:"matches,omitempty"`
}

type externalRepoIntakeRunSummary struct {
	SchemaVersion         string                       `json:"schema_version"`
	ID                    string                       `json:"id"`
	CreatedAt             string                       `json:"created_at"`
	MapPath               string                       `json:"map_path"`
	SourceObservationPath string                       `json:"source_observation_path"`
	CandidateCount        int                          `json:"candidate_count"`
	SourceObserveOutput   []string                     `json:"source_observe_output"`
	Decisions             []ExternalRepoIntakeDecision `json:"decisions"`
	CountsByDecision      map[string]int               `json:"counts_by_decision"`
	CountsByLane          map[string]int               `json:"counts_by_lane"`
}

type externalRepoIntakeMap struct {
	Boundaries []string                    `json:"boundaries"`
	Items      []externalRepoIntakeMapItem `json:"items"`
}

type externalRepoIntakeMapItem struct {
	Repo           string `json:"repo"`
	Owner          string `json:"owner"`
	NextReviewStep string `json:"next_review_step"`
	NotToDo        string `json:"not_to_do"`
}

type externalRepoIntakeSummaryFile struct {
	path    string
	summary externalRepoIntakeRunSummary
	created time.Time
}

type ExternalRepoIntakeReviewPlanReport struct {
	SchemaVersion     string                             `json:"schema_version"`
	Status            string                             `json:"status"`
	CreatedAt         string                             `json:"created_at"`
	AuthorityRef      string                             `json:"authority_ref"`
	ReviewArea        string                             `json:"review_area"`
	SourceListPath    string                             `json:"source_list_path"`
	SourceListTotal   int                                `json:"source_list_total,omitempty"`
	SourceListKnown   int                                `json:"source_list_known,omitempty"`
	SourceListUnknown int                                `json:"source_list_unknown,omitempty"`
	LatestBatchID     string                             `json:"latest_batch_id,omitempty"`
	WroteCount        int                                `json:"wrote_count"`
	Items             []ExternalRepoIntakeReviewPlanItem `json:"items,omitempty"`
	Boundaries        []string                           `json:"boundaries,omitempty"`
}

type ExternalRepoIntakeReviewPlanItem struct {
	ID                    string                              `json:"id"`
	Status                string                              `json:"status"`
	CreatedAt             string                              `json:"created_at"`
	Readiness             string                              `json:"readiness"`
	SourceKind            string                              `json:"source_kind"`
	SourcePath            string                              `json:"source_path"`
	TargetPath            string                              `json:"target_path,omitempty"`
	SourceListQuery       string                              `json:"source_list_query"`
	NormalizedRepo        string                              `json:"normalized_repo"`
	Repo                  string                              `json:"repo"`
	Decision              string                              `json:"decision"`
	Lane                  string                              `json:"lane,omitempty"`
	Owner                 string                              `json:"owner,omitempty"`
	NextAction            string                              `json:"next_action,omitempty"`
	NotToDo               string                              `json:"not_to_do,omitempty"`
	GraphDecisionHint     string                              `json:"graph_decision_hint,omitempty"`
	GraphEvidenceStrength float64                             `json:"graph_evidence_strength,omitempty"`
	EvidenceRefs          []string                            `json:"evidence_refs,omitempty"`
	Boundaries            []string                            `json:"boundaries,omitempty"`
	ReviewQuestions       []string                            `json:"review_questions,omitempty"`
	EvaluationPlan        []ExternalRepoContextEvaluationStep `json:"evaluation_plan,omitempty"`
	JSONPath              string                              `json:"json_path,omitempty"`
	MarkdownPath          string                              `json:"markdown_path,omitempty"`
	ReviewYAMLPath        string                              `json:"review_yaml_path,omitempty"`
}

type ExternalRepoContextEvaluationStep struct {
	ID             string `json:"id"`
	Lens           string `json:"lens"`
	Trigger        string `json:"trigger"`
	Question       string `json:"question"`
	EvidenceNeeded string `json:"evidence_needed"`
	Boundary       string `json:"boundary"`
	Silence        string `json:"silence"`
	Proof          string `json:"proof"`
	Retirement     string `json:"retirement"`
}

type ExternalRepoReviewedManifestImportReport struct {
	SchemaVersion       string                                   `json:"schema_version"`
	Status              string                                   `json:"status"`
	AuthorityRef        string                                   `json:"authority_ref"`
	SourceListPath      string                                   `json:"source_list_path"`
	SourceListTotal     int                                      `json:"source_list_total,omitempty"`
	PendingImportCount  int                                      `json:"pending_import_count"`
	ImportLimit         int                                      `json:"import_limit,omitempty"`
	ImportedCount       int                                      `json:"imported_count"`
	OmittedPendingCount int                                      `json:"omitted_pending_count,omitempty"`
	Items               []ExternalRepoReviewedManifestImportItem `json:"items,omitempty"`
	Boundaries          []string                                 `json:"boundaries,omitempty"`
}

type ExternalRepoReviewedManifestImportItem struct {
	Repo         string   `json:"repo"`
	NodeID       string   `json:"node_id"`
	NodeStatus   string   `json:"node_status"`
	ManifestPath string   `json:"manifest_path"`
	Status       string   `json:"status"`
	NodeIDs      []string `json:"node_ids,omitempty"`
	EdgeIDs      []string `json:"edge_ids,omitempty"`
}

func ExternalRepoIntakeReportForHome(homePath string, repoQuery ...string) (ExternalRepoIntakeReport, error) {
	query := ""
	if len(repoQuery) > 0 {
		query = strings.TrimSpace(repoQuery[0])
	}
	return externalRepoIntakeReportForHome(homePath, query, "")
}

func ExternalRepoIntakeReportForHomeWithSourceList(homePath, sourceListPath string, repoQuery ...string) (ExternalRepoIntakeReport, error) {
	query := ""
	if len(repoQuery) > 0 {
		query = strings.TrimSpace(repoQuery[0])
	}
	return externalRepoIntakeReportForHome(homePath, query, sourceListPath)
}

func externalRepoIntakeReportForHome(homePath, repoQuery, sourceListPath string) (ExternalRepoIntakeReport, error) {
	report := ExternalRepoIntakeReport{
		SchemaVersion: ExternalRepoIntakeReportSchemaVersion,
		AuthorityRef:  ExternalRepoIntakeAuthorityRef,
		Status:        "missing",
		Boundaries: []string{
			"review-only",
			"no clone/install/serve",
			"no plugin execution or MCP auth",
			"no CapabilityGraph mutation",
			"no skill inbox candidate creation unless a later create_candidate decision is recorded",
			"no memory promotion",
		},
	}
	if strings.TrimSpace(homePath) == "" {
		return report, fmt.Errorf("COMPOUND_HOME required")
	}
	root := filepath.Join(homePath, "inbox", "external-repo-intake")
	var summaries []externalRepoIntakeSummaryFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(path) != "run-summary.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read external repo intake summary %s: %w", path, err)
		}
		var summary externalRepoIntakeRunSummary
		if err := json.Unmarshal(data, &summary); err != nil {
			return fmt.Errorf("parse external repo intake summary %s: %w", path, err)
		}
		created, _ := time.Parse(time.RFC3339, summary.CreatedAt)
		if created.IsZero() {
			if info, statErr := d.Info(); statErr == nil {
				created = info.ModTime()
			}
		}
		summaries = append(summaries, externalRepoIntakeSummaryFile{path: path, summary: summary, created: created})
		return nil
	})
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	if len(summaries) == 0 {
		return report, nil
	}
	sort.Slice(summaries, func(i, j int) bool {
		if !summaries[i].created.Equal(summaries[j].created) {
			return summaries[i].created.After(summaries[j].created)
		}
		return summaries[i].path > summaries[j].path
	})
	latest := summaries[0].summary
	report.Status = "ok"
	report.BatchCount = len(summaries)
	report.LatestBatchID = latest.ID
	report.LatestSummaryPath = summaries[0].path
	report.MapPath = latest.MapPath
	report.SourceObservationPath = latest.SourceObservationPath
	report.CandidateCount = latest.CandidateCount
	report.CountsByDecision = copyIntMap(latest.CountsByDecision)
	report.CountsByLane = copyIntMap(latest.CountsByLane)
	report.MergeExistingCount = latest.CountsByDecision["merge_existing"]
	report.ReferenceOnlyCount = latest.CountsByDecision["reference_only"]
	report.SourceObservationID, report.SourceCandidateIDs, report.NoPromoteIDs = parseExternalRepoSourceObserve(latest.SourceObserveOutput)

	metadata := loadExternalRepoIntakeMap(latest.MapPath)
	decisions := enrichExternalRepoDecisions(latest.Decisions, metadata)
	decisions = annotateExternalRepoDecisionState(homePath, decisions)
	report.OpenReviewCount = countPendingExternalRepoOwnerReviews(decisions)
	if repoQuery != "" {
		report.RepoQuery = repoQuery
		report.Matches = externalRepoMatchesForQuery(homePath, decisions, repoQuery)
	}
	if strings.TrimSpace(sourceListPath) != "" {
		items, err := externalRepoSourceListCoverage(homePath, decisions, sourceListPath)
		if err != nil {
			return report, err
		}
		report.SourceListPath = sourceListPath
		report.SourceListItems = items
		report.SourceListTotal = len(items)
		report.SourceListByDecision = map[string]int{}
		report.SourceListByLane = map[string]int{}
		report.SourceListByOwner = map[string]int{}
		report.SourceListByOwnerReviewStatus = map[string]int{}
		report.SourceListByCapabilityEvaluationStatus = map[string]int{}
		report.SourceListByCapabilityGraphStatus = map[string]int{}
		for _, item := range items {
			if item.Status == "known" {
				report.SourceListKnown++
				countSourceListMatches(item, report.SourceListByDecision, report.SourceListByLane, report.SourceListByOwner, report.SourceListByOwnerReviewStatus, report.SourceListByCapabilityEvaluationStatus, report.SourceListByCapabilityGraphStatus)
			} else {
				report.SourceListUnknown++
			}
		}
		graphImportNext := filterSourceListGraphImportFollowUps(items)
		report.SourceListReviewNext = sourceListReviewNext(items, 12)
		report.SourceListEvalNext = sourceListEvaluationNext(items, 12)
		report.SourceListGraphImportNext = limitSourceListFollowUps(graphImportNext, 12)
		reviewNextCount := len(report.SourceListReviewNext)
		evalNextCount := len(report.SourceListEvalNext)
		graphImportNextCount := len(report.SourceListGraphImportNext)
		report.SourceListReviewNextCount = &reviewNextCount
		report.SourceListEvalNextCount = &evalNextCount
		report.SourceListGraphImportNextCount = &graphImportNextCount
		report.SourceListGraphImportNextOmitted = len(graphImportNext) - len(report.SourceListGraphImportNext)
		report.SourceListReviewNextMeaning = "owner_review_suggestions_not_open_review_debt"
		report.SourceListEvalNextMeaning = "focused_capability_evaluation_follow_up_queue"
		report.SourceListGraphImportNextMeaning = "reviewed_manifest_import_queue_not_graph_mutation"
	}
	report.HighValue = filterExternalRepoDecisions(decisions, "keep_needs_review", 12)
	report.Recent = limitExternalRepoDecisions(decisions, 12)
	if len(metadata.Boundaries) > 0 {
		report.Boundaries = metadata.Boundaries
	}
	return report, nil
}

func ImportExternalRepoReviewedManifests(homePath, sourceListPath string, limit int, now time.Time) (ExternalRepoReviewedManifestImportReport, error) {
	report := ExternalRepoReviewedManifestImportReport{
		SchemaVersion:  ExternalRepoReviewedManifestImportVersion,
		Status:         "blocked",
		AuthorityRef:   "compound://registries/capability-graph",
		SourceListPath: strings.TrimSpace(sourceListPath),
		Boundaries: []string{
			"imports reviewed capability-evaluation manifests only",
			"uses existing import-skills graph mutation boundary",
			"does not clone repos or install served skills",
			"needs_review manifests remain non-surfacing graph nodes",
		},
	}
	if limit < 0 {
		return report, fmt.Errorf("limit must be >= 0")
	}
	if now.IsZero() {
		now = time.Now()
	}
	intake, err := ExternalRepoIntakeReportForHomeWithSourceList(homePath, sourceListPath)
	if err != nil {
		return report, err
	}
	report.SourceListTotal = intake.SourceListTotal
	pending := limitSourceListFollowUps(filterSourceListGraphImportFollowUps(intake.SourceListItems), 0)
	report.PendingImportCount = len(pending)
	report.ImportLimit = limit
	if limit > 0 && len(pending) > limit {
		report.OmittedPendingCount = len(pending) - limit
		pending = pending[:limit]
	}
	if intake.Status != "ok" {
		report.Status = intake.Status
		return report, nil
	}
	if len(pending) == 0 {
		report.Status = "no_pending_import"
		return report, nil
	}

	type preparedImport struct {
		item     ExternalRepoReviewedManifestImportItem
		manifest SkillManifest
	}
	prepared := make([]preparedImport, 0, len(pending))
	for _, sourceItem := range pending {
		match := sourceItem.Matches[0]
		manifestPath := strings.TrimSpace(match.CapabilityEvaluationManifestPath)
		if manifestPath == "" {
			return report, fmt.Errorf("external repo %s missing reviewed manifest path", match.Repo)
		}
		manifest, err := LoadSkillManifest(manifestPath)
		if err != nil {
			return report, fmt.Errorf("load reviewed manifest %s: %w", manifestPath, err)
		}
		if validation := ValidateSkillManifest(manifest); validation.Status != "ok" {
			return report, fmt.Errorf("validate reviewed manifest %s: %s", manifestPath, validation.Status)
		}
		prepared = append(prepared, preparedImport{
			item: ExternalRepoReviewedManifestImportItem{
				Repo:         match.Repo,
				NodeID:       match.CapabilityEvaluationNodeID,
				NodeStatus:   match.CapabilityEvaluationNodeStatus,
				ManifestPath: manifestPath,
				Status:       "pending",
			},
			manifest: manifest,
		})
	}

	for _, entry := range prepared {
		applied, err := ApplySkillManifest(homePath, entry.manifest, now)
		if err != nil {
			return report, fmt.Errorf("apply reviewed manifest %s: %w", entry.item.ManifestPath, err)
		}
		item := entry.item
		item.Status = "imported"
		item.NodeIDs = append([]string(nil), applied.NodeIDs...)
		item.EdgeIDs = append([]string(nil), applied.EdgeIDs...)
		report.Items = append(report.Items, item)
		report.ImportedCount++
	}
	report.Status = "ok"
	return report, nil
}

func countSourceListMatches(item ExternalRepoSourceListItem, byDecision, byLane, byOwner, byOwnerReviewStatus, byCapabilityEvaluationStatus, byCapabilityGraphStatus map[string]int) {
	if len(item.Matches) == 0 {
		return
	}
	decision := item.Matches[0]
	incrementNonEmpty(byDecision, decision.Decision)
	incrementNonEmpty(byLane, decision.Lane)
	incrementNonEmpty(byOwner, decision.Owner)
	incrementNonEmpty(byOwnerReviewStatus, ExternalRepoOwnerReviewStatus(decision))
	incrementNonEmpty(byCapabilityEvaluationStatus, ExternalRepoCapabilityEvaluationStatus(decision))
	incrementNonEmpty(byCapabilityGraphStatus, ExternalRepoCapabilityGraphStatus(decision))
}

func ExternalRepoOwnerReviewStatus(decision ExternalRepoIntakeDecision) string {
	if strings.TrimSpace(decision.Decision) != "keep_needs_review" {
		return "not_required"
	}
	if strings.TrimSpace(decision.OwnerReviewStatus) == "recorded" {
		return "recorded"
	}
	return "pending"
}

func ExternalRepoCapabilityEvaluationStatus(decision ExternalRepoIntakeDecision) string {
	if strings.TrimSpace(decision.Decision) != "keep_needs_review" {
		return "not_required"
	}
	if ExternalRepoOwnerReviewStatus(decision) != "recorded" {
		return "blocked_on_owner_review"
	}
	if status := strings.TrimSpace(decision.CapabilityEvaluationStatus); status != "" {
		return status
	}
	return "pending"
}

func ExternalRepoCapabilityGraphStatus(decision ExternalRepoIntakeDecision) string {
	if strings.TrimSpace(decision.Decision) != "keep_needs_review" {
		return "not_required"
	}
	if ExternalRepoOwnerReviewStatus(decision) != "recorded" {
		return "blocked_on_owner_review"
	}
	evaluationStatus := ExternalRepoCapabilityEvaluationStatus(decision)
	if evaluationStatus == "review_required" {
		return "blocked_on_evaluation_review"
	}
	if evaluationStatus != "recorded" {
		return "blocked_on_capability_evaluation"
	}
	if strings.TrimSpace(decision.CapabilityGraphStatus) != "" {
		return strings.TrimSpace(decision.CapabilityGraphStatus)
	}
	if strings.TrimSpace(decision.CapabilityEvaluationManifestPath) != "" {
		return "reviewed_manifest_pending_import"
	}
	return "reviewed_manifest_missing"
}

func annotateExternalRepoDecisionState(homePath string, decisions []ExternalRepoIntakeDecision) []ExternalRepoIntakeDecision {
	if len(decisions) == 0 {
		return decisions
	}
	out := annotateExternalRepoOwnerReviews(homePath, decisions)
	out = annotateExternalRepoCapabilityEvaluations(homePath, out)
	for i := range out {
		if strings.TrimSpace(out[i].OwnerReviewStatus) == "" {
			out[i].OwnerReviewStatus = ExternalRepoOwnerReviewStatus(out[i])
		}
		if strings.TrimSpace(out[i].CapabilityEvaluationStatus) == "" {
			out[i].CapabilityEvaluationStatus = ExternalRepoCapabilityEvaluationStatus(out[i])
		}
		if strings.TrimSpace(out[i].CapabilityGraphStatus) == "" {
			out[i].CapabilityGraphStatus = ExternalRepoCapabilityGraphStatus(out[i])
		}
	}
	return out
}

func countPendingExternalRepoOwnerReviews(decisions []ExternalRepoIntakeDecision) int {
	count := 0
	for _, decision := range decisions {
		if strings.TrimSpace(decision.Decision) == "keep_needs_review" && ExternalRepoOwnerReviewStatus(decision) == "pending" {
			count++
		}
	}
	return count
}

func incrementNonEmpty(counts map[string]int, key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	counts[key]++
}

func sourceListReviewNext(items []ExternalRepoSourceListItem, limit int) []ExternalRepoSourceListItem {
	return limitSourceListFollowUps(filterSourceListFollowUps(items, false), limit)
}

func sourceListEvaluationNext(items []ExternalRepoSourceListItem, limit int) []ExternalRepoSourceListItem {
	return limitSourceListFollowUps(filterSourceListFollowUps(items, true), limit)
}

func sourceListGraphImportNext(items []ExternalRepoSourceListItem, limit int) []ExternalRepoSourceListItem {
	return limitSourceListFollowUps(filterSourceListGraphImportFollowUps(items), limit)
}

func filterSourceListFollowUps(items []ExternalRepoSourceListItem, reviewed bool) []ExternalRepoSourceListItem {
	var out []ExternalRepoSourceListItem
	for _, item := range items {
		if len(item.Matches) == 0 || item.Matches[0].Decision != "keep_needs_review" {
			continue
		}
		match := item.Matches[0]
		isReviewed := match.OwnerReviewStatus == "recorded"
		if isReviewed != reviewed {
			continue
		}
		if reviewed && ExternalRepoCapabilityEvaluationStatus(match) != "pending" {
			continue
		}
		out = append(out, item)
	}
	return out
}

func filterSourceListGraphImportFollowUps(items []ExternalRepoSourceListItem) []ExternalRepoSourceListItem {
	var out []ExternalRepoSourceListItem
	for _, item := range items {
		if len(item.Matches) == 0 || item.Matches[0].Decision != "keep_needs_review" {
			continue
		}
		if ExternalRepoCapabilityGraphStatus(item.Matches[0]) != "reviewed_manifest_pending_import" {
			continue
		}
		out = append(out, item)
	}
	return out
}

func limitSourceListFollowUps(items []ExternalRepoSourceListItem, limit int) []ExternalRepoSourceListItem {
	sort.Slice(items, func(i, j int) bool {
		li, lj := items[i].Matches[0], items[j].Matches[0]
		if li.EvidenceStrength != lj.EvidenceStrength {
			return li.EvidenceStrength > lj.EvidenceStrength
		}
		return items[i].Normalized < items[j].Normalized
	})
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

func WriteExternalRepoIntakeReviewPlan(homePath, sourceListPath string, now time.Time) (ExternalRepoIntakeReviewPlanReport, error) {
	createdAt := reviewartifact.Timestamp(now)
	report := ExternalRepoIntakeReviewPlanReport{
		SchemaVersion:  ExternalRepoIntakeReviewPlanSchemaVersion,
		Status:         "missing",
		CreatedAt:      createdAt,
		AuthorityRef:   ExternalRepoIntakeAuthorityRef,
		ReviewArea:     ExternalRepoIntakeReviewArea,
		SourceListPath: strings.TrimSpace(sourceListPath),
	}
	intake, err := ExternalRepoIntakeReportForHomeWithSourceList(homePath, sourceListPath)
	if err != nil {
		return report, err
	}
	report.Status = intake.Status
	report.SourceListTotal = intake.SourceListTotal
	report.SourceListKnown = intake.SourceListKnown
	report.SourceListUnknown = intake.SourceListUnknown
	report.LatestBatchID = intake.LatestBatchID
	report.Boundaries = append([]string{}, intake.Boundaries...)
	if intake.Status != "ok" {
		return report, nil
	}
	if len(intake.SourceListReviewNext) == 0 {
		report.Status = "no_review_required"
		return report, nil
	}
	for _, sourceItem := range intake.SourceListReviewNext {
		if len(sourceItem.Matches) == 0 {
			continue
		}
		match := sourceItem.Matches[0]
		item := externalRepoIntakeReviewPlanItem(intake, sourceItem, match, createdAt)
		paths, err := reviewartifact.Write(homePath, ExternalRepoIntakeReviewArea, item.ID, item, " ", renderExternalRepoIntakeReviewPlanMarkdown(item))
		if err != nil {
			return report, err
		}
		reviewYAMLPath, err := reviewartifact.WriteRaw(homePath, ExternalRepoIntakeReviewArea, item.ID+".review.yaml", []byte(RenderExternalRepoIntakeReviewYAMLDraft(item)))
		if err != nil {
			return report, err
		}
		item.JSONPath = paths.PlanPath
		item.MarkdownPath = paths.ReviewPath
		item.ReviewYAMLPath = reviewYAMLPath
		report.Items = append(report.Items, item)
	}
	report.WroteCount = len(report.Items)
	if report.WroteCount == 0 {
		report.Status = "no_review_required"
	}
	return report, nil
}

func externalRepoIntakeReviewPlanItem(report ExternalRepoIntakeReport, sourceItem ExternalRepoSourceListItem, match ExternalRepoIntakeDecision, createdAt string) ExternalRepoIntakeReviewPlanItem {
	boundaries := reviewartifact.Unique(append(append([]string{}, report.Boundaries...), match.NotToDo))
	repo := reviewartifact.FirstNonEmpty(match.Repo, sourceItem.Normalized)
	evidenceRefs := reviewartifact.Unique([]string{
		ExternalRepoIntakeAuthorityRef,
		ExternalRepoIntakeAuthorityRef + "/" + report.LatestBatchID,
		ExternalRepoIntakeAuthorityRef + "/" + report.LatestBatchID + "#" + repo,
	})
	return ExternalRepoIntakeReviewPlanItem{
		ID:                    reviewartifact.ID("extrepo_", report.LatestBatchID, sourceItem.Normalized),
		Status:                "review_required",
		CreatedAt:             createdAt,
		Readiness:             "needs_owner_review",
		SourceKind:            "external-repo-source-list",
		SourcePath:            report.SourceListPath,
		TargetPath:            "github.com/" + repo,
		SourceListQuery:       sourceItem.Query,
		NormalizedRepo:        sourceItem.Normalized,
		Repo:                  repo,
		Decision:              match.Decision,
		Lane:                  match.Lane,
		Owner:                 match.Owner,
		NextAction:            match.NextReviewStep,
		NotToDo:               match.NotToDo,
		GraphDecisionHint:     match.Decision,
		GraphEvidenceStrength: match.EvidenceStrength,
		EvidenceRefs:          evidenceRefs,
		Boundaries:            boundaries,
		ReviewQuestions: []string{
			"Which existing Compound owner should consume this repository, if any?",
			"What proof would be required before any skill, hook, MCP, plugin, or graph mutation is promoted?",
			"What existing surface would this merge into or retire instead of creating a parallel system?",
			"What boundary must stay true even if the repo remains useful as reference evidence?",
		},
		EvaluationPlan: externalRepoContextEvaluationPlan(repo, match),
	}
}

func externalRepoContextEvaluationPlan(repo string, match ExternalRepoIntakeDecision) []ExternalRepoContextEvaluationStep {
	context := strings.ToLower(strings.Join([]string{
		repo,
		match.Lane,
		match.Owner,
		match.NextReviewStep,
		match.NotToDo,
	}, " "))
	steps := []ExternalRepoContextEvaluationStep{
		{
			ID:             "existing-owner-fit",
			Lens:           "owner-fit",
			Trigger:        "external repo is marked keep_needs_review by source-list intake",
			Question:       "Which existing Compound owner should consume the repo's useful behavior?",
			EvidenceNeeded: "recorded external-intake review with owner, rationale, confidence, and next proof",
			Boundary:       "do not create a new registry, skill, hook, MCP, plugin, or package before owner fit is recorded",
			Silence:        "stay silent for reference_only, reject, unknown repos, and ordinary product work",
			Proof:          "compound reviews --area external-repo-intake --json exposes this plan on the review card",
			Retirement:     "repo remains source evidence until a later reviewed manifest narrows or retires an old owner",
		},
		{
			ID:             "capability-shape-fit",
			Lens:           "capability-fit",
			Trigger:        "owner review records keep_needs_review and names a next proof",
			Question:       "What trigger, action, boundary, silence, proof, and retirement would this capability add?",
			EvidenceNeeded: "external-intake-evaluation-plan output plus reviewed capability-evaluation YAML",
			Boundary:       "no CapabilityGraph mutation, install, clone, serve, auth, or tool execution during evaluation",
			Silence:        "stay silent until owner review is recorded; do not evaluate merge_existing/reference_only as adoption",
			Proof:          "capability evaluation plan is review_required, not active, and graph import remains a separate gate",
			Retirement:     "only a reviewed manifest may narrow an old owner or add a graph offer; never adopt repo wholesale",
		},
		{
			ID:             "risk-cost-fit",
			Lens:           "risk-cost",
			Trigger:        "repo could affect public/private boundary, auth, spend, execution, or operator workload",
			Question:       "What can this repo make cheaper or safer, and what new maintenance burden does it create?",
			EvidenceNeeded: "explicit not_to_do boundary, duplicate-risk/evidence-strength fields, and required next proof",
			Boundary:       "treat external repo as evidence only; no public/private leak, credentials, accounts, or private bodies",
			Silence:        "stay silent when the repo is only a reference or the expected savings do not beat adoption cost",
			Proof:          "review card names boundary and next proof before any downstream evaluation can run",
			Retirement:     "drop, park, or keep as reference if it adds surfaces without retiring or narrowing anything",
		},
	}
	if strings.Contains(context, "harness") || strings.Contains(context, "agent") || strings.Contains(context, "browser") || strings.Contains(context, "hook") {
		steps = append(steps, ExternalRepoContextEvaluationStep{
			ID:             "harness-fit",
			Lens:           "harness-fit",
			Trigger:        "repo claims agent, harness, browser, hook, or executor behavior",
			Question:       "Does this improve Compound's replaceable-executor path without making the executor source of truth?",
			EvidenceNeeded: "parity test, hook/context-pack proof, or receipt showing the old path can be narrowed",
			Boundary:       "do not register hooks, mutate harness config, or grant tool auth from intake",
			Silence:        "stay silent for non-harness repos and for harness ideas without a parity proof",
			Proof:          "later slice proves a shim, doctor, or ContextPack consumer; intake only records the evaluation need",
			Retirement:     "old harness-specific prompt or script becomes evidence-only after Compound owns the behavior",
		})
	}
	if strings.Contains(context, "design") || strings.Contains(context, "ui") || strings.Contains(context, "ux") || strings.Contains(context, "visual") || strings.Contains(context, "taste") {
		steps = append(steps, ExternalRepoContextEvaluationStep{
			ID:             "taste-fit",
			Lens:           "taste-fit",
			Trigger:        "repo claims design, UI, UX, visual, or taste capability",
			Question:       "What concrete standard or example would improve product taste without becoming vague prose?",
			EvidenceNeeded: "before/after fixture, design-standard example, or reviewable taste criterion",
			Boundary:       "do not import style prose as ambient instruction or make public core depend on private taste examples",
			Silence:        "stay silent for non-design repos and for taste claims without examples",
			Proof:          "future capability evaluation includes a fixture or example-based standard, not adjectives alone",
			Retirement:     "old design note remains reference until a consumed standard or skill node proves value",
		})
	}
	if strings.Contains(context, "product") || strings.Contains(context, "marketing") || strings.Contains(context, "social") || strings.Contains(context, "distribution") || strings.Contains(context, "analytics") {
		steps = append(steps, ExternalRepoContextEvaluationStep{
			ID:             "product-loop-fit",
			Lens:           "product-loop-fit",
			Trigger:        "repo claims product, marketing, distribution, social, or analytics loop behavior",
			Question:       "How would the repo close change -> signal -> hypothesis -> observed result -> next move?",
			EvidenceNeeded: "signal source, receipt shape, hypothesis/update path, and public/private boundary",
			Boundary:       "no posting, public mutation, spend, analytics auth, or private account access during intake",
			Silence:        "stay silent for repos that do not touch product feedback loops",
			Proof:          "future product loop proof names a signal and next bounded move before any automation",
			Retirement:     "external service stays a rented adapter feeding Compound receipts, not product-loop authority",
		})
	}
	return steps
}

func renderExternalRepoIntakeReviewPlanMarkdown(item ExternalRepoIntakeReviewPlanItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# External Repo Intake Review: %s\n\n", item.Repo)
	fmt.Fprintf(&b, "- status: %s\n", item.Status)
	fmt.Fprintf(&b, "- source_list_query: %s\n", item.SourceListQuery)
	fmt.Fprintf(&b, "- decision: %s\n", item.Decision)
	if item.Lane != "" {
		fmt.Fprintf(&b, "- lane: %s\n", item.Lane)
	}
	if item.Owner != "" {
		fmt.Fprintf(&b, "- owner: %s\n", item.Owner)
	}
	if item.GraphEvidenceStrength > 0 {
		fmt.Fprintf(&b, "- evidence_strength: %.2f\n", item.GraphEvidenceStrength)
	}
	if item.NextAction != "" {
		fmt.Fprintf(&b, "- next_action: %s\n", item.NextAction)
	}
	if item.NotToDo != "" {
		fmt.Fprintf(&b, "- boundary: %s\n", item.NotToDo)
	}
	if len(item.ReviewQuestions) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Review Questions")
		fmt.Fprintln(&b)
		for _, question := range item.ReviewQuestions {
			fmt.Fprintf(&b, "- %s\n", question)
		}
	}
	if len(item.EvaluationPlan) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Context-Aware Evaluation Plan")
		fmt.Fprintln(&b)
		for _, step := range item.EvaluationPlan {
			fmt.Fprintf(&b, "- %s (%s): %s\n", step.ID, step.Lens, step.Question)
			fmt.Fprintf(&b, "  - trigger: %s\n", step.Trigger)
			fmt.Fprintf(&b, "  - evidence_needed: %s\n", step.EvidenceNeeded)
			fmt.Fprintf(&b, "  - boundary: %s\n", step.Boundary)
			fmt.Fprintf(&b, "  - silence: %s\n", step.Silence)
			fmt.Fprintf(&b, "  - proof: %s\n", step.Proof)
			fmt.Fprintf(&b, "  - retirement: %s\n", step.Retirement)
		}
	}
	if len(item.Boundaries) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Boundaries")
		fmt.Fprintln(&b)
		for _, boundary := range item.Boundaries {
			fmt.Fprintf(&b, "- %s\n", boundary)
		}
	}
	if len(item.EvidenceRefs) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Evidence")
		fmt.Fprintln(&b)
		for _, ref := range item.EvidenceRefs {
			fmt.Fprintf(&b, "- %s\n", ref)
		}
	}
	return b.String()
}

func RenderExternalRepoIntakeReviewYAMLDraft(item ExternalRepoIntakeReviewPlanItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %s\n", ExternalRepoIntakeReviewSchemaVersion)
	fmt.Fprintf(&b, "id: %s\n", reviewartifact.ID("err_", item.ID, item.Repo))
	fmt.Fprintf(&b, "plan_id: %s\n", item.ID)
	fmt.Fprintf(&b, "repo: %s\n", item.Repo)
	fmt.Fprintf(&b, "decision: %s\n", reviewartifact.FirstNonEmpty(item.Decision, "keep_needs_review"))
	fmt.Fprintf(&b, "owner: %q\n", item.Owner)
	fmt.Fprintln(&b, "rationale: \"\"")
	fmt.Fprintf(&b, "confidence: %.2f\n", item.GraphEvidenceStrength)
	fmt.Fprintf(&b, "next_action: %q\n", item.NextAction)
	fmt.Fprintln(&b, "evidence_refs:")
	for _, ref := range item.EvidenceRefs {
		fmt.Fprintf(&b, "  - %q\n", ref)
	}
	fmt.Fprintln(&b, "boundaries:")
	for _, boundary := range item.Boundaries {
		fmt.Fprintf(&b, "  - %q\n", boundary)
	}
	return b.String()
}

func externalRepoMatchesForQuery(homePath string, decisions []ExternalRepoIntakeDecision, query string) []ExternalRepoIntakeDecision {
	// Decisions already carry this report's fresh review projection. Only
	// evaluation-only references need enrichment here; re-reading the entire
	// review archive for every match makes hook delivery depend on history size.
	matches := matchExternalRepoDecisions(decisions, query)
	references := matchExternalRepoEvaluationReferences(homePath, query, matches)
	references = annotateExternalRepoOwnerReviews(homePath, references)
	references = annotateExternalRepoCapabilityEvaluations(homePath, references)
	return append(matches, references...)
}

func ExternalRepoMatchedEvidenceForTask(homePath, task string, limit int) ([]SurfacingConstraint, error) {
	report, err := ExternalRepoIntakeReportForHome(homePath, task)
	if err != nil {
		return nil, err
	}
	if report.Status != "ok" || len(report.Matches) == 0 {
		return nil, nil
	}
	matches := report.Matches
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]SurfacingConstraint, 0, len(matches))
	for _, match := range matches {
		out = append(out, SurfacingConstraint{
			Kind:       "external_repo_review_evidence",
			Ref:        ExternalRepoIntakeAuthorityRef + "/" + normalizeExternalRepoQuery(match.Repo),
			Detail:     externalRepoMatchedEvidenceDetail(match),
			Confidence: externalRepoMatchedEvidenceConfidence(match),
		})
	}
	return out, nil
}

func externalRepoMatchedEvidenceDetail(match ExternalRepoIntakeDecision) string {
	parts := []string{
		"repo=" + match.Repo,
		"decision=" + match.Decision,
		"evidence only; not adoption or execution",
	}
	if match.Owner != "" {
		parts = append(parts, "owner="+match.Owner)
	}
	if match.Lane != "" {
		parts = append(parts, "lane="+match.Lane)
	}
	if match.CapabilityGraphStatus != "" {
		parts = append(parts, "capability_graph="+match.CapabilityGraphStatus)
	}
	if match.CapabilityEvaluationNodeStatus != "" {
		parts = append(parts, "node_status="+match.CapabilityEvaluationNodeStatus)
	}
	if match.NextReviewStep != "" {
		parts = append(parts, "next="+match.NextReviewStep)
	}
	if match.NotToDo != "" {
		parts = append(parts, "boundary="+match.NotToDo)
	}
	return strings.Join(parts, "; ")
}

func externalRepoMatchedEvidenceConfidence(match ExternalRepoIntakeDecision) float64 {
	if match.EvidenceStrength > 0 {
		return match.EvidenceStrength
	}
	switch match.Decision {
	case "keep_needs_review":
		return 0.72
	case "merge_existing":
		return 0.66
	case "reference_only":
		return 0.58
	default:
		return 0.5
	}
}

func externalRepoSourceListCoverage(homePath string, decisions []ExternalRepoIntakeDecision, path string) ([]ExternalRepoSourceListItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read external repo source list %s: %w", path, err)
	}
	queries := extractExternalRepoQueries(string(data))
	items := make([]ExternalRepoSourceListItem, 0, len(queries))
	for _, query := range queries {
		matches := externalRepoMatchesForQuery(homePath, decisions, query)
		item := ExternalRepoSourceListItem{
			Query:      query,
			Normalized: normalizeExternalRepoQuery(query),
			Status:     "unknown",
			MatchCount: len(matches),
			Matches:    matches,
		}
		if len(matches) > 0 {
			item.Status = "known"
		}
		items = append(items, item)
	}
	return items, nil
}

func annotateExternalRepoOwnerReviews(homePath string, matches []ExternalRepoIntakeDecision) []ExternalRepoIntakeDecision {
	if len(matches) == 0 {
		return matches
	}
	reviews := externalRepoReviewedReportsByRepo(homePath)
	if len(reviews) == 0 {
		return matches
	}
	out := make([]ExternalRepoIntakeDecision, 0, len(matches))
	for _, match := range matches {
		if report, ok := reviews[normalizeExternalRepoQuery(match.Repo)]; ok {
			match.OwnerReviewStatus = report.Status
			match.OwnerReviewPath = report.ReviewPath
			match.OwnerReviewDecision = report.Decision
			match.OwnerReviewNextAction = report.NextAction
		}
		out = append(out, match)
	}
	return out
}

func externalRepoReviewedReportsByRepo(homePath string) map[string]ExternalRepoIntakeReviewReport {
	out := map[string]ExternalRepoIntakeReviewReport{}
	dir := filepath.Join(homePath, "reviews", ExternalRepoIntakeReviewArea)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".reviewed.json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		report, err := LoadExternalRepoIntakeReviewReport(path)
		if err != nil {
			continue
		}
		key := normalizeExternalRepoQuery(report.Repo)
		if key == "" {
			continue
		}
		if existing, ok := out[key]; ok && existing.ReviewPath < report.ReviewPath {
			continue
		}
		out[key] = report
	}
	return out
}

type externalRepoCapabilityEvaluationReview struct {
	Status       string
	Path         string
	Decision     string
	NodeID       string
	NodeStatus   string
	ManifestPath string
	NextAction   string
}

func annotateExternalRepoCapabilityEvaluations(homePath string, matches []ExternalRepoIntakeDecision) []ExternalRepoIntakeDecision {
	if len(matches) == 0 {
		return matches
	}
	reviews := externalRepoCapabilityEvaluationReviewsByRepo(homePath)
	if len(reviews) == 0 {
		return matches
	}
	importedNodeIDs := externalRepoImportedCapabilityNodeIDs(homePath)
	out := make([]ExternalRepoIntakeDecision, 0, len(matches))
	for _, match := range matches {
		if review, ok := reviews[normalizeExternalRepoQuery(match.Repo)]; ok {
			match.CapabilityEvaluationStatus = review.Status
			match.CapabilityEvaluationPath = review.Path
			match.CapabilityEvaluationDecision = review.Decision
			match.CapabilityEvaluationNodeID = review.NodeID
			match.CapabilityEvaluationNodeStatus = review.NodeStatus
			match.CapabilityEvaluationManifestPath = review.ManifestPath
			match.CapabilityEvaluationNextAction = review.NextAction
			if importedNodeIDs[review.NodeID] {
				match.CapabilityGraphStatus = "imported"
			} else if review.Status != "recorded" {
				match.CapabilityGraphStatus = "blocked_on_evaluation_review"
			} else if review.ManifestPath != "" {
				match.CapabilityGraphStatus = "reviewed_manifest_pending_import"
			} else {
				match.CapabilityGraphStatus = "reviewed_manifest_missing"
			}
		}
		out = append(out, match)
	}
	return out
}

func externalRepoImportedCapabilityNodeIDs(homePath string) map[string]bool {
	out := map[string]bool{}
	store, err := Open(homePath)
	if err != nil {
		return out
	}
	nodes, err := store.loadNodes()
	if err != nil {
		return out
	}
	for _, node := range nodes {
		out[node.ID] = true
	}
	return out
}

func externalRepoCapabilityEvaluationReviewsByRepo(homePath string) map[string]externalRepoCapabilityEvaluationReview {
	out := map[string]externalRepoCapabilityEvaluationReview{}
	queue, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{Area: capabilityEvaluationReviewArea, IncludeArchived: true}, time.Now())
	if err != nil {
		return out
	}
	for _, item := range queue.Items {
		if item.Area != capabilityEvaluationReviewArea {
			continue
		}
		planData, err := os.ReadFile(item.JSONPath)
		if err != nil {
			continue
		}
		var plan CapabilityEvaluationPlan
		if err := json.Unmarshal(planData, &plan); err != nil {
			continue
		}
		repo := signalString(plan.Signals, "external_repo")
		key := normalizeExternalRepoQuery(repo)
		if key == "" {
			continue
		}
		if item.Status == "review_required" {
			if _, exists := out[key]; exists {
				continue
			}
			out[key] = externalRepoCapabilityEvaluationReview{
				Status:       "review_required",
				Path:         capabilityEvaluationReviewYAMLPath(item),
				NodeID:       plan.NodeDraft.NodeID,
				NodeStatus:   plan.Status,
				ManifestPath: capabilityEvaluationManifestDraftPath(item),
				NextAction:   "review capability evaluation YAML before any graph import",
			}
			if out[key].Path == "" {
				review := out[key]
				review.Path = item.JSONPath
				out[key] = review
			}
			continue
		}
		if item.Status != "recorded" {
			continue
		}
		reviewedPath := capabilityEvaluationReviewedPath(item)
		if reviewedPath == "" {
			continue
		}
		data, err := os.ReadFile(reviewedPath)
		if err != nil {
			continue
		}
		var review CapabilityEvaluationReviewReport
		if err := json.Unmarshal(data, &review); err != nil {
			continue
		}
		out[key] = externalRepoCapabilityEvaluationReview{
			Status:       "recorded",
			Path:         reviewedPath,
			Decision:     review.Decision,
			NodeID:       review.NodeID,
			NodeStatus:   review.NodeStatus,
			ManifestPath: review.ManifestPath,
			NextAction:   "capability evaluation review recorded; graph mutation requires separate reviewed manifest import",
		}
	}
	return out
}

func capabilityEvaluationReviewYAMLPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".review.yaml") {
			return path
		}
	}
	return ""
}

func capabilityEvaluationManifestDraftPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".skill-manifest.yaml") {
			return path
		}
	}
	return ""
}

func capabilityEvaluationReviewedPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".reviewed.json") {
			return path
		}
	}
	return ""
}

func extractExternalRepoQueries(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, token := range strings.FieldsFunc(text, externalRepoTokenBoundary) {
		token = strings.Trim(token, "`\"'.,;")
		if token == "" {
			continue
		}
		normalized := normalizeExternalRepoQuery(token)
		if !looksLikeExternalRepoToken(token, normalized) || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, token)
	}
	return out
}

func externalRepoTokenBoundary(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("<>()[]{}", r)
}

func looksLikeExternalRepoToken(token, normalized string) bool {
	if normalized == "" || !strings.Contains(normalized, "/") {
		return false
	}
	lower := strings.ToLower(token)
	if strings.Contains(lower, "github.com/") || strings.Contains(lower, "gist.github.com/") {
		return true
	}
	parts := strings.Split(normalized, "/")
	return len(parts) == 2 && !strings.Contains(token, ":")
}

func parseExternalRepoSourceObserve(lines []string) (string, []string, []string) {
	var sourceObservation string
	var candidates []string
	var noPromote []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
		switch {
		case strings.HasPrefix(trimmed, "source observation "):
			sourceObservation = strings.TrimSpace(strings.TrimPrefix(trimmed, "source observation "))
		case strings.HasPrefix(trimmed, "source_candidate:"):
			candidates = append(candidates, strings.TrimSpace(strings.TrimPrefix(trimmed, "source_candidate:")))
		case strings.HasPrefix(trimmed, "source_no_promote:"):
			noPromote = append(noPromote, strings.TrimSpace(strings.TrimPrefix(trimmed, "source_no_promote:")))
		}
	}
	return sourceObservation, candidates, noPromote
}

func loadExternalRepoIntakeMap(path string) externalRepoIntakeMap {
	if strings.TrimSpace(path) == "" {
		return externalRepoIntakeMap{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return externalRepoIntakeMap{}
	}
	var m externalRepoIntakeMap
	if err := json.Unmarshal(data, &m); err != nil {
		return externalRepoIntakeMap{}
	}
	return m
}

func enrichExternalRepoDecisions(decisions []ExternalRepoIntakeDecision, metadata externalRepoIntakeMap) []ExternalRepoIntakeDecision {
	byRepo := map[string]externalRepoIntakeMapItem{}
	for _, item := range metadata.Items {
		byRepo[strings.ToLower(item.Repo)] = item
	}
	out := make([]ExternalRepoIntakeDecision, 0, len(decisions))
	for _, decision := range decisions {
		if item, ok := byRepo[strings.ToLower(decision.Repo)]; ok {
			decision.Owner = item.Owner
			decision.NextReviewStep = item.NextReviewStep
			decision.NotToDo = item.NotToDo
		}
		out = append(out, decision)
	}
	return out
}

func matchExternalRepoDecisions(decisions []ExternalRepoIntakeDecision, query string) []ExternalRepoIntakeDecision {
	needle := normalizeExternalRepoQuery(query)
	if needle == "" {
		return nil
	}
	var out []ExternalRepoIntakeDecision
	seen := map[string]bool{}
	for _, decision := range decisions {
		haystack := normalizeExternalRepoQuery(decision.Repo)
		if haystack == needle || strings.Contains(haystack, needle) || strings.Contains(needle, haystack) {
			out = append(out, decision)
			seen[haystack] = true
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Lane < out[j].Lane
	})
	out = append(out, matchExternalRepoDecisionsByContext(decisions, query, seen)...)
	return out
}

type externalRepoContextMatch struct {
	decision ExternalRepoIntakeDecision
	score    int
}

func matchExternalRepoDecisionsByContext(decisions []ExternalRepoIntakeDecision, query string, seen map[string]bool) []ExternalRepoIntakeDecision {
	queryTokens := tokenSet(query)
	if len(queryTokens) == 0 {
		return nil
	}
	var matches []externalRepoContextMatch
	for _, decision := range decisions {
		key := normalizeExternalRepoQuery(decision.Repo)
		if seen[key] {
			continue
		}
		score := externalRepoContextScore(queryTokens, decision)
		if score < 2 {
			continue
		}
		seen[key] = true
		matches = append(matches, externalRepoContextMatch{decision: decision, score: score})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if matches[i].decision.EvidenceStrength != matches[j].decision.EvidenceStrength {
			return matches[i].decision.EvidenceStrength > matches[j].decision.EvidenceStrength
		}
		return matches[i].decision.Repo < matches[j].decision.Repo
	})
	out := make([]ExternalRepoIntakeDecision, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.decision)
	}
	return out
}

func externalRepoContextScore(queryTokens map[string]bool, decision ExternalRepoIntakeDecision) int {
	fields := []string{
		decision.Repo,
		decision.Lane,
		decision.Owner,
		decision.NextReviewStep,
		decision.NotToDo,
		decision.GraphHint,
		decision.CapabilityEvaluationDecision,
		decision.CapabilityEvaluationNodeID,
		decision.CapabilityEvaluationNextAction,
	}
	contextTokens := tokenSet(strings.Join(fields, " "))
	score := 0
	for token := range queryTokens {
		if contextTokens[token] {
			score++
		}
	}
	return score
}

func matchExternalRepoEvaluationReferences(homePath, query string, existing []ExternalRepoIntakeDecision) []ExternalRepoIntakeDecision {
	needle := normalizeExternalRepoQuery(query)
	if needle == "" {
		return nil
	}
	seen := map[string]bool{}
	for _, decision := range existing {
		seen[normalizeExternalRepoQuery(decision.Repo)] = true
	}
	dir := filepath.Join(homePath, "inbox", "capability-evaluations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []ExternalRepoIntakeDecision
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		eval, _, err := loadCapabilityEvaluationFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		decision, ok := externalRepoDecisionFromEvaluation(eval, needle)
		if !ok {
			continue
		}
		key := normalizeExternalRepoQuery(decision.Repo)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, decision)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EvidenceStrength != out[j].EvidenceStrength {
			return out[i].EvidenceStrength > out[j].EvidenceStrength
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

func externalRepoDecisionFromEvaluation(eval CapabilityEvaluation, needle string) (ExternalRepoIntakeDecision, bool) {
	if len(eval.Signals) == 0 {
		return ExternalRepoIntakeDecision{}, false
	}
	repo := signalString(eval.Signals, "external_repo")
	url := signalString(eval.Signals, "external_url")
	if repo == "" && url == "" {
		return ExternalRepoIntakeDecision{}, false
	}
	if !externalRepoQueryMatches(needle, repo) && !externalRepoQueryMatches(needle, url) {
		return ExternalRepoIntakeDecision{}, false
	}
	decision := ExternalRepoIntakeDecision{
		Repo:             firstExternalRepoString(repo, url),
		Decision:         externalRepoDecisionForEvaluationVerdict(eval.Verdict),
		Lane:             firstExternalRepoString(signalString(eval.Signals, "cluster"), signalString(eval.Signals, "kind"), "capability-evaluation"),
		Status:           "recorded",
		Owner:            signalString(eval.Signals, "existing_owner"),
		NextReviewStep:   signalString(eval.Signals, "required_next_proof"),
		NotToDo:          signalString(eval.Signals, "main_risks"),
		GraphHint:        eval.Verdict,
		EvidenceStrength: evaluationConfidence(eval),
		ReviewID:         eval.ID,
		ReviewPath:       "capability_evaluation:" + eval.ID,
	}
	if decision.Owner == "" {
		decision.Owner = eval.Capability
	}
	return decision, decision.Repo != ""
}

func externalRepoDecisionForEvaluationVerdict(verdict string) string {
	switch strings.TrimSpace(verdict) {
	case "useful", "keep", "adapt", "merge", "publicize_candidate":
		return "merge_existing"
	case "evaluate_further", "needs_improvement":
		return "keep_needs_review"
	default:
		return "reference_only"
	}
}

func externalRepoQueryMatches(needle, value string) bool {
	haystack := normalizeExternalRepoQuery(value)
	if needle == "" || haystack == "" {
		return false
	}
	return haystack == needle || strings.Contains(haystack, needle) || strings.Contains(needle, haystack)
}

func firstExternalRepoString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizeExternalRepoQuery(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	v = strings.TrimSuffix(v, ".git")
	for _, prefix := range []string{
		"https://github.com/",
		"http://github.com/",
		"github.com/",
		"https://gist.github.com/",
		"http://gist.github.com/",
		"gist.github.com/",
	} {
		v = strings.TrimPrefix(v, prefix)
	}
	return strings.Trim(v, "/")
}

func filterExternalRepoDecisions(decisions []ExternalRepoIntakeDecision, decision string, limit int) []ExternalRepoIntakeDecision {
	var out []ExternalRepoIntakeDecision
	for _, row := range decisions {
		if row.Decision == decision {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EvidenceStrength != out[j].EvidenceStrength {
			return out[i].EvidenceStrength > out[j].EvidenceStrength
		}
		return out[i].Repo < out[j].Repo
	})
	return limitExternalRepoDecisions(out, limit)
}

func limitExternalRepoDecisions(decisions []ExternalRepoIntakeDecision, limit int) []ExternalRepoIntakeDecision {
	if limit <= 0 || len(decisions) <= limit {
		return decisions
	}
	return decisions[:limit]
}

func copyIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
