package capgraph

import "testing"

// Both behaviors below were shipped against a measured defect on the live graph.
// Neither had a test of its own until now: the change modified two adjacent
// tests whose fixtures happened to depend on the old behavior, which is not the
// same as asserting the new one. A guarantee with no failing case is a guarantee
// that erodes at the next refactor.

func TestReputationCannotSurfaceAnIrrelevantCapability(t *testing.T) {
	// The live failure: a video-generation skill tied with the correct
	// debugging skill for "fix the flaky swift test", having matched zero task
	// tokens, purely on validated_by history. A track record is evidence about
	// ranking among candidates, never evidence of being a candidate.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Add(Node{
		ID: "skill:unrelated-but-popular", Kind: "skill", Name: "Unrelated",
		Triggers: []string{"videography"}, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// The validation CONTEXT must match the task while the node's own triggers do
	// not -- that is the shape of the live failure, and the only shape that
	// exercises the gate. An earlier version of this test used a context that
	// also missed the task, so validatedAdd never accumulated and the test
	// passed with the gate removed: it asserted nothing.
	for i := 0; i < 8; i++ {
		if err := s.AddEdge(Edge{
			Kind: EdValidatedBy, From: "context:migration", To: "skill:unrelated-but-popular",
			Confidence: 1, Weight: 1, SourceReceipt: "rec_" + string(rune('a'+i)),
		}, t0); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.Surface("fix the flaky database migration")
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if hit := findSurfaceHit(hits, "skill:unrelated-but-popular"); hit != nil {
		t.Fatalf("a capability matching NO task token surfaced on reputation alone (score %.1f); "+
			"validated_by must reorder candidates, not create them", hit.Score)
	}
}

func TestReputationStillLiftsARelevantCapability(t *testing.T) {
	// The gate must not have thrown away what validated_by is for. If this and
	// the test above cannot both hold, the gate was implemented as a mute.
	build := func(withHistory bool) float64 {
		s, err := Open(t.TempDir())
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := s.Add(Node{
			ID: "skill:relevant", Kind: "skill", Name: "Relevant",
			Triggers: []string{"migration"}, Weight: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if withHistory {
			for i := 0; i < 3; i++ {
				if err := s.AddEdge(Edge{
					Kind: EdValidatedBy, From: "context:migration", To: "skill:relevant",
					Confidence: 1, Weight: 1, SourceReceipt: "rec_" + string(rune('a'+i)),
				}, t0); err != nil {
					t.Fatal(err)
				}
			}
		}
		hits, err := s.Surface("write a database migration")
		if err != nil {
			t.Fatalf("surface: %v", err)
		}
		hit := findSurfaceHit(hits, "skill:relevant")
		if hit == nil {
			t.Fatal("a topically relevant capability failed to surface at all")
		}
		return hit.Score
	}
	bare, validated := build(false), build(true)
	if validated <= bare {
		t.Fatalf("validation did not lift a relevant capability: bare %.1f, validated %.1f", bare, validated)
	}
}

func TestTriggerMatchingToleratesThePluralSplit(t *testing.T) {
	// The live failure: skill:paywalls declares the trigger "paywalls" and the
	// operator wrote "the paywall is not converting", so the skill ranked 29th
	// on its own topic. Both directions matter -- a plural trigger against a
	// singular task and the reverse -- because either half leaves a capability
	// unreachable by the obvious phrasing.
	cases := []struct{ name, trigger, task string }{
		{"plural trigger, singular task", "paywalls", "the paywall is not converting"},
		{"singular trigger, plural task", "paywall", "our paywalls are not converting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := s.Add(Node{
				ID: "skill:subject", Kind: "skill", Name: "Subject",
				Triggers: []string{tc.trigger}, Weight: 1,
			}); err != nil {
				t.Fatal(err)
			}
			hits, err := s.Surface(tc.task)
			if err != nil {
				t.Fatalf("surface: %v", err)
			}
			if findSurfaceHit(hits, "skill:subject") == nil {
				t.Fatalf("trigger %q did not match task %q; the obvious phrasing must reach the capability",
					tc.trigger, tc.task)
			}
		})
	}
}

func TestPluralToleranceDoesNotCollapseShortDistinctTokens(t *testing.T) {
	// The tolerance is trailing-"s" only and length-guarded on purpose. A router
	// that quietly conflates two different capabilities is harder to debug than
	// one that misses a plural, so this pins the conservative bound rather than
	// trusting the comment.
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Add(Node{
		ID: "skill:short", Kind: "skill", Name: "Short", Triggers: []string{"api"}, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// "apis" -> "api" is a legitimate plural and SHOULD match.
	hits, err := s.Surface("design our apis")
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if findSurfaceHit(hits, "skill:short") == nil {
		t.Fatal("a 3-character trigger lost its legitimate plural")
	}
	// A 2-character token must not gain an "s" form: too short to be a safe stem.
	s2, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s2.Add(Node{
		ID: "skill:tiny", Kind: "skill", Name: "Tiny", Triggers: []string{"go"}, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	hits2, err := s2.Surface("review the gos")
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if findSurfaceHit(hits2, "skill:tiny") != nil {
		t.Fatal("a 2-character trigger gained a plural form; the length guard is not holding")
	}
}
