package mem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

// Concurrence is the claim-level, ACTION-gating seam: "may the system act on this
// already-promoted assumption on the operator's behalf?" It asks the SAME content
// of N INDEPENDENT lanes and admits the action only when they CONCUR; the moment
// any one lane dissents, the claim is PARKED (flagged needs_review) — agreement
// under contest is treated as NOT certain. It is the evidence-threshold input a
// policy standing grant reads BEFORE acting on a reversible work class.
//
// BOUNDARY — do not confuse with the existing corroboration organ:
//
//   - Corroboration (EdgeKindCorroboration, B3 cross-lane promotion gate;
//     authority docs_private/plan/packets/b3-cross-lane-promotion-gate.md) marks a
//     CANDIDATE pre-promotion: it raises a parked seed to needs_review, grants
//     decay protection (corroborated candidates never expire), and lets
//     Adjudicate("promote") derive promotion_basis=runtime_proof. Its direction is
//     candidate -> claim (ADMISSION to the ledger).
//   - Concurrence (this organ) operates on a CLAIM already on the ledger and gates
//     ACTION on it. Its direction is claim -> act, and its conservative move is
//     park-on-dissent, not promote-on-proof. It consumes the CorroborateClaim
//     reinforce PRIMITIVE (evidence -> claim) on the concur path, but writes no
//     EdgeKindCorroboration edges and never touches promotion basis.
//
// Together with internal/policy (autonomy_risk.go's operator-only mutation
// classes/reversibilities = the irreversible ceiling, plus StandingGrantID = the
// self-admit grant), the gate a self-admission must pass is a CONJUNCTION:
//
//	may self-admit = concurrence.gate(passed) AND NOT external/destructive/irreversible
//
// Concurrence supplies only the certainty half; policy supplies the safety half.
//
// Mem never calls a model — lane traffic is the rented edge. Callers supply a
// ConcurrenceRunner (e.g. internal/distiller's openai-compatible extractor wired
// across lanes.json). Mem owns the reduction, the independence rule, the ledger
// writes, and the park-on-dissent invariant. "Own spine; rent executors," applied
// to multi-model judgment.
//
// Decision grammar (reuses the spine's adjudication vocabulary; deliberately NOT
// "veto" — that word is owned by laneselect/harvest — and NOT "quorum" — owned by
// the panel lens):
//
//   - any lane dissents           -> park  (mark claim needs_review, NO reinforce)
//   - concur >= threshold, no dissent -> concur (one reinforces edge per witness)
//   - concur <  threshold, no dissent -> hold  (not enough witnesses; no edges)
const (
	ConcurrenceProbeSchemaVersion  = "compound.memory_concurrence_probe.v0.1"
	ConcurrenceReportSchemaVersion = "compound.memory_concurrence_report.v0.1"

	ConcurrenceVerdictAgree    = "agree"
	ConcurrenceVerdictDisagree = "disagree"
	ConcurrenceVerdictAbstain  = "abstain"

	ConcurrenceDecisionConcur = "concur"
	ConcurrenceDecisionPark   = "park"
	ConcurrenceDecisionHold   = "hold"

	concurrenceLaneSourceKind = "concurrence_lane"
)

// ConcurrenceProbe is the request: send the SAME content to independent lanes and
// require Threshold of them to concur before the action may self-admit.
type ConcurrenceProbe struct {
	SchemaVersion string   `json:"schema_version"`
	ID            string   `json:"id"`
	ClaimID       string   `json:"claim_id"`  // the assumption under test (must be on the ledger)
	Content       string   `json:"content"`   // the identical payload sent to every lane
	Lanes         []string `json:"lanes"`     // independent providers to query
	Threshold     int      `json:"threshold"` // distinct concurring lanes required to pass the gate
}

// ConcurrenceAnswer is one lane's verdict on the shared content.
type ConcurrenceAnswer struct {
	Lane          string `json:"lane"`
	ModelProvider string `json:"model_provider,omitempty"`
	ModelName     string `json:"model_name,omitempty"`
	Verdict       string `json:"verdict"` // agree | disagree | abstain
	Summary       string `json:"summary,omitempty"`
	EvidenceRef   string `json:"evidence_ref,omitempty"` // optional pre-recorded evidence id
}

