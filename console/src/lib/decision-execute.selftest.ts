/*
 * Falsifiers for the execute-route ↔ decide-model agreement on WHO may act.
 * A bespoke owner with zero options means the current item has no compatible
 * workflow. A projected evidence/record command must not be substituted for a
 * different represented effect (lived: route demote cards exposed the
 * hold-record command as if it could change routing authority).
 *
 * These checks pin the agreement contract at the pure layer (no spawned CLI):
 * an exact authority-bound projected action wins; a legacy bespoke body is
 * not an identity-free mutation path.
 * Runs under bun via scripts/decision-execute-smoke.mjs.
 */

import {
  actionAuthorityRequestError,
  actionAuthorityAdmissionError,
  decisionCommandFailureResponse,
  decisionFailureStatus,
  decisionStaleRevisionResponse,
  executeDecision,
  exactActionOwnerConstraint,
  extractDecisionRefs,
  genericActionRevisionMatches,
  genericActionAllowedForOwner,
  genericActionRejectionResponse,
  missingBespokeConfirmation,
  renderedRevisionMatches,
  reviewResolutionCommandFailureResponse,
  resolveReviewResolutionAction,
} from "./decision-execute";
import { headlineAct, type HeadlineActDescriptor } from "./briefing";
import type { AttentionGroup } from "./attention";
import {
  buildDecideActionRequest,
  buildEnumerableActionRequest,
  buildHeadlineActionRequest,
  type ClientDecideAct,
  decideActIdentity,
  headlineActIdentity,
} from "./governed-action-client";
import { specFor } from "./decision-registry";
import { genericActionRevision, reviewResolutionRevision } from "./generic-actions";
import { isStandingLawCandidate } from "./memory-data";
import { str, type Json } from "./compound";

type Check = { name: string; ok: boolean; detail: string };

function routePromotionItem(recommendation: string): Json {
  return {
    id: "route_promotion_selftest01",
    area: "route-promotions",
    group_key: `route-promotions|${recommendation}|classify`,
    json_path: "/tmp/route_promotion_selftest01.json",
  };
}

