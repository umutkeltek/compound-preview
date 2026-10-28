import type { ReactNode } from "react";

import { IconTile } from "@/components/kit/IconTile";
import type { Tone } from "@/components/kit/tone";

/**
 * SectionHeader (DESIGN.md §9.1/§9.3) — the anatomy that replaces bare `<h3>`s
 * at the top of every section card: IconTile(sm) + UPPERCASE tracked label
 * (`--text-xs`/700) + an optional right slot (SegmentedControl, menu, chip,
 * count). One header grammar so every screen scans identically.
 *
 * `icon` is optional; when omitted the label stands alone (a lighter-touch
 * header). `sub` is an inline muted clause after the label for a one-line
 * clarifier.
 */
export function SectionHeader({
  icon,
  tone = "neutral",
  label,
  sub,
  right,
  className = "",
}: {
  icon?: ReactNode;
  tone?: Tone;
  label: ReactNode;
  sub?: ReactNode;
  right?: ReactNode;
  className?: string;
}) {
  return (
    <div className={`flex items-center gap-2.5 ${className}`}>
      {icon != null ? <IconTile tone={tone}>{icon}</IconTile> : null}
      <span className="text-xs font-bold uppercase tracking-wide text-muted">{label}</span>
      {sub != null ? <span className="truncate text-xs font-medium text-faint">{sub}</span> : null}
      {right != null ? <span className="ml-auto flex items-center gap-2">{right}</span> : null}
    </div>
  );
}
