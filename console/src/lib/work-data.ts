import { arr, getWorkbench, Json, runJson, str } from "@/lib/compound";

export type PacketSummary = {
  path: string;
  title: string;
  objective: string;
  success: string;
  boundaries: string[];
  canonical_surface?: string;
  evidence_ref?: string;
  source_digest?: string;
};

export type WorkCardData = Json & {
  displayId: string;
  packet: PacketSummary | null;
};

export function workId(item: Json): string {
  return str(item.work_id) || str(item.id);
}

export function shortText(value: string, max = 300): string {
  const compact = value.replace(/\s+/g, " ").trim();
  if (compact.length <= max) return compact;
  return `${compact.slice(0, max - 1).trimEnd()}...`;
}

function packetSummaryFromItem(item: Json): PacketSummary {
  return {
    path: str(item.path) || str(item.source_ref),
    title: str(item.title),
    objective: "",
    success: "",
    boundaries: [],
    canonical_surface: str(item.canonical_surface),
    evidence_ref: str(item.evidence_ref),
    source_digest: str(item.source_digest),
  };
}

/** `compound packets list --json` fetched once and indexed by canonical_surface. */
async function packetsBySurface(): Promise<Map<string, PacketSummary>> {
  const data = await runJson(["packets", "list", "--json"]);
  const index = new Map<string, PacketSummary>();
  for (const item of arr(data?.items)) {
    const surface = str(item.canonical_surface);
    if (surface) index.set(surface, packetSummaryFromItem(item));
  }
  return index;
}

export async function packetForSurface(surface: string): Promise<PacketSummary | null> {
  if (!surface.startsWith("packet:")) return null;
  const index = await packetsBySurface();
  return index.get(surface) ?? null;
}

export async function enrichWorkItem(item: Json): Promise<WorkCardData> {
  const id = workId(item);
  const packet = await packetForSurface(str(item.canonical_surface));
  return { ...item, displayId: id, packet };
}

/**
 * Fetches the packets list ONCE for the whole batch and maps it in memory -
 * previously this ran `packets list --json` once per item (N+1 fan-out).
 */
export async function enrichWorkItems(items: Json[]): Promise<WorkCardData[]> {
  const index = await packetsBySurface();
  return items.map((item) => {
    const id = workId(item);
    const surface = str(item.canonical_surface);
    const packet = surface.startsWith("packet:") ? index.get(surface) ?? null : null;
    return { ...item, displayId: id, packet };
  });
}

export async function findWorkbenchItem(id: string): Promise<Json | null> {
  const workbench = await getWorkbench();
  const items = [...arr(workbench?.open), ...arr(workbench?.recent)];
  return items.find((item) => workId(item) === id) ?? null;
}
