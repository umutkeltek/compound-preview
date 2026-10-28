package main

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/work"
)

// Without --wait the second battery is refused while the first holds the proof
// resource (TestProofCheckAdmissionRejectsSecondBeforeRunnerStarts). With it, the
// second neither starts nor fails while the first runs, then runs once it frees.
func TestProofCheckAdmissionWaitQueuesBehindALiveBattery(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "proof-admission-test", now); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	first := createClaimedProofWork(t, store, "first governed proof", "codex:alpha", now)
	second := createClaimedProofWork(t, store, "second governed proof", "codex:beta", now)

	started := make(chan struct{})
	finish := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, _, runErr := runProofCheckWithAdmission(proofCheckAdmissionInput{
			HomePath:  homePath,
			WorkID:    first.ID,
			Actor:     "codex:alpha",
			Workspace: "/synthetic/repo-alpha",
			Command:   "compound proof-check --out /synthetic/report-alpha.json",
			Profile:   syntheticAdmissionProfile(t, "/synthetic/repo-alpha"),
			TTL:       time.Minute,
			Now:       time.Now,
		}, func() (proofCheckReport, error) {
			close(started)
			<-finish
			return realTimeProofCheckReport("/synthetic/repo-alpha"), nil
		})
		firstDone <- runErr
	}()
	<-started

	var progress bytes.Buffer
	var secondRunnerStarts atomic.Int32
	secondDone := make(chan error, 1)
	go func() {
		_, _, runErr := runProofCheckWithAdmission(proofCheckAdmissionInput{
			HomePath:     homePath,
			WorkID:       second.ID,
			Actor:        "codex:beta",
			Workspace:    "/synthetic/repo-beta",
			Command:      "compound proof-check --wait 30s --out /synthetic/report-beta.json",
			Profile:      syntheticAdmissionProfile(t, "/synthetic/repo-beta"),
			TTL:          time.Minute,
			Wait:         30 * time.Second,
			WaitProgress: &progress,
			Now:          time.Now,
		}, func() (proofCheckReport, error) {
			secondRunnerStarts.Add(1)
			return realTimeProofCheckReport("/synthetic/repo-beta"), nil
		})
		secondDone <- runErr
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("queued battery returned while the first held the resource: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if secondRunnerStarts.Load() != 0 {
		t.Fatal("queued battery started while the first held the resource")
	}

	close(finish)
	if err := <-firstDone; err != nil {
		t.Fatalf("first governed proof: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("queued governed proof: %v", err)
	}
	if secondRunnerStarts.Load() != 1 {
		t.Fatalf("queued runner started %d times, want 1", secondRunnerStarts.Load())
	}
	if !strings.Contains(progress.String(), "waiting for proof:") {
		t.Fatalf("progress %q does not name the battery it waited on", progress.String())
	}
}

// realTimeProofCheckReport stamps the shared fixture with the real clock that
// --wait runs on, so the report falls inside its reservation window.
func realTimeProofCheckReport(repo string) proofCheckReport {
	report := successfulProofCheckReport()
	report.GeneratedAt = time.Now().UTC().Format(time.RFC3339Nano)
	report.Repo = repo
	return report
}
