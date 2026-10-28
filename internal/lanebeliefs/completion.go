package lanebeliefs

import (
	"sort"
	"strings"

	"compound/internal/envelope"
)

// The completion posterior is the fold's per-(lane, capability_class) Beta posterior of success —
// P(a work this lane attempts in this class terminates completed, not failed/cancelled). It is the
// p_success denominator input the EV-card composer prices against a lane's cost_per_attempt; the
// fold is the ONE producer of it (there is no second belief owner), and the card only ever READS it.
//
// It reuses the fold's machinery wholesale: the same event cursor, the same receipt grouping, the
// same BETA_PRIOR_ALPHA/BETA (2/2) and MIN_LANE_RECEIPTS floor as the tendency/capability kinds. It
// is deliberately NOT a promotable belief shape — it mints no memory candidate and never touches the
// B1 valve — so it stays propose-only in the strongest sense: the fold writes its own cursor + its
// belief observations + its completion observations + its belief candidates, and NOTHING else.
//
// Class conditioning (investigation result, docs_private/plan/ev-card-spec.md S0):
// capability_class is NOT carried on a Receipt or on its WorkEnvelope — verified against the landed
// shapes. The receipt exposes producer.lane_id (resolved to a lane node via the B0 door) and a
// closed move_type enum, but move_type is a mutation-shape taxonomy, not the routing capability class
// (build-implement / judge-critique / classify-extract / plan-decompose …). The only surface that
// carries capability class at all is the route-evidence ledger, and even there it is not a stored
// field: it is reverse-derived from the selected target through the model-capability routing
// defaults, a mapping that is both lossy and multi-valued (routeevidence.RouteOutcomeReview keeps it
// as a []string). Conditioning a per-attempt Beta trial on a multi-valued, ledger-dependent
// reverse-map is neither clean nor in this slice's scope, so every receipt-sourced attempt buckets
// into the explicit "unknown" class today (see completionCapabilityClass). The bucket dimension is
// nonetheless first-class: a KNOWN class is never pooled with another and "unknown" is its own
// bucket, so when a later slice wires a single-valued class resolver the guarantee already holds.

const completionPosteriorSchemaVersion = "compound.lanebeliefs.completion_posterior.v0.1"

// completionCapabilityClass resolves the capability class a completion trial is conditioned on. It
// returns the explicit "unknown" sentinel for every receipt-sourced attempt: neither the Receipt nor
// its WorkEnvelope carries a capability class, and the route-evidence reverse-map that could infer
// one is lossy/multi-valued and out of this slice's read-only, self-contained scope. It is a seam,
// not a guess — a future producer that can attribute a SINGLE class to a work substitutes a real
// resolution here without reshaping the store, and the never-pool invariant still holds because the
// class is the observation domain (the GroupKey axis), never merged after the fact.
func completionCapabilityClass(_ envelope.Receipt) string {
	return capabilityClassUnknown
}

// completionSignal mints the completion Beta trial for one terminal work attempt: a hit (1) only when
// the WORK's terminal state is completed, a miss (0) for failed/cancelled, conditioned on the given
// capability class (defaulted to the "unknown" bucket). Opportunities is always 1 — one work is one
// attempt, receipt spam inside it never inflates the denominator.
func completionSignal(terminalState, capabilityClass string) beliefSignal {
	hits := 0
	if strings.TrimSpace(terminalState) == completionOutcomeCompleted {
		hits = 1
	}
	class := strings.TrimSpace(capabilityClass)
	if class == "" {
		class = capabilityClassUnknown
	}
	return beliefSignal{
		Kind:          beliefKindCompletion,
		Domain:        class,
		Object:        completionObjectAttempt,
		Hits:          hits,
		Opportunities: 1,
	}
}

