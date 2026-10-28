// Package contract compiles an EffectiveContract for a harness+repo+task and
// assembles the ContextPack an agent actually orients from. The point of the slice
// is path-independence of *meaning*: the same task in a product repo and in the
// control-plane repo must yield DIFFERENT packs, and the pack must record what was
// EXCLUDED and why — so OS-governance context stops being ambient noise in a product
// repo. See docs/architecture/contracts/README.md and docs/architecture/runtime.md.
package contract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"compound/internal/authority"
	"compound/internal/breadth"
	"compound/internal/candidates"
	"compound/internal/capgraph"
	decisionstate "compound/internal/decisions"
	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/mem"
	"compound/internal/negatives"
	"compound/internal/projection"
	"compound/internal/work"
)

const (
	EngineOutcomeNotAttempted = envelope.RetrievalLevelNotAttempted
	EngineOutcomeUsed         = envelope.RetrievalLevelUsed
	EngineOutcomeDegraded     = envelope.RetrievalLevelDegraded
	EngineOutcomeRefused      = envelope.RetrievalLevelRefused

	EngineErrorUnavailable      = envelope.RetrievalErrorUnavailable
	EngineErrorUnsupported      = envelope.RetrievalErrorUnsupported
	EngineErrorEmbedderMismatch = envelope.RetrievalErrorEmbedderMismatch
	EngineErrorProtocolVersion  = envelope.RetrievalErrorProtocolVersion
	EngineErrorEngineAhead      = envelope.RetrievalErrorEngineAhead
	EngineErrorUnownedClaim     = envelope.RetrievalErrorUnownedClaim
	EngineErrorRetrieval        = envelope.RetrievalErrorRetrieval
)

// EngineDependencyTrace is bounded, privacy-safe evidence about the rented breadth
// dependency used while assembling a ContextPack. It never carries a command, argv,
// environment value, query, or raw engine error.
type EngineDependencyTrace struct {
	Attempted        bool
	RequestedEngine  string
	ObservedEngine   string
	Embedder         string
	CorpusGeneration int64
	Outcome          string
	ErrorClass       string
}

func retrievalPostureFromEngineTrace(trace EngineDependencyTrace) *envelope.RetrievalPosture {
	level := trace.Outcome
	if level == "" {
		level = EngineOutcomeNotAttempted
	}
	posture := &envelope.RetrievalPosture{
		Level:     level,
		Authority: envelope.RetrievalAuthorityCompoundLedger,
	}
	switch level {
	case EngineOutcomeUsed:
		posture.RequestedEngine = trace.RequestedEngine
		posture.ObservedEngine = trace.ObservedEngine
		posture.ErrorClass = trace.ErrorClass
	case EngineOutcomeDegraded, EngineOutcomeRefused:
		posture.Fallback = envelope.RetrievalFallbackLedgerRanker
		posture.RequestedEngine = trace.RequestedEngine
		posture.ObservedEngine = trace.ObservedEngine
		posture.ErrorClass = trace.ErrorClass
	case EngineOutcomeNotAttempted:
		posture.RequestedEngine = boundedEngineIdentity(trace.RequestedEngine)
		posture.Fallback = envelope.RetrievalFallbackLedgerRanker
	default:
		// Preserve an impossible internal outcome so ContextPack validation fails
		// closed instead of relabeling it as an offline dependency.
		posture.RequestedEngine = trace.RequestedEngine
		posture.ObservedEngine = trace.ObservedEngine
		posture.ErrorClass = trace.ErrorClass
	}
	return posture
}

const (
	defaultRepoKind                  = "product"
	cheapLaneRankPenalty             = -0.1
	universalPrinciplesRef           = "constitution://universal-principles"
	universalPrinciplesPath          = "docs/architecture/universal-principles.md"
	memoryRelevanceSourceRef         = "memory://relevance-gated"
	positionMemoryReasonPrefix       = "Compound memory folded position; source stores and indexes are evidence, not authority: "
	osAdaptationRuleRef              = "private-plan://operating-system-adaptation-rule"
	osAdaptationCapabilityRef        = "capability://doctrine:context-aware-os-adaptation"
	oracleSecondOpinionRuleRef       = "private-plan://oracle-second-opinion-rule"
	oracleSecondOpinionCapabilityRef = "capability://procedure:oracle-second-opinion"
	// repoMarker is the file a repo commits to self-declare its kind. Generic and
	// leak-free: compound reads the marker, it does not heuristically sniff paths.
	repoMarker = ".compound/repo.yaml"

	repoKindRegistrySchema = "compound.repo_kind_registry.v0.1"
)

// ContextSource is one named class of orientation context and the repo kinds it is
// ambient for. A source ambient for "control-plane" only is excluded in a product repo.
type ContextSource struct {
	Class       string   `yaml:"class" json:"class"`
	SourceClass string   `yaml:"source_class" json:"source_class"`
	AmbientFor  []string `yaml:"ambient_for" json:"ambient_for"`
	Ref         string   `yaml:"ref" json:"ref"`
}

// MemoryRule is the recall gate per repo kind — encodes "personal corpus is not ambient".
type MemoryRule struct {
	Ambient      []string `yaml:"ambient" json:"ambient"`
	ExplicitOnly []string `yaml:"explicit_only" json:"explicit_only"`
	Forbidden    []string `yaml:"forbidden" json:"forbidden"`
}

// OrientationPolicy is the declarative orientation registry: what context is ambient
// for which repo kind, and what memory each kind may load. Shipped as a built-in
// default; overridable from COMPOUND_HOME/config/orientation.yaml.
type OrientationPolicy struct {
	RepoKinds []string              `yaml:"repo_kinds" json:"repo_kinds"`
	Sources   []ContextSource       `yaml:"sources" json:"sources"`
	Memory    map[string]MemoryRule `yaml:"memory" json:"memory"`
}

// RepoKindRegistry is a private/runtime overlay for repos that cannot or should
// not commit a repo-kind marker. It belongs under COMPOUND_HOME/config, not
// public docs; it is still an explicit declaration, not path-name guessing.
type RepoKindRegistry struct {
	SchemaVersion string          `yaml:"schema_version" json:"schema_version"`
	Repos         []RepoKindEntry `yaml:"repos" json:"repos"`
}

type RepoKindEntry struct {
	Kind  string   `yaml:"kind" json:"kind"`
	Roots []string `yaml:"roots" json:"roots"`
}

// DefaultPolicy is the built-in, leak-free orientation policy. Refs use generic
// schemes (no personal paths). It makes the orientation-scope fix concrete: governance
// context is ambient for control-plane only, so a product repo excludes it.
func DefaultPolicy() OrientationPolicy {
	all := []string{"control-plane", "product", "library"}
	return OrientationPolicy{
		RepoKinds: all,
		Sources: []ContextSource{
			{Class: "operating-contract", SourceClass: "source", AmbientFor: all, Ref: "AGENTS.md"},
			// The build ceremonies (execute/land/verify/extract-approach) are the procedural
			// half of the operating contract. Tracked under skills/ (fresh-clone reachable, unlike
			// the gitignored .claude/skills overlay) and ambient-for-all like AGENTS.md, so a
			// stranger's `orient --harness claude` serves them regardless of repo-kind guess.
			{Class: "operating-ceremonies", SourceClass: "source", AmbientFor: all, Ref: "skills/README.md"},
			{Class: "tier0-constitution", SourceClass: "source", AmbientFor: all, Ref: universalPrinciplesRef},
			{Class: "governance-map", SourceClass: "source", AmbientFor: []string{"control-plane"}, Ref: "control-plane://orientation-map"},
			{Class: "governance-workflow-contract", SourceClass: "source", AmbientFor: []string{"control-plane"}, Ref: "control-plane://workflow-contract"},
			{Class: "governance-sop-packets", SourceClass: "source", AmbientFor: []string{"control-plane"}, Ref: "control-plane://sop-packets"},
			{Class: "product-docs", SourceClass: "source", AmbientFor: []string{"product"}, Ref: "repo://docs"},
			{Class: "memory-index", SourceClass: "generated", AmbientFor: all, Ref: memoryRelevanceSourceRef},
		},
		Memory: map[string]MemoryRule{
			"control-plane": {Ambient: []string{"project-facts", "decisions", "system-state"}, ExplicitOnly: []string{"self-model", "personal-corpus"}},
			"product":       {Ambient: []string{"project-facts"}, ExplicitOnly: []string{"decisions"}, Forbidden: []string{"self-model", "personal-corpus"}},
			"library":       {Ambient: []string{"project-facts"}, ExplicitOnly: []string{"decisions"}, Forbidden: []string{"self-model", "personal-corpus"}},
		},
	}
}

// LoadPolicy returns the policy from COMPOUND_HOME/config/orientation.yaml if present,
// else the built-in default. Returns the source ("built-in" or the file path) for
// observability. An empty homePath always falls back to the default.
func LoadPolicy(homePath string) (OrientationPolicy, string, error) {
	if homePath != "" {
		p := filepath.Join(homePath, "config", "orientation.yaml")
		if data, err := os.ReadFile(p); err == nil {
			var pol OrientationPolicy
			if err := yaml.Unmarshal(data, &pol); err != nil {
				return OrientationPolicy{}, p, fmt.Errorf("parse %s: %w", p, err)
			}
			return pol, p, nil
		}
	}
	return DefaultPolicy(), "built-in", nil
}

// LoadRepoKindRegistry reads COMPOUND_HOME/config/repo-kinds.yaml when present.
// Missing registry is not an error; malformed registry is, because silent
// fallback would recreate ambient-context drift.
func LoadRepoKindRegistry(homePath string) (RepoKindRegistry, string, error) {
	if strings.TrimSpace(homePath) == "" {
		return RepoKindRegistry{}, "", nil
	}
	p := filepath.Join(homePath, "config", "repo-kinds.yaml")
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return RepoKindRegistry{}, "", nil
	}
	if err != nil {
		return RepoKindRegistry{}, p, fmt.Errorf("read %s: %w", p, err)
	}
	var reg RepoKindRegistry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return RepoKindRegistry{}, p, fmt.Errorf("parse %s: %w", p, err)
	}
	if reg.SchemaVersion != repoKindRegistrySchema {
		return RepoKindRegistry{}, p, fmt.Errorf("repo kind registry schema_version must be %s, got %q", repoKindRegistrySchema, reg.SchemaVersion)
	}
	for i, repo := range reg.Repos {
		if strings.TrimSpace(repo.Kind) == "" {
			return RepoKindRegistry{}, p, fmt.Errorf("repo kind registry repos[%d] missing kind", i)
		}
		if len(repo.Roots) == 0 {
			return RepoKindRegistry{}, p, fmt.Errorf("repo kind registry repos[%d] missing roots", i)
		}
	}
	return reg, p, nil
}

