package mem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

const (
	reconcileCheckpointSchema = "compound.synthesis_reconcile_checkpoint.v0.1"
	reconcileParkSampleLimit  = 5
	reconcileConfidenceFloor  = 0.5

	reconcileParkMissingDate          = "missing_date"
	reconcileParkUnlinkedClaim        = "unlinked_claim"
	reconcileParkAmbiguous            = "ambiguous"
	reconcileParkLowConfidence        = "low_confidence"
	reconcileParkIneligibleDerivation = "ineligible_derivation"
	// reconcileParkBackdatedOutsideWindow marks a resumed-scan event whose
	// CreatedAt falls before the 30-day since-window: it is backdated beyond
	// retention. The resumed path cannot silently drop it (a full rescan would
	// never have seen it either, but a resumed sweep DID serve it), so it is
	// parked with this distinct reason instead.
	reconcileParkBackdatedOutsideWindow = "backdated_outside_window"
)

type reconcileDisposition string

const (
	reconcileDispositionDismiss    reconcileDisposition = "dismiss"
	reconcileDispositionReopen     reconcileDisposition = "reopen"
	reconcileDispositionRaise      reconcileDisposition = "raise"
	reconcileDispositionDecayRoute reconcileDisposition = "decay-route"
	reconcileDispositionContested  reconcileDisposition = "contested"
	reconcileDispositionPark       reconcileDisposition = "park"
)

var reconcileDispositions = []reconcileDisposition{
	reconcileDispositionDismiss,
	reconcileDispositionReopen,
	reconcileDispositionRaise,
	reconcileDispositionDecayRoute,
	reconcileDispositionContested,
	reconcileDispositionPark,
}

var reconcileActedDispositions = []reconcileDisposition{
	reconcileDispositionReopen,
	reconcileDispositionRaise,
	reconcileDispositionDecayRoute,
}

type SynthesisReconcileParkedReason struct {
	Reason   string   `json:"reason"`
	Count    int      `json:"count"`
	EventIDs []string `json:"event_ids,omitempty"`
}

type reconcileCheckpoint struct {
	SchemaVersion string `json:"schema_version"`
	UpdatedAt     string `json:"updated_at"`
	Since         string `json:"since"`
	LastEventID   string `json:"last_event_id,omitempty"`
	LastEventTime string `json:"last_event_time,omitempty"`
	// Cursor is the opaque eventstore Scan resume token (byte high-water per
	// segment). It advances atomically with the rest of the checkpoint, so a
	// crash before write leaves both it and LastEventTime unadvanced and the
	// next sweep re-scans. It is a cache: an empty/stale token converges to a
	// full rescan, never a skipped event — the LastEventTime/ID filter below is
	// the semantic authority for which events fold.
	Cursor              string                 `json:"scan_cursor,omitempty"`
	DecayRoutedRefs     []string               `json:"decay_routed_refs,omitempty"`
	LastProcessedReport SynthesisSweepReport   `json:"last_processed_report"`
	ParkedReasons       map[string][]string    `json:"parked_reasons,omitempty"`
	Counts              map[string]int         `json:"counts,omitempty"`
	Meta                map[string]interface{} `json:"meta,omitempty"`
}

type reconcileEvent struct {
	ID          string
	CreatedAt   time.Time
	Kind        string
	ScopeEntity string
	ClaimRefs   []string
	// Backdated is true when this event's CreatedAt is earlier than the
	// checkpoint's LastEventTime at the start of this sweep AND it was served
	// only because the scan resumed from a usable byte cursor (Resumed=true) —
	// on a full rescan the same event would have been filtered as a dup by the
	// LastEventTime/ID check. It exists purely for observability (the
	// backdated_folded count); it never changes disposition.
	Backdated bool
}

// reconcileSkippedEvent is a fold-relevant event the resumed scan served that
// loadReconcileEvents did not include in its output because it fell outside
// the retention window — the caller parks it under Reason rather than
// dropping it silently.
type reconcileSkippedEvent struct {
	Reason  string
	EventID string
}

