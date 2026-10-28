// memory_engine_config.go owns the memory engine config subsystem that
// `compound memory engine-config` and `compound memory doctor` call through,
// plus the breadth-sidecar factory (memBreadthEngine) that the orient path
// and `compound memory parity-replay` call through. Extracted from
// cmd/compound/main.go so the engine config surface is its own file
// alongside memory_cli.go (the verb dispatcher) and memory_parity_replay.go
// (the journal-driven parity harness).
//
// The cmdMemory header (memory_cli.go) named this as the next extraction
// after cmdMemory landed; this is that slice.
//
// Composition: this file holds ~470 lines of memory-engine-config helpers,
// types, and the breadth-sidecar factory. The CLI verb dispatcher stays in
// memory_cli.go; the journal-driven parity harness stays in
// memory_parity_replay.go; this file is the cohesive subsystem between
// them.
//
// Public surface (package main, all reachable from memory_cli.go and
// memory_parity_replay.go without a new package boundary):
//   - memBreadthEngine() (mem.Engine, func())
//   - normalizeMemoryEngineProfile(string) (string, error)
//   - memoryEngineConfigPath(string) string
//   - loadMemoryEngineConfig(string) (memoryEngineConfig, string, error)
//   - writeMemoryEngineConfig(string, string) (memoryEngineConfigReport, error)
//   - clearMemoryEngineConfig(string) (memoryEngineConfigReport, error)
//   - validateCorpusLaneConfigs([]corpus.LaneConfig) error
//   - configureMemoryEngineFromHome(string, string) (func(), error)
//   - applyEmbedConfigEnv(memoryEmbedConfig) func()
//   - memorySemanticReadiness(string, mem.Engine) *mem.SemanticReadiness
//   - memoryVectorCacheReadiness(string, string, int64) (memoryVectorCacheStatus, string)
//   - memoryManifestEmbedderIdentity(memoryVectorCacheManifest) string
//   - memoryStampSlug(string) string
//   - configureMemoryEngineProfile(string) (func(), error)
//   - defaultMemoryCLISidecarCommand() string
//   - defaultBreadthSidecarCommand() (string, error)
//   - captureEnv(...string) func()
//   - dashNote(string) string
//   - okStr(bool) string
//   - types: memoryEngineConfig, memoryEmbedConfig, memoryEngineConfigReport,
//     memoryVectorCacheManifest, memoryVectorCacheStatus
//   - const: memoryEngineConfigSchemaVersion
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"compound/internal/breadth"
	"compound/internal/corpus"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/persist"

	"gopkg.in/yaml.v3"
)

// memBreadthEngine selects the rented breadth engine: a protocol sidecar (held warm) when
// COMPOUND_MEM_ENGINE_BIN is set (e.g. the turbovec sidecar, or compound-mock-engine), else the
// in-process lexical engine. Its own env var — distinct from the legacy COMPOUND_ENGINE_BIN, which
// speaks a different protocol and retires with the legacy engine. COMPOUND_MEM_ENGINE_EMBEDDER is
// compound's record of what it indexed with; it drives the identity guard (empty = adopt on first use).
func memBreadthEngine() (mem.Engine, func()) {
	if err := memoryExternalStoreRefusal(); err != nil {
		return refusedMemoryBreadthEngine{err: err}, func() {}
	}
	// One shared factory (mem.DefaultBreadthEngine) so the CLI and the orient/ContextPack
	// path build the identical engine. Resolve the home here (CLI layer owns home resolution)
	// and hand it to the factory, which forwards it to the sidecar's vector cache.
	resolved := ""
	if hp, err := home.Resolve(""); err == nil {
		resolved = strings.TrimSpace(hp)
	}
	return mem.DefaultBreadthEngine(resolved)
}

