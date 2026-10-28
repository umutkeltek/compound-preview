package runattempt

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ListByNativeSessionID returns matching attempts, including terminal ones, in
// normal List order. Only the main record is probed for unrelated sessions;
// their historical launch sidecars are not read. An unreadable probe falls
// through the ordinary validated read and remains an error, never absence.
// This is a history lookup for Stop recovery, not a per-tool-call census.
func (s *Store) ListByNativeSessionID(sessionID string) ([]RunAttempt, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("runattempt native session lookup requires a session id")
	}
	attempts, err := s.list(func(id string) bool {
		data, err := os.ReadFile(s.path(id))
		if err != nil {
			return false
		}
		var probe struct {
			Selection *struct {
				SessionID string `json:"native_session_id"`
			} `json:"execution_selection"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			return false
		}
		return probe.Selection == nil || strings.TrimSpace(probe.Selection.SessionID) != sessionID
	})
	var matching []RunAttempt
	for _, attempt := range attempts {
		if attempt.ExecutionSelection != nil &&
			strings.TrimSpace(attempt.ExecutionSelection.NativeSessionID) == sessionID {
			matching = append(matching, attempt)
		}
	}
	return matching, err
}
