// usage.go — the capability/skill usage telemetry projection.
//
// This generated view folds two evidence streams: immediate hook observations
// (task-scoped capability offers and completed Skill tool calls) plus close
// receipts and their referenced ContextPacks. Hook rows make a fired skill
// observable before SessionEnd; receipts retain declared-use/missed/ignored
// correlation, not independent validation.
//
// Honest attribution: application requires an explicit method; reference scans
// record mentions only. Non-use requires an exact actor- and proof-bound declaration.
// Historical used_declared alone gives neither positive nor negative claim evidence.
//
// It is a projection, never truth: rebuilt from scratch on every call and written
// only under projections/ (never the ledger or capability registry). See
// docs/architecture/runtime.md (§ home layout) and epistemics (generated view can
// prove freshness/drift, it cannot promote truth).
package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
)

const (
	// CapabilityUsageSchemaVersion versions the projection body.
	CapabilityUsageSchemaVersion = "compound.capability_usage.v0.2"
	// CapabilityUsageProjectionRef is the stable ref for this generated view.
	CapabilityUsageProjectionRef = "compound://projections/capability-usage"
	// CapabilityUsageRelPath is the home-relative path the projection is written to.
	CapabilityUsageRelPath = "projections/capability-usage.json"

	defaultSourceReceiptCap = 5
	defaultSummaryTop       = 20
)

// CapabilityUsage is per-capability/skill-ref usage telemetry.
type CapabilityUsage struct {
	Ref          string `json:"ref"`
	OfferedCount int    `json:"offered_count"`
	// UsedCount is the historical alias for DeclaredUseCount, not observed execution.
	UsedCount                   int `json:"used_count"`
	DeclaredUseCount            int `json:"declared_use_count"`
	AcceptedUseDeclarationCount int `json:"accepted_use_declaration_count"`
	FiredCount                  int `json:"fired_count"`
	// ValidatedCount remains zero: this projection does not join independent evaluations.
	ValidatedCount      int      `json:"validated_count"`
	MissedCount         int      `json:"missed_count"`
	FalseTriggeredCount int      `json:"false_triggered_count"`
	LastUsed            string   `json:"last_used,omitempty"`
	LastFired           string   `json:"last_fired,omitempty"`
	LastOffered         string   `json:"last_offered,omitempty"`
	SourceReceipts      []string `json:"source_receipts"`
	SourceEvents        []string `json:"source_events"`
}

