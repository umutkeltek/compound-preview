package operatornotice

// Away mode: asks wait for the operator's return.
//
// While the operator-state presence window says away (policy EffectivePresence,
// B1), an ask the operator cannot answer is not asked. NotifyFromInputs appends
// it to the return queue -- $COMPOUND_HOME/operator/return-queue.jsonl, one
// JSON object per line -- and the card it emits says "queued for your return"
// instead of carrying a decide command. The queue is keyed on work_id+kind, so
// a hold that stays on the frontier across many flushes is ONE line whose
// last_seen moves, never a growing pile.
//
// A queued ask that names a holdable work item is also parked through the
// governed lifecycle door (work.Store.Advance to "held", which mints its own
// ActionIntent + PolicyDecision) with the note operator_away_until:<through>.
// Releasing held work needs operator provenance, so nothing autonomous resumes
// it before the operator is back.
//
// `compound notice return-queue list` is the first thing to run on return;
// `ack <id> --by <actor>` marks an entry handled and prints the stored decide
// command, which is the command the card would have carried.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"compound/internal/attention"
	"compound/internal/policy"
	"compound/internal/work"
)

// Return-queue entry statuses.
const (
	ReturnQueueQueued = "queued"
	ReturnQueueAcked  = "acked"
)

// Work-hold outcomes recorded on a queue entry.
const (
	holdOutcomeHeld        = "held"
	holdOutcomeAlreadyHeld = "already_held"
	holdOutcomeNotWork     = "not_a_work_item"
	holdOutcomeRefused     = "refused"
)

// awayHoldActor is the lifecycle actor for the away hold. It deliberately has
// no operator provenance: parking is the system's move, releasing is the
// operator's (heldReleaseOperatorError).
const awayHoldActor = "compound:operator-notice"

// awayHoldReasonPrefix starts the note on the held transition; the window's
// end follows it, so the hold is dated on the lifecycle event itself.
const awayHoldReasonPrefix = "operator_away_until:"

// ReturnQueueEntry is one line of operator/return-queue.jsonl.
type ReturnQueueEntry struct {
	ID              string    `json:"id"`
	At              string    `json:"at"`
	LastSeen        string    `json:"last_seen,omitempty"`
	Kind            string    `json:"kind"`
	WorkID          string    `json:"work_id"`
	HoldID          string    `json:"hold_id,omitempty"`
	Summary         string    `json:"summary"`
	DecideCommand   string    `json:"decide_command"`
	PresenceThrough string    `json:"presence_through"`
	Status          string    `json:"status"`
	AckedAt         string    `json:"acked_at,omitempty"`
	AckedBy         string    `json:"acked_by,omitempty"`
	Hold            *WorkHold `json:"work_hold,omitempty"`
}

// WorkHold records what the governed hold did for the entry's work item.
type WorkHold struct {
	Outcome          string `json:"outcome"`
	FromState        string `json:"from_state,omitempty"`
	Reason           string `json:"reason,omitempty"`
	PolicyDecisionID string `json:"policy_decision_id,omitempty"`
	ActionIntentID   string `json:"action_intent_id,omitempty"`
}

// ReturnQueuePath is where the queue lives under a home.
func ReturnQueuePath(homePath string) string {
	return filepath.Join(homePath, "operator", "return-queue.jsonl")
}

// ReturnQueueIDFor is the entry id for one ask: stable per work_id+kind.
func ReturnQueueIDFor(workID, kind string) string {
	sum := sha256.Sum256([]byte(workID + "\x00" + kind))
	return "rq_" + hex.EncodeToString(sum[:])[:16]
}

// awayThrough returns the active away window's end, or "" when the operator is
// present. An unreadable operator state reads as present: the flush then
// behaves exactly as it did before away mode, and the policy loader already
// fails loudly on that state elsewhere.
func awayThrough(homePath string, now time.Time) string {
	state, err := policy.LoadOperatorStateFromHome(homePath)
	if err != nil || state.EffectivePresence(now) != policy.PresenceAway {
		return ""
	}
	return state.Presence.Through
}

// holdFn parks a queued ask's work item. A seam so tests can observe calls;
// production is governedAwayHold.
var holdFn = governedAwayHold

