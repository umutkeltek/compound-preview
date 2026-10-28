package product

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
)

// HypothesisDraftExpectedEffectPlaceholder is the literal, non-invented
// expected_effect a draft carries into BuildHypothesisPlan. The literal text is
// the package-level guarantee: a draft never invents a number or a causal
// claim. Operators replace it before review.
const HypothesisDraftExpectedEffectPlaceholder = "OPERATOR MUST WRITE: what should %s do after %s?"

// HypothesisDraftScopePlaceholder is the literal observation_window a draft
// carries when the supplied signal ids carry no parseable observed_at span.
// It forces a human to fill the actual review window; the loop model no longer
// matches the candidate to a signal until then.
const HypothesisDraftScopePlaceholder = "review window required"

// MintHypothesisDraftCandidate writes a `product_hypothesis` candidate that
// captures "this product has signals but no hypothesis yet" in the form
// `compound product hypothesis-plan --candidate <id>` accepts. The
// expected_effect is a LITERAL placeholder the human MUST replace with a real,
// evidence-shaped claim before review; the writer never invents numbers, lift,
// or causality.
//
// Idempotency: the candidate id is a deterministic digest of (productID,
// changeEvent) so re-running with the same pair never files a second record
// (WriteCandidate rejects overwrite). A second guard short-circuits the write
// when an EXISTING candidate for the same (productID, changeEvent) is already
// open or parked — the loop already sees it, and minting again would only
// confuse the operator.
//
// Inputs:
//   - homePath: COMPOUND_HOME.
//   - productID: the starved product (e.g. "product:alpha-app").
//   - changeEvent: the human sentence grouping the signals.
//   - metrics: signal metric names; the first is the placeholder metric.
//   - signalIDs: the signal ids the draft hangs from (one product_signal:<id>
//     per entry in evidence_refs).
//   - evidenceRefs: extra refs the director chose to attach (always merged
//     with per-signal refs, then unique-sorted).
//   - now: stamped onto the candidate.
//
// Returns the deterministic candidate id (so the director can compose the
// `compound product hypothesis-plan --candidate <id>` command on the same
// pass) and the write error, if any.
func MintHypothesisDraftCandidate(homePath, productID, changeEvent string, metrics, signalIDs, evidenceRefs []string, now time.Time) (string, error) {
	homePath = strings.TrimSpace(homePath)
	productID = strings.TrimSpace(productID)
	changeEvent = strings.TrimSpace(changeEvent)
	if homePath == "" {
		return "", fmt.Errorf("compound product MintHypothesisDraftCandidate: COMPOUND_HOME required")
	}
	if productID == "" {
		return "", fmt.Errorf("compound product MintHypothesisDraftCandidate: product_id required")
	}
	if changeEvent == "" {
		return "", fmt.Errorf("compound product MintHypothesisDraftCandidate: change_event required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	stableMetrics := compact(metrics)
	stableSignals := compact(signalIDs)
	if existingID, exists, err := hypothesisDraftAlreadyExists(homePath, productID, changeEvent); err != nil {
		return "", err
	} else if exists {
		// Dedupe returns the ACTUAL existing candidate's id (2026-09-15): the
		// matched candidate may have been minted by another source under a
		// different id, and the director must compose the plan command against
		// the record that is really on disk — never a freshly calculated id
		// that names nothing.
		return existingID, nil
	}
	observationWindow := observationWindowForDraft(homePath, stableSignals)
	primaryMetric := "the metric"
	if len(stableMetrics) > 0 {
		primaryMetric = stableMetrics[0]
	}
	expectedEffect := fmt.Sprintf(HypothesisDraftExpectedEffectPlaceholder, primaryMetric, changeEvent)
	refs := append([]string{}, evidenceRefs...)
	for _, id := range stableSignals {
		refs = append(refs, "product_signal:"+id)
	}
	refs = compact(refs)
	candidate := HypothesisCandidate{
		SchemaVersion:     ProductHypothesisCandidate,
		ID:                hypothesisDraftCandidateID(productID, changeEvent),
		Kind:              string(candidates.ProductHypothesis),
		ProductID:         productID,
		ChangeEvent:       changeEvent,
		ExpectedEffect:    expectedEffect,
		Metrics:           stableMetrics,
		ObservationWindow: observationWindow,
		Result:            "unknown",
		NextAction:        "operator reviews the draft and replaces the placeholder expected_effect before hypothesis-plan",
		Verdict:           candidates.Park,
		EvidenceRefs:      refs,
		CreatedAt:         now.UTC().Format(time.RFC3339),
	}
	if err := writeHypothesisCandidate(homePath, candidate); err != nil {
		return "", fmt.Errorf("compound product MintHypothesisDraftCandidate: write candidate: %w", err)
	}
	return candidate.ID, nil
}

// hypothesisDraftAlreadyExists reports whether ANY product_hypothesis candidate
// under the home already targets this (productID, changeEvent) pair AND is in a
// recognized verdict state. Parked and open candidates are both
// already-on-the-radar; minting another on top would race the existing one.
// It returns the MATCHED candidate's actual id so the caller can point at the
// real record instead of re-deriving an id the existing candidate may not
// carry (dedupe contract 2026-09-15).
func hypothesisDraftAlreadyExists(homePath, productID, changeEvent string) (string, bool, error) {
	existing, err := loadHypotheses(homePath)
	if err != nil {
		return "", false, err
	}
	normalizedChange := normalizeIDPart(changeEvent)
	for _, h := range existing {
		if strings.TrimSpace(h.ProductID) != strings.TrimSpace(productID) {
			continue
		}
		if normalizeIDPart(h.ChangeEvent) != normalizedChange {
			continue
		}
		if !hypothesisDraftVerdictRecognized(h.Verdict) {
			continue
		}
		id := strings.TrimSpace(h.ID)
		if id == "" {
			continue
		}
		return id, true, nil
	}
	return "", false, nil
}

// hypothesisDraftVerdictRecognized reports whether the verdict is one the
// product_hypothesis spec admits. An unrecognized verdict means the existing
// record was never a real candidate (it is inbox noise from a different
// producer) and should NOT block us from drafting a fresh one.
func hypothesisDraftVerdictRecognized(verdict string) bool {
	switch strings.TrimSpace(verdict) {
	case "", candidates.Park, candidates.Promote, candidates.Operationalize, candidates.Reject, candidates.NoPromote:
		return true
	}
	return false
}

// observationWindowForDraft reads the signal JSONL and returns
// "<earliest>..<latest>" when at least one signal id resolves to a recorded
// observed_at, falling back to HypothesisDraftScopePlaceholder otherwise.
func observationWindowForDraft(homePath string, signalIDs []string) string {
	if len(signalIDs) == 0 {
		return HypothesisDraftScopePlaceholder
	}
	signals, err := loadSignals(homePath)
	if err != nil {
		return HypothesisDraftScopePlaceholder
	}
	want := make(map[string]struct{}, len(signalIDs))
	for _, id := range signalIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			want[id] = struct{}{}
		}
	}
	var earliest, latest time.Time
	for _, s := range signals {
		if _, ok := want[s.ID]; !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(s.ObservedAt))
		if err != nil {
			continue
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
		if latest.IsZero() || at.After(latest) {
			latest = at
		}
	}
	if earliest.IsZero() || latest.IsZero() {
		return HypothesisDraftScopePlaceholder
	}
	return earliest.UTC().Format("2006-01-02") + ".." + latest.UTC().Format("2006-01-02")
}

// hypothesisDraftCandidateID is the deterministic id minted from
// (productID, changeEvent). Re-running for the same pair reuses the same id
// (WriteCandidate's overwrite guard is the first idempotency layer; the
// "already exists" guard above is the second, content-aware layer).
func hypothesisDraftCandidateID(productID, changeEvent string) string {
	productID = strings.TrimSpace(productID)
	changeEvent = strings.TrimSpace(changeEvent)
	sum := sha256.Sum256([]byte("hypdraft|" + productID + "|" + changeEvent))
	return "hyp_draft_" + hex.EncodeToString(sum[:])[:12]
}

// ReadHypothesisDraftCandidate reads the candidate the writer left so tests
// can verify the writer (production code keeps calling
// MintHypothesisDraftCandidate and the downstream BuildHypothesisPlan).
func ReadHypothesisDraftCandidate(homePath, id string) (HypothesisCandidate, error) {
	var c HypothesisCandidate
	if err := candidates.ReadCandidate(homePath, candidates.ProductHypothesis, id, &c); err != nil {
		return HypothesisCandidate{}, err
	}
	if strings.TrimSpace(c.ID) == "" {
		c.ID = id
	}
	return c, nil
}