// CapabilityUsageEvent is one immediate offer/use observation. Trigger is a
// bounded explanation from CapabilityOffer.Why or normalized Skill tool args.
type CapabilityUsageEvent struct {
	EventID       string `json:"event_id"`
	OccurredAt    string `json:"occurred_at"`
	Ref           string `json:"ref"`
	Skill         string `json:"skill"`
	Harness       string `json:"harness"`
	Lane          string `json:"lane"`
	WorkID        string `json:"work_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	ContextPackID string `json:"context_pack_id,omitempty"`
	HookName      string `json:"hook_name"`
	Trigger       string `json:"trigger,omitempty"`
	Outcome       string `json:"outcome"`
}

// InjectedRefUsage is per-ContextPack.included[] ref reinforcement telemetry:
// injected (loaded into a work's context) vs consumed (echoed in that work's
// receipt used refs) vs ignored (injected but never used).
//
// ProbeGroup is additive (work_8713c4206232, S4): "<class>/<group>" parsed from the
// ref's IncludedRef.Reason via parseProbeGroupLabel (probe_group.go) when the ref was
// selected through internal/contract's probe-group path (probe_contracts.go). It is
// omitempty and left unset for a legacy ref (no probe label at all) or a ref whose label
// never parses (malformed) — so a pre-probe-group fixture's projection JSON stays
// byte-identical (pinned by TestBuildCapabilityUsageLegacyPacksOmitProbeGroup).
type InjectedRefUsage struct {
	Ref             string   `json:"ref"`
	ReferencedCount int      `json:"referenced_count,omitempty"`
	SourceClass     string   `json:"source_class,omitempty"`
	ProbeGroup      string   `json:"probe_group,omitempty"`
	InjectedCount   int      `json:"injected_count"`
	ConsumedCount   int      `json:"consumed_count"`
	IgnoredCount    int      `json:"ignored_count"`
	LastInjected    string   `json:"last_injected,omitempty"`
	SourceReceipts  []string `json:"source_receipts"`
}

// FailureCauseCount aggregates the optional close-receipt failure_cause field.
type FailureCauseCount struct {
	SurfaceOutcome string `json:"surface_outcome"`
	Mechanism      string `json:"mechanism"`
	Count          int    `json:"count"`
}

// UsageSummaryEntry is one ranked ref in a summary list.
type UsageSummaryEntry struct {
	Ref   string `json:"ref"`
	Count int    `json:"count"`
}

// UsageSummary is the operator/Bitter-Lesson-watch quick view.
type UsageSummary struct {
	// TopIgnored ranks injected refs by how often they were injected but never used.
	TopIgnored []UsageSummaryEntry `json:"top_ignored"`
	// NeverFired is the historical field for offers with no declared use or observed
	// skill call, ranked by offered_count. Missing evidence does not prove non-use.
	NeverFired []UsageSummaryEntry `json:"never_fired"`
}

// CapabilityUsageProjection is the full derived surface.
type CapabilityUsageProjection struct {
	SchemaVersion       string                 `json:"schema_version"`
	ProjectionRef       string                 `json:"projection_ref"`
	GeneratedAt         string                 `json:"generated_at"`
	Since               string                 `json:"since,omitempty"`
	ReceiptsScanned     int                    `json:"receipts_scanned"`
	ReceiptsInWindow    int                    `json:"receipts_in_window"`
	HookEventsScanned   int                    `json:"hook_events_scanned"`
	HookEventsInWindow  int                    `json:"hook_events_in_window"`
	ContextPacksScanned int                    `json:"context_packs_scanned"`
	Capabilities        []CapabilityUsage      `json:"capabilities"`
	Events              []CapabilityUsageEvent `json:"events"`
	InjectedRefs        []InjectedRefUsage     `json:"injected_refs"`
	FailureCauses       []FailureCauseCount    `json:"failure_causes"`
	Summary             UsageSummary           `json:"summary"`
}

// UsageBuildOptions parameterizes the sweep. GeneratedAt is injected so the build
// is deterministic (byte-identical for equal inputs + equal GeneratedAt).
type UsageBuildOptions struct {
	Since            time.Time // zero = no window filter
	GeneratedAt      time.Time // stamped into GeneratedAt; inject for determinism
	SourceReceiptCap int       // per-ref evidence cap (last N); 0 => default
	SummaryTop       int       // summary list length; 0 => default
}

// --- internal accumulators ---

type refReceipt struct {
	id string
	ts string
}

type capAccum struct {
	offered                 int
	used                    int
	fired                   int
	acceptedUseDeclarations int
	missed                  int
	falseTriggered          int
	lastUsed                string
	lastFired               string
	lastOffered             string
	offerKeys               map[string]bool
	usedIn                  []refReceipt
	firedIn                 []refReceipt
}

type injAccum struct {
	sourceClass  string
	probeGroup   string
	injected     int
	referenced   int
	consumed     int
	ignored      int
	lastInjected string
	injectedIn   []refReceipt
}

// BuildCapabilityUsage sweeps receipts under homePath/events and the context packs
// they reference, and returns the aggregated usage projection. It never mutates the
// ledger; the caller persists the result under projections/ via WriteCapabilityUsage.
func BuildCapabilityUsage(homePath string, opts UsageBuildOptions) (CapabilityUsageProjection, error) {
	srcCap := opts.SourceReceiptCap
	if srcCap <= 0 {
		srcCap = defaultSourceReceiptCap
	}
	top := opts.SummaryTop
	if top <= 0 {
		top = defaultSummaryTop
	}
	gen := opts.GeneratedAt
	if gen.IsZero() {
		gen = time.Now()
	}

	proj := CapabilityUsageProjection{
		SchemaVersion: CapabilityUsageSchemaVersion,
		ProjectionRef: CapabilityUsageProjectionRef,
		GeneratedAt:   gen.UTC().Format(time.RFC3339),
		Capabilities:  []CapabilityUsage{},
		Events:        []CapabilityUsageEvent{},
		InjectedRefs:  []InjectedRefUsage{},
		FailureCauses: []FailureCauseCount{},
		Summary:       UsageSummary{TopIgnored: []UsageSummaryEntry{}, NeverFired: []UsageSummaryEntry{}},
	}
	if !opts.Since.IsZero() {
		proj.Since = opts.Since.UTC().Format(time.RFC3339)
	}

	caps := map[string]*capAccum{}
	injs := map[string]*injAccum{}
	failures := map[string]*FailureCauseCount{}
	packCache := map[string]*envelope.ContextPack{} // id -> pack (nil = missing/invalid)
	packsLoaded := map[string]bool{}

	// The graph's node set, so a recorded tool invocation can be attributed
	// to the capability it evidences. A tool the graph does not hold is
	// UNMEASURABLE, and attributing it anyway would put a capability in
	// the projection that no catalog ever declared.
	//
	// The keys are NODE ids ("tool:headroom.headroom-retrieve"): that is the
	// graph's own vocabulary, and the projection converts to a capability
	// ref when it counts.
	nodeIDs := map[string]bool{}
	nodeNameIDs := map[string][]string{}
	if store, storeErr := Open(homePath); storeErr == nil {
		if graphNodes, nodesErr := store.List(); nodesErr == nil {
			for _, n := range graphNodes {
				id := strings.TrimSpace(n.ID)
				if id == "" {
					continue
				}
				nodeIDs[id] = true
				if name := strings.TrimSpace(strings.ToLower(n.Name)); name != "" {
					nodeNameIDs[name] = append(nodeNameIDs[name], id)
				}
			}
		}
	}

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

		ts := strings.TrimSpace(rec.CreatedAt)
		acceptedUseDeclaration := rec.Outcome == "completed" && rec.EffectiveChanged()
		usedSet := dedupeUsageSet(rec.Skills.Used)
		packID := strings.TrimSpace(rec.IDs.ContextPackID)

		for ref := range usedSet {
			if strings.HasPrefix(ref, "claim://") && !rec.Skills.ApplicationObserved(ref) {
				continue
			}
			a := capFor(caps, ref)
			a.used++
			if acceptedUseDeclaration {
				a.acceptedUseDeclarations++
			}
			a.lastUsed = laterTS(a.lastUsed, ts)
			a.usedIn = append(a.usedIn, refReceipt{id: rec.ID, ts: ts})
		}
		offerKey := "receipt:" + rec.ID
		if packID != "" {
			offerKey = "pack:" + packID
		}
		offeredSet := dedupeUsageSet(append(append([]string{}, rec.Skills.CapabilitiesOffered...), rec.Skills.Candidate...))
		for ref := range offeredSet {
			addCapabilityOffer(capFor(caps, ref), offerKey, ts)
		}
		for ref := range dedupeUsageSet(rec.Skills.Missed) {
			capFor(caps, ref).missed++
		}
		for ref := range dedupeUsageSet(rec.Skills.FalseTriggered) {
			capFor(caps, ref).falseTriggered++
		}

		if rec.FailureCause != nil {
			so := strings.TrimSpace(rec.FailureCause.SurfaceOutcome)
			mech := strings.TrimSpace(rec.FailureCause.Mechanism)
			if so != "" || mech != "" {
				key := so + "\x00" + mech
				if fc := failures[key]; fc != nil {
					fc.Count++
				} else {
					failures[key] = &FailureCauseCount{SurfaceOutcome: so, Mechanism: mech, Count: 1}
				}
			}
		}

		// injected-ref reinforcement: resolve the receipt's context pack and join
		// its included refs against this receipt's used set.
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
		for _, inc := range pack.Included {
			ref := strings.TrimSpace(inc.Ref)
			if ref == "" {
				continue
			}
			a := injs[ref]
			if a == nil {
				a = &injAccum{}
				injs[ref] = a
			}
			a.injected++
			if rec.Skills.ReferenceObserved(ref) {
				a.referenced++
			}
			if strings.TrimSpace(inc.SourceClass) != "" {
				a.sourceClass = inc.SourceClass
			}
			if label, ok := parseProbeGroupLabel(inc.Reason); ok {
				a.probeGroup = label
			}
			if usedSet[ref] && rec.Skills.ApplicationObserved(ref) {
				a.consumed++
			} else if rec.ExplicitNonUseObserved(ref) {
				// Exact sourced non-use declaration, with receipt work and actor provenance.
				a.ignored++
			}
			// else: undeclared receipt — the injected ref stays at its injected count only. Absence of
			// a declaration is UNKNOWN, never ignored (mirrors temperature's "never-injected = neutral").
			a.lastInjected = laterTS(a.lastInjected, ts)
			a.injectedIn = append(a.injectedIn, refReceipt{id: rec.ID, ts: ts})
		}
	}
	hookEntries, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"hook_"}})
	if err != nil && !os.IsNotExist(err) {
		return proj, err
	}
	hookEvents := make([]envelope.HookEvent, 0, len(hookEntries))
	for _, entry := range hookEntries {
		record, readErr := entry.Read()
		if readErr != nil {
			return proj, readErr
		}
		var ev envelope.HookEvent
		if json.Unmarshal(record.Bytes, &ev) != nil || ev.Validate() != nil {
			continue
		}
		proj.HookEventsScanned++
		if !opts.Since.IsZero() {
			created, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(ev.CreatedAt))
			if parseErr == nil && created.Before(opts.Since) {
				continue
			}
		}
		proj.HookEventsInWindow++
		hookEvents = append(hookEvents, ev)
	}
	sort.Slice(hookEvents, func(i, j int) bool {
		if hookEvents[i].CreatedAt != hookEvents[j].CreatedAt {
			return hookEvents[i].CreatedAt < hookEvents[j].CreatedAt
		}
		return hookEvents[i].ID < hookEvents[j].ID
	})

	offerTriggers := map[string]string{}
	usageEventKeys := map[string]bool{}
	for _, ev := range hookEvents {
		packID := strings.TrimSpace(ev.IDs.ContextPackID)
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
		for _, offer := range pack.CapabilityOffers {
			// A pack offers capabilities of every kind -- tools, instruction
			// rules, lenses, doctrines -- and a DROPPED offer is an offer
			// nobody can account for: the capability was served, and the
			// projection reported it as never touched. Only a ref that names
			// no known kind is dropped.
			ref, ok := normalizeCapabilityRef(offer.Ref)
			if !ok {
				continue
			}
			trigger := boundedUsageTrigger(offer.Why)
			if ev.IDs.SessionID != "" && trigger != "" {
				offerTriggers[ev.IDs.SessionID+"\x00"+ref] = trigger
			}
			addCapabilityOffer(capFor(caps, ref), "pack:"+packID, ev.CreatedAt)
			rowKey := "offered\x00" + packID + "\x00" + ref
			if usageEventKeys[rowKey] {
				continue
			}
			usageEventKeys[rowKey] = true
			proj.Events = append(proj.Events, CapabilityUsageEvent{
				EventID:       ev.ID,
				OccurredAt:    ev.CreatedAt,
				Ref:           ref,
				Skill:         capabilityRefName(ref),
				Harness:       ev.Harness,
				Lane:          usageLane(ev, ""),
				WorkID:        ev.IDs.WorkID,
				SessionID:     ev.IDs.SessionID,
				ContextPackID: packID,
				HookName:      ev.HookName,
				Trigger:       trigger,
				Outcome:       "offered",
			})
		}
	}
	for _, ev := range hookEvents {
		use := ev.CapabilityUse
		if use == nil {
			continue
		}
		// A use names a capability of any kind. A skill use keeps the
		// historical collapse; a tool use names its node id directly.
		ref, ok := normalizeCapabilityRef(use.Ref)
		if !ok {
			continue
		}
		trigger := boundedUsageTrigger(use.Trigger)
		if trigger == "" {
			trigger = offerTriggers[ev.IDs.SessionID+"\x00"+ref]
		}
		a := capFor(caps, ref)
		a.fired++
		a.lastFired = laterTS(a.lastFired, ev.CreatedAt)
		a.firedIn = append(a.firedIn, refReceipt{id: ev.ID, ts: ev.CreatedAt})
		proj.Events = append(proj.Events, CapabilityUsageEvent{
			EventID:    ev.ID,
			OccurredAt: ev.CreatedAt,
			Ref:        ref,
			Skill:      capabilityRefName(ref),
			Harness:    ev.Harness,
			Lane:       usageLane(ev, use.Lane),
			WorkID:     ev.IDs.WorkID,
			SessionID:  ev.IDs.SessionID,
			HookName:   ev.HookName,
			Trigger:    trigger,
			Outcome:    use.Outcome,
		})
	}
	// A recorded TOOL invocation is evidence of a tool's use. The event
	// carries the native name; the graph decides which node ids it
	// evidences. This is the path that was missing: a tool invoked
	// thousands of times had no way to reach a fired count, so it read as
	// never fired -- the measurement defect, not a behaviour fact.
	for _, ev := range hookEvents {
		// The invoked tool's NATIVE name, read from the event's
		// side_effects where the hook has always recorded it.
		for _, native := range toolInvocations(ev) {
			for _, ref := range toolUseRefs(native, nodeIDs, nodeNameIDs) {
				a := capFor(caps, ref)
				a.fired++
				a.lastFired = laterTS(a.lastFired, ev.CreatedAt)
				a.firedIn = append(a.firedIn, refReceipt{id: ev.ID, ts: ev.CreatedAt})
				proj.Events = append(proj.Events, CapabilityUsageEvent{
					EventID:    ev.ID,
					OccurredAt: ev.CreatedAt,
					Ref:        ref,
					Skill:      capabilityRefName(ref),
					Harness:    ev.Harness,
					Lane:       usageLane(ev, ""),
					WorkID:     ev.IDs.WorkID,
					SessionID:  ev.IDs.SessionID,
					HookName:   ev.HookName,
					Outcome:    "used",
				})
			}
		}
	}
	sort.Slice(proj.Events, func(i, j int) bool {
		if proj.Events[i].OccurredAt != proj.Events[j].OccurredAt {
			return proj.Events[i].OccurredAt < proj.Events[j].OccurredAt
		}
		if proj.Events[i].EventID != proj.Events[j].EventID {
			return proj.Events[i].EventID < proj.Events[j].EventID
		}
		return proj.Events[i].Outcome < proj.Events[j].Outcome
	})

	proj.ContextPacksScanned = len(packsLoaded)
	// materialize capabilities, sorted by ref.
	for ref, a := range caps {
		proj.Capabilities = append(proj.Capabilities, CapabilityUsage{
			Ref:                         ref,
			OfferedCount:                a.offered,
			UsedCount:                   a.used,
			DeclaredUseCount:            a.used,
			AcceptedUseDeclarationCount: a.acceptedUseDeclarations,
			FiredCount:                  a.fired,
			MissedCount:                 a.missed,
			FalseTriggeredCount:         a.falseTriggered,
			LastUsed:                    a.lastUsed,
			LastFired:                   a.lastFired,
			LastOffered:                 a.lastOffered,
			SourceReceipts:              lastReceiptIDs(a.usedIn, srcCap),
			SourceEvents:                lastReceiptIDs(a.firedIn, srcCap),
		})
	}
	sort.Slice(proj.Capabilities, func(i, j int) bool {
		return proj.Capabilities[i].Ref < proj.Capabilities[j].Ref
	})

	// materialize injected refs, sorted by ref.
	for ref, a := range injs {
		proj.InjectedRefs = append(proj.InjectedRefs, InjectedRefUsage{
			Ref:             ref,
			SourceClass:     a.sourceClass,
			ProbeGroup:      a.probeGroup,
			InjectedCount:   a.injected,
			ConsumedCount:   a.consumed,
			ReferencedCount: a.referenced,
			IgnoredCount:    a.ignored,
			LastInjected:    a.lastInjected,
			SourceReceipts:  lastReceiptIDs(a.injectedIn, srcCap),
		})
	}
	sort.Slice(proj.InjectedRefs, func(i, j int) bool {
		return proj.InjectedRefs[i].Ref < proj.InjectedRefs[j].Ref
	})

	// materialize failure causes, sorted by (surface_outcome, mechanism).
	for _, fc := range failures {
		proj.FailureCauses = append(proj.FailureCauses, *fc)
	}
	sort.Slice(proj.FailureCauses, func(i, j int) bool {
		if proj.FailureCauses[i].SurfaceOutcome != proj.FailureCauses[j].SurfaceOutcome {
			return proj.FailureCauses[i].SurfaceOutcome < proj.FailureCauses[j].SurfaceOutcome
		}
		return proj.FailureCauses[i].Mechanism < proj.FailureCauses[j].Mechanism
	})

	proj.Summary = buildUsageSummary(proj.Capabilities, proj.InjectedRefs, top)
	return proj, nil
}

// WriteCapabilityUsage persists the projection under projections/, returning the
// absolute path written. This is derived state; it must never touch the ledger.
func WriteCapabilityUsage(homePath string, proj CapabilityUsageProjection) (string, error) {
	path := filepath.Join(homePath, filepath.FromSlash(CapabilityUsageRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return "", err
	}
	// Atomic: BuildTemperature reads this body, and that chain terminates on the live
	// orient path. See home.WriteFileAtomic.
	if err := home.WriteFileAtomic(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func buildUsageSummary(capsList []CapabilityUsage, injList []InjectedRefUsage, top int) UsageSummary {
	sum := UsageSummary{TopIgnored: []UsageSummaryEntry{}, NeverFired: []UsageSummaryEntry{}}

	ignored := make([]UsageSummaryEntry, 0, len(injList))
	for _, inj := range injList {
		if inj.IgnoredCount > 0 && inj.ConsumedCount == 0 {
			ignored = append(ignored, UsageSummaryEntry{Ref: inj.Ref, Count: inj.IgnoredCount})
		}
	}
	sort.Slice(ignored, func(i, j int) bool {
		if ignored[i].Count != ignored[j].Count {
			return ignored[i].Count > ignored[j].Count
		}
		return ignored[i].Ref < ignored[j].Ref
	})
	sum.TopIgnored = capEntries(ignored, top)

	never := make([]UsageSummaryEntry, 0, len(capsList))
	for _, c := range capsList {
		if c.OfferedCount > 0 && c.UsedCount == 0 && c.FiredCount == 0 {
			never = append(never, UsageSummaryEntry{Ref: c.Ref, Count: c.OfferedCount})
		}
	}
	sort.Slice(never, func(i, j int) bool {
		if never[i].Count != never[j].Count {
			return never[i].Count > never[j].Count
		}
		return never[i].Ref < never[j].Ref
	})
	sum.NeverFired = capEntries(never, top)
	return sum
}

func capEntries(entries []UsageSummaryEntry, top int) []UsageSummaryEntry {
	if top > 0 && len(entries) > top {
		entries = entries[:top]
	}
	if entries == nil {
		return []UsageSummaryEntry{}
	}
	return entries
}

func capFor(m map[string]*capAccum, ref string) *capAccum {
	a := m[ref]
	if a == nil {
		a = &capAccum{offerKeys: map[string]bool{}}
		m[ref] = a
	}
	return a
}

func addCapabilityOffer(a *capAccum, key, ts string) {
	if a.offerKeys[key] {
		return
	}
	a.offerKeys[key] = true
	a.offered++
	a.lastOffered = laterTS(a.lastOffered, ts)
}

func dedupeSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			out[t] = true
		}
	}
	return out
}

func dedupeUsageSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		ref := strings.TrimSpace(value)
		// A receipt may declare a use of any capability kind. Normalizing a
		// skill ref keeps its historical canonical form; a non-skill ref is
		// already canonical, and dropping it here would erase a declared
		// use of a tool, rule or lens before it could be counted.
		if normalized, ok := normalizeCapabilityRef(ref); ok {
			ref = normalized
		}
		if ref != "" {
			out[ref] = true
		}
	}
	return out
}

// normalizeSkillRef normalizes a SKILL ref to its canonical
// "capability://skill:<name>" form. It is deliberately skill-only: a skill's
// name IS its identity, so "skill://x", "skill:x" and
// "capability://skill:x" are the same capability and collapse to one form.
//
// It is NOT the general capability-ref normalizer. A tool, rule, lens or
// doctrine carries a node id, not a name, and "tool:headroom" is not
// "capability://tool:headroom" by spelling alone -- only the graph knows
// which ids exist. See normalizeCapabilityRef.
func normalizeSkillRef(raw string) (string, bool) {
	ref := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(ref, "capability://skill:"):
		ref = strings.TrimPrefix(ref, "capability://skill:")
	case strings.HasPrefix(ref, "skill://"):
		ref = strings.TrimPrefix(ref, "skill://")
	case strings.HasPrefix(ref, "skill:"):
		ref = strings.TrimPrefix(ref, "skill:")
	default:
		return "", false
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	return "capability://skill:" + ref, true
}

// normalizeCapabilityRef normalizes a capability ref of ANY kind to its
// canonical "capability://<kind>:<name>" form.
//
// The defect this exists to fix: a capability that is not a skill (a tool, an
// instruction rule, a lens, a doctrine) could never reach a fired or
// declared-use count, because every consumer funnelled through
// normalizeSkillRef, which accepts skill refs only. A tool offered 800 times
// and invoked 2,269 times still read as never fired, because the EVIDENCE
// EXISTED and the READER COULD NOT HOLD IT.
//
// A skill ref keeps its historical collapse (see normalizeSkillRef) so no
// existing skill row changes identity. Every other kind is accepted as
// already-canonical: "capability://tool:headroom.headroom-retrieve" stays
// exactly that. The graph decides which ids exist; this only decides that a
// ref is well-formed and names a known kind.
func normalizeCapabilityRef(raw string) (string, bool) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return "", false
	}
	if skill, ok := normalizeSkillRef(ref); ok {
		return skill, true
	}
	rest, ok := strings.CutPrefix(ref, "capability://")
	if !ok {
		return "", false
	}
	kind, name, ok := strings.Cut(rest, ":")
	if !ok {
		return "", false
	}
	kind = strings.TrimSpace(kind)
	name = strings.TrimSpace(name)
	if kind == "" || name == "" {
		return "", false
	}
	if !capabilityRefKindRe.MatchString(kind) {
		return "", false
	}
	if !capabilityRefNameRe.MatchString(name) {
		return "", false
	}
	return "capability://" + kind + ":" + name, true
}

// toolInvocations returns the native tool names a hook event observed.
//
// The tool name is read from the event's side_effects, where the hook has
// ALWAYS merged it: a PostToolUse event records the invoked tool alongside
// the adapter's governed artifacts. Reading it there rather than from a new
// envelope field means the repair reaches the entire existing history, and
// it leaves the shared event contract untouched.
//
// A side effect is only treated as a tool invocation when it HAS the shape of
// one -- an "mcp__server__method" name, or a bare native tool name with no
// path, scheme or space in it. The hook also merges non-tool refs into
// side_effects (source-control refs, output refs, registry paths), and those
// must never be read as tool calls.
func toolInvocations(ev envelope.HookEvent) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, effect := range ev.SideEffects {
		name := strings.TrimSpace(effect)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "mcp__") {
			add(name)
			continue
		}
		if !strings.ContainsAny(name, "/:@ ") && !strings.HasPrefix(name, "compound://") {
			add(name)
		}
	}
	return out
}

// toolUseRefs resolves a recorded native tool invocation to the capability
// node ids it evidences.
//
// A harness invokes an MCP tool by its NATIVE name ("mcp__server__method"),
// which the graph does not store: the graph stores the catalog's own id
// ("tool:server.method", slugged) and name ("server.method", raw). This maps
// one to the others using the SAME slug convention the catalog builder used
// (internal/tooltopology: lowercase, non-alphanumeric runs joined by "-"),
// so the attribution is derived, not guessed.
//
// It returns nothing when the invocation names a capability the graph does
// not hold: a server with no node is UNMEASURABLE, and inventing a node for
// it would put a capability in the graph that no catalog ever declared.
//
// The method-level tool node and the server-level mcp node are BOTH credited:
// invoking headroom.headroom_retrieve is evidence for the tool AND for the
// server that carries it, and the graph holds them as separate capabilities.
func toolUseRefs(native string, nodes map[string]bool, nameToID map[string][]string) []string {
	native = strings.TrimSpace(native)
	if !strings.HasPrefix(native, "mcp__") {
		return nil
	}
	body := strings.TrimPrefix(native, "mcp__")
	parts := strings.Split(body, "__")
	if len(parts) < 2 {
		return nil
	}
	server := parts[0]
	method := strings.Join(parts[1:], "__")
	if server == "" || method == "" {
		return nil
	}
	var out []string
	// The graph names its nodes "tool:<server>.<method>"; a capability REF
	// is the same identity with the "capability://" scheme. Resolve in node
	// space (the graph's own vocabulary) and convert to ref space, so the
	// projection counts the same identity the graph holds.
	add := func(nodeID string) {
		if nodeID == "" || !nodes[nodeID] {
			return
		}
		ref := "capability://" + nodeID
		for _, existing := range out {
			if existing == ref {
				return
			}
		}
		out = append(out, ref)
	}
	// The catalog's canonical id form: "tool:<slug(server)>.<slug(method)>".
	add("tool:" + usageSlug(server) + "." + usageSlug(method))
	// The server-level node the method belongs to.
	add("mcp:" + usageSlug(server))
	// The catalog's raw name form, for a catalog that kept the name verbatim.
	for _, id := range nameToID[strings.ToLower(server+"."+method)] {
		add(id)
	}
	return out
}

// usageSlug is the graph's canonical id slug: lowercase, non-alphanumeric
// runs joined by "-". It is the same rule the topology catalog builder uses
// to mint "tool:<server>.<method>" ids, so a native invocation and a graph
// id land on the same spelling.
func usageSlug(s string) string {
	var parts []string
	for _, part := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "-")
}

// capabilityRefName returns the NAME inside a canonical capability ref, for
// any kind. It is the honest value for a usage row's Skill column: a skill
// row shows the skill name, and a tool row shows the tool node's name
// ("headroom.headroom-retrieve"), because the graph's own node name is the
// only thing that identifies it.
func capabilityRefName(ref string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(ref), "capability://")
	if !ok {
		return ""
	}
	_, name, ok := strings.Cut(rest, ":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

// capabilityRefKindRe is the bounded grammar of a capability kind: the
// graph's own node-kind vocabulary, lower case, no separators.
var capabilityRefKindRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// capabilityRefNameRe is the bounded grammar of a capability name inside a
// ref: the same character class a skill name uses, so a ref can never carry
// a path, a URI or whitespace.
var capabilityRefNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

func boundedUsageTrigger(raw string) string {
	trigger := strings.Join(strings.Fields(raw), " ")
	runes := []rune(trigger)
	if len(runes) > 120 {
		trigger = string(runes[:120])
	}
	return trigger
}

func usageLane(ev envelope.HookEvent, stated string) string {
	for _, lane := range []string{stated, ev.Producer.LaneID, ev.Harness} {
		if lane = strings.TrimSpace(lane); lane != "" {
			return lane
		}
	}
	return ""
}

// lastReceiptIDs returns the ids of the last N receipts (by created_at, then id),
// in chronological order — bounded evidence, deterministic.
func lastReceiptIDs(list []refReceipt, srcCap int) []string {
	sort.Slice(list, func(i, j int) bool {
		if list[i].ts != list[j].ts {
			return list[i].ts < list[j].ts
		}
		return list[i].id < list[j].id
	})
	if srcCap > 0 && len(list) > srcCap {
		list = list[len(list)-srcCap:]
	}
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.id)
	}
	return out
}

// laterTS returns the later of two RFC3339 timestamps; parse failures sort as zero.
func laterTS(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	ta, ea := time.Parse(time.RFC3339, a)
	tb, eb := time.Parse(time.RFC3339, b)
	if ea != nil || eb != nil {
		if a >= b {
			return a
		}
		return b
	}
	if tb.After(ta) {
		return b
	}
	return a
}

func loadContextPack(homePath, packID string) *envelope.ContextPack {
	path := filepath.Join(homePath, "context", "packs", packID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pack envelope.ContextPack
	if json.Unmarshal(data, &pack) != nil {
		return nil
	}
	if pack.Validate() != nil {
		return nil
	}
	return &pack
}
