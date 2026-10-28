import Link from "next/link";

import { EmptyDoctrine, Panel } from "@/components/briefing/primitives";
import { FeedDegraded, type FeedProjectionStatus } from "@/components/kit";
import { ago } from "@/lib/time";
import type { ProofBackedChange } from "@/representation";

const SOURCE = "compound workbench --json --recent 500";

/**
 * The latest ledger-proven change, paired with the Briefing's highest operator
 * gate. It is deliberately a proof instrument rather than another activity
 * card: one outcome, a compact three-stage proof chain, and the two references
 * that let an operator inspect the claim.
 */
export function LatestMoment({
  moment,
  read,
}: {
  moment: ProofBackedChange | null;
  read: { status: FeedProjectionStatus; lastGoodAt: string | null };
}) {
  if (read.status === "unreachable") {
    return (
      <Panel className="px-5 py-5">
        <FeedDegraded label="proof-backed change" source={SOURCE} lastGoodAt={read.lastGoodAt} />
      </Panel>
    );
  }

  if (!moment) {
    return (
      <Panel className="px-5 py-5">
        <div className="text-xs font-bold uppercase tracking-wide text-faint">Proof-backed change</div>
        <div className="mt-3">
          <EmptyDoctrine>No proof-backed change has been recorded yet.</EmptyDoctrine>
        </div>
      </Panel>
    );
  }

  const lastGood = read.status === "stale" && read.lastGoodAt ? ago(read.lastGoodAt) : "";

  return (
    <section className="overflow-hidden rounded-lg border border-hair bg-panel shadow-sm">
      <div className="px-5 pb-4 pt-5">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <span className="text-xs font-bold uppercase tracking-wide text-faint">Proof-backed change</span>
          <span className="inline-flex items-center gap-1.5 rounded-pill border border-good-line bg-good-soft px-2.5 py-1 text-2xs font-semibold text-good">
            <span className="h-1.5 w-1.5 rounded-full bg-good" aria-hidden />
            {moment.moveLabel}
          </span>
        </div>

        <h2 className="mt-4 text-lg font-semibold leading-snug tracking-tight text-strong text-balance">
          {moment.headline}
        </h2>

        <p className="mt-2 text-xs text-muted">Reuse and downstream benefit have not been verified.</p>
        <ol className="mt-3 flex flex-wrap gap-x-3 gap-y-1 text-2xs text-faint" aria-label="Improvement evidence">
          <li aria-current="step" className="font-semibold text-ink">Proof-backed change</li>
          <li>Observed reuse · unknown</li>
          <li>Downstream outcome · unknown</li>
          <li>Measured improvement · unknown</li>
        </ol>

        <div className="mt-2 flex flex-wrap items-center justify-between gap-x-3 gap-y-1 text-xs text-muted">
          <span>
            recorded by <strong className="font-semibold text-ink">{moment.producer}</strong>
          </span>
          {lastGood ? <span className="text-faint">stale feed · last good {lastGood}</span> : null}
        </div>

        <div className="mt-5">
          <div
            className="flex h-2 gap-1 rounded-pill bg-soft-2 p-0.5"
            role="img"
            aria-label="Proof chain: changed, receipted, transitioned"
          >
            <span className="flex-1 rounded-pill bg-work" aria-hidden />
            <span className="flex-1 rounded-pill bg-good" aria-hidden />
            <span className="flex-1 rounded-pill bg-route" aria-hidden />
          </div>
          <div className="mt-2 grid grid-cols-3 text-2xs font-medium text-faint" aria-hidden>
            <span>Changed</span>
            <span className="text-center">Receipted</span>
            <span className="text-right">Transitioned</span>
          </div>
        </div>
      </div>

      <div className="grid grid-cols-2 border-t border-hair bg-soft">
        <div className="min-w-0 px-5 py-3.5">
          <div className="text-2xs font-semibold text-muted">Receipt</div>
          <div className="mono mt-1 truncate text-2xs text-faint" title={moment.receiptRef}>
            {moment.receiptRef}
          </div>
        </div>
        <div className="min-w-0 border-l border-hair px-5 py-3.5">
          <div className="flex items-center justify-between gap-2">
            <div className="text-2xs font-semibold text-muted">Transition</div>
            {moment.workId ? (
              <Link
                href={`/work/${moment.workId}`}
                className="text-2xs font-semibold text-work hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
              >
                Open trace →
              </Link>
            ) : null}
          </div>
          <div className="mono mt-1 truncate text-2xs text-faint" title={moment.transitionRef}>
            {moment.transitionRef}
          </div>
        </div>
      </div>
    </section>
  );
}
