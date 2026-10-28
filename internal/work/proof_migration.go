package work

import (
	"compound/internal/envelope"
	"compound/internal/landlease"
	"compound/internal/policy"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// This migration uses the same exact security approval and audit reconciliation
// as the legacy layout cutover, with a separate preparation journal. A scoped
// legacy layout is never sufficient proof that the class cutover completed.
func (s *Store) proofMigrationPendingPath() string {
	return filepath.Join(s.home, "work", "resource-leases", "proof-domain-preparation.json")
}
func (s *Store) migrateProofCoordinationResources(workID string, in ResourceMigrationInput, backing string, now func() time.Time, window func() error) (landlease.LayoutStatusResult, error) {
	if _, err := resourceProofMigrationRevision(in); err != nil {
		return landlease.LayoutStatusResult{}, err
	}
	if len(in.ProofHomes) == 0 {
		return landlease.LayoutStatusResult{}, fmt.Errorf("explicit participating homes required")
	}
	homes := make([]string, len(in.ProofHomes))
	for i, h := range in.ProofHomes {
		absolute, err := filepath.Abs(h)
		if err != nil {
			return landlease.LayoutStatusResult{}, err
		}
		homes[i], err = filepath.EvalSymlinks(absolute)
		if err != nil {
			return landlease.LayoutStatusResult{}, err
		}
	}
	sort.Strings(homes)
	canonical, err := filepath.EvalSymlinks(s.home)
	if err != nil {
		return landlease.LayoutStatusResult{}, err
	}
	found := false
	for i, h := range homes {
		if i > 0 && homes[i-1] == h {
			return landlease.LayoutStatusResult{}, fmt.Errorf("duplicate participating home")
		}
		found = found || h == canonical
	}
	if !found {
		return landlease.LayoutStatusResult{}, fmt.Errorf("migration owner home must participate")
	}
	in.ProofHomes = homes
	err = s.withLifecycleMutation(func() error {
		w, err := s.resourceMutationOwner(workID, in.Actor)
		if err != nil {
			return err
		}
		if err := window(); err != nil {
			return err
		}
		resumed, history, err := s.resumeProofMigrationLocked(w, in, backing, now, window)
		if resumed || err != nil {
			return err
		}
		historyRef := ""
		if len(history) != 0 {
			historyRef = proofMigrationHistoryRef(history)
		}
		cfg, _, err := policy.LoadConfig(s.home)
		if err != nil {
			return err
		}
		revision := policy.ActionBindingForIntent(envelope.ActionIntent{}, cfg).PolicyRevision
		preview := proofMigrationPreviewWithHistory(in, revision, historyRef)
		action := s.resourceMutationAction(w, in.Actor, "coordination.proof-migrate", preview, backing, now())
		action.MutationClass = "security"
		action.Reversibility = "irreversible"
		action.BlastRadius = "host"
		action.RollbackPlan = "one-way historical fence; keep prepared generation and recover forward under a new current writer pause"
		action.ArgumentsHash, err = workEnvelopeSHA256(w)
		if err != nil {
			return err
		}
		action.EvidenceRefs = append(action.EvidenceRefs, in.EvidenceRef)
		if historyRef != "" {
			action.EvidenceRefs = append(action.EvidenceRefs, historyRef)
		}
		decision, err := s.authorizeResourceMutationLocked(w, action, now())
		if err != nil {
			return err
		}
		pending := resourceMigrationPending{PredecessorPreparationRef: historyRef, SchemaVersion: resourceMigrationPendingSchema, Work: w, Input: in, BackingRoot: backing, Action: action, Decision: decision, PolicyRevision: policy.ActionBindingForIntent(action, cfg).PolicyRevision}
		if err := s.preserveProofMigrationHistory(pending, history); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(s.proofMigrationPendingPath()), 0700); err != nil {
			return err
		}
		if err := s.writeJSON(s.proofMigrationPendingPath(), pending); err != nil {
			return err
		}
		return s.performProofMigrationLocked(w, pending, now, window)
	})
	if err != nil {
		return landlease.LayoutStatusResult{}, err
	}
	return landlease.LayoutStatusResult{Mode: "scoped", BackingRoot: backing, ProofGeneration: landlease.ProofDomainMigrationGeneration(backing, homes)}, nil
}
func (s *Store) performProofMigrationLocked(w envelope.WorkEnvelope, p resourceMigrationPending, now func() time.Time, window func() error) error {
	validate := func() error {
		if err := s.validateProofMigrationEffectAuthority(p, now()); err != nil {
			return err
		}
		if err := window(); err != nil {
			return err
		}
		for _, h := range p.Input.ProofHomes {
			owner, err := OpenReadOnly(h)
			if err != nil {
				return err
			}
			if err := owner.validateResourceMigrationCensus(); err != nil {
				return fmt.Errorf("participating home %s: %w", h, err)
			}
		}
		if err := s.validateProofMigrationEffectAuthority(p, now()); err != nil {
			return err
		}
		return window()
	}
	target, err := resourceProofMigrationRevision(p.Input)
	if err != nil {
		return err
	}
	err = landlease.MigrateProofDomain(p.BackingRoot, p.Input.ProofHomes, validate, s.resourceMigrationFault, target)
	if err != nil {
		status, statusErr := landlease.ReadProofDomainStatus(p.BackingRoot)
		if statusErr != nil || status.Phase != "" {
			return s.resourceMigrationRecoveryError(p, errors.Join(err, statusErr))
		}
		outcomeErr := s.finishResourceMutationLocked(w, p.Action, p.Decision, err, now())
		return errors.Join(outcomeErr, os.Remove(s.proofMigrationPendingPath()))
	}
	if s.resourceMigrationFault != nil {
		if err := s.resourceMigrationFault("published"); err != nil {
			return s.resourceMigrationRecoveryError(p, err)
		}
	}
	if err := s.finishResourceMigrationLocked(w, p, now()); err != nil {
		return s.resourceMigrationRecoveryError(p, err)
	}
	return nil
}

