import { arr, type Json, type ProjectionStatus, runProjection, str } from "@/lib/compound";
import { parseActionAuthorityBundle } from "@/lib/action-authority";
import { argvMatchesAllowlist } from "@/lib/decision-registry";
import type { AuthorityFace } from "@/representation";
import { OPERATOR_INPUTS, RATIONALE_REQUIRED_AREAS, type OperatorInputSpec } from "@/lib/operator-inputs";

// Re-exported so server-side importers keep one import site; the specs are
// owned by operator-inputs.ts, which client components may import safely.
export { OPERATOR_INPUTS, RATIONALE_REQUIRED_AREAS };
export type { OperatorInputSpec };

/*
 * The generic governed-action pathway — Phase 3a of the console redesign
 * (Plans/i-am-not-happy-rosy-floyd.md Part 4, "Closing the 91-item action
 * gap"). Extends `POST /api/decision/execute` (decision-execute.ts) so a
 * review item that has NO bespoke `DecisionAreaSpec` in decision-registry.ts
 * can still be executed one-click, by trusting only the backend's own
 * freshly-resolved `actions[]` (per item) / `safe_batch_action` (per group)
 * projection — never client-supplied argv.
 *
 * Request contract (unchanged endpoint, additive fields):
 *   POST /api/decision/execute
 *   { area, itemId, actionId, expectedRevision, confirmed? }
 *   -> looks up `compound reviews --json --area <area>` fresh, finds the item
 *      (by `items[].id`) or group (by `groups[].key`), finds the action (by
 *      `actions[].id` or `safe_batch_action.id`), and — if it clears gates
 *      1-3 below and any required confirmation — runs the resolved argv
 *      through the same governed `runCompound` + cache-invalidate path the
 *      bespoke registry uses.
 *
 * An exact Go-projected `actionId` engages this path before any legacy bespoke
 * form, even when the area still has a registry entry. That preserves the
 * stamped action semantics and prevents editable client fields from replacing
 * the ActionIntent. Without an exact action id, no mutation is admitted.
 *
 * Three gates, in order, each with a distinct rejection reason:
 *   1. `gate-1-shell-form`     — resolved argv[0] must be literally
 *      "compound". Panel per-item actions are deliberately unexecutable
 *      `/bin/sh -lc ...` shell forms (see decision-registry.ts's
 *      "panel-apply" comment) — they must stay copy-only forever, from any
 *      area, even a certified one.
 *   2. `gate-2-allowlist`      — the area must be a key in
 *      GENERIC_ACTION_ALLOWLIST AND the bare argv (argv with the leading
 *      "compound" stripped) must match one of that area's certified
 *      verb-prefixes. An area absent from the map is never executable
 *      generically, full stop — deliberate, never blanket.
 *   3. `gate-3-global-allowlist` — defense in depth: the SAME bare argv must
 *      also match the flat, area-agnostic GENERIC_GLOBAL_ARGV_ALLOWLIST
 *      below (reusing the exported `argvMatchesAllowlist` mechanics
 *      decision-registry.ts already uses for every bespoke spec). Extended
 *      only with the exact verbs the seed areas need.
 *
 * Plus, independent of the three gates: if the resolved action's
 * `requires_review` is true, the request must carry `{confirmed: true}`
 * (the UI's ConfirmDialog sets this after showing label/policy_note/argv) or
 * the caller gets 428.
 */

export type GenericActionGate =
  | "gate-1-shell-form"
  | "gate-2-allowlist"
  | "gate-3-global-allowlist"
  | "owner-action-present"
  | "authority-unavailable"
  | "authority-not-allowed"
  | "stale-action-revision"
  | "rationale-required"
  | "operator-input-required"
  | "projection-unavailable"
  | "not-found";

export type ResolvedGenericAction = {
  /** argv WITHOUT the leading "compound" — ready for runCompound(). */
  argv: string[];
  actionId: string;
  label: string;
  requiresReview: boolean;
  policyNote: string;
  revision: string;
  actionIntentId: string;
  authorityRevision: string;
  authority: AuthorityFace;
};

