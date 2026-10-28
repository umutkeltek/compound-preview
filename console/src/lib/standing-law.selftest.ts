/*
 * Depth test for the standing-law mapper (slice B falsifier, packet follow-up
 * rider). These assertions FAIL if the view were shallow — if a superseded
 * record leaked as live law, if the contradiction strip dropped, or if search
 * did not narrow to the right record. Pure (synthetic payload mirroring the
 * `compound decisions standing-law --json` shape); run via
 * scripts/standing-law-smoke.mjs.
 */

import { buildStandingLaw, filterStandingLaw, type StandingLawRecord } from "./standing-law";

type Check = { name: string; ok: boolean; detail: string };

/** A payload with 2 live records (one superseding an older ruling) + 1 conflict
 *  candidate — exactly the falsifier's seed, as the CLI would emit it. The
 *  superseded 'old-exec-lane' appears ONLY nested under its successor, never as
 *  a top-level record (that is what the CLI's AllActiveDecisions guarantees). */
function seededPayload() {
  return {
    schema_version: "compound.standing_law_view.v0.1",
    records: [
      {
        claim_id: "claim_grant0001",
        decision_id: "director-apply-grant",
        kind: "grant",
        title: "Let the director apply job-health work on its own",
        scope: "autonomy-evidence-threshold",
        reviewed_by: "operator:test",
        date: "2026-07-11T05:00:00Z",
        keywords: ["job_health"],
        corroboration_count: 2,
        corroboration_evidence: ["ev_aaa", "ev_bbb"],
        supersedes: [],
      },
      {
        claim_id: "claim_ruling0002",
        decision_id: "new-exec-lane",
        kind: "ruling",
        title: "Execution routes through Codex only, never Opus subagents",
        reviewed_by: "operator:test",
        date: "2026-07-11T03:00:00Z",
        falsifier: "A later decisions record supersedes this one.",
        review_trigger: "Re-check when a superseding record names this id.",
        source_path: "/home/.compound/decisions/operator/new-exec-lane.md",
        corroboration_count: 0,
        supersedes: [
          {
            decision_id: "old-exec-lane",
            claim_id: "claim_ruling0001",
            kind: "ruling",
            title: "Execution routes through Sonnet subagents",
            date: "2026-07-11T01:00:00Z",
          },
        ],
      },
    ],
    conflicts: [
      {
        candidate_id: "harvest-ruling-deadbeef",
        kind: "ruling",
        restatement: "Ruling: Execution should route through Opus subagents",
        verbatim: "actually execution should route through Opus subagents for everything",
        session: "sess-conflict",
        transcript: "/tmp/sess-conflict.jsonl",
        line: "42",
        conflicts_with_claim: "claim_ruling0002",
        conflicts_with_decision: "new-exec-lane",
        conflicting_title: "Execution routes through Codex only, never Opus subagents",
      },
    ],
    counts: { records: 2, rulings: 1, grants: 1, freezes: 0, superseded: 1, conflicts: 1 },
  };
}

export function assertStandingLawModel(
  model: ReturnType<typeof buildStandingLaw>,
  push: (name: string, ok: boolean, detail: string) => void,
) {
  const live = new Map<string, StandingLawRecord>(model.records.map((r) => [r.decisionId, r]));

  // 1. Both live records render.
  push(
    "both-live-records-render",
    live.has("director-apply-grant") && live.has("new-exec-lane"),
    `live: ${[...live.keys()].join(",") || "none"}`,
  );

  // 2. The superseded record is NEVER a live top-level row.
  push(
    "superseded-never-live",
    !live.has("old-exec-lane"),
    live.has("old-exec-lane") ? "old-exec-lane leaked as live law" : "excluded from live rows",
  );

  // 3. The supersession chain hangs the old ruling under its successor, titled.
  const successor = live.get("new-exec-lane");
  push(
    "supersession-chain-nested-and-titled",
    Boolean(
      successor &&
        successor.supersedes.length === 1 &&
        successor.supersedes[0].decisionId === "old-exec-lane" &&
        successor.supersedes[0].title.length > 0,
    ),
    `supersedes: ${JSON.stringify(successor?.supersedes ?? [])}`,
  );

  // 4. Newest-first: the grant (latest date) leads.
  push(
    "newest-first-ordering",
    model.records[0]?.decisionId === "director-apply-grant",
    `head: ${model.records[0]?.decisionId}`,
  );

  // 5. The contradiction strip is present and links the conflicting live claim.
  push(
    "conflict-strip-present-and-linked",
    model.conflicts.length === 1 &&
      model.conflicts[0].conflictsWithClaim === "claim_ruling0002" &&
      model.conflicts[0].conflictingTitle.length > 0,
    `conflicts: ${JSON.stringify(model.conflicts.map((c) => c.conflictsWithClaim))}`,
  );

  // 6. Corroboration count survives the mapping (restatement meter).
  push(
    "corroboration-count-mapped",
    live.get("director-apply-grant")?.corroborationCount === 2,
    `grant corroborations: ${live.get("director-apply-grant")?.corroborationCount}`,
  );

  // 7. Search narrows to the right record ("codex" hits only the exec-lane ruling).
  const codexHits = filterStandingLaw(model.records, "codex");
  push(
    "search-narrows-to-record",
    codexHits.length === 1 && codexHits[0].decisionId === "new-exec-lane",
    `codex hits: ${codexHits.map((r) => r.decisionId).join(",") || "none"}`,
  );

  // 8. A non-matching query narrows to nothing (no accidental match-all).
  const miss = filterStandingLaw(model.records, "kubernetes");
  push("search-empty-on-miss", miss.length === 0, `miss hits: ${miss.length}`);

  // 9. Empty query returns every record (search box does not hide law by default).
  const all = filterStandingLaw(model.records, "");
  push("empty-query-returns-all", all.length === model.records.length, `all: ${all.length}`);
}

export function runStandingLawSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  const model = buildStandingLaw(seededPayload());
  assertStandingLawModel(model, push);

  // Defensive: a null payload yields an empty, non-crashing model.
  const empty = buildStandingLaw(null);
  push(
    "null-payload-empty-model",
    empty.records.length === 0 && empty.conflicts.length === 0 && empty.counts.records === 0,
    `records=${empty.records.length} conflicts=${empty.conflicts.length}`,
  );

  const details = checks.map((c) => `${c.ok ? "ok  " : "FAIL"} ${c.name} — ${c.detail}`);
  return { ok: checks.every((c) => c.ok), details };
}
