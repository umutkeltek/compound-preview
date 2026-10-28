import Link from "next/link";

/**
 * The one machine-id renderer (DESIGN.md §7.4 + redesign plan Part 3's
 * "no id is dead text" law). Mono + tabular, and NEVER truncated without a
 * `title` carrying the full value. By default a `work_…` id links to its
 * detail route (`/work/[id]`); on the Work screen the board cards open the
 * Trace drawer via the `?trace=<id>` deep link instead, so those call sites
 * pass `plain` (render the id as text, the card itself is the trace trigger).
 *
 * Other waves adopt this for their own id kinds (claims → Memory, route ids →
 * Routing, …) by extending `hrefFor`; work is the first consumer.
 */
export type IdRefKind = "work" | "receipt" | "transition" | "policy" | "action" | "candidate" | "plan" | "generic";

function inferKind(id: string): IdRefKind {
  if (/^work_/.test(id)) return "work";
  if (/^(rec|receipt)_/.test(id)) return "receipt";
  if (/^(tr|transition)_/.test(id)) return "transition";
  if (/^(pol|pd)_/.test(id)) return "policy";
  if (/^act_/.test(id)) return "action";
  if (/^cand_/.test(id)) return "candidate";
  if (/^(wcrp|dirprop)_/.test(id)) return "plan";
  return "generic";
}

/** The only kind with a live destination today; others render as titled text. */
function hrefFor(kind: IdRefKind, id: string): string | null {
  return kind === "work" ? `/work/${id}` : null;
}

export function IdRef({
  id,
  kind,
  plain = false,
  truncate = false,
  className = "",
}: {
  id: string;
  kind?: IdRefKind;
  /** Render as titled text even when a destination exists (the card is the trigger). */
  plain?: boolean;
  /** Allow CSS truncation — always paired with a full-value title. */
  truncate?: boolean;
  className?: string;
}) {
  if (!id) return null;
  const resolved = kind ?? inferKind(id);
  const href = plain ? null : hrefFor(resolved, id);
  const base = `mono text-2xs tabular-nums ${truncate ? "block truncate" : "break-all"} ${className}`.trim();

  if (href) {
    return (
      <Link href={href} title={id} className={`${base} text-muted underline-offset-2 hover:text-work hover:underline`}>
        {id}
      </Link>
    );
  }
  return (
    <span title={id} className={`${base} text-faint`}>
      {id}
    </span>
  );
}
