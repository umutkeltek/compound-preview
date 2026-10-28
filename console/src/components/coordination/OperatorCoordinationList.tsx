import { IdRef } from "@/components/kit";
import type { OperatorCoordinationProjection } from "@/lib/coordination-model";

export type OperatorCoordinationListProps = {
  projection: OperatorCoordinationProjection;
  label?: string;
};

export function OperatorCoordinationList({
  projection,
  label = "Recorded coordination",
}: OperatorCoordinationListProps) {
  if (projection.items.length === 0) return null;

  return (
    <section className="flex flex-col gap-2">
      <div className="flex items-baseline justify-between gap-3">
        <h3 className="section-label">{label}</h3>
        <span className="text-2xs text-faint">
          {projection.total} current{projection.truncated ? " · showing 8" : ""}
        </span>
      </div>
      <div className="overflow-hidden rounded-xl border border-hair bg-panel shadow-sm">
        {projection.items.map((item) => (
          <article
            key={`${item.workRef}:${item.delta.kind}:${item.delta.subjectRef}`}
            className="flex flex-col gap-2 border-b border-hair px-3 py-3 last:border-b-0"
          >
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
              <span className="mono rounded-pill border border-hair bg-soft px-1.5 py-0.5 text-2xs text-muted">
                {item.deliveryClass || "silent_state"}
              </span>
              <IdRef id={item.workRef} kind="work" plain />
              <IdRef id={item.eventRef} kind="generic" plain />
            </div>
            <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 text-2xs">
              <span className="mono text-muted">{item.delta.kind || "state"}:{item.delta.subjectRef || "unknown"}</span>
              {item.delta.before ? <span className="mono text-faint">{item.delta.before}</span> : null}
              {item.delta.before || item.delta.after ? <span className="text-faint">→</span> : null}
              {item.delta.after ? <span className="mono text-ink">{item.delta.after}</span> : null}
            </div>
            <div className="flex flex-wrap gap-x-3 gap-y-1">
              {item.artifactRefs.map((ref) => <IdRef key={ref} id={ref} kind="generic" plain />)}
              {item.semanticRevision ? <IdRef id={item.semanticRevision} kind="generic" plain truncate /> : null}
            </div>
          </article>
        ))}
      </div>
    </section>
  );
}
