package mem

import (
	"strings"
	"time"

	"compound/internal/policy"
)

const (
	AdmissionClassDurableMemory     = "durable_memory_candidate"
	AdmissionClassSourceMetadata    = "source_metadata"
	AdmissionClassAnnotatedEvidence = "annotated_evidence"

	AdmissionSurfaceOperatorQueue     = "operator_queue"
	AdmissionSurfaceSourceIndex       = "source_index"
	AdmissionSurfaceAnnotatedEvidence = "annotated_evidence"
	AdmissionSurfaceFiled             = "filed"
	AdmissionSurfaceDecayPark         = "decay_park"
)

type CandidateAdmission struct {
	Class          string
	State          string
	Reason         string
	ReviewRequired bool
	TargetSurface  string
	Disposition    string // policy disposition when applied
}

// AdmitCandidate classifies and stamps state/reason. Prefer AdmitCandidateAt when
// a clock is available so decay_park rows get ExpiresAt.
func AdmitCandidate(c Candidate) (Candidate, CandidateAdmission) {
	return AdmitCandidateAt(c, time.Time{})
}

// AdmitCandidateAt is the write-seam admission: knowledge tiers first, then the
// decisions-down disposition for durable memory.
func AdmitCandidateAt(c Candidate, now time.Time) (Candidate, CandidateAdmission) {
	decision := ClassifyCandidateAdmission(c, now)
	if strings.TrimSpace(c.State) == "" || strings.TrimSpace(c.State) == "proposed" {
		c.State = decision.State
	}
	if strings.TrimSpace(c.State) == "" {
		c.State = "proposed"
	}
	if strings.TrimSpace(c.Reason) == "" && c.State == decision.State && strings.TrimSpace(decision.Reason) != "" {
		c.Reason = decision.Reason
	}
	if decision.Disposition == policy.DispositionDecayPark && strings.TrimSpace(c.ExpiresAt) == "" && !now.IsZero() {
		days := policy.DefaultDecayParkDays
		c.ExpiresAt = rfc3339(now.AddDate(0, 0, days))
	}
	return c, decision
}

func ClassifyCandidateAdmission(c Candidate, now time.Time) CandidateAdmission {
	switch strings.TrimSpace(c.Qualifiers["source"]) {
	case "knowledge_import":
		return CandidateAdmission{
			Class:          AdmissionClassSourceMetadata,
			State:          "parked",
			Reason:         "source metadata; source-index tier, not operator approval",
			ReviewRequired: false,
			TargetSurface:  AdmissionSurfaceSourceIndex,
		}
	case "knowledge_extraction":
		return CandidateAdmission{
			Class:          AdmissionClassAnnotatedEvidence,
			State:          "parked",
			Reason:         "knowledge extraction evidence; annotated-evidence tier, not operator approval",
			ReviewRequired: false,
			TargetSurface:  AdmissionSurfaceAnnotatedEvidence,
		}
	}

	disp := policy.ResolveAdjudication(policy.AdjudicationInput{
		Visibility:     c.Visibility,
		ProvenanceLane: c.ProvenanceLane,
		PromotionBasis: c.PromotionBasis,
		ClaimType:      c.ClaimType,
		CanonClass:     c.CanonClass,
		Source:         c.Qualifiers["source"],
		Tags:           c.Tags,
		Object:         c.Object,
	})
	switch disp.Kind {
	case policy.DispositionAutoFile:
		return CandidateAdmission{
			Class:          AdmissionClassDurableMemory,
			State:          "no_promote",
			Reason:         disp.Reason,
			ReviewRequired: false,
			TargetSurface:  AdmissionSurfaceFiled,
			Disposition:    disp.Kind,
		}
	case policy.DispositionDecayPark:
		return CandidateAdmission{
			Class:          AdmissionClassDurableMemory,
			State:          "parked",
			Reason:         disp.Reason,
			ReviewRequired: false,
			TargetSurface:  AdmissionSurfaceDecayPark,
			Disposition:    disp.Kind,
		}
	default:
		return CandidateAdmission{
			Class:          AdmissionClassDurableMemory,
			State:          "proposed",
			Reason:         disp.Reason,
			ReviewRequired: true,
			TargetSurface:  AdmissionSurfaceOperatorQueue,
			Disposition:    policy.DispositionOperatorRequired,
		}
	}
}
