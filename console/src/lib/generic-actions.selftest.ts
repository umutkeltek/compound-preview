import {
  adaptAttentionProjectionForArea,
  GENERIC_ACTION_ALLOWLIST,
  genericActionEffectKind,
  genericActionProjectionEligible,
  genericActionRevision,
  legacyAuthorityCutoverGuardFromRead,
  reviewResolutionRevision,
  resolveGenericActionFromProjection,
  resolveGenericActionFromRead,
} from "./generic-actions";
import { parseActionAuthorityBundle } from "./action-authority";

/**
 * The depth test for Phase 3a (Plans/i-am-not-happy-rosy-floyd.md Part 4):
 * proves the three-gate generic-action executor actually gates, not just
 * typechecks. `scripts/generic-actions-smoke.mjs` runs this under `bun`
 * (generic-actions.ts imports `@/lib/decision-registry` via the `@/` path
 * alias, which plain `node --experimental-strip-types` cannot resolve; bun
 * resolves it natively from tsconfig paths).
 *
 * Every case here runs `resolveGenericActionFromProjection` against a
 * crafted, in-memory projection fixture — no on-disk compound runtime home,
 * no subprocess, no network. That is the point: an attacker (or a bug) gets
 * exactly one shot at each gate, and none of them should ever require a real
 * backend to prove.
 */
export type SelfTestResult = { ok: boolean; details: string[] };

function authorityAction(overrides: Record<string, unknown> = {}) {
  const scope = { entity_id: "review_fixture", project_id: "compound", surface: "reviews" };
  const target = { kind: "review_artifact", id: "review_fixture" };
  const actionIntent = {
    schema_version: "compound.action_intent.v0.1",
    id: "act_authority_fixture",
    created_at: "2026-07-17T12:00:00Z",
    producer: { kind: "cli", name: "compound", version: "fixture" },
    ids: { work_id: "work_authority_fixture" },
    scope,
    authority: { source_class: "runtime", status: "current" },
    privacy: "private",
    risk: "tier1",
    evidence_refs: ["evidence:authority-fixture"],
    uncertainty: { level: "known" },
    actor: "operator-console",
    capability: "review-record",
    action: "record current review",
    target,
    mutation_class: "local_reversible",
    reversibility: "easy",
    blast_radius: "local_reversible",
    estimated_cost: { usd: 0, tokens: 0, human_review_minutes: 1 },
    rollback_plan: "supersede the review record",
  };
  const policyDecision = {
    schema_version: "compound.policy_decision.v0.1",
    id: "pol_authority_fixture",
    created_at: "2026-07-17T12:00:01Z",
    producer: { kind: "policy", name: "compound", version: "fixture" },
    ids: { work_id: "work_authority_fixture" },
    scope,
    authority: { source_class: "runtime", status: "current" },
    privacy: "private",
    risk: "tier1",
    evidence_refs: ["evidence:policy-fixture"],
    uncertainty: { level: "known" },
    action_intent_id: "act_authority_fixture",
    outcome: "allow",
    reasons: ["standing scope covers the exact target"],
    obligations: [],
    standing_grant_id: "grant_fixture",
  };
  const policyAuthority = {
    schema_version: "compound.policy_authority_face.v0.1",
    revision: `policy-authority-v1:sha256:${"a".repeat(64)}`,
    current: true,
    allowed: true,
    wording: "Allowed by current policy",
    action_intent_id: "act_authority_fixture",
    policy_decision_id: "pol_authority_fixture",
    policy_outcome: "allow",
    standing_grant_id: "grant_fixture",
    target,
    target_scope: "private",
    blast_radius: "local_reversible",
    reversibility: "easy",
    verified_recoverability: "verified",
    operator_control_continuity: "continuous",
    policy_source: "fixture-policy",
    exact_gate: "standing_scope",
    expected_receipt: "review receipt",
    expected_proof: ["proof:review-recorded"],
  };
  return {
    id: "record_fixture",
    label: "Record fixture",
    argv: ["compound", "authority", "dirty-review", "/tmp/fixture.review.yaml"],
    requires_review: true,
    action_intent: actionIntent,
    policy_decision: policyDecision,
    policy_authority: policyAuthority,
    ...overrides,
  };
}

