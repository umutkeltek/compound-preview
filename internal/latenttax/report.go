package latenttax

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Falsifier comparison kinds — the pre-registered kill-criteria. Pinned BEFORE any
// data (the registry carries a preregistered_at stamp) so the analysis cannot be
// steered by the numbers it produces.
const (
	CmpFillerGEInstructions = "filler_ge_instructions"   // latent-tax null
	CmpGateOnlyThrashGGBoth = "gate_only_thrash_gg_both" // binary placement
	CmpObserverApproxInBand = "observer_approx_inband"   // terminating trajectory
)

// Falsifier metric selector (for the latent-tax null: compare quality harm or cost).
const (
	MetricQuality = "quality" // harm = 1 - success_rate
	MetricCost    = "cost"    // harm = mean tokens
)

// Falsifier verdicts.
const (
	Killed           = "killed"
	Survived         = "survived"
	InsufficientData = "insufficient_data"
)

// FalsifierCriterion is one pre-registered kill-criterion.
type FalsifierCriterion struct {
	ID         string  `json:"id"`
	Kills      string  `json:"kills"`
	Comparison string  `json:"comparison"`
	Metric     string  `json:"metric,omitempty"` // only for the latent-tax null
	Margin     float64 `json:"margin"`
}

// FalsifierRegistry is the pre-registered set of kill-criteria. Tracked as a fixture
// and stamped with when it was pinned — the report echoes the registry id/stamp so a
// reader can confirm the criteria predate the data.
type FalsifierRegistry struct {
	SchemaVersion   string               `json:"schema_version"`
	RegistryID      string               `json:"registry_id"`
	PreregisteredAt string               `json:"preregistered_at"`
	Criteria        []FalsifierCriterion `json:"criteria"`
}

// DefaultFalsifierRegistry is the canonical three-criterion registry so the rig has
// its pinned falsifiers even without a fixture on disk. The tracked fixture mirrors
// this; the CLI can load either.
func DefaultFalsifierRegistry() FalsifierRegistry {
	return FalsifierRegistry{
		SchemaVersion:   FalsifierSchemaVersion,
		RegistryID:      "latenttax-default-v0.1",
		PreregisteredAt: "2026-07-13T00:00:00Z",
		Criteria: []FalsifierCriterion{
			{
				ID:         "latent_tax_null",
				Kills:      "filler >= instructions on the chosen metric kills the latent-tax framing, leaving only keep-contexts-short",
				Comparison: CmpFillerGEInstructions,
				Metric:     MetricQuality,
				Margin:     0.05,
			},
			{
				ID:         "binary_placement",
				Kills:      "gate-only thrash >> both kills binary obligation placement (guidance in context cuts thrash; the answer is BOTH)",
				Comparison: CmpGateOnlyThrashGGBoth,
				Margin:     1.0,
			},
			{
				ID:         "terminating_trajectory",
				Kills:      "observer ~ in-band on downstream quality kills terminating-trajectory as a practical lever",
				Comparison: CmpObserverApproxInBand,
				Metric:     MetricQuality,
				Margin:     0.05,
			},
		},
	}
}

// LoadFalsifierRegistry reads and validates a falsifier-registry JSON file.
func LoadFalsifierRegistry(path string) (FalsifierRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FalsifierRegistry{}, err
	}
	var reg FalsifierRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return FalsifierRegistry{}, fmt.Errorf("parse falsifier registry %s: %w", path, err)
	}
	if err := reg.Validate(); err != nil {
		return FalsifierRegistry{}, err
	}
	return reg, nil
}

// Validate checks the registry is well-formed and pins recognized comparisons.
func (reg FalsifierRegistry) Validate() error {
	var errs []string
	if reg.SchemaVersion != FalsifierSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version=%q, want %q", reg.SchemaVersion, FalsifierSchemaVersion))
	}
	if strings.TrimSpace(reg.RegistryID) == "" {
		errs = append(errs, "registry_id is required")
	}
	if strings.TrimSpace(reg.PreregisteredAt) == "" {
		errs = append(errs, "preregistered_at is required (criteria must be pinned before data)")
	}
	if len(reg.Criteria) == 0 {
		errs = append(errs, "registry has no criteria")
	}
	seen := map[string]bool{}
	for _, c := range reg.Criteria {
		if strings.TrimSpace(c.ID) == "" {
			errs = append(errs, "criterion has no id")
			continue
		}
		if seen[c.ID] {
			errs = append(errs, "duplicate criterion id "+c.ID)
		}
		seen[c.ID] = true
		switch c.Comparison {
		case CmpFillerGEInstructions, CmpObserverApproxInBand:
			if c.Metric != MetricQuality && c.Metric != MetricCost {
				errs = append(errs, fmt.Sprintf("criterion %s: metric=%q, want %q or %q", c.ID, c.Metric, MetricQuality, MetricCost))
			}
		case CmpGateOnlyThrashGGBoth:
			// thrash comparison uses no metric selector
		default:
			errs = append(errs, fmt.Sprintf("criterion %s: unknown comparison %q", c.ID, c.Comparison))
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("falsifier registry invalid: %s", strings.Join(errs, "; "))
	}
	return nil
}

