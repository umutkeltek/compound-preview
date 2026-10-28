package routeevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"compound/internal/candidates"
	homepkg "compound/internal/home"
	"compound/internal/reviewartifact"
	autonomyproof "compound/internal/routeevidence/autonomy"
	"compound/internal/writeback"
	"gopkg.in/yaml.v3"
)

const (
	ModelCapabilitiesAuthorityRef          = "compound://registries/model-router"
	ModelCapabilitiesSourceAuthoritySchema = "compound.model_capabilities_source_authority.v0.1"
	ModelCapabilitiesProjectionSchema      = "compound.model_capabilities_projection.v0.1"
	CapabilityRoutePlanSchemaVersion       = "compound.capability_route_plan.v0.1"

	modelCapabilitiesFile              = "model-capabilities.yaml"
	modelCapabilitiesSourceAuthority   = "model-capabilities-source-authority.jsonl"
	modelCapabilitiesImportCommandName = "route import-model-capabilities"

	ProviderRouteKindProviderClass = "provider_class"
	ProviderRouteKindDirectModel   = "direct_model"
)

// planCapabilityRouteCacheTTL bounds how long one PlanCapabilityRoute result
// is reused inside a single process. The director runs the planner once per
// heartbeat; cache entries older than TTL are evicted so a long-lived process
// (interactive CLI, daemon) eventually re-probes codexbar and re-reads
// manifests instead of serving stale lane state forever.
const planCapabilityRouteCacheTTL = 60 * time.Second

// planCapabilityRouteCacheBuckets rounds now down to planCapabilityRouteCacheTTL
// so two calls a few seconds apart share a bucket. A finer bucket would defeat
// the cache; a coarser one risks serving decisions past an upstream freshness
// window.
func planCapabilityRouteCacheBuckets(now time.Time) int64 {
	return now.UTC().Truncate(planCapabilityRouteCacheTTL).UnixNano()
}

type planCapabilityRouteCacheEntry struct {
	plan  CapabilityRoutePlan
	err   error
	stamp time.Time
}

var (
	planCapabilityRouteCacheMu sync.Mutex
	planCapabilityRouteCache   = map[string]planCapabilityRouteCacheEntry{}
	planCapabilityRouteStats   struct {
		hits   int64
		misses int64
	}
)

// ResetPlanCapabilityRouteCache clears the in-process cache and counter state.
// Tests call this in setup so a previous test's plans do not leak across cases.
func ResetPlanCapabilityRouteCache() {
	planCapabilityRouteCacheMu.Lock()
	defer planCapabilityRouteCacheMu.Unlock()
	planCapabilityRouteCache = map[string]planCapabilityRouteCacheEntry{}
	planCapabilityRouteStats.hits = 0
	planCapabilityRouteStats.misses = 0
}

// PlanCapabilityRouteCacheStats returns the (hits, misses) counter pair for
// the in-process planner cache. The director uses these to log cache
// effectiveness; tests assert the second call hits the cache.
func PlanCapabilityRouteCacheStats() (hits, misses int64) {
	planCapabilityRouteCacheMu.Lock()
	defer planCapabilityRouteCacheMu.Unlock()
	return planCapabilityRouteStats.hits, planCapabilityRouteStats.misses
}

// planCapabilityRouteInputSignature folds the identity (mtime + size) of every
// on-disk input PlanCapabilityRoute reads into the cache key, so a manifest
// import, a recorded route observation, a lane-speed probe, or a capacity
// usage write inside one TTL bucket recomputes the plan instead of serving
// the previous one (TestRouteImportModelCapabilitiesFeedsCapabilityPlan
// records a cost-doctor observation between two plan calls and expects the
// second plan to carry the debt). A stat costs microseconds; the ~8s the
// cache saves is the codexbar probe, not these reads.
func planCapabilityRouteInputSignature(homePath, usageHistoryPath string) string {
	paths := []string{
		ModelCapabilitiesPath(homePath),
		ModelCapabilitiesSourceAuthorityPath(homePath),
		LaneSpeedLedgerPath(homePath),
		filepath.Join(homePath, candidates.InboxDir(candidates.RouteObservation)),
	}
	if strings.TrimSpace(usageHistoryPath) != "" {
		paths = append(paths, usageHistoryPath)
	}
	var b strings.Builder
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			b.WriteString("absent;")
			continue
		}
		fmt.Fprintf(&b, "%d:%d;", info.ModTime().UnixNano(), info.Size())
	}
	return b.String()
}

func planCapabilityRouteCacheKey(homePath, capabilityClass string, opts CapabilityRoutePlanOptions, bucket int64) string {
	var capPtr, latencyBudgetFingerprint string
	if opts.CapacityConfig != nil {
		capPtr = opts.CapacityConfig.Paths.UsageHistoryPath
	}
	if opts.LatencyBudget != nil {
		latencyBudgetFingerprint = fmt.Sprintf("%g/%d/%g/%d",
			opts.LatencyBudget.Interactive.MinTokensPerSec,
			opts.LatencyBudget.Interactive.MaxTTFTms,
			opts.LatencyBudget.Batch.MinTokensPerSec,
			opts.LatencyBudget.Batch.MaxTTFTms,
		)
	}
	// LatencyClass participates in the plan filter (worth-the-wait veto) and
	// must invalidate the cache even when no LatencyBudget override is passed;
	// otherwise the second call with a different LatencyClass returns the
	// previously filtered chain instead of recomputing.
	return fmt.Sprintf("%s\x00%s\x00%t\x00%t\x00%t\x00%d\x06%t\x06%s\x00%s\x00%s\x00%d\x00%s",
		homePath,
		capabilityClass,
		opts.PrivacySensitive,
		opts.SuppressPendingObservations,
		opts.SuppressRefusalEvidence,
		opts.ExpectedMinutes,
		opts.Confirm,
		capPtr,
		opts.LatencyClass,
		latencyBudgetFingerprint,
		bucket,
		planCapabilityRouteInputSignature(homePath, capPtr),
	)
}

func planCapabilityRouteCacheLookup(key string) (CapabilityRoutePlan, error, bool) {
	planCapabilityRouteCacheMu.Lock()
	defer planCapabilityRouteCacheMu.Unlock()
	entry, ok := planCapabilityRouteCache[key]
	if !ok {
		return CapabilityRoutePlan{}, nil, false
	}
	if time.Since(entry.stamp) > planCapabilityRouteCacheTTL {
		delete(planCapabilityRouteCache, key)
		return CapabilityRoutePlan{}, nil, false
	}
	return entry.plan, entry.err, true
}

func planCapabilityRouteCacheStore(key string, plan CapabilityRoutePlan, err error) {
	planCapabilityRouteCacheMu.Lock()
	defer planCapabilityRouteCacheMu.Unlock()
	planCapabilityRouteCache[key] = planCapabilityRouteCacheEntry{plan: plan, err: err, stamp: time.Now()}
}

