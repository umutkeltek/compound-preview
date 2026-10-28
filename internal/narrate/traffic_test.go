package narrate_test

import (
	"strings"
	"testing"

	"compound/internal/narrate"
	"compound/internal/reviewartifact"
)

func trafficBeat(res narrate.Result) *narrate.Beat {
	for i := range res.Script.Beats {
		if res.Script.Beats[i].ThreadID == "route|cheap-share" {
			return &res.Script.Beats[i]
		}
	}
	return nil
}

// A present split renders ONE self-describing cheap-share beat: the by-call and
// by-token percentages are computed independently, the body passes the operator
// grammar, and the beat sits OUTSIDE attention conservation (it never changes
// narrated+folded==total_members).
func TestTrafficBeatRendersSelfDescribingRatio(t *testing.T) {
	report := baselineReport()
	res := buildOrDie(t, narrate.Inputs{
		Attention:  report,
		Changed:    narrate.ChangedSet{CurrentSeq: 10, FirstRun: true},
		Receipts:   baselineReceipts(),
		Traffic:    narrate.TrafficSplit{Present: true, TotalCalls: 4, CheapCalls: 3, TotalTokens: 200, CheapTokens: 120},
		ThreadSeen: map[string]narrate.ThreadSeen{},
		Now:        fixtureNow(),
	}, narrate.Options{Mood: narrate.MoodDecide})

	beat := trafficBeat(res)
	if beat == nil {
		t.Fatalf("no cheap-share beat produced from a present traffic split")
	}
	if beat.Act != narrate.ActReport {
		t.Fatalf("cheap-share beat act = %q, want report", beat.Act)
	}
	// by-call share (3 of 4 = 75%) and the independent by-token share (120/200 = 60%).
	for _, want := range []string{"3 of 4", "75%", "60%", "token volume"} {
		if !strings.Contains(beat.Body, want) {
			t.Fatalf("cheap-share body missing %q: %q", want, beat.Body)
		}
	}
	// self-describing basis: the sentence states the split is spend-based.
	if !strings.Contains(beat.Body, "By spend") {
		t.Fatalf("cheap-share body does not name its spend basis: %q", beat.Body)
	}
	// the body must pass the same operator grammar every beat obeys.
	card := reviewartifact.OperatorCard{Summary: beat.Body, ContextLabel: beat.Voice, GroupKey: "narrate|report"}
	if err := reviewartifact.ValidateOperatorCard(card); err != nil {
		t.Fatalf("cheap-share body failed grammar: %q -> %v", beat.Body, err)
	}

	// conservation is over ATTENTION members only; the cheap-share beat is an
	// extra stream and must not perturb it.
	c := res.Script.Counts
	if c.Narrated+c.Folded != c.TotalMembers {
		t.Fatalf("cheap-share beat broke attention conservation: %+v", c)
	}
	if c.TotalMembers != len(report.Groups) {
		t.Fatalf("total_members=%d, want %d (cheap-share must not be a member)", c.TotalMembers, len(report.Groups))
	}
	attnKeys := map[string]bool{}
	for _, g := range report.Groups {
		attnKeys[g.Key] = true
	}
	attnBeats := 0
	for _, b := range res.Script.Beats {
		if attnKeys[b.ThreadID] {
			attnBeats++
		}
	}
	if attnBeats != c.Narrated {
		t.Fatalf("attention beats %d != narrated %d (a non-attention beat leaked into the count)", attnBeats, c.Narrated)
	}
}

// No imported route ledger (Present=false) => the beat is absent, never a fake
// 0% / 100%.
func TestTrafficBeatAbsentWithoutLedger(t *testing.T) {
	res := buildOrDie(t, narrate.Inputs{
		Attention:  baselineReport(),
		Changed:    narrate.ChangedSet{CurrentSeq: 10, FirstRun: true},
		Traffic:    narrate.TrafficSplit{Present: false},
		ThreadSeen: map[string]narrate.ThreadSeen{},
		Now:        fixtureNow(),
	}, narrate.Options{Mood: narrate.MoodDecide})
	if beat := trafficBeat(res); beat != nil {
		t.Fatalf("cheap-share beat present with no route ledger: %q", beat.Body)
	}
}

// A present ledger with no recorded token usage renders the call share only —
// no fabricated token percentage.
func TestTrafficBeatCallsOnlyWhenNoTokens(t *testing.T) {
	res := buildOrDie(t, narrate.Inputs{
		Attention:  baselineReport(),
		Changed:    narrate.ChangedSet{CurrentSeq: 10, FirstRun: true},
		Traffic:    narrate.TrafficSplit{Present: true, TotalCalls: 2, CheapCalls: 1, TotalTokens: 0, CheapTokens: 0},
		ThreadSeen: map[string]narrate.ThreadSeen{},
		Now:        fixtureNow(),
	}, narrate.Options{Mood: narrate.MoodDecide})
	beat := trafficBeat(res)
	if beat == nil {
		t.Fatalf("no cheap-share beat for a token-less ledger")
	}
	if strings.Contains(beat.Body, "token volume") {
		t.Fatalf("cheap-share body invented a token share with no token usage: %q", beat.Body)
	}
	if !strings.Contains(beat.Body, "1 of 2") {
		t.Fatalf("cheap-share body missing the call split: %q", beat.Body)
	}
}
