package eventstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// RebaselineRecoveryRecord is the minimal journal metadata the eventstore
// recovery-finding helper needs to write a durable Finding without depending
// on the command-adapter's typed rebaselinePending shape. Every field comes
// straight from the parsed physical journal or its bound receipt/transition
// envelopes, so callers can populate it without re-implementing the parsing.
type RebaselineRecoveryRecord struct {
	PreImageRef    string
	PostImageRef   string
	ReceiptID      string
	TransitionID   string
	ActionIntentID string
	WorkID         string
	PriorExisted   bool
	ReceiptOutcome string
	PendingRaw     []byte
}

// RebaselineFinding is the exact body contract the recovery path hands the
// writer: producer/topic/work/evidence-bind so the durable Findings ledger is
// shaped identically regardless of which adapter called the eventstore.
type RebaselineFinding struct {
	Topic        string
	Body         string
	WorkID       string
	EvidenceRefs []string
}

// ArtifactPutter is the durable artifact writer the recovery path uses. The
// cmd adapter passes home.ArtifactStore; tests can pass a fake.
type ArtifactPutter interface {
	Put(body []byte) (string, error)
}

// RebaselineRecoveryFindingFn is the durable finding writer seam. Production
// wires it to internal/finding.Record; tests inject a recorder that captures
// the body for exact-contract assertions. The returned string is the durable
// finding id, which the wrapper embeds in the surfaced recovery error so
// operators and tests can name the recorded finding by id.
type RebaselineRecoveryFindingFn func(homePath string, finding RebaselineFinding, now time.Time) (string, error)

// RecordRebaselineRecoveryFinding is the eventstore-side helper the verb's
// recovery path calls when the locked transaction reaches a state from which
// the locked state machine cannot continue safely (the sidecar is on disk in
// an unrolled-back state, the journaled Receipt has no durable proof, or the
// rebaseline's journal is malformed). It artifactizes the actual on-disk
// state plus the raw pending marker (so an operator can recover without any
// non-durable context) and writes the durable Finding — then wraps the error
// with ErrRebaselineMutatedUnreceipted when the sidecar is the journaled
// post-image AND the Receipt physical proof is absent (exit code 3), keeping
// the well-formed-recovery (exit code 1) outcome distinct.
func RecordRebaselineRecoveryFinding(homePath string, artifacts ArtifactPutter, tx *RebaselineTransaction, rec RebaselineRecoveryRecord, now time.Time, cause error, writeFinding RebaselineRecoveryFindingFn) error {
	current, currentExisted, readErr := tx.ReadSidecar()
	if readErr != nil {
		return fmt.Errorf("rebaseline recovery required (%v); read actual state: %w", cause, readErr)
	}
	currentRef, artifactErr := artifacts.Put(current)
	if artifactErr != nil {
		return fmt.Errorf("rebaseline recovery required (%v); artifact actual state: %w", cause, artifactErr)
	}
	markerRef := ""
	if len(rec.PendingRaw) > 0 {
		markerRef, artifactErr = artifacts.Put(rec.PendingRaw)
		if artifactErr != nil {
			return fmt.Errorf("rebaseline recovery required (%v); artifact raw pending marker: %w", cause, artifactErr)
		}
	}
	body := fmt.Sprintf("event-store rebaseline manual recovery required.\nsidecar_path: %s\nprior_existed: %t\ncurrent_existed: %t\npre_image_ref: %s\nexpected_post_ref: %s\ncurrent_state_ref: %s\npending_marker_ref: %s\naction_intent_id: %s\nreceipt_id: %s\ntransition_id: %s\ncause: %v\n", tx.SidecarPath(), rec.PriorExisted, currentExisted, rec.PreImageRef, rec.PostImageRef, currentRef, markerRef, rec.ActionIntentID, rec.ReceiptID, rec.TransitionID, cause)
	evidenceRefs := nonEmptyRefs(rec.PreImageRef, currentRef, markerRef)
	finding := RebaselineFinding{Topic: "eventstore-rebaseline-recovery/stream-baseline", Body: body, WorkID: rec.WorkID, EvidenceRefs: evidenceRefs}
	if writeFinding == nil {
		return fmt.Errorf("rebaseline recovery required (%v); no finding writer bound", cause)
	}
	findingID, err := writeFinding(homePath, finding, now)
	if err != nil {
		return fmt.Errorf("rebaseline recovery required (%v); recovery finding failed: %w", cause, err)
	}
	wrapped := fmt.Errorf("rebaseline recovery finding %s recorded; journal retained; MANUAL RECOVERY REQUIRED: %v", findingID, cause)
	mutated, mutatedErr := tx.IsCurrentSidecarMutated(rec.PostImageRef)
	if mutatedErr != nil {
		return fmt.Errorf("rebaseline recovery required (%v); sidecar presence unprovable: %w", cause, mutatedErr)
	}
	if !mutated {
		return wrapped
	}
	absent, absentErr := tx.ReceiptAbsentForRebaseline(rec.ReceiptID)
	if absentErr != nil {
		return fmt.Errorf("rebaseline recovery required (%v); receipt presence unprovable: %w", cause, absentErr)
	}
	if absent {
		return fmt.Errorf("%w: %w", ErrRebaselineMutatedUnreceipted, wrapped)
	}
	return wrapped
}

func nonEmptyRefs(refs ...string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

// PhysicalProofPresence is the eventstore-side equivalent of the CLI
// adapter's proofPresence check. It exists so the recovery path can be
// invoked outside the verb (migrations, repair, etc.) without forcing each
// caller to import the envelope package.
type PhysicalProofPresence int

const (
	PhysicalProofAbsent PhysicalProofPresence = iota
	PhysicalProofExact
	PhysicalProofConflict
)

// PhysicalProofPresenceOf reports whether an event with the given canonical
// bytes exists in the store and is byte-identical to the journaled proof.
func (s *Store) PhysicalProofPresenceOf(id string, wantCanonical []byte) (PhysicalProofPresence, error) {
	stored, err := s.Read(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PhysicalProofAbsent, nil
		}
		return PhysicalProofAbsent, err
	}
	compact, err := RebaselineCanonicalJSON(stored.Bytes)
	if err != nil {
		return PhysicalProofConflict, nil
	}
	if bytes.Equal(compact, wantCanonical) {
		return PhysicalProofExact, nil
	}
	return PhysicalProofConflict, nil
}

// RebaselineCanonicalJSON is the envelope-style canonicalization helper the
// CLI's exact-event presence check uses. JSON may carry unknown future
// fields; the check tolerates them by round-tripping the body through this
// canonicalize step rather than comparing raw bytes.
func RebaselineCanonicalJSON(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return json.Marshal(value)
}
