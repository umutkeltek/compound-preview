import type { ReactNode } from "react";

/* ---------- accents ---------- */

export type Accent = "blue" | "green" | "orange" | "pink" | "violet" | "red" | "gray";

const ACCENT: Record<Accent, string> = {
  blue: "var(--color-accent-blue)",
  green: "var(--color-accent-green)",
  orange: "var(--color-accent-orange)",
  pink: "var(--color-accent-pink)",
  violet: "var(--color-accent-violet)",
  red: "var(--color-accent-red)",
  gray: "#9aa4b2",
};

/** Resolve an accent to its CSS color value (used for inline dots/marks). */
export function accentColor(accent: Accent): string {
  return ACCENT[accent];
}

export function AccentSquare({ accent, children }: { accent: Accent; children?: ReactNode }) {
  return (
    <span
      className="inline-flex h-6 w-6 items-center justify-center rounded-[8px] text-[11px] font-bold text-white"
      style={{ background: ACCENT[accent] }}
    >
      {children}
    </span>
  );
}

/* ---------- chips ---------- */

export function Chip({
  tone = "gray",
  children,
  title,
}: {
  tone?: Accent;
  children: ReactNode;
  title?: string;
}) {
  const bg: Record<Accent, string> = {
    blue: "bg-blue-50 text-blue-700",
    green: "bg-emerald-50 text-emerald-700",
    orange: "bg-amber-50 text-amber-700",
    pink: "bg-pink-50 text-pink-700",
    violet: "bg-violet-50 text-violet-700",
    red: "bg-red-50 text-red-600",
    gray: "bg-slate-100 text-slate-600",
  };
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11.5px] font-medium ${bg[tone]}`}
    >
      {children}
    </span>
  );
}

/** Every card declares where its data comes from and how fresh it is. */
export function FreshnessChip({ generatedAt, source }: { generatedAt?: string; source?: string }) {
  const t = generatedAt ? new Date(generatedAt) : null;
  const label = t && !Number.isNaN(t.getTime())
    ? t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })
    : "unknown";
  return (
    <Chip tone={t ? "green" : "gray"} title={source ? `source: ${source}` : undefined}>
      <span className="h-1.5 w-1.5 rounded-full" style={{ background: t ? "#10b981" : "#9aa4b2" }} />
      live · {label}
    </Chip>
  );
}

const STATE_TONE: Record<string, { tone: Accent; label: string }> = {
  proposed: { tone: "orange", label: "draft" },
  admitted: { tone: "orange", label: "queued" },
  ready: { tone: "green", label: "ready" },
  claimed: { tone: "violet", label: "assigned" },
  in_progress: { tone: "blue", label: "in progress" },
  running: { tone: "blue", label: "running" },
  verifying: { tone: "blue", label: "verifying" },
  needs_review: { tone: "violet", label: "needs review" },
  held: { tone: "violet", label: "needs me" },
  blocked: { tone: "orange", label: "blocked" },
  completed: { tone: "green", label: "completed" },
  failed: { tone: "red", label: "failed" },
  cancelled: { tone: "gray", label: "cancelled" },
};

export function StateBadge({ state }: { state: string }) {
  const m = STATE_TONE[state] ?? { tone: "gray" as Accent, label: state || "unknown" };
  return <Chip tone={m.tone}>{m.label}</Chip>;
}

/* ---------- card + layout primitives ---------- */

export function Card({
  title,
  accent = "blue",
  glyph,
  right,
  children,
  className = "",
}: {
  title?: string;
  accent?: Accent;
  glyph?: string;
  right?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div className={`card p-5 ${className}`}>
      {(title || right) && (
        <div className="mb-4 flex items-center justify-between gap-3">
          <div className="flex items-center gap-2.5">
            {glyph ? <AccentSquare accent={accent}>{glyph}</AccentSquare> : null}
            {title ? <h2 className="text-[13.5px] font-semibold text-ink">{title}</h2> : null}
          </div>
          {right}
        </div>
      )}
      {children}
    </div>
  );
}

export function Stat({
  label,
  value,
  sub,
  accent = "blue",
  glyph,
}: {
  label: string;
  value: ReactNode;
  sub?: ReactNode;
  accent?: Accent;
  glyph?: string;
}) {
  return (
    <div className="card flex flex-col gap-1.5 p-5">
      <div className="flex items-center gap-2">
        {glyph ? <AccentSquare accent={accent}>{glyph}</AccentSquare> : null}
        <span className="text-[12.5px] font-medium text-ink-2">{label}</span>
      </div>
      <div className="text-[28px] font-semibold leading-none tracking-tight">{value}</div>
      {sub ? <div className="text-[12px] text-ink-3">{sub}</div> : null}
    </div>
  );
}

export function EmptyState({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-xl border border-dashed border-line bg-canvas/60 px-4 py-6 text-center text-[12.5px] text-ink-3">
      {children}
    </div>
  );
}