// ConcurrenceReport is the receipt: who was asked, what they said, the
// independent tally, and the gate decision. It embeds the operator card and lives
// under COMPOUND_HOME/reviews/memory-concurrences/ — the same operator-card review
// subtree as memory-bakeoffs — so every outcome surfaces as a card, never a
// silent ledger write.
type ConcurrenceReport struct {
	reviewartifact.OperatorCard
	SchemaVersion             string              `json:"schema_version"`
	GeneratedAt               string              `json:"generated_at"`
	ProbeID                   string              `json:"probe_id"`
	ClaimID                   string              `json:"claim_id"`
	ContentHash               string              `json:"content_hash"`
	Threshold                 int                 `json:"threshold"`
	Answers                   []ConcurrenceAnswer `json:"answers"`
	IndependentLaneCount      int                 `json:"independent_lane_count"`
	IndependentAgreementCount int                 `json:"independent_agreement_count"`
	Dissenters                []string            `json:"dissenters,omitempty"`
	Abstainers                []string            `json:"abstainers,omitempty"`
	AgreementEvidenceRefs     []string            `json:"agreement_evidence_refs,omitempty"`
	CertaintyGate             bool                `json:"certainty_gate"`
	Decision                  string              `json:"decision"`
	Reason                    string              `json:"reason"`
	ReportPath                string              `json:"report_path,omitempty"`
}

// ConcurrenceRunner is the rented edge: given a probe and a lane, produce that
// lane's verdict. Mem supplies the contract; the distiller/CLI supplies the model
// call. A runner that errors for one lane is treated as an abstention, never a
// crash — a flapping free lane must not take down a concurrence sweep.
type ConcurrenceRunner interface {
	RunConcurrence(probe ConcurrenceProbe, lane string) (ConcurrenceAnswer, error)
}

// ConcurrenceRunnerFunc adapts a closure to ConcurrenceRunner.
type ConcurrenceRunnerFunc func(probe ConcurrenceProbe, lane string) (ConcurrenceAnswer, error)

func (f ConcurrenceRunnerFunc) RunConcurrence(probe ConcurrenceProbe, lane string) (ConcurrenceAnswer, error) {
	return f(probe, lane)
}

// RunConcurrence fans the shared content out to every lane via the runner,
// records lane failures as abstentions, and reduces the answers. It is the
// orchestration entrypoint; ReduceConcurrence is the pure, network-free core.
func (s *Store) RunConcurrence(probe ConcurrenceProbe, runner ConcurrenceRunner, now time.Time) (ConcurrenceReport, error) {
	if runner == nil {
		return ConcurrenceReport{}, fmt.Errorf("concurrence: runner required")
	}
	answers := make([]ConcurrenceAnswer, 0, len(probe.Lanes))
	for _, lane := range probe.Lanes {
		ans, err := runner.RunConcurrence(probe, lane)
		if err != nil {
			// A lane that errors abstains; it never poisons the sweep.
			ans = ConcurrenceAnswer{Lane: lane, Verdict: ConcurrenceVerdictAbstain, Summary: "lane error: " + err.Error()}
		}
		if strings.TrimSpace(ans.Lane) == "" {
			ans.Lane = lane
		}
		answers = append(answers, ans)
	}
	return s.ReduceConcurrence(probe, answers, now)
}

