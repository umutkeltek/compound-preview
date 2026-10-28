/**
 * The three representation LAWS — pure functions, no React, no I/O. Each is the
 * structural kill for a recorded playtest finding:
 *   FR1 (pf1): a chip advances only on a receipt.
 *   FR2 (pf2): the home number IS the count of actionable cards.
 *   FR3 (pf3): an unclosed goal is labelled, never rendered as a fact.
 */
import { ago } from "@/lib/time";

import type { ActResult, BoardHeadline, Evidence, HomeAccount, RepresentationCard, StateChip } from "./types";

/* ------------------------------------------------------------------ FR1 --- */

/**
 * The state the ledger actually holds after an act. A chip may show only THIS.
 * The chip advances only when the server returns a receipt; a receipt-less
 * "ok" (pf1's phantom) and any error both leave the state where it was.
 */
export function ledgerStateFrom(current: string, result: ActResult): string {
  if (result.ok && result.receipt_ref) return result.new_state ?? current;
  return current;
}

/**
 * Compute a chip purely from the server's receipt-or-error result. There is no
 * argument for client optimism, so a chip can never flip from client state
 * alone (pf1). A failed act, or an "ok" with no receipt, surfaces an error and
 * leaves the chip untouched.
 */
export function resolveStateChip(current: string, result: ActResult): StateChip {
  if (result.ok && result.receipt_ref) {
    return { state: result.new_state ?? current, error: null, receiptRef: result.receipt_ref };
  }
  const error = result.ok
    ? "act returned no receipt — nothing was recorded; state unchanged"
    : result.error || "act failed — state unchanged";
  return { state: current, error, receiptRef: null };
}

/* ------------------------------------------------------------------ FR2 --- */

/** A card counts toward the home number only if it is actually actionable. */
export function homeAccount(cards: RepresentationCard[]): HomeAccount {
  const actionable = cards.filter((c) => c.actionable);
  return { number: actionable.length, actionable, all: cards };
}

/**
 * Resolve one card: drop it from the set and re-derive the account. Because the
 * number is derived, the count and the rendered list decrement in lockstep —
 * the 2-vs-4-vs-6 incoherence (pf2) is impossible by construction. Resolving a
 * non-actionable or unknown id is a no-op on the number.
 */
export function resolveCard(account: HomeAccount, id: string): HomeAccount {
  return homeAccount(account.all.filter((c) => c.id !== id));
}

/* ------------------------------------------------------------------ FR3 --- */

/** Lifecycle states that mean the envelope is genuinely finished. */
const CLOSED = new Set(["completed", "promoted", "filed", "cancelled", "failed"]);
/** States (or a present predicate) that assert activity/outcome and thus need receipts to be a fact. */
const ASSERTS_OUTCOME = new Set(["running", "in_progress", "verifying", "completed", "promoted", "needs_claim", "claimed"]);

const STATE_PHRASE: Record<string, string> = {
  needs_claim: "waiting for you to claim",
  needs_review: "waiting for your review",
  claimed: "claimed, not yet run",
  admitted: "admitted to the queue",
  proposed: "proposed, nothing runs yet",
  running: "running now",
  in_progress: "in progress",
  verifying: "verifying",
  held: "held at your gate",
  blocked: "blocked",
  completed: "completed",
  promoted: "promoted",
  cancelled: "cancelled",
  failed: "failed",
};

function statePhrase(state: string): string {
  return STATE_PHRASE[state] ?? (state ? state.replace(/_/g, " ") : "unknown");
}

/**
 * Build a board headline that cannot turn a desired outcome into a believed
 * event (pf3). For an unclosed envelope the success_predicate is labelled as a
 * goal and never leads the headline; a status that asserts an outcome without
 * receipts renders claim-styled (dashed) with a last-receipt age.
 */
export function boardHeadline(evidence: Evidence): BoardHeadline {
  const state = evidence.state ?? "";
  const subject = evidence.subject || evidence.id;
  const receipts = evidence.receipt_refs ?? [];
  const hasReceipts = receipts.length > 0;
  const closed = CLOSED.has(state);
  const predicate = evidence.success_predicate?.trim() || "";

  // A fact requires BOTH closure and receipts. Anything else may not be stated
  // as accomplished.
  const rendersAsFact = closed && hasReceipts;

  // The headline leads with the subject + a plain state phrase — never the raw
  // predicate. A goal predicate on a non-fact envelope is explicitly labelled.
  const predicateLabeled = predicate.length > 0 && !rendersAsFact;
  const text = rendersAsFact && predicate ? `${subject}: ${predicate}` : `${subject} — ${statePhrase(state)}`;

  const asserts = ASSERTS_OUTCOME.has(state) || predicate.length > 0;
  const claimStyled = asserts && !hasReceipts;
  const lastReceiptAge = hasReceipts ? ago(evidence.last_receipt_at) : "no receipts yet";

  return { text, predicateLabeled, rendersAsFact, claimStyled, lastReceiptAge };
}
