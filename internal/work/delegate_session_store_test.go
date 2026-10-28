package work_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/harness"
	"compound/internal/work"
)

// minimalDelegateRunner is the smallest SessionRunner a governed spawn needs:
// Create succeeds and nothing else is probed on the happy path. It records the
// spawn so the test proves a real binding was minted per lane.
type minimalDelegateRunner struct {
	created []string
}

func (r *minimalDelegateRunner) Available() error         { return nil }
func (r *minimalDelegateRunner) Has(string) (bool, error) { return false, nil }
func (r *minimalDelegateRunner) SessionStatus(string) (work.SessionStatus, error) {
	return work.SessionStatus{Exists: false}, nil
}
func (r *minimalDelegateRunner) Create(name, _, _ string, _ []string) error {
	r.created = append(r.created, name)
	return nil
}
func (r *minimalDelegateRunner) SendKeys(string, string, bool) error { return nil }
func (r *minimalDelegateRunner) Kill(string) error                   { return nil }
func (r *minimalDelegateRunner) AttachArgv(name string) []string {
	return []string{"tmux", "attach", "-t", name}
}

// fixturePlanLookup builds a stand-in lane owner from the committed
// launch-profiles fixture: every profile id the owner has ever declared
// publishes a harness_settings artifact, and every other id is unknown to it,
// exactly like `sb profile plan` answering for a profile that does not exist.
// The artifact paths are synthetic /home/example roots; only the profile-id
// set is real.
func fixturePlanLookup(t *testing.T) work.DelegateSessionStorePlanLookupFunc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "launch-profiles.fixture.json"))
	if err != nil {
		t.Fatalf("read launch-profiles fixture: %v", err)
	}
	var fixture struct {
		Schema         string         `json:"schema"`
		LaunchProfiles map[string]any `json:"launch_profiles"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse launch-profiles fixture: %v", err)
	}
	if fixture.Schema != "switchback/launch-profiles@1" || len(fixture.LaunchProfiles) == 0 {
		t.Fatalf("fixture is not a launch-profiles snapshot: %q %d entries", fixture.Schema, len(fixture.LaunchProfiles))
	}
	return func(profile string) (map[string]string, error) {
		if _, known := fixture.LaunchProfiles[profile]; !known {
			return nil, errors.New("lane profile owner could not plan profile " + profile)
		}
		return map[string]string{
			work.SessionStoreSettingsArtifactKind: filepath.Join("/home", "example", "profiles", profile, "settings.json"),
		}, nil
	}
}

// claimReadyWork mirrors the delegate test pool's bootstrap: a work envelope
// created ready and claimed under the actor that will delegate it.
func claimReadyWork(t *testing.T, s *work.Store, actor string) string {
	t.Helper()
	w, err := s.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "delegate spawn is audited as a work lifecycle event",
		State:            "ready",
	}, time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := s.ClaimWithLane(w.ID, actor, "", time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	return w.ID
}

// TestEnrolledLaneSessionStoreResolutionIsTotalOrActionablyReported pins the
// blindness the keeper hit live: `work delegate status` on a claude_zai-bound
// item answered transcript_dir_exists=false with a prose transcript_error and
// nothing else, so the lane could not be verified by effect at all.
//
// The contract under test: for EVERY enrolled harness lane, resolving the
// lane's session store must either succeed (total) or fail into a structured
// repair field that names the lane, every launch profile consulted, and the
// exact missing artifact. A lane that resolves neither a store root nor a
// repair is blind, and this test fails.
//
// Each enrolled lane is driven the way production drives it: a real governed
// Delegate spawn binds harness+lane onto a RunAttempt, then DelegateStatus
// renders the JSON the keeper consumes. The lane identity is the bare
// capacity-lane form (claude_zai -> zai), mirroring how the live binding was
// recorded.
func TestEnrolledLaneSessionStoreResolutionIsTotalOrActionablyReported(t *testing.T) {
	origResolver := work.DelegateSessionStoreResolver
	origLookup := work.DelegateSessionStorePlanLookup
	origHarnessResolver := work.DelegateHarnessResolver
	origReadiness := work.DelegateHarnessReadiness
	origGuard := work.LaneCapacityGuard
	work.DelegateSessionStorePlanLookup = fixturePlanLookup(t)
	work.DelegateSessionStoreResolver = work.ResolveDelegateSessionStore
	work.DelegateHarnessReadiness = func(work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	}
	work.LaneCapacityGuard = func(work.LaneCapacityGuardRequest) (work.LaneCapacityGuardResult, error) {
		return work.LaneCapacityGuardResult{Capacity: work.DispatchLaneCapacity{State: "alive"}}, nil
	}
	t.Cleanup(func() {
		work.DelegateSessionStoreResolver = origResolver
		work.DelegateSessionStorePlanLookup = origLookup
		work.DelegateHarnessResolver = origHarnessResolver
		work.DelegateHarnessReadiness = origReadiness
		work.LaneCapacityGuard = origGuard
	})

	enrolled := harness.DefaultRegistry().Names()
	if len(enrolled) == 0 {
		t.Fatal("DefaultRegistry enrolled no harness lanes; the enumeration has nothing to prove")
	}
	for _, enrolledHarness := range enrolled {
		lane := strings.TrimPrefix(strings.ReplaceAll(enrolledHarness, "_", "-"), "claude-")
		t.Run(enrolledHarness, func(t *testing.T) {
			work.DelegateHarnessResolver = func(work.DelegateHarnessResolveRequest) (work.DelegateHarnessResolution, error) {
				return work.DelegateHarnessResolution{
					Harness:      enrolledHarness,
					Command:      "lane-worker",
					LaneID:       enrolledHarness,
					CapacityLane: lane,
				}, nil
			}
			s, err := work.Open(t.TempDir())
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			s.EnableExperimentalDelegateMutations()
			wID := claimReadyWork(t, s, "operator")
			runner := &minimalDelegateRunner{}
			result, err := s.Delegate(work.DelegateOptions{
				WorkID:    wID,
				Harness:   enrolledHarness,
				Actor:     "operator",
				Workspace: t.TempDir(),
				Now:       time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC),
			}, runner)
			if err != nil || result.Status != "spawned" {
				t.Fatalf("delegate spawn failed: err=%v result=%+v", err, result)
			}
			if len(runner.created) != 1 {
				t.Fatalf("spawn recorded %d sessions, want 1", len(runner.created))
			}
			obs, err := s.DelegateStatus(wID, nil)
			if err != nil {
				t.Fatalf("delegate status: %v", err)
			}
			if !obs.Bound || obs.RunAttemptID == "" {
				t.Fatalf("status is not bound to a run attempt: %+v", obs)
			}
			payload, err := json.Marshal(obs)
			if err != nil {
				t.Fatalf("marshal status: %v", err)
			}
			var doc map[string]any
			if err := json.Unmarshal(payload, &doc); err != nil {
				t.Fatalf("unmarshal status: %v", err)
			}

			transcriptDir, _ := doc["transcript_dir"].(string)
			repair, hasRepair := doc["session_store_repair"]
			if strings.TrimSpace(transcriptDir) == "" && !hasRepair {
				t.Fatalf(
					"enrolled lane %q resolved neither a session store root nor a repair field; status: %s",
					lane, payload,
				)
			}
			if !hasRepair {
				// Total resolution: the transcript directory must be a real
				// derivation under the resolved store root, not an echo.
				if !strings.Contains(transcriptDir, string(filepath.Separator)+"projects"+string(filepath.Separator)) {
					t.Fatalf("lane %q resolved a transcript dir that is not a projects derivation: %q", lane, transcriptDir)
				}
				return
			}
			rep, ok := repair.(map[string]any)
			if !ok {
				t.Fatalf("session_store_repair is not an object: %s", payload)
			}
			if gotLane, _ := rep["lane"].(string); gotLane != lane {
				t.Fatalf("repair names lane %q, want the binding's lane %q: %s", gotLane, lane, payload)
			}
			profiles, _ := rep["launch_profiles_consulted"].([]any)
			if len(profiles) == 0 {
				t.Fatalf("repair names no launch profiles consulted: %s", payload)
			}
			for _, p := range profiles {
				if s, _ := p.(string); strings.TrimSpace(s) == "" {
					t.Fatalf("repair lists a blank launch profile: %s", payload)
				}
			}
			if artifact, _ := rep["missing_artifact"].(string); artifact != work.SessionStoreSettingsArtifactKind {
				t.Fatalf("repair missing_artifact = %q, want %q: %s", artifact, work.SessionStoreSettingsArtifactKind, payload)
			}
			// A repair is only honest alongside the refusal to guess: no
			// transcript dir, no exists=true, and the prose reason still present.
			if dir, _ := doc["transcript_dir"].(string); dir != "" {
				t.Fatalf("repair carried alongside a resolved transcript dir: %s", payload)
			}
			if exists, _ := doc["transcript_dir_exists"].(bool); exists {
				t.Fatalf("repair carried alongside transcript_dir_exists=true: %s", payload)
			}
			if reason, _ := doc["transcript_error"].(string); strings.TrimSpace(reason) == "" {
				t.Fatalf("repair carried without a transcript_error reason: %s", payload)
			}
		})
	}
}
