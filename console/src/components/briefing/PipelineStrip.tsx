import Link from "next/link";

import type { PipelineStage } from "@/lib/briefing";
import { FeedDegraded, feedRenderModel, type FeedProjectionStatus, type StateTone } from "@/components/kit";

/**
 * Seven work stages at board scale: Signal → Suggest → Add to work → Start →
 * Run → Proof → Learn, with live counts and one beating node while work runs.
 * Read-only — the Work screen owns the actions; this is the map.
 */

const NODE: Record<StateTone, string> = {
  draft: "border-hair bg-panel",
  queued: "border-hair bg-soft-2",
  seam: "border-seam bg-seam-soft",
  live: "border-work bg-work-soft",
  done: "border-good bg-good-soft",
  failed: "border-crit bg-crit-soft",
  dropped: "border-hair bg-soft",
};

const NODE_TEXT: Record<StateTone, string> = {
  draft: "text-faint",
  queued: "text-muted",
  seam: "text-seam",
  live: "text-work",
  done: "text-good",
  failed: "text-crit",
  dropped: "text-faint",
};

function Node({ stage }: { stage: PipelineStage }) {
  if (stage.live) {
    return (
      <span className={`relative flex h-3.5 w-3.5 items-center justify-center rounded-full border ${NODE.live}`}>
        <span className={`live-dot ${NODE_TEXT.live}`} aria-hidden />
      </span>
    );
  }
  return <span className={`h-3.5 w-3.5 rounded-full border ${NODE[stage.tone]}`} aria-hidden />;
}

export function PipelineStrip({
  stages,
  read,
}: {
  stages: PipelineStage[];
  read: { status: FeedProjectionStatus; lastGoodAt: string | null };
}) {
  // A dead workbench door must never render as an idle-but-healthy pipeline
  // (all-zero stage counts are pixel-identical to a genuinely quiet queue) -
  // packet: console-honest-degradation.
  if (feedRenderModel(read.status, stages.some((stage) => stage.count > 0)) === "degraded") {
    return (
      <FeedDegraded label="Pipeline" source="compound workbench --json --recent 500" lastGoodAt={read.lastGoodAt} />
    );
  }

  const lastIndex = stages.length - 1;
  return (
    <Link
      href="/work?lens=pipeline"
      className="block rounded-lg border border-hair bg-panel px-5 py-4 shadow-sm transition-colors hover:bg-soft focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus"
    >
      <div className="flex items-start">
        {stages.map((stage, index) => (
          <div key={stage.key} className="flex flex-1 flex-col items-center gap-1.5 text-center">
            <div className="flex w-full items-center">
              <span className={`h-px flex-1 ${index === 0 ? "opacity-0" : "bg-hair"}`} aria-hidden />
              <Node stage={stage} />
              <span className={`h-px flex-1 ${index === lastIndex ? "opacity-0" : "bg-hair"}`} aria-hidden />
            </div>
            <span className={`text-2xs font-semibold uppercase tracking-wide ${stage.live ? NODE_TEXT.live : "text-faint"}`} title={stage.hint}>
              {stage.label}
            </span>
            <span className={`text-md font-bold tabular-nums ${NODE_TEXT[stage.tone]}`}>{stage.count.toLocaleString("en-US")}</span>
          </div>
        ))}
      </div>
    </Link>
  );
}
