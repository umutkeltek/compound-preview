package routeevidencecli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/routeevidence"
)

// ModelCapabilitiesUsage describes the read/projection/drift-check surface.
func ModelCapabilitiesUsage() string {
	return "usage: compound route model-capabilities [--json] [--yaml] [--check-view <view.yaml>]"
}

// RunModelCapabilities renders Compound-owned model-router authority or checks
// an external compatibility view. It owns no process exits so the thin command
// package only maps the returned code onto its process boundary.
func RunModelCapabilities(args []string, homePath string, stdout, stderr io.Writer) int {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", "--yaml":
		case "--check-view":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(stderr, ModelCapabilitiesUsage())
				return 2
			}
		default:
			fmt.Fprintln(stderr, ModelCapabilitiesUsage())
			return 2
		}
	}

	if viewPath := strings.TrimSpace(flagValue(args, "--check-view")); viewPath != "" {
		check, err := routeevidence.CheckModelCapabilitiesView(homePath, viewPath)
		if err != nil {
			fmt.Fprintln(stderr, "route model-capabilities --check-view:", err)
			return 1
		}
		if hasFlag(args, "--json") {
			if err := writeJSON(stdout, check); err != nil {
				fmt.Fprintln(stderr, "route model-capabilities --check-view:", err)
				return 1
			}
		} else {
			fmt.Fprintln(stdout, "# route model-capabilities --check-view")
			fmt.Fprintf(stdout, "- authority: %s\n", check.AuthorityRef)
			fmt.Fprintf(stdout, "- authority_revision: %s\n", check.AuthorityRevision)
			fmt.Fprintf(stdout, "- view: %s\n", check.ViewPath)
			fmt.Fprintf(stdout, "- view_revision: %s\n", check.ViewRevision)
			for _, drift := range check.Drifts {
				fmt.Fprintf(stdout, "- drift: %s\n", drift)
			}
			if check.InSync {
				fmt.Fprintln(stdout, "- verdict: in_sync")
			} else {
				fmt.Fprintf(stdout, "- verdict: drifted (%d)\n", len(check.Drifts))
			}
		}
		if !check.InSync {
			return 1
		}
		return 0
	}

	if hasFlag(args, "--yaml") {
		body, err := routeevidence.ProjectModelCapabilitiesYAML(homePath)
		if err != nil {
			fmt.Fprintln(stderr, "route model-capabilities --yaml:", err)
			return 1
		}
		if _, err := stdout.Write(body); err != nil {
			fmt.Fprintln(stderr, "route model-capabilities --yaml:", err)
			return 1
		}
		return 0
	}

	report, err := routeevidence.ProjectModelCapabilities(homePath, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "route model-capabilities:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "route model-capabilities:", err)
			return 1
		}
		return 0
	}

	fmt.Fprintln(stdout, "# route model-capabilities")
	fmt.Fprintf(stdout, "- authority: %s\n", report.AuthorityRef)
	fmt.Fprintf(stdout, "- authority_path: %s\n", report.AuthorityPath)
	if report.SourceAuthorityID != "" {
		fmt.Fprintf(stdout, "- source_authority: %s\n", report.SourceAuthorityID)
	}
	fmt.Fprintf(stdout, "- models: %d\n", report.ModelCount)
	fmt.Fprintf(stdout, "- capability_classes: %s\n", strings.Join(report.CapabilityClasses, ","))
	fmt.Fprintf(stdout, "- routing_defaults: %d\n", report.RoutingDefaultCount)
	for _, lane := range report.Lanes {
		fmt.Fprintf(stdout, "- lane: %s tier=%s cost_class=%s harness=%s provider_route=%s capabilities=%d\n",
			lane.ModelID,
			emptyDash(lane.Tier),
			emptyDash(lane.CostClass),
			emptyDash(lane.Harness),
			emptyDash(lane.ProviderRoute),
			len(lane.Capabilities),
		)
	}
	fmt.Fprintln(stdout, "- verdict: read_only_projection")
	return 0
}

// The governed model-router promotion door.
//
// `route promotion-record` stays hold-only (it records a reviewed
// hold_insufficient_evidence decision and reports authority_mutation: none).
// These two verbs are the separate governed door:
//
//	promotion-apply   reviewed promote/demote -> one claim in compound://registries/model-router
//	promotion-revert  the one-line undo, restoring the exact captured pre-image
//
// Both are DRY-RUN by default. --confirm requires --work and an ALLOWING
// PolicyDecision; without one the act seam refuses and the authority stays
// byte-identical.

