package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"compound/internal/candidates"
	"compound/internal/capgraph"
)

const SecondReadSchemaVersion = "compound.panel_second_read.v0.1"

// Divergence classes. severity_blindspot is the only class shaped as a
// capgraph feedback signal (missed_when against the blind lane's node); the
// other two are reader_divergence-shaped and route to panel adjudication —
// which reader is right is a judgment call, not a mechanical one.
const (
	DivergenceDecision          = "decision_divergence"
	DivergenceSeverityBlindspot = "severity_blindspot"
	DivergenceConfidenceGap     = "confidence_gap"
)

// defaultConfidenceGapThreshold: below this, two readers scoring the same lens
// differently is noise, not perspective.
const defaultConfidenceGapThreshold = 0.3

// blindspotObservationConfidence is deliberately below promotion strength: a
// blindspot observation is evidence for adjudication, never a self-promoting
// belief (Consume only applies it after an operationalize verdict).
const blindspotObservationConfidence = 0.6

type SecondReadInput struct {
	BaseRunID   string
	ReaderRunID string
	// ConfidenceGapAbove overrides the divergence threshold; zero means default.
	ConfidenceGapAbove float64
}

type SecondReadReport struct {
	SchemaVersion string               `json:"schema_version"`
	BaseRunID     string               `json:"base_run_id"`
	ReaderRunID   string               `json:"reader_run_id"`
	BaseModel     string               `json:"base_model"`
	ReaderModel   string               `json:"reader_model"`
	TargetKind    string               `json:"target_kind"`
	SharedTargets int                  `json:"shared_targets"`
	Agreement     bool                 `json:"agreement"`
	Divergences   []Divergence         `json:"divergences"`
	Emitted       []EmittedObservation `json:"emitted"`
}

type Divergence struct {
	Class       string `json:"class"`
	TargetRef   string `json:"target_ref"`
	LensRef     string `json:"lens_ref,omitempty"`
	BaseValue   string `json:"base_value"`
	ReaderValue string `json:"reader_value"`
	Detail      string `json:"detail,omitempty"`
}

type EmittedObservation struct {
	ID      string `json:"id"`
	Class   string `json:"class"`
	Skipped bool   `json:"skipped,omitempty"` // already emitted by a prior second-read (idempotent)
}

// SecondRead diffs two completed panel runs over their shared targets and
// emits reader-divergence capability observations into the candidates inbox.
// The runs must read the same target kind through DIFFERENT lens-lane models —
// a second read by the same model is redundancy, not perspective.
func SecondRead(homePath string, in SecondReadInput) (SecondReadReport, error) {
	store, err := Open(homePath)
	if err != nil {
		return SecondReadReport{}, err
	}
	base, err := store.LoadRun(in.BaseRunID)
	if err != nil {
		return SecondReadReport{}, fmt.Errorf("load base run: %w", err)
	}
	reader, err := store.LoadRun(in.ReaderRunID)
	if err != nil {
		return SecondReadReport{}, fmt.Errorf("load reader run: %w", err)
	}
	for _, run := range []Run{base, reader} {
		if run.Status != "verdicts_recorded" && run.Status != "applied" {
			return SecondReadReport{}, fmt.Errorf("second-read requires verdicts_recorded or applied runs, %s is %q", run.ID, run.Status)
		}
	}
	if base.TargetKind != reader.TargetKind {
		return SecondReadReport{}, fmt.Errorf("target kinds differ: base %q vs reader %q", base.TargetKind, reader.TargetKind)
	}
	baseModel := effectiveLensModel(base)
	readerModel := effectiveLensModel(reader)
	if baseModel == "" || readerModel == "" {
		return SecondReadReport{}, fmt.Errorf("both runs must record a lens-lane model")
	}
	if baseModel == readerModel {
		return SecondReadReport{}, fmt.Errorf("second-read requires a different reader model (both runs used %q)", baseModel)
	}

	baseFindings := findingsByTarget(base)
	readerFindings := findingsByTarget(reader)
	shared := []string{}
	for ref := range baseFindings {
		if _, ok := readerFindings[ref]; ok {
			shared = append(shared, ref)
		}
	}
	if len(shared) == 0 {
		return SecondReadReport{}, fmt.Errorf("runs share no target refs (base %d targets, reader %d)", len(baseFindings), len(readerFindings))
	}
	sort.Strings(shared)

	gapThreshold := in.ConfidenceGapAbove
	if gapThreshold <= 0 {
		gapThreshold = defaultConfidenceGapThreshold
	}

	report := SecondReadReport{
		SchemaVersion: SecondReadSchemaVersion,
		BaseRunID:     base.ID,
		ReaderRunID:   reader.ID,
		BaseModel:     baseModel,
		ReaderModel:   readerModel,
		TargetKind:    base.TargetKind,
		SharedTargets: len(shared),
		Divergences:   []Divergence{},
		Emitted:       []EmittedObservation{},
	}

	for _, ref := range shared {
		bf, rf := baseFindings[ref], readerFindings[ref]
		report.Divergences = append(report.Divergences, diffTarget(ref, bf, rf, baseModel, readerModel, gapThreshold)...)
	}
	report.Agreement = len(report.Divergences) == 0

	for _, div := range report.Divergences {
		obs := observationFor(div, base, reader, baseModel, readerModel)
		emitted := EmittedObservation{ID: obs.ID, Class: div.Class}
		var existing capabilityObservationTarget
		if err := candidates.ReadCandidate(homePath, candidates.CapabilityObs, obs.ID, &existing); err == nil {
			// Never overwrite: adjudication may already have set a verdict on it.
			emitted.Skipped = true
			report.Emitted = append(report.Emitted, emitted)
			continue
		}
		if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, obs.ID, obs); err != nil {
			return report, fmt.Errorf("write observation %s: %w", obs.ID, err)
		}
		report.Emitted = append(report.Emitted, emitted)
	}
	return report, nil
}

