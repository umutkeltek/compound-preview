package work

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Coordination projects actor activity, evidenced work constraints and shared
// queue availability. It keeps the planning half of work continue unchanged:
// one Workbench, dry-run candidate prep, continue eligibility and a ReadOnly
// lane-capacity probe. Constraint synthesis uses only held/owned work, the
// exact selected item and actor-owned previews. Other open work is not an
// execution dependency. No dispatch, claim, advance, admission or recording
// occurs; an unchanged home remains byte-identical.
//
// Actors are generic: enumerated from the executor fleet rows, lease holders
// (lease.By), and live claimers — nothing hardcoded. `codex`/`claude` are just
// executors that happen to be bound in; the projection never names them.

const CoordinationSchemaVersion = "compound.coordination.v0.1"

// CoordinationOptions controls the read-only coordination projection.
type CoordinationOptions struct {
	// Actor selects who "me" is. Empty picks the most-present actor
	// deterministically (presence then lexicographic); ActorExplicit reports
	// whether the caller chose.
	Actor string
	// CapacityGuard overrides the package-level lane-capacity guard for THIS
	// request only. Nil keeps the existing global guard. The guard is chosen
	// once per request and used for every actor and every check, so one request
	// never mixes two guards (and never observes a global that another writer
	// swapped mid-projection). A caller that injects a request-scoped
	// observation gets one source read per request instead of one per actor.
	CapacityGuard LaneCapacityGuardFunc
}

// CoordinationLane is the lane-capacity gate for an actor at projection time:
// the dispatch-plan capacity annotation plus the guard's refuse/hold verdict.
// Nil on ActorCoordination means no gate applied (the actor is not at a
// dispatch point, or no capacity adapter is installed).
type CoordinationLane struct {
	State                string  `json:"state,omitempty"`
	Remaining            float64 `json:"remaining"`
	ResetsAt             string  `json:"resets_at,omitempty"`
	Refused              bool    `json:"refused,omitempty"`
	ConfirmationRequired bool    `json:"confirmation_required,omitempty"`
	Message              string  `json:"message,omitempty"`
}

type CoordinationDoing struct {
	WorkID                string `json:"work_id,omitempty"`
	Title                 string `json:"title,omitempty"`
	State                 string `json:"state,omitempty"`
	NextAction            string `json:"next_action,omitempty"`
	ClaimedBy             string `json:"claimed_by,omitempty"`
	Owner                 string `json:"owner,omitempty"`
	AccountableOwner      string `json:"accountable_owner,omitempty"`
	CreatorHarness        string `json:"creator_harness,omitempty"`
	CreatorRequestedModel string `json:"creator_requested_model,omitempty"`
	CreatorLaneID         string `json:"creator_lane_id,omitempty"`
}

// ActorCoordination is one actor's coordination face — the same shape for
// "me" and for every other actor (nothing is special-cased per identity).
type ActorCoordination struct {
	Actor       string             `json:"actor"`
	Doing       *CoordinationDoing `json:"doing,omitempty"`
	NextAction  string             `json:"next_action,omitempty"`
	CanContinue bool               `json:"can_continue"`
	WaitReason  string             `json:"wait_reason,omitempty"`
	BlockedOn   []string           `json:"blocked_on,omitempty"`
	Lane        *CoordinationLane  `json:"lane,omitempty"`
	// Counts describe the shared queue, not this actor's commitments.
	Open        int `json:"open"`
	InProgress  int `json:"in_progress"`
	NeedsReview int `json:"needs_review"`
}

// WaitEdge is one evidenced work or lane constraint for an actor's queue.
// Work-scoped edges carry the work; actor-scoped edges (lane gates, empty
// workbench) leave Work empty.
type WaitEdge struct {
	Work      string            `json:"work,omitempty"`
	Actor     string            `json:"actor"`
	Reason    string            `json:"reason"`
	BlockedOn []string          `json:"blocked_on,omitempty"`
	Lane      *CoordinationLane `json:"lane,omitempty"`
}

// CoordinationReport is compound.coordination.v0.1: me, every other actor
// (generic), and the actor work constraints — consumed by the console, the
// terminal, or another agent.
type CoordinationReport struct {
	SchemaVersion string              `json:"schema_version"`
	GeneratedAt   string              `json:"generated_at"`
	Actor         string              `json:"actor"`
	ActorExplicit bool                `json:"actor_explicit"`
	Me            ActorCoordination   `json:"me"`
	Others        []ActorCoordination `json:"others"`
	Waits         []WaitEdge          `json:"waits"`
	Integrity     []string            `json:"integrity,omitempty"`
}

