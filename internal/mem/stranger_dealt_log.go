// stranger_dealt_log.go — work_c13acdc090b5 (S3b, D5): the weak-tie "stranger card" dealt-log.
//
// The dealer (cmd/compound/orient_operator.go, --operator path only) picks which un-promoted
// candidate to deal; this file is the shared, ledger-adjacent-never artifact both sides read
// and write: the dealer reads it to honor the 7-day re-deal cooldown (F7) and appends to it
// after a successful deal, and the S3a consumption report (cause_consumption.go) reads it to
// join dealt candidates against later Cause-transitions (D2/F8's dealt->caused kill-condition
// meter). Same write-class as the S3a report artifact: state/runs/, never the ledger.
package mem

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
)

// StrangerDealtLogRelPath is the home-relative path of the append-only JSONL dealt-log (D5).
const StrangerDealtLogRelPath = "state/runs/stranger-card-deals.jsonl"

// ErrStrangerDealPairedCommit distinguishes a cursor-side failure from a deal-log failure so
// the operator report can preserve its cursor error contract after compensating the append.
var ErrStrangerDealPairedCommit = errors.New("stranger deal paired commit failed")

// StrangerDealtRow is one dealt stranger card: ts is the deal time (RFC3339 UTC), card_id is
// the orientTensionCard.ID the operator saw, candidate_ref is the dealt Candidate.ID, and topic
// is that candidate's own Candidate.Topic() at deal time.
type StrangerDealtRow struct {
	Ts           string `json:"ts"`
	CardID       string `json:"card_id"`
	CandidateRef string `json:"candidate_ref"`
	Topic        string `json:"topic,omitempty"`
}

// StrangerDealCardSnapshot is the card-relevant candidate snapshot prepared from memory
// authority. The caller renders the optional orient card from these fields; it never reads a
// second, potentially newer candidate body after preparation.
type StrangerDealCardSnapshot struct {
	CandidateRef string
	Entity       string
	Predicate    string
	Object       string
	State        string
	Topic        string
}

// StrangerDealPreparation is the opaque authorization token passed back to the finalizer. Its
// fingerprint binds the rendered card to the candidate state and topic taxonomy observed during
// preparation; finalization rebuilds both under the memory lock and suppresses stale cards.
type StrangerDealPreparation struct {
	card         StrangerDealCardSnapshot
	excludeTopic string
	fingerprint  [sha256.Size]byte
}

// CardSnapshot returns a copy of the fields the orient surface may render.
func (p StrangerDealPreparation) CardSnapshot() StrangerDealCardSnapshot {
	return p.card
}

type strangerDealFingerprintPayload struct {
	ID                        string   `json:"id"`
	Entity                    string   `json:"entity"`
	Predicate                 string   `json:"predicate"`
	Object                    string   `json:"object"`
	State                     string   `json:"state"`
	Topic                     string   `json:"topic"`
	ExpiresAt                 string   `json:"expires_at"`
	RawTopicMemberships       []string `json:"raw_topic_memberships"`
	CanonicalTopicMemberships []string `json:"canonical_topic_memberships"`
}

func strangerDealtLogPath(homePath string) string {
	return filepath.Join(homePath, filepath.FromSlash(StrangerDealtLogRelPath))
}

