package director

// harness_edit.go — the 7th director candidate class: the bounded, propose-only
// self-harness loop (Weng map item 4, Self-Harness lite).
//
// It mines three already-existing, read-only failure signals — the capability
// usage projection (offers that never fire / misfire / are injected but ignored),
// the latest bench report(s) (graph-parity gaps and per-split surfacing weakness),
// and route-remorse reviews (routes that did not prove out) — and turns each into
// a harness_edit candidate that names ONE editable surface (EVOLVE-BLOCK). Every
// candidate rides the same rails as the other six classes: it becomes a proposed
// WorkEnvelope whose TargetSurface the admission seam gates against the
// operator-owned editable-surfaces whitelist, and it emits an operator review card
// under reviews/harness-edit-proposals/.
//
// Two hard guarantees:
//   - It NEVER constructs a candidate targeting a hard-deny-core surface (policy,
//     permissions, ledger, evaluator): newHarnessEditCandidate refuses at
//     construction, before the candidate exists, so a tempting signal (e.g. a
//     misfiring evaluator/judge capability) yields zero candidates, not a denied
//     one. The grader/policy/ledger spine must stay outside the loop that could
//     edit it.
//   - It is propose-only: nothing here admits, claims, advances, or applies. The
//     success predicate REQUIRES a before/after bench run and operator approval;
//     the candidate only proposes the edit.
//
// It reads the bench report SHAPE (internal/bench) but reimplements a tiny reader
// so it never imports cmd; it consumes internal/capgraph and internal/policy as a
// read-only API client.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/bench"
	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
)

const (
	harnessEditReviewArea         = "harness-edit-proposals"
	harnessEditProposalCardSchema = "compound.harness_edit_proposal_card.v0.1"

	// maxHarnessEditPerSignal bounds how many candidates one sub-signal may emit
	// per run, and maxHarnessEditCandidates bounds the whole class. The per-run
	// cap and class-fair round-robin bound the final proposal set on top of this;
	// these keep a noisy home from generating hundreds of raw candidates.
	maxHarnessEditPerSignal   = 3
	maxHarnessEditCandidates  = 8
	harnessEditWeakHit1Cutoff = 0.5
	maxBenchAbsentRefs        = 5
)

// harnessEditInput is the shaped, already-humanized signal newHarnessEditCandidate
// turns into a candidate. Keeping it a struct (not a positional signature) keeps
// each call site legible about which plain-language phrasing it supplies.
type harnessEditInput struct {
	Surface      string   // editable surface id (never hard-deny-core; refused if it is)
	SourceID     string   // stable per-signal id (drives the dedup trigger digest)
	Title        string   // work title
	Action       string   // lowercase verb phrase for the operator card ("trim the …")
	Signal       string   // plain "because" clause ("it was offered 12 times and never fired")
	Unlock       string   // plain future consequence for the card (no "?", <=90)
	EvidenceRefs []string // mandatory: what justifies the edit
}

// harnessEditCandidates is the 7th scanner. Every return is read-only.
func harnessEditCandidates(homePath string, now time.Time) ([]candidate, error) {
	var out []candidate

	usage, err := capgraph.BuildCapabilityUsage(homePath, capgraph.UsageBuildOptions{GeneratedAt: now})
	if err != nil {
		return nil, err
	}
	out = append(out, harnessEditCapabilityUsageCandidates(usage)...)

	reports, err := loadLatestBenchReports(homePath)
	if err != nil {
		return nil, err
	}
	out = append(out, harnessEditBenchCandidates(reports)...)

	reviews, err := loadRouteRemorseReviews(homePath)
	if err != nil {
		return nil, err
	}
	out = append(out, harnessEditRouteRemorseCandidates(reviews)...)

	// Deterministic order + explicit class bound. SourceIDs are stable and unique
	// per signal, so lexicographic order is reproducible across runs.
	sort.SliceStable(out, func(i, j int) bool { return out[i].SourceID < out[j].SourceID })
	if len(out) > maxHarnessEditCandidates {
		out = out[:maxHarnessEditCandidates]
	}
	return out, nil
}