// DetectRepoKind walks up from cwd for a committed repo-kind marker. Returns ("","")
// when none is found; the caller defaults to product (the lean pack).
func DetectRepoKind(cwd string) (kind, via string) {
	dir := cwd
	for {
		p := filepath.Join(dir, repoMarker)
		if data, err := os.ReadFile(p); err == nil {
			var m struct {
				Kind string `yaml:"kind"`
			}
			if yaml.Unmarshal(data, &m) == nil && m.Kind != "" {
				return m.Kind, p
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

// ResolveRepoKind picks the repo kind: explicit flag wins, else a committed marker,
// else the product default. `via` explains the source for the contract's record.
func DetectRepoKindFromRegistry(reg RepoKindRegistry, registryPath, cwd string) (kind, via string) {
	cwdAbs, err := canonicalContainmentPath(expandPath(cwd))
	if err != nil {
		return "", ""
	}
	bestLen := -1
	for _, repo := range reg.Repos {
		for _, rawRoot := range repo.Roots {
			root := expandPath(rawRoot)
			if !filepath.IsAbs(root) && registryPath != "" {
				root = filepath.Join(filepath.Dir(registryPath), root)
			}
			rootAbs, err := canonicalContainmentPath(root)
			if err != nil || !isWithin(rootAbs, cwdAbs) {
				continue
			}
			if len(rootAbs) > bestLen {
				bestLen = len(rootAbs)
				kind = repo.Kind
				via = "repo-kind-registry:" + registryPath
			}
		}
	}
	return kind, via
}

// canonicalContainmentPath keeps registry roots and orientation cwd values in
// the same physical namespace. On platforms where a public temporary-path
// prefix is itself a symlink, canonicalizing only the cwd makes a valid
// registry root look unrelated to its child. Missing registry roots retain
// their absolute lexical identity so existing declarative entries do not
// become parse errors merely because a checkout is currently absent.
func canonicalContainmentPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(canonical), nil
	}
	if os.IsNotExist(err) {
		return filepath.Clean(abs), nil
	}
	return "", err
}

func ResolveRepoKindWithHome(homePath, flag, cwd string) (kind, via string, err error) {
	if flag != "" {
		return flag, "flag", nil
	}
	if k, p := DetectRepoKind(cwd); k != "" {
		return k, p, nil
	}
	reg, registryPath, err := LoadRepoKindRegistry(homePath)
	if err != nil {
		return "", "", err
	}
	if k, p := DetectRepoKindFromRegistry(reg, registryPath, cwd); k != "" {
		return k, p, nil
	}
	// A linked worktree of a registered repository inherits that repository's
	// DECLARED kind. The worktree path stays the workspace; only the kind and
	// its declared authority change.
	if k, p, werr := resolveRepoKindFromLinkedWorktree(reg, registryPath, cwd); werr != nil {
		return "", "", werr
	} else if k != "" {
		return k, p, nil
	}
	return defaultRepoKind, "default", nil
}

func ResolveRepoKind(flag, cwd string) (kind, via string) {
	kind, via, _ = ResolveRepoKindWithHome("", flag, cwd)
	return kind, via
}

// Input is a resolved orientation request (repo kind already resolved by the caller).
type Input struct {
	Harness           string
	Cwd               string
	Task              string
	TaskContract      envelope.TaskContract
	RepoKind          string
	RepoKindVia       string // "flag" | "<marker path>" | "default"
	Mode              string // observe | plan | execute | review
	Topic             string // optional topic slug; when set, ambient recall narrows to this topic's subtree. Empty = behavior unchanged.
	ReadOnlyRetrieval bool   // transient callers may rank through breadth without capturing observed_fact proposals
}

// ResolveOrientationIdentity validates and canonicalizes the two identities
// that anchor an orientation request before repo detection or runtime
// materialization. An unknown harness must not silently fall through to the
// generic capability profile, and a fictitious workspace must not produce a
// ContextPack that looks like evidence of a real checkout.
//
// An empty harness is preserved for the CLI's usage-error path. Callers are
// still required to supply a real cwd; parseOrientArgsWithHome fills the
// process cwd when --cwd is omitted.
func ResolveOrientationIdentity(in Input, registry harness.Registry) (Input, error) {
	resolved := in
	requestedHarness := strings.TrimSpace(in.Harness)
	if requestedHarness != "" {
		registered, ok := registry.Get(requestedHarness)
		if !ok {
			return Input{}, fmt.Errorf("orientation harness %q is not registered", requestedHarness)
		}
		resolved.Harness = registered.Name
	} else {
		resolved.Harness = ""
	}

	requestedCWD := in.Cwd
	if strings.TrimSpace(requestedCWD) == "" {
		return Input{}, errors.New("orientation cwd is unavailable: path is empty")
	}
	absCWD, err := filepath.Abs(requestedCWD)
	if err != nil {
		return Input{}, fmt.Errorf("orientation cwd %q is unavailable: %w", requestedCWD, err)
	}
	canonicalCWD, err := filepath.EvalSymlinks(absCWD)
	if err != nil {
		return Input{}, fmt.Errorf("orientation cwd %q is unavailable: %w", requestedCWD, err)
	}
	info, err := os.Stat(canonicalCWD)
	if err != nil {
		return Input{}, fmt.Errorf("orientation cwd %q is unavailable: %w", requestedCWD, err)
	}
	if !info.IsDir() {
		return Input{}, fmt.Errorf("orientation cwd %q is not a directory", requestedCWD)
	}
	resolved.Cwd = filepath.Clean(canonicalCWD)
	return resolved, nil
}

const installedHooksSchemaVersion = "compound.installed_hooks.v0.1"

const (
	InstalledHooksAuthorityRef              = "compound://config/installed-hooks"
	InstalledHooksSourceAuthoritySchema     = "compound.installed_hooks_source_authority.v0.1"
	installedHooksSourceAuthorityFile       = "installed-hooks-source-authority.jsonl"
	installedHooksCodexImportedVia          = "contract import-hooks --codex-hooks-json"
	installedHooksRegistryImportedVia       = "contract import-hooks --hook-registration-ts"
	installedHookHandlerUnavailableRevision = "unavailable:concrete-handler"
)

type InstalledHooksManifest struct {
	SchemaVersion string          `yaml:"schema_version" json:"schema_version"`
	Hooks         []InstalledHook `yaml:"hooks" json:"hooks"`
}

type InstalledHook struct {
	Harness            string `yaml:"harness" json:"harness"`
	Name               string `yaml:"name" json:"name"`
	Event              string `yaml:"event,omitempty" json:"event,omitempty"`
	Matcher            string `yaml:"matcher,omitempty" json:"matcher,omitempty"`
	Ref                string `yaml:"ref" json:"ref"`
	InvocationRevision string `yaml:"invocation_revision,omitempty" json:"invocation_revision,omitempty"`
	Revision           string `yaml:"revision" json:"revision"`
	Status             string `yaml:"status" json:"status"`
}

type InstalledHooksImportReport struct {
	AuthorityRef           string                        `json:"authority_ref"`
	AuthorityPath          string                        `json:"authority_path"`
	SourceAuthorityID      string                        `json:"source_authority_id"`
	SourceAuthority        InstalledHooksSourceAuthority `json:"source_authority"`
	IncomingHookCount      int                           `json:"incoming_hook_count"`
	HookCount              int                           `json:"hook_count"`
	AuthorityRevision      string                        `json:"authority_revision"`
	PriorAuthorityRevision string                        `json:"prior_authority_revision,omitempty"`
	Harnesses              []string                      `json:"harnesses"`
	Manifest               InstalledHooksManifest        `json:"manifest"`
}

type HookGateReadiness struct {
	Harness         string           `json:"harness"`
	Hook            string           `json:"hook"`
	NativeBlock     bool             `json:"native_block"`
	Installed       bool             `json:"installed"`
	InstalledRef    string           `json:"installed_ref,omitempty"`
	Status          string           `json:"status"`
	AdapterRequired bool             `json:"adapter_required,omitempty"`
	AdapterCommand  string           `json:"adapter_command,omitempty"`
	Reason          string           `json:"reason,omitempty"`
	Actions         []HookGateAction `json:"actions,omitempty"`
}

type HookGateAction struct {
	Kind             string `json:"kind"`
	Label            string `json:"label"`
	Command          string `json:"command,omitempty"`
	ExpectedExitCode int    `json:"expected_exit_code,omitempty"`
	ExpectedStatus   string `json:"expected_status,omitempty"`
	Boundary         string `json:"boundary,omitempty"`
	Proof            string `json:"proof,omitempty"`
	Rollback         string `json:"rollback,omitempty"`
	RequiresOperator bool   `json:"requires_operator,omitempty"`
}

type HookRuntimeSupport struct {
	Harness        string `json:"harness"`
	Hook           string `json:"hook"`
	FixturePresent bool   `json:"fixture_present"`
	FixtureRef     string `json:"fixture_ref,omitempty"`
	LiveAdapter    bool   `json:"live_adapter"`
	LiveCommand    string `json:"live_command,omitempty"`
}

type InstalledHookRuntimeReadiness struct {
	Harness        string           `json:"harness"`
	Hook           string           `json:"hook"`
	Installed      bool             `json:"installed"`
	InstalledRef   string           `json:"installed_ref,omitempty"`
	FixturePresent bool             `json:"fixture_present"`
	FixtureRef     string           `json:"fixture_ref,omitempty"`
	LiveAdapter    bool             `json:"live_adapter"`
	LiveCommand    string           `json:"live_command,omitempty"`
	Status         string           `json:"status"`
	Reason         string           `json:"reason,omitempty"`
	Actions        []HookGateAction `json:"actions,omitempty"`
}

type InstalledHooksSourceAuthority struct {
	SchemaVersion          string   `json:"schema_version"`
	ID                     string   `json:"id"`
	CreatedAt              string   `json:"created_at"`
	SourcePath             string   `json:"source_path"`
	SourceRevision         string   `json:"source_revision"`
	ImportedVia            string   `json:"imported_via"`
	AuthorityRef           string   `json:"authority_ref"`
	AuthorityRevision      string   `json:"authority_revision,omitempty"`
	PriorAuthorityRevision string   `json:"prior_authority_revision,omitempty"`
	AuthorityStatus        string   `json:"authority_status"`
	SourceRole             string   `json:"source_role"`
	Retires                string   `json:"retires"`
	RetirementCondition    string   `json:"retirement_condition"`
	Rollback               string   `json:"rollback"`
	Harnesses              []string `json:"harnesses"`
	IncomingHookCount      int      `json:"incoming_hook_count,omitempty"`
	HookCount              int      `json:"hook_count"`
	EvidenceRefs           []string `json:"evidence_refs"`
}

// Compile produces the EffectiveContract: the instructions, hooks, memory/capability/
// mutation policy, and unknowns in force for this harness+repo+task.
func Compile(in Input, pol OrientationPolicy, now time.Time) envelope.EffectiveContract {
	incl, excl := partition(pol, in.RepoKind)
	var active []string
	var revs []envelope.SourceRev
	for _, r := range incl {
		if r.SourceClass == "source" {
			active = append(active, r.Ref)
		}
		revs = append(revs, envelope.SourceRev{Source: r.Ref, Rev: "unknown"})
	}
	mem := pol.Memory[in.RepoKind]
	unknowns := []string{}
	if in.RepoKindVia == "default" {
		unknowns = append(unknowns, fmt.Sprintf("repo_kind defaulted to %s (no %s marker found)", in.RepoKind, repoMarker))
	}
	return envelope.EffectiveContract{
		Header:             header(envelope.SVEffectiveContract, contractIDForInput(in), now, in),
		ContextKey:         runtimeContextKey(in),
		Harness:            in.Harness,
		TaskMode:           resolvedTaskContract(in).Mode,
		TaskContract:       resolvedTaskContract(in),
		ActiveInstructions: orEmpty(active),
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		Excluded:           excl,
		MemoryPolicy: envelope.MemoryPolicy{
			Ambient:      orEmpty(mem.Ambient),
			ExplicitOnly: orEmpty(mem.ExplicitOnly),
			Forbidden:    orEmpty(mem.Forbidden),
		},
		CapabilityPolicy: envelope.CapabilityPolicy{
			ToolsAllowed:     []string{},
			ToolsOnDemand:    []string{},
			MCPsNotByDefault: []string{},
		},
		MutationPolicy:  envelope.MutationPolicy{RequiresActionIntent: true, IrreversibleRequiresHuman: true},
		SourceRevisions: revs,
		Unknowns:        unknowns,
		Conflicts:       []string{},
	}
}

// RuntimeMaterialization projects one task-scoped runtime snapshot into both
// artifacts consumed by prompt-time hooks. The trace describes the single
// semantic-memory dependency used by that snapshot.
type RuntimeMaterialization struct {
	EffectiveContract     envelope.EffectiveContract
	ContextPack           envelope.ContextPack
	EngineDependencyTrace EngineDependencyTrace
}

// SealRuntimeMaterialization replaces the request-shaped semantic ids produced
// by Compile and Pack with content-addressed ids for the exact runtime bodies
// that will be persisted and cited as execution proof. latest.json remains the
// mutable "what applies now" projection; the returned contract_* and ctx_* ids
// are immutable materialization refs.
func SealRuntimeMaterialization(runtime RuntimeMaterialization) (RuntimeMaterialization, error) {
	ec, pack := runtime.EffectiveContract, runtime.ContextPack
	if err := ec.ValidateSchemaVersion(); err != nil {
		return RuntimeMaterialization{}, err
	}
	if pack.IDs.ContractID != ec.ID {
		return RuntimeMaterialization{}, fmt.Errorf(
			"context pack contract_id %q does not match effective contract %q",
			pack.IDs.ContractID,
			ec.ID,
		)
	}

	contractBody := ec
	contractBody.ID = ""
	contractDigest, err := json.Marshal(contractBody)
	if err != nil {
		return RuntimeMaterialization{}, err
	}
	ec.ID = runtimeMaterializationID("contract_", contractDigest)

	pack.IDs.ContractID = ec.ID
	packBody := pack
	packBody.ID = ""
	packDigest, err := json.Marshal(packBody)
	if err != nil {
		return RuntimeMaterialization{}, err
	}
	pack.ID = runtimeMaterializationID("ctx_", packDigest)

	runtime.EffectiveContract = ec
	runtime.ContextPack = pack
	return runtime, nil
}

func runtimeMaterializationID(prefix string, body []byte) string {
	digest := sha256.Sum256(body)
	return prefix + hex.EncodeToString(digest[:])[:16]
}

type runtimeOffer struct {
	offer envelope.CapabilityOffer
	rev   envelope.SourceRev
	ok    bool
}

type runtimeSnapshot struct {
	hooks                []string
	excludedHooks        []envelope.ExcludedRef
	hookRoster           []envelope.HookRosterEntry
	hookReadiness        []envelope.HookReadiness
	hookRevisions        []envelope.SourceRev
	decisions            []string
	excludedDecisions    []envelope.ExcludedRef
	decisionRevisions    []envelope.SourceRev
	memoryRefs           []contextMemoryRef
	excludedMemoryRefs   []envelope.ExcludedRef
	positionRefs         []contextPositionRef
	excludedPositionRefs []envelope.ExcludedRef
	memoryRevision       string
	probePlan            *probeSelectionPlan
	probeGroups          []probeGroupSelection
	probeRevision        string
	productFenceRevision string
	productEssence       productEssenceBlock
	hasProductEssence    bool
	capabilityHits       []capgraph.Surfaced
	capabilityTotal      int
	capabilityRevision   string
	osAdaptation         runtimeOffer
	oracleSecondOpinion  runtimeOffer
}

// MaterializeRuntime loads runtime state once and projects the same snapshot
// into the EffectiveContract and ContextPack. Pack-only contextual additions
// remain on the ContextPack projection; the descriptor-backed installed
// entrypoint revision is then bound once into both artifacts.
func MaterializeRuntime(in Input, pol OrientationPolicy, homePath string, now time.Time) (RuntimeMaterialization, error) {
	in = canonicalRuntimeInput(in)
	result := RuntimeMaterialization{
		EffectiveContract:     Compile(in, pol, now),
		ContextPack:           Pack(in, pol, now),
		EngineDependencyTrace: EngineDependencyTrace{Outcome: EngineOutcomeNotAttempted},
	}
	if strings.TrimSpace(homePath) == "" {
		return result, nil
	}
	result.EffectiveContract.SourceRevisions = append([]envelope.SourceRev(nil), result.ContextPack.SourceRevisions...)
	snapshot, err := loadRuntimeSnapshot(in, pol, homePath, &result.EngineDependencyTrace)
	if err != nil {
		return result, err
	}
	result.EffectiveContract = projectRuntimeContract(result.EffectiveContract, snapshot)
	result.ContextPack = projectRuntimePack(result.ContextPack, in, snapshot)
	result.ContextPack.Retrieval = retrievalPostureFromEngineTrace(result.EngineDependencyTrace)
	result.ContextPack, err = appendPackOnlyRuntime(result.ContextPack, in, homePath, now)
	if err != nil {
		return result, err
	}
	return bindRuntimeInstalledHarnessEntrypointRevision(homePath, result)
}

func bindRuntimeInstalledHarnessEntrypointRevision(
	homePath string,
	runtime RuntimeMaterialization,
) (RuntimeMaterialization, error) {
	source, revision, found, err := authority.InstalledHarnessEntrypointBinding(
		homePath,
		runtime.EffectiveContract.Harness,
	)
	if err != nil || !found {
		return runtime, err
	}
	binding := []envelope.SourceRev{{Source: source, Rev: revision}}
	runtime.EffectiveContract.SourceRevisions = mergeSourceRevisions(
		runtime.EffectiveContract.SourceRevisions,
		binding,
	)
	runtime.ContextPack.SourceRevisions = mergeSourceRevisions(
		runtime.ContextPack.SourceRevisions,
		binding,
	)
	return runtime, nil
}

// CompileWithRuntime compiles the static contract and enriches it with runtime
// state owned by COMPOUND_HOME. It remains a compatibility wrapper over the
// shared snapshot loader without paying for ContextPack-only projections.
func CompileWithRuntime(in Input, pol OrientationPolicy, homePath string, now time.Time) (envelope.EffectiveContract, error) {
	ec := Compile(in, pol, now)
	if strings.TrimSpace(homePath) == "" {
		return ec, nil
	}
	ec.SourceRevisions = resolveSourceRevisions(ec.SourceRevisions, in.Cwd)
	trace := EngineDependencyTrace{Outcome: EngineOutcomeNotAttempted}
	snapshot, err := loadRuntimeSnapshot(in, pol, homePath, &trace)
	if err != nil {
		return ec, err
	}
	return projectRuntimeContract(ec, snapshot), nil
}

func loadRuntimeSnapshot(in Input, pol OrientationPolicy, homePath string, trace *EngineDependencyTrace) (runtimeSnapshot, error) {
	var snapshot runtimeSnapshot
	var err error
	snapshot.hooks, snapshot.excludedHooks, snapshot.hookRoster, snapshot.hookRevisions, err = loadInstalledHooksForInput(homePath, in)
	if err != nil {
		return snapshot, err
	}
	snapshot.hookReadiness, err = hookReadinessForInput(homePath, in)
	if err != nil {
		return snapshot, err
	}
	taskEmbedder := resolveTaskEmbedder(homePath, in.Task)
	if in.ReadOnlyRetrieval && taskEmbedder != nil {
		taskEmbedder = &exactTaskMemoizingEmbedder{inner: taskEmbedder, task: in.Task}
	}
	laneResults := startRuntimeSnapshotLanes(homePath, in, taskEmbedder)
	lanes := <-laneResults
	if lanes.err != nil {
		return snapshot, lanes.err
	}
	snapshot.capabilityHits, snapshot.capabilityTotal = lanes.capability.hits, lanes.capability.total
	if snapshot.capabilityTotal > 0 {
		snapshot.capabilityRevision, err = capgraph.AuthorityRevision(homePath)
		if err != nil {
			return snapshot, err
		}
	}
	snapshot.osAdaptation.offer, snapshot.osAdaptation.rev, snapshot.osAdaptation.ok, err = osAdaptationSteeringOffer(in)
	if err != nil {
		return snapshot, err
	}
	snapshot.oracleSecondOpinion.offer, snapshot.oracleSecondOpinion.rev, snapshot.oracleSecondOpinion.ok, err = oracleSecondOpinionOffer(in)
	if err != nil {
		return snapshot, err
	}
	memoryResult := lanes.memory
	dependencies := captureRuntimeMemoryDependencies(homePath, in, memoryResult.view)
	snapshot.productFenceRevision = dependencies.ProductFence.Revision
	snapshot.productEssence, snapshot.hasProductEssence = productEssenceForFence(memoryResult.view, dependencies.ProductFence)
	snapshot.decisions, snapshot.decisionRevisions, snapshot.excludedDecisions, err = loadDecisionRefsFromView(in, memoryResult.view, dependencies.ProductFence)
	if err != nil {
		return snapshot, err
	}
	if captured := runtimeMemoryDependenciesCaptured; captured != nil {
		captured()
	}
	semantic := semanticTaskRankResultWithEmbedderFromView(homePath, in, trace, taskEmbedder, memoryResult.store, memoryResult.view, &dependencies.RecallCandidate)
	dependencies.RecallConsumed = semantic.ConsumedRecall
	semanticRanks := semantic.Ranks
	snapshot.probePlan, err = buildProbeSelectionPlan(homePath, in, memoryResult.store, memoryResult.view)
	if err != nil {
		return snapshot, err
	}
	if snapshot.probePlan == nil {
		snapshot.memoryRefs, snapshot.excludedMemoryRefs, err = loadMemoryRefsFromView(in, pol.Memory[in.RepoKind], semanticRanks, memoryResult.view, &dependencies)
		if err != nil {
			return snapshot, err
		}
		snapshot.positionRefs, snapshot.excludedPositionRefs, err = loadPositionRefsFromView(in, pol.Memory[in.RepoKind], semanticRanks, memoryResult.view, dependencies.TopicSet, dependencies.SelfEntities, dependencies.ProductFence)
		if err != nil {
			return snapshot, err
		}
	} else {
		dependencies.ProbeRevision = snapshot.probePlan.Revision
		snapshot.probeRevision = snapshot.probePlan.Revision
		snapshot.memoryRefs, snapshot.positionRefs, snapshot.excludedMemoryRefs, snapshot.probeGroups = loadProbeGroupedRefsFromView(
			in, pol.Memory[in.RepoKind], semanticRanks, memoryResult.view, &dependencies, snapshot.probePlan,
		)
	}
	if len(snapshot.memoryRefs) > 0 || len(snapshot.excludedMemoryRefs) > 0 || len(snapshot.positionRefs) > 0 || len(snapshot.excludedPositionRefs) > 0 || snapshot.probePlan != nil {
		snapshot.memoryRevision = runtimeMemoryRevision(memoryResult.view, dependencies)
	}
	return snapshot, nil
}

type runtimeCapabilityLaneResult struct {
	hits  []capgraph.Surfaced
	total int
}

type runtimeMemoryLaneResult struct {
	store *mem.Store
	view  *mem.CurrentView
}

type runtimeSnapshotLaneResult struct {
	capability runtimeCapabilityLaneResult
	memory     runtimeMemoryLaneResult
	err        error
}

type runtimeLaneOutcome[T any] struct {
	value T
	err   error
}

// joinRuntimeSnapshotLanes starts both independent lanes before awaiting either
// and always joins both completions, including error paths. A caller can therefore
// return without leaving background reads or cache work running behind it.
func joinRuntimeSnapshotLanes[A, B any](left func() (A, error), right func() (B, error)) (A, B, error) {
	leftDone := make(chan runtimeLaneOutcome[A], 1)
	rightDone := make(chan runtimeLaneOutcome[B], 1)
	go func() {
		value, err := left()
		leftDone <- runtimeLaneOutcome[A]{value: value, err: err}
	}()
	go func() {
		value, err := right()
		rightDone <- runtimeLaneOutcome[B]{value: value, err: err}
	}()
	leftResult := <-leftDone
	rightResult := <-rightDone
	return leftResult.value, rightResult.value, errors.Join(leftResult.err, rightResult.err)
}

func startRuntimeSnapshotLanes(homePath string, in Input, taskEmbedder breadth.EmbedderAdapter) <-chan runtimeSnapshotLaneResult {
	done := make(chan runtimeSnapshotLaneResult, 1)
	go func() {
		capability, memory, err := joinRuntimeSnapshotLanes(
			func() (runtimeCapabilityLaneResult, error) {
				hits, total, err := surfaceCapabilityHits(homePath, in, taskEmbedder)
				return runtimeCapabilityLaneResult{hits: hits, total: total}, err
			},
			func() (runtimeMemoryLaneResult, error) {
				store, err := mem.Open(homePath)
				if err != nil {
					return runtimeMemoryLaneResult{}, err
				}
				var view *mem.CurrentView
				if strings.TrimSpace(in.Topic) == "" {
					view, err = store.CurrentView()
				} else {
					view, err = store.CurrentViewForTopic(in.Topic)
				}
				return runtimeMemoryLaneResult{store: store, view: view}, err
			},
		)
		done <- runtimeSnapshotLaneResult{capability: capability, memory: memory, err: err}
	}()
	return done
}

type runtimeTemperatureDependency struct {
	ClaimID     string  `json:"claim_id"`
	Temperature float64 `json:"temperature"`
}

type runtimeTopicDependency struct {
	Status  string   `json:"status"`
	Slug    string   `json:"slug,omitempty"`
	Via     string   `json:"via,omitempty"`
	Subtree []string `json:"subtree,omitempty"`
}

type runtimeMemoryDependencies struct {
	Temperatures        map[string]float64
	AppliedTemperatures map[string]float64
	Topic               *runtimeTopicDependency
	TopicSet            map[string]bool
	RecallCandidate     mem.ExpandedQuery
	RecallConsumed      *mem.ExpandedQuery
	ProbeRevision       string
	ProductFence        productRecallFence
	// SelfEntities is the resolved self-entity set for homePath (see
	// compound/internal/mem.ResolveSelfEntitySet), captured once per runtime
	// snapshot and threaded through to selfModelClaim/personalMemoryClaim
	// rather than re-resolved per claim.
	SelfEntities map[string]bool
}

// runtimeMemoryDependenciesCaptured is a test seam immediately after every
// auxiliary dependency has been captured and before any consumer runs.
var runtimeMemoryDependenciesCaptured func()

// dependencySelfEntities is the nil-safe accessor callers use to reach
// dependencies.SelfEntities: a nil dependencies pointer (defensive/test
// callers that skip captureRuntimeMemoryDependencies) resolves an
// unconfigured default set rather than silently dropping self-model
// classification.
func dependencySelfEntities(dependencies *runtimeMemoryDependencies) map[string]bool {
	if dependencies != nil && dependencies.SelfEntities != nil {
		return dependencies.SelfEntities
	}
	return mem.ResolveSelfEntitySet("")
}

func captureRuntimeMemoryDependencies(homePath string, in Input, view *mem.CurrentView) runtimeMemoryDependencies {
	dependencies := runtimeMemoryDependencies{
		Temperatures:        loadMemoryTemperatures(homePath),
		SelfEntities:        mem.ResolveSelfEntitySet(homePath),
		AppliedTemperatures: map[string]float64{},
		RecallCandidate:     mem.ExpandedQuery{Original: in.Task, Text: in.Task},
		ProductFence:        resolveProductRecallFence(in, view),
	}
	if topic := strings.TrimSpace(in.Topic); topic != "" {
		root := topic
		resolved, captured := view.TopicResolution(topic)
		if !captured {
			resolved = mem.EntityResolution{Status: "unresolved"}
		}
		if resolved.Status == "resolved" {
			root = resolved.Slug
		}
		subtree := view.TopicSubtree(root)
		dependencies.Topic = &runtimeTopicDependency{Status: resolved.Status, Slug: resolved.Slug, Via: resolved.Via, Subtree: subtree}
		dependencies.TopicSet = make(map[string]bool, len(subtree))
		for _, slug := range subtree {
			dependencies.TopicSet[slug] = true
		}
	}
	if in.ReadOnlyRetrieval {
		if aliases, err := mem.LoadRecallAliases(homePath); err == nil {
			dependencies.RecallCandidate = mem.ExpandQuery(in.Task, aliases)
		}
	}
	return dependencies
}

// runtimeMemoryRevision fingerprints the exact owned view plus the bounded
// auxiliary values that affected this task's prompt projection. Unmatched alias
// rows, absent-claim temperatures, and unrelated entity rows do not churn it.
func runtimeMemoryRevision(view *mem.CurrentView, captured runtimeMemoryDependencies) string {
	dependencies := struct {
		View         string                         `json:"view"`
		Temperatures []runtimeTemperatureDependency `json:"temperatures,omitempty"`
		Topic        *runtimeTopicDependency        `json:"topic,omitempty"`
		RecallAdded  []string                       `json:"recall_added,omitempty"`
		Probe        string                         `json:"probe,omitempty"`
		ProductFence string                         `json:"product_fence,omitempty"`
	}{
		View:         view.Revision(),
		Topic:        captured.Topic,
		Probe:        captured.ProbeRevision,
		ProductFence: captured.ProductFence.Revision,
	}

	for claimID, temperature := range captured.AppliedTemperatures {
		dependencies.Temperatures = append(dependencies.Temperatures, runtimeTemperatureDependency{ClaimID: claimID, Temperature: temperature})
	}
	sort.Slice(dependencies.Temperatures, func(i, j int) bool {
		return dependencies.Temperatures[i].ClaimID < dependencies.Temperatures[j].ClaimID
	})

	if captured.RecallConsumed != nil && captured.RecallConsumed.Active {
		dependencies.RecallAdded = append([]string(nil), captured.RecallConsumed.Added...)
	}
	body, err := json.Marshal(dependencies)
	if err != nil {
		return view.Revision()
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func projectRuntimeContract(ec envelope.EffectiveContract, snapshot runtimeSnapshot) envelope.EffectiveContract {
	ec.ActiveHooks = snapshot.hooks
	ec.HookRoster = snapshot.hookRoster
	ec.HookReadiness = snapshot.hookReadiness
	ec.Excluded = append(ec.Excluded, snapshot.excludedHooks...)
	ec.SourceRevisions = mergeSourceRevisions(ec.SourceRevisions, snapshot.hookRevisions)
	for _, hit := range snapshot.capabilityHits {
		ec.CapabilityOffers = append(ec.CapabilityOffers, capabilityOffer(hit))
	}
	if snapshot.capabilityRevision != "" {
		ec.SourceRevisions = mergeSourceRevisions(ec.SourceRevisions, []envelope.SourceRev{{Source: capgraph.CapabilityGraphAuthorityRef, Rev: snapshot.capabilityRevision}})
	}
	for _, runtime := range []runtimeOffer{snapshot.osAdaptation, snapshot.oracleSecondOpinion} {
		if !runtime.ok {
			continue
		}
		ec.CapabilityOffers = append(ec.CapabilityOffers, runtime.offer)
		ec.SourceRevisions = mergeSourceRevisions(ec.SourceRevisions, []envelope.SourceRev{runtime.rev})
	}
	ec.ActiveDecisions = snapshot.decisions
	ec.Excluded = appendServedRuntimeExclusions(ec.Excluded, snapshot.excludedDecisions)
	ec.SourceRevisions = mergeSourceRevisions(ec.SourceRevisions, snapshot.decisionRevisions)
	ec.Excluded = appendServedRuntimeExclusions(ec.Excluded, snapshot.excludedMemoryRefs)
	for _, ref := range snapshot.memoryRefs {
		ec.ActiveMemory = append(ec.ActiveMemory, ref.Ref)
	}
	ec.Excluded = appendServedRuntimeExclusions(ec.Excluded, snapshot.excludedPositionRefs)
	for _, ref := range snapshot.positionRefs {
		ec.ActiveMemory = append(ec.ActiveMemory, ref.Ref)
	}
	if snapshot.memoryRevision != "" {
		ec.SourceRevisions = mergeSourceRevisions(ec.SourceRevisions, []envelope.SourceRev{{Source: memoryRelevanceSourceRef, Rev: snapshot.memoryRevision}})
	}
	if snapshot.probeRevision != "" {
		ec.SourceRevisions = mergeSourceRevisions(ec.SourceRevisions, []envelope.SourceRev{{Source: probeContractsAuthorityRef, Rev: snapshot.probeRevision}})
	}
	if snapshot.productFenceRevision != "" {
		ec.SourceRevisions = mergeSourceRevisions(ec.SourceRevisions, []envelope.SourceRev{{Source: productRecallFenceAuthorityRef, Rev: snapshot.productFenceRevision}})
	}
	return ec
}

// servedPositionStatus renders a folded position's contested-ness into the wire vocabulary the
// ContextPack schema already carries (envelope.AuthorityStatuses).
//
// The fold decides this, not the projection: mem.Position.Status is "contested" when any claim
// in the slot's chain is marked needs_review, and activePositionFromFold carries that through as
// ActivePosition.Contested. This function only chooses the word.
//
// Every IncludedRef used to be minted "current" unconditionally, which made a served pack
// disagree with itself -- the summary line rendered "[contested]" while the machine-readable ref
// asserted "current". The consumer that exists to act on this (internal/hook
// includedRefTrustTier, which demotes a non-current ref to trust=evidence-only) could not fire
// on status grounds at all, so a reader was told to trust a claim the same pack described as
// disputed. "disputed" is the honest word here: two claims in one slot disagree and neither has
// won, which is not the same as "stale" (superseded by something newer) or "superseded" (a
// successor exists). Reporting the contradiction is the whole move -- the ref stays served,
// demoted, rather than being silently dropped.
func servedPositionStatus(position envelope.ActivePosition) string {
	if position.Contested {
		return "disputed"
	}
	return "current"
}

func projectRuntimePack(pack envelope.ContextPack, in Input, snapshot runtimeSnapshot) envelope.ContextPack {
	pack.ActiveHooks = snapshot.hooks
	pack.HookRoster = snapshot.hookRoster
	for _, row := range snapshot.hookRoster {
		if row.Status != "active" {
			continue
		}
		pack.Included = append(pack.Included, envelope.IncludedRef{
			Ref: row.Ref, SourceClass: "runtime",
			Reason:     firstNonEmpty(row.Reason, "installed hook selected for harness and task"),
			Confidence: 1, Status: "current",
		})
	}
	pack.HookReadiness = snapshot.hookReadiness
	pack.RequiredChecks = append(pack.RequiredChecks, requiredChecksForHookReadiness(snapshot.hookReadiness, in)...)
	pack.Excluded = append(pack.Excluded, snapshot.excludedHooks...)
	pack.SourceRevisions = mergeSourceRevisions(pack.SourceRevisions, snapshot.hookRevisions)
	if len(snapshot.hooks) > 0 {
		pack.SummaryMD += fmt.Sprintf(" %d active hook ref(s) included Compound runtime.", len(snapshot.hooks))
	}
	pack.Excluded = appendServedRuntimeExclusions(pack.Excluded, snapshot.excludedDecisions)
	for _, ref := range snapshot.decisions {
		pack.Included = append(pack.Included, envelope.IncludedRef{
			Ref: ref, SourceClass: "runtime",
			Reason:     "accepted decision from Compound memory ledger; old source is source-in-place evidence",
			Confidence: 1, Status: "current",
		})
	}
	pack.ActiveDecisions = snapshot.decisions
	pack.SourceRevisions = mergeSourceRevisions(pack.SourceRevisions, snapshot.decisionRevisions)
	if len(snapshot.decisions) > 0 {
		pack.SummaryMD += fmt.Sprintf(" %d active decision ref(s) included from Compound runtime.", len(snapshot.decisions))
	}
	pack.Excluded = appendServedRuntimeExclusions(pack.Excluded, snapshot.excludedMemoryRefs)
	for _, ref := range snapshot.memoryRefs {
		pack.ActiveMemory = append(pack.ActiveMemory, ref.Ref)
		pack.Included = append(pack.Included, envelope.IncludedRef{
			Ref: ref.Ref, SourceClass: "runtime", Reason: ref.Reason,
			Confidence: ref.Confidence, Status: "current",
		})
	}
	pack.Excluded = appendServedRuntimeExclusions(pack.Excluded, snapshot.excludedPositionRefs)
	for _, ref := range snapshot.positionRefs {
		pack.ActiveMemory = append(pack.ActiveMemory, ref.Ref)
		pack.ActivePositions = append(pack.ActivePositions, ref.Position)
		pack.Included = append(pack.Included, envelope.IncludedRef{
			Ref: ref.Ref, SourceClass: "runtime", Reason: ref.Reason,
			Confidence: ref.Confidence, Status: servedPositionStatus(ref.Position),
		})
	}
	if snapshot.memoryRevision != "" {
		pack.SourceRevisions = mergeSourceRevisions(pack.SourceRevisions, []envelope.SourceRev{{Source: memoryRelevanceSourceRef, Rev: snapshot.memoryRevision}})
	}
	if snapshot.probeRevision != "" {
		pack.SourceRevisions = mergeSourceRevisions(pack.SourceRevisions, []envelope.SourceRev{{Source: probeContractsAuthorityRef, Rev: snapshot.probeRevision}})
	}
	if snapshot.productFenceRevision != "" {
		pack.SourceRevisions = mergeSourceRevisions(pack.SourceRevisions, []envelope.SourceRev{{Source: productRecallFenceAuthorityRef, Rev: snapshot.productFenceRevision}})
	}
	if snapshot.probePlan == nil && len(snapshot.memoryRefs) > 0 {
		lines := make([]string, 0, len(snapshot.memoryRefs))
		for _, ref := range snapshot.memoryRefs {
			lines = append(lines, ref.Line)
		}
		pack.SummaryMD += " Compound memory (owned ledger): " + strings.Join(lines, "; ")
	}
	if snapshot.probePlan == nil && len(snapshot.positionRefs) > 0 {
		lines := make([]string, 0, len(snapshot.positionRefs))
		for _, ref := range snapshot.positionRefs {
			lines = append(lines, ref.Line)
		}
		pack.SummaryMD += " Compound positions (folded current view): " + strings.Join(lines, "; ")
	}
	if snapshot.probePlan != nil {
		for _, group := range snapshot.probeGroups {
			label := group.Class + "/" + group.Group
			switch {
			case group.Empty:
				pack.SummaryMD += " Probe " + label + ": probed-and-empty."
			case len(group.Lines) > 0:
				pack.SummaryMD += " Probe " + label + ": " + strings.Join(group.Lines, "; ") + "."
			case group.Overlap:
				pack.SummaryMD += " Probe " + label + ": covered by an earlier probe."
			}
		}
	}
	pack = appendProductEssence(pack, snapshot.productEssence, snapshot.hasProductEssence)
	if topic := strings.TrimSpace(in.Topic); topic != "" {
		pack.SummaryMD += fmt.Sprintf(" Ambient recall narrowed to topic %q (subtree-scoped); empty topic leaves recall unchanged.", topic)
	}
	pack = appendSnapshotOfferToPack(pack, snapshot.osAdaptation,
		osAdaptationCapabilityRef,
		"ContextPack selected private OS-adaptation rule for a control-plane task that mentions adapting old operating-system material; refs only, no private body injection",
		" 1 context-aware OS adaptation offer included; private rule is refs-only and not imported as authority.")
	pack = appendSnapshotOfferToPack(pack, snapshot.oracleSecondOpinion,
		oracleSecondOpinionCapabilityRef,
		"ContextPack selected private Oracle second-opinion rule for a high-leverage control-plane task; refs only, no private rule body injection",
		" 1 Oracle second-opinion offer included; private rule is refs-only and Oracle output remains evidence, not authority.")
	if snapshot.capabilityRevision != "" {
		pack.SourceRevisions = mergeSourceRevisions(pack.SourceRevisions, []envelope.SourceRev{{Source: capgraph.CapabilityGraphAuthorityRef, Rev: snapshot.capabilityRevision}})
		for _, hit := range snapshot.capabilityHits {
			pack.CapabilityOffers = append(pack.CapabilityOffers, capabilityOffer(hit))
			pack.Included = append(pack.Included, envelope.IncludedRef{
				Ref: "capability://" + hit.Node.ID, SourceClass: "generated",
				Reason: capabilitySurfaceReason(hit), Confidence: hit.Score, Status: "current",
			})
		}
		if snapshot.capabilityTotal > len(snapshot.capabilityHits) {
			pack.SummaryMD += fmt.Sprintf(" %d of %d capability offer(s) surfaced; %d omitted from context budget; offers are not execution.", len(snapshot.capabilityHits), snapshot.capabilityTotal, snapshot.capabilityTotal-len(snapshot.capabilityHits))
		} else {
			pack.SummaryMD += fmt.Sprintf(" %d capability offer(s) surfaced; offers are not execution.", len(snapshot.capabilityHits))
		}
	}
	return pack
}

const (
	servedProductFenceExclusionRef    = "memory://product-associated-context-omitted"
	servedProductFenceExclusionReason = "product-associated context omitted by workspace product fence"
)

func appendServedRuntimeExclusions(dst []envelope.ExcludedRef, refs []envelope.ExcludedRef) []envelope.ExcludedRef {
	productFenceOmitted := false
	out := make([]envelope.ExcludedRef, 0, len(dst)+len(refs)+1)
	for _, ref := range dst {
		if ref.Ref == servedProductFenceExclusionRef {
			productFenceOmitted = true
			continue
		}
		out = append(out, ref)
	}
	for _, ref := range refs {
		if ref.Ref == servedProductFenceExclusionRef {
			productFenceOmitted = true
			continue
		}
		out = append(out, ref)
	}
	if productFenceOmitted {
		out = append(out, envelope.ExcludedRef{
			Ref:    servedProductFenceExclusionRef,
			Reason: servedProductFenceExclusionReason,
		})
	}
	return out
}

func appendSnapshotOfferToPack(pack envelope.ContextPack, runtime runtimeOffer, ref, reason, summary string) envelope.ContextPack {
	if !runtime.ok {
		return pack
	}
	pack.CapabilityOffers = append(pack.CapabilityOffers, runtime.offer)
	pack.Included = append(pack.Included, envelope.IncludedRef{
		Ref: ref, SourceClass: "private_overlay", Reason: reason, Confidence: 1, Status: "current",
	})
	pack.SourceRevisions = mergeSourceRevisions(pack.SourceRevisions, []envelope.SourceRev{runtime.rev})
	pack.SummaryMD += summary
	return pack
}

type RuntimeMaterializationArtifactPaths struct {
	ScopedLatestPath string `json:"scoped_latest_path,omitempty"`
	ContractPath     string `json:"contract_path"`
	LatestPath       string `json:"latest_path"`
	PackPath         string `json:"pack_path"`
}

type runtimeMaterializationFS struct {
	createTemp    func(string, string) (*os.File, error)
	rename        func(string, string) error
	beforeLock    func()
	lockContended func()
}

// RecordRuntimeMaterialization requires a caller-visible sealed pair, stages
// the dated contract and pack before making either visible, publishes latest
// only after both dated artifacts, and restores preimages on ordinary
// commit-phase errors. Refusing request-shaped ids prevents a caller from
// retaining a stale semantic id for different persisted bytes. Any failed
// restore is joined into the returned error instead of hiding a partial pair.
// A process crash between filesystem renames can still leave a partial pair;
// readers must continue joining artifacts by contract_id rather than treating
// presence as atomic transaction proof.
func RecordRuntimeMaterialization(homePath string, runtime RuntimeMaterialization) (RuntimeMaterializationArtifactPaths, error) {
	return recordRuntimeMaterializationWithFS(homePath, runtime, runtimeMaterializationFS{
		createTemp: os.CreateTemp,
		rename:     os.Rename,
	})
}

func recordRuntimeMaterializationWithFS(homePath string, runtime RuntimeMaterialization, fsOps runtimeMaterializationFS) (resultPaths RuntimeMaterializationArtifactPaths, resultErr error) {
	sealed, err := SealRuntimeMaterialization(runtime)
	if err != nil {
		return RuntimeMaterializationArtifactPaths{}, err
	}
	if sealed.EffectiveContract.ID != runtime.EffectiveContract.ID ||
		sealed.ContextPack.ID != runtime.ContextPack.ID ||
		sealed.ContextPack.IDs.ContractID != runtime.ContextPack.IDs.ContractID {
		return RuntimeMaterializationArtifactPaths{}, fmt.Errorf(
			"runtime materialization must be sealed before recording",
		)
	}
	ec, pack := runtime.EffectiveContract, runtime.ContextPack
	if strings.TrimSpace(homePath) == "" {
		return RuntimeMaterializationArtifactPaths{}, fmt.Errorf("compound home required")
	}
	if pack.IDs.ContractID != ec.ID {
		return RuntimeMaterializationArtifactPaths{}, fmt.Errorf("context pack contract_id %q does not match effective contract %q", pack.IDs.ContractID, ec.ID)
	}
	if err := ec.Validate(); err != nil {
		return RuntimeMaterializationArtifactPaths{}, fmt.Errorf("effective contract invalid: %w", err)
	}
	if err := pack.Validate(); err != nil {
		return RuntimeMaterializationArtifactPaths{}, fmt.Errorf("context pack invalid: %w", err)
	}
	if err := validateInstalledEntrypointRevisionParity(ec.SourceRevisions, pack.SourceRevisions); err != nil {
		return RuntimeMaterializationArtifactPaths{}, err
	}
	createdAt, err := time.Parse(time.RFC3339, ec.CreatedAt)
	if err != nil {
		return RuntimeMaterializationArtifactPaths{}, err
	}
	paths := RuntimeMaterializationArtifactPaths{
		ContractPath: filepath.Join(homePath, "contracts", "effective", createdAt.UTC().Format("2006-01-02"), ec.ID+".json"),
		LatestPath:   filepath.Join(homePath, "contracts", "effective", "latest.json"),
		PackPath:     filepath.Join(homePath, "context", "packs", pack.ID+".json"),
	}
	if ec.ContextKey != "" {
		paths.ScopedLatestPath, err = ScopedEffectiveContractPath(homePath, ec.ContextKey)
		if err != nil {
			return RuntimeMaterializationArtifactPaths{}, err
		}
	}
	ecBytes, err := json.MarshalIndent(ec, "", "  ")
	if err != nil {
		return RuntimeMaterializationArtifactPaths{}, err
	}
	packBytes, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return RuntimeMaterializationArtifactPaths{}, err
	}
	if fsOps.beforeLock != nil {
		fsOps.beforeLock()
	}
	lock, err := acquireRuntimeMaterializationLock(homePath, fsOps.lockContended)
	if err != nil {
		return RuntimeMaterializationArtifactPaths{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.release())
	}()
	type staged struct {
		path, temp string
		body       []byte
		old        []byte
		existed    bool
		immutable  bool
		skip       bool
	}
	items := []staged{
		{path: paths.ContractPath, body: ecBytes, immutable: true},
		{path: paths.PackPath, body: packBytes, immutable: true},
	}
	if paths.ScopedLatestPath != "" {
		items = append(items, staged{path: paths.ScopedLatestPath, body: ecBytes})
	}
	items = append(items, staged{path: paths.LatestPath, body: ecBytes})
	defer func() {
		for _, item := range items {
			if item.temp != "" {
				_ = os.Remove(item.temp)
			}
		}
	}()
	for i := range items {
		if err := os.MkdirAll(filepath.Dir(items[i].path), 0o755); err != nil {
			return RuntimeMaterializationArtifactPaths{}, err
		}
		if old, readErr := os.ReadFile(items[i].path); readErr == nil {
			items[i].old, items[i].existed = old, true
			if items[i].immutable {
				want := append(append([]byte(nil), items[i].body...), '\n')
				if !bytes.Equal(old, want) {
					return RuntimeMaterializationArtifactPaths{}, fmt.Errorf(
						"immutable runtime materialization collision at %q",
						items[i].path,
					)
				}
				items[i].skip = true
				continue
			}
		} else if !os.IsNotExist(readErr) {
			return RuntimeMaterializationArtifactPaths{}, readErr
		}
		file, createErr := fsOps.createTemp(filepath.Dir(items[i].path), ".runtime-materialization-*")
		if createErr != nil {
			return RuntimeMaterializationArtifactPaths{}, createErr
		}
		items[i].temp = file.Name()
		if _, err = file.Write(append(items[i].body, '\n')); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			file.Close()
			os.Remove(items[i].temp)
			return RuntimeMaterializationArtifactPaths{}, err
		}
	}
	committed := []int{}
	for i := range items {
		if items[i].skip {
			continue
		}
		if err := fsOps.rename(items[i].temp, items[i].path); err != nil {
			commitErr := fmt.Errorf("commit runtime materialization %q: %w", items[i].path, err)
			var rollbackErr error
			for j := len(committed) - 1; j >= 0; j-- {
				itemIndex := committed[j]
				if items[itemIndex].existed {
					if restoreErr := os.WriteFile(items[itemIndex].path, items[itemIndex].old, 0o644); restoreErr != nil {
						rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback restore %q: %w", items[itemIndex].path, restoreErr))
					}
				} else {
					if removeErr := os.Remove(items[itemIndex].path); removeErr != nil && !os.IsNotExist(removeErr) {
						rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback remove %q: %w", items[itemIndex].path, removeErr))
					}
				}
			}
			return RuntimeMaterializationArtifactPaths{}, errors.Join(commitErr, rollbackErr)
		}
		committed = append(committed, i)
	}
	return paths, nil
}

// ContextPackContractArtifacts is the immutable context/contract pair selected
// by a recorded ContextPack. The latest EffectiveContract projection is
// deliberately absent: callers executing old work must follow the pack's
// contract_id to its owned dated artifact, not whichever orientation ran last.
type ContextPackContractArtifacts struct {
	ContextPack               envelope.ContextPack
	EffectiveContract         envelope.EffectiveContract
	ContextPackRevision       string
	EffectiveContractRevision string
}

// ReadContextPackContract reads and validates one persisted ContextPack plus
// the immutable EffectiveContract it names. Consumers must not reconstruct
// artifact paths or consult contracts/effective/latest.json.
func ReadContextPackContract(homePath, contextPackID string) (ContextPackContractArtifacts, error) {
	if strings.TrimSpace(homePath) == "" {
		return ContextPackContractArtifacts{}, fmt.Errorf("compound home required")
	}
	contextPackID = strings.TrimSpace(contextPackID)
	if !safeOwnedContractArtifactID(contextPackID, "ctx_") {
		return ContextPackContractArtifacts{}, fmt.Errorf("context pack id %q is invalid", contextPackID)
	}

	packPath := filepath.Join(homePath, "context", "packs", contextPackID+".json")
	packBody, err := os.ReadFile(packPath)
	if err != nil {
		return ContextPackContractArtifacts{}, fmt.Errorf("read ContextPack %s: %w", contextPackID, err)
	}
	var pack envelope.ContextPack
	if err := json.Unmarshal(packBody, &pack); err != nil {
		return ContextPackContractArtifacts{}, fmt.Errorf("decode ContextPack %s: %w", contextPackID, err)
	}
	if pack.ID != contextPackID {
		return ContextPackContractArtifacts{}, fmt.Errorf("ContextPack path id %s contains %s", contextPackID, pack.ID)
	}
	if err := pack.Validate(); err != nil {
		return ContextPackContractArtifacts{}, fmt.Errorf("ContextPack %s invalid: %w", contextPackID, err)
	}

	contractID := strings.TrimSpace(pack.IDs.ContractID)
	if !safeOwnedContractArtifactID(contractID, "contract_") {
		return ContextPackContractArtifacts{}, fmt.Errorf("ContextPack %s contract_id %q is invalid", contextPackID, contractID)
	}
	pattern := filepath.Join(homePath, "contracts", "effective", "*", contractID+".json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return ContextPackContractArtifacts{}, fmt.Errorf("find EffectiveContract %s: %w", contractID, err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return ContextPackContractArtifacts{}, fmt.Errorf("ContextPack %s references missing EffectiveContract %s", contextPackID, contractID)
	}
	if len(matches) != 1 {
		return ContextPackContractArtifacts{}, fmt.Errorf("ContextPack %s references ambiguous EffectiveContract %s (%d owned artifacts)", contextPackID, contractID, len(matches))
	}

	contractBody, err := os.ReadFile(matches[0])
	if err != nil {
		return ContextPackContractArtifacts{}, fmt.Errorf("read EffectiveContract %s: %w", contractID, err)
	}
	var effective envelope.EffectiveContract
	if err := json.Unmarshal(contractBody, &effective); err != nil {
		return ContextPackContractArtifacts{}, fmt.Errorf("decode EffectiveContract %s: %w", contractID, err)
	}
	if effective.ID != contractID {
		return ContextPackContractArtifacts{}, fmt.Errorf("EffectiveContract path id %s contains %s", contractID, effective.ID)
	}
	if err := effective.Validate(); err != nil {
		return ContextPackContractArtifacts{}, fmt.Errorf("EffectiveContract %s invalid: %w", contractID, err)
	}

	sealed, err := SealRuntimeMaterialization(RuntimeMaterialization{EffectiveContract: effective, ContextPack: pack})
	if err != nil {
		return ContextPackContractArtifacts{}, fmt.Errorf("validate immutable runtime pair: %w", err)
	}
	if sealed.EffectiveContract.ID != effective.ID || sealed.ContextPack.ID != pack.ID {
		return ContextPackContractArtifacts{}, fmt.Errorf("immutable runtime pair is unsealed or its content identity changed; materialize a fresh context")
	}
	return ContextPackContractArtifacts{
		ContextPack:               pack,
		EffectiveContract:         effective,
		ContextPackRevision:       "sha256:" + hashBytesFull(packBody),
		EffectiveContractRevision: "sha256:" + hashBytesFull(contractBody),
	}, nil
}

func safeOwnedContractArtifactID(id, prefix string) bool {
	return id != "" &&
		strings.HasPrefix(id, prefix) &&
		id != "." &&
		id != ".." &&
		id == filepath.Base(id) &&
		!strings.ContainsAny(id, `/\`) &&
		!strings.ContainsRune(id, 0)
}

func validateInstalledEntrypointRevisionParity(contractRevisions, packRevisions []envelope.SourceRev) error {
	collect := func(revisions []envelope.SourceRev) map[string]string {
		bindings := map[string]string{}
		for _, revision := range revisions {
			if strings.HasPrefix(revision.Source, authority.InstalledHarnessEntrypointRevisionPrefix) {
				bindings[revision.Source] = revision.Rev
			}
		}
		return bindings
	}
	contractBindings := collect(contractRevisions)
	packBindings := collect(packRevisions)
	sources := make([]string, 0, len(contractBindings)+len(packBindings))
	seen := map[string]bool{}
	for source := range contractBindings {
		seen[source] = true
		sources = append(sources, source)
	}
	for source := range packBindings {
		if !seen[source] {
			sources = append(sources, source)
		}
	}
	sort.Strings(sources)
	for _, source := range sources {
		contractRevision, contractFound := contractBindings[source]
		packRevision, packFound := packBindings[source]
		if !contractFound || !packFound {
			return fmt.Errorf(
				"installed entrypoint revision parity: binding %q must be present on both effective contract and context pack",
				source,
			)
		}
		if strings.TrimSpace(contractRevision) == "" || strings.TrimSpace(packRevision) == "" {
			return fmt.Errorf("installed entrypoint revision parity: binding %q must carry a non-empty revision", source)
		}
		if contractRevision != packRevision {
			return fmt.Errorf(
				"installed entrypoint revision parity: binding %q differs between effective contract %q and context pack %q",
				source,
				contractRevision,
				packRevision,
			)
		}
	}
	return nil
}

type runtimeMaterializationLock struct {
	file *os.File
}

// acquireRuntimeMaterializationLock is the outermost lock for orient artifact
// publication; callers must not hold another Compound lock. It spans preimage
// capture, staging, renames, rollback, and temp cleanup. A process crash makes
// the kernel release flock, but cannot make the multi-rename publication
// atomic; contract_id joins remain the recovery boundary for crash residuals.
func acquireRuntimeMaterializationLock(homePath string, onContended func()) (*runtimeMaterializationLock, error) {
	lockPath := filepath.Join(homePath, "lock", "runtime-materialization.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create runtime materialization lock directory: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open runtime materialization lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(fmt.Errorf("lock runtime materialization: %w", err), file.Close())
		}
		if onContended != nil {
			onContended()
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
			return nil, errors.Join(fmt.Errorf("wait for runtime materialization lock: %w", err), file.Close())
		}
	}
	return &runtimeMaterializationLock{file: file}, nil
}

func (lock *runtimeMaterializationLock) release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}

// Pack assembles the ContextPack an agent orients from. Its EXCLUDED list — what was
// NOT loaded and why — is the whole point: it is required and never silently dropped.
func Pack(in Input, pol OrientationPolicy, now time.Time) envelope.ContextPack {
	incl, excl := partition(pol, in.RepoKind)
	var active []string
	var revs []envelope.SourceRev
	for _, r := range incl {
		if r.SourceClass == "source" {
			active = append(active, r.Ref)
		}
		revs = append(revs, envelope.SourceRev{Source: r.Ref, Rev: "unknown"})
	}
	mem := pol.Memory[in.RepoKind]
	// The hook serves this summary verbatim, so it leads with the same
	// shared baseline + orient reminder the static block carries; both
	// helpers are task-free, keeping per-run text out of the lead.
	summary := HumanOutputBaseline() + "\n\n" + HumanOutputOrientReminder() + "\n\n" + fmt.Sprintf(
		"Oriented as a **%s** repo for `%s`: %d sources included, %d excluded.",
		in.RepoKind, in.Harness, len(incl), len(excl))
	if len(excl) > 0 {
		summary += " Out-of-scope context recorded — see `excluded`."
	}
	if includedRefPresent(incl, universalPrinciplesRef) {
		if laws := universalPrinciplesLawLine(in.Cwd); laws != "" {
			summary += " Tier-0 constitution: " + laws
		}
	}
	hdr := header(envelope.SVContextPack, packIDForInput(in), now, in)
	hdr.IDs.ContractID = contractIDForInput(in)
	return envelope.ContextPack{
		Header:             hdr,
		SummaryMD:          summary,
		TaskContract:       resolvedTaskContract(in),
		Included:           incl,
		Excluded:           excl,
		ActiveInstructions: orEmpty(active),
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       []string{},
		SourceRevisions:    resolveSourceRevisions(revs, in.Cwd),
		RequiredChecks:     []string{},
		Avoid:              orEmpty(mem.Forbidden),
		Retrieval:          retrievalPostureFromEngineTrace(EngineDependencyTrace{Outcome: EngineOutcomeNotAttempted}),
	}
}

// PackWithCapabilities appends CapabilityGraph offers to the ContextPack. It is
// still orientation-only: surfaced capabilities are included refs, not execution.
func PackWithCapabilities(in Input, pol OrientationPolicy, homePath string, now time.Time) (envelope.ContextPack, error) {
	return packWithCapabilities(in, pol, homePath, now, nil)
}

// PackWithCapabilitiesTraced preserves PackWithCapabilities behavior while also
// returning bounded evidence about the breadth dependency used by the real orient
// consumer path.
func PackWithCapabilitiesTraced(in Input, pol OrientationPolicy, homePath string, now time.Time) (envelope.ContextPack, EngineDependencyTrace, error) {
	trace := EngineDependencyTrace{Outcome: EngineOutcomeNotAttempted}
	pack, err := packWithCapabilities(in, pol, homePath, now, &trace)
	return pack, trace, err
}

func packWithCapabilities(in Input, pol OrientationPolicy, homePath string, now time.Time, trace *EngineDependencyTrace) (envelope.ContextPack, error) {
	if trace == nil {
		trace = &EngineDependencyTrace{Outcome: EngineOutcomeNotAttempted}
	}
	pack := Pack(in, pol, now)
	if strings.TrimSpace(homePath) == "" {
		pack.Retrieval = retrievalPostureFromEngineTrace(*trace)
		return pack, nil
	}
	snapshot, err := loadRuntimeSnapshot(in, pol, homePath, trace)
	if err != nil {
		return pack, err
	}
	pack = projectRuntimePack(pack, in, snapshot)
	pack.Retrieval = retrievalPostureFromEngineTrace(*trace)
	return appendPackOnlyRuntime(pack, in, homePath, now)
}

func appendPackOnlyRuntime(pack envelope.ContextPack, in Input, homePath string, now time.Time) (envelope.ContextPack, error) {
	externalIntake, err := capgraph.ExternalRepoIntakeReportForHome(homePath, in.Task)
	if err != nil {
		return pack, err
	}
	if len(externalIntake.Matches) > 0 {
		matches := externalIntake.Matches
		if len(matches) > maxContextExternalRepoIntakeRefs {
			matches = matches[:maxContextExternalRepoIntakeRefs]
		}
		for _, match := range matches {
			pack.Included = append(pack.Included, envelope.IncludedRef{
				Ref:         externalRepoIntakeRef(match.Repo),
				SourceClass: "generated",
				Reason:      externalRepoIntakeReason(match),
				Confidence:  externalRepoIntakeConfidence(match),
				Status:      "current",
			})
		}
		if len(externalIntake.Matches) > len(matches) {
			pack.SummaryMD += fmt.Sprintf(" %d of %d external repo intake match(es) included; intake is review evidence, not adoption.", len(matches), len(externalIntake.Matches))
		} else {
			pack.SummaryMD += fmt.Sprintf(" %d external repo intake match(es) included; intake is review evidence, not adoption.", len(matches))
		}
	}
	records, err := projection.LoadContext(homePath, in.RepoKind)
	if err != nil {
		return pack, err
	}
	for _, record := range records {
		freshness := record.Freshness
		pack.Included = append(pack.Included, envelope.IncludedRef{
			Ref:         projection.ContextRef(freshness),
			SourceClass: "generated",
			Reason:      fmt.Sprintf("projection_freshness status=%s view=%s; generated-view context evidence, not authority or execution", freshness.Status, freshness.ViewRef),
			Confidence:  projectionContextConfidence(freshness.Status),
			Status:      projection.ContextStatus(freshness.Status),
		})
	}
	if len(records) > 0 {
		pack.SummaryMD += fmt.Sprintf(" %d projection freshness ref(s) included; generated views are context evidence, not truth.", len(records))
	}
	pack, err = appendNegatives(pack, homePath, in, now)
	if err != nil {
		return pack, err
	}
	// Attach the CONTEXTUAL, display-only peers view (active leases + capped agent-lane cards).
	// Read-only: it never writes a capgraph/mem byte and never influences routing. Absent content
	// (fresh home) leaves the section off entirely.
	pack.Peers = buildPeersSection(homePath, now)
	return pack, nil
}

// appendNegatives joins the negatives projection (internal/negatives — a
// rebuildable read-only view over inbox/failure-patterns + reviews/route-remorse)
// on the incoming Intent's owner/task-class scope and attaches top-k negative
// cards to the ContextPack. Read-only influence, exactly like the capability-offer
// and projection-freshness sections above it: display only, never routing — it
// never touches ActiveHooks/ActiveDecisions/RequiredChecks/MutationPolicy, and a
// missing projection file fails open (zero cards, no error), mirroring
// mem.LoadTemperatureLookup's degrade-gracefully contract.
// The `now` parameter is the PACK's clock, never time.Now(): negatives.ReadFresh
// compares a persisted projection's age against negatives.StaleAfter, and a wall-clock
// read here would age every fixed-date fixture past the window and red main days after
// a green land (the failure mode AGENTS.md's "inject the clock" rule names).
func appendNegatives(pack envelope.ContextPack, homePath string, in Input, now time.Time) (envelope.ContextPack, error) {
	proj, ok, err := negatives.ReadFresh(homePath, now)
	if err != nil {
		return pack, err
	}
	for _, warning := range proj.Warnings {
		pack.Excluded = append(pack.Excluded, envelope.ExcludedRef{
			Ref: fmt.Sprintf("%s/source/%s/%s", negatives.ProjectionRef, warning.Source, warning.Code),
			Reason: fmt.Sprintf(
				"%d %s source candidate(s) excluded by canonical custody: %s",
				warning.Count,
				warning.Source,
				warning.Code,
			),
		})
	}
	if !ok || len(proj.Records) == 0 {
		return pack, nil
	}
	ownerTokens := negatives.ScopeTokens(in.Harness, in.RepoKind, filepath.Base(in.Cwd))
	taskTokens := negatives.ScopeTokens(in.Task)
	matched := negatives.MatchingRecords(proj.Records, ownerTokens, taskTokens, maxContextNegativeCards)
	if len(matched) == 0 {
		return pack, nil
	}
	for _, rec := range matched {
		pack.Negatives = append(pack.Negatives, envelope.NegativeCard{
			Source:       rec.Source,
			ApproachKey:  rec.ApproachKey,
			WhatWasTried: rec.WhatWasTried,
			WhyItFailed:  rec.WhyItFailed,
			Ref:          rec.Ref,
			Confidence:   rec.Confidence,
		})
		pack.Included = append(pack.Included, envelope.IncludedRef{
			Ref:         rec.Ref,
			SourceClass: "generated",
			Reason:      fmt.Sprintf("negatives_projection source=%s approach_key=%s scope-matched to this task; display-only, never routes/dispatches/claims", rec.Source, rec.ApproachKey),
			Confidence:  rec.Confidence,
			Status:      "current",
		})
	}
	pack.SourceRevisions = mergeSourceRevisions(pack.SourceRevisions, []envelope.SourceRev{{Source: negatives.ProjectionRef, Rev: proj.ComputedAt}})
	pack.SummaryMD += fmt.Sprintf(" %d already-tried-and-failed negative(s) surfaced (scope-matched, display-only).", len(matched))
	return pack, nil
}

const (
	// peerLeaseStaleAfter is the deterministic v1 staleness window for a work lease surfaced in the
	// peers section: a lease whose claimed_at is older than this renders stale. Process/session
	// liveness is deliberately OUT of v1 — a dead-but-unexpired holder still renders active, which
	// is acceptable and documented (the design packet's falsifier d covers only the expiry path).
	peerLeaseStaleAfter    = 24 * time.Hour
	peerLaneCardCap        = 5
	peerTopCapabilitiesCap = 2
	// peerActiveLeaseCap bounds the lease rows carried in the pack, mirroring
	// peerLaneCardCap. work/leases grows monotonically until a lease is released, so an
	// uncapped list makes the peers section the largest thing in the ContextPack on a
	// mature home while saying the least: a stale lease is one bit of information
	// repeated per row. Cap + omitted count keeps the display-only section display-sized.
	peerActiveLeaseCap = 12
)

// buildPeersSection assembles the CONTEXTUAL, display-only peers view at orient time: the active
// work leases (deterministic expiry-based staleness) plus capped agent-lane cards read from slice
// A's capability graph. It is strictly READ-ONLY: it opens the capability graph read-only and never
// writes a capgraph or memory byte (ratified: orient writes ZERO capgraph/mem bytes), and it has
// zero routing influence. It returns nil when there is nothing to show (a fresh home), so the pack
// omits the peers key entirely; read errors fail open (skip that part) rather than break orient.
// Belief-claim cards (slice B — agent-lane beliefs) join this section later; v1 is leases + lanes.
func buildPeersSection(homePath string, now time.Time) *envelope.PeersSection {
	if strings.TrimSpace(homePath) == "" {
		return nil
	}
	leases, omitted := peerActiveLeases(homePath, now)
	lanes := peerLaneCards(homePath)
	if len(leases) == 0 && len(lanes) == 0 {
		return nil
	}
	return &envelope.PeersSection{ActiveLeases: leases, ActiveLeasesOmitted: omitted, Lanes: lanes}
}

// peerActiveLeases lists the work leases as peer rows with deterministic expiry staleness. It only
// touches the work store when a leases dir already exists, so orient never materializes work
// runtime dirs on a fresh home just to report an empty peer set.
func peerActiveLeases(homePath string, now time.Time) ([]envelope.PeerLease, int) {
	if _, err := os.Stat(filepath.Join(homePath, "work", "leases")); err != nil {
		return nil, 0
	}
	store, err := work.Open(homePath)
	if err != nil {
		return nil, 0
	}
	raw, err := store.ListLeases()
	if err != nil {
		return nil, 0
	}
	all := make([]envelope.PeerLease, 0, len(raw))
	for _, l := range raw {
		age, stale := peerLeaseAgeStale(l.ClaimAt, now)
		all = append(all, envelope.PeerLease{WorkID: l.WorkID, Holder: l.By, Age: age, Stale: stale})
	}
	// ListLeases orders oldest-claim-first, which is exactly the wrong end to keep: the
	// rows worth a peer's attention are the ones someone may still be holding. Order
	// live-before-stale, then newest claim first, so the cap drops the least informative
	// rows. Ties fall back to work id for a deterministic pack.
	claimAt := make(map[string]string, len(raw))
	for _, l := range raw {
		claimAt[l.WorkID] = l.ClaimAt
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Stale != all[j].Stale {
			return !all[i].Stale
		}
		ci, cj := claimAt[all[i].WorkID], claimAt[all[j].WorkID]
		if ci != cj {
			return ci > cj
		}
		return all[i].WorkID < all[j].WorkID
	})
	if len(all) <= peerActiveLeaseCap {
		return all, 0
	}
	return all[:peerActiveLeaseCap], len(all) - peerActiveLeaseCap
}

// peerLeaseAgeStale renders a coarse age and the deterministic staleness verdict for a claimed_at
// timestamp. An unparseable time is surfaced as stale/unknown rather than silently rendered active.
func peerLeaseAgeStale(claimedAt string, now time.Time) (string, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(claimedAt))
	if err != nil {
		return "unknown", true
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return peerCoarseAge(d), d > peerLeaseStaleAfter
}

// peerLaneCards reads slice A's agent-lane cards read-only and reduces them to display cards: the
// non-retired lanes capped at peerLaneCardCap (ordered by node id, the reader's stable order), each
// with its top capability domains and tendency slugs. top_capabilities is the first peerTopCapabilities
// domains in the reader's deterministic slug order — validated_by evidence is lane-level (not
// per-domain) in slice A, so it cannot re-rank a single lane's domains; per-domain weighting is
// slice-B territory. Read-only: OpenReadOnly + a pure reader, zero writes.
func peerLaneCards(homePath string) []envelope.PeerLaneCard {
	if !capgraph.Exists(homePath) {
		return nil
	}
	store, err := capgraph.OpenReadOnly(homePath)
	if err != nil {
		return nil
	}
	cards, err := store.AgentLaneCards()
	if err != nil {
		return nil
	}
	out := make([]envelope.PeerLaneCard, 0, peerLaneCardCap)
	for _, c := range cards {
		if c.Retired {
			continue
		}
		if len(out) >= peerLaneCardCap {
			break
		}
		top := c.Capabilities
		if len(top) > peerTopCapabilitiesCap {
			top = top[:peerTopCapabilitiesCap]
		}
		out = append(out, envelope.PeerLaneCard{
			Lane:            c.Name,
			Harness:         c.Harness,
			TopCapabilities: append([]string{}, top...),
			Tendencies:      append([]string{}, c.Tendencies...),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func peerCoarseAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

func surfaceCapabilityHits(homePath string, in Input, embedder breadth.EmbedderAdapter) ([]capgraph.Surfaced, int, error) {
	if !capgraph.Exists(homePath) {
		return nil, 0, nil
	}
	store, err := capgraph.Open(homePath)
	if err != nil {
		return nil, 0, err
	}
	surfaceCtx := capgraph.SurfaceContext{
		Task:     in.Task,
		RepoKind: in.RepoKind,
		Harness:  in.Harness,
		FilePath: in.Cwd,
		Phase:    in.Mode,
	}
	if embedder != nil {
		surfaceCtx.Embedder = embedder
		surfaceCtx.EmbedTimeout = 3 * time.Second
	}
	surfaced, err := store.SurfaceForContext(surfaceCtx)
	if err != nil {
		return nil, 0, err
	}
	surfacedTotal := len(surfaced)
	if len(surfaced) > maxContextCapabilityOffers {
		surfaced = surfaced[:maxContextCapabilityOffers]
	}
	return surfaced, surfacedTotal, nil
}

func osAdaptationSteeringOffer(in Input) (envelope.CapabilityOffer, envelope.SourceRev, bool, error) {
	if !shouldOfferOSAdaptationSteering(in) {
		return envelope.CapabilityOffer{}, envelope.SourceRev{}, false, nil
	}
	rulePath := findUp(in.Cwd, filepath.Join("docs_private", "plan", "OPERATING-SYSTEM-ADAPTATION-RULE.md"))
	if rulePath == "" {
		return envelope.CapabilityOffer{}, envelope.SourceRev{}, false, nil
	}
	rule, err := os.ReadFile(rulePath)
	if err != nil {
		return envelope.CapabilityOffer{}, envelope.SourceRev{}, false, err
	}
	offer := envelope.CapabilityOffer{
		Ref:   osAdaptationCapabilityRef,
		Kind:  "doctrine",
		Name:  "Context-aware operating-system adaptation",
		Score: 1,
		Why:   "private adaptation rule matched control-plane task: trigger=old-system+adaptation; silence=ordinary product/library work",
		Required: []envelope.CapabilityOfferConstraint{
			{
				Kind:       "context_rule",
				Ref:        osAdaptationRuleRef,
				Detail:     "translate old OS material into trigger/action/boundary/silence/proof/retirement metadata before Compound authority",
				Confidence: 1,
			},
			{
				Kind:       "trigger",
				Ref:        "context://old-system-adaptation-task",
				Detail:     "load only when a control-plane task explicitly adapts, migrates, ports, or translates old operating-system, brain, skill, hook, registry, or instruction material into Compound",
				Confidence: 1,
			},
			{
				Kind:       "action",
				Ref:        "compound://runtime/context-pack-adaptation-contract",
				Detail:     "translate old material into a consumed command, hook, projection, doctor, panel, ContextPack, product loop, or proof command",
				Confidence: 1,
			},
			{
				Kind:       "boundary",
				Ref:        "compound://boundary/public-private",
				Detail:     "refs only; do not inject private rule bodies, personal paths, accounts, app IDs, credentials, product data, or private corpus text into public Compound surfaces",
				Confidence: 1,
			},
			{
				Kind:       "silence",
				Ref:        "context://ordinary-product-library-work",
				Detail:     "stay silent for ordinary product/library work and for control-plane tasks that do not mention old-system adaptation",
				Confidence: 1,
			},
			{
				Kind:       "proof_gate",
				Ref:        "compound://architecture-doctor/adaptation-steering",
				Detail:     "strict architecture doctor catches shallow private steering or duplicate active prompts",
				Confidence: 1,
			},
			{
				Kind:       "retirement",
				Ref:        "compound://adaptation/old-owner-narrowing",
				Detail:     "before claiming proof, name the old owner that becomes narrower, retired, or explicitly evidence-only",
				Confidence: 1,
			},
		},
		Conflicts: []envelope.CapabilityOfferConstraint{
			{
				Kind:       "conflicts_with",
				Ref:        "pattern://copy-old-instructions-verbatim",
				Detail:     "do not copy old AGENTS.md, skill bodies, prompt blocks, paths, accounts, or private corpus text into Compound",
				Confidence: 1,
			},
		},
		Evidence: []envelope.CapabilityOfferConstraint{
			{
				Kind:       "source_revision",
				Ref:        osAdaptationRuleRef,
				Detail:     "private rule body consumed by hash only; body is not injected",
				Confidence: 1,
			},
		},
		CostRiskNote: "Use only for Compound control-plane adaptation work; stays silent for ordinary product/library tasks and never imports old-system content by itself.",
	}
	return offer, envelope.SourceRev{Source: osAdaptationRuleRef, Rev: "sha256:" + hashBytes(rule)}, true, nil
}

func oracleSecondOpinionOffer(in Input) (envelope.CapabilityOffer, envelope.SourceRev, bool, error) {
	if !shouldOfferOracleSecondOpinion(in) {
		return envelope.CapabilityOffer{}, envelope.SourceRev{}, false, nil
	}
	rulePath := findUp(in.Cwd, filepath.Join("docs_private", "plan", "ORACLE-SECOND-OPINION-RULE.md"))
	if rulePath == "" {
		return envelope.CapabilityOffer{}, envelope.SourceRev{}, false, nil
	}
	rule, err := os.ReadFile(rulePath)
	if err != nil {
		return envelope.CapabilityOffer{}, envelope.SourceRev{}, false, err
	}
	offer := envelope.CapabilityOffer{
		Ref:   oracleSecondOpinionCapabilityRef,
		Kind:  "procedure",
		Name:  "Oracle second-opinion evidence",
		Score: 1,
		Why:   "private Oracle rule matched control-plane task: trigger=high-leverage-governance; silence=ordinary deterministic work",
		Required: []envelope.CapabilityOfferConstraint{
			{
				Kind:       "context_rule",
				Ref:        oracleSecondOpinionRuleRef,
				Detail:     "ask Oracle only as external evidence for high-leverage governance, authority, routing, hook/spine, memory, or public/private policy decisions",
				Confidence: 1,
			},
			{
				Kind:       "trigger",
				Ref:        "context://high-leverage-governance-change",
				Detail:     "load when the control-plane task explicitly asks for Oracle/second opinion or changes doctrine, authority, ownership, hooks, memory governance, routing, approval boundaries, or public/private policy",
				Confidence: 1,
			},
			{
				Kind:       "action",
				Ref:        "compound://runtime/oracle-second-opinion-evidence",
				Detail:     "attach relevant files, ask one concise architecture question, save the answer privately, then decide locally against Compound evidence and tests",
				Confidence: 1,
			},
			{
				Kind:       "writeback",
				Ref:        "compound://source-observe/second-opinion-evidence",
				Detail:     "preserve useful answers as compound.source_observation.v0.1 and import through compound source observe as method_upgrade no-promote evidence",
				Confidence: 1,
			},
			{
				Kind:       "boundary",
				Ref:        "compound://boundary/oracle-evidence-not-authority",
				Detail:     "do not edit Oracle config, expose private bodies in public docs, treat Oracle output as canon, or block reversible local work if Oracle is unavailable",
				Confidence: 1,
			},
			{
				Kind:       "silence",
				Ref:        "context://ordinary-deterministic-work",
				Detail:     "stay silent for ordinary implementation, tests, docs, source bridges, product work, and deterministic grep/script tasks",
				Confidence: 1,
			},
			{
				Kind:       "proof_gate",
				Ref:        "compound://proof/local-state-delta-after-oracle",
				Detail:     "Oracle alone is not proof; proof is saved private answer plus local tests, doctor, and installed command output for the resulting state delta",
				Confidence: 1,
			},
			{
				Kind:       "retirement",
				Ref:        "compound://adaptation/oracle-backend-narrowing",
				Detail:     "retire or narrow this private rule when a generic governed second-opinion adapter with receipts and policy gates exists",
				Confidence: 1,
			},
		},
		Conflicts: []envelope.CapabilityOfferConstraint{
			{
				Kind:       "conflicts_with",
				Ref:        "pattern://oracle-as-authority",
				Detail:     "do not let Oracle become source of truth, replacement for tests, or a blocker for reversible local execution",
				Confidence: 1,
			},
		},
		Evidence: []envelope.CapabilityOfferConstraint{
			{
				Kind:       "source_revision",
				Ref:        oracleSecondOpinionRuleRef,
				Detail:     "private Oracle rule consumed by hash only; body and saved answers stay private evidence",
				Confidence: 1,
			},
		},
		CostRiskNote: "Use for high-leverage control-plane judgment only; stays silent for deterministic work and never makes Oracle output authoritative.",
	}
	return offer, envelope.SourceRev{Source: oracleSecondOpinionRuleRef, Rev: "sha256:" + hashBytes(rule)}, true, nil
}

func shouldOfferOSAdaptationSteering(in Input) bool {
	if strings.TrimSpace(in.RepoKind) != "control-plane" {
		return false
	}
	task := strings.ToLower(in.Task)
	if !containsAny(task, []string{"operating-system", "old os", "old operating", "legacy os", "brain"}) {
		return false
	}
	return containsAny(task, []string{
		"adapt",
		"absorb",
		"migrate",
		"port",
		"translate",
		"copy",
		"instruction",
		"skill",
		"hook",
		"registry",
		"context-aware",
		"contextpack",
		"authority",
	})
}

func shouldOfferOracleSecondOpinion(in Input) bool {
	if strings.TrimSpace(in.RepoKind) != "control-plane" {
		return false
	}
	task := strings.ToLower(in.Task)
	if strings.Contains(task, "oracle") || strings.Contains(task, "second opinion") || strings.Contains(task, "external opinion") || strings.Contains(task, "cross-model") {
		return true
	}
	if !containsAny(task, []string{"change", "redesign", "decide", "decision", "policy", "governance", "authority", "boundary", "architecture", "doctrine", "route", "routing", "approval"}) {
		return false
	}
	return containsAny(task, []string{
		"ownership",
		"source truth",
		"source-of-truth",
		"spine",
		"hook",
		"memory governance",
		"memory",
		"model routing",
		"tool routing",
		"routing",
		"public private",
		"public/private",
		"approval",
		"doctrine",
	})
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func capabilitySurfaceReason(hit capgraph.Surfaced) string {
	reason := fmt.Sprintf("capability_graph surface score=%.1f kind=%s why=%s", hit.Score, hit.Node.Kind, hit.Why)
	if len(hit.MatchedTokens) > 0 {
		reason += " matched=" + strings.Join(hit.MatchedTokens, ",")
	}
	return reason
}

func capabilityOffer(hit capgraph.Surfaced) envelope.CapabilityOffer {
	return envelope.CapabilityOffer{
		Ref:                   "capability://" + hit.Node.ID,
		Kind:                  hit.Node.Kind,
		Name:                  hit.Node.Name,
		Score:                 hit.Score,
		Why:                   hit.Why,
		Required:              capabilityOfferConstraints(hit.Constraints.Required),
		Conflicts:             capabilityOfferConstraints(hit.Constraints.Conflicts),
		Harnesses:             capabilityOfferConstraints(hit.Constraints.Harnesses),
		KnownMissedWhen:       capabilityOfferConstraints(hit.Constraints.KnownMissedWhen),
		KnownFalseTriggeredBy: capabilityOfferConstraints(hit.Constraints.KnownFalseTriggeredBy),
		Evidence:              capabilityOfferConstraints(hit.Constraints.Evidence),
		ConnectionSurfaces:    capabilityOfferConstraints(hit.Constraints.ConnectionSurfaces),
		CostRiskNote:          hit.Constraints.CostRiskNote,
	}
}

func capabilityOfferConstraints(values []capgraph.SurfacingConstraint) []envelope.CapabilityOfferConstraint {
	out := make([]envelope.CapabilityOfferConstraint, 0, len(values))
	for _, value := range values {
		out = append(out, envelope.CapabilityOfferConstraint{
			Kind:       value.Kind,
			Ref:        value.Ref,
			Detail:     value.Detail,
			Confidence: value.Confidence,
		})
	}
	return out
}

const (
	maxContextMemoryRefs             = 5
	maxContextDecisionRefs           = 12
	maxContextCapabilityOffers       = 12
	maxContextExternalRepoIntakeRefs = 5
	// maxContextNegativeCards bounds the negatives section — scoped surfacing only,
	// never a firehose of every recorded failure (packet boundary).
	maxContextNegativeCards = 5
)

func externalRepoIntakeRef(repo string) string {
	slug := strings.ToLower(strings.TrimSpace(repo))
	slug = strings.TrimSuffix(slug, ".git")
	slug = strings.Trim(slug, "/")
	if slug == "" {
		slug = "unknown"
	}
	return capgraph.ExternalRepoIntakeAuthorityRef + "/" + slug
}

func externalRepoIntakeReason(match capgraph.ExternalRepoIntakeDecision) string {
	parts := []string{
		fmt.Sprintf("external_repo_intake repo=%s decision=%s", match.Repo, match.Decision),
	}
	if match.Lane != "" {
		parts = append(parts, "lane="+match.Lane)
	}
	if match.Owner != "" {
		parts = append(parts, "owner="+match.Owner)
	}
	if match.NextReviewStep != "" {
		parts = append(parts, "next_review_step="+match.NextReviewStep)
	}
	if match.NotToDo != "" {
		parts = append(parts, "boundary="+match.NotToDo)
	}
	return strings.Join(parts, "; ")
}

func externalRepoIntakeConfidence(match capgraph.ExternalRepoIntakeDecision) float64 {
	if match.EvidenceStrength > 0 {
		return match.EvidenceStrength
	}
	switch match.Decision {
	case "keep_needs_review":
		return 0.72
	case "merge_existing":
		return 0.66
	case "reference_only":
		return 0.58
	default:
		return 0.5
	}
}

type contextMemoryRef struct {
	Ref        string
	Reason     string
	Line       string
	Confidence float64
	Seq        int64
	Rank       float64
}

type contextPositionRef struct {
	Ref        string
	Reason     string
	Line       string
	Position   envelope.ActivePosition
	Confidence float64
	Seq        int64
	Rank       float64
}

// breadthEngineFactory builds the rented breadth engine for ambient recall. It is a package var
// so tests can substitute a deterministic fake; production points at the ONE shared factory the
// CLI also uses, so the agent-facing path inherits the exact engine that gives `memory query` its
// measured recall.
var breadthEngineFactory = mem.DefaultBreadthEngine

// externalStoreSelected reports whether the operator selected anything other
// than the supported file backend or supplied any Postgres-only configuration.
// Unknown and partial values deliberately return true: the injected factory
// then rejects them as degraded configuration, so a typo or stray credential
// can never silently fall back to file/breadth recall.
func externalStoreSelected() bool {
	selected := os.Getenv("COMPOUND_STORE")
	if selected != "" && selected != "file" {
		return true
	}
	for _, key := range []string{
		"COMPOUND_EXPERIMENTAL_POSTGRES",
		"COMPOUND_POSTGRES_PROFILE",
		"COMPOUND_DSN",
		"COMPOUND_VECTOR_DIMS",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// pgvectorEnabled preserves the existing composition predicate name while the
// experimental adapter remains the only external-store factory. Admission
// (exact postgres selection + separate opt-in + profile + DSN) is enforced by
// that factory before it parses or dials connection data.
func pgvectorEnabled() bool {
	return externalStoreSelected()
}

// CloseableEngine is a rented recall engine whose connections the caller closes
// after use. The pgvector engine the composition root injects satisfies it.
type CloseableEngine interface {
	mem.Engine
	Close()
}

// pgvectorEngineFactory builds the rented pgvector recall engine when the operator
// selects the postgres store backend. It is a package var wired ONCE by the CLI
// composition root (ADR-0005: the spine stays dependency-free; backend construction
// — pg.Open, the embed endpoint, credentials — lives in cmd/compound, never here).
// A nil factory means no pgvector lane is wired; a factory ERROR is a Degraded
// outcome recorded on the trace, never a silent fall-through to another engine.
var pgvectorEngineFactory func() (CloseableEngine, error)

// SetPgvectorEngineFactory installs the postgres-backed recall engine factory.
// The CLI composition root calls it at startup; tests substitute a fake. A nil
// factory under COMPOUND_STORE=postgres is a selected-but-unavailable backend:
// recall reports Degraded rather than silently falling through to another engine.
func SetPgvectorEngineFactory(factory func() (CloseableEngine, error)) {
	pgvectorEngineFactory = factory
}

// pgvectorEngineOrNil returns the injected pgvector engine, or nil when the
// postgres store is not selected, no factory is wired, or construction fails.
// A missing/failing factory under COMPOUND_STORE=postgres is a Degraded trace
// outcome (mirroring the mem.Open degradation in
// semanticTaskRankResultWithEmbedderFromView): visible evidence, never a hidden
// fallback to another backend. The caller owns Close.
func pgvectorEngineOrNil(trace *EngineDependencyTrace) CloseableEngine {
	if !pgvectorEnabled() {
		return nil
	}
	if pgvectorEngineFactory == nil {
		if trace != nil {
			trace.Attempted = true
			trace.Outcome = EngineOutcomeDegraded
			trace.ErrorClass = EngineErrorUnavailable
		}
		return nil
	}
	eng, err := pgvectorEngineFactory()
	if err != nil {
		if trace != nil {
			trace.Attempted = true
			trace.Outcome = EngineOutcomeDegraded
			trace.ErrorClass = EngineErrorUnavailable
		}
		return nil
	}
	return eng
}

var breadthEmbedderResolver = breadth.ResolveEmbedder

type exactTaskMemoizingEmbedder struct {
	inner breadth.EmbedderAdapter
	task  string

	mu       sync.Mutex
	resolved bool
	vector   []float32
	err      error
}

func resolveTaskEmbedder(homePath, task string) breadth.EmbedderAdapter {
	embedder, ok := breadthEmbedderResolver(homePath)
	if !ok || embedder == nil {
		return nil
	}
	return embedder
}

func (e *exactTaskMemoizingEmbedder) Identity() string { return e.inner.Identity() }
func (e *exactTaskMemoizingEmbedder) Dim() int         { return e.inner.Dim() }

func (e *exactTaskMemoizingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) != 1 || texts[0] != e.task {
		return e.inner.Embed(ctx, texts)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.resolved {
		vectors, err := e.inner.Embed(ctx, texts)
		e.err = err
		if err == nil && len(vectors) == 1 {
			e.vector = append([]float32(nil), vectors[0]...)
		}
		e.resolved = true
	}
	if e.err != nil {
		return nil, e.err
	}
	if e.vector == nil {
		return [][]float32{}, nil
	}
	return [][]float32{append([]float32(nil), e.vector...)}, nil
}

type inProcessBreadthEngine struct {
	engine   *breadth.Engine
	embedder breadth.EmbedderAdapter
}

func (e *inProcessBreadthEngine) Capabilities() mem.EngineCapabilities {
	return mem.EngineCapabilities{
		Name: breadth.Name, Breadth: true, Embedder: e.embedder.Identity(), CorpusGen: e.engine.CorpusGen(),
	}
}

func (e *inProcessBreadthEngine) Index(claims []mem.Claim, gen int64) error {
	texts := make([]mem.ClaimText, 0, len(claims))
	for _, claim := range claims {
		texts = append(texts, mem.ClaimText{ClaimID: claim.ID, Text: mem.ClaimIndexText(claim)})
	}
	_, err := e.engine.IndexWithError(texts, gen)
	return err
}

func (e *inProcessBreadthEngine) Query(text, gate string, allowlist []string, k int) ([]mem.Hit, error) {
	hits, _, err := e.QueryWithDiagnostics(text, gate, allowlist, k)
	return hits, err
}

func (e *inProcessBreadthEngine) QueryWithDiagnostics(text, gate string, allowlist []string, k int) ([]mem.Hit, []string, error) {
	result, err := e.engine.QueryWithLanes(text, gate, allowlist, k)
	return result.Hits, append([]string(nil), result.DegradedLanes...), err
}

// semanticTaskRankK bounds how many breadth hits contribute a relevance weight; the ambient pack
// only surfaces maxContextMemoryRefs, so a small top-k is plenty.
const semanticTaskRankK = 24

type semanticTaskRankResult struct {
	Ranks          map[string]float64
	ConsumedRecall *mem.ExpandedQuery
}

// semanticTaskRankResultWithEmbedderFromView returns claimID → relevance weight from the rented
// breadth engine for the task, used ONLY to sharpen the ambient ranker — the ledger still gates
// class-tier/visibility (the engine ranks, the ledger gates). Transient read-only callers use the
// configured embedder and owned breadth cache in-process; ordinary callers retain the configured
// sidecar path; an operator-selected postgres store routes to the injected pgvector engine.
// Without a configured lane, or on ANY engine error, it returns an empty result so the ambient
// rankers (loadMemoryRefsFromView et al.) run exactly as they would without breadth (bulletproof
// degradation — orient never fails because an embedder/sidecar is down). Weights are descending
// rank positions (not raw cosine) so they compose with the small-integer entity/token scores
// instead of swamping them.
func semanticTaskRankResultWithEmbedderFromView(homePath string, in Input, trace *EngineDependencyTrace, embedder breadth.EmbedderAdapter, store *mem.Store, view *mem.CurrentView, expanded *mem.ExpandedQuery) semanticTaskRankResult {
	if trace != nil {
		if in.ReadOnlyRetrieval && embedder != nil && !pgvectorEnabled() {
			trace.RequestedEngine = breadth.Name
		} else {
			trace.RequestedEngine = requestedEngineIdentity()
		}
	}
	if !in.ReadOnlyRetrieval && strings.TrimSpace(os.Getenv("COMPOUND_MEM_ENGINE_BIN")) == "" && !pgvectorEnabled() {
		return semanticTaskRankResult{}
	}
	if in.ReadOnlyRetrieval && embedder == nil && !pgvectorEnabled() {
		return semanticTaskRankResult{}
	}
	if len(memoryTextTokens(in.Task)) == 0 {
		return semanticTaskRankResult{}
	}
	var err error
	if store == nil {
		store, err = mem.Open(homePath)
		if err != nil {
			if trace != nil {
				trace.Outcome = EngineOutcomeDegraded
				trace.ErrorClass = EngineErrorRetrieval
			}
			return semanticTaskRankResult{}
		}
	}
	if trace != nil {
		trace.Attempted = true
	}
	var eng mem.Engine
	closeEng := func() {}
	if pgvectorEnabled() {
		// The operator selected the postgres+pgvector recall lane: it is the ONLY
		// semantic engine here. A missing/failing factory is Degraded evidence on
		// the trace (marked by pgvectorEngineOrNil), never a silent fall-through
		// to another backend (ADR-0005).
		pgEng := pgvectorEngineOrNil(trace)
		if pgEng == nil {
			return semanticTaskRankResult{}
		}
		eng = pgEng
		closeEng = pgEng.Close
	} else if in.ReadOnlyRetrieval {
		core, err := breadth.NewEngine(breadth.EngineConfig{Embedder: embedder, CompoundHome: homePath})
		if err != nil {
			if trace != nil {
				trace.Outcome = EngineOutcomeDegraded
				trace.ErrorClass = EngineErrorRetrieval
			}
			return semanticTaskRankResult{}
		}
		eng = &inProcessBreadthEngine{engine: core, embedder: embedder}
	} else {
		eng, closeEng = breadthEngineFactory(homePath)
	}
	defer closeEng()
	if trace != nil {
		caps := eng.Capabilities()
		if observed := boundedEngineIdentity(caps.Name); observed != "protocol" {
			trace.ObservedEngine = observed
		}
		trace.Embedder = boundedEmbedderIdentity(caps.Embedder)
		if caps.CorpusGen > 0 {
			trace.CorpusGeneration = caps.CorpusGen
		}
	}
	var queryResult mem.ReadOnlyBreadthQueryResult
	if in.ReadOnlyRetrieval {
		if view != nil {
			if expanded != nil {
				queryResult, err = store.QueryBreadthReadOnlyFromViewExpandedResult(in.Task, semanticTaskRankK, eng, view, *expanded)
			} else {
				queryResult, err = store.QueryBreadthReadOnlyFromViewResult(in.Task, semanticTaskRankK, eng, view)
			}
		} else {
			queryResult, err = store.QueryBreadthReadOnlyResult(in.Task, semanticTaskRankK, eng)
		}
	} else {
		queryResult, err = store.QueryBreadthResult(in.Task, semanticTaskRankK, eng)
	}
	if err != nil {
		if trace != nil {
			trace.Outcome, trace.ErrorClass = classifyEngineTraceError(err)
		}
		return semanticTaskRankResult{}
	}
	if trace != nil {
		caps := eng.Capabilities()
		trace.ObservedEngine = boundedEngineIdentity(caps.Name)
		trace.Embedder = boundedEmbedderIdentity(caps.Embedder)
		if caps.CorpusGen >= 0 {
			trace.CorpusGeneration = caps.CorpusGen
		}
		if trace.ObservedEngine == "" {
			trace.Outcome = EngineOutcomeRefused
			trace.ErrorClass = EngineErrorProtocolVersion
		} else if len(queryResult.DegradedLanes) > 0 {
			trace.Outcome = EngineOutcomeDegraded
			trace.ErrorClass = EngineErrorRetrieval
		} else {
			trace.Outcome = EngineOutcomeUsed
			trace.ErrorClass = ""
		}
	}
	if len(queryResult.Claims) == 0 {
		return semanticTaskRankResult{ConsumedRecall: queryResult.ConsumedExpansion}
	}
	ranks := make(map[string]float64, len(queryResult.Claims))
	for i, c := range queryResult.Claims {
		ranks[c.ID] = float64(len(queryResult.Claims) - i)
	}
	return semanticTaskRankResult{Ranks: ranks, ConsumedRecall: queryResult.ConsumedExpansion}
}

func requestedEngineIdentity() string {
	if pgvectorEnabled() {
		return "pgvector"
	}
	if profile := boundedEngineIdentity(os.Getenv("COMPOUND_MEMORY_CLI_PROFILE")); profile != "" {
		return profile
	}
	if strings.TrimSpace(os.Getenv("COMPOUND_MEM_ENGINE_BIN")) != "" {
		return "protocol"
	}
	return ""
}

func boundedEngineIdentity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 0 || len(value) > 64 {
		return ""
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (i > 0 && (r == '.' || r == '_' || r == '-')) {
			continue
		}
		return ""
	}
	return value
}

func boundedEmbedderIdentity(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == ':' {
			continue
		}
		return ""
	}
	return value
}

func classifyEngineTraceError(err error) (string, string) {
	switch {
	case errors.Is(err, mem.ErrEmbedderMismatch):
		return EngineOutcomeRefused, EngineErrorEmbedderMismatch
	case errors.Is(err, mem.ErrProtocolVersion):
		return EngineOutcomeRefused, EngineErrorProtocolVersion
	case errors.Is(err, mem.ErrEngineAhead):
		return EngineOutcomeRefused, EngineErrorEngineAhead
	case errors.Is(err, mem.ErrEngineServedUnownedClaim):
		return EngineOutcomeRefused, EngineErrorUnownedClaim
	case errors.Is(err, mem.ErrUnsupported):
		return EngineOutcomeDegraded, EngineErrorUnsupported
	case errors.Is(err, mem.ErrEngineUnavailable):
		return EngineOutcomeDegraded, EngineErrorUnavailable
	default:
		return EngineOutcomeDegraded, EngineErrorRetrieval
	}
}

// topicScopeForInput resolves a topic slug to the set of subtree slugs ambient recall
// narrows to (the topic itself plus everything that parts/narrows into it), reusing the
// proven TopicSubtree walk. An empty topic returns a nil set, so the caller's topic gate
// is skipped entirely and ambient recall behaves EXACTLY as it did before topic wiring.
func topicScopeForInput(store *mem.Store, topic string) (map[string]bool, error) {
	if strings.TrimSpace(topic) == "" {
		return nil, nil
	}
	subtree, err := store.TopicSubtree(topic)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(subtree)+1)
	for _, slug := range subtree {
		set[slug] = true
	}
	return set, nil
}

func topicScopeExclusionReason(topic string) string {
	return fmt.Sprintf("ambient recall narrowed to topic %q; claim outside topic subtree left out of active_memory", strings.TrimSpace(topic))
}

func loadMemoryRefsFromView(in Input, rule MemoryRule, semanticRanks map[string]float64, view *mem.CurrentView, dependencies *runtimeMemoryDependencies) ([]contextMemoryRef, []envelope.ExcludedRef, error) {
	if len(rule.Ambient) == 0 {
		return nil, nil, nil
	}
	entities := memoryEntityKeys(in)
	var productFence productRecallFence
	if dependencies != nil {
		productFence = dependencies.ProductFence
	}
	seedProductFenceEntity(entities, productFence)
	taskTokens := memoryTaskTokens(in.Task)
	if len(entities) == 0 && len(taskTokens) == 0 {
		return nil, nil, nil
	}
	claims := view.Claims()
	var topicSet map[string]bool
	var temps map[string]float64
	if dependencies != nil {
		topicSet = dependencies.TopicSet
		temps = dependencies.Temperatures
	}
	selfEntities := dependencySelfEntities(dependencies)
	out := make([]contextMemoryRef, 0, maxContextMemoryRefs)
	excluded := make([]contextMemoryRef, 0, maxContextMemoryRefs)
	for _, claim := range claims {
		sem := semanticRanks[claim.ID]
		baseRelevanceRank := memoryRelevanceRank(claim, entities, taskTokens, sem, nil)
		relevanceRank := memoryRelevanceRank(claim, entities, taskTokens, sem, temps)
		if dependencies != nil && baseRelevanceRank > 0 {
			if temperature := memoryTemperatureTerm(temps, claim.ID); temperature != 0 {
				dependencies.AppliedTemperatures[claim.ID] = temperature
			}
		}
		if !productRecallFenceAllowsClaim(productFence, claim) {
			if relevanceRank > 0 {
				excluded = append(excluded, contextMemoryRef{
					Ref:    servedProductFenceExclusionRef,
					Reason: servedProductFenceExclusionReason,
					Seq:    claim.Seq,
					Rank:   relevanceRank,
				})
			}
			continue
		}
		if topicSet != nil && !mem.ClaimHasTopicRef(claim, topicSet) {
			// Topic narrowing is a HARD scope gate (mirrors the recall/position topic filter):
			// a claim outside the subtree never enters ambient recall. Record only the ones that
			// WOULD have surfaced (relevant) so the narrowing is observable without noise.
			if relevanceRank > 0 {
				excluded = append(excluded, contextMemoryRef{
					Ref:    "claim://" + claim.ID,
					Reason: topicScopeExclusionReason(in.Topic),
					Seq:    claim.Seq,
					Rank:   relevanceRank,
				})
			}
			continue
		}
		if reason := memoryPolicyExclusionReason(claim, rule, relevanceRank, selfEntities); reason != "" {
			excluded = append(excluded, contextMemoryRef{
				Ref:    "claim://" + claim.ID,
				Reason: reason,
				Seq:    claim.Seq,
				Rank:   relevanceRank,
			})
			continue
		}
		if reason := addressedWorkStatusExclusionReason(claim, entities, taskTokens, sem, topicSet != nil); reason != "" {
			if relevanceRank > 0 {
				excluded = append(excluded, contextMemoryRef{Ref: "claim://" + claim.ID, Reason: reason, Seq: claim.Seq, Rank: relevanceRank})
			}
			continue
		}
		rank := ambientMemoryRank(claim, rule, entities, taskTokens, sem, temps, selfEntities)
		if rank <= 0 {
			continue
		}
		line := memoryLine(claim)
		out = append(out, contextMemoryRef{
			Ref:        "claim://" + claim.ID,
			Reason:     "Compound memory ledger claim; source stores and indexes are evidence, not authority: " + line,
			Line:       line,
			Confidence: claim.Confidence,
			Seq:        claim.Seq,
			Rank:       rank,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank > out[j].Rank
		}
		if out[i].Seq == out[j].Seq {
			return out[i].Ref < out[j].Ref
		}
		return out[i].Seq > out[j].Seq
	})
	if len(out) > maxContextMemoryRefs {
		out = out[:maxContextMemoryRefs]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	sort.Slice(excluded, func(i, j int) bool {
		if excluded[i].Rank != excluded[j].Rank {
			return excluded[i].Rank > excluded[j].Rank
		}
		if excluded[i].Seq == excluded[j].Seq {
			return excluded[i].Ref < excluded[j].Ref
		}
		return excluded[i].Seq > excluded[j].Seq
	})
	overflowExcluded := 0
	if len(excluded) > maxContextMemoryRefs {
		overflowExcluded = len(excluded) - maxContextMemoryRefs
		excluded = excluded[:maxContextMemoryRefs]
	}
	excludedRefs := make([]envelope.ExcludedRef, 0, len(excluded)+1)
	for _, ref := range excluded {
		excludedRefs = append(excludedRefs, envelope.ExcludedRef{Ref: ref.Ref, Reason: ref.Reason})
	}
	sort.Slice(excludedRefs, func(i, j int) bool { return excludedRefs[i].Ref < excludedRefs[j].Ref })
	if overflowExcluded > 0 {
		// The itemized list is capped at maxContextMemoryRefs, but the Excluded ledger is
		// documented as never-silently-dropped: record a visible summary of what the cap
		// left out instead of letting the 6th+ exclusion vanish without a trace (audit E11).
		excludedRefs = append(excludedRefs, memoryExclusionOverflowRef(overflowExcluded))
	}
	return out, excludedRefs, nil
}

// memoryExclusionOverflowRef is the visible stand-in for exclusions the itemized-list
// cap (maxContextMemoryRefs) dropped from loadMemoryRefsFromView's Excluded output.
// Its ref uses a dedicated "exclusion-overflow://" scheme so it can never collide with
// a real claim/position/decision/hook ref (each of which uses its own scheme: claim://,
// position://, ...).
func memoryExclusionOverflowRef(omitted int) envelope.ExcludedRef {
	return envelope.ExcludedRef{
		Ref: "exclusion-overflow://memory",
		Reason: fmt.Sprintf(
			"%d additional memory exclusion(s) omitted: the excluded ledger itemizes at most %d entries per contract build; the omitted entries' individual refs/reasons are not listed here",
			omitted, maxContextMemoryRefs,
		),
	}
}

// BenchMemoryRank ranks current memory claim IDs by the SAME ambient-recall relevance
// orient's ContextPack uses (memoryRelevanceRank + ambientMemoryRank under the repo-kind
// MemoryRule, with the identical rank/seq/id tie-breaks as loadMemoryRefs), for the
// evaluator organ (`compound bench`). It lives next to loadMemoryRefs on purpose: it calls
// the real scoring primitives instead of forking a parallel scorer, so the two cannot drift.
//
// It is read-only and deterministic: it uses the lexical relevance core and never invokes the
// semantic sidecar (semanticScore is passed as 0), so a bench run makes zero model calls.
//
// Returns: claim IDs ranked most-relevant first; the set of ALL current claim IDs (so the
// evaluator can separate "engine failed to rank it" from "the id isn't in the ledger"); and
// whether recall was available at all (false => caller marks the memory cases skipped rather
// than failing the run — the graceful-degrade contract).
func BenchMemoryRank(homePath string, in Input, pol OrientationPolicy) (ranked []string, present map[string]bool, available bool, err error) {
	rule := pol.Memory[in.RepoKind]
	if len(rule.Ambient) == 0 {
		return nil, nil, false, nil
	}
	store, err := mem.Open(homePath)
	if err != nil {
		return nil, nil, false, err
	}
	claims, err := store.AllCurrent()
	if err != nil {
		return nil, nil, false, err
	}
	if len(claims) == 0 {
		return nil, map[string]bool{}, false, nil
	}
	present = make(map[string]bool, len(claims))
	for _, claim := range claims {
		present[claim.ID] = true
	}
	entities := memoryEntityKeys(in)
	taskTokens := memoryTaskTokens(in.Task)
	if len(entities) == 0 && len(taskTokens) == 0 {
		// Ledger is available but the task carries no usable recall signal; nothing ranks.
		return nil, present, true, nil
	}
	topicSet, err := topicScopeForInput(store, in.Topic)
	if err != nil {
		return nil, nil, false, err
	}
	type scoredClaim struct {
		id   string
		rank float64
		seq  int64
	}
	// Temperature is loaded INSIDE BenchMemoryRank (not added to its exported signature) so the
	// bench evaluator organ's call site (internal/bench/bench.go) stays call-compatible while the
	// temperature term is exercised through the same scorer orient uses. Fail-open on absence.
	temps := loadMemoryTemperatures(homePath)
	selfEntities := mem.ResolveSelfEntitySet(homePath)
	ordered := make([]scoredClaim, 0, len(claims))
	for _, claim := range claims {
		if topicSet != nil && !mem.ClaimHasTopicRef(claim, topicSet) {
			continue
		}
		relevanceRank := memoryRelevanceRank(claim, entities, taskTokens, 0, temps)
		if memoryPolicyExclusionReason(claim, rule, relevanceRank, selfEntities) != "" {
			continue
		}
		if addressedWorkStatusExclusionReason(claim, entities, taskTokens, 0, topicSet != nil) != "" {
			continue
		}
		rank := ambientMemoryRank(claim, rule, entities, taskTokens, 0, temps, selfEntities)
		if rank <= 0 {
			continue
		}
		ordered = append(ordered, scoredClaim{id: claim.ID, rank: rank, seq: claim.Seq})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].rank != ordered[j].rank {
			return ordered[i].rank > ordered[j].rank
		}
		if ordered[i].seq != ordered[j].seq {
			return ordered[i].seq > ordered[j].seq
		}
		return ordered[i].id < ordered[j].id
	})
	ranked = make([]string, 0, len(ordered))
	for _, sc := range ordered {
		ranked = append(ranked, sc.id)
	}
	return ranked, present, true, nil
}

// contestedPositionExclusionReason names the one condition this branch is willing to explain.
const contestedPositionExclusionReason = "position is contested (its active claim is under review) and is withheld from ambient recall; resolve it with compound memory position <entity>"

// unservedPositionExclusionReason explains why a folded position could not be served, for the
// positions the incoming task was actually asking about.
//
// Two deliberate narrowings. It reports ONLY contested positions: a position can also be
// unservable because its active claim was retired, and claiming that as a dispute would be a
// wrong reason, which is worse than a documented gap. And it reports only task-relevant ones,
// because every ledger holds positions nobody asked about -- listing them all would bury the
// signal in the surface built to carry it, which is how a reader learns to stop reading
// exclusions at all.
func unservedPositionExclusionReason(position mem.Position, entities, taskTokens map[string]bool) string {
	if !strings.EqualFold(strings.TrimSpace(position.Status), "contested") {
		return ""
	}
	if !positionMatchesTaskScope(position, entities, taskTokens) {
		return ""
	}
	return contestedPositionExclusionReason
}

// positionMatchesTaskScope reports whether a position's own identity overlaps the task, using
// only the position row. The usual relevance rank needs the active claim, which is exactly what
// is missing whenever this is called.
func positionMatchesTaskScope(position mem.Position, entities, taskTokens map[string]bool) bool {
	for _, field := range []string{position.Entity, position.Topic, position.Predicate} {
		key := memoryKey(field)
		if len(key) < 3 {
			continue
		}
		if entities[key] || taskTokens[key] {
			return true
		}
		for token := range memoryTextTokens(field) {
			if entities[token] || taskTokens[token] {
				return true
			}
		}
	}
	return false
}

func loadPositionRefsFromView(in Input, rule MemoryRule, semanticRanks map[string]float64, view *mem.CurrentView, topicSet map[string]bool, selfEntities map[string]bool, productFence productRecallFence) ([]contextPositionRef, []envelope.ExcludedRef, error) {
	if len(rule.Ambient) == 0 {
		return nil, nil, nil
	}
	entities := memoryEntityKeys(in)
	seedProductFenceEntity(entities, productFence)
	taskTokens := memoryTaskTokens(in.Task)
	if len(entities) == 0 && len(taskTokens) == 0 {
		return nil, nil, nil
	}
	positions := view.Positions()
	if len(positions) == 0 {
		return nil, nil, nil
	}
	if selfEntities == nil {
		selfEntities = mem.ResolveSelfEntitySet("")
	}
	claims := view.Claims()
	claimByID := map[string]mem.Claim{}
	for _, claim := range claims {
		claimByID[claim.ID] = claim
	}
	out := make([]contextPositionRef, 0, maxContextMemoryRefs)
	excluded := make([]contextPositionRef, 0, maxContextMemoryRefs)
	for _, position := range positions {
		activeClaim, ok := claimByID[position.ActiveClaimID]
		if !ok {
			// The active claim is absent from the current view -- overwhelmingly because it
			// carries an open needs_review mark, which mem.visibleCurrentViewClaims drops so an
			// unadjudicated stance is never served as truth. That part is correct and stays.
			//
			// Dropping the POSITION in silence too was not: the pack then said nothing at all,
			// and a reader could not tell a fact that was adjudicated away from one nobody had
			// ever looked at. Measured on the live ledger when this was written, 25 of 26
			// contested positions reached exactly this branch, which is why the disputed status
			// on the served side was almost entirely dormant. The claim still does not enter
			// ambient recall; only the omission becomes visible, which is the rule every other
			// skip in this loop already follows.
			if reason := unservedPositionExclusionReason(position, entities, taskTokens); reason != "" {
				excluded = append(excluded, contextPositionRef{
					Ref:    "position://" + position.ID,
					Reason: reason,
					Seq:    position.AsOfSeq,
				})
			}
			continue
		}
		activePosition := activePositionFromFold(position, activeClaim)
		if activePosition.Ref == "" {
			continue
		}
		sem := semanticRanks[activeClaim.ID]
		// Positions rank by stance stability, not consumption; temperature is scoped to claim
		// recall this round (nil lookup => no temperature term). Extending it to positions is a
		// named follow-up.
		relevanceRank := memoryRelevanceRank(activeClaim, entities, taskTokens, sem, nil)
		if !productRecallFenceAllowsClaim(productFence, activeClaim) {
			if relevanceRank > 0 {
				excluded = append(excluded, contextPositionRef{
					Ref:    servedProductFenceExclusionRef,
					Reason: servedProductFenceExclusionReason,
					Seq:    activeClaim.Seq,
					Rank:   relevanceRank,
				})
			}
			continue
		}
		if topicSet != nil && !probePositionInTopicSet(activePosition, activeClaim, topicSet) {
			// A folded position owns its topic axis. Older promoted claims may predate TopicRefs,
			// so narrowing must also accept Position.Topic or valid subtree positions disappear.
			if relevanceRank > 0 {
				excluded = append(excluded, contextPositionRef{
					Ref:    activePosition.Ref,
					Reason: topicScopeExclusionReason(in.Topic),
					Seq:    activeClaim.Seq,
					Rank:   relevanceRank,
				})
			}
			continue
		}
		if reason := memoryPolicyExclusionReason(activeClaim, rule, relevanceRank, selfEntities); reason != "" {
			excluded = append(excluded, contextPositionRef{
				Ref:    activePosition.Ref,
				Reason: reason,
				Seq:    activeClaim.Seq,
				Rank:   relevanceRank,
			})
			continue
		}
		if reason := addressedWorkStatusExclusionReason(activeClaim, entities, taskTokens, sem, topicSet != nil); reason != "" {
			if relevanceRank > 0 {
				excluded = append(excluded, contextPositionRef{Ref: activePosition.Ref, Reason: reason, Seq: position.AsOfSeq, Rank: relevanceRank})
			}
			continue
		}
		rank := ambientMemoryRank(activeClaim, rule, entities, taskTokens, sem, nil, selfEntities)
		if rank <= 0 {
			continue
		}
		if reason := currentStatePositionFreshnessExclusionReason(activePosition.Ref, activeClaim); reason != "" {
			excluded = append(excluded, contextPositionRef{
				Ref:    activePosition.Ref,
				Reason: reason,
				Seq:    activeClaim.Seq,
				Rank:   rank,
			})
			continue
		}
		out = append(out, contextPositionRef{
			Ref:        activePosition.Ref,
			Reason:     positionMemoryReasonPrefix + activePosition.Line,
			Line:       activePosition.Line,
			Position:   activePosition,
			Confidence: activeClaim.Confidence,
			Seq:        activeClaim.Seq,
			Rank:       rank,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank > out[j].Rank
		}
		if out[i].Seq == out[j].Seq {
			return out[i].Ref < out[j].Ref
		}
		return out[i].Seq > out[j].Seq
	})
	if len(out) > maxContextMemoryRefs {
		out = out[:maxContextMemoryRefs]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	sort.Slice(excluded, func(i, j int) bool {
		if excluded[i].Rank != excluded[j].Rank {
			return excluded[i].Rank > excluded[j].Rank
		}
		if excluded[i].Seq == excluded[j].Seq {
			return excluded[i].Ref < excluded[j].Ref
		}
		return excluded[i].Seq > excluded[j].Seq
	})
	if len(excluded) > maxContextMemoryRefs {
		excluded = excluded[:maxContextMemoryRefs]
	}
	excludedRefs := make([]envelope.ExcludedRef, 0, len(excluded))
	for _, ref := range excluded {
		excludedRefs = append(excludedRefs, envelope.ExcludedRef{Ref: ref.Ref, Reason: ref.Reason})
	}
	sort.Slice(excludedRefs, func(i, j int) bool { return excludedRefs[i].Ref < excludedRefs[j].Ref })
	return out, excludedRefs, nil
}

func currentStatePositionFreshnessExclusionReason(positionRef string, activeClaim mem.Claim) string {
	if !strings.EqualFold(strings.TrimSpace(activeClaim.ClaimType), "status") {
		return ""
	}
	if !hasBarePositionEvidenceRef(activeClaim.EvidenceRefs, "compound://workbench") {
		return ""
	}
	return fmt.Sprintf(
		"current-state position %s backed by claim://%s excluded: compound://workbench evidence is mutable and unversioned; resolve current state with compound workbench --json",
		positionRef,
		activeClaim.ID,
	)
}

func hasBarePositionEvidenceRef(refs []string, want string) bool {
	for _, ref := range refs {
		if strings.TrimSpace(ref) == want {
			return true
		}
	}
	return false
}

func activePositionFromFold(position mem.Position, activeClaim mem.Claim) envelope.ActivePosition {
	entity := firstNonEmpty(position.Entity, activeClaim.Entity)
	topic := firstNonEmpty(position.Topic, activeClaim.Topic())
	predicate := firstNonEmpty(position.Predicate, activeClaim.Predicate)
	claimType := firstNonEmpty(position.ClaimType, activeClaim.ClaimType)
	stance := firstNonEmpty(position.ActiveObject, activeClaim.Object)
	if entity == "" || topic == "" || stance == "" {
		return envelope.ActivePosition{}
	}
	positionID := firstNonEmpty(position.ID, entity+"::"+topic)
	chainLength := len(position.ClaimChain)
	if chainLength == 0 && position.ActiveClaimID != "" {
		chainLength = 1
	}
	contested := strings.EqualFold(strings.TrimSpace(position.Status), "contested")
	line := activePositionLine(positionID, predicate, claimType, stance, activeClaim.Seq, chainLength, contested)
	return envelope.ActivePosition{
		Ref:         "position://" + positionID,
		Entity:      entity,
		Topic:       topic,
		Stance:      stance,
		SinceSeq:    activeClaim.Seq,
		ChainLength: chainLength,
		Contested:   contested,
		Line:        line,
	}
}

func activePositionLine(positionID, predicate, claimType, stance string, sinceSeq int64, chainLength int, contested bool) string {
	slotFields := make([]string, 0, 2)
	if predicate = strings.TrimSpace(predicate); predicate != "" {
		slotFields = append(slotFields, "predicate="+predicate)
	}
	if claimType = strings.TrimSpace(claimType); claimType != "" {
		slotFields = append(slotFields, "claim_type="+claimType)
	}
	slot := ""
	if len(slotFields) > 0 {
		slot = " [" + strings.Join(slotFields, ", ") + "]"
	}
	// The stance is the only unbounded part of this line, and it is the same kind of
	// ledger-derived body memoryLine has always capped — an uncapped stance made positions
	// 1,707 bytes of a 3,935-byte SummaryMD. positionID and the slot stay whole: they are how
	// the reader resolves the full position, so truncating them would cost the drill-down.
	line := fmt.Sprintf("%s%s → %s (since-seq %d, chain-length %d)", positionID, slot, boundedSummaryText(stance), sinceSeq, chainLength)
	if contested {
		line += " [contested]"
	}
	return line
}

// temps is the claim-id → frozen temperature lookup from the memory-temperature projection.
// A nil/empty map (no projection) is fail-open: every rank is byte-identical to the
// pre-temperature behavior. See memoryTemperatureTerm.
func memoryRelevanceRank(claim mem.Claim, entities, taskTokens map[string]bool, semanticScore float64, temps map[string]float64) float64 {
	// Gates first — the engine RANKS, the ledger GATES. A claim these zero out stays zeroed no
	// matter how relevant the breadth engine judged it (decisions go through loadDecisionRefs;
	// volatile repo-state and non-ambient-visible claims never enter ambient memory).
	if !memoryAbsoluteGateAllowsOrient(claim) {
		return 0
	}
	rank := 0.0
	if entities[memoryKey(claim.Entity)] {
		rank += 2
	}
	// Semantic relevance and literal token overlap answer the same question ("is this claim about
	// the task"); take the stronger signal so a semantically-relevant claim with zero shared
	// tokens still clears the >0 cutoff and surfaces — the whole point of wiring the engine.
	if overlap := memoryTaskOverlapScore(claim, taskTokens); overlap > semanticScore {
		rank += overlap
	} else {
		rank += semanticScore
	}
	// Bounded temperature nudge (Weng item 5): reinforcement-on-access from the
	// memory-temperature projection, clamped to ±mem.MaxTemperatureContribution (< the
	// entity-match weight, so relevance always dominates). Applied ONLY to claims that are
	// already relevant (rank>0), so temperature re-ranks the relevant set and never manufactures
	// relevance for an off-task claim nor resurfaces a gated one. A gated/irrelevant claim keeps
	// its exact 0. Fail-open: a nil/empty map leaves every rank unchanged.
	if rank > 0 {
		rank += memoryTemperatureTerm(temps, claim.ID)
		rank += memoryProvenanceLaneTerm(claim.ProvenanceLane)
	}
	return rank
}

func memoryAbsoluteGateAllowsOrient(claim mem.Claim) bool {
	return claim.ClaimType != "decision" &&
		!volatileOperationalRepoStateClaim(claim) &&
		memoryVisibilityAllowedForOrient(claim)
}

// loadMemoryTemperatures reads the memory-temperature projection into the rank lookup, fail-open:
// any read/parse error (or an unbuilt projection) yields a nil map, so ranks stay byte-identical
// to the pre-temperature behavior. The projection is derived state, never authority.
func loadMemoryTemperatures(homePath string) map[string]float64 {
	temps, err := mem.LoadTemperatureLookup(homePath)
	if err != nil {
		return nil
	}
	return temps
}

// memoryTemperatureTerm returns the claim's temperature contribution to the rank, clamped to
// [-mem.MaxTemperatureContribution, +mem.MaxTemperatureContribution]. The clamp is defensive:
// the builder already bounds stored temperatures, but a hand-fed/corrupt projection can never
// breach the ceiling and let temperature overwhelm relevance. Absent id (or nil map) => 0.
func memoryTemperatureTerm(temps map[string]float64, claimID string) float64 {
	t := temps[claimID]
	if t > mem.MaxTemperatureContribution {
		return mem.MaxTemperatureContribution
	}
	if t < -mem.MaxTemperatureContribution {
		return -mem.MaxTemperatureContribution
	}
	return t
}

func memoryProvenanceLaneTerm(lane string) float64 {
	if cheapProvenanceLane(lane) {
		return cheapLaneRankPenalty
	}
	return 0
}

func cheapProvenanceLane(lane string) bool {
	lane = strings.ToLower(strings.TrimSpace(lane))
	return strings.Contains(lane, "cheap")
}

func memoryPolicyExclusionReason(claim mem.Claim, rule MemoryRule, relevanceRank float64, selfEntities map[string]bool) string {
	if relevanceRank <= 0 {
		return ""
	}
	if contains(rule.Forbidden, "self-model") && selfModelClaim(claim, selfEntities) {
		return "memory policy forbids self-model for this repo kind; relevant claim left out of active_memory"
	}
	if contains(rule.Forbidden, "personal-corpus") && personalMemoryClaim(claim, selfEntities) {
		return "memory policy forbids personal-corpus for this repo kind; relevant claim left out of active_memory"
	}
	if contains(rule.ExplicitOnly, "self-model") && selfModelClaim(claim, selfEntities) {
		return "memory policy requires explicit self-model handling; relevant claim left out of active_memory"
	}
	if contains(rule.ExplicitOnly, "personal-corpus") && personalMemoryClaim(claim, selfEntities) {
		if claim.PromotionBasis != candidates.BasisExplicitConfirmation {
			return "memory policy requires explicit personal-corpus handling; relevant claim left out of active_memory"
		}
	}
	return ""
}

func ambientMemoryRank(claim mem.Claim, rule MemoryRule, entities, taskTokens map[string]bool, semanticScore float64, temps map[string]float64, selfEntities map[string]bool) float64 {
	rank := memoryRelevanceRank(claim, entities, taskTokens, semanticScore, temps)
	if memoryPolicyExclusionReason(claim, rule, rank, selfEntities) != "" {
		return 0
	}
	return rank
}

func memoryTaskOverlapScore(claim mem.Claim, taskTokens map[string]bool) float64 {
	if len(taskTokens) == 0 {
		return 0
	}
	claimTokens := memoryTextTokens(claim.Entity + " " + claim.Predicate + " " + claim.Object)
	for k, v := range claim.Qualifiers {
		for token := range memoryTextTokens(k + " " + v) {
			claimTokens[token] = true
		}
	}
	score := 0.0
	for token := range taskTokens {
		if isMemoryWorkAddress(token) {
			if memoryClaimMentionsWork(claim, token) {
				score++
			}
			continue
		}
		if claimTokens[token] {
			score++
		}
	}
	return score
}

func volatileOperationalRepoStateClaim(claim mem.Claim) bool {
	if !strings.EqualFold(strings.TrimSpace(claim.Visibility), "operational") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(claim.ClaimType), "status") {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(claim.Entity)), "repo:") {
		return false
	}
	predicate := memoryKey(claim.Predicate)
	switch predicate {
	case "head", "aheadcount", "behindcount", "stagedcount", "unstagedcount", "untrackedcount", "changedpathcount", "sourcecontrolstatus":
		return true
	default:
		return false
	}
}

func memoryEntityKeys(in Input) map[string]bool {
	keys := map[string]bool{}
	add := func(value string) {
		key := memoryKey(value)
		if len(key) >= 3 {
			keys[key] = true
		}
	}
	add(filepath.Base(in.Cwd))
	for _, part := range strings.Fields(in.Task) {
		add(part)
	}
	return keys
}

// memoryStopwords are function words dropped from task/claim token overlap so
// ambient memory selection keys on content-bearing tokens only; short tokens
// (<3 chars) are already dropped by memoryTextTokens.
var memoryStopwords = map[string]bool{
	"about": true, "after": true, "against": true, "all": true, "also": true,
	"and": true, "any": true, "are": true, "because": true, "been": true,
	"before": true, "being": true, "between": true, "both": true, "but": true,
	"can": true, "could": true, "did": true, "does": true, "during": true,
	"each": true, "for": true, "from": true, "had": true, "has": true,
	"have": true, "her": true, "here": true, "him": true, "his": true,
	"how": true, "into": true, "its": true, "just": true, "may": true,
	"might": true, "more": true, "most": true, "must": true, "not": true,
	"off": true, "only": true, "onto": true, "other": true, "our": true,
	"out": true, "over": true, "per": true, "shall": true, "she": true,
	"should": true, "some": true, "such": true, "than": true, "that": true,
	"the": true, "their": true, "them": true, "then": true, "there": true,
	"these": true, "they": true, "this": true, "those": true, "through": true,
	"too": true, "under": true, "upon": true, "very": true, "via": true,
	"was": true, "were": true, "what": true, "when": true, "where": true,
	"which": true, "while": true, "who": true, "why": true, "will": true,
	"with": true, "within": true, "without": true, "would": true, "you": true,
	"your": true,
}

func memoryTextTokens(value string) map[string]bool {
	tokens := map[string]bool{}
	for _, part := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		key := memoryKey(part)
		if len(key) >= 3 && !memoryStopwords[key] {
			tokens[key] = true
		}
	}
	return tokens
}

func memoryKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if i := strings.LastIndex(value, ":"); i >= 0 && i+1 < len(value) {
		value = value[i+1:]
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func memoryVisibilityAllowed(visibility string) bool {
	switch strings.ToLower(strings.TrimSpace(visibility)) {
	case "", "ambient", "contextual", "operational", "world", "public":
		return true
	default:
		return false
	}
}

func memoryVisibilityAllowedForOrient(claim mem.Claim) bool {
	if strings.EqualFold(strings.TrimSpace(claim.Visibility), "private") {
		return true
	}
	return memoryVisibilityAllowed(claim.Visibility)
}

// selfModelClaim classifies claim as self-model by membership in the
// resolved self-entity set (see compound/internal/mem.ResolveSelfEntitySet:
// always "operator", extendable per-home via
// COMPOUND_HOME/config/identity.yaml) rather than one hardcoded name. A nil
// selfEntities (defensive/test callers that skip resolution) falls back to
// the bare "operator" default so classification never goes silently
// permissive.
func selfModelClaim(claim mem.Claim, selfEntities map[string]bool) bool {
	key := memoryKey(claim.Entity)
	if selfEntities == nil {
		if key == "operator" {
			return true
		}
	} else if selfEntities[key] {
		return true
	}
	return qualifierContains(claim, "domain", "self") || qualifierContains(claim, "domain", "personal")
}

func personalMemoryClaim(claim mem.Claim, selfEntities map[string]bool) bool {
	return strings.EqualFold(claim.Visibility, "private") ||
		strings.EqualFold(claim.ClaimType, "preference") ||
		selfModelClaim(claim, selfEntities) ||
		qualifierContains(claim, "source", "corpus")
}

func qualifierContains(claim mem.Claim, key, needle string) bool {
	if claim.Qualifiers == nil {
		return false
	}
	return strings.Contains(strings.ToLower(claim.Qualifiers[key]), needle)
}

func memoryLine(claim mem.Claim) string {
	return boundedSummaryText(fmt.Sprintf("%s.%s = %s", claim.Entity, claim.Predicate, claim.Object))
}

// boundedSummaryText caps one ledger-derived fragment destined for SummaryMD. SummaryMD is
// the ONE injected surface with no budget of its own — every other injected section is capped
// by renderInjection — and it is appended to per hook/decision/memory/position/probe group, so
// an uncapped fragment makes the session payload grow with the ledger. The ellipsis is the
// signal that the reader is holding a fragment, not the claim: the full body stays resolvable
// through the ref that accompanies it in active_memory / active_positions.
func boundedSummaryText(text string) string {
	const max = 180
	if len(text) <= max {
		return text
	}
	return strings.TrimSpace(text[:max-3]) + "..."
}

func loadDecisionRefsFromView(in Input, view *mem.CurrentView, productFence productRecallFence) ([]string, []envelope.SourceRev, []envelope.ExcludedRef, error) {
	byRef := decisionstate.ActiveByRef(view.Claims())
	refs := make([]string, 0, len(byRef))
	for ref := range byRef {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	type rankedDecision struct {
		ref   string
		claim mem.Claim
		score float64
	}

	relevant := make([]rankedDecision, 0, len(refs))
	var excluded []envelope.ExcludedRef
	taskTokens := memoryTextTokens(in.Task)
	for _, ref := range refs {
		claim := byRef[ref]
		if !productRecallFenceAllowsClaim(productFence, claim) {
			excluded = append(excluded, envelope.ExcludedRef{
				Ref:    servedProductFenceExclusionRef,
				Reason: servedProductFenceExclusionReason,
			})
			continue
		}
		score := decisionTaskOverlapScore(claim, in.Task, taskTokens, ref)
		if score <= 0 {
			excluded = append(excluded, envelope.ExcludedRef{
				Ref:    ref,
				Reason: "accepted decision from Compound ledger omitted: no task relevance",
			})
			continue
		}
		relevant = append(relevant, rankedDecision{
			ref:   ref,
			claim: claim,
			score: score,
		})
	}
	sort.Slice(relevant, func(i, j int) bool {
		if relevant[i].score != relevant[j].score {
			return relevant[i].score > relevant[j].score
		}
		return relevant[i].ref < relevant[j].ref
	})

	selected := relevant
	if len(selected) > maxContextDecisionRefs {
		selected = selected[:maxContextDecisionRefs]
		for _, decision := range relevant[maxContextDecisionRefs:] {
			excluded = append(excluded, envelope.ExcludedRef{
				Ref:    decision.ref,
				Reason: fmt.Sprintf("accepted decision from Compound ledger omitted: relevant but outside %d-ref task context budget", maxContextDecisionRefs),
			})
		}
	}

	active := make([]string, 0, len(selected))
	revs := make([]envelope.SourceRev, 0, len(selected))
	for _, decision := range selected {
		active = append(active, decision.ref)
		revs = append(revs, envelope.SourceRev{Source: decision.ref, Rev: "claim:" + decision.claim.ID})
	}
	return active, revs, excluded, nil
}

// decisionTaskOverlapScore ranks only meaning-bearing decision text against the task.
// Structural ledger vocabulary (decision, decided, lifecycle_status, accepted) is shared by
// every decision and would manufacture relevance for an arbitrary alphabetical slice. An exact
// canonical id mention receives priority without scoring shared identity fragments.
func decisionTaskOverlapScore(claim mem.Claim, task string, taskTokens map[string]bool, canonicalRef string) float64 {
	decisionID := strings.TrimPrefix(canonicalRef, decisionstate.RefPrefix)
	exactIdentity := taskMentionsDecisionIdentity(task, decisionID)
	decisionTokens := memoryTextTokens(claim.Object)
	addTokens := func(value string) {
		for token := range memoryTextTokens(value) {
			decisionTokens[token] = true
		}
	}
	if memoryKey(claim.Predicate) != "decided" {
		addTokens(claim.Predicate)
	}
	if claim.Qualifiers != nil {
		for _, key := range []string{"domain", "product", "product_id", "project", "project_id", "scope", "tags", "topic"} {
			addTokens(claim.Qualifiers[key])
		}
	}

	score := 0.0
	for token := range taskTokens {
		if decisionTokens[token] {
			score++
		}
	}
	if exactIdentity {
		// Exact identity always outranks lexical relevance without giving shared id
		// fragments (DEC, brain, claim) relevance of their own.
		score += float64(len(taskTokens) + 1)
	}
	return score
}

func taskMentionsDecisionIdentity(task, decisionID string) bool {
	return textMentionsIdentity(task, decisionID)
}

func textMentionsIdentity(task, decisionID string) bool {
	task = strings.ToLower(task)
	decisionID = strings.ToLower(strings.TrimSpace(decisionID))
	if task == "" || decisionID == "" {
		return false
	}
	for offset := 0; offset <= len(task)-len(decisionID); {
		relative := strings.Index(task[offset:], decisionID)
		if relative < 0 {
			return false
		}
		start := offset + relative
		end := start + len(decisionID)
		beforeBoundary := start == 0 || !decisionIdentityByte(task[start-1])
		afterBoundary := end == len(task) || !decisionIdentityByte(task[end])
		if beforeBoundary && afterBoundary {
			return true
		}
		offset = start + 1
	}
	return false
}

func decisionIdentityByte(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= '0' && value <= '9') || value == '_' || value == '-'
}

type codexHooksFile struct {
	Hooks map[string][]codexHookGroup `json:"hooks"`
}

type codexHookGroup struct {
	Matcher string             `json:"matcher"`
	Hooks   []codexHookCommand `json:"hooks"`
}

type codexHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

func InstalledHooksPath(homePath string) string {
	return filepath.Join(homePath, "config", "installed-hooks.yaml")
}

func InstalledHooksSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", installedHooksSourceAuthorityFile)
}

func LoadInstalledHooksManifest(homePath string) (InstalledHooksManifest, error) {
	return loadInstalledHooksManifest(InstalledHooksPath(homePath))
}

func LoadInstalledHooksSourceAuthority(homePath string) ([]InstalledHooksSourceAuthority, error) {
	data, err := os.ReadFile(InstalledHooksSourceAuthorityPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	byID := map[string]InstalledHooksSourceAuthority{}
	var order []string
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec InstalledHooksSourceAuthority
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse installed hooks source authority line %d: %w", i+1, err)
		}
		if strings.TrimSpace(rec.ID) == "" {
			return nil, fmt.Errorf("installed hooks source authority line %d missing id", i+1)
		}
		if _, seen := byID[rec.ID]; !seen {
			order = append(order, rec.ID)
		}
		byID[rec.ID] = rec
	}
	records := make([]InstalledHooksSourceAuthority, 0, len(order))
	for _, id := range order {
		records = append(records, byID[id])
	}
	return records, nil
}

type InstalledHooksSourceAuthoritySummary struct {
	SourceRoleCounts            map[string]int `json:"source_role_counts,omitempty"`
	RetiredSourceAuthorityCount int            `json:"retired_source_authority_count"`
	RetiredSourceAuthorityRefs  []string       `json:"retired_source_authority_refs,omitempty"`
}

func SummarizeInstalledHooksSourceAuthority(records []InstalledHooksSourceAuthority) InstalledHooksSourceAuthoritySummary {
	out := InstalledHooksSourceAuthoritySummary{SourceRoleCounts: map[string]int{}}
	retired := map[string]bool{}
	for _, rec := range records {
		role := strings.TrimSpace(rec.SourceRole)
		if role == "" {
			role = "unknown"
		}
		out.SourceRoleCounts[role]++
		if ref := strings.TrimSpace(rec.Retires); ref != "" && !retired[ref] {
			retired[ref] = true
			out.RetiredSourceAuthorityRefs = append(out.RetiredSourceAuthorityRefs, ref)
		}
	}
	sort.Strings(out.RetiredSourceAuthorityRefs)
	out.RetiredSourceAuthorityCount = len(out.RetiredSourceAuthorityRefs)
	return out
}

func ImportCodexHooksJSON(homePath, sourcePath string, now time.Time) (InstalledHooksImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return InstalledHooksImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(sourcePath) == "" {
		return InstalledHooksImportReport{}, fmt.Errorf("source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return InstalledHooksImportReport{}, err
	}
	manifest, err := parseCodexHooksJSON(data)
	if err != nil {
		return InstalledHooksImportReport{}, err
	}
	attestInstalledHookHandlers(&manifest)
	return importInstalledHooks(homePath, sourcePath, data, manifest, installedHooksCodexImportedVia, now)
}

func ImportHookRegistrationTS(homePath, sourcePath string, now time.Time) (InstalledHooksImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return InstalledHooksImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(sourcePath) == "" {
		return InstalledHooksImportReport{}, fmt.Errorf("source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return InstalledHooksImportReport{}, err
	}
	manifest, err := parseHookRegistrationTS(data)
	if err != nil {
		return InstalledHooksImportReport{}, err
	}
	attestInstalledHookHandlers(&manifest)
	return importInstalledHooks(homePath, sourcePath, data, manifest, installedHooksRegistryImportedVia, now)
}

func attestInstalledHookHandlers(manifest *InstalledHooksManifest) {
	attestInstalledHookHandlersAtHome(manifest, "")
}

func attestInstalledHookHandlersAtHome(manifest *InstalledHooksManifest, harnessHome string) {
	if manifest == nil {
		return
	}
	for i := range manifest.Hooks {
		revision := installedHookHandlerRevision(manifest.Hooks[i].Ref, harnessHome)
		if revision == "" && concreteInstalledHookHandlerRef(manifest.Hooks[i].Ref) {
			revision = installedHookHandlerUnavailableRevision
		}
		manifest.Hooks[i].Revision = revision
	}
}

func concreteInstalledHookHandlerRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	return ref != "" && !strings.HasPrefix(ref, "handler:") && !strings.HasPrefix(ref, "hook://")
}

func importInstalledHooks(homePath, sourcePath string, data []byte, manifest InstalledHooksManifest, importedVia string, now time.Time) (InstalledHooksImportReport, error) {
	if err := os.MkdirAll(filepath.Join(homePath, "config"), 0o755); err != nil {
		return InstalledHooksImportReport{}, err
	}
	incomingHookCount := len(manifest.Hooks)
	authorityPath := InstalledHooksPath(homePath)
	priorAuthorityRevision := ""
	if authorityData, err := os.ReadFile(authorityPath); err == nil {
		existing, err := parseInstalledHooksManifest(authorityPath, authorityData)
		if err != nil {
			return InstalledHooksImportReport{}, err
		}
		priorAuthorityRevision = hashBytes(authorityData)
		manifest = mergeInstalledHooksManifest(existing, manifest)
	} else if !os.IsNotExist(err) {
		return InstalledHooksImportReport{}, err
	}
	manifest.SchemaVersion = installedHooksSchemaVersion
	manifestData, err := yaml.Marshal(manifest)
	if err != nil {
		return InstalledHooksImportReport{}, err
	}
	authorityRevision := hashBytes(manifestData)
	if err := os.WriteFile(authorityPath, manifestData, 0o644); err != nil {
		return InstalledHooksImportReport{}, err
	}
	sourceRevision := hashBytes(data)
	evidenceRefs := []string{sourceRevision, InstalledHooksAuthorityRef, "authority_revision:" + authorityRevision}
	if priorAuthorityRevision != "" {
		evidenceRefs = append(evidenceRefs, "prior_authority_revision:"+priorAuthorityRevision)
	}
	rec := InstalledHooksSourceAuthority{
		SchemaVersion:          InstalledHooksSourceAuthoritySchema,
		ID:                     installedHooksSourceAuthorityID(sourcePath, sourceRevision, importedVia, authorityRevision),
		CreatedAt:              now.UTC().Format(time.RFC3339),
		SourcePath:             sourcePath,
		SourceRevision:         sourceRevision,
		ImportedVia:            importedVia,
		AuthorityRef:           InstalledHooksAuthorityRef,
		AuthorityRevision:      authorityRevision,
		PriorAuthorityRevision: priorAuthorityRevision,
		AuthorityStatus:        "compound_primary",
		SourceRole:             "evidence_only_input_after_import",
		Retires:                "source_view_authority:" + sourcePath,
		RetirementCondition:    "EffectiveContract reads compound://config/installed-hooks from COMPOUND_HOME; source view remains rollback/evidence input",
		Rollback:               "re-import source hooks JSON or remove COMPOUND_HOME config/installed-hooks.yaml",
		Harnesses:              installedHookHarnesses(manifest),
		IncomingHookCount:      incomingHookCount,
		HookCount:              len(manifest.Hooks),
		EvidenceRefs:           evidenceRefs,
	}
	if err := appendInstalledHooksSourceAuthority(homePath, rec); err != nil {
		return InstalledHooksImportReport{}, err
	}
	return InstalledHooksImportReport{
		AuthorityRef:           InstalledHooksAuthorityRef,
		AuthorityPath:          authorityPath,
		SourceAuthorityID:      rec.ID,
		SourceAuthority:        rec,
		IncomingHookCount:      incomingHookCount,
		HookCount:              len(manifest.Hooks),
		AuthorityRevision:      authorityRevision,
		PriorAuthorityRevision: priorAuthorityRevision,
		Harnesses:              rec.Harnesses,
		Manifest:               manifest,
	}, nil
}

// mergeInstalledHooksManifest treats an import as authoritative for managed
// rows belonging to harnesses represented by that import. Managed rows for
// other harnesses and preserved external declarations survive until a source
// for their harness replaces them or an incoming row with the same stable
// identity replaces them explicitly.
func mergeInstalledHooksManifest(existing, incoming InstalledHooksManifest) InstalledHooksManifest {
	byIdentity := map[string]InstalledHook{}
	incomingHarnesses := map[string]bool{}
	for _, hook := range incoming.Hooks {
		incomingHarnesses[strings.TrimSpace(hook.Harness)] = true
		byIdentity[installedHookStableIdentity(hook)] = hook
	}
	for _, hook := range existing.Hooks {
		identity := installedHookStableIdentity(hook)
		if _, replaced := byIdentity[identity]; replaced {
			continue
		}
		if strings.TrimSpace(hook.Status) == "preserved_external" || !incomingHarnesses[strings.TrimSpace(hook.Harness)] {
			byIdentity[identity] = hook
		}
	}
	identities := make([]string, 0, len(byIdentity))
	for identity := range byIdentity {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	merged := InstalledHooksManifest{SchemaVersion: installedHooksSchemaVersion, Hooks: make([]InstalledHook, 0, len(identities))}
	for _, identity := range identities {
		merged.Hooks = append(merged.Hooks, byIdentity[identity])
	}
	return merged
}

func installedHookStableIdentity(hook InstalledHook) string {
	event := firstNonEmpty(strings.TrimSpace(hook.Event), strings.TrimSpace(hook.Name))
	return strings.Join([]string{
		strings.TrimSpace(hook.Harness),
		event,
		strings.TrimSpace(hook.Matcher),
		strings.TrimSpace(hook.Ref),
	}, "\x00")
}

func installedHookCurrentStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "", "current", "active", "non_blocking":
		return true
	default:
		return false
	}
}

func parseHookRegistrationTS(data []byte) (InstalledHooksManifest, error) {
	body, err := hookRegistrationArrayBody(string(data))
	if err != nil {
		return InstalledHooksManifest{}, err
	}
	objects, err := splitTopLevelObjects(body)
	if err != nil {
		return InstalledHooksManifest{}, err
	}
	manifest := InstalledHooksManifest{SchemaVersion: installedHooksSchemaVersion}
	seen := map[string]bool{}
	for i, obj := range objects {
		rawHarness, ok := tsStringField(obj, "harness")
		if !ok {
			return InstalledHooksManifest{}, fmt.Errorf("hook registration %d missing harness", i+1)
		}
		harness := normalizeInstalledHookHarness(rawHarness)
		event, ok := tsStringField(obj, "event")
		if !ok {
			return InstalledHooksManifest{}, fmt.Errorf("hook registration %d missing event", i+1)
		}
		file, ok := tsStringField(obj, "file")
		if !ok {
			return InstalledHooksManifest{}, fmt.Errorf("hook registration %d missing file", i+1)
		}
		command, ok := tsStringField(obj, "command")
		if !ok {
			return InstalledHooksManifest{}, fmt.Errorf("hook registration %d missing command", i+1)
		}
		matcher, _ := tsStringField(obj, "matcher")
		status, _ := tsStringField(obj, "status")
		status = installedHookStatusForCommand(event, command, status)
		ref := expandHomeRef(hookRefFromCommand(command))
		if ref == "" {
			ref = hookRefFromHarnessFile(rawHarness, file)
		}
		key := strings.Join([]string{harness, event, matcher, ref}, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		manifest.Hooks = append(manifest.Hooks, InstalledHook{
			Harness:            harness,
			Name:               event,
			Event:              event,
			Matcher:            matcher,
			Ref:                ref,
			InvocationRevision: canonicalHookInvocationRevision(command, ""),
			Status:             status,
		})
	}
	if len(manifest.Hooks) == 0 {
		return InstalledHooksManifest{}, fmt.Errorf("hook-registration ts has no hook registrations")
	}
	sort.Slice(manifest.Hooks, func(i, j int) bool {
		a, b := manifest.Hooks[i], manifest.Hooks[j]
		return strings.Join([]string{a.Harness, a.Name, a.Ref}, "\x00") < strings.Join([]string{b.Harness, b.Name, b.Ref}, "\x00")
	})
	return manifest, nil
}

func normalizeInstalledHookHarness(harness string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(harness), "-", "_")
	switch normalized {
	case "claude":
		return "claude_code"
	default:
		return normalized
	}
}

func hookRegistrationArrayBody(text string) (string, error) {
	idx := strings.Index(text, "HOOK_REGISTRATIONS")
	if idx == -1 {
		return "", fmt.Errorf("HOOK_REGISTRATIONS not found")
	}
	eq := strings.Index(text[idx:], "=")
	if eq == -1 {
		return "", fmt.Errorf("HOOK_REGISTRATIONS assignment not found")
	}
	eq += idx
	start := strings.Index(text[eq:], "[")
	if start == -1 {
		return "", fmt.Errorf("HOOK_REGISTRATIONS array not found")
	}
	start += eq
	end, err := findMatchingDelimited(text, start, '[', ']')
	if err != nil {
		return "", err
	}
	return text[start+1 : end], nil
}

func splitTopLevelObjects(body string) ([]string, error) {
	var objects []string
	for i := 0; i < len(body); i++ {
		if body[i] != '{' {
			continue
		}
		end, err := findMatchingDelimited(body, i, '{', '}')
		if err != nil {
			return nil, err
		}
		objects = append(objects, body[i+1:end])
		i = end
	}
	return objects, nil
}

func findMatchingDelimited(text string, start int, open, close byte) (int, error) {
	if start < 0 || start >= len(text) || text[start] != open {
		return -1, fmt.Errorf("delimiter %q not found", open)
	}
	depth := 0
	var quote byte
	escaped := false
	lineComment := false
	blockComment := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if lineComment {
			if c == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if c == '*' && i+1 < len(text) && text[i+1] == '/' {
				blockComment = false
				i++
			}
			continue
		}
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '/' && i+1 < len(text) {
			switch text[i+1] {
			case '/':
				lineComment = true
				i++
				continue
			case '*':
				blockComment = true
				i++
				continue
			}
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return -1, fmt.Errorf("no matching %q found", close)
}

func tsStringField(obj, key string) (string, bool) {
	idx := strings.Index(obj, key+":")
	if idx == -1 {
		return "", false
	}
	rest := strings.TrimSpace(obj[idx+len(key)+1:])
	if rest == "" {
		return "", false
	}
	quote := rest[0]
	if quote != '\'' && quote != '"' && quote != '`' {
		return "", false
	}
	var b strings.Builder
	escaped := false
	for i := 1; i < len(rest); i++ {
		c := rest[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == quote {
			return strings.TrimSpace(b.String()), true
		}
		b.WriteByte(c)
	}
	return "", false
}

func hookRefFromHarnessFile(harness, file string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch harness {
	case "claude":
		return filepath.Join(home, ".claude", "hooks", file)
	case "codex":
		return filepath.Join(home, ".codex", "hooks", file)
	case "opencode":
		return filepath.Join(home, ".config", "opencode", "plugins", file)
	default:
		return ""
	}
}

func parseCodexHooksJSON(data []byte) (InstalledHooksManifest, error) {
	var src codexHooksFile
	if err := json.Unmarshal(data, &src); err != nil {
		return InstalledHooksManifest{}, fmt.Errorf("parse codex hooks json: %w", err)
	}
	events := make([]string, 0, len(src.Hooks))
	for event := range src.Hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	manifest := InstalledHooksManifest{SchemaVersion: installedHooksSchemaVersion}
	seen := map[string]bool{}
	for _, event := range events {
		for _, group := range src.Hooks[event] {
			for _, hook := range group.Hooks {
				ref := expandHomeRef(hookRefFromCommand(hook.Command))
				if ref == "" {
					continue
				}
				matcher := strings.TrimSpace(group.Matcher)
				key := strings.Join([]string{event, matcher, ref}, "\x00")
				if seen[key] {
					continue
				}
				seen[key] = true
				manifest.Hooks = append(manifest.Hooks, InstalledHook{
					Harness:            "codex",
					Name:               event,
					Event:              event,
					Matcher:            matcher,
					Ref:                ref,
					InvocationRevision: canonicalHookInvocationRevision(hook.Command, ""),
					Status:             installedHookStatusForCommand(event, hook.Command, ""),
				})
			}
		}
	}
	preserveExternalCodexHookPeers(&manifest)
	if len(manifest.Hooks) == 0 {
		return InstalledHooksManifest{}, fmt.Errorf("codex hooks json has no command hooks")
	}
	return manifest, nil
}

// preserveExternalCodexHookPeers keeps one managed owner per lifecycle slot. Codex permits multiple
// commands for an event, so an existing context hook can legitimately share SessionStart with
// Compound's compiled posture. The Compound command owns that managed slot; peer commands remain
// observable rollback/evidence inputs without becoming duplicate desired HookPlan bindings.
func preserveExternalCodexHookPeers(manifest *InstalledHooksManifest) {
	compoundOwnedSlots := map[string]bool{}
	for _, hook := range manifest.Hooks {
		if codexHookIsCompoundOwned(hook) {
			compoundOwnedSlots[codexHookSlotIdentity(hook)] = true
		}
	}
	for i := range manifest.Hooks {
		hook := &manifest.Hooks[i]
		if compoundOwnedSlots[codexHookSlotIdentity(*hook)] && !codexHookIsCompoundOwned(*hook) {
			hook.Status = "preserved_external"
		}
	}
}

func codexHookSlotIdentity(hook InstalledHook) string {
	event := firstNonEmpty(strings.TrimSpace(hook.Event), strings.TrimSpace(hook.Name))
	return strings.Join([]string{strings.TrimSpace(hook.Harness), event, strings.TrimSpace(hook.Matcher)}, "\x00")
}

func codexHookIsCompoundOwned(hook InstalledHook) bool {
	ref := filepath.ToSlash(strings.TrimSpace(hook.Ref))
	return ref == "compound" || strings.HasSuffix(ref, "/.local/bin/compound")
}

func hookRefFromCommand(command string) string {
	for _, field := range strings.Fields(command) {
		field = strings.Trim(field, `"'`)
		if strings.Contains(field, ".hook.") || strings.HasSuffix(field, ".hook.ts") {
			return field
		}
	}
	for _, field := range strings.Fields(command) {
		field = strings.Trim(field, `"'`)
		if field == "compound" || strings.HasSuffix(field, "/.local/bin/compound") {
			return canonicalHookHandlerRef(command, "")
		}
	}
	return ""
}

func installedHookStatusForCommand(event, command, explicitStatus string) string {
	if status := strings.TrimSpace(explicitStatus); status != "" {
		return status
	}
	if canonicalHookEvent(event) == "PreToolUse" &&
		compoundLivePreToolUseCommand(command) &&
		!hookCommandHasExactField(command, "--enforce") {
		return "non_blocking"
	}
	return "current"
}

func compoundLivePreToolUseCommand(command string) bool {
	fields := canonicalHookCommandFields(command)
	if len(fields) < 3 {
		return false
	}
	// Managed registrations execute Compound directly. Treat wrappers and
	// launchers conservatively because Compound-shaped arguments do not prove
	// which program owns the hook's blocking behavior.
	executable := filepath.ToSlash(strings.TrimSpace(fields[0]))
	if executable != "compound" && !strings.HasSuffix(executable, "/compound") {
		return false
	}
	if fields[1] != "hook" || fields[2] != "pre-tool-use" {
		return false
	}
	for _, field := range fields[3:] {
		if field == "--live" {
			return true
		}
	}
	return false
}

func hookCommandHasExactField(command, want string) bool {
	for _, field := range canonicalHookCommandFields(command) {
		if field == want {
			return true
		}
	}
	return false
}

func expandHomeRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ref
	}
	ref = strings.ReplaceAll(ref, "${HOME}", home)
	ref = strings.ReplaceAll(ref, "$HOME", home)
	if strings.HasPrefix(ref, "~/") {
		ref = filepath.Join(home, strings.TrimPrefix(ref, "~/"))
	}
	return ref
}

func loadInstalledHooksManifest(path string) (InstalledHooksManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return InstalledHooksManifest{}, err
	}
	return parseInstalledHooksManifest(path, data)
}

