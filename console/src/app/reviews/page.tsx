import { DecideQueue } from "@/components/decide/DecideQueue";
import { CheckIcon, ClockIcon, FeedDegraded, feedRenderModel, GavelIcon, PageHeader, SectionHeader } from "@/components/kit";
import { attentionNeedsYouGroups, getAttentionProjection } from "@/lib/attention";
import { arr, getReviews, HEAVY_PROJECTION_TIMEOUT_MS, num, str, type Json } from "@/lib/compound";
import { ago } from "@/lib/time";
import { consoleMutationsEnabled } from "@/lib/console-mutations";
import { computeConsoleMutationReadiness } from "@/lib/instance-pin";
import { buildDecideGroups } from "@/lib/decide-model";
import { areaPlain } from "@/lib/glossary";
import { areaOf, bandOf, enrichAttentionGroups, externalReviewRows, groupsForBand } from "@/lib/review-data";

export const dynamic = "force-dynamic";

/* ---------- item index + sample resolution (unchanged data contract) ----------
 * The Decide page renders the SAME ordered needs-you group set as
 * `compound attention --json`. Reviews only enrich those identities with item
 * detail. Phase 3b only changes how those groups are PRESENTED and ACTED on
 * (kit cards, honest batch modes, keyboard triage) — not which groups show.
 */

function buildItemIndex(reviews: Json | null): Map<string, Json> {
  const map = new Map<string, Json>();
  for (const item of arr(reviews?.items)) {
    const id = str(item.id) || str(item.review_id);
    if (id) map.set(id, item);
  }
  return map;
}

function sampleForGroup(group: Json, itemByID: Map<string, Json>): Json {
  const sample = group.sample;
  if (sample && typeof sample === "object") return sample as Json;
  for (const id of arr(group.item_ids).map(String)) {
    const item = itemByID.get(id);
    if (item) return item;
  }
  return {
    id: str(group.key),
    area: areaOf(group),
    readiness: str(group.readiness),
    status: str(group.status),
    next_action: str(group.operator_summary),
    operator_summary: str(group.operator_summary),
    operator_unlock: str(group.operator_unlock),
    group_key: str(group.key),
    evidence_refs: arr(group.evidence_refs),
  };
}

function groupCreatedAt(group: Json, itemByID: Map<string, Json>): string {
  const sample = group.sample;
  const sampleCreated = sample && typeof sample === "object" ? str((sample as Json).created_at) : "";
  if (sampleCreated) return sampleCreated;
  let min = "";
  for (const id of arr(group.item_ids).map(String)) {
    const created = str(itemByID.get(id)?.created_at);
    if (created && (!min || created < min)) min = created;
  }
  return min || str(group.created_at);
}

function sortByAge(rows: Json[], itemByID: Map<string, Json>): Json[] {
  return rows
    .slice()
    .sort((a, b) => (groupCreatedAt(a, itemByID) || "9999").localeCompare(groupCreatedAt(b, itemByID) || "9999"));
}

function asWatchingEntry(row: Json): Json {
  return {
    ...row,
    count: num(row.count) || 1,
    item_ids: arr(row.item_ids).length ? row.item_ids : [str(row.id) || str(row.review_id)],
  };
}

/* ---------- watching band (informational — no buttons, on purpose) ---------- */
function WatchingRow({ entry, itemByID }: { entry: Json; itemByID: Map<string, Json> }) {
  const summary = str(entry.operator_summary) || "Still watching.";
  const area = areaOf(entry);
  const count = num(entry.count) || arr(entry.item_ids).length || 1;
  const createdAt = groupCreatedAt(entry, itemByID);
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 border-b border-hair px-3 py-2.5 last:border-b-0">
      <div className="flex min-w-0 items-center gap-2">
        {area ? (
          <span
            className="shrink-0 rounded-sm bg-soft-2 px-1.5 py-0.5 text-2xs font-semibold uppercase tracking-wide text-faint"
            title={areaPlain(area)}
          >
            {area.replace(/-/g, " ")}
          </span>
        ) : null}
        <span className="min-w-0 truncate text-sm text-muted">{summary}</span>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {count > 1 ? <span className="mono text-2xs tabular-nums text-faint">{count}</span> : null}
        <span className="text-xs tabular-nums text-faint">{createdAt ? ago(createdAt) : ""}</span>
      </div>
    </div>
  );
}

