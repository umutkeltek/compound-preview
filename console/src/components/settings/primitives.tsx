import type { ReactNode } from "react";

import { CopyCmd } from "@/components/CopyCmd";
import { DocIcon, FreshnessChip, IconTile, ShieldIcon, SlidersIcon, type Tone } from "@/components/kit";

/**
 * Settings shared primitives (redesign plan Part 3 "Settings — 7 groups").
 *
 * The whole area obeys one doctrine: the console shows EFFECTIVE AUTHORITY
 * (read-mostly). File-owned state is edited via import commands, not web
 * buttons; a mutation with no governed console path is shown as its exact
 * copy-command, never a fabricated act. Every group carries a FreshnessChip
 * naming the projection it read (§7.8 "every card declares provenance").
 *
 * Pure/token-only and node-free, so both server group components and the one
 * client disclosure can render them. Tokens only — no arbitrary values (§7).
 */

const SEAM_VAR: Record<string, string> = {
  seam: "var(--seam-fg)",
  crit: "var(--crit-fg)",
  good: "var(--good-fg)",
  work: "var(--work-fg)",
  memory: "var(--memory-fg)",
  route: "var(--route-fg)",
  capability: "var(--cap-fg)",
};

export type SeamTone = keyof typeof SEAM_VAR;

/**
 * Provenance chip (DESIGN.md §9.5) — every tunable is exactly one kind:
 *   file-owned — read-only file authority, edited via an import command;
 *   governed   — one-click through the decision registry (receipt + undo);
 *   local      — client-only view preference (localStorage, zero governance).
 * Only the governed kind spends a hue (amber — it is an act); file-owned and
 * local stay quiet, matching §7's "hue is reserved" rule.
 */
export type ProvenanceKind = "file-owned" | "governed" | "local";

const PROVENANCE: Record<ProvenanceKind, { label: string; cls: string; Icon: typeof DocIcon; gloss: string }> = {
  "file-owned": {
    label: "file-owned",
    cls: "border-hair bg-soft-2 text-muted",
    Icon: DocIcon,
    gloss: "File-owned authority — read-only here; edit at the source and re-import.",
  },
  governed: {
    label: "governed",
    cls: "border-seam-line bg-seam-soft text-seam",
    Icon: ShieldIcon,
    gloss: "Governed toggle — one human click through the decision registry, with a receipt and undo path.",
  },
  local: {
    label: "local",
    cls: "border-hair bg-soft text-faint",
    Icon: SlidersIcon,
    gloss: "View preference — client-only (localStorage); changes no server state.",
  },
};

export function ProvenanceChip({ kind, title }: { kind: ProvenanceKind; title?: string }) {
  const p = PROVENANCE[kind];
  return (
    <span
      title={title ?? p.gloss}
      className={`inline-flex items-center gap-1 rounded-pill border px-2 py-0.5 text-2xs font-semibold ${p.cls}`}
    >
      <p.Icon size={12} />
      {p.label}
    </span>
  );
}

/** A numbered, danger-ordered settings group led by an IconTile (§9.1/§9.5). */
export function SettingsSection({
  index,
  icon,
  tone = "neutral",
  title,
  question,
  source,
  generatedAt,
  children,
}: {
  index: number;
  icon?: ReactNode;
  tone?: Tone;
  title: string;
  question: string;
  source?: string;
  generatedAt?: string;
  children: ReactNode;
}) {
  return (
    <section className="flex flex-col gap-4" id={`group-${index}`}>
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-hair pb-2.5">
        <div className="flex items-center gap-2.5">
          {icon != null ? <IconTile tone={tone} size="md">{icon}</IconTile> : null}
          <span className="mono text-xs font-semibold tabular-nums text-faint">{String(index).padStart(2, "0")}</span>
          <div className="min-w-0">
            <h2 className="text-md font-semibold text-ink">{title}</h2>
            <p className="text-xs text-muted">{question}</p>
          </div>
        </div>
        {source ? <FreshnessChip generatedAt={generatedAt} source={source} /> : null}
      </div>
      {children}
    </section>
  );
}

