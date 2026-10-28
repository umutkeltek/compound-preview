package capgraph

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	brainRecipeTreeSourceKind  = "brain_recipe_tree"
	brainRecipeTreeSourceOwner = "solo-builder-os"
	brainRecipeTreeSourceClass = "solo-builder-recipes"
)

type brainRecipeTreeFile struct {
	rel      string
	path     string
	data     []byte
	fileHash string
}

// LoadBrainRecipeTree adapts a curated markdown recipe tree into procedure nodes.
// It only uses explicit recipe header markers for triggers and boundaries; body
// text remains source drilldown, never surfacing soup.
func LoadBrainRecipeTree(dir string, now time.Time) (SkillManifest, error) {
	root, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return SkillManifest{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return SkillManifest{}, err
	}
	if !info.IsDir() {
		return SkillManifest{}, fmt.Errorf("brain recipe tree %s is not a directory", root)
	}
	if now.IsZero() {
		now = time.Now()
	}

	var files []brainRecipeTreeFile
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") && path != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(name, ".bak") || filepath.Ext(name) != ".md" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files = append(files, brainRecipeTreeFile{
			rel:      filepath.ToSlash(rel),
			path:     path,
			data:     data,
			fileHash: hex.EncodeToString(sum[:]),
		})
		return nil
	})
	if err != nil {
		return SkillManifest{}, err
	}
	if len(files) == 0 {
		return SkillManifest{}, fmt.Errorf("brain recipe tree %s has no markdown recipes", root)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	revision := brainRecipeTreeRevision(files)
	evidenceRef := "brain_recipe_tree:" + revision[:19]
	manifest := SkillManifest{
		SchemaVersion: SkillManifestSchemaVersion,
		ID:            "skill_manifest_brain_recipe_tree_" + revision[7:19],
		AdapterID:     "brain.recipe-tree",
		SourceSystem:  "brain",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: SkillSourceRef{
			Path:        root,
			Kind:        brainRecipeTreeSourceKind,
			Owner:       brainRecipeTreeSourceOwner,
			SourceClass: brainRecipeTreeSourceClass,
			Revision:    revision,
		},
		EvidenceRefs: []string{evidenceRef},
	}

	for _, file := range files {
		entry, err := brainRecipeTreeEntry(file, evidenceRef)
		if err != nil {
			return SkillManifest{}, err
		}
		manifest.Skills = append(manifest.Skills, entry)
	}
	sort.SliceStable(manifest.Skills, func(i, j int) bool {
		return manifest.Skills[i].ID < manifest.Skills[j].ID
	})
	return manifest, nil
}

func brainRecipeTreeRevision(files []brainRecipeTreeFile) string {
	parts := make([]string, 0, len(files))
	for _, file := range files {
		parts = append(parts, file.rel+"\n"+file.fileHash)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func brainRecipeTreeEntry(file brainRecipeTreeFile, evidenceRef string) (SkillManifestEntry, error) {
	name, triggerText, boundaries, err := parseBrainRecipeMarkdown(file.data)
	if err != nil {
		return SkillManifestEntry{}, fmt.Errorf("parse brain recipe %s: %w", file.rel, err)
	}
	relNoExt := strings.TrimSuffix(file.rel, ".md")
	if strings.TrimSpace(name) == "" {
		name = brainRecipeFallbackName(relNoExt)
	}
	triggers := skillTokenList(triggerText)
	if len(triggers) == 0 {
		triggers = skillTokenList(name)
	}
	return SkillManifestEntry{
		ID:           "procedure:" + slugSkill(strings.ReplaceAll(relNoExt, "/", "-")),
		Kind:         "procedure",
		Name:         name,
		Status:       "active",
		Triggers:     triggers,
		EvidenceRefs: []string{evidenceRef},
		Read:         []string{file.path},
		Boundary:     boundaries,
	}, nil
}

func parseBrainRecipeMarkdown(data []byte) (string, string, []string, error) {
	var name string
	var triggerText string
	var boundaries []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if name == "" && strings.HasPrefix(line, "# ") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
		if text, ok := brainRecipeMarkerText(line, "DO NOT TRIGGER:"); ok {
			if text != "" {
				boundaries = append(boundaries, text)
			}
			continue
		}
		if triggerText == "" {
			if text, ok := brainRecipeMarkerText(line, "TRIGGER:"); ok {
				triggerText = text
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", nil, err
	}
	return name, triggerText, boundaries, nil
}

func brainRecipeMarkerText(line, marker string) (string, bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, ">") {
		line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
	}
	if len(line) < len(marker) || !strings.EqualFold(line[:len(marker)], marker) {
		return "", false
	}
	return strings.TrimSpace(line[len(marker):]), true
}

func brainRecipeFallbackName(relNoExt string) string {
	base := filepath.Base(filepath.FromSlash(relNoExt))
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, "_", " ")
	return strings.TrimSpace(base)
}