export default async function Decide({
  searchParams,
}: {
  searchParams: Promise<{ decision?: string | string[] }>;
}) {
  const [reviews, attentionProjection, params] = await Promise.all([
    getReviews({ timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
    getAttentionProjection({ timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
    searchParams,
  ]);
  const { read: attentionRead, report: attention } = attentionProjection;
  const itemByID = buildItemIndex(reviews);
  const mutationsEnabled = computeConsoleMutationReadiness(consoleMutationsEnabled()).ready;
  const initialSelectedKey = Array.isArray(params.decision) ? str(params.decision[0]) : str(params.decision);

  const externalRows = externalReviewRows(reviews);
  const externalWatching = externalRows.filter((r) => bandOf(r) === "watching");

  // Attention owns both membership and order. Reviews only supply richer
  // samples for identities already selected by that projection.
  const reviewEnrichmentGroups = [
    ...arr(reviews?.groups).filter((group: Json) => bandOf(group) === "needs_you"),
    ...groupsForBand(reviews, "needs_you", attention),
  ];
  const orderedGroups = enrichAttentionGroups(attentionNeedsYouGroups(attention), reviewEnrichmentGroups);
  const decideGroups = buildDecideGroups(
    orderedGroups,
    itemByID,
    attention,
    (g) => sampleForGroup(g, itemByID),
    attentionRead.status,
  );
  // Feature-detect a queue-level `systemic_blocker` reason the projection
  // may start carrying directly; DecideQueue also computes the
  // all-identical-reasons case itself from decideGroups, so the banner is
  // correct with or without this field.
  const systemicBlockerReason = str(reviews?.systemic_blocker) || null;

  const watchingEntries = sortByAge(
    [...groupsForBand(reviews, "watching", attention), ...externalWatching.map(asWatchingEntry)],
    itemByID,
  );

  const allItems = arr(reviews?.items);
  const filedItems = allItems.filter((it) => bandOf(it) === "filed");
  const filedTotal = filedItems.length;
  const filedByAreaMap = new Map<string, number>();
  for (const item of filedItems) {
    const area = areaOf(item) || "unclassified";
    filedByAreaMap.set(area, (filedByAreaMap.get(area) ?? 0) + 1);
  }
  const filedByArea = [...filedByAreaMap.entries()].sort((a, b) => b[1] - a[1]);

  const empty = decideGroups.length === 0;
  const decideRender = feedRenderModel(attentionRead.status, decideGroups.length > 0);
  const initialGroup = initialSelectedKey
    ? decideGroups.find((group) => group.key === initialSelectedKey)
    : null;
  const panelDetailOpen = Boolean(initialGroup?.panel);

  return (
    <div className={`flex flex-col ${panelDetailOpen ? "gap-3" : "gap-6"}`}>
      <PageHeader breadcrumb={panelDetailOpen ? undefined : "operate / decide"} title="Decide" />

      <section className="flex flex-col gap-3">
        {panelDetailOpen ? null : (
          <SectionHeader
            tone="seam"
            icon={<GavelIcon />}
            label="Needs your decision"
          />
        )}

        {decideRender === "degraded" ? (
          <FeedDegraded
            label="Decide"
            source="compound attention --json"
            lastGoodAt={attentionRead.lastGoodAt}
          />
        ) : empty ? (
          <p className="rounded-lg border border-dashed border-hair bg-page px-4 py-8 text-center text-sm text-muted">
            Nothing is ready for a decision right now.
          </p>
        ) : (
          <DecideQueue
            groups={decideGroups}
            mutationsEnabled={mutationsEnabled}
            initialSelectedKey={initialSelectedKey}
            systemicBlockerReason={systemicBlockerReason}
          />
        )}
      </section>

      {watchingEntries.length ? (
        <details className="group overflow-hidden rounded-lg border border-hair bg-panel shadow-sm">
          <summary className="flex cursor-pointer list-none items-center gap-3 px-4 py-3 marker:hidden">
            <span className="inline-flex h-6 w-6 items-center justify-center rounded-sm bg-soft-2 text-sm text-muted"><ClockIcon /></span>
            <span className="flex-1 text-xs font-bold uppercase text-muted">Watching</span>
            <span className="mono text-2xs tabular-nums text-faint">{watchingEntries.length}</span>
            <span aria-hidden className="text-sm text-faint transition-transform group-open:rotate-90">›</span>
          </summary>
          <div className="border-t border-hair">
            {watchingEntries.map((entry, index) => (
              <WatchingRow key={str(entry.key) || str(entry.id) || index} entry={entry} itemByID={itemByID} />
            ))}
          </div>
        </details>
      ) : null}

      <details className="group overflow-hidden rounded-lg border border-hair bg-panel shadow-sm">
        <summary className="flex cursor-pointer list-none items-center gap-3 px-4 py-3 marker:hidden">
          <span className="inline-flex h-6 w-6 items-center justify-center rounded-sm bg-good-soft text-sm text-good"><CheckIcon /></span>
          <span className="flex-1 text-xs font-bold uppercase text-muted">Filed</span>
          <span className="mono text-2xs tabular-nums text-faint">{filedTotal}</span>
          <span aria-hidden className="text-sm text-faint transition-transform group-open:rotate-90">›</span>
        </summary>
        <div className="border-t border-hair px-4 py-3">
          {filedByArea.length ? (
            <div className="flex flex-wrap gap-2">
              {filedByArea.map(([area, count]) => (
                <span
                  key={area}
                  title={areaPlain(area)}
                  className="rounded-md border border-hair bg-soft px-2.5 py-1 text-xs text-muted"
                >
                  {area.replace(/-/g, " ")} <span className="mono tabular-nums text-faint">· {count}</span>
                </span>
              ))}
            </div>
          ) : (
            <p className="text-xs text-faint">Nothing filed yet.</p>
          )}
        </div>
      </details>
    </div>
  );
}
