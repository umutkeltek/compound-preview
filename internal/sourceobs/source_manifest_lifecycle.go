package sourceobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const SourceManifestLifecycleAdvanceSchemaVersion = "compound.source_manifest_lifecycle_advance.v0.1"

const SourceLifecycleEvidenceBindingSchemaVersion = "compound.source_lifecycle_evidence_binding.v0.1"

const (
	SourceLifecycleRegistered            = "registered"
	SourceLifecycleCapturing             = "capturing"
	SourceLifecycleOwnedSnapshot         = "owned_snapshot"
	SourceLifecycleVerified              = "verified"
	SourceLifecycleCompoundPrimaryWriter = "compound_primary_writer"
	SourceLifecycleLegacyFrozen          = "legacy_frozen"
	SourceLifecycleQuietWindowProven     = "quiet_window_proven"
	SourceLifecycleArchived              = "archived"
	SourceLifecycleRetired               = "retired"
)

const (
	SourceLifecycleGateSourceInventory                  = "source_inventory"
	SourceLifecycleGateExactCustody                     = "exact_custody"
	SourceLifecycleGateIsolatedRestore                  = "isolated_restore"
	SourceLifecycleGateSemanticParity                   = "semantic_parity"
	SourceLifecycleGateRealConsumerReplay               = "real_consumer_replay"
	SourceLifecycleGateLegacyUnavailableNegativeControl = "legacy_unavailable_negative_control"
	SourceLifecycleGateWriterRoster                     = "writer_roster"
	SourceLifecycleGateWriterDestination                = "writer_destination"
	SourceLifecycleGateRollbackRehearsal                = "rollback_rehearsal"
	SourceLifecycleGatePrimaryWriterCutover             = "primary_writer_cutover"
	SourceLifecycleGateLegacyFreeze                     = "legacy_freeze"
	SourceLifecycleGateQuietWindowMeasurement           = "quiet_window_measurement"
	SourceLifecycleGateArchiveVerified                  = "archive_verified"
	SourceLifecycleGateOperatorRetirementDecision       = "operator_retirement_decision"
	SourceLifecycleGateCaptureAbandoned                 = "capture_abandoned"
	SourceLifecycleGateCustodyInvalidated               = "custody_invalidated"
	SourceLifecycleGateVerificationInvalidated          = "verification_invalidated"
	SourceLifecycleGateWriterRollback                   = "writer_rollback"
	SourceLifecycleGateLegacyUnfreeze                   = "legacy_unfreeze"
	SourceLifecycleGateQuietWindowInvalidated           = "quiet_window_invalidated"
	SourceLifecycleGateArchiveRollback                  = "archive_rollback"
)

type SourceManifestLifecycle struct {
	State         string                      `json:"state"`
	Revision      int64                       `json:"revision"`
	WriterHandoff *SourceWriterHandoff        `json:"writer_handoff,omitempty"`
	History       []SourceLifecycleTransition `json:"history"`
}

type SourceWriterHandoff struct {
	LegacyWriterRefs           []string `json:"legacy_writer_refs"`
	CompoundWriterRef          string   `json:"compound_writer_ref"`
	RollbackRef                string   `json:"rollback_ref"`
	FenceRef                   string   `json:"fence_ref"`
	WriterEpoch                int64    `json:"writer_epoch"`
	RequiredQuietWindowSeconds int64    `json:"required_quiet_window_seconds"`
}

type SourceLifecycleProof struct {
	Gate            string                          `json:"gate"`
	EvidenceRef     string                          `json:"evidence_ref"`
	SubjectRef      string                          `json:"subject_ref"`
	RecordedAt      string                          `json:"recorded_at"`
	QuietWindow     *SourceQuietWindowMeasurement   `json:"quiet_window,omitempty"`
	EvidenceBinding *SourceLifecycleEvidenceBinding `json:"evidence_binding,omitempty"`
}

type SourceLifecycleEvidenceBinding struct {
	SchemaVersion        string `json:"schema_version"`
	EvidenceWorkID       string `json:"evidence_work_id"`
	EvidenceWorkSHA256   string `json:"evidence_work_sha256"`
	ReceiptID            string `json:"receipt_id"`
	ReceiptSHA256        string `json:"receipt_sha256"`
	TransitionID         string `json:"transition_id"`
	TransitionSHA256     string `json:"transition_sha256"`
	CanonicalSurface     string `json:"canonical_surface"`
	RequiredGateProofRef string `json:"required_gate_proof_ref"`
}

