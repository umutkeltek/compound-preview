package hook

import (
	"strings"
	"testing"
)

// content_key_heldout_test.go — slice 3 of the consumption-detector-v2 packet: the held-out
// fixture gate. This is the packet's false-credit floor, enforced BY A TEST (falsifier f):
// content-key-path precision must be >= ContentKeyPrecisionFloor, where the denominator is
// content-key CREDITS ONLY (a claim v1's exact-id alone already would have caught is excluded --
// it is not something content-key is responsible for), and there must be exactly ZERO false
// credits in the sibling-negative (category C) and self-echo (category E) fixtures specifically.
//
// Categories required by the packet, each represented below: (A) v1 additive-preservation
// positives, (B) content-key true positives (no id), (C) sibling negatives, (D) large padded
// ambient-scatter negatives, (E) self-echo negatives + a requoted-digest variant, (F) low-
// specificity ineligible claims, (G) missing/torn transcript fail-open (checked separately, not a
// "credit" row), (H) a multilingual/paraphrase reference asserted as an accepted MISS.

// ContentKeyPrecisionFloor is the packet-specified false-credit floor (falsifier f): content-key
// credits (claims consumed via content-key where v1 exact-id alone would NOT have fired) must be
// at least this precise on the held-out set below. Never loosen this to make a test pass --
// per the packet, the window may only shrink.
const ContentKeyPrecisionFloor = 0.95

// heldOutClaim is one claim in a held-out fixture, plus its ground-truth label and category tag.
type heldOutClaim struct {
	Category     string // A..H
	Claim        claimFixture
	WantConsumed bool // ground truth: should this ref be reported consumed
}

// heldOutFixture is one transcript scenario: every claim injected into that "pack" (for
// content-key derivation and echo-exclusion scope) plus the ground-truth label per claim.
type heldOutFixture struct {
	Name       string
	Claims     []heldOutClaim
	Transcript string
}

