package mem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"compound/internal/policy"
)

const OperatorConfirmationSourceKind = "operator_confirmation"

// Evidence source kinds are OPEN by design: any medium may record evidence through the
// generic knowledge intake (new medium = 1 edge + 0 core changes), so there is no allowlist.
// A kind is a lowercase slug so provenance stays greppable and typo-resistant in shape.
var evidenceSourceKindPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]*$`)

// canonicalEvidenceID matches the content-addressed format RecordEvidence mints
// (ev_ + first 12 hex chars of the content hash). Rows predating that ratchet
// (UUID-suffixed session-transcript ids, extended-hex note ids, and canonical-
// looking ids hashed by the old formula) can never match the canonical address.
// The fold treats pre-ratchet rows as legacy: structural checks only, never a
// hard fail — legacy data is old, not a forgery. Post-ratchet rows are still
// validated strictly and fail loud on drift.
const evidenceAddressRatchetDate = "2026-08-03"

var canonicalEvidenceID = regexp.MustCompile(`^ev_[0-9a-f]{12}$`)

// isLegacyEvidence reports whether a row predates the content-address ratchet,
// by its own observed_at when present; rows without a timestamp are treated as
// legacy (old data, conservative for the strict fold). Operator-confirmation
// rows are governed and always validate strictly — the strict fold exists to
// protect adjudication, so no confirmation row is ever "legacy".
func isLegacyEvidence(e EvidenceItem) bool {
	if e.SourceKind == OperatorConfirmationSourceKind {
		return false
	}
	if canonicalEvidenceID.MatchString(e.ID) && e.ObservedAt >= evidenceAddressRatchetDate {
		return false
	}
	return true
}

// EvidenceItem is immutable, citable raw material — never truth on its own, but the thing a claim
// points at. The spine-native convergence of the Gen-1 memory.EvidenceItem: mem previously kept
// only a SnippetHash on the candidate, with no stored, retrievable evidence object. The id is
// CONTENT-ADDRESSED, so recording the same evidence twice is idempotent and an evidence ref is stable.
type EvidenceItem struct {
	ID          string `json:"id"` // ev_<content-hash prefix>
	SourceKind  string `json:"source_kind"`
	SourceID    string `json:"source_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	QuoteText   string `json:"quote_text,omitempty"`
	ContentHash string `json:"content_hash"`
	EventTime   string `json:"event_time,omitempty"` // when the discussed thing happened
	ObservedAt  string `json:"observed_at"`          // when compound ingested it
	Actor       string `json:"actor,omitempty"`      // user | assistant | tool | system | agent
	Visibility  string `json:"visibility,omitempty"`
}

func (s *Store) evidenceLog() string { return s.home + "/ledger/evidence.jsonl" }

// RecordEvidence stores a citable evidence item, content-addressed and idempotent (same
// source+content → same id → no duplicate append). Returns the stored item (with id).
func (s *Store) RecordEvidence(e EvidenceItem, now time.Time) (EvidenceItem, error) {
	return s.recordEvidence(e, now, false)
}

// RecordOperatorConfirmation is the reserved operator-owned evidence adapter for
// an already-parked memory candidate. Generic evidence intake cannot mint this
// source kind, and confirmation consumption revalidates the content address and
// candidate state before claim append.
func (s *Store) RecordOperatorConfirmation(candidateID, confirmation, actor string, now time.Time) (EvidenceItem, error) {
	candidateID = strings.TrimSpace(candidateID)
	confirmation = strings.TrimSpace(confirmation)
	actor = strings.TrimSpace(actor)
	if !policy.HasOperatorProvenance(actor) {
		return EvidenceItem{}, fmt.Errorf("operator confirmation requires operator provenance, got %q", actor)
	}
	candidate, err := s.getCandidate(candidateID)
	if err != nil {
		return EvidenceItem{}, err
	}
	if candidate.State != "parked" {
		return EvidenceItem{}, fmt.Errorf("operator confirmation requires parked candidate %s, got state %q", candidateID, candidate.State)
	}
	if confirmation == "" {
		return EvidenceItem{}, fmt.Errorf("operator confirmation text required")
	}
	return s.recordEvidence(EvidenceItem{
		SourceKind: OperatorConfirmationSourceKind,
		SourceID:   candidateID,
		QuoteText:  confirmation,
		Actor:      actor,
		Visibility: "private",
	}, now, true)
}

