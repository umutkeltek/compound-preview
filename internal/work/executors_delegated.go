package work

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/runattempt"
)

// DelegatedSessionsSourceRef labels the eventstore door the delegated-session
// slice consults: the compound.work_delegate_event.v0.1 records the delegate
// verbs already emit. The console reads this projection field; it NEVER reads
// eventstore files itself.
const DelegatedSessionsSourceRef = "compound.events.work_delegate"

// DelegatedSessionRow is the SAFE, reduced view of one work+lane's latest
// supervised delegated session, folded deterministically from the
// work.delegate.{spawned,steered,refused,exited} lifecycle records plus the
// exact RunAttempt those records name.
//
// Privacy law (packet 2026-07-20-governed-execution-delegate-steer-attach):
// this row NEVER carries prompt text, pane/stdout content, credentials, argv,
// or provider environment. It exposes only the binding shape (lane, session
// name, holder), the reduced lifecycle status, the last steer TIME (not its
// text), and the exact RunAttempt id/status. The delegate event Message field
// (which can echo guard prose or a steer length) is deliberately dropped; only
// the enum reason survives for a refusal.
type DelegatedSessionRow struct {
	WorkID string `json:"work_id"`
	Lane   string `json:"lane,omitempty"`
	// Session is the tmux session name ("compound-<work_id>"), NOT pane content.
	Session string `json:"session,omitempty"`
	// Status is the reduced lifecycle: running|exited|refused|unknown. Terminal
	// evidence (a recorded exit, or a terminal RunAttempt) always wins over a
	// live-looking binding.
	Status string `json:"status"`
	Holder string `json:"holder,omitempty"`
	// RunAttemptID is the exact ratt_<UTC>_<hex> the delegate spawn minted.
	RunAttemptID string `json:"run_attempt_id,omitempty"`
	// RunAttemptStatus is that attempt's authoritative status (from the
	// runattempt store), or empty for a legacy binding with no ratt.
	RunAttemptStatus string                  `json:"run_attempt_status,omitempty"`
	Progress         *runattempt.RunProgress `json:"progress,omitempty"`
	// RefusedReason is the enum reason (walled|gate_absent|session_live|...)
	// for a refused row. NEVER the free-text guard message.
	RefusedReason string `json:"refused_reason,omitempty"`
	SpawnedAt     string `json:"spawned_at,omitempty"`
	// LastSteerAt is the TIME of the most recent steer for the live generation.
	// The steered text is never read.
	LastSteerAt string `json:"last_steer_at,omitempty"`
	ExitedAt    string `json:"exited_at,omitempty"`
	// ObservedExit is the safe exit label (succeeded|failed|vanished) mapped
	// with the same supervision semantics DelegateSweep uses; no exit content.
	ObservedExit string `json:"observed_exit,omitempty"`
}

// DelegatedSessionCounts is the fold summary the console header reads.
type DelegatedSessionCounts struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Exited  int `json:"exited"`
	Refused int `json:"refused"`
	Unknown int `json:"unknown"`
}

// reduceDelegatedSessions folds every work's delegate lifecycle records into
// at most one safe row per work+lane. It reuses the event snapshot Workbench
// already loaded and then reads the small number of attached wde_ records; it
// never walks raw eventstore files on the console's behalf.
func (s *Store) reduceDelegatedSessions(byWork map[string][]Event) ([]DelegatedSessionRow, DelegatedSessionCounts, error) {
	refs := make([]string, 0)
	seen := make(map[string]bool)
	for _, events := range byWork {
		for _, event := range events {
			for _, ref := range event.Attached {
				if strings.HasPrefix(ref, "wde_") && !seen[ref] {
					seen[ref] = true
					refs = append(refs, ref)
				}
			}
		}
	}
	records := s.events.ReadMany(refs)
	readEvent := func(ref string, value any) error {
		result := records[ref]
		return decodeEventJSON(ref, result.Record, result.Err, value)
	}
	raStore := runattempt.Open(s.home)
	rows := make([]DelegatedSessionRow, 0)
	for workID, events := range byWork {
		workRows, rerr := s.reduceDelegatedSessionsForWorkRead(workID, events, raStore, readEvent)
		if rerr != nil {
			return nil, DelegatedSessionCounts{}, rerr
		}
		rows = append(rows, workRows...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		li, lj := delegatedStatusRank(rows[i].Status), delegatedStatusRank(rows[j].Status)
		if li != lj {
			return li < lj
		}
		if rows[i].WorkID != rows[j].WorkID {
			return rows[i].WorkID < rows[j].WorkID
		}
		if rows[i].Lane != rows[j].Lane {
			return rows[i].Lane < rows[j].Lane
		}
		return rows[i].Session < rows[j].Session
	})
	counts := DelegatedSessionCounts{Total: len(rows)}
	for _, r := range rows {
		switch r.Status {
		case "running":
			counts.Running++
		case "exited":
			counts.Exited++
		case "refused":
			counts.Refused++
		default:
			counts.Unknown++
		}
	}
	return rows, counts, nil
}

