import { IdRef } from "@/components/kit";
import { STATUS_META, type SystemNode } from "@/lib/map/types";
import type { Accent } from "@/components/ui";

/**
 * One system in the substrate topology. Honest about status: live nodes show
 * their real volume; intake-only / planned nodes carry a dashed border so it's
 * clear what is wired today vs intended. Restyled onto the token system.
 */

/* domain/lane accent → token glyph-tile classes (accent only, never a fill). */
const TILE: Record<Accent, string> = {
  blue: "border-work-line bg-work-soft text-work",
  violet: "border-memory-line bg-memory-soft text-memory",
  green: "border-good-line bg-good-soft text-good",
  orange: "border-seam-line bg-seam-soft text-seam",
  red: "border-crit-line bg-crit-soft text-crit",
  pink: "border-product-line bg-product-soft text-product",
  gray: "border-hair bg-soft-2 text-muted",
};

/* status tone → token pill classes. */
const STATUS_PILL: Record<Accent, string> = {
  green: "border-good-line bg-good-soft text-good",
  orange: "border-seam-line bg-seam-soft text-seam",
  blue: "border-work-line bg-work-soft text-work",
  gray: "border-hair bg-soft-2 text-muted",
  violet: "border-memory-line bg-memory-soft text-memory",
  red: "border-crit-line bg-crit-soft text-crit",
  pink: "border-product-line bg-product-soft text-product",
};

export function SystemNodeCard({ node }: { node: SystemNode }) {
  const status = STATUS_META[node.status];
  return (
    <div
      className={`flex flex-col gap-2 rounded-lg bg-panel p-3.5 ${
        status.dashed ? "border border-dashed border-hair" : "border border-hair"
      }`}
    >
      <div className="flex items-start justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <span
            aria-hidden
            className={`inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-sm border text-xs font-bold ${TILE[node.accent]}`}
          >
            {node.glyph}
          </span>
          <div className="min-w-0">
            <div className="truncate text-sm font-semibold text-ink">{node.name}</div>
            <IdRef id={node.id} />
          </div>
        </div>
        <span
          title={node.status}
          className={`inline-flex shrink-0 items-center rounded-pill border px-2 py-0.5 text-2xs font-semibold ${STATUS_PILL[status.tone]}`}
        >
          {status.label}
        </span>
      </div>

      <p className="text-xs leading-snug text-muted">{node.description}</p>

      {node.volume ? (
        <div className="text-2xs font-bold uppercase tracking-wide text-faint">{node.volume}</div>
      ) : null}

      {node.evidenceRefs?.length ? (
        <details>
          <summary className="cursor-pointer text-2xs text-faint hover:text-muted">
            {node.status === "intake-only" || node.status === "reference-only"
              ? `review · ${node.evidenceRefs[0]}`
              : `${node.evidenceRefs.length} source${node.evidenceRefs.length === 1 ? "" : "s"}`}
          </summary>
          <ul className="mt-1 flex flex-col gap-0.5">
            {node.evidenceRefs.map((ref) => (
              <li key={ref} className="mono truncate text-2xs text-faint" title={ref}>
                {ref}
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </div>
  );
}
