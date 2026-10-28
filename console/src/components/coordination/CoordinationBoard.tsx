"use client";

import { EmptyState, SectionHeader, StatusChip } from "@/components/kit";
import { PulseIcon } from "@/components/kit/icons";
import { laneGated, type CoordinationActor, type CoordinationLane, type CoordinationView } from "@/lib/coordination-model";
import { useLiveFleet } from "@/lib/stream/useLiveFleet";
import type { ProjectionStatus } from "@/lib/compound";
import { OperatorCoordinationList } from "./OperatorCoordinationList";

/*
 * Coordination view: Me / Others / Waiting-on. The reframe of the old fleet
 * monitor — the load-bearing part is the WAITING-ON edges ("I'm doing X, that
 * actor is doing Y, and I'm waiting on it to start Z"), not an inventory of
 * agents. Actors are generic (whatever `compound coordination` enumerates);
 * the board never names "Codex"/"Claude" itself.
 *
 * Data plane: server seed from `compound coordination --json`
 * (lib/coordination) — the read-only verb projecting the planning half of
 * `work continue` per actor plus the lane-capacity gate — overlaid LIVE by
 * useLiveFleet (compound stream --follow; events carry `by`). Execution
 * detail (harness/run-attempt) joins from the traffic data plane per work.
 *
 * Honesty contracts:
 *  - coordination door unreachable => the degraded seam, never an invented
 *    empty fleet (present=false is a stated fact, not a doctrine empty state);
 *  - stream degraded => render the seed with "showing last known", never blank;
 *  - run-attempt door unreachable => "execution detail unavailable", never
 *    invented.
 */

export type ExecDetail = {
  harness: string;
  runStatus: string;
  actor: string;
  retryDepth: number;
};

export type CoordinationBoardProps = {
  view: CoordinationView;
  doorStatus: ProjectionStatus;
  doorError: string | null;
  /** work_id -> how it is executing (run-attempt join); {} when no join. */
  execDetail: Record<string, ExecDetail>;
  /** false => run-attempt door unreachable. */
  runAttemptsPresent: boolean;
};

const DOOR_COPY: Record<ProjectionStatus, { label: string; tone: "work" | "muted" | "seam" }> = {
  live: { label: "coordination live", tone: "work" },
  stale: { label: "coordination stale — showing last known", tone: "seam" },
  unreachable: { label: "coordination door unreachable", tone: "seam" },
};

const STREAM_COPY = {
  live: { label: "stream live", tone: "work" as const },
  connecting: { label: "stream connecting…", tone: "muted" as const },
  degraded: { label: "stream offline — seed only", tone: "seam" as const },
};

function stateClass(state: string): string {
  switch (state) {
    case "running":
      return "text-work";
    case "verifying":
    case "reviewing":
    case "claimed":
      return "text-seam";
    case "failed":
      return "text-crit";
    case "closed":
      return "text-good";
    default:
      return "text-muted";
  }
}

function LaneChip({ lane }: { lane: CoordinationLane | null }) {
  if (!lane) return null;
  const gated = laneGated(lane);
  const label = lane.refused
    ? "lane refused"
    : lane.confirmationRequired
      ? "lane needs confirmation"
      : gated
        ? `lane ${lane.state}`
        : `lane ${lane.state || "open"} · ${Math.floor(lane.remaining)} left`;
  return (
    <span
      className={`mono inline-flex items-center rounded-pill border px-1.5 py-0.5 text-2xs font-semibold ${
        gated ? "border-seam-line bg-seam-soft text-seam" : "border-hair bg-soft text-muted"
      }`}
      title={lane.message || undefined}
    >
      {label}
      {lane.resetsAt ? <span className="ml-1 font-normal text-faint">↻ {lane.resetsAt}</span> : null}
    </span>
  );
}

function BlockedChips({ blockedOn }: { blockedOn: string[] }) {
  if (blockedOn.length === 0) return null;
  return (
    <span className="flex flex-wrap gap-1">
      {blockedOn.map((edge) => (
        <span key={edge} className="mono rounded-pill border border-hair bg-soft px-1.5 py-0.5 text-2xs text-faint">
          {edge}
        </span>
      ))}
    </span>
  );
}

function ExecLine({ workId, detail, present }: { workId: string; detail: ExecDetail | undefined; present: boolean }) {
  if (!present) {
    return <span className="text-2xs text-faint italic">execution detail unavailable</span>;
  }
  const d = detail ?? { harness: "", runStatus: "", actor: "", retryDepth: 0 };
  if (!d.harness && !d.runStatus) {
    return <span className="text-2xs text-faint">no run-attempt</span>;
  }
  return (
    <span className="mono text-2xs text-muted">
      {d.harness || "?"}
      {d.runStatus ? <span className={stateClass(d.runStatus)}> · {d.runStatus}</span> : null}
      {d.actor ? <span> · {d.actor}</span> : null}
      {d.retryDepth > 0 ? <span className="text-seam"> · ↻{d.retryDepth}</span> : null}
      <span className="text-faint"> · {workId}</span>
    </span>
  );
}

