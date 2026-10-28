import type { ReactNode } from "react";

/**
 * Doctrine empty-state (DESIGN.md §3/§7.9): dashed hairline, page-tone fill,
 * constitution copy — never "No data". Tokens only. Optional single CTA slot.
 */
export function EmptyState({ children, cta }: { children: ReactNode; cta?: ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-2 rounded-lg border border-dashed border-hair bg-page px-4 py-6 text-center text-sm text-muted">
      <span className="leading-relaxed">{children}</span>
      {cta ? <div>{cta}</div> : null}
    </div>
  );
}
