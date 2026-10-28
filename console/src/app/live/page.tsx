import Link from "next/link";
import { Suspense } from "react";

import { DelegatedSessions } from "@/components/agents/DelegatedSessions";
import { normalizeExecutorProjection } from "@/components/agents/executor-normalize";
import { CoordinationBoard, type ExecDetail } from "@/components/coordination/CoordinationBoard";
import { LiveHarnessCanvas, type LiveLane } from "@/components/live/LiveHarnessCanvas";
import { LiveOperationsTimeline } from "@/components/live/LiveOperationsTimeline";
import { FreshnessChip, PageHeader, PulseIcon, SectionHeader, StatusChip } from "@/components/kit";
import { getExecutors } from "@/lib/compound";
import { getCoordination } from "@/lib/coordination";
import { getTraffic } from "@/lib/traffic";

export const dynamic = "force-dynamic";

function Metric({ label, value, note, tone = "ink" }: { label: string; value: number; note: string; tone?: "ink" | "good" | "seam" }) {
  const color = tone === "good" ? "text-good" : tone === "seam" ? "text-seam" : "text-ink";
  return (
    <div className="rounded-lg border border-hair bg-panel px-3 py-2.5 shadow-sm">
      <div className="section-label">{label}</div>
      <div className={`mono mt-1 text-xl tabular-nums ${color}`}>{value}</div>
      <div className="mt-0.5 text-2xs text-faint">{note}</div>
    </div>
  );
}

async function LiveCoordination({ execDetail, runAttemptsPresent }: { execDetail: Record<string, ExecDetail>; runAttemptsPresent: boolean }) {
  const coordination = await getCoordination();
  return (
    <section data-depth-min="technical" className="flex flex-col gap-3">
      <SectionHeader tone="route" icon={<PulseIcon />} label="Coordination" sub="me · others · waiting-on" />
      <CoordinationBoard
        view={coordination.view}
        doorStatus={coordination.status}
        doorError={coordination.error}
        execDetail={execDetail}
        runAttemptsPresent={runAttemptsPresent}
      />
    </section>
  );
}

export default async function LivePage() {
  const [traffic, executorRaw] = await Promise.all([getTraffic(), getExecutors()]);
  const projection = normalizeExecutorProjection(executorRaw);
  const lanes: LiveLane[] = traffic.lanes.map((lane) => {
    const ranked = lane.inFlight.slice().sort((a, b) => {
      const rank = (value: typeof a) => value.runStatus === "running" || value.state === "running" ? 0 : value.state === "verifying" ? 1 : value.state === "blocked" || value.state === "held" ? 2 : 3;
      return rank(a) - rank(b);
    });
    return {
      id: lane.agentId,
      label: lane.agentLabel,
      status: lane.agentStatus,
      running: lane.running,
      needsMe: lane.needsMe,
      totalWork: lane.inFlight.length,
      work: ranked.slice(0, 2).map((work) => ({
        workId: work.workId,
        title: work.title,
        state: work.state,
        nextAction: work.nextAction,
        harness: work.harness,
        runStatus: work.runStatus,
        actor: work.actor,
        retryDepth: work.retryDepth,
      })),
    };
  });

  const execDetail: Record<string, ExecDetail> = {};
  for (const lane of traffic.lanes) {
    for (const work of lane.inFlight) {
      if (execDetail[work.workId]) continue;
      execDetail[work.workId] = {
        harness: work.harness,
        runStatus: work.runStatus,
        actor: work.actor,
        retryDepth: work.retryDepth,
      };
    }
  }

  const delegatedRunning = projection?.delegatedCounts.running ?? 0;
  return (
    <div className="console-stage flex flex-col gap-6">
      <PageHeader
        breadcrumb="live / overview"
        title="Live"
        right={
          <div className="flex items-center gap-2">
            <StatusChip tone={traffic.runningCount > 0 ? "good" : "muted"} label={traffic.runningCount > 0 ? "activity now" : "quiet now"} pulse={traffic.runningCount > 0} />
            <FreshnessChip generatedAt={traffic.generatedAt} source="compound executors / run-attempts" />
          </div>
        }
      />
      <div className="flex flex-wrap items-end justify-between gap-3">
        <p className="max-w-2xl text-sm leading-relaxed text-muted">
          See which harness owns each unit of work, what is genuinely active, and where the system is waiting. Motion
          appears only when heartbeat or transition evidence says something is moving.
        </p>
        <Link href="/traffic" className="text-xs font-semibold text-work hover:underline">Open coordination detail</Link>
      </div>

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Metric label="Harnesses" value={traffic.lanes.length} note="projected lanes" />
        <Metric label="Running" value={traffic.runningCount} note="heartbeat-backed" tone="good" />
        <Metric label="In flight" value={traffic.inFlightCount} note="work assignments" />
        <Metric label="Waiting on you" value={traffic.needsMeCount} note="paused at a seam" tone="seam" />
      </div>

      <section data-depth-min="understand" className="flex flex-col gap-3">
        <SectionHeader
          tone="work"
          icon={<PulseIcon />}
          label="Harness workfield"
          sub="ownership → active WorkEnvelope → next seam"
          right={<span className="mono text-2xs text-faint">{traffic.runAttemptsPresent ? "RunAttempt joined" : "execution detail unavailable"}</span>}
        />
        <LiveHarnessCanvas lanes={lanes} />
      </section>

      <section data-depth-min="investigate" className="flex flex-col gap-3">
        <SectionHeader
          tone="work"
          icon={<PulseIcon />}
          label="Operations evidence"
          sub="observed → executing → current seam · motion requires live evidence"
        />
        <LiveOperationsTimeline report={traffic} />
      </section>

      <Suspense fallback={<section data-depth-min="technical" className="rounded-xl border border-hair bg-panel px-4 py-6 text-sm text-muted">Loading coordination evidence…</section>}>
        <LiveCoordination execDetail={execDetail} runAttemptsPresent={traffic.runAttemptsPresent} />
      </Suspense>

      {projection ? (
        <div data-depth-min="technical" className="flex flex-col gap-3">
          <div className="flex items-center justify-between gap-3">
            <div>
              <div className="section-label">Supervised delegated sessions</div>
              <p className="mt-1 text-xs text-muted">Exact session and RunAttempt identity; prompt content never reaches this projection.</p>
            </div>
            <StatusChip tone={delegatedRunning > 0 ? "good" : "muted"} label={`${delegatedRunning} running`} />
          </div>
          <DelegatedSessions rows={projection.delegatedSessions} counts={projection.delegatedCounts} />
        </div>
      ) : null}
    </div>
  );
}