func parseInstalledHooksManifest(path string, data []byte) (InstalledHooksManifest, error) {
	var manifest InstalledHooksManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return InstalledHooksManifest{}, fmt.Errorf("parse installed hooks %s: %w", path, err)
	}
	if manifest.SchemaVersion != installedHooksSchemaVersion {
		return InstalledHooksManifest{}, fmt.Errorf("installed hooks schema_version must be %s, got %q", installedHooksSchemaVersion, manifest.SchemaVersion)
	}
	return manifest, nil
}

func appendInstalledHooksSourceAuthority(homePath string, rec InstalledHooksSourceAuthority) error {
	records, err := LoadInstalledHooksSourceAuthority(homePath)
	if err != nil {
		return err
	}
	for _, existing := range records {
		if existing.ID == rec.ID {
			return nil
		}
	}
	path := InstalledHooksSourceAuthorityPath(homePath)
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

func installedHookHarnesses(manifest InstalledHooksManifest) []string {
	seen := map[string]bool{}
	for _, hook := range manifest.Hooks {
		h := strings.TrimSpace(hook.Harness)
		if h != "" {
			seen[h] = true
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func installedHooksSourceAuthorityID(path, revision, importedVia, authorityRevision string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{path, revision, importedVia, authorityRevision}, "|")))
	return "hksrc_" + hex.EncodeToString(sum[:])[:12]
}

func hookReadinessForInput(homePath string, in Input) ([]envelope.HookReadiness, error) {
	if !writeMutationTask(in) && !hookReadinessTask(in) {
		return nil, nil
	}
	path := InstalledHooksPath(homePath)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = filepath.Join(homePath, "config", "installed-hooks.json")
	}
	manifest, err := loadInstalledHooksManifest(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows := InstalledHookGateReadiness(manifest, nativePreToolUseBlockMap(harness.DefaultRegistry()))
	out := make([]envelope.HookReadiness, 0, len(rows))
	requestedHarness := strings.TrimSpace(in.Harness)
	for _, row := range rows {
		if requestedHarness != "" && row.Harness != requestedHarness {
			continue
		}
		out = append(out, envelope.HookReadiness{
			Harness:         row.Harness,
			Hook:            row.Hook,
			NativeBlock:     row.NativeBlock,
			Installed:       row.Installed,
			InstalledRef:    row.InstalledRef,
			Status:          row.Status,
			AdapterRequired: row.AdapterRequired,
			AdapterCommand:  row.AdapterCommand,
			Reason:          row.Reason,
			Actions:         envelopeHookReadinessActions(row.Actions),
		})
	}
	return out, nil
}

func envelopeHookReadinessActions(actions []HookGateAction) []envelope.HookReadinessAction {
	if len(actions) == 0 {
		return nil
	}
	out := make([]envelope.HookReadinessAction, 0, len(actions))
	for _, action := range actions {
		out = append(out, envelope.HookReadinessAction{
			Kind:             action.Kind,
			Label:            action.Label,
			Command:          action.Command,
			ExpectedExitCode: action.ExpectedExitCode,
			ExpectedStatus:   action.ExpectedStatus,
			Boundary:         action.Boundary,
			Proof:            action.Proof,
			Rollback:         action.Rollback,
			RequiresOperator: action.RequiresOperator,
		})
	}
	return out
}

func requiredChecksForHookReadiness(readiness []envelope.HookReadiness, in Input) []string {
	if !writeMutationTask(in) {
		return nil
	}
	checks := []string{}
	for _, row := range readiness {
		status := strings.TrimSpace(row.Status)
		if status == "handler_unavailable" {
			harness := firstNonEmpty(strings.TrimSpace(row.Harness), strings.TrimSpace(in.Harness), "unknown")
			hook := firstNonEmpty(strings.TrimSpace(row.Hook), "PreToolUse")
			checks = append(checks, fmt.Sprintf("pre_write_gate %s/%s status=handler_unavailable: restore a readable non-symlink concrete handler, re-import installed-hook authority, then run `compound contract hooks-authority` before mutation", harness, hook))
			continue
		}
		if !row.AdapterRequired || strings.TrimSpace(row.AdapterCommand) == "" {
			continue
		}
		if status != "missing_gate" && status != "wrapper_required" {
			continue
		}
		harness := firstNonEmpty(strings.TrimSpace(row.Harness), strings.TrimSpace(in.Harness), "unknown")
		hook := firstNonEmpty(strings.TrimSpace(row.Hook), "PreToolUse")
		if proof := hookReadinessAction(row.Actions, "prove_fixture_gate"); proof != nil && strings.TrimSpace(proof.Command) != "" {
			expected := ""
			if proof.ExpectedExitCode != 0 {
				expected = fmt.Sprintf(" expected_exit_code=%d", proof.ExpectedExitCode)
			}
			checks = append(checks, fmt.Sprintf("pre_write_gate %s/%s status=%s: run %s%s before mutation", harness, hook, status, strings.TrimSpace(proof.Command), expected))
			continue
		}
		checks = append(checks, fmt.Sprintf("pre_write_gate %s/%s status=%s: run %s before mutation", harness, hook, status, strings.TrimSpace(row.AdapterCommand)))
	}
	sort.Strings(checks)
	return checks
}

func hookReadinessAction(actions []envelope.HookReadinessAction, kind string) *envelope.HookReadinessAction {
	for i := range actions {
		if actions[i].Kind == kind {
			return &actions[i]
		}
	}
	return nil
}

func hookReadinessTask(in Input) bool {
	text := strings.ToLower(strings.Join([]string{in.Task, in.Mode}, " "))
	for _, sig := range []string{"hook", "pretooluse", "pre-tool-use", "write gate", "write-gate", "placementguard"} {
		if strings.Contains(text, sig) {
			return true
		}
	}
	return false
}

func nativePreToolUseBlockMap(reg harness.Registry) map[string]bool {
	out := map[string]bool{}
	for name, h := range reg.Harnesses {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out[name] = h.Capabilities.PreToolUseBlock
	}
	return out
}

func loadInstalledHooksForInput(homePath string, in Input) ([]string, []envelope.ExcludedRef, []envelope.HookRosterEntry, []envelope.SourceRev, error) {
	path := InstalledHooksPath(homePath)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = filepath.Join(homePath, "config", "installed-hooks.json")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []string{}, []envelope.ExcludedRef{}, []envelope.HookRosterEntry{}, []envelope.SourceRev{}, nil
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}
	manifest, err := loadInstalledHooksManifest(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	active := []string{}
	excluded := []envelope.ExcludedRef{}
	roster := []envelope.HookRosterEntry{}
	writeScopedHarnesses := writeScopedHookHarnesses(manifest)
	hasWriteScopedHook := false
	hasUnavailableWriteScopedHook := false
	revs := []envelope.SourceRev{{Source: InstalledHooksAuthorityRef, Rev: hashBytes(data)}}
	for _, hook := range manifest.Hooks {
		if strings.TrimSpace(hook.Name) == "" {
			continue
		}
		if !installedHookCurrentStatus(hook.Status) {
			continue
		}
		if hook.Harness != "" && in.Harness != "" && hook.Harness != in.Harness {
			continue
		}
		h := firstNonEmpty(hook.Harness, in.Harness, "unknown")
		ref := "hook://" + h + "/" + hook.Name
		if installedHookHandlerUnavailable(hook) {
			reason := "installed concrete handler content is unavailable or symlinked; hook excluded until handler content is restored and installed-hook authority is re-imported"
			excluded = append(excluded, envelope.ExcludedRef{Ref: ref, Reason: reason})
			roster = append(roster, hookRosterEntry(hook, h, ref, "handler_unavailable", reason))
			if writeScopedHook(hook) {
				hasUnavailableWriteScopedHook = true
			}
			continue
		}
		if writeScopedHook(hook) {
			hasWriteScopedHook = true
		}
		if installedHookExcludedForTask(hook, in) {
			reason := "write-scoped hook excluded from read-only task context; installed hook remains Compound authority/fallback"
			excluded = append(excluded, envelope.ExcludedRef{
				Ref:    ref,
				Reason: reason,
			})
			roster = append(roster, hookRosterEntry(hook, h, ref, "excluded_read_only", reason))
			continue
		}
		active = append(active, ref)
		roster = append(roster, hookRosterEntry(hook, h, ref, "active", installedHookActiveReason(hook, in)))
		rev := strings.TrimSpace(hook.Revision)
		if rev == "" && strings.TrimSpace(hook.Ref) != "" {
			rev = revisionForPath(hook.Ref)
		}
		if rev == "" {
			rev = "unknown"
		}
		revs = append(revs, envelope.SourceRev{Source: ref, Rev: rev})
	}
	if writeMutationTask(in) && strings.TrimSpace(in.Harness) != "" && !hasWriteScopedHook && !hasUnavailableWriteScopedHook {
		excluded = append(excluded, envelope.ExcludedRef{
			Ref:    "hook://" + in.Harness + "/PreToolUse",
			Reason: missingWriteGateReason(in.Harness, writeScopedHarnesses),
		})
	}
	sort.Strings(active)
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].Ref < excluded[j].Ref })
	sort.Slice(roster, func(i, j int) bool {
		if roster[i].Ref == roster[j].Ref {
			return roster[i].Status < roster[j].Status
		}
		return roster[i].Ref < roster[j].Ref
	})
	sort.Slice(revs, func(i, j int) bool { return revs[i].Source < revs[j].Source })
	return active, excluded, roster, revs, nil
}

