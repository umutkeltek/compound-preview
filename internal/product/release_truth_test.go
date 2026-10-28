package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/sourceobs"
)

func truthRelease(now time.Time, channel, version, build, state string, age time.Duration) sourceobs.ReleaseObservation {
	return sourceobs.ReleaseObservation{
		ProductID: "product:truth", Channel: channel, Platform: "desktop",
		ObservedVersion: version, ObservedBuild: build, ObservedState: state,
		ObservedAt: now.Add(-age).Format(time.RFC3339), SourceRevision: "fixture-revision",
		EvidenceRef: "https://releases.invalid/truth/" + channel + "/" + version,
	}
}

func storeTruthRelease(t *testing.T, homePath string, now time.Time, obs sourceobs.ReleaseObservation) (string, string) {
	t.Helper()
	src, err := sourceobs.FromReleaseObservation(obs, now)
	if err != nil {
		t.Fatal(err)
	}
	report, err := sourceobs.Apply(homePath, src, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CandidateIDs) != 1 {
		t.Fatalf("intake: %+v", report)
	}
	return report.CandidateIDs[0], src.ID
}

func TestReleaseTruthCurrentVersionRequiresLocalLiveProof(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name         string
		observations []sourceobs.ReleaseObservation
		status       string
		current      map[string]string
		outdated     bool
	}{
		{"candidate", []sourceobs.ReleaseObservation{truthRelease(now, "store", "2.0", "20", "candidate", time.Hour)}, "partial", nil, false},
		{"submitted", []sourceobs.ReleaseObservation{truthRelease(now, "store", "2.0", "20", "submitted", time.Hour)}, "partial", nil, false},
		{"stale live", []sourceobs.ReleaseObservation{truthRelease(now, "direct", "2.0", "20", "live", 8*24*time.Hour)}, "stale", nil, false},
		{"fresh candidate cannot refresh another channel", []sourceobs.ReleaseObservation{
			truthRelease(now, "direct", "2.0", "20", "live", 8*24*time.Hour),
			truthRelease(now, "store", "3.0", "30", "candidate", time.Hour),
		}, "partial", nil, false},
		{"newer candidate cannot hide live mismatch", []sourceobs.ReleaseObservation{
			truthRelease(now, "direct", "2.0", "20", "live", 2*time.Hour),
			truthRelease(now, "store", "3.0", "30", "candidate", time.Hour),
		}, "current", map[string]string{"direct": "2.0"}, true},
		{"sequential releases are history not conflict", []sourceobs.ReleaseObservation{
			truthRelease(now, "direct", "1.0", "10", "live", 2*time.Hour),
			truthRelease(now, "direct", "2.0", "20", "live", time.Hour),
		}, "current", map[string]string{"direct": "2.0"}, true},
		{"simultaneous versions conflict", []sourceobs.ReleaseObservation{
			truthRelease(now, "direct", "1.0", "10", "live", time.Hour),
			truthRelease(now, "direct", "2.0", "20", "live", time.Hour),
		}, "conflicting", nil, false},
		{"simultaneous builds conflict", []sourceobs.ReleaseObservation{
			truthRelease(now, "direct", "2.0", "20", "live", time.Hour),
			truthRelease(now, "direct", "2.0", "21", "live", time.Hour),
		}, "conflicting", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			for _, obs := range tt.observations {
				storeTruthRelease(t, homePath, now, obs)
			}
			got, err := BuildReleaseAwareness(homePath, "product:truth", ReleaseIdentity{Version: "1.0"}, ReleaseAwarenessOptions{Now: now, StaleAfter: 7 * 24 * time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.status {
				t.Errorf("status=%s want %s: %+v", got.Status, tt.status, got)
			}
			if got.OutdatedContract != tt.outdated {
				t.Errorf("outdated=%t want %t", got.OutdatedContract, tt.outdated)
			}
			for _, view := range got.ChannelPlatforms {
				if view.CurrentVersion != tt.current[view.Channel] {
					t.Errorf("%s current_version=%q want %q", view.Channel, view.CurrentVersion, tt.current[view.Channel])
				}
			}
		})
	}
}

