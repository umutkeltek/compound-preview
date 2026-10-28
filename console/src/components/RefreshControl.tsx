"use client";

import { useRouter } from "next/navigation";
import { useEffect, useState, useTransition } from "react";

export function RefreshControl({ responsive = false }: { responsive?: boolean }) {
  const router = useRouter();
  const [auto, setAuto] = useState(true);
  const [tick, setTick] = useState(0);
  const [pending, startTransition] = useTransition();

  useEffect(() => {
    if (!auto) return;
    const id = setInterval(() => {
      startTransition(() => router.refresh());
      setTick((t) => t + 1);
    }, 30_000);
    return () => clearInterval(id);
  }, [auto, router]);

  return (
    <div className="flex shrink-0 items-center" data-tick={tick}>
      <div
        data-refresh-control="expanded"
        className={responsive ? "hidden shrink-0 items-center gap-2 sm:flex" : "flex shrink-0 items-center gap-2"}
      >
        <button
          type="button"
          aria-pressed={auto}
          onClick={() => setAuto(!auto)}
          className={`shrink-0 whitespace-nowrap rounded-full px-2.5 py-1 text-[11.5px] font-medium transition-colors ${
            auto ? "bg-emerald-50 text-emerald-700" : "bg-slate-100 text-slate-500"
          }`}
          title="Refresh every 30 seconds"
        >
          auto 30s {auto ? "on" : "off"}
        </button>
        <button
          type="button"
          aria-label={pending ? "Refreshing" : "Refresh now"}
          onClick={() => startTransition(() => router.refresh())}
          disabled={pending}
          className="shrink-0 whitespace-nowrap rounded-full bg-slate-100 px-2.5 py-1 text-[11.5px] font-medium text-slate-600 transition-colors hover:bg-slate-200 disabled:cursor-wait disabled:opacity-60"
        >
          {pending ? "refreshing..." : "refresh"}
        </button>
      </div>
      {responsive ? (
        <div data-refresh-control="compact" className="flex shrink-0 items-center gap-1 sm:hidden">
          <button
            type="button"
            aria-pressed={auto}
            aria-label={`Automatic refresh every 30 seconds ${auto ? "on" : "off"}`}
            onClick={() => setAuto(!auto)}
            className={`inline-flex h-8 w-9 shrink-0 items-center justify-center whitespace-nowrap rounded-md text-2xs font-semibold tabular-nums ${
              auto ? "bg-good-soft text-good" : "bg-soft text-muted"
            }`}
            title={`Automatic refresh every 30 seconds ${auto ? "on" : "off"}`}
          >
            30s
          </button>
          <button
            type="button"
            aria-label={pending ? "Refreshing" : "Refresh now"}
            title={pending ? "Refreshing" : "Refresh now"}
            onClick={() => startTransition(() => router.refresh())}
            disabled={pending}
            className="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded-md border border-hair bg-panel text-base text-muted hover:bg-soft disabled:cursor-wait disabled:opacity-60"
          >
            <span aria-hidden>{pending ? "…" : "↻"}</span>
          </button>
        </div>
      ) : null}
    </div>
  );
}
