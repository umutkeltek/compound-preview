"use client";

import Link from "next/link";
import { useState, type ReactNode } from "react";

import { AvatarTile, StateBadge, toneForState, type StateTone } from "@/components/kit";
import { ago } from "@/lib/time";

import { durationLabel, truncate, type ExecutorRow, type ExecutorWorkRef } from "./executor-model";

/**
 * The executor table — "compare → table" doctrine (DESIGN.md §4): one row per
 * executor from `compound executors --json`, homogeneous columns scanned
 * top-to-bottom. NOT cards (cards are for per-item judgment). Each row opens a
 * progressive-disclosure panel (can_use_tools, cost_limits, last hook, current
 * work) so the dense table stays scannable while the full record is one click
 * away — the buried-`<details>` anti-pattern fixed with an accessible toggle.
 */

const TONE_DOT: Record<StateTone, string> = {
  draft: "bg-faint",
  queued: "bg-muted",
  live: "bg-work",
  seam: "bg-seam",
  done: "bg-good",
  failed: "bg-crit",
  dropped: "bg-faint",
};

const COLUMN_COUNT = 9;

function StatusDot({ status }: { status: string }) {
  const tone = toneForState(status);
  if (tone === "live") {
    return (
      <span className="inline-flex text-work" aria-hidden>
        <span className="live-dot" />
      </span>
    );
  }
  return <span className={`h-2 w-2 shrink-0 rounded-full ${TONE_DOT[tone]}`} aria-hidden />;
}

function Chips({ items, max = 3 }: { items: string[]; max?: number }) {
  if (!items.length) return null;
  const shown = items.slice(0, max);
  const overflow = items.length - shown.length;
  return (
    <div className="flex flex-wrap items-center gap-1">
      {shown.map((chip) => (
        <span
          key={chip}
          className="rounded-pill border border-hair bg-soft-2 px-2 py-0.5 text-2xs font-medium text-muted"
        >
          {chip}
        </span>
      ))}
      {overflow > 0 ? (
        <span className="text-2xs text-faint" title={items.join(", ")}>
          +{overflow}
        </span>
      ) : null}
    </div>
  );
}

// adopt kit IdRef when available — the Work lane will export <IdRef kind id/>
// (Trace-drawer aware). Until it lands, a local mono link to the work route.
function WorkIdLink({ workId }: { workId: string }) {
  if (!workId) return <span className="text-faint">—</span>;
  return (
    <Link href={`/work/${workId}`} title={workId} className="mono text-2xs text-work hover:underline">
      {workId}
    </Link>
  );
}

function CountCell({ value, seam = false }: { value: number; seam?: boolean }) {
  const emphasise = seam && value > 0;
  return (
    <td className="px-2 py-3 text-right align-top">
      <span
        className={`mono text-sm tabular-nums ${
          emphasise ? "font-semibold text-seam" : value > 0 ? "text-ink" : "text-faint"
        }`}
      >
        {value}
      </span>
    </td>
  );
}

function DetailField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <div className="text-2xs font-bold uppercase tracking-wide text-faint">{label}</div>
      <div className="mt-1 text-sm leading-relaxed text-muted">{children}</div>
    </div>
  );
}

function WorkLine({ work }: { work: ExecutorWorkRef }) {
  return (
    <li className="rounded-md border border-hair px-3 py-2">
      <div className="flex items-center justify-between gap-2">
        <WorkIdLink workId={work.workId} />
        {work.state ? <StateBadge state={work.state} /> : null}
      </div>
      {work.title ? <p className="mt-1 text-sm leading-relaxed text-muted">{truncate(work.title, 150)}</p> : null}
      {work.nextAction ? (
        <p className="mt-1 text-2xs leading-relaxed text-faint">Next: {truncate(work.nextAction, 160)}</p>
      ) : null}
    </li>
  );
}

function ExpansionRow({ executor }: { executor: ExecutorRow }) {
  const routeEvidenceVisible = executor.routeEvidencePresent || executor.routeEvidenceCount > 0;
  return (
    <tr className="border-b border-hair bg-soft">
      <td colSpan={COLUMN_COUNT} className="px-4 py-4">
        <div className="grid gap-4 md:grid-cols-2">
          {executor.canUseTools.length ? (
            <DetailField label="Can use tools">{executor.canUseTools.join(", ")}</DetailField>
          ) : null}
          {executor.costLimits.length ? (
            <DetailField label="Cost / limits">{executor.costLimits.join(", ")}</DetailField>
          ) : null}
          {executor.lastHook ? (
            <DetailField label="Last hook event">
              <span className="mono text-2xs">
                {executor.lastHook.hook || "hook"} · {executor.lastHook.status || "unknown"}
                {ago(executor.lastHook.createdAt) ? ` · ${ago(executor.lastHook.createdAt)}` : ""}
              </span>
            </DetailField>
          ) : null}
          {executor.waitReason ? <DetailField label="Wait reason">{executor.waitReason}</DetailField> : null}
          {executor.statusSource ? (
            <DetailField label="Status source">
              <span className="mono text-2xs">{executor.statusSource}</span>
            </DetailField>
          ) : null}
          {routeEvidenceVisible ? (
            <DetailField label="Route evidence">
              <span className="mono text-2xs">
                {executor.routeEvidenceCount} {executor.routeEvidenceCount === 1 ? "row" : "rows"}
              </span>{" "}
              <span className="text-faint">— evidence, not task-success proof</span>
            </DetailField>
          ) : null}
          {executor.bestFor.length > 3 ? (
            <DetailField label="Best for (all)">{executor.bestFor.join(", ")}</DetailField>
          ) : null}
        </div>

        {executor.projectionNote ? (
          <p className="mt-3 rounded-md border border-hair bg-panel px-3 py-2 text-2xs leading-relaxed text-muted">
            {executor.projectionNote}
          </p>
        ) : null}

        {executor.currentWork.length ? (
          <div className="mt-4">
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">Current work</div>
            <ul className="mt-2 flex flex-col gap-2">
              {executor.currentWork.slice(0, 6).map((work, index) => (
                <WorkLine key={work.workId || `${executor.id}-${index}`} work={work} />
              ))}
            </ul>
          </div>
        ) : null}

        {executor.nextAssigned ? (
          <p className="mt-3 text-2xs leading-relaxed text-faint">
            Next assigned: <WorkIdLink workId={executor.nextAssigned.workId} />{" "}
            {truncate(executor.nextAssigned.title, 120)}
          </p>
        ) : null}
      </td>
    </tr>
  );
}

