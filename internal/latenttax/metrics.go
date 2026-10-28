package latenttax

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/routeevidence"
)

// Dist is a full numeric distribution: summary stats PLUS the raw values. The rig
// carries raw rows on purpose — the pre-registration demands full distributions and
// no cherry-picking, so aggregates never stand alone.
type Dist struct {
	N      int       `json:"n"`
	Min    float64   `json:"min"`
	Max    float64   `json:"max"`
	Mean   float64   `json:"mean"`
	Median float64   `json:"median"`
	P25    float64   `json:"p25"`
	P75    float64   `json:"p75"`
	Values []float64 `json:"values"`
}

func distOf(vals []float64) Dist {
	d := Dist{Values: append([]float64{}, vals...)}
	d.N = len(vals)
	if d.N == 0 {
		return d
	}
	sorted := append([]float64{}, vals...)
	sort.Float64s(sorted)
	d.Min = sorted[0]
	d.Max = sorted[d.N-1]
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	d.Mean = sum / float64(d.N)
	d.Median = percentile(sorted, 0.50)
	d.P25 = percentile(sorted, 0.25)
	d.P75 = percentile(sorted, 0.75)
	return d
}

// percentile uses nearest-rank on a pre-sorted slice — deterministic, no interpolation
// ambiguity, and stable for the small N the rig sees per arm.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(math.Round(p * float64(n-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// RunMetric is the derived per-run row: thrash from PolicyDecision denials, tokens +
// latency from route evidence, success from the recorded verdict. Every field is
// READ (or copied from the run record); nothing is recomputed by a parallel scorer.
type RunMetric struct {
	RunID        string `json:"run_id"`
	TaskID       string `json:"task_id"`
	ArmID        string `json:"arm_id"`
	ArmKind      string `json:"arm_kind"`
	Dose         int    `json:"dose"`
	WorkID       string `json:"work_id"`
	Thrash       int    `json:"thrash"`     // PolicyDecision denial count for this work
	Tokens       int    `json:"tokens"`     // summed over route evidence rows for this work
	LatencyMS    int64  `json:"latency_ms"` // summed over route evidence rows for this work
	RouteRows    int    `json:"route_rows"`
	Verdict      string `json:"verdict"`
	Graded       bool   `json:"graded"`  // verdict != ungraded
	Success      bool   `json:"success"` // verdict == pass
	JudgeLaneID  string `json:"judge_lane_id,omitempty"`
	Blinded      bool   `json:"blinded,omitempty"`
	ReceiptRef   string `json:"receipt_ref,omitempty"`
	ReceiptFound bool   `json:"receipt_found"` // cited receipt exists in the ledger and binds this work
	CeremonyMode string `json:"ceremony_mode,omitempty"`
}

// ArmMetrics is the per-arm aggregate: success rate over graded runs plus full
// distributions of thrash/tokens/latency, and the raw per-run rows.
type ArmMetrics struct {
	ArmID       string      `json:"arm_id"`
	ArmKind     string      `json:"arm_kind"`
	Dose        int         `json:"dose"`
	Runs        int         `json:"runs"`   // dispatched runs (have a work_id)
	Graded      int         `json:"graded"` // runs with a non-ungraded verdict
	Passes      int         `json:"passes"`
	SuccessRate float64     `json:"success_rate"` // passes / graded (0 when graded==0)
	Thrash      Dist        `json:"thrash"`
	Tokens      Dist        `json:"tokens"`
	LatencyMS   Dist        `json:"latency_ms"`
	Rows        []RunMetric `json:"rows"`
}

// ledgerIndex is the single-pass read model over the ledger, keyed by work_id. The
// rig scans events/ and the route-evidence ledger ONCE, then every run's metrics are
// an O(1) lookup — it owns no second metric store beyond this in-memory index.
type ledgerIndex struct {
	denialsByWork map[string]int
	receiptWork   map[string]string // rec_ id -> the work_id it binds (for receipt_ref traceability)
	routeByWork   map[string]routeAgg
}

type routeAgg struct {
	tokens    int
	latencyMS int64
	rows      int
}

// buildLedgerIndex reads events/*.json (PolicyDecision denials, Receipt work-binding)
// and ledger/execution-route-evidence.jsonl (tokens/latency), each exactly once. It
// decodes objects via the envelope registry (envelope.New) and the routeevidence
// exported Evidence type — it reads existing objects, it never writes.
func buildLedgerIndex(homePath string) (ledgerIndex, error) {
	idx := ledgerIndex{
		denialsByWork: map[string]int{},
		receiptWork:   map[string]string{},
		routeByWork:   map[string]routeAgg{},
	}
	if err := idx.scanEvents(homePath); err != nil {
		return ledgerIndex{}, err
	}
	if err := idx.scanRouteEvidence(homePath); err != nil {
		return ledgerIndex{}, err
	}
	return idx, nil
}