// FalsifierResult is one evaluated kill-criterion. Left/Right are the compared
// quantities; CitedReceipts backs the comparison with the run receipts the verdict
// stands on (traceability into the ledger).
type FalsifierResult struct {
	ID            string   `json:"id"`
	Kills         string   `json:"kills"`
	Comparison    string   `json:"comparison"`
	Metric        string   `json:"metric,omitempty"`
	Verdict       string   `json:"verdict"` // killed | survived | insufficient_data
	Left          float64  `json:"left"`
	Right         float64  `json:"right"`
	Margin        float64  `json:"margin"`
	Detail        string   `json:"detail"`
	CitedReceipts []string `json:"cited_receipts"`
}

// AdjudicationRequest is the deliverable's ask — a REQUEST, never a promotion. The
// rig has no promote code path; it proposes what the data SUGGESTS for the
// obligation-placement law candidate and cites the run receipts, then stops. Only
// the operator adjudicates.
type AdjudicationRequest struct {
	Note          string            `json:"note"`
	LawCandidate  string            `json:"law_candidate"`
	Proposed      []ProposedVerdict `json:"proposed_verdicts"`
	CitedReceipts []string          `json:"cited_receipts"`
}

// ProposedVerdict is a per-criterion SUGGESTION (promote/amend/kill/gather-more). It
// is advisory — the field name says "suggests", never "decides".
type ProposedVerdict struct {
	Criterion string `json:"criterion"`
	Suggests  string `json:"suggests"`
	Basis     string `json:"basis"`
}

// Report is the rig's evidence artifact. It carries per-arm full distributions, the
// evaluated falsifiers, and the adjudication request. Identity fields key it to a
// task set / arm config / registry / build so two reports are comparable only when
// they mean the same thing. It never carries the home path (a personal path).
type Report struct {
	SchemaVersion   string              `json:"schema_version"`
	TaskSetID       string              `json:"task_set_id"`
	ArmConfigID     string              `json:"arm_config_id"`
	RegistryID      string              `json:"registry_id"`
	PreregisteredAt string              `json:"preregistered_at"`
	Timestamp       string              `json:"timestamp"`
	BuildSHA        string              `json:"build_sha"`
	RunCount        int                 `json:"run_count"`
	Dispatched      int                 `json:"dispatched"`
	Graded          int                 `json:"graded"`
	Pending         int                 `json:"pending"`
	Arms            []ArmMetrics        `json:"arms"`
	Falsifiers      []FalsifierResult   `json:"falsifiers"`
	Adjudication    AdjudicationRequest `json:"adjudication_request"`
}

// BuildReport derives all metrics by READING the ledger under homePath for the run
// records' work ids, aggregates per-arm distributions, evaluates the pre-registered
// falsifiers, and assembles the adjudication REQUEST. It mutates no authority and
// promotes nothing. timestamp/buildSHA are injected for deterministic reports.
func BuildReport(ts TaskSet, ac ArmConfig, reg FalsifierRegistry, runs []RunRecord, homePath, buildSHA, timestamp string) (Report, error) {
	if err := reg.Validate(); err != nil {
		return Report{}, err
	}
	idx, err := buildLedgerIndex(homePath)
	if err != nil {
		return Report{}, err
	}
	arms := aggregateArms(idx, runs)

	rep := Report{
		SchemaVersion:   ReportSchemaVersion,
		TaskSetID:       ts.TaskSetID,
		ArmConfigID:     ac.ConfigID,
		RegistryID:      reg.RegistryID,
		PreregisteredAt: reg.PreregisteredAt,
		Timestamp:       timestamp,
		BuildSHA:        buildSHA,
		RunCount:        len(runs),
		Arms:            arms,
	}
	for _, r := range runs {
		if r.WorkID == "" {
			rep.Pending++
			continue
		}
		rep.Dispatched++
		if r.Verdict != VerdictUngraded {
			rep.Graded++
		}
	}

	rep.Falsifiers = evaluateFalsifiers(reg, arms)
	rep.Adjudication = buildAdjudication(rep.Falsifiers)
	return rep, nil
}