func hookRosterEntry(hook InstalledHook, harness, ref, status, reason string) envelope.HookRosterEntry {
	return envelope.HookRosterEntry{
		Ref:     ref,
		Harness: harness,
		Hook:    strings.TrimSpace(hook.Name),
		Event:   firstNonEmpty(strings.TrimSpace(hook.Event), strings.TrimSpace(hook.Name)),
		Matcher: strings.TrimSpace(hook.Matcher),
		Status:  status,
		Reason:  reason,
	}
}

func installedHookActiveReason(hook InstalledHook, in Input) string {
	if strings.TrimSpace(hook.Status) == "non_blocking" {
		return "installed non-blocking hook selected for harness and task"
	}
	if writeScopedHook(hook) && writeMutationTask(in) {
		return "write-scoped hook selected for mutation task"
	}
	return "installed hook selected for harness and task"
}

func writeScopedHookHarnesses(manifest InstalledHooksManifest) []string {
	seen := map[string]bool{}
	for _, hook := range manifest.Hooks {
		if strings.TrimSpace(hook.Name) == "" {
			continue
		}
		if !installedHookCurrentStatus(hook.Status) {
			continue
		}
		if !writeScopedHook(hook) {
			continue
		}
		if installedHookHandlerUnavailable(hook) {
			continue
		}
		seen[firstNonEmpty(hook.Harness, "unknown")] = true
	}
	out := make([]string, 0, len(seen))
	for harness := range seen {
		out = append(out, harness)
	}
	sort.Strings(out)
	return out
}

