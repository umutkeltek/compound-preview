import Link from "next/link";

import { CopyCmd } from "@/components/CopyCmd";
import { IdRef, StateBadge } from "@/components/kit";
import { ago } from "@/lib/time";
import {
  formatMetricValue,
  groupSignalsByLens,
  metricLabel,
  metricSurface,
  nextMove,
  productDisplayName,
  type ProductLoop,
  type ProductSignal,
} from "@/lib/products";

/*
 * One product = one identity, rendered as its loop: signals (evidence) →
 * hypothesis → review → verdict, with freshness and the next move. Restyled on
 * the kit (DESIGN.md): card grammar, the 7-tone StateBadge, a done→now→next
 * hypothesis chain (the cockpit pattern), token-only color. A loop that is
 * "blocked on you" (a signal+hypothesis matched, verdict due) carries the amber
 * admission seam and floats to the top of the page.
 */

/** Map a loop onto its lifecycle state + a plain label for the StateBadge. */
export function loopState(loop: ProductLoop): { state: string; label: string; blockedOnYou: boolean } {
  if (loop.loopGaps.includes("review_ready")) return { state: "needs_you", label: "verdict due", blockedOnYou: true };
  if (loop.signalCount === 0) return { state: "candidate", label: "awaiting signal", blockedOnYou: false };
  const verdict = loop.reviews[0]?.verdict;
  if (verdict === "supported") return { state: "completed", label: "supported", blockedOnYou: false };
  if (verdict === "refuted") return { state: "failed", label: "refuted", blockedOnYou: false };
  if (verdict === "inconclusive") return { state: "held", label: "inconclusive", blockedOnYou: false };
  if (loop.hypothesisCount > 0) return { state: "running", label: "testing", blockedOnYou: false };
  return { state: "watching", label: "collecting", blockedOnYou: false };
}

/** Last-signal freshness — respects the projection's server-computed `stale`
 *  flag (not a client TTL), so it never disagrees with the backend. */
function SignalFreshness({ stale, latestSignalAt }: { stale: boolean; latestSignalAt: string }) {
  const hasSignal = Boolean(latestSignalAt);
  const tone = !hasSignal ? "text-faint" : stale ? "text-seam" : "text-good";
  const dot = !hasSignal ? "bg-faint" : stale ? "bg-seam" : "bg-good";
  const text = hasSignal ? `${stale ? "stale" : "live"} · last signal ${ago(latestSignalAt)}` : "no signal yet";
  return (
    <span
      className={`mono inline-flex items-center gap-1.5 rounded-pill border border-hair bg-soft px-2.5 py-0.5 text-2xs font-semibold tabular-nums ${tone}`}
    >
      <span className={`h-1.5 w-1.5 rounded-full ${dot}`} aria-hidden />
      {text}
    </span>
  );
}

/** The hypothesis chain: done → now → next (the cockpit motif). */
function StepChain({ loop }: { loop: ProductLoop }) {
  const move = nextMove(loop);
  const done = loop.signalCount > 0 ? `${loop.signalCount} signals` : "connected";
  const now =
    loop.hypothesisCount > 0
      ? `${loop.hypothesisCount} hypothes${loop.hypothesisCount === 1 ? "is" : "es"}`
      : loop.signalCount > 0
        ? "forming hypothesis"
        : "no signal yet";
  const steps: { key: string; tone: string; value: string }[] = [
    { key: "done", tone: "border-good-line bg-good-soft text-good", value: done },
    { key: "now", tone: "border-work-line bg-work-soft text-work", value: now },
    { key: "next", tone: "border-hair bg-soft-2 text-muted", value: move.text },
  ];
  return (
    <div className="flex flex-wrap items-stretch gap-1.5">
      {steps.map((step, i) => (
        <div key={step.key} className="flex items-center gap-1.5">
          <div className={`rounded-md border px-2.5 py-1.5 ${step.tone}`}>
            <div className="text-2xs font-bold uppercase tracking-wide opacity-70">{step.key}</div>
            <div className="mt-0.5 text-xs font-medium leading-snug">{step.value}</div>
          </div>
          {i < steps.length - 1 ? (
            <span className="text-faint" aria-hidden>
              →
            </span>
          ) : null}
        </div>
      ))}
    </div>
  );
}

