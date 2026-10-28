// Package reviewqueue is the canonical builder for the fully-annotated
// review-queue report that the compound CLI's various dispatchers
// (workbench, narrate, attention, reviews-resolve, orient) consume.
//
// Prior to this package, the assembly lived in cmd/compound as
// buildReviewQueueReport -- a 50-line function that called
// reviewartifact.Queue and then ran 24 package-level Annotate* calls
// across 16 internal packages. Eight different CLI dispatchers called
// it, so it was a hidden utility, not an orient-specific helper. This
// package makes the assembly canonical and reachable from anywhere in
// the repo.
//
// Composition:
//
//   - reviewqueue.Build(homePath, opts) returns a reviewartifact.QueueReport
//     fully annotated with every Annotate* pass that the CLI dispatchers
//     depend on.
//   - All callers share the same assembly; the queue report's content
//     is identical regardless of which CLI surface produced it.
//
// Concurrency / clock:
//
//   - The opts.Now field, when non-zero, is the single timestamp used for
//     queue loading AND authority stamping. This avoids the prior shape's
//     time.Now()-called-twice pattern (the loader and the stamper could
//     disagree by microseconds and produce internally-inconsistent
//     snapshots).
//   - When opts.Now is the zero value, the package falls back to
//     time.Now() at the start of Build, so callers without a supplied
//     `now` still work.
//
// Synthesis annotation:
//
//   - The synthesis annotator (cmd/compound/synthesis.go) is NOT included
//     here -- it is a synthesis concern, not a queue-assembly concern.
//     Callers that need synthesis annotations should run them on top of
//     the report returned by Build. The legacy buildReviewQueueReport
//     wrapper in cmd/compound (which 8 dispatchers call) preserves the
//     "synthesis for free" behavior by running both Build and the
//     synthesis annotator back-to-back.
package reviewqueue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/authority"
	"compound/internal/capgraph"
	"compound/internal/decisions"
	"compound/internal/director"
	"compound/internal/distiller"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/observation"
	"compound/internal/panel"
	"compound/internal/policy"
	"compound/internal/product"
	"compound/internal/projection"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
	"compound/internal/runtimebinding"
	"compound/internal/sourceobs"
	"compound/internal/work"
)

// Options narrows the queue report's read shape and pins the timestamp.
type Options struct {
	Area            string
	IncludeArchived bool
	Now             time.Time
}

const (
	ReviewAuthoritySourceWork   = "source_work"
	ReviewAuthorityFallbackWork = "fallback_work"
)

// ReviewWorkAuthority is the read-only result of applying owner-work binding
// model 3 to one review. SourceWorkID remains provenance even when WorkID
// selects the explicit area fallback because the source is terminal.
type ReviewWorkAuthority struct {
	WorkID         string `json:"work_id"`
	Source         string `json:"source"`
	BindingID      string `json:"binding_id"`
	SourceWorkID   string `json:"source_work_id,omitempty"`
	FallbackWorkID string `json:"fallback_work_id,omitempty"`
}

func ReviewAreaSurface(area string) string {
	return "review-area:" + strings.TrimSpace(area)
}

