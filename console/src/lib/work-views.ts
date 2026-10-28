import type { Json } from "@/lib/compound";
import { str } from "@/lib/compound";
import type { TrafficReport, TrafficWork } from "@/lib/traffic";
import { stageProgressForState, type StageProgress } from "@/lib/work-pipeline";
import { ago } from "@/lib/time";
import { shortText, workId } from "@/lib/work-data";
import { harnessDisplayName } from "@/lib/harness-label";

/** Three presentations over one WorkEnvelope + executor + RunAttempt join. */
export type WorkLensItem = {
  workId: string;
  title: string;
  state: string;
  owner: string;
  nextAction: string;
  canonicalSurface: string;
  ageLabel: string;
  laneId: string;
  laneLabel: string;
  harness: string;
  harnessLabel: string;
  runStatus: string;
  actor: string;
  retryDepth: number;
  startedAt: string;
  lastHeartbeatAt: string;
  progress: StageProgress;
};

export type WorkLensGroup = { id: string; label: string; status: string; items: WorkLensItem[] };

function titleFor(item: Json): string {
  const surface = str(item.canonical_surface);
  if (surface.startsWith("packet:")) return surface.slice("packet:".length).replace(/-/g, " ");
  return str(item.success_predicate) || str(item.desired_outcome) || str(item.title) || workId(item) || "work item";
}

function byWork(report: TrafficReport): Map<string, { laneId: string; laneLabel: string; work: TrafficWork }> {
  const out = new Map<string, { laneId: string; laneLabel: string; work: TrafficWork }>();
  for (const lane of report.lanes) {
    for (const work of lane.inFlight) {
      if (!out.has(work.workId)) out.set(work.workId, { laneId: lane.agentId, laneLabel: lane.agentLabel, work });
    }
  }
  return out;
}

export function buildWorkLensItems(open: Json[], traffic: TrafficReport): WorkLensItem[] {
  const execution = byWork(traffic);
  return open.map((item) => {
    const id = workId(item);
    const joined = execution.get(id);
    const run = joined?.work;
    const age = str(item.claimed_at) || str(item.created_at) || str(item.updated_at);
    return {
      workId: id,
      title: shortText(titleFor(item), 180),
      state: str(item.state) || "unknown",
      owner: str(item.claimed_by) || str(item.owner) || "Unassigned",
      nextAction: shortText(str(item.next_action), 160),
      canonicalSurface: str(item.canonical_surface),
      ageLabel: age ? ago(age) : "age unavailable",
      laneId: joined?.laneId ?? "unassigned",
      laneLabel: joined?.laneLabel ?? "Unassigned",
      harness: run?.harness ?? "",
      harnessLabel: harnessDisplayName(run?.harness ?? ""),
      runStatus: run?.runStatus ?? "",
      actor: run?.actor ?? "",
      retryDepth: run?.retryDepth ?? 0,
      startedAt: run?.startedAt ?? "",
      lastHeartbeatAt: run?.lastHeartbeatAt ?? "",
      progress: stageProgressForState(str(item.state)),
    };
  });
}

export function groupWorkLensItems(items: WorkLensItem[], traffic: TrafficReport): WorkLensGroup[] {
  const groups = traffic.lanes.map((lane) => ({
    id: lane.agentId,
    label: lane.agentLabel,
    status: lane.agentStatus,
    items: items.filter((item) => item.laneId === lane.agentId),
  }));
  const assigned = new Set(groups.flatMap((group) => group.items.map((item) => item.workId)));
  const unassigned = items.filter((item) => !assigned.has(item.workId));
  if (unassigned.length) groups.push({ id: "unassigned", label: "Unassigned", status: "waiting", items: unassigned });
  return groups.filter((group) => group.items.length > 0);
}