type SourceQuietWindowMeasurement struct {
	StartedAt         string `json:"started_at"`
	EndedAt           string `json:"ended_at"`
	ObservedReads     int64  `json:"observed_reads"`
	ObservedWrites    int64  `json:"observed_writes"`
	UnexplainedReads  int64  `json:"unexplained_reads"`
	UnexplainedWrites int64  `json:"unexplained_writes"`
}

type SourceLifecycleTransition struct {
	ID               string                 `json:"id"`
	FromState        string                 `json:"from_state"`
	ToState          string                 `json:"to_state"`
	OccurredAt       string                 `json:"occurred_at"`
	Actor            string                 `json:"actor"`
	WorkID           string                 `json:"work_id"`
	ActionIntentID   string                 `json:"action_intent_id"`
	PolicyDecisionID string                 `json:"policy_decision_id"`
	Proofs           []SourceLifecycleProof `json:"proofs"`
	WriterHandoff    *SourceWriterHandoff   `json:"writer_handoff,omitempty"`
}

type SourceManifestLifecycleAdvanceRequest struct {
	ToState          string
	ExpectedRevision int64
	WorkID           string
	Actor            string
	ActionIntentID   string
	PolicyDecisionID string
	Proofs           []SourceLifecycleProof
	WriterHandoff    *SourceWriterHandoff
}

type SourceManifestLifecycleAdvanceReport struct {
	SchemaVersion string                    `json:"schema_version"`
	Applied       bool                      `json:"applied"`
	SourceFamily  string                    `json:"source_family"`
	ManifestRef   string                    `json:"manifest_ref"`
	FromState     string                    `json:"from_state"`
	ToState       string                    `json:"to_state"`
	Revision      int64                     `json:"revision"`
	Transition    SourceLifecycleTransition `json:"transition"`
	Status        SourceLifecycleStatus     `json:"status"`
	Lifecycle     SourceManifestLifecycle   `json:"lifecycle"`
}

type SourceLifecycleStatus struct {
	State             string   `json:"state"`
	Revision          int64    `json:"revision"`
	Terminal          bool     `json:"terminal"`
	NextState         string   `json:"next_state,omitempty"`
	RequiredGates     []string `json:"required_gates,omitempty"`
	MissingGates      []string `json:"missing_gates,omitempty"`
	NextSafeAction    string   `json:"next_safe_action"`
	RollbackAvailable bool     `json:"rollback_available"`
	RollbackTarget    string   `json:"rollback_target,omitempty"`
	RollbackGate      string   `json:"rollback_gate,omitempty"`
	ReadPosture       string   `json:"read_posture"`
	WritePosture      string   `json:"write_posture"`
}

type sourceLifecycleEdge struct {
	from    string
	to      string
	gates   []string
	forward bool
}

