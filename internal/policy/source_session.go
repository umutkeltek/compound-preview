package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	homepkg "compound/internal/home"
)

const (
	SourceSessionStateSchema = "compound.source_session_state.v0.1"

	SourceRecordReset   SourceRecordMode = "reset"
	SourceRecordAppend  SourceRecordMode = "append"
	SourceRecordReplace SourceRecordMode = "replace"

	maxActiveSessionSources = 64
)

type SourceRecordMode string

type sourceSessionState struct {
	SchemaVersion string                    `json:"schema_version"`
	SessionKey    string                    `json:"session_key"`
	Sources       []envelope.SourceEnvelope `json:"sources"`
	UpdatedAt     string                    `json:"updated_at"`
}

// SessionSourceStore is Compound's live, content-free lineage projection.
// Source bodies stay at their existing custody boundary; only typed envelopes
// and content hashes are retained here.
type SessionSourceStore struct {
	homePath string
}

func NewSessionSourceStore(homePath string) *SessionSourceStore {
	return &SessionSourceStore{homePath: strings.TrimSpace(homePath)}
}

// Record persists the immutable SourceEnvelope in the event store, then
// updates the bounded per-session active-lineage projection under one lock.
func (s *SessionSourceStore) Record(
	sessionID string,
	source envelope.SourceEnvelope,
	mode SourceRecordMode,
	now time.Time,
) (string, error) {
	if s == nil || s.homePath == "" {
		return "", fmt.Errorf("session source store requires compound home")
	}
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("session source record requires session id")
	}
	if err := source.Validate(); err != nil {
		return "", fmt.Errorf("session source invalid: %w", err)
	}
	switch mode {
	case SourceRecordReset, SourceRecordAppend, SourceRecordReplace:
	default:
		return "", fmt.Errorf("unknown source record mode %q", mode)
	}
	if now.IsZero() {
		now = time.Now()
	}

	events, err := eventstore.Open(s.homePath)
	if err != nil {
		return "", err
	}
	persisted, err := events.PutJSON(source.ID, source, "  ")
	if err != nil {
		return "", fmt.Errorf("persist source envelope: %w", err)
	}

	if err := s.withSessionLock(sessionID, func() error {
		state, err := s.loadState(sessionID)
		if err != nil {
			return err
		}
		switch mode {
		case SourceRecordReset, SourceRecordReplace:
			state.Sources = []envelope.SourceEnvelope{source}
		case SourceRecordAppend:
			state.Sources = appendDistinctSource(state.Sources, source)
			if len(state.Sources) > maxActiveSessionSources {
				state.Sources = append([]envelope.SourceEnvelope(nil), state.Sources[len(state.Sources)-maxActiveSessionSources:]...)
			}
		}
		state.SchemaVersion = SourceSessionStateSchema
		state.SessionKey = sessionSourceKey(sessionID)
		state.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		return s.writeState(sessionID, state)
	}); err != nil {
		return "", err
	}
	return persisted.Reference.String(), nil
}

func (s *SessionSourceStore) Active(sessionID string) ([]envelope.SourceEnvelope, error) {
	if s == nil || s.homePath == "" {
		return nil, fmt.Errorf("session source store requires compound home")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	var active []envelope.SourceEnvelope
	err := s.withSessionLock(sessionID, func() error {
		state, err := s.loadState(sessionID)
		if err != nil {
			return err
		}
		active = append([]envelope.SourceEnvelope(nil), state.Sources...)
		return nil
	})
	return active, err
}

func (s *SessionSourceStore) withSessionLock(sessionID string, fn func() error) error {
	dir := filepath.Join(s.homePath, "context", "source-lineage")
	if err := homepkg.EnsureRootedDirectoryDurable(s.homePath, dir, "source lineage", 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(dir, sessionSourceKey(sessionID)+".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open source lineage lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock source lineage: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *SessionSourceStore) loadState(sessionID string) (sourceSessionState, error) {
	path := s.statePath(sessionID)
	body, err := homepkg.ReadRootedRegularFile(s.homePath, path, "source lineage state")
	if errors.Is(err, os.ErrNotExist) {
		return sourceSessionState{
			SchemaVersion: SourceSessionStateSchema,
			SessionKey:    sessionSourceKey(sessionID),
			Sources:       []envelope.SourceEnvelope{},
		}, nil
	}
	if err != nil {
		return sourceSessionState{}, err
	}
	var state sourceSessionState
	if err := json.Unmarshal(body, &state); err != nil {
		return sourceSessionState{}, fmt.Errorf("parse source lineage state: %w", err)
	}
	if state.SchemaVersion != SourceSessionStateSchema || state.SessionKey != sessionSourceKey(sessionID) {
		return sourceSessionState{}, fmt.Errorf("source lineage state identity mismatch")
	}
	for i := range state.Sources {
		if err := state.Sources[i].Validate(); err != nil {
			return sourceSessionState{}, fmt.Errorf("source lineage state source %d: %w", i, err)
		}
	}
	return state, nil
}

func (s *SessionSourceStore) writeState(sessionID string, state sourceSessionState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := homepkg.WriteFileAtomic(s.statePath(sessionID), body, 0o600); err != nil {
		return fmt.Errorf("write source lineage state: %w", err)
	}
	return nil
}

func (s *SessionSourceStore) statePath(sessionID string) string {
	return filepath.Join(s.homePath, "context", "source-lineage", sessionSourceKey(sessionID)+".json")
}

func sessionSourceKey(sessionID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	return hex.EncodeToString(sum[:])[:24]
}

func appendDistinctSource(sources []envelope.SourceEnvelope, source envelope.SourceEnvelope) []envelope.SourceEnvelope {
	for i := range sources {
		if sources[i].ID == source.ID {
			return sources
		}
	}
	return append(sources, source)
}
