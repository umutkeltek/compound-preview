// injection_observation.go — work_7df249b46755: the memory-injection observation report.
//
// Authority: docs_private/plan/packets/packet-memory-injection-observation.md, design
// points 1-2 ("condition tagging" + "consumption measure"). The packet's mandate is thin
// instrumentation, no new organs: classify each already-observed session by its
// memory-injection CONDITION (on/off/reduced) purely from what its ContextPack already
// recorded, and read consumption from the receipt that already closed it. This file adds
// no new sweep: it reuses the identical events/*.json + context/packs/*.json walk
// BuildCapabilityUsage performs (usage.go) and the same loadContextPack helper, so the
// two projections can never disagree about which receipts/packs exist. It never writes
// to the ledger, candidates, claims, or projections/ — the caller may persist the
// returned report as a plain timestamped artifact under state/runs/ (see
// WriteInjectionObservationRun), never as ambient injection (falsifier f3).
//
// CONDITION DISCRIMINATOR — a pure function of one ContextPack (no I/O, no clock, no
// ledger access), stated here because the packet requires the report header to carry it
// verbatim (see InjectionObservationDiscriminator):
//
//	ON      pack.ActiveMemory carries at least one claim:// ref — the ambient-recall/
//	        position digest (internal/contract.PackWithCapabilities) actually served
//	        ledger claim content into this session.
//	REDUCED ActiveMemory is empty, but pack.Excluded carries at least one claim:// ref —
//	        the ambient mechanism considered candidate claims (topic scope, policy rule,
//	        or relevance rank) and suppressed every one of them. The mechanism engaged;
//	        the digest came back empty. This is the packet's "pointers-only, ambient
//	        digest suppressed" case kept as its own bucket rather than folded into OFF,
//	        because a future live on/off/reduced toggle would land exactly here.
//	OFF     Neither ActiveMemory nor a claim:// Excluded entry is present — no ambient
//	        memory mechanism engaged at all. This is the retrospective baseline: sessions
//	        that predate ambient memory, or whose repo-kind carries an empty
//	        MemoryRule.Ambient policy (loadMemoryRefs returns nil, nil in that case).
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
	// InjectionObservationSchemaVersion versions the report body.
	InjectionObservationSchemaVersion = "compound.memory_injection_observation.v0.1"

	InjectionConditionOn      = "on"
	InjectionConditionReduced = "reduced"
	InjectionConditionOff     = "off"

	claimRefPrefix = "claim://"

	// injectionHotClaimsTop bounds the per-condition hot-claims list.
	injectionHotClaimsTop = 10

	// InjectionObservationDiscriminator is the condition rule verbatim, stamped into every
	// report so a reader never has to trust an out-of-band description of how "on" /
	// "reduced" / "off" were decided (see the package doc comment above for the full text).
	InjectionObservationDiscriminator = "ON: ContextPack.active_memory has >=1 claim:// ref (ambient digest served). REDUCED: active_memory empty but ContextPack.excluded has >=1 claim:// ref (ambient mechanism engaged, all candidates suppressed). OFF: neither (no ambient memory mechanism engaged)."
)

// ClassifyInjectionCondition is the pure classifier documented above. served is the
// sorted, deduped set of claim:// refs the pack actually served (ActiveMemory);
// excludedCandidates is the sorted, deduped set of claim:// refs the pack considered and
// excluded. Both are always non-nil so callers/tests never branch on nilness.
func ClassifyInjectionCondition(pack envelope.ContextPack) (condition string, served, excludedCandidates []string) {
	served = dedupedClaimRefs(pack.ActiveMemory)
	excluded := make([]string, 0, len(pack.Excluded))
	for _, ex := range pack.Excluded {
		excluded = append(excluded, ex.Ref)
	}
	excludedCandidates = dedupedClaimRefs(excluded)
	switch {
	case len(served) > 0:
		condition = InjectionConditionOn
	case len(excludedCandidates) > 0:
		condition = InjectionConditionReduced
	default:
		condition = InjectionConditionOff
	}
	return condition, served, excludedCandidates
}

