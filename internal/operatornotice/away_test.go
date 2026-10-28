package operatornotice

// Red-first falsifiers for away mode (B2): while the operator's presence window
// says away, an ask is queued for their return instead of asked, and a work item
// it names is parked through the governed lifecycle door.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/work"
)

var (
	awayFrom      = fixedNow.Add(-time.Hour)
	awayWindowEnd = time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
)

// installPresence imports a minimal operator state and sets a presence window
// through the B1 door (the same one `compound policy presence set` uses).
func installPresence(t *testing.T, homePath string, from, through time.Time) {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "operator-state.json")
	raw := []byte(`{
  "mode": "closure_over_expansion",
  "autonomy_lever": {"level": "L3", "set_at": "2026-06-30T12:00:00Z", "set_by": "fixture", "meaning": "test"}
}`)
	if err := os.WriteFile(sourcePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportOperatorState(homePath, sourcePath, fixedNow); err != nil {
		t.Fatalf("seed operator state: %v", err)
	}
	if _, err := policy.SetOperatorPresence(homePath, policy.OperatorPresence{
		State: policy.PresenceAway, From: from.Format(time.RFC3339), Through: through.Format(time.RFC3339), SetBy: "operator:test",
	}, fixedNow); err != nil {
		t.Fatalf("set presence: %v", err)
	}
}

