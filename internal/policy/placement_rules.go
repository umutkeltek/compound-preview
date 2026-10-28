package policy

import (
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

	"compound/internal/candidates"
	homepkg "compound/internal/home"
)

const (
	PlacementRulesAuthorityRef          = "compound://registries/placement-rules"
	PlacementSourceAuthoritySchema      = "compound.placement_source_authority.v0.1"
	placementRulesFile                  = "placement-rules.json"
	placementSourceAuthorityFile        = "placement-source-authority.jsonl"
	placementSourceAuthorityImportedVia = "policy import-placement-rules"

	// governedHomeRootID is the synthesized closed-world root that protects COMPOUND_HOME
	// itself: a write under the home that lands in NO typed habitat dir is unowned scaffolding.
	// A registry-declared root with this id overrides the synthesized one (lets an operator
	// stage warn -> block, or scope the allow-set).
	governedHomeRootID = "compound-home"
)

type PlacementRules struct {
	GeneratedFrom  string               `json:"generated_from,omitempty"`
	Note           string               `json:"note,omitempty"`
	OpenWriteGlobs []string             `json:"open_write_globs,omitempty"`
	Owners         []PlacementOwnerRule `json:"owners,omitempty"`
	GovernedRoots  []GovernedRoot       `json:"governed_roots,omitempty"`
}

// GovernedRoot is a closed-world rule: any write UNDER RootGlobs that is NOT in AllowGlobs
// (the typed/allowed sub-paths) is unowned scaffolding. This is what an open-world owner-glob
// registry cannot express — it catches a NOVEL filename in an unclaimed location (e.g. an agent
// inventing `<COMPOUND_HOME>/private/stray-note.md`), instead of only denying known deny-zones.
type GovernedRoot struct {
	ID         string   `json:"id"`
	RootGlobs  []string `json:"root_globs,omitempty"`
	AllowGlobs []string `json:"allow_globs,omitempty"`
	Confidence string   `json:"confidence,omitempty"` // "" -> warn; only "warn"|"block" are valid
	Redirect   string   `json:"redirect,omitempty"`   // where the write SHOULD go instead
}

type PlacementOwnerRule struct {
	ID                  string   `json:"id"`
	Owner               string   `json:"owner,omitempty"`
	Status              string   `json:"status,omitempty"`
	CanonicalWriteGlobs []string `json:"canonical_write_globs,omitempty"`
	DenyGlobs           []string `json:"deny_globs,omitempty"`
	CanonFilenameGlobs  []string `json:"canon_filename_globs,omitempty"`
	WriteRoles          []string `json:"write_roles,omitempty"`
	Confidence          string   `json:"confidence,omitempty"`
}

type PlacementRulesImportReport struct {
	AuthorityRef      string                         `json:"authority_ref"`
	AuthorityPath     string                         `json:"authority_path"`
	SourceAuthorityID string                         `json:"source_authority_id"`
	SourceAuthority   PlacementSourceAuthorityRecord `json:"source_authority"`
	OwnerCount        int                            `json:"owner_count"`
	OpenWriteCount    int                            `json:"open_write_count"`
}

type PlacementSourceAuthorityRecord struct {
	SchemaVersion       string   `json:"schema_version"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	SourcePath          string   `json:"source_path"`
	SourceRevision      string   `json:"source_revision"`
	ImportedVia         string   `json:"imported_via"`
	AuthorityRef        string   `json:"authority_ref"`
	AuthorityStatus     string   `json:"authority_status"`
	SourceRole          string   `json:"source_role"`
	Retires             string   `json:"retires"`
	RetirementCondition string   `json:"retirement_condition"`
	Rollback            string   `json:"rollback"`
	OwnerIDs            []string `json:"owner_ids"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

func LoadPlacementRules(path string) (PlacementRules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PlacementRules{}, err
	}
	return parsePlacementRules(data)
}