func dedupedClaimRefs(refs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if !strings.HasPrefix(r, claimRefPrefix) || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// InjectionSessionRow is one observed session (a closed work's receipt that carries a
// context_pack_id) with its condition and served/consumed claims. "Consumed" here is
// computed directly from THIS receipt's own Skills.Used against THIS pack's served
// claims — a strictly per-session signal, not the cross-session aggregate the
// capability-usage/temperature projections carry (that aggregate is a different,
// coarser view of the same underlying receipts; this table is the session-level join
// the packet's per-session requirement needs).
type InjectionSessionRow struct {
	ReceiptID      string   `json:"receipt_id"`
	WorkID         string   `json:"work_id,omitempty"`
	ContextPackID  string   `json:"context_pack_id"`
	CreatedAt      string   `json:"created_at,omitempty"`
	Condition      string   `json:"condition"`
	ServedClaims   []string `json:"served_claims"`
	ConsumedClaims []string `json:"consumed_claims"`
	ServedCount    int      `json:"served_count"`
	ConsumedCount  int      `json:"consumed_count"`
	UsedDeclared   bool     `json:"used_declared"`
	ExcludedClaims []string `json:"excluded_claims,omitempty"`
}

// InjectionClaimCount is one claim ranked by a count (used for the hot-claims list).
type InjectionClaimCount struct {
	ClaimID string `json:"claim_id"`
	Count   int    `json:"count"`
}

// InjectionConditionSummary is the per-condition rollup the packet's §2/§3 report
// content requires: session count, served-claim counts, the consumed_count
// distribution, the cold-served ratio, and the hot-claims list.
type InjectionConditionSummary struct {
	Condition string `json:"condition"`

	SessionCount int `json:"session_count"`

	// ServedClaimEvents is the sum of served_count across this condition's sessions
	// (one claim served in three sessions counts three times); DistinctServedClaims is
	// the number of unique claim ids served at least once.
	ServedClaimEvents    int `json:"served_claim_events"`
	DistinctServedClaims int `json:"distinct_served_claims"`

	// ConsumedClaimEvents mirrors ServedClaimEvents for consumption.
	ConsumedClaimEvents int `json:"consumed_claim_events"`

	// ConsumedCountDistribution buckets sessions by their own consumed_count: "0","1",
	// "2","3+". Keys are always present (zero-filled) so the shape never depends on
	// which counts happened to occur.
	ConsumedCountDistribution map[string]int `json:"consumed_count_distribution"`

	// ColdServedCount is the number of distinct claims served at least once in this
	// condition's sessions but consumed in NONE of them; ColdServedRatio is that count
	// over DistinctServedClaims (0 when nothing was served — never divides by zero).
	ColdServedCount int     `json:"cold_served_count"`
	ColdServedRatio float64 `json:"cold_served_ratio"`

	// HotClaims ranks claims by total consumed events within this condition, warmest
	// first (ties broken by claim id), capped at injectionHotClaimsTop. Only claims with
	// at least one consumption appear.
	HotClaims []InjectionClaimCount `json:"hot_claims"`
}

// InjectionObservationReport is the full derived, read-only report. It is never written
// under projections/ (that would make it look like ledger-adjacent truth); WriteRun
// persists it as a plain timestamped artifact under state/runs/ instead.
type InjectionObservationReport struct {
	SchemaVersion string `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	Discriminator string `json:"discriminator"`

	ReceiptsScanned         int `json:"receipts_scanned"`
	ReceiptsWithContextPack int `json:"receipts_with_context_pack"`
	ContextPacksMissing     int `json:"context_packs_missing"`

	Sessions   []InjectionSessionRow       `json:"sessions"`
	Conditions []InjectionConditionSummary `json:"conditions"`

	// F2 is the packet's falsifier f2, evaluated directly: "consumed_count=0 across ALL
	// on-condition sessions". OnSessionCount 0 means f2 is not yet evaluable (no ON
	// sessions observed) — AllZero is still reported (vacuously true) but Note says so
	// explicitly; never silently claim the falsifier fired on zero evidence.
	F2AllZero                   bool   `json:"f2_all_on_sessions_zero_consumed"`
	F2OnSessionCount            int    `json:"f2_on_session_count"`
	F2OnSessionsWithConsumption int    `json:"f2_on_sessions_with_consumption"`
	F2Note                      string `json:"f2_note"`
}

// InjectionObservationOptions parameterizes the build. GeneratedAt is injected for
// determinism (byte-identical output for equal inputs + equal GeneratedAt).
type InjectionObservationOptions struct {
	GeneratedAt time.Time
}

// BuildInjectionObservationReport sweeps homePath/events for valid receipts, resolves
// each receipt's context pack, classifies the session's condition, and joins the
// receipt's own used-skills set against the claims its pack served. It is read-only: it
// never mutates events/, context/, the ledger, or projections/.
func BuildInjectionObservationReport(homePath string, opts InjectionObservationOptions) (InjectionObservationReport, error) {
	gen := opts.GeneratedAt
	if gen.IsZero() {
		gen = time.Now()
	}
	rep := InjectionObservationReport{
		SchemaVersion: InjectionObservationSchemaVersion,
		GeneratedAt:   gen.UTC().Format(time.RFC3339),
		Discriminator: InjectionObservationDiscriminator,
		Sessions:      []InjectionSessionRow{},
		Conditions:    []InjectionConditionSummary{},
	}

	events, err := eventstore.Open(homePath)
	if err != nil {
		return rep, err
	}
	entries, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"rec_"}})
	if err != nil {
		if os.IsNotExist(err) {
			rep.Conditions = summarizeInjectionConditions(nil)
			rep.F2AllZero, rep.F2OnSessionCount, rep.F2OnSessionsWithConsumption, rep.F2Note = evaluateInjectionF2(nil)
			return rep, nil
		}
		return rep, err
	}

	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			return rep, err
		}
		data := record.Bytes
		var head struct {
			SchemaVersion string `json:"schema_version"`
		}
		if json.Unmarshal(data, &head) != nil {
			continue // torn/foreign file — skip, do not brick the sweep
		}
		if head.SchemaVersion != envelope.SVReceipt {
			continue
		}
		var rec envelope.Receipt
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		if rec.Validate() != nil {
			continue
		}
		rep.ReceiptsScanned++

		packID := strings.TrimSpace(rec.IDs.ContextPackID)
		if packID == "" {
			continue // never entered ambient recall at all — no session to classify
		}
		rep.ReceiptsWithContextPack++

		pack := loadContextPack(homePath, packID)
		if pack == nil {
			rep.ContextPacksMissing++
			continue
		}

		condition, served, excludedCandidates := ClassifyInjectionCondition(*pack)
		usedSet := dedupeSet(rec.Skills.Used)
		consumed := make([]string, 0, len(served))
		for _, ref := range served {
			if usedSet[ref] && rec.Skills.ApplicationObserved(ref) {
				consumed = append(consumed, ref)
			}
		}

		rep.Sessions = append(rep.Sessions, InjectionSessionRow{
			ReceiptID:      rec.ID,
			WorkID:         rec.IDs.WorkID,
			ContextPackID:  packID,
			CreatedAt:      strings.TrimSpace(rec.CreatedAt),
			Condition:      condition,
			ServedClaims:   served,
			ConsumedClaims: consumed,
			ServedCount:    len(served),
			ConsumedCount:  len(consumed),
			UsedDeclared:   rec.Skills.UsedDeclared,
			ExcludedClaims: excludedCandidates,
		})
	}

	sort.Slice(rep.Sessions, func(i, j int) bool {
		if rep.Sessions[i].CreatedAt != rep.Sessions[j].CreatedAt {
			return rep.Sessions[i].CreatedAt < rep.Sessions[j].CreatedAt
		}
		return rep.Sessions[i].ReceiptID < rep.Sessions[j].ReceiptID
	})

	rep.Conditions = summarizeInjectionConditions(rep.Sessions)
	rep.F2AllZero, rep.F2OnSessionCount, rep.F2OnSessionsWithConsumption, rep.F2Note = evaluateInjectionF2(rep.Sessions)
	return rep, nil
}

// summarizeInjectionConditions builds the per-condition rollup, always emitting all
// three condition buckets (even when a condition has zero sessions) so the report shape
// never depends on what happened to be observed.
func summarizeInjectionConditions(sessions []InjectionSessionRow) []InjectionConditionSummary {
	order := []string{InjectionConditionOn, InjectionConditionReduced, InjectionConditionOff}
	out := make([]InjectionConditionSummary, 0, len(order))
	for _, cond := range order {
		out = append(out, summarizeOneInjectionCondition(cond, sessions))
	}
	return out
}

func summarizeOneInjectionCondition(condition string, sessions []InjectionSessionRow) InjectionConditionSummary {
	sum := InjectionConditionSummary{
		Condition:                 condition,
		ConsumedCountDistribution: map[string]int{"0": 0, "1": 0, "2": 0, "3+": 0},
		HotClaims:                 []InjectionClaimCount{},
	}
	served := map[string]int{}   // claim id -> total served events
	consumed := map[string]int{} // claim id -> total consumed events
	for _, s := range sessions {
		if s.Condition != condition {
			continue
		}
		sum.SessionCount++
		sum.ServedClaimEvents += s.ServedCount
		sum.ConsumedClaimEvents += s.ConsumedCount
		sum.ConsumedCountDistribution[distributionBucket(s.ConsumedCount)]++
		for _, ref := range s.ServedClaims {
			served[ref]++
		}
		for _, ref := range s.ConsumedClaims {
			consumed[ref]++
		}
	}
	sum.DistinctServedClaims = len(served)
	for ref := range served {
		if consumed[ref] == 0 {
			sum.ColdServedCount++
		}
	}
	if sum.DistinctServedClaims > 0 {
		sum.ColdServedRatio = float64(sum.ColdServedCount) / float64(sum.DistinctServedClaims)
	}

	hot := make([]InjectionClaimCount, 0, len(consumed))
	for ref, count := range consumed {
		hot = append(hot, InjectionClaimCount{ClaimID: strings.TrimPrefix(ref, claimRefPrefix), Count: count})
	}
	sort.Slice(hot, func(i, j int) bool {
		if hot[i].Count != hot[j].Count {
			return hot[i].Count > hot[j].Count
		}
		return hot[i].ClaimID < hot[j].ClaimID
	})
	if len(hot) > injectionHotClaimsTop {
		hot = hot[:injectionHotClaimsTop]
	}
	sum.HotClaims = hot
	return sum
}

func distributionBucket(consumedCount int) string {
	switch {
	case consumedCount <= 0:
		return "0"
	case consumedCount == 1:
		return "1"
	case consumedCount == 2:
		return "2"
	default:
		return "3+"
	}
}

// evaluateInjectionF2 checks the packet's falsifier f2 directly against the session
// table: consumed_count=0 across every ON-condition session. onCount 0 means there is no
// evidence yet either way (allZero is reported true — vacuously — but Note says so).
func evaluateInjectionF2(sessions []InjectionSessionRow) (allZero bool, onCount, onWithConsumption int, note string) {
	for _, s := range sessions {
		if s.Condition != InjectionConditionOn {
			continue
		}
		onCount++
		if s.ConsumedCount > 0 {
			onWithConsumption++
		}
	}
	allZero = onWithConsumption == 0
	switch {
	case onCount == 0:
		note = "no ON-condition sessions observed yet; f2 not yet falsifiable"
	case allZero:
		note = "every ON-condition session shows consumed_count=0 (f2 fires: injection as shaped is pure cost)"
	default:
		note = "on-condition sessions with nonzero consumption observed; f2 does not fire"
	}
	return allZero, onCount, onWithConsumption, note
}

// WriteInjectionObservationRun persists the report as a plain timestamped artifact
// under homePath/state/runs/memory-injection-observation/ — the RUNTIME home, never the
// repo, and never projections/ (this is an observation artifact, not derived ledger
// state). It never mutates the ledger, candidates, claims, or any projection.
func WriteInjectionObservationRun(homePath string, report InjectionObservationReport, now time.Time) (string, error) {
	dir := filepath.Join(homePath, "state", "runs", "memory-injection-observation")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	stamp := now.UTC().Format("20060102T150405Z")
	path := filepath.Join(dir, stamp+".json")
	b, err := json.MarshalIndent(&report, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
