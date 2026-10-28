import type { SVGProps } from "react";

/**
 * The ONE icon grammar (DESIGN.md §7.10): 15px default, stroke 1.7, `fill:none`,
 * `currentColor`, round caps/joins — so an IconTile tints its glyph by ink alone
 * and no second icon set ever creeps in. Hand-built line paths (no icon lib,
 * matching §8's "hand-built SVG" stance). Add a glyph here, never inline a
 * one-off `<svg>` in a screen.
 */
type IconProps = SVGProps<SVGSVGElement> & { size?: number };

function Base({ size = 15, children, ...rest }: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
      {...rest}
    >
      {children}
    </svg>
  );
}

export const GridIcon = (p: IconProps) => (
  <Base {...p}>
    <rect x="3" y="3" width="7" height="7" rx="1.5" />
    <rect x="14" y="3" width="7" height="7" rx="1.5" />
    <rect x="3" y="14" width="7" height="7" rx="1.5" />
    <rect x="14" y="14" width="7" height="7" rx="1.5" />
  </Base>
);

export const LayersIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M12 3 3 8l9 5 9-5-9-5Z" />
    <path d="M3 13l9 5 9-5" />
  </Base>
);

export const BoltIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M13 2 4 14h7l-1 8 9-12h-7l1-8Z" />
  </Base>
);

export const PulseIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M3 12h4l2 6 4-14 2 8h6" />
  </Base>
);

export const ClockIcon = (p: IconProps) => (
  <Base {...p}>
    <circle cx="12" cy="12" r="9" />
    <path d="M12 7v5l3 2" />
  </Base>
);

export const CoinsIcon = (p: IconProps) => (
  <Base {...p}>
    <ellipse cx="9" cy="7" rx="6" ry="3" />
    <path d="M3 7v5c0 1.66 2.69 3 6 3s6-1.34 6-3V7" />
    <path d="M15 12.5c2.5-.2 6-1.3 6-3.5" />
    <path d="M9 15v3c0 1.66 2.69 3 6 3s6-1.34 6-3v-5" />
  </Base>
);

export const RouteIcon = (p: IconProps) => (
  <Base {...p}>
    <circle cx="6" cy="19" r="2.5" />
    <circle cx="18" cy="5" r="2.5" />
    <path d="M8 19h6a4 4 0 0 0 0-8h-4a4 4 0 0 1 0-8h6" />
  </Base>
);

export const UsersIcon = (p: IconProps) => (
  <Base {...p}>
    <circle cx="9" cy="8" r="3.5" />
    <path d="M3 20a6 6 0 0 1 12 0" />
    <path d="M16 5.5a3.5 3.5 0 0 1 0 6.5M21 20a6 6 0 0 0-4-5.6" />
  </Base>
);

export const InboxIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M3 13h5l1.5 3h5L16 13h5" />
    <path d="M5 5h14l2 8v5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-5L5 5Z" />
  </Base>
);

export const GavelIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="m14 4 6 6-3 3-6-6 3-3Z" />
    <path d="m8 10 6 6-4 4-6-6 4-4Z" />
    <path d="M14 20h7" />
  </Base>
);

export const BrainIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M9 4a3 3 0 0 0-3 3 3 3 0 0 0-1 5 3 3 0 0 0 2 4 3 3 0 0 0 5 1V4.5A2.5 2.5 0 0 0 9 4Z" />
    <path d="M15 4a3 3 0 0 1 3 3 3 3 0 0 1 1 5 3 3 0 0 1-2 4 3 3 0 0 1-5 1" />
  </Base>
);

export const CubeIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M12 3 4 7.5v9L12 21l8-4.5v-9L12 3Z" />
    <path d="M4 7.5 12 12l8-4.5M12 12v9" />
  </Base>
);

export const PlugIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M9 3v6M15 3v6" />
    <path d="M7 9h10v3a5 5 0 0 1-10 0V9Z" />
    <path d="M12 17v4" />
  </Base>
);

export const LoopIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M4 9a8 8 0 0 1 14-3l2 2M20 15a8 8 0 0 1-14 3l-2-2" />
    <path d="M20 4v4h-4M4 20v-4h4" />
  </Base>
);

export const GearIcon = (p: IconProps) => (
  <Base {...p}>
    <circle cx="12" cy="12" r="3" />
    <path d="M12 2v3M12 19v3M22 12h-3M5 12H2M19 5l-2 2M7 17l-2 2M19 19l-2-2M7 7 5 5" />
  </Base>
);

export const ShieldIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M12 3 5 6v5c0 4.5 3 8 7 10 4-2 7-5.5 7-10V6l-7-3Z" />
    <path d="m9 12 2 2 4-4" />
  </Base>
);

export const DatabaseIcon = (p: IconProps) => (
  <Base {...p}>
    <ellipse cx="12" cy="5" rx="8" ry="3" />
    <path d="M4 5v14c0 1.66 3.58 3 8 3s8-1.34 8-3V5" />
    <path d="M4 12c0 1.66 3.58 3 8 3s8-1.34 8-3" />
  </Base>
);

export const HeartPulseIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M12 20s-7-4.5-9-9a4.5 4.5 0 0 1 8-3 4.5 4.5 0 0 1 8 3c-.4 1-1 1.9-1.7 2.8" />
    <path d="M11 12h2l1.5-2 1.5 4 1-2h3" />
  </Base>
);

export const ArrowUpIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M12 20V5M6 11l6-6 6 6" />
  </Base>
);

export const CheckIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="m5 12 5 5 9-11" />
  </Base>
);

export const SourceIcon = (p: IconProps) => (
  <Base {...p}>
    <circle cx="12" cy="12" r="2.5" />
    <path d="M12 3v6.5M12 14.5V21M3 12h6.5M14.5 12H21" />
  </Base>
);

export const DocIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M14 3H6a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8l-5-5Z" />
    <path d="M14 3v5h5M8 13h8M8 17h5" />
  </Base>
);

export const TableIcon = (p: IconProps) => (
  <Base {...p}>
    <rect x="3" y="4" width="18" height="16" rx="1.5" />
    <path d="M3 9h18M3 14h18M9 4v16" />
  </Base>
);

export const BarChartIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M4 20V10M10 20V4M16 20v-8M22 20H2" />
  </Base>
);

export const SlidersIcon = (p: IconProps) => (
  <Base {...p}>
    <path d="M5 21v-6M5 11V3M12 21v-9M12 8V3M19 21v-4M19 13V3" />
    <path d="M2 15h6M9 8h6M16 17h6" />
  </Base>
);