func harnessEditCapabilityUsageCandidates(usage capgraph.CapabilityUsageProjection) []candidate {
	var out []candidate

	n := 0
	for _, entry := range usage.Summary.NeverFired {
		if n >= maxHarnessEditPerSignal {
			break
		}
		surface := harnessEditSurfaceForCapabilityRef(entry.Ref)
		label := plainCapabilityLabel(entry.Ref)
		if cand, ok := newHarnessEditCandidate(harnessEditInput{
			Surface:  surface,
			SourceID: "capability_usage:never_fired:" + entry.Ref,
			Title:    "Trim the never-fired " + label + " offer",
			Action:   "trim the never-fired " + label + " offer",
			Signal:   fmt.Sprintf("it was offered %d times and never fired", entry.Count),
			Unlock:   "the " + reviewartifact.PlainLabel(surface) + " surface drops an offer nothing uses",
			EvidenceRefs: []string{
				capgraph.CapabilityUsageProjectionRef,
				"capability_usage_never_fired:" + entry.Ref,
				fmt.Sprintf("capability_usage_offered_count:%d", entry.Count),
			},
		}); ok {
			out = append(out, cand)
			n++
		}
	}

	n = 0
	for _, c := range usage.Capabilities {
		if n >= maxHarnessEditPerSignal {
			break
		}
		if c.FalseTriggeredCount == 0 {
			continue
		}
		surface := harnessEditSurfaceForCapabilityRef(c.Ref)
		label := plainCapabilityLabel(c.Ref)
		if cand, ok := newHarnessEditCandidate(harnessEditInput{
			Surface:  surface,
			SourceID: "capability_usage:false_triggered:" + c.Ref,
			Title:    "Fix the false-triggering " + label + " offer",
			Action:   "fix the false-triggering " + label + " offer",
			Signal:   fmt.Sprintf("it misfired on %d receipts", c.FalseTriggeredCount),
			Unlock:   "the " + reviewartifact.PlainLabel(surface) + " surface stops misfiring on unrelated work",
			EvidenceRefs: []string{
				capgraph.CapabilityUsageProjectionRef,
				"capability_usage_false_triggered:" + c.Ref,
				fmt.Sprintf("capability_usage_false_triggered_count:%d", c.FalseTriggeredCount),
			},
		}); ok {
			out = append(out, cand)
			n++
		}
	}

	n = 0
	for _, entry := range usage.Summary.TopIgnored {
		if n >= maxHarnessEditPerSignal {
			break
		}
		label := plainCapabilityLabel(entry.Ref)
		if cand, ok := newHarnessEditCandidate(harnessEditInput{
			Surface:  "injection_templates",
			SourceID: "capability_usage:ignored:" + entry.Ref,
			Title:    "Trim the ignored " + label + " injection",
			Action:   "trim the ignored " + label + " injection",
			Signal:   fmt.Sprintf("it was injected %d times, never used", entry.Count),
			Unlock:   "context injection stops carrying an unused ref",
			EvidenceRefs: []string{
				capgraph.CapabilityUsageProjectionRef,
				"capability_usage_ignored:" + entry.Ref,
				fmt.Sprintf("capability_usage_ignored_count:%d", entry.Count),
			},
		}); ok {
			out = append(out, cand)
			n++
		}
	}

	return out
}

func harnessEditBenchCandidates(reports []bench.Report) []candidate {
	var out []candidate
	for _, report := range reports {
		suiteLabel := firstNonEmpty(reviewartifact.PlainLabel(report.Suite), "the capability suite")
		if len(report.AbsentFromGraph) > 0 {
			if cand, ok := newHarnessEditCandidate(harnessEditInput{
				Surface:  "capability_edges",
				SourceID: "bench:absent_from_graph:" + report.Suite + ":" + report.GraphHash,
				Title:    "Close the " + suiteLabel + " graph parity gap",
				Action:   "close the " + suiteLabel + " capability graph parity gap",
				Signal:   fmt.Sprintf("%d bench cases miss graph nodes", len(report.AbsentFromGraph)),
				Unlock:   "the capability graph regains the edges the bench expects",
				EvidenceRefs: benchEvidenceRefs(report, append(
					[]string{fmt.Sprintf("bench_absent_from_graph_count:%d", len(report.AbsentFromGraph))},
					benchAbsentRefs(report)...,
				)),
			}); ok {
				out = append(out, cand)
			}
		}
		for _, split := range harnessEditWeakSplits(report) {
			splitLabel := reviewartifact.PlainLabel(split.name)
			if cand, ok := newHarnessEditCandidate(harnessEditInput{
				Surface:  "capability_edges",
				SourceID: "bench:weak_split:" + report.Suite + ":" + split.name,
				Title:    "Strengthen " + suiteLabel + " surfacing on the " + splitLabel + " split",
				Action:   "strengthen " + suiteLabel + " surfacing on the " + splitLabel + " split",
				Signal:   fmt.Sprintf("hit-at-1 is %d%% on %s", int(math.Round(split.hit1*100)), splitLabel),
				Unlock:   "the router surfaces the right capability more often",
				EvidenceRefs: benchEvidenceRefs(report, []string{
					"bench_weak_split:" + report.Suite + ":" + split.name,
					fmt.Sprintf("bench_weak_split_hit1:%.3f", split.hit1),
				}),
			}); ok {
				out = append(out, cand)
			}
		}
	}
	return out
}

