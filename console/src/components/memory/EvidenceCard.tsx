import { ago } from "@/lib/time";
import { IdRef } from "@/components/kit";
import type { MemoryCandidate } from "@/lib/memory-data";
import { buildEvidenceCardObservation, evidenceProvenanceGapNotice } from "@/lib/evidence-card";

/*
 * A source-evidence row (knowledge imports / extractions). These stay OUT of
 * the adjudication queue on purpose — they are evidence, not approval debt,
 * and only earn canon by being cited into a promoted claim. The card renders
 * the pure view from buildEvidenceCardObservation: observation first,
 * provenance separate from record identity, raw values behind a labeled
 * disclosure, and no invented verification.
 */

export function EvidenceCard({ candidate }: { candidate: MemoryCandidate }) {
  const view = buildEvidenceCardObservation(candidate);

  return (
    <div className="rounded-lg border border-hair bg-panel px-4 py-3">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="inline-flex items-center rounded-pill border border-memory-line bg-memory-soft px-2 py-0.5 text-2xs font-semibold text-memory">
              {view.kindLabel}
            </span>
            {view.sourceLabels.map((label) => (
              <span
                key={label}
                className="inline-flex items-center rounded-pill border border-hair bg-soft-2 px-2 py-0.5 text-2xs font-semibold text-muted"
              >
                {label}
              </span>
            ))}
            {view.metadataField ? (
              <span className="inline-flex items-center rounded-pill border border-hair bg-soft-2 px-2 py-0.5 text-2xs font-semibold text-muted">
                {view.metadataField}
              </span>
            ) : null}
          </div>
          <h3 className="mt-2 line-clamp-2 text-sm font-semibold leading-snug text-ink">
            {view.observation}
          </h3>
        </div>
        {view.observedAt ? (
          <span className="shrink-0 text-2xs tabular-nums text-faint">{ago(view.observedAt)}</span>
        ) : null}
      </div>

      <p className="mt-2 text-2xs leading-relaxed text-muted">{view.note}</p>

      {view.provenanceMissing ? (
        <p className="mt-1 text-2xs leading-relaxed text-crit">{evidenceProvenanceGapNotice}</p>
      ) : null}

      <div className="mt-2 flex items-center justify-between gap-2">
        <details className="min-w-0">
          <summary className="cursor-pointer text-2xs font-medium text-faint hover:text-muted">
            Sources and details · {view.refs.length} source references
          </summary>
          <div className="mt-2"><IdRef id={candidate.id} kind="candidate" /></div>
          {view.refs.length > 0 ? (
            <ul className="mt-1 flex flex-col gap-0.5">
              {view.refs.map((ref) => (
                <li key={ref} className="mono break-all text-2xs text-faint">
                  {ref}
                </li>
              ))}
            </ul>
          ) : (
            <p className="mono mt-1 text-2xs text-crit">{evidenceProvenanceGapNotice}</p>
          )}
          <div className="mt-2 flex flex-col gap-0.5 border-t border-hair pt-1.5">
            <p className="text-2xs font-medium text-faint">technical details</p>
            {view.technicalDetails.map((detail) => (
              <p key={detail.label} className="mono whitespace-pre-wrap break-all text-2xs text-muted">
                {detail.label}: {detail.value}
              </p>
            ))}
          </div>
        </details>
      </div>
    </div>
  );
}
