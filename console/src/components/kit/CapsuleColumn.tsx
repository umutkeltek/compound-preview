import { type Tone, TONE_FILL, TONE_TEXT } from "@/components/kit/tone";
import { TooltipCard, type TooltipRow } from "@/components/kit/TooltipCard";

/**
 * CapsuleColumn (DESIGN.md §9.3) — a vertical capsule: a full-height `--soft-2`
 * capacity track with a value segment filling from the bottom (domain/state
 * tone), plus an optional `--hatch` segment above it for a PROJECTED value.
 * Answers on hover with a TooltipCard. Used where a capacity semantic genuinely
 * exists (budget vs spend, lane capacity, queue depth by day, lease staleness
 * vs threshold) — never forced where there is no ceiling.
 *
 * `value`/`projected` are measured against `max` (the track ceiling) and clamp
 * to it, so an over-capacity value pins full rather than overflowing.
 */
const TRACK_H = 88; // data-viz geometry constant (like Sparkline's width/height)

export function CapsuleColumn({
  value,
  max,
  projected = 0,
  tone,
  label,
  caption,
  unit = "value",
  extraRows,
  className = "",
}: {
  value: number;
  max: number;
  projected?: number;
  tone: Tone;
  /** axis label rendered under the capsule (e.g. a weekday, an initial). */
  label?: string;
  /** hover-card title; defaults to `label`. */
  caption?: string;
  unit?: string;
  extraRows?: TooltipRow[];
  className?: string;
}) {
  const ceiling = Math.max(1, max);
  const valuePct = Math.max(0, Math.min(100, (value / ceiling) * 100));
  const projPct = Math.max(0, Math.min(100 - valuePct, (projected / ceiling) * 100));

  const rows: TooltipRow[] = [{ label: unit, value: value.toLocaleString("en-US"), tone }];
  if (projected > 0) rows.push({ label: "projected", value: `+${projected.toLocaleString("en-US")}`, tone: "neutral" });
  if (extraRows) rows.push(...extraRows);

  return (
    <TooltipCard
      side="top"
      title={caption ?? label ?? unit}
      rows={rows}
      derived={{ label: "of capacity", value: `${value.toLocaleString("en-US")}/${ceiling.toLocaleString("en-US")}` }}
      trigger={
        <button
          type="button"
          aria-label={`${caption ?? label ?? unit}: ${value} of ${ceiling}`}
          className={`flex flex-col items-center gap-1.5 focus-visible:outline-none ${className}`}
        >
          <span
            className="relative w-6 overflow-hidden rounded-pill bg-soft-2 transition-shadow group-hover:shadow"
            style={{ height: `${TRACK_H}px` }}
          >
            <span
              className={`absolute inset-x-0 bottom-0 rounded-pill ${TONE_FILL[tone]}`}
              style={{ height: `${valuePct}%` }}
              aria-hidden
            />
            {projPct > 0 ? (
              <span
                className={`hatch absolute inset-x-0 ${TONE_TEXT[tone]}`}
                style={{ bottom: `${valuePct}%`, height: `${projPct}%` }}
                aria-hidden
              />
            ) : null}
          </span>
          {label ? <span className="text-2xs font-medium tabular-nums text-faint">{label}</span> : null}
        </button>
      }
    />
  );
}
