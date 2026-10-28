// Package work — reseal.go is the governed repair door for terminal work
// envelopes whose latest recorded custody seal no longer describes their
// bytes. It exists because of the 2026-08-28 sweep-stale incident: a sweep
// pass reclaimed 71 envelopes before e5ecfd35 taught sweep-stale to re-seal
// what it mutates, leaving 71 terminal envelopes whose newest recorded
// work_sha256 still describes the PRE-sweep bytes. The autonomy director
// verifies terminal work by re-hashing the envelope and comparing against the
// most recent recorded seal, so those envelopes permanently failed its
// custody check. Prevention (the sweep fix) cannot repair them; this verb is
// the auditable repair.
//
// Design contract:
//   - A candidate is a TERMINAL envelope (completed/failed/cancelled) with at
//     least one terminal lifecycle event whose LATEST recorded work_sha256
//     (over all its events) differs from the canonical hash of the envelope
//     on disk — the exact comparison the director performs. A missing seal
//     counts as disagreement: no recorded seal covers the current bytes.
//   - Non-terminal envelopes are never candidates: a live writer owns them
//     and may legitimately re-seal through the ordinary doors.
//   - Repair NEVER rewrites envelope bytes or history. It appends exactly one
//     same-state lifecycle event (from == to) carrying the current envelope
//     hash as work_sha256 and the stale seal as previous_work_sha256. A
//     same-state row is an annotation, not a second terminal transition, so
//     "exactly one terminal event" invariants are untouched — the same shape
//     delegate steer/exit notes already use.
//   - Dry-run is the default; --yes applies. Diagnosis and repair are CAS'd:
//     inside the work-event lock the envelope is re-read and re-hashed and
//     the seal recomputed, so a concurrent legitimate re-seal is detected and
//     skipped rather than double-appended.
package work

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	homepkg "compound/internal/home"
)

// ResealResealActorDefault is the audit actor recorded on the repair event
// when the caller supplies none.
const ResealActorDefault = "work-reseal"

// ResealCandidate is one terminal envelope whose recorded seal disagrees with
// its bytes. StoredWorkSHA256 is the most recent non-empty work_sha256 across
// the envelope's lifecycle events (empty when no event ever sealed one);
// ComputedWorkSHA256 is the canonical hash of the envelope on disk — the
// value the director compares the stored seal against.
type ResealCandidate struct {
	WorkID             string `json:"work_id"`
	State              string `json:"state"`
	Location           string `json:"location"` // "hot" (work/work) or "archive" (operational archive)
	Path               string `json:"path"`     // home-relative slash path of the envelope file
	StoredWorkSHA256   string `json:"stored_work_sha256,omitempty"`
	ComputedWorkSHA256 string `json:"computed_work_sha256"`
	Resealed           bool   `json:"resealed"`
	ResealError        string `json:"reseal_error,omitempty"`
}

// ResealReport is the operator-visible outcome of one reseal pass. Scanned
// counts every readable terminal envelope examined; Mismatched the subset
// whose seal disagrees; Resealed the subset actually repaired by --yes.
type ResealReport struct {
	Scanned       int               `json:"scanned"`
	Mismatched    int               `json:"mismatched"`
	Resealed      int               `json:"resealed"`
	Skipped       int               `json:"skipped"`
	Unreadable    int               `json:"unreadable,omitempty"`
	Candidates    []ResealCandidate `json:"candidates"`
	SkippedIDs    []string          `json:"skipped_ids,omitempty"`
	UnreadableIDs []string          `json:"unreadable_ids,omitempty"`
}

