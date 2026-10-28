// probe_group_usage.go — the per-probe-group consumption attribution projection
// (work_8713c4206232, S4 of the probe-group/temperature program: work_e18d3622972d /
// 697a427a's deterministic probe-group selection feeding this file's per-group
// aggregate).
//
// FOOD SUPPLY ONLY: this is an additive, fail-open derived projection. No contract
// compiler, hook, or ranker reads it yet (docs/architecture/status.md's package table
// gets the pointer row when this cuts over to a live consumer). It exists so a
// per-group reinforcement signal — the same injected/consumed/ignored join usage.go
// already performs per REF, here bucketed per GROUP — can be inspected/audited before
// any consumer depends on it: exactly the "generated view proves freshness, it cannot
// promote truth" posture every projections/ file carries (docs/architecture/epistemics.md).
//
// It runs its OWN events/*.json + context/packs/*.json sweep (mirroring usage.go's
// receipt-scan/pack-load shape rather than post-processing BuildCapabilityUsage's
// already-aggregated-by-ref output) so this projection's schema and cadence can evolve
// independently of the capability-usage projection it shares a data source with, and so
// group attribution is computed at full (receipt, included-ref) granularity — the same
// duplicated-sweep idiom injection_observation.go already uses alongside usage.go over
// the identical receipts+packs.
//
// Per-group counts mirror usage.go's per-ref honest-attribution rule exactly:
//   - injected increments on every (receipt, included-ref) pair whose ref carries a
//     well-formed "probe:<class>/<group>; " label (parseProbeGroupLabel, probe_group.go).
//     A ref with no such label (a legacy pack, or a ref that never routed through the
//     probe-group path) contributes to no group at all — it is usage.go's concern, not
//     this file's.
//   - application increments only for provenance-qualified Used refs.
//   - ignored increments only for an exact non-use declaration with receipt
//     work, actor, and proof provenance.
//   - unknown includes transcript references and historical ambiguous receipts.
//
// over every injection event contributing to it) — pinned by this file's tests.
package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

const (
	// ProbeGroupUsageSchemaVersion versions this projection body, independent of
	// CapabilityUsageSchemaVersion — the two projections evolve on separate cadences.
	ProbeGroupUsageSchemaVersion = "compound.probe_group_usage.v0.1"
	// ProbeGroupUsageProjectionRef is the stable ref for this generated view.
	ProbeGroupUsageProjectionRef = "compound://projections/probe-group-usage"
	// ProbeGroupUsageRelPath is the home-relative path the projection is written to.
	ProbeGroupUsageRelPath = "projections/probe-group-usage.json"
)

// ProbeGroupUsage is one probe group's ("<class>/<group>", e.g.
// "system-work/standing-law") aggregate injected/consumed/ignored/unknown counts across
// every scanned receipt. Injected == Consumed+Ignored+Unknown always.
type ProbeGroupUsage struct {
	Group    string `json:"group"`
	Injected int    `json:"injected"`
	Consumed int    `json:"consumed"`
	Ignored  int    `json:"ignored"`
	Unknown  int    `json:"unknown"`
}

// ProbeGroupUsageProjection is the full derived surface written under projections/.
type ProbeGroupUsageProjection struct {
	SchemaVersion       string            `json:"schema_version"`
	ProjectionRef       string            `json:"projection_ref"`
	GeneratedAt         string            `json:"generated_at"`
	Since               string            `json:"since,omitempty"`
	ReceiptsScanned     int               `json:"receipts_scanned"`
	ReceiptsInWindow    int               `json:"receipts_in_window"`
	ContextPacksScanned int               `json:"context_packs_scanned"`
	Groups              []ProbeGroupUsage `json:"groups"`
}

// ProbeGroupUsageBuildOptions parameterizes the sweep, mirroring UsageBuildOptions.
// GeneratedAt is injected so the build is deterministic (byte-identical for equal
// inputs + equal GeneratedAt).
type ProbeGroupUsageBuildOptions struct {
	Since       time.Time // zero = no window filter
	GeneratedAt time.Time // stamped into GeneratedAt; inject for determinism
}

type probeGroupAccum struct {
	injected int
	consumed int
	ignored  int
	unknown  int
}