export type GenericActionRejection = {
  gate: GenericActionGate;
  message: string;
};

export type GenericActionResolution =
  | { ok: true; action: ResolvedGenericAction }
  | { ok: false; rejection: GenericActionRejection };

/**
 * Gate 2 — per-area certification. Seeded 2026-07-09 by reading the LIVE
 * `compound reviews --json --area <area>` output and the Go annotators that
 * build it (verified against source, not just the projection, since a review
 * area can have zero live review_required items at seed time):
 *
 *  - "job-health": internal/authority/job_health_review.go emits
 *    `actions: [{id: "record_job_health_review",
 *      argv: ["compound","authority","job-health-review",<review.yaml>]}]`
 *    on every review_required item. Verified live 2026-07-09 (area had 6
 *    items, 0 review_required at seed time, all recorded/resolved) against
 *    the Go source, which is unconditional for any review_required item.
 *  - "source-control-cleanup": internal/authority/sourcecontrol_plan.go
 *    emits `actions: [{id: "record_source_control_cleanup_review",
 *      argv: ["compound","authority","dirty-review",<review.yaml>]}]` on
 *    every review_required item. Verified live 2026-07-09 (area had 3 items,
 *    0 review_required at seed time) against the Go source.
 *  - "observation-candidates": internal/observation/review.go emits
 *    `actions: [{id: "plan_observation_lens_review",
 *      argv: ["compound","panel","plan","--target-kind",
 *      "capability_observation","--target",<id>,"--domain",<d>,"--task",<t>,
 *      "--json"]}]` on every review_required item (a panel *plan*, not an
 *      apply — read-only planning, safe to certify). Verified live
 *      2026-07-09 (area had 2 items, 0 review_required at seed time) against
 *      the Go source.
 *  - "decision-candidates": internal/decisions/candidate_plan.go annotates
 *    each readable, still-parked DecisionLifecycle draft with exactly one
 *    bounded `panel plan --target-kind decision_candidate --target <id>
 *    --supervised --max-items 1 --json` action. It creates review evidence;
 *    it never imports, accepts, promotes, or writes decision source material.
 *
 *  - "route-promotions": internal/routeevidence/review_queue.go emits
 *    `actions: [{id: "route_promotion_record",
 *      argv: ["compound","route","promotion-record","--review",<path>]}]` on
 *    every review_required item (landed same-change with this allowlist
 *    entry; the earlier Actions=nil gap is closed). --rationale is REQUIRED
 *    by the CLI and deliberately absent from the projected argv — the
 *    rationale-append rule below supplies it from operator input.
 *  - "panel-verdicts": internal/panel/review_queue.go emits one clean
 *    per-member `panel_verdict_record_member` action
 *    (argv ["compound","panel","verdict-record","--review",<path>,
 *    "--reviewed-by",<actor>]) beside its copy-only shell choices (landed
 *    eeaab41). Same rationale-append rule applies.
 *  - "lease-hygiene": NOT a reviews area at all -- internal/attention emits
 *    ONE cross-executor `lease-hygiene` needs_you group (packet:
 *    lease-hygiene-verb) carrying a `safe_batch_action`-shaped `action`
 *    field: `{id: "lease-hygiene.sweep", argv: ["compound","home",
 *    "archive-history","--kind","lease"]}`. See ATTENTION_SOURCED_GENERIC_AREAS
 *    below for how this area's actions get resolved from `compound attention
 *    --json` instead of `compound reviews --json --area <area>`.
 *  - "skill-surfacing": internal/capgraph/surfacing_misses.go's
 *    AnnotateSkillSurfacingQueue emits `actions: [{id: "skill-surfacing.import",
 *    argv: ["compound","capability","import-surfacing-misses",<JSONPath>]}]`
 *    on every OPEN skill-surfacing review item (packet
 *    2026-07-15-slice2A-skill-surfacing-verb) -- resolved items (a generic
 *    resolution sidecar already recorded) carry none. The verb ADDITIVELY
 *    records that same generic resolution after a successful apply, so
 *    running it CLEARS the card. Verified live 2026-07-15 against the Go
 *    source and the live open card under the home's
 *    reviews/skill-surfacing/ directory.
 */
