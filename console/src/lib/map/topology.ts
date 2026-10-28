import { readFile } from "node:fs/promises";
import path from "node:path";

import { COMPOUND_HOME, num, privateFallbacksEnabled, runJson } from "@/lib/compound";
import {
  getCapabilityAuthority,
  getKnowledgeSourceCount,
  getKnowledgeSourceKinds,
  getLedgerStats,
  getProductAuthority,
  getRouteAuthority,
  getSystemStatsProjection,
} from "@/lib/map/projections";
import { LANE_ORDER } from "@/lib/map/types";
import type {
  SystemNode,
  TopologyEdge,
  TopologyEntry,
  TopologySnapshot,
} from "@/lib/map/types";

/*
 * Topology DATA lives in the operator's private runtime home
 * (~/.compound/config/system-topology.json) — never in the public repo. The
 * repo ships only a synthetic fixture so the Map renders for dev/other
 * operators. This is the public/private boundary: mechanism here, data there.
 */

const OVERLAY = path.resolve(COMPOUND_HOME, "config", "system-topology.json");
const FIXTURE = path.resolve(process.cwd(), "fixtures", "map", "topology.json");

type TopologyFile = { nodes: TopologyEntry[]; edges: TopologyEdge[] };

async function loadTopologyFile(): Promise<TopologyFile> {
  const files = privateFallbacksEnabled() ? [OVERLAY, FIXTURE] : [FIXTURE];
  for (const file of files) {
    try {
      const data = JSON.parse(await readFile(file, "utf8")) as TopologyFile;
      if (Array.isArray(data.nodes) && Array.isArray(data.edges)) return data;
    } catch {
      /* fall through to the next source */
    }
  }
  return { nodes: [], edges: [] };
}

/** Hydrate the topology with live numbers from real projections. */
export async function loadTopology(): Promise<TopologySnapshot> {
  const projected = await runJson(["system", "map", "--json"]);
  if (projected && Array.isArray(projected.nodes) && Array.isArray(projected.edges)) {
    return {
      nodes: projected.nodes as SystemNode[],
      edges: projected.edges as TopologyEdge[],
      stats: (projected.stats as TopologySnapshot["stats"]) ?? {
        capture: { live: 0, notLive: 0 },
        storage: { live: 0, notLive: 0 },
        meaning: { live: 0, notLive: 0 },
        outward: { live: 0, notLive: 0 },
        infra: { live: 0, notLive: 0 },
      },
      generatedAt: typeof projected.generated_at === "string" ? projected.generated_at : new Date().toISOString(),
    };
  }

  // `system stats --json` backs ledger/knowledge-source stats below - fetched
  // ONCE and shared, instead of each reader re-execing the same command.
  const [file, cap, prod, route, systemStats] = await Promise.all([
    loadTopologyFile(),
    getCapabilityAuthority(),
    getProductAuthority(),
    getRouteAuthority(),
    getSystemStatsProjection(),
  ]);
  const [ledger, ksCount, ksKinds] = await Promise.all([
    getLedgerStats(systemStats),
    getKnowledgeSourceCount(systemStats),
    getKnowledgeSourceKinds(systemStats),
  ]);

  const nodes: SystemNode[] = file.nodes.map((entry) => {
    let volume: string | undefined;
    let evidenceRefs: string[] | undefined;

    switch (entry.projection) {
      case "capability-authority":
        volume = `${num(cap?.node_count)} nodes · ${num(cap?.edge_count)} edges`;
        break;
      case "product-authority":
        volume = `${num(prod?.product_count)} products · ${num(prod?.signal_count)} signals`;
        break;
      case "route-authority":
        volume = `${num(route?.route_evidence_count)} route evidence`;
        break;
      case "ledger-stats":
        if (entry.id === "memory") {
          volume = `${ledger.claims} claims · ${ledger.entities} entities · ${ledger.edges} edges`;
        } else if (entry.id === "compound-home") {
          volume = `${ledger.evidence} evidence · ${ledger.knowledgeImports} imports`;
        }
        break;
      case "knowledge-sources":
        volume = `${ksCount} sources`;
        evidenceRefs = ksKinds;
        break;
      case "external-intake-map":
        if (entry.reviewId) evidenceRefs = [entry.reviewId];
        break;
      case "host-reachability":
        // Live reachability is a Go-side probe: the primary path above returns
        // `compound system map --json` (real status/volume) directly. This
        // CLI-unavailable fallback can't cheaply probe hosts, so it renders the
        // last-known static status from the overlay/fixture — no false "live".
        break;
      case "static":
      case "job-roster":
      default:
        break;
    }

    return {
      ...entry,
      volume,
      evidenceRefs: evidenceRefs?.length ? evidenceRefs : undefined,
    };
  });

  const stats = {} as TopologySnapshot["stats"];
  for (const lane of LANE_ORDER) {
    const inLane = nodes.filter((n) => n.lane === lane);
    stats[lane] = {
      live: inLane.filter((n) => n.status === "live").length,
      notLive: inLane.filter((n) => n.status !== "live").length,
    };
  }

  return {
    nodes,
    edges: file.edges,
    stats,
    generatedAt: new Date().toISOString(),
  };
}