// Coordination projects the read-only coordination view. It is a pure observer:
// workbench reads, dry-run candidate prep, and a ReadOnly lane-capacity probe —
// it never dispatches, claims, advances, admits, or records.
//
// The lane-capacity guard is chosen ONCE per request (the request's own guard
// when given, otherwise the package-level one) and that one guard answers for
// every actor, so a request never mixes two gates. A caller that injects a
// request-scoped observation (routeevidence.NewCoordinationCapacityObservation)
// makes the projection read each capacity source at most once per request
// instead of once per eligible actor.
//
// A guard failure is NOT swallowed. An unreadable capacity source used to
// arrive as a nil lane, which the projection read as "no gate applied" — an
// unreadable source looked exactly like an ungated actor. It now travels out
// through this error return.
func (s *Store) Coordination(now time.Time, opts CoordinationOptions) (CoordinationReport, error) {
	report := CoordinationReport{
		SchemaVersion: CoordinationSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Others:        []ActorCoordination{},
		Waits:         []WaitEdge{},
	}
	// Choose the guard ONCE. The request's own guard wins; otherwise the
	// package-level guard applies. Reading the global per actor could mix two
	// guards inside one projection.
	guard := opts.CapacityGuard
	if guard == nil {
		guard = LaneCapacityGuard
	}
	workbench, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 8})
	if err != nil {
		return CoordinationReport{}, err
	}
	report.Integrity = mergeUnique(report.Integrity, workbench.Integrity)
	projection, err := s.ExecutorsProjection(now, ExecutorsProjectionOptions{Workbench: &workbench})
	if err != nil {
		return CoordinationReport{}, err
	}
	actors, involved := coordinationActorsFromSnapshot(projection, workbench)
	explicit := strings.TrimSpace(opts.Actor) != ""
	me := strings.TrimSpace(opts.Actor)

	faces := make([]ActorCoordination, 0, len(actors))
	presence := make(map[string]int, len(actors))
	edgeKeys := make(map[string]struct{})
	gateNeeded := false
	for _, actor := range actors {
		_, isInvolved := involved[actor]
		face, waits, needsGate, err := s.coordinationActor(now, actor, isInvolved, workbench, edgeKeys, guard)
		if err != nil {
			return CoordinationReport{}, err
		}
		gateNeeded = gateNeeded || needsGate
		presence[actor] = coordinationPresence(face)
		faces = append(faces, face)
		report.Waits = append(report.Waits, waits...)
	}
	if !explicit {
		me = coordinationDefaultActor(actors, presence)
	}
	report.Actor = me
	report.ActorExplicit = explicit

	meSet := false
	for _, face := range faces {
		report.Integrity = mergeUnique(report.Integrity, face.integrityNotes())
		if !meSet && face.Actor == me {
			report.Me = face
			meSet = true
			continue
		}
		report.Others = append(report.Others, face)
	}
	if !meSet {
		// Explicit actor with no live presence (no fleet row, lease, claim,
		// or admission preview): still honor the choice AND project its real
		// workbench perspective, so a console "view as actor X" never silently
		// re-targets or flattens X to an empty face. Its shared-queue summary
		// remains visible without inventing relationships to foreign work.
		if explicit && strings.TrimSpace(me) != "" {
			face, waits, needsGate, aerr := s.coordinationActor(now, me, true, workbench, edgeKeys, guard)
			if aerr != nil {
				return CoordinationReport{}, aerr
			}
			report.Me = face
			report.Waits = append(report.Waits, waits...)
			report.Integrity = mergeUnique(report.Integrity, face.integrityNotes())
			if needsGate && guard == nil {
				report.Integrity = mergeUnique(report.Integrity, []string{"lane_capacity_guard_not_installed"})
			}
		} else {
			report.Me = ActorCoordination{Actor: me}
		}
	}
	sort.SliceStable(report.Others, func(i, j int) bool {
		pi, pj := presence[report.Others[i].Actor], presence[report.Others[j].Actor]
		if pi != pj {
			return pi > pj
		}
		return report.Others[i].Actor < report.Others[j].Actor
	})
	if gateNeeded && guard == nil {
		report.Integrity = mergeUnique(report.Integrity, []string{"lane_capacity_guard_not_installed"})
	}
	return report, nil
}

