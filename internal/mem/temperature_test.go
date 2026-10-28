package mem

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tempRecent/tempOld are last_injected stamps relative to t0 (the mem-test fixed clock).
var (
	tempRecent = t0.Format(time.RFC3339)                           // age 0 => decay 1
	tempOld    = t0.Add(-60 * 24 * time.Hour).Format(time.RFC3339) // 60 days old => heavy decay
)

// writeUsageProjection fabricates a minimal capability-usage projection under projections/ so a
// temperature build has a reinforcement signal to join against, without running the receipt sweep.
func writeUsageProjection(t *testing.T, home string, refs []usageInjectedRef) {
	t.Helper()
	doc := map[string]any{
		"schema_version": "compound.capability_usage.v0.1",
		"injected_refs":  refs,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "projections", "capability-usage.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// tempSeedClaim captures + promotes one claim and returns its current ledger id.
func tempSeedClaim(t *testing.T, s *Store, entity, pred, obj string, now time.Time) string {
	t.Helper()
	got, err := s.Capture([]Candidate{cand(entity, pred, obj, "2026-06-01T00:00:00Z")}, now)
	if err != nil || len(got) != 1 {
		t.Fatalf("capture %s.%s: got %d, err %v", entity, pred, len(got), err)
	}
	res, err := promoteForTest(t, s, got[0].ID, "", now)
	if err != nil || res.Claim == nil {
		t.Fatalf("promote %s.%s: %v", entity, pred, err)
	}
	return res.Claim.ID
}

func findTempEntry(proj TemperatureProjection, claimID string) (TemperatureEntry, bool) {
	for _, e := range proj.Entries {
		if e.ClaimID == claimID {
			return e, true
		}
	}
	return TemperatureEntry{}, false
}

// Falsifier 1 + 7: the projection JOINS usage injected_refs against CURRENT claims — a consumed
// claim warms, an ignored claim cools, and refs that are not a current claim:// (a skill ref, an
// absent claim id) never appear. Red without the join/filter; green with it.
func TestBuildTemperatureJoinsUsageAgainstCurrentClaims(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	idWarm := tempSeedClaim(t, s, "compound", "warm_pred", "consumed fact", t0)
	idCool := tempSeedClaim(t, s, "compound", "cool_pred", "ignored fact", t0)

	writeUsageProjection(t, home, []usageInjectedRef{
		{Ref: "claim://" + idWarm, InjectedCount: 3, ConsumedCount: 3, IgnoredCount: 0, LastInjected: tempRecent},
		{Ref: "claim://" + idCool, InjectedCount: 3, ConsumedCount: 0, IgnoredCount: 3, LastInjected: tempRecent},
		{Ref: "skill://ui-ux-pro", InjectedCount: 2, ConsumedCount: 2, IgnoredCount: 0, LastInjected: tempRecent},
		{Ref: "claim://absent-claim-zzz", InjectedCount: 1, ConsumedCount: 1, IgnoredCount: 0, LastInjected: tempRecent},
	})

	proj, err := BuildTemperature(home, TemperatureBuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(proj.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (skill ref + absent claim must drop): %+v", len(proj.Entries), proj.Entries)
	}
	warm, ok := findTempEntry(proj, idWarm)
	if !ok || warm.Temperature <= 0 {
		t.Fatalf("warm claim entry = %+v ok=%v, want temperature > 0", warm, ok)
	}
	cool, ok := findTempEntry(proj, idCool)
	if !ok || cool.Temperature >= 0 {
		t.Fatalf("cool claim entry = %+v ok=%v, want temperature < 0", cool, ok)
	}
	if _, ok := findTempEntry(proj, "absent-claim-zzz"); ok {
		t.Fatal("absent (non-current) claim must not appear in the projection")
	}
	if warm.Consumed != 3 || cool.Ignored != 3 {
		t.Fatalf("raw counts not carried through: warm=%+v cool=%+v", warm, cool)
	}
}

// Falsifier 2: the v1 formula's five spec-enforced properties, one named sub-case each.
func TestTemperatureProperties(t *testing.T) {
	// (a) consumed reinforces — more consumed => warmer, and warm is positive.
	t.Run("consumed_reinforces", func(t *testing.T) {
		less := temperatureValue(1, 0, tempRecent, t0)
		more := temperatureValue(5, 0, tempRecent, t0)
		if !(more > less && less > 0) {
			t.Fatalf("want more(%v) > less(%v) > 0", more, less)
		}
	})
	// (b) ignored cools relative to consumed — an always-ignored claim is colder than a consumed
	// one, and drops below neutral (0).
	t.Run("ignored_cools_relative_to_consumed", func(t *testing.T) {
		consumed := temperatureValue(5, 0, tempRecent, t0)
		ignored := temperatureValue(0, 5, tempRecent, t0)
		if !(ignored < consumed) {
			t.Fatalf("ignored(%v) must be colder than consumed(%v)", ignored, consumed)
		}
		if !(ignored < 0) {
			t.Fatalf("always-ignored must cool below neutral, got %v", ignored)
		}
	})
	// (c) never-injected / balanced = exactly neutral 0 — absence of a net signal is not cold; a
	// new claim is never penalized.
	t.Run("neutral_is_exactly_zero", func(t *testing.T) {
		if v := temperatureValue(0, 0, tempRecent, t0); v != 0 {
			t.Fatalf("never-injected temperature = %v, want exactly 0", v)
		}
		if v := temperatureValue(4, 4, tempRecent, t0); v != 0 {
			t.Fatalf("balanced consumed==ignored temperature = %v, want exactly 0", v)
		}
	})
	// (d) recency decay — same counts, older last_injected => cooler (closer to neutral).
	t.Run("recency_decay", func(t *testing.T) {
		fresh := temperatureValue(5, 0, tempRecent, t0)
		aged := temperatureValue(5, 0, tempOld, t0)
		if !(fresh > aged && aged > 0) {
			t.Fatalf("want fresh(%v) > aged(%v) > 0", fresh, aged)
		}
	})
	// (e) bounded — |temperature| never exceeds the ceiling no matter how large the counts
	// (tanh saturates to ±1 in float64, so the extreme lands exactly ON the ceiling, never past).
	t.Run("bounded_by_ceiling", func(t *testing.T) {
		hot := temperatureValue(1_000_000, 0, tempRecent, t0)
		cold := temperatureValue(0, 1_000_000, tempRecent, t0)
		if !(hot <= MaxTemperatureContribution && hot > 0) {
			t.Fatalf("hot(%v) must be in (0, %v]", hot, MaxTemperatureContribution)
		}
		if !(cold >= -MaxTemperatureContribution && cold < 0) {
			t.Fatalf("cold(%v) must be in [-%v, 0)", cold, MaxTemperatureContribution)
		}
	})
}

// Absent usage projection => empty (but valid) temperature projection, not an error.
func TestBuildTemperatureAbsentUsageProjectionIsEmpty(t *testing.T) {
	home := t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	proj, err := BuildTemperature(home, TemperatureBuildOptions{Now: t0})
	if err != nil {
		t.Fatalf("absent usage projection should not error: %v", err)
	}
	if len(proj.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(proj.Entries))
	}
	if proj.SchemaVersion != TemperatureSchemaVersion || proj.ProjectionRef != TemperatureProjectionRef {
		t.Fatalf("projection header not stamped: %+v", proj)
	}
}

// Falsifier 6: same inputs + same Now => identical projection bytes.
func TestBuildTemperatureDeterministic(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	idA := tempSeedClaim(t, s, "compound", "pred_a", "fact a", t0)
	idB := tempSeedClaim(t, s, "compound", "pred_b", "fact b", t0)
	writeUsageProjection(t, home, []usageInjectedRef{
		{Ref: "claim://" + idA, InjectedCount: 4, ConsumedCount: 3, IgnoredCount: 1, LastInjected: tempRecent},
		{Ref: "claim://" + idB, InjectedCount: 2, ConsumedCount: 0, IgnoredCount: 2, LastInjected: tempOld},
	})
	first, err := BuildTemperature(home, TemperatureBuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildTemperature(home, TemperatureBuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(first)
	b2, _ := json.Marshal(second)
	if string(b1) != string(b2) {
		t.Fatalf("non-deterministic build:\n%s\n%s", b1, b2)
	}
}

// Falsifier 7 (explicit): a superseded claim's stale usage entry disappears on rebuild — the
// projection is rebuilt from scratch against CURRENT claims, never accumulated.
func TestBuildTemperatureRebuiltFromScratchDropsSupersededClaim(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	got1, err := s.Capture([]Candidate{cand("compound", "stance", "first value", "2026-06-01T00:00:00Z")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	res1, err := promoteForTest(t, s, got1[0].ID, "", t0)
	if err != nil || res1.Claim == nil {
		t.Fatalf("promote v1: %v", err)
	}
	idV1 := res1.Claim.ID

	// Usage references v1; a first build sees it.
	writeUsageProjection(t, home, []usageInjectedRef{
		{Ref: "claim://" + idV1, InjectedCount: 2, ConsumedCount: 2, IgnoredCount: 0, LastInjected: tempRecent},
	})
	before, err := BuildTemperature(home, TemperatureBuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findTempEntry(before, idV1); !ok {
		t.Fatalf("v1 should be present before supersession: %+v", before.Entries)
	}

	// Supersede v1 with a new value; v1 is no longer a current claim.
	got2, err := s.Capture([]Candidate{cand("compound", "stance", "second value", "2026-06-02T00:00:00Z")}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	res2, err := promoteForTest(t, s, got2[0].ID, "", t0.Add(time.Minute))
	if err != nil || res2.Claim == nil {
		t.Fatalf("promote v2: %v", err)
	}
	if res2.Superseded == "" {
		t.Fatalf("v2 promote should supersede v1")
	}

	// Rebuild with the SAME (stale) usage still pointing at v1 => v1 drops out.
	after, err := BuildTemperature(home, TemperatureBuildOptions{Now: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findTempEntry(after, idV1); ok {
		t.Fatalf("superseded v1 must disappear on rebuild: %+v", after.Entries)
	}
}

// LoadTemperatureLookup fail-open: an unbuilt projection returns a nil map (all-neutral).
func TestLoadTemperatureLookupFailOpen(t *testing.T) {
	home := t.TempDir()
	lookup, err := LoadTemperatureLookup(home)
	if err != nil {
		t.Fatalf("missing projection should not error: %v", err)
	}
	if lookup != nil {
		t.Fatalf("missing projection lookup = %v, want nil", lookup)
	}
	if lookup["anything"] != 0 {
		t.Fatalf("nil lookup must read 0 for any id")
	}
}

// Round-trip: build => write => load yields the same temperatures the ranker will read.
func TestTemperatureWriteReadRoundTrip(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id := tempSeedClaim(t, s, "compound", "pred", "fact", t0)
	writeUsageProjection(t, home, []usageInjectedRef{
		{Ref: "claim://" + id, InjectedCount: 3, ConsumedCount: 3, IgnoredCount: 0, LastInjected: tempRecent},
	})
	proj, err := BuildTemperature(home, TemperatureBuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTemperature(home, proj); err != nil {
		t.Fatal(err)
	}
	lookup, err := LoadTemperatureLookup(home)
	if err != nil {
		t.Fatal(err)
	}
	want := proj.Entries[0].Temperature
	if got := lookup[id]; math.Abs(got-want) > 1e-12 {
		t.Fatalf("lookup[%s] = %v, want %v", id, got, want)
	}
}