// governedAwayHold moves a work item to held through work.Store.Advance -- the
// governed lifecycle path, which evaluates and records its PolicyDecision and
// refuses illegal transitions, foreign leases, and policy holds by itself.
// Only work_* ids are work items; review-item ids and the like are recorded as
// not_a_work_item without touching the work store.
func governedAwayHold(homePath, workID, through string, now time.Time) *WorkHold {
	if !strings.HasPrefix(workID, "work_") {
		return &WorkHold{Outcome: holdOutcomeNotWork}
	}
	store, err := work.Open(homePath)
	if err != nil {
		return &WorkHold{Outcome: holdOutcomeRefused, Reason: err.Error()}
	}
	current, err := store.Get(workID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &WorkHold{Outcome: holdOutcomeNotWork}
		}
		return &WorkHold{Outcome: holdOutcomeRefused, Reason: err.Error()}
	}
	if current.State == "held" {
		return &WorkHold{Outcome: holdOutcomeAlreadyHeld, FromState: current.State}
	}
	reason := awayHoldReasonPrefix + through
	advanced, err := store.Advance(workID, work.AdvanceInput{To: "held", Actor: awayHoldActor, Note: reason}, now)
	if err != nil {
		return &WorkHold{Outcome: holdOutcomeRefused, FromState: current.State, Reason: err.Error()}
	}
	hold := &WorkHold{Outcome: holdOutcomeHeld, FromState: current.State, Reason: reason, PolicyDecisionID: advanced.PolicyDecisionID}
	if n := len(advanced.ActionIntentIDs); n > 0 {
		hold.ActionIntentID = advanced.ActionIntentIDs[n-1]
	}
	return hold
}

// queueAsksForReturn upserts one entry per ask under the queue lock. A new
// entry is parked through holdFn; a known one only moves last_seen and the
// window end, so re-notifying never duplicates and never re-advances work.
func queueAsksForReturn(homePath string, asks []attention.Group, through string, now time.Time) (int, error) {
	stamp := now.UTC().Format(time.RFC3339)
	queued := 0
	err := withReturnQueueLock(homePath, func() error {
		entries, err := readReturnQueue(homePath)
		if err != nil {
			return err
		}
		index := make(map[string]int, len(entries))
		for i, e := range entries {
			index[e.ID] = i
		}
		for _, g := range asks {
			workID := firstWorkID(g.ItemIDs)
			if workID == "" {
				continue
			}
			id := ReturnQueueIDFor(workID, g.Kind)
			if i, ok := index[id]; ok {
				entries[i].LastSeen = stamp
				entries[i].PresenceThrough = through
				continue
			}
			entry := ReturnQueueEntry{
				ID:              id,
				At:              stamp,
				LastSeen:        stamp,
				Kind:            g.Kind,
				WorkID:          workID,
				HoldID:          HoldIDFor(g),
				Summary:         askSummary(g),
				DecideCommand:   decideCommand(g),
				PresenceThrough: through,
				Status:          ReturnQueueQueued,
				Hold:            holdFn(homePath, workID, through, now),
			}
			index[id] = len(entries)
			entries = append(entries, entry)
			queued++
		}
		return writeReturnQueue(homePath, entries)
	})
	return queued, err
}

func askSummary(g attention.Group) string {
	if ask := strings.TrimSpace(g.OperatorSummary); ask != "" {
		return ask
	}
	return "attention hold requires operator review"
}

// withReturnQueueLock serializes queue rewrites (a flush and an ack must not
// lose each other's update). Advisory; released with the descriptor on crash.
func withReturnQueueLock(homePath string, fn func() error) error {
	dir := filepath.Dir(ReturnQueuePath(homePath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, "return-queue.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN) //nolint:errcheck // released with the fd regardless
	return fn()
}

// readReturnQueue reads every entry. A missing file is an empty queue; a line
// that does not parse is an error, never skipped -- the rewrite would drop it.
func readReturnQueue(homePath string) ([]ReturnQueueEntry, error) {
	data, err := os.ReadFile(ReturnQueuePath(homePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ReturnQueueEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e ReturnQueueEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("return queue line %d: %w", n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// writeReturnQueue replaces the file atomically (temp + fsync + rename).
func writeReturnQueue(homePath string, entries []ReturnQueueEntry) error {
	path := ReturnQueuePath(homePath)
	var buf bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "return-queue.jsonl.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// sortOldestFirst orders entries by first-queued time, then id.
func sortOldestFirst(entries []ReturnQueueEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].At != entries[j].At {
			return entries[i].At < entries[j].At
		}
		return entries[i].ID < entries[j].ID
	})
}

const returnQueueUsage = "usage: compound notice return-queue list [--json] [--home P]\n" +
	"       compound notice return-queue show <id> [--home P]\n" +
	"       compound notice return-queue ack <id> --by <actor> [--home P]"

func runReturnQueueCLI(homePath string, args []string, stdout, stderr io.Writer, now time.Time) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, returnQueueUsage)
		return 2
	}
	switch args[0] {
	case "list":
		jsonOut := false
		for _, a := range args[1:] {
			if a != "--json" {
				fmt.Fprintln(stderr, returnQueueUsage)
				return 2
			}
			jsonOut = true
		}
		entries, err := readReturnQueue(homePath)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		sortOldestFirst(entries)
		if jsonOut {
			if entries == nil {
				entries = []ReturnQueueEntry{}
			}
			return writeJSON(stdout, stderr, entries)
		}
		printReturnQueue(stdout, entries)
		return 0
	case "show":
		if len(args) != 2 {
			fmt.Fprintln(stderr, returnQueueUsage)
			return 2
		}
		entries, err := readReturnQueue(homePath)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, e := range entries {
			if e.ID == args[1] {
				return writeJSON(stdout, stderr, e)
			}
		}
		fmt.Fprintf(stderr, "error: no return-queue entry %s\n", args[1])
		return 1
	case "ack":
		id, by, ok := parseAckArgs(args[1:])
		if !ok {
			fmt.Fprintln(stderr, returnQueueUsage)
			return 2
		}
		acked, err := ackReturnQueue(homePath, id, by, now)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "acked %s by %s (work=%s)\n", acked.ID, acked.AckedBy, acked.WorkID)
		if acked.DecideCommand != "" {
			fmt.Fprintf(stdout, "decide: %s\n", acked.DecideCommand)
		} else {
			fmt.Fprintln(stdout, "decide: (none stored; this ask carried no adjudicating command)")
		}
		return 0
	default:
		fmt.Fprintln(stderr, returnQueueUsage)
		return 2
	}
}

