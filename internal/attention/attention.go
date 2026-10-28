package attention

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

const (
	SchemaVersion = "compound.attention.v0.1"

	BandNeedsYou = "needs_you"
	BandWatching = "watching"
	BandFiled    = "filed"

	SourceReviews   = "reviews"
	SourceWorkbench = "workbench"
	SourceExecutor  = "executor"
	SourceActivity  = "activity"

	KindGateAdmit     = "gate_admit"
	KindVerdict       = "verdict"
	KindPromotion     = "promotion"
	KindContradiction = "contradiction"
	KindAdjudicate    = "adjudicate"
	KindIntake        = "intake"
	KindAdvance       = "advance"
	KindHygiene       = "hygiene"
	KindJobHealth     = "job_health"
	KindProductMove   = "product_move"
	KindActivity      = "activity"

	BlastRead               = "read"
	BlastLocalReversible    = "local_reversible"
	BlastExternalSideEffect = "external_side_effect"
	BlastPublic             = "public"
	BlastDestructive        = "destructive"
	BlastFinancial          = "financial"
	BlastSecurity           = "security"

	Reversible   = "reversible"
	Irreversible = "irreversible"

	// RiskGrade values mirror Class(g)'s three-tier admission grade — gate needs a human before
	// anything moves, judge needs a human's judgment on the outcome, tidy is receipt-only. Exported
	// so callers (the CLI's --receipt-only-grade flag) validate against the same vocabulary the
	// classifier emits instead of duplicating string literals.
	RiskGradeGate  = "gate"
	RiskGradeJudge = "judge"
	RiskGradeTidy  = "tidy"

	ActionOneClick = "one_click"
	ActionBatch    = "batch"
	ActionCopyOnly = "copy_only"
)

type Inputs struct {
	Reviews   *reviewartifact.QueueReport
	Workbench *work.WorkbenchReport
	Executors *work.ExecutorsProjection
	// Activity is the operator's Tier-0 ambient-activity day inference (unaccounted blocks). Nil for
	// every non-activity caller; Build is nil-safe on it. Defined HERE (not imported from
	// internal/activity) to keep the import direction clean — the CLI builds it from activity outputs.
	Activity *ActivityDayInput
}

// ActivityDayInput carries one day's pre-computed unaccounted activity blocks (from
// internal/activity.InferDay). At most one attention group is built per day: the largest block wins,
// the rest are counted in the summary.
type ActivityDayInput struct {
	Date                string
	GeneratedAt         string
	TotalUnaccountedMin int
	UnaccountedBlocks   []ActivityBlock
}

// ActivityBlock is one contiguous unaccounted block.
type ActivityBlock struct {
	Start       string
	End         string
	ActiveMin   int
	DurationMin int
	CategoryMix string
}

type Report struct {
	SchemaVersion string  `json:"schema_version"`
	GeneratedAt   string  `json:"generated_at"`
	Counts        Counts  `json:"counts"`
	Top           *Group  `json:"top"`
	Groups        []Group `json:"groups"`
}

type Counts struct {
	NeedsYou       int `json:"needs_you"`
	NeedsYouGroups int `json:"needs_you_groups"`
	ActionableNow  int `json:"actionable_now"`
	Watching       int `json:"watching"`
	BlockedOnYou   int `json:"blocked_on_you"`
	FallbackGroups int `json:"fallback_groups"`
}

type Group struct {
	Key             string    `json:"key"`
	Area            string    `json:"area"`
	Band            string    `json:"band"`
	Source          string    `json:"source"`
	Kind            string    `json:"kind"`
	Count           int       `json:"count"`
	OperatorSummary string    `json:"operator_summary"`
	OperatorUnlock  string    `json:"operator_unlock"`
	BlastRadius     string    `json:"blast_radius"`
	Reversibility   string    `json:"reversibility"`
	RiskGrade       string    `json:"risk_grade,omitempty"`
	Leverage        float64   `json:"leverage"`
	OldestCreatedAt string    `json:"oldest_created_at"`
	Action          *Action   `json:"action"`
	TerminalReason  string    `json:"terminal_reason"`
	ItemIDs         []string  `json:"item_ids"`
	Freshness       Freshness `json:"freshness"`
}

type Action struct {
	ID                   string                   `json:"id"`
	Label                string                   `json:"label"`
	Argv                 []string                 `json:"argv"`
	Kind                 string                   `json:"kind"`
	RequiresConfirmation bool                     `json:"requires_confirmation"`
	ActionIntent         *envelope.ActionIntent   `json:"action_intent,omitempty"`
	PolicyDecision       *envelope.PolicyDecision `json:"policy_decision,omitempty"`
	PolicyAuthority      *policy.AuthorityFace    `json:"policy_authority,omitempty"`
}

type Freshness struct {
	GeneratedAt string `json:"generated_at"`
	Source      string `json:"source"`
}

type areaClassification struct {
	Kind          string
	BlastRadius   string
	Reversibility string
}

