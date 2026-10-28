package narrate_test

import (
	"strings"
	"testing"

	"compound/internal/narrate"
)

func injectionBeat(res narrate.Result) *narrate.Beat {
	for i := range res.Script.Beats {
		if res.Script.Beats[i].ThreadID == "memory|injection" {
			return &res.Script.Beats[i]
		}
	}
	return nil
}

// A present injection legibility renders ONE self-describing meter beat: the
// served/excluded/bytes numbers are preserved verbatim, the body passes the
// operator grammar, and the beat sits OUTSIDE attention conservation.
func TestInjectionBeatRendersGlanceableMeter(t *testing.T) {
	report := baselineReport()
	res := buildOrDie(t, narrate.Inputs{
		Attention:  report,
		Changed:    narrate.ChangedSet{CurrentSeq: 10, FirstRun: true},
		Receipts:   baselineReceipts(),
		ThreadSeen: map[string]narrate.ThreadSeen{},
		Injection: &narrate.InjectionLegibility{
			Present:         true,
			PackID:          "ctx_abc.json",
			ServedRefs:      46,
			ServedMemory:    10,
			ServedPositions: 5,
			ServedOffers:    12,
			ExcludedRefs:    103,
			ExcludedNoTask:  98,
			InjectionBytes:  4031,
		},
		Now: fixtureNow(),
	}, narrate.Options{Mood: narrate.MoodDecide})

	beat := injectionBeat(res)
	if beat == nil {
		t.Fatal("no injection beat produced from a present legibility")
	}
	if beat.Act != narrate.ActReport {
		t.Fatalf("injection beat act = %q, want report", beat.Act)
	}
	for _, want := range []string{"46 refs served", "10 memory", "5 positions", "12 offers", "103 excluded", "98 no-task", "~4KB"} {
		if !strings.Contains(beat.Body, want) {
			t.Fatalf("injection beat body missing %q: %s", want, beat.Body)
		}
	}
}

// An absent legibility (no ContextPack observed yet) degrades to NO beat — never
// a fabricated "0 served" — and attention conservation is untouched.
func TestInjectionBeatAbsentWithoutPack(t *testing.T) {
	report := baselineReport()
	res := buildOrDie(t, narrate.Inputs{
		Attention:  report,
		Changed:    narrate.ChangedSet{CurrentSeq: 10, FirstRun: true},
		Receipts:   baselineReceipts(),
		ThreadSeen: map[string]narrate.ThreadSeen{},
		Injection:  nil,
		Now:        fixtureNow(),
	}, narrate.Options{Mood: narrate.MoodDecide})

	if beat := injectionBeat(res); beat != nil {
		t.Fatalf("injection beat must be absent when no pack observed, got %+v", beat)
	}
	// Conservation holds: narrated+folded==total_members, unchanged by the absent beat.
	if res.Script.Counts.Narrated+res.Script.Counts.Folded != res.Script.Counts.TotalMembers {
		t.Fatalf("attention conservation broken: narrated+folded=%d total=%d", res.Script.Counts.Narrated+res.Script.Counts.Folded, res.Script.Counts.TotalMembers)
	}
}