func (s *Store) synthesisReconcile(report *SynthesisSweepReport, now time.Time) error {
	checkpoint, err := s.loadReconcileCheckpoint(now)
	if err != nil {
		return err
	}
	events, nextCursor, skippedEvents, err := s.loadReconcileEvents(checkpoint, now)
	if err != nil {
		return err
	}
	checkpoint.Cursor = nextCursor
	claims, err := s.loadClaims()
	if err != nil {
		return err
	}
	candidates, err := s.AllCandidates()
	if err != nil {
		return err
	}
	if report.ClaimCount == 0 {
		report.ClaimCount = len(claims)
	}
	if report.CandidateCount == 0 {
		report.CandidateCount = len(candidates)
	}

	positions, err := s.Positions()
	if err != nil {
		return err
	}
	activePositionClaims := map[string]bool{}
	for _, position := range positions {
		activePositionClaims[position.ActiveClaimID] = true
	}

	superseded, err := s.supersededAsOf(s.maxSeq(claims))
	if err != nil {
		return err
	}
	reviewMarks, err := s.claimReviewMarksAsOf(0)
	if err != nil {
		return err
	}
	claimsByID := claimsByID(claims)
	currentByCluster, err := s.currentClaimsByCluster(claims, superseded)
	if err != nil {
		return err
	}
	parked := newReconcileParkedReasons()
	for _, skip := range skippedEvents {
		parked.add(skip.Reason, skip.EventID)
	}
	routedRefs := map[string]bool{}
	for _, ref := range checkpoint.DecayRoutedRefs {
		routedRefs[ref] = true
	}

	var proposals []SynthesisContradictionProposal
	reopenClaims := map[string]Claim{}
	reopenEvidence := map[string]Candidate{}
	contested := newReconcileContestedGroups()
	protectedFromClear := map[string]bool{}
	for claimID := range reviewMarks {
		protectedFromClear[claimID] = true
	}
	recentClaimIDs := map[string]bool{}
	// lastEvent tracks the high-water event for the checkpoint, seeded from
	// the PRE-sweep checkpoint value (not just the last item in `events`): a
	// resumed sweep can serve a backdated event as the only new entry, and
	// blindly taking "the last event processed" would regress LastEventTime
	// backward, corrupting the full-rescan path's dedup authority (F2).
	var lastEvent *reconcileEvent
	if checkpoint.LastEventTime != "" {
		if t, err := time.Parse(time.RFC3339, checkpoint.LastEventTime); err == nil {
			lastEvent = &reconcileEvent{ID: checkpoint.LastEventID, CreatedAt: t}
		}
	}
	for i := range events {
		event := events[i]
		if event.Backdated {
			report.ReconcileBackdatedFoldedCount++
		}
		if lastEvent == nil || event.CreatedAt.After(lastEvent.CreatedAt) ||
			(event.CreatedAt.Equal(lastEvent.CreatedAt) && event.ID > lastEvent.ID) {
			lastEvent = &event
		}
		if event.Kind == "receipt" {
			for _, claimID := range event.ClaimRefs {
				if _, ok := claimsByID[claimID]; !ok {
					parked.add(reconcileParkUnlinkedClaim, event.ID)
					continue
				}
				ref := "receipt:" + event.ID + ":claim:" + claimID
				if routedRefs[ref] {
					continue
				}
				routedRefs[ref] = true
				report.ReconcileDecayRoutedCount++
			}
			continue
		}
		for _, claimID := range event.ClaimRefs {
			evidence, ok := claimsByID[claimID]
			if !ok {
				parked.add(reconcileParkUnlinkedClaim, event.ID)
				continue
			}
			recentClaimIDs[claimID] = true
			disposition, proposal, synthetic, reason, err := s.reconcileEvidenceDisposition(event, evidence, currentByCluster, superseded, reviewMarks, activePositionClaims)
			if err != nil {
				return err
			}
			switch disposition {
			case reconcileDispositionDismiss:
				report.ReconcileDismissedCount++
			case reconcileDispositionPark:
				parked.add(reason, event.ID)
			case reconcileDispositionReopen:
				proposals = append(proposals, proposal)
				for _, id := range proposal.ClaimIDs {
					if claim, ok := claimsByID[id]; ok {
						reopenClaims[id] = claim
						protectedFromClear[id] = true
					}
				}
				if synthetic.ID != "" {
					reopenEvidence[synthetic.ID] = synthetic
				}
			case reconcileDispositionContested:
				contested.add(proposal, claimsByID, evidence.ID)
			}
		}
	}

	if len(proposals) > 0 {
		reopened, err := s.reopenClaimsForContradictions(proposals, reopenClaims, reopenEvidence, protectedFromClear, now)
		if err != nil {
			return err
		}
		report.ReopenedClaims = append(report.ReopenedClaims, reopened...)
		report.ReconcileReopenedCount += len(reopened)
		report.ReviewDebtCount += len(reopened)
	}
	if contested.len() > 0 {
		cards, err := s.writeReconcileContestedCards(contested, reviewMarks, now)
		if err != nil {
			return err
		}
		report.ReconcileContestedCards = append(report.ReconcileContestedCards, cards...)
		report.ReconcileContestedCount += len(cards)
		report.ReviewDebtCount += len(cards)
	}

	recentClaims := filterClaimsByID(claims, recentClaimIDs)
	if len(recentClaims) > 0 {
		_, raised, err := s.synthesisCorroborations(recentClaims, candidates, now)
		if err != nil {
			return err
		}
		report.RaisedSeeds = append(report.RaisedSeeds, raised...)
		report.ReconcileRaisedCount += len(raised)
		report.ReviewDebtCount += len(raised)
	}

	for _, candidate := range candidates {
		if candidate.State != "parked" || !candidateExpired(candidate, now) {
			continue
		}
		ref := "candidate:" + candidate.ID
		if routedRefs[ref] {
			continue
		}
		routedRefs[ref] = true
		report.ReconcileDecayRoutedCount++
	}

	report.ReconcileParkedReasons = parked.report()
	for _, reason := range report.ReconcileParkedReasons {
		report.ReconcileParkedCount += reason.Count
	}
	checkpoint.DecayRoutedRefs = sortedKeysBool(routedRefs)
	if lastEvent != nil {
		checkpoint.LastEventID = lastEvent.ID
		checkpoint.LastEventTime = rfc3339(lastEvent.CreatedAt)
	}
	return s.writeReconcileCheckpoint(checkpoint, *report, now)
}

