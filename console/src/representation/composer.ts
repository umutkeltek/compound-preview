/**
 * Composer v1 — deterministic evidence -> shape mapping. NO model calls in v1
 * (settled decision #2): a later model composer swaps in HERE, inside the
 * cluster, behind the same frozen interface (the tambo option in the ruling).
 *
 * The order is a priority ladder: the most specific structural signal wins, and
 * `story` catches everything that no shape beats. Every branch returns the
 * "chosen because" reason the shape self-declares on screen (like the reference
 * sim) — so the representation always explains why it took this form.
 */
import type { Evidence, ShapeChoice } from "./types";

function finiteCount(series: number[] | undefined): number {
  return (series ?? []).filter((v) => Number.isFinite(v)).length;
}

function hasReceipts(evidence: Evidence): boolean {
  return (evidence.receipt_refs ?? []).length > 0;
}

const ACTIVE_OR_DONE = new Set(["running", "in_progress", "verifying", "completed", "promoted", "needs_claim", "claimed"]);

export function chooseShape(evidence: Evidence): ShapeChoice {
  // 1. split-timeline — an expected/observed pair that disagrees (a divergence).
  if (evidence.expected_state && evidence.observed_state && evidence.expected_state !== evidence.observed_state) {
    return { shape: "split-timeline", reason: "expected and observed states disagree" };
  }

  // 2. collision — two statements that cancel each other (a contradiction).
  if (evidence.statements && evidence.statements[0] && evidence.statements[1]) {
    return { shape: "collision", reason: "two statements that cancel each other" };
  }

  // 3. ascii-pipe — a flow that gained or lost an edge.
  if (evidence.flow && evidence.flow.nodes.length >= 2) {
    return { shape: "ascii-pipe", reason: "a flow whose edges changed this cycle" };
  }

  // 4. sparkline — a quantity moving over time (needs >=2 real samples).
  if (finiteCount(evidence.series) >= 2) {
    return { shape: "sparkline", reason: "a quantity moving over time" };
  }

  // 5. heartbeat — a claim that asserts an outcome with no receipts to back it.
  const asserts = (evidence.state ? ACTIVE_OR_DONE.has(evidence.state) : false) || Boolean(evidence.success_predicate);
  if (asserts && !hasReceipts(evidence)) {
    return { shape: "heartbeat", reason: "a claim with no receipts to back it" };
  }

  // 6. story — text wins; no shape beats it.
  return { shape: "story", reason: "text wins — no shape beats it" };
}
