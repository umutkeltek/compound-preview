import type { ReactNode } from "react";

import { type Tone, TONE_SOFT_BG, TONE_TEXT } from "@/components/kit/tone";

/**
 * IconTile (DESIGN.md §9.3) — a rounded-square tinted chip that leads every
 * SectionHeader, KPI, knob row and feed item. `*-soft` fill + `*-fg` glyph, so
 * the domain hue reads as an accent chip, never a full fill (§7.2). Sizes:
 * sm 24px / md 32px, glyph ~15px. The glyph is `currentColor`, inheriting the
 * tone's text color — pass any 15px stroke-1.7 icon or a short mono char.
 */
export function IconTile({
  children,
  tone = "neutral",
  size = "sm",
  className = "",
}: {
  children: ReactNode;
  tone?: Tone;
  size?: "sm" | "md";
  className?: string;
}) {
  const box = size === "md" ? "h-8 w-8" : "h-6 w-6";
  const glyph = size === "md" ? "text-md" : "text-sm";
  return (
    <span
      aria-hidden
      className={`inline-flex shrink-0 items-center justify-center rounded-sm ${box} ${glyph} ${TONE_SOFT_BG[tone]} ${TONE_TEXT[tone]} ${className}`}
    >
      {children}
    </span>
  );
}