var knownAreaClassifications = map[string]areaClassification{
	"panel-verdicts":                 {Kind: KindVerdict},
	"route-promotions":               {Kind: KindPromotion},
	"synthesis-contradictions":       {Kind: KindContradiction},
	"external-repo-intake":           {Kind: KindIntake},
	"job-health":                     {Kind: KindJobHealth},
	"product-action-cards":           {Kind: KindProductMove},
	"bench":                          {Kind: KindHygiene},
	"synthesis-sweeps":               {Kind: KindHygiene},
	"capability-evaluation-intake":   {Kind: KindIntake},
	"source-control-cleanup":         {Kind: KindHygiene},
	"worktree-retirement":            {Kind: KindHygiene},
	"capability-lifecycle-proposals": {Kind: KindPromotion},
	"observation-candidates":         {Kind: KindAdjudicate},
	"source-observation-verdicts":    {Kind: KindVerdict},
	"distiller-improvements":         {Kind: KindPromotion},
	"distiller-requests":             {Kind: KindIntake},
	"parity":                         {Kind: KindHygiene},
	"route-remorse":                  {Kind: KindContradiction},
	"lease-hygiene":                  {Kind: KindHygiene, BlastRadius: BlastLocalReversible, Reversibility: Reversible},
	"activity-unaccounted":           {Kind: KindActivity, BlastRadius: BlastRead, Reversibility: Reversible},
}

func Build(in Inputs, now time.Time) Report {
	if now.IsZero() {
		now = time.Now()
	}
	generatedAt := now.UTC().Format(time.RFC3339)
	report := Report{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   generatedAt,
		Groups:        []Group{},
	}
	if in.Reviews != nil {
		report.Groups = append(report.Groups, groupsFromReviews(*in.Reviews, now)...)
		report.Counts.FallbackGroups = fallbackGroupCount(*in.Reviews)
	}
	if in.Workbench != nil {
		report.Groups = append(report.Groups, groupsFromWorkbench(*in.Workbench, now)...)
	}
	if in.Executors != nil {
		report.Groups = append(report.Groups, groupsFromExecutors(*in.Executors, now)...)
	}
	if in.Activity != nil {
		report.Groups = append(report.Groups, groupsFromActivity(*in.Activity, now)...)
	}
	for i := range report.Groups {
		normalizeGroup(&report.Groups[i], generatedAt, now)
	}
	sortGroups(report.Groups)
	report.Counts = countGroups(report.Groups, report.Counts.FallbackGroups)
	if len(report.Groups) > 0 {
		top := report.Groups[0]
		report.Top = &top
	}
	return report
}

func Class(g Group) string {
	switch classRank(g) {
	case 0:
		return RiskGradeGate
	case 1:
		return RiskGradeJudge
	default:
		return RiskGradeTidy
	}
}

func ClassifyArea(area string) (kind, blastRadius, reversibility string) {
	c := classifyArea(area)
	return c.Kind, c.BlastRadius, c.Reversibility
}

func groupsFromReviews(report reviewartifact.QueueReport, now time.Time) []Group {
	out := make([]Group, 0, len(report.Groups))
	freshness := Freshness{GeneratedAt: firstNonEmpty(report.GeneratedAt, now.UTC().Format(time.RFC3339)), Source: SourceReviews}
	for _, group := range report.Groups {
		action := actionFromReviewGroup(group)
		g := Group{
			Key:             firstNonEmpty(group.Key, group.Area),
			Area:            group.Area,
			Band:            firstNonEmpty(group.Band, reviewartifact.BandForStatus(group.Status, group.Readiness)),
			Source:          SourceReviews,
			Count:           maxInt(group.Count, len(group.ItemIDs)),
			OperatorSummary: firstNonEmpty(group.OperatorSummary, group.ContextLabel, group.Area),
			OperatorUnlock:  group.OperatorUnlock,
			OldestCreatedAt: oldestReviewGroupCreatedAt(group, report.Items),
			Action:          action,
			TerminalReason:  group.TerminalReason,
			ItemIDs:         append([]string{}, group.ItemIDs...),
			Freshness:       freshness,
		}
		if validReviewAuthority(action) {
			g.BlastRadius = action.ActionIntent.BlastRadius
			g.Reversibility = action.ActionIntent.Reversibility
		}
		out = append(out, g)
	}
	return out
}