func InstalledHookGateReadiness(manifest InstalledHooksManifest, nativePreToolUseBlock map[string]bool) []HookGateReadiness {
	harnesses := map[string]bool{}
	for _, hook := range manifest.Hooks {
		if !installedHookCurrentStatus(hook.Status) {
			continue
		}
		harness := strings.TrimSpace(hook.Harness)
		if harness != "" {
			harnesses[harness] = true
		}
	}
	for harness := range nativePreToolUseBlock {
		harness = strings.TrimSpace(harness)
		if harness != "" {
			harnesses[harness] = true
		}
	}
	names := make([]string, 0, len(harnesses))
	for harness := range harnesses {
		names = append(names, harness)
	}
	sort.Strings(names)

	out := make([]HookGateReadiness, 0, len(names))
	for _, harness := range names {
		installed, unavailable, ref := installedWriteGateForHarness(manifest, harness)
		nativeBlock := nativePreToolUseBlock[harness]
		nonBlockingLane := installedHooksExplicitlyNonBlocking(manifest, harness) ||
			installedHooksExplicitlyObservationOnlyAdapter(manifest, harness)
		row := HookGateReadiness{
			Harness:     harness,
			Hook:        "PreToolUse",
			NativeBlock: nativeBlock,
			Installed:   installed,
		}
		if unavailable {
			row.InstalledRef = ref
		} else if installed {
			row.InstalledRef = ref
		}
		switch {
		case unavailable:
			row.Status = "handler_unavailable"
			row.Reason = "installed write-gate declaration names concrete handler content that is missing, unreadable, or symlinked"
		case nonBlockingLane:
			row.Status = "non_blocking"
			row.Reason = "harness is registered for context injection and observation only; no blocking PreToolUse gate is registered"
		case installed && nativeBlock:
			row.Status = "native_blocking"
			row.Reason = "installed write gate can block in the harness"
		case installed:
			row.Status = "wrapper_required"
			row.AdapterRequired = true
			row.AdapterCommand = "compound hook pre-tool-use --enforce FIXTURE_JSON"
			row.Reason = "installed write gate is fail-open in this harness; wrapper adapter must enforce the CLI exit code"
		case nativeBlock:
			row.Status = "not_installed"
			row.Reason = "harness can block, but no installed write-scoped PreToolUse hook is registered"
		default:
			row.Status = "missing_gate"
			row.AdapterRequired = true
			row.AdapterCommand = "compound hook pre-tool-use --enforce FIXTURE_JSON"
			row.Reason = "no installed write-scoped PreToolUse hook and harness cannot block natively"
		}
		row.Actions = hookGateActions(row)
		out = append(out, row)
	}
	return out
}