var sourceLifecycleEdges = []sourceLifecycleEdge{
	{SourceLifecycleRegistered, SourceLifecycleCapturing, []string{SourceLifecycleGateSourceInventory}, true},
	{SourceLifecycleCapturing, SourceLifecycleOwnedSnapshot, []string{SourceLifecycleGateExactCustody}, true},
	{SourceLifecycleOwnedSnapshot, SourceLifecycleVerified, []string{SourceLifecycleGateIsolatedRestore, SourceLifecycleGateSemanticParity, SourceLifecycleGateRealConsumerReplay, SourceLifecycleGateLegacyUnavailableNegativeControl}, true},
	{SourceLifecycleVerified, SourceLifecycleCompoundPrimaryWriter, []string{SourceLifecycleGateWriterRoster, SourceLifecycleGateWriterDestination, SourceLifecycleGateRollbackRehearsal, SourceLifecycleGatePrimaryWriterCutover}, true},
	{SourceLifecycleCompoundPrimaryWriter, SourceLifecycleLegacyFrozen, []string{SourceLifecycleGateLegacyFreeze}, true},
	{SourceLifecycleLegacyFrozen, SourceLifecycleQuietWindowProven, []string{SourceLifecycleGateQuietWindowMeasurement}, true},
	{SourceLifecycleQuietWindowProven, SourceLifecycleArchived, []string{SourceLifecycleGateArchiveVerified, SourceLifecycleGateRollbackRehearsal}, true},
	{SourceLifecycleArchived, SourceLifecycleRetired, []string{SourceLifecycleGateOperatorRetirementDecision}, true},
	{SourceLifecycleCapturing, SourceLifecycleRegistered, []string{SourceLifecycleGateCaptureAbandoned}, false},
	{SourceLifecycleOwnedSnapshot, SourceLifecycleCapturing, []string{SourceLifecycleGateCustodyInvalidated}, false},
	{SourceLifecycleVerified, SourceLifecycleOwnedSnapshot, []string{SourceLifecycleGateVerificationInvalidated}, false},
	{SourceLifecycleCompoundPrimaryWriter, SourceLifecycleVerified, []string{SourceLifecycleGateWriterRollback}, false},
	{SourceLifecycleLegacyFrozen, SourceLifecycleCompoundPrimaryWriter, []string{SourceLifecycleGateLegacyUnfreeze}, false},
	{SourceLifecycleQuietWindowProven, SourceLifecycleLegacyFrozen, []string{SourceLifecycleGateQuietWindowInvalidated}, false},
	{SourceLifecycleArchived, SourceLifecycleQuietWindowProven, []string{SourceLifecycleGateArchiveRollback}, false},
}

func defaultSourceManifestLifecycle() *SourceManifestLifecycle {
	return &SourceManifestLifecycle{State: SourceLifecycleRegistered, Revision: 0, History: []SourceLifecycleTransition{}}
}

func SourceManifestLifecycleStatus(manifest SourceManifest) SourceLifecycleStatus {
	lifecycle := manifest.Lifecycle
	if lifecycle == nil {
		lifecycle = defaultSourceManifestLifecycle()
	}
	status := SourceLifecycleStatus{
		State:        lifecycle.State,
		Revision:     lifecycle.Revision,
		Terminal:     lifecycle.State == SourceLifecycleRetired,
		ReadPosture:  sourceLifecycleReadPosture(lifecycle.State),
		WritePosture: sourceLifecycleWritePosture(lifecycle.State),
	}
	if status.Terminal {
		status.NextSafeAction = "terminal: retain archive/evidence; a reactivation requires a new governed source-family lifecycle"
		return status
	}
	if edge, ok := forwardSourceLifecycleEdge(lifecycle.State); ok {
		status.NextState = edge.to
		status.RequiredGates = append([]string{}, edge.gates...)
		status.MissingGates = append([]string{}, edge.gates...)
		status.NextSafeAction = fmt.Sprintf("collect [%s], then dry-run compound source manifest-advance %s --to %s", strings.Join(edge.gates, ", "), manifest.SourceFamily, edge.to)
	} else {
		status.NextSafeAction = "no forward lifecycle edge is defined"
	}
	if edge, ok := rollbackSourceLifecycleEdge(lifecycle.State); ok {
		status.RollbackAvailable = true
		status.RollbackTarget = edge.to
		if len(edge.gates) > 0 {
			status.RollbackGate = edge.gates[0]
		}
	}
	return status
}

func PlanSourceManifestLifecycleAdvance(homePath, sourceFamily string, req SourceManifestLifecycleAdvanceRequest, now time.Time) (SourceManifestLifecycleAdvanceReport, error) {
	manifest, err := LoadSourceManifest(homePath, sourceFamily)
	if err != nil {
		return SourceManifestLifecycleAdvanceReport{}, err
	}
	return planSourceManifestLifecycleAdvance(homePath, manifest, req, now)
}

