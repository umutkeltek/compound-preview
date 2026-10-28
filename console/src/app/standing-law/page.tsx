import Link from "next/link";

import { StandingLawBrowser } from "@/components/standing-law/StandingLawBrowser";
import { StandingLawCandidateCard } from "@/components/standing-law/StandingLawCandidateCard";
import { EmptyState, FreshnessChip, GavelIcon, PageHeader, SectionHeader } from "@/components/kit";
import { runJson, type Json } from "@/lib/compound";
import { loadStandingLawCandidates } from "@/lib/memory-data";
import { buildStandingLaw, type StandingLawConflict } from "@/lib/standing-law";

export const dynamic = "force-dynamic";

/*
 * Standing law — the operator-facing twin of what orient already serves agents.
 * "Did I already rule this?" used to mean grepping worklogs, so settled law got
 * re-litigated every session. This read-only view lists every ACCEPTED
 * ruling/grant/freeze, newest-first and searchable, with verbatim provenance +
 * the supersession chain on disclosure. Contradictions (parked harvest
 * candidates that reverse accepted law) render as a loud strip at the top.
 *
 * Read-only: no ratify/record/adjudicate here. Ratification stays the
 * operator-provenance path; this view never accepts anything.
 */

async function loadStandingLaw() {
  const data = (await runJson(["decisions", "standing-law", "--json"])) as Json | null;
  return buildStandingLaw(data);
}

function ConflictStrip({ conflicts }: { conflicts: StandingLawConflict[] }) {
  if (!conflicts.length) return null;
  return (
    <section className="flex flex-col gap-2">
      <SectionHeader
        tone="crit"
        icon={<GavelIcon />}
        label="Contradictions"
        sub="a spoken ruling reverses accepted law — resolve before it re-litigates"
        right={
          <span className="mono rounded-pill border border-crit-line bg-crit-soft px-2 py-0.5 text-2xs font-semibold text-crit tabular-nums">
            {conflicts.length}
          </span>
        }
      />
      <div className="flex flex-col gap-2">
        {conflicts.map((c) => (
          <div
            key={c.candidateId}
            className="flex flex-col gap-2 rounded-lg border border-crit-line bg-crit-soft/60 p-4"
          >
            <div className="text-sm font-semibold leading-snug text-ink">{c.restatement || c.verbatim}</div>
            {c.verbatim ? (
              <blockquote className="border-l-2 border-crit-line pl-3 text-xs italic text-muted">
                “{c.verbatim}”
              </blockquote>
            ) : null}
            <div className="mono flex flex-wrap items-center gap-x-3 gap-y-1 text-2xs text-faint">
              {c.conflictingTitle ? (
                <span className="not-italic text-crit">
                  conflicts with: <span className="font-semibold">{c.conflictingTitle}</span>
                </span>
              ) : null}
              {c.conflictsWithClaim ? <span>{c.conflictsWithClaim}</span> : null}
              {c.session ? <span>session {c.session}</span> : null}
              {c.line ? <span>L{c.line}</span> : null}
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

export default async function StandingLaw() {
  const [model, lawCandidates] = await Promise.all([loadStandingLaw(), loadStandingLawCandidates()]);
  const waiting = lawCandidates.length;

  return (
    <div className="flex flex-col gap-8">
      <PageHeader
        breadcrumb="know / standing law"
        title="Standing law"
        right={
          <>
            <span className="inline-flex items-center gap-1.5 rounded-pill border border-hair bg-soft-2 px-2.5 py-0.5 text-xs font-semibold text-muted">
              <span className="mono tabular-nums">{model.counts.records}</span> in force
            </span>
            <FreshnessChip source="compound decisions standing-law --json" />
          </>
        }
      />
      <p className="text-sm text-muted" style={{ maxWidth: "var(--read-max)" }}>
        Every accepted ruling, grant, and freeze — the same standing law orient serves agents. Search it
        before you re-decide. Superseded records are kept for provenance but shown struck-through under their
        successor, never as live law.
      </p>

      {waiting > 0 ? (
        <Link
          href="/memory"
          className="inline-flex items-center gap-2 rounded-lg border border-seam-line bg-seam-soft px-4 py-2.5 text-sm font-semibold text-seam transition-colors hover:bg-seam-soft/70 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
        >
          <span className="mono tabular-nums">{waiting}</span>
          {waiting === 1
            ? " candidate is waiting for your call"
            : " candidates are waiting for your call"}
          <span aria-hidden>→</span>
        </Link>
      ) : null}

      <ConflictStrip conflicts={model.conflicts} />

      <section className="flex flex-col gap-3">
        <SectionHeader
          tone="seam"
          icon={<GavelIcon />}
          label="In force"
          sub="accepted rulings · grants · freezes — newest first"
          right={
            <span className="mono text-2xs text-faint">
              {model.counts.rulings}R · {model.counts.grants}G · {model.counts.freezes}F
            </span>
          }
        />
        <StandingLawBrowser records={model.records} />
      </section>

      <section className="flex flex-col gap-3">
        <SectionHeader
          tone="seam"
          icon={<GavelIcon />}
          label="Candidates awaiting your call"
          sub="parked decision candidates — chip is a derived readiness signal, promotion stays governed"
          right={
            <span className="mono text-2xs text-faint">
              {lawCandidates.length} waiting
            </span>
          }
        />
        {lawCandidates.length ? (
          <div className="flex flex-col gap-2.5">
            {lawCandidates.map((candidate) => (
              <StandingLawCandidateCard key={candidate.id} candidate={candidate} />
            ))}
          </div>
        ) : (
          <EmptyState>No parked decision candidates are waiting for a ruling.</EmptyState>
        )}
      </section>
    </div>
  );
}