type delegatedLaneState struct {
	binding      *delegateEvent
	lastRefused  *delegateEvent
	lastSteerAt  string
	exitedAt     string
	observedExit string
	exited       bool
}

// reduceDelegatedSessionsForWork walks one work's lifecycle events
// oldest-first and keeps the latest generation for each lane. A new spawn on a
// lane replaces that lane's older generation; activity on another lane remains
// visible instead of being collapsed away.
func (s *Store) reduceDelegatedSessionsForWork(workID string, events []Event, raStore *runattempt.Store) ([]DelegatedSessionRow, error) {
	return s.reduceDelegatedSessionsForWorkRead(workID, events, raStore, s.readEventJSON)
}

// The reader changes acquisition only. Every original occurrence still passes
// validation and the same ordered lifecycle reduction.
func (s *Store) reduceDelegatedSessionsForWorkRead(workID string, events []Event, raStore *runattempt.Store, readEvent func(string, any) error) ([]DelegatedSessionRow, error) {
	states := make(map[string]*delegatedLaneState)

	for _, e := range events {
		for _, att := range e.Attached {
			if !strings.HasPrefix(att, "wde_") {
				continue
			}
			var dev delegateEvent
			if rerr := readEvent(att, &dev); rerr != nil {
				return nil, rerr
			}
			if verr := validateAttachedDelegateEvent(att, workID, dev); verr != nil {
				return nil, verr
			}
			lane := strings.TrimSpace(dev.Lane)
			if lane == "" {
				lane = strings.TrimSpace(dev.Harness)
			}
			state := states[lane]
			if state == nil {
				state = &delegatedLaneState{}
				states[lane] = state
			}
			switch dev.Kind {
			case "work.delegate.spawned":
				b := dev
				state.binding = &b
				state.lastRefused = nil
				state.exited = false
				state.exitedAt = ""
				state.observedExit = ""
				state.lastSteerAt = ""
			case "work.delegate.steered":
				if state.binding != nil && dev.Session == state.binding.Session && delegatedTSAfter(dev.TS, state.lastSteerAt) {
					state.lastSteerAt = dev.TS
				}
			case "work.delegate.exited":
				if state.binding != nil && delegatedSameGeneration(dev, state.binding) {
					state.exited = true
					state.exitedAt = dev.TS
					state.observedExit = delegatedObservedExitLabel(dev)
				}
			case "work.delegate.refused":
				r := dev
				state.lastRefused = &r
			}
		}
	}
	rows := make([]DelegatedSessionRow, 0, len(states))
	for _, state := range states {
		row, ok, err := s.reduceDelegatedLaneRow(workID, state, raStore)
		if err != nil {
			return nil, err
		}
		if ok {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (s *Store) reduceDelegatedLaneRow(workID string, state *delegatedLaneState, raStore *runattempt.Store) (DelegatedSessionRow, bool, error) {
	binding := state.binding
	hasRatt := binding != nil && strings.TrimSpace(binding.RunAttemptID) != ""
	raStatus := ""
	raCompletedAt := ""
	raMissing := false
	var progress *runattempt.RunProgress
	if hasRatt {
		if ra, gerr := raStore.Get(binding.RunAttemptID); gerr == nil {
			raStatus = ra.Status
			if ra.Progress != nil {
				copy := *ra.Progress
				progress = &copy
			}
			if !ra.CompletedAt.IsZero() {
				raCompletedAt = ra.CompletedAt.UTC().Format(time.RFC3339Nano)
			}
		} else if errors.Is(gerr, runattempt.ErrNotFound) {
			// The binding NAMES an attempt but its record is gone: we cannot
			// vouch for liveness. This is NOT the legacy empty-ratt case; do
			// not collapse the two and guess running.
			raMissing = true
		} else {
			return DelegatedSessionRow{}, false, gerr
		}
	}

	bindingTerminal := binding != nil && (state.exited || runattempt.IsTerminal(raStatus))
	terminalAt := state.exitedAt
	if terminalAt == "" {
		terminalAt = raCompletedAt
	}
	refusalSupersedes := bindingTerminal && state.lastRefused != nil && delegatedTSAfter(state.lastRefused.TS, terminalAt)
	row := DelegatedSessionRow{WorkID: workID}
	switch {
	case refusalSupersedes:
		// Once a prior session is terminal, a newer refusal is the lane's current
		// truth. A refusal while the binding is still live stays binding-neutral.
		row.Status = "refused"
	case bindingTerminal:
		row.Status = "exited"
	case binding != nil && !raMissing && (raStatus == runattempt.StatusRunning || !hasRatt):
		// Running only when either the named attempt is actually running, or
		// this is a legacy binding with NO ratt that the eventlog still
		// vouches for. A named-but-missing attempt never reads running.
		row.Status = "running"
	case binding == nil && state.lastRefused != nil:
		row.Status = "refused"
	default:
		// A spawned binding whose ratt is pending (minted, not yet running),
		// missing, or otherwise indeterminate: honestly unknown, never a
		// guessed running/exited.
		row.Status = "unknown"
	}

	if binding != nil && row.Status != "refused" {
		row.Lane = binding.Lane
		row.Session = binding.Session
		row.Holder = binding.Holder
		row.RunAttemptID = binding.RunAttemptID
		row.RunAttemptStatus = raStatus
		row.Progress = progress
		row.SpawnedAt = delegatedRFC3339(binding.TS)
		if state.lastSteerAt != "" {
			row.LastSteerAt = delegatedRFC3339(state.lastSteerAt)
		}
		if state.exited {
			row.ExitedAt = delegatedRFC3339(state.exitedAt)
			row.ObservedExit = state.observedExit
		}
	}
	if row.Status == "refused" && state.lastRefused != nil {
		row.Lane = state.lastRefused.Lane
		row.Holder = state.lastRefused.Holder
		row.RefusedReason = state.lastRefused.Reason
	}
	return row, true, nil
}

// delegatedSameGeneration reports whether an exit record belongs to the live
// binding's generation. It matches the exact RunAttemptID (so a stale exit from
// a reused session name cannot finalize a fresh generation); a legacy binding
// with no ratt falls back to session identity, mirroring recordedDelegateExit.
func delegatedSameGeneration(exit delegateEvent, binding *delegateEvent) bool {
	if exit.Session != binding.Session {
		return false
	}
	return binding.RunAttemptID == "" || exit.RunAttemptID == binding.RunAttemptID
}

// delegatedObservedExitLabel maps an exit record to a safe outcome label using
// the same mapping DelegateSweep applies to terminalize a RunAttempt.
func delegatedObservedExitLabel(dev delegateEvent) string {
	switch {
	case dev.Reason == "spawn_finalize_failure":
		return "failed"
	case dev.ExitCode != nil && *dev.ExitCode == 0:
		return "succeeded"
	case dev.ExitCode != nil && *dev.ExitCode > 0:
		return "failed"
	case dev.Signal != "":
		return "failed"
	default:
		return "vanished"
	}
}

func delegatedStatusRank(status string) int {
	switch status {
	case "running":
		return 10
	case "unknown":
		return 20
	case "refused":
		return 30
	case "exited":
		return 40
	default:
		return 90
	}
}

// delegatedParseTime accepts the UnixNano string ts() emits or an RFC3339
// timestamp, returning a comparable time.
func delegatedParseTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(0, n).UTC(), true
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), true
	}
	return time.Time{}, false
}

func delegatedTSAfter(candidate, current string) bool {
	if strings.TrimSpace(current) == "" {
		return strings.TrimSpace(candidate) != ""
	}
	c, cok := delegatedParseTime(candidate)
	p, pok := delegatedParseTime(current)
	if cok && pok {
		return c.After(p)
	}
	return candidate > current
}

func delegatedRFC3339(value string) string {
	if parsed, ok := delegatedParseTime(value); ok {
		return parsed.Format(time.RFC3339)
	}
	return ""
}
