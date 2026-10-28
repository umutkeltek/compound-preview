/**
 * The one status-pill grammar (DESIGN.md §2): ~15 lifecycle states collapse
 * into 7 tones. Replaces STATE_TONE/MOVE_TONE (src/components/ui.tsx) where
 * `claimed` -> violet stole the memory hue; under this grammar domain lives
 * on the seam only, never the pill.
 */
export type StateTone = "draft" | "queued" | "live" | "seam" | "done" | "failed" | "dropped";

const STATE_TONE_MAP: Record<string, StateTone> = {
  // draft — "nothing runs yet"
  proposed: "draft",
  suggested: "draft",
  candidate: "draft",
  // queued — "in line"
  admitted: "queued",
  ready: "queued",
  claimed: "queued",
  watching: "queued",
  parked: "queued",
  // live — the only animated pill
  running: "live",
  in_progress: "live",
  verifying: "live",
  // seam — the operator's gate
  held: "seam",
  needs_review: "seam",
  blocked: "seam",
  hold: "seam",
  needs_you: "seam",
  "needs-you": "seam",
  // done
  completed: "done",
  promoted: "done",
  allow: "done",
  filed: "done",
  // failed
  failed: "failed",
  deny: "failed",
  rejected: "failed",
  // dropped — muted, no border emphasis
  cancelled: "dropped",
  demoted: "dropped",
  no_promote: "dropped",
};

/** Map any of the ~15 lifecycle state strings onto one of the 7 pill tones. Unknown -> queued. */
export function toneForState(state: string): StateTone {
  return STATE_TONE_MAP[state] ?? "queued";
}

const TONE_CLASSES: Record<StateTone, string> = {
  draft: "border-hair bg-transparent text-muted", // hollow outline
  queued: "border-hair bg-soft-2 text-muted", // slate filled
  live: "border-work-line bg-work-soft text-work", // work-blue + pulse dot
  seam: "border-seam-line bg-seam-soft text-seam",
  done: "border-good-line bg-good-soft text-good",
  failed: "border-crit-line bg-crit-soft text-crit",
  dropped: "border-transparent bg-soft text-faint",
};

export function StateBadge({ state, label }: { state: string; label?: string }) {
  const tone = toneForState(state);
  const text = label ?? (state ? state.replace(/_/g, " ") : "unknown");
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-pill border px-2.5 py-0.5 text-xs font-semibold tabular-nums ${TONE_CLASSES[tone]}`}
    >
      {tone === "live" ? <span className="live-dot" aria-hidden /> : null}
      {text}
    </span>
  );
}
