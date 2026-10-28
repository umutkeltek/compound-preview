package runattempt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ForWork discovers membership from current main records, including attempts
// created by older writers and attempts never linked to a WorkEnvelope. It
// freshly validates owned launch evidence without reading unrelated sidecars.
// Main-record enumeration is intentionally retained: an unproven cached
// membership list could hide a prepared close after a crash.
func (s *Store) ForWork(workID string) ([]RunAttempt, error) {
	if strings.TrimSpace(workID) == "" || strings.TrimSpace(workID) != workID ||
		filepath.Base(workID) != workID || workID == "." || workID == ".." {
		return nil, fmt.Errorf("runattempt for-work: invalid work id %q", workID)
	}
	entries, err := os.ReadDir(filepath.Join(s.home, "run_attempts"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RunAttempt
	var issues []RunAttemptReadIssue
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !Valid(id) {
			continue
		}
		s.mu.Lock()
		delete(s.cache, id)
		s.mu.Unlock()
		body, err := os.ReadFile(s.path(id))
		var route RunAttempt
		if err == nil {
			err = json.Unmarshal(body, &route)
		}
		// Inline launch data has never been authority.
		route.Launch = nil
		if err == nil && route.ID != id {
			err = fmt.Errorf("embedded identity %q contradicts named record %q", route.ID, id)
		}
		if err == nil {
			err = validateDecodedRunAttempt(route)
		}
		if err != nil {
			issues = append(issues, RunAttemptReadIssue{ID: id, Cause: err})
			continue
		}
		if route.WorkID != workID {
			continue
		}
		fresh, err := s.readFresh(id)
		if err == nil && (fresh.ID != id || fresh.WorkID != workID) {
			err = fmt.Errorf("attempt membership changed during for-work lookup")
		}
		if err != nil {
			issues = append(issues, RunAttemptReadIssue{ID: id, Cause: err})
			continue
		}
		out = append(out, fresh)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if len(issues) > 0 {
		return out, &UnreadableRunAttemptsError{Issues: issues}
	}
	return out, nil
}
