"use client";

import * as Tabs from "@radix-ui/react-tabs";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useCallback, useEffect, useRef, useState } from "react";

import { BoardLens } from "@/components/work/BoardLens";
import { CapsuleColumns } from "@/components/work/CapsuleColumns";
import { OfferedLens } from "@/components/work/OfferedLens";
import { PipelineLens } from "@/components/work/PipelineLens";
import { TraceDrawer } from "@/components/work/TraceDrawer";
import { TransitionsTicker } from "@/components/work/TransitionsTicker";
import { WorkFlowLens } from "@/components/work/WorkFlowLens";
import { WorkSpaceLens } from "@/components/work/WorkSpaceLens";
import { WorkTableLens } from "@/components/work/WorkTableLens";
import type { LaneTrafficReport } from "@/lib/lane-traffic";
import type { BoardColumnModel, OfferedPacketModel, PipelineStageModel } from "@/lib/work-pipeline";
import type { WorkLensGroup, WorkLensItem } from "@/lib/work-views";

/**
 * Work screen shell (redesign plan Part 3): three lenses over the same units of
 * work — Table (default, the needs-first operational view), then Flow, Space,
 * Pipeline, Board, and Offered packets — with ONE Trace drawer mounted once
 * and opened by any card in any lens. The open trace is mirrored into
 * `?trace=<id>` so it is
 * deep-linkable and shareable (Briefing's pipeline strip links straight here).
 *
 * Above the lenses: the DESIGN.md §9.6 "information flow" ambient row — the
 * live Transitions ticker (streams over /api/stream) and CapsuleColumns lane
 * traffic — visible regardless of which lens tab is active, since both are
 * ambient organism-is-alive signal, not lens-scoped data.
 */

export type WorkLensKey = "pipeline" | "table" | "flow" | "space" | "board" | "offered";

const TAB_CLASS =
  "shrink-0 rounded-md px-3 py-1.5 text-xs font-semibold text-muted " +
  "data-[state=active]:bg-work-soft data-[state=active]:text-work data-[state=active]:shadow-sm";

export function WorkLenses({
  stages,
  columns,
  offered,
  openCount,
  initialTrace,
  laneTraffic,
  items,
  groups,
  defaultLens,
  mutationsReady,
}: {
  stages: PipelineStageModel[];
  columns: BoardColumnModel[];
  offered: OfferedPacketModel[];
  openCount: number;
  initialTrace: string | null;
  laneTraffic: LaneTrafficReport;
  items: WorkLensItem[];
  groups: WorkLensGroup[];
  defaultLens: WorkLensKey;
  mutationsReady: boolean;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const paramsRef = useRef(searchParams.toString());
  const [trace, setTrace] = useState<string | null>(initialTrace);
  const [lens, setLens] = useState<WorkLensKey>(defaultLens);

  useEffect(() => {
    paramsRef.current = searchParams.toString();
  }, [searchParams]);

  const replaceParams = useCallback((next: { trace?: string | null; lens?: WorkLensKey }) => {
    // Keep a synchronous query snapshot. A close followed immediately by a
    // lens switch must not resurrect the trace while router.replace settles.
    const params = new URLSearchParams(paramsRef.current);
    if (next.trace === null) params.delete("trace");
    else if (next.trace) params.set("trace", next.trace);
    if (next.lens) {
      if (next.lens === "table") params.delete("lens");
      else params.set("lens", next.lens);
    }
    const query = params.toString();
    paramsRef.current = query;
    router.replace(query ? `${pathname}?${query}` : pathname, { scroll: false });
  }, [pathname, router]);

  const openTrace = useCallback(
    (workId: string) => {
      setTrace(workId);
      replaceParams({ trace: workId });
    },
    [replaceParams],
  );

  const closeTrace = useCallback(() => {
    setTrace(null);
    replaceParams({ trace: null });
  }, [replaceParams]);

  return (
    <>
      <Tabs.Root value={lens} onValueChange={(value) => {
        const next = value as WorkLensKey;
        setLens(next);
        replaceParams({ lens: next });
      }} className="flex min-w-0 flex-col gap-4">
        <Tabs.List className="inline-flex w-full max-w-full items-center gap-1 overflow-x-auto rounded-lg border border-hair bg-soft p-1 sm:w-fit">
          <Tabs.Trigger value="table" className={TAB_CLASS}>
            Table
            <span className="mono ml-1.5 tabular-nums text-faint">{items.length}</span>
          </Tabs.Trigger>
          <Tabs.Trigger value="flow" className={TAB_CLASS}>Flow</Tabs.Trigger>
          <Tabs.Trigger value="space" className={TAB_CLASS}>Space</Tabs.Trigger>
          <Tabs.Trigger value="pipeline" className={TAB_CLASS}>
            Pipeline
          </Tabs.Trigger>
          <Tabs.Trigger value="board" className={TAB_CLASS}>
            Board
            <span className="mono ml-1.5 tabular-nums text-faint">{openCount}</span>
          </Tabs.Trigger>
          <Tabs.Trigger value="offered" className={TAB_CLASS}>
            Offered packets
            <span className="mono ml-1.5 tabular-nums text-faint">{offered.length}</span>
          </Tabs.Trigger>
        </Tabs.List>

        <Tabs.Content value="pipeline" className="min-w-0 focus-visible:outline-none">
          <PipelineLens stages={stages} onOpenTrace={openTrace} />
        </Tabs.Content>
        <Tabs.Content value="table" className="min-w-0 focus-visible:outline-none">
          <WorkTableLens items={items} onOpenTrace={openTrace} />
        </Tabs.Content>
        <Tabs.Content value="flow" className="min-w-0 focus-visible:outline-none">
          <WorkFlowLens stages={stages} groups={groups} onOpenTrace={openTrace} />
        </Tabs.Content>
        <Tabs.Content value="space" className="min-w-0 focus-visible:outline-none">
          <WorkSpaceLens stages={stages} groups={groups} onOpenTrace={openTrace} />
        </Tabs.Content>
        <Tabs.Content value="board" className="min-w-0 focus-visible:outline-none">
          <BoardLens columns={columns} onOpenTrace={openTrace} />
        </Tabs.Content>
        <Tabs.Content value="offered" className="min-w-0 focus-visible:outline-none">
          <OfferedLens packets={offered} />
        </Tabs.Content>
      </Tabs.Root>

      <section data-depth-min="investigate" className="mt-2 flex min-w-0 flex-col gap-3">
        <div>
          <div className="section-label">Activity context</div>
          <p className="mt-1 text-xs text-muted">Recent transitions and route traffic stay available as supporting evidence, below the selected work lens.</p>
        </div>
        <div className="grid min-w-0 gap-4 lg:grid-cols-[3fr_2fr]">
          <TransitionsTicker onOpenTrace={openTrace} />
          <CapsuleColumns report={laneTraffic} />
        </div>
      </section>

      <TraceDrawer workId={trace} onClose={closeTrace} mutationsReady={mutationsReady} />
    </>
  );
}
