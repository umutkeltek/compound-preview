"use client";

import { useEffect, useState } from "react";

/**
 * Three-state theme control (DESIGN.md §6): light / auto / dark, persisted to
 * localStorage under `compound-theme`. The no-flash inline script in the app
 * <head> (layout.tsx) applies the resolved theme BEFORE first paint; this
 * control only mirrors + mutates that state, so there is no flash and no
 * hydration mismatch (it renders a stable shell on the server and fills the
 * active segment after mount).
 *
 * `auto` follows the OS via matchMedia and re-resolves live while selected.
 */
export type ThemePref = "light" | "auto" | "dark";
export const THEME_KEY = "compound-theme";

function systemDark(): boolean {
  return typeof window !== "undefined" && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

function applyTheme(pref: ThemePref) {
  const resolved = pref === "auto" ? (systemDark() ? "dark" : "light") : pref;
  document.documentElement.setAttribute("data-theme", resolved);
}

const OPTIONS: { value: ThemePref; label: string; icon: React.ReactNode }[] = [
  { value: "light", label: "Light", icon: <SunIcon /> },
  { value: "auto", label: "Auto", icon: <AutoIcon /> },
  { value: "dark", label: "Dark", icon: <MoonIcon /> },
];

export function ThemeToggle({ responsive = false }: { responsive?: boolean }) {
  const [pref, setPref] = useState<ThemePref | null>(null);

  useEffect(() => {
    const stored = (localStorage.getItem(THEME_KEY) as ThemePref | null) ?? "auto";
    setPref(stored);
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => {
      if ((localStorage.getItem(THEME_KEY) as ThemePref | null) === "auto") applyTheme("auto");
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  function choose(next: ThemePref) {
    setPref(next);
    localStorage.setItem(THEME_KEY, next);
    applyTheme(next);
  }

  const current = pref ?? "auto";
  const currentIndex = OPTIONS.findIndex((option) => option.value === current);
  const activeOption = OPTIONS[currentIndex];
  const nextOption = OPTIONS[(currentIndex + 1) % OPTIONS.length];

  return (
    <>
      {responsive ? (
        <button
          type="button"
          data-theme-control="compact"
          aria-label={`${activeOption.label} theme. Switch to ${nextOption.label} theme`}
          title={`${activeOption.label} theme · switch to ${nextOption.label.toLowerCase()}`}
          onClick={() => choose(nextOption.value)}
          className="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded-md border border-hair bg-soft text-muted transition-colors hover:bg-soft-2 hover:text-ink sm:hidden"
        >
          {activeOption.icon}
        </button>
      ) : null}
      <div
        role="radiogroup"
        aria-label="Theme"
        data-theme-control="expanded"
        className={`${responsive ? "hidden sm:inline-flex" : "inline-flex"} shrink-0 items-center rounded-pill border border-hair bg-soft p-0.5`}
      >
        {OPTIONS.map((opt) => {
          const active = pref === opt.value;
          return (
            <button
              key={opt.value}
              type="button"
              role="radio"
              aria-checked={active}
              aria-label={opt.label}
              title={`${opt.label} theme`}
              onClick={() => choose(opt.value)}
              className={`inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-pill transition-colors ${
                active ? "bg-panel text-ink shadow-sm" : "text-faint hover:text-muted"
              }`}
            >
              {opt.icon}
            </button>
          );
        })}
      </div>
    </>
  );
}

/* one icon grammar: 15px box, stroke 1.7, fill:none, currentColor (DESIGN.md §7.10) */
function SunIcon() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
    </svg>
  );
}
function MoonIcon() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z" />
    </svg>
  );
}
function AutoIcon() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 3a9 9 0 0 0 0 18Z" fill="currentColor" stroke="none" />
    </svg>
  );
}