func planCapabilityRouteCacheRecordHit() {
	planCapabilityRouteCacheMu.Lock()
	defer planCapabilityRouteCacheMu.Unlock()
	planCapabilityRouteStats.hits++
}

func planCapabilityRouteCacheRecordMiss() {
	planCapabilityRouteCacheMu.Lock()
	defer planCapabilityRouteCacheMu.Unlock()
	planCapabilityRouteStats.misses++
}

type CapabilityClaim struct {
	Level    string `json:"level" yaml:"level"`
	Evidence string `json:"evidence" yaml:"evidence"`
}

type ResolvedTarget struct {
	Provider string `json:"provider" yaml:"provider"`
	Model    string `json:"model" yaml:"model"`
}

type LaneResolution struct {
	Mode    string           `json:"mode" yaml:"mode"`
	Targets []ResolvedTarget `json:"targets,omitempty" yaml:"targets,omitempty"`
}

type LaneCostDetail struct {
	APIUSDPerMtokIn  float64               `json:"api_usd_per_mtok_in,omitempty" yaml:"api_usd_per_mtok_in,omitempty"`
	APIUSDPerMtokOut float64               `json:"api_usd_per_mtok_out,omitempty" yaml:"api_usd_per_mtok_out,omitempty"`
	Subscription     *LaneSubscriptionCost `json:"subscription,omitempty" yaml:"subscription,omitempty"`
}

type LaneSubscriptionCost struct {
	WindowMinutes  int     `json:"window_minutes,omitempty" yaml:"window_minutes,omitempty"`
	BusyMultiplier float64 `json:"busy_multiplier,omitempty" yaml:"busy_multiplier,omitempty"`
	PlanRef        string  `json:"plan_ref,omitempty" yaml:"plan_ref,omitempty"`
}

type LaneCapacitySourceRef struct {
	Kind             string `json:"kind,omitempty" yaml:"kind,omitempty"`
	FreshnessMinutes int    `json:"freshness_minutes,omitempty" yaml:"freshness_minutes,omitempty"`
	Ref              string `json:"ref,omitempty" yaml:"ref,omitempty"`
}

type ModelCapabilityEntry struct {
	Lane              string                     `json:"lane" yaml:"lane"`
	Harness           string                     `json:"harness,omitempty" yaml:"harness,omitempty"`
	ProviderRoute     string                     `json:"provider_route,omitempty" yaml:"provider_route,omitempty"`
	ProviderEndpoint  string                     `json:"provider_endpoint,omitempty" yaml:"provider_endpoint,omitempty"`
	ProviderRouteKind string                     `json:"provider_route_kind,omitempty" yaml:"provider_route_kind,omitempty"`
	Resolution        LaneResolution             `json:"resolution" yaml:"resolution"`
	EffortDefault     string                     `json:"effort_default,omitempty" yaml:"effort_default,omitempty"`
	CostClass         string                     `json:"cost_class,omitempty" yaml:"cost_class,omitempty"`
	Tier              string                     `json:"tier" yaml:"tier"`
	PacketCeiling     string                     `json:"packet_ceiling" yaml:"packet_ceiling"`
	Capabilities      map[string]CapabilityClaim `json:"capabilities" yaml:"capabilities"`
	Notes             string                     `json:"notes,omitempty" yaml:"notes,omitempty"`
	// OPTIONAL LaneTuple facts (compound://registries/model-router additive extension).
	// All fields are optional; legacy YAML without these keys continues to import cleanly.
	ContextWindow    int                    `json:"context_window,omitempty" yaml:"context_window,omitempty"`
	MaxOutput        int                    `json:"max_output,omitempty" yaml:"max_output,omitempty"`
	CostDetail       *LaneCostDetail        `json:"cost_detail,omitempty" yaml:"cost_detail,omitempty"`
	RelativeCostHint map[string]float64     `json:"relative_cost_hint,omitempty" yaml:"relative_cost_hint,omitempty"`
	CapacitySource   *LaneCapacitySourceRef `json:"capacity_source,omitempty" yaml:"capacity_source,omitempty"`
	Aliases          []string               `json:"aliases,omitempty" yaml:"aliases,omitempty"`
}

type ModelCapabilitiesManifest struct {
	Version           int                             `json:"version" yaml:"version"`
	Purpose           string                          `json:"purpose" yaml:"purpose"`
	Rules             []string                        `json:"rules" yaml:"rules"`
	CapabilityClasses []string                        `json:"capability_classes" yaml:"capability_classes"`
	Models            map[string]ModelCapabilityEntry `json:"models" yaml:"models"`
	RoutingDefaults   map[string][]string             `json:"routing_defaults" yaml:"routing_defaults"`
}

type ModelCapabilitiesProjection struct {
	SchemaVersion       string                          `json:"schema_version"`
	GeneratedAt         string                          `json:"generated_at"`
	AuthorityRef        string                          `json:"authority_ref"`
	AuthorityPath       string                          `json:"authority_path"`
	SourceAuthorityID   string                          `json:"source_authority_id,omitempty"`
	SourceRevision      string                          `json:"source_revision,omitempty"`
	ModelCount          int                             `json:"model_count"`
	CapabilityClasses   []string                        `json:"capability_classes"`
	RoutingDefaultCount int                             `json:"routing_default_count"`
	Lanes               []ModelCapabilityProjectionLane `json:"lanes"`
}

type ModelCapabilityProjectionLane struct {
	ModelID           string                           `json:"model_id"`
	Label             string                           `json:"label"`
	Lane              string                           `json:"lane"`
	Harness           string                           `json:"harness,omitempty"`
	ProviderRoute     string                           `json:"provider_route,omitempty"`
	ProviderEndpoint  string                           `json:"provider_endpoint,omitempty"`
	ProviderRouteKind string                           `json:"provider_route_kind,omitempty"`
	Resolution        LaneResolution                   `json:"resolution"`
	EffortDefault     string                           `json:"effort_default,omitempty"`
	CostClass         string                           `json:"cost_class,omitempty"`
	Tier              string                           `json:"tier"`
	PacketCeiling     string                           `json:"packet_ceiling,omitempty"`
	Capabilities      []ModelCapabilityProjectionClaim `json:"capabilities,omitempty"`
	RoutingDefaults   []string                         `json:"routing_defaults,omitempty"`
	Notes             string                           `json:"notes,omitempty"`
	// OPTIONAL LaneTuple facts — mirror of ModelCapabilityEntry.
	ContextWindow    int                    `json:"context_window,omitempty"`
	MaxOutput        int                    `json:"max_output,omitempty"`
	CostDetail       *LaneCostDetail        `json:"cost_detail,omitempty"`
	RelativeCostHint map[string]float64     `json:"relative_cost_hint,omitempty"`
	CapacitySource   *LaneCapacitySourceRef `json:"capacity_source,omitempty"`
	Aliases          []string               `json:"aliases,omitempty"`
}

type ModelCapabilityProjectionClaim struct {
	CapabilityClass string `json:"capability_class"`
	Level           string `json:"level"`
	Evidence        string `json:"evidence,omitempty"`
	GateRole        string `json:"gate_role"`
}

