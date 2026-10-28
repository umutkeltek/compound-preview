package routeevidence

// Lane speed is MEASURED, never declared. A bounded probe records tokens/sec and
// time-to-first-token for one lane tuple (harness x route x model x effort, with the
// per-task token budget carried as metadata) and lands here as an append-only receipt.
// At plan time the receipt can only DEMOTE a lane's effective latency class relative to
// its declared class (compound's own tier taxonomy) -- never auto-promote it, and never
// from provider marketing. Promotion still runs through the owner-review promotion path.
//
// No code path in this file makes a provider or network call. The actual probe RUN is a
// later dispatcher/orchestrator act; this file only ingests a probe RESULT (from a file or
// stdin), stores it, and derives a conservative role adjustment + worth-the-wait veto.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	LaneSpeedProbeSchemaVersion = "compound.lane_speed_probe.v0.1"
	LaneSpeedAuthorityRef       = "compound://ledger/lane-speed-evidence"

	laneSpeedLedgerFile = "lane-speed-evidence.jsonl"
	latencyBudgetFile   = "latency-budget.json"
)

// LatencyClass is the operator "worth-the-wait" taxonomy: how long a caller is willing to
// wait for a lane. It is ordinal (interactive < batch < overnight) so a slower measured
// class strictly dominates a faster declared one when the two disagree.
type LatencyClass string

const (
	LatencyInteractive LatencyClass = "interactive"
	LatencyBatch       LatencyClass = "batch"
	LatencyOvernight   LatencyClass = "overnight"
)

// latencyRank orders the classes fast->slow. An unknown class is treated as the slowest so
// that filtering fails toward "make the caller wait / verify" rather than admitting silently.
func latencyRank(c LatencyClass) int {
	switch c {
	case LatencyInteractive:
		return 0
	case LatencyBatch:
		return 1
	case LatencyOvernight:
		return 2
	default:
		return 3
	}
}

// NormalizeLatencyClass parses a caller-supplied latency class. The empty string is a valid
// "no latency filtering" signal and returns ("", false).
func NormalizeLatencyClass(value string) (LatencyClass, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(LatencyInteractive):
		return LatencyInteractive, true
	case string(LatencyBatch):
		return LatencyBatch, true
	case string(LatencyOvernight):
		return LatencyOvernight, true
	default:
		return "", false
	}
}

// DeclaredLatencyClass is what a lane CLAIMS by virtue of compound's own tier taxonomy --
// not a provider marketing number. A local-batch tier is declared batch; every other tier
// is offered as interactive-capable until a measured receipt demotes it.
func DeclaredLatencyClass(tier string) LatencyClass {
	if strings.EqualFold(strings.TrimSpace(tier), "local-batch") {
		return LatencyBatch
	}
	return LatencyInteractive
}

// LatencyThreshold is the operator SLA a lane must MEET to qualify for a latency class.
// A zero field means "no constraint on that dimension".
type LatencyThreshold struct {
	MinTokensPerSec float64 `json:"min_tokens_per_sec"`
	MaxTTFTms       int64   `json:"max_ttft_ms"`
}

// LatencyBudget is the operator "worth-the-wait" SLA used to classify a measured probe.
// Interactive is what a PERSON tolerates before a lane stops feeling live; batch is what a
// job you check back on tolerates. Anything slower than batch is overnight.
type LatencyBudget struct {
	Interactive LatencyThreshold `json:"interactive"`
	Batch       LatencyThreshold `json:"batch"`
}

// DefaultLatencyBudget is the built-in operator human-wait SLA. These numbers describe how
// long a PERSON waits (interactive) or a background job waits (batch) -- they are operator
// policy, NOT a provider spec sheet or marketing claim. Override per home via
// registries/latency-budget.json or per call via CapabilityRoutePlanOptions.LatencyBudget.
func DefaultLatencyBudget() LatencyBudget {
	return LatencyBudget{
		Interactive: LatencyThreshold{MinTokensPerSec: 12, MaxTTFTms: 2500},
		Batch:       LatencyThreshold{MinTokensPerSec: 2, MaxTTFTms: 60000},
	}
}

