package work

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"compound/internal/runattempt"
)

// SessionLiveRow is one reconciled run attempt, plus row metadata
// contributed by the multiplexer observation source.
//
// Liveness comes from the session name RECORDED AT SPAWN
// (DelegateBinding.Session, via DelegateStatus) and a direct probe of THAT
// name. It is never inferred from similarity between an actor name and a
// session name. A prototype that matched actor "claude-minimax:wave10-contract"
// against session "mmx-w10-contract" called four healthy lanes ghosts; the same
// guess, wired to a reaper, would have reclaimed leases from a live controller
// whose sessions are named after their work rather than their owner.
//
// Each row carries Provenance so the operator can tell which observation
// source contributed which columns:
//
//	"compound"    — the row's state and identity come from a recorded
//	                 RunAttempt binding. CWD / LastActivity / ResumeCmd may
//	                 be enriched from the multiplexer source when the same
//	                 session name appears there.
//	"multiplexer" — the row exists only because the terminal multiplexer
//	                 (tmux) reported a session that compound never recorded.
//	                 WorkID and RunAttemptID are empty; the row's identity
//	                 is the session name itself.
type SessionLiveRow struct {
	State        string  `json:"state"`
	WorkID       string  `json:"work_id"`
	RunAttemptID string  `json:"run_attempt_id"`
	Actor        string  `json:"actor,omitempty"`
	Harness      string  `json:"harness,omitempty"`
	Session      string  `json:"session,omitempty"`
	Status       string  `json:"run_attempt_status"`
	AgeHours     float64 `json:"age_hours"`
	PaneExists   bool    `json:"pane_exists"`
	PaneDead     bool    `json:"pane_dead"`
	Detail       string  `json:"detail,omitempty"`

	// Provenance is "compound" for rows whose state comes from a recorded
	// RunAttempt, "multiplexer" for rows that exist only because the
	// terminal multiplexer reported the session. Always present.
	Provenance string `json:"provenance"`
	// Lane is the best-effort lane identity. For compound rows it is
	// "<harness>/<actor>"; for multiplexer rows it falls back to the
	// session name, which is the only signal the multiplexer itself
	// carries. Empty string is allowed for unbound compound rows.
	Lane string `json:"lane,omitempty"`
	// CWD is the focused pane's working directory, observed via the
	// multiplexer source. Empty for compound rows whose session the
	// multiplexer did not see (a server crash, a different tmux server,
	// etc).
	CWD string `json:"cwd,omitempty"`
	// LastActivity is the multiplexer's session_activity, formatted as
	// RFC3339 in UTC. Empty when the multiplexer did not observe the
	// pane (same conditions as CWD).
	LastActivity string `json:"last_activity,omitempty"`
	// ResumeCmd is the argv an operator can paste to put this pane back
	// on screen. For compound-bound rows this is the compound attach
	// command (`compound work delegate attach <work_id>` when a binding
	// exists); for multiplexer-only rows it is `tmux attach-session -t
	// <name>`. Empty for unbound compound rows with no multiplexer match.
	ResumeCmd string `json:"resume_cmd,omitempty"`
}

// SessionsLiveReport is one full reconciliation result. Counts and rows.
// Multiplexer rows are NOT counted in Live/Ghost/Unbound/Reservation/
// Unknown — they are a separate population with a separate provenance.
type SessionsLiveReport struct {
	SchemaVersion string           `json:"schema_version"`
	GeneratedAt   string           `json:"generated_at"`
	Live          int              `json:"live"`
	Ghost         int              `json:"ghost"`
	Unbound       int              `json:"unbound"`
	Reservation   int              `json:"reservation"`
	Unknown       int              `json:"unknown"`
	Multiplexer   int              `json:"multiplexer"`
	Rows          []SessionLiveRow `json:"rows"`
}

// SessionsLiveSchemaVersion is the current schema identifier. Bumping it
// requires coordinated consumer updates; do not change casually.
const SessionsLiveSchemaVersion = "compound.sessions_live.v0.1"

// SessionsLiveReconcileOptions is the seam used by the CLI and by tests
// to inject the multiplexer source and the runner. nil means defaults
// from the work-package seams (a fresh TmuxRunner and its Multiplexer()).
type SessionsLiveReconcileOptions struct {
	Runner         SessionRunner
	Multiplexer    MultiplexerSource
	Now            time.Time
	DelegateStatus func(workID string, runner SessionRunner) (DelegateStatusObservation, error)
}

