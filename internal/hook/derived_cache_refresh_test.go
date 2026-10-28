package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/home"
)

func writeCacheBody(t *testing.T, home, rel, stampField, stamp string) {
	t.Helper()
	path := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"schema_version": "test", "entries": []any{}}
	if stampField != "" {
		body[stampField] = stamp
	}
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func cacheStamp(t *testing.T, home, rel, field string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("cache body did not parse: %v", err)
	}
	s, _ := doc[field].(string)
	return s
}

const (
	usageRel = "projections/capability-usage.json"
	tempRel  = "projections/memory-temperature.json"
	negRel   = "projections/negatives.json"
)

// writeAllCacheBodies stamps every body the refresh gate inspects. Writing only some of
// them makes the rest count as absent-and-therefore-stale, which silently turns a
// "fresh chain" fixture into a stale one.
func writeAllCacheBodies(t *testing.T, home, stamp string) {
	t.Helper()
	writeCacheBody(t, home, usageRel, "generated_at", stamp)
	writeCacheBody(t, home, tempRel, "computed_at", stamp)
	writeCacheBody(t, home, negRel, "computed_at", stamp)
}

// TestDerivedCachesNeedRefreshTreatsUndatedAndUnreadableAsStale pins the gate.
//
// A body that cannot prove its own age must never be trusted as fresh — the same rule
// negatives.projectionStale applies. Trusting an undated body is how a cache that
// silently stopped recording its build time would age forever without ever being
// rebuilt.
func TestDerivedCachesNeedRefreshTreatsUndatedAndUnreadableAsStale(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	t.Run("both fresh means no refresh", func(t *testing.T) {
		home := t.TempDir()
		writeAllCacheBodies(t, home, now.Add(-1*time.Hour).Format(time.RFC3339))
		if derivedCachesNeedRefresh(home, time.Time{}, now) {
			t.Error("two bodies inside the window must not trigger a rebuild")
		}
	})

	t.Run("either one stale triggers the chain", func(t *testing.T) {
		for _, stale := range []string{usageRel, tempRel} {
			home := t.TempDir()
			old := now.Add(-400 * time.Hour).Format(time.RFC3339)
			writeAllCacheBodies(t, home, now.Add(-1*time.Hour).Format(time.RFC3339))
			field := "generated_at"
			if stale == tempRel {
				field = "computed_at"
			}
			writeCacheBody(t, home, stale, field, old)
			if !derivedCachesNeedRefresh(home, time.Time{}, now) {
				t.Errorf("%s stale must trigger the chain: the two stages are not independent", stale)
			}
		}
	})

	t.Run("undated is stale", func(t *testing.T) {
		home := t.TempDir()
		writeAllCacheBodies(t, home, now.Format(time.RFC3339))
		writeCacheBody(t, home, usageRel, "", "")
		if !derivedCachesNeedRefresh(home, time.Time{}, now) {
			t.Error("a body that cannot date itself must not be trusted as fresh")
		}
	})

	t.Run("absent is stale", func(t *testing.T) {
		home := t.TempDir()
		if !derivedCachesNeedRefresh(home, time.Time{}, now) {
			t.Error("a never-built cache must be treated as needing a build")
		}
	})
}

// TestClaimDerivedCacheAttemptThrottlesRetries pins the retry throttle.
//
// Without it, a build that fails every time would run on EVERY tool call — turning a
// stale cache into a permanent ~0.8s tax on every tool use, which is far worse than
// the staleness it was trying to fix.
func TestClaimDerivedCacheAttemptThrottlesRetries(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	if !claimDerivedCacheAttempt(home, now) {
		t.Fatal("first attempt must be granted")
	}
	if claimDerivedCacheAttempt(home, now.Add(time.Minute)) {
		t.Error("a second attempt inside the retry window must be refused")
	}
	if claimDerivedCacheAttempt(home, now.Add(derivedCacheAttemptRetry-time.Second)) {
		t.Error("still inside the retry window must be refused")
	}
	if !claimDerivedCacheAttempt(home, now.Add(derivedCacheAttemptRetry+time.Second)) {
		t.Error("past the retry window the attempt must be granted again")
	}

	// The marker must not carry a raw session id or path — it lands under a home that
	// gets archived and exported.
	body, err := os.ReadFile(derivedCacheAttemptPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]string
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["attempted_at"] == "" || doc["schema_version"] == "" {
		t.Errorf("marker must be self-describing, got %v", doc)
	}
	if len(doc) != 2 {
		t.Errorf("marker must carry only schema_version and attempted_at, got %v", doc)
	}
}

