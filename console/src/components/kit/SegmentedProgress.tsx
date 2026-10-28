import { type Tone, TONE_FILL } from "@/components/kit/tone";

/**
 * SegmentedProgress (DESIGN.md §9.3) — a count pill ("2/4") + n discrete
 * segments for a staged process: done = good-fg, the one active stage = work-fg
 * and breathing (`.seg-active`), the rest = `--soft-2`. On work cards it shows
 * envelope-stage position; it also serves batch-apply progress and phase
 * completion.
 *
 * `done` segments are complete; `activeIndex` (>= done) is the single in-flight
 * stage. Omit `activeIndex` (or set null) for a stalled/terminal process — then
 * nothing pulses. The active fill can be re-toned (e.g. seam for a held gate)
 * via `activeTone`.
 */
export function SegmentedProgress({
  total,
  done,
  activeIndex = null,
  activeTone = "work",
  pulse = true,
  label,
  showCount = true,
  className = "",
}: {
  total: number;
  done: number;
  activeIndex?: number | null;
  activeTone?: Tone;
  /** the active segment breathes only when genuinely live (§9.4). */
  pulse?: boolean;
  label?: string;
  showCount?: boolean;
  className?: string;
}) {
  const n = Math.max(0, total);
  const complete = Math.max(0, Math.min(n, done));
  const active = activeIndex == null ? null : Math.max(0, Math.min(n - 1, activeIndex));

  return (
    <div className={`flex items-center gap-2 ${className}`}>
      {showCount ? (
        <span className="mono shrink-0 text-2xs font-semibold tabular-nums text-faint">
          {complete}/{n}
        </span>
      ) : null}
      <span
        className="flex flex-1 items-center gap-1"
        role="img"
        aria-label={label ?? `${complete} of ${n} stages complete`}
      >
        {Array.from({ length: n }, (_, i) => {
          const isDone = i < complete;
          const isActive = active != null && i === active && !isDone;
          const fill = isDone
            ? "bg-good"
            : isActive
              ? `${TONE_FILL[activeTone]}${pulse ? " seg-active" : ""}`
              : "bg-soft-2";
          return <span key={i} className={`h-1.5 flex-1 rounded-pill ${fill}`} />;
        })}
      </span>
    </div>
  );
}