// SessionsLiveReconcile walks every non-terminal RunAttempt through the
// compound-side probe, then folds in the multiplexer observation source.
// This is the entire logic of `compound sessions live`; the CLI file is
// just flag parsing and JSON/text dispatch.
func SessionsLiveReconcile(store *Store, homePath string, opts SessionsLiveReconcileOptions) (SessionsLiveReport, error) {
	if store == nil {
		return SessionsLiveReport{}, fmt.Errorf("sessions live: store is nil")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	runner := opts.Runner
	if runner == nil {
		runner = NewTmuxRunner()
	}
	mux := opts.Multiplexer
	if mux == nil {
		if tmuxRunner, ok := runner.(*TmuxRunner); ok {
			mux = tmuxRunner.Multiplexer()
		}
	}
	delegateStatus := opts.DelegateStatus
	if delegateStatus == nil {
		delegateStatus = store.DelegateStatus
	}
	// Non-terminal attempts are exactly the population that CLAIMS to be
	// alive; everything terminal has already settled and needs no
	// reconciliation.
	attempts, err := runattempt.Open(homePath).ListNonTerminal()
	if err != nil {
		return SessionsLiveReport{}, err
	}
	report := SessionsLiveReport{
		SchemaVersion: SessionsLiveSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
	}
	// One probe per work id: several attempts can share an envelope, and
	// the binding is a property of the envelope, not of the attempt.
	probed := map[string]DelegateStatusObservation{}
	for _, ra := range attempts {
		row := SessionLiveRow{
			WorkID:       ra.WorkID,
			RunAttemptID: ra.ID,
			Actor:        ra.Actor,
			Harness:      ra.Harness,
			Status:       ra.Status,
			AgeHours:     now.Sub(ra.CreatedAt).Hours(),
			Provenance:   "compound",
		}
		row.Lane = compoundLaneIdentity(ra.Harness, ra.Actor)

		// A claim-time placeholder is not a session, and judging it as
		// one is how a single status came to mean both "died" and
		// "worked".
		if ra.Origin == runattempt.OriginClaimReservation {
			row.State = "reservation"
			row.Detail = "claim-time placeholder; superseded when a real execution attaches"
			report.Reservation++
			report.Rows = append(report.Rows, row)
			continue
		}

		obs, ok := probed[ra.WorkID]
		if !ok {
			obs, err = delegateStatus(ra.WorkID, runner)
			if err != nil {
				obs = DelegateStatusObservation{WorkID: ra.WorkID, ProbeError: err.Error()}
			}
			probed[ra.WorkID] = obs
		}
		row.Session = obs.Session
		row.PaneExists = obs.PaneStatus.Exists
		row.PaneDead = obs.PaneStatus.PaneDead
		row.State, row.Detail = ClassifySessionLiveness(ra.Status, obs)
		switch row.State {
		case "live":
			report.Live++
		case "ghost":
			report.Ghost++
		case "unbound":
			report.Unbound++
		case "unknown":
			report.Unknown++
		}
		report.Rows = append(report.Rows, row)
	}

	// Second observation source: every tmux session the multiplexer
	// itself can see. Rows that match a recorded binding by session
	// name are merged INTO the existing compound row (provenance stays
	// "compound"; cwd / last_activity / resume_cmd come from the
	// multiplexer). Rows the multiplexer sees but compound never
	// recorded become their own rows with provenance="multiplexer" —
	// these are the operator's own panes (claude, codex, manual work)
	// that the report used to miss.
	if mux != nil {
		applyMultiplexerToReport(&report, mux)
	}

	sort.SliceStable(report.Rows, func(i, j int) bool {
		ri, rj := stateRank(report.Rows[i].State), stateRank(report.Rows[j].State)
		if ri != rj {
			return ri < rj
		}
		return report.Rows[i].AgeHours > report.Rows[j].AgeHours
	})
	return report, nil
}

// applyMultiplexerToReport is the multiplexer merge step, factored out
// of the CLI file. Errs are surfaced as a multiplexer_unknown row so
// the operator sees a real reason instead of an empty report (the
// dangerous failure is "nothing reported, looks like everything is
// fine").
func applyMultiplexerToReport(report *SessionsLiveReport, mux MultiplexerSource) {
	observations, muxErr := mux.List()
	if muxErr != nil {
		report.Rows = append(report.Rows, SessionLiveRow{
			State:      "multiplexer_unknown",
			Provenance: "multiplexer",
			Session:    "-",
			Detail:     "multiplexer source failed: " + muxErr.Error(),
		})
		report.Multiplexer++
		return
	}
	for _, obs := range observations {
		idx := findRowBySession(report.Rows, obs.Name)
		if idx >= 0 {
			mergeMultiplexerIntoRow(&report.Rows[idx], obs)
			continue
		}
		report.Rows = append(report.Rows, multiplexerOnlyRow(obs))
		report.Multiplexer++
	}
}

// stateRank orders rows for human and JSON consumers: ghosts first
// (the load-bearing ones), then unknown / unbound / live / reservation,
// then multiplexer rows (a separate population).
func stateRank(state string) int {
	switch state {
	case "ghost":
		return 0
	case "unknown":
		return 1
	case "unbound":
		return 2
	case "live":
		return 3
	case "reservation":
		return 4
	case "multiplexer":
		return 5
	case "multiplexer_unknown":
		return 6
	}
	return 99
}

// ClassifySessionLiveness decides one attempt's state from its recorded
// binding and the direct probe of the recorded session name. Order is
// load-bearing — once a branch is taken, the others are not considered:
//
//   - failed probe ABSENT measurement, never evidence of death. It is
//     checked first so a broken prober can never manufacture ghosts,
//     which a wired reaper would reclaim leases from healthy owners.
//   - an unbound attempt has no session to be absent, so cannot ghost.
//   - ghost is DEFAULT only after the three above are excluded: the
//     session exists, the probe worked, and the recorded session is
//     genuinely not running.
func ClassifySessionLiveness(attemptStatus string, obs DelegateStatusObservation) (state, detail string) {
	switch {
	case obs.ProbeError != "":
		return "unknown", "probe failed: " + obs.ProbeError
	case !obs.Bound:
		return "unbound", "non-terminal execution with no recorded session binding"
	case obs.PaneStatus.Exists && !obs.PaneStatus.PaneDead:
		return "live", ""
	default:
		return "ghost", fmt.Sprintf(
			"recorded session %q gone; attempt still reads %s and work lease still held",
			obs.Session, attemptStatus)
	}
}

// compoundLaneIdentity builds the lane string shown on every compound
// row from the harness and actor recorded at spawn. Empty parts are
// dropped so a row with no actor is not "codex/" with a trailing
// slash.
func compoundLaneIdentity(harness, actor string) string {
	harness = strings.TrimSpace(harness)
	actor = strings.TrimSpace(actor)
	switch {
	case harness == "" && actor == "":
		return ""
	case actor == "":
		return harness
	case harness == "":
		return actor
	default:
		return harness + "/" + actor
	}
}

// findRowBySession returns the index of the first row whose Session
// matches the given name, or -1 if none. Used by the multiplexer merge
// to pair multiplexer observations with compound rows by session name.
// It is intentionally substring-free — the prototype of this tool
// matched actor name against session name and called healthy lanes
// ghosts.
func findRowBySession(rows []SessionLiveRow, name string) int {
	name = strings.TrimSpace(name)
	if name == "" {
		return -1
	}
	for i := range rows {
		if rows[i].Session == name {
			return i
		}
	}
	return -1
}

// mergeMultiplexerIntoRow enriches an existing compound row with the
// columns only the multiplexer source can answer: cwd, last activity,
// and the resume command. Provenance stays "compound" — the row's
// identity still comes from the recorded RunAttempt; the multiplexer
// just observed the same pane.
//
// Existing values are not overwritten: a compound row's ResumeCmd may
// already point at `compound work delegate attach <work_id>`, which is
// a higher-level command than the multiplexer-resume
// `tmux attach-session -t <name>` and is the one an operator under
// compound control wants.
func mergeMultiplexerIntoRow(row *SessionLiveRow, obs MultiplexerObservation) {
	if row == nil {
		return
	}
	if row.CWD == "" {
		row.CWD = obs.CWD
	}
	if row.LastActivity == "" && !obs.LastActivity.IsZero() {
		row.LastActivity = obs.LastActivity.UTC().Format(time.RFC3339)
	}
	if row.ResumeCmd == "" && obs.Name != "" {
		row.ResumeCmd = "tmux attach-session -t " + obs.Name
	}
}

// multiplexerOnlyRow builds the row emitted for a tmux session that
// compound never recorded. State is "multiplexer" (a separate bucket
// that does NOT inflate Live/Ghost/Unbound/Reservation/Unknown), and
// the ResumeCmd is the raw `tmux attach-session -t <name>` the
// operator can paste to put the pane back.
func multiplexerOnlyRow(obs MultiplexerObservation) SessionLiveRow {
	row := SessionLiveRow{
		State:      "multiplexer",
		Provenance: "multiplexer",
		Session:    obs.Name,
		Lane:       obs.Name,
		CWD:        obs.CWD,
	}
	if !obs.LastActivity.IsZero() {
		row.LastActivity = obs.LastActivity.UTC().Format(time.RFC3339)
	}
	if obs.Name != "" {
		row.ResumeCmd = "tmux attach-session -t " + obs.Name
	}
	return row
}

// FilterGhostsOnly returns a copy with only ghost rows retained. Used
// by the CLI when `--ghosts-only` is requested.
func (r *SessionsLiveReport) FilterGhostsOnly() {
	kept := make([]SessionLiveRow, 0, len(r.Rows))
	for _, row := range r.Rows {
		if row.State == "ghost" {
			kept = append(kept, row)
		}
	}
	// Counts follow the visible rows so a `--ghosts-only` report does
	// not lie about the population it shows.
	r.Live, r.Ghost, r.Unbound, r.Reservation, r.Unknown, r.Multiplexer = 0, 0, 0, 0, 0, 0
	for _, row := range kept {
		switch row.State {
		case "ghost":
			r.Ghost++
		case "multiplexer", "multiplexer_unknown":
			r.Multiplexer++
		}
	}
	r.Rows = kept
}

// WriteJSON encodes the report with human-friendly indentation.
func (r *SessionsLiveReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	return enc.Encode(r)
}

// WriteText renders the report for an operator's terminal. The
// multiplexer-only rows carry extra columns (cwd / last / resume) on a
// continuation line so the table stays narrow.
func (r *SessionsLiveReport) WriteText(w io.Writer, now time.Time) {
	fmt.Fprintf(w, "%-12s %-16s %-30s %8s  %-44s\n", "STATE", "HARNESS", "SESSION", "AGE", "WORK")
	for _, row := range r.Rows {
		session := row.Session
		if session == "" {
			session = "-"
		}
		harness := row.Harness
		if harness == "" && row.Provenance == "multiplexer" {
			harness = row.Lane
		}
		age := fmt.Sprintf("%7.1fh", row.AgeHours)
		if row.AgeHours == 0 && row.LastActivity != "" {
			if parsed, err := time.Parse(time.RFC3339, row.LastActivity); err == nil {
				age = fmt.Sprintf("%7.1fh", now.Sub(parsed).Hours())
			} else {
				age = "-"
			}
		}
		work := row.WorkID
		if work == "" {
			work = "-"
		}
		fmt.Fprintf(w, "%-12s %-16s %-30s %s  %-44s\n",
			row.State, clipRunes(harness, 16), clipRunes(session, 30), age, clipRunes(work, 44))
		if row.CWD != "" || row.LastActivity != "" || row.ResumeCmd != "" {
			parts := []string{}
			if row.CWD != "" {
				parts = append(parts, "cwd="+row.CWD)
			}
			if row.LastActivity != "" {
				parts = append(parts, "last="+row.LastActivity)
			}
			if row.ResumeCmd != "" {
				parts = append(parts, "resume="+row.ResumeCmd)
			}
			fmt.Fprintf(w, "             └─ %s\n", strings.Join(parts, " "))
		}
	}
	fmt.Fprintf(w, "\nlive=%d ghost=%d unbound=%d reservation=%d unknown=%d multiplexer=%d\n",
		r.Live, r.Ghost, r.Unbound, r.Reservation, r.Unknown, r.Multiplexer)
	if r.Ghost > 0 {
		fmt.Fprintf(w, "\n%d attempt(s) claim to be running with no live session; each still holds its work lease.\n", r.Ghost)
		fmt.Fprintln(w, "Confirm the owner is really gone before reclaiming:")
		fmt.Fprintln(w, "  compound work delegate status <work_id>")
	}
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