export const GENERIC_ACTION_ALLOWLIST: Record<string, string[][]> = {
  "job-health": [["authority", "job-health-review"]],
  "source-control-cleanup": [["authority", "dirty-review"]],
  "observation-candidates": [["panel", "plan"]],
  "decision-candidates": [["panel", "plan"]],
  "route-promotions": [["route", "promotion-record"]],
  "panel-verdicts": [["panel", "verdict-record"]],
  "lease-hygiene": [["home", "archive-history"]],
  "skill-surfacing": [["capability", "import-surfacing-misses"]],
  "harness-edit-proposals": [["authority", "harness-edit-review"]],
  "product-hypotheses": [["product", "hypothesis-approve"]],
};

function hasOwnKey<T extends object>(value: T, key: PropertyKey): boolean {
  return Object.prototype.hasOwnProperty.call(value, key);
}

export type GenericActionEffectKind = "apply" | "record" | "track";

const GENERIC_ACTION_EFFECT_KIND: Record<string, GenericActionEffectKind> = {
  "job-health": "record",
  "source-control-cleanup": "record",
  "observation-candidates": "track",
  "decision-candidates": "track",
  "route-promotions": "record",
  "panel-verdicts": "record",
  "lease-hygiene": "apply",
  "skill-surfacing": "apply",
  "harness-edit-proposals": "record",
  "product-hypotheses": "record",
};

export function genericActionEffectKind(area: string): GenericActionEffectKind | null {
  return hasOwnKey(GENERIC_ACTION_EFFECT_KIND, area) ? GENERIC_ACTION_EFFECT_KIND[area] ?? null : null;
}

/**
 * Stable optimistic-concurrency token shared by Attention (requires_confirmation)
 * and Reviews (requires_review). The readable JSON payload is intentional: the
 * token is an exact comparison value, not a secret or an authority signature.
 */
export function genericActionRevision(rawAction: unknown): string {
  const action = rawAction && typeof rawAction === "object" ? rawAction as Json : {};
  const requiresReview = Boolean(action.requires_review ?? action.requires_confirmation);
  const actionIntent = action.action_intent && typeof action.action_intent === "object" ? action.action_intent as Json : {};
  const authority = action.policy_authority && typeof action.policy_authority === "object" ? action.policy_authority as Json : {};
  return `generic-action-v2:${JSON.stringify([
    str(action.id),
    str(action.label),
    arr(action.argv).map(String),
    requiresReview,
    str(actionIntent.id),
    str(authority.revision),
  ])}`;
}

const DECISION_CANDIDATE_ACTION_ID = "plan_decision_candidate_review";
const DECISION_CANDIDATE_REVIEW_TASK =
  "review falsifier, review trigger, source ownership, and second-order effects";

function decisionCandidatePlanArgvMatches(rawArgv: unknown, expectedCandidateId = ""): boolean {
  const argv = arr(rawArgv).map(String);
  // The argv-only fallback is for client presentation honesty. The server
  // resolver always supplies the non-empty owner-projected candidate id.
  const candidateId = expectedCandidateId || argv[6] || "";
  if (!candidateId) return false;
  const expected = [
    "compound",
    "panel",
    "plan",
    "--target-kind",
    "decision_candidate",
    "--target",
    candidateId,
    "--domain",
    "reasoning-lenses",
    "--task",
    DECISION_CANDIDATE_REVIEW_TASK,
    "--supervised",
    "--max-items",
    "1",
    "--json",
  ];
  return argv.length === expected.length && argv.every((value, index) => value === expected[index]);
}

const REVIEW_RESOLUTION_REVISION_PATTERN = /^review-resolution-v2:sha256:[0-9a-f]{64}$/;

/**
 * Opaque optimistic-concurrency token for a synthetic Park/Dismiss control.
 * Go stamps it after every owner annotator and the final queue recount. The
 * console deliberately does not reproduce that hash from a selective client
 * view: one meaning owner computes it, every other runtime only carries it.
 */
