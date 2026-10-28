import type { ReactNode } from "react";

/**
 * MetaCount (DESIGN.md §9.3) — a 15px icon + tabular count, muted; the `title`
 * expands what it counts. Used for evidence refs, member counts, run counts on
 * cards. Purely a compact metric badge — never interactive on its own.
 */
export function MetaCount({
  icon,
  count,
  title,
  className = "",
}: {
  icon: ReactNode;
  count: number | string;
  title?: string;
  className?: string;
}) {
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1 text-2xs font-medium tabular-nums text-muted ${className}`}
    >
      <span aria-hidden className="text-faint">
        {icon}
      </span>
      {typeof count === "number" ? count.toLocaleString("en-US") : count}
    </span>
  );
}