// LoadLatencyBudget reads the operator override at registries/latency-budget.json if present,
// else returns the built-in operator SLA default. The second return value names the source
// so callers can surface it (never a silent number). Missing file is a clean default.
func LoadLatencyBudget(homePath string) (LatencyBudget, string, error) {
	path := LatencyBudgetPath(homePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultLatencyBudget(), "default_operator_sla", nil
		}
		return LatencyBudget{}, "", fmt.Errorf("read latency budget: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var budget LatencyBudget
	if err := dec.Decode(&budget); err != nil {
		return LatencyBudget{}, "", fmt.Errorf("parse latency budget %s: %w", path, err)
	}
	if err := budget.validate(); err != nil {
		return LatencyBudget{}, "", err
	}
	return budget, "override:" + path, nil
}

func LatencyBudgetPath(homePath string) string {
	return filepath.Join(homePath, "registries", latencyBudgetFile)
}

func (b LatencyBudget) validate() error {
	var errs []string
	check := func(name string, t LatencyThreshold) {
		if t.MinTokensPerSec < 0 || math.IsNaN(t.MinTokensPerSec) || math.IsInf(t.MinTokensPerSec, 0) {
			errs = append(errs, name+".min_tokens_per_sec must be a finite non-negative number")
		}
		if t.MaxTTFTms < 0 {
			errs = append(errs, name+".max_ttft_ms must be non-negative")
		}
	}
	check("interactive", b.Interactive)
	check("batch", b.Batch)
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("latency budget invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (t LatencyThreshold) meets(tokensPerSec float64, ttftMs int64) bool {
	if t.MinTokensPerSec > 0 && tokensPerSec < t.MinTokensPerSec {
		return false
	}
	if t.MaxTTFTms > 0 && ttftMs > t.MaxTTFTms {
		return false
	}
	return true
}

// MeasuredLatencyClass classifies a measured probe against the operator budget. It is the
// only place raw tokens/sec + TTFT become a class, so the class is always MEASURED, never
// declared by whoever ran the probe.
func MeasuredLatencyClass(tokensPerSec float64, ttftMs int64, budget LatencyBudget) LatencyClass {
	switch {
	case budget.Interactive.meets(tokensPerSec, ttftMs):
		return LatencyInteractive
	case budget.Batch.meets(tokensPerSec, ttftMs):
		return LatencyBatch
	default:
		return LatencyOvernight
	}
}

// LaneSpeedProbe is one bounded-probe RESULT: measured tokens/sec + time-to-first-token for
// a lane tuple, ingested from a file/stdin. It is route evidence, never task success.
type LaneSpeedProbe struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	RecordedAt    string `json:"recorded_at"`
	// Lane tuple identity. Throughput (tokens/sec, TTFT) is a RATE independent of the
	// per-task token budget, so the speed identity is harness x route x model x effort; the
	// budget dimension governs total wait (tokens / rate) and rides as Budget metadata.
	Harness     string `json:"harness,omitempty"`
	Route       string `json:"route,omitempty"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	Budget      string `json:"budget,omitempty"`
	LaneRef     string `json:"lane_ref,omitempty"` // optional model-router model id this probe covers
	LaneTupleID string `json:"lane_tuple_id"`
	// Measured facts.
	TokensPerSec     float64  `json:"tokens_per_sec"`
	TTFTms           int64    `json:"ttft_ms"`
	SampleCount      int      `json:"sample_count,omitempty"`
	SampleWindowSecs float64  `json:"sample_window_secs,omitempty"`
	MeasuredAt       string   `json:"measured_at,omitempty"`
	Source           string   `json:"source"`
	EvidenceRefs     []string `json:"evidence_refs,omitempty"`
}

func laneSpeedSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "*"
	}
	var b strings.Builder
	dash := false
	for _, r := range value {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "*"
	}
	return out
}

// LaneSpeedTupleID is the canonical measured-speed lane identity (harness x route x model x
// effort). Budget is deliberately excluded because throughput is a rate.
func LaneSpeedTupleID(harness, route, model, effort string) string {
	return "lane:" + strings.Join([]string{
		laneSpeedSlug(harness),
		laneSpeedSlug(route),
		laneSpeedSlug(model),
		laneSpeedSlug(effort),
	}, "--")
}

func (p LaneSpeedProbe) tupleID() string {
	return LaneSpeedTupleID(p.Harness, p.Route, p.Model, p.Effort)
}

// modelLaneSpeedTupleID derives the measured-speed identity for a model-router lane entry so
// a probe can be matched to the lane it covers. Route falls back to the human lane label when
// no provider_route is declared (legacy manifests).
func modelLaneSpeedTupleID(modelID string, m ModelCapabilityEntry) string {
	route := strings.TrimSpace(m.ProviderRoute)
	if route == "" {
		route = strings.TrimSpace(m.Lane)
	}
	return LaneSpeedTupleID(m.Harness, route, modelID, m.EffortDefault)
}

func (p LaneSpeedProbe) matchesModel(modelID string, m ModelCapabilityEntry) bool {
	if ref := strings.TrimSpace(p.LaneRef); ref != "" && ref == strings.TrimSpace(modelID) {
		return true
	}
	return p.tupleID() == modelLaneSpeedTupleID(modelID, m)
}

func (p LaneSpeedProbe) measuredTime() time.Time {
	for _, candidate := range []string{p.MeasuredAt, p.RecordedAt} {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(candidate)); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (p LaneSpeedProbe) validate() error {
	var errs []string
	if strings.TrimSpace(p.Harness) == "" && strings.TrimSpace(p.Model) == "" && strings.TrimSpace(p.Route) == "" && strings.TrimSpace(p.LaneRef) == "" {
		errs = append(errs, "lane identity required (harness/route/model or lane_ref)")
	}
	if math.IsNaN(p.TokensPerSec) || math.IsInf(p.TokensPerSec, 0) || p.TokensPerSec < 0 {
		errs = append(errs, "tokens_per_sec must be a finite non-negative number")
	}
	if p.TTFTms < 0 {
		errs = append(errs, "ttft_ms must be non-negative")
	}
	if p.MeasuredAt != "" {
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(p.MeasuredAt)); err != nil {
			errs = append(errs, fmt.Sprintf("measured_at invalid: %v", err))
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("lane speed probe invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

// DecodeLaneSpeedProbe parses one probe RESULT from bytes, rejecting unknown fields so a
// malformed or provider-specific blob cannot smuggle extra state into the ledger.
func DecodeLaneSpeedProbe(data []byte) (LaneSpeedProbe, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var probe LaneSpeedProbe
	if err := dec.Decode(&probe); err != nil {
		return LaneSpeedProbe{}, fmt.Errorf("decode lane speed probe: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return LaneSpeedProbe{}, fmt.Errorf("decode lane speed probe: trailing JSON")
		}
		return LaneSpeedProbe{}, fmt.Errorf("decode lane speed probe trailing JSON: %w", err)
	}
	return probe, nil
}

// IngestLaneSpeedProbe is the file/stdin door: decode -> normalize -> append. It returns the
// stored probe and whether it was newly written (false = idempotent no-op for a re-ingest).
func IngestLaneSpeedProbe(homePath string, data []byte, now time.Time) (LaneSpeedProbe, bool, error) {
	probe, err := DecodeLaneSpeedProbe(data)
	if err != nil {
		return LaneSpeedProbe{}, false, err
	}
	return RecordLaneSpeedProbe(homePath, probe, now)
}

// InvalidatePlanCapabilityRouteCache evicts every cached route plan rooted at homePath.
// Mutating operations that change route-evidence inputs (a freshly appended speed
// probe, a re-imported model-capabilities manifest, a refreshed account projection)
// must call this so the next PlanCapabilityRoute call recomputes rather than
// serving a cached plan that contradicts the just-written state.
func InvalidatePlanCapabilityRouteCache(homePath string) {
	planCapabilityRouteCacheMu.Lock()
	defer planCapabilityRouteCacheMu.Unlock()
	for key := range planCapabilityRouteCache {
		if strings.HasPrefix(key, homePath+"\x00") {
			delete(planCapabilityRouteCache, key)
		}
	}
}

// RecordLaneSpeedProbe normalizes and appends a probe result to the lane-speed evidence
// ledger. Re-recording an identical probe is a no-op (dedup by derived id).
func RecordLaneSpeedProbe(homePath string, probe LaneSpeedProbe, now time.Time) (LaneSpeedProbe, bool, error) {
	if strings.TrimSpace(homePath) == "" {
		return LaneSpeedProbe{}, false, fmt.Errorf("COMPOUND_HOME required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if err := probe.validate(); err != nil {
		return LaneSpeedProbe{}, false, err
	}
	probe.SchemaVersion = LaneSpeedProbeSchemaVersion
	probe.RecordedAt = rfc3339(now)
	if strings.TrimSpace(probe.Source) == "" {
		probe.Source = "cli-ingest"
	}
	if strings.TrimSpace(probe.MeasuredAt) == "" {
		probe.MeasuredAt = probe.RecordedAt
	}
	probe.LaneTupleID = probe.tupleID()
	probe.EvidenceRefs = dedupeRefs(probe.EvidenceRefs)
	probe.ID = idFor("lanespeed_", probe.LaneTupleID, probe.LaneRef, probe.MeasuredAt,
		fmt.Sprintf("%g", probe.TokensPerSec), fmt.Sprintf("%d", probe.TTFTms), probe.Source)

	existing, err := LoadLaneSpeedProbes(homePath)
	if err != nil {
		return LaneSpeedProbe{}, false, err
	}
	for _, prior := range existing {
		if prior.ID == probe.ID {
			return prior, false, nil
		}
	}
	if err := appendLaneSpeedProbe(homePath, probe); err != nil {
		return LaneSpeedProbe{}, false, err
	}
	// A newly written probe changes route-evidence truth for THIS home: any
	// cached plan rooted here was computed without it and would now serve
	// stale lane state to a second PlanCapabilityRoute caller in this
	// process. Evict this home's entries only — other homes are unaffected.
	InvalidatePlanCapabilityRouteCache(homePath)
	return probe, true, nil
}

func appendLaneSpeedProbe(homePath string, probe LaneSpeedProbe) error {
	path := LaneSpeedLedgerPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(probe)
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

func LaneSpeedLedgerPath(homePath string) string {
	return filepath.Join(homePath, "ledger", laneSpeedLedgerFile)
}

// LoadLaneSpeedProbes returns every probe row in ledger order. A missing ledger is a clean
// empty slice so a fresh home reads as "no measurements yet".
func LoadLaneSpeedProbes(homePath string) ([]LaneSpeedProbe, error) {
	path := LaneSpeedLedgerPath(homePath)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lane speed evidence: %w", err)
	}
	defer f.Close()

	var probes []LaneSpeedProbe
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var probe LaneSpeedProbe
		if err := json.Unmarshal([]byte(raw), &probe); err != nil {
			return nil, fmt.Errorf("parse lane speed evidence line %d: %w", line, err)
		}
		probes = append(probes, probe)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan lane speed evidence: %w", err)
	}
	return probes, nil
}

// LatestLaneSpeedByTuple reduces the ledger to the freshest probe per lane tuple (by
// measured_at, then recorded_at). It is the read surface for role adjustment and CLI display.
func LatestLaneSpeedByTuple(homePath string) (map[string]LaneSpeedProbe, error) {
	probes, err := LoadLaneSpeedProbes(homePath)
	if err != nil {
		return nil, err
	}
	latest := map[string]LaneSpeedProbe{}
	for _, probe := range probes {
		key := probe.tupleID()
		if current, ok := latest[key]; ok && !laneSpeedNewer(probe, current) {
			continue
		}
		latest[key] = probe
	}
	return latest, nil
}

func laneSpeedNewer(candidate, current LaneSpeedProbe) bool {
	ct, cur := candidate.measuredTime(), current.measuredTime()
	if ct.Equal(cur) {
		return candidate.RecordedAt >= current.RecordedAt
	}
	return ct.After(cur)
}

// latestMatchingProbe finds the freshest probe covering a model-router lane, matched by the
// lane tuple identity or an explicit lane_ref.
func latestMatchingProbe(probes []LaneSpeedProbe, modelID string, m ModelCapabilityEntry) (LaneSpeedProbe, bool) {
	var best LaneSpeedProbe
	found := false
	for _, probe := range probes {
		if !probe.matchesModel(modelID, m) {
			continue
		}
		if !found || laneSpeedNewer(probe, best) {
			best = probe
			found = true
		}
	}
	return best, found
}

// LaneSpeedAdjustment is a receipt-derived role change: a lane whose MEASURED latency class is
// slower than its DECLARED class is demoted for this plan. It only ever demotes (never
// auto-promotes, never from marketing) and always cites the probe receipt it derives from.
type LaneSpeedAdjustment struct {
	DeclaredLatencyClass string   `json:"declared_latency_class"`
	MeasuredLatencyClass string   `json:"measured_latency_class"`
	MeasuredTokensPerSec float64  `json:"measured_tokens_per_sec"`
	MeasuredTTFTms       int64    `json:"measured_ttft_ms"`
	PriorGateRole        string   `json:"prior_gate_role"`
	AdjustedGateRole     string   `json:"adjusted_gate_role"`
	ProbeID              string   `json:"probe_id"`
	MeasuredAt           string   `json:"measured_at,omitempty"`
	EvidenceRefs         []string `json:"evidence_refs,omitempty"`
}

// speedDemotedGateRole names the demoted role so `route plan` output visibly changes when a
// lane is contradicted by measurement (e.g. eligible -> speed_demoted_batch).
func speedDemotedGateRole(measured LatencyClass) string {
	return "speed_demoted_" + string(measured)
}

// laneEffectiveLatency computes a lane's effective latency class and, when a measured receipt
// contradicts the declared class (measured strictly slower), stamps a demotion adjustment onto
// the lane and returns a warning. Measurement can only make a lane look SLOWER: a probe that is
// faster than the declared class never promotes it (promotion stays with owner review). Returns
// the effective class used by the worth-the-wait veto and an optional warning string.
func laneEffectiveLatency(lane *CapabilityRouteLane, model ModelCapabilityEntry, probes []LaneSpeedProbe, budget LatencyBudget) (LatencyClass, string) {
	declared := DeclaredLatencyClass(model.Tier)
	probe, ok := latestMatchingProbe(probes, lane.ModelID, model)
	if !ok {
		return declared, ""
	}
	measured := MeasuredLatencyClass(probe.TokensPerSec, probe.TTFTms, budget)
	if latencyRank(measured) <= latencyRank(declared) {
		// Measurement does not contradict the declared class (equal or faster). No demotion,
		// no promotion: the declared class stands.
		return declared, ""
	}
	prior := lane.GateRole
	adjusted := speedDemotedGateRole(measured)
	lane.GateRole = adjusted
	lane.SpeedAdjustment = &LaneSpeedAdjustment{
		DeclaredLatencyClass: string(declared),
		MeasuredLatencyClass: string(measured),
		MeasuredTokensPerSec: probe.TokensPerSec,
		MeasuredTTFTms:       probe.TTFTms,
		PriorGateRole:        prior,
		AdjustedGateRole:     adjusted,
		ProbeID:              probe.ID,
		MeasuredAt:           probe.MeasuredAt,
		EvidenceRefs:         probe.EvidenceRefs,
	}
	warning := fmt.Sprintf(
		"lane %s demoted by measured speed: declared=%s measured=%s (%.1f tok/s, ttft=%dms) role %s -> %s [probe=%s]",
		lane.ModelID, declared, measured, probe.TokensPerSec, probe.TTFTms, prior, adjusted, probe.ID,
	)
	return measured, warning
}

// laneLatencyVeto enforces the worth-the-wait rule for a required task latency class: a lane
// whose effective latency class is slower than what the task requires is vetoed for that task,
// but the SAME lane is admitted when the task can afford the wait.
func laneLatencyVeto(lane CapabilityRouteLane, effective, required LatencyClass) (bool, string) {
	if required == "" || latencyRank(effective) <= latencyRank(required) {
		return false, ""
	}
	basis := "declared"
	if lane.SpeedAdjustment != nil {
		basis = "measured"
	}
	return true, fmt.Sprintf(
		"latency veto: lane %s %s class=%s is slower than required %s work",
		lane.ModelID, basis, effective, required,
	)
}