func harnessEditRouteRemorseCandidates(reviews []routeevidence.RouteOutcomeReview) []candidate {
	var out []candidate
	n := 0
	for _, review := range reviews {
		if n >= maxHarnessEditPerSignal {
			break
		}
		if !review.Assessment.ShouldReviewPolicy {
			continue
		}
		classLabel := firstNonEmpty(reviewartifact.PlainLabel(review.Assessment.Class), "route remorse")
		if cand, ok := newHarnessEditCandidate(harnessEditInput{
			Surface:  "steering_config",
			SourceID: "route_remorse:" + review.ID,
			Title:    "Revisit routing guidance after " + classLabel,
			Action:   "revisit the routing guidance flagged by " + classLabel,
			Signal:   "the selected route did not prove out",
			Unlock:   "routing steering reflects the reviewed remorse evidence",
			EvidenceRefs: append([]string{
				"route_remorse_review:" + review.ID,
			}, review.EvidenceRefs...),
		}); ok {
			out = append(out, cand)
			n++
		}
	}
	return out
}

// newHarnessEditCandidate constructs a harness_edit candidate, or refuses. It
// refuses (returns ok=false) when the surface is empty, a hard-deny-core surface,
// or the signal carries no evidence — so a hard-deny-core surface can never be
// proposed, not merely denied at admission.
func newHarnessEditCandidate(in harnessEditInput) (candidate, bool) {
	surface := strings.TrimSpace(in.Surface)
	if surface == "" || isHardDenyCoreSurface(surface) {
		return candidate{}, false
	}
	refs := compactRefs(in.EvidenceRefs)
	if len(refs) == 0 {
		return candidate{}, false
	}
	surfaceLabel := reviewartifact.PlainLabel(surface)
	consequence := "Adding this creates a bounded, propose-only harness-edit proposal for the " + surfaceLabel + " surface"
	return candidate{
		Kind:             "harness_edit",
		SourceID:         in.SourceID,
		Title:            in.Title,
		Consequence:      consequence,
		CapabilityClass:  "judge-critique",
		Owner:            "compound-harness",
		CanonicalSurface: "director:harness-edit:" + surface + ":" + slug(in.SourceID),
		TargetSurface:    surface,
		EditAction:       in.Action,
		EditSignal:       in.Signal,
		SuccessPredicate: harnessEditSuccessPredicate(surface),
		DesiredOutcome:   firstNonEmpty(in.Unlock, consequence),
		Command:          []string{"compound", "bench", "run"},
		RequiredInputs: []string{
			"the capability-usage / bench / route-remorse evidence named in evidence_refs",
			"a before/after bench run on the affected suite",
		},
		ExpectedValidation: []string{
			"`compound bench compare` accepts the edit with no regression in either split (held_in and held_out)",
			"the operator admits the proposal only after the target surface is actively whitelisted in editable-surfaces",
		},
		StopConditions: []string{
			"the bench comparison shows a regression in either split",
			"the target surface is not actively whitelisted for edits",
		},
		Boundary:     "propose-only harness edit against one named editable surface; never a hard-deny-core surface; no autonomous admission, claim, advance, or apply",
		Proof:        "a follow-up bench report shows no regression on the affected suite, and the operator-admitted edit rode the editable-surfaces gate",
		EvidenceRefs: refs,
	}, true
}

func harnessEditSuccessPredicate(surface string) string {
	return "Edit the " + surface + " surface exactly as proposed, only after: run `compound bench run` on the affected suite before and after; `compound bench compare` accepts with no regression in either split; and the operator approves admission with the surface actively whitelisted. Do not apply the edit autonomously."
}

// harnessEditSurfaceForCapabilityRef maps a capability ref to the editable surface
// an edit would target, by node kind. `eval:` maps to the evaluator core surface
// on purpose: newHarnessEditCandidate then refuses it, keeping the grader outside
// the self-improvement loop.
func harnessEditSurfaceForCapabilityRef(ref string) string {
	switch capabilityNodeKind(ref) {
	case "skill":
		return "skill_descriptions"
	case "instruction_rule", "doctrine", "taste_standard":
		return "steering_config"
	case "eval":
		return "evaluator"
	default:
		return "capability_edges"
	}
}