// BuildProbeGroupUsage sweeps receipts under homePath/events and the context packs they
// reference, and returns the per-probe-group aggregate projection. Fail-open throughout:
// a missing events/ directory, a torn/foreign receipt, or a missing/invalid/unreferenced
// context pack is skipped, never an error that would block the sweep. A home with no
// probe-group-labeled refs at all (every pack predates probe-group selection, or none
// parse) returns a valid, empty (zero-groups) projection — not an error. It never
// mutates the ledger; the caller persists the result via WriteProbeGroupUsage.
func BuildProbeGroupUsage(homePath string, opts ProbeGroupUsageBuildOptions) (ProbeGroupUsageProjection, error) {
	gen := opts.GeneratedAt
	if gen.IsZero() {
		gen = time.Now()
	}
	proj := ProbeGroupUsageProjection{
		SchemaVersion: ProbeGroupUsageSchemaVersion,
		ProjectionRef: ProbeGroupUsageProjectionRef,
		GeneratedAt:   gen.UTC().Format(time.RFC3339),
		Groups:        []ProbeGroupUsage{},
	}
	if !opts.Since.IsZero() {
		proj.Since = opts.Since.UTC().Format(time.RFC3339)
	}

	groups := map[string]*probeGroupAccum{}
	packCache := map[string]*envelope.ContextPack{} // id -> pack (nil = missing/invalid)
	packsLoaded := map[string]bool{}

	events, err := eventstore.Open(homePath)
	if err != nil {
		return proj, err
	}
	entries, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"rec_"}})
	if err != nil {
		if os.IsNotExist(err) {
			return proj, nil
		}
		return proj, err
	}

	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			return proj, err
		}
		data := record.Bytes
		var head struct {
			SchemaVersion string `json:"schema_version"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			continue // torn/foreign file — skip, do not brick the sweep
		}
		if head.SchemaVersion != envelope.SVReceipt {
			continue
		}
		var rec envelope.Receipt
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		if rec.Validate() != nil {
			continue
		}
		proj.ReceiptsScanned++

		if !opts.Since.IsZero() {
			created, perr := time.Parse(time.RFC3339, strings.TrimSpace(rec.CreatedAt))
			if perr == nil && created.Before(opts.Since) {
				continue
			}
		}
		proj.ReceiptsInWindow++

		packID := strings.TrimSpace(rec.IDs.ContextPackID)
		if packID == "" {
			continue
		}
		pack, ok := packCache[packID]
		if !ok {
			pack = loadContextPack(homePath, packID)
			packCache[packID] = pack
			if pack != nil {
				packsLoaded[packID] = true
			}
		}
		if pack == nil {
			continue
		}

		usedSet := dedupeSet(rec.Skills.Used)
		for _, inc := range pack.Included {
			label, ok := parseProbeGroupLabel(inc.Reason)
			if !ok {
				continue // unlabeled ref — usage.go's per-ref join already carries it;
				// this projection only ever buckets labeled refs into a group.
			}
			ref := strings.TrimSpace(inc.Ref)
			if ref == "" {
				continue
			}
			g := groups[label]
			if g == nil {
				g = &probeGroupAccum{}
				groups[label] = g
			}
			g.injected++
			switch {
			case usedSet[ref] && rec.Skills.ApplicationObserved(ref):
				g.consumed++
			case rec.ExplicitNonUseObserved(ref):
				// IGNORED only when the receipt DECLARED its used-list: absence from a
				// declaring receipt is real evidence the ref was not used (mirrors
				// usage.go's identical anti-famine rule).
				g.ignored++
			default:
				// Undeclared receipt: neither consumed nor honestly ignorable — unknown,
				// never a cooling signal.
				g.unknown++
			}
		}
	}
	proj.ContextPacksScanned = len(packsLoaded)

	for label, g := range groups {
		proj.Groups = append(proj.Groups, ProbeGroupUsage{
			Group:    label,
			Injected: g.injected,
			Consumed: g.consumed,
			Ignored:  g.ignored,
			Unknown:  g.unknown,
		})
	}
	sort.Slice(proj.Groups, func(i, j int) bool { return proj.Groups[i].Group < proj.Groups[j].Group })
	return proj, nil
}

// WriteProbeGroupUsage persists the projection under projections/, returning the
// absolute path written. This is derived state; it must never touch the ledger.
func WriteProbeGroupUsage(homePath string, proj ProbeGroupUsageProjection) (string, error) {
	path := filepath.Join(homePath, filepath.FromSlash(ProbeGroupUsageRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