func installedHooksExplicitlyNonBlocking(manifest InstalledHooksManifest, harness string) bool {
	foundNonBlocking := false
	foundBlockingPreTool := false
	for _, hook := range manifest.Hooks {
		if strings.TrimSpace(hook.Harness) != strings.TrimSpace(harness) || !installedHookCurrentStatus(hook.Status) {
			continue
		}
		if !writeScopedHook(hook) || installedHookHandlerUnavailable(hook) {
			continue
		}
		nonBlocking := strings.TrimSpace(hook.Status) == "non_blocking"
		if nonBlocking {
			foundNonBlocking = true
		}
		if !nonBlocking {
			foundBlockingPreTool = true
		}
	}
	return foundNonBlocking && !foundBlockingPreTool
}

// OpenCode has no blocking before-tool callback, so its adapter-level
// observation-only declaration remains distinct from native PreToolUse posture
// inference. Other harnesses must prove an available non-blocking PreToolUse
// binding and cannot inherit that posture from SessionStart or outcome hooks.
func installedHooksExplicitlyObservationOnlyAdapter(manifest InstalledHooksManifest, harness string) bool {
	if strings.TrimSpace(harness) != "opencode" {
		return false
	}
	found := false
	for _, hook := range manifest.Hooks {
		if strings.TrimSpace(hook.Harness) != "opencode" || !installedHookCurrentStatus(hook.Status) {
			continue
		}
		if strings.TrimSpace(hook.Status) != "non_blocking" || installedHookHandlerUnavailable(hook) {
			return false
		}
		found = true
	}
	return found
}