export function reviewResolutionRevision(area: string, rawItem: unknown): string {
  const item = rawItem && typeof rawItem === "object" ? rawItem as Json : {};
  if (!area || str(item.area) !== area) return "";
  const revision = str(item.review_resolution_revision);
  return REVIEW_RESOLUTION_REVISION_PATTERN.test(revision) ? revision : "";
}

/**
 * Gate 3 — flat, area-agnostic defense in depth. Reuses the exported
 * `argvMatchesAllowlist` function decision-registry.ts already uses for
 * every bespoke spec's own `argvAllowlist`; this array is local to the
 * generic pathway and extended only with the exact verbs the seed areas
 * above need, never blanket.
 */
const GENERIC_GLOBAL_ARGV_ALLOWLIST: string[][] = [
  ["authority", "job-health-review"],
  ["authority", "dirty-review"],
  ["panel", "plan"],
  ["route", "promotion-record"],
  ["panel", "verdict-record"],
  ["home", "archive-history"],
  ["capability", "import-surfacing-misses"],
  ["authority", "harness-edit-review"],
  ["product", "hypothesis-approve"],
];

/** Client-side honesty probe using the same two allowlists as execution. */
export function genericActionProjectionEligible(area: string, rawArgv: unknown): boolean {
  const argv = arr(rawArgv).map(String);
  if (argv.length < 2 || argv[0] !== "compound") return false;
  if (area === "decision-candidates" && !decisionCandidatePlanArgvMatches(argv)) return false;
  const bare = argv.slice(1);
  const areaAllowlist = hasOwnKey(GENERIC_ACTION_ALLOWLIST, area) ? GENERIC_ACTION_ALLOWLIST[area] : undefined;
  return Boolean(
    areaAllowlist &&
      argvMatchesAllowlist(bare, areaAllowlist) &&
      argvMatchesAllowlist(bare, GENERIC_GLOBAL_ARGV_ALLOWLIST),
  );
}

function actionFromItem(item: Json, actionId: string): Json | null {
  return arr(item.actions).find((a) => str(a.id) === actionId) ?? null;
}

/** Locate the item (by `items[].id`) or group (by `groups[].key`) carrying itemId. */
function findItemOrGroup(
  projection: Json | null,
  itemId: string,
): { item: Json | null; group: Json | null } {
  const item = arr(projection?.items).find((it) => str(it.id) === itemId) ?? null;
  if (item) return { item, group: null };
  const group = arr(projection?.groups).find((g) => str(g.key) === itemId) ?? null;
  return { item: null, group };
}

/**
 * Pure core: resolves + gates an action against an already-fetched
 * projection. Kept side-effect-free and injectable so
 * generic-actions.selftest.ts can exercise every gate with a crafted
 * fixture — no on-disk compound runtime home, no subprocess, no network.
 */
