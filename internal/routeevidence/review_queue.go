package routeevidence

import (
	"encoding/json"
	"os"
	"strings"

	"compound/internal/envelope"
	"compound/internal/reviewartifact"
)

const routeOutcomeReviewArea = "route-remorse"

// AnnotateRouteOutcomeQueue fills in the operator card for route-remorse
// (RouteOutcomeReview) review artifacts. There is no separate applied/reviewed
// sidecar for this area; the artifact's own status (set once at creation by
// BuildRouteOutcomeReview) is already terminal ("recorded") or open
// ("review_required"), so the existing band is trusted as-is.
func AnnotateRouteOutcomeQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != routeOutcomeReviewArea {
			continue
		}
		annotateRouteOutcomeQueueItem(item)
	}
}

func annotateRouteOutcomeQueueItem(item *reviewartifact.QueueItem) {
	if item.JSONPath == "" {
		return
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return
	}
	var review RouteOutcomeReview
	if err := json.Unmarshal(data, &review); err != nil || review.SchemaVersion != RouteOutcomeReviewSchemaVersion {
		return
	}
	card, err := OperatorCardForRouteOutcomeReview(review)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "route outcome operator card invalid: "+err.Error())
		return
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, item.Band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "route outcome operator card invalid: "+err.Error())
	}
}

// AnnotatePromotionReviewQueue makes route-promotion review artifacts actionable
// without applying lane promotions or mutating routing authority.
func AnnotatePromotionReviewQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != routePromotionReviewArea {
			continue
		}
		annotatePromotionReviewQueueItem(item)
	}
}

func annotatePromotionReviewQueueItem(item *reviewartifact.QueueItem) {
	review, ok := promotionReviewFromQueueItem(*item)
	if !ok {
		item.Readiness = "blocked"
		item.BlockedReason = "route promotion review artifact unreadable"
		item.NextAction = "regenerate route promotion-review artifacts"
		item.Band = reviewartifact.BandNeedsYou
		item.Actions = nil
		return
	}
	item.EvidenceRefs = reviewartifact.UniqueSorted(review.EvidenceRefs)
	if reviewedPath, holdReport, ok := routePromotionReviewedHold(*item, review); ok {
		item.Status = "recorded"
		item.Readiness = "review_recorded"
		item.BlockedReason = ""
		item.Actions = nil
		item.NextAction = "route promotion hold review recorded"
		// A recorded hold-insufficient-evidence review is still an
		// informational monitoring state, not an applied decision. It must
		// stay in watching, not disappear into filed.
		band := reviewartifact.BandFiled
		summaryOverride := "The route watcher hold review for " + review.LaneID + " is already recorded."
		if review.Recommendation == promotionRecommendationHoldInsufficient {
			band = reviewartifact.BandWatching
			summaryOverride = ""
		}
		annotatePromotionOperatorCard(item, review, band, summaryOverride)
		item.EvidenceRefs = reviewartifact.UniqueSorted(append(item.EvidenceRefs, holdReport.EvidenceRefs...))
		item.NextAction = reviewartifact.FirstNonEmpty(holdReport.NextAction, item.NextAction)
		item.ExtraPaths = reviewartifact.UniqueSorted(append(item.ExtraPaths, reviewedPath))
		return
	}
	// Terminal words are owned by reviewartifact.TerminalStatus; the switch
	// this replaced listed five of the seven and billed candidate_recorded
	// and completed reviews to the operator as open decisions.
	if reviewartifact.TerminalStatus(review.Status) {
		item.Readiness = "no_action"
		item.NextAction = "route promotion review artifact terminal"
		item.Actions = nil
		annotatePromotionOperatorCard(item, review, reviewartifact.BandFiled, "")
		return
	}
	switch strings.TrimSpace(review.Status) {
	case "review_required":
		item.Readiness = "needs_review"
		item.BlockedReason = ""
		action := promotionAuthorityReviewCleanAction(review.ID)
		if review.Recommendation == promotionRecommendationHoldInsufficient {
			action = promotionRecordCleanAction(item.JSONPath)
		}
		seedPromotionReviewAuthority(&action, *item, review)
		item.Actions = []reviewartifact.Action{action}
		band := reviewartifact.BandNeedsYou
		if review.Recommendation == promotionRecommendationHoldInsufficient {
			band = reviewartifact.BandWatching
		}
		annotatePromotionOperatorCard(item, review, band, "")
		switch review.Recommendation {
		case promotionRecommendationHoldInsufficient:
			item.NextAction = "owner review: hold lane promotion until more route evidence meets criteria"
		case promotionRecommendationHoldFailing:
			item.NextAction = "owner review: hold or demote lane after failing-rate evidence review"
		case promotionRecommendationPromote, promotionRecommendationDemote:
			item.NextAction = "owner review required before separate route/model authority mutation"
		default:
			item.NextAction = "owner review required for route promotion recommendation"
		}
	default:
		item.Readiness = "needs_review"
		item.NextAction = "owner review required for route promotion artifact"
		annotatePromotionOperatorCard(item, review, reviewartifact.BandNeedsYou, "")
		item.Actions = nil
	}
}

