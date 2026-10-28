import { PageHeader } from "@/components/kit";
import { AutonomySafety } from "@/components/settings/AutonomySafety";
import { ConsoleEnv } from "@/components/settings/ConsoleEnv";
import { HarnessesHooks } from "@/components/settings/HarnessesHooks";
import { HealthBackup } from "@/components/settings/HealthBackup";
import { MemoryLenses } from "@/components/settings/MemoryLenses";
import { PolicyGroup } from "@/components/settings/PolicyGroup";
import { RoutingModels } from "@/components/settings/RoutingModels";
import { getAttentionProjection } from "@/lib/attention";
import { HEAVY_PROJECTION_TIMEOUT_MS, privateFallbacksEnabled, runCompound, runJson, runProjection, str, type Json } from "@/lib/compound";
import { consoleMutationsEnabled } from "@/lib/console-mutations";
import { computeConsoleMutationReadiness } from "@/lib/instance-pin";

export const dynamic = "force-dynamic";

/**
 * Settings — the compound console control surface (redesign plan Part 3).
 *
 * Doctrine: the console shows EFFECTIVE AUTHORITY (read-mostly). File-owned
 * state is edited via import commands, a few governed toggles could be editable,
 * and the dangerous ones (the kill switch) are guarded behind type-to-confirm.
 * Every group states the exact projection it read (FreshnessChip). Where a
 * mutation has no governed console path it shows the exact copy-command — never
 * a fabricated act (honesty rule).
 *
 * Group order is danger-first: autonomy/safety → policy → routing → memory →
 * harnesses → console/env (the coverage transparency table) → health/backup.
 */

/** `route promotion-review --json` emits a top-level ARRAY, which runJson (first
 * JSON object) cannot read — count it here from the raw text, defensively. */
async function promotionReviewCount(): Promise<number> {
  try {
    const { stdout } = await runCompound(["route", "promotion-review", "--json"]);
    const start = stdout.indexOf("[");
    if (start < 0) return 0;
    const parsed = JSON.parse(stdout.slice(start));
    return Array.isArray(parsed) ? parsed.length : 0;
  } catch {
    return 0;
  }
}

/** Harness list ignores --json and prints text; capture it for the parser. */
async function harnessListText(): Promise<string> {
  try {
    const { stdout } = await runCompound(["harness", "list"]);
    return stdout;
  } catch {
    return "";
  }
}

export default async function Settings() {
  const [
    halt,
    authority,
    grants,
    editable,
    placement,
    frontier,
    prompts,
    holds,
    decisions,
    routeEvidence,
    engine,
    hooks,
    doctor,
    attentionProjection,
    promotionCount,
    harnessText,
  ] = await Promise.all([
    runJson(["policy", "autonomy-halt", "--json"]),
    runJson(["policy", "operator-authority", "--json"]),
    runJson(["policy", "standing-grants-authority", "--json"]),
    runJson(["policy", "editable-surfaces", "--json"]),
    runJson(["policy", "placement-authority", "--json"]),
    runJson(["policy", "operator-approval-frontier-authority", "--json"]),
    runJson(["policy", "operator-prompts-authority", "--json"]),
    runJson(["policy", "release-goal-holds-authority", "--json"]),
    runJson(["decisions", "authority", "--json"]),
    runJson(["route", "evidence-authority", "--json"]),
    runJson(["memory", "engine-config", "--json"]),
    runProjection(["contract", "hooks-authority"]),
    runProjection(["home", "doctor", "--json"], { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
    getAttentionProjection(),
    promotionReviewCount(),
    harnessListText(),
  ]);

  const { read: attentionRead, report: attention } = attentionProjection;
  const mutationReadiness = computeConsoleMutationReadiness(consoleMutationsEnabled());
  const privateFallbacks = privateFallbacksEnabled();
  const gen = (value: Json | null): string | undefined => str(value?.generated_at) || undefined;

  return (
    <div className="flex flex-col gap-12">
      <PageHeader
        breadcrumb="control / settings"
        title="Settings"
        right={
          <span className="text-xs text-muted">
            Effective authority — read-mostly. File-owned state edits via import commands.
          </span>
        }
      />

      <AutonomySafety
        halt={halt}
        authority={authority}
        grants={grants}
        editable={editable}
        generatedAt={gen(grants)}
      />
      <PolicyGroup
        placement={placement}
        frontier={frontier}
        prompts={prompts}
        holds={holds}
        decisions={decisions}
        generatedAt={gen(prompts) || gen(frontier) || gen(holds)}
      />
      <RoutingModels routeEvidence={routeEvidence} promotionCount={promotionCount} />
      <MemoryLenses engine={engine} />
      <HarnessesHooks harnessText={harnessText} hooks={hooks} />
      <ConsoleEnv
        mutationsEnabled={mutationReadiness.ready}
        mutationFlagEnabled={mutationReadiness.flagEnabled}
        instancePinned={mutationReadiness.instancePinned}
        standardClientAuthorized={mutationReadiness.standardClientAuthorized}
        privateFallbacks={privateFallbacks}
        attention={attention}
        attentionRead={attentionRead}
        generatedAt={str(attention?.generated_at) || undefined}
      />
      <HealthBackup doctor={doctor} />
    </div>
  );
}