export function resolveGenericActionFromProjection(
  projection: Json | null,
  area: string,
  itemId: string,
  actionId: string,
  rationale?: string,
): GenericActionResolution {
  const { item, group } = findItemOrGroup(projection, itemId);

  let action: Json | null = null;
  if (item) {
    action = actionFromItem(item, actionId);
  } else if (group && str((group.safe_batch_action as Json | undefined)?.id) === actionId) {
    action = (group.safe_batch_action as Json) ?? null;
  }

  if (!action) {
    return {
      ok: false,
      rejection: { gate: "not-found", message: `no action "${actionId}" found for ${area}/${itemId}` },
    };
  }

  const rawArgv = arr(action.argv).map(String);

  // Gate 1: clean compound argv only. Shell forms (argv[0] !== "compound",
  // e.g. panel per-item "/bin/sh -lc ...") stay copy-only forever.
  if (rawArgv.length < 2 || rawArgv[0] !== "compound") {
    return {
      ok: false,
      rejection: {
        gate: "gate-1-shell-form",
        message: `action "${actionId}" is not a clean compound command; copy it instead of running it here`,
      },
    };
  }

  const decisionCandidateId = item ? str(item.candidate_id) : "";
  const actionRequiresReview = Boolean(action.requires_review ?? action.requires_confirmation);
  if (
    area === "decision-candidates" &&
    (
      actionId !== DECISION_CANDIDATE_ACTION_ID ||
      !item ||
      !decisionCandidateId ||
      !actionRequiresReview ||
      !decisionCandidatePlanArgvMatches(rawArgv, decisionCandidateId)
    )
  ) {
    return {
      ok: false,
      rejection: {
        gate: "gate-2-allowlist",
        message: `action "${actionId}" is not the exact bounded decision-candidate review plan`,
      },
    };
  }

  const bareArgv = rawArgv.slice(1);

  // Gate 2: area must be certified AND the resolved verb must match one of
  // that area's certified prefixes.
  const areaAllowlist = hasOwnKey(GENERIC_ACTION_ALLOWLIST, area) ? GENERIC_ACTION_ALLOWLIST[area] : undefined;
  if (!areaAllowlist || !argvMatchesAllowlist(bareArgv, areaAllowlist)) {
    return {
      ok: false,
      rejection: {
        gate: "gate-2-allowlist",
        message: `area "${area}" is not certified for generic execution of "${bareArgv.join(" ")}"`,
      },
    };
  }

  // Gate 3: defense-in-depth flat check, independent of gate 2's per-area map.
  if (!argvMatchesAllowlist(bareArgv, GENERIC_GLOBAL_ARGV_ALLOWLIST)) {
    return {
      ok: false,
      rejection: {
        gate: "gate-3-global-allowlist",
        message: `command "${bareArgv.join(" ")}" is not in the global generic-action allowlist`,
      },
    };
  }

  // Rationale-append rule: runs AFTER the gates (appending flags cannot
  // change the verb the gates certified). Only for areas whose CLI requires
  // --rationale; the projected argv never carries one (asserted below so a
  // future backend placeholder can't sneak past).
  let finalArgv = bareArgv;
  const operatorInput = OPERATOR_INPUTS[area];
  if (operatorInput) {
    if (bareArgv.includes(operatorInput.flag)) {
      return {
        ok: false,
        rejection: {
          gate: "gate-2-allowlist",
          message: `projected argv for "${actionId}" unexpectedly carries ${operatorInput.flag}; refusing to run a placeholder`,
        },
      };
    }
    const text = (rationale ?? "").trim();
    if (!text) {
      return {
        ok: false,
        rejection: operatorInput.flag === "--rationale"
          ? {
              gate: "rationale-required",
              message: `action "${actionId}" requires an operator rationale; it becomes provenance on the recorded review`,
            }
          : {
              gate: "operator-input-required",
              message: `action "${actionId}" requires the operator's ${operatorInput.label.toLowerCase()}; it is appended as ${operatorInput.flag} and the verb refuses the placeholder`,
            },
      };
    }
    finalArgv = [...finalArgv, operatorInput.flag, text];
  }

  const authority = parseActionAuthorityBundle(action);
  if (!authority || !authority.current) {
    return {
      ok: false,
      rejection: {
        gate: "authority-unavailable",
        message: `action "${actionId}" has no complete current owner authority bundle`,
      },
    };
  }
  if (!authority.allowed) {
    return {
      ok: false,
      rejection: {
        gate: "authority-not-allowed",
        message: authority.wording,
      },
    };
  }

  return {
    ok: true,
    action: {
      argv: finalArgv,
      actionId,
      label: str(action.label),
      // Normalized the same way genericActionRevision already is: an
      // attention-sourced action carries `requires_confirmation`, never
      // `requires_review` (that field belongs to the reviews projection).
      // Falling back keeps the 428-confirm gate in decision-execute.ts
      // enforcing for both projection shapes without that file knowing which
      // one it got.
      requiresReview: Boolean(action.requires_review ?? action.requires_confirmation),
      policyNote: str(action.policy_note),
      revision: genericActionRevision(action),
      actionIntentId: authority.actionIntentId,
      authorityRevision: authority.authorityRevision,
      authority,
    },
  };
}

/**
 * Areas whose generic actions come from `compound attention --json` (a
 * cross-source needs_you projection) instead of `compound reviews --json
 * --area <area>` (a per-area review queue) -- there is no bespoke reviews
 * area for these; the group IS the executable unit. See
 * adaptAttentionProjectionForArea for the shape translation.
 */