function SurfaceChip({ label }: { label: string }) {
  return (
    <span className="inline-flex items-center rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs font-semibold text-muted">
      {label}
    </span>
  );
}

function MetricRow({ s }: { s: ProductSignal }) {
  const surface = metricSurface(s.metric, s.provider);
  const before = formatMetricValue(s.metric, s.before);
  const after = formatMetricValue(s.metric, s.after);
  const changed = s.before != null && s.after != null && s.before !== s.after;
  const up = changed && (s.after as number) > (s.before as number);
  return (
    <div className="flex items-center justify-between gap-3 py-1">
      <div className="flex min-w-0 items-center gap-2">
        <SurfaceChip label={surface.label} />
        <span className="truncate text-xs text-ink">{metricLabel(s.metric)}</span>
      </div>
      <div className="flex shrink-0 items-center gap-1.5 text-xs tabular-nums">
        {changed ? (
          <>
            <span className="text-faint">{before}</span>
            <span className={up ? "text-good" : "text-crit"}>→ {after}</span>
          </>
        ) : (
          <span className="font-medium text-ink" title="before == after (snapshot)">
            {after}
          </span>
        )}
      </div>
    </div>
  );
}

export function ProductLoopCard({ loop }: { loop: ProductLoop }) {
  const move = nextMove(loop);
  const latestReview = loop.reviews[0];
  const empty = loop.signalCount === 0;
  const { state, label, blockedOnYou } = loopState(loop);
  const transitionRef = loop.signals.find((s) => s.transitionRef)?.transitionRef ?? "";

  return (
    <article
      className={`flex flex-col gap-4 rounded-lg border border-hair bg-panel p-4 shadow-sm ${
        blockedOnYou ? "admission-seam" : ""
      }`}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="text-md font-semibold text-ink">{productDisplayName(loop.productId)}</h2>
          <p className="mt-0.5 text-2xs text-faint">
            {empty
              ? "connected · awaiting first signal"
              : `${loop.metricNames.length} metric${loop.metricNames.length === 1 ? "" : "s"} tracked`}
          </p>
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1.5">
          <StateBadge state={state} label={label} />
          <SignalFreshness stale={loop.freshness.stale} latestSignalAt={loop.freshness.latestSignalAt} />
        </div>
      </div>

      {/* the loop as a done → now → next chain */}
      <StepChain loop={loop} />

      {/* signals through domain lenses (a view of the same evidence) */}
      {!empty ? (
        <div className="rounded-md border border-hair bg-soft px-3 py-2.5">
          <div className="section-label mb-2">Lenses · the same signals, read by what matters</div>
          <div className="flex flex-col gap-3">
            {groupSignalsByLens(loop.signals).map(({ lens, meta, signals }) => (
              <div key={lens}>
                <div className="mb-0.5 flex items-center gap-2">
                  <SurfaceChip label={meta.label} />
                  <span className="text-2xs text-faint">{meta.question}</span>
                </div>
                {signals.map((s) => (
                  <MetricRow key={s.signalId} s={s} />
                ))}
              </div>
            ))}
          </div>
        </div>
      ) : null}

      {/* the verdict's own next-action, if a review exists */}
      {latestReview ? (
        <p className="text-2xs text-faint">
          Reviewed {ago(latestReview.reviewedAt)} →{" "}
          <span className="font-medium text-muted">{latestReview.verdict}</span>. Causality stays unproven until a
          before/after change event is reviewed.
        </p>
      ) : null}

      {/* next move (the doctor's answer) */}
      <div className="rounded-md border border-hair bg-soft px-3 py-2.5">
        <div className="section-label mb-1.5">Next move</div>
        <p className="mb-2 text-xs text-ink">{move.text}</p>
        <CopyCmd cmd={move.cmd} />
        {blockedOnYou ? (
          <Link
            href="/reviews"
            className="mt-2 inline-flex items-center gap-1 text-xs font-medium text-work underline-offset-2 hover:underline"
          >
            Record the verdict in Decide →
          </Link>
        ) : null}
      </div>

      {transitionRef ? (
        <div className="flex items-center gap-1.5 text-2xs text-faint">
          <span>proof</span>
          <IdRef id={transitionRef} kind="transition" />
        </div>
      ) : null}
    </article>
  );
}
