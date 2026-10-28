import { hashToneFor, TONE_SOFT_BG, TONE_TEXT } from "@/components/kit/tone";

/**
 * AvatarTile (DESIGN.md §9.3) — an initials tile with a DETERMINISTIC identity
 * tint: the name hashes to one of six token tone-pairs (`*-soft` fill + `*-fg`
 * ink), so an executor/harness keeps the same color across renames and reloads.
 * The six pairs are the existing domain/good token pairs (never raw pastels),
 * so identity tints stay theme-reactive and on-palette (§7.1).
 */
function initials(name: string): string {
  const parts = name
    .replace(/[_\-/.]+/g, " ")
    .trim()
    .split(/\s+/)
    .filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

export function AvatarTile({
  name,
  size = "md",
  className = "",
}: {
  name: string;
  size?: "sm" | "md";
  className?: string;
}) {
  const tone = hashToneFor(name || "?");
  const box = size === "md" ? "h-8 w-8 text-xs" : "h-6 w-6 text-2xs";
  return (
    <span
      title={name}
      aria-hidden
      className={`inline-flex shrink-0 items-center justify-center rounded-sm font-bold tabular-nums ${box} ${TONE_SOFT_BG[tone]} ${TONE_TEXT[tone]} ${className}`}
    >
      {initials(name)}
    </span>
  );
}