func groupsFromWorkbench(report work.WorkbenchReport, now time.Time) []Group {
	freshness := Freshness{GeneratedAt: firstNonEmpty(report.GeneratedAt, now.UTC().Format(time.RFC3339)), Source: SourceWorkbench}
	var out []Group
	canMove := filterWorkbenchItems(report.Open, func(item work.WorkbenchItem) bool {
		return workbenchItemCanMove(item) && !workbenchItemNeedsOperator(item)
	})
	if len(canMove) > 0 {
		var action *Action
		if workbenchItemsContain(canMove, report.DispatchNext.WorkID) {
			action = actionFromWorkbenchDispatch(report.DispatchNext)
		}
		out = append(out, Group{
			Key:             "workbench:can-move",
			Area:            "workbench-needs-claim",
			Band:            BandWatching,
			Source:          SourceWorkbench,
			Kind:            KindAdvance,
			Count:           len(canMove),
			OperatorSummary: plural(len(canMove), "safe open work item can move.", "safe open work items can move."),
			OperatorUnlock:  "Executors can claim or advance this work inside the admitted envelope.",
			OldestCreatedAt: oldestWorkbenchItemTime(canMove, freshness.GeneratedAt),
			Action:          action,
			ItemIDs:         workbenchItemIDs(canMove),
			Freshness:       freshness,
		})
	}
	blocked := filterWorkbenchItems(report.Open, func(item work.WorkbenchItem) bool {
		return item.State == "held" || item.State == "blocked" || (workbenchItemCanMove(item) && workbenchItemNeedsOperator(item))
	})
	if len(blocked) > 0 {
		blastRadius, reversibility := workbenchGateRisk(blocked)
		out = append(out, Group{
			Key:             "workbench:blocked-on-you",
			Area:            "workbench-blocked-on-you",
			Band:            BandNeedsYou,
			Source:          SourceWorkbench,
			Kind:            KindGateAdmit,
			Count:           len(blocked),
			OperatorSummary: plural(len(blocked), "open work item is blocked on operator review.", "open work items are blocked on operator review."),
			OperatorUnlock:  "Resolve the hold before executors can continue.",
			BlastRadius:     blastRadius,
			Reversibility:   reversibility,
			OldestCreatedAt: oldestWorkbenchItemTime(blocked, freshness.GeneratedAt),
			TerminalReason:  firstWorkbenchReason(blocked),
			ItemIDs:         workbenchItemIDs(blocked),
			Freshness:       freshness,
		})
	}
	if !report.DispatchNext.CanRun && (strings.TrimSpace(report.DispatchNext.WaitReason) != "" || strings.TrimSpace(report.DispatchNext.Reason) != "") {
		out = append(out, Group{
			Key:             "workbench:dispatch-next",
			Area:            "workbench-dispatch-next",
			Band:            BandWatching,
			Source:          SourceWorkbench,
			Kind:            KindAdvance,
			Count:           1,
			OperatorSummary: "Dispatch-next is waiting.",
			OperatorUnlock:  strings.TrimSpace(report.DispatchNext.Action),
			OldestCreatedAt: freshness.GeneratedAt,
			Action:          actionFromWorkbenchDispatch(report.DispatchNext),
			TerminalReason:  firstNonEmpty(report.DispatchNext.WaitReason, report.DispatchNext.Reason),
			ItemIDs:         compactStrings([]string{report.DispatchNext.WorkID}),
			Freshness:       freshness,
		})
	}
	return out
}

func groupsFromExecutors(report work.ExecutorsProjection, now time.Time) []Group {
	freshness := Freshness{GeneratedAt: firstNonEmpty(report.GeneratedAt, now.UTC().Format(time.RFC3339)), Source: SourceExecutor}
	var out []Group
	if group := leaseHygieneGroup(report, freshness); group != nil {
		out = append(out, *group)
	}
	for _, row := range report.Executors {
		if group := executorContextUnavailableGroup(row, freshness); group != nil {
			out = append(out, *group)
		}
		count, itemIDs := executorOwnedBurden(row)
		if count <= 0 {
			continue
		}
		summary := row.Label
		if strings.TrimSpace(summary) == "" {
			summary = row.ID
		}
		summary = summary + " needs operator attention."
		// Prefer the plain-language reason on the burden item itself (e.g. "executor
		// died — re-dispatch or release") over row.WaitReason, which can carry a raw
		// internal dispatch token (e.g. "proof_ready_close") that must never render
		// on a needs_you card.
		unlock := firstNonEmpty(firstBurdenNeedsMeReason(row), row.WaitReason, row.ProjectionNote)
		out = append(out, Group{
			Key:             "executor:" + firstNonEmpty(row.ID, row.Label),
			Area:            "job-health",
			Band:            BandNeedsYou,
			Source:          SourceExecutor,
			Kind:            KindJobHealth,
			Count:           count,
			OperatorSummary: summary,
			OperatorUnlock:  unlock,
			OldestCreatedAt: oldestExecutorTime(row, freshness.GeneratedAt, now),
			TerminalReason:  unlock,
			ItemIDs:         itemIDs,
			Freshness:       freshness,
		})
	}
	return out
}

func executorContextUnavailableGroup(row work.ExecutorRow, freshness Freshness) *Group {
	event := row.LastHookEvent
	if event == nil || event.Status != "fail_open" || event.FailOpenReason != envelope.FailOpenContextUnavailable {
		return nil
	}
	id := firstNonEmpty(row.ID, row.Label, "unknown")
	const warning = "Context build failed. Harness may be running without Compound context."
	return &Group{
		Key:             "executor-context:" + id,
		Area:            "job-health",
		Band:            BandNeedsYou,
		Source:          SourceExecutor,
		Kind:            KindJobHealth,
		Count:           1,
		OperatorSummary: warning,
		OperatorUnlock:  "Inspect or rebuild Compound context before continuing this harness.",
		OldestCreatedAt: firstNonEmpty(event.CreatedAt, freshness.GeneratedAt),
		TerminalReason:  warning,
		ItemIDs: compactStrings([]string{
			"executor-context:" + id + ":" + event.Hook + ":" + event.CreatedAt,
		}),
		Freshness: freshness,
	}
}

