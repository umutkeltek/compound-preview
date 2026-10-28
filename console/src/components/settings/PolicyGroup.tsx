import { arr, num, str, type Json } from "@/lib/compound";

import { GavelIcon } from "@/components/kit";

import { Card, CountChip, DoctrineNote, FileOwnedFoot, KV, SettingsSection } from "./primitives";

/**
 * Group 2 — Policy. Effective placement mode + approval frontier + operator
 * prompts + release-goal holds, all read-only. Each card is footed with the
 * exact `compound policy import-…` command: these are file-owned authorities,
 * so the console shows what is in effect and never edits them.
 */
export function PolicyGroup({
  placement,
  frontier,
  prompts,
  holds,
  decisions,
  generatedAt,
}: {
  placement: Json | null;
  frontier: Json | null;
  prompts: Json | null;
  holds: Json | null;
  decisions: Json | null;
  generatedAt?: string;
}) {
  const placementMode = str(placement?.placement_guard_mode) || "—";
  const blockOwners = arr(placement?.block_owner_ids).map(String);
  const openWrites = num(placement?.open_write_count);
  const ownerCount = num(placement?.owner_count);

  const familyCount = num(frontier?.family_count);
  const deferredCount = num(frontier?.deferred_count);
  const openApprovals = num(frontier?.approval_required_open_count);
  const families = arr(frontier?.families);

  const promptCount = num(prompts?.prompt_count);
  const highSeverity = num(prompts?.high_severity_count);
  const promptRows = arr(prompts?.prompts);

  const holdCount = num(holds?.hold_count);
  const holdRows = arr(holds?.holds);

  const decisionRecords = num(decisions?.record_count);
  const decisionsByStatus = (decisions?.counts_by_status ?? {}) as Record<string, number>;
  const decisionsByKind = (decisions?.counts_by_kind ?? {}) as Record<string, number>;
  const decisionPrimary = num(decisionsByStatus.compound_primary);
  const decisionParked = num(decisionsByStatus.compound_parked);

  return (
    <SettingsSection
      index={2}
      icon={<GavelIcon />}
      tone="route"
      title="Policy"
      question="What does effective policy allow, forbid, and hold for your approval?"
      source="compound policy placement-authority --json"
      generatedAt={generatedAt}
    >
      <div className="grid gap-4 md:grid-cols-2">
        {/* Placement mode / forbidden owners. */}
        <Card
          title="Placement & forbidden paths"
          badge={<CountChip n={openWrites} label="open writes" tone={openWrites > 0 ? "seam" : "good"} />}
        >
          <KV k="placement_guard_mode" v={placementMode} />
          <KV k="owner_count" v={ownerCount} />
          <KV k="block owners" v={blockOwners.length ? blockOwners.join(", ") : "—"} mono={false} />
          <DoctrineNote>
            <span className="mt-2 block">
              Block-mode owners fail-closed at the PreToolUse gate; open writes are the reversible surface. Governance
              is enforced at the adapter, not asked for in a prompt.
            </span>
          </DoctrineNote>
          <FileOwnedFoot cmd="compound policy import-placement-rules <placement-rules.json>" />
        </Card>

        {/* Approval frontier. */}
        <Card
          title="Approval frontier"
          badge={<CountChip n={openApprovals} label="open" tone={openApprovals > 0 ? "seam" : "good"} />}
        >
          <KV k="families" v={familyCount} />
          <KV k="deferred" v={deferredCount} />
          <KV k="approval_required_open" v={openApprovals} />
          {families.length ? (
            <div className="mt-2 flex flex-col gap-1.5">
              {families.slice(0, 5).map((family: Json, index: number) => (
                <div key={str(family.id) || index} className="flex items-center justify-between gap-2 text-2xs">
                  <span className="mono min-w-0 truncate text-muted" title={str(family.id)}>
                    {str(family.id) || "—"}
                  </span>
                  <span className="shrink-0 text-faint">{str(family.current_decision) || "—"}</span>
                </div>
              ))}
            </div>
          ) : null}
          <FileOwnedFoot cmd="compound policy import-operator-approval-frontier <frontier.json>" />
        </Card>

        {/* Operator decision prompts. */}
        <Card
          title="Operator prompts"
          badge={<CountChip n={highSeverity} label="high" tone={highSeverity > 0 ? "seam" : "muted"} />}
        >
          <KV k="prompt_count" v={promptCount} />
          <KV k="high_severity_count" v={highSeverity} />
          {promptRows.length ? (
            <div className="mt-2 flex flex-col gap-1.5">
              {promptRows.slice(0, 4).map((prompt: Json, index: number) => (
                <div key={str(prompt.id) || index} className="flex items-baseline justify-between gap-2 text-2xs">
                  <span className="min-w-0 truncate text-muted" title={str(prompt.question)}>
                    {str(prompt.topic) || "prompt"} — {str(prompt.question) || "—"}
                  </span>
                  <span className="shrink-0 uppercase text-faint">{str(prompt.severity)}</span>
                </div>
              ))}
            </div>
          ) : (
            <DoctrineNote>No open prompts. Nothing is waiting on a written operator answer.</DoctrineNote>
          )}
          <FileOwnedFoot cmd="compound policy import-operator-prompts <prompts.json>" />
        </Card>

        {/* Release-goal holds. */}
        <Card
          title="Release-goal holds"
          badge={<CountChip n={holdCount} label="holds" tone={holdCount > 0 ? "seam" : "good"} />}
        >
          <KV k="hold_count" v={holdCount} />
          {holdRows.length ? (
            <div className="mt-2 flex flex-col gap-1.5">
              {holdRows.slice(0, 6).map((hold: Json, index: number) => (
                <div key={str(hold.goal_id) || index} className="flex items-center justify-between gap-2 text-2xs">
                  <span className="mono min-w-0 truncate text-muted" title={str(hold.objective)}>
                    {str(hold.app_id) || str(hold.goal_id) || "—"}
                  </span>
                  <span className="shrink-0 uppercase tracking-wide text-seam">{str(hold.state) || "HOLD"}</span>
                </div>
              ))}
            </div>
          ) : (
            <DoctrineNote>No release-goal holds. Nothing is frozen pending GO/HOLD evidence.</DoctrineNote>
          )}
          <FileOwnedFoot cmd="compound policy import-release-goal-holds <holds.json>" />
        </Card>

        {/* Decision source authority coverage. */}
        <Card
          title="Decision source authority"
          badge={<CountChip n={decisionRecords} label="records" tone="muted" />}
        >
          <KV k="record_count" v={decisionRecords} />
          <KV k="compound_primary" v={decisionPrimary} />
          <KV k="compound_parked" v={decisionParked} />
          <KV
            k="by kind"
            v={
              Object.entries(decisionsByKind)
                .map(([kind, count]) => `${kind}:${count}`)
                .join("  ") || "—"
            }
          />
          <DoctrineNote>
            <span className="mt-2 block">
              Imported decisions become compound-primary authority; the source markdown stays evidence-only after
              import.
            </span>
          </DoctrineNote>
          <FileOwnedFoot cmd="compound decisions import --policy-dir <decisions-dir>" />
        </Card>
      </div>
    </SettingsSection>
  );
}