// Build loads the review queue from COMPOUND_HOME and runs every
// per-domain Annotate* pass that the CLI dispatchers depend on. The
// returned QueueReport is read-only -- callers consume it as a
// projection, never mutate it.
func Build(homePath string, opts Options) (reviewartifact.QueueReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	rqLog := func(label string, t time.Time) {
		if os.Getenv("COMPOUND_PROFILE_REVIEWQUEUE") == "1" {
			fmt.Fprintf(os.Stderr, "[RQ-BUILD] %s: %s\n", label, time.Since(t))
		}
	}
	t0 := time.Now()
	report, err := reviewartifact.Queue(homePath, reviewartifact.QueueOptions{
		Area:            opts.Area,
		IncludeArchived: opts.IncludeArchived,
	}, now)
	rqLog("queue_walk", t0)
	if err != nil {
		return reviewartifact.QueueReport{}, err
	}
	decisions.AnnotateLifecycleRepairQueue(&report)
	rqLog("annot_lifecycle", t0)
	decisions.AnnotateDecisionCandidateQueue(&report)
	rqLog("annot_decisions", t0)
	decisions.AnnotateAutonomyGrantMintQueue(&report, homePath)
	rqLog("annot_autonomy", t0)
	product.AnnotateReviewQueue(&report)
	rqLog("annot_product", t0)
	product.AnnotateOutboundDryRunEvidenceQueue(&report)
	rqLog("annot_outbound", t0)
	product.AnnotateContentQueue(&report)
	rqLog("annot_content", t0)
	authority.AnnotateJobHealthQueue(&report)
	rqLog("annot_job_health", t0)
	authority.AnnotateSourceControlCleanupQueue(&report)
	rqLog("annot_source_control", t0)
	director.AnnotateHarnessEditProposalQueue(&report)
	rqLog("annot_harness_edit", t0)
	runtimebinding.AnnotateRepairQueue(&report)
	rqLog("annot_repair", t0)
	capgraph.AnnotateCapabilityEvaluationQueue(&report)
	rqLog("annot_capgraph1", t0)
	capgraph.AnnotateExternalRepoIntakeQueue(&report)
	capgraph.AnnotateOutboundDistributionVerdictQueue(&report)
	capgraph.AnnotateOutboundDistributionDryRunQueue(&report)
	capgraph.AnnotateSkillCandidateQueue(&report)
	capgraph.AnnotateCapabilityLifecycleProposalQueue(&report)
	capgraph.AnnotateSkillSurfacingQueue(&report)
	rqLog("annot_capgraph2", t0)
	distiller.AnnotateImprovementQueue(&report)
	distiller.AnnotateRequestQueue(&report)
	mem.AnnotateBakeoffQueue(&report)
	observation.AnnotateReviewQueue(&report)
	panel.AnnotateVerdictQueue(&report)
	panel.AnnotateCapabilityFeedbackQueue(&report)
	projection.AnnotateProjectionRegenerationQueue(&report)
	routeevidence.AnnotatePromotionReviewQueue(&report)
	routeevidence.AnnotateRouteOutcomeQueue(&report)
	sourceobs.AnnotateObservationVerdictQueue(&report)
	rqLog("annotators_done", t0)
	if err := StampReviewActionAuthority(homePath, &report, now); err != nil {
		return reviewartifact.QueueReport{}, err
	}
	rqLog("stamp_authority", t0)
	reviewartifact.Recount(&report)
	if err := StampReviewGroupActionAuthority(homePath, &report, now); err != nil {
		return reviewartifact.QueueReport{}, err
	}
	rqLog("group_authority", t0)
	reviewartifact.RefreshSystemicBlocker(&report)
	rqLog("systemic_blocker", t0)
	return report, nil
}

func StampReviewActionAuthority(homePath string, report *reviewartifact.QueueReport, now time.Time) error {
	if report == nil {
		return fmt.Errorf("review authority report required")
	}
	for itemIndex := range report.Items {
		report.Items[itemIndex].AuthorityWorkID = ""
		report.Items[itemIndex].AuthoritySource = ""
		for actionIndex := range report.Items[itemIndex].Actions {
			action := &report.Items[itemIndex].Actions[actionIndex]
			action.PolicyDecision = nil
			action.PolicyAuthority = nil
		}
	}

	cfg, policySource, err := policy.LoadConfig(homePath)
	if err != nil {
		return fmt.Errorf("load current policy for review authority: %w", err)
	}
	// OpenReadOnly: the queue report is assembled by reads only (Get +
	// policy.EvaluateAuthority), so stamping authority must not materialize
	// work/ or events/ as a side effect of building the report.
	workStore, err := work.OpenReadOnly(homePath)
	if err != nil {
		return fmt.Errorf("open work store for review authority: %w", err)
	}

	for itemIndex := range report.Items {
		item := &report.Items[itemIndex]
		resolved, bound, resolveErr := resolveReviewWorkAuthority(homePath, workStore, *item)
		if resolveErr != nil {
			item.BlockedReason = reviewartifact.FirstNonEmpty(item.BlockedReason, resolveErr.Error())
			item.Warnings = reviewartifact.Unique(append(item.Warnings, "owner-work authority blocked: "+resolveErr.Error()))
		} else if bound {
			item.AuthorityWorkID = resolved.WorkID
			item.AuthoritySource = resolved.Source
		}
		for actionIndex := range report.Items[itemIndex].Actions {
			action := &report.Items[itemIndex].Actions[actionIndex]
			if resolveErr != nil {
				continue
			}
			if bound {
				if err := bindReviewActionAuthority(action, resolvedActionAuthority{
					WorkID:         resolved.WorkID,
					AllowedWorkIDs: bindingAllowedWorkIDs(item.WorkBinding),
					BindingIDs:     []string{resolved.BindingID},
					EvidenceRefs:   bindingAuthorityEvidence(item.WorkBinding),
				}); err != nil {
					item.Warnings = reviewartifact.Unique(append(item.Warnings, "owner-work action binding blocked: "+err.Error()))
					continue
				}
			}
			if err := stampReviewAction(action, workStore, cfg, policySource, now); err != nil {
				item.Warnings = reviewartifact.Unique(append(item.Warnings, "owner-work authority stamp blocked: "+err.Error()))
			}
		}
	}
	return nil
}

