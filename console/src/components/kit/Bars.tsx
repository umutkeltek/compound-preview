import { TooltipCard } from "@/components/kit/TooltipCard";
import type { Tone } from "@/components/kit/tone";

/**
 * Hand-built horizontal bar rows (DESIGN.md §8). "compare → table"-adjacent,
 * but for a tiny fixed set (a move-type mix, a lane split) a labelled bar reads
 * faster than a table. Tokens only; the fill color is a semantic tone, never a
 * raw palette color. `tabular-nums` on every value so columns align.
 *
 * Bars are proportioned against the largest value in the set (or an explicit
 * `max`), with a small floor so a non-zero-but-tiny value still shows a sliver.
 * With `withTooltip`, each bar answers on hover with a TooltipCard (§9.4) —
 * label + value + share of the busiest bar.
 */
export type BarTone = "work" | "memory" | "capability" | "route" | "product" | "good" | "seam" | "crit" | "muted";

const BAR_TOOLTIP_TONE: Record<BarTone, Tone> = {
  work: "work",
  memory: "memory",
  capability: "capability",
  route: "route",
  product: "product",
  good: "good",
  seam: "seam",
  crit: "crit",
  muted: "neutral",
};

const FILL: Record<BarTone, string> = {
  work: "bg-work",
  memory: "bg-memory",
  capability: "bg-capability",
  route: "bg-route",
  product: "bg-product",
  good: "bg-good",
  seam: "bg-seam",
  crit: "bg-crit",
  muted: "bg-faint",
};

export type BarItem = { label: string; value: number; tone?: BarTone; unit?: string };

export function Bars({
  items,
  max,
  withTooltip = false,
  className = "",
}: {
  items: BarItem[];
  max?: number;
  /** wrap each bar in a TooltipCard (label + value + share) — §9.4. */
  withTooltip?: boolean;
  className?: string;
}) {
  const ceiling = Math.max(1, max ?? Math.max(...items.map((i) => i.value), 0));
  return (
    <div className={`grid gap-2.5 ${className}`}>
      {items.map((item) => {
        const pct = item.value > 0 ? Math.max(5, Math.round((item.value / ceiling) * 100)) : 0;
        const share = Math.round((item.value / ceiling) * 100);
        const tone = item.tone ?? "muted";
        const track = (
          <span className="block h-1.5 overflow-hidden rounded-pill bg-hair">
            <span className={`block h-full rounded-pill ${FILL[tone]}`} style={{ width: `${pct}%` }} />
          </span>
        );
        return (
          <div
            key={item.label}
            className="grid items-center gap-3 text-xs"
            style={{ gridTemplateColumns: "68px 1fr 28px" }}
          >
            <span className="truncate text-muted">{item.label}</span>
            {withTooltip ? (
              <TooltipCard
                side="top"
                title={item.label}
                rows={[
                  { label: item.unit ?? "value", value: item.value.toLocaleString("en-US"), tone: BAR_TOOLTIP_TONE[tone] },
                ]}
                derived={{ label: "of busiest", value: `${share}%` }}
                trigger={
                  <button
                    type="button"
                    aria-label={`${item.label}: ${item.value}`}
                    className="cursor-default rounded-pill transition-shadow hover:shadow"
                  >
                    {track}
                  </button>
                }
              />
            ) : (
              track
            )}
            <span className="text-right tabular-nums text-muted">{item.value.toLocaleString("en-US")}</span>
          </div>
        );
      })}
    </div>
  );
}
