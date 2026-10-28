import { EmptyState, FreshnessChip, SectionHeader, TableIcon } from "@/components/kit";
import type { ModelLane, ModelLanesReport } from "@/lib/routing";
import { tierRank } from "@/lib/routing";

/**
 * The lane roster — "which mind, at what cost class" (compare → table doctrine,
 * DESIGN.md §4). Reads `compound route model-capabilities --json`: every routed
 * model lane with its tier + cost_class + harness + provider route. Grouped by
 * tier (premium → cheap → free → local-batch) so the operator scans "what am I
 * paying to think" top-down. Tier/cost-class are metadata, not domain or state,
 * so they render as quiet neutral chips — no hue is spent on them (§7.2/§7.7).
 */

const TIER_LABEL: Record<string, string> = {
  premium: "Premium",
  cheap: "Cheap",
  free: "Free",
  "local-batch": "Local batch",
};

const COST_LABEL: Record<string, string> = {
  free: "free",
  "api-marginal": "api-marginal",
  "subscription-fixed": "subscription-fixed",
  local: "local",
};

/** One-line honest gloss of what a cost class means for spend. */
const COST_GLOSS: Record<string, string> = {
  free: "no marginal cost",
  "api-marginal": "pay per token",
  "subscription-fixed": "already paid (flat)",
  local: "your hardware / watts",
};

function CostChip({ costClass }: { costClass: string }) {
  const label = COST_LABEL[costClass] ?? costClass ?? "—";
  return (
    <span
      title={COST_GLOSS[costClass] ?? undefined}
      className="mono inline-flex items-center rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs font-semibold tabular-nums text-muted"
    >
      {label}
    </span>
  );
}

export function LanesTable({ report }: { report: ModelLanesReport | null }) {
  if (!report || report.lanes.length === 0) {
    return (
      <section className="flex flex-col gap-3">
        <SectionHead
          report={report}
          note="The lane roster reads compound route model-capabilities --json."
        />
        <EmptyState>
          No model lanes imported yet. Import a capability set with{" "}
          <code className="mono">compound route import-model-capabilities &lt;model-capabilities.yaml&gt;</code>.
        </EmptyState>
      </section>
    );
  }

  // Group by tier in cost-priority order (premium first), models alpha within.
  const byTier = new Map<string, ModelLane[]>();
  for (const lane of report.lanes) {
    const key = lane.tier || "—";
    (byTier.get(key) ?? byTier.set(key, []).get(key)!).push(lane);
  }
  const tiers = [...byTier.keys()].sort((a, b) => tierRank(a) - tierRank(b) || a.localeCompare(b));

  return (
    <section className="flex flex-col gap-3">
      <SectionHead report={report} />

      <div className="overflow-hidden rounded-xl bg-panel shadow-sm">
        <div className="overflow-x-auto">
          <table className="w-full text-left" style={{ minWidth: "36rem" }}>
            <thead>
              <tr className="text-2xs font-bold uppercase tracking-wide text-faint">
                <th className="pt-3 pr-3 pb-1.5 pl-4 font-bold">Model lane</th>
                <th className="pt-3 pr-3 pb-1.5 font-bold">Cost class</th>
                <th className="pt-3 pr-3 pb-1.5 font-bold">Harness</th>
                <th className="hidden pt-3 pr-3 pb-1.5 font-bold sm:table-cell">Provider route</th>
                <th className="pt-3 pr-4 pb-1.5 pl-3 text-right font-bold">Caps</th>
              </tr>
            </thead>
            {tiers.map((tier) => {
              const lanes = byTier.get(tier)!.sort((a, b) => a.modelId.localeCompare(b.modelId));
              return (
                <tbody key={tier}>
                  {/* Tier tone-band (§9.1): a soft-2 header separates each cost
                      tier without a hairline; tier/cost stay neutral (§7.2). */}
                  <tr>
                    <td colSpan={5} className="bg-soft-2 px-4 pt-2.5 pb-1.5">
                      <span className="text-2xs font-bold uppercase tracking-wide text-muted">
                        {TIER_LABEL[tier] ?? tier}
                      </span>
                      <span className="mono ml-2 text-2xs tabular-nums text-faint">· {lanes.length}</span>
                    </td>
                  </tr>
                  {lanes.map((lane) => (
                    <LaneCells key={lane.modelId} lane={lane} />
                  ))}
                </tbody>
              );
            })}
          </table>
        </div>
      </div>
    </section>
  );
}

/** One lane row — its own <tr> so tier <tbody> groups stay well-formed. */
function LaneCells({ lane }: { lane: ModelLane }) {
  return (
    <tr>
      <td className="border-t border-hair py-2 pr-3 pl-4">
        <span className="mono text-xs break-all text-ink" title={lane.modelId}>
          {lane.modelId || "—"}
        </span>
      </td>
      <td className="border-t border-hair py-2 pr-3">
        <CostChip costClass={lane.costClass} />
      </td>
      <td className="border-t border-hair py-2 pr-3 text-xs text-muted">{lane.harness || "—"}</td>
      <td className="hidden border-t border-hair py-2 pr-3 sm:table-cell">
        <span className="mono text-2xs text-faint" title={lane.providerRoute}>
          {lane.providerRoute || "—"}
        </span>
      </td>
      <td className="border-t border-hair py-2 pr-4 pl-3 text-right text-xs tabular-nums text-muted">
        {lane.capabilityCount}
      </td>
    </tr>
  );
}

function SectionHead({ report, note }: { report: ModelLanesReport | null; note?: string }) {
  return (
    <div className="flex flex-col gap-1">
      <SectionHeader
        tone="route"
        icon={<TableIcon />}
        label="Model lanes"
        right={
          report?.generatedAt ? (
            <FreshnessChip generatedAt={report.generatedAt} source="compound route model-capabilities --json" />
          ) : null
        }
      />
      <p className="text-2xs leading-relaxed text-muted">
        {note ??
          `${report?.modelCount ?? 0} lanes across ${report?.capabilityClasses.length ?? 0} capability ${
            (report?.capabilityClasses.length ?? 0) === 1 ? "class" : "classes"
          } — the mind routing can pick, by cost class.`}
      </p>
    </div>
  );
}