export async function runDecisionExecuteSelfTest(): Promise<{ ok: boolean; details: string[] }> {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  // ObjectFace requests carry all three optimistic identities. Missing or
  // malformed identity is a bad request; drift against the freshly resolved
  // owner action is a conflict. Projection failure and a current hold keep
  // their distinct retry/decision status.
  {
    const authorityRevision = `policy-authority-v1:sha256:${"a".repeat(64)}`;
    const action = {
      revision: "generic-action-v2:[fixture]",
      actionIntentId: "act_authority_fixture",
      authorityRevision,
    };
    const missing = actionAuthorityRequestError(action, {
      expectedRevision: "",
      actionIntentId: "",
      expectedAuthorityRevision: "",
    });
    const matching = actionAuthorityRequestError(action, {
      expectedRevision: action.revision,
      actionIntentId: action.actionIntentId,
      expectedAuthorityRevision: authorityRevision,
    });
    const actionDrift = actionAuthorityRequestError(action, {
      expectedRevision: "generic-action-v2:[other]",
      actionIntentId: action.actionIntentId,
      expectedAuthorityRevision: authorityRevision,
    });
    const intentDrift = actionAuthorityRequestError(action, {
      expectedRevision: action.revision,
      actionIntentId: "act_other",
      expectedAuthorityRevision: authorityRevision,
    });
    const authorityDrift = actionAuthorityRequestError(action, {
      expectedRevision: action.revision,
      actionIntentId: action.actionIntentId,
      expectedAuthorityRevision: `policy-authority-v1:sha256:${"b".repeat(64)}`,
    });
    const missingPayload = missing ? await missing.json() as { error_code?: string } : {};
    const driftPayloads = await Promise.all(
      [actionDrift, intentDrift, authorityDrift].map(async (response) =>
        response ? await response.json() as { error_code?: string } : {}),
    );
    const hold = genericActionRejectionResponse({ gate: "authority-not-allowed", message: "Needs your decision · current policy holds this action" });
    const unavailable = genericActionRejectionResponse({ gate: "projection-unavailable", message: "owner projection is not live" });
    push(
      "authority-request-identity-is-required-and-drift-bound",
      missing?.status === 400 &&
        missingPayload.error_code === "malformed_authority_request" &&
        matching === null &&
        [actionDrift, intentDrift, authorityDrift].every((response) => response?.status === 409) &&
        driftPayloads.every((payload) => payload.error_code === "authority_drift") &&
        hold.status === 409 &&
        unavailable.status === 503,
      `missing=${missing?.status}/${missingPayload.error_code} matching=${matching == null} drift=${[actionDrift, intentDrift, authorityDrift].map((response, index) => `${response?.status}/${driftPayloads[index]?.error_code}`).join(",")} hold=${hold.status} unavailable=${unavailable.status}`,
    );
  }

  // Every ObjectFace transport shape carries the same ActionIntent and
  // AuthorityFace identity, including enumerable members and the headline.
  {
    const authorityRevision = `policy-authority-v1:sha256:${"c".repeat(64)}`;
    const authorityIdentity = {
      actionIntentId: "act_transport_fixture",
      expectedAuthorityRevision: authorityRevision,
    };
    const generic = {
      mode: "generic" as const,
      area: "source-control-cleanup",
      itemId: "scr_transport",
      actionId: "record_source_control_cleanup_review",
      expectedRevision: "generic-action-v2:[transport]",
      ...authorityIdentity,
      label: "Record review",
      argv: ["authority", "dirty-review", "/tmp/review.yaml"],
      requiresConfirmation: true,
      copyCommand: "compound reviews --area source-control-cleanup",
      effectKind: "record" as const,
    };
    const bespoke = {
      mode: "bespoke" as const,
      area: "source-control-cleanup",
      itemId: "scr_transport",
      option: "",
      options: [],
      defaultOption: "",
      label: "Record review",
      requiresConfirmation: true,
      argv: ["authority", "dirty-review", "/tmp/review.yaml"],
      policyNote: "",
      fields: [],
      expectedRevision: "generic-action-v2:[transport]",
      ...authorityIdentity,
      supportsPreview: false,
      effectKind: "record" as const,
    };
    const member = {
      id: "scr_transport",
      actionId: "record_source_control_cleanup_review",
      expectedRevision: "generic-action-v2:[transport]",
      ...authorityIdentity,
    };
    const headlineDescriptor = {
      pathway: "generic" as const,
      area: "source-control-cleanup",
      itemId: "scr_transport",
      actionId: "record_source_control_cleanup_review",
      expectedRevision: "generic-action-v2:[transport]",
      ...authorityIdentity,
      label: "Record review",
      argv: ["compound", "authority", "dirty-review", "/tmp/review.yaml"],
      requiresConfirmation: true,
      rationaleRequired: false,
    };
    const genericBody = buildDecideActionRequest(generic, "", {}, "confirm");
    const bespokeBody = buildDecideActionRequest(bespoke, "", {}, "confirm");
    const memberBody = buildEnumerableActionRequest("source-control-cleanup", member, true, "");
    const headlineBody = buildHeadlineActionRequest(headlineDescriptor, "", "confirm");
    const bodies = [genericBody, bespokeBody, memberBody, headlineBody];
    const changedIdentity = decideActIdentity({ ...generic, expectedAuthorityRevision: `policy-authority-v1:sha256:${"d".repeat(64)}` });
    push(
      "authority-identity-propagates-across-objectface-transports",
      bodies.every((body) =>
        body.actionIntentId === authorityIdentity.actionIntentId &&
        body.expectedAuthorityRevision === authorityRevision) &&
        decideActIdentity(generic) !== changedIdentity &&
        String(memberBody.actionId) === member.actionId,
      `bodies=${JSON.stringify(bodies)} identity_changed=${decideActIdentity(generic) !== changedIdentity}`,
    );
  }

  {
    const owner = specFor("source-control-cleanup");
    const item = { id: "scr_fixture", json_path: "/tmp/scr_fixture.json", actions: [{ argv: ["compound", "authority", "dirty-review", "/tmp/scr_fixture.review.yaml"] }] } as Json;
    const options = owner?.optionsFor?.(item) ?? [];
    push("source-control-has-explicit-decision-owner", options.length === 4 && owner?.requiresConfirmation === true, `options=${options.map((option) => option.value).join(",")}`);
  }

  // An exact stamped action keeps precedence over the editable legacy form,
  // but it must satisfy both the generic gates and the area's narrower owner
  // contract. The legacy owner may only narrow; it cannot rebuild the action.
  {
    const owner = specFor("source-control-cleanup");
    const exact = {
      argv: ["authority", "dirty-review", "/tmp/scr_fixture.review.yaml", "--json"],
      requiresReview: false,
    };
    const allowed = exactActionOwnerConstraint(owner, "scr_fixture", exact);
    const invalidTarget = exactActionOwnerConstraint(owner, "not-a-source-review", exact);
    const foreignVerb = exactActionOwnerConstraint(owner, "scr_fixture", {
      ...exact,
      argv: ["authority", "job-health-review", "/tmp/scr_fixture.review.yaml", "--json"],
    });
    push(
      "exact-projected-action-is-narrowed-by-bespoke-owner",
      allowed.ok && allowed.requiresConfirmation === true &&
        !invalidTarget.ok && invalidTarget.status === 400 &&
        !foreignVerb.ok && foreignVerb.status === 403,
      `allowed=${JSON.stringify(allowed)} invalid_target=${JSON.stringify(invalidTarget)} foreign_verb=${JSON.stringify(foreignVerb)}`,
    );
  }

  // Untouched legacy callers remain compatible until their concrete producer
  // cuts over. Any request that claims stamped authority must carry the whole
  // tuple; partial identity cannot downgrade itself into a legacy request.
  {
    const legacy = actionAuthorityAdmissionError("", false, {
      expectedRevision: "legacy-owner-revision",
      actionIntentId: "",
      expectedAuthorityRevision: "",
    });
    const partial = actionAuthorityAdmissionError("", false, {
      expectedRevision: "generic-action-v2:[legacy]",
      actionIntentId: "act_legacy_bespoke",
      expectedAuthorityRevision: `policy-authority-v1:sha256:${"f".repeat(64)}`,
    });
    const missingTuple = actionAuthorityAdmissionError("record_source_control_cleanup_review", false, {
      expectedRevision: "",
      actionIntentId: "",
      expectedAuthorityRevision: "",
    });
    const preview = actionAuthorityAdmissionError("", true, {
      expectedRevision: "",
      actionIntentId: "",
      expectedAuthorityRevision: "",
    });
    const partialPreview = actionAuthorityAdmissionError("", true, {
      expectedRevision: "generic-action-v2:[preview]",
      actionIntentId: "act_preview_partial",
      expectedAuthorityRevision: "",
    });
    const fullPreview = actionAuthorityAdmissionError("record_source_control_cleanup_review", true, {
      expectedRevision: "generic-action-v2:[preview]",
      actionIntentId: "act_preview_complete",
      expectedAuthorityRevision: `policy-authority-v1:sha256:${"e".repeat(64)}`,
    });
    const partialPayload = partial ? await partial.json() as { error_code?: string } : {};
    const missingPayload = missingTuple ? await missingTuple.json() as { error_code?: string } : {};
    const partialPreviewPayload = partialPreview ? await partialPreview.json() as { error_code?: string } : {};
    const fullPreviewPayload = fullPreview ? await fullPreview.json() as { error_code?: string } : {};
    push(
      "legacy-callers-remain-compatible-while-stamped-claims-require-full-identity",
      legacy == null && preview == null &&
        partial?.status === 400 && partialPayload.error_code === "malformed_authority_request" &&
        missingTuple?.status === 400 && missingPayload.error_code === "malformed_authority_request" &&
        partialPreview?.status === 400 && partialPreviewPayload.error_code === "malformed_authority_request" &&
        fullPreview?.status === 400 && fullPreviewPayload.error_code === "preview_unsupported",
      `legacy=${legacy == null} preview=${preview == null} partial=${partial?.status}/${partialPayload.error_code} missing=${missingTuple?.status}/${missingPayload.error_code} partial_preview=${partialPreview?.status}/${partialPreviewPayload.error_code} full_preview=${fullPreview?.status}/${fullPreviewPayload.error_code}`,
    );
  }

  const spec = specFor("route-promotions");
  if (!spec || !spec.optionsFor) {
    return { ok: false, details: ["FAIL route-promotions spec or optionsFor missing"] };
  }

  // 1. Demote has no effect-compatible bespoke option. The projected
  //    promotion-record action records holds only, so it must stay unavailable.
  {
    const options = spec.optionsFor(routePromotionItem("demote"));
    push(
      "demote-zero-options-does-not-use-generic-record-action",
      options.length === 0 && !genericActionAllowedForOwner(true, "route_promotion_record"),
      `options=${options.length} generic=${genericActionAllowedForOwner(true, "route_promotion_record")}`,
    );
  }

  // 8. A stale revision caught by the server preflight has the same stable
  //    conflict code as a stale revision reported by the CLI race path.
  {
    const response = decisionStaleRevisionResponse();
    const payload = await response.json() as { ok?: boolean; error_code?: string };
    push(
      "lifecycle-preflight-stale-carries-stable-code",
      response.status === 409 && payload.ok === false && payload.error_code === "stale_revision",
      `response=${response.status} code=${payload.error_code ?? "missing"}`,
    );
  }

  // 7. A lifecycle conflict discovered by the CLI after the server's fresh
  //    projection check is still an optimistic-concurrency conflict, never a
  //    misleading HTTP 200. Other command failures remain server errors.
  {
    const area = "capability-lifecycle-proposals";
    const conflict = JSON.stringify({
      schema_version: "compound.capability.lifecycle_review_error.v0.1",
      status: "error",
      error_code: "disposition_conflict",
      error: "refresh and review the current proposal",
    });
    const response = decisionCommandFailureResponse(area, { message: "command failed", stdout: conflict, stderr: "" }, false);
    const payload = await response.json() as { ok?: boolean; error_code?: string };
    push(
      "lifecycle-cli-conflicts-map-to-http-409",
      response.status === 409 && payload.ok === false && payload.error_code === "disposition_conflict" &&
        decisionFailureStatus(area, JSON.stringify({ error_code: "stale_revision" })) === 409 &&
        decisionFailureStatus(area, JSON.stringify({ error_code: "command_failed" })) === 500,
      `response=${response.status} code=${payload.error_code ?? "missing"}; unknown failures are 500`,
    );
  }

  // 2. hold_failing is another mutating recommendation, so the same owner
  //    boundary applies.
  {
    const options = spec.optionsFor(routePromotionItem("hold_failing"));
    push(
      "hold-failing-zero-options-does-not-use-generic-record-action",
      options.length === 0 && !genericActionAllowedForOwner(true, "route_promotion_record"),
      `options=${options.length}`,
    );
  }

  // 3. hold_insufficient_evidence: the bespoke spec CAN act (record_hold) —
  //    the bespoke path must keep winning; fallback must refuse even with an
  //    actionId present ("a bespoke spec always wins when it can act").
  {
    const options = spec.optionsFor(routePromotionItem("hold_insufficient_evidence"));
    push(
      "bespoke-wins-when-it-can-act",
      options.length === 1 && !genericActionAllowedForOwner(true, "route_promotion_record"),
      `options=${options.length} generic=${genericActionAllowedForOwner(true, "route_promotion_record")}`,
    );
  }

  // 4. Generic actions remain available for ownerless areas only, and still
  //    require a projected action id.
  {
    push(
      "generic-actions-require-ownerless-area-and-action-id",
      genericActionAllowedForOwner(false, "record_job_health_review") &&
        !genericActionAllowedForOwner(false, "") &&
        !genericActionAllowedForOwner(true, "record_job_health_review"),
      "ownerless+id accepted; missing id or bespoke owner refused",
    );
  }

  // 5. Structured JSON output must retain non-ID-shaped proof refs and the
  //    real `pol_` prefix. The old text-only regex silently dropped both.
  {
    const refs = extractDecisionRefs(JSON.stringify({
      schema_version: "compound.work_create_plan.v0.2",
      proof_refs: ["capability_lifecycle_proposal:clp_fixture", "sha256:abc123"],
      disposition: {
        id: "cld_fixture",
        work_id: "work_fixture",
        action_intent_id: "act_fixture",
        policy_decision_id: "pol_fixture",
      },
    }));
    push(
      "structured-proof-refs-retained",
      refs.proof_refs.includes("capability_lifecycle_proposal:clp_fixture") &&
        refs.proof_refs.includes("sha256:abc123") &&
        refs.work_ids.includes("work_fixture") &&
        !refs.work_ids.includes("work_create_plan") &&
        refs.action_intent_ids.includes("act_fixture") &&
        refs.policy_decision_ids.includes("pol_fixture") &&
        refs.decision_ids.includes("cld_fixture"),
      JSON.stringify(refs),
    );
  }

  // 6. Optimistic concurrency compares the revision rendered to the operator
  //    with the freshly resolved owner revision. Missing or changed is stale.
  {
    push(
      "rendered-revision-must-match-fresh-owner",
      renderedRevisionMatches("hash_a", "hash_a", true) &&
        !renderedRevisionMatches("hash_b", "hash_a", true) &&
        !renderedRevisionMatches("hash_a", "", true) &&
        renderedRevisionMatches("", "", false),
      "matching accepted; changed/missing rejected; non-versioned actions unaffected",
    );
  }

  // 9. A reserved lifecycle proposal resumes only through the freshly
  //    projected named action. Editable client fields never rebuild this argv.
  {
    const lifecycle = specFor("capability-lifecycle-proposals");
    const hash = "b".repeat(64);
    const projected = [
      "compound", "capability", "lifecycle-review",
      "--proposal", "clp_reserved",
      "--expected-hash", hash,
      "--decision", "open_work",
      "--rationale", "resume the owner-reserved admission",
      "--actor", "operator-console",
      "--json",
    ];
    const item = {
      id: "clp_reserved",
      actions: [{ id: "resume_open_work", argv: projected }],
      decision_card: {
        choices: [{ id: "resume_open_work", label: "Resume tracked work", effect: "Resume the reserved admission." }],
        meta: { proposal_hash: hash },
      },
    } as Json;
    const options = lifecycle?.optionsFor?.(item) ?? [];
    const argv = lifecycle?.buildArgv?.({
      itemId: "clp_reserved",
      item,
      option: "resume_open_work",
      fields: { rationale: "client text must not replace owner argv" },
      expectedRevision: hash,
    });
    const rationaleField = lifecycle?.fields.find((field) => field.name === "rationale");
    push(
      "reserved-lifecycle-resolves-fresh-named-action",
      options.map((option) => option.value).join(",") === "resume_open_work" &&
        argv?.join(" ") === projected.slice(1).join(" ") &&
        Boolean(rationaleField?.showForOptions) &&
        !rationaleField?.showForOptions?.includes("resume_open_work"),
      `options=${options.map((option) => option.value).join(",")} argv=${argv?.join(" ") ?? "missing"}`,
    );
  }

  // 10. The new server confirmation marker belongs to owners that declare it.
  //     Existing bespoke dashboard actions keep their established one-click
  //     contract; lifecycle preview remains write-free and confirmation-free.
  {
    const lifecycle = specFor("capability-lifecycle-proposals");
    const existing = specFor("work-advance");
    push(
      "bespoke-confirmation-is-owner-scoped",
      lifecycle?.requiresConfirmation === true &&
        existing?.requiresConfirmation !== true &&
        missingBespokeConfirmation(Boolean(lifecycle?.requiresConfirmation), false, false) &&
        !missingBespokeConfirmation(Boolean(lifecycle?.requiresConfirmation), true, false) &&
        !missingBespokeConfirmation(Boolean(lifecycle?.requiresConfirmation), false, true) &&
        !missingBespokeConfirmation(Boolean(existing?.requiresConfirmation), false, false),
      `lifecycle=${Boolean(lifecycle?.requiresConfirmation)} existing=${Boolean(existing?.requiresConfirmation)}`,
    );
  }

  // A console operator is not an executor. Until the owner can select and
  // atomically lease to a real executor, ready work must stay dispatchable.
  {
    const workId = "work_ready_fixture";
    const work = specFor("work-advance");
    const item = { id: workId, state: "ready" } as Json;
    const options = work?.optionsFor?.(item) ?? [];
    const startArgv = work?.buildArgv?.({
      itemId: workId,
      item,
      option: "claimed",
      fields: {},
    });
    const heldArgv = work?.buildArgv?.({
      itemId: workId,
      item,
      option: "held",
      fields: {},
    });
    push(
      "ready-work-has-no-nonexecutor-claim-action",
      !options.some((option) => option.value === "claimed") &&
        startArgv == null &&
        heldArgv?.join("\u0000") ===
          ["work", "advance", workId, "--to", "held", "--by", "operator-console"].join("\u0000"),
      `options=${options.map((option) => option.value).join(",")} start=${startArgv?.join(" ") ?? "none"} held=${heldArgv?.join(" ") ?? "none"}`,
    );
  }

  // 11. Preview is a bespoke owner contract. An actionId-only preview claims
  //     stamped authority without its tuple and is rejected before resolution.
  {
    const response = await executeDecision({
      area: "generic-preview-fixture",
      itemId: "fixture_item",
      actionId: "future_action",
      preview: true,
    });
    const payload = await response.json() as { ok?: boolean; error_code?: string };
    push(
      "generic-preview-rejected-before-resolution",
      response.status === 400 && payload.ok === false && payload.error_code === "malformed_authority_request",
      `response=${response.status} code=${payload.error_code ?? "missing"}`,
    );
  }

  // 12. Panel mutations are server-confirmed owners, and Briefing recognizes
  //     panel apply only from the exact action id plus exact argv shape.
  {
    const base: Omit<AttentionGroup, "action"> = {
      key: "panel-verdicts|prun_fixture|needs_human",
      area: "panel-verdicts",
      band: "needs_you",
      source: "reviews",
      kind: "review",
      count: 1,
      operator_summary: "Review the panel run.",
      operator_unlock: "",
      blast_radius: "local_reversible",
      reversibility: "reversible",
      leverage: 1,
      oldest_created_at: "",
      terminal_reason: "",
      item_ids: ["fixture_item"],
      freshness: { generated_at: "", source: "fixture" },
    };
    const future = headlineAct({
      ...base,
      action: {
        id: "future_panel_record",
        label: "Record panel review",
        argv: ["compound", "panel", "run", "--plan", "prun_fixture", "--apply"],
        kind: "batch",
        requires_confirmation: true,
      },
    });
    const exact = headlineAct({
      ...base,
      action: {
        id: "panel_run_apply",
        label: "Apply panel run",
        argv: ["compound", "panel", "run", "--plan", "prun_fixture", "--apply"],
        kind: "batch",
        requires_confirmation: false,
      },
    });
    const grouped = headlineAct({
      ...base,
      action: {
        id: "panel_verdict_record_group",
        label: "Record panel reviews",
        argv: [
          "compound", "panel", "verdict-record", "--group", base.key,
          "--rationale", "reviewed current panel evidence", "--reviewed-by", "operator-fixture",
        ],
        kind: "batch",
        requires_confirmation: true,
      },
    });
    const groupedBody = grouped ? buildHeadlineActionRequest(grouped, "", "confirm") : {};
    const groupedFields = groupedBody.fields as Record<string, string> | undefined;
    push(
      "panel-owner-requires-confirmation-and-exact-action-shape",
      specFor("panel-apply")?.requiresConfirmation === true &&
        specFor("panel-verdict-record-group")?.requiresConfirmation === true &&
        future === null &&
        exact?.area === "panel-apply" &&
        exact.requiresConfirmation &&
        groupedFields?.rationale === "reviewed current panel evidence" &&
        groupedFields.reviewed_by === "operator-fixture",
      `panel=${Boolean(specFor("panel-apply")?.requiresConfirmation)} group=${Boolean(specFor("panel-verdict-record-group")?.requiresConfirmation)} future=${future?.area ?? "null"} exactConfirm=${Boolean(exact?.requiresConfirmation)} groupedFields=${JSON.stringify(groupedFields)}`,
    );
  }

  // 13. First-click requests never claim operator confirmation. Only the
  //     explicit modal confirmation phase may set confirmed=true, in both
  //     Decide and Briefing.
  {
    const decideAct: ClientDecideAct = {
      mode: "generic",
      area: "job-health",
      itemId: "job_fixture",
      actionId: "record_job_health_review",
      expectedRevision: "revision_fixture",
      label: "Record review",
      argv: ["authority", "job-health-review", "/tmp/review.yaml"],
      requiresConfirmation: false,
      copyCommand: "compound reviews --area job-health",
      effectKind: "record",
    };
    const headlineDescriptor: HeadlineActDescriptor = {
      pathway: "generic",
      area: "job-health",
      itemId: "job_fixture",
      actionId: "record_job_health_review",
      expectedRevision: "revision_fixture",
      label: "Record review",
      argv: ["compound", "authority", "job-health-review", "/tmp/review.yaml"],
      requiresConfirmation: false,
      rationaleRequired: false,
    };
    const decideDirect = buildDecideActionRequest(decideAct, "", {}, "direct");
    const decideConfirm = buildDecideActionRequest(decideAct, "", {}, "confirm");
    const headlineDirect = buildHeadlineActionRequest(headlineDescriptor, "", "direct");
    const headlineConfirm = buildHeadlineActionRequest(headlineDescriptor, "", "confirm");
    push(
      "only-modal-submit-claims-confirmation",
      decideDirect.confirmed === false &&
        decideConfirm.confirmed === true &&
        decideDirect.expectedRevision === "revision_fixture" &&
        headlineDirect.confirmed === false &&
        headlineConfirm.confirmed === true &&
        headlineDirect.expectedRevision === "revision_fixture",
      `decide=${String(decideDirect.confirmed)}/${String(decideConfirm.confirmed)}:${String(decideDirect.expectedRevision)} headline=${String(headlineDirect.confirmed)}/${String(headlineConfirm.confirmed)}:${String(headlineDirect.expectedRevision)}`,
    );
  }

  // 14. Action identity includes the actual target. React keys based on this
  //     value remount draft controls when one stable group advances A -> B.
  {
    const decideA: ClientDecideAct = {
      mode: "generic",
      area: "job-health",
      itemId: "job_a",
      actionId: "record_job_health_review",
      expectedRevision: "revision_a",
      label: "Record review",
      argv: ["authority", "job-health-review", "/tmp/a.yaml"],
      requiresConfirmation: true,
      copyCommand: "compound reviews --area job-health",
      effectKind: "record",
    };
    const decideB: ClientDecideAct = { ...decideA, itemId: "job_b", expectedRevision: "revision_b", argv: ["authority", "job-health-review", "/tmp/b.yaml"] };
    const headlineA: HeadlineActDescriptor = {
      pathway: "generic",
      area: "job-health",
      itemId: "job_a",
      actionId: "record_job_health_review",
      label: "Record review",
      argv: ["compound", "authority", "job-health-review", "/tmp/a.yaml"],
      requiresConfirmation: true,
      rationaleRequired: false,
    };
    const headlineB: HeadlineActDescriptor = { ...headlineA, itemId: "job_b", argv: ["compound", "authority", "job-health-review", "/tmp/b.yaml"] };
    push(
      "action-identity-changes-with-target",
      decideActIdentity(decideA) !== decideActIdentity(decideB) &&
        headlineActIdentity(headlineA) !== headlineActIdentity(headlineB),
      `decide=${decideActIdentity(decideA)} -> ${decideActIdentity(decideB)} headline=${headlineActIdentity(headlineA)} -> ${headlineActIdentity(headlineB)}`,
    );
  }

  // 15. Briefing cannot route around a bespoke owner. A clean projected route
  //     record action for a demote group remains unavailable because that
  //     owner's current item has no effect-compatible option.
  {
    const routeHeadline = headlineAct({
      key: "route-promotions|demote|classify",
      area: "route-promotions",
      band: "needs_you",
      source: "reviews",
      kind: "review",
      count: 1,
      operator_summary: "Demote this route?",
      operator_unlock: "",
      blast_radius: "local_reversible",
      reversibility: "reversible",
      leverage: 1,
      oldest_created_at: "",
      action: {
        id: "route_promotion_record",
        label: "Record route review",
        argv: ["compound", "route", "promotion-record", "--review", "/tmp/route.json"],
        kind: "one_click",
        requires_confirmation: true,
      },
      terminal_reason: "",
      item_ids: ["route_promotion_fixture01"],
      freshness: { generated_at: "", source: "fixture" },
    });
    push(
      "briefing-honors-bespoke-owner-precedence",
      routeHeadline === null,
      `headline=${routeHeadline?.pathway ?? "null"}`,
    );
  }

  // 16. Generic confirmation is optimistic-concurrency bound to the rendered
  //     owner action. Field-name normalization keeps Attention and Reviews
  //     projections equivalent; any argv drift or missing revision is stale.
  {
    const rendered = genericActionRevision({
      id: "record_job_health_review",
      label: "Record job-health review",
      argv: ["compound", "authority", "job-health-review", "/tmp/a.yaml"],
      requires_confirmation: true,
    });
    const sameFresh = genericActionRevision({
      id: "record_job_health_review",
      label: "Record job-health review",
      argv: ["compound", "authority", "job-health-review", "/tmp/a.yaml"],
      requires_review: true,
    });
    const changedFresh = genericActionRevision({
      id: "record_job_health_review",
      label: "Record job-health review",
      argv: ["compound", "authority", "job-health-review", "/tmp/b.yaml"],
      requires_review: true,
    });
    push(
      "generic-confirmation-binds-rendered-action-revision",
      rendered === sameFresh &&
        rendered !== changedFresh &&
        genericActionRevisionMatches(sameFresh, rendered) &&
        !genericActionRevisionMatches(changedFresh, rendered) &&
        !genericActionRevisionMatches(sameFresh, ""),
      `rendered=${rendered} same=${sameFresh} changed=${changedFresh}`,
    );
  }

  // 17. A one-click Attention action resolves by its sole item id; only a
  //     genuinely batch-scoped action uses the group key. Uses "panel-verdicts"
  //     (still generic-only — no bespoke REGISTRY entry): "job-health" is no
  //     longer a valid fixture for this check because THIS change gives it a
  //     bespoke spec (see 17b below and decision-registry.ts's "job-health"
  //     entry) — seam drift from adding that entry, not a break in this check.
  {
    const base: Omit<AttentionGroup, "action"> = {
      key: "panel-verdicts|fixture",
      area: "panel-verdicts",
      band: "needs_you",
      source: "reviews",
      kind: "review",
      count: 1,
      operator_summary: "Record this panel verdict.",
      operator_unlock: "",
      blast_radius: "local_reversible",
      reversibility: "reversible",
      leverage: 1,
      oldest_created_at: "",
      terminal_reason: "",
      item_ids: ["pv_fixture"],
      freshness: { generated_at: "", source: "fixture" },
    };
    const action = {
      id: "panel_verdict_record_member",
      label: "Record panel verdict",
      argv: ["compound", "panel", "verdict-record", "--review", "/tmp/pv_fixture.json", "--reviewed-by", "operator-fixture"],
      requires_confirmation: true,
    };
    const oneClick = headlineAct({ ...base, action: { ...action, kind: "one_click" } });
    const batch = headlineAct({ ...base, action: { ...action, kind: "batch" } });
    push(
      "briefing-generic-action-preserves-owner-scope",
      oneClick?.itemId === "pv_fixture" && batch?.itemId === base.key,
      `oneClick=${oneClick?.itemId ?? "null"} batch=${batch?.itemId ?? "null"}`,
    );
  }

  // 17b. job-health becoming a bespoke owner (this change) shadows its former
  //      generic-actions.ts entry from headlineAct's inline card act — the
  //      same precedence check 15 proves for route-promotions. The
  //      generic-actions.ts "job-health" entry is left untouched; it just
  //      stops being reachable here (decision-execute.ts's specFor(area) fork,
  //      "a bespoke spec always wins").
  {
    const headline = headlineAct({
      key: "job-health|fixture",
      area: "job-health",
      band: "needs_you",
      source: "reviews",
      kind: "review",
      count: 1,
      operator_summary: "Record this job review.",
      operator_unlock: "",
      blast_radius: "local_reversible",
      reversibility: "reversible",
      leverage: 1,
      oldest_created_at: "",
      terminal_reason: "",
      item_ids: ["jhp_fixture"],
      freshness: { generated_at: "", source: "fixture" },
      action: {
        id: "record_job_health_review",
        label: "Record job-health review",
        argv: ["compound", "authority", "job-health-review", "/tmp/review.yaml"],
        kind: "one_click",
        requires_confirmation: true,
      },
    });
    push(
      "job-health-bespoke-shadows-generic-headline-action",
      headline === null,
      `headline=${headline?.pathway ?? "null"}`,
    );
  }

  // 5. Generic review resolution is a fixed adapter: blank reason is rejected
  //    before any subprocess can run.
  {
    const resolution = resolveReviewResolutionAction(
      "review_resolution_park",
      "parity_unknown_fixture",
      "   ",
    );
    push(
      "generic-resolution-requires-reason",
      resolution != null && !resolution.ok && resolution.rejection.gate === "rationale-required",
      `resolution=${JSON.stringify(resolution)}`,
    );
  }

  // 6. Reason text stays one argv element; no shell or client-supplied argv is
  //    involved in the adapter.
  {
    const reason = "hold: day-2 evidence decides; '; $(echo text) stays text";
    const ownerRevision = `review-resolution-v2:sha256:${"a".repeat(64)}`;
    const item = {
      id: "skill_surfacing_unknown_fixture",
      area: "skill-surfacing",
      status: "unknown",
      review_required: true,
      review_resolution_revision: ownerRevision,
    } as Json;
    const resolution = resolveReviewResolutionAction(
      "review_resolution_dismiss",
      str(item.id),
      reason,
      {
        area: "skill-surfacing",
        expectedRevision: reviewResolutionRevision("skill-surfacing", item),
        read: { status: "live", value: { items: [item] }, error: null },
      },
    );
    const argv = resolution?.ok ? resolution.action.argv : [];
    const reasonIndex = argv.indexOf("--reason");
    push(
      "reason-stays-one-argv-element",
      resolution?.ok === true &&
        argv.join("\u0000") ===
          [
            "reviews", "resolve",
            "--item", "skill_surfacing_unknown_fixture",
            "--area", "skill-surfacing",
            "--expected-revision", ownerRevision,
            "--decision", "dismissed",
            "--reason", reason,
            "--json",
          ].join("\u0000") &&
        reasonIndex >= 0 &&
        argv[reasonIndex + 1] === reason,
      `resolution=${JSON.stringify(resolution)}`,
    );
  }

  // The combined Park/Dismiss control selects one fixed adapter action while
  // preserving the single shared reason and explicit confirmation marker.
  {
    const sentinelRevision = `review-resolution-v2:sha256:${"c".repeat(64)}`;
    const act: ClientDecideAct = {
      mode: "generic",
      area: "parity",
      itemId: "parity_unknown_fixture",
      actionId: "review_resolution_park",
      expectedRevision: sentinelRevision,
      label: "Review disposition",
      argv: null,
      requiresConfirmation: true,
      copyCommand: "compound reviews --area parity",
      effectKind: "record",
      reasonRequired: true,
      defaultOption: "review_resolution_park",
      decisionOptions: [
        { value: "review_resolution_park", label: "Park", tone: "orange", description: "Park this review." },
        { value: "review_resolution_dismiss", label: "Dismiss", tone: "gray", description: "Dismiss this review." },
      ],
    };
    const request = buildDecideActionRequest(
      act,
      "review_resolution_dismiss",
      { reason: "duplicate evidence" },
      "confirm",
    );
    push(
      "combined-resolution-selects-one-action",
      request.actionId === "review_resolution_dismiss" &&
        request.expectedRevision === sentinelRevision &&
        request.reason === "duplicate evidence" &&
        request.confirmed === true,
      `request=${JSON.stringify(request)}`,
    );
  }

  // 7. The adapter is deliberately narrow. Every other action id must return
  //    null so executeGenericAction continues through the existing projection
  //    resolver byte-for-byte.
  {
    const resolution = resolveReviewResolutionAction(
      "route_promotion_record",
      "route_promotion_selftest01",
      "record the hold",
    );
    const prototypeNamed = resolveReviewResolutionAction(
      "toString",
      "parity_unknown_fixture",
      "must remain an ordinary unknown action",
    );
    push(
      "non-resolution-action-still-uses-existing-generic-resolver",
      resolution === null && prototypeNamed === null,
      `resolution=${JSON.stringify(resolution)} prototypeNamed=${JSON.stringify(prototypeNamed)}`,
    );
  }

  // A conflict discovered by the owner CLI after the Next preflight keeps its
  // stable 409 contract instead of collapsing into a generic server failure.
  {
    const stale = reviewResolutionCommandFailureResponse({
      message: "command failed",
      stdout: JSON.stringify({
        schema_version: "compound.review_resolution_error.v0.1",
        status: "error",
        error_code: "stale_action_revision",
        error: "review state changed; refresh before deciding",
      }),
      stderr: "",
    });
    const ownerAction = reviewResolutionCommandFailureResponse({
      message: "command failed",
      stdout: JSON.stringify({
        schema_version: "compound.review_resolution_error.v0.1",
        status: "error",
        error_code: "owner_action_present",
        error: "owner action appeared; refresh",
      }),
      stderr: "",
    });
    const unknown = reviewResolutionCommandFailureResponse({ message: "boom", stdout: "", stderr: "boom" });
    const stalePayload = await stale.json() as { error_code?: string };
    const ownerPayload = await ownerAction.json() as { error_code?: string };
    push(
      "review-resolution-cli-conflicts-map-to-http-409",
      stale.status === 409 &&
        ownerAction.status === 409 &&
        unknown.status === 500 &&
        stalePayload.error_code === "stale_action_revision" &&
        ownerPayload.error_code === "owner_action_present",
      `stale=${stale.status}/${stalePayload.error_code ?? ""} owner=${ownerAction.status}/${ownerPayload.error_code ?? ""} unknown=${unknown.status}`,
    );
  }

  // A forged resolution request must not bypass a freshly projected owner
  // action. The adapter also refuses a copy-only card whose rendered
  // resolution revision no longer matches owner truth.
  {
    const itemId = "dcp_decision_review_selftest";
    const actionable = resolveReviewResolutionAction(
      "review_resolution_dismiss",
      itemId,
      "hide the actionable card",
      {
        area: "decision-candidates",
        expectedRevision: "",
        read: {
          status: "live",
          value: {
            items: [{
              id: itemId,
              area: "decision-candidates",
              candidate_id: "cand_decision_review_selftest",
              actions: [{
                id: "plan_decision_candidate_review",
                argv: ["compound", "panel", "plan", "--target-kind", "decision_candidate"],
              }],
            }],
          },
          error: null,
        },
      },
    );
    push(
      "generic-resolution-refuses-fresh-owner-action",
      actionable != null && !actionable.ok && actionable.rejection.gate === "owner-action-present",
      `resolution=${JSON.stringify(actionable)}`,
    );

    const stale = resolveReviewResolutionAction(
      "review_resolution_park",
      "parity_unknown_fixture",
      "park the old evidence",
      {
        area: "parity",
        expectedRevision: "review-resolution-v1:stale",
        read: {
          status: "live",
          value: {
            items: [{
              id: "parity_unknown_fixture",
              area: "parity",
              status: "unknown",
              review_required: true,
            }],
          },
          error: null,
        },
      },
    );
    push(
      "generic-resolution-binds-rendered-owner-revision",
      stale != null && !stale.ok && stale.rejection.gate === "stale-action-revision",
      `resolution=${JSON.stringify(stale)}`,
    );

    const lastGoodItem = {
      id: "parity_unknown_fixture",
      area: "parity",
      status: "unknown",
      review_required: true,
    } as Json;
    const staleProjection = resolveReviewResolutionAction(
      "review_resolution_park",
      str(lastGoodItem.id),
      "park the last-good snapshot",
      {
        area: "parity",
        expectedRevision: reviewResolutionRevision("parity", lastGoodItem),
        read: {
          status: "stale",
          value: { items: [lastGoodItem] },
          error: "fresh owner read timed out",
        },
      },
    );
    push(
      "generic-resolution-requires-fresh-owner-projection",
      staleProjection != null && !staleProjection.ok && staleProjection.rejection.gate === "projection-unavailable",
      `resolution=${JSON.stringify(staleProjection)}`,
    );
  }

  // 18. autonomy-grant-mint (operator correction of record). The bespoke owner
  //     rebuilds the EXACT `decisions record` argv from the operator's three
  //     thresholds, mirrors record.go's threshold rules (reject out-of-range),
  //     renders the verbatim plain-language copy with the dynamic count, and —
  //     because `decisions record` has no write-free --preview — must NOT set
  //     supportsPreview (a "preview" would mint the grant for real).
  {
    const grant = specFor("autonomy-grant-mint");
    const item = {
      id: "agm_job_health",
      area: "autonomy-grant-mint",
      decision_card: { meta: { auto_admit_class: "job_health", waiting_count: "3" } },
    } as Json;
    const argv = grant?.buildArgv?.({
      itemId: "agm_job_health",
      item,
      option: "grant",
      fields: { min_terminal_samples: "5", min_window_days: "7", max_failures: "0" },
    });
    const wantArgv = [
      "decisions", "record",
      "--kind", "grant",
      "--scope", "autonomy-evidence-threshold",
      "--auto-admit-class", "job_health",
      "--min-terminal-samples", "5",
      "--min-window-days", "7",
      "--max-failures", "0",
      "--reviewed-by", "operator-console",
      "--title", "Let proven job-health work enter the queue on its own",
      "--json",
    ];
    const reject = (fields: Record<string, string>) =>
      grant?.buildArgv?.({ itemId: "agm_job_health", item, option: "grant", fields });
    const pl = grant?.plainLanguage(item);
    push(
      "grant-mint-buildargv-exact-and-rules-mirror-record-go",
      grant?.requiresConfirmation === true &&
        grant?.supportsPreview !== true &&
        grant?.idPattern.test("agm_job_health") === true &&
        grant?.idPattern.test("agm_") === false &&
        argv?.join("\u0000") === wantArgv.join("\u0000") &&
        reject({ min_terminal_samples: "5", min_window_days: "7", max_failures: "5" }) == null &&
        reject({ min_terminal_samples: "0", min_window_days: "7", max_failures: "0" }) == null &&
        reject({ min_terminal_samples: "abc", min_window_days: "7", max_failures: "0" }) == null &&
        pl?.headline === "Let proven job-health work enter the queue on its own?" &&
        pl?.body.startsWith("Anything external, destructive, irreversible, financial, or security-related") === true &&
        pl?.consequences?.hold === "3 job-health items are waiting; each currently needs your click",
      `argv=${argv?.join(" ") ?? "null"} preview=${String(grant?.supportsPreview)} hold=${pl?.consequences?.hold ?? "none"}`,
    );
  }

  // 19. job-health (operator-blocking fix, mirrors check 18's shape). The
  //     bespoke owner reads the draft path from the item's extra_paths,
  //     builds the EXACT `authority job-health-review <path> --decision
  //     <enum> --rationale <text> --json` argv, rejects an empty rationale
  //     and an out-of-enum decision client-side (the Go validator remains the
  //     real gate), and — because `job-health-review` has no write-free
  //     --preview — must NOT set supportsPreview.
  {
    const jobHealth = specFor("job-health");
    const item = {
      id: "jhp_fixture01",
      area: "job-health",
      extra_paths: ["/tmp/reviews/job-health/jhp_fixture01.reviewed.json", "/tmp/reviews/job-health/jhp_fixture01.review.yaml"],
      operator_summary: `The scheduled job "svc-alpha backup" is failing — last ran 2026-07-01. Repair it or park it?`,
      context_label: "svc-alpha backup",
      next_action: "review failing scheduled job dependency and decide owner-approved repair or parking",
    } as Json;
    const argv = jobHealth?.buildArgv?.({
      itemId: "jhp_fixture01",
      item,
      option: "parked",
      fields: { rationale: "owner reviewed; keep parked until repair owner acts" },
    });
    const wantArgv = [
      "authority", "job-health-review", "/tmp/reviews/job-health/jhp_fixture01.review.yaml",
      "--decision", "parked",
      "--rationale", "owner reviewed; keep parked until repair owner acts",
      "--json",
    ];
    const options = jobHealth?.optionsFor?.(item) ?? [];
    const pl = jobHealth?.plainLanguage(item);
    push(
      "job-health-buildargv-exact-and-rejects-empty-rationale-and-bogus-decision",
      jobHealth?.requiresConfirmation === true &&
        jobHealth?.supportsPreview !== true &&
        jobHealth?.idPattern.test("jhp_fixture01") === true &&
        jobHealth?.idPattern.test("jhp_") === false &&
        argv?.join(" ") === wantArgv.join(" ") &&
        options.map((option) => option.value).sort().join(",") ===
          ["disabled", "no_action", "owner_action_done", "parked", "repair_required"].join(",") &&
        jobHealth?.buildArgv?.({ itemId: "jhp_fixture01", item, option: "parked", fields: { rationale: "  " } }) == null &&
        jobHealth?.buildArgv?.({ itemId: "jhp_fixture01", item, option: "bogus", fields: { rationale: "x" } }) == null &&
        pl?.headline === item.operator_summary &&
        pl?.consequences?.hold === "nothing changes; it stays in this queue and the job keeps failing silently",
      `argv=${argv?.join(" ") ?? "null"} preview=${String(jobHealth?.supportsPreview)} options=${options.map((option) => option.value).join(",")}`,
    );
  }

  // 20. standing-law-candidate (operator-legibility fix). Parked accepted
  //     decisions become plain-language cards whose FOUR buttons never spell a
  //     raw verdict — the verb (promote/park/reject/no_promote) lives only in
  //     the disclosure argv. The bespoke owner builds the EXACT bare
  //     `memory adjudicate <id> --verdict <verdict>` pair (no --basis: the
  //     candidate already carries promotion_basis, so a bare promote succeeds),
  //     rejects a verdict outside the four, and keeps every id out of the
  //     headline (that is THE CLAIM — the decision's object text).
  {
    const law = specFor("standing-law-candidate");
    const item = {
      id: "cand_lawfixture01",
      state: "parked",
      claim_type: "decision",
      entity: "decision:brain-fixture01",
      predicate: "decided",
      object: "Decision: adopt the envelope spine (2026-06-01)",
      promotion_basis: "decision_record",
      created_at: "2026-06-30T14:20:51Z",
      qualifiers: { source: "brain", decision_id: "brain-fixture01", kind: "doctrine", lifecycle_status: "accepted" },
    } as Json;
    const options = law?.optionsFor?.(item) ?? [];
    const values = options.map((o) => o.value).sort().join(",");
    const labels = options.map((o) => o.label).join("|");
    const promoteArgv = law?.buildArgv?.({ itemId: "cand_lawfixture01", item, option: "promote", fields: {} });
    const wantPromote = ["memory", "adjudicate", "cand_lawfixture01", "--verdict", "promote"];
    const noPromoteArgv = law?.buildArgv?.({ itemId: "cand_lawfixture01", item, option: "no_promote", fields: {} });
    const bogus = law?.buildArgv?.({ itemId: "cand_lawfixture01", item, option: "sudo_make_law", fields: {} });
    const pl = law?.plainLanguage(item);
    // A verdict verb must never appear as a button label (dual-encode contract).
    const RAW_VERDICTS = new Set(["promote", "park", "reject", "no_promote"]);
    const labelLeaksVerb = options.some((o) =>
      RAW_VERDICTS.has(o.value) && new RegExp(`\\b${o.value.replace("_", "[ _]")}\\b`, "i").test(o.label),
    );
    push(
      "standing-law-candidate-exact-bare-argv-dual-encoded-labels-rejects-bogus-verdict",
      Boolean(law) &&
        law?.source === "orient" &&
        law?.requiresResolvedItem !== true &&
        law?.idPattern.test("cand_lawfixture01") === true &&
        law?.idPattern.test("cand_") === false &&
        values === ["no_promote", "park", "promote", "reject"].join(",") &&
        labels === ["Make it law", "Keep aside", "Discard", "Decline — stop asking"].join("|") &&
        labelLeaksVerb === false &&
        promoteArgv?.join(" ") === wantPromote.join(" ") &&
        noPromoteArgv?.join(" ") === ["memory", "adjudicate", "cand_lawfixture01", "--verdict", "no_promote"].join(" ") &&
        bogus == null &&
        pl?.headline === "Decision: adopt the envelope spine (2026-06-01)" &&
        /adjudicate cand_lawfixture01 --verdict/.test(`compound ${(law?.previewArgv?.(item) ?? []).join(" ")}`),
      `values=${values} labels=${labels} promote=${promoteArgv?.join(" ") ?? "null"} bogus=${bogus ?? "null"} leak=${labelLeaksVerb}`,
    );
  }

  // 21. Queue filter (memory page). The 18-shaped fixture (decision-typed +
  //     entity:decision: prefix branch) enters the durable queue; parked
  //     knowledge/x_likes rows and proposed decisions never do — proving the
  //     clean-seam empty state cannot lie while a law candidate waits, and the
  //     28k-row knowledge corpus cannot flood the operator's queue.
  {
    const lawTyped: Json = { id: "cand_l1", state: "parked", claim_type: "decision", entity: "decision:brain-a", object: "Decision: A" };
    const lawEntityPrefix: Json = { id: "cand_l2", state: "parked", claim_type: "doctrine", entity: "decision:brain-b", object: "Decision: B" };
    const xLikeParked: Json = { id: "cand_x1", state: "parked", claim_type: "knowledge", entity: "knowledge:x_like_1", qualifiers: { source: "knowledge_import" }, object: "a liked tweet" };
    const proposedDecision: Json = { id: "cand_p1", state: "proposed", claim_type: "decision", entity: "decision:brain-c", object: "Decision: C" };
    const fixture: Json[] = [
      ...Array.from({ length: 16 }, (_, i) => ({ id: `cand_law_${i}`, state: "parked", claim_type: "decision", entity: `decision:brain-${i}`, object: `Decision ${i}` } as Json)),
      lawTyped,
      lawEntityPrefix,
      ...Array.from({ length: 500 }, (_, i) => ({ id: `cand_xl_${i}`, state: "parked", claim_type: "knowledge", entity: `knowledge:x_like_${i}`, qualifiers: { source: "knowledge_import" } } as Json)),
      proposedDecision,
    ];
    const queue = fixture.filter(isStandingLawCandidate);
    push(
      "standing-law-queue-admits-18-law-shaped-and-excludes-knowledge-and-proposed",
      isStandingLawCandidate(lawTyped) === true &&
        isStandingLawCandidate(lawEntityPrefix) === true &&
        isStandingLawCandidate(xLikeParked) === false &&
        isStandingLawCandidate(proposedDecision) === false &&
        queue.length === 18 &&
        queue.every((c) => str(c.entity).startsWith("decision:") || str(c.claim_type) === "decision"),
      `typed=${isStandingLawCandidate(lawTyped)} prefix=${isStandingLawCandidate(lawEntityPrefix)} xlike=${isStandingLawCandidate(xLikeParked)} proposed=${isStandingLawCandidate(proposedDecision)} queue=${queue.length}/${fixture.length}`,
    );
  }

  const failed = checks.filter((c) => !c.ok);
  return {
    ok: failed.length === 0,
    details: checks.map((c) => `${c.ok ? "ok" : "FAIL"} ${c.name} — ${c.detail}`),
  };
}