func PromotionApplyUsage() string {
	return "usage: compound route promotion-apply --review <route_promotion.json> --rationale <text> --work W [--confirm] [--home P] [--json]"
}

func PromotionRevertUsage() string {
	return "usage: compound route promotion-revert --applied <route_promotion.applied.json> --work W [--confirm] [--home P] [--json]"
}

func RunPromotionApply(args []string, defaultHome string, stdout, stderr io.Writer) int {
	reviewPath := strings.TrimSpace(flagValue(args, "--review"))
	rationale := strings.TrimSpace(flagValue(args, "--rationale"))
	if reviewPath == "" || rationale == "" {
		fmt.Fprintln(stderr, PromotionApplyUsage())
		return 2
	}
	homePath, err := resolveRoutePromotionHome(args, defaultHome)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	plan, err := routeevidence.PlanPromotionApply(homePath, reviewPath, rationale)
	if err != nil {
		fmt.Fprintln(stderr, "route promotion-apply:", err)
		return 1
	}

	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	now := time.Now()
	asJSON := hasFlag(args, "--json")
	work := strings.TrimSpace(flagValue(args, "--work"))

	if !hasFlag(args, "--confirm") {
		previewWork := work
		if previewWork == "" {
			previewWork = "work_dryrun"
		}
		ai := routeevidence.PromotionApplyIntent(previewWork, plan, now)
		pd := policy.Evaluate(ai, cfg, now)
		return printRoutePromotionApplyPreview(stdout, plan, ai, pd, asJSON)
	}

	if work == "" {
		fmt.Fprintln(stderr, "error: --work W is required for --confirm (a governed authority mutation belongs to a work)")
		return 2
	}
	ai := routeevidence.PromotionApplyIntent(work, plan, now)
	pd := policy.Evaluate(ai, cfg, now)
	if !policy.Allowed(pd) {
		fmt.Fprintf(stderr, "refused: policy outcome %s for capability %q on %s — no standing grant covers this governed authority write.\n",
			pd.Outcome, routeevidence.PromotionApplyCapability, plan.AuthorityPath)
		for _, reason := range pd.Reasons {
			fmt.Fprintf(stderr, "  - %s\n", reason)
		}
		fmt.Fprintln(stderr, "next:")
		fmt.Fprintln(stderr, "  compound policy standing-grants-authority --json   # which capabilities already carry a standing grant in this home")
		fmt.Fprintf(stderr, "  compound route promotion-apply --review %s --rationale %q --work %s   # without --confirm: dry-run preview, writes nothing\n", reviewPath, rationale, work)
		return 1
	}

	res, err := routeevidence.ApplyPromotionReview(homePath, work, plan, pd, now)
	if err != nil {
		fmt.Fprintln(stderr, "refused:", err)
		return 1
	}
	return printRoutePromotionApplyResult(stdout, res, asJSON)
}

func RunPromotionRevert(args []string, defaultHome string, stdout, stderr io.Writer) int {
	appliedPath := strings.TrimSpace(flagValue(args, "--applied"))
	if appliedPath == "" {
		fmt.Fprintln(stderr, PromotionRevertUsage())
		return 2
	}
	homePath, err := resolveRoutePromotionHome(args, defaultHome)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	plan, err := routeevidence.PlanPromotionRevert(homePath, appliedPath)
	if err != nil {
		fmt.Fprintln(stderr, "route promotion-revert:", err)
		return 1
	}
	record, err := routeevidence.LoadPromotionAppliedRecord(appliedPath)
	if err != nil {
		fmt.Fprintln(stderr, "route promotion-revert:", err)
		return 1
	}

	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	now := time.Now()
	asJSON := hasFlag(args, "--json")
	work := strings.TrimSpace(flagValue(args, "--work"))

	if !hasFlag(args, "--confirm") {
		previewWork := work
		if previewWork == "" {
			previewWork = "work_dryrun"
		}
		ai := routeevidence.PromotionRevertIntent(previewWork, record, now)
		pd := policy.Evaluate(ai, cfg, now)
		return printRoutePromotionRevertPreview(stdout, plan, ai, pd, asJSON)
	}

	if work == "" {
		fmt.Fprintln(stderr, "error: --work W is required for --confirm (a governed authority mutation belongs to a work)")
		return 2
	}
	ai := routeevidence.PromotionRevertIntent(work, record, now)
	pd := policy.Evaluate(ai, cfg, now)
	if !policy.Allowed(pd) {
		fmt.Fprintf(stderr, "refused: policy outcome %s for capability %q on %s — no standing grant covers this governed authority revert.\n",
			pd.Outcome, ai.Capability, plan.AuthorityPath)
		for _, reason := range pd.Reasons {
			fmt.Fprintf(stderr, "  - %s\n", reason)
		}
		return 1
	}

	res, err := routeevidence.RevertPromotionApply(homePath, work, plan, pd, now)
	if err != nil {
		fmt.Fprintln(stderr, "refused:", err)
		return 1
	}
	return printRoutePromotionRevertResult(stdout, res, asJSON)
}