// ReduceConcurrence is the pure reducer: it tallies INDEPENDENT verdicts (one vote
// per distinct lane), applies the park-on-dissent invariant, writes the ledger
// edges, and emits the report card. It performs no model calls and is fully
// deterministic in its inputs.
//
// Safety invariant (the load-bearing rule): any dissent parks the claim and marks
// it needs_review, regardless of how many lanes concurred. The gate opens only
// when agreement is independent AND threshold-met AND uncontested.
func (s *Store) ReduceConcurrence(probe ConcurrenceProbe, answers []ConcurrenceAnswer, now time.Time) (ConcurrenceReport, error) {
	if err := validateConcurrenceProbe(probe); err != nil {
		return ConcurrenceReport{}, err
	}
	exists, err := s.claimExists(probe.ClaimID)
	if err != nil {
		return ConcurrenceReport{}, err
	}
	if !exists {
		return ConcurrenceReport{}, fmt.Errorf("concurrence: claim %s not found on ledger", probe.ClaimID)
	}

	// Independence: one vote per distinct lane, first answer wins. A single model
	// answering twice (or a lane alias resolving to the same provider) cannot
	// inflate the agreement count.
	byLane := make(map[string]ConcurrenceAnswer, len(answers))
	var order []string
	for _, a := range answers {
		lane := strings.TrimSpace(a.Lane)
		if lane == "" {
			continue
		}
		if _, seen := byLane[lane]; seen {
			continue
		}
		byLane[lane] = a
		order = append(order, lane)
	}
	sort.Strings(order)

	var agreeLanes, dissenters, abstainers []string
	for _, lane := range order {
		switch strings.TrimSpace(strings.ToLower(byLane[lane].Verdict)) {
		case ConcurrenceVerdictAgree:
			agreeLanes = append(agreeLanes, lane)
		case ConcurrenceVerdictDisagree:
			dissenters = append(dissenters, lane)
		default:
			abstainers = append(abstainers, lane)
		}
	}

	report := ConcurrenceReport{
		SchemaVersion:             ConcurrenceReportSchemaVersion,
		GeneratedAt:               rfc3339(now),
		ProbeID:                   probe.ID,
		ClaimID:                   probe.ClaimID,
		ContentHash:               concurrenceContentHash(probe.Content),
		Threshold:                 probe.Threshold,
		Answers:                   append([]ConcurrenceAnswer(nil), answers...),
		IndependentLaneCount:      len(order),
		IndependentAgreementCount: len(agreeLanes),
		Dissenters:                dissenters,
		Abstainers:                abstainers,
	}

	switch {
	case len(dissenters) > 0:
		// One witness against is enough to stop a self-admission. Mark the claim
		// needs_review so a human (or a later sweep) must clear it; do NOT add any
		// reinforce edges — agreement under contest is not certainty.
		refs, err := s.recordConcurrenceDissent(probe, byLane, dissenters, now)
		if err != nil {
			return ConcurrenceReport{}, err
		}
		report.Decision = ConcurrenceDecisionPark
		report.CertaintyGate = false
		report.AgreementEvidenceRefs = refs
		report.Reason = fmt.Sprintf("parked: %d independent lane(s) dissented (%s); agreement under contest is not certainty",
			len(dissenters), strings.Join(dissenters, ", "))
		report.OperatorCard = reviewartifact.OperatorCard{
			Summary:      fmt.Sprintf("Concurrence PARKED %s: %d dissent (%s)", probe.ClaimID, len(dissenters), strings.Join(dissenters, ", ")),
			ContextLabel: "concurrence",
			GroupKey:     "concurrence:" + probe.ClaimID,
		}

	case len(agreeLanes) >= probe.Threshold:
		// Uncontested, threshold-met, independent agreement: append one reinforces
		// edge per witnessing lane via the idempotent CorroborateClaim PRIMITIVE
		// (evidence -> claim). This is reuse of a primitive, NOT the
		// EdgeKindCorroboration candidate-promotion mark — see the boundary note above.
		refs, err := s.recordConcurrenceAgreement(probe, byLane, agreeLanes, now)
		if err != nil {
			return ConcurrenceReport{}, err
		}
		report.Decision = ConcurrenceDecisionConcur
		report.CertaintyGate = true
		report.AgreementEvidenceRefs = refs
		report.Reason = fmt.Sprintf("concurred: %d/%d independent lanes agreed (threshold %d), no dissent",
			len(agreeLanes), len(order), probe.Threshold)
		report.OperatorCard = reviewartifact.OperatorCard{
			Summary:      fmt.Sprintf("Concurrence PASSED %s: %d/%d lanes agree (threshold %d)", probe.ClaimID, len(agreeLanes), len(order), probe.Threshold),
			ContextLabel: "concurrence",
			GroupKey:     "concurrence:" + probe.ClaimID,
		}

	default:
		// Not enough witnesses, but nobody disagreed: hold. No edges either way —
		// absence of agreement is not evidence against, it is evidence of "not yet."
		report.Decision = ConcurrenceDecisionHold
		report.CertaintyGate = false
		report.Reason = fmt.Sprintf("hold: insufficient independent agreement (%d have, %d needed), no dissent",
			len(agreeLanes), probe.Threshold)
		report.OperatorCard = reviewartifact.OperatorCard{
			Summary:      fmt.Sprintf("Concurrence HELD %s: %d/%d lanes agree (need %d)", probe.ClaimID, len(agreeLanes), len(order), probe.Threshold),
			ContextLabel: "concurrence",
			GroupKey:     "concurrence:" + probe.ClaimID,
		}
	}

	if err := s.writeConcurrenceReport(&report, now); err != nil {
		return ConcurrenceReport{}, err
	}
	return report, nil
}

func validateConcurrenceProbe(probe ConcurrenceProbe) error {
	if strings.TrimSpace(probe.ClaimID) == "" {
		return fmt.Errorf("concurrence: claim_id required")
	}
	if strings.TrimSpace(probe.Content) == "" {
		return fmt.Errorf("concurrence: content required")
	}
	if probe.Threshold < 1 {
		return fmt.Errorf("concurrence: threshold must be >= 1")
	}
	distinct := map[string]bool{}
	for _, l := range probe.Lanes {
		if l = strings.TrimSpace(l); l != "" {
			distinct[l] = true
		}
	}
	if len(distinct) == 0 {
		return fmt.Errorf("concurrence: at least one lane required")
	}
	if probe.Threshold > len(distinct) {
		return fmt.Errorf("concurrence: threshold %d exceeds distinct lane count %d; gate could never pass", probe.Threshold, len(distinct))
	}
	return nil
}

