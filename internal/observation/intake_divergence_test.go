package observation

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// divergenceProbeBatch builds a synthetic svc_probe tool_health batch used to
// exercise the divergence_declared card. Subject/source names are synthetic
// per repo privacy law: never a real hostname or service name.
func divergenceProbeBatch(observationID string, dimensions map[string]string) Batch {
	b := validBatch()
	b.Source = Source{
		Kind:       "svc_probe",
		Ref:        "fixture:svc-alpha-probe",
		CapturedAt: tObservation.Format(time.RFC3339),
		Collector:  "fixture-svc-probe",
		Privacy:    "synthetic",
	}
	b.Subject = Subject{
		Kind:    "service",
		ID:      "svc-alpha",
		Surface: "infra",
		Channel: "probe",
	}
	b.Observations = []Observation{{
		ID:           observationID,
		Kind:         "tool_health",
		ObservedAt:   tObservation.Format(time.RFC3339),
		Title:        "svc-alpha health probe",
		Confidence:   1,
		Dimensions:   dimensions,
		EvidenceRefs: []string{"fixture:svc-alpha-probe"},
	}}
	return b
}

func countActionCardsByKind(cards []SourceIntakeActionCard, kind string) int {
	count := 0
	for _, card := range cards {
		if card.Kind == kind {
			count++
		}
	}
	return count
}

// TestSourceIntakeProjectionDeclaresDivergenceImmediately is the red-first
// falsifier: a tool_health observation whose dimensions declare
// expected_state != observed_state must surface a divergence_declared card
// immediately, well below the 72h stale_source age threshold.
func TestSourceIntakeProjectionDeclaresDivergenceImmediately(t *testing.T) {
	home := t.TempDir()
	sourcePath := filepath.Join(home, "svc-alpha-batch.json")
	batch := divergenceProbeBatch("obs_svc_alpha_divergence", map[string]string{
		"expected_state": "active_running",
		"observed_state": "crash_loop",
	})
	writeJSONFile(t, sourcePath, batch)

	report, err := ObserveBatch(home, nil, batch, sourcePath, Options{ApplySourceObservations: true}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SourceObservationApplyResults) != 1 {
		t.Fatalf("apply results = %+v", report.SourceObservationApplyResults)
	}
	sourceObservationID := report.SourceObservationApplyResults[0].SourceObservationID

	projection, err := LoadSourceIntakeProjection(home, tObservation, 0)
	if err != nil {
		t.Fatal(err)
	}

	if count := countActionCardsByKind(projection.ActionCards, "divergence_declared"); count != 1 {
		t.Fatalf("divergence_declared card count = %d, cards = %+v", count, projection.ActionCards)
	}
	card := findSourceIntakeActionCard(projection.ActionCards, "divergence_declared")
	if card == nil {
		t.Fatalf("expected divergence_declared card, got: %+v", projection.ActionCards)
	}
	if !strings.Contains(card.Title, "active_running") || !strings.Contains(card.Title, "crash_loop") {
		t.Fatalf("divergence card title = %q, want expected/observed state text", card.Title)
	}
	if !containsString(card.EvidenceRefs, "source_observation:"+sourceObservationID) {
		t.Fatalf("divergence card evidence refs = %+v, want source_observation:%s", card.EvidenceRefs, sourceObservationID)
	}
	// The record is fresh (observed at tObservation, well below the 72h stale
	// window), so this card must not be riding stale_source.
	if findSourceIntakeActionCard(projection.ActionCards, "stale_source") != nil {
		t.Fatalf("fresh divergence record unexpectedly produced a stale_source card: %+v", projection.ActionCards)
	}
}