// TestRefreshDerivedCachesRebuildsInDependencyOrder is the falsifier for the ordering
// bug that would otherwise report success while changing nothing that matters.
//
// capability-usage feeds BuildTemperature (readInjectedRefUsage), so rebuilding
// temperature FIRST derives it from the stale usage body. Both files would carry fresh
// timestamps and every freshness check would go green, while the signal inside
// temperature was still computed from two-week-old usage data. The test asserts the
// usage body is at least as new as the temperature body — the observable consequence
// of correct ordering.
func TestRefreshDerivedCachesRebuildsInDependencyOrder(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	old := now.Add(-400 * time.Hour).Format(time.RFC3339)
	writeAllCacheBodies(t, home, old)

	refs := refreshDerivedCaches(home, true, now)
	if len(refs) == 0 {
		t.Fatal("a stale chain must produce at least one rebuilt ref")
	}

	usageStamp := cacheStamp(t, home, usageRel, "generated_at")
	tempStamp := cacheStamp(t, home, tempRel, "computed_at")
	if usageStamp == old {
		t.Error("capability-usage was not rebuilt, so temperature was derived from the stale input")
	}
	if tempStamp == old {
		t.Error("memory-temperature was not rebuilt")
	}
	usageAt, err := time.Parse(time.RFC3339, usageStamp)
	if err != nil {
		t.Fatalf("usage stamp %q: %v", usageStamp, err)
	}
	tempAt, err := time.Parse(time.RFC3339, tempStamp)
	if err != nil {
		t.Fatalf("temp stamp %q: %v", tempStamp, err)
	}
	if usageAt.After(tempAt) {
		t.Errorf("usage (%s) must not be newer than temperature (%s): temperature must be built from the usage body, not before it", usageStamp, tempStamp)
	}
}

// TestRefreshDerivedCachesSkipsAFreshChain pins that the common case costs nothing.
// This runs on every tool call, so a gate that does not hold turns ~0.8s of rebuild
// into a per-tool-call tax.
func TestRefreshDerivedCachesSkipsAFreshChain(t *testing.T) {
	homePath := t.TempDir()
	codeBuiltAt, err := home.RunningBinaryBuiltAt()
	if err != nil {
		t.Fatal(err)
	}
	// Inject a clock strictly after the running binary at RFC3339 precision.
	// time.Now().Format(RFC3339) can truncate below the binary's nanosecond mtime.
	now := codeBuiltAt.Add(2 * time.Second).UTC().Truncate(time.Second)
	fresh := now.Format(time.RFC3339)
	writeAllCacheBodies(t, homePath, fresh)

	if refs := refreshDerivedCaches(homePath, true, now); len(refs) != 0 {
		t.Errorf("a fresh chain must not rebuild, got %v", refs)
	}
	// And it must not have written an attempt marker either — nothing was attempted.
	if _, err := os.Stat(derivedCacheAttemptPath(homePath)); !os.IsNotExist(err) {
		t.Error("a skipped refresh must not claim an attempt")
	}
	if got := cacheStamp(t, homePath, tempRel, "computed_at"); got != fresh {
		t.Errorf("a fresh body must be left byte-identical, stamp moved to %q", got)
	}
}

// TestRefreshDerivedCachesIsANoOpWithoutAHome keeps the hook fail-open: a homeless
// invocation must not error and must not touch the filesystem.
func TestRefreshDerivedCachesIsANoOpWithoutAHome(t *testing.T) {
	if refs := refreshDerivedCaches("", true, time.Now().UTC()); refs != nil {
		t.Errorf("homeless refresh must be a no-op, got %v", refs)
	}
	if refs := refreshDerivedCaches("   ", true, time.Now().UTC()); refs != nil {
		t.Errorf("blank home must be a no-op, got %v", refs)
	}
}