func hookGateActions(row HookGateReadiness) []HookGateAction {
	if row.Status == "handler_unavailable" {
		return unavailableHandlerActions()
	}
	if !row.AdapterRequired && row.Status != "not_installed" {
		return nil
	}
	var actions []HookGateAction
	if fixture := preToolUseFixtureForHarness(row.Harness); fixture != "" {
		actions = append(actions, HookGateAction{
			Kind:             "prove_fixture_gate",
			Label:            "Prove the PreToolUse policy gate with a temporary Compound home",
			Command:          fmt.Sprintf("COMPOUND_HOME=$(mktemp -d) compound hook pre-tool-use --enforce %s", fixture),
			ExpectedExitCode: 3,
			ExpectedStatus:   "fail_open_or_blocked",
			Boundary:         "local fixture proof only; does not install or register a hook",
			Proof:            "command exits through the same PolicyDecision gate path used by wrapper adapters",
		})
	}
	if row.AdapterRequired {
		if strings.TrimSpace(row.Harness) == "codex" {
			actions = append(actions, HookGateAction{
				Kind:           "prove_live_wrapper_gate",
				Label:          "Prove Codex live PreToolUse wrapper input temporary Compound home",
				Command:        codexPreToolUseLiveWrapperProofCommand(),
				ExpectedStatus: "ok",
				Boundary:       "local synthetic live proof only; does not install hooks, register wrappers, or mutate harness config",
				Proof:          "command emits compound.codex_live_hook_response.v0.1 with status ok through the same live PreToolUse adapter a wrapper would call",
			})
		}
		actions = append(actions, HookGateAction{
			Kind:             "operator_wrapper_required",
			Label:            "Install or wire a blocking wrapper only after fixture proof",
			Boundary:         "operator-only hook registration or wrapper change; agents must not flip it",
			Proof:            "rerun `compound contract hooks-authority` and require status to move out of missing_gate/wrapper_required",
			Rollback:         "remove the wrapper or restore the previous hook registration, then rerun hooks-authority",
			RequiresOperator: true,
		})
		return actions
	}
	actions = append(actions, HookGateAction{
		Kind:             "operator_install_required",
		Label:            "Install native blocking PreToolUse hook only after fixture proof",
		Boundary:         "operator-only hook registration change; agents may prepare proof but not install",
		Proof:            "rerun `compound contract hooks-authority` and require status native_blocking",
		Rollback:         "remove the hook registration or restore previous installed-hooks authority",
		RequiresOperator: true,
	})
	return actions
}

