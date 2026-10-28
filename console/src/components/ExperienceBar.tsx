"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useState } from "react";

import { routeIsActive, surfaceForPath } from "@/lib/experience";

type Depth = "glance" | "understand" | "investigate" | "technical";
const DEPTHS: Array<{ id: Depth; label: string }> = [
  { id: "glance", label: "Glance" },
  { id: "understand", label: "Understand" },
  { id: "investigate", label: "Investigate" },
  { id: "technical", label: "Technical" },
];
const DEPTH_KEY = "compound-experience-depth";

export function ExperienceBar() {
  const pathname = usePathname();
  const surface = surfaceForPath(pathname);
  const progressive = surface.id === "today" || surface.id === "live";
  const [depth, setDepth] = useState<Depth>("understand");

  useEffect(() => {
    const saved = window.localStorage.getItem(DEPTH_KEY) as Depth | null;
    const resolved = DEPTHS.some((item) => item.id === saved) ? saved! : "understand";
    setDepth(resolved);
    document.documentElement.dataset.depth = resolved;
  }, []);

  function choose(next: Depth) {
    setDepth(next);
    window.localStorage.setItem(DEPTH_KEY, next);
    document.documentElement.dataset.depth = next;
  }

  return (
    <div className="border-b border-hair bg-panel/75 px-3 py-1.5 backdrop-blur sm:px-5 md:px-6">
      <div className="mx-auto flex w-full items-center gap-3" style={{ maxWidth: "1440px" }}>
        <div className="sr-only">
          <div className="text-xs font-semibold text-ink">{surface.label}</div>
          <div>{surface.description}</div>
        </div>

        {surface.routes.length > 1 ? (
          <nav className="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto" aria-label={`${surface.label} views`}>
            {surface.routes.map((route) => {
              const active = routeIsActive(pathname, route);
              return (
                <Link
                  key={route.href}
                  href={route.href}
                  data-active={active}
                  className="whitespace-nowrap rounded-pill border border-transparent px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:bg-soft hover:text-ink data-[active=true]:border-hair data-[active=true]:bg-soft-2 data-[active=true]:text-ink"
                  aria-current={active ? "page" : undefined}
                >
                  {route.label}
                </Link>
              );
            })}
          </nav>
        ) : <div className="flex-1" />}

        {progressive ? <div className="flex shrink-0 items-center gap-1 rounded-pill border border-hair bg-soft p-0.5" aria-label="Information depth">
          {DEPTHS.map((item) => (
            <button
              key={item.id}
              type="button"
              aria-pressed={depth === item.id}
              onClick={() => choose(item.id)}
              className="rounded-pill px-2 py-1 text-2xs font-semibold text-faint transition-colors hover:text-ink aria-pressed:bg-panel aria-pressed:text-ink aria-pressed:shadow-sm"
            >
              <span className={item.id === "investigate" || item.id === "technical" ? "hidden sm:inline" : ""}>{item.label}</span>
            </button>
          ))}
        </div> : null}
      </div>
    </div>
  );
}