export const ATTENTION_SOURCED_GENERIC_AREAS: ReadonlySet<string> = new Set(["lease-hygiene"]);

/**
 * Reshapes `compound attention --json`'s `groups[]` (each carrying a plain
 * `action` field) into the `{groups:[{key, safe_batch_action}]}` shape
 * resolveGenericActionFromProjection already understands from the reviews
 * projection -- so the same three gates apply unmodified. Pure and
 * deterministic: filtering by `area` and renaming `action` to
 * `safe_batch_action` changes nothing else about the action payload, so
 * genericActionRevision (which normalizes `requires_review ??
 * requires_confirmation`) yields the identical token whether it runs over
 * the client's rendered attention action or this server-side adaptation.
 */
export function adaptAttentionProjectionForArea(attention: Json | null, area: string): Json {
  const groups = arr(attention?.groups)
    .filter((raw) => str((raw as Json).area) === area)
    .map((raw) => {
      const g = raw as Json;
      return { key: str(g.key), safe_batch_action: g.action ?? null };
    });
  return { groups };
}

/**
 * The act path always resolves against a COLD read: decision-execute.ts
 * deliberately calls invalidateRunJsonCache() before resolving, so owner
 * truth is re-fetched outside the read cache — and `compound attention
 * --json` takes ~13-17s wall today, past the 15s runJson default. Grant
 * resolution the same extended budget the dashboard doors get (page.tsx,
 * 45s per door): a click is worth waiting for, and the argv is the same
 * cache key, so a successful read warms both surfaces.
 */
const GENERIC_RESOLUTION_TIMEOUT_MS = 45_000;

/** Minimal slice of ProjectionRead the resolver consumes — lets the selftest
 * craft reads with no runtime home, subprocess, or network. */
export type GenericResolutionRead = {
  status: ProjectionStatus;
  value: Json | null;
  error: string | null;
};

export type LegacyAuthorityCutoverGuard =
  | { ok: true }
  | { ok: false; rejection: GenericActionRejection };

export type ReviewResolutionGuard =
  | { ok: true; revision: string }
  | { ok: false; rejection: GenericActionRejection };

/**
 * Fail-closed guard for the fixed Park/Dismiss adapter. The item must still
 * exist in the exact area, expose no item- or group-scoped owner action, and
 * match the revision the operator actually reviewed.
 */
export function reviewResolutionGuardFromRead(
  read: GenericResolutionRead,
  area: string,
  itemId: string,
  expectedRevision: string,
): ReviewResolutionGuard {
  if (read.status !== "live") {
    return {
      ok: false,
      rejection: {
        gate: "projection-unavailable",
        message:
          `the reviews projection did not return fresh owner truth, so no disposition was recorded` +
          (read.error ? ` (${read.error})` : ""),
      },
    };
  }
  const item = arr(read.value?.items).find((candidate) => str(candidate.id) === itemId) ?? null;
  if (!item || str(item.area) !== area) {
    return {
      ok: false,
      rejection: { gate: "not-found", message: `review item ${area}/${itemId} is no longer available` },
    };
  }
  const groups = arr(read.value?.groups).filter((group) => arr(group.item_ids).map(String).includes(itemId));
  const hasOwnerAction =
    arr(item.actions).length > 0 ||
    groups.some((group) => Boolean(group.safe_batch_action || group.action));
  if (hasOwnerAction) {
    return {
      ok: false,
      rejection: {
        gate: "owner-action-present",
        message: "the owner now exposes an action for this review; refresh and use that governed workflow",
      },
    };
  }
  const revision = reviewResolutionRevision(area, item);
  if (!expectedRevision || revision !== expectedRevision) {
    return {
      ok: false,
      rejection: {
        gate: "stale-action-revision",
        message: "review state changed since this disposition was rendered; refresh before deciding",
      },
    };
  }
  return { ok: true, revision };
}

