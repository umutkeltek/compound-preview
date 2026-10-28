package routeevidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"compound/internal/work"
)

// Coordination capacity observation.
//
// Coordination projects the planning half of `work continue` for every actor in ONE request. The
// legacy path asked the global lane-capacity guard once per eligible actor, and the guard
// re-read every capacity source per call: the operator lane-alias file, both usage JSONL feeds,
// the switchback account projection, and — for codex lanes — the multi-auth quota cache. N
// eligible actors therefore read the same bytes N times.
//
// This observation keeps the SAME decisions. It runs the SAME composition and the SAME resolver
// as the production guard (readLaneCapacityWithFacts, resolveLaneCapacityKeyFrom,
// capacityAlternativeLadder), so a decision can never differ between the two. What it changes is
// the reading: each applicable source is read at most once per request, and every actor is then
// evaluated separately from those parsed bytes. It never caches an actor verdict and never lets
// one actor see another's rows: each actor copies its own provider family's rows before the
// shared decision runs.
//
// SCOPE: one observation serves one home at one request instant. A request naming a different
// home or a different instant is refused — the cached facts describe one instant of one home,
// and serving them for another would answer a different question. A NEW request builds a NEW
// observation, which re-reads the sources and therefore sees current bytes.
//
// The observation is READ-ONLY by construction. It refuses a request that is not read-only
// before it reads anything, never records capacity-refusal evidence, never runs a live provider
// probe on the coordination path, and never touches the global work.LaneCapacityGuard. It
// carries no execution authority: it answers "what does the capacity source say", not "may this
// actor run".

// CoordinationSourceAccess counts how an observation touched each capacity source.
type CoordinationSourceAccess struct {
	// Reads counts content read attempts per source path. A path that does not exist still
	// counts as one attempt: what the bound promises is that no source is read twice.
	Reads map[string]int
	// Stats counts existence probes per source path.
	Stats map[string]int
}

// CoordinationCapacityObservation is a request-scoped lane-capacity observation. It answers the
// same questions as the production guard, but reads each applicable source once per request.
type CoordinationCapacityObservation struct {
	cfg   LaneCapacityConfig
	home  string
	now   time.Time
	probe codexBarCapacityProbeFunc

	mu    sync.Mutex
	facts *coordinationCapacityFacts
	reads map[string]int
	stats map[string]int
}

// NewCoordinationCapacityObservation builds a request-scoped capacity observation for one home
// at one request instant. A request that names a different home or instant is refused.
func NewCoordinationCapacityObservation(cfg LaneCapacityConfig, home string, now time.Time) *CoordinationCapacityObservation {
	cfg = normalizeLaneCapacityConfig(cfg, home)
	observation := &CoordinationCapacityObservation{
		cfg:   cfg,
		home:  home,
		now:   now,
		probe: ProbeCodexBarCapacity,
		reads: map[string]int{},
		stats: map[string]int{},
	}
	observation.facts = &coordinationCapacityFacts{o: observation, home: home, cfg: cfg}
	return observation
}

// Guard returns the request-scoped guard function — the existing work.LaneCapacityGuardFunc seam,
// so a caller can inject it through work.CoordinationOptions without a new package, a new global,
// or a new execution authority. The returned guard is request-scoped: build ONE per request.
func (o *CoordinationCapacityObservation) Guard() work.LaneCapacityGuardFunc {
	return func(req work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		return o.observe(req)
	}
}

// SourceAccess reports how many times this observation read or stat'ed each capacity source on
// the COORDINATION path. It exists so the read-once property is observable rather than asserted.
// A declared-source request is delegated to the existing fresh guard, whose own reads are not
// attributed here.
func (o *CoordinationCapacityObservation) SourceAccess() CoordinationSourceAccess {
	o.mu.Lock()
	defer o.mu.Unlock()
	reads := make(map[string]int, len(o.reads))
	for path, count := range o.reads {
		reads[path] = count
	}
	stats := make(map[string]int, len(o.stats))
	for path, count := range o.stats {
		stats[path] = count
	}
	return CoordinationSourceAccess{Reads: reads, Stats: stats}
}

func (o *CoordinationCapacityObservation) countRead(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	o.reads[path]++
}

func (o *CoordinationCapacityObservation) countStat(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	o.stats[path]++
}

