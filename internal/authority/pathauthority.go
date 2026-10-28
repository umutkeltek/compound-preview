package authority

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/candidates"
	"compound/internal/mem"

	"gopkg.in/yaml.v3"
)

const PathAuthoritySchemaVersion = "compound.path_authority.v0.1"

type PathAuthority struct {
	SchemaVersion       string          `json:"schema_version" yaml:"schema_version"`
	EntityRef           string          `json:"entity_ref" yaml:"entity_ref"`
	EntityKind          string          `json:"entity_kind" yaml:"entity_kind"`
	Aliases             []string        `json:"aliases,omitempty" yaml:"aliases,omitempty"`
	AuthoritativeSource string          `json:"authoritative_source" yaml:"authoritative_source"`
	PrecedenceRank      int             `json:"precedence_rank,omitempty" yaml:"precedence_rank,omitempty"`
	SourceBindings      []SourceBinding `json:"source_bindings,omitempty" yaml:"source_bindings,omitempty"`
	PathBindings        []PathBinding   `json:"path_bindings" yaml:"path_bindings"`
	HostBindings        []HostBinding   `json:"host_bindings,omitempty" yaml:"host_bindings,omitempty"`
	EvidenceRefs        []string        `json:"evidence_refs" yaml:"evidence_refs"`
}

type SourceBinding struct {
	Kind  string `json:"kind" yaml:"kind"`
	Value string `json:"value" yaml:"value"`
}

type PathBinding struct {
	Path   string `json:"path" yaml:"path"`
	Host   string `json:"host,omitempty" yaml:"host,omitempty"`
	Status string `json:"status" yaml:"status"` // active | stale | retired
}

type HostBinding struct {
	Host   string `json:"host" yaml:"host"`
	Status string `json:"status" yaml:"status"` // active | stale | retired
}

type PathAuthorityReport struct {
	EntityRef      string   `json:"entity_ref"`
	Status         string   `json:"status"`
	Identifiers    []string `json:"identifiers"`
	CandidateCount int      `json:"candidate_count"`
}

func ImportPathAuthority(path string, existing []mem.Entity) (PathAuthority, mem.Entity, []mem.Candidate, PathAuthorityReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PathAuthority{}, mem.Entity{}, nil, PathAuthorityReport{}, err
	}
	var pa PathAuthority
	if err := yaml.Unmarshal(data, &pa); err != nil {
		return PathAuthority{}, mem.Entity{}, nil, PathAuthorityReport{}, err
	}
	if err := canonicalizeActivePathBindings(&pa); err != nil {
		return pa, mem.Entity{}, nil, PathAuthorityReport{EntityRef: pa.EntityRef, Status: "fail"}, err
	}
	if err := validatePathAuthority(pa); err != nil {
		return pa, mem.Entity{}, nil, PathAuthorityReport{EntityRef: pa.EntityRef, Status: "fail"}, err
	}
	entity, facts, report := buildPathAuthorityImport(pa, "path-authority:"+filepath.Base(path), existing)
	return pa, entity, facts, report, nil
}

type OSPathRegistryReport struct {
	Status         string                `json:"status"`
	EntityCount    int                   `json:"entity_count"`
	CandidateCount int                   `json:"candidate_count"`
	Reports        []PathAuthorityReport `json:"reports"`
}

type osPathRegistryFile struct {
	Version            int                   `yaml:"version"`
	CanonicalRoots     map[string]osPathRoot `yaml:"canonical_roots"`
	StaleOrCompatRoots map[string]osPathRoot `yaml:"stale_or_compat_roots"`
}

type osPathRoot struct {
	Path        string   `yaml:"path"`
	Status      string   `yaml:"status"`
	Owns        []string `yaml:"owns"`
	ArchivedTo  string   `yaml:"archived_to"`
	MovedTo     string   `yaml:"moved_to"`
	AllowedUse  string   `yaml:"allowed_use"`
	WritePolicy string   `yaml:"write_policy"`
}

func ImportOSPathRegistry(path string, existing []mem.Entity) ([]PathAuthority, []mem.Entity, []mem.Candidate, OSPathRegistryReport, error) {
	return importOSPathRegistryWithAccountHome(path, existing, currentAccountHome)
}