func seedPromotionReviewAuthority(action *reviewartifact.Action, item reviewartifact.QueueItem, review PromotionReview) {
	workID := reviewartifact.AuthoritySeedWorkID(item, "")
	if workID == "" {
		return
	}
	capability := "review.route_promotion"
	actionName := "prepare_route_promotion_authority_review"
	mutationClass := "read"
	preview := "Prepare an evidence-linked authority brief for route promotion review " + review.ID + "; routing authority remains unchanged."
	rollback := "No owner state changes; discard the generated instruction packet."
	receipt := "Prepared authority review brief for route promotion review " + review.ID + "."
	if action.ID == "route_promotion_record" {
		actionName = "record_route_promotion_hold"
		mutationClass = "local_reversible"
		preview = "Record the reviewed hold for route promotion review " + review.ID + "; routing authority remains unchanged."
		rollback = "Record a superseding review after additional route evidence arrives."
		receipt = "Recorded reviewed hold for route promotion review " + review.ID + "."
	}
	evidenceRefs := reviewartifact.UniqueSorted(append(append([]string{}, review.EvidenceRefs...), "route-promotion-review:"+review.ID))
	_ = reviewartifact.SeedActionAuthority(action, reviewartifact.ActionAuthoritySeedSpec{
		WorkID: workID, CreatedAt: review.CreatedAt, ProducerName: "compound/internal/routeevidence",
		Capability: capability, Action: actionName,
		Target:        envelope.ActionTarget{Kind: "route_promotion_review", ID: review.ID, Path: item.JSONPath},
		MutationClass: mutationClass, Reversibility: "easy", BlastRadius: "local",
		Preview: preview, RollbackPlan: rollback, Privacy: "private", Risk: "tier1",
		EvidenceRefs: evidenceRefs, TargetScope: "private", ExpectedReceipt: receipt,
		ExpectedProof: append([]string{
			"route-promotion-review:" + review.ID,
			"route-lane:" + review.LaneID,
			"route-capability:" + review.CapabilityClass,
		}, evidenceRefs...),
	})
}

// promotionRecordCleanAction emits the one clean-argv (argv[0]=="compound", no shell)
// action for a promotion review awaiting the owner. The CLI requires --rationale;
// it is deliberately omitted here — a governed renderer appends
// ["--rationale", <operator text>] at execution time, after its own review gate.
// promotion-record only records the reviewed hold; route/model authority never
// mutates from this action.
func promotionRecordCleanAction(reviewPath string) reviewartifact.Action {
	return reviewartifact.Action{
		ID:             "route_promotion_record",
		Label:          "Record reviewed hold",
		Command:        "compound route promotion-record --review '" + reviewPath + "' --rationale '<why>'",
		Argv:           []string{"compound", "route", "promotion-record", "--review", reviewPath},
		RequiresReview: true,
		PolicyNote:     "records the reviewed hold decision only; route/model authority mutation stays a separate explicit act",
	}
}

func promotionAuthorityReviewCleanAction(reviewID string) reviewartifact.Action {
	return reviewartifact.Action{
		ID:             "route_promotion_authority_review",
		Label:          "Prepare authority review",
		Command:        "compound reviews brief-plan --item '" + reviewID + "' --json",
		Argv:           []string{"compound", "reviews", "brief-plan", "--item", reviewID, "--json"},
		RequiresReview: true,
		PolicyNote:     "prepares an evidence-linked decision brief only; route/model authority mutation stays a separate explicit act",
	}
}

func annotatePromotionOperatorCard(item *reviewartifact.QueueItem, review PromotionReview, band, summaryOverride string) {
	card, err := OperatorCardForPromotionReview(review)
	if err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "route promotion operator card invalid: "+err.Error())
		return
	}
	if strings.TrimSpace(summaryOverride) != "" {
		card.Summary = reviewartifact.ShortLabel(summaryOverride, 159)
		if !strings.HasSuffix(card.Summary, ".") {
			card.Summary += "."
		}
		card.Unlock = ""
	}
	if err := reviewartifact.SetOperatorCard(item, card, reviewartifact.SummarySourceAnnotator, band); err != nil {
		item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, "route promotion operator card invalid: "+err.Error())
	}
}

func routePromotionReviewedHold(item reviewartifact.QueueItem, review PromotionReview) (string, PromotionHoldReviewReport, bool) {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, ".reviewed.json") {
			report, err := LoadPromotionHoldReviewReport(path)
			if err != nil {
				continue
			}
			if promotionHoldReviewMatches(review, report) {
				return path, report, true
			}
		}
	}
	return "", PromotionHoldReviewReport{}, false
}

func promotionHoldReviewMatches(review PromotionReview, report PromotionHoldReviewReport) bool {
	if strings.TrimSpace(report.Status) != "recorded" || strings.TrimSpace(report.Decision) != "hold_recorded" {
		return false
	}
	if strings.TrimSpace(report.ReviewID) != strings.TrimSpace(review.ID) {
		return false
	}
	if strings.TrimSpace(report.Recommendation) != strings.TrimSpace(review.Recommendation) {
		return false
	}
	if strings.TrimSpace(report.LaneID) != strings.TrimSpace(review.LaneID) {
		return false
	}
	if strings.TrimSpace(report.CapabilityClass) != strings.TrimSpace(review.CapabilityClass) {
		return false
	}
	return strings.TrimSpace(report.Reason) == strings.TrimSpace(review.Reason)
}

func promotionReviewFromQueueItem(item reviewartifact.QueueItem) (PromotionReview, bool) {
	if strings.TrimSpace(item.JSONPath) == "" {
		return PromotionReview{}, false
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return PromotionReview{}, false
	}
	var review PromotionReview
	if err := json.Unmarshal(data, &review); err != nil {
		return PromotionReview{}, false
	}
	return review, strings.TrimSpace(review.ID) != ""
}
