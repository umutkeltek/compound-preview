"use client";

import { useEffect, useMemo, useState } from "react";

import { EmptyState, IdRef, StateBadge } from "@/components/kit";
import type { PipelineStageModel } from "@/lib/work-pipeline";
import type { WorkLensGroup, WorkLensItem } from "@/lib/work-views";

const MAX_VISIBLE_PER_LANE = 3;

function workPriority(item: WorkLensItem): number {
  const state = item.runStatus || item.state;
  if (state === "running") return 0;
  if (state === "verifying") return 1;
  if (["blocked", "held"].includes(item.state)) return 2;
  if (item.state === "claimed") return 3;
  if (item.state === "ready") return 4;
  return 5;
}

function visibleItems(group: WorkLensGroup): WorkLensItem[] {
  return group.items.slice().sort((a, b) => workPriority(a) - workPriority(b)).slice(0, MAX_VISIBLE_PER_LANE);
}

function stageIndexFor(item: WorkLensItem): number {
  return item.progress.active ?? item.progress.done ?? 0;
}

function laneTone(group: WorkLensGroup): string {
  if (group.status === "live" || group.status === "available") return "text-good";
  if (group.status === "blocked" || group.status === "waiting") return "text-seam";
  return "text-work";
}

export function WorkSpaceLens({ stages, groups, onOpenTrace }: { stages: PipelineStageModel[]; groups: WorkLensGroup[]; onOpenTrace: (workId: string) => void }) {
  const visibleGroups = useMemo(() => groups.map((group) => ({ ...group, items: visibleItems(group) })), [groups]);
  const all = useMemo(() => visibleGroups.flatMap((group) => group.items), [visibleGroups]);
  const total = useMemo(() => groups.reduce((count, group) => count + group.items.length, 0), [groups]);
  const [selectedId, setSelectedId] = useState(all[0]?.workId ?? "");
  const selected = all.find((item) => item.workId === selectedId) ?? all[0] ?? null;

  useEffect(() => {
    if (!selectedId && all[0]) setSelectedId(all[0].workId);
  }, [all, selectedId]);

  if (!groups.length) return <EmptyState>No open work is available for the spatial view.</EmptyState>;

  return (
    <section className="live-canvas min-w-0 overflow-hidden rounded-xl border p-4">
      <div className="mb-4 flex flex-wrap items-start justify-between gap-3 text-[var(--canvas-ink)]">
        <div>
          <div className="text-sm font-semibold">Spatial workfield</div>
          <p className="mt-0.5 text-xs text-[var(--canvas-muted)]">Harness rooms show ownership; horizontal position shows each WorkEnvelope&apos;s current lifecycle stage.</p>
        </div>
        <div className="mono text-2xs text-[var(--canvas-muted)]">showing {all.length} of {total} · {groups.length} harness rooms</div>
      </div>

      <div className="overflow-x-auto pb-1">
        <div className="grid min-w-[1180px] grid-cols-[minmax(0,1fr)_272px] gap-3">
          <div className="overflow-hidden rounded-lg border border-[var(--canvas-border)] bg-[var(--canvas-panel)]" style={{ perspective: "1400px" }}>
            <div className="grid border-b border-[var(--canvas-border)] bg-[var(--canvas-panel)]" style={{ gridTemplateColumns: "142px repeat(7,minmax(105px,1fr))" }}>
              <div className="px-3 py-3 text-2xs font-bold uppercase tracking-wide text-[var(--canvas-muted)]">Harness rooms</div>
              {stages.map((stage) => (
                <div key={stage.key} className="border-l border-[var(--canvas-border)] px-2.5 py-3">
                  <div className="text-xs font-semibold text-[var(--canvas-ink)]">{stage.label}</div>
                  <div className="mt-0.5 text-2xs text-[var(--canvas-muted)]">{stage.count} now</div>
                </div>
              ))}
            </div>

            <div className="relative" style={{ transform: "rotateX(1.25deg)", transformOrigin: "top center" }}>
              {visibleGroups.map((group) => (
                <HarnessRoom key={group.id} group={group} stages={stages} selectedId={selected?.workId ?? ""} onSelect={setSelectedId} />
              ))}
            </div>
          </div>

          <aside className="sticky right-0 z-40 self-start rounded-lg border border-[var(--canvas-border)] bg-[var(--canvas-panel)] p-3 text-[var(--canvas-ink)] shadow-lg">
            <div className="flex items-center justify-between gap-3">
              <div className="text-2xs font-bold uppercase tracking-wide text-[var(--canvas-muted)]">Selected work</div>
              {selected ? <span className="inline-flex items-center gap-1 text-2xs text-good"><span className="h-1.5 w-1.5 rounded-full bg-current" />projected</span> : null}
            </div>
            {selected ? (
              <div className="mt-3 grid gap-4">
                <div>
                  <h3 className="line-clamp-5 text-base font-semibold leading-snug" title={selected.title}>{selected.title}</h3>
                  <div className="mt-2 flex flex-wrap items-center gap-2"><StateBadge state={selected.runStatus || selected.state} /><span className="mono text-2xs text-[var(--canvas-muted)]">{selected.ageLabel}</span></div>
                </div>
                <div>
                  <div className="text-2xs font-bold uppercase tracking-wide text-[var(--canvas-muted)]">Harness ownership</div>
                  <p className="mt-1 text-sm font-medium">{selected.harnessLabel}</p>
                  <p className="mt-0.5 text-xs text-[var(--canvas-muted)]">{selected.laneLabel}{selected.actor ? ` · ${selected.actor}` : ""}</p>
                </div>
                <div>
                  <div className="text-2xs font-bold uppercase tracking-wide text-[var(--canvas-muted)]">Current position</div>
                  <p className="mt-1 text-sm font-medium">{stages[stageIndexFor(selected)]?.label ?? "Unknown stage"}</p>
                  <p className="mt-0.5 text-xs leading-relaxed text-[var(--canvas-muted)]">Only the current state is drawn. Compound has not projected historical dependencies for this item.</p>
                </div>
                <div>
                  <div className="text-2xs font-bold uppercase tracking-wide text-[var(--canvas-muted)]">What moves next</div>
                  <p className="mt-1 text-xs leading-relaxed text-[var(--canvas-muted)]">{selected.nextAction || "No next action projected."}</p>
                </div>
                <IdRef id={selected.workId} kind="work" plain />
                <button type="button" onClick={() => onOpenTrace(selected.workId)} className="rounded-md border border-work-line bg-work-soft px-3 py-2 text-xs font-semibold text-work transition-colors hover:bg-work-soft/70">Open full trace →</button>
              </div>
            ) : <EmptyState>No work selected.</EmptyState>}
          </aside>
        </div>
      </div>

      <div className="mt-4 flex flex-wrap gap-x-5 gap-y-2 text-2xs text-[var(--canvas-muted)]">
        <span><span className="text-good">●</span> available harness</span>
        <span><span className="text-work">■</span> selected work</span>
        <span><span className="text-seam">■</span> waiting or blocked</span>
        <span>No dependency is drawn unless Compound projects one.</span>
        <span>Use Table for all {total} open items.</span>
      </div>
    </section>
  );
}

