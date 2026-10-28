package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const PrivateAuditExportSchemaVersion = "compound.release_private_audit_export.v0.1"

type PrivateAuditExportOptions struct {
	RepoPath   string
	TargetPath string
	Prefix     string
}

type PrivateAuditExportReport struct {
	SchemaVersion   string   `json:"schema_version"`
	Status          string   `json:"status"`
	GeneratedAt     string   `json:"generated_at"`
	Repo            string   `json:"repo"`
	Commit          string   `json:"commit,omitempty"`
	TargetPath      string   `json:"target_path"`
	Prefix          string   `json:"prefix"`
	Files           int      `json:"files"`
	Bytes           int64    `json:"bytes"`
	PrivateSurfaces []string `json:"private_surfaces,omitempty"`
	ExcludedPaths   []string `json:"excluded_paths,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
	ProofRefs       []string `json:"proof_refs"`
}

type privateAuditManifest struct {
	SchemaVersion   string   `json:"schema_version"`
	GeneratedAt     string   `json:"generated_at"`
	Repo            string   `json:"repo"`
	Commit          string   `json:"commit,omitempty"`
	Prefix          string   `json:"prefix"`
	PrivateSurfaces []string `json:"private_surfaces,omitempty"`
	ExcludedPaths   []string `json:"excluded_paths,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
	ProofRefs       []string `json:"proof_refs"`
}

func ExportPrivateAudit(ctx context.Context, opts PrivateAuditExportOptions, now time.Time) (PrivateAuditExportReport, error) {
	repo, err := gitTopLevel(ctx, opts.RepoPath)
	if err != nil {
		return PrivateAuditExportReport{}, err
	}
	commit, err := gitTrimmed(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		commit = ""
	}
	target, err := normalizeTarget(opts.TargetPath)
	if err != nil {
		return PrivateAuditExportReport{}, err
	}
	prefix := normalizePrivatePrefix(opts.Prefix)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return PrivateAuditExportReport{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".compound-private-audit-*.tar.gz")
	if err != nil {
		return PrivateAuditExportReport{}, err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	state := &privateAuditState{
		repo:          repo,
		targetPath:    target,
		prefix:        prefix,
		privateSeen:   map[string]bool{},
		excludedSeen:  map[string]bool{},
		warningsSeen:  map[string]bool{},
		redactNeedles: privateAuditRedactionNeedles(repo, opts.RepoPath),
	}
	if err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			state.warn(filepath.ToSlash(strings.TrimPrefix(path, repo+string(filepath.Separator))) + ": " + walkErr.Error())
			return nil
		}
		if path == repo {
			return nil
		}
		rel, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if privateAuditExcluded(rel, d) {
			state.exclude(rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		state.notePrivate(rel)
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			state.exclude(rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			state.warn(rel + ": " + err.Error())
			return nil
		}
		if !info.Mode().IsRegular() {
			state.exclude(rel)
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			state.warn(rel + ": " + err.Error())
			return nil
		}
		body = state.redact(rel, body)
		hdr := &tar.Header{
			Name:    filepath.ToSlash(filepath.Join(prefix, rel)),
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
		state.files++
		state.bytes += int64(len(body))
		return nil
	}); err != nil {
		return PrivateAuditExportReport{}, err
	}
	report := state.report(now, commit)
	manifest := privateAuditManifest{
		SchemaVersion:   report.SchemaVersion,
		GeneratedAt:     report.GeneratedAt,
		Repo:            "${REPO}",
		Commit:          report.Commit,
		Prefix:          report.Prefix,
		PrivateSurfaces: report.PrivateSurfaces,
		ExcludedPaths:   report.ExcludedPaths,
		Warnings:        report.Warnings,
		ProofRefs:       report.ProofRefs,
	}
	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return PrivateAuditExportReport{}, err
	}
	manifestBody = append(manifestBody, '\n')
	if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(filepath.Join(prefix, "PRIVATE_AUDIT_MANIFEST.json")), Mode: 0o644, Size: int64(len(manifestBody)), ModTime: now}); err != nil {
		return PrivateAuditExportReport{}, err
	}
	if _, err := tw.Write(manifestBody); err != nil {
		return PrivateAuditExportReport{}, err
	}
	report.Files++
	report.Bytes += int64(len(manifestBody))
	if err := tw.Close(); err != nil {
		return PrivateAuditExportReport{}, err
	}
	if err := gz.Close(); err != nil {
		return PrivateAuditExportReport{}, err
	}
	if err := tmp.Close(); err != nil {
		return PrivateAuditExportReport{}, err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return PrivateAuditExportReport{}, err
	}
	report.TargetPath = target
	ok = true
	return report, nil
}