// evaluateFalsifiers renders every pinned criterion — even when data is insufficient,
// it emits an insufficient_data result rather than dropping the criterion, so the
// report always carries the full pre-registered set.
func evaluateFalsifiers(reg FalsifierRegistry, arms []ArmMetrics) []FalsifierResult {
	out := make([]FalsifierResult, 0, len(reg.Criteria))
	for _, c := range reg.Criteria {
		res := FalsifierResult{ID: c.ID, Kills: c.Kills, Comparison: c.Comparison, Metric: c.Metric, Margin: c.Margin, CitedReceipts: []string{}}
		switch c.Comparison {
		case CmpFillerGEInstructions:
			evalLatentTaxNull(&res, c, arms)
		case CmpGateOnlyThrashGGBoth:
			evalBinaryPlacement(&res, c, arms)
		case CmpObserverApproxInBand:
			evalTerminatingTrajectory(&res, c, arms)
		default:
			res.Verdict = InsufficientData
			res.Detail = "unknown comparison " + c.Comparison
		}
		out = append(out, res)
	}
	return out
}

// evalLatentTaxNull compares filler vs obligation_in_context harm at dose > 0. Killed
// when filler harms at least as much as the instructions (within margin) — the tax is
// then indistinguishable from generic length cost.
func evalLatentTaxNull(res *FalsifierResult, c FalsifierCriterion, arms []ArmMetrics) {
	fillerRows := poolRows(arms, ArmFiller, true)
	instrRows := poolRows(arms, ArmObligationInContext, true)
	if len(fillerRows) == 0 || len(instrRows) == 0 {
		res.Verdict = InsufficientData
		res.Detail = "need dose>0 filler and obligation_in_context runs"
		return
	}
	fillerHarm, fillerOK := harm(fillerRows, c.Metric)
	instrHarm, instrOK := harm(instrRows, c.Metric)
	if !fillerOK || !instrOK {
		res.Verdict = InsufficientData
		res.Detail = "no graded runs to compute " + c.Metric + " harm"
		return
	}
	res.Left = fillerHarm
	res.Right = instrHarm
	res.CitedReceipts = citedReceipts(fillerRows, instrRows)
	if fillerHarm >= instrHarm-c.Margin {
		res.Verdict = Killed
		res.Detail = fmt.Sprintf("filler %s harm %.4f >= instruction harm %.4f - margin %.4f: length-matched filler harms as much as instructions", c.Metric, fillerHarm, instrHarm, c.Margin)
		return
	}
	res.Verdict = Survived
	res.Detail = fmt.Sprintf("instruction %s harm %.4f exceeds filler harm %.4f by more than margin %.4f: content-specific cost beyond length", c.Metric, instrHarm, fillerHarm, c.Margin)
}

// evalBinaryPlacement compares gate-only vs both mean thrash. Killed when gate-only
// thrash exceeds both by at least the margin (>>): in-context guidance cut thrash, so
// removing it is not free and the binary "gate-only" placement is wrong.
func evalBinaryPlacement(res *FalsifierResult, c FalsifierCriterion, arms []ArmMetrics) {
	gateRows := poolRows(arms, ArmGateOnly, false)
	bothRows := poolRows(arms, ArmBoth, false)
	if len(gateRows) == 0 || len(bothRows) == 0 {
		res.Verdict = InsufficientData
		res.Detail = "need gate_only and both runs"
		return
	}
	gateThrash := meanOf(thrashValues(gateRows))
	bothThrash := meanOf(thrashValues(bothRows))
	res.Left = gateThrash
	res.Right = bothThrash
	res.CitedReceipts = citedReceipts(gateRows, bothRows)
	if gateThrash-bothThrash >= c.Margin {
		res.Verdict = Killed
		res.Detail = fmt.Sprintf("gate-only mean thrash %.4f exceeds both %.4f by >= margin %.4f: guidance in context cut thrash; placement is BOTH, not gate-only", gateThrash, bothThrash, c.Margin)
		return
	}
	res.Verdict = Survived
	res.Detail = fmt.Sprintf("gate-only mean thrash %.4f is not >> both %.4f (margin %.4f): removing in-context guidance did not spike thrash", gateThrash, bothThrash, c.Margin)
}

// evalTerminatingTrajectory compares observer vs in-band ceremony downstream quality.
// Killed when they are approximately equal (within margin): the observer buys no
// downstream-quality improvement, so terminating trajectories are not a practical
// lever. Requires ceremony-tagged runs; absent them it renders insufficient_data.
func evalTerminatingTrajectory(res *FalsifierResult, c FalsifierCriterion, arms []ArmMetrics) {
	observerRows := poolCeremony(arms, CeremonyObserver)
	inBandRows := poolCeremony(arms, CeremonyInBand)
	if len(observerRows) == 0 || len(inBandRows) == 0 {
		res.Verdict = InsufficientData
		res.Detail = "need observer and in_band ceremony-tagged runs (Claim 3 axis)"
		return
	}
	obsHarm, obsOK := harm(observerRows, c.Metric)
	inbHarm, inbOK := harm(inBandRows, c.Metric)
	if !obsOK || !inbOK {
		res.Verdict = InsufficientData
		res.Detail = "no graded ceremony runs to compute " + c.Metric + " harm"
		return
	}
	// Downstream quality = 1 - harm; compare the qualities directly.
	obsQuality := 1 - obsHarm
	inbQuality := 1 - inbHarm
	res.Left = obsQuality
	res.Right = inbQuality
	res.CitedReceipts = citedReceipts(observerRows, inBandRows)
	if absF(obsQuality-inbQuality) <= c.Margin {
		res.Verdict = Killed
		res.Detail = fmt.Sprintf("observer downstream quality %.4f ~ in-band %.4f (|diff| <= margin %.4f): the observer buys no measurable downstream gain", obsQuality, inbQuality, c.Margin)
		return
	}
	res.Verdict = Survived
	res.Detail = fmt.Sprintf("observer downstream quality %.4f differs from in-band %.4f by more than margin %.4f: terminating-trajectory routing is a real lever", obsQuality, inbQuality, c.Margin)
}