func (s *Store) reconcileEvidenceDisposition(event reconcileEvent, evidence Claim, currentByCluster map[string][]Claim, superseded map[string]bool, marks map[string]claimReviewMark, activePositionClaims map[string]bool) (reconcileDisposition, SynthesisContradictionProposal, Candidate, string, error) {
	if !sameEntityScope(event.ScopeEntity, evidence.Entity) {
		return reconcileDispositionDismiss, SynthesisContradictionProposal{}, Candidate{}, "", nil
	}
	if superseded[evidence.ID] || evidence.Visibility == "retired" {
		return reconcileDispositionDismiss, SynthesisContradictionProposal{}, Candidate{}, "", nil
	}
	if evidence.Confidence < reconcileConfidenceFloor {
		return reconcileDispositionPark, SynthesisContradictionProposal{}, Candidate{}, reconcileParkLowConfidence, nil
	}
	if !reconcileLinkableDerivation(evidence.Derivation) {
		return reconcileDispositionPark, SynthesisContradictionProposal{}, Candidate{}, reconcileParkIneligibleDerivation, nil
	}
	clusterKey, err := s.claimClusterKey(evidence)
	if err != nil {
		return "", SynthesisContradictionProposal{}, Candidate{}, "", err
	}
	if strings.TrimSpace(clusterKey) == "" {
		return reconcileDispositionPark, SynthesisContradictionProposal{}, Candidate{}, reconcileParkAmbiguous, nil
	}
	targets := currentByCluster[clusterKey]
	var different []Claim
	objects := map[string]bool{NormalizeClaimValue(evidence.Object): true}
	for _, target := range targets {
		if target.ID == evidence.ID {
			continue
		}
		if SameClaimObject(target.Object, evidence.Object) {
			continue
		}
		different = append(different, target)
		objects[NormalizeClaimValue(target.Object)] = true
	}
	if len(different) == 0 {
		if activePositionClaims[evidence.ID] || len(targets) <= 1 {
			return reconcileDispositionDismiss, SynthesisContradictionProposal{}, Candidate{}, "", nil
		}
		return reconcileDispositionDismiss, SynthesisContradictionProposal{}, Candidate{}, "", nil
	}
	evidenceAt, ok := parseReconcileClaimDate(evidence)
	if !ok {
		return reconcileDispositionPark, SynthesisContradictionProposal{}, Candidate{}, reconcileParkMissingDate, nil
	}
	var eligible []Claim
	for _, target := range different {
		targetAt, ok := parseReconcileClaimDate(target)
		if !ok {
			return reconcileDispositionPark, SynthesisContradictionProposal{}, Candidate{}, reconcileParkMissingDate, nil
		}
		if !evidenceAt.After(targetAt) {
			continue
		}
		if _, marked := marks[target.ID]; marked {
			continue
		}
		eligible = append(eligible, target)
	}
	if len(eligible) == 0 {
		if canOpenContestedReconcileCard(evidence, different, marks) {
			return reconcileDispositionContested, contestedReconcileProposal(clusterKey, evidence, different, objects), Candidate{}, "", nil
		}
		return reconcileDispositionDismiss, SynthesisContradictionProposal{}, Candidate{}, "", nil
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Seq == eligible[j].Seq {
			return eligible[i].ID < eligible[j].ID
		}
		return eligible[i].Seq < eligible[j].Seq
	})
	claimIDs := make([]string, 0, len(eligible))
	for _, claim := range eligible {
		claimIDs = append(claimIDs, claim.ID)
	}
	objectList := sortedKeysBool(objects)
	syntheticID := "claim:" + evidence.ID
	proposal := SynthesisContradictionProposal{
		ID:           idFor("synth_reconcile_contradiction_", clusterKey, strings.Join(claimIDs, ","), syntheticID, strings.Join(objectList, ",")),
		ClusterKey:   clusterKey,
		ClaimIDs:     claimIDs,
		CandidateIDs: []string{syntheticID},
		Objects:      objectList,
	}
	synthetic := Candidate{ID: syntheticID, Object: evidence.Object, Confidence: evidence.Confidence}
	return reconcileDispositionReopen, proposal, synthetic, "", nil
}