func capabilityNodeKind(ref string) string {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "capability://")
	if i := strings.Index(ref, ":"); i > 0 {
		return ref[:i]
	}
	return ""
}

func plainCapabilityLabel(ref string) string {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "capability://")
	if i := strings.Index(ref, ":"); i >= 0 {
		ref = ref[i+1:]
	}
	if label := reviewartifact.PlainLabel(ref); label != "" {
		return label
	}
	return "capability"
}

func isHardDenyCoreSurface(surface string) bool {
	surface = strings.TrimSpace(surface)
	for _, s := range policy.HardDenyCoreSurfaces() {
		if strings.EqualFold(surface, s) {
			return true
		}
	}
	return false
}

type harnessEditSplit struct {
	name string
	hit1 float64
}

func harnessEditWeakSplits(report bench.Report) []harnessEditSplit {
	var out []harnessEditSplit
	for _, name := range []string{bench.SplitHeldIn, bench.SplitHeldOut} {
		m, ok := report.Splits[name]
		if !ok || m.Cases == 0 {
			continue
		}
		if m.Hit1 < harnessEditWeakHit1Cutoff {
			out = append(out, harnessEditSplit{name: name, hit1: m.Hit1})
		}
	}
	return out
}

func benchEvidenceRefs(report bench.Report, extra []string) []string {
	refs := []string{"bench_report:" + report.Suite}
	if strings.TrimSpace(report.GraphHash) != "" {
		refs = append(refs, "bench_graph_hash:"+report.GraphHash)
	}
	if strings.TrimSpace(report.Timestamp) != "" {
		refs = append(refs, "bench_report_timestamp:"+report.Timestamp)
	}
	return append(refs, extra...)
}

func benchAbsentRefs(report bench.Report) []string {
	var refs []string
	for _, node := range report.AbsentFromGraph {
		node = strings.TrimSpace(node)
		if node == "" {
			continue
		}
		refs = append(refs, "bench_absent_node:"+node)
		if len(refs) >= maxBenchAbsentRefs {
			break
		}
	}
	return refs
}

// loadLatestBenchReports reads $COMPOUND_HOME/reviews/bench/*.json and keeps the
// newest report per suite. Filenames are <ts>-<suite>.json, so lexicographic order
// is chronological and the last entry per suite wins. Reimplemented here (small,
// read-only) rather than importing cmd.
func loadLatestBenchReports(homePath string) ([]bench.Report, error) {
	dir := filepath.Join(homePath, "reviews", "bench")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	latest := map[string]bench.Report{}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var report bench.Report
		if json.Unmarshal(data, &report) != nil {
			continue // skip torn/foreign json rather than brick the scan
		}
		if report.Schema != bench.SchemaVersion || strings.TrimSpace(report.Suite) == "" {
			continue
		}
		if !report.IsActionable() {
			continue // synthetic_fixture / diagnostic reports are evidence, not suggested work
		}
		latest[report.Suite] = report
	}
	suites := make([]string, 0, len(latest))
	for suite := range latest {
		suites = append(suites, suite)
	}
	sort.Strings(suites)
	out := make([]bench.Report, 0, len(suites))
	for _, suite := range suites {
		out = append(out, latest[suite])
	}
	return out, nil
}

// loadRouteRemorseReviews reads $COMPOUND_HOME/reviews/route-remorse/*.json core
// artifacts (skipping the .reviewed/.apply/.decision siblings). The director is
// the first propose-time reader of this surface (Weng map item 6 gap).
func loadRouteRemorseReviews(homePath string) ([]routeevidence.RouteOutcomeReview, error) {
	dir := filepath.Join(homePath, "reviews", "route-remorse")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.Contains(name, ".reviewed.") || strings.Contains(name, ".apply.") || strings.Contains(name, ".decision.") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var out []routeevidence.RouteOutcomeReview
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var review routeevidence.RouteOutcomeReview
		if json.Unmarshal(data, &review) != nil {
			continue
		}
		if review.SchemaVersion != routeevidence.RouteOutcomeReviewSchemaVersion || strings.TrimSpace(review.ID) == "" {
			continue
		}
		out = append(out, review)
	}
	return out, nil
}