// CompletionPosteriorReport is the read surface a downstream consumer (the EV-card composer) prices
// against. It carries the Beta posterior mean AND the raw evidence counts, because a 2/2 prior must
// never be mistaken for knowledge: Attempts==0 (PriorOnly) means the mean is the bare prior, and
// ReceiptCount below MIN_LANE_RECEIPTS (MeetsFloor==false) means the lane has not cleared the noise
// floor and the card must treat the class as unpriceable rather than guess.
type CompletionPosteriorReport struct {
	SchemaVersion   string `json:"schema_version"`
	LaneID          string `json:"lane_id"`
	CapabilityClass string `json:"capability_class"`
	// Attempts is the Beta denominator (distinct work attempts observed), Completions the numerator
	// (attempts that terminated completed), Failures the remainder (failed/cancelled). ReceiptCount is
	// raw receipt volume across those attempts — the quantity the MIN_LANE_RECEIPTS floor gates on.
	Attempts     int `json:"attempts"`
	Completions  int `json:"completions"`
	Failures     int `json:"failures"`
	ReceiptCount int `json:"receipt_count"`
	// PriorAlpha/PriorBeta echo the Beta prior so PosteriorMean is legible as (alpha+completions) /
	// (alpha+beta+attempts) rather than an opaque scalar.
	PriorAlpha    int     `json:"prior_alpha"`
	PriorBeta     int     `json:"prior_beta"`
	PosteriorMean float64 `json:"posterior_mean"`
	// MeetsFloor is false until ReceiptCount ≥ MIN_LANE_RECEIPTS; PriorOnly is true when zero attempts
	// were observed (the mean is the untouched prior). A consumer must refuse to price when either
	// signals no knowledge.
	MeetsFloor bool `json:"meets_floor"`
	PriorOnly  bool `json:"prior_only"`
	// ObservationRefs are the completion trials this posterior derives from (one evidence receipt id
	// per work attempt), sorted and deduped. There is no belief ref: the completion kind mints no
	// promotable belief candidate, so the posterior is derived purely from its observation trials.
	ObservationRefs []string `json:"observation_refs,omitempty"`
}

// CompletionPosterior reads the fold's recorded completion trials and returns the Beta posterior of
// completion for one (lane node id, capability_class) cell. laneID is the RESOLVED agent-lane node id
// the fold stores (the same id the B0 door mints), and capabilityClass defaults to the "unknown"
// bucket when empty. It is a pure read: it opens no store, mints no candidate, and writes nothing.
func CompletionPosterior(homePath, laneID, capabilityClass string) (CompletionPosteriorReport, error) {
	laneID = strings.TrimSpace(laneID)
	capabilityClass = strings.TrimSpace(capabilityClass)
	if capabilityClass == "" {
		capabilityClass = capabilityClassUnknown
	}
	report := CompletionPosteriorReport{
		SchemaVersion:   completionPosteriorSchemaVersion,
		LaneID:          laneID,
		CapabilityClass: capabilityClass,
		PriorAlpha:      BETA_PRIOR_ALPHA,
		PriorBeta:       BETA_PRIOR_BETA,
	}
	observations, err := loadCompletionObservations(homePath)
	if err != nil {
		return report, err
	}
	seenRefs := map[string]bool{}
	var refs []string
	for _, obs := range observations {
		// A KNOWN class is never pooled with another and never merged into unknown: the cell is the
		// exact (lane, class) pair, so a mismatch on either axis contributes nothing here.
		if obs.SubjectLaneNodeID != laneID || obs.Domain != capabilityClass {
			continue
		}
		report.Attempts += obs.Opportunities
		report.Completions += obs.Hits
		report.ReceiptCount += obs.ReceiptCount
		if ref := strings.TrimSpace(obs.EvidenceEventID); ref != "" && !seenRefs[ref] {
			seenRefs[ref] = true
			refs = append(refs, ref)
		}
	}
	report.Failures = report.Attempts - report.Completions
	report.PosteriorMean = posteriorMean(report.Completions, report.Attempts)
	report.MeetsFloor = report.ReceiptCount >= MIN_LANE_RECEIPTS
	report.PriorOnly = report.Attempts == 0
	sort.Strings(refs)
	report.ObservationRefs = refs
	return report, nil
}