// coordinationActor projects one actor's face plus its wait edges. needsGate
// reports whether the actor stood at a dispatch point where a lane-capacity
// gate would decide (used only for the integrity note about a missing guard).
// involved marks actors with a live lease or claim — only involved actors (or
// an explicitly chosen me) contribute wait edges to the global graph. guard is
// the request's chosen lane-capacity guard: the same one for every actor.
func (s *Store) coordinationActor(now time.Time, actor string, involved bool, workbench WorkbenchReport, edgeKeys map[string]struct{}, guard LaneCapacityGuardFunc) (ActorCoordination, []WaitEdge, bool, error) {
	face := ActorCoordination{Actor: actor}
	wb := workbench
	wb.DispatchNext = workbenchDispatchNext(wb, actor)
	face.Open = wb.Counts.Open
	face.InProgress = wb.Counts.InProgress
	face.NeedsReview = wb.Counts.NeedsReview
	face.Doing = coordinationDoing(wb, actor)
	waitWorkbench := coordinationWaitWorkbench(wb, actor)

	atDispatchPoint := wb.DispatchNext.CanRun
	lane, err := s.coordinationLane(now, actor, wb, guard)
	if err != nil {
		return ActorCoordination{}, nil, false, err
	}
	face.Lane = lane
	gated := coordinationLaneGated(face.Lane)

	canContinue, _ := continueCanContinue(wb, actor, "")
	if canContinue && !wb.DispatchNext.CanRun && continueAdmissionPreview(wb.AdmissionPreviews, "") == nil {
		// Continuation would be a candidate prepare: run the exact dry-run
		// advance plan `continue` runs (AdvancePlan without Record) so
		// can_continue is true only when policy admits the prepare.
		if item, _, _ := s.continuePrepareCandidate(now, wb, actor, ""); item == nil {
			canContinue = false
		}
	}
	if gated {
		canContinue = false
	}
	face.CanContinue = canContinue

	if canContinue {
		face.NextAction = coordinationNextAction(wb, actor)
	} else {
		// Reuse continue's wait classifiers on the actor's scoped constraints,
		// carrying the actual capacity verdict so lane.* gates stay visible.
		synth := ContinueResult{Actor: actor}
		switch {
		case face.Lane != nil && face.Lane.Refused:
			synth.Status = "capacity_refused"
		case face.Lane != nil && face.Lane.ConfirmationRequired:
			synth.Status = "capacity_confirmation_required"
		}
		face.WaitReason, face.BlockedOn = continueWait(synth, waitWorkbench)
		if len(waitWorkbench.Open) == 0 && len(waitWorkbench.AdmissionPreviews) == 0 && !gated {
			// Shared-queue availability is an actor summary, not a work edge.
			face.BlockedOn = nil
			face.WaitReason = "no_dispatchable_work"
			if wb.Counts.Open == 0 && len(wb.AdmissionPreviews) == 0 {
				face.WaitReason = "no_open_work"
			}
		}
		face.NextAction = continueNoAction(wb, "")
	}

	var waits []WaitEdge
	addEdge := func(edge WaitEdge) {
		key := edge.Work + "\x00" + edge.Actor + "\x00" + edge.Reason
		if _, dup := edgeKeys[key]; dup {
			return
		}
		edgeKeys[key] = struct{}{}
		waits = append(waits, edge)
	}
	// Edges land in the global graph only for a blocked actor with live skin
	// in the game (lease/claim) — an idle fleet row is idle, not "waiting,"
	// and an actor that can continue waits on nothing.
	if involved && !face.CanContinue {
		// Only the actor's actual queue or exact selected item can contribute
		// work constraints. Other open projects are availability, not causality.
		for _, item := range waitWorkbench.Open {
			reason, token := coordinationItemWait(item, actor)
			if reason == "" {
				continue
			}
			addEdge(WaitEdge{
				Work:      item.WorkID,
				Actor:     actor,
				Reason:    reason,
				BlockedOn: []string{token},
			})
		}
		// Actor-scoped edge: the continue-style wait (incl. lane.* edges).
		if len(face.BlockedOn) > 0 || gated {
			addEdge(WaitEdge{
				Actor:     actor,
				Reason:    face.WaitReason,
				BlockedOn: append([]string{}, face.BlockedOn...),
				Lane:      face.Lane,
			})
		}
	}
	return face, waits, atDispatchPoint, nil
}

