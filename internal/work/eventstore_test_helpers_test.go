package work

import (
	"path/filepath"

	"compound/internal/eventstore"
)

// These helpers keep same-package tests on the event-store seam. Production
// code uses the same Store operations directly.
func (s *Store) eventsLog() string {
	return filepath.Join(s.home, "events", "work.jsonl")
}

func (s *Store) envPath(id string) string {
	ref, err := eventstore.ReferenceForID(id)
	if err != nil {
		return ""
	}
	return ref.String()
}

func (s *Store) eventExists(id string) (bool, error) { return s.events.Exists(id) }

func (s *Store) eventBytes(id string) ([]byte, error) {
	record, err := s.events.Read(id)
	if err != nil {
		return nil, err
	}
	return record.Bytes, nil
}

func (s *Store) removeEvent(id string) error { return s.events.Remove(id) }

func (s *Store) replaceEvent(id string, value any) error {
	if err := s.events.Remove(id); err != nil {
		return err
	}
	_, err := s.events.PutJSON(id, value, "  ")
	return err
}
