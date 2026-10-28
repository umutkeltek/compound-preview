package product

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"compound/internal/mem"
)

const AuthorityRef = "compound://ledger/product-feedback"

type AuthorityReport struct {
	AuthorityRef            string                 `json:"authority_ref"`
	AuthorityPaths          []string               `json:"authority_paths"`
	ReleasePlanningRef      string                 `json:"release_planning_ref"`
	ProductReleaseCommand   string                 `json:"product_release_command"`
	RepositoryExportCommand string                 `json:"repository_export_command"`
	ReleaseBoundary         string                 `json:"release_boundary"`
	Status                  string                 `json:"status"`
	ProductCount            int                    `json:"product_count"`
	SignalCount             int                    `json:"signal_count"`
	HypothesisCount         int                    `json:"hypothesis_count"`
	PendingReviewCount      int                    `json:"pending_review_count"`
	ReviewReadyCount        int                    `json:"review_ready_count"`
	SourceIssueCount        int                    `json:"source_issue_count"`
	TransitionCount         int                    `json:"transition_count"`
	MissingEvidenceCount    int                    `json:"missing_evidence_count"`
	Products                []ProductAuthorityItem `json:"products"`
}

type ProductAuthorityItem struct {
	ProductID            string   `json:"product_id"`
	Status               string   `json:"status"`
	SignalCount          int      `json:"signal_count"`
	HypothesisCount      int      `json:"hypothesis_count"`
	PendingReviewCount   int      `json:"pending_review_count"`
	ReviewReadyCount     int      `json:"review_ready_count"`
	SourceIssueCount     int      `json:"source_issue_count"`
	TransitionCount      int      `json:"transition_count"`
	MissingEvidenceCount int      `json:"missing_evidence_count"`
	LatestSignalID       string   `json:"latest_signal_id,omitempty"`
	LatestTransitionID   string   `json:"latest_transition_id,omitempty"`
	ReviewPlanCommands   []string `json:"review_plan_commands,omitempty"`
}

func Authority(homePath string, store *mem.Store) (AuthorityReport, error) {
	productIDs, err := authorityProductIDs(homePath, store)
	if err != nil {
		return AuthorityReport{}, err
	}
	report := AuthorityReport{
		AuthorityRef:            AuthorityRef,
		ReleasePlanningRef:      "compound://product/release-flow",
		ProductReleaseCommand:   "compound product release-plan --product <id> --change <summary> --candidate <ref> --rollback <text> --metric <post-release-metric>",
		RepositoryExportCommand: "compound release export-public",
		ReleaseBoundary:         "compound release is repository export, not product release; Compound owns product release authorization, operator-held work planning, and proof-backed observation while external execution remains separately PolicyDecision-gated",
		AuthorityPaths: []string{
			"ledger/claims.jsonl",
			"ledger/product-signals.jsonl",
			"ledger/product-ads.jsonl",
			"ledger/product-ads-imports.jsonl",
			"inbox/product-hypothesis-candidates",
			"compound://events/<id>",
			"ledger/product-reviews.jsonl",
			"ledger/product-source-issue-reviews.jsonl",
			"ledger/no-promote-decisions.jsonl",
		},
		Status: "ok",
	}
	for _, productID := range productIDs {
		tl, err := LoadTimeline(homePath, productID)
		if err != nil {
			return AuthorityReport{}, err
		}
		doctor, err := Doctor(homePath, productID)
		if err != nil {
			return AuthorityReport{}, err
		}
		item := ProductAuthorityItem{
			ProductID:            productID,
			Status:               doctor.Status,
			SignalCount:          doctor.SignalCount,
			HypothesisCount:      doctor.HypothesisCount,
			PendingReviewCount:   doctor.PendingReviewCount,
			ReviewReadyCount:     doctor.ReviewReadyCount,
			SourceIssueCount:     doctor.SourceIssueCount,
			TransitionCount:      len(tl.Transitions),
			MissingEvidenceCount: len(doctor.MissingEvidenceRefs),
		}
		if len(tl.Signals) > 0 {
			item.LatestSignalID = latestSignalID(tl.Signals)
		}
		if len(tl.Transitions) > 0 {
			item.LatestTransitionID = tl.Transitions[len(tl.Transitions)-1].ID
		}
		for _, ready := range doctor.ReviewReady {
			item.ReviewPlanCommands = append(item.ReviewPlanCommands, ready.ReviewPlanCommand)
		}
		report.Products = append(report.Products, item)
		report.SignalCount += item.SignalCount
		report.HypothesisCount += item.HypothesisCount
		report.PendingReviewCount += item.PendingReviewCount
		report.ReviewReadyCount += item.ReviewReadyCount
		report.SourceIssueCount += item.SourceIssueCount
		report.TransitionCount += item.TransitionCount
		report.MissingEvidenceCount += item.MissingEvidenceCount
		if item.Status != "ok" {
			report.Status = "needs_review"
		}
	}
	report.ProductCount = len(report.Products)
	return report, nil
}

func authorityProductIDs(homePath string, store *mem.Store) ([]string, error) {
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		seen[id] = true
	}
	signals, err := loadSignals(homePath)
	if err != nil {
		return nil, err
	}
	for _, signal := range signals {
		add(signal.ProductID)
	}
	hypotheses, err := loadHypotheses(homePath)
	if err != nil {
		return nil, err
	}
	for _, hypothesis := range hypotheses {
		add(hypothesis.ProductID)
	}
	issues, err := productIDsFromSourceIssues(homePath)
	if err != nil {
		return nil, err
	}
	for _, productID := range issues {
		add(productID)
	}
	if store != nil {
		claims, err := store.AllCurrent()
		if err != nil {
			return nil, fmt.Errorf("load product claims: %w", err)
		}
		for _, claim := range claims {
			if claim.Qualifiers["domain"] == "product" {
				add(claim.Entity)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func productIDsFromSourceIssues(homePath string) ([]string, error) {
	ids := map[string]bool{}
	err := readJSONL(noPromoteLog(homePath), func(line []byte) error {
		var d struct {
			OwnerTarget struct {
				Kind string `json:"kind"`
			} `json:"owner_target"`
			EvidenceRefs []string `json:"evidence_refs"`
		}
		if err := json.Unmarshal(line, &d); err != nil {
			return nil
		}
		if d.OwnerTarget.Kind != "product_signal_source" {
			return nil
		}
		for _, ref := range d.EvidenceRefs {
			if strings.HasPrefix(ref, "product_id:") {
				ids[strings.TrimPrefix(ref, "product_id:")] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func latestSignalID(signals []Signal) string {
	if len(signals) == 0 {
		return ""
	}
	latest := signals[0]
	for _, signal := range signals[1:] {
		if signal.ObservedAt > latest.ObservedAt || (signal.ObservedAt == latest.ObservedAt && signal.ID > latest.ID) {
			latest = signal
		}
	}
	return latest.ID
}