// buildAdjudication maps falsifier verdicts to advisory suggestions and cites every
// receipt the falsifiers stood on. It proposes; it never promotes.
func buildAdjudication(fals []FalsifierResult) AdjudicationRequest {
	req := AdjudicationRequest{
		Note:         "REQUEST only — the rig promotes nothing and has no promotion code path; the operator adjudicates the obligation-placement law candidate from these run receipts.",
		LawCandidate: "obligation-placement law candidate",
	}
	citeSet := map[string]bool{}
	for _, f := range fals {
		req.Proposed = append(req.Proposed, ProposedVerdict{
			Criterion: f.ID,
			Suggests:  suggestionFor(f),
			Basis:     f.Detail,
		})
		for _, r := range f.CitedReceipts {
			citeSet[r] = true
		}
	}
	for r := range citeSet {
		req.CitedReceipts = append(req.CitedReceipts, r)
	}
	sort.Strings(req.CitedReceipts)
	return req
}

func suggestionFor(f FalsifierResult) string {
	if f.Verdict == InsufficientData {
		return "gather more runs — insufficient data to adjudicate"
	}
	switch f.ID {
	case "latent_tax_null":
		if f.Verdict == Killed {
			return "kill the latent-tax framing; keep-contexts-short survives"
		}
		return "promote latent-tax as content-specific cost beyond length"
	case "binary_placement":
		if f.Verdict == Killed {
			return "amend obligation placement to BOTH (in-context guidance + gate)"
		}
		return "gate-only placement holds under test"
	case "terminating_trajectory":
		if f.Verdict == Killed {
			return "kill terminating-trajectory as a practical lever"
		}
		return "promote the terminating-observer as a downstream-quality lever"
	default:
		return "operator to adjudicate"
	}
}

// --- pooling / harm helpers ---

func poolRows(arms []ArmMetrics, kind string, doseAboveZero bool) []RunMetric {
	var out []RunMetric
	for _, am := range arms {
		if am.ArmKind != kind {
			continue
		}
		if doseAboveZero && am.Dose == 0 {
			continue
		}
		out = append(out, am.Rows...)
	}
	return out
}

func poolCeremony(arms []ArmMetrics, mode string) []RunMetric {
	var out []RunMetric
	for _, am := range arms {
		for _, r := range am.Rows {
			if r.CeremonyMode == mode {
				out = append(out, r)
			}
		}
	}
	return out
}

// harm computes the harm metric over graded rows: quality harm = 1 - pass_rate; cost
// harm = mean tokens. Returns ok=false when there are no rows to compute it.
func harm(rows []RunMetric, metric string) (float64, bool) {
	if metric == MetricCost {
		if len(rows) == 0 {
			return 0, false
		}
		return meanOf(tokenValues(rows)), true
	}
	graded := 0
	passes := 0
	for _, r := range rows {
		if r.Graded {
			graded++
			if r.Success {
				passes++
			}
		}
	}
	if graded == 0 {
		return 0, false
	}
	return 1 - float64(passes)/float64(graded), true
}

func thrashValues(rows []RunMetric) []float64 {
	out := make([]float64, 0, len(rows))
	for _, r := range rows {
		out = append(out, float64(r.Thrash))
	}
	return out
}

func tokenValues(rows []RunMetric) []float64 {
	out := make([]float64, 0, len(rows))
	for _, r := range rows {
		out = append(out, float64(r.Tokens))
	}
	return out
}

func citedReceipts(groups ...[]RunMetric) []string {
	set := map[string]bool{}
	for _, g := range groups {
		for _, r := range g {
			if r.ReceiptRef != "" {
				set[r.ReceiptRef] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func meanOf(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// MarshalReport renders the report as indented JSON for the CLI to write under
// COMPOUND_HOME/reviews/latenttax (or an operator-pointed file).
func MarshalReport(rep Report) ([]byte, error) {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
