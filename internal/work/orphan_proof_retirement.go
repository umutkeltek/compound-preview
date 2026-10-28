package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

const orphanProofRetirementSchema = "compound.orphan_proof_retirement.v0.1"

// OrphanProofRetirementPlan binds one uncommitted failed completion to its
// predecessor Work and producer attempt. It is a read-only proposal.
type OrphanProofRetirementPlan struct {
	SchemaVersion         string
	RecoveryWorkID        string
	ReservationID         string
	PredecessorWorkID     string
	RunAttemptID          string
	JournalSHA256         string
	PredecessorWorkSHA256 string
	RecoveryWorkSHA256    string
	RunAttemptSHA256      string
	CompletionID          string
	ResultID              string
	OriginalHolder        string
	ArchivePath           string
	DecisionPath          string
}

type OrphanProofRetirementInput struct {
	Actor         string
	Operator      string
	EvidenceRef   string
	WritersPaused bool
	ExpiresAt     time.Time
	Plan          OrphanProofRetirementPlan
}

type OrphanProofRetirementDecision struct {
	SchemaVersion           string
	RecoveryWorkID          string
	PredecessorWorkID       string
	ReservationID           string
	RunAttemptID            string
	JournalSHA256           string
	ArchivePath             string
	FailedResultUncertified bool
	ActionIntentID          string
	PolicyDecisionID        string
	DecidedAt               string
}

type orphanProofRetirementPending struct {
	SchemaVersion  string
	Plan           OrphanProofRetirementPlan
	Input          OrphanProofRetirementInput
	Action         envelope.ActionIntent
	Decision       envelope.PolicyDecision
	AuthorizedAt   time.Time
	ReceiptInput   ReceiptedActionInput
	PolicyRevision string
}

var orphanProofRetirementFault func(string) error

func sha256Bytes(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func orphanProofRetirementPaths(homePath, reservationID string) (string, string, string, error) {
	if filepath.Base(reservationID) != reservationID || !strings.HasPrefix(reservationID, "pfr_") {
		return "", "", "", fmt.Errorf("invalid proof reservation id %q", reservationID)
	}
	base := filepath.Join(homePath, "work")
	return filepath.Join(base, "proof-resource-completions", reservationID+".json"),
		filepath.Join(base, "proof-resource-retirement-archives", reservationID+".json"),
		filepath.Join(base, "proof-resource-retirements", reservationID+".json"), nil
}

func (s *Store) orphanPredecessorWorkBody(workID string) ([]byte, error) {
	body, err := os.ReadFile(s.workPath(workID))
	if errors.Is(err, os.ErrNotExist) {
		if archivedPath, ok := s.archivedWorkPath(workID); ok {
			return os.ReadFile(archivedPath)
		}
	}
	return body, err
}

func (s *Store) orphanProofRetirementPendingPath(reservationID string) (string, error) {
	_, _, _, err := orphanProofRetirementPaths(s.home, reservationID)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.home, "work", "proof-resource-retirement-pending", reservationID+".json"), nil
}

