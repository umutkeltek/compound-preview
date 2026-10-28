"use client";

import * as Dialog from "@radix-ui/react-dialog";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState } from "react";

import { BrainIcon, GavelIcon, GridIcon, LoopIcon, PulseIcon, RouteIcon, TableIcon } from "@/components/kit/icons";
import { EXPERIENCE_SURFACES, routeIsActive, surfaceForPath } from "@/lib/experience";

/**
 * The primary rail is needs-led rather than object-led. Existing routes stay
 * stable beneath one of seven product surfaces; the active surface expands its
 * secondary navigation without making every advanced route permanently loud.
 */
type BadgeTone = "seam" | "quiet";
type NavBadge = { count: number; tone: BadgeTone };

export type NavProps = {
  /** Needs-you group count for the Decide queue (interim source: `compound reviews --json` groups — see NavContainer). Amber, the only amber badge. */
  decideCount?: number | null;
  /** Cheap needs-claim count from the existing workbench fetch (`counts.needs_claim`). Quiet slate — not a needs-you signal. */
  workCount?: number | null;
  variant?: "desktop" | "mobile";
};

const SURFACE_ICONS = {
  today: GridIcon,
  live: PulseIcon,
  work: TableIcon,
  decide: GavelIcon,
  knowledge: BrainIcon,
  system: RouteIcon,
  outcomes: LoopIcon,
};

function CountPill({ count, tone }: { count: number; tone: BadgeTone }) {
  if (count <= 0) return null;
  const cls = tone === "seam" ? "border-seam-line bg-seam-soft text-seam" : "border-hair bg-soft-2 text-muted";
  return (
    <span
      className={`mono ml-auto inline-flex min-w-5 items-center justify-center rounded-pill border px-1.5 py-0.5 text-2xs font-semibold tabular-nums ${cls}`}
    >
      {count}
    </span>
  );
}

function Brand() {
  return (
    <div className="flex items-center gap-2.5 px-1">
      <span className="flex h-8 w-8 items-center justify-center rounded-md bg-ink text-md font-bold text-panel">
        c
      </span>
      <div className="leading-tight">
        <div className="text-base font-semibold text-ink">compound</div>
        <div className="text-xs text-faint">console</div>
      </div>
    </div>
  );
}

function NavLinks({
  pathname,
  decideCount,
  workCount,
  mobile = false,
  onNavigate,
}: {
  pathname: string;
  decideCount?: number | null;
  workCount?: number | null;
  mobile?: boolean;
  onNavigate?: () => void;
}) {
  const linkClass = mobile ? "nav-item py-2.5" : "nav-item";
  const activeSurface = surfaceForPath(pathname);
  return (
    <nav className="flex flex-1 flex-col gap-1" aria-label="Primary">
      {EXPERIENCE_SURFACES.map((surface) => {
        const active = activeSurface.id === surface.id;
        const Icon = SURFACE_ICONS[surface.id];
        const badge: NavBadge | undefined = surface.id === "decide" && decideCount != null
          ? { count: decideCount, tone: "seam" }
          : surface.id === "work" && workCount != null
            ? { count: workCount, tone: "quiet" }
            : undefined;
        return (
          <div key={surface.id}>
            <Link
              href={surface.href}
              className={linkClass}
              data-active={active}
              aria-current={pathname === surface.href ? "page" : undefined}
              onClick={onNavigate}
              title={surface.description}
            >
              <span className={active ? "text-work" : "text-faint"}><Icon size={14} /></span>
              {surface.label}
              {badge ? <CountPill count={badge.count} tone={badge.tone} /> : null}
            </Link>
            {active && surface.routes.length > 1 ? (
              <div className="ml-7 mt-0.5 flex flex-col border-l border-hair pl-2">
                {surface.routes.map((route) => (
                  <Link
                    key={route.href}
                    href={route.href}
                    onClick={onNavigate}
                    className="rounded-md px-2 py-1.5 text-xs text-faint transition-colors hover:bg-soft hover:text-ink data-[active=true]:text-work"
                    data-active={routeIsActive(pathname, route)}
                  >
                    {route.label}
                  </Link>
                ))}
              </div>
            ) : null}
          </div>
        );
      })}
    </nav>
  );
}

function NavFooter() {
  return (
    <div className="px-1 text-2xs leading-relaxed text-faint">
      Projection-first · evidence-backed.
      <br />
      Depth changes presentation, never truth.
    </div>
  );
}

export function Nav({ decideCount, workCount, variant = "desktop" }: NavProps) {
  const pathname = usePathname();
  const [open, setOpen] = useState(false);

  if (variant === "mobile") {
    return (
      <Dialog.Root open={open} onOpenChange={setOpen}>
        <Dialog.Trigger asChild>
          <button
            type="button"
            aria-label="Open navigation"
            title="Open navigation"
            className="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded-md border border-hair bg-panel text-muted hover:bg-soft md:hidden"
          >
            <span aria-hidden className="grid gap-1">
              <span className="block h-px w-4 bg-current" />
              <span className="block h-px w-4 bg-current" />
              <span className="block h-px w-4 bg-current" />
            </span>
          </button>
        </Dialog.Trigger>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 z-[60] bg-scrim backdrop-blur-sm motion-reduce:backdrop-blur-none md:hidden" />
          <Dialog.Content className="fixed inset-y-0 left-0 z-[60] flex w-[min(86vw,300px)] flex-col gap-6 overflow-y-auto overscroll-contain border-r border-hair bg-panel px-4 py-5 shadow-lg focus:outline-none md:hidden">
            <div className="flex items-center justify-between gap-3">
              <Dialog.Title asChild><div><Brand /></div></Dialog.Title>
              <Dialog.Close asChild>
                <button
                  type="button"
                  aria-label="Close navigation"
                  title="Close navigation"
                  className="inline-flex h-8 w-8 items-center justify-center rounded-md border border-hair text-lg text-muted hover:bg-soft"
                >
                  <span aria-hidden>×</span>
                </button>
              </Dialog.Close>
            </div>
            <Dialog.Description className="sr-only">Navigate between Compound console workspaces.</Dialog.Description>
            <NavLinks pathname={pathname} decideCount={decideCount} workCount={workCount} mobile onNavigate={() => setOpen(false)} />
            <NavFooter />
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
    );
  }

  return (
    <aside className="sticky top-0 hidden h-screen w-[var(--nav-w)] shrink-0 flex-col gap-6 border-r border-hair bg-panel/70 px-4 py-5 backdrop-blur md:flex">
      <Brand />
      <NavLinks pathname={pathname} decideCount={decideCount} workCount={workCount} />
      <NavFooter />
    </aside>
  );
}
