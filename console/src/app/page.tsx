import { ActivityFeed } from "@/components/briefing/ActivityFeed";
import { AttentionAgeMatrix } from "@/components/briefing/AttentionAgeMatrix";
import { BriefingHeader } from "@/components/briefing/BriefingHeader";
import { ContinueCard } from "@/components/briefing/ContinueCard";
import { CausalFocus } from "@/components/briefing/CausalFocus";
import { DecideList } from "@/components/briefing/DecideList";
import { Headline } from "@/components/briefing/Headline";
import { KnowsStrip } from "@/components/briefing/KnowsStrip";
import { KpiStrip } from "@/components/briefing/KpiStrip";
import { LatestMoment } from "@/components/briefing/LatestMoment";
import { PipelineStrip } from "@/components/briefing/PipelineStrip";
import { SectionLabel } from "@/components/briefing/primitives";
import { SelfHealingPanel } from "@/components/briefing/SelfHealingPanel";
import { OperatorCoordinationList } from "@/components/coordination/OperatorCoordinationList";
import { FreshnessChip } from "@/components/kit";
import { attentionNeedsYouGroups, getAttentionProjection, hasClassAGate } from "@/lib/attention";
import {
  autonomyPosture,
  deriveContinue,
  headlineAct,
  memoryBriefing,
  oldestCreatedAt,
  pipelineStages,
  rowTsMs,
  selfHealing,
  weeklyChange,
} from "@/lib/briefing";
import { arr, getExecutors, HEAVY_PROJECTION_TIMEOUT_MS, num, runJson, runProjection, str } from "@/lib/compound";
import { consoleMutationsEnabled } from "@/lib/console-mutations";
import { computeConsoleMutationReadiness } from "@/lib/instance-pin";
import { moveTypeGroup, summarizeMoveTypeMix } from "@/lib/move-mix";
import { latestProofBackedChange } from "@/representation";

export const dynamic = "force-dynamic";