func readQueue(t *testing.T, homePath string) []ReturnQueueEntry {
	t.Helper()
	f, err := os.Open(ReturnQueuePath(homePath))
	if err != nil {
		t.Fatalf("open return queue: %v", err)
	}
	defer f.Close()
	var out []ReturnQueueEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e ReturnQueueEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("queue line is not JSON: %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

// TestAwayWindowNotYetActiveIsPresent: a scheduled window is not an away
// window. Cards stay byte-identical to the B1 golden and nothing is queued.
func TestAwayWindowNotYetActiveIsPresent(t *testing.T) {
	homePath := seedHeldHome(t, "work_golden_a", "work_golden_b", "work_golden_c")
	installPresence(t, homePath, fixedNow.Add(24*time.Hour), awayWindowEnd)
	got := captureCards(t, homePath, goldenFrontier(), fixedNow)
	want, err := os.ReadFile(presentCardsGolden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("a not-yet-active window changed the cards.\nwant:\n%s\ngot:\n%s", want, got)
	}
	if _, err := os.Stat(ReturnQueuePath(homePath)); !os.IsNotExist(err) {
		t.Fatalf("a present flush wrote the return queue (stat err=%v)", err)
	}
}

// TestAwayQueuesAsksForReturn: while away every ask lands in the return queue
// carrying the window's end, the card says it is queued instead of asking, and
// a second flush updates last_seen rather than appending a duplicate.
func TestAwayQueuesAsksForReturn(t *testing.T) {
	homePath := seedHeldHome(t, "work_golden_a", "work_golden_b", "work_golden_c")
	installPresence(t, homePath, awayFrom, awayWindowEnd)
	through := awayWindowEnd.Format(time.RFC3339)

	cards := captureCards(t, homePath, goldenFrontier(), fixedNow)
	for _, card := range strings.Split(strings.TrimSpace(cards), "\n---\n") {
		if !strings.Contains(card, "queued for your return (through "+through+")") {
			t.Errorf("away card does not say it is queued:\n%s", card)
		}
		if strings.Contains(card, "decide=") {
			t.Errorf("away card still asks for a decision:\n%s", card)
		}
	}

	first := readQueue(t, homePath)
	if len(first) != 3 {
		t.Fatalf("queue lines = %d, want 3: %+v", len(first), first)
	}
	byWork := map[string]ReturnQueueEntry{}
	for _, e := range first {
		byWork[e.WorkID] = e
		if e.Status != ReturnQueueQueued || e.PresenceThrough != through || e.At != fixedNow.Format(time.RFC3339) || e.ID == "" || e.Kind == "" {
			t.Errorf("queue entry shape: %+v", e)
		}
	}
	if got := byWork["work_golden_a"].DecideCommand; got != "compound reviews resolve --area route-promotions" {
		t.Errorf("decide_command = %q", got)
	}
	if got := byWork["work_golden_b"].DecideCommand; got != "" {
		t.Errorf("an actionless ask must store no decide command, got %q", got)
	}
	if got := byWork["work_golden_a"].Summary; got != "route-promotions needs an operator decision." {
		t.Errorf("summary = %q", got)
	}

	later := fixedNow.Add(2 * time.Hour)
	captureCards(t, homePath, goldenFrontier(), later)
	second := readQueue(t, homePath)
	if len(second) != 3 {
		t.Fatalf("re-notify duplicated the queue: %d lines", len(second))
	}
	for _, e := range second {
		if e.LastSeen != later.Format(time.RFC3339) {
			t.Errorf("re-notify did not update last_seen: %+v", e)
		}
		if e.At != fixedNow.Format(time.RFC3339) {
			t.Errorf("re-notify moved the first-queued time: %+v", e)
		}
	}
}

func runRQ(t *testing.T, homePath string, now time.Time, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runReturnQueueCLI(homePath, args, &stdout, &stderr, now)
	return code, stdout.String(), stderr.String()
}

func writeQueueLines(t *testing.T, homePath string, entries ...ReturnQueueEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(ReturnQueuePath(homePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(line, '\n'))
	}
	if err := os.WriteFile(ReturnQueuePath(homePath), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReturnQueueListShowAck: list is oldest first whatever the file order; ack
// flips the status, records who, and prints the stored decide command.
func TestReturnQueueListShowAck(t *testing.T) {
	homePath := t.TempDir()
	newer := ReturnQueueEntry{ID: "rq_newer", At: "2026-12-20T00:00:00Z", LastSeen: "2026-12-21T00:00:00Z", Kind: "adjudicate", WorkID: "work_newer", Summary: "newer ask", DecideCommand: "compound reviews resolve --area newer", PresenceThrough: "2027-01-15T00:00:00Z", Status: ReturnQueueQueued}
	older := ReturnQueueEntry{ID: "rq_older", At: "2026-12-02T00:00:00Z", LastSeen: "2026-12-21T00:00:00Z", Kind: "review", WorkID: "work_older", Summary: "older ask", DecideCommand: "compound reviews resolve --area older", PresenceThrough: "2027-01-15T00:00:00Z", Status: ReturnQueueQueued}
	writeQueueLines(t, homePath, newer, older)

	code, out, errOut := runRQ(t, homePath, fixedNow, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, errOut)
	}
	if i, j := strings.Index(out, "rq_older"), strings.Index(out, "rq_newer"); i < 0 || j < 0 || i > j {
		t.Fatalf("list is not oldest first:\n%s", out)
	}

	code, out, _ = runRQ(t, homePath, fixedNow, "show", "rq_older")
	if code != 0 || !strings.Contains(out, `"work_id": "work_older"`) {
		t.Fatalf("show exit %d:\n%s", code, out)
	}

	if code, _, _ := runRQ(t, homePath, fixedNow, "ack", "rq_older"); code != 2 {
		t.Fatalf("ack without --by must be a usage error, got exit %d", code)
	}
	if code, _, _ := runRQ(t, homePath, fixedNow, "ack", "rq_missing", "--by", "operator:test"); code != 1 {
		t.Fatalf("ack of an unknown id must fail, got exit %d", code)
	}

	code, out, errOut = runRQ(t, homePath, fixedNow, "ack", "rq_older", "--by", "operator:test")
	if code != 0 {
		t.Fatalf("ack exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "compound reviews resolve --area older") {
		t.Fatalf("ack did not print the stored decide command:\n%s", out)
	}
	entries := readQueue(t, homePath)
	if len(entries) != 2 {
		t.Fatalf("ack changed the line count: %+v", entries)
	}
	for _, e := range entries {
		switch e.ID {
		case "rq_older":
			if e.Status != ReturnQueueAcked || e.AckedBy != "operator:test" || e.AckedAt != fixedNow.Format(time.RFC3339) {
				t.Fatalf("ack did not flip the entry: %+v", e)
			}
		case "rq_newer":
			if e.Status != ReturnQueueQueued {
				t.Fatalf("ack touched another entry: %+v", e)
			}
		}
	}
}

// TestAwayDoctorLines: one away line while away (count + oldest queued), and a
// deadman configured/not configured line.
func TestAwayDoctorLines(t *testing.T) {
	homePath := seedHeldHome(t)
	var present bytes.Buffer
	RenderDoctorLines(&present, homePath, fixedNow)
	if strings.Contains(present.String(), "away:") || !strings.Contains(present.String(), "deadman: not configured") {
		t.Fatalf("present doctor lines = %q", present.String())
	}

	installPresence(t, homePath, awayFrom, awayWindowEnd)
	writeQueueLines(t, homePath,
		ReturnQueueEntry{ID: "rq_b", At: "2026-12-05T00:00:00Z", Status: ReturnQueueQueued},
		ReturnQueueEntry{ID: "rq_a", At: "2026-12-02T00:00:00Z", Status: ReturnQueueQueued},
		ReturnQueueEntry{ID: "rq_done", At: "2026-12-01T00:00:00Z", Status: ReturnQueueAcked},
	)
	writePingURL(t, homePath, "https://hc-ping.invalid/abc", 0o600)
	var away bytes.Buffer
	RenderDoctorLines(&away, homePath, fixedNow)
	got := away.String()
	if !strings.Contains(got, "away: 2 asks queued for return (oldest 2026-12-02T00:00:00Z)") {
		t.Fatalf("away doctor line = %q", got)
	}
	if !strings.Contains(got, "deadman: configured") {
		t.Fatalf("deadman doctor line = %q", got)
	}
	if strings.Contains(got, "hc-ping.invalid") {
		t.Fatalf("doctor leaked the ping URL: %q", got)
	}
}

// TestAwayHoldsReadyWorkThroughGovernedAdvance: an ask bound to a holdable work
// item moves it to held through work.Advance, carrying the dated reason and a
// recorded PolicyDecision; re-notifying does not advance it again.
func TestAwayHoldsReadyWorkThroughGovernedAdvance(t *testing.T) {
	homePath := seedHeldHome(t, "work_already_held")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.Create(work.CreateInput{
		Owner:            "compound-tests",
		Actor:            "operator-test",
		SuccessPredicate: "a ready work an away ask names is parked until the operator returns",
		State:            "ready",
	}, fixedNow.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	installPresence(t, homePath, awayFrom, awayWindowEnd)
	through := awayWindowEnd.Format(time.RFC3339)

	in := reportWith(
		needsYouGroup("route-promotions", ready.ID, true),
		needsYouGroup("workbench-holds", "work_already_held", false),
		needsYouGroup("capability-lifecycle-proposals", "clp_not_a_work_item", true),
	)
	captureCards(t, homePath, in, fixedNow)

	held, events, err := store.Show(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.State != "held" {
		t.Fatalf("ready work was not held: state=%s", held.State)
	}
	last := events[len(events)-1]
	if last.To != "held" || !strings.HasPrefix(last.Note, "operator_away_until:"+through) {
		t.Fatalf("hold event lacks the dated reason: %+v", last)
	}
	if held.PolicyDecisionID == "" {
		t.Fatal("hold recorded no policy decision")
	}
	es, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.Read(held.PolicyDecisionID); err != nil {
		t.Fatalf("policy decision %s not recorded: %v", held.PolicyDecisionID, err)
	}

	byWork := map[string]ReturnQueueEntry{}
	for _, e := range readQueue(t, homePath) {
		byWork[e.WorkID] = e
	}
	if h := byWork[ready.ID].Hold; h == nil || h.Outcome != "held" || h.PolicyDecisionID != held.PolicyDecisionID {
		t.Fatalf("queue entry does not record the hold: %+v", byWork[ready.ID])
	}
	if h := byWork["work_already_held"].Hold; h == nil || h.Outcome != "already_held" {
		t.Fatalf("already-held work: %+v", byWork["work_already_held"])
	}
	if h := byWork["clp_not_a_work_item"].Hold; h == nil || h.Outcome != "not_a_work_item" {
		t.Fatalf("non-work ask: %+v", byWork["clp_not_a_work_item"])
	}

	captureCards(t, homePath, in, fixedNow.Add(time.Hour))
	_, again, err := store.Show(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(events) {
		t.Fatalf("re-notify advanced the work again: %d -> %d events", len(events), len(again))
	}
}
