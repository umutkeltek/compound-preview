package narrate_test

import (
	"strings"
	"testing"
	"time"

	"compound/internal/attention"
	"compound/internal/narrate"
	"compound/internal/reviewartifact"
)

// fullInputs builds the broadest script surface: attention members across
// bands (including a malformed-summary group that exercises the beat
// fallback), receipts with a failure reason, the cheap-share meter, and the
// injection meter.
func fullInputs() narrate.Inputs {
	fresh := fixtureNow().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	return narrate.Inputs{
		Attention: attention.Report{
			SchemaVersion: attention.SchemaVersion,
			GeneratedAt:   fixtureNow().Add(-5 * time.Minute).UTC().Format(time.RFC3339),
			Groups: []attention.Group{
				attnGroup("gate|one", attention.BandNeedsYou, attention.KindGateAdmit, "A gate decision waits on you.", "executors resume when you clear it", oneClick("gate.clear"), 2, fresh),
				attnGroup("job|one", attention.BandNeedsYou, attention.KindJobHealth, "A scheduled job is failing right now.", "the schedule stops skipping it", nil, 1, fresh),
				attnGroup("malformed|one", attention.BandNeedsYou, attention.KindVerdict, "== raw machine summary with paths /Users/tester/x.json ==", "", nil, 1, fresh),
				attnGroup("offer|one", attention.BandWatching, attention.KindProductMove, "A product move is ready to advance.", "the release inches forward", oneClick("offer.advance"), 1, fresh),
			},
		},
		Changed:    narrate.ChangedSet{CurrentSeq: 10, LastSeenSeq: 0, FirstRun: true},
		Receipts:   baselineReceipts(),
		Traffic:    narrate.TrafficSplit{Present: true, TotalCalls: 10, CheapCalls: 3, TotalTokens: 4096, CheapTokens: 2048},
		ThreadSeen: map[string]narrate.ThreadSeen{},
		Injection: &narrate.InjectionLegibility{
			Present: true, PackID: "ctx_fixture", ServedRefs: 12, ServedMemory: 3,
			ServedPositions: 2, ExcludedRefs: 4, ExcludedNoTask: 1, InjectionBytes: 4096,
		},
		Now: fixtureNow(),
	}
}

// TestScriptReadabilityHoldsAcrossMoods is the eighth-grade regression gate
// for the daily narrate surface: every mood renders the full member set, and
// every prose field narrate serves — beat bodies and unlocks, the standing
// summary, the quiet reason — must sit at or below the shared ceiling.
func TestScriptReadabilityHoldsAcrossMoods(t *testing.T) {
	for _, mood := range []narrate.Mood{narrate.MoodDecide, narrate.MoodWander, narrate.MoodWorry} {
		res := buildOrDie(t, fullInputs(), narrate.Options{Mood: mood})
		if err := narrate.CheckScriptReadability(res.Script); err != nil {
			t.Fatalf("mood %s: %v", mood, err)
		}
		if len(res.Script.Beats) == 0 {
			t.Fatalf("mood %s: no beats rendered", mood)
		}
	}
}

// TestBeatFallbackIsSpecificAndReadable pins the malformed-fixture fallback:
// the beat names the queue the item came from (not the retired generic "A new
// item needs your read."), carries an unlock action, and holds the ceiling.
// A dense area name degrades to the plain terminal sentence — never to
// jargon, never to the generic line.
func TestBeatFallbackIsSpecificAndReadable(t *testing.T) {
	fresh := fixtureNow().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name       string
		area       string
		wantBody   string
		specificTo string
	}{
		{"plain area", "panel-verdicts", "A panel verdicts item needs your decision.", "panel verdicts"},
		{"jargon area", "capability-evaluation-intake", "A review item needs your decision.", ""},
	}
	for _, c := range cases {
		in := fullInputs()
		in.Attention.Groups = []attention.Group{
			attnGroup("malformed|one", attention.BandNeedsYou, attention.KindVerdict, "== raw summary /Users/tester/x.json ==", "", oneClick("review.record"), 1, fresh),
		}
		in.Attention.Groups[0].Area = c.area
		res := buildOrDie(t, in, narrate.Options{Mood: narrate.MoodDecide})
		var beat *narrate.Beat
		for i := range res.Script.Beats {
			if res.Script.Beats[i].ThreadID == "malformed|one" {
				beat = &res.Script.Beats[i]
			}
		}
		if beat == nil {
			t.Fatalf("%s: fallback beat missing: %+v", c.name, res.Script.Beats)
		}
		if beat.Body != c.wantBody {
			t.Errorf("%s: body = %q, want %q", c.name, beat.Body, c.wantBody)
		}
		if strings.Contains(beat.Body, "needs your read") {
			t.Errorf("%s: retired generic fallback returned: %q", c.name, beat.Body)
		}
		if c.specificTo != "" && !strings.Contains(beat.Body, c.specificTo) {
			t.Errorf("%s: fallback lost the queue name: %q", c.name, beat.Body)
		}
		if beat.Unlock == "" {
			t.Errorf("%s: fallback beat carries no action unlock", c.name)
		}
	}
}

// TestCheckScriptReadabilityFailsOnDenseProse proves the gate itself trips:
// a script whose beat body reads above the ceiling fails with the field name
// and grade, so a prose regression cannot land silently.
func TestCheckScriptReadabilityFailsOnDenseProse(t *testing.T) {
	dense := narrate.Script{
		Beats: []narrate.Beat{
			{ThreadID: "dense|one", Body: "Pursuant to our antecedent correspondence, implementation of the aforementioned provisions necessitates reconsideration."},
		},
	}
	err := narrate.CheckScriptReadability(dense)
	if err == nil {
		t.Fatal("dense beat body passed the readability gate")
	}
	if !strings.Contains(err.Error(), "dense|one body") || !strings.Contains(err.Error(), "grade") {
		t.Fatalf("error must name the field and grade: %v", err)
	}
	if reviewartifact.GradeLevel(dense.Beats[0].Body) <= reviewartifact.ReadabilityGradeCeiling {
		t.Fatal("fixture body is not actually above the ceiling")
	}
}

// TestCheckScriptReadabilityCoversEveryProseField sweeps standing and quiet
// fields too.
func TestCheckScriptReadabilityCoversEveryProseField(t *testing.T) {
	err := narrate.CheckScriptReadability(narrate.Script{
		Standing: narrate.Standing{Summary: "Notwithstanding the foregoing, deliberation regarding disposition remains forthcoming."},
	})
	if err == nil || !strings.Contains(err.Error(), "standing summary") {
		t.Fatalf("standing summary not gated: %v", err)
	}
	err = narrate.CheckScriptReadability(narrate.Script{
		Quiet: &narrate.Quiet{Reason: "Notwithstanding the foregoing, tranquility persists notwithstanding extant considerations."},
	})
	if err == nil || !strings.Contains(err.Error(), "quiet reason") {
		t.Fatalf("quiet reason not gated: %v", err)
	}
}
