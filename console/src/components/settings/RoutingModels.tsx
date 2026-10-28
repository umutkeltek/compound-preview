import Link from "next/link";

import { num, type Json } from "@/lib/compound";

import { RouteIcon } from "@/components/kit";

import { Card, CountChip, DoctrineNote, KV, SettingsSection } from "./primitives";

/**
 * Group 3 — Routing & models. A read-only lane/route-evidence summary plus the
 * promotion-review backlog count, then a deep-link into the full Routing & cost
 * screen (a parallel lane owns that page). Deliberately light: this group
 * orients, it does not duplicate the routing surface.
 */
export function RoutingModels({
  routeEvidence,
  promotionCount,
  generatedAt,
}: {
  routeEvidence: Json | null;
  promotionCount: number;
  generatedAt?: string;
}) {
  const evidenceCount = num(routeEvidence?.route_evidence_count);
  const providerLedger = num(routeEvidence?.provider_ledger_evidence_count);
  const totalTokens = num(routeEvidence?.total_tokens);
  const costMicros = num(routeEvidence?.total_cost_micros);
  const statusCounts = (routeEvidence?.status_counts ?? {}) as Record<string, number>;
  const providerCounts = (routeEvidence?.provider_counts ?? {}) as Record<string, number>;
  const providers = Object.entries(providerCounts).sort((a, b) => b[1] - a[1]);

  return (
    <SettingsSection
      index={3}
      icon={<RouteIcon />}
      tone="route"
      title="Routing & models"
      question="Is routing sending the right mind at the right cost, and what wants promoting?"
      source="compound route evidence-authority --json"
      generatedAt={generatedAt}
    >
      <div className="grid gap-4 md:grid-cols-2">
        <Card title="Route evidence" badge={<CountChip n={evidenceCount} label="routes" tone="muted" />}>
          <KV k="route_evidence_count" v={evidenceCount.toLocaleString("en-US")} />
          <KV k="provider_ledger_rows" v={providerLedger} />
          <KV k="total_tokens" v={totalTokens.toLocaleString("en-US")} />
          <KV k="total_cost" v={`$${(costMicros / 1_000_000).toFixed(6)}`} />
          <div className="mt-2 flex flex-wrap gap-1.5">
            {Object.entries(statusCounts).map(([status, count]) => (
              <span
                key={status}
                className="mono rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs tabular-nums text-muted"
              >
                {status} · {count}
              </span>
            ))}
          </div>
        </Card>

        <Card
          title="Promotion reviews"
          badge={<CountChip n={promotionCount} label="pending" tone={promotionCount > 0 ? "seam" : "good"} />}
        >
          <DoctrineNote>
            {promotionCount > 0
              ? `${promotionCount.toLocaleString("en-US")} lane promotion${promotionCount === 1 ? "" : "s"} await a promote/hold call. A one-way promotion is a Class-A gate on the Decide queue.`
              : "No lane promotions pending. Claim levels are settled."}
          </DoctrineNote>
          <div className="mt-3 border-t border-hair pt-2.5">
            <span className="mb-1.5 block text-2xs font-semibold uppercase tracking-wide text-faint">providers seen</span>
            <div className="flex flex-wrap gap-1.5">
              {providers.length ? (
                providers.map(([provider, count]) => (
                  <span
                    key={provider}
                    className="mono rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs tabular-nums text-muted"
                  >
                    {provider} · {count}
                  </span>
                ))
              ) : (
                <span className="text-2xs text-faint">none recorded</span>
              )}
            </div>
          </div>
        </Card>
      </div>

      <Link
        href="/routing"
        className="flex items-center justify-between gap-3 rounded-lg border border-hair bg-panel px-4 py-3 text-sm text-ink shadow-sm transition-colors hover:bg-soft focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
      >
        <span>
          <span className="font-semibold">Routing & cost →</span>{" "}
          <span className="text-muted">lanes, promotion criteria, remorse, cost-by-lane, provider ledger</span>
        </span>
        <span className="mono text-2xs text-faint">/routing</span>
      </Link>
      <DoctrineNote>
        Lane config and promotion criteria are file-owned (`compound route import-model-capabilities`,
        `import-promotion-criteria`). The Routing & cost screen lands with the parallel routing lane.
      </DoctrineNote>
    </SettingsSection>
  );
}
