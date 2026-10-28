package reviewartifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
)

const QueueSchemaVersion = "compound.review_queue.v0.1"

// QueueOptions narrows the read-only review queue.
type QueueOptions struct {
	Area            string
	IncludeArchived bool
}

// QueueReport is a read-only projection over COMPOUND_HOME/reviews.
type QueueReport struct {
	SchemaVersion string       `json:"schema_version"`
	GeneratedAt   string       `json:"generated_at"`
	ReviewRoot    string       `json:"review_root"`
	Area          string       `json:"area,omitempty"`
	Counts        QueueCounts  `json:"counts"`
	Groups        []QueueGroup `json:"groups,omitempty"`
	// SystemicBlocker is set only when EVERY group in the queue is blocked for
	// the exact same reason (e.g. no producer currently seeds an owner authority
	// bundle). It lets a console render one banner instead of stamping the same
	// jargon on every card. It is additive and optional: it never changes
	// SchemaVersion, and consumers that ignore it see the unchanged projection.
	SystemicBlocker *SystemicBlocker `json:"systemic_blocker,omitempty"`
	Items           []QueueItem      `json:"items"`
}

// SystemicBlocker names one reason that blocks the whole decide surface at once.
// It is a projection convenience, not a new authority: the per-group blocked
// reason it summarizes is still computed the same fail-closed way.
type SystemicBlocker struct {
	Reason             string `json:"reason"`
	AffectedGroupCount int    `json:"affected_group_count"`
}

// systemicNoAuthorityReason is the fail-closed reason a group carries no owner
// authority bundle an authority-aware surface can act on: none was seeded, the
// action is unsigned, the bundle is partial, or it is no longer current. All of
// these parse to nothing on the console and stamp one identical face, so this
// string matches the console's wording exactly and one banner can stand in for
// many identical cards.
const systemicNoAuthorityReason = "No complete current owner authority bundle is available."

// QueueCounts summarizes review artifact status without interpreting domain payloads.
type QueueCounts struct {
	Total                             int            `json:"total"`
	ReviewRequired                    int            `json:"review_required"`
	ReadyToApply                      int            `json:"ready_to_apply"`
	OK                                int            `json:"ok"`
	ReviewGroupCount                  int            `json:"review_group_count,omitempty"`
	DecisionCardCount                 int            `json:"decision_card_count"`
	ActionableCount                   int            `json:"actionable_count"`
	ReviewRequiredMissingDecisionCard int            `json:"review_required_missing_decision_card"`
	ReviewRequiredMissingAction       int            `json:"review_required_missing_action"`
	FallbackSummaryCount              int            `json:"fallback_summary_count,omitempty"`
	ReviewRequiredFallbackSummary     int            `json:"review_required_fallback_summary,omitempty"`
	NeedsYouFallbackGroupCount        int            `json:"needs_you_fallback_group_count,omitempty"`
	ByArea                            map[string]int `json:"by_area"`
	ByStatus                          map[string]int `json:"by_status"`
	BySummarySource                   map[string]int `json:"by_summary_source,omitempty"`
}

// QueueGroup is the operator-level review projection: one card per decision
// shape, with item details still available through Items/member_ids.
type QueueGroup struct {
	Key               string            `json:"key"`
	Area              string            `json:"area"`
	Count             int               `json:"count"`
	Status            string            `json:"status,omitempty"`
	Readiness         string            `json:"readiness,omitempty"`
	Band              string            `json:"band,omitempty"`
	OperatorSummary   string            `json:"operator_summary"`
	ContextLabel      string            `json:"context_label,omitempty"`
	OperatorUnlock    string            `json:"operator_unlock,omitempty"`
	SummarySource     string            `json:"summary_source,omitempty"`
	ItemIDs           []string          `json:"item_ids"`
	EvidenceRefs      []string          `json:"evidence_refs,omitempty"`
	EvidenceSummaries []EvidenceSummary `json:"evidence_summaries,omitempty"`
	SafeBatchAction   *Action           `json:"safe_batch_action,omitempty"`
	TerminalReason    string            `json:"terminal_reason,omitempty"`
	Sample            *QueueItem        `json:"sample,omitempty"`
}

type EvidenceSummary struct {
	Ref         string `json:"ref"`
	Label       string `json:"label"`
	SourceKind  string `json:"source_kind"`
	Privacy     string `json:"privacy,omitempty"`
	Summary     string `json:"summary"`
	OpenCommand string `json:"open_command,omitempty"`
}