// coordinationWaitWorkbench scopes constraint synthesis without changing the
// full workbench used for eligibility, selection, candidate prep or lane probes.
// A named foreign item remains classifiable when an owner has selected it
// exactly; mere presence elsewhere in the queue never supplies that relation.
func coordinationWaitWorkbench(wb WorkbenchReport, actor string) WorkbenchReport {
	out := wb
	out.Open = nil
	out.AdmissionPreviews = nil
	for _, item := range wb.Open {
		if item.ClaimedBy == actor ||
			(strings.TrimSpace(item.ClaimedBy) == "" && item.Owner == actor) ||
			(wb.DispatchNext.WorkID != "" && item.WorkID == wb.DispatchNext.WorkID) {
			out.Open = append(out.Open, item)
		}
	}
	for _, preview := range wb.AdmissionPreviews {
		if preview.Actor == actor ||
			(strings.TrimSpace(preview.Actor) == "" && preview.WorkOwner == actor) {
			out.AdmissionPreviews = append(out.AdmissionPreviews, preview)
		}
	}
	out.Counts.Open = len(out.Open)
	return out
}

// coordinationLane probes the lane-capacity gate for an actor exactly where a
// dispatch would meet it: when the workbench says a run is dispatchable. The
// probe is ReadOnly, the same seam DispatchPlan calls with ReadOnly:true.
//
// A guard error is NOT swallowed. An unreadable capacity source used to land
// here as a nil lane, which the projection read as "no gate" — an unreadable
// source looked exactly like an ungated actor. The error now travels out
// through Coordination's own error return so the caller sees a hard failure
// instead of a fabricated clearance.
func (s *Store) coordinationLane(now time.Time, actor string, wb WorkbenchReport, guard LaneCapacityGuardFunc) (*CoordinationLane, error) {
	if guard == nil || !wb.DispatchNext.CanRun {
		return nil, nil
	}
	result, err := guard(LaneCapacityGuardRequest{
		HomePath: s.home,
		Lane:     actor,
		WorkID:   strings.TrimSpace(wb.DispatchNext.WorkID),
		ReadOnly: true,
		Now:      now,
	})
	if err != nil {
		return nil, fmt.Errorf("coordination: lane capacity for actor %s: %w", actor, err)
	}
	lane := CoordinationLane{
		State:                result.Capacity.State,
		Remaining:            result.Capacity.Remaining,
		ResetsAt:             result.Capacity.ResetsAt,
		Refused:              result.Refused,
		ConfirmationRequired: result.ConfirmationRequired,
		Message:              result.Message,
	}
	if lane.State == "" && lane.Remaining == 0 && lane.ResetsAt == "" && !lane.Refused && !lane.ConfirmationRequired && lane.Message == "" {
		return nil, nil
	}
	return &lane, nil
}

func coordinationLaneGated(lane *CoordinationLane) bool {
	if lane == nil {
		return false
	}
	if lane.Refused || lane.ConfirmationRequired {
		return true
	}
	switch lane.State {
	case "walled", "dead", "refused":
		return true
	default:
		return false
	}
}

// coordinationItemWait classifies one open work as a wait exactly the way
// dispatchStepWait classifies open items (same reasons, same work.* tokens).
// Empty reason means the item is not a wait for this actor (ready/dispatchable
// work, or the actor's own claim).
func coordinationItemWait(item WorkbenchItem, actor string) (reason, token string) {
	switch item.State {
	case "held", "blocked":
		return "needs_review", "work.review:" + item.WorkID
	case "running":
		return "run_evidence", "work.run_evidence:" + item.WorkID
	case "verifying":
		if !hasCloseProofEvidence(item) {
			return "close_proof", "work.close_proof:" + item.WorkID
		}
		return "", ""
	case "claimed":
		if strings.TrimSpace(item.ClaimedBy) != "" && item.ClaimedBy != actor {
			return "claimed_by_other_actor", "work.claimed_by_other_actor:" + item.WorkID
		}
		return "", ""
	case "proposed", "admitted":
		return "not_ready", "work.ready_state:" + item.WorkID
	default:
		return "", ""
	}
}

