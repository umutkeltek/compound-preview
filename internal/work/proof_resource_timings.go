package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"compound/internal/home"
)

// Timing observations are advisory, never verification or lease authority. They
// live outside sealed reports so an observation failure cannot change a verdict.
const proofResourceTimingsSchema = "compound.proof_resource_timings.v0.1"
const proofResourceTimingsPath = "work/proof-resource-timings.json"
const proofResourceTimingsLimit = 50

type ProofResourceTiming struct {
	ReservationID         string `json:"reservation_id"`
	WorkID                string `json:"work_id"`
	RepositoryFingerprint string `json:"repository_fingerprint,omitempty"`
	Repo                  string `json:"repo"`
	AdmissionMS           *int64 `json:"admission_ms,omitempty"`
	QueueWaitMS           *int64 `json:"queue_wait_ms,omitempty"`
	AdmittedAt            string `json:"admitted_at"`
	ReleasedAt            string `json:"released_at,omitempty"`
	// ObservedReservedMS spans successful admission to confirmed release.
	// It excludes reservation preparation and includes retained-completion
	// recovery time. It is not a gate duration or an exact kernel-lock hold.
	ObservedReservedMS *int64 `json:"observed_reserved_ms,omitempty"`
}

type proofResourceTimings struct {
	SchemaVersion string                `json:"schema_version"`
	Samples       []ProofResourceTiming `json:"samples"`
}

// ReadProofResourceTimings does not create files. Empty fingerprint selects all
// repositories explicitly; callers must not use it after an identity failure.
func ReadProofResourceTimings(homePath, fingerprint string, window int) ([]ProofResourceTiming, error) {
	history, err := readProofResourceTimings(homePath)
	if err != nil {
		return nil, err
	}
	if window <= 0 {
		window = 20
	}
	samples := make([]ProofResourceTiming, 0)
	for _, sample := range history.Samples {
		if fingerprint == "" || sample.RepositoryFingerprint == fingerprint {
			samples = append(samples, sample)
		}
	}
	if len(samples) > window {
		samples = samples[len(samples)-window:]
	}
	return samples, nil
}

func readProofResourceTimings(homePath string) (proofResourceTimings, error) {
	history := proofResourceTimings{SchemaVersion: proofResourceTimingsSchema}
	body, err := os.ReadFile(filepath.Join(homePath, proofResourceTimingsPath))
	if errors.Is(err, os.ErrNotExist) {
		return history, nil
	}
	if err != nil {
		return history, err
	}
	if err := json.Unmarshal(body, &history); err != nil {
		return history, err
	}
	if history.SchemaVersion != proofResourceTimingsSchema {
		return history, fmt.Errorf("unsupported proof resource timing schema %q", history.SchemaVersion)
	}
	return history, nil
}

// Nonblocking: even a stuck telemetry writer must never hold up proof admission
// or release. A skipped observation is reported, never converted to a zero.
func (s *Store) updateProofResourceTiming(id string, update func(*ProofResourceTiming)) error {
	if s.readOnly {
		return fmt.Errorf("read-only work store")
	}
	if id == "" {
		return fmt.Errorf("reservation id required for timing observation")
	}
	path := filepath.Join(s.home, "lock/proof-resource-timings.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	history, err := readProofResourceTimings(s.home)
	if err != nil {
		return err
	} // preserve unreadable observations for inspection
	index := -1
	for i := range history.Samples {
		if history.Samples[i].ReservationID == id {
			index = i
			break
		}
	}
	if index < 0 {
		history.Samples = append(history.Samples, ProofResourceTiming{ReservationID: id})
		index = len(history.Samples) - 1
	}
	update(&history.Samples[index])
	if len(history.Samples) > proofResourceTimingsLimit {
		history.Samples = history.Samples[len(history.Samples)-proofResourceTimingsLimit:]
	}
	body, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	return home.WriteFileAtomic(filepath.Join(s.home, proofResourceTimingsPath), append(body, '\n'), 0o600)
}

func proofResourceTimingWarning(err error) {
	if err != nil {
		log.Printf("proof resource timing observation skipped: %v", err)
	}
}

func (s *Store) observeProofAdmission(reservation ProofResourceReservation, elapsed time.Duration, observedAt time.Time, queued bool) {
	proofResourceTimingWarning(s.updateProofResourceTiming(reservation.ID, func(sample *ProofResourceTiming) {
		if sample.AdmittedAt != "" {
			return
		} // retry cannot reset the original observation
		sample.WorkID = reservation.WorkID
		sample.Repo = reservation.Profile.Repo
		if reservation.Profile.SourceSnapshot != nil {
			sample.RepositoryFingerprint = reservation.Profile.SourceSnapshot.RepositoryFingerprint
		}
		if !queued {
			elapsedMS, zero := elapsed.Milliseconds(), int64(0)
			sample.AdmissionMS, sample.QueueWaitMS = &elapsedMS, &zero
		}
		sample.AdmittedAt = observedAt.UTC().Format(time.RFC3339Nano)
	}))
}

func (s *Store) observeProofQueue(id string, admission, waiting time.Duration) {
	proofResourceTimingWarning(s.updateProofResourceTiming(id, func(sample *ProofResourceTiming) {
		admissionMS, waitingMS := admission.Milliseconds(), waiting.Milliseconds()
		sample.AdmissionMS, sample.QueueWaitMS = &admissionMS, &waitingMS
	}))
}

func (s *Store) observeProofRelease(id string, observedAt time.Time) {
	proofResourceTimingWarning(s.updateProofResourceTiming(id, func(sample *ProofResourceTiming) {
		if sample.ReleasedAt != "" {
			return
		} // resume never duplicates or extends a completed sample
		sample.ReleasedAt = observedAt.UTC().Format(time.RFC3339Nano)
		admitted, err := time.Parse(time.RFC3339Nano, sample.AdmittedAt)
		if err == nil && !observedAt.Before(admitted) {
			elapsed := observedAt.Sub(admitted).Milliseconds()
			sample.ObservedReservedMS = &elapsed
		}
	}))
}