func (s *Store) recordEvidence(e EvidenceItem, now time.Time, allowOperatorConfirmation bool) (EvidenceItem, error) {
	if strings.TrimSpace(e.QuoteText) == "" && strings.TrimSpace(e.SourceID) == "" {
		return EvidenceItem{}, fmt.Errorf("evidence needs quote_text or source_id")
	}
	e.SourceKind = strings.TrimSpace(e.SourceKind)
	if e.SourceKind == "" {
		return EvidenceItem{}, fmt.Errorf("evidence source_kind required")
	}
	if !evidenceSourceKindPattern.MatchString(e.SourceKind) {
		return EvidenceItem{}, fmt.Errorf("evidence source_kind %q must be a lowercase slug (a-z0-9._:-)", e.SourceKind)
	}
	if e.SourceKind == OperatorConfirmationSourceKind && !allowOperatorConfirmation {
		return EvidenceItem{}, fmt.Errorf("evidence source_kind %q is reserved; use the operator confirmation adapter", e.SourceKind)
	}
	if e.SourceKind == OperatorConfirmationSourceKind {
		e.Actor = strings.TrimSpace(e.Actor)
	}
	e.ContentHash = evidenceContentHash(e)
	e.ID = "ev_" + e.ContentHash[:12]
	if e.Visibility == "" {
		e.Visibility = "operational"
	}
	if existing, ok, err := s.GetEvidence(e.ID); err != nil {
		return EvidenceItem{}, err
	} else if ok {
		return existing, nil // idempotent: identical evidence already recorded
	}
	e.ObservedAt = rfc3339(now)
	return e, appendJSONL(s.evidenceLog(), &e)
}

func evidenceContentHash(e EvidenceItem) string {
	material := e.SourceKind + "\x00" + e.SourceID + "\x00" + e.QuoteText
	if e.SourceKind == OperatorConfirmationSourceKind {
		material += "\x00" + e.Actor
	}
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

func validateEvidenceContentAddress(e EvidenceItem) error {
	wantHash := evidenceContentHash(e)
	wantID := "ev_" + wantHash[:12]
	if e.ContentHash != wantHash || e.ID != wantID {
		return fmt.Errorf("evidence %q content address mismatch", e.ID)
	}
	return nil
}

// Evidence loads all evidence items (append-only, last-wins by id, deterministic order).
// Every row's content address is revalidated at fold time — an ev_ id is
// derived from the content, so a second same-id row with mismatched content
// (or any tampered row) is a forgery the fold must fail loud on, never
// silently last-win. Previously only the explicit-confirmation path
// revalidated (mem.go adjudicate); the fold now shares that discipline.
func (s *Store) Evidence() ([]EvidenceItem, error) {
	byID := map[string]EvidenceItem{}
	var order []string
	err := readJSONL(s.evidenceLog(), func(line []byte) error {
		var e EvidenceItem
		if json.Unmarshal(line, &e) != nil || e.ID == "" {
			return nil
		}
		if err := validateEvidenceContentAddress(e); err != nil {
			// Pre-ratchet rows (observed before the strict fold shipped) cannot
			// satisfy the canonical address formula; verify their shape instead of
			// failing the whole ledger. Post-ratchet rows keep the strict fail-loud
			// fold — a drift there is a forgery.
			if !isLegacyEvidence(e) {
				return err
			}
			if !strings.HasPrefix(e.ID, "ev_") || !evidenceSourceKindPattern.MatchString(e.SourceKind) {
				return fmt.Errorf("legacy evidence %q malformed: id prefix or source_kind", e.ID)
			}
		}
		if _, seen := byID[e.ID]; !seen {
			order = append(order, e.ID)
		}
		byID[e.ID] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]EvidenceItem, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

// GetEvidence returns one evidence item by id.
func (s *Store) GetEvidence(id string) (EvidenceItem, bool, error) {
	items, err := s.Evidence()
	if err != nil {
		return EvidenceItem{}, false, err
	}
	for _, e := range items {
		if e.ID == id {
			return e, true, nil
		}
	}
	return EvidenceItem{}, false, nil
}
