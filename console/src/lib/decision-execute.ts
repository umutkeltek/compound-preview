import {
  argvFromActions,
  argvMatchesAllowlist,
  clamp01,
  type DecisionAreaSpec,
  specFor,
} from "@/lib/decision-registry";
import {
  arr,
  compoundFailure,
  invalidateRunJsonCache,
  type Json,
  reviewItems,
  runCompound,
  runJson,
  str,
} from "@/lib/compound";
import {
  type GenericActionGate,
  type GenericActionRejection,
  type GenericActionResolution,
  type GenericResolutionRead,
  type ResolvedGenericAction,
  legacyAuthorityCutoverGuardFromRead,
  readReviewResolutionProjection,
  reviewResolutionGuardFromRead,
  resolveGenericAction,
} from "@/lib/generic-actions";
import { selectedWorkLease } from "@/lib/work-selected-continue";

export type ActionAuthorityRequest = {
  expectedRevision: string;
  actionIntentId: string;
  expectedAuthorityRevision: string;
};

export type ExactActionOwnerConstraint =
  | { ok: true; requiresConfirmation: boolean }
  | { ok: false; status: 400 | 403; message: string };

type ResolvedAuthorityIdentity = Pick<
  ResolvedGenericAction,
  "revision" | "actionIntentId" | "authorityRevision"
>;

const ACTION_INTENT_ID = /^act_\S+$/;
const ACTION_AUTHORITY_REVISION = /^policy-authority-v1:sha256:[0-9a-f]{64}$/;
const LEGACY_AUTHORITY_CUTOVER_AREAS: ReadonlySet<string> = new Set(["source-control-cleanup"]);

/**
 * Validate the identity carried by an ObjectFace act, then (when a freshly
 * resolved owner action is supplied) compare all three optimistic tokens.
 * The AuthorityFace revision remains opaque; this seam only carries/compares.
 */
export function actionAuthorityRequestError(
  current: ResolvedAuthorityIdentity | null,
  request: ActionAuthorityRequest,
): Response | null {
  if (
    !request.expectedRevision.startsWith("generic-action-v2:") ||
    !ACTION_INTENT_ID.test(request.actionIntentId) ||
    !ACTION_AUTHORITY_REVISION.test(request.expectedAuthorityRevision)
  ) {
    return Response.json(
      {
        ok: false,
        error_code: "malformed_authority_request",
        stderr: "action revision, ActionIntent id, and AuthorityFace revision are required",
      },
      { status: 400 },
    );
  }
  if (!current) return null;
  if (
    current.revision !== request.expectedRevision ||
    current.actionIntentId !== request.actionIntentId ||
    current.authorityRevision !== request.expectedAuthorityRevision
  ) {
    return Response.json(
      {
        ok: false,
        error_code: "authority_drift",
        stderr: "action authority changed since it was rendered; refresh and review current policy",
      },
      { status: 409 },
    );
  }
  return null;
}

/**
 * Preserve legacy bespoke callers until their concrete item has cut over,
 * while making every request that claims stamped authority carry the complete
 * optimistic identity tuple. A non-v2 legacy revision is not such a claim.
 */
export function actionAuthorityAdmissionError(
  actionId: string,
  preview: boolean,
  request: ActionAuthorityRequest,
): Response | null {
  const claimsStampedAuthority = Boolean(
    actionId ||
    request.actionIntentId ||
    request.expectedAuthorityRevision ||
    request.expectedRevision.startsWith("generic-action-v2:"),
  );
  if (!claimsStampedAuthority) return null;
  if (!actionId) {
    return Response.json(
      {
        ok: false,
        error_code: "malformed_authority_request",
        stderr: "action id, action revision, ActionIntent id, and AuthorityFace revision are required",
      },
      { status: 400 },
    );
  }
  const identityError = actionAuthorityRequestError(null, request);
  if (identityError) return identityError;
  if (preview) {
    return Response.json(
      { ok: false, error_code: "preview_unsupported", stderr: "stamped owner actions do not support bespoke preview" },
      { status: 400 },
    );
  }
  return null;
}

