package sourceobs

import (
	"testing"
	"time"
)

func TestReleaseTruthBuildAndEvidenceIdentityAreLossless(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, change := range []string{"build", "evidence"} {
		t.Run(change, func(t *testing.T) {
			homePath := t.TempDir()
			first := *rawTypedRelease(t, "json", now).Release
			second := first
			if change == "build" {
				second.ObservedBuild = "build-21"
			} else {
				second.EvidenceRef = "https://evidence.invalid/independent"
			}
			ids := map[string]bool{}
			for _, obs := range []ReleaseObservation{first, second} {
				src, err := FromReleaseObservation(obs, now)
				if err != nil {
					t.Fatal(err)
				}
				report, err := Apply(homePath, src, now)
				if err != nil {
					t.Fatal(err)
				}
				ids[report.CandidateIDs[0]] = true
			}
			if len(ids) != 2 {
				t.Errorf("%s-only evidence difference overwrote candidate", change)
			}
			got, err := LoadReleaseObservations(homePath, []string{first.ProductID}, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("%s-only evidence difference collapsed: %+v", change, got)
			}
			found := map[ReleaseObservation]bool{}
			for _, obs := range got {
				found[obs] = true
			}
			if !found[first] || !found[second] {
				t.Fatalf("payload lost: %+v", got)
			}
		})
	}
}

func TestReleaseTruthIdentityMismatchCannotHideBehindProductFilter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	homePath := t.TempDir()
	obs := rawTypedRelease(t, "json", now)
	obs.Release.ProductID = "product:other"
	if _, err := Apply(homePath, obs, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"product:alpha", "product:other", "product:unrelated"} {
		report, err := LoadReleaseObservationsReport(homePath, []string{id}, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Records) != 0 {
			t.Errorf("ambiguous identity became valid for %s: %+v", id, report.Records)
		}
		wantIssues := 1
		if id == "product:unrelated" {
			wantIssues = 0
		}
		if len(report.Issues) != wantIssues {
			t.Errorf("%s issue count=%d want %d", id, len(report.Issues), wantIssues)
		}
		for _, issue := range report.Issues {
			if issue.ProductID != "" {
				t.Errorf("ambiguous identity assigned to %s", issue.ProductID)
			}
		}
	}
}