func validatePlacementRules(rules PlacementRules) error {
	if len(rules.Owners) == 0 && len(rules.GovernedRoots) == 0 {
		return fmt.Errorf("placement rules need owners or governed_roots")
	}
	for _, owner := range rules.Owners {
		if strings.TrimSpace(owner.ID) == "" {
			return fmt.Errorf("placement owner id required")
		}
		confidence := strings.TrimSpace(owner.Confidence)
		if confidence != "" && confidence != "warn" && confidence != "block" {
			return fmt.Errorf("placement owner %q has invalid confidence %q", owner.ID, owner.Confidence)
		}
	}
	for _, root := range rules.GovernedRoots {
		if strings.TrimSpace(root.ID) == "" {
			return fmt.Errorf("governed root id required")
		}
		confidence := strings.TrimSpace(root.Confidence)
		if confidence != "" && confidence != "warn" && confidence != "block" {
			return fmt.Errorf("governed root %q has invalid confidence %q", root.ID, root.Confidence)
		}
	}
	return nil
}

func parsePlacementRules(data []byte) (PlacementRules, error) {
	var rules PlacementRules
	if err := json.Unmarshal(data, &rules); err != nil {
		return PlacementRules{}, fmt.Errorf("parse placement rules: %w", err)
	}
	if err := validatePlacementRules(rules); err != nil {
		return PlacementRules{}, err
	}
	return rules, nil
}

func ImportPlacementRules(homePath, sourcePath string, now time.Time) (PlacementRulesImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return PlacementRulesImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(sourcePath) == "" {
		return PlacementRulesImportReport{}, fmt.Errorf("source path required")
	}
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return PlacementRulesImportReport{}, fmt.Errorf("resolve placement rules source path: %w", err)
	}
	sourcePath = filepath.Clean(sourcePath)
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return PlacementRulesImportReport{}, err
	}
	rules, err := parsePlacementRules(data)
	if err != nil {
		return PlacementRulesImportReport{}, err
	}
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return PlacementRulesImportReport{}, err
	}
	authorityPath := PlacementRulesPath(homePath)
	if err := os.WriteFile(authorityPath, data, 0o644); err != nil {
		return PlacementRulesImportReport{}, err
	}
	sourceRevision := digestHex(data)
	rec := PlacementSourceAuthorityRecord{
		SchemaVersion:       PlacementSourceAuthoritySchema,
		ID:                  placementSourceAuthorityID(sourcePath, sourceRevision),
		CreatedAt:           now.UTC().Format(time.RFC3339),
		SourcePath:          sourcePath,
		SourceRevision:      "sha256:" + sourceRevision,
		ImportedVia:         placementSourceAuthorityImportedVia,
		AuthorityRef:        PlacementRulesAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "source_view_authority:" + sourcePath,
		RetirementCondition: "PreToolUse reads compound://registries/placement-rules from COMPOUND_HOME; source view remains rollback/evidence input",
		Rollback:            "re-import source placement-rules.json or remove COMPOUND_HOME registries/placement-rules.json",
		OwnerIDs:            placementOwnerIDs(rules),
		EvidenceRefs:        []string{"sha256:" + sourceRevision, PlacementRulesAuthorityRef},
	}
	if err := appendPlacementSourceAuthority(homePath, rec); err != nil {
		return PlacementRulesImportReport{}, err
	}
	return PlacementRulesImportReport{
		AuthorityRef:      PlacementRulesAuthorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: rec.ID,
		SourceAuthority:   rec,
		OwnerCount:        len(rules.Owners),
		OpenWriteCount:    len(rules.OpenWriteGlobs),
	}, nil
}

func LoadPlacementRulesFromHome(homePath string) (PlacementRules, error) {
	return LoadPlacementRules(PlacementRulesPath(homePath))
}

func PlacementRulesPath(homePath string) string {
	return filepath.Join(homePath, "registries", placementRulesFile)
}

func PlacementSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", placementSourceAuthorityFile)
}

func LoadPlacementSourceAuthority(homePath string) ([]PlacementSourceAuthorityRecord, error) {
	data, err := os.ReadFile(PlacementSourceAuthorityPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []PlacementSourceAuthorityRecord
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec PlacementSourceAuthorityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse placement source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	return records, nil
}

func PlacementContextFromHome(homePath, targetPath string) (*PlacementGuardContext, []string, error) {
	rules, err := LoadPlacementRulesFromHome(homePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, err
		}
		// No installed registry: still protect the governed home from nothing (fresh clone).
		rules = PlacementRules{}
	}
	rules = ensureGovernedHomeRoot(rules, homePath)
	ctx := rules.PlacementContext(targetPath)
	return ctx, []string{filepath.Join("registries", placementRulesFile)}, nil
}

