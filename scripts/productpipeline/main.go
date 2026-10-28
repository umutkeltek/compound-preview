// Command productpipeline emits a portable public bundle as the native SkillManifest.
// It reads local files only. Native capability import owns all governance and writes.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/capgraph"
)

func main() {
	root := flag.String("root", "skills/product-pipeline", "public skill bundle directory")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	m, err := generate(*root, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(root string, now time.Time) (capgraph.SkillManifest, error) {
	var m capgraph.SkillManifest
	root, err := filepath.Abs(root)
	if err != nil {
		return m, err
	}
	revision, err := bundleRevision(root)
	if err != nil {
		return m, err
	}
	b, err := os.ReadFile(filepath.Join(root, "routing.template.json"))
	if err != nil {
		return m, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("template: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return m, fmt.Errorf("template must contain exactly one JSON object")
	}
	m.ObservedAt = now.UTC().Format(time.RFC3339)
	m.SourceRef.Revision = revision
	m.SourceRef.Path, err = localPath(root, m.SourceRef.Path, false)
	if err != nil {
		return m, fmt.Errorf("source: %w", err)
	}
	for i, ref := range m.EvidenceRefs {
		m.EvidenceRefs[i], err = localPath(root, ref, false)
		if err != nil {
			return m, fmt.Errorf("evidence: %w", err)
		}
	}
	ids := make(map[string]bool, len(m.Skills))
	for i := range m.Skills {
		s := &m.Skills[i]
		ids[s.ID] = true
		if s.SkillPath != "" {
			s.SkillPath, err = localPath(root, s.SkillPath, true)
			if err != nil {
				return m, fmt.Errorf("%s skill path: %w", s.ID, err)
			}
			rel, err := filepath.Rel(root, s.SkillPath)
			if err != nil {
				return m, err
			}
			if _, err := localPath(root, filepath.Join(rel, "SKILL.md"), false); err != nil {
				return m, fmt.Errorf("%s skill body: %w", s.ID, err)
			}
		}
		for j, read := range s.Read {
			s.Read[j], err = localPath(root, read, false)
			if err != nil {
				return m, fmt.Errorf("%s read: %w", s.ID, err)
			}
		}
	}
	for _, s := range m.Skills {
		for _, route := range s.Route {
			if !ids[route.Destination] {
				return m, fmt.Errorf("%s routes to unbundled context %s", s.ID, route.Destination)
			}
		}
		// This public core must not silently bind unavailable external bodies.
		for _, refs := range [][]string{s.Requires, s.Specializes, s.ConflictsWith, s.ValidatedBy, s.ReplacementFor} {
			for _, ref := range refs {
				if !ids[ref] {
					return m, fmt.Errorf("%s references unbundled node %s", s.ID, ref)
				}
			}
		}
	}
	if r := capgraph.ValidateSkillManifest(m); r.Status != "ok" {
		return m, fmt.Errorf("native manifest validation failed: %+v", r.Checks)
	}
	return m, nil
}

// bundleRevision is independent of location and clock. It also rejects symlinked
// content rather than importing a body from an uninspected external directory.
func bundleRevision(root string) (string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinked bundle entry: %s", p)
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular bundle entry: %s", p)
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, p := range files {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%d:%s:%d:", len(filepath.ToSlash(rel)), filepath.ToSlash(rel), len(b))
		h.Write(b)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func localPath(root, rel string, directory bool) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("expected local relative path: %q", rel)
	}
	p := filepath.Join(root, rel)
	check, err := filepath.Rel(root, p)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes bundle: %q", rel)
	}
	info, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("symlinked target: %q", rel)
	}
	if directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return "", fmt.Errorf("wrong target type: %q", rel)
	}
	return p, nil
}
