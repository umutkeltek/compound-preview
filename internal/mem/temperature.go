// temperature.go — the memory-temperature projection (Weng item 5, ACE loop v1).
//
// Temperature is a DERIVED score (docs/memory-architecture.md: "salience·decay +
// reinforcement-on-access", docs/architecture/transition.md: "volatile: useful now,
// expires unless reinforced") — never a mutable Claim field. It joins the landed
// capability-usage projection's per-claim injected/consumed/ignored reinforcement signal
// against the CURRENT ledger claims and freezes a recency-decayed temperature per claim.
//
// It is a projection, never truth: rebuilt from scratch on every call (so stale entries for
// superseded/absent claims disappear), derived only from an existing projection + the ledger
// (it does NOT re-scan receipts — that receipt-sweep already runs in capgraph), and written
// only under projections/ (never the ledger). Decay is applied at BUILD time and frozen into
// the stored temperature; rank time reads the stored numbers only, so ranking stays
// deterministic given the projection file.
//
// Layering note: this reads the capability-usage projection as a FILE with a local decode
// struct rather than importing internal/capgraph. mem is the ledger/authority (a lower layer);
// capgraph is a projection consumer that sits ON TOP of receipts+envelope. Importing capgraph
// here would invert that layering, so the projection is treated as external data (the packet's
// "read its projection output as data" boundary). No import cycle exists today either way.
package mem

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	homepkg "compound/internal/home"
)

const (
	// TemperatureSchemaVersion versions the projection body.
	TemperatureSchemaVersion = "compound.memory_temperature.v0.1"
	// TemperatureProjectionRef is the stable ref for this generated view.
	TemperatureProjectionRef = "compound://projections/memory-temperature"
	// TemperatureRelPath is the home-relative path the projection is written to.
	TemperatureRelPath = "projections/memory-temperature.json"

	// capabilityUsageRelPath is the capgraph projection this joins against. It is duplicated
	// here as a local constant on purpose (mem must not depend upward on capgraph — see the
	// layering note above); capgraph.CapabilityUsageRelPath is the source of truth for the value.
	capabilityUsageRelPath = "projections/capability-usage.json"

	// claimRefPrefix is the ref scheme injected claims carry in ContextPack.included[].
	claimRefPrefix = "claim://"

	// temperatureHalfLifeDays: a consumed claim's warmth halves every 14 days since it was
	// last injected. Older reinforcement decays toward neutral (0), never below/above the bound.
	temperatureHalfLifeDays = 14.0
	// temperatureScale maps the (consumed-ignored) net signal into tanh: a net of ~3 lands at
	// ~0.76·ceiling, so small counts get real gradation before the bound saturates.
	temperatureScale = 3.0
)

// MaxTemperatureContribution is the hard ceiling on |temperature| — both the amplitude the
// builder scales the tanh to AND the clamp the ranker applies. It is deliberately 0.25:
// strictly less than the entity-match rank weight (+2) and strictly less than HALF one
// token-overlap unit (1.0), so the temperature term (swing 0.5 across [-0.25,+0.25]) can never
// overturn even a single-unit relevance difference — relevance always dominates. Temperature is
// a bounded re-ranker of already-relevant claims, not a relevance manufacturer.
const MaxTemperatureContribution = 0.25

// TemperatureEntry is one claim's frozen, recency-decayed temperature plus the raw
// reinforcement counts it was derived from (kept for observability/audit).
type TemperatureEntry struct {
	ClaimID      string  `json:"claim_id"`
	Injected     int     `json:"injected"`
	Consumed     int     `json:"consumed"`
	Ignored      int     `json:"ignored"`
	LastInjected string  `json:"last_injected,omitempty"`
	Temperature  float64 `json:"temperature"`
}

// TemperatureProjection is the full derived surface written under projections/.
type TemperatureProjection struct {
	SchemaVersion string             `json:"schema_version"`
	ProjectionRef string             `json:"projection_ref"`
	ComputedAt    string             `json:"computed_at"`
	Entries       []TemperatureEntry `json:"entries"`
}

// TemperatureBuildOptions parameterizes the build. Now is the frozen decay reference AND the
// ComputedAt stamp; inject it for determinism (byte-identical output for equal inputs + equal Now).
type TemperatureBuildOptions struct {
	Now time.Time
}

// usageInjectedRef is the minimal decode shape of one capability-usage projection injected_refs[]
// row — mirrors capgraph.InjectedRefUsage's JSON tags without importing the package.
type usageInjectedRef struct {
	Ref           string `json:"ref"`
	InjectedCount int    `json:"injected_count"`
	ConsumedCount int    `json:"consumed_count"`
	IgnoredCount  int    `json:"ignored_count"`
	LastInjected  string `json:"last_injected"`
}

