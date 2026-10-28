import type { ReactNode } from "react";

import { Panel } from "@/components/briefing/primitives";
import { DeltaPill } from "@/components/kit";
import { memoryBriefingNotice, type AutonomyPosture, type MemoryBriefing } from "@/lib/briefing";

/**
 * The KPI strip (redesign plan Part 3 / DESIGN.md §3 KPI stat): World Δ · Mind Δ
 * · Autonomy · Spend. Each tile is label (domain-tinted dot) → value (26px/800
 * tabular) → sub → optional visual. World Δ + Mind Δ carry a thin stacked bar
 * of their composition; Autonomy reads real operator authority; Spend is an
 * honest doctrine placeholder — no cheap projection exists, so it invents no
 * number (see page TODO).
 */

type StackSeg = { value: number; className: string; label: string };

function StackBar({ segments }: { segments: StackSeg[] }) {
  const total = segments.reduce((sum, s) => sum + s.value, 0);
  if (total <= 0) return null;
  return (
    <span className="mt-3 flex h-1.5 w-full overflow-hidden rounded-pill bg-hair" role="img" aria-label={segments.map((s) => `${s.label} ${s.value}`).join(", ")}>
      {segments.map((seg) =>
        seg.value > 0 ? (
          <span key={seg.label} title={`${seg.label}: ${seg.value}`} className={`block h-full ${seg.className}`} style={{ width: `${(seg.value / total) * 100}%` }} />
        ) : null,
      )}
    </span>
  );
}

function KpiTile({
  label,
  dot,
  value,
  delta,
  sub,
  children,
}: {
  label: string;
  dot?: string;
  value: ReactNode;
  delta?: ReactNode;
  sub: ReactNode;
  children?: ReactNode;
}) {
  return (
    <Panel className="flex flex-col px-5 py-4">
      <span className="flex items-center gap-1.5 text-xs font-bold uppercase tracking-wide text-faint">
        {dot ? <span className={`h-1.5 w-1.5 rounded-full ${dot}`} aria-hidden /> : null}
        {label}
      </span>
      <span className="mt-2 flex items-baseline gap-2">
        <span className="text-2xl font-extrabold tracking-tight tabular-nums text-strong">{value}</span>
        {delta ?? null}
      </span>
      <span className="mt-1 text-xs leading-snug text-muted">{sub}</span>
      {children}
    </Panel>
  );
}

export function KpiStrip({
  world,
  mind,
  autonomy,
  worldDelta,
}: {
  world: { outward: number; meaning: number; rules: number; upkeep: number; total: number; lowOutward: boolean };
  mind: MemoryBriefing;
  autonomy: AutonomyPosture;
  /** week-over-week change in outward moves — null when the window can't cover the prior week (§9.6). */
  worldDelta?: number | null;
}) {
  return (
    <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
      <KpiTile
        label="World Δ · 7d"
        dot="bg-work"
        value={world.outward.toLocaleString("en-US")}
        delta={
          worldDelta != null ? (
            <DeltaPill value={worldDelta} showZero title="outward moves vs previous 7 days" />
          ) : null
        }
        sub={
          world.lowOutward && world.total > 0 ? (
            <span className="text-seam">outward moves — thin week</span>
          ) : (
            <>outward moves · {world.total.toLocaleString("en-US")} total</>
          )
        }
      >
        <StackBar
          segments={[
            { value: world.outward, className: "bg-good", label: "Outward" },
            { value: world.meaning, className: "bg-memory", label: "Meaning" },
            { value: world.rules, className: "bg-route", label: "Rules" },
            { value: world.upkeep, className: "bg-faint", label: "Upkeep" },
          ]}
        />
      </KpiTile>

      <KpiTile
        label="Mind Δ · 7d"
        dot="bg-memory"
        value={mind.promotedThisWeek == null ? "—" : `${mind.promotedThisWeek > 0 ? "+" : ""}${mind.promotedThisWeek.toLocaleString("en-US")}`}
        delta={mind.delta != null ? <DeltaPill value={mind.delta} showZero title="promotions vs previous 7 days" /> : null}
        sub={mind.totals ? <>promoted · {mind.totals.awaiting.toLocaleString("en-US")} awaiting · {mind.totals.parked.toLocaleString("en-US")} parked</> : memoryBriefingNotice(mind)}
      >
        {mind.totals ? <StackBar segments={[
          { value: mind.totals.promoted, className: "bg-good", label: "Promoted" },
          { value: mind.totals.awaiting, className: "bg-memory", label: "Awaiting" },
          { value: mind.totals.parked, className: "bg-faint", label: "Parked" },
        ]} /> : null}
        {mind.observation === "stale" && mind.totals ? (
          <span className="mt-2 text-xs text-seam">
            {memoryBriefingNotice(mind)}
            {mind.lastGoodAt ? <span className="block">Last successful read: <time dateTime={mind.lastGoodAt}>{mind.lastGoodAt}</time></span> : null}
          </span>
        ) : null}
      </KpiTile>
      <KpiTile
        label="Autonomy"
        dot={autonomy.tone === "good" ? "bg-good" : "bg-seam"}
        value={autonomy.level}
        sub={<span title={autonomy.detail}>{autonomy.posture}</span>}
      >
        <span className="mt-3 text-2xs leading-snug text-faint">{autonomy.detail}</span>
      </KpiTile>

      <KpiTile label="Spend · today" value={<span className="text-faint">—</span>} sub="No spend projection wired yet">
        <span className="mt-3 text-2xs leading-snug text-faint">
          Cost lives in a provider ledger, not a cheap ambient read. Wire when a projection exists.
        </span>
      </KpiTile>
    </div>
  );
}