// ensureGovernedHomeRoot guarantees COMPOUND_HOME is a closed-world governed root even when no
// registry is installed, so a fresh clone protects its own home with zero setup ("works from
// nothing"). A registry-declared root with the same id wins — the operator's escape hatch to
// widen the allow-set or flip warn->block. Synthesized default confidence is "warn" (records,
// never holds); the global placement_guard_mode is the observe->warn->block staging dial.
func ensureGovernedHomeRoot(rules PlacementRules, homePath string) PlacementRules {
	if strings.TrimSpace(homePath) == "" {
		return rules
	}
	for _, gr := range rules.GovernedRoots {
		if strings.TrimSpace(gr.ID) == governedHomeRootID {
			return rules // registry override wins
		}
	}
	rules.GovernedRoots = append(rules.GovernedRoots, GovernedRoot{
		ID:         governedHomeRootID,
		RootGlobs:  []string{filepath.Join(homePath, "**")},
		AllowGlobs: homeAllowGlobs(homePath),
		Confidence: "warn",
		Redirect:   "resolve with `compound placement target --kind <kind>` or write under a typed COMPOUND_HOME habitat dir",
	})
	return rules
}

// homeAllowGlobs is the typed habitat allow-set from HabitatCatalog. Compatibility
// bridges are deliberately excluded: doctor/backup preserve them, but placement
// still redirects new writes toward their canonical destinations.
func homeAllowGlobs(homePath string) []string {
	dirs := homepkg.HabitatPlacementDirs()
	globs := make([]string, 0, len(dirs)*2)
	for _, d := range dirs {
		globs = append(globs, filepath.Join(homePath, d), filepath.Join(homePath, d, "**"))
	}
	return globs
}

func (rules PlacementRules) PlacementContext(targetPath string) *PlacementGuardContext {
	target := normalizePlacementPath(targetPath)
	if target == "" || anyPlacementPathMatch(target, rules.OpenWriteGlobs) {
		return nil
	}
	for _, owner := range rules.Owners {
		if !placementOwnerActive(owner) {
			continue
		}
		if anyPlacementPathMatch(target, owner.DenyGlobs) {
			return owner.placementContext(target, "deny-zone")
		}
		if anyPlacementNameMatch(target, owner.CanonFilenameGlobs) && !anyPlacementPathMatch(target, owner.CanonicalWriteGlobs) {
			return owner.placementContext(target, "canon-filename-outside-owner-home")
		}
	}
	// Closed-world governed roots: a write UNDER a root but in no allowed sub-path is unowned
	// scaffolding. This is the open-world fix — it catches novel filenames in unclaimed places,
	// which the owner deny/canon rules above (negative collisions only) cannot see.
	for _, gr := range rules.GovernedRoots {
		if !anyPlacementPathMatch(target, gr.RootGlobs) {
			continue
		}
		if anyPlacementPathMatch(target, gr.AllowGlobs) {
			continue
		}
		return gr.placementContext(target)
	}
	return nil
}

