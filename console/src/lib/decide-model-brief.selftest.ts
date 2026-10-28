/*
 * Falsifiers for the decision-brief sidecar's console-side seam-audit fact:
 * `group.sample` is only the FIRST member internal/reviewartifact/queue.go's
 * buildQueueGroups saw while building the group — a brief prepared for any
 * OTHER member must still surface on the card. These checks operate purely on
 * the DecideGroup data model (no React renderer is wired into this repo) —
 * DecideQueue.tsx's brief block is gated on `group.brief` alone, so proving
 * the model's `brief` field correct/absent here is equivalent to proving the
 * rendered card is correct/frozen. decide-model resolves through `@/lib` path
 * aliases, so this runs under bun via scripts/decide-model-brief-smoke.mjs
 * (same reason as generic-actions.selftest.ts).
 */

import { decideReadinessSections, enumerableAuthorityFaces, remainingDecideMembers } from "./decide-members";
import { toAttentionReport } from "./attention";
import { authoritySummarySegments } from "./action-authority";
import { headlineAct } from "./briefing";
import { buildDecideGroups, genericReviewResolutionActs } from "./decide-model";
import { manualCopyCommandFor } from "./decide-resolution-acts";
import { buildEnumerableActionRequest } from "./governed-action-client";
import type { Json } from "./compound";

type Check = { name: string; ok: boolean; detail: string };

function baseGroup(overrides: Partial<Json> = {}): Json {
  return {
    key: "capability-lifecycle-proposals|fixture",
    area: "capability-lifecycle-proposals",
    count: 2,
    status: "review_required",
    band: "needs_you",
    operator_summary: "Two members need a call.",
    context_label: "capability-lifecycle-proposals",
    item_ids: ["member_a", "member_b"],
    evidence_refs: ["compound://evidence/a", "compound://evidence/b"],
    sample: { id: "member_a", area: "capability-lifecycle-proposals", created_at: "2026-07-10T00:00:00Z" },
    ...overrides,
  };
}

function validBriefPayload(preparedBy = "switchback_api"): Json {
  return {
    schema_version: "compound.decision_brief.v0.1",
    headline: "Choose the bounded next move",
    what_this_is: "A prepared translation of the underlying review artifact.",
    findings: [{ finding: "No wired action exists yet.", evidence_ref: "compound://evidence/finding-1" }],
    options: [
      {
        value: "approve",
        label: "Approve the direction",
        consequence_approve: "A separate governed action may move the work forward.",
        consequence_hold: "The review remains open without mutation.",
        command_argv: null,
      },
    ],
    recommendation: { option: "approve", confidence: 0.8, rationale: "Evidence supports it." },
    prepared_by: preparedBy,
    prepared_at: "2026-07-10T04:00:00Z",
    propose_only: true,
  };
}

function authorityActionFixture(itemId = "scr_authority_face"): Json {
  const scope = { entity_id: itemId, project_id: "compound", surface: "reviews" };
  const target = { kind: "review_artifact", id: itemId };
  return {
    id: "record_source_control_cleanup_review",
    label: "Record the current source-control review",
    argv: ["compound", "authority", "dirty-review", `/tmp/${itemId}.review.yaml`, "--json"],
    requires_review: false,
    action_intent: {
      schema_version: "compound.action_intent.v0.1",
      id: `act_${itemId}`,
      created_at: "2026-07-17T12:00:00Z",
      producer: { kind: "cli", name: "compound", version: "fixture" },
      ids: { work_id: "work_authority_face" },
      scope,
      authority: { source_class: "runtime", status: "current" },
      privacy: "private",
      risk: "tier1",
      evidence_refs: ["evidence:authority-face"],
      uncertainty: { level: "known" },
      actor: "operator-console",
      capability: "source-control-review",
      action: "record exact review",
      target,
      mutation_class: "local_reversible",
      reversibility: "easy",
      blast_radius: "local_reversible",
      estimated_cost: { usd: 0, tokens: 0, human_review_minutes: 1 },
      rollback_plan: "supersede the recorded review",
    },
    policy_decision: {
      schema_version: "compound.policy_decision.v0.1",
      id: `pol_${itemId}`,
      created_at: "2026-07-17T12:00:01Z",
      producer: { kind: "policy", name: "compound", version: "fixture" },
      ids: { work_id: "work_authority_face" },
      scope,
      authority: { source_class: "runtime", status: "current" },
      privacy: "private",
      risk: "tier1",
      evidence_refs: ["evidence:policy-face"],
      uncertainty: { level: "known" },
      action_intent_id: `act_${itemId}`,
      outcome: "allow",
      reasons: ["current policy allows the exact action"],
      obligations: [],
      standing_grant_id: "grant_authority_face",
    },
    policy_authority: {
      schema_version: "compound.policy_authority_face.v0.1",
      revision: `policy-authority-v1:sha256:${"e".repeat(64)}`,
      current: true,
      allowed: true,
      wording: "Allowed by current policy",
      action_intent_id: `act_${itemId}`,
      policy_decision_id: `pol_${itemId}`,
      policy_outcome: "allow",
      standing_grant_id: "grant_authority_face",
      target,
      target_scope: "private",
      blast_radius: "local_reversible",
      reversibility: "easy",
      verified_recoverability: "verified",
      operator_control_continuity: "continuous",
      policy_source: "fixture-policy",
      exact_gate: "standing_scope",
      expected_receipt: "source-control review receipt",
      expected_proof: ["proof:review-recorded"],
    },
  };
}

function build(group: Json, itemByID: Map<string, Json>) {
  const [decided] = buildDecideGroups([group], itemByID, null, () => itemByID.get("member_a")!);
  return decided;
}