// ReadStrangerDealtLog reads every dealt-card row ever appended. A log that does not exist yet
// (no stranger card has ever been dealt) returns an empty slice, nil error — the normal
// first-run case. A present-but-corrupt log (a line that fails to parse as StrangerDealtRow)
// returns a non-nil error so callers can fail closed: the dealer degrades to NO card + no crash
// (F7) rather than risk silently ignoring a row that might be holding an active cooldown, and
// the S3a report degrades to skipping the dealt->caused join for that run rather than bricking
// the whole consumption sweep.
func ReadStrangerDealtLog(homePath string) ([]StrangerDealtRow, error) {
	data, err := os.ReadFile(strangerDealtLogPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	out := make([]StrangerDealtRow, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row StrangerDealtRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		if err := validateStrangerDealtRow(row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// appendStrangerDealtRow is the raw append primitive used by FinalizeStrangerDeal while its
// Store lock is held, plus same-package fixtures that seed the consumption join.
func appendStrangerDealtRow(homePath string, row StrangerDealtRow) error {
	_, err := appendStrangerDealtRowReversible(homePath, row)
	return err
}

// appendStrangerDealtRowReversible appends one durable row and returns the compensating
// operation needed by FinalizeStrangerDeal when the caller's paired commit fails. Store's
// memory lock prevents another production deal append from interleaving before compensation.
func appendStrangerDealtRowReversible(homePath string, row StrangerDealtRow) (func() error, error) {
	if err := validateStrangerDealtRow(row); err != nil {
		return nil, err
	}
	path := strangerDealtLogPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	priorSize := int64(0)
	priorExists := false
	if info, err := os.Stat(path); err == nil {
		priorSize = info.Size()
		priorExists = true
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	b, err := json.Marshal(&row)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	payload := append(b, '\n')
	n, writeErr := f.Write(payload)
	syncErr := f.Sync()
	closeErr := f.Close()
	rollback := func() error {
		if priorExists {
			return os.Truncate(path, priorSize)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if writeErr != nil || n != len(payload) || syncErr != nil || closeErr != nil {
		writeFailure := writeErr
		switch {
		case writeFailure == nil && n != len(payload):
			writeFailure = fmt.Errorf("short write: wrote %d of %d bytes", n, len(payload))
		case writeFailure == nil && syncErr != nil:
			writeFailure = syncErr
		case writeFailure == nil:
			writeFailure = closeErr
		}
		if rollbackErr := rollback(); rollbackErr != nil {
			return nil, fmt.Errorf("append stranger deal: %w; rollback append: %v", writeFailure, rollbackErr)
		}
		return nil, writeFailure
	}
	return rollback, nil
}

func validateStrangerDealtRow(row StrangerDealtRow) error {
	if strings.TrimSpace(row.Ts) != row.Ts || row.Ts == "" {
		return fmt.Errorf("stranger deal ts must be canonical RFC3339 UTC")
	}
	parsed, err := time.Parse(time.RFC3339, row.Ts)
	if err != nil || parsed.UTC().Format(time.RFC3339) != row.Ts {
		if err != nil {
			return fmt.Errorf("stranger deal ts: %w", err)
		}
		return fmt.Errorf("stranger deal ts must be canonical RFC3339 UTC, got %q", row.Ts)
	}
	if err := validateStrangerCandidateRef(row.CandidateRef); err != nil {
		return err
	}
	if row.CardID != "stranger:"+row.CandidateRef {
		return fmt.Errorf("stranger deal card_id %q does not match candidate_ref %q", row.CardID, row.CandidateRef)
	}
	if strings.TrimSpace(row.Topic) != row.Topic {
		return fmt.Errorf("stranger deal topic must be trimmed")
	}
	return nil
}

func validateStrangerCandidateRef(candidateRef string) error {
	if candidateRef == "" || strings.TrimSpace(candidateRef) != candidateRef {
		return fmt.Errorf("stranger deal candidate_ref must be a non-empty bare id")
	}
	if candidateRef == "." || candidateRef == ".." || candidateRef != filepath.Base(candidateRef) || strings.ContainsAny(candidateRef, `/\`) {
		return fmt.Errorf("stranger deal candidate_ref must be a bare id, got %q", candidateRef)
	}
	return nil
}

// PrepareStrangerDeal re-reads one admitted candidate against a per-orient taxonomy snapshot.
// A nil preparation with nil error means the candidate is no longer live, is expired, or belongs
// to the excluded subtree; callers continue to the next oldest candidate. Ambiguous identity or
// damaged authority returns an error so the optional dealer fails closed.
func (s *Store) PrepareStrangerDeal(candidateID string, context StrangerDealContext, now time.Time) (*StrangerDealPreparation, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("stranger deal preparation time is required")
	}
	if context.resolver == nil {
		return nil, fmt.Errorf("stranger deal context is required")
	}
	if err := validateStrangerCandidateRef(candidateID); err != nil {
		return nil, err
	}
	var candidate Candidate
	if err := candidates.ReadCandidate(s.home, candidates.Memory, candidateID, &candidate); err != nil {
		return nil, err
	}
	if candidate.ID == "" {
		candidate.ID = candidateID
	}
	candidate, _ = AdmitCandidate(candidate)
	if candidate.State != "proposed" && candidate.State != "parked" {
		return nil, nil
	}
	if candidateExpired(candidate, now) {
		return nil, nil
	}
	rawMemberships := normalizeStringRefs(append([]string{candidate.Topic()}, candidate.TopicRefs...))
	canonicalMemberships, err := canonicalStrangerTopicMembership(candidate, context.resolver, context.topicSlugs)
	if err != nil {
		return nil, err
	}
	for _, membership := range canonicalMemberships {
		if context.excluded[membership] {
			return nil, nil
		}
	}
	card := StrangerDealCardSnapshot{
		CandidateRef: candidate.ID,
		Entity:       candidate.Entity,
		Predicate:    candidate.Predicate,
		Object:       candidate.Object,
		State:        candidate.State,
		Topic:        candidate.Topic(),
	}
	payload := strangerDealFingerprintPayload{
		ID:                        candidate.ID,
		Entity:                    candidate.Entity,
		Predicate:                 candidate.Predicate,
		Object:                    candidate.Object,
		State:                     candidate.State,
		Topic:                     candidate.Topic(),
		ExpiresAt:                 strings.TrimSpace(candidate.ExpiresAt),
		RawTopicMemberships:       rawMemberships,
		CanonicalTopicMemberships: canonicalMemberships,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &StrangerDealPreparation{
		card:         card,
		excludeTopic: context.excludeTopic,
		fingerprint:  sha256.Sum256(encoded),
	}, nil
}

// FinalizeStrangerDeal is the single production commit door for a prepared stranger card.
// Candidate content, state, expiry, canonical topic membership, and cooldown are re-read while
// Store's cross-process lock is held, then the row is appended inside the same critical section.
// The bool is false when another orient won the cooldown race or the prepared snapshot changed;
// callers must suppress the prepared card in either case.
func (s *Store) FinalizeStrangerDeal(preparation StrangerDealPreparation, now time.Time, cooldown time.Duration, pairedCommit ...func() error) (bool, error) {
	candidateRef := preparation.card.CandidateRef
	if err := validateStrangerCandidateRef(candidateRef); err != nil {
		return false, err
	}
	if now.IsZero() {
		return false, fmt.Errorf("stranger deal finalize time is required")
	}
	if cooldown <= 0 {
		return false, fmt.Errorf("stranger deal cooldown must be positive")
	}
	if len(pairedCommit) > 1 || (len(pairedCommit) == 1 && pairedCommit[0] == nil) {
		return false, fmt.Errorf("stranger deal accepts at most one non-nil paired commit")
	}

	finalized := false
	err := s.withLock(func() error {
		context, err := s.NewStrangerDealContext(preparation.excludeTopic)
		if err != nil {
			return fmt.Errorf("rebuild stranger deal context: %w", err)
		}
		current, err := s.PrepareStrangerDeal(candidateRef, context, now)
		if err != nil {
			return fmt.Errorf("revalidate stranger candidate %s: %w", candidateRef, err)
		}
		if current == nil || current.fingerprint != preparation.fingerprint {
			return nil
		}

		dealt, err := ReadStrangerDealtLog(s.home)
		if err != nil {
			return fmt.Errorf("recheck stranger cooldown: %w", err)
		}
		for _, existing := range dealt {
			if strings.TrimSpace(existing.CandidateRef) != candidateRef {
				continue
			}
			dealtAt, err := time.Parse(time.RFC3339, strings.TrimSpace(existing.Ts))
			if err != nil {
				return fmt.Errorf("recheck stranger cooldown for %s: %w", candidateRef, err)
			}
			if now.Sub(dealtAt) < cooldown {
				return nil
			}
		}
		row := StrangerDealtRow{
			Ts:           now.UTC().Format(time.RFC3339),
			CardID:       "stranger:" + candidateRef,
			CandidateRef: candidateRef,
			Topic:        preparation.card.Topic,
		}
		rollback, err := appendStrangerDealtRowReversible(s.home, row)
		if err != nil {
			return fmt.Errorf("append stranger deal: %w", err)
		}
		if len(pairedCommit) == 1 {
			if err := pairedCommit[0](); err != nil {
				if rollbackErr := rollback(); rollbackErr != nil {
					return fmt.Errorf("%w: %v; rollback deal append: %v", ErrStrangerDealPairedCommit, err, rollbackErr)
				}
				return fmt.Errorf("%w: %v", ErrStrangerDealPairedCommit, err)
			}
		}
		finalized = true
		return nil
	})
	return finalized, err
}
