package runattempt

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepStaleAttemptWithProbeRecoversOnlyExactID(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	target, err := store.Mint("work_targeted", "actor:test", "harness:test", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := store.Mint("work_unrelated", "actor:test", "harness:test", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	unrelatedPath := filepath.Join(home, "run_attempts", unrelated.ID+".json")
	corrupt := []byte("{not-json\\n")
	if err := os.WriteFile(unrelatedPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := store.SweepStaleAttemptWithProbe(target.ID, time.Minute, now, func(RunAttempt) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatalf("targeted sweep: %v", err)
	}
	if report.Scanned != 1 || report.Swept != 1 || len(report.SweptIDs) != 1 || report.SweptIDs[0] != target.ID {
		t.Fatalf("report = %+v, want only target %s swept", report, target.ID)
	}
	got, err := Open(home).Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAbandoned {
		t.Fatalf("target status = %q, want %q", got.Status, StatusAbandoned)
	}
	after, err := os.ReadFile(unrelatedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("targeted recovery rewrote unrelated corrupt record: got %q want %q", after, corrupt)
	}
}

func TestSweepStaleAttemptWithProbePreservesFreshTerminalAndPreparedRecords(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, store *Store) RunAttempt
	}{
		{
			name: "fresh",
			setup: func(t *testing.T, store *Store) RunAttempt {
				ra, err := store.Mint("work_fresh", "actor:test", "harness:test", now.Add(-time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				return ra
			},
		},
		{
			name: "terminal",
			setup: func(t *testing.T, store *Store) RunAttempt {
				ra, err := store.Mint("work_terminal", "actor:test", "harness:test", now.Add(-2*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Complete(ra.ID, StatusSucceeded, now.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
				return ra
			},
		},
		{
			name: "prepared terminalization",
			setup: func(t *testing.T, store *Store) RunAttempt {
				ra := mintRunAttemptForTerminalizationIntent(t, store, now.Add(-2*time.Hour))
				if _, err := store.PrepareTerminalization(ra.ID, terminalizationIntentForTest(now.Add(-time.Hour))); err != nil {
					t.Fatal(err)
				}
				return ra
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := Open(t.TempDir())
			ra := tc.setup(t, store)
			probed := false
			report, err := store.SweepStaleAttemptWithProbe(ra.ID, 30*time.Minute, now, func(RunAttempt) (bool, error) {
				probed = true
				return false, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if report.Scanned != 1 || report.Swept != 0 || report.Skipped != 1 || probed {
				t.Fatalf("report=%+v probed=%t, want selected record skipped without probe", report, probed)
			}
			got, err := Open(store.Home()).Get(ra.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "terminal" && got.Status != StatusSucceeded {
				t.Fatalf("terminal status = %q, want %q", got.Status, StatusSucceeded)
			}
			if tc.name != "terminal" && got.Status != StatusPending {
				t.Fatalf("status = %q, want pending", got.Status)
			}
			if tc.name == "prepared terminalization" && got.TerminalizationIntent == nil {
				t.Fatal("prepared terminalization intent was removed")
			}
		})
	}
}

func TestSweepStaleAttemptWithProbeLivenessAndSnapshotRace(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		probe func(t *testing.T, store *Store, id string) LivenessProbe
		want  string
	}{
		{
			name: "probe error skips",
			probe: func(_ *testing.T, _ *Store, _ string) LivenessProbe {
				return func(RunAttempt) (bool, error) { return false, errors.New("probe unavailable") }
			},
			want: StatusPending,
		},
		{
			name: "live probe protects",
			probe: func(_ *testing.T, _ *Store, _ string) LivenessProbe {
				return func(RunAttempt) (bool, error) { return true, nil }
			},
			want: StatusPending,
		},
		{
			name: "heartbeat race invalidates stale snapshot",
			probe: func(t *testing.T, store *Store, id string) LivenessProbe {
				return func(RunAttempt) (bool, error) {
					if _, err := store.Heartbeat(id, now); err != nil {
						t.Fatal(err)
					}
					return false, nil
				}
			},
			want: StatusPending,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := Open(t.TempDir())
			ra, err := store.Mint("work_"+tc.name, "actor:test", "harness:test", now.Add(-2*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			report, err := store.SweepStaleAttemptWithProbe(ra.ID, time.Minute, now, tc.probe(t, store, ra.ID))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Open(store.Home()).Get(ra.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.want || report.Swept != 0 || report.Skipped != 1 {
				t.Fatalf("report=%+v status=%q, want skipped pending", report, got.Status)
			}
			if tc.name == "live probe protects" && report.Protected != 1 {
				t.Fatalf("live report = %+v, want protected", report)
			}
		})
	}
}

func TestSweepStaleAttemptWithProbeRefusesMissingInvalidAndNilProbe(t *testing.T) {
	store := Open(t.TempDir())
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if _, err := store.SweepStaleAttemptWithProbe("ratt_20260910T120000Z_deadbeef", time.Minute, now, nil); err == nil {
		t.Fatal("nil probe was accepted")
	}
	if _, err := store.SweepStaleAttemptWithProbe("ratt_20260910T120000Z_deadbeef", time.Minute, now, func(RunAttempt) (bool, error) { return false, nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing exact attempt error = %v, want ErrNotFound", err)
	}
	if _, err := store.SweepStaleAttemptWithProbe("../ratt_bad", time.Minute, now, func(RunAttempt) (bool, error) { return false, nil }); err == nil {
		t.Fatal("path-like id was accepted")
	}
}

func TestSweepStaleAttemptWithProbeClassifiesExactLaunchDisposition(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name           string
		start          func() error
		classification string
		wantAutoClosed int
		wantEscalated  int
	}{
		{
			name:           "committed launch is ambiguous",
			start:          func() error { return nil },
			classification: SweepClassificationAmbiguous,
			wantEscalated:  1,
		},
		{
			name: "exact start error is side effect free",
			start: func() error {
				return errors.New("start failed before execution")
			},
			classification: SweepClassificationSideEffectFree,
			wantAutoClosed: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := Open(t.TempDir())
			ra, err := store.Mint("work_targeted_launch", "actor:test", "harness:test", now.Add(-2*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetOrigin(ra.ID, OriginHarnessExecution); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CommitLaunch(ra.ID, now.Add(-90*time.Minute), tc.start); tc.wantAutoClosed == 0 && err != nil {
				t.Fatalf("CommitLaunch committed target: %v", err)
			}

			report, err := store.SweepStaleAttemptWithProbe(ra.ID, time.Minute, now, func(RunAttempt) (bool, error) {
				return false, nil
			})
			if err != nil {
				t.Fatalf("targeted sweep: %v", err)
			}
			if report.Scanned != 1 || report.Swept != 1 || report.AutoClosed != tc.wantAutoClosed || report.Escalated != tc.wantEscalated {
				t.Fatalf("report = %+v, want scanned=1 swept=1 auto_closed=%d escalated=%d", report, tc.wantAutoClosed, tc.wantEscalated)
			}
			got, err := Open(store.Home()).Get(ra.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != StatusAbandoned || got.SweepClassification != tc.classification {
				t.Fatalf("recovered target = %+v, want abandoned %q", got, tc.classification)
			}
		})
	}
}

func TestSweepStaleAttemptWithProbeSkipsEvidenceRaceAndReturnsCompletionErrors(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	t.Run("evidence attachment invalidates snapshot", func(t *testing.T) {
		store := Open(t.TempDir())
		ra, err := store.Mint("work_targeted_evidence_race", "actor:test", "harness:test", now.Add(-2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		report, err := store.SweepStaleAttemptWithProbe(ra.ID, time.Minute, now, func(RunAttempt) (bool, error) {
			if _, err := store.SetTranscript(ra.ID, "transcript://attached-after-get"); err != nil {
				t.Fatal(err)
			}
			return false, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if report.Scanned != 1 || report.Swept != 0 || report.Skipped != 1 {
			t.Fatalf("report = %+v, want snapshot-race skip", report)
		}
		got, err := Open(store.Home()).Get(ra.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusPending || got.TranscriptRef != "transcript://attached-after-get" {
			t.Fatalf("evidence race lost or terminalized target: %+v", got)
		}
	})

	t.Run("completion write failure is returned", func(t *testing.T) {
		store := Open(t.TempDir())
		ra, err := store.Mint("work_targeted_write_failure", "actor:test", "harness:test", now.Add(-2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		injected := errors.New("injected targeted completion write failure")
		originalWrite := runattemptWriteFile
		runattemptWriteFile = func(path string, candidate RunAttempt) error {
			if candidate.ID == ra.ID && candidate.Status == StatusAbandoned {
				return injected
			}
			return originalWrite(path, candidate)
		}
		t.Cleanup(func() { runattemptWriteFile = originalWrite })

		report, err := store.SweepStaleAttemptWithProbe(ra.ID, time.Minute, now, func(RunAttempt) (bool, error) {
			return false, nil
		})
		if !errors.Is(err, injected) {
			t.Fatalf("targeted completion error = %v, want %v", err, injected)
		}
		if report.Scanned != 1 || report.Swept != 0 || report.Skipped != 1 {
			t.Fatalf("report = %+v, want truthful skipped exact target", report)
		}
		got, getErr := Open(store.Home()).Get(ra.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status != StatusPending {
			t.Fatalf("write-failed target status = %q, want pending", got.Status)
		}
	})

	t.Run("live heartbeat write failure is returned", func(t *testing.T) {
		store := Open(t.TempDir())
		ra, err := store.Mint("work_targeted_heartbeat_failure", "actor:test", "harness:test", now.Add(-2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		injected := errors.New("injected targeted heartbeat write failure")
		originalWrite := runattemptWriteFile
		runattemptWriteFile = func(path string, candidate RunAttempt) error {
			if candidate.ID == ra.ID && candidate.LastHeartbeatAt != nil {
				return injected
			}
			return originalWrite(path, candidate)
		}
		t.Cleanup(func() { runattemptWriteFile = originalWrite })

		report, err := store.SweepStaleAttemptWithProbe(ra.ID, time.Minute, now, func(RunAttempt) (bool, error) {
			return true, nil
		})
		if !errors.Is(err, injected) {
			t.Fatalf("targeted heartbeat error = %v, want %v", err, injected)
		}
		if report.Scanned != 1 || report.Protected != 0 || report.Swept != 0 || report.Skipped != 1 {
			t.Fatalf("report = %+v, want truthful skipped target", report)
		}
		got, getErr := Open(store.Home()).Get(ra.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status != StatusPending || got.LastHeartbeatAt != nil {
			t.Fatalf("heartbeat write-failed target = %+v, want unchanged pending record", got)
		}
	})
}

func TestSweepStaleAttemptWithProbeSkipsBusyAttemptWithoutBlockingIndependentRecovery(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	store := Open(t.TempDir())
	blocked, err := store.Mint("work_targeted_lock_blocked", "actor:test", "harness:test", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(blocked.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	independent, err := store.Mint("work_targeted_lock_independent", "actor:test", "harness:test", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	launchDone := make(chan error, 1)
	go func() {
		_, err := store.CommitLaunch(blocked.ID, now.Add(-90*time.Minute), func() error {
			close(entered)
			<-release
			return nil
		})
		launchDone <- err
	}()
	<-entered
	mainPath := filepath.Join(store.Home(), "run_attempts", blocked.ID+".json")
	preparedPath := launchEvidencePath(store.Home(), blocked.ID, launchEvidencePrepared)
	beforeMain, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	beforePrepared, err := os.ReadFile(preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			close(release)
			if err := <-launchDone; err != nil {
				t.Errorf("release held launch: %v", err)
			}
		}
	}()

	blockedReport, err := store.SweepStaleAttemptWithProbe(blocked.ID, time.Minute, now, func(RunAttempt) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if blockedReport.Scanned != 1 || blockedReport.Swept != 0 || blockedReport.Skipped != 1 {
		t.Fatalf("busy target report = %+v, want nonblocking skip", blockedReport)
	}
	afterMain, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	afterPrepared, err := os.ReadFile(preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeMain, afterMain) || !bytes.Equal(beforePrepared, afterPrepared) {
		t.Fatal("busy exact sweep rewrote selected record or prepared launch evidence")
	}

	independentDone := make(chan struct {
		report SweepReport
		err    error
	}, 1)
	go func() {
		report, err := Open(store.Home()).SweepStaleAttemptWithProbe(independent.ID, time.Minute, now, func(RunAttempt) (bool, error) {
			return false, nil
		})
		independentDone <- struct {
			report SweepReport
			err    error
		}{report, err}
	}()
	select {
	case result := <-independentDone:
		if result.err != nil || result.report.Scanned != 1 || result.report.Swept+result.report.Skipped != 1 {
			t.Fatalf("independent exact recovery while launch guard held = %+v, %v", result.report, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("independent exact recovery blocked behind another attempt lock")
	}

	close(release)
	if err := <-launchDone; err != nil {
		t.Fatalf("release held launch: %v", err)
	}
	released = true
	postRelease, err := Open(store.Home()).SweepStaleAttemptWithProbe(blocked.ID, time.Minute, now, func(RunAttempt) (bool, error) {
		return false, nil
	})
	if err != nil || postRelease.Swept != 1 || postRelease.SweptIDs[0] != blocked.ID {
		t.Fatalf("busy exact recovery after launch release = %+v, %v", postRelease, err)
	}
}
