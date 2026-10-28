/**
 * Plain-language glossary (EP-U1): one-liners for every term and review area
 * an operator sees. The design law: if a term needs a glossary to be
 * understood, any UI showing the term must carry its one-liner in place.
 * Single source for tooltips/subtitles across the console.
 */

export const AREA_GLOSSARY: Record<string, string> = {
  "external-repo-intake": "Repos or frameworks you shared, awaiting your keep / reference / reject decision.",
  "route-promotions": "Model lanes that performed well and are proposed for an upgraded role.",
  "route-remorse": "A routing choice that looks wrong in hindsight — review whether the lane should be demoted.",
  "panel-verdicts": "Judge-panel results waiting to be applied to memory.",
  "synthesis-contradictions": "The memory sweep found claims that disagree; someone must rule.",
  "synthesis-sweeps": "Reports from the background pass that clusters, contradicts, and corroborates memory.",
  "seed-corroborations": "Something you once said just turned out to matter — a later outcome corroborates it. Promote it or leave it parked.",
  "job-health": "Background jobs (crons, daemons) that failed or look unhealthy.",
  "autonomy-grant-mint": "Job-health work is piling up on your click — decide whether proven items may enter the queue on their own.",
  "source-control-cleanup": "Git hygiene chores (dirty files, stray artifacts) queued for cleanup.",
  "distiller-improvements": "The extraction organ proposing upgrades to its own method.",
  "distiller-requests": "Asks for the distiller to (re)process specific material.",
  "capability-lifecycle-proposals": "Proposals to revise, narrow, or retire a skill — mined from your real sessions. Nothing changes without you.",
  "capability-evaluation-intake": "Shared repos queued for a deeper capability evaluation before any adoption.",
  "observation-candidates": "Raw observations that arrived through the intake door, not yet trusted.",
  "source-observation-verdicts": "Verdicts on observed sources waiting for your confirmation.",
  "product-action-cards": "Suggested next moves on a product loop, derived from live signals.",
};

export const TERM_GLOSSARY: Record<string, string> = {
  proposed: "Suggested. Nothing runs until you add it to work.",
  admitted: "Queued. It may be picked up, but nothing runs yet.",
  ready: "Prepared; waiting for an executor to claim it.",
  claimed: "An executor took it (with a lease so two agents can't collide).",
  running: "An executor is working on it now.",
  in_progress: "An executor is working on it now.",
  verifying: "Being proof-checked before close.",
  blocked: "Waiting on a dependency.",
  held: "Parked for your judgment. Nothing happens until you act.",
  completed: "Done, with proof on record.",
  failed: "Did not succeed; closed.",
  cancelled: "Dropped; closed without running.",
  review_required: "Waiting on your decision.",
  needs_review: "Raised for your attention by evidence, not by a schedule.",
  receipt: "Proof a job ran: what was touched, what it cost, what came out.",
  transition: "The ledger entry: the system moved from state A to B because of this work.",
  candidate: "Arrived but not trusted yet. Everything is born a candidate.",
  promote: "Becomes real memory agents can recall. Needs a recorded reason.",
  park: "Keep it, decide later. The safe default.",
};

// Move-type plain-language layer is owned by `./move-types` (EP-U1 plain
// register). The Go side (internal/envelope/glossary.go MoveTypePlain) is the
// canonical one-liner owner; this module re-exports from the client-safe
// move-types module so existing consumers (e.g. components/work/TransitionsTicker)
// keep importing from `@/lib/glossary` without dragging in server-only deps.
// move-type-drift-smoke.mjs asserts the values stay byte-equal to Go.
import { MOVE_TYPE_GLOSSARY } from "./move-types";
export { MOVE_TYPE_LABELS, MOVE_TYPE_GLOSSARY } from "./move-types";

export function areaPlain(area: string): string {
  return AREA_GLOSSARY[area] ?? "A review area awaiting a decision.";
}

/**
 * Replaces the bare "Owner action unavailable" label with a plain-language
 * line naming the missing owner workflow and the honest state — the card is
 * read-only and park/dismiss remain available (decide-resolution-acts.ts
 * always offers them for a copy-mode card). Derived from the area already
 * carried by the read model (DecideGroup.area) — never invents server
 * authority or a new capability.
 */
export function ownerUnavailableMessage(area: string): string {
  const label = area ? area.replace(/-/g, " ") : "this";
  return `No ${label} owner workflow is built yet — this card is read-only; park/dismiss still works.`;
}

export function movePlain(moveType: string): string {
  return MOVE_TYPE_GLOSSARY[moveType] ?? "A recorded move in the work's lifecycle.";
}

/**
 * Known compound-internal jargon reason strings — literally the text
 * decide-model.ts's authority/act resolution emits as `DecideGroup.face.
 * unavailableReason` / a copy-mode `DecideAct.reason` — mapped to one
 * plain operator sentence with a next step. A single `$1` capture carries
 * a dynamic token (e.g. a projection status word) into the plain sentence.
 * Anything not listed here (backend-owned AuthorityFace wording, raw
 * `terminal_reason`, area-specific text) passes through unchanged: it is
 * either already policy-owned plain language or genuinely can't be mapped
 * generically without guessing at backend intent.
 */
const BLOCKED_REASON_PLAIN: Array<[RegExp, string]> = [
  [
    /^No complete current owner authority bundle is available\.$/,
    "Can't apply yet — the owner hasn't exposed an approve/reject action for this group.",
  ],
  [
    /^The owner authority bundle is not current\.$/,
    "Can't apply yet — the owner's last-known permission has expired; refresh to check again.",
  ],
  [
    /^Owner authority evidence is (\w+); execution is unavailable\.$/,
    "Can't apply yet — the live status feed is $1, so actions are paused until it's back.",
  ],
  [
    /^Current owner authority evidence is not live\.$/,
    "Can't apply yet — the live status feed is down, so actions are paused until it's back.",
  ],
  [
    /^The current owner authority has no executable adapter on this surface\.$/,
    "Can't apply yet — this kind of decision has no one-click action built for it yet.",
  ],
  [
    /^The owner exposes no effect-compatible workflow for this decision\.$/,
    "Can't apply yet — this kind of decision has no one-click action built for it yet.",
  ],
  [
    /^Each repository requires its own reviewed owner artifact before it can be decided\.$/,
    "Review each repository on its own — this group can't be decided all at once.",
  ],
];

export function blockedReasonPlain(reason: string): string {
  const trimmed = reason.trim();
  if (!trimmed) return "";
  for (const [pattern, template] of BLOCKED_REASON_PLAIN) {
    const match = trimmed.match(pattern);
    if (match) return template.replace(/\$(\d)/g, (_, i) => match[Number(i)] ?? "");
  }
  return reason;
}