// harnessEditProposalCard is the propose-only operator review artifact written
// under reviews/harness-edit-proposals/. Its ReviewOperatorCard builds the
// plain-language card the console grammar requires; whether the surface is
// currently whitelisted is carried as INFORMATION (admission_status), never a
// reason to suppress the proposal.
type harnessEditProposalCard struct {
	SchemaVersion      string   `json:"schema_version"`
	ID                 string   `json:"id"`
	CreatedAt          string   `json:"created_at"`
	Status             string   `json:"status"`
	TriggerRef         string   `json:"trigger_ref"`
	TargetSurface      string   `json:"target_surface"`
	SurfaceLabel       string   `json:"surface_label"`
	Title              string   `json:"title"`
	EditAction         string   `json:"edit_action"`
	EditSignal         string   `json:"edit_signal"`
	Consequence        string   `json:"consequence"`
	SuccessPredicate   string   `json:"success_predicate"`
	SurfaceWhitelisted bool     `json:"surface_whitelisted"`
	AdmissionStatus    string   `json:"admission_status"`
	NonGoals           []string `json:"non_goals"`
	EvidenceRefs       []string `json:"evidence_refs"`
}

// ReviewOperatorCard builds the plain-language card. A card whose template hooks
// (action, signal, surface) are empty FAILS here rather than emitting mush (G6).
func (c harnessEditProposalCard) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	action, err := reviewartifact.RequireText("harness edit action", c.EditAction)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	signal, err := reviewartifact.RequireText("harness edit signal", c.EditSignal)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	surfaceLabel, err := reviewartifact.RequireText("harness edit surface", c.SurfaceLabel, reviewartifact.PlainLabel(c.TargetSurface))
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	summary := fmt.Sprintf("Compound suggests to %s because %s. Review the edit?",
		reviewartifact.ShortLabel(action, 64), reviewartifact.ShortLabel(signal, 40))
	return reviewartifact.NewOperatorCard(
		summary,
		surfaceLabel,
		harnessEditReviewArea+"|"+firstNonEmpty(c.TargetSurface, "surface"),
		firstNonEmpty(c.Consequence, "the harness gets a reviewed, bench-validated edit"),
	)
}

// recordHarnessEditCard writes one propose-only operator card. It reads the
// editable-surfaces registry (read-only) to report whether admission is currently
// blocked, and writes nothing else.
func recordHarnessEditCard(homePath string, cand candidate, proposal Proposal, now time.Time) error {
	whitelisted, _, err := policy.EditableSurfaceAllowed(homePath, cand.TargetSurface)
	if err != nil {
		return err
	}
	admission := "blocked_until_operator_whitelists_surface"
	if whitelisted {
		admission = "surface_actively_whitelisted"
	}
	card := harnessEditProposalCard{
		SchemaVersion:      harnessEditProposalCardSchema,
		ID:                 proposal.ID,
		CreatedAt:          now.UTC().Format(time.RFC3339),
		Status:             "review_required",
		TriggerRef:         proposal.TriggerRef,
		TargetSurface:      cand.TargetSurface,
		SurfaceLabel:       reviewartifact.PlainLabel(cand.TargetSurface),
		Title:              cand.Title,
		EditAction:         cand.EditAction,
		EditSignal:         cand.EditSignal,
		Consequence:        cand.DesiredOutcome,
		SuccessPredicate:   cand.SuccessPredicate,
		SurfaceWhitelisted: whitelisted,
		AdmissionStatus:    admission,
		NonGoals:           nonGoalsFor(cand),
		EvidenceRefs:       proposal.EvidenceRefs,
	}
	_, err = reviewartifact.Write(homePath, harnessEditReviewArea, proposal.ID, card, "  ", renderHarnessEditCard(card))
	return err
}

// --- Slice 2B: the review verb that resolves a harness-edit proposal card ---
//
// A harness-edit proposal card names ONE editable surface and rides in
// review_required until an operator decides. `compound authority
// harness-edit-review <card> --decision whitelist|reject --rationale <text>`
// records that decision. whitelist opens the target surface for edits (writes
// the editable-surfaces registry through policy.ActivateSurface); reject writes
// no registry byte. Both record a decision sidecar so the card leaves needs-you.
// The verb never executes, admits, or applies the edit — it only opens (or
// declines to open) the surface and files the decision. It mirrors the
// authority job-health-review shape: validate-then-write, record into a sidecar
// the annotator reads, refuse (writing nothing) on any invalid input.

const (
	harnessEditReviewSchemaVersion = "compound.harness_edit_review.v0.1"
	// harnessEditReviewedSuffix names the recorded-decision sidecar written
	// beside a card as <card-id>.harness-edit.reviewed.json. It ends in
	// .reviewed.json so the queue excludes it from being its own review item,
	// yet does NOT collide with the generic <card-id>.reviewed.json resolution
	// sidecar (reviewartifact/resolution.go) — so a harness-edit decision keeps
	// its whitelist|reject fidelity instead of collapsing to reviewed/parked.
	harnessEditReviewedSuffix  = ".harness-edit.reviewed.json"
	harnessEditDefaultReviewer = "operator-console"

	// HarnessEditDecisionWhitelist opens the card's target surface for edits
	// (writes the editable-surfaces registry); HarnessEditDecisionReject
	// declines and writes no registry byte. Both decisions resolve the card.
	HarnessEditDecisionWhitelist = "whitelist"
	HarnessEditDecisionReject    = "reject"
)