func (idx ledgerIndex) scanEvents(homePath string) error {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: []string{"pol_", "rec_"}})
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			return err
		}
		data := record.Bytes
		var probe struct {
			SchemaVersion string `json:"schema_version"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			return fmt.Errorf("event %s: %w", entry.Reference.String(), err)
		}
		env := envelope.New(probe.SchemaVersion)
		if env == nil {
			continue
		}
		if err := json.Unmarshal(data, env); err != nil {
			return fmt.Errorf("event %s: %w", entry.Reference.String(), err)
		}
		switch obj := env.(type) {
		case *envelope.PolicyDecision:
			if obj.Outcome == "deny" && obj.IDs.WorkID != "" {
				idx.denialsByWork[obj.IDs.WorkID]++
			}
		case *envelope.Receipt:
			if obj.IDs.WorkID != "" {
				idx.receiptWork[obj.ID] = obj.IDs.WorkID
			}
		}
	}
	return nil
}

func (idx ledgerIndex) scanRouteEvidence(homePath string) error {
	path := filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// routeevidence.Evidence is the exported read type; plain Unmarshal (no
		// DisallowUnknownFields) keeps the rig forward-compatible with new evidence fields.
		var ev routeevidence.Evidence
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return fmt.Errorf("execution-route-evidence.jsonl line %d: %w", i+1, err)
		}
		if ev.WorkID == "" {
			continue
		}
		agg := idx.routeByWork[ev.WorkID]
		agg.tokens += evidenceTokens(ev)
		agg.latencyMS += ev.LatencyMS
		agg.rows++
		idx.routeByWork[ev.WorkID] = agg
	}
	return nil
}

// evidenceTokens prefers the total_tokens field and falls back to prompt+completion.
func evidenceTokens(ev routeevidence.Evidence) int {
	if ev.Usage.TotalTokens > 0 {
		return ev.Usage.TotalTokens
	}
	return ev.Usage.PromptTokens + ev.Usage.CompletionTokens
}

// deriveRunMetric turns one run record into its derived metric row by O(1) lookups
// into the ledger index. A run with no work_id is not dispatched yet — the caller
// filters those out of the aggregates.
func deriveRunMetric(idx ledgerIndex, r RunRecord) RunMetric {
	m := RunMetric{
		RunID:        r.RunID,
		TaskID:       r.TaskID,
		ArmID:        r.ArmID,
		ArmKind:      r.ArmKind,
		Dose:         r.Dose,
		WorkID:       r.WorkID,
		Verdict:      r.Verdict,
		Graded:       r.Verdict != VerdictUngraded,
		Success:      r.Verdict == VerdictPass,
		JudgeLaneID:  r.JudgeLaneID,
		Blinded:      r.Blinded,
		ReceiptRef:   r.ReceiptRef,
		CeremonyMode: r.CeremonyMode,
	}
	if r.WorkID != "" {
		m.Thrash = idx.denialsByWork[r.WorkID]
		agg := idx.routeByWork[r.WorkID]
		m.Tokens = agg.tokens
		m.LatencyMS = agg.latencyMS
		m.RouteRows = agg.rows
	}
	if r.ReceiptRef != "" {
		// Traceability: the cited receipt must exist AND bind the same work the verdict
		// grades — a graded verdict citing a receipt from another work is not "found".
		if boundWork, ok := idx.receiptWork[r.ReceiptRef]; ok && (r.WorkID == "" || boundWork == r.WorkID) {
			m.ReceiptFound = true
		}
	}
	return m
}

// aggregateArms groups dispatched runs by arm_id into per-arm metrics with full
// distributions. Pending runs (no work_id) are excluded from aggregates; the caller
// reports their count separately.
func aggregateArms(idx ledgerIndex, runs []RunRecord) []ArmMetrics {
	byArm := map[string]*ArmMetrics{}
	var order []string
	for _, r := range runs {
		if r.WorkID == "" {
			continue // pending — not dispatched
		}
		m := deriveRunMetric(idx, r)
		am, ok := byArm[r.ArmID]
		if !ok {
			am = &ArmMetrics{ArmID: r.ArmID, ArmKind: r.ArmKind, Dose: r.Dose}
			byArm[r.ArmID] = am
			order = append(order, r.ArmID)
		}
		am.Runs++
		if m.Graded {
			am.Graded++
			if m.Success {
				am.Passes++
			}
		}
		am.Rows = append(am.Rows, m)
	}
	sort.Strings(order)
	out := make([]ArmMetrics, 0, len(order))
	for _, id := range order {
		am := byArm[id]
		var thr, tok, lat []float64
		for _, row := range am.Rows {
			thr = append(thr, float64(row.Thrash))
			tok = append(tok, float64(row.Tokens))
			lat = append(lat, float64(row.LatencyMS))
		}
		am.Thrash = distOf(thr)
		am.Tokens = distOf(tok)
		am.LatencyMS = distOf(lat)
		if am.Graded > 0 {
			am.SuccessRate = float64(am.Passes) / float64(am.Graded)
		}
		out = append(out, *am)
	}
	return out
}
