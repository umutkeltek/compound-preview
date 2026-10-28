/**
 * Stable product routes grouped into seven user-facing surfaces. Keep this
 * client-safe: the shell and command palette both consume it.
 */
export type ExperienceSurfaceId =
  | "today"
  | "live"
  | "work"
  | "decide"
  | "knowledge"
  | "system"
  | "outcomes";

export type ExperienceRoute = { label: string; href: string; match?: string[] };
export type ExperienceSurface = {
  id: ExperienceSurfaceId;
  label: string;
  href: string;
  description: string;
  routes: ExperienceRoute[];
};

export const EXPERIENCE_SURFACES: ExperienceSurface[] = [
  {
    id: "today",
    label: "Today",
    href: "/",
    description: "The few things worth your attention now.",
    routes: [{ label: "Briefing", href: "/" }],
  },
  {
    id: "live",
    label: "Live",
    href: "/live",
    description: "Harnesses, work, handoffs, and waits as they happen.",
    routes: [
      { label: "Overview", href: "/live" },
      { label: "Harnesses", href: "/executors" },
      { label: "Coordination", href: "/traffic" },
    ],
  },
  {
    id: "work",
    label: "Work",
    href: "/work",
    description: "What is moving, what is blocked, and what can move next.",
    routes: [{ label: "Pipeline", href: "/work", match: ["/work/"] }],
  },
  {
    id: "decide",
    label: "Decide",
    href: "/reviews",
    description: "Consequential calls, with evidence and downstream impact.",
    routes: [{ label: "Decision queue", href: "/reviews" }],
  },
  {
    id: "knowledge",
    label: "Knowledge",
    href: "/memory",
    description: "What Compound knows, trusts, and is still evaluating.",
    routes: [
      { label: "Memory", href: "/memory", match: ["/map/memory"] },
      { label: "Standing law", href: "/standing-law" },
      { label: "Sources", href: "/sources" },
    ],
  },
  {
    id: "system",
    label: "System",
    href: "/map",
    description: "Capabilities, routes, policy, and effective authority.",
    routes: [
      { label: "Map", href: "/map", match: ["/map/"] },
      { label: "Routing", href: "/routing" },
      { label: "Settings", href: "/settings" },
      { label: "View grammar", href: "/representation" },
    ],
  },
  {
    id: "outcomes",
    label: "Outcomes",
    href: "/loops",
    description: "What shipped, what changed, and what the system learned.",
    routes: [
      { label: "Feedback loops", href: "/loops" },
      { label: "Ads", href: "/ads" },
    ],
  },
];

function matches(pathname: string, route: ExperienceRoute): boolean {
  return pathname === route.href || (route.match ?? []).some((prefix) => pathname.startsWith(prefix));
}

export function surfaceForPath(pathname: string): ExperienceSurface {
  if (pathname === "/flow") return EXPERIENCE_SURFACES.find((surface) => surface.id === "work")!;
  return EXPERIENCE_SURFACES.find((surface) => surface.routes.some((route) => matches(pathname, route))) ?? EXPERIENCE_SURFACES[0];
}

export function routeIsActive(pathname: string, route: ExperienceRoute): boolean {
  return matches(pathname, route);
}