type privateAuditState struct {
	repo          string
	targetPath    string
	prefix        string
	files         int
	bytes         int64
	privateSeen   map[string]bool
	excludedSeen  map[string]bool
	warningsSeen  map[string]bool
	redactNeedles []privateAuditRedaction
}

type privateAuditRedaction struct {
	Value string
	Label string
}

func (s *privateAuditState) report(now time.Time, commit string) PrivateAuditExportReport {
	return PrivateAuditExportReport{
		SchemaVersion:   PrivateAuditExportSchemaVersion,
		Status:          "ok",
		GeneratedAt:     now.UTC().Format(time.RFC3339),
		Repo:            "${REPO}",
		Commit:          commit,
		TargetPath:      s.targetPath,
		Prefix:          s.prefix,
		Files:           s.files,
		Bytes:           s.bytes,
		PrivateSurfaces: sortedKeys(s.privateSeen),
		ExcludedPaths:   sortedKeys(s.excludedSeen),
		Warnings:        sortedKeys(s.warningsSeen),
		ProofRefs:       []string{"private-audit-explicit-command", "private-audit-heavy-path-exclusions", "private-audit-local-path-redaction"},
	}
}

func (s *privateAuditState) exclude(path string) { s.excludedSeen[path] = true }
func (s *privateAuditState) warn(text string)    { s.warningsSeen[text] = true }

func (s *privateAuditState) notePrivate(rel string) {
	if rel == "" {
		return
	}
	if strings.HasSuffix(rel, ".private.md") {
		s.privateSeen["*.private.md"] = true
		return
	}
	first := strings.Split(rel, "/")[0]
	if first == "docs_private" || first == ".claude" || first == "Plans" || first == "memory" || first == "home" || first == ".compound-private" {
		s.privateSeen[first] = true
	}
}

func (s *privateAuditState) redact(rel string, body []byte) []byte {
	if bytes.IndexByte(body, 0) >= 0 {
		s.warn(rel + ": binary content not redacted")
		return body
	}
	out := string(body)
	for _, needle := range s.redactNeedles {
		if needle.Value == "" {
			continue
		}
		out = strings.ReplaceAll(out, needle.Value, needle.Label)
	}
	return []byte(out)
}

func privateAuditRedactionNeedles(repo string, repoAliases ...string) []privateAuditRedaction {
	var needles []privateAuditRedaction
	add := func(value, label string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		needles = append(needles, privateAuditRedaction{Value: value, Label: label})
		slashed := filepath.ToSlash(value)
		if slashed != value {
			needles = append(needles, privateAuditRedaction{Value: slashed, Label: label})
		}
		if abs, err := filepath.Abs(value); err == nil && abs != value {
			needles = append(needles, privateAuditRedaction{Value: abs, Label: label})
			slashedAbs := filepath.ToSlash(abs)
			if slashedAbs != abs {
				needles = append(needles, privateAuditRedaction{Value: slashedAbs, Label: label})
			}
		}
		if resolved, err := filepath.EvalSymlinks(value); err == nil && resolved != value {
			needles = append(needles, privateAuditRedaction{Value: resolved, Label: label})
			slashedResolved := filepath.ToSlash(resolved)
			if slashedResolved != resolved {
				needles = append(needles, privateAuditRedaction{Value: slashedResolved, Label: label})
			}
		}
	}
	add(repo, "${REPO}")
	for _, alias := range repoAliases {
		add(alias, "${REPO}")
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home, "${HOME}")
	}
	return compactRedactions(needles)
}

func privateAuditExcluded(rel string, d fs.DirEntry) bool {
	base := filepath.Base(rel)
	parts := strings.Split(rel, "/")
	if base == ".DS_Store" || base == "__MACOSX" {
		return true
	}
	if len(parts) == 1 && parts[0] == "compound" {
		return true
	}
	for _, part := range parts {
		switch part {
		case ".git", "node_modules", ".next", "__MACOSX":
			return true
		}
	}
	if len(parts) > 0 && parts[0] == "bin" {
		return true
	}
	return false
}

func normalizePrivatePrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = "compound-private-audit"
	}
	return prefix + "/"
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func compactRedactions(values []privateAuditRedaction) []privateAuditRedaction {
	seen := map[string]bool{}
	out := []privateAuditRedaction{}
	for _, value := range values {
		value.Value = strings.TrimSpace(value.Value)
		if value.Value == "" || seen[value.Value] {
			continue
		}
		seen[value.Value] = true
		out = append(out, value)
	}
	return out
}