// groupsFromActivity builds AT MOST ONE watching-band group per day from the operator's Tier-0
// activity day inference: the largest unaccounted block is the headline, the rest are counted in the
// summary. Zero unaccounted blocks -> zero groups (an accounted or incomplete-coverage day never
// surfaces — that exclusion happens upstream in internal/activity.InferDay). Evidence only: read
// blast, reversible, never a gate.
func groupsFromActivity(in ActivityDayInput, now time.Time) []Group {
	blocks := in.UnaccountedBlocks
	if len(blocks) == 0 {
		return nil
	}
	// Largest block wins; ties break by earlier start for determinism.
	sorted := make([]ActivityBlock, len(blocks))
	copy(sorted, blocks)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].DurationMin != sorted[j].DurationMin {
			return sorted[i].DurationMin > sorted[j].DurationMin
		}
		return sorted[i].Start < sorted[j].Start
	})
	top := sorted[0]
	freshness := Freshness{GeneratedAt: firstNonEmpty(in.GeneratedAt, now.UTC().Format(time.RFC3339)), Source: SourceActivity}
	summary := fmt.Sprintf("%d min unaccounted activity (%s) on %s — no compound work accounts for it", top.DurationMin, top.CategoryMix, in.Date)
	if extra := len(sorted) - 1; extra > 0 {
		summary += fmt.Sprintf("; +%d more unaccounted block(s), %d min total", extra, in.TotalUnaccountedMin)
	}
	return []Group{{
		Key:             "activity:unaccounted:" + in.Date,
		Area:            "activity-unaccounted",
		Band:            BandWatching,
		Source:          SourceActivity,
		Kind:            KindActivity,
		Count:           len(sorted),
		OperatorSummary: summary,
		OperatorUnlock:  "Resume, record, or dismiss the thread this time belonged to — the sensor reconciles activity against the work ledger, it never gates.",
		BlastRadius:     BlastRead,
		Reversibility:   Reversible,
		OldestCreatedAt: firstNonEmpty(top.Start, freshness.GeneratedAt),
		ItemIDs:         []string{"activity-block:" + top.Start},
		Freshness:       freshness,
	}}
}

// leaseHygieneGroup collapses stale-lease residue across every executor row
// into ONE cross-executor needs_you group carrying the governed sweep
// (`compound home archive-history --kind lease`), instead of four
// per-executor "needs operator attention" cards for work that is already
// terminal. Returns nil when nothing is stale.
func leaseHygieneGroup(report work.ExecutorsProjection, freshness Freshness) *Group {
	count := 0
	var itemIDs []string
	for _, row := range report.Executors {
		count += row.StaleLeaseCount
		itemIDs = append(itemIDs, row.StaleLeaseWorkIDs...)
	}
	if count <= 0 {
		return nil
	}
	summaryText := "finished job(s) still hold their claim tags."
	return &Group{
		Key:             "lease-hygiene",
		Area:            "lease-hygiene",
		Band:            BandNeedsYou,
		Source:          SourceExecutor,
		Kind:            KindHygiene,
		Count:           count,
		OperatorSummary: plural(count, summaryText, summaryText),
		OperatorUnlock:  "Sweep them into the archive — live claims are kept, and the sweep can be restored.",
		ItemIDs:         uniqueStrings(itemIDs),
		Freshness:       freshness,
		Action: &Action{
			ID:                   "lease-hygiene.sweep",
			Label:                "Sweep leftover claim tags",
			Argv:                 []string{"compound", "home", "archive-history", "--kind", "lease"},
			Kind:                 ActionBatch,
			RequiresConfirmation: true,
		},
	}
}

// firstBurdenNeedsMeReason returns the first plain-language NeedsMe reason
// carried by the row's own current-work burden items (dead-executor cards),
// so the group's operator-facing text never falls back to a raw internal
// dispatch token.
func firstBurdenNeedsMeReason(row work.ExecutorRow) string {
	for _, ref := range row.CurrentWork {
		if strings.TrimSpace(ref.NeedsMe) != "" {
			return ref.NeedsMe
		}
	}
	return ""
}

