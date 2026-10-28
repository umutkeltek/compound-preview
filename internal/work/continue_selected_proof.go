package work

import (
	"errors"
	"strings"

	"compound/internal/envelope"
	"compound/internal/runattempt"
)

// The existing finalizer records transcripts in VerificationIDs as execution
// evidence. Selected Continue must not silently upgrade those to acceptance.
// An executor/verifier must record a separate check through the existing work
// owner; the unchanged close/proof/policy gates still decide whether to close.
func (s *Store) selectedVerificationReady(w envelope.WorkEnvelope) error {
	transcripts := map[string]bool{}
	var latest runattempt.RunAttempt
	for _, id := range w.RunAttemptIDs {
		attempt, err := runattempt.Open(s.home).Get(id)
		if err != nil {
			return errors.New("attempt evidence is unreadable; verification remains unresolved")
		}
		if attempt.TranscriptRef != "" {
			transcripts[attempt.TranscriptRef] = true
		}
		if attempt.Origin == runattempt.OriginHarnessExecution {
			latest = attempt
		}
	}
	if latest.ID == "" || latest.WorkID != w.ID || latest.Status != runattempt.StatusSucceeded {
		return errors.New("the latest selected execution has not succeeded; prior success or proof cannot accept this attempt")
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		return errors.New("verification event provenance is unreadable")
	}
	finalized := -1
	linked := false
	for i, event := range events {
		if event.From != "running" || event.To != "verifying" {
			continue
		}
		finalized, linked = i, false
		for _, ref := range event.Attached {
			if ref == latest.ID {
				linked = true
			}
		}
	}
	if finalized < 0 || !linked {
		return errors.New("latest execution has no linked verifying finalization")
	}
	checks := map[string]bool{}
	for _, ref := range w.VerificationIDs {
		if strings.TrimSpace(ref) != "" && !transcripts[ref] {
			checks[ref] = true
		}
	}
	// Reattaching an earlier check is not a fresh observation. Callers must
	// supply a new check invocation/revision after a resumed execution.
	for _, event := range events[:finalized+1] {
		for _, ref := range event.Attached {
			delete(checks, ref)
		}
	}
	// Event order, not wall time, binds the check to a later generation. The
	// event's source/work digests and fresh action/policy retain provenance;
	// a cumulative check from before this attempt cannot be reused silently.
	for _, event := range events[finalized+1:] {
		if event.From != "verifying" || event.To != "verifying" {
			continue
		}
		for _, ref := range event.Attached {
			if checks[ref] {
				return nil
			}
		}
	}
	return errors.New("a separate verification check after the latest execution is required; prior checks and captured transcripts are not current acceptance")
}