// observe answers one actor's capacity question from the request's cached source facts.
func (o *CoordinationCapacityObservation) observe(req work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// The observation is read-only by construction: it never records capacity-refusal
	// evidence and never runs a live probe on the coordination path. A request that asks for
	// a mutating verdict is refused BEFORE every other branch, so it reads nothing, probes
	// nothing and writes nothing.
	if !req.ReadOnly {
		return work.LaneCapacityGuardResult{}, errors.New("coordination capacity observation is read-only: refusing a request that is not read-only")
	}
	// One observation answers for one home at one instant. A mismatch is refused, never
	// served from another instant's facts.
	if req.HomePath != o.home {
		return work.LaneCapacityGuardResult{}, fmt.Errorf("coordination capacity observation is bound to home %q and cannot answer for %q", o.home, req.HomePath)
	}
	if !req.Now.Equal(o.now) {
		return work.LaneCapacityGuardResult{}, fmt.Errorf(
			"coordination capacity observation is bound to request instant %s and cannot answer for %s",
			o.now.UTC().Format(time.RFC3339Nano), req.Now.UTC().Format(time.RFC3339Nano))
	}
	// A tuple-backed request with no declared source consults no Compound capacity
	// source at all: runtime quota stays execution-gateway-owned.
	if req.TupleBacked && req.CapacitySource == nil {
		return work.LaneCapacityGuardResult{
			Capacity: work.DispatchLaneCapacity{State: "gateway_owned"},
			Message:  "no Compound capacity source declared; runtime quota remains execution-gateway-owned",
		}, nil
	}
	if req.CapacitySource != nil {
		// A declared source keeps the EXISTING fresh guard semantics, including its
		// live-probe fallback. The observation only adds the read-only posture.
		decision, err := o.guardDeclared(req)
		if err != nil {
			return work.LaneCapacityGuardResult{}, err
		}
		return laneCapacityGuardResult(decision), nil
	}
	decision, err := o.guardLegacy(req)
	if err != nil {
		return work.LaneCapacityGuardResult{}, err
	}
	return laneCapacityGuardResult(decision), nil
}

// guardLegacy evaluates the legacy (undeclared) capacity path for one actor from the request's
// cached facts, through the SAME composition the production guard runs.
func (o *CoordinationCapacityObservation) guardLegacy(req work.LaneCapacityGuardRequest) (LaneCapacityDecision, error) {
	expected := capacityExpectedMinutes(req.ExpectedMinutes)
	capacity, err := readLaneCapacityWithFacts(req.Lane, req.Now, expected, o.facts.compositionFacts())
	if err != nil {
		return LaneCapacityDecision{}, err
	}
	alternatives := capacityAlternativeLadder(req.CapabilityClass, req.Alternatives, o.facts.routingDefaults)
	cfg := o.cfg
	cfg.ExpectedMinutes = expected
	// The observation is read-only by construction, so refusal evidence is never recorded
	// from here — the same posture the production guard takes for a ReadOnly request.
	cfg.SuppressRefusalEvidence = true
	return decideLaneCapacity(req.HomePath, req.Lane, req.Now, req.Confirm, alternatives, cfg, capacity)
}

// guardDeclared delegates a declared-source request to the EXISTING fresh guard path, with the
// observation's read-only posture: refusal evidence is never recorded from here. The delegated
// path reads its own sources, so those reads are not attributed to this observation's counters
// and are not shared with the request's cached facts.
func (o *CoordinationCapacityObservation) guardDeclared(req work.LaneCapacityGuardRequest) (LaneCapacityDecision, error) {
	probe := o.probe
	if probe == nil {
		probe = ProbeCodexBarCapacity
	}
	effective := o.cfg
	effective.ExpectedMinutes = req.ExpectedMinutes
	effective.SuppressRefusalEvidence = true
	return guardDeclaredLaneCapacityWithConfigAndProbe(
		req.HomePath,
		req.Lane,
		req.CapabilityClass,
		LaneCapacitySourceRef{
			Kind:             req.CapacitySource.Kind,
			FreshnessMinutes: req.CapacitySource.FreshnessMinutes,
			Ref:              req.CapacitySource.Ref,
		},
		req.Now,
		req.ExpectedMinutes,
		req.Confirm,
		req.Alternatives,
		true,
		effective,
		probe,
	)
}

