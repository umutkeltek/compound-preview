/**
 * DateTile (DESIGN.md §9.3) — a stacked mono day/month block, soft fill,
 * radius-sm. Leads event rows (receipts, scheduled loops, transitions). The
 * numerals are mono/tabular so a stacked column of dates aligns. Renders a
 * faint "··" placeholder rather than a fabricated date when the input is
 * unparseable (never invent a timestamp).
 */
const MONTHS = ["JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"];

export function DateTile({ date, className = "" }: { date: string | Date | null | undefined; className?: string }) {
  const d = date instanceof Date ? date : date ? new Date(date) : null;
  const valid = d != null && !Number.isNaN(d.getTime());
  const day = valid ? String(d.getDate()).padStart(2, "0") : "··";
  const month = valid ? MONTHS[d.getMonth()] : "";

  return (
    <span
      aria-hidden
      className={`mono inline-flex h-9 w-9 shrink-0 flex-col items-center justify-center rounded-sm bg-soft leading-none ${className}`}
    >
      <span className="text-sm font-bold tabular-nums text-ink">{day}</span>
      <span className="mt-0.5 text-2xs font-semibold tracking-wide text-faint">{month}</span>
    </span>
  );
}