export async function readReviewResolutionProjection(area: string): Promise<GenericResolutionRead> {
  return runProjection(["reviews", "--json", "--area", area], { timeoutMs: GENERIC_RESOLUTION_TIMEOUT_MS });
}

/**
 * Pure status gate between the projection read and the three-gate core. Only
 * a live read can authorize execution. `stale` may still carry a last-good
 * value for explanation, while `unreachable` means no successful value has
 * ever landed for this argv; neither may cross the action seam. Collapsing an
 * unavailable read into "your item does not exist" was the cold-cache 404.
 */
export function resolveGenericActionFromRead(
  read: GenericResolutionRead,
  area: string,
  itemId: string,
  actionId: string,
  rationale?: string,
): GenericActionResolution {
  if (read.status !== "live") {
    const source = ATTENTION_SOURCED_GENERIC_AREAS.has(area) ? "attention" : "reviews";
    return {
      ok: false,
      rejection: {
        gate: "projection-unavailable",
        message:
          `the ${source} projection is not live, so nothing was run — retry in a moment` +
          (read.error ? ` (${read.error})` : ""),
      },
    };
  }
  const projection = ATTENTION_SOURCED_GENERIC_AREAS.has(area)
    ? adaptAttentionProjectionForArea(read.value, area)
    : read.value;
  return resolveGenericActionFromProjection(projection, area, itemId, actionId, rationale);
}

/**
 * Guard a narrowly declared legacy-to-stamped cutover for one concrete item.
 * Once that producer is armed, an owner action of any policy outcome owns the
 * semantics. Missing/malformed/non-current authority and unavailable owner
 * truth fail closed; only a live item with no owner action may use its legacy
 * bespoke form.
 */
export function legacyAuthorityCutoverGuardFromRead(
  read: GenericResolutionRead,
  area: string,
  itemId: string,
): LegacyAuthorityCutoverGuard {
  if (read.status !== "live") {
    return {
      ok: false,
      rejection: {
        gate: "projection-unavailable",
        message: `current owner truth is ${read.status}; the legacy action cannot run` +
          (read.error ? ` (${read.error})` : ""),
      },
    };
  }
  const projection = ATTENTION_SOURCED_GENERIC_AREAS.has(area)
    ? adaptAttentionProjectionForArea(read.value, area)
    : read.value;
  const { item, group } = findItemOrGroup(projection, itemId);
  const actions = item
    ? arr(item.actions)
    : group
      ? [group.safe_batch_action ?? group.action].filter(Boolean)
      : [];
  if (actions.length === 0) return { ok: true };
  for (const raw of actions) {
    const action = raw && typeof raw === "object" ? raw as Json : null;
    const actionId = str(action?.id);
    const authority = parseActionAuthorityBundle(action);
    if (!actionId || !authority || !authority.current) {
      return {
        ok: false,
        rejection: {
          gate: "authority-unavailable",
          message: "the current owner action has incomplete or non-current authority; refresh before acting",
        },
      };
    }
    return {
      ok: false,
      rejection: {
        gate: "owner-action-present",
        message: `the owner now exposes action "${actionId}" for this item; refresh and use that governed workflow`,
      },
    };
  }
  return { ok: true };
}

/**
 * Server-side re-resolution. NEVER trusts client-supplied argv: re-fetches
 * the owning projection fresh (through the same memoized read path every
 * page uses) and resolves the action from that live projection, so a
 * stale/forged client argv can never be replayed. Attention-sourced areas
 * (ATTENTION_SOURCED_GENERIC_AREAS) read `compound attention --json` through
 * the adapter above; every other area keeps reading `compound reviews --json
 * --area <area>` as before.
 */
export async function resolveGenericAction(
  area: string,
  itemId: string,
  actionId: string,
  rationale?: string,
): Promise<GenericActionResolution> {
  const argv = ATTENTION_SOURCED_GENERIC_AREAS.has(area)
    ? ["attention", "--json"]
    : ["reviews", "--json", "--area", area];
  const read = await runProjection(argv, { timeoutMs: GENERIC_RESOLUTION_TIMEOUT_MS });
  return resolveGenericActionFromRead(read, area, itemId, actionId, rationale);
}