// coordinationCapacityFacts holds the request's lazily-read capacity facts. Every cached fact is
// INDEPENDENT of the actor: the freshness and family tests run per actor against these same
// parsed bytes.
type coordinationCapacityFacts struct {
	o    *CoordinationCapacityObservation
	home string
	cfg  LaneCapacityConfig

	aliases     map[string]string
	aliasesErr  error
	aliasesRead bool

	usageByFamily  map[string][]codexBarCapacityRow
	usageInstalled bool
	usageErr       error
	usageRead      bool

	quotaModTime  time.Time
	quotaPresent  bool
	quotaStatErr  error
	quotaStatRead bool

	quotaCache   quotaCacheDocument
	quotaReadErr error
	quotaRead    bool

	account     *AccountProjection
	accountErr  error
	accountRead bool

	routing     map[string][]string
	routingErr  error
	routingRead bool

	installed     bool
	installedRead bool
}

// compositionFacts projects the request's cached facts onto the shared composition seam. The
// alias file is loaded eagerly here — even for a lane that resolves structurally — so a malformed
// file can still surface on a final-unknown state, exactly as the production path does.
func (f *coordinationCapacityFacts) compositionFacts() laneCapacityFacts {
	aliases, aliasErr := f.aliasMap()
	installed := f.sourceInstalled()
	facts := laneCapacityFacts{aliases: aliases, aliasErr: aliasErr, installed: installed}
	facts.usage = f.usageCapacity
	facts.quotaWall = f.quotaWallCapacity
	facts.deadAccount = f.deadAccountCapacity
	return facts
}

// aliasMap loads the operator-owned alias override file at most once per request.
func (f *coordinationCapacityFacts) aliasMap() (map[string]string, error) {
	if !f.aliasesRead {
		f.aliasesRead = true
		path := f.cfg.Paths.AliasesPath
		if strings.TrimSpace(path) != "" {
			f.o.countRead(path)
		}
		f.aliases, f.aliasesErr = loadLaneCapacityAliases(path)
	}
	return f.aliases, f.aliasesErr
}

// sourceInstalled reports whether ANY configured capacity source file exists. The existence
// probes happen once per request, not once per actor, and the probe sequence is the production
// one.
func (f *coordinationCapacityFacts) sourceInstalled() bool {
	if !f.installedRead {
		f.installedRead = true
		f.installed = capacitySourceInstalledCounted(f.cfg.Paths, f.o.countStat)
	}
	return f.installed
}

// usageCapacity answers the usage-feed question for one provider family from the request's
// parsed rows.
func (f *coordinationCapacityFacts) usageCapacity(provider string, now time.Time, expectedMinutes int) (LaneCapacity, bool, error) {
	_, installed, err := f.usageRows()
	if err != nil {
		return LaneCapacity{}, false, err
	}
	filtered := f.rowsForFamily(provider, now)
	return decideCodexBarCapacity(filtered, installed, provider, now, expectedMinutes, capacityFreshness(provider))
}

// usageRows parses every configured usage feed at most once per request and indexes the rows by
// capacity family. The index is request-only: no actor verdict is cached, and each actor copies
// its own family's rows before the shared decision runs.
func (f *coordinationCapacityFacts) usageRows() (map[string][]codexBarCapacityRow, bool, error) {
	if !f.usageRead {
		f.usageRead = true
		byFamily := map[string][]codexBarCapacityRow{}
		installed := false
		for _, src := range capacityRowSourcesFor(f.cfg.Paths) {
			fileRows, fileInstalled, err := f.readUsageSource(src)
			if err != nil {
				f.usageErr = err
				f.usageByFamily = nil
				f.usageInstalled = false
				return nil, false, err
			}
			installed = installed || fileInstalled
			for _, row := range fileRows {
				family := normalizeLaneCapacityKey(row.Provider)
				if family == "" {
					continue
				}
				byFamily[family] = append(byFamily[family], row)
			}
		}
		f.usageByFamily = byFamily
		f.usageInstalled = installed
	}
	return f.usageByFamily, f.usageInstalled, f.usageErr
}

// readUsageSource reads one usage feed once per request. The shared parser retains only rows
// that name a capacity family, so a row that could never constrain a decision is never
// materialized.
func (f *coordinationCapacityFacts) readUsageSource(src capacityRowSource) ([]codexBarCapacityRow, bool, error) {
	if strings.TrimSpace(src.path) == "" {
		return nil, false, nil
	}
	f.o.countRead(src.path)
	return scanCapacityRows(src, func(row codexBarCapacityRow) bool {
		return normalizeLaneCapacityKey(row.Provider) != ""
	})
}