export function runGenericActionsSelfTest(): SelfTestResult {
  const details: string[] = [];
  let ok = true;

  const fail = (msg: string) => {
    ok = false;
    details.push(`FAIL: ${msg}`);
  };
  const pass = (msg: string) => details.push(`ok: ${msg}`);

  // ObjectFace authority is one exact Go-owned bundle. The console validates
  // linkage and carries the opaque revision; it never reconstructs policy.
  {
    const valid = parseActionAuthorityBundle(authorityAction());
    const crossLinked = authorityAction();
    (crossLinked.policy_decision as Record<string, unknown>).action_intent_id = "act_other";
    const malformed = authorityAction();
    (malformed.policy_authority as Record<string, unknown>).revision = "client-recomputed";
    const missingDisplay = authorityAction();
    delete (missingDisplay.action_intent as Record<string, unknown>).blast_radius;
    delete (missingDisplay.policy_authority as Record<string, unknown>).blast_radius;
    const partial = authorityAction();
    delete (partial as Record<string, unknown>).policy_decision;
    if (!valid) {
      fail("complete current authority bundle was rejected");
    } else if (
      valid.actionIntentId !== "act_authority_fixture" ||
      valid.authorityRevision !== `policy-authority-v1:sha256:${"a".repeat(64)}` ||
      valid.wording !== "Allowed by current policy" ||
      valid.rollbackPlan !== "supersede the review record" ||
      valid.expectedReceipt !== "review receipt" ||
      valid.expectedProof.join(",") !== "proof:review-recorded"
    ) {
      fail(`authority bundle lost the Go display contract: ${JSON.stringify(valid)}`);
    } else if (
      parseActionAuthorityBundle(crossLinked) ||
      parseActionAuthorityBundle(malformed) ||
      parseActionAuthorityBundle(missingDisplay) ||
      parseActionAuthorityBundle(partial)
    ) {
      fail("cross-linked, malformed, display-incomplete, or partial authority bundle was accepted");
    } else {
      pass("authority parser preserves exact display fields and rejects partial/cross-linked/display-incomplete bundles");
    }
  }

  // The armed legacy cutover distinguishes absence from broken authority:
  // only a live concrete item with no owner action may use the old form.
  {
    const area = "source-control-cleanup";
    const itemId = "scr_cutover_fixture";
    const readWith = (action: unknown, status: "live" | "stale" = "live") => ({
      status,
      value: { items: [{ id: itemId, area, actions: action == null ? [] : [action] }] },
      error: status === "live" ? null : "owner refresh failed",
    });
    const valid = authorityAction({ id: "record_source_control_cleanup_review" });
    const malformed = structuredClone(valid);
    delete (malformed as Record<string, unknown>).policy_authority;
    const nonCurrent = structuredClone(valid);
    (nonCurrent.policy_authority as Record<string, unknown>).current = false;
    const held = structuredClone(valid);
    (held.policy_decision as Record<string, unknown>).outcome = "hold";
    (held.policy_decision as Record<string, unknown>).standing_grant_id = "";
    (held.policy_authority as Record<string, unknown>).allowed = false;
    (held.policy_authority as Record<string, unknown>).policy_outcome = "hold";
    (held.policy_authority as Record<string, unknown>).standing_grant_id = "";

    const absent = legacyAuthorityCutoverGuardFromRead(readWith(null), area, itemId);
    const present = legacyAuthorityCutoverGuardFromRead(readWith(valid), area, itemId);
    const broken = legacyAuthorityCutoverGuardFromRead(readWith(malformed), area, itemId);
    const old = legacyAuthorityCutoverGuardFromRead(readWith(nonCurrent), area, itemId);
    const heldGuard = legacyAuthorityCutoverGuardFromRead(readWith(held), area, itemId);
    const stale = legacyAuthorityCutoverGuardFromRead(readWith(valid, "stale"), area, itemId);
    if (
      !absent.ok ||
      present.ok || present.rejection.gate !== "owner-action-present" ||
      broken.ok || broken.rejection.gate !== "authority-unavailable" ||
      old.ok || old.rejection.gate !== "authority-unavailable" ||
      heldGuard.ok || heldGuard.rejection.gate !== "owner-action-present" ||
      stale.ok || stale.rejection.gate !== "projection-unavailable"
    ) {
      fail(`legacy cutover guard drifted: ${JSON.stringify({ absent, present, broken, old, heldGuard, stale })}`);
    } else {
      pass("legacy cutover allows absent action and fails closed on present/malformed/non-current/held/stale owner truth");
    }
  }

  // (a) Gate 1 — a shell-form action (argv[0] !== "compound") must be
  // rejected regardless of area, exactly like panel per-item "/bin/sh -lc
  // ..." forms stay copy-only forever.
  {
    const projection = {
      items: [
        {
          id: "shell_item",
          actions: [{ id: "danger", argv: ["/bin/sh", "-lc", "echo pwned"] }],
        },
      ],
    };
    const res = resolveGenericActionFromProjection(projection, "job-health", "shell_item", "danger");
    if (res.ok) fail("shell-form argv resolved ok:true instead of being rejected");
    else if (res.rejection.gate !== "gate-1-shell-form")
      fail(`shell-form argv rejected at "${res.rejection.gate}" instead of gate-1-shell-form`);
    else pass('shell-form argv (argv[0]="/bin/sh") rejected at gate-1-shell-form');
  }

  // (b) Gate 2 — an area with NO entry in GENERIC_ACTION_ALLOWLIST must be
  // rejected even with an otherwise clean compound-prefixed argv.
  {
    const uncertifiedArea = "totally-uncertified-area";
    if (GENERIC_ACTION_ALLOWLIST[uncertifiedArea]) {
      fail(`fixture area "${uncertifiedArea}" is unexpectedly allowlisted -- update the test`);
    } else {
      const projection = {
        items: [
          { id: "it1", actions: [{ id: "act1", argv: ["compound", "work", "close", "work_x"] }] },
        ],
      };
      const res = resolveGenericActionFromProjection(projection, uncertifiedArea, "it1", "act1");
      if (res.ok) fail("un-allowlisted area resolved ok:true instead of being rejected");
      else if (res.rejection.gate !== "gate-2-allowlist")
        fail(`un-allowlisted area rejected at "${res.rejection.gate}" instead of gate-2-allowlist`);
      else pass("un-allowlisted area rejected at gate-2-allowlist");
    }
  }

  // (c) Gate 2 — an allowlisted area whose resolved verb does not match any
  // of that area's certified prefixes must ALSO fail at gate 2 -- proves the
  // check is area+verb together, not just area presence.
  {
    const area = "job-health";
    if (!GENERIC_ACTION_ALLOWLIST[area]) {
      fail(`fixture assumes "${area}" is allowlisted but GENERIC_ACTION_ALLOWLIST disagrees -- update the test`);
    } else {
      const projection = {
        items: [
          {
            id: "it2",
            // Real compound verb ("authority dirty-review"), but not one of
            // job-health's certified prefixes (["authority","job-health-review"]).
            actions: [{ id: "act2", argv: ["compound", "authority", "dirty-review", "x.yaml"] }],
          },
        ],
      };
      const res = resolveGenericActionFromProjection(projection, area, "it2", "act2");
      if (res.ok) fail("mismatched verb for an allowlisted area resolved ok:true instead of being rejected");
      else if (res.rejection.gate !== "gate-2-allowlist")
        fail(`mismatched verb rejected at "${res.rejection.gate}" instead of gate-2-allowlist`);
      else pass('allowlisted area ("job-health") with a mismatched verb rejected at gate-2-allowlist');
    }
  }

  // (d) A well-formed, allowlisted action resolves cleanly with the argv read
  // straight from the (mocked) projection -- proves the happy path actually
  // works end to end, not just that gates reject.
  {
    const area = "job-health";
    const projection = {
      items: [
        {
          id: "jhp_test123456",
          actions: [
            authorityAction({
              id: "record_job_health_review",
              label: "Record job-health review",
              argv: ["compound", "authority", "job-health-review", "/tmp/fixture.review.yaml"],
              requires_review: true,
              policy_note: "records a reviewed job-health decision; no roster mutation",
            }),
          ],
        },
      ],
    };
    const res = resolveGenericActionFromProjection(
      projection,
      area,
      "jhp_test123456",
      "record_job_health_review",
    );
    if (!res.ok) {
      fail(`well-formed allowlisted action was rejected: ${res.rejection.gate} - ${res.rejection.message}`);
    } else if (res.action.argv.join(" ") !== "authority job-health-review /tmp/fixture.review.yaml") {
      fail(`resolved argv unexpected: ${JSON.stringify(res.action.argv)}`);
    } else if (!res.action.requiresReview) {
      fail("resolved action lost requires_review:true from the projection");
    } else {
      pass("well-formed allowlisted action resolves with the freshly-read argv and requiresReview intact");
    }
  }

  // (e) An unknown item id or action id must come back not-found, never
  // throw and never silently pick a different action.
  {
    const res1 = resolveGenericActionFromProjection({ items: [] }, "job-health", "missing", "missing");
    if (res1.ok || res1.rejection.gate !== "not-found") {
      fail(`unknown item id should be not-found, got ${JSON.stringify(res1)}`);
    } else {
      pass("unknown item id rejected as not-found");
    }

    const projection = {
      items: [{ id: "jhp_known", actions: [{ id: "record_job_health_review", argv: ["compound", "authority", "job-health-review", "x.yaml"] }] }],
    };
    const res2 = resolveGenericActionFromProjection(projection, "job-health", "jhp_known", "not_a_real_action");
    if (res2.ok || res2.rejection.gate !== "not-found") {
      fail(`unknown action id on a known item should be not-found, got ${JSON.stringify(res2)}`);
    } else {
      pass("unknown action id on a known item rejected as not-found (no fallback to another action)");
    }
  }

  // (f) A group-scoped safe_batch_action resolves the same way an item's
  // actions[] does, keyed by group.key instead of item.id -- proves the
  // "itemId is a group key" path the spec describes actually works, even
  // though none of the currently-allowlisted areas emit groups today.
  {
    const area = "job-health";
    const projection = {
      groups: [
        {
          key: "job-health|batch-key",
          safe_batch_action: authorityAction({
            id: "record_job_health_review_batch",
            argv: ["compound", "authority", "job-health-review", "/tmp/batch.review.yaml"],
            requires_review: true,
          }),
        },
      ],
    };
    const res = resolveGenericActionFromProjection(
      projection,
      area,
      "job-health|batch-key",
      "record_job_health_review_batch",
    );
    if (!res.ok) {
      fail(`group-scoped safe_batch_action should resolve, got ${JSON.stringify(res)}`);
    } else {
      pass("group-scoped safe_batch_action resolves when itemId is a group key");
    }
  }

  // Rationale-required areas (route-promotions / panel-verdicts): the CLI
  // requires --rationale, the projection never carries it, and the operator's
  // text is appended as its own argv element post-gates.
  {
    const projection = {
      items: [
        {
          id: "route_promotion_clean",
          actions: [
            authorityAction({
              id: "route_promotion_record",
              argv: ["compound", "route", "promotion-record", "--review", "/tmp/rp.json"],
              requires_review: true,
            }),
          ],
        },
      ],
    };
    const none = resolveGenericActionFromProjection(
      projection,
      "route-promotions",
      "route_promotion_clean",
      "route_promotion_record",
    );
    if (none.ok || none.rejection.gate !== "rationale-required") {
      fail(`missing rationale should reject at rationale-required, got ${JSON.stringify(none)}`);
    } else {
      pass("rationale-required area without rationale rejected at rationale-required");
    }
    const quirkyRationale = "held: day-2 bakeoff decides; '; $(echo injected) is just text here";
    const withText = resolveGenericActionFromProjection(
      projection,
      "route-promotions",
      "route_promotion_clean",
      "route_promotion_record",
      quirkyRationale,
    );
    if (!withText.ok) {
      fail(`rationale-supplied resolution should pass, got ${JSON.stringify(withText)}`);
    } else {
      const argv = withText.action.argv;
      const idx = argv.indexOf("--rationale");
      if (idx === -1 || argv[idx + 1] !== quirkyRationale) {
        fail(`rationale must be appended verbatim as one argv element, got ${JSON.stringify(argv)}`);
      } else {
        pass("operator rationale appended verbatim as a single argv element (no shell)");
      }
    }
    const placeholder = resolveGenericActionFromProjection(
      {
        items: [
          {
            id: "route_promotion_placeholder",
            actions: [
              {
                id: "route_promotion_record",
                argv: [
                  "compound",
                  "route",
                  "promotion-record",
                  "--review",
                  "/tmp/rp.json",
                  "--rationale",
                  "$RATIONALE",
                ],
                requires_review: true,
              },
            ],
          },
        ],
      },
      "route-promotions",
      "route_promotion_placeholder",
      "route_promotion_record",
      "real rationale",
    );
    if (placeholder.ok) {
      fail("projected argv already carrying --rationale must be refused (placeholder risk)");
    } else {
      pass("projected argv with a pre-baked --rationale refused (placeholder risk)");
    }
  }

  // product-hypotheses (work_6ede073fe356): the hypothesis approve verb. The
  // Go projection (internal/product/review_queue.go) emits argv WITHOUT
  // --expected-effect; the operator's sentence is required input appended as
  // ["--expected-effect", text] exactly like the rationale areas.
  {
    const argv = ["compound", "product", "hypothesis-approve", "hyp_draft_synth01"];
    const projection = {
      items: [
        {
          id: "php_synth01",
          candidate_id: "hyp_draft_synth01",
          actions: [authorityAction({ id: "product_hypothesis_approve", argv })],
        },
      ],
    };
    if (!genericActionProjectionEligible("product-hypotheses", argv)) {
      fail("product-hypotheses approve argv must be projection-eligible after allowlist certification");
    } else {
      pass("product-hypotheses approve argv certified by both allowlists");
    }
    const none = resolveGenericActionFromProjection(projection, "product-hypotheses", "php_synth01", "product_hypothesis_approve");
    if (none.ok || none.rejection.gate !== "operator-input-required") {
      fail(`missing expected effect should reject at operator-input-required, got ${JSON.stringify(none)}`);
    } else {
      pass("product-hypotheses without operator input rejected at operator-input-required");
    }
    const sentence = "synthetic first-time downloads per 1000 impressions should rise at least 10%; '; $(echo injected) is just text";
    const withText = resolveGenericActionFromProjection(projection, "product-hypotheses", "php_synth01", "product_hypothesis_approve", sentence);
    if (!withText.ok) {
      fail(`expected-effect-supplied resolution should pass, got ${JSON.stringify(withText)}`);
    } else {
      const got = withText.action.argv;
      const idx = got.indexOf("--expected-effect");
      if (idx === -1 || got[idx + 1] !== sentence) {
        fail(`expected effect must be appended verbatim as one argv element, got ${JSON.stringify(got)}`);
      } else {
        pass("operator expected effect appended verbatim as a single argv element (no shell)");
      }
    }
    const prebaked = resolveGenericActionFromProjection(
      {
        items: [
          {
            id: "php_prebaked",
            actions: [
              authorityAction({
                id: "product_hypothesis_approve",
                argv: [...argv, "--expected-effect", "OPERATOR MUST WRITE: what should X do after Y?"],
              }),
            ],
          },
        ],
      },
      "product-hypotheses",
      "php_prebaked",
      "product_hypothesis_approve",
      "real sentence",
    );
    if (prebaked.ok) {
      fail("projected argv already carrying --expected-effect must be refused (placeholder risk)");
    } else {
      pass("projected argv with a pre-baked --expected-effect refused (placeholder risk)");
    }
  }

  if (
    genericActionEffectKind("job-health") === "record" &&
    genericActionEffectKind("panel-verdicts") === "record" &&
    genericActionEffectKind("observation-candidates") === "track" &&
    genericActionEffectKind("unknown-area") === null
  ) {
    pass("generic action effect labels distinguish certified record/track semantics and reject unknown areas");
  } else {
    fail("generic action effect labels drifted from certified area semantics");
  }

  // Object-prototype names are not certified areas. A plain Record lookup can
  // accidentally inherit toString/constructor/__proto__; every allowlist and
  // effect lookup must require an own property.
  {
    const projection = {
      items: [{
        id: "prototype_area_fixture",
        actions: [{
          id: "record_job_health_review",
          argv: ["compound", "authority", "job-health-review", "/tmp/fixture.review.yaml"],
        }],
      }],
    };
    for (const area of ["toString", "constructor", "__proto__"]) {
      const resolved = resolveGenericActionFromProjection(
        projection,
        area,
        "prototype_area_fixture",
        "record_job_health_review",
      );
      if (
        genericActionProjectionEligible(area, ["compound", "authority", "job-health-review", "/tmp/fixture.review.yaml"]) ||
        genericActionEffectKind(area) !== null ||
        resolved.ok ||
        resolved.rejection.gate !== "gate-2-allowlist"
      ) {
        fail(`prototype-shaped area ${area} escaped the own-property allowlist guard: ${JSON.stringify(resolved)}`);
      } else {
        pass(`prototype-shaped area ${area} rejected at the own-property allowlist seam`);
      }
    }
  }

  // The console carries the opaque Go-owned QueueItem fingerprint exactly.
  // Missing, malformed, or cross-area tokens fail closed; no selective local
  // reconstruction can silently bless a changed owner projection.
  {
    const ownerRevision = `review-resolution-v2:sha256:${"a".repeat(64)}`;
    const item = {
      id: "parity_revision_fixture",
      area: "parity",
      status: "unknown",
      operator_summary: "owner-rendered summary",
      review_resolution_revision: ownerRevision,
    };
    const carried = reviewResolutionRevision("parity", item);
    const changedOwnerRevision = reviewResolutionRevision("parity", {
      ...item,
      readiness: "ready_to_apply",
      review_resolution_revision: `review-resolution-v2:sha256:${"b".repeat(64)}`,
    });
    if (
      carried === ownerRevision &&
      changedOwnerRevision !== carried &&
      reviewResolutionRevision("source", item) === "" &&
      reviewResolutionRevision("parity", { ...item, area: "" }) === "" &&
      reviewResolutionRevision("parity", { ...item, review_resolution_revision: "review-resolution-v1:legacy" }) === "" &&
      reviewResolutionRevision("parity", { ...item, review_resolution_revision: "" }) === ""
    ) {
      pass("review resolution carries only the scoped, privacy-safe Go owner revision");
    } else {
      fail(`review resolution owner token did not fail closed: carried=${carried} changed=${changedOwnerRevision}`);
    }
  }

  // F4/F5: the lease-hygiene attention-sourced pathway. `compound attention
  // --json` groups[] carry a plain `action` field (not `safe_batch_action`
  // keyed items[]) -- adaptAttentionProjectionForArea() reshapes that into
  // the {groups:[{key, safe_batch_action}]} projection
  // resolveGenericActionFromProjection already understands, so the same
  // three gates apply without a bespoke reviews area existing for lease
  // hygiene.
  {
    const leaseHygieneAction = authorityAction({
      id: "lease-hygiene.sweep",
      label: "Sweep leftover claim tags",
      argv: ["compound", "home", "archive-history", "--kind", "lease"],
      kind: "batch",
      requires_confirmation: true,
    });
    const attentionFixture = {
      groups: [
        { key: "lease-hygiene", area: "lease-hygiene", action: leaseHygieneAction },
        {
          key: "executor:codex",
          area: "job-health",
          action: null,
        },
      ],
    };

    // (g) The happy path: a well-formed attention-sourced action resolves ok
    // with the exact projected argv and a stable, non-empty revision.
    {
      const projection = adaptAttentionProjectionForArea(attentionFixture, "lease-hygiene");
      const res = resolveGenericActionFromProjection(
        projection,
        "lease-hygiene",
        "lease-hygiene",
        "lease-hygiene.sweep",
      );
      const res2 = resolveGenericActionFromProjection(
        projection,
        "lease-hygiene",
        "lease-hygiene",
        "lease-hygiene.sweep",
      );
      if (!res.ok) {
        fail(`lease-hygiene sweep action was rejected: ${res.rejection.gate} - ${res.rejection.message}`);
      } else if (res.action.argv.join(" ") !== "home archive-history --kind lease") {
        fail(`lease-hygiene resolved argv unexpected: ${JSON.stringify(res.action.argv)}`);
      } else if (!res.action.requiresReview) {
        fail("lease-hygiene resolved action lost requires_review from the attention action's requires_confirmation");
      } else if (!res.action.revision || !res2.ok || res.action.revision !== res2.action.revision) {
        fail(`lease-hygiene revision must be stable and non-empty across identical resolutions, got ${JSON.stringify([res, res2])}`);
      } else {
        pass("lease-hygiene attention-sourced action resolves ok with the exact argv and a stable revision");
      }
    }

    // (h) A forged action argv (a destructive verb the operator never saw
    // certified) must be rejected at gate-2 or gate-3, even keyed under the
    // real lease-hygiene area/key/actionId.
    {
      const forgedFixture = {
        groups: [
          {
            key: "lease-hygiene",
            area: "lease-hygiene",
            action: { ...leaseHygieneAction, argv: ["compound", "home", "restore-history"] },
          },
        ],
      };
      const projection = adaptAttentionProjectionForArea(forgedFixture, "lease-hygiene");
      const res = resolveGenericActionFromProjection(
        projection,
        "lease-hygiene",
        "lease-hygiene",
        "lease-hygiene.sweep",
      );
      if (res.ok) {
        fail("forged lease-hygiene argv (home restore-history) resolved ok:true instead of being rejected");
      } else if (res.rejection.gate !== "gate-2-allowlist" && res.rejection.gate !== "gate-3-global-allowlist") {
        fail(`forged lease-hygiene argv rejected at "${res.rejection.gate}" instead of gate-2/gate-3`);
      } else {
        pass(`forged lease-hygiene argv (home restore-history) rejected at ${res.rejection.gate}`);
      }

      // (i) The SAME forged argv under an uncertified area must also reject
      // at gate-2 -- proves certification is per-area, not a global bypass.
      const uncertifiedArea = "totally-uncertified-attention-area";
      if (GENERIC_ACTION_ALLOWLIST[uncertifiedArea]) {
        fail(`fixture area "${uncertifiedArea}" is unexpectedly allowlisted -- update the test`);
      } else {
        const uncertifiedProjection = adaptAttentionProjectionForArea(
          { groups: [{ key: "lease-hygiene", area: uncertifiedArea, action: { ...leaseHygieneAction, argv: ["compound", "home", "restore-history"] } }] },
          uncertifiedArea,
        );
        const res2 = resolveGenericActionFromProjection(
          uncertifiedProjection,
          uncertifiedArea,
          "lease-hygiene",
          "lease-hygiene.sweep",
        );
        if (res2.ok || res2.rejection.gate !== "gate-2-allowlist") {
          fail(`forged argv under an uncertified area should reject at gate-2-allowlist, got ${JSON.stringify(res2)}`);
        } else {
          pass("forged argv under an uncertified area rejected at gate-2-allowlist");
        }
      }
    }
  }

  // A3 (packet 2026-07-15-slice2A-skill-surfacing-verb): the skill-surfacing
  // area reads from `compound reviews --json --area skill-surfacing` (a
  // plain items[].actions[] projection, unlike lease-hygiene's
  // attention-adapted groups[]) -- proves the certified area resolves a
  // well-formed import action with a stable revision, and rejects a forged
  // argv for a real compound verb that is not skill-surfacing's certified
  // prefix.
  {
    const openItemPath = "/home/.compound/reviews/skill-surfacing/2026-07-09-session-mining-backfill-feedback.json";
    const projection = {
      items: [
        {
          id: "ssfb_20260709_session_backfill",
          area: "skill-surfacing",
          actions: [
            authorityAction({
              id: "skill-surfacing.import",
              label: "Fold these misses into the capability graph",
              argv: ["compound", "capability", "import-surfacing-misses", openItemPath],
              requires_review: true,
            }),
          ],
        },
      ],
    };

    // (j) The happy path: a well-formed skill-surfacing import action
    // resolves ok with the exact projected argv and a stable, non-empty
    // revision.
    {
      const res = resolveGenericActionFromProjection(
        projection,
        "skill-surfacing",
        "ssfb_20260709_session_backfill",
        "skill-surfacing.import",
      );
      const res2 = resolveGenericActionFromProjection(
        projection,
        "skill-surfacing",
        "ssfb_20260709_session_backfill",
        "skill-surfacing.import",
      );
      if (!res.ok) {
        fail(`skill-surfacing import action was rejected: ${res.rejection.gate} - ${res.rejection.message}`);
      } else if (res.action.argv.join(" ") !== `capability import-surfacing-misses ${openItemPath}`) {
        fail(`skill-surfacing resolved argv unexpected: ${JSON.stringify(res.action.argv)}`);
      } else if (!res.action.requiresReview) {
        fail("skill-surfacing resolved action lost requires_review from the projection");
      } else if (!res.action.revision || !res2.ok || res.action.revision !== res2.action.revision) {
        fail(`skill-surfacing revision must be stable and non-empty across identical resolutions, got ${JSON.stringify([res, res2])}`);
      } else {
        pass("skill-surfacing import action resolves ok with the exact argv and a stable revision");
      }
    }

    // (k) A forged action argv -- a real compound verb ("capability
    // skill-plan") that is NOT skill-surfacing's certified prefix -- must be
    // rejected at gate-2, even keyed under the real item/action id.
    {
      const forgedProjection = {
        items: [
          {
            id: "ssfb_20260709_session_backfill",
            area: "skill-surfacing",
            actions: [
              {
                id: "skill-surfacing.import",
                argv: ["compound", "capability", "skill-plan"],
                requires_review: true,
              },
            ],
          },
        ],
      };
      const res = resolveGenericActionFromProjection(
        forgedProjection,
        "skill-surfacing",
        "ssfb_20260709_session_backfill",
        "skill-surfacing.import",
      );
      if (res.ok) {
        fail("forged skill-surfacing argv (capability skill-plan) resolved ok:true instead of being rejected");
      } else if (res.rejection.gate !== "gate-2-allowlist") {
        fail(`forged skill-surfacing argv rejected at "${res.rejection.gate}" instead of gate-2-allowlist`);
      } else {
        pass("forged skill-surfacing argv (capability skill-plan) rejected at gate-2-allowlist");
      }
    }
  }

  // F5: the client-side honesty probe agrees with the certified allowlist
  // for the lease-hygiene area.
  {
    const eligible = genericActionProjectionEligible("lease-hygiene", [
      "compound",
      "home",
      "archive-history",
      "--kind",
      "lease",
    ]);
    const wrongShellForm = genericActionProjectionEligible("lease-hygiene", [
      "home",
      "archive-history",
      "--kind",
      "lease",
    ]);
    const wrongVerb = genericActionProjectionEligible("lease-hygiene", ["compound", "home", "restore-history"]);
    if (eligible && !wrongShellForm && !wrongVerb) {
      pass("genericActionProjectionEligible agrees with the lease-hygiene allowlist: certified argv true, argv[0]!=='compound' and wrong verb both false");
    } else {
      fail(
        `genericActionProjectionEligible drifted for lease-hygiene: eligible=${eligible} wrongShellForm=${wrongShellForm} wrongVerb=${wrongVerb}`,
      );
    }
  }

  // A3: the client-side honesty probe agrees with the certified allowlist
  // for the skill-surfacing area.
  {
    const eligible = genericActionProjectionEligible("skill-surfacing", [
      "compound",
      "capability",
      "import-surfacing-misses",
      "/home/.compound/reviews/skill-surfacing/2026-07-09-session-mining-backfill-feedback.json",
    ]);
    const wrongVerb = genericActionProjectionEligible("skill-surfacing", ["compound", "capability", "skill-plan"]);
    if (eligible && !wrongVerb) {
      pass("genericActionProjectionEligible agrees with the skill-surfacing allowlist: certified argv true, wrong verb false");
    } else {
      fail(`genericActionProjectionEligible drifted for skill-surfacing: eligible=${eligible} wrongVerb=${wrongVerb}`);
    }
  }

  // B5: the harness-edit-proposals area. internal/director annotates each open
  // harness-edit card with ONE action {id:"harness-edit.whitelist", argv:
  // ["compound","authority","harness-edit-review",<card>,"--decision",
  // "whitelist","--json"]} -- the CLI REQUIRES --rationale, so the projected
  // argv deliberately omits it and the operator's text is appended post-gates.
  {
    const area = "harness-edit-proposals";
    const projection = {
      items: [
        {
          id: "dirprop_selftest",
          actions: [
            authorityAction({
              id: "harness-edit.whitelist",
              label: "Allow edits to this surface",
              argv: [
                "compound",
                "authority",
                "harness-edit-review",
                "/tmp/reviews/harness-edit-proposals/dirprop_selftest.json",
                "--decision",
                "whitelist",
                "--json",
              ],
              requires_review: true,
            }),
          ],
        },
      ],
    };

    // (b5-a) Without a rationale the CLI-required flag is missing -> rejected at
    // rationale-required (NOT silently run with a placeholder).
    const none = resolveGenericActionFromProjection(
      projection,
      area,
      "dirprop_selftest",
      "harness-edit.whitelist",
    );
    if (none.ok || none.rejection.gate !== "rationale-required") {
      fail(`harness-edit whitelist without rationale should reject at rationale-required, got ${JSON.stringify(none)}`);
    } else {
      pass("harness-edit-proposals without rationale rejected at rationale-required");
    }

    // (b5-b) With a rationale it resolves ok and the operator's text is the LAST
    // argv element (appended verbatim, never a shell string).
    const rationale = "agents may trim dead offers here; $(echo x) is just text";
    const withText = resolveGenericActionFromProjection(
      projection,
      area,
      "dirprop_selftest",
      "harness-edit.whitelist",
      rationale,
    );
    if (!withText.ok) {
      fail(`harness-edit whitelist with rationale should pass, got ${JSON.stringify(withText)}`);
    } else {
      const argv = withText.action.argv;
      if (argv[argv.length - 2] !== "--rationale" || argv[argv.length - 1] !== rationale) {
        fail(`harness-edit final argv must END with ["--rationale", <text>], got ${JSON.stringify(argv)}`);
      } else if (!argv.includes("--json") || argv.includes("--decision") === false) {
        fail(`harness-edit resolved argv lost its projected flags: ${JSON.stringify(argv)}`);
      } else {
        pass("harness-edit-proposals with rationale resolves ok; argv ends with [--rationale, <text>]");
      }
    }

    // (b5-c) A forged argv (a real but UNcertified verb for this area) keyed
    // under the same area/item/actionId is rejected at gate-2 -- certification is
    // per-area+verb, not "any authority subcommand".
    const forged = resolveGenericActionFromProjection(
      {
        items: [
          {
            id: "dirprop_selftest",
            actions: [
              {
                id: "harness-edit.whitelist",
                argv: ["compound", "authority", "dirty-review", "/tmp/x.yaml"],
                requires_review: true,
              },
            ],
          },
        ],
      },
      area,
      "dirprop_selftest",
      "harness-edit.whitelist",
      "rationale supplied",
    );
    if (forged.ok || forged.rejection.gate !== "gate-2-allowlist") {
      fail(`forged harness-edit argv (authority dirty-review) should reject at gate-2-allowlist, got ${JSON.stringify(forged)}`);
    } else {
      pass("forged harness-edit argv (authority dirty-review) rejected at gate-2-allowlist");
    }

    // (b5-d) The effect label + client-side honesty probe agree with the
    // certified allowlist for this area.
    if (genericActionEffectKind(area) !== "record") {
      fail(`harness-edit-proposals effect kind = ${genericActionEffectKind(area)}, want record`);
    } else if (
      !genericActionProjectionEligible(area, [
        "compound",
        "authority",
        "harness-edit-review",
        "/tmp/x.json",
        "--decision",
        "whitelist",
        "--json",
      ]) ||
      genericActionProjectionEligible(area, ["compound", "authority", "dirty-review", "/tmp/x.yaml"])
    ) {
      fail("genericActionProjectionEligible drifted for harness-edit-proposals");
    } else {
      pass("harness-edit-proposals effect kind is record and the eligibility probe agrees with the allowlist");
    }
  }

  // (j) The cold-cache honesty gate. The act path always resolves against a
  // cold read (decision-execute invalidates the read cache first), and
  // `compound attention --json` can outlast its timeout — an `unreachable`
  // read must reject as "the projection did not answer", NEVER as
  // "not-found". Collapsing the two was the cold-cache act 404: the operator
  // clicked a rendered, real button and was told the item did not exist.
  {
    const read = { status: "unreachable" as const, value: null, error: "compound command timed out" };
    const res = resolveGenericActionFromRead(read, "lease-hygiene", "lease-hygiene", "lease-hygiene.sweep");
    if (res.ok) {
      fail("unreachable projection read resolved ok:true instead of being rejected");
    } else if (res.rejection.gate !== "projection-unavailable") {
      fail(`unreachable projection read rejected at "${res.rejection.gate}" instead of projection-unavailable`);
    } else if (!res.rejection.message.includes("nothing was run")) {
      fail(`projection-unavailable message must say nothing ran, got: ${res.rejection.message}`);
    } else {
      pass("unreachable projection read rejects at projection-unavailable (slow door), never not-found");
    }
  }

  // B6: a decision-candidate review plan is an evidence-building owner action,
  // never an acceptance shortcut. The Decisions owner projects one bounded,
  // supervised panel-plan argv over the freshly resolved candidate id; both
  // generic allowlists must admit exactly that verb and keep stronger decision
  // source/import verbs out.
  {
    const area = "decision-candidates";
    const itemId = "dcp_decision_review_selftest";
    const actionId = "plan_decision_candidate_review";
    const argv = [
      "compound",
      "panel",
      "plan",
      "--target-kind",
      "decision_candidate",
      "--target",
      "cand_decision_review_selftest",
      "--domain",
      "reasoning-lenses",
      "--task",
      "review falsifier, review trigger, source ownership, and second-order effects",
      "--supervised",
      "--max-items",
      "1",
      "--json",
    ];
    const projection = {
      items: [{
        id: itemId,
        candidate_id: "cand_decision_review_selftest",
        actions: [authorityAction({
          id: actionId,
          label: "Plan decision review",
          argv,
          requires_review: true,
          policy_note: "creates review evidence only; decision source and authority stay unchanged",
        })],
      }],
    };
    const resolved = resolveGenericActionFromProjection(projection, area, itemId, actionId);
    if (!resolved.ok) {
      fail(`decision-candidate panel plan should resolve, got ${JSON.stringify(resolved)}`);
    } else if (
      resolved.action.argv.join("\u0000") !== argv.slice(1).join("\u0000") ||
      !resolved.action.requiresReview ||
      resolved.action.label !== "Plan decision review"
    ) {
      fail(`decision-candidate panel plan lost exact owner argv/confirmation: ${JSON.stringify(resolved.action)}`);
    } else {
      pass("decision-candidates resolves the exact bounded panel-plan owner action behind confirmation");
    }

    const forged = resolveGenericActionFromProjection(
      {
        items: [{
          id: itemId,
          actions: [{ id: actionId, argv: ["compound", "decisions", "import", "--source", "/tmp/DEC.md"] }],
        }],
      },
      area,
      itemId,
      actionId,
    );
    if (forged.ok || forged.rejection.gate !== "gate-2-allowlist") {
      fail(`decision-candidate source import must reject at gate-2-allowlist, got ${JSON.stringify(forged)}`);
    } else if (
      genericActionEffectKind(area) !== "track" ||
      !genericActionProjectionEligible(area, argv) ||
      genericActionProjectionEligible(area, ["compound", "decisions", "import", "--source", "/tmp/DEC.md"])
    ) {
      fail("decision-candidates effect label or client honesty probe drifted from the bounded panel-plan allowlist");
    } else {
      pass("decision-candidates is track-only; source import remains ineligible in both client and server gates");
    }

    const escalatedArgv = [
      "compound",
      "panel",
      "plan",
      "--target-kind",
      "memory_candidate",
      "--target",
      "mcand_private",
      "--domain",
      "reasoning-lenses",
      "--task",
      "review something else",
      "--unsupervised",
      "--max-items",
      "999",
      "--json",
    ];
    const sameVerbEscalation = resolveGenericActionFromProjection(
      {
        items: [{
          id: itemId,
          candidate_id: "cand_decision_review_selftest",
          actions: [{ id: actionId, argv: escalatedArgv }],
        }],
      },
      area,
      itemId,
      actionId,
    );
    if (
      sameVerbEscalation.ok ||
      sameVerbEscalation.rejection.gate !== "gate-2-allowlist" ||
      genericActionProjectionEligible(area, escalatedArgv)
    ) {
      fail(`decision-candidate same-verb flag escalation must reject at gate-2-allowlist, got ${JSON.stringify(sameVerbEscalation)}`);
    } else {
      pass("decision-candidates rejects same-verb target, supervision, task, and bound escalation");
    }

    const missingCandidateIdentity = resolveGenericActionFromProjection(
      {
        items: [{
          id: itemId,
          actions: [{ id: actionId, argv, requires_review: true }],
        }],
      },
      area,
      itemId,
      actionId,
    );
    if (missingCandidateIdentity.ok || missingCandidateIdentity.rejection.gate !== "gate-2-allowlist") {
      fail(`decision-candidate action without owner target identity must reject, got ${JSON.stringify(missingCandidateIdentity)}`);
    } else {
      pass("decision-candidates requires the owner-projected candidate identity before certifying a target");
    }

    const confirmationDowngrade = resolveGenericActionFromProjection(
      {
        items: [{
          id: itemId,
          candidate_id: "cand_decision_review_selftest",
          actions: [{ id: actionId, argv, requires_review: false }],
        }],
      },
      area,
      itemId,
      actionId,
    );
    if (confirmationDowngrade.ok || confirmationDowngrade.rejection.gate !== "gate-2-allowlist") {
      fail(`decision-candidate confirmation downgrade must reject, got ${JSON.stringify(confirmationDowngrade)}`);
    } else {
      pass("decision-candidates requires the owner-projected confirmation gate");
    }
  }

  // (k) Stale last-good context is display evidence only. It must never cross
  // the action seam: click-time execution requires a live owner read.
  {
    const read = {
      status: "stale" as const,
      value: {
        groups: [
          {
            key: "lease-hygiene",
            area: "lease-hygiene",
            action: {
              id: "lease-hygiene.sweep",
              label: "Sweep leftover claim tags",
              argv: ["compound", "home", "archive-history", "--kind", "lease"],
              kind: "batch",
              requires_confirmation: true,
            },
          },
        ],
      },
      error: "latest refresh attempt failed",
    };
    const res = resolveGenericActionFromRead(read, "lease-hygiene", "lease-hygiene", "lease-hygiene.sweep");
    if (res.ok) {
      fail(`stale read resolved executable argv: ${JSON.stringify(res.action.argv)}`);
    } else if (res.rejection.gate !== "projection-unavailable") {
      fail(`stale read rejected at ${res.rejection.gate}, want projection-unavailable`);
    } else {
      pass("stale last-good context cannot resolve an executable action");
    }
  }

  // ObjectFace actions require one complete, current, allowed authority
  // bundle. The action revision binds the action tuple + stable authority
  // identity, while equivalent policy reevaluations may rotate pol_ ids/time.
  {
    const area = "source-control-cleanup";
    const itemId = "scr_authority_fixture";
    const baseAction = authorityAction({
      id: "record_source_control_cleanup_review",
      label: "Record source-control review",
    });
    const projection = { items: [{ id: itemId, area, actions: [baseAction] }] };
    const resolved = resolveGenericActionFromProjection(
      projection,
      area,
      itemId,
      "record_source_control_cleanup_review",
    );
    const missing = structuredClone(baseAction);
    delete (missing as Record<string, unknown>).policy_authority;
    const malformed = structuredClone(baseAction);
    (malformed.policy_authority as Record<string, unknown>).action_intent_id = "act_other";
    const held = structuredClone(baseAction);
    const heldDecision = held.policy_decision as Record<string, unknown>;
    const heldFace = held.policy_authority as Record<string, unknown>;
    heldDecision.outcome = "hold";
    heldDecision.standing_grant_id = "";
    heldFace.allowed = false;
    heldFace.policy_outcome = "hold";
    heldFace.standing_grant_id = "";
    heldFace.wording = "Needs your decision · current policy holds this action";
    heldFace.exact_gate = "policy_hold";

    const missingResolution = resolveGenericActionFromProjection(
      { items: [{ id: itemId, actions: [missing] }] },
      area,
      itemId,
      "record_source_control_cleanup_review",
    );
    const malformedResolution = resolveGenericActionFromProjection(
      { items: [{ id: itemId, actions: [malformed] }] },
      area,
      itemId,
      "record_source_control_cleanup_review",
    );
    const heldResolution = resolveGenericActionFromProjection(
      { items: [{ id: itemId, actions: [held] }] },
      area,
      itemId,
      "record_source_control_cleanup_review",
    );

    const reevaluated = structuredClone(baseAction);
    (reevaluated.policy_decision as Record<string, unknown>).id = "pol_authority_reevaluated";
    (reevaluated.policy_decision as Record<string, unknown>).created_at = "2026-07-17T12:05:00Z";
    (reevaluated.policy_authority as Record<string, unknown>).policy_decision_id = "pol_authority_reevaluated";
    const changedAction = structuredClone(baseAction);
    changedAction.argv = [...changedAction.argv, "--json"];
    const changedAuthority = structuredClone(baseAction);
    (changedAuthority.policy_authority as Record<string, unknown>).revision =
      `policy-authority-v1:sha256:${"b".repeat(64)}`;

    if (!resolved.ok) {
      fail(`live allowed authority action did not resolve: ${JSON.stringify(resolved)}`);
    } else if (
      resolved.action.actionIntentId !== "act_authority_fixture" ||
      resolved.action.authorityRevision !== `policy-authority-v1:sha256:${"a".repeat(64)}` ||
      !resolved.action.revision.startsWith("generic-action-v2:")
    ) {
      fail(`resolved action lost authority identity: ${JSON.stringify(resolved.action)}`);
    } else if (
      missingResolution.ok || missingResolution.rejection.gate !== "authority-unavailable" ||
      malformedResolution.ok || malformedResolution.rejection.gate !== "authority-unavailable" ||
      heldResolution.ok || heldResolution.rejection.gate !== "authority-not-allowed"
    ) {
      fail(`missing/malformed/held authority gates drifted: ${JSON.stringify({ missingResolution, malformedResolution, heldResolution })}`);
    } else if (
      genericActionRevision(baseAction) !== genericActionRevision(reevaluated) ||
      genericActionRevision(baseAction) === genericActionRevision(changedAction) ||
      genericActionRevision(baseAction) === genericActionRevision(changedAuthority)
    ) {
      fail("generic action v2 revision must ignore only policy decision identity/time and bind action + AuthorityFace");
    } else {
      pass("live allowed authority resolves; missing/malformed/held reject; v2 revision binds action + AuthorityFace only");
    }
  }

  return { ok, details };
}