func importOSPathRegistryWithAccountHome(
	path string,
	existing []mem.Entity,
	resolveAccountHome func() (string, error),
) ([]PathAuthority, []mem.Entity, []mem.Candidate, OSPathRegistryReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, OSPathRegistryReport{}, err
	}
	var rf osPathRegistryFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, nil, nil, OSPathRegistryReport{}, err
	}
	if rf.Version == 0 {
		return nil, nil, nil, OSPathRegistryReport{Status: "fail"}, fmt.Errorf("version required")
	}
	if len(rf.CanonicalRoots) == 0 && len(rf.StaleOrCompatRoots) == 0 {
		return nil, nil, nil, OSPathRegistryReport{Status: "fail"}, fmt.Errorf("canonical_roots or stale_or_compat_roots required")
	}

	allExisting := append([]mem.Entity{}, existing...)
	var out []PathAuthority
	var entities []mem.Entity
	var facts []mem.Candidate
	var reports []PathAuthorityReport
	for _, key := range sortedKeys(rf.CanonicalRoots) {
		pa, err := pathAuthorityFromOSRoot(key, rf.CanonicalRoots[key], true, filepath.Base(path), resolveAccountHome)
		if err != nil {
			return out, entities, facts, OSPathRegistryReport{Status: "fail", Reports: reports}, err
		}
		entity, cs, report := buildPathAuthorityImport(pa, "os-path-authority:"+filepath.Base(path)+":"+key, allExisting)
		out = append(out, pa)
		entities = append(entities, entity)
		facts = append(facts, cs...)
		reports = append(reports, report)
		allExisting = mergeEntityForImport(allExisting, entity)
	}
	for _, key := range sortedKeys(rf.StaleOrCompatRoots) {
		pa, err := pathAuthorityFromOSRoot(key, rf.StaleOrCompatRoots[key], false, filepath.Base(path), resolveAccountHome)
		if err != nil {
			return out, entities, facts, OSPathRegistryReport{Status: "fail", Reports: reports}, err
		}
		entity, cs, report := buildPathAuthorityImport(pa, "os-path-authority:"+filepath.Base(path)+":"+key, allExisting)
		out = append(out, pa)
		entities = append(entities, entity)
		facts = append(facts, cs...)
		reports = append(reports, report)
		allExisting = mergeEntityForImport(allExisting, entity)
	}
	return out, entities, facts, OSPathRegistryReport{
		Status:         "ok",
		EntityCount:    len(entities),
		CandidateCount: len(facts),
		Reports:        reports,
	}, nil
}

func buildPathAuthorityImport(pa PathAuthority, src string, existing []mem.Entity) (mem.Entity, []mem.Candidate, PathAuthorityReport) {
	entity := mergePathAuthorityEntity(pa, existing)
	var facts []mem.Candidate
	add := func(pred, val string) {
		val = strings.TrimSpace(val)
		if val == "" {
			return
		}
		facts = append(facts, mem.Candidate{
			Entity:         pa.EntityRef,
			Predicate:      pred,
			Object:         val,
			Confidence:     1.0,
			Derivation:     "deterministic",
			ClaimType:      "status",
			Qualifiers:     map[string]string{"domain": pred},
			PromotionBasis: candidates.BasisTrustedDeterministic,
			SourceTraceRef: src,
			SnippetHash:    src + ":" + pa.EntityRef + ":" + pred + ":" + val,
			Visibility:     "operational",
		})
	}
	add("authoritative_source", pa.AuthoritativeSource)
	for _, sb := range pa.SourceBindings {
		add("source_"+sb.Kind, sb.Value)
	}
	for _, pb := range pa.PathBindings {
		if pb.Status == "active" {
			add("current_path", pb.Path)
		} else {
			add("known_path_"+pb.Status, pb.Path)
		}
	}
	for _, hb := range pa.HostBindings {
		if hb.Status == "active" {
			add("current_host", hb.Host)
		} else {
			add("known_host_"+hb.Status, hb.Host)
		}
	}

	report := PathAuthorityReport{
		EntityRef:      pa.EntityRef,
		Status:         "ok",
		Identifiers:    entity.Identifiers,
		CandidateCount: len(facts),
	}
	return entity, facts, report
}

