import { notFound } from "next/navigation";

import { Card, Chip, EmptyState, StateBadge } from "@/components/ui";
import { WorkDetailActions } from "@/components/work/WorkDetailActions";
import { arr, getWork, getWorkLeases, str } from "@/lib/compound";
import { consoleMutationsEnabled } from "@/lib/console-mutations";
import { computeConsoleMutationReadiness } from "@/lib/instance-pin";
import { buildActionRow } from "@/lib/work-pipeline";
import {
  enrichWorkItem,
  findWorkbenchItem,
  packetForSurface,
  shortText,
} from "@/lib/work-data";
import { selectedContinueView, selectedWorkLease } from "@/lib/work-selected-continue";

export const dynamic = "force-dynamic";

function TextBlock({ title, body }: { title: string; body: string }) {
  return (
    <div>
      <div className="section-label mb-1">{title}</div>
      <p className="text-base leading-relaxed text-ink-2">{body || "Not surfaced yet."}</p>
    </div>
  );
}

function BulletList({ items, empty }: { items: string[]; empty: string }) {
  if (!items.length) return <EmptyState>{empty}</EmptyState>;
  return (
    <ul className="flex flex-col gap-2">
      {items.map((item) => (
        <li key={item} className="rounded-lg bg-soft px-3 py-2 text-sm leading-relaxed text-ink-2">
          {item}
        </li>
      ))}
    </ul>
  );
}

function CodeList({ items, empty }: { items: string[]; empty: string }) {
  if (!items.length) return <EmptyState>{empty}</EmptyState>;
  return (
    <ul className="flex flex-col gap-1.5">
      {items.map((ref) => (
        <li key={ref} className="mono break-all text-xs leading-relaxed text-ink-2">
          {ref}
        </li>
      ))}
    </ul>
  );
}

export default async function WorkDetail({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const work = await getWork(id);
  if (!work) notFound();
  const leases = await getWorkLeases();
  const execution = selectedContinueView(work, selectedWorkLease(leases, id));

  const workbenchItem = await findWorkbenchItem(id);
  const merged = await enrichWorkItem({ ...(workbenchItem ?? {}), ...work, work_id: id });
  const packet = merged.packet ?? (await packetForSurface(str(work.canonical_surface)));
  const state = str(work.state);
  const evidence = arr(work.evidence_refs).map(String);
  const nonGoals = arr(work.non_goals).map(String);
  const ids = (work.ids ?? {}) as Record<string, unknown>;
  const receiptId = str(workbenchItem?.receipt_id) || str(work.receipt_id);
  const transitionId = str(workbenchItem?.transition_id) || str(ids.transition_id) || str(work.transition_id);
  const actionContext = ((merged.action_context ?? work.action_context ?? {}) || {}) as Record<string, unknown>;
  const currentWait =
    str(merged.next_action) ||
    str(work.next_action) ||
    (state === "held" ? "This work needs your input before Compound can continue." : "No current wait surfaced.");
  const whyExists =
    str(actionContext.title) ||
    str(actionContext.action_summary) ||
    str(work.review_trigger) ||
    str(merged.open_reason) ||
    str(work.open_reason) ||
    str(work.canonical_surface) ||
    "This work was created from Compound-owned evidence or a reviewed action.";
  const expectedProof =
    str(actionContext.proof) ||
    str(work.success_predicate) ||
    str(merged.success_predicate) ||
    "Proof expectation has not been surfaced for this work item.";

  const actionRow = buildActionRow(id, state);
  const mutationsReady = computeConsoleMutationReadiness(consoleMutationsEnabled()).ready;

  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <div className="mb-2 flex flex-wrap items-center gap-2">
            <StateBadge state={state} />
            {str(work.owner) ? <Chip tone="violet">{str(work.owner)}</Chip> : null}
            {str(work.claimed_by) ? <Chip tone="blue">{str(work.claimed_by)}</Chip> : null}
          </div>
          <h1 className="max-w-4xl text-xl font-semibold leading-tight tracking-tight">
            {packet?.title || str(work.canonical_surface) || id}
          </h1>
          <p className="mt-1 mono text-ink-3">{id}</p>
        </div>
      </div>

      <div className="work-detail-grid">
        <div className="flex flex-col gap-4">
          <Card title="Objective" accent="blue" glyph="O">
            <TextBlock title="What this task is" body={packet?.objective || str(work.desired_outcome)} />
          </Card>

          <Card title="Why this exists" accent="violet" glyph="?">
            <TextBlock title="Source reason" body={whyExists} />
          </Card>

          <Card title="Current wait" accent={state === "held" || state === "blocked" ? "orange" : "green"} glyph="!">
            <TextBlock title="What needs to happen" body={currentWait} />
          </Card>

          <Card title="Boundaries" accent="orange" glyph="B">
            <BulletList items={[...(packet?.boundaries ?? []), ...nonGoals]} empty="No boundaries found." />
          </Card>
        </div>

        <div className="flex flex-col gap-4">
          <Card title="Expected proof" accent="green" glyph="P">
            <TextBlock title="Good result looks like" body={expectedProof} />
          </Card>

          <Card title="Proof trail" accent="green" glyph="✓">
            <div className="flex flex-col gap-4">
              <div>
                <div className="section-label mb-1">Evidence</div>
                <CodeList items={evidence} empty="No evidence refs surfaced." />
              </div>
              {receiptId || transitionId ? (
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-1">
                  {receiptId ? (
                    <div>
                      <div className="section-label mb-1">Proof ID</div>
                      <div className="mono break-all text-xs leading-relaxed text-ink-2">{receiptId}</div>
                    </div>
                  ) : null}
                  {transitionId ? (
                    <div>
                      <div className="section-label mb-1">Recorded change</div>
                      <div className="mono break-all text-xs leading-relaxed text-ink-2">{transitionId}</div>
                    </div>
                  ) : null}
                </div>
              ) : null}
            </div>
          </Card>

          <Card title="Execution" accent="violet" glyph="▶">
            <div className="flex flex-col gap-3 text-sm text-ink-2">
              <div>
                <div className="font-semibold text-ink">{execution.status}</div>
                <p className="mt-1">{execution.detail}</p>
              </div>
              <TextBlock title="Goal" body={execution.goal} />
              <TextBlock title="Saved selection" body={execution.selection} />
              {execution.contextPackId ? <div><div className="section-label mb-1">Context pack</div><p className="mono break-all text-xs">{execution.contextPackId}</p></div> : null}
              {execution.actor ? <div><div className="section-label mb-1">Current lease holder</div><p className="mono break-all text-xs">{execution.actor}</p></div> : null}
              {execution.runAttemptIds.length ? <div><div className="section-label mb-1">Run attempts</div><CodeList items={execution.runAttemptIds} empty="No run attempt recorded." /></div> : null}
              <p className="text-xs text-muted">This browser view is read-only. An authorized client may request one exact-work detached run; verification and close remain separate.</p>
            </div>
          </Card>
          <Card title="Move & trace" accent="blue" glyph="→">
            <div className="flex flex-col gap-3">
              <WorkDetailActions workId={id} actionRow={actionRow} mutationsReady={mutationsReady} />

              {packet?.path ? (
                <details>
                  <summary className="cursor-pointer text-sm font-medium text-ink-2 hover:text-ink">
                    Packet source
                  </summary>
                  <p className="mono mt-2 break-all text-xs leading-relaxed text-ink-3">
                    {shortText(packet.path, 260)}
                  </p>
                </details>
              ) : null}
            </div>
          </Card>
        </div>
      </div>
    </div>
  );
}
