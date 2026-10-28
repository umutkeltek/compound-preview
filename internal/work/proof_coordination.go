package work

import (
	"compound/internal/authority/gitrepository"
	"compound/internal/landlease"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

// CanonicalProofCapacityClass derives admission cost from the exact sealed
// source inventory. It does not change canonical_full certification semantics.
func CanonicalProofCapacityClass(profile ProofResourceProfile) (landlease.ProofCapacityClass, error) {
	if profile.SourceSnapshot == nil {
		return "", fmt.Errorf("coordinated proof requires sealed source snapshot")
	}
	if err := gitrepository.RevalidateSnapshot(profile.Repo, *profile.SourceSnapshot); err != nil {
		return "", err
	}
	plan, err := resolveCanonicalProofPlan(profile.Repo, *profile.SourceSnapshot, nil, true)
	if err != nil {
		return "", err
	}
	if plan.ID == CanonicalProofPlanCompoundV1 || plan.ID == CanonicalProofPlanRustWorkspaceV1 {
		return landlease.ProofCapacitySystemFull, nil
	}
	// Public and private reviewed redaction suffixes change neither consumers nor class.
	// Reproduce its exact typed shape here; Store still authenticates its owner
	// before minting any reservation.
	for _, gate := range profile.Gates {
		binding, reviewed, err := PublicRedactionBindingForStep(ProofCheckStep{Name: gate.Name, Command: gate.Command, ProofRef: gate.ProofRef})
		if err != nil {
			return "", err
		}
		if reviewed {
			plan, err = BindPublicRedactionReview(plan, binding)
			if err != nil {
				return "", err
			}
		}
		binding, reviewed, err = PrivateSourceBindingForStep(ProofCheckStep{Name: gate.Name, Command: gate.Command, ProofRef: gate.ProofRef})
		if err != nil {
			return "", err
		}
		if reviewed {
			plan, err = bindPrivateSourceReview(plan, binding)
			if err != nil {
				return "", err
			}
		}
	}
	expected, err := ProofResourceProfileForCanonicalPlan(plan)
	if err != nil {
		return "", err
	}
	normalized, err := normalizeProofResourceProfile(profile)
	if err != nil {
		return "", err
	}
	if profile.Digest != "" && profile.Digest != normalized.Digest {
		return "", fmt.Errorf("native proof profile digest changed")
	}
	if !reflect.DeepEqual(normalized, expected) {
		return "", fmt.Errorf("native capacity requires exact sealed canonical consumer inventory")
	}
	return landlease.ProofCapacityNativeConsumer, nil
}

func (s *Store) proofCoordinationForProfile(profile ProofResourceProfile) (*landlease.ProofCoordinationIdentity, error) {
	if profile.RemoteGoTest != nil {
		if err := ValidateRemoteProofDomain(s.home); err != nil {
			return nil, err
		}
	}
	generation, err := landlease.ProofCoordinationGeneration(s.home)
	if err != nil || generation == "" {
		return nil, err
	}
	class, err := CanonicalProofCapacityClass(profile)
	if err != nil {
		return nil, err
	}
	return landlease.ProofCoordinationFor(s.home, class, profile.SourceSnapshot.RepositoryFingerprint)
}
func validateProofCoordination(reservation ProofResourceReservation) error {
	if !proofReservationSchemaValid(reservation) {
		return fmt.Errorf("proof reservation schema and coordination identity disagree")
	}
	identity := reservation.ProofCoordination
	if identity == nil {
		return nil
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	if reservation.Profile.SourceSnapshot == nil || reservation.Profile.SourceSnapshot.RepositoryFingerprint != identity.RepositoryFingerprint {
		return fmt.Errorf("proof reservation coordination does not match sealed repository")
	}
	return nil
}
func (s *Store) readProofReservationInto(id string, reservation *ProofResourceReservation) error {
	if err := s.readEventJSON(id, reservation); err != nil {
		return err
	}
	if reservation.ID != id {
		return fmt.Errorf("proof reservation identity mismatch")
	}
	return validateProofCoordination(*reservation)
}
func (s *Store) proofResourceForHolder(holder string) (*landlease.Resource, error) {
	generation, err := landlease.ProofCoordinationGeneration(s.home)
	if err != nil {
		return nil, err
	}
	if generation == "" {
		return landlease.ProofResource(s.home), nil
	}

	id, _, _, ok := parseProofResourceHolder(holder)
	if !ok {
		return landlease.ProofResource(s.home), nil
	}
	var reservation ProofResourceReservation
	if err := s.readProofReservationInto(id, &reservation); err != nil {
		return nil, err
	}
	if reservation.Holder != holder {
		return nil, fmt.Errorf("proof holder does not match persisted reservation")
	}
	return landlease.ProofResourceForIdentity(s.home, reservation.ProofCoordination), nil
}
func (s *Store) proofStatusForWork(workID string, now time.Time) (landlease.StatusResult, error) {
	statuses, err := landlease.ProofResourceStatuses(s.home, now)
	if err != nil {
		return landlease.StatusResult{}, err
	}
	var found landlease.StatusResult
	for _, row := range statuses {
		_, _, heldWorkID, ok := parseProofResourceHolder(row.Status.Holder)
		if ok && heldWorkID == workID {
			if found.Holder != "" {
				return landlease.StatusResult{}, fmt.Errorf("work has multiple current proof capacities")
			}
			found = row.Status
		}
	}
	return found, nil
}
func (s *Store) prepareProofAdmission(in *ProofResourceReserveInput) (owned bool, err error) {
	if in.admission != nil {
		in.coordination = in.admission.Identity()
		return false, nil
	}
	identity, err := s.proofCoordinationForProfile(in.Profile)
	if err != nil {
		return false, err
	}
	in.coordination = identity
	if identity == nil {
		return false, nil
	}
	admission, err := landlease.BeginProofAdmission(s.home, *identity, time.Now().Add(time.Minute))
	if err != nil {
		return false, err
	}
	in.admission = admission
	if in.admissionContext == nil {
		in.admissionContext = context.Background()
	}
	return true, nil
}
func proofAdmissionBusy(err error) bool {
	var capacity *landlease.ProofAdmissionBusyError
	var queue *landlease.ProofQueueBusyError
	var descendant *landlease.ExecutionPendingError
	return errors.As(err, &capacity) || errors.As(err, &queue) || errors.As(err, &descendant)
}

func (s *Store) reserveCoordinatedProofWhenFree(ctx context.Context, workID string, in ProofResourceReserveInput, identity *landlease.ProofCoordinationIdentity, clk landlease.Clock, deadline time.Time, progress io.Writer) (result ProofResourceReserveResult, err error) {
	duration := deadline.Sub(clk.Now())
	if duration <= 0 {
		return result, context.DeadlineExceeded
	}
	admission, err := landlease.BeginProofAdmission(s.home, *identity, time.Now().Add(duration))
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, admission.Cancel()) }()
	in.admission = admission
	in.admissionContext = ctx
	in.coordination = identity
	started := clk.Now()
	announced := ""
	for {
		if err := checkProofWaitAdmission(in); err != nil {
			return result, err
		}
		// Occupied capacity cannot admit work. Avoid folding the lifecycle log
		// under the home guard merely to rediscover the same busy class.
		// Vacancy is only a hint: ReserveProofResource retains every locked check.
		status, statusErr := admission.WaitingStatus(clk.Now())
		if statusErr != nil {
			return result, statusErr
		}
		if status.Held {
			err = &landlease.ProofAdmissionBusyError{Holder: status.Holder, Reason: "capacity occupied"}
		} else {
			result, err = s.ReserveProofResource(workID, in, clk.Now())
		}
		if err == nil {
			s.observeProofQueue(result.Reservation.ID, clk.Now().Sub(started), clk.Now().Sub(started))
			return result, nil
		}
		if result.Reservation.ID != "" || !proofAdmissionBusy(err) {
			return result, err
		}
		if progress != nil && announced != err.Error() {
			announced = err.Error()
			fmt.Fprintln(progress, announced)
		}
		delay := proofResourceWaitPoll
		if remaining := deadline.Sub(clk.Now()); remaining < delay {
			delay = remaining
		}
		if delay <= 0 {
			return result, context.DeadlineExceeded
		}
		clk.Sleep(delay)
	}
}