// resealTerminalStates mirrors the director's terminalWorkState: only
// terminal envelopes are custody-relevant and only they are repairable here.
func resealTerminalState(state string) bool {
	switch strings.TrimSpace(state) {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// latestRecordedSeal walks the work's lifecycle events in ledger order and
// returns the most recent non-empty work_sha256 — the exact "latest seal"
// rule the autonomy director applies (see receiptEvidenceByWork in
// internal/routeevidence/autonomy). Also reports whether any event is a
// transition INTO a terminal state (from != to), which the director requires
// exactly one of.
func latestRecordedSeal(events []Event) (seal string, hasTerminalEvent bool) {
	for _, event := range events {
		if h := strings.TrimSpace(event.WorkSHA256); h != "" {
			seal = h
		}
		if resealTerminalState(event.To) && strings.TrimSpace(event.From) != strings.TrimSpace(event.To) {
			hasTerminalEvent = true
		}
	}
	return seal, hasTerminalEvent
}

// resealEnvelopeLocations enumerates every envelope the director can verify:
// hot work/work/*.json plus the operational archive's indexed work entries.
// The director's loadWorks walks the same two sets; repairing a subset would
// leave the pass red.
func (s *Store) resealEnvelopeLocations() ([]ResealCandidate, []string, error) {
	locations := []ResealCandidate{}
	unreadable := []string{}
	hotDir := filepath.Join(s.home, "work", "work")
	entries, err := os.ReadDir(hotDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		locations = append(locations, ResealCandidate{
			WorkID:   strings.TrimSuffix(entry.Name(), ".json"),
			Location: "hot",
			Path:     "work/work/" + entry.Name(),
		})
	}
	archiveIndex, err := homepkg.LoadOperationalArchiveIndex(s.home)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range archiveIndex {
		if entry.Kind != "work" {
			continue
		}
		rel := strings.TrimSpace(entry.ArchivePath)
		if rel == "" || !strings.HasSuffix(filepath.Base(filepath.FromSlash(rel)), ".json") {
			unreadable = append(unreadable, entry.ID)
			continue
		}
		locations = append(locations, ResealCandidate{
			WorkID:   strings.TrimSuffix(filepath.Base(filepath.FromSlash(rel)), ".json"),
			Location: "archive",
			Path:     rel,
		})
	}
	sort.Slice(locations, func(i, j int) bool { return locations[i].WorkID < locations[j].WorkID })
	return locations, unreadable, nil
}

// resealReadEnvelope reads one enumerated envelope from its location and
// returns the decoded envelope plus its canonical hash. The path must be a
// home-relative slash path; it is re-rooted absolutely because
// ReadRootedRegularFile resolves non-absolute paths against process CWD.
func (s *Store) resealReadEnvelope(candidate ResealCandidate) (envelope.WorkEnvelope, string, error) {
	rel := strings.TrimSpace(candidate.Path)
	if rel == "" || filepath.IsAbs(rel) || strings.ContainsAny(rel, "\x00") {
		return envelope.WorkEnvelope{}, "", fmt.Errorf("work %s: invalid envelope path %q", candidate.WorkID, rel)
	}
	body, err := homepkg.ReadRootedRegularFile(s.home, filepath.Join(s.home, filepath.FromSlash(rel)), "reseal envelope")
	if err != nil {
		return envelope.WorkEnvelope{}, "", err
	}
	var work envelope.WorkEnvelope
	if err := json.Unmarshal(body, &work); err != nil {
		return envelope.WorkEnvelope{}, "", fmt.Errorf("decode work %s: %w", candidate.WorkID, err)
	}
	if strings.TrimSpace(work.ID) == "" {
		work.ID = candidate.WorkID
	}
	if work.ID != candidate.WorkID {
		return envelope.WorkEnvelope{}, "", fmt.Errorf("work %s: envelope identity %q does not match location", candidate.WorkID, work.ID)
	}
	hash, err := workEnvelopeSHA256(work)
	if err != nil {
		return envelope.WorkEnvelope{}, "", err
	}
	return work, hash, nil
}

// ResealPlan diagnoses every seal mismatch and returns the operator-facing
// report WITHOUT mutating anything. --dry-run (the default) calls this.
func (s *Store) ResealPlan(actor string, now time.Time) (ResealReport, error) {
	return s.resealEvaluate(false, actor, now)
}

// ResealApply diagnoses every seal mismatch and appends the repair seal.
// --yes calls this.
func (s *Store) ResealApply(actor string, now time.Time) (ResealReport, error) {
	return s.resealEvaluate(true, actor, now)
}

func (s *Store) resealEvaluate(apply bool, actor string, now time.Time) (ResealReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if strings.TrimSpace(actor) == "" {
		actor = ResealActorDefault
	}
	rep := ResealReport{Candidates: []ResealCandidate{}}
	eventsByWork, err := s.eventsByWorkID()
	if err != nil {
		return rep, fmt.Errorf("reseal: read work events: %w", err)
	}
	locations, archiveUnreadable, err := s.resealEnvelopeLocations()
	if err != nil {
		return rep, fmt.Errorf("reseal: enumerate work envelopes: %w", err)
	}
	rep.UnreadableIDs = archiveUnreadable
	for _, location := range locations {
		if strings.HasPrefix(location.WorkID, "work_live_") {
			// Legacy ephemeral records are excluded from director custody;
			// sealing them would assert custody that cannot be verified.
			continue
		}
		work, computed, err := s.resealReadEnvelope(location)
		if err != nil {
			rep.Unreadable++
			rep.UnreadableIDs = append(rep.UnreadableIDs, location.WorkID)
			continue
		}
		if !resealTerminalState(work.State) {
			continue
		}
		rep.Scanned++
		stored, hasTerminalEvent := latestRecordedSeal(eventsByWork[work.ID])
		if !hasTerminalEvent {
			// No terminal transition at all is a different defect (and the
			// director reports it separately); resealing cannot invent one.
			continue
		}
		if stored == computed {
			continue
		}
		candidate := location
		candidate.State = strings.TrimSpace(work.State)
		candidate.StoredWorkSHA256 = stored
		candidate.ComputedWorkSHA256 = computed
		rep.Mismatched++
		if !apply {
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, work.ID)
			rep.Candidates = append(rep.Candidates, candidate)
			continue
		}
		resealed, resealErr := s.resealAppendSeal(candidate, actor, now)
		candidate.Resealed = resealed
		candidate.ResealError = ""
		if resealErr != nil {
			candidate.ResealError = resealErr.Error()
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, work.ID)
		} else if resealed {
			rep.Resealed++
		} else {
			// A concurrent writer re-sealed between diagnosis and repair;
			// the mismatch is already gone, so nothing to do.
			rep.Skipped++
			rep.SkippedIDs = append(rep.SkippedIDs, work.ID)
		}
		rep.Candidates = append(rep.Candidates, candidate)
	}
	return rep, nil
}

