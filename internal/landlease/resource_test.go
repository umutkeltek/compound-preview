package landlease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func migratedResource(t *testing.T, home, backing string, key ResourceKey) *Resource {
	t.Helper()
	if err := MigrateLegacyLayout(home, backing, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	resource, err := OpenResource(home, key)
	if err != nil {
		t.Fatal(err)
	}
	return resource
}

func TestScopedResourcesExcludeByKeyAndOverlapAcrossKeys(t *testing.T) {
	home, backing := t.TempDir(), t.TempDir()
	proof := migratedResource(t, home, backing, ProofResourceKey())
	fingerprint := strings.Repeat("a", 64)
	keyA, err := LandingResourceKey(fingerprint, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := LandingResourceKey(fingerprint, "refs/heads/release")
	if err != nil {
		t.Fatal(err)
	}
	landingA, _ := OpenResource(home, keyA)
	landingB, _ := OpenResource(home, keyB)

	for _, resource := range []*Resource{proof, landingA, landingB} {
		if _, err := resource.Acquire(laneAlpha, time.Minute, baseNow); err != nil {
			t.Fatalf("independent acquire %v: %v", resource.key, err)
		}
		if _, err := resource.Acquire(laneBeta, time.Minute, baseNow); err == nil {
			t.Fatalf("same-key competing acquire succeeded for %v", resource.key)
		} else {
			var held *HeldError
			if !errors.As(err, &held) {
				t.Fatalf("same-key error = %v", err)
			}
		}
	}
	for _, resource := range []*Resource{proof, landingA, landingB} {
		status, err := resource.StatusReadOnly(baseNow)
		if err != nil || !status.Held || status.Holder != laneAlpha {
			t.Fatalf("status %v = %+v, %v", resource.key, status, err)
		}
	}
}

func TestProofExcludesAcrossHomesWithSharedBackingRoot(t *testing.T) {
	backing := t.TempDir()
	first := migratedResource(t, t.TempDir(), backing, ProofResourceKey())
	second := migratedResource(t, t.TempDir(), backing, ProofResourceKey())
	if _, err := first.Acquire(laneAlpha, time.Minute, baseNow); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Acquire(laneBeta, time.Minute, baseNow); err == nil {
		t.Fatal("separate homes sharing backing root acquired proof simultaneously")
	}
	if _, err := first.Release(laneAlpha, baseNow); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Acquire(laneBeta, time.Minute, baseNow); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationCutoverRefusesOldBinariesAndIsOneWay(t *testing.T) {
	home, backing := t.TempDir(), t.TempDir()
	if err := MigrateLegacyLayout(home, backing, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(home, stateRelPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readLeaseFromBytesForMarkerTest(marker); err == nil {
		t.Fatal("old lease decoder accepted cutover marker")
	}
	if _, err := Status(home, baseNow); err == nil {
		t.Fatal("old status accepted marker")
	}
	if _, err := Acquire(home, laneAlpha, time.Minute, baseNow); err == nil {
		t.Fatal("old acquire accepted marker")
	}
	if _, err := Steal(home, laneAlpha, "test takeover", baseNow); err == nil {
		t.Fatal("old steal accepted marker")
	}
	if err := MigrateLegacyLayout(home, backing, func() error { return nil }); err == nil {
		t.Fatal("migration rewrote a published marker")
	}
	got, err := os.ReadFile(filepath.Join(home, stateRelPath))
	if err != nil || string(got) != string(marker) {
		t.Fatalf("marker changed: %v", err)
	}
}

func TestMigrationRefusalLeavesLegacyStateAndNoMarker(t *testing.T) {
	t.Run("owner validator", func(t *testing.T) {
		home, backing := t.TempDir(), t.TempDir()
		sentinel := errors.New("pending proof")
		if err := MigrateLegacyLayout(home, backing, func() error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("migration error = %v", err)
		}
		status, err := LayoutStatus(home)
		if err != nil || status.Mode != "legacy" {
			t.Fatalf("layout = %+v, %v", status, err)
		}
		if _, err := Acquire(home, laneAlpha, time.Minute, baseNow); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("legacy lease", func(t *testing.T) {
		home, backing := t.TempDir(), t.TempDir()
		if _, err := Acquire(home, laneAlpha, time.Second, baseNow); err != nil {
			t.Fatal(err)
		}
		if err := MigrateLegacyLayout(home, backing, func() error { return nil }); err == nil {
			t.Fatal("migrated an expired but unreleased lease")
		}
	})
	t.Run("live barrier", func(t *testing.T) {
		home, backing := t.TempDir(), t.TempDir()
		barrier, err := lockExecutionBarrier(home, "proof:live")
		if err != nil {
			t.Fatal(err)
		}
		defer barrier.Close()
		if err := MigrateLegacyLayout(home, backing, func() error { return nil }); err == nil {
			t.Fatal("migrated with a live legacy descendant")
		}
	})
}

func TestLayoutStatusReadOnlyAndMalformedMarkerFailsClosed(t *testing.T) {
	home, backing := t.TempDir(), t.TempDir()
	status, err := LayoutStatus(home)
	if err != nil || status.Mode != "legacy" {
		t.Fatalf("initial = %+v, %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(home, lockRelPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created lock: %v", err)
	}
	resource := ProofResource(home)
	if _, err := resource.StatusReadOnly(baseNow); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, lockRelPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resource status created lock: %v", err)
	}
	if err := MigrateLegacyLayout(home, backing, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	status, err = LayoutStatus(home)
	canonical, canonicalErr := filepath.EvalSymlinks(backing)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	if err != nil || status.Mode != "scoped" || status.BackingRoot != canonical {
		t.Fatalf("scoped status = %+v, %v", status, err)
	}
	if _, err := resource.StatusReadOnly(baseNow); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(scopedRoot(canonical, ProofResourceKey()), lockRelPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scoped status created lock: %v", err)
	}
	bad := []byte("{\"schema_version\":\"" + layoutSchema + "\",\"holder\":{\"layout\":\"scoped\",\"backing_root\":\"relative\"}}")
	if err := os.WriteFile(filepath.Join(home, stateRelPath), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LayoutStatus(home); err == nil {
		t.Fatal("accepted malformed marker")
	}
	if _, err := resource.Acquire(laneAlpha, time.Minute, baseNow); err == nil {
		t.Fatal("acquired through malformed marker")
	}
}

func TestLandingResourceKeyRejectsInvalidRefs(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	for _, ref := range []string{
		"main", "refs/heads/", "refs/heads/.hidden", "refs/heads/a/.hidden",
		"refs/heads/topic.lock", "refs/heads/topic.lock/sub",
		"refs/heads/a//b", "refs/heads/a/../b", "refs/heads/a\nb",
	} {
		if _, err := LandingResourceKey(fingerprint, ref); err == nil {
			t.Errorf("accepted invalid ref %q", ref)
		}
	}
}

func TestMigrationBackingFailureDoesNotPublishMarker(t *testing.T) {
	home, parent := t.TempDir(), t.TempDir()
	backing := filepath.Join(parent, "blocked")
	if err := os.WriteFile(backing, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacyLayout(home, backing, func() error { return nil }); err == nil {
		t.Fatal("migration succeeded with invalid backing root")
	}
	status, err := LayoutStatus(home)
	if err != nil || status.Mode != "legacy" {
		t.Fatalf("layout = %+v, %v", status, err)
	}
	if _, err := Acquire(home, laneAlpha, time.Minute, baseNow); err != nil {
		t.Fatalf("old lease unavailable after failed publication: %v", err)
	}
}

func TestScopedFinalizerDoesNotBlockOtherResource(t *testing.T) {
	home, backing := t.TempDir(), t.TempDir()
	proof := migratedResource(t, home, backing, ProofResourceKey())
	landingKey, err := LandingResourceKey(strings.Repeat("b", 64), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	landing, err := OpenResource(home, landingKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proof.Acquire(laneAlpha, time.Minute, baseNow); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	unblock := make(chan struct{})
	released := make(chan error, 1)
	go func() {
		_, err := proof.ReleaseAfter(laneAlpha, baseNow, func() error {
			close(entered)
			<-unblock
			return nil
		})
		released <- err
	}()
	<-entered
	acquired := make(chan error, 1)
	go func() {
		_, err := landing.Acquire(laneBeta, time.Minute, baseNow)
		acquired <- err
	}()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("unrelated landing acquire: %v", err)
		}
	case <-time.After(3 * time.Second):
		close(unblock)
		t.Fatal("unrelated landing resource blocked behind proof finalizer")
	}
	close(unblock)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}

func TestResourceUsesSharedLeaseBeforeMigration(t *testing.T) {
	home := t.TempDir()
	proof := ProofResource(home)
	key, err := LandingResourceKey(strings.Repeat("c", 64), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	landing, err := OpenResource(home, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proof.Acquire(laneAlpha, time.Minute, baseNow); err != nil {
		t.Fatal(err)
	}
	if _, err := landing.Acquire(laneBeta, time.Minute, baseNow); err == nil {
		t.Fatal("landing bypassed legacy shared lease before migration")
	}
	status, err := landing.StatusReadOnly(baseNow)
	if err != nil || !status.Held || status.Holder != laneAlpha {
		t.Fatalf("legacy landing view = %+v, %v", status, err)
	}
}

func TestProofResourceDefersInvalidHomeError(t *testing.T) {
	resource := ProofResource("")
	if resource == nil {
		t.Fatal("nil proof resource")
	}
	if _, err := resource.StatusReadOnly(baseNow); err == nil {
		t.Fatal("invalid home did not fail on use")
	}
}

func TestScopedStealExpectedRejectsStalePredecessor(t *testing.T) {
	home, backing := t.TempDir(), t.TempDir()
	key, err := LandingResourceKey(strings.Repeat("d", 64), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	resource := migratedResource(t, home, backing, key)
	if _, err := resource.Acquire(laneBeta, time.Minute, baseNow); err != nil {
		t.Fatal(err)
	}

	replaced, err := resource.StealExpected(laneGamma, laneBeta, "first approved takeover", baseNow)
	if err != nil {
		t.Fatalf("exact predecessor steal: %v", err)
	}
	if replaced.Holder != laneGamma || replaced.PriorHolder != laneBeta {
		t.Fatalf("unexpected audited transfer: %+v", replaced)
	}
	before, err := resource.StatusReadOnly(baseNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resource.StealExpected(laneAlpha, laneBeta, "stale approved takeover", baseNow); err == nil {
		t.Fatal("stale approval stole from replacement holder")
	}
	after, err := resource.StatusReadOnly(baseNow)
	if err != nil {
		t.Fatal(err)
	}
	if after != before || after.Holder != laneGamma || !after.Held {
		t.Fatalf("stale steal changed replacement lease: before=%+v after=%+v", before, after)
	}
}

func TestScopedStealObservedRejectsSameHolderReacquisition(t *testing.T) {
	home, backing := t.TempDir(), t.TempDir()
	key, err := LandingResourceKey(strings.Repeat("e", 64), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	resource := migratedResource(t, home, backing, key)
	if _, err := resource.Acquire(laneBeta, time.Minute, baseNow); err != nil {
		t.Fatal(err)
	}
	stale, err := resource.StatusReadOnly(baseNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Release(laneBeta, baseNow); err != nil {
		t.Fatal(err)
	}
	reacquiredAt := baseNow.Add(2 * time.Second)
	if _, err := resource.Acquire(laneBeta, time.Minute, reacquiredAt); err != nil {
		t.Fatal(err)
	}
	current, err := resource.StatusReadOnly(reacquiredAt)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Holder != current.Holder || stale.AcquiredAt == current.AcquiredAt {
		t.Fatalf("test needs same holder in a new lease generation: stale=%+v current=%+v", stale, current)
	}
	if _, err := resource.StealObserved(laneGamma, stale, "stale approval", reacquiredAt); err == nil {
		t.Fatal("stale snapshot stole a same-holder replacement lease")
	}
	after, err := resource.StatusReadOnly(reacquiredAt)
	if err != nil {
		t.Fatal(err)
	}
	if after != current || !after.Held {
		t.Fatalf("stale steal changed replacement lease: before=%+v after=%+v", current, after)
	}
	// Clock-derived fields are not lease identity and need not match.
	current.Held = false
	current.RemainingSeconds = -1
	current.IsExpired = true
	stolen, err := resource.StealObserved(laneGamma, current, "fresh approval", reacquiredAt)
	if err != nil {
		t.Fatalf("fresh durable snapshot refused: %v", err)
	}
	if stolen.Holder != laneGamma || stolen.PriorHolder != laneBeta {
		t.Fatalf("unexpected audited transfer: %+v", stolen)
	}
}

func TestScopedStealObservedRejectsSameSecondReacquisition(t *testing.T) {
	home, backing := t.TempDir(), t.TempDir()
	key, err := LandingResourceKey(strings.Repeat("f", 64), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	resource := migratedResource(t, home, backing, key)
	if _, err := resource.Acquire(laneBeta, time.Minute, baseNow); err != nil {
		t.Fatal(err)
	}
	stale, err := resource.StatusReadOnly(baseNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resource.Release(laneBeta, baseNow); err != nil {
		t.Fatal(err)
	}
	reacquiredAt := baseNow.Add(time.Nanosecond)
	if _, err := resource.Acquire(laneBeta, time.Minute, reacquiredAt); err != nil {
		t.Fatal(err)
	}
	current, err := resource.StatusReadOnly(reacquiredAt)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Holder != current.Holder || stale.AcquiredAt == current.AcquiredAt || stale.ExpiresAt == current.ExpiresAt {
		t.Fatalf("nanosecond reacquisition must have distinct durable timestamps: stale=%+v current=%+v", stale, current)
	}
	if _, err := resource.StealObserved(laneGamma, stale, "stale same-second approval", reacquiredAt); err == nil {
		t.Fatal("same-second stale snapshot stole replacement lease")
	}
	after, err := resource.StatusReadOnly(reacquiredAt)
	if err != nil {
		t.Fatal(err)
	}
	if after != current || !after.Held {
		t.Fatalf("stale steal changed same-second replacement: before=%+v after=%+v", current, after)
	}
}
