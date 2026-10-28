import { DeltaPill, GearIcon } from "@/components/kit";
import type { AttentionReport } from "@/lib/attention";
import { FeedDegraded } from "@/components/kit/FeedDegraded";
import { num, type ProjectionRead } from "@/lib/compound";

import { buildCoverage, type AreaCoverage } from "./data";
import { Card, CountChip, DoctrineNote, KV, SettingsSection } from "./primitives";

const COVERAGE_TONE: Record<AreaCoverage, string> = {
  bespoke: "text-good",
  generic: "text-work",
  fallback: "text-seam",
};

const COVERAGE_LABEL: Record<AreaCoverage, string> = {
  bespoke: "server adapter (bespoke)",
  generic: "server adapter (generic)",
  fallback: "copy-only (no adapter)",
};

/**
 * Group 6 — Console & environment. Live mutation-gate booleans, and THE
 * transparency table: every wired bespoke area + every generic-certified verb
 * cross-referenced against the live review areas, so the operator sees exactly
 * which areas have server-adapter coverage vs an honest copy-only fallback.
 * This is backend coverage, not a claim that the standard browser may invoke it.
 */
export function ConsoleEnv({
  mutationsEnabled,
  mutationFlagEnabled,
  instancePinned,
  standardClientAuthorized,
  privateFallbacks,
  attention,
  attentionRead,
  generatedAt,
}: {
  mutationsEnabled: boolean;
  mutationFlagEnabled: boolean;
  instancePinned: boolean;
  standardClientAuthorized: boolean;
  privateFallbacks: boolean;
  attention: AttentionReport | null;
  attentionRead: ProjectionRead;
  generatedAt?: string;
}) {
  const coverage = buildCoverage(attention);
  const raw = attentionRead.value;
  const coverageKnown = attention && raw?.schema_version === "compound.attention.v0.1" &&
    Array.isArray(raw.groups) && ["needs_you_groups", "actionable_now", "fallback_groups"].every((key) => {
      const value = (raw.counts as Record<string, unknown> | undefined)?.[key];
      return Number.isInteger(value) && Number(value) >= 0;
    });
  const needsYouGroups = num(attention?.counts?.needs_you_groups);
  const serverActionable = num(attention?.counts?.actionable_now);
  const fallbackGroups = num(attention?.counts?.fallback_groups);

  return (
    <SettingsSection
      index={6}
      icon={<GearIcon />}
      tone="neutral"
      title="Console & environment"
      question="Can this console mutate, and which review areas have server-adapter coverage?"
      source="compound attention --json"
      generatedAt={attentionRead.status === "live" && coverageKnown ? generatedAt : undefined}
    >
      <div className="grid gap-4 md:grid-cols-2">
        <Card title="Mutation gates" provenance="local" seamTone={mutationsEnabled ? "seam" : "good"}>
          <KV k="effective readiness" v={mutationsEnabled ? "ready" : "read-only"} />
          <KV k="mutation flag" v={mutationFlagEnabled ? "enabled" : "disabled"} />
          <KV k="instance pin" v={instancePinned ? "explicit" : "missing"} />
          <KV k="standard client" v={standardClientAuthorized ? "authorized" : "projection-only"} />
          <KV k="privateFallbacksEnabled()" v={privateFallbacks ? "true" : "false"} />
          <DoctrineNote>
            <span className="mt-2 block">
              {mutationsEnabled
                ? "Mutations are enabled — governed acts run through /api/decision/execute behind the argv allowlist. Every act is still a human click (DEC-0021: nothing auto-runs)."
                : "Read-only. The standard browser never receives the server capability; unsupported API extensions require an explicit home, opt-in flag, and out-of-band header."}
            </span>
          </DoctrineNote>
        </Card>

        <Card title="Coverage burn-down">
          {!coverageKnown || attentionRead.status === "unreachable" ? (
            <FeedDegraded label="Coverage" source="compound attention --json" lastGoodAt={attentionRead.lastGoodAt} />
          ) : (
          <>
          {attentionRead.status === "stale" ? <p className="mb-2 text-sm text-seam">Last-known coverage; current gaps are unconfirmed.</p> : null}
          <div className="flex flex-wrap gap-2">
            <CountChip n={needsYouGroups} label="need you" tone={needsYouGroups > 0 ? "seam" : "good"} title="counts.needs_you_groups" />
            <CountChip
              n={serverActionable}
              label="server-ready"
              tone="muted"
              title="counts.actionable_now — server-side action candidates; the standard browser remains projection-only"
            />
            <CountChip n={fallbackGroups} label="fallback" tone={fallbackGroups > 0 ? "seam" : "good"} title="counts.fallback_groups" />
          </div>
          <DoctrineNote>
            <span className="mt-3 block">
              {fallbackGroups > 0
                ? `${fallbackGroups.toLocaleString("en-US")} group${fallbackGroups === 1 ? "" : "s"} still lack a server adapter and fall back to copy-only.`
                : "No server-adapter gaps. Every live area resolves to a wired server act; the standard browser remains projection-only."}
            </span>
          </DoctrineNote>
          </>
          )}
        </Card>
      </div>

      {/* THE transparency table. */}
      {coverageKnown && attentionRead.status !== "unreachable" ? (
      <Card
        title="Server-adapter coverage"
        badge={
          <span className="flex items-center gap-1.5">
            <CountChip n={coverage.serverAdapterAreas} label={attentionRead.status === "stale" ? "last-known adapters" : "live adapters"} tone="good" />
            <CountChip n={coverage.fallbackAreas} label={attentionRead.status === "stale" ? "last-known fallback" : "live fallback"} tone={coverage.fallbackAreas > 0 ? "seam" : "good"} />
            {/* Burn-down lead: server-adapter areas minus fallback areas —
                positive means adapter coverage leads copy-only debt. */}
            <DeltaPill
              value={coverage.serverAdapterAreas - coverage.fallbackAreas}
              showZero
              title="live server-adapter areas minus live fallback areas — the coverage lead over copy-only debt"
            />
          </span>
        }
      >
        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs">
            <thead>
              <tr className="border-b border-hair text-2xs uppercase tracking-wide text-faint">
                <th className="py-1.5 pr-3 font-semibold">area</th>
                <th className="py-1.5 pr-3 font-semibold">bespoke</th>
                <th className="py-1.5 pr-3 font-semibold">generic</th>
                <th className="py-1.5 pr-3 font-semibold">live</th>
                <th className="py-1.5 font-semibold">coverage</th>
              </tr>
            </thead>
            <tbody className="mono tabular-nums">
              {coverage.rows.map((row) => (
                <tr key={row.area} className="border-b border-hair last:border-b-0">
                  <td className="py-1.5 pr-3 text-ink" title={row.source ? `source: ${row.source}` : undefined}>
                    {row.area}
                  </td>
                  <td className="py-1.5 pr-3 text-muted">{row.bespoke ? "✓" : "—"}</td>
                  <td className="py-1.5 pr-3 text-muted">{row.generic ? "✓" : "—"}</td>
                  <td className="py-1.5 pr-3 text-muted">{row.live ? row.liveCount.toLocaleString("en-US") : "—"}</td>
                  <td className={`py-1.5 ${COVERAGE_TONE[row.coverage]}`}>{COVERAGE_LABEL[row.coverage]}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="mt-3 grid gap-3 border-t border-hair pt-3 md:grid-cols-2">
          <div>
            <span className="mb-1.5 block text-2xs font-semibold uppercase tracking-wide text-faint">
              bespoke specs ({coverage.bespokeAreas.length})
            </span>
            <div className="flex flex-wrap gap-1">
              {coverage.bespokeAreas.map((b) => (
                <span
                  key={b.area}
                  title={`source: ${b.source}`}
                  className="mono rounded-pill border border-good-line bg-good-soft px-2 py-0.5 text-2xs text-good"
                >
                  {b.area}
                </span>
              ))}
            </div>
          </div>
          <div>
            <span className="mb-1.5 block text-2xs font-semibold uppercase tracking-wide text-faint">
              generic-certified ({coverage.genericAreas.length})
            </span>
            <div className="flex flex-wrap gap-1">
              {coverage.genericAreas.map((area) => (
                <span
                  key={area}
                  className="mono rounded-pill border border-work-line bg-work-soft px-2 py-0.5 text-2xs text-work"
                >
                  {area}
                </span>
              ))}
            </div>
          </div>
        </div>
        <div className="mt-3">
          <DoctrineNote>
            Adding a bespoke server adapter for a fallback area is one registry entry (REGISTRY in
            decision-registry.ts); certifying a generic verb is one allowlist row (GENERIC_ACTION_ALLOWLIST in
            generic-actions.ts). This table records server coverage only; it does not authorize the standard client.
          </DoctrineNote>
        </div>
      </Card>
      ) : null}
    </SettingsSection>
  );
}