func reconcileLinkableDerivation(derivation string) bool {
	switch strings.TrimSpace(derivation) {
	case "deterministic", "extracted":
		return true
	default:
		return false
	}
}

func sameEntityScope(scopeEntity, entity string) bool {
	scopeEntity = strings.TrimSpace(scopeEntity)
	if scopeEntity == "" {
		return true
	}
	return NormalizeClaimValue(scopeEntity) == NormalizeClaimValue(entity)
}

func parseReconcileClaimDate(claim Claim) (time.Time, bool) {
	raw := strings.TrimSpace(claim.ValidFrom)
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	return t, err == nil
}

func claimsByID(claims []Claim) map[string]Claim {
	out := make(map[string]Claim, len(claims))
	for _, claim := range claims {
		out[claim.ID] = claim
	}
	return out
}

func filterClaimsByID(claims []Claim, ids map[string]bool) []Claim {
	out := make([]Claim, 0, len(ids))
	for _, claim := range claims {
		if ids[claim.ID] {
			out = append(out, claim)
		}
	}
	return out
}

func (s *Store) currentClaimsByCluster(claims []Claim, superseded map[string]bool) (map[string][]Claim, error) {
	out := map[string][]Claim{}
	for _, claim := range claims {
		if superseded[claim.ID] || claim.Visibility == "retired" {
			continue
		}
		key, err := s.claimClusterKey(claim)
		if err != nil {
			return nil, err
		}
		out[key] = append(out[key], claim)
	}
	for key := range out {
		sort.Slice(out[key], func(i, j int) bool {
			if out[key][i].Seq == out[key][j].Seq {
				return out[key][i].ID < out[key][j].ID
			}
			return out[key][i].Seq < out[key][j].Seq
		})
	}
	return out, nil
}

