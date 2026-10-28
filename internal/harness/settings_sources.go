package harness

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// SettingSource declares an executor-owned observation surface, never another
// defaults owner. Mappings go from Compound-owned native key to source key.
type SettingSource struct {
	ID       string            `json:"id" yaml:"id"`
	Owner    string            `json:"owner" yaml:"owner"`
	Format   string            `json:"format" yaml:"format"`
	Path     string            `json:"path" yaml:"path"`
	Mappings map[string]string `json:"mappings" yaml:"mappings"`
}

type SettingSourceKey struct {
	NativeKey string `json:"native_key"`
	SourceKey string `json:"source_key"`
	Status    string `json:"status"`
}

// No values or file bodies belong in a settings observation report.
type SettingSourceReport struct {
	ID       string             `json:"id"`
	Owner    string             `json:"owner"`
	Format   string             `json:"format"`
	Revision string             `json:"revision,omitempty"`
	Status   string             `json:"status"`
	Keys     []SettingSourceKey `json:"keys"`
}

// PermissionDrift combines the existing native posture probe with declared
// launcher observations. Unavailable declarations are not a clean posture.
func PermissionDrift(homePath, name string) (PermissionDriftReport, error) {
	policy, revision, err := LoadPermissionAuthority(homePath, name)
	if err != nil {
		return PermissionDriftReport{}, err
	}
	report, err := permissionNativeDrift(homePath, name)
	if err != nil {
		return report, err
	}
	_, afterRevision, err := LoadPermissionAuthority(homePath, name)
	if err != nil {
		return report, err
	}
	if revision != afterRevision {
		return report, fmt.Errorf("settings authority changed during probe")
	}
	report.SettingSources = ObserveSettingSources(policy)
	for _, source := range report.SettingSources {
		if source.Status != "matches" {
			report.Drifted = true
			report.Details = append(report.Details, "declared setting source "+source.ID+": "+source.Status)
		}
	}
	return report, nil
}

var shellSettingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var shellSettingAssignment = regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

func ObserveSettingSources(policy PermissionAuthority) []SettingSourceReport {
	reports := make([]SettingSourceReport, 0, len(policy.SettingSources))
	seen := map[string]bool{}
	for _, source := range policy.SettingSources {
		row := SettingSourceReport{ID: source.ID, Owner: source.Owner, Format: source.Format, Status: "invalid_declaration", Keys: []SettingSourceKey{}}
		keys := make([]string, 0, len(source.Mappings))
		for key := range source.Mappings {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		valid := source.ID != "" && source.Owner != "" && filepath.IsAbs(source.Path) && len(keys) > 0 && !seen[source.ID]
		seen[source.ID] = true
		envKeys := map[string]bool{}
		for _, key := range keys {
			native := source.Mappings[key]
			_, owned := policy.Settings[key]
			_, scalar := policy.Settings[key].(string)
			if !owned || !scalar || !shellSettingName.MatchString(native) || envKeys[native] {
				valid = false
			}
			envKeys[native] = true
			row.Keys = append(row.Keys, SettingSourceKey{NativeKey: key, SourceKey: native, Status: "unverified"})
		}
		if !valid {
			reports = append(reports, row)
			continue
		}
		if source.Format != "shell_env" {
			row.Status = "unsupported"
			reports = append(reports, row)
			continue
		}
		body, err := readSettingSource(source.Path)
		if err != nil {
			row.Status = "unavailable"
			reports = append(reports, row)
			continue
		}
		sum := sha256.Sum256(body)
		row.Revision = fmt.Sprintf("sha256:%x", sum)
		values, statuses := literalShellSettings(string(body), envKeys)
		row.Status = "matches"
		for i := range row.Keys {
			key := &row.Keys[i]
			key.Status = statuses[key.SourceKey]
			if key.Status == "" {
				key.Status = "missing"
			}
			if key.Status == "literal" {
				if values[key.SourceKey] == policy.Settings[key.NativeKey].(string) {
					key.Status = "matches"
				} else {
					key.Status = "mismatch"
				}
			}
			if key.Status != "matches" {
				row.Status = "drifted"
			}
		}
		reports = append(reports, row)
	}
	return reports
}

// Bounded regular-file reads; O_NONBLOCK avoids a raced FIFO blocking a probe.
// The parent may be a platform alias, but the declared leaf cannot be a symlink.
func readSettingSource(path string) ([]byte, error) {
	const limit = 128 * 1024
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("source unavailable")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("source unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() > limit {
		return nil, fmt.Errorf("source unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(body) > limit {
		return nil, fmt.Errorf("source unavailable")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) || !after.Mode().IsRegular() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, fmt.Errorf("source changed")
	}
	return body, nil
}

func literalShellSettings(body string, wanted map[string]bool) (map[string]string, map[string]string) {
	values := map[string]string{}
	statuses := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := shellSettingAssignment.FindStringSubmatch(line)
		if len(parts) != 3 {
			for key := range wanted {
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `\b`).MatchString(line) {
					statuses[key] = "not_literal"
				}
			}
			continue
		}
		key := parts[1]
		if !wanted[key] {
			continue
		}
		if statuses[key] != "" {
			statuses[key] = "duplicate"
			continue
		}
		value, ok := shellSettingLiteral(parts[2])
		if !ok {
			statuses[key] = "not_literal"
			continue
		}
		statuses[key] = "literal"
		values[key] = value
	}
	return values, statuses
}

func shellSettingLiteral(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	value := ""
	tail := ""
	if raw[0] == '\'' || raw[0] == '"' {
		quote := raw[0]
		end := strings.IndexByte(raw[1:], quote)
		if end < 0 {
			return "", false
		}
		end++
		value = raw[1:end]
		tail = raw[end+1:]
		if tail != "" && tail[0] != ' ' && tail[0] != '\t' {
			return "", false
		}
	} else {
		end := strings.IndexAny(raw, " \t")
		if end < 0 {
			end = len(raw)
		}
		value = raw[:end]
		tail = raw[end:]
	}
	tail = strings.TrimSpace(tail)
	if tail != "" && !strings.HasPrefix(tail, "#") {
		return "", false
	}
	if strings.ContainsAny(value, "$\x60\\;|&()<>{}\n\r") {
		return "", false
	}
	return value, true
}