// effectiveLensModel resolves the model that actually served the lens lane: an
// override run records the LANE id in its findings and route (observed live
// 2026-07-18 — two override runs through the same auto/extract lane looked
// like the "same" reader), so a recorded override:model:<lane>=<model>
// evidence ref wins over the lane's model id.
func effectiveLensModel(run Run) string {
	laneID := strings.TrimSpace(run.Route.LensLane.ModelID)
	prefix := "override:model:" + laneID + "="
	for _, ref := range run.EvidenceRefs {
		if strings.HasPrefix(ref, prefix) {
			if model := strings.TrimSpace(strings.TrimPrefix(ref, prefix)); model != "" {
				return model
			}
		}
	}
	return laneID
}

func findingsByTarget(run Run) map[string]TargetFinding {
	out := make(map[string]TargetFinding, len(run.Findings))
	for _, f := range run.Findings {
		out[f.TargetRef] = f
	}
	return out
}

func diffTarget(ref string, base, reader TargetFinding, baseModel, readerModel string, gapThreshold float64) []Divergence {
	out := []Divergence{}
	if base.Synthesis.present() && reader.Synthesis.present() {
		bd := normalizePanelPosition(base.Synthesis.Decision)
		rd := normalizePanelPosition(reader.Synthesis.Decision)
		if bd != rd {
			out = append(out, Divergence{
				Class: DivergenceDecision, TargetRef: ref,
				BaseValue: bd, ReaderValue: rd,
			})
		}
	}
	baseLens := lensFindingsByRef(base)
	readerLens := lensFindingsByRef(reader)
	lensRefs := []string{}
	for lensRef := range baseLens {
		if _, ok := readerLens[lensRef]; ok {
			lensRefs = append(lensRefs, lensRef)
		}
	}
	sort.Strings(lensRefs)
	for _, lensRef := range lensRefs {
		bl, rl := baseLens[lensRef], readerLens[lensRef]
		if bl.Status != "completed" || rl.Status != "completed" {
			continue
		}
		bSev, rSev := maxSeverityRank(bl), maxSeverityRank(rl)
		switch {
		case rSev >= severityRankHigh && bSev <= severityRankLow:
			out = append(out, Divergence{
				Class: DivergenceSeverityBlindspot, TargetRef: ref, LensRef: lensRef,
				BaseValue: severityRankName(bSev), ReaderValue: severityRankName(rSev),
				Detail: "blind=" + baseModel,
			})
		case bSev >= severityRankHigh && rSev <= severityRankLow:
			out = append(out, Divergence{
				Class: DivergenceSeverityBlindspot, TargetRef: ref, LensRef: lensRef,
				BaseValue: severityRankName(bSev), ReaderValue: severityRankName(rSev),
				Detail: "blind=" + readerModel,
			})
		}
		if gap := bl.Confidence - rl.Confidence; gap > gapThreshold || gap < -gapThreshold {
			out = append(out, Divergence{
				Class: DivergenceConfidenceGap, TargetRef: ref, LensRef: lensRef,
				BaseValue:   fmt.Sprintf("%.2f", bl.Confidence),
				ReaderValue: fmt.Sprintf("%.2f", rl.Confidence),
			})
		}
	}
	return out
}