// PlanOrphanProofRetirement does not write an intent, consume an approval, or
// change the predecessor. Apply must rederive and compare every bound image.
func (s *Store) PlanOrphanProofRetirement(recoveryWorkID, reservationID string) (OrphanProofRetirementPlan, error) {
	var plan OrphanProofRetirementPlan
	journalPath, archivePath, decisionPath, err := orphanProofRetirementPaths(s.home, reservationID)
	if err != nil {
		return plan, err
	}
	journal, err := os.ReadFile(journalPath)
	if err != nil {
		return plan, err
	}
	var pending proofResourceCompletionPending
	if err := json.Unmarshal(journal, &pending); err != nil {
		return plan, fmt.Errorf("decode proof completion journal: %w", err)
	}
	if pending.SchemaVersion != proofResourceCompletionPendingSchemaVersion || pending.ReservationID != reservationID ||
		pending.WorkID == "" || pending.Result.ID == "" || pending.Completion.ID == "" ||
		pending.Result.Status != ProofResourceFailed || pending.Completion.RunAttemptID != pending.Result.RunAttemptID ||
		pending.Result.RunAttemptID == "" || pending.Result.WorkID != pending.WorkID ||
		pending.Completion.WorkID != pending.WorkID || pending.Completion.ReservationID != reservationID {
		return plan, fmt.Errorf("journal is not an exact failed uncommitted completion")
	}
	predecessor, err := s.Get(pending.WorkID)
	if err != nil {
		return plan, err
	}
	if predecessor.State != "completed" {
		return plan, fmt.Errorf("predecessor Work must be completed")
	}
	recovery, err := s.Get(recoveryWorkID)
	if err != nil {
		return plan, err
	}
	if recovery.State != "claimed" && recovery.State != "running" && recovery.State != "verifying" {
		return plan, fmt.Errorf("recovery Work must be active")
	}
	recoveryBody, err := os.ReadFile(s.workPath(recoveryWorkID))
	if err != nil {
		return plan, err
	}
	if slices.Contains(predecessor.VerificationIDs, pending.Completion.ID) || slices.Contains(predecessor.VerificationIDs, pending.Result.VerificationRef) && pending.Result.VerificationRef != "" {
		return plan, fmt.Errorf("completion is linked as published proof")
	}
	if _, err := s.runAttempts().Get(pending.Result.RunAttemptID); err != nil {
		return plan, err
	}
	workBody, err := s.orphanPredecessorWorkBody(pending.WorkID)
	if err != nil {
		return plan, err
	}
	attemptBody, err := os.ReadFile(filepath.Join(s.home, "run_attempts", pending.Result.RunAttemptID+".json"))
	if err != nil {
		return plan, err
	}
	var attempt runattempt.RunAttempt
	if err := json.Unmarshal(attemptBody, &attempt); err != nil {
		return plan, err
	}
	if attempt.ID != pending.Result.RunAttemptID || attempt.WorkID != pending.WorkID || attempt.Status != runattempt.StatusFailed {
		return plan, fmt.Errorf("journal producer RunAttempt must be terminal failed on predecessor Work")
	}
	var completion ProofResourceCompletion
	if err := s.readEventJSON(pending.Completion.ID, &completion); err == nil {
		return plan, fmt.Errorf("completion %s is already published", pending.Completion.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return plan, err
	}
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(reservationID, &reservation); err != nil {
		return plan, err
	}
	if reservation.WorkID != pending.WorkID || reservation.RunAttemptID != attempt.ID || reservation.Holder == "" {
		return plan, fmt.Errorf("reservation does not bind journal and producer")
	}
	completeInput := ProofResourceCompleteInput{
		Actor: pending.Result.Actor, ReservationID: reservationID,
		ReportArtifactRef: pending.Result.ReportArtifactRef,
		ProducerExitKnown: pending.Result.ProducerExitKnown,
		ProducerExitCode:  pending.Result.ProducerExitCode,
		FailureReason:     pending.Result.FailureReason,
	}
	expectation, err := s.deriveProofResourceCompletionExpectation(pending.WorkID, reservation, completeInput)
	if err != nil {
		return plan, err
	}
	if expectation.ResultStatus != ProofResourceFailed {
		return plan, fmt.Errorf("journal no longer derives a failed result")
	}
	historical := predecessor
	historical.State = pending.LifecycleEvent.From
	if _, migrated, err := s.validateAndNormalizeProofResourceCompletionPending(
		historical, reservation, pending, attempt, completeInput, expectation,
	); err != nil {
		return plan, err
	} else if migrated {
		return plan, fmt.Errorf("orphan journal lacks current completion governance seal")
	}
	var result ProofResourceResult
	if err := s.readEventJSON(pending.Result.ID, &result); err == nil {
		return plan, fmt.Errorf("failed result %s was already published", pending.Result.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return plan, err
	}
	if archive, err := os.ReadFile(archivePath); err == nil {
		if sha256Bytes(archive) != sha256Bytes(journal) {
			return plan, fmt.Errorf("existing archive differs from exact journal")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return plan, err
	}
	if _, err := os.Stat(decisionPath); err == nil {
		return plan, fmt.Errorf("retirement already decided")
	} else if !errors.Is(err, os.ErrNotExist) {
		return plan, err
	}
	return OrphanProofRetirementPlan{
		SchemaVersion: orphanProofRetirementSchema, RecoveryWorkID: recoveryWorkID,
		ReservationID: reservationID, PredecessorWorkID: pending.WorkID,
		RunAttemptID: attempt.ID, JournalSHA256: sha256Bytes(journal),
		PredecessorWorkSHA256: sha256Bytes(workBody), RunAttemptSHA256: sha256Bytes(attemptBody),
		RecoveryWorkSHA256: sha256Bytes(recoveryBody),
		CompletionID:       pending.Completion.ID, ResultID: pending.Result.ID,
		OriginalHolder: reservation.Holder, ArchivePath: archivePath, DecisionPath: decisionPath,
	}, nil
}

func (s *Store) checkOrphanProofRetirementSafety(plan OrphanProofRetirementPlan, now time.Time) error {
	resource, resolveErr := s.proofResourceForHolder(plan.OriginalHolder)
	if resolveErr != nil {
		return resolveErr
	}

	status, err := resource.ObserveHistoricalHolder(plan.OriginalHolder, now)
	if err != nil {
		return err
	}
	if status.Holder == plan.OriginalHolder {
		return fmt.Errorf("exact original proof holder can still use normal completion recovery")
	}
	live, err := resource.HistoricalExecutionPending(plan.OriginalHolder)
	if err != nil {
		return err
	}
	if live {
		return fmt.Errorf("original proof execution tree is still live")
	}
	attempts, err := s.runAttempts().List()
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		if attempt.WorkID == plan.PredecessorWorkID && !runattempt.IsTerminal(attempt.Status) {
			return fmt.Errorf("predecessor proof RunAttempt %s remains nonterminal", attempt.ID)
		}
	}
	return nil
}

func (s *Store) checkOrphanProofRetirementPause(in OrphanProofRetirementInput, now time.Time) error {
	if !in.WritersPaused || !strings.HasPrefix(in.Operator, "operator:") ||
		len(in.Operator) <= len("operator:") || strings.TrimSpace(in.EvidenceRef) == "" {
		return fmt.Errorf("current operator-provenanced all-writer pause evidence required")
	}
	if !in.ExpiresAt.After(now) || in.ExpiresAt.After(now.Add(5*time.Minute)) {
		return fmt.Errorf("writer pause must be current and expire within five minutes")
	}
	return nil
}

func (s *Store) readOrphanProofRetirementPending(id string) (orphanProofRetirementPending, bool, string, error) {
	var p orphanProofRetirementPending
	path, err := s.orphanProofRetirementPendingPath(id)
	if err != nil {
		return p, false, "", err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, false, path, nil
	}
	if err != nil {
		return p, false, path, err
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, false, path, err
	}
	if p.SchemaVersion != orphanProofRetirementSchema || p.Plan.ReservationID != id || p.Action.ID == "" || p.Decision.ID == "" {
		return p, false, path, fmt.Errorf("incomplete orphan proof retirement transaction")
	}
	return p, true, path, nil
}

func writeOrphanProofBytes(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	staged, err := os.CreateTemp(dir, "."+filepath.Base(path)+".stage-")
	if err != nil {
		return err
	}
	stagePath := staged.Name()
	defer os.Remove(stagePath)
	if err := staged.Chmod(0600); err != nil {
		staged.Close()
		return err
	}
	if _, err := staged.Write(body); err != nil {
		staged.Close()
		return err
	}
	if err := staged.Sync(); err != nil {
		staged.Close()
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	// Hard-link publication is exclusive and atomic: a crash cannot expose a
	// partially written archive or decision under its authoritative name.
	if err := os.Link(stagePath, path); err != nil {
		return err
	}
	return syncReceiptedActionDir(dir)
}

func orphanProofRetirementDecision(p orphanProofRetirementPending) OrphanProofRetirementDecision {
	return OrphanProofRetirementDecision{
		SchemaVersion: orphanProofRetirementSchema, RecoveryWorkID: p.Plan.RecoveryWorkID,
		PredecessorWorkID: p.Plan.PredecessorWorkID, ReservationID: p.Plan.ReservationID,
		RunAttemptID: p.Plan.RunAttemptID, JournalSHA256: p.Plan.JournalSHA256,
		ArchivePath: p.Plan.ArchivePath, FailedResultUncertified: true,
		ActionIntentID: p.Action.ID, PolicyDecisionID: p.Decision.ID,
		DecidedAt: p.AuthorizedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (s *Store) retireOrphanProofJournal(p orphanProofRetirementPending) error {
	journalPath, archivePath, decisionPath, err := orphanProofRetirementPaths(s.home, p.Plan.ReservationID)
	if err != nil {
		return err
	}
	archive, archiveErr := os.ReadFile(archivePath)
	if archiveErr != nil && !errors.Is(archiveErr, os.ErrNotExist) {
		return archiveErr
	}
	journal, journalErr := os.ReadFile(journalPath)
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return journalErr
	}
	if archiveErr == nil && sha256Bytes(archive) != p.Plan.JournalSHA256 {
		return fmt.Errorf("retirement archive changed")
	}
	if journalErr == nil && sha256Bytes(journal) != p.Plan.JournalSHA256 {
		return fmt.Errorf("active proof journal changed")
	}
	if archiveErr != nil {
		if journalErr != nil {
			return fmt.Errorf("both journal and archive are missing")
		}
		if err := writeOrphanProofBytes(archivePath, journal); err != nil {
			return err
		}
	}
	if orphanProofRetirementFault != nil {
		if err := orphanProofRetirementFault("archived"); err != nil {
			return err
		}
	}
	if journalErr == nil {
		current, err := os.ReadFile(journalPath)
		if err != nil || sha256Bytes(current) != p.Plan.JournalSHA256 {
			return fmt.Errorf("active proof journal changed before retirement: %w", err)
		}
		if err := os.Remove(journalPath); err != nil {
			return err
		}
		if err := syncReceiptedActionDir(filepath.Dir(journalPath)); err != nil {
			return err
		}
	}
	if orphanProofRetirementFault != nil {
		if err := orphanProofRetirementFault("retired"); err != nil {
			return err
		}
	}
	decision := orphanProofRetirementDecision(p)
	if body, err := os.ReadFile(decisionPath); err == nil {
		var existing OrphanProofRetirementDecision
		if json.Unmarshal(body, &existing) != nil || existing != decision {
			return fmt.Errorf("retirement decision changed")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	encoded, err := json.MarshalIndent(decision, "", "  ")
	if err != nil {
		return err
	}
	return writeOrphanProofBytes(decisionPath, append(encoded, '\n'))
}

func orphanProofRetirementInputHash(in OrphanProofRetirementInput) (string, error) {
	body, err := json.Marshal(struct {
		Input      OrphanProofRetirementInput
		WorkSHA256 string
	}{in, in.Plan.RecoveryWorkSHA256})
	if err != nil {
		return "", err
	}
	return sha256Bytes(body), nil
}

// A crash between receipted reservation and the orphan-specific marker leaves
// the former as the exact work-owned blocker. Rebuild only from its persisted
// input and this same reviewable plan; never spend another approval.
func (s *Store) recoverReservedOrphanProofRetirement(workID string, in OrphanProofRetirementInput, path string) (orphanProofRetirementPending, bool, error) {
	var empty orphanProofRetirementPending
	reservation, exists, _, err := s.preflightReceiptedActionReservation(workID)
	if err != nil || !exists {
		return empty, false, err
	}
	if reservation.Input == nil {
		return empty, false, fmt.Errorf("recovery Work has reservation without reconstructable input")
	}
	input := *reservation.Input
	if input.Action.Action != "proof.completion.orphan-retire" || input.Action.Actor != in.Actor ||
		input.PreEvaluatedDecision == nil || input.Action.ID != reservation.ActionID ||
		input.PreEvaluatedDecision.ID != reservation.PolicyDecisionID {
		return empty, false, fmt.Errorf("recovery Work has a different receipted reservation")
	}
	hash, err := orphanProofRetirementInputHash(in)
	if err != nil {
		return empty, false, err
	}
	if input.Action.ArgumentsHash != hash || len(input.BeforeRefs) != 4 ||
		input.BeforeRefs[0] != in.Plan.JournalSHA256 ||
		input.BeforeRefs[1] != in.Plan.PredecessorWorkSHA256 ||
		input.BeforeRefs[2] != in.Plan.RunAttemptSHA256 ||
		!strings.HasPrefix(input.BeforeRefs[3], "policy_revision:") ||
		!reflect.DeepEqual(input.ProofRefs, []string{in.Plan.ArchivePath, in.Plan.DecisionPath}) {
		return empty, false, fmt.Errorf("reserved orphan proof transaction does not bind supplied exact plan")
	}
	if err := policy.NewExactActionApprovalStore(s.home).ValidateConsumedDecision(input.Action, *input.PreEvaluatedDecision); err != nil {
		return empty, false, fmt.Errorf("reserved orphan proof approval changed: %w", err)
	}
	authorizedAt, err := time.Parse(time.RFC3339Nano, reservation.AuthorizedAt)
	if err != nil {
		return empty, false, err
	}
	rebuilt := orphanProofRetirementPending{
		SchemaVersion: orphanProofRetirementSchema, Plan: in.Plan, Input: in,
		Action: input.Action, Decision: *input.PreEvaluatedDecision,
		AuthorizedAt: authorizedAt, ReceiptInput: input,
		PolicyRevision: strings.TrimPrefix(input.BeforeRefs[3], "policy_revision:"),
	}
	err = s.withLifecycleMutation(func() error {
		actual, present, _, err := s.preflightReceiptedActionReservation(workID)
		if err != nil {
			return err
		}
		if !present || !reflect.DeepEqual(actual, reservation) {
			return fmt.Errorf("receipted reservation changed during orphan recovery")
		}
		fresh, err := s.PlanOrphanProofRetirement(workID, in.Plan.ReservationID)
		if err != nil {
			return err
		}
		if fresh != in.Plan {
			return fmt.Errorf("reserved orphan plan preimage changed")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		return s.writeJSON(path, rebuilt)
	})
	if err != nil {
		return empty, false, err
	}
	return rebuilt, true, nil
}

func (s *Store) validateOrphanProofRetirementPending(p orphanProofRetirementPending) error {
	if p.SchemaVersion != orphanProofRetirementSchema || p.Plan != p.Input.Plan ||
		p.Plan.RecoveryWorkID != p.Action.IDs.WorkID ||
		p.Input.Actor != p.Action.Actor || p.Action.Action != "proof.completion.orphan-retire" ||
		p.ReceiptInput.PreEvaluatedDecision == nil ||
		!reflect.DeepEqual(*p.ReceiptInput.PreEvaluatedDecision, p.Decision) ||
		!reflect.DeepEqual(p.ReceiptInput.Action, p.Action) ||
		p.ReceiptInput.ExpectedWorkState != "running" ||
		!p.ReceiptInput.OutcomePending || p.ReceiptInput.Outcome != "blocked" {
		return fmt.Errorf("orphan retirement pending authority is inconsistent")
	}
	hash, err := orphanProofRetirementInputHash(p.Input)
	if err != nil {
		return err
	}
	if p.Action.ArgumentsHash != hash ||
		!reflect.DeepEqual(p.ReceiptInput.BeforeRefs, []string{
			p.Plan.JournalSHA256, p.Plan.PredecessorWorkSHA256, p.Plan.RunAttemptSHA256,
			"policy_revision:" + p.PolicyRevision,
		}) ||
		!reflect.DeepEqual(p.ReceiptInput.ProofRefs, []string{p.Plan.ArchivePath, p.Plan.DecisionPath}) {
		return fmt.Errorf("orphan retirement pending exact preimage binding changed")
	}
	if err := policy.NewExactActionApprovalStore(s.home).ValidateConsumedDecision(p.Action, p.Decision); err != nil {
		return fmt.Errorf("orphan retirement pending approval changed: %w", err)
	}
	return nil
}

func (s *Store) reconcileRecordedOrphanRetirement(workID string, p orphanProofRetirementPending, pendingPath string) (OrphanProofRetirementDecision, bool, error) {
	var out OrphanProofRetirementDecision
	completion, recorded, _, err := s.preflightReceiptedActionCompletion(workID, p.Action.ID)
	if err != nil || !recorded {
		return out, false, err
	}
	var receipt envelope.Receipt
	if err := s.readEventJSON(completion.ReceiptID, &receipt); err != nil {
		return out, true, err
	}
	final := p.ReceiptInput
	final.OutcomePending = false
	final.Outcome = receipt.Outcome
	final.Changed = receipt.StateDelta.Changed
	final.Summary = receipt.StateDelta.Summary
	if _, err := s.RecordReservedReceiptedAction(workID, final, p.AuthorizedAt); err != nil {
		return out, true, err
	}
	if receipt.Outcome == "completed" {
		journalPath, archivePath, _, err := orphanProofRetirementPaths(s.home, p.Plan.ReservationID)
		if err != nil {
			return out, true, err
		}
		if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
			return out, true, fmt.Errorf("recorded retirement still has active journal: %v", err)
		}
		archive, err := os.ReadFile(archivePath)
		if err != nil || sha256Bytes(archive) != p.Plan.JournalSHA256 {
			return out, true, fmt.Errorf("recorded retirement archive differs: %w", err)
		}
		body, err := os.ReadFile(p.Plan.DecisionPath)
		if err != nil {
			return out, true, err
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return out, true, err
		}
		expected := orphanProofRetirementDecision(p)
		if out != expected {
			return out, true, fmt.Errorf("recorded retirement decision differs from exact pending transaction")
		}
	}
	if err := os.Remove(pendingPath); err != nil {
		return out, true, err
	}
	if err := syncReceiptedActionDir(filepath.Dir(pendingPath)); err != nil {
		return out, true, err
	}
	if receipt.Outcome != "completed" {
		return out, true, fmt.Errorf("orphan retirement previously recorded failed before journal removal")
	}
	return out, true, nil
}

// RetireOrphanProofCompletion is an operator-approved exact recovery of a
// failed, uncommitted proof result. Replay of an admitted transaction only
// completes the same archive and receipt; it never certifies the old proof.
func (s *Store) RetireOrphanProofCompletion(workID string, in OrphanProofRetirementInput, now time.Time) (OrphanProofRetirementDecision, error) {
	var out OrphanProofRetirementDecision
	started := time.Now()
	currentTime := func() time.Time { return now.Add(time.Since(started)) }
	if in.Plan.RecoveryWorkID != workID {
		return out, fmt.Errorf("recovery Work mismatch")
	}
	pending, exists, pendingPath, err := s.readOrphanProofRetirementPending(in.Plan.ReservationID)
	if err != nil {
		return out, err
	}
	in.ExpiresAt = in.ExpiresAt.UTC()
	if !exists {
		pending, exists, err = s.recoverReservedOrphanProofRetirement(workID, in, pendingPath)
		if err != nil {
			return out, err
		}
	}
	if exists {
		if !reflect.DeepEqual(pending.Input, in) || pending.Plan != in.Plan {
			return out, fmt.Errorf("pending retirement requires exact original input")
		}
		if err := s.validateOrphanProofRetirementPending(pending); err != nil {
			return out, err
		}
		if settled, done, err := s.reconcileRecordedOrphanRetirement(workID, pending, pendingPath); done || err != nil {
			return settled, err
		}
	} else {
		if err := s.checkOrphanProofRetirementPause(in, currentTime()); err != nil {
			return out, err
		}
		current, err := s.PlanOrphanProofRetirement(workID, in.Plan.ReservationID)
		if err != nil {
			return out, err
		}
		if current != in.Plan {
			return out, fmt.Errorf("orphan proof retirement preimage changed")
		}
		if err := s.checkOrphanProofRetirementSafety(current, currentTime()); err != nil {
			return out, err
		}
		var w envelope.WorkEnvelope
		var action envelope.ActionIntent
		var decision envelope.PolicyDecision
		var policyRevision string
		err = s.withLifecycleMutation(func() error {
			var err error
			w, err = s.resourceMutationOwner(workID, in.Actor)
			if err != nil {
				return err
			}
			fresh, err := s.PlanOrphanProofRetirement(workID, in.Plan.ReservationID)
			if err != nil {
				return err
			}
			if fresh != in.Plan {
				return fmt.Errorf("retirement preimage changed before authorization")
			}
			if err := s.checkOrphanProofRetirementSafety(fresh, currentTime()); err != nil {
				return err
			}
			if err := s.checkOrphanProofRetirementPause(in, currentTime()); err != nil {
				return err
			}
			action = s.resourceMutationAction(w, in.Actor, "proof.completion.orphan-retire",
				"archive exact failed uncommitted journal; predecessor="+in.Plan.PredecessorWorkID+" journal="+in.Plan.JournalSHA256+" operator="+in.Operator+" pause="+in.EvidenceRef+" expires="+in.ExpiresAt.UTC().Format(time.RFC3339Nano),
				in.Plan.ArchivePath, currentTime())
			action.MutationClass = "security"
			action.Reversibility = "irreversible"
			action.BlastRadius = "host"
			action.RollbackPlan = "restore exact archived bytes only through a separately governed recovery"
			action.ArgumentsHash, err = orphanProofRetirementInputHash(in)
			if err != nil {
				return err
			}
			action.EvidenceRefs = append(action.EvidenceRefs, in.EvidenceRef)
			cfg, _, err := policy.LoadConfig(s.home)
			if err != nil {
				return err
			}
			policyRevision = policy.ActionBindingForIntent(action, cfg).PolicyRevision
			decision, err = s.authorizeResourceMutationLocked(w, action, currentTime())
			return err
		})
		if err != nil {
			return out, err
		}
		receiptInput := ReceiptedActionInput{
			OutcomePending: true, Action: action, PreEvaluatedDecision: &decision,
			ExpectedWorkState: w.State, Outcome: "blocked", Changed: false,
			Summary: "orphan proof retirement pending", MoveType: "control_plane_repair",
			BeforeRefs: []string{in.Plan.JournalSHA256, in.Plan.PredecessorWorkSHA256, in.Plan.RunAttemptSHA256, "policy_revision:" + policyRevision},
			ProofRefs:  []string{in.Plan.ArchivePath, in.Plan.DecisionPath},
		}
		authorizedAt := currentTime()
		if err := s.ReserveReceiptedAction(workID, receiptInput, authorizedAt, func() error {
			fresh, err := s.PlanOrphanProofRetirement(workID, in.Plan.ReservationID)
			if err != nil {
				return err
			}
			if fresh != in.Plan {
				return fmt.Errorf("retirement preimage changed before reservation")
			}
			if err := s.checkOrphanProofRetirementPause(in, currentTime()); err != nil {
				return err
			}
			return s.checkOrphanProofRetirementSafety(fresh, currentTime())
		}); err != nil {
			return out, err
		}
		pending = orphanProofRetirementPending{
			SchemaVersion: orphanProofRetirementSchema, Plan: in.Plan, Input: in,
			Action: action, Decision: decision, AuthorizedAt: authorizedAt, ReceiptInput: receiptInput, PolicyRevision: policyRevision,
		}
		if orphanProofRetirementFault != nil {
			if err := orphanProofRetirementFault("reserved"); err != nil {
				return out, fmt.Errorf("retirement retry pending: %w", err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(pendingPath), 0700); err != nil {
			return out, err
		}
		if err := s.writeJSON(pendingPath, pending); err != nil {
			return out, err
		}
	}
	journalPath, archivePath, _, err := orphanProofRetirementPaths(s.home, pending.Plan.ReservationID)
	if err != nil {
		return out, err
	}
	journal, journalErr := os.ReadFile(journalPath)
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return out, journalErr
	}
	archive, archiveErr := os.ReadFile(archivePath)
	if archiveErr != nil && !errors.Is(archiveErr, os.ErrNotExist) {
		return out, archiveErr
	}
	if archiveErr == nil && sha256Bytes(archive) != pending.Plan.JournalSHA256 {
		return out, fmt.Errorf("retirement archive changed")
	}
	if journalErr == nil && sha256Bytes(journal) != pending.Plan.JournalSHA256 {
		return out, fmt.Errorf("active proof journal changed")
	}
	if journalErr != nil && archiveErr != nil {
		return out, fmt.Errorf("both active journal and exact archive are missing")
	}
	if journalErr == nil {
		if err := s.checkOrphanProofRetirementPause(pending.Input, currentTime()); err != nil {
			final := pending.ReceiptInput
			final.OutcomePending = false
			final.Outcome = "failed"
			final.Changed = archiveErr == nil
			final.Summary = "retirement stopped before journal removal: writer pause expired; failed proof remains uncertified"
			if sealErr := s.SealReceiptedActionOutcome(workID, pending.ReceiptInput, final, pending.AuthorizedAt); sealErr != nil {
				return out, fmt.Errorf("retirement retry pending: %w", errors.Join(err, sealErr))
			}
			if _, recordErr := s.RecordReservedReceiptedAction(workID, final, pending.AuthorizedAt); recordErr != nil {
				return out, fmt.Errorf("retirement retry pending: %w", errors.Join(err, recordErr))
			}
			if removeErr := os.Remove(pendingPath); removeErr != nil {
				return out, removeErr
			}
			if syncErr := syncReceiptedActionDir(filepath.Dir(pendingPath)); syncErr != nil {
				return out, syncErr
			}
			return out, fmt.Errorf("retirement stopped before journal removal: %w", err)
		}
	}
	err = s.withLifecycleMutation(func() error {
		current, err := s.resourceMutationOwner(workID, pending.Input.Actor)
		if err != nil {
			return err
		}
		currentBody, err := os.ReadFile(s.workPath(workID))
		if err != nil {
			return err
		}
		if sha256Bytes(currentBody) != pending.Plan.RecoveryWorkSHA256 || current.ID != pending.Plan.RecoveryWorkID {
			return fmt.Errorf("recovery Work generation changed")
		}
		var durableDecision envelope.PolicyDecision
		if err := s.readEventJSON(pending.Decision.ID, &durableDecision); err != nil {
			return err
		}
		if !reflect.DeepEqual(durableDecision, pending.Decision) || !policy.Allowed(durableDecision) {
			return fmt.Errorf("reserved policy decision changed")
		}
		lockedJournal, lockedErr := os.ReadFile(journalPath)
		if lockedErr != nil && !errors.Is(lockedErr, os.ErrNotExist) {
			return lockedErr
		}
		if lockedErr == nil && sha256Bytes(lockedJournal) != pending.Plan.JournalSHA256 {
			return fmt.Errorf("active proof journal changed under retirement lock")
		}
		if lockedErr != nil {
			lockedArchive, err := os.ReadFile(archivePath)
			if err != nil || sha256Bytes(lockedArchive) != pending.Plan.JournalSHA256 {
				return fmt.Errorf("retired journal lacks exact archive: %w", err)
			}
		}
		if lockedErr == nil {
			if err := s.checkOrphanProofRetirementPause(pending.Input, currentTime()); err != nil {
				return err
			}
			if err := s.checkOrphanProofRetirementSafety(pending.Plan, currentTime()); err != nil {
				return err
			}
			cfg, _, err := policy.LoadConfig(s.home)
			if err != nil {
				return err
			}
			if policy.ActionBindingForIntent(pending.Action, cfg).PolicyRevision != pending.PolicyRevision {
				return fmt.Errorf("policy revision changed before retirement effect")
			}
			for _, class := range []string{"build-implement", pending.Action.Capability} {
				halted, reason, err := policy.AutonomyHalted(s.home, class)
				if err != nil {
					return err
				}
				if halted {
					return fmt.Errorf("retirement halted: %s", reason)
				}
			}
		}
		workBody, err := s.orphanPredecessorWorkBody(pending.Plan.PredecessorWorkID)
		if err != nil {
			return err
		}
		attemptBody, err := os.ReadFile(filepath.Join(s.home, "run_attempts", pending.Plan.RunAttemptID+".json"))
		if err != nil {
			return err
		}
		if sha256Bytes(workBody) != pending.Plan.PredecessorWorkSHA256 || sha256Bytes(attemptBody) != pending.Plan.RunAttemptSHA256 {
			return fmt.Errorf("predecessor Work or producer attempt changed")
		}
		return s.retireOrphanProofJournal(pending)
	})
	if err != nil {
		return out, fmt.Errorf("retirement retry pending: %w", err)
	}
	final := pending.ReceiptInput
	final.OutcomePending = false
	final.Outcome = "completed"
	final.Changed = true
	final.Summary = "exact failed proof journal archived and retired; failed result remains uncertified"
	if err := s.SealReceiptedActionOutcome(workID, pending.ReceiptInput, final, pending.AuthorizedAt); err != nil {
		return out, fmt.Errorf("retirement receipt retry pending: %w", err)
	}
	if _, err := s.RecordReservedReceiptedAction(workID, final, pending.AuthorizedAt); err != nil {
		return out, fmt.Errorf("retirement receipt retry pending: %w", err)
	}
	if orphanProofRetirementFault != nil {
		if err := orphanProofRetirementFault("recorded"); err != nil {
			return out, fmt.Errorf("retirement receipt retry pending: %w", err)
		}
	}
	if err := os.Remove(pendingPath); err != nil {
		return out, err
	}
	if err := syncReceiptedActionDir(filepath.Dir(pendingPath)); err != nil {
		return out, err
	}
	body, err := os.ReadFile(in.Plan.DecisionPath)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	return out, nil
}
