package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A session announcement is the missing link between a native harness session
// and a Compound actor. The hook knows (harness, session_id, cwd); leases and
// work events know the actor; nothing joined them, so a session Compound did
// not spawn could never be identified as a recipient.
//
// An announcement is a CLAIM, deliberately stored observed-class and never in
// the ledger. Any local process can assert any identity, so a declaration on
// its own buys nothing. It becomes usable only when presence -- built from
// leases and work events, evidence this file never writes -- independently
// shows that actor active. That is the conjunctive rule the recipient resolver
// already defends: neither self-declaration nor proximity alone may adopt a
// session.
const (
	PresenceSessionSchemaVersion = "compound.presence_session.v0.1"
	presenceSessionDirName       = "presence-sessions"
)

// PresenceSessionAnnouncement is one session's self-declared identity.
type PresenceSessionAnnouncement struct {
	SchemaVersion string `json:"schema_version"`
	Key           string `json:"key"`
	Harness       string `json:"harness"`
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	Actor         string `json:"actor"`
	FirstSeenAt   string `json:"first_announced_at"`
	LastSeenAt    string `json:"last_announced_at"`
	Announces     int    `json:"announce_count"`
}

// PresenceSessionInput is the declaration a session makes about itself.
type PresenceSessionInput struct {
	Harness   string
	SessionID string
	Cwd       string
	Actor     string
}

func (in PresenceSessionInput) normalized() (PresenceSessionInput, error) {
	in.Harness = normalizeCollaborationHarness(in.Harness)
	in.SessionID = strings.TrimSpace(in.SessionID)
	in.Cwd = strings.TrimSpace(in.Cwd)
	in.Actor = strings.TrimSpace(in.Actor)
	if in.Harness == "" {
		return in, errors.New("presence announce: --harness is required")
	}
	if in.SessionID == "" {
		return in, errors.New("presence announce: --session is required")
	}
	if in.Cwd == "" {
		return in, errors.New("presence announce: --cwd is required")
	}
	if in.Actor == "" {
		return in, errors.New("presence announce: --actor is required")
	}
	canonical, err := canonicalCollaborationPath(in.Cwd)
	if err != nil {
		return in, fmt.Errorf("presence announce: %w", err)
	}
	in.Cwd = canonical
	return in, nil
}

// presenceSessionKey identifies the (harness, session, cwd) triple. The actor
// is deliberately NOT part of the key: re-announcing the same session under a
// different actor must overwrite the claim rather than accumulate a second one,
// so a session can never appear to be two actors at once.
func presenceSessionKey(in PresenceSessionInput) string {
	sum := sha256.Sum256([]byte(in.Harness + "|" + in.SessionID + "|" + in.Cwd))
	return hex.EncodeToString(sum[:])
}

func (s *Store) presenceSessionDir() string {
	return filepath.Join(s.home, "observed", presenceSessionDirName)
}

// AnnouncePresenceSession records a session's claimed actor. It is idempotent
// per (harness, session, cwd): repeat announcements refresh last-seen and bump
// the count rather than creating rivals.
func (s *Store) AnnouncePresenceSession(in PresenceSessionInput, now time.Time) (PresenceSessionAnnouncement, error) {
	normalized, err := in.normalized()
	if err != nil {
		return PresenceSessionAnnouncement{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	dir := s.presenceSessionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return PresenceSessionAnnouncement{}, err
	}
	key := presenceSessionKey(normalized)
	path := filepath.Join(dir, key+".json")

	record := PresenceSessionAnnouncement{
		SchemaVersion: PresenceSessionSchemaVersion,
		Key:           key,
		Harness:       normalized.Harness,
		SessionID:     normalized.SessionID,
		Cwd:           normalized.Cwd,
		Actor:         normalized.Actor,
		FirstSeenAt:   now.UTC().Format(time.RFC3339),
		LastSeenAt:    now.UTC().Format(time.RFC3339),
		Announces:     1,
	}
	if existing, err := readPresenceSession(path); err == nil {
		record.FirstSeenAt = existing.FirstSeenAt
		record.Announces = existing.Announces + 1
	} else if !errors.Is(err, os.ErrNotExist) {
		return PresenceSessionAnnouncement{}, err
	}

	blob, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return PresenceSessionAnnouncement{}, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(blob, '\n'), 0o644); err != nil {
		return PresenceSessionAnnouncement{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return PresenceSessionAnnouncement{}, err
	}
	return record, nil
}

func readPresenceSession(path string) (PresenceSessionAnnouncement, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return PresenceSessionAnnouncement{}, err
	}
	var record PresenceSessionAnnouncement
	if err := json.Unmarshal(blob, &record); err != nil {
		return PresenceSessionAnnouncement{}, err
	}
	return record, nil
}

// exactPresenceSession distinguishes an absent declaration from a present but
// unusable one. Delivery authority must not use the tolerant inventory reader:
// skipping a corrupt exact record would allow a weaker legacy binding to win.
// Only the file owned by these normalized coordinates is read; unrelated bad
// announcements neither establish nor prevent this recipient's identity.
func (s *Store) exactPresenceSession(query CollaborationRecipientQuery) (PresenceSessionAnnouncement, bool, error) {
	cwd, err := canonicalCollaborationPath(query.Cwd)
	if err != nil {
		return PresenceSessionAnnouncement{}, false, err
	}
	in := PresenceSessionInput{
		Harness:   normalizeCollaborationHarness(query.Harness),
		SessionID: strings.TrimSpace(query.SessionID), Cwd: cwd,
	}
	key := presenceSessionKey(in)
	path := filepath.Join(s.presenceSessionDir(), key+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return PresenceSessionAnnouncement{}, false, nil
	}
	if err != nil {
		return PresenceSessionAnnouncement{}, true, fmt.Errorf("read exact session declaration: %w", err)
	}
	if !info.Mode().IsRegular() {
		return PresenceSessionAnnouncement{}, true, errors.New("exact session declaration is not a regular owned file")
	}
	record, err := readPresenceSession(path)
	if err != nil {
		return PresenceSessionAnnouncement{}, true, fmt.Errorf("read exact session declaration: %w", err)
	}
	if record.SchemaVersion != PresenceSessionSchemaVersion || record.Key != key ||
		record.Harness != in.Harness || record.SessionID != in.SessionID || record.Cwd != in.Cwd ||
		strings.TrimSpace(record.Actor) == "" {
		return PresenceSessionAnnouncement{}, true, errors.New("exact session declaration has invalid identity")
	}
	return record, true, nil
}

// ListPresenceSessions returns every announcement, newest declaration first.
func (s *Store) ListPresenceSessions() ([]PresenceSessionAnnouncement, error) {
	entries, err := os.ReadDir(s.presenceSessionDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	records := make([]PresenceSessionAnnouncement, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, err := readPresenceSession(filepath.Join(s.presenceSessionDir(), entry.Name()))
		if err != nil {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].LastSeenAt != records[j].LastSeenAt {
			return records[i].LastSeenAt > records[j].LastSeenAt
		}
		return records[i].Key < records[j].Key
	})
	return records, nil
}
