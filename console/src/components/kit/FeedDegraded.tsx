import { TONE_BORDER, TONE_SOFT_BG, TONE_TEXT } from "@/components/kit/tone";
import { ago } from "@/lib/time";

/**
 * Honest-degradation seam (packet: console-honest-degradation, 2026-07-14).
 * A dead projection door must never render as the doctrine "Clean seam." /
 * "No moves recorded yet." empty state - that reads as a healthy system when
 * the feed is actually unreachable. Amber SEAM tokens (DESIGN.md §0/§9.3 -
 * "your gate"), never crit red: an unreachable read is a seam to notice, not
 * an alarm to sound.
 *
 * Deliberately client-safe (no value import of `@/lib/compound`, which pulls
 * node builtins) - `ago` comes from the pure `@/lib/time` module, same
 * convention as FreshnessChip, so this atom stays usable from a future client
 * consumer without tripping the client-bundle guard.
 */
export type FeedProjectionStatus = "live" | "stale" | "unreachable";

export type FeedRenderModel = "degraded" | "empty" | "data";

/**
 * The one gate every honest-degradation consumer shares: `unreachable` is
 * ALWAYS `degraded`, regardless of whatever data happens to be on hand - the
 * structural honesty rule (never let a swallowed failure read as fine). The
 * doctrine empty state is earned ONLY by a genuinely empty `live` read.
 * Everything else (live-with-data, stale-with-last-good-data) renders the
 * caller's normal data path unchanged - `data`.
 */
export function feedRenderModel(status: FeedProjectionStatus, hasData: boolean): FeedRenderModel {
  if (status === "unreachable") return "degraded";
  if (status === "live" && !hasData) return "empty";
  return "data";
}

export function FeedDegraded({
  label,
  source,
  lastGoodAt,
}: {
  /** plain-language feed name, e.g. "Decide", "Pipeline", "Live activity". */
  label: string;
  /** the exact compound command this feed reads - provenance, never hidden. */
  source: string;
  lastGoodAt?: string | null;
}) {
  const age = lastGoodAt ? ago(lastGoodAt).replace(/\s*ago$/, "") : "";
  return (
    <div
      className={`flex flex-col items-start gap-2 rounded-lg border ${TONE_BORDER.seam} ${TONE_SOFT_BG.seam} px-4 py-5 text-sm ${TONE_TEXT.seam}`}
    >
      <p className="leading-relaxed">
        The {label} feed is unreachable — showing nothing rather than guessing.
      </p>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-2xs">
        <span className="mono" title={source}>
          {source}
        </span>
        {age ? <span>last good {age} ago</span> : null}
      </div>
    </div>
  );
}
