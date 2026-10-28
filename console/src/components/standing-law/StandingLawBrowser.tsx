"use client";

import { useMemo, useState } from "react";

import { EmptyState } from "@/components/kit";
import { filterStandingLaw, type StandingLawKind, type StandingLawRecord } from "@/lib/standing-law";

/*
 * The searchable standing-law list (slice B). Client-side so the <4-interaction
 * journey holds: land on the page → type → see the ruling (3 interactions, no
 * round-trip). Each row is the plain restatement (the record title) + a kind
 * badge + date + corroboration count; expanding one row discloses the verbatim
 * provenance, the supersession chain (dimmed, struck-through — never live law),
 * the falsifier + review trigger, and the corroboration evidence list.
 */

const KIND_LABEL: Record<string, string> = {
  ruling: "ruling",
  grant: "grant",
  freeze: "freeze",
  policy: "policy",
};

function KindBadge({ kind }: { kind: StandingLawKind }) {
  const tone =
    kind === "grant"
      ? "border-good-line bg-good-soft text-good"
      : kind === "freeze"
        ? "border-work-line bg-work-soft text-work"
        : kind === "ruling"
          ? "border-seam-line bg-seam-soft text-seam"
          : "border-hair bg-soft-2 text-muted";
  return (
    <span
      className={`inline-flex items-center rounded-pill border px-2 py-0.5 text-2xs font-bold uppercase tracking-wide ${tone}`}
    >
      {KIND_LABEL[kind] ?? kind}
    </span>
  );
}

function DisclosureRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1 sm:flex-row sm:gap-3">
      <span className="w-32 shrink-0 text-2xs font-bold uppercase tracking-wide text-faint">{label}</span>
      <div className="min-w-0 flex-1 text-xs text-muted">{children}</div>
    </div>
  );
}

function RecordCard({
  record,
  expanded,
  onToggle,
}: {
  record: StandingLawRecord;
  expanded: boolean;
  onToggle: () => void;
}) {
  const date = record.date ? record.date.slice(0, 10) : "";
  return (
    <div className="card flex flex-col gap-3 p-4">
      <div className="flex items-start gap-3">
        <KindBadge kind={record.kind} />
        <button
          type="button"
          onClick={onToggle}
          aria-expanded={expanded}
          className="min-w-0 flex-1 text-left"
        >
          <div className="text-sm font-semibold leading-snug text-ink">{record.title || record.decisionId}</div>
          <div className="mono mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-2xs text-faint">
            {date ? <span className="tabular-nums">{date}</span> : null}
            {record.scope ? <span>scope: {record.scope}</span> : null}
            <span className="tabular-nums" title="restatements of this ruling recorded as corroboration">
              {record.corroborationCount} corroboration{record.corroborationCount === 1 ? "" : "s"}
            </span>
            {record.supersedes.length ? (
              <span className="text-muted">supersedes {record.supersedes.length}</span>
            ) : null}
          </div>
        </button>
        <button
          type="button"
          onClick={onToggle}
          aria-label={expanded ? "Collapse details" : "Expand details"}
          className="shrink-0 rounded-md border border-hair px-2 py-0.5 text-2xs text-muted hover:bg-soft"
        >
          {expanded ? "hide" : "details"}
        </button>
      </div>

      {expanded ? (
        <div className="flex flex-col gap-3 border-t border-hair pt-3">
          {record.supersedes.length ? (
            <DisclosureRow label="supersedes">
              <ul className="flex flex-col gap-1">
                {record.supersedes.map((sup) => (
                  <li key={sup.decisionId} className="flex items-center gap-2">
                    <span className="text-faint line-through">{sup.title || sup.decisionId}</span>
                    <span className="mono text-2xs text-faint">({sup.decisionId})</span>
                    <span className="rounded-pill border border-hair bg-soft px-1.5 text-2xs text-faint">
                      superseded
                    </span>
                  </li>
                ))}
              </ul>
            </DisclosureRow>
          ) : null}

          {record.falsifier ? <DisclosureRow label="falsifier">{record.falsifier}</DisclosureRow> : null}
          {record.reviewTrigger ? (
            <DisclosureRow label="review trigger">{record.reviewTrigger}</DisclosureRow>
          ) : null}

          <DisclosureRow label="provenance">
            <div className="flex flex-col gap-0.5">
              {record.reviewedBy ? <span>reviewed by {record.reviewedBy}</span> : null}
              {record.sourcePath ? (
                <span className="mono break-all text-2xs text-faint">{record.sourcePath}</span>
              ) : (
                <span className="text-faint">no source path recorded</span>
              )}
              <span className="mono text-2xs text-faint">{record.claimId}</span>
            </div>
          </DisclosureRow>

          {record.corroborationCount ? (
            <DisclosureRow label="corroboration">
              <div className="flex flex-wrap gap-1.5">
                {record.corroborationEvidence.length ? (
                  record.corroborationEvidence.map((ev) => (
                    <span key={ev} className="mono rounded-md border border-hair bg-soft px-1.5 py-0.5 text-2xs text-muted">
                      {ev}
                    </span>
                  ))
                ) : (
                  <span className="text-faint">{record.corroborationCount} restatement(s) recorded</span>
                )}
              </div>
            </DisclosureRow>
          ) : null}

          {record.keywords.length || record.domains.length ? (
            <DisclosureRow label="keywords">
              {[...record.keywords, ...record.domains].join(", ")}
            </DisclosureRow>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

export function StandingLawBrowser({ records }: { records: StandingLawRecord[] }) {
  const [query, setQuery] = useState("");
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  const filtered = useMemo(() => filterStandingLaw(records, query), [records, query]);

  function toggle(id: string) {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search standing law — did I already rule this?"
          aria-label="Search standing law"
          className="w-full max-w-md rounded-lg border border-hair bg-panel px-3 py-1.5 text-sm text-ink outline-none placeholder:text-faint focus:border-ink/40 sm:w-80"
        />
        <span className="mono text-2xs text-faint tabular-nums">
          {filtered.length}/{records.length}
        </span>
      </div>

      {filtered.length ? (
        <div className="flex flex-col gap-2.5">
          {filtered.map((record) => (
            <RecordCard
              key={record.claimId || record.decisionId}
              record={record}
              expanded={expanded.has(record.claimId || record.decisionId)}
              onToggle={() => toggle(record.claimId || record.decisionId)}
            />
          ))}
        </div>
      ) : records.length ? (
        <EmptyState>No standing law matches “{query.trim()}”. Clear the search to see every ruling.</EmptyState>
      ) : (
        <EmptyState>
          No accepted rulings, grants, or freezes yet. Settled law recorded via a decision record shows up here.
        </EmptyState>
      )}
    </div>
  );
}
