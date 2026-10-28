import { ProductLoopCard, loopState } from "@/components/products/ProductLoopCard";
import { EmptyState, FreshnessChip, LoopIcon, PageHeader, SectionHeader } from "@/components/kit";
import { getProductLoopsReport, getProductPortfolio } from "@/lib/products";

export const dynamic = "force-dynamic";

/**
 * Loops — "how does output change the next move; which loop is blocked on me?"
 * (redesign plan Part 3). Product loops are the first shape; the same
 * change → signal → hypothesis → review → next-move loop holds any product,
 * channel, or workflow. Loops blocked on the operator (a verdict is due) float
 * to the top with the amber admission seam.
 */

function StatTile({
  label,
  value,
  sub,
  seam,
}: {
  label: string;
  value: number;
  sub?: string;
  seam?: boolean;
}) {
  const emphasise = seam && value > 0;
  return (
    <div className="rounded-lg border border-hair bg-panel px-3 py-2.5 shadow-sm">
      <div className={`text-2xs font-bold uppercase tracking-wide ${emphasise ? "text-seam" : "text-faint"}`}>
        {label}
      </div>
      <div className={`mono mt-1 text-xl tabular-nums ${emphasise ? "text-seam" : "text-ink"}`}>{value}</div>
      {sub ? <div className="mt-0.5 text-2xs text-faint">{sub}</div> : null}
    </div>
  );
}

export default async function LoopsPage() {
  const [{ generatedAt, loops }, portfolio] = await Promise.all([
    getProductLoopsReport(),
    getProductPortfolio(),
  ]);

  const withSignals = loops.filter((l) => l.signalCount > 0).length;
  const totalSignals = loops.reduce((n, l) => n + l.signalCount, 0);
  const reviewReady = portfolio.reduce((n, p) => n + p.reviewReadyCount, 0);
  const blockedOnYou = loops.filter((l) => loopState(l).blockedOnYou).length;

  // Loops blocked on the operator float to the top; the rest keep the
  // signal-volume order the projection returns.
  const ordered = loops
    .map((loop, index) => ({ loop, index, blocked: loopState(loop).blockedOnYou }))
    .sort((a, b) => (a.blocked === b.blocked ? a.index - b.index : a.blocked ? -1 : 1))
    .map((entry) => entry.loop);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        breadcrumb="operate / loops"
        title="Feedback loops"
        right={<FreshnessChip generatedAt={generatedAt} source="compound product loops --json" />}
      />
      <p className="max-w-prose text-sm text-muted">
        One shape for anything with outputs and feedback — change → signal → hypothesis → review → next move. Today these
        are your apps; the same loop holds any product, channel, or workflow as sources connect.
      </p>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile label="Loops" value={loops.length} sub="tracked" />
        <StatTile label="With live signals" value={withSignals} sub={`${totalSignals} signals total`} />
        <StatTile label="Blocked on you" value={blockedOnYou} sub="a verdict is due" seam />
        <StatTile label="Awaiting data" value={loops.length - withSignals} sub="connected, no signal yet" />
      </div>

      {loops.length === 0 ? (
        <EmptyState>
          No loops yet. A loop starts when a subject gets a signal — register an app with{" "}
          <code className="mono">compound product import &lt;product.yaml&gt;</code> and feed it a metric.
        </EmptyState>
      ) : (
        <section className="flex flex-col gap-3">
          <SectionHeader
            tone="product"
            icon={<LoopIcon />}
            label="Product loops"
            sub="blocked-on-you first, then by signal volume"
            right={<span className="mono text-2xs tabular-nums text-faint">{loops.length}</span>}
          />
          <div className="grid gap-4 lg:grid-cols-2">
            {ordered.map((loop) => (
              <ProductLoopCard key={loop.productId} loop={loop} />
            ))}
          </div>
        </section>
      )}

      <p className="mono text-2xs leading-snug text-faint">
        source: compound product loops --json · compound product authority --json · reviewReady {reviewReady}.
        Connected: App Store Connect, Search Console, canonical metrics. Shape-supported (no puller yet): RevenueCat,
        PostHog. Not connected: TelemetryDeck.
      </p>
    </div>
  );
}