func proofMigrationPreview(in ResourceMigrationInput, revision string) string {
	return proofMigrationPreviewWithHistory(in, revision, "")
}

func proofMigrationPreviewWithHistory(in ResourceMigrationInput, revision, historyRef string) string {
	body, _ := json.Marshal(struct {
		Input          ResourceMigrationInput
		PolicyRevision string
	}{in, revision})
	preview := "activate two source-derived proof capacities; permanently fence historical shared endpoint; inputs=" + string(body)
	if historyRef != "" {
		preview += "; predecessor_preparation=" + historyRef
	}
	return preview
}

func (s *Store) validateProofMigrationEffectAuthority(p resourceMigrationPending, now time.Time) error {
	if err := policy.NewExactActionApprovalStore(s.home).ValidateConsumedDecision(p.Action, p.Decision); err != nil {
		return err
	}
	if err := validatePreEvaluatedReceiptedActionAt(p.Action, p.Decision, now); err != nil {
		return err
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return err
	}
	if policy.ActionBindingForIntent(p.Action, cfg).PolicyRevision != p.PolicyRevision {
		return fmt.Errorf("proof migration policy changed; fresh exact approval required")
	}
	for _, class := range []string{"build-implement", p.Action.Capability} {
		halted, reason, err := policy.AutonomyHalted(s.home, class)
		if err != nil {
			return err
		}
		if halted {
			return fmt.Errorf("proof migration halted: %s", reason)
		}
	}
	return nil
}

func resourceProofMigrationRevision(in ResourceMigrationInput) (landlease.ProofDomainRevision, error) {
	if in.ProofTargetRevision == "" {
		return landlease.ProofDomainLocalV2, nil
	}
	if in.ProofTargetRevision != landlease.ProofDomainLocalV2 && in.ProofTargetRevision != landlease.ProofDomainFencedV3 {
		return "", fmt.Errorf("unknown proof domain target revision %q", in.ProofTargetRevision)
	}
	return in.ProofTargetRevision, nil
}