export function runDecideModelBriefSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  // The real console passes ProjectionRead status into the model. Only a live,
  // current, allowed Go authority bundle may mount an act; held remains exact
  // policy context, while stale/malformed are waiting with no synthetic act.
  {
    const itemId = "scr_authority_face";
    const allowedAction = authorityActionFixture(itemId);
    const item = { id: itemId, area: "source-control-cleanup", actions: [allowedAction] } as Json;
    const group = {
      key: "source-control-cleanup|authority-face",
      area: "source-control-cleanup",
      count: 1,
      item_ids: [itemId],
      sample: item,
      safe_batch_action: allowedAction,
      operator_summary: "Review the current source-control record.",
    } as Json;
    const attention = {
      schema_version: "compound.attention.v0.1",
      generated_at: "2026-07-17T12:00:02Z",
      counts: {},
      top: null,
      groups: [{ ...group, action: { ...allowedAction, kind: "one_click", requires_confirmation: false } }],
    } as Parameters<typeof buildDecideGroups>[2];
    const buildWith = (action: Json, status: "live" | "stale" | "unreachable") => {
      const nextItem = { ...item, actions: [action] } as Json;
      const nextGroup = { ...group, sample: nextItem, safe_batch_action: action } as Json;
      const nextAttention = {
        ...attention,
        groups: [{ ...nextGroup, action: { ...action, kind: "one_click", requires_confirmation: false } }],
      } as Parameters<typeof buildDecideGroups>[2];
      return buildDecideGroups(
        [nextGroup],
        new Map([[itemId, nextItem]]),
        nextAttention,
        () => nextItem,
        status,
      )[0];
    };
    const live = buildWith(allowedAction, "live");
    const heldAction = structuredClone(allowedAction);
    heldAction.policy_decision.outcome = "hold";
    heldAction.policy_decision.standing_grant_id = "";
    heldAction.policy_authority.allowed = false;
    heldAction.policy_authority.policy_outcome = "hold";
    heldAction.policy_authority.standing_grant_id = "";
    heldAction.policy_authority.wording = "Needs your decision · current policy holds this action";
    heldAction.policy_authority.exact_gate = "policy_hold";
    const held = buildWith(heldAction, "live");
    const stale = buildWith(allowedAction, "stale");
    const malformedAction = structuredClone(allowedAction);
    delete malformedAction.policy_decision;
    const malformed = buildWith(malformedAction, "live");
    const missingDisplayAction = structuredClone(allowedAction);
    delete missingDisplayAction.action_intent.blast_radius;
    delete missingDisplayAction.policy_authority.blast_radius;
    const missingDisplay = buildWith(missingDisplayAction, "live");
    const normalizedAttention = toAttentionReport(attention as Json);
    const headlineLive = headlineAct(normalizedAttention?.groups[0] ?? null, "live");
    const headlineStale = headlineAct(normalizedAttention?.groups[0] ?? null, "stale");
    const headlineAuthoritySummary = live.face.authority ? authoritySummarySegments(live.face.authority).join(" · ") : "";
    push(
      "objectface-readiness-is-live-authority-bound",
        live.act.mode === "generic" &&
        live.act.requiresConfirmation === true &&
        live.act.actionIntentId === `act_${itemId}` &&
        live.act.expectedAuthorityRevision === allowedAction.policy_authority.revision &&
        live.face.state === "ready" &&
        live.face.primaryAction?.label === allowedAction.policy_authority.wording &&
        live.authorityBound === true &&
        live.face.authority?.expectedReceipt === "source-control review receipt" &&
        live.face.authority.rollbackPlan === "supersede the recorded review" &&
        live.face.authority.target.id === itemId &&
        live.face.authority.targetScope === "private" &&
        live.face.authority.blastRadius === "local_reversible" &&
        live.face.authority.reversibility === "easy" &&
        live.face.authority.verifiedRecoverability === "verified" &&
        live.face.authority.operatorControlContinuity === "continuous" &&
        live.face.authority.policySource === "fixture-policy" &&
        live.face.authority.exactGate === "standing_scope" &&
        live.face.authority.expectedProof.join(",") === "proof:review-recorded" &&
        held.act.mode === "copy" &&
        held.face.state === "needs_attention" &&
        held.face.stateLabel === heldAction.policy_authority.wording &&
        held.face.primaryAction == null &&
        stale.act.mode === "copy" &&
        stale.face.state === "waiting" &&
        stale.face.primaryAction == null &&
        malformed.act.mode === "copy" &&
        malformed.face.state === "waiting" &&
        malformed.face.authority == null &&
        missingDisplay.act.mode === "copy" &&
        missingDisplay.face.state === "waiting" &&
        missingDisplay.face.authority == null &&
        headlineLive?.pathway === "generic" &&
        headlineLive.requiresConfirmation === true &&
        headlineLive.actionIntentId === `act_${itemId}` &&
        headlineLive.expectedAuthorityRevision === allowedAction.policy_authority.revision &&
        headlineLive.label === allowedAction.policy_authority.wording &&
        headlineStale == null &&
        headlineAuthoritySummary.includes("Standing grant: grant_authority_face") &&
        manualCopyCommandFor(stale) == null &&
        manualCopyCommandFor(malformed) == null &&
        genericReviewResolutionActs(stale).length === 0 &&
        genericReviewResolutionActs(malformed).length === 0,
      `live=${live.act.mode}/${live.face.state}/${JSON.stringify(live.face.authority)} held=${held.act.mode}/${held.face.state}/${held.face.stateLabel} stale=${stale.act.mode}/${stale.face.state} malformed=${malformed.act.mode}/${malformed.face.state} missing_display=${missingDisplay.act.mode}/${missingDisplay.face.state}/${missingDisplay.face.authority == null} headline=${headlineLive?.pathway}/${headlineLive?.label}/stale:${headlineStale == null}/grant:${headlineAuthoritySummary.includes("Standing grant: grant_authority_face")} manual=${manualCopyCommandFor(stale)}/${manualCopyCommandFor(malformed)} synthetic=${genericReviewResolutionActs(stale).length}/${genericReviewResolutionActs(malformed).length}`,
    );
  }

  // 1. The seam-audit falsifier: group.sample points at member_a (no brief);
  //    the brief lives on member_b, the SECOND member — exactly the shape the
  //    live proof target (a multi-member capability-lifecycle-proposals
  //    group) hits when the briefed item isn't group-order-first.
  {
    const decisionCard = {
      claim: "Review member B.",
      choices: [{ id: "reject", label: "Reject proposal", effect: "File member B without work." }],
      meta: { proposal_hash: "b".repeat(64) },
    };
    const itemByID = new Map<string, Json>([
      ["member_a", { id: "member_a", area: "capability-lifecycle-proposals" }],
      ["member_b", { id: "member_b", area: "capability-lifecycle-proposals", brief: validBriefPayload(), decision_card: decisionCard }],
    ]);
    const decided = build(baseGroup(), itemByID);
    push(
      "scans-every-group-member",
      decided.brief != null &&
        decided.brief.headline === "Choose the bounded next move" &&
        decided.brief.preparedBy === "switchback_api" &&
        decided.brief.findings.length === 1 &&
        decided.brief.findings[0].evidenceRef === "compound://evidence/finding-1" &&
        decided.brief.options[0].value === "approve" &&
        decided.brief.recommendation.option === "approve" &&
        decided.act.mode === "bespoke" &&
        decided.act.itemId === "member_b",
      `brief: ${decided.brief ? "present" : "MISSING"} preparedBy=${decided.brief?.preparedBy ?? "-"} target=${decided.act.mode === "bespoke" ? decided.act.itemId : decided.act.mode}`,
    );
  }

  // 13. A panel apply SBA and per-member verdict-record actions are different
  //     effects. The clean member action must not replace the atomic apply.
  {
    const item = {
      id: "panel_apply_member01",
      area: "panel-verdicts",
      actions: [
        { id: "decision:promote", argv: ["/bin/sh", "-lc", "compound panel verdict-record && apply"] },
        {
          id: "panel_verdict_record_member",
          argv: ["compound", "panel", "verdict-record", "--review", "/tmp/panel_apply_member01.json"],
        },
      ],
    } as Json;
    const group = {
      key: "panel-verdicts|prun_fixture_apply|promote",
      area: item.area,
      count: 1,
      item_ids: [item.id],
      sample: item,
      safe_batch_action: {
        id: "panel_run_apply",
        argv: ["compound", "panel", "run", "--plan", "prun_fixture_apply", "--apply"],
        requires_review: true,
      },
    } as Json;
    const decided = buildDecideGroups([group], new Map([[item.id, item]]), null, () => item)[0];
    push(
      "panel-apply-is-not-replaced-by-member-record",
      decided.act.mode === "batch-atomic" && decided.act.area === "panel-apply" && decided.capability.kind === "apply",
      `act=${decided.act.mode}${decided.act.mode === "batch-atomic" ? ` area=${decided.act.area}` : ""} capability=${decided.capability.kind}`,
    );
  }

  // 14. A future clean panel review action must not become panel-apply merely
  //     because the group key happens to carry a recoverable panel run id.
  //     Only the owner's exact panel_run_apply action and argv shape may cross
  //     the apply boundary.
  {
    const item = {
      id: "future_panel_member01",
      area: "panel-verdicts",
    } as Json;
    const group = {
      key: "panel-verdicts|prun_fixture_future|needs_human",
      area: item.area,
      count: 1,
      item_ids: [item.id],
      sample: item,
      safe_batch_action: {
        id: "future_panel_record",
        argv: ["compound", "panel", "verdict-record", "--group", "panel-verdicts|prun_fixture_future|needs_human"],
        requires_review: true,
      },
    } as Json;
    const decided = buildDecideGroups([group], new Map([[item.id, item]]), null, () => item)[0];
    push(
      "panel-apply-requires-exact-owner-action",
      !(decided.act.mode === "batch-atomic" && decided.act.area === "panel-apply"),
      `act=${decided.act.mode}${decided.act.mode === "batch-atomic" ? ` area=${decided.act.area}` : ""}`,
    );
  }

  // 15. The grouped record id cannot relabel an apply argv. Atomic handlers
  //     bind both the owner action id and the exact represented command.
  {
    const item = { id: "mismatched_panel_member01", area: "panel-verdicts" } as Json;
    const group = {
      key: "panel-verdicts|prun_fixture_mismatch|needs_human",
      area: item.area,
      count: 1,
      item_ids: [item.id],
      sample: item,
      safe_batch_action: {
        id: "panel_verdict_record_group",
        argv: ["compound", "panel", "run", "--plan", "prun_fixture_mismatch", "--apply"],
        requires_review: true,
      },
    } as Json;
    const decided = buildDecideGroups([group], new Map([[item.id, item]]), null, () => item)[0];
    push(
      "panel-record-group-requires-exact-owner-argv",
      !(decided.act.mode === "batch-atomic" && decided.act.area === "panel-verdict-record-group"),
      `act=${decided.act.mode}${decided.act.mode === "batch-atomic" ? ` area=${decided.act.area}` : ""}`,
    );
  }

  // 12. Certification is area + verb, not area alone. A job-health action
  //     carrying another area's clean verb stays unavailable before any click.
  {
    const item = { id: "job_health_mismatch01", area: "job-health" } as Json;
    const group = {
      key: "job-health|mismatch",
      area: item.area,
      count: 1,
      item_ids: [item.id],
      sample: item,
      operator_summary: "Inspect a mismatched action.",
    } as Json;
    const attention = {
      groups: [{
        key: group.key,
        area: item.area,
        action: {
          id: "wrong_verb",
          kind: "one_click",
          argv: ["compound", "authority", "dirty-review", "/tmp/other.review.yaml"],
        },
      }],
    } as Parameters<typeof buildDecideGroups>[2];
    const decided = buildDecideGroups([group], new Map([[item.id, item]]), attention, () => item)[0];
    push(
      "certified-area-mismatched-verb-stays-unavailable",
      decided.act.mode === "copy" && decided.capability.kind === "unavailable",
      `act=${decided.act.mode} capability=${decided.capability.kind}`,
    );
  }

  // An Attention-owned external-repo group may contain several repositories,
  // while the legacy bespoke registry action resolves only one review YAML.
  // Keep the grouped card read-only so one click can never look like it ruled
  // on every member while mutating only the first sample.
  {
    const area = "external-repo-intake";
    const items = ["extrepo_alpha01", "extrepo_bravo02"].map((id) => ({
      id,
      area,
      context_label: id,
      actions: [{
        id: `review_${id}`,
        argv: ["compound", "capability", "external-intake-review", `/tmp/${id}.review.yaml`],
      }],
    })) as Json[];
    const group = {
      key: "external-repo-intake|needs-you",
      area,
      count: items.length,
      item_ids: items.map((item) => String(item.id)),
      operator_summary: "Two external repositories need separate intake decisions.",
      sample: items[0],
    } as Json;
    const attention = {
      groups: [{
        ...group,
        action: {
          id: "review_external_repo",
          kind: "one_click",
          argv: ["compound", "capability", "external-intake-review", "/tmp/extrepo_alpha01.review.yaml"],
        },
      }],
    } as Parameters<typeof buildDecideGroups>[2];
    const itemByID = new Map(items.map((item) => [String(item.id), item]));
    const decided = buildDecideGroups([group], itemByID, attention, () => items[0])[0];
    const resolutionActs = genericReviewResolutionActs(decided);
    push(
      "external-attention-group-stays-copy-only",
      decided.key === group.key &&
        decided.count === items.length &&
        decided.members.map((member) => member.id).join(",") === items.map((item) => item.id).join(",") &&
        decided.act.mode === "copy" &&
        decided.capability.kind === "unavailable" &&
        resolutionActs.length === 0,
      `key=${decided.key} count=${decided.count} members=${decided.members.map((member) => member.id).join(",")} act=${decided.act.mode} capability=${decided.capability.kind} resolutions=${resolutionActs.length}`,
    );
  }

  // Attention-only groups are real operator gates, but they are not review
  // items. A copy fallback must not invent reviews-resolve Park/Dismiss actions
  // against the synthetic group key.
  {
    const group = {
      key: "workbench:blocked-on-you",
      area: "workbench-blocked-on-you",
      count: 1,
      item_ids: ["work_blocked_fixture"],
      operator_summary: "One work item needs a ruling.",
    } as Json;
    const decided = buildDecideGroups([group], new Map(), null, () => ({}))[0];
    const resolutionActs = genericReviewResolutionActs(decided);
    push(
      "attention-only-copy-card-has-no-review-disposition",
      decided.act.mode === "copy" && resolutionActs.length === 0,
      `act=${decided.act.mode} sample=${decided.sampleId} resolutions=${resolutionActs.length}`,
    );
  }

  // Attention-only identities still carry their oldest observed timestamp.
  // Do not erase it merely because Reviews has no sample to enrich the group.
  {
    const group = {
      key: "workbench:blocked-on-you",
      area: "workbench-blocked-on-you",
      count: 1,
      item_ids: ["work_blocked_fixture"],
      oldest_created_at: "2024-01-02T03:04:05Z",
      operator_summary: "One work item needs a ruling.",
    } as Json;
    const decided = buildDecideGroups([group], new Map(), null, () => ({}))[0];
    push(
      "attention-oldest-created-at-survives-without-review-sample",
      decided.createdAt === group.oldest_created_at && decided.ageLabel !== "age unknown" && decided.old,
      `created_at=${decided.createdAt} age=${decided.ageLabel} old=${decided.old}`,
    );
  }

  // 11. After a partial enumerable batch, a retry excludes both deliberately
  //     skipped members and members already recorded before the failure.
  {
    const members = ["a", "b", "c"].map((id) => ({ id, label: id, argv: ["panel", "verdict-record"], actionId: `record_${id}` }));
    const remaining = remainingDecideMembers(members, new Set(["c"]), new Set(["a"]));
    push(
      "partial-enumerable-retry-skips-completed-members",
      remaining.map((member) => member.id).join(",") === "b",
      `remaining=${remaining.map((member) => member.id).join(",")}`,
    );
  }

  // 2. A briefed group.sample (first member) still surfaces the brief.
  {
    const itemByID = new Map<string, Json>([
      ["member_a", { id: "member_a", area: "capability-lifecycle-proposals", brief: validBriefPayload("codex") }],
      ["member_b", { id: "member_b", area: "capability-lifecycle-proposals" }],
    ]);
    const group = baseGroup({
      sample: { id: "member_a", area: "capability-lifecycle-proposals", brief: validBriefPayload("codex") },
    });
    const decided = build(group, itemByID);
    push(
      "briefed-sample-surfaces",
      decided.brief?.preparedBy === "codex",
      `preparedBy=${decided.brief?.preparedBy ?? "MISSING"}`,
    );
  }

  // 3. No member briefed: brief absent and every other card field unchanged
  //    (frozen-markup proxy — DecideQueue's brief block gates on group.brief
  //    alone, so an absent brief means the card renders exactly as before
  //    this packet).
  {
    const itemByID = new Map<string, Json>([
      ["member_a", { id: "member_a", area: "capability-lifecycle-proposals" }],
      ["member_b", { id: "member_b", area: "capability-lifecycle-proposals" }],
    ]);
    const decided = build(baseGroup(), itemByID);
    push(
      "unbriefed-card-frozen",
      (decided.brief ?? null) === null &&
        decided.title === "Two members need a call." &&
        decided.members.map((m) => m.id).join(",") === "member_a,member_b" &&
        decided.evidence.length > 0 &&
        decided.act.mode === "copy",
      `brief=${decided.brief ? "PRESENT" : "absent"} title=${decided.title} act=${decided.act.mode}`,
    );
  }

  // 4. An invalid brief shape (missing evidence_ref) never renders client-side.
  {
    const invalid = validBriefPayload();
    invalid.findings = [{ finding: "no evidence", evidence_ref: "" }];
    const itemByID = new Map<string, Json>([
      ["member_a", { id: "member_a", area: "capability-lifecycle-proposals", brief: invalid }],
    ]);
    const group = baseGroup({
      item_ids: ["member_a"],
      sample: { id: "member_a", area: "capability-lifecycle-proposals" },
    });
    const decided = build(group, itemByID);
    push(
      "invalid-brief-missing-evidence-rejected",
      (decided.brief ?? null) === null,
      `brief=${decided.brief ? "PRESENT (BUG)" : "absent"}`,
    );
  }

  // 5. A brief missing prepared_by never renders client-side either
  //    (provenance is not optional).
  {
    const invalid = validBriefPayload();
    invalid.prepared_by = "";
    const itemByID = new Map<string, Json>([
      ["member_a", { id: "member_a", area: "capability-lifecycle-proposals", brief: invalid }],
    ]);
    const group = baseGroup({
      item_ids: ["member_a"],
      sample: { id: "member_a", area: "capability-lifecycle-proposals" },
    });
    const decided = build(group, itemByID);
    push(
      "brief-without-provenance-rejected",
      (decided.brief ?? null) === null,
      `brief=${decided.brief ? "PRESENT (BUG)" : "absent"}`,
    );
  }

  // 6. command_argv passes through as copy/display data only — the group's
  //    OWN act stays whatever the real projection supports (here: honest
  //    copy, no registry spec exists) and the brief's argv is never consulted
  //    by resolveAct.
  {
    const brief = validBriefPayload();
    brief.options = [
      {
        value: "approve",
        label: "Approve",
        consequence_approve: "",
        consequence_hold: "",
        command_argv: ["compound", "capability", "lifecycle-promote", "--id", "clp_1"],
      },
    ];
    const itemByID = new Map<string, Json>([
      ["member_a", { id: "member_a", area: "capability-lifecycle-proposals", brief } as Json],
    ]);
    const group = baseGroup({
      item_ids: ["member_a"],
      sample: { id: "member_a", area: "capability-lifecycle-proposals" },
    });
    const decided = build(group, itemByID);
    push(
      "brief-argv-copy-only",
      decided.brief?.options[0].commandArgv?.join(" ") ===
        "compound capability lifecycle-promote --id clp_1" && decided.act.mode === "copy",
      `argv=${decided.brief?.options[0].commandArgv?.join(" ") ?? "MISSING"} act=${decided.act.mode}`,
    );
  }

  // 7. Owner-projected lifecycle choices become an explicit, unselected
  //    disposition target. The rendered proposal hash is carried separately
  //    for optimistic concurrency; brief options never feed this list.
  {
    const card = {
      claim: "Teach the existing skill one verified failure pattern.",
      why: [{ label: "evidence", detail: "Two source refs support the proposal." }],
      choices: [
        { id: "open_work", label: "Turn into tracked work", effect: "Create proposed work only." },
        { id: "defer", label: "Defer with a trigger", effect: "Watch until the trigger." },
        { id: "reject", label: "Reject proposal", effect: "File it without work." },
      ],
      meta: { proposal_hash: "a".repeat(64) },
      boundary: ["no capability mutation"],
    };
    const item = {
      id: "clp_fixture",
      area: "capability-lifecycle-proposals",
      decision_card: card,
      evidence_refs: ["session:fixture"],
    } as Json;
    const group = baseGroup({
      key: "capability-lifecycle-proposals|clp_fixture",
      count: 1,
      item_ids: ["clp_fixture"],
      sample: item,
      evidence_summaries: [{
        ref: "session:fixture",
        label: "Source session",
        source_kind: "session",
        summary: "The same failure repeated twice.",
      }],
    });
    const decided = buildDecideGroups([group], new Map([["clp_fixture", item]]), null, () => item)[0];
    const act = decided.act;
    const actionableHere = [decided].filter((candidate) => candidate.capability.kind !== "unavailable").length;
    push(
      "owner-choices-explicit-and-version-bound",
      act.mode === "bespoke" &&
        act.options.map((option) => option.value).join(",") === "open_work,defer,reject" &&
        act.option === "" &&
        act.expectedRevision === "a".repeat(64) &&
        act.supportsPreview &&
        decided.capability.kind === "track" &&
        actionableHere === 1 &&
        decided.evidence[0]?.summary === "The same failure repeated twice.",
      `act=${act.mode} option=${act.mode === "bespoke" ? act.option : "-"} capability=${decided.capability.kind} actionable=${actionableHere}`,
    );
  }

  // 8. A bespoke owner with zero effect-compatible choices stays inspection
  //    only even when the projection carries a different record action.
  {
    const item = {
      id: "route_promotion_fixture01",
      area: "route-promotions",
      group_key: "route-promotions|demote|classify",
      json_path: "/tmp/route_promotion_fixture01.json",
      actions: [{
        id: "route_promotion_record",
        argv: ["compound", "route", "promotion-record", "--review", "/tmp/route_promotion_fixture01.json"],
        requires_review: true,
      }],
    } as Json;
    const group = {
      key: item.group_key,
      area: item.area,
      count: 1,
      item_ids: [item.id],
      sample: item,
      safe_batch_action: item.actions[0],
      operator_summary: "Demote this lane?",
    } as Json;
    const decided = buildDecideGroups([group], new Map([[item.id, item]]), null, () => item)[0];
    push(
      "zero-option-bespoke-owner-stays-unavailable",
      decided.act.mode === "copy" &&
        decided.capability.kind === "unavailable" &&
        decided.act.command === "compound reviews --area route-promotions" &&
        decided.consequences?.approve.includes("this review card cannot promote or demote") === true,
      `act=${decided.act.mode} capability=${decided.capability.kind}`,
    );
  }

  // 9. Truly generic enumerable actions carry confirmation and rationale
  //    requirements into the client instead of self-confirming on first click.
  {
    const item = {
      id: "panel_verdict_fixture01",
      area: "panel-verdicts",
      context_label: "fixture verdict",
      actions: [
        ...["promote", "park", "reject", "needs_human"].map((choice) => ({
          id: `decision:${choice}`,
          argv: ["/bin/sh", "-lc", `compound panel verdict-record --review fixture && ${choice}`],
          requires_review: true,
        })),
        {
          id: "panel_verdict_record_member",
          argv: ["compound", "panel", "verdict-record", "--review", "/tmp/panel_verdict_fixture01.json"],
          requires_review: true,
        },
      ],
    } as Json;
    const group = {
      key: "panel-verdicts|fixture",
      area: item.area,
      count: 1,
      item_ids: [item.id],
      sample: item,
      safe_batch_action: {
        id: "panel_verdict_record_group",
        label: "Record selected verdicts",
        argv: ["compound", "panel", "verdict-record", "--review", "/tmp/panel_verdict_fixture01.json"],
        requires_review: true,
        policy_note: "Records reviewed provenance only.",
      },
      operator_summary: "Record the reviewed verdict.",
    } as Json;
    const decided = buildDecideGroups([group], new Map([[item.id, item]]), null, () => item)[0];
    const act = decided.act;
    push(
      "generic-enumerable-carries-confirmation-contract",
      act.mode === "batch-enumerable" && act.requiresConfirmation && act.requiresRationale &&
        act.effectKind === "record" && decided.capability.kind === "record",
      `act=${act.mode}${act.mode === "batch-enumerable" ? ` confirm=${act.requiresConfirmation} rationale=${act.requiresRationale} effect=${act.effectKind}` : ""}`,
    );
  }

  // 10. A clean projection action in an uncertified area remains inspection
  //     only. The UI must not promise "Apply now" before the server 403s it.
  {
    const item = {
      id: "uncertified_fixture01",
      area: "uncertified-area",
      actions: [{ id: "looks_clean", argv: ["compound", "work", "close", "work_fixture"] }],
    } as Json;
    const group = {
      key: "uncertified-area|fixture",
      area: item.area,
      count: 1,
      item_ids: [item.id],
      sample: item,
      safe_batch_action: item.actions[0],
      operator_summary: "Inspect an uncertified action.",
    } as Json;
    const decided = buildDecideGroups([group], new Map([[item.id, item]]), null, () => item)[0];
    push(
      "uncertified-clean-action-stays-unavailable",
      decided.act.mode === "copy" &&
        decided.capability.kind === "unavailable" &&
        decided.face.state === "waiting" &&
        decided.face.stateLabel === "Needs context" &&
        decided.face.primaryAction == null &&
        /owner workflow/i.test(decided.face.unavailableReason) &&
        /manual owner command/i.test(decided.face.unblockPath),
      `act=${decided.act.mode} capability=${decided.capability.kind} face=${JSON.stringify(decided.face)}`,
    );
  }

  // A singleton decision candidate with the owner-projected bounded panel
  // plan becomes one enumerable tracked action. The queue-hygiene fallback is
  // retired for this item; no accept/import/source-write option is invented.
  {
    const area = "decision-candidates";
    const itemId = "dcp_decision_review_selftest";
    const action = {
      id: "plan_decision_candidate_review",
      label: "Plan decision review",
      argv: [
        "compound", "panel", "plan",
        "--target-kind", "decision_candidate",
        "--target", "cand_decision_review_selftest",
        "--domain", "reasoning-lenses",
        "--task", "review falsifier, review trigger, source ownership, and second-order effects",
        "--supervised",
        "--max-items", "1",
        "--json",
      ],
      requires_review: true,
      policy_note: "creates review evidence only; decision source and authority stay unchanged",
    } as Json;
    const item = {
      id: itemId,
      area,
      context_label: "Require an explicit decision when recovery is not verified",
      actions: [action],
    } as Json;
    const group = {
      key: "decision-candidates|ruling",
      area,
      count: 1,
      item_ids: [itemId],
      operator_summary: "Review this candidate ruling.",
      operator_unlock: "A bounded judge review can start without changing decision authority.",
      sample: item,
      safe_batch_action: action,
    } as Json;
    const [decided] = buildDecideGroups([group], new Map([[itemId, item]]), null, () => item);
    push(
      "decision-candidate-owner-plan-retires-unavailable-fallback",
      decided.act.mode === "batch-enumerable" &&
        decided.act.effectKind === "track" &&
        decided.act.requiresConfirmation === true &&
        decided.act.label === "Plan decision review" &&
        decided.act.policyNote === "This prepares review evidence only. The proposed rule stays unadopted." &&
        decided.act.members.length === 1 &&
        decided.act.members[0].id === itemId &&
        decided.act.members[0].actionId === "plan_decision_candidate_review" &&
        decided.capability.kind === "track" &&
        decided.face.kind === "Decision" &&
        decided.face.label === "Review a proposed operating rule" &&
        decided.face.state === "ready" &&
        decided.face.stateLabel === "Ready" &&
        decided.face.whyNow === "This is not the final approval. Compound first needs an independent review before this rule can enter the decision source." &&
        decided.face.primaryAction?.label === "Prepare independent review" &&
        decided.face.primaryAction.description === "Creates review evidence only. It does not adopt the rule or change decision authority." &&
        decided.face.primaryAction.effect === "track" &&
        Boolean(decided.face.owner) &&
        genericReviewResolutionActs(decided).length === 0,
      `act=${decided.act.mode} effect=${decided.act.mode === "batch-enumerable" ? decided.act.effectKind : "-"} label=${decided.act.mode === "batch-enumerable" ? JSON.stringify(decided.act.label) : "-"} policy=${decided.act.mode === "batch-enumerable" ? JSON.stringify(decided.act.policyNote) : "-"} capability=${decided.capability.kind} face=${JSON.stringify(decided.face)} resolutions=${genericReviewResolutionActs(decided).length}`,
    );
  }

  // 11. The client-safe queue grouper consumes the server-owned face state.
  //     It must never re-derive actionability from area names or button copy.
  {
    const sections = decideReadinessSections([
      { key: "ready-a", face: { state: "ready" as const } },
      { key: "waiting-a", face: { state: "waiting" as const } },
      { key: "ready-b", face: { state: "ready" as const } },
    ]);
    push(
      "queue-groups-by-server-owned-face-state",
      sections.length === 2 &&
        sections[0].key === "act_now" &&
        sections[0].groups.map((group) => group.key).join(",") === "ready-a,ready-b" &&
        sections[1].key === "needs_context" &&
        sections[1].groups.map((group) => group.key).join(",") === "waiting-a",
      `sections=${sections.map((section) => `${section.key}:${section.groups.map((group) => group.key).join(",")}`).join("|")}`,
    );
  }

  // Same-kind candidates are serialized as separate owner groups. Each card
  // must retain its own target-bound Plan action, and owner-action precedence
  // must keep the synthetic Park/Dismiss adapter off both cards.
  {
    const area = "decision-candidates";
    const task = "review falsifier, review trigger, source ownership, and second-order effects";
    const candidates = [
      { itemId: "dcp_alpha", candidateId: "cand_decision_alpha" },
      { itemId: "dcp_bravo", candidateId: "cand_decision_bravo" },
    ];
    const items = candidates.map(({ itemId, candidateId }) => {
      const action = {
        id: "plan_decision_candidate_review",
        label: "Plan decision review",
        argv: [
          "compound", "panel", "plan",
          "--target-kind", "decision_candidate",
          "--target", candidateId,
          "--domain", "reasoning-lenses",
          "--task", task,
          "--supervised",
          "--max-items", "1",
          "--json",
        ],
        requires_review: true,
        policy_note: "creates review evidence only; decision source and authority stay unchanged",
      } as Json;
      return {
        id: itemId,
        area,
        candidate_id: candidateId,
        context_label: candidateId,
        actions: [action],
      } as Json;
    });
    const itemByID = new Map(items.map((item) => [String(item.id), item]));
    const groups = items.map((item) => ({
      key: `decision-candidates|${String(item.id)}`,
      area,
      count: 1,
      item_ids: [item.id],
      operator_summary: `Review ${String(item.candidate_id)}.`,
      sample: item,
      safe_batch_action: (item.actions as Json[])[0],
    } as Json));
    const decided = buildDecideGroups(
      groups,
      itemByID,
      null,
      (group) => itemByID.get(String((group.item_ids as unknown[])[0]))!,
    );
    const exactTargets = decided.map((group) =>
      group.act.mode === "batch-enumerable" ? group.act.members[0]?.argv?.[5] : "",
    );
    push(
      "same-kind-decision-candidates-keep-per-member-plan-and-owner-precedence",
      decided.length === 2 &&
        decided.every((group) =>
          group.act.mode === "batch-enumerable" &&
          group.act.members.length === 1 &&
          group.act.members[0]?.actionId === "plan_decision_candidate_review" &&
          group.capability.kind === "track" &&
          genericReviewResolutionActs(group).length === 0
        ) &&
        exactTargets.join(",") === candidates.map(({ candidateId }) => candidateId).join(","),
      `groups=${decided.length} modes=${decided.map((group) => group.act.mode).join(",")} targets=${exactTargets.join(",")} resolutions=${decided.map((group) => genericReviewResolutionActs(group).length).join(",")}`,
    );
  }

  // Generic review resolution derives at render time. A zero-wired
  // unknown-status card keeps its serialized model untouched while one
  // governed control shares a reason across Park and Dismiss.
  {
    const ownerRevision = `review-resolution-v2:sha256:${"d".repeat(64)}`;
    const item: Json = {
      id: "parity_unknown_fixture",
      area: "parity",
      context_label: "Parity unknown",
      review_resolution_revision: ownerRevision,
    };
    const group: Json = {
      key: "parity|unknown",
      area: "parity",
      count: 1,
      status: "unknown",
      operator_summary: "Review parity unknown.",
      context_label: "parity",
      item_ids: [item.id],
      evidence_refs: ["evidence:parity-unknown"],
      sample: item,
    };
    const [decided] = buildDecideGroups([group], new Map([[String(item.id), item]]), null, () => item);
    const before = JSON.stringify(decided);
    const acts = genericReviewResolutionActs(decided);
    const resolutionAct = acts[0];
    const after = JSON.stringify(decided);
    push(
      "copy-card-gains-one-park-dismiss-control",
      decided.act.mode === "copy" &&
        acts.length === 1 &&
        resolutionAct?.reasonRequired === true &&
        resolutionAct.expectedRevision === ownerRevision &&
        resolutionAct.effectKind === "record" &&
        manualCopyCommandFor(decided) === decided.act.command &&
        resolutionAct.decisionOptions?.map((option) => option.value).join(",") === "review_resolution_park,review_resolution_dismiss" &&
        before === after,
      `model_unchanged=${before === after} controls=${acts.length} revision=${resolutionAct?.expectedRevision ?? ""} options=${JSON.stringify(resolutionAct?.decisionOptions?.map((option) => option.value))}`,
    );
    push(
      "single-review-copy-card-targets-its-sole-member",
      resolutionAct?.itemId === "parity_unknown_fixture",
      `control_item=${resolutionAct?.itemId ?? ""}`,
    );
  }

  // Deriving resolution acts against a briefed card never mutates the
  // serialized model (frozen-rendering law), and a card whose act is already
  // executable gets no derived resolution acts at all.
  {
    const brief = validBriefPayload();
    const item: Json = { id: "unknown_freeze", area: "parity", context_label: "Parity unknown", brief };
    const group: Json = {
      key: "parity|unknown",
      area: "parity",
      count: 1,
      status: "unknown",
      operator_summary: "Review parity unknown.",
      context_label: "parity",
      item_ids: [item.id],
      evidence_refs: ["evidence:parity-freeze"],
      sample: item,
    };
    const [decided] = buildDecideGroups([group], new Map([[String(item.id), item]]), null, () => item);
    const before = JSON.stringify(decided);
    genericReviewResolutionActs(decided);
    push(
      "briefed-card-derivation-never-mutates-model",
      JSON.stringify(decided) === before,
      `after_match=${JSON.stringify(decided) === before}`,
    );
    const actionable = { ...decided, act: { ...decided.act, mode: "generic" } } as typeof decided;
    push(
      "actionable-card-gets-no-derived-acts",
      genericReviewResolutionActs(actionable).length === 0,
      `acts=${genericReviewResolutionActs(actionable).length}`,
    );
  }

  // A decision candidate must read like a decision, not like a lifecycle
  // record. The face also owns proposal time so queue age is not confused
  // with projection freshness.
  {
    const area = "decision-candidates";
    const itemId = "dcp_story_fixture";
    const createdAt = "2026-07-11T04:09:15Z";
    const action = {
      id: "plan_decision_candidate_review",
      label: "Plan decision review",
      argv: [
        "compound", "panel", "plan", "--target-kind", "decision_candidate",
        "--target", "candidate_story_fixture", "--domain", "reasoning-lenses",
        "--task", "review falsifier, review trigger, source ownership, and second-order effects",
        "--supervised", "--max-items", "1", "--json",
      ],
      requires_review: true,
      policy_note: "creates review evidence only; decision source and authority stay unchanged",
    } as Json;
    const item = {
      id: itemId,
      area,
      candidate_id: "candidate_story_fixture",
      context_label: "Ruling: High-consequence mutations require an explicit operator decision when recovery is not verified or operator control may be impaired.",
      created_at: createdAt,
      actions: [action],
    } as Json;
    const group = {
      key: "decision-candidates|story",
      area,
      count: 1,
      item_ids: [itemId],
      operator_summary: "Decision lifecycle has candidate Ruling Keep approval boundaries proportional to consequence. Review it?",
      operator_unlock: "decision source changes only after reviewed approval",
      sample: item,
      safe_batch_action: action,
    } as Json;
    const [decided] = buildDecideGroups([group], new Map([[itemId, item]]), null, () => item);
    const actionDescription = decided.face.primaryAction && "description" in decided.face.primaryAction
      ? String(decided.face.primaryAction.description)
      : "";
    const proposedAt = "proposedAt" in decided.face ? String(decided.face.proposedAt) : "";
    const proposedAtLabel = "proposedAtLabel" in decided.face ? String(decided.face.proposedAtLabel) : "";
    push(
      "decision-candidate-face-explains-question-step-and-proposal-time",
      decided.face.label === "Review a proposed operating rule" &&
        decided.body === "Proposed rule: High-consequence mutations require an explicit operator decision when recovery is not verified or operator control may be impaired." &&
        decided.face.whyNow === "This is not the final approval. Compound first needs an independent review before this rule can enter the decision source." &&
        decided.face.owner === "Decision review" &&
        decided.face.primaryAction?.label === "Prepare independent review" &&
        actionDescription === "Creates review evidence only. It does not adopt the rule or change decision authority." &&
        proposedAt === createdAt && proposedAtLabel.length > 0,
      `label=${JSON.stringify(decided.face.label)} body=${JSON.stringify(decided.body)} why=${JSON.stringify(decided.face.whyNow)} action=${JSON.stringify(decided.face.primaryAction)} proposed=${JSON.stringify({ proposedAt, proposedAtLabel })}`,
    );
  }

  // A group with no atomic batch command can still be actionable when every
  // member carries its own certified owner action. Expose those separate
  // actions instead of claiming that no workflow exists.
  {
    const area = "harness-edit-proposals";
    const items = [
      {
        id: "dirprop_story_alpha",
        area,
        operator_summary: "Compound suggests to close the capability suite capability graph parity gap because 7 bench cases miss graph nodes. Review the edit?",
        context_label: "capability edges",
        created_at: "2026-07-10T02:02:00Z",
        actions: [{
          id: "harness-edit.whitelist",
          label: "Allow edits to this surface",
          argv: ["compound", "authority", "harness-edit-review", "/tmp/dirprop_story_alpha.json", "--decision", "whitelist", "--json"],
          requires_review: true,
        }],
      },
      {
        id: "dirprop_story_bravo",
        area,
        operator_summary: "Compound suggests to strengthen capability suite surfacing on the held in split because hit-at-1 is 20% on held in. Review the edit?",
        context_label: "capability edges",
        created_at: "2026-07-11T01:37:35Z",
        actions: [{
          id: "harness-edit.whitelist",
          label: "Allow edits to this surface",
          argv: ["compound", "authority", "harness-edit-review", "/tmp/dirprop_story_bravo.json", "--decision", "whitelist", "--json"],
          requires_review: true,
        }],
      },
    ] as Json[];
    const itemByID = new Map(items.map((item) => [String(item.id), item]));
    const group = {
      key: "harness-edit-proposals|capability_edges",
      area,
      count: 2,
      item_ids: items.map((item) => item.id),
      operator_summary: "Compound found capability graph gaps. Review the edits?",
      operator_unlock: "the capability graph regains the edges the bench expects",
      terminal_reason: "actions are item-specific; no safe batch action exposed",
      sample: items[0],
    } as Json;
    const [decided] = buildDecideGroups([group], itemByID, null, () => items[0]);
    const act = decided.act;
    push(
      "item-specific-certified-actions-are-exposed-without-inventing-a-batch-owner",
      act.mode === "batch-enumerable" &&
        act.label === "Review 2 proposals" &&
        act.effectKind === "record" &&
        act.requiresConfirmation &&
        act.requiresRationale &&
        act.policyNote === "Approving a proposal authorizes only that capability map edit. It does not apply the edit." &&
        act.members.length === 2 &&
        act.members.every((member) => member.actionId === "harness-edit.whitelist") &&
        act.members.map((member) => member.label).join("|") ===
          "Make 7 missing capabilities discoverable to the capability-suite check|Improve the first result for capability-suite's held-in test cases (currently 20% correct)" &&
        act.members.every((member) => "proposedAtLabel" in member && String(member.proposedAtLabel).length > 0) &&
        decided.face.state === "ready" &&
        decided.face.stateLabel === "Ready" &&
        decided.face.label === "Choose which capability map fixes to allow" &&
        decided.body === "2 proposed fixes would help Compound find capabilities it currently misses. Nothing changes until you approve a proposal." &&
        decided.face.whyNow === "Some checks ask for capabilities that Compound cannot find even though they should be available." &&
        decided.face.owner === "Capability map editor" &&
        decided.face.primaryAction?.label === "Review 2 proposals" &&
        decided.face.primaryAction.description === "Select the fixes you want to allow. Each approval authorizes only that map edit; it does not apply the edit.",
      `act=${act.mode}${act.mode === "batch-enumerable" ? ` label=${JSON.stringify(act.label)} members=${JSON.stringify(act.members)}` : ""} face=${JSON.stringify(decided.face)}`,
    );

    const strictItems = items.map((item, index) => {
      const authorityAction = authorityActionFixture(String(item.id));
      const projected = (item.actions as Json[])[0];
      authorityAction.id = projected.id;
      authorityAction.label = projected.label;
      authorityAction.argv = projected.argv;
      authorityAction.requires_review = false;
      authorityAction.policy_authority.wording = `Allowed in current scope · reversible · member ${index + 1}`;
      authorityAction.policy_authority.expected_receipt = `member ${index + 1} review receipt`;
      authorityAction.policy_authority.expected_proof = [`proof:member-${index + 1}`];
      return { ...item, actions: [authorityAction] } as Json;
    });
    const strictGroup = { ...group, sample: strictItems[0] } as Json;
    const strictItemsByID = new Map(strictItems.map((item) => [String(item.id), item]));
    const [strict] = buildDecideGroups(
      [strictGroup],
      strictItemsByID,
      null,
      () => strictItems[0],
      "live",
    );
    const strictAct = strict.act;
    const selectedFaces = strictAct.mode === "batch-enumerable"
      ? enumerableAuthorityFaces(strictAct.members)
      : null;

    const partialItems = structuredClone(strictItems);
    delete partialItems[1].actions[0].action_intent.blast_radius;
    delete partialItems[1].actions[0].policy_authority.blast_radius;
    const partialByID = new Map(partialItems.map((item) => [String(item.id), item]));
    const [partial] = buildDecideGroups(
      [{ ...strictGroup, sample: partialItems[0] }],
      partialByID,
      null,
      () => partialItems[0],
      "live",
    );
    push(
      "strict-no-confirm-enumerable-member-authorities-reach-action-surface-and-malformed-member-stays-unchanged",
      strictAct.mode === "batch-enumerable" &&
        strictAct.members.length === 2 &&
        strictAct.representedCount === 2 &&
        strictAct.unavailableCount === 0 &&
        strictAct.requiresConfirmation === false &&
        strictAct.members.every((member) => member.actionIntentId && member.expectedAuthorityRevision && member.authority) &&
        strict.face.state === "ready" &&
        strict.face.authority == null &&
        selectedFaces?.length === 2 &&
        selectedFaces.every((member, index) =>
          member.authority.wording.endsWith(`member ${index + 1}`) &&
          member.authority.expectedReceipt === `member ${index + 1} review receipt` &&
          member.authority.expectedProof.join(",") === `proof:member-${index + 1}` &&
          authoritySummarySegments(member.authority).length === 13) &&
        partial.act.mode === "batch-enumerable" &&
        partial.act.members.length === 1 &&
        partial.act.members[0].id === String(strictItems[0].id) &&
        partial.face.state === "ready",
      `strict=${strictAct.mode}/${strict.face.state}/faces:${JSON.stringify(selectedFaces)} partial=${partial.act.mode}/${partial.face.state}`,
    );

    const missingItem = structuredClone(items[1]);
    const heldId = "dirprop_story_charlie";
    const heldAction = authorityActionFixture(heldId);
    heldAction.id = "harness-edit.whitelist";
    heldAction.label = "Allow edits to this held surface";
    heldAction.argv = ["compound", "authority", "harness-edit-review", `/tmp/${heldId}.json`, "--decision", "whitelist", "--json"];
    heldAction.requires_review = true;
    heldAction.policy_decision.outcome = "hold";
    heldAction.policy_decision.standing_grant_id = "";
    heldAction.policy_authority.allowed = false;
    heldAction.policy_authority.policy_outcome = "hold";
    heldAction.policy_authority.standing_grant_id = "";
    heldAction.policy_authority.wording = "Needs your decision · current policy holds this action";
    const heldItem = {
      ...structuredClone(items[1]),
      id: heldId,
      operator_summary: "Keep the held capability surface unchanged",
      context_label: heldId,
      actions: [heldAction],
    } as Json;
    const mixedItems = [strictItems[0], missingItem, heldItem];
    const mixedGroup = {
      ...group,
      count: mixedItems.length,
      item_ids: mixedItems.map((item) => String(item.id)),
      sample: mixedItems[0],
    } as Json;
    const mixedByID = new Map(mixedItems.map((item) => [String(item.id), item]));
    const [mixed] = buildDecideGroups([mixedGroup], mixedByID, null, () => mixedItems[0], "live");
    const mixedRequest = mixed.act.mode === "batch-enumerable"
      ? buildEnumerableActionRequest(area, mixed.act.members[0], mixed.act.requiresConfirmation, "")
      : null;

    const zeroReadyItems = [missingItem, heldItem];
    const zeroReadyGroup = {
      ...group,
      count: zeroReadyItems.length,
      item_ids: zeroReadyItems.map((item) => String(item.id)),
      sample: zeroReadyItems[0],
    } as Json;
    const zeroReadyByID = new Map(zeroReadyItems.map((item) => [String(item.id), item]));
    const [zeroReady] = buildDecideGroups([zeroReadyGroup], zeroReadyByID, null, () => zeroReadyItems[0], "live");

    push(
      "mixed-authority-b2-exposes-only-complete-current-allowed-members",
      mixed.act.mode === "batch-enumerable" &&
        mixed.count === 3 &&
        mixed.members.length === 3 &&
        mixed.act.members.length === 1 &&
        mixed.act.representedCount === 3 &&
        mixed.act.unavailableCount === 2 &&
        mixed.act.members[0].id === String(strictItems[0].id) &&
        mixed.act.members[0].authority?.current === true &&
        mixed.act.members[0].authority?.allowed === true &&
        mixed.act.members[0].authority?.expectedReceipt === "member 1 review receipt" &&
        mixed.act.members[0].authority?.expectedProof.join(",") === "proof:member-1" &&
        !mixed.act.members.some((member) => member.id === String(missingItem.id) || member.id === heldId) &&
        mixed.members.every((member) => member.argv === null) &&
        mixed.face.state === "ready" &&
        mixedRequest?.itemId === mixed.act.members[0].id &&
        mixedRequest?.actionId === mixed.act.members[0].actionId &&
        mixedRequest?.expectedRevision === mixed.act.members[0].expectedRevision &&
        mixedRequest?.actionIntentId === mixed.act.members[0].actionIntentId &&
        mixedRequest?.expectedAuthorityRevision === mixed.act.members[0].expectedAuthorityRevision &&
        mixedRequest?.confirmed === false &&
        zeroReady.act.mode === "copy" &&
        zeroReady.face.state === "waiting",
      `mixed=${mixed.act.mode}/${mixed.face.state}/${mixed.act.mode === "batch-enumerable" ? mixed.act.members.map((member) => member.id).join(",") : "none"} total=${mixed.count}/${mixed.members.length} request=${JSON.stringify(mixedRequest)} zero=${zeroReady.act.mode}/${zeroReady.face.state}`,
    );
  }

  {
    const runId = "prun_20260723T120000Z_fixture";
    const panelItem = (suffix: string, claim: string, evidenceRef: string): Json => {
      const id = `panel_verdict_fixture_${suffix}`;
      const action = authorityActionFixture(id);
      action.id = "panel_verdict_record_member";
      action.label = "Record this verdict";
      action.argv = [
        "compound",
        "panel",
        "verdict-record",
        "--review",
        `/tmp/${id}.json`,
        "--reviewed-by",
        "operator",
      ];
      action.requires_review = true;
      action.policy_authority.wording = "Allowed by current policy";
      action.policy_authority.expected_receipt = `Recorded human review for ${id}.`;
      return {
        id,
        area: "panel-verdicts",
        created_at: "2026-07-23T12:05:00Z",
        group_key: `panel-verdicts|${runId}|needs_human`,
        context_label: "Synthetic source statement.",
        status: "review_required",
        readiness: "needs_review",
        authority_work_id: "work_review_area_panel_verdicts",
        evidence_refs: [evidenceRef],
        actions: [action],
        decision_card: {
          claim,
          why: [
            {
              label: "panel rationale",
              detail: "Evidence strength is low and one lens disagrees, so an operator must decide.",
            },
            {
              label: "evidence strength",
              question: "How well is this supported?",
              severity: "high",
              detail: "knowledge:fixture:tweet:0000000000000000001 reports the synthetic claim.",
            },
          ],
          choices: [
            { id: "promote", label: "Promote", effect: "Becomes ambient canon." },
            { id: "park", label: "Park", effect: "Remains evidence, not ambient." },
            { id: "reject", label: "Reject", effect: "Discarded for this memory lane." },
            { id: "no_promote", label: "No-promote", effect: "Declined so echoes stop." },
          ],
          panel_lean: "needs_human (confidence 0.70)",
          optional_rationale: true,
          canonical_action_hint: "Record review evidence before a separate memory adjudication.",
          boundary: ["read-only projection", "operator choice required before mutation"],
          meta: {
            run_id: runId,
            target_kind: "memory_candidate",
            target_ref: `cand_fixture_${suffix}`,
            verdict_id: id,
          },
        },
      };
    };
    const first = panelItem("alpha", "Synthetic claim alpha needs adjudication.", "route_fixture_alpha");
    const second = panelItem("bravo", "Synthetic claim bravo needs adjudication.", "route_fixture_bravo");
    const items = [first, second];
    const itemByID = new Map(items.map((item) => [String(item.id), item]));
    const group = {
      key: `panel-verdicts|${runId}|needs_human`,
      area: "panel-verdicts",
      count: 2,
      item_ids: items.map((item) => item.id),
      operator_summary: "The judge panel needs your call on this claim.",
      operator_unlock: "memory gets a reviewed answer",
      sample: first,
    } as Json;
    const [decided] = buildDecideGroups([group], itemByID, null, () => first, "live");

    push(
      "panel-decision-card-remains-claim-granular-with-truthful-proof-inputs",
      decided.title === "Synthetic claim alpha needs adjudication." &&
        decided.objectText === "Synthetic claim alpha needs adjudication." &&
        decided.body === "Evidence strength is low and one lens disagrees, so an operator must decide." &&
        decided.face.owner === "Panel review" &&
        decided.face.confidence?.value === 0.7 &&
        decided.evidence.length === 1 &&
        decided.evidence[0]?.ref === "route_fixture_alpha" &&
        decided.panel?.runId === runId &&
        decided.panel.disposition === "needs_human" &&
        decided.panel.reviews.length === 2 &&
        decided.panel.reviews[0]?.sourceRef === "tweet:0000000000000000001" &&
        decided.panel.reviews[0]?.choices.map((choice) => choice.id).join(",") ===
          "promote,park,reject,no_promote" &&
        decided.panel.reviews[0]?.member.actionId === "panel_verdict_record_member" &&
        decided.panel.reviews[0]?.member.authority?.current === true &&
        decided.panel.reviews[1]?.claim === "Synthetic claim bravo needs adjudication.",
      `title=${decided.title} evidence=${decided.evidence.map((row) => row.ref).join(",")} panel=${JSON.stringify(decided.panel)}`,
    );

    const churnedGroup = {
      ...group,
      item_ids: ["panel_verdict_refreshed_member"],
      review_item_ids: items.map((item) => item.id),
      sample: undefined,
      safe_batch_action: undefined,
    } as Json;
    const [churned] = buildDecideGroups(
      [churnedGroup],
      itemByID,
      null,
      () => ({
        id: churnedGroup.key,
        area: "panel-verdicts",
        operator_summary: churnedGroup.operator_summary,
      }),
      "live",
    );

    push(
      "panel-proof-continuity-survives-cross-projection-member-churn-read-only",
      churned.panel?.projectionAligned === false &&
        churned.panel.reviews.length === 2 &&
        churned.title === "Synthetic claim alpha needs adjudication." &&
        churned.act.mode === "copy" &&
        churned.face.state === "waiting" &&
        churned.panel.reviews.every((review) => review.member.actionId === ""),
      `title=${churned.title} act=${churned.act.mode} state=${churned.face.state} panel=${JSON.stringify(churned.panel)}`,
    );
  }

  const failed = checks.filter((c) => !c.ok);
  return {
    ok: failed.length === 0,
    details: checks.map((c) => `${c.ok ? "ok" : "FAIL"} ${c.name} — ${c.detail}`),
  };
}
