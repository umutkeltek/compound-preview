import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";

import {
  LEASE_HISTORY_LABEL,
  LEASE_STALE_AFTER_SECONDS,
  hygieneStaleCount,
  leasePressureBars,
  leaseThresholdLabel,
  summarizeHygiene,
} from "../src/components/agents/executor-model.ts";

/*
 * The Agents screen and the briefing self-healing row both say "stale leases"
 * but count DIFFERENT populations, and the operator must never see the two
 * numbers without the UI naming which is which. This smoke pins both, live:
 *
 *   (a) `compound home doctor --json` — operational_hygiene.stale_lease_count
 *       (the briefing self-healing number): lease files whose work is terminal /
 *       archived / missing. Age-INDEPENDENT "unarchived lease history".
 *   (b) `compound executors --json` — counts.stale_leases mirrors (a) from the
 *       executors door, and counts.leases + per-lease ages feed the Agents
 *       lease-hygiene band's threshold count (summarizeHygiene): ACTIVE leases
 *       held past the 48h staleness threshold. Age-DEPENDENT.
 *
 * It asserts:
 *   1. HISTORY is one population, two doors: doctor.stale_lease_count ==
 *      executors.counts.stale_leases (they can never silently disagree).
 *   2. THRESHOLD count recomputes from the raw ages exactly as the band renders
 *      it, and is bounded by the ACTIVE lease population (live-stale ⊆ leases).
 *   3. The two populations are DISJOINT (residue vs active), so the two rendered
 *      numbers cannot be the same measurement that drifted — the per-executor
 *      splits reconcile to each door count.
 *   4. LABELS name distinct definitions: the history label carries residue
 *      vocabulary, the threshold label carries threshold vocabulary, and neither
 *      is the bare conflatable phrase "stale leases".
 *   5. The lease-pressure viz uses a bounded scale: DISTINCT ages must render
 *      DISTINCT bar heights (no saturation), and a lease well under threshold
 *      must not read as red.
 *
 * Wired into proof-check (registration landed with the coordination-view
 * land). Manual run (needs a built binary):
 *   COMPOUND_BIN=../compound bun scripts/agents-consistency-smoke.mjs
 */

const BIN = process.env.COMPOUND_BIN || "compound";