function ExecutorTableRow({ executor, staleAfterSeconds }: { executor: ExecutorRow; staleAfterSeconds: number }) {
  const [open, setOpen] = useState(false);
  const panelId = `executor-detail-${executor.id || "row"}`;
  const leaseStale = executor.oldestLeaseAgeSeconds !== null && executor.oldestLeaseAgeSeconds > staleAfterSeconds;
  const firstWork = executor.currentWork[0];
  const overflowWork = executor.currentWork.length - (firstWork ? 1 : 0);

  return (
    <>
      <tr className="border-b border-hair transition-colors hover:bg-soft">
        <td className="px-2 py-3 align-top">
          <button
            type="button"
            aria-expanded={open}
            aria-controls={panelId}
            onClick={() => setOpen((prev) => !prev)}
            title={open ? "Hide detail" : "Show can-use-tools, cost limits, last hook"}
            className="mono rounded-md border border-hair px-1.5 py-0.5 text-2xs font-semibold text-muted hover:bg-soft-2"
          >
            {open ? "−" : "+"}
          </button>
        </td>

        <td className="px-2 py-3 align-top">
          <div className="flex items-start gap-2.5">
            {/* Deterministic identity tint — survives renames (§9.3). */}
            <AvatarTile name={executor.label} size="md" className="mt-0.5" />
            <div className="min-w-0">
              <div className="flex items-center gap-1.5">
                <StatusDot status={executor.status} />
                <span className="text-md font-semibold text-ink">{executor.label}</span>
              </div>
              {executor.id ? <div className="mono mt-0.5 text-2xs text-faint">{executor.id}</div> : null}
              <div className="mt-1.5">
                <Chips items={executor.bestFor} />
              </div>
            </div>
          </div>
        </td>

        <td className="px-2 py-3 align-top">
          <StateBadge state={executor.status} />
        </td>

        <CountCell value={executor.running} />
        <CountCell value={executor.ready} />
        <CountCell value={executor.assigned} />
        <CountCell value={executor.needsMe} seam />

        <td className="px-2 py-3 align-top">
          {firstWork ? (
            <div className="flex flex-col gap-0.5">
              <WorkIdLink workId={firstWork.workId} />
              {overflowWork > 0 ? <span className="text-2xs text-faint">+{overflowWork} more</span> : null}
            </div>
          ) : (
            <span className="text-2xs text-faint">—</span>
          )}
        </td>

        <td className="px-2 py-3 text-right align-top">
          <span className={`mono text-2xs tabular-nums ${leaseStale ? "font-semibold text-seam" : "text-muted"}`}>
            {executor.oldestLeaseAgeSeconds !== null ? durationLabel(executor.oldestLeaseAgeSeconds) : "—"}
          </span>
          {executor.leaseCount > 0 ? (
            <div className="mono text-2xs text-faint">
              {executor.leaseCount} {executor.leaseCount === 1 ? "lease" : "leases"}
            </div>
          ) : null}
        </td>
      </tr>
      {open ? (
        <tr id={panelId}>
          <td colSpan={COLUMN_COUNT} className="p-0">
            <table className="w-full">
              <tbody>
                <ExpansionRow executor={executor} />
              </tbody>
            </table>
          </td>
        </tr>
      ) : null}
    </>
  );
}

export function ExecutorTable({
  executors,
  staleAfterSeconds,
}: {
  executors: ExecutorRow[];
  staleAfterSeconds: number;
}) {
  return (
    <div className="overflow-x-auto rounded-lg border border-hair bg-panel shadow-sm">
      <table className="w-full border-collapse text-left" style={{ minWidth: "var(--read-max)" }}>
        <thead>
          <tr className="border-b border-hair">
            <th className="px-2 py-2.5" aria-label="expand" />
            <th className="px-2 py-2.5 text-2xs font-bold uppercase tracking-wide text-faint">Executor</th>
            <th className="px-2 py-2.5 text-2xs font-bold uppercase tracking-wide text-faint">Status</th>
            <th className="px-2 py-2.5 text-right text-2xs font-bold uppercase tracking-wide text-faint">Run</th>
            <th className="px-2 py-2.5 text-right text-2xs font-bold uppercase tracking-wide text-faint">Ready</th>
            <th className="px-2 py-2.5 text-right text-2xs font-bold uppercase tracking-wide text-faint">Assg</th>
            <th className="px-2 py-2.5 text-right text-2xs font-bold uppercase tracking-wide text-faint">Paused</th>
            <th className="px-2 py-2.5 text-2xs font-bold uppercase tracking-wide text-faint">Current work</th>
            <th className="px-2 py-2.5 text-right text-2xs font-bold uppercase tracking-wide text-faint">Lease age</th>
          </tr>
        </thead>
        <tbody>
          {executors.map((executor) => (
            <ExecutorTableRow
              key={executor.id || executor.label}
              executor={executor}
              staleAfterSeconds={staleAfterSeconds}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}
