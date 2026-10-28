package work

import (
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/policy"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"
)

func proofMigrationStoreFixture(t *testing.T) (*Store, *Store, envelope.WorkEnvelope, ResourceMigrationInput) {
	t.Helper()
	s, other := newStore(t), newStore(t)
	root, err := home.HabitatPath(s.home, "work.resource-leases")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	homes := []string{s.home, other.home}
	for i, h := range homes {
		homes[i], err = filepath.EvalSymlinks(h)
		if err != nil {
			t.Fatal(err)
		}
		if err := landlease.MigrateLegacyLayout(h, root, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(homes)
	w := claimedProofWork(t, s, "proof class cutover", proofActorAlpha, fixedNow)
	in := ResourceMigrationInput{Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://paused-writers", BackingHome: s.home, WritersPaused: true, ProofClasses: true, ProofHomes: homes, ExpiresAt: fixedNow.Add(3 * time.Minute)}
	return s, other, w, in
}
func TestProofMigrationRequiresExactApprovalAndCensusesEveryHome(t *testing.T) {
	for _, area := range []string{"", "proof-resource-completions", "proof-resource-legacy-recoveries", "proof-resource-retirement-pending", "active-attempt"} {
		t.Run(area, func(t *testing.T) {
			s, other, w, in := proofMigrationStoreFixture(t)
			_, held := s.MigrateCoordinationResources(w.ID, in, fixedNow)
			if held == nil {
				t.Fatal("one-way migration bypassed exact approval")
			}
			approveResourceAction(t, s, held, fixedNow)
			if area == "active-attempt" {
				if _, err := other.runAttempts().Mint(w.ID, proofActorAlpha, proofResourceHarness, fixedNow); err != nil {
					t.Fatal(err)
				}
			} else if area != "" {
				dir := filepath.Join(other.home, "work", area)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "pending.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.MigrateCoordinationResources(w.ID, in, fixedNow.Add(3*time.Second))
			if area != "" {
				if err == nil {
					t.Fatal("other home's pending journal ignored")
				}
				if generation, err := landlease.ProofCoordinationGeneration(s.home); err != nil || generation != "" {
					t.Fatalf("blocked census activated domain: %q %v", generation, err)
				}
			} else if err != nil || result.ProofGeneration == "" {
				t.Fatalf("migration: %+v %v", result, err)
			}
		})
	}
}
func TestProofMigrationExpiredPreparedNeedsFreshExactApproval(t *testing.T) {
	s, _, w, in := proofMigrationStoreFixture(t)
	_, held := s.MigrateCoordinationResources(w.ID, in, fixedNow)
	approveResourceAction(t, s, held, fixedNow)
	s.resourceMigrationFault = func(stage string) error {
		if stage == "proof-prepared" {
			return errors.New("interrupt")
		}
		return nil
	}
	if _, err := s.MigrateCoordinationResources(w.ID, in, fixedNow.Add(3*time.Second)); err == nil {
		t.Fatal("missing injected failure")
	}
	s.resourceMigrationFault = nil
	later := fixedNow.Add(4 * time.Minute)
	if _, err := s.MigrateCoordinationResources(w.ID, in, later); err == nil {
		t.Fatal("expired pause replayed")
	}
	in.ExpiresAt = later.Add(time.Minute)
	in.EvidenceRef = "test://new-writer-pause"
	_, held = s.MigrateCoordinationResources(w.ID, in, later)
	approveResourceAction(t, s, held, later)
	if _, err := landlease.ProofCoordinationGeneration(s.home); err == nil {
		t.Fatal("prepared domain activated before fresh approval consumed")
	}
	result, err := s.MigrateCoordinationResources(w.ID, in, later.Add(3*time.Second))
	if err != nil || result.ProofGeneration == "" {
		t.Fatalf("forward recovery: %+v %v", result, err)
	}
}
func TestProofMigrationCannotCertifyScopedLayoutOrOverwriteNewerWork(t *testing.T) {
	for _, mode := range []string{"prepared", "before-work"} {
		t.Run(mode, func(t *testing.T) {
			s, _, w, in := proofMigrationStoreFixture(t)
			_, held := s.MigrateCoordinationResources(w.ID, in, fixedNow)
			approveResourceAction(t, s, held, fixedNow)
			s.resourceMigrationFault = func(stage string) error {
				if stage == mode || (mode == "prepared" && stage == "proof-prepared") {
					return errors.New("interrupt")
				}
				return nil
			}
			if _, err := s.MigrateCoordinationResources(w.ID, in, fixedNow.Add(3*time.Second)); err == nil {
				t.Fatal("missing interruption")
			}
			s.resourceMigrationFault = nil
			current, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			current.DesiredOutcome += " newer owner generation"
			if err := s.writeJSON(s.workPath(w.ID), current); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.MigrateCoordinationResources(w.ID, in, fixedNow.Add(4*time.Second)); err == nil {
				t.Fatal("overwrote newer work authority")
			}
			after, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("recovery mutated newer work")
			}
		})
	}
}

func TestProofMigrationPreparedRechecksDecisionExpiryAndHalt(t *testing.T) {
	for _, mode := range []string{"expiry", "halt", "policy", "tampered-policy"} {
		t.Run(mode, func(t *testing.T) {
			s, _, w, in := proofMigrationStoreFixture(t)
			_, held := s.MigrateCoordinationResources(w.ID, in, fixedNow)
			match := regexp.MustCompile("resource mutation action (act_[a-z0-9]+) policy hold").FindStringSubmatch(held.Error())
			if len(match) != 2 {
				t.Fatal(held)
			}
			cfg, _, err := policy.LoadConfig(s.home)
			if err != nil {
				t.Fatal(err)
			}
			approvals := policy.NewExactActionApprovalStore(s.home)
			plan, _, err := approvals.Plan(match[1], cfg, 20*time.Second, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := approvals.Approve(plan.ID, "operator:fixture", cfg, fixedNow.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			s.resourceMigrationFault = func(stage string) error {
				if stage == "proof-prepared" {
					return errors.New("interrupt")
				}
				return nil
			}
			if _, err := s.MigrateCoordinationResources(w.ID, in, fixedNow.Add(3*time.Second)); err == nil {
				t.Fatal("missing interruption")
			}
			s.resourceMigrationFault = nil
			at := fixedNow.Add(30 * time.Second)
			if mode == "halt" {
				at = fixedNow.Add(5 * time.Second)
				if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{Global: true, Reason: "fixture incident"}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "policy" || mode == "tampered-policy" {
				at = fixedNow.Add(5 * time.Second)
				cfg.PolicyEvidenceRefs = append(cfg.PolicyEvidenceRefs, "test://policy-revision-changed")
				body, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(s.home, "config"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "tampered-policy" {
				body, err := os.ReadFile(s.proofMigrationPendingPath())
				if err != nil {
					t.Fatal(err)
				}
				var pending resourceMigrationPending
				if err := json.Unmarshal(body, &pending); err != nil {
					t.Fatal(err)
				}
				pending.PolicyRevision = policy.ActionBindingForIntent(pending.Action, cfg).PolicyRevision
				if err := s.writeJSON(s.proofMigrationPendingPath(), pending); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.MigrateCoordinationResources(w.ID, in, at); err == nil {
				t.Fatal("stale effect authority activated prepared domain")
			}
			if _, err := landlease.ProofCoordinationGeneration(s.home); err == nil {
				t.Fatal("domain activated despite expired approval or halt")
			}
		})
	}
}