function runJson(args) {
  let out;
  try {
    out = execFileSync(BIN, args, { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
  } catch (err) {
    // `home doctor` exits non-zero when live hygiene/habitat is red but still
    // emits its full JSON report. This smoke pins door CONSISTENCY, not
    // hygiene health, so a parseable report on a red verdict is valid input;
    // anything without parseable stdout is a real failure (assert below).
    out = typeof err?.stdout === "string" ? err.stdout : "";
  }
  const start = out.indexOf("{");
  assert.ok(start >= 0, `no JSON object in \`${BIN} ${args.join(" ")}\` output`);
  return JSON.parse(out.slice(start));
}

const num = (v) => (typeof v === "number" && Number.isFinite(v) ? v : 0);
const optNum = (v) => (typeof v === "number" && Number.isFinite(v) ? v : null);
const str = (v) => (typeof v === "string" ? v : "");
const arr = (v) => (Array.isArray(v) ? v : []);

const doctor = runJson(["home", "doctor", "--json"]);
const executors = runJson(["executors", "--json"]);

/* ---- build the ExecutorProjection the Agents page normalizes to, so we drive
       the REAL summarizeHygiene / leasePressureBars rules the surfaces use ---- */
const doorCounts = executors.counts && typeof executors.counts === "object" ? executors.counts : {};
const staleAfterSeconds =
  optNum(executors.lease_stale_after_seconds) ??
  optNum(doorCounts.lease_stale_after_seconds) ??
  LEASE_STALE_AFTER_SECONDS;

const projectionExecutors = arr(executors.executors).map((raw) => ({
  id: str(raw.id),
  label: str(raw.label) || str(raw.id),
  leaseCount: num(raw.lease_count),
  oldestLeaseAgeSeconds: optNum(raw.oldest_lease_age_seconds),
  staleLeaseCount: num(raw.stale_lease_count),
  leases: arr(raw.leases).map((l) => ({ workId: str(l.work_id), ageSeconds: optNum(l.age_seconds) })),
}));
const projection = { staleAfterSeconds, executors: projectionExecutors };

/* ---- (1) HISTORY: one population, two doors ---- */
const historyDoctor = num(doctor.operational_hygiene?.stale_lease_count);
const historyExecutors = num(doorCounts.stale_leases);
assert.equal(
  historyDoctor,
  historyExecutors,
  `history/residue stale count disagrees across doors: home doctor operational_hygiene.stale_lease_count=${historyDoctor} ` +
    `vs executors counts.stale_leases=${historyExecutors} — the "unarchived lease history" number must be one thing, two doors`,
);

/* ---- (2) THRESHOLD: recomputes from raw ages == band render, bounded by active ---- */
const summary = summarizeHygiene(projection);
const bandStale = hygieneStaleCount(summary);

let recomputedThreshold = 0;
for (const e of projectionExecutors) {
  let hasStale = false;
  for (const l of e.leases) {
    if (l.ageSeconds !== null && l.ageSeconds > staleAfterSeconds) {
      recomputedThreshold += 1;
      hasStale = true;
    }
  }
  if (!hasStale && e.leaseCount > 0 && e.oldestLeaseAgeSeconds !== null && e.oldestLeaseAgeSeconds > staleAfterSeconds) {
    recomputedThreshold += 1; // executor-level fallback: age known, ids not
  }
}
assert.equal(
  bandStale,
  recomputedThreshold,
  `Agents band threshold count (${bandStale}) != independent recompute from raw lease ages (${recomputedThreshold})`,
);
const activeLeases = num(doorCounts.leases);
assert.ok(
  bandStale <= activeLeases,
  `threshold-stale (${bandStale}) must be a subset of ACTIVE leases (${activeLeases}) — live-stale ⊆ live-leases`,
);

/* ---- (3) DISJOINT populations: per-executor splits reconcile to each door count ---- */
const sumActive = projectionExecutors.reduce((s, e) => s + e.leaseCount, 0);
const sumResidue = projectionExecutors.reduce((s, e) => s + e.staleLeaseCount, 0);
assert.equal(sumActive, activeLeases, `Σ executor lease_count (${sumActive}) != counts.leases (${activeLeases})`);
assert.equal(
  sumResidue,
  historyExecutors,
  `Σ executor stale_lease_count (${sumResidue}) != counts.stale_leases (${historyExecutors})`,
);
// The active list never carries a residue lease (leaseWorkTerminal excludes
// them), so the two rendered numbers count non-overlapping sets by construction.

/* ---- (4) LABELS name distinct definitions ---- */
const historyLabel = LEASE_HISTORY_LABEL;
const thresholdLabel = leaseThresholdLabel(staleAfterSeconds);
const lc = (s) => s.toLowerCase();
assert.ok(
  /history|unarchived|residue|archive/.test(lc(historyLabel)),
  `history label "${historyLabel}" must name the residue population (history/unarchived), else it reads as the same "stale leases" the band shows`,
);
assert.ok(
  /threshold|past|held longer/.test(lc(thresholdLabel)),
  `threshold label "${thresholdLabel}" must name the threshold population (past the threshold), else it reads as the same "stale leases" the briefing shows`,
);
assert.notEqual(lc(historyLabel), lc(thresholdLabel), "the two stale-lease labels must not be identical");
for (const [what, label] of [["history", historyLabel], ["threshold", thresholdLabel]]) {
  assert.notEqual(lc(label).trim(), "stale leases", `${what} label must not be the bare conflatable phrase "stale leases"`);
  assert.notEqual(lc(label).trim(), "stale executor leases", `${what} label must not be the bare conflatable phrase "stale executor leases"`);
}

/* ---- (5) lease-pressure viz: bounded scale, no saturation ---- */
const HOUR = 3600;
const fixture = [
  { id: "f5d", label: "five", leaseCount: 1, oldestLeaseAgeSeconds: 5 * 24 * HOUR, leases: [{ workId: "w5", ageSeconds: 5 * 24 * HOUR }] },
  { id: "f4d", label: "four", leaseCount: 1, oldestLeaseAgeSeconds: 4 * 24 * HOUR, leases: [{ workId: "w4", ageSeconds: 4 * 24 * HOUR }] },
  { id: "f3d", label: "three", leaseCount: 1, oldestLeaseAgeSeconds: 3 * 24 * HOUR, leases: [{ workId: "w3", ageSeconds: 3 * 24 * HOUR }] },
  { id: "f12h", label: "half", leaseCount: 1, oldestLeaseAgeSeconds: 12 * HOUR, leases: [{ workId: "w12", ageSeconds: 12 * HOUR }] },
];
const { bars } = leasePressureBars(fixture, LEASE_STALE_AFTER_SECONDS);
assert.equal(bars.length, 4, "expected one bar per fixture executor");
const fillPct = (b) => Math.max(0, Math.min(100, (b.ageHours / Math.max(1, b.ceilingHours)) * 100));
const fills = bars.map(fillPct);
// DISTINCT ages must map to DISTINCT heights (the saturation bug collapses every
// past-threshold age to a full-height bar).
const distinct = new Set(fills.map((f) => f.toFixed(2)));
assert.equal(
  distinct.size,
  fills.length,
  `lease-pressure bars saturate: ages {5d,4d,3d,12h} rendered heights ${JSON.stringify(fills.map((f) => Math.round(f)))} — distinct ages must render distinct bar heights`,
);
// Strictly decreasing 5d > 4d > 3d > 12h.
for (let i = 1; i < fills.length; i++) {
  assert.ok(fills[i - 1] > fills[i], `bar heights must decrease with age; got ${JSON.stringify(fills.map((f) => Math.round(f)))}`);
}
// A lease well under threshold must not read as red.
const twelveH = bars[3];
assert.notEqual(twelveH.tone, "crit", `12h lease (ratio ${twelveH.ratio.toFixed(2)}) must not render red (crit)`);

console.log("agents-consistency-smoke");
console.log(`  history / unarchived-residue  = ${historyDoctor} (home doctor) == ${historyExecutors} (executors) [${historyLabel}]`);
console.log(`  threshold / live-past-${Math.round(staleAfterSeconds / HOUR)}h  = ${bandStale} of ${activeLeases} active [${thresholdLabel}]`);
console.log(`  populations disjoint: Σactive=${sumActive} Σresidue=${sumResidue}`);
console.log(`  lease-pressure fixture heights: ${JSON.stringify(fills.map((f) => Math.round(f)))} (12h tone=${twelveH.tone})`);
console.log("agents-consistency-smoke ok");
