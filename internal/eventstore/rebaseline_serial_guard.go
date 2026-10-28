package eventstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// RebaselineStreamBaselineCapability is the governed capability the rebaseline
// verb mints on its ActionIntent. It is the receipt Skills.Used anchor the
// serial-guard preview uses to count rebaseline-shaped moves in the durable
// event store over the last seven days.
const RebaselineStreamBaselineCapability = "eventstore.stream_baseline.rewrite"

// RebaselineSerialCooldown is the default minimum interval between two
// confirmed rebaselines. It bounds how often the operator can churn the
// frozen-stream baseline; --force-serial overrides the cooldown when an
// operator explicitly accepts the ratchet risk.
const RebaselineSerialCooldown = 24 * time.Hour

// ErrRebaselineCooldownActive is returned by EnforceRebaselineSerialGuard when
// the cooldown has not yet elapsed since the last rebaseline. The CLI maps
// this to exit code 2 (operator must pass --force-serial or wait).
var ErrRebaselineCooldownActive = errors.New("event-store rebaseline cooldown active")

// ErrRebaselineMutatedUnreceipted marks a physical rebaseline outcome where
// the sidecar on disk is the POST-image written by THIS transaction and the
// journaled Receipt / Transition has no durable record AND the compensating
// rollback could not run (also failed). It is the "mutated but unreceipted"
// terminal state the CLI surfaces as exit code 3 — distinct from a
// well-formed recovery finding, which the CLI catches as exit 1.
var ErrRebaselineMutatedUnreceipted = errors.New("event-store rebaseline mutated but unreceipted")

// RebaselineGuardStatus is the read-only projection the verb's preview line
// shows: the timestamp of the last confirmed rebaseline (nil when the current
// sidecar was written by migration completion) and the count of rebaseline
// receipts in the durable event store over the last seven days. The preview
// is informative only — it never blocks execution.
type RebaselineGuardStatus struct {
	LastRebaseline   *time.Time
	MovesInLast7Days int
}

// ReadRebaselineGuardStatus returns the preview-time guard status without
// mutating anything. It is the source of truth for "last rebaseline: ..." and
// "moves in last 7d: ..." on the verb's preview (dry-run) output, and it is
// also what EnforceRebaselineSerialGuard uses to decide whether to refuse a
// second confirmed rebaseline inside the cooldown. A nil LastRebaseline
// always passes the cooldown (the sidecar was written by completion, not by
// an operator rebaseline).
func (s *Store) ReadRebaselineGuardStatus(now time.Time) (RebaselineGuardStatus, error) {
	if s == nil || s.engine == nil {
		return RebaselineGuardStatus{}, fmt.Errorf("event store requires a home")
	}
	status := RebaselineGuardStatus{}
	baseline, ok, err := readStreamBaselineRaw(s.engine.streamBaselinePath())
	if err != nil {
		return RebaselineGuardStatus{}, err
	}
	if ok && baseline.Reason == streamBaselineReasonRebaseline {
		parsed, err := time.Parse(time.RFC3339Nano, baseline.RecordedAt)
		if err != nil {
			return RebaselineGuardStatus{}, fmt.Errorf("parse stream baseline recorded_at: %w", err)
		}
		status.LastRebaseline = &parsed
	}
	moves, err := s.countRebaselineReceiptsInWindow(now, 7*24*time.Hour)
	if err != nil {
		return status, err
	}
	status.MovesInLast7Days = moves
	return status, nil
}