func heldOutFixtures() []heldOutFixture {
	pad := func(text string, times int) string {
		var b strings.Builder
		for i := 0; i < times; i++ {
			b.WriteString(text)
		}
		return b.String()
	}

	return []heldOutFixture{
		// --- Category A: v1 additive-preservation positives (id present; must not become a
		// content-key "credit" since v1 alone already accounts for the consumed verdict). ---
		{
			Name: "A1_exact_id_hit",
			Claims: []heldOutClaim{{
				Category: "A", WantConsumed: true,
				Claim: claimFixture{Ref: "claim://claim_a1", Entity: "cache_warmup", Predicate: "duration",
					Object: "reduced after the prefetch change shipped"},
			}},
			Transcript: `{"role":"assistant","content":"applied claim_a1 successfully, no further action."}` + "\n",
		},
		{
			Name: "A2_exact_id_hit",
			Claims: []heldOutClaim{{
				Category: "A", WantConsumed: true,
				Claim: claimFixture{Ref: "claim://claim_a2", Entity: "session_store", Predicate: "eviction_policy",
					Object: "switched to least recently used"},
			}},
			Transcript: `{"role":"assistant","content":"used claim_a2 to update the config."}` + "\n",
		},

		// --- Category B: content-key true positives (no id anywhere). ---
		{
			Name: "B1_checkout_flow",
			Claims: []heldOutClaim{{
				Category: "B", WantConsumed: true,
				Claim: claimFixture{Ref: "claim://claim_b1", Entity: "checkout_flow", Predicate: "conversion_rate",
					Object: "increased to 12 percent this week"},
			}},
			Transcript: `{"role":"assistant","content":"I confirmed checkout_flow conversion_rate increased after the deploy this week."}` + "\n",
		},
		{
			Name: "B2_source_host",
			Claims: []heldOutClaim{{
				Category: "B", WantConsumed: true,
				Claim: claimFixture{Ref: "claim://claim_b2", Entity: "source_host", Predicate: "deploy_status",
					Object: "rolled back due to failing health checks"},
			}},
			Transcript: `{"role":"assistant","content":"source_host deploy_status shows it rolled back due to failing health checks."}` + "\n",
		},
		{
			Name: "B3_billing_export",
			Claims: []heldOutClaim{{
				Category: "B", WantConsumed: true,
				Claim: claimFixture{Ref: "claim://claim_b3", Entity: "billing_export", Predicate: "retry_policy",
					Object: "backs off exponentially after repeated failures"},
			}},
			Transcript: `{"role":"assistant","content":"Noted billing_export retry_policy backs off exponentially after repeated failures."}` + "\n",
		},
		{
			Name: "B4_lookup_cache",
			Claims: []heldOutClaim{{
				Category: "B", WantConsumed: true,
				Claim: claimFixture{Ref: "claim://claim_b4", Entity: "lookup_cache", Predicate: "hit_ratio",
					Object: "improved substantially after tuning"},
			}},
			Transcript: `{"role":"assistant","content":"lookup_cache hit_ratio improved substantially after the recent tuning pass."}` + "\n",
		},
		{
			Name: "B5_onboarding_flow",
			Claims: []heldOutClaim{{
				Category: "B", WantConsumed: true,
				Claim: claimFixture{Ref: "claim://claim_b5", Entity: "onboarding_flow", Predicate: "completion_rate",
					Object: "dropped sharply after the redesign"},
			}},
			Transcript: `{"role":"assistant","content":"Users onboarding_flow completion_rate dropped sharply once the redesign shipped."}` + "\n",
		},
		{
			Name: "B6_search_index",
			Claims: []heldOutClaim{{
				Category: "B", WantConsumed: true,
				Claim: claimFixture{Ref: "claim://claim_b6", Entity: "search_index", Predicate: "refresh_interval",
					Object: "shortened following the incident review"},
			}},
			Transcript: `{"role":"assistant","content":"search_index refresh_interval was shortened following the incident review meeting."}` + "\n",
		},

		// --- Category C: sibling negatives (same entity AND same object value, different
		// predicate; the sibling is never referenced and must not be credited). ---
		{
			Name: "C1_db_host_region_vs_capacity_tier",
			Claims: []heldOutClaim{
				{Category: "C", WantConsumed: true, Claim: claimFixture{
					Ref: "claim://claim_c1_referenced", Entity: "db_host", Predicate: "region",
					Object: "frankfurt datacenter primary",
				}},
				{Category: "C", WantConsumed: false, Claim: claimFixture{
					Ref: "claim://claim_c1_sibling", Entity: "db_host", Predicate: "capacity_tier",
					Object: "frankfurt datacenter primary",
				}},
			},
			Transcript: `{"role":"assistant","content":"Confirmed db_host region is the frankfurt datacenter primary site."}` + "\n",
		},
		{
			Name: "C2_payment_gateway_latency_vs_error_rate",
			Claims: []heldOutClaim{
				{Category: "C", WantConsumed: true, Claim: claimFixture{
					Ref: "claim://claim_c2_referenced", Entity: "payment_gateway", Predicate: "latency_p95",
					Object: "stable within sla bounds this month",
				}},
				{Category: "C", WantConsumed: false, Claim: claimFixture{
					Ref: "claim://claim_c2_sibling", Entity: "payment_gateway", Predicate: "error_rate",
					Object: "stable within sla bounds this month",
				}},
			},
			Transcript: `{"role":"assistant","content":"payment_gateway latency_p95 has stayed stable within sla bounds this month."}` + "\n",
		},

		// --- Category D: large padded ambient-scatter negatives (entity mentioned dozens of
		// times; predicate+object present but never within one window of each other). ---
		{
			Name: "D1_replica_set_scatter",
			Claims: []heldOutClaim{{
				Category: "D", WantConsumed: false,
				Claim: claimFixture{Ref: "claim://claim_d1", Entity: "replica_set", Predicate: "sync_lag",
					Object: "reduced significantly after the patch"},
			}},
			Transcript: `{"role":"assistant","content":"` +
				pad("replica_set stayed nominal throughout the whole rollout window. ", 40) +
				"meanwhile sync_lag was reported separately in a different channel. " +
				pad("replica_set kept behaving normally and nobody flagged anything odd. ", 40) +
				`patch notes were filed elsewhere and things reduced later on."}` + "\n",
		},
		{
			Name: "D2_notification_queue_scatter",
			Claims: []heldOutClaim{{
				Category: "D", WantConsumed: false,
				Claim: claimFixture{Ref: "claim://claim_d2", Entity: "notification_queue", Predicate: "backlog_depth",
					Object: "cleared after scaling workers up",
				},
			}},
			Transcript: `{"role":"assistant","content":"` +
				pad("notification_queue looked steady across every check we ran today. ", 40) +
				"separately, backlog_depth came up in the standup notes. " +
				pad("notification_queue remained unremarkable for the rest of the shift. ", 40) +
				`workers were scaling up and things eventually cleared."}` + "\n",
		},

		// --- Category E: self-echo negatives (content-key text ONLY inside its own injected
		// block) + a byte-exact requoted-digest variant elsewhere in the transcript. ---
		{
			Name: "E1_self_echo_only",
			Claims: []heldOutClaim{{
				Category: "E", WantConsumed: false,
				Claim: claimFixture{Ref: "claim://claim_e1", Entity: "auth_service", Predicate: "token_expiry",
					Object: "shortened to reduce replay risk exposure"},
			}},
			Transcript: `{"role":"system","content":"# Compound Context Injection\n- ` + "`claim://claim_e1`" +
				` source=generated reason=` + claimLedgerReason("auth_service", "token_expiry", "shortened to reduce replay risk exposure") +
				`"}` + "\n" +
				`{"role":"assistant","content":"Everything looks fine, no further action needed."}` + "\n",
		},
		{
			Name: "E2_requoted_digest",
			Claims: []heldOutClaim{{
				Category: "E", WantConsumed: false,
				Claim: claimFixture{Ref: "claim://claim_e2", Entity: "export_pipeline", Predicate: "throughput",
					Object: "dropped after the schema migration rollout"},
			}},
			Transcript: `{"role":"system","content":"# Compound Context Injection\n- ` + "`claim://claim_e2`" +
				` source=generated reason=` + claimLedgerReason("export_pipeline", "throughput", "dropped after the schema migration rollout") +
				`"}` + "\n" +
				`{"role":"assistant","content":"Recapping context: ` +
				claimContentLineText("export_pipeline", "throughput", "dropped after the schema migration rollout") +
				`"}` + "\n",
		},

		// --- Category F: low-specificity ineligible claims (content-key must stay dead-code for
		// these; no id anywhere either, so under-detection is the correct, safe outcome). ---
		{
			Name: "F1_thin_claim",
			Claims: []heldOutClaim{{
				Category: "F", WantConsumed: false,
				Claim: claimFixture{Ref: "claim://claim_f1", Entity: "ab", Predicate: "cd", Object: "ef"},
			}},
			Transcript: `{"role":"assistant","content":"ab cd ef, all good."}` + "\n",
		},
		{
			Name: "F2_thin_claim",
			Claims: []heldOutClaim{{
				Category: "F", WantConsumed: false,
				Claim: claimFixture{Ref: "claim://claim_f2", Entity: "io", Predicate: "ok", Object: "hi"},
			}},
			Transcript: `{"role":"assistant","content":"io ok hi, all good."}` + "\n",
		},

		// --- Category H: multilingual/paraphrase reference — an ACCEPTED miss, not a bug. ---
		{
			Name: "H1_turkish_paraphrase",
			Claims: []heldOutClaim{{
				Category: "H", WantConsumed: false,
				Claim: claimFixture{Ref: "claim://claim_h1", Entity: "onboarding_flow", Predicate: "completion_rate",
					Object: "dropped sharply after the redesign"},
			}},
			Transcript: `{"role":"assistant","content":"Kullanıcı tamamlama oranı yeniden tasarımdan sonra ciddi şekilde düştü."}` + "\n",
		},
		{
			Name: "H2_synonym_paraphrase_no_shared_tokens",
			Claims: []heldOutClaim{{
				Category: "H", WantConsumed: false,
				Claim: claimFixture{Ref: "claim://claim_h2", Entity: "checkout_flow", Predicate: "conversion_rate",
					Object: "increased to 12 percent this week"},
			}},
			Transcript: `{"role":"assistant","content":"The purchase completion percentage climbed by roughly a tenth over the last several days."}` + "\n",
		},
	}
}