/**
 * An exact owner-projected action keeps its stamped argv semantics, but a
 * legacy area owner remains a narrowing contract. Apply its target shape,
 * argv prefix, and confirmation floor in addition to the generic/global
 * allowlists; never rebuild argv from editable legacy fields.
 */
export function exactActionOwnerConstraint(
  spec: DecisionAreaSpec | null,
  itemId: string,
  action: Pick<ResolvedGenericAction, "argv" | "requiresReview">,
): ExactActionOwnerConstraint {
  if (!spec) return { ok: true, requiresConfirmation: action.requiresReview };
  if (!spec.idPattern.test(itemId)) {
    return { ok: false, status: 400, message: `invalid id for ${spec.area}` };
  }
  if (!argvMatchesAllowlist(action.argv, spec.argvAllowlist)) {
    return { ok: false, status: 403, message: `command not allowed for ${spec.area}` };
  }
  return {
    ok: true,
    requiresConfirmation: action.requiresReview || Boolean(spec.requiresConfirmation),
  };
}

export type DecisionExecutionRefs = {
  action_refs: string[];
  action_intent_ids: string[];
  policy_decision_ids: string[];
  receipt_ids: string[];
  transition_ids: string[];
  proof_refs: string[];
  work_ids: string[];
  review_ids: string[];
  candidate_ids: string[];
  decision_ids: string[];
};

export type DecisionExecutionEffect = {
  kind: string;
  status: string;
  changed: boolean;
  summary: string;
  before: string;
  after: string;
  workId: string;
  workState: string;
  decisionId: string;
};

const REF_KEYS: Record<string, keyof DecisionExecutionRefs> = {
  action_ref: "action_refs",
  action_refs: "action_refs",
  action_intent: "action_intent_ids",
  action_intent_id: "action_intent_ids",
  action_intent_ids: "action_intent_ids",
  policy_decision: "policy_decision_ids",
  policy_decision_id: "policy_decision_ids",
  policy_decision_ids: "policy_decision_ids",
  receipt: "receipt_ids",
  receipt_id: "receipt_ids",
  receipt_ids: "receipt_ids",
  transition: "transition_ids",
  transition_id: "transition_ids",
  transition_ids: "transition_ids",
  proof_ref: "proof_refs",
  proof_refs: "proof_refs",
  work: "work_ids",
  work_id: "work_ids",
  work_ids: "work_ids",
  review: "review_ids",
  review_id: "review_ids",
  review_ids: "review_ids",
  candidate: "candidate_ids",
  candidate_id: "candidate_ids",
  candidate_ids: "candidate_ids",
  decision_id: "decision_ids",
  decision_ids: "decision_ids",
};

const ID_PATTERNS: Array<[keyof DecisionExecutionRefs, RegExp]> = [
  ["action_intent_ids", /\bact_[a-z0-9][a-z0-9_]{5,}\b/g],
  ["policy_decision_ids", /\b(?:pol|pd)_[a-z0-9][a-z0-9_]{5,}\b/g],
  ["receipt_ids", /\b(?:receipt|rec)_[a-z0-9][a-z0-9_]{5,}\b/g],
  ["transition_ids", /\b(?:transition|tr)_[a-z0-9][a-z0-9_]{5,}\b/g],
  ["work_ids", /\bwork_[a-z0-9][a-z0-9_]{5,}\b/g],
  ["review_ids", /\brev_[a-z0-9][a-z0-9_]{5,}\b/g],
  ["candidate_ids", /\bcand_[a-z0-9][a-z0-9_]{5,}\b/g],
  ["decision_ids", /\bcld_[a-z0-9][a-z0-9_]{5,}\b/g],
];

function emptyRefs(): DecisionExecutionRefs {
  return {
    action_refs: [],
    action_intent_ids: [],
    policy_decision_ids: [],
    receipt_ids: [],
    transition_ids: [],
    proof_refs: [],
    work_ids: [],
    review_ids: [],
    candidate_ids: [],
    decision_ids: [],
  };
}