// memoryExternalStoreRefusal keeps the public memory CLI from silently using
// the file-backed breadth lane when an external store was selected. The
// Postgres experiment has its own explicit `compound store` surface; until the
// memory CLI is wired to that engine, selecting it is a categorical refusal.
func memoryExternalStoreRefusal() error {
	cfg, err := persist.ConfigFromEnv()
	if err != nil {
		return err
	}
	if cfg.Backend == persist.BackendPostgres {
		return &persist.ConfigError{
			Field:    "COMPOUND_STORE",
			Reason:   "the selected experimental Postgres backend is not wired to the memory breadth CLI",
			Recovery: "use the explicit experimental `compound store` commands, or unset Postgres selection for the supported file-backed memory CLI",
		}
	}
	return nil
}

type refusedMemoryBreadthEngine struct {
	err error
}

func (e refusedMemoryBreadthEngine) Capabilities() mem.EngineCapabilities {
	return mem.EngineCapabilities{Name: "external-store-refused", Breadth: true}
}

func (e refusedMemoryBreadthEngine) Index([]mem.Claim, int64) error {
	return e.err
}

func (e refusedMemoryBreadthEngine) Query(string, string, []string, int) ([]mem.Hit, error) {
	return nil, e.err
}

const memoryEngineConfigSchemaVersion = "compound.memory_engine_config.v0.1"

type memoryEngineConfig struct {
	SchemaVersion string `json:"schema_version" yaml:"schema_version"`
	Profile       string `json:"profile,omitempty" yaml:"profile,omitempty"`
	// Embed is the OPT-IN embedder for the breadth profile's semantic lane. Absent/empty =
	// lexical only (OFF by default, no silent default — the EP-A6 ruling). When Model is set,
	// the breadth profile forwards these to the sidecar so semantic recall engages WITHOUT the
	// operator exporting COMPOUND_BREADTH_EMBED_* in every shell.
	Embed *memoryEmbedConfig `json:"embed,omitempty" yaml:"embed,omitempty"`
	// CorpusLanes are class-separated imported-corpus indexes. They are never
	// ambient memory profiles and never inherit COMPOUND_BREADTH_EMBED_*.
	CorpusLanes []corpus.LaneConfig `json:"corpus_lanes,omitempty" yaml:"corpus_lanes,omitempty"`
}

type memoryEmbedConfig struct {
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"` // e.g. ollama | openai
	Model    string `json:"model,omitempty" yaml:"model,omitempty"`       // e.g. nomic-embed-text | text-embedding-3-large
	BaseURL  string `json:"base_url,omitempty" yaml:"base_url,omitempty"` // e.g. localhost:11434/v1 (ollama)
	Embedder string `json:"embedder,omitempty" yaml:"embedder,omitempty"` // optional identity guard (provider:model:dim:bits)
}

type memoryEngineConfigReport struct {
	SchemaVersion string `json:"schema_version"`
	ConfigPath    string `json:"config_path"`
	Configured    bool   `json:"configured"`
	Profile       string `json:"profile,omitempty"`
	CorpusLanes   int    `json:"corpus_lanes,omitempty"`
	Cleared       bool   `json:"cleared,omitempty"`
}

func normalizeMemoryEngineProfile(profile string) (string, error) {
	profile = strings.ToLower(strings.TrimSpace(profile))
	switch profile {
	case "", "generic", "memory-cli", "breadth":
		return profile, nil
	case "gbrain":
		// gbrain is RETIRED as a memory engine (2026-08-08, operator-directed):
		// compound owns the memory layer — ledger + corpus lanes — and the
		// dormant COMPOUND_MEMORY_CLI_PROFILE=gbrain path that shelled to a
		// gbrain binary must not be selectable. Reject it loudly instead of
		// silently falling back, so a stale config cannot resurrect it.
		return "", fmt.Errorf("memory engine profile %q is retired; compound owns memory — use %q (ledger + corpus lanes)", "gbrain", "breadth")
	default:
		return "", fmt.Errorf("unknown memory engine profile %q", profile)
	}
}

func memoryEngineConfigPath(homePath string) string {
	return filepath.Join(homePath, "config", "memory-engine.yaml")
}