func validatePathAuthority(pa PathAuthority) error {
	if pa.SchemaVersion != PathAuthoritySchemaVersion {
		return fmt.Errorf("schema_version must be %s, got %q", PathAuthoritySchemaVersion, pa.SchemaVersion)
	}
	if strings.TrimSpace(pa.EntityRef) == "" {
		return fmt.Errorf("entity_ref required")
	}
	if strings.TrimSpace(pa.EntityKind) == "" {
		return fmt.Errorf("entity_kind required")
	}
	if strings.TrimSpace(pa.AuthoritativeSource) == "" {
		return fmt.Errorf("authoritative_source required")
	}
	if len(pa.PathBindings) == 0 && len(pa.SourceBindings) == 0 {
		return fmt.Errorf("at least one path_binding or source_binding required")
	}
	if len(pa.EvidenceRefs) == 0 {
		return fmt.Errorf("evidence_refs required")
	}
	activePaths := 0
	for _, pb := range pa.PathBindings {
		if strings.TrimSpace(pb.Path) == "" {
			return fmt.Errorf("path_binding path required")
		}
		if !oneOf(pb.Status, []string{"active", "stale", "retired"}) {
			return fmt.Errorf("path_binding %q bad status %q", pb.Path, pb.Status)
		}
		if pb.Status == "active" && !filepath.IsAbs(strings.TrimSpace(pb.Path)) {
			return fmt.Errorf("active path_binding must be absolute, got %q", pb.Path)
		}
		if pb.Status == "active" {
			activePaths++
		}
	}
	if activePaths > 1 {
		return fmt.Errorf("path authority %s has %d active path_bindings; at most one is allowed", pa.EntityRef, activePaths)
	}
	for _, sb := range pa.SourceBindings {
		if strings.TrimSpace(sb.Kind) == "" || strings.TrimSpace(sb.Value) == "" {
			return fmt.Errorf("source_binding kind and value required")
		}
	}
	return nil
}

func canonicalizeActivePathBindings(pa *PathAuthority) error {
	for i := range pa.PathBindings {
		binding := &pa.PathBindings[i]
		path := filepath.Clean(strings.TrimSpace(binding.Path))
		if binding.Status != "active" {
			binding.Path = path
			continue
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("active path_binding must be absolute, got %q", binding.Path)
		}
		canonicalPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("active path_binding %q must resolve to an existing canonical path: %w", path, err)
		}
		canonicalPath = filepath.Clean(canonicalPath)
		if !filepath.IsAbs(canonicalPath) {
			return fmt.Errorf("active path_binding canonical path must be absolute, got %q", canonicalPath)
		}
		binding.Path = canonicalPath
	}
	return nil
}

func mergePathAuthorityEntity(pa PathAuthority, existing []mem.Entity) mem.Entity {
	var ids []string
	for _, sb := range pa.SourceBindings {
		ids = append(ids, sb.Value)
		ids = append(ids, sb.Kind+":"+sb.Value)
	}
	for _, pb := range pa.PathBindings {
		ids = append(ids, pb.Path)
	}
	for _, hb := range pa.HostBindings {
		ids = append(ids, "host:"+hb.Host)
	}
	return mergeEntity(existing, pa.EntityRef, pa.EntityKind, pa.Aliases, ids)
}