// TestContentKeyHeldOutFalseCreditFloor is the packet's slice-3 gate (falsifier f): computes
// content-key-path precision over the held-out set above (denominator = content-key credits only,
// v1 hits excluded) and asserts it clears ContentKeyPrecisionFloor, AND asserts zero false credits
// in every category-C (sibling) and category-E (self-echo) fixture specifically, AND asserts every
// row's consumed verdict matches its ground truth (proves recall gain + no regression in one pass).
func TestContentKeyHeldOutFalseCreditFloor(t *testing.T) {
	var (
		contentKeyCredits         int
		contentKeyTruePositives   int
		contentKeyFalseCredits    int
		groundTruthPositives      int
		v1AloneDetectedPositives  int
		orDetectedPositives       int
		zeroToleranceFalseCredits []string
	)

	for _, fx := range heldOutFixtures() {
		injected := make([]injectedClaim, 0, len(fx.Claims))
		for _, hc := range fx.Claims {
			injected = append(injected, injectedClaim{
				Ref:  hc.Claim.Ref,
				Line: claimContentLine(claimLedgerReason(hc.Claim.Entity, hc.Claim.Predicate, hc.Claim.Object)),
			})
		}
		orConsumed := matchConsumedClaims(injected, fx.Transcript)
		orSet := map[string]bool{}
		for _, ref := range orConsumed {
			orSet[ref] = true
		}

		for _, hc := range fx.Claims {
			id := strings.TrimPrefix(hc.Claim.Ref, claimScheme)
			v1Fired := bareClaimReference(fx.Transcript, id)
			gotConsumed := orSet[hc.Claim.Ref]

			if gotConsumed != hc.WantConsumed {
				t.Errorf("%s claim %s (category %s): got consumed=%v, want=%v",
					fx.Name, hc.Claim.Ref, hc.Category, gotConsumed, hc.WantConsumed)
			}

			if hc.WantConsumed {
				groundTruthPositives++
				if v1Fired {
					v1AloneDetectedPositives++
				}
				if gotConsumed {
					orDetectedPositives++
				}
			}

			// A content-key CREDIT is a consumed verdict NOT explained by v1 alone -- this is
			// exactly the packet's "denominator = content-key credits only, v1 hits excluded".
			contentKeyFired := gotConsumed && !v1Fired
			if !contentKeyFired {
				continue
			}
			contentKeyCredits++
			if hc.WantConsumed {
				contentKeyTruePositives++
				continue
			}
			contentKeyFalseCredits++
			t.Errorf("%s claim %s (category %s): FALSE content-key credit (ground truth says NOT consumed)",
				fx.Name, hc.Claim.Ref, hc.Category)
			if hc.Category == "C" || hc.Category == "E" {
				zeroToleranceFalseCredits = append(zeroToleranceFalseCredits, fx.Name+"/"+hc.Claim.Ref)
			}
		}
	}

	if len(zeroToleranceFalseCredits) > 0 {
		t.Fatalf("zero-tolerance categories (sibling-negative C, self-echo E) had false credits: %v", zeroToleranceFalseCredits)
	}
	if contentKeyCredits == 0 {
		t.Fatalf("held-out set produced zero content-key credits -- the positive fixtures (category B) are not exercising the v2 path at all")
	}

	precision := float64(contentKeyTruePositives) / float64(contentKeyCredits)
	t.Logf("content-key credits=%d true_positives=%d false_credits=%d precision=%.4f (floor=%.2f)",
		contentKeyCredits, contentKeyTruePositives, contentKeyFalseCredits, precision, ContentKeyPrecisionFloor)
	t.Logf("recall: ground_truth_positives=%d v1_alone=%d (%.4f) v1_or_content_key=%d (%.4f)",
		groundTruthPositives, v1AloneDetectedPositives, float64(v1AloneDetectedPositives)/float64(groundTruthPositives),
		orDetectedPositives, float64(orDetectedPositives)/float64(groundTruthPositives))

	if precision < ContentKeyPrecisionFloor {
		t.Fatalf("content-key precision %.4f below floor %.2f -- per the packet, this match path must stay dead-code-gated, not shipped live, until it clears the floor; never loosen the floor to pass",
			precision, ContentKeyPrecisionFloor)
	}
}

// TestContentKeyHeldOutMissingTranscriptFailsOpen is category G: a missing/torn transcript still
// yields (nil, declared=false), never blocking session close -- re-verified here alongside the
// held-out set (already proven independently by TestSessionEndAbsentTranscriptFailsOpen).
func TestContentKeyHeldOutMissingTranscriptFailsOpen(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	claim := claimFixture{Ref: "claim://claim_g1", Entity: "cache_layer", Predicate: "warmup_time",
		Object: "reduced after prefetching enabled"}
	writeConsumePackWithClaims(t, home, "ctx_g1", claim)
	wid := closeableWork(t, store, "ctx_g1")

	consumed, declared := detectSessionConsumption(deps, Fixture{WorkID: wid, TranscriptPath: "does-not-exist.jsonl"})
	if declared {
		t.Fatalf("expected declared=false (fail-open) for a missing transcript, got declared=true consumed=%v", consumed)
	}
	if consumed != nil {
		t.Fatalf("expected nil consumed for a missing transcript, got %v", consumed)
	}
}