func loadMemoryEngineConfig(homePath string) (memoryEngineConfig, string, error) {
	path := memoryEngineConfigPath(homePath)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return memoryEngineConfig{}, path, nil
	}
	if err != nil {
		return memoryEngineConfig{}, path, err
	}
	var cfg memoryEngineConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return memoryEngineConfig{}, path, fmt.Errorf("parse memory engine config: %w", err)
	}
	if cfg.SchemaVersion != "" && cfg.SchemaVersion != memoryEngineConfigSchemaVersion {
		return memoryEngineConfig{}, path, fmt.Errorf("unsupported memory engine config schema %q", cfg.SchemaVersion)
	}
	profile, err := normalizeMemoryEngineProfile(cfg.Profile)
	if err != nil {
		return memoryEngineConfig{}, path, err
	}
	cfg.SchemaVersion = memoryEngineConfigSchemaVersion
	cfg.Profile = profile
	if err := validateCorpusLaneConfigs(cfg.CorpusLanes); err != nil {
		return memoryEngineConfig{}, path, err
	}
	for i := range cfg.CorpusLanes {
		lane, err := corpus.ValidateLaneConfig(cfg.CorpusLanes[i])
		if err != nil {
			return memoryEngineConfig{}, path, err
		}
		cfg.CorpusLanes[i] = lane
	}
	return cfg, path, nil
}

func validateCorpusLaneConfigs(lanes []corpus.LaneConfig) error {
	seen := map[string]bool{}
	for _, lane := range lanes {
		name := strings.TrimSpace(lane.Lane)
		if name == "" {
			return fmt.Errorf("corpus lane name required")
		}
		if seen[name] {
			return fmt.Errorf("duplicate corpus lane %q", name)
		}
		seen[name] = true
	}
	return nil
}

func writeMemoryEngineConfig(homePath, profile string) (memoryEngineConfigReport, error) {
	profile, err := normalizeMemoryEngineProfile(profile)
	if err != nil {
		return memoryEngineConfigReport{}, err
	}
	if profile == "" {
		return memoryEngineConfigReport{}, fmt.Errorf("memory engine profile is required")
	}
	path := memoryEngineConfigPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return memoryEngineConfigReport{}, err
	}
	cfg := memoryEngineConfig{SchemaVersion: memoryEngineConfigSchemaVersion, Profile: profile}
	// Preserve an existing opt-in embedder block so switching profile doesn't silently disable
	// the semantic lane the operator configured.
	if existing, _, err := loadMemoryEngineConfig(homePath); err == nil && existing.Embed != nil {
		cfg.Embed = existing.Embed
	}
	if existing, _, err := loadMemoryEngineConfig(homePath); err == nil && len(existing.CorpusLanes) > 0 {
		cfg.CorpusLanes = existing.CorpusLanes
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return memoryEngineConfigReport{}, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return memoryEngineConfigReport{}, err
	}
	return memoryEngineConfigReport{
		SchemaVersion: memoryEngineConfigSchemaVersion,
		ConfigPath:    path,
		Configured:    true,
		Profile:       profile,
	}, nil
}

func clearMemoryEngineConfig(homePath string) (memoryEngineConfigReport, error) {
	path := memoryEngineConfigPath(homePath)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return memoryEngineConfigReport{}, err
	}
	return memoryEngineConfigReport{
		SchemaVersion: memoryEngineConfigSchemaVersion,
		ConfigPath:    path,
		Configured:    false,
		Cleared:       true,
	}, nil
}

func configureMemoryEngineFromHome(homePath, explicitProfile string) (func(), error) {
	profile, err := normalizeMemoryEngineProfile(explicitProfile)
	if err != nil {
		return func() {}, err
	}
	var cfg memoryEngineConfig
	cfg, _, err = loadMemoryEngineConfig(homePath)
	if err != nil {
		return func() {}, err
	}
	if profile == "" {
		profile = cfg.Profile
	}
	restore, err := configureMemoryEngineProfile(profile)
	if err != nil {
		return restore, err
	}
	// Opt-in embedder: makes the breadth semantic lane engage from home config, not shell exports.
	// Fill-in only (a shell export still wins) and gated on Model being set, so an empty/absent
	// block leaves the lane lexical — OFF by default, no silent default (EP-A6 ruling).
	if profile == "breadth" && cfg.Embed != nil && strings.TrimSpace(cfg.Embed.Model) != "" {
		embedRestore := applyEmbedConfigEnv(*cfg.Embed)
		return func() { embedRestore(); restore() }, nil
	}
	return restore, nil
}