// TestSourceIntakeProjectionSkipsDivergenceCardWhenStatesAgreeOrMissing is the
// negative falsifier: equal expected/observed values, or missing dimensions,
// must never produce a divergence_declared card (never guess divergence from
// prose).
func TestSourceIntakeProjectionSkipsDivergenceCardWhenStatesAgreeOrMissing(t *testing.T) {
	home := t.TempDir()

	equalPath := filepath.Join(home, "svc-alpha-equal-batch.json")
	equalBatch := divergenceProbeBatch("obs_svc_alpha_equal", map[string]string{
		"expected_state": "active_running",
		"observed_state": "active_running",
	})
	writeJSONFile(t, equalPath, equalBatch)
	if _, err := ObserveBatch(home, nil, equalBatch, equalPath, Options{ApplySourceObservations: true}, tObservation); err != nil {
		t.Fatal(err)
	}

	noDimsPath := filepath.Join(home, "svc-alpha-no-dims-batch.json")
	noDimsBatch := divergenceProbeBatch("obs_svc_alpha_no_dims", nil)
	writeJSONFile(t, noDimsPath, noDimsBatch)
	if _, err := ObserveBatch(home, nil, noDimsBatch, noDimsPath, Options{ApplySourceObservations: true}, tObservation); err != nil {
		t.Fatal(err)
	}

	projection, err := LoadSourceIntakeProjection(home, tObservation, 0)
	if err != nil {
		t.Fatal(err)
	}
	if card := findSourceIntakeActionCard(projection.ActionCards, "divergence_declared"); card != nil {
		t.Fatalf("equal/missing dimensions must not produce a divergence_declared card: %+v", card)
	}
}

// TestSourceIntakeProjectionDedupesDivergenceCardOnReapply is the dedupe
// falsifier: re-applying the identical batch must not stack a second
// divergence_declared card for the same observation.
func TestSourceIntakeProjectionDedupesDivergenceCardOnReapply(t *testing.T) {
	home := t.TempDir()
	sourcePath := filepath.Join(home, "svc-alpha-batch.json")
	batch := divergenceProbeBatch("obs_svc_alpha_dedupe", map[string]string{
		"expected_state": "active_running",
		"observed_state": "crash_loop",
	})
	writeJSONFile(t, sourcePath, batch)

	for i := 0; i < 2; i++ {
		if _, err := ObserveBatch(home, nil, batch, sourcePath, Options{ApplySourceObservations: true}, tObservation); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	projection, err := LoadSourceIntakeProjection(home, tObservation, 0)
	if err != nil {
		t.Fatal(err)
	}
	if count := countActionCardsByKind(projection.ActionCards, "divergence_declared"); count != 1 {
		t.Fatalf("expected exactly one divergence_declared card after reapply, got %d: %+v", count, projection.ActionCards)
	}
}

// TestSourceConnectionsProjectionCarriesDivergenceCard is the join-proof
// falsifier: the connections projection (the edge the director reads) must
// surface the divergence_declared card on the ad-hoc source connection.
func TestSourceConnectionsProjectionCarriesDivergenceCard(t *testing.T) {
	home := t.TempDir()
	sourcePath := filepath.Join(home, "svc-alpha-batch.json")
	batch := divergenceProbeBatch("obs_svc_alpha_join", map[string]string{
		"expected_state": "active_running",
		"observed_state": "crash_loop",
	})
	writeJSONFile(t, sourcePath, batch)
	report, err := ObserveBatch(home, nil, batch, sourcePath, Options{ApplySourceObservations: true}, tObservation)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SourceObservationApplyResults) != 1 {
		t.Fatalf("apply results = %+v", report.SourceObservationApplyResults)
	}
	sourceObservationID := report.SourceObservationApplyResults[0].SourceObservationID

	connections, err := LoadSourceConnectionsProjection(home, tObservation)
	if err != nil {
		t.Fatal(err)
	}

	var conn *SourceConnection
	for i := range connections.Connections {
		if connections.Connections[i].ID == "source_svc_probe" {
			conn = &connections.Connections[i]
			break
		}
	}
	if conn == nil {
		t.Fatalf("no ad-hoc source_svc_probe connection found: %+v", connections.Connections)
	}
	card := findSourceIntakeActionCard(conn.LatestCards, "divergence_declared")
	if card == nil {
		t.Fatalf("connection missing divergence_declared card: %+v", conn.LatestCards)
	}
	if !containsString(card.EvidenceRefs, "source_observation:"+sourceObservationID) {
		t.Fatalf("card evidence refs = %+v, want source_observation:%s", card.EvidenceRefs, sourceObservationID)
	}
}
