package reviewartifact

import (
	"fmt"
	"path/filepath"
	"strings"

	"compound/internal/envelope"
	"compound/internal/policy"
)

// ActionAuthoritySeedSpec is filled by the domain producer that owns an
// action's exact capability, target, mutation, recovery, and proof semantics.
// The shared review layer supplies no generic review capability and never
// guesses these fields.
type ActionAuthoritySeedSpec struct {
	WorkID                    string
	CreatedAt                 string
	ProducerName              string
	Actor                     string
	Capability                string
	Action                    string
	Target                    envelope.ActionTarget
	MutationClass             string
	Reversibility             string
	BlastRadius               string
	Preview                   string
	RollbackPlan              string
	Privacy                   string
	Risk                      string
	RunID                     string
	EvidenceRefs              []string
	TargetScope               string
	VerifiedRecoverability    string
	OperatorControlContinuity string
	ExpectedReceipt           string
	ExpectedProof             []string
}

// AuthoritySeedWorkID returns the producer's source work when available, then
// the explicit binding source/fallback. It chooses identity only; liveness and
// terminal fallback selection remain centralized in reviewqueue.
func AuthoritySeedWorkID(item QueueItem, producerSourceWorkID string) string {
	if workID := strings.TrimSpace(producerSourceWorkID); workID != "" {
		return workID
	}
	if item.WorkBinding == nil {
		return ""
	}
	if workID := strings.TrimSpace(item.WorkBinding.SourceWorkID); workID != "" {
		return workID
	}
	return strings.TrimSpace(item.WorkBinding.FallbackWorkID)
}

// SeedActionAuthority attaches a producer-owned ActionIntent and authority
// context. It returns an error instead of attaching a partial seed.
func SeedActionAuthority(action *Action, spec ActionAuthoritySeedSpec) error {
	if action == nil {
		return fmt.Errorf("review action required")
	}
	workID := strings.TrimSpace(spec.WorkID)
	if len(workID) <= len("work_") || !strings.HasPrefix(workID, "work_") ||
		strings.HasPrefix(workID, "work_live_") || filepath.Base(workID) != workID {
		return fmt.Errorf("review action authority requires a bare durable work_ id")
	}
	evidenceRefs := UniqueSorted(spec.EvidenceRefs)
	intent := &envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID: ID("act_",
				action.ID,
				workID,
				strings.TrimSpace(spec.RunID),
				strings.TrimSpace(spec.Capability),
				strings.TrimSpace(spec.Action),
				strings.TrimSpace(spec.Target.Kind),
				strings.TrimSpace(spec.Target.ID),
				strings.TrimSpace(spec.Target.Path),
				strings.TrimSpace(spec.CreatedAt),
			),
			CreatedAt: strings.TrimSpace(spec.CreatedAt),
			Producer: envelope.Producer{
				Kind: "owner_projection",
				Name: FirstNonEmpty(strings.TrimSpace(spec.ProducerName), "compound"),
			},
			IDs: envelope.IDs{
				WorkID: workID,
				RunID:  strings.TrimSpace(spec.RunID),
			},
			Authority:    envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
			Privacy:      FirstNonEmpty(strings.TrimSpace(spec.Privacy), "private"),
			Risk:         FirstNonEmpty(strings.TrimSpace(spec.Risk), "tier1"),
			EvidenceRefs: evidenceRefs,
			Uncertainty:  envelope.Uncertainty{Level: "known"},
		},
		Actor:         FirstNonEmpty(strings.TrimSpace(spec.Actor), "compound"),
		Capability:    strings.TrimSpace(spec.Capability),
		Action:        strings.TrimSpace(spec.Action),
		Target:        spec.Target,
		MutationClass: strings.TrimSpace(spec.MutationClass),
		Reversibility: strings.TrimSpace(spec.Reversibility),
		BlastRadius:   strings.TrimSpace(spec.BlastRadius),
		Preview:       strings.TrimSpace(spec.Preview),
		RollbackPlan:  strings.TrimSpace(spec.RollbackPlan),
	}
	if err := intent.Validate(); err != nil {
		return fmt.Errorf("review action authority intent invalid: %w", err)
	}
	context := &policy.AuthorityContext{
		TargetScope:               FirstNonEmpty(strings.TrimSpace(spec.TargetScope), "private"),
		VerifiedRecoverability:    FirstNonEmpty(strings.TrimSpace(spec.VerifiedRecoverability), policy.RecoverabilityVerified),
		OperatorControlContinuity: FirstNonEmpty(strings.TrimSpace(spec.OperatorControlContinuity), policy.OperatorControlContinuous),
		ExpectedReceipt:           strings.TrimSpace(spec.ExpectedReceipt),
		ExpectedProof:             UniqueSorted(spec.ExpectedProof),
		EvidenceRefs:              evidenceRefs,
	}
	action.ActionIntent = intent
	action.AuthorityContext = context
	return nil
}