// resolveRoutePromotionHome honors an explicit --home and otherwise keeps the
// already-resolved process home, so these verbs never invent a home path.
func resolveRoutePromotionHome(args []string, defaultHome string) (string, error) {
	if explicit := strings.TrimSpace(flagValue(args, "--home")); explicit != "" {
		return home.Resolve(explicit)
	}
	if strings.TrimSpace(defaultHome) == "" {
		return home.Resolve("")
	}
	return defaultHome, nil
}

func printRoutePromotionApplyPreview(stdout io.Writer, plan routeevidence.PromotionApplyPlan, ai envelope.ActionIntent, pd envelope.PolicyDecision, asJSON bool) int {
	allowed := policy.Allowed(pd)
	if asJSON {
		if err := encodeJSON(stdout, map[string]any{
			"mode":                    "dry-run",
			"authority_mutation":      "none",
			"review_id":               plan.ReviewID,
			"recommendation":          plan.Recommendation,
			"lane_id":                 plan.LaneID,
			"capability_class":        plan.CapabilityClass,
			"current_claim_level":     plan.CurrentClaimLevel,
			"proposed_claim_level":    plan.ProposedClaimLevel,
			"proposed_claim_evidence": plan.ProposedEvidence,
			"authority_ref":           plan.AuthorityRef,
			"authority_path":          plan.AuthorityPath,
			"authority_revision":      plan.AuthorityRevision,
			"proposed_revision":       plan.ProposedRevision,
			"action_intent_id":        ai.ID,
			"policy_decision_id":      pd.ID,
			"policy_outcome":          pd.Outcome,
			"allowed":                 allowed,
		}); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# route promotion-apply (dry-run — nothing written)")
	fmt.Fprintf(stdout, "- review: %s (%s)\n", plan.ReviewID, plan.Recommendation)
	fmt.Fprintf(stdout, "- claim: %s/%s %q -> %q\n", plan.LaneID, plan.CapabilityClass, plan.CurrentClaimLevel, plan.ProposedClaimLevel)
	fmt.Fprintf(stdout, "- authority: %s (%s)\n", plan.AuthorityRef, plan.AuthorityPath)
	fmt.Fprintf(stdout, "- authority_revision: %s\n", plan.AuthorityRevision)
	fmt.Fprintf(stdout, "- proposed_revision: %s\n", plan.ProposedRevision)
	fmt.Fprintf(stdout, "- action_intent: %s\n", ai.ID)
	fmt.Fprintf(stdout, "- policy: %s (%s)\n", pd.Outcome, allowedLabel(allowed))
	if allowed {
		fmt.Fprintln(stdout, "- next: rerun with --confirm --work W to perform the governed write")
	} else {
		fmt.Fprintln(stdout, "- next: no standing grant covers this capability; --confirm would refuse and change nothing")
	}
	fmt.Fprintln(stdout, "- authority_mutation: none")
	return 0
}

func printRoutePromotionApplyResult(stdout io.Writer, res routeevidence.PromotionApplyResult, asJSON bool) int {
	if asJSON {
		if err := encodeJSON(stdout, res.Record); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	rec := res.Record
	fmt.Fprintln(stdout, "# route promotion-apply")
	fmt.Fprintf(stdout, "- review: %s (%s)\n", rec.ReviewID, rec.Recommendation)
	fmt.Fprintf(stdout, "- claim: %s/%s %q -> %q\n", rec.LaneID, rec.CapabilityClass, rec.PriorClaimLevel, rec.AppliedClaimLevel)
	fmt.Fprintf(stdout, "- authority: %s\n", rec.AuthorityRef)
	fmt.Fprintf(stdout, "- authority_revision: %s -> %s\n", rec.AuthorityRevisionBefore, rec.AuthorityRevisionAfter)
	fmt.Fprintf(stdout, "- action_intent: %s\n", rec.ActionIntentID)
	fmt.Fprintf(stdout, "- policy_decision: %s\n", rec.PolicyDecisionID)
	fmt.Fprintf(stdout, "- receipt: %s\n", rec.ReceiptID)
	fmt.Fprintf(stdout, "- transition: %s\n", rec.TransitionID)
	fmt.Fprintf(stdout, "- pre_image: %s\n", rec.PreImageRef)
	fmt.Fprintf(stdout, "- post_image: %s\n", rec.PostImageRef)
	fmt.Fprintf(stdout, "- applied_record: %s\n", rec.RecordPath)
	fmt.Fprintf(stdout, "- undo: %s\n", rec.UndoCommand)
	fmt.Fprintln(stdout, "- authority_mutation: applied")
	return 0
}

func printRoutePromotionRevertPreview(stdout io.Writer, plan routeevidence.PromotionRevertPlan, ai envelope.ActionIntent, pd envelope.PolicyDecision, asJSON bool) int {
	allowed := policy.Allowed(pd)
	if asJSON {
		if err := encodeJSON(stdout, map[string]any{
			"mode":                 "dry-run",
			"authority_mutation":   "none",
			"review_id":            plan.ReviewID,
			"applied_record_id":    plan.AppliedRecordID,
			"lane_id":              plan.LaneID,
			"capability_class":     plan.CapabilityClass,
			"current_claim_level":  plan.CurrentClaimLevel,
			"restored_claim_level": plan.RestoredClaimLevel,
			"authority_ref":        plan.AuthorityRef,
			"authority_path":       plan.AuthorityPath,
			"pre_image_ref":        plan.PreImageRef,
			"post_image_ref":       plan.PostImageRef,
			"action_intent_id":     ai.ID,
			"policy_decision_id":   pd.ID,
			"policy_outcome":       pd.Outcome,
			"allowed":              allowed,
		}); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# route promotion-revert (dry-run — nothing written)")
	fmt.Fprintf(stdout, "- applied_record: %s (review %s)\n", plan.AppliedRecordID, plan.ReviewID)
	fmt.Fprintf(stdout, "- claim: %s/%s %q -> %q\n", plan.LaneID, plan.CapabilityClass, plan.CurrentClaimLevel, plan.RestoredClaimLevel)
	fmt.Fprintf(stdout, "- pre_image: %s\n", plan.PreImageRef)
	fmt.Fprintf(stdout, "- action_intent: %s\n", ai.ID)
	fmt.Fprintf(stdout, "- policy: %s (%s)\n", pd.Outcome, allowedLabel(allowed))
	fmt.Fprintln(stdout, "- authority_mutation: none")
	return 0
}

func printRoutePromotionRevertResult(stdout io.Writer, res routeevidence.PromotionRevertResult, asJSON bool) int {
	if asJSON {
		if err := encodeJSON(stdout, res); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# route promotion-revert")
	fmt.Fprintf(stdout, "- applied_record: %s (review %s)\n", res.Plan.AppliedRecordID, res.Plan.ReviewID)
	fmt.Fprintf(stdout, "- claim: %s/%s restored to %q\n", res.Plan.LaneID, res.Plan.CapabilityClass, res.Plan.RestoredClaimLevel)
	fmt.Fprintf(stdout, "- action_intent: %s\n", res.IntentID)
	fmt.Fprintf(stdout, "- policy_decision: %s\n", res.PolicyID)
	fmt.Fprintf(stdout, "- receipt: %s\n", res.ReceiptID)
	fmt.Fprintf(stdout, "- transition: %s\n", res.TransitionID)
	fmt.Fprintln(stdout, "- authority_mutation: reverted")
	return 0
}

func allowedLabel(allowed bool) string {
	if allowed {
		return "ALLOWED"
	}
	return "NOT allowed"
}

func flagValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func encodeJSON(w io.Writer, value any) error {
	return writeJSON(w, value)
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
