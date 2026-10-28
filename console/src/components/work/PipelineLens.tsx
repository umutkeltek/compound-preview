"use client";

import * as Tooltip from "@radix-ui/react-tooltip";
import Link from "next/link";

import { EmptyState } from "@/components/kit/EmptyState";
import { WorkBoardCard } from "@/components/work/WorkBoardCard";
import type { PipelineStageModel } from "@/lib/work-pipeline";

/**
 * Pipeline lens: seven work stages as one horizontal board — Signal → Suggest →
 * Add to work → Start → Run → Proof → Learn. `minmax(178px, 1fr)` columns that grow to fill on
 * wide screens and scroll horizontally when they can't. The row of stage nodes
 * over a continuous hairline is the progress motif at board scale; the Run node
 * beats while work is genuinely running, and Add to work wears the amber seat.
 * Stage headers carry plain-language
 * tooltips explaining each stage (Radix Tooltip — a11y we would hand-roll wrong).
 */

const GRID: React.CSSProperties = { gridTemplateColumns: "repeat(7, minmax(178px, 1fr))" };

function InfoTip({ text }: { text: string }) {
  return (
    <Tooltip.Root>
      <Tooltip.Trigger asChild>
        <button
          type="button"
          aria-label="What this stage means"
          className="ml-auto flex h-4 w-4 items-center justify-center rounded-pill border border-hair text-2xs font-bold text-faint hover:text-muted"
        >
          i
        </button>
      </Tooltip.Trigger>
      <Tooltip.Portal>
        <Tooltip.Content
          sideOffset={6}
          className="z-50 max-w-56 rounded-md border border-hair bg-panel px-2.5 py-1.5 text-2xs leading-relaxed text-muted shadow-lg
                     data-[state=delayed-open]:animate-[scrim-in_var(--dur-fast)_var(--ease-out)]"
        >
          {text}
          <Tooltip.Arrow className="fill-hair" />
        </Tooltip.Content>
      </Tooltip.Portal>
    </Tooltip.Root>
  );
}

function StageHeader({ stage }: { stage: PipelineStageModel }) {
  return (
    <div className="flex items-center gap-2 border-b border-hair pb-2">
      <span className="pipe-node" data-live={stage.node} aria-hidden />
      <span className="text-2xs font-bold tabular-nums text-faint">{stage.index}</span>
      <span
        className={`text-xs font-bold uppercase tracking-wide ${stage.seat ? "text-seam" : "text-muted"}`}
      >
        {stage.label}
      </span>
      <span className="mono text-2xs tabular-nums text-faint">{stage.count}</span>
      <InfoTip text={stage.tooltip} />
    </div>
  );
}

export function PipelineLens({
  stages,
  onOpenTrace,
}: {
  stages: PipelineStageModel[];
  onOpenTrace: (workId: string) => void;
}) {
  return (
    <Tooltip.Provider delayDuration={150}>
      <div className="overflow-x-auto pb-2">
        <div className="grid gap-3" style={GRID}>
          {stages.map((stage) => (
            <div key={stage.key} className="flex min-w-0 flex-col gap-3">
              <StageHeader stage={stage} />

              {stage.signal ? (
                <Link
                  href={stage.signal.href}
                  className="flex flex-col gap-1 rounded-lg border border-hair bg-soft px-3 py-3 hover:bg-soft-2"
                >
                  <span className="text-2xl font-extrabold tabular-nums tracking-tight text-ink">
                    {stage.signal.count}
                  </span>
                  <span className="text-xs font-semibold text-muted">{stage.signal.label}</span>
                  <span className="text-2xs text-faint">{stage.signal.sub} · open sources →</span>
                </Link>
              ) : stage.cards.length ? (
                <div className="flex flex-col gap-2">
                  {stage.cards.map((card) => (
                    <WorkBoardCard key={`${stage.key}:${card.key}`} card={card} onOpen={onOpenTrace} compact />
                  ))}
                </div>
              ) : (
                <EmptyState>{stage.emptyCopy}</EmptyState>
              )}
            </div>
          ))}
        </div>
      </div>
    </Tooltip.Provider>
  );
}
