package actuator

import (
	"context"
	"errors"

	"compound/internal/envelope"
	"compound/internal/harness"
	homepkg "compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

const (
	ompPromptOnlyHarness     = "omp-prompt-only"
	ompNativeProducerHarness = "omp-native-verifier"
)

var errOMPPromptOnlyAdmission = errors.New("OMP_PROMPT_ONLY_ADMISSION_INVALID")

func validateOMPPromptOnlyBeforeAdvance(homePath string, claimed envelope.WorkEnvelope, adapter harness.Adapter, in harness.RunInput) error {
	selected, err := exactOMPPromptOnlyAdapter(adapter)
	if err != nil || !selected {
		return err
	}
	if claimed.ID == "" || in.WorkID != claimed.ID {
		return errOMPPromptOnlyAdmission
	}
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return errOMPPromptOnlyAdmission
	}
	current, err := store.Get(claimed.ID)
	if err != nil {
		return errOMPPromptOnlyAdmission
	}
	n, m := len(current.ActionIntentIDs), len(claimed.ActionIntentIDs)
	if current.ID != claimed.ID || current.State != claimed.State ||
		current.PolicyDecisionID != claimed.PolicyDecisionID || n == 0 || m == 0 ||
		current.ActionIntentIDs[n-1] != claimed.ActionIntentIDs[m-1] {
		return errOMPPromptOnlyAdmission
	}
	prepared := in
	prepared.ProofRefs = append([]string(nil), in.ProofRefs...)
	return validateOMPPromptOnlyAdmission(homePath, prepared, "")
}

func prepareOMPPromptOnlyLaunch(in GovernedHarnessLaunchInput, runInput harness.RunInput) (harness.RunInput, error) {
	selected, err := exactOMPPromptOnlyAdapter(in.Adapter)
	if err != nil {
		return harness.RunInput{}, err
	}
	if !selected {
		return runInput, nil
	}
	baseGuard := runInput.StartGuard
	if baseGuard == nil {
		return harness.RunInput{}, errOMPPromptOnlyAdmission
	}
	refs := append([]string(nil), in.RunInput.ProofRefs...)
	prepared := in.RunInput
	prepared.ProofRefs = append([]string(nil), refs...)
	runInput.ProofRefs = append([]string(nil), refs...)
	runInput.ExecutionPosture = &runattempt.ExecutionPosture{
		PermissionMode: "prompt_only", PolicyDecisionID: in.RunningWork.PolicyDecisionID,
		AuthorityEvidence: []runattempt.ExecutionPostureEvidence{{Kind: "omp_prompt_only", Ref: "harness:omp-prompt-only", Revision: "v0.1"}},
	}
	currentActionIntentID := in.ActionIntentID
	runInput.StartGuard = func(ctx context.Context, start func() error) error {
		if start == nil {
			return errOMPPromptOnlyAdmission
		}
		return baseGuard(ctx, func() error {
			if err := validateOMPPromptOnlyAdmission(in.HomePath, prepared, currentActionIntentID); err != nil {
				return err
			}
			return start()
		})
	}
	return runInput, nil
}

func exactOMPPromptOnlyAdapter(adapter harness.Adapter) (bool, error) {
	if typed, ok := adapter.(*harness.OMPPromptOnlyAdapter); ok {
		if typed == nil || typed.Name() != ompPromptOnlyHarness {
			return false, errOMPPromptOnlyAdmission
		}
		return true, nil
	}
	if adapter != nil && adapter.Name() == ompPromptOnlyHarness {
		return false, errOMPPromptOnlyAdmission
	}
	return false, nil
}

func validateOMPPromptOnlyAdmission(homePath string, prepared harness.RunInput, currentActionIntentID string) error {
	if err := harness.ValidateOMPPromptOnlyPreparedInput(prepared); err != nil {
		return errOMPPromptOnlyAdmission
	}
	proofRef, proof, err := selectOMPPromptOnlyNativeProof(homePath, prepared.ProofRefs)
	if err != nil || (currentActionIntentID != "" && proof.ActionIntentID == currentActionIntentID) {
		return errOMPPromptOnlyAdmission
	}
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return errOMPPromptOnlyAdmission
	}
	producer, err := store.LoadGovernedRunAttempt(proof.RunAttemptID)
	if err != nil || !validOMPPromptOnlyProducer(producer, proof, proofRef) {
		return errOMPPromptOnlyAdmission
	}
	if err := harness.ValidateOMPPromptOnlyPreparedProof(homePath, prepared, proof); err != nil {
		return errOMPPromptOnlyAdmission
	}
	return nil
}

func selectOMPPromptOnlyNativeProof(homePath string, refs []string) (string, harness.OMPNativePublicProof, error) {
	reader, err := homepkg.OpenArtifactReader(homePath)
	if err != nil {
		return "", harness.OMPNativePublicProof{}, errOMPPromptOnlyAdmission
	}
	var selectedRef string
	var selected harness.OMPNativePublicProof
	found := 0
	for _, ref := range refs {
		body, err := reader.Get(ref)
		if err != nil {
			continue
		}
		proof, err := harness.DecodeOMPNativePublicProof(body)
		if err != nil {
			continue
		}
		found++
		if found > 1 {
			return "", harness.OMPNativePublicProof{}, errOMPPromptOnlyAdmission
		}
		selectedRef, selected = ref, proof
	}
	if found != 1 {
		return "", harness.OMPNativePublicProof{}, errOMPPromptOnlyAdmission
	}
	return selectedRef, selected, nil
}

func validOMPPromptOnlyProducer(producer runattempt.RunAttempt, proof harness.OMPNativePublicProof, proofRef string) bool {
	if producer.ID != proof.RunAttemptID || producer.WorkID != proof.WorkID ||
		producer.ActionIntentID != proof.ActionIntentID || producer.Origin != runattempt.OriginHarnessExecution ||
		producer.Harness != ompNativeProducerHarness || producer.Status != runattempt.StatusSucceeded ||
		producer.Launch == nil || producer.Launch.PreparedAt == nil || producer.Launch.CommittedAt == nil ||
		producer.Launch.StartErrorAt != nil || producer.ExecutionPosture == nil ||
		producer.ExecutionPosture.PermissionMode != "prompt_only" ||
		len(producer.ExecutionPosture.AuthorityEvidence) != 1 {
		return false
	}
	marker := producer.ExecutionPosture.AuthorityEvidence[0]
	if marker.Kind != "omp_native_verifier" || marker.Ref != "harness:omp-native-verifier" || marker.Revision != "v0.1" {
		return false
	}
	attached := 0
	for _, ref := range producer.ProofRefs {
		if ref == proofRef {
			attached++
		}
	}
	return attached == 1
}