function ActorCard({
  actor,
  isMe,
  liveWorkId,
  liveBy,
  execDetail,
  runAttemptsPresent,
}: {
  actor: CoordinationActor;
  isMe: boolean;
  liveWorkId: string | null;
  liveBy: string;
  execDetail: Record<string, ExecDetail>;
  runAttemptsPresent: boolean;
}) {
  const doing = actor.doing;
  const live = doing && liveWorkId === doing.workId;
  return (
    <section
      className={`flex flex-col gap-2 rounded-xl border p-4 shadow-sm ${
        isMe ? "border-work-line bg-work-soft" : "border-hair bg-panel"
      }`}
    >
      <div className="flex items-center gap-2">
        {live ? (
          <span className="inline-block h-1.5 w-1.5 shrink-0 animate-pulse rounded-full bg-work" aria-label="live move" />
        ) : null}
        <span className="text-md font-semibold text-ink">{actor.actor || "(none)"}</span>
        {isMe ? <span className="mono rounded-pill border border-work-line bg-panel px-1.5 py-0.5 text-2xs text-work">me</span> : null}
        <span className="ml-auto flex items-center gap-2 text-2xs tabular-nums text-faint">
          <span>shared queue: {actor.open} open</span>
          <span>{actor.inProgress} in flight</span>
        </span>
      </div>

      {doing ? (
        <div className="flex flex-col gap-1">
          <div className="flex items-center gap-2">
            <span className="truncate text-sm text-ink" title={doing.title}>
              {doing.title}
            </span>
            <span className={`mono ml-auto shrink-0 text-2xs font-semibold ${stateClass(doing.state)}`}>{doing.state || "—"}</span>
          </div>
          {doing.nextAction ? <span className="text-2xs text-muted">{doing.nextAction}</span> : null}
          <ExecLine workId={doing.workId} detail={execDetail[doing.workId]} present={runAttemptsPresent} />
          {live ? <span className="text-2xs text-work">live · {liveBy || "moved"} just moved this</span> : null}
        </div>
      ) : (
        <p className="text-2xs text-faint">idle — nothing claimed</p>
      )}

      {actor.canContinue ? (
        <div className="flex items-center gap-2">
          <StatusChip tone="work" label="can continue" />
          <span className="truncate text-2xs text-muted">{actor.nextAction}</span>
        </div>
      ) : (
        <div className="flex flex-col gap-1.5">
          <div className="flex items-center gap-2">
            <StatusChip tone={laneGated(actor.lane) ? "seam" : "muted"} label={laneGated(actor.lane) ? "waiting · lane gate" : "waiting"} />
            <span className="mono truncate text-2xs text-faint">{actor.waitReason || "no safe continue step"}</span>
          </div>
          <BlockedChips blockedOn={actor.blockedOn} />
        </div>
      )}
      <LaneChip lane={actor.lane} />
    </section>
  );
}