func AdvanceSourceManifestLifecycle(homePath, sourceFamily string, req SourceManifestLifecycleAdvanceRequest, now time.Time) (SourceManifestLifecycleAdvanceReport, error) {
	var report SourceManifestLifecycleAdvanceReport
	err := withSourceManifestAuthorityLock(homePath, func() error {
		return withSourceManifestLifecycleLock(homePath, sourceFamily, func() error {
			manifest, err := LoadSourceManifest(homePath, sourceFamily)
			if err != nil {
				return err
			}
			report, err = planSourceManifestLifecycleAdvance(homePath, manifest, req, now)
			if err != nil {
				return err
			}
			manifest.SchemaVersion = SourceManifestSchemaVersion
			manifest.AuthorityRef = SourceManifestAuthorityRef(sourceFamily)
			manifest.Lifecycle = cloneSourceManifestLifecycle(&report.Lifecycle)
			if err := validateSourceManifest(manifest); err != nil {
				return fmt.Errorf("validate source lifecycle publication: %w", err)
			}
			body, err := json.MarshalIndent(manifest, "", "  ")
			if err != nil {
				return err
			}
			if err := writeSourceManifestAtomic(SourceManifestPath(homePath, sourceFamily), append(body, '\n')); err != nil {
				return err
			}
			report.Applied = true
			return nil
		})
	})
	return report, err
}

func planSourceManifestLifecycleAdvance(homePath string, manifest SourceManifest, req SourceManifestLifecycleAdvanceRequest, now time.Time) (SourceManifestLifecycleAdvanceReport, error) {
	if now.IsZero() {
		now = time.Now()
	}
	lifecycle := manifest.Lifecycle
	if lifecycle == nil {
		lifecycle = defaultSourceManifestLifecycle()
	}
	if req.ExpectedRevision != lifecycle.Revision {
		return SourceManifestLifecycleAdvanceReport{}, fmt.Errorf("source lifecycle revision changed: expected %d, current %d", req.ExpectedRevision, lifecycle.Revision)
	}
	if lifecycle.State == SourceLifecycleRetired {
		return SourceManifestLifecycleAdvanceReport{}, fmt.Errorf("source lifecycle is terminal at retired; register a new governed lifecycle to reactivate")
	}
	edge, ok := findSourceLifecycleEdge(lifecycle.State, strings.TrimSpace(req.ToState))
	if !ok {
		return SourceManifestLifecycleAdvanceReport{}, fmt.Errorf("illegal source lifecycle transition %s -> %s", lifecycle.State, strings.TrimSpace(req.ToState))
	}
	if strings.TrimSpace(req.WorkID) == "" || strings.TrimSpace(req.Actor) == "" || strings.TrimSpace(req.ActionIntentID) == "" || strings.TrimSpace(req.PolicyDecisionID) == "" {
		return SourceManifestLifecycleAdvanceReport{}, fmt.Errorf("source lifecycle transition requires work_id, actor, action_intent_id, and policy_decision_id")
	}
	proofs, err := normalizeSourceLifecycleProofs(manifest.AuthorityRef, req.Proofs, edge.gates, now)
	if err != nil {
		return SourceManifestLifecycleAdvanceReport{}, err
	}
	proofs, err = bindSourceLifecycleEvidence(homePath, manifest, proofs)
	if err != nil {
		return SourceManifestLifecycleAdvanceReport{}, err
	}
	if edge.forward && sourceLifecycleRank(edge.to) >= sourceLifecycleRank(SourceLifecycleOwnedSnapshot) {
		doctor, err := DoctorSourceManifest(homePath, manifest.SourceFamily)
		if err != nil {
			return SourceManifestLifecycleAdvanceReport{}, err
		}
		if !doctor.CustodyComplete || doctor.HashMismatchCount != 0 || doctor.MissingFieldCount != 0 || doctor.LifecycleEvidenceMissingCount != 0 || doctor.LifecycleEvidenceMismatchCount != 0 {
			return SourceManifestLifecycleAdvanceReport{}, fmt.Errorf("source lifecycle %s requires current exact custody and bound lifecycle evidence; doctor custody_complete=%t missing=%d mismatch=%d evidence_missing=%d evidence_mismatch=%d", edge.to, doctor.CustodyComplete, doctor.MissingFieldCount, doctor.HashMismatchCount, doctor.LifecycleEvidenceMissingCount, doctor.LifecycleEvidenceMismatchCount)
		}
	}
	nextHandoff := cloneSourceWriterHandoff(lifecycle.WriterHandoff)
	if edge.from == SourceLifecycleVerified && edge.to == SourceLifecycleCompoundPrimaryWriter {
		if err := validateSourceWriterHandoff(req.WriterHandoff, lifecycle.WriterHandoff); err != nil {
			return SourceManifestLifecycleAdvanceReport{}, err
		}
		nextHandoff = cloneSourceWriterHandoff(req.WriterHandoff)
	} else if req.WriterHandoff != nil {
		return SourceManifestLifecycleAdvanceReport{}, fmt.Errorf("writer_handoff is accepted only for verified -> compound_primary_writer")
	}
	if edge.forward && edge.to == SourceLifecycleQuietWindowProven {
		if err := validateSourceQuietWindow(proofs, nextHandoff); err != nil {
			return SourceManifestLifecycleAdvanceReport{}, err
		}
	}
	if edge.to == SourceLifecycleRetired {
		if !strings.HasPrefix(strings.TrimSpace(req.Actor), "operator:") {
			return SourceManifestLifecycleAdvanceReport{}, fmt.Errorf("retirement requires an explicit operator actor")
		}
		if !strings.HasPrefix(proofs[0].EvidenceRef, "decision://") {
			return SourceManifestLifecycleAdvanceReport{}, fmt.Errorf("operator_retirement_decision evidence_ref must be decision://...")
		}
	}
	transition := SourceLifecycleTransition{
		ID:               idFor("srclife_", manifest.SourceFamily, lifecycle.State, edge.to, fmt.Sprintf("%d", lifecycle.Revision+1), now.UTC().Format(time.RFC3339Nano)),
		FromState:        lifecycle.State,
		ToState:          edge.to,
		OccurredAt:       now.UTC().Format(time.RFC3339),
		Actor:            strings.TrimSpace(req.Actor),
		WorkID:           strings.TrimSpace(req.WorkID),
		ActionIntentID:   strings.TrimSpace(req.ActionIntentID),
		PolicyDecisionID: strings.TrimSpace(req.PolicyDecisionID),
		Proofs:           proofs,
	}
	if edge.from == SourceLifecycleVerified && edge.to == SourceLifecycleCompoundPrimaryWriter {
		transition.WriterHandoff = cloneSourceWriterHandoff(nextHandoff)
	}
	next := cloneSourceManifestLifecycle(lifecycle)
	next.State = edge.to
	next.Revision++
	next.WriterHandoff = nextHandoff
	next.History = append(next.History, transition)
	postManifest := manifest
	postManifest.Lifecycle = next
	report := SourceManifestLifecycleAdvanceReport{
		SchemaVersion: SourceManifestLifecycleAdvanceSchemaVersion,
		SourceFamily:  manifest.SourceFamily,
		ManifestRef:   manifest.AuthorityRef,
		FromState:     lifecycle.State,
		ToState:       edge.to,
		Revision:      next.Revision,
		Transition:    transition,
		Status:        SourceManifestLifecycleStatus(postManifest),
		Lifecycle:     *next,
	}
	return report, nil
}