function addRef(refs: DecisionExecutionRefs, key: keyof DecisionExecutionRefs, value: string) {
  const clean = value.trim().replace(/^["']|["']$/g, "").replace(/[,.]$/g, "");
  if (!clean || refs[key].includes(clean)) return;
  refs[key].push(clean);
}

function addDelimitedRefs(refs: DecisionExecutionRefs, key: keyof DecisionExecutionRefs, raw: string) {
  for (const part of raw.split(/[,\s]+/)) addRef(refs, key, part);
}

function firstJsonObject(text: string): Json | null {
  const start = text.indexOf("{");
  if (start < 0) return null;
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let i = start; i < text.length; i++) {
    const ch = text[i];
    if (inString) {
      if (escaped) escaped = false;
      else if (ch === "\\") escaped = true;
      else if (ch === '"') inString = false;
      continue;
    }
    if (ch === '"') inString = true;
    else if (ch === "{") depth++;
    else if (ch === "}" && --depth === 0) {
      try {
        return JSON.parse(text.slice(start, i + 1)) as Json;
      } catch {
        return null;
      }
    }
  }
  return null;
}

function collectStructuredRefs(refs: DecisionExecutionRefs, value: unknown, path: string[] = []) {
  if (Array.isArray(value)) {
    for (const item of value) collectStructuredRefs(refs, item, path);
    return;
  }
  if (!value || typeof value !== "object") return;
  for (const [rawKey, nested] of Object.entries(value as Json)) {
    const key = rawKey.toLowerCase();
    const refKey = REF_KEYS[key] ?? (key === "id" && path.includes("disposition") ? "decision_ids" : undefined);
    if (refKey) {
      if (Array.isArray(nested)) {
        for (const item of nested) addRef(refs, refKey, String(item));
      } else if (typeof nested === "string") {
        addRef(refs, refKey, nested);
      }
    }
    collectStructuredRefs(refs, nested, [...path, key]);
  }
}

export function extractDecisionRefs(...texts: string[]): DecisionExecutionRefs {
  const refs = emptyRefs();
  const fallbackTexts: string[] = [];

  for (const source of texts) {
    const parsed = firstJsonObject(source);
    if (parsed) collectStructuredRefs(refs, parsed);
    else fallbackTexts.push(source);
  }

  // Structured JSON is authoritative. Avoid regex-reading schema names such
  // as `compound.work_create_plan.v0.2` as if they were emitted work IDs.
  const text = fallbackTexts.join("\n");

  for (const line of text.split(/\r?\n/)) {
    const trimmed = line.trim().replace(/^-\s*/, "");
    const match = trimmed.match(/^([a-zA-Z_]+):\s*(.+)$/);
    if (!match) continue;
    const key = REF_KEYS[match[1].toLowerCase()];
    if (!key) continue;
    addDelimitedRefs(refs, key, match[2]);
  }

  for (const [key, pattern] of ID_PATTERNS) {
    const matches = text.match(pattern) ?? [];
    for (const match of matches) addRef(refs, key, match);
  }
  return refs;
}

export function renderedRevisionMatches(current: string, rendered: string, required: boolean): boolean {
  if (!required) return true;
  return Boolean(current.trim() && rendered.trim() && current.trim() === rendered.trim());
}

export function decisionStaleRevisionResponse(): Response {
  return Response.json(
    {
      ok: false,
      error_code: "stale_revision",
      stderr: "decision source changed since it was rendered; refresh and review the current proposal",
    },
    { status: 409 },
  );
}

export function genericActionAllowedForOwner(hasBespokeOwner: boolean, actionId: string): boolean {
  return !hasBespokeOwner && actionId.trim() !== "";
}

export function genericActionRevisionMatches(current: string, rendered: string): boolean {
  return Boolean(current && rendered && current === rendered);
}

export function missingBespokeConfirmation(required: boolean, preview: boolean, confirmed: boolean): boolean {
  return required && !preview && !confirmed;
}

export function decisionFailureStatus(area: string, ...outputs: string[]): number {
  if (area !== "capability-lifecycle-proposals") return 500;
  for (const output of outputs) {
    const payload = firstJsonObject(output);
    const code = str(payload?.error_code);
    if (code === "stale_revision" || code === "disposition_conflict") return 409;
  }
  return 500;
}

export function decisionCommandFailureResponse(
  area: string,
  failure: { message: string; stdout: string; stderr: string },
  preview: boolean,
): Response {
  const payload = firstJsonObject(failure.stdout) ?? firstJsonObject(failure.stderr);
  const errorCode = str(payload?.error_code);
  const errorMessage = str(payload?.error) || failure.stderr || failure.message;
  return Response.json({
    ok: false,
    stdout: failure.stdout,
    stderr: errorMessage,
    error_code: errorCode || undefined,
    refs: extractDecisionRefs(failure.stdout, failure.stderr || failure.message),
    preview,
    effect: decisionExecutionEffect(area, failure.stdout, preview),
  }, { status: decisionFailureStatus(area, failure.stdout, failure.stderr) });
}

export function reviewResolutionCommandFailureResponse(
  failure: { message: string; stdout: string; stderr: string },
): Response {
  const payload = firstJsonObject(failure.stdout) ?? firstJsonObject(failure.stderr);
  const errorCode = str(payload?.error_code);
  const conflict = errorCode === "stale_action_revision" || errorCode === "owner_action_present";
  return Response.json({
    ok: false,
    stdout: failure.stdout,
    stderr: str(payload?.error) || failure.stderr || failure.message,
    error_code: errorCode || undefined,
    refs: extractDecisionRefs(failure.stdout, failure.stderr || failure.message),
  }, { status: conflict ? 409 : 500 });
}

function decisionExecutionEffect(area: string, stdout: string, preview: boolean): DecisionExecutionEffect | null {
  const payload = firstJsonObject(stdout);
  if (!payload) return null;
  if (area === "autonomy-grant-mint") return autonomyGrantMintEffect(payload, preview);
  if (area === "job-health") return jobHealthReviewEffect(payload, preview);
  if (area !== "capability-lifecycle-proposals") return null;
  const delta = payload.state_delta && typeof payload.state_delta === "object" ? (payload.state_delta as Json) : {};
  const disposition = payload.disposition && typeof payload.disposition === "object" ? (payload.disposition as Json) : {};
  const workPlan = payload.work_plan && typeof payload.work_plan === "object" ? (payload.work_plan as Json) : {};
  const work = workPlan.work && typeof workPlan.work === "object" ? (workPlan.work as Json) : {};
  return {
    kind: str(disposition.decision) || str((payload.plan as Json | undefined)?.input?.decision) || "lifecycle_disposition",
    status: str(payload.status) || (preview ? "preview" : "recorded"),
    changed: Boolean(delta.changed),
    summary: str(delta.summary),
    before: str(delta.before),
    after: str(delta.after),
    workId: str(disposition.work_id) || str(workPlan.work_id) || str(work.id),
    workState: str(disposition.work_state) || str(workPlan.work_state) || str(work.state),
    decisionId: str(disposition.id),
  };
}

/**
 * Renders `decisions record --json` (RecordReport: internal/decisions/record.go)
 * as a plain consequence for the grant-mint owner, so DecisionOutcome shows
 * "job-health work can now start on its own…" + the recorded decision id
 * instead of raw JSON. It never claims task success — recording a grant is a
 * permission, and every future auto-admit still writes its own receipt.
 */
function autonomyGrantMintEffect(payload: Json, preview: boolean): DecisionExecutionEffect {
  const imp = payload.import && typeof payload.import === "object" ? (payload.import as Json) : {};
  const lifecycle = imp.lifecycle && typeof imp.lifecycle === "object" ? (imp.lifecycle as Json) : {};
  const cls = str(lifecycle.auto_admit_class) || "job_health";
  const plain = cls.replace(/_/g, "-");
  const label = plain.charAt(0).toUpperCase() + plain.slice(1);
  const status = str(lifecycle.status) || (preview ? "preview" : "recorded");
  return {
    kind: str(lifecycle.kind) || "grant",
    status,
    changed: !preview && Boolean(str(lifecycle.id)),
    summary: `${label} work can now start on its own once it clears the bar you set — every start still writes a receipt, and one switch halts it.`,
    before: "",
    after: "",
    workId: "",
    workState: "",
    decisionId: str(lifecycle.id),
  };
}

/**
 * Renders `authority job-health-review --json` (JobHealthReviewReport:
 * internal/authority/job_health_review.go) as a plain consequence for the
 * job-health owner, so DecisionOutcome shows the recorded decision + review
 * id in plain language instead of raw JSON. `job-health-review` has no
 * --preview (it fills the draft and writes the record for real), so preview
 * is never true for this owner in practice — the parameter is kept only for
 * signature symmetry with the other effect renderers.
 */
function jobHealthReviewEffect(payload: Json, preview: boolean): DecisionExecutionEffect {
  const decision = str(payload.decision);
  const plain = decision.replace(/_/g, "-");
  const jobLabel = str(payload.job_name) || str(payload.job_id) || "this job";
  const status = str(payload.status) || (preview ? "preview" : "recorded");
  const nextAction = str(payload.next_action);
  return {
    kind: decision || "job_health_review",
    status,
    changed: !preview && status === "recorded",
    summary:
      `${jobLabel}'s job-health review is recorded as ${plain || "reviewed"}.` +
      (nextAction ? ` Next: ${nextAction}.` : ""),
    before: "",
    after: "",
    workId: "",
    workState: "",
    decisionId: str(payload.id),
  };
}

async function resolveItem(spec: DecisionAreaSpec, itemId: string): Promise<Json | null> {
  if (spec.area === "work-selected-continue") {
    const [work, leases] = await Promise.all([
      runJson(["work", "show", itemId, "--json"]),
      runJson(["work", "leases", "--json"]),
    ]);
    if (!work || str(work.id) !== itemId || !leases) return null;
    return { ...work, selected_lease: selectedWorkLease(leases, itemId) };
  }
  if (spec.source === "reviews") {
    const data = await runJson(["reviews", "--json", "--area", spec.area]);
    return reviewItems(data).find((it) => str(it.id) === itemId) ?? null;
  }
  if (spec.source === "workbench") {
    const data = await runJson(["workbench", "--json"]);
    const open = arr(data?.open);
    const admissionPreviews = arr(data?.admission_previews);
    return (
      open.find((it) => str(it.work_id) === itemId || str(it.id) === itemId) ??
      admissionPreviews.find((it) => str(it.work_id) === itemId || str(it.id) === itemId) ??
      null
    ) as Json | null;
  }
  if (spec.area === "memory-adjudicate") {
    const workbench = await runJson(["workbench", "--json"]);
    for (const work of arr(workbench?.open)) {
      for (const card of arr(work.review_cards)) {
        if (str(card.kind) === "memory_candidate_adjudication" && str(card.candidate_id) === itemId) {
          return { ...card, id: itemId } as Json;
        }
      }
    }
    const data = await runJson(["memory", "candidates", "--json", "--include-evidence"]);
    return (arr(data?.items).find((it) => str(it.id) === itemId) ?? null) as Json | null;
  }
  return null;
}

async function currentDispatchTarget(): Promise<Json | null> {
  const data = await runJson(["workbench", "--json"]);
  const next = (data?.dispatch_next ?? {}) as Json;
  if (!next.can_run) return null;
  return next;
}

export async function executeDecision(body: Record<string, unknown>): Promise<Response> {
  const area = typeof body.area === "string" ? body.area : "";
  const itemId = typeof body.itemId === "string" ? body.itemId.trim() : "";
  const option = typeof body.option === "string" ? body.option : "";
  const actionId = typeof body.actionId === "string" ? body.actionId.trim() : "";
  const confirmed = body.confirmed === true;
  const preview = body.preview === true;
  const rationale = typeof body.rationale === "string" ? body.rationale : "";
  const expectedRevision = typeof body.expectedRevision === "string" ? body.expectedRevision.trim() : "";
  const actionIntentId = typeof body.actionIntentId === "string" ? body.actionIntentId.trim() : "";
  const expectedAuthorityRevision = typeof body.expectedAuthorityRevision === "string"
    ? body.expectedAuthorityRevision.trim()
    : "";
  const reason = typeof body.reason === "string" ? body.reason : rationale;

  const fields: Record<string, string> = {};
  if (body.fields && typeof body.fields === "object") {
    for (const [k, v] of Object.entries(body.fields as Record<string, unknown>)) {
      fields[k] = typeof v === "string" ? v : String(v ?? "");
    }
  }
  if ("confidence" in fields) fields.confidence = String(clamp01(fields.confidence));

  const authorityRequest = { expectedRevision, actionIntentId, expectedAuthorityRevision };
  const admissionError = actionAuthorityAdmissionError(actionId, preview, authorityRequest);
  if (admissionError) return admissionError;

  // Every execution path re-resolves owner truth outside the short read cache.
  // This must happen before the generic branch as well as the bespoke branch.
  invalidateRunJsonCache();
  const spec = specFor(area);
  // Legacy bespoke callers remain live until owner truth proves that THIS
  // concrete item has a complete current stamped action. Once it does, the
  // editable legacy body is stale UI and cannot bypass the stamped semantics.
  if (!preview && !actionId && spec && LEGACY_AUTHORITY_CUTOVER_AREAS.has(area)) {
    const cutover = legacyAuthorityCutoverGuardFromRead(
      await readReviewResolutionProjection(area),
      area,
      itemId,
    );
    if (!cutover.ok) return genericActionRejectionResponse(cutover.rejection);
  }
  // An exact Go-projected action keeps its semantics even when the area also
  // has a legacy editable bespoke form. The server resolves argv from the
  // owner projection and applies both existing allowlists; client fields can
  // never replace the stamped ActionIntent.
  if (!preview && actionId) {
    return executeGenericAction(
      area,
      itemId,
      actionId,
      authorityRequest,
      confirmed,
      reason,
      spec,
    );
  }
  if (!spec) {
    if (preview) {
      return Response.json(
        { ok: false, error_code: "preview_unsupported", stderr: "generic actions do not support write-free preview" },
        { status: 400 },
      );
    }
    // No bespoke REGISTRY spec for this area. If the caller named a backend
    // action, fall through to the generic governed-action pathway
    // (generic-actions.ts) instead of the "unknown decision area" 400 below —
    // this is the ONLY way areas without a bespoke spec become executable.
    // A bespoke spec always wins: this branch never runs when `spec` exists.
    if (genericActionAllowedForOwner(false, actionId)) {
      return executeGenericAction(area, itemId, actionId, authorityRequest, confirmed, reason, null);
    }
    return Response.json({ ok: false, stderr: `unknown decision area: ${area}` }, { status: 400 });
  }

  if (!spec.idPattern.test(itemId)) {
    return Response.json({ ok: false, stderr: `invalid id for ${area}` }, { status: 400 });
  }

  const item = await resolveItem(spec, itemId);
  if (!item && (spec.source !== "orient" || spec.requiresResolvedItem)) {
    return Response.json({ ok: false, stderr: `item ${itemId} not found in ${area}` }, { status: 404 });
  }

  const currentRevision = spec.revisionFor?.(item ?? {}) ?? "";
  if (!renderedRevisionMatches(currentRevision, expectedRevision, Boolean(spec.revisionFor))) {
    return decisionStaleRevisionResponse();
  }

  if (area === "work-dispatch") {
    const next = await currentDispatchTarget();
    const nextWorkID = str(next?.work_id);
    if (!next || nextWorkID !== itemId) {
      return Response.json(
        {
          ok: false,
          stderr: nextWorkID
            ? `continue target changed; refresh the board before continuing ${nextWorkID}`
            : "nothing is currently safe to continue; refresh the board",
        },
        { status: 409 },
      );
    }
    const requestedActor = str(fields.actor) || "codex";
    const nextActor = str(next.actor) || "codex";
    if (requestedActor !== nextActor) {
      return Response.json(
        { ok: false, stderr: `continue target actor changed; refresh the board before continuing as ${nextActor}` },
        { status: 409 },
      );
    }
  }

  const allowedOptions = spec.optionsFor ? spec.optionsFor(item ?? {}) : spec.options;
  if (!allowedOptions.some((o) => o.value === option)) {
    return Response.json(
      { ok: false, stderr: `decision "${option}" is not valid for this item's current state` },
      { status: 400 },
    );
  }

  for (const field of spec.fields) {
    const relevant = !field.showForOptions || field.showForOptions.includes(option);
    if (field.required && relevant && !str(fields[field.name]).trim()) {
      return Response.json({ ok: false, stderr: `${field.label} is required` }, { status: 400 });
    }
  }

  const ctx = { itemId, item: item ?? {}, option, fields, expectedRevision };

  let argv = spec.buildArgv ? spec.buildArgv(ctx) : argvFromActions(item ?? {});
  if (!argv || argv.length === 0) {
    return Response.json({ ok: false, stderr: "could not resolve the command to run" }, { status: 400 });
  }

  if (!argvMatchesAllowlist(argv, spec.argvAllowlist)) {
    return Response.json({ ok: false, stderr: `command not allowed for ${area}` }, { status: 403 });
  }

  if (preview) {
    if (!spec.supportsPreview) {
      return Response.json({ ok: false, stderr: `preview is not available for ${area}` }, { status: 400 });
    }
    argv = [...argv, "--preview"];
  } else if (missingBespokeConfirmation(Boolean(spec.requiresConfirmation), preview, confirmed)) {
    return Response.json({ ok: false, stderr: "confirmation required before this decision runs" }, { status: 428 });
  }

  try {
    const result = await runCompound(argv);
    // The mutation succeeded - every cached `compound ... --json` read is now
    // potentially stale. Clear the memo so the client's router.refresh() (see
    // DecisionForm/ReviewDecisionForm/ContinueButton) re-execs and shows truth.
    invalidateRunJsonCache();
    return Response.json({
      ok: true,
      stdout: result.stdout,
      stderr: result.stderr,
      refs: extractDecisionRefs(result.stdout, result.stderr),
      preview,
      effect: decisionExecutionEffect(area, result.stdout, preview),
    });
  } catch (error) {
    const failure = compoundFailure(error);
    return decisionCommandFailureResponse(area, failure, preview);
  }
}

const GENERIC_GATE_STATUS: Record<GenericActionGate, number> = {
  "not-found": 404,
  "gate-1-shell-form": 403,
  "gate-2-allowlist": 403,
  "gate-3-global-allowlist": 403,
  "owner-action-present": 409,
  "authority-unavailable": 409,
  "authority-not-allowed": 409,
  "stale-action-revision": 409,
  "rationale-required": 422,
  "operator-input-required": 422,
  // A projection that did not answer is a slow door, not a missing item —
  // 503 tells the client "retry", where the old collapse into 404 told the
  // operator their real button pointed at nothing (the cold-cache act 404).
  "projection-unavailable": 503,
};

export function genericActionRejectionResponse(rejection: GenericActionRejection): Response {
  const errorCode = rejection.gate === "stale-action-revision"
    ? "stale_action_revision"
    : rejection.gate === "owner-action-present"
      ? "owner_action_present"
      : rejection.gate === "authority-unavailable" || rejection.gate === "authority-not-allowed"
        ? "authority_drift"
        : rejection.gate === "projection-unavailable"
          ? "projection_unavailable"
          : undefined;
  return Response.json(
    { ok: false, error_code: errorCode, stderr: rejection.message },
    { status: GENERIC_GATE_STATUS[rejection.gate] },
  );
}

const REVIEW_RESOLUTION_DECISIONS = {
  review_resolution_park: { decision: "parked", label: "Park" },
  review_resolution_dismiss: { decision: "dismissed", label: "Dismiss" },
} as const;

function reviewResolutionDecision(actionId: string): (typeof REVIEW_RESOLUTION_DECISIONS)[keyof typeof REVIEW_RESOLUTION_DECISIONS] | null {
  if (!Object.prototype.hasOwnProperty.call(REVIEW_RESOLUTION_DECISIONS, actionId)) return null;
  return REVIEW_RESOLUTION_DECISIONS[actionId as keyof typeof REVIEW_RESOLUTION_DECISIONS];
}

type ReviewResolutionContext = {
  area: string;
  expectedRevision: string;
  read: GenericResolutionRead;
};

// Fixed adapter for cards whose owner exposes no executable action. A named
// resolution id + item id + operator reason is necessary but not sufficient:
// live owner truth must still expose no action and match the rendered revision.
// Only then is canonical CLI argv constructed server-side. Returning null
// preserves the existing generic projection resolver for non-resolution acts.
export function resolveReviewResolutionAction(
  actionId: string,
  itemId: string,
  reason?: string,
  context?: ReviewResolutionContext,
): GenericActionResolution | null {
  const config = reviewResolutionDecision(actionId);
  if (!config) return null;
  const cleanReason = (reason ?? "").trim();
  if (!cleanReason) {
    return {
      ok: false,
      rejection: {
        gate: "rationale-required",
        message: `action "${actionId}" requires a reason; it becomes provenance on the review resolution`,
      },
    };
  }
  if (!context) {
    return {
      ok: false,
      rejection: {
        gate: "projection-unavailable",
        message: "fresh owner truth is required before a review disposition can be recorded",
      },
    };
  }
  const guard = reviewResolutionGuardFromRead(
    context.read,
    context.area,
    itemId,
    context.expectedRevision,
  );
  if (!guard.ok) return guard;
  const action = {
    argv: [
      "reviews",
      "resolve",
      "--item",
      itemId,
      "--area",
      context.area,
      "--expected-revision",
      guard.revision,
      "--decision",
      config.decision,
      "--reason",
      cleanReason,
      "--json",
    ],
    actionId,
    label: config.label,
    requiresReview: true,
    policyNote: "records a reversible review disposition only; underlying authority is unchanged",
    revision: guard.revision,
  } as ResolvedGenericAction;
  return { ok: true, action };
}


/**
 * The generic governed-action pathway (Phase 3a): executes an area's
 * backend-declared action. Every argv comes from resolveGenericAction's fresh
 * server-side re-resolution — this function never trusts client argv. When a
 * legacy REGISTRY spec remains, it narrows the exact action without replacing
 * its semantics. The rendered action revision must match the freshly resolved
 * owner action before confirmation can authorize it.
 */
async function executeGenericAction(
  area: string,
  itemId: string,
  actionId: string,
  authorityRequest: ActionAuthorityRequest,
  confirmed: boolean,
  rationale?: string,
  areaSpec: DecisionAreaSpec | null = null,
): Promise<Response> {
  if (!itemId) {
    return Response.json({ ok: false, stderr: "itemId is required" }, { status: 400 });
  }

  const isReviewResolution = reviewResolutionDecision(actionId) !== null;
  const reviewResolution = isReviewResolution
    ? resolveReviewResolutionAction(
        actionId,
        itemId,
        rationale,
        {
          area,
          expectedRevision: authorityRequest.expectedRevision,
          read: await readReviewResolutionProjection(area),
        },
      )
    : null;
  let action: ResolvedGenericAction;
  if (reviewResolution) {
    if (!reviewResolution.ok) {
      return genericActionRejectionResponse(reviewResolution.rejection);
    }
    action = reviewResolution.action;
  } else {
    const resolution = await resolveGenericAction(area, itemId, actionId, rationale);
    if (!resolution.ok) {
      return genericActionRejectionResponse(resolution.rejection);
    }
    action = resolution.action;
  }
  const authorityConflict = actionAuthorityRequestError(action, authorityRequest);
  if (authorityConflict) return authorityConflict;
  const ownerConstraint = exactActionOwnerConstraint(areaSpec, itemId, action);
  if (!ownerConstraint.ok) {
    return Response.json({ ok: false, stderr: ownerConstraint.message }, { status: ownerConstraint.status });
  }
  if (ownerConstraint.requiresConfirmation && !confirmed) {
    return Response.json(
      { ok: false, stderr: `action "${action.actionId}" requires confirmation before it runs` },
      { status: 428 },
    );
  }

  try {
    const result = await runCompound(action.argv);
    // Same reasoning as the bespoke path above: a mutation just ran, so every
    // cached `compound ... --json` read is potentially stale.
    invalidateRunJsonCache();
    return Response.json({
      ok: true,
      stdout: result.stdout,
      stderr: result.stderr,
      refs: extractDecisionRefs(result.stdout, result.stderr),
    });
  } catch (error) {
    const failure = compoundFailure(error);
    if (isReviewResolution) return reviewResolutionCommandFailureResponse(failure);
    return Response.json({
      ok: false,
      stdout: failure.stdout,
      stderr: failure.stderr || failure.message,
      refs: extractDecisionRefs(failure.stdout, failure.stderr || failure.message),
    }, { status: 500 });
  }
}
