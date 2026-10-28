"use client";

import { useEffect, useState } from "react";

import { Drawer, FreshnessChip, IdRef, TooltipCard } from "@/components/kit";
import type { Tone } from "@/components/kit";
import { WorkActionRow, type WorkActionRowData } from "@/components/work/WorkActionRow";
import type { TraceModel, TraceStep } from "@/lib/work-trace";

/** Classify an envelope-trace meta ref so its TooltipCard names what it is. */
function refKind(ref: string): { label: string; tone: Tone } {
  if (/\$|\bcost\b|tokens?\b/i.test(ref)) return { label: "cost", tone: "good" };
  if (/^ctx_/.test(ref)) return { label: "context pack", tone: "capability" };
  if (/^rec_|receipt/i.test(ref)) return { label: "receipt", tone: "good" };
  if (/^tr_|transition/i.test(ref)) return { label: "transition", tone: "route" };
  if (/^work_/.test(ref)) return { label: "work id", tone: "work" };
  if (/lease|lz_/i.test(ref)) return { label: "lease", tone: "seam" };
  return { label: "reference", tone: "neutral" };
}

/** A mono meta ref with a HoverCard that reveals the full (often truncated)
 *  value and names its kind — hover reveals depth, no mystery meat (§9.4). */
function MetaRef({ value }: { value: string }) {
  const kind = refKind(value);
  return (
    <TooltipCard
      side="left"
      align="start"
      title={kind.label}
      rows={[{ label: "value", value, tone: kind.tone }]}
      trigger={
        <span
          className="mono w-fit max-w-full cursor-default truncate text-2xs text-faint underline decoration-dotted decoration-hair underline-offset-2 hover:text-muted"
          title={value}
        >
          {value}
        </span>
      }
    />
  );
}

/*
 * THE Trace drawer (DESIGN.md §3) — the signature element of the redesign: the
 * envelope spine (Intent → ContextPack → WorkEnvelope → ActionIntent →
 * PolicyDecision → Receipt → Transition) rendered as a vertical timeline, one
 * node beating while work is genuinely live, hollow/"no record" where the
 * projection has no data (never fabricated), and a contextual governed
 * action row in the footer. Populated on open from `/api/work/trace`.
 */

type TracePayload = { generatedAt: string; trace: TraceModel; actionRow: WorkActionRowData };

const PHASE_TONE: Record<TraceStep["status"], string> = {
  done: "border-good-line bg-good-soft text-good",
  live: "border-work-line bg-work-soft text-work",
  pending: "border-hair bg-transparent text-faint",
};

function StepRow({ step }: { step: TraceStep }) {
  return (
    <li className="flex gap-3">
      <span className="trace-node mt-0.5" data-status={step.status} aria-hidden />
      <div className="min-w-0 flex-1 pb-4">
        <div className="flex flex-wrap items-center gap-2">
          <span className={`text-sm font-semibold ${step.status === "pending" ? "text-muted" : "text-ink"}`}>
            {step.name}
          </span>
          <span
            className={`inline-flex items-center rounded-pill border px-2 py-0.5 text-2xs font-semibold tabular-nums ${PHASE_TONE[step.status]}`}
          >
            {step.status === "live" ? <span className="live-dot mr-1" aria-hidden /> : null}
            {step.phase}
          </span>
        </div>
        <p className={`mt-1 text-xs leading-relaxed ${step.noRecord ? "text-faint italic" : "text-muted"}`}>
          {step.description}
        </p>
        {step.meta.length ? (
          <div className="mt-1.5 flex flex-col items-start gap-0.5">
            {step.meta.map((ref, index) => (
              <MetaRef key={index} value={ref} />
            ))}
          </div>
        ) : null}
      </div>
    </li>
  );
}

function Spine({ payload }: { payload: TracePayload }) {
  const { trace } = payload;
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-2xs font-bold uppercase tracking-wide text-faint">Envelope trace · intent → proof</span>
        <FreshnessChip generatedAt={payload.generatedAt} source={trace.source} />
      </div>

      <ol className="trace-spine flex flex-col">
        <span className="trace-rail" aria-hidden />
        {trace.steps.map((step) => (
          <StepRow key={step.key} step={step} />
        ))}
      </ol>

      {trace.lifecycle.length ? (
        <details className="rounded-md border border-hair bg-soft px-3 py-2">
          <summary className="cursor-pointer text-xs font-semibold text-muted">
            Lifecycle history · {trace.lifecycle.length} step{trace.lifecycle.length === 1 ? "" : "s"}
          </summary>
          <ul className="mt-2 flex flex-col gap-1">
            {trace.lifecycle.map((line, index) => (
              <li key={index} className="mono text-2xs leading-relaxed text-faint">
                {line}
              </li>
            ))}
          </ul>
        </details>
      ) : null}

      <div className="flex items-center gap-2 text-2xs text-faint">
        <span>Trace lineage →</span>
        <span className="mono">compound system lineage {trace.workId}</span>
      </div>
    </div>
  );
}

export function TraceDrawer({
  workId,
  onClose,
  mutationsReady,
}: {
  workId: string | null;
  onClose: () => void;
  mutationsReady: boolean;
}) {
  const [payload, setPayload] = useState<TracePayload | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!workId) return;
    let cancelled = false;
    setPayload(null);
    setError(null);
    setLoading(true);
    fetch(`/api/work/trace?id=${encodeURIComponent(workId)}`)
      .then(async (response) => {
        const body = await response.json().catch(() => null);
        if (cancelled) return;
        if (response.ok && body?.trace) {
          setPayload(body as TracePayload);
        } else {
          setError(body?.error || `Could not load trace (${response.status}).`);
        }
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : "Could not load trace.");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [workId]);

  return (
    <Drawer
      open={Boolean(workId)}
      onOpenChange={(open) => !open && onClose()}
      title={payload?.trace.title || "Work trace"}
      meta={workId ? <IdRef id={workId} plain /> : undefined}
      footer={
        payload ? (
          <div className="w-full">
            <div className="mb-2 text-2xs font-bold uppercase tracking-wide text-faint">Move this work</div>
            <WorkActionRow data={payload.actionRow} mutationsReady={mutationsReady} onDone={onClose} />
          </div>
        ) : null
      }
    >
      {loading ? (
        <div className="flex flex-col gap-3">
          <div className="skeleton h-4 w-40" />
          <div className="skeleton h-24" />
          <div className="skeleton h-24" />
        </div>
      ) : error ? (
        <p className="text-sm text-crit">{error}</p>
      ) : payload ? (
        <Spine payload={payload} />
      ) : null}
    </Drawer>
  );
}
