/**
 * Hand-built SVG sparkline (DESIGN.md §8 — "no chart lib for v1"). Pure,
 * deterministic, tokens-only: the line is `currentColor`, so a caller tints it
 * by setting a domain/severity text color on the wrapping element (e.g.
 * `text-work`). No axes, no labels — a KPI whisper, not a chart.
 *
 * Draws nothing for <2 points (a single sample is not a trend). Values are
 * normalized over their own min/max so a flat series still reads as a flat
 * line rather than collapsing to the floor.
 */
export function Sparkline({
  values,
  width = 60,
  height = 20,
  strokeWidth = 1.5,
  fill = true,
  className = "",
}: {
  values: number[];
  width?: number;
  height?: number;
  strokeWidth?: number;
  fill?: boolean;
  className?: string;
}) {
  const clean = values.filter((v) => Number.isFinite(v));
  if (clean.length < 2) return null;

  const min = Math.min(...clean);
  const max = Math.max(...clean);
  const span = max - min || 1;
  const pad = strokeWidth; // keep the stroke inside the box
  const stepX = (width - pad * 2) / (clean.length - 1);

  const points = clean.map((v, i) => {
    const x = pad + i * stepX;
    const y = pad + (height - pad * 2) * (1 - (v - min) / span);
    return [x, y] as const;
  });

  const line = points.map(([x, y], i) => `${i === 0 ? "M" : "L"}${x.toFixed(2)} ${y.toFixed(2)}`).join(" ");
  const area = `${line} L${points[points.length - 1][0].toFixed(2)} ${height} L${points[0][0].toFixed(2)} ${height} Z`;

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      fill="none"
      aria-hidden
      className={className}
    >
      {fill ? <path d={area} fill="currentColor" opacity={0.1} /> : null}
      <path
        d={line}
        fill="none"
        stroke="currentColor"
        strokeWidth={strokeWidth}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <circle cx={points[points.length - 1][0]} cy={points[points.length - 1][1]} r={strokeWidth} fill="currentColor" />
    </svg>
  );
}
