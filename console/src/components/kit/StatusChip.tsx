import type { ReactNode } from "react";

/**
 * A small dot + label status chip for topbar chrome (DESIGN.md §3/§4) — the
 * kill-switch / autonomy posture indicator. Distinct from FreshnessChip (which
 * is about data recency): this states a system posture. Tokens only; the dot
 * carries the tone, the pill stays quiet so it never competes with the amber
 * seam grammar.
 */
export type StatusTone = "good" | "seam" | "crit" | "muted" | "work";

const DOT: Record<StatusTone, string> = {
  good: "bg-good",
  seam: "bg-seam",
  crit: "bg-crit",
  muted: "bg-faint",
  work: "bg-work",
};

const TEXT: Record<StatusTone, string> = {
  good: "text-good",
  seam: "text-seam",
  crit: "text-crit",
  muted: "text-muted",
  work: "text-work",
};

export function StatusChip({
  tone = "muted",
  label,
  title,
  icon,
  pulse = false,
}: {
  tone?: StatusTone;
  label: ReactNode;
  title?: string;
  icon?: ReactNode;
  pulse?: boolean;
}) {
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1.5 rounded-pill border border-hair bg-soft px-2.5 py-0.5 text-2xs font-semibold tabular-nums ${TEXT[tone]}`}
    >
      {icon ?? (
        pulse ? (
          <span className={`live-dot ${TEXT[tone]}`} aria-hidden />
        ) : (
          <span className={`h-1.5 w-1.5 rounded-full ${DOT[tone]}`} aria-hidden />
        )
      )}
      {label}
    </span>
  );
}
