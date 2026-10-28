"use client";

import { CopyCmd } from "@/components/CopyCmd";
import { AuthorityFaceDetails } from "@/components/authority/AuthorityFaceDetails";
import { GovernedAct } from "@/components/decide/GovernedAct";
import { ArrowUpIcon, ClockIcon, EmptyState, FreshnessChip, IdRef, SectionHeader, StateBadge } from "@/components/kit";
import type { DecideGroup } from "@/lib/decide-model";
import { manualCopyCommandFor } from "@/lib/decide-resolution-acts";
import { decideActIdentity } from "@/lib/governed-action-client";

/**
 * The route promotion/remorse gate — "should a lane change?" Renders the same
 * DecideGroup model the Decide queue builds (via decide-model's resolver), so
 * the bespoke `route-promotions` Record-hold act is governed identically:
 * every mutation is one human click through /api/decision/execute, confirmed,
 * with its rationale written verbatim as provenance. Cards that expose no safe
 * one-click (a promote/demote lane change needs a separate authority review)
 * fall through to an honest copy-command — never a fabricated button.
 *
 * Two sub-bands, honest about attention band:
 *   - "Needs your call" — the needs-you route groups (amber admission seam,
 *     Class-A framing when a change is irreversible or high-blast).
 *   - "Record a hold"   — watching route groups where evidence is still thin;
 *     the operator MAY record a hold (reversible, changes no lane) or leave them
 *     collecting. No amber seam — these do not block the seam.
 */

const ROUTE_TILE = "border-route-line bg-route-soft text-route";

function GlyphTile({ glyph }: { glyph: string }) {
  return (
    <span
      aria-hidden
      className={`inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-sm border text-xs font-bold ${ROUTE_TILE}`}
    >
      {glyph}
    </span>
  );
}

function ConsequencePair({ approve, hold }: { approve: string; hold: string }) {
  return (
    <div className="mt-2 rounded-md bg-soft px-3 py-2 text-xs leading-relaxed">
      <div className="text-ink">
        <span className="font-semibold text-good">if you act</span>
        <span className="text-faint"> → </span>
        {approve}
      </div>
      <div className="mt-0.5 text-muted">
        <span className="font-semibold">if you wait</span>
        <span> → </span>
        {hold}
      </div>
    </div>
  );
}

