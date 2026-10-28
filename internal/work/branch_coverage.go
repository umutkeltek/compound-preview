package work

import (
	"encoding/json"
	"runtime"
	"sort"
	"sync"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

// BranchCoverageReport is the deterministic read model over events/tr_*.json
// that answers transition.md's deferred question: which declared future
// branches are still OPEN, which were COVERED by later transitions, and which
// opens have aged past the staleness window without coverage (branch debt).
//
// Semantics (pinned in docs/architecture/transition.md):
//   - kinds opens|blocks|supersedes|validates|retires|routes_to;
//     ONLY validates|supersedes|retires cover an opened branch.
//   - blocks/routes_to are neutral records (counted, no lifecycle effect).
//   - a repeated `opens` on a live slug is an EXPANSION: it keeps the original
//     opened_at (age stays honest) and never duplicates the branch.
//   - covering a slug that was never opened is a DANGLING cover (integrity).
//   - nothing here closes a branch: debt is advisory visibility pressure, and
//     humans stay on irreversible decisions.
//
// Evaluation-rule teeth: a transition claiming future_behavior_changed with no
// opens_branches and no cause.next_consumer names no checkable future branch —
// it is counted as an UNVERIFIABLE claim (telemetry dressed as compounding).
type BranchCoverageReport struct {
	GeneratedAt  string                `json:"generated_at"`
	WindowDays   int                   `json:"window_days"`
	Counts       BranchCoverageCounts  `json:"counts"`
	Debt         []BranchDebtEntry     `json:"debt,omitempty"`
	Dangling     []BranchDanglingCover `json:"dangling_covers,omitempty"`
	Unverifiable []string              `json:"unverifiable_transitions,omitempty"`
	Integrity    []string              `json:"integrity,omitempty"`
}

// BranchCoverageCounts aggregates the deterministic fold. Naming:
//   - Opened: total `opens` statements ever recorded (cumulative).
//   - Covered: total `validates|supersedes|retires` statements that resolved
//     an existing open branch (cumulative).
//   - Open: currently-uncovered branches (snapshot — Open = Opened - Covered).
//     This is the "debt" numerator; the workbench surfaces it.
//   - Stale: subset of Open whose opened_at is older than the staleness window
//     (snapshot). Coverage is responsibility, not enforcement: debt is
//     advisory visibility, never auto-closed.
//   - Expansions: re-`opens` on a live branch (the original opened_at is kept
//     so age stays honest).
//   - Neutral: `blocks | routes_to` records + duplicate covers of covered
//     branches. Recorded, no lifecycle effect.
//   - DanglingCovers: covers of a slug that was never opened (integrity).
//   - UnverifiableChanged: transitions claiming future_behavior_changed with
//     no opens_branches and no next_consumer — telemetry dressed as
//     compounding, the evaluation rule's first count.
type BranchCoverageCounts struct {
	Opened              int `json:"opened"`
	Covered             int `json:"covered"`
	Open                int `json:"open"`
	Stale               int `json:"stale"`
	Expansions          int `json:"expansions"`
	Neutral             int `json:"neutral"`
	DanglingCovers      int `json:"dangling_covers"`
	UnverifiableChanged int `json:"unverifiable_changed"`
}

type BranchDebtEntry struct {
	Branch     string `json:"branch"`
	Owner      string `json:"owner"`
	OpenedBy   string `json:"opened_by"`
	WorkID     string `json:"work_id,omitempty"`
	OpenedAt   string `json:"opened_at"`
	AgeDays    int    `json:"age_days"`
	Stale      bool   `json:"stale"`
	Expansions int    `json:"expansions"`
}

type BranchDanglingCover struct {
	Branch       string `json:"branch"`
	Kind         string `json:"kind"`
	ByTransition string `json:"by_transition"`
	At           string `json:"at"`
}

// BranchDebtStaleWindowDays is the default staleness window: an open branch
// older than this surfaces as synthesis debt. The value is operator-tunable
// per call via the BranchCoverage(now, windowDays) parameter (windowDays <= 0
// falls back to this constant). The "operator-approved" framing reflects the
// workbench's constant choice — the live operator can still override per
// call without changing code.
const BranchDebtStaleWindowDays = 7

// branchDebtListCap bounds the rendered debt/dangling/unverifiable lists in
// the JSON surface. Counts always reflect the FULL fold, so capping never
// hides scale (callers see the totals in Counts).
const branchDebtListCap = 10

// branchDebtTextRenderCap is the bounded list size for the human-facing
// `compound workbench` text render. Smaller than the JSON cap because a
// terminal reader only needs the first few concrete entries to act; the
// counts line gives the precision. The text render is intentionally a
// NUDGE, not a replacement for `compound workbench --json`.
const branchDebtTextRenderCap = 3

// BranchDebtTextRenderCap exposes the text-render cap so non-Go callers
// (CLI surface) can render consistently with the read model's own render.
func BranchDebtTextRenderCap() int { return branchDebtTextRenderCap }

// branchCoveringKinds resolves an opened branch (transition.md lifecycle).
var branchCoveringKinds = map[string]bool{"validates": true, "supersedes": true, "retires": true}

type branchState struct {
	owner      string
	openedBy   string
	workID     string
	openedAt   time.Time
	expansions int
	covered    bool
	coverKind  string
	coverBy    string
	coverAt    time.Time
}

// BranchCoverage folds every transition event into the branch-lifecycle read
// model. The fold order is (created_at, id), so the report is deterministic
// for a fixed event store and clock.
func (s *Store) BranchCoverage(now time.Time, windowDays int) (BranchCoverageReport, error) {
	if windowDays <= 0 {
		windowDays = BranchDebtStaleWindowDays
	}
	report := BranchCoverageReport{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		WindowDays:  windowDays,
	}

	entries, err := s.events.Scan(eventstore.ScanOptions{Prefixes: []string{"tr_"}})
	if err != nil {
		return report, err
	}
	type timedTransition struct {
		id string
		at time.Time
		tr envelope.Transition
	}
	// Decode phase is embarrassingly parallel (a 20k-transition home folds in
	// seconds, not tens of seconds — the workbench hot path earns its keep).
	// Each worker writes ONLY its own index slot, so collection order stays
	// the deterministic scan order; the fold below then sorts by
	// (created_at, id), making the report independent of scheduling.
	type decodeResult struct {
		ok   bool
		item timedTransition
		note string
	}
	results := make([]decodeResult, len(entries))
	workers := runtime.NumCPU()
	if workers > len(entries) {
		workers = len(entries)
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	slots := make(chan int, len(entries))
	for i := range entries {
		slots <- i
	}
	close(slots)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range slots {
				entry := entries[i]
				record, err := entry.Read()
				if err != nil {
					results[i].note = entry.ID + ": unreadable: " + err.Error()
					continue
				}
				var tr envelope.Transition
				if err := json.Unmarshal(record.Bytes, &tr); err != nil {
					results[i].note = entry.ID + ": not a transition: " + err.Error()
					continue
				}
				at, err := time.Parse(time.RFC3339, tr.CreatedAt)
				if err != nil {
					results[i].note = entry.ID + ": bad created_at: " + err.Error()
					continue
				}
				results[i].ok = true
				results[i].item = timedTransition{id: entry.ID, at: at, tr: tr}
			}
		}()
	}
	wg.Wait()
	ordered := make([]timedTransition, 0, len(entries))
	for _, res := range results {
		if res.note != "" {
			report.Integrity = append(report.Integrity, res.note)
			continue
		}
		if res.ok {
			ordered = append(ordered, res.item)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].at.Equal(ordered[j].at) {
			return ordered[i].at.Before(ordered[j].at)
		}
		return ordered[i].id < ordered[j].id
	})

	branches := map[string]*branchState{}
	for _, item := range ordered {
		tr := item.tr
		if tr.SecondOrder.FutureBehaviorChanged && len(tr.OpensBranches) == 0 &&
			(tr.Cause == nil || tr.Cause.NextConsumer == "") {
			report.Counts.UnverifiableChanged++
			// The fold is chronological. Retain its bounded tail, not a
			// capped prefix that permanently loses newer observations.
			if len(report.Unverifiable) == branchDebtListCap {
				copy(report.Unverifiable, report.Unverifiable[1:])
				report.Unverifiable = report.Unverifiable[:branchDebtListCap-1]
			}
			report.Unverifiable = append(report.Unverifiable, item.id)
		}
		for _, ref := range tr.OpensBranches {
			state, exists := branches[ref.Branch]
			switch {
			case ref.Kind == "opens":
				if !exists {
					branches[ref.Branch] = &branchState{
						owner: ref.Owner, openedBy: item.id, workID: tr.IDs.WorkID, openedAt: item.at,
					}
					report.Counts.Opened++
				} else {
					state.expansions++
					report.Counts.Expansions++
				}
			case branchCoveringKinds[ref.Kind]:
				if !exists {
					report.Counts.DanglingCovers++
					if len(report.Dangling) < branchDebtListCap {
						report.Dangling = append(report.Dangling, BranchDanglingCover{
							Branch: ref.Branch, Kind: ref.Kind, ByTransition: item.id,
							At: item.at.UTC().Format(time.RFC3339),
						})
					}
				} else if !state.covered {
					state.covered = true
					state.coverKind, state.coverBy, state.coverAt = ref.Kind, item.id, item.at
					report.Counts.Covered++
				} else {
					report.Counts.Neutral++ // duplicate cover of an already-resolved branch
				}
			default: // blocks | routes_to — recorded, no lifecycle effect
				report.Counts.Neutral++
			}
		}
	}

	window := time.Duration(windowDays) * 24 * time.Hour
	debt := make([]BranchDebtEntry, 0, len(branches))
	for slug, state := range branches {
		if state.covered {
			continue
		}
		report.Counts.Open++
		age := now.Sub(state.openedAt)
		if age < 0 {
			age = 0
		}
		stale := age > window
		if stale {
			report.Counts.Stale++
		}
		debt = append(debt, BranchDebtEntry{
			Branch:     slug,
			Owner:      state.owner,
			OpenedBy:   state.openedBy,
			WorkID:     state.workID,
			OpenedAt:   state.openedAt.UTC().Format(time.RFC3339),
			AgeDays:    int(age.Hours() / 24),
			Stale:      stale,
			Expansions: state.expansions,
		})
	}
	sort.Slice(debt, func(i, j int) bool {
		if debt[i].Stale != debt[j].Stale {
			return debt[i].Stale
		}
		if debt[i].OpenedAt != debt[j].OpenedAt {
			return debt[i].OpenedAt < debt[j].OpenedAt
		}
		return debt[i].Branch < debt[j].Branch
	})
	if len(debt) > branchDebtListCap {
		debt = debt[:branchDebtListCap]
	}
	report.Debt = debt

	// Newest first even when the full history is smaller than the cap.
	for i, j := 0, len(report.Unverifiable)-1; i < j; i, j = i+1, j-1 {
		report.Unverifiable[i], report.Unverifiable[j] = report.Unverifiable[j], report.Unverifiable[i]
	}
	return report, nil
}