// BuildTemperature joins the capability-usage projection's injected-ref reinforcement signal
// against the current ledger claims and returns the memory-temperature projection. It never
// mutates the ledger; the caller persists the result via WriteTemperature. If the usage
// projection is absent, the result is an empty (but valid) projection, not an error.
func BuildTemperature(homePath string, opts TemperatureBuildOptions) (TemperatureProjection, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	proj := TemperatureProjection{
		SchemaVersion: TemperatureSchemaVersion,
		ProjectionRef: TemperatureProjectionRef,
		ComputedAt:    now.UTC().Format(time.RFC3339),
		Entries:       []TemperatureEntry{},
	}

	// 1. the reinforcement signal — reuse the receipt-sweep's output, never re-scan receipts.
	injected, err := readInjectedRefUsage(homePath)
	if err != nil {
		return proj, err
	}
	if len(injected) == 0 {
		return proj, nil
	}

	// 2. current claims — rebuilt-from-scratch: an injected ref whose claim has since been
	//    superseded or removed is no longer current, so it drops out of the projection.
	store, err := Open(homePath)
	if err != nil {
		return proj, err
	}
	claims, err := store.AllCurrent()
	if err != nil {
		return proj, err
	}
	current := make(map[string]bool, len(claims))
	for _, c := range claims {
		current[c.ID] = true
	}

	// 3. join: keep only claim:// refs pointing at a current claim.
	for _, u := range injected {
		ref := strings.TrimSpace(u.Ref)
		if !strings.HasPrefix(ref, claimRefPrefix) {
			continue // non-claim ref (skill://, capability://, ...) — not a ledger claim
		}
		id := strings.TrimPrefix(ref, claimRefPrefix)
		if id == "" || !current[id] {
			continue // absent/superseded claim — dropped on rebuild
		}
		proj.Entries = append(proj.Entries, TemperatureEntry{
			ClaimID:      id,
			Injected:     u.InjectedCount,
			Consumed:     u.ConsumedCount,
			Ignored:      u.IgnoredCount,
			LastInjected: u.LastInjected,
			Temperature:  temperatureValue(u.ConsumedCount, u.IgnoredCount, u.LastInjected, now),
		})
	}
	sort.Slice(proj.Entries, func(i, j int) bool {
		return proj.Entries[i].ClaimID < proj.Entries[j].ClaimID
	})
	return proj, nil
}

// temperatureValue is the v1 formula: a recency-decayed, bounded reinforcement score.
//
//	net   = consumed - ignored          // never-injected or perfectly-balanced => 0 (neutral)
//	decay = 2^(-ageDays / halfLife)      // ∈ (0,1], frozen at build time
//	T     = ceiling · tanh(net·decay / scale)   // ∈ [-ceiling, +ceiling]
//
// Properties this guarantees (falsifier-enforced):
//   - consumed reinforces: more consumed => larger net => warmer.
//   - ignored cools relative to consumed: net goes negative => T < 0 < a consumed claim's T.
//   - never-injected = exactly 0: such a claim is absent from the usage projection entirely
//     (no entry), and a balanced consumed==ignored claim yields net 0 => T 0 — absence of a
//     net signal is neutral, never a penalty on new claims.
//   - recency decay: same counts, older last_injected => smaller decay => |T| smaller (a warm
//     claim cools toward neutral).
//   - bounded: |T| < ceiling = MaxTemperatureContribution.
func temperatureValue(consumed, ignored int, lastInjected string, now time.Time) float64 {
	net := float64(consumed - ignored)
	if net == 0 {
		return 0
	}
	return MaxTemperatureContribution * math.Tanh(net*temperatureDecay(lastInjected, now)/temperatureScale)
}

// temperatureDecay returns 2^(-ageDays/halfLife) ∈ (0,1]. An unparseable/empty timestamp or a
// future (clock-skewed) timestamp is treated as fresh (decay 1) — decay never erases a signal
// on a parse hiccup, it only fades genuinely-aged ones.
func temperatureDecay(lastInjected string, now time.Time) float64 {
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(lastInjected))
	if err != nil {
		return 1.0
	}
	ageDays := now.Sub(ts).Hours() / 24.0
	if ageDays <= 0 {
		return 1.0
	}
	return math.Exp2(-ageDays / temperatureHalfLifeDays)
}

// WriteTemperature persists the projection under projections/, returning the absolute path
// written. This is derived state; it must never touch the ledger.
func WriteTemperature(homePath string, proj TemperatureProjection) (string, error) {
	path := filepath.Join(homePath, filepath.FromSlash(TemperatureRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return "", err
	}
	// Atomic: the orient path reads this body on every prompt and fail-opens on a
	// parse error, so a torn write would silently disable temperature ranking rather
	// than raise anything. See home.WriteFileAtomic.
	if err := homepkg.WriteFileAtomic(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ReadTemperature loads the persisted projection. The bool is false (with no error) when the
// projection has not been built yet — an expected, not exceptional, state.
func ReadTemperature(homePath string) (TemperatureProjection, bool, error) {
	path := filepath.Join(homePath, filepath.FromSlash(TemperatureRelPath))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return TemperatureProjection{}, false, nil
		}
		return TemperatureProjection{}, false, err
	}
	var proj TemperatureProjection
	if err := json.Unmarshal(data, &proj); err != nil {
		return TemperatureProjection{}, false, err
	}
	return proj, true, nil
}

// LoadTemperatureLookup reads the persisted projection into a claim-id → temperature map for the
// ranker. A missing projection returns (nil, nil): the ranker treats a nil map as all-neutral,
// so ranks stay byte-identical to the pre-temperature behavior (fail-open).
func LoadTemperatureLookup(homePath string) (map[string]float64, error) {
	proj, ok, err := ReadTemperature(homePath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	out := make(map[string]float64, len(proj.Entries))
	for _, e := range proj.Entries {
		out[e.ClaimID] = e.Temperature
	}
	return out, nil
}

// readInjectedRefUsage decodes the injected_refs[] rows from the capability-usage projection
// file. An absent projection is (nil, nil) — no reinforcement signal yet, not an error.
func readInjectedRefUsage(homePath string) ([]usageInjectedRef, error) {
	path := filepath.Join(homePath, filepath.FromSlash(capabilityUsageRelPath))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		InjectedRefs []usageInjectedRef `json:"injected_refs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc.InjectedRefs, nil
}