export function CoordinationBoard({ view, doorStatus, doorError, execDetail, runAttemptsPresent }: CoordinationBoardProps) {
  const { status, moves } = useLiveFleet();
  const door = DOOR_COPY[doorStatus];
  const stream = STREAM_COPY[status];

  if (!view.present) {
    return (
      <div className="flex flex-col gap-4">
        <section className="rounded-xl bg-panel p-4 shadow-sm">
          <SectionHeader
            tone="seam"
            icon={<PulseIcon />}
            label="Coordination"
            sub="me / others / waiting-on"
            right={<StatusChip tone={door.tone} label={door.label} />}
          />
        </section>
        <EmptyState>
          Coordination door unreachable — the installed `compound` binary does not answer `coordination --json`
          {doorError ? <span className="mono"> ({doorError})</span> : null}. Install the current build (`make install`) to see who is doing what, and who is waiting on whom.
        </EmptyState>
      </div>
    );
  }

  const liveByWork = (workId: string) => moves.get(workId);
  const waitsWithLiveness = view.waits.map((wait) => ({
    wait,
    live: wait.work ? moves.get(wait.work) : undefined,
  }));

  return (
    <div className="flex flex-col gap-4">
      <section className="rounded-xl bg-panel p-4 shadow-sm">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <SectionHeader
            tone="work"
            icon={<PulseIcon />}
            label="Coordination"
            sub="who is doing what — and who is waiting on whom to start"
            right={
              <span className="flex items-center gap-2">
                <StatusChip tone={door.tone} label={door.label} pulse={doorStatus === "live"} />
                <StatusChip tone={stream.tone} label={stream.label} pulse={status === "live"} />
              </span>
            }
          />
          <form action="/traffic" method="get" className="ml-auto flex items-center gap-2 text-2xs text-faint">
            <label htmlFor="coordination-actor">view as</label>
            <select
              id="coordination-actor"
              name="actor"
              defaultValue={view.actor}
              onChange={(event) => event.currentTarget.form?.requestSubmit()}
              className="mono rounded-md border border-hair bg-soft px-2 py-1 text-2xs text-ink"
            >
              {view.actorOptions.map((option) => (
                <option key={option} value={option}>
                  {option}
                </option>
              ))}
            </select>
            <noscript>
              <button type="submit" className="rounded-md border border-hair bg-soft px-2 py-1">
                switch
              </button>
            </noscript>
          </form>
        </div>
        {doorStatus === "stale" ? (
          <p className="mt-3 rounded-md border border-hair bg-soft px-3 py-2 text-2xs text-seam">
            Coordination projection is stale — showing the last good read while the verb re-runs.
          </p>
        ) : null}
        {status === "degraded" ? (
          <p className="mt-3 rounded-md border border-hair bg-soft px-3 py-2 text-2xs text-seam">
            Live stream offline — the view shows the last reconciled coordination until the stream reconnects.
          </p>
        ) : null}
      </section>

      <OperatorCoordinationList projection={view.operatorCoordination} />

      {/* ME — what I'm on, and what I'm waiting for. */}
      <section className="flex flex-col gap-2">
        <SectionHeader tone="work" label="Me" sub={view.me.actor ? `viewing as ${view.me.actor}${view.actorExplicit ? "" : " (most-present actor)"}` : "no actor selected"} />
        <ActorCard
          actor={view.me}
          isMe
          liveWorkId={view.me.doing ? liveByWork(view.me.doing.workId)?.workId ?? null : null}
          liveBy={view.me.doing ? liveByWork(view.me.doing.workId)?.by ?? "" : ""}
          execDetail={execDetail}
          runAttemptsPresent={runAttemptsPresent}
        />
      </section>

      {/* OTHERS — the same coordination face, per generic actor. */}
      <section className="flex flex-col gap-2">
        <SectionHeader tone="neutral" label="Others" sub={`${view.others.length} other actor${view.others.length === 1 ? "" : "s"}`} />
        {view.others.length === 0 ? (
          <EmptyState>No other actors — leases, claims, and the executor fleet surfaced nobody else.</EmptyState>
        ) : (
          <div className="grid grid-cols-1 gap-3 lg:grid-cols-2 xl:grid-cols-3">
            {view.others.map((actor) => {
              const live = actor.doing ? moves.get(actor.doing.workId) : undefined;
              return (
                <ActorCard
                  key={actor.actor}
                  actor={actor}
                  isMe={false}
                  liveWorkId={live?.workId ?? null}
                  liveBy={live?.by ?? ""}
                  execDetail={execDetail}
                  runAttemptsPresent={runAttemptsPresent}
                />
              );
            })}
          </div>
        )}
      </section>

      {/* Work and lane constraints; shared queue availability is not a dependency. */}
      <section className="flex flex-col gap-2">
        <SectionHeader tone="seam" label="Work constraints" sub={`${view.waits.length} constraint${view.waits.length === 1 ? "" : "s"}`} />
        {view.waits.length === 0 ? (
          <EmptyState>No specific work or lane constraints to show.</EmptyState>
        ) : (
          <div className="overflow-hidden rounded-xl border border-hair bg-panel shadow-sm">
            <table className="w-full text-left text-sm">
              <thead>
                <tr className="border-b border-hair text-2xs uppercase tracking-wide text-faint">
                  <th className="px-3 py-2 font-semibold">actor</th>
                  <th className="px-3 py-2 font-semibold">waits on</th>
                  <th className="px-3 py-2 font-semibold">work</th>
                  <th className="px-3 py-2 font-semibold">blocked-on edges</th>
                  <th className="px-3 py-2 font-semibold">lane</th>
                </tr>
              </thead>
              <tbody>
                {waitsWithLiveness.map(({ wait, live }, index) => (
                  <tr key={`${wait.actor}:${wait.work}:${wait.reason}:${index}`} className={`border-b border-hair last:border-b-0 ${live ? "bg-work-soft" : ""}`}>
                    <td className="px-3 py-2">
                      <span className="flex items-center gap-2">
                        {live ? <span className="inline-block h-1.5 w-1.5 shrink-0 animate-pulse rounded-full bg-work" aria-label="live move" /> : null}
                        <span className={`font-medium ${wait.actor === view.actor ? "text-work" : "text-ink"}`}>{wait.actor}</span>
                        {wait.actor === view.actor ? <span className="mono text-2xs text-work">me</span> : null}
                      </span>
                    </td>
                    <td className="mono px-3 py-2 text-2xs text-seam">{wait.reason || "—"}</td>
                    <td className="mono px-3 py-2 text-2xs text-muted">{wait.work || "—"}</td>
                    <td className="px-3 py-2">
                      <BlockedChips blockedOn={wait.blockedOn} />
                    </td>
                    <td className="px-3 py-2">
                      <LaneChip lane={wait.lane} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