func validateSourceManifestLifecycle(manifest SourceManifest) error {
	if manifest.Lifecycle == nil {
		if manifest.SchemaVersion == SourceManifestSchemaVersion {
			return fmt.Errorf("source manifest %s requires lifecycle", SourceManifestSchemaVersion)
		}
		return nil
	}
	lifecycle := manifest.Lifecycle
	if sourceLifecycleRank(lifecycle.State) < 0 {
		return fmt.Errorf("unknown source lifecycle state %q", lifecycle.State)
	}
	if lifecycle.Revision < 0 || int64(len(lifecycle.History)) != lifecycle.Revision {
		return fmt.Errorf("source lifecycle revision/history mismatch: revision=%d history=%d", lifecycle.Revision, len(lifecycle.History))
	}
	state := SourceLifecycleRegistered
	var handoff *SourceWriterHandoff
	seen := map[string]bool{}
	for i, transition := range lifecycle.History {
		if strings.TrimSpace(transition.ID) == "" || seen[transition.ID] {
			return fmt.Errorf("source lifecycle history[%d] has missing/duplicate id", i)
		}
		seen[transition.ID] = true
		if transition.FromState != state {
			return fmt.Errorf("source lifecycle history[%d] starts at %s, want %s", i, transition.FromState, state)
		}
		edge, ok := findSourceLifecycleEdge(transition.FromState, transition.ToState)
		if !ok {
			return fmt.Errorf("source lifecycle history[%d] has illegal transition %s -> %s", i, transition.FromState, transition.ToState)
		}
		if strings.TrimSpace(transition.OccurredAt) == "" || strings.TrimSpace(transition.Actor) == "" || strings.TrimSpace(transition.WorkID) == "" || strings.TrimSpace(transition.ActionIntentID) == "" || strings.TrimSpace(transition.PolicyDecisionID) == "" {
			return fmt.Errorf("source lifecycle history[%d] lacks governance identity", i)
		}
		normalizedProofs, err := normalizeSourceLifecycleProofs(manifest.AuthorityRef, transition.Proofs, edge.gates, time.Time{})
		if err != nil {
			return fmt.Errorf("source lifecycle history[%d]: %w", i, err)
		}
		if err := validateStoredSourceLifecycleEvidenceBindings(manifest.SourceFamily, normalizedProofs); err != nil {
			return fmt.Errorf("source lifecycle history[%d]: %w", i, err)
		}
		if edge.from == SourceLifecycleVerified && edge.to == SourceLifecycleCompoundPrimaryWriter {
			if err := validateSourceWriterHandoff(transition.WriterHandoff, handoff); err != nil {
				return fmt.Errorf("source lifecycle history[%d]: %w", i, err)
			}
			handoff = cloneSourceWriterHandoff(transition.WriterHandoff)
		} else if transition.WriterHandoff != nil {
			return fmt.Errorf("source lifecycle history[%d] carries writer_handoff outside writer cutover", i)
		}
		if edge.forward && edge.to == SourceLifecycleQuietWindowProven {
			if err := validateSourceQuietWindow(transition.Proofs, handoff); err != nil {
				return fmt.Errorf("source lifecycle history[%d]: %w", i, err)
			}
		}
		if edge.to == SourceLifecycleRetired {
			if !strings.HasPrefix(transition.Actor, "operator:") || !strings.HasPrefix(transition.Proofs[0].EvidenceRef, "decision://") {
				return fmt.Errorf("source lifecycle history[%d] retirement lacks operator decision", i)
			}
		}
		state = transition.ToState
	}
	if lifecycle.State != state {
		return fmt.Errorf("source lifecycle state %s does not match history tip %s", lifecycle.State, state)
	}
	if !equalSourceWriterHandoff(lifecycle.WriterHandoff, handoff) {
		return fmt.Errorf("source lifecycle writer_handoff does not match history tip")
	}
	return nil
}

