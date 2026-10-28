/**
 * DeltaPill (DESIGN.md §9.3) — a signed change chip: ↗/↘ glyph + tabular value,
 * good-soft/crit-soft tint, `title` carrying the comparison window. Direction of
 * "good" is configurable so a burn-down counter (fewer = better) reads green
 * when it drops: `goodDirection="down"`. A zero delta is neutral (no drama).
 *
 * Semantic-token only; the value is always tabular so a column of deltas aligns.
 */
export function DeltaPill({
  value,
  goodDirection = "up",
  suffix = "",
  title,
  showZero = false,
  className = "",
}: {
  value: number;
  goodDirection?: "up" | "down";
  suffix?: string;
  /** comparison window, e.g. "vs previous 7 days" — lands on the hover title. */
  title?: string;
  /** render a neutral "→ 0" chip instead of nothing when the delta is 0. */
  showZero?: boolean;
  className?: string;
}) {
  if (value === 0 && !showZero) return null;

  const up = value > 0;
  const flat = value === 0;
  const glyph = flat ? "→" : up ? "↗" : "↘";
  const isGood = flat ? null : up === (goodDirection === "up");
  const tone = flat
    ? "border-hair bg-soft-2 text-muted"
    : isGood
      ? "border-good-line bg-good-soft text-good"
      : "border-crit-line bg-crit-soft text-crit";
  const magnitude = Math.abs(value).toLocaleString("en-US");
  const sign = flat ? "" : up ? "+" : "−";

  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1 rounded-pill border px-1.5 py-0.5 text-2xs font-semibold tabular-nums ${tone} ${className}`}
    >
      <span aria-hidden>{glyph}</span>
      <span>
        {sign}
        {magnitude}
        {suffix}
      </span>
    </span>
  );
}
