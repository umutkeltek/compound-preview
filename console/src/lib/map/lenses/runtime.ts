import { arr, getWorkbench, str } from "@/lib/compound";
import type { LensGraph, LensNode, LensEdge, LensStatus } from "@/lib/map/lens";
import { nodeKindAccent } from "@/lib/map/lens";
import { readLeases } from "@/lib/map/leases";
import { readRouteEvidence } from "@/lib/map/route-evidence";

/*
 * Runtime lens — "what is happening right now."
 * executors + open work + models(recent routing) hung off the real workbench.
 * Sources: compound workbench --json, compound work leases --json,
 *          compound route evidence-authority --json --recent N.
 */

const COL = { executor: 0, work: 360, gateway: 720, model: 720, dispatch: 1080 };
const ROW = 118;

function workStatus(state: string): LensStatus {
  switch (state) {
    case "held":
      return "held";
    case "blocked":
      return "blocked";
    case "needs_review":
    case "verifying":
      return "needs-review";
    case "running":
    case "in_progress":
    case "claimed":
      return "running";
    default:
      return "idle";
  }
}

function shortLabel(text: string, fallback: string): string {
  const t = text.trim().replace(/\s+/g, " ");
  if (!t) return fallback;
  return t.length > 46 ? `${t.slice(0, 45)}…` : t;
}

