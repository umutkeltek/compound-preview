package policy

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
)

// Historical plans retain their original typed binding. They are readable
// evidence, but cannot provide the full-intent authority required today.
func legacyExactApprovalFixture(t *testing.T, intent envelope.ActionIntent, at time.Time, withGrant bool) (ExactActionApprovalPlan, ExactActionApproval) {
	t.Helper()
	binding := ActionBindingForIntent(intent, DefaultConfig())
	binding.FullIntentDigest = ""
	plan := ExactActionApprovalPlan{
		SchemaVersion:  ExactActionApprovalPlanSchema,
		ID:             "approval_plan_" + shortActionHash(intent.ID, "legacy"),
		ActionIntentID: intent.ID,
		Binding:        binding,
		BindingDigest:  ActionBindingDigest(binding),
		CreatedAt:      at.UTC().Format(time.RFC3339Nano),
		ExpiresAt:      at.Add(5 * time.Minute).UTC().Format(time.RFC3339Nano),
	}
	if !withGrant {
		return plan, ExactActionApproval{}
	}
	grant, err := NewExactActionApproval("approval_grant_"+shortActionHash(plan.ID, plan.BindingDigest), "operator:fixture", binding, at.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	planRef, _ := eventstore.ReferenceForID(plan.ID)
	actionRef, _ := eventstore.ReferenceForID(intent.ID)
	grant.EvidenceRefs = mergeStrings(grant.EvidenceRefs, planRef.String(), actionRef.String())
	return plan, grant
}

func persistExactApprovalHistoryFixture(t *testing.T, homePath string, intent envelope.ActionIntent, plan ExactActionApprovalPlan, grant ExactActionApproval) {
	t.Helper()
	persistActionIntentForTest(t, homePath, intent)
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(plan.ID, plan, "  "); err != nil {
		t.Fatal(err)
	}
	if grant.ID != "" {
		if _, err := store.PutJSON(grant.ID, grant, "  "); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExactApprovalHistoricalPlansDoNotPoisonCurrentAuthority(t *testing.T) {
	for _, expired := range []bool{false, true} {
		for _, withGrant := range []bool{false, true} {
			for _, sameBody := range []bool{false, true} {
				name := "live"
				if expired {
					name = "expired"
				}
				if withGrant {
					name += "_grant"
				}
				if sameBody {
					name += "_same_body"
				}
				t.Run(name, func(t *testing.T) {
					homePath := t.TempDir()
					if err := home.Init(homePath, "test", fixedNow); err != nil {
						t.Fatal(err)
					}
					cfg := DefaultConfig()
					store := NewExactActionApprovalStore(homePath)
					old := generatedWorkIntent("act_history_fixture", "work_history_fixture", "codex:fixture", "srv/history.example.invalid", "historical preview")
					at := fixedNow
					if expired {
						at = fixedNow.Add(-24 * time.Hour)
					}
					legacy, legacyGrant := legacyExactApprovalFixture(t, old, at, withGrant)
					persistExactApprovalHistoryFixture(t, homePath, old, legacy, legacyGrant)
					current := old
					current.ID = "act_current_fixture"
					if !sameBody {
						current.Preview = "different current preview"
					}
					persistActionIntentForTest(t, homePath, current)
					plan, _, reused, err := store.EnsurePlanForIntent(current, cfg, 5*time.Minute, fixedNow)
					if err != nil {
						t.Fatalf("historical evidence blocked current plan: %v", err)
					}
					if reused || plan.ID == legacy.ID || plan.Binding.FullIntentDigest == "" {
						t.Fatalf("legacy authority reused: %+v reused=%v", plan, reused)
					}
					// Planning alone, including an old grant for the same body,
					// must not turn a hold into authority.
					decision, matched, err := store.ApplyMatching(current, cfg, holdDecisionForTest(current), fixedNow)
					if err != nil || matched || Allowed(decision) {
						t.Fatalf("legacy granted authority: matched=%v decision=%+v err=%v", matched, decision, err)
					}
					if _, _, err := store.Approve(legacy.ID, "operator:fixture", cfg, at.Add(time.Second)); err == nil {
						t.Fatal("legacy plan was approvable")
					}
					if _, _, err := store.Approve(plan.ID, "operator:fixture", cfg, fixedNow.Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					decision, matched, err = store.ApplyMatching(current, cfg, holdDecisionForTest(current), fixedNow.Add(2*time.Second))
					if err != nil || !matched || !Allowed(decision) {
						t.Fatalf("current approval broken: matched=%v decision=%+v err=%v", matched, decision, err)
					}
					if withGrant {
						status, err := store.Check(legacyGrant.ID, fixedNow.Add(2*time.Second))
						if !errors.Is(err, errLegacyExactApprovalPlan) || status.Status == ExactApprovalStatusConsumed {
							t.Fatalf("legacy grant accepted as current authority: %+v %v", status, err)
						}
						events, err := eventstore.Open(homePath)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := events.Read("approval_use_" + shortActionHash(legacyGrant.ID)); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("legacy consumption was written: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestExactApprovalHistoricalScanStillRejectsCorruptAuthority(t *testing.T) {
	for _, corruption := range []string{"legacy_lifetime", "legacy_source_binding", "legacy_grant_binding", "current_source_body"} {
		t.Run(corruption, func(t *testing.T) {
			homePath := t.TempDir()
			if err := home.Init(homePath, "test", fixedNow); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			store := NewExactActionApprovalStore(homePath)
			old := generatedWorkIntent("act_history_fixture", "work_history_fixture", "codex:fixture", "srv/history.example.invalid", "historical preview")
			legacy, grant := legacyExactApprovalFixture(t, old, fixedNow.Add(-24*time.Hour), true)
			if corruption == "current_source_body" {
				legacy.Binding = ActionBindingForIntent(old, cfg)
				legacy.BindingDigest = ActionBindingDigest(legacy.Binding)
				grant = ExactActionApproval{}
				old.Preview = "tampered source body"
			} else {
				switch corruption {
				case "legacy_lifetime":
					legacy.ExpiresAt = legacy.CreatedAt
				case "legacy_source_binding":
					legacy.Binding.Tool = "tampered.tool"
					legacy.BindingDigest = ActionBindingDigest(legacy.Binding)
				case "legacy_grant_binding":
					grant.ExpiresAt = fixedNow.Add(time.Hour).Format(time.RFC3339Nano)
				}
			}
			persistExactApprovalHistoryFixture(t, homePath, old, legacy, grant)
			current := old
			current.ID = "act_current_fixture"
			current.Preview = "unrelated current request"
			persistActionIntentForTest(t, homePath, current)
			_, _, _, err := store.EnsurePlanForIntent(current, cfg, 5*time.Minute, fixedNow)
			if err == nil || !strings.Contains(err.Error(), "invalid exact") {
				t.Fatalf("corrupt authority was silently ignored: %v", err)
			}
		})
	}
}