// QueueItem describes one review artifact JSON payload and nearby review files.
type QueueItem struct {
	Area                    string            `json:"area"`
	ID                      string            `json:"id"`
	Status                  string            `json:"status"`
	OperatorSummary         string            `json:"operator_summary,omitempty"`
	ContextLabel            string            `json:"context_label,omitempty"`
	GroupKey                string            `json:"group_key,omitempty"`
	OperatorUnlock          string            `json:"operator_unlock,omitempty"`
	Band                    string            `json:"band,omitempty"`
	SummarySource           string            `json:"summary_source,omitempty"`
	CreatedAt               string            `json:"created_at,omitempty"`
	JSONPath                string            `json:"json_path"`
	MarkdownPath            string            `json:"markdown_path,omitempty"`
	ExtraPaths              []string          `json:"extra_paths,omitempty"`
	SourceKind              string            `json:"source_kind,omitempty"`
	SourceRoot              string            `json:"source_root,omitempty"`
	SourcePath              string            `json:"source_path,omitempty"`
	TargetPath              string            `json:"target_path,omitempty"`
	TargetHash              string            `json:"target_hash,omitempty"`
	CandidateID             string            `json:"candidate_id,omitempty"`
	Owner                   string            `json:"owner,omitempty"`
	NotToDo                 string            `json:"not_to_do,omitempty"`
	Boundaries              []string          `json:"boundaries,omitempty"`
	RepairCount             int               `json:"repair_count,omitempty"`
	EvidenceRefs            []string          `json:"evidence_refs,omitempty"`
	GraphDecisionHint       string            `json:"graph_decision_hint,omitempty"`
	GraphDecisionConfidence float64           `json:"graph_decision_confidence,omitempty"`
	GraphDuplicateRisk      float64           `json:"graph_duplicate_risk,omitempty"`
	GraphEvidenceStrength   float64           `json:"graph_evidence_strength,omitempty"`
	GraphAuditRisk          string            `json:"graph_audit_risk,omitempty"`
	GraphTopOverlapScore    int               `json:"graph_top_overlap_score,omitempty"`
	Readiness               string            `json:"readiness,omitempty"`
	BlockedReason           string            `json:"blocked_reason,omitempty"`
	NextAction              string            `json:"next_action,omitempty"`
	Actions                 []Action          `json:"actions,omitempty"`
	DecisionCard            *DecisionCard     `json:"decision_card,omitempty"`
	Resolution              *ReviewResolution `json:"resolution,omitempty"`
	// ReviewResolutionRevision is an opaque owner-computed fingerprint of the
	// complete rendered QueueItem plus its area. Clients carry it back when
	// recording a synthetic Park/Dismiss disposition; they never reconstruct
	// it from a selective subset of fields.
	ReviewResolutionRevision string `json:"review_resolution_revision,omitempty"`

	// Brief is the generic, propose-only decision-brief translation attached
	// from the `<id>.brief.json` sidecar, if one exists and is valid — for
	// EVERY area (see loadDecisionBriefSidecar in brief.go). It never affects
	// Status/Band/Readiness above; a brief that tries to inject those fields
	// is rejected outright (see ParseDecisionBrief's strict decode).
	Brief *DecisionBrief `json:"brief,omitempty"`
	// Warnings carries item-level problems that do not block the item itself
	// from queueing (e.g. an invalid `.brief.json` sidecar) — distinct from
	// BlockedReason, which explains why the ITEM's own action is blocked.
	Warnings []string `json:"warnings,omitempty"`
	// WorkBinding is the current append-only owner-work companion for this
	// sealed review artifact. The queue only loads provenance here; active work
	// authority is resolved later by reviewqueue against the current work store.
	WorkBinding      *ReviewWorkBinding `json:"work_binding,omitempty"`
	WorkBindingError string             `json:"work_binding_error,omitempty"`
	AuthorityWorkID  string             `json:"authority_work_id,omitempty"`
	AuthoritySource  string             `json:"authority_source,omitempty"`
}

// Action is a non-mutating operator hint attached by the domain owner that
// knows how a review artifact is safely consumed.
type Action struct {
	ID               string                   `json:"id"`
	Label            string                   `json:"label"`
	Command          string                   `json:"command"`
	Argv             []string                 `json:"argv,omitempty"`
	RequiresReview   bool                     `json:"requires_review,omitempty"`
	PolicyNote       string                   `json:"policy_note,omitempty"`
	ActionIntent     *envelope.ActionIntent   `json:"action_intent,omitempty"`
	PolicyDecision   *envelope.PolicyDecision `json:"policy_decision,omitempty"`
	PolicyAuthority  *policy.AuthorityFace    `json:"policy_authority,omitempty"`
	AuthorityContext *policy.AuthorityContext `json:"-"`
}

// DecisionCard is a read-only, operator-facing explanation of a review item.
// It is deliberately generic so renderers do not parse owner-specific payloads.
type DecisionCard struct {
	Claim               string               `json:"claim"`
	Why                 []DecisionCardReason `json:"why"`
	Choices             []DecisionCardChoice `json:"choices"`
	PanelLean           string               `json:"panel_lean,omitempty"`
	OptionalRationale   bool                 `json:"optional_rationale,omitempty"`
	CanonicalActionHint string               `json:"canonical_action_hint,omitempty"`
	Boundary            []string             `json:"boundary,omitempty"`
	Meta                map[string]string    `json:"meta,omitempty"`
}

