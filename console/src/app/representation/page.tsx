import { FreshnessChip, PageHeader } from "@/components/kit";
import { arr, getWorkbench, str } from "@/lib/compound";
import { RepresentationRack, type Evidence } from "@/representation";

export const dynamic = "force-dynamic";

/**
 * Representation — the live consumer of the representation cluster (packet
 * 2026-07-11-representation-cluster-v1). Maps the workbench projection into
 * normalized `Evidence` and lets the cluster choose a shape per item, so the
 * six-shape vocabulary is exercised on REAL projection data (not a fixture).
 *
 * DEVIATION (documented in the handoff): the packet names home/work as the
 * mount. Those surfaces are being edited by live parallel lanes
 * (operator-language-surface, ask-triage, W0/1A); a self-owned route is the
 * zero-collision live consumer. Lifting the rack into the contested home/work
 * board — and wiring the act affordance through the governed path — is left as
 * an orchestrator landing-gate decision (acts stay read-only here: no button
 * that looks live but does nothing, which would re-introduce pf1).
 */
export default async function RepresentationPage() {
  const workbench = await getWorkbench();
  const open = arr(workbench?.open);

  const evidence: Evidence[] = open.slice(0, 24).map((raw): Evidence => {
    const row = (raw ?? {}) as Record<string, unknown>;
    const receiptRefs = arr(row.receipt_refs).map(String).filter(Boolean);
    return {
      id: str(row.id) || str(row.work_id) || "work_unknown",
      subject: str(row.title) || str(row.subject) || str(row.id) || str(row.work_id),
      state: str(row.state),
      expected_state: str(row.expected_state) || undefined,
      observed_state: str(row.observed_state) || undefined,
      success_predicate: str(row.success_predicate) || str(row.desired_outcome) || undefined,
      receipt_refs: receiptRefs,
      last_receipt_at: str(row.updated_at) || undefined,
      summary: str(row.desired_outcome) || str(row.title) || undefined,
      // Read-only in v1: no act is dispatched from this route yet (see header).
      action: null,
    };
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        breadcrumb="operate / representation"
        title="Representation"
        right={<FreshnessChip generatedAt={str(workbench?.generated_at)} source="compound workbench --json" />}
      />
      <p className="text-sm text-muted" style={{ maxWidth: "var(--read-max)" }}>
        Each open item chooses its own shape from the evidence — a divergence splits into expected vs observed, an
        unbacked claim beats like a heartbeat, a quantity draws a sparkline. Every card states why it took this form.
      </p>
      <RepresentationRack evidence={evidence} />
    </div>
  );
}
