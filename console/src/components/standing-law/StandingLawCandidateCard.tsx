import Link from "next/link";

import { standingLawReadinessChip, type MemoryCandidate } from "@/lib/memory-data";
import { ago } from "@/lib/time";

/*
 * Standing-law-candidate card — rendered on the /standing-law page so the
 * operator sees the same "needs your call" cards the /memory queue shows,
 * scoped to the standing-law area. The card is read-only here (the actual
 * adjudicate act lives on /memory, where the governed end-point and form
 * fields live); this view exists so the readiness chip + restatement live
 * next to the standing law the candidate is meant to become.
 *
 * The chip (`standingLawReadinessChip`) is a derived-only signal projected
 * by `compound memory candidates --json` — it is a cue, not a promotion
 * criterion. Promotion remains the governed memory-adjudicate act on
 * /memory.
 */

const CHIP_TONE: Record<"seam" | "good" | "muted", string> = {
  seam: "border-seam-line bg-seam-soft text-seam",
  good: "border-good-line bg-good-soft text-good",
  muted: "border-hair bg-soft-2 text-muted",
};

function headLine(candidate: MemoryCandidate): string {
  const statement = String(
    candidate.statement ?? candidate.subject ?? candidate.object ?? candidate.entity ?? candidate.id,
  ).trim();
  return statement || candidate.id;
}

function subLine(candidate: MemoryCandidate): string {
  const entity = String(candidate.entity ?? "").trim();
  const predicate = String(candidate.predicate ?? "").trim();
  if (entity && predicate) return `${entity} · ${predicate}`;
  if (entity) return entity;
  return "";
}

export function StandingLawCandidateCard({ candidate }: { candidate: MemoryCandidate }) {
  const headline = headLine(candidate);
  const sub = subLine(candidate);
  const chip = standingLawReadinessChip(candidate);
  const createdAt = String(candidate.created_at ?? "");
  const age = createdAt ? ago(createdAt) : "age unknown";
  const recurrence =
    typeof candidate.recurrence_count === "number" && Number.isFinite(candidate.recurrence_count)
      ? candidate.recurrence_count
      : null;
  const confidence =
    typeof candidate.confidence === "number" && Number.isFinite(candidate.confidence)
      ? candidate.confidence
      : null;

  return (
    <article className="admission-seam flex flex-col gap-2 rounded-lg border border-hair bg-panel px-4 py-3 shadow-sm">
      <div className="flex items-start justify-between gap-2">
        <div className="flex min-w-0 flex-col gap-0.5">
          <span className="text-2xs font-bold uppercase tracking-wide text-faint">
            Standing law · candidate
          </span>
          <h3 className="text-md font-semibold leading-snug text-ink">{headline}</h3>
          {sub ? <div className="mono text-2xs text-faint">{sub}</div> : null}
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1">
          <span className="rounded-pill border border-hair bg-transparent px-2.5 py-0.5 text-xs font-semibold text-muted">
            parked
          </span>
          {chip ? (
            <span
              title="derived-only signal — promotion stays the governed memory-adjudicate act"
              className={`inline-flex items-center rounded-pill border px-2 py-0.5 text-2xs font-semibold tabular-nums ${CHIP_TONE[chip.tone]}`}
            >
              {chip.label}
            </span>
          ) : null}
        </div>
      </div>

      <div className="mono flex flex-wrap items-center gap-x-3 gap-y-1 text-2xs text-faint">
        <span className="tabular-nums">{age}</span>
        {recurrence !== null ? (
          <span className="tabular-nums" title="distinct evidence refs citing the same normalized claim">
            recurrence {recurrence}
          </span>
        ) : null}
        {confidence !== null ? (
          <span className="tabular-nums" title="model-extracted confidence">
            conf {confidence.toFixed(2)}
          </span>
        ) : null}
      </div>

      <div className="flex flex-wrap items-center justify-between gap-2 border-t border-hair pt-2">
        <Link
          href="/memory"
          className="inline-flex items-center gap-1.5 text-2xs font-medium text-seam hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
        >
          Adjudicate on /memory
          <span aria-hidden>→</span>
        </Link>
        <code className="mono truncate text-2xs text-faint" title={candidate.id}>
          {candidate.id}
        </code>
      </div>
    </article>
  );
}