// StampReviewGroupActionAuthority signs the final post-Recount batch action.
// Domain group summaries (notably panel verdicts) synthesize their action only
// while Recount builds Groups, so item stamping alone can never reach them.
func StampReviewGroupActionAuthority(homePath string, report *reviewartifact.QueueReport, now time.Time) error {
	if report == nil {
		return fmt.Errorf("review authority report required")
	}
	cfg, policySource, err := policy.LoadConfig(homePath)
	if err != nil {
		return fmt.Errorf("load current policy for review group authority: %w", err)
	}
	// OpenReadOnly: read-only assembly, same contract as StampReviewActionAuthority above.
	workStore, err := work.OpenReadOnly(homePath)
	if err != nil {
		return fmt.Errorf("open work store for review group authority: %w", err)
	}
	for groupIndex := range report.Groups {
		group := &report.Groups[groupIndex]
		action := group.SafeBatchAction
		if action == nil {
			continue
		}
		action.PolicyDecision = nil
		action.PolicyAuthority = nil
		resolved, bound, err := resolveReviewGroupAuthority(homePath, workStore, *group, report.Items)
		if err != nil {
			group.TerminalReason = reviewartifact.FirstNonEmpty(group.TerminalReason, err.Error())
			continue
		}
		if bound {
			if err := bindReviewActionAuthority(action, resolved); err != nil {
				group.TerminalReason = reviewartifact.FirstNonEmpty(group.TerminalReason, err.Error())
				continue
			}
		}
		if err := stampReviewAction(action, workStore, cfg, policySource, now); err != nil {
			group.TerminalReason = reviewartifact.FirstNonEmpty(group.TerminalReason, "owner-work authority stamp blocked: "+err.Error())
		}
	}
	return nil
}

// ResolveReviewWorkAuthority reads only the current work store and the
// already-loaded binding. It never creates, reopens, advances, or guesses work.
func ResolveReviewWorkAuthority(homePath string, item reviewartifact.QueueItem) (ReviewWorkAuthority, error) {
	// OpenReadOnly honors the read-only contract in the doc above: this resolves
	// authority off the current work store without creating or advancing it.
	workStore, err := work.OpenReadOnly(homePath)
	if err != nil {
		return ReviewWorkAuthority{}, fmt.Errorf("open work store for review authority: %w", err)
	}
	resolved, bound, err := resolveReviewWorkAuthority(homePath, workStore, item)
	if err != nil {
		return ReviewWorkAuthority{}, err
	}
	if !bound {
		return ReviewWorkAuthority{}, fmt.Errorf("review %s/%s has no explicit owner-work binding", item.Area, item.ID)
	}
	return resolved, nil
}