// loadReconcileEvents folds INCREMENTALLY: it resumes the eventstore Scan from
// the checkpoint's byte cursor so a second consecutive sweep decodes only frames
// appended since the last one, instead of re-parsing the whole event history
// every invocation (the month-two re-parse creep the write-shape spec calls
// out). The returned next cursor is persisted by the caller.
//
// The real invariant (the byte cursor is NOT a superset the filter narrows
// identically on every path): a resumed scan (page.Resumed) returns exactly
// the frames appended since the cursor, by byte-offset construction — every
// one of them is new, including a backdated append (created_at earlier than
// checkpoint.LastEventTime, e.g. a migration/import writing historical
// records past an already-consumed cursor position). The store enforces this:
// a decodable-but-stale cursor whose offsets force a whole-segment reparse (a
// page that CAN re-serve consumed frames) comes back Resumed=false, so the
// filter-skip below can never double-fold re-served events. Applying the
// LastEventTime/ID filter on THAT set would wrongly discard a backdated event
// exactly because it looks like a dup by timestamp, permanently losing it
// (the cursor has already moved past its bytes, so no future sweep re-serves
// it). So the filter is applied ONLY when !page.Resumed (a full rescan, where
// every already-folded event really is decoded again and the filter is the
// dedup authority). On the resumed path the filter is skipped entirely, and a
// backdated-but-in-window event is tagged Backdated for the caller's
// observability counter. A fold-relevant event on the resumed path older than
// the 30-day since-window is not folded either way, but the caller parks it
// (reconcileParkBackdatedOutsideWindow) instead of a silent continue, since a
// resumed sweep — unlike a fresh full rescan — actually saw it go by.
func (s *Store) loadReconcileEvents(checkpoint reconcileCheckpoint, now time.Time) ([]reconcileEvent, string, []reconcileSkippedEvent, error) {
	page, err := s.eventStore.ScanFrom(eventstore.ScanOptions{Cursor: checkpoint.Cursor})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, checkpoint.Cursor, nil, nil
		}
		return nil, "", nil, err
	}
	entries := page.Entries
	since := now.AddDate(0, 0, -30)
	if checkpoint.Since != "" {
		if t, err := time.Parse(time.RFC3339, checkpoint.Since); err == nil {
			since = t
		}
	}
	lastTime, hasLast := time.Time{}, false
	if checkpoint.LastEventTime != "" {
		if t, err := time.Parse(time.RFC3339, checkpoint.LastEventTime); err == nil {
			lastTime, hasLast = t, true
		}
	}

	var out []reconcileEvent
	var skipped []reconcileSkippedEvent
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			return nil, "", nil, err
		}
		event, ok, err := readReconcileEvent(entry.Name, record.Bytes)
		if err != nil {
			return nil, "", nil, err
		}
		if !ok {
			continue
		}
		if event.CreatedAt.Before(since) {
			if page.Resumed {
				skipped = append(skipped, reconcileSkippedEvent{Reason: reconcileParkBackdatedOutsideWindow, EventID: event.ID})
			}
			continue
		}
		if hasLast && !page.Resumed {
			if event.CreatedAt.Before(lastTime) || (event.CreatedAt.Equal(lastTime) && event.ID <= checkpoint.LastEventID) {
				continue
			}
		}
		if page.Resumed && hasLast && event.CreatedAt.Before(lastTime) {
			event.Backdated = true
		}
		out = append(out, event)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, page.Cursor, skipped, nil
}

func readReconcileEvent(name string, data []byte) (reconcileEvent, bool, error) {
	kind, err := envelope.ValidateBytes(data)
	if err != nil {
		return reconcileEvent{}, false, fmt.Errorf("load event %s: %w", name, err)
	}
	switch kind {
	case "transition":
		var tr envelope.Transition
		if err := json.Unmarshal(data, &tr); err != nil {
			return reconcileEvent{}, false, err
		}
		refs := transitionMemoryClaimRefs(tr)
		if len(refs) == 0 {
			return reconcileEvent{}, false, nil
		}
		createdAt, err := time.Parse(time.RFC3339, tr.CreatedAt)
		if err != nil {
			return reconcileEvent{}, false, fmt.Errorf("transition %s created_at: %w", tr.ID, err)
		}
		return reconcileEvent{ID: tr.ID, CreatedAt: createdAt, Kind: "transition", ScopeEntity: tr.Scope.EntityID, ClaimRefs: refs}, true, nil
	case "receipt":
		var rec envelope.Receipt
		if err := json.Unmarshal(data, &rec); err != nil {
			return reconcileEvent{}, false, err
		}
		var refs []string
		for _, ref := range claimRefsFromUsed(rec.Skills.Used) {
			if rec.Skills.ApplicationObserved(ref) {
				refs = append(refs, ref)
			}
		}
		if len(refs) == 0 {
			return reconcileEvent{}, false, nil
		}
		createdAt, err := time.Parse(time.RFC3339, rec.CreatedAt)
		if err != nil {
			return reconcileEvent{}, false, fmt.Errorf("receipt %s created_at: %w", rec.ID, err)
		}
		return reconcileEvent{ID: rec.ID, CreatedAt: createdAt, Kind: "receipt", ScopeEntity: rec.Scope.EntityID, ClaimRefs: refs}, true, nil
	default:
		return reconcileEvent{}, false, nil
	}
}