// resealAppendSeal appends the repair event for one diagnosed candidate
// inside the work-event lock, re-checking the diagnosis against the current
// bytes and ledger so a concurrent repair or legitimate re-seal is never
// double-applied. Returns resealed=false, nil when the mismatch already
// resolved itself.
func (s *Store) resealAppendSeal(candidate ResealCandidate, actor string, now time.Time) (bool, error) {
	resealed := false
	err := s.withWorkEventLock(func() error {
		work, computed, err := s.resealReadEnvelope(candidate)
		if err != nil {
			return err
		}
		if !resealTerminalState(work.State) || work.State != candidate.State {
			return fmt.Errorf("work %s state changed from %q to %q between reseal diagnosis and repair; refusing (next pass re-diagnoses)", work.ID, candidate.State, work.State)
		}
		events, err := s.readEventsStrictUnlocked(work.ID)
		if err != nil {
			return err
		}
		stored, hasTerminalEvent := latestRecordedSeal(events)
		if !hasTerminalEvent {
			return fmt.Errorf("work %s lost its terminal event between reseal diagnosis and repair", work.ID)
		}
		if stored == computed {
			// Already sealed with the current bytes by a concurrent writer.
			return nil
		}
		event := Event{
			TS:                 ts(now),
			WorkID:             work.ID,
			From:               strings.TrimSpace(work.State),
			To:                 strings.TrimSpace(work.State),
			By:                 actor,
			Note:               "reseal: recorded custody seal did not match envelope bytes; appended governed repair seal",
			PreviousWorkSHA256: stored,
			WorkSHA256:         computed,
		}
		if err := s.appendEventUnlocked(event); err != nil {
			return fmt.Errorf("append reseal event for %s: %w", work.ID, err)
		}
		resealed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return resealed, nil
}
