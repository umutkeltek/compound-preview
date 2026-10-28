package activity

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Block-inference tuning (decision 11).
const (
	// MinBlockMinutes is the shortest contiguous activity block worth surfacing as unaccounted.
	MinBlockMinutes = 20
	// GapToleranceMinutes coalesces spans separated by a short gap into one block.
	GapToleranceMinutes = 5
)

// WorkActivity is one compound work-ledger timestamp (an envelope/receipt time) used to reconcile
// activity blocks against real agent work. A block overlapping any WorkActivity is "accounted".
type WorkActivity struct {
	Ref string
	At  time.Time
}

// Block is a contiguous run of foreground activity. category_mix summarizes what the operator was
// doing; DurationMin is wall span (end-start), ActiveMin is summed span active time.
type Block struct {
	Start       string `json:"start"`
	End         string `json:"end"`
	ActiveMin   int    `json:"active_min"`
	DurationMin int    `json:"duration_min"`
	CategoryMix string `json:"category_mix"`
}

// DayInference is the deterministic reconciliation for one day: the unaccounted blocks (largest
// first) plus the work refs it reconciled against. It is the payload the lens decodes and the source
// the attention group is built from. Zero unaccounted blocks -> no attention group.
type DayInference struct {
	Date              string   `json:"date"`
	UnaccountedBlocks []Block  `json:"blocks"`
	AccountedRefs     []string `json:"accounted_refs"`
	// LargestUnaccountedMin is the wall duration of the biggest unaccounted block (0 if none).
	LargestUnaccountedMin int `json:"largest_unaccounted_min"`
	// TotalUnaccountedMin sums the wall duration of every unaccounted block.
	TotalUnaccountedMin int `json:"total_unaccounted_min"`
}

// InferDay clusters eligible spans into blocks and marks those with no overlapping work activity as
// unaccounted. Eligibility (decision 11 + F11): coverage must be complete (incomplete windows are
// EXCLUDED — a silent outage must never manufacture an unaccounted block), and category system is
// dropped (system chrome is not "work outside agent sessions"). Deterministic and pure.
func InferDay(date string, spans []ActivitySpan, work []WorkActivity) DayInference {
	inf := DayInference{Date: date, UnaccountedBlocks: []Block{}, AccountedRefs: []string{}}
	for _, w := range work {
		inf.AccountedRefs = append(inf.AccountedRefs, w.Ref)
	}
	sort.Strings(inf.AccountedRefs)

	eligible := make([]ActivitySpan, 0, len(spans))
	for _, s := range spans {
		if s.Coverage != CoverageComplete {
			continue // F11: never infer a block from an incomplete window
		}
		if s.Category == CategorySystem {
			continue
		}
		eligible = append(eligible, s)
	}
	if len(eligible) == 0 {
		return inf
	}
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].Start < eligible[j].Start })

	type cluster struct {
		start, end time.Time
		activeSec  int64
		catSec     map[Category]int64
	}
	var clusters []cluster
	tol := time.Duration(GapToleranceMinutes) * time.Minute
	for _, s := range eligible {
		st, err1 := time.Parse(time.RFC3339, s.Start)
		en, err2 := time.Parse(time.RFC3339, s.End)
		if err1 != nil || err2 != nil || !en.After(st) {
			continue
		}
		if n := len(clusters); n > 0 && st.Sub(clusters[n-1].end) <= tol {
			c := &clusters[n-1]
			if en.After(c.end) {
				c.end = en
			}
			c.activeSec += s.ActiveSec
			c.catSec[s.Category] += s.ActiveSec
			continue
		}
		clusters = append(clusters, cluster{start: st, end: en, activeSec: s.ActiveSec, catSec: map[Category]int64{s.Category: s.ActiveSec}})
	}

	for _, c := range clusters {
		durMin := int(c.end.Sub(c.start).Minutes())
		if durMin < MinBlockMinutes {
			continue
		}
		if overlapsWork(c.start, c.end, work) {
			continue // accounted by a real work timestamp inside the block
		}
		inf.UnaccountedBlocks = append(inf.UnaccountedBlocks, Block{
			Start:       c.start.UTC().Format(time.RFC3339),
			End:         c.end.UTC().Format(time.RFC3339),
			ActiveMin:   int(c.activeSec / 60),
			DurationMin: durMin,
			CategoryMix: categoryMix(c.catSec),
		})
		inf.TotalUnaccountedMin += durMin
	}
	// Largest first; ties broken by earlier start for determinism.
	sort.SliceStable(inf.UnaccountedBlocks, func(i, j int) bool {
		if inf.UnaccountedBlocks[i].DurationMin != inf.UnaccountedBlocks[j].DurationMin {
			return inf.UnaccountedBlocks[i].DurationMin > inf.UnaccountedBlocks[j].DurationMin
		}
		return inf.UnaccountedBlocks[i].Start < inf.UnaccountedBlocks[j].Start
	})
	if len(inf.UnaccountedBlocks) > 0 {
		inf.LargestUnaccountedMin = inf.UnaccountedBlocks[0].DurationMin
	}
	return inf
}

func overlapsWork(start, end time.Time, work []WorkActivity) bool {
	for _, w := range work {
		if !w.At.Before(start) && !w.At.After(end) {
			return true
		}
	}
	return false
}

// categoryMix renders the block's categories by descending active time, e.g. "browsing/design".
func categoryMix(catSec map[Category]int64) string {
	type kv struct {
		cat Category
		sec int64
	}
	var kvs []kv
	for c, s := range catSec {
		kvs = append(kvs, kv{c, s})
	}
	sort.SliceStable(kvs, func(i, j int) bool {
		if kvs[i].sec != kvs[j].sec {
			return kvs[i].sec > kvs[j].sec
		}
		return kvs[i].cat < kvs[j].cat
	})
	parts := make([]string, 0, len(kvs))
	for _, k := range kvs {
		parts = append(parts, string(k.cat))
	}
	return strings.Join(parts, "/")
}

// OperatorSummary renders the human sentence for the day's largest unaccounted block, counting the
// rest. Empty when nothing is unaccounted.
func (d DayInference) OperatorSummary() string {
	if len(d.UnaccountedBlocks) == 0 {
		return ""
	}
	top := d.UnaccountedBlocks[0]
	base := fmt.Sprintf("%d min unaccounted activity (%s) on %s — no compound work accounts for it", top.DurationMin, top.CategoryMix, d.Date)
	if extra := len(d.UnaccountedBlocks) - 1; extra > 0 {
		base += fmt.Sprintf("; +%d more unaccounted block(s), %d min total", extra, d.TotalUnaccountedMin)
	}
	return base
}
