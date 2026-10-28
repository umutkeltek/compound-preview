import { str, type Json } from "@/lib/compound";
import { toDate } from "@/lib/time";

/**
 * Move-type mix — the World Δ classifier (formerly buried in the now-retired
 * ChangedProofStream component). Pure, server-safe, node-free. Groups the last
 * 7 days of terminal transitions into the four move classes the briefing KPI
 * strip reads: outward (product change), meaning (memory), rules, upkeep.
 */

export type MoveTypeGroup = "outward" | "meaning" | "rules" | "upkeep";

export type MoveTypeMix = {
  groups: Record<MoveTypeGroup, number>;
  total: number;
  lowOutward: boolean;
};

export function moveTypeGroup(move: string): MoveTypeGroup {
  switch (move) {
    case "outward_delta":
      return "outward";
    case "memory_delta":
      return "meaning";
    case "rule_delta":
      return "rules";
    default:
      return "upkeep";
  }
}

export function summarizeMoveTypeMix(recent: Json[], now = Date.now()): MoveTypeMix {
  const groups: Record<MoveTypeGroup, number> = { outward: 0, meaning: 0, rules: 0, upkeep: 0 };
  const weekMs = 7 * 24 * 3600 * 1000;

  for (const item of recent) {
    const when = toDate(item.claimed_at ?? item.created_at ?? item.generated_at)?.getTime() ?? 0;
    if (when > 0 && now - when > weekMs) continue;
    groups[moveTypeGroup(str(item.move_type) || str(item.move))]++;
  }

  const total = groups.outward + groups.meaning + groups.rules + groups.upkeep;
  return {
    groups,
    total,
    lowOutward: total > 0 && groups.outward <= Math.max(1, Math.floor(total * 0.15)),
  };
}