func lensFindingsByRef(f TargetFinding) map[string]LensFinding {
	out := make(map[string]LensFinding, len(f.LensFindings))
	for _, lf := range f.LensFindings {
		out[lf.LensRef] = lf
	}
	return out
}

const (
	severityRankLow  = 1
	severityRankHigh = 3
)

// maxSeverityRank: none=0 low=1 medium=2 high=3 critical=4. Unknown severities
// rank medium — inert on both sides of the blindspot rule, so a novel severity
// word can never fabricate a blindspot.
func maxSeverityRank(lf LensFinding) int {
	max := 0
	for _, f := range lf.Findings {
		rank := 2
		switch strings.ToLower(strings.TrimSpace(f.Severity)) {
		case "low", "info":
			rank = severityRankLow
		case "medium":
			rank = 2
		case "high":
			rank = severityRankHigh
		case "critical":
			rank = 4
		}
		if rank > max {
			max = rank
		}
	}
	return max
}

func severityRankName(rank int) string {
	switch rank {
	case 0:
		return "none"
	case severityRankLow:
		return "low"
	case 2:
		return "medium"
	case severityRankHigh:
		return "high"
	default:
		return "critical"
	}
}

// observationFor shapes one divergence as a capability-observation candidate.
// A blindspot targets the BLIND lane's capgraph node with a missed_when signal
// (statement = the target kind it went blind on) so an operationalize verdict
// re-weights that lane's belief through capgraph.Consume. Other classes carry
// detail=reader_divergence, which Consume ignores by design — they exist for
// panel/operator adjudication, not automatic belief updates.
func observationFor(div Divergence, base, reader Run, baseModel, readerModel string) capabilityObservationTarget {
	id := "cobs_second_read_" + shortHash(strings.Join([]string{base.ID, reader.ID, div.TargetRef, div.LensRef, div.Class}, "|"))
	evidence := []string{
		"panel:second_read",
		"base_run:" + base.ID,
		"reader_run:" + reader.ID,
		"target:" + div.TargetRef,
		"base_model:" + baseModel,
		"reader_model:" + readerModel,
	}
	if div.LensRef != "" {
		lensRef := div.LensRef
		if !strings.HasPrefix(lensRef, "lens:") {
			lensRef = "lens:" + lensRef
		}
		evidence = append(evidence, lensRef)
	}
	if div.Class == DivergenceSeverityBlindspot {
		blindModel := strings.TrimPrefix(div.Detail, "blind=")
		sightedModel := baseModel
		if blindModel == baseModel {
			sightedModel = readerModel
		}
		subject := blindModel
		if nodeID, ok := capgraph.AgentLaneNodeIDFromShortLaneID(blindModel); ok {
			subject = nodeID
		} else {
			evidence = append(evidence, "lane_node_unresolved:"+blindModel)
		}
		evidence = append(evidence,
			"blind_model:"+blindModel,
			"sighted_model:"+sightedModel,
			"base_max_severity:"+div.BaseValue,
			"reader_max_severity:"+div.ReaderValue,
		)
		return capabilityObservationTarget{
			ID:      id,
			Kind:    string(candidates.CapabilityObs),
			Subject: subject,
			// statement = context token for the missed_when edge: the lane
			// misses when reading this target kind.
			Statement:    base.TargetKind,
			Detail:       capgraph.EdMissedWhen,
			Confidence:   blindspotObservationConfidence,
			State:        "needs_review",
			EvidenceRefs: evidence,
		}
	}
	confidence := 1.0
	statement := fmt.Sprintf("Readers disagree on %s: %s=%s vs %s=%s", div.TargetRef, baseModel, div.BaseValue, readerModel, div.ReaderValue)
	if div.Class == DivergenceConfidenceGap {
		statement = fmt.Sprintf("Reader confidence gap on %s lens %s: %s=%s vs %s=%s", div.TargetRef, div.LensRef, baseModel, div.BaseValue, readerModel, div.ReaderValue)
	}
	return capabilityObservationTarget{
		ID:           id,
		Kind:         string(candidates.CapabilityObs),
		Subject:      "panel_reader_divergence",
		Statement:    statement,
		Detail:       "reader_divergence",
		Confidence:   confidence,
		State:        "needs_review",
		EvidenceRefs: evidence,
	}
}

func shortHash(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:12]
}