// HarnessEditReviewInput carries the CLI flags into RecordHarnessEditReview.
// ReviewedBy defaults to operator-console when empty.
type HarnessEditReviewInput struct {
	CardPath   string
	Decision   string
	Rationale  string
	ReviewedBy string
}

// HarnessEditReviewReport is the recorded-decision sidecar. It is deliberately
// NOT the generic review-resolution schema so the whitelist|reject decision (and
// its rationale/provenance) survives verbatim.
type HarnessEditReviewReport struct {
	SchemaVersion      string `json:"schema_version"`
	ID                 string `json:"id"`
	CreatedAt          string `json:"created_at"`
	Status             string `json:"status"`
	CardID             string `json:"card_id"`
	TargetSurface      string `json:"target_surface"`
	Decision           string `json:"decision"`
	Rationale          string `json:"rationale"`
	ReviewedBy         string `json:"reviewed_by"`
	SurfaceWhitelisted bool   `json:"surface_whitelisted"`
	SourcePath         string `json:"source_path,omitempty"`
	ReviewPath         string `json:"review_path,omitempty"`
}

// RecordHarnessEditReview validates the decision, then records it. Every refusal
// (unknown decision, empty rationale, missing/schema-invalid card, a card path
// outside COMPOUND_HOME/reviews) returns an error BEFORE any write, so a refused
// run leaves the registry and card byte-identical. On whitelist it opens the
// card's target surface via policy.ActivateSurface (idempotent); on reject it
// writes no registry byte. Both write one decision sidecar.
func RecordHarnessEditReview(homePath string, in HarnessEditReviewInput, now time.Time) (HarnessEditReviewReport, error) {
	decision := strings.TrimSpace(in.Decision)
	if decision != HarnessEditDecisionWhitelist && decision != HarnessEditDecisionReject {
		return HarnessEditReviewReport{}, fmt.Errorf("harness-edit review decision must be %s|%s, got %q", HarnessEditDecisionWhitelist, HarnessEditDecisionReject, in.Decision)
	}
	rationale := strings.TrimSpace(in.Rationale)
	if rationale == "" {
		return HarnessEditReviewReport{}, fmt.Errorf("harness-edit review rationale required")
	}
	reviewedBy := strings.TrimSpace(in.ReviewedBy)
	if decision == HarnessEditDecisionWhitelist {
		// whitelist opens the card's target surface for edits (it writes the
		// editable-surfaces registry), so the default reviewer can never satisfy
		// it: an executor could author a card naming its own steering surface and
		// whitelist it with no operator in the loop. Require an explicit
		// reviewer carrying operator provenance BEFORE any write.
		if reviewedBy == "" {
			return HarnessEditReviewReport{}, fmt.Errorf("harness-edit review whitelist requires an explicit reviewer with operator provenance (--actor operator:<id>); the %q default cannot open a surface", harnessEditDefaultReviewer)
		}
		if !policy.HasOperatorProvenance(reviewedBy) {
			return HarnessEditReviewReport{}, fmt.Errorf("harness-edit review whitelist requires operator provenance on reviewed_by, got %q", reviewedBy)
		}
	}
	if reviewedBy == "" {
		reviewedBy = harnessEditDefaultReviewer
	}
	card, stem, err := loadHarnessEditProposalCardForReview(homePath, in.CardPath)
	if err != nil {
		return HarnessEditReviewReport{}, err
	}
	surface := strings.TrimSpace(card.TargetSurface)

	whitelisted := false
	if decision == HarnessEditDecisionWhitelist {
		if _, _, err := policy.ActivateSurface(homePath, surface); err != nil {
			return HarnessEditReviewReport{}, err
		}
		whitelisted = true
	}

	createdAt := reviewartifact.Timestamp(now)
	report := HarnessEditReviewReport{
		SchemaVersion:      harnessEditReviewSchemaVersion,
		ID:                 reviewartifact.ID("her_", card.ID, decision, createdAt),
		CreatedAt:          createdAt,
		Status:             "recorded",
		CardID:             card.ID,
		TargetSurface:      surface,
		Decision:           decision,
		Rationale:          rationale,
		ReviewedBy:         reviewedBy,
		SurfaceWhitelisted: whitelisted,
		SourcePath:         in.CardPath,
		ReviewPath:         filepath.Join(homePath, "reviews", harnessEditReviewArea, stem+harnessEditReviewedSuffix),
	}
	if _, err := reviewartifact.WriteJSON(homePath, harnessEditReviewArea, stem+harnessEditReviewedSuffix, report, " "); err != nil {
		return HarnessEditReviewReport{}, err
	}
	return report, nil
}