func pathAuthorityFromOSRoot(
	key string,
	root osPathRoot,
	canonical bool,
	sourceName string,
	resolveAccountHome func() (string, error),
) (PathAuthority, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return PathAuthority{}, fmt.Errorf("os path root key required")
	}
	if strings.TrimSpace(root.Path) == "" {
		return PathAuthority{}, fmt.Errorf("os path root %q path required", key)
	}
	status := normalizeOSPathStatus(root.Status, canonical)
	if status == "" {
		return PathAuthority{}, fmt.Errorf("os path root %q bad status %q", key, root.Status)
	}
	resolvedPath, err := resolveImportedOSRootPath(key, root.Path, status, resolveAccountHome)
	if err != nil {
		return PathAuthority{}, err
	}
	aliases := []string{key, strings.ReplaceAll(key, "_", "-")}
	pa := PathAuthority{
		SchemaVersion:       PathAuthoritySchemaVersion,
		EntityRef:           "source:" + strings.ReplaceAll(key, "_", "-"),
		EntityKind:          "repo",
		Aliases:             aliases,
		AuthoritativeSource: "os-path-authority:" + sourceName,
		SourceBindings: []SourceBinding{
			{Kind: "registry_key", Value: key},
		},
		PathBindings: []PathBinding{{
			Path:   resolvedPath,
			Status: status,
		}},
		EvidenceRefs: []string{"os-path-authority:" + sourceName},
	}
	if root.ArchivedTo != "" {
		pa.SourceBindings = append(pa.SourceBindings, SourceBinding{Kind: "archived_to", Value: root.ArchivedTo})
	}
	if root.MovedTo != "" {
		pa.SourceBindings = append(pa.SourceBindings, SourceBinding{Kind: "moved_to", Value: root.MovedTo})
	}
	if root.AllowedUse != "" {
		pa.SourceBindings = append(pa.SourceBindings, SourceBinding{Kind: "allowed_use", Value: root.AllowedUse})
	}
	if root.WritePolicy != "" {
		pa.SourceBindings = append(pa.SourceBindings, SourceBinding{Kind: "write_policy", Value: root.WritePolicy})
	}
	if err := validatePathAuthority(pa); err != nil {
		return PathAuthority{}, err
	}
	return pa, nil
}

func normalizeOSPathStatus(status string, canonical bool) string {
	switch strings.TrimSpace(status) {
	case "", "active":
		if canonical {
			return "active"
		}
		return ""
	case "active_index":
		return "active"
	case "missing_stale":
		return "stale"
	case "retired_archived", "retired_merged", "retired":
		return "retired"
	default:
		return ""
	}
}

func currentAccountHome() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current account: %w", err)
	}
	home := strings.TrimSpace(account.HomeDir)
	if home == "" {
		return "", fmt.Errorf("current account home directory is empty")
	}
	return home, nil
}

func resolveImportedOSRootPath(
	key string,
	path string,
	status string,
	resolveAccountHome func() (string, error),
) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		if resolveAccountHome == nil {
			return "", fmt.Errorf("os path root %q account-home resolver required", key)
		}
		accountHome, err := resolveAccountHome()
		if err != nil {
			return "", fmt.Errorf("os path root %q resolve account home: %w", key, err)
		}
		accountHome = strings.TrimSpace(accountHome)
		if accountHome == "" {
			return "", fmt.Errorf("os path root %q resolve account home: empty path", key)
		}
		if path == "~" {
			path = accountHome
		} else {
			path = filepath.Join(accountHome, strings.TrimPrefix(path, "~/"))
		}
	}
	path = filepath.Clean(path)
	if status == "active" && !filepath.IsAbs(path) {
		return "", fmt.Errorf("os path root %q active path must be absolute, got %q", key, path)
	}
	if !filepath.IsAbs(path) {
		return path, nil
	}

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if status == "active" && !info.IsDir() {
			return "", fmt.Errorf("os path root %q must resolve to an existing directory: %s", key, path)
		}
		canonicalPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("os path root %q canonicalize %s: %w", key, path, err)
		}
		if !filepath.IsAbs(canonicalPath) {
			return "", fmt.Errorf("os path root %q canonical path must be absolute, got %q", key, canonicalPath)
		}
		return filepath.Clean(canonicalPath), nil
	case os.IsNotExist(err):
		if status == "active" {
			return "", fmt.Errorf("os path root %q active path must resolve to an existing canonical path: %s", key, path)
		}
		return path, nil
	default:
		return "", fmt.Errorf("os path root %q inspect %s: %w", key, path, err)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mergeEntityForImport(existing []mem.Entity, entity mem.Entity) []mem.Entity {
	for i, e := range existing {
		if e.Slug == entity.Slug {
			existing[i] = entity
			return existing
		}
	}
	return append(existing, entity)
}

func mergeStrings(values []string, more ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range append(values, more...) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}
