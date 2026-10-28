package work

import (
	"errors"
	"fmt"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

// This is the existing launch owner's exact content-free start_error shape,
// not a caller assertion that a failed attempt probably did nothing.
func runAttemptRefusedStartBeforeExecution(ra runattempt.RunAttempt) bool {
	return ra.Status == runattempt.StatusFailed && !ra.CompletedAt.IsZero() &&
		ra.Origin == runattempt.OriginHarnessExecution && ra.Launch != nil &&
		ra.Launch.PreparedAt != nil && ra.Launch.StartErrorAt != nil &&
		ra.Launch.CommittedAt == nil && ra.Launch.PreparedAt.Equal(*ra.Launch.StartErrorAt)
}

func runAttemptHasUncertainLaunch(ra runattempt.RunAttempt) bool {
	return ra.Launch != nil && (!runattempt.IsTerminal(ra.Status) ||
		(ra.Launch.CommittedAt == nil && !runAttemptRefusedStartBeforeExecution(ra)))
}

// A doctor pass takes one census, rather than scanning all attempts per work.
// Linked unreadable records retain their existing precise proof findings;
// unreadable unlisted records leave membership unknown and fail the census.
func (s *Store) proofRunAttemptCensus(works []envelope.WorkEnvelope, report *ProofChainDoctorReport) (map[string][]runattempt.RunAttempt, error) {
	byWork := map[string][]runattempt.RunAttempt{}
	attempts, err := s.runAttempts().List()
	if err != nil {
		var unreadable *runattempt.UnreadableRunAttemptsError
		if !errors.As(err, &unreadable) {
			return nil, fmt.Errorf("proof runattempt census: %w", err)
		}
		linked := map[string]bool{}
		for _, w := range works {
			if !terminalProofRequired(w.State) {
				continue
			}
			for _, id := range w.RunAttemptIDs {
				linked[id] = true
			}
		}
		for _, issue := range unreadable.Issues {
			if !linked[issue.ID] {
				report.Counts.InvalidRunAttempt++
				report.Findings = append(report.Findings, ProofChainDoctorFinding{Code: "run_attempt_census_incomplete", Message: fmt.Sprintf("unlisted runattempt %s is unreadable; work membership cannot be certified: %v", issue.ID, issue.Cause)})
			}
		}
	}
	for _, ra := range attempts {
		byWork[ra.WorkID] = append(byWork[ra.WorkID], ra)
	}
	return byWork, nil
}

func addReverseRunAttemptFindings(w envelope.WorkEnvelope, attempts []runattempt.RunAttempt, item *ProofChainDoctorItem, report *ProofChainDoctorReport) {
	for _, ra := range attempts {
		if attachedHas(w.RunAttemptIDs, ra.ID) {
			continue
		}
		report.Counts.InvalidRunAttempt++
		item.Problems = append(item.Problems, "unlisted_run_attempt:"+ra.ID)
		report.Findings = append(report.Findings, ProofChainDoctorFinding{WorkID: w.ID, Code: "unlisted_run_attempt", Message: fmt.Sprintf("terminal work omitted same-work runattempt %s (%s); reconcile execution history", ra.ID, ra.Status)})
	}
}

// closeRunAttemptCensus discovers reverse links in the existing attempt store.
// It only assembles the prospective close generation; it never repairs an
// earlier attachment by changing the currently authoritative WorkEnvelope.
func (s *Store) closeRunAttemptCensus(w envelope.WorkEnvelope) ([]string, error) {
	attempts, err := s.runAttempts().ForWork(w.ID)
	if err != nil {
		// An unreadable record has uncertain membership; a partial enumeration
		// cannot certify that every same-work execution was considered.
		return nil, fmt.Errorf("work %s runattempt census incomplete: %w", w.ID, err)
	}
	ids := append([]string{}, w.RunAttemptIDs...)
	byID := make(map[string]runattempt.RunAttempt, len(attempts))
	for _, ra := range attempts {
		byID[ra.ID] = ra
	}
	for _, ra := range attempts {
		if ra.WorkID != w.ID {
			continue
		}
		if err := validateRunAttemptRecord(ra.ID, w, ra); err != nil {
			return nil, err
		}
		if runAttemptHasUncertainLaunch(ra) {
			return nil, fmt.Errorf("work %s runattempt %s has active or uncertain launch effects; reconcile execution before terminal certification", w.ID, ra.ID)
		}
		if !attachedHas(w.RunAttemptIDs, ra.ID) && !runattempt.IsTerminal(ra.Status) &&
			ra.Origin != runattempt.OriginClaimReservation {
			return nil, fmt.Errorf("work %s reverse-only runattempt %s is nonterminal; reconcile execution before terminal certification", w.ID, ra.ID)
		}
		if attachedHas(ids, ra.ID) {
			continue
		}
		// Preserve existing forward order, inserting discovered older history
		// before later attachments so a reverse-only predecessor remains earlier
		// than its retry. The history validator still rejects invalid lineage.
		at := len(ids)
		for i, id := range ids {
			if existing, ok := byID[id]; ok && ra.CreatedAt.Before(existing.CreatedAt) {
				at = i
				break
			}
		}
		ids = append(ids, "")
		copy(ids[at+1:], ids[at:])
		ids[at] = ra.ID
	}
	return ids, nil
}