func (rules PlacementRules) BlockOwnerIDs() []string {
	ids := make([]string, 0, len(rules.Owners))
	for _, owner := range rules.Owners {
		if strings.TrimSpace(owner.Confidence) != "block" {
			continue
		}
		id := strings.TrimSpace(owner.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (owner PlacementOwnerRule) placementContext(targetPath, rule string) *PlacementGuardContext {
	confidence := strings.TrimSpace(owner.Confidence)
	if confidence == "" {
		confidence = "warn"
	}
	ownerID := strings.TrimSpace(owner.ID)
	redirect := deglobTarget(owner.CanonicalWriteGlobs)
	if door, ok := governedDoorRedirect(ownerID); ok {
		// Doors-by-attraction (Wave-2): when this owner's intake is a compound governed door,
		// the guard names THAT door — not the canonical_write_glob a bridge/imported OS source
		// still carries. The bridge glob stays in the registry (keep-all-data) so legacy
		// brain/OS homes remain valid canon; only the redirect an agent acts on is repointed.
		redirect = door
	}
	return &PlacementGuardContext{
		Confidence:     confidence,
		OwnerRef:       "placement-owner:" + ownerID,
		TargetRef:      targetPath,
		Reason:         rule,
		RedirectTarget: redirect,
		EvidenceRefs:   []string{PlacementRulesAuthorityRef, "placement-owner:" + ownerID},
	}
}

// governedDoorOwners maps a placement-owner id to the governed-door candidate kind whose intake
// compound OWNS. When an owner's writes belong to a compound governed door, the placement guard's
// redirect must name THAT door rather than the owner's canonical_write_glob — an imported OS/brain
// source still points that glob at a legacy bridge home outside compound. This
// is the Wave-2 write-surface cutover: compound attracts new writes to its own intake while the
// bridge glob stays authored in the registry (keep-all-data), never a prohibition.
var governedDoorOwners = map[string]candidates.Kind{
	"decisions": candidates.Decision,
}

// governedDoorRedirect returns "<inbox-dir> (via <consumer>)" for a placement owner whose intake is
// a compound governed door, and whether such a door exists. The string names both the door an agent
// should write to and the verb that consumes it (`compound placement target --kind <kind>` resolves
// the same door positively from candidates.Spec, so guard and resolver agree by construction).
func governedDoorRedirect(ownerID string) (string, bool) {
	kind, ok := governedDoorOwners[strings.TrimSpace(ownerID)]
	if !ok {
		return "", false
	}
	spec, ok := candidates.SpecFor(kind)
	if !ok {
		return "", false
	}
	return spec.InboxDir + " (via " + spec.Consumer + ")", true
}

func (gr GovernedRoot) placementContext(targetPath string) *PlacementGuardContext {
	confidence := strings.TrimSpace(gr.Confidence)
	if confidence == "" {
		confidence = "warn"
	}
	id := strings.TrimSpace(gr.ID)
	return &PlacementGuardContext{
		Confidence:     confidence,
		OwnerRef:       "governed-root:" + id,
		TargetRef:      targetPath,
		Reason:         "unowned-under-governed-root",
		RedirectTarget: strings.TrimSpace(gr.Redirect),
		EvidenceRefs:   []string{PlacementRulesAuthorityRef, "governed-root:" + id},
	}
}

// deglobTarget turns the first canonical write glob into a concrete directory hint by stripping
// trailing glob segments (`/**`, `/*`) — the redirect an agent can act on.
func deglobTarget(globs []string) string {
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		g = strings.TrimSuffix(g, "/**")
		g = strings.TrimSuffix(g, "/*")
		return g
	}
	return ""
}

func placementOwnerActive(owner PlacementOwnerRule) bool {
	status := strings.TrimSpace(owner.Status)
	return status == "" || status == "active" || status == "draft-active"
}

func anyPlacementPathMatch(target string, globs []string) bool {
	for _, glob := range globs {
		pattern := expandPlacementPath(glob)
		if pattern == "" {
			continue
		}
		if ok, _ := regexp.MatchString(globToRegexp(filepath.ToSlash(pattern)), target); ok {
			return true
		}
	}
	return false
}

func anyPlacementNameMatch(target string, globs []string) bool {
	name := filepath.Base(target)
	for _, glob := range globs {
		if ok, _ := regexp.MatchString(globToRegexp(filepath.ToSlash(glob)), name); ok {
			return true
		}
	}
	return false
}

func normalizePlacementPath(path string) string {
	return filepath.ToSlash(expandPlacementPath(strings.TrimSpace(path)))
}

func expandPlacementPath(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	if strings.HasPrefix(path, "$HOME/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[6:])
		}
	}
	return path
}

func placementOwnerIDs(rules PlacementRules) []string {
	ids := make([]string, 0, len(rules.Owners))
	for _, owner := range rules.Owners {
		id := strings.TrimSpace(owner.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func appendPlacementSourceAuthority(homePath string, rec PlacementSourceAuthorityRecord) error {
	path := PlacementSourceAuthorityPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func placementSourceAuthorityID(path, revision string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{path, revision, placementSourceAuthorityImportedVia}, "|")))
	return "plsrc_" + hex.EncodeToString(sum[:])[:12]
}

func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// DigestHex exposes the exact hash import uses to stamp SourceRevision, so a reader
// (doctor) can recompute a source view's revision and compare it to what was imported.
// Exported deliberately rather than duplicated: a second implementation that drifted
// from this one would report false drift, which is worse than not checking at all.
func DigestHex(data []byte) string { return digestHex(data) }
