/**
 * Pure, client-safe time helpers. Extracted from `@/lib/compound` (which pulls
 * in node:child_process/fs and therefore cannot be imported by a client
 * component) so the kit — FreshnessChip et al. — can render relative times in
 * both server and client bundles. `@/lib/compound` re-exports these so every
 * existing `import { ago, toDate } from "@/lib/compound"` keeps working.
 */

function fromEpoch(n: number): Date {
  if (n > 1e16) return new Date(n / 1e6); // ns
  if (n > 1e13) return new Date(n / 1e3); // us
  if (n > 1e11) return new Date(n); // ms
  return new Date(n * 1000); // s
}

/** Timestamps arrive as ISO strings, or ms/us/ns since epoch. */
export function toDate(value: unknown): Date | null {
  if (typeof value === "string") {
    if (/^\d+$/.test(value)) return fromEpoch(Number(value));
    const d = new Date(value);
    return Number.isNaN(d.getTime()) ? null : d;
  }
  if (typeof value === "number") return fromEpoch(value);
  return null;
}

export function ago(value: unknown): string {
  const d = toDate(value);
  if (!d) return "";
  const seconds = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (seconds < 90) return `${Math.round(seconds)}s ago`;
  if (seconds < 90 * 60) return `${Math.round(seconds / 60)}m ago`;
  if (seconds < 36 * 3600) return `${Math.round(seconds / 3600)}h ago`;
  return `${Math.round(seconds / 86400)}d ago`;
}
