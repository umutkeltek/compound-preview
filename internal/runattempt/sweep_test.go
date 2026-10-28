// C3 tests: crash terminality — List, ListNonTerminal, SweepStale.
// C5 tests: fsync durability (write() actually syncs before rename).
//
// Falsifiers:
//   - a pending attempt with no heartbeat older than threshold is swept to abandoned
//   - a fresh heartbeat keeps the attempt alive (skipped)
//   - terminal attempts are untouched by sweep
//   - sweep records a reason on the swept attempt
//   - empty dir is not an error
//   - write() produces a durable file (fsync path exercised)
//   - SweepStaleWithProbe continues past a heartbeat-write failure on one
//     candidate so a later candidate is still processed (mirrors the
//     per-attempt best-effort contract of SweepStale).

package runattempt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListFailsClosedOnMalformedRunAttempt(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC)
	if _, err := store.Mint("work_list_malformed", "codex", HarnessPlaceholder, now); err != nil {
		t.Fatalf("mint valid RunAttempt: %v", err)
	}
	malformedID := MintDeterministic("list-malformed", now.Add(time.Second))
	if err := os.WriteFile(filepath.Join(home, "run_attempts", malformedID+".json"), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatalf("write malformed RunAttempt: %v", err)
	}
	if _, err := Open(home).List(); err == nil || !strings.Contains(err.Error(), malformedID) {
		t.Fatalf("List malformed error = %v, want id-bearing refusal", err)
	}
	if _, err := Open(home).ListNonTerminal(); err == nil || !strings.Contains(err.Error(), malformedID) {
		t.Fatalf("ListNonTerminal malformed error = %v, want id-bearing refusal", err)
	}
	if _, err := Open(home).ListNonTerminalByStatus(); err == nil || !strings.Contains(err.Error(), malformedID) {
		t.Fatalf("ListNonTerminalByStatus malformed error = %v, want id-bearing refusal", err)
	}
}

