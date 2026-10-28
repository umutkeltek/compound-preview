import { ago } from "@/lib/time";

/**
 * Every card declares provenance (DESIGN.md §3/§7.8): dot + mono label,
 * `title` = source command. Fresh (<TTL): green "live · HH:MM:SS". Stale:
 * amber "stale · 4m". Unknown: faint. Adapted from src/components/ui.tsx's
 * FreshnessChip, which only had fresh/unknown — this adds the stale tier.
 *
 * Client-safe: `ago` comes from the pure `@/lib/time` module (never
 * `@/lib/compound`, which pulls node builtins) so the kit barrel stays
 * usable from client components like the Trace drawer.
 */
export type FreshnessStatus = "fresh" | "stale" | "unknown";

function statusFor(generatedAt: string | undefined, staleAfterMs: number): FreshnessStatus {
  if (!generatedAt) return "unknown";
  const t = new Date(generatedAt);
  if (Number.isNaN(t.getTime())) return "unknown";
  return Date.now() - t.getTime() > staleAfterMs ? "stale" : "fresh";
}

function formatFresh(generatedAt: string): string {
  const t = new Date(generatedAt);
  return t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

/** "4m ago" -> "4m" (matches DESIGN.md's literal example "stale · 4m"). */
function formatStale(generatedAt: string): string {
  return ago(generatedAt).replace(/\s*ago$/, "");
}

const DOT_TONE: Record<FreshnessStatus, string> = {
  fresh: "bg-good",
  stale: "bg-seam",
  unknown: "bg-faint",
};

const TEXT_TONE: Record<FreshnessStatus, string> = {
  fresh: "text-good",
  stale: "text-seam",
  unknown: "text-faint",
};

export function FreshnessChip({
  generatedAt,
  source,
  staleAfterMs = 60_000,
}: {
  generatedAt?: string;
  source?: string;
  staleAfterMs?: number;
}) {
  const status = statusFor(generatedAt, staleAfterMs);
  const label =
    status === "fresh"
      ? `live · ${formatFresh(generatedAt!)}`
      : status === "stale"
        ? `stale · ${formatStale(generatedAt!)}`
        : "unknown";

  return (
    <span
      title={source ? `source: ${source}` : undefined}
      className={`mono inline-flex items-center gap-1.5 rounded-pill border border-hair bg-soft px-2.5 py-0.5 text-2xs font-semibold tabular-nums ${TEXT_TONE[status]}`}
    >
      <span className={`h-1.5 w-1.5 rounded-full ${DOT_TONE[status]}`} aria-hidden />
      {label}
    </span>
  );
}