export default async function Briefing() {
  // Heavy whole-home projections (attention/workbench/home-doctor) read through
  // `runProjection` with HEAVY_PROJECTION_TIMEOUT_MS so each door can genuinely
  // SUCCEED cold (30-70s on the live home) and the briefing renders the honest
  // degradation model (live/stale/unreachable) instead of ever caching a
  // swallowed timeout as null (packet: console-honest-degradation).
  //
  // The memory candidates call is window-bounded on purpose: the unbounded
  // projection returns the FULL inbox (150k+ candidates, >100MB — past the
  // exec maxBuffer, so it can never land at any timeout). `--state promoted
  // --created-after` bounds the returned items to exactly the promoted rows
  // the 14d weekly delta consumes (custody order cannot truncate the window's
  // promoted set), while `counts.by_state` stays global over the full snapshot
  // (the Mind Δ headline numbers read from counts, not items). The 15d window
  // gives weeklyChange's covered check real prior-week rows.
  // The 15d window is QUANTIZED to 6h buckets so the argv (and therefore the
  // projection cache key) is stable within a bucket — a per-render timestamp
  // would key a fresh cold exec on every request and defeat the cache tier.
  const sixHours = 6 * 3_600_000;
  const windowBucket = Math.floor(Date.now() / sixHours) * sixHours;
  const memoryWindowStart = new Date(windowBucket - 15 * 24 * 3_600_000).toISOString();
  const [attentionProjection, executors, workbenchRead, memoryRead, policyAuthority, homeDoctor] = await Promise.all([
    getAttentionProjection({ timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
    getExecutors(),
    runProjection(["workbench", "--json", "--recent", "500"], { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
    runProjection(
      ["memory", "candidates", "--json", "--state", "promoted", "--created-after", memoryWindowStart, "--limit", "4000"],
      { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS },
    ),
    runJson(["policy", "operator-authority", "--json"]),
    runProjection(["home", "doctor", "--json"], { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
  ]);
  const { read: attentionRead, report: attention } = attentionProjection;
  const workbench = workbenchRead.value;

  const now = new Date();
  const mutationsEnabled = computeConsoleMutationReadiness(consoleMutationsEnabled()).ready;

  // Attention — the ONE projection. Groups arrive leverage-sorted; `top` is the
  // single highest-leverage gate. Never re-ranked client-side.
  const needsYouGroups = attentionNeedsYouGroups(attention);
  const top = attention?.top ?? needsYouGroups[0] ?? null;
  const needsYouGroupCount = attention?.counts?.needs_you_groups ?? needsYouGroups.length;
  const decideGroups = needsYouGroups.slice(0, 6);
  const act = headlineAct(top, attentionRead.status);
  const oldest = oldestCreatedAt(needsYouGroups);
  // Any Class-A gate waiting → the hero glows amber (§9.2/§9.4).
  const classAWaiting = hasClassAGate(needsYouGroups);
  const runningAgents = arr(executors?.executors).filter((row) => num(row.running_count) > 0).length;

  // Continue + pipeline off the workbench projection.
  const open = arr(workbench?.open);
  const continueModel = deriveContinue(workbench, open);
  const stages = pipelineStages(workbench);
  const recent = arr(workbench?.recent);
  const compounded = latestProofBackedChange(recent);

  // World Δ (7-day move mix) + an honest week-over-week delta (only shown when
  // the recent window reaches back far enough to cover the prior week — §9.6).
  const mix = summarizeMoveTypeMix(recent);
  const worldChange = weeklyChange(
    recent,
    rowTsMs,
    (item) => moveTypeGroup(str(item.move_type) || str(item.move)) === "outward",
  );

  const memory = memoryBriefing(memoryRead);

  const autonomy = autonomyPosture(policyAuthority);
  const health = selfHealing(homeDoctor);

  // Decide/World read the ProjectionRead's own `generatedAt` (packet:
  // console-honest-degradation) so "unknown" appears only when a door has
  // genuinely never succeeded — never for a swallowed timeout that still has
  // a good last-good value to show.
  const stamps: { label: string; generatedAt: string; source: string }[] = [
    { label: "Decide", generatedAt: attentionRead.generatedAt ?? "", source: "compound attention --json" },
    { label: "World", generatedAt: workbenchRead.generatedAt ?? "", source: "compound workbench --json --recent 500" },
    { label: "Mind", generatedAt: memoryRead.generatedAt ?? "", source: "compound memory candidates --json" },
    { label: "Health", generatedAt: health.observation === "live" ? homeDoctor.generatedAt ?? "" : "", source: "compound home doctor --json" },
  ];

  return (
    <div className="console-stage mx-auto flex flex-col gap-7 pb-16" style={{ maxWidth: "1180px" }}>
      <BriefingHeader now={now} needsYouCount={needsYouGroupCount} oldest={oldest} />

      <div className="grid gap-4 lg:grid-cols-[1.3fr_.9fr]">
        <Headline
          top={top}
          act={act}
          mutationsEnabled={mutationsEnabled}
          runningAgents={runningAgents}
          classA={classAWaiting}
          read={{ status: attentionRead.status, lastGoodAt: attentionRead.lastGoodAt }}
        />
        <LatestMoment
          moment={compounded}
          read={{ status: workbenchRead.status, lastGoodAt: workbenchRead.lastGoodAt }}
        />
      </div>

      <div data-depth-min="understand">
        <CausalFocus top={top} openWork={open.length} activeLanes={runningAgents} />
      </div>

      <div data-depth-min="understand"><ContinueCard model={continueModel} mutationsReady={mutationsEnabled} /></div>

      <section data-depth-min="understand">
        <SectionLabel>Decide</SectionLabel>
        <DecideList
          groups={decideGroups}
          total={needsYouGroupCount}
          read={{ status: attentionRead.status, lastGoodAt: attentionRead.lastGoodAt }}
        />
        {attention ? (
          <OperatorCoordinationList projection={attention.operatorAttention} label="Authority references" />
        ) : null}
      </section>

      <div data-depth-min="understand"><KpiStrip
        world={{
          outward: mix.groups.outward,
          meaning: mix.groups.meaning,
          rules: mix.groups.rules,
          upkeep: mix.groups.upkeep,
          total: mix.total,
          lowOutward: mix.lowOutward,
        }}
        mind={memory}
        autonomy={autonomy}
        worldDelta={worldChange.covered ? worldChange.delta : null}
      /></div>

      {/* ---------- below the fold ---------- */}

      <div data-depth-min="investigate"><AttentionAgeMatrix
        groups={needsYouGroups}
        read={{ status: attentionRead.status, lastGoodAt: attentionRead.lastGoodAt }}
      /></div>

      <section data-depth-min="investigate">
        <SectionLabel>Pipeline</SectionLabel>
        <PipelineStrip stages={stages} read={{ status: workbenchRead.status, lastGoodAt: workbenchRead.lastGoodAt }} />
      </section>

      <div data-depth-min="technical"><SelfHealingPanel health={health} /></div>

      <section data-depth-min="investigate">
        <SectionLabel>Live activity</SectionLabel>
        <ActivityFeed recent={recent} read={{ status: workbenchRead.status, lastGoodAt: workbenchRead.lastGoodAt }} />
      </section>

      <section data-depth-min="technical">
        <SectionLabel>Knows</SectionLabel>
        <KnowsStrip memory={memory} />
      </section>

      <footer data-depth-min="technical" className="mt-4 flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-hair pt-4 text-2xs text-faint">
        <span>Every panel reads a live projection and stamps its own freshness.</span>
        <span className="flex flex-wrap items-center gap-2">
          {stamps.map((stamp) => (
            <span key={stamp.label} className="inline-flex items-center gap-1.5">
              <span className="text-faint">{stamp.label}</span>
              <FreshnessChip generatedAt={stamp.generatedAt || undefined} source={stamp.source} />
            </span>
          ))}
        </span>
      </footer>
    </div>
  );
}
