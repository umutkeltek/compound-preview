"use client";

import { useEffect, useState } from "react";

/*
 * The dashboard truth contract, made visible: a stamp that AGES between polls.
 * Green when fresh, amber past 5 minutes stale. Reflects DATA freshness
 * (graph.generatedAt), not request time. FreshnessChip can't do this — it has
 * no amber path and doesn't tick.
 */

const STALE_MS = 5 * 60 * 1000;

function relative(ms: number): string {
  const s = Math.max(0, ms / 1000);
  if (s < 90) return `${Math.round(s)}s ago`;
  if (s < 90 * 60) return `${Math.round(s / 60)}m ago`;
  if (s < 36 * 3600) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}

export function LiveStamp({
  generatedAt,
  sources,
}: {
  generatedAt?: string;
  sources?: string[];
}) {
  // Relative age is time-dependent, so compute it only after mount — otherwise
  // the SSR string ("2s ago") won't match the client's ("3s ago") and React
  // throws a hydration mismatch. Pre-mount both sides render "just now".
  const [mounted, setMounted] = useState(false);
  const [, setTick] = useState(0);
  useEffect(() => {
    setMounted(true);
    const id = setInterval(() => setTick((n) => n + 1), 10_000);
    return () => clearInterval(id);
  }, []);

  const t = generatedAt ? new Date(generatedAt) : null;
  const valid = t && !Number.isNaN(t.getTime());
  const ageMs = valid ? Date.now() - t.getTime() : Infinity;
  const stale = mounted && ageMs > STALE_MS;

  const dot = !valid ? "#9aa4b2" : stale ? "#f59e0b" : "#10b981";
  const bg = !valid ? "#f1f5f9" : stale ? "#fef3c7" : "#ecfdf5";
  const fg = !valid ? "#64748b" : stale ? "#b45309" : "#047857";
  const label = !mounted ? "just now" : valid ? relative(ageMs) : "unknown";

  return (
    <span
      title={sources?.length ? `source: ${sources.join(" · ")}` : undefined}
      className="inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-[11.5px] font-medium"
      style={{ background: bg, color: fg }}
    >
      <span className="h-1.5 w-1.5 rounded-full" style={{ background: dot }} />
      {stale ? "stale" : "live"} · {label}
    </span>
  );
}
