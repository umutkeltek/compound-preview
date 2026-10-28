import { arr, str, type Json } from "@/lib/compound";

/** Shorten prose to ~n chars with an ellipsis. */
export function shortText(value: unknown, n = 180): string {
  const s = str(value).trim().replace(/\s+/g, " ");
  if (s.length <= n) return s;
  return `${s.slice(0, n - 1).trimEnd()}…`;
}

/**
 * De-duplicate a review/work item's boundary lines. Boundaries arrive both as
 * an array (`boundaries[]`) and as a comma-joined string (`not_to_do`); collapse
 * overlaps so the card shows distinct limits, not ten near-identical chips.
 */
export function uniqueBoundaries(item: Json, limit = 6): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  const raw = [...arr(item.boundaries).map(String), str(item.not_to_do)];
  for (const block of raw) {
    for (const part of block.split(/[,;]/)) {
      const p = part.trim();
      const key = p.toLowerCase();
      if (p.length < 3 || seen.has(key)) continue;
      seen.add(key);
      out.push(p);
      if (out.length >= limit) return out;
    }
  }
  return out;
}

/** A readable target label — repo slug, path, or id, stripped of protocol noise. */
export function targetLabel(item: Json): string {
  const raw = str(item.target_path) || str(item.source_path) || str(item.id);
  return raw.replace(/^https?:\/\//, "").replace(/^www\./, "");
}