func normalizeGroup(g *Group, generatedAt string, now time.Time) {
	if strings.TrimSpace(g.Key) == "" {
		g.Key = firstNonEmpty(g.Area, g.Source, "attention")
	}
	if strings.TrimSpace(g.Area) == "" {
		g.Area = "unknown"
	}
	if strings.TrimSpace(g.Band) == "" {
		g.Band = BandNeedsYou
	}
	if strings.TrimSpace(g.Kind) == "" {
		g.Kind = classifyArea(g.Area).Kind
	}
	classification := classifyArea(g.Area)
	// A blast/reversibility EXPLICITLY authored in knownAreaClassifications (the
	// raw entry, not withDefaults' fill-ins) outranks the keyword net, which
	// exists to guess for areas that declare nothing — e.g. lease-hygiene's
	// restorable sweep says "archive" in its card text and would otherwise read
	// as destructive/irreversible.
	raw, known := knownAreaClassifications[strings.TrimSpace(g.Area)]
	if strings.TrimSpace(g.BlastRadius) == "" {
		if known && raw.BlastRadius != "" {
			g.BlastRadius = raw.BlastRadius
		} else {
			g.BlastRadius = deriveBlastRadius(*g, classification.BlastRadius)
		}
	}
	if strings.TrimSpace(g.Reversibility) == "" {
		if known && raw.Reversibility != "" {
			g.Reversibility = raw.Reversibility
		} else {
			g.Reversibility = deriveReversibility(*g, classification.Reversibility)
		}
	}
	// RiskGrade is DERIVED here, once BlastRadius/Reversibility/Kind have all settled for this
	// group — the same three-tier Class() every caller already computes, just frozen onto the wire
	// instead of thrown away outside the CLI text printer. No new classification logic.
	g.RiskGrade = Class(*g)
	if g.Count <= 0 {
		g.Count = maxInt(1, len(g.ItemIDs))
	}
	if strings.TrimSpace(g.OperatorSummary) == "" {
		// Last-resort prose: every real producer authors a summary, so this
		// only fires on malformed inputs — but it still must state why the
		// group is in front of the operator and, for needs-you, one concrete
		// action (never a generic "needs attention" line). Renderers print
		// Area/Key beside the summary, so identity lives there, not in prose.
		if g.Band == BandNeedsYou {
			g.OperatorSummary = plural(g.Count, "item needs your decision.", "items need your decision.")
			if strings.TrimSpace(g.OperatorUnlock) == "" {
				g.OperatorUnlock = "Open the review queue to read the details."
			}
		} else {
			g.OperatorSummary = "Background work moved; nothing is needed from you."
		}
	}
	if strings.TrimSpace(g.OldestCreatedAt) == "" {
		g.OldestCreatedAt = generatedAt
	}
	if strings.TrimSpace(g.Freshness.GeneratedAt) == "" {
		g.Freshness.GeneratedAt = generatedAt
	}
	if strings.TrimSpace(g.Freshness.Source) == "" {
		g.Freshness.Source = g.Source
	}
	if g.Action != nil {
		normalizeAction(g.Action, *g)
	}
	g.Leverage = leverage(g.Count, g.OldestCreatedAt, g.OperatorUnlock, now)
	g.ItemIDs = uniqueStrings(g.ItemIDs)
}

func classifyArea(area string) areaClassification {
	if c, ok := knownAreaClassifications[strings.TrimSpace(area)]; ok {
		return withDefaults(c)
	}
	return areaClassification{Kind: KindAdjudicate, BlastRadius: BlastLocalReversible, Reversibility: Reversible}
}

func withDefaults(c areaClassification) areaClassification {
	if c.Kind == "" {
		c.Kind = KindAdjudicate
	}
	if c.BlastRadius == "" {
		c.BlastRadius = BlastLocalReversible
	}
	if c.Reversibility == "" {
		c.Reversibility = Reversible
	}
	return c
}

func deriveBlastRadius(g Group, fallback string) string {
	text := strings.ToLower(strings.Join([]string{
		g.Area,
		g.Kind,
		g.OperatorSummary,
		g.OperatorUnlock,
		g.TerminalReason,
		actionText(g.Action),
	}, " "))
	switch {
	case strings.Contains(text, "security") || strings.Contains(text, "secret") || strings.Contains(text, "credential"):
		return BlastSecurity
	case strings.Contains(text, "financial") || strings.Contains(text, "spend") || strings.Contains(text, "billing") || strings.Contains(text, "cost"):
		return BlastFinancial
	case strings.Contains(text, "destructive") || strings.Contains(text, "delete") || strings.Contains(text, "reset") || strings.Contains(text, "archive"):
		return BlastDestructive
	case strings.Contains(text, "public") || strings.Contains(text, "publish") || strings.Contains(text, "release") || strings.Contains(text, "push"):
		return BlastPublic
	case strings.Contains(text, "external") || strings.Contains(text, "send") || strings.Contains(text, "submit"):
		return BlastExternalSideEffect
	case strings.Contains(text, "read-only") || strings.Contains(text, "read only"):
		return BlastRead
	default:
		return firstNonEmpty(fallback, BlastLocalReversible)
	}
}

func deriveReversibility(g Group, fallback string) string {
	text := strings.ToLower(strings.Join([]string{
		g.Area,
		g.Kind,
		g.OperatorSummary,
		g.OperatorUnlock,
		g.TerminalReason,
		actionText(g.Action),
	}, " "))
	if strings.Contains(text, "irreversible") || g.BlastRadius == BlastDestructive || g.BlastRadius == BlastFinancial || g.BlastRadius == BlastSecurity || g.BlastRadius == BlastPublic {
		return Irreversible
	}
	return firstNonEmpty(fallback, Reversible)
}