func normalizeSourceLifecycleProofs(subjectRef string, proofs []SourceLifecycleProof, required []string, now time.Time) ([]SourceLifecycleProof, error) {
	want := append([]string{}, required...)
	got := make([]string, 0, len(proofs))
	byGate := map[string]SourceLifecycleProof{}
	for _, proof := range proofs {
		proof.Gate = strings.TrimSpace(proof.Gate)
		proof.EvidenceRef = strings.TrimSpace(proof.EvidenceRef)
		if proof.Gate == "" || proof.EvidenceRef == "" || byGate[proof.Gate].Gate != "" {
			got = append(got, proof.Gate)
			continue
		}
		proof.SubjectRef = strings.TrimSpace(proof.SubjectRef)
		if proof.SubjectRef == "" {
			proof.SubjectRef = subjectRef
		}
		if proof.SubjectRef != subjectRef {
			return nil, fmt.Errorf("proof %s subject_ref %q does not match %q", proof.Gate, proof.SubjectRef, subjectRef)
		}
		proof.RecordedAt = strings.TrimSpace(proof.RecordedAt)
		if proof.RecordedAt == "" && !now.IsZero() {
			proof.RecordedAt = now.UTC().Format(time.RFC3339)
		}
		if proof.RecordedAt == "" {
			return nil, fmt.Errorf("proof %s requires recorded_at", proof.Gate)
		}
		got = append(got, proof.Gate)
		byGate[proof.Gate] = proof
	}
	if len(got) != len(want) {
		return nil, fmt.Errorf("source lifecycle transition requires exactly [%s] proof gates; got [%s]", strings.Join(want, " "), strings.Join(got, " "))
	}
	normalized := make([]SourceLifecycleProof, 0, len(want))
	for _, gate := range want {
		proof, ok := byGate[gate]
		if !ok {
			return nil, fmt.Errorf("source lifecycle transition requires exactly [%s] proof gates; got [%s]", strings.Join(want, " "), strings.Join(got, " "))
		}
		normalized = append(normalized, proof)
	}
	return normalized, nil
}

