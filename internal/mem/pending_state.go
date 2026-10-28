package mem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
)

// PendingState is the reconciled pending-inbox count — the honest backlog
// signal. The raw candidate directory mixes open (proposed) candidates with
// every candidate already closed (parked / promoted / rejected /
// no_promote), so a file count read as "backlog" is a fiction (lived
// 2026-08-25: doctor reported 133k backlog; 34 were actually pending).
//
// PendingState is written ONLY by a full-budget walk that read every inbox
// entry — the promote-driver sweep at walkedAll, or an explicit reconcile —
// never per-write deltas: candidate files have too many writers (capture,
// every adjudication door, page import, stable-candidate adapters) for a
// counter maintained at the write sites to stay truthful. Between
// reconciliations the state is advisory; AsOf names its age so a reader can
// tell a fresh count from a stale one.
type PendingState struct {
	Pending      int    `json:"pending"`                 // candidates in state proposed at reconciliation time
	Files        int    `json:"files"`                   // total candidate files in the inbox directory
	Generation   string `json:"generation,omitempty"`    // sha256 of the sorted candidate JSON name set
	Corrupt      int    `json:"corrupt,omitempty"`       // inbox files unreadable at reconciliation time
	AsOf         string `json:"as_of"`                   // RFC3339 reconciliation time
	ReconciledBy string `json:"reconciled_by,omitempty"` // who reconciled ("promote-driver sweep", "manual", ...)
}

func (s *Store) pendingStatePath() string {
	return filepath.Join(s.home, "state", "inbox-pending.json")
}

// ReadPendingState returns the last reconciled pending count. A missing file
// is not an error in the caller's sense — it means "never reconciled" — but
// it is returned as one here so callers must consciously choose their
// fallback; the sentinel is os.IsNotExist-checkable via the wrapped path
// error readJSON produces ("not found: inbox-pending.json").
func (s *Store) ReadPendingState() (PendingState, error) {
	var st PendingState
	err := readJSON(s.pendingStatePath(), &st)
	return st, err
}

// WritePendingState atomically replaces the reconciled state (stage to a
// temp file in the same directory, rename over). A torn state file must
// never read as a confident-but-wrong count, so the write is atomic even
// though the value itself is advisory between reconciliations.
func (s *Store) WritePendingState(st PendingState) error {
	path := s.pendingStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".inbox-pending-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ReconcilePendingState walks the full inbox exactly once and records the
// true pending count. The census decodes ONLY each descriptor-pinned
// candidate's state field. Any custody corruption fails the reconciliation so
// a symlink, alias, or ID mismatch cannot become a trusted backlog state.
func (s *Store) ReconcilePendingState(reconciledBy string, now time.Time) (PendingState, error) {
	pending, files, corrupt, generation, err := s.pendingInboxCensus()
	if err != nil {
		return PendingState{}, err
	}
	st := PendingState{
		Pending:      pending,
		Files:        files,
		Corrupt:      corrupt,
		Generation:   generation,
		AsOf:         rfc3339(now),
		ReconciledBy: reconciledBy,
	}
	if err := s.WritePendingState(st); err != nil {
		return PendingState{}, err
	}
	return st, nil
}

// pendingInboxCensus counts the inbox in one bounded-memory walk: per file it
// decodes only {"state"}, counting proposed entries. An absent inbox is an
// empty census, not an error. files counts every candidate-root entry;
// corrupt counts entries that would not decode; generation identifies the
// sorted candidate name set so remove-one/add-one cannot masquerade as the
// same corpus merely because the count stayed constant.
func (s *Store) pendingInboxCensus() (pending, files, corrupt int, generation string, err error) {
	page, err := candidates.ReadPage(s.home, candidates.Memory, "", 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			sum := sha256.Sum256(nil)
			return 0, 0, 0, "sha256:" + hex.EncodeToString(sum[:]), nil
		}
		return 0, 0, 0, "", err
	}
	var names []string
	var custodyErrors []error
	for _, entry := range page.Entries {
		name := string(entry.EntryCursor)
		names = append(names, name)
		files++
		if entry.Corruption != nil {
			corrupt++
			custodyErrors = append(custodyErrors, entry.Corruption)
			continue
		}
		var state struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(entry.Record.Body, &state); err != nil {
			corrupt++
			custodyErrors = append(custodyErrors, err)
			continue
		}
		if state.State == "proposed" {
			pending++
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(names, "\x00")))
	generation = "sha256:" + hex.EncodeToString(sum[:])
	if len(custodyErrors) > 0 {
		return pending, files, corrupt, generation, errors.Join(custodyErrors...)
	}
	return pending, files, corrupt, generation, nil
}

func (s *Store) candidateInboxMetadata() (names []string, generation string, newest time.Time, err error) {
	page, err := candidates.ReadPage(s.home, candidates.Memory, "", 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			sum := sha256.Sum256(nil)
			return nil, "sha256:" + hex.EncodeToString(sum[:]), time.Time{}, nil
		}
		return nil, "", time.Time{}, err
	}
	var custodyErrors []error
	for _, entry := range page.Entries {
		name := string(entry.EntryCursor)
		names = append(names, name)
		if entry.Corruption != nil {
			custodyErrors = append(custodyErrors, entry.Corruption)
			continue
		}
		if modifiedAt := entry.Record.ModifiedAt(); modifiedAt.After(newest) {
			newest = modifiedAt
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(names, "\x00")))
	generation = "sha256:" + hex.EncodeToString(sum[:])
	if len(custodyErrors) > 0 {
		return names, generation, newest, errors.Join(custodyErrors...)
	}
	return names, generation, newest, nil
}

// pendingStateFresh reports whether a reconciled pending count is still
// authoritative for the doctor: reconciled recently enough AND against the
// same candidate-name generation. Legacy states without a generation fall
// back to the old count check so stored v0.1 state remains readable.
func pendingStateFresh(st PendingState, fileCount int, generation string, now time.Time) bool {
	asOf, err := time.Parse(time.RFC3339, strings.TrimSpace(st.AsOf))
	if err != nil || now.Sub(asOf) > memDoctorThresholds.PendingStateFreshFor {
		return false
	}
	if strings.TrimSpace(st.Generation) != "" {
		return st.Generation == generation
	}
	return st.Files == fileCount
}