// TestPostToolUseEventStaysIdenticalWhetherARefreshFired is the regression guard for a
// defect this file's first version shipped.
//
// The rebuilt refs were appended to ev.SideEffects. The event store keys an event on a
// hash of its INPUT and rejects the same id arriving with different content, so the
// first PostToolUse (stale chain, refresh fires, 2 refs) and an identical re-run (marker
// present, refresh refused, 0 refs) produced the same id with different content and the
// re-run failed outright with "event id already exists with different content".
//
// The rule it violated: a hook event's content must be a pure function of its input.
// Cache age is ambient state, not input. So the refresh must leave no trace on the
// event, and its evidence lives in the rebuilt projections' own timestamps instead.
func TestPostToolUseEventStaysIdenticalWhetherARefreshFired(t *testing.T) {
	home := t.TempDir()
	deps := baseDeps()
	deps.HomePath = home

	// Live, because the refresh only runs on a live hook — a fixture replay would skip
	// it and this test would pass without ever exercising the property it guards.
	fx := Fixture{
		Hook: "post-tool-use", Harness: "claude_code", Cwd: "/repo/x", Tool: "Bash",
		WorkID: "work_1177cb550af3", Live: true,
	}

	// First run: no projections exist, so the chain is stale and a refresh fires.
	first, err := Run(fx, deps, fixedNow)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !derivedCacheAttemptClaimed(t, home) {
		t.Fatal("expected the first run to attempt a refresh; otherwise this test cannot detect the defect")
	}

	// Second run, byte-identical input. The refresh is now throttled, so if any part of
	// the refresh reached the event, the content differs and the store rejects it.
	second, err := Run(fx, deps, fixedNow)
	if err != nil {
		t.Fatalf("identical re-run must be idempotent, got: %v", err)
	}

	if first.Event.Header.ID != second.Event.Header.ID {
		t.Errorf("identical input produced different event ids: %s vs %s", first.Event.Header.ID, second.Event.Header.ID)
	}
	if len(first.Event.SideEffects) != len(second.Event.SideEffects) {
		t.Errorf("side effects differ between runs (%v vs %v): the event is not a pure function of its input",
			first.Event.SideEffects, second.Event.SideEffects)
	}
	for _, effect := range first.Event.SideEffects {
		if effect == "compound://projections/capability-usage" || effect == "compound://projections/memory-temperature" {
			t.Errorf("refresh ref %q leaked onto the tool event; cache age is ambient state, not input", effect)
		}
	}
}

func derivedCacheAttemptClaimed(t *testing.T, home string) bool {
	t.Helper()
	_, err := os.Stat(derivedCacheAttemptPath(home))
	return err == nil
}

// TestDerivedCachesRefreshWhenTheyPredateTheRunningBinary pins the criterion that makes
// this agree with `compound doctor`.
//
// Window-only staleness cannot answer doctor's question — "was this body built by the
// code I am about to trust?". Right after installing a fix the bodies are minutes old,
// comfortably inside the 6h window, and are precisely the pre-fix output the fix was
// meant to replace. That is find_b27a3de99682, and it is the case where the rebuild most
// needs to be automatic. Without this criterion doctor reports a gap that the refresh
// structurally cannot close, and an operator watching two guards contradict each other
// learns to trust neither.
func TestDerivedCachesRefreshWhenTheyPredateTheRunningBinary(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	// Well inside the 6h window: window-only staleness would say "fresh".
	stamp := now.Add(-10 * time.Minute)
	writeAllCacheBodies(t, home, stamp.Format(time.RFC3339))

	if derivedCachesNeedRefresh(home, time.Time{}, now) {
		t.Fatal("fixture should be fresh by window alone; the test cannot isolate the binary criterion otherwise")
	}

	// A binary installed AFTER the bodies were built must trigger the rebuild.
	installedAfter := stamp.Add(5 * time.Minute)
	if !derivedCachesNeedRefresh(home, installedAfter, now) {
		t.Error("bodies predating the running binary must refresh: they are the pre-fix output")
	}

	// A binary installed BEFORE them must not: they already reflect this code.
	installedBefore := stamp.Add(-5 * time.Minute)
	if derivedCachesNeedRefresh(home, installedBefore, now) {
		t.Error("bodies newer than the running binary must not refresh; the guard has to be able to go quiet")
	}
}