func validateSourceWriterHandoff(handoff, previous *SourceWriterHandoff) error {
	if handoff == nil {
		return fmt.Errorf("writer_handoff required")
	}
	if len(nonEmptyStrings(handoff.LegacyWriterRefs)) == 0 || strings.TrimSpace(handoff.CompoundWriterRef) == "" || strings.TrimSpace(handoff.RollbackRef) == "" || strings.TrimSpace(handoff.FenceRef) == "" {
		return fmt.Errorf("writer_handoff requires legacy_writer_refs, compound_writer_ref, rollback_ref, and fence_ref")
	}
	if handoff.WriterEpoch <= 0 || handoff.RequiredQuietWindowSeconds <= 0 {
		return fmt.Errorf("writer_handoff requires writer_epoch > 0 and required_quiet_window_seconds > 0")
	}
	if previous != nil && handoff.WriterEpoch <= previous.WriterEpoch {
		return fmt.Errorf("writer_handoff writer_epoch must advance beyond %d", previous.WriterEpoch)
	}
	return nil
}

func validateSourceQuietWindow(proofs []SourceLifecycleProof, handoff *SourceWriterHandoff) error {
	if handoff == nil || handoff.RequiredQuietWindowSeconds <= 0 {
		return fmt.Errorf("quiet window requires a prior writer_handoff duration")
	}
	var measurement *SourceQuietWindowMeasurement
	for _, proof := range proofs {
		if proof.Gate == SourceLifecycleGateQuietWindowMeasurement {
			measurement = proof.QuietWindow
			break
		}
	}
	if measurement == nil {
		return fmt.Errorf("quiet_window_measurement requires typed window fields")
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(measurement.StartedAt))
	if err != nil {
		return fmt.Errorf("quiet window started_at: %w", err)
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(measurement.EndedAt))
	if err != nil {
		return fmt.Errorf("quiet window ended_at: %w", err)
	}
	if !end.After(start) || int64(end.Sub(start).Seconds()) < handoff.RequiredQuietWindowSeconds {
		return fmt.Errorf("quiet window is shorter than required %ds", handoff.RequiredQuietWindowSeconds)
	}
	if measurement.ObservedReads < 0 || measurement.ObservedWrites < 0 || measurement.UnexplainedReads < 0 || measurement.UnexplainedWrites < 0 {
		return fmt.Errorf("quiet window counts cannot be negative")
	}
	if measurement.UnexplainedReads != 0 || measurement.UnexplainedWrites != 0 {
		return fmt.Errorf("quiet window has unexplained legacy traffic: reads=%d writes=%d", measurement.UnexplainedReads, measurement.UnexplainedWrites)
	}
	return nil
}

func findSourceLifecycleEdge(from, to string) (sourceLifecycleEdge, bool) {
	for _, edge := range sourceLifecycleEdges {
		if edge.from == from && edge.to == to {
			return edge, true
		}
	}
	return sourceLifecycleEdge{}, false
}

func forwardSourceLifecycleEdge(from string) (sourceLifecycleEdge, bool) {
	for _, edge := range sourceLifecycleEdges {
		if edge.from == from && edge.forward {
			return edge, true
		}
	}
	return sourceLifecycleEdge{}, false
}

func rollbackSourceLifecycleEdge(from string) (sourceLifecycleEdge, bool) {
	for _, edge := range sourceLifecycleEdges {
		if edge.from == from && !edge.forward {
			return edge, true
		}
	}
	return sourceLifecycleEdge{}, false
}

func sourceLifecycleRank(state string) int {
	states := []string{SourceLifecycleRegistered, SourceLifecycleCapturing, SourceLifecycleOwnedSnapshot, SourceLifecycleVerified, SourceLifecycleCompoundPrimaryWriter, SourceLifecycleLegacyFrozen, SourceLifecycleQuietWindowProven, SourceLifecycleArchived, SourceLifecycleRetired}
	for i, candidate := range states {
		if state == candidate {
			return i
		}
	}
	return -1
}