function HarnessRoom({ group, stages, selectedId, onSelect }: { group: WorkLensGroup; stages: PipelineStageModel[]; selectedId: string; onSelect: (workId: string) => void }) {
  return (
    <div className="relative min-h-32 border-b border-[var(--canvas-border)] last:border-b-0">
      <div className="absolute inset-y-0 left-0 z-20 w-[142px] border-r border-[var(--canvas-border)] bg-[var(--canvas-panel)] px-3 py-4">
        <div className="flex items-center gap-2">
          <span className={`grid h-7 w-7 place-items-center rounded-md border border-current/30 bg-black/10 text-2xs font-bold ${laneTone(group)}`}>{group.label.slice(0, 2).toUpperCase()}</span>
          <div className="min-w-0"><div className="truncate text-sm font-semibold text-[var(--canvas-ink)]">{group.label}</div><div className="mono mt-0.5 truncate text-2xs text-[var(--canvas-muted)]">{group.id}</div></div>
        </div>
        <div className={`mt-3 inline-flex items-center gap-1 text-2xs ${laneTone(group)}`}><span className="h-1.5 w-1.5 rounded-full bg-current" />{group.status || "projected"}</div>
      </div>

      <div className="absolute inset-y-0 left-[142px] right-0 grid grid-cols-7">
        {stages.map((stage) => <div key={stage.key} className="border-r border-[var(--canvas-border)]/70 last:border-r-0" />)}
      </div>
      <div className="absolute left-[128px] right-5 top-1/2 h-px bg-[var(--canvas-border)]" />
      <span className={`absolute left-[126px] top-1/2 z-20 h-3 w-3 -translate-y-1/2 rounded-full border border-current bg-[var(--canvas-panel)] ${laneTone(group)}`} />

      {group.items.map((item, itemIndex) => {
        const stageIndex = stageIndexFor(item);
        const left = `calc(142px + ((100% - 142px) / 7) * ${stageIndex} + 8px)`;
        const width = `calc(((100% - 142px) / 7) - 16px)`;
        return (
          <button
            key={item.workId}
            type="button"
            onClick={() => onSelect(item.workId)}
            data-selected={selectedId === item.workId}
            className={`live-node absolute z-30 flex h-7 items-center gap-1.5 rounded-md border px-2 text-left shadow-lg transition-transform hover:-translate-y-0.5 hover:border-work data-[selected=true]:border-work data-[selected=true]:ring-1 data-[selected=true]:ring-work/40 ${["blocked", "held"].includes(item.state) ? "!border-seam" : ""}`}
            style={{ left, width, top: `${14 + itemIndex * 34}px` }}
            aria-label={`${item.title}, ${item.runStatus || item.state}, ${item.ageLabel}`}
          >
            <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${["blocked", "held"].includes(item.state) ? "bg-seam" : item.runStatus === "running" ? "bg-good" : "bg-work"}`} />
            <span className="min-w-0 flex-1 truncate text-2xs font-semibold text-[var(--canvas-ink)]">{item.title}</span>
            <span className="mono shrink-0 text-[9px] text-[var(--canvas-muted)]">{item.ageLabel}</span>
          </button>
        );
      })}
    </div>
  );
}
