/**
 * Client-safe move-type register — EP-U1 plain-language owner for the console.
 *
 * The Go side (internal/envelope/glossary.go MoveTypePlain) owns the canonical
 * one-liners; this module mirrors them byte-for-byte so client components can
 * import the values + labels + one-liners without pulling in server-only
 * modules (decision-registry.ts pulls in @/lib/compound's node:child_process/fs).
 *
 * What lives here:
 *   - MOVE_TYPES:        ordered array of {value, label} for selects / dropdowns
 *   - MOVE_TYPE_LABELS:  Record<value, label> — the headline shown in tables
 *   - MOVE_TYPE_GLOSSARY: Record<value, one-liner> — the operator-readable line
 *
 * What deliberately does NOT live here:
 *   - any import from @/lib/compound, node:child_process, node:fs, etc.
 *   - any decision-area spec (that stays in decision-registry.ts)
 *   - any UI rendering helper (that stays in glossary.ts)
 *
 * Divergence guard: console/scripts/move-type-drift-smoke.mjs reads the Go
 * MoveTypePlain map and asserts byte-equal one-liners here. Mutating any
 * value without updating the Go side reds the smoke.
 */

export interface MoveTypeEntry {
  value: string;
  label: string;
}

export const MOVE_TYPES: MoveTypeEntry[] = [
  { value: "outward_delta", label: "Outward delta" },
  { value: "memory_delta", label: "Memory delta" },
  { value: "rule_delta", label: "Rule delta" },
  { value: "control_plane_repair", label: "Control-plane repair" },
  { value: "audit_only", label: "Audit only" },
];

export const MOVE_TYPE_LABELS: Record<string, string> = {
  outward_delta: "Outward delta",
  memory_delta: "Memory delta",
  rule_delta: "Rule delta",
  control_plane_repair: "Control-plane repair",
  audit_only: "Audit only",
};

export const MOVE_TYPE_GLOSSARY: Record<string, string> = {
  outward_delta: "Shipped a real product change — the outward-facing move.",
  memory_delta: "Wrote new memory the system can recall later.",
  rule_delta: "Changed a rule, contract, or doctrine the system follows.",
  control_plane_repair: "Fixed the control plane itself — no product or memory change.",
  audit_only: "Recorded for the audit trail; no state moved.",
};

/** All move-type values, derived from the canonical array. */
export function moveTypeValues(): string[] {
  return MOVE_TYPES.map((entry) => entry.value);
}

/** One-liner for a move type, with a guarded fallback for unknown values. */
export function moveTypePlain(value: string): string {
  return MOVE_TYPE_GLOSSARY[value] ?? "A recorded move in the work's lifecycle.";
}

/** Display label for a move type, with a guarded fallback that derives a
 * human-readable form from the snake_case value (same fallback the transitions
 * ticker was already using). */
export function moveTypeLabel(value: string): string {
  return MOVE_TYPE_LABELS[value] ?? value.replace(/_/g, " ");
}