// loadHarnessEditProposalCardForReview resolves and validates the card path: it
// must sit under COMPOUND_HOME/reviews, be a readable .json, and carry the
// harness-edit card schema with a target_surface. It returns the parsed card and
// its filename stem (which names the decision sidecar). Every failure is a
// refusal the caller surfaces before writing anything.
func loadHarnessEditProposalCardForReview(homePath, cardPath string) (harnessEditProposalCard, string, error) {
	homeAbs, err := filepath.Abs(strings.TrimSpace(homePath))
	if err != nil {
		return harnessEditProposalCard{}, "", err
	}
	cardAbs, err := filepath.Abs(strings.TrimSpace(cardPath))
	if err != nil {
		return harnessEditProposalCard{}, "", err
	}
	rel, err := filepath.Rel(homeAbs, cardAbs)
	if err != nil {
		return harnessEditProposalCard{}, "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || !strings.HasPrefix(rel, "reviews/") {
		return harnessEditProposalCard{}, "", fmt.Errorf("harness-edit card %s is outside COMPOUND_HOME/reviews", cardAbs)
	}
	if filepath.Ext(cardAbs) != ".json" {
		return harnessEditProposalCard{}, "", fmt.Errorf("harness-edit card must be a .json file: %s", cardAbs)
	}
	data, err := os.ReadFile(cardAbs)
	if err != nil {
		return harnessEditProposalCard{}, "", err
	}
	var card harnessEditProposalCard
	if err := json.Unmarshal(data, &card); err != nil {
		return harnessEditProposalCard{}, "", fmt.Errorf("parse harness-edit card %s: %w", cardAbs, err)
	}
	if card.SchemaVersion != harnessEditProposalCardSchema {
		return harnessEditProposalCard{}, "", fmt.Errorf("harness-edit card schema_version must be %s, got %q", harnessEditProposalCardSchema, card.SchemaVersion)
	}
	if strings.TrimSpace(card.TargetSurface) == "" {
		return harnessEditProposalCard{}, "", fmt.Errorf("harness-edit card missing target_surface")
	}
	stem := strings.TrimSuffix(filepath.Base(cardAbs), ".json")
	return card, stem, nil
}

// LoadHarnessEditReviewReport reads a recorded decision sidecar.
func LoadHarnessEditReviewReport(path string) (HarnessEditReviewReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HarnessEditReviewReport{}, err
	}
	var report HarnessEditReviewReport
	if err := json.Unmarshal(data, &report); err != nil {
		return HarnessEditReviewReport{}, err
	}
	return report, nil
}

// AnnotateHarnessEditProposalQueue gives every OPEN harness-edit card exactly one
// governed action — "Allow edits to this surface" — whose argv is the review verb
// with --decision whitelist (never --rationale; the console appends the operator's
// text). A card that already carries a recorded-decision sidecar is banded out of
// needs-you and carries no action. It owns only its own area's items.
func AnnotateHarnessEditProposalQueue(report *reviewartifact.QueueReport) {
	if report == nil {
		return
	}
	for i := range report.Items {
		item := &report.Items[i]
		if item.Area != harnessEditReviewArea {
			continue
		}
		annotateHarnessEditProposalItem(item)
	}
}

func annotateHarnessEditProposalItem(item *reviewartifact.QueueItem) {
	if reviewedPath := harnessEditReviewedPath(*item); reviewedPath != "" {
		item.Status = "recorded"
		item.Readiness = "review_recorded"
		item.Actions = nil
		item.BlockedReason = ""
		item.Band = reviewartifact.BandFiled
		item.NextAction = "harness-edit review recorded"
		if rep, err := LoadHarnessEditReviewReport(reviewedPath); err == nil {
			switch rep.Decision {
			case HarnessEditDecisionWhitelist:
				item.NextAction = "surface whitelisted for edits; the proposal can be admitted when ready"
			case HarnessEditDecisionReject:
				item.NextAction = "harness-edit proposal declined by " + reviewartifact.FirstNonEmpty(rep.ReviewedBy, "operator")
			}
		}
		return
	}
	if strings.TrimSpace(item.JSONPath) == "" {
		item.Readiness = "blocked"
		item.BlockedReason = "harness-edit proposal path missing"
		item.Band = reviewartifact.BandNeedsYou
		return
	}
	argv := []string{"compound", "authority", "harness-edit-review", item.JSONPath, "--decision", HarnessEditDecisionWhitelist, "--json"}
	item.Actions = []reviewartifact.Action{{
		ID:             "harness-edit.whitelist",
		Label:          "Allow edits to this surface",
		Command:        "compound authority harness-edit-review " + item.JSONPath + " --decision " + HarnessEditDecisionWhitelist,
		Argv:           argv,
		RequiresReview: true,
	}}
	seedHarnessEditReviewAuthority(item)
	item.Readiness = "needs_review"
	item.Band = reviewartifact.BandNeedsYou
}

