package home

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// leaseLivenessFreshness matches runattempt.DefaultSweepThreshold.
const leaseLivenessFreshness = 30 * time.Minute

type runAttemptFiles interface {
	ReadDir(string) ([]os.DirEntry, error)
	ReadFile(string) ([]byte, error)
}

type osRunAttemptFiles struct{}

func (osRunAttemptFiles) ReadDir(path string) ([]os.DirEntry, error) { return os.ReadDir(path) }
func (osRunAttemptFiles) ReadFile(path string) ([]byte, error)       { return os.ReadFile(path) }

// runAttemptLivenessIndex is a per-scan projection of the newest non-terminal
// heartbeat (or CreatedAt fallback) for each work item. It is never persisted
// or shared across hygiene passes.
type runAttemptLivenessIndex map[string]time.Time

// loadRunAttemptLivenessIndex reads every RunAttempt at most once. Missing or
// unreadable directories and files, malformed records, and blank work ids are
// skipped exactly as the historical best-effort per-work probe skipped them.
func loadRunAttemptLivenessIndex(homePath string, files runAttemptFiles) runAttemptLivenessIndex {
	index := runAttemptLivenessIndex{}
	dir := filepath.Join(homePath, "run_attempts")
	entries, err := files.ReadDir(dir)
	if err != nil {
		return index
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := files.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var attempt struct {
			WorkID          string     `json:"work_id"`
			Status          string     `json:"status"`
			CreatedAt       time.Time  `json:"created_at"`
			LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
		}
		if err := json.Unmarshal(data, &attempt); err != nil || strings.TrimSpace(attempt.WorkID) == "" {
			continue
		}
		switch attempt.Status {
		case "succeeded", "failed", "abandoned", "superseded":
			continue
		}
		last := attempt.CreatedAt
		if attempt.LastHeartbeatAt != nil {
			last = *attempt.LastHeartbeatAt
		}
		last = last.UTC()
		if prior, ok := index[attempt.WorkID]; !ok || last.After(prior) {
			index[attempt.WorkID] = last
		}
	}
	return index
}

func (index runAttemptLivenessIndex) live(workID string, now time.Time) bool {
	if strings.TrimSpace(workID) == "" {
		return false
	}
	last, ok := index[workID]
	return ok && last.After(now.UTC().Add(-leaseLivenessFreshness))
}

// liveRunAttemptForWork retains the fresh, early-exit probe used by the
// mutating archive selector. The read-only hygiene scan uses the bulk index.
func liveRunAttemptForWork(homePath, workID string, now time.Time) bool {
	if strings.TrimSpace(workID) == "" {
		return false
	}
	dir := filepath.Join(homePath, "run_attempts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if fs.ErrNotExist == err || os.IsNotExist(err) {
			return false
		}
		// unreadable dir is read as "no live pane" — the orphan backstop
		// is not the layer that surfaces probe errors.
		return false
	}
	threshold := now.UTC().Add(-leaseLivenessFreshness)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var ra struct {
			WorkID          string     `json:"work_id"`
			Status          string     `json:"status"`
			CreatedAt       time.Time  `json:"created_at"`
			LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
		}
		if jerr := json.Unmarshal(data, &ra); jerr != nil {
			continue
		}
		if ra.WorkID != workID {
			continue
		}
		// only non-terminal panes are alive; succeeded/failed/abandoned/
		// superseded are by definition done.
		switch ra.Status {
		case "succeeded", "failed", "abandoned", "superseded":
			continue
		}
		last := ra.CreatedAt
		if ra.LastHeartbeatAt != nil {
			last = *ra.LastHeartbeatAt
		}
		if last.UTC().After(threshold) {
			return true
		}
	}
	return false
}