type DecisionCardReason struct {
	Label    string `json:"label"`
	Question string `json:"question,omitempty"`
	Severity string `json:"severity,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type DecisionCardChoice struct {
	ID             string   `json:"id"`
	Label          string   `json:"label"`
	Effect         string   `json:"effect"`
	Command        string   `json:"command,omitempty"`
	Argv           []string `json:"argv,omitempty"`
	RequiresReview bool     `json:"requires_review,omitempty"`
}

// Queue scans COMPOUND_HOME/reviews for review artifacts. It never mutates
// artifacts, owner files, ledgers, or promotion state.
func Queue(homePath string, opt QueueOptions, now time.Time) (QueueReport, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return QueueReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	opt.Area = strings.TrimSpace(opt.Area)
	if opt.Area != "" {
		if err := validArea(opt.Area); err != nil {
			return QueueReport{}, err
		}
	}
	root := filepath.Join(homePath, "reviews")
	report := QueueReport{
		SchemaVersion: QueueSchemaVersion,
		GeneratedAt:   Timestamp(now),
		ReviewRoot:    root,
		Area:          opt.Area,
		Counts: QueueCounts{
			ByArea:   map[string]int{},
			ByStatus: map[string]int{},
		},
	}
	walkRoot := root
	if opt.Area != "" {
		walkRoot = filepath.Join(root, filepath.FromSlash(opt.Area))
	}
	err := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".json" {
			return nil
		}
		if !coreReviewJSON(path) {
			return nil
		}
		item, err := queueItem(root, path)
		if err != nil {
			return err
		}
		if opt.Area != "" && item.Area != opt.Area {
			return nil
		}
		report.Items = append(report.Items, item)
		return nil
	})
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return QueueReport{}, err
	}
	// Sidecars and historical rows must use one fresh archive snapshot per queue.
	archiveEntries, err := home.LoadOperationalArchiveIndex(homePath)
	if err != nil {
		return QueueReport{}, err
	}
	if err := attachArchivedReviewExtraPaths(homePath, report.Items, archiveEntries); err != nil {
		return QueueReport{}, err
	}
	if opt.IncludeArchived {
		archived, err := archivedQueueItems(homePath, opt.Area, archiveEntries)
		if err != nil {
			return QueueReport{}, err
		}
		report.Items = append(report.Items, archived...)
	}
	sort.Slice(report.Items, func(i, j int) bool {
		return queueItemLess(report.Items[i], report.Items[j])
	})
	Recount(&report)
	return report, nil
}

// Recount refreshes queue counters after domain annotators adjust item status or
// readiness. The queue is read-only; this mutates only the projection.
func Recount(report *QueueReport) {
	if report == nil {
		return
	}
	ApplyGenericResolutions(report)
	report.Groups = buildQueueGroups(report.Items)
	report.SystemicBlocker = computeSystemicBlocker(report.Groups)
	report.Counts = QueueCounts{
		ByArea:          map[string]int{},
		ByStatus:        map[string]int{},
		BySummarySource: map[string]int{},
	}
	report.Counts.ReviewGroupCount = len(report.Groups)
	for _, group := range report.Groups {
		if group.Band == BandNeedsYou && group.SummarySource == SummarySourceFallback {
			report.Counts.NeedsYouFallbackGroupCount++
		}
	}
	for _, item := range report.Items {
		report.Counts.Total++
		report.Counts.ByArea[item.Area]++
		report.Counts.ByStatus[item.Status]++
		if source := strings.TrimSpace(item.SummarySource); source != "" {
			report.Counts.BySummarySource[source]++
			if source == SummarySourceFallback {
				report.Counts.FallbackSummaryCount++
			}
		}
		if item.Readiness == "ready_to_apply" {
			report.Counts.ReadyToApply++
		}
		if item.DecisionCard != nil {
			report.Counts.DecisionCardCount++
		}
		if len(item.Actions) > 0 {
			report.Counts.ActionableCount++
		}
		if TerminalStatus(item.Status) {
			report.Counts.OK++
		} else {
			report.Counts.ReviewRequired++
			if item.SummarySource == SummarySourceFallback {
				report.Counts.ReviewRequiredFallbackSummary++
			}
			if itemNeedsReviewCard(item) && item.DecisionCard == nil {
				report.Counts.ReviewRequiredMissingDecisionCard++
			}
			if itemNeedsReviewCard(item) && len(item.Actions) == 0 {
				report.Counts.ReviewRequiredMissingAction++
			}
		}
	}
	stampReviewResolutionRevisions(report)
}

// RefreshSystemicBlocker recomputes only the authority-aware group blocker
// after a caller stamps post-Recount domain batch actions.
func RefreshSystemicBlocker(report *QueueReport) {
	if report == nil {
		return
	}
	report.SystemicBlocker = computeSystemicBlocker(report.Groups)
}

func buildQueueGroups(items []QueueItem) []QueueGroup {
	if len(items) == 0 {
		return nil
	}
	byKey := map[string]*QueueGroup{}
	order := []string{}
	for i := range items {
		item := items[i]
		if !itemNeedsReviewGroup(item) {
			continue
		}
		key := queueGroupKey(item)
		mapKey := item.Area + "\x00" + key
		group := byKey[mapKey]
		if group == nil {
			sample := item
			group = &QueueGroup{
				Key:             key,
				Area:            item.Area,
				Status:          item.Status,
				Readiness:       item.Readiness,
				Band:            item.Band,
				OperatorSummary: FirstNonEmpty(item.OperatorSummary, item.NextAction, item.ContextLabel, "This record waits on your decision."),
				ContextLabel:    FirstNonEmpty(item.ContextLabel, item.Area),
				OperatorUnlock:  item.OperatorUnlock,
				SummarySource:   item.SummarySource,
				Sample:          &sample,
			}
			byKey[mapKey] = group
			order = append(order, mapKey)
		}
		group.Count++
		group.ItemIDs = append(group.ItemIDs, item.ID)
		group.EvidenceRefs = append(group.EvidenceRefs, item.EvidenceRefs...)
		group.EvidenceRefs = UniqueSorted(group.EvidenceRefs)
	}
	out := make([]QueueGroup, 0, len(order))
	for _, key := range order {
		group := *byKey[key]
		group.ItemIDs = Unique(group.ItemIDs)
		group.SafeBatchAction, group.TerminalReason = queueGroupAction(group, items)
		group.EvidenceSummaries = queueGroupEvidenceSummaries(group, items)
		applyGroupSummary(&group)
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Band != out[j].Band {
			return bandPriority(out[i].Band) < bandPriority(out[j].Band)
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Area != out[j].Area {
			return out[i].Area < out[j].Area
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func itemNeedsReviewGroup(item QueueItem) bool {
	if item.Status == "" || item.Status == "unknown" || item.Status == "review_required" || item.Readiness == "needs_review" || item.Readiness == "ready_to_apply" || len(item.Actions) > 0 {
		return true
	}
	return !TerminalStatus(item.Status)
}

func queueGroupKey(item QueueItem) string {
	if strings.TrimSpace(item.GroupKey) != "" {
		return strings.TrimSpace(item.GroupKey)
	}
	context := FirstNonEmpty(item.ContextLabel, item.OperatorSummary, item.NextAction, item.ID, "review")
	return FirstNonEmpty(item.Area, "review") + "|" + Slugify(context, 72)
}

func queueGroupAction(group QueueGroup, items []QueueItem) (*Action, string) {
	var action *Action
	var authority actionAuthorityIdentity
	for _, item := range items {
		if item.Area != group.Area || queueGroupKey(item) != group.Key || !itemNeedsReviewGroup(item) {
			continue
		}
		if len(item.Actions) == 0 {
			return nil, emptyGroupActionReason(group)
		}
		if len(item.Actions) != 1 {
			return nil, "group has multiple item actions; review item details before acting"
		}
		next := item.Actions[0]
		nextAuthority, err := authorityIdentity(next)
		if err != nil {
			return nil, "owner authority bundle is incomplete or invalid; review item details before acting"
		}
		if action == nil {
			copy := cloneAction(next)
			action = &copy
			authority = nextAuthority
			continue
		}
		if action.ID != next.ID || action.Command != next.Command || action.RequiresReview != next.RequiresReview || strings.Join(action.Argv, "\x00") != strings.Join(next.Argv, "\x00") || authority != nextAuthority {
			return nil, "actions are item-specific; no safe batch action exposed"
		}
	}
	if action == nil {
		return nil, emptyGroupActionReason(group)
	}
	return action, ""
}

// emptyGroupActionReason is the W5 J9-R3G plain-language fallback the queue
// surfaces when a group has no owner-side safe action. Most areas keep the
// exact legacy wording; the product-hypotheses area — the one a tired operator
// sees when a starved product's draft is waiting on their review — gets a
// sentence that says what the operator is being asked to review and what
// concrete move gets the loop evidence-shaped. The replacement stays below the
// shared eighth-grade readability ceiling (see internal/reviewartifact/readability_test.go).
func emptyGroupActionReason(group QueueGroup) string {
	if strings.TrimSpace(group.Area) == "product-hypotheses" {
		return "This product has a hypothesis ready for your review. Approve it to give the loop real evidence."
	}
	return "owner has not exposed a safe action for this decision group"
}

// groupConsoleBlockedReason returns the reason an authority-aware surface (the
// operator console) cannot act on a group, or "" when the group carries a
// complete, current owner authority bundle (allowed → act now, held → decide
// now: both are operator-decidable). It mirrors the console's fail-closed gate
// so one systemic reason can summarize many identical cards. queueGroupAction
// still exposes an unsigned legacy SafeBatchAction for non-authority consumers,
// so the authority requirement is read back here rather than from the
// batch-construction TerminalReason: a missing action, an unsigned action, a
// partial bundle, and a non-current bundle all parse to nothing on the console
// (authorityIdentity fails closed on the last three) and share one reason.
func groupConsoleBlockedReason(group QueueGroup) string {
	action := group.SafeBatchAction
	if action == nil {
		return systemicNoAuthorityReason
	}
	if identity, err := authorityIdentity(*action); err != nil || !identity.Present {
		return systemicNoAuthorityReason
	}
	return ""
}

// computeSystemicBlocker returns a non-nil summary only when every group in the
// queue is blocked for one identical reason. A single decidable group, or two
// groups blocked for different reasons, leaves it nil so the console keeps
// rendering per-card reasons. It never fabricates an actionable state.
func computeSystemicBlocker(groups []QueueGroup) *SystemicBlocker {
	if len(groups) == 0 {
		return nil
	}
	reason := ""
	for i := range groups {
		got := groupConsoleBlockedReason(groups[i])
		if got == "" {
			return nil
		}
		if reason == "" {
			reason = got
			continue
		}
		if got != reason {
			return nil
		}
	}
	return &SystemicBlocker{Reason: reason, AffectedGroupCount: len(groups)}
}

func cloneAction(action Action) Action {
	clone := action
	clone.Argv = cloneActionStrings(action.Argv)
	if action.ActionIntent != nil {
		intent := *action.ActionIntent
		intent.EvidenceRefs = cloneActionStrings(action.ActionIntent.EvidenceRefs)
		intent.EstimatedCost.USD = cloneActionFloat64(action.ActionIntent.EstimatedCost.USD)
		intent.EstimatedCost.Tokens = cloneActionInt(action.ActionIntent.EstimatedCost.Tokens)
		intent.EstimatedCost.HumanReviewMin = cloneActionInt(action.ActionIntent.EstimatedCost.HumanReviewMin)
		clone.ActionIntent = &intent
	}
	if action.PolicyDecision != nil {
		decision := *action.PolicyDecision
		decision.EvidenceRefs = cloneActionStrings(action.PolicyDecision.EvidenceRefs)
		decision.Reasons = cloneActionStrings(action.PolicyDecision.Reasons)
		decision.Obligations = cloneActionStrings(action.PolicyDecision.Obligations)
		clone.PolicyDecision = &decision
	}
	if action.PolicyAuthority != nil {
		authority := *action.PolicyAuthority
		authority.ExpectedProof = cloneActionStrings(action.PolicyAuthority.ExpectedProof)
		clone.PolicyAuthority = &authority
	}
	if action.AuthorityContext != nil {
		context := *action.AuthorityContext
		context.ExpectedProof = cloneActionStrings(action.AuthorityContext.ExpectedProof)
		context.EvidenceRefs = cloneActionStrings(action.AuthorityContext.EvidenceRefs)
		clone.AuthorityContext = &context
	}
	return clone
}

func cloneActionStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func cloneActionFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneActionInt(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

type actionAuthorityIdentity struct {
	Present        bool
	ActionIntentID string
	FaceRevision   string
}

func authorityIdentity(action Action) (actionAuthorityIdentity, error) {
	present := action.ActionIntent != nil || action.PolicyDecision != nil || action.PolicyAuthority != nil
	if !present {
		return actionAuthorityIdentity{}, nil
	}
	if action.ActionIntent == nil || action.PolicyDecision == nil || action.PolicyAuthority == nil {
		return actionAuthorityIdentity{}, fmt.Errorf("partial authority bundle")
	}
	if err := policy.ValidateAuthorityBundle(*action.ActionIntent, *action.PolicyDecision, *action.PolicyAuthority); err != nil {
		return actionAuthorityIdentity{}, err
	}
	return actionAuthorityIdentity{
		Present:        true,
		ActionIntentID: action.ActionIntent.ID,
		FaceRevision:   action.PolicyAuthority.Revision,
	}, nil
}

func queueGroupEvidenceSummaries(group QueueGroup, items []QueueItem) []EvidenceSummary {
	if len(group.EvidenceRefs) == 0 {
		return nil
	}
	const maxEvidenceSummaries = 12
	out := make([]EvidenceSummary, 0, min(len(group.EvidenceRefs), maxEvidenceSummaries))
	for _, ref := range group.EvidenceRefs {
		if len(out) >= maxEvidenceSummaries {
			break
		}
		item := queueGroupItemForEvidenceRef(group, ref, items)
		out = append(out, evidenceSummary(ref, item))
	}
	return out
}

func queueGroupItemForEvidenceRef(group QueueGroup, ref string, items []QueueItem) QueueItem {
	for _, item := range items {
		if item.Area != group.Area || queueGroupKey(item) != group.Key || !itemNeedsReviewGroup(item) {
			continue
		}
		for _, got := range item.EvidenceRefs {
			if got == ref {
				return item
			}
		}
	}
	if group.Sample != nil {
		return *group.Sample
	}
	return QueueItem{Area: group.Area, ContextLabel: group.ContextLabel, OperatorSummary: group.OperatorSummary}
}

func evidenceSummary(ref string, item QueueItem) EvidenceSummary {
	kind := evidenceSourceKind(ref, item.SourceKind)
	label := evidenceLabel(ref)
	summary := evidenceSummaryText(kind, item)
	return EvidenceSummary{
		Ref:         ref,
		Label:       label,
		SourceKind:  kind,
		Privacy:     evidencePrivacy(ref),
		Summary:     summary,
		OpenCommand: evidenceOpenCommand(ref),
	}
}

func evidenceSummaryText(kind string, item QueueItem) string {
	if item.Area == "panel-verdicts" && item.DecisionCard != nil {
		meta := item.DecisionCard.Meta
		runID := strings.TrimSpace(meta["run_id"])
		targetKind := strings.TrimSpace(meta["target_kind"])
		targetRef := strings.TrimSpace(meta["target_ref"])
		target := strings.Trim(strings.TrimSpace(targetKind+"/"+targetRef), "/")
		lean := PlainLabel(FirstNonEmpty(item.DecisionCard.PanelLean, item.Status, item.Readiness))
		claim := FirstNonEmpty(item.DecisionCard.Claim, item.ContextLabel, item.OperatorSummary)
		var b strings.Builder
		b.WriteString("Panel verdict")
		if runID != "" {
			b.WriteByte(' ')
			b.WriteString(runID)
		}
		if lean != "" {
			b.WriteString(" says ")
			b.WriteString(lean)
		}
		if target != "" {
			b.WriteString(" for ")
			b.WriteString(target)
		}
		if claim != "" {
			b.WriteString(": ")
			b.WriteString(claim)
		}
		return ShortLabel(b.String(), 220)
	}
	context := FirstNonEmpty(item.ContextLabel, item.OperatorSummary, item.NextAction, item.Area, "review item")
	return ShortLabel(fmt.Sprintf("%s supporting %s", PlainLabel(kind), context), 160)
}

func evidenceSourceKind(ref, fallback string) string {
	ref = strings.TrimSpace(ref)
	low := strings.ToLower(ref)
	switch {
	case strings.HasPrefix(low, "ev_"), strings.HasPrefix(low, "evidence:"), strings.HasPrefix(low, "evidence/"):
		return "evidence"
	case strings.HasPrefix(low, "claim:"), strings.HasPrefix(low, "claim_"), strings.HasPrefix(low, "cl_"):
		return "claim"
	case strings.HasPrefix(low, "cand_"), strings.HasPrefix(low, "candidate:"):
		return "candidate"
	case strings.HasPrefix(low, "work_"):
		return "work"
	case strings.HasPrefix(low, "artifact:"):
		return "artifact"
	case strings.HasPrefix(low, "http://"), strings.HasPrefix(low, "https://"):
		return "external_ref"
	case strings.Contains(low, "route"), strings.Contains(low, "comparison"), strings.Contains(low, "model"):
		return "route_evidence"
	}
	if fallback = strings.TrimSpace(fallback); fallback != "" {
		return fallback
	}
	return "reference"
}

func evidencePrivacy(ref string) string {
	low := strings.ToLower(ref)
	switch {
	case strings.Contains(low, "priv"), strings.Contains(low, "private"), strings.Contains(low, "docs_private"), strings.Contains(low, ".compound"):
		return "private"
	case strings.HasPrefix(low, "http://"), strings.HasPrefix(low, "https://"):
		return "public"
	default:
		return "unknown"
	}
}

func evidenceLabel(ref string) string {
	label := strings.TrimSpace(ref)
	if strings.HasPrefix(label, "http://") || strings.HasPrefix(label, "https://") {
		label = strings.TrimRight(label, "/")
		if idx := strings.LastIndex(label, "/"); idx >= 0 {
			label = label[idx+1:]
		}
	}
	for _, prefix := range []string{"artifact:", "evidence:", "claim:", "candidate:"} {
		label = strings.TrimPrefix(label, prefix)
	}
	label = strings.Trim(label, " /")
	label = FirstNonEmpty(PlainLabel(label), "evidence")
	return ShortLabel(label, 64)
}

func evidenceOpenCommand(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return ""
	}
	ref = strings.TrimSpace(ref)
	low := strings.ToLower(ref)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return ""
	}
	return "compound system lineage " + evidenceShellQuote(ref) + " --json"
}

func evidenceShellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>*?()[]{}!") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// GroupSummaryResult is what a domain package's GroupSummaryFunc returns:
// the plain-language group headline/unlock ("group" template lines) and, when the
// area has no safe multi-item verb, the honest reason a batch act is
// unavailable (the batch_act unavailable case must carry a reason).
type GroupSummaryResult struct {
	Summary                string
	Unlock                 string
	BatchUnavailableReason string
	// BatchAction, when set, overrides the default per-item-action-derived
	// SafeBatchAction with a real run/area-scoped batch verb. Domain
	// packages set this only when they can prove the underlying batch
	// command is safe for every member of the group (e.g. a panel run
	// whose status already permits apply).
	BatchAction *Action
}

// GroupSummaryFunc composes a group's operator-facing sentence from the
// already-built QueueGroup (area, count, band, sample). Domain packages
// register one per area whose template documents a distinct "group:"
// line. Grouping several items must not just repeat the first item's
// singular sentence.
type GroupSummaryFunc func(group QueueGroup) GroupSummaryResult

var groupSummaryFuncs = map[string]GroupSummaryFunc{}

// RegisterGroupSummary wires an area's group-sentence template into the
// shared queue projection. Call it from a domain package's init().
func RegisterGroupSummary(area string, fn GroupSummaryFunc) {
	if strings.TrimSpace(area) == "" || fn == nil {
		return
	}
	groupSummaryFuncs[area] = fn
}

func applyGroupSummary(group *QueueGroup) {
	fn, ok := groupSummaryFuncs[group.Area]
	if !ok {
		return
	}
	res := fn(*group)
	if strings.TrimSpace(res.Summary) != "" {
		group.OperatorSummary = res.Summary
		group.OperatorUnlock = res.Unlock
		group.SummarySource = SummarySourceAnnotator
	}
	if res.BatchAction != nil {
		group.SafeBatchAction = res.BatchAction
		group.TerminalReason = ""
	} else if strings.TrimSpace(res.BatchUnavailableReason) != "" {
		group.SafeBatchAction = nil
		group.TerminalReason = res.BatchUnavailableReason
	}
}

func bandPriority(band string) int {
	switch band {
	case BandNeedsYou:
		return 0
	case BandWatching:
		return 1
	case BandFiled:
		return 3
	default:
		return 2
	}
}

func itemNeedsReviewCard(item QueueItem) bool {
	return item.Status == "review_required" || item.Readiness == "needs_review"
}

func queueItem(root, path string) (QueueItem, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return QueueItem{}, err
	}
	item, err := queueItemForRel(path, filepath.ToSlash(rel))
	if err != nil {
		return QueueItem{}, err
	}
	if strings.TrimSpace(item.Area) == "" {
		return item, nil
	}
	homePath := filepath.Dir(root)
	binding, bindingErr := LoadCurrentWorkBinding(homePath, WorkBindingTarget{
		Area: item.Area, ReviewID: item.ID, CoreReviewPath: path,
	})
	if bindingErr != nil {
		item.WorkBindingError = bindingErr.Error()
		item.Warnings = append(item.Warnings, "owner-work binding invalid: "+bindingErr.Error())
	} else {
		item.WorkBinding = binding
	}
	return item, nil
}

func queueItemForRel(path, rel string) (QueueItem, error) {
	area := filepath.ToSlash(filepath.Dir(rel))
	if area == "." {
		area = ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return QueueItem{}, err
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return QueueItem{}, fmt.Errorf("parse review artifact %s: %w", path, err)
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".json")
	item := QueueItem{
		Area:                    area,
		ID:                      firstString(payload, "id", "plan_id", "review_id", "repair_plan_id"),
		Status:                  firstString(payload, "status", "verdict", "result"),
		OperatorSummary:         firstString(payload, "operator_summary"),
		ContextLabel:            firstString(payload, "context_label"),
		GroupKey:                firstString(payload, "group_key"),
		OperatorUnlock:          firstString(payload, "operator_unlock"),
		Band:                    firstString(payload, "band"),
		SummarySource:           firstString(payload, "summary_source"),
		CreatedAt:               firstString(payload, "created_at", "generated_at"),
		JSONPath:                path,
		SourceKind:              firstString(payload, "source_kind"),
		SourceRoot:              firstString(payload, "source_root"),
		SourcePath:              firstString(payload, "source_path"),
		TargetPath:              firstString(payload, "target_path"),
		TargetHash:              firstString(payload, "target_hash"),
		CandidateID:             firstString(payload, "candidate_id", "candidate_ref"),
		Owner:                   firstString(payload, "owner"),
		NotToDo:                 firstString(payload, "not_to_do"),
		Boundaries:              stringSlice(payload["boundaries"]),
		RepairCount:             intField(payload, "repair_count"),
		EvidenceRefs:            stringSlice(payload["evidence_refs"]),
		GraphDecisionHint:       firstString(payload, "graph_decision_hint"),
		GraphDecisionConfidence: floatField(payload, "graph_decision_confidence"),
		GraphDuplicateRisk:      floatField(payload, "graph_duplicate_risk"),
		GraphEvidenceStrength:   floatField(payload, "graph_evidence_strength"),
		GraphAuditRisk:          firstString(payload, "graph_audit_risk"),
		GraphTopOverlapScore:    intField(payload, "graph_top_overlap_score"),
		Readiness:               firstString(payload, "readiness"),
		BlockedReason:           firstString(payload, "blocked_reason"),
		NextAction:              firstString(payload, "next_action"),
	}
	if item.ID == "" {
		item.ID = stem
	}
	if item.Status == "" {
		item.Status = "unknown"
	}
	if item.Band == "" {
		item.Band = BandForStatus(item.Status, item.Readiness)
	}
	if card, ok := OperatorCardFromMap(payload); ok && ValidateOperatorCard(card) == nil && !LegacyGenericCardSummary(card.Summary) {
		item.OperatorSummary = card.Summary
		item.ContextLabel = card.ContextLabel
		item.GroupKey = card.GroupKey
		item.OperatorUnlock = card.Unlock
		if item.SummarySource == "" {
			item.SummarySource = SummarySourceWriter
		}
	} else {
		// Missing, invalid, or retired-generic card: derive the plain-language
		// card (what/why/one action) from the payload so every queued item —
		// including artifacts written before derivation existed — reads
		// specifically. The card the operator sees is projection-derived, so
		// summary_source says so; the debt counters keep tracking cards the
		// writer did not author.
		card := DerivedOperatorCard(payload, area)
		item.OperatorSummary = card.Summary
		item.ContextLabel = card.ContextLabel
		item.GroupKey = card.GroupKey
		item.OperatorUnlock = card.Unlock
		item.SummarySource = SummarySourceFallback
	}
	if item.RepairCount == 0 {
		if repairs, ok := payload["repairs"].([]any); ok {
			item.RepairCount = len(repairs)
		}
	}
	mdPath := filepath.Join(filepath.Dir(path), stem+".md")
	if _, err := os.Stat(mdPath); err == nil {
		item.MarkdownPath = mdPath
	} else if err != nil && !os.IsNotExist(err) {
		return QueueItem{}, err
	}
	extras, err := queueExtraPaths(filepath.Dir(path), stem, path, item.MarkdownPath)
	if err != nil {
		return QueueItem{}, err
	}
	item.ExtraPaths = extras
	if result, err := ownerTerminalResult(filepath.Dir(path), stem); err != nil {
		return QueueItem{}, err
	} else if result != nil && !TerminalStatus(item.Status) {
		// The owner already ran this plan to completion; the plan's frozen birth
		// status is not a decision the operator still owes.
		item.Status = result.Status
		item.Readiness = "no_action"
		item.Band = BandFiled
		item.Actions = nil
		item.BlockedReason = ""
		item.NextAction = "owner result recorded: " + FirstNonEmpty(result.ID, filepath.Base(result.Path)) + " (" + result.Status + ")"
	}
	if resolution, warning := reviewResolutionForCore(path); resolution != nil {
		item.Resolution = resolution
	} else if warning != "" {
		item.Warnings = append(item.Warnings, warning)
	}
	brief, warnings, err := loadDecisionBriefSidecar(filepath.Dir(path), stem)
	if err != nil {
		return QueueItem{}, err
	}
	item.Brief = brief
	item.Warnings = append(item.Warnings, warnings...)
	return item, nil
}

func archivedQueueItems(homePath, area string, entries []home.OperationalArchiveEntry) ([]QueueItem, error) {
	byOriginal := map[string]home.OperationalArchiveEntry{}
	for _, entry := range entries {
		byOriginal[entry.OriginalPath] = entry
	}
	var out []QueueItem
	for _, entry := range entries {
		if entry.Kind != "review" || filepath.Ext(entry.OriginalPath) != ".json" || !coreReviewJSON(entry.OriginalPath) {
			continue
		}
		rel := strings.TrimPrefix(filepath.ToSlash(entry.OriginalPath), "reviews/")
		if area != "" && !strings.HasPrefix(rel, filepath.ToSlash(area)+"/") && rel != filepath.ToSlash(area) {
			continue
		}
		item, err := queueItemForRel(filepath.Join(homePath, filepath.FromSlash(entry.ArchivePath)), rel)
		if err != nil {
			return nil, err
		}
		item.Status = FirstNonEmpty(item.Status, entry.Status, "archived")
		item.ExtraPaths = archivedReviewExtraPaths(entries, byOriginal, homePath, entry.OriginalPath)
		out = append(out, item)
	}
	return out, nil
}

func attachArchivedReviewExtraPaths(homePath string, items []QueueItem, entries []home.OperationalArchiveEntry) error {
	if len(entries) == 0 {
		return nil
	}
	byOriginal := map[string]home.OperationalArchiveEntry{}
	for _, entry := range entries {
		byOriginal[entry.OriginalPath] = entry
	}
	for i := range items {
		rel, err := filepath.Rel(homePath, items[i].JSONPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, "reviews/") {
			continue
		}
		items[i].ExtraPaths = uniqueSortedPaths(append(items[i].ExtraPaths, archivedReviewExtraPaths(entries, byOriginal, homePath, rel)...))
	}
	return nil
}

func archivedReviewExtraPaths(entries []home.OperationalArchiveEntry, byOriginal map[string]home.OperationalArchiveEntry, homePath, originalPath string) []string {
	stem := strings.TrimSuffix(filepath.Base(originalPath), ".json")
	dir := filepath.ToSlash(filepath.Dir(originalPath))
	var out []string
	for _, entry := range entries {
		if entry.Kind != "review" || entry.OriginalPath == originalPath {
			continue
		}
		if filepath.ToSlash(filepath.Dir(entry.OriginalPath)) != dir {
			continue
		}
		if !strings.HasPrefix(filepath.Base(entry.OriginalPath), stem+".") {
			continue
		}
		if _, ok := byOriginal[entry.OriginalPath]; !ok {
			continue
		}
		out = append(out, filepath.Join(homePath, filepath.FromSlash(entry.ArchivePath)))
	}
	sort.Strings(out)
	return out
}

func coreReviewJSON(path string) bool {
	name := filepath.Base(path)
	for _, suffix := range []string{".apply.json", ".decision.json", ".reviewed.json", ".brief.json"} {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}
	if strings.Contains(name, ".work-binding.") {
		return false
	}
	return true
}

func queueExtraPaths(dir, stem, jsonPath, mdPath string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, stem+".") {
			continue
		}
		path := filepath.Join(dir, name)
		if path == jsonPath || path == mdPath {
			continue
		}
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

func uniqueSortedPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func queueItemLess(a, b QueueItem) bool {
	ap, bp := statusPriority(a.Status), statusPriority(b.Status)
	if ap != bp {
		return ap < bp
	}
	if a.Area != b.Area {
		return a.Area < b.Area
	}
	return a.ID < b.ID
}

func statusPriority(status string) int {
	switch status {
	case "review_required", "unknown":
		return 0
	case "held", "blocked":
		return 1
	}
	if TerminalStatus(status) {
		return 3
	}
	return 2
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func intField(payload map[string]any, key string) int {
	switch value := payload[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

func floatField(payload map[string]any, key string) float64 {
	switch value := payload[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	default:
		return 0
	}
}

func stringSlice(value any) []string {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return UniqueSorted(out)
}
