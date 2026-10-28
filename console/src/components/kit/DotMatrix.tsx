import { rampClass, type Tone } from "@/components/kit/tone";
import { TooltipCard } from "@/components/kit/TooltipCard";

/**
 * DotMatrix (DESIGN.md §9.3) — a waffle grid of radius-xs cells whose intensity
 * ramps `--soft-2` → `*-soft` → `*-line` → `*-fg`; every cell answers on hover
 * with a TooltipCard. It shows DENSITY OVER TWO AXES (rows × columns): attention
 * age × kind class, activity coverage, verdict distribution — the SHAPE of a
 * queue, not just its count (§9.6).
 *
 * Each row carries its own domain tone; each cell a 0..3 level + a raw count for
 * the hover card. An empty cell (level 0 / count 0) still renders, so gaps read.
 */
export type MatrixCell = {
  /** 0..3 intensity ramp step. */
  level: number;
  count: number;
  /** column identity, shown in the hover card (e.g. the age bucket). */
  colLabel: string;
  /** optional extra clarifier line in the hover card. */
  sub?: string;
};

export type MatrixRow = { label: string; tone: Tone; cells: MatrixCell[] };

export function DotMatrix({
  columns,
  rows,
  legend = true,
  className = "",
}: {
  columns: string[];
  rows: MatrixRow[];
  legend?: boolean;
  className?: string;
}) {
  const grid = { gridTemplateColumns: `minmax(0,auto) repeat(${columns.length}, 1.25rem)` } as const;

  return (
    <div className={`flex flex-col gap-2 ${className}`}>
      <div className="grid items-center gap-1 text-2xs text-faint" style={grid}>
        <span />
        {columns.map((col) => (
          <span key={col} className="text-center tabular-nums">
            {col}
          </span>
        ))}
        {rows.map((row) => (
          <RowCells key={row.label} row={row} />
        ))}
      </div>

      {legend ? (
        <div className="flex items-center gap-1.5 text-2xs text-faint">
          <span>less</span>
          {([0, 1, 2, 3] as const).map((level) => (
            <span key={level} className={`h-2.5 w-2.5 rounded-xs ${rampClass("neutral", level)}`} aria-hidden />
          ))}
          <span>more</span>
        </div>
      ) : null}
    </div>
  );
}

function RowCells({ row }: { row: MatrixRow }) {
  return (
    <>
      <span className="truncate pr-2 text-2xs font-medium text-muted">{row.label}</span>
      {row.cells.map((cell, i) => (
        <TooltipCard
          key={i}
          side="top"
          title={`${row.label} · ${cell.colLabel}`}
          rows={[{ label: "items", value: cell.count, tone: row.tone }]}
          derived={cell.sub ? { label: cell.sub, value: "" } : undefined}
          trigger={
            <button
              type="button"
              aria-label={`${row.label}, ${cell.colLabel}: ${cell.count}`}
              className={`h-5 w-5 rounded-xs transition-shadow hover:shadow focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-1 focus-visible:outline-focus ${rampClass(
                row.tone,
                cell.level,
              )}`}
            />
          }
        />
      ))}
    </>
  );
}
