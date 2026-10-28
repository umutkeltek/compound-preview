package runattempt

import (
	"errors"
	"testing"
	"time"

	homepkg "compound/internal/home"
)

func withAdmissionAssertingRunAttemptWriter(t *testing.T, homePath, workID string, fn func() error) {
	t.Helper()
	original := runattemptWriteFile
	observed := false
	runattemptWriteFile = func(path string, attempt RunAttempt) error {
		if attempt.WorkID == workID {
			acquired, err := homepkg.TryWithWorkAdmissionLock(homePath, workID, func() error { return nil })
			if err != nil {
				return err
			}
			if acquired {
				return errors.New("runattempt write occurred outside work admission")
			}
			observed = true
		}
		return original(path, attempt)
	}
	err := fn()
	runattemptWriteFile = original
	if err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("mutation did not reach the admission-asserting writer")
	}
}

func TestLivenessIncreasingWritesHoldWorkAdmission(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	t.Run("mint", func(t *testing.T) {
		homePath := t.TempDir()
		store := Open(homePath)
		withAdmissionAssertingRunAttemptWriter(t, homePath, "work_mint_admission", func() error {
			_, err := store.MintWithActionIntent("work_mint_admission", "codex", HarnessPlaceholder, "act_mint_admission", t0)
			return err
		})
	})

	t.Run("mint_with_id", func(t *testing.T) {
		homePath := t.TempDir()
		store := Open(homePath)
		id := MintDeterministic("work-admission", t0)
		withAdmissionAssertingRunAttemptWriter(t, homePath, "work_mint_id_admission", func() error {
			_, err := store.MintWithID(id, "work_mint_id_admission", "codex", HarnessPlaceholder, t0)
			return err
		})
	})

	t.Run("mark_running_binds_exact_heartbeat", func(t *testing.T) {
		homePath := t.TempDir()
		store := Open(homePath)
		attempt, err := store.Mint("work_mark_running_admission", "codex", HarnessPlaceholder, t0)
		if err != nil {
			t.Fatal(err)
		}
		markAt := t0.Add(time.Minute)
		withAdmissionAssertingRunAttemptWriter(t, homePath, attempt.WorkID, func() error {
			updated, err := store.MarkRunning(attempt.ID, markAt)
			if err != nil {
				return err
			}
			if updated.LastHeartbeatAt == nil || !updated.LastHeartbeatAt.Equal(markAt) {
				t.Fatalf("MarkRunning heartbeat = %v, want exact injected %v", updated.LastHeartbeatAt, markAt)
			}
			return nil
		})
	})

	t.Run("heartbeat", func(t *testing.T) {
		homePath := t.TempDir()
		store := Open(homePath)
		attempt, err := store.Mint("work_heartbeat_admission", "codex", HarnessPlaceholder, t0)
		if err != nil {
			t.Fatal(err)
		}
		withAdmissionAssertingRunAttemptWriter(t, homePath, attempt.WorkID, func() error {
			_, err := store.Heartbeat(attempt.ID, t0.Add(time.Minute))
			return err
		})
	})

	t.Run("heartbeat_progress", func(t *testing.T) {
		homePath := t.TempDir()
		store := Open(homePath)
		attempt, err := store.Mint("work_progress_admission", "codex", HarnessPlaceholder, t0)
		if err != nil {
			t.Fatal(err)
		}
		withAdmissionAssertingRunAttemptWriter(t, homePath, attempt.WorkID, func() error {
			_, err := store.HeartbeatProgress(attempt.ID, t0.Add(time.Minute), RunProgress{Done: 1, Remaining: 1})
			return err
		})
	})

	t.Run("commit_launch_spans_start", func(t *testing.T) {
		homePath := t.TempDir()
		store := Open(homePath)
		attempt, err := store.Mint("work_launch_admission", "codex", HarnessPlaceholder, t0)
		if err != nil {
			t.Fatal(err)
		}
		callbackChecked := false
		withAdmissionAssertingRunAttemptWriter(t, homePath, attempt.WorkID, func() error {
			_, err := store.CommitLaunch(attempt.ID, t0.Add(time.Minute), func() error {
				acquired, err := homepkg.TryWithWorkAdmissionLock(homePath, attempt.WorkID, func() error { return nil })
				if err != nil {
					return err
				}
				if acquired {
					return errors.New("launch callback ran outside work admission")
				}
				callbackChecked = true
				return nil
			})
			return err
		})
		if !callbackChecked {
			t.Fatal("launch callback did not observe held work admission")
		}
	})
}