func resolveReviewWorkAuthority(homePath string, workStore *work.Store, item reviewartifact.QueueItem) (ReviewWorkAuthority, bool, error) {
	if strings.TrimSpace(item.WorkBindingError) != "" {
		return ReviewWorkAuthority{}, true, fmt.Errorf("review %s/%s owner-work binding invalid: %s", item.Area, item.ID, item.WorkBindingError)
	}
	binding := item.WorkBinding
	if binding == nil {
		return ReviewWorkAuthority{}, false, nil
	}
	if binding.Area != item.Area || binding.ReviewID != item.ID {
		return ReviewWorkAuthority{}, true, fmt.Errorf("review owner-work binding identity does not match %s/%s", item.Area, item.ID)
	}
	if binding.SourceRun != nil && strings.TrimSpace(binding.SourceRun.WorkID) != "" &&
		strings.TrimSpace(binding.SourceRun.WorkID) != strings.TrimSpace(binding.SourceWorkID) {
		return ReviewWorkAuthority{}, true, fmt.Errorf("review owner-work binding source_run work does not match declared source work")
	}
	if err := validateReviewSourceRun(homePath, item, binding); err != nil {
		return ReviewWorkAuthority{}, true, err
	}
	result := ReviewWorkAuthority{
		BindingID:      binding.ID,
		SourceWorkID:   strings.TrimSpace(binding.SourceWorkID),
		FallbackWorkID: strings.TrimSpace(binding.FallbackWorkID),
	}
	if result.SourceWorkID != "" {
		source, err := workStore.Get(result.SourceWorkID)
		if err != nil {
			return ReviewWorkAuthority{}, true, fmt.Errorf("declared source work %s is missing or unreadable: %w", result.SourceWorkID, err)
		}
		if source.ID != result.SourceWorkID || source.Validate() != nil {
			return ReviewWorkAuthority{}, true, fmt.Errorf("declared source work %s is corrupt", result.SourceWorkID)
		}
		if !reviewAuthorityTerminalWorkState(source.State) {
			result.WorkID = result.SourceWorkID
			result.Source = ReviewAuthoritySourceWork
			return result, true, nil
		}
		if result.FallbackWorkID == "" {
			return ReviewWorkAuthority{}, true, fmt.Errorf("terminal source work %s requires an explicit area fallback", result.SourceWorkID)
		}
	}
	fallback, err := loadReviewAreaFallback(workStore, item.Area, result.FallbackWorkID)
	if err != nil {
		return ReviewWorkAuthority{}, true, err
	}
	result.WorkID = fallback.ID
	result.Source = ReviewAuthorityFallbackWork
	return result, true, nil
}

func validateReviewSourceRun(homePath string, item reviewartifact.QueueItem, binding *reviewartifact.ReviewWorkBinding) error {
	if binding == nil || binding.SourceRun == nil {
		return nil
	}
	data, err := os.ReadFile(item.JSONPath)
	if err != nil {
		return fmt.Errorf("review source artifact unreadable while validating source_run: %w", err)
	}
	var source struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(data, &source); err != nil {
		return fmt.Errorf("review source artifact invalid while validating source_run: %w", err)
	}
	if strings.TrimSpace(source.RunID) != strings.TrimSpace(binding.SourceRun.ID) {
		return fmt.Errorf("review source_run %s does not match core artifact run_id %s", binding.SourceRun.ID, source.RunID)
	}
	switch binding.SourceRun.Kind {
	case "panel_run":
		runData, err := os.ReadFile(filepath.Join(homePath, "panel-runs", binding.SourceRun.ID+".json"))
		if err != nil {
			return fmt.Errorf("panel source_run %s missing or invalid: %w", binding.SourceRun.ID, err)
		}
		run, err := panel.Decode(runData)
		if err != nil {
			return fmt.Errorf("panel source_run %s missing or invalid: %w", binding.SourceRun.ID, err)
		}
		if run.ID != binding.SourceRun.ID {
			return fmt.Errorf("panel source_run identity mismatch")
		}
		if run.WorkBound != (strings.TrimSpace(binding.SourceRun.WorkID) != "") ||
			strings.TrimSpace(run.WorkID) != strings.TrimSpace(binding.SourceRun.WorkID) {
			return fmt.Errorf("panel source_run %s work binding mismatch", binding.SourceRun.ID)
		}
	default:
		return fmt.Errorf("review source_run kind %q has no verifier", binding.SourceRun.Kind)
	}
	return nil
}

