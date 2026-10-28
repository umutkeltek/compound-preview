import type { ReactNode } from "react";

/**
 * Shared briefing chrome — one card grammar (DESIGN.md §3) and one section
 * label, tokens only, so every briefing panel reads as one system and the page
 * stays free of arbitrary utility values.
 */

/** DESIGN.md §3 card: hairline border, --radius-lg, --shadow-sm at rest. */
export function Panel({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <div className={`rounded-lg border border-hair bg-panel shadow-sm ${className}`}>{children}</div>;
}

/** Uppercase small-caps section label (11px/700, DESIGN.md §1). */
export function SectionLabel({ children, right }: { children: ReactNode; right?: ReactNode }) {
  return (
    <div className="mb-3 flex items-baseline justify-between gap-3">
      <span className="text-xs font-bold uppercase tracking-wide text-faint">{children}</span>
      {right ?? null}
    </div>
  );
}

/** Dashed doctrine empty state (DESIGN.md §3/§7.9 — never "No data"). */
export function EmptyDoctrine({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed border-hair bg-page px-4 py-6 text-center text-sm text-muted">
      {children}
    </div>
  );
}

/* ---------- domain accent (DESIGN.md §0 — "which subsystem?", accent only) ---------- */

export type DomainTone = "work" | "memory" | "capability" | "route" | "product";

/** Classify a decision area onto its domain hue. Accent only — a dot + label,
 * never a fill (DESIGN.md §0/§7.2). Deterministic keyword match. */
export function domainForArea(area: string): DomainTone {
  const a = area.toLowerCase();
  if (a.includes("memory") || a.includes("synthesis") || a.includes("contradiction") || a.includes("candidate")) return "memory";
  if (a.includes("capability") || a.includes("source") || a.includes("intake")) return "capability";
  if (a.includes("route") || a.includes("promotion") || a.includes("panel") || a.includes("bench") || a.includes("parity")) return "route";
  if (a.includes("product") || a.includes("loop")) return "product";
  return "work";
}

const DOMAIN_DOT: Record<DomainTone, string> = {
  work: "bg-work",
  memory: "bg-memory",
  capability: "bg-capability",
  route: "bg-route",
  product: "bg-product",
};

const DOMAIN_TEXT: Record<DomainTone, string> = {
  work: "text-work",
  memory: "text-memory",
  capability: "text-capability",
  route: "text-route",
  product: "text-product",
};

/** Domain kicker: dot + uppercase area name (accent only). */
export function DomainKicker({ area }: { area: string }) {
  if (!area) return null;
  const tone = domainForArea(area);
  return (
    <span className={`inline-flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide ${DOMAIN_TEXT[tone]}`}>
      <span className={`h-1.5 w-1.5 rounded-full ${DOMAIN_DOT[tone]}`} aria-hidden />
      {area.replace(/-/g, " ")}
    </span>
  );
}
