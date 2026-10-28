package work

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/landlease"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

func TestResourceMigrationRefusesUnresolvedAuthority(t *testing.T) {
	for _, mode := range []string{"missing-pause", "expired", "foreign-actor", "active-attempt", "pending-completion", "pending-recovery", "expired-holder"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			w := claimedProofWork(t, s, "resource migration", proofActorAlpha, fixedNow)
			now := fixedNow.Add(time.Minute)
			in := ResourceMigrationInput{BackingHome: s.home, Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://writer-pause", WritersPaused: true, ExpiresAt: now.Add(time.Minute)}
			_, held := s.MigrateCoordinationResources(w.ID, in, now)
			approveResourceAction(t, s, held, now)
			switch mode {
			case "missing-pause":
				in.WritersPaused = false
			case "expired":
				in.ExpiresAt = now
			case "foreign-actor":
				in.Actor = proofActorBeta
			case "active-attempt":
				if _, err := s.runAttempts().Mint(w.ID, proofActorAlpha, proofResourceHarness, now); err != nil {
					t.Fatal(err)
				}
			case "pending-completion", "pending-recovery":
				area := "proof-resource-completions"
				if mode == "pending-recovery" {
					area = "proof-resource-legacy-recoveries"
				}
				dir := filepath.Join(s.home, "work", area)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "pending.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "expired-holder":
				if _, err := landlease.Acquire(s.home, "legacy:holder", time.Second, now.Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.MigrateCoordinationResources(w.ID, in, now.Add(3*time.Second)); err == nil {
				t.Fatal("unsafe migration succeeded")
			}
			layout, err := landlease.LayoutStatus(s.home)
			if err != nil || layout.Mode != "legacy" {
				t.Fatalf("refusal changed layout: %+v %v", layout, err)
			}
		})
	}
}

func TestResourceMigrationAndProofConsumerUseScopedLease(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "scoped consumer", proofActorAlpha, fixedNow)
	now := fixedNow.Add(time.Minute)
	in := ResourceMigrationInput{BackingHome: s.home, Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://writer-pause", WritersPaused: true, ExpiresAt: now.Add(time.Minute)}
	_, held := s.MigrateCoordinationResources(w.ID, in, now)
	approveResourceAction(t, s, held, now)
	layout, err := s.MigrateCoordinationResources(w.ID, in, now.Add(3*time.Second))
	if err != nil || layout.Mode != "scoped" {
		t.Fatalf("migration: %+v %v", layout, err)
	}
	key, err := landlease.LandingResourceKey(strings.Repeat("a", 64), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	landing, err := landlease.OpenResource(s.home, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := landing.Acquire("synthetic:"+w.ID, time.Minute, now); err != nil {
		t.Fatal(err)
	}
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor: proofActorAlpha, TTL: time.Minute, Workspace: "/synthetic/repo",
		Command: "compound proof-check --out /synthetic/report.json",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	probe := &DefaultSweepStaleProbe{Home: s.home, Now: func() time.Time { return now.Add(2 * time.Second) }}
	if held, err := probe.LeaseHeld(w.ID); err != nil || !held {
		t.Fatalf("proof work liveness lost: %v %v", held, err)
	}
	if err := acquired.ExecutionBarrier.Close(); err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, FailureReason: "synthetic gate failure",
	}, now.Add(3*time.Second))
	if err != nil || completed.Result.RunAttemptStatus != runattempt.StatusFailed {
		t.Fatalf("completion: %+v %v", completed, err)
	}
	if status, err := landlease.ProofResource(s.home).StatusReadOnly(now); err != nil || status.Held {
		t.Fatalf("proof not released: %+v %v", status, err)
	}
	if status, err := landing.StatusReadOnly(now); err != nil || !status.Held {
		t.Fatalf("landing disturbed: %+v %v", status, err)
	}
}

func TestLandingResourceAuthorityRejectsForeignWorkAndIdentityDrift(t *testing.T) {
	repo, base, _ := sharedLandingReadyRepo(t)
	s := newStore(t)
	w, err := s.Create(CreateInput{Owner: "codex", Actor: proofActorAlpha, SuccessPredicate: "bound landing", GitDelivery: localGitDelivery(repo, base)}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.Claim(w.ID, proofActorAlpha, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	good, err := s.LandingResourceKey(w.ID)
	if err != nil || good.RepositoryFingerprint != w.GitDelivery.RepositoryFingerprint {
		t.Fatalf("key: %+v %v", good, err)
	}
	_, err = s.MutateLandingResource(w.ID, LandingResourceMutation{Actor: proofActorBeta, Holder: "codex:" + w.ID, Operation: "acquire", TTL: time.Minute}, fixedNow.Add(2*time.Second))
	if err == nil {
		t.Fatal("foreign actor acquired")
	}
	w.GitDelivery.RepositoryFingerprint = strings.Repeat("b", 64)
	if err := s.writeJSON(s.workPath(w.ID), w); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LandingResourceKey(w.ID); err == nil {
		t.Fatal("drifted fingerprint accepted")
	}
}

func TestSweepProofHolderUsesFourthComponent(t *testing.T) {
	s := newStore(t)
	holder := proofResourceHolder("pfr_fixture", "ratt_fixture", "work_fixture", "codex:actor")
	// Real minted attempt id keeps the production holder parser strict.
	attempt, err := s.runAttempts().Mint("work_fixture", "codex:actor", proofResourceHarness, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	holder = proofResourceHolder("pfr_fixture", attempt.ID, "work_fixture", "codex:actor")
	if _, err := landlease.Acquire(s.home, holder, time.Minute, fixedNow); err != nil {
		t.Fatal(err)
	}
	p := &DefaultSweepStaleProbe{Home: s.home, Now: func() time.Time { return fixedNow.Add(time.Second) }}
	if held, err := p.LeaseHeld("work_fixture"); err != nil || !held {
		t.Fatalf("liveness: %v %v", held, err)
	}
	if held, err := p.LeaseHeld("work_other"); err != nil || held {
		t.Fatalf("foreign liveness: %v %v", held, err)
	}
}

func approveResourceAction(t *testing.T, s *Store, held error, now time.Time) policy.ExactActionApproval {
	t.Helper()
	if held == nil {
		t.Fatal("action bypassed exact approval")
	}
	match := regexp.MustCompile("resource mutation action (act_[a-z0-9]+) policy hold").FindStringSubmatch(held.Error())
	if len(match) != 2 {
		t.Fatalf("expected policy hold: %v", held)
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		t.Fatal(err)
	}
	approvals := policy.NewExactActionApprovalStore(s.home)
	plan, _, err := approvals.Plan(match[1], cfg, 5*time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	grant, _, err := approvals.Approve(plan.ID, "operator:fixture", cfg, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func TestResourceMigrationRequiresExactUnchangedApproval(t *testing.T) {
	for _, drift := range []string{"none", "evidence", "expiry", "backing", "work-generation"} {
		t.Run(drift, func(t *testing.T) {
			s := newStore(t)
			w := claimedProofWork(t, s, "migration approval", proofActorAlpha, fixedNow)
			now := fixedNow.Add(time.Minute)
			in := ResourceMigrationInput{BackingHome: s.home, Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://pause", WritersPaused: true, ExpiresAt: now.Add(time.Minute)}
			_, held := s.MigrateCoordinationResources(w.ID, in, now)
			layout, err := landlease.LayoutStatus(s.home)
			if err != nil || layout.Mode != "legacy" {
				t.Fatalf("unapproved cutover: %+v %v", layout, err)
			}
			unchanged, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeSHA, _ := workEnvelopeSHA256(w)
			afterSHA, _ := workEnvelopeSHA256(unchanged)
			if beforeSHA != afterSHA {
				t.Fatal("held proposal changed work generation")
			}
			grant := approveResourceAction(t, s, held, now)
			switch drift {
			case "evidence":
				in.EvidenceRef = "test://different-pause"
			case "expiry":
				in.ExpiresAt = in.ExpiresAt.Add(time.Second)
			case "backing":
				in.BackingHome = t.TempDir()
			case "work-generation":
				w.SuccessPredicate += " revised"
				if err := s.writeJSON(s.workPath(w.ID), w); err != nil {
					t.Fatal(err)
				}
			}
			layout, err = s.MigrateCoordinationResources(w.ID, in, now.Add(3*time.Second))
			if drift == "none" {
				if err != nil || layout.Mode != "scoped" {
					t.Fatalf("exact retry failed: %+v %v", layout, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "policy hold") {
					t.Fatalf("drift accepted or wrong refusal: %v", err)
				}
				layout, err = landlease.LayoutStatus(s.home)
				if err != nil || layout.Mode != "legacy" {
					t.Fatalf("drift changed layout: %+v %v", layout, err)
				}
				status, err := policy.NewExactActionApprovalStore(s.home).Check(grant.ID, now.Add(4*time.Second))
				if err != nil || status.Status != policy.ExactApprovalStatusReady {
					t.Fatalf("drift consumed grant: %+v %v", status, err)
				}
			}
		})
	}
}

func TestResourceMigrationRequiresSharedBackingHome(t *testing.T) {
	for range 2 {
		s := newStore(t)
		w := claimedProofWork(t, s, "explicit shared capacity", proofActorAlpha, fixedNow)
		_, err := s.MigrateCoordinationResources(w.ID, ResourceMigrationInput{Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://pause", WritersPaused: true, ExpiresAt: fixedNow.Add(time.Minute)}, fixedNow)
		if err == nil || !strings.Contains(err.Error(), "explicit shared --backing-home") {
			t.Fatalf("implicit split capacity allowed: %v", err)
		}
	}
}

func TestResourceMutationFailedTransitionIsNotPerformed(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "failed migration", proofActorAlpha, fixedNow)
	now := fixedNow.Add(time.Minute)
	in := ResourceMigrationInput{BackingHome: s.home, Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://pause", WritersPaused: true, ExpiresAt: now.Add(time.Minute)}
	_, held := s.MigrateCoordinationResources(w.ID, in, now)
	approveResourceAction(t, s, held, now)
	if _, err := landlease.Acquire(s.home, "foreign", time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigrateCoordinationResources(w.ID, in, now.Add(3*time.Second)); err == nil {
		t.Fatal("occupied layout migrated")
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.ActionIntentIDs) != len(w.ActionIntentIDs) || after.PolicyDecisionID != w.PolicyDecisionID {
		t.Fatal("failed transition entered performed history")
	}
	events, err := s.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if !strings.HasPrefix(last.Note, "resource transition failed:") || len(last.Attached) != 2 {
		t.Fatalf("missing exact failed outcome: %+v", last)
	}
	record, err := s.events.Read(last.Attached[0])
	if err != nil {
		t.Fatal(err)
	}
	var action envelope.ActionIntent
	if err := json.Unmarshal(record.Bytes, &action); err != nil {
		t.Fatal(err)
	}
	if action.MutationClass != "security" || action.Reversibility != "irreversible" {
		t.Fatalf("wrong migration risk: %+v", action)
	}
}

func TestLandingStealApprovalCannotTransferChangedHolder(t *testing.T) {
	for _, nextHolder := range []string{"predecessor:C", "predecessor:B"} {
		t.Run(nextHolder, func(t *testing.T) {
			repo, base, _ := sharedLandingReadyRepo(t)
			s := newStore(t)
			w, err := s.Create(CreateInput{Owner: "codex", Actor: proofActorAlpha, SuccessPredicate: "exact predecessor", GitDelivery: localGitDelivery(repo, base)}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			w, err = s.Claim(w.ID, proofActorAlpha, fixedNow.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			now := fixedNow.Add(time.Minute)
			key, err := s.LandingResourceKey(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			resource, err := landlease.OpenResource(s.home, key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resource.Acquire("predecessor:B", time.Minute, now); err != nil {
				t.Fatal(err)
			}
			in := LandingResourceMutation{Actor: proofActorAlpha, Holder: "codex:" + w.ID, Operation: "steal", Reason: "reviewed transfer"}
			_, held := s.MutateLandingResource(w.ID, in, now)
			grant := approveResourceAction(t, s, held, now)
			if _, err := resource.Release("predecessor:B", now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := resource.Acquire(nextHolder, time.Minute, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			_, held = s.MutateLandingResource(w.ID, in, now.Add(3*time.Second))
			if held == nil || !strings.Contains(held.Error(), "policy hold") {
				t.Fatalf("stale approval transferred successor: %v", held)
			}
			status, err := resource.StatusReadOnly(now.Add(4 * time.Second))
			if err != nil || status.Holder != nextHolder {
				t.Fatalf("successor changed: %+v %v", status, err)
			}
			approval, err := policy.NewExactActionApprovalStore(s.home).Check(grant.ID, now.Add(4*time.Second))
			if err != nil || approval.Status != policy.ExactApprovalStatusReady {
				t.Fatalf("stale grant consumed: %+v %v", approval, err)
			}
			approveResourceAction(t, s, held, now.Add(3*time.Second))
			if _, err := s.MutateLandingResource(w.ID, in, now.Add(6*time.Second)); err != nil {
				t.Fatalf("new exact approval did not transfer: %v", err)
			}
		})
	}
}

func TestResourceMigrationRecoversExactPublishedMarker(t *testing.T) {
	for _, stage := range []string{"published", "before-result", "before-work"} {
		t.Run(stage, func(t *testing.T) {
			s := newStore(t)
			w := claimedProofWork(t, s, "recover cutover audit", proofActorAlpha, fixedNow)
			now := fixedNow.Add(time.Minute)
			in := ResourceMigrationInput{BackingHome: s.home, Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://pause", WritersPaused: true, ExpiresAt: now.Add(time.Minute)}
			_, held := s.MigrateCoordinationResources(w.ID, in, now)
			approveResourceAction(t, s, held, now)
			s.resourceMigrationFault = func(actual string) error {
				if actual == stage {
					return errors.New("injected audit write failure")
				}
				return nil
			}
			_, err := s.MigrateCoordinationResources(w.ID, in, now.Add(3*time.Second))
			var recovery *ResourceMigrationRecoveryRequired
			if !errors.As(err, &recovery) || recovery.ActionIntentID == "" || recovery.PolicyDecisionID == "" {
				t.Fatalf("missing exact recovery: %v", err)
			}
			layout, err := landlease.LayoutStatus(s.home)
			if err != nil || layout.Mode != "scoped" {
				t.Fatalf("marker not published: %+v %v", layout, err)
			}
			// Reopen after a simulated crash; expiry bars new cutover, not audit reconciliation.
			s, err = Open(s.home)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := s.MigrateCoordinationResources(w.ID, in, now.Add(10*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			after, err := s.Get(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, id := range after.ActionIntentIDs {
				if id == recovery.ActionIntentID {
					count++
				}
			}
			if count != 1 || after.PolicyDecisionID != recovery.PolicyDecisionID {
				t.Fatalf("reconciliation not idempotent: count=%d", count)
			}
			events, err := s.eventsFor(w.ID)
			if err != nil {
				t.Fatal(err)
			}
			count = 0
			for _, event := range events {
				if event.Note == "coordination cutover published; exact marker reconciled" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("duplicate result events: %d", count)
			}
		})
	}
}

func TestResourceMigrationRecoveryRefusesChangedAuthority(t *testing.T) {
	for _, drift := range []string{"input", "work", "marker", "decision"} {
		t.Run(drift, func(t *testing.T) {
			s := newStore(t)
			w := claimedProofWork(t, s, "cutover recovery drift", proofActorAlpha, fixedNow)
			now := fixedNow.Add(time.Minute)
			in := ResourceMigrationInput{BackingHome: s.home, Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://pause", WritersPaused: true, ExpiresAt: now.Add(time.Minute)}
			_, held := s.MigrateCoordinationResources(w.ID, in, now)
			approveResourceAction(t, s, held, now)
			s.resourceMigrationFault = func(stage string) error {
				if stage == "published" {
					return errors.New("crash")
				}
				return nil
			}
			if _, err := s.MigrateCoordinationResources(w.ID, in, now.Add(3*time.Second)); err == nil {
				t.Fatal("fault did not fire")
			}
			s.resourceMigrationFault = nil
			switch drift {
			case "input":
				in.EvidenceRef = "test://different"
			case "work":
				w.SuccessPredicate += " newer"
				if err := s.writeJSON(s.workPath(w.ID), w); err != nil {
					t.Fatal(err)
				}
			case "marker":
				path := filepath.Join(s.home, "state", "land-lease.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var marker map[string]any
				if err := json.Unmarshal(data, &marker); err != nil {
					t.Fatal(err)
				}
				marker["holder"].(map[string]any)["backing_root"] = t.TempDir()
				if err := s.writeJSON(path, marker); err != nil {
					t.Fatal(err)
				}
			case "decision":
				data, err := os.ReadFile(s.resourceMigrationPendingPath())
				if err != nil {
					t.Fatal(err)
				}
				var pending resourceMigrationPending
				if err := json.Unmarshal(data, &pending); err != nil {
					t.Fatal(err)
				}
				pending.Decision.Reasons = append(pending.Decision.Reasons, "forged")
				if err := s.writeResourceMigrationPending(pending); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.MigrateCoordinationResources(w.ID, in, now.Add(10*time.Minute)); err == nil {
				t.Fatal("drifted recovery accepted")
			}
		})
	}
}

func TestResourceMigrationPreparedWithoutMarkerNeedsFreshApproval(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "pre-publication crash", proofActorAlpha, fixedNow)
	now := fixedNow.Add(time.Minute)
	in := ResourceMigrationInput{BackingHome: s.home, Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://pause", WritersPaused: true, ExpiresAt: now.Add(time.Minute)}
	_, held := s.MigrateCoordinationResources(w.ID, in, now)
	approveResourceAction(t, s, held, now)
	s.resourceMigrationFault = func(stage string) error {
		if stage == "prepared" {
			return errors.New("crash")
		}
		return nil
	}
	if _, err := s.MigrateCoordinationResources(w.ID, in, now.Add(3*time.Second)); err == nil {
		t.Fatal("fault did not fire")
	}
	s.resourceMigrationFault = nil
	if _, err := s.MigrateCoordinationResources(w.ID, in, now.Add(4*time.Second)); err == nil || !strings.Contains(err.Error(), "fresh exact approval") {
		t.Fatalf("pre-publication recovery: %v", err)
	}
	if _, err := s.MigrateCoordinationResources(w.ID, in, now.Add(5*time.Second)); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("consumed grant replayed: %v", err)
	}
	layout, err := landlease.LayoutStatus(s.home)
	if err != nil || layout.Mode != "legacy" {
		t.Fatalf("recovery published marker: %+v %v", layout, err)
	}
}

func installBroadResourceGrant(t *testing.T, s *Store) {
	t.Helper()
	body := []byte("standing_grants:\n  - id: broad_home_grant\n    capability: coordination.resources\n    mutation_class: security\n    reversibility: irreversible\n    blast_radius: host\n")
	if err := os.MkdirAll(filepath.Join(s.home, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "config", "policy.yaml"), body, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		t.Fatal(err)
	}
	decision := policy.Evaluate(envelope.ActionIntent{Capability: "coordination.resources", MutationClass: "security", Reversibility: "irreversible", BlastRadius: "host"}, cfg, fixedNow)
	if !policy.Allowed(decision) || decision.StandingGrantID != "broad_home_grant" {
		t.Fatalf("fixture must exercise broad grant allowance: %+v", decision)
	}
}

func TestResourceMigrationRequiresExactApprovalDespiteStandingGrant(t *testing.T) {
	s := newStore(t)
	w := claimedProofWork(t, s, "migration broad grant", proofActorAlpha, fixedNow)
	installBroadResourceGrant(t, s)
	now := fixedNow.Add(time.Minute)
	in := ResourceMigrationInput{BackingHome: s.home, Actor: proofActorAlpha, Operator: "operator:fixture", EvidenceRef: "test://pause", WritersPaused: true, ExpiresAt: now.Add(time.Minute)}
	_, held := s.MigrateCoordinationResources(w.ID, in, now)
	layout, err := landlease.LayoutStatus(s.home)
	if err != nil || layout.Mode != "legacy" {
		t.Fatalf("unapproved cutover: %+v %v", layout, err)
	}
	approveResourceAction(t, s, held, now)
	if _, err := s.MigrateCoordinationResources(w.ID, in, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
}
