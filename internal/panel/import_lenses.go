package panel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"compound/internal/capgraph"
	"gopkg.in/yaml.v3"
)

const LensManifestSchemaVersion = "compound.lens_manifest.v0.1"
const LensSourceAuthoritySchemaVersion = "compound.lens_source_authority.v0.1"

type lensManifest struct {
	SchemaVersion string                     `yaml:"schema_version"`
	Lenses        []lensManifestEntry        `yaml:"lenses"`
	JudgePrompts  []judgePromptManifestEntry `yaml:"judge_prompts"`
}

type lensManifestEntry struct {
	ID       string   `yaml:"id"`
	Name     string   `yaml:"name"`
	Triggers []string `yaml:"triggers"`
	Weight   float64  `yaml:"weight"`
	Prompt   string   `yaml:"prompt"`
}

type judgePromptManifestEntry struct {
	ID     string `yaml:"id"`
	Prompt string `yaml:"prompt"`
}

type ImportResult struct {
	SchemaVersion     string            `json:"schema_version"`
	ManifestPath      string            `json:"manifest_path"`
	ManifestSHA256    string            `json:"manifest_sha256"`
	Imported          int               `json:"imported"`
	NodesChanged      int               `json:"nodes_changed"`
	PromptsChanged    int               `json:"prompts_changed"`
	AuthorityAppended bool              `json:"authority_appended"`
	AuthorityPath     string            `json:"authority_path"`
	AuthorityID       string            `json:"authority_id"`
	NodeIDs           []string          `json:"node_ids"`
	PromptSHA256      map[string]string `json:"prompt_sha256"`
	Changed           bool              `json:"changed"`
}

type LensSourceAuthorityRecord struct {
	SchemaVersion       string            `json:"schema_version"`
	ID                  string            `json:"id"`
	CreatedAt           string            `json:"created_at"`
	SourcePath          string            `json:"source_path"`
	SourceKind          string            `json:"source_kind"`
	SourceOwner         string            `json:"source_owner"`
	SourceClass         string            `json:"source_class"`
	SourceRevision      string            `json:"source_revision"`
	ImportedVia         string            `json:"imported_via"`
	AuthorityRef        string            `json:"authority_ref"`
	AuthorityStatus     string            `json:"authority_status"`
	SourceRole          string            `json:"source_role"`
	Retires             string            `json:"retires"`
	Rollback            string            `json:"rollback"`
	NodeIDs             []string          `json:"node_ids"`
	EdgeIDs             []string          `json:"edge_ids"`
	PromptSHA256        map[string]string `json:"prompt_sha256"`
	SourceObservationID string            `json:"source_observation_id,omitempty"`
	NoPromoteIDs        []string          `json:"no_promote_ids,omitempty"`
	EvidenceRefs        []string          `json:"evidence_refs"`
}

var lensSlugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func ImportLensManifest(homePath, manifestPath string, now time.Time) (ImportResult, error) {
	if strings.TrimSpace(homePath) == "" {
		return ImportResult{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(manifestPath) == "" {
		return ImportResult{}, fmt.Errorf("manifest path required")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return ImportResult{}, err
	}
	manifest, err := decodeLensManifest(data)
	if err != nil {
		return ImportResult{}, err
	}
	manifestSHA := sha256HexString(data)
	result := ImportResult{
		SchemaVersion:  "compound.lens_import.v0.1",
		ManifestPath:   manifestPath,
		ManifestSHA256: manifestSHA,
		Imported:       len(manifest.Lenses),
		AuthorityPath:  lensSourceAuthorityLog(homePath),
		PromptSHA256:   map[string]string{},
	}

	store, err := capgraph.Open(homePath)
	if err != nil {
		return ImportResult{}, err
	}
	existingNodes, err := store.List()
	if err != nil {
		return ImportResult{}, err
	}
	existingByID := map[string]capgraph.Node{}
	for _, node := range existingNodes {
		existingByID[node.ID] = node
	}
	promptDir := filepath.Join(homePath, "registries", "lens-prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		return ImportResult{}, err
	}

	for _, entry := range manifest.Lenses {
		nodeID := "lens:" + entry.ID
		node := capgraph.Node{ID: nodeID, Kind: "lens", Name: strings.TrimSpace(entry.Name), Triggers: cleanStrings(entry.Triggers), Weight: entry.Weight}
		if node.Weight == 0 {
			node.Weight = 1
		}
		if existing, ok := existingByID[nodeID]; !ok || !sameCapgraphNode(existing, node) {
			if err := store.Add(node); err != nil {
				return ImportResult{}, err
			}
			result.NodesChanged++
		}
		promptBody := []byte(entry.Prompt)
		promptPath := filepath.Join(promptDir, nodeID+".md")
		if existing, err := os.ReadFile(promptPath); err != nil {
			if !os.IsNotExist(err) {
				return ImportResult{}, err
			}
			if err := os.WriteFile(promptPath, promptBody, 0o644); err != nil {
				return ImportResult{}, err
			}
			result.PromptsChanged++
		} else if !bytes.Equal(existing, promptBody) {
			if err := os.WriteFile(promptPath, promptBody, 0o644); err != nil {
				return ImportResult{}, err
			}
			result.PromptsChanged++
		}
		result.NodeIDs = append(result.NodeIDs, nodeID)
		result.PromptSHA256[nodeID] = sha256HexString(promptBody)
	}
	for _, entry := range manifest.JudgePrompts {
		promptID := "judge:" + entry.ID
		promptBody := []byte(entry.Prompt)
		promptPath := filepath.Join(promptDir, promptID+".md")
		existing, err := os.ReadFile(promptPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return ImportResult{}, err
			}
			if err := os.WriteFile(promptPath, promptBody, 0o644); err != nil {
				return ImportResult{}, err
			}
			result.PromptsChanged++
		} else if !bytes.Equal(existing, promptBody) {
			if err := os.WriteFile(promptPath, promptBody, 0o644); err != nil {
				return ImportResult{}, err
			}
			result.PromptsChanged++
		}
		result.PromptSHA256[promptID] = sha256HexString(promptBody)
	}

	sort.Strings(result.NodeIDs)

	rec := LensSourceAuthorityRecord{
		SchemaVersion:   LensSourceAuthoritySchemaVersion,
		ID:              lensSourceAuthorityID(manifestPath, manifestSHA, "compound panel import-lenses"),
		CreatedAt:       now.UTC().Format(time.RFC3339),
		SourcePath:      manifestPath,
		SourceKind:      "lens_manifest",
		SourceOwner:     "compound",
		SourceClass:     "registry_seed",
		SourceRevision:  manifestSHA,
		ImportedVia:     "compound panel import-lenses",
		AuthorityRef:    "compound://registries/capability-graph",
		AuthorityStatus: "current",
		SourceRole:      "lens_registry_seed",
		Retires:         "",
		Rollback:        "re-import a prior lens manifest revision",
		NodeIDs:         append([]string(nil), result.NodeIDs...),
		EdgeIDs:         []string{},
		PromptSHA256:    result.PromptSHA256,
		EvidenceRefs:    []string{"sha256:" + manifestSHA},
	}
	result.AuthorityID = rec.ID
	exists, err := lensSourceAuthorityExists(homePath, rec)
	if err != nil {
		return ImportResult{}, err
	}
	if !exists {
		if err := appendLensSourceAuthority(homePath, rec); err != nil {
			return ImportResult{}, err
		}
		result.AuthorityAppended = true
	}
	result.Changed = result.NodesChanged > 0 || result.PromptsChanged > 0 || result.AuthorityAppended
	return result, nil
}

func decodeLensManifest(data []byte) (lensManifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var manifest lensManifest
	if err := dec.Decode(&manifest); err != nil {
		return lensManifest{}, fmt.Errorf("decode lens manifest: %w", err)
	}
	if manifest.SchemaVersion != LensManifestSchemaVersion {
		return lensManifest{}, fmt.Errorf("schema_version must be %q", LensManifestSchemaVersion)
	}
	if len(manifest.Lenses) == 0 {
		return lensManifest{}, fmt.Errorf("lens manifest requires at least one lens")
	}
	seen := map[string]bool{}
	for i, lens := range manifest.Lenses {
		if !lensSlugRE.MatchString(lens.ID) {
			return lensManifest{}, fmt.Errorf("lenses[%d].id must be slug, got %q", i, lens.ID)
		}
		if seen[lens.ID] {
			return lensManifest{}, fmt.Errorf("duplicate lens id %q", lens.ID)
		}
		seen[lens.ID] = true
		if strings.TrimSpace(lens.Name) == "" {
			return lensManifest{}, fmt.Errorf("lenses[%d].name required", i)
		}
		if len(cleanStrings(lens.Triggers)) == 0 {
			return lensManifest{}, fmt.Errorf("lenses[%d].triggers required", i)
		}
		if strings.TrimSpace(lens.Prompt) == "" {
			return lensManifest{}, fmt.Errorf("lenses[%d].prompt required", i)
		}
		if lens.Weight < 0 {
			return lensManifest{}, fmt.Errorf("lenses[%d].weight must be >= 0", i)
		}
	}
	seenJudgePrompts := map[string]bool{}
	for i, prompt := range manifest.JudgePrompts {
		if !lensSlugRE.MatchString(prompt.ID) {
			return lensManifest{}, fmt.Errorf("judge_prompts[%d].id must be slug, got %q", i, prompt.ID)
		}
		if seenJudgePrompts[prompt.ID] {
			return lensManifest{}, fmt.Errorf("duplicate judge prompt id %q", prompt.ID)
		}
		seenJudgePrompts[prompt.ID] = true
		if strings.TrimSpace(prompt.Prompt) == "" {
			return lensManifest{}, fmt.Errorf("judge_prompts[%d].prompt required", i)
		}
	}
	return manifest, nil
}

// sameCapgraphNode is a hand-maintained field comparison, so every field added
// to capgraph.Node must be added here too -- a field omitted is not a compile
// error, it is a change this function reports as "no change", which means the
// update is silently dropped. Health is compared for exactly that reason: a
// server recovering from offline is a health transition and nothing else, so
// omitting it here would make recovery invisible.
func sameCapgraphNode(a, b capgraph.Node) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.Name == b.Name && a.Weight == b.Weight && a.Retired == b.Retired && a.Health == b.Health && a.EvidenceOnly == b.EvidenceOnly && equalStrings(a.Triggers, b.Triggers)
}

func cleanStrings(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func lensSourceAuthorityExists(homePath string, want LensSourceAuthorityRecord) (bool, error) {
	recs, err := loadLensSourceAuthority(homePath)
	if err != nil {
		return false, err
	}
	for _, rec := range recs {
		if rec.ID == want.ID && rec.SourceRevision == want.SourceRevision && equalStrings(rec.NodeIDs, want.NodeIDs) {
			return true, nil
		}
	}
	return false, nil
}

func loadLensSourceAuthority(homePath string) ([]LensSourceAuthorityRecord, error) {
	data, err := os.ReadFile(lensSourceAuthorityLog(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []LensSourceAuthorityRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec LensSourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func appendLensSourceAuthority(homePath string, rec LensSourceAuthorityRecord) error {
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(lensSourceAuthorityLog(homePath), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

func lensSourceAuthorityLog(homePath string) string {
	return filepath.Join(homePath, "registries", "lens-source-authority.jsonl")
}

func lensSourceAuthorityID(path, revision, via string) string {
	return "lenssrc_" + sha256HexString([]byte(strings.Join([]string{path, revision, via}, "|")))[:12]
}

func sha256HexString(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