func loadReviewAreaFallback(workStore *work.Store, area, workID string) (envelope.WorkEnvelope, error) {
	workID = strings.TrimSpace(workID)
	if workID == "" {
		return envelope.WorkEnvelope{}, fmt.Errorf("review area %s has no explicit fallback work", area)
	}
	fallback, err := workStore.Get(workID)
	if err != nil {
		return envelope.WorkEnvelope{}, fmt.Errorf("review area %s fallback work %s is missing or unreadable: %w", area, workID, err)
	}
	if fallback.ID != workID || fallback.Validate() != nil {
		return envelope.WorkEnvelope{}, fmt.Errorf("review area %s fallback work %s is corrupt", area, workID)
	}
	if reviewAuthorityTerminalWorkState(fallback.State) {
		return envelope.WorkEnvelope{}, fmt.Errorf("review area %s fallback work %s is terminal", area, workID)
	}
	wantSurface := ReviewAreaSurface(area)
	if strings.TrimSpace(fallback.CanonicalSurface) != wantSurface {
		return envelope.WorkEnvelope{}, fmt.Errorf("review area %s fallback work %s canonical surface = %q, want %q", area, workID, fallback.CanonicalSurface, wantSurface)
	}
	return fallback, nil
}

type resolvedActionAuthority struct {
	WorkID         string
	AllowedWorkIDs map[string]bool
	BindingIDs     []string
	EvidenceRefs   []string
}

func resolveReviewGroupAuthority(homePath string, workStore *work.Store, group reviewartifact.QueueGroup, items []reviewartifact.QueueItem) (resolvedActionAuthority, bool, error) {
	memberIDs := make(map[string]bool, len(group.ItemIDs))
	for _, id := range group.ItemIDs {
		memberIDs[id] = true
	}
	var result resolvedActionAuthority
	var sawBound bool
	for _, item := range items {
		if item.Area != group.Area || !memberIDs[item.ID] {
			continue
		}
		resolved, bound, err := resolveReviewWorkAuthority(homePath, workStore, item)
		if err != nil {
			return resolvedActionAuthority{}, true, err
		}
		if !bound {
			if sawBound {
				return resolvedActionAuthority{}, true, fmt.Errorf("review group %s mixes bound and unbound members", group.Key)
			}
			continue
		}
		if !sawBound {
			result.WorkID = resolved.WorkID
			result.AllowedWorkIDs = map[string]bool{}
			sawBound = true
		} else if result.WorkID != resolved.WorkID {
			return resolvedActionAuthority{}, true, fmt.Errorf("review group %s resolves to multiple authority work items", group.Key)
		}
		for workID := range bindingAllowedWorkIDs(item.WorkBinding) {
			result.AllowedWorkIDs[workID] = true
		}
		result.BindingIDs = append(result.BindingIDs, resolved.BindingID)
		result.EvidenceRefs = append(result.EvidenceRefs, bindingAuthorityEvidence(item.WorkBinding)...)
	}
	if sawBound {
		sort.Strings(result.BindingIDs)
		result.BindingIDs = reviewartifact.Unique(result.BindingIDs)
		result.EvidenceRefs = reviewartifact.UniqueSorted(result.EvidenceRefs)
	}
	return result, sawBound, nil
}

func bindingAllowedWorkIDs(binding *reviewartifact.ReviewWorkBinding) map[string]bool {
	allowed := map[string]bool{}
	if binding == nil {
		return allowed
	}
	for _, workID := range []string{binding.SourceWorkID, binding.FallbackWorkID} {
		if workID = strings.TrimSpace(workID); workID != "" {
			allowed[workID] = true
		}
	}
	return allowed
}

func bindingAuthorityEvidence(binding *reviewartifact.ReviewWorkBinding) []string {
	if binding == nil {
		return nil
	}
	refs := append([]string{}, binding.EvidenceRefs...)
	refs = append(refs, "review-work-binding:"+binding.ID)
	if binding.SourceWorkID != "" {
		refs = append(refs, "review-source-work:"+binding.SourceWorkID)
	}
	if binding.FallbackWorkID != "" {
		refs = append(refs, "review-fallback-work:"+binding.FallbackWorkID)
	}
	if binding.SourceRun != nil {
		refs = append(refs, "review-source-run:"+binding.SourceRun.Kind+":"+binding.SourceRun.ID)
	}
	return reviewartifact.UniqueSorted(refs)
}

