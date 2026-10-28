package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/finding"
	"compound/internal/landlease"
	"compound/internal/policy"
)

func privateNativeConsumerFixture(t *testing.T) (privateSourceFixture, CanonicalProofPlan) {
	t.Helper()
	at := time.Now().UTC().Add(-20 * time.Second)
	f := newPrivateSourceFixtureAt(t, "public", false, at, map[string]string{
		CanonicalConsumerContractPath: string(pytestContract(t)),
		"pyproject.toml":              "[project]\nname = 'synthetic'\nversion = '0.1.0'\n",
		"uv.lock":                     "version = 1\n",
		"proof.sh":                    "#!/bin/sh\nexit 0\n",
	})
	stubPrivateDestination(t, f.review.Destination)
	plan, err := ResolveCanonicalProofPlan(f.repo, f.source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID != "python_uv_consumers_v1" || len(plan.Steps) != 3 {
		t.Fatalf("expected the real declared Python consumer plan: %+v", plan)
	}
	f.review.SourceProfileDigest, err = privateSourceProfileDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	f.finding, err = finding.Record(f.store.home, finding.Finding{
		Topic: "synthetic-private-consumer-review", Body: string(publicReviewJSON(t, f.review)),
		ProducedBy: proofActorAlpha, WorkID: f.workID,
	}, at.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	f.action.ID = "act_private_consumer_review_fixture"
	f.action.ArgumentsHash = f.finding.CID
	f.action.EvidenceRefs = []string{"finding:" + f.finding.ID}
	result, err := f.store.EvaluateAndRecordAction(f.workID, f.action, at.Add(8*time.Second))
	if err != nil || result.Recorded || result.Decision.Outcome != "hold" {
		t.Fatalf("review must start without release authority: %+v %v", result, err)
	}
	cfg, _, err := policy.LoadConfig(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	approvals := policy.NewExactActionApprovalStore(f.store.home)
	approval, _, err := approvals.Plan(f.action.ID, cfg, 5*time.Minute, at.Add(9*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := approvals.Approve(approval.ID, "operator:synthetic-reviewer", cfg, at.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err = f.store.EvaluateAndRecordAction(f.workID, f.action, at.Add(11*time.Second))
	if err != nil || !result.Recorded {
		t.Fatalf("exact native review approval: %+v %v", result, err)
	}
	return f, plan
}

func privateNativeConsumerProfile(t *testing.T) (privateSourceFixture, ProofResourceProfile) {
	t.Helper()
	f, plan := privateNativeConsumerFixture(t)
	bound, err := BindNativeSourceRedactionReview(plan, f.store.home, f.workID, proofActorAlpha, f.action.ID)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := ProofResourceProfileForCanonicalPlan(bound)
	if err != nil {
		t.Fatal(err)
	}
	return f, profile
}

func TestPrivateSourceNativeConsumerCapacityReconstructsReviewedInventory(t *testing.T) {
	f, profile := privateNativeConsumerProfile(t)
	class, err := CanonicalProofCapacityClass(profile)
	if err != nil || class != landlease.ProofCapacityNativeConsumer {
		t.Fatalf("private reviewed Python consumer capacity = %q: %v", class, err)
	}
	if err := f.store.validatePrivateSourceProfileAuthority(f.workID, proofActorAlpha, profile, time.Now()); err != nil {
		t.Fatalf("unchanged exact private review lost native authority: %v", err)
	}
}

func copyPrivateConsumerProfile(profile ProofResourceProfile) ProofResourceProfile {
	changed := profile
	changed.Digest = ""
	changed.Gates = append([]ProofResourceGateProfile(nil), profile.Gates...)
	for i := range changed.Gates {
		changed.Gates[i].Command = append([]string(nil), changed.Gates[i].Command...)
	}
	source := *profile.SourceSnapshot
	changed.SourceSnapshot = &source
	return changed
}

func TestPrivateSourceNativeConsumerCapacityRefusesInventoryDrift(t *testing.T) {
	_, profile := privateNativeConsumerProfile(t)
	for _, kind := range []string{"missing-consumer", "changed-command", "reordered", "extra-argument", "missing-binding", "changed-proof-ref", "wrong-source", "stale-digest", "changed-kind"} {
		t.Run(kind, func(t *testing.T) {
			changed := copyPrivateConsumerProfile(profile)
			last := len(changed.Gates) - 1
			switch kind {
			case "missing-consumer":
				changed.Gates = changed.Gates[1:]
			case "changed-command":
				changed.Gates[0].Command = []string{"true"}
			case "reordered":
				changed.Gates[0], changed.Gates[1] = changed.Gates[1], changed.Gates[0]
			case "extra-argument":
				changed.Gates[last].Command = append(changed.Gates[last].Command, "unexpected")
			case "missing-binding":
				changed.Gates[last].Command = changed.Gates[last].Command[:8]
			case "changed-proof-ref":
				changed.Gates[last].ProofRef = "redact-public-clean"
			case "wrong-source":
				changed.SourceSnapshot.RepositoryFingerprint = strings.Repeat("0", 64)
			case "stale-digest":
				changed.Digest = strings.Repeat("b", 64)
			case "changed-kind":
				changed.Kind = "development"
			}
			if _, err := CanonicalProofCapacityClass(changed); err == nil {
				t.Fatal("modified private consumer inventory accepted")
			}
		})
	}
}

func TestPrivateSourceNativeConsumerCapacityDoesNotGrantReviewOwnership(t *testing.T) {
	f, profile := privateNativeConsumerProfile(t)
	for _, kind := range []string{"action", "policy", "finding", "cid", "actor"} {
		t.Run(kind, func(t *testing.T) {
			changed := copyPrivateConsumerProfile(profile)
			last := len(changed.Gates) - 1
			actor := proofActorAlpha
			switch kind {
			case "action":
				changed.Gates[last].Command[5] = "act_missing_private_consumer_review"
			case "policy":
				changed.Gates[last].Command[6] = "pol_missing_private_consumer_review"
			case "finding":
				changed.Gates[last].Command[7] = "find_000000000000"
			case "cid":
				changed.Gates[last].Command[8] = "sha256:" + strings.Repeat("b", 64)
			case "actor":
				actor = "synthetic-other-actor"
			}
			if class, err := CanonicalProofCapacityClass(changed); err != nil || class != landlease.ProofCapacityNativeConsumer {
				t.Fatalf("typed capacity alone must not authenticate a review: %q %v", class, err)
			}
			before, err := f.store.Get(f.workID)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{
				Actor: actor, TTL: time.Minute, Workspace: f.repo, Command: "compound proof-check", Profile: changed,
			}, time.Now())
			if err == nil || result.Reservation.ID != "" {
				t.Fatalf("unowned private source profile minted a reservation: %+v %v", result, err)
			}
			after, err := f.store.Get(f.workID)
			if err != nil || len(after.RunAttemptIDs) != len(before.RunAttemptIDs) {
				t.Fatalf("refused private authority minted an attempt: %+v %v", after, err)
			}
		})
	}
}

func TestPrivateSourceNativeConsumerCapacityRejectsSourceDrift(t *testing.T) {
	f, profile := privateNativeConsumerProfile(t)
	if err := os.WriteFile(filepath.Join(f.repo, "catalog.txt"), []byte("changed source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalProofCapacityClass(profile); err == nil {
		t.Fatal("dirty source accepted with the previous private review")
	}
}