func codexPreToolUseLiveWrapperProofCommand() string {
	return `tmp_home=$(mktemp -d); tmp_repo=$(mktemp -d); printf '{"hook_event_name":"PreToolUse","session_id":"sess_codex_wrapper_probe","cwd":"%s","tool_name":"Write","tool_input":{"file_path":"%s/README.md"}}' "$tmp_repo" "$tmp_repo" | COMPOUND_HOME="$tmp_home" compound hook pre-tool-use --live --harness codex`
}

func preToolUseFixtureForHarness(harnessName string) string {
	switch strings.TrimSpace(harnessName) {
	case "claude_code", "claude_zai":
		return "fixtures/hooks/pre-tool-use.claude.json"
	case "codex":
		return "fixtures/hooks/pre-tool-use.codex.json"
	default:
		return ""
	}
}

func InstalledHookRuntimeReadinessRows(manifest InstalledHooksManifest, supports []HookRuntimeSupport) []InstalledHookRuntimeReadiness {
	byHook := map[string]HookRuntimeSupport{}
	for _, support := range supports {
		harnessName := strings.TrimSpace(support.Harness)
		hookName := canonicalHookEvent(support.Hook)
		if harnessName == "" || hookName == "" {
			continue
		}
		byHook[harnessName+"\x00"+hookName] = support
	}
	var rows []InstalledHookRuntimeReadiness
	for _, hook := range manifest.Hooks {
		harnessName := strings.TrimSpace(hook.Harness)
		hookName := canonicalHookEvent(firstNonEmpty(hook.Event, hook.Name))
		if harnessName == "" || hookName == "" {
			continue
		}
		if !installedHookCurrentStatus(hook.Status) {
			continue
		}
		row := InstalledHookRuntimeReadiness{
			Harness:      harnessName,
			Hook:         hookName,
			Installed:    true,
			InstalledRef: strings.TrimSpace(hook.Ref),
		}
		if support, ok := byHook[harnessName+"\x00"+hookName]; ok {
			row.FixturePresent = support.FixturePresent
			row.FixtureRef = strings.TrimSpace(support.FixtureRef)
			row.LiveAdapter = support.LiveAdapter
			row.LiveCommand = strings.TrimSpace(support.LiveCommand)
		}
		switch {
		case installedHookHandlerUnavailable(hook):
			row.Status = "handler_unavailable"
			row.Reason = "installed hook declaration names concrete handler content that is missing, unreadable, or symlinked"
		case row.FixturePresent && row.LiveAdapter:
			row.Status = "ready"
			row.Reason = "installed hook has fixture proof and live adapter support"
		case !row.FixturePresent && !row.LiveAdapter:
			row.Status = "missing_fixture_and_live_adapter"
			row.Reason = "installed hook lacks checked fixture proof and live adapter support"
		case !row.FixturePresent:
			row.Status = "missing_fixture"
			row.Reason = "installed hook lacks checked fixture proof"
		default:
			row.Status = "missing_live_adapter"
			row.Reason = "installed hook lacks live adapter support for this harness"
		}
		row.Actions = hookRuntimeReadinessActions(row)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a := rows[i].Harness + "\x00" + rows[i].Hook + "\x00" + rows[i].InstalledRef
		b := rows[j].Harness + "\x00" + rows[j].Hook + "\x00" + rows[j].InstalledRef
		return a < b
	})
	return rows
}

func hookRuntimeReadinessActions(row InstalledHookRuntimeReadiness) []HookGateAction {
	if row.Status == "handler_unavailable" {
		return unavailableHandlerActions()
	}
	if row.Status == "ready" {
		return nil
	}
	cliHook := hookCLIName(row.Hook)
	if cliHook == "" {
		return nil
	}
	var actions []HookGateAction
	if row.FixturePresent && strings.TrimSpace(row.FixtureRef) != "" {
		actions = append(actions, HookGateAction{
			Kind:     "prove_fixture_hook",
			Label:    "Prove installed hook fixture with a temporary Compound home",
			Command:  fmt.Sprintf("COMPOUND_HOME=$(mktemp -d) compound hook %s %s", cliHook, row.FixtureRef),
			Boundary: "local fixture proof only; does not install hooks, register adapters, or mutate harness config",
			Proof:    "command persists normal hook artifacts in a temp COMPOUND_HOME and leaves installed-hook authority unchanged",
		})
	}
	if !row.LiveAdapter {
		actions = append(actions, HookGateAction{
			Kind:             "operator_live_adapter_required",
			Label:            "Wire live adapter or registration shim only after fixture proof",
			Command:          fmt.Sprintf("compound hook %s --live --harness %s", cliHook, hookLiveHarness(row.Harness)),
			Boundary:         "operator-only live adapter or hook registration change; agents may prepare proof and rollback notes but must not flip registration",
			Proof:            "rerun `compound contract hooks-authority` and require this hook_readiness row to leave missing_live_adapter",
			Rollback:         "remove the live adapter shim or restore the previous hook registration, then rerun `compound contract hooks-authority`",
			RequiresOperator: true,
		})
	}
	return actions
}

func unavailableHandlerActions() []HookGateAction {
	return []HookGateAction{{
		Kind:             "operator_restore_handler",
		Label:            "Restore and attest concrete handler content before enabling this hook",
		Boundary:         "operator-only handler repair or hook registration change; agents may inspect and prepare proof but must not activate an unattested handler",
		Proof:            "re-import installed-hook authority, rerun `compound contract hooks-authority`, and require this row to leave handler_unavailable",
		Rollback:         "restore the prior handler bytes or hook registration, then re-import and rerun `compound contract hooks-authority`",
		RequiresOperator: true,
	}}
}

func hookCLIName(hookName string) string {
	switch canonicalHookEvent(hookName) {
	case "SessionStart":
		return "session-start"
	case "UserPromptSubmit":
		return "user-prompt-submit"
	case "PreToolUse":
		return "pre-tool-use"
	case "PostToolUse":
		return "post-tool-use"
	case "SessionEnd":
		return "session-end"
	case "Stop":
		return "stop"
	default:
		return ""
	}
}

func hookLiveHarness(harnessName string) string {
	harnessName = strings.TrimSpace(harnessName)
	if harnessName == "claude_code" {
		return "claude-code"
	}
	return strings.ReplaceAll(harnessName, "_", "-")
}

func canonicalHookEvent(value string) string {
	key := strings.ToLower(strings.TrimSpace(value))
	key = strings.ReplaceAll(key, "-", "")
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, " ", "")
	switch key {
	case "sessionstart":
		return "SessionStart"
	case "userpromptsubmit":
		return "UserPromptSubmit"
	case "pretooluse":
		return "PreToolUse"
	case "posttooluse":
		return "PostToolUse"
	case "sessionend":
		return "SessionEnd"
	case "stop":
		return "Stop"
	default:
		return strings.TrimSpace(value)
	}
}

func installedWriteGateForHarness(manifest InstalledHooksManifest, harness string) (installed, unavailable bool, ref string) {
	installedRef := ""
	for _, hook := range manifest.Hooks {
		if strings.TrimSpace(hook.Name) == "" {
			continue
		}
		if !installedHookCurrentStatus(hook.Status) {
			continue
		}
		if strings.TrimSpace(hook.Harness) != strings.TrimSpace(harness) {
			continue
		}
		if !writeScopedHook(hook) {
			continue
		}
		ref := "hook://" + strings.TrimSpace(harness) + "/" + strings.TrimSpace(hook.Name)
		if installedHookHandlerUnavailable(hook) {
			return false, true, ref
		}
		if !installedHookBlocksMutation(hook) {
			continue
		}
		if installedRef == "" {
			installedRef = ref
		}
	}
	if installedRef != "" {
		return true, false, installedRef
	}
	return false, false, ""
}

func installedHookHandlerUnavailable(hook InstalledHook) bool {
	return strings.TrimSpace(hook.Revision) == installedHookHandlerUnavailableRevision
}

func missingWriteGateReason(harness string, availableHarnesses []string) string {
	base := "write-scoped hook missing for mutation task; no installed PreToolUse write gate registered for harness"
	if len(availableHarnesses) == 0 {
		return base
	}
	return base + "; write gate registered for harness(es) " + strings.Join(availableHarnesses, ", ") + " but not requested harness " + harness
}

func installedHookExcludedForTask(hook InstalledHook, in Input) bool {
	// A universal PreToolUse gate must stay active on read-shaped tasks:
	// secret reads, network egress, browser interactions, connector calls, and
	// unknown tools are prompt-injection sinks too. Only legacy write-only
	// matchers may be omitted from a genuinely read-only contract.
	return readOnlyTask(in) && writeOnlyHook(hook)
}

func writeScopedHook(hook InstalledHook) bool {
	event := strings.ToLower(strings.TrimSpace(firstNonEmpty(hook.Event, hook.Name)))
	matcher := strings.ToLower(strings.TrimSpace(hook.Matcher))
	ref := strings.ToLower(strings.TrimSpace(hook.Ref))
	if event != "pretooluse" {
		return false
	}
	if matcher == "" || matcher == "*" {
		return true
	}
	return strings.Contains(matcher, "write") ||
		strings.Contains(matcher, "edit") ||
		strings.Contains(matcher, "multiedit") ||
		strings.Contains(ref, "placementguard")
}

func installedHookBlocksMutation(hook InstalledHook) bool {
	return strings.TrimSpace(hook.Status) != "non_blocking" && writeScopedHook(hook)
}

func writeOnlyHook(hook InstalledHook) bool {
	event := strings.ToLower(strings.TrimSpace(firstNonEmpty(hook.Event, hook.Name)))
	matcher := strings.ToLower(strings.TrimSpace(hook.Matcher))
	ref := strings.ToLower(strings.TrimSpace(hook.Ref))
	if event != "pretooluse" || matcher == "" || matcher == "*" {
		return false
	}
	return strings.Contains(matcher, "write") ||
		strings.Contains(matcher, "edit") ||
		strings.Contains(matcher, "multiedit") ||
		strings.Contains(ref, "placementguard")
}

func writeMutationTask(in Input) bool {
	text := strings.ToLower(strings.Join([]string{in.Task, in.Mode}, " "))
	if strings.TrimSpace(text) == "" {
		return false
	}
	writeSignals := []string{
		"write", "edit", "modify", "change", "implement", "fix", "patch", "commit",
		"apply", "create", "add", "delete", "remove", "move", "rename",
	}
	for _, sig := range writeSignals {
		if strings.Contains(text, sig) {
			return true
		}
	}
	return false
}

func readOnlyTask(in Input) bool {
	text := strings.ToLower(strings.Join([]string{in.Task, in.Mode}, " "))
	if strings.TrimSpace(text) == "" {
		return false
	}
	mutationSignals := []string{
		"write", "edit", "modify", "change", "implement", "fix", "patch", "commit",
		"apply", "create", "add", "delete", "remove", "move", "rename", "run", "execute",
	}
	for _, sig := range mutationSignals {
		if strings.Contains(text, sig) {
			return false
		}
	}
	readOnlySignals := []string{
		"audit", "inspect", "explain", "review", "status", "list", "show", "read",
		"understand", "analyze", "analyse", "summarize", "summarise", "diagnose",
	}
	for _, sig := range readOnlySignals {
		if strings.Contains(text, sig) {
			return true
		}
	}
	return false
}

func resolveSourceRevisions(revs []envelope.SourceRev, cwd string) []envelope.SourceRev {
	out := append([]envelope.SourceRev(nil), revs...)
	for i := range out {
		if rev := revisionForSource(out[i].Source, cwd); rev != "" {
			out[i].Rev = rev
		}
	}
	return out
}

// SourceRefPaths returns the absolute file paths backing a source ref in the context
// of `cwd`, reusing the existing ref→path mapping (revisionForSource /
// revisionForControlPlaneSource) WITHOUT reading file contents. Used by the body
// materializer's source-ref file resolver (cmd/compound) so it can read body text
// for public repo files only; the existing per-ref revisioner still produces the
// sha256 provenance independently.
//
// Returns nil for:
//   - refs with no path mapping (memory://…, capability://…, private-plan://…);
//   - directory-backed refs (repo://docs — its body would blow any token budget,
//     and the directory hash is the authoritative revision, not the contents);
//   - paths that do not exist on disk (the resolver surfaces source_unavailable).
//
// Slice 1 boundary: this helper exposes path resolution only. Body reads stay the
// caller's job; private-overlay refs (SourceClass == "private_overlay") never reach
// here — the materializer-side law (contract.go:1262) is the load-bearing gate.
func SourceRefPaths(ref, cwd string) []string {
	switch {
	case ref == "AGENTS.md" || ref == "CLAUDE.md" || ref == "CONTEXT.md":
		if p := findUp(cwd, ref); p != "" {
			return []string{p}
		}
	case ref == universalPrinciplesRef:
		if p := findUp(cwd, universalPrinciplesPath); p != "" {
			return []string{p}
		}
	case strings.HasPrefix(ref, "control-plane://"):
		return controlPlaneSourcePaths(ref, cwd)
	case ref == "skills/README.md":
		if p := findUp(cwd, filepath.Join("skills", "README.md")); p != "" {
			return []string{p}
		}
	case ref == "repo://docs":
		// Directory-backed; no single body to include. The directory hash remains
		// the authoritative revision; bodies for docs/* are out of slice 1.
		return nil
	case filepath.IsAbs(ref) || strings.HasPrefix(ref, "~/"):
		return []string{expandPath(ref)}
	}
	return nil
}

// controlPlaneSourcePaths mirrors revisionForControlPlaneSource's ref→paths mapping,
// returning the concrete file paths instead of a composite hash. Empty when the docs
// root cannot be located or the ref has no mapping.
func controlPlaneSourcePaths(source, cwd string) []string {
	docsDir := findUp(cwd, "docs")
	if docsDir == "" {
		return nil
	}
	root := filepath.Dir(docsDir)
	var rels []string
	switch source {
	case "control-plane://orientation-map":
		rels = []string{"docs/ARCHITECTURE.md", "docs/LAYERS.md"}
	case "control-plane://workflow-contract":
		rels = []string{"docs/architecture/contracts/README.md"}
	case "control-plane://sop-packets":
		rels = []string{"docs/architecture/runtime.md", "docs/roadmap/phases.md"}
	default:
		return nil
	}
	out := make([]string, 0, len(rels))
	for _, rel := range rels {
		out = append(out, filepath.Join(root, filepath.FromSlash(rel)))
	}
	return out
}

func revisionForSource(source, cwd string) string {
	switch {
	case source == "AGENTS.md" || source == "CLAUDE.md" || source == "CONTEXT.md":
		if p := findUp(cwd, source); p != "" {
			return revisionForPath(p)
		}
	case source == universalPrinciplesRef:
		if p := findUp(cwd, universalPrinciplesPath); p != "" {
			return revisionForPath(p)
		}
	case strings.HasPrefix(source, "control-plane://"):
		return revisionForControlPlaneSource(source, cwd)
	case source == "skills/README.md":
		if p := findUp(cwd, filepath.Join("skills", "README.md")); p != "" {
			return revisionForPath(p)
		}
	case source == "repo://docs":
		if p := findUp(cwd, "docs"); p != "" {
			return revisionForPath(p)
		}
	case filepath.IsAbs(source) || strings.HasPrefix(source, "~/"):
		return revisionForPath(source)
	}
	return ""
}

func revisionForControlPlaneSource(source, cwd string) string {
	docsDir := findUp(cwd, "docs")
	if docsDir == "" {
		return ""
	}
	root := filepath.Dir(docsDir)
	var rels []string
	switch source {
	case "control-plane://orientation-map":
		rels = []string{"docs/ARCHITECTURE.md", "docs/LAYERS.md"}
	case "control-plane://workflow-contract":
		rels = []string{"docs/architecture/contracts/README.md"}
	case "control-plane://sop-packets":
		rels = []string{"docs/architecture/runtime.md", "docs/roadmap/phases.md"}
	default:
		return ""
	}
	return revisionForRelativePaths(root, rels)
}

func revisionForRelativePaths(root string, rels []string) string {
	rows := make([]string, 0, len(rels))
	for _, rel := range rels {
		rev := revisionForPath(filepath.Join(root, filepath.FromSlash(rel)))
		if rev == "" {
			return ""
		}
		rows = append(rows, rel+":"+rev)
	}
	sort.Strings(rows)
	return "sha256:" + hashBytes([]byte(strings.Join(rows, "\n")))
}

func revisionForPath(path string) string {
	path = expandPath(path)
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return "sha256:" + hashDir(path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return "sha256:" + hashBytes(data)
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

func hashBytesFull(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashDir(root string) string {
	var rows []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rows = append(rows, filepath.ToSlash(rel)+":"+hashBytes(data))
		return nil
	})
	sort.Strings(rows)
	return hashBytes([]byte(strings.Join(rows, "\n")))
}

func findUp(cwd, name string) string {
	dir := expandPath(cwd)
	if dir == "" {
		return ""
	}
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		cand := filepath.Join(dir, name)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func mergeSourceRevisions(groups ...[]envelope.SourceRev) []envelope.SourceRev {
	bySource := map[string]envelope.SourceRev{}
	for _, group := range groups {
		for _, rev := range group {
			if strings.TrimSpace(rev.Source) == "" {
				continue
			}
			bySource[rev.Source] = rev
		}
	}
	keys := make([]string, 0, len(bySource))
	for key := range bySource {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]envelope.SourceRev, 0, len(keys))
	for _, key := range keys {
		out = append(out, bySource[key])
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func projectionContextConfidence(status string) float64 {
	switch status {
	case projection.StatusFresh:
		return 1
	case projection.StatusStale:
		return 0.6
	case projection.StatusDrifted:
		return 0.3
	default:
		return 0.1
	}
}

// partition splits the policy's sources into included (ambient for this repo kind) and
// excluded (ambient elsewhere), each excluded ref carrying a why.
func includedRefPresent(refs []envelope.IncludedRef, ref string) bool {
	for _, r := range refs {
		if r.Ref == ref {
			return true
		}
	}
	return false
}

func universalPrinciplesLawLine(cwd string) string {
	path := findUp(cwd, universalPrinciplesPath)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var laws []string
	inLaws := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "## The Laws · *irreducible core*" || trimmed == "## The Laws":
			inLaws = true
			continue
		case inLaws && strings.HasPrefix(trimmed, "## "):
			inLaws = false
		case inLaws && isNumberedLawLine(trimmed):
			laws = append(laws, trimmed)
		}
	}
	if len(laws) == 0 {
		return ""
	}
	return "Laws " + strings.Join(laws, " · ")
}

func isNumberedLawLine(line string) bool {
	dot := strings.Index(line, ".")
	if dot <= 0 || dot > 2 {
		return false
	}
	for _, r := range line[:dot] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return strings.TrimSpace(line[dot+1:]) != ""
}

func partition(pol OrientationPolicy, repoKind string) ([]envelope.IncludedRef, []envelope.ExcludedRef) {
	incl := []envelope.IncludedRef{}
	excl := []envelope.ExcludedRef{}
	for _, s := range pol.Sources {
		if contains(s.AmbientFor, repoKind) {
			incl = append(incl, envelope.IncludedRef{
				Ref:         s.Ref,
				SourceClass: s.SourceClass,
				Reason:      "ambient for " + repoKind + " repos",
				Confidence:  1,
				Status:      "current",
			})
			continue
		}
		excl = append(excl, envelope.ExcludedRef{
			Ref:    s.Ref,
			Reason: fmt.Sprintf("%s context (ambient for %s) is out of scope in a %s repo", s.Class, strings.Join(s.AmbientFor, "+"), repoKind),
		})
	}
	return incl, excl
}

// header builds a runtime, private, tier0 (read-only orientation) envelope header.
// Uncertainty is "inferred" when the repo kind was defaulted rather than declared.
func header(sv, id string, now time.Time, in Input) envelope.Header {
	level := "known"
	if in.RepoKindVia == "default" {
		level = "inferred"
	}
	return envelope.Header{
		SchemaVersion: sv,
		ID:            id,
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
		Scope:         envelope.Scope{Surface: in.Cwd},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       "private",
		Risk:          "tier0",
		EvidenceRefs:  []string{},
		Uncertainty:   envelope.Uncertainty{Level: level},
	}
}

// idFor derives a deterministic id from inputs so the same request yields the same id
// (testable; no clock/random in the id).
func idFor(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(h[:])[:12]
}

func contractIDForInput(in Input) string {
	return idFor("contract_", inputIDParts(in)...)
}

func packIDForInput(in Input) string {
	return idFor("ctx_", inputIDParts(in)...)
}

// inputIDParts are the deterministic id inputs shared by the effective contract and the
// context pack. Topic is appended ONLY when set, so an empty topic yields byte-identical
// ids (behavior unchanged) while distinct topics get distinct artifacts instead of
// clobbering the same pack/contract file.
func inputIDParts(in Input) []string {
	parts := []string{in.Harness, in.RepoKind, in.Task, modeIDPart(in.Mode)}
	if topic := strings.TrimSpace(in.Topic); topic != "" {
		parts = append(parts, "topic:"+topic)
	}
	if taskContractAddsMeaning(in) {
		data, _ := json.Marshal(resolvedTaskContract(in))
		digest := sha256.Sum256(data)
		parts = append(parts, "task_contract:"+hex.EncodeToString(digest[:]))
	}
	return parts
}

func resolvedTaskContract(in Input) envelope.TaskContract {
	task := in.TaskContract
	if intent := strings.TrimSpace(in.Task); intent != "" {
		task.Intent = intent
	} else {
		task.Intent = strings.TrimSpace(task.Intent)
	}
	if mode := strings.TrimSpace(in.Mode); mode != "" {
		task.Mode = mode
	} else {
		task.Mode = strings.TrimSpace(task.Mode)
	}
	task.DesiredOutcome = strings.TrimSpace(task.DesiredOutcome)
	task.SuccessPredicate = strings.TrimSpace(task.SuccessPredicate)
	task.ProofMedium = strings.TrimSpace(task.ProofMedium)
	nonGoals := make([]string, 0, len(task.NonGoals))
	for _, nonGoal := range task.NonGoals {
		if nonGoal = strings.TrimSpace(nonGoal); nonGoal != "" {
			nonGoals = append(nonGoals, nonGoal)
		}
	}
	task.NonGoals = nonGoals
	return task
}

func taskContractAddsMeaning(in Input) bool {
	task := resolvedTaskContract(in)
	return task.Intent != strings.TrimSpace(in.Task) ||
		task.Mode != strings.TrimSpace(in.Mode) ||
		task.DesiredOutcome != "" ||
		task.SuccessPredicate != "" ||
		len(task.NonGoals) > 0 ||
		task.ProofMedium != ""
}

func modeIDPart(mode string) string {
	return "mode:" + strings.TrimSpace(mode)
}

func expandPath(path string) string {
	path = os.ExpandEnv(strings.TrimSpace(path))
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func isWithin(root, cwd string) bool {
	rel, err := filepath.Rel(root, cwd)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