// applyEmbedConfigEnv fills the breadth sidecar's embedder env from config WITHOUT overriding an
// explicit shell export, capturing prior values so the caller can restore them.
func applyEmbedConfigEnv(embed memoryEmbedConfig) func() {
	restore := captureEnv(
		"COMPOUND_BREADTH_EMBED_MODEL",
		"COMPOUND_BREADTH_EMBED_PROVIDER",
		"COMPOUND_BREADTH_EMBED_BASE_URL",
		"COMPOUND_MEM_ENGINE_EMBEDDER",
	)
	setIfUnset := func(key, val string) {
		if strings.TrimSpace(val) == "" {
			return
		}
		if strings.TrimSpace(os.Getenv(key)) == "" {
			_ = os.Setenv(key, val)
		}
	}
	setIfUnset("COMPOUND_BREADTH_EMBED_MODEL", embed.Model)
	setIfUnset("COMPOUND_BREADTH_EMBED_PROVIDER", embed.Provider)
	setIfUnset("COMPOUND_BREADTH_EMBED_BASE_URL", embed.BaseURL)
	setIfUnset("COMPOUND_MEM_ENGINE_EMBEDDER", embed.Embedder)
	return restore
}

type memoryVectorCacheManifest struct {
	SchemaVersion string `json:"schema_version"`
	Embedder      struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Dim      int    `json:"dim"`
		Bits     string `json:"bits"`
	} `json:"embedder"`
	CorpusGen     int64 `json:"corpus_gen"`
	ClaimCount    int   `json:"claim_count"`
	EmbeddedCount int   `json:"embedded_count"`
}

