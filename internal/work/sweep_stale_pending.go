package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

const sweepStalePendingSchemaVersion = "compound.work.sweep_stale_pending.v0.1"

const (
	sweepStaleFaultBeforePendingPersist = "before_pending_persist"
	sweepStaleFaultAfterPendingPersist  = "after_pending_persist"
	sweepStaleFaultBeforeEventPublish   = "before_event_publish"
	sweepStaleFaultBeforeWorkPublish    = "before_work_publish"
	sweepStaleFaultAfterPublication     = "after_publication"
	sweepStaleFaultBeforeLeaseRelease   = "before_lease_release"
	sweepStaleFaultBeforePendingClear   = "before_pending_clear"
)

type sweepStalePendingLease struct {
	Present bool   `json:"present"`
	Body    []byte `json:"body,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

type sweepStalePending struct {
	SchemaVersion           string                 `json:"schema_version"`
	FrameSHA256             string                 `json:"frame_sha256"`
	WorkID                  string                 `json:"work_id"`
	FrozenAt                string                 `json:"frozen_at"`
	ThresholdNanos          int64                  `json:"threshold_nanos"`
	SourceWork              envelope.WorkEnvelope  `json:"source_work"`
	SourceWorkSHA256        string                 `json:"source_work_sha256"`
	FinalWork               envelope.WorkEnvelope  `json:"final_work"`
	FinalWorkSHA256         string                 `json:"final_work_sha256"`
	SourceEventCount        int                    `json:"source_event_count"`
	SourceEventPrefixSHA256 string                 `json:"source_event_prefix_sha256"`
	Event                   Event                  `json:"event"`
	Lease                   sweepStalePendingLease `json:"lease"`
	Candidate               SweepStaleCandidate    `json:"candidate"`
}

func (s *Store) sweepStalePendingPath(workID string) string {
	return s.workPath(workID) + ".sweep-pending"
}

func sweepStaleBytesSHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sweepStalePendingDigest(pending sweepStalePending) (string, error) {
	copyPending := pending
	copyPending.FrameSHA256 = ""
	body, err := canonicalCloseJSON(copyPending)
	if err != nil {
		return "", err
	}
	return sweepStaleBytesSHA256(body), nil
}

func sweepStaleEventPrefixDigest(events []Event) (string, error) {
	body, err := canonicalCloseJSON(events)
	if err != nil {
		return "", err
	}
	return sweepStaleBytesSHA256(body), nil
}

func (s *Store) newSweepStalePending(
	source envelope.WorkEnvelope,
	final envelope.WorkEnvelope,
	candidate SweepStaleCandidate,
	threshold time.Duration,
	now time.Time,
) (sweepStalePending, error) {
	sourceSHA256, err := workEnvelopeSHA256(source)
	if err != nil {
		return sweepStalePending{}, err
	}
	finalSHA256, err := workEnvelopeSHA256(final)
	if err != nil {
		return sweepStalePending{}, err
	}
	events, err := s.eventsFor(source.ID)
	if err != nil {
		return sweepStalePending{}, err
	}
	prefixSHA256, err := sweepStaleEventPrefixDigest(events)
	if err != nil {
		return sweepStalePending{}, err
	}
	lease, err := s.captureSweepStalePendingLease(source.ID)
	if err != nil {
		return sweepStalePending{}, err
	}
	candidate.SourceWorkSHA256 = sourceSHA256
	candidate.State = source.State
	candidate.TargetState = final.State
	candidate.Reclaimed = false
	candidate.ReclaimError = ""
	pending := sweepStalePending{
		SchemaVersion:           sweepStalePendingSchemaVersion,
		WorkID:                  source.ID,
		FrozenAt:                ts(now),
		ThresholdNanos:          int64(threshold),
		SourceWork:              source,
		SourceWorkSHA256:        sourceSHA256,
		FinalWork:               final,
		FinalWorkSHA256:         finalSHA256,
		SourceEventCount:        len(events),
		SourceEventPrefixSHA256: prefixSHA256,
		Event: Event{
			TS:                 ts(now),
			WorkID:             source.ID,
			From:               source.State,
			To:                 final.State,
			By:                 "sweep-stale",
			Note:               candidate.SweepReason,
			PreviousWorkSHA256: sourceSHA256,
			WorkSHA256:         finalSHA256,
		},
		Lease:     lease,
		Candidate: candidate,
	}
	pending.FrameSHA256, err = sweepStalePendingDigest(pending)
	if err != nil {
		return sweepStalePending{}, err
	}
	if err := validateSweepStalePending(pending); err != nil {
		return sweepStalePending{}, err
	}
	return pending, nil
}

func validateSweepStalePending(pending sweepStalePending) error {
	if pending.SchemaVersion != sweepStalePendingSchemaVersion {
		return fmt.Errorf("schema_version %q, want %q", pending.SchemaVersion, sweepStalePendingSchemaVersion)
	}
	if strings.TrimSpace(pending.WorkID) == "" {
		return errors.New("work_id is required")
	}
	if pending.ThresholdNanos <= 0 {
		return errors.New("threshold_nanos must be positive")
	}
	frozenAt, err := parseLifecycleTS(pending.FrozenAt)
	if err != nil || frozenAt.IsZero() || pending.FrozenAt != ts(frozenAt) {
		return fmt.Errorf("frozen_at must be the exact nonzero UnixNano timestamp: %q", pending.FrozenAt)
	}
	if pending.SourceWork.ID != pending.WorkID || pending.FinalWork.ID != pending.WorkID {
		return errors.New("source/final WorkEnvelope IDs must equal work_id")
	}
	if err := pending.SourceWork.Validate(); err != nil {
		return fmt.Errorf("source WorkEnvelope is invalid: %w", err)
	}
	if err := pending.FinalWork.Validate(); err != nil {
		return fmt.Errorf("final WorkEnvelope is invalid: %w", err)
	}
	if !sweepStaleEnvelopeStates[pending.SourceWork.State] {
		return fmt.Errorf("source WorkEnvelope state %q is not reclaimable", pending.SourceWork.State)
	}
	if pending.SourceEventCount <= 0 || strings.TrimSpace(pending.SourceEventPrefixSHA256) == "" {
		return errors.New("source event prefix binding is required")
	}
	sourceSHA256, err := workEnvelopeSHA256(pending.SourceWork)
	if err != nil {
		return fmt.Errorf("seal source work: %w", err)
	}
	if sourceSHA256 != pending.SourceWorkSHA256 {
		return fmt.Errorf("source WorkEnvelope digest %s, want %s", sourceSHA256, pending.SourceWorkSHA256)
	}
	finalSHA256, err := workEnvelopeSHA256(pending.FinalWork)
	if err != nil {
		return fmt.Errorf("seal final work: %w", err)
	}
	if finalSHA256 != pending.FinalWorkSHA256 {
		return fmt.Errorf("final WorkEnvelope digest %s, want %s", finalSHA256, pending.FinalWorkSHA256)
	}
	expectedFinal := pending.SourceWork
	expectedFinal.State = sweepStaleTargetState(pending.SourceWork.State)
	if !reflect.DeepEqual(expectedFinal, pending.FinalWork) {
		return errors.New("final WorkEnvelope must differ from source only by the canonical sweep target state")
	}
	if pending.Candidate.WorkID != pending.WorkID ||
		pending.Candidate.SourceWorkSHA256 != pending.SourceWorkSHA256 ||
		pending.Candidate.State != pending.SourceWork.State ||
		pending.Candidate.TargetState != pending.FinalWork.State ||
		pending.Candidate.Reclaimed ||
		strings.TrimSpace(pending.Candidate.ReclaimError) != "" {
		return errors.New("candidate does not bind the frozen source/final WorkEnvelope generation")
	}
	lastEventAt, err := parseLifecycleTS(pending.Candidate.LastEventTS)
	if err != nil || lastEventAt.IsZero() {
		return errors.New("candidate last_event_ts must be parseable and nonzero")
	}
	lastEventAge, err := time.ParseDuration(pending.Candidate.LastEventAge)
	if err != nil || lastEventAge < 0 {
		return errors.New("candidate last_event_age must be a nonnegative duration")
	}
	expectedAge := frozenAt.Sub(lastEventAt)
	if expectedAge < 0 || lastEventAge != expectedAge.Round(time.Second) ||
		pending.Candidate.LastEventAge != expectedAge.Round(time.Second).String() {
		return errors.New("candidate last_event_age does not match frozen timestamp")
	}
	if expectedAge < time.Duration(pending.ThresholdNanos) {
		return errors.New("candidate last_event_age is below the frozen threshold")
	}
	if pending.Candidate.LiveAttempts != 0 || pending.Candidate.DelegateLive || pending.Candidate.LeaseHeld {
		return errors.New("candidate liveness and lease facts must be inactive")
	}
	if pending.Candidate.LastHeartbeatAt != "" {
		return errors.New("candidate last_heartbeat_at must be blank")
	}
	if pending.Candidate.SweepReason != sweepStaleReason(pending.Candidate, time.Duration(pending.ThresholdNanos)) {
		return errors.New("candidate sweep_reason does not match frozen facts")
	}
	wantEvent := Event{
		TS:                 pending.FrozenAt,
		WorkID:             pending.WorkID,
		From:               pending.SourceWork.State,
		To:                 pending.FinalWork.State,
		By:                 "sweep-stale",
		Note:               pending.Candidate.SweepReason,
		PreviousWorkSHA256: pending.SourceWorkSHA256,
		WorkSHA256:         pending.FinalWorkSHA256,
	}
	if !sameSweepStaleEvent(pending.Event, wantEvent) {
		return errors.New("event does not match the frozen reclaim")
	}
	eventAt, err := parseLifecycleTS(pending.Event.TS)
	if err != nil || eventAt.IsZero() || !eventAt.Equal(frozenAt) || pending.Event.TS != pending.FrozenAt {
		return errors.New("event timestamp must be the exact frozen lifecycle timestamp")
	}
	if pending.Lease.Present {
		if len(pending.Lease.Body) == 0 || pending.Lease.SHA256 != sweepStaleBytesSHA256(pending.Lease.Body) {
			return errors.New("lease preimage digest is invalid")
		}
		var lease Lease
		if err := json.Unmarshal(pending.Lease.Body, &lease); err != nil {
			return fmt.Errorf("decode lease preimage: %w", err)
		}
		if lease.WorkID != pending.WorkID {
			return fmt.Errorf("lease work_id %q, want %q", lease.WorkID, pending.WorkID)
		}
	} else if len(pending.Lease.Body) != 0 || strings.TrimSpace(pending.Lease.SHA256) != "" {
		return errors.New("absent lease cannot carry a body or digest")
	}
	frameSHA256, err := sweepStalePendingDigest(pending)
	if err != nil {
		return err
	}
	if pending.FrameSHA256 != frameSHA256 {
		return fmt.Errorf("frame digest %s, want %s", pending.FrameSHA256, frameSHA256)
	}
	return nil
}

func sameSweepStaleEvent(left, right Event) bool {
	leftBody, leftErr := canonicalCloseJSON(left)
	rightBody, rightErr := canonicalCloseJSON(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBody, rightBody)
}

func (s *Store) captureSweepStalePendingLease(workID string) (sweepStalePendingLease, error) {
	body, err := home.ReadRootedRegularFile(s.home, s.leasePath(workID), "sweep-stale lease")
	if errors.Is(err, os.ErrNotExist) {
		return sweepStalePendingLease{}, nil
	}
	if err != nil {
		return sweepStalePendingLease{}, err
	}
	var lease Lease
	if err := json.Unmarshal(body, &lease); err != nil {
		return sweepStalePendingLease{}, fmt.Errorf("decode lease for %s: %w", workID, err)
	}
	if lease.WorkID != workID {
		return sweepStalePendingLease{}, fmt.Errorf("lease work_id %q, want %q", lease.WorkID, workID)
	}
	return sweepStalePendingLease{Present: true, Body: body, SHA256: sweepStaleBytesSHA256(body)}, nil
}

func (s *Store) loadSweepStalePending(workID string) (sweepStalePending, bool, error) {
	// Read through the physical removal chain, not just the canonical name:
	// an interrupted clear leaves the frozen frame quarantined under a
	// .remove-pending residue with the canonical name gone. That residue is
	// still the live transaction authority for a rerun (and for the mutation
	// fence below); a plain canonical read would mistake the quarantined
	// frame for a completed transaction and orphan the residue.
	body, present, err := home.ReadRootedRegularFileRemovalChain(s.home, s.sweepStalePendingPath(workID), "sweep-stale pending")
	if err != nil {
		return sweepStalePending{}, false, err
	}
	if !present {
		return sweepStalePending{}, false, nil
	}
	var pending sweepStalePending
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil {
		return pending, true, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return pending, true, errors.New("sweep-stale pending contains trailing JSON")
	}
	if err := validateSweepStalePending(pending); err != nil {
		return pending, true, err
	}
	return pending, true, nil
}

func (s *Store) persistSweepStalePending(pending sweepStalePending) error {
	if err := validateSweepStalePending(pending); err != nil {
		return err
	}
	staged, err := s.stageJSON(s.sweepStalePendingPath(pending.WorkID), &pending)
	if err != nil {
		return err
	}
	if err := commitStagedJSONExclusive(staged, s.sweepStalePendingPath(pending.WorkID)); err != nil {
		_ = os.Remove(staged)
		return err
	}
	loaded, present, err := s.loadSweepStalePending(pending.WorkID)
	if err != nil {
		return err
	}
	if !present || loaded.FrameSHA256 != pending.FrameSHA256 {
		return errors.New("published sweep-stale pending frame does not match the frozen intent")
	}
	return nil
}

func (s *Store) clearSweepStalePending(pending sweepStalePending) error {
	return home.RemoveRootedRegularFileDurableValidated(
		s.home,
		s.sweepStalePendingPath(pending.WorkID),
		"sweep-stale pending",
		func(body []byte) error {
			var current sweepStalePending
			if err := json.Unmarshal(body, &current); err != nil {
				return err
			}
			if err := validateSweepStalePending(current); err != nil {
				return err
			}
			if current.FrameSHA256 != pending.FrameSHA256 {
				return errors.New("sweep-stale pending frame changed before clear")
			}
			return nil
		},
	)
}

func (s *Store) injectSweepStaleFault(stage string) error {
	if s.sweepStaleFault == nil {
		return nil
	}
	return s.sweepStaleFault(stage)
}

func sweepStaleRecoveryRequired(workID string, err error) error {
	return fmt.Errorf("sweep-stale recovery_required for work %s: %w", workID, err)
}

func (s *Store) completeSweepStalePending(pending sweepStalePending) error {
	if err := validateSweepStalePending(pending); err != nil {
		return err
	}
	published, err := s.sweepStalePublicationComplete(pending)
	if err != nil {
		return err
	}
	if err := s.verifySweepStaleLease(pending, published); err != nil {
		return err
	}
	if !published {
		if err := s.publishSweepStaleEventAndWork(pending); err != nil {
			return err
		}
	}
	if err := s.injectSweepStaleFault(sweepStaleFaultAfterPublication); err != nil {
		return err
	}
	published, err = s.sweepStalePublicationComplete(pending)
	if err != nil {
		return err
	}
	if !published {
		return errors.New("sweep-stale event/work publication is incomplete")
	}
	if err := s.injectSweepStaleFault(sweepStaleFaultBeforeLeaseRelease); err != nil {
		return err
	}
	if err := s.removeSweepStaleLease(pending); err != nil {
		return err
	}
	if err := s.injectSweepStaleFault(sweepStaleFaultBeforePendingClear); err != nil {
		return err
	}
	return s.clearSweepStalePending(pending)
}

func (s *Store) sweepStalePublicationComplete(pending sweepStalePending) (bool, error) {
	var complete bool
	err := s.withWorkEventLock(func() error {
		_, suffix, err := s.validateSweepStaleEventPrefixUnlocked(pending)
		if err != nil {
			return err
		}
		current, err := s.Get(pending.WorkID)
		if err != nil {
			return err
		}
		currentSHA256, err := workEnvelopeSHA256(current)
		if err != nil {
			return err
		}
		switch currentSHA256 {
		case pending.SourceWorkSHA256:
			complete = false
		case pending.FinalWorkSHA256:
			complete = len(suffix) == 1 && sameSweepStaleEvent(suffix[0], pending.Event)
			if !complete {
				return errors.New("final WorkEnvelope exists without the exact frozen sweep event")
			}
		default:
			return fmt.Errorf("current WorkEnvelope matches neither frozen source nor final digest: %s", currentSHA256)
		}
		return nil
	})
	return complete, err
}

func (s *Store) validateSweepStaleEventPrefixUnlocked(pending sweepStalePending) ([]Event, []Event, error) {
	events, err := s.readEventsStrictUnlocked(pending.WorkID)
	if err != nil {
		return nil, nil, err
	}
	if pending.SourceEventCount <= 0 {
		return nil, nil, errors.New("frozen sweep event prefix must contain a last lifecycle event")
	}
	if len(events) < pending.SourceEventCount {
		return nil, nil, errors.New("work lifecycle event prefix was truncated")
	}
	prefixSHA256, err := sweepStaleEventPrefixDigest(events[:pending.SourceEventCount])
	if err != nil {
		return nil, nil, err
	}
	if prefixSHA256 != pending.SourceEventPrefixSHA256 {
		return nil, nil, errors.New("work lifecycle event prefix changed after sweep intent")
	}
	if events[pending.SourceEventCount-1].TS != pending.Candidate.LastEventTS {
		return nil, nil, errors.New("candidate last_event_ts does not match the frozen event prefix")
	}
	suffix := events[pending.SourceEventCount:]
	if len(suffix) > 1 {
		return nil, nil, errors.New("multiple lifecycle events follow the frozen sweep prefix")
	}
	if len(suffix) == 1 && !sameSweepStaleEvent(suffix[0], pending.Event) {
		return nil, nil, errors.New("competing lifecycle event follows the frozen sweep prefix")
	}
	return events, suffix, nil
}

func (s *Store) publishSweepStaleEventAndWork(pending sweepStalePending) error {
	return s.withWorkEventLock(func() error {
		_, suffix, err := s.validateSweepStaleEventPrefixUnlocked(pending)
		if err != nil {
			return err
		}
		current, err := s.Get(pending.WorkID)
		if err != nil {
			return err
		}
		currentSHA256, err := workEnvelopeSHA256(current)
		if err != nil {
			return err
		}
		publishWork := func() error {
			if err := s.injectSweepStaleFault(sweepStaleFaultBeforeWorkPublish); err != nil {
				return err
			}
			return s.publishClaimJSON(s.workPath(pending.WorkID), &pending.FinalWork)
		}
		switch currentSHA256 {
		case pending.SourceWorkSHA256:
			switch len(suffix) {
			case 0:
				return s.appendEventControlledUnlocked(
					pending.Event,
					func() error { return s.injectSweepStaleFault(sweepStaleFaultBeforeEventPublish) },
					publishWork,
				)
			case 1:
				return publishWork()
			default:
				return errors.New("unexpected sweep event suffix")
			}
		case pending.FinalWorkSHA256:
			if len(suffix) != 1 {
				return errors.New("final WorkEnvelope exists without the exact frozen sweep event")
			}
			return nil
		default:
			return fmt.Errorf("current WorkEnvelope matches neither frozen source nor final digest: %s", currentSHA256)
		}
	})
}

func (s *Store) verifySweepStaleLease(pending sweepStalePending, allowReleased bool) error {
	body, err := home.ReadRootedRegularFile(s.home, s.leasePath(pending.WorkID), "sweep-stale lease")
	if errors.Is(err, os.ErrNotExist) {
		if pending.Lease.Present && !allowReleased {
			return errors.New("frozen claim lease disappeared before sweep publication")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !pending.Lease.Present {
		return errors.New("a claim lease appeared after sweep intent")
	}
	if sweepStaleBytesSHA256(body) != pending.Lease.SHA256 || !bytes.Equal(body, pending.Lease.Body) {
		return errors.New("claim lease changed after sweep intent")
	}
	return nil
}

func (s *Store) removeSweepStaleLease(pending sweepStalePending) error {
	if !pending.Lease.Present {
		return s.verifySweepStaleLease(pending, true)
	}
	err := home.RemoveRootedRegularFileDurableValidated(
		s.home,
		s.leasePath(pending.WorkID),
		"sweep-stale lease",
		func(body []byte) error {
			if sweepStaleBytesSHA256(body) != pending.Lease.SHA256 || !bytes.Equal(body, pending.Lease.Body) {
				return errors.New("claim lease changed before release")
			}
			return nil
		},
	)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) reconcileSweepStalePendingLocked(workID string) (SweepStaleCandidate, error) {
	pending, present, err := s.loadSweepStalePending(workID)
	if err != nil {
		return SweepStaleCandidate{}, err
	}
	if !present {
		return SweepStaleCandidate{}, os.ErrNotExist
	}
	if err := s.completeSweepStalePending(pending); err != nil {
		return pending.Candidate, sweepStaleRecoveryRequired(workID, err)
	}
	candidate := pending.Candidate
	candidate.Reclaimed = true
	return candidate, nil
}

func (s *Store) fenceSweepStalePendingMutation(workID string) error {
	pending, present, err := s.loadSweepStalePending(workID)
	if err != nil {
		return sweepStaleRecoveryRequired(workID, fmt.Errorf("read sweep-stale pending for %s: %w", workID, err))
	}
	if !present {
		return nil
	}
	return sweepStaleRecoveryRequired(workID, fmt.Errorf("frozen frame %s must be reconciled before another lifecycle mutation", pending.FrameSHA256))
}