func TestListFailsClosedOnContradictoryEmbeddedIdentity(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	now := time.Now().UTC()
	original, err := store.Mint("work_census_original", "codex", HarnessPlaceholder, now)
	if err != nil {
		t.Fatal(err)
	}
	contradictory := original
	contradictory.ID = MintDeterministic("census-foreign-identity", now.Add(time.Second))
	contradictory.WorkID = "work_census_foreign"
	body, err := json.Marshal(contradictory)
	if err != nil {
		t.Fatal(err)
	}
	// This corruption cannot pass Mint; plant it to falsify the read boundary.
	if err := os.WriteFile(filepath.Join(home, "run_attempts", original.ID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home).List(); err == nil || !strings.Contains(err.Error(), original.ID) {
		t.Fatalf("census silently substituted a different identity: %v", err)
	}
}

func TestListFailsClosedOnNamedAttemptDirectory(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	ra, err := store.Mint("work_directory_census", "codex", HarnessPlaceholder, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "run_attempts", ra.ID+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil || !strings.Contains(err.Error(), ra.ID) {
		t.Fatalf("named record directory escaped census: %v", err)
	}
}

func TestListFailsClosedOnMissingWorkIdentity(t *testing.T) {
	home := t.TempDir()
	ra, err := Open(home).Mint("work_missing_membership", "codex", HarnessPlaceholder, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ra.WorkID = ""
	body, err := json.Marshal(ra)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "run_attempts", ra.ID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home).List(); err == nil || !strings.Contains(err.Error(), ra.ID) {
		t.Fatalf("missing work membership escaped census: %v", err)
	}
}

func TestListReturnsAllSortedByCreatedAt(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	// Mint three attempts at different times.
	ra1, err := s.Mint("work_a", "actor1", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	ra2, err := s.Mint("work_b", "actor2", "fake", t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ra3, err := s.Mint("work_c", "actor3", "fake", t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Terminalize one so we have a mix.
	if _, err := s.Complete(ra2.ID, StatusSucceeded, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List returned %d, want 3", len(all))
	}
	// Sorted by CreatedAt ascending.
	if all[0].ID != ra1.ID || all[1].ID != ra2.ID || all[2].ID != ra3.ID {
		t.Fatalf("List order wrong: %s, %s, %s", all[0].ID, all[1].ID, all[2].ID)
	}
}

func TestListNonTerminalFiltersTerminal(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra1, _ := s.Mint("work_a", "actor", "fake", t0)
	ra2, _ := s.Mint("work_b", "actor", "fake", t0.Add(time.Minute))
	ra3, _ := s.Mint("work_c", "actor", "fake", t0.Add(2*time.Minute))

	// Terminalize two with different terminal statuses.
	s.Complete(ra1.ID, StatusSucceeded, t0.Add(3*time.Minute))
	s.Complete(ra3.ID, StatusAbandoned, t0.Add(3*time.Minute))

	stuck, err := s.ListNonTerminal()
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 1 {
		t.Fatalf("ListNonTerminal returned %d, want 1 (only ra2)", len(stuck))
	}
	if stuck[0].ID != ra2.ID {
		t.Fatalf("ListNonTerminal returned %s, want %s", stuck[0].ID, ra2.ID)
	}
}

func TestListEmptyDirNoError(t *testing.T) {
	s := Open(t.TempDir())
	all, err := s.List()
	if err != nil {
		t.Fatalf("List on fresh home: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("List on fresh home returned %d, want 0", len(all))
	}
	stuck, err := s.ListNonTerminal()
	if err != nil {
		t.Fatalf("ListNonTerminal on fresh home: %v", err)
	}
	if len(stuck) != 0 {
		t.Fatalf("ListNonTerminal on fresh home returned %d, want 0", len(stuck))
	}
}

func TestListNonTerminalSurfacesUnreadableLaunchEvidence(t *testing.T) {
	lists := map[string]func(*Store) ([]RunAttempt, error){
		"ListNonTerminal":         (*Store).ListNonTerminal,
		"ListNonTerminalByStatus": (*Store).ListNonTerminalByStatus,
	}
	for name, list := range lists {
		for _, corruption := range []string{"malformed-json", "unsafe-mode"} {
			t.Run(name+"/"+corruption, func(t *testing.T) {
				home := t.TempDir()
				attempt, _ := seedUnreadableLaunchAttempt(t, home, corruption)

				got, err := list(Open(home))
				if err == nil {
					t.Fatalf("%s silently omitted %s attempt: got %+v", name, corruption, got)
				}
				if !strings.Contains(err.Error(), attempt.ID) {
					t.Fatalf("%s error = %q, want unreadable attempt id %s", name, err, attempt.ID)
				}
				if len(got) != 0 {
					t.Fatalf("%s returned unreadable attempt material: %+v", name, got)
				}
			})
		}
	}
}

func TestListNonTerminalByStatusMatchesListNonTerminal(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	var ids []string
	for i, status := range []string{StatusSucceeded, "", StatusAbandoned, "", StatusFailed} {
		ra, err := s.Mint("work_bystatus", "actor", "fake", t0.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ra.ID)
		if status != "" {
			if _, err := s.Complete(ra.ID, status, t0.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}

	want, err := Open(home).ListNonTerminal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(home).ListNonTerminalByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(want) != 2 {
		t.Fatalf("live counts: ByStatus=%d ListNonTerminal=%d, want 2 each", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("ByStatus[%d] = %s, want %s (same set, same order)", i, got[i].ID, want[i].ID)
		}
	}
	if got[0].ID != ids[1] || got[1].ID != ids[3] {
		t.Fatalf("ByStatus = [%s %s], want the two pending attempts [%s %s]", got[0].ID, got[1].ID, ids[1], ids[3])
	}
}

// The point of ByStatus is to never touch a terminal record's launch sidecars.
// A terminal attempt with unreadable evidence proves it: ListNonTerminal still
// walks it and refuses, ByStatus skips it without reading the sidecar at all.
func TestListNonTerminalByStatusSkipsTerminalLaunchEvidence(t *testing.T) {
	for _, corruption := range []string{"malformed-json", "unsafe-mode"} {
		t.Run(corruption, func(t *testing.T) {
			home := t.TempDir()
			store := Open(home)
			created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
			done, err := store.Mint("work_terminal_launch_"+corruption, "actor:test", "harness:test", created)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CommitLaunch(done.ID, created.Add(time.Minute), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Complete(done.ID, StatusSucceeded, created.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			corruptPreparedLaunchEvidence(t, home, done.ID, corruption)
			live, err := store.Mint("work_live_next_to_terminal", "actor:test", "harness:test", created.Add(3*time.Minute))
			if err != nil {
				t.Fatal(err)
			}

			if _, err := Open(home).ListNonTerminal(); err == nil || !strings.Contains(err.Error(), done.ID) {
				t.Fatalf("fixture not unreadable: ListNonTerminal error = %v, want refusal naming %s", err, done.ID)
			}
			got, err := Open(home).ListNonTerminalByStatus()
			if err != nil {
				t.Fatalf("ListNonTerminalByStatus read terminal %s launch evidence: %v", corruption, err)
			}
			if len(got) != 1 || got[0].ID != live.ID {
				t.Fatalf("ListNonTerminalByStatus = %+v, want only live attempt %s", got, live.ID)
			}
		})
	}
}

func TestUnreadableRunAttemptsErrorDoesNotRenderPrivateCauses(t *testing.T) {
	const privateCause = "private local launch-evidence path detail"
	err := (&UnreadableRunAttemptsError{Issues: []RunAttemptReadIssue{
		{ID: "ratt_20260814T080000Z_abcdef12", Cause: errors.New(privateCause)},
	}}).Error()
	if strings.Contains(err, privateCause) {
		t.Fatalf("public unreadable error rendered private cause: %q", err)
	}
	if !strings.Contains(err, "ratt_20260814T080000Z_abcdef12") {
		t.Fatalf("public unreadable error omitted safe attempt identity: %q", err)
	}
}

func TestSweepStaleReportsUnreadableLaunchEvidence(t *testing.T) {
	for _, corruption := range []string{"malformed-json", "unsafe-mode"} {
		t.Run(corruption, func(t *testing.T) {
			home := t.TempDir()
			attempt, launchAt := seedUnreadableLaunchAttempt(t, home, corruption)

			report, err := Open(home).SweepStale(time.Minute, launchAt.Add(time.Hour))
			if err == nil {
				t.Fatalf("SweepStale silently accepted %s launch evidence: %+v", corruption, report)
			}
			if report.Scanned != 1 || report.Skipped != 1 || report.Swept != 0 {
				t.Fatalf("SweepStale report = %+v, want scanned=1 skipped=1 swept=0", report)
			}

			body, marshalErr := json.Marshal(report)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var raw struct {
				Unreadable    int      `json:"unreadable"`
				UnreadableIDs []string `json:"unreadable_ids"`
			}
			if unmarshalErr := json.Unmarshal(body, &raw); unmarshalErr != nil {
				t.Fatal(unmarshalErr)
			}
			if raw.Unreadable != 1 || len(raw.UnreadableIDs) != 1 || raw.UnreadableIDs[0] != attempt.ID {
				t.Fatalf("SweepStale unreadable evidence = %+v, want count=1 id=%s", raw, attempt.ID)
			}
		})
	}
}

func TestSweepStaleContinuesPastUnreadableLaunchEvidence(t *testing.T) {
	home := t.TempDir()
	unreadable, launchAt := seedUnreadableLaunchAttempt(t, home, "unsafe-mode")
	store := Open(home)
	readable, err := store.Mint("work_readable_after_unreadable", "actor:test", "harness:test", launchAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(readable.ID, OriginClaimReservation); err != nil {
		t.Fatal(err)
	}

	report, err := Open(home).SweepStale(time.Minute, launchAt.Add(time.Hour))
	if err == nil {
		t.Fatalf("SweepStale omitted unreadable evidence from its error: %+v", report)
	}
	if report.Scanned != 2 || report.Skipped != 1 || report.Swept != 1 {
		t.Fatalf("SweepStale report = %+v, want scanned=2 skipped=1 swept=1", report)
	}
	if len(report.UnreadableIDs) != 1 || report.UnreadableIDs[0] != unreadable.ID {
		t.Fatalf("unreadable ids = %v, want [%s]", report.UnreadableIDs, unreadable.ID)
	}
	if len(report.SweptIDs) != 1 || report.SweptIDs[0] != readable.ID {
		t.Fatalf("swept ids = %v, want [%s]", report.SweptIDs, readable.ID)
	}
	got, getErr := Open(home).Get(readable.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != StatusAbandoned {
		t.Fatalf("readable stale status = %q, want abandoned", got.Status)
	}
}

func seedUnreadableLaunchAttempt(t *testing.T, home, corruption string) (RunAttempt, time.Time) {
	t.Helper()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_unreadable_launch_"+corruption, "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	launchAt := created.Add(time.Minute)
	if _, err := store.CommitLaunch(attempt.ID, launchAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	corruptPreparedLaunchEvidence(t, home, attempt.ID, corruption)
	return attempt, launchAt
}

func corruptPreparedLaunchEvidence(t *testing.T, home, id, corruption string) {
	t.Helper()
	preparedPath := launchEvidencePath(home, id, launchEvidencePrepared)
	switch corruption {
	case "malformed-json":
		if err := os.WriteFile(preparedPath, []byte("{\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(preparedPath, 0o600); err != nil {
			t.Fatal(err)
		}
	case "unsafe-mode":
		if err := os.Chmod(preparedPath, 0o644); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown corruption fixture %q", corruption)
	}
}

func TestSweepStaleTerminalizesOldPending(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	// Old pending attempt (no heartbeat) — should be swept.
	ra, err := s.Mint("work_old", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}

	// Sweep 1 hour later with 30m threshold.
	rep, err := s.SweepStale(30*time.Minute, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 1 {
		t.Fatalf("scanned = %d, want 1", rep.Scanned)
	}
	if rep.Swept != 1 {
		t.Fatalf("swept = %d, want 1", rep.Swept)
	}
	if rep.SweptIDs[0] != ra.ID {
		t.Fatalf("swept id = %s, want %s", rep.SweptIDs[0], ra.ID)
	}

	// The attempt is now abandoned with a reason.
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAbandoned {
		t.Fatalf("status = %q, want abandoned", got.Status)
	}
	if got.SweepReason == "" {
		t.Fatal("swept attempt must have a sweep_reason")
	}
	if !strings.Contains(got.SweepReason, "stale") {
		t.Fatalf("sweep_reason %q must mention staleness", got.SweepReason)
	}
	if got.CompletedAt.IsZero() {
		t.Fatal("swept attempt must have a completed_at")
	}
}

// TestSweepStaleTerminalizesOldRunning: F5 makes StatusRunning genuinely
// reachable (previously observe-run's "running" job status silently left
// the attempt at StatusPending forever, so this path was never exercised
// with a real running attempt). SweepStale must treat a stale RUNNING
// attempt exactly like a stale pending one -- IsTerminal excludes only
// succeeded/failed/abandoned, so ListNonTerminal (and therefore
// SweepStale) already includes running by construction; this pins that
// guarantee now that running is reachable in practice.
func TestSweepStaleTerminalizesOldRunning(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, err := s.Mint("work_running", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRunning(ra.ID, t0); err != nil {
		t.Fatal(err)
	}

	rep, err := s.SweepStale(30*time.Minute, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Swept != 1 || rep.SweptIDs[0] != ra.ID {
		t.Fatalf("sweep report = %+v, want the running attempt swept", rep)
	}
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAbandoned {
		t.Fatalf("status = %q, want abandoned", got.Status)
	}
}

func TestSweepStaleSkipsFreshHeartbeat(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	// Old attempt but with a fresh heartbeat — should NOT be swept.
	ra, err := s.Mint("work_hb", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	// Heartbeat at t0+50m (within the 30m threshold of sweep time t0+60m).
	if _, err := s.Heartbeat(ra.ID, t0.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rep, err := s.SweepStale(30*time.Minute, t0.Add(60*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Swept != 0 {
		t.Fatalf("swept = %d, want 0 (fresh heartbeat)", rep.Swept)
	}
	if rep.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", rep.Skipped)
	}

	// Still pending.
	got, _ := Open(home).Get(ra.ID)
	if got.Status != StatusPending {
		t.Fatalf("status = %q, want pending (not swept)", got.Status)
	}
}

func TestSweepStaleSkipsTerminal(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, _ := s.Mint("work_done", "actor", "fake", t0)
	s.Complete(ra.ID, StatusSucceeded, t0.Add(time.Minute))

	// Sweep much later — terminal attempt must not be touched.
	rep, err := s.SweepStale(30*time.Minute, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 0 {
		t.Fatalf("scanned = %d, want 0 (terminal not scanned)", rep.Scanned)
	}
	if rep.Swept != 0 {
		t.Fatalf("swept = %d, want 0", rep.Swept)
	}
}

func TestSweepStaleDefaultThreshold(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, _ := s.Mint("work_default", "actor", "fake", t0)

	// threshold=0 → uses DefaultSweepThreshold (30m).
	// Sweep at t0+31m → should sweep.
	rep, err := s.SweepStale(0, t0.Add(31*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Swept != 1 {
		t.Fatalf("swept = %d, want 1 (default threshold)", rep.Swept)
	}
	if rep.SweptIDs[0] != ra.ID {
		t.Fatalf("swept id = %s, want %s", rep.SweptIDs[0], ra.ID)
	}
}

func TestSweepStaleMixedBatch(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	// Old pending → swept.
	old, _ := s.Mint("work_old", "actor", "fake", t0)
	// Fresh pending → kept.
	s.Mint("work_fresh", "actor", "fake", t0.Add(50*time.Minute))
	// Old with fresh heartbeat → kept.
	hb, _ := s.Mint("work_hb", "actor", "fake", t0)
	s.Heartbeat(hb.ID, t0.Add(55*time.Minute))
	// Old terminal → not scanned.
	done, _ := s.Mint("work_done", "actor", "fake", t0)
	s.Complete(done.ID, StatusFailed, t0.Add(time.Minute))

	rep, err := s.SweepStale(30*time.Minute, t0.Add(60*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// Scanned: old, fresh, hb (3 non-terminal). done is terminal → not scanned.
	if rep.Scanned != 3 {
		t.Fatalf("scanned = %d, want 3", rep.Scanned)
	}
	if rep.Swept != 1 {
		t.Fatalf("swept = %d, want 1 (only old)", rep.Swept)
	}
	if rep.SweptIDs[0] != old.ID {
		t.Fatalf("swept id = %s, want %s", rep.SweptIDs[0], old.ID)
	}
	if rep.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2 (fresh + hb)", rep.Skipped)
	}
}

func TestCompleteWithReasonRecordsReason(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, _ := s.Mint("work_reason", "actor", "fake", t0)
	completed, err := s.CompleteWithReason(ra.ID, StatusAbandoned, "stale: no heartbeat for 30m", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if completed.SweepReason != "stale: no heartbeat for 30m" {
		t.Fatalf("sweep_reason = %q, want the reason", completed.SweepReason)
	}
	// Re-complete with reason is rejected (already terminal).
	if _, err := s.CompleteWithReason(ra.ID, StatusFailed, "double", t0.Add(2*time.Hour)); err == nil {
		t.Fatal("re-CompleteWithReason on terminal must fail")
	}
	// Non-terminal status is rejected.
	ra2, _ := s.Mint("work_reason2", "actor", "fake", t0)
	if _, err := s.CompleteWithReason(ra2.ID, StatusRunning, "nope", t0); err == nil {
		t.Fatal("CompleteWithReason with non-terminal status must fail")
	}
}

func TestSweepStaleIdempotentSecondPass(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	s.Mint("work_once", "actor", "fake", t0)

	// First pass sweeps it.
	rep1, _ := s.SweepStale(30*time.Minute, t0.Add(time.Hour))
	if rep1.Swept != 1 {
		t.Fatalf("first sweep: swept = %d, want 1", rep1.Swept)
	}
	// Second pass: already terminal → nothing to sweep.
	rep2, _ := s.SweepStale(30*time.Minute, t0.Add(2*time.Hour))
	if rep2.Scanned != 0 || rep2.Swept != 0 {
		t.Fatalf("second sweep: scanned=%d swept=%d, want 0/0", rep2.Scanned, rep2.Swept)
	}
}

// C5: write() now actually fsyncs before rename. This test exercises the
// write path and verifies the file is durable (exists, decodable, correct
// content) after a Mint — the fsync path is the only changed line, so a
// successful round-trip through a fresh Store is the falsifier.
func TestWriteDurableRoundTrip(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, err := s.Mint("work_durable", "actor", "codex", t0)
	if err != nil {
		t.Fatal(err)
	}
	// The .tmp file must NOT exist (renamed away).
	tmpPath := filepath.Join(home, "run_attempts", ra.ID+".json.tmp")
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf(".tmp file must not exist after write, got err=%v", err)
	}
	// The final file must exist and decode.
	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != ra.ID || got.WorkID != ra.WorkID || got.Status != StatusPending {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, ra)
	}
	// SweepReason is empty (not a swept attempt) → omitempty → not in JSON.
	data, _ := os.ReadFile(filepath.Join(home, "run_attempts", ra.ID+".json"))
	if strings.Contains(string(data), "sweep_reason") {
		t.Fatal("sweep_reason must be absent from non-swept attempt JSON (omitempty)")
	}
}

// Falsifier (audit E15): SweepStaleWithProbe must be per-attempt best-effort
// even when Heartbeat itself fails on a live-flagged candidate — a sibling
// to the probe-error branch (continue) and the CompleteWithReason failure
// branch (continue) one block below. A heartbeat-write failure today does
// `return rep, error`, aborting the entire sweep, so a later stale
// candidate never gets touched.
func TestSweepStaleWithProbeHeartbeatFailureContinuesPastIt(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	// Two stale non-terminal attempts; both well past the 30m threshold at
	// sweep time, so each one enters the live branch.
	ra1, err := s.Mint("work_a", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	ra2, err := s.Mint("work_b", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}

	// Fail only ra1's heartbeat publication through the package's explicit
	// write seam. Production publication uses random exclusive temp names, so
	// poisoning a historical fixed .tmp path would no longer inject a failure.
	// ra2 still uses the real writer, preserving the test's progress proof.
	originalWrite := runattemptWriteFile
	injected := errors.New("injected ra1 heartbeat write failure")
	runattemptWriteFile = func(path string, attempt RunAttempt) error {
		if attempt.ID == ra1.ID && attempt.LastHeartbeatAt != nil {
			return injected
		}
		return originalWrite(path, attempt)
	}
	defer func() { runattemptWriteFile = originalWrite }()

	probe := func(ra RunAttempt) (bool, error) {
		return true, nil // every probe says "live" -> heartbeat path
	}

	rep, err := s.SweepStaleWithProbe(30*time.Minute, t0.Add(time.Hour), probe)
	if err != nil {
		t.Fatalf("SweepStaleWithProbe aborted on a per-attempt heartbeat failure: %v", err)
	}
	if rep.Scanned != 2 {
		t.Fatalf("scanned = %d, want 2 (both attempts must be processed)", rep.Scanned)
	}
	if rep.Protected != 1 {
		t.Fatalf("protected = %d, want 1 (ra2 must have been heartbeated even though ra1's heartbeat failed)", rep.Protected)
	}
	if rep.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2 (ra1 via failed heartbeat, ra2 via successful protected)", rep.Skipped)
	}
	wantSkipped := map[string]bool{ra1.ID: false, ra2.ID: false}
	for _, id := range rep.SkippedIDs {
		if _, ok := wantSkipped[id]; ok {
			wantSkipped[id] = true
		}
	}
	for id, seen := range wantSkipped {
		if !seen {
			t.Fatalf("skipped_ids missing %s, got %v", id, rep.SkippedIDs)
		}
	}

	// Hard proof the loop actually reached ra2: its LastHeartbeatAt is now
	// populated (Heartbeat was called on it successfully).
	got, err := s.Get(ra2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastHeartbeatAt == nil {
		t.Fatal("ra2 must have received a heartbeat during the sweep -- the loop aborted on ra1's heartbeat failure")
	}
}

// A liveness verdict applies only to the exact attempt snapshot the probe
// inspected. If another writer heartbeats the attempt before terminalization,
// the stale verdict must not abandon that newer activity.
func TestSweepStaleWithProbeSkipsHeartbeatWrittenAfterSnapshot(t *testing.T) {
	home := t.TempDir()
	seed := Open(home)
	t0 := time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)
	sweepNow := t0.Add(time.Hour)

	ra, err := seed.Mint("work_probe_heartbeat_race", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.SetOrigin(ra.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	newHeartbeat := sweepNow.Add(time.Second)
	rep, err := seed.SweepStaleWithProbe(30*time.Minute, sweepNow, func(snapshot RunAttempt) (bool, error) {
		if snapshot.ID != ra.ID {
			t.Fatalf("probe snapshot id = %q, want %q", snapshot.ID, ra.ID)
		}
		_, err := Open(home).Heartbeat(ra.ID, newHeartbeat)
		return false, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Swept != 0 || rep.Skipped != 1 {
		t.Fatalf("race report = %+v, want changed candidate skipped", rep)
	}

	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if IsTerminal(got.Status) {
		t.Fatalf("heartbeat written after probe snapshot was abandoned: %+v", got)
	}
	if got.LastHeartbeatAt == nil || !got.LastHeartbeatAt.Equal(newHeartbeat) {
		t.Fatalf("last_heartbeat_at = %v, want %s", got.LastHeartbeatAt, newHeartbeat)
	}
}

// Classification is snapshot-bound too. A transcript attached while the
// liveness probe runs must not be terminalized under the earlier
// side_effect_free classification.
func TestSweepStaleWithProbeSkipsEvidenceWrittenAfterSnapshot(t *testing.T) {
	home := t.TempDir()
	seed := Open(home)
	t0 := time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)

	ra, err := seed.Mint("work_probe_evidence_race", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.SetOrigin(ra.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	transcript := "transcripts/" + ra.ID + ".json"
	rep, err := seed.SweepStaleWithProbe(30*time.Minute, t0.Add(time.Hour), func(snapshot RunAttempt) (bool, error) {
		if snapshot.ID != ra.ID {
			t.Fatalf("probe snapshot id = %q, want %q", snapshot.ID, ra.ID)
		}
		_, err := Open(home).SetTranscript(ra.ID, transcript)
		return false, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Swept != 0 || rep.AutoClosed != 0 || rep.Skipped != 1 {
		t.Fatalf("race report = %+v, want changed candidate skipped without auto-close", rep)
	}

	got, err := Open(home).Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if IsTerminal(got.Status) {
		t.Fatalf("evidence written after probe snapshot was abandoned: %+v", got)
	}
	if got.TranscriptRef != transcript {
		t.Fatalf("transcript_ref = %q, want %q", got.TranscriptRef, transcript)
	}
}

// -----------------------------------------------------------------------
// Sweep-teeth classification (DEC-0027 build step 1, work_195771b63a58):
// the sweep MUST distinguish side-effect-free stale attempts (auto-close)
// from ambiguous ones (escalate). The classification function is pure;
// these tests pin each rule without touching the store.
// -----------------------------------------------------------------------

func TestClassifyStaleRunAttemptClaimReservationIsSideEffectFree(t *testing.T) {
	ra := RunAttempt{Origin: OriginClaimReservation}
	if got := ClassifyStaleRunAttempt(ra); got != SweepClassificationSideEffectFree {
		t.Fatalf("classify(claim_reservation, no artifacts) = %q, want %q", got, SweepClassificationSideEffectFree)
	}
}

func TestClassifyStaleRunAttemptHarnessExecutionWithoutConclusiveLaunchIsAmbiguous(t *testing.T) {
	ra := RunAttempt{Origin: OriginHarnessExecution}
	if got := ClassifyStaleRunAttempt(ra); got != SweepClassificationAmbiguous {
		t.Fatalf("classify(harness_execution, no launch or artifacts) = %q, want %q", got, SweepClassificationAmbiguous)
	}
}

func TestClassifyStaleRunAttemptUnknownOriginIsAmbiguous(t *testing.T) {
	ra := RunAttempt{Origin: "future_or_corrupt_origin"}
	if got := ClassifyStaleRunAttempt(ra); got != SweepClassificationAmbiguous {
		t.Fatalf("classify(unknown origin) = %q, want %q", got, SweepClassificationAmbiguous)
	}
}

func TestClassifyStaleRunAttemptTranscriptRefIsAmbiguous(t *testing.T) {
	ra := RunAttempt{Origin: OriginHarnessExecution, TranscriptRef: "transcripts/foo.json"}
	if got := ClassifyStaleRunAttempt(ra); got != SweepClassificationAmbiguous {
		t.Fatalf("classify(harness_execution + transcript_ref) = %q, want %q", got, SweepClassificationAmbiguous)
	}
}

func TestClassifyStaleRunAttemptDiffRefIsAmbiguous(t *testing.T) {
	ra := RunAttempt{Origin: OriginHarnessExecution, DiffRef: "diffs/foo.patch"}
	if got := ClassifyStaleRunAttempt(ra); got != SweepClassificationAmbiguous {
		t.Fatalf("classify(harness_execution + diff_ref) = %q, want %q", got, SweepClassificationAmbiguous)
	}
}

func TestClassifyStaleRunAttemptTestRefIsAmbiguous(t *testing.T) {
	ra := RunAttempt{Origin: OriginHarnessExecution, TestRef: "tests/foo.json"}
	if got := ClassifyStaleRunAttempt(ra); got != SweepClassificationAmbiguous {
		t.Fatalf("classify(harness_execution + test_ref) = %q, want %q", got, SweepClassificationAmbiguous)
	}
}

func TestClassifyStaleRunAttemptObservedExternalIsAmbiguous(t *testing.T) {
	ra := RunAttempt{Origin: OriginObservedExternal}
	if got := ClassifyStaleRunAttempt(ra); got != SweepClassificationAmbiguous {
		t.Fatalf("classify(observed_external) = %q, want %q", got, SweepClassificationAmbiguous)
	}
}

func TestClassifyStaleRunAttemptEmptyOriginIsAmbiguous(t *testing.T) {
	// pre-F2 legacy record: origin was not persisted. Conservative = operator
	// review, because we cannot prove no side effects.
	ra := RunAttempt{Origin: ""}
	if got := ClassifyStaleRunAttempt(ra); got != SweepClassificationAmbiguous {
		t.Fatalf("classify(empty origin) = %q, want %q", got, SweepClassificationAmbiguous)
	}
}

// Falsifier: even a claim_reservation placeholder with a transcript_ref
// must escalate — the placeholder might have been replaced by a real
// execution whose transcript we shouldn't auto-trust.
func TestClassifyStaleRunAttemptClaimReservationWithTranscriptIsAmbiguous(t *testing.T) {
	ra := RunAttempt{Origin: OriginClaimReservation, TranscriptRef: "transcripts/placeholder-leftover.json"}
	if got := ClassifyStaleRunAttempt(ra); got != SweepClassificationAmbiguous {
		t.Fatalf("classify(claim_reservation + transcript_ref) = %q, want %q", got, SweepClassificationAmbiguous)
	}
}

// -----------------------------------------------------------------------
// Sweep-teeth wiring: the sweep MUST set SweepClassification on every
// attempt it terminalizes, AND track AutoClosed vs Escalated separately
// in the SweepReport so doctor can surface non-zero Escalated.
// -----------------------------------------------------------------------

func TestSweepStaleSetsClassificationOnRunAttempt(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, err := s.Mint("work_classify", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	// Mint leaves Origin empty (pre-F2 default); stamp a real origin so
	// classification has the data it needs.
	if _, err := s.SetOrigin(ra.ID, OriginClaimReservation); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SweepStale(30*time.Minute, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SweepClassification != SweepClassificationSideEffectFree {
		t.Fatalf("classification = %q, want %q (no artifacts)", got.SweepClassification, SweepClassificationSideEffectFree)
	}
	if !strings.Contains(got.SweepReason, "side_effect_free") {
		t.Fatalf("sweep_reason %q must mention the classification so an operator can grep it", got.SweepReason)
	}
	if !strings.Contains(got.SweepReason, "stale") {
		t.Fatalf("sweep_reason %q must still mention staleness (back-compat with existing tests/audits)", got.SweepReason)
	}
}

func TestSweepStaleEscalatesAmbiguousWithTranscript(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, err := s.Mint("work_ambiguous", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the harness having produced a transcript before dying.
	if _, err := s.SetTranscript(ra.ID, "transcripts/"+ra.ID+".json"); err != nil {
		t.Fatalf("set transcript: %v", err)
	}

	rep, err := s.SweepStale(30*time.Minute, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Escalated != 1 {
		t.Fatalf("escalated = %d, want 1 (transcript_ref → ambiguous)", rep.Escalated)
	}
	if rep.AutoClosed != 0 {
		t.Fatalf("auto_closed = %d, want 0 (this attempt had a transcript)", rep.AutoClosed)
	}
	if len(rep.EscalatedIDs) != 1 || rep.EscalatedIDs[0] != ra.ID {
		t.Fatalf("escalated_ids = %v, want [%s]", rep.EscalatedIDs, ra.ID)
	}

	got, _ := s.Get(ra.ID)
	if got.SweepClassification != SweepClassificationAmbiguous {
		t.Fatalf("classification = %q, want %q", got.SweepClassification, SweepClassificationAmbiguous)
	}
	if !strings.Contains(got.SweepReason, "ambiguous") {
		t.Fatalf("sweep_reason %q must mention classification", got.SweepReason)
	}
}

// TestSweepStaleMixedBatchClassification splits a stale batch into
// auto-closed and escalated sets and verifies both counters AND ids are
// right. This is the falsifier the predicate needs: the sweep must NEVER
// route an ambiguous attempt into the auto-closed set, even when other
// attempts in the same pass are clean.
func TestSweepStaleMixedBatchClassification(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	// Clean: claim_reservation placeholder (no artifacts).
	clean, _ := s.Mint("work_clean", "actor", "fake", t0)
	if _, err := s.SetOrigin(clean.ID, OriginClaimReservation); err != nil {
		t.Fatal(err)
	}
	// Ambiguous: has a transcript_ref.
	withTr, _ := s.Mint("work_tr", "actor", "fake", t0)
	if _, err := s.SetTranscript(withTr.ID, "transcripts/"+withTr.ID+".json"); err != nil {
		t.Fatal(err)
	}
	// Ambiguous: observed_external origin (mint, then SetOrigin).
	observed, _ := s.Mint("work_obs", "actor", "fake", t0)
	if _, err := s.SetOrigin(observed.ID, OriginObservedExternal); err != nil {
		t.Fatalf("set origin: %v", err)
	}

	rep, err := s.SweepStale(30*time.Minute, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 3 {
		t.Fatalf("scanned = %d, want 3", rep.Scanned)
	}
	if rep.Swept != 3 {
		t.Fatalf("swept = %d, want 3", rep.Swept)
	}
	if rep.AutoClosed != 1 {
		t.Fatalf("auto_closed = %d, want 1 (clean)", rep.AutoClosed)
	}
	if rep.Escalated != 2 {
		t.Fatalf("escalated = %d, want 2 (tr + observed)", rep.Escalated)
	}
	if len(rep.AutoClosedIDs) != 1 || rep.AutoClosedIDs[0] != clean.ID {
		t.Fatalf("auto_closed_ids = %v, want [%s]", rep.AutoClosedIDs, clean.ID)
	}
	gotEsc := map[string]bool{}
	for _, id := range rep.EscalatedIDs {
		gotEsc[id] = true
	}
	if !gotEsc[withTr.ID] || !gotEsc[observed.ID] {
		t.Fatalf("escalated_ids = %v, want both %s and %s present", rep.EscalatedIDs, withTr.ID, observed.ID)
	}
}

// TestSweepStaleRefusesEmptyClassification pins the fail-closed behavior that
// a stale attempt with an unrecognized classification must surface an error,
// not silently skip the candidate. The production path never produces an
// unrecognized classification because ClassifyStaleRunAttempt is total, but a
// future regression (e.g. ClassifyStaleRunAttempt returning "") must not leave
// the candidate non-terminal.
func TestSweepStaleRefusesEmptyClassification(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, _ := s.Mint("work_empty", "actor", "fake", t0)

	// Force a classification-rejection by calling
	// CompleteWithReasonAndClassification directly with the empty string.
	// Sweep itself is wired to always classify, so the production path
	// cannot reach this — the test pins the rejection at the entrypoint.
	if _, err := s.CompleteWithReasonAndClassification(ra.ID, StatusAbandoned, "stale: test", "", t0.Add(time.Hour)); err == nil {
		t.Fatal("CompleteWithReasonAndClassification with empty classification must reject (sweep must always classify)")
	}
}

// TestCompleteStaleSnapshotSurfacesClassificationError is the direct falsifier for
// completeStaleSnapshot: if ClassifyStaleRunAttempt returns an unrecognized
// value, the helper must return an error and leave the attempt non-terminal.
func TestCompleteStaleSnapshotSurfacesClassificationError(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, err := s.Mint("work_classify_error", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrigin(ra.ID, OriginClaimReservation); err != nil {
		t.Fatal(err)
	}

	// Snapshot the stale candidate exactly as the sweep would see it.
	candidate, err := s.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt classification by tampering with the recognized set. We simulate
	// the regression by replacing ClassifyStaleRunAttempt with a function that
	// returns an unrecognized value. The production implementation is restored.
	origClassify := classifyStaleRunAttemptForTest
	classifyStaleRunAttemptForTest = func(RunAttempt) string { return "not_a_classification" }
	defer func() { classifyStaleRunAttemptForTest = origClassify }()

	_, _, swept, cerr := s.completeStaleSnapshot(candidate, 30*time.Minute, t0.Add(time.Hour))
	if cerr == nil {
		t.Fatal("completeStaleSnapshot with unrecognized classification must error")
	}
	if !strings.Contains(cerr.Error(), "not a valid SweepClassification") {
		t.Fatalf("error = %q, want classification invalid", cerr)
	}
	if swept {
		t.Fatal("unrecognized classification must not sweep the attempt")
	}

	got, err := s.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if IsTerminal(got.Status) {
		t.Fatalf("attempt became terminal despite classification error: %s", got.Status)
	}
}

// TestCompleteStaleSnapshotSurfacesWriteError is the direct falsifier for the
// write path inside completeStaleSnapshot: if the terminal write fails, the
// helper must return the error and leave the attempt non-terminal.
func TestCompleteStaleSnapshotSurfacesWriteError(t *testing.T) {
	home := t.TempDir()
	s := Open(home)
	t0 := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	ra, err := s.Mint("work_write_error", "actor", "fake", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrigin(ra.ID, OriginClaimReservation); err != nil {
		t.Fatal(err)
	}

	candidate, err := s.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Force the atomic write to fail by overriding the production write helper.
	// This directly exercises the regression: if write failure inside
	// completeStaleSnapshot is ignored, swept=true is returned and the candidate
	// is left terminal.
	origWrite := runattemptWriteFile
	runattemptWriteFile = func(path string, ra RunAttempt) error {
		return errors.New("injected: runattempt write blocked")
	}
	defer func() { runattemptWriteFile = origWrite }()

	_, _, swept, cerr := s.completeStaleSnapshot(candidate, 30*time.Minute, t0.Add(time.Hour))
	if cerr == nil {
		t.Fatal("completeStaleSnapshot with blocked write must error")
	}
	if swept {
		t.Fatal("blocked write must not report swept=true")
	}
	if !strings.Contains(cerr.Error(), "runattempt write blocked") {
		t.Fatalf("expected injected write error, got %v", cerr)
	}

	// Restore the writer so the Get below reads the untampered original file.
	runattemptWriteFile = origWrite

	got, err := s.Get(ra.ID)
	if err != nil {
		t.Fatal(err)
	}
	if IsTerminal(got.Status) {
		t.Fatalf("attempt became terminal despite write error: %s", got.Status)
	}
}