func bindReviewActionAuthority(action *reviewartifact.Action, resolved resolvedActionAuthority) error {
	if action == nil || action.ActionIntent == nil || action.AuthorityContext == nil {
		return nil
	}
	oldWorkID := strings.TrimSpace(action.ActionIntent.IDs.WorkID)
	if oldWorkID != "" && !resolved.AllowedWorkIDs[oldWorkID] {
		return fmt.Errorf("action work %s is not the review source or area fallback", oldWorkID)
	}
	intent := *action.ActionIntent
	intent.EvidenceRefs = append([]string{}, action.ActionIntent.EvidenceRefs...)
	intent.IDs.WorkID = resolved.WorkID
	intent.ID = reboundReviewActionID(action.ID, intent, resolved.BindingIDs)
	intent.EvidenceRefs = reviewartifact.UniqueSorted(append(intent.EvidenceRefs, resolved.EvidenceRefs...))
	context := *action.AuthorityContext
	context.ExpectedProof = reviewartifact.UniqueSorted(append(append([]string{}, action.AuthorityContext.ExpectedProof...), resolved.EvidenceRefs...))
	context.EvidenceRefs = reviewartifact.UniqueSorted(append(append([]string{}, action.AuthorityContext.EvidenceRefs...), resolved.EvidenceRefs...))
	action.ActionIntent = &intent
	action.AuthorityContext = &context
	return nil
}

func reboundReviewActionID(actionID string, intent envelope.ActionIntent, bindingIDs []string) string {
	intent.ID = ""
	intent.IDs.WorkID = ""
	filtered := make([]string, 0, len(intent.EvidenceRefs))
	for _, ref := range intent.EvidenceRefs {
		if !strings.HasPrefix(ref, "review-work-binding:") &&
			!strings.HasPrefix(ref, "review-source-work:") &&
			!strings.HasPrefix(ref, "review-fallback-work:") &&
			!strings.HasPrefix(ref, "review-source-run:") {
			filtered = append(filtered, ref)
		}
	}
	intent.EvidenceRefs = reviewartifact.UniqueSorted(filtered)
	body, _ := json.Marshal(intent)
	ids := append([]string{}, bindingIDs...)
	sort.Strings(ids)
	return "act_" + reviewartifact.Digest12(strings.Join([]string{actionID, string(body), strings.Join(ids, "\x00")}, "\x00"))
}

func stampReviewAction(action *reviewartifact.Action, workStore *work.Store, cfg policy.Config, policySource string, now time.Time) error {
	if action == nil || action.ActionIntent == nil || action.AuthorityContext == nil {
		return fmt.Errorf("complete producer ActionIntent and AuthorityContext required")
	}
	if err := action.ActionIntent.Validate(); err != nil {
		return fmt.Errorf("producer ActionIntent invalid: %w", err)
	}
	workID := strings.TrimSpace(action.ActionIntent.IDs.WorkID)
	if !strings.HasPrefix(workID, "work_") || strings.HasPrefix(workID, "work_live_") || filepath.Base(workID) != workID {
		return fmt.Errorf("action work id %q is not durable", workID)
	}
	owner, err := workStore.Get(workID)
	if err != nil {
		return fmt.Errorf("action work %s unreadable: %w", workID, err)
	}
	if owner.ID != workID {
		return fmt.Errorf("action work id mismatch")
	}
	if err := owner.Validate(); err != nil {
		return fmt.Errorf("action work %s invalid: %w", workID, err)
	}
	if reviewAuthorityTerminalWorkState(owner.State) {
		return fmt.Errorf("action work %s is terminal", workID)
	}
	evaluation := policy.EvaluateAuthority(
		*action.ActionIntent,
		cfg,
		policy.EvaluationContext{Authority: action.AuthorityContext},
		policySource,
		now,
	)
	if err := policy.ValidateAuthorityBundle(*action.ActionIntent, evaluation.Decision, evaluation.Authority); err != nil {
		return fmt.Errorf("evaluated authority bundle invalid: %w", err)
	}
	action.PolicyDecision = &evaluation.Decision
	action.PolicyAuthority = &evaluation.Authority
	return nil
}

func reviewAuthorityTerminalWorkState(state string) bool {
	switch strings.TrimSpace(state) {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}
