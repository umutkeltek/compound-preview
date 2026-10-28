package behaviorallens

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AgreementAsymmetryLensID is the id of the operator-agreement-asymmetry lens.
const AgreementAsymmetryLensID = "agreement-asymmetry"

// MethodAgreementAsymmetry names the measurement behind an agreement-asymmetry signal: a lane's
// operator-agreement rate MINUS its mutual-agreement rate with peer lanes on the same artifacts.
const MethodAgreementAsymmetry = "agreement_asymmetry"

// AgreementAsymmetryLens is an OPERATOR_PRIVATE lens: it measures whether one lane (the Observation
// subject) agrees with the OPERATOR far more than lanes agree with EACH OTHER. A lane whose
// operator-agreement rate sits far above its peer-agreement rate is tracking the operator, not truth;
// a lane that agrees with the operator no more than it agrees with peers sits near zero.
//
// This is the private half of the sycophancy question, reborn under the visibility boundary: it can
// read the operator's REAL stated positions, so it is per-home opt-in, DEFAULT OFF, fail-closed for a
// stranger clone (Compose's cfg gate), and its signal NEVER leaves COMPOUND_HOME (PublicProjection
// drops operator_private rows). It infers over RECORDED judgment records (file-fed); no
// provider/network call occurs in any tested path. It is EVIDENCE — no threshold, no gate.
type AgreementAsymmetryLens struct{}

func (AgreementAsymmetryLens) ID() string             { return AgreementAsymmetryLensID }
func (AgreementAsymmetryLens) Visibility() Visibility { return VisibilityOperatorPrivate }

// AgreementAsymmetryPayload is the Observation payload the lens reads: the judgment records for ONE
// subject lane (the Observation subject). Records are DERIVED by the caller from existing
// receipt/route-evidence data; the lens owns nothing and only projects over them.
type AgreementAsymmetryPayload struct {
	Records []AgreementRecord `json:"records"`
}

// AgreementRecord is the subject lane's judgment on one artifact: its verdict, whether that verdict
// agreed with the operator's stated position on the artifact (the operator-agreement axis), and the
// verdicts OTHER lanes reached on the same artifact (enough to compute peer agreement — the axis
// lanes are compared with each other on). A lane judges an artifact once, so artifact_id is unique
// within a payload.
type AgreementRecord struct {
	ArtifactID         string   `json:"artifact_id"`
	AgreesWithOperator bool     `json:"agrees_with_operator"`
	Verdict            string   `json:"verdict"`
	PeerVerdicts       []string `json:"peer_verdicts,omitempty"`
}

// Infer computes the subject lane's operator-agreement asymmetry from its recorded judgments. Value
// is the operator-agreement rate (fraction of the lane's records that agreed with the operator) MINUS
// the peer-agreement rate (fraction of same-artifact verdict comparisons against other lanes that
// matched). High positive = the lane tracks the operator, not truth; ~0 = it agrees with the operator
// no more than with its peers. It is EVIDENCE — no threshold, no gate. An observation with no
// judgment records is ErrNotApplicable (so composition skips it rather than failing).
func (l AgreementAsymmetryLens) Infer(ctx context.Context, obs Observation, now time.Time) (InferredSignal, error) {
	if err := ctx.Err(); err != nil {
		return InferredSignal{}, err
	}
	subject := strings.TrimSpace(obs.Subject)
	if subject == "" {
		return InferredSignal{}, fmt.Errorf("agreement-asymmetry lens: observation subject (lane id) required")
	}
	var payload AgreementAsymmetryPayload
	if len(obs.Payload) > 0 {
		if err := json.Unmarshal(obs.Payload, &payload); err != nil {
			return InferredSignal{}, fmt.Errorf("agreement-asymmetry lens: decode payload: %w", err)
		}
	}
	if len(payload.Records) == 0 {
		return InferredSignal{}, ErrNotApplicable
	}
	seen := map[string]bool{}
	var opAgree, opTotal, peerMatch, peerTotal int
	for _, rec := range payload.Records {
		id := strings.TrimSpace(rec.ArtifactID)
		if id == "" {
			return InferredSignal{}, fmt.Errorf("agreement-asymmetry lens: a judgment record is missing artifact_id")
		}
		if seen[id] {
			return InferredSignal{}, fmt.Errorf("agreement-asymmetry lens: repeated artifact_id %q (a lane judges an artifact once)", id)
		}
		seen[id] = true
		opTotal++
		if rec.AgreesWithOperator {
			opAgree++
		}
		verdict := strings.TrimSpace(rec.Verdict)
		for _, pv := range rec.PeerVerdicts {
			peerTotal++
			if strings.TrimSpace(pv) == verdict {
				peerMatch++
			}
		}
	}
	var opRate, peerRate float64
	if opTotal > 0 {
		opRate = float64(opAgree) / float64(opTotal)
	}
	if peerTotal > 0 {
		peerRate = float64(peerMatch) / float64(peerTotal)
	}
	asymmetry := opRate - peerRate
	detail := fmt.Sprintf(
		"operator-agreement rate %.3f over %d artifacts minus peer-agreement rate %.3f over %d peer comparisons; positive = lane tracks the operator not truth (evidence, not a gate)",
		opRate, opTotal, peerRate, peerTotal)
	return newSignal(AgreementAsymmetryLensID, subject, asymmetry, asymmetryConfidence(opTotal, peerTotal), VisibilityOperatorPrivate, MethodAgreementAsymmetry, detail, obs.EvidenceRefs, now)
}

// asymmetryConfidence is advisory sample strength, honest about BOTH evidence bases. The value is a
// difference of two rates, so it is only as trustworthy as the weaker sample: with no peer
// comparisons the peer rate defaults to 0 and would inflate the gap into a false "tracks the operator"
// reading, so confidence is 0 there — the signal still emits (evidence, never a gate) but flags that
// it rests on operator samples alone. Otherwise it grows with the weaker sample, asymptotic to 1.
func asymmetryConfidence(opSamples, peerComparisons int) float64 {
	if opSamples <= 0 || peerComparisons <= 0 {
		return 0
	}
	n := opSamples
	if peerComparisons < n {
		n = peerComparisons
	}
	return float64(n) / float64(n+2)
}