// rowsForFamily copies the rows of ONE provider family, dropping rows sampled beyond the
// clock-skew allowance. The copy is sized to that family alone, so a broad multi-provider feed
// never costs an actor an allocation proportional to the other providers' rows. The copy is
// required: the shared decision sorts the rows in place, and one actor must never reorder
// another's rows.
func (f *coordinationCapacityFacts) rowsForFamily(family string, now time.Time) []codexBarCapacityRow {
	rows := f.usageByFamily[family]
	out := make([]codexBarCapacityRow, 0, len(rows))
	for _, row := range rows {
		if row.sampled.After(now.Add(capacityClockSkew)) {
			continue
		}
		out = append(out, row)
	}
	return out
}

// quotaWallCapacity answers the multi-auth quota-cache question for one provider family. The
// cache is stat'ed once and read at most once per request.
func (f *coordinationCapacityFacts) quotaWallCapacity(provider string, now time.Time) (LaneCapacity, bool, error) {
	if !quotaWallApplies(provider, f.cfg.Paths.QuotaCachePath) {
		return LaneCapacity{}, false, nil
	}
	observed, present, err := f.quotaCacheModTime()
	if err != nil {
		return LaneCapacity{}, false, err
	}
	if !present {
		return LaneCapacity{}, false, nil
	}
	if !quotaCacheWithinFreshness(observed, now) {
		return LaneCapacity{}, false, nil
	}
	cache, err := f.quotaCacheContent()
	if err != nil {
		return LaneCapacity{}, false, err
	}
	return decideFreshQuotaWall(observed, cache)
}

func (f *coordinationCapacityFacts) quotaCacheModTime() (time.Time, bool, error) {
	if !f.quotaStatRead {
		f.quotaStatRead = true
		path := f.cfg.Paths.QuotaCachePath
		f.o.countStat(path)
		info, err := os.Stat(path)
		switch {
		case err == nil:
			f.quotaPresent = true
			f.quotaModTime = info.ModTime()
		case os.IsNotExist(err):
			f.quotaPresent = false
		default:
			f.quotaStatErr = fmt.Errorf("stat quota cache: %w", err)
		}
	}
	return f.quotaModTime, f.quotaPresent, f.quotaStatErr
}

func (f *coordinationCapacityFacts) quotaCacheContent() (quotaCacheDocument, error) {
	if !f.quotaRead {
		f.quotaRead = true
		path := f.cfg.Paths.QuotaCachePath
		f.o.countRead(path)
		data, err := os.ReadFile(path)
		switch {
		case err != nil:
			f.quotaReadErr = fmt.Errorf("read quota cache: %w", err)
		default:
			var cache quotaCacheDocument
			if err := json.Unmarshal(data, &cache); err != nil {
				f.quotaReadErr = fmt.Errorf("parse quota cache: %w", err)
			} else {
				f.quotaCache = cache
			}
		}
	}
	return f.quotaCache, f.quotaReadErr
}

// deadAccountCapacity answers the account-projection question for one provider family. The
// projection is read at most once per request.
func (f *coordinationCapacityFacts) deadAccountCapacity(provider string, now time.Time) (LaneCapacity, bool, error) {
	if !accountProjectionApplies(f.cfg.Paths.AccountProjectionPath) {
		return LaneCapacity{}, false, nil
	}
	projection, err := f.accountProjection()
	if err != nil {
		return LaneCapacity{}, false, err
	}
	if projection == nil {
		return LaneCapacity{}, false, nil
	}
	return decideDeadAccountProjection(projection, provider, now)
}

func (f *coordinationCapacityFacts) accountProjection() (*AccountProjection, error) {
	if !f.accountRead {
		f.accountRead = true
		path := f.cfg.Paths.AccountProjectionPath
		f.o.countRead(path)
		projection, err := readAccountProjectionFile(path)
		if err != nil {
			f.accountErr = err
		} else {
			f.account = projection
		}
	}
	return f.account, f.accountErr
}

// routingDefaults reads the model-capabilities routing defaults at most once per request.
func (f *coordinationCapacityFacts) routingDefaults() (map[string][]string, error) {
	if !f.routingRead {
		f.routingRead = true
		f.o.countRead(ModelCapabilitiesPath(f.home))
		manifest, _, _, err := loadModelCapabilitiesSnapshot(f.home)
		if err != nil {
			f.routingErr = err
		} else {
			f.routing = manifest.RoutingDefaults
		}
	}
	return f.routing, f.routingErr
}
