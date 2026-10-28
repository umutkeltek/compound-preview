"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

/**
 * Topbar breadcrumb slot (DESIGN.md §3/§4 — the PageHeader breadcrumb pattern,
 * hoisted into the sticky topbar so every screen states where it is). Reads the
 * current path and maps the leading segment to its screen name from the Part-2
 * IA; a deeper segment (a work/candidate id) renders as a mono tail.
 */
const SECTION: Record<string, string> = {
  "": "Briefing",
  reviews: "Decide",
  work: "Work",
  flow: "Flow",
  executors: "Agents",
  loops: "Loops",
  memory: "Memory",
  map: "Capabilities",
  sources: "Sources",
};

export function Breadcrumb() {
  const pathname = usePathname() || "/";
  const segments = pathname.split("/").filter(Boolean);
  const head = segments[0] ?? "";
  const section = SECTION[head] ?? (head ? head.replace(/-/g, " ") : "Briefing");
  const tail = segments[1];

  return (
    <nav aria-label="Breadcrumb" className="mono flex items-center gap-1.5 text-2xs text-faint">
      <Link href="/" className="transition-colors hover:text-muted">
        compound
      </Link>
      <span aria-hidden>/</span>
      {tail ? (
        <>
          <Link href={`/${head}`} className="transition-colors hover:text-muted">
            {section}
          </Link>
          <span aria-hidden>/</span>
          <span className="max-w-40 truncate text-muted" title={tail}>
            {tail}
          </span>
        </>
      ) : (
        <span className="font-semibold text-muted">{section}</span>
      )}
    </nav>
  );
}
