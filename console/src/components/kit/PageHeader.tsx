import type { ReactNode } from "react";

/**
 * Header row with a breadcrumb slot + a right-side chip/actions slot
 * (DESIGN.md §3). Toolbar is the companion row for filter/action chrome
 * beneath it.
 */
export function PageHeader({
  breadcrumb,
  title,
  right,
}: {
  breadcrumb?: ReactNode;
  title: ReactNode;
  right?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        {breadcrumb ? <div className="mono mb-1 text-2xs text-faint">{breadcrumb}</div> : null}
        <h1 className="text-xl font-semibold tracking-tight text-ink">{title}</h1>
      </div>
      {right ? <div className="flex min-w-0 w-full flex-wrap items-center gap-2 sm:w-auto sm:justify-end">{right}</div> : null}
    </div>
  );
}

export function Toolbar({ children }: { children: ReactNode }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-hair bg-panel px-3 py-2">
      {children}
    </div>
  );
}