func proofReservationSchemaValid(r ProofResourceReservation) bool {
	switch r.SchemaVersion {
	case ProofResourceReservationSchemaVersion:
		return r.ProofCoordination == nil
	case ProofResourceCoordinatedReservationSchemaVersion:
		return r.ProofCoordination != nil
	default:
		return false
	}
}

// ProofExecutionLive is the Work owner's liveness view across current capacity
// classes. The caller still checks the ordinary harness delegate if no proof
// capacity belongs to this exact run.
func (s *Store) ProofExecutionLive(runID string, now time.Time) (bool, error) {
	rows, err := landlease.ProofResourceStatuses(s.home, now)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		status := row.Status
		_, heldRun, _, ok := parseProofResourceHolder(status.Holder)
		if !ok || heldRun != runID {
			continue
		}
		proof := landlease.ProofResourceForIdentity(s.home, status.ProofCoordination)
		pending, err := proof.ExecutionPending(status.Holder)
		if err != nil {
			return false, err
		}
		if status.Held || pending {
			return true, nil
		}
	}
	return false, nil
}

// preferTerminatedRun lets an audited steal target the slot whose holder is the
// exact terminated predecessor run. Without a run id, slot choice is lowest first.
func preferTerminatedRun(runAttemptID string) func(string) bool {
	runAttemptID = strings.TrimSpace(runAttemptID)
	if runAttemptID == "" {
		return nil
	}
	return func(holder string) bool {
		_, run, _, ok := parseProofResourceHolder(holder)
		return ok && run == runAttemptID
	}
}