type ModelCapabilitiesImportReport struct {
	AuthorityRef         string                           `json:"authority_ref"`
	AuthorityPath        string                           `json:"authority_path"`
	SourceAuthorityID    string                           `json:"source_authority_id"`
	SourceAuthority      ModelCapabilitiesSourceAuthority `json:"source_authority"`
	ModelCount           int                              `json:"model_count"`
	CapabilityClassCount int                              `json:"capability_class_count"`
	RoutingDefaultCount  int                              `json:"routing_default_count"`
}

type ModelCapabilitiesSourceAuthority struct {
	SchemaVersion       string   `json:"schema_version"`
	ID                  string   `json:"id"`
	CreatedAt           string   `json:"created_at"`
	SourcePath          string   `json:"source_path"`
	SourceRevision      string   `json:"source_revision"`
	InputRevision       string   `json:"input_revision,omitempty"`
	ImportedVia         string   `json:"imported_via"`
	AuthorityRef        string   `json:"authority_ref"`
	AuthorityStatus     string   `json:"authority_status"`
	SourceRole          string   `json:"source_role"`
	Retires             string   `json:"retires"`
	RetirementCondition string   `json:"retirement_condition"`
	Rollback            string   `json:"rollback"`
	ModelIDs            []string `json:"model_ids"`
	CapabilityClasses   []string `json:"capability_classes"`
	RoutingDefaultCount int      `json:"routing_default_count"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

type CapabilityRoutePlanOptions struct {
	PrivacySensitive            bool
	SuppressPendingObservations bool
	// SuppressRefusalEvidence makes planning projection-only: hard capacity
	// walls remain visible in Omitted and Warnings but append no route evidence.
	SuppressRefusalEvidence bool
	ExpectedMinutes         int
	Confirm                 bool
	// CapacityConfig injects source paths for tests and alternate runtime projections. Nil uses
	// the production defaults rooted at the supplied Compound home.
	CapacityConfig *LaneCapacityConfig
	// LatencyClass, when set (interactive|batch|overnight), enforces the worth-the-wait rule:
	// a lane whose effective latency class is slower than the required class is vetoed (omitted)
	// for this plan, while the same lane is admitted for a class that can afford the wait. Empty
	// keeps today's behavior byte-identical (no latency filtering).
	LatencyClass string
	// LatencyBudget injects the operator worth-the-wait SLA used to classify measured probes.
	// Nil loads registries/latency-budget.json (or the built-in operator-SLA default).
	LatencyBudget *LatencyBudget
}

type CapabilityRoutePlan struct {
	ReadOnly          bool   `json:"read_only,omitempty"`
	SchemaVersion     string `json:"schema_version"`
	ID                string `json:"id"`
	CreatedAt         string `json:"created_at"`
	Status            string `json:"status"`
	AuthorityRef      string `json:"authority_ref"`
	AuthorityPath     string `json:"authority_path"`
	SourceAuthorityID string `json:"source_authority_id,omitempty"`
	SourceRevision    string `json:"source_revision,omitempty"`
	CapabilityClass   string `json:"capability_class"`
	PrivacySensitive  bool   `json:"privacy_sensitive"`
	RoutingRule       string `json:"routing_rule"`
	// RequiredLatencyClass is the task worth-the-wait class this plan was filtered against
	// (interactive|batch|overnight). Empty means no latency filtering ran.
	RequiredLatencyClass string `json:"required_latency_class,omitempty"`
	// LatencyBudgetSource names where the measured-speed SLA came from (default vs override),
	// so a speed demotion is never a silent number.
	LatencyBudgetSource string                    `json:"latency_budget_source,omitempty"`
	LaneJustification   []string                  `json:"lane_justification"`
	Chain               []CapabilityRouteLane     `json:"chain"`
	Omitted             []CapabilityRouteLane     `json:"omitted,omitempty"`
	PendingObservations []PendingRouteObservation `json:"pending_observations,omitempty"`
	Warnings            []string                  `json:"warnings,omitempty"`
	// CapacityConfirmationRequired means one or more otherwise-capable subscription lanes were
	// withheld because their capacity was unknown or reset inside the expected work window.
	CapacityConfirmationRequired bool `json:"capacity_confirmation_required,omitempty"`
}

type CapabilityRouteLane struct {
	Position            int            `json:"position"`
	ModelID             string         `json:"model_id"`
	Lane                string         `json:"lane"`
	Harness             string         `json:"harness,omitempty"`
	ProviderRoute       string         `json:"provider_route,omitempty"`
	ProviderRouteKind   string         `json:"provider_route_kind,omitempty"`
	Resolution          LaneResolution `json:"resolution"`
	EffortDefault       string         `json:"effort_default,omitempty"`
	CostClass           string         `json:"cost_class,omitempty"`
	Tier                string         `json:"tier"`
	PacketCeiling       string         `json:"packet_ceiling,omitempty"`
	ClaimLevel          string         `json:"claim_level,omitempty"`
	ClaimEvidence       string         `json:"claim_evidence,omitempty"`
	GateRole            string         `json:"gate_role"`
	SupervisionRequired bool           `json:"supervision_required,omitempty"`
	Justification       string         `json:"justification"`
	OmitReason          string         `json:"omit_reason,omitempty"`
	// AccountProjectionRevision is the imported account projection's source_revision, stamped on
	// every lane the account-admission gate evaluated (see account_projection.go). Empty when the
	// gate did not run (no/stale projection) or the lane maps to no gated provider.
	AccountProjectionRevision string `json:"account_projection_revision,omitempty"`
	// AccountAdmission is the account-admission gate's decision for this lane: "omitted" (zero
	// healthy accounts for its provider), "admitted" (mapped provider, at least one healthy
	// account, or no data for it), or "ungated" (harness maps to no gated provider). Empty when the
	// gate did not run.
	AccountAdmission string `json:"account_admission,omitempty"`
	// Capacity carries the exact source-derived state used by the veto. It annotates both
	// admitted and omitted subscription lanes and is absent for non-subscription lanes.
	Capacity *LaneCapacity `json:"capacity,omitempty"`
	// SpeedAdjustment records a measured-speed role change derived from lane-speed probe
	// evidence (never from provider marketing, never a promotion). Present only when a probe's
	// measured latency class is slower than the lane's declared class; GateRole above is then
	// the ADJUSTED role and PriorGateRole holds what it was.
	SpeedAdjustment *LaneSpeedAdjustment `json:"speed_adjustment,omitempty"`
}

// PendingRouteObservation is read-only review debt visible to route planners.
// It points at existing route_observation candidates; it never mutates
// model-router, gateway, provider config, or claims.
type PendingRouteObservation struct {
	CandidateID     string   `json:"candidate_id"`
	Subject         string   `json:"subject,omitempty"`
	Statement       string   `json:"statement,omitempty"`
	Detail          string   `json:"detail,omitempty"`
	Verdict         string   `json:"verdict,omitempty"`
	EvidenceRefs    []string `json:"evidence_refs,omitempty"`
	MatchReasons    []string `json:"match_reasons"`
	NoPromoteID     string   `json:"no_promote_id,omitempty"`
	NoPromoteBasis  string   `json:"no_promote_basis,omitempty"`
	OwnerTargetKind string   `json:"owner_target_kind,omitempty"`
	OwnerTargetRef  string   `json:"owner_target_ref,omitempty"`
	ReviewCommand   string   `json:"review_command"`
}

func ImportModelCapabilitiesYAML(homePath, sourcePath string, now time.Time) (ModelCapabilitiesImportReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return ModelCapabilitiesImportReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if strings.TrimSpace(sourcePath) == "" {
		return ModelCapabilitiesImportReport{}, fmt.Errorf("source path required")
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return ModelCapabilitiesImportReport{}, fmt.Errorf("read model capabilities %s: %w", sourcePath, err)
	}
	manifest, err := DecodeModelCapabilitiesYAML(data)
	if err != nil {
		return ModelCapabilitiesImportReport{}, err
	}
	if err := validateModelCapabilities(manifest); err != nil {
		return ModelCapabilitiesImportReport{}, err
	}
	var report ModelCapabilitiesImportReport
	err = homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		installedData := data
		installedManifest := manifest
		if existing, _, _, loadErr := loadModelCapabilitiesSnapshot(homePath); loadErr == nil {
			installedManifest = mergeModelCapabilities(existing, manifest)
			if err := validateModelCapabilities(installedManifest); err != nil {
				return err
			}
			installedData, err = yaml.Marshal(installedManifest)
			if err != nil {
				return fmt.Errorf("marshal merged model capabilities: %w", err)
			}
		} else if !os.IsNotExist(loadErr) {
			return fmt.Errorf("load existing model capabilities authority: %w", loadErr)
		}
		var installErr error
		report, installErr = installModelCapabilities(homePath, sourcePath, data, installedData, installedManifest, now)
		return installErr
	})
	return report, err
}

func installModelCapabilities(homePath, sourcePath string, inputData, installedData []byte, manifest ModelCapabilitiesManifest, now time.Time) (ModelCapabilitiesImportReport, error) {
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return ModelCapabilitiesImportReport{}, err
	}
	authorityPath := ModelCapabilitiesPath(homePath)
	if err := os.WriteFile(authorityPath, installedData, 0o644); err != nil {
		return ModelCapabilitiesImportReport{}, err
	}
	revision := "sha256:" + digestHex(installedData)
	inputRevision := "sha256:" + digestHex(inputData)
	rec := ModelCapabilitiesSourceAuthority{
		SchemaVersion:       ModelCapabilitiesSourceAuthoritySchema,
		ID:                  modelCapabilitiesSourceAuthorityID(sourcePath, revision),
		CreatedAt:           rfc3339(now),
		SourcePath:          sourcePath,
		SourceRevision:      revision,
		InputRevision:       inputRevision,
		ImportedVia:         modelCapabilitiesImportCommandName,
		AuthorityRef:        ModelCapabilitiesAuthorityRef,
		AuthorityStatus:     "compound_primary",
		SourceRole:          "evidence_only_input_after_import",
		Retires:             "source_view_authority:" + sourcePath,
		RetirementCondition: "compound route plan reads compound://registries/model-router from COMPOUND_HOME; OS generated view remains rollback/evidence input",
		Rollback:            "re-import source model-capabilities.yaml or remove COMPOUND_HOME registries/model-capabilities.yaml",
		ModelIDs:            modelCapabilityIDs(manifest),
		CapabilityClasses:   sortedStrings(manifest.CapabilityClasses),
		RoutingDefaultCount: len(manifest.RoutingDefaults),
		EvidenceRefs:        sortedStrings([]string{revision, inputRevision, ModelCapabilitiesAuthorityRef}),
	}
	if err := appendModelCapabilitiesSourceAuthority(homePath, rec); err != nil {
		return ModelCapabilitiesImportReport{}, err
	}
	return ModelCapabilitiesImportReport{
		AuthorityRef:         ModelCapabilitiesAuthorityRef,
		AuthorityPath:        authorityPath,
		SourceAuthorityID:    rec.ID,
		SourceAuthority:      rec,
		ModelCount:           len(manifest.Models),
		CapabilityClassCount: len(manifest.CapabilityClasses),
		RoutingDefaultCount:  len(manifest.RoutingDefaults),
	}, nil
}

// mergeModelCapabilities applies bridge imports additively. Incoming facts
// update matching ids while omitted tuple facts, rows, and route positions
// survive. Deletion requires a future explicit retirement operation; import
// is never an implicit destructive command.
func mergeModelCapabilities(existing, incoming ModelCapabilitiesManifest) ModelCapabilitiesManifest {
	merged := incoming
	merged.Models = make(map[string]ModelCapabilityEntry, len(existing.Models)+len(incoming.Models))
	for id, entry := range existing.Models {
		merged.Models[id] = entry
	}
	for id, entry := range incoming.Models {
		if previous, ok := existing.Models[id]; ok {
			entry = mergeModelCapabilityEntry(previous, entry)
		}
		merged.Models[id] = entry
	}
	merged.Rules = mergeOrderedStrings(existing.Rules, incoming.Rules)
	merged.CapabilityClasses = mergeOrderedStrings(existing.CapabilityClasses, incoming.CapabilityClasses)
	merged.RoutingDefaults = make(map[string][]string, len(existing.RoutingDefaults)+len(incoming.RoutingDefaults))
	for class, lanes := range existing.RoutingDefaults {
		merged.RoutingDefaults[class] = append([]string(nil), lanes...)
	}
	for class, lanes := range incoming.RoutingDefaults {
		merged.RoutingDefaults[class] = mergeOrderedStrings(existing.RoutingDefaults[class], lanes)
	}
	return merged
}

// mergeModelCapabilityEntry lets an older bridge refresh the facts it still
// owns without clearing newer Compound-owned optional tuple facts merely
// because the bridge has no fields for them. Explicit retirement remains the
// only deletion path.
func mergeModelCapabilityEntry(existing, incoming ModelCapabilityEntry) ModelCapabilityEntry {
	merged := existing
	if strings.TrimSpace(incoming.Lane) != "" {
		merged.Lane = incoming.Lane
	}
	if strings.TrimSpace(incoming.Harness) != "" {
		merged.Harness = incoming.Harness
	}
	if strings.TrimSpace(incoming.ProviderRoute) != "" {
		merged.ProviderRoute = incoming.ProviderRoute
	}
	if strings.TrimSpace(incoming.ProviderEndpoint) != "" {
		merged.ProviderEndpoint = incoming.ProviderEndpoint
	}
	if strings.TrimSpace(incoming.ProviderRouteKind) != "" {
		merged.ProviderRouteKind = incoming.ProviderRouteKind
	}
	if strings.TrimSpace(incoming.Resolution.Mode) != "" || len(incoming.Resolution.Targets) > 0 {
		merged.Resolution = incoming.Resolution
	}
	if strings.TrimSpace(incoming.EffortDefault) != "" {
		merged.EffortDefault = incoming.EffortDefault
	}
	if strings.TrimSpace(incoming.CostClass) != "" {
		merged.CostClass = incoming.CostClass
	}
	if strings.TrimSpace(incoming.Tier) != "" {
		merged.Tier = incoming.Tier
	}
	if strings.TrimSpace(incoming.PacketCeiling) != "" {
		merged.PacketCeiling = incoming.PacketCeiling
	}
	if strings.TrimSpace(incoming.Notes) != "" {
		merged.Notes = incoming.Notes
	}
	if incoming.ContextWindow > 0 {
		merged.ContextWindow = incoming.ContextWindow
	}
	if incoming.MaxOutput > 0 {
		merged.MaxOutput = incoming.MaxOutput
	}
	if incoming.CostDetail != nil {
		merged.CostDetail = incoming.CostDetail
	}
	if len(incoming.RelativeCostHint) > 0 {
		merged.RelativeCostHint = incoming.RelativeCostHint
	}
	if incoming.CapacitySource != nil {
		merged.CapacitySource = incoming.CapacitySource
	}
	if len(incoming.Aliases) > 0 {
		merged.Aliases = append([]string(nil), incoming.Aliases...)
	}
	merged.Capabilities = make(map[string]CapabilityClaim, len(existing.Capabilities)+len(incoming.Capabilities))
	for class, claim := range existing.Capabilities {
		merged.Capabilities[class] = claim
	}
	for class, claim := range incoming.Capabilities {
		merged.Capabilities[class] = claim
	}
	return merged
}

func mergeOrderedStrings(primary, preserved []string) []string {
	out := make([]string, 0, len(primary)+len(preserved))
	seen := make(map[string]struct{}, len(primary)+len(preserved))
	for _, values := range [][]string{primary, preserved} {
		for _, value := range values {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

func DecodeModelCapabilitiesYAML(data []byte) (ModelCapabilitiesManifest, error) {
	var manifest ModelCapabilitiesManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return ModelCapabilitiesManifest{}, fmt.Errorf("parse model capabilities: %w", err)
	}
	return manifest, nil
}

func LoadModelCapabilitiesFromHome(homePath string) (ModelCapabilitiesManifest, string, error) {
	manifest, path, _, err := loadModelCapabilitiesSnapshot(homePath)
	return manifest, path, err
}

// LoadModelCapabilitiesSnapshot returns the validated authority plus its
// content revision so execution preflight can bind tuple lookup to a plan.
func LoadModelCapabilitiesSnapshot(homePath string) (ModelCapabilitiesManifest, string, string, error) {
	return loadModelCapabilitiesSnapshot(homePath)
}

func loadModelCapabilitiesSnapshot(homePath string) (ModelCapabilitiesManifest, string, string, error) {
	path := ModelCapabilitiesPath(homePath)
	data, err := os.ReadFile(path)
	if err != nil {
		return ModelCapabilitiesManifest{}, path, "", err
	}
	manifest, err := DecodeModelCapabilitiesYAML(data)
	if err != nil {
		return ModelCapabilitiesManifest{}, path, "", err
	}
	if err := validateModelCapabilities(manifest); err != nil {
		return ModelCapabilitiesManifest{}, path, "", err
	}
	return manifest, path, "sha256:" + digestHex(data), nil
}

func LoadModelCapabilitiesSourceAuthority(homePath string) ([]ModelCapabilitiesSourceAuthority, error) {
	data, err := os.ReadFile(ModelCapabilitiesSourceAuthorityPath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []ModelCapabilitiesSourceAuthority
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec ModelCapabilitiesSourceAuthority
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse model capabilities source authority line %d: %w", i+1, err)
		}
		records = append(records, rec)
	}
	return records, nil
}

func ProjectModelCapabilities(homePath string, now time.Time) (ModelCapabilitiesProjection, error) {
	manifest, authorityPath, authorityRevision, err := loadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return ModelCapabilitiesProjection{}, fmt.Errorf("load model capabilities authority: %w", err)
	}
	sourceAuthorityID, sourceRevision, err := latestModelCapabilitiesSource(homePath, authorityRevision)
	if err != nil {
		return ModelCapabilitiesProjection{}, err
	}

	projection := ModelCapabilitiesProjection{
		SchemaVersion:       ModelCapabilitiesProjectionSchema,
		GeneratedAt:         rfc3339(now),
		AuthorityRef:        ModelCapabilitiesAuthorityRef,
		AuthorityPath:       authorityPath,
		SourceAuthorityID:   sourceAuthorityID,
		SourceRevision:      sourceRevision,
		ModelCount:          len(manifest.Models),
		CapabilityClasses:   sortedStrings(manifest.CapabilityClasses),
		RoutingDefaultCount: len(manifest.RoutingDefaults),
		Lanes:               []ModelCapabilityProjectionLane{},
	}

	for _, modelID := range modelCapabilityIDs(manifest) {
		model := manifest.Models[modelID]
		label := strings.TrimSpace(model.Lane)
		if label == "" {
			label = modelID
		}
		lane := ModelCapabilityProjectionLane{
			ModelID:           modelID,
			Label:             label,
			Lane:              strings.TrimSpace(model.Lane),
			Harness:           strings.TrimSpace(model.Harness),
			ProviderRoute:     strings.TrimSpace(model.ProviderRoute),
			ProviderEndpoint:  sanitizeModelCapabilitiesProjectionEndpoint(model.ProviderEndpoint),
			ProviderRouteKind: strings.TrimSpace(model.ProviderRouteKind),
			Resolution:        model.Resolution,
			EffortDefault:     strings.TrimSpace(model.EffortDefault),
			CostClass:         strings.TrimSpace(model.CostClass),
			Tier:              strings.TrimSpace(model.Tier),
			PacketCeiling:     strings.TrimSpace(model.PacketCeiling),
			RoutingDefaults:   routingDefaultsForModel(manifest.RoutingDefaults, modelID),
			Notes:             strings.TrimSpace(model.Notes),
			ContextWindow:     model.ContextWindow,
			MaxOutput:         model.MaxOutput,
			CostDetail:        model.CostDetail,
			RelativeCostHint:  model.RelativeCostHint,
			CapacitySource:    model.CapacitySource,
			Aliases:           model.Aliases,
		}
		for _, class := range sortedCapabilityClaimClasses(model.Capabilities) {
			claim := model.Capabilities[class]
			lane.Capabilities = append(lane.Capabilities, ModelCapabilityProjectionClaim{
				CapabilityClass: class,
				Level:           strings.TrimSpace(claim.Level),
				Evidence:        strings.TrimSpace(claim.Evidence),
				GateRole:        gateRole(class, model.Tier, strings.TrimSpace(claim.Level)),
			})
		}
		projection.Lanes = append(projection.Lanes, lane)
	}

	return projection, nil
}

func PlanCapabilityRoute(homePath, capabilityClass string, opts CapabilityRoutePlanOptions, now time.Time) (CapabilityRoutePlan, error) {
	capabilityClass = strings.TrimSpace(capabilityClass)
	if capabilityClass == "" {
		return CapabilityRoutePlan{}, fmt.Errorf("capability class required")
	}
	cacheKey := planCapabilityRouteCacheKey(homePath, capabilityClass, opts, planCapabilityRouteCacheBuckets(now))
	if cachedPlan, cachedErr, ok := planCapabilityRouteCacheLookup(cacheKey); ok {
		planCapabilityRouteCacheRecordHit()
		return cachedPlan, cachedErr
	}
	planCapabilityRouteCacheRecordMiss()
	manifest, authorityPath, authorityRevision, err := loadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return CapabilityRoutePlan{}, fmt.Errorf("load model capabilities authority: %w", err)
	}
	defaults, ok := manifest.RoutingDefaults[capabilityClass]
	if !ok || len(defaults) == 0 {
		// Cache the negative answer too so a class with no routing_defaults is
		// reported in constant time on every retry; otherwise the second scan
		// re-parses YAML and re-validates the manifest for nothing.
		missingErr := fmt.Errorf("capability class %q has no routing_defaults entry", capabilityClass)
		planCapabilityRouteCacheStore(cacheKey, CapabilityRoutePlan{}, missingErr)
		return CapabilityRoutePlan{}, missingErr
	}
	sourceAuthorityID, sourceRevision, err := latestModelCapabilitiesSource(homePath, authorityRevision)
	if err != nil {
		return CapabilityRoutePlan{}, err
	}
	plan := CapabilityRoutePlan{
		ReadOnly:          opts.SuppressRefusalEvidence && opts.SuppressPendingObservations,
		SchemaVersion:     CapabilityRoutePlanSchemaVersion,
		ID:                autonomyproof.RoutePlanID(capabilityClass, opts.PrivacySensitive, authorityPath, sourceRevision),
		CreatedAt:         rfc3339(now),
		Status:            "planned",
		AuthorityRef:      ModelCapabilitiesAuthorityRef,
		AuthorityPath:     authorityPath,
		SourceAuthorityID: sourceAuthorityID,
		SourceRevision:    sourceRevision,
		CapabilityClass:   capabilityClass,
		PrivacySensitive:  opts.PrivacySensitive,
		RoutingRule:       "routing_defaults[" + capabilityClass + "]",
	}
	// Account-admission gate: refuse a subscription lane whose provider (via switchback's imported,
	// identity-free account projection) has zero healthy fresh accounts, so work fails over BEFORE
	// hitting a lane with no live credential. The projection is loaded once; absent -> no gate
	// (byte-identical fail-open, F4); present-but-stale (>6h by its own imported_at) -> no gate,
	// warned (F7). See account_projection.go.
	accountGate := resolveAccountAdmissionGate(homePath, now)
	capacityConfig := DefaultLaneCapacityConfig(homePath)
	if opts.CapacityConfig != nil {
		capacityConfig = *opts.CapacityConfig
	}
	capacityConfig = normalizeLaneCapacityConfig(capacityConfig, homePath)
	capacityConfig.SuppressRefusalEvidence = opts.SuppressRefusalEvidence
	if opts.ExpectedMinutes > 0 {
		capacityConfig.ExpectedMinutes = opts.ExpectedMinutes
	}
	// Worth-the-wait inputs: measured-speed receipts + operator SLA budget. Both are clean
	// no-ops on a fresh home (no probes -> no demotion; empty LatencyClass -> no veto), so
	// existing plans stay byte-identical.
	requiredLatency, requiredLatencyOK := NormalizeLatencyClass(opts.LatencyClass)
	if strings.TrimSpace(opts.LatencyClass) != "" && !requiredLatencyOK {
		return CapabilityRoutePlan{}, fmt.Errorf("unknown latency class %q (want interactive|batch|overnight)", opts.LatencyClass)
	}
	plan.RequiredLatencyClass = string(requiredLatency)
	latencyBudget := DefaultLatencyBudget()
	budgetSource := "default_operator_sla"
	if opts.LatencyBudget != nil {
		if err := opts.LatencyBudget.validate(); err != nil {
			return CapabilityRoutePlan{}, err
		}
		latencyBudget = *opts.LatencyBudget
		budgetSource = "options"
	} else {
		loaded, source, err := LoadLatencyBudget(homePath)
		if err != nil {
			return CapabilityRoutePlan{}, err
		}
		latencyBudget = loaded
		budgetSource = source
	}
	plan.LatencyBudgetSource = budgetSource
	speedProbes, err := LoadLaneSpeedProbes(homePath)
	if err != nil {
		return CapabilityRoutePlan{}, err
	}
	for i, modelID := range defaults {
		model := manifest.Models[modelID]
		lane := capabilityRouteLane(capabilityClass, modelID, model, i)
		// Measured speed can only DEMOTE (never auto-promote, never from marketing). Stamped on
		// the lane before any filter so an omitted lane still carries its demotion receipt.
		effectiveLatency, speedWarning := laneEffectiveLatency(&lane, model, speedProbes, latencyBudget)
		if speedWarning != "" {
			plan.Warnings = append(plan.Warnings, speedWarning)
		}
		if opts.PrivacySensitive && model.Tier != "local-batch" {
			lane.OmitReason = "privacy-sensitive route requires local-batch tier"
			plan.Omitted = append(plan.Omitted, lane)
			continue
		}
		// Worth-the-wait veto: a lane slower than the task's required latency class is omitted
		// for interactive work but admitted when the task can afford the wait.
		if omit, reason := laneLatencyVeto(lane, effectiveLatency, requiredLatency); omit {
			lane.OmitReason = reason
			plan.Omitted = append(plan.Omitted, lane)
			plan.Warnings = append(plan.Warnings, reason)
			continue
		}
		accountKey, accountMapped := laneAccountProviderKey(lane)
		if accountGate.active {
			if accountMapped {
				lane.AccountProjectionRevision = accountGate.projection.SourceRevision
				if entry, present := accountGate.projection.Providers[accountKey]; present && entry.HealthyAccountCount == 0 {
					lane.AccountAdmission = AccountAdmissionOmitted
				} else {
					lane.AccountAdmission = AccountAdmissionAdmitted
				}
			} else {
				lane.AccountAdmission = AccountAdmissionUngated
			}
		}
		var capacityDecision *LaneCapacityDecision
		if model.CapacitySource != nil {
			decision, err := GuardDeclaredLaneCapacityWithConfig(
				homePath,
				model.ProviderRoute,
				capabilityClass,
				*model.CapacitySource,
				now,
				capacityConfig.ExpectedMinutes,
				opts.Confirm,
				defaults,
				opts.SuppressRefusalEvidence,
				capacityConfig,
			)
			if err != nil {
				return CapabilityRoutePlan{}, fmt.Errorf("guard lane %s declared capacity: %w", modelID, err)
			}
			capacityDecision = &decision
		} else if accountMapped {
			decision, err := GuardLaneCapacity(homePath, accountKey, capabilityClass, now, opts.Confirm, defaults, capacityConfig)
			if err != nil {
				return CapabilityRoutePlan{}, fmt.Errorf("guard lane %s capacity: %w", modelID, err)
			}
			capacityDecision = &decision
		}
		if capacityDecision != nil {
			capacity := capacityDecision.Capacity
			lane.Capacity = &capacity
			switch {
			case capacityDecision.Refused:
				lane.OmitReason = capacityDecision.Message
				plan.Omitted = append(plan.Omitted, lane)
				plan.Warnings = append(plan.Warnings, capacityDecision.Message)
				continue
			case capacityDecision.ConfirmationRequired:
				lane.OmitReason = capacityDecision.Message
				plan.Omitted = append(plan.Omitted, lane)
				plan.CapacityConfirmationRequired = true
				plan.Warnings = append(plan.Warnings, capacityDecision.Message)
				continue
			case strings.TrimSpace(capacityDecision.Message) != "":
				plan.Warnings = append(plan.Warnings, capacityDecision.Message)
			}
		}
		if lane.AccountAdmission == AccountAdmissionOmitted {
			entry := accountGate.projection.Providers[accountKey]
			lane.OmitReason = fmt.Sprintf("no healthy accounts for provider=%s (total=%d, revision=%s)", accountKey, entry.TotalAccountCount, lane.AccountProjectionRevision)
			plan.Omitted = append(plan.Omitted, lane)
			continue
		}
		plan.Chain = append(plan.Chain, lane)
		plan.LaneJustification = append(plan.LaneJustification, lane.Justification)
	}
	if accountGate.warning != "" {
		plan.Warnings = append(plan.Warnings, accountGate.warning)
	}
	if opts.PrivacySensitive && len(plan.Omitted) > 0 {
		plan.Warnings = append(plan.Warnings, "privacy-sensitive plan omitted non-local-batch lanes")
	}
	for _, lane := range plan.Chain {
		if lane.ClaimLevel == "" {
			plan.Warnings = append(plan.Warnings, "routing default references "+lane.ModelID+" without explicit "+capabilityClass+" claim")
		}
		if lane.SupervisionRequired {
			plan.Warnings = append(plan.Warnings, "lane "+lane.ModelID+" claim is assumed; supervised-only until promoted by evidence")
		}
		if lane.GateRole == "tripwire_only" {
			plan.Warnings = append(plan.Warnings, "free judge/critique lane "+lane.ModelID+" may object but must not gate verdicts")
		}
	}
	if !opts.SuppressPendingObservations {
		pending, err := pendingRouteObservations(homePath, capabilityClass, defaults, now)
		if err != nil {
			return CapabilityRoutePlan{}, err
		}
		plan.PendingObservations = pending
		if len(pending) > 0 {
			plan.Warnings = append(plan.Warnings, "pending route observations require owner review before routing policy changes")
		}
	}
	plan.Warnings = sortedStrings(plan.Warnings)
	if len(plan.Chain) == 0 {
		if plan.CapacityConfirmationRequired {
			plan.Status = "capacity_confirmation_required"
			plan.Warnings = append(plan.Warnings, "no lane remains until capacity is confirmed")
		} else {
			plan.Status = "blocked_no_lane"
			plan.Warnings = append(plan.Warnings, "no lane remains after policy filters")
		}
	}
	planCapabilityRouteCacheStore(cacheKey, plan, nil)
	return plan, nil
}

func ModelCapabilitiesPath(homePath string) string {
	return filepath.Join(homePath, "registries", modelCapabilitiesFile)
}

func ModelCapabilitiesSourceAuthorityPath(homePath string) string {
	return filepath.Join(homePath, "registries", modelCapabilitiesSourceAuthority)
}

func validProviderRouteKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case ProviderRouteKindProviderClass, ProviderRouteKindDirectModel:
		return true
	default:
		return false
	}
}

func validateModelCapabilities(manifest ModelCapabilitiesManifest) error {
	var errs []string
	if manifest.Version == 0 {
		errs = append(errs, "version required")
	}
	if len(manifest.Models) == 0 {
		errs = append(errs, "models required")
	}
	if len(manifest.RoutingDefaults) == 0 {
		errs = append(errs, "routing_defaults required")
	}
	for id, model := range manifest.Models {
		if strings.TrimSpace(id) == "" {
			errs = append(errs, "model id required")
		}
		if strings.TrimSpace(model.Lane) == "" {
			errs = append(errs, "model "+id+" lane required")
		}
		if strings.TrimSpace(model.Tier) == "" {
			errs = append(errs, "model "+id+" tier required")
		} else if !autonomyproof.ValidTier(model.Tier) {
			errs = append(errs, "model "+id+" has unknown tier "+model.Tier)
		}
		if kind := strings.TrimSpace(model.ProviderRouteKind); kind != "" && !validProviderRouteKind(kind) {
			errs = append(errs, "model "+id+" has unknown provider_route_kind "+kind)
		}
		if strings.TrimSpace(model.Resolution.Mode) != "" || len(model.Resolution.Targets) > 0 {
			errs = append(errs, validateLaneResolution(id, model.Resolution)...)
		}
		if source := model.CapacitySource; source != nil {
			if strings.TrimSpace(source.Kind) != "codexbar" {
				errs = append(errs, "model "+id+" has unknown capacity source kind "+source.Kind)
			}
			if source.FreshnessMinutes <= 0 {
				errs = append(errs, "model "+id+" capacity source freshness_minutes must be positive")
			}
			if strings.TrimSpace(source.Ref) == "" {
				errs = append(errs, "model "+id+" capacity source ref required")
			} else if strings.TrimSpace(source.Kind) == "codexbar" {
				if _, err := codexBarProviderFromRef(source.Ref); err != nil {
					errs = append(errs, "model "+id+" "+err.Error())
				}
			}
		}
		for class, claim := range model.Capabilities {
			if strings.TrimSpace(claim.Level) == "" {
				errs = append(errs, "model "+id+" capability "+class+" level required")
			} else if !autonomyproof.ValidClaimLevel(claim.Level) {
				errs = append(errs, "model "+id+" capability "+class+" has unknown claim level "+claim.Level)
			}
		}
	}
	for class, chain := range manifest.RoutingDefaults {
		if strings.TrimSpace(class) == "" {
			errs = append(errs, "routing default class required")
		}
		if len(chain) == 0 {
			errs = append(errs, "routing default "+class+" chain required")
		}
		for _, modelID := range chain {
			if _, ok := manifest.Models[modelID]; !ok {
				errs = append(errs, "routing default "+class+" references unknown model "+modelID)
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("model capabilities invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validateLaneResolution(modelID string, resolution LaneResolution) []string {
	mode := strings.TrimSpace(resolution.Mode)
	var errs []string
	switch mode {
	case "adaptive":
		if len(resolution.Targets) != 0 {
			errs = append(errs, "model "+modelID+" adaptive resolution must not declare concrete targets")
		}
	case "direct":
		if len(resolution.Targets) == 0 {
			errs = append(errs, "model "+modelID+" direct resolution requires at least one target")
		}
		seen := map[string]bool{}
		for i, target := range resolution.Targets {
			provider := strings.TrimSpace(target.Provider)
			model := strings.TrimSpace(target.Model)
			if provider == "" || model == "" {
				errs = append(errs, fmt.Sprintf("model %s resolution target %d requires provider and model", modelID, i))
				continue
			}
			key := provider + "\x00" + model
			if seen[key] {
				errs = append(errs, fmt.Sprintf("model %s resolution target %d duplicates provider/model identity", modelID, i))
			}
			seen[key] = true
		}
	default:
		errs = append(errs, "model "+modelID+" has unknown resolution mode "+mode)
	}
	return errs
}

func appendModelCapabilitiesSourceAuthority(homePath string, rec ModelCapabilitiesSourceAuthority) error {
	path := ModelCapabilitiesSourceAuthorityPath(homePath)
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

func latestModelCapabilitiesSource(homePath, authorityRevision string) (id, revision string, err error) {
	records, err := LoadModelCapabilitiesSourceAuthority(homePath)
	if err != nil {
		return "", "", err
	}
	if len(records) == 0 {
		return "", "", nil
	}
	latest := records[len(records)-1]
	if latest.SourceRevision != authorityRevision {
		return "", "", fmt.Errorf("model-router source authority revision %q does not match parsed authority %q", latest.SourceRevision, authorityRevision)
	}
	return latest.ID, latest.SourceRevision, nil
}

func capabilityRouteLane(class, modelID string, model ModelCapabilityEntry, position int) CapabilityRouteLane {
	claim := model.Capabilities[class]
	level := strings.TrimSpace(claim.Level)
	role := gateRole(class, model.Tier, level)
	lane := CapabilityRouteLane{
		Position:          position,
		ModelID:           modelID,
		Lane:              model.Lane,
		Harness:           strings.TrimSpace(model.Harness),
		ProviderRoute:     strings.TrimSpace(model.ProviderRoute),
		ProviderRouteKind: strings.TrimSpace(model.ProviderRouteKind),
		Resolution:        model.Resolution,
		EffortDefault:     strings.TrimSpace(model.EffortDefault),
		CostClass:         strings.TrimSpace(model.CostClass),
		Tier:              model.Tier,
		PacketCeiling:     model.PacketCeiling,
		ClaimLevel:        level,
		ClaimEvidence:     strings.TrimSpace(claim.Evidence),
		GateRole:          role,
		Justification:     laneJustification(class, position, modelID, model, claim, role),
	}
	if level == "assumed" {
		lane.SupervisionRequired = true
	}
	return lane
}

func gateRole(class, tier, level string) string {
	return autonomyproof.GateRole(class, tier, level)
}

func laneJustification(class string, position int, modelID string, model ModelCapabilityEntry, claim CapabilityClaim, role string) string {
	level := strings.TrimSpace(claim.Level)
	if level == "" {
		level = "missing"
	}
	cost := strings.TrimSpace(model.CostClass)
	if cost == "" {
		cost = "unknown"
	}
	return fmt.Sprintf("%s[%d] -> %s (%s cost=%s) claim=%s role=%s", "routing_defaults["+class+"]", position, modelID, model.Tier, cost, level, role)
}

func pendingRouteObservations(homePath, capabilityClass string, modelIDs []string, now time.Time) ([]PendingRouteObservation, error) {
	dir := filepath.Join(homePath, candidates.InboxDir(candidates.RouteObservation))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	store, err := writeback.Open(homePath)
	if err != nil {
		return nil, err
	}
	var out []PendingRouteObservation
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		candidateID := strings.TrimSuffix(entry.Name(), ".json")
		c, err := readRouteCandidate(homePath, candidateID)
		if err != nil {
			return nil, err
		}
		if c.Kind != string(candidates.RouteObservation) {
			continue
		}
		matchReasons := routeObservationMatchReasons(c, capabilityClass, modelIDs)
		if len(matchReasons) == 0 {
			continue
		}
		pending := PendingRouteObservation{
			CandidateID:   c.ID,
			Subject:       c.Subject,
			Statement:     c.Statement,
			Detail:        c.Detail,
			Verdict:       c.Verdict,
			EvidenceRefs:  reviewartifact.UniqueSorted(c.EvidenceRefs),
			MatchReasons:  matchReasons,
			ReviewCommand: "compound route plan --candidate " + c.ID,
		}
		if nop, ok, err := store.ActiveNoPromote(c.ID, now); err != nil {
			return nil, err
		} else if ok {
			pending.NoPromoteID = nop.ID
			pending.NoPromoteBasis = nop.Basis
			pending.OwnerTargetKind = nop.OwnerTarget.Kind
			pending.OwnerTargetRef = nop.OwnerTarget.Ref
		}
		out = append(out, pending)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CandidateID < out[j].CandidateID
	})
	return out, nil
}

func routeObservationMatchReasons(c routeCandidate, capabilityClass string, modelIDs []string) []string {
	haystack := strings.ToLower(strings.Join([]string{c.Subject, c.Statement, c.Detail}, " "))
	var reasons []string
	if containsRouteToken(haystack, capabilityClass) {
		reasons = append(reasons, "capability_class:"+capabilityClass)
	}
	for _, modelID := range modelIDs {
		if containsRouteToken(haystack, modelID) {
			reasons = append(reasons, "model:"+modelID)
		}
	}
	if strings.Contains(haystack, "provider-ledger") || strings.Contains(haystack, "cost-coverage") {
		reasons = append(reasons, "global:provider_cost_coverage")
	}
	return reviewartifact.UniqueSorted(reasons)
}

func containsRouteToken(haystack, token string) bool {
	token = strings.ToLower(strings.TrimSpace(token))
	return token != "" && strings.Contains(haystack, token)
}

func modelCapabilityIDs(manifest ModelCapabilitiesManifest) []string {
	ids := make([]string, 0, len(manifest.Models))
	for id := range manifest.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedCapabilityClaimClasses(claims map[string]CapabilityClaim) []string {
	out := make([]string, 0, len(claims))
	for class := range claims {
		if strings.TrimSpace(class) != "" {
			out = append(out, class)
		}
	}
	sort.Strings(out)
	return out
}

func routingDefaultsForModel(defaults map[string][]string, modelID string) []string {
	var out []string
	for class, chain := range defaults {
		for position, candidate := range chain {
			if candidate == modelID {
				out = append(out, fmt.Sprintf("%s[%d]", class, position))
			}
		}
	}
	sort.Strings(out)
	return out
}

func sortedStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func modelCapabilitiesSourceAuthorityID(path, revision string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{path, revision, modelCapabilitiesImportCommandName}, "|")))
	return "mrcsrc_" + hex.EncodeToString(sum[:])[:12]
}

func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