func memorySemanticReadiness(homePath string, eng mem.Engine) *mem.SemanticReadiness {
	cfg, cfgPath, err := loadMemoryEngineConfig(homePath)
	readiness := &mem.SemanticReadiness{
		Status:            "lexical_only",
		ConfigPath:        cfgPath,
		Sidecar:           "none",
		VectorCacheStatus: "not_configured",
		Detail:            "breadth is lexical only; no semantic embedder model is configured",
	}
	if err != nil {
		readiness.Status = "semantic_degraded"
		readiness.Detail = "memory engine config unreadable: " + err.Error()
		return readiness
	}
	readiness.Profile = cfg.Profile
	if sidecar := strings.TrimSpace(os.Getenv("COMPOUND_MEM_ENGINE_BIN")); sidecar != "" {
		readiness.Sidecar = "configured"
	}
	if eng != nil {
		caps := eng.Capabilities()
		readiness.Engine = firstNonEmptyString(caps.Name, "configured-engine")
	}

	model := strings.TrimSpace(os.Getenv("COMPOUND_BREADTH_EMBED_MODEL"))
	if model == "" && cfg.Embed != nil {
		model = strings.TrimSpace(cfg.Embed.Model)
	}
	guard := strings.TrimSpace(os.Getenv("COMPOUND_MEM_ENGINE_EMBEDDER"))
	if guard == "" && cfg.Embed != nil {
		guard = strings.TrimSpace(cfg.Embed.Embedder)
	}
	readiness.EmbedderGuard = guard
	if model == "" {
		if readiness.Sidecar == "configured" {
			readiness.Detail = fmt.Sprintf("profile=%s sidecar=%s engine=%s; semantic embedder model is not configured", firstNonEmptyString(readiness.Profile, "default"), readiness.Sidecar, firstNonEmptyString(readiness.Engine, "none"))
		}
		return readiness
	}
	readiness.EmbedderConfigured = true

	embedder, ok := breadth.ResolveEmbedder(homePath)
	if !ok || embedder == nil {
		readiness.Status = "semantic_degraded"
		readiness.VectorCacheStatus = "unknown"
		readiness.Detail = fmt.Sprintf("profile=%s sidecar=%s; semantic embedder config did not resolve or guard mismatched", firstNonEmptyString(readiness.Profile, "default"), readiness.Sidecar)
		return readiness
	}
	identity := embedder.Identity()
	readiness.EmbedderIdentity = identity
	cacheStatus, cacheDetail := memoryVectorCacheReadiness(homePath, identity, 0)
	readiness.VectorCacheStatus = cacheStatus.Status
	readiness.VectorCachePath = cacheStatus.Path
	readiness.VectorCacheCorpusGen = cacheStatus.CorpusGen
	readiness.VectorCacheClaims = cacheStatus.ClaimCount
	readiness.VectorCacheEmbedded = cacheStatus.EmbeddedCount

	if readiness.Sidecar != "configured" {
		readiness.Status = "semantic_inactive"
		readiness.Detail = fmt.Sprintf("profile=%s sidecar=none embedder=%s vector_cache=%s; semantic embedder configured but no breadth sidecar is active", firstNonEmptyString(readiness.Profile, "default"), identity, readiness.VectorCacheStatus)
		return readiness
	}
	if eng == nil {
		readiness.Status = "semantic_degraded"
		readiness.Detail = fmt.Sprintf("profile=%s sidecar=configured embedder=%s vector_cache=%s; memory doctor did not attach engine", firstNonEmptyString(readiness.Profile, "default"), identity, readiness.VectorCacheStatus)
		return readiness
	}
	caps := eng.Capabilities()
	readiness.Engine = firstNonEmptyString(caps.Name, "configured-engine")
	if caps.Embedder != "" && caps.Embedder != identity {
		readiness.Status = "semantic_degraded"
		readiness.Detail = fmt.Sprintf("profile=%s sidecar=configured engine=%s embedder_mismatch configured=%s engine=%s vector_cache=%s", firstNonEmptyString(readiness.Profile, "default"), readiness.Engine, identity, caps.Embedder, readiness.VectorCacheStatus)
		return readiness
	}
	if caps.Embedder == "" || caps.Embedder == breadth.DefaultEmbedder {
		readiness.Status = "semantic_inactive"
		readiness.Detail = fmt.Sprintf("profile=%s sidecar=configured engine=%s embedder=%s vector_cache=%s; sidecar reported lexical/no semantic embedder", firstNonEmptyString(readiness.Profile, "default"), readiness.Engine, identity, readiness.VectorCacheStatus)
		return readiness
	}
	if readiness.VectorCacheStatus == "embedder_mismatch" || readiness.VectorCacheStatus == "unreadable" || readiness.VectorCacheStatus == "schema_mismatch" {
		readiness.Status = "semantic_degraded"
		readiness.Detail = fmt.Sprintf("profile=%s sidecar=configured engine=%s embedder=%s vector_cache=%s: %s", firstNonEmptyString(readiness.Profile, "default"), readiness.Engine, identity, readiness.VectorCacheStatus, cacheDetail)
		return readiness
	}
	readiness.Status = "semantic_active"
	readiness.Detail = fmt.Sprintf("profile=%s sidecar=configured engine=%s embedder=%s vector_cache=%s", firstNonEmptyString(readiness.Profile, "default"), readiness.Engine, identity, readiness.VectorCacheStatus)
	if cacheDetail != "" {
		readiness.Detail += " (" + cacheDetail + ")"
	}
	return readiness
}

type memoryVectorCacheStatus struct {
	Status        string
	Path          string
	CorpusGen     int64
	ClaimCount    int
	EmbeddedCount int
}