func actionFromReviewGroup(group reviewartifact.QueueGroup) *Action {
	if group.SafeBatchAction != nil {
		return actionFromReviewAction(*group.SafeBatchAction, group.Count)
	}
	if group.Count == 1 && group.Sample != nil && len(group.Sample.Actions) == 1 {
		return actionFromReviewAction(group.Sample.Actions[0], 1)
	}
	return nil
}

func actionFromReviewAction(action reviewartifact.Action, count int) *Action {
	projected := &Action{
		ID:                   firstNonEmpty(action.ID, "review.action"),
		Label:                firstNonEmpty(action.Label, "Review"),
		Argv:                 append([]string{}, action.Argv...),
		Kind:                 ActionCopyOnly,
		RequiresConfirmation: true,
		ActionIntent:         cloneActionIntent(action.ActionIntent),
		PolicyDecision:       clonePolicyDecision(action.PolicyDecision),
		PolicyAuthority:      clonePolicyAuthority(action.PolicyAuthority),
	}
	if !validReviewAuthority(projected) || !projected.PolicyAuthority.Allowed || !cleanCompoundArgv(action.Argv) {
		return projected
	}
	kind := ActionOneClick
	if count > 1 {
		kind = ActionBatch
	}
	projected.Kind = kind
	projected.RequiresConfirmation = action.RequiresReview || kind == ActionBatch
	return projected
}

func validReviewAuthority(action *Action) bool {
	if action == nil || action.ActionIntent == nil || action.PolicyDecision == nil || action.PolicyAuthority == nil {
		return false
	}
	// Attention is a projection client: it verifies the producer-owned bundle but
	// never evaluates policy or reconstructs authority from card text.
	return policy.ValidateAuthorityBundle(*action.ActionIntent, *action.PolicyDecision, *action.PolicyAuthority) == nil
}

func cloneActionIntent(source *envelope.ActionIntent) *envelope.ActionIntent {
	if source == nil {
		return nil
	}
	clone := *source
	clone.EvidenceRefs = cloneStrings(source.EvidenceRefs)
	clone.EstimatedCost.USD = cloneFloat64(source.EstimatedCost.USD)
	clone.EstimatedCost.Tokens = cloneInt(source.EstimatedCost.Tokens)
	clone.EstimatedCost.HumanReviewMin = cloneInt(source.EstimatedCost.HumanReviewMin)
	return &clone
}

func clonePolicyDecision(source *envelope.PolicyDecision) *envelope.PolicyDecision {
	if source == nil {
		return nil
	}
	clone := *source
	clone.EvidenceRefs = cloneStrings(source.EvidenceRefs)
	clone.Reasons = cloneStrings(source.Reasons)
	clone.Obligations = cloneStrings(source.Obligations)
	return &clone
}

func clonePolicyAuthority(source *policy.AuthorityFace) *policy.AuthorityFace {
	if source == nil {
		return nil
	}
	clone := *source
	clone.ExpectedProof = cloneStrings(source.ExpectedProof)
	return &clone
}

func cloneStrings(source []string) []string {
	if source == nil {
		return nil
	}
	return append([]string{}, source...)
}