// EnforceRebaselineSerialGuard refuses a confirmed rebaseline when the last
// rebaseline (not completion) is more recent than RebaselineSerialCooldown.
// forceSerial bypasses the cooldown for operators accepting the ratchet risk.
// recovery is true when an existing rebaseline-pending journal is on disk:
// the verb is then resuming a crashed/in-flight rebaseline, not starting a
// fresh one, and the cooldown MUST NOT block recovery. The CLI is expected
// to call this BEFORE WithRebaselineTransaction so the transaction lock is
// never acquired for a refused rebaseline. A home without a sidecar yet
// (never-rebaselined, OR migration not yet complete) is treated as
// LastRebaseline == nil: guard failure is a higher-priority refusal floor
// the verb owns in PreviewStreamBaseline.
func (s *Store) EnforceRebaselineSerialGuard(now time.Time, forceSerial, recovery bool) error {
	if forceSerial || recovery {
		return nil
	}
	status, err := s.ReadRebaselineGuardStatus(now)
	if err != nil {
		// Unreadable guard (e.g. home has no events/ yet — the migration
		// gate hasn't fired). Defer the refusal to the next predicate in
		// the verb rather than masking it with an unrelated io error.
		return nil
	}
	if status.LastRebaseline == nil {
		return nil
	}
	if now.Sub(*status.LastRebaseline) < RebaselineSerialCooldown {
		return fmt.Errorf("%w: last_rebaseline=%s cooldown=%s (use --force-serial to override)",
			ErrRebaselineCooldownActive, status.LastRebaseline.UTC().Format(time.RFC3339Nano), RebaselineSerialCooldown)
	}
	return nil
}

// ArtifactRefFromBytes returns the canonical eventstore artifact ref for a
// given byte body. The CLI uses it to detect "mutated but unreceipted": when
// the on-disk sidecar's artifact ref equals the journaled PostImageRef the
// locked mutation reached disk, and the operator must distinguish that from a
// clean rollback.
func ArtifactRefFromBytes(body []byte) string {
	return rebaselineArtifactRef(body)
}

// IsCurrentSidecarMutated reports whether the bytes tx sees right now at the
// sidecar path are the journaled post-image (i.e. our locked WriteSidecarCAS
// reached disk and any rollback since failed). The CLI layers one receipt
// presence check on top to translate this to the "mutated but unreceipted"
// exit-code-3 signal; this method is the half the eventstore itself owns.
func (tx *RebaselineTransaction) IsCurrentSidecarMutated(postImageRef string) (bool, error) {
	if err := tx.lockActive(); err != nil {
		return false, err
	}
	defer tx.state.mu.Unlock()
	current, existed, err := tx.readSidecarRaw()
	if err != nil {
		return false, err
	}
	if !existed {
		return false, nil
	}
	return rebaselineArtifactRef(current) == postImageRef, nil
}

// ReceiptAbsentForRebaseline reports whether the journaled receipt has NO
// canonical event-store record. A true result plus an IsCurrentSidecarMutated
// true result is the "mutated but unreceipted" terminal state the verb's
// recovery path wraps with ErrRebaselineMutatedUnreceipted for exit code 3.
func (tx *RebaselineTransaction) ReceiptAbsentForRebaseline(receiptID string) (bool, error) {
	if err := tx.lockActive(); err != nil {
		return false, err
	}
	defer tx.state.mu.Unlock()
	if _, err := tx.store.Read(receiptID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// countRebaselineReceiptsInWindow scans receipt records and returns the count
// whose Skills.Used contains the rebaseline capability and whose CreatedAt
// falls in [now-window, now]. Receipts are JSON-decoded only enough to read
// those two fields — the rest of the body is left untouched.
func (s *Store) countRebaselineReceiptsInWindow(now time.Time, window time.Duration) (int, error) {
	entries, err := s.Scan(ScanOptions{Prefixes: []string{"rec_"}})
	if err != nil {
		return 0, err
	}
	count := 0
	minStamp := now.Add(-window)
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			continue
		}
		var header struct {
			CreatedAt string `json:"created_at"`
			Skills    struct {
				Used []string `json:"used"`
			} `json:"skills"`
		}
		if err := json.Unmarshal(record.Bytes, &header); err != nil {
			continue
		}
		usedHit := false
		for _, cap := range header.Skills.Used {
			if cap == RebaselineStreamBaselineCapability {
				usedHit = true
				break
			}
		}
		if !usedHit {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, header.CreatedAt)
		if err != nil {
			continue
		}
		if !ts.Before(minStamp) && !ts.After(now) {
			count++
		}
	}
	return count, nil
}
