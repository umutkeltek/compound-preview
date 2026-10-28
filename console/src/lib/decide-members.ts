import type { DecideGroup } from "./decide-model";
import type { AuthorityFace } from "../representation";

export function remainingDecideMembers<T extends { id: string }>(
  members: T[],
  excluded: ReadonlySet<string>,
  completed: ReadonlySet<string>,
): T[] {
  return members.filter((member) => !excluded.has(member.id) && !completed.has(member.id));
}

export type EnumerableAuthorityFace = {
  id: string;
  label: string;
  authority: AuthorityFace;
};

/**
 * Confirmation selector for strict per-member acts. It carries owner faces
 * unchanged and fails closed on any missing face; policy was already decided
 * by the server model and is not re-evaluated here.
 */
export function enumerableAuthorityFaces<
  T extends { id: string; label: string; authority?: AuthorityFace },
>(members: T[]): EnumerableAuthorityFace[] | null {
  if (members.some((member) => !member.authority)) return null;
  return members.map((member) => ({
    id: member.id,
    label: member.label,
    authority: member.authority!,
  }));
}

export type SystemicBlocker = {
  /** The shared blocked reason, verbatim from the model — map to plain
   * language at render time (see `blockedReasonPlain` in `./glossary`). */
  reason: string;
  blockedCount: number;
  totalCount: number;
};

/**
 * Detects a "systemic blocker": (near-)every group in the queue is stuck on
 * the identical owner-unavailable reason. When true, the queue should show
 * ONE banner ("N of M decidable — <reason>") instead of stamping the same
 * reason on every card — 21 identical stamps is one fact, not 21.
 * `minShare` is the near-all threshold (default 0.9): not every blocked
 * group has to share the reason, only enough that repeating it per-card
 * would be noise rather than information.
 *
 * `projectedReason` feature-detects a queue-level reason the projection may
 * carry directly (a `systemic_blocker` field landing separately, from
 * another executor's slice); when absent this falls back to computing the
 * all-identical-reasons case from the groups actually rendered, so the
 * banner works correctly before that field exists.
 */
export function queueSystemicBlocker<
  T extends { face: Pick<DecideGroup["face"], "state" | "unavailableReason"> },
>(groups: T[], projectedReason?: string | null, minShare = 0.9): SystemicBlocker | null {
  const totalCount = groups.length;
  if (totalCount === 0) return null;
  const blocked = groups.filter((g) => g.face.state !== "ready");
  if (projectedReason && projectedReason.trim()) {
    return { reason: projectedReason.trim(), blockedCount: blocked.length, totalCount };
  }
  const reasoned = blocked.filter((g) => g.face.unavailableReason);
  if (reasoned.length === 0) return null;
  const reason = reasoned[0].face.unavailableReason;
  if (!reasoned.every((g) => g.face.unavailableReason === reason)) return null;
  if (reasoned.length / totalCount < minShare) return null;
  return { reason, blockedCount: reasoned.length, totalCount };
}

export type DecideReadinessSectionKey = "act_now" | "needs_context";

export type DecideReadinessSection<T> = {
  key: DecideReadinessSectionKey;
  label: string;
  description: string;
  groups: T[];
};

/**
 * The queue never guesses actionability from area names or UI copy. The
 * server-owned ObjectFace state is the only input to this client-safe split.
 */
export function decideReadinessSections<T extends { face: Pick<DecideGroup["face"], "state"> }>(
  groups: T[],
): DecideReadinessSection<T>[] {
  const actNow = groups.filter((group) => group.face.state === "ready");
  const needsContext = groups.filter((group) => group.face.state !== "ready");
  return [
    {
      key: "act_now" as const,
      label: "Act now",
      description: "A governed next step is ready.",
      groups: actNow,
    },
    {
      key: "needs_context" as const,
      label: "Needs context",
      description: "The owner workflow is missing or this group needs a narrower review.",
      groups: needsContext,
    },
  ].filter((section) => section.groups.length > 0);
}