func transitionMemoryClaimRefs(tr envelope.Transition) []string {
	var refs []string
	refs = append(refs, tr.Projections.Memory...)
	for _, ref := range tr.AfterRefs {
		refs = append(refs, normalizeReconcileClaimRef(ref))
	}
	for _, ref := range tr.EvidenceRefs {
		refs = append(refs, normalizeReconcileClaimRef(ref))
	}
	return uniqueNonEmptyClaimRefs(refs)
}

func claimRefsFromUsed(values []string) []string {
	refs := make([]string, 0, len(values))
	for _, value := range values {
		refs = append(refs, normalizeReconcileClaimRef(value))
	}
	return uniqueNonEmptyClaimRefs(refs)
}

func normalizeReconcileClaimRef(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "claim://")
	ref = strings.TrimPrefix(ref, "claim:")
	if !strings.HasPrefix(ref, "claim_") {
		return ""
	}
	return ref
}

func uniqueNonEmptyClaimRefs(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = normalizeReconcileClaimRef(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *Store) reconcileDir() string {
	return filepath.Join(s.home, "reviews", "reconcile")
}

func (s *Store) reconcileCheckpointPath() string {
	return filepath.Join(s.reconcileDir(), "checkpoint.json")
}

func (s *Store) loadReconcileCheckpoint(now time.Time) (reconcileCheckpoint, error) {
	checkpoint := reconcileCheckpoint{
		SchemaVersion: reconcileCheckpointSchema,
		Since:         rfc3339(now.AddDate(0, 0, -30)),
	}
	if err := readJSON(s.reconcileCheckpointPath(), &checkpoint); err != nil {
		if strings.HasPrefix(err.Error(), "not found:") {
			return checkpoint, nil
		}
		return checkpoint, err
	}
	if checkpoint.SchemaVersion == "" {
		checkpoint.SchemaVersion = reconcileCheckpointSchema
	}
	if checkpoint.Since == "" {
		checkpoint.Since = rfc3339(now.AddDate(0, 0, -30))
	}
	return checkpoint, nil
}

func (s *Store) writeReconcileCheckpoint(checkpoint reconcileCheckpoint, report SynthesisSweepReport, now time.Time) error {
	if err := os.MkdirAll(s.reconcileDir(), 0o755); err != nil {
		return err
	}
	checkpoint.SchemaVersion = reconcileCheckpointSchema
	checkpoint.UpdatedAt = rfc3339(now)
	checkpoint.LastProcessedReport = report
	checkpoint.Counts = map[string]int{
		string(reconcileDispositionDismiss):    report.ReconcileDismissedCount,
		string(reconcileDispositionReopen):     report.ReconcileReopenedCount,
		string(reconcileDispositionRaise):      report.ReconcileRaisedCount,
		string(reconcileDispositionDecayRoute): report.ReconcileDecayRoutedCount,
		string(reconcileDispositionContested):  report.ReconcileContestedCount,
		string(reconcileDispositionPark):       report.ReconcileParkedCount,
		"backdated_folded":                     report.ReconcileBackdatedFoldedCount,
	}
	checkpoint.ParkedReasons = map[string][]string{}
	for _, reason := range report.ReconcileParkedReasons {
		checkpoint.ParkedReasons[reason.Reason] = append([]string(nil), reason.EventIDs...)
	}
	return writeJSON(s.reconcileCheckpointPath(), checkpoint)
}

type reconcileParkedReasons struct {
	counts  map[string]int
	samples map[string][]string
}

func newReconcileParkedReasons() *reconcileParkedReasons {
	return &reconcileParkedReasons{
		counts:  map[string]int{},
		samples: map[string][]string{},
	}
}

func (r *reconcileParkedReasons) add(reason, eventID string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = reconcileParkAmbiguous
	}
	r.counts[reason]++
	if len(r.samples[reason]) < reconcileParkSampleLimit && strings.TrimSpace(eventID) != "" {
		r.samples[reason] = append(r.samples[reason], eventID)
	}
}

func (r *reconcileParkedReasons) report() []SynthesisReconcileParkedReason {
	reasons := sortedKeys(r.counts)
	out := make([]SynthesisReconcileParkedReason, 0, len(reasons))
	for _, reason := range reasons {
		out = append(out, SynthesisReconcileParkedReason{
			Reason:   reason,
			Count:    r.counts[reason],
			EventIDs: append([]string(nil), r.samples[reason]...),
		})
	}
	return out
}