func sourceLifecycleReadPosture(state string) string {
	switch state {
	case SourceLifecycleRegistered, SourceLifecycleCapturing:
		return "legacy_primary_compound_inventory"
	case SourceLifecycleOwnedSnapshot, SourceLifecycleVerified:
		return "compound_recovery_legacy_available"
	case SourceLifecycleCompoundPrimaryWriter:
		return "compound_primary_legacy_available"
	case SourceLifecycleLegacyFrozen, SourceLifecycleQuietWindowProven:
		return "compound_primary_legacy_read_only"
	case SourceLifecycleArchived:
		return "compound_primary_archive_rollback"
	case SourceLifecycleRetired:
		return "compound_primary_archive_evidence"
	default:
		return "unknown"
	}
}

func sourceLifecycleWritePosture(state string) string {
	switch state {
	case SourceLifecycleRegistered, SourceLifecycleCapturing, SourceLifecycleOwnedSnapshot, SourceLifecycleVerified:
		return "legacy_authoritative"
	case SourceLifecycleCompoundPrimaryWriter:
		return "compound_authoritative_legacy_writable_for_rollback"
	case SourceLifecycleLegacyFrozen, SourceLifecycleQuietWindowProven:
		return "compound_authoritative_legacy_frozen"
	case SourceLifecycleArchived:
		return "compound_authoritative_legacy_archived"
	case SourceLifecycleRetired:
		return "compound_authoritative_legacy_retired"
	default:
		return "unknown"
	}
}

func cloneSourceManifestLifecycle(in *SourceManifestLifecycle) *SourceManifestLifecycle {
	if in == nil {
		return defaultSourceManifestLifecycle()
	}
	out := *in
	out.WriterHandoff = cloneSourceWriterHandoff(in.WriterHandoff)
	out.History = append([]SourceLifecycleTransition{}, in.History...)
	for i := range out.History {
		out.History[i].Proofs = append([]SourceLifecycleProof{}, in.History[i].Proofs...)
		for j := range out.History[i].Proofs {
			if in.History[i].Proofs[j].QuietWindow != nil {
				copyWindow := *in.History[i].Proofs[j].QuietWindow
				out.History[i].Proofs[j].QuietWindow = &copyWindow
			}
			if in.History[i].Proofs[j].EvidenceBinding != nil {
				copyBinding := *in.History[i].Proofs[j].EvidenceBinding
				out.History[i].Proofs[j].EvidenceBinding = &copyBinding
			}
		}
		out.History[i].WriterHandoff = cloneSourceWriterHandoff(in.History[i].WriterHandoff)
	}
	return &out
}

func cloneSourceWriterHandoff(in *SourceWriterHandoff) *SourceWriterHandoff {
	if in == nil {
		return nil
	}
	out := *in
	out.LegacyWriterRefs = append([]string{}, in.LegacyWriterRefs...)
	return &out
}

func withSourceManifestAuthorityLock(homePath string, fn func() error) error {
	if strings.TrimSpace(homePath) == "" {
		return fmt.Errorf("COMPOUND_HOME required")
	}
	lockPath := filepath.Join(homePath, "lock", "source-manifest-authority.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func withSourceManifestLifecycleLock(homePath, sourceFamily string, fn func() error) error {
	if strings.TrimSpace(homePath) == "" {
		return fmt.Errorf("COMPOUND_HOME required")
	}
	lockPath := filepath.Join(homePath, "lock", "source-manifest-"+safeManifestName(sourceFamily)+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func equalSourceWriterHandoff(a, b *SourceWriterHandoff) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.CompoundWriterRef != b.CompoundWriterRef || a.RollbackRef != b.RollbackRef || a.FenceRef != b.FenceRef || a.WriterEpoch != b.WriterEpoch || a.RequiredQuietWindowSeconds != b.RequiredQuietWindowSeconds {
		return false
	}
	aa, bb := nonEmptyStrings(a.LegacyWriterRefs), nonEmptyStrings(b.LegacyWriterRefs)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func nonEmptyStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