function RouteCard({
  group,
  seam,
  mutationsEnabled,
}: {
  group: DecideGroup;
  seam: boolean;
  mutationsEnabled: boolean;
}) {
  const { act } = group;
  const manualCopyCommand = manualCopyCommandFor(group);
  return (
    <article
      className={`rounded-lg border border-hair bg-panel px-4 py-3.5 shadow-sm transition-shadow hover:shadow ${
        seam ? "admission-seam" : ""
      }`}
    >
      <div className="flex items-start justify-between gap-2">
        <div className="flex min-w-0 items-start gap-2">
          <GlyphTile glyph={group.glyph} />
          <div className="min-w-0">
            <div className="flex items-center gap-1.5">
              <span className="text-2xs font-bold uppercase tracking-wide text-faint" title={group.areaPlain}>
                {group.areaLabel}
              </span>
              {group.count > 1 ? <span className="mono text-2xs tabular-nums text-faint">· {group.count}</span> : null}
            </div>
            <h3 className="mt-0.5 text-md font-semibold leading-snug text-ink">{group.title}</h3>
          </div>
        </div>
        <StateBadge state={seam ? "needs_you" : "watching"} label={seam ? "needs you" : "watching"} />
      </div>

      {group.capability.kind !== "unavailable" && group.unlock ? (
        <div className="mt-1 text-xs font-medium text-good">→ {group.unlock}</div>
      ) : null}
      {group.body ? (
        <p className="mt-2 text-sm leading-relaxed text-muted" style={{ maxWidth: "var(--read-max)" }}>
          {group.body}
        </p>
      ) : null}
      {group.consequences ? <ConsequencePair approve={group.consequences.approve} hold={group.consequences.hold} /> : null}

      {group.face.authority ? (
        <AuthorityFaceDetails authority={group.face.authority} embedded className="mt-3" />
      ) : null}

      <div className="mt-3 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className={`text-xs tabular-nums ${group.old ? "font-semibold text-seam" : "text-faint"}`}>
            {group.ageLabel}
            {group.old ? " · stale" : ""}
          </span>
            <FreshnessChip generatedAt={group.face.freshness.generatedAt} source={group.face.freshness.source} />
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {act.mode === "copy" ? (
            <div className="flex flex-col items-end gap-1">
              {manualCopyCommand ? <CopyCmd cmd={manualCopyCommand} compact /> : null}
              <span className="text-2xs text-faint">
                {group.authorityBound ? group.face.unavailableReason : "No one-click action wired yet"}
                {!group.authorityBound && act.reason ? ` — ${act.reason}` : ""}.
              </span>
            </div>
          ) : act.mode === "batch-enumerable" ? (
            <span className="text-2xs text-faint">Per-member apply — open in the Decide queue.</span>
          ) : (
            <GovernedAct
              key={decideActIdentity(act)}
              act={act}
              authority={group.face.authority}
              mutationsEnabled={mutationsEnabled}
              effectKind={group.capability.kind === "unavailable" ? "apply" : group.capability.kind}
            />
          )}
        </div>
      </div>

      {group.evidence.length ? (
        <details className="mt-3 rounded-md border border-hair bg-soft px-3 py-2 text-xs text-muted">
          <summary className="cursor-pointer font-medium text-muted">Evidence · {group.evidence.length}</summary>
          <ul className="mt-2 flex flex-col gap-0.5">
            {group.evidence.map((evidence, i) => (
              <li key={i} className="truncate text-2xs text-faint" title={evidence.ref}>
                {evidence.label}
              </li>
            ))}
          </ul>
        </details>
      ) : null}

      <div className="mt-2">
        <IdRef id={group.sampleId} kind="generic" truncate />
      </div>
    </article>
  );
}

export function PromotionBand({
  needsYou,
  holdRecord,
  classA,
  mutationsEnabled,
}: {
  needsYou: DecideGroup[];
  holdRecord: DecideGroup[];
  classA: boolean;
  mutationsEnabled: boolean;
}) {
  return (
    <section className="flex flex-col gap-4">
      <SectionHeader
        tone="route"
        icon={<ArrowUpIcon />}
        label="Promotion reviews"
        right={
          <span className="text-xs tabular-nums text-faint">
            {needsYou.length
              ? `${needsYou.length} need${needsYou.length === 1 ? "s" : ""} your call`
              : "nothing needs your call"}
          </span>
        }
      />

      {classA ? (
        <div className="rounded-lg border border-crit-line bg-crit-soft px-4 py-3">
          <div className="text-2xs font-bold uppercase tracking-wide text-crit">Class A · irreversible gate</div>
          <p className="mt-1 text-xs leading-relaxed text-crit">
            A change below is one-way or high-blast. Read the route evidence before you record — this is not a reversible
            hold.
          </p>
        </div>
      ) : null}

      {needsYou.length ? (
        <div className="flex flex-col gap-3">
          {needsYou.map((group) => (
            <RouteCard key={group.key} group={group} seam mutationsEnabled={mutationsEnabled} />
          ))}
        </div>
      ) : (
        <EmptyState>Clean seam. No lane change waits on your call.</EmptyState>
      )}

      {holdRecord.length ? (
        <div className="flex flex-col gap-3">
          <SectionHeader
            tone="neutral"
            icon={<ClockIcon />}
            label="Record a hold — evidence still thin"
            right={<span className="mono text-2xs tabular-nums text-faint">{holdRecord.length}</span>}
          />
          <p className="text-2xs leading-relaxed text-faint">
            Not enough route evidence to change these lanes yet. Recording a hold marks that you reviewed it — it changes
            no lane and is reversible. Or leave them collecting.
          </p>
          {holdRecord.map((group) => (
            <RouteCard key={group.key} group={group} seam={false} mutationsEnabled={mutationsEnabled} />
          ))}
        </div>
      ) : null}
    </section>
  );
}
