/**
 * Shared tone → token-class maps (DESIGN.md §0). One vocabulary the §9 atoms
 * (IconTile, DotMatrix, CapsuleColumn, AvatarTile, SegmentedProgress) read from,
 * so a domain/severity hue is chosen ONCE and never drifts between atoms. Every
 * value is a semantic Tailwind utility backed by a runtime var — no raw palette,
 * so a theme flip re-picks all of them (§7.1/§7.7).
 *
 * `neutral` is the "no domain" fall-through (a section that isn't owned by a
 * subsystem): soft-2 fill + muted ink, never a hue.
 */
export type Tone =
  | "work"
  | "memory"
  | "capability"
  | "route"
  | "product"
  | "good"
  | "seam"
  | "crit"
  | "neutral";

/** Soft tinted fill (`*-soft`) — IconTile chip, AvatarTile, DotMatrix level 1. */
export const TONE_SOFT_BG: Record<Tone, string> = {
  work: "bg-work-soft",
  memory: "bg-memory-soft",
  capability: "bg-capability-soft",
  route: "bg-route-soft",
  product: "bg-product-soft",
  good: "bg-good-soft",
  seam: "bg-seam-soft",
  crit: "bg-crit-soft",
  neutral: "bg-soft-2",
};

/** Foreground ink (`*-fg`) — glyphs, initials, values. */
export const TONE_TEXT: Record<Tone, string> = {
  work: "text-work",
  memory: "text-memory",
  capability: "text-capability",
  route: "text-route",
  product: "text-product",
  good: "text-good",
  seam: "text-seam",
  crit: "text-crit",
  neutral: "text-muted",
};

/** Solid full-strength fill (`*` = the fg hue as a background) — DotMatrix level 3,
 *  CapsuleColumn value segment, SegmentedProgress. */
export const TONE_FILL: Record<Tone, string> = {
  work: "bg-work",
  memory: "bg-memory",
  capability: "bg-capability",
  route: "bg-route",
  product: "bg-product",
  good: "bg-good",
  seam: "bg-seam",
  crit: "bg-crit",
  neutral: "bg-faint",
};

/** Line tint (`*-line`) — DotMatrix level 2, hairline emphasis. */
export const TONE_LINE_BG: Record<Tone, string> = {
  work: "bg-work-line",
  memory: "bg-memory-line",
  capability: "bg-capability-line",
  route: "bg-route-line",
  product: "bg-product-line",
  good: "bg-good-line",
  seam: "bg-seam-line",
  crit: "bg-crit-line",
  neutral: "bg-hair",
};

/** Border tint (`border-*-line`). */
export const TONE_BORDER: Record<Tone, string> = {
  work: "border-work-line",
  memory: "border-memory-line",
  capability: "border-capability-line",
  route: "border-route-line",
  product: "border-product-line",
  good: "border-good-line",
  seam: "border-seam-line",
  crit: "border-crit-line",
  neutral: "border-hair",
};

/**
 * Four-step intensity ramp for DotMatrix cells (DESIGN.md §9.3):
 * 0 empty → soft-2, 1 → *-soft, 2 → *-line, 3 → *-fg. Level clamps to 0..3.
 */
export function rampClass(tone: Tone, level: number): string {
  const l = Math.max(0, Math.min(3, Math.round(level)));
  if (l === 0) return "bg-soft-2";
  if (l === 1) return TONE_SOFT_BG[tone];
  if (l === 2) return TONE_LINE_BG[tone];
  return TONE_FILL[tone];
}

/**
 * The six identity tones for AvatarTile (DESIGN.md §9.3 "6 pastel pairs, hash
 * of name"). We reuse the six existing token tone-pairs rather than inventing
 * raw pastels, so identity tints stay theme-reactive and on-palette.
 */
export const IDENTITY_TONES: Tone[] = ["work", "memory", "capability", "route", "product", "good"];

/** Deterministic FNV-1a hash → stable index; identity survives renames.
 *  Constants in hex: 0x811c9dc5 = FNV offset basis, 0x01000193 = FNV prime. */
export function hashToneFor(name: string): Tone {
  let h = 0x811c9dc5;
  for (let i = 0; i < name.length; i++) {
    h ^= name.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return IDENTITY_TONES[Math.abs(h) % IDENTITY_TONES.length];
}