/** Read-only authority card; optional left seam tone marks a domain/severity.
 *  §9.5 knob-row anatomy: optional leading IconTile + name + provenance chip. */
export function Card({
  title,
  icon,
  tone = "neutral",
  provenance,
  badge,
  seamTone,
  children,
}: {
  title?: ReactNode;
  icon?: ReactNode;
  tone?: Tone;
  provenance?: ProvenanceKind;
  badge?: ReactNode;
  seamTone?: SeamTone;
  children: ReactNode;
}) {
  const hasHeader = title || badge || icon || provenance;
  return (
    <div
      className="rounded-xl bg-panel p-4 shadow-sm"
      style={
        seamTone
          ? { borderLeftWidth: "3px", borderLeftStyle: "solid", borderLeftColor: SEAM_VAR[seamTone] }
          : undefined
      }
    >
      {hasHeader ? (
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <div className="flex min-w-0 items-center gap-2">
            {icon != null ? <IconTile tone={tone}>{icon}</IconTile> : null}
            {title ? <h3 className="text-sm font-semibold text-ink">{title}</h3> : null}
          </div>
          <div className="flex flex-wrap items-center gap-1.5">
            {provenance ? <ProvenanceChip kind={provenance} /> : null}
            {badge}
          </div>
        </div>
      ) : null}
      {children}
    </div>
  );
}

/** One mono/tabular key → value row inside a card. */
export function KV({ k, v, mono = true }: { k: ReactNode; v: ReactNode; mono?: boolean }) {
  return (
    <div className="flex items-baseline justify-between gap-4 border-b border-hair py-1.5 last:border-b-0">
      <span className="shrink-0 text-xs text-muted">{k}</span>
      <span className={`min-w-0 text-right text-xs tabular-nums text-ink ${mono ? "mono truncate" : ""}`}>{v}</span>
    </div>
  );
}

/**
 * The honest file-owned footer: this state is not editable from the console —
 * it is imported from its source file. Shows the exact command to copy.
 */
export function FileOwnedFoot({ cmd, note }: { cmd: string; note?: ReactNode }) {
  return (
    <div className="mt-3 flex flex-col gap-1.5 border-t border-hair pt-2.5">
      <div className="flex items-center gap-2">
        <ProvenanceChip kind="file-owned" />
        <span className="text-2xs leading-relaxed text-faint">
          {note ?? "The console shows effective authority; edit at the source, then re-import."}
        </span>
      </div>
      <CopyCmd cmd={cmd} />
    </div>
  );
}

/**
 * Honest "not wired" row: a projection that errored or needs args gets NO fake
 * card — it states the gap plainly and hands over the exact inspect command.
 */
export function InspectRow({ label, cmd, reason }: { label: string; cmd: string; reason?: ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed border-hair bg-page p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-sm text-muted">{label}</span>
        <span className="mono rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs font-semibold text-faint">
          not wired
        </span>
      </div>
      {reason ? <p className="mt-1 text-xs leading-relaxed text-faint">{reason}</p> : null}
      <div className="mt-2">
        <CopyCmd cmd={cmd} />
      </div>
    </div>
  );
}

/** Small constitution-voice note (§7.9 doctrine-as-copy). */
export function DoctrineNote({ children }: { children: ReactNode }) {
  return <p className="text-xs leading-relaxed text-muted">{children}</p>;
}

/** Count chip: mono/tabular, quiet by default, amber when it flags attention. */
export function CountChip({
  n,
  label,
  tone = "muted",
  title,
}: {
  n: number;
  label: string;
  tone?: "muted" | "seam" | "good" | "crit";
  title?: string;
}) {
  const text =
    tone === "seam" ? "text-seam" : tone === "good" ? "text-good" : tone === "crit" ? "text-crit" : "text-muted";
  return (
    <span
      title={title}
      className={`mono inline-flex items-center gap-1 rounded-pill border border-hair bg-soft px-2.5 py-0.5 text-2xs font-semibold tabular-nums ${text}`}
    >
      <span className="tabular-nums">{n.toLocaleString("en-US")}</span> {label}
    </span>
  );
}