func cloneFloat64(source *float64) *float64 {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func cloneInt(source *int) *int {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func actionFromWorkbenchDispatch(dispatch work.WorkbenchDispatchNext) *Action {
	if !dispatch.CanRun || !cleanCompoundArgv(dispatch.Command) {
		return nil
	}
	return &Action{
		ID:                   "workbench.dispatch_next",
		Label:                firstNonEmpty(dispatch.Action, "Dispatch next work"),
		Argv:                 append([]string{}, dispatch.Command...),
		Kind:                 ActionOneClick,
		RequiresConfirmation: false,
	}
}

func normalizeAction(action *Action, g Group) {
	if action == nil {
		return
	}
	if len(action.Argv) == 0 || !cleanCompoundArgv(action.Argv) {
		action.Kind = ActionCopyOnly
		action.RequiresConfirmation = true
	} else if action.Kind == "" {
		action.Kind = ActionOneClick
	}
	if classRank(g) == 0 {
		action.RequiresConfirmation = true
	}
	if action.Argv == nil {
		action.Argv = []string{}
	}
}

func cleanCompoundArgv(argv []string) bool {
	return len(argv) > 0 && argv[0] == "compound"
}

func sortGroups(groups []Group) {
	sort.SliceStable(groups, func(i, j int) bool {
		ci, cj := classRank(groups[i]), classRank(groups[j])
		if ci != cj {
			return ci < cj
		}
		if groups[i].Leverage != groups[j].Leverage {
			return groups[i].Leverage > groups[j].Leverage
		}
		ti, iOK := parseTime(groups[i].OldestCreatedAt)
		tj, jOK := parseTime(groups[j].OldestCreatedAt)
		if iOK && jOK && !ti.Equal(tj) {
			return ti.Before(tj)
		}
		if iOK != jOK {
			return iOK
		}
		return groups[i].Key < groups[j].Key
	})
}

func classRank(g Group) int {
	if validReviewAuthority(g.Action) {
		if !g.Action.PolicyAuthority.Allowed {
			return 0
		}
	} else if g.Reversibility == Irreversible || highBlast(g.BlastRadius) {
		return 0
	}
	switch g.Kind {
	case KindVerdict, KindPromotion, KindContradiction, KindAdjudicate, KindIntake, KindAdvance, KindProductMove:
		return 1
	default:
		return 2
	}
}

func highBlast(blast string) bool {
	switch blast {
	case BlastPublic, BlastDestructive, BlastFinancial, BlastSecurity:
		return true
	default:
		return false
	}
}

func leverage(count int, oldestCreatedAt, operatorUnlock string, now time.Time) float64 {
	if count < 1 {
		count = 1
	}
	if now.IsZero() {
		now = time.Now()
	}
	ageDays := 1.0
	stale := false
	if oldest, ok := parseTime(oldestCreatedAt); ok {
		age := now.UTC().Sub(oldest.UTC())
		if age < 0 {
			age = 0
		}
		ageDays = clamp(age.Hours()/24, 1, 14)
		stale = age > 48*time.Hour
	}
	score := (1 + math.Log2(float64(count))) * ageDays
	if strings.TrimSpace(operatorUnlock) != "" {
		score *= 1.25
	}
	if stale {
		score *= 1.25
	}
	return score
}

func countGroups(groups []Group, fallbackGroups int) Counts {
	counts := Counts{FallbackGroups: fallbackGroups}
	for _, group := range groups {
		switch group.Band {
		case BandNeedsYou:
			counts.NeedsYou += group.Count
			counts.NeedsYouGroups++
			if group.Action != nil && group.Action.Kind != ActionCopyOnly {
				counts.ActionableNow++
			}
			if group.Source == SourceWorkbench || group.Source == SourceExecutor {
				counts.BlockedOnYou += group.Count
			}
		case BandWatching:
			counts.Watching += group.Count
		}
	}
	return counts
}

func fallbackGroupCount(report reviewartifact.QueueReport) int {
	if report.Counts.NeedsYouFallbackGroupCount > 0 {
		return report.Counts.NeedsYouFallbackGroupCount
	}
	count := 0
	for _, group := range report.Groups {
		if group.Band == reviewartifact.BandNeedsYou && group.SummarySource == reviewartifact.SummarySourceFallback {
			count++
		}
	}
	return count
}

func oldestReviewGroupCreatedAt(group reviewartifact.QueueGroup, items []reviewartifact.QueueItem) string {
	ids := map[string]bool{}
	for _, id := range group.ItemIDs {
		ids[id] = true
	}
	oldest := ""
	for _, item := range items {
		if item.Area != group.Area {
			continue
		}
		if len(ids) > 0 && !ids[item.ID] {
			continue
		}
		oldest = olderTimestamp(oldest, item.CreatedAt)
	}
	if oldest == "" && group.Sample != nil {
		oldest = group.Sample.CreatedAt
	}
	return oldest
}

func oldestWorkbenchItemTime(items []work.WorkbenchItem, fallback string) string {
	oldest := ""
	for _, item := range items {
		oldest = olderTimestamp(oldest, item.ClaimedAt)
		for _, obs := range item.RunObservations {
			oldest = olderTimestamp(oldest, obs.GeneratedAt)
		}
		for _, outcome := range item.RouteOutcomes {
			oldest = olderTimestamp(oldest, outcome.CreatedAt)
		}
	}
	return firstNonEmpty(oldest, fallback)
}

func oldestExecutorTime(row work.ExecutorRow, fallback string, now time.Time) string {
	oldest := ""
	for _, ref := range row.CurrentWork {
		oldest = olderTimestamp(oldest, ref.ClaimedAt)
	}
	for _, lease := range row.Leases {
		oldest = olderTimestamp(oldest, lease.ClaimedAt)
		if lease.AgeSeconds > 0 {
			oldest = olderTimestamp(oldest, now.UTC().Add(-time.Duration(lease.AgeSeconds)*time.Second).Format(time.RFC3339))
		}
	}
	for _, job := range row.StalledJobs {
		if job.AgeSeconds > 0 {
			oldest = olderTimestamp(oldest, now.UTC().Add(-time.Duration(job.AgeSeconds)*time.Second).Format(time.RFC3339))
		}
	}
	if row.OldestLeaseAgeSeconds != nil && *row.OldestLeaseAgeSeconds > 0 {
		oldest = olderTimestamp(oldest, now.UTC().Add(-time.Duration(*row.OldestLeaseAgeSeconds)*time.Second).Format(time.RFC3339))
	}
	return firstNonEmpty(oldest, fallback)
}

func filterWorkbenchItems(items []work.WorkbenchItem, keep func(work.WorkbenchItem) bool) []work.WorkbenchItem {
	out := make([]work.WorkbenchItem, 0, len(items))
	for _, item := range items {
		if keep(item) {
			out = append(out, item)
		}
	}
	return out
}

func workbenchItemCanMove(item work.WorkbenchItem) bool {
	switch strings.TrimSpace(item.State) {
	case "ready", "admitted", "proposed":
		return true
	default:
		return false
	}
}

func workbenchItemNeedsOperator(item work.WorkbenchItem) bool {
	if item.State == "held" || item.State == "blocked" {
		return true
	}
	if item.ActionContext != nil && item.ActionContext.RequiresOperator {
		return true
	}
	for _, card := range item.ReviewCards {
		if card.RequiresOperator {
			return true
		}
	}
	text := workbenchItemRiskText(item)
	if strings.Contains(text, "conflict") || strings.Contains(text, "operator required") || strings.Contains(text, "requires operator") {
		return true
	}
	blast := deriveBlastRadius(Group{OperatorSummary: text}, BlastLocalReversible)
	return blast == BlastExternalSideEffect || highBlast(blast)
}

func workbenchGateRisk(items []work.WorkbenchItem) (string, string) {
	blast := BlastLocalReversible
	reversibility := Reversible
	for _, item := range items {
		candidate := deriveBlastRadius(Group{OperatorSummary: workbenchItemRiskText(item)}, BlastLocalReversible)
		if blastSeverity(candidate) > blastSeverity(blast) {
			blast = candidate
		}
		if deriveReversibility(Group{BlastRadius: candidate, OperatorSummary: workbenchItemRiskText(item)}, Reversible) == Irreversible {
			reversibility = Irreversible
		}
	}
	return blast, reversibility
}

func workbenchItemRiskText(item work.WorkbenchItem) string {
	// Read only the action-bearing fields. Boundaries, stop conditions, and
	// success predicates commonly name forbidden public/external actions (for
	// example "do not publish") and must not turn safe work into a false gate.
	parts := []string{item.CanonicalSurface, item.ReviewTrigger, item.NextAction, item.OpenReason}
	if item.ActionContext != nil {
		ctx := item.ActionContext
		parts = append(parts, ctx.Kind, ctx.SourceRef, ctx.SourceID, ctx.Title, ctx.ActionSummary)
		parts = append(parts, ctx.Command...)
	}
	for _, card := range item.ReviewCards {
		parts = append(parts, card.Kind, card.Title, card.NextAction, card.GateID, card.SourceKind)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func blastSeverity(blast string) int {
	switch blast {
	case BlastSecurity:
		return 6
	case BlastFinancial:
		return 5
	case BlastDestructive:
		return 4
	case BlastPublic:
		return 3
	case BlastExternalSideEffect:
		return 2
	case BlastLocalReversible:
		return 1
	default:
		return 0
	}
}

func workbenchItemsContain(items []work.WorkbenchItem, workID string) bool {
	for _, item := range items {
		if item.WorkID == workID {
			return true
		}
	}
	return false
}

func workbenchItemIDs(items []work.WorkbenchItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.WorkID)
	}
	return out
}

// executorOwnedBurden is the per-executor "needs operator attention" burden:
// dead-executor needs_me items and stalled jobs ONLY. Stale leases are
// terminal-work residue, not something the executor is doing wrong — they
// contribute nothing here; the cross-executor lease-hygiene group (see
// leaseHygieneGroup) owns them instead.
func executorOwnedBurden(row work.ExecutorRow) (int, []string) {
	workbenchHeld := 0
	var out []string
	for _, ref := range row.CurrentWork {
		if ref.State == "held" || ref.State == "blocked" {
			workbenchHeld++
			continue
		}
		if strings.TrimSpace(ref.NeedsMe) != "" {
			out = append(out, ref.WorkID)
		}
	}
	executorNeeds := row.NeedsMeCount - workbenchHeld
	if executorNeeds < 0 {
		executorNeeds = 0
	}
	for _, job := range row.StalledJobs {
		out = append(out, firstNonEmpty(job.WorkID, job.JobID))
	}
	return executorNeeds + len(row.StalledJobs), uniqueStrings(out)
}

func firstWorkbenchReason(items []work.WorkbenchItem) string {
	for _, item := range items {
		if strings.TrimSpace(item.NextAction) != "" {
			return item.NextAction
		}
		if strings.TrimSpace(item.OpenReason) != "" {
			return item.OpenReason
		}
	}
	return ""
}

func actionText(action *Action) string {
	if action == nil {
		return ""
	}
	return strings.Join(append([]string{action.ID, action.Label, action.Kind}, action.Argv...), " ")
}

func olderTimestamp(a, b string) string {
	b = strings.TrimSpace(b)
	if b == "" {
		return a
	}
	if a == "" {
		return b
	}
	at, aOK := parseTime(a)
	bt, bOK := parseTime(b)
	switch {
	case aOK && bOK && bt.Before(at):
		return b
	case !aOK && bOK:
		return b
	default:
		return a
	}
}

func parseTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, true
	}
	// Workbench claim times retain the work lease writer's Unix nanoseconds.
	if nanos, err := strconv.ParseInt(value, 10, 64); err == nil && nanos > 0 {
		return time.Unix(0, nanos).UTC(), true
	}
	return time.Time{}, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func plural(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return itoa(count) + " " + many
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range compactStrings(values) {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func maxInt(values ...int) int {
	max := 0
	for _, value := range values {
		if value > max {
			max = value
		}
	}
	return max
}

func clamp(value, lo, hi float64) float64 {
	if value < lo {
		return lo
	}
	if value > hi {
		return hi
	}
	return value
}
