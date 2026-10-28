package projection

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ContextRecordSchemaVersion = "compound.projection_context.v0.1"

// ContextRecord is a persisted orientation snapshot for generated-view
// freshness. It is context evidence only; it never proves source truth.
type ContextRecord struct {
	SchemaVersion string    `json:"schema_version"`
	ID            string    `json:"id"`
	CreatedAt     string    `json:"created_at"`
	RepoKinds     []string  `json:"repo_kinds"`
	Freshness     Freshness `json:"freshness"`
}

func RecordContext(homePath string, f Freshness, repoKinds []string, now time.Time) (ContextRecord, string, error) {
	if strings.TrimSpace(homePath) == "" {
		return ContextRecord{}, "", fmt.Errorf("projection context record requires COMPOUND_HOME")
	}
	repoKinds = normalizeRepoKinds(repoKinds)
	rec := ContextRecord{
		SchemaVersion: ContextRecordSchemaVersion,
		ID:            contextRecordID(f, repoKinds),
		CreatedAt:     now.UTC().Format(time.RFC3339),
		RepoKinds:     repoKinds,
		Freshness:     f,
	}
	path := filepath.Join(contextDir(homePath), rec.ID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ContextRecord{}, "", err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return ContextRecord{}, "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return ContextRecord{}, "", err
	}
	return rec, path, nil
}

func LoadContext(homePath, repoKind string) ([]ContextRecord, error) {
	if strings.TrimSpace(homePath) == "" {
		return nil, nil
	}
	dir := contextDir(homePath)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []ContextRecord{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read projection context %s: %w", path, err)
		}
		var rec ContextRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("parse projection context %s: %w", path, err)
		}
		if rec.SchemaVersion != ContextRecordSchemaVersion {
			return nil, fmt.Errorf("projection context %s schema %q, want %q", path, rec.SchemaVersion, ContextRecordSchemaVersion)
		}
		if containsKind(rec.RepoKinds, repoKind) {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func ContextRef(f Freshness) string {
	id := strings.TrimPrefix(strings.TrimSpace(f.ProjectionID), "projection:")
	if id == "" {
		id = "unknown"
	}
	return "projection://" + id
}

func ContextStatus(status string) string {
	switch status {
	case StatusFresh:
		return "current"
	case StatusStale:
		return "stale"
	case StatusDrifted:
		return "disputed"
	default:
		return "unknown"
	}
}

func normalizeRepoKinds(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		out = []string{"control-plane"}
	}
	sort.Strings(out)
	return out
}

func containsKind(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func contextDir(homePath string) string {
	return filepath.Join(homePath, "projections", "context")
}

func contextRecordID(f Freshness, repoKinds []string) string {
	sum := sha([]byte(strings.Join([]string{
		f.ProjectionID,
		f.ViewRef,
		fmt.Sprintf("%d", f.SourceLedgerSeq),
		f.OutputHash,
		f.Status,
		strings.Join(repoKinds, ","),
	}, "|")))
	return "pctx_" + strings.TrimPrefix(sum, "sha256:")[:12]
}