export async function buildRuntimeLens(): Promise<LensGraph> {
  const [workbench, leases, routeEv] = await Promise.all([
    getWorkbench(),
    readLeases(),
    readRouteEvidence(200),
  ]);

  const nodes: LensNode[] = [];
  const edges: LensEdge[] = [];
  const sources = [
    "compound workbench --json → open[]/counts/dispatch_next",
    "compound work leases --json",
    "compound route evidence-authority --json --recent 200",
  ];

  const generatedAt = workbench ? str(workbench.generated_at) : "";
  const open = workbench ? arr(workbench.open) : [];
  const dispatchNext = (workbench?.dispatch_next ?? null) as Record<
    string,
    unknown
  > | null;

  const leaseByWork = new Map<string, string>();
  for (const l of leases) if (l.work_id) leaseByWork.set(l.work_id, l.by);

  // ----- executors: distinct actors touching open work + dispatch -----
  const executorRow = new Map<string, number>();
  const executorSet = new Set<string>();
  const addExecutor = (name: string) => {
    if (name && !executorSet.has(name)) executorSet.add(name);
  };
  for (const w of open) {
    addExecutor(str(w.owner));
    addExecutor(str(w.claimed_by));
    const by = leaseByWork.get(str(w.work_id));
    if (by) addExecutor(by);
  }
  if (dispatchNext) addExecutor(str(dispatchNext.actor));
  [...executorSet].forEach((name, i) => {
    executorRow.set(name, i);
    nodes.push({
      id: `exec:${name}`,
      kind: "executor",
      label: name,
      sublabel: "executor",
      accent: nodeKindAccent("executor"),
      position: { x: COL.executor, y: i * ROW },
      sources: ["compound workbench --json → open[].owner/claimed_by"],
      cli: "compound workbench --json",
      raw: { executor: name },
    });
  });

  // ----- open work items -----
  open.forEach((w, i) => {
    const workId = str(w.work_id);
    const state = str(w.state);
    const label = shortLabel(str(w.desired_outcome) || str(w.success_predicate), workId);
    const proofRefs = [str(w.receipt_id), str(w.transition_id)].filter(Boolean);
    nodes.push({
      id: `work:${workId}`,
      kind: "work",
      label,
      sublabel: workId,
      status: workStatus(state),
      accent: nodeKindAccent("work"),
      position: { x: COL.work, y: i * ROW },
      sources: ["compound workbench --json → open[]"],
      cli: `compound work show ${workId}`,
      proofRefs: proofRefs.length ? proofRefs : undefined,
      raw: w,
    });

    // executor → work (owns / claimed)
    const owner = str(w.owner);
    if (owner && executorSet.has(owner)) {
      edges.push({
        id: `owns:${owner}:${workId}`,
        source: `exec:${owner}`,
        target: `work:${workId}`,
        kind: "owns",
        label: "owns",
      });
    }
    const claimant = str(w.claimed_by) || leaseByWork.get(workId) || "";
    if (claimant && claimant !== owner && executorSet.has(claimant)) {
      edges.push({
        id: `claimed:${claimant}:${workId}`,
        source: `exec:${claimant}`,
        target: `work:${workId}`,
        kind: "claimed",
        label: `claimed · ${claimant}`,
      });
    }
  });

  const openIds = new Set(open.map((w) => `work:${str(w.work_id)}`));

  // ----- recent routing: gateway → models, model → work when tied -----
  const modelTokens = new Map<string, { tokens: number; latency: number; count: number }>();
  const modelToWork: { model: string; work: string; tokens: number; latency: number }[] = [];
  for (const r of routeEv.records) {
    const model = str(r.selected_target?.model);
    if (!model) continue;
    const tokens = typeof r.usage?.total_tokens === "number" ? r.usage.total_tokens : 0;
    const latency = typeof r.latency_ms === "number" ? r.latency_ms : 0;
    const agg = modelTokens.get(model) ?? { tokens: 0, latency: 0, count: 0 };
    agg.tokens += tokens;
    agg.latency = latency || agg.latency;
    agg.count += 1;
    modelTokens.set(model, agg);
    const workId = str(r.work_id);
    if (workId && openIds.has(`work:${workId}`)) {
      modelToWork.push({ model, work: workId, tokens, latency });
    }
  }

  const models = [...modelTokens.entries()]
    .sort((a, b) => b[1].count - a[1].count)
    .slice(0, 8);

  if (models.length) {
    nodes.push({
      id: "gateway",
      kind: "gateway",
      label: "execution gateway",
      sublabel: "rented plane · route evidence",
      accent: nodeKindAccent("gateway"),
      position: { x: COL.gateway, y: -ROW },
      sources: ["compound route evidence-authority --json --recent 200"],
      cli: "compound route evidence-authority --json",
      raw: { note: "route evidence is metadata, not task success" },
    });
    models.forEach(([model, agg], i) => {
      const id = `model:${model}`;
      nodes.push({
        id,
        kind: "model",
        label: model,
        sublabel: `${agg.count} routes · ${Math.round(agg.tokens / 1000)}k tok`,
        accent: nodeKindAccent("model"),
        position: { x: COL.model, y: i * ROW },
        sources: ["compound route evidence-authority --json --recent 200 → recent[].model"],
        cli: "compound route evidence-authority --json",
        raw: { model, ...agg },
      });
      edges.push({
        id: `routes:${model}`,
        source: "gateway",
        target: id,
        kind: "routes",
        dashed: true,
      });
    });
    for (const m of modelToWork) {
      edges.push({
        id: `route:${m.model}:${m.work}`,
        source: `model:${m.model}`,
        target: `work:${m.work}`,
        kind: "route",
        label: `${Math.round(m.tokens / 1000)}k tok · ${m.latency}ms`,
      });
    }
  }

  // ----- dispatch_next (the suggested next safe move) -----
  if (dispatchNext && (str(dispatchNext.actor) || arr(dispatchNext.blocked_on).length)) {
    const canRun = dispatchNext.can_run === true;
    const waitReason = str(dispatchNext.wait_reason);
    const blockedOn = arr(dispatchNext.blocked_on).map(str).filter(Boolean);
    nodes.push({
      id: "dispatch",
      kind: "dispatch",
      label: canRun ? "next: ready" : "next: waiting",
      sublabel: waitReason || str(dispatchNext.actor) || "dispatch",
      status: canRun ? "running" : "blocked",
      accent: nodeKindAccent("dispatch"),
      position: { x: COL.dispatch, y: 0 },
      sources: ["compound workbench --json → dispatch_next"],
      cli: "compound workbench --json",
      raw: dispatchNext,
    });
    for (const b of blockedOn) {
      if (openIds.has(`work:${b}`)) {
        edges.push({
          id: `blocked:${b}`,
          source: `work:${b}`,
          target: "dispatch",
          kind: "blocked",
          dashed: true,
          label: "blocks",
        });
      }
    }
  }

  const terminal = workbench ? Number((workbench.counts as Record<string, unknown>)?.terminal ?? 0) : 0;
  const note = open.length
    ? `${open.length} open · ${terminal} terminal (not shown). Runtime = live open work only.`
    : `No open work right now. ${terminal} terminal items exist (not shown).`;

  return {
    lens: "runtime",
    nodes,
    edges,
    generatedAt,
    sources,
    note,
  };
}
