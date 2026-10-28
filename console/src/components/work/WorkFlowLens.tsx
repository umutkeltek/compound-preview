"use client";

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

export function WorkFlowLens({ stages, groups, onOpenTrace }: { stages: PipelineStageModel[]; groups: WorkLensGroup[]; onOpenTrace: (workId: string) => void }) {
  if (!groups.length) return <EmptyState>No open work is available for the execution flow.</EmptyState>;
  return (
    <section className="min-w-0 overflow-hidden rounded-xl border border-hair bg-panel shadow-sm">
      <div className="border-b border-hair px-4 py-3">
        <div className="text-sm font-semibold text-ink">Execution flow</div>
        <p className="mt-0.5 text-xs text-muted">Current lifecycle positions by executor lane. This is a live projection, not reconstructed history.</p>
      </div>
      <div className="overflow-x-auto">
        <div className="min-w-[1120px]">
          <div className="grid border-b border-hair bg-soft/70" style={{ gridTemplateColumns: "150px repeat(7,minmax(132px,1fr))" }}>
            <div className="px-3 py-3 text-2xs font-bold uppercase tracking-wide text-faint">Executor lane</div>
            {stages.map((stage) => <div key={stage.key} className="border-l border-hair px-3 py-3"><div className="text-xs font-semibold text-ink">{stage.label}</div><div className="mt-0.5 text-2xs text-faint">{stage.count} now</div></div>)}
          </div>
          {groups.map((group) => (
            <div key={group.id} className="grid min-h-28 border-b border-hair last:border-b-0" style={{ gridTemplateColumns: "150px repeat(7,minmax(132px,1fr))" }}>
              <div className="bg-soft/40 px-3 py-4"><div className="text-sm font-semibold text-ink">{group.label}</div><div className="mono mt-1 text-2xs text-faint">showing {Math.min(group.items.length, MAX_VISIBLE_PER_LANE)} of {group.items.length}</div></div>
              {stages.map((stage, stageIndex) => {
                const items = visibleItems(group).filter((item) => item.progress.active === stageIndex || (item.progress.active == null && item.progress.done === stageIndex));
                return <div key={stage.key} className="relative border-l border-hair px-2 py-3 before:absolute before:left-0 before:right-0 before:top-1/2 before:h-px before:bg-hair before:content-['']">
                  <div className="relative z-10 grid gap-2">
                    {items.map((item) => <button key={item.workId} type="button" onClick={() => onOpenTrace(item.workId)} className={`rounded-md border bg-panel px-2.5 py-2 text-left shadow-sm hover:border-work-line hover:shadow ${["blocked", "held"].includes(item.state) ? "border-seam-line" : "border-hair"}`}>
                      <div className="line-clamp-2 text-xs font-semibold leading-snug text-ink">{item.title}</div><div className="mt-1 flex items-center justify-between gap-2"><StateBadge state={item.runStatus || item.state} /><span className="text-2xs text-faint">{item.ageLabel}</span></div><div className="mt-1"><IdRef id={item.workId} kind="work" plain truncate /></div>
                    </button>)}
                  </div>
                </div>;
              })}
            </div>
          ))}
        </div>
      </div>
      <div className="flex flex-wrap items-center justify-between gap-2 border-t border-hair px-4 py-2 text-2xs text-faint"><span>Active and proof-bound work is prioritized in each lane.</span><span>Use Table for the complete backlog.</span></div>
    </section>
  );
}