func memoryVectorCacheReadiness(homePath, identity string, engineGen int64) (memoryVectorCacheStatus, string) {
	status := memoryVectorCacheStatus{Status: "not_configured"}
	if strings.TrimSpace(identity) == "" {
		return status, ""
	}
	status.Status = "missing"
	status.Path = filepath.Join(homePath, "indexes", "breadth", memoryStampSlug(identity), "manifest.json")
	raw, err := os.ReadFile(status.Path)
	if os.IsNotExist(err) {
		return status, "manifest missing"
	}
	if err != nil {
		status.Status = "unreadable"
		return status, err.Error()
	}
	var manifest memoryVectorCacheManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		status.Status = "unreadable"
		return status, "parse manifest: " + err.Error()
	}
	status.CorpusGen = manifest.CorpusGen
	status.ClaimCount = manifest.ClaimCount
	status.EmbeddedCount = manifest.EmbeddedCount
	if manifest.SchemaVersion != "compound.breadth.index.v1" {
		status.Status = "schema_mismatch"
		return status, "schema=" + manifest.SchemaVersion
	}
	if got := memoryManifestEmbedderIdentity(manifest); got != identity {
		status.Status = "embedder_mismatch"
		return status, fmt.Sprintf("cache=%s configured=%s", got, identity)
	}
	if manifest.EmbeddedCount < manifest.ClaimCount {
		status.Status = "partial"
		return status, fmt.Sprintf("%d/%d embedded", manifest.EmbeddedCount, manifest.ClaimCount)
	}
	if engineGen > 0 && manifest.CorpusGen < engineGen {
		status.Status = "stale"
		return status, fmt.Sprintf("cache_gen=%d engine_gen=%d", manifest.CorpusGen, engineGen)
	}
	status.Status = "healthy"
	return status, fmt.Sprintf("gen=%d embedded=%d/%d", manifest.CorpusGen, manifest.EmbeddedCount, manifest.ClaimCount)
}

func memoryManifestEmbedderIdentity(manifest memoryVectorCacheManifest) string {
	return fmt.Sprintf("%s:%s:%d:%s", manifest.Embedder.Provider, manifest.Embedder.Model, manifest.Embedder.Dim, manifest.Embedder.Bits)
}

func memoryStampSlug(identity string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(identity) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func configureMemoryEngineProfile(profile string) (func(), error) {
	profile, err := normalizeMemoryEngineProfile(profile)
	if err != nil {
		return func() {}, err
	}
	if profile == "" {
		return func() {}, nil
	}
	restore := captureEnv("COMPOUND_MEM_ENGINE_BIN", "COMPOUND_MEMORY_CLI_PROFILE")
	if profile == "breadth" {
		bin, err := defaultBreadthSidecarCommand()
		if err != nil {
			return restore, err
		}
		if err := os.Setenv("COMPOUND_MEM_ENGINE_BIN", bin); err != nil {
			return restore, err
		}
		_ = os.Unsetenv("COMPOUND_MEMORY_CLI_PROFILE")
		return restore, nil
	}
	if strings.TrimSpace(os.Getenv("COMPOUND_MEM_ENGINE_BIN")) == "" {
		if err := os.Setenv("COMPOUND_MEM_ENGINE_BIN", defaultMemoryCLISidecarCommand()); err != nil {
			return restore, err
		}
	}
	if err := os.Setenv("COMPOUND_MEMORY_CLI_PROFILE", profile); err != nil {
		restore()
		return func() {}, err
	}
	return restore, nil
}

func defaultMemoryCLISidecarCommand() string {
	for _, path := range []string{"./cmd/compound-memory-cli-engine", "../compound-memory-cli-engine"} {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return "go run " + path
		}
	}
	return "compound-memory-cli-engine"
}

func defaultBreadthSidecarCommand() (string, error) {
	path, err := exec.LookPath("compound-breadth-engine")
	if err != nil {
		return "", fmt.Errorf("resolve compound-breadth-engine: %w", err)
	}
	return path, nil
}

func captureEnv(names ...string) func() {
	type value struct {
		text string
		ok   bool
	}
	saved := map[string]value{}
	for _, name := range names {
		text, ok := os.LookupEnv(name)
		saved[name] = value{text: text, ok: ok}
	}
	return func() {
		for _, name := range names {
			v := saved[name]
			if v.ok {
				_ = os.Setenv(name, v.text)
			} else {
				_ = os.Unsetenv(name)
			}
		}
	}
}

func dashNote(n string) string {
	if n == "" {
		return ""
	}
	return " — " + n
}
func okStr(b bool) string {
	if b {
		return "ok"
	}
	return "FAIL"
}