func TestReleaseTruthPreservesNativeProvenance(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	homePath := t.TempDir()
	obs := truthRelease(now, "direct", "2.0", "20", "live", time.Hour)
	candidateID, observationID := storeTruthRelease(t, homePath, now, obs)
	got, err := BuildReleaseAwareness(homePath, obs.ProductID, ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	refs := strings.Join(got.EvidenceRefs, "\n")
	if !strings.Contains(refs, candidateID) || !strings.Contains(refs, observationID) {
		t.Fatalf("canonical evidence references lost: %v", got.EvidenceRefs)
	}
	if len(got.ChannelPlatforms) != 1 || !strings.Contains(strings.Join(got.ChannelPlatforms[0].ObservationIDs, "\n"), observationID) {
		t.Fatalf("source observation identity replaced: %+v", got.ChannelPlatforms)
	}
}

func TestReleaseTruthInvalidEvidenceIsVisible(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	homePath := t.TempDir()
	src, err := sourceobs.FromReleaseObservation(truthRelease(now, "direct", "2.0", "20", "live", time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	// Shape-valid future evidence can exist in historical intake. It must be
	// reported as invalid, not disappear into an unobserved/current summary.
	src.Release.ObservedAt = now.Add(time.Hour).Format(time.RFC3339)
	report, err := sourceobs.Apply(homePath, src, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BuildReleaseAwareness(homePath, "product:truth", ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "invalid" || len(got.MissingEvidence) == 0 {
		t.Fatalf("invalid evidence silently hidden: %+v", got)
	}
	if !strings.Contains(strings.Join(got.MissingEvidence, "\n"), report.CandidateIDs[0]) {
		t.Fatalf("invalid evidence not actionable: %v", got.MissingEvidence)
	}
}

func TestReleaseTruthCorruptInventoryIsNotSuccessfulEmptyRead(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	homePath := t.TempDir()
	candidateID, _ := storeTruthRelease(t, homePath, now, truthRelease(now, "direct", "2.0", "20", "live", time.Hour))
	// Deliberately corrupt persisted bytes; the intake door cannot create this.
	spec, ok := candidates.SpecFor(candidates.RouteObservation)
	if !ok {
		t.Fatal("missing route observation owner")
	}
	path := filepath.Join(homePath, spec.InboxDir, candidateID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildReleaseAwareness(homePath, "product:truth", ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now}); err == nil {
		t.Fatal("corrupt source inventory reported as successful empty read")
	}
}

func TestReleaseTruthLiveBuildMismatch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	homePath := t.TempDir()
	storeTruthRelease(t, homePath, now, truthRelease(now, "direct", "2.0", "20", "live", time.Hour))
	got, err := BuildReleaseAwareness(homePath, "product:truth", ReleaseIdentity{Version: "2.0", Build: "10"}, ReleaseAwarenessOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "current" || !got.OutdatedContract {
		t.Fatalf("live build drift hidden: %+v", got)
	}
}

func TestReleaseTruthAmbiguousIdentityDoesNotPoisonOtherProduct(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	homePath := t.TempDir()
	for _, id := range []string{"product:truth", "product:unrelated"} {
		obs := truthRelease(now, "direct", "2.0", "20", "live", time.Hour)
		obs.ProductID = id
		storeTruthRelease(t, homePath, now, obs)
	}
	src, err := sourceobs.FromReleaseObservation(truthRelease(now, "direct", "3.0", "30", "live", 30*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	src.ID += "_ambiguous"
	src.Release.ProductID = "product:other"
	if _, err := sourceobs.Apply(homePath, src, now); err != nil {
		t.Fatal(err)
	}
	report, err := sourceobs.LoadReleaseObservationsReport(homePath, []string{"product:truth", "product:unrelated"}, now)
	if err != nil {
		t.Fatal(err)
	}
	byProduct := releaseReportsByProduct(report, []string{"product:truth", "product:unrelated"})
	for id, want := range map[string]string{"product:truth": "invalid", "product:unrelated": "current"} {
		got := buildReleaseAwareness(id, ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now}, byProduct[id])
		if got.Status != want {
			t.Errorf("%s status=%s want %s", id, got.Status, want)
		}
	}
}
