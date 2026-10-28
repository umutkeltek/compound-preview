/*
 * Falsifier for the keep-aside-drain lane (work_4033b98c5837, standing-law
 * queue side). Before this fix, `compound memory adjudicate --verdict park`
 * left no trace distinguishing "operator explicitly kept this aside" from "sat
 * parked since ingestion, nobody has looked" — so a card the operator dismissed
 * kept reappearing in the /memory standing-law "needs your call" queue forever.
 *
 * isStandingLawCandidate (memory-data.ts) is the authority filter behind that
 * queue. Run via scripts/memory-data-smoke.mjs.
 */

import { isStandingLawCandidate, standingLawReadinessChip } from "./memory-data";

type Check = { name: string; ok: boolean; detail: string };

export function runMemoryDataSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  // A raw harvested parked decision candidate (cand_9df96833602e's shape
  // BEFORE the operator ever ran `memory adjudicate` on it) — no
  // adjudicated_by. Must still render: nobody has ruled on it yet.
  const rawHarvested = {
    id: "cand_9df96833602e",
    entity: "decision:brain-80e65c8a4ccd",
    claim_type: "decision",
    state: "parked",
  };
  push(
    "untouched-parked-decision-still-renders",
    isStandingLawCandidate(rawHarvested) === true,
    `isStandingLawCandidate(untouched) = ${isStandingLawCandidate(rawHarvested)}`,
  );

  // The SAME candidate after the operator runs `compound memory adjudicate
  // <id> --verdict park` (AdjudicateAsOperator stamps adjudicated_by). Must
  // leave the needs-your-call queue — it stays parked as evidence, but the
  // operator already ruled "keep aside".
  const operatorParked = { ...rawHarvested, adjudicated_by: "operator", adjudicated_at: "2026-07-13T12:00:00Z" };
  push(
    "operator-adjudicated-park-excluded",
    isStandingLawCandidate(operatorParked) === false,
    `isStandingLawCandidate(operator-parked) = ${isStandingLawCandidate(operatorParked)}`,
  );

  // A second real falsifier id (cand_77d12450595e), same shape, same result —
  // the predicate is general, not keyed to one candidate.
  const operatorParked2 = {
    id: "cand_77d12450595e",
    entity: "decision:brain-9614adef7d64",
    claim_type: "decision",
    state: "parked",
    adjudicated_by: "operator",
    adjudicated_at: "2026-07-13T12:05:00Z",
  };
  push(
    "second-operator-adjudicated-park-excluded",
    isStandingLawCandidate(operatorParked2) === false,
    `isStandingLawCandidate(operator-parked-2) = ${isStandingLawCandidate(operatorParked2)}`,
  );

  // An automated (non-operator) park — e.g. panel apply — never stamps
  // adjudicated_by (mem.Store.Adjudicate, not AdjudicateAsOperator), so it
  // keeps rendering exactly like the untouched case.
  const automatedPark = { ...rawHarvested, state: "parked" };
  push(
    "automated-unstamped-park-still-renders",
    isStandingLawCandidate(automatedPark) === true,
    `isStandingLawCandidate(automated) = ${isStandingLawCandidate(automatedPark)}`,
  );

  // Whitespace-only adjudicated_by must not accidentally exclude (defensive:
  // the field is trimmed before the emptiness check).
  const whitespaceOnly = { ...rawHarvested, adjudicated_by: "   " };
  push(
    "whitespace-adjudicated-by-does-not-exclude",
    isStandingLawCandidate(whitespaceOnly) === true,
    `isStandingLawCandidate(whitespace) = ${isStandingLawCandidate(whitespaceOnly)}`,
  );

  // An operator-adjudicated candidate that is NOT parked (e.g. promoted) is
  // excluded on state alone — the adjudicated_by check must not be the only
  // gate keeping non-parked rows out.
  const promoted = { ...operatorParked, state: "promoted" };
  push(
    "non-parked-state-still-excluded",
    isStandingLawCandidate(promoted) === false,
    `isStandingLawCandidate(promoted) = ${isStandingLawCandidate(promoted)}`,
  );

  // Non-decision knowledge-corpus rows stay excluded regardless of
  // adjudicated_by (must not accidentally admit the 28k-row flood).
  const knowledgeRow = { id: "cand_x", entity: "x_like:123", claim_type: "", state: "parked" };
  push(
    "non-decision-parked-row-excluded",
    isStandingLawCandidate(knowledgeRow) === false,
    `isStandingLawCandidate(knowledge) = ${isStandingLawCandidate(knowledgeRow)}`,
  );

  // ----------------------------------------------------------------------
  // F3 (packet work_709c9ba49c56): standing-law-candidate readiness chip.
  // Derived-only signal projected by `compound memory candidates --json`.
  // The chip renders on a parked decision candidate when
  //   recurrence_count >= 2 AND confidence >= 0.8
  // and reports the trend label projected by the same call. A qualifying
  // candidate MUST render the chip; a non-qualifying one MUST NOT.
  // ----------------------------------------------------------------------

  // Qualifying candidate: 2 distinct evidence refs + high confidence + rising.
  const qualifyingRising = {
    id: "cand_ready_001",
    entity: "decision:brain-ready001",
    claim_type: "decision",
    state: "parked",
    confidence: 0.9,
    recurrence_count: 2,
    confidence_trend: "rising",
  };
  const chipRising = standingLawReadinessChip(qualifyingRising);
  push(
    "qualifying-rising-renders-chip",
    chipRising !== null && chipRising.label === "seen 2× · rising" && chipRising.tone === "good",
    `chip=${JSON.stringify(chipRising)}`,
  );

  // Qualifying candidate: 3 distinct evidence refs + high confidence + flat.
  const qualifyingFlat = {
    id: "cand_ready_002",
    entity: "decision:brain-ready002",
    claim_type: "decision",
    state: "parked",
    confidence: 0.85,
    recurrence_count: 3,
    confidence_trend: "flat",
  };
  const chipFlat = standingLawReadinessChip(qualifyingFlat);
  push(
    "qualifying-flat-renders-chip-with-seam-tone",
    chipFlat !== null && chipFlat.label === "seen 3× · flat" && chipFlat.tone === "seam",
    `chip=${JSON.stringify(chipFlat)}`,
  );

  // Non-qualifying: only one evidence ref (operator has not corroborated).
  const singleRef = {
    id: "cand_not_ready_001",
    entity: "decision:brain-notready001",
    claim_type: "decision",
    state: "parked",
    confidence: 0.95,
    recurrence_count: 1,
    confidence_trend: "rising",
  };
  push(
    "single-ref-candidate-skipped",
    standingLawReadinessChip(singleRef) === null,
    `chip=${JSON.stringify(standingLawReadinessChip(singleRef))}`,
  );

  // Non-qualifying: low confidence (must not promote on a single high-conf
  // observation either — both gates must hold).
  const lowConfidence = {
    id: "cand_not_ready_002",
    entity: "decision:brain-notready002",
    claim_type: "decision",
    state: "parked",
    confidence: 0.5,
    recurrence_count: 4,
    confidence_trend: "rising",
  };
  push(
    "low-confidence-candidate-skipped",
    standingLawReadinessChip(lowConfidence) === null,
    `chip=${JSON.stringify(standingLawReadinessChip(lowConfidence))}`,
  );

  // Non-qualifying: missing derived fields (older binary without the
  // projection must not render a chip — the chip is a derived-only signal,
  // never inferred from arbitrary numeric inputs).
  const missingSignals = {
    id: "cand_not_ready_003",
    entity: "decision:brain-notready003",
    claim_type: "decision",
    state: "parked",
    confidence: 0.9,
  };
  push(
    "missing-derived-fields-candidate-skipped",
    standingLawReadinessChip(missingSignals) === null,
    `chip=${JSON.stringify(standingLawReadinessChip(missingSignals))}`,
  );

  // Non-qualifying: a candidate cited by 2 distinct refs but whose confidence
  // exactly equals the 0.8 threshold (boundary case — the comparison is
  // strict-less-than, so 0.8 itself qualifies).
  const boundaryConf = {
    id: "cand_boundary_001",
    entity: "decision:brain-boundary001",
    claim_type: "decision",
    state: "parked",
    confidence: 0.8,
    recurrence_count: 2,
    confidence_trend: "rising",
  };
  push(
    "confidence-equals-threshold-qualifies",
    standingLawReadinessChip(boundaryConf) !== null,
    `chip=${JSON.stringify(standingLawReadinessChip(boundaryConf))}`,
  );

  const details = checks.map((c) => `${c.ok ? "ok  " : "FAIL"} ${c.name} — ${c.detail}`);
  return { ok: checks.every((c) => c.ok), details };
}
