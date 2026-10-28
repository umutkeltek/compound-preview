import type { Metadata } from "next";
import "./globals.css";
import { NavContainer } from "@/components/NavContainer";
import { InstanceBadge } from "@/components/InstanceBadge";
import { CommandK } from "@/components/CommandK";
import { RefreshControl } from "@/components/RefreshControl";
import { SystemBanner } from "@/components/SystemBanner";
import { AskCompound } from "@/components/AskCompound";
import { ExperienceBar } from "@/components/ExperienceBar";
import { Breadcrumb, FreshnessChip, StatusChip, ThemeToggle } from "@/components/kit";
import { getAttention } from "@/lib/attention";
import { autonomyPosture } from "@/lib/briefing";
import { runJson, str } from "@/lib/compound";

export const metadata: Metadata = {
  title: "compound console",
  description: "Operator control surface over compound projections",
};

/**
 * No-flash theme resolution (DESIGN.md §6): runs synchronously at the top of
 * <body>, before content paints, so the persisted (or system-resolved) theme is
 * on <html data-theme> from the first frame. Mirrors ThemeToggle's key +
 * resolution exactly. `suppressHydrationWarning` on <html> covers the attribute
 * this script stamps that the server didn't render.
 */
const THEME_SCRIPT = `(function(){try{var k='compound-theme';var p=localStorage.getItem(k)||'auto';var d=(p==='dark')||(p!=='light'&&window.matchMedia('(prefers-color-scheme: dark)').matches);document.documentElement.setAttribute('data-theme',d?'dark':'light');var q=localStorage.getItem('compound-experience-depth')||'understand';document.documentElement.setAttribute('data-depth',q);}catch(e){}})();`;

export default async function RootLayout({ children }: { children: React.ReactNode }) {
  // Both reads are cheap and memoized (getAttention shares the runJson TTL cache
  // with NavContainer; operator-authority is a small registry projection).
  const [attention, policyAuthority] = await Promise.all([
    getAttention(),
    runJson(["policy", "operator-authority", "--json"]),
  ]);
  const autonomy = autonomyPosture(policyAuthority);

  return (
    <html lang="en" suppressHydrationWarning>
      <body>
        <script dangerouslySetInnerHTML={{ __html: THEME_SCRIPT }} />
        <div className="flex min-h-screen">
          <NavContainer variant="desktop" />
          <div className="flex min-w-0 flex-1 flex-col">
            <header className="sticky top-0 z-40 flex min-h-12 flex-wrap items-center gap-2 border-b border-hair bg-page/80 px-3 py-2 backdrop-blur sm:flex-nowrap sm:gap-3 sm:px-5 md:px-6 md:py-2.5">
              <NavContainer variant="mobile" />
              <InstanceBadge />
              <div className="hidden min-w-0 sm:block"><Breadcrumb /></div>
              <div className="hidden min-w-0 2xl:block"><SystemBanner /></div>
              <div className="ml-auto flex shrink-0 items-center gap-1 sm:gap-2">
                <div className="hidden xl:block"><FreshnessChip generatedAt={str(attention?.generated_at) || undefined} source="compound attention --json" /></div>
                {/* Kill-switch / autonomy posture. Read from operator-authority
                    (level + mode); there is no cheap "halted" boolean today, so
                    this reflects governed posture, not an armed/tripped bit.
                    TODO: policy autonomy halted-state read when a projection lands. */}
                <div className="shrink-0">
                  <StatusChip
                    tone={autonomy.tone}
                    label={<span className="sr-only sm:not-sr-only">Autonomy {autonomy.level}</span>}
                    title={`${autonomy.detail} — kill switch: compound policy autonomy-halt · source: compound policy operator-authority --json`}
                  />
                </div>
                <RefreshControl responsive />
                <AskCompound />
                <div className="hidden lg:block"><CommandK /></div>
                <ThemeToggle responsive />
              </div>
            </header>
            <div className="2xl:hidden"><SystemBanner compact /></div>
            <ExperienceBar />
            <main className="mx-auto w-full flex-1 px-3 py-4 sm:px-5 sm:py-5 md:px-6 md:py-6" style={{ maxWidth: "1440px" }}>
              {children}
            </main>
          </div>
        </div>
      </body>
    </html>
  );
}