// coordinationDoing picks the work that best represents what the actor is on.
// Doing is strictly actor-scoped: only work the actor holds (claim, or owner
// of a still-running work with no live claim). Another actor's running work
// is neither its doing nor an inferred dependency. Nil means the actor is idle —
// itself coordination truth.
func coordinationDoing(wb WorkbenchReport, actor string) *CoordinationDoing {
	if strings.TrimSpace(actor) == "" {
		return nil
	}
	inMotion := func(state string) bool {
		switch state {
		case "claimed", "running", "verifying", "held", "blocked":
			return true
		default:
			return false
		}
	}
	pick := -1
	rank := func(i int) int {
		item := wb.Open[i]
		mine := item.ClaimedBy == actor
		switch {
		case item.State == "running" && mine:
			return 6
		case item.State == "verifying" && mine:
			return 5
		case item.State == "claimed" && mine:
			return 4
		case inMotion(item.State) && mine:
			return 3
		case item.State == "running" && item.Owner == actor && strings.TrimSpace(item.ClaimedBy) == "":
			return 1
		default:
			return 0
		}
	}
	best := 0
	for i := range wb.Open {
		if r := rank(i); r > best {
			best, pick = r, i
		}
	}
	if pick < 0 {
		return nil
	}
	item := wb.Open[pick]
	return &CoordinationDoing{
		WorkID:                item.WorkID,
		Title:                 firstNonEmpty(item.DesiredOutcome, item.WorkID),
		State:                 item.State,
		NextAction:            item.NextAction,
		ClaimedBy:             item.ClaimedBy,
		Owner:                 item.Owner,
		AccountableOwner:      item.AccountableOwner,
		CreatorHarness:        item.CreatorHarness,
		CreatorRequestedModel: item.CreatorRequestedModel,
		CreatorLaneID:         item.CreatorLaneID,
	}
}

// coordinationNextAction mirrors the can-continue branches of
// continueNextAction without having executed a step.
func coordinationNextAction(wb WorkbenchReport, actor string) string {
	if wb.DispatchNext.CanRun {
		for _, item := range wb.Open {
			if item.WorkID == wb.DispatchNext.WorkID && strings.TrimSpace(item.NextAction) != "" {
				return item.NextAction
			}
		}
		return "continue next safe lifecycle step"
	}
	for _, item := range wb.Open {
		if (item.State == "proposed" || item.State == "admitted") && !workActionContextRequiresOperator(item.ActionContext) {
			return "prepare candidate to ready"
		}
	}
	if continueAdmissionPreview(wb.AdmissionPreviews, "") != nil {
		return "add suggested task to work"
	}
	return "continue next safe lifecycle step"
}

// coordinationActors enumerates the generic actor set: executor fleet rows,
// live lease holders, live claimers, and actors with an admission preview
// (a suggested task awaiting their admit — continue's first step). Sorted for
// deterministic output. The involved set (lease holders ∪ claimers) marks
// actors with live skin in the game — only involved actors emit wait edges;
// an idle fleet row is idle, not "waiting."
func coordinationActorsFromSnapshot(projection ExecutorsProjection, wb WorkbenchReport) (actors []string, involved map[string]struct{}) {
	seen := make(map[string]struct{})
	involved = make(map[string]struct{})
	add := func(a string, isInvolvement bool) {
		a = strings.TrimSpace(a)
		if a == "" {
			return
		}
		if isInvolvement {
			involved[a] = struct{}{}
		}
		if _, dup := seen[a]; dup {
			return
		}
		seen[a] = struct{}{}
		actors = append(actors, a)
	}
	for _, row := range projection.Executors {
		add(row.ID, false)
		for _, lease := range row.Leases {
			add(lease.By, true)
		}
	}
	for _, item := range wb.Open {
		add(item.ClaimedBy, true)
	}
	for _, preview := range wb.AdmissionPreviews {
		add(preview.Actor, false)
	}
	sort.Strings(actors)
	return actors, involved
}

func coordinationPresence(face ActorCoordination) int {
	p := face.NeedsReview * 2
	if face.Doing != nil {
		switch face.Doing.State {
		case "running":
			p += 6
		case "verifying":
			p += 5
		case "claimed":
			p += 4
		default:
			p += 2
		}
	}
	if face.Open > 0 {
		p++
	}
	return p
}

func coordinationDefaultActor(actors []string, presence map[string]int) string {
	best := ""
	bestScore := -1
	for _, actor := range actors { // actors sorted: ties resolve lexicographic
		if score := presence[actor]; score > bestScore {
			best, bestScore = actor, score
		}
	}
	return best
}

// integrityNotes lets ActorCoordination carry per-actor degradation facts
// without exporting them onto the wire (kept off the JSON face).
func (face ActorCoordination) integrityNotes() []string { return nil }