func parseAckArgs(args []string) (id, by string, ok bool) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--by" && i+1 < len(args):
			by = strings.TrimSpace(args[i+1])
			i++
		case strings.HasPrefix(args[i], "--"):
			return "", "", false
		case id == "":
			id = strings.TrimSpace(args[i])
		default:
			return "", "", false
		}
	}
	return id, by, id != "" && by != ""
}

// ackReturnQueue marks one entry acked. Acking an acked entry keeps the first
// ack's who/when and still returns the entry, so the command can be reprinted.
func ackReturnQueue(homePath, id, by string, now time.Time) (ReturnQueueEntry, error) {
	var acked ReturnQueueEntry
	err := withReturnQueueLock(homePath, func() error {
		entries, err := readReturnQueue(homePath)
		if err != nil {
			return err
		}
		for i := range entries {
			if entries[i].ID != id {
				continue
			}
			if entries[i].Status != ReturnQueueAcked {
				entries[i].Status = ReturnQueueAcked
				entries[i].AckedAt = now.UTC().Format(time.RFC3339)
				entries[i].AckedBy = by
				if err := writeReturnQueue(homePath, entries); err != nil {
					return err
				}
			}
			acked = entries[i]
			return nil
		}
		return fmt.Errorf("no return-queue entry %s", id)
	})
	return acked, err
}

func printReturnQueue(w io.Writer, entries []ReturnQueueEntry) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "return queue: empty")
		return
	}
	queued := 0
	for _, e := range entries {
		if e.Status == ReturnQueueQueued {
			queued++
		}
	}
	fmt.Fprintf(w, "return queue: %d queued, %d acked (oldest first)\n", queued, len(entries)-queued)
	for _, e := range entries {
		fmt.Fprintf(w, "%s  %-6s  %s  work=%s  kind=%s\n    %s\n", e.ID, e.Status, e.At, e.WorkID, e.Kind, e.Summary)
		if e.Hold != nil && e.Hold.Outcome == holdOutcomeHeld {
			fmt.Fprintf(w, "    work held from %s (%s)\n", e.Hold.FromState, e.Hold.Reason)
		}
	}
	fmt.Fprintln(w, "next: compound notice return-queue show <id> | ack <id> --by <actor>")
}

func writeJSON(stdout, stderr io.Writer, v any) int {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(body))
	return 0
}

// RenderDoctorLines prints the away-mode doctor lines: while an away window is
// in force, how many asks wait for return and since when; always, whether the
// dead-man ping is configured. Read-only: it creates nothing in the home.
func RenderDoctorLines(w io.Writer, homePath string, now time.Time) {
	if through := awayThrough(homePath, now); through != "" {
		entries, err := readReturnQueue(homePath)
		if err != nil {
			fmt.Fprintf(w, "- away: return queue unreadable: %v\n", err)
		} else {
			queued, oldest := 0, ""
			for _, e := range entries {
				if e.Status != ReturnQueueQueued {
					continue
				}
				queued++
				if oldest == "" || e.At < oldest {
					oldest = e.At
				}
			}
			if queued == 0 {
				fmt.Fprintf(w, "- away: 0 asks queued for return (window through %s)\n", through)
			} else {
				fmt.Fprintf(w, "- away: %d asks queued for return (oldest %s)\n", queued, oldest)
			}
		}
	}
	fmt.Fprintf(w, "- deadman: %s\n", deadmanState(homePath))
}