// claimExists mirrors CorroborateClaim's ledger check under a brief lock so a
// concurrence can never attach to a dangling claim id.
func (s *Store) claimExists(claimID string) (bool, error) {
	var exists bool
	err := s.withLock(func() error {
		claims, err := s.loadClaimsRaw()
		if err != nil {
			return err
		}
		for _, c := range claims {
			if c.ID == claimID {
				exists = true
				break
			}
		}
		return nil
	})
	return exists, err
}

// recordConcurrenceAgreement appends one reinforces edge per concurring lane via
// the idempotent CorroborateClaim primitive, returning the recorded evidence ids.
func (s *Store) recordConcurrenceAgreement(probe ConcurrenceProbe, byLane map[string]ConcurrenceAnswer, agreeLanes []string, now time.Time) ([]string, error) {
	refs := make([]string, 0, len(agreeLanes))
	for _, lane := range agreeLanes {
		ev := concurrenceEvidenceItem(probe, byLane[lane])
		recorded, _, err := s.CorroborateClaim(probe.ClaimID, ev, 1, now)
		if err != nil {
			return nil, err
		}
		refs = append(refs, recorded.ID)
	}
	return refs, nil
}

// recordConcurrenceDissent records each dissent as evidence and marks the claim
// needs_review (the repo's "a human must look" marker), deduped by (from, to) so
// re-running a probe never multiplies review edges. Returns the evidence ids.
func (s *Store) recordConcurrenceDissent(probe ConcurrenceProbe, byLane map[string]ConcurrenceAnswer, dissenters []string, now time.Time) ([]string, error) {
	refs := make([]string, 0, len(dissenters))
	for _, lane := range dissenters {
		ev := concurrenceEvidenceItem(probe, byLane[lane])
		recorded, err := s.RecordEvidence(ev, now)
		if err != nil {
			return nil, err
		}
		if err := s.appendNeedsReviewEdgeOnce(probe.ClaimID, recorded.ID, now); err != nil {
			return nil, err
		}
		refs = append(refs, recorded.ID)
	}
	return refs, nil
}

// appendNeedsReviewEdgeOnce appends a needs_review edge (evidence -> claim)
// unless one already exists for that pair, mirroring the reinforce-edge dedup.
func (s *Store) appendNeedsReviewEdgeOnce(claimID, evidenceID string, now time.Time) error {
	return s.withLock(func() error {
		exists, err := s.hasEdge(EdgeKindNeedsReview, evidenceID, claimID)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		seq, err := s.nextSeq()
		if err != nil {
			return err
		}
		edge := Edge{
			Seq:          seq,
			Kind:         EdgeKindNeedsReview,
			From:         evidenceID,
			To:           claimID,
			Confidence:   1,
			EvidenceRefs: []string{evidenceID},
			CreatedAt:    rfc3339(now),
		}
		return appendJSONL(s.edgesLog(), &edge)
	})
}

// concurrenceEvidenceItem builds a content-addressed evidence item for one lane's
// verdict. The lane's own summary (or the shared content) is the quote; the lane
// id is the source, so two lanes agreeing on identical text still record DISTINCT
// evidence (different source_id => different content hash), which is what makes
// the reinforce edges genuinely independent witnesses.
func concurrenceEvidenceItem(probe ConcurrenceProbe, ans ConcurrenceAnswer) EvidenceItem {
	quote := strings.TrimSpace(ans.Summary)
	if quote == "" {
		quote = probe.Content
	}
	return EvidenceItem{
		SourceKind: concurrenceLaneSourceKind,
		SourceID:   "lane:" + strings.TrimSpace(ans.Lane),
		QuoteText:  fmt.Sprintf("[%s] %s :: %s", ans.Verdict, strings.TrimSpace(ans.Lane), quote),
		Actor:      "agent",
	}
}

func concurrenceContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:16]
}

// writeConcurrenceReport persists the receipt under the operator-card review
// subtree, mirroring memory-bakeoffs (COMPOUND_HOME/reviews/memory-bakeoffs/), so
// concurrence reports join the established reviews lineage instead of opening a
// parallel top-level root.
func (s *Store) writeConcurrenceReport(report *ConcurrenceReport, now time.Time) error {
	dir := filepath.Join(s.home, "reviews", "memory-concurrences")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stamp := now.UTC().Format("20060102T150405Z")
	name := fmt.Sprintf("concurrence-%s-%s.json", sanitizeConcurrenceID(report.ProbeID), stamp)
	path := filepath.Join(dir, name)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	report.ReportPath = path
	return nil
}

func sanitizeConcurrenceID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "probe"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