func seedHarnessEditReviewAuthority(item *reviewartifact.QueueItem) {
	if item == nil {
		return
	}
	workID := reviewartifact.AuthoritySeedWorkID(*item, "")
	if workID == "" || len(item.Actions) != 1 {
		return
	}
	homePath := filepath.Dir(filepath.Dir(filepath.Dir(item.JSONPath)))
	card, _, err := loadHarnessEditProposalCardForReview(homePath, item.JSONPath)
	if err != nil {
		item.Warnings = reviewartifact.Unique(append(item.Warnings, "harness-edit authority seed blocked: "+err.Error()))
		return
	}
	evidenceRefs := reviewartifact.UniqueSorted(append(append([]string{}, card.EvidenceRefs...),
		"harness-edit-proposal:"+card.ID,
		"harness-edit-surface:"+card.TargetSurface,
	))
	if err := reviewartifact.SeedActionAuthority(&item.Actions[0], reviewartifact.ActionAuthoritySeedSpec{
		WorkID: workID, CreatedAt: card.CreatedAt, ProducerName: "compound/internal/director",
		Capability: "harness_edit.whitelist", Action: "whitelist_harness_edit_surface",
		Target:        envelope.ActionTarget{Kind: "editable_surface", ID: card.TargetSurface, Path: item.JSONPath},
		MutationClass: "security", Reversibility: "manual", BlastRadius: "control_plane",
		Preview:      "Whitelist editable surface " + card.TargetSurface + " for the exact reviewed harness-edit proposal " + card.ID + ".",
		RollbackPlan: "Retire the editable-surface whitelist record and verify subsequent admission is denied again.",
		Privacy:      "private", Risk: "tier2", EvidenceRefs: evidenceRefs, TargetScope: "private",
		ExpectedReceipt: "Recorded reviewed editable-surface whitelist decision for " + card.TargetSurface + ".",
		ExpectedProof: append([]string{
			"harness-edit-proposal:" + card.ID,
			"harness-edit-surface:" + card.TargetSurface,
			"editable-surface-admission-proof:" + card.TargetSurface,
		}, evidenceRefs...),
	}); err != nil {
		item.Warnings = reviewartifact.Unique(append(item.Warnings, "harness-edit authority seed blocked: "+err.Error()))
	}
}

func harnessEditReviewedPath(item reviewartifact.QueueItem) string {
	for _, path := range item.ExtraPaths {
		if strings.HasSuffix(path, harnessEditReviewedSuffix) {
			return path
		}
	}
	return ""
}

func renderHarnessEditCard(card harnessEditProposalCard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Harness-edit proposal: %s\n\n", card.Title)
	fmt.Fprintf(&b, "- target_surface: %s\n", card.TargetSurface)
	fmt.Fprintf(&b, "- admission_status: %s\n", card.AdmissionStatus)
	fmt.Fprintf(&b, "- trigger_ref: %s\n", card.TriggerRef)
	fmt.Fprintf(&b, "- success_predicate: %s\n\n", card.SuccessPredicate)
	fmt.Fprintln(&b, "This is a propose-only harness-edit proposal. It admits nothing, applies nothing, and mutates no owner state; admission stays operator-gated behind the editable-surfaces whitelist.")
	if len(card.NonGoals) > 0 {
		fmt.Fprint(&b, "\n## Non-goals\n\n")
		for _, ng := range card.NonGoals {
			fmt.Fprintf(&b, "- %s\n", ng)
		}
	}
	if len(card.EvidenceRefs) > 0 {
		fmt.Fprint(&b, "\n## Evidence\n\n")
		for _, ref := range card.EvidenceRefs {
			fmt.Fprintf(&b, "- %s\n", ref)
		}
	}
	return b.String()
}
